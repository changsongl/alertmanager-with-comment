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
>[×] 代表还未全部完成备注。
>
>[-] 代表无需备注，自行阅读。

````

├── api            [x]      #Alertmanager接口逻辑。
├── asset          [x]      #生成Alertmanager静态文件。
├── cli            [x]      #Alertmanager的cli逻辑。
├── client         [x]      #Alertmanager客户端。
├── cluster        [x]      #集群逻辑。
├── cmd            [√]      #程序入口。
├── config         [x]      #配置目录，进行配置文件加载和解析。
├── dispatch       [x]      #程序主线，进行告警/恢复事件调度。
├── doc            [-]      #文档和架构图。
├── docs           [-]      #Alertmanager文档描述，包含整个架构，配置文档等等。
├── examples       [√]      #展示Alertmanager HA 的例子，里面包含多个am的配置和发告警脚本，和receiver服务。
├── inhibit        [√]      #提供抑制规则的检查和匹配。
├── nflog          [x]     
├── notify         [x]      #通知包，里面包含通知抽象类，和各个实现类如webhook，微信等等。
├── pkg            [x]      #公共包，labels和modtimevfs。
├── provider       [√]      #提供告警的监听分发，存储和管理。
├── scripts        [-]      #protobuf脚本。
├── silence        [√]      #提供静默的核心储存和匹配，会将静默状态写到文件里，和同步给其他集群的节点等等。
├── store          [√]      #告警具体存储的实现方式，现在都是基于内存。
├── template       [√]      #解析配置文件的包，原理很简单。通过golang的template来实现，并且自己封装了一些方便使用的类。
├── test           [-]      #测试Alertmanager的代码。
├── types          [√]      #告警匹配器（用于静默），还有Marker标识告警状态，Muter静默和抑制的行为接口等等。
├── ui             [√]      #Alertmanager UI页面。
└── vendor         [-]      #golang vendor目录。

````

