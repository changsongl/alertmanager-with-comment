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

package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"
)

var (
	// ErrNotFound is returned if a Store cannot find the Alert.
	// -----------------------------------------------------------
	// ErrNotFound 是当一个存储告警的对象找不到某个告警时，返回的错误。
	ErrNotFound = errors.New("alert not found")
)

// Alerts provides lock-coordinated to an in-memory map of alerts, keyed by
// their fingerprint. Resolved alerts are removed from the map based on
// gcInterval. An optional callback can be set which receives a slice of all
// resolved alerts that have been removed.
// -----------------------------------------------------------------------------
// Alerts 是一个存储告警的map，提供锁来进行同步和协调。map的key为告警的职位。已经被解决
// 的告警将被从map里面移除，移除的频率取决于gcInterval配置。其可以设置一个可选的回调函数，
// 方法签名为func([]*types.Alert)。当gc发生时，会移除已经解决的告警，并把这些告警作为参数
// 传给回调函数，来继续拓展行为。
type Alerts struct {
	sync.Mutex // 结构体锁
	c  map[model.Fingerprint]*types.Alert // 存储告警的map
	cb func([]*types.Alert) // gc掉已解决的告警后的回调函数
}

// NewAlerts returns a new Alerts struct.
// -------------------------------------------------------------
// NewAlerts 返回一个 Alerts 结构体，初始化告警map和一个空的
// 回调函数。
func NewAlerts() *Alerts {
	a := &Alerts{
		c:  make(map[model.Fingerprint]*types.Alert),
		cb: func(_ []*types.Alert) {},
	}

	return a
}

// SetGCCallback sets a GC callback to be executed after each GC.
// -------------------------------------------------------------
// SetGCCallback 设置一个GC的回调函数，会在GC回收已经解决的告警之后，
// 被调用。
func (a *Alerts) SetGCCallback(cb func([]*types.Alert)) {
	a.Lock()
	defer a.Unlock()

	a.cb = cb
}

// Run starts the GC loop. The interval must be greater than zero; if not, the function will panic.
// -----------------------------------------------------------------------------------------
// 设置定时器，每次触发，对已解决告警进行垃圾回收和GC回调。
func (a *Alerts) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.gc()
		}
	}
}

// 告警垃圾回收方法，进行告警上锁，删除已解决的告警，并对已解决的告警调用callback方法。
func (a *Alerts) gc() {
	a.Lock()
	defer a.Unlock()

	var resolved []*types.Alert
	for fp, alert := range a.c {
		if alert.Resolved() {
			delete(a.c, fp)
			resolved = append(resolved, alert)
		}
	}
	a.cb(resolved)
}

// Get returns the Alert with the matching fingerprint, or an error if it is
// not found.
// ---------------------------------------------------------------------------
// Get 返回匹配到指纹的告警，或者返回一个未找到的错误 ErrNotFound 。
func (a *Alerts) Get(fp model.Fingerprint) (*types.Alert, error) {
	a.Lock()
	defer a.Unlock()

	alert, prs := a.c[fp]
	if !prs {
		return nil, ErrNotFound
	}
	return alert, nil
}

// Set unconditionally sets the alert in memory.
// ---------------------------------------------------------------------------
// Set 没有任何条件的设置告警到内存当中。
func (a *Alerts) Set(alert *types.Alert) error {
	a.Lock()
	defer a.Unlock()

	a.c[alert.Fingerprint()] = alert
	return nil
}

// Delete removes the Alert with the matching fingerprint from the store.
// ---------------------------------------------------------------------------
// Delete 根据匹配到的指纹，删除告警
func (a *Alerts) Delete(fp model.Fingerprint) error {
	a.Lock()
	defer a.Unlock()

	delete(a.c, fp)
	return nil
}

// List returns a slice of Alerts currently held in memory.
// ---------------------------------------------------------------------------
// List 返回现在全部在内存中的告警
func (a *Alerts) List() []*types.Alert {
	a.Lock()
	defer a.Unlock()

	alerts := make([]*types.Alert, 0, len(a.c))
	for _, alert := range a.c {
		alerts = append(alerts, alert)
	}

	return alerts
}

// Empty returns true if the store is empty.
// ---------------------------------------------------------------------------
// Empty 当当前存储中没有告警则返回true
func (a *Alerts) Empty() bool {
	a.Lock()
	defer a.Unlock()

	return len(a.c) == 0
}
