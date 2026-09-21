# friend 服务移植 —— 交接文档(2026-09-20)

> **读者**:接手这批代码的工程师(零上下文)。本文写完就是为了让你直接开工。
> **本文所有结论都来自读 `E:/work/xuanming-server-mmo` 分支 `main` 的源码**(首轮核实快照:2026-09-19,当时 HEAD `a5ca66851`;**复核于 2026-09-20**,其间 `main` 已前进到 `5524daf0c` → `4a04d0819`,但 `git log a5ca66851..HEAD -- go/friend proto/friend` **零提交** —— friend 这批代码本身没被别的会话动过)。
> **行号一律只作辅助坐标,几乎肯定已漂移** —— 本仓同时有多个会话在提交。
> 定位请一律用 `grep -n "<符号名>" <文件>`,**不要按行号跳**。
>
> **2026-09-20 续写**:上面那台机器(下称**机器 A**)的会话写到复核阶段时额度用尽。另一台机器(**机器 B**,仓库 `D:\luyuan\wuxingqitan\mmorpg`,HEAD `d9e471b80`)上的会话续完了本文:对 §1–§7 逐章重新复核(7 路并行核对 606 条可核对论断,报出的 52 条差异另经一道"默认文档是对的"的反驳式复验,**确认 50 条、驳回 2 条**,确认项已就地改进正文,清单见 §8),补了 §0(换机说明)与 §4.3(客户端细节,已获只读授权)。**两台机器的路径和工具链都不一样,正文里的盘符与 `buildenv.ps1` 不要照抄 —— 先读 §0。**
>
> **2026-09-20 收尾批(机器 B,接手会话)**:三个拍板已定(Python 由用户自己装、编译 / 导表 / proto-gen 由用户自己跑;`FriendBlocked` 改中性文案;proto-gen **带客户端**),§3 的 9 条收尾项里 8 条已落码(第 2 条 `EXPLAIN` 是运行期核对,仍未做),`friend_capacity` 加了 `created_ms` 列并有了回收路径。**现状、用户要执行的命令序列、以及 §2 / §3 / §5.6 里因此过期的期望值,统一见文末 §9;正文与 §9 冲突处以 §9 为准。** 仍然全部未编译、未运行。

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

**而且现在的 `main` 上 `go/friend` 一定编译不过** —— 不是"可能有问题",是生成物没跟上源。悬空引用共**五类**(同一个根因;proto-gen 与导表器跑完之后请按这五行逐项验收):

| 代码里引用的符号 | 现在的生成物里有没有 |
|---|---|
| `friendpb.RegisterClientPlayerFriendServer` / `UnimplementedClientPlayerFriendServer` / `ClientPlayerFriendServer`(引用点:`go/friend/friend.go`、`go/friend/internal/server/friend_server.go` 的 `NewFriendServer`);10 个 `friendpb.ClientPlayerFriend_*_FullMethodName`(引用点:`go/friend/internal/session/session.go` 的 `ClientMethods`)与 `ClientPlayerFriend_ServiceDesc`(`session_test.go`) | **没有**。`go/proto/friend/friend_grpc.pb.go` 里仍是旧的 `FriendService_*`,只有 8 个方法,还带着已删的 `NotifyOnline` / `NotifyOffline` |
| `friendpb.BlockRequest/Response`、`UnblockRequest/Response`、`ListBlocksRequest/Response`、`BlockEntry`、`RecommendFriendsRequest/Response`、`RecommendEntry`、`FriendEventS2C`、`FriendEventReason_*`(引用点:`go/friend/internal/logic/friend_logic.go`、`recommend.go`、`push.go`,`go/friend/internal/server/friend_server.go`,`robot/friend_smoke_scenario.go`) | **没有**。`go/proto/friend/friend.pb.go` 里仍是旧的 18 个 message(还带着已删的 `NotifyOnline/OfflineRequest/Response`),本批新增的 message 与 `FriendEventReason` 枚举一个都没有 |
| `friendpb.FriendEdgeRecord` / `FriendRequestRecord` / `FriendCapacityRecord` / `FriendBlockRecord`(引用点:`go/friend/internal/data/tables.go` 的 `Tables()`) | **没有**。`proto/friend/friend_table.proto` 从未生成过 Go 产物:`go/proto/friend/` 下只有 `friend.pb.go` 与 `friend_grpc.pb.go`,不存在 `friend_table.pb.go`(对照:`go/proto/trade/` 下有 `trade_table.pb.go`)—— **proto-gen 之后要专门确认这个文件出来了** |
| `table.FriendError_kFriendBlocked` / `_kFriendBlockListFull` / `_kFriendTargetInboxFull`(引用点:`go/friend/internal/constants/constants.go` 的 `ErrBlocked` / `ErrBlockListFull` / `ErrTargetInboxFull`;`robot/friend_smoke_scenario.go` 的 `tipFriendBlocked`) | **没有**。`go/shared/generated/pb/table/friend_error_tip.pb.go` 只到 `FriendError_kFriendTooManyPending = 15006`;`robot/vendor/shared/generated/pb/table/friend_error_tip.pb.go` 同样 |
| `game.ClientPlayerFriend*MessageId`(引用点:`go/friend/internal/logic/push.go` 的发号处;`robot/friend_smoke_scenario.go` 多处) | **没有**。各 `generated/pb/game/message_id.go` 里仍是 `FriendServiceAddFriendMessageId = 11` 这一套旧名 |

另外 `go/client_rpc_router/generated/pb/game/route_table.go` 里 friend 的路由项还是 `/friendpb.FriendService/*` 且 `ClientProtocol: false`,说明路由表也没重生成过;`robot/vendor/proto/` 下**没有 `friend` 包**;`bin/go_services/` 下**没有本批代码构建出的 `friend.exe`**(该目录被 `.gitignore` 忽略,各机器状态不同:机器 A 上没有这个文件;⚠ **机器 B 上留着一份 2026-08-02 的移植前旧构建**,是个陷阱,见 §0.3)。

**所以接手第一个动作不是 `go build`,而是 §2 的第 1–3 步(补齐工具链 → 导表器 → proto-gen)。** 缺的工具两台机器正好相反:机器 A 缺 Go,机器 B 缺 Python(见 §0.2)。

---

## §0 换机说明(2026-09-20 续写)—— 两台机器的路径与工具链都不同

### 0.1 路径对照 —— 正文里的命令不要照抄盘符

| 正文写的(机器 A) | 机器 B 上对应的 | 备注 |
|---|---|---|
| `E:\work\xuanming-server-mmo` | `D:\luyuan\wuxingqitan\mmorpg` | 服务端仓 |
| `E:\work\mmorpg-client` | `D:\luyuan\wuxingqitan\mmorpg-client` | 客户端仓。`proto_gen.yaml` 的 `unity_client_dir`(`{{output_root}}../mmorpg-client/`)与 `exporter_config.yaml` 的两条 deploy(`../../../mmorpg-client/Assets/Scripts/Table/Generated`)都是**相对路径**,两台机器都成立 —— 前提是**两个仓保持同级目录** |
| `C:\Users\luyua\go\bin` | `C:\Users\Administrator\go\bin` | protoc 的两个 Go 插件所在 |
| `. E:\work\tools\buildenv.ps1` | **不存在,机器 B 上也不需要**(见 0.2)—— 正文里每条命令的这一行在机器 B 上直接删掉 | |
| `C:/Users/luyua/AppData/Local/Temp/claude/E--work/…/scratchpad/…`(A 仓浅克隆、`gocheck.py`、`port_plan_mail.json`、`port_ledger.json`) | **机器 B 上全都没有** | 会话级临时目录,不随 git 走。§3 第 1 条、§4.1、§4.4、§5.3 引用到它们的地方,在机器 B 上一律视为"需要重新取得"(A 仓要重新浅克隆;`gocheck.py` 按 §5.3 的两条判据重写,约 70 行;mail 的摸底稿没有了,§4.1 写下来的就是全部) |

### 0.2 工具链现状 —— 两台机器**正好相反**

§2 第 1 步那张表描述的是机器 A(缺 Go、有 Python)。机器 B 于 2026-09-20 实测(只查存在性与版本,没有跑任何构建):

| 工具 | 机器 B | 位置 / 版本 |
|---|---|---|
| `go` / `gofmt` | ✅ | `C:\Program Files\Go\bin`,`go1.26.5 windows/amd64`,已在 PATH。满足 §2 第 1 步的 ≥ 1.26.5 下限 |
| `protoc` | ✅ 但**不在 PATH** | `third_party\grpc\install_vs2026_dbg\bin\protoc.exe`,`libprotoc 35.1`(`install_vs2026\bin\` 下还有一份 release 的)。服务端 protogen(`descriptor.go` 的 `protocPath := "protoc"`)与客户端 `tools\gen_proto.ps1`(`$Protoc = "protoc"`)**都靠 PATH 找它**。`dev.bat` 的 `:prepare_protoc` 会自己前置 PATH;**裸跑 `dev_tools.ps1 -Command proto-gen-run` 或客户端 `gen_proto.ps1` 时要自己加**(后者也可传 `-Protoc <绝对路径>`) |
| `protoc-gen-go` / `protoc-gen-go-grpc` | ✅ | `C:\Users\Administrator\go\bin`,`v1.36.10` / `1.6.0`,**已在 PATH**(机器 A 上不在) |
| **Python 3** | ✅ 在(**本行已于 2026-09-20 晚订正**)| `py -3 -V` = **Python 3.14.7**,`py -3 -m pip -V` = pip 26.2.1(`C:\Users\Administrator\AppData\Local\Python\pythoncore-3.14-64`)。⚠ **别用 `python` / `python3` 探测**:那两个名字命中的是 WindowsApps 的商店占位桩,一律走 `py -3` |
| **导表器的 4 个依赖** | ✅ **已装**(2026-09-20 21:57 起;当晚早些时候一个都没有) | 现为 `openpyxl 3.1.5` / `Jinja2 3.1.6` / `PyYAML 6.0.3` / `protobuf 7.36.2`。清单在 `tools/data_table_exporter/requirements.txt`(`protobuf>=7.35.1,<8` 是 checked-in protoc 35.1 的 gencode 要求,**跨 major runtime 会拒绝加载**)。`dev.bat export` 每次开跑前都会自己 `pip install -q -r` 这份清单,所以走 `dev.bat` 时无需手装;**只装 openpyxl 不够**这条只对"绕开 dev.bat 直接调 run.py"有意义 |
| Docker | ⚠ 装了,**守护进程没起** | `docker version` 连不上 `dockerDesktopLinuxEngine`。§2 第 6–9 步之前要先起 Docker Desktop |
| Visual Studio | ✅ | Visual Studio Enterprise 2026(regen 之后 C++ 重编用) |
| `GOPROXY` | ⚠ 默认值 | `https://proxy.golang.org,direct`。机器 A 上官方 proxy TLS 超时、改用 goproxy.cn;**机器 B 上通不通未实测**,`go mod tidy` 卡住就 `go env -w GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy/,direct` |
| Windows TCP 保留区 | ⚠ 与文档里的历史值不同 | `netsh int ipv4 show excludedportrange protocol=tcp` 实测含 **51840–51939**(`go_services.ps1` 注释里记的 51573–51872 是别的机器上的历史观测值;保留区**每次开机随机**)。落进保留区不致命:`go_services.ps1` 的 `Resolve-BindablePort` 会自动上挪并走派生 yaml,对等方经 etcd 发现实际端口 |

**由此,机器 B 上 §2 第 1 步改为**:只补导表器依赖(上表那一条 `py -3 -m pip install -r ...`);Python 本体与 Go 都不用装,`buildenv.ps1` 不用重建。装依赖属于 `AGENTS.md §10.2` 的"修改环境"事项,由用户执行。
⚠ **"本机没有 Python"是 2026-09-20 早些时候的误判**,当时 `py -0p` 报了 `No Installed Pythons Found`;晚些时候 `py -0` / `py -3 -V` 实测都正常。正文其它地方(§2 第 1 步、§4.5 第 0 步)若还写着"机器 B 缺 Python / 装 Python 3.12",一律以本节为准。

> **顺序约束不变**:第 2 步(导表器,要 Python)没过,不要跑第 5 步之后的任何一步 —— friend 的三个新 tip 码出不来,`go/friend` 照样编译不过。proto-gen(第 3 步,要 Go)本身不依赖导表器产物;Python 装好之前可以先做第 3 步的前置核对、与 B3a 那条线对齐 data_service 三个 rpc 的发号(§2 第 3 步)、以及客户端 `gen_proto.ps1` 的登记方案(§4.3),但**别把两步的产物拆到两次提交里发布**。

### 0.3 机器 B 上两个**本机特有**的陷阱

1. **`bin\go_services\friend.exe` 是一份 2026-08-02 的移植前旧构建**(83 MB;二进制里是旧的 `friendpb.FriendService`、零处 `ClientPlayerFriend`;同目录其余 exe 都是 9 月 12–14 日的)。`bin/go_services/` 被 `.gitignore` 忽略,git 看不出来。
   后果:`tools\scripts\start_game.ps1` 对 friend 的"缺 exe 跳过"只是 `Test-Path`,不看新旧,对这份旧 exe **不生效**;`go_services.ps1` 的 `Resolve-GoExecutablePath` 也优先取它。此时还挡着的只剩 `mmorpg_friend` 库预检 —— **库一建好、而 `go-svc-build` 还没跑**的这段窗口里,一键启动(它走 `go-svc-start-exe`)就会拿旧 exe 配新 `etc\friend.yaml` 起服,得到的失败与本批代码无关,而且 `Start-LocalGoServices` 抛错会让一键启动停在网关之前(这条后果是读脚本推出来的,未实跑)。普通 `go-svc-start` 走 `go run`,不受影响。
   处置:§2 第 7a 步的 `go-svc-build -GoServices friend` 会覆盖它(以 `friend.exe` 的修改时间变新为准);在那之前如果要用一键启动起别的服务,先请用户把这份旧 exe 挪走(它不在 git 里,挪走不丢任何东西),或启动时显式排除 friend。
2. **`tools\proto_generator\protogen\proto-gen.exe` / `pbgen.exe` 的时间戳是 2026-09-09 23:45**,比机器 A 上的(09-16 22:14)还旧,而且**早于 `cecb52995`(09-16 22:11)**—— 那次提交才让生成器认得 `scene_node_service.cpp` 里 `CreateScene` 包装函数的守护段。用这份旧 exe 跑 proto-gen,**Agones 块一定会被吞**(详见 §2 第 3 步的陷阱三)。"必须先 `proto-gen-build`、千万别只跑 `proto-gen-run`"在机器 B 上不是建议,是硬前提。

### 0.4 机器 B 的工作区同样是多会话共享的

2026-09-20 续写期间,`git status` 里除了常驻的子模块噪声(`.gitmodules` 三个 url 从 `luyuancpp` 改成 `luyuan-cpp`、`third_party/boost` / `librdkafka` / `ue5navmesh` 指针漂移、未跟踪的 `third_party/redis`)之外,还有 20 多个**别的会话**的未提交改动:帮会二期会话正在做 B3a-2(login 建角带名字),它自报的文件范围是 `go/login/internal/logic/clientplayerlogin/`、`go/login/internal/svc/servicecontext.go`、`go/login/etc/login.yaml`、`cpp` 的 `player_database_loader.cpp` / `player_battle.cpp` / `bag_test/player_feature_persistence_test.cpp`、java `LoginRpcClient.java`、`tools/merge_zone/{player_rows,audit_resources,merge_unit_test}.go`、`tools/scripts/stress_summarize.ps1`、`robot/login.go`;另有 `go/guild/`、`go/scene_manager/`、`deploy/k8s/` 下的零散改动。**§5.1 的全部纪律在机器 B 上原样适用**:动手前 `ListAgents` 问归属,提交一律路径限定,`git diff --cached` 逐行看,`git push` 之前先看 `git log origin/main..main` 里有没有别人的提交。注意 `tools/merge_zone/audit_resources.go` 与 `player_rows.go` 同时是 friend 改连的落点(§7)—— 改它们之前先问帮会二期会话。

---

## §1 现在的状态

三批标题里的 20 / 19 / 22 取自设计文档 §4 改动集(合计 61,`friend.go` 在 F1、F3 各计一次)。**它们不是 `f06090b19` 的完整改动面** —— 该提交实际改了 **66** 个文件,§4 的表格按行点名、去重后只有 58 个,没被任何一行点名的有 8 个:`PROGRESS.md`、`docs/ops/merge-zone-runbook.md`、`go/friend/go.mod`、`tools/merge_zone/main.go` / `merge_run.go` / `merge_unit_test.go`(删 `-friend-redis-*` 参数的落点就在这几个里),以及 `go/friend/internal/server/friend_server.go` / `inband_observability_test.go`(接四个新 RPC 的服务端薄包装;应是 F2 那 19 个里没点名的两个,属推断)。评审或回滚请以 `git show --name-status f06090b19` 为准,不要拿三批清单当全集。

friend 服务从 A 仓(Pandora)`services/social/friend` 移植而来,因超过 `AGENTS.md §10.2` 的 30 文件门禁,拆成 F1 / F2 / F3 三批。设计文档是 `docs/design/friend-port-20260918.md`(背景与裁定 / 决策 F1–F17 / 改动集 / 不做项 / 已知缺口 / 验证清单 / 发布顺序),持久化形态在 `docs/design/friend-persistence-architecture.md`。

### F1 —— 让协议可达、让表有唯一事实源、让服务有骨架(20 文件)

解决的是"这个服务在 B 仓的体系里要长什么样"。三件事:

1. **协议改名 `FriendService` → `ClientPlayerFriend`,并标 `option (OptionIsClientProtocolService) = true`。** 这不是洁癖:Unity / robot 的下行 handler 生成器只认服务名含 `ClientPlayer` / `GamePlayer` 的服务(判据在 `tools/proto_generator/protogen/internal/generator/unity/unity_client_handler.go` 的 `isRelevantService` 与 `.../generator/go/robot_case.go` 的同名函数 —— 后者同时被同包的 `robot_handler.go`(真正出 robot 下行 handler 的生成器)调用;两处**判据语义相同**:服务名含 `GamePlayer` 或 `ClientPlayer`。字面略有差别:unity 那份先取 `svc := method.Service()` 再判,robot 那份直接对 `method.Service()` 判、并多一段 debug 日志。定位请 `grep -rn isRelevantService tools/proto_generator`,不要拿某一行字面去 grep),**本期新增的 S2C 推送在 robot 侧没有 handler 就等于没接**(Unity 侧是另一回事:生成的 handler 桩在客户端从未被接线,真正的接线要手写,见 §4.3)。同时删掉 `NotifyOnline` / `NotifyOffline`(全仓非生成代码零调用方,`friend:online` 这把键从来没有写者),六个请求体删 `player_id` 改 `reserved`(D-9:身份只从会话取)。
2. **新建 `proto/friend/friend_table.proto`,作为四张表的唯一事实源**(`friend` / `friend_request` / `friend_capacity` / `friend_block`),列名与当时的 `deploy/mysql-init/guild_friend_tables.sql` 逐字对齐(⚠ **该文件现在已不在 main 上** —— 帮会二期 B1 与本批各搬走一半后,它在 `cda218956` 那次合并里被整个删掉;要看当时对齐的 friend 三张表建表原文,用 `git show f06090b19^:deploy/mysql-init/guild_friend_tables.sql`(父提交 `2a2b793f8`;**注意是 `f06090b19^`** —— `f06090b19` 自己已经把 friend 的三段 CREATE TABLE 与 `friend_capacity` 回填段删掉了,`git show f06090b19:...` 只能看到"只剩 guild"的版本)。旧 SQL 里只有 `friend` / `friend_request` / `friend_capacity` 三张;`friend_block` 是本批新增表,本来就没有可对齐的原文),整数主键、零 UNIQUE KEY(D-14 §2 约束)。
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

- **五处登记**(契约 §7 的那五处):`tools/scripts/go_services.ps1`(Tier 1、可多开,条目 `Port = 50400`)、`tools/scripts/go_svc_image.ps1`(镜像 `mmorpg-friend`,不带端口)、`tools/scripts/k8s_deploy.ps1`(`$GoSvcCatalogue` 条目 `Port = 50400` / `Global=$true` + `MigrateJob`;ConfigMap 的 22 个契约值全部用 `Get-AuthoritativeScalar` 从 `etc/friend.yaml` 读(核实:`k8s_deploy.ps1` 里 `$friend* = Get-AuthoritativeScalar` 共 22 行))、`deploy/k8s/manifests/go-svc/friend.yaml` 与 `friend-migrate.yaml`(后者是 Job,不带端口)、`tools/scripts/start_game.ps1`(列为**可选服务**,缺 exe 或库不就绪只告警跳过;不带端口)。配套测试 `tools/scripts/tests/k8s_migrate_gate.tests.ps1` 本批也改了,夹具里抄了一份 `Port = 50400`。
- **端口"五处逐字一致"说的是另一组位置**(50400 / `:9180`;契约 §7「端口五处一致」,`go_services.ps1` 的 friend 条目上方注释列的就是这五处):`go/friend/etc/friend.yaml` 的 `ListenOn` / `MetricsListenAddr`、`go_services.ps1` 的 friend 条目(只有 `Port = 50400`,9180 只出现在注释里)、`k8s_deploy.ps1` 的 `$GoSvcCatalogue` 条目、`k8s_deploy.ps1` 的 friend ConfigMap(`ListenOn: 0.0.0.0:50400` / `MetricsListenAddr: ":9180"`)、`manifests/go-svc/friend.yaml`(Service / containerPort / 探针 / `prometheus.io/port`)。在 `go_svc_image.ps1`、`start_game.ps1`、`friend-migrate.yaml` 里 grep 不到这两个数是正常的。现值已逐处核对一致;改端口时按这一组 grep `50400` 与 `9180`,别忘了测试夹具里那一份。
- **`Etcd.Key` 与 `Redis.Key` 在 ConfigMap 里都显式写空串**(D-13)—— 省略整行会让 go-zero 的 `conf.MustLoad` 直接 Fatal、Pod CrashLoop,chat 在 kind 上实测踩过。
- **迁库**:`deploy/mysql-init/00_init_zone_dbs.sql` 加 `mmorpg_friend` 建库与授权(D-14 第 5 条:这里是建库的唯一登记处);friend 的三张表与 `friend_capacity` 回填段从 `guild_friend_tables.sql` 里删掉(⚠ 后续那个文件已被**整个删除**,现在 `deploy/mysql-init/` 下只剩 `00_init_zone_dbs.sql` 与 `gateway_tables.sql`;删除理由写在 `00_init_zone_dbs.sql` 第 55–60 行)。
- **运维工具改连**:`tools/merge_zone` 的在线审计不再扫 `friend:online`,改看 `player:session:{id}`;friend 表审计改用库名限定 `mmorpg_friend.friend`(配 `validateFriendSchemaName` 防注入);`data_consistency_check` 另开 `friendDB` 连接,连不上报 "NOT CHECKED" 而不是伪装通过。
- **robot `friend-smoke`**:`robot/etc/friend_smoke.yaml` + `robot/friend_smoke_scenario.go`,七步跨区冒烟(见 §2 第 9 步)。
- **sweep 接线**:`logic.StartSweep` 在 F2 落地时无调用方,本批接上(`metrics.Start` 之后一行,用 `signal.NotifyContext` 的 ctx)。默认 `report_only`,接上之后不删任何数据。

---

## §2 第一件事:验证这批代码

**顺序是硬约束**,前一步没过不要跑下一步 —— 顺序错了会得到误导性的失败。

### 第 1 步 —— 恢复 Go 工具链(其余全部步骤的前提)

> ⚠ **下表是机器 A 的状态。机器 B 正好相反(有 Go、没 Python),见 §0.2** —— 在机器 B 上本步改为"装 Python 3.12 + openpyxl",Go 不用装、`buildenv.ps1` 不用重建。

机器 A 现状(2026-09-19 逐条核实):

| 工具 | 状态 | 位置 |
|---|---|---|
| `go` / `gofmt` | ❌ **不存在** | 不在 PATH;`C:\Program Files\Go`、`C:\Go`、`E:\work\tools\go126` 都不存在;`C:\`/`D:\`/`E:\` 深度 4 递归搜 `go.exe` 零命中 |
| `buildenv.ps1` | ❌ **不存在** | `E:\work\tools\` 只剩 `figma-context-mcp\` 与 `push-image-in-batches.ps1`(2026-09-07 被清空)。仓库里(不含本文档)还有 **36 处**、分布在 4 个 md 文件(`docs/design/handoff-backlog-2026-09-05.md` 一个就占 32 处,其余是 `deploy/k8s/README.md` 2 处、`tools/battle_art_gen/README.md` 与 `docs/design/nav-spawn-fix-2026-09-05.md` 各 1 处)写着 `. E:\work\tools\buildenv.ps1`,**那些命令行在没有重建该脚本的机器上全都跑不通**,别照抄 |
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

> **更正一条既有文档说法**:`docs/design/friend-port-20260918.md` §7 ① 写「fault 列必须为 0」。实际约定是**留空** —— 全表 39 个故障码写 `1`,非故障行一律空单元格,三者都是业务拒绝。**xlsx 的行与 `fault` 列不需要再改。**
>
> ⚠ **但导表前请先定一件事(需要用户拍板)**:`FriendBlocked`(第 211 行)现在的中文文案是「对方已将你拉黑」,而 `constants.go` 里 `ErrBlocked` 的设计说明(以及 `friend_logic.go` AddFriend 分支的注释)要求文案**不区分方向** —— AddFriend / AcceptFriend 的两个拉黑方向共用这一个码,点明「对方拉黑了你」等于把别人的黑名单变成可探测接口,而在「我拉黑了对方」的方向上这句话本身也是错的。要么把 B211 改成中性文案(如「无法添加该玩家为好友」),要么接受现文案并同步改掉 `constants.go` / `friend_logic.go` 那两段注释 —— **二者必须一致**。`TipInfoMessage.id` → 文案是客户端可见契约,文案随导表一起发出去。改文案不影响发号(码由行序与段决定)。改 xlsx 请严格按 §5.4 的写法。定完之后直接跑导表器。
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

⚠ **本步的 diff 里还会冒出三样不属于 friend 的东西,属于预期、不要当成异常回退**(导表器每次都是全量导出,没有按表过滤):① 新表 `RoleNameRule`(`data/RoleNameRule.xlsx` + `data/schema/rolenamerule_table.proto` 已在仓里但从未导出;`generated/tables/manifest.json` 的 `table_count` 31 → 32,并生成该表的 cpp / go / java / 客户端产物);② `login_error` 组的 `RoleNameInvalid` / `RoleNameTaken` / `RoleNameSensitive` 三个码(`Tip.xlsx` 第 71–73 行)会和 friend 三码一起发号 —— 全表 247 个码名里未生成的恰好是这 6 个;③ `MessageLimiter` 已改未导的 7 行(见第 4 步)。①② 是帮会二期 B3a 的内容(`105c5560b` 那个 WIP 提交混进来的),**提交前先和那条线确认归属**。

**多会话共用仓库的顺带注意**:导表器会按 `exporter_config.yaml` 的 deploy 覆盖客户端仓的 `Assets\Scripts\Table\Generated` 与仓内 cpp/go/java 产物(tip 枚举**不进**客户端:`core/generators/proto_gen.py` 的 `compile_proto_csharp` 只编根目录下的表 proto)。跑前先 `git status --porcelain --ignore-submodules=all data generated go/shared/generated cpp/generated/table java` 存一份,跑完逐项对比。

**失败时保留**:导表器完整 stdout/stderr;若 `BitIndexStateError` 之类中止,保留 `tools/data_table_exporter/state/` 原样(**不要手改**)。

### 第 3 步 —— proto-gen(**必须全量**)

**为什么必须全量**:`proto/friend/friend.proto` 这批改了三件牵动全仓发号的事 —— 服务改名 `ClientPlayerFriend` 并标客户端协议、删 `NotifyOnline` / `NotifyOffline`、新增 `Block` / `Unblock` / `ListBlocks` / `RecommendFriends` 与 `NotifyFriendEvent`。

**⚠ 硬约束**:改名释放了旧消息号 **2 / 7 / 11 / 12 / 53 / 76 / 119 / 120**(核自 `proto/message_id.txt`)。生成器(`tools/proto_generator/protogen/internal/generator/cpp/service_register_info.go` 的 `InitMessageId`)把未占用号灌进 `unUseMessageId` 再 `for uk := range unUseMessageId { mv.Id = uk; break }` 分配 —— **Go map 迭代顺序不定**,同一个空号这次指向哪个新方法每次都可能不同。
**但已在 `proto/message_id.txt` 里的方法原号保留**(`InitMessageId` 第一遍先按 `KeyName` = 服务名 + 方法名 查表回填,只有查不到的才去空号池取号),**不会全仓洗牌**(本文早期版本写"全仓所有服务的消息号都可能重新洗牌",那是错的)。不确定的只有两件事:① 本次**新增 / 改名**的方法各自落到哪个空号,每次生成都可能不同;② 被释放的旧号 **2 / 7 / 11 / 12 / 53 / 76 / 119 / 120 会易主**,指向别的方法(历史先例:`969bbec4c` 里 19 号从 `GuildServiceJoinGuild` 变成 `GuildServiceSetGuildMemberRole`,外加 216–223 八个新号,其余一行未动)。

⚠ **这次抢号的不只 friend**:`proto/data_service/data_service.proto` 在 `105c5560b`(09-19 的 WIP 提交)里新增了 `ReservePlayerName` / `ReleasePlayerName` / `BatchGetPlayerName` 三个 rpc(帮会二期 B3a 的内容),同样还没发号、没有任何生成物。全量 regen 后方法总数是 228 − 8 + 11 + 3 = **234**;空号池是 8 个释放号 + 228–233 共 14 个,随机分给这 14 个新方法 —— friend 的 11 个方法不一定正好占回那 8 个旧号,旧 friend 号可能落到 `DataService*` 上。`git diff --stat` 里出现 data_service 的 C++ / Go 生成物变化属于预期,不是生成器出错。**跑之前先和 B3a 那条线确认这三个 rpc 可以一起发号。**(`player_locator` / `instance` 的 8 个方法也不在 `message_id.txt` 里,但它们不在 `proto_gen.yaml` 的 `domain_meta.source` 里,历来无号,不受影响。)

由此:**gate 的 `rpc_event_registry`、路由服的 `route_table`、Unity、robot 的消息号必须来自同一次 proto-gen 并同批发布。** 混用两次生成的产物**不报错**,只会让客户端发 A 方法、服务端按 B 方法解包 —— 错位的是那些易主号与新号,不是所有服务。**不要手改 `proto/message_id.txt`**(它是生成物)。regen 之后请通知其他在改客户端协议的会话重取消息号。

**命令**:

```powershell
. E:\work\tools\buildenv.ps1
cd E:\work\xuanming-server-mmo
.\dev.bat proto
```

`dev.bat proto` = `:prepare_protoc`(校验 libprotoc 35.1 + 前置 PATH)→ `dev_tools.ps1 -Command proto-gen-build` → `dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools\proto_generator\protogen\etc\proto_gen.yaml`。

> ⚠ **上面这条 `.\dev.bat proto` 用的是默认配置(`enable_unity_client: true`),只有在"这批连客户端一起动"时才能用** —— 见下面陷阱一的 (a) / (b) 二选一。`proto_gen.yaml` 的 team 块注释本身就写着这道门禁。

> **⚠ 千万别只跑 `-Command proto-gen-run`**:`Invoke-ProtoGenRun` 默认"优先用现成的 `proto-gen.exe`,其次 `pbgen.exe`,最后才 `go run ./cmd`"。这两个 exe 被 gitignore、不随仓库走,各机器时间戳不同(机器 A 是 **2026-09-16 22:14**,机器 B 是 **2026-09-09 23:45**)。生成器 `.go` 源码最后一次改动是 `1f971aecc`(09-16 22:50,`code_parser.go` 的方法边界识别),再前一次是 `cecb52995`(09-16 22:11,`grpc_handler_gen.go` 的包装函数守护段);`7af8342ad`(09-18)只改了 `etc/proto_gen.yaml`。exe 早于 09-16 22:50 的,不先 `proto-gen-build` 就是用陈旧二进制生成,改动静默不生效。
>
> **更正一条常见误解**:`go/build.bat` → `go/build.ps1` **不是** proto-gen 入口,它只跑 db/login 的 goctl 包装与 import 修补;它自己的 docstring 就指向 `dev_tools.ps1 -Command proto-gen-run`。

**期望产物 / 通过标准(三项,全要)**:

① `go/proto/friend/friend_grpc.pb.go` 里 `ClientPlayerFriend_ServiceDesc.Methods` **恰好 11 条**(10 个 C2S + `NotifyFriendEvent`;当前是旧的 `FriendService_ServiceDesc`,8 条):

```powershell
Select-String -Path go\proto\friend\friend_grpc.pb.go -Pattern 'MethodName:' | Measure-Object   # 期望 11
Select-String -Path go\proto\friend\friend_grpc.pb.go -Pattern 'ServiceName:'                   # 期望 "friendpb.ClientPlayerFriend"
Test-Path go\proto\friend\friend_table.pb.go                                                    # 期望 True(四张表的 Go 产物,此前从未生成过)
```

② `go/client_rpc_router/generated/pb/game/route_table.go` 里 friend **11 行**全为 `/friendpb.ClientPlayerFriend/*` 且 `ClientProtocol: true`:

```bash
grep -c 'NotifyOnline\|NotifyOffline' go/client_rpc_router/generated/pb/game/route_table.go   # 期望 0
grep -c 'friendpb.FriendService'      go/client_rpc_router/generated/pb/game/route_table.go   # 期望 0
```
(当前状态:8 行,全 `/friendpb.FriendService/*`、`ClientProtocol: false`,前一条 grep 计数为 2。)

③ robot 与 Unity 侧生成出下行 handler。**生成器是每方法一个 handler**(robot 侧 ClientPlayerTeam 有 15 个),所以 friend 应新增 **11 个 robot handler + 11 个 Unity handler 桩**,`NotifyFriendEvent` 是其中最关键的一个 —— **robot 侧没有它,这次改名就白改了**。
- robot:`robot/logic/handler/` 下按现有 team 的命名规律,如 `client_player_friend_notify_friend_event.go`、`client_player_friend_add_friend.go`。
- Unity:客户端仓 `Assets\Scripts\Net\Generated\Handlers\` 下如 `ClientPlayerFriendNotifyFriendEventHandler.cs`,并重写 `HandlerRegistry.cs`。⚠ **走默认配置时客户端一次会新增 31 个桩,不是 11 个**(Team 15 + Jubaozhai 4 + `SceneSceneClientPlayerTravelToZone` 1 + Friend 11;现有 72 个,之后 103 个)—— 见陷阱一。另:Unity 的桩**只提供 `MessageId` 常量与 `Parser`**,`HandlerRegistry.Register` 在客户端全仓零调用,桩生成出来 ≠ 客户端接上了推送(§4.3)。
- 同时核 `robot/generated/pb/game/message_id.go` 与 `go/friend/generated/pb/game/message_id.go`:`FriendService*MessageId` 应全部消失、换成 11 个 `ClientPlayerFriend*MessageId`,并**逐个**与 `robot/friend_smoke_scenario.go` 里用到的常量名对上(那些名字是按命名规律推导写的)。

**⚠ 本步必须额外处理的四个陷阱(都已核实仍然适用)**:

- **陷阱一:客户端 `gen_proto.ps1` 尚未收录 friend —— 而且缺的不止 friend**。客户端仓 `tools\gen_proto.ps1` 的 `$files` 列表(32 项)里没有 `proto/friend/friend.proto`;而 `proto_gen.yaml` 的 `generators.enable_unity_client: true` 是**全局开关、没有按域细分**。用默认配置跑会往客户端写 11 个 `ClientPlayerFriend*Handler.cs`,而它们引用的 `Friendpb.*` 类型不存在 → **客户端 CS0246**。
  **⚠ `$files` 里同样没有 `proto/team/team.proto` 与 `proto/trade/jubaozhai.proto`**,而 `ClientPlayerTeam`(15)/ `ClientPlayerJubaozhai`(4)早已有消息号,客户端 `Handlers\` 下至今一个都没生成过(现有 72 个:Battle 12 / ClientPlayerChat 2 / ClientPlayerLogin 6 / Scene 52);09-18 新增的 `SceneSceneClientPlayerTravelToZone`(226 号)也还没有 handler,客户端 `PlayerScene.cs` 里也没有 `TravelToZoneResponse`。所以用默认 `proto_gen.yaml` 跑,一次会写入 15 + 4 + 1 + 11 = **31** 个新 handler 并重写 `HandlerRegistry.cs` —— **只补 friend 一行仍会因 `Teampb.*` / `Trade.*` / `TravelToZoneResponse` 不存在而 CS0246**。`proto_gen.yaml` team 块的注释本身就写着门禁:`gen_proto.ps1` 收录 `team.proto` 之前**不得用默认配置跑 `dev.bat proto` / `dev.bat gen`**。(`SceneRollbackClientPlayer` 的 12 个方法只标了 `OptionIsPlayerService`、没标客户端协议,不会出 handler。)
  **处置二选一(需要用户拍板:这批要不要把 Team / 聚宝斋的桩一起带进客户端)**:
  (a) **这批连客户端一起动**:往 `gen_proto.ps1` 的 `$files` 补 `proto/friend/friend.proto`、`proto/team/team.proto`、`proto/trade/jubaozhai.proto` 三行(注意现在的末项 `"proto/scene/player_currency.proto"` 后面要补逗号;三个 proto 的 import 只有 `proto_option` / `empty` / `tip`,都已在清单里),`.\dev.bat proto` 之后在客户端仓跑 `pwsh -File tools/gen_proto.ps1 -ProtoRoot <服务端仓根>` 与 `pwsh -File tools/gen_messageids.ps1 -ProtoRoot <服务端仓根>`。**`-ProtoRoot` 必须显式传**:两个脚本的默认值是 `$PSScriptRoot/../../..`(按"客户端是 `<服务端>/client/unity` 子模块"的旧布局写的),两仓同级布局下会解析到两个仓共同父目录的再上一级(机器 B 上是 `D:\luyuan`),目录存在所以不报参数错,要到 protoc 找不到 proto 才失败。`player_scene.proto` 已在清单里,重跑即补上 `TravelToZoneResponse`。`friend_error_tip.proto` **不是必需项**:清单里没有任何域的 `*_error_tip.proto`,handler 模板只引用响应类型。客户端侧的验证与提交细节见 §4.3。
  (b) **这批不动客户端**:**不要跑 `.\dev.bat proto`**。先 `dev_tools.ps1 -Command proto-gen-build`,再用 `enable_unity_client: false` 的**配置副本**跑 `dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath <副本>`(不经 `dev.bat` 就没有 `:prepare_protoc`,要自己保证该 shell PATH 上的 protoc 是 libprotoc 35.1)。**不要**直接改默认 `proto_gen.yaml` 的这个开关(其他域会失去同步),**也不要**删 friend 块(未解析的域会丢号)。走 (b) 时通过标准 ③ 里 Unity 那 11 个 handler 不会出现,属于预期。
- **陷阱二:C++ 侧会被一起重算**。`cpp/generated/rpc/service_metadata/friend_service_metadata.h` 现在写的是 `FriendServiceAddFriendMessageId = 11` 等,被 `rpc_event_registry.cpp` include;`cpp/generated/proto/friend/*.pb.{h,cc}` 与 `cpp/generated/grpc_client/friend/friend_grpc_client.cpp` 也是生成物、已登记进 `proto.vcxproj` / `grpc_client.vcxproj`。**regen 之后 C++ 必须重编**(串行 `msbuild /m:1 /nr:false`,顺序 proto → rpc → 各节点库 → 节点),否则 gate 的消息号表还是旧的。并核 `rpc_event_registry.h` 的 `kMaxRpcMethodCount` **等于 `proto/message_id.txt` 最大 id + 1**(regen 前是 228 / 227,对得上;`.cpp` 里还有一条 `static_assert`);按上面"抢号的不只 friend"的算法,regen 后的期望是 **`kMaxRpcMethodCount = 234`、最大 id 233**。⚠ **234 已不是定数**:帮会二期 B5a 会话(2026-09-20)在 `proto/guild/guild.proto` 的 `GuildService` 末尾新增了 5 个 rpc,它若先于这次全量 proto-gen 落到 main,期望值就是 239。**不变判据只有一条:`kMaxRpcMethodCount` = `proto/message_id.txt` 最大 id + 1**;数字对不上先 `git log -- proto/` 看是不是又多了 rpc。确实是生成器半途退出的话,重跑一次即可。
- **陷阱三:`cpp/nodes/scene/handler/grpc/scene_node_service.cpp` 的 Agones 块 —— 已进守护段,但陈旧生成器仍会吞。** `auto createPermit = agones::SceneLifecycle::Instance().AcquireCreatePermitBlocking();` 那块现在位于 `CreateScene` 包装函数的 `BEGIN/END WRITING YOUR CODE` 守护段**之内**(本文早期版本写"守护段外",那是 09-15 之前的状态),生成器自 `cecb52995`(09-16 22:11)起会按包装函数签名回读并原样写回这段「投递前」守护段,所以用当前源码编出的生成器不会再吞它。**但 `cecb52995` 之前编的 `proto-gen.exe` / `pbgen.exe` 不认这个守护段**(模板是 `.go` 里的字符串常量,编在二进制里)—— 机器 B 上那两个 exe 是 09-09 的,正属于这种情况(§0.3)。这是「必须先 `proto-gen-build`」的又一条硬理由。regen 后仍用 `git diff` 确认该块还在,作为兜底检查;被删了就恢复。
- **陷阱四:robot vendor**。`robot/vendor/proto/` 下只有 battle/chat/common/db/guild/login/match/scene/team/trade,**没有 friend**,`modules.txt` 里也搜不到。robot 默认 `-mod=vendor`,regen 之后必须 `cd robot && go mod tidy && go mod vendor`。另外 robot 的 handler stub 是"文件存在即跳过",坏 stub 重跑生成器不会自愈 —— 要先删再生成。

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

**通过标准**:`generated/tables/messagelimiter.json`(文件名**全小写**)的 `data` 数组里能查到这 10 个新 id 且数值与上表一致;`generated/tables/manifest.json` 里 `MessageLimiter` 的 `rows` 变成 **63**、顶层 `version` 比本步开跑前 +1。
⚠ **注意基线,不要拿仓里现在的 46 / 17 去对**:那是 09-18 那次导表的旧值(`generated_at` 2026-09-18),xlsx 此后多出的 7 行(不是 friend 的,是 219/221/222/223 这类帮会号)一直没导过。导表器每次都是全量导出,所以**第 2 步跑完时 manifest 就已经是 `rows: 53`、`version: 18`**,按硬顺序走到这里的正确期望是 **53 → 63、18 → 19**;只有跳过第 2 步直接跑才会看到 46 → 63(manifest 的 `version`「只在内容变化时 +1」,见 `core/manifest.py`)。
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
- 库里**四张业务表 + 一张迁移台账表,共 5 张**:`friend` / `friend_request` / `friend_capacity` / `friend_block`(清单源是 `go/friend/internal/data/tables.go` 的 `Tables()`,四个 message `FriendEdgeRecord` / `FriendRequestRecord` / `FriendCapacityRecord` / `FriendBlockRecord`),外加 `schema_migrations`(`go/schemamigrate/runner.go` 的 `ledgerDDL`;`Up` 每次都先 `CREATE TABLE IF NOT EXISTS` 它,`schemamigrate.Options` 没有关掉它的开关)。`SHOW TABLES` 看到 5 张是**正确结果**,**不要删台账表**;全新空库上除这 5 张之外再多任何一张才是异常。下面的 STATISTICS 查询会多列出 `schema_migrations` 的 `PRIMARY(version)` 与非唯一的 `idx_schema_migrations_checksum`,不违反"零 UNIQUE KEY"(台账表自身的建表语句不计入 `N statement(s)`,所以下面"再跑一次应执行 0 条"仍成立);
- **主键全是整数列、零 UNIQUE KEY**(D-14 §2):
  ```sql
  SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SEQ_IN_INDEX
  FROM information_schema.STATISTICS WHERE TABLE_SCHEMA='mmorpg_friend' ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX;
  -- 期望:NON_UNIQUE=0 的索引名只有 PRIMARY
  ```
- `friend_request` 有 `updated_ms` 列与两条索引 `(to_player_id,status)`、`(status,updated_ms)`;
- **(收尾批新增)**`friend_capacity` 有 `created_ms` 列(`bigint unsigned NOT NULL DEFAULT 0`)与一条**普通**索引 `(friend_count, created_ms)`(`NON_UNIQUE=1`;索引名预期 `idx_friend_capacity_0`,**以实际产物为准**),且 `-migrate` 的输出里**不得出现** `缺索引(不会自动建)` 这条 warning。
  ⚠ `go/schemamigrate` 对**已存在的表**只会 `ADD COLUMN`、**不会补建索引**(只出 warning、退出码仍是 0)—— 缺了这条索引,容量行回收的候选读每轮都是全表扫(`report_only` 下也一样)。全新空库走 `CREATE TABLE`,索引随表建出,不受影响;friend 库从未上线,理论上不存在"旧 schema 的存量表",真遇到就删库重建,或手工 `ALTER TABLE friend_capacity ADD INDEX idx_friend_capacity_0 (friend_count, created_ms);`。收尾批起 friend 与 guild 同口径:**缺索引会让常驻启动拒启、让 `-migrate` 以 4 退出**(`go/friend/friend.go` 的 `missingIndexError`),所以这里撞上时你会看到非 0 退出码而不是一条容易漏看的 warning;
- **再跑一次 `-migrate` 应执行 0 条语句**(幂等);
- `friend.exe -allow-modify`(**不带** `-migrate`)必须**非 0** 退出(`friend.go` 的 `validateMigrationFlags`,走 `schemamigrate.ExitFailed`)。

**7b. 常驻启动**

```powershell
..\..\bin\go_services\friend.exe -f etc\friend.yaml
```
前置:etcd(2379)、Redis(6379)、MySQL(3306)都在跑。`etc/friend.yaml` 当前:`ListenOn: 127.0.0.1:50400`、`MetricsListenAddr: ":9180"`、`Mode: dev`、`Schema.AutoMigrate: true`、`Friend.Sweep.Mode: report_only`。

**通过标准**:

- 日志里有一行 `  FRIEND SERVICE STARTED SUCCESSFULLY`(`go/friend/friend.go:279`,行首两个空格是横幅排版)。
  ⚠ **就绪判据是子串 `STARTED SUCCESSFULLY`,不是整行**:`tools/scripts/go_services.ps1:495` 是 `$content -match "STARTED SUCCESSFULLY"`(全量读日志文件,不逐行比)。所以两个前导空格与 `FRIEND` 这个词都不影响启动器;真正不能动的是 `STARTED SUCCESSFULLY` 这两个词(中间一个空格,共 20 个字符)—— 改掉它启动器就永远等不到 friend 就绪(`friend.go` 那行上方的注释写的是同一件事)。
- 横幅里 `mysql:` 那行**不含密码**(走 `svc.MySQLTarget`,格式 `<host>/<db> (user=<user>)`;含密码的 `BuildDSN` 只交给 `sql.Open`)。
- 其余可核项:`friend_redis:` 显示 `shared-fallback host=127.0.0.1:6379 type=node`;`sweep:` 显示 `mode=report_only`;`schema:` 显示 `auto-migrate (schemamigrate.Up at startup)`。
- `curl http://127.0.0.1:9180/metrics` 能看到**五个** friend 指标的预建 0 值序列:`friend_push_total` / `friend_rate_quota_total` / `friend_online_lookup_total` / `friend_sweep_pending_rows` / `friend_sweep_idle_capacity_rows`(最后一个是收尾批新增的:上一轮 sweep 看到的"零好友且超过保留期"的容量行数)。
- `etcdctl get --prefix FriendNodeService.rpc/zone/` 的值(protojson 的 NodeInfo)里 `endpoint` 与 `grpcEndpoint` **都**填了(只填前者路由服拨不通,guild 踩过;`friend_test.go` 的 `TestNodeInfoValueMatchesRegistryContract` 钉住这点)。前缀要带到 `/zone/`:`noderegistry` 在 `FriendNodeService.rpc/` 下写的是**双 key**(`go/shared/noderegistry/registry.go` 的 `RpcPath` / `AllocationKey`)—— `…/zone/<z>/node_type/27/node_id/<n>` 的值才是 NodeInfo;另一条 `…/allocated/node_type/27/node_id/<n>` 是全局占位 key,值是裸的 node UUID,里面没有 endpoint 属正常,不要拿它对这条标准。
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
# 然后经 gate/路由服对这个 <pid> **只调一次 GetFriendList**;DEL 之后、这次读之前不能有任何涉及他的好友写操作
#(任何写提交后都会 INCR generation,补 "0" 那条分支就走不到了)
docker exec redis redis-cli GET 'friend:{f:<pid>}:list:v3'
```
(**shell 里必须给键名加引号**,否则 `{}` 会被展开。)

**通过标准**:第三步 **必须非空**。若恒为空而调用又不报错,就是那处 generation 补 `"0"` 的修复丢了。
**失败时保留**:`redis-cli MONITOR` 抓到的 `EVAL` 参数 —— 看 `ARGV[1]` 是不是空串。

⚠ **不要拿第 9 步的冒烟当这里的触发器**(本文早期版本建议过,那是错的;核自 `robot/friend_smoke_scenario.go` 与 `friend_repo.go` / `block_repo.go` 的失效路径),两个原因:

1. **跑完后 GET 必为空,那是预期不是缺陷**:冒烟第 4 步 `C Block(A)` 失效 A、C 的列表键,第 7 步清理 `A RemoveFriend(B)` 再失效 A、B 的列表键(失效脚本 = `INCR generation` + `DEL 数据键`),C 全程不调 `GetFriendList`。冒烟成功结束后 A/B/C 三个 pid 的列表键都是空的,按上面的通过标准会被误判成"修复丢了"。
2. **它根本走不到要验的分支**:冒烟里每一次读之前都有写 —— `GetFriendList` 排在 `AcceptFriend` 之后、`GetPendingRequests` 排在 `AddFriend` 之后,写提交后的失效已经把 generation `INCR` 成非空,`generation == ""` 补 `"0"` 这条分支一次都不会执行;即使修复真丢了,冒烟期间缓存也照样写得进去。

**现状**:仓内没有"单发一次 GetFriendList"的现成工具 —— robot 里只有 friend-smoke 用到它,Unity 客户端的好友功能尚未开工(§4.3)。所以这一步**更稳的做法是补一条不依赖 MySQL 的单测把这条分支钉住**:用 `friend_repo_mysql_test.go` 已有的 `newCacheOnlyRepo`(miniredis,db 为 nil)直接调 `loadVersionedFriendCache(ctx, repo, friendListKey(42), 假 loader)`,断言 generation 键不存在时 `mr.Exists(key)` 为真 —— 修复丢了这条会稳定变红(按 `AGENTS.md §10.1` 由 Codex 跑)。冒烟只能当旁证:开着 `redis-cli MONITOR` 跑冒烟,能看到第 3 步回填 `EVAL` 把 `friend:{f:<B>}:list:v3` 写进去(B 的键从第 3 步活到第 7 步),这只证明缓存写得进去,**证明不了**补 `"0"` 分支。

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
4. **`mmorpg_friend` 已建且表已迁移**。两条启动路径在库未就绪时的表现不同,别混着判:
   - `start_game.ps1`(**本机一区一键启动,gate / scene / battle 全部写死 `-Zone 1`,两区冒烟用不上它**)把 friend 列为可选服务:库不存在或 appuser 无权限时打 `WARNING: 好友库 mmorpg_friend 未就绪（…），本次跳过 friend（好友请求会回「服务不可用」），其余服务照常启动。` 并继续启动其余服务 —— 看到这条 WARN 就说明 friend 根本没起。原文是**全角括号与全角逗号**,grep 请用子串 `本次跳过 friend`。
   - 前置条件 3 实际用的 `dev-start-zones` **没有这道库预检,也不会打这条 WARN**:它直接调 `go_services.ps1 -Command start-exe`,库不在时 friend 在启动期连库 / 建表失败直接退出,启动器只会打 `[warn]    z<N>_friend :<端口> not LISTEN within …s; continuing anyway` 与 `[timeout] z<N>_friend did not report startup within 30s - check logs`,然后**照常继续起后面的服务**。所以"没看到 WARN"不代表 friend 起来了;以 `run\logs\go_services\z1_friend.stdout.log` / `z2_friend.stdout.log` 里有 `STARTED SUCCESSFULLY` 为准,没有就是 friend 没起,冒烟必挂。(`start-exe` 走的是 `bin\go_services\friend.exe` —— 机器 B 上先确认它已被 7a 步重新构建,见 §0.3。)
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

> 全部条目都在 main(`f06090b19` / `cda218956` / `7b48b0a98` / `57af3fd09` 之后)逐条 `sed -n` 核实过,核实日期 2026-09-19;2026-09-20 在机器 B 上对着 `d9e471b80` 又逐条复核了一遍(92 条论断,更正 10 处,其中第 1 条的 `AcceptFriend` 结论被**改回**、新登记第 9 条)。
> ⚠ 大前提:**整批代码从未编译、从未测试**。下面所有"修法建议"都排在 §2 的第 2、3、5 步之后 —— 在那三步过掉之前动这里的任何一条,你分不清报错是你改出来的还是本来就有的。

> **2026-09-20 收尾批状态**:下表第 1、3–9 条**已落码(未编译、未运行)**,做法与偏离见 §9.2 / §9.3;第 2 条(`EXPLAIN`)是运行期核对,**仍未做**,而且现在要核的是**四**条 SQL(多了容量行回收的候选读)。各小节正文保留为当时的推导,**其中"现状"类的陈述已不成立**(例如"`friend_capacity` 只有两列、没有时间戳列""sweep 的三条 SQL 全部只写 `FROM friend_request`""三个零调用方方法""`LastActiveMs` 字段"),读的时候以 §9 与现行代码为准。

| # | 严重度 | 条目 | 改动面 | 收尾批 |
|---|---|---|---|---|
| 1 | **P2** | `friend_capacity` 无回收路径 | 3–5 文件(含 proto) | ✅ 已落码:路线一(加 `created_ms` 列)+ 缺行有上限重试,同批处理了 fail-closed 契约 |
| 2 | **P2** | 三条锁序假设未经 `EXPLAIN` 核对 | 0(纯核对)或 1–2 | ❌ 未做(运行期);现为四条 |
| 3 | P3 | pending 计数的并发不变量只靠注释守着,无测试 | 1 文件 | ✅ 注释更正 + 交叉 pending 并发用例 |
| 4 | P3 | `metrics` 里第三份 sweep 模式字面量对齐不到 | 1–2 文件 | ✅ 常量导出 + 三份对齐断言 |
| 5 | P3 | 三个零调用方的导出方法(`AreFriends` 尤其危险) | 1–2 文件 | ✅ 三个全删 |
| 6 | P3 | 三处缺测试(session_reader / recommend_repo / sweep ticker) | 新增 2–3 个测试文件 | ✅ 三个新测试文件 |
| 7 | P3 | `data.FriendEntry.LastActiveMs` 是死字段 | 2 文件 | ✅ 已删 |
| 8 | P3 | `friend_table.proto` 的 `updated_ms` 注释已过期;连带两条 `updated_ms` 断言是假绿 | 1 文件(+ 连带 regen)+ 2 个测试文件 | ✅ 注释重写 + 两处先归零再断言 |
| 9 | P3 | `logic/friend_logic.go` 包头「F2 之后仍然开着的口子(F3)」已过期(sweep 早已接线) | 1–2 文件(纯注释) | ✅ |

### 1.【P2】`friend_capacity` 表没有任何回收路径

**位置**:`(*FriendRepo).ensureFriendCapacityRows`(`go/friend/internal/data/friend_repo.go`,`INSERT IGNORE INTO friend_capacity ...` 在**事务外、自动提交**);生产调用点共 4 个,其中仍然**无条件**调它的写路径**只有一条**:`(*FriendRepo).AddFriendRequest`(`friend_repo.go:287`,参数校验之后直接 ensure)。另外三个调用点前面都已有事务外的前置探针:`(*FriendRepo).AcceptFriend`(`:449`,之前先普通读判 `(from,to)` 的 pending 申请行在不在,不在直接回 `ErrRequestNotFound`)、`(*FriendRepo).RemoveFriend`(`:622`,之前先 `friendEdgeExists`)、`(*FriendRepo).Block`(`block_repo.go:89`,之前先名额探针);sweep 的三条 SQL(`SweepTerminalRequests` / `deleteTerminalRequestsBefore`,`go/friend/internal/data/sweep_repo.go`)**全部只写 `FROM friend_request`**,`friend_capacity` 一个字没出现;频率配额的三个 fail-open 分支在 `(*FriendLogic).allowFriendRequest`(`go/friend/internal/logic/rate_quota.go`)。已登记处:`docs/design/friend-port-20260918.md` §6 第 12 条、`PROGRESS.md` F2 批条目。

**为什么是问题**:`AddFriendRequest` 为了拿容量守卫,会对**任意** `to_player_id` 建行 —— friend 服务没有玩家名册,无法验证 target 是否真实存在(代码注释自己就这么写)。`friend_capacity` 没有 TTL、不在 sweep 范围内、没有任何删除语句,于是该表随"被发起过好友申请的 id 个数"**单调增长**,且增长可由客户端驱动。唯一的闸是每分钟频率配额,而配额在 Redis 故障时**按设计 fail-open 放行**(理由写在 `rate_quota.go` 文件头,是有意的可用性取舍,不是缺陷)—— 即"Redis 挂掉的窗口内,这张表的增长完全不受限"。

**⚠ 已缓解的部分(别重复做,这一条更正了早期的描述)**:
- `Block`(`go/friend/internal/data/block_repo.go`)**已不再无条件 ensure**:已加事务外快速失败探针,名额满就直接 `ErrBlockListFull`、不 ensure;`config.Validate` 拒收 `Friend.MaxBlocks == 0`(`internal/config/config.go`),所以该探针恒生效。残留向量只剩代码注释自己点名的 **Block → Unblock 反复换目标**(`Unblock` 只删 `friend_block`,不碰 `friend_capacity`)。
- `RemoveFriend` 已被 F2-15 改成"先 `friendEdgeExists` 普通读判是不是好友,再 ensure"。
- **仍然无条件 ensure 的只有 `AddFriendRequest` 一条。**(⚠ 这一条来回改过:最早的说法「只剩 AddFriendRequest」是**对的**;机器 A 的复核把 `AcceptFriend` 也算了进来、还写成"更松的增长面",那是**错的**,机器 B 复验时对着代码更正回来。)
  `AcceptFriend` **已有**同形的快速失败,**别重复做**:`friend_repo.go` 的 `AcceptFriend` 在 ensure(`:449`)之前先用事务外普通读判 `(from,to)` 的 pending 申请行在不在(`SELECT 1 FROM friend_request WHERE from_player_id=? AND to_player_id=? AND status=?`),不在直接回 `ErrRequestNotFound`、不 ensure,注释原话是「事务外预检只用于避免恶意无申请调用制造无界 capacity 空行」;回归断言在 `friend_repo_mysql_test.go` 的 `TestAcceptFriend_ConcurrentHardLimit` 末尾(`neverRequested` 那三行,「无申请调用也不能制造无界 capacity 空行」)。`AcceptFriend` 确实**不吃**每分钟频率配额(`allowFriendRequest` 全仓只有 `friend_logic.go:272` 一个调用方,在 `AddFriend` 路径上),但它只有在 pending 行存在时才走得到 ensure,而 pending 行存在 ⇒ `AddFriendRequest` 早已为双方建过行(存量搬迁行除外,数量同样受 pending 行数约束)—— 所以它**不是**客户端可无界驱动的独立增长向量。
  核心结论(无回收路径、P2)不变;修回收路径时要算进增长面的是两条:`AddFriendRequest`(受每分钟配额约束,配额 fail-open)与 `Block → Unblock` 反复换目标(见上一条)。

**建议修法**。A 仓有先例可参考(浅克隆只读副本,见 §6 的失效提醒):`.../scratchpad/xuanming-server/services/social/friend/internal/data/friend_repo.go` 的 `DeletePairGuardsBefore`,接在 `internal/biz/sweep.go`。
⚠ **不能直接照搬**:A 仓删的是**专用**的 `friend_pair_guards` 表(带 `created_at`、纯守卫载体,按年龄删没有语义损失);本仓的 `friend_capacity` **本身就是好友数的权威计数行**(D-10),且 `FriendCapacityRecord`(`proto/friend/friend_table.proto`)只有 `player_id` / `friend_count` 两列,**没有任何时间戳列**。两条路选一条:

1. **加时间戳列**(更贴近 A 仓):给 `FriendCapacityRecord` 加一列(如 `created_ms`)并配 `OptionIndex`,由 `go/schemamigrate` 建;在 `sweep_repo.go` 加 `DELETE FROM friend_capacity WHERE friend_count = 0 AND created_ms < ? LIMIT ?`,在 `internal/logic/sweep.go` 的一轮里调它。**前置:改 proto = 要跑 proto-gen + `-migrate`**。
2. **不加列,用 `friend_count = 0` + 反查**:`DELETE FROM friend_capacity fc WHERE fc.friend_count = 0 AND NOT EXISTS (SELECT 1 FROM friend_request WHERE (from_player_id=fc.player_id OR to_player_id=fc.player_id) AND status=1) AND NOT EXISTS (SELECT 1 FROM friend_block ...) LIMIT ?`。不动 schema,但这条 DELETE 会去锁守卫行,**必须在 `friend_repo.go` 顶部的锁序说明里补一段**,并纳入 `friend_guard_lock_order_mysql_test.go` 的并发回归(否则就是给自己造一个新的 ABBA 来源)。

无论哪条:删掉一行 `friend_capacity` **不会放宽硬上限** —— `ensureFriendCapacityRows` 补行时从 `SELECT COUNT(*) FROM friend WHERE player_id = ?` 重算初值,不会猜 0(`TestMissingCapacityRowUsesAuthoritativeFriendCount` 钉着)。**这一点值得写进 PR 说明。**
⚠ 但「可自愈」**不等于无代价**,回收路径与现行代码的一条契约直接冲突:ensure 在**事务外自动提交**,而 `lockCapacityRows` 在事务内发现缺行时是**刻意 fail-closed** 的(`friend capacity rows missing for players ...`,非哨兵 error → logic 层定性为 `ErrStorage`,本域唯一 fault 码,会记成 `rpc_inband_fault` 触发告警),它的注释前提正是「没有人会删这张表」。加了回收 DELETE 之后,「ensure(行已在,`INSERT IGNORE` 空操作)→ 回收删行 → 取守卫缺行」与「`FOR UPDATE` 正等着这行、回收提交后该行消失」两个窗口都会把一次正常的 `AddFriend` / `AcceptFriend` / `Block` / `RemoveFriend` 打成 fault。**两条修法都绕不开,必须同批处理**:要么让写路径在 `lockCapacityRows` 缺行时回到事务外重新 ensure 并**有上限地**重试一次,要么明确接受这一罕见 fault(回收只删足够老且 `friend_count = 0` 的行)并写进注释与 PR 说明;同时改掉 `lockCapacityRows` 上方那段「此时还缺行说明有人在并发删这张表……是不变量破裂」的注释,并补一条「回收与写路径并发」的真 MySQL 回归。

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

今天成立:能让 pending 计数**变大**的写路径只有 `AddFriendRequest` 步骤 ⑧ 的 upsert 一条,它先 `lockCapacityRows`。其余对 `friend_request` 的写 —— `AcceptFriend` 的正/反向 UPDATE、`RejectFriend` 的 CAS、`Block` 取消双向 pending、sweep 的 DELETE —— 都只会让 pending **变小**或只碰终态行(全仓对 `friend_request` 的写语句只有这 6 条,已逐条核过)。(⑤⑥ 旁的代码注释把 `AcceptFriend` 也列成「会让这两个计数变大的写者」,那是注释不严谨:它增加的是 `friend_count`,不是 pending 数;改那段注释时顺手更正。)**这条不变量没有任何测试。** 下一个人新增一条写 pending 的路径而不取守卫时,这两处判定会**静默退化**成 check-then-act:上限被并发穿透,零报错、零日志。

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

**建议修法**:`AreFriends` → **删,或改名**为 `AreFriendsCachedApproximate` / `AreFriendsMaybeStale`。若保留:它自己的 doc comment(函数上方两行)**已经**写了「走好友列表缓存、最多陈旧 CacheTTL」与「权威结论用事务内的 `friendEdgeExistsForUpdate`」,IDE 悬浮看得到;缺的只是「**非权威**」这个词 —— 它目前只出现在上面那段**分节注释**里(「下面三个都**不是**权威判定」),与函数 doc comment 隔着一个空行,悬浮时看不到。注释层面能做的仅是把 doc comment 首行改成以「**非权威**」开头。注释拦不住误用,所以仍然推荐删或改名。`HasPendingRequest` / `CountOutgoingPending`:**直接删** —— 它们要做的事 `AddFriendRequest` 的步骤 ④⑤ 已在守卫内做了,留着只是多一份会漂移的第二实现。
**注意**:删之前先确认没有通过接口断言间接引用 —— 本轮 grep 覆盖全仓 `.go` 零命中,但 `go build` 从未跑过,**以编译器为准**。

### 6.【P3】三处缺测试

已核实:`go/friend/internal/data/` 下只有 `block_repo_mysql_test.go` / `friend_guard_lock_order_mysql_test.go` / `friend_repo_mysql_test.go` / `sweep_repo_test.go` —— **没有** `session_reader_test.go`、**没有** `recommend_repo_test.go`;`internal/logic/` 下只有 `friend_logic_test.go`,其 25 个 `Test*` 里没有一个碰 `StartSweep` 的 ticker。

**6a. `session_reader.go`(最划算,miniredis 就够,不需要 MySQL)**。要钉的四条行为:
- **分批**:`(*SessionReader).BatchOnlineStatus` 按 `batchSize` 切块,`batchSize` 取 `Friend.ListReadHardLimit`,传 0 时回落 `defaultSessionBatchSize = 256`。用 >256 个 id 验"真的切了多批、结果合并完整"。
- **MGET 空串**:缺席 = 离线(零值),不是错误。
- **proto 解码失败降级**:坏 payload 不能把整批拖成错误。
- **只认 `SESSION_STATE_ONLINE`**:显式排除 `DISCONNECTING`(断线待重连的租约期)。这条是**业务语义**,极易被后来者"顺手放宽",必须有测试钉住。

**6b. `recommend_repo.go` 的三段 SQL(需要真 MySQL)**:`RecommendByMutual`(FOF 查询)/ `RecommendRandom`(自己只有一条 `SELECT MIN(player_id), MAX(player_id) FROM friend`,随后委托锚点扫)/ `recommendAnchor`(锚点正向扫)。四类排除(自己 / 已是好友 / 双向拉黑 / 任一方向仍 pending 的申请)**内联写在 `RecommendByMutual` 与 `recommendAnchor` 两条 query 字符串里、各一份**,列名不同:前者是 `f2.friend_player_id`,后者在两处 `NOT EXISTS` 里必须写全限定的 `friend.player_id`(原因见该处注释:裸 `player_id` 会解析到子查询自己的表上,排除条件静默失效);`recommendExcludeClause` **只**负责拼调用方传入的 `exclude` 列表(客户端「换一批」回传的 id),与四类业务排除无关。要对两份内联子句**分别**验**列名拼写**与**排除集**,再单独验 `exclude` —— 拼错列名在 Go 侧完全静态无感。
⚠ **已登记的假绿**:robot `friend-smoke` 第 5 步"推荐不含已拉黑的 C"在三账号数据集下结构性不可能失败,**不要把冒烟的绿当成 `friend_block` 排除生效的证据**(见 §2 第 9 步)。

**6c. `logic/sweep.go` 的 ticker 侧**。职责分界写在文件头(本文件只管节拍 + 抖动 + `safego` panic 边界 + 单轮预算 + 指标与日志;模式判定与保险在 SQL 侧)。`sweep_repo_test.go` 已把 SQL 侧钉得很死,**ticker 侧零覆盖**:未知 mode 的错误日志分支、单轮预算 `sweepRoundBudget`、`ctx` 取消即退出、`SetSweepPendingRows` 被喂进去的 mode 值(与第 4 条联动)。用假 `SweepStore`(接口定义在 `sweep.go`)+ 注入的 ctx 即可,不需要库。

**改动面**:新增 2–3 个测试文件;**miniredis 不用再加** —— `go/friend/go.mod` 的 require 块里已经有 `github.com/alicebob/miniredis/v2 v2.35.0`(`friend_repo_mysql_test.go` / `friend_logic_test.go` 已在用)。(**注意** `go.mod` 顶部的纪律注释:`go mod tidy` 之后 `github.com/redis/go-redis/v9` 只应出现在 indirect 块,别顺手接受 tidy 把它提上去)。

### 7.【P3】`data.FriendEntry.LastActiveMs` 是无写入方的死字段,且与邻近注释自相矛盾

**位置**:`data.FriendEntry.LastActiveMs`(`go/friend/internal/data/friend_repo.go`,`int64` + json tag `last_active_ms`),邻近注释有一句"**不进缓存**:它是每次请求都会变的展示态"。唯一的 MySQL 装载点 `loadFriendListFromMySQL` 的 SQL 是 `SELECT friend_player_id, since_ms FROM friend WHERE player_id = ? LIMIT ?`,Scan 只扫两列 —— **该字段没有任何写入方**。logic 侧读的是**另一个类型**:`(*FriendLogic).GetFriendList`(`internal/logic/friend_logic.go`)里 `entry.LastActiveMs = st.LastActiveMs` 的 `entry` 是 `*pb.FriendEntry`、`st` 是 `data.OnlineStatus`;`recommend.go` 的 `fillRecommendOnline` 同理,写的是 `*pb.RecommendEntry`(**不是** `RecommendCandidate` —— 那是 data 包里的 SQL 结果行,只有 `CandidatePlayerID` / `MutualFriends` 两个字段,根本没有 `LastActiveMs`)。→ `data.FriendEntry.LastActiveMs` 在生产代码里**零写入、零读取**。

**为什么是问题**:缓存回填走 `loadVersionedFriendCache` 的 `json.Marshal(value)`,**整个 `FriendEntry` 连同 `last_active_ms` 一起被序列化进 Redis**。所以"不进缓存"那句今天只是**碰巧成立**(值恒为 0),结构上没有任何东西拦着 —— 谁哪天给它赋了值,那个展示态就会被写进 **30 分钟 TTL** 的缓存,玩家看到的"最后活跃"会冻在半小时前,而注释还在声称这不可能发生。次要代价:每条好友白占约 20 字节缓存空间。

**建议修法**:**删掉该字段**,并同步**两处**注释(改动面是 **2 文件**):① `friend_repo.go` 里 `FriendEntry` 上方的邻近注释改成「在线态与最后活跃时刻都不在本结构里 —— 它们由 logic 层从 `session_reader` 现取,见 `logic/friend_logic.go` 的 `GetFriendList`」;② `logic/friend_logic.go` 的 `GetFriendList` 循环里那句「也正因如此,data.FriendEntry.LastActiveMs 不进缓存」点名了这个字段,要一起改掉。代码引用靠编译确认(全仓 grep 零读写;测试里两处 `data.FriendEntry{...}` 字面量都没给它赋值,删了不会编不过),**注释引用编译器看不见,必须再 grep 一次 `LastActiveMs`**。
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

旁证:`go/friend/internal/logic/sweep.go` 的注释已正确写着「F2 已补齐四个写路径」(四条路径 = 五条语句,`AcceptFriend` 占正、反向两条)。
⚠ **测试覆盖没有看上去那么全**:`friend_repo_mysql_test.go` 的 `TestAddFriend_WritesUpdatedMs` 只钉了第 1、2、4 处(且用了「先直写 `updated_ms=0`、再迁移状态」的防糊弄手法,这三处是**真**钉住的)。第 3 处(反向 UPDATE)在同文件 `TestAcceptFriend_AlsoResolvesReversePending`、第 5 处(`Block`)在 `block_repo_mysql_test.go` 的 `TestBlock_CancelsPendingInBothDirections` 里各有一条 `assert.NotZero(readRequestUpdatedMs(...))`,但这两个用例的申请行都是 `seedPending` 造的,而 `seedPending` 直写 `updated_ms = now`(非零)、之后也没有归零 —— **这两条断言结构性不可能失败**,生产 UPDATE 哪怕漏写 `updated_ms` 也照绿。顺手修法(各 2 行,2 个测试文件):在 `AcceptFriend` / `Block` 调用之前照 `TestAddFriend_WritesUpdatedMs` 的手法把对应行的 `updated_ms` 直写归零。几个用例都要真 MySQL(`FRIEND_TEST_MYSQL_DSN`),SKIP 不算过。

**为什么是问题**:`friend_table.proto` 是这四张表的**唯一事实源**,下一个人排查 sweep 时第一站就是它。读到"本列当前没有任何写入方"会直接得出**"delete 永远不能开"**的结论,而真实结论应当是"**delete 的剩余前置只有一条:从旧共享库 `mmorpg` 搬过来的存量行 `updated_ms` 会是 0**"(搬迁必须显式赋值,建议取 `request_time_ms`)。SQL 侧已为此留了保险:`SweepTerminalRequests` 发现"终态且 `updated_ms=0`"的行时只统计不删并打错误日志(`sweep_repo.go`,日志里连存量修法的 SQL 都给了)。

**建议修法**:把那段注释改写成"写入方已齐 + 剩余前置是存量搬迁 + SQL 侧已有保险"。F3 批当时禁止改 `proto/friend/**`(所以留到了现在,见 `docs/design/friend-port-20260918.md` §6 第 5 条),**该禁令已随 F3 合并结束**。
⚠ **前置**:改 proto 即便只动注释,也请与 proto-gen 那一步放在同一个窗口里做,并核对生成物 diff 只有注释变化(该文件不进客户端 `gen_proto.ps1` 清单,见文件头,所以不牵动 Unity)。
另注:`etc/friend.yaml` 的 `Sweep.Mode` 仍是 `report_only`;改 `delete` 前建议先在 `report_only` 下观察一轮 `friend_sweep_pending_rows`。

### 9.【P3】`logic/friend_logic.go` 包头的「仍然开着的口子」**现在是错的**(机器 B 复核时新登记)

**位置**:`go/friend/internal/logic/friend_logic.go` 文件头「# F2 之后仍然开着的口子(F3)」三条。与第 8 条同一类问题:注释在说一件已经不成立的事。

1. 第 1 条仍写「**sweep 没有启动点**……终态申请清理目前是**死代码**」。**已失实**:F3 已在 `go/friend/friend.go` 的 `metrics.Start` 之后接上 `logic.StartSweep(ctx, deps)`(上方有完整接线说明,见本文 §1)。排查 sweep 的人读到 logic 包头会以为清理循环根本没起。→ 删掉,或改成「已由 F3 接线,见 `friend.go`」。**现在就能改**。
2. 第 2 条(三个新 tip 码)**半过期**:三行已由 `7b48b0a98` 写进 `data/tip/Tip.xlsx`(已解包核实),但导表器还没跑,`FriendError_kFriendBlocked` 等三个枚举在生成物里仍不存在 —— 「常量不存在」这半句今天还成立。`go/friend/internal/constants/constants.go` 里「⚠ F2 批的已知前置」那段注释是同样的状态。→ 两处都等 §2 第 2 步(导表器)过了、核对枚举名逐字一致之后再改。
3. 第 3 条(推送消息号常量待 proto-gen 后核对)→ 等 §2 第 3 步。

**改动面**:1–2 文件,纯注释,不牵动 regen。

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
2. **号段 `biz_tag = mail` 要在四处同加**(代码里自己写明"必须一致"):`go/data_service/internal/config/config.go` 的 `DefaultIdSegmentBootstrapTags`、`go/data_service/internal/store/id_segment_store.go` 的同名包级变量(`var`,Go 没有 slice 常量)、`go/data_service/etc/data_service.yaml` 的 `IdSegment.BootstrapTags`、`tools/scripts/k8s_deploy.ps1` 的 data-service ConfigMap。现值四处一致 `[player, guild, item, txlog, snapshot, trade_listing]`。**friend 没有用号段,`mail` 会是第一个新 tag。** 另:`config_test.go` 与 `store/id_segment_integration_test.go` 里的断言比的都是包级变量 `DefaultIdSegmentBootstrapTags` 本身(没有硬编码清单,也没有硬编码长度),**加 tag 不需要改任何测试**(上一个新增的 `trade_listing` 就没动测试)。但也正因如此,config / store 两份默认清单之间、以及它们与两份 yaml 之间的一致性**没有任何测试守着**,只靠注释里的"必须一致"—— 四处要人工逐字对一遍。
3. **建库**:`deploy/mysql-init/00_init_zone_dbs.sql` 加 `mmorpg_mail` 的 CREATE + GRANT(照 `mmorpg_friend`)。
4. **五处登记**:照 friend 已落地的样板(§1 的 F3 段)。
5. **⚠ guild 没有 K8s 登记**:`k8s_deploy.ps1` 的 `$GoSvcCatalogue` 只有 db / data-service / login / player-locator / scene-manager / match / chat / client-rpc-router / trade / friend —— **没有 guild**,`deploy/k8s/manifests/go-svc/` 下也没有 `guild.yaml`。M2 的公会邮件要经 guild gRPC 取成员,**在 K8s 上现在拿不到 guild**。这是 M2 的硬前置,M1 不受影响。

**端口**:mail 50900 / metrics `:9240`。来源是会话 scratchpad 的排批稿;仓内**只有 `PROGRESS.md` 的 friend F1 条目顺带记了一笔**(「mail 50900/:9240、rank 51000/:9250」),**没有进任何代码、yaml 或契约文档**。可交叉核实的是:50900 / 9240 全仓 grep 零占用,以及 friend 的 50400/:9180 按同一份表落地。`-Zone 2` 位移后是 51900 —— ⚠ **不要把"不落保留区"当成定论**:Windows 保留区每次开机随机(`netsh int ipv4 show excludedportrange protocol=tcp`),`go_services.ps1` 注释里记的历史观测值是 51573–51872,而机器 B 2026-09-20 实测是 **51840–51939,51900 正好落在里面**。落进去也不致命:`go_services.ps1` 的 `Resolve-BindablePort` 会自动上挪并走派生 yaml,对等方经 etcd 发现实际端口(chat 51700 / trade 51800 同理)。若要正式采用,应写进契约 §7 的指标端口分工(该行现值到 `9220 guild` 为止,trade 的 9230 也还没补进去)。

**规模**:摸底估 20 人日 / 46 文件,超过 `AGENTS.md §10.2` 的 30 文件门禁,**必须再拆 2–3 批**。摸底产物 `scratchpad/port_plan_mail.json` 是**待评审的提案,不是拍板**(里面 11 条 `decisions_needed` 都没有用户确认)。

### 4.2 leaderboard(排行榜)—— 本轮只能出设计文档,不能落码

**现状核实**:`proto/rank/` 与 `go/rank/` 都**不存在**;节点枚举**已有**(`RankNodeService = 13`);Tip 段**未开**(只有 `rank_error 21000` 预留注释);proto-gen `domain_meta` 没有 rank。B 仓现有的榜只有公会榜(`go/guild/internal/data/guild_repo.go`),而且是**两层**:全局 ZSET `guild_rank`(`guildRankKey`,member=guildID,score=rankScore)+ 每 zone 一个分区榜 `guild_rank:zone:<zoneID>`。注释写明"Score 是公会排行分的 MySQL 权威副本;Redis ZSET 只是读加速层"。`RebuildRanks` 先写临时键 `guild_rank:rebuild:<token>:global` / `…:zone:<id>`,再 SCAN `guild_rank:zone:*` 并 DEL + RENAME 原子切换,多实例之间用 `guild_rank:maintenance_lock` 互斥。**leaderboard 的 Redis key 命名要避开整个 `guild_rank*` 前缀**;下文"GUILD scope 继续留在 `go/guild`"指的是这两层一起留下。

**⚠ 更正:`PlayerRatingChangedEvent` 不存在**。早期说法是"它要消费 match 的 `PlayerRatingChangedEvent`" —— 全仓(含 A 仓浅克隆)grep `PlayerRatingChanged` **零命中**。`proto/contracts/kafka/` 下的全部 message 是:`GateCommand`、`RoutePlayerEvent`、`KickPlayerEvent`、`PlayerDisconnectedEvent`、`PlayerLeaseExpiredEvent`、`BindSessionEvent`、`RedirectToGateEvent`、`PushToPlayerEvent`、`BroadcastToPlayersEvent`、`BroadcastToSceneEvent`、`BroadcastToAllEvent`、`BindBattleEvent`、`UnbindBattleEvent`、`PlayerLifecycleCommand`、`SceneCommand`、`BattleResultTeam`、`BattleResultEvent`。**没有任何 rating 相关事件。**

实际的评分形态(`go/match/internal/logic/rating.go`):评分存在 Redis hash `match:rating:{player_id}`(字段 `rating` / `games` / `updated_at_ms` / `recent_battles`,`defaultRating = 1500`,`eloK = 32`);写分入口是 `ApplyBattleResult(svcCtx, *kafkapb.BattleResultEvent)`。消费循环在 `go/match/internal/kafka/result_consumer.go`(topic `match-results`,消费组默认 `match-rating`),但**调用点不在那里,在 `go/match/match_service.go`**:kafka 包刻意不 import `logic`(`svc → kafka → logic → svc` 会成环),`ApplyBattleResult` 是在 `if c.RatingEnabled` 分支里以回调注入 `StartResultConsumer` 的。整条评分链受 `RatingEnabled`(默认 true)控制,关掉后不消费、评分恒为 1500。入账是逐人增量 Lua(`ratingApplyScript`)+ 两层幂等。**match 不对外发布任何评分变更事件。**
这条成环约束对下面的候选 (a) 有直接影响:出站事件的构造器 / 发送器可以照 `gate_command_builder.go` 放在 kafka 包里,但**发布调用点只能在 `logic` 内**(Δ 只在 `ApplyBattleResult` 里算得出;它的返回值是 `(outcome string, error)`,回调那一层拿不到 Δ),kafka 包不能反过来调 `logic`。

**所以 leaderboard 的真实阻塞是:写分来源这条东西向边根本不存在,要新造。** 两个候选,都得先拍板:
- **(a)** 让 match 新增一条出站事件(带 battle_id 做幂等键)→ 要改 `go/match`;
- **(b)** leaderboard 自己也消费 `match-results` 的 `BattleResultEvent` → 但 Elo 的 Δ 是在 match 进程内算的(`applyRatingDelta` / `eloExpected` / `teamAverageRating`),leaderboard 要么重算一遍(两份真相),要么只能拿到胜负而拿不到分。

`docs/design/microservice-zone-contract-20260914.md` §8「C++ scene → 全局 Go 服务(东西向)」明确规定:路由服不是东西向通道(缺会话回 Unauthenticated),内部调用只有 Kafka(带幂等键)或直连 gRPC 配专用 READY 选择器两条路,后者还要补 `nodeTypeNameMap`、两份前缀表、scene 白名单。**leaderboard 的写分路径必须落在这两条之一。**

**⚠ 更正:"go/match 归组队会话所有"已过期**。`E:/work/xuanming-server-mmo-team2` 与 `-team` 两个 worktree 目录**都已不存在**,组队已于 09-18 全量合并进 main(`go/match/internal/team/` 在 main 上)。改 `go/match` 不再有 worktree 冲突,但**组队那批代码同样从未编译、从未测试**,叠加改动前先把它编译过一次。另外 `go/team/` 下**只有 `generated/`**(实现在 `go/match/internal/team/`),`go/battle/` 同样只有 `generated/`。**team 不是独立进程,不需要、也不应该进 `go_services.ps1` 的 `$ServiceCatalogue` 或 `k8s_deploy.ps1` 的 `$GoSvcCatalogue`**(本文早期版本把这说成"组队那批的尾巴",那是错的):按 `docs/design/team-system.md` 的 D-2("team 不新增进程、端口、镜像或 manifest"),它与 match 同进程、同一个 zrpc server(`go/match/match_service.go` 里 `RegisterClientPlayerTeamServer` + `TeamNodeService` 第二次节点注册),随 match 的 50500 端口、match 镜像与 `match_service.yaml` 一起部署,k8s 的 match ConfigMap 模板已带 `Team.AllowCrossZone` 段。**不要去给 team 补 catalogue 条目或 manifest。** 组队那批真正的尾巴只有一条:**从未编译、从未测试**(`team_smoke` 两种 `AllowCrossZone` 形态也没跑过,清单见 `PROGRESS.md` 09-18 组队收口条目)。`data/MessageLimiter.xlsx` 里 team 的 12 个 C2S 消息号(201/202/204–212/214)**已经配过档位**(查询类 5 次/秒、写操作 3 次/秒,3 个 `Notify*` 按惯例不进表),不用再填 —— 只需在 proto-gen 之后按 `proto/message_id.txt` 复核这 12 个号仍指向 `ClientPlayerTeam*`。

**端口**:rank 暂记 51000 / metrics `:9250`(来源与效力同 mail)。⚠ **51000 不能直接采用,它并非全仓未占用**:① `deploy/docker-compose.login-stack.yml`(`"51000:51000"`、`LOGIN_GRPC_ENDPOINTS=login:51000`)与 `deploy/login-stack.linux/login.yaml`(`ListenOn: 0.0.0.0:51000`)把它用作 login 的 staging 端口并映射到宿主机,`docs/ops/release-checklist.md`("staging 51000;prod 50000")、`docs/ops/linux-staging-stress-runbook.md` 同口径;② 本地多 zone 下 `go_services.ps1` 按 `base + (Zone-1)*1000` 位移,rank 51000 在 `-Zone 3` 时 = **53000,正好撞 zone 1 的 login**(`$ServiceCatalogue` 里 login 的基址就是 53000;三 zone 压测是真实用法)。写 leaderboard 设计文档时请另挑端口 —— 例如 51100(全仓 grep 零命中,位移后的 52100 / 53100 / 54100 也不撞 login 53000 段与 player_locator 53200 段),落笔前自己再全仓 grep 一遍。`:9250` 全仓未占用。
**本轮交付物**:**只写设计文档,1 个文件**。v1 范围建议:只做 ZSET 榜 + 客户端读;`SettleBoard`(发奖)等 SYSTEM_CREDIT 白名单开了再说;GUILD scope 继续留在 `go/guild`,不搬。Tip 的 21000 组头放到 leaderboard 的首码批再开。

### 4.3 Unity 客户端的好友功能 —— 另立客户端任务

> ⚠ **客户端是同级独立仓库 `../mmorpg-client/`(机器 A `E:/work/mmorpg-client`,机器 B `D:\luyuan\wuxingqitan\mmorpg-client`)。** 依据:仓根 `AGENTS.md` §9 —— 未获客户端任务授权时不要读取或修改独立客户端。
> **授权现状(2026-09-20)**:用户已授权**只读**,机器 B 的会话据此做了完整摸底(生成管线 + 手写层样板),**完整规格单独落在 [`docs/design/friend-client-spec-20260920.md`](friend-client-spec-20260920.md)**(文件清单、两条链路接线点、三个拉取时机的钩子、红点规则、不要做的事、12 条 EditMode 验收用例)。**修改客户端仓的授权还没有** —— 开工前先向用户要。下面只放摘要。

**摸底推翻了本文早期的三个假设**(细节见规格文档 §0):

1. **Unity 的生成桩不是接线点。** `HandlerRegistry.Register` 在客户端全仓零调用;`GameClient.WireSceneNotifyHandlers` 的注释明确警告 `OnNotify` 是**覆盖语义**,接上它会静默盖掉手写的 `RedirectToGate` / `NotifyEnterScene` 处理器。桩只提供 `MessageId` 常量与 `Parser`。真正的接线是手写 `_net.RegisterNotify(ClientPlayerFriendNotifyFriendEventHandler.MessageId, …)`。
2. **`gen_proto.ps1` 缺的不止 friend**,还缺 team 与 jubaozhai;默认配置跑 proto-gen 会给客户端新增 31 个桩(§2 第 3 步陷阱一)。两个生成脚本的默认 `-ProtoRoot` 在两仓同级布局下是错的,必须显式传。
3. **客户端没有红点系统、没有全局 tip 文案表、没有 UIManager、没有「登录后首拉」的先例、没有自动重连。** friend 是第一个需要"入场即拉"的功能,唯一可用的钩子是 `GameClient.OnSceneEntered`(首次登录、`RedirectToGate` 跨区落地、断线重登、同 zone 切场景都会触发它)。样板用 `PetClient`(C2S + 1 条 S2C + 列表)+ `PlayerFeaturesClient`(三重防旧回包)+ `TeamWindow` / `TeamUiRoot`(同意/拒绝列表、分页、入口文字带计数)。

**服务端已定死的契约**(逐字核自 `proto/friend/friend.proto`):服务名 `ClientPlayerFriend` 带 `OptionIsClientProtocolService`;S2C 方法 `rpc NotifyFriendEvent (FriendEventS2C) returns (Empty);` 与 10 个 C2S 方法**同处一个 service**;消息体 `FriendEventS2C { FriendEventReason reason = 1; uint64 by_player_id = 2; int64 ts_ms = 3; }`;`reason` 只有两个有效值(加 UNSPECIFIED=0):`FRIEND_EVENT_REASON_REQUEST_RECEIVED = 1`、`FRIEND_EVENT_REASON_REQUEST_ACCEPTED = 2`。

**客户端要写的逻辑**(proto-gen 只生成 handler 桩,桩里什么都不做):

1. **UI**:好友列表、待处理申请、黑名单、推荐好友。注意本项目**已弃用 FairyGUI,新 UI 一律原生 uGUI + TextMeshPro**,面板全部用代码搭(`QdaoUguiFactory` + `GameplayUiArt`,复用 `GameplayV1` 素材,不需要新美术),不是预制体。服务端只返回**收到的**申请,没有「我发出的」列表 —— UI 不要做「已发送」页。协议里没有昵称 / 等级 / 头像(`display_name` 留给玩家档案二期),用中性占位,别伪造。
2. **"收到推送就去拉"**:proto 注释原文口径 —— 推送经 `kafkautil.PushToPlayer` → `gate-cmd_g<N>`,**at-most-once**,玩家刚好掉线时这条事件会丢,所以推送只用来"立刻刷红点",真相仍要靠 `GetPendingRequests` 拉取。契约 §5 是同一条口径。
3. **登录后拉一次 + 打开好友面板时再拉一次 + 跨区落地后再拉一次**。**这才是跨区传送窗口的真正兜底**:传送期间 gate 绑定在换,窗口内发出的推送会丢(最坏约 60 秒,at-most-once 的设计内行为),只有主动拉取能补回来。**不要把红点的正确性建立在推送上。** 三个时机里"登录后"与"跨区落地"挂的是同一个钩子 `GameClient.OnSceneEntered`;注意它触发时 `InGame` 仍为 false(就绪判据用 `_net.IsReady && _net.PlayerId != 0`),且重定向期间**不触发** `OnDisconnected`(入场钩子里要自己把请求序号 +1 并清在途标志)—— 规格文档 §4。
4. **不要调 `NotifyFriendEvent`**:它和 C2S 同 service,服务端在会话拦截器的方法白名单里**刻意排除**了它(否则客户端可以直接伪造一条好友事件),服务端也不实现它(继承 Unimplemented)。客户端只做接收方。
5. **限频**:见 §2 第 4 步 —— 不配档位就吃 gate 默认 3 次/窗口,且配档位必须排在 proto-gen 之后。⚠ **被限频后客户端不得自动重试**:gate 的 `CheckMessageLimit`(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp`)每次拒绝都会计入该会话的非法包计数(`IllegalPacketCounter::RegisterAndShouldKill`),到阈值直接 `forceClose`。1008 有两条到达路径:gate 限流器回**信封级**(客户端表现为 `onError("server tip=1008")`),friend 自己的每分钟配额(仅 `AddFriend`)回**响应体**;客户端现在对 1008 没有任何专门处理,friend 两条都要接。
6. **tip 文案**:客户端没有全局 tip 表,各功能自己手抄 `switch`(`PetClient.DescribeTip` 是先例)。`FriendBlocked` 一律展示**中性文案**,不泄露拉黑方向(§2 第 2 步那条待拍板事项不管怎么定,客户端都这么做)。

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
| `matchmaking/team` | `go/match/internal/team`(+ `proto/team`) | 已落 main(09-18),随 match 进程部署(D-2:无独立进程 / catalogue / manifest,**设计如此**);**未编译未测试** | 已移植,**待编译验证** |
| `runtime/leaderboard` | 无 | 缺 | **待做**,本轮只出设计文档(§4.2) |
| `runtime/owner` | 三态 `IsNodeAlive` 已落码 | 部分 | **不搬 A**,按 B 自己的设计做 |
| `runtime/player_locator` | `go/player_locator` | 有(同名不同物) | **不搬** |
| `runtime/push` | `go/shared/kafkautil` 经 `gate-cmd_g<N>` | 有 | **不搬** |
| `social/chat` | `go/chat` | 有(v1 已落地) | 已移植;v1.1(推送 / since 游标 / TEAM 频道 / chat 私有 tip 段 18000)未做 |
| `social/dialogue` | 无 proto 也无服务 | 缺 | **前提未拍板,不开工**(NPC 刷点表 / 交互协议 / 距离判定都没人做) |
| `social/friend` | `go/friend` + `proto/friend` | **本轮已移植**,未编译未测试、导表器没跑 | 已移植,**尾巴未清** |
| `social/guild` | `go/guild` + `proto/guild` | 有(按 zone 隔离已落地);**K8s 未登记** | 已移植;**二期已开工**:20 批里 6 批已落码(B1、B1b、B2s、B2c、B3a-1、B4a-client)+ 资产通道 B4a-1 / B4b(聚宝斋会话落的),**全部未编译未测试**;剩 11 批 + B4c / BK8s 两个门禁(B3a-2 于 2026-09-20 在机器 B 上由帮会会话进行中)。现状与待办以 `docs/design/guild-phase2/92-handoff.md` 为准,归帮会会话,**不属于本移植线** |
| `social/mail` | 无 | 缺 | **待做,下一个**(§4.1) |
| `social/mission` | C++ 任务系统已接通 | 有 | **不搬 A 的 Go mission** |

**明确"不搬"的 A 仓基础设施**(B 有等价物,不要再花时间评估):`ds_allocator`、`hub_allocator`、`owner`、`push`、`data_service`、`player_locator`、`login`、`matchmaker`、`mission`。
**还真正要做的**,按优先级:**mail** → **leaderboard(先设计)** → dialogue / battle_result / P2P trade / auction(都要先拍前提)。

### 4.5 接手人开工顺序建议

0. **补齐工具链并向用户要三个拍板**:机器 B 装 Python 3.12 + openpyxl(§0.2);`FriendBlocked` 的文案改不改(§2 第 2 步);proto-gen 这批带不带客户端(§2 第 3 步陷阱一的 (a)/(b));另与帮会二期 B3a 那条线确认 data_service 三个 rpc 与 `RoleNameRule` / 三个 login tip 码可以一起发号 / 导出。
1. 跑**导表器**(§2 第 2 步)—— 否则 `go/friend` 编译不过。
2. 跑 **proto-gen**(§2 第 3 步)。已有方法的消息号保留不变,变的只有新方法落到哪个空号、以及 8 个被释放的旧 friend 号易主;gate / 路由服 / robot / 客户端的生成物**必须出自同一次**。
3. 填 `data/MessageLimiter.xlsx`(**只需填 friend 的 10 行**;team 的 12 行已在表里,regen 后复核号没漂即可),**再导表一次**。
4. **编译 + 跑测试**:main 上叠着的**全都从未编译过**,远不止 friend —— friend 三批、组队那批、聚宝斋 P1 + 通用资产通道(据 `docs/design/guild-phase2/92-handoff.md` §6 记载,`go/trade` 目前与 `go/friend` 一样编译不过)、帮会 zone 接入 + 帮会二期已落码的 6 批、跨 zone 传送阶段 1/2/3 及终审回修、战斗 G1-G9、单点加固批(C++ `KafkaProducer` 幂等化与 fatal 重建、scene_manager `world_init`、login / player_locator 租约 500s→60s 等)。完整清单与顺序以 `PROGRESS.md`「2026-09-19 单点加固交接」B 节第 1 条和 `docs/design/turn-battle-gap-closure.md` §7 为准,再按各自设计文档的验证清单逐个过;C++ 必须串行 `msbuild game.sln /m:1 /nr:false`(并发会报假的 C1041 / LNK1104)。
5. `robot friend-smoke` 七步冒烟跑通,friend 才算真的完成。
6. **然后**才开 mail M1。

---

## §5 接手前必须知道的坑

### 5.1 这个仓库同时有多个 Claude 会话在改 —— git 索引是共享的

friend 这批从落码到合完,`main` 上另一批会话一直在推进:`f06090b19` 之后到 `origin/main` 之间,friend 这条线上(`git log --merges --first-parent f06090b19..origin/main`)有 **1 次 37 冲突的大合并(`cda218956`,相对第一父 `f06090b19` 为 788 文件)+ 6 次后续追平合并**(`d924b2377` / `1eb35babb` / `a5e598179` / `88a262f2f` / `52f99780e` / `5c574bc0b`)。(早期口述说"main 前进了 5 轮",实际按**第一父链上的**合并提交数是这个形状;不加 `--first-parent` 会多数出 2 个 —— `66c546e70` / `5486e0684`,是 main 一侧带进来的 `Merge branch 'main' into feature/team-system-v3`,别的会话的组队分支,不是 friend 的。)

这些会话共用**同一个工作树、同一个 git index**。别人 `git add` 过但还没 commit 的文件,就躺在你即将提交的索引里。更糟的是还有一个**每小时 `git add -A` 的自动保存会话**:`git log --oneline` 全量里有 **21 个** `WIP:每小时保存全部进度 <时间戳>`(⚠ 提交标题里是**全角冒号** `：`,本文为排版统一写成了半角;计数请用 `git log --oneline | grep -c '每小时保存全部进度'`,拿半角 `WIP:` 去 grep 会得到 0,别误以为自动保存会话不存在。2026-09-20 复核时的数;一天前是 20 个,它每小时还在长),最近 30 个提交里占 4–5 个(随 HEAD 漂移)。它在 `add` 与 `commit` 之间的窗口会卷走任何人暂存的东西。已核实的实例:

- `105c5560b`(WIP 03:50)混进了 `data/tip/Tip.xlsx`(14231→14425 字节)、`data/RoleNameRule.xlsx`、`go/shared/playername/` 等**别人正在做的帮会二期 B3a** 内容;
- `969bbec4c`(WIP 09-18 03:56,261 文件)把一次全仓 proto-gen 的 10 份 `message_id.go` 生成物(含 `go/friend/generated/pb/game/message_id.go`)连同帮会会话的 `proto/guild/guild.proto`、`data/tip/Tip.xlsx`、`go/guild/` 等一起卷了进去 —— 注意 `go/friend/` 下只有这 1 个生成物,diff 全是 `GuildService*` 的消息号,**不是 friend 的手写改动**,别去这个提交里找 friend 代码。真正卷走 `go/friend/` 手写文件的是 `1301a4b50`(WIP 09-17 22:55,102 文件):`go/friend/friend.go`、`go/friend/internal/data/friend_repo.go`(−54)、`friend_repo_mysql_test.go`,外加 `deploy/mysql-init/guild_friend_tables.sql`(−183)—— 从注释看是帮会二期 B1 删就绪闸的那批改动;
- **标题与内容对不上的例子**:`10538f5f5` 标题是「regen:trade_table 的 resolved_by / resolve_reason 落到生成物(未编译)」,实际内容包含 `docs/ops/grafana-loki-local-logs.md`(+283/−16)、`tools/scripts/k8s_deploy.ps1`(+227/−49)、`deploy/k8s/manifests/infra/loki.yaml`、`PROGRESS.md` —— 整段 Grafana/Loki 的工作被卷了进去。

**怎么防**:

1. **动手前 `ListAgents` + `SendMessage` 问归属**,而且要**明确问**"你有没有**未提交的、和我重叠的文件**" —— 只问"你在改哪个模块"不够,索引是按文件卷的。
2. **提交一律路径限定**:`git commit -F <msgfile> -- go/friend/ proto/friend/ ...`。**永远不要 `git add -A` / `git commit -a`。**
3. **提交前 `git diff --cached` 逐行看**,不是只核对文件名。同一个文件里可能既有你的行、也有别人的行(尤其 `k8s_deploy.ps1`、`go_services.ps1`、`PROGRESS.md` 这几个"人人都改"的文件)。
4. 对方会话 idle 很久**不要假设它死了** —— 可能是撞额度,会 resume,并接着用它记忆里的那份索引状态。
5. 接手时先 `git fetch && git log --oneline origin/main..main`:**这个数每天都在变,别把本文的数字当现状**:2026-09-19 核实时 ahead 4(`ab5e7f96d` / `33ba69e20` / `2df78d4a5` / `a5ca66851`,都是别人的 Kafka / Redis / battle 改动);2026-09-20 复核时那 4 个已被推上 `origin/main`,机器 A 当时是 ahead 1(一个新的自动保存 WIP `4a04d0819`);其后该 WIP 与交接提交 `d9e471b80` 也都已推上去 —— 在机器 B 上续写开始时实测 `main == origin/main == d9e471b80`(ahead 0 / behind 0),工作区里则有别的会话 20 多个未提交改动(§0.4)。**别顺手 `git push` 把别人没准备好的东西推上去。**

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

现状:`git worktree list` 只有一个工作树(机器 A `E:/work/xuanming-server-mmo`、机器 B `D:/luyuan/wuxingqitan/mmorpg`,都是 `main`),本地只有 `main` 一个分支(`feature/port-remaining` 合并后已删)。**新开工作时请自己再拉一条分支/worktree,不要直接在 `main` 上攒改动。**

### 5.6 friend 特有的三条不变量 —— 改代码时不许破

三条的全文都在 `go/friend/internal/data/friend_repo.go` 里,但**不在同一段注释**:(a) 在文件顶部那段「全局锁序与隔离级别」注释里(到 `const friendWriteTxIsolation` 为止;标题原文中间带加粗星号,整句 grep 不中,请 grep `全局锁序与隔离级别`),设计依据是 `docs/design/friend-port-20260918.md` 的 **F6 / F7**;(b) 在 `ensureFriendCapacityRows` 的函数注释与函数体注释、以及 `lockCapacityRows` 的函数注释里,依据是同一文档的 **F8**(就绪闸退役见 **F9** 与 D-10 修订);(c) 在 `deleteFriendEdges` 的函数注释里,依据是 **F12** 末句(「Block 删边必须按 `RowsAffected` 减 `friend_count`」)。**只读顶部那段是看不到 (b)(c) 的,三处都要读。**

**(a) 全局锁序:任何写事务在拿到容量守卫之前,不得做任何锁定读。**
守卫本体是 `lockCapacityRows`(`SELECT player_id, friend_count FROM friend_capacity WHERE player_id IN (...) ORDER BY player_id FOR UPDATE`),**升序是防 ABBA 的全部依据**,顺序本身就是正确性、不是风格(升序由 `ascendingUniqueIDs` 保证)。三个写事务(`AddFriendRequest` / `AcceptFriend` / `RemoveFriend`)与 `block_repo.go` 的拉黑事务都是第一把锁就拿它。
**最容易被改回去的那一处**:`AcceptFriend` 里第 ③ 步 `SELECT status FROM friend_request WHERE from_player_id=? AND to_player_id=? FOR UPDATE` **必须留在容量守卫之后** —— 它原先排在守卫之前,与 F2 新增的 `AddFriendRequest` 权威事务正好互为 ABBA。
⚠ **不要被"B 仓当初那个 1213"的说法带跑偏**:那次修的是"过早把 `status` 从 1 改成 2 会让并发事务在 `idx_to_player` 前缀上删/插而触发 1213",修法是**把 UPDATE 延后**,**不是**把 SELECT 提前;现在下移的只是一条按 `(from_player_id, to_player_id)` 主键的**单行 SELECT**,它锁那一行、不碰 `idx_to_player` 的范围,而那条 UPDATE 依然在守卫之后。改之前先读第 ③ 步上方的注释。
> **更正**:早期交接口述提到"B 侧有一段老注释讲『先用主键锁定并验证申请』"。**当前 main 上找不到这条字面注释**(`go/friend/`、`friend-port-20260918.md`、`friend-persistence-architecture.md` 全文 grep 均无),它应该在落码时就被替换成了正确的 F6 说明。风险本身仍成立,只是不会再有一条误导人的残留注释。

另外两处**刻意不加 `FOR UPDATE`** 的普通读(`AddFriendRequest` 的第 ⑤⑥ 步两个 pending 计数)**别"顺手补上"** —— 理由见 §3 第 3 条。
隔离级别固定 **READ COMMITTED**(`beginWriteTx`):RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类只碰不同行的事务互相挡,**且只在 MySQL 上炸,TiDB 没有间隙锁恒绿**。"判定读在守卫之后用当前读"在 RC 下**不是可选优化,是正确性要求**。

**(b) 缺容量行时,按 `friend` 表的权威边数建行,绝不猜 0。**
实现在 `ensureFriendCapacityRows`:先 `SELECT COUNT(*) FROM friend WHERE player_id = ?` 得到权威值,再 `INSERT IGNORE INTO friend_capacity (player_id, friend_count, created_ms) VALUES (?, ?, ?)`(收尾批起是三列;`created_ms` 的含义与回收的关系见 §9.2)。收尾批之后这张表**有了删除方**(sweep 的 `SweepIdleCapacityRows`,只删零好友且足够老的行),所以 (b) 比以前更要紧:被回收的行下次就是靠这里按权威边数重建的。**为什么不能写 0**:将来若有任何路径先写了 `friend` 边再补容量行,猜 0 会让这个玩家的硬上限**凭空放宽一轮**,且**全程零报错**。调用顺序也是不变量的一部分:**先在事务外、按 `player_id` 升序、自动提交地补齐行**,**再**在事务里按升序 `FOR UPDATE` 锁双方容量行。
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

**怎么防**:凡是从 go-redis 往 go-zero 改写的地方都查一遍这一类 —— `GetCtx` / `GetExCtx` / `GetDelCtx` / `GetSetCtx`(go-zero v1.10.0 `core/stores/redis/redis.go`,四个方法体都核过;注意是 `GetExCtx`、大写 E)"键不存在"的语义在 go-zero 里都是**零值 + nil error**。搜索关键词:`redis.Nil`、`errors.Is(err, red.Nil)`、`if err == redis.Nil` 的残留写法。

---

## §6 存疑与未核实项(如实列出)

**本文的全部结论都来自读源码,没有任何一条经过编译器 / 类型检查 / 运行时确认。** 机器 B 虽然有 Go,但按 `AGENTS.md §10.1`,Claude 不跑构建 / 测试命令,续写会话同样一条都没跑;下面凡写"本机"而未注明的,指的是机器 A。

1. **机器 A 没有 Go 工具链**,所以"零调用方""符号不存在""签名对得上"这类结论都只有 grep 支撑。特别是**接口断言式的间接引用 grep 未必抓得到** —— **以首次 `go build ./...` 为准。**
2. **§3 第 2 条的三条 `EXPLAIN` 假设在本机无法核对**(没有 MySQL 实例)。优化器在小表与大表上的选择不同,`friend_guard_lock_order_mysql_test.go` 的绿灯不能替代它。
3. **本机 mysql 容器的实际版本没有实测**(Docker 未启动):`deploy/docker-compose.yml` 钉的是 `mysql:latest` 不是固定 8.4,K8s 侧用的是 `mysql:8.0`。所以 §2 第 6 步写的是"跑之前先 `SELECT VERSION()` 确认是 8.4+ 的真 MySQL,必要时把 image 钉到 `mysql:8.4`",而不是断言本机就是 8.4。
4. **§3 第 1 条的两条回收修法都只是纸面设计**,尤其路线 2 的 DELETE 与现有锁序如何共存(它会去锁守卫行)**未经推演验证**,落码前应由接手人自行重做一遍 ABBA 分析。
5. **§3 第 7 条"删字段不需要刷缓存"**基于 `encoding/json` 默认忽略未知字段这一标准行为,**未在本仓实测**。
6. **proto-gen 全量重跑后 friend 的 C++ 产物是否真会被重写,是推断而非实测**:`proto_gen.yaml` 的 friend 块只声明了 go outputs(与 guild/trade 同形),但 `cpp/generated/proto/{guild,trade,team}/` 都存在,说明 cpp proto 由另一条全局路径产出。同理 `friend_service_metadata.h` 会不会被改名成 `ClientPlayerFriend*` 也只能等实跑。
   顺带说明一个**生成物的既有形状(不是本批引入,也不是 jubaozhai 独有)**:凡是在 `proto_gen.yaml` 里只声明 go outputs 的服务,其 `cpp/generated/rpc/service_metadata/*_service_metadata.h` 里的 `#define XxxMethod ::Xxx_Stub::descriptor()->method(N)` 都指向各自 `.pb.h` 里**不存在**的 `_Stub`(没开 `cc_generic_services`)—— chat 2 个(`::ClientPlayerChat_Stub`)、team 15 个、guild 18 个(`::GuildService_Stub`)、jubaozhai 4 个(`::ClientPlayerJubaozhai_Stub`)、friend 现行 8 个(`::FriendService_Stub`)全是如此,在 `cpp/generated/proto/` 整棵树里 grep 这些符号均为 0。这些头已经被 `rpc_event_registry.cpp` 和各 `*_grpc_client.h` include 进编译单元,但它们是宏、全仓(`cpp/` 下)没有任何展开点,所以不报错。friend 重生成后出现同样不存在的 `::ClientPlayerFriend_Stub` 属正常,**不要当成 proto-gen 故障去修**;只有哪天有人在 C++ 里真的展开这些 `*Method` 宏才会炸。
7. **第 3 步跑完后 `kMaxRpcMethodCount` 的期望值是 234**(228 − 8 个释放号 + 11 个 friend 方法 + 3 个 data_service 新 rpc;算法见 §2 第 3 步)。这是按 `proto/` 下全部 service+rpc 与 `message_id.txt` 做差集、再按 `proto_gen.yaml` 的 `domain_meta.source` 过滤算出来的,**未实跑**;"等于 `message_id.txt` 最大 id + 1"是不变判据,234 是预期值 —— 对不上 234 时先查是不是又有别的会话加了 rpc,而不是先怀疑生成器。哪个新方法落到哪个号无法预判(Go map 迭代顺序)。
8. **robot 与 Unity 新 handler 的确切文件名**是按现有 team 的命名规律推导的(`client_player_friend_notify_friend_event.go` / `ClientPlayerFriendNotifyFriendEventHandler.cs`),**未实跑**。robot 那 11 个 `game.ClientPlayerFriend*MessageId` 常量名同理。
9. **§2 第 4 步的限流档位数值**(读 10/s、写 5/s、推荐 1/s)是建议值,**不是仓内既有约定**,没有代码或文档背书。`data/MessageLimiter.xlsx` 的现状已于 2026-09-20 在机器 B 上解包核对(与 §2 第 4 步写的数字一致):单 sheet、无 `sharedStrings.xml`(内联值),第 1 行表头 `id / max_requests / time_window / tip_message`,数据占第 6–58 行共 53 条,按**数字消息号**登记;其中没有 friend 的任何旧号(2/7/11/12/53/76/119/120),即 friend 现在吃 gate 默认档。注意该表不含方法名,grep "friend" 字样天然为 0、不是有效判据;新行必须等第 3 步 proto-gen 定出 11 个新消息号之后按号填。
10. **§4 的端口分配**(mail 50900/:9240、rank 51000/:9250)来源是会话 scratchpad 的排批稿;仓内唯一的落点是 `PROGRESS.md` 的 F1 条目「配置」一条末尾的一句话,**没有进任何代码、部署脚本、契约 §7 的端口分工表或设计文档**。交叉核实结果:50900 / 9240 / 9250 全仓零命中;**51000 不是空号**(login 的 staging 端口 + `-Zone 3` 位移后撞 login 53000,见 §4.2),rank 正式定端口前必须换号。本地一键栈的 login 在 53000,所以 51000 与本地单区 dev 栈不撞,撞的是 login-stack compose、staging 口径与三 zone 本地栈。
11. **§4.1 mail 的表清单、事务顺序、sweep 口径、20 人日 / 46 文件的估算**全部来自 `scratchpad/port_plan_mail.json` 这份摸底产物,**仓库里没有任何对应代码或设计文档可交叉核实**,其自身 11 条 `decisions_needed` 也没有拍板记录。**当作待评审提案,不是既定设计。**
12. **§4.2 "leaderboard v1 只做 ZSET 榜 + 客户端读"属于建议而非拍板** —— 仓内没有 leaderboard 设计文档。
13. **§2 第 8 步"经 gate 调一次 GetFriendList"的具体手法未核实**:`Mode: dev` 下 friend 开了 gRPC reflection,理论上 grpcurl 可直连 50400,但 session 拦截器对"无会话 metadata"的处理是"内部调用放行",语义与经 gate 的真链路不同。建议直接用第 9 步的冒烟触发。
14. **客户端仓已于 2026-09-20 在机器 B 上做了只读摸底**(用户授权只读;机器 A 当时只读过 `tools/gen_proto.ps1` 与 `Net/Generated` 的目录清单),结果见 `docs/design/friend-client-spec-20260920.md`。该规格自己的未核实清单(§9)里最要紧的三条:① `OnSceneEntered` 触发那一刻 friend 服务对该会话是否已可达(首拉会不会拿到 1003);② protoc 35.1 生成的 C# 与客户端内置 Google.Protobuf 3.28.3 运行时是否兼容;③ 与别的正在改客户端协议的会话是否有同批生成冲突。**修改客户端仓的授权尚未取得。**
15. **§5.3 "解冲突时连踩三次吃括号"是过程性事实,无法从 main 的现行代码反查**(冲突已解、痕迹不在 git 历史里);"括号配平比差值"这条判据本身也未实跑。PowerShell 解析器那条手段本轮**没有实跑**。
16. **"每小时 `git add -A` 的自动保存会话"**:WIP 提交的存在、间隔与跨模块内容都核实了,但"它用的是 `git add -A`"是从提交内容横跨多个不相关模块推出的**推断**。
17. **§5.5 "从别的 worktree 用 `update-ref` / `branch -f` 挪 main 会造成静默回退"**这条机理**没有在本仓复现**,reflog 里也没有该类条目 —— 它作为告诫保留,不是本次实际踩过的事故。
18. **`cda218956` 的"37 处冲突"来自提交标题自述**,没有独立方法重算(只能核实它是 merge commit 且 788 files changed)。
19. **A 仓各服务的代码行数**来自 scratchpad 的 `port_ledger.json`,未逐个统计核实,本文刻意不引用这些数字。
20. **`robot/etc/friend_smoke.yaml` 的"七步"**取自 `PROGRESS.md` 的 F3 条目与任务背景描述,本轮只读了该 yaml 的头部注释,未逐条独立核对。
21. **"`data/tip/Tip.xlsx` 的三个新 friend 码是哪次提交加进去的"**只核实到"xlsx 里已有、生成物里没有"这个事实状态。
22. ⚠ **本仓同时有多个会话在提交,首轮核实的是 2026-09-19 的快照(HEAD `a5ca66851`),续写复核的是 2026-09-20 的 `d9e471b80`。** 接手人开工前请重新 `git pull` 并按**符号名**复核一遍 —— 行号几乎肯定已漂移,个别条目也可能已被别的会话顺手做掉。
23. **机器 B 的三条环境未知数**:`GOPROXY` 默认值(`proxy.golang.org`)在机器 B 上通不通未实测;Docker 守护进程没起,所以 mysql / redis / etcd / kafka 容器在机器 B 上是否存在、什么版本,一概未核实;`bin\go_services\friend.exe` 旧构建会卡住一键启动(§0.3)这条后果是读脚本推出来的,未实跑。
24. **续写复核的边界**:§8 的 50 条更正都经过两个独立 agent 各自对着代码核对;但复核 agent 同样只读、不编译。被驳回的 2 条(§2 第 1 步"三个 module 的 go 指令"、§7 契约文档那一行的内容摘要)属于"可改可不改的润色",正文保持原样。客户端规格文档没有经过第二道复验,只由续写会话抽查了 10 个关键符号(全部属实)。

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
| **D-11** | Go 服务注册的失租口径:**先重夺原 id;抢不回时,无状态全局服务换 id 继续,持久身份服务退出** |
| **D-12** | 新 Go 服务的客户端入口:**只承诺路由服模式;「翻转」落在部署层,C++ 默认值不改** |
| **D-13** | 全局服务 yaml 不注册 go-zero 发现键:**有 `Etcd` 段就显式写 `Key: ""`,不写 `<svc>.rpc`;等首个 Go 调用方出现再同批拍 `.z<N>` 豁免** |
| **D-14** | 新全局服务的库归属与建表方式:**每服务一库;表以 proto 为源;迁移用 go/db runner 的语义,抽成独立 module,由服务二进制 `-migrate` 在 K8s Job 里跑**(那个独立 module 即 `go/schemamigrate`,见该条标题下的落地说明与结论 3) |

### PROGRESS.md 条目

- `## 2026-09-18 friend 移植 F1 批:协议改名可达化 + 表以 proto 为源 + 服务骨架(Claude,未编译)`
- `## 2026-09-18/19 friend 移植 F2 批:事务重写(RC + 全局锁序)+ 拉黑/推荐/配额/清理/推送/在线状态(Claude,未编译)`
- `## 2026-09-19 friend 移植 F3 批:五处登记 + K8s 部署链 + 迁库 + 运维工具改连 + robot 冒烟 + 设计文档(Claude,未编译)`
- `## 2026-09-19 单点加固交接:已落码清单 + 剩余工作与验证步骤(Claude,全部未编译)`(B.1 明确写着"`go/friend` 当前编译不过是 friend 移植会话的既定状态")
- `## 2026-09-20 friend 移植交接:交接文档续完(换机复核 + 客户端只读摸底)(Claude,未编译)`

### 主要代码位置

| 路径 | 内容 |
|---|---|
| `proto/friend/friend.proto` | `service ClientPlayerFriend`(10 C2S + `NotifyFriendEvent`) |
| `proto/friend/friend_table.proto` | 四张表的唯一事实源 |
| `go/friend/friend.go` | 入口、`-migrate` / `-allow-modify`、`ensureSchema`、生命周期 |
| `go/friend/internal/data/` | `friend_repo.go`(锁序说明 + 权威事务)/ `block_repo.go` / `recommend_repo.go` / `sweep_repo.go` / `session_reader.go` / `tables.go` |
| `go/friend/internal/logic/` | `friend_logic.go` / `recommend.go` / `rate_quota.go` / `sweep.go` / `push.go` |
| `go/friend/internal/{session,lifecycle,metrics,config,svc,server,constants,kafka}/` | 拦截器链 `grpcstats → killswitch → session → serverbase`(`friend.go` 的 `buildUnaryInterceptors`)、`kafka/gate_command_builder.go`(S2C 推送组 gate 命令;由 `svc/servicecontext.go` 装配、`logic/push.go` 经 `kafkautil.PushToPlayer` 使用)等 |
| `go/friend/etc/friend.yaml` | 端口 50400 / metrics `:9180` / `Sweep.Mode: report_only` / `DBName: mmorpg_friend` |
| `robot/friend_smoke_scenario.go` + `robot/etc/friend_smoke.yaml` | 跨区七步冒烟 |
| `deploy/k8s/manifests/go-svc/friend.yaml` / `friend-migrate.yaml` | 部署与迁移 Job |
| `deploy/mysql-init/00_init_zone_dbs.sql` | `mmorpg_friend` 建库的**唯一登记处** |
| `tools/merge_zone/`(`main.go` 的 `-friend-schema` flag 与已删的 `-friend-redis-*` / `audit_resources.go` 的 `defaultFriendSchema`、`validateFriendSchemaName`、`auditFriend`、`auditFriendRequest` / `audit_checks.go`(`friend:online` 证据面已删,只留 `player:session`)/ `integration_test.go`;库名正则 `schemaNamePattern` 定义在 `player_rows.go`,friend / guild / trade 三家共用,该文件本身没有 friend 代码)、`tools/data_consistency_check/main.go`(`-friend-mysql-dsn` 第二连接 + `checkFriendOrphans`) | 运维工具的 friend 改连 |
| `docs/design/friend-client-spec-20260920.md` | Unity 客户端任务的完整规格(生成管线 + 手写层;2026-09-20 只读摸底) |

---

## §8 续写复核记录(2026-09-20,机器 B)

**做法**:7 个 agent 按章节把本文的可核对论断(文件 / 符号 / 常量 / SQL / 日志原文 / 计数 / 提交号 / 调用方清单)逐条对着 `d9e471b80` 的现行文件核对,共 **606** 条;报出的 **52** 条差异各交给另一个独立 agent,按"默认文档是对的、差异报告是错的"重新取证,**确认 50 条、驳回 2 条**。确认项已全部就地改进正文。全程只读,没有跑任何构建 / 测试 / 生成命令。

下面只列**会让接手人做错事**的那一类(其余是数字、名称、表述层面的更正,不再罗列):

| 位置 | 早期版本的说法 | 更正后 |
|---|---|---|
| 开头 | 悬空引用三类 | **五类**:还缺本批新增的 message 类型(`BlockRequest` 等 12 个)与 `friend_table.pb.go`(四张表的 Go 产物**从未生成过**)|
| 开头 / §0.3 | `bin/go_services/` 下没有 `friend.exe` | 机器 B 上有一份 **08-02 的移植前旧构建**,会让 `start_game.ps1` 的"缺 exe 跳过"失效 |
| §1 F1 | 看旧 SQL 原文用 `git show f06090b19:…` | 要用 **`f06090b19^`** —— `f06090b19` 自己已经把 friend 三张表删了 |
| §2 第 2 步 | 不需要再改 xlsx,直接跑导表器 | `FriendBlocked` 的文案「对方已将你拉黑」与"不泄露方向"的设计意图矛盾,**先拍板**;导表 diff 里还会带出 `RoleNameRule` 新表与 3 个 login 码(B3a 的内容)|
| §2 第 3 步 | 改名会让**全仓**消息号重新洗牌 | **已有方法原号保留**;变的只有新方法的落号与 8 个被释放旧号的易主 |
| §2 第 3 步 | (未提)| **data_service 的 3 个新 rpc 会一起抢号**,regen 后 `kMaxRpcMethodCount` 期望 234;跑前先和 B3a 那条线对齐 |
| §2 第 3 步 | 客户端 `gen_proto.ps1` 补 friend 两行即可 | 还缺 **team 与 jubaozhai**,默认配置会给客户端新增 **31** 个桩;`-ProtoRoot` 默认值在两仓同级布局下是错的,必须显式传 |
| §2 第 3 步 | Agones 块在守护段**外**,regen 必吞 | 已进守护段;**只有 `cecb52995` 之前编的陈旧生成器才吞** —— 机器 B 的 exe 正是 |
| §2 第 4 步 | manifest 期望 46 → 63 | 按硬顺序走是 **53 → 63、version 18 → 19**(第 2 步已经把那 7 行导进去了)|
| §2 第 7 步 | 库里恰好四张表 | **5 张**,多一张 `schema_migrations` 台账表,别删 |
| §2 第 8 步 | 用第 9 步的冒烟触发缓存核对最稳 | **不能用**:冒烟跑完列表键必为空(会误判成修复丢了),而且冒烟根本走不到补 `"0"` 那条分支。改为补一条 miniredis 单测 |
| §2 第 9 步 | 看到 WARN 就说明 friend 没起 | 那条 WARN 只有 `start_game.ps1` 打;两区冒烟用的 `dev-start-zones` **没有库预检、不打 WARN**,以 `z<N>_friend.stdout.log` 里的 `STARTED SUCCESSFULLY` 为准 |
| §3 第 1 条 | 无条件建容量行的路径有两条,`AcceptFriend` 更松 | **只有 `AddFriendRequest` 一条**。`AcceptFriend` 早有事务外 pending 预检和回归断言 —— 这条是机器 A 复核时**改错**的,别照着"再加一道快速失败" |
| §3 第 1 条 | 删容量行可自愈 | 不放宽上限,但会撞上 `lockCapacityRows` 缺行时**刻意 fail-closed → `ErrStorage` 告警**的契约;回收路径必须同批处理这一点 |
| §3 第 8 条 | `TestAddFriend_WritesUpdatedMs` 逐个钉了五处写入 | 只真钉了 3 处;另两处的 `NotZero` 断言因 `seedPending` 直写非零值而**结构性不可能失败** |
| §4.2 | team 还没进 `$ServiceCatalogue` / k8s,是组队的尾巴 | **设计如此**(D-2:team 随 match 进程,不新增进程 / 端口 / manifest),**不要去补**;team 的限流档位也早已填好 |
| §4.2 | rank 51000 全仓未占用 | **已被 login 的 staging 档占用**,且 `-Zone 3` 位移后撞 login 53000;另挑端口(如 51100)|
| §4.3 | 没有 `NotifyFriendEvent` 的 Unity handler 这次改名就白改了 | Unity 的生成桩**从未被接线**(`HandlerRegistry.Register` 零调用,接上反而会盖掉手写处理器);接线要手写,见客户端规格 |
| §4.4 | 帮会二期五块增量未开工 | **已开工**,6 批已落码,归帮会会话 |

**上一个会话的早期说法(对用户的口头汇报或摸底稿)里,被核实推翻或修正的**(转述给别人时请以此为准):

1. "这台机器上 go 和 protoc 都不存在" —— 机器 A 上 protoc 在(`third_party/grpc/install_vs2026_dbg/bin/`),缺的只有 Go;机器 B 上 Go 与 protoc 都在,缺的是 Python。
2. "leaderboard 要消费 match 的 `PlayerRatingChangedEvent`" —— 这个事件**不存在**,全仓零命中;match 不对外发布任何评分事件,这条东西向边要新造(§4.2)。
3. "leaderboard 依赖的事件在组队会话手里 / `go/match` 归组队会话所有" —— 组队已于 09-18 全量合进 main,那两个 worktree 都没了。
4. "mail 的附件发放要等帮会二期的通用资产账本" —— 资产通道**已经在 main 上**;真正的阻塞是 `kAssetOpCallerRules` 白名单里没有 mail(SYSTEM_CREDIT 流没开),§4.1。
5. "friend 的收尾约 8 个文件、8 条" —— 现在是 9 条(新登记 logic 包头的过期注释),且第 7 条是 2 文件、第 8 条连带 2 个测试文件。

---

## §9 收尾批(2026-09-20,机器 B,接手会话)—— 现状、偏离、用户执行序列

> **全部未编译、未运行、未跑测试。** 本批只做了 `gofmt -l` / `gofmt -e`(语法解析与格式),本批改过的 19 个 `.go` 输出为空。按用户 2026-09-20 的口径:Python 由用户自己装,编译 / 导表 / proto-gen / 测试由用户(或 Codex)自己跑,Claude 会话只负责把代码做完。

### 9.1 三个拍板的结果

| 拍板 | 结果 | 落点 |
|---|---|---|
| ① 装工具 | 用户自己装 / 自己起 Docker。⚠ **后来查明本机已有 Python 3.14.7**(`py -3`;`python` 是商店占位桩),真实缺口只有导表器的 4 个依赖,见 §0.2 订正行 | — |
| ② `FriendBlocked` 文案 | **改 xlsx 为中性文案「无法添加该玩家为好友」**;代码里"不区分方向"的注释保持 | 当时以为本机没有 Python(误判),写成幂等脚本 `tools/scripts/friend_xlsx_patch.py tip-text`:按码名定位(不写死行号 —— 帮会 B5a 的脚本会在它上方插 10 行),改前改后按段计数核对,现值既非旧文案也非新文案时中止不覆盖 |
| ③ proto-gen 带不带客户端 | **带**。用户授权修改客户端仓 | 见下方「客户端仓基线已变」—— 现状是**只差 `friend.proto` 一行,且已在客户端工作区补上(未提交)** |

**⚠ 客户端仓基线已变(2026-09-20 05:48–05:50,续做时核实)**:本会话最初在客户端旧基线 `d2b165a` 上给 `gen_proto.ps1` 补了 `team` / `jubaozhai` / `friend` 三行;随后有人(stash 名是 `codex-before-client-update-20260920`)先把工作区存进 `stash@{0}`,再把客户端 **fast-forward 到 `120e2d8`**(`merge origin/main`,把机器 A 那边积压的客户端提交拉了下来)。新基线的事实:
- `gen_proto.ps1` 的 `$files` **已自带** `proto/team/team.proto` 与 `proto/trade/jubaozhai.proto`(机器 A 的提交,另外还收了 guild / scene / trade / common / team 五个 `generated/code/proto/tip/*_error_tip.proto`);
- `Assets/Scripts/Net/Generated/Handlers/` 已有 **92** 个 handler(Team 15 / Jubaozhai 4 / `SceneSceneClientPlayerTravelToZone` 都已生成),`Team.cs` / `Jubaozhai.cs` 也在;
- 工作区里已有人**只**把 `proto/friend/friend.proto` 一行重新补上(未提交)—— 这就是现在缺的全部。
- 由此:**不要 `git stash pop`**(那份 stash 基于旧基线,pop 会冲突或把 team/jubaozhai 加成重复行);跑 proto-gen 之前只需确认 `friend.proto` 那一行在。§2 第 3 步陷阱一里"客户端一次新增 31 个桩"在新基线上变成**只新增 Friend 的 11 个(92 → 103)**,`HandlerRegistry.cs` 被重写。
- ~~`friend_error_tip.proto` 不需要加进客户端清单~~ **已更正并已加**(客户端规格复核推翻了这条):新基线里 guild / scene / trade / common / team 五个域的 `*_error_tip.proto` 都已在清单里,客户端按生成的枚举写分支(`(uint)trade_error.KTradeXxx => …`,先例 `JubaozhaiClient.DescribeTip` / `GuildClient` / `GameClient.DescribeTravelTip`),**不手抄 `Tip.xlsx` 的数字**。friend 照 trade / team 的形状成对收录,`generated/code/proto/tip/friend_error_tip.proto` 已补在客户端工作区 `friend.proto` 之后(未提交)。**时序**:这份 proto 现在只到 15006,客户端 `gen_proto.ps1` 必须在服务端导表器跑完(15007–15009 落表)之后再跑,否则 `FriendErrorTip.cs` 缺码 —— §9.4 的顺序(导表 → proto-gen → 3b 客户端生成)天然满足。
- 两个客户端生成脚本的 `-ProtoRoot` 默认值在新基线上**仍是** `$PSScriptRoot/../../..`(按旧的子模块布局写的),两仓同级布局下仍必须显式传。

另:帮会 B3a 会话确认 data_service 的 3 个 rpc、`RoleNameRule` 新表、3 个 login tip 码是终态,可以随这次一起发号 / 导出;`MessageLimiter` 已改未导的那 7 行它不认领也不反对。

### 9.2 本批落了什么(相对 `6c6451359`)

**`friend_capacity` 回收(§3 第 1 条,P2)—— 路线一**

- `proto/friend/friend_table.proto`:`FriendCapacityRecord` 加 `uint64 created_ms = 3` 与 `OptionIndex = "friend_count,created_ms"`。`created_ms` 的含义是"**本行被(重新)建出、或最近一次 `friend_count` 减少的时刻**",回收的保留期从这一刻起算。与 `updated_ms` 相反,`created_ms = 0` 的存量行是**安全的**(零好友行被回收后按权威边数重建)。
- `go/friend/internal/data/sweep_repo.go`:新增 `SweepIdleCapacityRows`,与 `SweepTerminalRequests` 共用同一份 `Friend.Sweep` 配置(**没有新增任何配置键**,K8s ConfigMap 的契约值不用动),入参校验与"负截止点 = 清空整表"的护栏抽成 `sweepCutoffMs` 一份。
  **SQL 形状是防 ABBA 的关键,不许改成一条批量 DELETE**:候选用**普通读** `SELECT player_id FROM friend_capacity WHERE friend_count = 0 AND created_ms < ? LIMIT ?`,再逐行、自动提交、按主键 `DELETE ... WHERE player_id = ? AND friend_count = 0 AND created_ms < ?`(WHERE 里重复的两个条件是提交点复核)。批量 `DELETE ... LIMIT` 会在一个语句事务里按**二级索引序**锁多行守卫行,与业务写事务"按 player_id 升序"的取锁顺序不同,可以成环;逐行删时回收任一时刻至多持一把守卫锁且持锁时不再等别的锁,不可能处在等待环里。
- `go/friend/internal/data/friend_repo.go`:四条写路径(`AddFriendRequest` / `AcceptFriend` / `RemoveFriend` / `Block`)的外层骨架收成一份 `runGuardedWrite`(事务外 ensure → `BeginTx(RC)` → ① `lockCapacityRows` → body → Commit);步骤编号、SQL 文本、哨兵、注释原样搬进闭包,事务外前置判定仍在它之前、不进重试。`lockCapacityRows` 缺行时返回包内哨兵 `errCapacityRowsMissing`,`runGuardedWrite` 回到事务外重新 ensure 并重试,**上限 `capacityGuardMaxAttempts = 3`**;用尽后哨兵原样上抛,logic 照旧定性 `ErrStorage`(fail-closed 不变)。缺行只可能发生在事务的第一条语句,此时没有任何副作用,整遍重跑安全;body 在一次调用里至多执行一次。
- `go/friend/internal/logic/sweep.go` + `internal/metrics/metrics.go`:`SweepStore` 多一个方法;`runSweepRound` 拆成两段依次跑(前一段失败不跳过后一段,共用一个单轮预算,未知 mode 的错误日志一轮只打一次);新 Gauge `friend_sweep_idle_capacity_rows{mode}`,两个 mode 预建 0 值。

**评审轮挡下的三条真问题**(六维度评审 15 条发现,逐条经独立反驳式复验,全部确认并已处理):

1. **回收给事务外的 ensure 带来了 1213。** InnoDB 手册里的经典形态:一方对某主键记录持 X(sweeper 的 DELETE,或另一个事务的守卫 `FOR UPDATE`),至少两个 `INSERT IGNORE` 同时排队等同一条记录的 S;X 释放后它们同时拿到 S、又都要升 X → 互等成环,其一得 1213。回收上线之前没有人删这张表,这个形态不存在;ensure 在事务外,它的 1213 会被定性成 `ErrStorage`。修法:`ensureFriendCapacityRows` 对每个玩家的 COUNT + INSERT 这一对语句做有上限的重试(`ensureDeadlockMaxAttempts = 3`,只认 `isMySQLDeadlock` = `errors.As` 到 `*mysql.MySQLError` 且 `Number == 1213`,每遍先查 `ctx.Err()`);自动提交且幂等,重试安全。`runGuardedWrite` 里 **body 的 1213 仍不重试**。⚠ 这条是**按手册推演,未在真库复现**;回归用例 `TestEnsureCapacityRows_ConcurrentInsertOnReclaimedRowSurvivesDeadlock` 照手册示例确定性编排(要读 `performance_schema.data_lock_waits`,读不了时平时 SKIP、设了 `FRIEND_REQUIRE_MYSQL_TESTS` 时 Fatal)。未采用的备选:把 `INSERT IGNORE` 换成 `INSERT … ON DUPLICATE KEY UPDATE player_id = player_id`(重复键上直接取 X,从根上消掉 S→X 升级),它会改热路径的取锁强度,要在真库上评估后再定。
2. **"删行无害"的论证不完整:回收新引入了 `friend_count` 永久偏大 1 的交错。** ensure 的 `COUNT(*)`(读到 1)与 `INSERT IGNORE` 不原子,中间夹进 `RemoveFriend` 提交(count→0)+ 回收删掉这行老行,随后 INSERT 用陈旧的 1 建行 —— 方向是 fail-closed(玩家少一个好友位),但**永久、无自愈**。修法:`deleteFriendEdges` 减计数时一并刷新 `created_ms`(签名多一个 `nowMs int64`),让"刚减过计数的行"在一个保留期内不可回收;残留是陈旧 ensure 的窗口要跨过整个 `RetentionDays`(≥ 1 天),视为不可达。**不要**改用单条 `INSERT IGNORE ... SELECT COUNT(*)` 合并 ensure:它在连接默认 RR 下会对 `friend` 表加共享 next-key 锁,违反锁序 (2),且只缩小不消除窗口。
3. **`go/schemamigrate` 对已存在的表不补建索引**(只出 warning、退出码 0)—— 见 §2 第 7a 步新增的那条通过标准。

**其余收尾项**:删 `AreFriends` / `HasPendingRequest` / `CountOutgoingPending`(#5);删 `data.FriendEntry.LastActiveMs`(#7;存量缓存里的旧 JSON 键解码兼容,不需要清 Redis);`metrics.SweepModeReportOnly/Delete` 导出并在 `TestSweepModeConstantsAreTheWireLiterals` 里钉住 config / data / metrics 三份一致(#4);`updated_ms` 注释重写、两处假绿断言改成先归零再断言(#8);`friend_logic.go` 包头与 `constants.go` 的过期注释(#9);⑤⑥ 旁"AcceptFriend 会让 pending 变大"的不严谨说法更正(#3)。
`friend_repo.go` 在 HEAD 上本来就过不了 gofmt(顶部长注释紧贴 `const friendWriteTxIsolation` 成了 doc comment,gofmt 会把分条论证当代码块重排),本批在两者之间插了一个空行解决,原注释一字未动。

**新增 / 修改的测试**(48 个新用例;`*_mysql_test.go` 与 `sweep_repo_test.go` 挂 `FRIEND_TEST_MYSQL_DSN`,其余不依赖任何库):

| 文件 | 钉的是什么 |
|---|---|
| `data/friend_cache_test.go`(新,miniredis)| §2 第 8 步那条分支:generation 键从未写过时仍能回填(修复丢了 → 脚本恒 return 0、缓存永远写不进去、零报错);加载期间 generation 被 INCR 时不得回填旧快照 |
| `data/session_reader_test.go`(新,miniredis)| 分批、缺席 = 离线、坏 payload 只降级该玩家、**只认 `SESSION_STATE_ONLINE`** |
| `data/recommend_repo_mysql_test.go`(新)| 两条 query 各自的四类排除 —— 每一类都造了"排除子句失效则候选集出现该人"的数据(冒烟里那条是结构性假绿,别拿它当证据)|
| `data/sweep_repo_test.go` | `SweepIdleCapacityRows` 八条,含 `TestDeleteIdleCapacityRow_RechecksAtCommitPoint`(提交点复核的两个条件各有一条变异必红的断言)|
| `data/friend_guard_lock_order_mysql_test.go` | 场景从 5 个变 8 个:交叉 pending 行(今天应当绿;谁给 ⑤⑥ 补上 `FOR UPDATE` 它会红)、回收与写路径并发、ensure 撞 1213 |
| `data/friend_repo_mysql_test.go` | 夹具 DDL 加列加索引;`TestRunGuardedWrite_ExhaustedMissingRowsFailClosed`(用 `CHECK` 约束注入"ensure 成功但行建不出来",断言哨兵、body 0 次调用、无落库;"`INSERT IGNORE` 违反 CHECK 时降为告警并跳过该行"是凭记忆写的前提,用例开头有夹具前提断言兜底,前提不成立时红因一眼可辨);`TestDeleteFriendEdges_RefreshesCreatedMsOnDecrement`(`RemoveFriend` / `Block` 两条子用例:减到 0 后**立即**回收不得删、推过保留期才删 —— 把 UPDATE 里的 `created_ms = ?` 删掉必红)|
| `logic/sweep_test.go`(新)| 两段都被调用且入参与配置 / 固定时钟逐字一致、前段失败后段仍跑、未知 mode 不进 Gauge label、ticker 真的在跑且取消即退出 |

### 9.3 对冻结规格 / 用户拍板原话的偏离(AGENTS §11 要求登记)

1. **缺行重试上限是 3 遍,不是拍板选项原话里的"重试一次"(2 遍)。** 论证:一次写至多涉及两行守卫行;本表唯一的删除方是回收,而 DELETE 在提交点复核 `created_ms < cutoff`,被 ensure 重建的行 `created_ms` 是当前时刻,所以**每行至多被删一次**(副本再多也一样)→ 因回收而缺行至多发生两遍,第三遍必过。2 遍时"双方都是陈旧零好友行、两次删除各落进一个窗口"会打出一次 fault;3 遍把它变成确定性保证,代价只是真不变量破裂时在 fault 之前多跑一次 ensure 和一个空事务。前提:各副本墙钟偏差小于 `RetentionDays`(`config.Validate` 保证 ≥ 1 天)。不认可就把 `capacityGuardMaxAttempts` 改回 2,并回退 `friend_repo.go` 锁序说明 (5)、`block_repo.go` / `sweep_repo.go` / `logic/sweep.go` 文件头里"至多重试两次"的文案,以及 `TestCapacityRowReclaimRacesWithGuardedWrites` 里双陈旧组的硬断言(改回容忍)。
2. **`created_ms` 不只在 INSERT 时写**,`deleteFriendEdges` 减计数时也刷新(原因见 9.2 评审第 2 条)。

### 9.4 用户执行序列(机器 B:`D:\luyuan\wuxingqitan\mmorpg`)

**顺序是硬约束。** 帮会 B5a 会话的脚本与本批的脚本都要 load→save 同一个 xlsx,**串行跑,不要并发**;顺序已与 B5a 会话对齐。

```powershell
# 0. 一次性前置。本机已有 Python 3.14.7(**用 py -3,别用 python** —— 那是商店占位桩;在 Claude 的 Bash 里敲
#    python 不报错而是挂到 120s 超时),导表器 4 个依赖 21:57 起也已装好(dev.bat export 每次还会自己补装)。
#    Docker Desktop 起起来(第 6 步之后才需要)
cd D:\luyuan\wuxingqitan\mmorpg
#    ⚠ 若 dev.bat export 已经报过"这些权威 schema 没有对应的源表:GuildDonate, GuildShop",那就是没先跑下面第 1 步的 new-tables

# 1. xlsx 前置(三条都幂等,可加 --dry-run 先看)
python tools\scripts\guild_b5a_xlsx_patch.py new-tables   # 帮会 B5a:不跑这条,导表器会因"有 schema 无 xlsx"整批失败
python tools\scripts\guild_b5a_xlsx_patch.py tip-codes    # 帮会 B5a:guild_error 段尾插 10 行
python tools\scripts\friend_xlsx_patch.py tip-text        # 本批:FriendBlocked 改中性文案

# 2. 导表(§2 第 2 步;通过标准四条 + friend 三个枚举名与 constants.go 逐字一致)
git status --porcelain --ignore-submodules=all data generated go/shared/generated cpp/generated/table java > ..\export-before.txt
.\dev.bat export

# 3. 全量 proto-gen(§2 第 3 步)。先确认客户端清单带着 friend.proto(见 9.1 ③ 下方「客户端仓基线已变」;
#    team / jubaozhai 新基线已自带)。⚠ 不要 git stash pop 客户端仓的 stash@{0}:它基于旧基线
Select-String -Path ..\mmorpg-client\tools\gen_proto.ps1 -Pattern 'proto/friend/friend.proto','proto/team/team.proto','proto/trade/jubaozhai.proto'   # 期望 3 行命中
.\dev.bat proto            # = 校验 protoc 35.1 → proto-gen-build → proto-gen-run;绝不要只跑 proto-gen-run
#    ⚠ 已知阻塞(2026-09-20 晚实跑撞到):proto-gen-build 报 proto2mysql v0.1.0 校验和不匹配。根因是
#    tools/proto_generator/protogen/go.mod 是全仓唯一还 require v0.1.0 的 module,而 D-14 第 7 条禁止 v0.1.0
#    (该 tag 被移动过,代理缓存与 tag 现指向的内容不同)。修法:照 go/schemamigrate/go.mod 改成 v0.1.1 +
#    replace 到 github.com/luyuan-cpp/proto2mysql v0.1.1,再在该目录 go mod tidy。protogen 只用
#    NewDB / RegisterTable / GetCreateTableSQL 三个 API,v0.1.1 都兼容。由「Data table exporter schema errors」会话处理
#    ⚠⚠ 事故(2026-09-20 22:10:40,已发生):**走的就是 .\dev.bat proto,不是有人单跑 proto-gen-run** —— 根因在
#    tools/scripts/dev_tools.ps1 的 Invoke-ProtoGenBuild:go build 是外部命令,失败不触发 $ErrorActionPreference=Stop,
#    脚本照样把上一次的旧 pbgen.exe 复制成 proto-gen.exe 并以 0 退出,dev.bat 于是接着用 09-09 的陈旧生成器跑了 run。
#    也就是说本文"先 build 再 run 就安全"的前提在当时并不成立。已修(「Data table exporter schema errors」会话,
#    未提交):go build 之后检查 $LASTEXITCODE,非 0 即 throw,dev.bat 的 errorlevel 会拦下。
#    结果与本文 §0.3 / 陷阱三的预言完全一致 —— scene_node_service.cpp 纯删 35 行(Agones 块 + 7 个空守护段)、
#    .h 纯删 3 行注释;其它手写文件未被改写(已按 22:09–22:14 的改写时间逐个核过)。这次运行同时写下了
#    message_id.txt(11 个 ClientPlayerFriend*,最大号 238 —— 含 B5a 的 5 个 guild rpc)与 Unity / robot 的 friend 桩。
#    **下一次跑生成器之前必须先恢复这两个文件**(新生成器会回读文件里现有的守护段,块已经不在就不会写回来):
#      git checkout HEAD -- cpp/nodes/scene/handler/grpc/scene_node_service.cpp cpp/nodes/scene/handler/grpc/scene_node_service.h
#    (两者相对 HEAD 只有删除、0 新增,恢复不会丢任何人的在途改动。)然后照常 .\dev.bat proto:消息号按 message_id.txt 保留,
#    robot / Unity 模板自 09-09 未变,陈旧运行建出的 friend 桩不用删。跑完 grep -c AcquireCreatePermitBlocking 应为 1。
#    通过标准见 §2 第 3 步 ①②③;kMaxRpcMethodCount = message_id.txt 最大 id + 1(234 或 239,见该步的更正)
#    兜底:git diff cpp/nodes/scene/handler/grpc/scene_node_service.cpp 确认 Agones 块还在
#    本批多出的预期 diff:go/proto/friend/friend_table.pb.go 首次生成,且 FriendCapacityRecord 带 CreatedMs
#    客户端侧预期:Handlers 92 → 103(只多 11 个 ClientPlayerFriend*),HandlerRegistry.cs 重写;多出别的域的桩就是基线又变了

# 3b. 客户端(-ProtoRoot 必须显式传;protoc 不在 PATH,用 -Protoc 传绝对路径)
cd ..\mmorpg-client
pwsh -File tools\gen_proto.ps1      -ProtoRoot D:\luyuan\wuxingqitan\mmorpg -Protoc D:\luyuan\wuxingqitan\mmorpg\third_party\grpc\install_vs2026_dbg\bin\protoc.exe
pwsh -File tools\gen_messageids.ps1 -ProtoRoot D:\luyuan\wuxingqitan\mmorpg
cd ..\mmorpg

# 4. 限流档位(必须在 proto-gen 之后;脚本按方法名去 proto\message_id.txt 查号,没跑 proto-gen 会直接拒绝)
python tools\scripts\guild_b5a_xlsx_patch.py message-limiter
python tools\scripts\friend_xlsx_patch.py message-limiter
.\dev.bat export           # 再导一次;MessageLimiter 的 rows 期望 = 脚本打印的那个数

# 5. go/friend(§2 第 5 步)
cd go\friend
gofmt -l .                 # 期望空(本批改过的文件已是空;别的文件若有输出,是 HEAD 的存量)
go mod tidy                # 人工看一眼:github.com/redis/go-redis/v9 只许在 indirect 块
go build ./...
go vet ./...
go test ./... -count=1     # 此时真 MySQL 用例会 SKIP,属预期

# 6. 真 MySQL 并发回归(§2 第 6 步;两个环境变量都要设;必须是真 MySQL 不是 TiDB)
go test ./internal/data -count=1 -v 2>&1 | Tee-Object -FilePath ..\..\..\friend-lockorder.log
```

第 6 步的通过标准在 §2 原有五条之上再加三条:① 锁序场景现在是 **8 个**,新增的 `TestAddFriendPendingCountsAreNotLockingReads_CrossedPendingRows` / `TestCapacityRowReclaimRacesWithGuardedWrites` / `TestEnsureCapacityRows_ConcurrentInsertOnReclaimedRowSurvivesDeadlock` 都要 PASS;② `TestCapacityRowReclaimRacesWithGuardedWrites` 若以 **SKIP** 呈现,含义是"这一轮一次缺行都没撞上、对重试没有证明力",不是失败 —— 调大迭代数或回收者数量再跑,别把它算进"零 SKIP";③ 输出里若出现 ensure 或 sweeper 的 `Error 1213`,**把原文和 `SHOW ENGINE INNODB STATUS` 的 `LATEST DETECTED DEADLOCK` 段贴回来**再裁定(9.2 评审第 1 条是推演、未复现)。
之后按 §2 第 7–9 步原样走(7a 多一条索引核对,7b 是五个指标)。`-migrate` 之后顺手对**四**条 SQL 跑 `EXPLAIN`(§3 第 2 条的三条 + `listIdleCapacityRowsBefore` 的候选读:期望 `key` 是 `(friend_count, created_ms)` 那条索引、`type=range` 或 `ref`)。

### 9.5 仍未做 / 新登记的待办

| # | 条目 | 说明 |
|---|---|---|
| 1 | §3 第 2 条 `EXPLAIN` | 运行期核对,现为四条 |
| 2 | ~~friend 对"缺索引"不拒绝启动~~ **✅ 已补(续做)** | 照 guild 的口径:`go/friend/friend.go` 新增 `missingIndexWarnings` / `missingIndexError`(前缀提成常量 `missingIndexWarningPrefix`),`ensureSchema` 的 Up / Plan 两个分支都调 —— Plan 分支必须**单独**判,因为 `Report.Clean()` 只看 Statements 与 Manual、不看 Warnings;`runMigration` 在 `ExitCode == ExitOK` 时把缺索引升级成 `ExitManual`(4),否则 K8s 迁移 Job 会绿着结束、随后 Pod 被拦下。测试:`friend_test.go` 的 `TestMissingIndexWarningIsBlocking`、`TestEnsureSchemaRejectsStartupWhenPlanNotClean` 新增两行、新增 `TestRunMigrationExitCodes`(`runMigration` 此前零覆盖;钉住 D-14 退出码表,含"迁移已失败时缺索引不得把 1 改善成 4")。⚠ 告警文案的前缀与 schemamigrate 的一致性**没有机械守住**(测试里是一条逐字抄来的样本,与 guild 同一取舍),真正的保障是 7a 步的真库核对 |
| 3 | ~~`tools/scripts/k8s_deploy.ps1` 的 friend ConfigMap Sweep 段注释~~ **✅ 已改(续做)** | 改成"两类后台清理共用这一段参数",补上 `friend_sweep_idle_capacity_rows{mode}`。纯注释;PowerShell 解析器 0 错误。⚠ 该文件另有帮会 B5a 的一处未提交改动(data-service ConfigMap 的 `BootstrapTags` 加 `guild_asset_op`),两处互不相干 —— **提交时只暂存自己那一块**(`git diff` 取出那个 hunk 再 `git apply --cached`),别把对方的行卷进来 |
| 4 | `runGuardedWrite` 重试分支没有确定性用例 | ensure 与守卫之间没有可注入的缝(为测试给生产代码开缝不值);"缺行 → 重试成功"由并发场景概率性覆盖,"耗尽 → fail-closed"有确定性用例 |
| 5 | `INSERT … ON DUPLICATE KEY UPDATE` 替代 `INSERT IGNORE` | 见 9.2 评审第 1 条的备选,待真库评估 |
| 5b | **mail M1 设计文档 ✅ 已出(续做)** | `docs/design/mail-system.md`(5 路侦察 → 起草 → 三视角对抗评审 27 条 → 回修)。§11.1 的 18 项已于 2026-09-20 全部由用户拍板:运维可定向发信的 `MailAdmin` + CLI + 令牌、系统邮件读时合并、附件用 mail 自有 `MailCurrency` / `MailItem`、领取方法 M1 定义并回 `MailClaimNotOpen`、上限 100 封拒收、端口 50900 / `:9240`、号段 `mail`、客户端改仓已授权(与 proto-gen 同批)。分四批 M1a 16 / M1b 25 / M1c 18 / M1d 18 个文件。**落码闸门**:M1a 排在 friend 的 proto-gen 与编译通过之后,且 U1 / U2 两条未核实项先核 |
| 6 | Unity 客户端好友功能、mail M1 落码 | 均未开工(§4)。客户端规格已按新基线 `120e2d8` 复核订正(40 条差异全部确认,见规格文件头的复核说明;最要紧的:样板改为 `GuildClient`、tip 一律按生成枚举写、只新增 11 个桩)|
| 7 | ~~leaderboard 设计文档~~ **✅ 已出(续做)** | `docs/design/leaderboard-system.md`(5 路侦察 → 起草 → 三视角对抗评审 28 条全部成立 → 回修)。端口 51100 / `:9250` 已复核未占用。**§9.1 七项已于 2026-09-20 全部由用户拍板**:(a) match 发评分快照(开关默认关 / 不做 outbox / 加 `rating_gen`)、恢复口径接受 7 天重放、同分先达到者在前、K8s 用独立 noeviction Redis、topic `match-rating-snapshot` 3 分区 7 天、客户端改仓已授权、K8s 开关用 `-MatchRatingSnapshotPublish`。**落码闸门仍在**:R1p 起排在 friend 的 proto-gen 与编译通过之后(该文 §9.1 末段)|
