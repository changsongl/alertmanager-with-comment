## Alertmanager With Comment

### 背景
因工作需求去了解Alertmanager的告警分组，静默，抑制的功能实现。
为了留下相应的内容沉淀，并且能够自己时长进行回顾和分享给大家。
因此创建此项目来进行代码备注，希望大家能够喜欢。

### 版本
release-0.21

### 目录结构
>[√] 代表已经完成备注。
>
>[×] 代表还未全部完成备注

````

├── api
├── asset
├── cli
├── client
├── cluster
├── cmd
├── config
├── dispatch
├── doc
├── docs
├── examples
├── inhibit
├── nflog
├── notify
├── pkg
├── provider       [√]      #提供告警的监听分发，存储和管理。
├── scripts
├── silence
├── store          [√]      #告警具体存储的实现方式，现在都是基于内存。
├── template
├── test
├── types
├── ui
└── vendor         [×]      #golang vendor目录。

````

