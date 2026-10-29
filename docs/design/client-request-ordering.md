# 客户端请求按会话保序:路由服水平扩展下的乱序修复(gate 路由 lane)

**Created:** 2026-10-14
**状态:** 已定稿,分批实施中(进度见 §16)。2026-10-14 用户确认按完整方案实施(「我不怕改动,我要最标准、可以水平扩展的做法」「记录到文档并开始做吧」),§11 的 Q1、Q2 按推荐项执行,Q3 授权已取得。各批**未编译、未验证**,编译与测试由 Codex 按 §13 执行。
**关联:** [client-rpc-router.md](./client-rpc-router.md)(D29–D34)、[client-access-band-routing.md](./client-access-band-routing.md)(D51–D64,阶段 0b/0d/0g)、[grpc-muduo-transport.md](./grpc-muduo-transport.md)、[microservice-zone-contract-20260914.md](./microservice-zone-contract-20260914.md)(预算链)、[team-system.md](./team-system.md)(expected_team_id、epoch/version)、[friend-client-spec-20260920.md](./friend-client-spec-20260920.md)(客户端单飞约定)、[scene-enter-session-fence.md](./scene-enter-session-fence.md)(登录代次 login_generation,与 §6.2 互补)、参考项目 `../xuanming-server/docs/design/protocol-ordering-rules.md`、Java 版 `D:\luyuan\xuanming-server-mmo` 的 `game-gate` 与 `PARITY.md`。

**证据标注:**
- 【码】代码或配置事实,写成 `路径:行号`,行号以 2026-10-14 当时的工作区为准(main 上有别的会话的未提交改动)。
- 【文】文档里的说法。
- 【推】静态阅读得出的推断,未运行验证。

**产出方式:** 两轮多代理工作流。第一轮 7 路只读调研(gate、路由服、login/scene_manager/match、社交服务、scene TCP 与推送、Java 版、参考规范);第二轮起草 1 份设计,做了 1 路对抗评审(替代方案视角)。另外三路评审(故障边界、业务栅栏、兼容上线)因账号额度中断,改由主会话逐条读代码复核,结果记在 §15。

---

## 0. 结论速览

1. **乱序是真的,而且与路由服副本数无关。** 这条链上有四处会让同一玩家的请求并发执行:gate 每条请求随机选路由服;路由服每条请求随机选业务实例;grpc-go 服务端每个 stream 起一个 goroutine;go-zero 超时拦截器再起一个 goroutine 跑 handler。所以即使全集群只有 1 个路由服、1 个 match,`JoinQueue → CancelQueue` 照样可能执行成 `Cancel → Join`,玩家以为取消了,实际还在队列里(§1)。
2. **粘性路由、一致性哈希都修不了。** 它们只能保证落到同一处,保证不了串行。按玩家加锁也只能保证互斥,保证不了先发的先拿到锁。
3. **推荐方案:在 gate 上按会话单飞排队。** 每个会话开一条「路由 lane」:经路由服的 gRPC 类请求按 gate 收包顺序逐条发出,上一条完成(成功回包、失败回调、本地截止到期,三者之一;2026-10-16 起路由服节点摘除经 `~Channel` 的 UNAVAILABLE 走失败回调,不再单列,见 §4.5)之后才发下一条。不同玩家之间照样并行;路由服保持无状态、继续随机选副本(D29 不变);业务实例也继续随机。只有 gate 看得到客户端的收包顺序,也只有 gate 持有会话生命周期,所以保序放在这里(O1、O2)。
4. **第一阶段每个会话只开一条 lane(全序),不按服务分域。** 分域需要人工标注哪些方法跨域、要做屏障,新功能忘了标就会静默乱序,与仓库「约定拦不住不知情的新服务」的教训相同(AGENTS §7.5)。单 lane 默认全序,自动覆盖 team↔match、帮会历练↔match、登录生命周期↔其他服务这些跨服务因果对,gate 也不需要认识任何 Go 方法号,与号段终态 D51 一致。代价是同一会话内的读请求也被串行:健康时每条多等一个毫秒级往返;某个服务降级时,排队超过等待上限的请求会快速回 1003,不会拖到客户端超时。以后要放宽,也只能凭压测数据、通过生成的方法级 option 按需放开,默认值始终是全序(O3、§4.12)。
5. **gate 排队只能管住「观察到完成」之前的顺序,另外两层必须同批补齐。**
   - **预算链与提交点:** go-zero 超时后 handler 不会停。所以要求目标服务超时 < 路由服转发超时 < gate deadline,业务把请求 ctx 传进存储调用,提交前检查 ctx。满足这些,gate 看到超时的时候,目标已经不会再发出新的写。要修两处:login 的 `Timeout: 10000`、match 的 `Timeout: 5000` 都超出了路由服 5s 的转发预算;match 的票据读写用的是 `context.Background()`(§5、§6.1)。
   - **业务栅栏:** 用来兜跨会话和「最后一条已发出的写晚落地」两类情况。login 加会话栅栏,同时修掉 Disconnect 抢跑 Login,以及 EnterGame 后台链与断线之间的竞态(后者即使顺序正确也会出问题);match 的 JoinQueue 复核会话;guild 写请求补 `expected_guild_id`(§6)。
6. **Java 版已经有这套模型的大半。** Java gate 对每个会话、每个后端域只放一个在途调用【码】`game-gate/.../ClientDispatcher.java:413-439`。要对齐的是:lane 粒度(Java 是按域分,本文建议全序)、排队等待上限、超限行为(Java 是满 64 断连)、login 失败回执带 id、把截止下传给后端,以及跨会话复核(§10)。
7. **需要你拍板的有 4 项**(§11):lane 粒度与 Java 对齐方式;预算链的修法(推荐把 login 和 match 的超时降到 4000);实施授权与批次顺序;客户端行为确认(由客户端负责人回答)。

---

## 1. 问题与根因

### 1.1 现状链路

```
客户端 ──TCP──▶ gate(单 EventLoop,入口对单会话严格 FIFO)
                 │ 每条请求 PickRandomNode(ClientRpcRouter)               ← 随机 ①
                 ▼
            路由服副本 R1 / R2 …(每请求一个 goroutine)                   ← 并发 ②
                 │ 每条请求 pickTarget:login 同 zone 随机,其余全局随机   ← 随机 ③
                 ▼
            业务实例 M1 / M2 …(grpc-go 每 stream 一个 goroutine;
                               go-zero 超时拦截器再起一个 goroutine)      ← 并发 ④
```

- gate 只有一个 EventLoop,同一连接的帧在 `ProtobufCodec::onMessage` 里按到达顺序同步派发【码】`cpp/libs/engine/core/network/codec/codec.cpp:152-197`、`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:542-546`。**乱序全部发生在 gate 出口之后。**
- ① gate 每条请求随机挑路由服副本【码】`client_message_processor.cpp:53-81,140-169`。
- ③ 路由服每条请求单独选实例,不看会话【码】`go/client_rpc_router/internal/logic/forwardlogic.go:148,196-201`、`internal/discovery/node_watcher.go:251-284`。
- ②④ grpc-go 服务端每个 stream 用 `go f()` 起一个 goroutine【码】`google.golang.org/grpc@v1.79.3/server.go:1057-1075`;go-zero 的 `UnaryTimeoutInterceptor` 再起一个 goroutine 跑 handler【码】`go-zero@v1.10.0/zrpc/internal/serverinterceptors/timeoutinterceptor.go:28-74`。
- gate 没有任何在途请求表,发出后不记账,回包到达也不认领【码】`cpp/libs/services/gate/session/comp/session_info_comp.h:12-120`。

### 1.2 为什么粘性 / 单副本 / 加锁都不够

| 手段 | 保证了什么 | 保证不了什么 |
|---|---|---|
| gate 按会话粘到同一路由服 | 落到同一路由服 | 同一 HTTP/2 连接上的两个 unary 是两个 stream,服务端并发执行(④) |
| 路由服按玩家一致性哈希选业务实例 | 落到同一实例 | 同上;而且业务的权威状态本来就在共享 Redis / MySQL,实例亲和不是需求【码】`go/match/internal/logic/queue.go:220-230` |
| 按玩家加分布式锁 | 互斥 | 先发的不一定先拿到锁;login 的 player_locker 拿不到锁就立即回错,不排队【码】`go/login/internal/logic/clientplayerlogin/entergamelogic.go:97-114` |
| 客户端回显 `ClientRequest.id` | 回包对得上号 | 服务端没有任何地方按它排序;它由客户端自定,可能重复或为 0 |

### 1.3 已确认的乱序后果(节选)

| 依赖对 | 乱序后的最终状态 | 现有兜底 | 证据 |
|---|---|---|---|
| JoinQueue → CancelQueue(不等回包、不带票号) | Cancel 找不到票回成功,Join 随后入队,玩家仍在匹配队列,可能被凑单拉进战斗 | 只有带 `queue_ticket` 时才安全,而带票号意味着已经等过回包 | 【码】`go/match/internal/logic/cancelqueuelogic.go:38-57` |
| RespondInvite(accept B) → LeaveTeam(expected=B) | Leave 先执行时未绑定,回成功;随后入队成功,玩家留在 B 队 | 无 | 【码】`go/match/internal/team/service.go:303-307,636-644` |
| AcceptFriend → RemoveFriend;Block → Unblock | 最终仍是好友 / 仍被拉黑,两条都回成功 | 无 | 【码】`go/friend/internal/logic/friend_logic.go:415-424`、`internal/data/block_repo.go:36-50` |
| ApplyJoinGuild → CancelGuildApplication | 撤回回 NotFound,申请随后生效,审批后玩家违背意图进帮 | 申请有过期时间 | 【码】`go/guild/internal/data/guild_manage_repo.go:1937-1955` |
| CancelQueue(match)→ RespondGuildTrialInvite(accept,凑齐开战,guild) | accept 先执行时,开战预检看到 queued 票,整个历练房间以 MEMBER_NOT_READY 结束 | 无 | 【码】`go/guild/internal/logic/activity_trial.go:594-619`、`go/match/internal/logic/activitybattlelogic.go:105-117` |
| CancelQueue(match)→ StartTeamMatch(team) | 预检看到队长自己的 queued 票,回「有成员未准备好」 | 无 | 【码】`go/match/internal/team/service.go:420-462` |
| Login → gate 合成的 Disconnect(客户端登录后立刻断线) | Disconnect 先执行,Login 再写入 login_session 与设备集,残留 30 分钟,可能误触设备数上限 | 只有 TTL | 【码】`go/login/internal/logic/clientplayerlogin/loginlogic.go:150-176`、`disconnectlogic.go:25-38` |
| EnterGame(已受理,后台链未完成)→ 断线 | Disconnect 带 player_id=0 早退;后台链随后写入 ONLINE,没有租约,玩家卡在线。**顺序正确也会发生** | 无 | 【码】`entergamelogic.go:238-241,705-722`、`session_lifecycle.go:117-120` |
| 有副作用的请求超过 5s | 路由服按 5s 回 kServiceUnavailable,目标 handler 继续跑完并提交;客户端接着发下一条或重发 | 部分服务有幂等键 | 【码】`forwardlogic.go:165-184`、`timeoutinterceptor.go:57-72` |

### 1.4 为什么保序点放在 gate

- 只有 gate 看得到客户端的原始收包顺序,也只有 gate 持有会话生命周期(建连、顶号、断线、重定向)。
- gate 是单 EventLoop,会话表是 thread_local,gRPC 完成回调也由主 loop 上的定时器轮询 CQ 执行【码】`cpp/libs/engine/core/node/system/etcd/etcd_service.cpp:33-37`(2026-10-16 订正:gRPC 传输第 3 批后改由 `muduo_grpc::Channel` 在 loop 上经 queueInLoop 恰好一次投递,CQ 轮询已删)。队列不需要加锁,也不需要新线程。
- 路由服按 D29 无状态,可以随时重启【文】`client-rpc-router.md:27`。要在路由服里排队,必须先把会话粘到固定副本、处理成员变化时的排空,而路由服被黑洞时,粘性会让一个会话的请求全部卡住。
- Java 版已经验证了这个位置:gate 按会话串行发出,后端契约直接写着「同一会话的调用由 gate 串行发出」【码】`game-api/src/main/java/com/game/api/ClientMessageService.java:30`。

---

## 2. 方案总览:三层

```
                ┌──────────────── 第 1 层:gate 路由 lane(O1–O9)────────────────┐
客户端帧 ─FIFO─▶│ 闸门(会话/验签/白名单/体积/限流/GM/battle)                        │
                │   └─ PROTOCOL_GRPC 且路由模式 → 会话 lane 准入                     │
                │        queued: [r3, r4] ← 有界(32)、带入队时刻与身份快照           │
                │        inFlight: r2(gate_call_seq=s2)── 只放 1 条               │
                │   完成 = 成功桥 / 失败桥(含节点摘除)/ 本地截止 ─ 恰好一次 → 发下一条│
                └───────────────┬──────────────────────────────────────────────────┘
                                │ x-session-detail-bin{…, incarnation, gate_call_seq}
                                ▼
                     路由服(无状态、随机,原值回写会话头)
                                ▼
                ┌──────── 第 2 层:预算链 + 提交点(O10、O11)────────┐
                │ 目标 Timeout + 1s ≤ ForwardTimeout < 路由服 Timeout   │
                │ 请求 ctx 传进 Redis/MySQL;提交前检查 ctx            │
                └──────────────────────┬──────────────────────────────┘
                                       ▼
                ┌──────── 第 3 层:业务栅栏(O12–O14)────────┐
                │ login 会话栅栏(双边握手)                       │
                │ match JoinQueue 复核会话;guild expected_guild_id │
                │ 现有:team expected_team_id + ver CAS、           │
                │       player_locator session+version CAS、       │
                │       chat request_id、match ticket CAS          │
                └───────────────────────────────────────────────────┘
```

三层各管一段,缺一不可:

- 第 1 层保证:同一会话的请求按收包顺序发出,前一条观察到完成后才发下一条。
- 第 2 层保证:「观察到完成」(包括超时)时,目标已经不会再发出新的写。
- 第 3 层兜住剩下的两类:跨会话(重连、换 gate),以及截止那一刻已经发出、但晚落地的最后一条写。

---

## 3. 决策

| # | 决策 | 理由 |
|---|---|---|
| O1 | 保序点放在 gate;路由服无状态、随机选副本;业务实例随机 | §1.4;D29、D33 不变 |
| O2 | 只在**路由模式**下、只对 **PROTOCOL_GRPC** 消息启用 lane。TCP(scene)路径不进 lane;直连模式不启用;gate 合成的 Disconnect 不进 lane | TCP 路径本身单连接 FIFO,两端单线程,已经有序;而且返回 Empty 的方法(MoveStart/MoveSync/MoveStop)不回包,排队会卡死【码】`proto/scene/player_movement.proto:105-107`。直连模式下 match 不回写会话头,无法认领(O7)。Disconnect 见 O8 |
| O3 | **第一阶段每个会话一条 lane,全序**;放宽只能凭压测数据、通过生成的方法级 option 显式放开,默认值始终是全序 | §0 第 4 条;fail-closed;不需要方法知识,与 D51 兼容 |
| O4 | lane 同时只放 1 条在途;完成来源有 4 个(2026-10-16 起为 3 个:节点摘除并入失败回调,§4.5),每个请求恰好得到一个回执,晚到的完成一律丢弃 | 认领恰好一次,与 Dubbo 超时后丢弃应答同语义【文】Java `docs/design/architecture.md:101` |
| O5 | 认领用 gate 自己的令牌:SessionDetails 新增 `session_incarnation = 7`、`gate_call_seq = 8`,在**准入时**按收包顺序分配;路由服原值回写,不改路由服代码 | 不信任客户端给的 id;incarnation 防 session_id 复用串号(F47);准入时分配的号以后也可以给业务做水位栅栏 |
| O6 | 有界:排队 + 在途 ≤ 32,超限回带 id 的 1008,不计非法包;排队等待超过 `kMaxLaneWait`(5s)不发出,回带 id 的 1003;所有 gate 级失败回执都带请求 id | 防内存放大与客户端超时;合并 band-routing D57;完成阶段 0d |
| O7 | 直连模式维持现状,在启动日志里注明「直连模式不保序」,交由 D61 删除直连路径时一并处理 | 直连模式下 match 不回写会话头【码】`go/match/match_service.go:346-350`,成功桥找不到会话就直接返回【码】`cpp/nodes/gate/main.cpp:292-293`,今天直连模式下 match 的回包本来就全部丢失 |
| O8 | 断线:丢弃未发出的排队项,注销在途项(晚到的回包丢弃);gate 合成的 Disconnect 立即发出,由 login 会话栅栏兜底 | EnterGame 回包只表示已受理,后台链还在跑,把 Disconnect 排在后面也挡不住竞态;跨 gate 重连时没有共享的 lane |
| O9 | 重定向(RedirectToGate):会话标记 redirected,丢弃排队项,之后新到的路由请求回带 id 的 1003;在途回包照常下发 | 与 Java 对齐【码】`ClientDispatcher.java:433-434,1058-1062`;防止旧连接的排队项与新 gate 上的请求交错 |
| O10 | 预算链:目标 `Timeout + 1000 ≤ ForwardTimeoutMs`;推荐把 login、match 的 Timeout 降到 4000;新增不变式 `gate deadline(router) + kMaxLaneWait + 1000 ≤ 客户端超时(15000)`;部署门禁核对 | §5 |
| O11 | 客户端请求路径上的存储调用一律传请求 ctx,提交前检查 ctx;补偿与回滚用独立 ctx | §6.1;先例 team【码】`go/match/internal/team/store.go:226-229` |
| O12 | login 会话栅栏:键 `(gate_instance_id, session_incarnation)`,单段 Lua 双边握手,TTL = chainBudget + 1min | §6.2 |
| O13 | match JoinQueue 提交前复核会话仍是当前会话(不原子,缩小跨会话窗口) | §6.3 |
| O14 | guild 的 Leave / Disband / Kick / Transfer / SetRole 补 `expected_guild_id`,0 表示旧客户端 | §6.4;先例 team 的 expected_team_id |

---

## 4. gate 路由 lane 详细设计

### 4.1 挂载点

当前闸门顺序【码】`client_message_processor.cpp:839-969`:

```
会话存在 → verified → IsClientMessageId 白名单 → 体积 + 限流 → GM 闸 → battle 拒绝 → 按 protocol 分派
```

lane 准入放在 `PROTOCOL_GRPC` 分支里、路由模式下,取代现在直接调用 `HandleRouterForward` 的做法(`:956-964`)。

- 限流、体积、白名单、GM、battle 这些拒绝仍然排在 lane 之前,立即回执,不占 lane 名额。它们的回执可能先于更早排队请求的回包到达客户端。这是允许的:本文保证的是**执行顺序**,不保证拒绝回执的到达顺序;拒绝回执带 id,客户端能对上号。

> 2026-10-15 订正(用户拍板两版同批):体积、限流原本就是带 id 的信封;GM 闸(1006)与 battle 拒绝(1003)由 23 号 tip 改为带 id 与原 message_id 的信封,Java 版同批修订 scene-battle-spec D12。**白名单拒绝有意不回执**:它计非法包,回执会把「无凭证刷不存在的消息号」变成回包放大面,所以不在「拒绝回执带 id」之列。仍是 23 号 tip、不带 id 的同类 gate 级失败本轮未改,完整清单(复核核实):两种模式都走的 `HandleTcpNodeMessage` 两处(scene 未就绪、RpcClient 未挂);只在直连模式发生的三处(`HandleGrpcNodeMessage` 请求体解析失败 `kRequestMessageParseError`、找不到业务节点,以及 `rpc_reply_bridge.cpp` 里 lane 不认领时的 gRPC 失败桥),后三处随 O7 / D61 删除直连路径时一并处理。`scene_entry_dispatch.cpp` 的 `kEnterSceneFailed` 是推送、不是请求回执,不在此列。**2026-10-15 稍后**:上列两种模式的 2 处与直连模式的前 2 处(请求体解析失败、找不到业务节点)已改为带请求 id 与原 message_id 的信封(`client_message_processor.cpp` 的 `SendRequestErrorEnvelope`,只用本次收包的 conn 与请求);仍是 23 号 tip 的只剩 `rpc_reply_bridge.cpp` 里 lane 不认领时的 gRPC 失败桥——失败回调拿不到客户端请求 id(SessionDetails 无此字段,发出的 metadata 只有会话头,生成客户端保存的是业务请求体),这一支只在直连模式与 seq = 0 的断线通知出现,随 O7 / D61 删直连路径处理。Java 版同批:「玩家不在场景」「未接入的域」改信封,并对齐「scene 链路不受理这一帧」(原为不回任何回执)也回信封 1003。
>
> 2026-10-19 补注:上段末尾「仍是 23 号 tip 的只剩 `rpc_reply_bridge.cpp` 里 lane 不认领时的 gRPC 失败桥——失败回调拿不到客户端请求 id」的前提已随 2026-10-17 直连改为每次调用回调而不成立。直连模式的每次调用 lambda 现在捕获 `ClientRequest.id` 与 message_id,成功回包回显请求号(此前包一层时不带 id,且按响应类型反查 message_id),失败回带请求 id 的 1003 信封;路由模式不变。客户端据此删掉按 message_id 先进先出的兜底,只按 id 认领。见 [client-rpc-callback.md](client-rpc-callback.md) §3。
- `loginStarted` 改在**发出时**置位(现在路由模式下就是发出之后置位【码】`client_message_processor.cpp:821-826`)。排队中、断线时作废的 Login 从没到达 login,不需要通知,这样也不会被「发 Login 后立刻断开」用来放大断线 RPC(`:439-451` 的注释)。

### 4.2 数据结构

全部在 gate 主 loop 线程上读写,不加锁、不新增线程。

| 状态 | 位置 | 说明 |
|---|---|---|
| `uint64 incarnation` | `SessionInfo` 新字段 | 建连 emplace 时从 SessionManager 的进程内单调计数器取,从 1 开始【码】`client_message_processor.cpp:634-654`、`session_manager.h:8-23` |
| `RouterLane lane` | `SessionInfo` 新成员 | `{deque<Item> queued; uint64 inFlightSeq = 0; bool redirected = false;}`,`Item = {shared_ptr<ClientRequest> request; SessionDetails snapshot; uint64 callSeq; steady_clock::time_point enqueuedAt;}`。会话 erase 即取消,先例是 `pendingSceneEntry`【码】`session_info_comp.h:73-80`。codec 每帧 `New()` 一个新对象【码】`codec.cpp:211,235`,持有 shared_ptr 是安全的,它不是连接对象 |
| 在途索引 `unordered_map<uint64 seq, InflightCall>` | 新文件 `cpp/nodes/gate/handler/rpc/client_request_lane.{h,cpp}`,thread_local | `{sessionId, incarnation, clientRequestId, messageId, expiresAt}`;用 `extract(seq)` 保证恰好一次(2026-10-16 删去 `routerEntity`:节点摘除不再按实体批量摘,见 §4.5) |
| 过期堆 `priority_queue<(expiresAt, seq)>` | 同上 | 惰性删除:弹出时 seq 已不在索引里就跳过 |
| `gate_call_seq` 发号器 | 同上 | 进程内单调 uint64;0 保留,表示「不受 lane 管理的调用」 |

纯逻辑核心(准入、取下一条、完成核对、过期判定)放在只依赖 STL 的头文件 `cpp/libs/services/gate/session/comp/router_lane.h`,照 `cpp/nodes/gate/tests/gate_security_test.cpp` 的方式脱离引擎单独编译测试。

### 4.3 准入(Admit)

```
if lane.redirected                         → 回 {id, message_id, error=1003},return
if lane.queued.size() + (inFlightSeq?1:0) ≥ kMaxLaneDepth(32)
                                           → 回 {id, message_id, error=1008},不计非法包,return
item.snapshot  = BuildSessionDetails(sessionId, session.playerId, &session)
                 并把 kInvalidGuid 归一成 0(阶段 0b)
                 再写 session_incarnation = session.incarnation
item.callSeq   = ++gCallSeq;写进 snapshot.gate_call_seq
item.enqueuedAt = steady_clock::now()
lane.queued.push_back(item);Pump(session)
```

- **身份取入队时刻的快照。** 它反映客户端发这条请求时的身份,与 Java 一致【码】`ClientSession.java:212-217`、`ClientDispatcher.java:427`,也符合 D-9「身份只从会话取」。排队期间 BindSession 才到达时,请求按未登录被拒。今天这类请求带的是 UINT64_MAX,会被各服务当成真实玩家【码】`go/match/internal/logic/joinqueuelogic.go:49-51`;归一成 0 之后是 fail-closed,只会更好。

### 4.4 发出(Pump)

```
while lane.inFlightSeq == 0 && !lane.queued.empty():     // 循环 ≤ 32 次
    item = pop_front
    if now - item.enqueuedAt > kMaxLaneWait(5s): 回 1003(带 id),计 expired_in_queue;continue
    router = PickRandomNode(ClientRpcRouterNodeService)
    if !router: 回 1003(带 id);continue
    SendClientRpcRouterForward(…, metadata = item.snapshot)
    lane.inFlightSeq = item.callSeq
    index[item.callSeq] = {…,
                           expiresAt = now + grpc_call_deadline::Get(router 类型) + 1000ms}
    heap.push(expiresAt, seq)
    if message_id == Login: session.loginStarted = true
```

- 排队超时回的 1003 明确表示「没有发出」,客户端重试是安全的。
- `grpc_call_deadline::Get` 读实际生效的 deadline【码】`grpc_call_deadline.h:34-35`,不另写常数;本地与 K8s 都是 8000【码】`bin/etc/base_deploy_config.yaml:230`、`tools/scripts/k8s_deploy.ps1:2175-2176`。多出的 1000ms 用来覆盖 5ms 的 CQ 轮询和 loop 积压(2026-10-16 订正:CQ 轮询已随 gRPC 传输第 3 批删除,现在只覆盖 loop 积压)。

### 4.5 认领(Complete)

> **2026-10-17 契约勘误**：下表入口随删除全局处理器 A–C 更新；主程序不再安装全局回包函数。路由 lane 的代次、队头、恰好一次和放行规则不变，见 `grpc-muduo-transport.md` §16.1。

| 来源 | 入口 | 拿 seq 的方式 |
|---|---|---|
| 成功回包 | 2026-10-17：直连 `ForwardDirectGrpcCallResult`、路由 `router_muduo_forward.cpp` 的 `SendForward` 完成回调 → `gate_rpc_reply_bridge::DeliverReply` → `gate_lane::OnRouterReply` | 直连用发送时捕获的 SessionDetails；路由解响应 initial metadata 里原值回显的 `x-session-detail-bin` |
| gRPC 失败 | 2026-10-17：直连 `ForwardDirectGrpcCallResult`、路由 `SendForward` 完成回调 → `gate_rpc_reply_bridge::NotifyCallFailure` → `gate_lane::OnRouterCallFailed` | 两种模式都用发送时捕获的 SessionDetails 原始字节；Disconnect 失败仅采样日志，不进客户端失败桥 |
| 路由服节点摘除 | 不单独处理(2026-10-16 起) | 走上一行「gRPC 失败」:gRPC 传输第 3 批起 node.cpp 先 `remove<MuduoGrpcChannelPtr>`,`~Channel` 把在途调用以 UNAVAILABLE 排进下一轮,再触发 OnNodeRemoveEvent、DestroyEntity;放行下一条时 `PickRandomNode` 要求实体挂着通道,不会再选中被摘节点。原设计按 `routerEntity` 遍历摘除,前提是官方 CQ 析构后在途调用永远不会再有回调(F29),该前提已不成立,`OnRouterNodeRemoved` 与 `InflightIndex::ExtractIf` 已删(ECS 审查 W1-45);它还会让每个被摘调用在 `late_dropped` 里多计一次 |
| 本地截止 | GateRuntimeContext 上新增 `TimerTaskComp laneExpiryTimer`,`RunEvery(0.25s)`,回调不捕获对象 | 弹出过期堆。**必须主动扫描**:队头卡住时客户端在等回包,不会再发新请求,纯惰性清理永远触发不到。先例是 `sceneEntryRetryTimer`【码】`main.cpp:141-144,396-398` |

统一处理:

```
call = index.extract(seq);  if !call: late_dropped++,丢弃这次回包;return
it = sessions.find(call.sessionId)
if it 不存在 || it.incarnation != call.incarnation: incarnation_mismatch++,丢弃;return
if it.lane.inFlightSeq != seq: lane_mismatch++,丢弃;return      // 防御性,正常不可达
it.lane.inFlightSeq = 0
成功:原样下发 MessageContent(路由服已回显 id);失败(含节点摘除) / 过期:下发 {id, message_id, error=1003}
conn = it.conn.lock();有连接才写
Pump(*it)
```

- `gate_call_seq == 0` 的成功回包只可能来自 gate 合成的 Disconnect,而那时会话通常已经 erase,保留旧行为即可(找不到会话就丢弃)。
- 失败桥改为回带 id 的信封,取代现在不带 id 的 `SendTipToClient(kServiceUnavailable)`【码】`main.cpp:350`,也就是阶段 0d。
- 本地截止与 gRPC deadline 的先后:正常情况下失败桥先在约 8s 时触发,本地截止(约 9s)只兜「回调丢失」这一种情况:F29,以及 CQ 轮询遇到 `!ok` 时直接 `return`、跳过本轮剩余事件【码】`cpp/generated/grpc_client/grpc_init_client.cpp:365-368`。
  2026-10-16 订正:这两个来源已随 gRPC 传输第 3 批(官方 CQ 删除)消失,`muduo_grpc::Channel` 对每个调用恰好回调一次;本地截止现在兜的是「完成没被 lane 认领」:传输层「恰好一次」契约出缺陷,或路由服成功回包缺 `x-session-detail-bin` 回显 / 回显解析失败 / 不带 `gate_call_seq`(版本不符,`router_muduo_forward.cpp` 只打采样 WARN 后丢弃)。正常运行时 `router_lane_expired` 不应增长,增长时先对照「no x-session-detail-bin echo」WARN。

### 4.6 断线、顶号、重定向

- **断线**(包括顶号:KickPlayerEvent 之后 shutdown,走同一断线路径【码】`gate_event_handler.cpp:107-139`):在 `sessions.erase` 之前【码】`client_message_processor.cpp:484`,把 `lane.inFlightSeq` 从在途索引里删掉,晚到的完成一律丢弃;排队项随会话析构,不发出,计 `dropped_on_disconnect`;不等在途调用。gate 合成的 Disconnect 照旧立即发出(O8)。
- **重定向**:在 `RedirectToGateEventHandler` 里置 `lane.redirected = true` 并清空排队项【码】`gate_event_handler.cpp:230-267`(现在只推一条通知)。

### 4.7 回执与码表

| 情形 | 回执 | 计非法包 | 码(导表生成,不新增) |
|---|---|---|---|
| lane 满(排队 + 在途 ≥ 32) | `MessageContent{id, message_id, error}` | 否 | `kRateLimitExceeded = 1008`【码】`generated/code/proto/tip/common_error_tip.proto:28` |
| 排队超时、无路由服、gRPC 失败、节点摘除、本地截止、已重定向 | 同上 | 否 | `kServiceUnavailable = 1003`【码】`common_error_tip.proto:18`(fault=1) |
| 业务回包与路由服拒绝 | 路由服信封原样下发(已带 id) | — | — |
| GM 闸拒绝(lane 之前,2026-10-15 起) | `MessageContent{id, message_id, error}`(原为 23 号 tip) | 是 | `kFeatureUnavailable = 1006` |
| battle 拒绝(lane 之前,2026-10-15 起) | 同上(原为 23 号 tip) | 否 | `kServiceUnavailable = 1003` |
| scene 未就绪 / RpcClient 未挂(TCP 路径,两种模式,2026-10-15 起) | 同上(原为 23 号 tip);应答类型为 Empty 的移动号(MoveStart / MoveSync / MoveStop)同样回 | 否 | `kServiceUnavailable = 1003` |
| 直连模式请求体解析失败 / 找不到业务节点(2026-10-15 起) | 同上(原为 23 号 tip) | 否 | `kRequestMessageParseError` / `kServiceUnavailable = 1003` |

**客户端可见的变化**:gate 级失败从「23 号 tip、不带 id」改成「带请求 id 和原 message_id 的错误信封」;新增 1008 拒绝;同一会话的路由类回包按请求顺序到达。第一项与路由服现有的拒绝信封形状相同,客户端本来就要处理【推】;robot 按 message_id 认领,也兼容【码】`robot/chat_smoke_scenario.go:367-368,402`。按 AGENTS §12,这一批要与 Java 同批对齐(§10)。

> 2026-10-15 评审订正:上面「robot 也兼容」只对 chat 冒烟成立。登录流程的 `robot/login.go` `sendAndRecvTimeout` 当时只解 body、不看信封错误,空 body 会被当成「成功、字段全默认」(Login 看成没有角色去建角,EnterGame 看成已进游戏去等 79)。路由服的上游错误信封早就是这个形状,所以这是既有缺陷,lane 只是让它更常见。已同批改为信封错误非 0 即返回 `envelopeError`。Java 版的 login 契约文档(`docs/reference/mmorpg-client-contract-login.md`「Java 必须做到」第 4 条)与 `ClientDispatcher` 类注释同批改成「gate 级失败一律带 id 的信封」。

### 4.8 SessionDetails 字段

【码】`proto/common/base/session.proto`:已占用 1–6。

```proto
  // gate 进程内单调,会话存续期间不变;0 = 旧版 gate(无栅栏)。与 gate_instance_id 合起来全局唯一。
  uint64 session_incarnation = 7;   // client-access-band-routing.md D60 预订的号
  // gate 进程内单调,准入时按收包顺序分配;0 = 不受 lane 管理(gate 合成的通知)。
  // 业务服务不得把它当幂等键(它按调用编号,不按客户端意图编号);可以作为 (incarnation, seq) 水位栅栏。
  uint64 gate_call_seq = 8;
```

- 这是内部契约,不是客户端可见契约。路由服不解析、按原值回写,无需改代码;login 拦截器重新 Marshal 后回写,protobuf-go 会保留未知字段【码】`go/shared/callerauth/interceptor.go:110-119`【推】。
- 改 proto 后按 AGENTS §4 重新生成,重编受影响的 C++ / Go 服务;Java 侧按其 ContractSync 规则同步。

### 4.9 生命周期合规(AGENTS §11.7)

排队项、在途索引、定时器闭包都不持有 `TcpConnectionPtr`,也不绑定裸 this。每次下发都先 `find(sessionId)`、核对 incarnation,再 `conn.lock()`,先例见 `main.cpp:294-297`。定时器回调不捕获对象,靠 TimerTaskComp 的存活令牌【码】`timer_task_comp.h:70-80`。

### 4.10 观测

gate 没有 Prometheus 出口。仿照 scene_entry 的汇总日志写法【码】`cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp:414`,每 30s 打一行 `[lane]` 汇总:`admitted / sent / replied / failed / expired / expired_in_queue / rejected_full / rejected_redirected / late_dropped / incarnation_mismatch / dropped_on_disconnect`,外加当前在途数和最大队长。(2026-10-16:`node_removed` 已删,路由服摘除计入 `failed`,见 §4.5。)本地截止进 error_reporter。不使用 player_id、session_id 或原始 message_id 作维度(AGENTS §9)。

> 2026-10-15 订正(与实现对齐,以 `client_request_lane.cpp` 的 `LogSummaryIfDue` 为准):`dropped_on_disconnect` 实际叫 `dropped_on_close`,它同时覆盖断线与租约到期摘无连接会话两条路径;另有 `no_router`(没有可用路由服时回 1003)、`dropped_on_redirect`(重定向时丢弃的排队项)两个计数,以及 `in_flight`、`expiry_heap`、`max_depth_seen` 三个观测量。§4.5 伪码里的 `lane_mismatch++` 实现为 `LOG_ERROR`(防御性,正常不可达),不单独计数。`incarnation_mismatch` 自 2026-10-15 起同时包含 scene TCP(RPC0)回包路径(`FindLiveSession` 被两条路径共用)。

### 4.11 性能

- 每条请求多一次 deque push/pop、一次哈希表插入与 extract、一次堆操作。在途索引 ≤ 会话数,每会话排队 ≤ 32。过期堆是惰性删除(§4.2),完成时不从堆里删,规模约为「路由类请求速率 ×(deadline + 1s)」,不是「≤ 会话数」(2026-10-15 订正,见 `inflight_index.h` 文件头)。
- 健康时,同一会话内的并发请求从「并行 1 个往返」变成「N 个往返之和」,单个往返是毫秒级。打开帮会面板时的并发查询【文】`client-access-band-routing.md:350` 会受影响。按 AGENTS §6,上线前要压测对比 prev-summary;没有对比表之前不宣称任何性能结论。

### 4.12 放宽路径(第三阶段,凭数据)

如果压测证明全序的队头阻塞代价不可接受,再引入方法级 option(例如 `OptionClientOrdering { SESSION = 0(默认); DOMAIN; READ_CONCURRENT }`,号避开 410000 / 410001 / 400002),由生成器用 protowire 读取【码】`tools/proto_generator/protogen/internal/prototools/option/value.go:10-83`,产出 lane 分类表。默认值永远是 SESSION:新方法不标注就是全序,忘了标只会慢,不会乱。这一步与 band-routing D51「gate 不认识 Go 方法号」冲突,届时要修订 D51(例如把分类表作为 gate 加载的数据文件)。

---

## 5. 预算链

### 5.1 现状

| 环节 | 值 | 证据 |
|---|---|---|
| 客户端超时 | 15000 | 【文】`friend-client-spec-20260920.md:341-346` |
| gate→路由服 deadline | 8000(本地与 K8s 相同) | 【码】`bin/etc/base_deploy_config.yaml:230`、`tools/scripts/k8s_deploy.ps1:2175-2176` |
| 路由服 zrpc Timeout / ForwardTimeoutMs | 6000 / 5000(所有目标同一个值) | 【码】`go/client_rpc_router/etc/client_rpc_router.yaml:5,32` |
| chat / friend / guild / trade | 4000,各自 Validate 强制 | 【码】`go/chat/internal/config/config.go:113`、`go/friend/internal/config/config.go:281`、`go/guild/internal/config/config.go:430` |
| match(含 team) | **5000 ✗**;team 每个方法自设 3500 ✓ | 【码】`go/match/etc/match_service.yaml:3`、`go/match/internal/team/server.go:21-24` |
| login | **10000 ✗**(注释按直连模式推导:CreatePlayer 持锁后串行三次跨进程调用,各 3s) | 【码】`go/login/etc/login.yaml:7-13` |

**路由模式下 login 的 10s 预算实际不生效。** 路由服用 `context.WithTimeout(l.ctx, ForwardTimeout)` 调目标【码】`forwardlogic.go:165`,deadline 经 gRPC 传播,go-zero 超时拦截器在入站 ctx 上再派生【码】`timeoutinterceptor.go:34`。所以 login handler 实际拿到的预算是 min(10000, 约 5000)。生产形态下 CreatePlayer 的 9s 推导本来就不成立【推,依据 gRPC deadline 传播语义】。

### 5.2 不变式

```
I1  目标 zrpc Timeout + 1000 ≤ ForwardTimeoutMs                        (新增部署门禁)
I2  ForwardTimeoutMs < 路由服 Timeout                                  (现有 Validate【码】config.go:79-93)
I3  路由服 Timeout + 2000 ≤ gate deadline(ClientRpcRouter)             (现有部署门禁【码】k8s_deploy.ps1:948-1000)
I4  gate deadline(ClientRpcRouter) + kMaxLaneWait + 1000 ≤ 客户端超时   (新增;8000 + 5000 + 1000 ≤ 15000 ✓)
I5  scene 下游最大 gRPC deadline + 1000 ≤ kGateToSceneClientCallTimeout ≤ 客户端超时 − 1000
                                                                       (2026-10-15 由 muduo RPC0 那一批新增;10000 + 1000 ≤ 12000 ≤ 14000 ✓)
```

I5 管的是 scene 的 TCP(RPC0)路径,不是路由 lane:gate 把内层非 Empty 的客户端消息经 `CallMethod` 发给 scene,超时即结果未知,回带请求 id 的 1003、不重发【码】`cpp/nodes/gate/handler/rpc/client_message_processor.cpp` 的 `kGateToSceneClientCallTimeout`;设计见 [muduo-rpc-channel.md](./muduo-rpc-channel.md) §8.1。下游最大值取 scene 实际会调的 gRPC 目标:当前只有 SceneManager(`base_deploy_config.yaml` 的 `SceneManagerNodeService: 10000`),scene 不调 login。今天 scene 的玩家 handler 都同步完成,这条超时只兜「scene 卡住 / 回包丢失」;以后有 handler 改为等下游再完成时,本式保证 gate 不会先于下游给客户端回结果未知。改 `SceneManagerNodeService` 或给 scene 新增 gRPC 下游时要重核本式。TCP 路径同会话本身单连接 FIFO,不进 lane(§7.1),所以 I5 与 kMaxLaneWait 无关。

### 5.3 修法(推荐 B)

- **B(推荐):** login、match 的 `Timeout` 降到 4000。生产形态下它们本来就被截到约 5s,这一步只是把实际预算写对。login 的 CreatePlayer 三个子预算要按 4s 重新分配,由 login 负责人决定数值。`deploy/login-stack.linux/login.yaml` 同步修改。`k8s_deploy.ps1` 新增 `Assert-RouterForwardBudget`,对每个路由目标核对 I1,并补 Pester 用例(含反例)。
- **A(不推荐):** 路由服按目标类型配置 ForwardTimeout(login 11000)。这会把 gate deadline 推到 14000 左右,I4 只能靠把 kMaxLaneWait 降到 0、或者调大客户端超时才能满足。

---

## 6. 业务提交点与栅栏

### 6.1 提交点核实

go-zero 超时拦截器用 `context.WithTimeout` 派生 ctx 交给 handler,超时就取消这个 ctx 并立即返回,handler 的 goroutine 继续跑【码】`timeoutinterceptor.go:34-71`。handler 只要把这个 ctx 传进存储调用,截止之后就不会再发出新命令。但截止前已经发出的那一条(一次 EVAL 或一次 COMMIT)撤不回来,这就是 §6.2–6.4 要兜的剩余窗口。

| 服务 | 请求 ctx 传进存储 | 提交前检查 ctx | 要改的地方 |
|---|---|---|---|
| **match** | **否** | **否** | `queue.go:216`(IsBattleLocked 传 `context.Background()`)、`:221`(无 ctx 的 Hgetall)、`:254`(createTicketIfAbsent 传 Background)、`:301,315,427,443,456,472`(无 ctx 的 Eval)、`:542`(deleteTicketIfOwned 传 Background)【码】;调用点在 `joinqueuelogic.go`、`cancelqueuelogic.go`、`challengelogic.go`、`watchbattlelogic.go`。改为传 `l.ctx`,已有 `createTicketIfAbsentCtx` / `deleteTicketIfOwnedCtx` 可用【码】`queue.go:261,554`;在 createTicket 与 enqueue 之前判 `l.ctx.Err()`;补偿与回滚继续用独立 ctx |
| team | 是 | 是 | ✓【码】`go/match/internal/team/store.go:226-229` |
| login Login | 是 | 隐式(go-redis 截止后不再发命令) | Save 与 SAdd 两步不原子,也不检查关闭状态 → §6.2 |
| login CreatePlayer | 是;补偿用 WithoutCancel | 是(围栏 Lua) | ✓ |
| login EnterGame 后台链 | 独立 background ctx,预算 5min | — | 设计如此【码】`entergamelogic.go:252-253`,由 §6.2 的握手兜住 |
| guild | 是(拦截器套 3500 预算) | 是(`BeginTx(ctx)`) | ✓;缺 expected_guild_id → §6.4 |
| friend / chat / trade | 是 | 是或隐式 | ✓ |

这张表的 match 一行与 go-zero 拦截器语义由主会话复核过;其余各行来自第二轮起草代理的逐服务阅读,未逐行复核。

> 2026-10-15 订正(收尾第一批逐服务复核,证据单测见 §16 同日行,未运行):
> - **team**:请求 ctx 一路进存储;Mutate 与开战锁 `CommitMatchLock` 提交前查 ctx;EndMatch 补偿用独立 ctx + 2s。提交后的推送与 scene 刷新信号用独立 ctx,属于 §7.1「下行靠版本号」,不是迟到写。✓
> - **friend**:`withRequestBudget` 从请求 ctx 派生;唯一写骨架 `runGuardedWrite` 在补齐容量行前查 ctx,BeginTx / Commit 受 ctx 约束;提交后的缓存失效 / 摘要 bump / 推送挂在请求 ctx 上,截止后丢弃、由客户端拉取兜底。✓
> - **chat**:全部 Redis 调用带请求 ctx(go-redis 取连接前查 ctx);幂等键释放与完结用 WithoutCancel + 1s。✓
> - **trade**:客户端路径只有收藏两处写,`assetop.WithTxRetry` 每次尝试前查 ctx。✓
> - **guild**:拦截器套 RequestBudget(Timeout − 500),`database/sql` 的 Commit 在 ctx 结束时不发 COMMIT;客户端写路径上的 Background / WithoutCancel 只有提交后的缓存失效、历练收尾、资产 settle、推送四类。✓
> - **login CreatePlayer**:在 player:zone 登记、种子、围栏写与名字登记之前显式查 ctx;补偿与提交后记账(名字释放、建角锁释放、回读、7b 反查映射)用 WithoutCancel + 1s。✓ **子预算未重分**:名义之和(发号 3s + 名字 3s + player:zone 3s + 种子 1s)超过 Timeout 4000,实际被 handler ctx 截在 4s、截止后不提交,收紧只是换取「在自己那一步干净失败」,且会牵动同名重试门槛,数值由 login 负责人定(§5.3 原文)。
> - **login RefreshToken**(新增一行):作废与签发合成一段按值比对的 Lua,提交前查 ctx。✓ 「脚本已执行、回包丢了,重试必然失败」仍是 §7.2 第 5 条的重试问题,已另立任务。
> - 四个社交服务与 guild 的 zrpc Timeout 都是 4000,满足 I1。

> 2026-10-16 订正(Java 版同期核实时发现同类缺口,反查 Go 版):上面 friend、trade 两行的 ✓ 偏乐观。go-sql-driver/mysql 在 ctx 结束时只断开连接、不发 KILL QUERY;**事务外的自动提交单语句**若正在等行锁,服务端会继续等到 `innodb_lock_wait_timeout`(friend / trade 连接串没设,MySQL 缺省 50 s),拿到锁后照样执行并提交,形成迟到写。显式事务里的语句不会:`Tx.Commit` 在 ctx 结束时不发 COMMIT,断连后服务端回滚。客户端路径上属于这一类的只有 friend `FriendRepo.Unblock` 与 trade `ListingRepo.DeleteFavorite` 两处,已改为单语句 RC 事务、提交前查 ctx(trade 走 `assetop.WithTxRetry`),各补录制驱动回归用例;friend 的守卫事务(含 RejectRequest)、`ensureFriendCapacityRow` 的 ODKU 补行(迟到落地跨不过回收保留期)、trade InsertFavorite 本来就安全。连接串加 `innodb_lock_wait_timeout=2` 的建议未落:会牵动守卫事务的 1205 定性、WithTxRetry 重试与后台回收路径,需单独评估。别的会话在途的 match 全球匹配(`internal/authority`)有同类问题,未改:`SavePlan` 自动提交覆盖;`Ticket` / `ReleaseReady` 在客户端路径上用 `context.Background()` 派生的 3 s ctx。见 §16 同日行。

### 6.2 login 会话栅栏(O12)

**要修的三种竞态:**
- Disconnect 抢跑仍在途的 Login,留下 login_session 和设备集残留;
- EnterGame 已受理、后台链未完成时断线,Disconnect 带 player_id=0 早退,后台链随后写入 ONLINE 而没有租约。这一条顺序正确也会发生;
- EnterGame 之后 LeaveGame:LeaveGame 不拿锁,与后台链并发,后台链在 LeaveGame 之后又写回会话和位置。另外还有同一会话连发 EnterGame(p1)、EnterGame(p2),两条后台链都跑完,同一会话绑了两个角色。

**记录:** `login:sessfence:{gate_instance_id}:{session_incarnation}`,Redis hash,字段 `closed`(0/1)、`gen`(LeaveGame 时递增)、`player`(本会话绑定的玩家)。它和 `login_session:*` 在同一个 Redis client 上,可以写多键 Lua【码】`go/login/internal/logic/pkg/loginsession/loginsession.go:48`。

**为什么要带 gate_instance_id 与 incarnation:** session_id 由 node 段加 17 位序号组成,gate 进程重启后序号从 0 开始【码】`cpp/libs/engine/core/utils/id/node_id_generator.h:49-52`;同 zone 的新进程还可能拿到同一个 node_id。只按 session_id 建键,上一个进程留下的 `closed=1` 会误拒新连接。gate_instance_id 是每次进程启动生成的 UUID;incarnation 在进程内单调、不回绕。incarnation 为 0(旧版 gate)时跳过栅栏,沿用旧行为并计数。

**TTL:** `chainBudget + 1min`,从 `entergamelogic.go:252` 的常量派生。它要比所有还可能读这条记录的操作活得久,最长的是 EnterGame 后台链(5min)。不用 SessionExpireMin(30min):那是登录态的寿命,不是在途操作的寿命。

**操作**(都是单段 Lua):

| 场景 | 位置 | 逻辑 |
|---|---|---|
| Login 提交 | 取代 `loginlogic.go:152-176` 的 Save + TxPipelined | `closed==1` → 拒绝(复用既有码,反正回包也送不到);否则 SET login_session EX、SADD 设备集、EXPIRE。后面的 SCard 判断不变 |
| Disconnect | 取代 `disconnectlogic.go:31` | HSET closed 1 并 EXPIRE;GET+DEL login_session;HGET player 得到 P。在 Lua 之外 SREM 设备集,这是安全的:closed 之后不会再有新的 SADD。要标记断线的玩家:会话里的 player_id,加上 P(P 非 0 且不同于会话里的 player_id 时),两者各调一次 `markPlayerSessionDisconnecting`。不能只在「为 0 时取 P」:gate 在 LeaveGame 之后不清 playerId,同一连接换角时会话头带的是旧角色,只认它会把 P 永久留在 ONLINE(2026-10-15 评审订正;下游按 session_id 做 CAS,对旧角色是 no-op)。gate 合成的这条通知必须带 session_incarnation,否则这一行整段跳过 |
| EnterGame 入口 | `entergamelogic.go:90` 之后 | 先建记录(gen 缺省 0)并续期到 TTL,再判:closed → 拒绝;player 非 0 且 ≠ 本次 player_id → 拒绝(复用 kLoginInProgress);记下 g0 = gen。续期是 2026-10-15 评审补的:只读不续期时,记录可能在入口与绑定之间过期,gen 归零造成误判「已离开」,或过期后的 LeaveGame 从空记录重建出与 g0 相同的 gen 而被漏判 |
| 后台链绑定 | 会话持久化成功之后、`SendBindSessionToGate` 之前(`entergamelogic.go:706-715`) | closed → 补 `markPlayerSessionDisconnecting(P, sid)` 后中止;gen ≠ g0 或 player 已是别人 → `deletePlayerSession`(MarkOffline CAS)后中止;否则 HSET player P |
| 后台链进场前 | `SceneManagerClient.EnterScene` 之前(`:739`) | 再查一次 closed / gen,有变化就跳过 EnterScene 并中止 |
| LeaveGame | `leavegamelogic.go:33-36` | HINCRBY gen;HGET+HDEL player 得到 P;清 login_session;再走原有的 DeleteSession:会话里的 player_id 与 P(不同且非 0 时)各删一次,理由同 Disconnect 行 |

**正确性:** Disconnect 是「先写 closed,再读 player」,后台链绑定是「先查 closed,再写 player」,两段都是同一 Redis 上的原子 Lua。关闭在前,绑定必然看到 closed,由后台链补发 SetDisconnecting;绑定在前,关闭必然读到 P,由 Disconnect 补发。两种顺序都收敛到「带租约的 DISCONNECTING」,现有 player_locator 的 session+version CAS 继续做最终仲裁【码】`go/player_locator/internal/logic/setsessionlogic.go:175-201`、`setdisconnectinglogic.go:35-107`。

**与 [scene-enter-session-fence.md](./scene-enter-session-fence.md) 的关系:** 那份文档按「登录链」铸造 `login_generation`,裁决的是旧会话的入场请求晚到 scene、覆盖新会话的问题。它回答「哪一次登录是当前合法的」,回答不了「发起这次登录的 gate 连接是否已经关闭」。本节的栅栏按「gate 连接」(gate_instance_id + session_incarnation)记录关闭与离开,两者互补,不是同一份真相的两个副本。实施批次 D 时,后台链里的两个检查点放在铸造代次与会话持久化之后,不改变那份文档定下的铸造顺序;那一批改动同样在改 `entergamelogic.go`,批次 D 要等它落地后再动手。

**这是本方案里最复杂的一块,需要 login 负责人评审。** 测试用 miniredis 覆盖「关闭在前 / 绑定在前」两种顺序、关闭之后的 Login 被拒、LeaveGame 推进 gen、同一会话的第二个角色被拒。

### 6.3 match(O13)

- JoinQueue 在 createTicket 之前用 `playercontract.LoadSession` 复核:`state == ONLINE`,且 `session_id`、`gate_instance_id` 与 SessionDetails 一致;不一致回既有码 `ErrNotInScene`。SharedRedis 与 MatchRedis 是两个库,这一步**不原子**,只能缩小跨会话窗口,剩下的由票据 TTL 和 GetQueueStatus 权威查询兜底。
- 不在 CancelQueue 上加墓碑。§6.1 改完之后,晚落地的 createTicket 只会留下「queued 但不在队列」的孤儿票,下一次 Join、整队开战或活动开战时由 healOrphanTicket 删掉【码】`joinqueuelogic.go:259-269`;晚落地的 enqueue 只留下没有票的残留项,matcher 会摘掉【码】`matcher.go:517-526`。压测证明剩余窗口确实存在时再加墓碑。
- **客户端约定(建议写进客户端规范):** CancelQueue 带上 JoinQueue 回包里的 `queue_ticket`;JoinQueue 结果未知(1003 / 超时)时,先调 GetQueueStatus 拿权威状态和票号,再决定是否取消。这条数据依赖在超时、重连、跨 gate 时都成立,是 lane 之外最便宜的一道防线。

### 6.4 guild expected_guild_id(O14)

`LeaveGuildRequest`、`DisbandGuildRequest`、`KickGuildMemberRequest`、`TransferGuildLeaderRequest`、`SetGuildMemberRoleRequest` 各加一个 `expected_guild_id`,字段号按各消息下一个空闲号【码】`proto/guild/guild.proto:78-128`。0 表示旧客户端,行为不变,并计数;非 0 时作为 guildID 传进 repo 事务,不一致回 `ErrNotInGuild` 加当前快照,不写入。这是客户端可见的 proto 改动,两个版本加客户端同批;它防的是「卡住的 LeaveGuild 恢复后把玩家从新帮会移出」(概率低),所以排在最后一批。

---

## 7. 本方案不管的顺序,以及顺带发现的缺陷

### 7.1 边界

| 顺序 | 现状 | 归属 |
|---|---|---|
| 同会话 scene TCP 消息之间 | 已经有序:单连接 FIFO,gate 与 scene 都单线程同步处理【码】`cpp/nodes/scene/handler/rpc/scene_handler.cpp:386-591` | 不改 |
| TCP(scene)与 gRPC(路由)之间,例如 JoinTeam 之后紧接 TravelToZone | 无顺序 | 各自在提交点复核,例如 scene 的 InBattleComp 复查【码】`player_lifecycle.cpp:3994-4000`;TravelToZone 的队伍缓存过期属于 scene 侧问题 |
| RPC 回包、Kafka 推送、scene 推送三条下行之间 | 无顺序 | 业务版本号(team 的 membership_epoch/version),或推送只提示客户端去拉(guild、friend);参考项目原则 5-A / 5-B |
| chat 消息的显示顺序 | 按处理实例的墙钟排序【码】`go/chat/internal/logic/chat_logic.go:208,302-306` | lane 修不了实例时钟偏差;改用权威存储的单调序(例如按频道 INCR) |

### 7.2 顺带发现的缺陷(建议各自立任务)

1. 直连模式下 match 回包全部丢失(match 不回写会话头)【码】`go/match/match_service.go:346-350`、`cpp/nodes/gate/main.cpp:292-293`。
2. `login_session` 键只用 uint32 session_id,gate 重启后可能与旧进程的会话撞号,存在继承别人登录态的风险【码】`loginsession.go:38-40`。
   > 2026-10-18 已修(未编译、未运行,待 Codex 验证;审计 [data-overwrite-rollback-audit-20261018.md](./data-overwrite-rollback-audit-20261018.md) §2.3 login-session-route#1):
   > - **login**:登录态改按 gate 连接建键 `login_session:{gate_instance_id}:{session_incarnation}`(`loginsession.RefOf`,与会话栅栏共用 `sessionfence.Key.String`);CreatePlayer / EnterGame 只读本连接的新键,**不回落**旧键,撞号的新连接读不到任何账号。账号设备集成员同步改为键后缀,`PruneStaleFromDeviceSet` 对两种形状通用。
   > - **旧版 gate 兼容**:代次为 0 或缺实例号时仍按 session_id 建键,计 `loginsession_legacy_key_total{op=save|get|cleanup}`;这条路径保留旧风险,只能靠升级 gate 消除。
   > - **滚动升级**:新路径 Login 时在同一段 `CommitLogin` 脚本里双写一份旧键,让并存窗口里仍按 session_id 读的旧 login 与 login 回滚都能工作;新 login 从不读它,Disconnect / LeaveGame 一并删除。反方向(旧 login 受理 Login、新 login 受理 EnterGame)会被判未登录、需要重登,这是有意的 fail-closed。全部 login 升级、上述计数长期为 0 之后,删除双写与兼容路径。
   > - **gate 纵深防御**:`SessionInfo.loginSucceeded` 记录本连接是否收到过成功的 Login 回包(`handler/rpc/login_admission.h`),CreatePlayer / EnterGame / LeaveGame 在此之前一律回带请求 id 的信封 2028(与 login 同一个「未登录」码)。路由模式在 lane 发出时判(连发的 Login → EnterGame 仍可用,O7 不受影响),直连模式在收包时判。这一层在「新 gate + 旧 login」窗口里挡住撞号请求。直连模式的 `BuildSessionDetails` 同时补上会话代次(此前只有 lane 写,直连的 Login / EnterGame 一直走旧版路径)。
   > - **Java 版**:不受影响。Java login 用 `SessionDirective.BindAccount` 把账号交给 gate,账号存在 gate 连接自己的 `SessionContext` 里(`EnterGameHandler` 取 `session.getAccount()`),没有按 session_id 建键的登录态;设备键已含 gate_instance_id。无需改代码。
3. gate 从不在 LeaveGame 之后清 playerId,同一连接换角色时,新角色 BindSession 到达之前的请求仍以旧角色身份执行【码】`gate_event_handler.cpp:78,211`、`leavegamelogic.go:25-39`。Java 靠 login 应答里的 SessionDirective 同步解绑。
4. DonateToGuild / BuyGuildShopGoods 没有客户端幂等键,超时重发会扣两次【码】`go/guild/internal/logic/economy_logic.go:742-746`。这是重试问题,不是顺序问题。
5. RefreshToken 一次性消费,回包丢失后重试必然失败【码】`go/login/internal/logic/pkg/token/token.go:145-159`。
6. CQ 轮询遇到 `!ok` 直接 `return`,跳过本轮剩余的完成事件【码】`grpc_init_client.cpp:365-368`。本文的本地截止能兜住,根治属于生成模板的问题。(2026-10-16:随 gRPC 传输第 3 批删除官方 CQ 而失效。)
7. 镜像 CreateScene 在途时没有登记 PlayerSceneChangeInFlightComp;SortBag 没有判冻结【码】`player_scene.cpp:288-303`、`player_bag_handler.cpp:56-69`。
   **2026-10-15 处理结果**(C++ 源码已改,未编译):
   - **镜像在途:属实,已修。** 用的是独立组件 `PlayerMirrorCreateInFlightComp`,不复用普通换图那个槽:那个槽记的是已发出的 EnterScene,旧版 scene_manager 不回显号时会按 player_id 把它收掉。`RequestEnterMirrorScene` 先登记再发,改用每次调用回调;`IsSceneChangeBusy` 把它与普通换图一起判,TTL 相同。完成回调 `HandleMirrorCreateDone` 按令牌认领,三路结局各回一条 tip;这也补上了「业务失败应答不回显 creator_ids、创建者收不到提示」的缺口。详见 [scene-creation-architecture.md](scene-creation-architecture.md)「C++ caller」。
   - **SortBag 冻结:误报,未改逻辑。** 冻结闸在 `BagService::MergeAndCompact` 开头,所有整理入口共用,2026-08-30 起就有;回归用例是 `PlayerFeatureSnapshotTest.FrozenSortKeepsSourceLayoutUnchanged`。只核对了 handler 才会误报,`PlayerBagSystem::Sort` 已补注释写明闸的位置。
   - **Java 版**:两条都已覆盖,未改。镜像取号复用 RESOLVING 在途槽;SortBag 用 `FreezePolicy.GATED`,由 `BagService.writable` 拒绝。
8. (批次 B 实施时发现)WatchBattle 的 `addObserver` 如果恰好在请求截止那一刻还在途,battle 侧可能已经挂上观众,match 却回了失败、删了观战标记。之后玩家排队开战时,gather 靠观战标记找不到这场观战,也就不会清退,玩家可能同时是 A 场的观众和新一局的参战者。批次 B 把这个窗口从「最长约 9s」缩到「截止那一刻在途的那一次 RPC」,但没有消除它。根治办法:RPC 失败且结果未知时,用独立 ctx 补发一次 `removeObserver`。它会给本玩家推一次 SpectateEnd,所以要先和 battle 侧确认语义再做。

> **2026-10-17 契约勘误**：第 7 项的镜像在途已由独立 `PlayerMirrorCreateInFlightComp` 补齐。普通换图与镜像统一使用当前生效的 SceneManager deadline + 1000ms 作为 TTL，时间回拨按失效处理。镜像完成回调同时核对捕获 token 与槽时效；token 不符不动新槽，匹配 token 但已过期或回拨时仅摘当前 token 并静默丢弃，不推 tip、不自动进场。后来的普通换图登记、交接全部预检通过、退出新挂 `UnregisterPlayer`、权威落点成功这四个提交点还会永久摘掉旧镜像令牌，UTC 回拨后恢复到原 TTL 内也不能使旧完成复活；忙碌检查仍只读，交接预检失败不取消旧镜像。此勘误不代表测试已执行。

---

## 8. 被否决的方案

| 方案 | 不做的理由 |
|---|---|
| 只靠客户端协议约束(按功能单飞、Cancel 必带票号),服务端不排队 | 写进规范的约束不等于实现,team 客户端的单飞守卫文档里登记为「全部未做」【文】`team-system.md:2229`;按功能单飞覆盖不到跨功能因果。它作为补充保留(§6.3 的客户端约定),不作为唯一防线 |
| 路由服按 hash(session_id) 粘性 + 路由服内排队 | 违背 D29;成员变化时要排空、重排;路由服被黑洞时,粘性让一个会话的请求全部卡住;gate 本来就持有会话状态 |
| 按 player_id 一致性哈希到业务实例 + 进程内 per-player mailbox | 每个服务都要做亲和路由与迁移时的停写/激活交接;路由服不透明转发(D30),拿不到 player_id;业务权威状态已在共享存储里 |
| gate 打会话序号,业务按 (player, 对象) 水位拒绝旧请求,作为主方案 | 每个互逆对都要在业务里落一份水位,还必须与提交写在同一段 Lua 或同一个事务里。作为主方案太重;但 O5 在准入时分配的 `gate_call_seq` 为以后按需落地留好了口子 |
| 按服务分域开 lane(Java 现状)作为第一阶段 | 要靠人工标注跨域屏障方法,忘标就静默乱序;team 和 match 同进程、共用票据却分在两条 lane,丢了因果却换不来隔离;它是 §4.12 放宽路径的目标形态之一,但不是默认 |
| 1003 之后静置一段时间再放行下一条 | 没有数据支撑,而且每次失败后都要多等;等压测有证据再说 |

### 8.1 业界做法对照(选型依据)

业界保证「同一实体的操作按序处理、同时可以水平扩展」的做法,核心都是同一条:**每个 key 只有一个处理者,处理者串行;状态层再用版本号 / 栅栏 / 幂等兜住超时与故障切换。** 常见的落地形态有三种:

| 形态 | 例子 | 适用 | 本仓 |
|---|---|---|---|
| 接入层按会话串行 | 本仓 Java 版 gate、常见游戏网关 | 业务服务无状态,状态在共享 Redis / MySQL | Go 服务正是这种,**本文补的就是这一层** |
| 按实体分片的 Actor(单激活、邮箱串行) | Orleans、Akka Cluster Sharding、BigWorld BaseApp | 实体状态常驻内存 | scene 已经是这种(玩家实体只在一个 scene 线程上),所以 TCP 路径本来有序 |
| 按 key 分区的日志 | Kafka 分区 + 每分区单消费者 | 异步事件 | 推送、存档已经这样用(AGENTS §7.3) |

三种形态都绕不开状态层的版本号 / 栅栏和幂等:超时与故障切换会打破任何传输层顺序。本文的第 3 层就是这一部分,会话代次栅栏与 Kafka 幂等生产者的「producer epoch + 序号」思路相同。

本仓选「接入层按会话串行」,理由:

- 它本身就是按 key 分区、单一所有者的结构。一个玩家只有一条连接、连着一个 gate,gate 就是这个玩家请求流的唯一所有者;加 gate 就是加分区,不需要哈希,也不需要再均衡。gate 挂了,连接随之断开,所有权自然结束。路由服与业务服保持无状态、随机分配。
- 把 Go 服务改成按玩家分片的 Actor,每个服务都要做分片协调和交接(旧节点先停写、新节点再激活),而这些服务的状态本来就在共享存储里。更关键的是,Actor 只保证同一个 Actor 内部的顺序:玩家先取消匹配、再接受帮会历练,两条请求落在两个服务的两个 Actor 上,彼此仍然没有顺序,最后还是要回到接入层保序。

---

## 9. 与既有决策的关系

| 决策 | 关系 | 需要修订的条文 |
|---|---|---|
| client-rpc-router D29 / D33 | 不变:路由服无状态、随机 | 无 |
| client-rpc-router D30 | 不变:gate 不解析 body | §3 转发契约补一句:「路由服按原值回写 `x-session-detail-bin` 是 gate lane 认领的依赖契约」 |
| client-rpc-router §6 / microservice-zone-contract §3 | 预算链 | 加入 I1–I4,写明 login、match 的 Timeout |
| client-access-band-routing D51 | **兼容**:单 lane 不需要方法知识,只需要知道「这条消息走路由服」 | 第三阶段放宽时再修订(§4.12) |
| client-access-band-routing D57 | 合并 | 「每会话在途 ≤ 32」改为「排队 + 在途 ≤ 32」;按次记账的键由 `(ClientContext*, seq)` 改为 `gate_call_seq`。理由:生成的 Send* 返回 void,gate 拿不到 ctx 指针【码】`cpp/generated/grpc_client/client_rpc_router/client_rpc_router_grpc_client.cpp:67-89` |
| client-access-band-routing D60 | 照用 7 号字段 | 无 |
| client-access-band-routing §12.4 | 「惰性清理、不挂定时器」 | 改为「主动扫描(TimerTaskComp,回调不捕获对象)+ 惰性清理」 |
| client-access-band-routing 阶段 0b / 0d / 0g | 0b(player_id 归一)、0d(失败回执带 id)并入本方案批次 C;0g(节点摘除排空)与本方案兼容:0g 落地后,晚到的真实完成会因 extract 取不到而被丢弃(2026-10-16 订正:0g 已不需要 —— gRPC 传输第 3 批后节点摘除时 `~Channel` 以 UNAVAILABLE 完成在途调用,经失败桥认领) | 在阶段表里注明 0b、0d 由本文批次 C 承担 |
| [gRPC 客户端契约](grpc-muduo-transport.md#grpc-client-call-sites) | 失败桥行为变化 | 失败回执改为带 id 的 1003 信封 |
| xuanming-port-decisions D-12 | **兼容**:lane 只在路由模式启用,C++ 默认值和现有单测都不改 | 无 |
| AGENTS §4 字段类型清单 | 新增两个 uint64 计数字段 | 是否登记进清单,由用户决定 |

---

## 10. Java 版对齐(AGENTS §12)

| 维度 | Java 现状 | 与本文的差异 | Java 要补 |
|---|---|---|---|
| lane 粒度 | login 与 scene 共用一条 pending 队列和一个 inFlight 位;6 个后端域各一条队列,每条一个 backendInFlight【码】`game-gate/.../ClientSession.java:93-102`、`ClientDispatcher.java:347-359,413-439` | 本文对路由类请求全序;C++ 的 scene 走 TCP、不进 lane | 取决于决策 Q1:推荐把 6 个后端域队列合成一条会话后端 lane(login+scene 队列不变,移动消息不受影响,现有测试「好友调用在途不阻塞 login 与 scene」仍成立) |
| 排队等待上限 | 入队与 drain 都不看入队时刻【码】`ClientDispatcher.java:417-438` | 本文 5s | 补上 |
| 超限 | 后端队列满 64 断连【码】`ClientDispatcher.java:417-426` | 本文回 1008 带 id | 统一为 1008 带 id(决策 Q1 一并定) |
| 失败回执 | 后端失败回带 id 的 1003【码】`:478-484`;**login 失败推 23 号 tip,不带 id**【码】`:533-536` | 本文全部带 id | login 失败改为带 id 的信封 |
| 截止 | gate 不向 `ClientMessageService.handle` 下传截止;match 从受理时刻起算 4500;`Deadline.await` 撤不回已发出的 Redis 命令,Redisson 超时会原样重发 EVAL【码】`game-common/.../Deadline.java:43-47` | 同样的剩余窗口 | handle 下传 gate 的绝对截止,后端从中派生预算、提交前检查;写脚本的重发不得越过截止;修正 `ClientMessageService.java:30` 的注释(超时窗口下「后端不需要并发控制」不成立) |
| 断线 | 丢弃未发出项;不等后端调用;等在途 login 调用完成后再通知 login【码】`:375-379,471-477,523-531` | 本文立即通知 + login 栅栏 | 可以保留等待;Java login 是否存在与 EnterGame 后台链同类的竞态,**待核实** |
| 跨会话 | JoinQueue 要求位置为 ONLINE,不校验会话身份 | 同 §6.3 | 用 SessionContext 的会话号与 gate 实例比对 |
| 重定向 | 丢弃排队项、停止 drain【码】`:433-434,1058-1062` | 本文对齐 Java | 无 |

> 2026-10-16 订正(Java 版后端截止核实时发现):Java gate 的会话后端 lane 不只在后端回包或 5 s 消费端超时时放行,Dubbo 调用以任何错误结束(断连、Triple 流被重置、提供方重启)都会立即放行,而后端 handler 可能还在跑、仍会在自己的预算内提交,与下一条交错。Dubbo 不像 gRPC 那样把取消沿调用链传给后端 handler。已改:future 异常完成(请求已交给 Dubbo、结果未知)时回执照常立即给,lane 等到「发出 + 调用超时 5 s」(`ClientDispatcher.BACKEND_RESULT_UNKNOWN_HOLD_NANOS`)才放行下一条,与超时路径一致;代理当场抛异常(「No provider available」,请求没发出)不等;断线撤销定时器。C++ 版不需要:gRPC 的取消沿 gate → 路由服 → 目标服务传播,目标 handler 的 ctx 随之结束,提交点前查 ctx(§6.1)。

`PARITY.md`:更新第 31 行「gate 接入与路由」,并新增一行「客户端请求按会话保序」,登记两版各自的批次状态。本仓 `docs/PROGRESS.md` 的条目要注明对应的 Java 版本号。

---

## 11. 需要拍板的决策

| # | 问题 | 选项 | 推荐 |
|---|---|---|---|
| Q1 | lane 粒度与两版对齐 | (a) 两版都对路由类 / 后端类请求全序(Java 把 6 个后端域合成一条);(b) 两版都按域分 lane,外加生成的「跨域屏障」方法类,team 并入 match;(c) C++ 全序、Java 维持按域,PARITY 登记差异 | **(a)**。理由见 §0 第 4 条;(c) 会让同一个客户端在两版上看到不同的顺序语义 |
| Q2 | 预算链 | B:login、match 的 Timeout 降到 4000;A:路由服按类型配置 ForwardTimeout | **B**。生产形态下本来就被截到约 5s;A 与 I4 冲突 |
| Q3 | 实施授权与顺序 | 约 60 个文件,跨两个仓库 | 按 §12 的批次顺序,每批独立上线、独立回滚 |
| Q4 | 客户端行为(问客户端负责人) | 正式客户端会不会不等回包连发有依赖的请求?路由模式下是否按 id 精确配对回包?CancelQueue 是否带 queue_ticket? | 不影响本方案的正确性,但决定严重程度和 1008 的实际出现频率 |

**拍板记录(2026-10-14,用户):**
- 用户要求「最标准、可以水平扩展的做法」,不在意改动量,并要求记录到文档后开始实施。
- Q1 按 (a) 执行:两个版本都对会话内的路由类 / 后端类请求全序,Java 把 6 个后端域队列合成一条会话后端 lane。
- Q2 按 B 执行:login、match 的 zrpc Timeout 降到 4000。
- Q3 已授权,按 §12 的批次顺序实施。
- Q4 仍待客户端负责人回答,不阻塞实施。
- 业界做法的对照与选型理由补在 §8.1。

**拍板记录(2026-10-15,用户):** E2(guild `expected_guild_id`,§6.4)推迟,记录下来以后再做;本轮不改客户端仓,也不改 guild 的 proto。在 E2 落地之前,§6.4 要防的「卡住的 LeaveGuild(及 Disband / Kick / Transfer / SetRole)恢复后作用到玩家的新帮会」维持现状:gate lane 让同一会话的这些请求按顺序发出,但路由服回超时、gate 放行下一条之后,上一条仍可能在 guild 里跑完,这个窗口只有 `expected_guild_id` 能关。概率低,所以本来就排在最后一批。

**拍板记录(2026-10-15 稍后,用户):** 用户要求「别管提交,把所有问题全部做完」,取代上一条的「E2 推迟」。E2 服务端两版已实施(§16 同日行);客户端仓 `../mmorpg-client` 未授权,仍未改:客户端下个版本发这五个请求时要带上 `expected_guild_id`(= 发请求那一刻界面上本人帮会的 guild_id),收到 kGuildNotInGuild(14002)时,任免 / 踢人 / 转让用回包里的 guild 刷新界面(没带说明已不在任何帮),退帮 / 解散拉一次 GetPlayerGuild;不要把 14002 当成退帮成功。客户端接入之前窗口仍在,迁移进度看 `guild_manage_expected_guild_total{outcome="absent"}`。

**拍板记录(2026-10-16,用户):** Java game-trade 收藏写入保留「写入预留」(缺省预算 3500 ms 时预留 1000 ms,受理后 2.5 s 起不再发收藏写、回 in-band 1003),登记为已定;备选「只对收藏语句把会话级 `innodb_lock_wait_timeout` 降到 1 s」不采用。mmorpg 没有这个预留,在这一窄窗口上两版的客户端可见行为不同,登记在 Java `docs/porting/trade-spec.md` §1.9 订正注与 PARITY 第 158 行。仍待拍板:RefreshToken 截止放弃的回执码(Java 1003 / mmorpg 2000)、game-gate 在 Dubbo 调用以错误结束时立即放行(归 gate 负责人)。

**按推荐默认值定(2026-10-16,用户「没做完继续做完」,可推翻):** ① RefreshToken 截止放弃时 Java 回 in-band 1003(旧 refresh 仍有效、可重试;mmorpg 路由模式下客户端实际收到的也是 gate 的 1003),不改回 2000。② Java gate 在 Dubbo 调用以错误结束时不再立即放行,改为等到「发出 + 调用超时」(§10 订正注)。③ Java match 157 建票失败后的回滚沿用请求截止(截止已过不发,靠票据 TTL、下一次 157 自愈与 153 兜底),不改成独立预算:独立预算的回滚本身就是回包之后的迟到写。

---

## 12. 分批实施清单

每批都能单独上线、单独回滚。

| 批次 | 内容 | 主要文件(约数) | 生成物 | 客户端契约 | 回滚 |
|---|---|---|---|---|---|
| **A 预算链** | O10:login、match 的 Timeout 改为 4000,重分 CreatePlayer 子预算;部署门禁 I1;文档 | `go/login/etc/login.yaml`、`deploy/login-stack.linux/login.yaml`、`go/match/etc/match_service.yaml`、login 子预算相关配置与注释、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`client-rpc-router.md`、`microservice-zone-contract-20260914.md`(约 8) | 无 | 无 | 回退 yaml |
| **B match ctx** | O11 中 match 那一行 | `go/match/internal/logic/{queue.go,joinqueuelogic.go,cancelqueuelogic.go,challengelogic.go,watchbattlelogic.go}` 及测试(约 8) | 无 | 无 | 回退镜像 |
| **C gate lane** | §4 全部:SessionDetails 7/8、incarnation、player_id 归一、失败回执带 id、准入与排队等待上限、主动扫描、节点摘除快速失败、重定向、汇总日志 | `proto/common/base/session.proto`、`cpp/libs/services/gate/session/comp/{router_lane.h(新),session_info_comp.h}`、`session/manager/session_manager.h`、`cpp/nodes/gate/handler/rpc/{client_request_lane.h,client_request_lane.cpp(新),client_message_processor.h,client_message_processor.cpp}`、`cpp/nodes/gate/main.cpp`、`cpp/nodes/gate/handler/event/gate_event_handler.cpp`、`gate.vcxproj` / `.filters` / `CMakeLists.txt`、`cpp/nodes/gate/tests/router_lane_test.cpp(新)`(约 14) | session.pb(C++、Go、generated 副本;Java 按 ContractSync) | **是**(§4.7) | 回退 gate 镜像(换 gate 要踢在线玩家)。新字段对旧二进制只是未知字段 |
| **D login 栅栏** | §6.2;依赖 C 提供 incarnation,incarnation 为 0 时跳过 | `go/login/internal/logic/pkg/sessionfence/{fence.go,fence_test.go}(新)`、`loginlogic.go`、`disconnectlogic.go`、`leavegamelogic.go`、`entergamelogic.go`、`session_lifecycle.go` 及测试(约 9) | 无 | 无(复用既有码) | 回退镜像;栅栏记录 6 分钟后自然过期 |
| **E1 match 会话复核** | §6.3 | 约 3 | 无 | 无 | 回退 |
| **E2 guild expected_guild_id** | §6.4 | 约 5,外加客户端仓 | guild.pb 各语言 | **是**,改客户端仓需 AGENTS §9 授权 | 字段为 0 时兼容 |
| **F Java** | §10 | `ClientDispatcher.java`、`ClientDispatcherTest.java`、`ClientSession.java`、`ClientMessageService.java`、match 预算相关、`application.yaml`、`PARITY.md`(约 8) | 按 Java AGENTS 执行 ContractSync | 与 C、E2 同批 | 按 Java 仓规范 |
| **R 冒烟** | 新 robot 场景:不等回包流水线连发 JoinQueue→CancelQueue、ApplyJoinGuild→CancelGuildApplication、AcceptFriend→RemoveFriend、Login→EnterGame,断言最终状态与发送顺序一致 | `robot/ordering_smoke_scenario.go`、`robot/etc/ordering_smoke.yaml`(约 2) | 无 | 无 | — |
| 文档 | 本文定稿;§9 列出的修订;`docs/PROGRESS.md` | 约 5 | — | — | — |

**推荐顺序:** A、B(改动小、互相独立,先关掉超时窗口)→ C → D → R → E1 → E2;F 与 C 同批。

---

## 13. 给 Codex 的验证命令

工作目录:仓库根 `D:\luyuan\xuanming-server-mmo-cpp`。按顺序执行,任何一步红了就停,保留失败日志摘要。

1. **proto 重生成(批次 C、E2):** 按 AGENTS §4 执行 `cd go && build.bat`(脚本结尾有 `pause`,非交互执行时用等价的 `go/build.ps1`)。期望:`go/proto/common/base/session.pb.go` 与 C++ 的 `session.pb.*` 更新,其余生成物无差异。
2. **C++(批次 C):** `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,**必须串行 `/m:1`**,通过标准是 0 error。SessionDetails 变了,所有节点都要重编。`router_lane_test` 照 `cpp/nodes/gate/tests/gate_security_test.cpp` 的方式独立编译运行,全绿;2026-10-15 起 `router_lane.h` 的在途索引改为引用 `cpp/libs/engine/core/utils/data_structures/inflight_index.h`(仍只依赖 STL),include 路径要多加 `-I cpp/libs/engine/core`,以 `router_lane_test.cpp` 文件头的命令为准。覆盖:满额拒绝;lane 内 FIFO;同时只有 1 条在途;Complete 恰好一次、旧 seq 被忽略;排队超时不发出;同步失败时 Pump 循环会终止;断线返回在途 seq;过期堆惰性删除;重定向之后拒绝新请求。「session_id 复用时按会话代次丢弃迟到完成」在装配层 `FindLiveSession`(依赖 `tlsSessionManager`),独立单测碰不到,靠代码审阅与冒烟覆盖(2026-10-15 评审订正:此处原先误列为单测覆盖项)。
3. **Go:** 分别在 `go/match`、`go/login`(以及 E2 时的 `go/guild`)目录执行 `go build ./... && go vet ./... && go test ./... -count=1`,全绿。login 栅栏用 miniredis 跑 §6.2 列出的用例;match 用例覆盖 ctx 取消后不再建票。
4. **部署门禁(批次 A):** `pwsh -NoProfile -Command "Invoke-Pester -Path tools/scripts/tests/k8s_deploy_contract.tests.ps1 -Output Detailed"`。期望新增的 I1 用例通过,并且反例(login Timeout 配成 10000)被拒。
5. **冒烟(批次 C、D、R):** 先按 AGENTS §6.2 清空 Redis / MySQL / etcd 与 Kafka offset,确认路由模式开启(`GATE_CLIENT_RPC_ROUTER=1`)。在 `robot/` 下依次跑 team / friend / guild / chat / battle / trade 现有冒烟,再跑 `ordering_smoke`。失败时保留:robot 输出、gate 的 `[lane]` 汇总行、路由服日志、login 与 match 日志。
6. **性能(批次 C 上线前):** 按 AGENTS §6 先保存上一轮的 `prev-summary.txt`,再压测,用 `stress_summarize.ps1` 出对比表。没有对比表之前,不宣称延迟变化的结论。
7. **Java(批次 F):** 按 Java 仓 AGENTS 的构建与测试口径执行,ContractSync 之后 clean install。

---

## 14. 开放问题

1. 正式客户端的行为(Q4)。
2. 在 pending 状态下析构 grpc::CompletionQueue 会不会崩溃(F29),本轮没有验证,属于阶段 0g。(2026-10-16:官方 CQ 已随 gRPC 传输第 3 批删除,问题失效。)
3. Java login 是否存在与 C++ EnterGame 后台链同类的「已受理、未完成即断线」竞态。
4. §6.1 表中 match 以外各行是起草代理逐服务读代码的结论,主会话没有逐行复核;实施时对每个服务补一条「ctx 取消后不再提交」的单测作为证据。
5. 工作区里 `client_message_processor.cpp`、`main.cpp` 等文件有别的会话的未提交改动,实施前要先分清归属。登录代次那一批(scene-enter-session-fence.md)正在改 login 的 EnterGame 链与 gate 的会话转发,批次 C、D 要排在它之后,并在它的基础上改。

---

## 15. 评审裁决记录

第二轮只有「替代方案」视角的代理评审完成;「故障边界」「业务栅栏」「兼容上线」三个视角因账号额度中断,由主会话按同样的问题清单读代码复核。

### 15.1 替代方案视角(代理评审,verdict = sound-with-fixes)

| 级别 | 意见 | 裁决 |
|---|---|---|
| major | team 与 match 同进程、共用 MatchRedis 票据,按节点类型分两条 lane 会丢因果(CancelQueue→StartTeamMatch 假拒绝;JoinQueue 与 StartTeamMatch 并发时 healOrphanTicket 误删) | **采纳**。第一阶段改为每会话一条 lane(O3),自然覆盖 |
| major | 帮会历练凑齐开战会在同一请求里调 match,与 CancelQueue 跨 lane 乱序会让整个房间失败 | **采纳**。单 lane 覆盖;屏障分类推迟到 §4.12 放宽阶段 |
| major | 排队等待时间没有计入预算,队尾可能在客户端 15s 超时之后才发出 | **采纳**。新增 `kMaxLaneWait` 与不变式 I4(O6、§5.2) |
| major | 否决 (d)、(e) 的理由不成立;序号应在准入时分配 | **采纳**。(e) 升为第一阶段方案;`gate_call_seq` 改在准入时分配,允许以后做水位栅栏,只禁止当幂等键(§4.8、§8) |
| major | lane 与客户端「按功能单飞」高度重叠,而批次 C 改动面最大、还改变客户端可见行为;建议先做服务端栅栏,并采用 Cancel 带票号的数据依赖 | **部分采纳**。批次顺序改为 A、B 先行;客户端约定写进 §6.3。lane 仍然要做:规范约束不等于实现,而且单 lane 能覆盖按功能单飞覆盖不到的跨功能因果(§8 第一行) |
| minor | 重定向时没有处理 lane,与 Java 不一致 | **采纳**(O9) |
| minor | gate 在 LeaveGame 之后不清 playerId | **采纳为相关缺陷**(§7.2 第 3 条),不在 lane 范围内 |
| minor | 「extent 对齐 lane(message_id >> 8)」把语义藏进号段布局 | **采纳**。单 lane 方案不再需要它,已删除 |
| minor | 域内读写全部串行的代价被低估 | **部分采纳**。用排队等待上限限制降级时的放大;放宽要凭压测数据(§4.11、§4.12) |

### 15.2 主会话复核(代替未完成的三个视角)

| 视角 | 复核项 | 结论 |
|---|---|---|
| 业务栅栏 | go-zero 超时拦截器取消的是不是 handler 拿到的那个 ctx | 是:`context.WithTimeout(ctx, t)` 派生后传给 handler,`ctx.Done()` 时返回错误,handler goroutine 继续跑【码】`timeoutinterceptor.go:34-71`。§6.1 的论断成立,剩余窗口限于截止前已发出的那一条命令 |
| 业务栅栏 | match 票据读写是否用了 `context.Background()` | 是【码】`queue.go:216,221,254,301,315,427,443,456,472,542` |
| 业务栅栏 | login 的超时预算 | `Timeout: 10000`,注释按直连模式推导【码】`login.yaml:5-13`;路由模式下被转发 deadline 截断【推】 |
| 业务栅栏 | guild 历练开战是否在同一请求里调 match | 是【码】`activity_trial.go:613-618` |
| 故障边界 | 失败桥能拿到什么 | 发出的 metadata 与请求副本;今天回的是不带 id 的 tip【码】`main.cpp:325-350` |
| 故障边界 | 直连模式能否按回显认领 | 不能:match 不回写会话头【码】`match_service.go:346-350`。因此 O7 只在路由模式启用 |
| 故障边界 | session_id 复用、晚到回包、5ms 批内多个完成 | 由 incarnation 核对与 `extract(seq)` 恰好一次处理;每个完成只 Pump 自己的会话,批内不需要跨会话排序(§4.5) |
| 故障边界 | 排队期间 BindSession 才到达 | 取入队快照,请求按未登录被拒;今天带的是 UINT64_MAX,归一之后更安全(§4.3) |
| 兼容上线 | 与 D-12 冲突吗 | 不冲突:只在路由模式启用,C++ 默认值和单测不改 |
| 兼容上线 | 与 D51 冲突吗 | 第一阶段不冲突(单 lane 不需要方法知识);§4.12 放宽时才需要修订 |
| 兼容上线 | 是否加运行时开关 | 不加。lane 关闭态就是要修的缺陷态,两条路径都得测;回滚靠回退 gate 镜像 |
| 兼容上线 | 1003 / 1008 是否存在、需不需要新 tip | 都存在【码】`common_error_tip.proto:18,28`,不新增 tip |

---

## 16. 实施进度

只追加,不改旧行。每批的「验证」一栏只写 Codex 实际跑出的结果;没跑就写「待 Codex」。

| 批次 | 状态 | 改动文件 | 验证 |
|---|---|---|---|
| A 预算链 | 进行中(2026-10-14) | — | 待 Codex |
| B match ctx | 未开始 | — | — |
| C gate lane | 等待:登录代次那一批正在改 `session_info_comp.h`、`gate_event_handler.cpp`,对方落地后通知 | — | — |
| D login 栅栏 | 等待:登录代次那一批正在改 `entergamelogic.go` | — | — |
| E1 / E2 / F / R | 未开始 | — | — |
| A 预算链 | 已改完(2026-10-14) | `go/login/etc/login.yaml`、`deploy/login-stack.linux/login.yaml`(Timeout 10000→4000)、`go/match/etc/match_service.yaml`(5000→4000)、`tools/scripts/k8s_deploy.ps1`(新增 `Get-RouterForwardBudgetViolations` / `Assert-RouterForwardBudget`,挂在写路径入口)、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`(3 条用例)、`docs/design/team-system.md`(§A.3 第 7 条更正注)、`microservice-zone-contract-20260914.md`、`client-rpc-router.md` | 待 Codex(§13 第 4 步) |
| B match ctx | 已改完(2026-10-14) | `go/match/internal/logic/{queue.go,joinqueuelogic.go,cancelqueuelogic.go,challengelogic.go,watchbattlelogic.go,spectate.go,spectate_test.go}`、新增 `request_deadline_fence_test.go`。要点:客户端路径的提交点(建票 / 入队 / 删票 / 出队 / 挑战占坑与写记录 / 应战决定点 / 观战标记 / addObserver)用请求 ctx 或先检查 ctx;JoinQueue 入队失败的补偿由无条件 DEL 改为按 ticket id CAS 删;Hmset 之后的 Expire、各类回滚仍用独立 ctx。已核实 go-redis v9.17.3 在取连接前检查 ctx(`internal/pool/pool.go:502,682-688`),ctx 已取消时命令不会发出 | 待 Codex(§13 第 3 步,`go/match`) |
| E1 match 会话复核 | 已改完(2026-10-14) | `go/match/internal/logic/joinqueuelogic.go`(`callerSessionStale`)、`request_deadline_fence_test.go`(4 个子用例)。已核实 login 写进 player_locator 的 session_id / gate_instance_id 取自 gate 的 SessionDetails(`go/login/internal/logic/clientplayerlogin/entergamelogic.go:556-558`) | 待 Codex |
| C gate lane(第 1 部分) | 已写好、**未接线**(2026-10-14) | 新文件(都不在 gate.vcxproj / CMakeLists 里,不参与编译):`cpp/libs/services/gate/session/comp/router_lane.h`(纯逻辑:RouterLane / InflightIndex / QueueWaitExceeded)、`router_lane_item.h`(排队项)、`cpp/nodes/gate/tests/router_lane_test.cpp`(独立单测 12 例)、`cpp/nodes/gate/handler/rpc/client_request_lane.{h,cpp}`(装配:Admit / OnRouterReply / OnRouterCallFailed / OnRouterNodeRemoved / ExpireDue / OnSessionClosing / OnSessionRedirected / NextIncarnation / SetRouterSender)。`proto/common/base/session.proto` 加 `session_incarnation = 7`、`gate_call_seq = 8`(纯追加,对旧二进制只是未知字段) | 待 Codex:router_lane_test 可先跑(§13 第 2 步的独立编译命令) |
| C gate lane(第 2 部分:接线) | **等待**:登录代次那一批正在改 `session_info_comp.h`、`gate_event_handler.cpp`,对方落地后通知 | 必须一次做完(只做一半会让 gate 编不过):① `session_info_comp.h` include `router_lane_item.h`,加 `gate_lane::SessionRouterLane routerLane;`、`uint64_t incarnation{0};`;② `client_message_processor.cpp`:include `client_request_lane.h`;`HandleConnectionEstablished` 置 `session.incarnation = gate_lane::NextIncarnation()`;`HandleRouterForward` 改为 `gate_lane::Admit(sessionId, session, request, BuildSessionDetails(...))`,删掉那里的 `loginStarted` 置位(已移进 Pump 的发出时刻);新增 `SendRawViaRouter(request, bin) -> optional<uint64_t>`(SendViaRouter 改为调用它,断线通知照旧走 SendViaRouter,seq = 0)并在构造函数里 `gate_lane::SetRouterSender`;`HandleConnectionDisconnection` 在 `sessions.erase` 之前 `gate_lane::OnSessionClosing`;`OnNodeRemoveEventHandler` 对 `ClientRpcRouterNodeService` 调 `gate_lane::OnRouterNodeRemoved(pb.entity())`;③ `main.cpp`:成功桥在拿到 sd 之后先 `if (gate_lane::OnRouterReply(*sd, reply)) return;`,失败桥解析出 sessionDetails 之后先 `if (gate_lane::OnRouterCallFailed(sessionDetails)) return;`;GateRuntimeContext 加 `TimerTaskComp routerLaneExpiryTimer`,与 sceneEntryRetryTimer 同处 `RunEvery(gate_lane::kSweepIntervalSeconds, []{ gate_lane::ExpireDue(std::chrono::steady_clock::now()); })`;④ `gate_event_handler.cpp` 的 `RedirectToGateEventHandler` 调 `gate_lane::OnSessionRedirected`;⑤ `gate.vcxproj` / `gate.vcxproj.filters` / `CMakeLists.txt` 登记 `handler/rpc/client_request_lane.{h,cpp}` | — |
| R 冒烟 | 已写好(2026-10-14) | `robot/ordering_smoke_scenario.go`(新)、`robot/etc/ordering_smoke.yaml`(新)、`robot/main.go`(分派)、`robot/config/config.go`(模式校验)。批次 C 接线之前**预期会间歇失败**,之后必须每轮通过 | 待 Codex(§13 第 5 步加 `.\robot.exe -c etc/ordering_smoke.yaml`) |
| F Java(第 1 部分) | 已改完(2026-10-14,Java 仓未提交) | `game-gate/.../ClientSession.java`、`ClientDispatcher.java`(6 个后端域队列合成一条会话后端 lane)、`ClientDispatcherTest.java`(两条「跨域并发」用例改写为新语义、两处字段引用)、`PARITY.md`(新增一行)、`docs/design/architecture.md`(§4.13 更正注)。线上形状不变 | 待 Java 仓构建测试(`mvn -pl game-gate -am test`,以 Java AGENTS 为准) |
| F Java(第 2 部分) | 未开始,与批次 C 接线同批 | 排队等待上限 5s、lane 满回 1008 不断连、login 失败改带 id 的信封、handle 下传截止、JoinQueue 复核会话 | — |
| C gate lane(第 2 部分:接线) | 已改完(2026-10-15) | `session_info_comp.h`(追加 `incarnation`、`routerLane`,不碰 loginGeneration)、`client_message_processor.cpp`(`SendRawViaRouter` + 注入、`Admit`、建连分配代次、断线 `OnSessionClosing`、节点摘除 `OnRouterNodeRemoved`)、`main.cpp`(成功桥 / 失败桥先交给 lane;`routerLaneExpiryTimer`)、`gate_event_handler.cpp`(重定向 `OnSessionRedirected`,写在守护段内)、`cpp/nodes/gate/{gate.vcxproj,gate.vcxproj.filters,CMakeLists.txt}`、`cpp/libs/services/gate/{gate.vcxproj,gate.vcxproj.filters}`(只登记头文件) | 待 Codex:proto 重生成后 `msbuild game.sln /m:1` |
| C 与 muduo 传输的协调 | 进行中(2026-10-15) | 「C++ gRPC 迁移至 muduo HTTP/2」会话同时在改 gate 到路由服的发送与回包桥(`router_muduo_forward.*`、`rpc_reply_bridge.*`)。已发消息建议:lane 的两个钩子挪进它的 `gate_rpc_reply_bridge`,让两种传输都保序;muduo 分支接进 `SendRawViaRouter` 内部,签名与返回值不变 | — |
| D login 会话栅栏 | 已改完(2026-10-15) | 新包 `go/login/internal/logic/pkg/sessionfence/{fence.go,fence_test.go}`(CommitLogin / Close / CheckEnterGame / BindPlayer / Recheck / Leave,单测 11 例);`loginsession.go` 导出 `Key` / `TTL`;`loginlogic.go`(有代次时原子提交)、`disconnectlogic.go`(Close + 用绑定玩家补挂租约)、`leavegamelogic.go`(Leave + 用绑定玩家删会话)、`entergamelogic.go`(入口 CheckEnterGame;persist 之后 `bindSessionFence`;EnterScene 前 Recheck;`chainBudget` 提为包级常量 `enterGameChainBudget`,登录代次铸造顺序不变)、新测试 `session_fence_wiring_test.go`(5 例)。旧版 gate(incarnation = 0)全部跳过,行为不变 | 待 Codex:`cd go/login && go build ./... && go vet ./... && go test ./... -count=1` |
| F Java(第 2 部分:gate 侧) | 已改完(2026-10-15,Java 仓未提交) | `ClientDispatcher.java`(`MAX_BACKEND_LANE_DEPTH = 32` 满回 1008 不断连;`MAX_BACKEND_LANE_WAIT_NANOS` 5s 排队超时回 1003;login 失败改带 id 的信封)、`ClientSession.java`(BackendPending 带入队时刻)、`GateMetrics.java`(新增 `LANE_FULL`、`QUEUE_EXPIRED`)、`ClientDispatcherTest.java`(改写 1 例、新增 2 例)、`PARITY.md` 行状态 | 待 Java 仓构建测试 |
| F Java(第 3 部分) | 未开始 | `ClientMessageService.handle` 下传 gate 截止、后端提交前检查;game-match JoinQueue 复核会话身份 | — |
| 文档修订 | 已改(2026-10-15) | `client-access-band-routing.md` §12.4 修订注、`grpc-muduo-transport.md` §16.5（现行失败桥归属）;此前已改 `client-rpc-router.md`、`microservice-zone-contract-20260914.md`、`team-system.md` | — |
| E2 guild expected_guild_id | 未开始 | 客户端可见的 proto 改动,两版加客户端仓同批,需要客户端仓授权(AGENTS §9) | — |
| C 回包桥 | 已合(2026-10-15,muduo 会话合入) | 成功 / 失败回包统一由 `cpp/nodes/gate/handler/rpc/rpc_reply_bridge.{h,cpp}` 认领:`DeliverReply` 第一行 `gate_lane::OnRouterReply`,`NotifyCallFailure` 采样日志之后 `gate_lane::OnRouterCallFailed`;官方 gRPC 与 muduo_h2 两种传输都走它。`SendRawViaRouter` 签名不变,内部多了 muduo 分支 | 待 Codex |
| F Java(第 3 部分,部分) | 已改(2026-10-15,Java 仓未提交) | game-match `QueueService.join(SessionContext, …)` 跨会话栅栏:在线目录条目存在、且会话号或 gate 实例不同就拒(16020);条目缺席放行,因为 gate 进场后才异步写入。`QueueHandlers` 改调新重载;`FakePlayerStatus` 导出 `GATE_INSTANCE` / `sessionIdOf`,`QueueHandlersTest`、`MatchClientMessageServiceTest` 的会话按它构造。**还没加**「旧会话被拒」的专门用例;`handle` 下传截止、Redisson 超时重发 EVAL 仍待评估 | 待 Java 仓构建测试 |
| proto 重生成 | 合并到登录代次那一批的同一次重生成 | 由 docs/PROGRESS.md 里登录代次批次的 Codex 步骤一并产出 SessionDetails 7、8 号字段,不另跑一遍 | — |
| F Java(第 3 部分) | 已改完(2026-10-15,Java 仓未提交) | game-match:`QueueHandlersTest` 新增 3 例(旧会话号被拒、换 gate 实例被拒、目录未写放行);`RedissonTicketStore` 新增 `CLIENT_WRITE_SCRIPT`(`OptionalOptions.defaults().codec(StringCodec).retryAttempts(0)`),157 的 enqueue / createMatched 与 148 的 cancel 改用它,截止前发出的写不再被 Redisson 在 gate 放行下一条之后原样重发(原 retry-attempts = 1,单条最坏约 4.2 s)。`handle` 下传截止不做:各后端自身预算已小于 gate 的 5 s | 待 Java 仓构建测试 |
| 实现评审(2026-10-15) | 已完成 | 四个切片(gate lane、login 栅栏、match / robot / 部署、Java)各一个评审代理加一个对抗复核代理。确认成立的问题见下一行;被复核否掉的一条:「MSVC 下 `std::max` 被 `<Windows.h>` 的 max 宏展开」不成立(muduo 的 `CrossPlatformAdapterFunction.h` 紧接着 `#undef min/max`),仍改成 `(std::max)` 与仓库写法统一 | — |
| 评审修复(2026-10-15) | 已改完,未编译 | **gate**:断线通知的 SessionDetails 补 `session_incarnation`(原先为 0,login 的 Close 整段跳过,§6.2 三种竞态在路由模式下一个都没关掉),`client_message_processor.cpp`;`OnRouterNodeRemoved` 摘出在途项仍同步做,回执与 Pump 推到下一轮 loop(原先同步 Pump 会以 1/N 概率把下一条发给正在被摘的路由服),`client_request_lane.cpp`;`PlayerLeaseExpired` 就地摘会话之前补 `OnSessionClosing`,`gate_event_handler.cpp`(守护段内)。**login**:Disconnect / LeaveGame 对会话头的玩家与栅栏绑定的玩家各收尾一次(`sessionPlayersToSettle`,换角时会话头是旧角色);`CheckEnterGame` 改为 Lua,入口建记录并续期(签名加 ttl);`bindSessionFence` 的补偿改用 `WithoutCancel` 派生的独立 ctx,BindPlayer 报错也补挂断线租约;`CommitLogin` 对非正 TTL 显式报错。新增单测:`fence_test.go` 3 例(共 13 例;上面 D 行写的「11 例」是笔误,当时实为 10 例)、`session_fence_wiring_test.go` 4 例(共 9 例)。**match**:WatchBattle 换场清退之前检查请求 ctx;AddObserver 截止 / 取消 / 不可用(结果未知)时先补发 RemoveObserver 再回滚标记(`errAddObserverOutcomeUnknown`);新增 `request_deadline_fence_test.go` 2 个用例(其一 3 个子用例)。挑战推送不跟随请求截止,原因与代价写进 `match_service.yaml` 的 `KafkaWriteTimeoutSeconds` 注释。**robot**:`sendAndRecvTimeout` 遇信封错误返回 `envelopeError`(§4.7 订正注);`ordering_smoke_scenario.go` 文件头 O2 与耗时订正。**注释**:`go/match/internal/team/server.go`、`bin/etc/base_deploy_config.yaml` 的 Timeout 旧值。**文档**:§4.7 订正注、§6.2 表 Disconnect / EnterGame 入口 / LeaveGame 三行、§13 第 2 步覆盖清单 | 待 Codex:gate `msbuild game.sln /m:1`;`go/login`、`go/match`、`robot` 各 `go build ./... && go vet ./... && go test ./... -count=1` |
| F Java(评审修复) | 已改完(2026-10-15,Java 仓未提交;F 前三部分已在 Java `88b9e65`、`18c48b8`) | `BattleUplinkRejectedTest`(两处 `backendQueues` → `backendQueue`,原先 test-compile 失败)、`MatchBackendForwardingTest`(「match 在途不阻塞别的后端」改为新语义:好友排在 match 之后)、`ClientDispatcherTest`(聚宝斋热关停用例先完成 friend 再发 trade)、`QueueServiceTest`(经 `QueueHandlers.Join` 的读序多一次 `presence`)、`ClientDispatcher` 类注释与 `docs/reference/mmorpg-client-contract-login.md` 第 4 条(gate 级失败一律带 id 的信封)。未做:在线目录残留条目误拒新会话的 owner_epoch 细化(窗口正常为毫秒级,put 失败时最长 60 s),登记在 PARITY | 待 Java 仓构建测试 |
| E2 guild expected_guild_id | 推迟(2026-10-15 用户决定以后再做) | 不改。理由与残留窗口见 §11 的 2026-10-15 拍板记录;重启时按 §6.4 与 §12 E2 行实施,需要客户端仓授权(AGENTS §9),两版加客户端同批 | — |
| C 与 RPC0 回包路径的协调 | 已合(2026-10-15,「C++ RPC 异步回调链式调用设计」会话改) | `router_lane.h` 的 `InflightIndex` 改为 `cpp/libs/engine/core/utils/data_structures/inflight_index.h` 的别名(语义逐行相同,多 `ExtractAll` / `Empty`;加了类型等同的 static_assert),`router_lane_test.cpp` 独立编译命令多 `-I cpp/libs/engine/core`(§13 第 2 步已注明);`SendEnvelope` / `FindLiveSession` 从匿名命名空间导出到 `gate_lane`,供 scene 的 TCP 回包路径复用,`[lane]` 汇总的 `incarnation_mismatch` 从此也包含该路径;`client_message_processor.cpp` 的 `HandleTcpNodeMessage` 改为非 Empty 走 `CallMethod`、超时 `kGateToSceneClientCallTimeout` 12s,新增不变式 I5(已登记 §5.2)。本文的三处评审修复对方确认未动 | 待 Codex |
| C++ 完成度核对(2026-10-15) | 已跑完,结论待主会话逐条读复核意见 | 只读工作流 `wf_6fae7fc0-8f5`(4 个视角 + 各 1 个对抗复核),结果在会话 tasks 目录 `w3zah72i2.output` 与 journal。已读到的报告(复核结论尚未逐条核对):**C-1(major)** muduo_h2 传输下 gate 关机时断线通知走 `SendRawViaRouter` 的官方 gRPC 回退分支,对没有 CQ / Stub 的路由服实体 `registry.get`,可能崩溃(`client_message_processor.cpp:211-226`);**C-2(minor)** 顶号 `KickPlayerEventHandler` 只 shutdown,lane 排队项要等断开回调才丢弃(`gate_event_handler.cpp:113-128`);**C-3(minor)** battle / GM 闸门拒绝仍是不带 id 的 tip(O6 / 阶段 0d,客户端可见)。其余视角的报告被截断,未读 | 待续 |
| C++ 完成度核对:复核结论与修复(2026-10-15) | 已改完,未编译 | 复核确认成立并已修:① `client_request_lane.cpp` Pump 里不加限定的 `eNodeType` 且没有 include `node_util.h`,gate 编不过 → 改为 `common::base::eNodeType::ClientRpcRouterNodeService`(与 `grpc_call_deadline.cpp` 同写法);② muduo_h2 + 路由模式下关机,断线通知经 `SendRawViaRouter` 落进官方 gRPC 分支,对没有 CQ / Stub 的实体 `registry.get` → `client_message_processor.cpp` 在官方分支前加 `all_of<grpc::CompletionQueue, ClientRpcRouterStubPtr>` 判断,两种传输都不可用时按「没有路由服」返回 nullopt(采样 WARN);③ O7 启动日志 → `main.cpp` 出口模式日志两支各补一句(路由模式注明 lane 已启用及上限,直连模式注明不保序);④ §4.10 / §4.11 文档漂移已订正。被复核否掉的:顶号只 shutdown(C-2 / F2)、直连模式 player_id 归一(C-5)、I4 无机械门禁(C-6)、非 lane 回包不核代次(F5)、inflight_index.h 未跟踪 / Release 链接清单(信息项)。**待决定**:C-3 battle / GM 闸门拒绝改成带 id 的信封,与 Java 版 scene-battle-spec D12(明确要求 23 推送、不是信封)冲突,属于客户端契约变更,未改,等用户拍板。生成物 7 / 8 号字段仍待 Codex 重生成 | 待 Codex:proto 重生成后 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64` |
| C-3 闸门拒绝回执带 id(两版同批,2026-10-15) | 已改完,未编译、未测试 | 用户拍板「改成带 id 的信封」。**C++**:`client_message_processor.cpp` 的 GM 闸(1006)与 battle 拒绝(1003)由 `SendTipToClient` 改为 `gate_lane::SendEnvelope(session, request->id(), request->message_id(), tip)`;GM 闸照旧计非法包与踢线阈值。白名单拒绝有意不回执(§4.1 订正注);scene 未就绪与直连模式找不到节点仍是 23,未改。**Java**:`ClientDispatcher` 两处改 `envelopeError`,类注释同步;测试 `BattleUplinkRejectedTest`(新 `expectRejected`,`expectTip1003` 只留给「没进场景」那条)、`ClientDispatcherTest`(新 `envelopeTipOf`,战斗上行 7 处、GM 2 处);robot `BattleSupport.lobbyRejection`(新)、`BattleScenario` / `BattleSettleScenario` 第 2 步、`AttributeScenario` / `CurrencyScenario` 的 deny 分支,以及 `RobotOptions` / `BattleIds` / `GuildEconomyScenario` 说明;文档 `scene-battle-spec.md` D12 与 :1222、`battle-node-spec.md` :364 / :1688、`architecture.md` :788、`data-tools-spec.md` :766 / :949、`inventory/contract-robot.md` :110。Go robot 已在上一轮改为读信封错误,本仓不需要再改 | 待 Codex:gate `msbuild game.sln /m:1`;Java `mvn -pl game-gate,game-robot -am test` |
| C-3 复核与收尾(2026-10-15) | 已改完,未编译 | 只读复核工作流 `wf_5b962971-3bb`(C++ / Java gate / Java robot 与文档三个切片,各配对抗复核):没有编译或语义问题;确认 9 条轻微问题均已修。**本仓**:`robot/travel_smoke_scenario.go` 的 GM 闸诊断(信封 tip 1006 单独报「gate 的 GM 闸拒绝,检查 GATE_RUN_MODE」,不再一律归因限流;上一行写的「本仓 Go robot 不需要再改」对这里不成立)与 `robot/etc/travel_smoke.yaml` 前置条件;`turn-based-battle-server.md` D66 加订正注;§4.1 订正注补全仍是 23 号 tip 的清单(两种模式 2 处 + 直连模式 3 处)。**Java**:game-gate `GateMetrics` / `GateLimits` / `MessageRoute` / `MessageRoutes` / `MessageRoutesTest` 注释;game-robot `RobotOptions` 帮助、`README.md`、`BattleScenario` / `BattleSettleScenario` 类注释、`TradeScenarioTest` 一处描述;文档 `architecture.md`(另修 :101 / :106 两处 login 失败形状,属上一批遗留)、`scene-battle-spec.md`、`battle-node-spec.md`、`inventory/contract-robot.md` 的残留表述 | 待 Codex:`robot` 下 `go build ./... && go vet ./...`;其余同上一行 |
| E2 guild expected_guild_id(收尾第一批,2026-10-15) | 已改完,未编译、未测试 | 用户「全部做完」取代「E2 推迟」(§11)。proto:`proto/guild/guild.proto` Leave / Disband / Kick / Transfer 加 2 号、SetRole 加 3 号 `expected_guild_id`(生成物已有人重生成)。go/guild:`internal/logic/expected_guild.go`(新,预检 + 事务内复核 + `guild_manage_expected_guild_total{method,outcome=absent\|mismatch}`)、`guild_logic.go`、`guild_manage_logic.go`、`internal/data/guild_manage_repo.go`(新增 `disbandRefusal`:解散时在同一把 guild 行锁下区分「不是帮主」与「已不是成员」,旧客户端两者都仍答 kGuildNotLeader);测试 `expected_guild_test.go`、`expected_guild_integration_test.go`(-tags integration)、`internal/data/request_deadline_fence_test.go`、`expected_guild_repo_test.go`(真库,需 `GUILD_TEST_MYSQL_DSN`)。Java 同批:`GuildAccess.bindExpectedGuild` / `GuildService` / `GuildManageService` / `JdbcGuildStore.disbandGuild` / `GuildTip.NOT_IN_EXPECTED_GUILD` / `GuildTips.forDisbandReject` / `GuildMetrics`(`game_guild_expected_guild_total`)/ `ExpectedGuildMetrics`(新)/ `ExpectedGuildServiceTest`(新)等,`docs/porting/guild-spec.md` §3.19;契约副本待 ContractSync。客户端仓未改 | 待 Codex:`go/guild` 构建测试与真库 / 集成用例;Java 先 `ContractSync --check` 再同步、`clean install`、`-pl game-guild -am test`(命令见 PROGRESS 同日条) |
| login 客户端写方法截止核对(收尾第一批,2026-10-15) | 已改完,未编译 | 栅栏之外经路由服的 login 写方法只有 CreatePlayer 与 RefreshToken。`createplayerlogic.go` 三处截止检查、名字登记发出前查 ctx、7b 改 WithoutCancel + 1s;`pkg/token/token.go` 轮换合成一段 Lua `rotateRefreshScript`(作废与签发原子)。新测试 `createplayer_deadline_test.go`(5 例)、`token_test.go`(5 例);`createplayer_name_test.go` 一例跟随;`login.yaml` 只改注释。CreatePlayer 子预算未重分(§6.1 订正注) | 待 Codex:`cd go/login && go build ./... && go vet ./... && go test ./... -count=1` |
| §6.1 社交服务复核(收尾第一批,2026-10-15) | 已改完,未编译 | 只补测试、没改业务代码,结论见 §6.1 订正注。上一次中断留下的 `go/chat/internal/logic/request_deadline_fence_test.go`、`go/friend/internal/data/request_deadline_fence_test.go`、`go/trade/internal/data/request_deadline_fence_test.go` 已核对;新增 `go/friend/internal/logic/request_deadline_fence_test.go`(6 个写 RPC)、`go/match/internal/team/request_deadline_fence_test.go`(开战锁 + 11 个写 RPC),`go/trade/internal/logic/jubaozhai_logic_test.go` 补完。§14 第 4 条:已补单测,待运行 | 待 Codex:`go/chat`、`go/friend`、`go/trade`、`go/match` 各 `go build ./... && go vet ./... && go test ./... -count=1` |
| O6 剩余 gate 级失败回执带 id(收尾第二批,两版同批,2026-10-15) | 已改完,未编译、未测试 | **C++**:`client_message_processor.cpp` 新增 `SendRequestErrorEnvelope`;`HandleTcpNodeMessage` 两处(scene 未就绪、RpcClient 未挂)与直连 `HandleGrpcNodeMessage` 两处(解析失败、找不到业务节点)由 23 号 tip 改为带 id 与原 message_id 的信封,不计非法包、不断连不变;`rpc_reply_bridge.cpp` 的非 lane 失败桥未改(拿不到客户端请求 id,§4.1 订正注)。**Java**:`ClientDispatcher` 的「玩家不在场景」「未接入的域」由 23 {1003} 改为 `envelopeError`;另对齐 `links.send` 返回 0(链路层不受理)时也回信封 1003(原为不回任何回执);`GateMetrics` / `GateConfiguration` 注释;测试 `ClientDispatcherTest`(新增「scene 链路不受理这一帧」用例)、`ClientPipelineTest`、`BattleUplinkRejectedTest`(删 `expectTip1003`)、`ClientRedirectTest`、`MessageRoutesTest`、`GateConfigurationTest`;robot `GameConnection` 注释、`TradeScenario`;文档 `mmorpg-client-contract-login.md` §2.4、`zone-travel-spec.md` :779 / :1058、`architecture.md` :1897。**客户端可见**:scene 未就绪时所有 scene 类请求(含应答为 Empty 的移动号)回以该请求号为 message_id 的错误信封,不再推 23,客户端要对这些号的信封错误做通用处理。**已知 robot 缺陷(未改)**:本仓 Go robot 的通用分发 `robot/logic/handler/message_body_handler.go` 不看 `error_message`,错误信封的空 body 会交给类型化 handler(例如 ListSkills 清空技能表);这是既有问题,travel_smoke 的 GM 流程又依赖 handler 被调用来关闭等待通道,改它要连带调整,留作后续 | 待 Codex:gate `msbuild game.sln /m:1`;Java `./mvnw -B -pl game-gate,game-robot -am test` |
| 交接(2026-10-15) | 本会话停止,交给别的账号续做 | 收尾第二批的「Java 后端截止后不提交 + 误拒窗口」「保序冒烟补齐」两块因认证报错(403)未开工,工作区无半截改动;剩余工作、验证命令与待拍板项见 [handoff-client-request-ordering-20261015.md](../handoff/handoff-client-request-ordering-20261015.md) | — |
| W1-45 删除 lane 的重复摘节点机制(2026-10-16,gRPC 迁移会话) | 已改完,未编译 | ECS 审查转交:删 `gate_lane::OnRouterNodeRemoved`、`InflightCall::routerEntity`、`LaneCounters::nodeRemoved`(汇总日志去掉 `node_removed`)、`InflightIndex::ExtractIf` 及其单测;`RouterSendFn` / `SendRawViaRouter` 改为返回 bool;`OnNodeRemoveEventHandler` 不再调 lane。路由服摘除改由 `~Channel` 的 UNAVAILABLE 失败回调经 `OnRouterCallFailed` 认领,客户端回执仍恰好一次,`late_dropped` 不再被摘除重复计数。§4.5 表、§4.4 伪码、汇总字段已订正 | 待 Codex:gate `msbuild game.sln /m:1`;`router_lane_test` 按 §13 第 2 步的独立编译命令复跑,用例由 12 例减为 11 例(删 `ExtractIfSelectsByRouterEntityForNodeRemoval`),预期 11 例全绿。Java 版:只改 C++ gate 内部的完成认领路径,客户端可见行为不变,不适用 |
| F Java(第 4 部分:后端「gate 放行后不再提交」,交接 §3.1,2026-10-16) | 已改完,未编译、未测试(Java 仓未提交,基线 `76e7bad`) | 逐方法核实六个模块。gate 放行同会话下一条的时刻 = min(后端回包, 发出 + 5000 ms Dubbo 超时),Dubbo 调用以错误结束时立即放行(残留,见 PROGRESS 同日条)。**game-match**:新增 `port/RedisClientWrites`(客户端写句柄:retryAttempts(0)、timeout = min(`game.redis.timeout-ms`, 剩余)),票据 enqueue / createMatched / cancel / heal、切磋 invite / consume、观战 acquire 改走它,截止已过不发;`QueueService` 跨会话栅栏加 `staleEntry`:在线目录条目 owner_epoch(非 0)小于位置记录时按缺席放行,修掉 PARITY 158 行「已知残留」的误拒;Queue / Challenge / SpectateStore 三个 Configuration 注入 `RedisProperties`。**game-login**:`ClientMessageDispatcher` 在 Dubbo 线程记受理时刻,经 `ClientMessageHandler` 三参 `handle` 传排队时长;CreatePlayer 3.5 s(发号前查,用完回 2020)、EnterGame 4 s 从受理起算(退避醒来已过预算不再夺权、预算用完不再请让出、注定空转的末次重试当场回 2005)、RefreshToken 3.5 s(`LoginTokens.refresh(token, pastDeadline)` 在发轮换脚本前查,放弃回 in-band 1003、旧 refresh 仍有效)。EnterGame 后台链复核:没有永久在线 / 双绑竞态(future 等整条链,迟到应答被 Dubbo 丢弃,绑定只在 gate 会话线程执行),Java 不需要批次 D 的会话栅栏;有界残留是设备登记迟到写(30 min 自愈)与超时后的孤儿 epoch(30 s 租约)。**game-friend**:Reject / Unblock 改显式 RC 事务、提交前查预算;发申请与在线目录配额 INCR 截止已过不发、不重发;NOSCRIPT 回退自处理(自发 EVALSHA,加载与重发前各查一次截止)。**game-chat**:占位 / 限速 / 写历史经 `clientWrite` 发出前查截止、不重发、超时取剩余,写历史前已过截止就释放占位回 1003;NOSCRIPT 同上。**game-trade**:收藏写入加写入预留(预算 3500 时 1000 ms,受理后 2.5 s 起不再发收藏写、回 in-band 1003),锁等待落定 ≤ 受理 + 4.5 s。**game-team**:`TeamStore.eval` 对写脚本先查截止,`RedissonTeamRedis.evalWrite` 自发 EVALSHA、不重发、NOSCRIPT 按截止决定是否加载重发。每个模块至少一条「截止已过 → 不提交」单测。各条都经对抗评审与修复 | 待 Codex(命令见 docs/PROGRESS.md 2026-10-16 条) |
| R 冒烟补齐(交接 §3.2,2026-10-16) | 已改完,未编译 | 新文件 `robot/ordering_smoke_groups.go`:O5 帮会 ApplyJoinGuild→CancelGuildApplication(robot_9902 建临时帮、9903 申请,帮主侧 ListGuildApplications 判定)、O6 好友 AcceptFriend→RemoveFriend(9905 申请、9904 连发,查双方好友列表)、O7 登录 Login→EnterGame(9906,新连接不等 Login 回包就发 EnterGame,回 kLoginSessionNotFound 记乱序,回 0 还要等进场推送);各 5 / 5 / 3 轮,开局安全清场,收尾解散 / 撤回 / 删除;乱序不当场退出,O3 与 O5–O7 的乱序最后汇总成一行 FAIL;帮会组并发撞锁(kGuildBusyRetry)记为乱序。`ordering_smoke_scenario.go` 只加调用、记录过滤与文件头,`etc/ordering_smoke.yaml` 更新流程 / 前置 / 耗时。O4 编号留给未合入的 worktree `claude/practical-bouman-1beb0d`(LeaveGame→JoinQueue,只约束主机器人);合入时把 yaml「预期」行改成汇总写法 | 待 Codex:`robot` 下 `go build ./... && go vet ./...`、`gofmt -l`、冒烟 |
| E2 robot 冒烟(交接 §3.2 可选项,2026-10-16) | 已改完,未编译 | `robot/guild_smoke_scenario.go` 第 11 步:第 10 步解散后 A 另建临时帮 H、B 入帮;B 带已解散旧帮的 `expected_guild_id` 退帮 → kGuildNotInGuild,B 仍是 H 的普通成员、H 仍 2 人;对照:带 H 的 id 退帮照常受理;A 解散 H。`etc/guild_smoke.yaml` 前置 6:guild 须是 E2 二进制,旧二进制以 step=stale-expected-leave、tip=0 失败 | 待 Codex:guild 冒烟 |
| 第三波补齐(2026-10-16) | 已改完,未编译、未测试 | **Java game-match**(Java 仓未提交):由 team 客户端请求发起的 211 整组建票(`createTeamTickets` → `GroupTickets.create` → `RedissonTicketStore.createGroup`)改走客户端写句柄;生产缺省组合(team `SQL_AUTHORITY` + match 非全球)下 `createDurableTickets` 也落到这条路径;回滚与 `releaseTeamTickets` 是补偿,保留重发。`RedisClientWrites` 自发 EVALSHA,NOSCRIPT 时先查截止再加载、加载后再查才重发;超时 = min(配置, 剩余 − 一个 Redisson 定时器刻度 100 ms),不足 1 ms 不发,保证放弃等连接落在截止之前(gate 在后端回包即放行,team 一跳超时后立刻回包,不能拿 5 s 作基准)。新增 `RedisClientWritesTest` 与 `RedissonTicketStoreTest` 整组建票 3 例。**mmorpg Go**:§6.1 2026-10-16 订正注所列两处,`go/friend/internal/data/block_repo.go`、`go/trade/internal/data/listing_repo.go` 改为单语句事务,`request_deadline_fence_test.go` 两包各补「语句跑过截止不提交」「截止前提交恰好一次」;两包 MySQL 集成测试改引生产 SQL 常量;friend / trade `svc/servicecontext.go` 的 BuildDSN 注释同步 | 待 Codex(命令见 docs/PROGRESS.md 2026-10-16 第二条) |
| Java gate 结果未知延后放行(2026-10-16) | 已改完,未编译、未测试(Java 仓未提交) | §10 的 2026-10-16 订正注:`ClientDispatcher` 记发出时刻,future 异常完成时回执照常立即给、lane 到「发出 + 5 s」(`BACKEND_RESULT_UNKNOWN_HOLD_NANOS`,与 `dubbo.consumer.timeout` 同值)才放行;同步抛出 / 返回 null 不等;`ClientSession.scheduleBackendRelease` / `cancelBackendRelease`,断线撤销。测试:`ClientDispatcherTest` 改写「好友后端调用失败…结果未知时等满调用超时才放行下一条」、新增「结果未知的延后放行期间断线_定时器撤销_排队项不再发出」,`MatchBackendForwardingTest` 的 148→153 用例按新语义推进时钟。Java `docs/design/architecture.md` 加更正注 | 待 Codex:`./mvnw -B -pl game-gate -am test` |
