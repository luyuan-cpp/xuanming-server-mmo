# robot/ —— 压测与冒烟测试客户端

用 Go 写的无界面游戏客户端。它走和真实客户端完全相同的路径(HTTP 网关 → gate → 各服务),
用来做两件事:**压测**(成千上万个机器人同时在线)和**端到端冒烟**(跑一遍某个功能的完整流程,
对结果做断言)。

## 运行

```powershell
dev.bat robot                              # 编译并用默认配置 etc/robot.yaml 运行
dev.bat robot robot\etc\chat_smoke.yaml    # 指定配置

cd robot && go run . -c etc/robot.yaml     # 直接运行
```

唯一的命令行参数是 `-c <配置文件>`,跑什么由配置里的 `mode` 决定。

## 模式

| `mode` | 做什么 |
|--------|--------|
| `stress`(默认) | 大量机器人并发登录、进场景、按 AI 策略行动 |
| `login-test` | 登录场景测试集:重连、顶号、超时等,跑完即退出 |
| `data-stress` | 反复登录、游玩、下线,配合 `go/db` 的校验器验证数据一致性 |
| `battle-smoke` | 两个机器人匹配并打完一场回合制战斗 |
| `attribute-smoke`、`pet-smoke` | 属性加点、宠物 |
| `chat-smoke`、`friend-smoke`、`guild-smoke`、`team-smoke`、`trade-smoke` | 聊天、好友、帮会、组队、交易 |
| `travel-smoke` | 跨区场景传送往返 |
| `currency-crash-snapshot` | 货币崩溃窗口验证用的单次登录快照 |

每种模式在 `etc/` 下都有对应的配置样例。

## 目录

```
robot/
├── main.go                 入口:读配置,按 mode 分发
├── *_scenario.go           各冒烟模式的流程与断言
├── login*.go、http_*.go    登录与网关 HTTP 调用
├── config/                 配置结构
├── etc/                    各模式的配置样例
├── pkg/                    GameClient:TCP 连接、编解码、跨区重定向
├── logic/
│   ├── ai/                 行为策略
│   ├── gameobject/         机器人侧的玩家状态
│   └── handler/            服务端消息的处理桩(生成)
├── metrics/                压测统计
├── generated/              消息号常量(生成)
└── vendor/                 依赖的本地副本
```

带 `vendor/` 是因为 `go.mod` 把协议与公共库 `replace` 到了 `../go/proto`、`../go/shared`,
以 `robot/` 为上下文构建镜像时拿不到这两个目录,所以用 `-mod=vendor` 构建。改了 `go/proto` 或
`go/shared` 之后要在这里重跑 `go mod vendor`。

## 压测

压测有固定的前后流程与复盘口径,见根目录 [AGENTS.md](../AGENTS.md) 的"压测纪律"一节;
历次复盘在 [docs/stress/](../docs/stress/)。
