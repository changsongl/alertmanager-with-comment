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
	// -------------------------------------------------------------------------
	// ErrNotFound 当没有找到请求对象时，返回此error。没被引用，WTF????!!!!
	ErrNotFound = fmt.Errorf("item not found")
)

// Iterator provides the functions common to all iterators. To be useful, a
// specific iterator interface (e.g. AlertIterator) has to be implemented that
// provides a Next method.
// -----------------------------------------------------------------------------
// Iterator 是一个interface，提供遍历器的基础方法。为了方便使用，需要去使用一个具体的遍历器
// 接口去实现具体的功能。Ex: AlertIterator 就是一个告警行为的遍历器，内嵌基础 Iterator 的
// 通用功能，其提供 Next 方法来使用。这也算是Golang继承的一种方式。
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
// ----------------------------------------------------------
// AlertIterator 是一个遍历器接头，专门为 Alerts 接口来使用的。
// 里面包含一个遍历器接口和 Next 方法将返回一个告警channel，当遍历器
// 已经遍历完成，需要将这个channel关闭。在没有遍历完成也可以进行channel
// 的关闭，因为只有关闭才会对遍历器使用的资源进行释放。
type AlertIterator interface {
	Iterator
	// Next returns a channel that will be closed once the iterator is
	// exhausted. It is not necessary to exhaust the iterator but Close must
	// be called in any case to release resources used by the iterator (even
	// if the iterator is exhausted).
	// -----------------------------------------------------------------------
	// Next 返回一个channel。当遍历器已经遍历完成，需要将这个channel关闭。在没有遍历
	// 完成也可以进行channel的关闭，因为只有关闭才会对遍历器使用的资源进行释放。
	Next() <-chan *types.Alert
}

// NewAlertIterator returns a new AlertIterator based on the generic alertIterator type
// -----------------------------------------------------------------------------------------
// NewAlertIterator 返回一个 AlertIterator 接口对象，底层实现类型是通过 alertIterator 来实现。
// Golang典型的设计思维，实现类接口为私有类，通过实现接口的方式，暴露出公开方法。
func NewAlertIterator(ch <-chan *types.Alert, done chan struct{}, err error) AlertIterator {
	return &alertIterator{
		ch:   ch,
		done: done,
		err:  err,
	}
}

// alertIterator implements AlertIterator. So far, this one fits all providers.
// -----------------------------------------------------------------------------------------
// alertIterator 实现了 AlertIterator 接口。以现在来说，这个实现满足现在所有providers的需求。
// 但是假如有新的需求，可以通过创建一个新的实现类，来实现 AlertIterator 接口，来让代码变得通用。
type alertIterator struct {
	ch   <-chan *types.Alert // ch   元素用来遍历告警的队列。
	done chan struct{}       // done 是用来通知这个遍历器被关闭。
	err  error               // err  用来存储是否有错误，当调用完 Next 方法后，需要拿到 err 判断是否有错误。
}

// Next 方法，获取下一个告警。
func (ai alertIterator) Next() <-chan *types.Alert {
	return ai.ch
}

// Err 方法，返回遍历时的错误。
func (ai alertIterator) Err() error { return ai.err }

// Close 关闭 done channel，停止遍历。
func (ai alertIterator) Close() { close(ai.done) }

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
