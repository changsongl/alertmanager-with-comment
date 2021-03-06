// Copyright 2015 Prometheus Team
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

package notify

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cespare/xxhash"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/cluster"
	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/nflog"
	"github.com/prometheus/alertmanager/nflog/nflogpb"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/types"
)

// ResolvedSender returns true if resolved notifications should be sent.
// -----------------------------------------------------------------------------
// ResolvedSender 接口的方法返回true，当已经解决的通知应该被发送。
type ResolvedSender interface {
	SendResolved() bool
}

// MinTimeout is the minimum timeout that is set for the context of a call
// to a notification pipeline.
// -----------------------------------------------------------------------------
// MinTimeout 是最小超时时间，当去调用一个消息通知的管道，需要设置context。
// 超时时间会被设置到context里。
const MinTimeout = 10 * time.Second

// Notifier notifies about alerts under constraints of the given context. It
// returns an error if unsuccessful and a flag whether the error is
// recoverable. This information is useful for a retry logic.
// -----------------------------------------------------------------------------
// Notifier 接口，在限定的上下文下进行通知告警。当通知失败时将返回错误，并且有一个
// flag用来判断是否这个错误是可以恢复的。这个信息对于重试非常有效。
type Notifier interface {
	Notify(context.Context, ...*types.Alert) (bool, error)
}

// Integration wraps a notifier and its configuration to be uniquely identified
// by name and index from its origin in the configuration.
// -----------------------------------------------------------------------------
// Integration 是消息通知的集成类，包含一个 Notifier 和其配置，
// 通过name和index来作为唯一命名
type Integration struct {
	notifier Notifier
	rs       ResolvedSender
	name     string
	idx      int
}

// NewIntegration returns a new integration.
// -----------------------------------------------------------------------------
// NewIntegration 创建integration对象。
func NewIntegration(notifier Notifier, rs ResolvedSender, name string, idx int) Integration {
	return Integration{
		notifier: notifier,
		rs:       rs,
		name:     name,
		idx:      idx,
	}
}

// Notify implements the Notifier interface.
// -----------------------------------------------------------------------------
// Notify 实现了 Notifier 接口。
func (i *Integration) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	return i.notifier.Notify(ctx, alerts...)
}

// SendResolved implements the ResolvedSender interface.
// -----------------------------------------------------------------------------
// SendResolved 实现了 ResolvedSender 接口。这里是对Integration里的 ResolvedSender
// 对象的方法进行了封装。
func (i *Integration) SendResolved() bool {
	return i.rs.SendResolved()
}

// Name returns the name of the integration.
// -----------------------------------------------------------------------------
// Name 返回integration的名字
func (i *Integration) Name() string {
	return i.name
}

// Index returns the index of the integration.
// -----------------------------------------------------------------------------
// Index 返回integration的索引
func (i *Integration) Index() int {
	return i.idx
}

// String implements the Stringer interface.
// -----------------------------------------------------------------------------
// String 返回integration的名字和索引的拼接名，主要被用在日志打印。
func (i *Integration) String() string {
	return fmt.Sprintf("%s[%d]", i.name, i.idx)
}

// notifyKey defines a custom type with which a context is populated to
// avoid accidental collisions.
// -----------------------------------------------------------------------------
// notifyKey 定义一个自定义的类型，用来设置和读取数据到context，防止意外碰撞。
type notifyKey int

const (
	keyReceiverName notifyKey = iota
	keyRepeatInterval
	keyGroupLabels
	keyGroupKey
	keyFiringAlerts
	keyResolvedAlerts
	keyNow
)

// 设置和读取数据到context的方法。
// -----------------------------------------------------------------------------
// WithReceiverName populates a context with a receiver name.
func WithReceiverName(ctx context.Context, rcv string) context.Context {
	return context.WithValue(ctx, keyReceiverName, rcv)
}

// WithGroupKey populates a context with a group key.
func WithGroupKey(ctx context.Context, s string) context.Context {
	return context.WithValue(ctx, keyGroupKey, s)
}

// WithFiringAlerts populates a context with a slice of firing alerts.
func WithFiringAlerts(ctx context.Context, alerts []uint64) context.Context {
	return context.WithValue(ctx, keyFiringAlerts, alerts)
}

// WithResolvedAlerts populates a context with a slice of resolved alerts.
func WithResolvedAlerts(ctx context.Context, alerts []uint64) context.Context {
	return context.WithValue(ctx, keyResolvedAlerts, alerts)
}

// WithGroupLabels populates a context with grouping labels.
func WithGroupLabels(ctx context.Context, lset model.LabelSet) context.Context {
	return context.WithValue(ctx, keyGroupLabels, lset)
}

// WithNow populates a context with a now timestamp.
func WithNow(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, keyNow, t)
}

// WithRepeatInterval populates a context with a repeat interval.
func WithRepeatInterval(ctx context.Context, t time.Duration) context.Context {
	return context.WithValue(ctx, keyRepeatInterval, t)
}

// RepeatInterval extracts a repeat interval from the context. Iff none exists, the
// second argument is false.
func RepeatInterval(ctx context.Context) (time.Duration, bool) {
	v, ok := ctx.Value(keyRepeatInterval).(time.Duration)
	return v, ok
}

// ReceiverName extracts a receiver name from the context. Iff none exists, the
// second argument is false.
func ReceiverName(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyReceiverName).(string)
	return v, ok
}

// GroupKey extracts a group key from the context. Iff none exists, the
// second argument is false.
func GroupKey(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(keyGroupKey).(string)
	return v, ok
}

// GroupLabels extracts grouping label set from the context. Iff none exists, the
// second argument is false.
func GroupLabels(ctx context.Context) (model.LabelSet, bool) {
	v, ok := ctx.Value(keyGroupLabels).(model.LabelSet)
	return v, ok
}

// Now extracts a now timestamp from the context. Iff none exists, the
// second argument is false.
func Now(ctx context.Context) (time.Time, bool) {
	v, ok := ctx.Value(keyNow).(time.Time)
	return v, ok
}

// FiringAlerts extracts a slice of firing alerts from the context.
// Iff none exists, the second argument is false.
func FiringAlerts(ctx context.Context) ([]uint64, bool) {
	v, ok := ctx.Value(keyFiringAlerts).([]uint64)
	return v, ok
}

// ResolvedAlerts extracts a slice of firing alerts from the context.
// Iff none exists, the second argument is false.
func ResolvedAlerts(ctx context.Context) ([]uint64, bool) {
	v, ok := ctx.Value(keyResolvedAlerts).([]uint64)
	return v, ok
}

// -----------------------------------------------------------------------------

// A Stage processes alerts under the constraints of the given context.
// -----------------------------------------------------------------------------
// Stage 在所给予的上下文限制中处理所有告警。
type Stage interface {
	Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)
}

// StageFunc wraps a function to represent a Stage.
// -----------------------------------------------------------------------------
// StageFunc 包裹一个方法，这个方法将继承 Stage 接口。
type StageFunc func(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error)

// Exec implements Stage interface.
// -----------------------------------------------------------------------------
// Exec 是包裹后的 StageFunc 用来实现 Stage 接口的。其实实现本身就是调用
// StageFunc 自己。基操，常用在配置里。
func (f StageFunc) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	return f(ctx, l, alerts...)
}

// NotificationLog 用于集群内部进行Log和Query查询的接口。
type NotificationLog interface {
	Log(r *nflogpb.Receiver, gkey string, firingAlerts, resolvedAlerts []uint64) error
	Query(params ...nflog.QueryParam) ([]*nflogpb.Entry, error)
}

// metrics 用来统计通知相关数据指标到普罗米修斯
type metrics struct {
	numNotifications           *prometheus.CounterVec   // 通知数量
	numFailedNotifications     *prometheus.CounterVec   // 通知失败数量
	notificationLatencySeconds *prometheus.HistogramVec // 通知相应时延
}

// 发送消息的普罗米修斯指标，统计消息数量，消息通知失败数量，消息延迟
func newMetrics(r prometheus.Registerer) *metrics {
	// 创建三个普罗米修斯指标，并且根据不同渠道来进行分别统计
	m := &metrics{
		numNotifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notifications_total",
			Help:      "The total number of attempted notifications.",
		}, []string{"integration"}),
		numFailedNotifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "alertmanager",
			Name:      "notifications_failed_total",
			Help:      "The total number of failed notifications.",
		}, []string{"integration"}),
		notificationLatencySeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "alertmanager",
			Name:      "notification_latency_seconds",
			Help:      "The latency of notifications in seconds.",
			Buckets:   []float64{1, 5, 10, 15, 20},
		}, []string{"integration"}),
	}
	for _, integration := range []string{
		"email",
		"pagerduty",
		"wechat",
		"pushover",
		"slack",
		"opsgenie",
		"webhook",
		"victorops",
	} {
		m.numNotifications.WithLabelValues(integration)
		m.numFailedNotifications.WithLabelValues(integration)
		m.notificationLatencySeconds.WithLabelValues(integration)
	}

	// 注册到普罗米修斯默认注册器中
	r.MustRegister(m.numNotifications, m.numFailedNotifications, m.notificationLatencySeconds)
	return m
}

// PipelineBuilder 消息路由管道构建者，负责构建整个路由的流程阶段。
type PipelineBuilder struct {
	metrics *metrics
}

// NewPipelineBuilder 创建消息路由管道构建者
func NewPipelineBuilder(r prometheus.Registerer) *PipelineBuilder {
	return &PipelineBuilder{
		metrics: newMetrics(r),
	}
}

// New returns a map of receivers to Stages.
// --------------------------------------------------------
// New 返回一个接收人map运行的Stages，每个接收人，都会经历固定的Gossip，
// 抑制和静默阶段。然后根据receiver的不同，创建各自的分组阶段。
func (pb *PipelineBuilder) New(
	receivers map[string][]Integration,
	wait func() time.Duration,
	inhibitor *inhibit.Inhibitor,
	silencer *silence.Silencer,
	notificationLog NotificationLog,
	peer *cluster.Peer,
) RoutingStage {
	rs := make(RoutingStage, len(receivers))

	// 创建gossip协议检查就绪阶段，和抑制和静默的静音阶段。
	ms := NewGossipSettleStage(peer)
	is := NewMuteStage(inhibitor)
	ss := NewMuteStage(silencer)

	// 根据接收人创建，分组等待，去重，重试，通知阶段。
	// 这里每个接收人都有一个独立的接收人阶段，并且
	// 它们公用gossip就绪和静音阶段。
	for name := range receivers {
		// 为每一个接收方式的所有接收人创建扇出的阶段
		st := createReceiverStage(name, receivers[name], wait, notificationLog, pb.metrics)
		rs[name] = MultiStage{ms, is, ss, st}
	}
	return rs
}

// createReceiverStage creates a pipeline of stages for a receiver.
// ------------------------------------------------------------------------------
// createReceiverStage 为一个接口人创建一个扇出的阶段管道。这里循环这个接受方式的
// 每一个接收人，然后为每个接收人创建一个MultiStage，用来装载多个阶段。每个Multistage
// 包含一个等待阶段，去重阶段，重试阶段和设置通知信息阶段。最后把每个MultiStage都添加
// 到扇出阶段，并进行返回。
func createReceiverStage(
	name string,
	integrations []Integration,
	wait func() time.Duration,
	notificationLog NotificationLog,
	metrics *metrics,
) Stage {
	var fs FanoutStage
	for i := range integrations {
		recv := &nflogpb.Receiver{
			GroupName:   name,
			Integration: integrations[i].Name(),
			Idx:         uint32(integrations[i].Index()),
		}
		var s MultiStage
		// 等待阶段
		s = append(s, NewWaitStage(wait))
		// 去重阶段
		s = append(s, NewDedupStage(&integrations[i], notificationLog, recv))
		// 重试阶段
		s = append(s, NewRetryStage(integrations[i], name, metrics))
		// 设置通知信息阶段
		s = append(s, NewSetNotifiesStage(notificationLog, recv))

		fs = append(fs, s)
	}
	return fs
}

// RoutingStage executes the inner stages based on the receiver specified in
// the context.
// ------------------------------------------------------------------------------
// RoutingStage 根据context里的receiver来决定，执行内部的阶段，
type RoutingStage map[string]Stage

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// RoutingStage 执行方法，从上下文中拿到这个告警的接收人。然后找到对应的 MultiStage,
// 继续执行 MultiStage 的阶段。
func (rs RoutingStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	receiver, ok := ReceiverName(ctx)
	if !ok {
		return ctx, nil, errors.New("receiver missing")
	}

	s, ok := rs[receiver]
	if !ok {
		return ctx, nil, errors.New("stage for receiver missing")
	}

	return s.Exec(ctx, l, alerts...)
}

// A MultiStage executes a series of stages sequentially.
// ------------------------------------------------------------------------------
// MultiStage 包含一个阶段切片，用来执行这里面的所有阶段。
type MultiStage []Stage

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 循环执行 MultiStage 里面的每一个阶段。MultiStage 主要使用两个场景。
// 场景一： RoutingStage 的map[receiver name] MultiStage。
//        里面有集群Gossip阶段，静默Mute阶段，抑制Mute阶段，Receiver阶段。
// 场景二： FanoutStage 的切片，里面每个元素是一个 MultiStage。
//        里面有分组等待阶段，去重阶段，重试阶段，设置通知阶段。
func (ms MultiStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var err error
	for _, s := range ms {
		if len(alerts) == 0 {
			return ctx, nil, nil
		}

		ctx, alerts, err = s.Exec(ctx, l, alerts...)
		if err != nil {
			return ctx, nil, err
		}
	}
	return ctx, alerts, nil
}

// FanoutStage executes its stages concurrently
// ------------------------------------------------------------------------------
// FanoutStage 并发的执行它的阶段
type FanoutStage []Stage

// Exec attempts to execute all stages concurrently and discards the results.
// It returns its input alerts and a types.MultiError if one or more stages fail.
// ------------------------------------------------------------------------------
// Exec 尝试并发的执行全部的阶段，并且丢弃结果。它将返回它的入参告警和一个多重错误收集器。
func (fs FanoutStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var (
		wg sync.WaitGroup
		me types.MultiError
	)
	wg.Add(len(fs))

	// go 每一个 Stage，并把错误添加到多重错误收集器中
	for _, s := range fs {
		go func(s Stage) {
			if _, _, err := s.Exec(ctx, l, alerts...); err != nil {
				me.Add(err)
			}
			wg.Done()
		}(s)
	}
	wg.Wait()

	// 如果多重错误收集器里有错误，则把错误进行返回
	if me.Len() > 0 {
		return ctx, alerts, &me
	}
	return ctx, alerts, nil
}

// GossipSettleStage waits until the Gossip has settled to forward alerts.
// ------------------------------------------------------------------------------
// GossipSettleStage 此阶段，一直等待gossip协议准备就绪状态，等待其他的节点就绪。
type GossipSettleStage struct {
	peer *cluster.Peer
}

// NewGossipSettleStage returns a new GossipSettleStage.
// ------------------------------------------------------------------------------
// NewGossipSettleStage 返回一个gossip检查就绪阶段，正如我们所知集群内的节点，
// 通过gossip协议进行通讯。
func NewGossipSettleStage(p *cluster.Peer) *GossipSettleStage {
	return &GossipSettleStage{peer: p}
}

// Exec 等到其他alertmanager节点就绪
func (n *GossipSettleStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	if n.peer != nil {
		n.peer.WaitReady()
	}
	return ctx, alerts, nil
}

// MuteStage filters alerts through a Muter.
// ------------------------------------------------------------------------------
// MuteStage 静音阶段，通过muter来过滤告警。
type MuteStage struct {
	muter types.Muter
}

// NewMuteStage return a new MuteStage.
// ------------------------------------------------------------------------------
// NewMuteStage 返回一个 MuteStage
func NewMuteStage(m types.Muter) *MuteStage {
	return &MuteStage{muter: m}
}

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 实现 Stage 接口。通过调用muter，过滤掉静默和抑制的告警，返回过滤后的告警。
func (n *MuteStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var filtered []*types.Alert
	for _, a := range alerts {
		// TODO(fabxc): increment total alerts counter.
		// Do not send the alert if muted.
		if !n.muter.Mutes(a.Labels) {
			filtered = append(filtered, a)
		}
		// TODO(fabxc): increment muted alerts counter if muted.
	}
	return ctx, filtered, nil
}

// WaitStage waits for a certain amount of time before continuing or until the
// context is done.
// ------------------------------------------------------------------------------
// WaitStage 等待特性的一段时间，在继续下一阶段行为。或者知道context已经done。
type WaitStage struct {
	wait func() time.Duration
}

// NewWaitStage returns a new WaitStage.
// ------------------------------------------------------------------------------
// NewWaitStage 返回一个新的等待阶段。设置wait方法到阶段之中。
func NewWaitStage(wait func() time.Duration) *WaitStage {
	return &WaitStage{
		wait: wait,
	}
}

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 实现了 Stage 接口，等待特定的一段时间，或者ctx.Done。取决于哪个事件先完成。
// 如果ctx.Done 先发生，则返回上下文里的错误。
func (ws *WaitStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	select {
	case <-time.After(ws.wait()):
	case <-ctx.Done():
		return ctx, nil, ctx.Err()
	}
	return ctx, alerts, nil
}

// DedupStage filters alerts.
// Filtering happens based on a notification log.
// ------------------------------------------------------------------------------
// DedupStage 过滤告警。
// 过滤的发生是基于通知的日志。
type DedupStage struct {
	rs    ResolvedSender
	nflog NotificationLog
	recv  *nflogpb.Receiver

	now  func() time.Time
	hash func(*types.Alert) uint64
}

// NewDedupStage wraps a DedupStage that runs against the given notification log.
// ------------------------------------------------------------------------------
// NewDedupStage 包裹一个 DedupStage， 这个。
func NewDedupStage(rs ResolvedSender, l NotificationLog, recv *nflogpb.Receiver) *DedupStage {
	return &DedupStage{
		rs:    rs,
		nflog: l,
		recv:  recv,
		now:   utcNow,
		hash:  hashAlert,
	}
}

func utcNow() time.Time {
	return time.Now().UTC()
}

var hashBuffers = sync.Pool{}

func getHashBuffer() []byte {
	b := hashBuffers.Get()
	if b == nil {
		return make([]byte, 0, 1024)
	}
	return b.([]byte)
}

func putHashBuffer(b []byte) {
	b = b[:0]
	//lint:ignore SA6002 relax staticcheck verification.
	hashBuffers.Put(b)
}

func hashAlert(a *types.Alert) uint64 {
	const sep = '\xff'

	b := getHashBuffer()
	defer putHashBuffer(b)

	names := make(model.LabelNames, 0, len(a.Labels))

	for ln := range a.Labels {
		names = append(names, ln)
	}
	sort.Sort(names)

	for _, ln := range names {
		b = append(b, string(ln)...)
		b = append(b, sep)
		b = append(b, string(a.Labels[ln])...)
		b = append(b, sep)
	}

	hash := xxhash.Sum64(b)

	return hash
}

// needsUpdate 检查是否需要更新现在准备发送的告警、
func (n *DedupStage) needsUpdate(entry *nflogpb.Entry, firing, resolved map[uint64]struct{}, repeat time.Duration) bool {
	// If we haven't notified about the alert group before, notify right away
	// unless we only have resolved alerts.
	// ------------------------------------------------------------------------------
	// entry 是空，则代表没有发送过这个告警组，则立刻发送，除非我们只有已恢复的告警。
	if entry == nil {
		return len(firing) > 0
	}

	// 如果现在处理的告警不是entry的子集，则代表有新的告警，则需要更新。
	if !entry.IsFiringSubset(firing) {
		return true
	}

	// Notify about all alerts being resolved.
	// This is done irrespective of the send_resolved flag to make sure that
	// the firing alerts are cleared from the notification log.
	// ------------------------------------------------------------------------------
	// 如果现在告警的为0，通知全部的告警已经被解决。
	// 这个是发生不考虑是应为send_resolved的方法解决的，需要确保所有的告警状态被清楚。
	// 需要更新到消息通知日志。
	if len(firing) == 0 {
		// If the current alert group and last notification contain no firing
		// alert, it means that some alerts have been fired and resolved during the
		// last interval. In this case, there is no need to notify the receiver
		// since it doesn't know about them.
		// ------------------------------------------------------------------------------
		// 如果当前的告警组和上次的通知日志不包含任何正在告警中的告警。这个意味着一些告警已经在
		// 上个周期已经被处理了。在这个场景下，不需要再通知接收人。
		return len(entry.FiringAlerts) > 0
	}

	// 如果已经解决的告警需要被发送，现在的已解决告警，并不是日志中告警的子集。
	// 则代表有新的解决告警，需要被更新。
	if n.rs.SendResolved() && !entry.IsResolvedSubset(resolved) {
		return true
	}

	// Nothing changed, only notify if the repeat interval has passed.
	// ------------------------------------------------------------------------------
	// 没有新的告警发生或状态的变化，只有当repeat间隔已经过去后，才需要更新。
	return entry.Timestamp.Before(n.now().Add(-repeat))
}

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 实现了 Stage 接口，将进行告警的去重，并获取消息日志，查看是否需要更新日志状态。
func (n *DedupStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	// 获得group key
	gkey, ok := GroupKey(ctx)
	if !ok {
		return ctx, nil, errors.New("group key missing")
	}

	// 检查repeat interval
	repeatInterval, ok := RepeatInterval(ctx)
	if !ok {
		return ctx, nil, errors.New("repeat interval missing")
	}

	// 区分告警，分别把正在告警和已恢复的告警进行分类。
	firingSet := map[uint64]struct{}{}
	resolvedSet := map[uint64]struct{}{}
	firing := []uint64{}
	resolved := []uint64{}

	var hash uint64
	for _, a := range alerts {
		hash = n.hash(a)
		if a.Resolved() {
			resolved = append(resolved, hash)
			resolvedSet[hash] = struct{}{}
		} else {
			firing = append(firing, hash)
			firingSet[hash] = struct{}{}
		}
	}

	// 存储正在告警和已恢复的告警到上下文中
	ctx = WithFiringAlerts(ctx, firing)
	ctx = WithResolvedAlerts(ctx, resolved)

	// 查询gossip日志，如果有通讯日志，则需要更新现在的告警。
	entries, err := n.nflog.Query(nflog.QGroupKey(gkey), nflog.QReceiver(n.recv))
	if err != nil && err != nflog.ErrNotFound {
		return ctx, nil, err
	}

	var entry *nflogpb.Entry
	switch len(entries) {
	case 0:
	case 1:
		entry = entries[0]
	default:
		return ctx, nil, errors.Errorf("unexpected entry result size %d", len(entries))
	}

	// 检查是否需要更新告警
	if n.needsUpdate(entry, firingSet, resolvedSet, repeatInterval) {
		return ctx, alerts, nil
	}
	return ctx, nil, nil
}

// RetryStage notifies via passed integration with exponential backoff until it
// succeeds. It aborts if the context is canceled or timed out.
// ------------------------------------------------------------------------------
// RetryStage 通重试阶段。知告警到通知人，通过指数的增长重试延迟不断发送，
// 一直到成功为止。这个重试过程会停止，当context被取消或超时。
type RetryStage struct {
	integration Integration
	groupName   string
	metrics     *metrics
}

// NewRetryStage returns a new instance of a RetryStage.
// ------------------------------------------------------------------------------
// NewRetryStage 返回一个重试阶段。
func NewRetryStage(i Integration, groupName string, metrics *metrics) *RetryStage {
	return &RetryStage{
		integration: i,
		groupName:   groupName,
		metrics:     metrics,
	}
}

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 实现 Stage 接口。进行告警的发送，并重试到发送成功或取消或超时为止。
func (r RetryStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	var sent []*types.Alert

	// If we shouldn't send notifications for resolved alerts, but there are only
	// resolved alerts, report them all as successfully notified (we still want the
	// notification log to log them for the next run of DedupStage).
	// ------------------------------------------------------------------------------
	// 如果我们不应该发送已解决掉的消息，但是这里只有已解决的告警。报告它们全部都已经成功通知。
	// 因为我们还是需要消息日志去记录它们，这个日志将用来下一次的去重阶段。
	if !r.integration.SendResolved() {
		firing, ok := FiringAlerts(ctx)
		if !ok {
			return ctx, nil, errors.New("firing alerts missing")
		}
		// 没有正在告警中的告警，并且不需要发送已解决告警，因此直接返回。
		if len(firing) == 0 {
			return ctx, alerts, nil
		}

		// 找到不是已解决的告警
		for _, a := range alerts {
			if a.Status() != model.AlertResolved {
				sent = append(sent, a)
			}
		}
	} else { // 因为需要发送已解决的告警，因此发送全部告警。
		sent = alerts
	}

	// 创建指数间隔重试器，并设置为永远重试。
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 0 // Always retry.

	tick := backoff.NewTicker(b)
	defer tick.Stop()

	var (
		i    = 0
		iErr error
	)
	l = log.With(l, "receiver", r.groupName, "integration", r.integration.String())

	// 开始循环重试发送消息
	for {
		i++
		// Always check the context first to not notify again.
		// 总是先查看是否context已经取消或者超时了。
		select {
		case <-ctx.Done():
			if iErr == nil {
				iErr = ctx.Err()
			}

			return ctx, nil, errors.Wrapf(iErr, "%s/%s: notify retry canceled after %d attempts", r.groupName, r.integration.String(), i)
		default:
		}

		// 尝试发送消息
		select {
		case <-tick.C:
			now := time.Now()
			// 进行通知，并记录状态到普罗米修斯指标
			retry, err := r.integration.Notify(ctx, sent...)
			r.metrics.notificationLatencySeconds.WithLabelValues(r.integration.Name()).Observe(time.Since(now).Seconds())
			r.metrics.numNotifications.WithLabelValues(r.integration.Name()).Inc()

			// 有错误，则进行错误指标记录。
			if err != nil {
				r.metrics.numFailedNotifications.WithLabelValues(r.integration.Name()).Inc()

				// 判断是否是可重试的错误，如果不是，则直接返回错误。
				if !retry {
					return ctx, alerts, errors.Wrapf(err, "%s/%s: notify retry canceled due to unrecoverable error after %d attempts", r.groupName, r.integration.String(), i)
				}
				if ctx.Err() == nil && (iErr == nil || err.Error() != iErr.Error()) {
					// Log the error if the context isn't done and the error isn't the same as before.
					level.Warn(l).Log("msg", "Notify attempt failed, will retry later", "attempts", i, "err", err)
				}

				// Save this error to be able to return the last seen error by an
				// integration upon context timeout.
				// ------------------------------------------------------------------------------
				// 保存这个error，为了能至少返回上一次见到过的error给调用方。
				// 走到这里后，会进行下一次的发送重试，或因为context超时或取消而终止。
				iErr = err
			} else {
				// 通知成功，记录日志并返回。
				lvl := level.Debug(l)
				if i > 1 {
					lvl = level.Info(l)
				}
				lvl.Log("msg", "Notify success", "attempts", i)
				return ctx, alerts, nil
			}
		case <-ctx.Done():
			if iErr == nil {
				iErr = ctx.Err()
			}

			return ctx, nil, errors.Wrapf(iErr, "%s/%s: notify retry canceled after %d attempts", r.groupName, r.integration.String(), i)
		}
	}
}

// SetNotifiesStage sets the notification information about passed alerts. The
// passed alerts should have already been sent to the receivers.
// ------------------------------------------------------------------------------
// SetNotifiesStage 设置告警的消息信息，这些通过的告警应该已经被发送给了接收人。
type SetNotifiesStage struct {
	nflog NotificationLog
	recv  *nflogpb.Receiver
}

// NewSetNotifiesStage returns a new instance of a SetNotifiesStage.
// ------------------------------------------------------------------------------
// NewSetNotifiesStage 返回一个 SetNotifiesStage 实例。
func NewSetNotifiesStage(l NotificationLog, recv *nflogpb.Receiver) *SetNotifiesStage {
	return &SetNotifiesStage{
		nflog: l,
		recv:  recv,
	}
}

// Exec implements the Stage interface.
// ------------------------------------------------------------------------------
// Exec 实现了 Stage 接口，拿到group key，正在告警中和已解决的告警，记录到消息日志中。
func (n SetNotifiesStage) Exec(ctx context.Context, l log.Logger, alerts ...*types.Alert) (context.Context, []*types.Alert, error) {
	gkey, ok := GroupKey(ctx)
	if !ok {
		return ctx, nil, errors.New("group key missing")
	}

	firing, ok := FiringAlerts(ctx)
	if !ok {
		return ctx, nil, errors.New("firing alerts missing")
	}

	resolved, ok := ResolvedAlerts(ctx)
	if !ok {
		return ctx, nil, errors.New("resolved alerts missing")
	}

	return ctx, alerts, n.nflog.Log(n.recv, gkey, firing, resolved)
}
