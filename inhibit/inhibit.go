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

package inhibit

import (
	"context"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/store"
	"github.com/prometheus/alertmanager/types"
)

// An Inhibitor determines whether a given label set is muted based on the
// currently active alerts and a set of inhibition rules. It implements the
// Muter interface.
type Inhibitor struct {
	alerts provider.Alerts
	rules  []*InhibitRule
	marker types.Marker
	logger log.Logger

	mtx    sync.RWMutex
	cancel func()
}

// NewInhibitor returns a new Inhibitor.
func NewInhibitor(ap provider.Alerts, rs []*config.InhibitRule, mk types.Marker, logger log.Logger) *Inhibitor {
	ih := &Inhibitor{
		alerts: ap,
		marker: mk,
		logger: logger,
	}
	for _, cr := range rs {
		r := NewInhibitRule(cr)
		ih.rules = append(ih.rules, r)
	}
	return ih
}

func (ih *Inhibitor) run(ctx context.Context) {
	// 开始订阅告警
	it := ih.alerts.Subscribe()
	defer it.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case a := <-it.Next():
			// 得到告警，
			if err := it.Err(); err != nil {
				level.Error(ih.logger).Log("msg", "Error iterating alerts", "err", err)
				continue
			}

			// Update the inhibition rules' cache.
			// -----------------------------------------
			// 循环每个抑制rule，假如当前告警匹配上某个source。
			// 缓存这个告警到这个抑制rule的告警map里，map的key
			// 为label的finger print，value为alert。
			for _, r := range ih.rules {
				if r.SourceMatchers.Match(a.Labels) {
					if err := r.scache.Set(a); err != nil {
						level.Error(ih.logger).Log("msg", "error on set alert", "err", err)
					}
				}
			}
		}
	}
}

// Run the Inhibitor's background processing.
// -----------------------------------------------
// alertmanager后台运行抑制器。
func (ih *Inhibitor) Run() {
	var (
		g   run.Group
		ctx context.Context
	)

	// 创建context和cancel方法
	ih.mtx.Lock()
	ctx, ih.cancel = context.WithCancel(context.Background())
	ih.mtx.Unlock()
	runCtx, runCancel := context.WithCancel(ctx)

	// 循环每个抑制规则，然后启动独立go routine去运行垃圾回收
	for _, rule := range ih.rules {
		go rule.scache.Run(runCtx, 15*time.Minute)
	}

	// 添加抑制方法到运行组，运行组并行运行。
	// 在所有的方法退出时才会退出，
	// 有错误时将返回第一个错误，一个return后，会中断其他方法。
	g.Add(func() error {
		ih.run(runCtx)
		return nil
	}, func(err error) {
		runCancel()
	})

	if err := g.Run(); err != nil {
		level.Warn(ih.logger).Log("msg", "error running inhibitor", "err", err)
	}
}

// Stop the Inhibitor's background processing.
func (ih *Inhibitor) Stop() {
	if ih == nil {
		return
	}

	ih.mtx.RLock()
	defer ih.mtx.RUnlock()
	if ih.cancel != nil {
		ih.cancel()
	}
}

// Mutes returns true iff the given label set is muted. It implements the Muter
// interface.
// -----------------------------------------------------------------------------
// 禁言方法，通过告警的标签，匹配抑制规则。如果被抑制则返回true。
func (ih *Inhibitor) Mutes(lset model.LabelSet) bool {
	fp := lset.Fingerprint()

	for _, r := range ih.rules {
		// 匹配target，如果连target都没有命中可直接去检查下个rule
		if !r.TargetMatchers.Match(lset) {
			// If target side of rule doesn't match, we don't need to look any further.
			continue
		}
		// If we are here, the target side matches. If the source side matches, too, we
		// need to exclude inhibiting alerts for which the same is true.
		// -----------------------------------------------------------------------------
		// 匹配source，假如匹配成功的
		if inhibitedByFP, eq := r.hasEqual(lset, r.SourceMatchers.Match(lset)); eq {
			// 设置当前告警已经被匹配上的source抑制
			ih.marker.SetInhibited(fp, inhibitedByFP.String())
			return true
		}
	}
	// 未匹配成功，则设置没有被告警抑制
	ih.marker.SetInhibited(fp)

	return false
}

// An InhibitRule specifies that a class of (source) alerts should inhibit
// notifications for another class of (target) alerts if all specified matching
// labels are equal between the two alerts. This may be used to inhibit alerts
// from sending notifications if their meaning is logically a subset of a
// higher-level alert.
type InhibitRule struct {
	// The set of Filters which define the group of source alerts (which inhibit
	// the target alerts).
	SourceMatchers types.Matchers
	// The set of Filters which define the group of target alerts (which are
	// inhibited by the source alerts).
	TargetMatchers types.Matchers
	// A set of label names whose label values need to be identical in source and
	// target alerts in order for the inhibition to take effect.
	Equal map[model.LabelName]struct{}

	// Cache of alerts matching source labels.
	scache *store.Alerts
}

// NewInhibitRule returns a new InhibitRule based on a configuration definition.
func NewInhibitRule(cr *config.InhibitRule) *InhibitRule {
	var (
		sourcem types.Matchers
		targetm types.Matchers
	)

	for ln, lv := range cr.SourceMatch {
		sourcem = append(sourcem, types.NewMatcher(model.LabelName(ln), lv))
	}
	for ln, lv := range cr.SourceMatchRE {
		sourcem = append(sourcem, types.NewRegexMatcher(model.LabelName(ln), lv.Regexp))
	}

	for ln, lv := range cr.TargetMatch {
		targetm = append(targetm, types.NewMatcher(model.LabelName(ln), lv))
	}
	for ln, lv := range cr.TargetMatchRE {
		targetm = append(targetm, types.NewRegexMatcher(model.LabelName(ln), lv.Regexp))
	}

	equal := map[model.LabelName]struct{}{}
	for _, ln := range cr.Equal {
		equal[ln] = struct{}{}
	}

	return &InhibitRule{
		SourceMatchers: sourcem,
		TargetMatchers: targetm,
		Equal:          equal,
		scache:         store.NewAlerts(),
	}
}

// hasEqual checks whether the source cache contains alerts matching the equal
// labels for the given label set. If so, the fingerprint of one of those alerts
// is returned. If excludeTwoSidedMatch is true, alerts that match both the
// source and the target side of the rule are disregarded.
func (r *InhibitRule) hasEqual(lset model.LabelSet, excludeTwoSidedMatch bool) (model.Fingerprint, bool) {
Outer:
	for _, a := range r.scache.List() {
		// The cache might be stale and contain resolved alerts.
		if a.Resolved() {
			continue
		}
		for n := range r.Equal {
			if a.Labels[n] != lset[n] {
				continue Outer
			}
		}
		if excludeTwoSidedMatch && r.TargetMatchers.Match(a.Labels) {
			continue Outer
		}
		return a.Fingerprint(), true
	}
	return model.Fingerprint(0), false
}
