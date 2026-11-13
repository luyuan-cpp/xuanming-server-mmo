# gate 排空时把在线玩家迁到同 zone 的其他 gate

> **状态**：设计稿 2026-11-05（Claude）。负责人在问答「gate 重启或灰度、金丝雀发布时 IP 和端口变了怎么办」之后点了「写设计稿」，11-06 点了「按建议落批 A」，§12 四项按本文的建议值执行。
> - **批 A（login、协议、robot、排空脚本）**：11-06 已落码，落码与本文的差异见 §17。协议生成物已随主干快照提交（`a8af9372d` 等），**仍未编译、未测试**，Codex 验证清单在 `docs/PROGRESS.md` 2026-11-06 同名条目。
> - **批 B（gate）**：11-10 已落码（负责人要求全部在 main 上做完），**未编译、未测试**，落码记录与验证清单见 §18 与 `docs/PROGRESS.md` 2026-11-10 同名条目。服务端到这里功能完整；迁移开关 `GateDrain.Migrate.Enabled` 当时默认关，11-12 收尾批改为默认开（§12 第 1 条）。
> - **收尾批（2026-11-12）**：负责人要求「以后修的现在修、不等压测先按标准定」。客户端的跳数清零已落码，`Migrate.Enabled` 改为默认开，速率定为 20，选目标优先不在外出构建上的 gate（§7.3），gate 与 battle 的部署注入构建标识；另加一条守卫用例钉住 login 的 goctl 服务包装必须有 `RequestGateMigration`（**现在还没有，Codex 第一步跑 `go/build.ps1` 重生**）。落码记录见 §19，未编译、未测试。
> - **收尾批续（2026-11-12）**：把原先留作残余或「以后再做」的几项都做掉：login 在会话记录被旧连接的断线通知改掉时重做进场提交，不再回 3023（§5）；客户端转移途中再收到 124 照跟（§10 第 4 条、§13）；接回时角色不离场、邻居看不到他闪一下（§13）；gate 迁移计数接上 `/metrics`（§8）；排空脚本估算同 zone 剩余容量、模板没变的重启先让 StatefulSet 修订变一次（§6、§7.3）；补 `PushRedirect` 的单测与 robot 迁移冒烟（§11）。落码记录见 §20，**未编译、未测试**。
> - **Java 版**：待做，已在 Java 仓 `PARITY.md` 登记（§15）。
> **核对基线**：HEAD `961f334a0` 加 11-05 工作区。当天二十多个会话同时在改，`gate_redirect.go`、`player_lifecycle.cpp`、`client_session_handler.cpp` 等文件都有别人未提交的改动，文中位置按函数名找，不写行号。
> **上线状态**：未上线（见 [存盘围栏设计](db-write-fencing-mysql.md) 文末）。不为旧部署补兼容；新旧版本并存只考虑本功能自己滚动上线的那一段（§9）。
> **客户端**：设计时没有读独立客户端仓 `../mmorpg-client`（AGENTS §9），客户端行为引自本仓文档与 robot 代码。2026-11-12 负责人授权改客户端，§10 逐条核对过，改动见 §19（跳数清零）与 §20（转移途中再收到 124 照跟）。

## 1. 要解决什么

计划内换 gate（重启、换镜像、灰度、金丝雀、节点维护）现在靠等人自然走光：

1. `k8s_gate_drain.ps1` 写 `gate:{gm:<zone>}:<id>:draining`，login 与 scene_manager 不再往这台 gate 分人；
2. login 的排空判定（`gatedrain_monitor.go`）等在线数掉到 `DrainedBelowPlayers`（默认 0），或等满 `Deadline`（默认 25 分钟），写 `gate:{gm:<zone>}:<id>:drained`；
3. 脚本看到 drained 才删 Pod，到期还在线的玩家随 Pod 删除断线。

这有两个问题：

- 在线玩家很少会在 25 分钟内自己走光。一个 zone 的 gate 逐台换完要以小时计，金丝雀的推进速度被它卡住。
- 到期留下的人会断线。11-05 的 [断线保留](player-connection-lost-hold.md) 让角色原地留 25 秒，但 [断线重连审计](disconnect-reconnect-audit-20261020.md) §3.1 记的是 Unity 客户端普通断线后不会自动重登，玩家要回选服界面手动进。

`gatedrain_monitor.go` 文件头写了当时不主动处理的两条理由：主动踢「只是把同一件事提前，并不改变玩家体验」；现有踢线原语会弹「账号在别处登录」。两条对**踢线**都成立，对**重定向**不成立。客户端收到 msg 124 `RedirectToGate` 会自己完成换连接、验票、重新 Login 与 EnterGame，玩家不用操作，也不弹踢线提示。这条链跨 zone 传送已经在用（[跨 zone 传送](cross-zone-scene-travel.md) §2，9-05 实机跑通过）。

## 2. 目标与非目标

目标：

- 排空开始后，按限速把这台 gate 上每个已登录的连接（游戏中与选角界面）重定向到同 zone 的另一台 gate；
- 玩家不用手动重登，回到原来的场景、接回同一个实体，拿到完整入场画面；
- 不改变任何资产、归属、会话的提交判定；
- 速率可控，删掉排空标记就能停；
- 客户端协议不变。

非目标：

- gate 崩溃。没有排空窗口，仍按现有断线与重登处理。
- 搬 TCP 连接。连接属于原进程，换进程一定换连接（[节点生命周期需求](node-lifecycle-elasticity-release-20261023.md) §2）。
- 补发迁移瞬间在途的推送与应答。仍按快照、查询与业务有主协议恢复（审计 §2.3）。
- 跨 zone 传送。它的票据与落点规则不变。
- 判定一次自动重连算不算「无感」。需求文档 §2 没给中断预算，本文不代填数字。

## 3. 现状：能直接复用的部分

逐条核对 11-05 工作区源码的结果：

| 环节 | 现状 | 位置 |
|---|---|---|
| 排空标记与判定 | `gate:{gm:<zone>}:<id>:draining` 的值是标记时刻（Unix 秒），带 TTL；login 每 5 秒判一次，写 `drained=below_threshold` 或 `deadline`；只有一台 gate 的 zone 脚本拒绝排空 | `go/login/internal/logic/pkg/loginqueue/gatedrain.go`、`gatedrain_monitor.go`；`tools/scripts/k8s_gate_drain.ps1` |
| 判定用的在线数 | gate 每 10 秒把 `sessions().size()` 写进 `NodeInfo.player_count`：全部连接都算，包括停在选角界面、还在登录途中的连接 | `cpp/nodes/gate/main.cpp`（`playerCountReportTimer`） |
| 选目标 gate | login：按 zone 过滤 → 选客户端地址并按地址去重 → 剔除排空 → 剔除停写与满载 → 随机二选一。排队 dispatcher 另有一拍内记账的确定性挑法 `gatePicker` | `loginqueue/gatetoken.go`、`gate_eligibility.go`；`go/shared/gatepick`、`go/shared/clientendpoint` |
| 签票 | login `SignGateToken`：`GateTokenPayload{gate_node_id, zone_id, expire_timestamp, hmac_session_key}`。scene_manager 的跨 zone 重定向另绑 `player_id` 与 `target_zone_id`，TTL 300 秒 | `gatetoken.go`；`go/scene_manager/internal/logic/gate_redirect.go` |
| 推重定向 | gate `RedirectToGateEventHandler`：`gate_lane::OnSessionRedirected`（丢掉排队的路由请求，之后的路由请求回 1003，在途那条照常完成），再推 msg 124。不关连接，断线路径不看重定向状态 | `cpp/nodes/gate/handler/event/gate_event_handler.cpp` |
| 客户端跟随 124 | Unity `GameClient.RedirectFlow`：探测 → 连新 gate → 摘掉旧连接处理器并关旧连接 → 原样转票据验票 → 在新连接上完整重跑 Login 与 EnterGame → 等 NotifyEnterScene；最坏 105 秒；每条会话最多跟 3 跳，玩家主动传送时清零。robot `FollowRedirect` 同形，`MaxRedirectHops = 3`，不清零 | 本仓文档：[跨 zone 传送](cross-zone-scene-travel.md) §2、§11.1、§12 的客户端核实结论，[好友客户端规格](friend-client-spec-20260920.md)；`robot/pkg/redirect.go` |
| 旧连接断开 | gate 向 scene 发 `ExitGame(DISCONNECTED)`，向 login 发断线通知（路由模式经路由服，直连模式直发 login）；login 把会话置 DISCONNECTING，租约 30 秒 | `cpp/nodes/gate/handler/rpc/client_session_handler.cpp` `HandleConnectionDisconnection` |
| 角色保留与接回 | scene 收到 DISCONNECTED 后角色原地保留 25 秒；新会话接回同一实体，换会话时只给新客户端重发入场画面（NotifyEnterScene、自身创建、当前看得见的目标），角色不离场（2026-11-12 起，之前是先离场再完整入场） | [断线保留](player-connection-lost-hold.md) §4、§5；`player_lifecycle.cpp` 的 `HandleClientConnectionLost`、`ResumeConnectionLostHold`、`EnterScene` |
| 回原场景 | login 对 ShortReconnect 与 ReplaceLogin 发 `ZoneId=0, SceneId=0`；scene_manager `resumeSceneOf` 按 location 派回原场景，同落点不铸 owner_epoch、不改 location | [断线保留](player-connection-lost-hold.md) §6；`go/scene_manager/internal/logic/enter_resume_scene.go` |
| 新旧会话的先后 | login：旧 gate 的断线通知先到判 ShortReconnect，后到判 ReplaceLogin，并在 SetSession 提交后踢旧会话；player_locator 按会话号、实例号与整字节 CAS 忽略迟到的旧断线；scene 入场栅栏按「会话号 + 登录代次」判，迟到的旧 ExitGame 因会话不符被丢弃 | `go/login/internal/logic/pkg/sessionmanager/session_manager.go`、`clientplayerlogin/entergamelogic.go`；`go/player_locator/internal/logic/setdisconnectinglogic.go`；[入场会话栅栏](scene-enter-session-fence.md) |

到达这一侧已经有完整的路：新连接上的 EnterGame 就是一次普通的租约内重连，11-05 的断线保留与回原场景让它接回同一个实体。**本设计新增的只有前半段：谁发起、发给谁、多快。**

有两处现状会挡路，必须在本设计里处理：

1. **票据不能钉 zone。** login `resolveEnterSceneRoute` 的第 1 条规则：票据 `target_zone_id == 本 zone` 时发 `ZoneId = 本 zone`，排在重连规则之前；而 `resumeSceneOf` 只在 `ZoneId`、`SceneId`、`SceneConfId` 全为 0 时生效。照抄 scene_manager 的跨 zone 票据（`target_zone_id = 本 zone`），scene_manager 会按默认大世界重新挑最空的频道：挑中别的场景，人就被换走；挑中别的节点，还会回可重试的 18，并请原节点按会话结束释放这个角色。同 zone 迁移的票据 `target_zone_id` 必须为 0（§4.4）。
2. **跟随次数上限。** Unity 与 robot 每条会话最多跟 3 次 124，robot 永不清零，Unity 只在玩家主动传送时清零。一个 zone 的 gate 逐台换完，同一个玩家可能被迁不止 3 次，第 4 次会被客户端拒绝（§7.3、§10）。

## 4. 方案

### 4.1 一句话

login 的排空判定看到某台 gate 正在排空，就给它发一条迁移许可；gate 在许可有效期内按限速挑出已登录的会话，逐个向 login 要一张去同 zone 另一台 gate 的票据（游戏中的会话绑定本人），拿到后推 msg 124；客户端照跨 zone 的流程换过去，游戏中的玩家到达后走现有的租约内重连。

### 4.2 时序

```text
运维   k8s_gate_drain.ps1 写 gate:{G1}:draining                              (不变)
login  排空判定每一拍:G1 在排空、在线数 > 阈值、迁移开关开
       → Kafka gate-cmd:GateDrainMigrationEvent{drain_marked_at, rate, lease_ms},发给 G1 这个实例
G1     收到即开启或续期迁移许可(单调时钟 + lease_ms)
       每秒挑至多 rate 个合格会话(§4.3),逐个发 ClientPlayerLogin.RequestGateMigration
         (仅服务端可调;路由模式经路由服,直连模式直发 login;带本会话签过名的 SessionDetails)
login  核实 G1 确在排空 → 选目标 G2(同 zone、非排空、合格,不回落)→ 签票
         GateTokenPayload{gate_node_id=G2, zone_id=Z, expire=now+300s, hmac_session_key,
                          player_id=P, target_zone_id=0}
       → 应答带回 G2 的客户端地址与票据
G1     复核会话还是原来那一个 → OnSessionRedirected + 推 msg 124(与 RedirectToGateEventHandler 同一段代码)
客户端 RedirectFlow:探测 G2 → 连 G2 → 关 G1 → 验票 → Login → EnterGame(P)
G1     旧连接断开 → ExitGame(DISCONNECTED) 给 scene,断线通知给 login                  (不变)
scene  P 进入断线保留:原地停步、摘会话、仍在 AOI 里                                  (不变)
login  通常判 ShortReconnect(G1 的断线先到),偶尔判 ReplaceLogin;都发 ZoneId=0、SceneId=0   (不变)
scene_manager  resumeSceneOf → 原场景、同落点,RoutePlayerEvent 发给 G2                 (不变)
scene  接回同一实体,只给新客户端重发入场画面,P 收到 NotifyEnterScene 与邻居,旁人看不到他闪一下  (11-12 改)
G1     在线数降到阈值 → login 写 drained=below_threshold → 脚本删 Pod                 (不变)
```

标「不变」的步骤都是现有代码；标「11-12 改」的是收尾批续改的（§20）。

### 4.3 gate：限速、挑会话、推 124

调度在 `gate_drain_migration`（`cpp/nodes/gate/handler/rpc/drain_migration.{h,cpp}`），推 124 的共用函数在 `gate_redirect::PushRedirect`（`cpp/nodes/gate/handler/gate_redirect_delivery.{h,cpp}`）。状态全在 gate 的 EventLoop 上（`thread_local` 加会话里的几个字段），不加线程。要票与推 124 是两个函数指针出口，由 `GateClientSessionHandler` 构造时注入（同 `gate_lane::SetRouterSender` 的做法），单测注入替身。

**许可**：收到 `GateDrainMigrationEvent` 时记下 `deadline = 当前单调时刻 + lease_ms`，以及 `rate`、`drain_marked_at`。`rate = 0` 或 `lease_ms = 0` 立即停。许可到期没续上也停：排空取消、已排空或开关关闭后 login 不再发，最多 `lease_ms` 之后 gate 自然停下。

**每秒一拍**（`GateRuntimeContext::drainMigrationTimer`，常驻；没有许可时只做下面滞留关闭的到期检查）：

1. 遍历会话表，挑至多 `rate` 个合格会话；在途请求总数不超过 `2 × rate`。
2. 合格的会话分两类，都要求验过票、`loginSucceeded` 为真、还没推过 124，且不在请求途中、不在退避期（10 秒）：
   - **已进游戏**：`HasBoundRole(session)` 为真（已绑定角色，且不在 LeaveGame 之后的选角期），已有场景路由，没有待补发的进场（`pendingSceneEntry` 为空）。票据绑定本人。
   - **停在选角界面**：`HasBoundRole` 为假，且最近 5 分钟内没有转发过 EnterGame。票据不绑玩家（`player_id = 0`），客户端换过去重新 Login 之后回到选角界面（§10 第 6 条）。这一类也要迁：判定用的在线数把它们算在里面（§3），不迁的话这台 gate 迁完游戏中的玩家后照样要等满期限。5 分钟对齐 login EnterGame 后台链的总预算（`entergamelogic.go` 的 `enterGameChainBudget`）：这段时间里 login 可能还在给这条连接绑角色，迁走会让新连接上的 EnterGame 撞上「登录中」。EnterGame 的时刻在客户端入口收包时记（直连与路由两种模式共用一个点）。
   - 其余会话（还没验票、Login 途中、进场途中、刚发过 EnterGame）这一拍跳过，状态变了之后的拍子再看。最后仍剩下的连接挡住 `below_threshold` 时，照旧按期限处理；运维也可以把 `DrainedBelowPlayers` 调成一个小正数。
3. 对每个合格会话发 `RequestGateMigration`（`client_session_handler.cpp` 的 `SendGateMigrationRequest`）。会话身份用与断线通知同一个 `BuildSessionDetails`，带会话代次。回调只捕获 `session_id`、会话代次，以及请求时的角色与类别这几个值，不捕获指针、不持有连接（AGENTS §11.7）。
   - 直连模式：用生成的 login 调用桩，metadata 与客户端请求的直连转发相同（会话、轨道、调用方签名）。
   - 路由模式：照 Disconnect 的先例合成一条 `ClientRequest`，经路由服 Forward 发出。Disconnect 不看应答，这里要接应答，所以新增 `gate_router_muduo::SendForwardForInternalReply`：与 `SendForward` 共用同一份请求构造（路径、两层 base64 的会话 metadata、调用方签名、截止），应答不进回包桥、不推给客户端，交给迁移模块。
   - 迁移请求与 Disconnect 一样不进会话的路由 lane：不占客户端请求的单飞槽，也不排在客户端请求后面。
   - 出口没发出（没有路由服或 login 实例、缺通道、签名失败）时当场收回在途状态，这条会话退避。

**应答**（到达时刻由出口传入，调度模块自己不读时钟）：

- `OK`：按 `session_id` 找会话。会话没了、`incarnation` 变了，丢弃并计数；再按同一份选人判定复核，已推过 124、开始进场、绑定的角色变了、刚发过 EnterGame，都丢弃这张票，下一拍按新状态重新判。复核通过才调 `PushRedirect`：身份栅栏（票据玩家不为 0 时必须等于会话当前角色，R13，与其他推送同一口径）→ `OnSessionRedirected` → 推 124 → 登记滞留关闭。跨 zone 的 Kafka 路径（`RedirectToGateEventHandler`）改调同一个函数，同样补上身份栅栏。推送没到客户端（连接已释放、身份不符）时票作废，这条会话退避。
- `NOT_DRAINING` 或 `DISABLED`：本 gate 不在排空，或 login 上的迁移开关已关，停掉本轮迁移，防止迟到的许可误迁。
- `NO_TARGET`、调用失败、应答解不出或其他结果：该会话进入退避，之后的拍子再试。

**滞留关闭**：推过 124 的会话，60 秒后连接还在就由 gate 关闭，不推提示。客户端不跟随时，这个会话的路由请求已经一律回 1003，留着只会让玩家卡住；关掉之后走断线保留，与到期删 Pod 的结果相同。

- 对所有推过 124 的会话生效，跨 zone 传送也一样。这与 Java 版现有的 `game.gate.redirect-linger`（缺省 60 秒，到点关闭、不推提示，`GateLimits.DEFAULT_REDIRECT_LINGER`）一致。
- 60 秒写成 gate 的常量，不进配置。客户端换连接时先连新的、再关旧的，旧连接通常在探测的 5 秒内就关了；60 秒没关，说明客户端没有跟随。

**踢线不弹提示**：`KickPlayerEventHandler` 对已推过 124 的会话直接 `shutdown`，不推「账号在别处登录」。ReplaceLogin 踢旧会话时，客户端已经在换连接（第一个 yield 之前就摘掉了旧连接的处理器），这条提示即使送到也不该被看到。这条对跨 zone 重定向同样成立。

**关机**：在关源（`disconnectAllClients`，停机、维护关源与节点号冲突三处共用）时取消定时器：全部连接马上要关，不再要票，也不必再做滞留关闭；拆除前再取消一次，与 gate 其他定时器放在一起（`cpp/nodes/gate/main.cpp`）。

**客户端不能自己发**：`login_admission.h` 的 `IsGateSynthesizedOnly` 加上新消息号。客户端发这个号按非法包处理，与 Disconnect 相同；不拦的话客户端能绕开 gate 的限速与选人条件，直接拿到一张换 gate 的票。

### 4.4 login：发许可、选目标、签票

**发许可**（`gatedrain_monitor.go`）：在 `EvaluateDrainingGates` 现有的循环里，对同时满足下列条件的 gate 发 `GateDrainMigrationEvent`：

- draining 标记存在；
- 在线数大于 `DrainedBelowPlayers`；
- 距标记时刻已满 `Migrate.StartAfter`；
- `Migrate.Enabled` 为真。

发送走 `gate_command.go` 的统一寻址收口，`target_instance_id` 必填（AGENTS §7.2）。快照 `GateOnline` 要带上 gate 实例 uuid。每个 login 副本每一拍各发一次，gate 侧幂等。`lease_ms = 3 × GateDrain.Interval`。

**签票**（新逻辑 `RequestGateMigration`）：

1. 身份取自 gate 签名的 SessionDetails，走现有的 `callerauth`；没有经验签的身份、或缺 gate 实例号与会话代次，一律不签。
   - 选角界面的连接（`player_id = 0`）要签的是不绑玩家的票，先确认这条连接有登录态（`loginsession.GetAccount`），没有就不签：迁移只给已登录的连接换 gate，不能变成一条绕过排队拿票的路。
2. 调用方必须是本 zone 目录里正在排空的那一台，否则回 `NOT_DRAINING`：
   - 在 gate 目录（watch 镜像）里，按本 login 的 zone、调用方的 node_id 与实例 uuid 找得到这台 gate。路由服把请求送错 zone、或同号 gate 的旧进程迟到的请求，都在这里挡下；
   - 它的 draining 标记存在。gate 的 bug 或迟到的许可，不能把一台健康 gate 上的人迁走；
   - 排空标记直读 Redis（`DrainingGateSet`），读失败就不签。登录挑 gate 用的 1 秒缓存（`DrainMarkCache`）是为热路径省读量；签票的读量受迁移速率约束，要的是读到当下的值、读不到就不签。
3. 选目标：候选取 `CandidatesForZone`（watch 镜像，不打 etcd），剔除调用方 gate、排空中的 gate、停写与满载的 gate，再随机二选一。
   - **迁移不回落。** 登录时「全部不合格就回落全集」是为了不把人挡在门外；迁移时回落只会把人推到满载或排空中的 gate 上。没有合格目标就回 `NO_TARGET`，玩家留在原地，等之后的拍子或等到期。
4. 签票：载荷与 `SignGateToken` 相同，再填两个字段，TTL 300 秒：
   - `player_id = 会话玩家`：票据只能本人用（login EnterGame 的持票人检查）。选角界面的会话还没有角色，填 0，等同一张普通的 AssignGate 票据；这个连接已经登录过，不经过排队不算绕过准入；
   - `target_zone_id = 0`：到达时的 EnterGame 走重连规则，发 `ZoneId = 0`，`resumeSceneOf` 才会派回原场景（§3 第 1 条）。客户端在 `target_zone_id` 为 0 时用 `zone_id` 记当前所在 zone，访客迁移后仍认为自己在本 zone。
5. 应答：目标地址（`clientendpoint.Select` 的结果）、payload、signature、deadline。

这一步不写 Redis、不改会话。同一个会话重复请求，只会多签一张没人用的票。

### 4.5 协议

| 改动 | 内容 |
|---|---|
| `proto/contracts/kafka/gate_event.proto` 新增 `GateDrainMigrationEvent`（`OptionKafkaIngress`） | `int64 drain_marked_at = 1;`（标记值，只用于日志和区分轮次）、`uint32 rate_per_second = 2;`（0 = 停）、`uint32 lease_ms = 3;` |
| `proto/login/login.proto` 的 `ClientPlayerLogin` 新增 `RequestGateMigration` | 标 `CLIENT_RPC_SERVER_ONLY`，与 `Disconnect` 同一先例。请求体为空，身份全在签名的 SessionDetails 里。应答：`GateMigrationResult result`，加 `target_gate_ip`、`target_gate_port`、`token_payload`、`token_signature`、`token_deadline`（与 `RedirectToGateEvent` 同名同型） |
| `GateMigrationResult` 枚举 | `GATE_MIGRATION_RESULT_UNSPECIFIED = 0`（按失败处理）、`GATE_MIGRATION_RESULT_OK`、`GATE_MIGRATION_RESULT_NOT_DRAINING`、`GATE_MIGRATION_RESULT_NO_TARGET`、`GATE_MIGRATION_RESULT_DISABLED`（login 上迁移开关已关） |
| 客户端可见协议 | 不变。msg 124 与 `GateTokenPayload` 都不加字段 |

字段类型按 AGENTS §4：时间戳 64 位、计数 uint32、枚举首值为 0。落码时注意：

- 新方法放在 `ClientPlayerLogin`，是为了沿用 Disconnect 的转发与验签路径。`proto/message_id.txt` 按追加分配，会在表尾追加一个号；客户端不调用、不处理它，已有消息号不变。Java 版的 proto 副本要同步追加这个号。
- 新 Kafka 事件要登记进 gate Kafka 命令路由的白名单。
- 路由服的路由表由生成器从 proto 生成；落码时核对生成的表里有新方法，它和 Disconnect 走同一条转发路径。

### 4.6 配置

只配在 login 一处。gate 从许可事件里拿参数，不另配：

```yaml
GateDrain:
  Interval: 5s
  DrainedBelowPlayers: 0
  Deadline: 25m
  Migrate:
    Enabled: true         # 默认开(2026-11-12 定案,§12 第 1 条);false 是应急开关
    RatePerSecond: 20     # 每台排空中的 gate 每秒最多发起的迁移数
    StartAfter: 0s        # 标记 draining 之后多久开始迁移,留给玩家自然下线
```

`Migrate` 的每个字段都要带 default，理由与 `GateDrainConf` 的注释相同：K8s 的 login ConfigMap 不写这一块，整块缺失时取默认值。

`k8s_gate_drain.ps1` 本来就读 login ConfigMap（取 GateDrain 参数校验 TTL）。改成同时打出 `Migrate.Enabled` 与速率，并按速率估算排空时长；`Deadline` 的校验不变。

## 5. 数据一致性

- **没有新的持久状态。** 共享状态只有原来的 draining 标记；许可、限速、退避都在 gate 进程内存里，进程没了就没了。
- **归属不变。** 迁移只换连接。同落点重连不铸 owner_epoch、不改 location（`resumeSceneOf`）。存盘仍按原来的 owner_epoch 与围栏提交。
- **新旧会话的先后靠现有栅栏。**
  - 新 EnterGame 铸出更大的 `login_generation`；
  - 旧连接迟到的断线通知被 player_locator 按会话号、实例号与整字节 CAS 忽略；
  - 迟到的旧 ExitGame 在 scene 因会话不符被丢弃；
  - 旧会话迟到的会话结束释放按代次判 `kSkipNewerLogin`。
  - 各种到达顺序的组合见 [断线保留](player-connection-lost-hold.md) §7 与 [入场会话栅栏](scene-enter-session-fence.md) §5。
- **在途请求。** 迁移只挑路由 lane 为空（没有在途、没有排队）的会话，要票期间 lane 变忙也不推（`Classify`，2026-11-10~11 客户端请求保序收口补的，见 [客户端请求保序](client-request-ordering.md) O9 与 §4.6）。万一推 124 时仍有排队项（跨 zone 重定向不看 lane），gate 按收包顺序对每条回带请求 id 的 1003、不再发出，在途那一条照常执行，应答发往旧连接，连接已关就丢；推 124 之后新到的请求在入口回 1003：路由模式全部拦，直连模式拦 scene TCP 与 gate 本地观战，直连的 gRPC 请求没有 lane、维持现状（与跨 zone 相同，`client_session_handler.cpp` 的重定向拦截一节）。会改资产的请求靠各业务的幂等、查询与快照恢复，迁移不放宽任何提交判定。
- **新连接的进场提交撞上旧连接的断线通知。** 旧 gate 的断线通知（`SetDisconnecting` 让会话版本加一）恰好落在新连接那次 EnterGame 的 GetSession 与 SetSession 之间时，player_locator 按版本拒绝这次写入。窗口是毫秒级的（RedirectFlow 先关旧连接，再验票、再 Login，旧 gate 的断线通知通常早于新的 EnterGame 到达 login），但整台 gate 迁移时次数会累积，所以按标准的乐观并发处理：读、判定、写，写的时候发现记录被改过就重读重做（2026-11-12）。
  - player_locator 把「读到之后记录被改过」（版本落后、CAS 冲突）回成 gRPC `ABORTED`（`setsessionlogic.go` 的 `sessionChangedError`）；别的拒绝（例如登录代次不前进）仍是普通错误。
  - login 把 `ABORTED` 认成 `sessionmanager.ErrSessionChanged`。`entergamelogic.go` 的 `commitEnterGameSession` 遇到它就重读会话、按新记录重新判定（这时旧连接已经断开，通常判成 ShortReconnect）、重新铸登录代次再提交，最多 3 次（`enterGameSessionCommitAttempts`），计 `login_enter_session_commit_redo_total{decision}`。重做不探测、不踢、不等旧会话收尾：被改过的记录说明本次写入根本没落地。
  - 别的写入失败不重做，照旧按失败处理。走短重连分支（`Reconnect`）时的冲突也不重做：那条路已经在租约末尾附近，重做多半撞上「清理中」。
  - 3 次都撞上是极端情形，仍给新连接推 3023，客户端按 3023 的现有契约处理（§10 第 4 条）。之前这里一撞上就推 3023。

## 6. 水平扩展与容量

- 每台排空中的 gate 只迁自己的会话，限速在本进程内，不需要协调者。login 签票是无状态的单次读（watch 镜像加一次 Redis MGET），随 login 副本扩展。
- 每迁一个人，负载等于一次 Login、一次 EnterGame、一次 scene 换会话入场，与一次普通短重连相同。默认每台每秒 20 人（`go/login/etc/login.yaml` 的 `GateDrain.Migrate.RatePerSecond`）；按容量模型一台 gate 约 4500 人，约 4 分钟迁完，远低于 25 分钟的期限。速率已定（2026-11-12），压测出基线后可调，依据见 §12 定案第 2 条，压测口径见 §11；本文不下性能结论。
- 目标侧：选目标时剔除满载（自报上限的 90%）且不回落，迁移不会把别的 gate 顶满。剩余容量不够时，多出来的人留在原地等到期。
- 排空脚本在打标记之前估算一次「其余 gate 装不装得下这台的人」（2026-11-12，`k8s_gate_drain.ps1` 的 `Get-GateDrainPeerHeadroom` / `Format-GateDrainCapacityLine`）：按 etcd 里各 gate 自报的在线数与连接上限，口径同 login 的 `go/shared/gatepick`（水位 = ceil(上限 × 90 / 100)，测试钉住两边一致），装不下时打一条 WARNING 给出大约缺多少人，`-DryRun` 也打。有 gate 没声明上限（本机开发）时不估；已在排空或停写的 gate 没扣掉，所以真实缺口可能更大，文案里写明。只提示，不改排空流程，扩容与否由运维决定。

## 7. 与发布轨道、逐台换版的关系

### 7.1 轨道

会话的轨道等于所在 gate 的轨道（[发布轨道](release-track-gray-canary.md) §2.3）。迁移是新连接，迁到哪台 gate 就属于哪条轨道，与断线后重登落到哪台 gate 一样。轨道按 zone 切，同一个 zone 里两条轨道只在换版期间短时并存；迁移不跨 zone，不会把稳定 zone 的玩家送进金丝雀。

### 7.2 第一批选目标不看轨道

规则与登录挑 gate 一致，代价见 7.3。

### 7.3 重复迁移

一个 zone 的 gate 是逐台换的：StatefulSet 用 `OnDelete`，排空一台、删一台、再用同一序号重建一台。

- 排空第一台时还没有新版 gate，它上面的人只能迁到其他旧 gate。
- 之后每排空一台，迁走的人仍有一部分落在还没换的旧 gate 上，轮到那台时会再迁一次。
- N 台 gate 的 zone，最坏情况下同一个玩家要迁 N−1 次。

这带来两个问题：玩家每迁一次就换一次连接；第 4 次会撞上客户端的 3 跳上限。两条都已处理（2026-11-12）：

- **跳数清零**：客户端和 robot 都在成功进场（收到 NotifyEnterScene）时把跳数清零（§10 第 2 条）。熔断原本防的是登录期重定向来回弹，那种来回弹不会成功进场，所以清零不削弱它。
- **优先迁到新构建**：迁移选目标时，合格目标里有不在「外出构建」上的，就只在它们里面挑；一个都没有才用全部合格目标（`loginqueue.PickMigrationTarget`）。
  - 外出构建 = 本 zone 里至少有一台排空中 gate 的构建，取自 gate 自报的 `NodeInfo.build`。它由两样拼成：部署经 `MMORPG_BUILD_LABEL` 注入的构建标识（与 Pod 标签 `mmorpg.io/build` 同值），以及经 downward API 注入的工作负载修订 `MMORPG_POD_REVISION`（gate StatefulSet 的 `controller-revision-hash`、Deployment 的 `pod-template-hash`）。与 scene 选落点同一口径（[scene 计划排空](scene-node-planned-drain.md) §4.6）：不需要知道哪个版本更新，升级时被排空的是旧版本，回退时被排空的是新版本。
  - 效果：第一台排空时还没有新构建的 gate，它上面的人只能迁到旧构建的 gate，之后还要再迁一次；从第二台起都迁到已换好的 gate 上，不再重复。一个玩家一轮换版最多迁 2 次，而不是 N−1 次。
  - 满载、停写的 gate 在分档之前就被剔掉，新构建的 gate 装满之后自动回到全部合格目标，偏好不会把人留在要下线的 gate 上。
  - 没上报构建（空串，本机开发）不产生偏好。
  - 同一个镜像只改配置的换版：Pod 模板变了，工作负载修订跟着变，已重建的 gate 与还没换的 gate 分得出来，规则照样生效。
  - 模板完全没变的整批重启：直接逐台删 Pod 的话，重建出来的还是同一个修订，分不出新旧，要先让修订变一次。`k8s_gate_drain.ps1 -DeletePod` 自动做这一步（`Sync-GateRestartRevision`，判定在纯函数 `Get-GateRevisionAction`）：
    - 在任何 Redis 写之前，读被排空 Pod 的标签 `controller-revision-hash` 与 StatefulSet 的 `status.updateRevision`；
    - 两者相等（这台已经是当前修订，删掉重建还是同一代）：先执行 `kubectl rollout restart statefulset/gate`，打一行 `[gate-drain] pod is already at the current revision; bumped the StatefulSet revision ...`，再有界（30 秒）等控制器的 `observedGeneration` 跟上、`updateRevision` 变化，等不到只告警。gate 的 StatefulSet 是 `OnDelete`，`rollout restart` 只给 Pod 模板写一个 `kubectl.kubernetes.io/restartedAt` 注解、不重启任何 Pod（核对过 kubectl 源码）；之后重建的 Pod 带新的修订，规则照样生效；
    - 两者不等（模板已经变过，正常换版）：什么都不做；
    - 读不到标签或状态、`rollout restart` 失败（包括 kubectl 对 1 秒内重复 restart 报的 empty patch）：告警后照常排空，不挡删 Pod，最坏回到「可能重复迁移」，跳数清零保证不会被熔断；
    - `-DryRun` 只读，打印将要执行的 restart 命令。
  - podip 形态的 gate 是 Deployment，`kubectl rollout restart deployment/gate` 本身就是整批重启。各形态的做法汇总在 [scene 计划排空](scene-node-planned-drain.md) §4.6「整批重启」。只重启一台（`kubectl delete pod`）时没有新旧之分，也不需要。
  - 原稿设想按发布轨道分档；轨道只分 stable / canary，同轨道内换版分不出新旧，所以改按构建标识。

## 8. 可观测性

只用低基数标签，不带 player_id（AGENTS §9）：

- gate 指标（2026-11-12 接上 C++ 节点的 `/metrics`：`metrics/metrics_registry.h`，设了 `MMORPG_METRICS_PORT` 才导出；收集函数在节点主循环上跑，直接读调度模块的 thread_local 计数）。`gate/main.cpp` 启动时调 `gate_drain_migration::RegisterMetrics()`，收集函数是 `CollectMetrics`：
  - `gate_drain_migration_total{result}`，进程累计：`requested`（发给 login 的要票请求）、`offered`（推出的 124）、`not_sent`、`not_draining`、`disabled`、`no_target`、`failed`、`stale_reply`、`push_dropped`（要票没有变成 124 的各种结局）；
  - `gate_drain_migration_lease_total{event}`：`received`（含续期）、`stopped`（到期、`rate` 或 `lease` 为 0、`NOT_DRAINING`、`DISABLED`）；
  - `gate_drain_migration_active`（许可在效为 1）、`gate_drain_migration_in_flight`（还没应答的要票数）；
  - `gate_redirect_linger_closed_total`：推过 124 满 60 秒仍连着、被 gate 收尾关闭的连接数（迁移与跨 zone 共用）。
  - 标签只有结果与事件，不带玩家或会话。
- gate 日志，前缀 `[DrainMigration]`：
  - 许可开始一行 INFO（`drain_marked_at`、速率、`lease_ms`、当时的会话数），停止一行 INFO（原因同上）并带一行累计汇总；
  - 迁移进行中每 10 秒一行汇总（与上面的计数同名），以及当前在途数；
  - 滞留关闭逐条一行 INFO（只带 `session_id`）。
- login：`login_gate_migration_sign_total{result}`、`login_gate_migration_lease_total{result}`；进场提交因会话记录被改而重做的次数 `login_enter_session_commit_redo_total{decision}`（§5）；排空判定发许可的日志按 gate 节流。
- 效果直接看现有信号：
  - 被排空 gate 的在线数曲线；
  - `gate:{gm:<zone>}:<id>:drained` 的理由，应当是 `below_threshold`；
  - scene 日志 `[ConnectionLostHold] resumed by a new session`；
  - `enter_scene_rejected_total{reason="resume_scene_fallback"}`，迁移期间应当接近 0。

## 9. 上线顺序与兼容

- 本功能自己的滚动顺序：先 login（新方法与新配置），再路由服（路由表需要重生时），最后 gate。项目未上线，首次上线整批带上，`Enabled` 默认开。
- gate 是逐台换的。没换到新版的 gate 收到许可事件会按白名单丢弃，照旧等自然排空。所以本功能第一次上线的那一轮换版用不上迁移，从下一轮开始生效。
- 新 gate 调到旧 login 拿到「未实现」或错误：会话进入退避、不迁，不影响排空与删 Pod。
- 关闭：把 `Migrate.Enabled` 置 false，最多一个 `lease_ms` 后所有 gate 停止迁移，不需要回滚二进制。

## 10. 要由客户端任务核实或修改的点

设计时没有读客户端仓，下列各条依据本仓文档与 robot 代码写成。2026-11-12 负责人授权改客户端，逐条核对结果（客户端 `Assets/Scripts/Game/GameClient.cs`）：

- 第 2 条原先没做，已落码（§19）；第 1、3、5、6 条客户端已有对应实现。
- 第 4 条客户端按失败收口（提示「切换服务器失败，请重新登录」并断开）。服务端 11-12 起在 login 里重做进场提交（§5），3023 只在连撞 3 次的极端情形才会出现，客户端不用改。
- 核对时另发现第 7 条：客户端在转移途中（`_redirecting`）收到的第二条 124 被直接忽略，60 秒后按失败收口。11-12 已改成照跟（§20）。

1. RedirectFlow 对同 zone 的 124（`target_zone_id = 0`）照常工作，进场后 `CurrentZoneId` 取票据里的 `zone_id`。
2. 跳数上限改成成功进场（收到新连接上的 NotifyEnterScene）后清零。robot 在本仓，同批修改 `robot/pkg/redirect.go`。
3. 战斗直连：重定向过程中战斗直连会不会断；断了的话，是否按 `BattleReconnectS2C` 用 `MatchService.RequestBattleTicket` 补签重建（[回合战斗](turn-based-battle-server.md) D38、D72）。
4. 新连接上的 EnterGame 回 3023（§5 的竞态）时，RedirectFlow 是重试还是回选服界面。
5. 旧连接上晚到的 `KickPlayer` 不弹框。§4.3 已在服务端去掉这条提示，这里只是确认。
6. 停在选角界面时收到 124（票据 `player_id = 0`）：换到新 gate、重新 Login 之后回到选角界面。本仓文档记的规则是「已进游戏取当前角色，EnterGame 应答未到取本次请求的角色，两者都为 0 才回选角」（[跨 zone 传送](cross-zone-scene-travel.md) §12 的客户端核实结论）。
7. 转移途中再收到 124：新连接上的 EnterGame 被 login 再重定向到别的 zone（迁移撞上跨 zone 传送，§13），或新 gate 自己也开始排空。客户端照跟这一条，仍受每条会话 3 跳的上限约束（成功进场后清零）；角色、所在 zone 与战斗号从上一跳带下来（新的一跳还没拿到它们时用上一跳的），被新一跳取代的旧流程结束时不清 `_redirecting`。robot 的 `FollowRedirect` 本来就照跟（重登期间暂存的推送补投时重入，`MaxRedirectHops` 封顶）。

## 11. 测试

单测（按 AGENTS §10.1 由 Codex 执行）：

- Go login：
  - 许可只发给「排空中、在线数大于阈值、已满 StartAfter、开关开」的 gate，且带实例 uuid；
  - 签票拒绝不在排空的 gate，拒绝缺失或验签不过的会话身份；
  - 目标剔除调用方、排空、停写、满载，全部不合格时回 `NO_TARGET`，不回落；
  - 票据的 `player_id` 等于会话玩家（选角界面的会话为 0），`target_zone_id = 0`；
  - 配置整块缺失时取默认值。
- C++ gate（`cpp/nodes/gate/tests/drain_migration_test.cpp`，编进 `routing_identity_test`）：
  - 许可续期与到期，`rate = 0` 或 `lease_ms = 0` 时停止；
  - 每拍不超过速率，在途请求不超过上限，在途的会话不重复要票；
  - 合格判定的每一条，包括已进游戏与停在选角界面两类、EnterGame 之后 5 分钟的避让、LeaveGame 之后按选角界面处理，以及跳过没验票、没登录成功、进场途中、已推过 124 的会话；
  - 应答到达时会话已换、开始进场、角色已变，都丢弃；`OK` 的票据字段原样进 124，游戏中绑定本人、选角界面为 0；
  - `NOT_DRAINING`、`DISABLED` 停许可；`NO_TARGET`、调用失败、没发出、推送没到客户端都退避；
  - 滞留关闭：满 60 秒关真实的回环连接，会话已不在或号已复用时不关。
  - 客户端入口拒绝客户端自发的新消息号（`gate_client_entry_admission_test.cpp`）。
  - 推送出口 `PushRedirect` 11-12 起有单测（`cpp/nodes/gate/tests/gate_redirect_delivery_test.cpp`，同样编进 `routing_identity_test`，用真实的路由 lane 与回环连接）：排队的请求先各回 1003、再发 124 的字节顺序，身份栅栏（票据给别的玩家、未绑定会话只收不绑玩家的票），会话不在或连接已释放时什么都不改，第二条 124 照发且滞留起点不变，滞留期满由扫描关闭。
  - 迁移计数导出成 `/metrics` 的格式：`DrainMigration.CountersAreExportedAsPrometheusMetrics`。
  - 仍进不了测试工程的：「推过 124 的会话被踢时不推提示」（在 `gate_event_handler.cpp`，拖着节点注册表与场景入口），以及要票出口与定时器（`client_session_handler.cpp`、`main.cpp`）。靠核对调用点与 robot 冒烟（G3-no-kick、G3-migrate）。
- 跨语言：许可事件与新 RPC 的字段两边都由同一份 proto 生成，没有手写的第二份，单位写在字段名里（`lease_ms`、`rate_per_second`），所以不另做字节对拍。两边各写一份字面量的只有选角避让窗口：gate 的 `kEnterGameSettle` 不得短于 login 的 `enterGameChainBudget`，由 `go/login/internal/logic/clientplayerlogin/gate_drain_settle_crosslang_test.go` 读 C++ 头文件对拍，做法同发布轨道的 `crosslang_test.go`。

集成（本地或 kind，用 robot）。第 1 条的主干已做成可重复跑的冒烟 `mode: gate-migrate-smoke`（`robot/gate_migrate_smoke_scenario.go`，前置与用法在 `robot/etc/gate_migrate_smoke.yaml` 文件头）：4 个机器人登录进场 → 打印每台 gate 的排空标记键与打标记命令（`GATE_MIGRATE_SMOKE_WAITING`），由人或外层脚本给其中一台打标记 → G3 被排空那台上的全部在 60 秒内迁走、票据签给别的 gate、落回原场景里的同一个 actor、没收到踢线推送 → G4 别的 gate 上的不动，并且谁都没看到被迁的人的 actor 被移除 → G5 新连接上发给场景的请求有应答、心跳有回包。结果行 `GATE_MIGRATE_SMOKE_OK` / `GATE_MIGRATE_SMOKE_FAIL step=…`。副本、非默认地图与选角界面的变体仍按第 1 条手工做。

1. 两台 gate，其中一台挂 N 个 robot，分别在大世界、副本、非默认地图与选角界面；打开迁移后给这台打排空标记。期望：
   - 全部 robot 换到另一台 gate，场景 id 与坐标不变，没有 robot 会话失败；
   - scene 日志里有保留与接回；
   - `drained=below_threshold`，删 Pod 时 0 人在线。
2. 迁移进行到一半删掉排空标记：一个 `lease_ms` 内停止，已迁走的人不回迁。
3. 目标 gate 全满：返回 `NO_TARGET`，人留在原地，到期按 `deadline` 处理。
4. 三台以上 gate 逐台换完：统计每个 robot 的跳数与迁移次数，对比跳数清零改动前后。

压测（AGENTS §6，按 `stress_summarize.ps1` 的口径）：在负载下迁移一台满载 gate，与不迁移的基线对比 login、scene_manager EnterScene 各子阶段与 scene 入场耗时，以及 `login_enter_session_commit_redo_total`。速率已按 §12 定案，压测只用来核对，不够时调参数。

## 12. 定案（2026-11-12）

负责人 2026-11-12：「查出来以后修的现在修；有些东西不等压测，先按最标准的定」。四项按下面执行：

1. **`Migrate.Enabled` 默认开。** 客户端与 robot 的跳数清零都已落码。不开的话 gate 重启只能等玩家自己下线、到期断开，做不到无感。置 false 是应急开关。
2. **默认速率 20 人/秒（每台排空中的 gate）。** 每迁一个人的负载等于一次短重连（Login + EnterGame + scene 换会话入场）。按[容量模型](capacity-model-hok-scale.md)一台 gate 约 4500 人，约 4 分钟迁完，远小于 25 分钟的排空期限；一台 gate 人数超过约 1.5 万（20 × 期限的一半）时再调大。压测只用来核对，不够时调参数。
3. **选目标优先不在外出构建上的 gate**（§7.3），按构建标识而不是发布轨道分档。
4. **战斗中的玩家不延后迁移。** gate 不知道谁在战斗（战斗绑定已从 gate 移除）；客户端在重定向完成后按 `battle_id` 补签重建战斗直连（`RedirectFlow` 成功回调里的 `BattleLink.HandleReconnectHint`）。

## 13. 残余

原先列在这里的三条，2026-11-12 收尾批续处理如下（§20）：

- **观察者看到被迁的人闪一下**：已改掉。接回时不再先离场再入场，scene 只把入场画面重发给新客户端，角色留在原地，邻居的画面不变（[断线保留](player-connection-lost-hold.md) §5 第 3 步）。
- **接回时只有场景这一层画面**：服务端实体没换，CD、Buff 与其它运行时状态都还在；新客户端收到入场通知、自身的创建、当前看得见的目标的创建，正在走的目标再补一条移动。目标身上已经广播过、之后没再变的持续状态（例如战斗标记）不在创建消息里，这是 AOI 新观察者基线的缺口（审计 §9.2），所有进视野的观察者都一样，归实时同步那条线修，迁移没有扩大它。换连接那一两秒里丢掉的推送按 §2 的非目标处理：靠快照、查询与[落地恢复](landing-recovery-sync-digest.md)的版本摘要恢复。
- **迁移撞上跨 zone 传送的交接窗口**：scene 照断线保留的规则立即退出而不保留（交接在途），交接照常裁决。交接已提交时，新连接上的 EnterGame 被 scene_manager 再重定向一次，客户端在转移途中收到这第二条 124 会照跟（§10 第 7 条）；交接没提交时玩家留在原 zone，这次传送算失败，数据按交接的裁决是一致的。gate 看不到交接状态，不为它延后迁移：交接窗口是秒级，整个排空是分钟级，撞上的概率小，结局正确，代价是多一跳。

剩下的只有一条：**同一个玩家一轮换版最多迁 2 次**（§7.3）。第一台排空时还没有换好的 gate，它上面的人只能先去旧的 gate，轮到那台时再迁一次。这是逐台原地替换的固有代价：先多扩一台新 gate 来承接也消不掉它，StatefulSet 缩回原副本数时要排空的正是最后扩出来的那台，上面的人照样再迁一次。

## 14. 改动清单（预估）

按 AGENTS §10.2，每批不超过 30 个文件：

- **批 A：login、协议、robot。**
  - 两个 proto；
  - `go/login`：排空判定、配置、`RequestGateMigration` 逻辑、签票、指标与单测；
  - `go/client_rpc_router`：生成的路由表（需要重生时）；
  - `robot/pkg/redirect.go`：跳数清零，加一个迁移冒烟场景；
  - `tools/scripts/k8s_gate_drain.ps1`：打印迁移配置，及其契约测试。
- **批 B：gate。**
  - `GateDrainMigration` 新文件与单测；
  - `gate_event_handler.cpp`：许可事件、共用投递函数、踢线不弹提示；
  - `client_session_handler.cpp`：发请求、处理应答，`SendViaRouter` 的完成回调；
  - `session_info_comp.h`：每个会话的迁移状态，以及推过 124 的时刻；
  - 滞留关闭的扫描：挂在 gate 的运行时上下文上，关机时取消；
  - `login_admission.h`、`gate/main.cpp`（装配、关机时取消定时器），以及工程文件。
- **文档**：落码时改写下面几处正文，不只加「已取代」标注：
  - `gatedrain_monitor.go` 与 `gatedrain.go` 文件头「刻意不做」的说明；
  - [集群外入口](k8s-client-entry.md)「明确不做」里排空超时迁移那一条；
  - [节点生命周期需求](node-lifecycle-elasticity-release-20261023.md) §3 的 gate 一行。

前置：[断线保留](player-connection-lost-hold.md) 与 `resumeSceneOf` 先经 Codex 编译、测试通过。本设计到达这一侧完全依赖它们。

## 15. Java 版

待做。Java 版（`D:\luyuan\xuanming-server-mmo`）的 gate 已有「推过 124 之后只收口、不再转发」的会话状态，以及 `game.gate.redirect-linger`（缺省 60 秒，到点关闭、不推提示）；gate 排空同样允许到期断开；断线保留也还是待做（[断线保留](player-connection-lost-hold.md) §9）。

Java 要实现同一组行为：排空时按限速推 124，选角界面的连接也迁（EnterGame 之后 5 分钟内不迁）；游戏中的会话票据绑定本人，所有迁移票据都不钉 zone；到达走租约内重连并回原场景；推过 124 的会话被踢不弹提示；客户端不能自己发迁移请求；跳数在成功进场后清零。11-06 已在 Java 仓 `PARITY.md` 登记待做，11-10 主仓批 B 落码后更新了那一行。

## 16. 参考

- 长连接网关排空的常见做法是：服务端通知客户端重连，客户端经正常入口连到别的实例，再按会话恢复（例如 Discord Gateway 的 Reconnect 与 Resume）。本设计用 124 与租约内重连实现同一思路。
- 相关设计：[节点生命周期需求](node-lifecycle-elasticity-release-20261023.md)、[发布轨道](release-track-gray-canary.md)、[断线保留](player-connection-lost-hold.md)、[跨 zone 传送](cross-zone-scene-travel.md)、[集群外入口](k8s-client-entry.md)、[gate 负载均衡](gate-load-balancing-design.md)、[入场会话栅栏](scene-enter-session-fence.md)。

## 17. 批 A 落码记录（2026-11-06，未生成协议、未编译、未测试）

### 17.1 改了什么

| 位置 | 内容 |
|---|---|
| `proto/contracts/kafka/gate_event.proto` | 新增 `GateDrainMigrationEvent{drain_marked_at, rate_per_second, lease_ms}` |
| `proto/login/login.proto` | `ClientPlayerLogin` 末尾新增 `RequestGateMigration`（`CLIENT_RPC_SERVER_ONLY`）；新增 `GateMigrationRequest`、`GateMigrationResponse`、`GateMigrationResult` |
| `go/login/internal/config/config.go`、`etc/login.yaml` | `GateDrain.Migrate{Enabled=false, RatePerSecond=20, StartAfter=0s}`，子块缺失取默认值 |
| `go/login/internal/logic/pkg/loginqueue/gatedrain_migrate.go`（新） | 发许可的判定 `DecideDrainMigration`、`DrainMigrationsDue`；不回落的选目标 `PickMigrationTarget`；fail-closed 的 `DrainingGateSet` |
| `go/login/internal/logic/pkg/loginqueue/gatetoken.go` | `SignGateToken` 与新的 `SignMigrationGateToken` 共用一个 `signGateToken`：迁移票据多绑 `player_id`，`target_zone_id` 恒为 0 |
| `go/login/internal/logic/pkg/loginqueue/gatedrain_monitor.go` | `GateOnline` 加 `InstanceID`（只给许可定向用） |
| `go/login/internal/svc/gate_migration.go`（新） | 每拍发许可 `sendDrainMigrationLeases`；`SendGateDrainMigration`；签票 `SignGateMigration` 与纯判定 `planGateMigration`；指标 `login_gate_migration_sign_total{result}`、`login_gate_migration_lease_total{result}` |
| `go/login/internal/svc/gate_command.go` | `buildGateDrainMigrationCommand`，走统一的寻址收口与三道 fail-closed 守卫 |
| `go/login/internal/svc/servicecontext.go` | 排空判定循环每拍在 `EvaluateDrainingGates` 之后发许可；快照带实例 uuid |
| `go/login/internal/logic/clientplayerlogin/requestgatemigrationlogic.go`（新） | RPC 逻辑：取验签过的会话身份，交给 `SignGateMigration` |
| `robot/pkg/redirect.go`、`robot/logic/handler/scene_scene_client_player_notify_enter_scene.go` | 收到 NotifyEnterScene 时 `NoteSceneEntered` 把跟随重定向的跳数清零 |
| `tools/scripts/k8s_gate_drain.ps1` | 读 `GateDrain.Migrate`，打印迁移开关、速率与按在线数估算的迁移时长；不改变排空流程与预算校验 |
| 单测 | `config/gate_drain_conf_test.go`、`loginqueue/gatedrain_migrate_test.go`（新）、`svc/gate_migration_test.go`（新）、`svc/gate_command_test.go`、`robot/pkg/redirect_test.go`、`tools/scripts/tests/k8s_gate_drain.tests.ps1` |

生成物没有手改：login 的 goctl 服务包装（`internal/server/clientplayerlogin/clientplayerloginserver.go`）、消息号与事件号表、路由服的路由表、各语言的 pb，都由 Codex 重新生成（AGENTS §3、§4）。

### 17.2 与上文的差异

- **robot 没有加专门的迁移冒烟场景。** robot 本来就跟随 124；迁移的集成验证按 §11 的步骤手工排空一台 gate，命令写在 Codex 清单里。
- **签票直读排空标记**，不用登录挑 gate 的 1 秒缓存，理由见 §4.4 第 2 条（正文已按落码改写）。
- **选角界面的连接签票前核对登录态**、**调用方要在本 zone 目录里对得上实例 uuid**，两条都已写进 §4.4 正文。

### 17.3 交给批 B 的点

批 A 当时列给 gate 的五件事（许可事件、每秒一拍与在途上限、要票的发送与应答、共用投递函数补身份栅栏、踢线不弹提示与滞留关闭、`IsGateSynthesizedOnly`）已全部由批 B 接上，见 §18。路由模式「接应答」没有改 `SendViaRouter`，而是新增了 `SendForwardForInternalReply`（§4.3 第 3 步）。

## 18. 批 B 落码记录（2026-11-10，未编译、未测试）

负责人 11-10 要求：全部改动在 main 上做，不开分支；只写服务端代码，客户端、协议生成、编译与冒烟交给 Codex；Java 只登记待做。协议生成物在动手前已随主干快照提交，批 B 没有改 proto。

### 18.1 改了什么

| 位置 | 内容 |
|---|---|
| `cpp/nodes/gate/handler/rpc/drain_migration.{h,cpp}`（新） | 调度：许可（开始、续期、到期、`rate`/`lease` 为 0 停）；每秒一拍按速率与在途上限挑会话；选人判定 `Classify`（应答到达时复核用同一份）；应答处置与退避；滞留关闭队列；`[DrainMigration]` 日志与进程累计计数。两个出口是函数指针，只能在 gate 的 EventLoop 线程调用 |
| `cpp/nodes/gate/handler/gate_redirect_delivery.{h,cpp}`（新） | `PushRedirect`：推 124 的唯一收口，跨 zone 与排空迁移共用。找会话 → 身份栅栏（`ClassifyPush`）→ lock 连接 → `OnSessionRedirected` → 推送 → 登记滞留关闭 |
| `cpp/nodes/gate/handler/event/gate_event_handler.cpp` | `GateDrainMigrationEventHandler` → `OnLease`；`RedirectToGateEventHandler` 改调 `PushRedirect`（新增身份栅栏丢弃的 WARN）；`KickPlayerEventHandler` 对推过 124 的会话直接 `shutdown`、不推「账号在别处登录」 |
| `cpp/nodes/gate/handler/rpc/client_session_handler.cpp` | `SendGateMigrationRequest`（直连 / 路由两种出站）；路由服选点拆成 `PickRouterChannel` 与 `MakeForwardRequest` 两个辅助，`SendRawViaRouter` 改用它们、行为不变；构造时注入两个出口；EnterGame 收包点调 `NoteEnterGameForwarded` |
| `cpp/nodes/gate/handler/rpc/router_muduo_forward.{h,cpp}` | 新增 `SendForwardForInternalReply`；与 `SendForward` 共用抽出来的 `PrepareForwardCall`，线上字节两条路径逐字相同 |
| `cpp/nodes/gate/handler/rpc/login_admission.h` | `IsGateSynthesizedOnly` 加 `ClientPlayerLoginRequestGateMigrationMessageId` |
| `cpp/libs/services/gate/session/comp/session_info_comp.h` | 会话上的迁移状态：在途标记、退避截止、最近一次 EnterGame 时刻、推 124 的时刻 |
| `cpp/nodes/gate/main.cpp` | `drainMigrationTimer` 每秒一拍；关源与拆除前取消 |
| `cpp/nodes/gate/gate.vcxproj`、`.filters` | 登记两组新文件。`CMakeLists.txt` 由 `build_linux.sh` 按 vcxproj 生成，没有手改 |
| 单测 | `cpp/nodes/gate/tests/drain_migration_test.cpp`（新，22 例：调度 20 例、滞留关闭真连接 2 例）；`cpp/tests/routing_identity_test/gate_client_entry_admission_test.cpp` 加 1 例；`routing_identity_test.vcxproj` 登记被测源码与新测试（仅 Debug\|x64，同 gate 其他被测源码的做法）；`go/login/internal/logic/clientplayerlogin/gate_drain_settle_crosslang_test.go`（新）读 C++ 头文件核对选角避让窗口不短于 login EnterGame 后台链预算 |
| 批 A 收尾 | `go/login/internal/svc/gate_command.go` 的空事件报错、`tools/scripts/k8s_gate_drain.ps1` 两条迁移配置的 throw 改成英文（AGENTS §2：服务与脚本的诊断文案用英文；测试按键名匹配，不受影响） |

### 18.2 与上文的差异与补充决定

- **应答回调带到达时刻。** `ReplyCallback` 的第二个参数是应答到达的单调时刻，由出口在回调点读取：退避从这里起算，调度模块自己不读时钟，单测全部用给定时刻。
- **推送没到客户端也退避。** 设计稿只写了 `NO_TARGET` 与错误退避；连接已释放或身份不符时票作废，不退避的话下一拍会立刻再要一张。
- **滞留关闭的定时器常驻。** 设计稿写「只在许可有效时挂定时器」；跨 zone 推过 124 的会话也要做滞留关闭，所以定时器常驻，没有许可时每拍只看一次队首。
- **gate 只走日志，不出指标**（§8 已改写）：C++ 节点还没有 `/metrics` 出口。
- **直连模式的迁移请求带轨道 metadata**，与客户端请求的直连转发一致；路由模式由路由服按 `ForwardRequest.release_track` 写同一个键。

### 18.3 验证（交 Codex，AGENTS §10.1）

步骤与命令写在 `docs/PROGRESS.md` 2026-11-10 同名条目：整解编译、`routing_identity_test` 全量、批 A 的 Go 与脚本测试、robot 单测，以及按 §11 的本地两台 gate 迁移冒烟（`GateDrain.Migrate.Enabled=true`）。没有运行结果之前，本节所有内容都只是静态落码。

**第一步必须先重生 login 的 goctl 服务包装**（`pwsh -NoProfile -ExecutionPolicy Bypass -File go/build.ps1`）。11-10 复核时 `go/login/internal/server/clientplayerlogin/clientplayerloginserver.go` 还没有 `RequestGateMigration`：Go pb 与 goctl 的输入 proto 都已带上这个方法，只差这一步。不重生时 login 照样编得过（服务结构体嵌着 `UnimplementedClientPlayerLoginServer`），但 gate 每次要票都收到 UNIMPLEMENTED，迁移一律退避、永远不生效，单测测不出来。更正写在 PROGRESS 同日「goctl 服务包装必须重生」一条。

### 18.4 还没做的

批 B 当时留下的两项（客户端跳数清零、默认打开迁移）已在 2026-11-12 收尾批做完，见 §19。**Java 版**待做（§15）。

## 19. 收尾批落码记录（2026-11-12，未编译、未测试）

### 19.1 改了什么

| 位置 | 改动 |
|---|---|
| 客户端 `Assets/Scripts/Game/GameClient.cs` | NotifyEnterScene 处理器调 `NoteSceneEnteredForRedirect`，把 `_redirectHops` 归零（与 robot 的 `NoteSceneEntered` 同语义）；重定向日志改用这一跳的序号 |
| 客户端 `Assets/Tests/EditMode/Net/GameClientReconnectTests.cs` | `SceneEntryResetsRedirectHopsSoRepeatedGateMigrationsAreNotCutOff` |
| `go/login/internal/config/config.go`、`gate_drain_conf_test.go`、`go/login/etc/login.yaml` | `Migrate.Enabled` 的 default 改 true；用例补「只写 Enabled: false 能关」「只写速率时开关仍是开」 |
| `go/login/internal/logic/pkg/loginqueue/{gatetoken.go,gatedrain_migrate.go,gatedrain_migrate_test.go}` | `GateCandidate.Build`；`PickMigrationTarget` 在合格目标里优先不在外出构建上的（`outgoingBuilds`、`preferIncomingBuild`）；4 条用例 |
| `go/login/internal/svc/{servicecontext.go,gate_migration_test.go}` | `buildGateCandidates` 带上 `NodeInfo.build`；接线用例 `TestPlanGateMigration_PrefersGatesOffTheOutgoingBuild` |
| `go/login/goctl_wrappers_test.go`（新） | 逐个服务核对 goctl 服务包装实现了 pb 里的每个方法。**现在是红的**：`clientplayerloginserver.go` 还没有 `RequestGateMigration`，要先跑 `go/build.ps1` 重生 |
| `tools/scripts/k8s_gate_drain.ps1`、`tests/k8s_gate_drain.tests.ps1` | 缺省迁移开关与 login 结构体同步为开；用例改为「缺省开、显式 false 关」 |
| `tools/scripts/k8s_deploy.ps1`、`lib/k8s_client_entry.ps1`、`tests/scene_planned_drain_deploy.tests.ps1` | gate 与 battle 的每种编排都注入 `MMORPG_BUILD_LABEL`（`Get-CppBuildLabelEnvLines`、`PodCommon.BuildLabel`）；Deployment 与 StatefulSet 形态再经 downward API 注入 `MMORPG_POD_REVISION`（Fleet 不注入） |
| `cpp/libs/engine/core/node/system/node/node_planned_drain.h`、`cpp/tests/cross_zone_test/planned_drain_test.cpp` | `ComposeBuildLabel`：`NodeInfo.build` = 构建标识 `@` 工作负载修订（`Node::InitRpcServer` 注册时写入，scene 计划排空批已落码）；用例 `PlannedDrainBuildLabel.*` |

### 19.2 验证（交 Codex）

命令与通过标准在 `docs/PROGRESS.md` 2026-11-12「无感重启收尾批」条目。顺序上先跑 `go/build.ps1` 重生 login 的 goctl 包装，再跑 login 单测；冒烟按 §11 起两台 gate，这次不用再手工打开迁移开关。

## 20. 收尾批续落码记录（2026-11-12，未编译、未测试）

负责人 11-12：查出来的问题不等以后、现在修；不等压测、先按最标准的定；服务端与客户端代码都写，协议生成、编译与冒烟交给 Codex，Java 只登记待做。本批把 §5、§6、§8、§10、§11、§13 原先留着的项都做掉。与「无感升级和重启方案」会话的分工：§7.3 的构建偏好与修订说明、§12、§19 是那边写的，本批没有重做；§7.3 里排空脚本自动让修订变一次的代码在本批（文件归本功能）。

### 20.1 改了什么

| 位置 | 改动 |
|---|---|
| `go/player_locator/internal/logic/setsessionlogic.go`、`set_session_changed_test.go`（新） | 「读到之后记录被改过」（版本落后、CAS 冲突）回 gRPC `ABORTED`（`sessionChangedError`）；登录代次不前进等别的拒绝不变。用例 `TestSetSession_RecordAdvancedByDisconnectIsAborted`、`TestSetSession_RewriteOnTheFreshRecordCommits`、`TestSetSession_NonAdvancingLoginGenerationIsNotAborted` |
| `go/login/internal/logic/pkg/sessionmanager/session_manager.go`、`session_manager_set_session_test.go`（新） | `ErrSessionChanged`：`SetSession` 把 `ABORTED` 包成它，别的失败不包。用例 `TestSetSession_AbortedIsReportedAsSessionChanged`、`TestSetSession_OtherFailuresAreNotSessionChanged`、`TestSetSession_SuccessReturnsNil` |
| `go/login/internal/logic/clientplayerlogin/entergamelogic.go`、`metrics.go` | `commitEnterGameSession`：读会话 → 判定 → 铸代 → 提交，遇 `ErrSessionChanged` 重读重做，最多 3 次；`persistEnterGameSession` 遇它直接返回，不探测、不踢；指标 `login_enter_session_commit_redo_total{decision}`（§5） |
| `go/login/internal/logic/clientplayerlogin/entergame_kick_test.go`、`entergame_session_changed_test.go`（新） | 夹具按追加方式扩出可编排的 SetSession 错误与 Reconnect 记录。用例 `TestEnterGame_ReplaceLoginInterruptedByDisconnectRedoesAsShortReconnect`、`TestEnterGame_SessionChangedOnEveryAttemptFailsAfterBoundedRedos`、`TestEnterGame_UnknownSetSessionErrorIsNotRedone` |
| `cpp/nodes/gate/handler/rpc/drain_migration.{h,cpp}`、`cpp/nodes/gate/main.cpp` | `CollectMetrics`、`RegisterMetrics`，gate 启动时登记（§8 的五个指标族） |
| `cpp/nodes/gate/tests/loopback_connection.h`（新）、`gate_redirect_delivery_test.cpp`（新）、`drain_migration_test.cpp`、`cpp/tests/routing_identity_test/routing_identity_test.vcxproj` | 回环连接夹具抽成共用头文件；`GateRedirectDelivery.*` 6 条（§11）；`DrainMigration.CountersAreExportedAsPrometheusMetrics`；测试工程登记被测源码 `gate_redirect_delivery.cpp` 与新测试（仅 Debug\|x64，同 gate 其他被测源码） |
| `cpp/libs/services/scene/player/system/player_scene.{h,cpp}`、`player_lifecycle.cpp`、`connection_lost_hold.h` | `HandleEnterScene` 多一个参数 `EntryClient`：已在目标场景、换了客户端时只重发入场画面（`SendOwnSceneEntry` + `AoiSystem::ResendVisibleTargets`），不再 `LeaveCurrentScene`；同一个客户端仍幂等。`EntrySnapshotResendCount` 供诊断与单测。EnterScene 第 3 步按 `sessionChanged` 传 `kNew` / `kSame`（§13） |
| `cpp/libs/services/scene/spatial/system/aoi.{h,cpp}` | `ResendVisibleTargets`：把观察者当前显示中的目标记成一批「显示」交给原有下发出口（同一出口会给正在走的目标补移动），不改关系、名额与格子 |
| `cpp/tests/cross_zone_test/cross_zone_test.cpp`、`cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp` | `ConnectionLostHoldReentry.*` 4 条改期望，`SceneEntryPermissionTest.*` 新增 3 条，`AoiDeliveryTest.ResendVisibleTargets*` 新增 2 条；清单见 [断线保留](player-connection-lost-hold.md) §10 第 8 条 |
| 客户端 `Assets/Scripts/Game/GameClient.cs` | 转移途中再收到 124 照跟（§10 第 7 条）：124 处理器不再在 `_redirecting` 时返回；每段 RedirectFlow 记一个序号（`_redirectGen`），只有最新的一段结束时才清 `_redirecting` 与 `_redirectBattleId`；角色、zone、战斗号经 `CarryOverOnChainedRedirect` 从上一段带下来 |
| 客户端 `Assets/Tests/EditMode/Net/GameClientReconnectTests.cs` | `RedirectArrivingMidRedirectIsFollowedAndInheritsRoleZoneAndBattle`、`FreshRedirectDoesNotInheritWhatAnEarlierRedirectLeftBehind`、`SupersededRedirectLeavesTheRedirectingFlagToItsSuccessor`、`ChainedRedirectsAreStillCutOffAtTheHopLimit`、`CarryOverOnChainedRedirectPrefersFreshValuesAndOnlyInheritsWhenChained` |
| `tools/scripts/k8s_gate_drain.ps1`、`tests/k8s_gate_drain.tests.ps1` | 同区容量估算（§6，`Get-GateDrainPeerHeadroom`、`Format-GateDrainCapacityLine`，etcd 记录多解析 `max_connections`）；带 `-DeletePod` 时模板没变先让 StatefulSet 修订变一次（§7.3，`Sync-GateRestartRevision`、`Get-GateRevisionAction`、`Get-GateStatefulSetRevision`，`Get-GateStatefulSetPod` 多返回 `Revision`）；用例 5 条新增、2 条按新的读顺序改期望（`-DryRun -DeletePod` 的只读序列多一次 StatefulSet 查询，全流程顺序看最后一次 StatefulSet 查询） |
| `robot/gate_migrate_smoke_scenario.go`（新）、`robot/etc/gate_migrate_smoke.yaml`（新）、`robot/main.go` | 迁移冒烟 `mode: gate-migrate-smoke`（§11）。引用 `shared/gatepick` 算排空标记键名，robot 要重跑 `go mod vendor` |
| 本文、[断线保留](player-connection-lost-hold.md)、[断线重连审计](disconnect-reconnect-audit-20261020.md) §9.1、[组队跟随](team-follow-server-trail.md) §3.6 | 按落码改正文 |

客户端的改动已进客户端仓 11-12 的提交，没有在 Unity 里跑过。

### 20.2 验证（交 Codex，AGENTS §10.1）

命令、顺序与通过标准在 `docs/PROGRESS.md` 2026-11-12「gate 排空迁移收尾批续」条目。要点：

- **第一步仍是 `go/build.ps1` 重生 login 的 goctl 包装**（§18.3），不然 gate 要票一律 UNIMPLEMENTED，冒烟停在 G3-mark。
- 改了 scene 库与 gate：先整解 `msbuild /m:1`，再跑 `routing_identity_test`、`cross_zone_test`、`aoi_delivery_test`（`run_cpp_tests.ps1 -Build` 不重编库）。
- robot 先 `go mod vendor`（新引用 `shared/gatepick`）。
- 排空脚本的契约用例里，gate 标记键形归「Redis 上 Cluster 第 4 批」同日在改，那几条红时先分清是哪一批的。

没有运行结果之前，本节所有内容都只是静态落码。
