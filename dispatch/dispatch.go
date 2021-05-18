// Copyright 2018 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dispatch

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/store"
	"github.com/prometheus/alertmanager/types"
)

// DispatcherMetrics represents metrics associated to a dispatcher.
type DispatcherMetrics struct {
	aggrGroups         prometheus.Gauge
	processingDuration prometheus.Summary
}

// NewDispatcherMetrics returns a new registered DispatchMetrics.
// -----------------------------------------------------------------
// 生成普罗米修斯分组数量和处理告警时长指标
func NewDispatcherMetrics(r prometheus.Registerer) *DispatcherMetrics {
	m := DispatcherMetrics{
		aggrGroups: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "alertmanager_dispatcher_aggregation_groups",
				Help: "Number of active aggregation groups",
			},
		),
		processingDuration: prometheus.NewSummary(
			prometheus.SummaryOpts{
				Name: "alertmanager_dispatcher_alert_processing_duration_seconds",
				Help: "Summary of latencies for the processing of alerts.",
			},
		),
	}

	if r != nil {
		r.MustRegister(m.aggrGroups, m.processingDuration)
	}

	return &m
}

// Dispatcher sorts incoming alerts into aggregation groups and
// assigns the correct notifiers to each.
// --------------------------------------------------------------
// 调度器对象，调度整个告警处理的核心流程。
type Dispatcher struct {
	route   *Route             // 分组的路由，负责告警label的匹配，树状结构
	alerts  provider.Alerts    // 告警集合
	stage   notify.Stage       // 包含整个流程的Pipeline，查看接收人，静默，抑制等等流程。
	metrics *DispatcherMetrics // 普罗米修斯调取器相关指标

	marker  types.Marker                      // 告警标记对象，标记高级被静默或/和抑制
	timeout func(time.Duration) time.Duration // TODO: ????, 自定义的timeout方法？

	aggrGroups map[*Route]map[model.Fingerprint]*aggrGroup // 分组列表map，所以分组都会在这里存放
	mtx        sync.RWMutex                                // 读写锁，用来操作分组map时使用

	// 关闭的通道，上下文和方法
	done   chan struct{}
	ctx    context.Context
	cancel func()

	logger log.Logger
}

// NewDispatcher returns a new Dispatcher.
func NewDispatcher(
	ap provider.Alerts,
	r *Route,
	s notify.Stage,
	mk types.Marker,
	to func(time.Duration) time.Duration,
	l log.Logger,
	m *DispatcherMetrics,
) *Dispatcher {
	disp := &Dispatcher{
		alerts:  ap,
		stage:   s,
		route:   r,
		marker:  mk,
		timeout: to,
		logger:  log.With(l, "component", "dispatcher"),
		metrics: m,
	}
	return disp
}

// Run starts dispatching alerts incoming via the updates channel.
// ----------------------------------------------------------------
// 运行调度器，初始化分组列表map和普罗米修斯分组计数指标。
func (d *Dispatcher) Run() {
	// 初始化结束通道
	d.done = make(chan struct{})

	d.mtx.Lock()
	d.aggrGroups = map[*Route]map[model.Fingerprint]*aggrGroup{}
	d.metrics.aggrGroups.Set(0)
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.mtx.Unlock()

	// 运行调度器子运行函数
	d.run(d.alerts.Subscribe())
	close(d.done)
}

// 调度器子运行方法，包含调度器运行的所有逻辑。
func (d *Dispatcher) run(it provider.AlertIterator) {
	// 创建清理ticker，其负责每30秒，检查所有的告警分组。
	cleanup := time.NewTicker(30 * time.Second)
	defer cleanup.Stop()

	defer it.Close()

	for {
		select {
		// 收到告警事件
		case alert, ok := <-it.Next():
			// 如果告警的通道被关闭，而且数据已经读取完毕，则返回。
			if !ok {
				// Iterator exhausted for some reason.
				// ------------------------------------
				// 记录下alert遍历器的错误
				if err := it.Err(); err != nil {
					level.Error(d.logger).Log("msg", "Error on alert update", "err", err)
				}
				return
			}

			level.Debug(d.logger).Log("msg", "Received alert", "alert", alert)

			// Log errors but keep trying.
			// ----------------------------------
			// 检查遍历器的错误，如果有错误则直接跳到下个循环
			if err := it.Err(); err != nil {
				level.Error(d.logger).Log("msg", "Error on alert update", "err", err)
				continue
			}

			// 根据这个告警的所有label来匹配分组，对匹配上的路由和告警进行处理
			now := time.Now()
			for _, r := range d.route.Match(alert.Labels) {
				d.processAlert(alert, r)
			}
			// 记录处理这个告警的时间到普罗米修斯指标中
			d.metrics.processingDuration.Observe(time.Since(now).Seconds())

		case <-cleanup.C: // 清理周期
			// 锁住调度器锁
			d.mtx.Lock()
			// 循环分组列表，并查看每个分组下的唯一标识分组
			for _, groups := range d.aggrGroups {
				for _, ag := range groups {
					// 如果这个唯一标识分组为空，终止并删除分组。
					// 普罗米修斯计数-1
					if ag.empty() {
						ag.stop()
						delete(groups, ag.fingerprint())
						d.metrics.aggrGroups.Dec()
					}
				}
			}

			d.mtx.Unlock()

		case <-d.ctx.Done():
			return
		}
	}
}

// AlertGroup represents how alerts exist within an aggrGroup.
// ------------------------------------------------------------------
// AlertGroup 表示告警是在 aggrGroup 告警聚合组里的详情。
type AlertGroup struct {
	Alerts   types.AlertSlice // 告警分组里的当前告警
	Labels   model.LabelSet   // 告警分组里的匹配标签
	Receiver string           // 告警分组的接收人
}

// AlertGroup 告警分组的的切片，以及排序方式（根据告警分组的匹配标签来进行排序）。
type AlertGroups []*AlertGroup

func (ag AlertGroups) Swap(i, j int) { ag[i], ag[j] = ag[j], ag[i] }
func (ag AlertGroups) Less(i, j int) bool {
	if ag[i].Labels.Equal(ag[j].Labels) {
		return ag[i].Receiver < ag[j].Receiver
	}
	return ag[i].Labels.Before(ag[j].Labels)
}
func (ag AlertGroups) Len() int { return len(ag) }

// Groups returns a slice of AlertGroups from the dispatcher's internal state.
// -----------------------------------------------------------------------------
// Groups 接受 routeFilter 路由过滤 和 alertFilter 告警过滤的两个方法，然后通过这两个
// 方法对现有的告警分组里和内容进行过滤，并最终返回过滤掉的告警分组和所有的告警接收人。
func (d *Dispatcher) Groups(routeFilter func(*Route) bool, alertFilter func(*types.Alert, time.Time) bool) (AlertGroups, map[model.Fingerprint][]string) {
	// 创建告警分组，并锁住调度器
	groups := AlertGroups{}

	d.mtx.RLock()
	defer d.mtx.RUnlock()

	// Keep a list of receivers for an alert to prevent checking each alert
	// again against all routes. The alert has already matched against this
	// route on ingestion.
	// -----------------------------------------------------------------------------
	// 保存每个告警的接收人列表，去避免将来需要检查所有的路由进行匹配。
	receivers := map[model.Fingerprint][]string{}

	now := time.Now()
	// 循环每个告警聚合组
	for route, ags := range d.aggrGroups {
		// 分组路由没有被过滤，则继续检查下一个
		if !routeFilter(route) {
			continue
		}

		// 循环每个聚合组，拿到具体的具体的分组
		for _, ag := range ags {
			// 获得分组的接收人和分组labels
			receiver := route.RouteOpts.Receiver
			alertGroup := &AlertGroup{
				Labels:   ag.labels,
				Receiver: receiver,
			}

			// 获取每个告警列表，并循环检查是否告警已经被filter方法过滤了。
			alerts := ag.alerts.List()
			filteredAlerts := make([]*types.Alert, 0, len(alerts))
			for _, a := range alerts {
				if !alertFilter(a, now) {
					continue
				}

				// 如果已被过滤掉，则添加接收人
				fp := a.Fingerprint()
				if r, ok := receivers[fp]; ok {
					// Receivers slice already exists. Add
					// the current receiver to the slice.
					receivers[fp] = append(r, receiver)
				} else {
					// First time we've seen this alert fingerprint.
					// Initialize a new receivers slice.
					receivers[fp] = []string{receiver}
				}

				filteredAlerts = append(filteredAlerts, a)
			}

			// 如果已经过滤掉的告警数为0，则忽略。
			if len(filteredAlerts) == 0 {
				continue
			}

			// 添加这个告警组到返回的告警组切片中
			alertGroup.Alerts = filteredAlerts

			groups = append(groups, alertGroup)
		}
	}

	// 进行排序和返回
	sort.Sort(groups)
	for i := range groups {
		sort.Sort(groups[i].Alerts)
	}
	for i := range receivers {
		sort.Strings(receivers[i])
	}

	return groups, receivers
}

// Stop the dispatcher.
// ---------------------------
// 停止调度器。调用cancel方法，并等待done通道，完成则代表已经取消成功。
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.mtx.Lock()
	if d.cancel == nil {
		return
	}
	d.cancel()
	d.cancel = nil
	d.mtx.Unlock()

	<-d.done
}

// notifyFunc is a function that performs notification for the alert
// with the given fingerprint. It aborts on context cancelation.
// Returns false iff notifying failed.
type notifyFunc func(context.Context, ...*types.Alert) bool

// processAlert determines in which aggregation group the alert falls
// and inserts it.
// ------------------------------------------------------------------
// 处理告警，得到相应分组，并对相应的分组插入这个告警。
// @param alert 告警结构体
// @param route 已经匹配上的分组路由
func (d *Dispatcher) processAlert(alert *types.Alert, route *Route) {
	// 根据分组路由的信息，获得此分组下的匹配中的labels。
	// 并根据所得labels得到唯一id(指纹 finger print)。
	groupLabels := getGroupLabels(alert, route)
	fp := groupLabels.Fingerprint()

	// 加锁进行hashmap操作
	d.mtx.Lock()
	defer d.mtx.Unlock()

	// 通过分组路由获得分组map，如果分组列表hashmap不存在这个分组，
	// 则进行创建。分组map里面key为分组finger print，value为具
	// 体唯一标识的分组。
	group, ok := d.aggrGroups[route]
	if !ok {
		group = map[model.Fingerprint]*aggrGroup{}
		d.aggrGroups[route] = group
	}

	// If the group does not exist, create it.
	// ----------------------------------------------------
	// 假如当前告警的group labels的指纹在这个告警分组map里找不到，
	// 则进行分组的创建。
	ag, ok := group[fp]
	if !ok {
		ag = newAggrGroup(d.ctx, groupLabels, route, d.timeout, d.logger)
		group[fp] = ag
		// 普罗米修斯的分组数量指标进行加一
		d.metrics.aggrGroups.Inc()

		// 开启新的协成，运行此告警指纹的分组
		go ag.run(func(ctx context.Context, alerts ...*types.Alert) bool {
			// 根据当前context的状态，来进行告警的处理。
			_, _, err := d.stage.Exec(ctx, d.logger, alerts...)
			if err != nil {
				lvl := level.Error(d.logger)
				if ctx.Err() == context.Canceled {
					// It is expected for the context to be canceled on
					// configuration reload or shutdown. In this case, the
					// message should only be logged at the debug level.
					// ---------------------------------------------------
					// 假如错误是因为reload或者关闭而导致的，那样日志等级为debug
					lvl = level.Debug(d.logger)
				}
				lvl.Log("msg", "Notify for alerts failed", "num_alerts", len(alerts), "err", err)
			}
			return err == nil
		})
	}

	// 插入alert到这个唯一标识的分组里。
	ag.insert(alert)
}

// 根据分组路由的信息，获得此分组下的所有label。
func getGroupLabels(alert *types.Alert, route *Route) model.LabelSet {
	groupLabels := model.LabelSet{}
	for ln, lv := range alert.Labels {
		// 检查路由规则，如果label在路由规则里，或者路由规则为全label group，
		// 则加入label到label set。
		if _, ok := route.RouteOpts.GroupBy[ln]; ok || route.RouteOpts.GroupByAll {
			groupLabels[ln] = lv
		}
	}

	return groupLabels
}

// aggrGroup aggregates alert fingerprints into groups to which a
// common set of routing options applies.
// It emits notifications in the specified intervals.
type aggrGroup struct {
	labels   model.LabelSet
	opts     *RouteOpts
	logger   log.Logger
	routeKey string

	alerts  *store.Alerts
	ctx     context.Context
	cancel  func()
	done    chan struct{}
	next    *time.Timer
	timeout func(time.Duration) time.Duration

	mtx        sync.RWMutex
	hasFlushed bool
}

// newAggrGroup returns a new aggregation group.
// ------------------------------------------------------------------
// 创建一个新的告警聚合分组，在同一时间下aggrGroup只会存在一个对象。这个对象
// 被保存在一个hash里面。
func newAggrGroup(ctx context.Context, labels model.LabelSet, r *Route, to func(time.Duration) time.Duration, logger log.Logger) *aggrGroup {
	if to == nil {
		to = func(d time.Duration) time.Duration { return d }
	}
	ag := &aggrGroup{
		labels:   labels,
		routeKey: r.Key(),
		opts:     &r.RouteOpts,
		timeout:  to,
		alerts:   store.NewAlerts(),
		done:     make(chan struct{}),
	}
	ag.ctx, ag.cancel = context.WithCancel(ctx)

	ag.logger = log.With(logger, "aggrGroup", ag)

	// Set an initial one-time wait before flushing
	// the first batch of notifications.
	// ------------------------------------------------
	// 设置一次使用的group wait timer，发送第一批的告警消息
	ag.next = time.NewTimer(ag.opts.GroupWait)

	return ag
}

// 获取聚合组的指纹
func (ag *aggrGroup) fingerprint() model.Fingerprint {
	return ag.labels.Fingerprint()
}

// 获取聚合组的分组KEY
func (ag *aggrGroup) GroupKey() string {
	return fmt.Sprintf("%s:%s", ag.routeKey, ag.labels)
}

// String 方法，获取聚合组的分组KEY
func (ag *aggrGroup) String() string {
	return ag.GroupKey()
}

// run 运行具体的聚合分组，当这个方法被运行时，则代表已经有告警在这个聚合分组里了。
func (ag *aggrGroup) run(nf notifyFunc) {
	defer close(ag.done)
	defer ag.next.Stop()

	for {
		select {
		case now := <-ag.next.C: // 分组的 timer 触发
			// Give the notifications time until the next flush to
			// finish before terminating them.
			// --------------------------------------------------------------
			// 创建发送告警的超时context，来避免下一轮告警要消息要触发了，但是上一轮
			// 的消息还没有发送成功。
			ctx, cancel := context.WithTimeout(ag.ctx, ag.timeout(ag.opts.GroupInterval))

			// The now time we retrieve from the ticker is the only reliable
			// point of time reference for the subsequent notification pipeline.
			// Calculating the current time directly is prone to flaky behavior,
			// which usually only becomes apparent in tests.
			// --------------------------------------------------------------
			// 设置当前的触发时间到context里面，为了时间的准确性
			ctx = notify.WithNow(ctx, now)

			// Populate context with information needed along the pipeline.
			// --------------------------------------------------------------
			// 设置其他的一些聚合组的信息到上下文
			ctx = notify.WithGroupKey(ctx, ag.GroupKey())
			ctx = notify.WithGroupLabels(ctx, ag.labels)
			ctx = notify.WithReceiverName(ctx, ag.opts.Receiver)
			ctx = notify.WithRepeatInterval(ctx, ag.opts.RepeatInterval)

			// Wait the configured interval before calling flush again.
			// --------------------------------------------------------------
			// 标识当前状态为已经FLUSH，并重置下一次timer的等待时间
			ag.mtx.Lock()
			ag.next.Reset(ag.opts.GroupInterval)
			ag.hasFlushed = true
			ag.mtx.Unlock()

			// 通知告警到接收人
			ag.flush(func(alerts ...*types.Alert) bool {
				return nf(ctx, alerts...)
			})

			cancel()

		case <-ag.ctx.Done():
			return
		}
	}
}

// 停止分组方法，cancel和并阻塞等待done通道
func (ag *aggrGroup) stop() {
	// Calling cancel will terminate all in-process notifications
	// and the run() loop.
	ag.cancel()
	<-ag.done
}

// insert inserts the alert into the aggregation group.
// ------------------------------------------------------
// 插入告警到这个唯一标识分组里
func (ag *aggrGroup) insert(alert *types.Alert) {
	// 设置这个告警消息
	if err := ag.alerts.Set(alert); err != nil {
		level.Error(ag.logger).Log("msg", "error on set alert", "err", err)
	}

	// Immediately trigger a flush if the wait duration for this
	// alert is already over.
	ag.mtx.Lock()
	defer ag.mtx.Unlock()
	// 如果现在分组里的消息没有flush，并且计算出现在时间在分组等待时间之后，
	// 可以发送这个分组。则reset这个分组的timer来触发flush。
	if !ag.hasFlushed && alert.StartsAt.Add(ag.opts.GroupWait).Before(time.Now()) {
		ag.next.Reset(0)
	}
}

// 检查聚合组是否为空，是否没有告警
func (ag *aggrGroup) empty() bool {
	return ag.alerts.Empty()
}

// flush sends notifications for all new alerts.
// ------------------------------------------------------
// flush 发送所有新的告警消息
func (ag *aggrGroup) flush(notify func(...*types.Alert) bool) {
	// 检查聚合组里是否为空
	if ag.empty() {
		return
	}

	var (
		alerts      = ag.alerts.List()
		alertsSlice = make(types.AlertSlice, 0, len(alerts))
		now         = time.Now()
	)
	for _, alert := range alerts {
		a := *alert
		// Ensure that alerts don't resolve as time move forwards.
		// -------------------------------------------------------
		// 确保告警解决时间不是将来的一个时间
		if !a.ResolvedAt(now) {
			a.EndsAt = time.Time{}
		}
		alertsSlice = append(alertsSlice, &a)
	}
	sort.Stable(alertsSlice)

	level.Debug(ag.logger).Log("msg", "flushing", "alerts", fmt.Sprintf("%v", alertsSlice))

	// 发送告警，如果发送成功
	if notify(alertsSlice...) {
		for _, a := range alertsSlice {
			// Only delete if the fingerprint has not been inserted
			// again since we notified about it.
			// -------------------------------------------------------
			// 删除已经解决的告警，并且只有这个告警并没有在出处理过程中被
			// 重新插入才会删除，防止并发问题。
			fp := a.Fingerprint()
			got, err := ag.alerts.Get(fp)
			if err != nil {
				// This should never happen.
				level.Error(ag.logger).Log("msg", "failed to get alert", "err", err, "alert", a.String())
				continue
			}
			if a.Resolved() && got.UpdatedAt == a.UpdatedAt {
				if err := ag.alerts.Delete(fp); err != nil {
					level.Error(ag.logger).Log("msg", "error on delete alert", "err", err, "alert", a.String())
				}
			}
		}
	}
}
