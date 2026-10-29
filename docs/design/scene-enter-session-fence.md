# 入场会话栅栏:登录代次(login_generation)

- 日期:2026-10-14
- 状态:已定稿,代码落在同一批改动里;**未编译、未测试,待 Codex 验证**(验证步骤见 §11)。*2026-10-20 回填(只读核对 PROGRESS 回执,10-22 补):C++ 单测已绿。`cross_zone_test` 随机全量 313/313 含 `EnterFence` 5 个、`EnterFenceEcs` 15 个;`routing_identity_test` 113/113 含 `LoginGeneration` 10 个。出处是 PROGRESS「2026-10-19 Codex：会话释放夹具修复核验完成，跨区随机全量313项通过」,跑的是合并前源码。Go 侧 login 519+375、scene_manager 444+226、player_locator 117+18 的全量单测见 PROGRESS 10-19 各条回执。2026-10-22 又有一轮:29 个 C++ 测试 EXE 共 2624 PASS(其中 cross_zone_test 339/339、routing_identity_test 140/140),出处是 PROGRESS「2026-10-22 全球同服验收：C++29套收口及Unity真实失败（Java 0.1.0-SNAPSHOT）」。真实环境的同节点顶号场景(两台设备连不同 gate)没有实跑。*
- 范围:login → player_locator → scene_manager → gate → scene 的入场链。存盘链不改(见 §8)。
- 来源:`cross-zone-scene-travel.md` §13.9「scene 侧会话栅栏」、§13.8、§13.3 L4 登记的待办;
  本文取代那几处"遇到过期 epoch 应直接拒绝"的口径(epoch 修不了同节点顶号,见 §1)。

## 1. 问题

同一角色,设备 A、B 连到不同 gate,两次入场落在同一个 scene 节点:

1. A 登录(会话 sA)。scene_manager 把 A 的路由经 Kafka 发给 gateA;gateA 已向 scene 转发
   `PlayerEnterGameNode(A)`,scene 还没处理。
2. A 的登录链结束、玩家锁释放。**锁释放不代表 scene 已经绑定。**
3. B 拿锁顶号(会话 sB)。scene 先处理了 B 的入场,正确绑定 B。
4. A 的旧入场才到。修复前的 `PlayerEnterGameNode` 先 `ReconcileRelocateOnReentry`,再
   `RemovePlayerSessionSilently` 摘掉 B,然后 `EnterScene` 把会话绑回 sA。

两条 gate→scene 连接之间没有统一的 FIFO;A 即使已被踢,也撤不回已经发出的 RPC。

实际后果比"B 变哑"更重。gateA 上的 A 被踢、断线后,gateA 在**同一条连接上、紧跟在旧入场之后**
代发 `ExitGame(sA)`。此时快照正好是 sA,"快照会话 == 请求会话"的守卫放行,实体被退出存盘并销毁。
结果是 B 在 gate 与 player_locator 上都在线,scene 上没有实体。载入中、退出存盘在途两种变体也会被
旧请求覆盖 pending 或取消退出。

`owner_epoch` 不能用来区分:它只在**持有节点换了**时铸造(cross-zone-scene-travel.md R1),
同一节点上顶号两次路由的 epoch 相同。`session_version` 也不行,见 §3.5。

## 2. 结论一览

| 问题 | 决定 |
|---|---|
| 当前合法入场身份 | `(会话号, 登录代次)`。登录代次 = 每条进入持久化阶段的 EnterGame 登录链铸造一次的 uint64 |
| 权威来源 | player_locator 会话记录里随整字节 CAS 提交的 `PlayerSession.login_generation`;scene 侧比较的是"本节点已放行的最高代次" |
| 谁铸 | login,持玩家锁、写会话之前;算法 `max(计数+1, 当前毫秒, 会话记录代次+1)` |
| 何时清空 | 铸造键 7 天无登录自动过期;过期 / 丢失后重建的值不回退(有毫秒时间下限),所以不必进清库、回档、合服清单 |
| 贯穿 | `EnterSceneRequest(12)` → `RoutePlayerEvent(7)` → gate 会话 → `PlayerEnterGameNodeRequest(8)` → scene 的 pending / 实体;`BindSessionEvent(5)` 另带一份给 gate |
| gate | 只透传、只抬高:转发时用本会话、本角色见过的最高代次盖章;不据此丢弃任何路由 |
| scene 栅栏 | 入口第一步零副作用纯判定;同一会话一律放行,只裁决跨会话;绑定时与载入回调再复核 |
| 版本 0 | 当前有代次、请求无代次、会话不同 → 拒;双方都没有代次 → 沿用旧行为并计数 |
| 被拒时 | 计数 + 一条 WARN,给**被拒请求自己的会话**推 `kEnterSceneFailed`;不碰当前会话、pending、票据、待确认条目 |
| 存盘 | 不带登录代次;存盘的版本号仍是 `owner_epoch`(§8) |

## 3. 登录代次

### 3.1 不变量

- **G1 单调**:同一名玩家任意两次**已提交**的会话,后提交的代次严格更大。
- **G2 一登一号**:一个代次只属于一条会话(一次登录链一个会话号);两条不同会话不会带同一个非 0 代次。
- **G3 只增不比计数器**:任何校验方只比较"随请求携带的值"与"本地已放行 / 会话记录里的值",
  不拿铸造键的当前值当"当前合法代次"。铸了没用上的号(会话 CAS 失败、链失败)直接作废,不影响任何人。
- **G4 允许落后**:scene 上某玩家的"已放行最高代次"不大于会话记录值,可以落后(例如同连接重登时
  scene_manager 去重重放不补发路由)。**任何人不得在两者之间做相等校验。**
- **G5 不进存盘**:登录代次不参与存盘 CAS、不进 DBTask。同节点只有一个实体做写者,带了只会让合法存盘自拒。

### 3.2 铸造

- 键:`player:{player_id}:login_generation`(纯十进制字符串),放在 login 的 `Node.RedisClient` 上
  (与 `player_locker`、`player:session` 同一个核心实例,见 shared-redis-split-by-purpose.md §2.2)。
- 一段 Lua,`ARGV[1]` = login 本机 `now.UnixMilli()`,`ARGV[2]` = 锁内刚读到的会话记录代次(没有记录为 0),
  `ARGV[3]` = TTL 秒数:

  ```lua
  local nxt = (tonumber(redis.call('GET', KEYS[1])) or 0) + 1
  local nowMs = tonumber(ARGV[1])
  if nowMs > nxt then nxt = nowMs end
  local floor = tonumber(ARGV[2]) + 1
  if floor > nxt then nxt = floor end
  local s = string.format('%.0f', nxt)
  redis.call('SET', KEYS[1], s, 'EX', ARGV[3])
  return s
  ```

  值在 2^53 以内(毫秒时间戳约 1.8e12),Lua 的 double 表示无损。
- TTL 7 天。远大于在途消息的寿命(命令 topic 保留 1h、gate 补发预算 20s)与任何合理的时钟偏差。
- **为什么带时间下限**:键丢失(FLUSHALL、主从切换丢写、回档 FLUSHDB、TTL 到期)后,下一次铸出的值
  不小于当前毫秒,仍大于此前发过的所有值;再加上"会话记录代次+1"的下限,铸出值恒大于权威记录,
  有会话记录时与各 login 实例的时钟偏差无关。纯 INCR 在清库后会从 1 重来,scene 上残留的实体会一直拒新登录。
- **唯一依赖时钟的窗口**:会话记录已删(登出 / 租约到期)**且**铸造键也丢了,此时下限只剩本机毫秒。若上一次登录由
  时钟快 δ 的 login 实例铸造、它的实体(退出存盘在途)还留在 scene 上,δ 之内经别的实例登录、落到同一节点会被判
  `kRejectStale`,客户端按 3023 重试,直到新值越过旧值或旧实体销毁。需要键丢失、会话记录已删、时钟偏差三者同时成立,
  只伤活性;不为它另存一份删会话时的代次下限。
- **为什么不用纯时间戳**:多个 login 实例时钟有偏差,同一毫秒还会撞号;计数部分保证同一把 Redis 上严格递增。
- 失败处理:铸造失败(Redis 错误)= 登录链失败,先于踢线与写会话;**不得带 0 继续**。

### 3.3 何时铸

每条走到 `persistEnterGameSession` 的 EnterGame 链铸一次:首登、顶号、同连接重入(同 session_id 的
Replace)、短线重连都铸。跨 zone 第二条腿在目标 zone 的 login 上走 ShortReconnect,同样铸新值。

### 3.4 提交时的同步机制

- 代次写进 `PlayerSession.login_generation`(14),随 SetSession / Reconnect 现有的整字节 CAS 一起提交。
- player_locator 在提交点追加一条:**现有记录与请求的代次都非 0 时,要求请求代次严格更大**,否则按
  stale 拒(与 `session_version` 的 +1 校验同一错误语义)。失锁后两条并发登录链里只有一条能提交,
  会话记录的代次不会回退。
- 请求代次为 0(旧版 login)时不校验,原样写入 0:记录如实表示"这条会话没有代次"。
- Reconnect 应答回带的是会话记录;login 下游(Bind、EnterScene)一律用**本链铸出的值**,
  应答里的值与之不等时只打 WARN(旧版 locator 会原样保留未知字段,回出上一会话的值)。

### 3.5 与另外两个"版本号"的区别

| | `session_version` | `owner_epoch` | `login_generation`(本文) |
|---|---|---|---|
| 含义 | 会话记录每次真实变化 +1 | 数据归属(持有节点)代际 | 第几次登录 |
| 谁改 | login / player_locator | scene_manager(INCR) | login(持锁铸造) |
| 断线 | +1 | 不变 | 不变 |
| 同节点顶号 | +1 | **不变** | 变大 |
| 何时清空 | 登出 / 租约到期删会话键,下次从 1 重来 | 永不(无 TTL、代码不删) | 铸造键 7 天过期,重建不回退 |
| 用途 | 会话记录 CAS | 存盘 CAS、废黜判定 | scene 入场栅栏 |

## 4. 贯穿路径

| 跳 | 字段(uint64,只加字段) | 取值 |
|---|---|---|
| login → player_locator | `PlayerSession.login_generation = 14`;`ReconnectRequest.login_generation = 9` | 铸出的值 |
| login → gate(Kafka) | `BindSessionEvent.login_generation = 5`,经 `GateCommand.payload` 下发 | 同上 |
| login → scene_manager | `EnterSceneRequest.login_generation = 12` | 同上 |
| scene_manager → gate(Kafka) | `RoutePlayerEvent.login_generation = 7` | 原样 `in.LoginGeneration`;**不进 request_id 指纹** |
| gate → scene | `PlayerEnterGameNodeRequest.login_generation = 8`(同时补 `reserved 3;`,原 centre_node_id) | `SessionInfo.loginGeneration` |
| scene 内 | `PlayerEnterContext.loginGeneration`、实体 `PlayerLoginIdentityComp`、改派条目 `Registration.loginGeneration` | 见 §5 |

**Bind 修通**:`buildBindSessionCommand` 改为把 `BindSessionEvent{session_id, player_id, session_version,
enter_gs_type, login_generation}` 序列化进 `GateCommand.payload`(信封字段照填)。生成的
`gate_kafka_command_router.cpp` 的 `DispatchGateKafkaCommand` 对非空 payload 按白名单解包后直接调用 handler，`BindSessionEvent=32` 已在 gate 白名单里，
新旧 gate 都能解,不改生成产物。顺带修好 `session_version` 一直传不到 gate 的问题(gate 侧仍无读取方)。

**gate 的盖章规则**(只抬高、不丢弃):

- 会话上记 `loginGeneration` = 本会话、本角色见过的最高代次。
- Bind 到达:换了角色(会话已绑另一名玩家)→ 取 Bind 的值;同一角色 → 取 max。
  **必须在 `OnLoginTypeBound` 之前写**:它会同步按 `lastSceneRoute` 转发。
- Route 到达:路由属于会话当前角色(或会话还没绑角色)→ 取 max;属于别的角色 → 不动(这条路由随后被
  玩家围栏撤销)。同样在 `OnRouteDecision` 之前写。
- `ForwardPlayerToScene` 把会话值写进 `PlayerEnterGameNodeRequest.login_generation`。
- 两处写入收成 `scene_route_helper.h` 里的纯函数(`ApplyLoginBinding` / `ApplyRouteIdentity`),handler 只调用,
  routing_identity_test 直接测。

为什么 Bind 也要带:login 先发 Bind 再调 EnterScene。在一条进过场的连接上重登时,Bind 先到,gate 立即按
上一次的 `lastSceneRoute` 补发并消费登录类型;随后的新路由若同场景同节点会被判成重复、不再转发。
只靠路由带代次,这次补发带的是上一次登录的旧代次,在中间被别的设备顶过号时会被 scene 判陈旧,
合法重登进不了场。

为什么 gate 不据此丢路由:一次失败的重登(Bind 已到、scene_manager 拒绝)会把会话代次抬高,
之后同连接上 scene 自发的换图路由带的是实体的旧代次,按代次丢弃会把它们全吞掉。

**scene 自发的 EnterScene**(换图、队伍跟随、疏散改派、交接重发、镜像自动进场,都经
`SendCorrelatedEnterScene`)不填代次(0)。它们的路由到 gate 后由会话值盖章;scene_manager 只透传。

## 5. scene 栅栏

### 5.1 当前身份

纯函数与组件放新头 `cpp/libs/services/scene/player/system/enter_fence.h`(先例:`player_exit_intent.h`
把组件与纯判定放在一起)。

```cpp
struct PlayerLoginIdentityComp { SessionId sessionId{kInvalidSessionId}; uint64_t loginGeneration{0}; };
struct Identity { bool exists{false}; SessionId sessionId{kInvalidSessionId}; uint64_t loginGeneration{0}; };
```

一名玩家在本节点最多有三处"当前身份",存在的都要判,任一拒即拒:

1. 实体:`PlayerLoginIdentityComp`(没有该组件时退回快照会话、代次 0)。它独立于
   `PlayerSessionSnapshotComp`:入口第 2 步会把快照会话置无效,身份组件不受影响。
2. 待入场上下文:`PlayerEnterContext{enterInfo.session_id, loginGeneration}`。
3. 改派待确认条目:`relocate_confirm::Registration{sessionId, loginGeneration}`。实体销毁之后、落回之前,
   只有它记着这名玩家;不纳入的话陈旧入场会把条目误结成 `superseded`(§13.9 原文预言的问题)。

不另建"墓碑表"。三处都不存在时放行,理由见 §9 残余 1。

### 5.2 判定表(`enter_fence::Decide`)

| 当前身份 | 请求 | 结论 |
|---|---|---|
| 不存在 | 任意 | `kAdmitFresh` |
| 存在,会话号相同(且有效) | 任意代次 | `kAdmitSameSession`,代次取 max |
| 当前代次 0 | 代次 0 | `kAdmitLegacy`(旧行为:后到者胜),计数 |
| 当前代次 0 | 代次非 0 | `kAdmitNewer` |
| 当前代次非 0 | 代次 0 | `kRejectUnversioned` |
| 当前代次非 0 | 更大 | `kAdmitNewer` |
| 当前代次非 0 | 更小 | `kRejectStale` |
| 当前代次非 0 | 相等 | 当前有会话 → `kRejectConflict`;当前没有会话可比 → `kAdmitSameSession` |

**同一会话一律放行**:同一会话号只经同一条 gate→scene 连接到达,FIFO 之下到达序就是 gate 的决策序,
不存在"旧会话抢新会话"。若按代次拒同会话请求,gate 已在 kSent 时提交了场景与节点指向,
scene 却没执行,三方分叉且之后的同图换图被 gate 判重复吞掉。gate 重启后会话号复用的情形:
新登录拿到同号时代次更大,照样是接管;旧进程的迟到帧在新进程的入场之后到达,实际不会发生。

多处身份的合并是 `enter_fence::DecideAll`(纯函数):逐个判,遇到拒即返回;全放行时返回第一处**存在的**身份的结论,
三处都不存在时返回 `kAdmitFresh`(不存在的那几处恒为 `kAdmitFresh`,不参与合并,以免盖掉真正比过的结论;只影响计数口径,
不影响放行与拒绝)。

### 5.3 入口顺序

`SceneHandler::PlayerEnterGameNode` 守护段改为薄包装:从请求组 `PlayerEnterContext`,调
`PlayerLifecycleSystem::HandleEnterGameNode(playerId, ctx)`,只在返回 `kLoadRequired` 时调 `AsyncLoad`
(下沉到 scene.lib,测试宿主可直接调,用例不碰 Redis)。`HandleEnterGameNode` 的顺序:

0. **栅栏**(只读):`DecideAll(实体, pending, 条目)`。拒 → 计数、WARN、给请求会话推 `kEnterSceneFailed`、
   返回 `kRejected`。
1. `ReconcileRelocateOnReentry`(从入口最前挪到这里:放行之后,擦会话、废黜销毁、登记 pending 之前,
   它靠"此刻 pending 里有没有他"区分补发与第二次进场)。
2. `RemovePlayerSessionSilently`。
3. 1.5 / 1.6 的 owner_epoch 废黜判定(不变)。
4. 实体在 → `EnterScene` → `kEnteredExisting`。
5. 预登记会话;覆盖 pending(规则见 5.5)。
6. `BeginInheritedMarkClear` → `kLoadRequired`。

### 5.4 复核点

- **绑定时**:`EnterScene` 第 0 步(取消退出)之前用同一个 `DecideAll` 对实体再判一次,拒则直接返回。
  入口已判过,这里对入口路径是纵深防御,真正的价值在于 `EnterScene` 的其它调用方。放行后在第 1 步
  绑定会话的同时写身份组件:会话有效则记会话号,代次取 max。
- **载入回调**:`HandlePlayerAsyncLoaded` 现有的"pending 会话仍在 SessionMap"收紧为"且映射到本玩家"。
  pending 只会被栅栏放行的请求覆盖,所以回调取到的永远是已放行的最高代次上下文;载入建出的新实体
  随后经 `EnterScene` 再走一遍复核。
- 不为"取出 ctx 时实体已存在"加分支:实体与 pending 不并存(实体在时入口不登记 pending,实体只在
  本回调里、pending 擦除之后才建),为不可达状态写代码属于 YAGNI。

### 5.5 覆盖 pending

- 放行才覆盖。`inheritClear` 照旧原样沿用(§12.6.3 第三步、M2 不变)。
- 不同会话:旧 pending 的预登记会话只在仍映射到本玩家时擦掉。
- **同一会话**:合并而不是整体替换 —— `enter_gs_type` 新值为 0 时保留旧的非 0 值,代次取 max,
  其余(scene_id、home_zone、owner_epoch)取新值。否则"先登录类型、后 0 型换图路由"在载入完成前
  到达时会丢掉登录类型,不置登录态也不触发 `PlayerLoginEvent`。

### 5.6 只清自己的

`SessionMap` 的擦除一律改成"该会话号此刻映射到本玩家才擦"(`PlayerLifecycleSystem::EraseSessionMappingIfOwned`)。
适用于:`RemovePlayerSession`(入口第 2 步、`FinishExitAfterPersist`、`DestroyDeposedPlayer`、
`UpdateSessionDetail` 都经过它)、`EnterScene` 第 1 步擦旧会话、覆盖 pending 时擦旧预登记、
载入放弃的三条路径(RedisError、A2′ 拒绝、截止拒绝)。

原因:gate 崩溃后以同一 node_id 重启,会话序号从头发。玩家 Q 拿到玩家 P 旧会话的同号会话、映射已改成 Q;
P 随后经新会话重登通过栅栏时,按 P 快照里的旧号无条件擦,擦掉的是 Q 的在线映射,Q 从此"在线但无法操作"。

### 5.7 被拒时

- `enter_fence_stats` 按结论计数(平铺数组 + `static_assert(kCount)`,不用宏);RedisSystem 现有的 30s 快照
  定时器在计数有变化时打一行 `[EnterFence]`(英文,与现行日志口径一致)。
- 逐条一行 WARN:玩家、请求会话与代次、当前身份来源与值、结论名。
- 只对**请求自己的会话**推 `kEnterSceneFailed`(`SendTipToPendingSession`)。陈旧会话多半已被踢,
  会话不在时 gate 侧是 no-op;仍连着的(踢线丢失、混版本被拒的合法会话)客户端按既有 3023 契约断线重登,
  不会干等 60s。gate 写 socket 前有 player_id 身份栅栏,推不到别人身上;被拒请求的会话号一定不等于
  当前身份的会话号(同会话一律放行),推不到当前会话上。

### 5.8 UpdateSessionDetail

该 RPC 无条件改绑会话,非生成代码里没有发送方,但消息号可达。它不带代次,按代次 0 过同一栅栏:
实体已有代次且会话不同即拒;放行后同步写身份组件的会话号。

## 6. 版本 0 与升级 / 回滚

- 判定表已覆盖四种组合。"当前有代次、请求无代次、会话不同"拒绝,不留默认放行的窗口;
  "双方都没有代次"只在上游尚未铸造时出现,沿用旧行为并计数,login 全量铸造后应恒为 0。
- **升级顺序**:scene、gate → scene_manager、player_locator → login 最后。
  login 之前的组件全 0 时行为与现状相同。每一层滚完再滚下一层。
- **混版窗口**(login 滚动期间新旧实例并存,两个方向都要知道):
  - 实体已有代次,经旧 login 顶号 → `kRejectUnversioned`,请求会话收 3023,客户端重登;旧会话被踢、实体退出
    销毁后下一次即成功。前提是旧实体真的被销毁、且这名玩家没有带代次的 pending / 改派条目:退出因存盘失败重试或
    保留上限(`kRetainExhausted`)而被保留时,经旧版 login 的重登会一直被拒,直到实体销毁或改走新版 login。
  - 当前身份无代次(经旧 login 登录),同时有一条经新 login 发出、更早的入场迟到 → 判 `kAdmitNewer`,
    原缺陷在这一个方向上仍可能出现。需要原始竞态与窗口同时成立。
  - 两者都只在 login 滚动的几分钟内存在。
- **回滚**:先回滚 scene(摘栅栏),再回滚其它。若只回滚 login / gate 到不带代次的版本、scene 保留栅栏:
  已带代次的实体会拒绝无代次的异会话登录,直到旧会话退出(被踢 → ExitGame → 销毁)。gate 崩溃留下的僵尸实体
  收不到 ExitGame,只能等 scene 重启。
- **只回滚 scene_manager** 不会让栅栏失效(gate 仍经 Bind 拿到代次并盖章),但有一个副作用:旧版 scene_manager 的
  请求指纹会把新 login 带来的 12 号字段当未知字段算进去,同一 request_id 的重试(每条登录链代次不同)会在 60s 去重窗口内
  回 `ErrEnterSceneIdempotencyConflict`,客户端收 3023。所以要回滚 scene_manager,先回滚 login。
- 不设开关。scene 的开关只能在进程启动时读,而重启 scene 本身就清掉了所有已放行的代次,开关不提供额外能力。

## 7. 踢线 / 断线 / 退出 / 租约

| 入口 | 结论 |
|---|---|
| scene 的 ExitGame / 断线代发 | 栅栏保证快照不会被旧请求改回之后,现有"会话映射 + player_id 围栏 + 快照会话相等"三道守卫已精确 |
| 载入中的 ExitGame | 只擦请求会话自己的映射,现状够 |
| `UpdateSessionDetail` | 本次加栅栏(§5.8) |
| SessionMap 擦除 | 本次改为只擦属于自己的(§5.6) |
| login → gate Kick | **本次不改**,登记待办:只按 session_id;失锁的陈旧登录链可在 SetSession 失败前踢掉当前会话;建议 KickPlayerEvent 加 `player_id` 由 gate 核对,并把踢线挪到 SetSession 成功之后 |
| Disconnect / LeaveGame → player_locator | 现状够(按 session_id + 整字节 CAS / 双字段);gate 重启后同号复用仍是 R13 残余 |
| LeaveGame / 租约到期的 `LeaveScene` | **本次不改**,登记待办:MarkOffline 删会话后同步调 LeaveScene,期间无屏障,同落点重登不写 location,迟到的 LeaveScene 会删掉新登录的 location(`leavescenelogic.go` 自述) |
| gRPC `ReleasePlayer` | **本次不改**,登记待办:只带 player_id,迟到会把交接回滚后的实体退掉;根治是带预期 owner_epoch |
| 上面三条与下文提到的五项余项 a–e | 设计见 [scene-enter-chain-residuals.md](scene-enter-chain-residuals.md)(2026-10-16 设计稿,未落码) |

接手(2026-10-14):上表三条"本次不改",加上 §9 残余 2(scene_manager 落点 Lua 记会话身份),以及"ExitGame 带登录代次、
断线上报带 gate 实例号"这条可选加固,共五项,由并行会话「玩家消息投递与位置管理」在本批落地之后接手(该会话转述其用户已确认)。
它们改的文件与本批重叠(`gate_event_handler.cpp`、`enterscenelogic.go` 等),要在本批的结构上追加,不要回退本批的栅栏与盖章规则。

## 8. 存盘与版本号(用户追问)

- **存盘有没有版本号**:有,是归属代际 `owner_epoch`。scene 写 Redis 父键时用它做 CAS
  (`redis_client.h` 的 guard Lua),go/db 落库前再比已落库的最大 epoch(`consumer:applied_epoch:*`)。
  它防的是"换了持有节点后旧节点还在写";没有每次存盘的序号,MySQL 那一层是无条件整行覆盖。
- **何时清空**:`owner_epoch` 无 TTL、代码里没有删除,只增不减;能回退的只有 Redis 丢数据。
  `applied_epoch` 同样只升不降。登录代次的铸造键 7 天过期但重建不回退(§3.2)。
- **存盘是否有同类问题**:本缺陷**不造成存盘回档** —— 同节点顶号时内存里只有一个实体做写者,旧入场抢的是
  会话绑定权,不是数据;被旧会话的 ExitGame 销毁之前会先存盘收敛。存盘自己另有一个独立的"旧盖新"窗口:
  go/db 两个实例在同一 epoch 内的僵尸写会把 MySQL 整行覆盖回旧版。修法是 `(owner_epoch, save_seq)`
  条件写,已在 [db-task-pipeline-bottlenecks.md](db-task-pipeline-bottlenecks.md) 与
  [godb-pipeline-handoff-20261013.md](../handoff/godb-pipeline-handoff-20261013.md) 立项,待负责人授权,不在本次。
- 登录代次**不应**进存盘 CAS(G5)。

## 9. 残余与不在范围

1. **三处身份都不存在时放行**。此刻本节点没有更新的状态可被损坏:新会话随后到达时代次更大,照常接管;
   新会话在别的节点时 A2′ 按 owner_epoch 拒建(仅在 `AllowUnsafeCrossNodeHandoff=false` 下成立;
   dev 旁路会产生同 epoch 双实体,属旁路本身声明的不安全范围);新会话来过又退出销毁时,会建出一个
   绑死会话的实体,由 gateA 同连接上紧随其后的 ExitGame 或下一次登录收掉。不做墓碑表(要新表与不短于
   命令 topic 保留期的清空策略),不做权威读(要改 A2′ Lua,属不许碰的机制)。
2. **scene_manager 只透传**。陈旧的登录请求或旧会话在途的 scene 自发换图,仍会在 scene_manager 改写
   location 与人数;scene 拒掉之后 location 与实体所在场景分叉,到下一次落点收敛。修复前跨会话时 scene 会
   执行陈旧入场(即本缺陷本身),所以这是更小的恶。根治要在 scene_manager 的落点 Lua 里记会话身份,登记待办。
3. scene 上的代次可以落后于会话记录(G4)。
4. 混版窗口(§6)。
5. 失锁双链:双方都可能失败,客户端重试。
6. §7 中三条登记的待办。
7. 既有问题(不是本次引入,登记备查):gate 收到属于**别的角色**的迟到路由时,`ApplyRouteIdentity` 沿用原语义,
   仍无条件覆盖会话的 homeZoneId / ownerEpoch,`OnRouteDecision` 还会用它替换本角色在途的欠账(随后被玩家围栏撤销)。
   同一连接换角色时,新角色之后的补发可能带上旧角色的 epoch。登录代次已按角色隔离,epoch 没有;改 epoch 的覆盖规则
   属于归属语义,不在本次范围(cross-zone-scene-travel.md §13.3 L 系列同源)。
8. 第 2、6 条与 §7 接手说明里的五项(余项 a–e)的设计、选定方案和新登记的残余,见 [scene-enter-chain-residuals.md](scene-enter-chain-residuals.md)。

## 10. 测试与验收映射

| 验收项 | 用例(工程) |
|---|---|
| B 入场 → A 旧入场,映射与快照仍指 B | `EnterFenceEcs.StaleEnterAfterTakeoverIsRejected`(cross_zone_test) |
| A 已被踢关闭,迟到 RPC 不损坏 B | 同上 + 断言 SessionMap 无 sA 映射(迟到的 ExitGame(sA) 在 "session id not found" 即结束) |
| owner_epoch 相同仍拒绝 | 上两条用例两次请求 owner_epoch 都取 5 |
| 已有实体 / 异步载入两条路径 | `...ExistingEntity...` 与 `StaleEnterDuringLoadKeepsPending`、`LoadCallbackBindsNewestAdmitted` |
| 旧载入回调、旧断线、旧退出不清新状态 | `LoadCallbackBindsNewestAdmitted`、`StaleEnterDoesNotCancelExit`、`RemovePlayerSessionOnlyErasesOwnMapping` |
| 重复投递幂等、正常重连 / 顶号可用 | `DuplicateEnterIsIdempotent`(登录事件只触发一次)、`NewerSessionTakesOver`、`PendingSameSessionKeepsLoginType` |
| 并发冲突安全失败 | `EqualGenerationDifferentSessionRejected`、locator `SetSession` 代次不增即拒 |
| 判定表全覆盖 | `EnterFence.*`(纯函数) |
| gate 盖章 | routing_identity_test:`LoginGeneration.*`(Bind 先到按旧路由补发也带新代次、换角色重置、外来路由不动) |
| 铸造 | go/login `TestMintLoginGeneration_*`(递增、毫秒下限、记录下限、TTL、丢键后仍更大) |
| 接线 | go/login:SetSession / Reconnect 收到的代次、Bind payload 五个字段、EnterSceneRequest 带代次 |
| 透传与指纹 | go/scene_manager:路由事件带代次;同 request_id 仅代次不同走重放而不是冲突 |

## 11. 改动清单与验证

手写文件(生成产物由 Codex 重生,不手改):

- proto(4):`proto/player_locator/player_locator.proto`、`proto/scene_manager/scene_manager_service.proto`、
  `proto/contracts/kafka/gate_event.proto`、`proto/scene/scene.proto`。
- login:`internal/logic/pkg/sessionmanager/{session_manager.go, login_generation.go(新), login_generation_test.go(新)}`、
  `internal/logic/clientplayerlogin/entergamelogic.go` 及其测试、`internal/svc/{gate_command.go, servicecontext.go, gate_command_test.go}`。
- player_locator:`internal/logic/{setsessionlogic.go, reconnectlogic.go, logic_test.go}`。
- scene_manager:`internal/logic/enterscenelogic.go` 及其测试。
- gate:`cpp/libs/services/gate/session/comp/session_info_comp.h`、`cpp/nodes/gate/handler/event/{scene_route_helper.h, gate_event_handler.cpp, scene_entry_dispatch.cpp}`、
  `cpp/tests/routing_identity_test/routing_identity_test.cpp`。
- scene:`cpp/libs/services/scene/player/system/{enter_fence.h(新), player_lifecycle.h, player_lifecycle.cpp, relocate_confirm.h}`、
  `cpp/libs/services/scene/scene.vcxproj` 与 `.filters`、`cpp/libs/services/scene/core/system/redis.cpp`、
  `cpp/nodes/scene/handler/rpc/scene_handler.cpp`(守护段)、`cpp/tests/cross_zone_test/cross_zone_test.cpp`。
- 文档:本文、`docs/README.md`、`docs/PROGRESS.md`、`cross-zone-scene-travel.md`、`shared-redis-split-by-purpose.md`。

给 Codex 的命令、通过标准与失败时要保留的内容写在 `docs/PROGRESS.md` 本次条目里。要点:

- **不要用 `dev.bat proto`**:它固定用默认 `proto_gen.yaml`(`enable_unity_client: true`),会往未授权的
  客户端仓写文件,失败分支还会 `pause`。用 `dev_tools.ps1 -Command proto-gen-build`,再
  `proto-gen-run -UseBinary -ConfigPath <关掉 enable_unity_client 的副本>`。
- AGENTS.md §4 写的 `cd go && build.bat` 不产 pb(只跑 goctl 包装),与实际流程不符,已报给负责人。

## 12. Java 版

Java 版(`D:\luyuan\xuanming-server-mmo`)结构上不存在同类问题:每次进游戏都在库行上自增 `owner_epoch`
(夺权 SQL),它本身就是登录代次、永不清零;scene 的加载回调先比 pending 对象身份、再比库里与内存里的 epoch,
才摘旧实例。已有回归用例(`SceneWorldTest` 的"旧 epoch 不能顶替更新实例")。`PARITY.md` 登记为
「不适用(结构上不存在)」,需要本仓 commit 号,待提交后补。本次改的 proto 都是服务端内部消息,
不涉及客户端契约;Java 仓需在本仓提交后跑 `ContractSync` 同步 `proto/` 目录。
