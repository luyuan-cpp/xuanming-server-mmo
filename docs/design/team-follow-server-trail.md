# 组队跟随:服务器沿队长轨迹驱动队员(2026-11-06 服务端源码已写;2026-11-10 补入场后补推、客户端已写,补跨节点与离线托管的跨场景跟随;均未编译未测试)

> 用户要求(2026-11-06):「组队跟随应该是队长一移动，队员马上跟着路径移动的，队员间同动作，同间隔一起走」;
> 「上 `dtCrowd::requestMoveTarget` 单线程顶不住一万人一个线」;「队长原地小范围来回动是特殊情况你看怎么处理」。
>
> 本文替代 [team-system.md](team-system.md) 文末「组队跟随 v1.1」V.2 第 6 条「走路跟随放在客户端」的结论;
> 入队即跟随、跨场景跟随、归队判定等其余规则不变。

## 1. 结论

- **不寻路,不用 dtCrowd。** 队员不是独立的寻路代理,而是"站在队长走过的路上、离队长固定路程的点"。
  服务器每 tick 把队长的位置追加成一条折线(轨迹),第 k 名队员的位置 = 轨迹上离队长 (k+1) × 间距 的那一点。
- **同动作、同间隔**:队长走多少,每个槽位就前进多少。全队同速、同一 tick 起步和停步、在队长拐弯的同一个拐角拐弯,
  沿路间距恒为 1.4。观察者收到的是同一个服务器、同一批 tick 发出的移动广播,所有人屏幕上的队形一致。
- **原地小范围来回走**:轨迹"回收"(队长回到自己刚走过、还没有队员走到的那段路附近,就把那段回路剪掉)
  加队员"只进不退"。队长在第一名队员前面怎么晃,队员都原地不动;晃完往外走,队员走的是剪干净的路。
- **离线也跟**:走位由服务器驱动,不需要客户端在场;队员下线后照样跟着队长走(§3.6)。开通了挂机的(离线托管),队长换地图、
  换线、换到本 zone 的别的节点,也跟过去(§3.7)。
- **跨节点也跟**(2026-11-10 起):队长在本 zone 的别的 scene 节点上时,队员经标准的同 zone 交接跟过去,不再只限同一节点(§3.7)。跨 zone 仍不跟。
- **兼容**:只驱动客户端声明了跟随(`TeamFollowC2S{follow=true}`)的队员。旧客户端从不声明,行为与改动前完全一样。

## 2. 为什么不是 dtCrowd

dtCrowd 解决的是"一群各自有目的地的代理,互相避让着走":每 tick 每个代理做邻居查询、局部边界、路径走廊修正、
速度采样避障;`requestMoveTarget` 每次都触发一次(分片)寻路。拿它做组队跟随有三个问题:

| 问题 | 说明 |
|---|---|
| 单线程预算 | 一个线 1 万人、全组队时 8000 名队员。每代理每 tick 的避障采样与邻居/墙段查询是微秒级以上,乘 8000 × 20 FPS,单个 loop 线程放不下(量级估算,未实测);队长每次移动还要给 4 名队员各发一次 `requestMoveTarget`,即每秒数万次寻路请求 |
| 结果不是要的形状 | 避障会把队员从队长的路上推开、彼此让位,队形是"一团人各自赶路",做不到"同间隔一条线" |
| 容量模型不合 | `dtCrowd::init(maxAgents)` 按固定上限分配,邻居数 `DT_CROWDAGENT_MAX_NEIGHBOURS` = 6,设计面向几百个代理 |

本方案每 tick 每支队伍只做 O(队员数) 的游标推进;导航只在队员加入时用一次 `FindPath`、在轨迹回收时用一次射线。
被驱动的队员还退出了 `MovementSystem` 的逐 tick 导航射线和客户端移动上报的逐条裁决,这部分负载反而减少。

## 3. 模型

纯逻辑在 `cpp/libs/services/scene/player/system/team_follow_formation.{h,cpp}`(无 ECS、无导航、无时钟,单测直接驱动),
ECS 接线在 `team_follow_system.{h,cpp}`,运行时组件在 `player/comp/team_follow_comp.h`。

### 3.1 轨迹(`TeamFollowTrail`)

- 每 tick(`World::Update` 里排在 `MovementSystem::Update` 之后)把队长的服务器位置喂进去,离上一点不足 5cm 不加点。
- 每个点带"里程":沿折线累计的水平长度。队员的位置就是一个里程,`PointAt(里程)` 在段内线性插值(含高度)。
- **可走性**:相邻两点是队长实际走过的直线段 —— 客户端上报经 `ApplyReportedLocation` 的导航射线裁决,
  tick 间积分经 `MovementSystem::Update` 的射线夹持,所以沿折线走不会穿墙,队员不需要寻路。
  队长一 tick 挪了 4 米以上(被瞬移、被裁决拉回到远处)视为不连续,轨迹从新位置重来,队员按加入规则重新落位。
- 长度有界:只保留到最后一名队员的游标(暂停中的队员最多落后 24 米),再加队长身后 7 米历史。

### 3.2 槽位与推进(`TeamFollowFormation`)

- 名次按入队序号 `join_seq` 升序(与组队界面一致),同号按玩家号;队长之外最多 4 人。
- 第 k 名的目标里程 = `max(轨迹尾, 队长里程 - (k+1) × 1.4)`。
- 每 tick 队员游标向目标前进,单步上限 `max(队长本 tick 前进的路程, 10.8 米/秒 × dt)`:跟上之后与队长逐 tick 同步(间距不被拉开),
  落后时(刚加入、前面有人离队)以 10.8 米/秒(基础移速 9 的 1.2 倍)追上。
- **只进不退**:目标在游标后面时原地站着,绝不倒着走。

### 3.3 特例:队长原地小范围来回走

**回收**:新位置离轨迹头部 4 米之内的某一段路不超过 0.6 米,且"直接连过去"比"沿轨迹绕过去"至少近 5cm,
就把那一段之后的轨迹剪掉,用一条短捷径连到新位置(捷径照样用导航射线校验,不可走就不剪)。
回收**不越过最前面那名队员的游标**:已经有人站上去的路不能剪。

几种走法的结果(都有单测,见 §8):

| 队长的走法 | 队员 |
|---|---|
| 沿刚走过的路往回走一小段再走回来,反复多次(只要不退到第一名队员身上) | 完全不动;轨迹不变长 |
| 横着往旁边走 1 米、再往另一边走 1 米,反复 | 第一次往旁边走时沿路跟上 1 米(那一下和真拐弯分不出来),之后不再被带动 |
| 绕一个比间距小的圈 | 第一圈时沿路往前挪不超过一圈的长度;圈在第一名队员走到之前合上、被整圈剪掉,之后再绕不再带动 |
| 绕一个比间距大的圈 | 队员已经走进圈里,圈不能剪,队员跟着绕 —— 这是真实行走,不算"原地" |
| 真掉头,沿原路往回走穿过整支队伍 | 轨迹在第一名队员脚下折成发夹:第一名原地转身,等队长走出 1.4 后跟上;后面的人依次走到折返点再掉头。顺序不乱,沿路间距恒定。玩家之间没有碰撞,掉头时会互相穿过 |

为什么不用"起步阈值 / 死区"(队长离开某个半径才让队员动):那会让每次起步都慢半拍、队伍先拉长再收拢,
违背"队长一移动，队员马上跟着"。只进不退 + 回收已经让来回走不传到队员身上,不需要死区。

### 3.4 队员加入

`PlayerTeamSystem::RegroupToLeader` 在既有守卫(战斗锁、归属交接、换图在途、同 zone 同节点同场景)通过后调
`TeamFollowSystem::TryAttach`;同场景接上不看会话,离线的队员也能接上(§3.6)。落位按顺序取第一个成立的:

1. **已站在队长走过的路上**(离轨迹 ≤ 0.75 米,例如整队打完一场战斗):就地接上,槽位靠前就沿路赶过去,靠后就等。
2. **能走过去**(互相可见且相距 ≤ 8,即原来的"不需要归队瞬移"):先把谁都不需要的旧历史剪到新队员的槽位处,
   再 `FindPath` 寻到轨迹尾部,把这段路接在轨迹前面,队员站在新的尾部,沿路走到自己的槽位。路长超过 16 米,
   或寻路终点到不了尾部(Detour 的半截路),不走。
3. **否则瞬移进槽位**(`TeleportWithinScene`,`TeleportS2C.reason = 5`,朝向取队长的朝向)。

新入队的人名次最大,走到队尾、停在最后一名身后 1.4 处。名次在中间的人(暂离后归队)从队尾沿路走过其他人回到自己的位置,
被他让出位置的人原地等队长走开再补上。

### 3.5 停止、暂停与队长变化

| 情形 | 处理 |
|---|---|
| 客户端暂离(`TeamFollowC2S{follow=false}`) | 立即停止驱动 |
| 离队 / 被踢 / 解散 / 换队(`ApplyMembership` 发现 TeamId 变) | 作为队员停止驱动,作为队长解散队形 |
| 队长换人(转让、队长离队后改选、惰性转让) | 组队服务给留在队里的全员也发一次刷新信号(`Decision.LeaderChanged`,`go/match/internal/team/notify.go`)。新队长停止被驱动、旧队长解散队形、其他人在各自的刷新里接到新队长身后 |
| 自己或队长换了场景 | 停止驱动;到了新场景按既有的跨场景跟随再接上 |
| 自己或队长正在离开本节点(归属交接、退出) | 停止驱动 / 解散。因队长离开而解散的,原随从记下"在等队长的去留",队长实体离开本节点或交接作废之后再跟一次(§3.7) |
| 队长在战斗中 | 整队原地不动,不解散(整队开战时队员也都在战斗里;战后接着走,不用重新排队) |
| 队员在战斗中 | 暂停:原地不动,名次照留;落后超过 24 米后不再为他保留轨迹,恢复后直接瞬移进槽位 |
| 队员离线(断线保留中;以后的离线托管) | **照常被驱动**,跟着队长走,见 §3.6 |
| 队长实体没了 | 每 tick 末尾扫一遍,队形已不在的随从摘掉 |
| 严格导航地图(节庆图)拒绝这一步走位或放进槽位的瞬移 | 停止驱动,原因记 `LEFT_FOLLOW`(11-09「节庆地图严格导航」提交 `6cc189611` 加入);跟随意图不摘,之后的声明或跟随检查会再试 |

停止驱动时:清零速度(此后 `MovementSystem` 不再排除他)、上一条广播还在走就补一条停步、摘掉瞬移落位等待、
发 `TeamFollowStateS2C{driven=false, reason}`。

### 3.6 离线的队员也跟(2026-11-06 用户要求,为以后的离线挂机留路)

服务器驱动走位不需要客户端在场,所以队员下线不影响跟随:

- **已经在队形里的**:断线进入断线保留(`PlayerConnectionLostComp`)后照常被驱动,跟着队长走,不暂停。
  断线保留进场时 `StopMotionForExit` 会清一次速度并给观察者发停步,下一 tick 队形把他当作重新起步立刻补发移动广播,
  路人屏幕上不会停顿。发给本人的移动广播与 `TeamFollowStateS2C` 在没有活会话时不发。
- **同场景里接上不要求活会话**:`player_team.cpp` 的 `CanFollow` 不再看会话。离线队员在队长换人、队长回到这个场景
  (扇出)时照样接到队长身后。前提是有跟随意图:断线前声明过跟随(意图挂在实体上,断线不摘),或开通了挂机、托管开始时
  由服务器挂上(下面第 3 条);没开通挂机、暂离中下线的不跟。
  这一步要先读组队权威(`RefreshMembership` → match `TeamAuthority/ReadMembership`)。2026-11-10 核实:这条读只认角色登记
  (`RoleAdmissionSystem::ConfirmedIdentity`,match 侧 `ReadTeamAdmission`),不看会话在不在线;开通挂机的队员断线后,
  player_locator 在月卡有效期间给会话续租,登记一直有效,所以托管中照样读得到、接得上。没开通挂机的,25 秒保留期内登记也还在。
  读不成(返回 retry)时只是这次不接上,已在队形里的不受影响。
- **仍要活会话的两条路径**:
  - 带 gate 的跨场景跟随(队长换地图 / 换线):要经 SceneManager 发 EnterScene、改 gate 路由。离线托管的队员(开通了挂机)
    改发托管落点,不要活会话(§3.7);普通断线保留中的(没开通挂机,25 秒)不发,留在原场景,队长回到这个场景时由扇出重新接上。
  - 旧客户端(没声明跟随)的归队瞬移:排队靠他自己的客户端,离线时不瞬移,与以前一致。

**用户要求(2026-11-06)**:开通了挂机的队员,客户端离线后照样跟。

没开通挂机的,断线保留只有 25 秒(`connection_lost_hold::kHoldWindow`),到期就存盘退出,队形随之摘掉他。
"开通挂机"在代码里就是挂机月卡:玩家域 Redis 键 `player:afk_pass:{<player_id>}`,值为到期的 Unix 秒
(由购买 / 开通流程写入);player_locator 的租约监视在月卡有效期间每 5 分钟给断线中的会话续一次租约
(`go/player_locator/internal/logic/leasemonitor.go` `afkPassState`)。要做到"开通挂机离线照样跟",分三块:

1. **离线托管本身(已写,`player-connection-lost-hold.md` §4.1)**:断线保留时查月卡,有效就不按 25 秒退出,
   保留到重连、月卡到期或被别的退出取代;第一次确认月卡有效时调下面第 3 条的入口。跟随代码不用为此改 —— 它只看实体在不在、
   在不在同一场景、有没有跟随意图,不看会话。
2. **离线队员跨场景跟随(2026-11-10 已写,§3.7)**:离线托管中的队员发托管落点(不带 gate、带断开那次登录的身份),
   scene_manager 提交后直接通知目标 scene 节点把他放进场景(player-connection-lost-hold.md §4.2)。同节点就地换场景,
   跨节点走与玩家自己换图到别的节点同一条交接链。
3. **托管开始时由服务器开启跟随(已写)**:`PlayerTeamSystem::OnOfflineHostingStarted(player)` —— 在队伍里就挂跟随意图,
   没接上就走一遍跟随检查,不依赖客户端声明;下线前暂离的也照样跟,重连后客户端按自己当前的选择重新声明(§5)。
   调用方是第 1 条的 `AfkHostingSystem::ApplyProbeResult`(第一次确认月卡有效时)。
4. **托管中的角色被重新登录接回(2026-11-10 补)**:玩家杀掉客户端后再登录,EnterScene 按"换会话"先离场再完整入场,
   新客户端收到入场通知后把自己的跟随状态清成"没被驱动",而服务器还带着他走。入场之后
   `PlayerTeamSystem::OnEnteredScene` 先调 `TeamFollowSystem::ResendStateAfterEntry`:仍接在队长身后(队形记的场景就是他现在的场景)
   就当场补推一次 `driven=true`,不等随后的组队读 —— 那条读可能失败或被别的守卫挡住,客户端就会以为自己能走、
   上报却被整条丢弃。换了场景实例的不推:下一 tick 按换场景停止驱动,随后的跟随检查再接上。

### 3.7 跨场景跟随:跨节点,以及离线托管的队员(2026-11-10,代码已写,未编译、未测试)

用户要求:"队员开了离线挂机卡,然后完全杀掉客户端",队长穿过传送点换图、换线,队员也要跟过去,到了之后照常被服务器接到队长身后。

**一条规则**:队长在本 zone 的另一个场景(本节点或别的节点),队员就发换场景请求;scene_manager 是落点的唯一裁决者,
目标在别的节点时回 18,跟随的应答处理按同一个目标起**服务器发起的同 zone 交接**(冻结 → 存盘 → 写落盘标记 → 重发),
与玩家自己换图到别的节点是同一条链。在线与离线托管的区别只在"路由怎么送过去":

| 队员 | 换场景请求 | 提交点(路由) | 到了之后 |
|---|---|---|---|
| 在线 | 带 gate 会话 | scene_manager 推 gate 路由,gate 把会话改绑到目标节点 | 目标节点入场,按既有规则接到队长身后 |
| 离线托管(断线保留 + 挂机月卡确认有效) | 托管落点:不带 gate,带断开那次登录的身份 | scene_manager 直接推 `PlayerHostedPlacementEvent` 给目标 scene 节点(player-connection-lost-hold.md §4.2) | 目标节点不绑会话入场,按断线托管重建保留并查月卡;托管开始时由服务器开启跟随,接到队长身后 |
| 普通断线保留(没开通挂机,25 秒) | 不发 | — | 留在原场景;队长回到这个场景时由扇出重新接上 |

**代码**:

- `player_team.cpp` `FollowLeaderAsync`:删掉"队长在别的节点就跳过"(`reason=cross_node`)的守卫,同 zone 守卫保留;换场景分支按上表分三类
  (`HasLiveSession` / `PlayerLifecycleSystem::OfflineHostedIdentity`)。这一改动同时让**在线**队员也跨节点跟随 —— 两者是同一条交接链,
  只在线才跟会让离线托管的反而比在线的走得远;team-system.md DV-6 的结论随之修订(它的依据"跟随请求不写落盘标记"已被这条交接链取代)。
- `player_lifecycle.cpp` `HandleSceneChangeEnterSceneReply`:跟随的应答(`playerRequested = false`)收到 18 时调
  `StartTravelHandoff(..., TravelInitiator::kServer)`;其余拒绝照旧只记日志。所有结局都不回 tip(`PlayerTravelHandoffComp.serverInitiated`,
  `AbortTravelHandoff` 据此静默);"标记已发出、归属核实不了"仍按原规则踢线,那是一致性的收口,与谁发起无关。
- `StartTravelHandoff` 的会话闸:服务器发起、同 zone、离线托管中的放行;`PublishAndEnterTravel` 重发时没有活会话就发托管落点。
- **原随从的唤醒**:队长在别的节点落地后,那个节点的扇出只照顾它本地的队员,没有任何信号会叫醒原节点上的随从(离线托管的尤其如此)。
  所以队形因"队长离开本节点"(交接、退出、实体没了,`LEADER_GONE`)解散时,`TeamFollowSystem` 给仍声明跟随的原随从挂
  `TeamFollowAwaitLeaderComp`;`World::Update` 每 tick 在 `TeamFollowSystem::Update` 之后调 `PlayerTeamSystem::FollowAfterLeaderSettled`,
  取走去留有了结论的(队长实体已不在本节点 —— location 已指向新落点;或队长不再处于离开中 —— 交接作废),各走一遍跟随检查。
  为什么不在解散那一刻就检查:那时队长的交接刚开始,location 还指着本节点的旧场景,检查只会跟回原地。最多等 90 秒
  (`kTeamFollowAwaitLeaderFrames`,盖住交接冻结上限 70 秒与退出存盘),等满就放弃,之后靠别的信号(队长回来的扇出、客户端重新声明、战斗结束)。
  没有在等的随从时这一步是一次空 view 遍历。
- **托管中的兜底重试**(2026-11-11 补):上面都是"当场触发",漏掉一个信号(那一刻不在队形里、交接被拒、目标节点镜像还没同步……)
  在线队员靠自己的客户端兜底 —— 没被驱动就每 5 秒重新声明一次跟随(客户端 `TeamRegroupPolicy`,队长不在线时不重试)。
  离线托管的没有客户端,由服务器做同一件事:1 秒保留扫描(`ExpireConnectionLostHolds`)里,托管中的角色每 15 秒
  (`connection_lost_hold::kHostedFollowRecheckPeriod`,按玩家号错开到不同的秒,`IsHostedFollowRecheckDue`)调一次
  `PlayerTeamSystem::OnOfflineHostingHeartbeat`:在队伍里、有跟随意图、没接在队长身后的,走一遍刷新与跟随检查;已接上的不发任何读。
  周期比客户端长,是因为正常路径都当场触发,这里只补漏;每次的代价是一次组队读加至多两次 Redis GET,只落在没接上的托管队员身上,
  按节点分摊,不引入全局状态。
- **节点排空 / 疏散时**(2026-11-12 补,player-connection-lost-hold.md §4.3):托管队员所在的频道被缩容、节点滚动重启或失去身份时,
  他不再存盘下线,而是拿托管票据走与在线玩家同一条改派链,按托管落点搬到别的节点(去向按「原频道 → 同图 → 默认大世界」解析)。
  到了之后与跟随换场景到达同一套:重建托管、查月卡、托管开始时由服务器开启跟随,队长在本 zone 别的场景就照上面的规则再跟过去。
  改派被拒且能证明没放行时有界重发两次;托管落点到了目标节点却没建起来(角色登记 / 认领一时失败)时,目标节点按退避有限次重试
  同一条落点(同文 §4.4)。在线队员不受影响。

**一次完整的离线托管跨节点跟随**(队长 L 与离线托管的队员 M 在节点 A,L 穿过传送点去了节点 B 的场景 S2):

1. L 在 A 起交接、冻结 → A 上的队形以 `LEADER_GONE` 解散,M 挂上等待组件;L 的交接放行,L 在 A 上被销毁(location 已指 B / S2)。
2. 下一 tick `FollowAfterLeaderSettled` 取走 M → 组队读 → 读 L 的 location:本 zone、节点 B、场景 S2 ≠ M 的场景。
3. M 离线托管中 → 发托管落点(S2,断开那次登录的身份)→ scene_manager 回 18(M 在 A,目标在 B,没有标记)。
4. 应答 → `StartTravelHandoff(kServer)`:冻结 M、存盘、写落盘标记 → 重发托管落点 → scene_manager 凭标记铸新代、CAS 落点、
   推 `PlayerHostedPlacementEvent` 给 B(提交点)、给 A 派 ReleasePlayer(A 交接在途,忽略)→ 应答成功 → A 原子取证判已放行,不存盘销毁 M。
5. B 收到事件 → 角色登记 + 交接栅栏 → 认领载入(新代)→ 放进 S2,不绑会话 → 重建保留、查月卡 → 托管开始 → 服务器开启跟随 →
   组队读 → L 与 M 同场景 → `TryAttach` 接到 L 身后(离得远就直接放进槽位)。

同节点(S2 也在 A)少掉第 3、4 步的交接:scene_manager 不铸代、直接提交,事件发回 A,A 复用实体就地换场景,保留随实体留着,
跟随意图本来就在,随后的组队刷新(`OnEnteredScene`)就把他接到队长身后。

**守住的两条**:强一致 —— 跨节点走的是既有的落盘标记 + 铸代 + 写权行这一套,源节点冻结到销毁不再写,目标节点以新代认领,提交点只有一个
(broker ACK);可水平扩展 —— 没有新的全局状态或单点,托管落点按目标节点号分区投递,等待组件只在本节点内存里、每 tick 只遍历在等的几个人。

## 4. 同步

- 被驱动的队员每 tick 写 `Transform`(位置、朝向)与 `Velocity`(本 tick 位移 / dt),脏位与普通移动相同;
  `MovementSystem::Update` 按 `TeamFollowerComp` 排除他,不会再积分一遍。
- 移动广播 `ActorMoveS2C` 发给正在显示他的观察者**和他本人**。节流:起步 / 停步、拐弯超过约 20°、速度变化超过 15%
  时立刻发,行走中每 5 帧(250ms)补一条校正,站着不发。量级与改动前队员客户端自己每 0.25 秒一条 MoveSync 相同。
- 同一队形同一 tick 要发的移动合成一批(2026-11-12,`spatial/system/movement.h` 的 `ActorMoveBatch`):按"每个接收者要收哪几名队员"
  分组,一组一条 —— 只收一个人时仍是 `ActorMoveS2C`,收几个人时是 `ActorMoveListS2C`(客户端逐条按 `ActorMoveS2C` 处理,
  自己的那条照样只在被驱动期间生效,过滤在 `ActorWorld.ApplyMove`,两种消息走同一处)。每组序列化一次、每个 gate 一个请求,
  与单条广播同一个形状。队员多半同一 tick 起步、停步,起步对齐时行走中的校正也对齐,所以观察者通常每 tick 每队只收一条;
  视野不同的接收者分到不同的组,最坏退回到每人一条。停止驱动那一刻补的停步不进批、就地发出,保持它在
  `TeamFollowStateS2C{driven=false}` 之前发出(到达先后本来不保证,见 §5 第 3 条)。
- 朝向:口径收在 `spatial/system/facing.h`,与客户端上报同一份(客户端 `WorldCoordinateConverter`):`Rotation.z` 是偏航角,单位**度**,
  服务器坐标里朝向 = (cos z, sin z),所以写 `atan2(dy, dx)` 换算成度,x / y 取 0;这一 tick 没有水平位移时保留原朝向。
  瞬移进槽位时直接拷队长的朝向。技能面向目标(`ViewSystem::LookAtPosition`)走同一份口径,2026-11-12 修正(此前写的是弧度、
  俯仰放在 x,客户端把 x 当翻滚),并像其它写 `Transform` 的入口一样标脏。客户端的角色动画按位置变化取方向,
  所以朝向错了画面上也不明显,但新进视野的人拿到的快照朝向此前是错的。
- 被驱动期间客户端的 `MoveStart / MoveSync / MoveStop` 整条丢弃。

## 5. 客户端契约(新增协议,`proto/scene/player_movement.proto`)

```proto
rpc TeamFollow            (TeamFollowC2S)      returns (Empty);  // ONE_WAY
rpc NotifyTeamFollowState (TeamFollowStateS2C) returns (Empty);  // PUSH
```

服务器一侧的承诺:跟随状态以服务器为准,每条 `TeamFollowStateS2C` 都是完整状态(不是增量);客户端收到入场通知后
把状态当作"没被驱动",仍接在队长身后的,服务器在入场之后当场补推一次 `driven=true`(§3.6 第 4 条)。

客户端要做的:

1. 自己是队员时声明跟随(没暂离发 `follow=true`):入队之后、每次进入场景(含重连)之后各一次;玩家点"归队"立刻发 `true`、
   点"暂离"立刻发 `false`。重复发送是幂等的。已经被驱动时不必再说(包括离线托管时服务器替他开的跟随,§3.6 第 3 条)。
   想跟却一直没被驱动(队长在别的场景、刚换线、这次没接上)时,隔一段时间再发一次 `true`,充当原来的"请求归队";
   队长换人不用客户端出声,服务器在刷新里自己把队员接到新队长身后,没接上时上面的重试兜底。
   暂离中却还被驱动(断线期间托管开始,带着暂离接回)时再发 `false`,恢复他自己的暂离。
2. 收到 `TeamFollowStateS2C{driven=true}`:停止按输入驱动本地角色、停止上报移动,
   本地角色改按发给自己的 `ActorMoveS2C`(entity 为自己)与 `TeleportS2C` 显示,平滑方式与远端角色相同。
   被驱动期间玩家点地 / 按键 / 点 NPC 寻路:只提示"正在跟随队长,点「暂离」后可以自己走"。
3. 收到 `driven=false`:落到最后一条移动给的位置,恢复本地控制,下一次输入从这里照常上报。
   `reason = LEADER_CHANGED` 时服务器会随后把自己接到新队长身后。
   发给自己的 `ActorMoveS2C` 与 `TeamFollowStateS2C` 走不同的发送路径,**到达先后不保证**(`client_message_sender.h`):
   客户端只在 `driven=true` 期间使用发给自己的 `ActorMoveS2C`,其余时间收到的一律忽略(停止驱动前补发的那条停步可能晚到)。
   `TeleportS2C` 不受此限,任何时候都照常吸附;驱动期间那次 `MoveAck` 迟到裁决不再把人拽回去。
4. 远端队员(别的队伍、别的队员)不需要任何新逻辑:它们的移动本来就由服务器广播。

客户端实现(`mmorpg-client` main,代码随客户端提交 `f8076ce9` 入库,11-11 补的用例与兼容说明在工作区未提交;
**都未编译、未在 Unity 里运行**,协议生成、编译与冒烟交 Codex;客户端兼容说明见该仓 `Docs/ClientProtocolCompatibility.md`
「组队跟随改为服务器驱动」一节):

- `Assets/Scripts/Game/GameClient.TeamFollow.cs`(新增 partial):`TeamFollowDriven`、`SceneEntryEpoch`、`SendTeamFollow`,
  用生成的 `SceneMovementClientPlayerRpc.OnNotifyTeamFollowState` 注册推送;`GameClient.cs` 只加四处挂钩:
  注册、入场通知时复位并让入场计数加一、断线时复位、被驱动时忽略 `MoveAck`。
- `Assets/Scripts/World/ActorWorld.cs`:`LocalServerDriven` / `SetLocalServerDriven`,被驱动时本地角色走远端插值;
  `Assets/Scripts/World/Tianyong/TianyongPlayerController.cs`:`ServerDriven` 模式(关掉 CharacterController、不上报、
  输入与 `SetDestination` 只换提示),删掉原来本地跟随的接管接口。
- `Assets/Scripts/Game/Team/TeamFollowDeclaration.cs`(新增,纯逻辑:第 1 条的时机)、`TeamFollowDriver.cs`(重写,只声明与显示状态)、
  `TeamRegroupPolicy.cs`(重试节奏);`UI/Ugui/Team/TeamFollowUiRoot.cs` 不再走 `TeamClient.RequestRegroup`;
  删除客户端轨迹跟随 `TeamFollowTrail.cs`、`TeamFollowPlanner.cs` 及其测试。
- 用例:`Assets/Tests/EditMode/Tianyong/TeamFollowDeclarationTests.cs`(新增)、`TeamRegroupPolicyTests.cs`(随接口改)。
  11-11 在 `ActorWorldFoundationTests.cs` 补了服务器驱动五条:被驱动时按自己的移动外推、停止驱动落到最后位置之后不再挪人、
  `Clear` 复位、控制器被驱动时关掉 CharacterController 且只出提示、`GameClient` 收到 382 切换驱动并在入场通知时复位、入场计数加一。

旧客户端不发 `TeamFollowC2S`,服务器对它保持原来的规则(归队瞬移到队长脚下,走路跟随由客户端自己做)。
新客户端连旧服务器:`TeamFollowC2S` 没有处理方,`driven=true` 永远不来,客户端应停在"不被驱动"的状态。

## 6. 性能预算(1 万人一个线,全部 5 人组队:2000 支队伍、8000 名队员;量级估算,压测前不作结论)

| 项 | 本方案 | 改动前(客户端跟随) |
|---|---|---|
| 每 tick 跟随计算 | 每支队伍:喂一个点(回收只搜头部约 9 段)+ 4 名队员各一次游标推进、一次二分插值、若干次 ECS 查找;约微秒级 | 无 |
| 队员逐 tick 导航射线 | 无(被 `MovementSystem` 排除) | 行走中每人每 tick 一次 `ValidateMove`(吸附 + 射线 + 吸附) |
| 队员移动上报裁决 | 无(整条丢弃) | 每人每秒 4~8 条,每条一次 `ValidateMove` |
| 寻路 | 只在加入时一次 `FindPath` | 无(客户端自己寻) |
| 广播 | 同一队形同一 tick 合成一条(§4),观察者通常每 tick 每队一条 | 每名队员每条上报一次扇出 |
| 内存 | 每支队伍一条有界轨迹(几十个点) | 无 |

要用 `stress_summarize.ps1` 的对比表说话(AGENTS §6),压测要覆盖"全员组队行走"与"队长原地来回点"两种负载。

## 7. 参数(`team_follow_formation.h` / `team_follow_system.cpp`)

| 常量 | 值 | 含义 |
|---|---|---|
| `kTeamFollowSpacing` | 1.4 米 | 槽位间距(沿路)。沿用客户端跟随的值,2026-11-12 定案保留(team-system.md V.7 第 5 条):紧凑纵队,整列 4 × 1.4 = 5.6 在视野半径 10 以内,队员之间不靠显式关注就互相看得见;贴图重叠由客户端按脚底先后排序。`player_team.cpp` 顶部两条 static_assert 守着"整列不超过归队阈值 8、归队阈值小于视野半径 10";按美术身宽拉开时这两条会先报错,要同时给队员之间加 AOI 显式关注(`AoiFollowKind::kTeam` 已预留)并核对 `kTeamFollowMaxLag` |
| `kTeamFollowCatchUpSpeed` | 10.8 米/秒 | 追赶速度上限 |
| `kTeamFollowMaxLag` | 24 米 | 暂停中的队员最多为他保留的轨迹;超过就恢复后直接瞬移进槽位 |
| `kTrailRewindRadius` / `kTrailRewindMinGain` / `kTrailRewindSearchLength` | 0.6 / 0.05 / 4 米 | 回收判定 |
| `kTrailKeepBehindHead` | 7 米 | 队长身后至少保留的历史 |
| `kTeamFollowSnapToTrailRadius` | 0.75 米 | 加入时就地接上的距离 |
| `kTeamFollowMaxApproachPath` | 16 米 | 加入时最长走多远,再远就瞬移 |
| `kTeamFollowMaxLeaderJump` | 4 米 | 队长一 tick 挪这么远视为不连续 |

## 8. 验证(未编译、未测试,待 Codex)

- 纯逻辑单测 `cpp/tests/aoi_test/team_follow_formation_test.cpp`(进 `aoi_test`):直走同速同距、同 tick 停步与起步、
  拐角不抄近路、沿路来回不动且轨迹不变长、横向来回只带动一次、真掉头后顺序与间距、加入走到队尾、比槽位近时原地等、
  离队补位、暂停与落后放置、名次按入队序号、长距离行走轨迹有界、回收捷径不可走时不剪、回收不越过保护里程、
  插值 / 最近点 / 剪尾 / 前接、广播节流。
- Go:`go/match/internal/team/service_test.go` `TestLeaderChangeRefreshesEveryRemainingMember`。
- ECS 层与跟随链(`cpp/tests/cross_zone_test/player_team_follow_test.cpp`):
  `TeamFollowDriveTest.*`(离线队员照样跟、战斗中原地等并在战后赶回槽位、暂离立刻停止驱动并清速度、队长离队后下一 tick 摘掉队员)、
  `PlayerTeamFollowTest.OfflineMemberWhoDeclaredFollowIsAttachedInTheSameScene`、`OfflineMemberDoesNotAskSceneManagerToChangeScene`;
  原有的 `DisconnectedWhileLocationPendingPreventsFollow`(没声明跟随的离线队员不被瞬移)行为不变。
  2026-11-10 补 `TeamFollowDriveTest.ResendAfterEntryLeavesTheFormationUntouched`(补推不改动队形与驱动);
  补推本身发没发出去要截获发给客户端的消息,这个测试工程没有假网关,留给联机验收。
- 协议、客户端联动没有自动化测试;需要新客户端后联机验收。联机验收要覆盖:开通挂机的队员杀掉客户端后照样跟着队长走,
  再登录接回时客户端立刻显示"正在跟随"、自己的点击只出提示。
- 2026-11-10 跨场景跟随(§3.7),未编译、未测试:
  - `cpp/tests/cross_zone_test/player_team_follow_test.cpp`:`PlayerTeamFollowTest.LeaderOnAnotherNodeOfTheZoneIsFollowedBySceneChange`
    (在线、带 gate)、`HostedOfflineMemberFollowsWithAHostedPlacement`(请求字段:托管落点、不带 gate、断开那次登录的身份)、
    `OfflineMemberWithoutConfirmedHostingDoesNotChangeScene`;`TeamFollowDriveTest.LeaderLeavingTheNodeIsAwaitedAndRecheckedOnceItIsGone`、
    `AbortedLeaderHandoffSettlesTheWaitingFollower`、`WaitingFollowerWhoLeftFollowIsNotRechecked`、`WaitingForTheLeaderGivesUpAfterTheTimeout`。
  - 改了一条既有用例:`MissingMalformedOrRemoteLeaderLocationDoesNotMovePlayer` 去掉"队长在本 zone 别的节点"那一项(现在要跟),
    改由上面第一条新用例覆盖;别的 zone 那一项保留。
  - 托管落点本身(scene_manager 与目标节点)的用例见 player-connection-lost-hold.md §10 第 6 条。
  - 联机验收:队员开挂机卡后杀掉客户端,队长穿过传送点换图(同节点、跨节点各一次),队员跟过去并继续沿路跟;步骤见 `docs/PROGRESS.md` 同日条目。
  - 2026-11-11 托管中的兜底重试:`cross_zone_test.cpp` 的 `ConnectionLostHoldAfkPass.HostedFollowRecheckIsSpreadOverThePeriod`(纯判定:
    每名玩家每个周期恰好一秒到期、同一秒的到期按玩家号错开)、`player_team_follow_test.cpp` 的
    `TeamFollowDriveTest.OnlyADetachedMemberWhoWantsToFollowNeedsARecheck`(判据 `NeedsFollowRecheck`:已接在队长身后、不在队伍里、
    暂离的都不重试);真发出去的组队读要组队权威,单测宿主没有,留给联机验收。
- 2026-11-12 朝向口径与合并广播(§4),未编译、未测试:
  - `cpp/tests/aoi_delivery_test/aoi_delivery_test.cpp`:`FacingTest.YawIsTheGroundAzimuthInDegrees`、
    `AoiDeliveryTest.LookAtPositionTurnsTheYawInDegreesAndMarksTheTransformDirty`、`AoiDeliveryTest.LookAtPositionStraightAboveKeepsTheFacing`;
  - `cpp/tests/aoi_test/aoi_system_test.cpp`:`MoveReceiverGroupingTest.*` 4 条(视野相同的一组一条、视野不同分组、重复接收者与无人接收、满批 64 位);
  - `player_team_follow_test.cpp`:`TeamFollowDriveTest.DrivenFollowerFacesItsWalkingDirectionInDegrees`(拐弯后偏航 90 度,不是 π/2)。
  - 合并批的发送胶水(按组选 133 / 135、每组一次广播)没有单测:能截获客户端消息的只有 `aoi_delivery_test`,它不编 `movement.cpp`,
    编进去会连带拉进大半个 scene 库、可能与假发送函数重复定义,不编译没法排除。留给联机验收:3 人以上的队伍同时起步,
    旁观者与队员各收到一条 135、列表里是全部在动的队员(本人那条也在),2 人队伍仍是 133。

- 2026-11-12 排空 / 疏散改派托管队员与托管落点的看护(§3.7 末条),未编译、未测试:用例列在 player-connection-lost-hold.md §10 第 7 条
  (`RelocateConfirmEcs.Hosted*`、`RelocateConfirmHosted.*`、`HostedPlacementRetry*`,Go 的 `TestEnterScene_HostedPlacementEvacuationLandsOnTheResolvedTarget`);
  联机验收(托管队员所在频道被排空后被改派到别的频道、继续跟随队长)见 `docs/PROGRESS.md` 同日条目。

## 9. 没做 / 已知边界

- **客户端**:§5 已写完(代码已入客户端 main,11-11 的用例与说明未提交),未编译、未在 Unity 里运行;编译、EditMode 与冒烟交 Codex。
  在新客户端发 `TeamFollowC2S` 之前,本改动对线上行为没有影响。
- **Java 版(AGENTS §12)**:待做,已登记 `PARITY.md`。需要对齐的客户端可见行为:`TeamFollowC2S` / `TeamFollowStateS2C`、
  服务器驱动队员沿队长轨迹走位并广播(含发给本人)、被驱动期间丢弃移动上报、队长换人时给全员发刷新、
  入场之后对仍在队形里的队员补推 `driven=true`。2026-11-10 补一行待做:队员跨节点跟随、离线托管的队员跨场景跟随(§3.7),本轮不写 Java 代码。
- 跨 zone 仍不跟(team-system.md DV-6 的同 zone 那一半不变);跨节点 2026-11-10 起跟(§3.7)。
- 没开通挂机的离线队员不跨场景跟随,断线保留到期(25 秒)就退出;开通了的跨场景、跨节点照样跟(§3.7),托管到月卡到期。
- 托管落点提交之后目标节点没把人建起来:2026-11-12 起目标节点按退避有限次重试(player-connection-lost-hold.md §4.4)。仍不补的两类 ——
  事件在目标节点消费之前丢了(节点崩溃 / 被同号新进程顶替)、提交之后目标场景被销毁 —— 不丢数据,但他不在世界里,直到重新登录或月卡
  到期后租约清理(同文 §8 第 9 条)。节点排空 / 疏散 2026-11-12 起把托管队员搬走、接着跟(§3.7 末条)。
- 原随从的唤醒只覆盖"因队长离开本节点而解散"的那一批;那一刻不在队形里的离线托管队员(例如正在战斗)靠战斗结束时的跟随检查,
  其余漏掉的由托管中的兜底重试(§3.7,每 15 秒)补上,最多晚一个周期(扫描偶尔跳过一秒时晚两个周期)。
- 掉头时队员互相穿过;绕比间距大的圈时队员跟着绕(§3.3)。
