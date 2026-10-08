# go/ —— Go 微服务

基于 [go-zero](https://go-zero.dev/) 的 gRPC 服务。这里不是一个大工程,而是**一个服务一个 Go
module**,各自有 `go.mod`,通过 `replace` 引用同目录下的公共库。

## 服务

| 目录 | 端口 | 职责 | 存储与中间件 |
|------|------|------|--------------|
| `login/` | 53000 | 登录、建角、进入游戏、分配 gate、登录排队 | Redis、Kafka |
| `player_locator/` | 53200 | 会话与位置的权威、断线租约 | Redis、Kafka |
| `scene_manager/` | 60300 | 场景分配、负载均衡、世界分线 | Redis、Kafka |
| `data_service/` | 9000 | 玩家数据、归属区映射、ID 号段、快照与流水 | MySQL、Redis、Kafka |
| `db/` | 6000 | 消费 Kafka 的落库任务,写区库 | MySQL、Redis、Kafka |
| `client_rpc_router/` | 50600 | gate 唯一的 gRPC 目标,按路由表转发原始字节 | 无 |
| `match/` | 50500 | 匹配与评分;组队服务也注册在这个进程里 | Redis、Kafka |
| `chat/` | 50700 | 世界频道与私聊 | Redis |
| `guild/` | 50300 | 帮会 | MySQL、Redis、Kafka |
| `friend/` | 50400 | 好友、黑名单、推荐 | MySQL、Redis、Kafka |
| `trade/` | 50800 | 玩家寄售交易 | MySQL、Redis |

所有服务都注册到 etcd。端口是各自 `etc/*.yaml` 里的本地默认值。

## 公共库

| 目录 | 内容 |
|------|------|
| `shared/` | 各服务共用的包:雪花 ID、号段、节点注册、Kafka 寻址、gRPC 拦截器、选主、热关停等 |
| `schemamigrate/` | 以 proto 为源的建表与迁移库,guild、friend、trade 使用 |
| `proto/` | 全部 `.pb.go`,由协议生成器产出,勿手改 |

`battle/` 与 `team/` 下只有生成的消息号常量,不是服务:战斗是 C++ 节点(`cpp/nodes/battle`),
组队的实现在 `match/internal/team`。

## 一个服务的结构

```
go/<service>/
├── <service>.go         入口:读配置、注册 etcd、启动 gRPC 服务
├── etc/<service>.yaml   本地默认配置
├── internal/
│   ├── config/          配置结构
│   ├── server/          gRPC 服务端适配
│   ├── logic/           业务逻辑
│   ├── svc/             ServiceContext:依赖的装配点
│   └── ...              该服务自己的子包(kafka、data、metrics……)
├── cmd/                 随服务附带的命令行工具(迁移、运维修复),不是每个服务都有
├── generated/           该服务用到的消息号常量(生成)
└── <service>_test.go    服务级测试
```

## 构建、运行、测试

```powershell
# 编译全部服务到 bin/go_services/
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build

# 启动 / 查看 / 停止
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-status
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-stop

# 测试:逐个 module
cd go/login && go test ./...
```

区库的表结构用 `go/db/cmd/migrate` 管理(`-command status | plan | up`);guild、friend、trade
的独占库在服务启动时按 proto 自动迁移,也可以用 `-migrate` 参数只迁移不启动。

`build.ps1` 不编译:它重新生成 db 与 login 的 goctl 包装代码并修正 import。`.pb.go` 的重新生成走
`dev.bat proto`。

编码约定与排查入口见 [AGENTS.md](AGENTS.md)。
