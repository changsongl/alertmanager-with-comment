// Copyright 2019 Prometheus Team
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

package metrics

import "github.com/prometheus/client_golang/prometheus"

// Alerts stores metrics for alerts which are common across all API versions.
// ----------------------------------------------------------------------------
// Alerts 结构体来包含告警，解决和无效的普罗米修斯counter指标。
// Alerts.firing 和 Alerts.resolved 公用同一个指标，通过status标签来区分是firing，
// 还是resolved，并且提供相应的getter方法。
type Alerts struct {
	firing   prometheus.Counter
	resolved prometheus.Counter
	invalid  prometheus.Counter
}

// NewAlerts returns an *Alerts struct for the given API version.
// ----------------------------------------------------------------------------
// NewAlerts 通过版本号来生成 Alerts 指针，里面的所有指标，都带有这个版本作为固定的
// version 指标。
func NewAlerts(version string, r prometheus.Registerer) *Alerts {
	numReceivedAlerts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "alertmanager_alerts_received_total",
		Help:        "The total number of received alerts.",
		ConstLabels: prometheus.Labels{"version": version},
	}, []string{"status"})
	numInvalidAlerts := prometheus.NewCounter(prometheus.CounterOpts{
		Name:        "alertmanager_alerts_invalid_total",
		Help:        "The total number of received alerts that were invalid.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	if r != nil {
		r.MustRegister(numReceivedAlerts, numInvalidAlerts)
	}
	return &Alerts{
		firing:   numReceivedAlerts.WithLabelValues("firing"),
		resolved: numReceivedAlerts.WithLabelValues("resolved"),
		invalid:  numInvalidAlerts,
	}
}

// ------------------------------ Getters ------------------------------

// Firing returns a counter of firing alerts.
// ------------------------------------------------------------
// Firing 返回告警的普罗米修斯counter
func (a *Alerts) Firing() prometheus.Counter { return a.firing }

// Resolved returns a counter of resolved alerts.
// ------------------------------------------------------------
// Resolved 返回已解决的的普罗米修斯counter
func (a *Alerts) Resolved() prometheus.Counter { return a.resolved }

// Invalid returns a counter of invalid alerts.
// ------------------------------------------------------------
// Invalid 返回无效的的的普罗米修斯counter，无效指的是，告警开始时间，
// 标签，等等数据不合法，则会认为是无效告警。
func (a *Alerts) Invalid() prometheus.Counter { return a.invalid }

// ------------------------------ End Getters ------------------------------
