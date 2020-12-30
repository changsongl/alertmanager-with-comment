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

// Package silence provides a storage for silences, which can share its
// state over a mesh network and snapshot it.
package silence

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/matttproud/golang_protobuf_extensions/pbutil"
	"github.com/pkg/errors"
	"github.com/prometheus/alertmanager/cluster"
	pb "github.com/prometheus/alertmanager/silence/silencepb"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	uuid "github.com/satori/go.uuid"
)

// ErrNotFound is returned if a silence was not found.
// --------------------------------------------------------
// ErrNotFound 当告警静默不存在时，会返回此error。
var ErrNotFound = fmt.Errorf("silence not found")

// ErrInvalidState is returned if the state isn't valid.
// --------------------------------------------------------
// ErrInvalidState 当反序列化告警时，发现告警数据状态有问题时，
// 返回此error。
var ErrInvalidState = fmt.Errorf("invalid state")

// utc时间方法，获取现在的时间。
func utcNow() time.Time {
	return time.Now().UTC()
}

// matcherCache 是对内存匹配器，map[静默规则] 匹配器 的封装
type matcherCache map[*pb.Silence]types.Matchers

// Get retrieves the matchers for a given silence. If it is a missed cache
// access, it compiles and adds the matchers of the requested silence to the
// cache.
// -----------------------------------------------------------------------------
// Get 获得某个静默规则的匹配器。如果当前内存获取失败，它将进行立即编译并且添加所
// 请求的匹配器到内存中。
func (c matcherCache) Get(s *pb.Silence) (types.Matchers, error) {
	if m, ok := c[s]; ok {
		return m, nil
	}
	return c.add(s)
}

// add compiles a silences' matchers and adds them to the cache.
// It returns the compiled matchers.
// -----------------------------------------------------------------------------
// add 编译静默的匹配器，并且把他们加入到内存当中。它回返回编译好的匹配器。
func (c matcherCache) add(s *pb.Silence) (types.Matchers, error) {
	var (
		ms types.Matchers
		mt *types.Matcher
	)

	// 循环Silence对象里面的每一个匹配器，并根据标签名和值来生成。
	// 并且还会判断值是否为正则。
	for _, m := range s.Matchers {
		mt = &types.Matcher{
			Name:  m.Name,
			Value: m.Pattern,
		}
		switch m.Type {
		case pb.Matcher_EQUAL:
			mt.IsRegex = false
		case pb.Matcher_REGEXP:
			mt.IsRegex = true
		}
		// 初始化匹配器，非常重要。在调用match方法前必须init。
		err := mt.Init()
		if err != nil {
			return nil, err
		}

		ms = append(ms, mt)
	}

	c[s] = ms

	return ms, nil
}

// Silencer binds together a Marker and a Silences to implement the Muter
// interface.
// -----------------------------------------------------------------------------
// Silencer 绑定一个 Silences 和一个 types.Marker 到一起。 Silences 存放所有的告警
// 规则，types.Marker 来标记告警是否被静默或同时被抑制。
type Silencer struct {
	silences *Silences // 所有告警规则
	marker   types.Marker // 标记告警静默抑制的marker
	logger   log.Logger // 日志
}

// NewSilencer returns a new Silencer.
// ----------------------------------------
// NewSilencer 返回一个新的 Silencer 结构体，来处理静默。
func NewSilencer(s *Silences, m types.Marker, l log.Logger) *Silencer {
	return &Silencer{
		silences: s,
		marker:   m,
		logger:   l,
	}
}

// Mutes implements the Muter interface.
// -----------------------------------------------------------------------------
// Silencer 实现 Muter interface。根据label来静默当前告警。
func (s *Silencer) Mutes(lset model.LabelSet) bool {
	// 获得当前告警label的指纹，通过指纹获得之前静默的id和
	// 静默规则总版本号。
	fp := lset.Fingerprint()
	ids, markerVersion, _ := s.marker.Silenced(fp)

	var (
		err        error
		sils       []*pb.Silence
		newVersion = markerVersion
	)

	// 之前的总版本号和现在的静默版本号一直，则代表可以继续检
	// 查之前的静默规则，查看之前相关的静默规则现在是否还有效。
	if markerVersion == s.silences.Version() {
		// No new silences added, just need to check which of the old
		// silences are still relevant.

		// 之前这个告警的静默规则是空的，则代表现在也无任何规则。则返回false。
		if len(ids) == 0 {
			// Super fast path: No silences ever applied to this
			// alert, none have been added. We are done.
			return false
		}

		// This is still a quite fast path: No silences have been added,
		// we only need to check which of the applicable silences are
		// currently active. Note that newVersion is left at
		// markerVersion because the Query call might already return a
		// newer version, which is not the version our old list of
		// applicable silences is based on.
		// --------------------------------------------------------------
		// 根据静默的IDs，来查找这些还是处在active的静默规则
		sils, _, err = s.silences.Query(
			QIDs(ids...),
			QState(types.SilenceStateActive),
		)
	} else {
		// New silences have been added, do a full query.
		// --------------------------------------------------------------
		// 因为版本已经发生了变化，因此需要全量的去查看现在匹配这个告警label的
		// 全量active静默规则。
		sils, newVersion, err = s.silences.Query(
			QState(types.SilenceStateActive),
			QMatches(lset),
		)
	}
	if err != nil {
		level.Error(s.logger).Log("msg", "Querying silences failed, alerts might not get silenced correctly", "err", err)
	}
	// 如果新的静默规则为空，则代表现在并无静默规则，则设置
	// 当前无静默，并且到更新版本。
	if len(sils) == 0 {
		s.marker.SetSilenced(fp, newVersion)
		return false
	}

	// 如果len无变化，则检查所有的id是否一致，如果发现不一致，则
	// 标记有变化。
	idsChanged := len(sils) != len(ids)
	if !idsChanged {
		// Length is the same, but is the content the same?
		for i, s := range sils {
			if ids[i] != s.Id {
				idsChanged = true
				break
			}
		}
	}

	// 静默id有变化，则从新排序id
	if idsChanged {
		// Need to recreate ids.
		ids = make([]string, len(sils))
		for i, s := range sils {
			ids[i] = s.Id
		}
		sort.Strings(ids) // For comparability.
	}

	// 如果版本有变化，或者active的静默ids变化，则进行对此
	// 告警进行重新配置静默。
	if idsChanged || newVersion != markerVersion {
		// Update marker only if something changed.
		s.marker.SetSilenced(fp, newVersion, ids...)
	}
	return true
}

// Silences holds a silence state that can be modified, queried, and snapshot.
// -----------------------------------------------------------------------------
// Silences 掌握一个静默的状态，可以被修改，查询，和快照。
type Silences struct {
	logger    log.Logger // log 对象
	metrics   *metrics // 存放告警状态，查询，快照等等的指标
	now       func() time.Time // 获取现在的时间
	retention time.Duration // 静默规则保留时间，用来设置静默规则的过期时间之后的保留时间。在静默规则
	                        // 结束后，将多保留这个规则一些时间。在保留时间过了之后的下一个GC周期，
	                        // 会对这个静默规则进行内存回收。

	mtx       sync.RWMutex // 对象锁
	st        state // 用来存所有静默规则的对象。
	version   int // Increments whenever silences are added.
	              // 版本号，当增加新的静默规则后会自增
	broadcast func([]byte) // 广播方法，用于集群下每个节点gossip协议传播方法
	mc        matcherCache // 存储每个静默规则和其匹配器
}

// 存放所有静默规则的普罗米修斯指标
type metrics struct {
	gcDuration              prometheus.Summary     // 静默规则垃圾回收所花的时间统计
	snapshotDuration        prometheus.Summary     // 生成静默快照所花的时间统计
	snapshotSize            prometheus.Gauge       // 快照的尺寸
	queriesTotal            prometheus.Counter     // 静默规则的查询次数
	queryErrorsTotal        prometheus.Counter     // 静默规则查询错误次数
	queryDuration           prometheus.Histogram   // 静默规则查询时间分布
	silencesActive          prometheus.GaugeFunc   // 存储当前激活的静默数量
	silencesPending         prometheus.GaugeFunc   // 存储当前即将发生的静默数量
	silencesExpired         prometheus.GaugeFunc   // 存储当前即将过期的静默数量
	propagatedMessagesTotal prometheus.Counter     // 存储收到的gossip并继续传给其他节点的数量
}

// 创建静默状态指标的通用方法， metrics.silencesActive, metrics.silencesPending 和
// metrics.silencesExpired 都是通过这个方法创建的。
func newSilenceMetricByState(s *Silences, st types.SilenceState) prometheus.GaugeFunc {
	return prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name:        "alertmanager_silences",
			Help:        "How many silences by state.",
			ConstLabels: prometheus.Labels{"state": string(st)},
		},
		func() float64 {
			count, err := s.CountState(st)
			if err != nil {
				level.Error(s.logger).Log("msg", "Counting silences failed", "err", err)
			}
			return float64(count)
		},
	)
}

// 创建普罗米修斯指标，并注册到普罗米修斯默认注册器中。返回 metrics 结构体。
func newMetrics(r prometheus.Registerer, s *Silences) *metrics {
	m := &metrics{}

	m.gcDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Name:       "alertmanager_silences_gc_duration_seconds",
		Help:       "Duration of the last silence garbage collection cycle.",
		Objectives: map[float64]float64{},
	})
	m.snapshotDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Name:       "alertmanager_silences_snapshot_duration_seconds",
		Help:       "Duration of the last silence snapshot.",
		Objectives: map[float64]float64{},
	})
	m.snapshotSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alertmanager_silences_snapshot_size_bytes",
		Help: "Size of the last silence snapshot in bytes.",
	})
	m.queriesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_silences_queries_total",
		Help: "How many silence queries were received.",
	})
	m.queryErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_silences_query_errors_total",
		Help: "How many silence received queries did not succeed.",
	})
	m.queryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "alertmanager_silences_query_duration_seconds",
		Help: "Duration of silence query evaluation.",
	})
	m.propagatedMessagesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "alertmanager_silences_gossip_messages_propagated_total",
		Help: "Number of received gossip messages that have been further gossiped.",
	})
	if s != nil {
		m.silencesActive = newSilenceMetricByState(s, types.SilenceStateActive)
		m.silencesPending = newSilenceMetricByState(s, types.SilenceStatePending)
		m.silencesExpired = newSilenceMetricByState(s, types.SilenceStateExpired)
	}

	if r != nil {
		r.MustRegister(
			m.gcDuration,
			m.snapshotDuration,
			m.snapshotSize,
			m.queriesTotal,
			m.queryErrorsTotal,
			m.queryDuration,
			m.silencesActive,
			m.silencesPending,
			m.silencesExpired,
			m.propagatedMessagesTotal,
		)
	}
	return m
}

// Options exposes configuration options for creating a new Silences object.
// Its zero value is a safe default.
// ------------------------------------------------------------------------------
// Options 暴露一些创建静默时使用的配置可选项，nil 是一个安全的默认项。
type Options struct {
	// A snapshot file or reader from which the initial state is loaded.
	// None or only one of them must be set.
	// -------------------------------------------------------------------
	// 一个快照文件或者一个reader，用来加载初始状态。这两个配置，只能全部不配置，
	// 或者只配置一个。
	SnapshotFile   string
	SnapshotReader io.Reader

	// Retention time for newly created Silences. Silences may be
	// garbage collected after the given duration after they ended.
	// -------------------------------------------------------------------
	// Retention 时间是给新创建的静默规则来使用的，配置静默规则的保留时间。
	// 静默规则只有过了结束时间后，并且过了保留时间后才可以被垃圾回收掉。
	Retention time.Duration

	// A logger used by background processing.
	// -------------------------------------------------------------------
	// 日志对象和普罗米修斯指标注册器对象
	Logger  log.Logger
	Metrics prometheus.Registerer
}

// 检查是否文件和reader都同事配置了，只能全不配置，或者只配置一个。
func (o *Options) validate() error {
	if o.SnapshotFile != "" && o.SnapshotReader != nil {
		return fmt.Errorf("only one of SnapshotFile and SnapshotReader must be set")
	}
	return nil
}

// New returns a new Silences object with the given configuration.
// ------------------------------------------------------------------------------
// New 根据配置返回一个新的 Silences 对象，
func New(o Options) (*Silences, error) {
	// 检验配置项，并查看是否有需要生成快照文件句柄
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.SnapshotFile != "" {
		if r, err := os.Open(o.SnapshotFile); err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else {
			o.SnapshotReader = r
		}
	}

	// 生成 Silences 对象
	s := &Silences{
		mc:        matcherCache{},
		logger:    log.NewNopLogger(),
		retention: o.Retention,
		now:       utcNow,
		broadcast: func([]byte) {},
		st:        state{},
	}
	s.metrics = newMetrics(o.Metrics, s)

	if o.Logger != nil {
		s.logger = o.Logger
	}
	// 查看是否需要加载快照
	if o.SnapshotReader != nil {
		if err := s.loadSnapshot(o.SnapshotReader); err != nil {
			return s, err
		}
	}
	return s, nil
}

// Maintenance garbage collects the silence state at the given interval. If the snapshot
// file is set, a snapshot is written to it afterwards.
// Terminates on receiving from stopc.
// ------------------------------------------------------------------------------
// Maintenance 是周期性的垃圾回收静默。如果快照文件已经设置，快照会在垃圾回收后写到里面。
// 这个方法会在收到stopc消息之后终止。
func (s *Silences) Maintenance(interval time.Duration, snapf string, stopc <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()

	// 垃圾回收方法，和生成快照的逻辑方法
	f := func() error {
		start := s.now()
		var size int64

		level.Debug(s.logger).Log("msg", "Running maintenance")
		defer func() {
			level.Debug(s.logger).Log("msg", "Maintenance done", "duration", s.now().Sub(start), "size", size)
			s.metrics.snapshotSize.Set(float64(size))
		}()

		if _, err := s.GC(); err != nil {
			return err
		}
		if snapf == "" {
			return nil
		}
		f, err := openReplace(snapf)
		if err != nil {
			return err
		}
		if size, err = s.Snapshot(f); err != nil {
			return err
		}
		return f.Close()
	}

	// 循环定期垃圾回收，同时监听终止channel
Loop:
	for {
		select {
		case <-stopc:
			break Loop
		case <-t.C:
			if err := f(); err != nil {
				level.Info(s.logger).Log("msg", "Running maintenance failed", "err", err)
			}
		}
	}

	// No need for final maintenance if we don't want to snapshot.
	// ------------------------------------------------------------
	// 检查是否有必要退出前，在保存一下快照。
	if snapf == "" {
		return
	}
	if err := f(); err != nil {
		level.Info(s.logger).Log("msg", "Creating shutdown snapshot failed", "err", err)
	}
}

// GC runs a garbage collection that removes silences that have ended longer
// than the configured retention time ago.
// ------------------------------------------------------------------------------
// GC 运行删除静默规则的垃圾回收方法，垃圾回收会删除到期并且已经过了保留时间的静默。
func (s *Silences) GC() (int, error) {
	// 垃圾回收时间统计
	start := time.Now()
	defer func() { s.metrics.gcDuration.Observe(time.Since(start).Seconds()) }()

	now := s.now()
	var n int

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// 循环每一个静默规则，删除所有过期的静默规则
	for id, sil := range s.st {
		if sil.ExpiresAt.IsZero() {
			return n, errors.New("unexpected zero expiration timestamp")
		}
		if !sil.ExpiresAt.After(now) {
			delete(s.st, id)
			delete(s.mc, sil.Silence)
			n++
		}
	}

	return n, nil
}

// 检查Matcher的数据是否合法。
func validateMatcher(m *pb.Matcher) error {
	if !model.LabelName(m.Name).IsValid() {
		return fmt.Errorf("invalid label name %q", m.Name)
	}
	switch m.Type {
	case pb.Matcher_EQUAL:
		if !model.LabelValue(m.Pattern).IsValid() {
			return fmt.Errorf("invalid label value %q", m.Pattern)
		}
	case pb.Matcher_REGEXP:
		if _, err := regexp.Compile(m.Pattern); err != nil {
			return fmt.Errorf("invalid regular expression %q: %s", m.Pattern, err)
		}
	default:
		return fmt.Errorf("unknown matcher type %q", m.Type)
	}
	return nil
}

// 检查匹配器是否为空。
func matchesEmpty(m *pb.Matcher) bool {
	switch m.Type {
	case pb.Matcher_EQUAL:
		return m.Pattern == ""
	case pb.Matcher_REGEXP:
		matched, _ := regexp.MatchString(m.Pattern, "")
		return matched
	default:
		return false
	}
}

// 检查匹配器方法，如是否有匹配条件，匹配器是否合法（如正则是否合法），是否匹配器为空。
// 静默规则的时间是否合法，如开始结束时间是否为空，是否结束时间比开始时间早等等。
func validateSilence(s *pb.Silence) error {
	if s.Id == "" {
		return errors.New("ID missing")
	}
	if len(s.Matchers) == 0 {
		return errors.New("at least one matcher required")
	}
	allMatchEmpty := true
	for i, m := range s.Matchers {
		if err := validateMatcher(m); err != nil {
			return fmt.Errorf("invalid label matcher %d: %s", i, err)
		}
		allMatchEmpty = allMatchEmpty && matchesEmpty(m)
	}
	if allMatchEmpty {
		return errors.New("at least one matcher must not match the empty string")
	}
	if s.StartsAt.IsZero() {
		return errors.New("invalid zero start timestamp")
	}
	if s.EndsAt.IsZero() {
		return errors.New("invalid zero end timestamp")
	}
	if s.EndsAt.Before(s.StartsAt) {
		return errors.New("end time must not be before start time")
	}
	if s.UpdatedAt.IsZero() {
		return errors.New("invalid zero update timestamp")
	}
	return nil
}

// cloneSilence returns a shallow copy of a silence.
// ------------------------------------------------------------------------------
// cloneSilence 克隆静默规则的方法
func cloneSilence(sil *pb.Silence) *pb.Silence {
	s := *sil
	return &s
}

// 根据静默Id获得静默规则
func (s *Silences) getSilence(id string) (*pb.Silence, bool) {
	msil, ok := s.st[id]
	if !ok {
		return nil, false
	}
	return msil.Silence, true
}

// 设置静默规则核心方法。会检验静默规则是否合法，广播给其他节点。
func (s *Silences) setSilence(sil *pb.Silence, now time.Time) error {
	sil.UpdatedAt = now

	// 检验静默是否合法
	if err := validateSilence(sil); err != nil {
		return errors.Wrap(err, "silence invalid")
	}

	msil := &pb.MeshSilence{
		Silence:   sil,
		ExpiresAt: sil.EndsAt.Add(s.retention),
	}

	// 序列化静默，并广播给其他节点。静默规则版本号+1
	b, err := marshalMeshSilence(msil)
	if err != nil {
		return err
	}

	if s.st.merge(msil, now) {
		s.version++
	}
	s.broadcast(b)

	return nil
}

// Set the specified silence. If a silence with the ID already exists and the modification
// modifies history, the old silence gets expired and a new one is created.
// ------------------------------------------------------------------------------
// Set 是用来设置的静默的公开方法。如果静默的ID已经存在，如果旧的静默已经过期了，将会创建一个新的。
func (s *Silences) Set(sil *pb.Silence) (string, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// 查看以前静默是否存在
	now := s.now()
	prev, ok := s.getSilence(sil.Id)

	if sil.Id != "" && !ok {
		return "", ErrNotFound
	}
	// 存在的话就检查是否可以更新，如果不能更新的话。如果没有过期的话，
	// 则让它过期。
	if ok {
		if canUpdate(prev, sil, now) {
			return sil.Id, s.setSilence(sil, now)
		}
		if getState(prev, s.now()) != types.SilenceStateExpired {
			// We cannot update the silence, expire the old one.
			if err := s.expire(prev.Id); err != nil {
				return "", errors.Wrap(err, "expire previous silence")
			}
		}
	}

	// If we got here it's either a new silence or a replacing one.
	// ---------------------------------------------------------------
	// 如果逻辑跑到这里，则代表是一个新的静默，或者是要进行替换。
	sil.Id = uuid.NewV4().String()

	// 如果开始时间比现在早，则使用现在的时间作为开始时间
	if sil.StartsAt.Before(now) {
		sil.StartsAt = now
	}

	return sil.Id, s.setSilence(sil, now)
}

// canUpdate returns true if silence a can be updated to b without
// affecting the historic view of silencing.
// ------------------------------------------------------------------------------
// canUpdate 如果a（之前的）静默可以在不影响静默历史的情况下去更新b（新的）静默，则返回true。
func canUpdate(a, b *pb.Silence, now time.Time) bool {
	// 查看是否两个匹配器是否深度相等，则不能跟新。
	if !reflect.DeepEqual(a.Matchers, b.Matchers) {
		return false
	}

	// Allowed timestamp modifications depend on the current time.
	// 检查静默a（之前的）的状态
	switch st := getState(a, now); st {
	case types.SilenceStateActive: // a（之前的）静默已经激活了
		if !b.StartsAt.Equal(a.StartsAt) { // a（之前的）和b（新的）的开始时间不相等，则不能更新
			return false
		}
		if b.EndsAt.Before(now) { // 如果b（新的）的结束时间比现在早，也不能更新
			return false
		}
	case types.SilenceStatePending: // a（之前的）静默状态是即将发生状态
		if b.StartsAt.Before(now) { // b（新的）静默在比现在早的话，也不能更新
			return false
		}
	case types.SilenceStateExpired: // 如果a（之前的）静默已经过期，则也不能更新
		return false
	default:
		panic("unknown silence state")
	}
	return true
}

// Expire the silence with the given ID immediately.
// -----------------------------------------------------
// Expire 公开方法，根据id过期某个静默
func (s *Silences) Expire(id string) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.expire(id)
}

// Expire the silence with the given ID immediately.
// -----------------------------------------------------
// expire 私有核心方法，根据不同的静默状态，来决定不同处理方式。
// 修改克隆的静默，并修改状态，并覆盖掉。
func (s *Silences) expire(id string) error {
	sil, ok := s.getSilence(id)
	if !ok {
		return ErrNotFound
	}

	// 克隆获取的静默
	sil = cloneSilence(sil)
	now := s.now()

	// 如果已经过期，则返回错误。
	// 如果静默已经开启，则设置结束时间为现在
	// 如果还未开始，则开始和结束时间都是现在
	switch getState(sil, now) {
	case types.SilenceStateExpired:
		return errors.Errorf("silence %s already expired", id)
	case types.SilenceStateActive:
		sil.EndsAt = now
	case types.SilenceStatePending:
		// Set both to now to make Silence move to "expired" state
		sil.StartsAt = now
		sil.EndsAt = now
	}

	// 覆盖静默
	return s.setSilence(sil, now)
}

// QueryParam expresses parameters along which silences are queried.
// ----------------------------------------------------------------------------
// QueryParam 查询方法封装类，它会操作query对象。
type QueryParam func(*query) error

// 查询结构体，包含id和过滤方法的切片
type query struct {
	ids     []string
	filters []silenceFilter
}

// silenceFilter is a function that returns true if a silence
// should be dropped from a result set for a given time.
// ----------------------------------------------------------------------------
// silenceFilter 是这个方法是专门用来过滤静默的。这个方法是返回bool来根据某个时间决定
// 是否需要丢弃这个静默。
type silenceFilter func(*pb.Silence, *Silences, time.Time) (bool, error)

// QIDs configures a query to select the given silence IDs.
// ----------------------------------------------------------------------------
// QIDs 通过ids返回一个QueryParam方法，会把id追加到 query 对象的 ids 里面。
func QIDs(ids ...string) QueryParam {
	return func(q *query) error {
		q.ids = append(q.ids, ids...)
		return nil
	}
}

// QMatches returns silences that match the given label set.
// ----------------------------------------------------------------------------
// QMatches 根据标签返回一个QueryParam方法，根据标签过滤的方法。
func QMatches(set model.LabelSet) QueryParam {
	return func(q *query) error {
		f := func(sil *pb.Silence, s *Silences, _ time.Time) (bool, error) {
			m, err := s.mc.Get(sil)
			if err != nil {
				return true, err
			}
			return m.Match(set), nil
		}
		q.filters = append(q.filters, f)
		return nil
	}
}

// getState returns a silence's SilenceState at the given timestamp.
// ------------------------------------------------------------------------------
// getState 返回静默规则的状态。如果静默还没开始，则是pending状态，
// 如果已经过了结束时间，则是过期状态。如果正在发生中，则是激活状态。
func getState(sil *pb.Silence, ts time.Time) types.SilenceState {
	if ts.Before(sil.StartsAt) {
		return types.SilenceStatePending
	}
	if ts.After(sil.EndsAt) {
		return types.SilenceStateExpired
	}
	return types.SilenceStateActive
}

// QState filters queried silences by the given states.
// ----------------------------------------------------------------------------
// QState 根据提供的状态返回一个QueryParam方法，根据静默状态过滤的方法。
func QState(states ...types.SilenceState) QueryParam {
	return func(q *query) error {
		f := func(sil *pb.Silence, _ *Silences, now time.Time) (bool, error) {
			s := getState(sil, now)

			for _, ps := range states {
				if s == ps {
					return true, nil
				}
			}
			return false, nil
		}
		q.filters = append(q.filters, f)
		return nil
	}
}

// QueryOne queries with the given parameters and returns the first result.
// Returns ErrNotFound if the query result is empty.
// ----------------------------------------------------------------------------
// QueryOne 根据所有QueryParam，来进行查询。假如没有查到，则返回 ErrNotFound ，
// 如果有查到，则使用第一个静默。
func (s *Silences) QueryOne(params ...QueryParam) (*pb.Silence, error) {
	res, _, err := s.Query(params...)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	return res[0], nil
}

// Query for silences based on the given query parameters. It returns the
// resulting silences and the state version the result is based on.
// ------------------------------------------------------------------------
// 查询静默规则，使用protobuf结构体，但是获取规则非GRPC调用。
func (s *Silences) Query(params ...QueryParam) ([]*pb.Silence, int, error) {
	// 普罗米修斯指标进行处理
	s.metrics.queriesTotal.Inc()
	defer prometheus.NewTimer(s.metrics.queryDuration).ObserveDuration()

	// 应用param到query上
	q := &query{}
	for _, p := range params {
		if err := p(q); err != nil {
			s.metrics.queryErrorsTotal.Inc()
			return nil, s.Version(), err
		}
	}

	// 查询query
	sils, version, err := s.query(q, s.now())
	if err != nil {
		s.metrics.queryErrorsTotal.Inc()
	}
	return sils, version, err
}

// Version of the silence state.
// ------------------------------------------------------
// Version 返回静默版本号
func (s *Silences) Version() int {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.version
}

// CountState counts silences by state.
// ------------------------------------------------------
// CountState 根据状态来数多少静默
func (s *Silences) CountState(states ...types.SilenceState) (int, error) {
	// This could probably be optimized.
	sils, _, err := s.Query(QState(states...))
	if err != nil {
		return -1, err
	}
	return len(sils), nil
}

// 查询query和当前时间来查询静默规则，如果未提供静默id，则进行全量查询。
// 如果提供，则进行部分查询。
func (s *Silences) query(q *query, now time.Time) ([]*pb.Silence, int, error) {
	// If we have no ID constraint, all silences are our base set.  This and
	// the use of post-filter functions is the trivial solution for now.
	var res []*pb.Silence

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// 已提供id，查看现在这些id是否还存在
	if q.ids != nil {
		for _, id := range q.ids {
			if s, ok := s.st[id]; ok {
				res = append(res, s.Silence)
			}
		}
	} else { // 未提供id，全量的静默规则进行查找
		for _, sil := range s.st {
			res = append(res, sil.Silence)
		}
	}

	// 进行静默规则检查，如检查不满足则从最终结果里去除
	var resf []*pb.Silence
	for _, sil := range res {
		remove := false
		for _, f := range q.filters {
			ok, err := f(sil, s, now)
			if err != nil {
				return nil, s.version, err
			}
			if !ok {
				remove = true
				break
			}
		}
		if !remove {
			resf = append(resf, cloneSilence(sil))
		}
	}

	return resf, s.version, nil
}

// loadSnapshot loads a snapshot generated by Snapshot() into the state.
// Any previous state is wiped.
// ------------------------------------------------------------------------
// loadSnapshot 加载通过 Snapshot() 产生的快照到 Silences.st (state) 里面。
// 以前的 state 都会被抹除。并且版本号+1
func (s *Silences) loadSnapshot(r io.Reader) error {
	st, err := decodeState(r)
	if err != nil {
		return err
	}
	for _, e := range st {
		// Comments list was moved to a single comment. Upgrade on loading the snapshot.
		if len(e.Silence.Comments) > 0 {
			e.Silence.Comment = e.Silence.Comments[0].Comment
			e.Silence.CreatedBy = e.Silence.Comments[0].Author
			e.Silence.Comments = nil
		}
		st[e.Silence.Id] = e
	}
	s.mtx.Lock()
	s.st = st
	s.version++
	s.mtx.Unlock()

	return nil
}

// Snapshot writes the full internal state into the writer and returns the number of bytes
// written.
// -----------------------------------------------------------------------------------------
// Snapshot 把内存里的静默状态写到io.writer里，并且返回已经写了多少个bytes。
func (s *Silences) Snapshot(w io.Writer) (int64, error) {
	start := time.Now()
	defer func() { s.metrics.snapshotDuration.Observe(time.Since(start).Seconds()) }()

	s.mtx.RLock()
	defer s.mtx.RUnlock()

	// 压缩内容为二进制
	b, err := s.st.MarshalBinary()
	if err != nil {
		return 0, err
	}

	return io.Copy(w, bytes.NewReader(b))
}

// MarshalBinary serializes all silences.
// ------------------------------------------------------------------
// MarshalBinary 序列化所有的静默，返回序列化之后的内容
func (s *Silences) MarshalBinary() ([]byte, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return s.st.MarshalBinary()
}

// Merge merges silence state received from the cluster with the local state.
// ------------------------------------------------------------------------------------
// Merge 从集群获得的静默状态合并到本地的静默状态
func (s *Silences) Merge(b []byte) error {
	// 解析出所有静默状态
	st, err := decodeState(bytes.NewReader(b))
	if err != nil {
		return err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()

	now := s.now()

	// 循环每个静默状态，如果合并成功，版本号加一。
	// 如果消息已经超长了，则不进行广播，否则进行广播，
	// 并统计+1。
	for _, e := range st {
		if merged := s.st.merge(e, now); merged {
			s.version++
			if !cluster.OversizedMessage(b) {
				// If this is the first we've seen the message and it's
				// not oversized, gossip it to other nodes. We don't
				// propagate oversized messages because they're sent to
				// all nodes already.
				s.broadcast(b)
				s.metrics.propagatedMessagesTotal.Inc()
				level.Debug(s.logger).Log("msg", "Gossiping new silence", "silence", e)
			}
		}
	}
	return nil
}

// SetBroadcast sets the provided function as the one creating data to be
// broadcast.
// ------------------------------------------------------------------------------------
// SetBroadcast 设置广播的方法。
func (s *Silences) SetBroadcast(f func([]byte)) {
	s.mtx.Lock()
	s.broadcast = f
	s.mtx.Unlock()
}

// 存储所有静默规则的map，map[静默id]静默状态
type state map[string]*pb.MeshSilence

// 合并静默规则，如果静默规则已经过期，则不合并。之前静默不存在，
// 或者之前静默的更新时间要比这个静默早，则替换掉之前的告警。
func (s state) merge(e *pb.MeshSilence, now time.Time) bool {
	id := e.Silence.Id
	if e.ExpiresAt.Before(now) {
		return false
	}
	// Comments list was moved to a single comment. Apply upgrade
	// on silences received from peers.
	if len(e.Silence.Comments) > 0 {
		e.Silence.Comment = e.Silence.Comments[0].Comment
		e.Silence.CreatedBy = e.Silence.Comments[0].Author
		e.Silence.Comments = nil
	}

	prev, ok := s[id]
	if !ok || prev.Silence.UpdatedAt.Before(e.Silence.UpdatedAt) {
		s[id] = e
		return true
	}
	return false
}

// 将当前状态，进行序列化为二进制
func (s state) MarshalBinary() ([]byte, error) {
	var buf bytes.Buffer

	for _, e := range s {
		if _, err := pbutil.WriteDelimited(&buf, e); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// 反序列化，从二进制转为 state 对象
func decodeState(r io.Reader) (state, error) {
	st := state{}
	for {
		var s pb.MeshSilence
		_, err := pbutil.ReadDelimited(r, &s)
		if err == nil {
			if s.Silence == nil {
				return nil, ErrInvalidState
			}
			st[s.Silence.Id] = &s
			continue
		}
		if err == io.EOF {
			break
		}
		return nil, err
	}
	return st, nil
}

// 序列化静默规则
func marshalMeshSilence(e *pb.MeshSilence) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := pbutil.WriteDelimited(&buf, e); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// replaceFile wraps a file that is moved to another filename on closing.
// ------------------------------------------------------------------------------------
// replaceFile 包了一个文件对象，还有一个要替换的文件名。
type replaceFile struct {
	*os.File
	filename string
}

// 关闭替换文件，把未写完的内容刷到文件里，然后关闭文件。最后替换掉
// replaceFile.filename 的文件。
func (f *replaceFile) Close() error {
	if err := f.File.Sync(); err != nil {
		return err
	}
	if err := f.File.Close(); err != nil {
		return err
	}
	return os.Rename(f.File.Name(), f.filename)
}

// openReplace opens a new temporary file that is moved to filename on closing.
// ------------------------------------------------------------------------------------
// openReplace 打开一个 filename 和 一个随机数的文件。并回返 replaceFile 对象。
// 当关闭的时候，将这个随机数文件，重命名到 filename 文件。
func openReplace(filename string) (*replaceFile, error) {
	tmpFilename := fmt.Sprintf("%s.%x", filename, uint64(rand.Int63()))

	f, err := os.Create(tmpFilename)
	if err != nil {
		return nil, err
	}

	rf := &replaceFile{
		File:     f,
		filename: filename,
	}
	return rf, nil
}
