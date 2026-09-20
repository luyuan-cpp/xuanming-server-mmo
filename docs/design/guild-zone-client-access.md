# 公会:按 zone 隔离的客户端接入

**Created:** 2026-09-14
**状态:** 2026-09-14 Codex 已完成导表、协议生成、编译、单元/真实存储集成、客户端 EditMode 与双区机器人冒烟;界面手测、独立客户端打包及 K8s 未验。实测结果见 §7。
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
| `go/guild/internal/data/guild_repo.go` | `ErrGuildNameTaken`(按 `uk_name` 索引名识别 1062)、`ErrGuildZoneMismatch`、`AddMemberInZone`;排行榜分页先用 `uint64` 计算并判空,再转换 Redis 索引,避免大页码溢出 |
| `go/guild/internal/svc/` | data_service 客户端与号段解耦(`initDataServiceClient`),有 `DataServiceRpc` 就拨号 |
| `go/guild/guild.go` | 拦截器链插入 session;接线 `DataServiceHomeZone` |
| `go/guild/internal/node/node.go` | 注册 NodeInfo 补 `grpcEndpoint`,使路由服能发现并拨通 guild;真实 etcd 回归验证发布值 |
| `go/guild/etc/guild.yaml` | 删 go-zero `Etcd.Key`(D-13);Prometheus 9170 → 9220(原与 match 撞号) |
| `tools/scripts/go_services.ps1` / `start_game.ps1` | 登记并随一键启动拉起 guild(Tier 1,端口 50300) |
| `robot/guild_smoke_scenario.go` + `etc/guild_smoke.yaml` | `guild-smoke` 三机器人冒烟(§6 第 6 步) |
| 客户端 `GuildClient.cs` / `GuildWindow.cs` / `GuildUiTests.cs` / `Docs/GuildUI.md` | 新码文案、公告 200 字 / 600 字节、「本区帮会排行」、2 条用例 |
| 测试 | `session_test.go`、`home_zone_test.go`、`client_zone_test.go`(miniredis)、`guild_repo_zone_test.go`(`GUILD_TEST_MYSQL_DSN` 门控)、`guild_test.go` 拦截器链用例、`rank_page_test.go` 全局/分区榜 16 个分页边界子例 |

## 4. 不变与兼容

- 无会话的内部调用(GM、运维工具、`tools/merge_zone`)对正常有效参数保持兼容;极大页码现在正确返回空页,既有 `merge_fence_test.go` 断言不变。
- 表结构、Redis 键、合服步骤零改动;帮名唯一性不变。
- 客户端线协议不变(消息号、请求 / 响应结构不变);客户端仍在请求里带 `PlayerId` / `ZoneId`,服务端忽略。
- 滚动顺序:先上 guild 新二进制(没有会话的旧流量不受影响)→ 再重生成并换 gate / 路由服(客户端开始可达)。反过来会在窗口内把没有会话校验的旧 guild 暴露给客户端。

## 5. 不做 / 已知缺口

> **2026-09-19 更新**:本节写于一期(2026-09-14)。帮会二期 B2s/B2c 已经把下面四条做掉了,
> 保留原文是为了不改写历史,但**不要再照它判断现状**;二期的实现见
> [docs/design/guild-phase2/02-management.md](guild-phase2/02-management.md)。

- ~~审批入会、踢人、职位任免、转让帮主~~ → **B2s/B2c 已落码**(申请制 `ApplyJoinGuild` /
  `CancelGuildApplication` / 两种列表 / `ReviewGuildApplication`,管理三件 `SetGuildMemberRole` /
  `KickGuildMember` / `TransferGuildLeader`)。捐献 / 活动 / 商店仍未开放(B5 / B6)。
- ~~成员变动推送:客户端靠刷新重读~~ → **B2s 已落 `NotifyGuildChanged`**(下行推送,13 种变更类型)。
- ~~合服闸门仍返回 `FailedPrecondition`~~ → **B2s 已改回 tip `kGuildZoneMerging`**;
  本服务的纪律是写冲突与闸门一律回业务 tip,不回 gRPC 错误(gRPC 错误会把客户端推进重连隔离)。
- **成员名字**:协议字段 `GuildMember.name` 已由 B2s 加好,但**服务端要到 B3b 才填值**;
  在那之前成员列表仍显示编号 +「道友 · id」兜底。名字注册表本身是 B3a-1。
- **存量玩家没有归属映射**会被 `kGuildHomeZoneUnknown` 拒绝:上线前按 zone 跑 `tools/merge_zone -backfill-home-zone -zone N`。
- K8s:`go_svc_image.ps1` / `k8s_deploy.ps1` / manifest 未登记 guild(G9)。

## 6. 验证清单(按序;失败保留日志,定位并修复后再执行)

1. **导表**:PATH 含 `protoc` 与 `protoc-gen-go`,运行 `python tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml`。`dev.bat gen` 已包含导表与 proto 生成,若用它不必重复执行下一项。核对 `generated/code/proto/tip/guild_error_tip.proto` 中 14009–14012 四码、`go/shared/generated` 同步、MessageLimiter 中 9 个公会消息号。
2. **proto-gen**:先确保生成器与源码一致,本轮在 `tools/proto_generator/protogen` 执行 `go build -o proto-gen.exe ./cmd`,再同步 `pbgen.exe`;从仓库根运行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml`。核对 gate 的 `IsClientMessageId` 含 8/15/19/27/29/35/38/39/52/60,路由表 10 个 Guild RPC 均为 `ClientProtocol: true`。本轮生成时 `kMaxRpcMethodCount=196` 未变;只在生成确实改坏 `scene_node_service.cpp` 的 Agones 块时恢复该块,不要覆盖其他会话改动。
3. **Go**:使用 Go 1.24.5 或更高版本(本轮缓存的 Go 1.26.5)。`go/guild` 下构建 `go build -o ../../bin/go_services/guild.exe .`,运行 `go vet ./...`。数据库测试须分别配置:
   - `GUILD_TEST_MYSQL_DSN`:指向一次性库,普通 `go test -count=1 -v ./...` 中的用例会 DROP 重建该库的 guild 两张表,绝不能指向开发或正式业务库。
   - `GUILD_IT_MYSQL_DSN`:带建库权限的测试连接;`go test -tags=integration -count=1 -v ./internal/data ./internal/logic` 才执行 `rank_zone_integration_test.go`,它自行创建并删除 `guild_it_<pid>_<序号>` 临时库。此命令也覆盖 `merge_fence_test.go`。检查没有 SKIP,不能把未连接数据库的绿灯算作集成通过。
   - 注册变更另跑真实 etcd:`go test -tags=integration -count=1 -v ./internal/node`,确认新注册值含 `grpcEndpoint` 且无 SKIP。
   - `go/client_rpc_router`:执行 `go build -o ../../bin/go_services/client_rpc_router.exe .`、`go test -count=1 ./...`、`go vet ./...`;`robot`:先 `go mod vendor` 同步 `proto/guild`,再 `go build -mod=vendor -o robot.exe .` 与 `go vet -mod=vendor ./...`。
4. **C++**:VS MSBuild `Debug x64 /m:1`,按依赖串行构建 proto → table → core → rpc → gate-lib → gate,部署新 `bin/gate.exe`。gate 白名单实现在生成的 rpc 库里,只编 gate 业务文件不足以更新它。
5. **客户端**:`tools/gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo` → `tools/client_compile_check.ps1` → Unity EditMode `-testFilter MmorpgClient.Tests.EditMode.Tianyong.Guild`(30 条)。若正式 Unity 正在运行,在隔离副本上跑批处理,保留 XML 并核对相关源码与生成文件哈希。
6. **双区冒烟**:预检 `robot_9201/9202/9203` 未被其他任务使用,并确认它们的角色归属分别是 1/1/2;首次创建会自动登记归属。gateway、两区的 db/login/player_locator/gate/scene 登录进场链需就绪,全局共享 client_rpc_router/data_service/scene_manager/guild。每区存档 schema 必须跟上当前 proto。两个 gate 都使用 `GATE_CLIENT_RPC_ROUTER=1` 并重启加载 MessageLimiter;router 模式 gate 不监听原 gRPC 端口,健康检查使用客户端 TCP 端口。核对 etcd 的 Guild NodeInfo 同时包含可拨通的 `endpoint` 和 `grpcEndpoint`,router 已发现该实例。从 `robot/` 执行 `.\robot.exe -c etc/guild_smoke.yaml`,验收必须同时满足:
   - 进程退出 0 且有 `GUILD_SMOKE_OK`;跨区隔离、全服重名、公告限制、伪造 PlayerId 均通过。
   - step 9 收到 `kServiceUnavailable`,随后的查榜成功且积分仍为 0;超时、任意错误或业务成功回包不能算通过。
   - 同一运行窗口的 router 日志明确记录 `/guildpb.GuildService/UpdateGuildScore` 的 `code=PermissionDenied`,并结合 `TestClientCannotCallInternalMethod` / `TestSessionGateWiredIntoUnaryChain` 确认白名单生效。信封不保留原始 gRPC code,仅靠机器人拒绝信封不能证明拒绝原因。
   - `cross_zone: false` 仅验同区子集,不能替代双区验收。结束检查帮会/成员及榜单清理结果,保留测试账号,不清空共享 Redis/数据库。
7. **手测**:游戏内按 G → 创建 → 另一个同区角色在排行加入 → 帮主改公告 → 成员刷新可见;别区排行看不到该帮。自动测试不替代界面手测或独立客户端打包。

启动器当前契约已由并行修订更新:`start_game.ps1` 默认路由服模式,guild/chat 都是可选 exe;缺失只告警跳过。因此 `-CheckOnly` 通过不代表帮会服务存在,帮会验收仍需确认 guild 二进制、进程和注册均正常。K8s 路由模式及 guild 部署不在本轮验收范围。

## 7. 2026-09-14 Codex 验证结果

日志统一保存在 `run/logs/guild-verify-20260914/`,机器可读摘要为 `verification-summary.json`。本轮使用已有 Go 1.26.5、protoc 35.1、VS MSBuild 与 Unity 6000.6.0f1。

| 验证项 | 实际结果 | 证据文件 |
|---|---|---|
| 导表 / proto | 29 张表;部署 12 OK / 0 failed;生成器重编后生成成功;guild 四码与 9 条限流记录、10 条客户端路由均核对 | `export.log`, `protogen-build.log`, `protogen.log` |
| guild 普通测试 | 54 个顶层测试通过,0 失败/跳过;包含已连接一次性 MySQL 的区服数据用例 | `guild-tests.log` |
| guild 数据/逻辑集成 | 33 个顶层测试通过,0 失败/跳过;实际 MySQL 与 merge fence 均覆盖 | `guild-integration.log` |
| guild 节点集成 | 真实 etcd 7/7,0 失败/跳过;新注册字段回归先红后绿 | `guild-grpc-registration-red.log`, `guild-node-integration-final.log` |
| Go 构建与静态检查 | guild/router/robot 构建、对应 test/vet 成功;guild 注册修复已重编部署 | `guild-build.log`, `guild-registration-fixed-build.log`, `guild-vet.log`, `guild-node-vet-final.log`, `router-*.log`, `robot-*.log` |
| C++ | proto/table/core/rpc/gate-lib/gate 串行 Debug x64 成功,0 编译错误/警告;新 gate 已用于两区 | `cpp-summary.json`, `cpp-gate-artifact.json`, `cpp-*.log` |
| 客户端 | 协议生成成功;离线检查 283 源文件、0 错误;隔离 Unity Guild EditMode 30/30,0 失败/跳过;六个相关文件哈希与正式源码一致 | `client-proto.log`, `client-compile.log`, `client-guild-editmode-results.xml`, `client-final-check.json` |
| 双区机器人 | 09:46:32–09:46:38 EDT,退出 0,`GUILD_SMOKE_OK guild_id=101 zone_a=1 player_a=602 player_b=601 player_c=701`,`cross_zone=true` | `guild-smoke.log`, `guild-smoke.exitcode` |
| 白名单证据 | 09:46:37 同一请求在 guild session 被拒,router 明确 `code=PermissionDenied method=/guildpb.GuildService/UpdateGuildScore`;机器人后验积分仍为 0 | `guild-smoke-security-evidence.log` |
| 收尾数据 | 测试帮会已由场景解散;`guild` / `guild_member`、全局榜、1/2 区榜均为 0;测试账号保留 | `guild-smoke-postcheck.log` |

本轮修复三处运行/测试问题:排行榜页起点的整数溢出;guild 注册遗漏 `grpcEndpoint` 导致路由服忽略实例;配置测试仍期待旧 metrics 9170(实际为 9220)。同时收紧冒烟第 9 步,不能用超时或任意错误宣称内部方法已拒绝;补清 DataServiceRpc 注释,明确 home-zone lookup 与发号开关独立。新启用的旧 etcd 类型复用测试原来让两个未定义枚举共用 `.rpc` 前缀,现只改夹具为各类型独立随机前缀,生产 allocator 行为未扩大修改。

两次完整冒烟失败均在修复后才重跑,原日志保留:

- 首轮二区进场超时:zone_2_db 旧 `player_database` 缺 `pet_component` / `bag_component` / `mission_component`。执行 `go/db` 官方迁移器,设置 `DB_ALLOWED_DATABASES=zone_2_db`,先 plan 再 up,默认增量补列;账本两项 dirty=0,三列均核对为 MEDIUMBLOB。`user_oauth.provider_id` 与 `user_phone.phone` 的既有 VARCHAR(191) 类型差异保留,未传 `-allow-modify`;工具因此报告 NEEDS-REVIEW / exit status 4,不能将本次描述为完整 schema 无漂移。见 `zone2-schema-{plan,up,verified}.log`、`guild-smoke-before-zone2-schema.log`。
- 第二轮首次帮会查询失败:etcd 只有 endpoint,router 报缺 grpcEndpoint。源码修复、真实 etcd 回归和构建通过后仅重启 guild;新实例成功被现有 router watch 发现。见 `guild-registration-{before,after}-fix.log`、`guild-smoke-before-registration-fix.log`。

启动联调还重编了与当前生成表版本不一致的 scene_manager/player_locator/login/match。二区 db 首次创建 Kafka topic 时碰到“CreateTopics 返回后立即 ListTopics 尚不可见”的既有竞态;确认 broker 已完成创建且 leader/ISR 正常后才重启成功,没有删除 topic。该通用 Kafka 初始化竞态未在本轮修改。双区启动使用保存在日志目录的 `start-guild-verification.ps1`,复用已有服务与并行 chat,无共享数据库/Redis 清空。

边界:存量角色归属映射回填未执行,本轮只使用预检为空的新测试账号;C++ 脚本因本机缺 `no_raw_ptr_check.exe` / `clang-query` 自动跳过了该附加检查,没有手动关闭;未做游戏内界面手测、独立客户端重新打包、负载测试或 K8s 部署。并行会话正在实现 trade 等能力,本条仅代表上述帮会文件与产物的验证,不代表随后新增协议/表也已生成与验证。本任务未提交或推送代码。
