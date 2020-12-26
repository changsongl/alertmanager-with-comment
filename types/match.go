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
	"bytes"
	"fmt"
	"regexp"
	"sort"

	"github.com/prometheus/common/model"
)

// Matcher defines a matching rule for the value of a given label.
// ----------------------------------------------------------------------
// Matcher 匹配器包含标签名和标签的值，假如是值是一个正则表达式的话，
// 会生成一个正则匹配器来进行匹配。
type Matcher struct {
	Name    string `json:"name"` // 标签名
	Value   string `json:"value"` // 标签值
	IsRegex bool   `json:"isRegex"` // 是否为正则

	regex *regexp.Regexp // 正则匹配器
}

// Init internals of the Matcher. Must be called before using Match.
// ----------------------------------------------------------------------
// Init 匹配器 Matcher 的核心。必须在使用 Match 方法前调用此方法。会检查
// 是否为正则类型的匹配。如果是的话，通过正则文字，生成正则匹配对象。
func (m *Matcher) Init() error {
	if !m.IsRegex {
		return nil
	}
	re, err := regexp.Compile("^(?:" + m.Value + ")$")
	if err != nil {
		return err
	}
	m.regex = re
	return nil
}

func (m *Matcher) String() string {
	if m.IsRegex {
		return fmt.Sprintf("%s=~%q", m.Name, m.Value)
	}
	return fmt.Sprintf("%s=%q", m.Name, m.Value)
}

// Validate returns true iff all fields of the matcher have valid values.
func (m *Matcher) Validate() error {
	if !model.LabelName(m.Name).IsValid() {
		return fmt.Errorf("invalid name %q", m.Name)
	}
	if m.IsRegex {
		if _, err := regexp.Compile(m.Value); err != nil {
			return fmt.Errorf("invalid regular expression %q", m.Value)
		}
	} else if !model.LabelValue(m.Value).IsValid() || len(m.Value) == 0 {
		return fmt.Errorf("invalid value %q", m.Value)
	}
	return nil
}

// Match checks whether the label of the matcher has the specified
// matching value.
func (m *Matcher) Match(lset model.LabelSet) bool {
	// Unset labels are treated as unset labels globally. Thus, if a
	// label is not set we retrieve the empty label which is correct
	// for the comparison below.
	v := lset[model.LabelName(m.Name)]

	if m.IsRegex {
		return m.regex.MatchString(string(v))
	}
	return string(v) == m.Value
}

// NewMatcher returns a new matcher that compares against equality of
// the given value.
func NewMatcher(name model.LabelName, value string) *Matcher {
	return &Matcher{
		Name:    string(name),
		Value:   value,
		IsRegex: false,
	}
}

// NewRegexMatcher returns a new matcher that compares values against
// a regular expression. The matcher is already initialized.
//
// TODO(fabxc): refactor usage.
func NewRegexMatcher(name model.LabelName, re *regexp.Regexp) *Matcher {
	return &Matcher{
		Name:    string(name),
		Value:   re.String(),
		IsRegex: true,
		regex:   re,
	}
}

// Matchers provides the Match and Fingerprint methods for a slice of Matchers.
// Matchers must always be sorted.
type Matchers []*Matcher

// NewMatchers returns the given Matchers sorted.
func NewMatchers(ms ...*Matcher) Matchers {
	m := Matchers(ms)
	sort.Sort(m)
	return m
}

func (ms Matchers) Len() int      { return len(ms) }
func (ms Matchers) Swap(i, j int) { ms[i], ms[j] = ms[j], ms[i] }

func (ms Matchers) Less(i, j int) bool {
	if ms[i].Name > ms[j].Name {
		return false
	}
	if ms[i].Name < ms[j].Name {
		return true
	}
	if ms[i].Value > ms[j].Value {
		return false
	}
	if ms[i].Value < ms[j].Value {
		return true
	}
	return !ms[i].IsRegex && ms[j].IsRegex
}

// Match checks whether all matchers are fulfilled against the given label set.
func (ms Matchers) Match(lset model.LabelSet) bool {
	for _, m := range ms {
		if !m.Match(lset) {
			return false
		}
	}
	return true
}

func (ms Matchers) String() string {
	var buf bytes.Buffer

	buf.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(m.String())
	}
	buf.WriteByte('}')

	return buf.String()
}
