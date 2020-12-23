// Copyright 2016 Prometheus Team
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


// 这个包为什么要叫mem，而不是叫alerts或者alert-provider之类的？这是因为
// mem包是alertmanager的内存实现告警存储的一种实现。通过实现provider接口的内存实现，
// 来完成整个告警的存储功能。假如alertmanager官方想要更改存储实现，或者说提供更多的，
// 存储实现方式，则可以通过添加新的一种实现来解决。Ex: 如redis实现，mysql实现等等。
package mem

import (
	"context"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/store"
	"github.com/prometheus/alertmanager/types"
)


// 告警channel的size值
const alertChannelLength = 200

// Alerts gives access to a set of alerts. All methods are goroutine-safe.
// --------------------------------------------------------------------------
// Alerts 是具体 provider.Alerts 的实现类。可以通过它来使用一组告警。全部的方法都是
// 协成安全的。
type Alerts struct {
	cancel context.CancelFunc		  // cancel Context的cancel方法。负责cancel Alerts.Run() 的协成。

	mtx       sync.Mutex			  // mtx 锁，确保 Alerts 的线程安全。
	alerts    *store.Alerts			  // alerts 是具体的存储告警的结构体，以内存map[fingerprint]alert来存储。
									  // 里面负责存储告警，并且负责进行已经解决的告警的垃圾回收。

	listeners map[int]listeningAlerts // listeners 管理和监听现在未解决的告警。
	next      int					  // next 配合 listeners 使用，获取下一个放到 listeners map的counter。

	logger log.Logger				  // logger 打印log呗。
}

// listeningAlerts TODO: 待更新
type listeningAlerts struct {
	alerts chan *types.Alert
	done   chan struct{}
}

// NewAlerts returns a new alert provider.
// -------------------------------------------------------------------------
// NetAlerts 返回 Alerts 指针，并且设置已经解决的告警垃圾回收方法，并且运行独立
// 协程去进行定时垃圾回收。
func NewAlerts(ctx context.Context, m types.Marker, intervalGC time.Duration, l log.Logger) (*Alerts, error) {
	ctx, cancel := context.WithCancel(ctx)
	a := &Alerts{
		alerts:    store.NewAlerts(),
		cancel:    cancel,
		listeners: map[int]listeningAlerts{},
		next:      0,
		logger:    log.With(l, "component", "provider"),
	}

	// 设置垃圾回收方法
	a.alerts.SetGCCallback(func(alerts []*types.Alert) {
		// alerts 为已经全部解决的告警，因此遍历每一个，并从map里面通过自己的指纹，删除这个告警。
		// 这个实现方式是内存的实现方式，不会进行告警的永久话，因此直接从内存删除极客。
		for _, alert := range alerts {
			// As we don't persist alerts, we no longer consider them after
			// they are resolved. Alerts waiting for resolved notifications are
			// held in memory in aggregation groups redundantly.
			m.Delete(alert.Fingerprint())
		}

		// 锁住 Alerts.listeners 遍历所有已经关闭掉的监听器，并从
		// 监听器map里面移除，并且关掉监听器里的channel。
		a.mtx.Lock()
		for i, l := range a.listeners {
			select {
			case <-l.done:
				delete(a.listeners, i)
				close(l.alerts)
			default:
				// listener is not closed yet, hence proceed.
			}
		}
		a.mtx.Unlock()
	})

	// 运行告警垃圾回收协程。
	go a.alerts.Run(ctx, intervalGC)

	return a, nil
}

// Close the alert provider.
// ----------------------------------
// Close 调用 Alerts 的 cancel 方法关闭 context。
func (a *Alerts) Close() {
	if a.cancel != nil {
		a.cancel()
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Subscribe returns an iterator over active alerts that have not been
// resolved and successfully notified about.
// They are not guaranteed to be in chronological order.
// ----------------------------------------------------------------------
// Subscribe 方法返回一个告警遍历器，里面包含全部活跃的告警（没有被解决或成功通知），
// 这个遍历器并不能保证告警是按照时间顺序排列的。
func (a *Alerts) Subscribe() provider.AlertIterator {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	var (
		done   = make(chan struct{})
		alerts = a.alerts.List()
		ch     = make(chan *types.Alert, max(len(alerts), alertChannelLength))
	)

	for _, a := range alerts {
		ch <- a
	}

	// 把新建告警channel，放入监听器存储到map里面。
	a.listeners[a.next] = listeningAlerts{alerts: ch, done: done}
	a.next++

	return provider.NewAlertIterator(ch, done, nil)
}

// GetPending returns an iterator over all the alerts that have
// pending notifications.
// ----------------------------------------------------------------------
// GetPending 返回遍历器，负责遍历全部的在等待通知告警
func (a *Alerts) GetPending() provider.AlertIterator {
	var (
		ch   = make(chan *types.Alert, alertChannelLength)
		done = make(chan struct{})
	)

	// 使用独立的goroutine去遍历，提高了效率。
	go func() {
		defer close(ch)

		for _, a := range a.alerts.List() {
			select {
			case ch <- a:
			case <-done:
				return
			}
		}
	}()

	return provider.NewAlertIterator(ch, done, nil)
}

// Get returns the alert for a given fingerprint.
// ----------------------------------------------------------------------
// Get 根据告警的指纹来获得告警对象。
func (a *Alerts) Get(fp model.Fingerprint) (*types.Alert, error) {
	return a.alerts.Get(fp)
}

// Put adds the given alert to the set.
// ------------------------------------------------------
// Put 添加一到多个告警到告警集合里。并且通知到所有监听器里。
func (a *Alerts) Put(alerts ...*types.Alert) error {

	for _, alert := range alerts {
		fp := alert.Fingerprint()

		// Check that there's an alert existing within the store before
		// trying to merge.
		// ---------------------------------------------------------------------
		// 检查是否已经有旧的告警了，如果有旧的告警，需要判断是否需要合并。
		if old, err := a.alerts.Get(fp); err == nil {

			// Merge alerts if there is an overlap in activity range.
			// -----------------------------------------------------------------
			// 合并条件：判断是否新旧告警有重叠时间，如果有的话，就进行合并告警。
			//
			// 条件1：新告警的结束时间大于旧的告警的开始时间，并且新告警结束时间在旧告警结束时间之前。
			//        新告警   |----------|
			// 		  旧告警     |-----------|       -> 时间线
			//
			// 条件2：新告警的开始时间大于旧的告警的开始时间，并且新告警开始时间在旧告警结束时间之前。
			//		  新告警        |----------|
			//		  旧告警     |-----------|       -> 时间线
			//
			if (alert.EndsAt.After(old.StartsAt) && alert.EndsAt.Before(old.EndsAt)) ||
				(alert.StartsAt.After(old.StartsAt) && alert.StartsAt.Before(old.EndsAt)) {
				alert = old.Merge(alert)
			}
		}

		// 设置告警到集合。
		if err := a.alerts.Set(alert); err != nil {
			level.Error(a.logger).Log("msg", "error on set alert", "err", err)
			continue
		}

		// 循环塞到每个监听器，去通知到每一个监听者。
		a.mtx.Lock()
		for _, l := range a.listeners {
			select {
			case l.alerts <- alert:
			case <-l.done:
			}
		}
		a.mtx.Unlock()
	}

	return nil
}
