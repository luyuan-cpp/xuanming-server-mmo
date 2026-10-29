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

## 网关与选区

当前 Gate 分配只走 HTTP，配置要求 `gateway_addr` 非空，不再有 gRPC 分配或 `gate_mode` 分支。

```yaml
gateway_addr: "http://127.0.0.1:8081"
zone_id: 0
```

显式配置非零 `zone_id` 时直接向该区分配，不请求 server-list；配置为 0 时先以 5 秒超时读取
`GET /api/server-list`，选择响应中第一个推荐区，否则取第一项。请求、读取、解码失败或列表为空时，
本次分配回退为传 0，由服务端继续决定；这不保证一定获得 Gate。当前自动选择不筛区服状态、不按负载排序，
也不单独检查 server-list 的 HTTP 状态，不能解释成“选择首个可进入区服”。默认样例实际为 `zone_id: 1`。

通用登录与选区入口是 `main.go` 的 `resolveGateAddrLocal` / `resolveGateViaHTTPLocal`；选区只保存在本次局部变量，
不回写 `cfg.ZoneID`。跨区或指定区验收应显式配置区号。`gate.go` 旧函数链没有当前外部调用者，不能把两份实现视为等价入口；
本次只整理文档，不清理该代码。旧 `deploy/k8s/robot_stress.ps1` 模板仍含 gRPC／空 Gateway 配置，不能直接当作当前可运行配置。

`asset-arrival-prepare` 使用独立分配入口并要求显式非零区号，不经过这条自动选区链。

分配后的持票查询与恢复由 `AssignGateWithQueue` 处理，契约见[开服削峰设计 §11.5](../docs/design/open-server-rate-limit-design.md)。

## 模式

| `mode` | 做什么 |
|--------|--------|
| `stress`(默认) | 大量机器人并发登录、进场景、按 AI 策略行动 |
| `login-test` | 登录场景测试集:重连、顶号、超时等,跑完即退出 |
| `data-stress` | 反复登录、游玩、下线,配合 `go/db` 的校验器验证数据一致性 |
| `battle-smoke` | 两个机器人匹配并打完一场回合制战斗 |
| `attribute-smoke`、`pet-smoke` | 属性加点、宠物 |
| `equip-smoke`、`equip-forge-smoke` | 装备随机属性与穿脱;装备强化、宝石镶嵌与取下(两条冒烟各用各的账号,强化不可逆) |
| `chat-smoke`、`friend-smoke`、`guild-smoke`、`team-smoke`、`trade-smoke` | 聊天、好友、帮会、组队、交易 |
| `mail-smoke` | 邮件:直连 MailAdmin 发系统 / 定向 / 带附件三封,两区读信、删信、领取未开放与 S2C 方法不可调(`etc/mail_smoke.yaml`;mail 与 robot 要拿到同一个 `MMORPG_MAIL_ADMIN_TOKEN`,取法见该文件头) |
| `rank-smoke` | 排行榜:两区各一人跨区打一局 1V1,轮询到写榜后核分数方向、名次、跨区名字补全与未上榜的 self;可选核 gate 限流(`etc/rank_smoke.yaml`;前提是本机 match 打开评分快照发布,见该文件头) |
| `team-backfill-smoke` | 队伍补位匹配:2 人队选「凑满 3 人」+ 1 个路人单排,验证队员取消 / 名单变化整队退出、补位成组同场开战(`etc/team_backfill_smoke.yaml`) |
| `team-follow-smoke` | 组队跟随:入队即被带到队长身边、可见性重建、双向移动广播;`client_follower: true` 时由机器人当队长、真实客户端当队员,验走路跟随 |
| `travel-smoke` | 跨区场景传送往返 |
| `channel-smoke` | 玩家主动切线:列出分线、切到同图另一条线、冷却与拒绝(地图只有 1 条线时打印 SKIP,不算失败) |
| `sync-smoke` | 落地恢复的按域版本摘要:登录取摘要、好友申请只让对应域 +1、短重连后摘要原样延续、实体就绪前发请求不致崩溃(跨区落地腿可选,缺省关) |
| `merchant-smoke` | 神秘商人:查到他在哪条线、切过去、走到旁边、购买、限购与各种拒绝(商人不在场时打印 SKIP,不算失败) |
| `mall-arrival-smoke` | 官方商品到账(D-17):下单(同号重发幂等)→ 错金额付款被商城拒 → dev 模拟渠道付款 → 等订单已发货 → 背包正好多出应得数量、不重复发、重登仍在(`etc/mall_arrival_smoke.yaml`;要先起 java/mall_game 并跑 `etc/mall_arrival_smoke_seed.sql`,见该文件头) |
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
