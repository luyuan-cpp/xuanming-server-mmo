# 公会:按 zone 隔离的客户端接入

**Created:** 2026-09-14
**状态:** 代码已落地,**未编译、未测试、未联调**(AGENTS.md §10.1,验证清单见 §6,由 Codex 执行)
**关联:** [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-2 / D-9 / D-10 / D-13、[client-rpc-router.md](./client-rpc-router.md)、[guild_ranking_architecture_zh.md](./guild_ranking_architecture_zh.md)、客户端 `mmorpg-client/Docs/GuildUI.md`

## 0. 一句话

客户端的帮会界面经 gate → client_rpc_router 直达 `go/guild`;帮会按 zone 隔离(只能在自己的区建帮、只看得见和加得进自己区的帮、只看本区榜),**不做全服帮会**。guild 进程仍是全局一份,zone 是数据维度不是部署维度。

## 1. 决策

| # | 决策 | 理由 |
|---|------|------|
| G1 | guild 仍是全局服务(D-2),隔离做在数据与逻辑层 | 表里本来就有 `zone_id` 列、分区榜与合服搬迁;按 zone 部署会让合服变成跨进程搬数据 |
| G2 | 客户端请求的 zone = data_service `player:zone:{id}` 归属映射(`GetPlayerHomeZone`),**不信请求体 `zone_id`** | 请求体是玩家自己选的;映射在建角时登记、合服时与公会行一起搬,是唯一真源。映射缺失 → `kGuildHomeZoneUnknown`(不猜);data_service 故障 → gRPC 错误,路由服回 `kServiceUnavailable` |
| G3 | 身份取 gate 注入的 `x-session-detail-bin`;带会话但解不开 → `Unauthenticated`;**无会话 = 内部调用**,沿用请求体字段 | D-9「身份从会话取先于客户端可达」。路由服对缺会话的客户端消息直接拒绝,客户端无法靠"不带会话"绕过 |
| G4 | 客户端来源只放行方法白名单(`internal/session.ClientMethods`),`UpdateGuildScore` 对客户端 `PermissionDenied` | `OptionIsClientProtocolService` 是服务级开关,内部方法会跟着进 gate 白名单与路由表;白名单默认拒绝,新 RPC 不登记就不会意外对外 |
| G5 | 可见性:`GetGuild` / `JoinGuild` 对别区帮会回 `kGuildNotFound`;`GetGuildRank` / `GetGuildRankByGuild` 强制本区,页长 ≤ 50;`GetPlayerGuild` 不过滤 | 别区与不存在同一答复,不泄露别区有哪些帮;自己的帮永远看得见 |
| G6 | 入会的 zone 校验在 `AddMemberInZone` 事务内 `FOR UPDATE` 判 | 缓存 `guild:v2:{id}` 的 zone_id 在合服刚搬迁时可能是旧值;与满员判定同一把行锁 |
| G7 | 帮名仍**全局唯一**(`uk_name`),重名 → `kGuildNameTaken` | 合服不必改名;`tools/merge_zone/guild_step.go` 的 `assertNoGuildNameCollision` 依赖这个前提 |
| G8 | 输入由服务端校验:帮名 trim 后 1–24 字、无控制字符 → 否则 `kGuildNameInvalid`;公告 ≤ 600 UTF-8 字节 → 否则 `kGuildAnnouncementTooLong` | gate 单包上限 1KB,500 个汉字约 1500 字节会在 gate 被丢弃、玩家只看到超时;客户端输入框同步改为 200 字 |
| G9 | 只承诺**路由服模式**可达(`GATE_CLIENT_RPC_ROUTER=1`);K8s 本轮不接 | 与 chat 同口径;K8s 上路由服部署链尚缺,接了也不可达 |

## 2. 请求流

```
客户端 GuildClient ── ClientRequest{message_id=15/19/27/29/35/38/39/52/60} ──▶ gate
gate:   会话校验 → ≤1KB → MessageLimiter(公会 9 个号已登记) → IsClientMessageId → ForwardRequest
路由服: route.Table[message_id] → GuildNodeService(全局随机实例)→ 原始字节 Invoke,透传 x-session-detail-bin
guild:  grpcstats → killswitch → session(解会话 + 方法白名单) → serverbase → logic
logic:  callerOf(ctx) 取会话 player_id → clientZone() 查 data_service 归属 zone → 按 zone 执行
```

## 3. 改动集

| 位置 | 改动 |
|---|---|
| `proto/guild/guild.proto` | `option (OptionIsClientProtocolService) = true` + import `proto_option.proto` |
| `data/tip/Tip.xlsx` | `//guild_error` 组追加 `GuildNameInvalid` / `GuildNameTaken` / `GuildAnnouncementTooLong` / `GuildHomeZoneUnknown`(均非 fault) |
| `data/MessageLimiter.xlsx` | 读 35/27/60/52 每秒 10 次;写 15/19/29/38/39 每秒 5 次 |
| `go/guild/internal/session/` | 新包:会话解码、方法白名单拦截器 |
| `go/guild/internal/logic/home_zone.go` | `HomeZoneLookup` + data_service 实现(1.5s 超时,NotFound / 老版 Unknown 文案判未映射) |
| `go/guild/internal/logic/guild_logic.go` | 各方法改走 `callerOf` / `clientZone`;帮名与公告校验;`NewGuildLogic` 加 `homeZones` 参数 |
| `go/guild/internal/data/guild_repo.go` | `ErrGuildNameTaken`(按 `uk_name` 索引名识别 1062)、`ErrGuildZoneMismatch`、`AddMemberInZone` |
| `go/guild/internal/svc/` | data_service 客户端与号段解耦(`initDataServiceClient`),有 `DataServiceRpc` 就拨号 |
| `go/guild/guild.go` | 拦截器链插入 session;接线 `DataServiceHomeZone` |
| `go/guild/etc/guild.yaml` | 删 go-zero `Etcd.Key`(D-13);Prometheus 9170 → 9220(原与 match 撞号) |
| `tools/scripts/go_services.ps1` / `start_game.ps1` | 登记并随一键启动拉起 guild(Tier 1,端口 50300) |
| `robot/guild_smoke_scenario.go` + `etc/guild_smoke.yaml` | `guild-smoke` 三机器人冒烟(§6 第 6 步) |
| 客户端 `GuildClient.cs` / `GuildWindow.cs` / `GuildUiTests.cs` / `Docs/GuildUI.md` | 新码文案、公告 200 字 / 600 字节、「本区帮会排行」、2 条用例 |
| 测试 | `session_test.go`、`home_zone_test.go`、`client_zone_test.go`(miniredis)、`guild_repo_zone_test.go`(`GUILD_TEST_MYSQL_DSN` 门控)、`guild_test.go` 拦截器链用例 |

## 4. 不变与兼容

- 无会话的内部调用(GM、运维工具、`tools/merge_zone`)行为与改前完全一致,既有 `merge_fence_test.go` 断言不变。
- 表结构、Redis 键、合服步骤零改动;帮名唯一性不变。
- 客户端线协议不变(消息号、请求 / 响应结构不变);客户端仍在请求里带 `PlayerId` / `ZoneId`,服务端忽略。
- 滚动顺序:先上 guild 新二进制(没有会话的旧流量不受影响)→ 再重生成并换 gate / 路由服(客户端开始可达)。反过来会在窗口内把没有会话校验的旧 guild 暴露给客户端。

## 5. 不做 / 已知缺口

- **成员名字**:项目尚无昵称(`proto/login/login.proto` 注释),成员列表继续显示编号。
- 审批入会、踢人、职位任免、转让帮主;捐献 / 活动 / 商店(客户端页面保持「暂未开放」)。
- 成员变动推送:客户端靠刷新重读。
- 合服闸门仍返回 `FailedPrecondition`(客户端看到通用失败),要定制文案需再加 `kGuildZoneMerging`。
- **存量玩家没有归属映射**会被 `kGuildHomeZoneUnknown` 拒绝:上线前按 zone 跑 `tools/merge_zone -backfill-home-zone -zone N`。
- K8s:`go_svc_image.ps1` / `k8s_deploy.ps1` / manifest 未登记 guild(G9)。

## 6. 验证清单(按序;任一步红即停)

1. **导表**:`dev.bat gen`(PATH 同时含 protoc 与 protoc-gen-go)。核对 `generated/code/proto/tip/guild_error_tip.proto` 出现 14009–14012 四个新码;`go/shared/generated` 同步;MessageLimiter 产物含 9 个公会消息号。
2. **proto-gen**:`pwsh tools/scripts/dev_tools.ps1 -Command proto-gen-run`。核对 `cpp/generated/rpc/service_metadata/rpc_event_registry.cpp` 的 `IsClientMessageId` 含 8/15/19/27/29/35/38/39/52/60;`go/client_rpc_router/generated/pb/game/route_table.go` 公会 10 条 `ClientProtocol: true`;`kMaxRpcMethodCount` 未变;按惯例恢复 `scene_node_service.cpp` 的 Agones 块。
3. **Go**:`go/guild` 下 `go build ./... && go test ./...`;集成用例另设 `GUILD_TEST_MYSQL_DSN` 指向**一次性测试库**(用例会 DROP 重建 guild 两张表,绝不能指向 `mmorpg` 开发库),必须连跑 `rank_zone_integration_test.go` 与 `merge_fence_test.go`(D-10)。`go/client_rpc_router` build + test。`robot` 下 `go mod vendor`(vendor 目前没有 `proto/guild`)后 build + vet。重编 `bin/go_services/guild.exe`(`start_game.ps1` 缺 exe 会在第 1 步拒启)。
4. **C++**:MSBuild 串行 `/m:1`:proto → rpc → gate(`IsClientMessageId` 在生成的 rpc 库里)。
5. **客户端**:`tools/gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo`(生成新错误码枚举,否则 `GuildClient.cs` 编译不过)→ `tools/client_compile_check.ps1` → Unity EditMode `-testFilter MmorpgClient.Tests.EditMode.Tianyong.Guild`(原 28 条 + 新 2 条)。
6. **冒烟**:gate 路由服模式重启(MessageLimiter 生效),起 client_rpc_router / data_service / guild;`robot` 目录 `.\robot.exe -c etc/guild_smoke.yaml` → `GUILD_SMOKE_OK`。只有单 zone 时把 `cross_zone` 改 false。
7. **手测**:游戏内按 G 打开帮会 → 创建 → 另一个同区角色在排行里加入 → 帮主改公告 → 成员刷新可见;别区角色的排行里看不到该帮。
