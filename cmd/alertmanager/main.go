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

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/route"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/prometheus/alertmanager/api"
	"github.com/prometheus/alertmanager/cluster"
	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/nflog"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/notify/email"
	"github.com/prometheus/alertmanager/notify/opsgenie"
	"github.com/prometheus/alertmanager/notify/pagerduty"
	"github.com/prometheus/alertmanager/notify/pushover"
	"github.com/prometheus/alertmanager/notify/slack"
	"github.com/prometheus/alertmanager/notify/victorops"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/alertmanager/notify/wechat"
	"github.com/prometheus/alertmanager/provider/mem"
	"github.com/prometheus/alertmanager/silence"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/alertmanager/ui"
)

// 这些变量是普罗米修斯指标，用于记录alertmanager的数据。这里展示出了一个观点，
// 就是告警系统本身也是需要被监控和告警的。
var (

	// alertmanager web服务 http请求耗时的histogram指标。
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "alertmanager_http_request_duration_seconds",
			Help:    "Histogram of latencies for HTTP requests.",
			Buckets: []float64{.05, 0.1, .25, .5, .75, 1, 2, 5, 20, 60},
		},
		[]string{"handler", "method"},
	)

	// alertmanager web服务 http回复体的大小的histogram指标，这里使用了ExponentialBuckets，
	// 去提供bucket。给予初始值，增长乘积，和增长次数。来生成float64切片。
	responseSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "alertmanager_http_response_size_bytes",
			Help:    "Histogram of response size for HTTP requests.",
			Buckets: prometheus.ExponentialBuckets(100, 10, 7),
		},
		[]string{"handler", "method"},
	)

	// 创建alertmanager集群是否开启的gauge指标，用于监控alertmanager集群的状态。
	clusterEnabled = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_cluster_enabled",
			Help: "Indicates whether the clustering is enabled or not.",
		},
	)

	// TODO: 待确定。创建alertmanager配置的告警接收数量，gauge指标。
	configuredReceivers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_receivers",
			Help: "Number of configured receivers.",
		},
	)

	// TODO: 待确定。创建alertmanager配置的集成数量，gauge指标。
	configuredIntegrations = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "alertmanager_integrations",
			Help: "Number of configured integrations.",
		},
	)

	// prom日志配置结构体，里面包含允许的日志等级和日志的格式。
	promlogConfig = promlog.Config{}
)

// init golang 函数，会在main函数之前运行。把之前创建的所有指标注册到普罗米修斯
// 默认注册表里。
func init() {
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(responseSize)
	prometheus.MustRegister(clusterEnabled)
	prometheus.MustRegister(configuredReceivers)
	prometheus.MustRegister(configuredIntegrations)

	// 创建新的gauge指标并注册，此指标主要展示Version，Revision，Branch，GoVersion。
	prometheus.MustRegister(version.NewCollector("alertmanager"))
}

func instrumentHandler(handlerName string, handler http.HandlerFunc) http.HandlerFunc {
	handlerLabel := prometheus.Labels{"handler": handlerName}
	return promhttp.InstrumentHandlerDuration(
		requestDuration.MustCurryWith(handlerLabel),
		promhttp.InstrumentHandlerResponseSize(
			responseSize.MustCurryWith(handlerLabel),
			handler,
		),
	)
}

// 默认监听的集群端口，通过此端口来进行接收gossip协议的传输数据。alertmanager通过
// gossip协议互相传达集群状态，来实现此节点对整个集群状态的感知。
const defaultClusterAddr = "0.0.0.0:9094"

// buildReceiverIntegrations builds a list of integration notifiers off of a
// receiver config.
// ----------------------------------------------------------------------------
// buildReceiverIntegrations 通过接收人配置来创建集成通知列表
func buildReceiverIntegrations(nc *config.Receiver, tmpl *template.Template, logger log.Logger) ([]notify.Integration, error) {
	var (
		errs         types.MultiError
		integrations []notify.Integration
		add          = func(name string, i int, rs notify.ResolvedSender, f func(l log.Logger) (notify.Notifier, error)) {
			n, err := f(log.With(logger, "integration", name))
			if err != nil {
				errs.Add(err)
				return
			}
			integrations = append(integrations, notify.NewIntegration(n, rs, name, i))
		}
	)

	for i, c := range nc.WebhookConfigs {
		add("webhook", i, c, func(l log.Logger) (notify.Notifier, error) { return webhook.New(c, tmpl, l) })
	}
	for i, c := range nc.EmailConfigs {
		add("email", i, c, func(l log.Logger) (notify.Notifier, error) { return email.New(c, tmpl, l), nil })
	}
	for i, c := range nc.PagerdutyConfigs {
		add("pagerduty", i, c, func(l log.Logger) (notify.Notifier, error) { return pagerduty.New(c, tmpl, l) })
	}
	for i, c := range nc.OpsGenieConfigs {
		add("opsgenie", i, c, func(l log.Logger) (notify.Notifier, error) { return opsgenie.New(c, tmpl, l) })
	}
	for i, c := range nc.WechatConfigs {
		add("wechat", i, c, func(l log.Logger) (notify.Notifier, error) { return wechat.New(c, tmpl, l) })
	}
	for i, c := range nc.SlackConfigs {
		add("slack", i, c, func(l log.Logger) (notify.Notifier, error) { return slack.New(c, tmpl, l) })
	}
	for i, c := range nc.VictorOpsConfigs {
		add("victorops", i, c, func(l log.Logger) (notify.Notifier, error) { return victorops.New(c, tmpl, l) })
	}
	for i, c := range nc.PushoverConfigs {
		add("pushover", i, c, func(l log.Logger) (notify.Notifier, error) { return pushover.New(c, tmpl, l) })
	}
	if errs.Len() > 0 {
		return nil, &errs
	}
	return integrations, nil
}

// 程序入口main函数，interesting写法。
func main() {
	os.Exit(run())
}

// run函数，真实的程序主函数。初始化，并开启相应的服务。阻塞监听reload和关闭信号，会进行平滑退出。
func run() int {

	// 设置golang取样配置参数，只有在配置环境为DEBUG才会生效。
	if os.Getenv("DEBUG") != "" {
		// go routine 阻塞频率的取样设置，20代表20次阻塞才进行一次取样。
		runtime.SetBlockProfileRate(20)
		// 设置mutux锁的取样设置，20次锁时间才进行一次取样。
		runtime.SetMutexProfileFraction(20)
	}

	// 所有的用户传参，也可以通过--help指令或者help参数来查看英文的指示描述。
	var (
		// 配置文件目录
		configFile      = kingpin.Flag("config.file", "Alertmanager configuration file name.").Default("alertmanager.yml").String()
		// 数据保存的目录
		dataDir         = kingpin.Flag("storage.path", "Base path for data storage.").Default("data/").String()
		// 数据保存的时间长度
		retention       = kingpin.Flag("data.retention", "How long to keep data for.").Default("120h").Duration()
		// golang垃圾回收间隔设置
		alertGCInterval = kingpin.Flag("alerts.gc-interval", "Interval between alert GC.").Default("30m").Duration()

		// TODO: 好奇为什么需要这个外部url？
		externalURL    = kingpin.Flag("web.external-url", "The URL under which Alertmanager is externally reachable (for example, if Alertmanager is served via a reverse proxy). Used for generating relative and absolute links back to Alertmanager itself. If the URL has a path portion, it will be used to prefix all HTTP endpoints served by Alertmanager. If omitted, relevant URL components will be derived automatically.").String()
		routePrefix    = kingpin.Flag("web.route-prefix", "Prefix for the internal routes of web endpoints. Defaults to path of --web.external-url.").String()
		// alertmanager监听的地址和端口，默认监听0.0.0.0:9093
		listenAddress  = kingpin.Flag("web.listen-address", "Address to listen on for the web interface and API.").Default(":9093").String()
		// 设置最大get http请求并发数量，通常设置为系统逻辑核数
		getConcurrency = kingpin.Flag("web.get-concurrency", "Maximum number of GET requests processed concurrently. If negative or zero, the limit is GOMAXPROC or 8, whichever is larger.").Default("0").Int()
		// alertmanager本身的http服务的，请求超时时间。如为0，则无超时时间
		httpTimeout    = kingpin.Flag("web.timeout", "Timeout for HTTP requests. If negative or zero, no timeout is set.").Default("0").Duration()

		// 设置监听集群的地址，可以设置为空字符串去禁止集群
		clusterBindAddr = kingpin.Flag("cluster.listen-address", "Listen address for cluster. Set to empty string to disable HA mode.").
				Default(defaultClusterAddr).String()
		// TODO: 集群配置？？？？？
		clusterAdvertiseAddr = kingpin.Flag("cluster.advertise-address", "Explicit address to advertise in cluster.").String()
		peers                = kingpin.Flag("cluster.peer", "Initial peers (may be repeated).").Strings()
		peerTimeout          = kingpin.Flag("cluster.peer-timeout", "Time to wait between peers to send notifications.").Default("15s").Duration()
		gossipInterval       = kingpin.Flag("cluster.gossip-interval", "Interval between sending gossip messages. By lowering this value (more frequent) gossip messages are propagated across the cluster more quickly at the expense of increased bandwidth.").Default(cluster.DefaultGossipInterval.String()).Duration()
		pushPullInterval     = kingpin.Flag("cluster.pushpull-interval", "Interval for gossip state syncs. Setting this interval lower (more frequent) will increase convergence speeds across larger clusters at the expense of increased bandwidth usage.").Default(cluster.DefaultPushPullInterval.String()).Duration()
		tcpTimeout           = kingpin.Flag("cluster.tcp-timeout", "Timeout for establishing a stream connection with a remote node for a full state sync, and for stream read and write operations.").Default(cluster.DefaultTcpTimeout.String()).Duration()
		probeTimeout         = kingpin.Flag("cluster.probe-timeout", "Timeout to wait for an ack from a probed node before assuming it is unhealthy. This should be set to 99-percentile of RTT (round-trip time) on your network.").Default(cluster.DefaultProbeTimeout.String()).Duration()
		probeInterval        = kingpin.Flag("cluster.probe-interval", "Interval between random node probes. Setting this lower (more frequent) will cause the cluster to detect failed nodes more quickly at the expense of increased bandwidth usage.").Default(cluster.DefaultProbeInterval.String()).Duration()
		settleTimeout        = kingpin.Flag("cluster.settle-timeout", "Maximum time to wait for cluster connections to settle before evaluating notifications.").Default(cluster.DefaultPushPullInterval.String()).Duration()
		reconnectInterval    = kingpin.Flag("cluster.reconnect-interval", "Interval between attempting to reconnect to lost peers.").Default(cluster.DefaultReconnectInterval.String()).Duration()
		peerReconnectTimeout = kingpin.Flag("cluster.reconnect-timeout", "Length of time to attempt to reconnect to a lost peer.").Default(cluster.DefaultReconnectTimeout.String()).Duration()
	)

	// 通过flag设置日志配置
	promlogflag.AddFlags(kingpin.CommandLine, &promlogConfig)

	// 设置版本flag，会进行版本，分支，修订，build用户，build日期和go版本的打印。
	kingpin.Version(version.Print("alertmanager"))
	// 配置help flag的短名称 h
	kingpin.CommandLine.GetFlag("help").Short('h')
	// 读取用户运行传参
	kingpin.Parse()

	// 通过日志配置初始化logger
	logger := promlog.New(&promlogConfig)

	// 打印版本信息日志
	level.Info(logger).Log("msg", "Starting Alertmanager", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())

	// 创建alertmanager的数据目录，并给予读写权限。
	err := os.MkdirAll(*dataDir, 0777)
	if err != nil {
		level.Error(logger).Log("msg", "Unable to create data directory", "err", err)
		return 1
	}

	// 初始化集群
	var peer *cluster.Peer
	if *clusterBindAddr != "" {
		peer, err = cluster.Create(
			log.With(logger, "component", "cluster"),
			prometheus.DefaultRegisterer,
			*clusterBindAddr,
			*clusterAdvertiseAddr,
			*peers,
			true,
			*pushPullInterval,
			*gossipInterval,
			*tcpTimeout,
			*probeTimeout,
			*probeInterval,
		)
		if err != nil {
			level.Error(logger).Log("msg", "unable to initialize gossip mesh", "err", err)
			return 1
		}
		// 设置普罗米修斯集群指标，为已启用
		clusterEnabled.Set(1)
	}

	// 配置 stop channel 和 等待组，确保优雅退出
	stopc := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	// TODO: 配置集群广播日志信息配置，并配置广播？？？
	notificationLogOpts := []nflog.Option{
		nflog.WithRetention(*retention),
		nflog.WithSnapshot(filepath.Join(*dataDir, "nflog")),
		nflog.WithMaintenance(15*time.Minute, stopc, wg.Done),
		nflog.WithMetrics(prometheus.DefaultRegisterer),
		nflog.WithLogger(log.With(logger, "component", "nflog")),
	}

	notificationLog, err := nflog.New(notificationLogOpts...)
	if err != nil {
		level.Error(logger).Log("err", err)
		return 1
	}
	if peer != nil {
		c := peer.AddState("nfl", notificationLog, prometheus.DefaultRegisterer)
		notificationLog.SetBroadcast(c.Broadcast)
	}

	marker := types.NewMarker(prometheus.DefaultRegisterer)

	// 静默配置，并生成静默对象，如果有配置集群，则设置集群广播。
	silenceOpts := silence.Options{
		SnapshotFile: filepath.Join(*dataDir, "silences"),
		Retention:    *retention,
		Logger:       log.With(logger, "component", "silences"),
		Metrics:      prometheus.DefaultRegisterer,
	}

	silences, err := silence.New(silenceOpts)
	if err != nil {
		level.Error(logger).Log("err", err)
		return 1
	}
	if peer != nil {
		c := peer.AddState("sil", silences, prometheus.DefaultRegisterer)
		silences.SetBroadcast(c.Broadcast)
	}

	// Start providers before router potentially sends updates.
	wg.Add(1)
	// 维护静默产生的data数据，每十五分钟进行下数据删除。
	go func() {
		silences.Maintenance(15*time.Minute, filepath.Join(*dataDir, "silences"), stopc)
		wg.Done()
	}()

	// defer wg.Wait 确保最后所有的任务都优雅退出之后，才会程序退出。
	defer func() {
		close(stopc)
		wg.Wait()
	}()

	// 集群peer的状态监听器已经进行注册成功，现在可以进行加入集群和初始化状态。
	// Peer state listeners have been registered, now we can join and get the initial state.
	if peer != nil {
		err = peer.Join(
			*reconnectInterval,
			*peerReconnectTimeout,
		)
		if err != nil {
			level.Warn(logger).Log("msg", "unable to join gossip mesh", "err", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), *settleTimeout)
		defer func() {
			cancel()
			if err := peer.Leave(10 * time.Second); err != nil {
				level.Warn(logger).Log("msg", "unable to leave gossip mesh", "err", err)
			}
		}()
		go peer.Settle(ctx, *gossipInterval*10)
	}

	// 创建监控对象
	alerts, err := mem.NewAlerts(context.Background(), marker, *alertGCInterval, logger)
	if err != nil {
		level.Error(logger).Log("err", err)
		return 1
	}
	defer alerts.Close()

	var disp *dispatch.Dispatcher
	defer disp.Stop()

	// 创建分组方法
	groupFn := func(routeFilter func(*dispatch.Route) bool, alertFilter func(*types.Alert, time.Time) bool) (dispatch.AlertGroups, map[model.Fingerprint][]string) {
		return disp.Groups(routeFilter, alertFilter)
	}

	// alertmanager api结构体，包含所有版本V1,V2的http接口。
	api, err := api.New(api.Options{
		Alerts:      alerts,
		Silences:    silences,
		StatusFunc:  marker.Status,
		Peer:        peer,
		Timeout:     *httpTimeout,
		Concurrency: *getConcurrency,
		Logger:      log.With(logger, "component", "api"),
		Registry:    prometheus.DefaultRegisterer,
		GroupFunc:   groupFn,
	})

	if err != nil {
		level.Error(logger).Log("err", errors.Wrap(err, "failed to create API"))
		return 1
	}

	amURL, err := extURL(logger, os.Hostname, *listenAddress, *externalURL)
	if err != nil {
		level.Error(logger).Log("msg", "failed to determine external URL", "err", err)
		return 1
	}
	level.Debug(logger).Log("externalURL", amURL.String())

	waitFunc := func() time.Duration { return 0 }
	if peer != nil {
		waitFunc = clusterWait(peer, *peerTimeout)
	}
	timeoutFunc := func(d time.Duration) time.Duration {
		if d < notify.MinTimeout {
			d = notify.MinTimeout
		}
		return d + waitFunc()
	}

	var (
		inhibitor *inhibit.Inhibitor
		tmpl      *template.Template
	)

	// 创建普罗米修斯相关统计指标，创建配置协调者。
	dispMetrics := dispatch.NewDispatcherMetrics(prometheus.DefaultRegisterer)
	pipelineBuilder := notify.NewPipelineBuilder(prometheus.DefaultRegisterer)
	configLogger := log.With(logger, "component", "configuration")
	configCoordinator := config.NewCoordinator(
		*configFile,
		prometheus.DefaultRegisterer,
		configLogger,
	)
	configCoordinator.Subscribe(func(conf *config.Config) error {
		tmpl, err = template.FromGlobs(conf.Templates...)
		if err != nil {
			return errors.Wrap(err, "failed to parse templates")
		}
		tmpl.ExternalURL = amURL

		// Build the routing tree and record which receivers are used.
		// ------------------------------------------------------------
		// 建立路由树和记录下被使用的接收人
		routes := dispatch.NewRoute(conf.Route, nil)
		activeReceivers := make(map[string]struct{})
		routes.Walk(func(r *dispatch.Route) {
			activeReceivers[r.RouteOpts.Receiver] = struct{}{}
		})

		// Build the map of receiver to integrations.
		receivers := make(map[string][]notify.Integration, len(activeReceivers))
		var integrationsNum int
		// 循环加载所有配置中的接受消息人
		for _, rcv := range conf.Receivers {
			// 查看此接受消息人，没有任何route在使用，则进行记录和丢弃，然后开始下一轮。
			if _, found := activeReceivers[rcv.Name]; !found {
				// No need to build a receiver if no route is using it.
				level.Info(configLogger).Log("msg", "skipping creation of receiver not referenced by any route", "receiver", rcv.Name)
				continue
			}
			// 创建接收人的integration，并插入到receiver map中。
			integrations, err := buildReceiverIntegrations(rcv, tmpl, logger)
			if err != nil {
				return err
			}
			// rcv.Name is guaranteed to be unique across all receivers.
			receivers[rcv.Name] = integrations
			integrationsNum += len(integrations)
		}

		// 停止抑制器和调度器
		inhibitor.Stop()
		disp.Stop()

		// 创建静默，抑制器并放到pipeline里面，pipeline包含告警处理的每一个stage。
		inhibitor = inhibit.NewInhibitor(alerts, conf.InhibitRules, marker, logger)
		silencer := silence.NewSilencer(silences, marker, logger)
		pipeline := pipelineBuilder.New(
			receivers,
			waitFunc,
			inhibitor,
			silencer,
			notificationLog,
			peer,
		)
		configuredReceivers.Set(float64(len(activeReceivers)))
		configuredIntegrations.Set(float64(integrationsNum))

		// 更新api的抑制和静默数据
		api.Update(conf, func(labels model.LabelSet) {
			inhibitor.Mutes(labels)
			silencer.Mutes(labels)
		})

		// 创建alertmanager调度器
		disp = dispatch.NewDispatcher(alerts, routes, pipeline, marker, timeoutFunc, logger, dispMetrics)
		routes.Walk(func(r *dispatch.Route) {
			if r.RouteOpts.RepeatInterval > *retention {
				level.Warn(configLogger).Log(
					"msg",
					"repeat_interval is greater than the data retention period. It can lead to notifications being repeated more often than expected.",
					"repeat_interval",
					r.RouteOpts.RepeatInterval,
					"retention",
					*retention,
					"route",
					r.Key(),
				)
			}
		})

		// 独立协成去运行调度器和抑制器
		go disp.Run()
		go inhibitor.Run()

		return nil
	})

	if err := configCoordinator.Reload(); err != nil {
		return 1
	}

	// Make routePrefix default to externalURL path if empty string.
	if *routePrefix == "" {
		*routePrefix = amURL.Path
	}
	*routePrefix = "/" + strings.Trim(*routePrefix, "/")
	level.Debug(logger).Log("routePrefix", *routePrefix)

	router := route.New().WithInstrumentation(instrumentHandler)
	if *routePrefix != "/" {
		router.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, *routePrefix, http.StatusFound)
		})
		router = router.WithPrefix(*routePrefix)
	}

	webReload := make(chan chan error)

	ui.Register(router, webReload, logger)

	mux := api.Register(router, *routePrefix)

	srv := http.Server{Addr: *listenAddress, Handler: mux}
	srvc := make(chan struct{})


	// 开启alertmanager http服务
	go func() {
		level.Info(logger).Log("msg", "Listening", "address", *listenAddress)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			level.Error(logger).Log("msg", "Listen error", "err", err)
			close(srvc)
		}
		defer func() {
			if err := srv.Close(); err != nil {
				level.Error(logger).Log("msg", "Error on closing the server", "err", err)
			}
		}()
	}()

	// reload信号监听
	var (
		hup      = make(chan os.Signal, 1)
		hupReady = make(chan bool)
		term     = make(chan os.Signal, 1)
	)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-hupReady
		for {
			select {
			case <-hup:
				// ignore error, already logged in `reload()`
				_ = configCoordinator.Reload()
			case errc := <-webReload:
				errc <- configCoordinator.Reload()
			}
		}
	}()

	// Wait for reload or termination signals.
	close(hupReady) // Unblock SIGHUP handler.

	// 优雅退出监听。其实全部的优雅退出逻辑在defer里面。多个defer以先进后出的形式
	// 一个接一个的优雅退出。
	for {
		select {
		case <-term:
			level.Info(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
			return 0
		case <-srvc:
			return 1
		}
	}

	// TODO: 列举defer的执行顺序和作用。
}

// clusterWait returns a function that inspects the current peer state and returns
// a duration of one base timeout for each peer with a higher ID than ourselves.
func clusterWait(p *cluster.Peer, timeout time.Duration) func() time.Duration {
	return func() time.Duration {
		return time.Duration(p.Position()) * timeout
	}
}

func extURL(logger log.Logger, hostnamef func() (string, error), listen, external string) (*url.URL, error) {
	if external == "" {
		hostname, err := hostnamef()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listen)
		if err != nil {
			return nil, err
		}
		if port == "" {
			level.Warn(logger).Log("msg", "no port found for listen address", "address", listen)
		}

		external = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	u, err := url.Parse(external)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.Errorf("%q: invalid %q scheme, only 'http' and 'https' are supported", u.String(), u.Scheme)
	}

	ppref := strings.TrimRight(u.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	u.Path = ppref

	return u, nil
}
