# friend 服务移植 —— 交接文档(2026-09-20)

> **读者**:接手这批代码的工程师(零上下文)。本文写完就是为了让你直接开工。
> **本文所有结论都来自读 `E:/work/xuanming-server-mmo` 分支 `main` 的源码**(首轮核实快照:2026-09-19,当时 HEAD `a5ca66851`;**复核于 2026-09-20**,其间 `main` 已前进到 `5524daf0c` → `4a04d0819`,但 `git log a5ca66851..HEAD -- go/friend proto/friend` **零提交** —— friend 这批代码本身没被别的会话动过)。
> **行号一律只作辅助坐标,几乎肯定已漂移** —— 本仓同时有多个会话在提交。
> 定位请一律用 `grep -n "<符号名>" <文件>`,**不要按行号跳**。

---

## ⚠ 先读这一段(最重要的事)

**friend 服务已全量移植并合入 `main`,但整批代码从未编译、从未运行、从未跑过任何测试;`proto-gen` 与导表器一次都没跑过。**

已进 `origin/main` 的四个提交(逐个 `git merge-base --is-ancestor` 核实过):

| 提交 | 说明 |
|---|---|
| `f06090b19` | friend 移植:A 仓好友服务全量落地(F1 协议+骨架 / F2 事务+能力 / F3 登记+部署+冒烟) |
| `cda218956` | 合并 main 到 friend 移植分支(提交标题自述解 37 处冲突,788 files changed) |
| `7b48b0a98` | friend 移植合并后收尾:三个新 tip 码进 `data/tip/Tip.xlsx` + 更正被合并作废的两处文档说法 |
| `57af3fd09` | merge_zone:friend 库名校验改用公共 `schemaNamePattern`(修合并引入的悬空符号) |

**而且现在的 `main` 上 `go/friend` 一定编译不过** —— 不是"可能有问题",是生成物没跟上源。三类悬空引用已逐个定位:

| 代码里引用的符号 | 现在的生成物里有没有 |
|---|---|
| `friendpb.RegisterClientPlayerFriendServer` / `UnimplementedClientPlayerFriendServer` / `ClientPlayerFriendServer`(引用点:`go/friend/friend.go`、`go/friend/internal/server/friend_server.go` 的 `NewFriendServer`) | **没有**。`go/proto/friend/friend_grpc.pb.go` 里仍是旧的 `FriendService_*`,只有 8 个方法,还带着已删的 `NotifyOnline` / `NotifyOffline` |
| `table.FriendError_kFriendBlocked` / `_kFriendBlockListFull` / `_kFriendTargetInboxFull`(引用点:`go/friend/internal/constants/constants.go` 的 `ErrBlocked` / `ErrBlockListFull` / `ErrTargetInboxFull`;`robot/friend_smoke_scenario.go` 的 `tipFriendBlocked`) | **没有**。`go/shared/generated/pb/table/friend_error_tip.pb.go` 只到 `FriendError_kFriendTooManyPending = 15006`;`robot/vendor/shared/generated/pb/table/friend_error_tip.pb.go` 同样 |
| `game.ClientPlayerFriend*MessageId`(引用点:`go/friend/internal/logic/push.go` 的发号处;`robot/friend_smoke_scenario.go` 多处) | **没有**。各 `generated/pb/game/message_id.go` 里仍是 `FriendServiceAddFriendMessageId = 11` 这一套旧名 |

另外 `go/client_rpc_router/generated/pb/game/route_table.go` 里 friend 的路由项还是 `/friendpb.FriendService/*` 且 `ClientProtocol: false`,说明路由表也没重生成过;`robot/vendor/proto/` 下**没有 `friend` 包**;`bin/go_services/` 下**没有 `friend.exe`**。

**所以接手第一个动作不是 `go build`,而是 §2 的第 1–3 步(装 Go → 导表器 → proto-gen)。**

---

## §1 现在的状态

friend 服务从 A 仓(Pandora)`services/social/friend` 移植而来,因超过 `AGENTS.md §10.2` 的 30 文件门禁,拆成 F1 / F2 / F3 三批。设计文档是 `docs/design/friend-port-20260918.md`(背景与裁定 / 决策 F1–F17 / 改动集 / 不做项 / 已知缺口 / 验证清单 / 发布顺序),持久化形态在 `docs/design/friend-persistence-architecture.md`。

### F1 —— 让协议可达、让表有唯一事实源、让服务有骨架(20 文件)

解决的是"这个服务在 B 仓的体系里要长什么样"。三件事:

1. **协议改名 `FriendService` → `ClientPlayerFriend`,并标 `option (OptionIsClientProtocolService) = true`。** 这不是洁癖:Unity / robot 的下行 handler 生成器只认服务名含 `ClientPlayer` / `GamePlayer` 的服务(判据在 `tools/proto_generator/protogen/internal/generator/unity/unity_client_handler.go` 的 `isRelevantService` 与 `.../generator/go/robot_case.go` 的同名函数,两处**判据**逐字相同 —— `strings.Contains(svc, "GamePlayer") || strings.Contains(svc, "ClientPlayer")`;函数体不完全一样,robot 那份多一段 debug 日志),**本期新增的 S2C 推送没有 handler 就等于没接**。同时删掉 `NotifyOnline` / `NotifyOffline`(全仓非生成代码零调用方,`friend:online` 这把键从来没有写者),六个请求体删 `player_id` 改 `reserved`(D-9:身份只从会话取)。
2. **新建 `proto/friend/friend_table.proto`,作为四张表的唯一事实源**(`friend` / `friend_request` / `friend_capacity` / `friend_block`),列名与当时的 `deploy/mysql-init/guild_friend_tables.sql` 逐字对齐(⚠ **该文件现在已不在 main 上** —— 帮会二期 B1 与本批各搬走一半后,它在 `cda218956` 那次合并里被整个删掉;要看原文用 `git show f06090b19:deploy/mysql-init/guild_friend_tables.sql`),整数主键、零 UNIQUE KEY(D-14 §2 约束)。
3. **骨架按 `go/trade/trade.go` 逐段移植**:`go/friend/friend.go` 的 `-migrate` / `-allow-modify` 两个 flag 与 D-14 退出码、`ensureSchema` 的 Up/Plan 两态、`lifecycle` 24s 硬截止、先起服再注册、`advertisedHost` 按 POD_IP 回落;删私有 node 包改用 `shared/noderegistry`(`NodeInfo` 的 `Endpoint` 与 `GrpcEndpoint` **双填** —— 只填前者路由服拨不通,guild 踩过);新增 `internal/session`(10 个 C2S 方法白名单,`NotifyFriendEvent` 刻意不收)、`internal/lifecycle`、`internal/metrics`、`internal/data/tables.go`。配置 `go/friend/etc/friend.yaml` 按契约锚点重写(`Timeout` 4000、`Etcd.Key` 显式空串、`LeaseTTL` 60、顶层 `MetricsListenAddr: ":9180"`、结构化 MySQL 字段且 `DBName: mmorpg_friend`)。

F1 还**推翻了一条书面决策**:`friend_capacity_backfill_v1` 就绪门禁**退役**(记在 `docs/design/xuanming-port-decisions-20260910.md` 的「D-10 修订(2026-09-18)」)。理由是它查的 `guild_schema_migration` 按 D-14 留在旧库 `mmorpg`,而 friend 的 `config.Validate` 断言只连 `mmorpg_friend` —— 这条查询在任何合法配置下都不可能命中。**退役的只有这道闸,D-10 的实质不变量原样保留**(见 §5.6)。

### F2 —— 事务重写(RC + 全局锁序)+ 四块新能力(19 文件)

解决的是"并发下不炸、A 仓有的能力 B 仓也有"。

开工前先解掉了一个会在生产随机炸的死锁环:F1 交付的 `AcceptFriend` 把申请行的主键锁排在容量守卫**之前**,而 F2 要把 `AddFriend` 改成先锁双方容量行的权威事务 —— 同一对玩家"一边接受、一边重发申请"就能撞 InnoDB 1213。裁定采用 A 仓 2026-08-11 在真 MySQL 8.4 上压出的纪律:**任何写事务在拿到容量守卫之前,不得做任何锁定读**,把 `AcceptFriend` 的申请行 `FOR UPDATE` 下移到守卫之后。

能力面:`AddFriend` 改成八步权威事务(守卫 → 双向拉黑 → 双向好友边 → 申请行 → 出站/入站 pending → 双方好友数 → upsert);`AcceptFriend` 补拉黑复核与反向 pending 收敛;`RemoveFriend` 先判是不是好友再建容量行;新增 `Block` / `Unblock` / `ListBlocks`、推荐(FOF → 随机兜底,pivot 先取 `MIN/MAX(player_id)` 再随机,绝不全表扫)、每分钟频率配额(Redis 故障 **fail-open**,是有意的可用性取舍)、终态申请清理 sweep(默认 `report_only` 只统计)、S2C 推送(提交后、事务外、失败不影响 RPC 结果)、在线状态改读共享库的 `player:session:{id}`。缓存键换成带 hash tag 的 v3(两条 Lua 各只动同 slot 的两个键)。隔离级别固定 **READ COMMITTED**(`beginWriteTx`)。

过程教训值得留下:五路并行写码后,存储层与逻辑层对同一组 API 做了五处互不相容的假设,整个 logic 包编译不过;根因是本批的冻结规格只冻了事务形状、没冻 Go 签名。修复后加的三行编译期接缝断言(`go/friend/internal/logic/friend_logic.go` 的 `var (...)` 块,`grep -n 'FriendStore  = (\*data.FriendRepo)(nil)'` 能定位)**是唯一能让这类漂移在编译期暴露的东西,不要删**。

落点:`go/friend/internal/data/`(`friend_repo.go` / `block_repo.go` / `recommend_repo.go` / `sweep_repo.go` / `session_reader.go`)、`go/friend/internal/logic/`(`friend_logic.go` / `recommend.go` / `rate_quota.go` / `sweep.go` / `push.go`)。

### F3 —— 五处登记 + 部署链 + 迁库 + 运维工具改连 + robot 冒烟 + 文档(22 文件)

解决的是"这个服务能被启动器拉起、客户端经路由服可达、有端到端冒烟"。F1/F2 只是让代码存在,F3 才让它进得了系统。

- **五处登记**(端口 50400 / 指标 `:9180` 逐字一致):`tools/scripts/go_services.ps1`(Tier 1、可多开)、`tools/scripts/go_svc_image.ps1`(镜像 `mmorpg-friend`)、`tools/scripts/k8s_deploy.ps1`(`$GoSvcCatalogue` 条目 `Global=$true` + `MigrateJob`;ConfigMap 的 22 个契约值全部用 `Get-AuthoritativeScalar` 从 `etc/friend.yaml` 读(核实:`k8s_deploy.ps1` 里 `$friend* = Get-AuthoritativeScalar` 共 22 行))、`deploy/k8s/manifests/go-svc/friend.yaml` 与 `friend-migrate.yaml`、`tools/scripts/start_game.ps1`(列为**可选服务**,缺 exe 或库不就绪只告警跳过)。
- **`Etcd.Key` 与 `Redis.Key` 在 ConfigMap 里都显式写空串**(D-13)—— 省略整行会让 go-zero 的 `conf.MustLoad` 直接 Fatal、Pod CrashLoop,chat 在 kind 上实测踩过。
- **迁库**:`deploy/mysql-init/00_init_zone_dbs.sql` 加 `mmorpg_friend` 建库与授权(D-14 第 5 条:这里是建库的唯一登记处);friend 的三张表与 `friend_capacity` 回填段从 `guild_friend_tables.sql` 里删掉(⚠ 后续那个文件已被**整个删除**,现在 `deploy/mysql-init/` 下只剩 `00_init_zone_dbs.sql` 与 `gateway_tables.sql`;删除理由写在 `00_init_zone_dbs.sql` 第 55–60 行)。
- **运维工具改连**:`tools/merge_zone` 的在线审计不再扫 `friend:online`,改看 `player:session:{id}`;friend 表审计改用库名限定 `mmorpg_friend.friend`(配 `validateFriendSchemaName` 防注入);`data_consistency_check` 另开 `friendDB` 连接,连不上报 "NOT CHECKED" 而不是伪装通过。
- **robot `friend-smoke`**:`robot/etc/friend_smoke.yaml` + `robot/friend_smoke_scenario.go`,七步跨区冒烟(见 §2 第 9 步)。
- **sweep 接线**:`logic.StartSweep` 在 F2 落地时无调用方,本批接上(`metrics.Start` 之后一行,用 `signal.NotifyContext` 的 ctx)。默认 `report_only`,接上之后不删任何数据。

---

## §2 第一件事:验证这批代码

**顺序是硬约束**,前一步没过不要跑下一步 —— 顺序错了会得到误导性的失败。

### 第 1 步 —— 恢复 Go 工具链(其余全部步骤的前提)

本机现状(2026-09-19 逐条核实):

| 工具 | 状态 | 位置 |
|---|---|---|
| `go` / `gofmt` | ❌ **不存在** | 不在 PATH;`C:\Program Files\Go`、`C:\Go`、`E:\work\tools\go126` 都不存在;`C:\`/`D:\`/`E:\` 深度 4 递归搜 `go.exe` 零命中 |
| `buildenv.ps1` | ❌ **不存在** | `E:\work\tools\` 只剩 `figma-context-mcp\` 与 `push-image-in-batches.ps1`(2026-09-07 被清空)。仓库里 ~15 处文档仍写着 `. E:\work\tools\buildenv.ps1`,**那些命令行现在全都跑不通**,别照抄 |
| `protoc` | ✅ 在 | `third_party/grpc/install_vs2026_dbg/bin/protoc.exe`,`libprotoc 35.1` |
| `protoc-gen-go` | ✅ 在 | `C:\Users\luyua\go\bin\protoc-gen-go.exe`,`v1.36.10` |
| `protoc-gen-go-grpc` | ✅ 在 | `C:\Users\luyua\go\bin\protoc-gen-go-grpc.exe`,`1.6.0` |
| Python 3 + openpyxl | ✅ 在 | `...\Programs\Python\Python312`(在 PATH),`openpyxl 3.1.5` |

⚠ **`C:\Users\luyua\go\bin` 不在 Windows PATH 上。** 导表器与 proto-gen 都靠它找 `protoc-gen-go`,每个 shell 都要自己加。

**要做的事**:

1. 装 **Go ≥ 1.26.5**(下限来自 `go/friend/go.mod`、`go/schemamigrate/go.mod`、`tools/proto_generator/protogen/go.mod` 三个 module 的 `go` 指令;`go/proto`、`go/shared`、`robot` 是 1.24.5,1.26 通吃)。
2. 重建 `E:\work\tools\buildenv.ps1`(建议保持这个路径与文件名,让既有文档重新生效)。要设:`GOROOT`;`GOPATH` → `C:\Users\luyua\go`(复用现有 module cache);`GOPROXY` → `https://goproxy.cn,https://mirrors.aliyun.com/goproxy/,direct`(**官方 proxy 在这台机器上 TLS 超时**,是既有记录的坑);`PATH` 前置 `%GOROOT%\bin`、`%GOPATH%\bin`、`<repo>\third_party\grpc\install_vs2026_dbg\bin`。

**通过标准**(一条 shell 里全绿):

```powershell
. E:\work\tools\buildenv.ps1
go version                 # >= go1.26.5
gofmt -l E:\work\xuanming-server-mmo\go\friend   # 能跑起来(输出内容第 5 步再看)
protoc --version           # libprotoc 35.1
protoc-gen-go --version    # protoc-gen-go v1.36.10
```

**失败时保留**:`go env` 全量输出 + 安装日志。

### 第 2 步 —— 导表器(出三个新 tip 码)

> 这一步**不需要 Go 本体**(导表器是 Python + protoc + protoc-gen-go)。第 1 步若卡在装 Go 上可以先跑这步,但**不能**跳到第 3 步之后再跑。

**前置**:`data/tip/Tip.xlsx` 的 `//friend_error base=15000 width=1000` 组下**已经有 10 行**(核实:第 203 行组头,204–213 行数据),末三行就是本批新增的 `FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull`,`fault` 列**留空**。

> **更正一条既有文档说法**:`docs/design/friend-port-20260918.md` §7 ① 写「fault 列必须为 0」。实际约定是**留空** —— 全表 39 个故障码写 `1`,非故障行一律空单元格,三者都是业务拒绝。**本步不需要再改 xlsx,直接跑导表器。**
>
> **另一条已过期的注释**:`go/friend/internal/constants/constants.go` 顶部写着三个枚举"要等本分支合回 main 时添加" —— 三行已在 `7b48b0a98` 加进 xlsx,注释没人删。仍未做的只是跑导表器。

**命令**(二选一,推荐 A):

```powershell
# A. 仓库自带入口(自动预检 protoc 版本 + 装 Python 依赖)
. E:\work\tools\buildenv.ps1    # dev.bat 不会自己加 C:\Users\luyua\go\bin
cd E:\work\xuanming-server-mmo
.\dev.bat export

# B. 直接调
python tools\data_table_exporter\run.py tools\data_table_exporter\exporter_config.yaml
```

**期望产物 / 通过标准(四条,全要)**:

1. `generated/code/proto/tip/friend_error_tip.proto` 的 `enum friend_error` 从 **7 个码(15000–15006)** 变 **10 个**,新增 `kFriendBlocked = 15007` / `kFriendBlockListFull = 15008` / `kFriendTargetInboxFull = 15009`。
2. `go/shared/generated/pb/table/friend_error_tip.pb.go` 同步出现 `FriendError_kFriendBlocked` 等三个常量。
3. `go/shared/generated/tip/segments.go` 里 friend 那行从 `Lo: 15000, Hi: 15006, Count: 7` 变成 `Hi: 15009, Count: 10`。**这是最省事的机械判据。**
4. `go/shared/generated/tip/faults.go` 的 `Faults` 切片里**不得出现**这三个码。

**⭐ 最高优先的一致性判据**:生成出的枚举名必须与 `go/friend/internal/constants/constants.go` 里写的逐字一致。**不一致就改 `constants.go`,绝不要改 xlsx 去将就代码**(配表是事实源)。

**多会话共用仓库的顺带注意**:导表器会按 `exporter_config.yaml` 的 deploy 覆盖 `E:\work\mmorpg-client\Assets\Scripts\Table\Generated` 与仓内 cpp/go/java 产物。跑前先 `git status --porcelain --ignore-submodules=all data generated go/shared/generated cpp/generated/table java` 存一份,跑完逐项对比。

**失败时保留**:导表器完整 stdout/stderr;若 `BitIndexStateError` 之类中止,保留 `tools/data_table_exporter/state/` 原样(**不要手改**)。

### 第 3 步 —— proto-gen(**必须全量**)

**为什么必须全量**:`proto/friend/friend.proto` 这批改了三件牵动全仓发号的事 —— 服务改名 `ClientPlayerFriend` 并标客户端协议、删 `NotifyOnline` / `NotifyOffline`、新增 `Block` / `Unblock` / `ListBlocks` / `RecommendFriends` 与 `NotifyFriendEvent`。

**⚠ 硬约束**:改名释放了旧消息号 **2 / 7 / 11 / 12 / 53 / 76 / 119 / 120**(核自 `proto/message_id.txt`)。生成器(`tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go` 的 `InitMessageId`)把未占用号灌进 `unUseMessageId` 再 `for uk := range unUseMessageId { mv.Id = uk; break }` 分配 —— **Go map 迭代顺序不定**,同一个号这次指向哪个方法每次都可能不同。**全仓所有服务的消息号都可能重新洗牌,不只 friend 自己**(历史上发生过一次:帮会 19 号消息 id 易主)。

由此:**gate 的 `rpc_event_registry`、路由服的 `route_table`、Unity、robot 的消息号必须来自同一次 proto-gen 并同批发布。** 混用两次生成的产物**不报错**,只会让客户端发 A 方法、服务端按 B 方法解包。**不要手改 `proto/message_id.txt`**(它是生成物)。regen 之后请通知其他在改客户端协议的会话重取消息号。

**命令**:

```powershell
. E:\work\tools\buildenv.ps1
cd E:\work\xuanming-server-mmo
.\dev.bat proto
```

`dev.bat proto` = `:prepare_protoc`(校验 libprotoc 35.1 + 前置 PATH)→ `dev_tools.ps1 -Command proto-gen-build` → `dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools\proto_generator\protogen\etc\proto_gen.yaml`。

> **⚠ 千万别只跑 `-Command proto-gen-run`**:`Invoke-ProtoGenRun` 默认"优先用现成的 `proto-gen.exe`,其次 `pbgen.exe`,最后才 `go run ./cmd`"。仓里那两个 exe 的时间戳是 **2026-09-16 22:14**,而生成器源码在 `7af8342ad`(09-18)之后还动过 —— 不先 `proto-gen-build` 就是用陈旧二进制生成,改动静默不生效。
>
> **更正一条常见误解**:`go/build.bat` → `go/build.ps1` **不是** proto-gen 入口,它只跑 db/login 的 goctl 包装与 import 修补;它自己的 docstring 就指向 `dev_tools.ps1 -Command proto-gen-run`。

**期望产物 / 通过标准(三项,全要)**:

① `go/proto/friend/friend_grpc.pb.go` 里 `ClientPlayerFriend_ServiceDesc.Methods` **恰好 11 条**(10 个 C2S + `NotifyFriendEvent`;当前是旧的 `FriendService_ServiceDesc`,8 条):

```powershell
Select-String -Path go\proto\friend\friend_grpc.pb.go -Pattern 'MethodName:' | Measure-Object   # 期望 11
Select-String -Path go\proto\friend\friend_grpc.pb.go -Pattern 'ServiceName:'                   # 期望 "friendpb.ClientPlayerFriend"
```

② `go/client_rpc_router/generated/pb/game/route_table.go` 里 friend **11 行**全为 `/friendpb.ClientPlayerFriend/*` 且 `ClientProtocol: true`:

```bash
grep -c 'NotifyOnline\|NotifyOffline' go/client_rpc_router/generated/pb/game/route_table.go   # 期望 0
grep -c 'friendpb.FriendService'      go/client_rpc_router/generated/pb/game/route_table.go   # 期望 0
```
(当前状态:8 行,全 `/friendpb.FriendService/*`、`ClientProtocol: false`,前一条 grep 计数为 2。)

③ robot 与 Unity 侧生成出下行 handler。**生成器是每方法一个 handler**(ClientPlayerTeam 有 15 个),所以 friend 应新增 **11 个 robot handler + 11 个 Unity handler**,`NotifyFriendEvent` 只是其中最关键的一个 —— **没有它这次改名就白改了**。
- robot:`robot/logic/handler/` 下按现有 team 的命名规律,如 `client_player_friend_notify_friend_event.go`、`client_player_friend_add_friend.go`。
- Unity:`E:\work\mmorpg-client\Assets\Scripts\Net\Generated\Handlers\` 下如 `ClientPlayerFriendNotifyFriendEventHandler.cs`,并重写 `HandlerRegistry.cs`。
- 同时核 `robot/generated/pb/game/message_id.go` 与 `go/friend/generated/pb/game/message_id.go`:`FriendService*MessageId` 应全部消失、换成 11 个 `ClientPlayerFriend*MessageId`,并**逐个**与 `robot/friend_smoke_scenario.go` 里用到的常量名对上(那些名字是按命名规律推导写的)。

**⚠ 本步必须额外处理的四个陷阱(都已核实仍然适用)**:

- **客户端 `gen_proto.ps1` 尚未收录 friend**。`E:\work\mmorpg-client\tools\gen_proto.ps1` 的 `$files` 列表里没有 `proto/friend/friend.proto`,也没有 `generated/code/proto/tip/friend_error_tip.proto`;而 `proto_gen.yaml` 的 `generators.enable_unity_client: true` 是**全局开关、没有按域细分**。用默认配置跑会往客户端写 11 个 `ClientPlayerFriend*Handler.cs`,而它们引用的 `Friendpb.*` 类型不存在 → **客户端 CS0246**。
  **处置**:同一批次里往 `gen_proto.ps1` 的 `$files` 补这两行并跑一次客户端 `gen_proto.ps1`。若这批不打算动客户端,改用 `enable_unity_client: false` 的**配置副本**跑 `proto-gen-run -ConfigPath <副本>` —— **不要**直接改默认 `proto_gen.yaml` 的这个开关(其他域会失去同步),**也不要**删 friend 块(未解析的域会丢号)。
- **C++ 侧会被一起重算**。`cpp/generated/rpc/service_metadata/friend_service_metadata.h` 现在写的是 `FriendServiceAddFriendMessageId = 11` 等,被 `rpc_event_registry.cpp` include;`cpp/generated/proto/friend/*.pb.{h,cc}` 与 `cpp/generated/grpc_client/friend/friend_grpc_client.cpp` 也是生成物、已登记进 `proto.vcxproj` / `grpc_client.vcxproj`。**regen 之后 C++ 必须重编**(串行 `msbuild /m:1 /nr:false`,顺序 proto → rpc → 各节点库 → 节点),否则 gate 的消息号表还是旧的。并核 `rpc_event_registry.h` 的 `kMaxRpcMethodCount` **等于 `proto/message_id.txt` 最大 id + 1**(当前 228 / 227,对得上);对不上就是生成器半途退出,重跑一次即可。
- **regen 会吃掉 `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` 里守护段外的 Agones 块**。该文件现在仍有 `auto createPermit = agones::SceneLifecycle::Instance().AcquireCreatePermitBlocking();`(核实:第 245 行),regen 后用 `git diff` 确认它还在,被删了就恢复。
- **robot vendor**。`robot/vendor/proto/` 下只有 battle/chat/common/db/guild/login/match/scene/team/trade,**没有 friend**,`modules.txt` 里也搜不到。robot 默认 `-mod=vendor`,regen 之后必须 `cd robot && go mod tidy && go mod vendor`。另外 robot 的 handler stub 是"文件存在即跳过",坏 stub 重跑生成器不会自愈 —— 要先删再生成。

**失败时保留**:生成器完整日志 + `git diff --stat`(`proto/ generated/ go/proto go/*/generated cpp/generated robot/generated robot/logic/handler`)+ 客户端仓的 `git status`。

### 第 4 步 —— `data/MessageLimiter.xlsx` 填档位(**必须在 proto-gen 之后**)

这张表按**消息号**配档位(schema 见 `data/schema/messagelimiter_table.proto`:`id` / `max_requests` / `time_window` / `tip_message`,`id` 就是消息号),而消息号第 3 步才定下来。

**当前状态**:`MessageLimiter` sheet 有 53 行数据(第 6 行起),friend 的 8 个旧号 2/7/11/12/53/76/119/120 **一个都不在里面** —— friend 从来没配过档位。

**不填的后果**:吃 gate 默认档 **3 次 / 1 秒 / 每消息号**(`cpp/libs/engine/core/message_limiter/message_limiter.h` 的 `defaultMaxRequests{3}` / `defaultTimeWindow{1}`,`MessageLimiter::CanSend` 只在 `FindByIdSilent` 命中时才覆盖),好友列表刷新、"换一批"推荐都会被打到并回 `kRateLimitExceeded`。

**要做的事**:从 `robot/generated/pb/game/message_id.go`(或 `proto/message_id.txt`)取 11 个 `ClientPlayerFriend*` 的真实号,按下表追加 10 行到现有数据块末尾(数据块从第 6 行开始,**当前最后一行是第 58 行**,所以新行落在第 59–68 行;列顺序 `id, max_requests, time_window, tip_message`):

| 方法 | max_requests | time_window | tip_message |
|---|---|---|---|
| `GetFriendList` / `GetPendingRequests` / `ListBlocks` | 10 | 1 | 1000 |
| `AddFriend` / `AcceptFriend` / `RejectFriend` / `RemoveFriend` / `Block` / `Unblock` | 5 | 1 | 1000 |
| `RecommendFriends` | 1 | 1 | 1000 |
| `NotifyFriendEvent`(S2C) | **不配**(下行,客户端不发) | — | — |

⚠ 这组档位数值是**建议值,没有代码或仓内文档背书**(既有行的 `time_window` 都是 1、`tip_message` 都是 1000,这两列照抄即可)。填完**再跑一次第 2 步的导表器**。

**通过标准**:`generated/tables/messagelimiter.json`(文件名**全小写**)的 `data` 数组里能查到这 10 个新 id 且数值与上表一致;`generated/tables/manifest.json` 里 `MessageLimiter` 的 `rows` 变成 **63**、顶层 `version` +1。
⚠ **不要拿 53 去对 manifest**:xlsx 现在是 53 行,但 manifest 里记的还是上一次导表的 **46**(`generated_at` 2026-09-18,顶层 `version: 17`)—— 说明这张表已经有 7 行(不是 friend 的)改完没导过。所以导表器跑完的正确期望是 46 → 63,中间那 7 行会一起进去;
另外 `robot/etc/friend_smoke.yaml` 的 `request_interval_ms`(当前 `1100`)**不用动**(它按默认档 3 次/秒 设的保守值,配档后只会更宽松)。

### 第 5 步 —— `go/friend` 编译与单测

**工作目录**:`E:\work\xuanming-server-mmo\go\friend`。**前置**:第 1–3 步全过。

```powershell
. E:\work\tools\buildenv.ps1
cd E:\work\xuanming-server-mmo\go\friend
gofmt -l .
go mod tidy
go build ./...
go vet ./...
go test ./... -count=1
```

**通过标准**:

- `gofmt -l .` **输出为空**。写这批代码的会话从未跑过任何格式化(本机当时没有 gofmt),所以这一步大概率会报一串文件 —— 报了就 `gofmt -w .` 修掉,不算缺陷。
- **`go mod tidy` 的结果要人工看一眼,不要直接接受**:`github.com/redis/go-redis/v9` **只应出现在 indirect 块**。落进直接依赖块说明还有一处 import 没从 go-redis 改到 go-zero 的 `core/stores/redis` —— 那是真实缺陷,先去找那处 import。(判据本身写在 `go/friend/go.mod` 的注释里;核实现状:`go/friend` 下零直接 import go-redis,`require` 块里也确实没写它,这是**正确的、不是漏写**;`go.mod` 故意没有 indirect 块,tidy 会补,也是预期。)
- `go build ./...` / `go vet ./...` 零输出;`go test ./... -count=1` 全绿。
- **此时 `internal/data` 的集成用例会 Skip,这是预期** —— 它们挂在 `FRIEND_TEST_MYSQL_DSN` 上(见第 6 步)。

**module 形状**(免得 tidy 时吓一跳):`go/friend/go.mod` 有三条路径 replace(`proto` / `shared` / `schemamigrate`)和一条发布包改名 replace(`github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`)。`go/` 下**没有 `go.work`**,各服务是独立 module,只能逐个进目录跑。

**失败时保留**:每一步完整输出。`go build` 首次大概率先报骨架与跨包签名问题 —— **那是 F1/F2 的遗留,不是最后那批部署登记引入的**,不要顺着最新提交去找。

### 第 6 步 ⭐ —— 真 MySQL 8.4 并发回归(唯一能支撑锁序结论的证据,必须点名要求跑)

**这一步存在的理由**:F2 把所有写事务改成 `READ COMMITTED + 全局锁序「容量守卫最先」`。A 仓 2026-08-11 就是因为守卫取得的时机晚于锁定读,在 InnoDB 上成环炸 1213,而 **CI 从不设 MySQL 的 DSN,这条路径长期"SKIP 在报告里等于绿",缺陷被盖了一个多月**。

**⚠⚠ 全体 SKIP 时 `go test` 的退出码仍然是 0。"退出 0"不是通过标准。**

**环境变量:两个都要设**

```powershell
$env:FRIEND_TEST_MYSQL_DSN      = 'appuser:apppass123@tcp(127.0.0.1:3306)/mmorpg_friend_test?parseTime=true&charset=utf8mb4&sql_mode=%27STRICT_TRANS_TABLES%27'
$env:FRIEND_REQUIRE_MYSQL_TESTS = '1'
```

- `FRIEND_TEST_MYSQL_DSN`(常量 `friendTestDSNEnv`,`go/friend/internal/data/friend_repo_mysql_test.go`)是**唯一门控**;`FRIEND_REQUIRE_MYSQL_TESTS`(`friendRequireMySQLEnv`)**不是第二个门控**,它只决定"跳过算不算失败"。
- DSN 形状抄自 `go/friend/internal/svc/servicecontext.go` 的 `BuildDSN`。
- **⚠ 指向一个单独的临时 schema,不要指 `mmorpg_friend`**:`openFriendTestDB` → `resetFriendIntegrationSchema` 每条用例开头都 `DROP TABLE IF EXISTS` 四张表再 CREATE。建库两句即可:`CREATE DATABASE mmorpg_friend_test ...; GRANT ALL PRIVILEGES ON mmorpg_friend_test.* TO 'appuser'@'%';`

**⚠ 必须是真 MySQL,不是 TiDB**:TiDB 没有间隙锁,这条路径在它上面**恒绿**。本机两套 SQL 入口:`127.0.0.1:3306` = MySQL 容器(`deploy/docker-compose.yml`,image `mysql:latest`)**用这个**;`127.0.0.1:4000` = TiDB(`deploy/docker-compose.tidb.yml`,`pingcap/tidb:v8.5.2`)**绝对不要用**。

compose 钉的是 `mysql:latest` 不是固定 8.4,所以跑前先确认:

```powershell
docker exec mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -B -e "SELECT VERSION(), @@transaction_isolation, @@binlog_format"'
```
期望看到 8.4 及以上的 MySQL(**不含 `TiDB` 字样**)。若 `latest` 解析出来不是 8.x/9.x InnoDB,先把 image 钉到 `mysql:8.4` 再跑。

```powershell
go test ./internal/data -count=1 -v 2>&1 | Tee-Object -FilePath ..\..\..\friend-lockorder.log
```

**通过标准(五条,全要)**:

1. `TestFriendIntegrationGateIsHonored` **PASS,不是 SKIP**(它就是"全体 Skip 不算通过"的机械保障)。
2. **五个锁序场景全部 PASS**(函数名逐个核自 `go/friend/internal/data/friend_guard_lock_order_mysql_test.go`):`TestAddFriendGuardBeforeLockingReads_SharedTarget`、`TestAddFriendGuardBeforeLockingReads_DistinctTargets`、`TestBlockGuardBeforeLockingReads_SharedBlocker`、`TestBlockAndAcceptFriendInterleaved`、`TestAddFriendAndAcceptFriendOnSamePair`。并发度常量 `guardConcurrency = 16`,**不要为了跑得快调低它**(同文件注释写明 8 并发时时序会错开、这个文件会变成一条昂贵的绿灯)。
3. **整个包零 SKIP**。
4. 输出里**不得出现 `Error 1213`**(测试自己会在看到 1213 时 `t.Fatalf`,见 `classifyGuardErrors` → `assertNoDeadlock`)。
5. **在交付里明写一句"看到了 PASS 而不是 SKIP"。**

**同批建议顺手做的 `EXPLAIN` 三条**:见 §3 第 2 条(锁序的书面保证里唯一靠优化器兜底的一环)。

**失败时保留**:完整 `-v` 输出(已 Tee 到文件)+ `SHOW ENGINE INNODB STATUS` 的 `LATEST DETECTED DEADLOCK` 段原文。

### 第 7 步 —— 全新空库 `-migrate` + 常驻启动

**前置**:第 5 步过;`mmorpg_friend` 是**全新空库**。建库语句的唯一登记处是 `deploy/mysql-init/00_init_zone_dbs.sql`,存量卷不会重跑 initdb,要手工执行:

```powershell
docker exec mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "DROP DATABASE IF EXISTS mmorpg_friend; CREATE DATABASE mmorpg_friend DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; GRANT ALL PRIVILEGES ON mmorpg_friend.* TO appuser@''%''; FLUSH PRIVILEGES;"'
```

**7a. 迁移**

```powershell
pwsh -File tools\scripts\dev_tools.ps1 -Command go-svc-build -GoServices friend   # 产出 bin\go_services\friend.exe
cd E:\work\xuanming-server-mmo\go\friend
..\..\bin\go_services\friend.exe -f etc\friend.yaml -migrate
```

**通过标准**:

- 退出码 **0**,stdout 有 `schema migration OK: mmorpg_friend synced from proto/friend/friend_table.proto (N statement(s) executed)`;
- 库里**恰好四张表**:`friend` / `friend_request` / `friend_capacity` / `friend_block`(清单源是 `go/friend/internal/data/tables.go` 的 `Tables()`,四个 message `FriendEdgeRecord` / `FriendRequestRecord` / `FriendCapacityRecord` / `FriendBlockRecord`);
- **主键全是整数列、零 UNIQUE KEY**(D-14 §2):
  ```sql
  SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SEQ_IN_INDEX
  FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='mmorpg_friend' ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX;
  -- 期望:NON_UNIQUE=0 的索引名只有 PRIMARY
  ```
- `friend_request` 有 `updated_ms` 列与两条索引 `(to_player_id,status)`、`(status,updated_ms)`;
- **再跑一次 `-migrate` 应执行 0 条语句**(幂等);
- `friend.exe -allow-modify`(**不带** `-migrate`)必须**非 0** 退出(`friend.go` 的 `validateMigrationFlags`,走 `schemamigrate.ExitFailed`)。

**7b. 常驻启动**

```powershell
..\..\bin\go_services\friend.exe -f etc\friend.yaml
```
前置:etcd(2379)、Redis(6379)、MySQL(3306)都在跑。`etc/friend.yaml` 当前:`ListenOn: 127.0.0.1:50400`、`MetricsListenAddr: ":9180"`、`Mode: dev`、`Schema.AutoMigrate: true`、`Friend.Sweep.Mode: report_only`。

**通过标准**:

- 日志里有一行 `  FRIEND SERVICE STARTED SUCCESSFULLY`(`go/friend/friend.go:279`,行首两个空格是横幅排版)。
  ⚠ **就绪判据是子串 `STARTED SUCCESSFULLY`,不是整行**:`tools/scripts/go_services.ps1:495` 是 `$content -match "STARTED SUCCESSFULLY"`(全量读日志文件,不逐行比)。所以两个前导空格与 `FRIEND` 这个词都不影响启动器;真正不能动的是 `STARTED SUCCESSFULLY` 这八个字符 —— 改掉它启动器就永远等不到 friend 就绪(`friend.go` 那行上方的注释写的是同一件事)。
- 横幅里 `mysql:` 那行**不含密码**(走 `svc.MySQLTarget`,格式 `<host>/<db> (user=<user>)`;含密码的 `BuildDSN` 只交给 `sql.Open`)。
- 其余可核项:`friend_redis:` 显示 `shared-fallback host=127.0.0.1:6379 type=node`;`sweep:` 显示 `mode=report_only`;`schema:` 显示 `auto-migrate (schemamigrate.Up at startup)`。
- `curl http://127.0.0.1:9180/metrics` 能看到四个 friend 指标的预建 0 值序列:`friend_push_total` / `friend_rate_quota_total` / `friend_online_lookup_total` / `friend_sweep_pending_rows`。
- `etcdctl get --prefix FriendNodeService.rpc/` 的值(protojson 的 NodeInfo)里 `endpoint` 与 `grpcEndpoint` **都**填了(只填前者路由服拨不通,guild 踩过;`friend_test.go` 的 `TestNodeInfoValueMatchesRegistryContract` 钉住这点)。
- Ctrl+C 后日志顺序是**先注销、再排空**,总时长 < 24s(`lifecycle.HardTimeout`)。

**失败时保留**:两段完整 stdout/stderr;四张表的 `SHOW CREATE TABLE` 原文;`/metrics` 抓取。

### 第 8 步 —— 缓存链路手工核对(唯一没有自动测试覆盖的改型)

go-zero 的 `GetCtx` 把 `redis.Nil` **吞成 `("", nil)`**。把 A 仓那套 go-redis 的缓存代码机械改型过来后,`generation` 会读成**空串**,与 Lua 脚本里的 `"0"` 比不上 → **脚本恒返回 0、缓存永远写不进去,而且一个错都不报**。修复在 `go/friend/internal/data/friend_repo.go` 的 `loadVersionedFriendCache` 里(显式把空串补成 `"0"`)。

**当前缓存键形状**(核自 `friendListKey` / `pendingRequestsKey` / `friendCacheGenerationKey`):

| 用途 | 键 |
|---|---|
| 好友列表 | `friend:{f:<playerID>}:list:v3` |
| 待处理申请 | `friend:{f:<playerID>}:req:v3` |
| generation | `friend:{f:<playerID>}:list:v3:generation` |

`{f:<pid>}` 是 Redis Cluster 的 hash tag,保证数据键与 generation 键同 slot。
**v2 的旧形状 `friends:v2:<pid>` 已经不存在** —— `PROGRESS.md` 里 F1 批那条写的是 v2,**以代码为准**。

```bash
docker exec redis redis-cli DEL 'friend:{f:<pid>}:list:v3' 'friend:{f:<pid>}:list:v3:generation'
# 然后经 gate/路由服调一次 GetFriendList(最稳的是直接跑第 9 步的冒烟,冒烟里就有 GetFriendList)
docker exec redis redis-cli GET 'friend:{f:<pid>}:list:v3'
```
(**shell 里必须给键名加引号**,否则 `{}` 会被展开。)

**通过标准**:第三步 **必须非空**。若恒为空而调用又不报错,就是那处 generation 补 `"0"` 的修复丢了。
**失败时保留**:`redis-cli MONITOR` 抓到的 `EVAL` 参数 —— 看 `ARGV[1]` 是不是空串。

### 第 9 步 —— 两区 robot `friend-smoke`

**工作目录**:`E:\work\xuanming-server-mmo\robot`(**必须从 `robot/` 跑**,配置里 `table_dir: "../generated/tables"` 是相对路径)。

```powershell
cd E:\work\xuanming-server-mmo\robot
go mod tidy; go mod vendor        # regen 之后必须做,vendor 里还没有 friend 包
go build -o robot.exe .
.\robot.exe -c etc\friend_smoke.yaml
```

**账号号段**(核自 `robot/etc/friend_smoke.yaml`):`robot_9701` / `robot_9702` / `robot_9703`,密码 `123456`,`auth_type: "password"`。A=9701、C=9703 登 `zone_a: 1`;B=9702 登 `zone_b: 2`。97xx 段是本冒烟专用(95xx 已被帮会二期资产通道冒烟占用,9601 是 travel_smoke)。
⚠ **归属区在建角时登记,之后不随登录区变化**:9701/9703 必须首次在 zone 1 建角,9702 必须首次在 zone 2 建角。B 若其实是 zone 1 的角色,推送断言照样过,但**证明不了任何跨区结论**。

**前置条件(缺一条就会失败,原因会写进 `FRIEND_SMOKE_FAIL` 的 `reason=`)**:

1. **两个 zone 的 gate 都以 `GATE_CLIENT_RPC_ROUTER=1` 启动**。friend 只承诺路由服模式可达(D-12),不开 gate 直连白名单;直连模式下冒烟会报 `kServiceUnavailable`。`start_game.ps1` 的 `-GateRouterMode` 默认就是 `'1'`。**已经在跑的 gate 不会换模式**(启动时读一次并缓存)—— 先 `dev_tools.ps1 -Command dev-stop`。
2. **`zone_config` 里有 zone 2 那一行**。`deploy/mysql-init/gateway_tables.sql` 的 seed 只插了 zone 1,要手工补:
   ```powershell
   "INSERT IGNORE INTO zone_config (zone_id,name,manual_status,capacity,recommended,sort_order) VALUES (2,'zone-2',0,5000,0,2);" |
     docker exec -i mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" mmorpg'
   ```
   网关已在跑且 server-list 仍只有 zone 1 → 重启网关。
3. **`client_rpc_router` 与 `friend` 已起**(friend 全服一份即可):
   ```powershell
   $env:GATE_CLIENT_RPC_ROUTER = '1'
   & .\tools\scripts\dev_tools.ps1 -Command dev-start-zones -Zones 1,2
   ```
   (同一个 PowerShell 会话里用 `&`,**不要** `pwsh -File`,否则数组参数会被压成单个字符串。)friend 在 `go_services.ps1` 的条目是 `friend = @{ Dir="friend"; Entry="friend.go"; Port=50400; ConfigFlag="-f"; ConfigFile="etc/friend.yaml"; AllowMultiInstance=$true; Tier=1 }`,`-Zone 2` 会把端口位移到 51400 / metrics 10180。
4. **`mmorpg_friend` 已建且表已迁移**。`start_game.ps1` 把 friend 列为可选服务:库不存在或 appuser 无权限时会打「好友库 mmorpg_friend 未就绪(…),本次跳过 friend」并继续启动其余服务 —— **看到这条 WARN 就说明 friend 根本没起,冒烟必挂**。
5. **friend 的共享 Redis 必须指向写 `player:session:{id}` 的那个库**(`etc/friend.yaml` 顶层 `Redis:` 段)。指错库**不会报错**,只会让好友列表里所有人恒为离线 → `is_online` 断言失败。
6. **推送要能落地**:两个 zone 的 gate 都在消费 `gate-cmd_g<N>`,且 friend 的 `KAFKA_COMMAND_TOPIC_PARTITIONS` / `KAFKA_COMMAND_TOPIC_GENERATION` 与 match **逐字一致**(`go/shared/kafkacmd/command_topic.go`)—— 分区数 / 代号对不上时推送**静默丢失**。
7. **发布顺序**:friend 新二进制与迁移先上 → `client_rpc_router` → gate。反过来会让 gate 认了新消息号而路由表还没有,请求被判 `unknown_message`。
8. **Tip 表已导出三个新码**(第 2 步),robot 编译依赖这些生成枚举。

**通过标准**:

- 退出码 **0**,日志里有一行 `FRIEND_SMOKE_OK player_a=… player_b=… player_c=… gate_a=… gate_b=…`(格式串在 `robot/friend_smoke_scenario.go` 的 `RunFriendSmoke` 收尾处)。任一步失败 → `FRIEND_SMOKE_FAIL step=… reason=…`,退出码 1。
- 逐步要看到:`cross_zone=true` 时 **A 与 B 的 gate 地址必须不同**;跨区推送两次(`REQUEST_RECEIVED` / `REQUEST_ACCEPTED`)都在 10s 内到达;重复 `AddFriend` 回 `FriendRequestAlreadySent`;黑名单**两个方向**(A→C 与 C→A)都回 `FriendBlocked`;推荐结果 ≤ 20 且不含自己 / 已是好友的 B / 已拉黑的 C;
- **D-9 反向断言**:客户端发 `NotifyFriendEvent` 的消息号必须**拿不到业务回包**,信封 tip = `kServiceUnavailable`(friend 回 `PermissionDenied`,路由服把一切 gRPC 错误翻成该码,见 `go/client_rpc_router/internal/logic/forwardlogic.go`),**并且要在同一运行窗口的 friend 日志里看到 `[friend] 拒绝客户端调用非客户端方法`**(逐字核自 `go/friend/internal/session/session.go:102`;早期交接口述写成「拒绝客户端调用内部方法」,按那句话去 grep 会一无所获) —— 信封不保留原始 gRPC code,只靠机器人收到失败**不能**证明拒绝原因。
- **可重复运行**:第 0 步会幂等地把三人关系清回"互不相识、互不拉黑"。唯一连跑限制是 `Friend.RequestQuotaPerMinute`(默认 10 次/分钟):一轮里 A 要发 3~4 次申请,**同一分钟内连跑三轮以上会撞 `kRateLimitExceeded`**。

**⚠ 如实记下的能力边界(别把绿当成证据)**:"推荐结果不含已拉黑的 C"这条断言在三账号数据集下**结构性不可能失败** —— 两条召回路径的候选都来自 `friend` 表,而 C 在该表零行,所以无论 `friend_block` 的双向排除是否失效它都进不了候选集。`friend_block` 排除与 `RecommendMaxLimit` 截断只能靠单测或人工造数据覆盖(见 §3 第 6 条)。
另有一处防假绿要留意:`ClientPlayerFriend` 的全部 **11 个**消息号(含 `NotifyFriendEvent`)必须在转交 `MessageBodyHandler` 之前被本场景认领 —— 漏一个,信封级拒绝会被解成全零响应、记成 tip 0,**失败显示为通过**(trade 冒烟踩过)。

**失败时保留**:robot 完整输出 + 两个 zone 的 gate 日志 + friend 日志 + `client_rpc_router` 日志(四份缺一不可,因为"信封级拒绝"与"真的没到"在 robot 侧长得一样)。

---

## §3 已登记但没做的收尾项

> 全部条目都在 main(`f06090b19` / `cda218956` / `7b48b0a98` / `57af3fd09` 之后)逐条 `sed -n` 核实过,核实日期 2026-09-19。
> ⚠ 大前提:**整批代码从未编译、从未测试**。下面所有"修法建议"都排在 §2 的第 2、3、5 步之后 —— 在那三步过掉之前动这里的任何一条,你分不清报错是你改出来的还是本来就有的。

| # | 严重度 | 条目 | 改动面 |
|---|---|---|---|
| 1 | **P2** | `friend_capacity` 无回收路径 | 3–5 文件(含 proto) |
| 2 | **P2** | 三条锁序假设未经 `EXPLAIN` 核对 | 0(纯核对)或 1–2 |
| 3 | P3 | pending 计数的并发不变量只靠注释守着,无测试 | 1 文件 |
| 4 | P3 | `metrics` 里第三份 sweep 模式字面量对齐不到 | 1–2 文件 |
| 5 | P3 | 三个零调用方的导出方法(`AreFriends` 尤其危险) | 1–2 文件 |
| 6 | P3 | 三处缺测试(session_reader / recommend_repo / sweep ticker) | 新增 2–3 个测试文件 |
| 7 | P3 | `data.FriendEntry.LastActiveMs` 是死字段 | 1 文件 |
| 8 | P3 | `friend_table.proto` 的 `updated_ms` 注释已过期 | 1 文件(+ 连带 regen) |

### 1.【P2】`friend_capacity` 表没有任何回收路径

**位置**:`(*FriendRepo).ensureFriendCapacityRows`(`go/friend/internal/data/friend_repo.go`,`INSERT IGNORE INTO friend_capacity ...` 在**事务外、自动提交**);仍然无条件调它的写路径**有两条**:`(*FriendRepo).AddFriendRequest`(`friend_repo.go:287`)与 `(*FriendRepo).AcceptFriend`(`friend_repo.go:449`);sweep 的三条 SQL(`SweepTerminalRequests` / `deleteTerminalRequestsBefore`,`go/friend/internal/data/sweep_repo.go`)**全部只写 `FROM friend_request`**,`friend_capacity` 一个字没出现;频率配额的三个 fail-open 分支在 `(*FriendLogic).allowFriendRequest`(`go/friend/internal/logic/rate_quota.go`)。已登记处:`docs/design/friend-port-20260918.md` §6 第 12 条、`PROGRESS.md` F2 批条目。

**为什么是问题**:`AddFriendRequest` 为了拿容量守卫,会对**任意** `to_player_id` 建行 —— friend 服务没有玩家名册,无法验证 target 是否真实存在(代码注释自己就这么写)。`friend_capacity` 没有 TTL、不在 sweep 范围内、没有任何删除语句,于是该表随"被发起过好友申请的 id 个数"**单调增长**,且增长可由客户端驱动。唯一的闸是每分钟频率配额,而配额在 Redis 故障时**按设计 fail-open 放行**(理由写在 `rate_quota.go` 文件头,是有意的可用性取舍,不是缺陷)—— 即"Redis 挂掉的窗口内,这张表的增长完全不受限"。

**⚠ 已缓解的部分(别重复做,这一条更正了早期的描述)**:
- `Block`(`go/friend/internal/data/block_repo.go`)**已不再无条件 ensure**:已加事务外快速失败探针,名额满就直接 `ErrBlockListFull`、不 ensure;`config.Validate` 拒收 `Friend.MaxBlocks == 0`(`internal/config/config.go`),所以该探针恒生效。残留向量只剩代码注释自己点名的 **Block → Unblock 反复换目标**(`Unblock` 只删 `friend_block`,不碰 `friend_capacity`)。
- `RemoveFriend` 已被 F2-15 改成"先 `friendEdgeExists` 普通读判是不是好友,再 ensure"。
- **仍然无条件 ensure 的是 `AddFriendRequest` 与 `AcceptFriend` 两条**(早期说法写的是"只剩 AddFriendRequest",漏了后者)。
  ⚠ `AcceptFriend` 这条**更松**:`from_player_id` 来自请求体、进事务复核申请行之前就先 ensure 了双方的容量行(`friend_repo.go:449` 在 `beginWriteTx` 之前),而 `AcceptFriend` **不吃**每分钟频率配额(`allowFriendRequest` 全仓只有 `friend_logic.go:272` 一个调用方,在 `AddFriend` 路径上)。也就是说客户端用互不相同的假 `from_player_id` 连调 `AcceptFriend`,每次都能凭空造出一行 `friend_capacity`,连 fail-open 的那道配额都不经过。
  核心结论(无回收路径、P2)不变,但**修回收路径时两条都要算进增长面**;顺带可以考虑给 `AcceptFriend` 也加一道"先普通读判申请行在不在,再 ensure"的快速失败(形状照 `RemoveFriend` 的 `friendEdgeExists`,`friend_repo.go:613`)。

**建议修法**。A 仓有先例可参考(浅克隆只读副本,见 §6 的失效提醒):`.../scratchpad/xuanming-server/services/social/friend/internal/data/friend_repo.go` 的 `DeletePairGuardsBefore`,接在 `internal/biz/sweep.go`。
⚠ **不能直接照搬**:A 仓删的是**专用**的 `friend_pair_guards` 表(带 `created_at`、纯守卫载体,按年龄删没有语义损失);本仓的 `friend_capacity` **本身就是好友数的权威计数行**(D-10),且 `FriendCapacityRecord`(`proto/friend/friend_table.proto`)只有 `player_id` / `friend_count` 两列,**没有任何时间戳列**。两条路选一条:

1. **加时间戳列**(更贴近 A 仓):给 `FriendCapacityRecord` 加一列(如 `created_ms`)并配 `OptionIndex`,由 `go/schemamigrate` 建;在 `sweep_repo.go` 加 `DELETE FROM friend_capacity WHERE friend_count = 0 AND created_ms < ? LIMIT ?`,在 `internal/logic/sweep.go` 的一轮里调它。**前置:改 proto = 要跑 proto-gen + `-migrate`**。
2. **不加列,用 `friend_count = 0` + 反查**:`DELETE FROM friend_capacity fc WHERE fc.friend_count = 0 AND NOT EXISTS (SELECT 1 FROM friend_request WHERE (from_player_id=fc.player_id OR to_player_id=fc.player_id) AND status=1) AND NOT EXISTS (SELECT 1 FROM friend_block ...) LIMIT ?`。不动 schema,但这条 DELETE 会去锁守卫行,**必须在 `friend_repo.go` 顶部的锁序说明里补一段**,并纳入 `friend_guard_lock_order_mysql_test.go` 的并发回归(否则就是给自己造一个新的 ABBA 来源)。

无论哪条:删掉一行 `friend_capacity` 都是**可自愈**的 —— `ensureFriendCapacityRows` 补行时从 `SELECT COUNT(*) FROM friend WHERE player_id = ?` 重算初值,不会猜 0,误删不会放宽硬上限。**这一点值得写进 PR 说明。**

### 2.【P2】三条锁序假设从未在真 MySQL 8.4 上 `EXPLAIN` 核对

**位置**(均在 `go/friend/internal/data/friend_repo.go`):`blockedEitherWay`(`SELECT 1 FROM friend_block WHERE (…) OR (…) LIMIT 1 FOR UPDATE`)、`friendEdgeExistsForUpdate`(同形 `OR ... FOR UPDATE`,表是 `friend`)、`lockCapacityRows`(`... WHERE player_id IN (...) ORDER BY player_id FOR UPDATE`)。已登记处:`docs/design/friend-port-20260918.md` §7 步骤 ④b。全仓 `go/friend/**` 零 `EXPLAIN` / `index_merge` / `filesort` 断言(已 grep)。

**为什么是问题**:这是"任何写事务在拿到容量守卫之前不得做任何锁定读"这条书面保证里**唯一不靠 SQL 文本、而靠优化器选择兜底**的一环。
- 前两条的 `OR` 形式必须走 `index_merge` / `range` 且 **key 含 PRIMARY**。退化成全索引扫时,`FOR UPDATE` 的锁集**当场越出守卫域** —— 守卫只串行化这一对玩家,锁集却扩到别人的行,ABBA 立刻可能成环。
- `lockCapacityRows` 必须 `type=range, key=PRIMARY` 且 **Extra 里没有 `Using filesort`**。`ORDER BY` 是**结果序**不是**取锁序**:走成全表扫 + filesort 时 InnoDB 按**表序**取锁,升序纪律与整个防 ABBA 的依据**一起失效**,而代码、测试、注释全都看不出来。

`friend_guard_lock_order_mysql_test.go` 的五个场景是在真 MySQL 上跑的,理论上锁序失效会表现成 1213 —— 但那五个场景都是小数据量,优化器在小表上的选择与线上大表不同,**绿灯不能替代 `EXPLAIN`**。

**建议修法**:不改代码,按 §7 ④b 执行 —— 起一个 MySQL 8.4(**不是 TiDB**,TiDB 无间隙锁、这条路径在它上面恒绿),用真实数据量对三条 SQL 各跑一次 `EXPLAIN`,把**输出原文**贴进交付说明。不达标再回来改 SQL 形状(例如把 `OR` 拆成 `UNION ALL` 的两条等值查询)。可选加固:在那三个函数的注释里写上"本条依赖的 EXPLAIN 形态"并注明核对日期。

### 3.【P3】`AddFriendRequest` 的 pending 计数并发不变量只靠注释守着

**位置**:`(*FriendRepo).AddFriendRequest`(`friend_repo.go`)步骤 ⑤⑥ 的两条 COUNT,**刻意不带 `FOR UPDATE`**,论证写在紧邻的注释里。

不加 `FOR UPDATE` 是**对的**(加了会引入跨玩家对的新死锁环:两条 COUNT 的加锁集合分别是"`from_player_id=A` 的行"与"`to_player_id=T` 的行",维度不同、不存在统一全序,容量守卫拦不住 —— 注释里给了完整的 TRX1/TRX2 反例)。但它的正确性因此挂在一条**不变量**上:

> 任何会让 pending 计数**增加**的写路径,都必须先持有对应玩家的容量守卫行。

今天成立(能让计数变大的只有 `AddFriendRequest` 与 `AcceptFriend`,两者都先 `lockCapacityRows`)。**这条不变量没有任何测试。** 下一个人新增一条写 pending 的路径而不取守卫时,这两处判定会**静默退化**成 check-then-act:上限被并发穿透,零报错、零日志。

**已核实的覆盖缺口(更正一条早期说法)**:`friend_guard_lock_order_mysql_test.go` 里共 **5 个**场景(不是 4 个),其中 `TestAddFriendGuardBeforeLockingReads_DistinctTargets` **不**共享容量行;它测不到该不变量的真正原因是**从空表起跑、没有预置交叉 pending 行**,所以注释里那个 ABBA 环在现有测试里**结构性不可能出现**。

**建议修法**:在该文件加一条用例 —— **预置交叉 pending 行**(例如直写 `(A,C)` 与 `(B,T)` 两条 `status=1`),再让两个**不相干 pair** 并发 `AddFriend(A→T)` 与 `AddFriend(B→C)`,断言无 1213、两条都成功、`friend_count` 与边数一致。这条用例的价值是**双向**的:今天应当绿(说明不加 `FOR UPDATE` 是对的),谁哪天"顺手补上 `FOR UPDATE`"时它会立刻红。

### 4.【P3】`metrics` 里的第三份 sweep 模式字面量,现有对齐断言够不到

**位置**:**不可导出**的 `sweepModeReportOnly` / `sweepModeDelete`(`go/friend/internal/metrics/metrics.go`),用在 `register()` 的预建循环,写侧是 `SetSweepPendingRows`。另两份是 `config.SweepModeReportOnly/Delete`(`internal/config/config.go`)与 `data.SweepModeReportOnly/Delete`(`internal/data/sweep_repo.go`)。够不到它的断言是 `TestSweepModeConstantsAreTheWireLiterals`(`go/friend/internal/logic/friend_logic_test.go`,它自己的注释就写明"本断言够不到第三份 —— 它不可导出,而 metrics 刻意保持叶子包。已登记给 F3 处理")。

> 补充:生产代码里是三份;**测试代码里还有第四份** —— `sweep_repo_test.go` 的 `testSweepModeReportOnly` / `testSweepModeDelete`(data 包不能 import config,只能再抄一次)。

**为什么是问题**:漂移时的表现是**静默的** —— `register()` 预建的 0 值序列落在一个 label 上、`SetSweepPendingRows` 写的是另一个。而预建 0 值序列的**全部意义**就是让"序列缺失 = 抓取出问题"与"值为 0 = 没积压"可区分。两个 label 一旦对不上,**"sweep 根本没在跑"这个唯一信号静默失效** —— 而 sweep 是本域唯一会删玩家数据的路径。

**建议修法**(推荐 1):① 把 `metrics` 这两个常量**导出**,在 `friend_logic_test.go` 的现有断言里加两行 `assert.Equal(t, config.SweepModeReportOnly, metrics.SweepModeReportOnly)`(logic 本来就 import metrics,不成环);② 或让 `SetSweepPendingRows` 改吃**枚举型**而不是 `string`,让编译期兜住。顺手把 `friend_logic_test.go` 里"已登记给 F3 处理"那段注释改掉,否则下一个人还会再读一遍。

### 5.【P3】三个零调用方的导出方法,`AreFriends` 是个真陷阱

**位置**(均在 `go/friend/internal/data/friend_repo.go`):`(*FriendRepo).AreFriends`、`HasPendingRequest`、`CountOutgoingPending`,三者共用的说明是"事务外的非权威探针,只配 logic 层做快速失败用"。已 grep 全仓 `--include=*.go`(排除 vendor 与生成物):除定义处外,只有 `recommend_repo.go` 的一条**注释**提到 `HasPendingRequest`,**零调用方**。已登记处:`docs/design/friend-port-20260918.md` §6 第 13 条。

**为什么是问题**:`AreFriends` 走的是 `GetFriendList` 的**好友列表缓存**,即**最多陈旧一个 `Friend.CacheTTL`** —— 默认 **30 分钟**(`internal/config/config.go`、`etc/friend.yaml`)。它的名字却像一条权威判定。下一个人拿它当 `AddFriend` 的前置门禁,就把 `AddFriendRequest` 里那个"容量守卫内复核"的权威事务**降级成 check-then-act** —— 而权威路径里对应的函数叫 `friendEdgeExistsForUpdate`,名字完全不提示"另一个更弱的同义词存在"。

**建议修法**:`AreFriends` → **删,或改名**为 `AreFriendsCachedApproximate` / `AreFriendsMaybeStale`;若保留,在 doc comment 首行就写"**非权威,最多陈旧 CacheTTL**,权威判定用 `friendEdgeExistsForUpdate`"(目前这句在函数名之后,IDE 悬浮时容易被略过)。`HasPendingRequest` / `CountOutgoingPending`:**直接删** —— 它们要做的事 `AddFriendRequest` 的步骤 ④⑤ 已在守卫内做了,留着只是多一份会漂移的第二实现。
**注意**:删之前先确认没有通过接口断言间接引用 —— 本轮 grep 覆盖全仓 `.go` 零命中,但 `go build` 从未跑过,**以编译器为准**。

### 6.【P3】三处缺测试

已核实:`go/friend/internal/data/` 下只有 `block_repo_mysql_test.go` / `friend_guard_lock_order_mysql_test.go` / `friend_repo_mysql_test.go` / `sweep_repo_test.go` —— **没有** `session_reader_test.go`、**没有** `recommend_repo_test.go`;`internal/logic/` 下只有 `friend_logic_test.go`,其 25 个 `Test*` 里没有一个碰 `StartSweep` 的 ticker。

**6a. `session_reader.go`(最划算,miniredis 就够,不需要 MySQL)**。要钉的四条行为:
- **分批**:`(*SessionReader).BatchOnlineStatus` 按 `batchSize` 切块,`batchSize` 取 `Friend.ListReadHardLimit`,传 0 时回落 `defaultSessionBatchSize = 256`。用 >256 个 id 验"真的切了多批、结果合并完整"。
- **MGET 空串**:缺席 = 离线(零值),不是错误。
- **proto 解码失败降级**:坏 payload 不能把整批拖成错误。
- **只认 `SESSION_STATE_ONLINE`**:显式排除 `DISCONNECTING`(断线待重连的租约期)。这条是**业务语义**,极易被后来者"顺手放宽",必须有测试钉住。

**6b. `recommend_repo.go` 的三段 SQL(需要真 MySQL)**:`RecommendByMutual` / `RecommendRandom` / `recommendAnchor`,排除子句在 `recommendExcludeClause`。要验**列名拼写**与**排除集**(自己 / 已是好友 / 双向拉黑 / 已有申请)—— 拼错列名在 Go 侧完全静态无感。
⚠ **已登记的假绿**:robot `friend-smoke` 第 5 步"推荐不含已拉黑的 C"在三账号数据集下结构性不可能失败,**不要把冒烟的绿当成 `friend_block` 排除生效的证据**(见 §2 第 9 步)。

**6c. `logic/sweep.go` 的 ticker 侧**。职责分界写在文件头(本文件只管节拍 + 抖动 + `safego` panic 边界 + 单轮预算 + 指标与日志;模式判定与保险在 SQL 侧)。`sweep_repo_test.go` 已把 SQL 侧钉得很死,**ticker 侧零覆盖**:未知 mode 的错误日志分支、单轮预算 `sweepRoundBudget`、`ctx` 取消即退出、`SetSweepPendingRows` 被喂进去的 mode 值(与第 4 条联动)。用假 `SweepStore`(接口定义在 `sweep.go`)+ 注入的 ctx 即可,不需要库。

**改动面**:新增 2–3 个测试文件;**miniredis 不用再加** —— `go/friend/go.mod` 的 require 块里已经有 `github.com/alicebob/miniredis/v2 v2.35.0`(`friend_repo_mysql_test.go` / `friend_logic_test.go` 已在用)。(**注意** `go.mod` 顶部的纪律注释:`go mod tidy` 之后 `github.com/redis/go-redis/v9` 只应出现在 indirect 块,别顺手接受 tidy 把它提上去)。

### 7.【P3】`data.FriendEntry.LastActiveMs` 是无写入方的死字段,且与邻近注释自相矛盾

**位置**:`data.FriendEntry.LastActiveMs`(`go/friend/internal/data/friend_repo.go`,`int64` + json tag `last_active_ms`),邻近注释有一句"**不进缓存**:它是每次请求都会变的展示态"。唯一的 MySQL 装载点 `loadFriendListFromMySQL` 的 SQL 是 `SELECT friend_player_id, since_ms FROM friend WHERE player_id = ? LIMIT ?`,Scan 只扫两列 —— **该字段没有任何写入方**。logic 侧读的是**另一个类型**:`(*FriendLogic).GetFriendList`(`internal/logic/friend_logic.go`)里 `entry.LastActiveMs = st.LastActiveMs` 的 `entry` 是 `*pb.FriendEntry`、`st` 是 `data.OnlineStatus`;`recommend.go` 同理写的是 `*pb.RecommendCandidate`。→ `data.FriendEntry.LastActiveMs` 在生产代码里**零写入、零读取**。

**为什么是问题**:缓存回填走 `loadVersionedFriendCache` 的 `json.Marshal(value)`,**整个 `FriendEntry` 连同 `last_active_ms` 一起被序列化进 Redis**。所以"不进缓存"那句今天只是**碰巧成立**(值恒为 0),结构上没有任何东西拦着 —— 谁哪天给它赋了值,那个展示态就会被写进 **30 分钟 TTL** 的缓存,玩家看到的"最后活跃"会冻在半小时前,而注释还在声称这不可能发生。次要代价:每条好友白占约 20 字节缓存空间。

**建议修法**:**删掉该字段**,并把邻近注释改成"在线态与最后活跃时刻都不在本结构里 —— 它们由 logic 层从 `session_reader` 现取,见 `logic/friend_logic.go` 的 `GetFriendList`"。删之后编译一次即可确认无人引用。
⚠ **副作用**:改了 JSON 形状,**存量缓存里的旧 payload 仍带 `last_active_ms` 键**。`encoding/json` 默认忽略未知字段,所以解码兼容、不需要刷缓存 —— 但请在 PR 说明里写明,免得下一个人以为要清 Redis。(这条基于 `encoding/json` 的标准行为,未在本仓实测。)

### 8.【P3】`friend_table.proto` 里 `updated_ms` 的注释**现在是错的**

**位置**:`proto/friend/friend_table.proto` 的 `message FriendRequestRecord` 的 `uint64 updated_ms = 5;`,注释块原文两句现已失实:

> ⚠ 本列**当前没有任何写入方**……在写者落地(F2 批)之前,`(status,updated_ms)` 索引**不能**用于保留期判定……同样在那之前不得把 `Sweep.Mode` 配成 `delete`。

**写入方已全部落地,逐条核实过(是五处,不是四处)**:

1. `AddFriendRequest` 的 upsert(`friend_repo.go`,`INSERT ... ON DUPLICATE KEY UPDATE ... updated_ms=VALUES(updated_ms)`)
2. `AcceptFriend` 的**正向** UPDATE(同文件)
3. `AcceptFriend` 的**反向** UPDATE(同文件)
4. `RejectFriend` 的 CAS(同文件)
5. `Block` 取消双向 pending(`go/friend/internal/data/block_repo.go`)

旁证:`go/friend/internal/logic/sweep.go` 的注释已正确写着"F2 已补齐四个写路径";`friend_repo_mysql_test.go` 的 `TestAddFriend_WritesUpdatedMs` 逐个钉了这几处。

**为什么是问题**:`friend_table.proto` 是这四张表的**唯一事实源**,下一个人排查 sweep 时第一站就是它。读到"本列当前没有任何写入方"会直接得出**"delete 永远不能开"**的结论,而真实结论应当是"**delete 的剩余前置只有一条:从旧共享库 `mmorpg` 搬过来的存量行 `updated_ms` 会是 0**"(搬迁必须显式赋值,建议取 `request_time_ms`)。SQL 侧已为此留了保险:`SweepTerminalRequests` 发现"终态且 `updated_ms=0`"的行时只统计不删并打错误日志(`sweep_repo.go`,日志里连存量修法的 SQL 都给了)。

**建议修法**:把那段注释改写成"写入方已齐 + 剩余前置是存量搬迁 + SQL 侧已有保险"。F3 批当时禁止改 `proto/friend/**`(所以留到了现在,见 `docs/design/friend-port-20260918.md` §6 第 5 条),**该禁令已随 F3 合并结束**。
⚠ **前置**:改 proto 即便只动注释,也请与 proto-gen 那一步放在同一个窗口里做,并核对生成物 diff 只有注释变化(该文件不进客户端 `gen_proto.ps1` 清单,见文件头,所以不牵动 Unity)。
另注:`etc/friend.yaml` 的 `Sweep.Mode` 仍是 `report_only`;改 `delete` 前建议先在 `report_only` 下观察一轮 `friend_sweep_pending_rows`。

---

## §4 还没开工的后续

### 4.0 friend 自己还欠一刀(会卡住后面所有批次)

见本文开头与 §2 第 2、3 步:`go/friend` 现在编译不过,根因是导表器与 proto-gen 没跑。**接手人第一件事就是这个。** 后续任何新服务都排在它之后。

### 4.1 mail(邮件)—— 下一个服务,设计已摸底,一行代码没写

**现状核实**:`proto/mail/` **不存在**,`go/mail/` **不存在**;节点枚举**已有**(`proto/common/base/node.proto` 的 `MailNodeService = 8`);Tip 段**未开**(`data/tip/Tip.xlsx` 里只有注释占位行 `// 以下号段为移植域预留…` + `mail_error 17000 / chat_error 18000 / auction_error 19000 / rank_error 21000 / dialogue_error 22000 / battle_result_error 23000 / grant_error 24000`);`tools/proto_generator/protogen/etc/proto_gen.yaml` 的 `proto_directories` / `proto_dirs` 里**已有** `mail: "mail/"`(历史遗留占位),但 `domain_meta` 段**没有** mail 条目。

**范围(M1)**:只做**系统邮件 + 个人邮件**(列表、已读、删除、过期清理)。**领取走短路**,直接回 `MailClaimNotOpen`,一行也不写。独占库 `mmorpg_mail`,表以 `proto/mail/mail_table.proto` 为源(照 friend / trade 的 D-14 口径)。

**⚠ 为什么领取必须短路 —— 这一条更正了早期的说法**。早期摸底写的是"附件发放依赖帮会二期的通用资产账本,而那个还没开工"。**这句不成立**:通用资产通道**已经落在 main 上**(`proto/common/asset/asset_op.proto`、`proto/common/component/asset_op_ledger_comp.proto`、`go/shared/assetop/` 全套含单测、`scene_node_service.proto` 的 `rpc AssetDebit` / `AssetCredit`、`cpp/libs/services/scene/player/system/asset_op_auth.cpp` 与 `asset_op_system.cpp` 及 `cpp/tests/currency_test/`)。
**真正的阻塞点是白名单**:`asset_op_auth.cpp` 的 `kAssetOpCallerRules` 只登记了 `guild` 与 `trade` 两个调用方,同文件注释原话:"SYSTEM_CREDIT(邮件 / GM 发物,D1 预留)在 v1 没有合法调用方,故意不出现在表里";`asset_op_auth.h` 的 `kCallerNotAllowed` 注释、`go/shared/assetop/types.go` 的 `ApplyRPCOf` 注释、`asset_op.proto` 里 `ASSET_OP_STREAM_SYSTEM_CREDIT = 5` 的注释都是同一口径。
**所以接手人要做的不是"等帮会二期把账本写出来",而是"开 SYSTEM_CREDIT 这条流"**:在 `kAssetOpCallerRules` 补一行 mail 调用方 + 一把独立密钥环境变量,Go 侧接 `assetop` 的 seq 分配与 reconcile。这是 C++ 改动 + 重编 scene,属于另一批,所以 **M1 仍然按短路做**是对的。

**其它前置(逐条核实过)**:

1. **Tip 段**:把预留注释行换成组头 `//mail_error base=17000 width=1000`。新组会生成新 `.proto`,按 `docs/design/tip-code-axis.md` 的"新开一个域"那段,要同步 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`(以及配套的 `.vcxproj.filters` —— 历史上漏过)。
   ⚠ 号段归属有一处**文档不一致**:`docs/design/jubaozhai-market.md` 的 J-7 写"17000–19999 留给 mail/chat/rank",而 Tip.xlsx 的预留注释把 rank 钉在 **21000**。**以 Tip.xlsx 为准**(它是导表器的输入)。
2. **号段 `biz_tag = mail` 要在四处同加**(代码里自己写明"必须一致"):`go/data_service/internal/config/config.go` 的 `DefaultIdSegmentBootstrapTags`、`go/data_service/internal/store/id_segment_store.go` 的同名常量、`go/data_service/etc/data_service.yaml` 的 `IdSegment.BootstrapTags`、`tools/scripts/k8s_deploy.ps1` 的 data-service ConfigMap。现值四处一致 `[player, guild, item, txlog, snapshot, trade_listing]`。**friend 没有用号段,`mail` 会是第一个新 tag。** 另:`config_test.go` 有 `EffectiveBootstrapTags` 的断言,改清单要一起改测试。
3. **建库**:`deploy/mysql-init/00_init_zone_dbs.sql` 加 `mmorpg_mail` 的 CREATE + GRANT(照 `mmorpg_friend`)。
4. **五处登记**:照 friend 已落地的样板(§1 的 F3 段)。
5. **⚠ guild 没有 K8s 登记**:`k8s_deploy.ps1` 的 `$GoSvcCatalogue` 只有 db / data-service / login / player-locator / scene-manager / match / chat / client-rpc-router / trade / friend —— **没有 guild**,`deploy/k8s/manifests/go-svc/` 下也没有 `guild.yaml`。M2 的公会邮件要经 guild gRPC 取成员,**在 K8s 上现在拿不到 guild**。这是 M2 的硬前置,M1 不受影响。

**端口**:mail 50900 / metrics `:9240`(`-Zone 2` 位移后 51900,不落本机 51573–51872 保留区)。**来源只有会话 scratchpad 的排批稿,没有进任何代码或仓内文档**;可交叉核实的只有"这组端口全仓未占用"以及"friend 的 50400/:9180 按同一份表落地"。若要正式采用,应写进契约 §7 的端口分工表。

**规模**:摸底估 20 人日 / 46 文件,超过 `AGENTS.md §10.2` 的 30 文件门禁,**必须再拆 2–3 批**。摸底产物 `scratchpad/port_plan_mail.json` 是**待评审的提案,不是拍板**(里面 11 条 `decisions_needed` 都没有用户确认)。

### 4.2 leaderboard(排行榜)—— 本轮只能出设计文档,不能落码

**现状核实**:`proto/rank/` 与 `go/rank/` 都**不存在**;节点枚举**已有**(`RankNodeService = 13`);Tip 段**未开**(只有 `rank_error 21000` 预留注释);proto-gen `domain_meta` 没有 rank。B 仓现有的榜只有公会榜:`go/guild/internal/data/guild_repo.go` 的 `guildRankKey = "guild_rank"`(Redis ZSET,member=guildID,注释写明"Score 是公会排行分的 MySQL 权威副本;Redis ZSET 只是读加速层"),并有 `RebuildRanks`。

**⚠ 更正:`PlayerRatingChangedEvent` 不存在**。早期说法是"它要消费 match 的 `PlayerRatingChangedEvent`" —— 全仓(含 A 仓浅克隆)grep `PlayerRatingChanged` **零命中**。`proto/contracts/kafka/` 下的全部 message 是:`GateCommand`、`RoutePlayerEvent`、`KickPlayerEvent`、`PlayerDisconnectedEvent`、`PlayerLeaseExpiredEvent`、`BindSessionEvent`、`RedirectToGateEvent`、`PushToPlayerEvent`、`BroadcastToPlayersEvent`、`BroadcastToSceneEvent`、`BroadcastToAllEvent`、`BindBattleEvent`、`UnbindBattleEvent`、`PlayerLifecycleCommand`、`SceneCommand`、`BattleResultTeam`、`BattleResultEvent`。**没有任何 rating 相关事件。**

实际的评分形态(`go/match/internal/logic/rating.go`):评分存在 Redis hash `match:rating:{player_id}`(字段 `rating` / `games` / `updated_at_ms` / `recent_battles`,`defaultRating = 1500`,`eloK = 32`);写分入口是 `ApplyBattleResult(svcCtx, *kafkapb.BattleResultEvent)`,由 `go/match/internal/kafka/result_consumer.go` 消费 topic `match-results` 触发;逐人增量 Lua(`ratingApplyScript`)+ 两层幂等。**match 不对外发布任何评分变更事件。**

**所以 leaderboard 的真实阻塞是:写分来源这条东西向边根本不存在,要新造。** 两个候选,都得先拍板:
- **(a)** 让 match 新增一条出站事件(带 battle_id 做幂等键)→ 要改 `go/match`;
- **(b)** leaderboard 自己也消费 `match-results` 的 `BattleResultEvent` → 但 Elo 的 Δ 是在 match 进程内算的(`applyRatingDelta` / `eloExpected` / `teamAverageRating`),leaderboard 要么重算一遍(两份真相),要么只能拿到胜负而拿不到分。

`docs/design/microservice-zone-contract-20260914.md` §8「C++ scene → 全局 Go 服务(东西向)」明确规定:路由服不是东西向通道(缺会话回 Unauthenticated),内部调用只有 Kafka(带幂等键)或直连 gRPC 配专用 READY 选择器两条路,后者还要补 `nodeTypeNameMap`、两份前缀表、scene 白名单。**leaderboard 的写分路径必须落在这两条之一。**

**⚠ 更正:"go/match 归组队会话所有"已过期**。`E:/work/xuanming-server-mmo-team2` 与 `-team` 两个 worktree 目录**都已不存在**,组队已于 09-18 全量合并进 main(`go/match/internal/team/` 在 main 上)。改 `go/match` 不再有 worktree 冲突,但**组队那批代码同样从未编译、从未测试**,叠加改动前先把它编译过一次。另外 `go/team/` 下**只有 `generated/`**(实现在 `go/match/internal/team/`),`go/battle/` 同样只有 `generated/`;**team 也还没进 `go_services.ps1` 的 `$ServiceCatalogue`,更没进 k8s** —— 这是组队那批自己的尾巴,会影响任何依赖 team 的排期。

**端口**:rank 51000 / metrics `:9250`(同上,来源与效力同 mail)。
**本轮交付物**:**只写设计文档,1 个文件**。v1 范围建议:只做 ZSET 榜 + 客户端读;`SettleBoard`(发奖)等 SYSTEM_CREDIT 白名单开了再说;GUILD scope 继续留在 `go/guild`,不搬。Tip 的 21000 组头放到 leaderboard 的首码批再开。

### 4.3 Unity 客户端的好友功能 —— 另立客户端任务

> ⚠ **客户端是同级独立仓库 `E:/work/mmorpg-client`,未获授权不得读写。** 依据:仓根 `AGENTS.md` §9 原文 —— "❌ 客户端统一使用同级独立仓库 `../mmorpg-client/`;服务端不再维护 `client/` 目录,未获客户端任务授权时不要读取或修改独立客户端"。本轮只确认了该目录存在。

**服务端已定死的契约**(逐字核自 `proto/friend/friend.proto`):服务名 `ClientPlayerFriend` 带 `OptionIsClientProtocolService`;S2C 方法 `rpc NotifyFriendEvent (FriendEventS2C) returns (Empty);` 与 10 个 C2S 方法**同处一个 service**;消息体 `FriendEventS2C { FriendEventReason reason = 1; uint64 by_player_id = 2; int64 ts_ms = 3; }`;`reason` 只有两个有效值(加 UNSPECIFIED=0):`FRIEND_EVENT_REASON_REQUEST_RECEIVED = 1`、`FRIEND_EVENT_REASON_REQUEST_ACCEPTED = 2`。

**客户端要写的逻辑**(proto-gen 只生成 handler 桩,桩里什么都不做):

1. **UI**:好友列表、待处理申请、黑名单、推荐好友。注意本项目**已弃用 FairyGUI,新 UI 一律 UGUI**。
2. **"收到推送就去拉"**:proto 注释原文口径 —— 推送经 `kafkautil.PushToPlayer` → `gate-cmd_g<N>`,**at-most-once**,玩家刚好掉线时这条事件会丢,所以推送只用来"立刻刷红点",真相仍要靠 `GetPendingRequests` 拉取。契约 §5 是同一条口径。
3. **登录后拉一次 + 打开好友面板时再拉一次**。**这才是跨区传送窗口的真正兜底**:传送期间 gate 绑定在换,窗口内发出的推送会丢,只有主动拉取能补回来。**不要把红点的正确性建立在推送上。**
4. **不要调 `NotifyFriendEvent`**:它和 C2S 同 service,服务端在会话拦截器的方法白名单里**刻意排除**了它(否则客户端可以直接伪造一条好友事件),服务端也不实现它(继承 Unimplemented)。客户端只做接收方。
5. **限频**:见 §2 第 4 步 —— 不配档位就吃 gate 默认 3 次/窗口,且配档位必须排在 proto-gen 之后。

**服务端侧的验收替代手段**:在客户端任务开工前,服务端批次按契约 §11.2 用 robot `friend-smoke` 代替 D-1 判据的客户端部分。

### 4.4 A 仓还有哪些服务没移植

A 仓浅克隆(只读):`C:/Users/luyua/AppData/Local/Temp/claude/E--work/e33631a7-2f38-4d8a-af69-16d5387200d5/scratchpad/xuanming-server/`。
⚠ **scratchpad 是会话级临时目录,随时可能被清掉。** 发现它不在了就重新浅克隆 A 仓再核;下表的**本仓侧**结论与它在不在无关。

`services/` 下按域分组共 **21 个叶子服务**:

| A 仓服务 | 本仓对应 | 状态 | 判定 |
|---|---|---|---|
| `account/login` | `go/login` | 有(B 原生两步登录) | **不搬** |
| `account/player` | 无独立服务,等级/属性/宠物在 C++ ECS | 壳 | 只搬**经验入账**一项增量,整服不搬 |
| `battle/battle_result` | 部分在 `go/match`(Elo) | 战报落库/出箱/保留期都没有 | **暂不开工**,等产品提需求 |
| `battle/ds_allocator` | `scene_manager` 的 Agones 层 | 不适用 | **不搬** |
| `battle/hub_allocator` | 同上 | 不适用 | **不搬** |
| `data/data_service` | `go/data_service` | 有(同名不同义) | **不搬** |
| `economy/auction` | 无 `go/auction` | 缺 | **不建新服务**,随聚宝斋后期在 `go/trade` 内实现 |
| `economy/inventory` | 背包权威在 C++ | 部分 | **不搬**(核心留 C++);"按 guid 全或无扣物 / 幂等发放"已由 `go/shared/assetop` + C++ `asset_op_*` 覆盖 |
| `economy/trade`(玩家间面对面订单) | `go/trade` 是聚宝斋寄售,**不是同一功能** | 缺 | **暂不开工** |
| `matchmaking/matchmaker` | `go/match` | 有 | **不搬** |
| `matchmaking/team` | `go/match/internal/team`(+ `proto/team`) | 已落 main(09-18),**未编译未测试**,未进 `$ServiceCatalogue`、未进 k8s | 已移植,**尾巴未清** |
| `runtime/leaderboard` | 无 | 缺 | **待做**,本轮只出设计文档(§4.2) |
| `runtime/owner` | 三态 `IsNodeAlive` 已落码 | 部分 | **不搬 A**,按 B 自己的设计做 |
| `runtime/player_locator` | `go/player_locator` | 有(同名不同物) | **不搬** |
| `runtime/push` | `go/shared/kafkautil` 经 `gate-cmd_g<N>` | 有 | **不搬** |
| `social/chat` | `go/chat` | 有(v1 已落地) | 已移植;v1.1(推送 / since 游标 / TEAM 频道 / chat 私有 tip 段 18000)未做 |
| `social/dialogue` | 无 proto 也无服务 | 缺 | **前提未拍板,不开工**(NPC 刷点表 / 交互协议 / 距离判定都没人做) |
| `social/friend` | `go/friend` + `proto/friend` | **本轮已移植**,未编译未测试、导表器没跑 | 已移植,**尾巴未清** |
| `social/guild` | `go/guild` + `proto/guild` | 有(按 zone 隔离已落地);**K8s 未登记** | 已移植;二期五块增量未开工 |
| `social/mail` | 无 | 缺 | **待做,下一个**(§4.1) |
| `social/mission` | C++ 任务系统已接通 | 有 | **不搬 A 的 Go mission** |

**明确"不搬"的 A 仓基础设施**(B 有等价物,不要再花时间评估):`ds_allocator`、`hub_allocator`、`owner`、`push`、`data_service`、`player_locator`、`login`、`matchmaker`、`mission`。
**还真正要做的**,按优先级:**mail** → **leaderboard(先设计)** → dialogue / battle_result / P2P trade / auction(都要先拍前提)。

### 4.5 接手人开工顺序建议

1. 跑**导表器**(§2 第 2 步)—— 否则 `go/friend` 编译不过。
2. 跑 **proto-gen**(§2 第 3 步),注意改名会让全仓消息号重新洗牌,gate / 路由服 / robot / 客户端的生成物**必须出自同一次**。
3. 填 `data/MessageLimiter.xlsx`(friend + team 的档位),**再导表一次**。
4. **编译 + 跑测试**:friend 三批、组队那批、聚宝斋 P1、帮会 zone 接入、跨区传送 —— 这些**全都从未编译过**。按 `PROGRESS.md` 与各自设计文档的清单逐个过。
5. `robot friend-smoke` 七步冒烟跑通,friend 才算真的完成。
6. **然后**才开 mail M1。

---

## §5 接手前必须知道的坑

### 5.1 这个仓库同时有多个 Claude 会话在改 —— git 索引是共享的

friend 这批从落码到合完,`main` 上另一批会话一直在推进:`f06090b19` 之后到 `origin/main` 之间,有 **1 次 37 冲突的大合并(`cda218956`,788 文件)+ 6 次后续追平合并**(`d924b2377` / `1eb35babb` / `a5e598179` / `88a262f2f` / `52f99780e` / `5c574bc0b`)。(早期口述说"main 前进了 5 轮",实际按合并提交数是这个形状。)

这些会话共用**同一个工作树、同一个 git index**。别人 `git add` 过但还没 commit 的文件,就躺在你即将提交的索引里。更糟的是还有一个**每小时 `git add -A` 的自动保存会话**:`git log --oneline` 全量里有 **21 个** `WIP:每小时保存全部进度`(2026-09-20 复核时的数;一天前是 20 个,它每小时还在长),最近 30 个提交里占 5 个。它在 `add` 与 `commit` 之间的窗口会卷走任何人暂存的东西。三个已核实的实例:

- `105c5560b`(WIP 03:50)混进了 `data/tip/Tip.xlsx`(14231→14425 字节)、`data/RoleNameRule.xlsx`、`go/shared/playername/` 等**别人正在做的帮会二期 B3a** 内容;
- `969bbec4c`(WIP 09-18 03:56)混进了 `go/friend/` 的文件;
- **标题与内容对不上的例子**:`10538f5f5` 标题是「regen:trade_table 的 resolved_by / resolve_reason 落到生成物(未编译)」,实际内容包含 `docs/ops/grafana-loki-local-logs.md`(+299)、`tools/scripts/k8s_deploy.ps1`(+276)、`deploy/k8s/manifests/infra/loki.yaml`、`PROGRESS.md` —— 整段 Grafana/Loki 的工作被卷了进去。

**怎么防**:

1. **动手前 `ListAgents` + `SendMessage` 问归属**,而且要**明确问**"你有没有**未提交的、和我重叠的文件**" —— 只问"你在改哪个模块"不够,索引是按文件卷的。
2. **提交一律路径限定**:`git commit -F <msgfile> -- go/friend/ proto/friend/ ...`。**永远不要 `git add -A` / `git commit -a`。**
3. **提交前 `git diff --cached` 逐行看**,不是只核对文件名。同一个文件里可能既有你的行、也有别人的行(尤其 `k8s_deploy.ps1`、`go_services.ps1`、`PROGRESS.md` 这几个"人人都改"的文件)。
4. 对方会话 idle 很久**不要假设它死了** —— 可能是撞额度,会 resume,并接着用它记忆里的那份索引状态。
5. 接手时先 `git fetch && git log --oneline origin/main..main`:**这个数每天都在变,别把本文的数字当现状**:2026-09-19 核实时 ahead 4(`ab5e7f96d` / `33ba69e20` / `2df78d4a5` / `a5ca66851`,都是别人的 Kafka / Redis / battle 改动);2026-09-20 复核时那 4 个已被推上 `origin/main`,本机改为 ahead 1(一个新的自动保存 WIP)。**别顺手 `git push` 把别人没准备好的东西推上去。**

### 5.2 3-way 合并干净 ≠ 能编译(本次最隐蔽的一个坑)

`cda218956` 那次 37 冲突的合并解完后 git 一声不吭、零冲突残留,但 `tools/merge_zone/audit_resources.go` 里新写的 `validateFriendSchemaName` 引用的 `tradeSchemaNamePattern` 在合并后的树里**已经不存在了** —— 另一个会话(帮会二期 B1b)把那份正则从 `trade_step.go` **提到公共位置 `player_rows.go` 并改名成 `schemaNamePattern`**。两边改的不是同一行、不是同一个文件,3-way 合并按行比对自然判"无冲突",但语义上引用已悬空。**git 只保护行,不保护符号。** 追加的修复是 `57af3fd09`。

现状:`tools/merge_zone/player_rows.go` 里 `var schemaNamePattern = regexp.MustCompile(...)`,被三处共用(`validateFriendSchemaName` / `validateGuildSchemaName` / `validateTradeSchemaName`)。**它是库名拼进 SQL 之前唯一的注入防线,不要再抄第四份。**

**怎么防**:合并之后,**把"我新增行里引用的包内标识符"提取出来逐个 grep 确认包内有定义**(没有编译器时这是唯一能抓到这类问题的办法)。更稳的是**双向互扫**:你扫"我删掉/改名的符号对方还有没有引用",让对方扫反方向 —— 单向只能抓一半,本次就是靠反方向那一半才发现的。重点盯"被提公共"和"被改名"两类动作。

### 5.3 解冲突时"两边都保留"的机械做法会吃掉闭括号 / 交错拼接块

冲突标记的边界**不等于**语法块的边界:一个 `if {` 在 ours 里、它的 `}` 在冲突区外,"两边都留"就变成"两个 `{` 一个 `}`"。没有编译器时这类错误**完全静默**。同一轮解冲突里连踩过三次。

**判据要对,不然白验**:
> ⚠ **括号配平要比"与两个父版本的差值",不是比绝对值。** 生成物文件、含括号字符串的文件,绝对差本来就不是 0。正确姿势:`git show :2:<path>`(ours)、`git show :3:<path>`(theirs)取两个父版本分别算配平值,再和解完的版本比**差值** —— 差值应等于你有意增删的量。

- **PowerShell**:用真解析器,不要数括号:
  ```powershell
  $e=$null;$t=$null
  [System.Management.Automation.Language.Parser]::ParseFile('tools/scripts/k8s_deploy.ps1',[ref]$t,[ref]$e)
  $e   # 空 = 语法上成立
  ```
- **Go**:没有 `go vet` 时用"剥掉字符串与注释后配平 + 顶层声明起点检查"。本次写的脚本在 `C:/Users/luyua/AppData/Local/Temp/claude/E--work/e33631a7-2f38-4d8a-af69-16d5387200d5/scratchpad/gocheck.py`:
  ```bash
  PYTHONIOENCODING=utf-8 python <上面那个路径> go/friend/friend.go go/friend/internal/data/friend_repo.go ...
  ```
  (不设 `PYTHONIOENCODING=utf-8` 时 Windows 默认 cp1252 会在**最后一行中文汇总**上抛 `UnicodeEncodeError` —— 逐文件的 `OK` / `FAIL` 已经打完,不影响判断,但退出码会变 1,别被误导。)
  它做两件事:① 剥掉行注释 / 块注释 / 解释型字符串 / raw 字符串 / rune 字面量后检查 `{}` `()` `[]` 配平;② 检查是否在 `balance != 0` 的状态下出现顶层 `func ` / `type ` 起点(这是"吃掉一个闭括号"的典型指纹)。
  **核实结果**:friend 这批 8 个主要 Go 文件当前全部 `OK`(`friend.go` / `friend_repo.go` / `block_repo.go` / `friend_logic.go` / `friend_server.go` / `constants.go` / `audit_resources.go` / `friend_smoke_scenario.go`)。**但这只说明结构没被吃掉,不说明能编译**(见开头的悬空符号)。
  > 该脚本在**会话专属临时目录**里,不在仓库内,随时可能被清掉。不在了就按上面两条判据自己重写一个(约 70 行 Python)。

### 5.4 `data/tip/Tip.xlsx` 同时有三个会话在写,而且丢失是**零报错**的

这个文件在 friend 窗口期同时被 friend、帮会二期、跨 zone 传送三条线改。它是 **xlsx 二进制,不能 3-way 合并**;`--ours` / `--theirs` 只能保住一半。**丢失的表现形式不是报错,而是"导表器少发几个号"** —— 表里少了几行,导表器照常跑通、照常出生成物,只是那几个 tip 码不存在了,然后编译在完全不相干的地方报 undefined。

**唯一安全的写法**:① 冲突时以 main 的版本为准(`git checkout main -- data/tip/Tip.xlsx`);② openpyxl 从磁盘 `load_workbook` 重新载入(不要用内存里旧的那份);③ **只在自己那一段 `insert_rows`**,插完 `save`;④ 读回来按段计数核对;⑤ **绝不整表写回、绝不重排、绝不动别人的段**。
xlsx 的改动刻意留到**合并窗口**做、不在分支上做(二进制不能 3-way 合并,在分支上改必然与并行会话打架)。要加 tip 码请沿用这个节奏。

**当前基线**(2026-09-19 核实:`data/tip/Tip.xlsx` 14458 字节,单 sheet `Tip`,291 行 3 列 `name` / `commen` / `fault`)。改完请对着这张表重新核一遍:

| 组头 | 码数 |
|---|---|
| `//common_error base=1000` | 19 |
| `//login_error base=2000` | **35** |
| `//scene_error base=3000` | **28**(ZoneTravel 的 4 个码在这一段:`ZoneTravelTargetZoneNotFound` / `ZoneTravelInBattle` / `ZoneTravelInTeam` / `ZoneTravelTargetBusy` —— **没有独立的 zone_travel 组**) |
| `//team_error base=4000` | 31 |
| `//mission_error base=5000` | 7 |
| `//bag_error base=6000` | 15 |
| `//skill_error base=7000` | 7 |
| `//buff_error base=8000` | 2 |
| `//entity_error base=9000` | 1 |
| `//actor_action_error base=10000` | 1 |
| `//mount_error base=11000` | 1 |
| `//reward_error base=12000` | 1 |
| `//cross_server_error base=13000` | 1(`SceneTransferInProgress`) |
| `//guild_error base=14000` | **22** |
| `//friend_error base=15000` | **10** |
| `//match_error base=16000` | 21 |
| `//trade_error base=20000` | 4 |
| `//attribute_error base=25000` | 15 |
| `//pet_error base=26000` | 17 |
| `//asset_error base=27000` | 9 |

- **组头行共 23 行**,其中 3 行(236–238)是"预留号段"说明行、不是真组头 → **真组头 20 个**。
- **数据行总码数 247。**
- **`friend_error` 段的 10 个码**(顺序即预期发号顺序 15000–15009):`FriendCannotAddSelf` / `FriendAlreadyFriends` / `FriendListFull` / `FriendRequestAlreadySent` / `FriendTargetListFull` / `FriendNoPendingRequest` / `FriendTooManyPending` / **`FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull`**。加粗三个是 `7b48b0a98` 补的,`fault` 列留空,**至今没有被导表器发号**。

### 5.5 `git merge --ff-only` 被拒是安全网,别绕过

工作区里有重叠的未提交文件时 `git merge --ff-only main` 会拒绝执行,很容易被当成"挡路的东西"而想办法绕开。

**为什么不能绕**:真正会造成**静默回退**的,是从**别的 worktree** 用 `git update-ref` / `git branch -f` 去挪 `main` 的引用 —— 那样 `main` 的**工作树和索引仍停在旧提交**,引用却跳到了新的;紧接着每小时的自动保存会话一跑 `git add -A && git commit`,就把你刚合进去的改动**原样回退掉,而且一个错都不报**,在 `git log` 里它看起来就是一个普通 WIP 提交。

**正确姿势**(本次实际用的,可从 reflog 核实):① 在**自己的分支 / worktree** 里反向 `git merge main`,在那里解冲突(那 6 个追平合并的由来);② `main` 这边**只做 `--ff-only`**(`git reflog show main` 里对应的是 `5c574bc0b … merge feature/port-remaining: Fast-forward`(reflog 下标随新提交往后挪 —— 09-19 是 `main@{4}`,09-20 复核时已是 `main@{8}`,**按提交号找,不要按下标找**) —— 干净快进,没有第二个父节点,不可能吞掉别人的提交)。

现状:`git worktree list` 只有一个工作树(`E:/work/xuanming-server-mmo`,`main`),本地只有 `main` 一个分支(`feature/port-remaining` 合并后已删)。**新开工作时请自己再拉一条分支/worktree,不要直接在 `main` 上攒改动。**

### 5.6 friend 特有的三条不变量 —— 改代码时不许破

三条都在 `go/friend/internal/data/friend_repo.go` 顶部那段「**全局锁序与隔离级别(F2 §2;改任何写路径之前必须读完这一段)**」注释里有全文,设计依据在 `docs/design/friend-port-20260918.md` 的 **F6 / F7**。

**(a) 全局锁序:任何写事务在拿到容量守卫之前,不得做任何锁定读。**
守卫本体是 `lockCapacityRows`(`SELECT player_id, friend_count FROM friend_capacity WHERE player_id IN (...) ORDER BY player_id FOR UPDATE`),**升序是防 ABBA 的全部依据**,顺序本身就是正确性、不是风格(升序由 `ascendingUniqueIDs` 保证)。三个写事务(`AddFriendRequest` / `AcceptFriend` / `RemoveFriend`)与 `block_repo.go` 的拉黑事务都是第一把锁就拿它。
**最容易被改回去的那一处**:`AcceptFriend` 里第 ③ 步 `SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=? FOR UPDATE` **必须留在容量守卫之后** —— 它原先排在守卫之前,与 F2 新增的 `AddFriendRequest` 权威事务正好互为 ABBA。
⚠ **不要被"B 仓当初那个 1213"的说法带跑偏**:那次修的是"过早把 `status` 从 1 改成 2 会让并发事务在 `idx_to_player` 前缀上删/插而触发 1213",修法是**把 UPDATE 延后**,**不是**把 SELECT 提前;现在下移的只是一条按 `(from_player_id, to_player_id)` 主键的**单行 SELECT**,它锁那一行、不碰 `idx_to_player` 的范围,而那条 UPDATE 依然在守卫之后。改之前先读第 ③ 步上方的注释。
> **更正**:早期交接口述提到"B 侧有一段老注释讲『先用主键锁定并验证申请』"。**当前 main 上找不到这条字面注释**(`go/friend/`、`friend-port-20260918.md`、`friend-persistence-architecture.md` 全文 grep 均无),它应该在落码时就被替换成了正确的 F6 说明。风险本身仍成立,只是不会再有一条误导人的残留注释。

另外两处**刻意不加 `FOR UPDATE`** 的普通读(`AddFriendRequest` 的第 ⑤⑥ 步两个 pending 计数)**别"顺手补上"** —— 理由见 §3 第 3 条。
隔离级别固定 **READ COMMITTED**(`beginWriteTx`):RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类只碰不同行的事务互相挡,**且只在 MySQL 上炸,TiDB 没有间隙锁恒绿**。"判定读在守卫之后用当前读"在 RC 下**不是可选优化,是正确性要求**。

**(b) 缺容量行时,按 `friend` 表的权威边数建行,绝不猜 0。**
实现在 `ensureFriendCapacityRows`:先 `SELECT COUNT(*) FROM friend WHERE player_id = ?` 得到权威值,再 `INSERT IGNORE INTO friend_capacity (player_id, friend_count) VALUES (?, ?)`。**为什么不能写 0**:将来若有任何路径先写了 `friend` 边再补容量行,猜 0 会让这个玩家的硬上限**凭空放宽一轮**,且**全程零报错**。调用顺序也是不变量的一部分:**先在事务外、按 `player_id` 升序、自动提交地补齐行**,**再**在事务里按升序 `FOR UPDATE` 锁双方容量行。
(原本还有一道「`friend_capacity` 就绪闸」D-10,在 D-14 迁到独占库之后**已退役** —— 它读的台账表留在旧库 `mmorpg`,而 `config.Validate` 断言 `DBName == mmorpg_friend` 且禁跨库,那条查询恒为"表不存在"、闸门恒放行。**退役的只有这道闸,(b) 这条实质不变量原样保留。**)

**(c) 所有删 friend 边的路径,都必须按 `RowsAffected` 减 `friend_count`。**
实现在 `deleteFriendEdges`。**已核实:全仓 `DELETE FROM friend` 只有这一处**,两个调用方是 `RemoveFriend` 与 `block_repo.go` 的拉黑路径,两者都在调用前先 `lockCapacityRows` 拿到双方守卫。
**漏减的后果**:`friend_count` 只增不减、单向上漂,玩家**永远加不满好友**,而且**零报错**。下溢保护写成 `AND friend_count > 0` 而不是 `IF()`,因为 `friend_count` 是 **uint 列,减到负数会回绕成天文数字**;拦截时会打一条 `[friend] friend_count 下溢被拦截` 的 Error 日志 —— **看到这条日志就说明计数已经和边数不一致了,要去查哪条路径漏减。**

**新增任何删边路径时,把这三条当 checklist 走一遍。**

### 5.7 go-zero 的 `GetCtx` 把"键不存在"吞成 `("", nil)`,不是 error

go-zero 的 `core/stores/redis/redis.go` 里,`GetCtx` 遇到 `redis.Nil` 时 `errors.Is(err, red.Nil) → return "", nil`。所以:
- `err == redis.Nil` 这条判未命中的分支**永远走不到**(从 go-redis 抄过来的代码里最常见的写法);
- 更阴的是 generation 那一步:key 从没被写过时读回**空串**,而 Lua 回填脚本按 `"0"` 比较 → CAS 恒失败 → 脚本恒 `return 0` → **缓存永远写不进去**;而 `EvalCtx` 本身成功、返回值本来就被丢弃,**一个错都不报**,纯静默故障。

当前代码(`loadVersionedFriendCache`)有两处对应处理,**都不要当冗余删掉**:① `readCache` 闭包先判 `err`(真故障)**再把空串当未命中**(能这么判是因为 JSON 序列化结果最短也是 `"null"` / `"[]"` / `"{}"`,永不为空串);② generation 那步**显式补 `"0"`**。同一函数还有一条纪律:传给 `EvalCtx` 的 ARGV **一律显式转成 string**(`generation`、`string(payload)`、`strconv.FormatInt(...)`),因为脚本里 `ARGV[1]` 是拿来和 `GET` 回来的 generation 做**字符串**比较的,`ARGV[3]` 要 `tonumber` 得动。

**怎么防**:凡是从 go-redis 往 go-zero 改写的地方都查一遍这一类 —— `GetCtx` / `GetexCtx` 之类"键不存在"的语义在 go-zero 里是**零值 + nil error**。搜索关键词:`redis.Nil`、`errors.Is(err, red.Nil)`、`if err == redis.Nil` 的残留写法。

---

## §6 存疑与未核实项(如实列出)

**本文的全部结论都来自读源码,没有任何一条经过编译器 / 类型检查 / 运行时确认。**

1. **本机没有 Go 工具链**,所以"零调用方""符号不存在""签名对得上"这类结论都只有 grep 支撑。特别是**接口断言式的间接引用 grep 未必抓得到** —— **以首次 `go build ./...` 为准。**
2. **§3 第 2 条的三条 `EXPLAIN` 假设在本机无法核对**(没有 MySQL 实例)。优化器在小表与大表上的选择不同,`friend_guard_lock_order_mysql_test.go` 的绿灯不能替代它。
3. **本机 mysql 容器的实际版本没有实测**(Docker 未启动):`deploy/docker-compose.yml` 钉的是 `mysql:latest` 不是固定 8.4,K8s 侧用的是 `mysql:8.0`。所以 §2 第 6 步写的是"跑之前先 `SELECT VERSION()` 确认是 8.4+ 的真 MySQL,必要时把 image 钉到 `mysql:8.4`",而不是断言本机就是 8.4。
4. **§3 第 1 条的两条回收修法都只是纸面设计**,尤其路线 2 的 DELETE 与现有锁序如何共存(它会去锁守卫行)**未经推演验证**,落码前应由接手人自行重做一遍 ABBA 分析。
5. **§3 第 7 条"删字段不需要刷缓存"**基于 `encoding/json` 默认忽略未知字段这一标准行为,**未在本仓实测**。
6. **proto-gen 全量重跑后 friend 的 C++ 产物是否真会被重写,是推断而非实测**:`proto_gen.yaml` 的 friend 块只声明了 go outputs(与 guild/trade 同形),但 `cpp/generated/proto/{guild,trade,team}/` 都存在,说明 cpp proto 由另一条全局路径产出。同理 `friend_service_metadata.h` 会不会被改名成 `ClientPlayerFriend*` 也只能等实跑。
   顺带发现一条**既有的潜在坑(不是本批引入)**:`jubaozhai_service_metadata.h` 引用的 `::ClientPlayerJubaozhai_Stub` 在 `cpp/generated/proto/trade/jubaozhai.pb.h` 里并不存在(grep 计数 0)—— 那是个宏、只在展开处才会报错。
7. **第 3 步跑完后 `kMaxRpcMethodCount` 会变成多少无法预判**(改名释放 8 个号又新增 11 个方法,发号靠 Go map 迭代顺序)。只有"等于 `message_id.txt` 最大 id + 1"这条不变判据。
8. **robot 与 Unity 新 handler 的确切文件名**是按现有 team 的命名规律推导的(`client_player_friend_notify_friend_event.go` / `ClientPlayerFriendNotifyFriendEventHandler.cs`),**未实跑**。robot 那 11 个 `game.ClientPlayerFriend*MessageId` 常量名同理。
9. **§2 第 4 步的限流档位数值**(读 10/s、写 5/s、推荐 1/s)是建议值,**不是仓内既有约定**,没有代码或文档背书;`data/MessageLimiter.xlsx` 本身没有解包逐行核对 friend 行是否真的不存在(依据是该表自述与"消息号未定"这一事实推断),填表前请自行解包确认。
10. **§4 的端口分配**(mail 50900/:9240、rank 51000/:9250)只有会话 scratchpad 的排批稿一处来源,**没有进任何代码或仓内文档**;可交叉核实的只有"全仓未占用"。
11. **§4.1 mail 的表清单、事务顺序、sweep 口径、20 人日 / 46 文件的估算**全部来自 `scratchpad/port_plan_mail.json` 这份摸底产物,**仓库里没有任何对应代码或设计文档可交叉核实**,其自身 11 条 `decisions_needed` 也没有拍板记录。**当作待评审提案,不是既定设计。**
12. **§4.2 "leaderboard v1 只做 ZSET 榜 + 客户端读"属于建议而非拍板** —— 仓内没有 leaderboard 设计文档。
13. **§2 第 8 步"经 gate 调一次 GetFriendList"的具体手法未核实**:`Mode: dev` 下 friend 开了 gRPC reflection,理论上 grpcurl 可直连 50400,但 session 拦截器对"无会话 metadata"的处理是"内部调用放行",语义与经 gate 的真链路不同。建议直接用第 9 步的冒烟触发。
14. **没有对 `E:\work\mmorpg-client` 做完整核实**(只读了 `tools/gen_proto.ps1` 与 `Assets/Scripts/Net/Generated` 的目录清单);客户端侧若还需要别的登记(如 `HandlerRegistry.cs` 之外的手写接线),本文未覆盖。
15. **§5.3 "解冲突时连踩三次吃括号"是过程性事实,无法从 main 的现行代码反查**(冲突已解、痕迹不在 git 历史里);"括号配平比差值"这条判据本身也未实跑。PowerShell 解析器那条手段本轮**没有实跑**。
16. **"每小时 `git add -A` 的自动保存会话"**:WIP 提交的存在、间隔与跨模块内容都核实了,但"它用的是 `git add -A`"是从提交内容横跨多个不相关模块推出的**推断**。
17. **§5.5 "从别的 worktree 用 `update-ref` / `branch -f` 挪 main 会造成静默回退"**这条机理**没有在本仓复现**,reflog 里也没有该类条目 —— 它作为告诫保留,不是本次实际踩过的事故。
18. **`cda218956` 的"37 处冲突"来自提交标题自述**,没有独立方法重算(只能核实它是 merge commit 且 788 files changed)。
19. **A 仓各服务的代码行数**来自 scratchpad 的 `port_ledger.json`,未逐个统计核实,本文刻意不引用这些数字。
20. **`robot/etc/friend_smoke.yaml` 的"七步"**取自 `PROGRESS.md` 的 F3 条目与任务背景描述,本轮只读了该 yaml 的头部注释,未逐条独立核对。
21. **"`data/tip/Tip.xlsx` 的三个新 friend 码是哪次提交加进去的"**只核实到"xlsx 里已有、生成物里没有"这个事实状态。
22. ⚠ **本仓同时有多个会话在提交,本轮核实的是 2026-09-19 的快照(HEAD `a5ca66851`)。** 接手人开工前请重新 `git pull` 并按**符号名**复核一遍 —— 行号几乎肯定已漂移,个别条目也可能已被别的会话顺手做掉。

---

## §7 参考

### 设计文档

| 文档 | 内容 |
|---|---|
| `docs/design/friend-port-20260918.md` | **主文档**。§1 背景与裁定 / §2 决策 F1–F17 / §3 请求流与写路径形状 / §4 改动集(F1 20 文件、F2 19 文件、F3 22 文件)/ §5 不做项 / §6 已知缺口 / §7 验证清单 / §8 发布顺序 |
| `docs/design/friend-persistence-architecture.md` | 持久化形态。「显式容量锁行(2026-09-18 起,就绪门禁已退役)」/「锁序纪律(**改任何写路径之前必须读完**)」/「拉黑表 `friend_block`」/「存量容量回填门禁 —— 已退役」/「Online/Offline:读契约 key `player:session:{id}` 的 Pull Model」/「与 S2C 推送的分工」 |
| `docs/design/microservice-zone-contract-20260914.md` | 微服务接入 zone 的总契约。§3 发布顺序 / §5 推送只触发拉取 / §7 端口与五处登记 / §8 东西向通道 / §11.2 robot 代替客户端验收 |
| `docs/design/tip-code-axis.md` | tip 号段轴。新开一个域时要同步 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`(及 `.filters`) |
| `docs/design/client-rpc-router.md` | 路由服(D29–D34) |
| `docs/design/jubaozhai-market.md` | 聚宝斋(J-7 的号段说法与 Tip.xlsx 不一致,见 §4.1) |

### 决策条目(均在 `docs/design/xuanming-port-decisions-20260910.md`)

| 条目 | 标题 |
|---|---|
| **D-9** | 安全前置:**「身份从会话取」必须先于「客户端可达」** |
| **D-10** | B 侧已有实现的处置:**搬不变量,不搬服务** |
| **D-10 修订(2026-09-18)** | `friend_capacity` 回填就绪门禁:**退役;保留容量锁行与「缺行按权威边数、绝不猜 0」** |
| **D-11** | Go 服务注册的失租口径 |
| **D-12** | 新 Go 服务的客户端入口:**只承诺路由服模式;翻转落在部署层,C++ 默认值不改** |
| **D-13** | 全局服务 yaml 不注册 go-zero 发现键:**有 `Etcd` 段就显式写 `Key: ""`** |
| **D-14** | 新全局服务的库归属与建表方式:**每服务一库;表以 proto 为源;迁移用 `go/schemamigrate` + 服务 `-migrate` 的 K8s Job** |

### PROGRESS.md 条目

- `## 2026-09-18 friend 移植 F1 批:协议改名可达化 + 表以 proto 为源 + 服务骨架(Claude,未编译)`
- `## 2026-09-18/19 friend 移植 F2 批:事务重写(RC + 全局锁序)+ 拉黑/推荐/配额/清理/推送/在线状态(Claude,未编译)`
- `## 2026-09-19 friend 移植 F3 批:五处登记 + K8s 部署链 + 迁库 + 运维工具改连 + robot 冒烟 + 设计文档(Claude,未编译)`
- `## 2026-09-19 单点加固交接:已落码清单 + 剩余工作与验证步骤(Claude,全部未编译)`(B.1 明确写着"`go/friend` 当前编译不过是 friend 移植会话的既定状态")

### 主要代码位置

| 路径 | 内容 |
|---|---|
| `proto/friend/friend.proto` | `service ClientPlayerFriend`(10 C2S + `NotifyFriendEvent`) |
| `proto/friend/friend_table.proto` | 四张表的唯一事实源 |
| `go/friend/friend.go` | 入口、`-migrate` / `-allow-modify`、`ensureSchema`、生命周期 |
| `go/friend/internal/data/` | `friend_repo.go`(锁序说明 + 权威事务)/ `block_repo.go` / `recommend_repo.go` / `sweep_repo.go` / `session_reader.go` / `tables.go` |
| `go/friend/internal/logic/` | `friend_logic.go` / `recommend.go` / `rate_quota.go` / `sweep.go` / `push.go` |
| `go/friend/internal/{session,lifecycle,metrics,config,svc,server,constants}/` | 拦截器链 `grpcstats → killswitch → session → serverbase` 等 |
| `go/friend/etc/friend.yaml` | 端口 50400 / metrics `:9180` / `Sweep.Mode: report_only` / `DBName: mmorpg_friend` |
| `robot/friend_smoke_scenario.go` + `robot/etc/friend_smoke.yaml` | 跨区七步冒烟 |
| `deploy/k8s/manifests/go-svc/friend.yaml` / `friend-migrate.yaml` | 部署与迁移 Job |
| `deploy/mysql-init/00_init_zone_dbs.sql` | `mmorpg_friend` 建库的**唯一登记处** |
| `tools/merge_zone/`(`audit_resources.go` / `player_rows.go`)、`tools/data_consistency_check` | 运维工具的 friend 改连 |
