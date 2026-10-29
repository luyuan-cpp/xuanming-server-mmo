# 进场链余项 a–e:踢线、离场、落点、释放与断线上报的身份栅栏

- 日期:2026-10-16
- 状态:设计 v0.2;**2026-10-16 已按 §7 三组落码(未编译、未测试),落码记录与出入见 §13**。*2026-10-20 回填(只读核对 PROGRESS 回执,10-22 订正):§8 第二轮(绿跑)的 C++ 部分已有证据。`routing_identity_test` 113/113 含 `KickFence` 10 个;`cross_zone_test` 313/313 含 `ReleaseFence` 8 个、`ReleaseFenceEcs` 22 个。出处是 PROGRESS「2026-10-19 Codex：会话释放夹具修复核验完成，跨区随机全量313项通过」,跑的是合并前源码。Go 侧 scene_manager / player_locator / login 的全量单测另有 10-19 回执。2026-10-22 又有一轮:29 个 C++ 测试 EXE 共 2624 PASS,其中 cross_zone_test 339/339、routing_identity_test 140/140。第一轮(红跑)的证据没有记录;§8 第 5 步的可选联机(robot 同频道重进、travel_smoke / channel_smoke)没有实跑。*已按「正确性」「发布与可实施性」两路对抗复核修订(逐条处置见 §12);
  下一步:Codex 按 §8 两轮验证(命令汇总见 §13 开头的链接)。
- 范围:login → gate 踢线(a)、player_locator → scene_manager 离场清理(b)、scene_manager → scene 的 `ReleasePlayer`(c)、
  scene_manager 落点(d)、login → player_locator 断线 / 登出上报(e)。scene 入场栅栏本身、存盘链、MySQL 行内围栏都不改。
- 上游:[scene-enter-session-fence.md](scene-enter-session-fence.md)(下称 fence)§7 三条待办、§9 残余 2 与「ExitGame / 断线」可选加固;
  接手记录 [landing-fencing-takeover-20261016/README.md](../handoff/landing-fencing-takeover-20261016/README.md) §3.3 的选定做法。
  四份调研在同一目录:`a-login-kick.md`、`bd-leave-and-landing-lua.md`、`c-release-player.md`、`e-exitgame-disconnect.md`
  (只读代理产出,未经运行验证)。
- 方案选择:负责人要求「按最标准的做」,各项待选点一律取调研的推荐项。本文对关键事实做过抽查(2026-10-16 工作区),
  调研里写「未核实」的除 §5.1 注明的一处外仍按未核实处理。
- 行号:取自 2026-10-16 的工作区,多数文件与 HEAD `a105c3efd` 相同;有别的会话在途改动的文件在 §7 标出。行号会漂移,以函数名为准。
  `gate_event_handler.cpp` 已被别的会话的入口生成器重构重生(删了 Register / UnRegister),文中该文件的行号按 HEAD 计,以函数名为准。

## 0. 结论速览

| 项 | 问题 | 选定方案 | 改动面 |
|---|---|---|---|
| a | login 先踢旧会话、后提交 SetSession:提交失败(失锁、超时、版本冲突)的链也会把当前合法会话踢掉;gate 只按 session_id 踢,可能踢到复用同号的别的玩家 | 方案 B:提交成功后才踢;结果未知时短超时复读消歧,复读确认已提交时补挂断线租约;`KickPlayerEvent` 带 player_id 与踢线者代次走 payload;gate 纯函数 `ClassifyKick` 核对玩家与代次;同会话判断改比 (实例号, 会话号) | proto `gate_event`;go/login;gate 纯头 + 守护段;routing_identity_test;protogen 测试 |
| b | MarkOffline 删会话后同步调 LeaveScene,期间无屏障;迟到的 LeaveScene 会删掉新登录的 location 并扣人数 | 方案 A:location 记会话身份(由 d 写),LeaveScene 带被删会话的身份,按代次决定删不删;Reconnect 重放分支补比代次(keep 分支不漏删的前提,§3.2) | proto `storage`、`scene_manager_service`;go/scene_manager;go/player_locator `leasemonitor.go`、`reconnectlogic.go` |
| c | `ReleasePlayer` 只带 player_id,且在落点提交之前派发;交接失败或回滚之后迟到的释放会退掉仍持有归属的实体 | 方案 B:派发挪到本请求提交之后;请求带 `expected_owner_epoch`;scene 纯判定 `release_fence::Decide` | proto `scene_node_service`;go/scene_manager;scene 新纯头 + 库函数 + 守护段委托 + 汇总行 |
| d | 落点 Lua 只做 owner_epoch CAS,陈旧登录或旧会话的换图在同节点不铸造时照样改写 location 与人数 | 方案 A(与 b 共用身份):入口纯判定 `decidePlacementIdentity` + 落点 Lua 加 location 原文期望;scene 自发请求填实体代次;新私有码 25 | 同 b;scene 三处发送点与码表 |
| e | 断线 / 登出上报只比 session_id;同号跨 gate 实例时,旧实例迟到的上报让活玩家 30s 后被踢,或立即删会话 | 方案 A:`SetDisconnecting` / `MarkOffline` 带 gate 实例号,两边都非空且不等即忽略;只改 Go | proto `player_locator`;go/login;go/player_locator 两个 logic |

不做(登记为残余、另立项,见 §10):e 的方案 B / C、fence §9 残余 7、6b「同 scene_id 换节点多算一人」、a 的 T5。

**发布与回滚总顺序(v0.2 起为硬约束)**:

- 发布:proto 生成 → scene → player_locator → scene_manager → gate → login。每一层全部实例滚完再滚下一层。
  - player_locator 先于 scene_manager:b 的 `keep_newer` 不漏删,依赖 locator 已是会持久化 Reconnect 代次的 fence 版本,
    且 Reconnect 重放分支已比代次(§3.2、§3.5)。
  - player_locator 先于 login:a 的跨实例同号踢线(§2.2 ⑤ 第 4 行)依赖 e 在 locator 侧已生效,否则踢线引出的旧实例断线上报
    会给新会话挂租约(§2.5、§6.5)。
  - gate 先于 login:a 的玩家与代次核对由 gate 执行。
- 回滚:按发布的反序。两条硬约束:
  - 要把 player_locator 回滚到 fence 之前的版本,先回滚 scene_manager(§3.5);
  - 要回滚 player_locator 的 e,先回滚 login(§2.5)。
  其余组件单独回滚都只是退回各自的旧行为。

## 1. 统一的会话身份口径

### 1.1 三个维度,各管一件事

| 维度 | 值 | 回答的问题 | 用在 |
|---|---|---|---|
| 会话身份 | `(session_id uint32, login_generation uint64)` | 哪一次登录 | a 踢线、b 离场、d 落点 |
| 连接化身 | `(gate_instance_id string, session_id)` | 哪个 gate 进程上的哪条连接 | e 断线 / 登出上报;a 的「同一连接」判断 |
| 归属代际 | `owner_epoch uint64` | 哪个节点持有数据 | c 释放 |

登录代次的不变量 G1–G5 见 fence §3.1,本文全部沿用,不新增状态,不新增 Redis 键。

### 1.2 会话身份的比较规则(a / b / d 共用)

1. **以代次为主**。G2 保证非 0 代次与会话一一对应。会话号只用于两件事:「同一会话」放行;代次相等而会话不同时识别契约破坏(G2 冲突)。
2. **只用 > 或 ≥ 判新旧**,不设「相等才放行」。唯一的相等判断是 G2 冲突检测(代次相等、会话不同 → 拒绝或不删),
   与 `enter_fence::Decide` 的 `kRejectConflict` 同一口径。
3. **0 表示不比较**:任一侧代次为 0(旧版发送方、旧格式记录)时退回旧行为并计数,不当成「更旧」。
4. **不在会话记录与下游之间做相等校验(G4)**:gate 会话上的代次、location 里的代次、scene 实体的代次都可以落后于会话记录。
5. **同一会话一律放行**:会话号相同且有效时放行,代次取 max,不因代次落后而拒。「有效」= 非 0 且非 `0xFFFFFFFF`,
   与 C++ `player_exit::IsBoundSession` 一致。

### 1.3 与 owner_epoch 的分工(c)

- `owner_epoch` 只在持有节点换了时铸造,同节点顶号不变(fence §1、§3.5),所以修不了 a / b / d。
  登录代次也修不了 c:交接回滚改的是归属代际,与第几次登录无关。
- c 的「相等才释放」比的是 `owner_epoch`,不是登录代次。SM 的期望值就是落点 CAS 用过的观察值,源节点缓存必须恰好是那一代,
  才允许执行退出这种破坏性动作。G4 只约束登录代次,与此不冲突。
- 两者互不进入对方的判定:a / b / d 不看 owner_epoch;c 不看会话与登录代次。登录代次仍不进存盘 CAS(G5)。

### 1.4 gate 实例号(e,以及 a 的同连接判断)

- `""` 表示未知(旧版 login、早期会话记录),一律不比较,不当成「不等」。
- 只有请求与记录两边都非空且不等,才判为旧实例的迟到上报。与 `LeaseExpired` 的 `target_instance_id`、
  `session_reconciler.go` 按实例存活对账是同一口径。
- 实例号不并进 location 身份。后果要如实写:同号跨实例时(gate 以同一 node_id 重启,旧实例 I1 上的 `3:k`(gA)与新实例 I2 上的
  `3:k`(gB > gA)会话号相同),
  - b 的判定表以代次为主,迟到的 LeaveScene{3:k, gA} 遇到 gB 判 `keep_newer`,不受影响;
  - d 的判定表「会话号优先」:A 迟到的 EnterScene{3:k, gA} 被判 `admit_same_session`,d1 在这种情形下仍成立。
    这就是已登记的 R13(§5.8),与 scene 栅栏「同号即同会话」同一口径。
  - 根治要把「同一会话」定义为 (gate 实例, session_id),location 也要记实例号(需与 GO-3 协调字段号),
    与 e 方案 C 同一件事,一并另立项(§10)。

## 2. 余项 a:login 踢线

### 2.1 问题与时序

现状。`applyLoadedPlayerSession`(go/login/internal/logic/clientplayerlogin/entergamelogic.go:729)的顺序是:
`GetSession`(:732)→ `DecideEnterGame`(:738)→ 铸代次(:745)→ `persistEnterGameSession`(:900)。
其中 ReplaceLogin 分支**先**调 `kickReplacedSession`(:909-913,出错只记日志),**后**调 `SetSession`(:925)。

- `kickReplacedSession`(:990-999)只在三种情况跳过:existing 为 nil、`existing.SessionID == 本会话号`、GateID 为空。
- `KickSessionOnGate`(go/login/internal/svc/servicecontext.go:616)不接收 ctx,链 ctx 已取消也照发。
- `buildKickPlayerCommand`(gate_command.go:134-142)只填信封里的 SessionId / PlayerId / EventId,payload 为空。
- gate 生成路由对空 payload 的 Kick 只拼 `session_id`(gate_kafka_command_router.cpp:44-49),因为 `KickPlayerEvent` 只有 1 号字段。
- `KickPlayerEventHandler`(cpp/nodes/gate/handler/event/gate_event_handler.cpp:100-131)按 session_id 找会话,
  推 `kLoginBeKickByAnOtherAccount` 后 shutdown,**不核对玩家**。
  同文件的 `PlayerLeaseExpiredEventHandler` 有内联的玩家核对(:164-172),写 socket 的推送路径有 `ClassifyPush`,踢线两者都没用。

fence 批次的登录代次对这个问题没有缓解。铸代次在踢线之前,locator 的代次校验(setsessionlogic.go:73)在踢线之后,
踢线夹在两者之间,不受任何栅栏约束。

时序:

- **T1 失锁的陈旧链**:
  1. A 拿锁,预加载在途。
  2. A 的锁键丢失(主从切换丢写或长停顿),心跳还没察觉。
  3. B 拿锁跑完整条链:sB、gB 已提交,已进场。
  4. A 的回调读到 existing = sB,判 ReplaceLogin,铸出 gA > gB。
  5. A 的心跳 onLost 取消链 ctx;A 照样发出踢线(不带 ctx),踢掉 sB。
  6. A 的 SetSession 因 ctx 取消而失败,链按失败收尾,给 sA 推 3023。

  结果:没有任何新登录提交,两台设备都掉线;实体多走一次退出、存盘、重载。不损坏数据。
- **T1′**:不需要失锁。踢线之后 SetSession 因 locator 抖动、超时或 CleanupPending 失败,后果同 T1。
- **T2 踢线自伤**:被踢连接的断线链(Kafka → gate → Disconnect → `SetDisconnecting` 版本 +1)先于 A 的 SetSession
  到达 locator,A 在版本校验处判 stale。X 被踢,A 也失败。
- **T3 踢到别人**:gate 只按 session_id 踢。两种情况会踢到别人:
  - 记录里的 sX 已关闭,序号回绕后被玩家 Q 复用(会话号 = node 前缀 + 17 位序号,建连时只跳过仍在用的号);
  - 同一连接 LeaveGame 后换成角色 Q(gate 在 LeaveGame 后不清 `SessionInfo.playerId`)。

  这时 Q 会收到「账号在别处登录」。
- **T4 提交之后仍有的窗口**:A 已提交,但踢线写 Kafka 被拖慢;同时 A 失锁,被替换的连接 sX 上又有一次更新的同连接重登
  X2(gX2 > gA)提交。只核对 player_id 挡不住(同一玩家)。按代次比较**只在 X2 的 Bind 先于 A 的踢线到达 gate 时**挡得住:
  两者来自不同的链,在分区里的先后由各自拿到 broker ack 的时间决定。若 A 的踢线先到,gate 上 sX 的代次仍是旧的 gX < gA,
  判 `kKick`,最新的 X2 被踢;X2 自己也已踢掉 sA,两台设备都掉线。gate 在 Bind 之前无从得知 gX2,gate 侧修不了。
  这一顺序登记为残余(§2.8),只伤活性。
- **T5(残余)**:换角发生在新 Bind 之前,gate 上的 playerId 仍是旧角色,player_id 核对会放行。见 §10。
- **跨实例同号**(e 调研的附带发现):`kickReplacedSession` 遇到 `existing.SessionID == 本会话号` 直接跳过,不比实例号。
  gate 以同一 node_id 重启后,同号会话落在新实例上,旧实例上的旧连接永远不会被踢,e 的迟到窗口因此没有上界。

### 2.2 选定设计(方案 B)

**① 提交成功后才踢。** 删掉 `persistEnterGameSession` 里先踢的那段(:909-913)。ReplaceLogin 在 `SetSession` 成功返回之后、
`bindSessionFence`(:763)之前踢线。不能更晚:若等 EnterScene 成功再踢,EnterScene 被可重试码(18 / 20)拒绝时,
记录已指向 sA,旧设备 X 却还在 scene 上照常游戏;之后 sA 断线进租约,到期清理会把 X 正在玩的实体退掉。
提交之后踢线写 Kafka 失败,只记 ERROR 并计数,不让链失败(提交已经发生)。旧设备会因 scene 栅栏改绑而变哑,与现状一致。

**② 结果未知时复读消歧。** `SetSession` 返回任何错误时(应答丢失、ctx 取消、locator 拒绝),ReplaceLogin 用独立短超时 ctx 复读一次:

- ctx:`context.WithTimeout(context.WithoutCancel(ctx), setSessionProbeTimeout)`,`setSessionProbeTimeout = 2 * time.Second`。
  做法同 `compensateFenceBind`(:886)。
- 记录非 nil、`SessionID == 本会话号`、`LoginGeneration == 本链铸出值`(且非 0,G2 保证唯一)时,视为**已提交**:
  1. 照常踢旧会话;
  2. 在同一个独立 ctx 上调一次 `markPlayerSessionDisconnecting(P, 本会话号, 本链实例号)` 补挂断线租约,
     等价于 `compensateFenceBind(placeLease=true)`(v0.2 新增,原登记在 §10 第 6 条);
  3. 链**仍按失败收尾**(返回原错误)。

  补挂租约的理由:此刻 `bindSessionFence` 还没执行,当前 Unity 客户端收到 3023 就拆连接(entergamelogic.go 中
  `notifyEnterGameFailed` 的注释),断线时栅栏没有绑定玩家,`sessionPlayersToSettle` 收不到 P;被踢的 X 的
  `SetDisconnecting(P, sX)` 又因会话号不符被忽略。不补挂,记录会停在 ONLINE、没有租约,X 留下的 location 也没人删
  (幽灵人数;A′ 标记过期后可能一直拿 18)。连接若还活着、客户端同连接重试,走短线重连接回,与绑定失败补偿同一语义。
- 复读失败或对不上:视为未提交,不踢,不挂租约,计 `skip_uncommitted`。
- 不在 FirstLogin / ShortReconnect 上做:这两条不踢线。它们「结果未知且已提交」时停在 ONLINE 的既有缺口仍在,见 §2.8。

**③ 踢线者代次栅栏。** `KickPlayerEvent` 加 `player_id = 2`、`login_generation = 3`,踢线改走 payload,信封照填。
代次是**本链(已提交)铸出的值**。

**④ gate 纯函数 `ClassifyKick`**,放在 `cpp/libs/services/gate/session/system/session_identity_fence.h`,先过 `ClassifyPush`:

| `ClassifyPush(会话绑定玩家, 事件 player_id)` | 代次条件 | 结论 | 动作 |
|---|---|---|---|
| `kDeliverUnfenced`(事件 player_id = 0,旧版 login) | 不比较 | `kKickUnfenced` | 按 session_id 踢;首次记一行 INFO |
| `kDrop`(玩家不符,或会话尚未绑定玩家) | — | `kSkipOtherPlayer` | 不踢,WARN |
| `kDeliver` | 事件代次 ≠ 0 且 会话代次 ≥ 事件代次 | `kSkipNewerLogin` | 不踢,WARN |
| `kDeliver` | 其余(事件代次为 0,或会话代次更小,含会话代次 0) | `kKick` | 推 `kLoginBeKickByAnOtherAccount` 后 shutdown |

- **为什么是 ≥**:会话代次只来自已提交登录的 Bind 与路由(都经 `ApplyLoginBinding` / `ApplyRouteIdentity` 取 max)。
  等号表示这条连接承载的正是踢线者本次登录,只会出现在同一会话上,而同一会话 login 本来就不发踢线,判「不踢」是安全方向。
- **为什么未绑定会话 fail-closed**:与 `ClassifyPush` 的 R13 口径一致。`buildGateCommandMessage` 按 gate 节点号显式选分区,
  同一 gate 的命令在同一分区内全序;被替换的链在 Bind 之后才释放玩家锁,所以正常时序下被踢会话一定已被 Bind 过,
  未绑定只会出现在会话号复用或失败路径上。代价:失锁导致 Bind 被拖到踢线之后时,旧连接暂时不被踢,之后由 scene 栅栏拒掉
  陈旧入场、推 3023 收口。
- 判定写在 `session_identity_fence.h`,不放进 `scene_route_helper.h`。这样 `loginGeneration` 的写入点仍只有那两个 helper(不变量 4)。

**⑤ 同一连接的判断改比 (GateInstanceID, SessionID)**:

按顺序判:

| # | existing 与本链 | 结论 | 计数 |
|---|---|---|---|
| 1 | `existing.GateID` 或 `existing.GateInstanceID` 为空 | 不踢:`buildGateCommandMessage` 守卫一本来就拒空实例号 | `skip_no_target` |
| 2 | 本链实例号为空 | 不踢(无论会话号) | `skip_no_target` |
| 3 | 会话号相同,实例号相同 | 同一连接,不踢 | `skip_same_session` |
| 4 | 会话号相同,两边实例号都非空且不同 | 跨实例同号,踢;`target_instance_id` 限定旧实例,新实例按防僵尸过滤丢弃。**前提:e 已在 player_locator 生效**(§2.5) | 按发送结果 |
| 5 | 会话号不同 | 踢 | 按发送结果 |

第 2 行的理由(v0.2 改):本链实例号为空时,`SendBindSessionToGate` 必在 `buildGateCommandMessage` 守卫一失败,3023 也推不出去,
这条链注定失败。v0.1 只在「会话号相同」时按同一连接处理(防自踢:本链的 Bind 在踢线之后才发,同连接重登时本连接的 gate 会话
仍绑着上一次的更小代次,`ClassifyKick` 会判 `kKick`);会话号不同时仍会先踢掉旧设备,再在 Bind 失败。现在一律不踢。
这条链仍会先提交一条实例号为空、之后无法寻址的会话记录,这是既有行为;改为提交前拒绝另立项(§10),原因见 §12 C-F8。

第 4 行的已知代价:旧实例 I1 收到踢线后关闭旧连接,会立刻向 scene 发 `ExitGame(3:k, P)`、向 login 发 `Disconnect(3:k, I1)`。
- login 侧:e 生效后被 player_locator 按实例号忽略;e 未生效时会给新会话挂租约、30s 后踢掉活玩家(所以有上面的前提)。
- scene 侧:同号、同玩家的 ExitGame 能通过三道守卫,晚于新会话的 `PlayerEnterGameNode` 到达时会让新实体退出。这是 §6.8 方案 A
  不覆盖的面。踢线并没有新造这个面:I1 上的旧连接无论因何关闭(客户端自己断开、I1 冲突收尾、进程退出)都会发出同一条 ExitGame;
  踢线只是把它从「旧连接关闭的那一刻」提前到「新登录提交之后」。不踢的话,旧设备会继续以同号、同玩家的身份向 scene 发消息
  (§6.8「旧客户端消息」面)。两者取舍待负责人确认(§12 待拍板 1)。

**⑥ 计数** `login_kick_total{result}`,`result ∈ sent | send_failed | skip_same_session | skip_no_target | skip_uncommitted`。
只有一个低基数 label,不放 player_id / session_id。

### 2.3 契约改动

proto/contracts/kafka/gate_event.proto(`KickPlayerEvent` 的 2、3 号字段历史上从未使用过):

```proto
message KickPlayerEvent {
  option (OptionKafkaIngress) = true;
  uint32 session_id = 1;
  // 被踢会话所属玩家。gate 只踢当前绑定该玩家的会话;0 = 旧版发送方,按 session_id 踢。
  // 信封路径由生成路由按同名同型从 GateCommand.player_id 映射(必须 uint64)。
  uint64 player_id = 2;
  // 发起踢线、已提交的那条登录链的代次。会话已见过不小于它的代次时不踢;0 = 不比较。只经 payload 下发。
  uint64 login_generation = 3;
}
```

- `GateCommand` 不改。Kick 报文改为非空 payload;信封里的 SessionId / PlayerId / EventId 照填,topic 与分区算法不变。
- 同文件 `RoutePlayerEvent.login_generation`(7)的注释「scene 节点自发的路由为 0」改为「scene 自发的路由带实体已放行的代次
  (≤ gate 会话值),旧版 scene 为 0」(对应 d 的 §5.2 ④)。
- Go 内部签名:
  - `(*ServiceContext).KickSessionOnGate(gateID, gateInstanceID string, sessionID uint32, playerID, loginGeneration uint64) error`
  - `buildKickPlayerCommand(gateID, gateInstanceID string, sessionID uint32, playerID, loginGeneration uint64)`
- login 测试接缝:接口 `replacedSessionKicker { KickSessionOnGate(...) error }`,生产实现是 `*svc.ServiceContext`,
  做法同 `enterFailureTipPusher`(entergamelogic.go:467)。
- C++(纯函数、零依赖):`enum class KickVerdict : uint8_t { kKick, kKickUnfenced, kSkipOtherPlayer, kSkipNewerLogin };`、
  `ClassifyKick(uint64_t sessionBoundPlayerId, uint64_t sessionLoginGeneration, uint64_t targetPlayerId, uint64_t kickerLoginGeneration)`,
  再加一个 switch 写的 `KickVerdictName`(只用于日志;不需要名表与 kCount)。
- 不改:Lua、Redis 键、player_locator 与 scene_manager 的 gRPC 契约、scene。

### 2.4 逐文件改动点(全部属 W1)

1. `proto/contracts/kafka/gate_event.proto`:见 §2.3。
2. `go/login/internal/svc/gate_command.go`:
   - `buildKickPlayerCommand` 照 `buildBindSessionCommand` 的做法,把 `KickPlayerEvent{SessionId, PlayerId, LoginGeneration}`
     序列化进 Payload,信封照填。
   - :101-111 的注释补上 Kick 改走 payload 的理由:代次只能经 payload 到 gate。
3. `go/login/internal/svc/servicecontext.go`:`KickSessionOnGate` 加参数并透传。只改这个函数,不碰 `initLoginQueue` 的在途 hunk。
4. `go/login/internal/logic/clientplayerlogin/entergamelogic.go`:
   - `EnterGameLogic` 加字段 `kicker replacedSessionKicker`,为 nil 时用 `l.svcCtx`(测试可替换)。
   - `persistEnterGameSession` 的 FirstLogin / ReplaceLogin 分支:
     1. 删掉先踢的那段。
     2. `SetSession` 成功 → ReplaceLogin 时踢线。
     3. `SetSession` 失败 → ReplaceLogin 时调 `setSessionCommittedDespiteError`;确认已提交则踢线,并在同一个独立 ctx 上调
        `markPlayerSessionDisconnecting(P, 本会话号, state.gateInstanceID)` 补挂断线租约(§2.2 ②);否则计 `skip_uncommitted`。
     4. 失败分支照旧返回原错误。
   - `kickReplacedSession` 改成自由函数 `kickReplacedSession(kicker, existing, state, loginGeneration) string`,
     按 §2.2 ⑤ 判跳过,返回 result 供计数。
   - 新日志(英文),例如:
     - `[login-kick] SetSession outcome unknown, probe confirms commit: player=%d session=%d generation=%d; kicking replaced session`
     - `[login-kick] kick after commit failed: ...`
5. `go/login/internal/logic/clientplayerlogin/metrics.go`:新增 `kickTotal`,`Namespace: "login"`、`Name: "kick_total"`、
   `Labels: []string{"result"}`(导出名 `login_kick_total`)。本文件现有指标的命名空间是 `entergame`,这里显式写 `login`。
6. `cpp/libs/services/gate/session/system/session_identity_fence.h`:新增 `KickVerdict`、`ClassifyKick`、`KickVerdictName`。
   头注释写明三点:未绑定会话 fail-closed 的理由;代次只用 ≥ 比较、只读;按值传参、不写会话。
7. `cpp/nodes/gate/handler/event/gate_event_handler.cpp`(只改守护段):
   - 这个文件的骨架正被别的会话的 Kafka 入口生成器重构改写(已删 Register / UnRegister 与空的 `PlayerDisconnectedEventHandler`,
     见 §7)。W1 只动 `KickPlayerEventHandler` 的守护段与文件头守护段,不碰骨架;重生后守护段原样保留。
   - 文件头守护段加 include。
   - `KickPlayerEventHandler` 在 find 成功之后、lock 连接之前调
     `ClassifyKick(it->second.playerId, it->second.loginGeneration, event.player_id(), event.login_generation())`。
   - 两种 skip 打 `LOG_WARN` 后返回,日志带会话号、双方玩家、双方代次、结论名。
   - `kKickUnfenced` 首次打一行 INFO(本文件内的 thread_local / static 一次性标记,做法同 `gate_service_handler.cpp` 的 `LogFenceUnfencedOnce`)。
   - 末尾的 kicked 日志补上 `event.player_id()` 与代次。
   - **只读 `loginGeneration`,不写。**
8. `cpp/libs/services/gate/session/comp/session_info_comp.h`:`loginGeneration` 的注释(:62-69)改两处。一是读取方多了一个:
   「盖章;另由踢线身份栅栏只读比较。写入仍只经 ApplyLoginBinding / ApplyRouteIdentity」。二是「scene 自发的换图路由带的是 0」
   改为「带实体代次(≤ 会话值),旧版 scene 为 0」。
9. 生成物(Codex 重生,不手改):`gate_event.pb.*`、`go/proto/contracts/kafka/gate_event.pb.go`、`gate_kafka_command_router.cpp`。
   工作区里的 `gate_kafka_command_router.cpp` 已按入口生成器重构的新模板重生:payload 分支逐事件 `ParseFromString` 后直接调
   `GateEventHandler::KickPlayerEventHandler(event)`,信封分支只拼 `set_session_id`。本批重生后相对**重生前的工作区**只应多出一处:
   信封分支 Kick 多一行 `event.set_player_id(command.player_id());`(按同名同型自动映射)。payload 分支本来就整份解出
   `KickPlayerEvent`,不变。相对 HEAD 的差异会混进那次重构,不能拿来判断,比对口径见 §8。

### 2.5 新旧并存与回滚

- **默认值**:
  - 事件 player_id 为 0 → `kKickUnfenced`,按旧行为踢。
  - 事件代次为 0 → 不比较。
  - 会话代次为 0(会话没见过代次)→ `0 ≥ gen` 不成立,照踢。
  - 会话未绑定而事件带 player_id → 不踢(R13)。
- **升级顺序**(v0.2 改为硬约束,与 §0 总顺序一致):player_locator(含 e)→ gate → login。
  - **player_locator 必须先于 login**。v0.1 写的「没有硬依赖」不成立:⑤ 第 4 行(跨实例同号踢线)是新行为,旧代码遇同号直接跳过。
    新 login 配上还只比 session_id 的旧 locator 时,时序是:A 在 I2 拿到 3:k 并提交,记录为 (3:k, I2) → login 踢 I1:3:k →
    I1 关闭旧连接,发 `Disconnect(3:k, I1)` → login 按栅栏绑定的玩家 P 调 `SetDisconnecting(P, 3:k)` → 旧 locator 只比会话号,
    把 A 的记录置为 DISCONNECTING 并挂租约 → 30s 后 LeaseExpired 投到 I2,活着的 A 被 forceClose,location 也被删。
    条件(17 位序号相撞)罕见,但条件一旦成立就必然发生。e 生效后这条 Disconnect 按实例号被忽略(§6.2)。
  - gate 先于 login:玩家与代次核对由 gate 执行。
  - 新 gate + 旧 login:旧 login 一直在信封里填 PlayerId,重生成后的路由能拿到 player_id,玩家核对立即生效;没有代次,不比较;
    先踢后提交仍在,login 升级后才消除。
  - 旧 gate + 新 login:payload 非空,旧 gate 按白名单解 `KickPlayerEvent`(旧版生成路由走 `DispatchGateCommandEvent`,
    新版直接调 handler,两者都能解),未知字段 2、3 被忽略,按 session_id 踢。行为与现状一致,不丢踢线;顺序修复由 login 单方生效。
  - Kafka 里在途的老格式踢线(空 payload)由新 gate 的信封路径处理,能拿到 player_id。
- **回滚**:按发布的反序。要回滚 player_locator 的 e,先回滚 login(否则又回到上面的时序);gate 单独回滚只是退回按 session_id 踢。
  proto 只加字段,回滚不删字段;字段一经上线不再复用(AGENTS §4.3)。
- 第 4 行还会把 scene 侧同号 ExitGame 的反例从「旧连接自然关闭时」提前到「新登录提交后」,见 §2.2 ⑤ 的已知代价与 §6.8。
- **行为变化**:改动前,SetSession 失败或结果未知时旧会话也已被踢;改动后,只有确认提交才踢。
  复读也没确认的「结果未知」(locator 已写入,但应答丢失且复读失败)不再踢:旧设备继续在线,记录却指向 sA,
  推送会投到 sA,直到下一次登录收敛。

### 2.6 不变量对照

| 不变量 | 结论 |
|---|---|
| 1 入口栅栏第一步且零副作用 | scene 一行不改,`HandleEnterGameNode` 第 0 步不动。同一精神落到 login 与 gate:login 把对外副作用(踢线)挪到提交点之后;`ClassifyKick` 是纯函数,在 shutdown 之前判完,跳过时零副作用 |
| 2 同一会话一律放行 | 同一连接(同号且同实例或实例不明)永不发踢线;`≥` 的等号只可能出现在同一会话上,判为不踢是安全方向;踢线不是入场,不拒绝任何入场请求 |
| 3 SessionMap 只擦本玩家的映射 | scene 擦除路径不碰。gate 侧的对应物是「只踢当前绑定本玩家的会话」:复用 `ClassifyPush`,未绑定或绑别人一律不踢 |
| 4 gate `loginGeneration` 只经两个 helper 写入,只抬高、不丢路由 | `ClassifyKick` 按值读、不写;判定放在 `session_identity_fence.h` 而非 `scene_route_helper.h`;踢线不是路由,跳过踢线不丢任何路由,`ForwardPlayerToScene` 盖章不变;`session_info_comp.h` 只改注释 |

### 2.7 测试(先复现,再修复)

现有测试刻意把记录的 GateID 留空,以避开 nil Kafka 生产者(`entergame_login_generation_test.go`)。所以要先引入 kicker 接缝
(纯重构、不改行为),再写复现用例。

go/login `internal/logic/clientplayerlogin/entergame_kick_test.go`(新)。**在本文件里新建独立 harness**,不复用、也不改
`newEnterWiringHarness`(player_identity_backfill_test.go):后者的 fake locator 的 SetSession / Reconnect 恒报错、GetSession 只回固定的
existing、ctx 里的 SessionDetails 只有 SessionId,满足不了下面的用例。新 harness 的要求:

- fake locator 的 SetSession 可配置成功或指定错误;GetSession 按调用序返回(首读、复读各自可配);记录 SetDisconnecting 请求与收到的 ctx。
- SessionDetails 设 `gate_instance_id` 非空、`gate_node_id = 0`。这样本链的 Bind 在 `buildGateCommandMessage` 守卫三报错返回,
  `notifyEnterGameFailed` 因 gateID 为 "0" 跳过,都碰不到 `KafkaClient`(harness 里为 nil,碰到即 panic,整个测试二进制崩掉)。
  踢线目标取自 existing 记录(GateID "7"、实例 "uuid-7"),走 kicker 接缝,同样碰不到 Kafka。
- 「本链实例号为空」的用例另设 `gate_instance_id` 为空。
- 辅助标识符统一加 `kick` 前缀,避免与同包已有的 `enterWiring*` 撞名。

| 用例 | 场景与断言 | 旧代码上 |
|---|---|---|
| `TestEnterGame_ReplaceLoginSetSessionFailureDoesNotKick` | existing ONLINE、GateID="7"、实例 "uuid-7"、会话号 ≠ 本链;SetSession 失败、复读不是本链 → 踢 0 次,计 `skip_uncommitted` | 红(踢 1 次) |
| `TestEnterGame_ReplaceLoginKicksOnlyAfterSetSessionCommits` | 事件序列为 GetSession → SetSession → Kick;踢线参数为 ("7", "uuid-7", 1, 玩家, 铸出代次) | 红(顺序相反) |
| `TestEnterGame_SameSessionSameInstanceDoesNotKick` | 同号同实例 → 0 次 | 绿(回归) |
| `TestEnterGame_SameSessionDifferentInstanceKicks` | 同号、实例 "uuid-old" ≠ 本链 → 踢 1 次,目标实例为 "uuid-old" | 红 |
| `TestEnterGame_ChainWithoutGateInstanceDoesNotKick` | 本链实例号为空,会话号与 existing 相同、不同各一例 → 都是 0 次,计 `skip_no_target` | 红(会话号不同的那例踢 1 次) |
| `TestEnterGame_CanceledChainDoesNotKick` | SetSession 返回 `context.Canceled`(模拟 onLost),复读未见本链 → 0 次 | 红 |
| `TestEnterGame_UnknownOutcomeProbeConfirmsCommitKicks` | SetSession 报错,复读得到本链的会话号与代次 → 踢 1 次,链仍返回错误 | — |
| `TestEnterGame_ProbeConfirmedCommitPlacesDisconnectLease` | 同上场景 → `SetDisconnecting` 恰好 1 次,参数为 (玩家, 本会话号, 本链实例号),收到的 ctx 未取消、带截止时间 | — |
| `TestEnterGame_UnconfirmedOutcomePlacesNoLease` | 复读未见本链 → `SetDisconnecting` 0 次 | — |
| `TestEnterGame_ProbeUsesIndependentDeadline` | 链 ctx 已取消,复读仍能执行(用 fake 断言收到的 ctx 未取消、带截止时间) | — |

go/login `internal/svc/gate_command_test.go`:

- 表驱动 kick 行(:170-190、:120、:301、:451)改为新签名。
- 新增 `TestKickPlayerCommandPayloadCarriesEvent`:payload 解出 `KickPlayerEvent` 三个字段都对;信封 SessionId / PlayerId / EventId
  不变;分区等于按 gate 节点号算出的值。

protogen `internal/generator/cpp/gate_kafka_router_automap_test.go`(**新文件**)。不往 `gate_kafka_command_router_test.go` 追加:
那个文件正被别的会话的入口生成器重构改写(§7)。

- 新增 `TestGateKafkaRouterAutoMapsSameNameSameTypeFields`:GateCommand 含 `player_id(uint64)`、`session_id(uint32)`,
  事件含同名同型字段,断言赋值同时有 `set_session_id` 与 `set_player_id`。
- 再构造事件 `player_id` 为 uint32 的版本,断言被跳过。这条固化「必须同型」的前提。
- 本文件自带一个带字段的描述符安装函数(取名 `installAutomapGateRouterDescriptors`,不改也不依赖同包的 `installGateRouterDescriptors`,
  后者的 GateCommand 没有字段,且所在文件在途)。
- 用例开头保存并设置 `logger.Global = zap.NewNop()`,`t.Cleanup` 里恢复(照 `grpc_handler_routing_test.go` 的写法)。
  类型不匹配的分支会调 `logger.Global.Warn`,而 `logger.Global` 默认为 nil,不设会 panic。

C++ `cpp/tests/routing_identity_test/routing_identity_test.cpp`(已 include `session_identity_fence.h`;只在文件末尾追加):

- `KickFence.MatchingPlayerKicks`、`OtherPlayerSkips`、`UnboundSessionSkips`、`LegacyKickWithoutPlayerIsUnfenced`
- `SessionHostingNewerOrEqualLoginSkips`、`OlderSessionGenerationKicks`、`ZeroKickerGenerationSkipsComparison`、`LegacySessionGenerationZeroKicks`
- `ClassifyDoesNotTouchSessionGeneration`:构造 `SessionInfo`,调用前后 `loginGeneration` 不变。

红运行证据(v0.2 改):按 §8 分两轮交 Codex。第一轮只含 proto、生成、纯重构接缝(kicker 接缝、新增参数只透传不使用)
与全部测试,期望恰好是表中标「红」的用例失败;第二轮落修复,要求全绿。这样能按 AGENTS §11.4 拿到旧行为上的红跑记录。

### 2.8 残余

- **T5**:换角发生在新 Bind 之前,gate 不清 playerId,player_id 核对放行。另立项(gate 在 LeaveGame 时清绑定会影响
  `disconnectlogic.go` 的 `sessionPlayersToSettle`,后者依赖「不清」)。
- **结果未知且已提交、链失败之后连接断开**:会话停在 ONLINE。这是既有缺口:Disconnect 时 fence 未绑定、SessionDetails 的玩家为 0,
  `sessionPlayersToSettle` 收不到它。v0.2 起 ReplaceLogin 在复读确认已提交时补挂断线租约(§2.2 ②),这一支本批关闭。仍开着的两支:
  - FirstLogin / ShortReconnect 不复读,「结果未知但已提交」时照旧停在 ONLINE(没有被踢的旧设备,影响比 ReplaceLogin 小);
  - ReplaceLogin 复读本身失败(locator 不可达)时无法确认,不挂租约。
  两支都由 `session_reconciler` 兜底:仅当 gate 实例从 etcd 消失时生效,活着的 gate 上的这种记录它不处理。另立项(§10)。
- **T4 的反向顺序**:失锁时 A 的踢线先于同连接重登 X2 的 Bind 到达 gate,会踢掉更新的 X2;X2 也已踢掉 sA,两台设备都掉线(§2.1 T4)。
  gate 在 Bind 之前无从得知 gX2,gate 侧修不了;只伤活性,客户端重登收敛。登记备查(§10)。
- **邻接**:`PushToPlayerEventHandler`(gate_event_handler.cpp:278)同样只按 session_id 投递,login 的 3023 失败通知
  可能投到复用了会话号的陌生连接。修法是给 `PushToPlayerEvent` 加 player_id 并写进 payload,另立项。

## 3. 余项 b:迟到的 LeaveScene 删掉新登录的 location

### 3.1 问题与时序

链路:

1. LeaveGame 进入 login(leavegamelogic.go:51-52,注释写明**不拿玩家锁**)→ `deletePlayerSession`(session_lifecycle.go:46)
   → `sessionmanager.DeleteSession`(session_manager.go:157)。
2. player_locator `MarkOffline`(markofflinelogic.go:39):比对 session_id + version(:65),整字节 CAS 删会话(:79),
   然后同步调 `notifySceneManagerLeaveOrEnqueue`(:98 / :105)。
3. `notifySceneManagerLeave`(leasemonitor.go:286):会话的 SceneId 恒为 0,所以由 `resolveSceneManagerSceneID`(:319-332)
   **现读** `player:{id}:location` 取 SceneId。发出的 `LeaveSceneRequest` 只带 PlayerId 与 SceneId(:304-307)。
4. scene_manager `LeaveScene`(leavescenelogic.go:116):GET 原文 → 只比 SceneId → `luaDeletePlayerLocationIfUnchanged`(:33)。
   原文仍相等才 DEL,同一段 Lua 里扣场景与节点人数。函数注释(:110-115)自认挡不住两类:同落点重连不写 location;
   同一场景的新落点在 GET 之前写入。

屏障现状:同步路径不登记 claim,新登录 SetSession 的 missing 模式不会被挡。租约到期路径在 ack 之前一直持有 processing claim,
能挡新登录,但挡不住 b4。

设 A 为旧会话 (sA, gA),在场景 S1、节点 X;B 为新登录 (sB, gB > gA)。

- **b1 同落点**:B 命中 samePlacement(enterscenelogic.go:658-674),不写 location;随后 A 的 LeaveScene 到达,
  原文没变、SceneId 相等,删除 location 并扣 S1 / X 的人数。
- **b2 新落点被现读删掉**:B 已把 location 写到 S2;locator 现读拿到 S2,LeaveScene(S2) 的原文正是 B 的,
  B 的新落点被删、S2 人数 -1。从删会话起到 locator 读 location 之前,B 的任何一次落点都会被删。
- **b3**:B 落到同一个 scene_id、换了节点,只要落在 locator 读之后、SM GET 之前,同样被删。
- **b4**:LeaveScene 的 RPC 在客户端超时而服务端仍在执行(go-zero 的 Redis 调用不受 ctx 约束),迟到执行发生在新登录之后。

后果:

- B 在 X 上,但 location 没了。
- 人数少 1:副本可能被 destroy-on-empty 带人销毁(instance_lifecycle.go:106-117);世界频道被误判为空,影响扩缩容与选线。
- B 下一次跨节点换图时 currentLoc 为 nil:不过换手门,也不发 ReleasePlayer,直接铸造。目标节点可能读到 X 尚未落盘的状态;
  X 之后的存盘被 epoch CAS 拒绝,实体按废黜销毁,进度回档。
- trade / guild / inbox / match 的 Locator 判 B 不在线。

根因(与 d 相同):location 只记「在哪」与「哪一代归属」,不记「哪次登录放行了它」。

### 3.2 选定设计(方案 A)

- location 记会话身份,由 d 的落点写入(§5.2 ②),字段见 §3.3。**b 与 d 必须同批落地**:只做 b 时 location 没有身份,b 全部退化为 legacy。
- player_locator 在 `notifySceneManagerLeave` 一处给 `LeaveSceneRequest` 填**被删那条会话记录的原值**(`session.SessionId`、
  `session.LoginGeneration`)。MarkOffline 同步路径、租约到期路径、enqueue 重试都经过这个函数;enqueue 用 `proto.Clone` 保留了代次,
  不需要再改别处。
- scene_manager 在 SceneId 比对之后调纯函数 `decideLeaveIdentity(loc, in)`。删除 Lua 不变,仍是原文 CAS + 原子扣人数,
  原文 CAS 保证判定与提交之间没有窗口。

L = location 里记的身份,R = 请求带的身份:

| 情形 | 结论 | 动作 |
|---|---|---|
| gen_R == 0 或 gen_L == 0 | `legacy` | 旧行为:原文 CAS 删除,计数 |
| gen_L > gen_R | `keep_newer` | 不删、不扣人数,回成功,Info 日志(带两边身份) |
| gen_L == gen_R、会话都**有效**且不同 | `keep_conflict` | 不删,Error 日志(违反 G2),回成功 |
| 其余(gen_L < gen_R,或身份完全相同,或代次相等而有一侧会话无效) | `deleted` | 照旧原文 CAS 删除并扣人数 |

- **为什么不按会话号精确匹配**:同一连接登出后再登录,会话号不变而代次变大。精确匹配会误删这次新登录的 location。
- **为什么 gen_L < gen_R 必须删**:location 的主人比正在离场的会话还旧,已经被它顶替。不删就泄漏(幽灵人数;A′ 释放标记 300s 过期后,
  下次登录落到别的节点会永久拿 18)。这一行与现状完全相同。
- 本次改动只收窄一种情况:location 的主人比正在离场的会话**新**。

「有效」与 §1.2 第 5 条、d 的判定表、C++ `player_exit::IsBoundSession` 同一口径:非 0 且非 `0xFFFFFFFF`。v0.1 写的是「会话都非 0」,
与 d 不一致:sid_L 为 `0xFFFFFFFF`、代次相等时,d 判同会话放行,b 却判 `keep_conflict`,两张表对同一份 location 结论相反。
按现有发送点这种取值基本构造不出来,但两张表在 `placement_identity.go` 里共用同一个 `validSession` helper(§3.4)。

**`keep_newer` 不漏删的前提**(v0.2 补):被删会话记录里的 `login_generation` 不小于这条会话自己写进 location 的代次。
location 里某会话的代次只有两个来源:该会话已提交登录链的铸出值,以及 scene 自发请求带的实体代次(≤ gate 会话值,而 gate 会话值
也来自已提交的链)。所以只要「链铸出的值」与「记录里的值」一致,前提就成立。现有代码有两处会打破它:

- **player_locator 低于 fence 版本**(混版窗口:fence 之前的旧副本处理了 Reconnect,带 b 的新副本处理到期清理;全部副本都是旧版时
  `leasemonitor.go` 也不填身份,判 legacy,不会漏删):旧 Reconnect 不认 `ReconnectRequest.login_generation`,记录里仍是上一会话的 gA;
  login 只打一行 `reconnect reply generation mismatch` 的 WARN,下游照用铸出的 gB。location 为 (sB, gB)、记录为 (sB, gA),
  到期清理的 LeaveScene{sB, gA} 判 `keep_newer`、claim 被 ack,之后再没人删这条 location,人数一直多 1。
- **Reconnect 重放分支不比代次**(reconnectlogic.go,`State == ONLINE && SessionId == NewSessionId && RequestId == RequestId` 即回成功):
  失锁时两条并发链用同一个 request_id(客户端重试复用),第二条链拿到「成功」,记录里是第一条链的代次,第二条链却用自己更大的
  铸出值发 EnterScene,形状同上。

对应处置:

1. 发布顺序硬约束:player_locator 先于 scene_manager,回滚 player_locator 到 fence 之前要先回滚 scene_manager(§0、§3.5)。
2. Reconnect 重放分支补比代次:`session.LoginGeneration == in.LoginGeneration` 才算同请求重放;不等按 stale 拒绝
   (`Success=false`,ErrorMessage 写明 replay generation mismatch)。同一幂等键配上不同内容,按幂等的通行做法拒绝,不当成功回。
   代价:第二条链失败,会给同一个会话推 3023,当前 Unity 客户端会拆连接、走短线重连收敛,只伤活性。
   SetSession 的重放判断已是 `proto.Equal` 整条比较(含代次),不用改。
3. `keep_newer` 计数接告警,按速率阈值而不是出现即告警(b1–b4 的合法拦截也计这一项);日志带两边身份,便于与会话记录对照。
4. 残余登记见 §3.8。

### 3.3 契约改动(b 与 d 共用)

`proto/scene_manager/storage.proto` 的 `PlayerLocation` 追加两个字段:

```proto
  // 最近一次被 scene_manager 放行、并写入或确认这条 location 的 EnterScene 所代表的会话
  // (docs/design/scene-enter-chain-residuals.md §3 / §5)。0 / 0 = 无身份(旧版 scene_manager 写入或请求不带会话),
  // 所有比较退回旧行为。只由落点 Lua 写入;bump 回滚克隆旧值、keep 回滚恢复原字节,两者都保留身份。
  uint32 session_id = 8;
  uint64 login_generation = 9;
```

- 同时把 `rollback_receipt` 注释里「GO-3 的新字段从 8 起取」改为「GO-3 的新字段从 10 起取(8 / 9 已被会话身份占用)」。
- 类型符合 AGENTS §4:session_id 用 uint32,代次用 uint64。

`proto/scene_manager/scene_manager_service.proto` 的 `LeaveSceneRequest` 追加:

```proto
  // 被清理的那条会话记录的原值,由 player_locator 从被删的 PlayerSession 里取。0 / 0 = 旧版调用方,按旧行为删除。
  uint32 session_id = 5;
  uint64 login_generation = 6;
```

其它约定:

- **Redis**:不新增键。`player:{id}:location` 的值多两个 varint 字段(tag 8 / 9,wire type 0)。旧读者已核对:
  - Go / C++ 生成代码忽略或保留未知字段;
  - `tools/merge_zone/scene_hot_state.go` 的手写解码器 `decodePlayerLocation` 按 wire type 跳过未知字段号。
- **RPC 签名**:不变。
- **指标**:`scene_manager_placement_identity_total{site, verdict}`,`site ∈ enter | leave`。
  - enter 的 verdict:`fresh | same_session | legacy | newer | stale | conflict`;
  - leave 的 verdict:`legacy | keep_newer | keep_conflict | deleted`;
  - 不以 player_id 作 label。

### 3.4 逐文件改动点

W2(Go):

1. `go/scene_manager/internal/logic/placement_identity.go`(新):
   - `placementIdentity{sessionID uint32; loginGeneration uint64}`;
   - `identityOfLocation`、`identityOfLeaveRequest`;
   - `leaveVerdict`(iota 平铺常量 + `leaveVerdictCount` + `[...]string` 名表,由测试守住名表长度);
   - `validSession(sid uint32) bool`(非 0 且非 `0xFFFFFFFF`),b、d 两张判定表共用;
   - `decideLeaveIdentity`。
   d 的 `decidePlacementIdentity` 也放这个文件(§5.4)。
2. `leavescenelogic.go`:SceneId 比对之后插入 `decideLeaveIdentity`。
   - `keep_newer` / `keep_conflict` 打日志后返回成功;`legacy` 计数后往下走;`deleted` 照旧走到 `deletePlayerLocationIfUnchanged`。
   - 改写 :99-115 的「挡不住的两类」注释:第一类由 location 身份覆盖;第二类由「GET 后原文 CAS + 身份判定」覆盖,只剩混版窗口。
   - 新日志用英文。
3. `metrics/metrics.go`:新增 `placementIdentityTotal` 与 `ObservePlacementIdentity(site, verdict)`。
4. `go/player_locator/internal/logic/leasemonitor.go`:`notifySceneManagerLeave` 的 `LeaveSceneRequest` 填 `SessionId`、`LoginGeneration`;
   日志带上这两项。`markofflinelogic.go` 不改(它属 W1)。
5. `go/player_locator/internal/logic/reconnectlogic.go`(v0.2 新增):重放分支的条件补上 `session.LoginGeneration == in.LoginGeneration`;
   会话号与 request_id 都对得上而代次不等时,回 `Success=false` 并打一行英文 Info(带记录代次与请求代次),不走后面的代次栅栏与 CAS。
   注释写明它是 `keep_newer` 不漏删的前提之一(§3.2)。

### 3.5 新旧并存与回滚

- **默认值**:location 身份 0 / 0、请求 0 / 0,都判 `legacy`,与现状逐字节一致。
- **升级顺序**(v0.2 改,与 §0 一致):proto 生成 → scene(W3,开始给自发请求盖代次)→ player_locator 全部副本 → scene_manager 全部副本。
  - player_locator 必须先于 scene_manager:`keep_newer` 不漏删依赖 locator 已是会持久化 Reconnect 代次的 fence 版本,且重放分支已比代次
    (§3.2)。反过来先升 SM,混版 locator 副本处理的 Reconnect 会留下「记录代次 < location 代次」,到期清理判 `keep_newer` 而永久漏删。
  - 新 locator + 旧 SM:旧版忽略 5 / 6 号字段,等于旧行为。
  - 旧 locator + 新 SM:请求无身份,判 legacy,等于旧行为。只在回滚窗口出现,见下。
  - SM 新旧副本并存:旧副本落点时从零构造 location,身份被抹成 0,这些玩家在新副本上退化为 legacy。b 维持现状,没有新的失败模式。
- **回滚**(v0.2 改):按发布的反序。v0.1 的「任一组件单独回滚,顺序不限」删除。
  - 要把 player_locator 回滚到 fence 之前的版本(或回滚掉 Reconnect 重放分支的代次比较),先回滚 scene_manager。否则混版 locator
    副本又会写出 (sB, gA) 这类记录,新 SM 判 `keep_newer` 漏删。
  - 单独回滚 scene_manager、或只回滚 player_locator 的 `leasemonitor.go` 填身份,都只是退回 legacy。
  - 已写入 8 / 9 号字段的 location 对旧代码无害:Go 的 `proto.Clone` / `Marshal` 保留未知字段,C++ 忽略,merge_zone 跳过。
  - v0.1 写「回滚后再升级,最坏结果是 legacy 删除,不会误删新登录」不成立。旧 SM 的 samePlacement 不写 location,回滚期间留下的身份
    非 0 但落后于真实持有者:新 SM 写下 (sB, gB) → 回滚到旧 SM → 新登录 C 经旧 SM 的 samePlacement 进场,身份仍是 (sB, gB) →
    再升级 → B 迟到的 LeaveScene{sB, gB}(SM 重启期间失败、进了 LeaseMonitor 重试队列的尤其容易落在这里)判 `deleted`,C 的 location
    被删;B 的陈旧换图也会被判同会话放行。准确的说法是:回滚后残留的身份只会比真实持有者旧;在这名玩家下一次写入身份之前,
    b / d 维持现状(可能误删),不会比现状更糟。登记为混版残余(§3.8、§5.8)。
- 不设开关:判定在每次请求中都可推导,零值天然退化(与 fence §6 口径一致)。

### 3.6 不变量对照

| 不变量 | 结论 |
|---|---|
| 1 入口栅栏第一步且零副作用 | scene 入口不改。SM 侧照同一原则:身份判定在删除 Lua 之前,纯函数;判为 keep 时一个字节都不写 |
| 2 同一会话一律放行 | 离场不是入场,不拒绝任何入场。同会话、同代次照旧删除;同会话、location 代次更大(同连接重登)不删,保护的正是那次新登录 |
| 3 SessionMap 只擦本玩家的映射 | 不碰 C++ SessionMap |
| 4 gate `loginGeneration` 写入规则 | 不碰 gate |

### 3.7 测试(先复现,再修复)

`go/scene_manager/internal/logic/leavescene_identity_test.go`(新,miniredis 夹具沿用 `leavescene_cas_test.go` 与 `login_generation_test.go`):

| 用例 | 场景与断言 | 旧代码上 |
|---|---|---|
| `TestLeaveScene_StaleLeaveAfterSamePlacementRelogKeepsLocation`(b1) | location S1 / 节点 10 / (11, gA),人数 1;B(22, gB) 走 samePlacement 的 EnterScene,再 LeaveScene{S1, 11, gA} → location 仍在、身份为 (22, gB)、人数仍为 1 | 红(删除、人数归 0) |
| `TestLeaveScene_SameConnectionRelogKeepsLocation` | 同会话号 11,location 代次 g2,离场代次 g1 < g2 → 不删 | 红 |
| `TestLeaveScene_NewerPlacementResolvedByLocatorKept`(b2) | location S2 / (22, gB),LeaveScene{S2, 11, gA} → 不删,S2 人数不变 | 红 |
| `TestLeaveScene_SameSceneRewrittenByNewerLoginKept`(b3) | 同 scene_id 换节点后身份更新 → 不删 | 红 |
| `TestLeaveScene_OlderLocationIdentityIsDeleted` | location (11, g1),LeaveScene (22, g2) → 删除且只扣一次人数 | 绿(防泄漏) |
| `TestLeaveScene_LegacyRequestWithoutIdentityDeletes` / `TestLeaveScene_LegacyLocationWithoutIdentityDeletes` | 任一侧 0 → 删除 | 绿(兼容) |
| `TestLeaveScene_EqualGenerationDifferentSessionNotDeleted` | 代次相等、会话都有效且不同 → 不删 | — |
| `TestLeaveScene_EqualGenerationInvalidLocationSessionDeleted` | 代次相等、location 会话号为 `0xFFFFFFFF` → 删除(与 d 同口径) | — |

`placement_identity_test.go`(新):`TestDecideLeaveIdentity_Table`,逐行覆盖 §3.2 判定表;`TestLeaveVerdictNamesCoverEveryValue`;
`TestValidSessionMatchesCppIsBoundSession`(0、`0xFFFFFFFF` 无效,其余有效)。

`go/player_locator/internal/logic/scene_manager_client_fake_test.go`(新,W2 建、W1 不依赖):全包唯一的 `smpb.SceneManagerClient` 假实现,
加锁记录全部 LeaveScene 请求,可配错误。player_locator 的 logic 包里现在没有这个假实现,W1、W2 的测试同包编译,各写一份会撞名。

`go/player_locator/internal/logic/leave_scene_identity_test.go`(新,复用 `logic_test.go` 的 `newTestSvcCtx`,不改 `logic_test.go`;
辅助标识符加 `leaveIdent` 前缀):

- `TestMarkOffline_LeaveSceneCarriesDeletedSessionIdentity`:用上面的假 client 捕获请求,
  断言 SessionId / LoginGeneration 取自被删的会话记录。会话记录与请求的实例号要一致或留空,避开 e 的新判定。
- `TestLeaseExpiry_LeaveSceneCarriesSessionIdentity`。
- `TestMarkOffline_EnqueuedRetryPreservesGeneration`。

`go/player_locator/internal/logic/reconnect_replay_generation_test.go`(新,W2):

| 用例 | 场景与断言 | 旧代码上 |
|---|---|---|
| `TestReconnect_ReplayWithDifferentGenerationRejected` | 记录 ONLINE {会话 7, request "r1", 代次 g1};请求 {7, "r1", g2 ≠ g1} → `Success=false`,记录逐字节不变 | 红(回成功) |
| `TestReconnect_ReplayWithSameGenerationSucceeds` | 请求 {7, "r1", g1} → `Success=true`,记录不变 | 绿(回归) |
| `TestReconnect_LegacyReplayWithZeroGenerationSucceeds` | 记录与请求代次都为 0 → 成功 | 绿(兼容) |

### 3.8 残余

- 混版窗口(旧 SM 副本写出的无身份 location)里 b 维持现状。
- 回滚后残留的落后身份:在这名玩家下一次写入身份之前,b 维持现状、可能误删新登录(§3.5)。
- **`keep_newer` 漏删**(v0.2 补):前提是「记录代次 ≥ 本会话写进 location 的代次」(§3.2)。本批用发布顺序与 Reconnect 重放比代次守住它;
  前提被打破(违反发布顺序、混版 locator 副本、或将来新增不经 locator 代次栅栏就写会话记录的路径)时 location 永久泄漏:
  幽灵人数,A′ 标记 300s 过期后落到别的节点的登录一直拿 18。玩家下一次登录若落回同一节点,会改写这条 location 而收敛;
  落到别的节点则如上一直拿 18。以 `keep_newer` 计数的速率告警与日志里的两边身份观察。
- `keep_conflict`:契约被破坏(G2 不成立)时不删,有泄漏风险。选不删是因为删错会让在线玩家丢 location,后果更重;以 Error 日志与计数观察。
- 6b「同 scene_id、换节点、非死节点接管的落点多算一人」:迟到的 LeaveScene 原本碰巧抵消了它,b 修好后会显形,另立项(§10)。

## 4. 余项 c:`ReleasePlayer` 迟到退掉仍持有归属的实体

### 4.1 问题与时序

现状。全仓只有一条生产调用链:

- `EnterScene` 第 5b 步(enterscenelogic.go:701-728)。条件是 `currentLoc != nil && currentLoc.NodeId != "" && !samePhysicalNode`。
  它排在换手门与预占之后、第 6 步落点(:734,铸造 epoch + CAS 写 location)之前,也在第 7 步路由(:763-784,
  Kafka broker ACK 是提交点)之前。
- `dispatchReleasePlayer`(scene_node_client.go:333-375)在 safego 后台协程里执行:首发 1s deadline,失败后按 1s、3s 退避再试两次,
  最坏约 7s 才送达。
- `RequestNodeReleasePlayer`(:150-166)只填 player_id、target_scene_id、target_node_id。
- scene 侧 `HandleReleasePlayer`(cpp/nodes/scene/handler/grpc/scene_node_service.cpp:149-206,守护段):
  不在本节点 → 幂等返回;`IsHandoffRequested`(player_lifecycle.cpp:5147)为真 → 忽略;
  否则 `HandleExitGameNode(kReleasedByTransfer)`。守护段注释自认「采纳后迟到的 ReleasePlayer」是已登记残余。

时序(生产口径,旁路关闭;P 在节点 X,缓存 epoch = E,要换到节点 Y):

- **主时序(B5 之后迟到)**:
  1. X 被 18 暂拒 → 冻结、落盘、写标记 "E:t" → 带标记重发 EnterScene。
  2. SM 观察到 E(:296),换手门放行,**先**异步派发释放,首发失败进入重试。
  3. SM 铸 E+1,第 7 步 Kafka 写超时,`rollbackEnterSceneAfterRouteFailure` 把 location 退回 X、epoch 推到 E+2、写回执。
  4. X 收到应答,`ResolveTravelOutcome` 取证得到 B5:先抬 epoch(player_lifecycle.cpp:5721),再 `AbortTravelHandoff` 解冻(:5725)。
     此刻 `IsHandoffRequested` 变为假。
  5. 迟到的重试释放到达,实体按 `kReleasedByTransfer` 退出、销毁。

  结果:location 是 X@E+2,gate 会话仍绑在 X,X 上没有实体,会话变哑,只能重登。归属不变量没被破坏,伤的是活性。
- **变体 B(B4,落点或路由失败后 epoch 不动)**:第 6 步落点 Lua 返回 `errHandoffWithdrawn` / `errOwnerEpochConflict` / Redis 错误。
  X 取证得到 B4,epoch 仍为 E,解冻。迟到的释放同样退掉实体。**此时缓存 E 恰好等于期望值,只靠「带期望 epoch、相等才释放」挡不住**,
  必须让 SM 只在本请求提交之后才派发。
- **变体 C(回流)**:换到 Y 成功(E+1),释放仍在重试;约 6s 内 P 又换回 X(铸 E+2),X 新载入一个缓存为 E+2 的实体,
  迟到的释放把它退掉。带期望 epoch = E 即可挡住。
- **变体 D(dev 旁路)**:不铸造,路由失败走 keep 回滚,epoch 不变,迟到释放照样退掉实体。属旁路自身声明的不安全范围。

### 4.2 选定设计(方案 B)

**① 派发挪到本请求提交之后。** 删掉第 5b 步的派发,把它和注释挪到第 7 步 if / else 之后、`if takenOverLoc != nil`(:788)之前。

- 带 GateId 的请求要等路由 broker ACK 成功才派发;不带 GateId 的请求要等第 6 步落点成功才派发。两种情形共用这一个插入点。
- 条件不变:`currentLoc != nil && currentLoc.NodeId != "" && !samePhysicalNode`。
- 第 6 步的各个失败返回(:734-753)、第 7 步路由失败与回滚的返回(:765-779)都不再派发。变体 B 与路由回滚这两类根本不发释放。
- `EnterSceneStageReleaseDispatch` 指标随之移动。重试策略不变(只重试传输错误,预算约 7s):迟到已经无害。

**② 请求带 `expected_owner_epoch` = `observedEpoch`。** 它同时是落点 CAS 的期望值、换手门比对的标记 epoch,也是源实体写标记时的缓存值,
三者一致;首发与重试带同一个值。`currentLoc.OwnerEpoch` 可能因键被单独淘汰而与键值不一致,不用。

**③ scene 纯判定** `release_fence::Decide(expected, cached, handoffRequested)`,按顺序判,全部零副作用:

| # | 条件 | 结论 | 动作 |
|---|---|---|---|
| 1 | 交接在途(`IsHandoffRequested`) | `kSkipHandoffInFlight` | 忽略。现有守卫,语义不变 |
| 2 | expected == 0 | `kReleaseUnversioned` | 照旧释放,计数(SM 滚完后应恒为 0) |
| 3 | cached == 0(含组件缺失) | `kReleaseCacheUnknown` | 照旧释放,计数并 WARN |
| 4 | cached == expected | `kRelease` | 释放(现行为) |
| 5 | cached > expected | `kSkipStaleRelease` | 忽略,WARN,计数:实体已被回滚采纳(E+2)或已在更新的一代重新载入 |
| 6 | cached < expected(两者都非 0) | `kSkipAheadOfHolder` | 忽略,WARN,计数:生产上构造不出(标记 epoch 就是缓存值,缓存只增不减),只在 dev 旁路或键异常时出现 |

- 第 3 行不选 fail-closed:缓存为 0 的实体存盘不带 guard,留着它反而可能用旧状态覆盖 Redis 与 MySQL。
- 第 6 行选忽略:退出是破坏性动作,要求严格相等。这样的实体是陈旧实体,下一次带 guard 的存盘会被 CAS 拒掉、按废黜流程自毁,
  或由 `DiscardStaleHandoffEntity` / AFK 收掉,不会写坏数据。
- 判为 1、5、6 时不调 `HandleExitGameNode`,也不做 `MergeExitCause` 或 rekick。
- **响应保持 Empty**:释放是通知,忽略就是终态,重试也不会改变结论。改成非 OK status 会被 SM 的重试循环当成传输错误白白重试。

### 4.3 契约改动

`proto/scene_manager/scene_node_service.proto`。只动 `ReleasePlayerRequest`(:41)与 `rpc ReleasePlayer` 的注释(:16-20),
不碰在途的 `AssetAckReceipt`:

```proto
message ReleasePlayerRequest {
  uint64 player_id = 1;
  uint64 target_scene_id = 2;
  string target_node_id = 3;
  // SM 本次 EnterScene 观察到、并用于落点 CAS 的 owner_epoch,即旧节点在提交时刻应持有的那一代。
  // scene 只在本地缓存 PlayerOwnerEpochComp.epoch 与它相等时释放;缓存为 0 时按旧行为释放并计数;其余忽略。
  // 0 = 旧版调用方或尚未铸造,按旧行为释放并计数。不进存盘,不与 login_generation 混用。
  uint64 expected_owner_epoch = 4;
}
```

- rpc 注释改为两点:交接在途的实体不处理,其余按期望 epoch 比对;SM 只在本次 EnterScene 提交(路由 broker ACK,或无 GateId 时落点成功)之后才发。
- 响应仍是 `google.protobuf.Empty`,RPC 签名与消息号 128 不变。
- Go 内部签名(加最后一个参数 `expectedOwnerEpoch uint64`):
  - `RequestNodeReleasePlayer(ctx, svcCtx, zoneId, nodeId, playerId, targetSceneId, targetNodeId, expectedOwnerEpoch)`
  - `dispatchReleasePlayer(svcCtx, log, oldZoneID, oldNodeId, playerId, targetSceneId, targetNodeId, expectedOwnerEpoch)`
- C++ 新纯头 `cpp/libs/services/scene/player/system/release_fence.h`,形状照 `enter_fence.h`:
  - `enum class Verdict : uint8_t { kRelease, kReleaseUnversioned, kReleaseCacheUnknown, kSkipHandoffInFlight, kSkipStaleRelease, kSkipAheadOfHolder, kCount };`
  - 平铺名表 `kVerdictNames`(release / release_unversioned / release_cache_unknown / skip_handoff_in_flight / skip_stale_release /
    skip_ahead_of_holder)+ `static_assert(std::size(kVerdictNames) == kVerdictCount)`,不用 X-macro。
  - `constexpr Decide(...)`、`constexpr bool Releases(Verdict)`。
  - `release_fence_stats`:`verdicts[]` 原子计数、`notFound` 计数、`Snapshot` / `Read`。
  - 不含 entt / redis / protobuf,不读时钟。
- C++ 库接口:`static void PlayerLifecycleSystem::HandleReleaseFromSceneManager(Guid playerId, uint64_t expectedOwnerEpoch,
  uint64_t targetSceneId, const std::string &targetNodeId);`
- 不改 Lua、Redis 键值、A2′ 跨语言金样。不新增 tip 码。

### 4.4 逐文件改动点

W2(Go):

1. `scene_node_client.go`:两个函数加参数;`RequestNodeReleasePlayer` 填 `ExpectedOwnerEpoch`;日志带 `expected_owner_epoch`;
   文件头注释写明「重试按代际幂等」。
2. `enterscenelogic.go`:按 §4.2 ① 移动派发;注释说明期望值为何取 observedEpoch(落点 CAS 已证实它是提交时刻的当前代)。
   为了能确定地测「未派发」,引入包级钩子 `var releaseDispatcher = dispatchReleasePlayer`,做法同现有的 `nodeDialer`。
3. `integration_test.go`:`fakeSceneNode.ReleasePlayer`(:90-93)加锁记录全部请求。

W3(C++):

1. `release_fence.h`(新),登记进 `cpp/libs/services/scene/scene.vcxproj` 与 `.filters`(ClInclude)。
2. `player_lifecycle.h` / `.cpp`:新增 `HandleReleaseFromSceneManager`,放在 `IsHandoffRequested`(.cpp:5147、.h:1245)旁边,
   避开 MySQL 围栏与资产提交的在途 hunk,不碰 `PlayerOwnerEpochComp` 的定义。顺序:
   1. playerList 查找,找不到则计 `notFound`、DEBUG 后返回;
   2. 读 `IsHandoffRequested`;
   3. 用 `try_get` 只读 `PlayerOwnerEpochComp`,缺失按 0;
   4. 调 `Decide` 并计数;
   5. skip 时打 WARN(带 expected / cached / target)后返回;
   6. 释放时调 `HandleExitGameNode(entity, ExitCause::kReleasedByTransfer)`。

   scene_node_service.cpp 守护段里「交接中不走退出」的注释搬到库函数旁,改写成派发后移之后的事实(SM 已不再先于落点派发)。
   `ResolveTravelOutcome` B5 采纳处(:5721 先于 :5725)加一行注释:本栅栏依赖「先抬 epoch、后摘交接组件」,重构不得打破。
3. `cpp/nodes/scene/handler/grpc/scene_node_service.cpp`:`HandleReleasePlayer` 守护段改为一行委托
   `PlayerLifecycleSystem::HandleReleaseFromSceneManager(request->player_id(), request->expected_owner_epoch(), request->target_scene_id(), request->target_node_id());`。
   守护段之外不动(文件里有 AssetAckReceipt 的在途骨架)。
4. `cpp/libs/services/scene/core/system/redis.cpp`:紧跟 `[EnterFence]`(:127-152)加一行 `[ReleaseFence]`。
   key=value 平铺、累计值、有变化才打;这一拍 `skip_stale_release`、`skip_ahead_of_holder` 或 `release_cache_unknown` 有增长时升 WARN,
   否则 INFO。lambda 捕获 `lastReleaseFence`,与 `lastEnterFence` 同法。

### 4.5 新旧并存与回滚

- **先 scene、后 SM**:
  - scene 先升:旧 SM 发 0,全部判 `kReleaseUnversioned`,行为与现状一致。用这段时间确认 `[ReleaseFence]` 计数正常。
  - SM 滚动期间:旧副本仍早派发、带 0;新副本提交后派发、带期望值。每个请求只派发自己的释放,互不干扰。
  - 新 SM 的释放到达旧 scene:未知字段 4 被忽略,按旧行为释放。缺陷照旧,但不会更糟。
- **回滚**:只回滚 SM → 回到现状;只回滚 scene → 字段被忽略,回到现状;任意顺序都安全。没有持久化格式、Redis 键或 Lua 变更。
- 不设开关(理由同 fence §6)。
- dev 旁路(`AllowUnsafeCrossNodeHandoff=true`)下,派发推后一个 Kafka 写时延,旁路本来就有的「新节点读到旧快照」窗口相应略增。只影响 dev。

### 4.6 不变量对照

| 不变量 | 结论 |
|---|---|
| 1 入口栅栏第一步且零副作用 | 入场入口不改。`HandleReleaseFromSceneManager` 照同一原则:查到实体后、任何副作用前先做纯判定;被忽略的请求不碰会话、pending、票据、待确认条目与退出意图 |
| 2 同一会话一律放行 | 释放不带会话、不比登录代次,不放行也不拒绝任何入场;只看归属代际 |
| 3 SessionMap 只擦本玩家的映射 | 不新增擦除;判为释放时走既有退出链,擦除沿用 `EraseSessionMappingIfOwned`(player_lifecycle.cpp:2424)。本项只减少进入退出链的次数 |
| 4 gate `loginGeneration` 写入规则 | 不改 gate;SM 只移动释放的派发时机,不改 `RoutePlayerEvent` 的内容、时机与回滚语义 |

另需守住:CZ-3 与「唯一的非路由写入口」(只读本地缓存,不读 Redis、不采纳 epoch);ECS 读路径用 `try_get`、不懒建组件。

### 4.7 测试(先复现,再修复)

C++ `cpp/tests/cross_zone_test/cross_zone_test.cpp`(只在末尾追加):

| 用例 | 断言 | 旧代码上 |
|---|---|---|
| `ReleaseFence.DecideTableEveryCell` | handoff 真 / 假 × expected{0, E} × cached{0, E-1, E, E+2},六种结论与 `Releases()` | — |
| `ReleaseFence.NamesCoverEveryValue` | 名表与 kCount 一致 | — |
| `ReleaseFenceEcs.LateReleaseAfterRollbackAdoptKeepsEntity` | 实体 epoch = E+2、无交接组件(有现成 B5 取证 harness 时走真实 `ResolveTravelOutcome`);释放带 E → 实体仍在 playerList,无 UnregisterPlayer、无退出意图,`skip_stale_release` +1 | 红(先把守护段搬进库函数、暂不加栅栏,此用例为红) |
| `ReleaseFenceEcs.LateReleaseOnReturnedEntityIgnored` | 新载入实体 E+2,释放带 E → 保留 | 红 |
| `ReleaseFenceEcs.EqualEpochReleases` | E / E → 挂 UnregisterPlayer,退出原因 `kReleasedByTransfer` | 绿 |
| `ReleaseFenceEcs.UnversionedReleaseKeepsLegacy` | 期望 0 → 释放,`release_unversioned` +1 | 绿 |
| `ReleaseFenceEcs.CacheUnknownReleasesAndCounts` | 缓存 0、期望 E → 释放并计数 | 绿 |
| `ReleaseFenceEcs.AheadOfHolderIgnored` | 缓存 E、期望 E+1 → 忽略并计数 | 红 |
| `ReleaseFenceEcs.HandoffInFlightStillIgnored` | 交接中,期望任意 → 忽略 | 绿(回归) |
| `ReleaseFenceEcs.StaleReleaseOnExitingEntityDoesNotMerge` | 退出中的实体缓存 E+2、释放带 E → 意图的 cause / suppressedBy / resaveRounds 都不变 | 红 |

Go `go/scene_manager/internal/logic/release_dispatch_test.go`(新):

| 用例 | 断言 | 旧代码上 |
|---|---|---|
| `TestEnterScene_RouteFailureDoesNotDispatchRelease` | 跨节点凭标记放行,fake Kafka 写失败走 bump 回滚 → `releaseDispatcher` 调用 0 次(同步钩子,不靠 sleep) | 红 |
| `TestEnterScene_PlacementFailureDoesNotDispatchRelease` | 分别构造 `errHandoffWithdrawn`(预检后改写标记)与 `errOwnerEpochConflict`(预检后 INCR)→ 都不派发 | 红 |
| `TestEnterScene_ReleaseCarriesObservedOwnerEpoch` | 成功交接,期望值 = 标记 epoch E,不是 E+1 | 红 |
| `TestEnterScene_DevBypassReleaseCarriesUnmintedEpoch` | 旁路下期望值为 E,且在路由成功后才派发 | 红 |
| `TestEnterScene_NoGateIdCrossNodeDispatchesAfterPlacement` | 无 GateId:落点成功后派发、带 E;落点失败不派发 | — |
| `TestDispatchReleasePlayer_RetriesCarrySameExpectedEpoch` | fake 前两次 Unavailable、第三次成功,三次请求的期望值相同 | — |

回归:`logic_test.go` 与 `owner_epoch_test.go` 里现有的「不得调用 ReleasePlayer」断言保持通过。

### 4.8 残余

- 生产上释放几乎总是 no-op:成功路径里,释放到达时源实体要么仍在交接中被忽略,要么已销毁。方案 D(生产口径不再发释放、只在旁路发)
  可作为后续收紧,本批不做。
- 【未核实】A1′ 写标记之后退出是否可能被取消、实体又回到活跃且不在交接中。若存在,「活实体只在交接收口后才会收到释放」要收窄,
  但栅栏的结论不受影响(缓存等于期望值时释放,与现状相同)。
- 【未核实】生产上 ReleasePlayer 首发失败走到重试的频率:看 `scene_manager_release_player_total{outcome}`。

## 5. 余项 d:落点 Lua 不认会话

### 5.1 问题与时序

现状。`EnterScene`(enterscenelogic.go:127)在 :289 读 location 原文,在 :296 读 owner_epoch(本请求唯一一次观察)。
落点 `placePlayerLocation`(changesceneutil.go:128)每次从零构造 `PlayerLocation`,经两段 Lua 写入:
`luaMintEpochAndSetLocation`(owner_epoch.go:103)与 `luaSetLocationIfEpoch`(:130)。两段都只做 epoch CAS,
**既不比 location 原文,也不认会话**。samePlacement(:658-674)退还预占后发路由,location 一个字节都不写。

- **d1 陈旧登录**:
  1. 链 A 的 EnterScene RPC 在客户端超时或链 A 丢锁,SM 的处理协程仍在跑,已观察到 L0 = (S1, X, N)。
  2. 链 A 失败、释放锁;链 B 提交会话后发 EnterScene,命中 samePlacement,不写 location,路由发往 gateB。
  3. A 的协程继续,目标 S2 在同一节点 X,不铸造,`luaSetLocationIfEpoch(N)` 通过(B 没动 epoch)。location 改成 S2,S1 -1、S2 +1,路由发往 gateA。
  4. gateA 按 gA 转发后,scene 栅栏判 `kRejectStale`。

  结果:location 写着 S2,B 实际在 S1;人数 S1 少 1、S2 多 1,直到下一次落点才收敛。这就是 fence §9 残余 2。
- **d2 旧会话在途的换图**:实体还绑着 sA 时发出换图 EnterScene{sA, 同节点 S2, 代次 0};B 的 samePlacement 先到;
  A 的换图后到,同样不铸造就改写 location 与人数。
  - A 若是跨节点:凭标记铸造时,B 一侧的 epoch 也被推进,X 上已绑 sB 的实体之后存盘被拒、被废黜。
  - A 若是跨区第一条腿:会把 B 的有效落点改成「等待落点」。

现有 owner_epoch CAS 只挡得住「两个都铸造(或一方铸造)」的并发,挡不住同节点不铸造的落点与 samePlacement。

抽查补正:调研写「scene 侧对未知非 0 码的处理未核实」。现核实,`HandleTravelEnterSceneReply`(player_lifecycle.cpp:5886)对任何非 0 码
都走 `ResolveTravelOutcome(kFailed)`,也就是原子取证后按 B4 解冻。新码 25 落在这条分支上。

### 5.2 选定设计(方案 A)

**① EnterScene 入口纯判定 `decidePlacementIdentity`。**

- 位置:读完 epoch(:296-299)之后、Gone 过滤(:313)之前,也在跨区分支、自动选线预占(:475)、同节点补种、任何落点写与路由之前。
- 用**观察到的原始** currentLoc,不受 Gone / Dead 过滤影响。拒绝时只会走到已有的去重 defer(:172-197,
  错误应答释放去重占位),没有别的东西要退。
- L = location 里记的身份,R = 请求的 `(in.SessionId, in.LoginGeneration)`:

| 情形 | 结论 | 应写入的身份 |
|---|---|---|
| L 不存在(无 location,或 sid_L = 0 且 gen_L = 0) | `admit_fresh` | R |
| sid_R 有效且 sid_R == sid_L | `admit_same_session` | (sid, max(gen_L, gen_R)) |
| gen_R == 0(会话不同) | `admit_legacy`,计数 | gen_L ≠ 0 时保持 L 不变(不降级);gen_L == 0 时 (sid_R, 0) |
| gen_L == 0,gen_R ≠ 0 | `admit_newer` | R |
| gen_R > gen_L | `admit_newer` | R |
| gen_R < gen_L | `reject_stale` | — |
| gen_R == gen_L,会话不同 | sid_L 有效 → `reject_conflict`;sid_L 无效 → 按同会话放行 | — / (sid_R, gen) |

- 拒绝回新码 25 `ErrPlacementSessionSuperseded`,`enter_scene_rejected_total{reason=session_stale|session_conflict}`;
  一行英文 Info 日志带玩家、请求身份、当前身份与结论。
- **legacy 行不降级已有身份**(v0.2 改):v0.1 让 legacy 行写 (sid_R, 0),会把 location 已有的高代次身份降掉,而且这个降级在全部组件
  升级之后仍会留着。时序:location 身份 (sB, gB) → 陈旧实体 A(会话 sA,旧版 scene 不盖章、或实体没有身份组件,代次 0)发同节点换图,
  legacy 放行,身份被写成 (sA, 0) → scene 全部升级后,A 的实体再发换图带上 gA,与 sid_L 相同,判 `admit_same_session`,d2 重新打开;
  A 迟到的 LeaveScene{sA, gA} 遇到 gen_L = 0 走 legacy 删除,b1 重新打开。这种状态要到 B 下一次写入身份才结束。
  改为 gen_L ≠ 0 时保持 L(落点照常写入,只是身份字段沿用 L;samePlacement 因此不需要写身份)。复核意见建议按 `correlation_id`
  区分 scene 自发与 login 发起、只对前者保持 L;逐一推演后,对 login 发起的无代次请求(旧版 login 的新会话)写 (sid_R, 0) 与保持 L
  在 b 与 d 的后续判定上结论相同(都是 legacy 删除或放行);唯一不同是代次小于 gA 的陈旧登录:保持 L 时判 stale,
  写 (sid_R, 0) 时因 gen_L = 0 被放行。保持 L 更严而不是更松。所以不引入这个区分,规则更简单,也不依赖旧版 scene 是否填了 `correlation_id`。
- **与 scene 栅栏只差一行**:「请求无代次、当前有代次、会话不同」在 scene 判 `kRejectUnversioned`,这里判 legacy 放行。
  理由是 location 的寿命长于实体:被踢的旧实体会销毁,location 却还在。这里若拒绝,login 滚动混版或回滚到无代次版本期间,
  location 带代次的玩家每次经旧 login 登录都会被拒,直到 30s 以上的租约清掉 location,这是活性回退。scene 栅栏仍在入口兜底。

**② 何时写身份。**

- 所有放行的落点(同 zone 落点、跨区第一条腿的等待落点、死节点接管、首次落点):`placePlayerLocation` 新建的 `PlayerLocation`
  带上应写入的身份。
- **samePlacement**:应写入的身份不等于 L 时,克隆 currentLoc,保留 owner_epoch、rollback_receipt、update_time、pending 等全部字段,
  只改身份,用不铸造 Lua(present 模式,期望 = 观察原文,epoch = observedEpoch)写入。
  - CAS 失败回 19,不发路由。
  - 随后推路由失败时(v0.2 改)**不回滚身份**,只返回 `ErrKafkaRoute`(7),与现状 samePlacement 推路由失败的返回一致。
    v0.1 设计的 keep 回滚会在「kafka-go 报错但 broker 其实已投递」时重开 b1 / d2(enterscenelogic.go 第 7 步的注释承认这种情况):
    A 的会话已删、LeaveScene{S1, sA, gA} 在途 → B 写身份 (sB, gB) → 推路由报错但已投递 → keep 回滚把身份退回 (sA, gA) →
    scene 把实体改绑到 sB → A 的 LeaveScene 到达,L 与 R 完全相同,判 `deleted`,location 被删、S1 人数 -1;同一窗口里
    A 实体在途的换图 (sA, gA) 也会被判同会话放行。
    不回滚是安全的:身份写入不动人数;B 的会话此时已提交,B 自己最终的清理带 (sB, gB) ≥ 当前身份,照样能删干净;
    路由真的没投递时,旧实体在途的 (sA, gA) 换图会拿 25,而它所属的会话本来就已被 B 取代。
    7 不在 C++ `SmRejectedBeforeAnyPlacementWrite` 白名单里,下游不要求「失败应答零写入」。
  - 身份写入与 GateId 无关;只有带 GateId 才会推路由。
- 身份不变:维持现状,不写。只送连接的重定向、各类拒绝:不写。

**③ 两段落点 Lua 加 location 原文期望。** ARGV[4] 为模式(`any` | `missing` | `present`),ARGV[5] 为期望原文(只在 present 时用)。

- ARGV[4] 缺省或为空串按 `any`,不检查。现有直接 EVAL 这两段脚本、只传 3 个 ARGV 的测试(owner_epoch_test.go:184 / :194、
  owner_epoch_crosslang_test.go:292)因此照常通过。
- 铸造版:epoch 比对(含原有的重放识别分支,放在原文检查之前,保持不变)→ location 期望不符返回 0 → 标记比对(-1)→ INCR + SET。返回值集合不变。
- 不铸造版:epoch 不符返回 0 → **location 已等于 ARGV[2] 返回 1**(go-redis 原样重发时幂等)→ location 期望不符返回 0 → SET 并返回 1。
- Go 侧期望取观察原文 `observedLocRaw`(:289 读到的原文,不受 Gone / Dead 清空影响):为空用 `missing`,否则用 `present`。
  Gone / Dead 分支清空的 currentLocRaw 继续只作为回滚基线。
- `expectLoc` 的零值语义(v0.2 补):Go 侧 mode 的零值就是 `any`(ARGV[4] 传空串),只供测试夹具直接调用 `placePlayerLocation`
  与 `UpdatePlayerLocation` 使用。现有 28 处测试直接写不带 expectLoc 的 `placementGuard` 字面量(owner_epoch_test.go、
  home_zone_merging_test.go、owner_epoch_crosslang_test.go,2026-10-16 工作区计数),零值若定义成 `missing`,这些用例在 location 已存在时会全部回 19。
  反过来,零值等于 `any` 意味着将来新增的生产构造点漏填时原文 CAS 静默失效。生产的三处构造点(跨区 guard、常规 guard、samePlacement)
  必须显式填 `missing` 或 `present`,由 §5.7 的行为用例逐处守住:每一处都有「观察之后 location 被改写或删除 → 回 19」的用例,
  漏填即红。不另加运行期计数。
- 理由:没有原文 CAS 时,A 在 B 更新身份之前完成观察与判定,之后仍能用同一个 epoch 覆盖掉 B 写入的身份,判定就形同虚设。

**④ scene 自发请求带实体代次。** 复用 `EnterSceneRequest` 的 12 号字段,不新开字段。值取实体 `PlayerLoginIdentityComp.loginGeneration`,
前提是组件存在、请求会话有效且与组件会话相同,否则填 0。纯函数 `enter_fence::ActingLoginGeneration(const PlayerLoginIdentityComp *, SessionId)`
放在 `enter_fence.h`。三处发送点:

- `RequestSceneChange`(player_lifecycle.cpp:4906):在 `SendCorrelatedEnterScene` 之前,`req.login_generation() == 0` 时用实体身份盖章。
  覆盖客户端换图(player_scene_handler.cpp:162)、镜像自动进场(scene_manager_response_handler.cpp:173)、队伍跟随(player_team.cpp:505),
  这三个文件本身不改。
- 交接 EnterScene `RequestTravelEnterScene`(:5394,发送在 :5473):playerEntity 在作用域内,用快照会话盖章。
- 疏散改派:`EmergencyRelocateTicket`(:101-107)加 `uint64_t loginGeneration{0}`。`DispatchEmergencyRelocate`(:3585)把
  :3592 的 `const` 票据副本改为可写,在 :3616 读身份组件时按票据会话算出代次填进票据;票据按值捕获进 Redis 回调。
  `SendEmergencyRelocateEnterScene`(:3826)执行 `req.set_login_generation(ticket.loginGeneration)`。

> **2026-10-17 契约勘误**：这里只订正镜像调用点：自动进场已迁到 `player_scene.cpp` 的 `PlayerSceneSystem::HandleMirrorCreateDone`，仍经 `RequestSceneChange` 盖实体代次；旧的 `scene_manager_response_handler.cpp` 已删。本勘误不改变本计划其余步骤、状态或验证结论。

SM 继续把 12 号字段原样放进 `RoutePlayerEvent.login_generation`(7)。gate 的 `ApplyRouteIdentity` 对同一角色取 max;
实体代次 ≤ 会话值,所以会话值不变。

**⑤ 新私有码 25。** `go/scene_manager/internal/constants/errors.go` 新增 `ErrPlacementSessionSuperseded uint32 = 25`:落点会话身份
已被更新的登录取代(stale 或 conflict),一个字节都没改;陈旧请求方不应以同一会话重试。注释写明**只许在任何落点写之前返回**
(C++ 依赖这一点)。C++ `player_lifecycle.h` 同步手抄 `kSmErrPlacementSessionSuperseded = 25`,并加入
`SmRejectedBeforeAnyPlacementWrite`(依据:唯一出处是 `decidePlacementIdentity`,早于任何预占与写)。

**⑥ 人数一致。** 判定在一切预占之前,拒绝时没有东西要退;只改身份的写入不动人数;原文 CAS 失败沿用 :737 的退还预占;
LeaveScene 仍是「谁删谁减」。

### 5.3 契约改动

- `PlayerLocation` 8 / 9、`LeaveSceneRequest` 5 / 6:见 §3.3。
- `EnterSceneRequest.login_generation = 12`:不加字段,只改语义注释(scene_manager_service.proto:107-112)。
  - login:本链铸出的代次。
  - scene 自发:实体已放行的代次,会话号与请求一致时才填,可落后于会话记录。
  - SM 不再只透传:用它做落点身份裁决,同时仍原样进入 `RoutePlayerEvent.login_generation`(7)。
  - 仍不进 request_id 指纹(enterscenelogic.go 算指纹的部分不变)。
- 错误码 25:scene_manager 私有数轴,不是 tip 码,不触发 AGENTS §7.5。
- Lua:两段落点脚本的 ARGV[4] / ARGV[5],见 §5.2 ③。`luaDeletePlayerLocationIfUnchanged`、`luaRollbackPlayerPlacement` 不变。
- 指标:见 §3.3;`enter_scene_rejected_total` 的 Help 补上两个 reason。
- Kafka 格式不变。scene 自发路由的 `RoutePlayerEvent.login_generation` 从 0 变成实体代次(≤ gate 会话值),gate 合并结果不变。

### 5.4 逐文件改动点

W2(Go):

1. `placement_identity.go`(新,与 §3.4 同一文件):`placementVerdict`(iota 平铺 + count + 名表)、`decidePlacementIdentity(cur, curExists, req) (verdict, record)`、
   `identityOfEnterRequest`;有效会话判定用 §3.4 的同一个 `validSession`(非 0、非 `0xFFFFFFFF`,与 C++ 一致)。
2. `enterscenelogic.go`:
   - :296-299 之后:`observedLocRaw := currentLocRaw`,调 `decidePlacementIdentity`。拒绝 → 计数、日志、`errResp(ErrPlacementSessionSuperseded, ...)`(文案跟随本文件现有的中文错误串)。
   - 跨区 guard(:425)与常规 guard(:650):补 `identity: record` 与 `expectLoc`(显式 `missing` / `present`,不留零值)。
   - samePlacement(:658-674):按 §5.2 ② 写身份;推路由失败不回滚身份,照旧回 7。
   - 19 的日志措辞改为「epoch 或 location 被并发推进」。
   - c 的派发后移也在本文件(§4.4),两项同组、同一执行者顺序落,不会文本冲突。
3. `changesceneutil.go`:
   - `placementGuard`(:92)加 `identity placementIdentity` 与 `expectLoc{mode, raw}`;mode 的零值即 `any`,注释写明只供测试夹具与
     `UpdatePlayerLocation`,生产构造点必须显式填写(§5.2 ③);
   - `placePlayerLocation`(:128)给 loc 填身份,Eval 追加 mode 与 raw;
   - `UpdatePlayerLocation`(:69,只有测试夹具调用)用 `any`;
   - 新增 `setLocationIdentityIfUnchanged`。
4. `owner_epoch.go`:两段 Lua 按 §5.2 ③ 修改,同步 ARGV 文档注释;`errOwnerEpochConflict`(:261)的注释改为「epoch 或 location 被并发推进」。
5. `constants/errors.go`:新增码 25 与注释。
6. `metrics/metrics.go`:见 §3.4。

W3(C++):

1. `enter_fence.h`:新增 `ActingLoginGeneration`。头注释写明它只读、只供发往 scene_manager 的请求盖章,不进入场判定。
2. `player_lifecycle.h`(:1100-1145):新增常量;把「另外七个拒绝码」改为八个,并在判据说明里补 25;`SmRejectedBeforeAnyPlacementWrite` 加 case。
3. `player_lifecycle.cpp`:按 §5.2 ④ 改三处发送点。读组件一律用 `try_get`,不懒建。

### 5.5 新旧并存与回滚

- **升级顺序**(与 §0 一致):proto 生成 → scene → player_locator → scene_manager;login 与 gate 不因 d 发版。
  d 本身不依赖 player_locator,locator 排在 SM 之前是 b 的约束(§3.5)。
  - 新 scene + 旧 SM:12 号字段非 0,只进入路由,gate 取 max 后会话值不变,无行为变化。
  - 旧 scene + 新 SM:自发请求代次为 0;同会话照常放行,不同会话走 legacy 放行、且不降级 location 已有的身份(§5.2 ①),
    d2 在这个窗口里维持现状,窗口结束后不留后遗症。
  - SM 新旧副本并存:旧副本从零构造 location(身份被抹成 0)、没有原文 CAS、samePlacement 不写身份;这些玩家在新副本上退化为 legacy。
    两种副本各自 EVAL 自己的脚本文本,不存在脚本版本错配。
  - login 混版或回滚到无代次版本:请求代次为 0,走 legacy 放行,不会卡死。这正是不照搬 `kRejectUnversioned` 的原因。
- **回滚**(v0.2 改):按发布的反序。单独回滚 scene 或 SM 都只退回旧行为;回滚 player_locator 的约束见 §3.5。
  码 25 只有新版 SM 会发:旧 login 对任何非 0 码按失败处理、推 3023;scene 侧对非 18 码给
  `kEnterSceneFailed`,交接走 `ResolveTravelOutcome(kFailed)`(§5.1)。回滚 SM 前先回滚 login 的既有约束(fence §6,指纹)不变。
  回滚到旧 SM 期间 samePlacement 不写身份,location 里会留下落后于真实持有者的身份;再升级后到这名玩家下一次写入身份之前,
  d 维持现状(陈旧换图可能被判同会话放行),不会比现状更糟(§3.5)。
- **并发语义变化**:同节点不铸造的落点从「后写者赢」变为「后到者拿 19 重试」。同一玩家正常最多只有 login 与 scene 两个来源,
  scene 侧又是单槽在途,所以只影响本来就该串行化的跨来源竞态。
- **去重**:请求字段没有增加,指纹不变,旧去重条目与新代码互认。
- 不设开关。

### 5.6 不变量对照

| 不变量 | 结论 |
|---|---|
| 1 入口栅栏第一步且零副作用 | C++ `HandleEnterGameNode` 第 0 步不改;C++ 只改发往 SM 的请求盖章,不在入场处理路径上。SM 照同一原则:身份判定在读完 location 与 epoch 之后、一切预占 / 补种 / 写入 / 路由之前,纯函数;此前唯一的副作用是已有的去重 SETNX,由 defer 在错误应答时释放 |
| 2 同一会话一律放行 | scene 栅栏不改;SM 判定表对同会话一律放行、代次取 max,不因 G4 落后拒绝。同会话唯一新增的失败是原文 CAS 冲突回 19,发生在发路由之前,gate 根本没收到路由,不会出现「gate 已在 kSent 提交而 scene 未执行」的三方分叉。已登记的 R13(gate 重启后会话号复用)在 SM 同样按同会话放行,不是本次引入 |
| 3 SessionMap 只擦本玩家的映射 | 不碰 C++ SessionMap 擦除路径 |
| 4 gate `loginGeneration` 写入规则 | gate 不改。scene 自发路由带实体代次(≤ 会话值),`ApplyRouteIdentity` 对同一角色取 max,会话值不变,对别的角色不动;SM 拒绝意味着不产生路由,不属于 gate 丢路由 |

另:只新增字段号、不复用;不新增 player_id label;登录代次不进 DBTask 与 owner_epoch(G5)。

### 5.7 测试(先复现,再修复)

Go `go/scene_manager/internal/logic/enterscene_identity_test.go`(新):

| 用例 | 断言 | 旧代码上 |
|---|---|---|
| `TestEnterScene_StaleLoginAfterNewerSamePlacementRejected`(d1) | B 先 samePlacement;A(11, gA < gB)指定同节点的 S2 → 回 25,location 原文不变,S1 / S2 / 节点人数不变,Kafka 无消息 | 红(改写成 S2) |
| `TestEnterScene_StaleSessionSceneChangeRejected`(d2) | location 身份 (22, gB),请求会话 11、代次 gA、CorrelationId ≠ 0 → 回 25,什么都没写 | 红 |
| `TestEnterScene_SameSessionWithoutGenerationAdmittedPreservesGeneration` | 同会话代次 0 放行,location 代次保持 gB | — |
| `TestEnterScene_UnversionedDifferentSessionAdmittedAsLegacy` | 钉住混版口径:location 无代次时放行并写 (sid_R, 0) | — |
| `TestEnterScene_LegacyAdmitDoesNotDowngradeIdentity` | location (22, gB),请求 (11, 0)、CorrelationId ≠ 0 → 放行,location 身份仍为 (22, gB) | — |
| `TestEnterScene_SamePlacementUpdatesIdentityPreservingOtherFields` | location 带 rollback_receipt / pending / update_time,samePlacement 之后只有 8 / 9 变;owner_epoch 键与人数不变 | — |
| `TestEnterScene_SamePlacementIdentityCASFailsWhenLocationDeleted` | miniredis 钩子在 GET 与 EVAL 之间删 location(模拟 LeaveScene)→ 回 19、不发路由 | — |
| `TestEnterScene_PlacementRejectedWhenIdentityChangedSinceObservation` | 钩子改写身份,同节点不铸造落点回 19,location 不被覆盖(常规 guard 的 `present`) | 红 |
| `TestEnterScene_FirstPlacementRejectedWhenLocationAppearedSinceObservation` | 观察时无 location,钩子在 EVAL 前写入一条 → 首次落点回 19(常规 guard 的 `missing`) | 红 |
| `TestEnterScene_CrossZoneFirstLegRejectedWhenLocationChangedSinceObservation` | 跨区第一条腿,钩子改写 location → 回 19,不写等待落点(跨区 guard) | 红 |
| `TestEnterScene_SamePlacementRouteFailureKeepsIdentity` | Kafka writer 报错 → 回 7;location 身份保留为本请求写入的值,owner_epoch 键与人数不动 | — |
| `TestEnterScene_CrossZoneFirstLegStaleRejected` | 陈旧请求不写等待落点 | 红 |

三处生产构造点各有「观察之后 location 变化 → 回 19」的用例:samePlacement 见 `...SamePlacementIdentityCASFailsWhenLocationDeleted`,
常规 guard 见 `...PlacementRejectedWhenIdentityChangedSinceObservation`(present)与 `...FirstPlacementRejectedWhenLocationAppearedSinceObservation`
(missing),跨区 guard 见 `...CrossZoneFirstLegRejectedWhenLocationChangedSinceObservation`。漏填 expectLoc 即红(§5.2 ③)。

- 回归:现有 `TestEnterScene_RouteEventCarriesRequestLoginGenerationAcrossSamePlacementTakeover` 必须仍通过。
- `placement_identity_test.go`:`TestDecidePlacementIdentity_Table`(逐行覆盖 §5.2 ①,与 C++ `EnterFence.*` 对照)、
  `TestPlacementVerdictNamesCoverEveryValue`。
- `placement_lua_expect_test.go`(新):直接执行 Lua,覆盖 `any` / `missing` / `present` 三种模式、ARGV[4] 缺省、不铸造版的重发幂等分支、
  铸造版重放识别仍先于原文检查。`owner_epoch_test.go` 不改。

C++ `cross_zone_test.cpp`:

- 末尾追加 `ActingLoginGeneration.NoIdentityComponentYieldsZero` / `SessionMismatchYieldsZero` / `MatchingSessionYieldsGeneration`。
- 码 25 **不另写用例**,加进现有钉死 `SmRejectedBeforeAnyPlacementWrite` 白名单的那个用例(`pinnedCodes` 数组,2026-10-16 工作区约
  :3449-3491):`pinnedCodes` 加一行 `{kSmErrPlacementSessionSuperseded, 25, "ErrPlacementSessionSuperseded"}`,注释里「这八个码」
  改为九个。同一份白名单知识只留一处(DRY)。这个文件有大量在途改动,此处只做这两处最小编辑。
- 可选:在可测宿主里验证 `RequestSceneChange` 与 `RequestTravelEnterScene` 的盖章。

### 5.8 残余

- 混版窗口(旧 SM 副本、旧 scene、无代次 login)里 d 维持现状,legacy 计数应在全部升级后趋近 0。
- 回滚到旧 SM 后留下的落后身份:再升级后到这名玩家下一次写入身份之前,d 维持现状(§3.5、§5.5)。
- R13:gate 重启后会话号复用,SM 与 scene 一样按同会话放行(已登记)。跨实例同号时 d1 仍成立(§1.4)。
- 【未核实】B 的入场会不会中止 A 在途的同 zone 交接冻结,这关系到 d2 跨节点变体在修复前的实际危害;修复后 A 的交接请求会先拿 25。

## 6. 余项 e:断线 / 登出上报带 gate 实例号

### 6.1 问题与时序

现状:

- gate 早已把实例号带给 login:`BuildSessionDetails` 填 `gate_instance_id = node_uuid`,放在 metadata `x-session-detail-bin` 里
  (cpp/nodes/gate/handler/rpc/client_message_processor.cpp)。
- login 没往下传:
  - Disconnect(disconnectlogic.go:26-57)→ `markPlayerSessionDisconnecting`(session_lifecycle.go:56,4 次退避重试)
    → `SetSessionDisconnecting`(session_manager.go:116),只发 {player_id, session_id, ttl}。
  - LeaveGame → `DeleteSession`(:157),本地预检只比 session_id。
  - 绑定失败补偿 `compensateFenceBind`(entergamelogic.go:886)用 WithoutCancel 加 10s 预算调同两个函数。
- player_locator 侧:`SetDisconnecting`(setdisconnectinglogic.go:31)只比 session_id(:48-53);`MarkOffline`(markofflinelogic.go:39)
  比 session_id + version(:65)。
- 精确的只有 login 会话栅栏、LeaseExpired、对账三处,它们都带实例号。

「三道守卫已精确」(fence §7)只在一个前提下成立:同一个 session_id 不会同时出现在两个仍能发消息的 gate 实例上、并指向同一名玩家。
反例需要三个条件同时成立:

1. 新旧实例同号:旧实例 I1 与新实例 I2 共用 node 3,而 I1 还能发帧。来源有三种:
   - 进程冻结或分区后恢复的僵尸 gate;
   - 冲突收尾:I1 发现 node_id 已被 I2 持有,`disconnectAllClients` 在 I2 已经服务时逐个代发 ExitGame 与 Disconnect,最长 15s;
   - 跨 zone:两个 zone 各有一台 node 3,会话键放在全服共享的 Redis。
2. P 在 I2 上拿到与旧会话相同的 17 位序号(I2 的序号从 1 开始)。
3. P 在 I2 上的新登录已提交,I1 的上报才到。

player_locator 链(最现实的一条):

- I1 迟到的 Disconnect(P, 3:k) → `SetDisconnecting(P, 3:k)`。记录是 {3:k, I2, ONLINE},会话号相等,被标成 DISCONNECTING 并挂租约。
- 30s 后 LeaseMonitor 向 I2 发 LeaseExpired(target_instance_id 取自记录,= I2)。I2 上会话与玩家都对得上,forceClose P 的活连接,
  同时 LeaveScene 删掉 location。**活玩家在 30s 后被踢。**
- 迟到的 LeaveGame 走 DeleteSession / MarkOffline,后果更重:立即删掉新会话并 LeaveScene。
- 迟到窗口的来源:login 4 次重试(约 1.4s 加 RPC)、`compensateFenceBind` 最长 10s、冲突收尾最长 15s、跨 zone 重定向「先连新、后关旧」。

量级(未实测):条件 2 对单名玩家约为 1/131072,罕见但不是「不会发生」。后果只伤活性(退出前先存盘),不回档。

scene 链也有同类反例:I1 的 ExitGame(3:k, P) 通过三道守卫,销毁活实体。这一面方案 A 不覆盖,见 §6.8。

### 6.2 选定设计(方案 A,只改 Go)

- login 把 `SessionDetails.gate_instance_id` 沿三条路径传下去:
  - Disconnect:`sessionDetails.GateInstanceId`;
  - LeaveGame:`sessionDetails.GateInstanceId`;
  - `compensateFenceBind`:`state.gateInstanceID`。

  `sessionPlayersToSettle` 里的已绑定玩家来自同一个栅栏键,键本身包含实例号,用同一个实例号是对的。
- `DeleteSession` 的本地预检同样比较实例号:两边都非空且不等就不调 MarkOffline。
- player_locator:
  - `SetDisconnecting` 在 session_id 比对之后、状态判断(:64-71)之前加一条:请求与记录的实例号都非空且不等 → Info 日志后忽略。
  - `MarkOffline` 在 session_id / version 比对处加同样的条件,判为陈旧并忽略,日志带两边的实例号。
  - 判定都在 GET 到的同一份字节上做,随后的整字节 CAS 不变,比对与提交之间没有窗口。被忽略的 MarkOffline 也不会调 LeaveScene。
- gate、scene 不改。
- 风险核对:login 写会话记录用的实例号与 gate 上报用的 `SessionDetails.gate_instance_id` 同源(node_uuid,进程启动时生成一次);
  新实例上**已提交**的新会话会经 SetSession / Reconnect 把记录改成新实例。所以被忽略的都是跨实例的上报,不会误伤记录所指连接的上报。
  v0.1 写的「合法上报不可能带与记录不同的实例号」说得太满(v0.2 改),有一种情形会把清理推迟:
  1. P 在 I1 上,记录 {3:k, I1} ONLINE;
  2. I1 整机崩溃,没发出 Disconnect;
  3. I2 以 node 3 启动,P 重连拿到同号 3:k,EnterGame 链在 SetSession 之前失败(例如预加载失败),记录仍是 {3:k, I1};
  4. P 断开,`Disconnect(3:k, I2)` 因实例号不同被忽略。
  改动前这条 Disconnect 会给残留记录挂上 30s 租约;改动后只能等 `session_reconciler` 发现 I1 不在 etcd:要连续两轮、每轮默认 60s,
  再加 30s 租约,清理时延从约 30s 变成约 2.5–3.5 分钟;对账被禁用(interval < 0)时不清理。这不是误伤活玩家,
  但把清理依赖转移到了对账器上,登记在 §6.8。被忽略时打一行带两边实例号的 Info,并计数
  (`player_locator_stale_gate_instance_total{op}`,`op ∈ set_disconnecting | mark_offline`)。

### 6.3 契约改动

`proto/player_locator/player_locator.proto`(两个消息都没有 reserved 记录,4 号是新号):

```proto
message SetDisconnectingRequest {
  uint64 player_id = 1;
  uint32 session_id = 2;
  uint32 lease_ttl_seconds = 3;  // Default 30s
  // 发起断线的 gate 实例 uuid(SessionDetails.gate_instance_id)。请求与会话记录的实例号都非空且不等时,
  // 判为旧实例迟到的上报,忽略;空 = 旧版 login,不校验。
  string gate_instance_id = 4;
}

message MarkOfflineRequest {
  uint64 player_id = 1;
  uint32 expected_session_id = 2;
  uint32 expected_session_version = 3;
  // 语义同 SetDisconnectingRequest.gate_instance_id。
  string expected_gate_instance_id = 4;
}
```

- 不改 Lua(`setDisconnectingScript`、`deleteSessionIfUnchanged` 仍按 GET 到的完整字节做 CAS),不改 Redis 键值
  (`PlayerSession.gate_instance_id = 4` 已存在),不改 RPC 签名。
- login 内部签名(都加最后一个参数 `gateInstanceID string`):
  - `sessionmanager.SetSessionDisconnecting(ctx, plClient, playerID, sessionID, gateInstanceID)`
  - `sessionmanager.DeleteSession(ctx, plClient, playerID, expectedSessionID, expectedGateInstanceID)`
  - `deletePlayerSession(ctx, svcCtx, playerID, sessionID, gateInstanceID)`
  - `markPlayerSessionDisconnecting(ctx, svcCtx, playerID, sessionID, gateInstanceID)`
- 不涉及客户端契约与 tip 码。

### 6.4 逐文件改动点(全部属 W1)

1. `proto/player_locator/player_locator.proto`:见 §6.3。
2. `go/login/internal/logic/pkg/sessionmanager/session_manager.go`:`SetSessionDisconnecting` 写入请求;`DeleteSession` 本地预检加实例号,
   请求填 `ExpectedGateInstanceId`。
3. `go/login/internal/logic/clientplayerlogin/session_lifecycle.go`:两个 helper 加参数透传;CRITICAL 日志带上实例号。
4. 调用点(现有三处,已 grep 核对;加上 a 新增的一处共四处):
   - `disconnectlogic.go:53` 传 `sessionDetails.GateInstanceId`;
   - `leavegamelogic.go:52` 传 `sessionDetails.GateInstanceId`;
   - `entergamelogic.go:890 / :893`(`compensateFenceBind`)传 `state.gateInstanceID`;
   - a 的复读确认补挂租约(§2.2 ②,`persistEnterGameSession`)传 `state.gateInstanceID`。
5. `go/player_locator/internal/logic/setdisconnectinglogic.go`、`markofflinelogic.go`:按 §6.2 判定;新日志英文,
   例如 `SetDisconnecting: stale gate instance player=%d session=%d record_instance=%s request_instance=%s, ignoring`;
   计数见 §6.2(指标注册放在 player_locator 现有的 metrics 位置,落码时按该服务现有做法定)。

### 6.5 新旧并存与回滚

- 升级顺序(v0.2 改为硬约束):**先 player_locator,后 login**。
  - 新 locator + 旧 login:字段为空,不校验,退回现状;
  - 记录的实例号为空(早期记录或 dev):不校验;只有两边都非空且不等才忽略。
  - 旧 locator + 新 login:**不能只看 e 本身**。e 的字段被旧 locator 忽略,单看 e 是退回现状;但同一个 login 二进制里有 a 的跨实例同号踢线
    (§2.2 ⑤ 第 4 行),它主动让旧实例发出 `Disconnect(3:k, I1)`,旧 locator 只比会话号,会给新会话挂租约、30s 后踢掉活玩家(§2.5)。
    v0.1 写的「四种组合都退回现状」不成立,这一组合必须避免。
- 回滚:要回滚 player_locator 的 e,先回滚 login;login 单独回滚只是退回旧行为。不涉及数据迁移,不进存盘。

### 6.6 不变量对照

| 不变量 | 结论 |
|---|---|
| 1 入口栅栏第一步且零副作用 | 不改 scene |
| 2 同一会话一律放行 | 不涉及入场;scene 侧「同号即同会话」的口径不变(这也是不做方案 B 的原因,§10) |
| 3 SessionMap 只擦本玩家的映射 | 不涉及 |
| 4 gate `loginGeneration` 写入规则 | 不改 gate |

### 6.7 测试(先复现,再修复)

`go/player_locator/internal/logic/gate_instance_fence_test.go`(新,复用 `newTestSvcCtx`,不改 `logic_test.go`):

| 用例 | 断言 | 旧代码上 |
|---|---|---|
| `TestSetDisconnecting_StaleGateInstanceIgnored` | 记录 {player 1, session 7, 实例 "gate-I2", ONLINE};请求 {1, 7, "gate-I1"} → 状态仍 ONLINE、版本不变、租约 ZSET 无成员 | 红(proto 生成后) |
| `TestSetDisconnecting_EmptyGateInstanceProceeds` | 请求为空,或记录为空 → 照常进入 DISCONNECTING | 绿(兼容) |
| `TestSetDisconnecting_SameGateInstanceProceeds` | 同实例 → 照常 | 绿 |
| `TestMarkOffline_StaleGateInstanceIgnored` | 预先种一条 location(错误放行时 `SceneManagerClient` 为 nil 会走入队);断言会话逐字节保留、租约 ready 队列无成员 | 红 |
| `TestMarkOffline_EmptyExpectedInstanceProceeds` | 照常删除 | 绿 |

本文件不自建 `SceneManagerClient` 假实现(全包唯一的一份由 W2 在 `scene_manager_client_fake_test.go` 提供,§3.7),而是用「会话保留 +
无入队」间接断言没有走到 LeaveScene;辅助标识符加 `gateInst` 前缀。这样 W1 不依赖 W2 的进度,两组的测试同包编译也不撞名。

login 侧另补:`entergame_kick_test.go` 的 `TestEnterGame_ProbeConfirmedCommitPlacesDisconnectLease` 同时断言 SetDisconnecting 请求带本链实例号(§2.7)。

go/login:

- `session_fence_wiring_test.go`:`fenceLocator` 记录 `SetDisconnectingRequest` 与 `MarkOfflineRequest`;断言 Disconnect、LeaveGame、
  `compensateFenceBind` 三条路径的实例号都等于 SessionDetails 或 state 里的值;另测 `sessionPlayersToSettle` 的已绑定玩家也带上同一实例号。
- `pkg/sessionmanager/session_manager_disconnect_test.go`:改调用(:78、:105),新增 `TestDeleteSession_StaleGateInstanceSkipsMarkOffline`
  (记录 "gate-I2"、期望 "gate-I1" 时不调 MarkOffline)。

### 6.8 残余

- scene 链(同号跨实例的 ExitGame 销毁活实体)、迟到的 PlayerEnterGameNode、旧客户端消息三个面仍开着(只伤活性)。
  - 方案 B(ExitGame 带代次)只堵其中一面,而且与入场栅栏「同号即同会话」的口径不一致,不做。
  - 方案 C(scene 按入站连接核对 gate 实例,把「同一会话」改为 (gate 实例, session_id))需要负责人拍板不变量 2 的措辞,另立项。
  - **a 的跨实例同号踢线会主动触发 ExitGame 这一面**(v0.2 补):I1 收到踢线、关闭旧连接时立刻向 scene 发 `ExitGame(3:k, P)`,
    晚于新会话的 `PlayerEnterGameNode` 到达就会让新实体退出。现状里这条反例要等旧连接自己关闭才触发;第 4 行把它提前到
    「新登录提交之后」、且每次跨实例同号顶号都会触发。取舍(保留踢线 / 方案 C 落地前只计数不踢)见 §2.2 ⑤ 与 §12 待拍板 1。
- **gate 崩溃后同号重连失败的残留记录**(v0.2 补):清理改由 `session_reconciler` 兜底,时延从约 30s 变成约 2.5–3.5 分钟,
  对账被禁用时不清理(§6.2)。
- 同一 gate 进程内 17 位序号回绕(R14):迟到窗口是秒级,而回绕要攒满 13 万次连接,实际不可达。
- 【未核实】scene 在 gate 节点从 etcd 摘除时是否关闭来自它的入站连接(决定 scene 链反例的窗口)。
- 【未核实】路由模式下 gate → 路由服 → login 的 Disconnect 截止时间(决定 login 侧迟到窗口的上界)。

## 7. 实施分工

三组的文件集互不相交,跨组只通过 proto 契约(以及 §5.2 ⑤ 手抄的私有码 25)耦合。但「文件不相交」不等于「编译单元不相交」:
Go 同一个包里的测试文件共享一个命名空间,W1、W2 在 `go/player_locator/internal/logic` 包里都有新测试文件,约定见「组间关系」。
各组动每个文件前先 `git diff HEAD -- <file>` 看清别人的在途 hunk,开工时与提交前各做一次快照比对;改已存在的文件只做最小编辑,
不回退、不重排、不格式化别人的行。生成产物不手改。下表「在途改动」一列按 2026-10-16 v0.2 修订时的 `git status` 重新核对过。文档收尾(
`docs/PROGRESS.md`、`cross-zone-scene-travel.md`、`shared-redis-split-by-purpose.md`、fence 正文状态、Java `PARITY.md`)由主线在复核通过后统一做,不归任何一组。

### W1:a + e(login / player_locator 上报 / gate)

| 文件 | 改动 | 别的会话在途改动 |
|---|---|---|
| `proto/contracts/kafka/gate_event.proto` | `KickPlayerEvent` 2 / 3;`RoutePlayerEvent.login_generation` 注释 | 无 |
| `proto/player_locator/player_locator.proto` | `SetDisconnectingRequest` 4、`MarkOfflineRequest` 4 | 无 |
| `go/login/internal/svc/gate_command.go` | Kick 走 payload | **有**(Bind 注释 2 行改写成新入口说法,不碰) |
| `go/login/internal/svc/servicecontext.go` | `KickSessionOnGate` 加参数 | **有**(`initLoginQueue`,不碰) |
| `go/login/internal/svc/gate_command_test.go` | 改调用、新增 payload 用例 | 无 |
| `go/login/internal/logic/clientplayerlogin/entergamelogic.go` | 踢线挪位、复读消歧与补挂租约、kicker 接缝、`compensateFenceBind` 透传实例号 | 无 |
| `.../clientplayerlogin/session_lifecycle.go`、`disconnectlogic.go`、`leavegamelogic.go` | 实例号透传 | 无 |
| `.../clientplayerlogin/metrics.go` | `login_kick_total` | 无 |
| `.../clientplayerlogin/entergame_kick_test.go`(新,自带 harness)、`session_fence_wiring_test.go` | §2.7、§6.7。`player_identity_backfill_test.go` 不改 | 无 |
| `go/login/internal/logic/pkg/sessionmanager/session_manager.go`、`session_manager_disconnect_test.go` | 实例号 | 无 |
| `go/player_locator/internal/logic/setdisconnectinglogic.go`、`markofflinelogic.go` | 实例号判定与计数 | 无 |
| `go/player_locator/internal/logic/gate_instance_fence_test.go`(新) | §6.7,不自建 SM 假 client | — |
| `cpp/libs/services/gate/session/system/session_identity_fence.h` | `ClassifyKick` | 无 |
| `cpp/libs/services/gate/session/comp/session_info_comp.h` | 注释 | **有**(私有成员 `entityIds` 改名 `nodeEntities`,不碰) |
| `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | Kick 守护段 | **有**(入口生成器重构删了 Register / UnRegister 与空 handler;只改守护段) |
| `cpp/tests/routing_identity_test/routing_identity_test.cpp` | `KickFence.*`(末尾追加) | **有**(16 行) |
| `tools/proto_generator/protogen/internal/generator/cpp/gate_kafka_router_automap_test.go`(新) | 同名同型映射用例(§2.7) | — |
| `tools/proto_generator/protogen/internal/generator/cpp/gate_kafka_command_router_test.go` | **不改** | **有**(入口生成器重构新增 `TestKafkaIngressDispatchCallsHandlerDirectly` 等;v0.1 误标为「无」) |

W1 的 gate 部分(`gate_event_handler.cpp` 守护段、生成器新用例)有一个前置门禁:Kafka 入口生成器重构(`gate_kafka_command_router.go` /
`.tmpl`、`event_handler.*.tmpl`、`event.go`、新增 `scene_kafka_command_router.*`)的归属方已完成,且它自己的生成器测试是绿的。
复核期间这组文件仍在被并发写入;门禁不满足时只暂停 W1 的 gate 部分,login 与 player_locator 部分照常做。

### W2:b / c / d 的 Go(scene_manager 与 locator 离场)

| 文件 | 改动 | 别的会话在途改动 |
|---|---|---|
| `proto/scene_manager/storage.proto` | `PlayerLocation` 8 / 9;GO-3 注释改为从 10 起 | 无 |
| `proto/scene_manager/scene_manager_service.proto` | `LeaveSceneRequest` 5 / 6;`EnterSceneRequest` 12 注释 | 无 |
| `proto/scene_manager/scene_node_service.proto` | `ReleasePlayerRequest` 4 + rpc 注释 | **有**(`AssetAckReceipt` 两行,不碰) |
| `go/scene_manager/internal/logic/placement_identity.go`(新) | 身份类型与两张判定表 | — |
| `go/scene_manager/internal/logic/enterscenelogic.go` | d 判定与身份写、guard、c 派发后移 | 无 |
| `.../logic/changesceneutil.go`、`owner_epoch.go`、`leavescenelogic.go`、`scene_node_client.go` | 见 §3.4、§4.4、§5.4 | 无 |
| `go/scene_manager/internal/constants/errors.go` | 码 25 | 无 |
| `go/scene_manager/internal/metrics/metrics.go` | `placement_identity_total`、reason Help | 无 |
| 测试:`placement_identity_test.go`、`enterscene_identity_test.go`、`leavescene_identity_test.go`、`placement_lua_expect_test.go`、`release_dispatch_test.go`(均新)、`integration_test.go` | §3.7、§4.7、§5.7 | 无 |
| `go/player_locator/internal/logic/leasemonitor.go` | `LeaveSceneRequest` 填身份 | 无 |
| `go/player_locator/internal/logic/reconnectlogic.go` | 重放分支补比代次(§3.4 第 5 条) | 无 |
| `go/player_locator/internal/logic/scene_manager_client_fake_test.go`(新) | 全包唯一的 `smpb.SceneManagerClient` 假实现(§3.7) | — |
| `go/player_locator/internal/logic/leave_scene_identity_test.go`、`reconnect_replay_generation_test.go`(均新) | §3.7 | — |

### W3:b / c / d 的 C++ scene

| 文件 | 改动 | 别的会话在途改动 |
|---|---|---|
| `cpp/libs/services/scene/player/system/enter_fence.h` | `ActingLoginGeneration` | 无 |
| `cpp/libs/services/scene/player/system/release_fence.h`(新) | §4.3 | — |
| `cpp/libs/services/scene/player/system/player_lifecycle.h` | 码 25、`SmRejectedBeforeAnyPlacementWrite`、`HandleReleaseFromSceneManager` 声明 | **有**(资产提交存盘入口,不碰) |
| `cpp/libs/services/scene/player/system/player_lifecycle.cpp` | 三处盖章、疏散票据、`HandleReleaseFromSceneManager`、B5 注释 | **有**(存盘序号 / 资产提交,不碰) |
| `cpp/nodes/scene/handler/grpc/scene_node_service.cpp` | `HandleReleasePlayer` 守护段委托 | **有**(AssetAckReceipt 骨架,不碰) |
| `cpp/libs/services/scene/core/system/redis.cpp` | `[ReleaseFence]` 汇总行 | 无 |
| `cpp/libs/services/scene/scene.vcxproj`、`.filters` | ClInclude `release_fence.h` | **有**(各删 3 行 `*_events.h`,不碰) |
| `cpp/tests/cross_zone_test/cross_zone_test.cpp` | `ReleaseFence*`、`ActingLoginGeneration.*`(末尾追加);码 25 加进现有 `pinnedCodes` 并改注释计数(§5.7) | **有**(约 570 行,含换行符差异) |

W3 的前置条件(v0.2 补):接手记录 README §4 写明「全域到账续批」写入方(不在 Claude Desktop 会话列表里,疑为 Codex 或 CLI)截至
2026-10-15 23:05 仍在改 `player_lifecycle.cpp`,并要求围栏代码合并等它停手。W3 开工前须确认该写入方已停手,或已被告知
`player_lifecycle.cpp/.h` 里本线要动的区段(`IsHandoffRequested` 旁的新函数、三处发送点、疏散票据、码表)归本线;开工时与提交前
各做一次 `git diff HEAD -- <file>` 快照比对,不一致就停下报告。否则一方整文件写回会抹掉另一方的改动,而两边都要等 Codex 编译才会发现。
README §4 的同步由主线做(本文只改设计稿)。

### 组间关系

- W1 与 W2 都改 proto,彼此不同文件;两组都改完后才做一次生成(§8)。
- W2 与 W3:靠 `ReleasePlayerRequest.expected_owner_epoch`、`EnterSceneRequest.login_generation` 的新语义、私有码 25 三点耦合。
  W3 的编译依赖生成后的 `expected_owner_epoch()` 访问器。
- W1 的 `markofflinelogic.go`(e 判定)排在 W2 的 `leasemonitor.go`(b 填身份)之前执行,行为上串联、文件不重叠。
  W2 的 MarkOffline 用例要让实例号一致或留空。
- `go/player_locator/internal/logic` 包内的测试命名约定:`smpb.SceneManagerClient` 假实现全包只有一份,由 W2 放在
  `scene_manager_client_fake_test.go`;W1 不自建、也不依赖它(§6.7)。各组新增的测试辅助标识符加前缀:W1 用 `gateInst`,
  W2 用 `leaveIdent` / `reconnectReplay`。否则两组并行各写一个 `fakeSceneManagerClient`,整个包编译失败,两组用例都跑不起来。
- W2 的 `reconnectlogic.go` 与 W1 的 `setdisconnectinglogic.go` / `markofflinelogic.go` 同包不同文件,互不调用。
- b 与 d 在 W2 内同批;c 与 d 同改 `enterscenelogic.go`,同组同一执行者顺序落,c 只移动派发、不碰 Lua。

## 8. proto 生成与给 Codex 的验证顺序

**前置门禁**(任一不满足就不开跑,报告等待):

- fence 批次本身仍「未编译、未测试」(fence 状态行)。b / d 的验证依赖它,应排在它验证通过之后,或与它同一次全量编译里一起验证。
- 工作区里 `proto/` 下约 21 个 proto 带别的会话的在途改动(trade、guild、match、`message_id.txt` 等),生成器模板与 `id_file.go` 也在途。
  `proto-gen-run` 是全量生成,会把这些半成品一起写进各语言产物。开跑前须由各归属方确认它们的在途 proto「可生成」;
  `AssetAckReceipt`(`scene_node_service.proto`)的合入顺序同样与其归属方确认。
- Kafka 入口生成器重构(§7 W1 表下的门禁)已完成,且归属方确认它自己的生成器测试是绿的。

**生成基线**(v0.2 新增;在 W1 / W2 改任何 proto **之前**,在仓库根执行一次):

1. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`。生成器源码有别的会话的在途改动,必须先构建;构建失败即停,保留输出。
2. `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   不用 `dev.bat proto` 与默认配置,不写客户端仓。
3. 对 `proto_gen.no-unity.yaml` 配置的全部输出目录逐文件算哈希,清单存到仓库外的临时目录,作为基线。
   不用 `git diff` 当基线:工作区相对 HEAD 的差异混着别人的在途改动,分不出哪些是本批的。

**第一轮:复现**(proto、生成、接缝、测试;不含修复):

1. W1 / W2 改完 proto;各组落纯重构接缝(login 的 kicker 接缝、SM 的 `releaseDispatcher` 钩子、C++ 不带栅栏的
   `HandleReleaseFromSceneManager`)与新增参数的透传(只透传、不使用),再写全部测试。
2. 重跑上面第 1、2 步生成,与基线清单逐文件比对。期望:
   - 有变化的文件只属于 `gate_event`、`player_locator`、`storage`、`scene_manager_service`、`scene_node_service` 五个 proto 的各语言产物,
     外加 `cpp/nodes/gate/handler/event/gate_kafka_command_router.cpp`;
   - 该路由文件只在信封分支的 Kick 多出一行 `event.set_player_id(command.player_id());`,payload 分支不变(§2.4 第 9 条);
   - `message_id.txt`、事件号文件与各语言的消息号 / 事件号常量相对基线**必须无变化**(本批不新增消息、rpc 与事件);
   - 守护段原样保留。
   出现清单之外的变化即停,保留比对结果。
3. 按下面的「验证命令」编译并运行。通过标准:§2.7、§3.7、§4.7、§5.7、§6.7 表中标「红」的用例恰好失败,标「绿」的通过,
   标「—」的不作判据、只记录结果;原有用例全部通过。保存完整输出,作为 AGENTS §11.4 的红跑证据。

**第二轮:修复**:各组落修复(不再改 proto,无需重新生成);按同一组验证命令重跑,要求全部为绿。

**验证命令**(每步红了就停,保留首个失败的完整输出,不连续自动重试;第一轮里表中标「红」的预期失败不触发停止,出现预期之外的
失败或编译错误才停):

1. 生成器:`cd tools/proto_generator/protogen && go test ./internal/generator/cpp/ -run TestGateKafkaRouterAutoMaps -count=1`,
   再跑 `go test ./internal/generator/cpp/... -count=1`。后者若因不属于本批的在途用例变红,停下并报告归属,不计为本批失败。
2. Go(均设 `GOTOOLCHAIN=local`):
   - `cd go/player_locator && go test ./internal/... -count=1 && go vet ./...`
   - `cd go/scene_manager && go test ./internal/... -count=1 && go vet ./...`
   - `cd go/login && go test ./internal/svc/... ./internal/logic/... -count=1 && go vet ./...`
3. C++(串行 `/m:1`,并发会报假的 C1041 / LNK1104):
   - `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`
   - 再串行构建 `cpp/tests/routing_identity_test` 与 `cpp/tests/cross_zone_test` 两个测试工程
4. 运行:
   - `routing_identity_test.exe --gtest_filter=KickFence.*:GateIdentityFence.*:LoginGeneration.*`,然后全量;
   - `cross_zone_test.exe --gtest_filter=ReleaseFence*:ActingLoginGeneration*:EnterFence*:*Travel*:*Relocate*`,然后全量。
5. 可选联机(一区、路由模式):
   - robot 做「LeaveGame 后立即同角色重进、落在同一频道」,检查 `player:{id}:location` 仍在、`instance:{sid}:player_count` = 1;
   - 回归 `travel_smoke` 与 `channel_smoke`;
   - 观察 scene 日志的 `[ReleaseFence]`、`[EnterFence]` 与 scene_manager 的 `scene_manager_placement_identity_total`。

第二轮通过标准:原有用例数不减少,全部为绿;新增用例全部为绿。失败时保留:首个失败用例的输出、生成产物与基线的比对结果、
MSBuild 首个错误前后 50 行。

## 9. Java 版(AGENTS §12)

本文所有 proto 改动都是服务端内部消息,不涉及客户端可见契约;码 25 是 scene_manager 私有数轴,不是 tip 码。
加了字段对 Java 运行无副作用,但 Java 仓的 `tools/ContractSync.java` 同步的是本仓全部 `proto/**`(只排除 `proto/etcd/`),本仓提交后
Java 侧 `--check` 会报契约漂移。Java 仓须同批运行 `java tools/ContractSync.java --mmorpg <本仓根目录>` 并提交(Java 仓的构建与提交口径
以它自己的 AGENTS.md 为准)。

各项结论(v0.2 按 Java 仓 `PARITY.md`「顶号」行与「短线重连与落点」行改写;登记时补本仓 commit 号):

| 项 | Java 状态 | 依据与待核点 |
|---|---|---|
| a 踢线 | 不适用(机制不同)+ 1 个待核点 | Java 顶号经归属协议:login 夺权撞上仍被持有 → 请持有的 scene 让出 → 旧实例写回释放、旧连接收 23 {2017} 后断开 → 新连接进场。没有「login 按 session_id 发踢线」这一步,T3(踢到复用同号的别人)与跨实例同号踢线都不存在。待核:旧连接在让出时就被断开,而新登录的夺权在让出之后才提交;夺权若随后失败,是否也有「两台设备都掉线」的同类活性问题 |
| b 离场 / d 落点 | 不适用(结构不同)+ 1 个待核点 | Java 的位置记录 `game:location:{player_id}` 由持有归属的 scene 唯一写入,按 (epoch, 序号) 只收更新的写,LeaveGame 写登出墓碑;没有 SM 侧的 LeaveScene 条件删除,也没有 SM 侧的落点写。待核:登出墓碑与同一 epoch 内迟到写的先后(依赖序号单调,未逐行核对) |
| c 释放 | 不适用(结构上不存在) | Java 无 `ReleasePlayer` 实现与调用方(调研 c 用 git grep 查过),归属靠库行 `owner_epoch` 与「交出」原语 |
| e 断线 / 登出上报 | 待核实 | Java 没有会话登记的 DISCONNECTING 状态(PARITY 有意差异 ①),断线由 scene 写重连租约记录;旧 gate 实例迟到的断线 / 离开通知能否影响同号的新会话,未核实 |

## 10. 残余与另立项

本批不做、登记备查:

1. **e 方案 C**:scene 按入站连接核对 gate 实例(把「同一会话」改为 (gate 实例, session_id)),覆盖 ExitGame、迟到入场、旧客户端消息三面。
   需负责人拍板不变量 2 的措辞,并解决「入口前多一道连接级判定」与「栅栏是第一步」的字面冲突。另立项。
2. **e 方案 B**(ExitGame 带登录代次):不做,理由见 §6.8。
3. **fence §9 残余 7**:外来角色的迟到路由经 `ApplyRouteIdentity` 无条件覆盖会话的 homeZoneId / ownerEpoch,`OnRouteDecision` 还会替换并
   随后丢掉本角色在途的欠账。两种修法待拍板:7-A 提前丢弃外来路由,7-B 让 homeZoneId / ownerEpoch 随路由目标走。
   先看 `[SceneEntry] player_changed` 计数再定。判据与 a 的 `ClassifyKick`、LeaseExpired 的内联核对同源,做时收成一个纯函数。另立项。
4. **6b「同 scene_id 换节点多算一人」**:同 scene_id、换了节点、不是死节点接管的落点,enterscenelogic.go:757-759 只在 scene 变化时
   扣旧场景人数,会多算 1。b 修好后显形。另立项。
5. **a 的 T5**:换角在新 Bind 之前(§2.8)。另立项。
6. **a 结果未知且已提交后断线**:v0.2 起 ReplaceLogin 复读确认已提交时补挂断线租约,本批完成(§2.2 ②)。仍开着的是 FirstLogin /
   ShortReconnect 不复读的「结果未知但已提交」,以及复读本身失败的情形(§2.8)。另立项。
7. **gate `PushToPlayerEvent` 无玩家栅栏**(§2.8)。另立项。
8. **R13 兼容位**:scene_handler.cpp 的 `CheckRoutedPlayerIdentity` 对 player_id = 0 放行。审计要求「灰度一版后改为丢弃」,目前仍放行。另立项收口。
9. **c 方案 D**:生产口径不再发释放(§4.8),可作后续收紧。
10. **GO-3 字段号**:`PlayerLocation` 8 / 9 已占用,GO-3(cross-zone-scene-travel.md §12.3,记属主进程实例)顺延到 10 起;
    GO-3 本身仍待拍板,文档收尾时同步该处说明。
11. **本链实例号为空时提交前拒绝**(v0.2 登记,来自复核 C-F8):这类链必在 Bind 失败,却仍先提交一条实例号为空、无法寻址的会话记录。
    本批只做到「不踢」(§2.2 ⑤ 第 2 行)。改为在 `persistEnterGameSession` 之前直接失败会同时改变 FirstLogin / ShortReconnect 的失败阶段
    与计数,超出 a 的范围;另立项时一并确认 dev 旁路(gate 不透传实例号)是否依赖现状。
12. **T4 的反向顺序**(v0.2 登记):失锁时踢线早于同连接重登的新 Bind,会踢掉更新的 X2(§2.1、§2.8)。gate 侧修不了,只伤活性。登记备查。
13. **`keep_newer` 漏删的告警阈值**(v0.2 登记):§3.2 第 3 条要求按速率告警,阈值要在上线后看 `scene_manager_placement_identity_total{site="leave",verdict="keep_newer"}`
    的基线再定。
14. **e 的同号跨实例残留记录改由对账器兜底**(v0.2 登记,§6.2、§6.8):对账被禁用的环境不清理;是否给 Disconnect 被忽略的情形补一条
    更快的清理路径,另议。

仍未核实(落码与复核时留意):

- a:Java 版踢线时序;「同一 gate 上 Bind 先于 Kick」依赖被替换链在 Bind 之后才释放锁、同步生产者确认才返回,没有运行验证。
- b:实际发生频率(现在没有任何指标)。
- c:A1′ 写标记后退出能否被取消;ReleasePlayer 重试的实际频率。
- d:B 的入场是否中止 A 在途的同 zone 交接冻结。
- e:scene 摘除 gate 节点时是否关闭入站连接;路由模式下 Disconnect 的截止时间;客户端跨区「先连新后关旧」的具体时序;
  GameNodeExitGameRequest 1 号字段的全历史使用情况(只在做方案 B 时才需要)。
- Java:a 的让出先于夺权提交是否有同类活性问题;b / d 的墓碑与迟到写顺序;e 的对应形态(§9)。

## 11. 修订历史

| 版本 | 日期 | 内容 |
|---|---|---|
| v0.1 | 2026-10-16 | 初稿:按接手记录 §3.3 的选定做法汇总四份调研,抽查关键事实。补正了一处调研写「未核实」的点:非 0 码在交接应答里一律走 `ResolveTravelOutcome(kFailed)`。补充了 a 的「实例号不明按同一连接处理」(防自踢)、落点 Lua 的 ARGV[4] 缺省按 `any`(保住现有直接 EVAL 的用例)、疏散票据带代次的落点位置。未改代码 |
| v0.2 | 2026-10-16 | 按「正确性」(C-F1–F8)与「发布与可实施性」(R-F1–F14)两路对抗复核修订,逐条处置见 §12。主要变化:①发布 / 回滚总顺序改为硬约束(§0),player_locator 先于 scene_manager 与 login;②b 写明 `keep_newer` 不漏删的前提,Reconnect 重放分支补比代次,`keep_conflict` 改用「会话有效」口径;③d 的 legacy 行不降级已有身份,samePlacement 推路由失败不回滚身份,`expectLoc` 零值语义写明并由行为用例守住三处生产构造点;④a 复读确认已提交时补挂断线租约,本链实例号为空一律不踢,T4 反向顺序登记为残余,§1.4 如实写明 R13 下 d1 仍成立;⑤e 的风险核对改为如实描述对账器兜底;⑥§7 按工作区重新核对在途改动,生成器用例改放新文件,player_locator 包内测试约定唯一假实现与前缀,W3 加「全域到账续批」写入方停手的前置条件;⑦§8 改为生成基线比对 + 两轮 Codex 运行(先复现后修复);⑧§9 Java 结论按 PARITY.md 改写并补 ContractSync 同步要求。未改代码,未编译、未测试 |

## 12. 复核意见处置表

两路复核(2026-10-16,只读)共 22 条。核实方式:逐条读当前工作区代码,或核对 git status / 调研报告 / Java 仓 PARITY.md。
「采纳」= 意见成立且按建议或等价方式改了文档;「部分采纳」= 问题成立,改法与建议不同;「不采纳」= 意见不成立或不改,写明理由。

### 12.1 正确性复核(C)

| 编号 | 意见 | 核实 | 处置 | 文档改动 |
|---|---|---|---|---|
| C-F1 | `keep_newer` 在「记录代次 < 本会话写进 location 的代次」时永久漏删;§3.5 只分析了误删 | 成立。`entergamelogic.go` 的 ShortReconnect 分支对应答代次不符只打 WARN、下游用铸出值;`reconnectlogic.go` 重放分支只比 State / SessionId / RequestId。补充核对:SetSession 的重放用 `proto.Equal` 整条比较,不受影响 | 采纳 | §0 总顺序;§3.2 写明前提、两处破坏路径与处置;§3.4 第 5 条(重放比代次);§3.5 删「任一组件单独回滚,顺序不限」;§3.7 新增重放用例;§3.8 漏删残余与速率告警;§10 第 13 条。重放代次不等时选「拒绝」而非「抬到较大值」:同一幂等键配不同内容按通行做法拒绝,代价(同会话收 3023)已写明 |
| C-F2 | 跨实例同号踢线依赖 e 在 locator 生效,文中却写「没有硬依赖」;还会提前触发 scene 侧 ExitGame | 成立。`setdisconnectinglogic.go` 只比 session_id;`disconnectlogic.go` 按栅栏绑定的玩家调 SetDisconnecting | 采纳(选硬顺序,不加开关) | §0、§2.2 ⑤ 第 4 行前提与已知代价、§2.5、§6.5、§6.8。踢线留不留见待拍板 1 |
| C-F3 | samePlacement 改身份后推路由「报错但已投递」时,keep 回滚会重开 b1 / d2 | 成立。enterscenelogic.go 第 7 步注释承认 kafka-go 报错但已投递;7 不在 `SmRejectedBeforeAnyPlacementWrite` 白名单 | 采纳 | §5.2 ② 改为不回滚身份;§5.4;§5.7 用例改名为 `...RouteFailureKeepsIdentity` |
| C-F4 | 复读确认已提交后链按失败收尾,对 Unity 客户端稳定形成「ONLINE、无租约」 | 成立。`notifyEnterGameFailed` 注释写明 Unity 收到 3023 即拆连接;`compensateFenceBind(placeLease=true)` 可直接复用 | 采纳(本批做) | §0、§2.2 ②、§2.4、§2.7 两条新用例、§2.8、§6.4 调用点、§10 第 6 条 |
| C-F5 | §1.4「同号跨实例时 d 不受影响」与 d 判定表「会话号优先」矛盾 | 成立 | 采纳(如实描述;改「同一会话」定义另立项) | §1.4、§5.8 |
| C-F6 | T4 只在 X2 的 Bind 先到 gate 时挡得住,反向顺序没登记 | 成立 | 采纳 | §2.1 T4、§2.8、§10 第 12 条 |
| C-F7 | b 用「会话都非 0」,d 与 C++ 用「有效」,口径不一致 | 成立(取值基本构造不出) | 采纳 | §3.2 表与说明、§3.4 共用 `validSession`、§3.7 新用例、§5.4 |
| C-F8 | 本链实例号为空的链注定在 Bind 失败,应在提交前拒绝 | 成立(既有行为,非回归)。`buildGateCommandMessage` 守卫一拒空实例号 | 部分采纳 | §2.2 ⑤ 第 2 行改为「无论会话号一律不踢」,消除了 a 范围内的危害(先踢掉旧设备);「提交前拒绝」会同时改变 FirstLogin / ShortReconnect 的失败阶段与计数,超出 a 的范围,登记 §10 第 11 条,见待拍板 3 |

### 12.2 发布与可实施性复核(R)

| 编号 | 意见 | 核实 | 处置 | 文档改动 |
|---|---|---|---|---|
| R-F1 | gate Kafka 入口生成器正被别的会话重构,W1 的测试文件在途,§8 的生成预期过时 | 成立。`gate_kafka_command_router_test.go` 为 M,已有 `TestKafkaIngressDispatchCallsHandlerDirectly`;工作区的 `gate_kafka_command_router.cpp` 已按新模板重生 | 采纳 | §2.4 第 7、9 条;§2.7 生成器用例改放新文件 `gate_kafka_router_automap_test.go`;§7 改标在途并加 gate 部分的前置门禁;§8 前置门禁与按基线比对的期望 |
| R-F2 | 21 个 proto 有在途改动,「差异只落在 5 个 proto」不成立 | 成立(`git status` 核对) | 采纳 | §8 生成基线(改 proto 之前全量生成一次、逐文件哈希)+ 前置门禁 + 消息号 / 事件号必须无变化 |
| R-F3 | 跨实例踢线与 e 之间有没写出的硬性升级顺序 | 成立(同 C-F2) | 采纳;总顺序调整为 scene → player_locator → scene_manager → gate → login | 意见给的顺序是 scene → SM → locator → gate → login;为同时满足 C-F1(locator 先于 SM),把 locator 提到 SM 之前,两条约束都满足。「只计数不踢」作为待拍板 1 的备选列出 |
| R-F4 | d 的 legacy 放行把 location 身份降成 (sid_R, 0),全部升级后仍可能重开 b1 / d | 成立 | 部分采纳 | §5.2 ① legacy 行改为「gen_L ≠ 0 时保持 L」;没有按建议用 `correlation_id` 区分 scene 自发与 login 发起:逐一推演后两类请求保持 L 与写 (sid_R, 0) 在后续判定上结论相同或更严,规则更简单(理由写在 §5.2 ①);§5.5、§5.7 新用例 |
| R-F5 | W1、W2 在 player_locator 同一 Go 包各写假 SceneManagerClient 会撞符号 | 成立(包内现无该假实现) | 采纳 | §3.7 由 W2 建唯一假实现;§6.7 W1 改为不依赖假实现的间接断言;§7 组间约定与前缀 |
| R-F6 | W3 改 `player_lifecycle.cpp` 时「全域到账续批」写入方仍在改 | 成立(接手记录 README §4) | 采纳(README 同步留给主线) | §7 W3 前置条件;本轮只改设计稿,README §4 的同步列为主线待办 |
| R-F7 | §3.5「回滚后再升级不会误删新登录」不成立 | 成立。旧 SM 的 samePlacement 不写 location | 采纳 | §3.5 改为如实描述并并入 §3.8、§5.5、§5.8 的混版残余 |
| R-F8 | 复用 `newEnterWiringHarness` 做不到;§7 漏列文件;nil KafkaClient 会 panic | 成立。harness 的 SetSession / Reconnect 恒报错、SessionDetails 只有 SessionId;`SendBindSessionToGate` 直接用 KafkaClient。补充核对:入场链失败通知走 `enterFailureTipPusher` 接缝,且 gateID 为 "0" 时直接跳过 | 采纳(新建独立 harness,不改 `player_identity_backfill_test.go`) | §2.7 harness 要求(实例号非空、gate_node_id = 0)、§7 |
| R-F9 | `expectLoc` 零值语义没写明 | 成立(28 处测试字面量) | 采纳(守法改为行为用例) | §5.2 ③ 写明零值 = any、生产三处必须显式;§5.4;§5.7 三处构造点各有「观察后变化 → 19」用例。没有按建议加运行期计数:行为用例按接口守住同一件事,不往生产路径加只为测试服务的计数 |
| R-F10 | 「先复现后修复」可以做到,与 c 的分阶段做法不一致 | 成立 | 采纳 | §2.7 删「拿不到」;§8 两轮运行与通过标准 |
| R-F11 | 「合法上报不可能带与记录不同的实例号」太满,gate 崩溃后清理推迟到 2 分钟以上 | 成立。对账器连续两轮、默认 60s,interval < 0 禁用 | 采纳 | §6.2 如实描述并加计数;§6.8;§10 第 14 条 |
| R-F12 | 类型不匹配用例会碰到 nil `logger.Global` | 成立(`logger.Global` 默认 nil,同目录其他测试先设 `zap.NewNop()`) | 采纳 | §2.7 |
| R-F13 | Java 写「待核实」不满足 AGENTS §12;ContractSync 会报漂移 | 成立(PARITY「顶号」「短线重连与落点」两行;`ContractSync.java` 只排除 `proto/etcd/`) | 部分采纳 | §9 改写:b / d、c 判不适用,a 判不适用但留 1 个待核点(PARITY 写明旧连接在让出时就断开,让出先于新登录夺权提交,与意见说的「结构上属提交后才踢」不完全一致),e 仍待核实;补 ContractSync 同批同步 |
| R-F14 | 码 25 应加进现有钉死白名单的用例 | 成立(`pinnedCodes`,当前约 :3449-3491,注释「这八个码」) | 采纳 | §5.7、§7 W3 |

### 12.3 仍需负责人拍板

> **2026-10-16 主线按负责人「按最标准的做」的授权先行定论**(负责人可推翻):
> - 第 1 点取「方案 C 落地前只计数不踢」(`result=skip_cross_instance`):不引入新的确定性风险,行为退回现状;等方案 C 落地后再打开踢线。
> - 第 2 点另立项,本批不做。
> - 第 3 点另立项,本批不做(§10 第 11 条)。
> - 第 4 点的协调由主线向负责人报告;W3 动 `player_lifecycle.cpp/.h` 时只做区段化最小 Edit,与写入方的在途区段不重叠。

1. **a 的跨实例同号踢线(§2.2 ⑤ 第 4 行)留不留**。
   - 保留踢线:旧设备不再以同号、同玩家的身份向 scene 串话;代价是 I1 关闭旧连接时立刻发出 `ExitGame(3:k, P)`,若晚于新会话的
     `PlayerEnterGameNode` 到达会让新实体退出(§6.8)。踢线在 Bind 与 EnterScene 之前发出,多数情况下 ExitGame 先到、退掉的是旧实体,
     但没有顺序保证。
   - 方案 C 落地前只计数不踢(新增 `result=skip_cross_instance`):退回现状,ExitGame 推迟到旧连接自然关闭时才发,串话持续到那时。
   - 两种都要求 player_locator 先于 login(§0)。
2. **e 方案 C 是否立项**(§10 第 1 条):要改不变量 2 的措辞,把「同一会话」定义为 (gate 实例, session_id),同时根治 §1.4 的 R13 与第 1 点的 ExitGame 面。
3. **C-F8「本链实例号为空时提交前拒绝」本批做还是另立项**(§10 第 11 条)。本稿按另立项写。
4. **协调事项(需负责人出面)**:
   - 告知「全域到账续批」写入方 `player_lifecycle.cpp/.h` 中本线要动的区段,或确认其已停手(§7 W3);同步接手记录 README §4。
   - Kafka 入口生成器重构的归属方确认完成并给出生成器测试绿的结果(§7、§8)。
   - `proto/` 在途改动的各归属方确认「可生成」(§8)。

---

## 13. 落码记录(2026-10-16,全部未编译、未测试)

按 §7 三组并行落码,每组「实现 → 只读复核 → 修复」各一轮。三组复核都没有发现正确性或数据一致性问题;下表只列与设计正文的出入和留给后续的项。给 Codex 的命令汇总在 [codex-verification.md](../handoff/landing-fencing-takeover-20261016/codex-verification.md)。

### 13.1 W1(a + e)

- 落地:`gate_event.proto` 2/3、`player_locator.proto` 两个 4 号字段;login 提交后踢线、复读消歧(独立 2s ctx)、复读确认已提交时经 `compensateFenceBind(placeLease=true)` 补挂租约(10s 预算,不与复读共用 ctx)、`login_kick_total{result}`(`sent / send_failed / skip_same_session / skip_cross_instance / skip_no_target / skip_uncommitted`);gate `ClassifyKick`(守护段内);player_locator `player_locator_stale_gate_instance_total{op}`。
- 与正文的出入(以代码为准,正文 §2 / §6 相应条目按此理解):
  - **§6.2 / §6.4**:login 侧**不做**实例号本地预检(复核 W1-R1)。`DeleteSession` 只保留会话号预检,`ExpectedGateInstanceId` 原样随 `MarkOffline` 发出,由 player_locator 在同一份字节上判定、打 Info、计 `op=mark_offline`——同一判定只留一处权威,代价是罕见的跨实例同号多一次 RPC。§6.7 的 login 用例相应改为 `TestDeleteSession_LeavesGateInstanceCheckToLocator`、`TestLeaveGame_CrossInstanceReportLeftToLocator`。
  - **§2.7**:`TestEnterGame_SameSessionDifferentInstanceKicks` 按 §12.3 定论改名 `…SkipsCrossInstance`(踢 0 次、`skip_cross_instance` +1);`TestEnterGame_SameSessionSameInstanceDoesNotKick` 加了 `skip_same_session` 计数断言,在旧代码上是「只差计数的红」。
  - 踢线的 `player_id` 取本链 `state.playerID`(与会话记录恒等;避免旧记录缺字段时 gate 退化成不核对玩家的旧式踢线)。
  - player_locator 原先没有业务指标文件,计数器与纯函数 `isStaleGateInstanceReport` 放在 `setdisconnectinglogic.go`,`markofflinelogic.go` 共用。
- §8 第一轮红跑:修复与接缝已一起落在工作区,第一轮用 W1 交付的 `w1_redrun.py` 机械切换 3 个文件、跑完按 sha256 逐字节恢复(见命令汇总)。

### 13.2 W2(b / c / d 的 Go 侧)

- 落地:`storage.proto` 8/9(GO-3 改从 10 起)、`LeaveSceneRequest` 5/6、`ReleasePlayerRequest` 4;`placement_identity.go` 两张判定表与 `validSession`;落点 Lua 原文期望(共用片段 `luaLocationExpectCheck`);samePlacement 只改身份、推路由失败不回滚;`ReleasePlayer` 派发挪到第 7 步之后(7b)经 `releaseDispatcher`;LeaveScene 按代次 keep / 删除;码 25;`scene_manager_placement_identity_total{site,verdict}`;Reconnect 重放补「代次相等」;player_locator 全包唯一的假 `SceneManagerClient`。
- 与正文的出入:
  - Lua 原文期望对**未知模式 fail-closed**(回 0,翻成 19);除缺省 / 空串外也接受字面 `any`。
  - samePlacement 写身份遇 Redis 错误回 3(`ErrUpdateLocation`,与第 6 步落点出错同口径,不在 C++ 白名单里);CAS 失败回 19。
  - LeaveScene 在 switch 之后加了一道防御:判定结果不是删除类就保留 location 并记 Error(防以后新增结论漏改分支)。
  - 复核顺带确认:原文期望也堵住了旧的 6b 双扣——LeaveScene 删除与落点之间不会再各扣一次人数。§10 里登记的「同 scene_id 换节点多算一人」仍另立项。
  - 新增包级 `releasePlayerRetryBackoffs`(默认仍 1s、3s,只为测试缩短等待)。
- 未做:`keep_newer` 速率告警规则(阈值等上线后看基线再定;计数器与告警表达式说明已写在 `metrics.go` 注释)。2026-10-18 已先在 `deploy/k8s/scene-manager-alerts.yaml` 加确定性告警 `SceneManagerPlacementIdentityConflict`(`conflict` / `keep_conflict` 出现即 critical,正常恒 0)。
- 提交时按 hunk 排除别的会话的改动:`enterscenelogic.go` 的 G4a 注释段、`scene_node_client.go` 的 CreateScene 注释、`metrics.go` 的 load_refresh、`scene_node_service.proto` 的 AssetAckReceipt 两行。

### 13.3 W3(b / c / d 的 C++ scene 侧)

- 落地:`release_fence.h`(判定表 + 名表 + static_assert + 原子计数)、`enter_fence::ActingLoginGeneration`、`HandleReleaseFromSceneManager`(守护段只剩一行委托)、`[ReleaseFence]` 30s 汇总、码 25 入白名单、三处发送点盖章(`RequestSceneChange`、`RequestTravelEnterScene`、疏散票据副本)、B5「先抬 epoch 后摘交接组件」顺序注释。
- 后续项(登记,本批不做):
  - **W3-1 盖章接线没有行为测试**:三处发送点的接线(比较哪个会话、在实体销毁前算、写进票据副本)目前只靠静态阅读和纯函数单测守住。补法是把请求组装收成纯函数或给 `SendCorrelatedEnterScene` 加测试捕获接缝;`player_lifecycle.cpp` 仍在被别的会话并发修改,等它稳定后另做。
  - **W3-2 码 25 的钉值(2026-10-18 已收)**:在途改动随快照入库后,25 已并回 `RelocateConfirmKick.UnverifiableKickOnlyWhenSceneManagerCannotHaveGrantedOurs` 的 `pinnedCodes`、注释改「九个码」,末尾单列的 `…PlacementSessionSupersededIsRejectedBeforeAnyPlacementWrite` 已删(DRY)。Go 侧另有 `TestErrPlacementSessionSupersededIsPinnedTo25`。
  - 「可选:在可测宿主里验证盖章」没做(现有宿主断言不许发真实 RPC)。

### 13.4 Java 版(AGENTS §12)

本批只改服务端内部契约(Kafka 命令、player_locator / scene_manager / scene_node 的 gRPC、location 存储格式),不改客户端可见契约。Java 仓需同批运行 `ContractSync` 同步 `game-proto` 副本(否则 `--check` 报漂移);功能结论按 §9:a 不适用(留 1 个待核点)、b / c / d 不适用(结构不同,b/d 留 1 个待核点)、e 待核实。已在 Java 仓 `PARITY.md` 登记。
