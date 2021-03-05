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

package template

import (
	"bytes"
	tmplhtml "html/template"
	"io/ioutil"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	tmpltext "text/template"
	"time"

	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/asset"
	"github.com/prometheus/alertmanager/types"
)

// Template bundles a text and a html template instance.
// -------------------------------------------------------
// Template 整合文本和html模板
type Template struct {
	text *tmpltext.Template
	html *tmplhtml.Template

	ExternalURL *url.URL
}

// FromGlobs calls ParseGlob on all path globs provided and returns the
// resulting Template.
// -----------------------------------------------------------------------
// FromGlobs 调用ParseGlob来获得全部的模板，加载所有相关模板
func FromGlobs(paths ...string) (*Template, error) {
	t := &Template{
		text: tmpltext.New("").Option("missingkey=zero"),
		html: tmplhtml.New("").Option("missingkey=zero"),
	}
	var err error

	t.text = t.text.Funcs(tmpltext.FuncMap(DefaultFuncs))
	t.html = t.html.Funcs(tmplhtml.FuncMap(DefaultFuncs))

	f, err := asset.Assets.Open("/templates/default.tmpl")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if t.text, err = t.text.Parse(string(b)); err != nil {
		return nil, err
	}
	if t.html, err = t.html.Parse(string(b)); err != nil {
		return nil, err
	}

	for _, tp := range paths {
		// ParseGlob in the template packages errors if not at least one file is
		// matched. We want to allow empty matches that may be populated later on.
		p, err := filepath.Glob(tp)
		if err != nil {
			return nil, err
		}
		if len(p) > 0 {
			if t.text, err = t.text.ParseGlob(tp); err != nil {
				return nil, err
			}
			if t.html, err = t.html.ParseGlob(tp); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// ExecuteTextString needs a meaningful doc comment (TODO(fabxc)).
// -----------------------------------------------------------------------
// ExecuteTextString 把TEXT模板里的占位符全部替换成data，并把替换后的文本进行返回。
func (t *Template) ExecuteTextString(text string, data interface{}) (string, error) {
	if text == "" {
		return "", nil
	}
	tmpl, err := t.text.Clone()
	if err != nil {
		return "", err
	}
	tmpl, err = tmpl.New("").Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	return buf.String(), err
}

// ExecuteHTMLString needs a meaningful doc comment (TODO(fabxc)).
// -----------------------------------------------------------------------
// ExecuteHTMLString 把HTML模板里的占位符全部替换成data，并把替换后的文本进行返回。
func (t *Template) ExecuteHTMLString(html string, data interface{}) (string, error) {
	if html == "" {
		return "", nil
	}
	tmpl, err := t.html.Clone()
	if err != nil {
		return "", err
	}
	tmpl, err = tmpl.New("").Option("missingkey=zero").Parse(html)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	return buf.String(), err
}

// FuncMap 模板的方法Map
type FuncMap map[string]interface{}

var DefaultFuncs = FuncMap{
	"toUpper": strings.ToUpper,
	"toLower": strings.ToLower,
	"title":   strings.Title,
	// join is equal to strings.Join but inverts the argument order
	// for easier pipelining in templates.
	"join": func(sep string, s []string) string {
		return strings.Join(s, sep)
	},
	"match": regexp.MatchString,
	"safeHtml": func(text string) tmplhtml.HTML {
		return tmplhtml.HTML(text)
	},
	"reReplaceAll": func(pattern, repl, text string) string {
		re := regexp.MustCompile(pattern)
		return re.ReplaceAllString(text, repl)
	},
	"stringSlice": func(s ...string) []string {
		return s
	},
}

// Pair is a key/value string pair.
// -----------------------------------------------------------------------
// Pair 是一个key/value对象，应用于label。方便用键进行排序。
type Pair struct {
	Name, Value string
}

// Pairs is a list of key/value string pairs.
// -----------------------------------------------------------------------
// Pairs 是一组kv，labels。
type Pairs []Pair

// Names returns a list of names of the pairs.
// -----------------------------------------------------------------------
// Names 返回所有的键
func (ps Pairs) Names() []string {
	ns := make([]string, 0, len(ps))
	for _, p := range ps {
		ns = append(ns, p.Name)
	}
	return ns
}

// Values returns a list of values of the pairs.
// -----------------------------------------------------------------------
// Values 返回所有的值
func (ps Pairs) Values() []string {
	vs := make([]string, 0, len(ps))
	for _, p := range ps {
		vs = append(vs, p.Value)
	}
	return vs
}

// KV is a set of key/value string pairs.
// -----------------------------------------------------------------------
// KV 是一组key/value字符串对
type KV map[string]string

// SortedPairs returns a sorted list of key/value pairs.
// -----------------------------------------------------------------------
// SortedPairs 返回一组根据键排序好的键值对
func (kv KV) SortedPairs() Pairs {
	var (
		pairs     = make([]Pair, 0, len(kv))
		keys      = make([]string, 0, len(kv))
		sortStart = 0
	)
	for k := range kv {
		if k == string(model.AlertNameLabel) {
			keys = append([]string{k}, keys...)
			sortStart = 1
		} else {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys[sortStart:])

	for _, k := range keys {
		pairs = append(pairs, Pair{k, kv[k]})
	}
	return pairs
}

// Remove returns a copy of the key/value set without the given keys.
// -----------------------------------------------------------------------
// Remove 返回一个KV副本，并且删除相应的keys。
func (kv KV) Remove(keys []string) KV {
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}

	res := KV{}
	for k, v := range kv {
		if _, ok := keySet[k]; !ok {
			res[k] = v
		}
	}
	return res
}

// Names returns the names of the label names in the LabelSet.
// -----------------------------------------------------------------------
// Names 返回在一组标签集合里的所有标签名
func (kv KV) Names() []string {
	return kv.SortedPairs().Names()
}

// Values returns a list of the values in the LabelSet.
// -----------------------------------------------------------------------
// Names 返回在一组标签集合里的所有标签的值
func (kv KV) Values() []string {
	return kv.SortedPairs().Values()
}

// Data is the data passed to notification templates and webhook pushes.
//
// End-users should not be exposed to Go's type system, as this will confuse them and prevent
// simple things like simple equality checks to fail. Map everything to float64/string.
// -----------------------------------------------------------------------
// Data 是被传参传入到通知告警模板和webhook推送里的数据。
type Data struct {
	Receiver string `json:"receiver"` // 接收人
	Status   string `json:"status"`   // 状态
	Alerts   Alerts `json:"alerts"`   // 告警对象

	GroupLabels       KV `json:"groupLabels"`       // 标签
	CommonLabels      KV `json:"commonLabels"`      // 通用标签
	CommonAnnotations KV `json:"commonAnnotations"` // 通用注解

	ExternalURL string `json:"externalURL"` // 外部URL
}

// Alert holds one alert for notification templates.
// -----------------------------------------------------------------------
// Alert 保存告警模板的一个告警信息
type Alert struct {
	Status       string    `json:"status"`       // 告警状态
	Labels       KV        `json:"labels"`       // 告警标签
	Annotations  KV        `json:"annotations"`  // 注解
	StartsAt     time.Time `json:"startsAt"`     // 开始时间
	EndsAt       time.Time `json:"endsAt"`       // 结束时间
	GeneratorURL string    `json:"generatorURL"` // URL
	Fingerprint  string    `json:"fingerprint"`  // 告警指纹
}

// Alerts is a list of Alert objects.
// -----------------------------------------------------------------------
// Alerts 是一组 Alert 对象
type Alerts []Alert

// Firing returns the subset of alerts that are firing.
// -----------------------------------------------------------------------
// Firing 返回这个告警组里的正在告警子集
func (as Alerts) Firing() []Alert {
	res := []Alert{}
	for _, a := range as {
		if a.Status == string(model.AlertFiring) {
			res = append(res, a)
		}
	}
	return res
}

// Resolved returns the subset of alerts that are resolved.
// -----------------------------------------------------------------------
// Firing 返回这个告警组里的已经解决的告警子集
func (as Alerts) Resolved() []Alert {
	res := []Alert{}
	for _, a := range as {
		if a.Status == string(model.AlertResolved) {
			res = append(res, a)
		}
	}
	return res
}

// Data assembles data for template expansion.
// -----------------------------------------------------------------------
// Data 组装好 data 对象，为生成模板而是用。
func (t *Template) Data(recv string, groupLabels model.LabelSet, alerts ...*types.Alert) *Data {
	data := &Data{
		Receiver:          regexp.QuoteMeta(recv),
		Status:            string(types.Alerts(alerts...).Status()),
		Alerts:            make(Alerts, 0, len(alerts)),
		GroupLabels:       KV{},
		CommonLabels:      KV{},
		CommonAnnotations: KV{},
		ExternalURL:       t.ExternalURL.String(),
	}

	// The call to types.Alert is necessary to correctly resolve the internal
	// representation to the user representation.
	for _, a := range types.Alerts(alerts...) {
		alert := Alert{
			Status:       string(a.Status()),
			Labels:       make(KV, len(a.Labels)),
			Annotations:  make(KV, len(a.Annotations)),
			StartsAt:     a.StartsAt,
			EndsAt:       a.EndsAt,
			GeneratorURL: a.GeneratorURL,
			Fingerprint:  a.Fingerprint().String(),
		}
		for k, v := range a.Labels {
			alert.Labels[string(k)] = string(v)
		}
		for k, v := range a.Annotations {
			alert.Annotations[string(k)] = string(v)
		}
		data.Alerts = append(data.Alerts, alert)
	}

	for k, v := range groupLabels {
		data.GroupLabels[string(k)] = string(v)
	}

	if len(alerts) >= 1 {
		var (
			commonLabels      = alerts[0].Labels.Clone()
			commonAnnotations = alerts[0].Annotations.Clone()
		)
		for _, a := range alerts[1:] {
			if len(commonLabels) == 0 && len(commonAnnotations) == 0 {
				break
			}
			for ln, lv := range commonLabels {
				if a.Labels[ln] != lv {
					delete(commonLabels, ln)
				}
			}
			for an, av := range commonAnnotations {
				if a.Annotations[an] != av {
					delete(commonAnnotations, an)
				}
			}
		}
		for k, v := range commonLabels {
			data.CommonLabels[string(k)] = string(v)
		}
		for k, v := range commonAnnotations {
			data.CommonAnnotations[string(k)] = string(v)
		}
	}

	return data
}
