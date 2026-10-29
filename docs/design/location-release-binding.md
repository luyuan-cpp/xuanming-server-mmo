# 删 location 绑定"持有者已释放"(审计 §2.1 修复)

- 日期:2026-10-18
- 状态:设计定稿,代码与测试同批落在工作区;**未编译、未测试,待 Codex 验证**(命令见 §10 与 `docs/PROGRESS.md` 同日条目)。
- 来源:[data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.1 的三条 medium:
  load-handover#1(⊇ login-session-route#0)、load-handover#0、cpp-save-producer#0(= load-handover#2)。
- 关联:[scene-owner-reentry-barrier.md](scene-owner-reentry-barrier.md)(owner_epoch 与换手门)、
  [scene-enter-session-fence.md](scene-enter-session-fence.md)(登录代次)、
  [scene-enter-chain-residuals.md](scene-enter-chain-residuals.md)(b 离场身份、c 释放栅栏)、
  [player-async-save-loss-windows.md](player-async-save-loss-windows.md) §4.1、
  [gate-login-rpc-boundary.md](gate-login-rpc-boundary.md)(LeaveGame 走 gate → login)、
  [cross-zone-scene-travel.md](cross-zone-scene-travel.md) §12.6(A1′ / A2′)。

## 1. 问题

`player:{id}:location` 是换手门(CZ-4)的入口条件:location 在,跨节点落点必须出示当前 owner_epoch 的 handoff 标记;
location 不在,scene_manager 按"首次落点"处理 —— 铸造新 epoch、不过换手门(`enterscenelogic.go` 的 `crossNodeHandoff`
要求 `currentLoc != nil`,`guard.mint = !samePhysicalNode`)。

修复前,location 的删除只看"会话结束",不看"持有它的 scene 是否已经落盘、释放":

| 路径 | 删除点 | 持有节点此刻的状态 |
|---|---|---|
| (1) LeaveGame | login → player_locator `MarkOffline` 删会话后**同步**调 `LeaveScene` | gate 不处理 LeaveGame,TCP 不断就不会代发 ExitGame:实体还活着、没退出、没存盘 |
| (2) 断线租约 | 30s 到期,`LeaseMonitor` 调 `LeaveScene` | gate 断线时已代发 ExitGame,但退出存盘可能还在 Redis 重试里(16s 封顶持续重试,永不放弃,§4.1) |
| (3) gate 崩溃 | player_locator 对账器把会话转租约 → 同 (2) | 没有任何 ExitGame:实体成了僵尸 |

删掉之后的下一次进场:首次落点铸 E+1 →
- 落到别的节点:新节点从 Redis 读到持有节点**上一次成功存盘**的快照;持有节点之后的存盘被 owner CAS 拒
  (`redis_client.h` 的 `SaveWithCommitBarrier`),`HandlePlayerSaveRejected` 把实体当"已废黜"直接销毁(不存盘)。
- 落回同一节点、同一频道:同一条连接上 gate 的 `ApplyRoute` 只看 `pendingEnterGsType` / scene_id / 节点有没有变,不看
  epoch,E+1 的路由被当成重复事件吞掉(load-handover#0)。实体永远停在 E,下一次真正写盘必然被拒并销毁。

两种结局都是**回档到上一次成功存盘**。

`markofflinelogic.go` 的同步 `LeaveScene` 本身是为修"正常登出 location 永久残留"加的:那时 location 永不删除,
幽灵人数 +1、destroy-on-empty 永不触发,多节点区服下次登录选到别的节点被换手门永久拒绝。修复不能把它带回来(§5)。

## 2. 结论一览

| 问题 | 决定 |
|---|---|
| 什么时候允许删 location | 只有拿到"持有者已释放"的证据(§3 不变量 R1):当前代 handoff 标记已写出、持有节点确认已死、location 本身没有持有者。拿不到就**保留** location,回可重试错误 |
| 谁让持有者释放 | scene_manager 在拿不到证据时向持有节点发 `ReleasePlayer{expected_owner_epoch=E, session_ended=true}`;持有节点持有 → 按客户端断线退出(存盘后 A1′ 写标记);不持有 → 按 owner_epoch==E 条件补写凭证标记(*2026-10-23 更正:改为带围栏的凭证 —— 先读 `save_barrier.handoff_fence` 原件,确认是本进程实例在 E 下的资格、本节点仍完全不持有,才以该原件做 CAS,见 §3.3*) |
| LeaveGame 的持有节点 | gate 收到 LeaveGame 时向会话绑定的 scene 代发 ExitGame(与断线同一封包),并清空会话的 scene 路由状态 |
| 重试 | player_locator 现有清理通道(at-least-once claim);MarkOffline 入队首拍延后 2s,之后按 claim 超时节拍;登出清理不给 gate 发 LeaseExpired |
| 永久卡死的兜底 | 等待超过 `LeaveSceneHolderReleaseWaitSeconds`(默认 600s)仍拿不到证据 → 按旧行为删除并计 `forced`、打 ERROR(有记录的 fail-open,§3.6)。*2026-10-23 批注:已被 §15(10-21)取消,等待超时不再授权删除;"持有节点活着而不持有"只能靠上一行的凭证收口,所以凭证不能拒发(§3.3 第 2 项的更正)* |
| gate 同节点新 epoch | `ApplyRoute` 加第 4 条:路由 epoch 大于上次实际转发给 scene 的 epoch 时,以 0 再转发一次 |
| 回档证据 | scene `[OwnerEpoch]` 行新增 `stale_owner_rejected_exiting`(退出中的实体被 owner CAS 拒 = 退出存盘从未落地);scene_manager `scene_manager_leave_holder_release_total{verdict}`。都不以 player_id 作 label |

## 3. 不变量与方案

### 3.1 不变量

- **R1(删 location 的前提)**:scene_manager 删除 `player:{id}:location` 之前,必须拿到以下证据之一,证明没有任何节点
  还持有该玩家在当前 owner_epoch(记 E)下**未落盘**的状态:
  1. `player:{id}:handoff` 的标记 epoch == E(A1′ 退出标记、交接标记、改派标记、本文的凭证标记都满足"E 代状态已落盘、
     写标记者不再写 E");
  2. location 的持有节点在 etcd 注册表里正面消失且再入屏障已过(`playerLocationOwnerDead`,与 EnterScene 死节点接管同一判据)。
     **不认**"zone 负载集为空"(`playerLocationOwnerGone` 修复前的判据;EnterScene 的陈旧位置判定同日也改为要求注册表正面证据,见 §14.5):
     负载键丢失 / 被淘汰时 ZCARD 读作 0、ZSCORE 为空、也没有 death_at,会把活着的持有节点判死,每次登出都立刻删 location
     (第二轮审计 r2-sm#3,2026-10-19 起,见 §14);
  3. location 没有持有者(`node_id` 为空的等待落点)。

  例外(沿用旧行为删除并计数):E == 0(存量玩家,存盘本来不带 CAS,epoch 守卫保护不到);dev 旁路
  `AllowUnsafeCrossNodeHandoff=true`(换手门本身已被旁路);功能关闭(`LeaveSceneHolderReleaseWaitSeconds < 0`;配置为 0 取默认 600s,不算关闭)。
- **R2(判定与提交无窗口)**:证据 1 在删除 Lua 里原子复核:location 原文未变、owner_epoch 仍为 E(缺键视作未变)、
  标记 epoch == E,三者同时成立才删。Go 侧的预读只决定走哪种模式。
- **R3(释放请求只发给可证明属于离场会话的持有者)**:只有离场身份判定为 `deleted`(location 的登录身份 ≤ 被删会话,
  scene-enter-chain-residuals.md §3.2)时才发 `session_ended` 释放;`legacy`(任一侧无身份,混版窗口)只等证据、不发,
  因为那条 location 可能属于一次更新的登录,退出它等于踢掉在线玩家。
- **R4(释放只看归属代际)**:scene 侧沿用 `release_fence::Decide` 的严格相等;交接在途、缓存与期望不等一律不动实体。
  凭证只在本节点**完全不持有**该玩家(没有实体、没有在途载入、没有改派待确认条目)时写,且以 owner_epoch == E 为条件。
  *2026-10-23 更正:只以 owner_epoch == E 为条件挡不住同 E 的迟到写;凭证改为消费本进程实例在 E 下登记的资格原件的 CAS,
  "完全不持有"另加在途的入场登记与未落地的存盘两项,见 §3.3 第 2 项的更正。*
- **R5(gate 先通知再遗忘)**:gate 清空会话的 scene 指向之前,必须先向该节点代发 ExitGame;之后同一条连接上的进场
  只能由新的路由事件驱动,不按 LeaveGame 之前的旧路由补发。
- **R6(location 不永久残留)**:持有者活着时 R3 的释放会让它退出或写凭证;持有者死了走证据 2;两者都拿不到时 §3.6 的
  等待上限兜底。*2026-10-23 批注:§15(10-21)起等待上限不再授权删除,"持有者活着而不持有"只能靠凭证收口,所以 r2-lc#2
  之后凭证不能拒发,改为带围栏(§3.3 第 2 项的更正);拿不出资格的残余见那里。*

### 3.2 scene_manager:LeaveScene

在现有"SceneId 比对 → 离场身份判定"之后插入释放判定(`leave_release.go`,纯函数 `decideHolderRelease` + 粘合):

| 顺序 | 条件 | 结论(指标 verdict) | 动作 |
|---|---|---|---|
| 0 | 身份 `keep_newer` / `keep_conflict` | — | 不变:不删、回成功 |
| 1 | 功能关闭或 dev 旁路 | `disabled` | 原文 CAS 删除(旧行为) |
| 2 | E == 0(owner_epoch 键与 location 记的 epoch 都为 0) | `legacy_epoch0` | 原文 CAS 删除 |
| 3 | location `node_id` 为空 | `no_holder` | 原文 CAS 删除 |
| 4 | 持有节点在注册表里正面消失且屏障已过(只认 `playerLocationOwnerDead`,r2-sm#3) | `owner_dead` | 原文 CAS 删除 |
| 5 | 标记 epoch == E | `released` | "released" 模式删除(R2);Lua 复核不过 → 按第 6 行处理 |
| 6a | 其余,等待未超限,身份 `deleted` | `pending` | 保留 location;异步派发 `ReleasePlayer{E, session_ended}`;回 `FailedPrecondition` |
| 6b | 其余,等待未超限,身份 `legacy` | `pending_unproven` | 保留 location;不派发;回 `FailedPrecondition` |
| 6c | 其余,等待已超限 | `forced` | 原文 CAS 删除,ERROR 日志(接受回档风险) |

- E 取 owner_epoch 键的值;键为 0 / 缺失时取 location 记的 epoch(与 EnterScene "两处同时为 0 才算存量"同一口径)。
  键缺失而 location 记着 N 时,释放模式的 Lua 接受缺键(标记只能在键为 N 时写出,已有的 "N:t" 仍是有效证据)。
- 删除 Lua(`luaDeletePlayerLocationIfReleased`)在原脚本上加 owner_epoch / handoff 两个键与模式参数;删 location 与扣人数
  仍在同一次提交里(GO-4 残余的修法不变)。返回 1 已删 / 0 键已不在 / -1 原文已变 / -2 未释放。
- `FailedPrecondition` 不是故障:go-zero 的熔断不计它;player_locator 据此识别"等持有者释放"(§3.4)。
- 等待计时:`player:{id}:leave_release_wait = "{E}:{node}:{sid}:{gen}:{first_ms}"`,EX = 上限 + 600s。第一次拒绝时写入,
  location 的 (E, 节点, 登录身份) 变了就重置;删除成功后尽力删掉。读写失败按"刚开始等"处理(不会因此提前 forced)。
  `first_ms` 与"已等多久"都取 **Redis 服务端时钟**:读、比 scope、登记在同一段 Lua(`luaNoteLeaveReleaseWait`,脚本里 `TIME`)
  里完成,不读任何 scene_manager 副本的墙钟(第二轮审计 r2-sm#2,2026-10-19 起;语义见 §3.6)。
- 释放判定的计数(`scene_manager_leave_holder_release_total` 的删除类取值)与 `forced` 的 ERROR 只在删除 Lua 真正删掉
  (返回 1)之后记;返回 0 / -1(已被别的调用删掉 / 已被并发 EnterScene 改写)时这一次什么都没删,只打一行 INFO,不计
  (第二轮审计 r2-sm#1)。等待类(`pending` / `pending_unproven`)照旧在每次回拒时计。
- 派发 `dispatchSessionEndedRelease`:后台协程,单发 1s 截止,不在内部重试 —— 下一次 LeaveScene 重试会再派发。包级钩子
  `sessionEndedReleaseDispatcher` 只为单测同步断言。

### 3.3 scene_manager → scene:`ReleasePlayer.session_ended`

`proto/scene_manager/scene_node_service.proto` 的 `ReleasePlayerRequest` 追加:

```proto
  // 玩家的登录会话已结束(登出 / 断线租约到期 / gate 消失),没有接手节点。docs/design/location-release-binding.md §3.3。
  // true 时 scene:持有该玩家且缓存 owner_epoch == expected → 按客户端断线退出(存盘落地后写 A1′ 断线释放标记);
  // 本节点完全不持有 → 按 owner_epoch == expected 条件补写凭证标记;其余忽略。false = 交接释放(原语义)。
  bool session_ended = 5;
```

scene 侧 `PlayerLifecycleSystem::HandleSessionEndedRelease(playerId, expectedOwnerEpoch)`(守护段按字段分派):

1. 本节点有该玩家实体:走与交接释放同一个 `release_fence::Decide`;判释放 → `HandleExitGameNode(kClientDisconnect)`。
   已在退出中 → `MergeExitCause` 只粘性置 `clientDisconnected`,原因不变。退出收尾的 A1′ 由既有判定决定写不写。
   (2026-10-18 起 `Decide` 判释放之后还要过一道登录身份判定,实体已绑上更新的登录时不退出,见 §13。)
   - 为什么用 `kClientDisconnect` 而不新增原因:语义就是"客户端会话已死";它在 A1′ 的可写集合里,并且会作废疏散票据
     (给死会话改派没有意义)。来源区分由 `[ReleaseFence]` 行的 `session_ended` 计数承担。
2. 本节点不持有(`playerList` 没有、没有待入场条目、没有改派待确认条目):纯判定 `release_fence::DecideSessionEndedCredential`:
   期望 epoch 为 0、A1′ 开关关闭、本节点身份不确认、dev 旁路开着 → 不写;否则 `SendConditionalMarkWrite(E)`
   (`MarkWriteSite::kSessionEndedCredential`,与 A1′ 同一段 Lua,owner_epoch != E 时不写)。
   安全性与"改派踢线前的凭证补写"同理:本节点没有该玩家的任何内存态,盘上就是最终态;条件写保证不会给更新的一代发证。
   **2026-10-23 更正(r2-lc#2,本条第 2 项改为带围栏的凭证)**:只以 owner_epoch == E 为条件的写挡不住同 E 的迟到写 ——
   存活探测摘掉旧连接后,旧连接上的写可能晚于同节点短重连的重载才执行,在新持有期间留下有效标记。标记写因此一律改为
   `save_barrier.handoff_fence` 原件的 CAS(见 cross-zone-scene-travel.md 文末两节)。本节点完全不持有时手里没有缓存的原 H,
   做法(`SendSessionEndedCredential`):
   - "完全不持有"另加两项:`RoleAdmissionSystem` 的入场登记在途(登记回包前 fence 上可能已是它刚写的 H)、有未落地的存盘。
   - 判定表判 `write` 之后 `HGET player:{id}:save_barrier handoff_fence` 读现任原件;回调(scene 线程)里**重采**判定,再按
     `release_fence::DecideCredentialQualification` 核对:仍完全不持有、原件 E == 期望 E、原件 S == 本进程实例(RoleAdmission
     登记时写入 S 的同一来源 `LocalInstanceId`)。合格才以**读到的原件**做 CAS(`handoff_sql_barrier::kCredentialMarkLua`:
     owner == E 且现任 == 原件 → HSET 轮换后的 F + SET "E:t" EX 300)。原件状态 H / F / R 都认:它们只说明这一代资格怎么结束
     (未写标记就销毁 / 已发布或已释放而标记已过期或被消费 / 登记被撤销),本节点不再持有时盘上就是 E 代终态。
   - 为什么不会复活凭证:原件若属于本节点一条正在建立的持有,读回那一刻它一定还在本地表里(登记在途 / 待入场 / 实体),被重采挡下;
     CAS 发出之后才开始的持有都会先轮换原件(同节点登记同 E 更大 N、跨节点接手先铸 E+1),迟到或被重放的凭证帧只能回 -2 / 0。
     不按读到的 owner_epoch 现造资格,也不替别的节点 / 别的进程实例 / 别的 E 的资格写。
   - 不能拒发了事:§15 起等待超时不再授权删 location,持有节点活着而不持有时(A1′ 没写成 / 被判 skip / 标记已过 300s TTL /
     登记被撤销),没有凭证这条 location 就永远删不掉,跨节点重登每次回 18。
   - 计数(`[ReleaseFence]` 行末追加):`credential_qualification_<qualified|read_failed|holder_appeared|no_qualification|malformed>`,
     后两项增长升 WARN;CAS 结果 `credential_written / epoch_moved / superseded(-2) / failed`。`credential_write` = 资格核对各项之和 + 在途读。
   - 残余(活性,不是数据风险):(1) 本节点从未为 E 登记成功(入场登记在写 fence 之前被拒)时 Redis 里是上一任的资格,判
     `no_qualification`,这条 location 要等玩家下次登录落回本节点重新登记,或持有节点死亡(证据 2)。同号重启顶替(GO-3,
     `owner_replaced.go`)只认「资格原件的 E 等于 location 的 E」,这里原件是上一任的 E,不在其内(`cross-zone-scene-travel.md`
     §13.10 第 2 条残余 5);收口见 §17;
     (2) 凭证 CAS 帧滞留、而同节点新登记的 HGET 恰在它之前执行时,那次登记的 CAS 回 -2、本次登录失败重试;
     (3) 读失败不重试(同 R7),下一次 LeaveScene 重试 / 登录撞门再请。
3. `playerList` 里留着无效句柄 → 不写凭证(表不一致,fail-closed)。

### 3.4 player_locator

- `notifySceneManagerLeave`:`status.Code(err) == codes.FailedPrecondition` → 包成 `errHolderReleasePending`。
- `MarkOffline`:同步 LeaveScene 回 `errHolderReleasePending` 时打 INFO(不是故障),入队的 ready 分数延后
  `holderReleaseFirstRetryDelay = 2s`(给 gate 代发的 ExitGame / 释放请求留出落盘时间,首拍重试大概率拿到标记);
  其它错误照旧立即入队、打 ERROR。
- **入队载荷清空 `gate_id` / `gate_instance_id`**:登出的连接仍在线(停在选角界面)。原先入队的清理到期后会给 gate 发
  `PlayerLeaseExpired`,gate 对仍连着的会话 `forceClose` —— 修复前只在 LeaveScene 同步失败时才入队,这条缺陷少见;
  现在"等持有者释放"是常态,必须一并修掉。
- `handleLeaseExpiry`:LeaveScene 回 `errHolderReleasePending` 时打 INFO,claim 照旧保留、按 claim 超时(30s)重试。
- 不改 claim / 会话 Lua:清理在途时首次登录 SetSession 仍回 `ErrSessionCleanupPending`(既有屏障)。代价见 §6。

### 3.5 gate

**LeaveGame**:`DispatchClientRpcMessage` 在协议分派前对 `ClientPlayerLoginLeaveGame(17)` 调
`gate_scene_entry::OnClientLeaveGame`:

1. `GateClientSessionHandler::NotifySceneExitGame`(从 `HandleConnectionDisconnection` 抽出的同一段代码,带 player_id 反向
   身份栅栏)向会话绑定的 scene 代发 ExitGame;
2. `gate_scene_route::ResetSceneRoute` 清空节点指向、sceneId、待入场登录类型、欠账、lastSceneRoute、ownerEpoch、
   homeZoneId、forwardedOwnerEpoch;保留 playerId、loginGeneration、sessionVersion(下一次 BindSession 按既有规则处理)。

在收包时做而不是等 login 回包:直连 / 路由两种模式共用一个点;LeaveGame 在 login 侧失败时实体退出、location 保留,
下一次 EnterGame 走同落点重连或换手门(均不回档),是良性的。"之后同连接的 Bind 按 LeaveGame 之前的旧路由补发"这类
陈旧补发因此消失:Bind 先到时 `lastSceneRoute.sceneId == 0`,等新路由到达再一起转发。

**重置之后迟到的旧路由**(2026-10-19 静态评审复核成立后补):LeaveGame 之前那条登录链发出的路由可能晚于重置才到 gate
(已在 Kafka 里的跨节点换图;路由模式下钩子在收包时就跑,lane 里更早的 EnterGame 的路由随后才到)。它不会被转发(会话已没有
场景、也没有登录类型),但若照常写会话,会重新填上 `lastSceneRoute` / `ownerEpoch`,下一次 BindSession 就按它、带旧 epoch
补发,让已释放(甚至已发过凭证)的旧节点按旧代重新载入玩家。所以 `ResetSceneRoute` 同时置位 `sceneRouteResetPending` 并记下
此刻的登录代次 `loginGenerationAtReset`;`RoutePlayerEventHandler` 在写任何会话字段之前用 `IsRouteFromBeforeLeaveGame` 判:
同一角色、代次为 0 或不大于记下的值 → 丢弃。新登录的代次由 login 铸得更大(fence G1),它的路由照常收下(先于 Bind 到达也行)。
标记在第一次真正转发进场(`ApplyRoute`)、换角色的 Bind、或不带代次的旧版 Bind 时清除。这是对 fence §4"gate 不据代次丢路由"
的有意例外:那条规则防的是重登失败后仍在游戏里的换图被吞掉,LeaveGame 之后玩家不在游戏里,旧链路由没有合法用途。
残余:旧版 login(代次 0)与重排同时出现、新登录的路由先于 Bind 到达时会被丢,这次登录等到客户端超时重连;登录代次键丢失
且 login 实例时钟偏差使新代次不大于旧值时同理(fence §3.2 已登记的时钟窗口)。

**旧链的 Bind 晚于 LeaveGame 到达**(第二轮审计 r2-gate#1,2026-10-19 复核成立后补):上面只按代次认旧链有缝。EnterGame 已被
login 受理、它的 Bind(gE)还在 Kafka 里时客户端就发了 LeaveGame,gate 收包重置记下的是旧代次 g0;随后 Bind(gE) 与路由(gE)
代次都大于 g0,被当成新登录收下:Bind 把登录类型挂回会话、保留重置标记,路由带着登录类型转发到旧节点 X 并清掉标记。之后
session_ended 释放退掉 X 上的实体、location 被删;同一连接再 EnterGame(gF) 时 Bind 先到,按这条旧路由带 E 补发到 X,而
scene_manager 铸了 E+1 落到 Y(幽灵实体,或 X 回 3023 并耗掉登录类型)。修法(只改 gate):

1. **在客户端再次 EnterGame 之前,一律是旧链**。login 只在处理 EnterGame 时发 Bind(`entergamelogic.go` `SendBindSessionToGate`
   是唯一调用点),所以重置之后、本连接下一次 EnterGame 收包之前到达的 Bind / 路由不可能属于新登录,不看代次、不看角色全部丢。
   `DispatchClientRpcMessage` 在与 LeaveGame 同一个收包点对 `ClientPlayerLoginEnterGame(26)` 调 `gate_scene_entry::OnClientEnterGame`
   置 `enterGameSentAfterLeave`;按收包顺序置位,路由模式下 lane 里排在 LeaveGame 之前的 EnterGame 不会把它置上。
   `BindSessionEventHandler` 新增 `IsBindFromBeforeLeaveGame`,在 `ApplyLoginBinding` 之前丢旧链 Bind(以前 Bind 不过滤)。
2. **之后按代次认,且代次跟着丢弃抬高**。被离开的那条链记成 (`playerIdAtReset`, `loginGenerationAtReset`):重置时取会话当时的
   角色与代次;会话那时还没绑角色(第一次登录就回选角)时取第一条被丢弃事件的角色。每丢一条旧链事件就把它的代次记进去
   (`NoteDroppedFromBeforeLeaveGame`,只抬不降、只收同一角色),同一条旧链在客户端再次 EnterGame 之后才到的兄弟事件(Bind 与
   路由是两条独立事件,路由要等 scene_manager 处理完,常更晚)仍被认出。另加一条:比会话已见过的本角色更新登录的代次旧的
   同角色事件也是旧链(新登录的 Bind 先到、旧链路由后到)。角色不同的代次不可比,不在这里判。重复 LeaveGame 时只抬不覆盖。
   安全性:被丢的事件在客户端再次 EnterGame 之前就已由 login 发出,它的代次必然小于下一次登录铸出的代次(fence G1)。
3. **没有采用审计建议的"LeaveGame 回包时再按 login 侧代次重置一次"**:LeaveGame 的回包是客户端可见的 `LoginEmptyResponse`,
   带代次要改客户端契约(AGENTS §12 两版同批);而只在 lane 有在途 EnterGame 时延后 / 拒绝 LeaveGame 盖不住 login 的后台链
   (EnterGame 的回包早于 Bind 发出)。1、2 两条只改 gate 内部状态。

被丢弃的旧链 Bind / 路由不进会话,旧节点 X 收不到这次进场,不建实体;scene_manager 那边 location 指向 X,LeaveScene 照 §3.2
请 X 按会话结束释放,X 不持有 → 写凭证(§3.3 第 2 条)→ 下一拍删 location,或下一次登录过换手门拿到凭证放行。

残余:旧链的 Bind 与路由**都**晚于客户端下一次 EnterGame 收包、又早于新登录的任何 Bind / 路由到达时,代次比任何已知值都大,
认不出,退回修复前的行为。要 Kafka 消费延迟超过"LeaveGame 回包 → 玩家再点进入 → gate 收包"的整个往返;scene 侧的入场
栅栏与 owner CAS 仍把损失限制在旧节点上的幽灵实体与毫秒级操作丢失。彻底收口需要 login 把"被离开的那条链的代次"交给 gate
(LeaveGame 回包或单独的 gate 命令),属客户端契约 / 跨服务改动,未做。

**ApplyRoute 第 4 条**(load-handover#0):`SessionInfo` 新增 `forwardedOwnerEpoch` = 上一次实际转发给 scene 的
`PlayerEnterGameNodeRequest.owner_epoch`。`sceneId != 0`、`forwardedOwnerEpoch != 0`(scene 此前被告知过一个 epoch)且路由
epoch **大于** `forwardedOwnerEpoch` 时,以 LOGIN_NONE(0) 转发一次;转发成功后记下这次带出的 epoch。

- 只认"更大":epoch 只增不减,迟到的旧路由不值得再转发;0(旧版 scene_manager)永不触发。
- scene 从没被告知过 epoch(路由先于登录类型到达、还没转发过)时不触发:下一次转发本来就会带上会话当前的值。
- scene 侧对它的处置已有:恰好 +1 → 复用实体、`EnterScene` 把缓存抬到新值(退出中的顺带取消退出);≥ +2 →
  `DiscardDeposedEntityOnReentry` 销毁重载(中间有别的持有者,复用才是回档)。gate 以前把这些路由吞掉,scene 的判据从来见不到。

### 3.6 等待上限(有记录的 fail-open)

拿不到证据的合法原因:持有节点存盘卡住(Redis 对该节点写失败)。这正是要等的情形,删了就回档。但还有拿不到证据的
非预期情形:A1′ 开关被运维关掉、实体缓存 epoch 落后于 location(已被废黜的僵尸,`release_fence` 判 `skip_ahead_of_holder`)、
标记写与凭证写长期失败。清理不 ack 会让该玩家的首次登录一直回 `ErrSessionCleanupPending`,不能无限期。

所以等待超过上限(默认 600s,`LeaveSceneHolderReleaseWaitSeconds`)按旧行为删除:计 `forced`、打 ERROR(带 E、节点、
已等时长)。这是 AGENTS §11.3 允许的、带日志与指标的 fail-open:比修复前"立刻删"严格得多,只把回档留给持续 10 分钟拿不到
释放证据的情形。`forced` 正常应恒 0,出现即查。

**时钟语义**(第二轮审计 r2-sm#2,2026-10-19 起):等待时长 = Redis 服务端 `TIME` − 计时键里的 `first_ms`,两者都由同一个 Redis
时钟给出。以前用各 scene_manager 副本自己的墙钟:副本 A 写下 `first_ms`、时钟快 Δ 的副本 B 来算,就多算 Δ,提前 forced;NTP
跳变同理。现在副本间偏差不再进入判定;剩下的只有 Redis 服务端自身的时钟跳变 —— 往回跳时等待时长钳在 0(只会多等),往前跳
会提前 forced,与任何单一时钟的跳变同一量级,不另设防。脚本里调 `TIME`:Redis 5+ 按效果复制脚本,非确定性命令合法(部署是
Redis 7)。脚本出错时按"刚开始等"返回 0,只会多等。

### 3.7 指标

| 指标 | 位置 | 含义 |
|---|---|---|
| `scene_manager_leave_holder_release_total{verdict}` | scene_manager | §3.2 判定表的结论;`pending` 是登出 / 断线后的正常短暂态,`forced` 应恒 0。删除类取值只在真正删掉时计(r2-sm#1),告警 `SceneManagerLeaveReleaseForced` 不再因并发改写误报 |
| `stale_owner_rejected_exiting` | scene `[OwnerEpoch]` WARN 行 | owner CAS 拒绝时实体正在退出 = 这次退出的存盘从未落地、内存差额被丢弃。**回档的直接证据**,应恒 0 |
| `session_ended` / `credential_*` | scene `[ReleaseFence]` 行 | 会话结束释放的次数、凭证判定分布与条件写结果;2026-10-23 行尾追加 `credential_superseded` 与 `credential_qualification_*`(读资格原件的核对分布,`no_qualification` / `malformed` 增长升 WARN,§3.3) |

全部不以 player_id 作 label;玩家号只进日志。

## 4. 时序走查

记 X 为持有节点、E 为当前 owner_epoch,A 为旧会话 (sA, gA)。

**LeaveGame 后不断线就重进(同一连接)**

1. gate 收到 LeaveGame:代发 ExitGame(sA) 给 X,清空会话路由状态;转发 LeaveGame 给 login。
2. X 退出存盘 → A1′ 写 "E:t" → 销毁实体。与此并行:login → MarkOffline 删会话 → LeaveScene:
   - 标记已在 → `released` → 删 location;
   - 标记还没写 → `pending` → 派发 session_ended(X 已在退出,合并后无动作)→ player_locator 延后 2s 入队 → 2s 后重试拿到标记 → 删。
3. 客户端 EnterGame(同 session_id,新代次 gB):Bind 先到 → 会话没有旧路由,不补发;scene_manager:
   - location 已删 → 首次落点铸 E+1。X 已存盘销毁,新节点(或 X 自己)从盘上读到最新状态;
   - location 还在(2s 窗口内重登,首次登录被清理屏障挡住,客户端收 3023 重试;或 LeaveGame 在 login 侧失败、会话仍在)→
     同节点走同落点重连 / 同物理节点,跨节点过换手门(标记在就放行并铸造,不在回 18)。
4. 新路由到达 gate:按新 epoch 转发。

**LeaveGame 后快速重登(新连接或换设备)**:同上,区别只在 gate 会话是新的。location 保留期间登录过换手门,
不再走首次落点;迟到的离场重试看到更新的登录身份判 `keep_newer`(b 的既有规则)。

**断线租约到期时退出存盘还在重试(cpp-save-producer#0)**:LeaveScene 拿不到标记 → `pending`,location 保留、claim 保留,
派发 session_ended(实体已在退出,合并)。Redis 恢复 → 退出存盘落地 → A1′ → 下一次重试删除。期间首次登录被清理屏障挡住
(fail-closed,宁可登不上也不回档);等待超过 600s 才 `forced`。

**gate 崩溃**:对账器把会话转租约 → 到期 → LeaveScene `pending` → session_ended 让 X 上的僵尸实体按断线退出、存盘、写 A1′ →
下一次重试删除。修复前这一步直接删 location,僵尸手里未落盘的进度丢失。

**Bind 先到、按旧路由补发,随后同节点新 epoch 路由**(不经 LeaveGame 的残留路径,如 LeaveGame 在 gate 之外丢失):
补发带旧 epoch E;随后 E+1 的路由同场景同节点,第 4 条判 E+1 > E → 以 0 再转发 → X 复用实体并抬到 E+1。

## 5. 为什么不把"location 永久残留"带回来

那个缺陷的根因是"登出路径从不删 location"。现在:

- 登出 / 断线 / gate 崩溃三条路径都会调 LeaveScene,且失败一律进 at-least-once 重试(不变);
- 持有者活着时,`pending` 会派发 session_ended:持有 → 退出并写 A1′;不持有 → 写凭证。下一拍重试拿到证据就删;
- 持有者死了走证据 2;没有持有者走证据 3;
- 以上都拿不到时 §3.6 的上限兜底,最迟约 10 分钟后按旧行为删除。

所以 location 只会"晚删",不会"不删";晚删期间登录要么落回持有节点(复用内存实体,零损失),要么过换手门。

## 6. 代价与残余

1. **登出后 ~2s 内的首次登录**可能被清理屏障挡住(`ErrSessionCleanupPending` → 客户端 3023 重试),只在同步 LeaveScene
   早于 A1′ 标记时出现。gate 在收到 LeaveGame 时就代发 ExitGame,多数情况下标记先到。
2. **存盘卡住期间该玩家登不上**,最长到等待上限。这是有意的 fail-closed。
3. **`legacy` 身份不发释放**:混版窗口里 gate 崩溃留下的僵尸只能等上限兜底(与修复前同样回档,但晚 10 分钟)。全量升级后
   `pending_unproven` 应恒 0。
4. **`skip_ahead_of_holder` 的僵尸**(实体缓存落后于 location 记的 epoch)不写凭证,等上限兜底。它已被废黜、写不进任何东西,
   兜底删除不丢数据;登记备查。
5. **登录在清理之前、且被选到别的节点**:gate 崩溃后玩家立刻从别的 gate 重登,location 指向僵尸,换手门回 18,而释放要等旧会话
   的清理到期才发。修复前同样回 18(这不是本次引入的),登记为后续:可让登录类 EnterScene 在 `no_marker` 时也派发 session_ended。
   **2026-10-18 已收口,见 §12**(登录撞门时同步请持有者释放;`stale_marker` 一并覆盖)。
6. 换手门证据的强度与 CZ-4 相同:同代标记存在即视为已落盘。撤回失败的交接标记(`WithdrawHandoffMark` 重试中)在 CZ-4
   与本文里是同一个已知窗口,不另做处理。

## 7. 新旧并存与发布顺序

- **发布**:proto 生成 → scene → player_locator → scene_manager → gate。
  - scene 先于 scene_manager:旧 scene 不认 `session_ended`,把它当交接释放(`kReleasedByTransfer`,A1′ 不写),
    不持有时也不写凭证,location 会一直等到上限。
  - player_locator 先于 scene_manager:旧 player_locator 把 `FailedPrecondition` 当普通失败,功能上可用(照样保留 claim
    重试),但入队载荷不清 gate、会踢掉登出后仍在线的连接。
  - gate 任意时刻都可以滚;第 4 条与 LeaveGame 处理不依赖其它组件。
- **回滚**:按反序。scene_manager 可单独把 `LeaveSceneHolderReleaseWaitSeconds` 设为 -1 立刻退回旧行为(不必回滚二进制;0 取默认 600s,不是关闭)。
  **要关 scene 的 A1′ 开关(`SCENE_EXIT_RELEASE_MARK=0`)之前,先把 scene_manager 的上限设为 -1**,否则关掉之后拿不到标记,
  每次登出都要等满上限。
- 不改 Redis 键格式;新增键 `player:{id}:leave_release_wait`(带 TTL,丢了只会让等待重新计时)。

## 8. 测试与验收映射

| 验收项 | 用例(工程) |
|---|---|
| 持有者未释放:保留 location、不扣人数、回 FailedPrecondition、派发 session_ended(期望 = E) | `TestLeaveScene_HolderNotReleasedKeepsLocationAndRequestsRelease`(go/scene_manager) |
| 当前代标记 → 删;旧代标记不算 | `TestLeaveScene_CurrentEpochMarkerReleasesLocation`、`TestLeaveScene_OlderEpochMarkerIsNotRelease` |
| 判定与提交之间标记被撤回 → 不删 | `TestLeaveScene_MarkerWithdrawnBeforeDeleteKeepsLocation` |
| 例外:epoch 0、等待落点、关闭开关、dev 旁路 | `TestLeaveScene_LegacyEpochZeroDeletes`、`..._AwaitingPlacementDeletes`、`..._ReleaseWaitDisabledDeletesImmediately`、`..._DevBypassDeletesImmediately` |
| legacy 身份只等不发 | `TestLeaveScene_LegacyIdentityWaitsWithoutRequestingRelease` |
| 超过上限 forced | `TestLeaveScene_WaitBudgetExceededForcesDelete` |
| 判定表全覆盖 | `TestDecideHolderRelease_Table`、`TestLeaveReleaseVerdictNamesCoverEveryValue` |
| **LeaveGame 后快速重登**:location 保留期间跨节点登录过换手门而不是首次落点;标记写出后放行并铸造;迟到的离场判 keep_newer | `TestLeaveGameQuickReloginGoesThroughHandoffGate`(go/scene_manager) |
| 登出清理:pending 延后入队、载荷不带 gate | `TestMarkOffline_HolderReleasePendingEnqueuesDelayedCleanupWithoutGate`(go/player_locator) |
| 其它 LeaveScene 失败照旧立即入队、仍清 gate | `TestMarkOffline_LeaveSceneFailureEnqueuesImmediatelyWithoutGate` |
| 分类 | `TestNotifySceneManagerLeave_ClassifiesHolderReleasePending` |
| scene:持有 → 按断线退出;代际不等 / 交接中 → 不动 | `ReleaseFenceEcs.SessionEndedReleaseExitsHolderAsClientDisconnect`、`..._SessionEndedReleaseOnStaleEntityIgnored`、`..._SessionEndedReleaseDuringHandoffIgnored`(cross_zone_test) |
| scene:不持有 → 先读资格原件再写凭证(2026-10-23 起,见 §3.3 更正;宿主无 Redis 时记 `read_failed`、不发命令);载入在途 / 入场登记在途 → 不写 | `ReleaseFenceEcs.SessionEndedReleaseWithoutHolderReadsQualificationBeforeCredential`(原名 `..._WithoutHolderWritesCredential`)、`..._SessionEndedReleaseWithPendingLoadSkipsCredential`、`RoleAdmissionTest.RegistrationInFlightBlocksSessionEndedCredential` |
| 凭证的资格核对表 / 名表 / 告警 | `ReleaseFence.CredentialQualificationDecideTable`、`..._CredentialQualificationNamesCoverEveryValue`、`..._SummaryEscalatesOnCredentialWithoutQualification`;Lua 形状 `ExitReleaseMarkWrite.CredentialLuaCasOnObservedOriginalBeforeSet`(cross_zone_test) |
| 凭证 CAS 的行为:原件 F / R 照写、迟到帧在同节点重载后回 -2、owner 推进回 0 | `CurrencyTest.HandoffMarkFramesDelayedOnReplacedConnectionCannotReviveCredentialAgainstRealRedis` 第三段(真 Redis 两条连接);Go `TestLateMarkWritesFromReplacedConnectionNeverOpenHandoffGate/session_ended_credential_*` |
| 凭证判定表 | `ReleaseFence.SessionEndedCredentialDecideTable`、`ReleaseFence.SessionEndedCredentialNamesCoverEveryValue` |
| 回档证据计数 | `OwnerEpochStats.RejectedExitingSaveCountsAsRollbackEvidence`(cross_zone_test) |
| **LeaveGame 后不断线就重进**:重置后 Bind 先到不补发,新路由带新 epoch 与登录类型一起转发 | `SceneRouteEntry.LeaveGameResetThenRelogForwardsFreshRouteOnly`(routing_identity_test) |
| **Bind 先到按旧路由补发,随后同节点新 epoch** | `SceneRouteEntry.BindReplayThenSameNodeNewerEpochIsForwarded` |
| LeaveGame 重置后迟到的旧路由被丢、Bind 不补发;新登录路由先于 Bind 照常;换角色 / 旧版 Bind 退回常规 | `SceneRouteEntry.LateOldRouteAfterLeaveGameResetIsDroppedAndNotReplayedOnBind`、`..._NewLoginRouteBeforeBindAfterLeaveGameStillReplays`、`..._LeaveGameRouteFilterYieldsToCharacterSwitchAndLegacyBind` |
| 第 4 条边界:同 epoch / 更旧 / 0 不转发 | `SceneRouteEntry.SameNodeSameOrOlderEpochStaysDeduped`、`..._ZeroEpochRouteNeverForwardsByEpoch` |
| 第二轮审计修复的回归用例 | 见 §14.2 |

## 9. 改动清单

手写(生成产物由 Codex 重生,不手改):

- proto:`proto/scene_manager/scene_node_service.proto`(`ReleasePlayerRequest.session_ended = 5`)。
- scene_manager:`internal/logic/{leave_release.go(新), leavescenelogic.go, scene_node_client.go}`、`internal/config/config.go`、
  `internal/metrics/metrics.go`、`etc/scene_manager_service.yaml`;测试 `leave_release_test.go(新)`、
  `leavescene_cas_test.go` / `leavescene_identity_test.go`(夹具补释放标记)。
- player_locator:`internal/logic/{leasemonitor.go, markofflinelogic.go, session_cas.go}`;测试 `leave_release_pending_test.go(新)`。
- scene:`player/system/{release_fence.h, player_lifecycle.h, player_lifecycle.cpp}`、`core/system/redis.cpp`、
  `nodes/scene/handler/grpc/scene_node_service.cpp`(守护段)、`cpp/tests/cross_zone_test/cross_zone_test.cpp`。
- gate:`libs/services/gate/session/comp/session_info_comp.h`、`nodes/gate/handler/event/{scene_route_helper.h,
  scene_entry_dispatch.h, scene_entry_dispatch.cpp, gate_event_handler.cpp(RoutePlayer 守护段)}`、`nodes/gate/handler/rpc/{client_session_handler.h,
  client_session_handler.cpp}`(工作区里由别的会话从 client_message_processor.* 改名而来)、`cpp/tests/routing_identity_test/routing_identity_test.cpp`。
- 文档:本文、审计 §2.1、`docs/README.md`、`docs/PROGRESS.md`。

## 10. 验证(Codex)

见 `docs/PROGRESS.md` 2026-10-18「删 location 绑定持有者释放」条目:先 proto-gen,再 Go 两个模块的 vet / test,
再串行 MSBuild 与 C++ 两个测试工程。通过标准:新增用例全绿,`leavescene_*`、`release_dispatch_test`、`ReleaseFence*`、
`SceneRoute*` / `SceneEntry*` 等既有用例不回归。

## 11. Java 版(AGENTS §12)

Java 版(`D:\luyuan\xuanming-server-mmo`)按其 `PARITY.md`(本次只读核对,未读代码)结构上不存在同类问题:

- 位置记录 `game:location:{player_id}` 由持有归属的 scene 唯一写入,没有"会话服务删位置、持有者还在写"这条路径,也没有
  scene_manager 侧的首次落点铸造(PARITY「场景分配」「短线重连与落点」行)。
- 归属靠库行 `owner_epoch` 夺权:新登录撞上仍被持有时请持有的 scene 让出(旧实例写回后释放),持有者 3s 内不让出回 2005、等
  30s 归属租约过期(PARITY「顶号」行),不会从旧快照起新实例。
- LeaveGame 下发 `UnbindPlayer`,gate 向 scene 发 `PlayerLeave{voluntary=true}` 并解绑玩家(PARITY「LeaveGame 后回选角」行):
  本仓本次补的"gate 收到 LeaveGame 代发 ExitGame"在 Java 版本来就有。

结论:本修复在 Java 版**不适用(结构不同)**,需在 `PARITY.md` 登记一行(待本仓提交后补 commit 号)。本次 proto 改动只在
服务端内部(`scene_node_service.proto` 的 `ReleasePlayerRequest.session_ended`),不涉及客户端契约;Java 仓需在本仓提交后跑
`ContractSync` 同步 `proto/` 副本。**待核实**:Java gate 转发层是否也有"同频道同节点按场景去重、吞掉新代际进场"的形态
(对应本仓 `ApplyRoute` 第 4 条)。

## 12. 登录撞上未释放的持有者(§6 残余 5 的收口)

- 日期:2026-10-18。状态:代码与测试落在工作区;**未编译、未测试,待 Codex 验证**(命令见 `docs/PROGRESS.md` 同日「登录撞上未释放的持有者」条目)。
- 只改 scene_manager。C++ 侧复用 §3.3 的 `HandleSessionEndedRelease`,proto 不改。

### 12.1 问题

gate 崩溃:对账器把旧会话 A(sA, gA)转成断线租约,租约到期 → LeaveScene → `pending` → session_ended 释放(§4「gate 崩溃」)。
玩家通常等不到这一步,马上从别的 gate 重登(会话 B,ShortReconnect / ReplaceLogin,`ZoneId = 0`):

1. login 铸 gB > gA,`SetSession(sB, gB)` 提交,Bind 新 gate,调 EnterScene(`correlation_id = 0`);
2. scene_manager 跟随 location 的 zone,按负载选频道,选到别的节点 Y ≠ X → `crossNodeHandoff` → 换手门要 E 代标记;
3. X 上的僵尸实体没有任何人通知,永远不写标记 → 回 18(`handoff_pending_no_marker`;若 300s 内换过图,残留的 E-1 标记让它变成
   `handoff_pending_stale_marker`);
4. login 不重试 18,推 3023;客户端重登,回到第 1 步。直到旧会话的清理到期、LeaveScene 发出释放,才有人让 X 退出。

修复前也是这样(当时 LeaveScene 直接删 location,下一次进场按首次落点铸造 —— 更糟,回档);§2.1 的修复没有引入它,但也没覆盖它。

### 12.2 做法

`enterscenelogic.go` 第 3 步,换手门以 `ErrHandoffPending`(18)拒绝之后、回应答之前,调 `requestHolderReleaseForLogin`
(`internal/logic/enter_holder_release.go`):按 §12.3 判定;合格时**同步**向 location 的持有节点发
`ReleasePlayer{expected_owner_epoch = observedEpoch, session_ended = true}`(`RequestNodeReleaseEndedSession`,与 §3.2 的 pending 同一个 RPC),
等应答(上限 1s,且不超过登录链剩余预算:截止由请求上下文派生),然后照回 18。不写任何 Redis 状态、不改应答。

X 的处置与 §3.3 完全相同 —— 本质上是**替崩溃的 gate 补发它没来得及发的那次断线通知**:

- 持有、未在交接、缓存 epoch == E → `HandleExitGameNode(kClientDisconnect)`:先存盘,落地后 A1′ 写 `E:t`;已在退出中只合并原因。
- 交接在途 / 缓存与 E 不等 → 不动(release_fence 严格相等,R4)。
- 完全不持有 → 按 owner_epoch == E 条件补写凭证。

下一次重登读到 E 代标记 → 放行并铸造 E+1(既有路径),第 7b 步给 X 的交接释放是 no-op。

取舍:

- **18 的两种「标记未就绪」都请**:`no_marker` 与 `stale_marker` 对持有者的含义相同 —— E 代还没释放;残留的旧代标记不改变谁持有 E。
  读标记失败(`ErrRedis`)不请:那是 Redis 故障,与持有者无关。
- **同步而不是后台派发**:§3.2 的 pending 用后台协程,它的调用方是会话已结束的清理通道,清理在途时首次登录被既有清理屏障挡住(§3.4)。
  这里没有这层屏障:18 回去之后客户端立刻重登,若下一次被派回 X(同物理节点不铸造,仍是 E)而释放还在路上,迟到的释放会按
  epoch 相等把**刚绑上新会话**的实体按断线退出。同步等到 X 应答 = 释放已在 X 的 loop 上执行完(§12.4 (f)),这个窗口不存在。
  代价只落在「登录撞门」这一条少见路径上:每次至多一次 RPC、上限 1s;login 的 EnterScene 预算是 5s(`login.yaml SceneManagerRpc.Timeout`)。
- **只改同 zone 的换手门(第 3 步),不改跨区重定向那道门(第 2 步)**:重连 / 顶号发 `ZoneId = 0`,跟随 location 的 zone,
  走的是只送连接、不过门的重定向;login 发出的请求要在第 2 步「离开所在 zone」只有 FirstLogin 的 RedirectOnEnter,它要求
  player_locator 里没有会话(或会话不在场景里)—— 旧会话已经结束,LeaveScene 的 pending 已经在请持有者释放。
- **开关**:与 §3.2 共用 `LeaveSceneHolderReleaseWaitSeconds`(< 0 一起关,dev 旁路下换手门本来就放行)。这是同一份释放协议,
  分两个开关只会多一种「一半开着」的组合。

### 12.3 判定表(`decideEnterHolderRelease`,按顺序)

| 顺序 | 条件 | verdict(指标) | 动作 |
|---|---|---|---|
| 0 | 开关关闭(`LeaveSceneHolderReleaseWaitSeconds < 0` / dev 旁路) | `disabled` | 不请 |
| 1 | `correlation_id != 0`(scene 进程发起) | `not_login` | 不请。18 的常规来源 |
| 2 | 落点身份不是严格更新:`decidePlacementIdentity` 不是 newer,或 location 没有代次 | `identity_not_newer` | 不请 |
| 3 | 观察到的 owner_epoch 为 0 | `legacy_epoch0` | 不请(scene 对期望 0 按 `kReleaseUnversioned` 释放、不比缓存,不能替它发) |
| 4 | location 无持有节点号,或 zone 未知 | `holder_unaddressable` | 不请(节点号只在 zone 内唯一) |
| 5 | 请求上下文已结束(login 超时 / 丢锁取消链) | `chain_gone` | 不请 |
| 6 | 其余 | `delivered` / `failed` | 同步请;`failed` = RPC 报错或超时,照回 18,下一次重登再请 |

零值输入判 `disabled`:漏填只会少请,不会误退持有者。

### 12.4 为什么不会退掉一个正在玩的合法持有者

释放只能作用于 X 上缓存 epoch 恰好为 E 的那一份实体(R4)。要证明的是:**派发的那一刻,这份实体服务的会话已经被这次登录取代,
或者就是这次登录自己的会话**(它要挪去别的节点,X 那一份本来就必须释放)。

- **(a) 只替 login 请**。scene 进程的所有 EnterScene 都经 `SendCorrelatedEnterScene` 带非 0 关联号(调用方漏填会补号并打 ERROR,
  `player_lifecycle.cpp`);login 固定填 0(`entergamelogic.go buildEnterSceneRequest`)。scene 自发请求的持有者就是发起方:换图第一跳
  是收到 18 之后才冻结(`StartTravelHandoff`),第一跳时 `release_fence` 的「交接在途」还挡不住,请了就把正在换图的玩家按断线踢下线。
  所以来源必须单独排除,不能只靠身份(旧版 scene 填代次 0 判 legacy;新版填实体已放行的代次,但不保证与 location 同会话)。
- **(b) login 发 EnterScene 时,会话权威里这次登录已经是当前会话**。`applyLoadedPlayerSession` 的顺序:持 `player_locker:{id}` →
  锁内铸 gR(单调,严格大于会话记录里的代次)→ `SetSession(sR, gR)` 提交 → 顶号时提交之后踢掉被替换的会话 → Bind gate →
  EnterScene。被取代的会话:gate 崩溃 → 连接已死;顶号 → 旧连接正被踢,gate 断线时会给 X 代发 ExitGame(本次释放与之合并)。
- **(c) 身份严格更新 = location 的会话比这次登录旧、且不是同一会话**。newer 要求请求代次更大且会话不同(同会话先判 same_session);
  另外要求 location 记着代次。同会话(LeaveGame 后同连接重进)不请 —— gate 收到 LeaveGame 时已代发 ExitGame(§3.5);
  任一侧无代次不请(与 R3 同一口径:旧版写入的 location 可能属于一次更新的登录)。
- **(d) X 上的实体不会服务比这次登录更新的会话**。实体放行过的代次只有两个来源:路由事件(scene_manager 在落点 / 同落点重连时
  先把身份写进 location,location 的代次只升不降)与 gate 盖章(同一 gate 会话按 Bind 带来的更大代次补发,去重缓存重放时就走这条,
  location 不随之更新)。两者带的都是**某条登录链铸出的代次**。登录链按玩家串行(同一把锁,核心 Redis 全服一份,
  shared-redis-split-by-purpose.md 的键闭包),铸造在锁内,所以 R 持锁期间不存在比 gR 更新的代次;实体的代次 ≤ gR,
  等于 gR 只能是 R 自己的会话。
- **(e) 「R 还持锁」由请求上下文证明**。login 心跳发现丢锁即 `chainCancel()`(`entergamelogic.go StartHeartbeat` 回调),
  链超时同理;EnterScene RPC 随之取消,服务端 `l.ctx` 结束 → `chain_gone`,不请。
- **(f) 同步应答给出先后**。X 的 gRPC 服务端在节点 EventLoop 上派发(`scene_node_service.cpp HandleReleasePlayer` 守护段),
  处理完才回包。应答回来 = 释放已在 X 上执行完;R 拿到 18 之后链才结束、才释放锁,之后任何一次登录的进场都在这之后才到 X。
  所以即使下一次登录落回 X、以同一个 E 复用实体,X 看到的也只是一个已在按断线退出的实体 —— 与「gate 断线后快速重连回同一节点」
  是同一条既有路径,不会被一条迟到的释放再退一次。
- **(g) 释放不丢数据**:退出先存盘,A1′ 在存盘落地后才写;之后跨节点放行铸造 E+1,新节点读到的是这次存盘。旧版的
  「删 location → 首次落点」回档不再可达(§3)。

### 12.5 残余

1. **超时(`failed`)之后迟到执行**:X 1s 内没应答时释放可能之后才在 X 执行。若客户端在这段时间里重登成功、落回 X 并以同一 E 复用实体,
   迟到的释放会把新会话按断线退出(先存盘,不回档;玩家再登一次)。只在 X 卡顿超过 1s 时出现,`failed` 计数可见。
   根治需要 `ReleasePlayerRequest` 带上被取代会话的身份,由 scene 按实体绑定的会话比对(proto + C++ 改动,本次不做)。
   **2026-10-18 已收口,见 §13**(请求带覆盖身份,scene 在代际放行后再比实体的登录身份;LeaveScene 的后台派发一并覆盖)。
2. **新 gate 分到与 location 相同的 session_id**(会话号撞号)→ 身份判 same_session,不请,退回修复前的行为(等旧会话清理)。
3. **X 上的实体在换图途中**(`handoffRequested`)→ X 不动它;等它自己的交接改写 location,下一次登录对新 location 再判。
4. **X 上实体的缓存 epoch 为 0**(`kReleaseCacheUnknown`)照样释放,与交接释放同一语义。
5. **gate 崩溃后的集中重登**:每个撞门的登录至多一次同步 RPC;X 的处理量与 gate 正常断线时代发 ExitGame 相当,重复请求在 X 上合并。

### 12.6 新旧并存与发布

- 依赖认 `session_ended` 的 scene(§3.3)。旧 scene 把它当交接释放(`kReleasedByTransfer`):僵尸照样退出存盘,但不写 A1′,
  登录仍回 18 直到旧会话清理 —— 与修复前相同,不更糟。发布顺序沿用 §7(scene 先于 scene_manager)。
- 不改 proto、不改 Redis 键格式;回滚:`LeaveSceneHolderReleaseWaitSeconds = -1`(与 §3.2 一起关)或回滚 scene_manager 二进制。

### 12.7 指标

`scene_manager_enter_holder_release_total{verdict}`,verdict 取 §12.3 的名表与 `delivered` / `failed`;只按 verdict 分,玩家号、
节点号、epoch 只进 `[EnterHolderRelease]` 日志。看法:`delivered` 的速率 ≈ 登录撞上未释放持有者的次数(gate 崩溃 / 顶号到别的节点时
出现);`failed` 持续非 0 = 持有节点不可达或过慢;`identity_not_newer` 正常只来自 LeaveGame 后同连接重进与混版窗口。

### 12.8 测试与验收映射(go/scene_manager `internal/logic/enter_holder_release_test.go`)

| 验收项 | 用例 |
|---|---|
| 登录撞未释放的持有者 → 回 18 之前请一次(期望 = E、发给 location 的节点、请求时 location 未改)、不走 LeaveScene 的异步钩子;持有者写标记后放行铸造,放行路径不再请 | `TestEnterScene_LoginBehindUnreleasedHolderRequestsReleaseOnce` |
| 非当前代标记同样请,期望取当前代 | `TestEnterScene_LoginBehindStaleMarkerAlsoRequestsRelease` |
| scene 自发的请求(持有者自己的第一跳;带关联号但身份更新)→ 不请 | `TestEnterScene_SceneInitiatedRejectionDoesNotRequestRelease` |
| 身份不是严格更新(同会话重进 / 请求无代次 / location 无代次)→ 不请 | `TestEnterScene_LoginWithoutStrictlyNewerIdentityDoesNotRequestRelease` |
| 登录链已结束 → 不请;开关关闭 → 不请;RPC 失败 → 照回 18、状态不变、计 failed | `TestEnterScene_LoginReleaseSkippedWhenChainContextDone`、`..._LoginReleaseDisabledBySwitch`、`..._LoginReleaseFailureStillRejectsUnchanged` |
| 真实 RPC 经共享 fake scene node:EnterScene 返回时持有节点已收到 `session_ended`、期望 = E、无接手节点 | `TestEnterScene_LoginReleaseReachesHolderAsSessionEnded` |
| 判定表、名表、身份口径 | `TestDecideEnterHolderRelease_Table`、`TestEnterHolderReleaseVerdictNamesCoverEveryValue`、`TestIdentityStrictlyNewer` |
| §8 的 LeaveGame 后快速重登走查补断言:第 2 步请一次、放行后不再请 | `TestLeaveGameQuickReloginGoesThroughHandoffGate`(`leave_release_test.go`) |

改动清单:`internal/logic/enter_holder_release.go(新)`、`enterscenelogic.go`(只改第 3 步换手门的拒绝分支)、
`internal/metrics/metrics.go`(新计数)、`internal/config/config.go` 与 `etc/scene_manager_service.yaml`(开关注释各一行);
测试 `enter_holder_release_test.go(新)`、`leave_release_test.go`(上表最后一行)。

### 12.9 Java 版(AGENTS §12)

按 Java 仓 `PARITY.md`「顶号」行(只读核对,未读代码):Java 的 login 夺权撞上仍被持有时,经 Redis pub/sub **主动请持有的 scene 让出**
(旧实例写回后释放),持有者 3s 内不让出回 2005、等 30s 归属租约过期。也就是「新登录请持有者释放」在 Java 版本来就是夺权协议的
一部分,不经换手门、不依赖旧会话清理,gate 崩溃后的重登不会卡在本节描述的形态上。结论:**已有等价行为(结构不同),Java 不需改**;
`PARITY.md` 待本仓提交后登记一行。本节不改 proto、不涉及客户端契约。

## 13. 会话结束释放带覆盖身份(§12.5 残余 1 的收口)

- 日期:2026-10-18。状态:代码与测试落在工作区;**未编译、未测试,待 Codex 验证**(命令见 `docs/PROGRESS.md` 同日「会话结束释放带覆盖身份」条目)。
- 改服务端内部 proto(`ReleasePlayerRequest` 追加两个字段)、scene_manager 两个派发点、scene 侧一道纯判定。不改 Redis 键、不改存盘。

### 13.1 问题

§3.3 的 `session_ended` 释放在持有节点上只按 owner_epoch 严格相等决定退不退(R4)。同物理节点重登不铸造 owner_epoch
(`guard.mint = !samePhysicalNode`),同一个 E 下实体可以先服务旧会话、再被新登录复用,owner_epoch 区分不了这两段。
两处派发都可能在新登录复用实体之后才在持有节点 X 上执行:

| 派发点 | 迟到窗口 |
|---|---|
| 登录撞门的同步请求(§12,`enter_holder_release.go`) | RPC 超时(`failed`)后释放仍在路上;客户端已重登、落回 X、以同一 E 复用实体 |
| LeaveScene pending 的后台派发(§3.2,`dispatchSessionEndedRelease`) | 后台协程单发 1s 截止;LeaveGame 后同一连接再登录(会话号不变、代次变大)落回 X,或别的登录在清理屏障之外落回 X |

结局:在线的新会话被按断线退出一次(先存盘、不回档,但玩家被踢)。§12.4 (f) 只证明了「送达并应答了的释放」先于之后的进场,
没覆盖「超时、迟到了」的那一半;后台派发连这层先后都没有。

### 13.2 做法

`ReleasePlayerRequest` 追加 `covered_session_id = 6`(uint32)与 `covered_login_generation = 7`(uint64):这次释放**覆盖到的最新登录身份**。
持有节点在 `release_fence::Decide` 判释放之后,再用纯判定 `release_fence::DecideSessionEndedBinding` 比实体的 `PlayerLoginIdentityComp`:
实体比覆盖身份新 → 不退出。

填法:

| 派发点 | covered | 理由 |
|---|---|---|
| LeaveScene pending | 离场请求的 (session_id, login_generation),即被清理的那条会话 | pending 只在离场身份判 `deleted`(location 的会话不比它新)时出现;它覆盖 location 上的会话与它自己 |
| 登录撞门(§12) | **这次登录自己的** (session_id, login_generation),不是 location 记的旧身份 | 见下 |

登录撞门为什么填请求自己的身份:§12.4 要证明的是「X 上这份实体服务的会话已被这次登录取代,**或者就是这次登录自己的会话**」。
按 §12.4 (d),R 持锁期间不存在比 gR 更新的代次,X 上不比 gR 新的每一份都该退出:

- location 上被取代的旧会话 L(主时序);
- L 与 R 之间的中间会话 M:M 也撞过门、location 没被它改写,但它的旧路由经 gate Bind 补发绑上了 X 的实体;
- 经 gate Bind 补发已绑上 R 自己的那一份(R 要落到别的节点,X 那一份本来就必须释放)。

只填 L 的话,后两种被判 `skip_newer_login`:X 永远不释放,R 每次重登都回 18,旧会话清理发来的释放(覆盖 L)同样被挡,一路等到
§3.6 的 600s 上限 —— 比不带身份的现行为还差。填 R 时它们都被覆盖,而 R 之后的登录(gC > gR)仍受保护。主时序下两种填法结论相同。

scene 侧(`PlayerLifecycleSystem::HandleSessionEndedRelease`;`scene_node_service.cpp` 守护段只多传两个字段):

1. 交接在途 / 代际不等 → 照旧不动(`Decide` 先于一切,R4 不变)。
2. `Decide` 判释放 → try_get `PlayerLoginIdentityComp`(缺失按 `kInvalidSessionId` / 0)→ `DecideSessionEndedBinding` → 计 `binding_*`。
3. `skip_newer_login` / `skip_conflict` → LOG_WARN 后返回,零副作用(不碰会话、退出意图、标记)。其余 → `HandleExitGameNode(kClientDisconnect)`(不变)。
4. 本节点完全不持有时的凭证路径不看覆盖身份(没有实体可比;在途载入 / 改派待确认仍按 §3.3 不写)。

交接释放(`session_ended = false`)不看 `covered_*`;`release_fence::Decide`「只比 owner_epoch」的不变量不变(scene-enter-chain-residuals.md §1.3)。

### 13.3 判定表(`DecideSessionEndedBinding`,按顺序)

| 顺序 | 条件 | 结论(汇总行 `binding_` 后缀) | 动作 |
|---|---|---|---|
| 1 | 覆盖代次 0 | `unversioned_request` | 退出(旧版 scene_manager,现行为) |
| 2 | 实体代次 0(没有身份组件 / 旧版 login 放行的) | `unversioned_entity` | 退出(证明不了它更新,现行为) |
| 3 | 实体代次 > 覆盖代次 | `skip_newer_login` | 不退出,WARN |
| 4 | 代次相等、两边会话都有效(`IsBoundSession`)且不同 | `skip_conflict` | 不退出,WARN(违反 G2) |
| 5 | 其余:实体代次更小;同代同会话;同代而任一侧没有有效会话 | `covered` | 退出 |

**先比代次、后比会话号**,与入场栅栏「同一会话一律放行」的口径有意不同:入场请求与 ExitGame 走同一条 gate→scene 连接,
FIFO 让同会话号的到达序就是 gate 的决策序;释放走 scene_manager 的 gRPC,没有这层顺序。LeaveGame 后同一连接再登录会话号不变、
代次变大(scene-enter-chain-residuals.md §3.2),只比会话号会把这次新登录当成离场会话退掉。口径与 scene_manager 的
`decideLeaveIdentity` 一致(代次大者新,同代异会话算冲突)。会话号无效只是「没得比」,不当作不同。

### 13.4 为什么既不误退新会话,也不留下不释放的持有者

- **(a) 实体身份同步更新。** 复用实体的进场(`HandleEnterGameNode` 第 4 步 → `EnterScene` 第 1 步 `RecordLoginIdentity`)在同一次回调里写
  `PlayerLoginIdentityComp`,代次只升不降。新登录一到 X,之后到达的释放就判 `skip_newer_login`。
- **(b) 释放先于新登录的进场到达**(实体身份还是旧的)→ 照常退出;随后的进场走「断线后快速重连回同一节点」的既有路径
  (EnterScene 第 0 步取消退出;退出已收尾则从刚落地的存盘载入)。不回档,与修复前同。
- **(c) 只挡更新的登录。** 不退出只发生在实体代次严格大于覆盖代次(或同代冲突)时;代次按玩家单调、在登录锁内铸造,所以被挡下的实体
  一定绑着一次晚于这次释放所依据的登录 —— 它不是这次释放要清走的那一份。
- **(d) 不会因此永久卡住。** 被挡下的实体服务的是一次更新的登录 L′。L′ 结束时自己的清理(覆盖 L′)或下一次撞门(覆盖更新的 R′)会再发
  释放并覆盖它。同代冲突只能等 §3.6 的上限兜底,应恒 0。
- **(e) 不碰 R1 / R2。** scene_manager 删 location 的证据与 Lua 不变;这里只决定 X 退不退,不发任何标记。

### 13.5 残余

1. **释放先于新登录的进场到达 X**(13.4 (b)):照常按断线退出,新进场取消退出或重载。玩家多一次载入或无感知,不回档;此刻实体还不知道
   新登录,身份堵不了这条窗口。
2. **混版窗口**:旧 scene_manager 不填(`binding_unversioned_request`)、旧版 login 放行的实体没有代次(`binding_unversioned_entity`)时退回现行为。
   全量升级后两项应恒 0。
3. **`verdicts[]` 的 release 系列不再等于退出次数**:会话结束释放代际放行、随后被 `binding_skip_*` 挡下的那几次同样记在 verdicts 里,读汇总行时相减。

### 13.6 新旧并存与发布

- proto 只追加字段 6、7,不复用。旧版 scene 忽略它们,按 §3.3 只比 owner_epoch;旧版 scene_manager 不填,新 scene 判 `unversioned_request`、按现行为。
- 发布顺序沿用 §7:proto 生成 → scene → scene_manager。scene_manager 滚完才生效。
- 回滚:任一侧单独回滚都安全(scene_manager 回滚即不再填;scene 回滚即忽略字段),都退回现行为。

### 13.7 指标

scene `[ReleaseFence]` 行末追加 `binding_covered / binding_unversioned_request / binding_unversioned_entity / binding_skip_newer_login /
binding_skip_conflict`;后两项有增长时整行升 WARN(`release_fence::BindingAlarming`)。`binding_skip_newer_login` 非 0 = 确实挡下过一条会退掉
新会话的迟到释放(可与 scene_manager 的 `scene_manager_enter_holder_release_total{verdict="failed"}` 对照);`binding_skip_conflict` 应恒 0。
不以 player_id 作 label;玩家号、实体身份与覆盖身份只进 WARN 日志。

### 13.8 测试与验收映射

| 验收项 | 用例(工程) |
|---|---|
| 判定表每一格(含同会话号更大代次、任一侧无会话、编译期求值) | `ReleaseFence.SessionEndedBindingDecideTable`(cross_zone_test) |
| 名表 / 退出集合 / 告警集合 / 越界 fail-closed | `ReleaseFence.SessionEndedBindingNamesCoverEveryValue` |
| 汇总行只在两项 skip 增长时升 WARN | `ReleaseFence.SummaryEscalatesOnBindingSkip` |
| **残余 1 主时序**:同节点重登复用实体后迟到的释放 → 不退出、代际照旧判 release、计 skip_newer_login、升 WARN、不写凭证 | `ReleaseFenceEcs.SessionEndedReleaseAfterSameNodeReloginKeepsNewerLogin` |
| LeaveGame 后同连接再登录(同会话号、更大代次)→ 不退出 | `ReleaseFenceEcs.SessionEndedReleaseKeepsSameConnectionRelogin` |
| 实体绑着被覆盖的登录 / 更早的登录(登录撞门)→ 按断线退出 | `ReleaseFenceEcs.SessionEndedReleaseExitsCoveredLogin`、`ReleaseFenceEcs.SessionEndedReleaseExitsOlderLoginOnLoginCollision` |
| 同代异会话 → 不退出 | `ReleaseFenceEcs.SessionEndedReleaseSameGenerationOtherSessionIgnored` |
| 旧版 scene_manager(0 / 0)→ 现行为退出 | `ReleaseFenceEcs.SessionEndedReleaseWithoutCoveredIdentityKeepsLegacyBehavior` |
| 代际先挡下时不做身份判定 | `ReleaseFenceEcs.SessionEndedBindingJudgedOnlyAfterEpochReleases` |
| 实体没有身份组件 → `unversioned_entity`、照常退出 | `ReleaseFenceEcs.SessionEndedReleaseExitsHolderAsClientDisconnect`(补一条断言) |
| LeaveScene pending 填离场请求的身份(location 比离场会话旧时也取离场身份,不退回 location 的) | `TestLeaveScene_PendingCoversLeaveRequestIdentityNotLocation`、`TestLeaveScene_HolderNotReleasedKeepsLocationAndRequestsRelease`(go/scene_manager) |
| 登录撞门填这次登录的身份、线上带 `covered_*` | `TestEnterScene_LoginBehindUnreleasedHolderRequestsReleaseOnce`、`TestEnterScene_LoginReleaseReachesHolderAsSessionEnded`、`TestLeaveGameQuickReloginGoesThroughHandoffGate` |

### 13.9 改动清单

- proto:`proto/scene_manager/scene_node_service.proto`(`ReleasePlayerRequest.covered_session_id = 6`、`covered_login_generation = 7`)。生成产物由 Codex 重生,不手改。
- scene_manager:`internal/logic/scene_node_client.go`(`RequestNodeReleaseEndedSession` / `dispatchSessionEndedRelease` 多一个 covered 参数)、
  `leavescenelogic.go`(pending 派发填离场身份)、`enter_holder_release.go`(同步请求填这次登录的身份);测试 `leave_release_test.go`、
  `enter_holder_release_test.go`(记录器带 covered、补断言)。
- scene:`player/system/release_fence.h`(`SessionEndedBinding` 与判定、`binding_*` 计数)、`player/system/player_lifecycle.{h,cpp}`
  (`HandleSessionEndedRelease` 多两个参数与第二道判定)、`core/system/redis.cpp`(汇总行)、`nodes/scene/handler/grpc/scene_node_service.cpp`
  (守护段内传字段);测试 `cpp/tests/cross_zone_test/cross_zone_test.cpp`(上表)。
- 文档:本节,§3.3 与 §12.5 各加一行指向,`docs/PROGRESS.md`。

### 13.10 Java 版(AGENTS §12)

按 Java 仓 `PARITY.md`「顶号」「短线重连与落点」行:Java 每次登录都经库行 `owner_epoch` 夺权,请持有者让出是夺权协议的一部分;
位置记录由持有归属的 scene 唯一写入,没有 scene_manager 代发「会话结束释放」这条路径。

2026-10-19 只读核对了 Java 代码(未改、未构建),确认迟到的让出请求退不掉新会话:

- 每次夺权都自增 epoch,与落到哪个节点无关:`game-player-store/.../PlayerMapper.java` `claimOwnerEpoch`
  (`SET owner_epoch = owner_epoch + 1 ... WHERE owner_released = 1 OR owner_lease_until < now`);login 只接受本次新夺到的 epoch。
- 让出请求 `game-api/src/main/proto/game/api/ownership.proto` `OwnerTakeover{player_id, owner_epoch}` 只带 epoch;scene 侧
  `game-scene/.../SceneWorld.java` `onTakeoverRequested` 只对 `ownerEpoch` 恰好相等的实例(或同 epoch 的加载中进场)动手,
  「更旧或更新一律不动」;gate 对 epoch 不符的 `PlayerKicked` 也忽略。
- `ScenePlayer` 的 `ownerEpoch` / `session` 是 `final`,实例不会被另一次登录复用。

也就是说,本仓靠覆盖身份补上的那一层(同一代下区分两次登录),在 Java 里由「每次登录都换代」直接给出。结论:**已有等价行为(结构不同)**,
Java 不改。本次 proto 改动只在服务端内部,不涉及客户端契约;Java 仓在本仓提交后跑 `ContractSync` 同步 `proto/` 副本,
`PARITY.md` 待本仓提交后登记一行。

## 14. 第二轮审计修复(2026-10-19)

- 来源:[data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.1 / §3.2 对本文修复的复核,
  四条:r2-gate#1、r2-sm#3、r2-sm#1、r2-sm#2。
- 状态:四条都对照当前代码复核成立,已修;代码与回归用例落在工作区,**未编译、未测试,待 Codex 验证**(命令见
  `docs/PROGRESS.md` 2026-10-19「location 释放绑定第二轮审计修复」条目)。不改 proto、不改 Redis 键格式(计时键的值格式不变)。

### 14.1 复核与修法

| 编号 | 复核 | 修法 | 代码 |
|---|---|---|---|
| r2-gate#1 | 成立。LeaveGame 先于本次登录的 Bind 到达时,重置记下的是旧代次,旧链的 Bind / 路由代次更大、照常收下 | 客户端再次 EnterGame 之前到达的 Bind / 路由一律丢;之后按被离开的那条链(角色 + 随丢弃抬高的代次)与会话已见过的更新登录认旧链(§3.5) | `session_info_comp.h`(`enterGameSentAfterLeave` / `playerIdAtReset`)、`scene_route_helper.h`(`ResetSceneRoute`、`NoteEnterGameRequested`、`IsLoginGenerationSupersededSinceLeave`、`IsRouteFromBeforeLeaveGame`、`IsBindFromBeforeLeaveGame`、`NoteDroppedFromBeforeLeaveGame`)、`gate_event_handler.cpp`(RoutePlayer / BindSession 守护段)、`scene_entry_dispatch.{h,cpp}`(`OnClientEnterGame`)、`client_session_handler.cpp`(EnterGame 收包钩子) |
| r2-sm#3 | 成立。负载键缺失时 `playerLocationOwnerGone` 对活节点返回 true,LeaveScene 按 `owner_dead` 立刻删 | 释放证据 2 只认 `playerLocationOwnerDead`(etcd 注册表正面消失 + 屏障);EnterScene 的陈旧位置判定本批不动(同日另批修,§14.5) | `leavescenelogic.go` `judgeHolderRelease` |
| r2-sm#1 | 成立。计数与 forced ERROR 在删除 Lua 结果出来之前就记 | 移到 `outcome == deleted` 之后;没删成只打 INFO | `leavescenelogic.go` `LeaveScene` 尾部 |
| r2-sm#2 | 成立。`first_ms` 与求差用各副本墙钟 | 读、比 scope、登记、求差合进一段 Lua,时钟取 Redis `TIME`(§3.6 时钟语义) | `leave_release.go` `luaNoteLeaveReleaseWait` / `noteLeaveReleaseWait`(删了 `parseLeaveReleaseWait`) |

为什么 r2-sm#3 不会把"location 永久残留"带回来(§5):真正下线的 zone(合服后源区)在 etcd 里同样已注销,走 `playerLocationOwnerDead`
照样立刻删;注册表未同步、或节点还在注册表里而负载键丢了这类拿不准的情形,落到 §3.6 的等待上限(只伤活性,不回档)。

### 14.2 回归用例(先写用例、再修)

| 缺陷 | 用例 | 旧实现下的表现 |
|---|---|---|
| r2-sm#3 | `TestLeaveScene_MissingLoadSetIsNotOwnerDead`(注册表里有节点 10、负载键整键丢失 → pending、location 保留);另一面 `TestLeaveScene_OwnerGoneFromRegistryDeletes`(注册表已同步且没有节点 10 → owner_dead 立刻删) | 前者判 owner_dead、location 被删 |
| r2-sm#1 | `TestLeaveScene_ReleaseVerdictCountedOnlyWhenDeleted`(forced 判定后 location 被并发改写 → superseded;released 判定后已被删掉 → already_gone;两者计数都不动、人数不扣) | forced / released 各多计 1 |
| r2-sm#2 | `TestLeaveScene_WaitTimerUsesRedisClockNotReplicaClock`(Redis 时钟比本进程慢 1 小时、按 Redis 只等了 30s → pending;Redis 走过上限后才 forced);`TestNoteLeaveReleaseWait_UsesRedisClock`(登记与求差都取 Redis 时钟、不重写起点、回拨钳 0、scope 变或值坏重新登记) | 前者按本进程墙钟算出 1h30s、立刻 forced |
| r2-gate#1 | `SceneRouteEntry.OldChainBindArrivingAfterLeaveGameIsDroppedAndNeverReplayed`(审计原文步骤);`..._OldChainRouteArrivingAfterNextEnterGameIsStillDropped`(两种到达顺序);`..._RouteOlderThanNewLoginBindIsDroppedWhileResetPending`;`..._LeftChainGenerationDoesNotBlockAnotherCharacter`;`..._RepeatedLeaveGameKeepsRaisedLeftChainGeneration` | 第一条:旧路由连同登录类型转发到 X,重登时再按它带旧 epoch 补发 |

`leave_release_test.go` 里原有的计时用例(`..._WaitBudgetExceededForcesDelete`、`..._WaitTimerDoesNotCarryAcrossLocations`)改为定死
miniredis 的服务端时钟(`mr.SetTime`),不再依赖真实墙钟;`routing_identity_test.cpp` 里原有的 LeaveGame 用例按生产调用序补上
"客户端再次 EnterGame"(`NoteEnterGameRequested`),断言不变。

### 14.3 残余

- r2-gate#1:旧链的 Bind 与路由都晚于客户端下一次 EnterGame 收包、又早于新登录的任何事件到达时认不出(§3.5 末段)。
- r2-sm#2:Redis 服务端时钟往前跳变仍会提前 forced(单一时钟,§3.6)。
- r2-sm#3:EnterScene 的陈旧位置判定(`playerLocationOwnerGone`,换手门放行)当时仍认"zone 负载集为空",负载键丢失的那一拍里同样会
  误判。它不在本批四条之内,也不是本文修复引入的(第二轮审计把它记为"本来就用同一判据")。**同日另批复核成立并已修**(未编译、未测试),见 §14.5。

### 14.4 Java 版(AGENTS §12)

- r2-sm#3 / r2-sm#1 / r2-sm#2:**不适用**。Java 版没有 scene_manager 的 LeaveScene 释放判定、负载集判死与等待计时(§11:位置记录由持有
  归属的 scene 唯一写入,归属靠库行 `owner_epoch` 夺权)。
- r2-gate#1:**不适用,结构上不可达**(2026-10-19 只读核对 Java HEAD `0e4a613`,未改代码、未构建;Java 没有待做项,
  `PARITY.md` 不登记)。本仓的缺陷前提是绑定与登录应答分走两条路:login 先回 EnterGame,再经 Kafka 发
  BindSession,路由由 scene_manager 另发。Java 的会话绑定只有一个来源,就是同一会话那次 EnterGame 的 login 同步应答里的
  `EnterScene` 指令,没有异步 Bind,也没有 scene_manager 到 gate 的路由事件。
  - gate 对每个会话的 login 调用严格串行:`game-gate/.../session/ClientDispatcher.java` `onRequest` 把 LeaveGame 排进 `s.pending`,
    `drain` 只在 `!s.inFlight` 时取下一条,`onLoginCompleted` 先回包、再 `applyDirectives`。所以 LeaveGame 到达 login 时,EnterGame
    的绑定已经挂上会话;login 的 `LeaveGameHandler` 按 `SessionContext.player_id` 回 `UnbindPlayer`,gate `unbindPlayer` 发
    `PlayerLeave{voluntary}` 并清空 (节点, 链路代次, 玩家, epoch)。
  - EnterGame 调用在 gate 超时(Dubbo 5 s)时,迟到应答由 Dubbo 消费端丢弃,到不了 gate(`EnterGameHandler` 类注释「时间预算」),
    不存在迟到的旧绑定。
  - LeaveGame 之后源 scene 的迟到帧都要与当前绑定的 (节点, 链路代次, 玩家, epoch) 比对:`PlayerTransfer` 失配时只带围栏放弃
    to_epoch、不改绑,`PlayerEnterResult` / `PlayerKicked` 直接丢弃;同一连接再进游戏时 login 夺到新 epoch,旧帧仍按 epoch 失配丢弃。
  - scene 侧:加载中收到 PlayerLeave,`SceneWorld.onPlayerLeave` 取消进场、释放归属;加载返回时 `finishPlayerLoaded` 发现 pending
    已被换掉就丢弃,旧实例不会按旧 epoch 重建。
  - 结论:不会把已回选角的客户端拉回旧场景,也不会让旧实例按旧 epoch 重建,Java 不改。Java 已知的残余不属于本条:EnterGame 在
    gate 超时后,login 已夺到的归属没有持有者,要等 `OWNER_LEASE` 到期才能再进,只影响活性。
- 本批不涉及客户端契约(没有 proto / tip / 配置表改动)。

### 14.5 EnterScene 陈旧位置判定补注册表正面证据(r2-sm#3 同源,2026-10-19 另批)

- 来源:§14.3 登记的 r2-sm#3 同源残余。状态:复核成立,已修;代码与回归用例落在工作区,**未编译、未测试,待 Codex 验证**
  (命令见 `docs/PROGRESS.md` 2026-10-19「EnterScene 陈旧位置判定补注册表正面证据」条目)。不改 proto、Redis 键与指标。
- 调用点:`playerLocationOwnerGone` 全仓只有一处调用(`enterscenelogic.go` 读完 location、落点身份裁决之后,`playerLocationOwnerDead`
  之前)。返回 true 时清空 currentLoc / currentLocRaw / currentZoneID,之后各分支都按"没有位置记录"走。逐个入口看「负载键整键丢失
  (ZCARD=0、ZSCORE 为空、无 death_at),持有节点却还活着、还在注册表里」的那一拍:

| 入口形态 | 旧实现 | 结论 |
|---|---|---|
| 从别区 gate 登录、不指定去向(ZoneId=0、SceneId=0;访客断线后经本区 gate 重登、gate 崩溃后换 gate) | 位置被过滤,不再按 GO-5 跟随位置的 zone,去向回落 gate zone;gate zone 有活节点就**直接落点、铸造 E+1**、发路由,不给持有节点发 ReleasePlayer | **误放行,回档**:持有节点之后带 E 的周期 / 退出存盘全被 owner CAS 拒、实体自毁,未落盘的进度丢失 |
| 已连到别区 gate、显式落到该区(ZoneId = 目标、带 SceneId / 地图) | 跳过跨节点换手门,同样铸造 E+1 | 同上,误放行 |
| scene 发起的跨 zone 传送第一条腿(gate zone = 持有 zone) | 位置被过滤,跨区重定向按"只送连接"放行(不写等待落点、不铸造)。源 scene 发请求前已存盘并写出交接标记(StartTravelHandoff),收到 Redirect 后销毁实体 | 不丢数据(源端已落盘);但没写等待落点、去向不随票据,语义偏离 |
| 同 zone(持有 zone 就是 gate / 目标 zone) | 位置被过滤,但同一 zone 负载集为空,`resolveScene` 与频道预占都按"节点不在负载集"拒(ErrNoAvailableNode) | 不铸造,只伤活性 |
| 等待落点(node_id 为空)所在 zone 负载键丢失 | 位置被过滤,未映射归属的玩家改按 gate zone 回落(3b 旁路) | 没有持有者,不回档 |

- 修法(`enterscenelogic.go` `playerLocationOwnerGone`、`load_reporter.go` 新增 `isZoneGoneFromRegistry`):先要 etcd 注册表的正面证据,
  即本副本已完成首次全量同步、且注册表镜像里该 zone 一个场景节点都没有;再加本副本观察屏障(`localGoneBarrierBlocks`,与 OwnerDead
  同口径);原来三条(负载集为空、`IsNodeAlive` 为假、death_at 屏障)保留。负载键丢了而节点还在注册表时返回 false,位置照常有效:登录按
  GO-5 跟随它的 zone 只送连接;同 zone / 显式落点走换手门(18 → §12 登录撞持有者的释放请求 → 标记 → 放行)。
- 为什么不直接改成只认 `playerLocationOwnerDead`:有持有节点的位置,新判据不弱于 OwnerDead 的全部条件(zone 里一个注册都没有,
  就推出该节点不在注册表、也不存在身份歧义),放行面是 OwnerDead 的子集,安全性与 LeaveScene 一侧相同。保留单独函数是为了口径:
  整 zone 下线不计 `dead_owner_takeover`、不还旧场景人数,屏障计数记在 `stale_location`;等待落点(node_id 为空,OwnerDead 恒为假)
  在下线 zone 的旧行为也不变。删掉它会把合服残留计进单节点接管指标,还会改变未映射玩家的 3b 旁路,超出本条范围。
- 合服后源区永久下线仍放行:源区节点都已从 etcd 注销,`isZoneGoneFromRegistry` 为真,其余条件与修复前相同。副本刚启动、注册表还没同步完
  的几秒里拿不到证据,按换手门 18 暂拒,上游重试即可。
- 回归用例(`go/scene_manager/internal/logic/enter_stale_location_test.go`):
  - `TestEnterScene_MissingLoadSetWhileZoneRegisteredKeepsHandoffGate`:node 10 在注册表、zone 1 负载键整键不存在,显式落到 zone 2 → 18,
    location 原文、owner_epoch 1、人数都不变,不发路由。旧实现:放行并铸造 2。
  - `TestEnterScene_LoginOverMissingLoadSetDecidedByRegistry`:不指定去向,从 zone 2 gate 登录。「源区节点仍在注册表」→ 只送连接回 zone 1,
    不改 location、不铸造(旧实现:在 zone 2 落点、铸造 2);「源区已从注册表注销」→ 在 gate zone 落点并铸造 2(合服放行)。
  - `TestPlayerLocationOwnerGone_RequiresZoneGoneFromRegistry`:注册表未同步、zone 里还有别的注册节点、本地观察在屏障内、负载集还有成员、
    death_at 刚写入,各自不放行;条件齐备时放行,且同一时刻 OwnerDead 也放行。
  - 既有用例按新前提补注册表状态,断言不变:`seedStaleLocationFromZone1` 改为摆出"zone 1 已从注册表注销"(覆盖 `TestEnterScene_StaleLocation*`
    三条);`TestEnterScene_ExistingLocationCrossZoneRejectedWhileOldNodeAliveThenRedirectsOnceZoneDead` 开头注册 node 10,整 zone 下线时注销与
    ZREM 一起做。
- 残余:
  - 判据依赖 `knownNodes` 是全服注册表镜像(scene_manager 现在 watch 所有 zone 的前缀)。将来 watch 收窄到本 zone(容量模型 A1)时,别的
    zone 会恒判"没有节点",跨 zone 的陈旧位置又只剩负载集一条证据;收窄前必须先改成按 zone 查注册表(`isZoneGoneFromRegistry` 注释已写明)。
    `playerLocationOwnerDead` 用的 `isNodeGoneFromRegistry` 有同样的依赖。
  - 注册内容解析失败的节点不进 `knownNodes`:一个 zone 的注册全部解析失败、负载集又为空时仍判下线(与 `isNodeGoneFromRegistry` 同一盲区;
    这类节点也不会被派场景)。
- Java 版(AGENTS §12):**不适用(结构不同)**。Java 的位置记录(`game-discovery/.../location/PlayerLocationDirectory.java`)只由持有归属的
  scene 写、带 TTL,被接管或停服时不动记录,由新持有者覆盖或按 TTL 消失;没有 scene_manager 侧的首次落点铸造,也没有按 zone 负载集判下线、
  过滤陈旧位置的路径(本次只读 grep 了 Java 主代码,未构建)。Java 没有待做项,`PARITY.md` 不登记。

## 15. 2026-10-21 修订：等待超时不授予释放权限

本节覆盖前文§3.6、§10回滚开关与相关`forced`旧语义。当前目标不允许没有持有者释放证明的回档风险：LeaveScene等待超过600秒或任意诊断预算，仍保持原location和人数，返回`holder_release_pending`继续按原会话催促。Redis时间跳变、负数旧配置、开发换手开关、epoch缺失均不能把UNKNOWN变成已释放。配置键仍兼容解析，非正值沿默认诊断预算；`forced`等旧指标名仅保留兼容，不再由生产删除判定产生。

删除仍只走正证路径：当前epoch匹配的原handoff释放标记（删除Lua再次原子复核）、正规节点死亡证明，或location本身明确没有持有者。没有真实持久化/释放证明，不因TTL、无应答或长时间等待删除。标记撤回、旧E、原location被新登录CAS替换时，原请求不能删除新location或扣人数。原生资产与SQL角色准入仍由既有DS/RoleAdmission负责，本修订不创建第二份资产权威。

本次新增/修订确定性回归覆盖预算到点仍UNKNOWN、旧负数/开发配置不能关闭证明、epoch0保持未知、真实标记放行及删除前撤回/新落点竞态。仅源码修订，尚未执行编译或测试。关于原owner_revision投影绑定与当前目录核验的后续接缝，另以本批最终源码和验证清单为准。

## 16. 2026-10-21 原归属修订绑定的调度投影

SceneManager不是角色授权者。EnterScene在读取成功缓存、预占人数和写位置之前，用既有DS LookupAccountRole强读当前原修订及冻结状态，要求请求原owner_revision逐项相等；不得把查询出的新修订补给旧请求。RPC只使用独立scene_manager caller及MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER，DS仅对Lookup放行，目录写、交易和原生准入仍拒绝此caller。DS全球启动要求四用途密钥互异，防止同值密钥使只读caller伪装写caller。本机启动临时摘掉SM不需要的login/trade/scene密钥，启动后恢复父环境；K8s只引用独立Secret键。复用既有DataServiceRpc连接，不新增SQL/Redis连接池，连接总账数量不变。

原PlayerLocation PB不改。location_revision侧车仅保存20位无符号十进制原R与原location字节SHA256；location_revision_floor保存已观察SQL修订下界，不设TTL、不随位置删除。Lua只按等长字符串比较R，不经double。生产落点、同落点身份刷新、回滚和Leave都在原raw CAS之外核tag/floor，不能让旧R延迟覆盖/回滚/删除新R。已知旧R投影可按原raw+tag精确移除，归属epoch及修订floor保留；无tag的未知原件不从当前目录补R，同落点身份刷新仍保持unknown并沿原holder释放协议。成功缓存使用done:v2:原R:完整请求指纹:PB应答，旧格式不能直接重放。

Lookup是缓存修复的观察点，不与Redis合并成角色授权事务。Lookup之后发生过户时，在途旧投影可能短暂出现；下一次合法入口强读当前R并修复，迟到Kafka携原R/E仍由Scene的继承epoch核验、DS原RoleAdmission同P核验拒绝，不能激活旧资产。Redis丢失/回档后floor也可能丢失，因此每次入口仍从SQL观察，绝不只信floor或缓存。原生SQL准入和存盘仍是唯一授权/持久化证明。

补正前次只读推导：不能仅凭“旧R曾写到无实体节点”断言永久HandoffPending；现有enter_holder_release路径会按原E请求持有节点，节点无实体时也可按原epoch条件补写真实释放标记。此接缝保留，不为软调度创建第二份永久SQL head。*（2026-10-23 批注：r2-lc#2 之后这条补写改为带围栏的凭证 —— 读本进程实例在原 E 下登记的 `handoff_fence` 原件、重核本节点仍完全不持有后以原件 CAS，见 §3.3 第 2 项的更正；本节点从未为原 E 登记成功时拿不出资格，见那里的残余 1。）*新增回归源码覆盖旧done拒绝、目录未知/冻结、2^53相邻及uint64最大R、raw+tag竞态、未知原件不补权、R2前缀后的迟到回滚、同R bump/keep及回滚重放不复活handoff。尚未运行编译/测试。


## 17. 2026-10-24 无资格持有者的收口：权威方带围栏撤销（设计，未落码）

负责人 2026-10-24 要求给 r2-lc 修复后的 `no_qualification` 残余加收口机制，按最标准的方式设计并落码。本节是设计；落码因周额度到 90%（会话组约定 92% 停线）推迟，接手说明见 §17.8。

### 17.1 问题

r2-lc#2 之后，会话结束凭证只能由本节点以"本进程实例在期望 E 下登记的资格原件"做 CAS 写出（§3.3 第 2 项、`release_fence::DecideCredentialQualification`）。location 记的是 (X, E)，X 却从未在 E 下登记成功时，X 拿不出资格，判 `no_qualification` 只打 WARN。典型情形：

- 入场登记在写 fence 之前被拒（`kRegisterFenceLua` 拒绝、`CancelPendingFence` 15s 截止、Seal），实体从未建出；
- 交接回滚把 location 推到 E+2，源实体已在销毁侧收尾，回滚转写的 "E+2:t" 标记又已过 TTL。

这时 LeaveScene 拿不到释放证据，EnterScene 的持有者释放门一直回 18；§15 已取消"等满上限就删"的兜底。玩家只能等下次登录恰好落回 X 重新登记，或者 X 死亡（证据 2）。X 被同号新进程顶替也不行：GO-3（`owner_replaced.go`）只认资格原件 E 等于 location E，这里两者不等。X 活着、又不再被派到这名玩家时，location 永久卡住，玩家永久进不了场。

### 17.2 为什么由权威方收口

标准做法是由发放租约的权威方回收租约：先推进围栏令牌，再把租约交给新人；旧持有者之后的任何写都因令牌过期被存储侧拒绝（fencing token）。本仓里令牌就是 `owner_epoch`，权威方是 scene_manager（铸造者）。收口不需要 X 配合，也不依赖 X 的应答、连接顺序或墙钟。

不选的两个方案：

- **X 自己"现造"资格再写凭证**：正是 r2-lc#2 要防的迟到写复活形状，否决。
- **等待超时直接删 location**：§15 明确否决，持有者活着且有未落盘进度时会回档。

### 17.3 正证：资格原件表明没有任何进程在 E 下登记过

不变量 I-1：玩家实体只会在 E 下的入场登记成功（`kRegisterFenceLua` 写出 E 下的 H）或交接采纳（`kResumeFenceLua` 把 R 恢复为 E 或 E+2 下的 H）之后才建出或继续持有。没有登记、没有采纳，就没有实体；没有实体，就没有 E 下未落盘的进度。

推论：`handoff_fence` 原件缺失，或其 E 不等于 location 的 E，且这一状态在一次原子检查里与 location、owner_epoch 同时成立，就是"此刻没有任何进程以 E 持有该玩家"的正证。

**接手时第一件事是核对 I-1**（§17.8 第 1 步）：A2′ 闸门、RoleAdmission、`InitPlayerFromAllData`、交接采纳、同节点复用实体、退出保留期重登这几条路径，是否都在登记或采纳成功之后才建出或复用实体。只要有一条例外，就要把例外写进 §17.4 的条件，不能靠宽限期掩盖。

### 17.4 触发条件（全部满足才撤销）

1. location 存在，指向 (X, E)，E ≠ 0。
2. 当前没有 E 下的交接释放标记（有的话走现有释放证据路径，不撤销）。
3. 资格原件缺失，或原件 E ≠ E。原件 E == E 时：实例与 X 的在册实例一致就是正常持有，不撤销；不一致归 GO-3 判定，不撤销。
4. 本副本已经按现有路径请过 X 释放（LeaveScene 的 `ReleasePlayer.session_ended`，或 EnterScene 持有者释放门的同步请求），并且从首次请求起、按 Redis `TIME` 计（复用 `luaNoteLeaveReleaseWait` 的计时键与口径）已过宽限 `UnqualifiedHolderRevokeGraceSeconds`。缺省 120s，须大于入场登记截止 15s 与交接冻结上限 70s 之和，留出 RPC 与重试余量。宽限只为不打断在途的入场与"采纳 E+2"窗口（§17.6 第 3 条），正确性不依赖它。

### 17.5 原子动作（一段 Lua，scene_manager 侧）

KEYS：location、`owner_epoch`、`save_barrier`（读 `handoff_fence`）、handoff 标记键，以及 §16 的 `location_revision` 侧车与 floor 键。键集与现有 LeaveScene 删除 Lua、`kRegisterFenceLua` 相同，都在共享核心 Redis 的同一实例里；上 Cluster 前的 hash tag 改形是既有残余（shared-redis §20.4 / §20.6）。ARGV：读到的 location 原字节、期望 E、读到的 fence 原字节（缺失为空串）、本次请求的身份（供日志与回执）。

在同一段 Lua 里：

1. location 字节等于 ARGV；`owner_epoch` 等于 E；`save_barrier.handoff_fence` 字节等于 ARGV（缺失对缺失），解析后 E ≠ 期望 E；E 下的交接标记不存在；§16 的 location_revision tag / floor 校验与现有删除 Lua 同口径。任一不满足返回 0（"状态已变"，什么都不写）。
2. `INCR owner_epoch`（E → E+1）。这是围栏：此后 E 下的存盘（严格屏障 Lua 比 owner）、标记写、入场登记、凭证都因 owner 不等而被拒。
3. 删除 location，按现有删除 Lua 的方式处理 location_revision 侧车（不删 floor）。
4. 返回 1。

撤销之后的下一次 EnterScene 看到无 location，按无持有者落点再铸一次（E+2），流程与首次进场相同。

### 17.6 安全论证

1. **不会回档**：由 I-1，撤销时没有 E 下的实体，也就没有 E 下未落盘的进度；E 之前各代已落盘的数据在父快照与 SQL 里，受提交屏障保护，新铸造照常加载。
2. **与在途登记的竞态**：X 上一次在途入场登记的 `kRegisterFenceLua` 若先执行，fence 原件变成 E 下的 H，撤销 Lua 的第 1 步判 fence 已变，返回 0，不撤销；若撤销先执行，owner 已是 E+1，登记脚本比 owner 失败，那次入场失败，客户端重登走新铸造。两种顺序都不会出现两个持有者。迟到帧（旧连接重传）同理，都被 owner CAS 拒绝。
3. **"采纳 E+2"窗口**：scene_manager 回滚把 location 推到 E+2 并写 "E+2:t" 标记期间，条件 2 不满足（标记在），不撤销。标记过期之后，源节点若还没取证采纳，fence 仍在 E。此时撤销会让采纳的 `kResumeFenceLua` 因 owner 不等失败，源实体按废黜处理；但它处于交接冻结中，冻结存盘已两表落地（`kPublishMarkLua` 的前置），没有未落盘进度。宽限期大于冻结上限，正常路径下采纳早已完成。
4. **与 GO-3 互不重叠**：GO-3 处理原件 E == location E、实例已从注册表消失；本机制只处理原件 E ≠ location E（或缺失）。两者都以推进 epoch 收尾。
5. **多副本**：判定与动作在一段 Lua 里，以 Redis 状态为准；计时键用 Redis `TIME`，各副本口径一致。两个副本同时撤销时，后到者第 1 步判 location 已变，返回 0。
6. **不扩大 §15 的放权**：撤销靠正证（§17.3）加围栏（§17.5 第 2 步），不靠超时；超时只决定"什么时候去检查正证"。

### 17.7 可观测性与测试

- 指标：`scene_manager_leave_holder_release_total{verdict="revoked_unqualified"}`，以及 EnterScene 释放门的同名 verdict（沿用现有指标族，不新增高基数 label，不用 player_id）。日志 WARN，带 node、期望 E、fence 原件 E（不打印完整原件）。C++ 侧 `credential_qualification_no_qualification` 仍计数，撤销后不再增长，可据此确认收口生效。
- Go 回归（miniredis，按 Redis 实际执行顺序重放，时钟注入，不依赖真实墙钟）：
  - 无资格且过宽限：撤销，owner 变 E+1，location 删除，侧车按规则处理；下一次 EnterScene 铸 E+2。
  - 未过宽限，或有 E 下标记，或原件 E == E：不撤销。
  - 登记先于撤销：返回 0，不撤销；撤销先于登记：登记脚本被拒。
  - location 原字节或 revision 已变：返回 0。
  - 两副本并发：只有一个返回 1。
- 跨语言金样：如果撤销 Lua 要读 `handoff_fence`，用 `owner_epoch_crosslang_test.go` 现有的 C++ / Go 副本比对方式钉住 `fence()` 解析的同一口径。
- C++ 侧不需要改动：撤销后 X 上任何 E 下的写都已被现有 owner CAS 拒绝。只需在 `cross-zone-scene-travel.md` 写入口对照节的残余 2 下补一句"由 location-release-binding §17 收口"。

### 17.8 接手说明（落码时按序做）

1. 先核对 §17.3 的不变量 I-1（读 `player_lifecycle.cpp` 的 A2′ 闸门、`InitPlayerFromAllData`、`RoleAdmissionSystem`、交接采纳、复用实体路径）。不成立就先改设计再落码。
2. 协调：`go/scene_manager/internal/logic/{leave_release.go, leavescenelogic.go, enter_holder_release.go, owner_epoch.go, owner_replaced.go}` 在 2026-10-24 有大量其他会话的未提交改动（PROGRESS「2026-10-23 跨 zone 收尾:GO-3 同号重启接管…」「scene_manager 死节点收尾…」，以及 zone Redis 回退防护会话的铸造地板 + WAITAOF）。开工前用 ListAgents 找到这些会话，确认文件归属；新 Lua 尽量放进新文件（例如 `unqualified_holder_revoke.go`），调用点只做小段锚点改动。
3. 落码：Lua 与 Go 判定、LeaveScene 与 EnterScene 释放门两个调用点、宽限配置键（`scene_manager` config，缺省 120s，启动时校验必须大于 85s）、指标、上面的回归用例。
4. 文档：本节改为"已落码"，§3.3 残余 1 与 `cross-zone-scene-travel.md` 残余 2 改为指向本节，`docs/ops/cross-zone-failure-test-runbook.md` 补 `revoked_unqualified` 的观测与演练项。
5. 交 Codex：`go/scene_manager` 的 gofmt、`go vet ./internal/logic`、定向与全量 `go test ./internal/logic`；没有 C++ 或 proto 变更。
6. Java 版：不适用。Java 的交接是一笔 MySQL 交出事务（PARITY 有意差异 D1），没有 location 标记链与 owner_epoch 铸造。按约定不登记 PARITY。
