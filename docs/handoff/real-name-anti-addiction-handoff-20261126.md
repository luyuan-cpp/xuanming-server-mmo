# 实名认证与防沉迷：代码已写完，全部验证交 Gemini（2026-11-26）

> **2026-11-26 晚更新**：本单原来 §2 的两项代码（账本截止时刻缺陷、国家系统沙箱 mock）已由实名会话（Claude Fable 5.2）写完，**不要再写一遍**，只做 §3 的验证。同一晚有另一个执行者照本单旧版往 `ledger_test.go` 补过一段用例，已并进现在的文件（其中一条断言按 §2.1 的口径改过）。
> **同日再补（§2.3）**：到点断开时 scene 立即存盘下线；有挂机月卡的未成年人，离线托管不越过截止时刻。这一批改了 `proto/player_locator/player_locator.proto`，**验证前要先重新生成协议**（§3 第 0 步）。
> **同日三补（§2.4）**：战斗直连到点由战斗服断开（「未成年人到点时战斗服断开战斗直连」会话写）。改了 `proto/battle/battle_data.proto`、`battle_node.proto`，同样要先重新生成协议，**客户端仓也要重新生成**（§3 第 0 步）。
> 生成、编译、跑测、冒烟：Carbon Medium。**代码已改完，Gemini 只验证**：跑出要改代码的问题时，保留第一条错误的原文与复现命令停下，交回 Claude（实名会话）改，Gemini 不改（负责人 2026-11-26 定：逻辑代码由 Claude 改，Gemini 只负责生成、编译、测试、冒烟；只有负责人明说让 Gemini 接手写代码时例外）。
> 只在 main 上改，不开分支；负责人没说提交就不提交，不 push。工作区里有很多别的会话的未提交改动：只碰本单列出的文件和段落，不要 `git add -A`。
> 本单是总入口 [third-party-login-and-anti-addiction-handoff-20261126.md](third-party-login-and-anti-addiction-handoff-20261126.md) §3 的细节。清场、生成以及几条线一起跑的顺序，按总入口 §4；实名这条线验什么、怎么验，以本单为准。

## 1. 现状

- 设计稿：[docs/design/real-name-anti-addiction.md](../design/real-name-anti-addiction.md)。§5 是 App 形态的方案，§5.3~§5.5 已按当前代码重写。
- 代码都在 main 上，**从没编译、运行过**。各批的文件清单与命令看 `docs/PROGRESS.md` 的这些条目：2026-11-19「实名认证与未成年人防沉迷落码」、同日 K8s 部署两条和 go/payment 限额闸两条、2026-11-20「收掉以后再做的四处」、2026-11-21 Gemini「NPPA 账本 16 分片」、2026-11-26「实名与防沉迷：修账本截止时刻缺陷、写国家系统沙箱 mock」。
- 2026-11-20 第二批在 PROGRESS 里没有单独条目，内容如下：
  - **上报改成 Redis 持久账本**：`go/login/internal/logic/pkg/nppa/ledger.go`（新增）和 `reporter.go`（重写，去掉了内存队列）；`go/shared/redisslot/login_keys.go`（账本键，删了 `LoginAntiAddictionKey`）。login 侧接线在 `clientplayerlogin/anti_addiction.go`（`beginWatchedSession` / `endWatchedSession` / `renewWatchedSession`）、`loginlogic.go`、`renewsessionlogic.go`、`session_lifecycle.go`；`svc/anti_addiction.go` 加了 `Ledger`、`Clock`、`Now()`。
  - **到点断开改由 gate 本地做**：`cpp/nodes/gate/handler/rpc/minor_play_deadline.{h,cpp}`（新增）；`cpp/libs/services/gate/session/comp/session_info_comp.h`（`minorPlayDeadlineMs`）、`login_admission.h`（`SuccessfulLoginResponse`）、`client_request_lane.cpp`、`rpc_reply_bridge.cpp`、`client_session_handler.cpp`（`CloseMinorAtPlayDeadline`）、`main.cpp`（`minorDeadlineTimer`）、`gate.vcxproj(.filters)`；用例 `cpp/nodes/gate/tests/minor_play_deadline_test.cpp`，登记在 `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`。CMakeLists 由 vcxproj 生成，不要手改。
  - `go/shared/antiaddiction` 加了 `PIBirthPrefix`；新增单测 `go/login/internal/svc/anti_addiction_test.go` 和 `clientplayerlogin/anti_addiction_login_test.go`。
- 生成物：11-26 核对过，`go/proto`、`cpp/generated` 里已有 `ResetAccountRealName` 和 `kLoginMinorPlayTimeOver`。§2.1、§2.2 不碰 proto；§2.3 给 `player_locator.proto` 加了两个字段，§2.4 给 `battle_data.proto`、`battle_node.proto` 各加了一个字段，生成物都还没更新。Tip.xlsx 没动。
- Java 版：11-24 已对齐。§2.1 的缺陷 Java 版同样有，已在 Java 仓 `PARITY.md` 登记待做（2026-11-26 那一节）；§2.3、§2.4 也各登记了一节待做。本仓不写 Java 代码。

## 2. 2026-11-26 写完的各项（验证时对照）

### 2.1 账本截止时刻缺陷（已修）

- **原来的问题**：未成年人的租约封顶在截止时刻。上报器每秒清扫一次，截止那一刻这条到期，被整条删掉。之后续租查不到它，`Renew` 回 -1，login 的兜底踢线不触发；gate 本地没关掉的未成年人会一直在线。
- **改动**：`go/login/internal/logic/pkg/nppa/ledger.go` 的两段 Lua。
  1. `lease_end` 只在截止时刻还没到时封顶（`cap_ms > now_ms and cap_ms < e`）。
  2. `sweepScript`：租约正好在截止时刻到期的条目（`lease_ended == cap`），要上报的照样补一条下线，但登记不删，改写成 `"||截止毫秒"`，租约续到 `now + lease`。
  3. 之后三种结局：连接还在，下一次续租仍回截止时刻，login 兜底踢线；连接断开，End 删掉条目、不再报第二条下线；一直没有续租，过一个租约在普通分支里删掉。
  4. 清扫的返回值只数收尾的会话：只为兜底留着的条目被清掉时不计数（返回值进上报器的 `swept` 指标）。
- **用例**：
  - `ledger_test.go`：`TestLedgerCapsAMinorsLeaseAtThePlayDeadline`（补了截止之后的断言，和一条不续租的会话）、`TestLedgerKeepsTheDeadlineAfterItPassesForTheLoginBackstop`（新增：截止后连续续租 40 分钟，每次都回截止时刻）、`TestLedgerDeadlineOnlySessionEmitsNothing`（加了截止后仍回截止时刻）。
  - `clientplayerlogin/anti_addiction_login_test.go`：`TestRenewSessionKicksAMinorTheGateFailedToClose` 在续租之前先把 Redis 的时钟拨过截止时刻并清扫一次。没修之前这一条是红的。

### 2.2 国家系统沙箱 mock（已写）

- `go/login/internal/logic/pkg/nppa/server.go`（新增）：`ReadRequest` 按规范验签并解开报文体，`ErrBadSignature`。`client.go` 抽出 `newGCM`；`client_test.go` 改用 `ReadRequest`，删了测试私有的解密与验签，加 `TestReadRequestRejectsATamperedRequest`。
- `go/login/cmd/sandbox_mock/nppa.go`（新增）与 `main.go` 的 `newMux()`：`/idcard/authentication/check`、`/idcard/authentication/query`、`/behavior/collection/loginout`，路径与真系统相同；`GET /mock/nppa/behaviors` 列出收下的上下线。
  - 结果按提交的姓名和身份证号定：校验码不对回 2001；姓名带「不匹配」认证失败；带「认证中」先回认证中，同一个 ai 查询时回成功；其余成功。pi 的前 6 位是身份证号里的出生日期。
  - 上下线逐条核对，发生时刻离现在 180 秒以上的拒收（3001 加逐条结果）。
  - secretKey 用规范 V1.9 公开的样例密钥，写成常量 `sandboxNppaSecretKey`。
- `go/login/cmd/sandbox_mock/nppa_test.go`（新增）：真的 `nppa.Client` 打 `newMux()`。
- `deploy/login-stack.linux/login.yaml` 加了 `AntiAddiction` 段，三个地址指向 `sandbox-mock:18090`，缺省 `Enabled: false`（robot 走口令登录、没有实名）。`deploy/docker-compose.login-stack.yml` 给 login 照传 `MMORPG_LOGIN_NPPA_SECRET_KEY`。
- [沙盒手册](../ops/wechat-qq-sandbox-runbook.md) 加了 §J：怎么打开、测试用的身份证号、端到端步骤。
- **与本单旧版的两处出入**：没有做「让整批上报失败」的开关，重试路径改用停掉 mock 的办法走（手册 §J.3）；mock 的可执行文件现在依赖 login 的 `nppa` 包和 `shared/antiaddiction`，镜像要重新构建。

### 2.3 到点立即下线、离线托管不越过截止时刻（已写）

- **原来的问题**：
  - gate 到点关连接后，scene 按普通断线处理：角色在世界里再留 25 秒。
  - 有挂机月卡的未成年人断线后进离线托管，角色一直挂机，不受 21:00 限制；到点之前自己断线的，更是没有人去关它。
- **到点立即下线**（C++ gate）：`client_session_handler.cpp` 的 `CloseMinorAtPlayDeadline` 和 `gate_event_handler.cpp` 的踢线处理（原因是 `kLoginMinorPlayTimeOver` 时），关连接前置 `SessionInfo::exitSceneImmediatelyOnClose`。scene 收到的退出原因不是 `DISCONNECTED`，立即存盘退出。`session_info_comp.h` 的注释同步。
- **离线托管不越过截止时刻**：
  - `proto/player_locator/player_locator.proto`：`PlayerSession.minor_play_deadline_ms = 17`、`ReconnectRequest.minor_play_deadline_ms = 12`。
  - login：`clientplayerlogin/anti_addiction.go` 的 `watchedPlayDeadlineMs` 在进游戏时从会话账本取截止时刻（读不了就拒绝进游戏，`kLoginRedisError`）；`entergamelogic.go` 把它经 SetSession / Reconnect 写进会话记录；`pkg/sessionmanager/session_manager.go` 加字段与 `Reconnect` 的参数。
  - player_locator：`reconnectlogic.go` 写入字段；`leasemonitor.go` 的 `hostedLeaseSeconds` 让月卡续租不超过离截止时刻的剩余时间，到点不再续租，按普通断线收尾。
- **用例**：
  - `go/player_locator/internal/logic/leasemonitor_minor_deadline_test.go`（新增 3 条）。
  - `go/login/internal/logic/pkg/sessionmanager/login_generation_test.go` 的 `TestMinorPlayDeadline_SessionMirrorRoundTrip`。
  - `go/login/internal/logic/clientplayerlogin/enter_wiring_test.go` 的 `TestEnterGame_MinorPlayDeadlineReachesTheSessionRecord`。
- **战斗直连由服务端到点断开**：原来列为没做的一项，同日已做，见 §2.4。

### 2.4 战斗直连到点由战斗服断开（已写）

- **原来的问题**：战斗流量走客户端直连战斗服的第二条连接，不经过 gate，gate 到点只关得掉大厅连接。票据期限等于房间作废期限，同一张票可以反复重连，改过的客户端能把手上这一场打完，观众同理。
- **做法**（来龙去脉与没选的做法见设计稿 §5.4 末条，直连面契约见 [turn-based-battle-server.md](../design/turn-based-battle-server.md) §18.9）：
  - **协议**：
    - `proto/battle/battle_data.proto`：`BattlePlayerSnapshot.minor_play_deadline_ms = 33`。
    - `proto/battle/battle_node.proto`：`AddObserverRequest.minor_play_deadline_ms = 8`；`IssueBattleTicketRequest` 不加字段，补签沿用战斗服按人记下的值。
    - `proto/battle/player_battle.proto`：只改注释。
    - `battle_data.proto` 经客户端可见的 `player_battle.proto` 进了客户端生成物（`BattleData.cs`），客户端仓要重新生成；客户端代码不用改。
  - **match**：
    - `internal/logic/gather.go` 的 `preparePlayerFencedWithJianghuFee`：冻结之前读会话记录。读失败不定罪、不冻结；没有记录按不在线定罪。成功后把 `PlayerSession.minor_play_deadline_ms` 盖到快照上，scene 填的值不作数。
    - `internal/logic/global_spectator.go` 的 `locateGlobalWatch`：把同一个值带进 L0 观战原件。
  - **battle**：
    - 新增 `cpp/nodes/battle/battle_minor_play_deadline.h`：纯函数 `kCloseLeadMs = 5000`、`DirectCloseAtMs`、`Reached`、`TicketExpireAtMs`。
    - `logic/battle_room_manager.{h,cpp}`：房间的 `minorPlayLimits` 与 `minorPlayDeadlineTimer`；`BuildAssignment` 按 `TicketExpireAtMs` 封顶；`AttachDirectConnection` 对到点的人拒绝；新增 `EnforceMinorPlayDeadlines` 与 `ArmMinorPlayDeadlineTimer`；`RetireObserver` 删观众的限制。
    - `battle.vcxproj(.filters)` 登记新头。没动 `client/battle_client_edge.{h,cpp}`。
- **用例**：
  - **C++**（登记在 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`）：
    - 新增 `cpp/nodes/battle/tests/battle_minor_play_deadline_test.cpp`：`BattleMinorPlayDeadline.*` 7 条。
    - `battle_observer_contract_test.cpp` 新增 6 条：`MinorPlayDeadlineCapsEveryTicketAtTheEarlierOfRoomAndPlayDeadline`、`MinorPlayDeadlineClosesOnlyThatPlayersDirectConnectionAndRefusesHisLaterHandshakes`、`MinorPlayDeadlineAlreadyReachedRefusesTheHandshakeFromTheStart`、`MinorPlayDeadlineAppliesToObserversAndTheSeatFollowsTheReconnectGrace`、`RoomWithoutMinorPlayDeadlinesIsUntouchedByEnforcement`、`MinorPlayDeadlineOfARefusedObserverDoesNotStickToHisNextAdmission`。
  - **Go**（`go/match/internal/logic`）：
    - `gather_fingerprint_test.go`：新增 `TestPreparePlayerFencedStampsTheMinorPlayDeadlineFromTheSession`；原有的 `TestPreparePlayerFencedClassifiesFailures` 补种了会话记录，不补会变红。
    - `global_spectate_lobby_native_test.go`：新增 `TestGlobalWatchTiDBCarriesTheViewersMinorPlayDeadlineIntoAddObserver`（要 TiDB）；桩 battle 记下每一份 AddObserver 原件。
    - `testfixture_test.go`：新增夹具 `setSessionMinorPlayDeadline`。
- **首次跑留意**：`BattleObserverContract` 的新用例要拿真实的服务端侧连接，用回环 TcpServer / TcpClient，有几条在建房之后才打开（与原有的 `DirectConnectionStopsSeatClockAndDetachStartsReconnectGrace` 同一写法）。到点由 `EnforceMinorPlayDeadlines` 的显式时刻驱动，不等真实时间。回环连接 5 秒内连不上时 `Open` 会失败，先查本机回环端口。

## 3. 生成、编译、测试、冒烟（Carbon Medium，按顺序）

0. **生成**：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（`player_locator.proto` 加了字段；整套步骤见总入口 §4 第 2 步）。没生成之前 go/login 和 go/player_locator 编不过。§2.4 另给 `battle_data.proto`、`battle_node.proto` 各加了一个字段，没生成之前 go/match 与 battle 也编不过。
   - `battle_data.proto` 进客户端生成物：服务端生成之后，在客户端仓依次跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`（AGENTS §4）。期望 `Assets/Scripts/Proto/Generated/BattleData.cs` 多出 `MinorPlayDeadlineMs`，`PlayerBattle.cs` 最多只有注释变化，编译检查通过。
   - robot 不读这两个字段，不刷新 vendor 也能编过。
1. **静态**：本单碰过的 Go 目录跑 `gofmt -l`，输出为空；`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`、`python tools/scripts/tests/test_cpp_thread_sources.py`、`python tools/scripts/check_log_text_english.py` 都要过。
2. **Go**：
   ```
   cd go/login && go build ./... && go vet ./internal/logic/pkg/nppa/... ./internal/logic/clientplayerlogin/... ./internal/svc/... ./cmd/sandbox_mock/...
   cd go/login && go test -count=1 -v ./internal/logic/pkg/nppa/... ./internal/logic/pkg/realname/... ./internal/svc/... ./internal/logic/clientplayerlogin/... ./internal/logic/loginadmin/... ./internal/logic/loginpregate/... ./cmd/sandbox_mock/...
   cd go/login && go test -count=1 -v ./internal/logic/pkg/sessionmanager/...
   cd go/player_locator && go build ./... && go test -count=1 -v ./internal/logic/...
   cd go/shared && go test -count=1 ./antiaddiction/... ./redisslot/... ./accountdirectory/...
   cd go/data_service && go test -count=1 ./internal/store/... ./internal/server/...
   cd go/payment && go test -count=1 ./...
   cd go/match && go build ./... && go vet ./internal/logic/...
   cd go/match && go test -count=1 -v ./internal/logic/ -run 'TestPreparePlayerFenced|TestGlobalWatchTiDBCarriesTheViewersMinorPlayDeadlineIntoAddObserver'
   cd go/match && go test -count=1 ./...
   ```
   go/match 那条带 TiDB 的用例要设 `MATCH_IT_TIDB=127.0.0.1:<端口>`（一次性 TiDB，与 match 其余原生用例同一组环境变量），没库时它是 SKIP，如实记「跳过」。

   通过标准：全部 PASS，下面这些用例在 `-v` 输出里都是 PASS，不是 SKIP：
   - `go/match` 的 `logic` 包（§2.4）：`TestPreparePlayerFencedClassifiesFailures`、`TestPreparePlayerFencedStampsTheMinorPlayDeadlineFromTheSession`；有 TiDB 时还有 `TestGlobalWatchTiDBCarriesTheViewersMinorPlayDeadlineIntoAddObserver`。`go/match` 全量不能变红；
   - `nppa` 包：`ledger_test.go` 14 条（含 §2.1 的三条）、`TestTick*` 7 条、`TestQuota*` 2 条、`client_test.go` 8 条（含 `TestReadRequestRejectsATamperedRequest`）；
   - `svc` 包：`TestEvaluate*` 3 条、`TestNowUsesTheInjectedClock`；
   - `clientplayerlogin` 包：`TestGateLogin*` 7 条、`TestRenewSessionKicksAMinorTheGateFailedToClose`、`TestRenewSessionNeverKicksAnAdult`、`TestEnterGame_MinorPlayDeadlineReachesTheSessionRecord`（3 个子用例）；
   - `sessionmanager` 包：`TestMinorPlayDeadline_SessionMirrorRoundTrip`；
   - `go/player_locator` 的 `logic` 包：`TestHostedLeaseSecondsStopsAtAMinorsPlayDeadline`（6 个子用例）、`TestLeaseMonitor_MinorPastThePlayDeadlineIsNotHosted`、`TestLeaseMonitor_MinorBeforeThePlayDeadlineIsStillHosted`，并且原有的租约与托管用例不能变红；
   - `loginadmin` 包：`TestResetAccountRealName*` 3 条；
   - `redisslot` 包：`TestNppaLedgerKeysShareSlotPerShardAndSpreadAcrossSlots`；
   - `cmd/sandbox_mock`：`TestNppaCheckAgainstSandboxMock`、`TestNppaPendingResultIsFetchedByQuery`、`TestNppaSandboxRefusesAWrongSecretKey`、`TestNppaLoginoutIsRecordedAndStaleBehaviorsAreRefused`、`TestLinuxLoginStackAntiAddictionPointsAtSandboxMock`。

   data_service 里带 TiDB 的实名用例要真库，没库时如实记「跳过」。

   账本的 Lua 只在纸面上推过，第一次跑要留意两处：miniredis 对脚本里 `redis.call('TIME')` 的支持；`ZRANGEBYSCORE ... WITHSCORES` 返回的分数转成数字后要与截止毫秒**相等**（`lease_ended == cap`）。`TestLedgerCapsAMinorsLeaseAtThePlayDeadline` 如果报「截止后续租拿到零值」，先查这两处。
3. **C++**（Windows，MSBuild 串行 `/m:1`）：命令见 `docs/rules/cpp-runtime-and-build.md`。先编 gate 依赖的库（测试工程链接的是预编好的 lib），再编 `cpp/nodes/gate/gate.vcxproj` 和 `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`。然后运行 `build\cpp\tests\routing_identity_test.exe --gtest_filter=MinorPlayDeadline*`，9 条都要 PASS（`MinorPlayDeadlineTest.*` 7 条、`MinorPlayDeadlineReplyTest.*` 2 条）。Linux 再用 `build_linux.sh` 编一遍 gate。
   - **§2.4 的 battle 部分**：
     - 编 battle 节点 `cpp/nodes/battle/battle.vcxproj`。
     - 跑 `pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test`。这个测试工程自己编 `battle_room_manager.cpp`，不靠预编的 battle 库。
     - 再运行 `build\cpp\tests\rpc_controller_test.exe --gtest_filter=BattleMinorPlayDeadline.*:BattleObserverContract.MinorPlayDeadline*:BattleObserverContract.RoomWithoutMinorPlayDeadlinesIsUntouchedByEnforcement`，13 条都要 PASS（`BattleMinorPlayDeadline.*` 7 条、`BattleObserverContract` 6 条）。
     - 同一个 exe 不带过滤再跑一遍：`BattleObserverContract.*` 原有用例不能变红。
     - Linux 用 `build_linux.sh` 编一遍 battle。
4. **镜像**：`docker compose -f deploy/docker-compose.login-stack.yml build sandbox-mock` 能构建出来（mock 多了两个包的依赖）。
5. **冒烟**：按[沙盒手册](../ops/wechat-qq-sandbox-runbook.md) §J 走一遍，要看到这几种结果：
   1. 没实名的自有渠道账号，gate 的 Login 回 `tip 2042`；实名后能进游戏。
   2. mock 的 `/mock/nppa/behaviors` 里，上线和下线成对，si 相同。
   3. 未成年人在时段外被拒（`tip 2048`）；payment 按限额拒单。时段内的到点断开由单测覆盖，正好在周五到周日 20:00~21:00 跑时顺带看一眼：到点后 scene 日志里这个角色应当立即存盘退出，不出现断线保留。
   3a. 离线托管：给一个未成年人账号开挂机月卡，在可玩时段里进游戏后杀掉客户端。期望角色托管到当天 21:00 为止，之后 player_locator 日志出现 `is a minor past the play deadline`，scene 收到 LeaveScene 后存盘下线。成年人账号同样操作，托管不受影响。
   3b. 战斗直连（§2.4，只在周五到周日 20:00~21:00 跑得了）：未成年人账号在 20:59 前开一场 PVP，对手用成年人账号。期望 20:59:55 前后：
       - battle 日志出现 `metric=battle_minor_play_deadline_reached ... role=participant ... direct_connection_closed=1`；
       - 同一时刻 gate 推踢线原因 `kLoginMinorPlayTimeOver` 并关大厅连接；
       - 之后再用他手里的票握手，被拒（`reason=expired`）；
       - 对手照常打完，结算正常。
       另用一个未成年人账号观战，期望日志里 `role=observer`，席位在重连宽限期满后回收。
   4. `kill -9` login 之后事件不丢。
   5. 真 Redis 上账本的 Lua 能跑：K8s 钉的 7.2 和本机 compose 的 8.x 各走一轮第 1、2 步，记下版本。
6. **记录**：在 `docs/PROGRESS.md` 追加一条（只追加），标题写「Gemini：实名与防沉迷验证」，列命令、通过数和失败的首错原文。设计稿状态行按结果更新。

## 4. 已定案，不要再改

- 姓名和身份证号不存、不打日志，只存 pi。
- `wechat_minigame`、`douyin_minigame` 由平台实名和执行限制，其余渠道一律自管、fail-closed。判定按本次登录的渠道（access_token 带着签发时的 auth_type）。账号上有 pi 时，走任何渠道都按我们的规则限。
- 单日充值上限 = 单次上限，只在 `antiaddiction.SpendLimitAt` 一处定义。
- 国家接口的全服共用额度（GCRA，放在登录域 Redis）：check 80/s，query 240/s，loginout 8/s。
- Disconnect 不为上报重放：End 失败靠清扫补。
- `play_deadline_ms` 是绝对 Unix 毫秒，gate 按自己的墙钟提前 5 秒关；时钟偏差由启动核对的 30 秒门槛兜住。
- 账本 16 分片（11-21）。将来要改分片数，必须同时清掉账本键。
- 截止时刻到了、连接却还在的未成年人：下线按截止时刻报，人由 login 在下一次续租时踢（最晚晚一个续租间隔）。这是 gate 出问题时的兜底，正常由 gate 提前 5 秒断开。
- 未成年人到点是立即存盘下线，不走 25 秒断线保留；离线托管只到当天的截止时刻为止，截止之前断线的照常托管。scene 不需要知道年龄，由 player_locator 到点停止续租。
- 战斗直连：战斗服按人记截止时刻（match 从会话记录取，随建房 / 观战原件冻结）；票据期限取房间期限与截止前 5 秒里较早的；到点关他的直连，之后本场拒绝他握手；战斗本身不变。补签请求不带截止时刻。
- match 成局时读不到会话记录：这一局不开、不定罪；没有会话记录：按不在线定罪。都在冻结之前判。

## 5. 没核实的（不是代码问题，留给负责人）

- 国家系统的报文格式只对过规范样例，还没连过真接口。真接口要等负责人拿到测试用的 appId、bizId、secretKey（设计稿 §9）。
- 微信 App 渠道的绑定方式、抖音小游戏是否由平台执行付费限额：见设计稿 §3、§9。
