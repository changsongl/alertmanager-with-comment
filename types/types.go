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

package types

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
)

// AlertState is used as part of AlertStatus.
// ---------------------------------------------
// AlertState 被使用作为告警的状态 AlertStatus.
type AlertState string

// Possible values for AlertState.
// ---------------------------------------------
// AlertState 的值
const (
	AlertStateUnprocessed AlertState = "unprocessed" // 未处理
	AlertStateActive      AlertState = "active"      // 已激活
	AlertStateSuppressed  AlertState = "suppressed"  // 被静音
)

// AlertStatus stores the state of an alert and, as applicable, the IDs of
// silences silencing the alert and of other alerts inhibiting the alert. Note
// that currently, SilencedBy is supposed to be the complete set of the relevant
// silences while InhibitedBy may contain only a subset of the inhibiting alerts
// – in practice exactly one ID. (This somewhat confusing semantics might change
// in the future.)
// ------------------------------------------------------------------------------------------
// AlertStatus 保存告警状态，合适的，被静默的规则ID和被抑制的告警指纹。SilencedBy是包含
// 所有相关静默规则的，然而抑制可能包含一个子集的抑制告警（一个告警指纹）。这个可能会进行
// 更改，因为现在这个语义有点模糊，令人困惑。
type AlertStatus struct {
	State       AlertState `json:"state"`       // 告警的状态
	SilencedBy  []string   `json:"silencedBy"`  // 被影响到的全部静默规则
	InhibitedBy []string   `json:"inhibitedBy"` // 被这些告警抑制，但是当前只保存一个被抑制的告警。

	silencesVersion int // 静默规则的版本
}

// Marker helps to mark alerts as silenced and/or inhibited.
// All methods are goroutine-safe.
// ---------------------------------------------------------------------
// Marker 是一个接口。方便标记一些告警被静默或者被抑制。所有的方法都是
// 协成安全的。
type Marker interface {
	// SetActive sets the provided alert to AlertStateActive and deletes all
	// SilencedBy and InhibitedBy entries.
	// ---------------------------------------------------------------------
	// SetActive 设置某一个告警的标签集的状态为告警激活状态，并且删除所有的 SilencedBy
	// 和 SilencedBy 字段。
	SetActive(alert model.Fingerprint)

	// SetSilenced replaces the previous SilencedBy by the provided IDs of
	// silences, including the version number of the silences state. The set
	// of provided IDs is supposed to represent the complete set of relevant
	// silences. If no ID is provided and InhibitedBy is already empty, this
	// call is equivalent to SetActive. Otherwise, it sets
	// AlertStateSuppressed.
	// ---------------------------------------------------------------------
	// SetSilenced 通过新的静默IDs代替以前的 AlertStatus.SilencedBy，包含静默状态
	// 的版本号。所有的静默IDs集合是全部的相关静默。如果没有提供ID，与此同时
	// AlertStatus.InhibitedBy 也已经是空的，这次调用等同于 SetActive。否则，它将
	// 设置为 AlertStateSuppressed。
	SetSilenced(alert model.Fingerprint, version int, silenceIDs ...string)

	// SetInhibited replaces the previous InhibitedBy by the provided IDs of
	// alerts. In contrast to SetSilenced, the set of provided IDs is not
	// expected to represent the complete set of inhibiting alerts. (In
	// practice, this method is only called with one or zero IDs. However,
	// this expectation might change in the future.) If no ID is provided and
	// SilencedBy is already empty, this call is equivalent to
	// SetActive. Otherwise, it sets AlertStateSuppressed.
	// -----------------------------------------------------------------------
	// SetInhibited 通过告警IDs来替换掉以前的 AlertStatus.InhibitedBy。对比 SetSilenced，
	// 提供的告警是不被代表是全部的抑制告警。换句话来说，就是这个方法只会被调用的传参为一个或者
	// 零个ID。 然而，这个可能会在将来进行个更改。如果这个ID没有提供，并且 AlertStatus.SilencedBy
	// 已经是空的，这个调用则等同于 SetActive。否则，它将被设置为 AlertStateSuppressed。
	SetInhibited(alert model.Fingerprint, alertIDs ...string)

	// Count alerts of the given state(s). With no state provided, count all
	// alerts.
	// -----------------------------------------------------------------------
	// Count 根据提供的告警状态，来数当前有多少告警处在这个状态
	Count(...AlertState) int

	// Status of the given alert.
	// -----------------------------------------------------------------------
	// Status 根据某个告警指纹，查看这个告警的当前状态
	Status(model.Fingerprint) AlertStatus

	// Delete the given alert.
	// -----------------------------------------------------------------------
	// Delete 根据指纹，删除这个告警
	Delete(model.Fingerprint)

	// Various methods to inquire if the given alert is in a certain
	// AlertState. Silenced also returns all the silencing silences, while
	// Inhibited may return only a subset of inhibiting alerts. Silenced
	// also returns the version of the silences state the result is based
	// on.
	// -----------------------------------------------------------------------
	// 根据告警的指纹，检查这个告警状态的方法。 Unprocessed 和 Active 会返回是
	// 不是出在处在这个状态。 Silenced 还会返回静默版本号，和影响到这个标签集的
	// 相关静默规则ID，而 Inhibited 会返回抑制这个标签的告警子集（但是其实永远
	// 只返回一个）
	Unprocessed(model.Fingerprint) bool
	Active(model.Fingerprint) bool
	Silenced(model.Fingerprint) ([]string, int, bool)
	Inhibited(model.Fingerprint) ([]string, bool)
	// -----------------------------------------------------------------------
}

// NewMarker returns an instance of a Marker implementation.
// -----------------------------------------------------------------------
// NewMarker 返回一个具体实现Marker接口的实例，而且还会注入普罗米修斯
// 注册器。一般都是默认注册器。全部都是依赖注入进去的，没有使用全局的
// 普罗米修斯注册器。主要还是方便管理。
func NewMarker(r prometheus.Registerer) Marker {
	m := &memMarker{
		m: map[model.Fingerprint]*AlertStatus{},
	}

	// 注册进去memMarker相关的普罗米修斯指标
	m.registerMetrics(r)

	return m
}

// memMarker 结构体，具体的Marker接口实现类。
type memMarker struct {
	m map[model.Fingerprint]*AlertStatus // 保存指纹和对应状态的Map

	mtx sync.RWMutex // 读写锁
}

// 注册memMarker相关普罗米修斯指标
func (m *memMarker) registerMetrics(r prometheus.Registerer) {
	// 展示多少告警状态的指标，统计每个状态的告警数量的方法
	newAlertMetricByState := func(st AlertState) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name:        "alertmanager_alerts",
				Help:        "How many alerts by state.",
				ConstLabels: prometheus.Labels{"state": string(st)},
			},
			func() float64 {
				return float64(m.Count(st))
			},
		)
	}

	// 用上面的方法，设置激活和静音的指标
	alertsActive := newAlertMetricByState(AlertStateActive)
	alertsSuppressed := newAlertMetricByState(AlertStateSuppressed)

	r.MustRegister(alertsActive)
	r.MustRegister(alertsSuppressed)
}

// Count implements Marker.
// ------------------------------------------------------
// Count 实现 Marker 接口，循环每个状态和告警，统计状态。
func (m *memMarker) Count(states ...AlertState) int {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	if len(states) == 0 {
		return len(m.m)
	}

	var count int
	for _, status := range m.m {
		for _, state := range states {
			if status.State == state {
				count++
			}
		}
	}
	return count
}

// SetSilenced implements Marker.
// ------------------------------------------------------
// SetSilenced 实现 Marker 接口。设置某个告警，被哪些
// 静默ID给静默掉，并更新静默版本ID。
func (m *memMarker) SetSilenced(alert model.Fingerprint, version int, ids ...string) {
	m.mtx.Lock()

	// 查找是否能找到这个告警，如果找不到则创建
	s, found := m.m[alert]
	if !found {
		s = &AlertStatus{}
		m.m[alert] = s
	}

	// 更新静默版本号
	s.silencesVersion = version

	// If there are any silence or alert IDs associated with the
	// fingerprint, it is suppressed. Otherwise, set it to
	// AlertStateUnprocessed.
	// -----------------------------------------------------------
	// 如果提供静默ID为空，代表无静默规则静默这个告警。并且无告警抑制发生。
	// 则代表这个告警的状态为激活状态。
	if len(ids) == 0 && len(s.InhibitedBy) == 0 {
		m.mtx.Unlock()
		m.SetActive(alert)
		return
	}

	// 反之，有静默或者抑制，则标志状态为静音状态。并设置id。
	s.State = AlertStateSuppressed
	s.SilencedBy = ids

	m.mtx.Unlock()
}

// SetInhibited implements Marker.
// -------------------------------------------------------------------------
// SetInhibited 实现 Marker 接口。设置抑制状态。
func (m *memMarker) SetInhibited(alert model.Fingerprint, ids ...string) {
	m.mtx.Lock()

	// 查找是否能找到这个告警，如果找不到则创建
	s, found := m.m[alert]
	if !found {
		s = &AlertStatus{}
		m.m[alert] = s
	}

	// If there are any silence or alert IDs associated with the
	// fingerprint, it is suppressed. Otherwise, set it to
	// AlertStateUnprocessed.
	// -----------------------------------------------------------
	// 如果提供静默ID为空，代表无静默规则静默这个告警。并且无告警静默发生。
	// 则代表这个告警的状态为激活状态。
	if len(ids) == 0 && len(s.SilencedBy) == 0 {
		m.mtx.Unlock()
		m.SetActive(alert)
		return
	}

	// 反之，有静默或者抑制，则标志状态为静音状态。并设置id。
	s.State = AlertStateSuppressed
	s.InhibitedBy = ids

	m.mtx.Unlock()
}

// SetActive implements Marker.
// -------------------------------------------------------------------------
// SetActive 实现 Marker 接口。设置marker里面的告警状态为激活，
// 并清空静默和抑制ID。
func (m *memMarker) SetActive(alert model.Fingerprint) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	// 查找是否能找到这个告警，如果找不到则创建
	s, found := m.m[alert]
	if !found {
		s = &AlertStatus{}
		m.m[alert] = s
	}

	// 标记激活状态，清空静默和抑制ID
	s.State = AlertStateActive
	s.SilencedBy = []string{}
	s.InhibitedBy = []string{}
}

// Status implements Marker.
// -------------------------------------------------------------------------
// Status 实现 Marker 接口。返回某个告警标签的告警状态。
func (m *memMarker) Status(alert model.Fingerprint) AlertStatus {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	// 如果未找到告警，则标识告警状态是未处理阶段。
	s, found := m.m[alert]
	if !found {
		s = &AlertStatus{
			State:       AlertStateUnprocessed,
			SilencedBy:  []string{},
			InhibitedBy: []string{},
		}
	}
	// 反之，直接返回状态。
	return *s
}

// Delete implements Marker.
// -------------------------------------------------------------------------
// Delete 实现 Marker 接口。删除告警。
func (m *memMarker) Delete(alert model.Fingerprint) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	delete(m.m, alert)
}

// Unprocessed implements Marker.
// -------------------------------------------------------------------------
// Unprocessed 实现 Marker 接口。检查这个告警是否是未处理状态。
func (m *memMarker) Unprocessed(alert model.Fingerprint) bool {
	return m.Status(alert).State == AlertStateUnprocessed
}

// Active implements Marker.
// -------------------------------------------------------------------------
// Active 实现 Marker 接口。检查这个告警是否是激活状态。
func (m *memMarker) Active(alert model.Fingerprint) bool {
	return m.Status(alert).State == AlertStateActive
}

// Inhibited implements Marker.
// -------------------------------------------------------------------------
// Inhibited 实现 Marker 接口。检查这个告警是否是抑制状态，返回抑制的
// 告警标签和是否被抑制。
func (m *memMarker) Inhibited(alert model.Fingerprint) ([]string, bool) {
	s := m.Status(alert)
	return s.InhibitedBy,
		s.State == AlertStateSuppressed && len(s.InhibitedBy) > 0
}

// Silenced returns whether the alert for the given Fingerprint is in the
// Silenced state, any associated silence IDs, and the silences state version
// the result is based on.
// -------------------------------------------------------------------------
// Silenced 实现 Marker 接口。返回是否这个指纹的告警是处在静默状态中。并且如果是的话，
// 会返回版本号和静默ID的相关一些信息。
func (m *memMarker) Silenced(alert model.Fingerprint) ([]string, int, bool) {
	s := m.Status(alert)
	return s.SilencedBy, s.silencesVersion,
		s.State == AlertStateSuppressed && len(s.SilencedBy) > 0
}

// MultiError contains multiple errors and implements the error interface. Its
// zero value is ready to use. All its methods are goroutine safe.
// -------------------------------------------------------------------------
// MultiError 包含多个错误，并且实现了 error 接口。他的空值也可被使用，并且所有
// 方法都是协成安全的。
type MultiError struct {
	mtx    sync.Mutex // 锁
	errors []error    // 错误切片
}

// Add adds an error to the MultiError.
// -------------------------------------------------------------------------
// Add 添加错误到 MultiError 里面。
func (e *MultiError) Add(err error) {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	e.errors = append(e.errors, err)
}

// Len returns the number of errors added to the MultiError.
// -------------------------------------------------------------------------
// Len 返回 MultiError 里面有多少错误。
func (e *MultiError) Len() int {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	return len(e.errors)
}

// Errors returns the errors added to the MuliError. The returned slice is a
// copy of the internal slice of errors.
// -------------------------------------------------------------------------
// Errors 返回所有假如到 MultiError 里的错误。返回的切片是里面所有错误的副本。
func (e *MultiError) Errors() []error {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	return append(make([]error, 0, len(e.errors)), e.errors...)
}

// Error 接口实现 error 接口。方便展示错误。
func (e *MultiError) Error() string {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	es := make([]string, 0, len(e.errors))
	for _, err := range e.errors {
		es = append(es, err.Error())
	}
	return strings.Join(es, "; ")
}

// Alert wraps a model.Alert with additional information relevant
// to internal of the Alertmanager.
// The type is never exposed to external communication and the
// embedded alert has to be sanitized beforehand.
// -------------------------------------------------------------------------
// Alert 包含一个通用的告警结构体，并且有一些AM特定的信息。这个类型绝不
// 在外部调用中暴露出去，并且内嵌的告警，需要预先处理干净。
type Alert struct {
	model.Alert

	// The authoritative timestamp.
	UpdatedAt time.Time // 告警更新时间
	Timeout   bool      // 告警是否已经过期
}

// AlertSlice is a sortable slice of Alerts.
// -------------------------------------------------------------------------
// AlertSlice 是一组告警的切片封装类，也是可以排序切片里面的告警。实现 sort.Sort
// 的接口根据标签里面的job和instance字段进行排序。
type AlertSlice []*Alert

func (as AlertSlice) Less(i, j int) bool {
	// Look at labels.job, then labels.instance.
	for _, overrideKey := range [...]model.LabelName{"job", "instance"} {
		iVal, iOk := as[i].Labels[overrideKey]
		jVal, jOk := as[j].Labels[overrideKey]
		if !iOk && !jOk {
			continue
		}
		if !iOk {
			return false
		}
		if !jOk {
			return true
		}
		if iVal != jVal {
			return iVal < jVal
		}
	}
	return as[i].Labels.Before(as[j].Labels)
}
func (as AlertSlice) Swap(i, j int) { as[i], as[j] = as[j], as[i] }
func (as AlertSlice) Len() int      { return len(as) }

// -------------------------------------------------------------------------

// Alerts turns a sequence of internal alerts into a list of
// exposable model.Alert structures.
// -------------------------------------------------------------------------
// Alerts 转换一系列的Alert内置对象，转换成可暴露的 model.Alert 结构。
func Alerts(alerts ...*Alert) model.Alerts {
	res := make(model.Alerts, 0, len(alerts))
	for _, a := range alerts {
		v := a.Alert
		// If the end timestamp is not reached yet, do not expose it.
		// -----------------------------------------------------------
		// 如果还未结束时间还没到的话，先不暴露这个时间。
		if !a.Resolved() {
			v.EndsAt = time.Time{}
		}
		res = append(res, &v)
	}
	return res
}

// Merge merges the timespan of two alerts based and overwrites annotations
// based on the authoritative timestamp.  A new alert is returned, the labels
// are assumed to be equal.
// ---------------------------------------------------------------------------
// Merge 合并在一段时间内的同一个告警的两个告警实例。一个新的告警对象会被返回，合并的的告警，默认其
// 标签是相等的。
func (a *Alert) Merge(o *Alert) *Alert {
	// Let o always be the younger alert.
	// -------------------------------------
	// 如果o的更新时间比a早。o 来合并 a 这里会
	// 递归一次，确保o比a早。
	if o.UpdatedAt.Before(a.UpdatedAt) {
		return o.Merge(a)
	}

	res := *o

	// Always pick the earliest starting time.
	// -----------------------------------------
	// 选择两个告警中的最早的开始时间
	if a.StartsAt.Before(o.StartsAt) {
		res.StartsAt = a.StartsAt
	}

	// 如果o已经告警已经解决
	if o.Resolved() {
		// The latest explicit resolved timestamp wins if both alerts are effectively resolved.
		// ------------------------------------------------------------------------------------
		// 使用更晚的结束时间，当两个o和a都是已经解决的状态。
		if a.Resolved() && a.EndsAt.After(o.EndsAt) {
			res.EndsAt = a.EndsAt
		}
	} else { // o告警实例是还未解决
		// A non-timeout timestamp always rules if it is the latest.
		// ------------------------------------------------------------
		//如果a的结束时间比o晚，并且a还未超时，则设置返回的告警为a的结束时间
		if a.EndsAt.After(o.EndsAt) && !a.Timeout {
			res.EndsAt = a.EndsAt
		}
	}

	return &res
}

// A Muter determines whether a given label set is muted. Implementers that
// maintain an underlying Marker are expected to update it during a call of
// Mutes.
// ------------------------------------------------------------------------------------
// Muter 决定是否一组标签结合会被静音掉。实现这个接口的那个实例维护一个 Marker ，并更
// 新它当调用 Mutes 方法时。
type Muter interface {
	Mutes(model.LabelSet) bool
}

// A MuteFunc is a function that implements the Muter interface.
// ------------------------------------------------------------------------------------
// MuteFunc 是一个函数的封装，并且实现了 Muter 接口。 MuteFunc 实例调用 Mutes
// 方法的时候，其实会运行自己这个方法，来返回 Mutes 方法的结果。呵呵，有点绕，
// 实现很有意思。
type MuteFunc func(model.LabelSet) bool

// Mutes implements the Muter interface.
// ------------------------------------------------------------------------------------
// MuteFunc 实现了 Muter 接口
func (f MuteFunc) Mutes(lset model.LabelSet) bool { return f(lset) }

// A Silence determines whether a given label set is muted.
// ------------------------------------------------------------------------------------
// Silence 结构体，来决定是否一组标签集是被静音的。
type Silence struct {
	// A unique identifier across all connected instances.
	// ---------------------------------------------------------
	// 一个唯一的标识ID，在所有静默实例中都是具有唯一性。
	ID string `json:"id"`

	// A set of matchers determining if a label set is affect
	// by the silence.
	// ---------------------------------------------------------
	// 一组匹配器，来决定是否标签集被这个静默影响到。
	Matchers Matchers `json:"matchers"`

	// Time range of the silence.
	//
	// * StartsAt must not be before creation time
	// * EndsAt must be after StartsAt
	// * Deleting a silence means to set EndsAt to now
	// * Time range must not be modified in different ways
	//
	// TODO(fabxc): this may potentially be extended by
	// creation and update timestamps.
	// ---------------------------------------------------------
	// 静默的影响时间范围
	//
	// * StartsAt 必须在创建静默规则的时间之后
	// * EndsAt 必须在 StartsAt 时间之后。
	// * 删除静默的时候其实就是设置结束时间 EndsAt 为现在。
	// * 时间范围必须满足以上要求。
	//
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`

	// The last time the silence was updated.
	// ---------------------------------------------------------
	// 静默规则上次的更新时间。
	UpdatedAt time.Time `json:"updatedAt"`

	// Information about who created the silence for which reason.
	// ---------------------------------------------------------
	// 静默创建人和描述为什么创建这个静默。
	CreatedBy string `json:"createdBy"`
	Comment   string `json:"comment,omitempty"`

	// 静默状态，如过期，激活中，和即将放生。
	Status SilenceStatus `json:"status"`
}

// Expired return if the silence is expired
// meaning that both StartsAt and EndsAt are equal
// ---------------------------------------------------------
// Expired 返回如果开始时间等于结束时间，则代表静默
// 已经过期了。
func (s *Silence) Expired() bool {
	return s.StartsAt.Equal(s.EndsAt)
}

// SilenceStatus stores the state of a silence.
// ---------------------------------------------------------
// SilenceStatus 存储静默的状态。
type SilenceStatus struct {
	State SilenceState `json:"state"`
}

// SilenceState is used as part of SilenceStatus.
// ---------------------------------------------------------
// SilenceState 被使用在 SilenceStatus 中。
type SilenceState string

// Possible values for SilenceState.
// ---------------------------------------------------------
// 静默状态的值
const (
	SilenceStateExpired SilenceState = "expired" // 过期，已过 EndsAt
	SilenceStateActive  SilenceState = "active"  // 已到 StartsAt 时间，未到 EndsAt
	SilenceStatePending SilenceState = "pending" // 未到 StartsAt 时间
)

// CalcSilenceState returns the SilenceState that a silence with the given start
// and end time would have right now.
// ------------------------------------------------------------------------------------
// CalcSilenceState 会根据提供的开始时间和结束返回 SilenceState
func CalcSilenceState(start, end time.Time) SilenceState {
	current := time.Now()
	if current.Before(start) { // 还未开始，pending 状态
		return SilenceStatePending
	}
	if current.Before(end) { // 还未结束，active 状态
		return SilenceStateActive
	}
	return SilenceStateExpired // 已过结束时间，过期状态
}
