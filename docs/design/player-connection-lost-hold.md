# 玩家断线保留与短重连回原处

> 2026-11-05,Claude。代码已随用户的快照提交入库(`0f484db0d`、`6452b949d` 等),协议生成物里已有 `ExitGameReason`。**未编译、未测试**,验证步骤见 §10。Java 版待做,见 §9。
> 2026-11-06 补:回原场景排除回收中的频道(§6 第一段),新增用例 `TestEnterScene_ZeroZoneReconnectSkipsDrainingChannel`。同样未编译。
> 2026-11-06 再补(未编译):开通挂机(挂机月卡有效)的玩家断线后离线托管,不按 25 秒退出,保留到重连、月卡到期或被别的退出取代(§4.1);保留中的队员照常被服务器驱动的组队跟随带着走(§4),托管开始时由服务器开启跟随。
> 2026-11-06 修正,两处判据都改为比登录身份。11-08 Codex 跑整套 `cross_zone_test`,458 项全部 PASS,含下面新增的 5 条与按新签名改过的 2 条(§10 第 2 条);本地联调没做。
> - `EnterScene` 判「换会话」原先比会话快照,同一会话的重复路由也被拆掉重进(§5 第 3 步);
> - 接回判据 `IsResumedBy` 原先只比会话号,gate 以同一 node_id 重启后拿回旧号的新登录摘不掉保留(§5 第 0.1 步)。
>
> 新增用例 `ConnectionLostHoldReentry.*` 与 `ConnectionLostHold.SameSessionNumberResumesOnlyWithNewerLogin`。
>
> 2026-11-10 补(**未编译、未测试**):离线托管的角色跟着队长换场景(同节点 / 本 zone 跨节点)—— 托管落点(§4.2):scene 发不带 gate 的
> EnterScene,scene_manager 提交后直接给目标 scene 节点发 `PlayerHostedPlacementEvent`,目标节点不绑会话入场、按断线托管重建保留。
> 新增协议字段与 Kafka 事件,要先跑协议生成;验证见 §10 第 6 条。
>
> 2026-11-12 补(**未编译、未测试**),把 §8 第 8、9 两条残余修掉:节点排空 / 疏散时离线托管中的角色按托管落点改派到别的节点、接着托管,
> 走与在线玩家同一条改派链与待确认表(§4.3);托管落点提交之后目标节点没把人建起来时,目标节点按退避有限次重试同一条落点(§4.4)。
> 没有新增协议字段;验证见 §10 第 7 条。

## 0. 一句话

玩家连接意外断开时,角色在原地保留 25 秒;30 秒断线租约内重连的新会话直接接回同一个实体、回到原来的场景,并拿到完整入场画面。超时才存盘下线,与原来断线即下线走同一条路。

服务器之间的断线重连不在本文:节点链路由 muduo `enableRetry` 自动重连,每次建连做一次双向身份握手后置 Ready(`muduo-rpc-channel.md` §8.3);节点链路的应用层心跳与半开检测见同一文档的新增一节。两者都不做重连后的业务逻辑(负责人 2026-11-05 定)。

## 1. 改之前

- 客户端断线后,gate 的 `HandleConnectionDisconnection` 立即向 scene 代发 ExitGame。scene 把角色摘出场景和 AOI、存盘、销毁实体。login 同时把会话置成 DISCONNECTING,租约 30 秒。
- 30 秒保住的只是「重连资格」,角色本身已经没了:
  - 重连要从库里重新加载;
  - 别的玩家看到角色瞬间消失;
  - 运行时状态(实时技能 CD、Buff)丢失;
  - gate 滚动重启时,整批玩家存盘,随后又整批加载。
- 落点也不对。login 对 ShortReconnect / ReplaceLogin 发 `ZoneId = 0` 的 EnterScene,把去向交给 scene_manager。scene_manager 只跟随了 location 的 zone,场景仍按默认大世界最空的频道重新挑。在副本、非默认地图或别的频道的玩家,重连后会被换走(`disconnect-reconnect-audit-20261020.md` §2.2)。这与 GO-5「重连窗口内回到原处」、login 注释「顶号接管角色当前所在的位置」不符。

## 2. 目标与非目标

目标:

- 意外断线不立刻下线;窗口内重连接回同一个实体、同一个场景。
- 窗口外的行为与原来相同:存盘、写断线释放标记、租约到期删 location。
- 不加「重连成功」的业务钩子。接回只是 EnterScene 的一次「换会话入场」,与 11-05 定的「连接层不做业务逻辑」一致。

非目标:

- 不补发断线期间的推送。仍按快照、查询、业务有主协议恢复(审计 §2.3)。
- 不做 gate 与客户端之间的应用层心跳(要新增客户端协议,见 §8 第 1 条)。
- 不改客户端,不改 AOI,不做 AI 控制。离线托管(11-06 补,§4.1)只让开通挂机的玩家角色常驻、照常被组队跟随带着走,不替他做别的事。

## 3. gate:断线时带上原因

`proto/scene/player_lifecycle.proto` 给 `GameNodeExitGameRequest` 加 `ExitGameReason reason = 1`。这是服务端内部字段,客户端不可见。gate 在 `NotifySceneExitGame` 里把原因序列化进请求体。

| 情形 | reason | scene 怎么做 |
|---|---|---|
| TCP 断开:断网、gate 停机、顶号时 login 踢旧连接、输出缓冲高水位强关 | `DISCONNECTED` | 断线保留(§4) |
| 客户端主动 LeaveGame(`gate_scene_entry::OnClientLeaveGame`) | `LOGOUT` | 立即退出 |
| 断线租约到期后,gate 关掉残留的假死连接(`PlayerLeaseExpiredEventHandler` 先置 `SessionInfo::exitSceneImmediatelyOnClose`) | `LEASE_EXPIRED` | 立即退出 |
| 旧版 gate,没有这个字段 | `UNSPECIFIED`(0) | 立即退出 |

scene 主动踢人(34)时,scene 自己先销毁实体,之后到达的 `DISCONNECTED` 找不到实体,什么都不发生。

顶号走保留是有意的:新会话随后就会接回同一个实体,比先退出再重载更快,也不丢运行时状态。

兼容:项目未上线,gate 与 scene 同批升级。即使混跑,两个方向都退回原来的行为:旧 scene 不认这个字段,按空请求立即退出;新 scene 收到旧 gate 的 0 也立即退出。

## 4. scene:断线保留

入口只有一个:`PlayerLifecycleSystem::HandleClientConnectionLost`,由 ExitGame handler 在 `reason == DISCONNECTED` 时调用。常量与纯判定在 `player/system/connection_lost_hold.h`。

判定 `connection_lost_hold::Decide`,按顺序:

1. 已在退出中 → 立即退出,交给退出流程合并原因。
2. 已在保留中 → 保持原来的计时,窗口不因重复通知延长。
3. 归属交接已发起、整节点疏散中、实体没有绑定会话 → 立即退出(原行为)。
4. 其余 → 保留。

保留时做这几件事:

1. 停步。与退出同一个函数 `StopMotionForExit`:清速度,并给观察者补一条停步广播,不然掉线时正在走的角色会一直走下去。
2. 摘掉已断开的会话(`RemovePlayerSession`):快照里的会话置无效,擦掉仍属于本玩家的 SessionMap 映射。登录身份组件不动。
3. 撤掉建镜像后的自动进场,与退出同口径。
4. 挂 `PlayerConnectionLostComp`,记下单调时钟时刻、断开的会话号和那次登录的代次(取登录身份组件,保留期间它不变)。
5. 请求一次关键进度存盘(有节流)。原来断线即存盘,这样落盘时机不比原来晚多少,节点崩溃时不多丢。
6. 确保 1 秒一次的到期扫描已经挂上。

为什么要摘会话:实体此刻确实没有活会话了。依赖「有没有活会话」的路径因此都自然按断线处理,不用改那些文件:

- 跨节点交接不会发起(`HasLiveGateSession`;例外是服务器替离线托管中的角色发起的交接,重发改为托管落点,§4.2);
- 跨场景的队伍跟随不带 gate 去切(`player_team.cpp` 换场景分支的 `HasLiveSession`):普通保留中的留在原场景,离线托管中的改发托管落点(§4.2);同一场景里由服务器驱动的跟随不看会话,照常带着它走,只是不给它发跟随推送([team-follow-server-trail.md](team-follow-server-trail.md) §3.6,`team_follow_system.cpp` 的 `HasLiveGateSession`);
- 排空、疏散对普通保留中的角色只存盘不改派(`EnqueueRelocateTicket`);离线托管中的另发托管票据,按托管落点改派到别的节点(§4.3);
- 推送不再发往 gate。

登录身份组件留着,是因为租约到期的会话结束释放要按它判断「实体是不是还绑着这次登录」(`location-release-binding.md` §13)。

保留期间:

- 角色仍在场景与 AOI 里,别的玩家看得到它站在原地;正被组队跟随驱动的队员例外,照常跟着队长走(进保留时那一次停步之后,下一 tick 就接着走)。
- 周期存盘照常;战斗照常推进(回合超时、自动行动);资产指令照常应用到在场的实体上。

到期:`ExpireConnectionLostHolds` 每秒扫描一次。保留满 25 秒的,先摘掉保留组件,再 `HandleExitGameNode(kClientDisconnect)`,与原来断线即退出是同一条路。

被别的退出取代(租约到期的会话结束释放、排空、疏散、停机、交易封存):`HandleExitGameNode` 入口摘掉保留组件。首次挂退出意图时,`clientDisconnected` 置位,在线票据因此作废(普通保留中的角色本来就没有票据)。离线托管中的角色被排空 / 疏散时拿的是托管票据,派发按登录身份判、不看这一位,搬到别的节点接着托管(§4.3)。

`PlayerConnectionLostComp` 属于「按需状态」(`ecs-component-access-rules.md`):没有它 = 连接正常或不在保留中。它是纯内存组件,不落库。

### 4.1 开通挂机的玩家:离线托管(2026-11-06,用户要求「队员客户端离线,但是开通挂机应该照样跟」)

"开通挂机"在代码里就是挂机月卡:玩家域 Redis 键 `player:afk_pass:{<player_id>}`,值为到期的 Unix 秒,由购买 / 开通流程写入
(与 `go/shared/redisslot.AfkPassKey` 同形)。player_locator 的租约监视在月卡有效期间每 5 分钟给断线中的会话续一次租约
(`go/player_locator/internal/logic/leasemonitor.go` 的 `afkPassState`,查不出来时推迟 10 秒再判);以前 scene 不看月卡,
角色照样 25 秒后退出,只保住了重连资格。现在:

- **进入保留时查一次月卡**(`AfkHostingSystem::Probe`,`player/system/afk_hosting.{h,cpp}`):异步 GET,回调按玩家号重新找实体,
  只认同一次保留(`lostAt` 相同)的结果;接回、退出之后或下一次断线到达的迟到结果直接丢弃。
- **判定**(纯函数 `connection_lost_hold::JudgeHold` / `ParseAfkPass`,`ExpireConnectionLostHolds` 每秒调一次):
  - 月卡有效且离到期还超过 `kHostingExitLead`(扫描周期 1 秒 + 退出存盘余量 4 秒)→ 继续保留,不受 25 秒窗口约束。
  - 快到期 → 再查一次:续了就按新的到期时刻继续托管;没续(键没了、到期了、剩下不到余量)→ 下一次扫描按客户端断线退出,
    与普通保留到期同一条收尾(存盘、写断线释放标记、销毁)。
  - 查询失败 → 状态不变。还不知道有没有月卡的,25 秒窗口照常计、窗口内每次扫描重查,窗口到了仍查不出来就照原来退出(不丢数据);
    托管中的,快到期时查不出来先继续托管,已知的到期时刻过去 `kHostingUnconfirmedGrace`(30 秒)仍确认不了续费,按到期退出。
  - 值解析不了(不是十进制非负整数)按没有月卡,与 player_locator 同口径,并打一行日志。
- **托管开始**(第一次确认月卡有效):调 `PlayerTeamSystem::OnOfflineHostingStarted`,在队伍里就由服务器开启组队跟随,
  不等客户端声明;下线前点过暂离的也照样跟,重连后客户端按自己当前的选择重新声明([team-follow-server-trail.md](team-follow-server-trail.md) §3.6、§5)。
- **托管期间**:与普通保留相同 —— 没有活会话、周期存盘照常、战斗照常推进(自动行动)、被别的退出取代时照常收尾;
  另外,同一场景里正被组队跟随驱动的,照常跟着队长走;队长换场景、换节点时跟过去(§4.2)。在队伍里却没接在队长身后的,
  1 秒扫描每 15 秒(按玩家号错开)替他重新走一遍跟随检查(`PlayerTeamSystem::OnOfflineHostingHeartbeat`,2026-11-11),
  相当于在线客户端没被驱动时隔几秒重新声明一次跟随,补上漏掉的信号(team-follow-server-trail.md §3.7)。
- **结束**:新会话接回(§5,与普通保留同一判据)、月卡到期或确认不了(上面)、被别的退出取代(租约到期的会话结束释放、停机、交易封存)。
  排空 / 疏散不算结束:他拿着托管票据存盘退出,按托管落点搬到别的节点,在那里按断线托管重建保留、接着托管(§4.3)。

时序为什么成立:普通保留靠 `kHoldWindow + 1 + 4 ≤ 30` 保证保留在 login 断线租约到期前结束(§7)。托管超过 30 秒,是因为 player_locator
在月卡有效期间一直续租;它只在看到月卡失效(到期时刻之后)才让租约到期,而 scene 在到期前 `kHostingExitLead` 就退出,
租约到期时退出存盘已经落地、断线释放标记已写,LeaveScene 照常当场拿到证据。两台机器的墙钟差超过这 5 秒时,可能走到
「保留拖过租约」的慢路径(§7),只是清理多等一个 claim 超时,不影响数据。

没做:托管中的角色不会自己去别处(不打怪、不自己换图;跟着队长换场景见 §4.2)。节点排空 / 疏散时,2026-11-12 起他不再存盘下线,
而是按 §4.2 同一套托管落点改派到别的节点、到了接着托管(§4.3)。

### 4.2 离线托管的角色换场景:托管落点(2026-11-10,用户要求「队员开了离线挂机卡,然后完全杀掉客户端」也要跟着队长过去)

> 代码已写,**未编译、未测试**(验证见 §10 第 6 条)。跟随侧的规则(谁替他发、什么时候发)在 [team-follow-server-trail.md](team-follow-server-trail.md) §3.7,
> 这里讲"没有客户端的角色怎么换场景"本身。

**问题**:同一场景里,离线托管的队员靠服务器驱动走位,不需要客户端(§4.1)。换场景不一样:普通换场景的提交点是 scene_manager 推给 gate 的
路由事件(RoutePlayerEvent → gate → 目标节点 PlayerEnterGameNode),离线的角色没有 gate 会话,这条路走不通;scene_manager 接受不带
`GateId` 的 EnterScene,但第 6 步落点成功就算提交、不发任何路由,没有人通知 scene 去挪这个实体。

**做法:托管落点** —— 把"推 gate 路由"换成"直接推给目标 scene 节点",别的一概不变。

1. **请求**(scene → scene_manager):`EnterSceneRequest.hosted_placement = true`,不带 `gate_id` / `gate_instance_id`;`session_id` 与
   `login_generation` 填**断开的那次登录**(`PlayerConnectionLostComp` 里记的会话号与代次,也就是 location 里记的落点会话身份)。
   只有离线托管中的角色可以发(`PlayerLifecycleSystem::OfflineHostedIdentity`:断线保留 + 挂机月卡确认有效且离到期还远 + 不在退出中,
   判据 `connection_lost_hold::IsHosting` 与 `JudgeHold` 判"托管继续"同一个条件)。普通 25 秒保留不搬:那个窗口建立在 login 断线租约
   30 秒之上(§7),搬到别的节点会在那里重新开一个窗口、拖过租约;托管期间 player_locator 一直续租,没有这个顾虑。
2. **scene_manager**(`go/scene_manager/internal/logic/enter_hosted_placement.go`,`enterscenelogic.go` 只有几处接线):
   - 预检,在任何去重、预占与写之前:带了 gate、缺登录身份 → 5;跨 zone → 5(没有 gate 可重定向,第二条腿也没有客户端去连);
     目标节点在本副本的节点镜像里没有唯一的实例号 → 4(投递不了防僵尸的事件,可重试)。拒绝原因记 `enter_scene_rejected_total{reason="hosted_placement_*"}`。
   - 其余与普通换场景完全相同:落点会话身份裁决、换手门(跨节点要源节点为当前代写的落盘标记,没有就回 18)、铸造(跨节点铸新代,同节点不铸)、
     CAS 落点、3b 归属查询(目标节点建实体要 home_zone,与 gate 路由下发的是同一个值)。
   - **提交点**:第 7 步(以及同落点的第 4 步)不推 gate 路由,改推 `PlayerHostedPlacementEvent`(`proto/common/event/player_placement_event.proto`)
     给目标 scene 节点:`SceneCommand{DispatchEvent}` 发到 `scene-cmd_g<N>`、分区 = 目标节点号 % P、key = player_id、`target_instance_id` = 目标节点实例号。
     broker ACK 是提交点;失败按推 gate 路由失败同一套回滚(location 退回、epoch 只进不退、人数还回去,回 7)。同落点(location 已经指着这个场景,
     通常是上一条事件还没被消费)照样再发一次,失败只回 7、不回滚。
   - 第 7b 步跨节点时给源节点派 ReleasePlayer,不变(源节点交接在途,忽略它,由交接应答收尾)。
3. **目标节点**(`PlayerPlacementEventHandler` → `PlayerLifecycleSystem::HandleHostedPlacement`):事件是 gate 路由在"没有 gate 会话"时的等价物,
   字段与 `PlayerEnterGameNodeRequest` 一一对应。组一份 `PlayerEnterContext{hostedPlacement = true}`,走与 gate 路由**同一条入场链**:
   `RoleAdmissionSystem::RequestEnter`(全局 SQL 角色登记 + `handoff_fence` 交接栅栏)→ `HandleEnterGameNode`(入场会话栅栏、复用实体或认领载入)
   → `EnterScene`。`hostedPlacement` 只改三件事:
   - 会话号照常当**登录身份**用(角色登记原件、入场栅栏、`PlayerLoginIdentityComp`、断线保留的接回判据),但**不绑** SessionMap、快照会话保持无效、
     **不推**任何提示(`PlayerEnterContext::NotifiableSessionId` 为 0);
   - 入场完成后(`EnterScene` 第 3.0 步 `EnterHostedHold`)按断线托管重建保留:同节点换图的实体本来就在保留中,原样留着(计时与月卡状态不重来);
     从别的节点搬来的新实体挂 `PlayerConnectionLostComp{now, 断开的会话, 代次}` 并查一次月卡 —— 确认有效时算托管开始,由服务器开启组队跟随
     (`OnOfflineHostingStarted`),接到队长身后;
   - 不取消退出:角色已在退出中(托管到期、被别的退出取代)时这条落点来晚了,让退出照常收尾(重连能取消退出,是因为有一条活着的新会话要接回)。
4. **源节点,同节点**:scene_manager 不铸代,location 指到新场景,事件发回本节点,`HandleEnterGameNode` 复用实体、`EnterScene` 就地换场景。
   EnterScene 的应答对跟随只记日志。
5. **源节点,跨节点**:第一跳拿 18,与玩家自己换图到别的节点**同一条交接链**:`StartTravelHandoff(..., TravelInitiator::kServer)` 冻结、存盘、
   写落盘标记,`PublishAndEnterTravel` 重发 —— 没有活会话时重发托管落点;scene_manager 凭标记铸新代、提交、给目标节点发事件;源节点收到成功应答后
   `ResolveTravelOutcome` 原子取证,判已放行就不存盘销毁(`DestroyDeposedPlayer`)。服务器发起的交接未成只记日志、不回 tip(`PlayerTravelHandoffComp.serverInitiated`)。

**不变量怎么保住**

| 不变量 | 机制(都是既有的,托管落点没有另开) |
|---|---|
| 不丢档 | 跨节点:写落盘标记之前源节点已把冻结那一刻的状态落盘(骨架 I1);scene_manager 只凭当前代的标记放行;目标节点认领载入的就是那份。同节点:实体不动,只换场景 |
| 不双活 | 源节点从冻结起不再改状态,放行后不存盘销毁;scene_manager 铸新代,目标节点以新代认领 TiDB 写权行,源节点迟到的写被写权行拒(STALE_OWNER);一次只有一个提交点(broker ACK) |
| 归属 epoch | 跨节点铸新代(凭标记的铸造),同节点不铸;事件带落点那一刻的代次,目标节点入场时按它认领 / 更新缓存(与 gate 路由同口径) |
| 交接栅栏 | 目标节点照常在 `RequestEnter` 里登记 `handoff_fence`(`kRegisterFenceLua` 要求 owner_epoch 等于事件的代次),载入按它认领 |
| 角色登记 | 目标节点照常在全局 SQL 登记入场原件(会话号与代次是断开的那次登录,`ValidAdmission` 要求非 0);源节点的登记随实体销毁按既有规则关闭。组队读(`ReadTeamAdmission`)认的就是这份原件,托管中照样读得到 |
| 登录身份 | location、目标节点的 `PlayerLoginIdentityComp` 与 `PlayerConnectionLostComp` 都是断开的那次登录:之后的重新登录按 §5 接回;租约到期的会话结束释放按 `location-release-binding.md` §13 比对身份,照常命中目标节点上的实体 |
| Kafka 防僵尸 / 保序 | `target_instance_id` = 目标节点实例号(节点镜像里拿不到唯一实例号就在写之前拒);key = player_id |

**失败、超时、重复、回滚**

| 情形 | 结果 |
|---|---|
| scene_manager 预检拒绝(4 / 5)、身份裁决拒绝(25)、换手门第一跳(18 以外) | 一个字节都没改;跟随只记日志,下一次跟随检查再试 |
| 推事件失败(broker 没 ACK) | 回滚落点,回 7;同节点:什么都没动;跨节点:源节点按既有取证(B4 / B5)解冻或采纳回滚后的代次,静默 |
| EnterScene 传输失败(结果未知) | 同节点:若已提交,事件照样会到,实体照样换过去;跨节点:交给交接的应答看门狗与冻结上限裁决(与玩家自己换图相同) |
| 事件重复 / 迟到 | 入场会话栅栏按登录代次挡住被新登录取代的旧事件;角色登记对在途的同一份上下文只重放;已在目标场景时幂等;实体在退出中不拉回 |
| 目标节点角色登记被拒、交接栅栏登记失败、载入 / 认领失败、继承标记清理放弃 | 目标节点按退避有限次重试同一条落点,每次先读一次权威落点、确认仍指着本节点才试(§4.4);用尽仍没建出来的,玩家留在 location 指向的落点上无人持有,数据在盘上:重新登录照常从盘上载入,不再登录的,月卡到期后 player_locator 停止续租,租约到期走会话结束释放清掉 location |
| 事件在消费前丢了(目标节点崩溃 / 被同号新进程顶替,实例号不对的事件被防僵尸过滤丢掉) | 与"托管中的角色随节点崩溃一起不在世界里"同一类,不补:数据在盘上,下次登录按死节点 / 被顶替节点的接管落点照常载入;不再登录的由月卡到期后的租约清理收尾(§8 第 9 条) |
| 托管落点到了、月卡刚好到期 | 目标节点查月卡不成立,按普通 25 秒窗口退出(租约仍有续租余量,§7 的时序不受影响) |
| 交接中重新登录 | 新登录的 EnterScene 抢先:location 记成新身份,在途的托管落点被 25 拒,源节点按 B4 静默解冻;托管落点抢先:新登录按新 location 派到目标节点,与目标节点的托管载入谁先谁后都由入场栅栏按代次收敛到新登录 |
| 托管落点撞上仍有活会话的实体(按构造走不到) | `HandleEnterGameNode` 在摘会话之前直接拒,不碰会话 |

### 4.3 节点排空 / 疏散时搬走离线托管的角色(2026-11-12)

> 代码已写,**未编译、未测试**(验证见 §10 第 7 条)。取代 11-11 计划排空批 5 的写法(scene-node-planned-drain.md §4.4):那一版派发时
> 去读已被退出入口摘掉的保留组件,每张托管票据都被作废、一个都没搬走过;票据也不进待确认表。

**问题**:单场景排空(频道缩容、迁移拆老实体、归属对账,`BeginSceneDrain`)、计划排空(`PumpPlannedEvacuations`)与整节点疏散
(身份冲突,`BeginEmergencyRelocateAll`)都按"抄票据 → 存盘 → 落地后改派"把住户搬走。改派票据原先只发给有活 gate 会话的人;
离线托管中的角色没有活会话,只存盘下线,托管提前结束、组队跟随随之断掉 —— 他要等下一次登录才回到世界里,而开挂机月卡
就是为了不登录。

**做法:与在线玩家同一条改派链、同一张待确认表,只把路由换成托管落点。**

| 步骤 | 在线玩家 | 离线托管中的角色 |
|---|---|---|
| 发票(`EnqueueRelocateTicket`) | 有活会话 → 抄会话、gate 号与实例号、去向提示 | `OfflineHostedIdentity` 有值 → 抄断开那次登录(会话号 + 代次)与去向提示,不抄 gate;已有一张前一次排空发的托管票据(那次退出已摘掉保留)→ 留着照样算。普通 25 秒保留(月卡还没确认)只存盘,理由同 §4.2 第 1 条 |
| 退出(`HandleExitGameNode`) | 存盘,落地后收尾 | 同左;保留组件在入口摘掉(本节点上的托管到此为止,到目标节点重建),计 `hosted_relocating`,不算"被取代" |
| 派发判定(`DispatchEmergencyRelocate`) | `DecideTicket`:退出期间客户端断线、或当前会话换了 → 作废 | `DecideHostedTicket`:断线是前提(保留中的退出意图一定记了 `clientDisconnected`),不看它;实体又绑上了活会话(新登录接回过)、或登录身份已不是票据那一次 → 作废(`void_session_replaced`) |
| 标记 | 条件写 "E:ms"(owner_epoch 仍是 E、只消费实体缓存的 H) | 同左 |
| A1′ | 消费了票据 → 不写断线释放标记(标记归改派);作废 → 按第一次退出原因判 | 同左 |
| 请求 | `evacuation`、去向提示、gate | `evacuation`、去向提示、`hosted_placement`、不带 gate,会话号与代次 = 断开那次登录 |
| scene_manager | 疏散解析 → 换手门 → 铸代 → CAS → 推 gate 路由 → 派 ReleasePlayer | 同左,只把推 gate 路由换成推 `PlayerHostedPlacementEvent`(§4.2 第 2 步);Go 侧没有为这个组合另写代码 |
| 待确认表 | 登记条目,应答 / 传输失败 / 落回本节点推动,核实后定案 | 同一张表,条目带 `hosted`,三处不同见下 |
| 到达 | gate 改绑,目标节点入场 | 目标节点按托管落点入场、不绑会话,按断线托管重建保留、查月卡,托管开始时由服务器开启组队跟随(§4.2 第 3 步);没建起来时目标节点自己补救(§4.4) |

**待确认表对托管条目的三处不同**(契约在 `relocate_confirm.h` 文件头):

1. **"踢线"不推送**(`push=no_client`):没有在场的客户端;条目里的会话号是断开那次登录的,可能已被同一玩家的新登录拿回
   (gate 以同一 node_id 重启后会话序号从头发),推过去会踢掉一条合法会话。凭证判定与补写照常,让他下一次登录到别的节点时
   过得了换手门。
2. **按登录身份认人**:落地判定("实体已建出且是同一次登录")与踢线前的本地复核比 `PlayerLoginIdentityComp`(会话号 + 代次)
   而不是快照会话 —— 托管落点不绑会话,快照会话恒无效;实体绑着活会话说明新登录接回过,不再是那一次(`RelocateEntryMatchesEntity`)。
   托管落点回到本节点时按它带来的会话号对账(`HandleEnterGameNode` 第 1 步):同一次登录 → 落地等载入;别的 → 已被取代,不踢。
3. **有界重发**:在线条目没生效就踢线,客户端被踢后自己重登,那就是重试;托管条目没有客户端,结清之后他要等下一次登录。
   所以核实读到"仍在本节点的源场景"、而这次改派的结局证明 scene_manager 没有放行他时,先不结清,按 5 秒、20 秒各重发一次
   (`relocate_confirm::ShouldResendHosted`、`kHostedResendDelays`):
   - 证明"没放行"的证据:没发出去;被拒且拒绝码可重试(1 没有去处、4 目标实例号未同步、8 Redis、17 再入屏障、20 归属不可用,
     都早于任何落点写,`PlayerLifecycleSystem::SmRejectionRetryableForHostedRelocate`);等不到完成通知或传输失败(走到"读到没变、
     许踢"时已过 settle,scene_manager 不会再处理那一条)。
   - 不重发的:成功应答(放行过,读到没变只可能是原样放回源场景,托管落点会送回本节点);5 / 18 / 25(形状不对、换手门不认这份
     标记、会话已被取代,重发也一样);落地两类(落点已提交到本节点,补救归 §4.4 的看护);location 已不存在(租约清理 / 登出)。
   - 两步:读到"没变"先定下、等一段(条目留在核实阶段,截止顺延);到点的那次核实仍读到"没变"才真发,发之前总有一次刚读到的
     权威读数。重发沿用派发时写的标记(两次等待加各自的完成通知预算仍在 300 秒 TTL 内,单测钉着),不新写任何键;条目回到
     `kMarkWriting` 后照派发路径发,关联号与 settle 重新起算,计 `hosted_resent`。
   - 用尽或不重发 → 结清、不推送。他不在世界里,数据在盘上:下次登录照常载入;不再登录的,月卡到期后 player_locator 停止续租,
     租约到期走会话结束释放清掉 location。

**为什么进表而不是发完即忘**:不进表就没人看应答,scene_manager 的拒绝(排空时最常见的是暂时没有去处)只能静默丢下;停机谓词
(`IsEmergencyRelocateDrained` / `IsPlannedDrainSettled` / 停机 drain 的 `relocate_confirms`)也不会等它的应答。进表之后这三个谓词都不用改:
票据、在写的标记、待确认条目本来就在谓词里。托管条目的重发最多把条目的寿命延长约 2 分钟(两次等待加各自的完成通知预算),
受各自的看门狗约束。

**不变量**:与在线玩家的改派完全相同,托管落点那一段同 §4.2 的不变量表。另补两条:
- 托管票据只在实体仍是那一次登录、没有活会话时派发:新登录接回过的,或身份被更新的登录取代的,都不会被替旧身份搬走。
- 重发只在读到"仍在源场景"之后发生:位置被任何请求改走(包括新登录)时读数就不是"没变",条目按"已在别处"结清。

**疏散中(身份冲突)**:确认阶段只读 Redis、不写玩家键的约束不变(凭证补写仍被身份闸挡住);重发是一次 EnterScene、由 scene_manager 写,
与疏散开始时那一轮改派同一类,受冲突 drain 看门狗约束。

### 4.4 托管落点提交之后目标节点没把人建起来:有界补救(2026-11-12)

> 代码已写,**未编译、未测试**(验证见 §10 第 7 条)。纯判定、预算与状态迁移在 `player/system/hosted_placement_retry.h`。

**问题**:托管落点的提交点是 scene_manager 推给目标节点的事件(broker ACK,§4.2 第 2 步)。提交之后 location 已指向目标节点、源节点
已销毁实体;目标节点的入场链 —— 全局 SQL 角色登记(15 秒内最多 3 次)→ 交接栅栏登记(要求 owner_epoch 等于落点代次)→ 认领载入
(30 秒)→ 继承标记清理(5 秒)→ EnterScene —— 任何一步失败,角色就不在任何节点上。在线玩家此时收到 kEnterSceneFailed、客户端
自己重登;托管中的角色没有客户端。这些失败多半是下游短暂不可用(data_service、zone Redis、TiDB),隔几秒就好。

**方案比较**:

| 方案 | 做法 | 问题 |
|---|---|---|
| A. 目标节点有界重试(采用) | 目标节点给到达的落点登记一条看护;入场链结束而没建出实体时按退避读一次权威落点,仍指着本节点就用同一条落点再走一遍入场链 | 目标节点崩溃时看护随进程消失 —— 与"托管中的角色随节点崩溃一起不在世界里"同一类,已接受 |
| B. scene_manager 对账 | 提交后记一条"待确认落点",定时核对目标节点有没有登记入场原件,没有就重发事件 | 要新增一份持久的待确认记录与扫描(还要选主或分片);入场原件在 data_service 的库里,scene_manager 去读就是跨库,得另开 RPC;它看不出失败是瞬时还是永久,只能盲重发 |
| C. player_locator 对账 | 租约监视顺带检查托管中的角色有没有持有者 | player_locator 管会话租约,不知道实体在哪个节点建没建成,得反问 scene 节点;职责错位 |
| D. 源节点确认 | 源节点等目标节点回执再结清 | 源节点放行后已销毁实体、不再持有任何状态(交接链的既有设计);组队跟随的跨节点交接根本没有源侧条目 |

选 A 的理由:失败发生在目标节点,只有它知道这一次入场链停在哪一步;重试走与第一次完全相同的入场链,既有栅栏(交接栅栏按代次登记、
继承标记清理按代次核对、TiDB 写权行按代次认领、入场会话栅栏按登录代次)把住一致性;不新增全局状态、不新增 RPC,状态只在目标节点
内存里、按到达的落点数线性增长,水平扩展不受影响。代价就是表里第一行那一条。

**做法**(契约写在 `hosted_placement_retry.h` 文件头):

1. **登记**:`HandleHostedPlacement` 收到字段齐全的事件、本节点此刻**没有**这名玩家的实体(跨节点搬来;改派落回本节点时实体也已销毁)
   时登记一条看护,再发起第一次入场链。同一玩家又来一条就整条替换(scene-cmd 按目标节点分区、key = player_id,后到的是更新的提交)。
   本节点有实体的是同节点换图:失败了他还在原场景里、照常托管,由组队跟随的兜底重试(team-follow-server-trail.md §3.7)再带他走,不登记。
2. **结清(成功)**:托管落点的载入建出实体时当场结清(`InitPlayerFromAllData`),不等下一拍 —— 刚建出的托管角色可能很快因月卡到期退出,
   扫描再看就会误判成"这一次没建成";每拍扫描时本节点已有他的实体(例如新登录的入场链建的)也结清。
3. **等**:入场链在途(`RoleAdmissionSystem::HasAdmissionInFlight`,或待入场表里有他)时不动,不区分是不是看护发起的那一次。
4. **退避与重试**:一次入场链结束而没建出实体 → 退避(5 秒 / 20 秒 / 60 秒,最多重试 3 次)→ 到点先读权威落点(只读脚本
   `relocate_confirm::kLuaReadPlacement`,同一个 slot 读回 owner_epoch 与 location;带 shebang 的脚本在只读副本 / 切主后的旧主上整体被拒,
   不会拿滞后读数去重试一条已被改走的落点)。全部成立才重试(`DecideAfterRead`):location 在本 zone、本节点、这个场景;location 的会话
   身份仍是这条落点的那一次登录;owner_epoch 仍是落点时的值;场景在本节点上还在。
   - location 不在了 / 在别处 / 身份变了 / 代次变了 → 结清(别人已经做了决定:登出或租约清理、别的落点、新登录、归属推进);
   - 判不清(location 解析失败、zone 为 0)→ 放弃;场景已不在本节点 → 放弃(重试也进不去,见残余);
   - 读不到(Redis 不可用、ERROR、形状不对),或读的时候恰有别的入场链在途 → 1 秒后再读,不盲试。

   重试 = 用同一条落点(同一代、同一身份、同一归属修订)再走一遍 `RoleAdmissionSystem::RequestEnter`。先读一次,是为了不在已经过时的
   落点上白白登记一份全局 SQL 角色原件;读完之后才变的,由上面那些栅栏拒掉。
5. **放弃**:重试用尽,或超出总预算(8 分钟:首次加三次重试各按入场链上限 90 秒算,再加 85 秒退避,`static_assert` 守着)→ ERROR。
   他不在世界里,数据在盘上:下次登录照常载入;不再登录的由月卡到期后的租约清理收尾。
6. **节点要走时不重试**:疏散中(身份冲突)、计划排空中、持久任务已停时不发起读与重试(新建的实体只会再被搬一次,或随停机退出),
   看护到总预算自然放弃。看护表不进任何停机谓词。
7. **线程模型与观测**:只在 scene 逻辑线程上;由 RedisSystem 的 1 秒定时器驱动(与改派待确认表同一个节拍),Redis 回调只按值捕获
   player_id 与读的代际;容量 4096,满了不登记、只打 ERROR。日志前缀 `[HostedPlacementRetry]`(watching / attempt ended / retrying /
   landed / resolved / gave up),每行带本线程的累计数。

**残余**(写清楚,不补):
- 事件在目标节点消费之前丢了(目标节点崩溃,或被同号新进程顶替、旧实例号的事件被防僵尸过滤丢掉):与"托管中的角色随节点崩溃一起
  不在世界里"同一类。下次登录时 scene_manager 按死节点 / 被顶替进程的接管规则落点,照常载入;不再登录的由租约清理收尾。看护期间
  目标节点崩溃同理。
- 落点提交之后、事件消费之前目标场景被销毁(`give_up_scene_gone`):目标节点在这一代没有登记过交接资格,写不出合法的落盘标记,
  替他重新问 scene_manager 时跨节点的落点过不了换手门;落点又只由 scene_manager 定、C++ 侧不复制选频道规则(scene-node-planned-drain.md §4.4)。
  场景销毁之前排空会先把住户改派走,这个窗口只剩事件在 Kafka 里的那一小段;数据在盘上,下次登录照常进默认去向。

## 5. 重连接回

1. login:租约内同账号重登判 ShortReconnect;别的在线会话判 ReplaceLogin。这部分不变。
2. scene_manager:按 §6 派回原场景,通常就是同落点重连,RoutePlayerEvent 发给新 gate。
3. scene 的 `HandleEnterGameNode` 复用实体,进 `EnterScene`:
   - 第 0.1 步 `ResumeConnectionLostHold`:只有新的一次登录才摘掉保留(`connection_lost_hold::IsResumedBy`),判据与第 3 步的「换会话」同一口径。
     - 会话号与断开的不同,算接回。
     - 会话号相同而请求的登录代次比断开时大,也算接回。gate 停机时先让在线角色都进保留,它以同一 node_id 起来后会话序号从头发,新登录可能恰好拿回断开的那个号。2026-11-06 之前只比会话号,这种新登录摘不掉保留:入场本身完整,但 25 秒后保留到期,正在玩的玩家被按断线退出。
     - 断开的那次登录迟到的进场(号相同、代次不大于断开时,或没带代次)、没有会话的进场,都不算接回。
   - 第 1 步绑定新会话、记登录身份。
   - 第 3 步按「换会话」先离场、再完整入场(审计 §9.1 的修复):新客户端收到入场通知、自身的创建通知和当前邻居。
     「换会话」比的是实体此前放行过的登录身份(`PlayerLoginIdentityComp`,与入场栅栏同一口径):会话号不同,或会话号相同而登录代次更新(gate 以同一 node_id 重启后会话序号从头发,新登录可能拿回旧号),都算换了;同一次登录的重复路由(gate 补发、owner_epoch / 归属修订变了的 0 型转发)不离场、不重发入场通知。2026-11-06 修正前比的是会话快照,而入口第 2 步与断线保留都已先把快照会话置无效,同一会话的重复路由也恒判换了:已在目标场景的角色被拆掉重进,客户端重收入场通知,邻居看到它先销毁再创建。
   - 之后战斗的 `BattleReconnectS2C`、落地恢复钩子照常执行。

保留到期之后、租约到期之前(25 到 30 秒)到达的重连,照常判 ShortReconnect。退出存盘还在途时,`EnterScene` 第 0 步取消退出、复用实体;实体已销毁时,按原来的路径从盘上重载。两种结局都正确,后一种慢一点。

## 6. scene_manager:回原场景

`go/scene_manager/internal/logic/enter_resume_scene.go` 的 `resumeSceneOf`,在 EnterScene 第 3 步解析场景之前调用。

条件(全部满足才回原场景):

- 请求没有任何去向:ZoneId、SceneId、SceneConfId 全为 0。只有 login 的 ShortReconnect / ReplaceLogin 这样发。
- location 落在本 zone 的具体节点上,并记着场景,不是跨 zone 传送途中的等待落点。

先排除回收中的频道:原场景带着缩容摘线时写的排空标记 `scene:{id}:draining` 时不回去,直接按默认大世界落点。排空自己就是把还在线上的人改派到默认大世界(`sweepDrainingWorldChannels` 每拍对还有人的频道发 DestroyScene,C++ 侧按 scene_id / scene_conf_id 为 0 改派),回进去只会再被搬一次。标记读不到也按回落处理。这一步只多一次 EXISTS,只发生在短重连 / 顶号上。排空集合才是排空状态的权威、标记带 TTL;标记过期而频道仍在排空的少数情形会回进去,随后被排空正常改派走,不影响归属安全。

结果:

- 解析成功:通常就是第 4 步的同落点重连。不铸造 owner_epoch,不改 location,不重复计人数。
- 解析失败(副本已回收、属主节点已死且场景重建不了、读失败):回落默认大世界,与原来相同,计 `enter_scene_rejected_total{reason="resume_scene_fallback"}`。
- 再入屏障未到:照旧回可重试的 `ErrSceneReentryBarrier`,不回落。回落也只会撞上死节点的换手门(18),等屏障过了按死节点接管处理才对。

不受影响的请求:

- 首登(带本 zone);
- 客户端选线(带 scene_id);
- scene 自发的换图、跟随、疏散、交接重发(都带 zone);
- 跨 zone 传送的第二条腿(票据指明本 zone);
- location 在别的 zone(第 2 步已重定向走)。

## 7. 时序与不变量

- `kHoldWindow` 25s + 扫描周期 1s + 余量 4s ≤ login 断线租约 30s,由 `connection_lost_hold.h` 的 static_assert 守住。
  - 保留在租约到期之前结束,退出存盘落地并写出断线释放标记(A1′)。租约到期时 LeaveScene 拿到释放证据,当场删 location。
  - 如果保留拖过租约,LeaveScene 拿不到证据,只能先派 `ReleasePlayer(session_ended)`、回「等持有者释放」。这名玩家的清理要再等一个 claim 超时(约 30 秒),期间重登回「清理中」。
- login 的 30(`sessionmanager.SetSessionDisconnecting` 的 `LeaseTtlSeconds`)与 C++ 的 `kLoginReconnectLease` 两边互相注明。改一边必须同步改另一边。
- 一个实体同一时刻只处于一种状态:在线、保留中、退出中。
  - 保留中 → 退出中(到期,或被别的退出取代);
  - 保留中 → 在线(新会话接回);
  - 退出中的实体不会进入保留。
- 保留中的实体没有活会话(快照会话无效),登录身份不变。所以租约到期的会话结束释放仍能按 §13 的绑定判定退出它;新会话接回后身份变新,迟到的旧释放会被忽略。
- 不丢数据:进入保留时请求一次关键进度存盘;保留期间周期存盘照常;到期退出走原来的退出存盘与收敛判定。

## 8. 残余与后续

1. **gate 与客户端之间的应用层心跳**:2026-11-06 已另起一批做完([gate-client-heartbeat.md](gate-client-heartbeat.md),C++/Go、客户端与 Java 都已落码,协议已生成,验证状态见该文 §11)。验签后开始判死,空闲 30 秒强关连接;强关走既有断开回调,代发 `ExitGame(DISCONNECTED)`,随后就是本文的断线保留,没有另开一条下线路径。未验签的连接有 10 秒握手期限,它们没绑玩家,不代发 ExitGame。节点链路(gate↔scene RPC0)的心跳由另一会话做完(`muduo-rpc-channel.md`)。
2. 客户端大厅断线后不会自动重登(审计 §3.1),属于客户端仓。没有客户端自动重连时,服务端的保留只在玩家手动重进时生效。
3. 保留期间战斗结束:结算照常应用到实体上,终局推送发不出去。重连后的结果面板由另一批「近期战斗结果」恢复([battle-result-recovery.md](battle-result-recovery.md),11-05 落码、未编译),本批不重复做。
4. 按 player_id 移动玩家、又不看活会话的入口:11-06 核对时没有。客户端换图要有客户端;镜像自动进场在进入保留时已撤掉;队伍跟随与跨节点交接都先看活会话;GM 传送(`TeleportRequest`)回 1006。以后新增这类入口要先看 `HasLiveGateSession`。漏看的后果是路由在 gate 被丢弃、location 指向新场景,重连按新 location 落地;不重连时与 gate 崩溃同一残余,由租约到期的会话结束释放和 `location-release-binding.md` §17 的撤销兜底。
   2026-11-10 起唯一的例外是托管落点(§4.2):只替离线托管中的角色发、不带 gate,提交点是直接推给目标 scene 节点的事件,不会落进"路由在 gate 被丢弃"这条残余。
5. 实时场景 CD 与 Buff:保留内重连沿用实体,运行时状态还在;保留到期之后才重连仍会丢。规则未定,见审计 §9.2。
6. 保留中的实体仍被 AOI 当作观察者计算,发给它的推送在 scene 侧因为没有会话直接丢弃。量级等于断线未归的玩家数,不处理。
7. 月卡离线托管:2026-11-06 已做(§4.1),挂机月卡有效的玩家断线后角色常驻到重连或月卡到期。ExitGame handler 里那条 TODO 指的就是这件事;托管中的角色还不会自己活动(§4.1 末段)。2026-11-10 起能跟着队长换场景、搬到本 zone 的别的节点(§4.2)。
8. 节点排空 / 疏散时托管中的角色:2026-11-12 起按托管落点改派到别的节点、接着托管(§4.3),走与在线玩家同一条改派链与待确认表。
   剩下的只有一种:scene_manager 一直不放行(重发两次用尽、或拒绝码不可重试),他不在世界里,直到下次登录(数据在盘上);不再登录的由
   月卡到期后的租约清理收尾。在线玩家这时被踢、自己重登,结局相同,只是托管中的角色没有客户端替他立刻重登。
9. 托管落点提交之后目标节点没把人建起来:2026-11-12 起目标节点按退避有限次重试同一条落点(§4.4,方案比较也在那里)。不补的两类:
   事件在消费之前丢了(目标节点崩溃 / 被同号新进程顶替,与"托管角色随节点崩溃一起消失"同一类),以及提交之后目标场景被销毁;
   理由见 §4.4 末的残余。

## 9. 双版本

Java 版待做,已登记在 Java 仓 `PARITY.md`。Java 目前断链清退玩家的策略与 C++ 不同(`muduo-rpc-channel.md` §8.3 末段)。要做的是同一组行为:断线带原因、保留 25 秒、新会话接回、重连回原场景。

2026-11-10 托管落点(§4.2):Java 版待做,只在 Java 仓 `PARITY.md` 末尾登记,本轮不写 Java 代码。客户端不可见(协议字段与 Kafka 事件都是服务端内部),要对齐的是行为:离线托管的队员跟着队长换场景、到了之后照常被接到队长身后。

2026-11-12 排空 / 疏散改派托管角色(§4.3)与托管落点的看护(§4.4):同样只在 Java 仓 `PARITY.md` 末尾登记待做,客户端不可见。要对齐的行为:
节点重启 / 缩容 / 疏散时离线托管中的角色被搬到别的节点、接着托管与跟随;搬迁被拒且能证明没放行时有界重发;落点到了目标节点却没建起来时有界重试。

2026-11-06 核对 §5 第 3 步的判据修正时看过 Java 版,没有同类问题,没有改 Java 仓:Java gate 发进场前先让旧场景放掉旧玩家,scene 每次进场都重新加载、新建实例并完整入场(`game-scene` 的 `SceneWorld.onPlayerEnter` / `finishPlayerLoaded`),不复用实体,也就没有「按已摘掉的快照会话判换会话」这一步。Java 做断线保留的接回时若改成复用实体,要用 §5 的两条判据:换会话按登录身份判(会话号或登录代次变了),不能比已摘掉的会话;接回保留也比登录代次,不能只比会话号。

## 10. 验证(Codex)

前置:协议生成物里已有 `ExitGameReason`(`cpp/generated/proto/scene/player_lifecycle.pb.h`、`go/proto/scene/player_lifecycle.pb.go`),说明已有人跑过一次完整生成。如需重跑,在仓库根执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。

1. C++ 编译(Windows Debug x64,串行):`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,0 个 `error C` / `LNK`。scene 库、gate、scene 节点都改了;`run_cpp_tests.ps1 -Build` 不重编库,不能代替这一步。
2. C++ 单测:`pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone_test -LogDir build/verification/connection-lost-hold-20261105`。
   - 定向复核(仓库根):`build\cpp\tests\cross_zone_test.exe --gtest_filter=ConnectionLostHold*:ExitPersistEcs.*`。
   - 新增 12 条全部 PASS:`ConnectionLostHold.*` 5 条、`ConnectionLostHoldEcs.*` 7 条;`ExitPersistEcs.*` 原有用例不回归。
   - 11-06 补 5 条,同一个过滤串已包含:`ConnectionLostHold.SameSessionNumberResumesOnlyWithNewerLogin` 1 条、`ConnectionLostHoldReentry.*` 4 条。`IsResumedBy` 与 `ResumeConnectionLostHold` 多了登录代次参数,原有的 `ConnectionLostHold.OnlyANewBoundSessionResumes`、`ConnectionLostHoldEcs.OnlyANewSessionResumesTheHold` 只补了代次 0,期望不变。
   - 另跑 `--gtest_filter=EnterFenceEcs.*:SceneEntryPermissionTest.*`,确认入场栅栏与场景入场的原有用例不回归。
   - 两条回归用例各守一条判据:`SameLoginRepeatRouteStaysInScene` 守第 3 步(拿快照比时,玩家会被移出格子、`Hex` 被摘);`ReusedSessionNumberWithNewerLoginResumesHold` 守第 0.1 步(只比会话号时,保留摘不掉)。
   - 运行证据:11-08 Codex 的整套运行已覆盖上面这 7 条,`cross_zone_test` 458 项全部 PASS(`build/verification/naming-audit-20261108/cpp-tests/cross_zone_test/cross_zone_test.stdout.log`)。测试能链接上三参的 `ResumeConnectionLostHold`,说明库是带本次修正重编过的。
   - 11-06 离线托管(§4.1)补 7 条,同一个过滤串已包含:`ConnectionLostHoldAfkPass.*` 3 条(解析、托管中的判定、没有月卡时的普通窗口)、
     `ConnectionLostHoldAfkPassEcs.*` 4 条(月卡有效过了 25 秒不退出、月卡没了按断线退出、迟到 / 失败 / 别的保留的结果不改状态、托管开始替队员开启跟随)。
     原有 `ConnectionLostHoldEcs.*` 不回归:测试里玩家域 Redis 没连上,查询是空操作,保留仍按 25 秒到期。
3. Go(`go/scene_manager`):
   - `go build ./...`、`go vet ./internal/logic/`;
   - `go test ./internal/logic/ -count=1 -v -run 'TestResumeSceneOf|TestEnterScene_ZeroZoneReconnect|TestEnterScene_ZeroZoneOverLocationInGateZoneBehavesLikeExplicitGateZone|TestEnterScene_SamePlacement'`,全部 PASS;
   - 最后 `go test ./... -count=1`。同包有别的会话的在途改动,红在别处时先分清是哪边的。
4. Go(`go/login`):`go build ./...`。本批只改了注释。
5. 本地联调(可选,不替代以上):起本地全套,机器人进大世界副本或非默认地图后断开 TCP。
   - 25 秒内用 access_token 重登:scene 日志依次有 `[ConnectionLostHold] holding player` 和 `resumed by a new session`;RoutePlayerEvent 的 scene_id 等于断线前的场景;客户端收到入场通知。
   - 超过 25 秒不重连:出现 `hold of player … expired`,随后是原来的退出存盘日志;30 秒租约到期时 LeaveScene 应判 `released`,不应出现 `pending`。
6. 2026-11-10 托管落点(§4.2),**未编译、未测试**。命令与顺序写在 `docs/PROGRESS.md` 同日条目,这里只列用例:
   - 先跑协议生成(新增 `EnterSceneRequest.hosted_placement`、`proto/common/event/player_placement_event.proto`;生成器会把新的 `.pb.cc` 同步进
     `cpp/generated/proto/proto.vcxproj`、生成事件号与 scene 的 Kafka 白名单 / 分发;`cpp/nodes/scene/handler/event/player_placement_event_handler.{h,cpp}`
     已按生成器模板手写好守护段,生成时原样保留)。
   - Go(`go/scene_manager/internal/logic/enter_hosted_placement_test.go`,8 条):同节点不铸代、事件发到目标节点分区并带实例号与断开那次登录;跨节点凭标记铸代、
     先提交后派 ReleasePlayer;没有标记回 18 不写;推事件失败回滚(epoch bump 到 3);请求形状不对 / 跨 zone / 目标节点没有唯一实例号都在写之前拒;同落点重发事件。
   - C++(`cross_zone_test`):`ConnectionLostHoldAfkPass.HostingNeedsAConfirmedPassWithTimeLeft`、
     `ConnectionLostHoldAfkPass.HostedFollowRecheckIsSpreadOverThePeriod`(11-11 兜底重试)、`HostedPlacementEcs.*` 2 条、`HostedPlacementReentry.*` 5 条、
     `TravelHandoffInitiatorEcs.*` 2 条;跟随侧见 team-follow-server-trail.md §8。
7. 2026-11-12 排空 / 疏散改派托管角色(§4.3)与托管落点的看护(§4.4),**未编译、未测试**。命令与联机验收写在 `docs/PROGRESS.md` 同日条目,这里只列用例:
   - C++(`cross_zone_test`)纯判定:`RelocateConfirmTicket.HostedTicketFollowsTheLoginIdentityNotTheDisconnect`、
     `RelocateConfirmHosted.ResendOnlyWhenSceneManagerCannotHaveGrantedTheRelocate`、`RelocateConfirmHosted.OnlyTransientRejectionsBeforeAnyWriteAreRetryable`、
     `RelocateConfirmTable.HostedResendWaitsThenReturnsToMarkWriting`、`HostedPlacementRetry.*` 3 条;改了一条既有用例的期望表:
     `RelocateConfirmNames.CoverEveryValue` 的推送结果名表多了 `no_client`(新增取值,不是放宽)。
   - C++ ECS:`RelocateConfirmEcs.HostedCharacterIsRelocatedThroughTheConfirmTableAndNeverPushed`(托管票据进表、身份是断开那次登录、
     没有 gate、A1′ 让位;读不到时结清不推送)、`RelocateConfirmEcs.HostedTicketIsVoidedWhenTheLoginIdentityChanged`、
     `RelocateConfirmEcs.HostedPlacementBackOnThisNodeLandsTheHostedEntry`(同一次登录 → landed_here,新登录 → superseded)、
     `HostedPlacementRetryEcs.*` 2 条(登记、退避、读不到不重试、替换、超预算放弃;同节点不登记、有实体即结清)。
   - Go(`enter_hosted_placement_test.go`):`TestEnterScene_HostedPlacementEvacuationLandsOnTheResolvedTarget`(托管落点带 evacuation:
     按疏散去向派到原频道所在的节点、凭标记铸代、事件发到目标节点,之后才派 ReleasePlayer)。
   - 宿主里没有 scene_manager / gate / 玩家域 Redis,所以"读到仍在源场景 → 重发"与"读到仍指着本节点 → 重试"这两条读后路径只有纯判定
     与状态迁移的单测,真实链路留给联机验收。

## 11. 改动清单

- proto:`proto/scene/player_lifecycle.proto`(`ExitGameReason`、`GameNodeExitGameRequest.reason`)。
- gate:
  - `cpp/libs/services/gate/session/comp/session_info_comp.h`(`exitSceneImmediatelyOnClose`);
  - `cpp/nodes/gate/handler/rpc/client_session_handler.{h,cpp}`(`NotifySceneExitGame` 带原因,断线日志带 `exit_reason`);
  - `cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`(LeaveGame 发 LOGOUT);
  - `cpp/nodes/gate/handler/event/gate_event_handler.cpp`(租约到期关连接前置位)。
- scene:
  - `cpp/libs/services/scene/player/system/connection_lost_hold.h`(新增,组件 + 常量 + 纯判定);
  - `player_lifecycle.{h,cpp}`(`HandleClientConnectionLost`、`ResumeConnectionLostHold`、`ExpireConnectionLostHolds`,`HandleExitGameNode` 摘保留,`EnterScene` 第 0.1 步);
  - `player_exit_intent.h`(注释);
  - 11-06 离线托管:`connection_lost_hold.h`(`AfkPass`、`ParseAfkPass`、`JudgeHold` 与保留组件的月卡字段)、
    `afk_hosting.{h,cpp}`(新增,查月卡与回包)、`player_lifecycle.cpp`(进入保留时查一次、扫描改用 `JudgeHold`);
  - `cpp/nodes/scene/handler/rpc/player/player_lifecycle_handler.cpp`(按原因分流);
  - `scene.vcxproj` / `.filters`(登记新头文件)。
- scene_manager:`go/scene_manager/internal/logic/enter_resume_scene.go`(新增)、`enterscenelogic.go`(第 3a 步与回落指标)、`enter_resume_scene_test.go`(新增)。
- login:`go/login/internal/logic/pkg/sessionmanager/session_manager.go`(租约与 C++ 常量互注)。
- 测试:`cpp/tests/cross_zone_test/cross_zone_test.cpp`(`ConnectionLostHold*`)。
- 2026-11-10 托管落点(§4.2):
  - proto:`proto/scene_manager/scene_manager_service.proto`(`EnterSceneRequest.hosted_placement = 14`)、`proto/common/event/player_placement_event.proto`(新增)。
  - scene_manager:`enter_hosted_placement.go`(新增:预检、实例号、`routePlacement` / `routeHostedPlayerToScene`)、`enterscenelogic.go`(接线 6 处)、
    `metrics/metrics.go`(Help 补三个 reason)、`enter_hosted_placement_test.go`(新增)。
  - scene 库:`player_lifecycle.{h,cpp}`(`PlayerEnterContext.hostedPlacement` / `NotifiableSessionId`、`OfflineHostedIdentity`、`HostedPlacementContext`、
    `HandleHostedPlacement`、`EnterHostedHold`、入场链不绑不推、`TravelInitiator`、`PublishAndEnterTravel` 的托管重发、跟随应答的 18、
    服务器发起的交接不回 tip)、`role_admission_system.cpp`(被拒时推给可推的会话、在途比对带 `hostedPlacement`)、
    `connection_lost_hold.h`(`IsHosting`)、`player/comp/player_ownership_comp.h`(`PlayerTravelHandoffComp.serverInitiated`)。
  - scene 节点:`handler/event/player_placement_event_handler.{h,cpp}`(新增,守护段)、`scene.vcxproj` / `.filters`(登记)。
  - 跟随侧文件见 team-follow-server-trail.md §3.7。
- 2026-11-12 排空 / 疏散改派托管角色(§4.3)与托管落点的看护(§4.4):
  - scene 库:`player/system/relocate_confirm.h`(`DecideHostedTicket`、`GatePushResult::kNoClient`、`ShouldResendHosted` / `HostedResendDelay`、
    条目的 `hosted` / `target` / `ownerRevision` / `hostedResends`、`Table::ArmHostedResend` / `BeginHostedResend`)、
    `player/system/hosted_placement_retry.h`(新增)、`player_lifecycle.h`(`SmRejectionRetryableForHostedRelocate`、`ResendHostedRelocate`、
    `SweepHostedPlacementRetries` / `HostedPlacementRetriesPending` / `PeekHostedPlacementRetry`、计数 `hosted_resent`)、
    `player_lifecycle.cpp`(托管票据的发票 / 派发 / 请求、踢线与落地按登录身份认人、重发、看护的登记 / 结清 / 扫描 / 权威读、
    托管落点按断开那次登录对账)、`core/system/redis.cpp`(1 秒节拍里调看护扫描)、`scene.vcxproj` / `.filters`(登记新头)。
  - 测试:`cpp/tests/cross_zone_test/cross_zone_test.cpp`、`go/scene_manager/internal/logic/enter_hosted_placement_test.go`。
  - 文档:本文 §4 / §4.1 / §4.2 / §4.3 / §4.4 / §8 / §10、team-follow-server-trail.md §3.7 / §8 / §9、scene-node-planned-drain.md §4.4 / §9、
    cross-zone-scene-travel.md §13.8、`docs/ops/cross-zone-failure-test-runbook.md`([RelocateConfirm] 汇总行与 kick 行)。
