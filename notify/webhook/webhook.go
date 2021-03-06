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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/version"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// userAgentHeader 带版本号的客户端头部
var userAgentHeader = fmt.Sprintf("Alertmanager/%s", version.Version)

// Notifier implements a Notifier for generic webhooks.
// ----------------------------------------------------------------------------
// Notifier 是通用webhook类, 实现 notify.Notifier 接口
type Notifier struct {
	conf    *config.WebhookConfig // webhook配置
	tmpl    *template.Template    // 模板对象(golang)
	logger  log.Logger
	client  *http.Client
	retrier *notify.Retrier
}

// New returns a new Webhook.
// ----------------------------------------------------------------------------
// New 返回一个webhook
func New(conf *config.WebhookConfig, t *template.Template, l log.Logger) (*Notifier, error) {
	// 创建一个Http Client
	client, err := commoncfg.NewClientFromConfig(*conf.HTTPConfig, "webhook", false)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		conf:   conf,
		tmpl:   t,
		logger: l,
		client: client,
		// Webhooks are assumed to respond with 2xx response codes on a successful
		// request and 5xx response codes are assumed to be recoverable.
		// ----------------------------------------------------------------------------
		// Webhook重试器，默认认为2xx的回复状态码代表成功，5xx的恢复状态码代表失败，并且
		// 认为可以被恢复并重试。
		retrier: &notify.Retrier{
			CustomDetailsFunc: func(int, io.Reader) string {
				return conf.URL.String()
			},
		},
	}, nil
}

// Message defines the JSON object send to webhook endpoints.
// ----------------------------------------------------------------------------
// Message 定义了发给Webhook服务器的对象。
type Message struct {
	*template.Data // 告警的元数据

	// The protocol version.
	// 告警版本
	Version string `json:"version"`
	// 分组Key
	GroupKey string `json:"groupKey"`
	// 截取掉的告警数量，假如告警数量过多，会截取超过上限的那部分告警。
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

// truncateAlerts 解决掉超过 maxAlerts 数量的告警，并把截取后的告警切片和截取掉的数量返回。
func truncateAlerts(maxAlerts uint64, alerts []*types.Alert) ([]*types.Alert, uint64) {
	if maxAlerts != 0 && uint64(len(alerts)) > maxAlerts {
		return alerts[:maxAlerts], uint64(len(alerts)) - maxAlerts
	}

	return alerts, 0
}

// Notify implements the Notifier interface.
// ----------------------------------------------------------------------------
// Notify 实现了 Notifier 接口。准备要发送的告警数据，序列化数据后进行发送。
// 发送完成后，查看结果是否需要进行重试。
func (n *Notifier) Notify(ctx context.Context, alerts ...*types.Alert) (bool, error) {
	// 截取告警，并从元数据里提取出data, 获取group key。
	alerts, numTruncated := truncateAlerts(n.conf.MaxAlerts, alerts)
	data := notify.GetTemplateData(ctx, n.tmpl, alerts, n.logger)

	groupKey, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		level.Error(n.logger).Log("err", err)
	}

	// 打包Message, 并序列化
	msg := &Message{
		Version:         "4",
		Data:            data,
		GroupKey:        groupKey.String(),
		TruncatedAlerts: numTruncated,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		return false, err
	}

	// 准备http请求, 并调用。
	req, err := http.NewRequest("POST", n.conf.URL.String(), &buf)
	if err != nil {
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgentHeader)

	resp, err := n.client.Do(req.WithContext(ctx))
	if err != nil {
		return true, err
	}
	notify.Drain(resp)

	// 检查是否需要重试
	return n.retrier.Check(resp.StatusCode, nil)
}
