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
// ---------------------------------------------------------------------------
// Inhibitor 是根据配置的抑制规则，来检查告警的标签是否被现在激活的告警被静音掉。它已经
// 实现了Muter接口。
type Inhibitor struct {
	alerts provider.Alerts // 告警对象，用来获得告警和遍历器
	rules  []*InhibitRule  // 抑制规则切片，里面包含抑制规则的匹配器
	marker types.Marker    // 告警的标记器，负责标记告警状态，如告警，静默，抑制等等
	logger log.Logger      // log对象

	mtx    sync.RWMutex // 锁
	cancel func()       // 取消抑制规则的方法，当其他流程出问题的时候，会调用此方法通知抑制器
}

// NewInhibitor returns a new Inhibitor.
// ---------------------------------------------------------------------------
// NewInhibitor 返回一个新的 Inhibitor。 其会循环解析配置文件中的抑制规则。
// 为每个规则生成 InhibitRule 结构体。每个 InhibitRule 结构体里包含source，
// target，equal等匹配抑制的信息。
func NewInhibitor(ap provider.Alerts, rs []*config.InhibitRule, mk types.Marker, logger log.Logger) *Inhibitor {
	ih := &Inhibitor{
		alerts: ap,
		marker: mk,
		logger: logger,
	}

	// 循环每个抑制规则，创建InhibitRule
	for _, cr := range rs {
		r := NewInhibitRule(cr)
		ih.rules = append(ih.rules, r)
	}
	return ih
}

// 运行抑制器，会开始订阅告警。然后开始循环，如果订阅的告警遍历器里面有告警，
// 则获得告警，并循环每个抑制规则，如果匹配到
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
// ---------------------------------------------
// Stop 停止 Inhibitor 的后台处理
func (ih *Inhibitor) Stop() {
	if ih == nil {
		return
	}

	// 锁住，并调用cancel方法
	ih.mtx.RLock()
	defer ih.mtx.RUnlock()
	if ih.cancel != nil {
		ih.cancel()
	}
}

// Mutes returns true iff the given label set is muted. It implements the Muter
// interface.
// -----------------------------------------------------------------------------
// Mutes 禁言方法，通过告警的标签，匹配抑制规则。如果被抑制则返回true。
func (ih *Inhibitor) Mutes(lset model.LabelSet) bool {
	fp := lset.Fingerprint()

	// 检查每个告警规则。
	for _, r := range ih.rules {
		// 匹配target，检查是否有匹配到规则。如果匹配到，则代表自己有可能
		// 被某个告警规则抑制，如果没有命中可直接去检查下个rule。
		if !r.TargetMatchers.Match(lset) {
			// If target side of rule doesn't match, we don't need to look any further.
			continue
		}
		// If we are here, the target side matches. If the source side matches, too, we
		// need to exclude inhibiting alerts for which the same is true.
		// -----------------------------------------------------------------------------
		// 检查有可能被抑制的这个规则。如果找到这个规则的source的告警，则代表匹配成功。
		// 这个当前告警应该被抑制掉。
		if inhibitedByFP, eq := r.hasEqual(lset, r.SourceMatchers.Match(lset)); eq {
			// 设置当前告警被抑制
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
// ---------------------------------------------------------------------------------------
// InhibitRule 描述一类源source告警应该去抑制掉目标target告警消息。如果根据这个抑制规则，
// 检查到某两个标签的告警都是相等的，那就会抑制掉target的那个告警。如果两个告警逻辑上是某个
// 告警是一个高级的子集。那么这个子集告警的通知应该被抑制掉。
//
// Ex: 机器DOWN了，会抑制掉机器不可访问的告警。
//
type InhibitRule struct {
	// The set of Filters which define the group of source alerts (which inhibit
	// the target alerts).
	// --------------------------------------------------------------------------
	// 一组过滤器，定义了一组源source告警。可以抑制目标告警。
	SourceMatchers types.Matchers

	// The set of Filters which define the group of target alerts (which are
	// inhibited by the source alerts).
	// --------------------------------------------------------------------------
	// 一组过滤器，定义了一组目标告警。可以被源告警给抑制掉。
	TargetMatchers types.Matchers

	// A set of label names whose label values need to be identical in source and
	// target alerts in order for the inhibition to take effect.
	// --------------------------------------------------------------------------
	// 一组标签，代表只有在源告警和目标告警之间，他们的这一组标签值都相等的时候，抑制规则才
	// 能起到效果。
	Equal map[model.LabelName]struct{}

	// Cache of alerts matching source labels.
	// --------------------------------------------------------------------------
	// 匹配到源告警规则标签的告警缓存
	scache *store.Alerts
}

// NewInhibitRule returns a new InhibitRule based on a configuration definition.
// ---------------------------------------------------------------------------------------
// NewInhibitRule 基于配置定义来返回一个新的 InhibitRule。
func NewInhibitRule(cr *config.InhibitRule) *InhibitRule {
	var (
		sourcem types.Matchers
		targetm types.Matchers
	)

	// 循环每一个源告警匹配和目标告警匹配的匹配器和正则匹配器，进行统计
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

	// 循环每个需要相等的，抑制规则告警标签
	equal := map[model.LabelName]struct{}{}
	for _, ln := range cr.Equal {
		equal[ln] = struct{}{}
	}

	// 生成抑制对象
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
// ---------------------------------------------------------------------------------------
// hasEqual 检查是否源告警的缓存里，有匹配目标告警标签的告警。如果有的话，符合条件的其中一个告警
// 会被返回。如果 excludeTwoSidedMatch 是true，同时匹配到源和目标规则的告警将被忽略掉。
//
// 这里使用的continue Outer感觉很有争议，和continue起到同样的效果。但是比较难理解。
func (r *InhibitRule) hasEqual(lset model.LabelSet, excludeTwoSidedMatch bool) (model.Fingerprint, bool) {
Outer:
	for _, a := range r.scache.List() {
		// The cache might be stale and contain resolved alerts.
		// ------------------------------------------------------
		// 这个缓存可能有脏数据，包含已经解决的告警，因此判断已经解决，则
		// 忽略掉。
		if a.Resolved() {
			continue
		}
		// 判断满足这个源规则的告警，是否和检查的告警，equal的标签都是相等的。
		// 如果不相等，则继续下个循环。
		for n := range r.Equal {
			if a.Labels[n] != lset[n] {
				continue Outer
			}
		}

		// 此时应该已经代表匹配到了抑制规则了，当前正在检查的告警，应该被抑制掉。
		// 但是这里又多做了一部检查。
		// 如果当前循环到的告警既满足源规则，也满足目标规则。并且检查的告警也是一样。
		// 则不会标识被抑制，而是被忽略掉。
		if excludeTwoSidedMatch && r.TargetMatchers.Match(a.Labels) {
			continue Outer
		}

		// 成功被抑制，返回源抑制告警的指纹
		return a.Fingerprint(), true
	}
	// 没有被抑制，返回空指纹
	return model.Fingerprint(0), false
}
