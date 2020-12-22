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

package provider

import (
	"fmt"

	"github.com/prometheus/common/model"

	"github.com/prometheus/alertmanager/types"
)

var (
	// ErrNotFound is returned if a provider cannot find a requested item.
	ErrNotFound = fmt.Errorf("item not found")
)

// Iterator provides the functions common to all iterators. To be useful, a
// specific iterator interface (e.g. AlertIterator) has to be implemented that
// provides a Next method.
type Iterator interface {
	// Err returns the current error. It is not safe to call it concurrently
	// with other iterator methods or while reading from a channel returned
	// by the iterator.
	Err() error
	// Close must be called to release resources once the iterator is not
	// used anymore.
	Close()
}

// AlertIterator is an Iterator for Alerts.
type AlertIterator interface {
	Iterator
	// Next returns a channel that will be closed once the iterator is
	// exhausted. It is not necessary to exhaust the iterator but Close must
	// be called in any case to release resources used by the iterator (even
	// if the iterator is exhausted).
	Next() <-chan *types.Alert
}

// NewAlertIterator returns a new AlertIterator based on the generic alertIterator type
func NewAlertIterator(ch <-chan *types.Alert, done chan struct{}, err error) AlertIterator {
	return &alertIterator{
		ch:   ch,
		done: done,
		err:  err,
	}
}

// alertIterator implements AlertIterator. So far, this one fits all providers.
type alertIterator struct {
	ch   <-chan *types.Alert
	done chan struct{}
	err  error
}

func (ai alertIterator) Next() <-chan *types.Alert {
	return ai.ch
}

func (ai alertIterator) Err() error { return ai.err }
func (ai alertIterator) Close()     { close(ai.done) }

// Alerts gives access to a set of alerts. All methods are goroutine-safe.
// --------------------------------------------------------------------------
// Alerts 接口负责装载告警对象，并且可以提供告警的遍历器，而且可以设置或可以通过告警
// 指纹获得告警。全部的方法，都是协成安全。
type Alerts interface {

	// Subscribe returns an iterator over active alerts that have not been
	// resolved and successfully notified about.
	// They are not guaranteed to be in chronological order.
	// --------------------------------------------------------------------
	// Subscribe 方法，返回一个告警遍历器接口。遍历器会返回还没有解决和还没有被成功
	// 通知出来的告警。遍历器所返回的告警，并不能保证是按照时间顺序来进行排序的。
	Subscribe() AlertIterator

	// GetPending returns an iterator over all alerts that have
	// pending notifications.
	// --------------------------------------------------------------------
	// GetPending 方法，返回一个告警遍历器接口。遍历器会返回在等待通知的告警。
	GetPending() AlertIterator

	// Get returns the alert for a given fingerprint.
	// --------------------------------------------------------------------
	// Get 方法，通过告警的Label指纹，来获得Alert对象。Alert对象包含的信息，如
	// 过期时间，更新时间，标签，告警开始时间等等。
	Get(model.Fingerprint) (*types.Alert, error)

	// Put adds the given alert to the set.
	// --------------------------------------------------------------------
	// Put 方法，把零到多个告警，放入此告警集合里。
	Put(...*types.Alert) error
}
