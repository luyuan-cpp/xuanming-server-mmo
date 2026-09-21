# 跨 zone 场景传送(玩家去别的 zone 的地图游玩)

**状态:** 阶段 1、2、3 的代码**全部已落码并进入 main**(2026-09-18),另打通了生产配置下客户端发起的跨节点换图。**全程未编译、未跑测试**(用户明确要求);导表与 proto 生成**已完成**(§11.4)。落码记录:阶段 1 见 §10,阶段 2/3 见 §11。§8 三个默认值按「不确认即按此执行」生效。
**关联:** [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) D1/D2(跨区 = 客户端重连,数据不搬家)、[enter-scene-zone-routing.md](./enter-scene-zone-routing.md)(Cross-Zone Flow / Offline-Return / Login-side)、[scene-owner-reentry-barrier.md](./scene-owner-reentry-barrier.md) §3.3(owner_epoch)、[scene-switch-release-design.md](./scene-switch-release-design.md)、[cross-zone-readiness-audit.md](./cross-zone-readiness-audit.md)、[team-system.md](./team-system.md) DV-6 / D.3、[turn-based-battle-server.md](./turn-based-battle-server.md) §19。

> 用户 2026-09-16 拍板:客户端直连 battle 与跨 zone 场景传送"两个都做",先直连。本文只管传送。
> 一句话:**玩家仍归属 home_zone,只是人换到目标 zone 的 gate/scene 上玩;数据不搬家,存盘按 home_zone 路由;换手必须过"源已落盘 + epoch"两道门。**

## 1. 目标与非目标

- 目标:A 区玩家可以传送到 B 区的地图,与 B 区玩家同场景互动(观光、PVE、进副本、打战斗),之后可回家或下线后回家。
- 非目标:合服(另有 [server_merge_design.md](./server_merge_design.md));跨区组队/帮会/榜单归属变更(仍按 home_zone,见 CZ-6);TiDB Phase 2 全局库(本设计是它的前置,不等它)。

## 2. 现状(2026-09-16 摸底,引用为工作树位置)

> **历史快照(2026-09-20 标注)**:本节是动工**之前**的摸底,其中"跨区 / 跨节点一律 `ErrUnsafeCrossNodeHandoff`""存盘固定按进程 zone 选 topic"等已被 §10–§12 的落码改变(14 已是历史码,同类拒绝现在回 18;存盘按 home_zone 选 topic)。读现行行为请看 §10 之后。

- **运输层已齐**:`scene_manager` `handleCrossZoneRedirect` → `AssignGateForZone` 签 5 分钟 HMAC 票据 → Kafka `RedirectToGateEvent` → gate 推 msg 124 → Unity `GameClient.RedirectFlow`(探测 / 先连新后关旧 / 原样转票据 / 完整重跑 Login + EnterGame / 3 跳熔断)。跨区匹配 9-05 实机跑通过这条链。
- **生产 fail-closed**:`enterscenelogic.go:278 / :333`——玩家 Redis 里有 `player:{id}:location` 时跨区/跨节点一律 `ErrUnsafeCrossNodeHandoff`;`AllowUnsafeCrossNodeHandoff` 生产 false(本地 yaml true 仅供联调)。只有干净登出后的"首次落点"才会被重定向。
- **login 侧**:`HomeZone.RedirectOnEnter` 只会把人"送回 home_zone",没有"去别的 zone"的意图表达;打开它,目标 zone 的 login 会把访客再弹回家(与 RedirectFlow 的 3 跳熔断互撞)。
- **数据归属**:scene 只读本 zone `zone_redis` 的 `PlayerAllData:{id}`(`scene_handler.cpp:198`),NIL 直接当新号(`player_lifecycle.cpp:126`);存盘固定 `GetDbTaskTopic(GetZoneId())`(`player_lifecycle.cpp:1309`)→ `zone_{本zone}_db`。K8s 脚本让所有 zone 指向同一个 infra Redis,但 MySQL 一定分库。
- **交接屏障**:再入屏障(节点判死后 20s)已落码;per-player `owner_epoch` 在 proto / Go / C++ 三处均未落码(`RoutePlayerEvent` 无 `owner_epoch`,`changesceneutil.go:66` 裸 SET,C++ 无 CAS)。
- **player_migrate 搬数据链**(*2026-09-18 已下线,见 §11.3;以下是下线前的摸底记录*):代码齐全(Frozen + ACK + reaper,已订阅),`PlayerAllData` 已含 bag / mission(2026-05 审计的"7 组件"已部分过时;mail 按决策走独立 Go 服务),但**全仓无任何写 `to_zone_id` / `is_cross_zone` 的触发点**,且目的端按空 enterInfo 建实体、不改 location、存盘打到目的 zone 库——与 D2 冲突,不作为路径。
- **组队/战斗**:`HandleCrossZoneTransfer` 拒绝 `InBattleComp` 在途;team DV-6 场景跟随只同 zone,`Team.AllowCrossZone` 默认 false。

## 3. 决策

| # | 决策 | 理由 / 代价 |
|---|---|---|
| CZ-1 | **路径 = 重定向 + 目标 zone 直接加载**,不走 player_migrate 搬数据。player_migrate 只保留 Frozen / 标记语义(D2 原话),数据搬运分支下线 | 与 D2、红线 2(玩家数据不作为常规机制跨服拷贝)一致;搬数据路径要重做到达侧、目标节点选择、location 更新,且绕不开存盘路由问题 |
| CZ-2 | **访客期间数据仍归 home_zone**:存盘 `DBTask` 按 **home_zone** 选 topic(`GetDbTaskTopic(home_zone)`),而非进程 zone;`PlayerAllData` Redis 以"生产 Redis 物理共享"为**契约**写进部署文档(K8s 现状即如此),`data_service.Regions` 分区模型留给 TiDB Phase 2 之后再议 | 这正是 TiDB 决策 Phase 2 的第一步(D5 "DBTask 按 home_zone 选 topic"),可独立先做;回家不回档。代价:zone_redis 物理分开的部署形态在本设计下**不支持**,必须写进契约 |
| CZ-3 | **scene 从路由事件拿 home_zone**:`scene_manager.EnterScene` 从 data_service `GetPlayerHomeZone` 取(它已是唯一真源,login 也这么取),写进 `RoutePlayerEvent.home_zone_id` 随 Kafka 下发;C++ 在 `HandlePlayerAsyncLoaded` 建实体时存进 `PlayerHomeZoneComp`,存盘按它选 topic;`home_zone_id == 0` 视为"未知",**fail-closed 用进程 zone 并打 WARN 计数**(不许静默落错库) | 不让 C++ 节点自己去 data_service 查:多一个同步依赖、且两次改派挨近时会读到后一次的值(与 owner_epoch 同理) |
| CZ-4 | **两道换手门**(全部在 `scene_manager.EnterScene` 这唯一闸口判定):① `owner_epoch` CAS 按 [scene-owner-reentry-barrier.md](./scene-owner-reentry-barrier.md) §3.3 **Go 写 + C++ 校验同一批**落码(`RoutePlayerEvent.owner_epoch`,C++ `PlayerOwnerEpochComp`,`kSaveAndMarkLuaScript` 原子比对,DBTask 带 epoch);② **已落盘标记**:源 scene 在 `HandlePlayerAsyncSaved`(Redis 落地回调)之后写 `player:{id}:handoff = {epoch, saved_at_ms}`,`EnterScene` 对"已有 location 的跨节点/跨 zone"改为:`handoff.epoch == 当前 owner_epoch` 即放行并 `INCR`,否则回 `ErrSceneReentryBarrier` 同款可重试拒绝(客户端已有退避);`AllowUnsafeCrossNodeHandoff` 开关保留为 dev 旁路,生产不再需要它 | 屏障靠时间、epoch 不靠时间,两层缺一不可(§3.3 半接线警告);标记与 epoch 同一原子域(`player:{id}:*` 同一 Redis 实例;键名无 hash-tag,不支持 Cluster)。**落码时的修正见 §10.2 R1/R4/R5/R6** |
| CZ-5 | **源端释放链**:传送请求在源 scene 内走"冻结输入 → `SavePlayerToRedis` → 落地回调写 handoff 标记 → 调 `SceneManager.EnterScene(ZoneId=目标, SceneId=0, PlayerId)` → 收到重定向后 `HandleExitGameNode` 销毁实体";`EnterScene` 放行后**更新** location 到目标 zone(而非删除),失败(目标 zone 无可用 gate / 世界频道)则回源 scene 解冻并回 tip | 复用紧急疏散已有的"先存盘后改派"顺序(`DispatchEmergencyRelocate`);location 更新而非删除,让崩溃遗留可被 Offline-Return 规则识别 |
| CZ-6 | **访客业务范围**:目标 zone 里允许观光、PVE、副本、战斗(battle 本就是全局池)、聊天(世界频道按物理 zone);**组队按 team D.3(默认只许同区)、帮会 / 榜单 / 聚宝斋按 home_zone**,访客在目标 zone 看到的是自己 home_zone 的帮会与榜单;战斗在途(`InBattleComp`)、组队在途、备战中拒绝传送 | 与已拍板的 team DV-6 / D.3、TiDB D1(业务表以 home_zone 区分)一致;避免"人在 B 数据在 A"的业务分叉 |
| CZ-7 | **意图入口 = 客户端 RPC `SceneSceneClientPlayer.TravelToZone{target_zone_id, scene_config_id}`**(走 scene,不走 login;*落码时更正:不能加在 `ScenePlayer`——那是 gate→scene 的内部服务,没有 `OptionIsClientProtocolService`,客户端包会在 gate 被当成非法包*):scene 校验 CZ-6 规则与目标 zone 存在性(`zone_config` 经 scene_manager)→ 触发 CZ-5。传送点 / 地图选择 UI 由客户端决定何时调用;GM 命令同一入口 | 传送是场景内行为,状态(冻结、存盘)只有 scene 知道;login 的 `RedirectOnEnter` 只负责"登录时送回 home",两者互不干扰 |
| CZ-8 | **目标 zone 识别访客**:重定向票据 `GateTokenPayload` 增加 `player_id` 与 `target_zone_id`(绑定持票者,顺手堵掉"持票者可冒用"的既有缺口);目标 zone 的 login `EnterGame` 看到票据 `target_zone_id == 本 zone` 时**不按 home_zone 弹回**,直接 `EnterScene(ZoneId=本 zone)`;票据 TTL 内未落地则视为传送失败,下次登录按 Offline-Return 规则回家 | 消除重定向乒乓;访客身份有据可查 |
| CZ-9 | **回家 = 同一条链反向**(`TravelToZone{target=home_zone}`);离线 / 崩溃后按 enter-scene-zone-routing.md Offline-Return:干净登出回家,崩溃遗留的异 zone location 由 CZ-4 的 handoff 标记决定能否直接接管(标记齐则放行、否则运维清理 —— *2026-09-19 起"否则运维清理"已被 §11.5 的属主已死自动接管取代;仍需人工介入的边界见 §12.3 GO-3 / GO-5,手工删 location 的安全前提见 enter-scene-zone-routing.md*) | 不新增第二套回家机制 |
| CZ-10 | **不搬 mail**:mail 走独立 Go 服务(cross-zone-readiness-audit §3.3 决策 6);该服务存在前,访客在目标 zone 收不到邮件提醒,写进已知限制 | 不把 MailAllData 临时塞进 PlayerAllData 再拆 |

## 4. 数据流

```
客户端(A区 gate) ──TravelToZone{B, scene_cfg}──▶ scene(A)
  scene(A): 校验 CZ-6 → PlayerFrozenComp 冻结输入 → SavePlayerToRedis
  ──落地回调──▶ SET player:{id}:handoff {epoch=E, saved_at}
  ──gRPC──▶ scene_manager.EnterScene{ZoneId=B, SceneId=0, PlayerId}
     scene_manager: location 存在 → 读 handoff → epoch==E 且已落盘 → INCR owner_epoch=E+1
                    → 选 B 区 gate → 签票据{player_id, target_zone_id=B, expire}
                    → 更新 location{zone=B, node=待定, epoch=E+1}
                    → Kafka RedirectToGateEvent → gate(A) → 客户端 msg 124
  scene(A): 收到 EnterScene 应答(redirect) → HandleExitGameNode 销毁实体
客户端: RedirectFlow → 连 gate(B) → Login → EnterGame(票据.target_zone_id=B → 不弹回)
  scene_manager.EnterScene(B): location.zone==B 且 epoch==E+1 → 选 scene 节点 → RoutePlayerEvent{home_zone_id=A, owner_epoch=E+1}
  scene(B): 从共享 Redis 读 PlayerAllData:{id}(A 区刚落盘的版本)→ 建实体 → 存盘 topic=db_task_zone_A、epoch CAS
```

失败分支:目标 zone 无可用 gate / 频道 → `EnterScene` 回可重试错误 → scene(A) 解冻、回 tip,location 不变;票据过期未落地 → 下次登录 Offline-Return。

## 5. 改动清单(按阶段;每阶段独立可回退)

**阶段 0 前置(不落码)**:并行会话 trade 的导表 / 生成 / 提交完成、regen 解冻;确认 `player:{id}:*` 键在 K8s 与本地都落同一个 Redis(CZ-2 契约);与 team-system 批 3(改 `player_battle.cpp`)协调顺序。

**阶段 1 归属与屏障(纯服务端,不改客户端)**
- proto:`contracts/kafka/gate_event.proto` `RoutePlayerEvent` 加 `home_zone_id`、`owner_epoch`;`db_task` 消息加 `owner_epoch`;`GateTokenPayload` 加 `player_id`、`target_zone_id`(字段号只追加)。
- Go `scene_manager`:`EnterScene` 铸造 epoch(INCR)+ 读 data_service home_zone + 写入路由事件;`changesceneutil.go` location 写入带 epoch CAS;新增 handoff 标记读取与 CZ-4 判定;`AllowUnsafeCrossNodeHandoff` 语义降为 dev 旁路。
- Go `db`:消费 DBTask 时按 `owner_epoch` 与 applied-seq 守卫(reentry-barrier §6.3)。
- C++ scene:`PlayerHomeZoneComp` / `PlayerOwnerEpochComp`;`SavePlayerToRedis` 的 Lua 变体做 epoch CAS;`GetDbTaskTopic(home_zone)`;`HandlePlayerAsyncSaved` 后写 handoff 标记(仅在"传送中"状态下写,常规存盘不写)。
- 验证:双 zone 联调脚本 + 断言(同一 player 在两 zone 的 `PlayerAllData` 版本、`zone_A_db` 与 `zone_B_db` 的 `player_database` 行:B 库不得出现该玩家)。

**阶段 2 意图入口与访客识别(服务端 + 客户端一行)**
- proto:`SceneSceneClientPlayer.TravelToZone` 客户端 RPC(`proto/scene/player_scene.proto`,必须追加在 service 末尾)+ tip 码(`Tip.xlsx` 新段或 scene 段:目标区不存在 / 战斗中不可传送 / 组队中不可传送 / 目标区繁忙);
- C++ scene:`TravelToZone` handler 走 CZ-5;`HandleCrossZoneTransfer` 的在途校验复用(*落码时更正:`HandleCrossZoneTransfer` 已随阶段 3 删除,CZ-6 校验落在 `PlayerLifecycleSystem::RequestZoneTravel`*);
- Go login:`EnterGame` 识别票据 `target_zone_id`(CZ-8);gate 令牌验签补 `player_id` 绑定(gate + scene_manager 签发侧);
- 客户端:`GameClient` 增加 `TravelToZone` 调用点与"传送中"状态文案;RedirectFlow 不改。

**阶段 3 收尾**:player_migrate 数据搬运分支下线(保留 Frozen 组件与标记语义);cross-zone-readiness-audit.md 顶部标注被本文取代的段落;enter-scene-zone-routing.md Cross-Zone Flow 改写为 CZ-4 口径;robot 增加 `travel-smoke`。

## 6. 不变量(并入 AGENTS §7 语义)

1. 任一时刻只有一个 scene 进程持有玩家状态;归属变更只经 `scene_manager.EnterScene`,顺序必须是"源存盘落地 → 写标记 → 放行 → 目标加载",违反即串档;
2. 玩家数据的落库目的地由 home_zone 决定,与进程所在 zone 无关;`home_zone_id` 未知时不得静默落进程 zone 库;
3. 老 epoch 的写入(Redis Lua / DBTask)一律被拒并计数 `stale_owner_write_rejected`,压测期恒 0;
4. 重定向票据绑定 `player_id`,目标 gate / login 只接受持票者本人。

## 7. 明确不做 / 已知限制

- 不做跨区实时同步、不做数据回迁协议;不支持 zone_redis 物理分开的部署形态(CZ-2);
- mail 提醒在目标 zone 不可用(CZ-10),mail 服务落地后消除;
- 访客的组队沿用 team D.3 默认只许同区;帮会 / 榜单按 home_zone;
- 不做 TiDB Phase 2 的全局表收敛,本设计与之兼容:Phase 2 落地后 CZ-2 的 topic 路由自然消失。
- **传送窗口内,全局服务推给该玩家的 S2C 会丢,且不补发**(2026-09-19 与 friend 移植会话对齐)。
  窗口是「源 scene 销毁实体 → 目标 zone 落点完成」之间,最坏约 60s(存盘与等应答两道看门狗各
  `kTravelReplyBudgetSec`=30s,串行)。成因不是本设计的疏漏,而是两个既有契约叠在一起:
  目标 zone 的 login 做严格重登时会重写共享库里的 `player:session:{id}`;而全局服务(friend 等)
  的推送走 `kafkautil.PushToPlayer` → `gate-cmd_g<N>`,读的就是这个键,**at-most-once**,
  投递到旧 gate 后被 `target_instance_id` 过滤掉就没了,没有重投也没有补推。
  兜底**不能**写成「收到推送就去拉」—— 在这个窗口里推送根本到不了。正确口径是
  **客户端在每次落地(登录后、跨 zone 传送落地后、打开对应面板时)各主动拉一次权威状态**,
  例如 friend 的 `GetPendingRequests` / `GetFriendList`。
  现状:目前只有 robot 的 friend-smoke 实现了「拉取验证」,Unity 客户端侧的 handler 尚未编写。
  参见 `docs/design/friend-port-20260918.md` 的推送决策与「已知缺口」、`go/friend/internal/logic/push.go`。

## 8. 需要用户确认的默认值(不确认即按此执行)

1. CZ-2:生产 Redis 全 zone 物理共享(与当前 K8s 脚本一致),不支持按 zone 分 Redis——可以接受吗?
2. CZ-6:访客在目标 zone 允许"观光 / PVE / 副本 / 战斗 / 聊天",组队仍默认只许同区,帮会 / 榜单 / 聚宝斋按 home_zone——可以接受吗?
3. CZ-7:传送入口是客户端在场景内发起(传送点 / 地图 UI),不是登录时选区——可以接受吗?

## 9. Codex 验证清单(阶段 1)

Claude 未运行任何构建 / 测试 / regen(AGENTS §10.1)。按顺序执行,任一步失败保留完整输出、不要跳过:

1. **regen**(新增 6 处 proto 字段,生成代码目前不存在):`cd go && build.bat`;C++ 侧按仓库惯例重生 `cpp/generated/proto`。涉及:`RoutePlayerEvent.home_zone_id/owner_epoch`、`PlayerEnterGameNodeRequest.home_zone_id/owner_epoch`、`DBTask.owner_epoch`、`GateTokenPayload.player_id/target_zone_id`、`PlayerLocation.owner_epoch`、`EnterSceneResponse.player_id`。
2. **Go**(各自目录,`gofmt -l .` 应为空;手工对齐过的文件可能要 `gofmt -w`):
   - `go/shared`:`go vet ./ownerepoch/... && go test ./ownerepoch/...`
   - `go/scene_manager`:`go mod tidy`(首次引入 `proto/data_service` 与 grpc `codes/status`)→ `go build ./... && go vet ./... && go test ./...`。重点文件 `internal/logic/owner_epoch_test.go`(新)、`logic_test.go`(3 处语义跟随)。
   - `go/db`:`go build ./... && go vet ./... && go test ./internal/kafka/...`
3. **C++**(MSBuild 串行 `/m:1`):先 `engine`(`redis_client.h` 是头文件,改动波及所有用到 `MessageAsyncClient` 的工程)→ `scene` 库 → `gate` / `scene` 节点 → `cpp/tests/currency_test`(含 3 个 `RedisGuardedSave*` 用例)。
4. **本地冒烟(单 zone,回归)**:`start_game.ps1` → `robot login-test` 应保持 23/23;日志里 `[OwnerEpoch]` 汇总行三个计数都应为 0(`owner_epoch_unknown` 非 0 = gate 或 scene_manager 还是旧二进制)。重点看**同节点换图 + 周期存盘**:`stale_owner_write_rejected` 必须恒 0(§10 R1 的回归点)。
5. **双 zone 联调**(`dev-start-zones 1,2`,`AllowUnsafeCrossNodeHandoff=false`):阶段 1 没有 `TravelToZone` 入口,传送往返要等阶段 2;可验:①跨节点换图**能成功**(§11.2:18 → 源 scene 冻结存盘写标记 → 重发 → 放行;日志序列见 §11.2),`stale_owner_write_rejected` 恒 0;②`BeginSceneDrain` / 整节点疏散的玩家能被改派(疏散链现在会写 handoff 标记);③`db_stale_owner_write_rejected_total == 0`、`scene_manager_home_zone_lookup_total{outcome="mapped"}` 在涨、`zone_B_db.player_database` 不出现 A 区玩家。
6. **K8s**:`k8s_deploy.ps1` 的 scene-manager ConfigMap 已补 `DataServiceRpc`;重新生成后确认 Pod 不因缺它而对所有 EnterScene 回 20。

## 10. 阶段 1 落码记录(2026-09-18)

实现由工作流 `wf_414e8336-2fd` 并行落码(5 包)+ 5 视角复审得到 45 条候选问题;核实阶段的子 agent 因额度全部失败,**核实与回修由主会话人工完成**,结论如下。

### 10.1 落了什么

| 包 | 内容 |
|---|---|
| proto | 上述 6 处字段(只追加字段号) |
| `go/shared/ownerepoch` | 两把键的格式契约与解析(`player:{id}:owner_epoch`、`player:{id}:handoff` = `"{epoch}:{saved_at_ms}"` EX 300) |
| `go/scene_manager` | `EnterScene` 单次观察 epoch → 换手门(标记比对)→ Lua 原子「CAS + 铸造 + 写 location」;路由失败按精确值回滚 location 与 epoch;`data_service.GetPlayerHomeZone` 客户端;票据带 `player_id/target_zone_id`;应答回显 `player_id` |
| `go/db` | 落库前 epoch 守卫 + 合并器把 epoch 变化当段边界 |
| C++ gate | `SessionInfo` 存 `homeZoneId/ownerEpoch`,两个转发点透传到 `PlayerEnterGameNodeRequest` |
| C++ redis client | `MessageAsyncClient::Save(..., guardKey, guardExpected)` + 变体 Lua + rejected 回调 |
| C++ scene | `PlayerHomeZoneComp / PlayerOwnerEpochComp / PlayerTravelHandoffComp`;存盘按 home_zone 选 topic、带 epoch 守卫;被废黜销毁;传送源端释放链(阶段 2 的入口挂上 comp 即生效);疏散 / 排空写 handoff 标记 |

### 10.2 复审后的关键修正(与最初落码 / 本文早期表述不同之处)

| # | 修正 | 原因 |
|---|---|---|
| R1 | **持有者没换就不铸造 epoch**:同节点换图、同落点重连只做「epoch 不变才写 location」的 CAS | 持有节点要等 Kafka→gate→scene 才知道新值,窗口内它的周期 / 退出存盘被 C++ CAS 拒,合法持有者被不存盘销毁(踢人 + 回档)。5 个复审视角各自独立命中 |
| R2 | C++ rejected 回调带**被拒的期望值**;实体当前 epoch 与它不同 → 只是旧代际的在途写,重新存盘而不是自毁;排队中带更新 guard 的值照常发出 | R1 的 C++ 兜底;也覆盖路由事件与在途存盘交错的情形 |
| R3 | **db 守卫比的是「该 key 已成功落库的最大 epoch」**(`consumer:applied_epoch:{topic}:{key}:{msgType}`,只升不降),不是 Redis 里的当前值 | 源节点交接前发出、交接后才被消费的最后一笔是合法写;拿当前值比会每次都把"传送前最终态"当僵尸写丢掉,`stale` 计数失去判别力 |
| R4 | 换手门预检与落点 CAS **锚在同一次观察**上(`observedEpoch` 原样进 Lua),且凭标记放行时标记要在**铸造的同一段 Lua 里**原样还在 | 落点时重新 GET 会让两个并发 EnterScene 各自 CAS 成功(双主);标记只在预检里读,源端"超时解冻"与迟到放行之间有窗口 |
| R5 | 源端在 EnterScene 发出之后的"未成"(超时 / 失败应答)一律走 `ResolveTravelOutcome`:先 `DEL handoff` 再 `GET owner_epoch`,相等才解冻,不等即已被放行 → 销毁 | 失败应答与超时都不能证明没被放行;配合 R4 后这个判定没有竞态 |
| R6 | dev 旁路(`AllowUnsafeCrossNodeHandoff=true`)下无标记放行**不铸造** | 旁路是「先异步通知旧节点释放、随即改派」,铸了旧节点的释放存盘必被拒,每次跨节点换图确定性回档 |
| R7 | 跨 zone 重定向分两种:目标 zone 就是玩家所在 zone(或无位置记录)→ **只送连接**,不过门、不写 location、不铸造;真正离开所在 zone 才过门并写「等待落点」 | 访客掉线后从别区 gate 登录时,持有节点没在交接、永远写不出标记,会被永久挡在门外。**2026-09-20 复核:这条只在请求 `ZoneId == 0` 时生效,而 login 恒发 `ZoneId = 本 zone`,生产上从 login 走不到,该场景实际仍回 18,见 §12.3 GO-5**。*2026-09-21:GO-5 已落码、待验证 —— login 在 ShortReconnect / ReplaceLogin 时发 `ZoneId = 0`,R7a「只送连接」从 login 可达(仅对落在具体节点上的 location),见 §12.7* |
| R8 | 「等待落点」只在票据有效期(300s)内牵引去向,过期按 gate zone 常规落点 | CZ-8 / CZ-9:票据过期 = 传送失败,回家。**已被 §12.7 取代(2026-09-21,已落码待验证)**:`ZoneId == 0` 时等待落点一律不牵引、按 gate zone 落点,过期与否对去向不再有区别;`awaitingExpired` 只剩一个用途 —— 决定第 3 步是否采用等待落点里记的目标地图(`enterscenelogic.go:420`) |
| R9 | guard 键**缺失**时 C++ 存盘放行并补种期望值 | Redis 被清 / 重启后,fail-closed 会把全服在线玩家逐个判成被废黜、丢掉内存里唯一完好的状态 |
| R10 | `DataServiceRpc` 未配置默认 **fail-closed**;单 zone 联调显式 `AllowGateZoneAsHomeZone: true`;K8s 模板已补 | 多 zone 下按 gate zone 当归属 = 访客落错库且零报错(不变量 2) |
| R11 | 应答对回玩家用 `EnterSceneResponse.player_id` 回显,不用 gRPC metadata | 少一层管线;去重缓存重放也不丢 |

### 10.3 本阶段明确没做(已知限制)

- ~~生产下客户端发起的跨节点换图仍被拒~~ **已在 §11.2 打通**(把 18 当成「请先存盘并出示标记」,只改 C++)。疏散 / 排空本来就先存盘,阶段 1 已接上。
- ~~单节点硬崩遗留的 location 被 18 永久挡住、需运维清理~~ **已在 §11.5 解决**(属主节点已确认死亡 + 再入屏障已过 → 按无持有者落点)。*2026-09-20 复核:死节点的 node_id 被新进程复用并重新注册后这条接管不再触发,见 §12.3 GO-3。*
- `EnterScene` 每次多一跳 `data_service` 同步 RPC(含同落点重连),`data_service` 不可用即拒绝进场景。压测时看 `EnterScene` 分阶段时延;若成瓶颈再议缓存(归属只在合服时变)。
- 两段多键 Lua 依赖三把 `player:{id}:*` 键在同一 Redis 实例。键名无 hash-tag,**不支持 Redis Cluster**(与 CZ-2 的"物理共享单库"契约一致)。db / scene_manager / C++ scene 必须指向同一个 Redis。
- C++ 三个归属计数只进 30s 一行的 `[OwnerEpoch]` WARN,没进 Prometheus,`stress_summarize.ps1` 也不解析;C++ scene 侧新逻辑无单测(redis client 有)。
- 路由发送失败会把 epoch 退回旧值(非单调)。残余窗口:kafka-go 报错但 broker 实际已投递 → 目标节点拿着被收回的值载入,其存盘被拒后自毁,location 已指回源节点,玩家重连恢复,不会双主。*2026-09-20 复核:这段只分析了 Redis CAS,漏了 DBTask —— 被收回的 N+1 会反过来毒化 db 的 applied_epoch 守卫,见 §12.3 GO-2。*
- 阶段 2 契约缺口(不在本阶段):login `EnterGame` 识别票据 `target_zone_id`、gate 验签绑定 `player_id`、`TravelToZone` RPC 与 tip 码、客户端调用点。

## 11. 阶段 2 / 3 落码记录(2026-09-18)

做法:先用 7 个只读 reader 并行摸底(客户端 RPC 入口 / tip 码 / gate 验票与 login / player_migrate / 跨节点换图 / robot / Unity),再按摸底切包落码。用户明确要求「不等 Codex、不编译、直接进 main」,所以本节全部内容**未经编译器与测试验证**。

### 11.1 阶段 2:意图入口与访客识别

| 项 | 落点 | 要点 |
|---|---|---|
| 客户端 RPC | `proto/scene/player_scene.proto`:`SceneSceneClientPlayer.TravelToZone(TravelToZoneRequest{target_zone_id, scene_config_id}) → TravelToZoneResponse{error_message}` | 应答无错 = **已受理**(已冻结、已开始存盘),不代表到达;到达 = 之后收到 msg 124;受理后未成 = 之后收到 `SendTipToClient`,scene 已解冻。*2026-09-21 扩展(§12.5.6):受理后未成且无法在原地恢复(epoch 已被推进、玩家已不在本节点)时,`SendTipToClient` 之后紧跟踢线 34,实体销毁而不是解冻* |
| tip 码 | `data/tip/Tip.xlsx` scene_error 段尾 4 行:`ZoneTravelTargetZoneNotFound / ZoneTravelInBattle / ZoneTravelInTeam / ZoneTravelTargetBusy` | 代码只按枚举名 `kZoneTravel*` 引用,不写数字;fault 列留空 |
| scene 入口 | `player_scene_handler.cpp` 的 `TravelToZone` 方法桩(守护段内,只委托)→ `PlayerLifecycleSystem::RequestZoneTravel` → 唯一入口 `StartTravelHandoff` | CZ-6 校验先于任何状态修改:目标 0 / 本 zone → NotFound;战斗或备战 → InBattle;`TeamId.team_id != 0` → InTeam;已冻结 / 已有交接 / 普通换图应答未回 → 既有的 `kSceneTransferInProgress` / `kEnterSceneChangingScene` |
| 既有缺陷顺带修 | `EnterScene` handler 里 7 处 `response->mutable_error_message()->set_id(...)` 改为写 TLS tip | 原写法会被 `TRANSFER_ERROR_MESSAGE` 宏整体覆盖成空,客户端一直把拒绝当成功 |
| 访客识别(CZ-8) | gate `DispatchTokenVerify` 把票据的 `player_id / target_zone_id` 存进 `SessionInfo` → `SessionDetails.ticket_player_id=5 / ticket_target_zone_id=6` → login `EnterGame` | 规则 1:持票者 ≠ 进游戏的玩家 → 拒(在写任何会话状态之前);规则 2:`target_zone_id == 本 zone` → 不按 home_zone 弹回、`sceneID` 清零。0 = 普通票据,一律放行。直连与路由两种模式统一走 `BuildSessionDetails`(原先直连模式是内联手写的另一份)。gate 另加一道:重定向票据的 `target_zone_id` ≠ 本 zone 直接拒(gate_node_id 只在 zone 内唯一) |
| 目标地图到第二条腿 | `PlayerLocation.pending_scene_conf_id = 6` | 第一条腿放行时随「等待落点」写入,第二条腿(目标 zone 的 login 发来的 EnterScene 不带地图)由 scene_manager 自己读;不经票据 / login / 客户端转手 |
| 客户端(独立仓 mmorpg-client) | `GameClient.cs`(`_travelPending`、`OnServerTip` 事件、124 与断线清状态、超时收口)、新文件 `Game/WorldTravel/ZoneTravelClient.cs`(引用未生成符号的代码全关在这一个文件里)、`DevAutoPilot.cs`(`-travelZone / -travelScene / -quitOnTravelEnd`,断言 gate 地址变了才 PASS)、`CityTravelUiRoot.cs`(订阅 `OnServerTip`,被拒的换图立刻收场)、`tools/gen_messageids.ps1` 白名单 | 玩家主动传送时 `_redirectHops` 归零(否则同一会话第 4 次传送被 3 跳熔断) |

### 11.2 顺带打通:生产配置下客户端发起的跨节点换图

`AllowUnsafeCrossNodeHandoff=false` 时,跨节点换图原先被 scene_manager 以 18(`ErrHandoffPending`)拒绝。做法是**被动式**:平时路径完全不动,只有收到 18 才进入新代码——

```
客户端 EnterSceneC2S → scene 记 PlayerSceneChangeInFlightComp → scene_manager.EnterScene
  ← 18  → StartTravelHandoff(本 zone, scene_id, scene_conf_id):冻结 → 存盘 → 落地写 handoff 标记 → 重发同一个 EnterScene
  ← 0 且无 redirect → ResolveTravelOutcome(replyWasSuccess):先 DEL 标记再 GET owner_epoch
        epoch 变了   = 已放行到别的节点 → 源实体不存盘销毁(盘上已是最新)
        epoch 没变   = scene_manager 重新挑频道时落回了本节点 → 静默解冻,不发失败 tip
```

配套:`HandleReleasePlayer` 对交接中的实体直接返回(去留由应答与看门狗裁决;绝不能在这里 DEL 标记,ReleasePlayer 可能早于铸造 Lua 到达);`PlayerEnterGameNode` 发现「实体在交接中且路由带来的 epoch 更大」时先按被废黜销毁再走异步加载,不复用过期内存态(堵「应答丢失后 30s 内回到原节点 → 回档」);冻结期间丢弃位置上报;scene_manager 侧 18 的日志由 Errorf 降为 Infof(它现在是跨节点换图的常规第一跳,异常要看 `handoff_pending` 指标的速率)。18 在 C++ 的唯一定义是 `PlayerLifecycleSystem::kSmErrHandoffPending`,改 Go 侧编号必须同步它。

预期日志序列:scene_manager「[Handoff] … 暂拒」→ scene「needs a cross-node handoff」→「[ZoneTravel] handoff started … (same-zone cross-node)」→「requested EnterScene」→「same-zone handoff granted」→「[scene_handoff_granted] … destroyed without persisting」;全程 `[OwnerEpoch] stale_owner_write_rejected` 恒 0。

### 11.3 阶段 3:player_migrate 搬数据链下线(CZ-1)

摸底证实它是运行期死代码:唯一入口靠 `try_get<ChangeSceneInfoComp>` 触发,而全仓没有任何地方 emplace 它,也没有任何写 `to_zone_id / is_cross_zone` 的地方。

- 删:`HandleCrossZoneTransfer / HandlePlayerMigration / HandlePlayerMigrationAck`、`cross_zone_reaper.{h,cpp}`、`kafka/system/kafka.{h,cpp}`、`main.cpp` 的两个 topic 订阅与 reaper 启停、`kPlayerMigrateEventName`,以及 CMake / vcxproj / filters 里对应的行。
- 留:`PlayerFrozenComp`(字段一个没动)、`IsCrossZoneFrozen()` 与约 20 处业务拦写闸——传送链复用同一道闸。
- 两处「等 ACK 或 reaper」的 Frozen 悬挂分支同步处理:没人会再来解它,改为「记 ERROR 后解冻继续退出,绝不悬挂」。
- proto 走保守路线:`player_migration_event.proto` 与 `scene_comp.proto` 的 11/12/13 只打 DEPRECATED 注释、**不删**——生成物(`rpc_event_registry.cpp`、空壳 event handler)还引用它们,真删要和一轮 regen 同批做,清单写在两个 proto 的注释里。
- robot:新模式 `travel-smoke`(`robot/travel_smoke_scenario.go` + `etc/travel_smoke.yaml`;依赖未生成符号的代码隔离在 `travel_smoke_wire.go`)。严格重登(按 player_id 选角、绝不自动建角),每一跳断言 player_id 不变 / 票据 zone / gate 地址变化 / 金币连续性(出发值 → 访客区 +7 → 回家仍是 +7),防「被弹回家的假绿」。
- broker 上残留的 consumer group `scene-cross-zone-{nodeId}`、topic `player_migrate(_ack)`、Redis 的 `player_migration:*`(TTL 120s)无害,不写清理代码。

### 11.4 生成物与验证状态

- 阶段 1 的 6 处 proto 字段、CZ-8 的 `SessionDetails` 票据字段:**已有 Go / C++ 生成物**(2026-09-18 03:52 回合制战斗会话跑 regen 时一并生成)。
- 本节新增的三样——`PlayerLocation.pending_scene_conf_id`、`TravelToZone`(消息、消息号、handler 分发)、4 个 `kZoneTravel*` tip——**已生成**(2026-09-18 09:06–09:11,提交 `670c69bad`;多会话串行协调,同轮带上了聚宝斋「通用资产通道」的源)。实际发号:tip 3024–3027;`SceneSceneClientPlayerTravelToZone = 226`(224 / 225 / 227 归聚宝斋的三个 Asset RPC);`kMaxRpcMethodCount = 228`。护栏逐条结果见 PROGRESS.md 同日条目:message_id 只增不减、Agones 正向断言命中、`player_scene_handler.{h,cpp}` 重生成后零 diff(手写桩与生成器逐字一致)。robot/vendor 已同步;客户端仓的 `MessageIds.TravelToZone` 与 C# proto 已生成(分支 `codex/guild-team-roster-v12`,提交 `fffd0da`)。
- 已知残余风险(均不致双主,去留始终由 owner_epoch 比对裁决):应答只回显 player_id,5s 之后才到的迟到应答可能记到下一条请求头上;`TeamId` 是 scene 从 Redis 投影来的缓存,刚入队未刷新时拦不住;副本节点上也允许发起跨 zone 传送(未做节点类型校验);交接在途时另一设备顶号落回同一节点的约 1 秒窗口。
- 验证清单在 §9 基础上追加:同 zone 起 2 个 scene 节点 + `AllowUnsafeCrossNodeHandoff=false` 跑换图,核对 §11.2 的日志序列;双 zone 跑 `robot -c etc/travel_smoke.yaml` 期望 `TRAVEL_SMOKE_OK`;`robot login-test` 在「EnterSceneC2S 拒绝码真的回到客户端」之后仍应 23/23。

### 11.5 单节点硬崩后的玩家接管(2026-09-19)

问题:换手门只认 handoff 标记,而硬崩的节点永远写不出标记。zone 里还有别的活节点时(整 zone 下线走 `playerLocationOwnerGone`),它名下的玩家在生产配置下会被 18 永久挡在门外,只能等运维手工清 location。

规则(`enterscenelogic.go playerLocationOwnerDead`):位置记录指向的节点**同时**满足下面几条,才把这条位置记录当作不存在,之后走首次落点(一定铸造新 epoch):

| 条件 | 为什么 |
|---|---|
| (zone,node) 身份无歧义 | node_id 会被复用,两代进程并存时不下结论 |
| 本进程已完成首次 etcd 全量同步,且节点**已从注册表(`knownNodes`)消失** | 死亡要正面证据。只看 Redis 负载集不够:成员资格由负载上报周期刷新,缺席只说明「这一刻没看到它」,而写入负载集的路径未必写 `death_at`——「没有 `death_at`」的语义恰恰是放行。若据此接管,一个只是暂时缺席的活节点名下正在玩的玩家,下一次换图会被直接派走、读到最长一个存盘周期前的旧档(对抗复审的 P0,两个视角各自独立命中) |
| 本副本亲眼看到它消失已超过一个屏障时长(`nodeGoneObservedAt`,进程本地) | `death_at` 只有 leader 写;leader 缺位时节点丢租约就没人写,而「没有 death_at」的语义是放行 |
| 不在负载集 | 在负载集 = 活着 |
| Redis 的 `death_at` 屏障已过(读失败 / 值非法 → 不放行) | 盖过 C++ 老进程 15s 的紧急疏散存盘窗口 |

任何一步拿不准都 fail-closed(沿用 18 的可重试拒绝)。进程已死但 etcd 租约未到期的那段时间里请求照旧被 18 暂拒,租约到期 + 屏障走完即放行,总锁定时长约「租约 TTL + 20s」。「其实没死的僵尸」由 owner_epoch CAS 兜底:接管铸了新 epoch,它之后的存盘被 C++ Lua 拒绝并自毁,DBTask 被 db 的 applied-epoch 守卫拒绝。崩溃固有的代价不变:没来得及落盘的那段进度丢失。

配套:

- `load_reporter.go` 周期巡检把「先摘负载集、后写 death_at」改成与 `removeNodeFromRedis` 同序(先写后摘)——反序窗口里并发的 EnterScene 会看到「已死 + 无 death_at ⇒ 屏障已过」。
- 接管落点**成功之后**把玩家在旧场景占的人数还回去(`releaseTakenOverSceneCount`):dead-node reconcile 只销毁副本实例,大世界频道沿用同一个 scene_id 迁走、人数原值保留、全仓没有重算路径,不还就永久虚高。旧场景已被销毁(`scene:{id}:node` 不在)时不动,避免把计数键重新建出来。
- 指标 `scene_manager_enter_scene_owner_dead_takeover_total{zone_id}`:稳态恒 0;没有节点死亡它却在涨 = 判死出了问题。
- 相关但独立:`world_init.go` 旧的 `markNodeDead` 会在一次 CreateScene RPC 超时(5s)后就 ZREM 负载集且不写 `death_at`,已被 `markNodeUnreachable` 取代(`ba2337b0d`):只在本轮候选里拿掉该节点、丢掉连接缓存,**不碰负载集也不碰场景归属**。本规则不依赖那次修复——负载集缺席在任何原因下都不单独构成死亡证据,写在这里只是为了避免再按旧行为推理。

单测 8 个(`owner_epoch_test.go` 末尾):屏障已过接管并铸造、屏障内仍拒、仍在注册表不接管、未完成首次同步不接管、身份歧义 fail-closed、本地观察到的消失受屏障约束、接管后归还旧场景人数、不为已销毁场景重建计数键。**均未运行。**


## 12. 交接收尾(2026-09-20):终审存活项的落码与已知限制

上一会话(另一台机器)终审后额度用尽,本节由接手会话补完。做法:先按上一会话留下的条目在本机代码上逐条复核,再跑一轮只读审计(Go / C++ 各一路,每条发现派一名反驳者沿代码走通失败时序,9 条里 8 条存活、其中 5 条被下调严重度),然后只落码**不需要改协议、改动面小、能配单测**的三处,其余写成已知限制。**本节所有代码未编译、未跑测试**(AGENTS §10.1)。

### 12.1 本轮落码

| 项 | 问题 | 改动 |
|---|---|---|
| A. handoff 标记撤回不再只靠 TTL | `FinishExitAfterPersist` 的"退出优先"分支与 `AbortTravelHandoff` 撤回标记用的是被 `redis->connected()` 守着的空回调 DEL:Redis 抖动时静默跳过,标记带着当前 epoch 残活 300s。**复核后的准确触发面**:① 断线退出后 30s 租约内重连回同一节点(同落点不铸造,epoch 仍是 E),之后 300s 内第一次跨节点换图凭旧标记免存盘过门 → 回档;② 更糟的在线变体——`AbortTravelHandoff` 的 DEL 被跳过后玩家就地解冻继续玩,不用重登就能命中。正常登出 / 租约到期后重登**不**受影响(location 已清,首次落点必铸新 epoch,旧标记自然失配) | 新增纯头文件 `handoff_mark_withdraw.h`(待撤回表 + 按标记原文的条件删除 Lua);两个撤回点统一走 `WithdrawHandoffMark`:先登记后发命令,同时检查 `connected()` 与 `command()` 返回值,只有整数应答才销账;`RedisSystem` 的重连回调与 1s 定时器重试,截止时刻 = 单调时钟上的"标记 TTL + 5s";未确认期间 `IsSceneChangeBusy` 对该玩家返回 true。`PlayerTravelHandoffComp.markEpoch` 只在 SET 派发成功后才写、SET 收到 ERROR 应答时清零,保证"确定没写的标记"不登记。计数 `withdraw_deferred` / `withdraw_expired` 进 `[TravelHandoff]` 汇总行。单测 7 个(`cross_zone_test` 的 `HandoffMarkWithdrawQueue.*`) |
| B. 未映射玩家跨 zone 传送会把访客存盘写进目标 zone 的库(审计 GO-1,违反 §6 不变量 2) | `resolveHomeZone` 把"未映射 / 映射为 0"一律当首登、回落 `GateZoneId`;第二条腿的 gate zone 就是目标 zone,第一条腿完全不查归属。全程只有一条 INFO | 第一条腿(`leavingZone` 为真)在过门、铸 epoch、写等待落点**之前**先查归属,未映射 / 查询失败 → 20,源 scene 据此解冻回 tip;第二条腿(`awaitingPlacement`)未映射同样回 20(纵深防御)。"未映射 = 首登"只保留给无位置记录的首次落点与同 zone 已有位置的换图。新 reason `home_zone_unmapped_travel` + 告警 `SceneManagerHomeZoneUnmappedTravel`。单测 7 组 |
| C. gate 对"同 scene_id、不同节点"的路由不转发进场(审计 CPP-1) | `ApplyRoute` 只按 `pendingEnterGsType` 与 scene_id 是否变化决定转发;world rebalance 迁频道沿用原 scene_id,规划与执行之间的竞态下玩家会落到"会话指向新节点、新旧节点上都没有实体",只能重登 | `RebindSceneNode` 把"比较旧指向 + 覆盖新指向"收进一个函数并返回 `SceneNodeChange`;节点变了也以 `enterType=0` 转发。旧指向无效(节点被摘除过)一律算换了节点。单测 `SceneRouteEntry` 共 16 个 |
| D. 告警与指标 | `SceneManagerEnterSceneRejectedSpike` 钉在无人发射的 `reason="scene_gone"`,恒空且 Prometheus 不报错;**另查出两条 critical 的 `*PoolEmpty` 同样恒空**(`nodes_by_role` 这个 gauge 从不发布 0 值,`== 0` 永远匹配不到,两条 pager 从未响过) | 按真实发射的 reason 拆成 5 条告警;`PoolEmpty` 改成 `count … unless on (zone_id) count …`;恢复 destroy-while-entering 分支的 `scene_gone` 发射点并配回原告警;`metrics.go` 的 Help / 注释与发射点对齐。现共 17 条规则,**未经 promtool 校验** |
| F. **scene 从不装 SceneManager 的 gRPC 应答处理器**(端到端闭环核查发现的 P0,详见 §12.5.1) | `InitSceneManagerReply()` 无任何调用点,`AsyncSceneManagerEnterScene/CreateSceneHandler` 恒为空,生成客户端静默跳过每一条应答。后果:同 zone 跨节点换图彻底失效(18 到不了 handler,玩家卡住零提示)、副本 / 镜像场景进入完全失效、跨 zone 传送源实体要冻满 30s、失败 tip 迟到 30s。**与本轮新代码无关,2026-04-16 的一次 regen 丢的** | 在 `cpp/nodes/scene/main.cpp` 装(生成物不能改,且生成器永远不会写它——谓词只收 `cc_generic_services`,scene_manager 是 grpc)。形状照 `id_segment_bootstrap.cpp` 的 `InitDataServiceReply()` |
| E. 文档 | `docs/ops/cross-zone-failure-test-runbook.md` 写着"每个版本上线前必跑",四个场景却全在测已删除的 reaper 链;`enter-scene-zone-routing.md` 教运维合服时手工清 location | runbook 重写为 v2(面向现行链路的 A–F 场景,观测点逐个 grep 核对,文件头声明"静态编写、未实跑");路由文档逐句核对改写,新增"合服后残留 location"小节;约 20 份旧设计文档与 12 个代码文件里把已删行为当现状的文字加了状态标注 / 改成实情(只动注释) |

### 12.2 新增的上线前置

- **对存量号开放跨 zone 传送之前,必须先跑完 `tools/merge_zone -backfill-home-zone`。** 12.1-B 之后,没有 `player:zone` 映射的玩家发起传送会被 20 拒绝(tip 后解冻,不受损)。新建角色由 `createplayerlogic.go` fail-closed 登记,不受影响;受影响的只有 2026-09-08 建角登记修复之前的号,以及映射 Redis 丢失的情形。
- dev 旁路 `AllowGateZoneAsHomeZone=true` 会绕过上述全部检查,在第二条腿上把访客记成目标 zone 归属,**只许单 zone 本地联调用**。
- **(2026-09-21 新增)K8s 的 scene-manager ConfigMap 必须配 zrpc `Timeout` ≥ `KafkaWriteTimeoutSeconds` + 回滚余量(例如 `Timeout: 8000`),或用 `MethodTimeouts` 只放宽 `EnterScene`。** 现状:本地 `go/scene_manager/etc/scene_manager_service.yaml:3` 是 `Timeout: 5000`;`tools/scripts/k8s_deploy.ps1` 生成的 scene-manager ConfigMap 没写 `Timeout`,落到 go-zero 默认 2000ms。两者都不大于 `KafkaWriteTimeoutSeconds: 5`(同一 yaml `:32`)。zrpc 服务端超时后调用方拿到 DeadlineExceeded,C++ 生成的 gRPC 客户端对非 OK 状态只打日志、不回调(`cpp/generated/grpc_client/scene_manager/scene_manager_service_grpc_client.cpp:141-153`),所以**任何超过 2s 的第一条腿**都会让源 scene 冻满 30s 应答看门狗;若此时 gate(A) 对 gate-cmd 的消费又滞后超过 30s,一次"迟到但成功"的传送会被 §12.5.6 的踢线抢先踢回选服。`tools/scripts/k8s_deploy.ps1` 由有权限的会话去改,**本轮未改**。

### 12.3 审计存活、本轮未修(已知限制)

| # | 严重度 | 限制 | 为什么没修 / 修法 |
|---|---|---|---|
| GO-2 | P2 | **路由失败回滚让 owner_epoch 回退(N+1→N),会反过来毒化 db 守卫。** `routePlayerToGate` 报错但 broker 实际已投递时:目标 B 拿着被收回的 N+1 载入,首次存盘 Redis CAS 被拒并自毁——但 C++ 存盘是先发 CAS、紧接着**无条件**发 DBTask(N+1),不等 CAS 结果;db 判 advance 落库并把 `applied_epoch` 记成 N+1(只升不降),此后合法持有者 A 的每笔 DBTask(N) 都被判 stale、ACK 丢弃。MySQL 停在 B 的快照上,与 Redis 分叉,直到下一次铸造(再铸得的 N+1 与 applied 相等 → 自愈)。每笔丢弃都有 `STALE-OWNER-WRITE rejected` 日志与计数,不是静默;真回档还需叠加 Redis 丢数据或有工具直读 MySQL | 根治要让 epoch 严格单调:回滚时不 SET 回旧值而是再 INCR 一次,并经 `EnterSceneResponse` 新增的 epoch 回显字段交给源 scene 采纳——要改 proto + regen + 两端。次选:C++ 在 CAS 成功回调之后才发 DBTask。现有回滚单测断言"退回旧值",要随之改 |
| GO-3 | P2 | **死节点的 node_id 被新进程复用并重新注册后,§11.5 的接管永不触发。** `playerLocationOwnerDead` 要求"已从注册表消失",而 `PlayerLocation` 只记 node_id、不记进程实例;C++ `NodeAllocator` 取最小空号,复用是常态。此后该节点名下的遗留 location 只要解析到别的节点就回 18,指标只记 `no_marker`(不告警)。**不是永久卡死**:玩家断线约 31s 不重登,player_locator 的租约到期会调 `LeaveScene` 无条件删 location;真正卡住的是间隔 <30s 持续重试的客户端(每次 ShortReconnect 都撤销租约) | 需要在 `PlayerLocation` 里记属主进程实例标识(etcd 注册的 uuid / create_revision),属 proto 变更;用"注册时刻晚于 location.UpdateTime"做过渡判据会在 scene_manager 重启首次 fullSync 时误判活节点,不可取。*2026-09-21:断线释放标记(§12.6 的 A′)能在 300s 内缓解"干净退出后同号重注册"这一类,根因(location 不记进程实例)不变;见 §12.6* |
| GO-5 | P2 | **login 恒发 `ZoneId = GateZoneId = 本 zone`,§10.2 的 R7(只送连接)/ R8(等待落点牵引)从 login 路径不可达。** 访客在 B 区掉线后从 A 区入口重登:请求 `{ZoneId=A}`、location 在 B → 同 zone 落点 + 换手门 → 18。此时 B 的源实体早已在断线当刻存盘销毁,挡路的只是没人清的 location;只要客户端还挂在 gate A 上重试,`Reconnect` 就一直撤销断线租约,没人来删。同 zone 多节点下断线重登也有同类问题(`ReserveBestWorldChannelForEnter` 没有玩家亲和)。对应单测用的是不带 ZoneId 的请求形状,与生产脱节 | 二选一:login 在无显式去向时发 `ZoneId=0`;或 scene_manager 在 `ZoneId==GateZoneId` 且 location 在别 zone 时按 R7a 处理。`go/login` 本轮有别的会话在改,未动。*2026-09-21 更正:"B 的源实体早已在断线当刻存盘销毁"多半不成立——真写盘的断线退出会留下僵尸实体(Z1);"卡住"的根因修复(Z1 + 断线释放标记)见 §12.6,待决、未落码*。**2026-09-21:已落码、待验证** —— 用户拍板"重连窗口内回原处,窗口外或主动登出回家",login 侧发 `ZoneId=0`、scene_manager 只跟随落在具体节点上的 location(§12.7);卡在 18 的根因修复(Z1 + A′)见 §12.6 / §12.6.10。C++ 未重新编译,Go 未编译,均未测试 |
| CPP-2 | P2 | **RoutePlayerEvent 在 gate 侧丢失后没有任何补发。** scene_manager 以 Kafka ACK 为提交点就回成功,源 scene 据此不存盘销毁实体;gate 若找不到目标节点(打 ERROR 后 return),或目标节点的 TCP 正在重连(`RpcClient::CallRemoteMethod` 静默丢弃而 `ForwardPlayerToScene` 仍返回 true),玩家在线但无实体、无提示。登录链路同样存在,非传送特有 | 需要"未连接如实报失败 + 有上限的补发 + 超限发 `kEnterSceneFailed` 并断开"的完整设计 |
| CPP-3 | P2 | **疏散 / 排空的改派 EnterScene 是 fire-and-forget**:票据发送前就删、发完立刻摘会话销毁实体;被拒(无可用节点 / 屏障未过 / data_service 不可用 / Kafka 失败)或无应答时,gate 会话还连着却指向尸体,没有 tip 也不踢线。换手门与归属查询上线后拒绝面变大了 | 需要"待确认表 + TTL + 用票据里的 sessionId 发 tip 并踢线",且整节点疏散时要并入 15s 收敛谓词 |
| 冻结上限 | P2 | 上一会话报的"交接冻结没有服务端上限"**大部分不成立**:已有存盘 / 应答两道 30s 看门狗,应答丢失、scene_manager 卡住、存盘回调不来都有界(最坏约 60s < 客户端 75s)。真正无上限的只有三段,共同前提是 zone Redis 不可用或半开:`ResolveTravelOutcome` 每 30s 无限重挂(U1);SET 已发但回调永不来(U2);DEL+GET 已发但回调永不来(U3)。另:看门狗计时用的是墙钟(muduo `runAfter`) | 已有完整设计(70s 单调时钟硬上限 + 晚发闸 + "标记已写则销毁不解冻"),但到期后要给客户端发踢线消息,**客户端收到后是否一定断线重登未确认**,且 scene 无法强制 gate 断开(无此 RPC)。需客户端侧确认后再落 |
| 标记残留(12.1-A 的残余) | P3 | `IsSceneChangeBusy` 这道闸只管得住**本节点**替在线玩家发出的 EnterScene;不经本节点的跨节点落点(断线重登被挑到别的节点)在撤回确认前仍只有 TTL 兜底。源实体已销毁时盘上就是最终态,无害;有害的只剩 Abort 后就地解冻、又被顶号挑到别节点的极窄窗口 | 根治在 scene_manager 侧:普通 EnterScene 永不凭标记放行,改由请求显式"出示标记"(proto 变更) |
| GO-6 | P3 | `markNodeDeath` 写 `death_at` 失败后仍无条件 ZREM 负载集,而缺键是放行语义、进程首次 fullSync 又不产生本地观察记录 → 该节点整道再入屏障失效(玩家接管、孤儿副本销毁、世界频道改派同时放行)。需一次只打中 Setex 的 Redis 抖动 | "写失败就不 ZREM"是错的修法:fullSync 不是周期任务,死节点会永远留在负载集。要显式重试:fullSync 路径返回 error 走 3s 重试;watch DELETE 路径入队由 5s ticker 补写 |
| GO-4 残余 | P3 | `LeaveScene` 是非原子的 GET→比 SceneId→裸 DEL。审计给的主时序被推翻(租约到期路径有 cleanup-pending 屏障 + 单测守着),只剩 MarkOffline 路径上毫秒级、需双端并发的窗口 | 改成 Lua compare-and-delete 即可,防御性加固 |
| 告警盲区 | P3 | zone 内 scene 节点**全灭**时两条 `PoolEmpty` 仍无从触发(没有任何 `nodes_by_role` 序列可供比较);`handoff_pending_no_marker` 刻意未配告警(它是换图正常第一跳),因此 GO-3 / GO-5 那种"只拒不放"目前没有告警能抓;新告警阈值均无实测基线 | 前者需 Go 侧为已知 zone 发布 0 值;后者可用它对 `enter_scene_stage_seconds_count{stage="update_loc"}` 的比值,口径待定 |
| Hiredis 泄漏 | P3 | `muduo_windows/.../Hiredis.cc` 在 `redisvAsyncCommand` 返回 ERR 时不释放刚 `new` 的 `CommandCallback`。既有问题,12.1-A 让"断开期间发命令"的路径多了一条 | 返回 ERR 时 delete 即可,另案 |

观测缺口(runbook v2 §6 的 G1–G10)不在此重复,其中影响验收的两条:C++ 的交接计数没进 Prometheus、`stress_summarize.ps1` 也不解析;travel-smoke 没有"停在交接窗口 / 在窗口内退出"的开关,B / C / F 场景的时序全靠外部撑窗口。

### 12.4 验证清单(在 §9 / §11.4 基础上追加,全部未执行)

C++ MSBuild 必须串行 `/m:1 /nr:false`;这批代码从未编译,失败时先按行号区分是不是本节引入的。

1. `go/scene_manager`:`gofmt -l internal/logic internal/metrics` 无输出 → `go build ./...` → `go vet ./internal/logic/... ./internal/metrics/...` → `go test ./internal/logic/ -run "TestEnterScene_|TestAwaitingPlacementExpired|TestCheckHandoffCommitted|TestPlayerLocationOwner" -count=1` → `go test ./... -count=1`。重点 7 个:`TestEnterScene_TravelFirstLegRejectsUnmappedHomeZoneWithoutSideEffects`(4 子用例)、`…TravelFirstLegWithMappedHomeZoneStillReleases`、`…RedirectOnlyFirstLandingDoesNotQueryHomeZone`、`…TravelSecondLegRejectsUnmappedHomeZone`(2 子用例)、`…HomeZoneUnmappedSameZoneSceneSwitchStillFallsBackToGateZone`、`…ExpiredAwaitingPlacementStillRejectsUnmappedHomeZone`、`…SceneDestroyedWhileEnteringIsCountedAsSceneGone`(失败时区分"计数差值 ≠ 1"与"`Gather()` 报错")。
2. `cpp/libs/services/scene/scene.vcxproj` → scene 节点 → `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone`:`HandoffMarkWithdrawQueue.*` 7 个全绿,原有 `CrossZoneBagMarshal.*` / `CrossZoneFrozen.*` 不变。`cross_zone_test` 自 09-05 后没重跑过;exe 无输出且退出码 0xC0000135 = 缺 DLL。
3. `cpp/tests/routing_identity_test/routing_identity_test.vcxproj` → 运行 `--gtest_filter=SceneRouteEntry.*`(16 个)→ 全量 → `cpp/nodes/gate/gate.vcxproj` 0 error。重点看 `scene_route_helper.h` 新增的 `#include "proto/common/base/node.pb.h"` 在两个工程下能否解析。
4. `promtool check rules`(先取出 `deploy/k8s/scene-manager-alerts.yaml` 的 `spec.groups`):17 条,无语法错误。本机没有 promtool 就如实记录。
5. 故障注入(本地 dev):交接冻结态下 `redis-cli CLIENT KILL TYPE normal` 再断开客户端 → 期望先一条 `[ZoneTravel][WithdrawMark] deferred`、重连后 `withdrawn`,`[TravelHandoff]` 行 `withdraw_expired=0`,`GET player:{id}:handoff` 为 nil。其余场景按 runbook v2。
6. **§12.5.1 的 P0 修复必须实跑验证**(它激活了约 150 行从未执行过的代码):同 zone 起 2 个 scene 节点 + `AllowUnsafeCrossNodeHandoff=false` 跑一次跨节点换图,scene 日志应出现 `SceneManager.EnterScene deferred (handoff pending)` 并随后起交接重发(此前这条日志**从不出现**);再跑一次进副本 / 镜像场景,确认 `CreateScene` 应答后玩家真的被自动带进去。两者在修复前都是"请求发出、无任何后续"。
7. 失败时保留:首个 error 及前后 20 行 / 失败用例的完整 `-v` 输出;C1041 / LNK1104 先排除并发构建;不连续重试、不注释断言。

### 12.5 端到端闭环核查(2026-09-20):发现链路从一开始就断在 scene 的应答装配上

用户问"整个流程闭环没有"。把链路拆成 8 段(客户端发包 → gate 白名单/路由 → scene 受理 → 写标记发请求 → SM 第一条腿 → 重定向出站 → gate(B) 验票 + login 识别访客 → 第二条腿 + 路由 + 目标节点加载 → 回家 → 失败分支收口),每段一名只读 agent 沿"上一段的出口 = 下一段的入口"走通,报出的每条断点再派一名反驳者。结论:**不闭环**,而且断点不在新写的那些环节上。

#### 12.5.1 P0(已修):scene 节点从不装 SceneManager 的 gRPC 应答处理器

- **事实**:`InitSceneManagerReply()`(`cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp:15`)是全仓唯一给 `AsyncSceneManagerEnterSceneHandler`(:17)与 `AsyncSceneManagerCreateSceneHandler`(:62)赋值的地方,**它没有任何调用点**。scene 的 `InitReply()`(`rpc_replies/register_response_handler.cpp`)只调 `InitGateReply()`;scene 也从不调生成客户端的兜底装配入口(`SetIfEmptyHandler` 全仓唯一调用方是 `cpp/nodes/gate/main.cpp:280`)。于是两个全局 `std::function` 恒为空,生成客户端的 `if (AsyncSceneManagerEnterSceneHandler)`(`cpp/generated/grpc_client/scene_manager/scene_manager_service_grpc_client.cpp:144-147`)**静默跳过每一条应答**——不报错、不计数、不打日志。该 .cpp 确实编进了 scene 节点(`CMakeLists.txt:117`、`scene.vcxproj:236`),所以不是"文件没进构建"这类更轻的解释。
- **怎么丢的**:`42bcbf05e`(2026-04-16)里这行调用还在;同日的 `f1b110bcc`("clear code")把它删了,之后再没加回来。根因不是手滑:`register_response_handler.cpp` 是 proto 生成器的产物(`WriteRepliedRegisterFile`,`tools/proto_generator/protogen/internal/generator/cpp/gen.go:362`),筛选谓词 `IsSceneNodeReceivedProtocolResponseHandler` 只收 `cc_generic_services`(muduo TCP RPC)的服务,而 scene_manager 在 `proto_gen.yaml` 里是 `rpc.type: grpc` —— **生成器永远不会把它写进去,手写加进那个文件下一次 regen 必然再被抹掉**。
- **后果**(这条一直是坏的,与本轮新代码无关):
  - **同 zone 跨节点换图彻底失效**:scene_manager 的 18(`ErrHandoffPending`)到不了 `HandleTravelEnterSceneReply`,交接不会起、请求不会重发,`PlayerSceneChangeInFlightComp` 5s 后静默过期,玩家原地卡住且零提示。§11.2 整条被动式链路是死码。
  - **副本 / 镜像场景进入完全失效**:`RequestEnterMirrorScene`(`player_scene.cpp:199`)发完 `CreateScene` 后**完全依赖这个应答**来自动进场,全仓没有第二条路。
  - **跨 zone 传送**:重定向靠 Kafka 仍能送达客户端,但源实体要冻满 30s 应答看门狗才被销毁,期间是不可交互的冻结体;计数全部落在 `grantedWithoutReply` 而非 `granted`,§11.2 的预期日志序列不会出现。
  - **第一条腿被拒时**(目标图没开 / 归属未映射回 20 / 无可用 gate / epoch 冲突),tip 不再随应答立刻回,玩家要冻满 30s 才由看门狗解冻并弹"目标区繁忙"。
- **修法**:在 `cpp/nodes/scene/main.cpp` 装,**不碰生成物**(AGENTS §3)。形状照 `ConfigureGuidSegmentClients` 里的 `InitDataServiceReply()`(`id_segment_bootstrap.cpp:244`)——手写引导代码自己装自己的 gRPC 应答处理器。只是给两个全局 `std::function` 赋值,无前置依赖,放在发出任何请求之前即可。注释里写清"为什么不能放进生成的 `InitReply()`",避免下一个人又搬回去。
- **风险**:这一修同时**激活了约 150 行从未执行过的代码**(两个应答处理器的全部分支),首次编译与联调会第一次真正走到它们。

#### 12.5.2 传送后半程失败时服务端对客户端零通知(4 条;S5-B1 / S8-1 已于 §12.5.5 修复,S7-1 与 S3L1-1 已于 §12.5.6 落码,均未编译)

四条审计发现指向同一个根:**第一条腿失败有出口(源 scene 解冻 + tip,§10.2 R5),第二条腿及其之后没有对等机制**,而那时源实体已经销毁。

| # | 触发点 | 说明 |
|---|---|---|
| S5-B1 / S8-1 | `entergamelogic.go:310-321` | login 的 `EnterGame` gRPC 在提交预加载后就同步回成功,真正的 `SceneManager.EnterScene` 在异步链里;被拒时唯一处置是 `logx.Errorf` + `return`,刻意跳过 `cleanupLoginSessionState`("把登录会话留给客户端重试")。login 全仓没有任何 `PushToPlayer` / `SendTipToClient`,唯一的客户端通知原语 `KickSessionOnGate` 只服务 `ReplaceLogin`;`kEnterSceneFailed`(3023)的发射点全在 scene 节点,而此刻没有 scene 参与,该 tip 通道不可达。gate 侧也没有"已绑会话但迟迟没进场"的看门狗。**玩家表现**(2026-09-20 客户端核查后更正):连上 gate(B)、`EnterGame` 回成功,然后停在"正在进入游戏…",**不会永远卡住**——客户端等 `NotifyEnterScene` 有 60s 硬上限(`GameClient.cs:645`),到期 `FailPipeline` 关连接、打回选服界面;但玩家看不到失败原因(见 §12.5.4 CL-1)。服务端侧仍是无 tip 无断线。可拒码含 1 / 7 / 8 / 17 / 18 / 19 / 20。其中 **20(归属未映射)在等待落点有效期内每次重试都会复现**,只能等 300s 票据过期回落 Offline-Return,或运维先跑回填。 |
| S7-1 | `enterscenelogic.go:358` | **已落码(§12.5.6 末尾,未编译)。**第一条腿的"目标地图在不在目标 zone 开着"只读预检被 `in.SceneConfId != 0` 挡住。而 `scene_config_id = 0` 是协议明文支持的形态(`player_scene.proto:81`:0 = 由目标 zone 挑默认大世界)。*(2026-09-20 客户端核查后更正:原文称它"也正是回家的典型形态",不成立——Unity 的地图窗把 `scene_config_id` 硬限在 1..4(`CityTravelUiRoot.cs:197`),回家也是"选归属区 + 选一张图";发 0 的只有 `DevAutoPilot` 与 robot。所以本条对真实玩家基本不可达,实际严重度 P3。)*传 0 时第一条腿只校验目标 zone 有没有 gate,不校验有没有任何世界频道;放行后源实体立刻销毁,第二条腿解析场景失败只回 `ErrNoAvailableNode`,再落进上面那条无通知路径。§4 写的"目标 zone 无可用 gate / 频道 → 回可重试错误 → 源 scene 解冻并回 tip"对 conf=0 不成立。 |
| S3L1-1 | `enterscenelogic.go:909-917`(工作树已移到 `:931-939`) | **已修(§12.5.6,未编译)。下面"双重故障才触发"的前提不准确**:主触发形态下源 scene 根本收不到 7(zrpc 超时不回调,只能等 30s 看门狗),且一次 Redis 读超时引发的铸造 EVAL 重放就能到达同一终态,详见 §12.5.6。原文:第一条腿 Kafka 推重定向失败后只有一层补偿:`rollbackPlayerPlacement` 回滚 location 与 epoch。该函数在 **Redis Eval 出错**时只记 Errorf 返回 false,epoch 停在 N+1 且无人重试;而应答里区分不出"已回滚 / 没回滚",源 scene 把 epoch 变化读成"已被放行",`DestroyDeposedPlayer` 不存盘销毁实体且全程不发任何客户端消息。双重故障才触发(Kafka 写失败 + Redis 回滚失败),但没有第二层出口。另一支(exact-value CAS 不过)销毁源实体本属正确,不算缺陷。 |

- **共同修法方向**(需要拍板,且 `go/login/**` 当前有别的会话在改,本轮未动):① 给 login 一条面向客户端的失败通道——最小做法是复用已有的 `KickSessionOnGate` 把会话踢掉,让客户端明确回到登录流程,而不是无声等待;② 或在 gate 侧加"已绑会话但 N 秒内没收到 RoutePlayerEvent 就踢"的看门狗(同时也能兜住 §12.3 的 CPP-2);③ S7-1 单独可低成本收敛:第一条腿在 `SceneConfId == 0` 时也做一次"目标 zone 有没有任何世界频道"的只读预检。
- 这组与 §12.3 的 CPP-2 / CPP-3 相邻但不同:CPP-2 是 `RoutePlayerEvent` 在 gate 丢失,CPP-3 是疏散改派 fire-and-forget,这里是 **EnterScene 被显式拒绝**且拒绝方是 login 异步链。

#### 12.5.3 判定为闭环、未发现断点的环节

以下每一跳都核对过接收方、字段透传与失败出口,证据链见工作流记录:协议契约与消息号 226 在 `message_id.txt` / C++ 生成物 / 10 个 Go 服务 / robot 之间一致且无重号;gate 的客户端白名单(`IsClientMessageId`)含 226,限速表缺省放行,按 `OptionFileDefaultNode = NODE_SCENE` 路由到 scene,应答经 `OnSceneProcessClientPlayerMessageReply` 原样回客户端;scene 侧 CZ-6 六条校验**每条拒绝都有 tip**(四个 `kZoneTravel*` 码 3024–3027 已发号、按枚举名引用);`StartTravelHandoff` 的两条存盘路径(写盘在途挂 30s 看门狗 / 脏数据快路径同步直调)都有接续,`BeginTravelHandoff` 与 `RequestTravelEnterScene` 的五条同步失败分支全部收口到 `AbortTravelHandoff`;第一条腿的换手门、归属前置检查、铸 epoch、签票据、写等待落点、Kafka 事件(含 `target_instance_id`)齐备;gate(A) 推 msg 124、gate(B) 验票绑定 `player_id` / `target_zone_id`、票据字段进 `SessionDetails`、第二条腿消费等待落点、`RoutePlayerEvent` 字段透传到 scene(B)、目标节点从共享 Redis 读档并按 `home_zone` 选 DBTask topic —— 均通。

被反驳者推翻、不作为缺陷记录的两条:① "只送连接的重定向也会让源 scene 无条件销毁实体"(票据语义两端其实一致);② "第二条腿读不到档时会把访客当新号建成空实体并覆盖原档"(读档失败有 fail-closed 保护)。另有一条 P2 时序问题(受理后同步失败时,失败 tip 会早于"已受理"应答到达,与 `player_scene.proto:74-76` 写的次序契约相反)被判 refuted,但它依赖客户端是否按"先收应答再开遮罩"实现,**归入客户端核对项**。

#### 12.5.4 客户端那半条链(2026-09-20,`mmorpg-client` @ `120e2d8`,只读核查)

客户端已推到远端(`main = 120e2d8`,比 09-14 的 `d2b165a` 多 26 个提交)。同样分段追踪 + 每条断点一名反驳者;其中"tip 码表"一段的 agent 因看不到用户授权而拒读客户端仓,由主会话手工补核。**Unity 实机未跑过**,以下全部是静态结论。

**主链判定为闭环**:

- 入口只有两处:地图窗选区(`CityTravelWindow.cs:107` → `CityTravelUiRoot.RequestZoneTravel`)与 `DevAutoPilot -travelZone`;没有"传送点"入口。`MessageIds.TravelToZone = 226`、`TravelToZoneRequest{target_zone_id=1, scene_config_id=2}`、应答 `error_message=1`、msg 23 / 34 / 79 / 124 的消息号与字段号,均与服务端 `proto/` 一致。应答不走 `HandlerRegistry`(全仓无人调 `Register`,生成的 handler 是空壳),走 `GameClient.Call` 的 `_pending` 回调,有效。
- `_travelPending` 只在 `BeginZoneTravel` 置位,清除路径 6 条齐全(同步失败 / 响应体拒绝码 / msg 23 且属于传送失败码 / msg 124 到达 / 断线 / `Tick` 75s 兜底),**未发现置位后清不掉的分支**。UI 侧另有 120s 总预算。
- 服务端那条"受理后同步失败时 tip 早于应答到达"(§12.5.3 末尾归入客户端核对项的)在客户端侧**表现正确**:tip 先到时 UI 把请求 `Reset`(代次 +1),迟到的无错应答因代次不符被丢弃,遮罩不会重开。该项关闭。
- **踢线消息 34 的结论(解除 §12.3"冻结上限"的前置)**:客户端收到 34 后**一定**主动断开 TCP 并回到选服界面(`GameClient.cs:1463-1467` → `DisconnectInternal`:关 socket、清 `InGame` / `TokenVerified` / 在途传送标志 / pending、触发 `OnDisconnected` → 选服界面重新激活),不自动重连。传送冻结期间收到同样成立。所以"到期发踢线"的设计在客户端侧成立,不需要 gate 强制断开(对不守规矩的客户端无效,那是另一回事)。
- 客户端没有任何大厅连接的自动重连 / 自动重发 `EnterGame`;断线后一律回选服界面走全新 HTTP login + assign-gate,**不带旧票据**(票据只是 `RedirectFlow` 的局部实参)。
- tip 码按**枚举名**引用生成的 `SceneErrorTip`(无手抄数字),取值 3023–3027 与服务端一致。

**存活的断点**(客户端仓,本轮未改;AGENTS §9 要求客户端改动须单独授权):

| # | 严重度 | 位置 | 问题 | 最小修法 |
|---|---|---|---|---|
| CL-1 | P1 | `GameClient.cs:645-658`、`QdaoServerSelectView.cs:493-501` | **与 §12.5.2 是同一个洞的客户端面**:第二条腿被拒时客户端等满 60s 后拆连接回选服,只显示"与服务器的连接已断开"("切换服务器失败,请重新登录"被后一条状态盖掉),玩家不知道原因。服务端 `entergamelogic.go` 刻意保留登录会话、票据与等待落点 300s 有效,"供客户端重试"——**客户端没有对应实现**,这份保留完全用不上;回选服后只能从家区入口全新登录,而那条路按 §12.3 GO-5 多半又是 18。 | 两端对齐后二选一:① 服务端在第二条腿被拒时补发 tip / 踢线(§12.5.2 的修法),客户端在等待循环里识别并立即收口;② 客户端在重定向分支超时后于同一连接上按 ≥30s 间隔重发 `EnterGame` 1–2 次。另:选服界面的断线处理不要覆盖已有的失败文案 |
| CL-2 | P2 | `GameClient.cs:717-722` | **已修(§12.5.6 末尾,Unity 未跑)。**客户端用**本机时钟**与服务端签出的 `token_deadline`(TTL 300s)做无容差比较,`now >= deadline` 即 `FailRedirect` 强制断线。玩家机器时钟快 5 分钟以上时,msg 124 一到就被本地判过期——而此刻服务端已放行、源实体将销毁——**每次跨区传送(含回家)必现**。robot 有同款校验但跑在服务端时钟下,压测测不出。 | 本地过期判定降级为告警,权威判定交给 gate(B)(它会回 token_expired 并关连接,现有收口路径已覆盖) |
| CL-3 | P2 | `CityTravelUiRoot.cs:224, :285-286` | "当前所在区"靠 UI 自记的 `_visitingZoneId`,只在"`_pendingZoneId` 非零且入场通知来自新连接"时更新。凡 UI 先收场、msg 124 随后才到的时序(RPC 15s 超时但服务端已受理 / 在途期间任意无关 tip / 75s 超时后服务端才放行),玩家实际已到 B 而客户端仍认为在 A:可去列表滤掉真正的家 A、列出 B,选 B 被服务端回 3024。**UI 上回不了家**,两个区时只能退出重登。 | 由 `GameClient` 在 `RedirectFlow` 换连接成功后只读解析票据的 `target_zone_id` 存成 `CurrentZoneId`(不影响 payload / signature 原样转发),UI 改读它,删掉旁路记账 |
| CL-4 | P2 | `CityTravelUiRoot.cs:306-316` | 地图窗在途期间把**任何** tip 都当传送失败收场,而 `GameClient.IsTravelFailureTip` 已收窄到 5 个码——两者脱节。无关 tip(限流、别的系统)会让窗口提前收场而底层仍在途:要么随后被 msg 124 突然搬走(先报失败后成功),要么 75s 内再点被"正在传送中"挡回。它也是 CL-3 的触发源之一。上一会话报过这条,属实。 | 跨区在途(`_pendingZoneId != 0`)时改用 `IsTravelFailureTip(tip.Id)`;同区换图分支可保持宽判 |
| CL-5 | P2 | `QdaoServerSelectView.cs:714` | 超时 / 断线收口后立刻可再点进入,无任何冷却,间隔必然 <30s;若服务端把这次全新登录判为 ShortReconnect,正好命中 §12.3 GO-3 / GO-5 描述的"每次重连都撤销断线租约、遗留 location 没人清"。是否真被判为 Reconnect 取决于服务端会话状态,**拿不准**。*2026-09-21:服务端侧已核实——30s 内重进一律判 ShortReconnect 并撤销断线租约;真正挡路的是干净断线后无人持有的 location + 僵尸实体(Z1),根因修复见 §12.6(待决,未落码),客户端冷却只是缓解*。*2026-09-21:服务端根因修复**已落码、待验证**(Z1 + A′ 见 §12.6 / §12.6.10,重登去向见 §12.7);客户端冷却不再是必需项,只作可选缓解(§12.6.8 的"退出存盘在途时重登回 18"建议 2–5s 退避)* | 服务端已按 §12.6 / §12.7 落码,待 Codex 验证;客户端本轮不改 |
| CL-6 | P3 | `GameClient.cs:922-930` | `DescribeTravelTip` 缺 3007 `kEnterSceneSceneNotFound`、3014 `kEnterSceneChangingScene` 的文案(枚举已生成),以及 `kSceneTransferInProgress`(在 `cross_server_error_tip.proto`,客户端没生成该枚举)。这些是同步拒绝,走响应体,**能正确收场**,只是显示成"传送失败(tip=3007)"。上一会话报过,属实。 | 补三条文案;`gen_proto.ps1` 收进 `cross_server_error_tip.proto` |
| CL-7 | P3 | `Assets/Scripts/Proto/Generated/SceneErrorTip.cs` | **已修(`.meta` 已入库,见 §12.5.6 末尾)。**该目录下唯一没有 `.meta` 的文件。纯枚举无 GUID 引用,编译不受影响,只会在每台机器首次打开 Unity 时各自生成随机 GUID、产生提交噪音。上一会话报过,属实。 | 有 Unity 的机器打开一次工程,提交生成的 `.meta` |
| CL-8 | P3 | 多处 | 跨区窗口总预算 120s 小于链路最坏耗时(约 165–180s),中途到期会清掉在途区号,是 CL-3 的又一触发源;验票被拒 / 新连接中途断开时管线协程不提前退出,`_redirecting` 会多挂 10–60s;`ZoneTravelClient.cs` 文件头仍写"跑 gen 之前编不过是预期"。 | 随 CL-3 一并处理 |

被反驳者推翻的两条:重定向进行中到达的第二条 msg 124 被丢弃(与 robot 语义不一致,但无害);以及 CL-1 的一个重复表述。

#### 12.5.5 ⑨ 第二条腿失败出口 + ⑩ 回家:两端落码(2026-09-20,未编译、Unity 未跑)

用户要求"按最标准的做",并授权同时改服务端与客户端。两端各一包,各经"编译级静读 + 语义与契约"两名只读复审后回修,零 blocker、零 major。

**新契约(写进 §11.1 的客户端契约,以本节为准)**:`EnterGame` 应答无错 = **已受理**;之后进场没成,服务端经 gate 推一条 `SendTipToClient`(消息号 23),tip 码 = `scene_error` 的 **`kEnterSceneFailed`(3023)**。异步进场链的所有显式失败(预加载失败、apply 失败——含 EnterScene 被 scene_manager 拒、RPC 失败、BindSession / 会话落盘失败)统一走这一个出口、同一个码;具体原因只进服务端日志。选 3023 是因为 Go 侧 `table.SceneError_kEnterSceneFailed` 与客户端 `scene_error.KEnterSceneFailed` 都已生成且有文案,两端都不需要 regen / 导表。

**服务端(`go/login`)**:

- `internal/svc/gate_command.go`:新增 `buildPushTipCommand`,`TipInfoMessage → MessageContent{message_id = SendTipToClient} → PushToPlayerEvent{session_id}` 装进 `GateCommand.payload`,仍经唯一寻址收口 `buildGateCommandMessage`(空 instance id / 非数字 gate id / node id 为 0 三道 fail-closed 守卫、显式分区)。消息号、事件号、tip 码全部用生成常量。
- `internal/svc/servicecontext.go`:`PushTipToSession`,形状与 `KickSessionOnGate` 一致。
- `internal/logic/clientplayerlogin/entergamelogic.go`:唯一失败出口 `notifyEnterGameFailed`,由 `onPreloadComplete` **最先注册的 defer** 触发(最后执行,排在停心跳、释放玩家锁之后——推送是一次同步 Kafka 写,不该拉长持锁窗口)。两处失败分支只置 `failedStage`,**既有处置一行没变**:不新增 `cleanupLoginSessionState`、不踢线(推 tip 是 best-effort 通知,不是状态变更;推送失败只记 ERROR + 计数,不重试)。会话上没有 gate 寻址信息时不硬造,计 `skipped_no_gate`。回调 panic(dispatcher 的 `safego` 各自 recover)不经过这里,由客户端 60s 超时兜底,注释写明。
- `metrics.go`:`entergame_failure_notify_total{stage=preload|apply, outcome=sent|failed|skipped_no_gate}`,无 player_id label。
- 顺带:两处仍把 14 `ErrUnsafeCrossNodeHandoff`、"重定向前先解析场景"当现状的过期注释改成实情。
- 单测:`gate_command_test.go` 补 push tip 的目标字段 / 守卫 / 逐层反序列化用例;新文件 `entergame_failure_notify_test.go` 覆盖 sent / failed(只调一次、不重试)/ skipped 三种结果与一条从 EnterGame 入口打的接线用例。**成功路径不通知、预加载失败分支的接线**无法单测(要起真 Kafka 生产者),只由代码阅读覆盖。

**客户端(`mmorpg-client`)**:

- **⑨**:`GameClient.IsEnterFailureTip`(只认契约码 3023);等进场期间 msg 23 命中即记下,等待循环立刻 `FailPipeline` 收口并显示 `DescribeTravelTip` 文案,不再干等 60s(60s 兜底保留)。msg 23 原有的"传送失败 → `EndZoneTravel` → 广播 `OnServerTip`"不变;游戏内换图失败同样会收到 3023,但那时不在等进场,新标志不生效。重定向场景下失败文案为"切换服务器失败,请重新登录(进入场景失败…)",其它失败只显示固定文案,开发者诊断串只进日志。选服界面断线处理改读 `GameClient.DisconnectReason`,**不再用"与服务器的连接已断开"盖掉真正的失败原因**。
- **⑩**:`GameClient.CurrentZoneId` 成为"当前所在区"的单一真源 = 当前连着的那个 gate 所属的区:普通进入取所选区;每次 `RedirectFlow` 换连接成功后,从服务端签发的票据**只读解析** `target_zone_id`(为 0 取 `zone_id`;解析失败保留旧值并打日志,不影响重定向,也不影响 payload / signature 原样透传);断线清零。地图窗改读它,**删掉 `_visitingZoneId` 旁路记账**;跨区在途只认 `IsTravelFailureTip` 的 5 个码,失败文案走 `DescribeTravelTip`,不再拼裸编号。
- 顺带:`DescribeTravelTip` 补 3007 / 3014 文案;`ZoneTravelClient.cs` 文件头过期注释改成实情。
- EditMode 单测 5 个(`Tianyong/CityTravelRequestTests.cs`):进场失败判据、文案、票据解析的两种情形。

**对应条目状态**:§12.5.2 的 S5-B1 / S8-1 **已修**(服务端出口);§12.5.4 的 CL-1(⑨ 客户端面)、CL-3 / CL-4(⑩)**已修**;CL-6 部分(3007 / 3014 已补,`kSceneTransferInProgress` 等客户端下次 gen 收进 `cross_server_error_tip.proto`);CL-8 的 `ZoneTravelClient` 文件头已修。**仍未修**:CL-5(手动重进无冷却;根因修复见 §12.6,待决 —— *2026-09-21 已落码待验证,见 §12.6.10 / §12.7*)。*(2026-09-21:原列在这里的 CL-2、S7-1、S3L1-1、CL-7 已落码,见 §12.5.6。)*

**残余与待验证**:

- 已知的重复通知:EnterScene RPC 超时、但 scene_manager 实际已放行时,客户端可能先后收到进场通知与 3023。先到进场则 3023 被忽略(已不在等进场);先到 3023 则客户端断线回选服,服务端侧玩家随之走正常退出。不回档,但玩家要重进一次。
- `Tianyong` 测试程序集是唯一没有显式声明 `Google.Protobuf.dll` 的测试 asmdef(`overrideReferences: false`)。插件 `isExplicitlyReferenced: 0`(Auto Reference 开),按 Unity 规则会自动引用,但**首次编译若报 CS0246 / CS0012**,照其它测试 asmdef 改成 `overrideReferences: true` + `precompiledReferences: ["Google.Protobuf.dll"]` 即可。
- 端到端链路(login → Kafka gate-cmd → gate `PushToPlayerEventHandler` 按 session_id 找连接 → 客户端 msg 23)只做了静读。

**验证**:

1. 服务端,工作目录 `go/login`:`go build ./...` → `go vet ./internal/svc/... ./internal/logic/clientplayerlogin/...` → `go test ./internal/svc/ -run "GateCommand|PushTip" -count=1 -v` → `go test ./internal/logic/clientplayerlogin/ -run "TestNotifyEnterGameFailed|TestEnterGame_" -count=1 -v` → `go test ./... -count=1`。`gofmt -l` 只允许出现 `entergamelogic.go`(`enterGameSessionState` 的字段对齐差异是 HEAD 既有的)。
2. 客户端:Unity 打开工程无 CS 错误 → Test Runner 跑 `MmorpgClient.Tests.EditMode.Tianyong`。
3. 联调(两端都就位):a) 构造目标区 EnterScene 被拒 → 客户端数秒内回选服,状态栏显示"切换服务器失败,请重新登录(进入场景失败…)",login 计数 `entergame_failure_notify_total{stage="apply",outcome="sent"}` +1;b) 构造换连接后验票失败 → 状态栏只显示固定文案、不含 `token verify`;c) 传送到外区后打开地图窗,归属区出现在可去列表里;d) 普通登录同样构造一次 EnterScene 被拒,确认也是秒级收口而不是等 60s。

#### 12.5.6 S3L1-1 第二层出口 + 铸造重放识别(2026-09-21,未编译)

用户要求把剩余几条"按最标准的做",授权同时改服务端与客户端。设计稿经两名对抗复审(归属 / 活性)后按"必须改"项定稿,C++ / Go / 客户端三包落码并各自回修。**服务端未编译、未跑测试;客户端 Unity 未打开、EditMode 未跑。**下文行号是 2026-09-21 工作树的位置。

**更正 S3L1-1 的前提(§12.5.2 原文说"双重故障才触发、应答里区分不出已回滚 / 没回滚",两处都不够准):**

1. **主触发形态下源 scene 收不到 7。** scene_manager 的 zrpc 服务端超时:本地 `Timeout: 5000`(`go/scene_manager/etc/scene_manager_service.yaml:3`);K8s ConfigMap 没写 `Timeout`,落到 go-zero 默认 2000ms。两者都**不大于** `KafkaWriteTimeoutSeconds: 5`(同一 yaml `:32`)。Kafka 写超时或 Redis 挂起时,handler 还没走到回 7,调用方已拿到 DeadlineExceeded;C++ 生成的 gRPC 客户端对非 OK 状态只打日志、不回调(`scene_manager_service_grpc_client.cpp:141-153`)。源 scene 只能等 30s 应答看门狗。只有"秒拒"型失败(broker 明确报错、Redis 也秒拒)才会带回显式 7。所以出口不能只认"显式失败应答"。
2. **不需要双重故障。** go-zero 给 go-redis 设了 `MaxRetries=3`,go-redis 对读超时 / EOF 会把同一条 EVAL 原样重发。第一条腿的铸造 Lua(`luaMintEpochAndSetLocation`)首发其实已执行(epoch N→N+1、location 写成等待落点)、只是应答丢了时,重发读到 `cur ≠ ARGV[1]` 回 0,被当成 epoch 冲突(19):**既不发重定向、也不回滚**,旧场景人数也不扣——与"推重定向失败 + 回滚失败"同一个终态,只要一次 Redis 读超时。scene_manager 在铸造之后、写 Kafka 之前崩溃,也是同一终态(源端收不到应答)。
3. 回滚 Lua 同样会被重放:首发已回滚、应答丢了时,重发原先回 0 被记成"并发推进",调用方连旧场景人数都不还(人数漂移,现存小缺陷)。

**新契约(写进 §11.1 的客户端契约,以本节为准)**:`TravelToZone` 受理后,客户端只会看到三种结局之一——到达(msg 124);未成、原地恢复(`SendTipToClient`,已解冻);**未成且无法在原地恢复(owner_epoch 已被推进、玩家已不在本节点,又没收到放行应答)时,`SendTipToClient` 之后紧跟踢线 34(`GameKickPlayerRequest.reason.id` = 同一个 tip),实体销毁而不是解冻,客户端断线回选服重登**。tip 码是 `kZoneTravelTargetBusy`(3027)。`player_scene.proto:73-76` 的契约注释留到下一次改 proto 时同步(本轮禁改 proto)。

**C++ scene(`cpp/libs/services/scene/player/`)**:

- **证据枚举**:`travel_outcome::Evidence : uint8_t { kSucceeded, kFailed, kNoReply, kMarkWriteUnknown, kAnomalous, kCount }`(`system/player_lifecycle.h` 的 `travel_outcome` 命名空间),名表是平铺数组 `kEvidenceNames[]` + `static_assert(std::size(...) == kEvidenceCount)`,不用宏(AGENTS §11.2)。`ResolveTravelOutcome` 的 `bool replyWasSuccess` 换成 `Evidence`,不给默认值。调用点(`system/player_lifecycle.cpp`):进场路由就地落到本节点 `:764` 与同 zone 成功应答 `:2389` → `kSucceeded`;显式失败应答 `:2378` → `kFailed`;首次挂应答看门狗 `:2083` → `kNoReply`;写标记结果未知(EnterScene 根本没发出去)`:2004` → `kMarkWriteUnknown`;跨 zone 却"成功且无 redirect" `:2396` → `kAnomalous`。
- **证据不丢**:`ArmTravelReplyWatchdog(playerId, requestedAtMs, Evidence, std::string reason)` 按值捕获 id / 代际 / 证据 / 原因文本(§11.7,不绑对象);`ResolveTravelOutcome` 因 Redis 不可用 / 应答异常 / 命令发不出去的三处重挂都传**当前**证据,不再一律翻成超时。复审另查出"首次挂的 kNoReply 看门狗从不取消、会先到期"会把已到的应答证据盖掉(同 zone 成功会补一条假失败 tip、跨 zone 协议异常会被当成超时踢线),回修为:入口把非 `kNoReply` 的证据记到 `PlayerTravelHandoffComp.hasRecordedEvidence / recordedEvidence`(`comp/player_ownership_comp.h`,纯内存,每次 `StartTravelHandoff` 重新 emplace),裁决一律用 `travel_outcome::EffectiveEvidence`(记下的优先),MGET 回调里按回调那一刻的组件再取一次。
- **单条原子读**:DEL 标记之后那次 `GET owner_epoch` 改为单条 `MGET owner_epoch location`(`:2280` 附近)。应答必须是两元素数组、第 0 项只能是 STRING 或 NIL,否则按读失败处理:保持冻结、带原证据重挂看门狗。
- **epoch 没变**:`AbortTravelHandoff(..., notifyFailure = evidence != kSucceeded)`,与原语义一致(`:2204` 附近)。
- **踢线判据(epoch 已变、证据不是 kSucceeded 时,五条全部满足才踢)**:
  1. 跨 zone(`targetZoneId != GetZoneId()`);
  2. 证据是 `kFailed` 或 `kNoReply` —— 1、2 合为纯函数 `travel_outcome::ShouldResetClientOnGrant(crossZone, evidence)`;
  3. 实体上没有 `UnregisterPlayer`(纵深防御:玩家已在退出时会话由退出流程收尾,不依赖"退出优先会立即销毁交接中实体"这一具体实现,§12.6 若改退出流程也不受影响);
  4. location 能解析成 `storage::PlayerLocation`(缺失 / 解析失败不踢);
  5. location 是**本次交接自己第一条腿**写下的等待落点:`node_id` 为空、`zone_id == targetZoneId`(且非 0)、`location.owner_epoch ==` 同一次 MGET 读到的 owner_epoch(且非 0)—— 纯函数 `travel_outcome::IsAwaitingPlacementOfHandoff`。

  任一不满足只销毁,打 `LOG_WARN [ZoneTravel][ClientReset] not resetting client of player … : <原因>; … destroying only`。第 5 条是复审的 P2:同一会话上一条迟到的同 zone 换图请求(zrpc 超时后 handler 仍在跑)可能凭这次交接的活标记把会话改绑到本 zone 别的节点;错配应答同理。此时 epoch 也变了,但 location 落在具体节点上,踢线会断掉一条合法会话。
- **踢线动作**:匿名命名空间里的 `SendTipAndKickToClient(entity, tipId)`(`:200`):先 `PlayerTipSystem::SendToPlayer`,再经 `SendMessageToClientViaGate` 发 `SceneClientPlayerCommonKickPlayer`(34)。**必须在 `DestroyDeposedPlayer` 之前**调(它会 `RemovePlayerSession`,之后两条都发不出去),实际调用点 `:2266` 附近。
- **为什么只对跨 zone**:第一条腿只推 `RedirectToGateEvent`、从不改绑 gate(A) 上的会话;重定向真送达时,客户端立即摘掉旧连接的处理器、连上新 gate 后关掉旧连接,gate(A) 随即给源节点发 ExitGame,实体先被"退出优先"销毁,走不到踢线。同 zone 放行靠 `RoutePlayerEvent` 把**同一个会话**改绑到目标节点,"没收到应答"常常是应答丢了、路由已经到了,踢线会误伤。
- **计数**:`travel_handoff_stats` 新增 `grantedClientReset`(Counters / Snapshot / Read),`[TravelHandoff]` 汇总行末尾追加 `granted_client_reset=`(`:238-252`)。它是 `granted_without_reply` 的子集。
- **共享解析**:location 键名收进 `player_ownership::LocationRedisKey`(`comp/player_ownership_comp.h`),解析收成 `player_ownership::ParsePlayerLocationElement`(声明在 `system/player_lifecycle.h`,定义在 `player_lifecycle.cpp:62`,原样搬自 `player_team.cpp` 的 `ParseLocationElement`)。没放进 ownership 头是因为它经 `player_frozen_comp.h` 被二十来个业务系统包含,不该带上 hiredis 与 `storage.pb.h`。`player_team.cpp` 只改为调用共享函数,MGET 命令本身不变。
- 契约注释已按新契约改写:`RequestZoneTravel` / `HandleTravelEnterSceneReply` / `DestroyDeposedPlayer` / `ResolveTravelOutcome` / `ArmTravelReplyWatchdog`(头文件)、`.cpp` 的链路总图与 `:139` 附近看门狗预算注释。
- 单测:`cpp/tests/cross_zone_test/cross_zone_test.cpp:506-596` 共 8 个 `TravelOutcomeReset.*`(跨 zone kFailed / kNoReply 才踢、其余证据不踢、同 zone 遍历全部证据都不踢、名表覆盖、等待落点正例与 5 个反例、记下的证据覆盖看门狗 kNoReply、没记时用带入证据)。

**Go scene_manager(`go/scene_manager/internal/`)**:

- **回滚 Lua 三态**(`logic/owner_epoch.go:150` `luaRollbackPlayerPlacement`):`1` = 本次回滚成功;`2` = 已经回滚过(location 恰为回滚目标原文,原文为空时键不存在;且 epoch 恰为 restoreEpoch)——**只在本次铸造过(`ARGV[3] ~= ARGV[4]`)时**返回;`0` = 被并发推进。非铸造回滚(同物理节点换图 / dev 旁路)证明不了"是本请求回滚的"(UpdateTime 秒级,同秒同目标字节相同),一律不返回 2。
- `rollbackPlayerPlacement`(`:258`)仍返回 bool,调用方不变:err → `redis_error`(false);1 → `rolled_back`(true);2 → `already_rolled_back`(true,人数照还,修掉现存漏还);其它 → `superseded`(false)。日志统一前缀 `[RouteRollback] outcome=<…>`(**旧日志原文"route 回滚玩家位置/epoch CAS 失败""route 回滚检测到…"已不存在**)。不做应用层重试:go-redis 已重试 3 次;zrpc 超时后应答本来就到不了源端;回滚晚于源端裁决落地更糟。注释写明:"Redis 出错时源端会重置客户端"只对跨 zone 调用点(`handleCrossZoneRedirect`)成立;同 zone 调用点(`rollbackEnterSceneAfterRouteFailure`)的 `redis_error` 意味着哑连接(已知限制,见下)。
- **铸造重放识别**(`logic/owner_epoch.go:80` `luaMintEpochAndSetLocation`,KEYS = owner_epoch / location / handoff,ARGV = 观察值 / 本次 location 原文(已带 epoch+1)/ 所凭标记原文):在 `return 0` 之前加判定——**仅当本请求带标记(`ARGV[3]` 非空)**,且 `cur == ARGV[1]+1`、当前 location 等于本请求要写的原文、handoff 标记仍等于所凭标记时,判为"本请求已生效",返回 `cur`(= 本该铸出的新 epoch),Go 侧按铸造成功继续(扣旧场景人数、发重定向 / 路由),Go 代码不用改。**为什么只对带标记的铸造**:不带标记的铸造(首次落点 / 等待落点)的 location 原文带的 UpdateTime 是秒级,同一秒、同一目标的两个并发请求字节完全相同,区分不开"我已生效"与"别人抢先写了同样的值";带标记时"标记仍在 + epoch 只前进一格 + location 原文一致"三条合起来能证明是本请求自己的写入。源端 DEL 标记之后不再认。
- **指标**:`scene_manager_enter_scene_rollback_total{outcome=rolled_back|already_rolled_back|superseded|redis_error}`(`metrics/metrics.go:262`,`ObserveEnterSceneRollback` `:410`),只有 outcome 一个标签。铸造重放识别另有 `scene_manager_enter_scene_mint_replay_recognized_total`(无 label,应恒近 0)与日志前缀 `[MintReplay]`:识别分支返回**取负的**新 epoch(≤ -2,不会与 -1 / 0 撞;凭标记放行时观察值 ≥ 1),`changesceneutil.go` 的 `decodeMintReply` 还原后计数(单测 `TestDecodeMintReply`)。
- **告警**:`deploy/k8s/scene-manager-alerts.yaml` 新增 `SceneManagerEnterSceneRollbackRedisError`(warning,`sum(increase(scene_manager_enter_scene_rollback_total{outcome="redis_error"}[10m])) > 0`,不设 `for`),注释分跨 zone / 同 zone 写了含义与处置。现共 18 条规则,**未经 promtool 校验**。
- 单测(`logic/owner_epoch_test.go` 末尾追加,原有内容未动):`:1830` 回滚执行两次(铸造过 / 首次落点 oldRaw 为空 / 非铸造第二次回 false 且不计 already);`:1908` 铸造重放(带标记的第二次被识别、epoch 只加一;标记已撤回 / location 已被换 / 不带标记三种仍回 0);`:1985` 推 Kafka 与回滚同时失败 → 回 7、无 Redirect、epoch=2、location 停在 zone 2 且 node 为空、`redis_error` +1;`:2033` 从源 zone 重登落到未回滚的等待落点(注入真实归属查询;未映射子用例回 20 且状态不变)。

**客户端(`mmorpg-client`,`Assets/Scripts/Game/GameClient.cs`)**:KickPlayer(34)处理器(`:1611`)解析 `GameKickPlayerRequest.Reason.Id`(解析抛 `InvalidProtocolBufferException` 时记 LogError、按 0 处理、照样断线,fail-closed);新纯函数 `DescribeKickReason(reasonId)`(`:1069`,与 `IsTravelFailureTip` 同一判据)命中时以 `DescribeTravelTip(id) + "请重新登录。"` 作为 `DisconnectReason` 调 `DisconnectInternal`,否则维持原来的 `Disconnect()`(reason = null)。否则前一条 3027 的文案会被选服界面的通用断线文案立即覆盖(两条通常在同一次 Poll 里先后派发)。日志 `[gate] kicked by server reason=<id>`。EditMode 单测 2 条(`Assets/Tests/EditMode/Tianyong/CityTravelRequestTests.cs:161`、`:174`)。

**同批收口的三条(用 git 核实,均未编译 / Unity 未跑)**:

- **CL-2**(客户端本机时钟拦截票据过期):随客户端自动保存提交 `2ca620e` 进库。`GameClient.ValidateRedirectTarget(ev, nowUnixSec, out pastDeadlineSec)`(`GameClient.cs:766`)抽成纯函数,`token_deadline` **只打日志、不拦截**,有效期以 gate(B) 用服务端时钟验票为准(过期回 token_expired 并关连接,现有收口路径覆盖);`pastDeadlineSec` 只供排查玩家时钟偏差。EditMode 单测 `CityTravelRequestTests.cs:204`、`:221`。
- **S7-1**(第一条腿 `scene_config_id = 0` 不预检):随服务端自动保存提交 `4624ddf9e` 进库。`rejectTravelToUnopenedMap`(`enterscenelogic.go:840`,调用点 `:362`)在 conf=0 时经 `defaultWorldConfID()`(`:1149`,World 表第一行,场景解析与预检共用这一个口径)解析默认大世界 conf 再查频道集合;World 表为空 fail-closed 回 1,Redis 读失败 fail-open(预检不是安全门)。单测 `owner_epoch_test.go:766`、`:824`。
- **CL-7**:`Assets/Scripts/Proto/Generated/SceneErrorTip.cs.meta` 已随 `2ca620e` 入库。

**残余风险**:

1. **同 zone 仍是哑连接**:同 zone 路由失败 + 回滚 `redis_error`、scene_manager 在铸造后写 Kafka 前崩溃,源端判"已放行"后只销毁不踢(同 zone 不踢是有意的,理由见上),gate 会话仍绑在源节点,玩家要等客户端自己断线重登。带标记的同 zone 铸造重放已能识别;不带标记的铸造重放仍回 19(login 发起的落点由 §12.5.5 的 3023 出口收口)。可选修法:gate 只接受会话当前绑定节点发来的 34(改 gate 手写段);或新增"路由失败且回滚失败"错误码。
2. **gate-cmd 消费滞后 >30s 时,迟到成功的传送会被踢回选服**:踢线走 scene→gate TCP 直达,先于 Kafka 上的 124 到达;数据不受影响,玩家多重登一次。K8s 下 zrpc 默认 2s 让"迟到"从"应答丢失"扩大到"任何超过 2s 的第一条腿",所以配 `Timeout` 列为上线前置(§12.2)。
3. **无视 34 的客户端仍挂着**:scene 没有强制 gate 断开的 RPC。主触发形态下玩家要先冻满 30s 才会被踢。
4. **C++ 计数只进 `[TravelHandoff]` 汇总行**(30s、有变化才打),不进 Prometheus,`stress_summarize.ps1` 不解析(§10.3 / runbook G1)。
5. ~~铸造重放识别无指标~~ 已补:计数 `enter_scene_mint_replay_recognized_total` + 日志 `[MintReplay]`(见上文"指标")。
6. **重放误判**:凭同一份标记、同一秒、同一目标的两个并发请求(只有上游重复提交才会出现)会都被判成功,归属终态正确,代价是旧场景人数多扣一次、重定向 / 路由多发一条(客户端与 gate 对重复 124 去重)。非铸造回滚的第二次执行记为 `superseded` 返回 false,首发若其实已执行则人数不还,是有意的保守取舍。
7. **回滚晚于源端裁决**:单个回滚 EVAL 在 go-redis 重试下最坏约 4×(Dial 5s + 读写 3s)+ 退避,可超过 30s;已发到 Redis 的 EVAL 在客户端放弃后仍可能执行。location 会指回已无实体的源节点,重登可能被 18 挡住;"断线租约到期后 LeaveScene 清掉 location 自愈"**只在玩家离线满 30s、期间不重进时成立**(30s 内重进一律 ShortReconnect 并撤销租约),挂到 GO-3 / CL-5 名下,根因修复见 §12.6。
8. **tip 与 34 的到达顺序**:两条走同一个 RpcSession,大概率有序,但 `player_message_utils.h:7-9` 声明不保证。颠倒时客户端文案只看 34 自带的 reason,不受影响;34 先到则后到的 tip 已无连接可收。
9. **应答晚于 30s 且看门狗那次核实已带 kNoReply 裁决完**:无法补救,时序本身决定。
10. robot `travel_smoke_wire.go:40` 与 `travel_smoke_scenario.go:762` 的注释 / 失败文案仍按旧契约写"scene 已解冻",对本路径不成立(robot 不在本轮授权范围)。

**验证(全部未执行,交 Codex;C++ MSBuild 必须串行 `/m:1 /nr:false`)**:

1. Go,工作目录 `go/scene_manager`:`gofmt -l internal/logic internal/metrics` 无输出 → `go build ./...` → `go vet ./internal/logic/... ./internal/metrics/...` → `go test ./internal/logic/ -run "TestRollbackPlayerPlacement|TestMintEpochLua|TestEnterScene_CrossZoneRedirectKafka|TestEnterScene_LoginAtSourceZone|TestEnterScene_TravelWithoutMap|TestEnterScene_RouteFailure|TestPlacePlayerLocation" -count=1 -v` → `go test ./... -count=1`。重点看 `TestMintEpochLuaRecognizesReplayOfItsOwnWrite` 的带标记子用例与 `:1985` 那组(它依赖 `SetError` 之后的调用顺序,拿不准)。
2. C++:`cpp/libs/services/scene/scene.vcxproj` → `cpp/nodes/scene/scene.vcxproj` → `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone`:`TravelOutcomeReset.*` 8 个全绿,`HandoffMarkWithdrawQueue.*` / `CrossZoneFrozen.*` / `CrossZoneBagMarshal.*` 不变。`player_ownership_comp.h` 加了字段、`player_lifecycle.h` 新增 `<iterator>` 与 `storage` 前向声明,建议再跑一次不带 `-Filter` 的全量 `run_cpp_tests.ps1`(asset_op_system_test、currency、bag 等引用它们的工程都要能编过)。
3. `promtool check rules`(先取出 `scene-manager-alerts.yaml` 的 `spec.groups`):18 条无语法错误;本机没有 promtool 就如实记录。
4. 客户端:Unity 打开无 CS 错误 → Test Runner 跑 `MmorpgClient.Tests.EditMode.Tianyong.CityTravelRequestTests`,重点两条 `DescribeKickReason_*` 与 CL-2 的两条 `ValidateRedirectTarget_*`。
5. 故障注入:runbook B3(Kafka pause + 回滚注入 Redis 错误)。另做一次"同 zone 换图成功应答到达那一刻让 zone Redis 不可用、再恢复":期望日志 `evidence=succeeded`(不是 `no_reply`)、不补发失败 tip。
6. 回归:基线 travel-smoke `TRAVEL_SMOKE_OK` 且 `granted_client_reset` 不变、不出现 KickPlayer;runbook B1 仍是"解冻 + tip,不踢线";另一台设备顶号时客户端仍显示原来的通用断线文案。

### 12.6 Z1 僵尸实体 + 断线释放标记(CL-5 / GO-5 的根因修复)—— 已落码(2026-09-21,C++ 未重新编译、未测试)

**状态:已落码(2026-09-21,用户授权"按最标准的做法做"),C++ 未重新编译、未测试;Go 侧配套(§12.6.9 的 epoch==0 铸造、§12.7 的 GO-5)未编译、未测试。**Codex 此前那次 `game.sln` 整体编译早于本批代码(提交 `9cef7b2ec` 与其后的工作树增量),不能算作本批的编译证据。实际实现与下文规格的偏差、M1–M15 的落点、复审回修、残余与 Codex 验证清单见 **§12.6.10 落码记录**。12.6.1–12.6.9 保留为规格原文(只在状态处加注);规格与 §12.6.10 冲突时以 §12.6.10(= 代码实情)为准。下文 12.6.1–12.6.5 的行号是落码**之前**的 2026-09-21 工作树位置,已随落码整体漂移,查代码请用 §12.6.10 的行号。

#### 12.6.1 发现:Z1 —— 真写盘的正常断线退出不销毁实体

- **事实链**:gate 断线回调对已绑实体的会话立即发 ExitGame(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:408-437`)→ `HandleExitGameNode`(`player_lifecycle.cpp:923` 起)只做三件事:挂 `UnregisterPlayer`、摘场景、`SavePlayerToRedis`,**不解绑会话**(解绑只发生在 `FinishExitAfterPersist` 的 `RemovePlayerSession` `:1061` 与 `DestroyDeposedPlayer`)。写盘落地后 `HandlePlayerAsyncSaved` 的退出分支里那段"防御性"判断(`:570-586`,HEAD `:540-556`,注释 "Defense in depth: if a reconnect rebound this entity to a live session…")看的是快照里的会话——**就是退出会话本身**——它仍在 `SessionMap` 里且映射到本玩家,于是判成"重连已取代退出",摘掉 `UnregisterPlayer` 后直接 return:`FinishExitAfterPersist` 不调、实体不销毁、快照更新(`:593-603`)也被跳过。只有 dirty-save 快路径(盘上已是同一份)会在 `HandleExitGameNode` 里同步 `FinishExitAfterPersist`(日志 `already persisted (dirty-save fast path); finishing exit inline`,`:967`)真正销毁。该代码块自 `ff7845bb7`(2026-04-21)起未改过。
- **实跑证据**(旧检出 `D:\luyuan\mmorpg` 的二进制,代码块相同):`bin/logs/cpp_nodes/scene.20260906-071422.DESKTOP-I6DK28J.26760.log:207-233` —— 两名玩家 `ExitGame` → `Saving complete` → `ignoring stale UnregisterPlayer … live session 131073 / 131074 indicates reconnect superseded the logout intent`,之后两人都又被周期存盘(`:222`、`:233`),全程没有 `Destroying player`。`scene.20260905-225020.DESKTOP-I6DK28J.38476.log:231-242`:同一时刻两名玩家退出,走快路径的被销毁,真写盘的成了僵尸。
- **后果**:a) 泄漏 + 周期存盘空转(快照没更新,退出后第一次周期存盘必定再写一遍);b) 停机 drain 可能卡到看门狗(`main.cpp:158-182` 对"没有 UnregisterPlayer"的实体再次 `HandleExitGameNode`,又被同一分支判成活会话——**推断,未见日志**);c) 队伍系统 `HasLiveSession`(`player_team.cpp:90-105`)可能把离线玩家当在线(推断,是否可达拿不准);d) **回档**:僵尸静默下来(数据不再变化、一直走快路径)→ 玩家去别的节点玩、epoch 推进 → 某次首次落点挑回原节点,`PlayerEnterGameNode` 复用僵尸(`DiscardStaleHandoffEntity` 只丢"交接已发起"的实体),`EnterScene` 把 epoch 抬到最新,旧内存态成了真身,下一次存盘 CAS 通过、覆盖盘上更新的进度(推断,需实跑确认,频率拿不准)。
- **但今天的僵尸在几种竞态下反而在"补存"差额,不能简单改成"落地即销毁"(复审 P0)**:落地的 payload 未必等于销毁那一刻的内存,今天这些差额由僵尸之后的周期存盘补回(快照停在旧值,每次都重写)。三条确定可达的时序:(a) 停机 / 排空期间 gate→scene 的 muduo RPC 照收,`ProcessClientPlayerMessage` 不看 `UnregisterPlayer`,退出存盘在途时客户端操作照常改实体;(b) 战斗结算在线路径只防冻结、不防退出,应用后立即 `ClearPendingSettlementIfMatch` 给 battle 销账——上面那份日志里就有:退出存盘落地约 21ms 后 `结算已应用 … gold=22`,随后僵尸在 `15:15:43` 把它存了下来;(c) `redis_client.h` 在 Redis 回 ERROR(MISCONF / OOM / READONLY,连接不断)时会让同 key 的旧值覆盖新值或丢掉新值(`redis_client.h:477-496`、`:613-625`、`:735-747`),Redis 恢复后落地的是旧值。修成"落地即销毁"后这些差额变成永久丢失,断线释放标记(A1)还会把旧档认证成终态。

#### 12.6.2 CL-5 / GO-5 的根因

干净断线后 location 滞留、无人持有:`HandleExitGameNode` 不写 handoff 标记(`FinishExitAfterPersist` 只在有疏散票据时经 `DispatchEmergencyRelocate` 写),location 要等 login 显式传的 30s 断线租约(`session_manager.go:115`)到期后由 player_locator 调 `LeaveScene` 删除;30s 内重进一律判 ShortReconnect 并撤销租约(`session_manager.go:169-185`、`reconnectlogic.go`)。login 带的 SceneId 恒为 0,scene_manager 按最小负载挑频道、没有玩家亲和,挑到别的节点即 crossNodeHandoff → 无标记 → 18 → login 推 3023(§12.5.5)→ 客户端秒级回选服 → 再点又是 ShortReconnect……**同 zone 多节点是概率性 18,访客从归属区入口重登是确定性 18**(login 恒发 `ZoneId = 本 zone`)。挑回原节点时反而能进——复用的是 Z1 僵尸。§12.5.5 之前这个环就存在,只是每轮 60s;客户端冷却(方案 D)只是缓解。

#### 12.6.3 方案 A′ 最终规格(只改 C++ scene,不改 proto / Go / 客户端)

**第一步:Z1 修复(硬前置,单独一个提交)**

- 新纯运行时组件 `PlayerExitIntentComp { SessionId sessionAtExit; ExitCause cause; bool releaseMarkSuppressed; }`,与 `UnregisterPlayer` 成对挂、成对摘(挂在 `HandleExitGameNode`;摘在 `EnterScene` 第 0 步与"被取代"分支)。
- `ExitCause : uint8_t { kUnspecified, kClientDisconnect, kNodeShutdown, kSceneDrain, kIdentityConflict, kReleasedByTransfer, kCount }` + 平铺名表 + `static_assert`(§11.2)。`HandleExitGameNode(entity, ExitCause cause = kUnspecified)`,默认值 = 不写标记(漏标只会少写)。调用点:ExitGame handler → `kClientDisconnect`;`main.cpp` 停机两处 → `kNodeShutdown`;`scene_node_service.cpp` ReleasePlayer → `kReleasedByTransfer`;`EnqueueRelocateTicket` 加参数:`BeginEmergencyRelocateAll` → `kIdentityConflict`,`BeginSceneDrain` → `kSceneDrain`;`s2s_player_scene_handler.cpp` 来源不明保持默认。已在退出中又来一个原因:`kIdentityConflict` / `kUnspecified` 粘性置 `releaseMarkSuppressed`;`kReleasedByTransfer` 只在 scene 侧 dev 开关(与 `AllowUnsafeCrossNodeHandoff` 同步)开着时压制;其余保持原原因。
- 退出分支的判定顺序(替换 `:570-586`):
  1. **被取代**:当前绑定会话 ≠ `sessionAtExit` 且它在 `SessionMap` 里映射到本玩家(纯函数 `IsSupersedingSession(current, atExit, mappedToPlayer)`)→ 保持今天的行为(摘标记、保留实体),WARN 文案改为 "superseded by a newer session" 并计数;
  2. **意图组件缺失**(不该发生)→ fail-closed:沿用今天的"保留实体、摘 UnregisterPlayer",LOG_ERROR + 计 `exit_intent_missing`,**不写 A1**;
  3. **收敛判定**:刚落地的 message 与当前 marshal 相等(`dirty_save::IsEqual`,比对前剔除 `stresstest_probe` 的 `test_seq / test_sig`,否则压测构建永不收敛),**且** `MessageAsyncClient` 里该 key 没有在途 / 排队中的存盘(新增只读查询)→ `FinishExitAfterPersist`(随后 A1);
  4. **不收敛** → 先用 message 更新 `PlayerLastPersistedSnapshotComp`,再 `SavePlayerToRedis`,保留 `UnregisterPlayer` 与意图组件,等下一次落地再判;设轮次上限,超限 fail-closed 保留实体,LOG_ERROR + 计数,交停机看门狗裁决。
- **快路径同样受第 3 条约束**:`HandleExitGameNode` 的 dirty-save 快路径同步收尾(含 A1)之前,先查该 key 没有 saving / pending;不满足就改走一次真实存盘,落地后按上面的规则收尾(Z2 与复审 P3 同源)。
- **关掉退出中实体的改动入口**(让收敛可达):`ProcessClientPlayerMessage` 对带 `UnregisterPlayer` 的实体只放行 ExitGame;战斗结算在线路径把 `UnregisterPlayer` 视同离线(走锁校验 + `StorePendingSettlement`,不销账)。
- **redis_client.h 同 key 新旧颠倒**:修(直接发出新的全量快照时丢弃同 key pending 里的旧值;`QueueSaveForRetry` 与成功路径只发比刚处理那笔更新的值,Element 加每 key 单调序号),或至少以第 3 条的"无在途 / 排队"判据绕开;两者都做最稳。
- 行为变化(需知会):写盘退出之后实体真的销毁;同节点重登改为从 Redis 重载,不再复用僵尸;重登会按 `enter_gs_type` 触发 `PlayerLoginEvent`(其处理器目前全是 TODO,今天无实际影响)。

**第二步:A1′ —— 干净退出后写"断线释放标记"**

- 位置:`FinishExitAfterPersist` 里 `DispatchEmergencyRelocate` 之后、`RemovePlayerSession` 之前;`DispatchEmergencyRelocate` 改为返回"本次是否消费了疏散票据"。
- 纯函数 `DecideExitReleaseMark`,**全部满足才写**,任一不满足不写并按原因计数:实体有效;意图组件存在且 `cause ∈ {kClientDisconnect, kNodeShutdown, kSceneDrain}` 且 `!releaseMarkSuppressed`(生产口径下 `kReleasedByTransfer` 不粘性压制,由下面的 owner_epoch 条件把关;dev 开关开着时压制);本次没有消费疏散票据;`PlayerOwnerEpochComp.epoch != 0`;**本节点身份当前确认有效**(`tlsEmergencyRelocating` 为真期间一律不写,计 `skip_identity_conflict`;并由 Node / EtcdService 暴露只读信号:最近一次 keepalive 成功在 TTL/2 内、且不在重注册中);**不是"退出优先且 handoff 标记已写、EnterScene 可能在途"**(该分支不写,计 `skip_handoff_inflight`——否则在途 EnterScene 会用掉新标记,放行到一个会话已断的目标节点);**运行期开关打开**(默认开)。
- 命令:Lua 条件写,`owner_epoch` 仍等于实体缓存的 E 才 `SET player:{id}:handoff "E:now_ms" EX 300`(值用 `HandoffRedisValue` 生成),走 `tlsRedis.GetZoneRedis()`(与存盘、A2、撤回同一条连接,FIFO)。回调只按值捕获 playerId 与标记原文(§11.7):1 计 `written`、0 计 `epoch_moved`、空应答 / ERROR / dispatch 失败计 `failed` 并 WARN;**一律不重试**。
- 在途计数 `ExitReleaseMarksInFlight`,计入停机 drain 谓词(`main.cpp` 的 drained 条件与进度日志)与 `IsEmergencyRelocateDrained`,受 Node drain 看门狗约束。

**第三步:A2′ —— 新载入前原子"先核归属再删"继承来的标记**

- 发起:`scene_handler.cpp` 新载入分支,写入 pending 条目之后、`AsyncLoad` 之前;覆盖在途条目时拷贝旧条目的清理状态。`ctx.ownerEpoch == 0`(旧版路由)跳过、闸门放行。
- Lua:先 `GET owner_epoch`(缺键按 0),`≠ ctx.ownerEpoch` 立即返回 `-1`、一个标记都不删;相等时才删 `epoch ≤ N` 的标记,并用不同返回值区分四种结果:没有标记 / 删掉的是更旧代际 / 删掉的恰好是 `epoch == N` / 保留了更新代际(`inherit_absent` 等计数按此拆开)。写坏的标记一并删。
- **闸门三态**(在 `HandlePlayerAsyncLoaded` 的会话取消判定之后、建实体之前):已确认(**针对本 `ctx.ownerEpoch` 的那一次 A2 返回了非 -1**,不能用 `max(已确认, 在途)` 覆盖)→ 建实体;EVAL 仍在途(含"GET 先发、A2 后发"——`AsyncLoad` 对在途 key 不重发 GET)→ 把载入结果暂存在 pending 条目里等应答再判;失败 → 有上限重发(例如 3 次、≤5s、单调时钟、带退避),期间保留载入结果,到期仍失败才拒绝。返回 `-1` → 一律拒建实体、回 `kEnterSceneFailed`、不补发。失败回调必须清掉 in-flight 标志。不对 `enter_gs_type == 0` fail-open。
- **载入被放弃时补写**:本 lifecycle 删掉过 `epoch == N` 的标记、却没建出实体(会话在载入期间被 ExitGame 取消、载入报 RedisError、闸门拒绝且 EVAL 应答为空;A2 应答比放弃晚到时由应答回调按 lifecycle 处理),按 A1 同款条件脚本补写一份(`owner_epoch == N` 才 `SET "N:now"`),只发一次、走同一连接。安全性同 A1:本 lifecycle 从未建过实体,盘上仍是上一任持有者的终态。
- 补发入口:`RedisSystem` 重连回调(在 `playerRedis->OnReconnected()` 之前)与 1s 定时器(在 `RetryDuePending()` 之前)。

**配套**:`exit_release_stats` 命名空间(written / epoch_moved / skip_cause / skip_suppressed_release / skip_suppressed_identity / skip_relocate / skip_epoch_unknown / skip_identity_conflict / skip_handoff_inflight / failed;inherit_deleted_older / inherit_deleted_exact / inherit_absent / inherit_kept_newer / inherit_epoch_mismatch / inherit_failed / inherit_refused / inherit_rewritten;exit_superseded / exit_intent_missing / exit_resave / exit_resave_capped),挂 `redis.cpp` 的 30s 快照定时器打 `[ExitRelease]` 汇总行(INFO,有变化才打);**写入 / 删除成功的逐条日志打 INFO**(验证步骤要看),失败 WARN,拒建实体 ERROR;player_id 不进任何指标 label。`player_ownership_comp.h` 与 `handoff_mark_withdraw.h` 的 handoff 键契约注释补上新写入方 / 删除方;`enter-scene-zone-routing.md` Offline-Return 补"干净断线也会写标记";告警 `SceneManagerEnterSceneHandoffAnomaly` 注释与 runbook 补 stale_marker 口径变化(见残余)。

#### 12.6.4 两名复审的必须改项及其处理

复审一(归属 / 数据一致性,判"可落但须改"):1 个 P0、1 个 P1、1 个 P2、6 个 P3。复审二(活性,判"可落但须改"):1 个 P2、7 个 P3。全部已并进 12.6.3,逐条如下:

| # | 来源 / 级别 | 问题 | 规格里怎么处理 |
|---|---|---|---|
| M1 | 归属 P0 | Z1 修复"第一次落地即销毁"时,落地内容未必等于内存(停机期间客户端操作 / 战斗结算 / redis_client ERROR 时新旧颠倒);今天这些差额靠僵尸补存,修后永久丢失,A1 还把旧档认证成终态 | 12.6.3 第一步:落地内容与当前 marshal 比对 + 无在途 / 排队才销毁,否则更新快照并重存,有轮次上限、超限 fail-closed 保留实体;关掉退出中实体的改动入口(客户端消息只放行 ExitGame、战斗结算视同离线);修 `redis_client.h` 同 key 新旧颠倒或以"无在途 / 排队"绕开;验证清单增加两条对应的故障注入 |
| M2 | 归属 P1 | A2 不核 owner_epoch:不铸造的同物理节点落点与凭 A1 标记的铸造可同时成功,节点为活会话建出"出生即被废黜"的实体,首次存盘被拒、进度丢失、玩家被踢(超时后重试的同一客户端即可触发) | A2 Lua 先核 `owner_epoch == ctx.ownerEpoch`,不等返回 -1、不删任何标记;闸门只认"本 epoch 那一次 A2 返回非 -1";-1 一律拒建实体、回 `kEnterSceneFailed`、不补发;GET 先发 A2 后发时等本 epoch 的 A2 应答;补纯函数用例"核对不过不删标记" |
| M3 | 归属 P2 | GO-3 并存窗口(失租未察觉、同号新进程已起)里 `kClientDisconnect` 的 A1 挡不住,会给新进程的同代持有留下有效标记 | A1 加"本节点身份当前确认有效":疏散中一律不写(`skip_identity_conflict`)+ Node / EtcdService 只读 keepalive 信号;做不到后者时残余里如实写明 |
| M4 | 归属 P3 | 快路径只比上次落地快照、不看同 key 在途 / 排队存盘,A1 可能在旧写未落地时写出标记 | 快路径收尾前查 saving / pending,不满足改走真实存盘,落地后按 M1 规则收尾 |
| M5 | 归属 P3 | GO-2 回滚后同一个 epoch 数值再被铸一次,两节点同持 E+1,A1 给后者未存盘进度开放行口;A2 的 epoch 核对也挡不住 | 记残余;GO-2 "铸过的 epoch 不回退"另起任务(§12.3 GO-2) |
| M6 | 归属 P3 / 活性 P3 | ReleasePlayer 在铸造**之前**发出(先发 ReleasePlayer 后提交落点),"ReleasePlayer 只跟在铸造之后"的前提错;粘性压制会让落点失败时的玩家"无实体也无标记" | 生产口径不对 `kReleasedByTransfer` 粘性压制,改由 A1 的 owner_epoch 条件把关;dev 旁路由 scene 侧与 `AllowUnsafeCrossNodeHandoff` 同步的开关压制;`skip_suppressed` 拆成 release / identity 两个计数;"ReleasePlayer 打到活实体被静默踢下线"记残余(可选缓解:对非交接实体先 GET location,仍指向本节点就忽略——与 GO-2 有取舍,拿不准时保持现状) |
| M7 | 归属 P3 / 活性 P2 | A2 先删后建:载入被放弃(会话取消 / RedisError / 闸门拒绝)时标记没了、实体也没建,玩家重新掉进 18 环 | A2 返回值区分四种结果;ctx 记"本 lifecycle 删过 epoch==N 的标记";所有放弃出口与晚到的 A2 应答按 A1 同款条件补写一次 |
| M8 | 归属 P3 | 退出前发出的普通换图 EnterScene 仍在 scene_manager 排队,凭 A1 标记放行到一个会话已断的节点 | 记为 CPP-2 的一个新入口(残余),不为此推迟 A1 |
| M9 | 归属 P3 | 意图组件缺失时"按未被取代销毁是安全一侧"不成立(有 M1 差额即丢数据;真有新会话时踢掉活玩家) | fail-closed:保留实体、摘 UnregisterPlayer、ERROR + 计数、不写 A1 |
| M10 | 活性 P3 | A2 闸门遇 EVAL 错误零耐心;失败不清 in-flight;"同连接先 EVAL 后 GET 必已确认"不成立;R5 论据("断线后自动重发 GET")与 hiredis 实际行为(对挂起命令回空应答、`FailLoad(RedisError)`)不符 | 闸门三态 + 有上限重发(单调时钟、退避);失败回调清 in-flight;R5 论据改写成 ERROR 应答场景;残余写明"新铸 epoch 的落点被拒 = 与 CPP-2 相同的无实体状态";不对 `enter_gs_type==0` fail-open |
| M11 | 活性 P3 | "退出优先且交接 EnterScene 已发出"时,A1 的新标记可能被在途 EnterScene 用掉,把本可放行的局面变成卡住 | 该分支不写 A1,计 `skip_handoff_inflight`(复审给的选项 a;选项 b 的延迟表不采用,窗口极窄) |
| M12 | 活性 P3 | A1 上线后 `stale_marker` 会混入"上一任的释放标记还在、而新铸落点没载入"的登录重试,`SceneManagerEnterSceneHandoffAnomaly` 会指向错误原因 | 本节、告警注释、runbook 各补一条口径说明,排查先对照 `[ExitRelease]` 汇总行与 CPP-2 症状;压测复盘分开统计;后续(需 Go 授权)在铸造成功后 DEL 已消费标记 |
| M13 | 活性 P3 | 身份冲突 / 网络分区下迟到的 A1(黑洞连接里滞留的 SET)可在新进程接手后落地 | 记残余,注明同一场景下 X 迟到的存盘本身就会同代覆盖 X′(GO-3 既有问题,A1 只是多一个来源);可选收敛:A1 Lua 用 `redis.call('TIME')` 与 ARGV 毫秒比对、迟到数秒即拒(前提:Redis ≥5 且脚本走效果复制,需先确认) |
| M14 | 活性 P3 | 回滚 / 新旧版本并存窗口:旧版 scene 没有 A2,新版停机写的标记 300s 内对旧版进程上的同代持有有效 | A1 运行期开关(默认开);回滚步骤:先关 A1 → 等 ≥300s(标记 TTL)→ 再回滚二进制 |
| M15 | 活性 P3 | 验证计划:第 8 步 `ACL -eval` 同时拒掉 scene_manager 的落点 Lua,会假通过;第 9 步 `CLIENT KILL` 卡不准;成功日志只打 DEBUG 看不到;缺 ECS 级 Z1 回归用例;"不改 Go"与可选改 `ownerepoch.go` 注释矛盾 | 第 8 步改为 scene 侧 dev-only 故障注入开关(让 A2 回调按 ERROR 处理),做不到就删;第 9 步删除;成功日志改 INFO 或看汇总行;补 ECS 级用例(直接操作 tlsEcs 的测试宿主,构造"退出会话仍在 SessionMap + UnregisterPlayer + 意图组件"断言实体被销毁;无 Redis 宿主里 A1 必须走 dispatch 失败分支、不增加在途计数);本方案**不改 Go**,删去 `ownerepoch.go` 注释那一项 |

#### 12.6.5 不采用的替代方案

| 方案 | 为什么不采用 |
|---|---|
| **B**:退出后调 `LeaveScene` 释放 location | `LeaveSceneRequest` 只有 player_id / scene_id / request_id / zone_id,`LeaveScene` 只比 SceneId;同节点重连不铸造、不改 location 字节,"迟到的释放"与"新一段持有"在 scene_id / node / epoch 上完全相同,改成 compare-and-delete 也分不开。迟到的释放删掉在线玩家的 location 后,下一次换图成了首次落点、跳过换手门直接铸造 → 回档 + 废黜。释放走 gRPC,与重登的 EnterScene 并发,挡不住;要安全必须给 location 加持有代次(改 proto + Go + C++) |
| **C**:login 无显式去向时发 `ZoneId=0` | 只覆盖访客;同 zone 多节点、停机、顶号都不管。等待落点有效期内第二条腿持续失败时引入 R8 乒乓(每次从 A 区登录都被送回 B 区再失败)。"短断线回访客区"与"干净登出回家"语义不同,是产品问题,可另议 |
| **D**:客户端冷却 ≥ 租约 | 只对"断线后等够 30s"有效;不覆盖顶号、停机(玩家仍连着,没有租约)、robot 与其它客户端;afk_pass 一旦启用租约变 300s 即失效;每次失败都要等 30s 以上。只作临时缓解 |
| **E**:scene_manager 同节点见到当前代际标记就改为铸造 | 省不掉 A2(A1 的 SET 可能晚于 scene_manager 的判定);会扩大 §12.1-A 残留标记的影响面:未确认撤回的旧标记 + 同节点顶号 → 铸造 → 活实体在路由到达前的周期存盘被 CAS 拒 → 不存盘销毁 |

#### 12.6.6 为什么本轮不落码(历史记录,已被 2026-09-21 的用户授权取代)

> 2026-09-21 用户明确授权落码 Z1 + A′(以及 §12.6.9 的 epoch==0 铸造与 §12.7 的 GO-5),要求"按最标准的做法做,不用等我"。下面三条理由里,第 1 条由授权解除;第 2、3 条(先复现 Z1、先有编译基线)没有被满足,而是改为**验证顺序**的硬要求:Codex 必须先用修复前的二进制复现 Z1,再编译本批代码(§12.6.10 验证清单第 1–2 步)。

1. **范围明显扩大。**它已从"客户端加冷却"扩大为改动存盘 / 退出核心路径(`HandlePlayerAsyncSaved`、`HandleExitGameNode`、`FinishExitAfterPersist`、`ProcessClientPlayerMessage`、战斗结算在线路径)与 Redis 客户端重试队列(`redis_client.h`,头文件改动波及所有用到 `MessageAsyncClient` 的工程),约 10+ 个文件。AGENTS §10.2 要求"任务范围明显扩大"时停下等决策。
2. **复审要求先在编译好的构建上复现 Z1 再修**,而 Claude 不编译、不运行(AGENTS §10.1)。修法改变了"写盘退出后实体是否存在"这一基本行为,没有复现就落码,等于在看不见现象的前提下改核心路径。
3. **这批跨 zone 代码尚无任何编译基线**(§12.1–§12.5.6 全部未编译)。再叠一处核心路径改动,首次编译 / 首次联调的失败将无法定位到是哪一批引入的。

#### 12.6.7 落码顺序建议与验证清单(全部未执行)

> 2026-09-21:本批已一次落完(Z1 与 A′ 没有分成两个提交,都在 `9cef7b2ec` + 其后工作树增量里)。**实际执行以 §12.6.10 末尾的 Codex 验证清单为准**,它按代码实情修正了下面的第 3、6、9 步(日志顺序、`inherit_deleted_exact` 只在同节点出现、M15 要求的 dev-only 故障注入开关没有实现)。下文保留为规格原文。

顺序:① 先把现有代码(§12.4 + §12.5.5 + §12.5.6 的验证清单)编译、跑通 → ② 用修复前的二进制复现 Z1 → ③ 落 Z1(单独提交)并回归 → ④ 落 A1′ / A2′(单独提交)。每一步由 Codex 执行、结果回写本节。

1. **编译**:scene 库 → scene 节点 → cross_zone_test(改 `redis_client.h` 时先 engine,并编所有用到 `MessageAsyncClient` 的工程);MSBuild 串行 `/m:1 /nr:false`。
2. **单测**:`run_cpp_tests.ps1 -Build -Filter cross_zone`,新增纯函数用例(可写原因 / 各类跳过 / Merge 粘性 / 同一会话不算取代 / `IsSupersedingSession` / A2 核对不过不删标记 / 名表 / 两段 Lua 关键片段)+ ECS 级 Z1 回归用例(M15)全绿,既有 `HandoffMarkWithdrawQueue.*` / `CrossZone*` / `TravelOutcomeReset.*` 不变。
3. **复现 Z1(修复前二进制,单 scene 节点)**:robot 登录、改一点数据后断线。期望 `ignoring stale UnregisterPlayer … live session <退出会话>` 且无 `Destroying player`。修复后期望 `Player marked for unregistration` → `Destroying player`。
4. **M1 故障注入**:a) 退出存盘在途时注入战斗结算 / 客户端背包操作:修复后实体要么收敛后才销毁(看 `exit_resave`),要么操作被拒(退出中只放行 ExitGame),**结算不被销账后丢失**;b) Redis 返回 MISCONF(`CONFIG SET` 制造写错误)期间让一名"有周期存盘在退避中"的玩家断线,恢复后核对盘上是退出那一刻的内容。
5. **回归**:`robot login-test` 23/23;双 zone `travel_smoke` 期望 `TRAVEL_SMOKE_OK`。
6. **单节点 A1 / A2**:断线后 `GET player:{id}:handoff` 形如 `E:ms` 且 E == `GET player:{id}:owner_epoch`;30s 内重登后出现 A2 删除的 INFO 日志(或 `[ExitRelease]` 汇总行 `inherit_deleted_exact` +1),handoff 键为 nil。
7. **同 zone 两节点(`AllowUnsafeCrossNodeHandoff=false`)**:让 X 负载高于 Z,玩家在 X 上断线后立刻重登:无 18,owner_epoch 变为 E+1,Z 载入后 handoff 键为 nil。
8. **访客**:`dev-start-zones 1,2`,travel-smoke 去 2 区后断线,立刻从 1 区入口重登:落在 1 区、epoch +1、无 18,2 区旧场景人数已扣减。
9. **A2 fail-closed**:只用 scene 侧 dev-only 故障注入开关让 A2 回调按 ERROR 处理(**不要**用 `ACL SETUSER default -eval`,它会同时拒掉 scene_manager 的落点 Lua,假通过):期望有上限重发后拒建实体、回 3023、日志 `refusing`;关掉开关后重登成功。
10. **A1 跳过**:dev 旁路下跨节点换图,旧节点计 `skip_suppressed_release` 且无 SET;`BeginSceneDrain` 计 `skip_relocate`,改派不出现 withdrawn;交接 EnterScene 在途时断线计 `skip_handoff_inflight`。
11. **停机**:有在线 robot 时 SIGTERM scene 节点:每个玩家一条标记;"Shutdown persistence barrier complete" 出现在 A1 在途数归零之后,不靠看门狗。
12. **回滚演练**(M14):关 A1 开关 → 等 ≥300s → 换回旧二进制,期间无同代标记放行。
13. 失败时保留首个 error 及前后 20 行;不连续重试、不注释断言。

#### 12.6.8 残余风险(A′ 落地后仍存在)

- GO-3 在 300s 标记 TTL 之后的情形原样保留;CPP-2 类"location 指向一个从未载入玩家的节点"(交接在途时退出、M8 的排队换图、M10 新铸落点被拒)不因 A′ 消失。
- 身份冲突 / 网络分区下迟到的 A1(M13)与 GO-2 同值再铸(M5)只写进残余,各有另案。
- dev 旁路下 A1 的 SET 恰好晚于"无标记不铸造"的落点与新节点 A2 时会留下同代旧标记——只在 dev 下可能,由 scene 侧 dev 开关压制缓解。
- ReleasePlayer 先于落点到达活实体时的"静默踢下线"是既有问题(M6),A′ 只保证不再因此卡住。
- 退出存盘在途时重登仍回 18,这是正确的拒绝(盘上还不是最新),下一次重试即放行;客户端 2–5s 退避可选,不需要 ≥30s。
- 拿不准:Z1 回档时序与"队伍系统把僵尸当在线"的实际频率;停机 drain 卡看门狗目前是推断。
- ~~需要用户拍板:Z1 修复带来的行为变化(12.6.3 第一步末尾);访客短断线后从归属区入口重登 = 回家~~ **2026-09-21 用户已拍板**:Z1 修复连同其行为变化一并落码;访客重登去向改为"重连窗口内回原处(B),窗口外或主动登出回家(A)",见 §12.7(取代本条原先"= 回家"的默认)。
- ~~(2026-09-21 落码后新增)M3 身份探针比规格弱~~ **已收紧(2026-09-21 主会话追加)**:探针改用新增的只读查询 `EtcdService::IsIdentityConfirmedFresh`(`cpp/libs/engine/core/node/system/etcd/etcd_service.{h,cpp}`),四条全满足才判有效:lease 已授予、不在重注册中(keepalive 回 TTL≤0 之后到分配键 CAS 裁定之前一律判无效 —— 原探针在这段窗口里会因 `OnLeaseGranted` 把 ACK 时间置为 now 而误判有效)、注册流未停、最近一次 keepalive ACK 在 TTL/2 内(毫秒精度)。原先的三条差距(TTL 而非 TTL/2 且秒级截断、无重注册条件、租约未授予判有效)全部消除。**残余**:本地 ACK 在 TTL/2 内时 etcd 侧租约至少还剩 TTL/2,正常不会被接手;只剩租约被 etcd 提前撤销(人工 revoke / etcd 数据恢复)而本地尚未收到 TTL≤0 的窗口。同一窗口里旧进程迟到的同代存盘本身已会覆盖新进程(GO-3 既有问题),A1′ 只是多一个来源。新查询**没有单测**(`EtcdService` 的状态全是私有成员且依赖 `gNode`,测它要为测试开口子,违背 §11.2);验证靠 §12.6.10 清单里的重注册故障注入。
- **(落码后新增)A2′ 在路由不带 owner_epoch 时照样清**(改用不核对归属的脚本,§12.6.10 偏差 9),代价:这类路由删掉当前代际后载入又被放弃时不补写(不知道 N),只影响活性。
- **(落码后新增)活僵尸 / 退出被保留的实体遇到 epoch 跳 ≥2 才丢弃重载**(`player_exit_intent.h:208-211`),前提是正常运行中不存在"没有中间持有者却跳 ≥2"的路由;scene 侧没找到反例,scene_manager 的所有铸造路径**没有逐条核对,拿不准**。缓存 epoch 为 0 不判废黜:兼容窗口里一个真被废黜、缓存又是 0 的实体会被复用(该窗口里存盘本来就跳过 CAS,不是新开的口子)。
- **(落码后新增)退出中实体上是否还有未识别的逐帧改动源**:已关掉客户端消息、战斗结算、运动学三个入口;若 `[ExitPersist] exit_resave_capped` 非 0,说明还有,实体会 fail-closed 保留(不丢数据,但没修干净),拿不准。
- **(落码后新增)epoch==0 同节点铸造的两条残余**见 §12.6.9(铸造 EVAL 重放回 19、补种受阻时持续 19)。

#### 12.6.9 与帮会二期 B4c(玩家存盘属主围栏)的分工(2026-09-21 约定)

帮会二期 B4c 要根治 K1(旧节点退出存盘迟到、覆盖新节点已 durable 的资产改动 → 帮会捐献的钱回到玩家手里 = 复制,`docs/design/guild-phase2/04-asset-channel.md` §4.35)。它不另造 token 围栏,改为在本文的 owner_epoch 机制上补缺口。双方核对后的结论与分工:

- **owner_epoch 已堵住的**:跨节点版 K1。新节点铸出 E+1 后,旧节点的 Redis 存盘(guard=E)被 Lua 拒,DBTask(epoch=E)被 db 的 applied_epoch 守卫拒。
- **没堵住的三处**(都在同节点、同 epoch 下,CAS 区分不开):
  - (a) `redis_client.h` 同 key 新旧颠倒:ERROR 重试 / 成功路径会把更旧的值发在更新的值之后。**B4c 负责修**,修法以 §12.6.4 M1 第 (3) 条为准,并顺带提供只读查询"该 key 是否有在途 / 排队中的存盘"(接口定稿前先与本线对齐),§12.6.3 的 Z1 修复与 M4 直接复用。
  - (b) Z1 僵尸被同节点重登复用 → 旧内存覆盖盘上新进度。**本线负责**(§12.6)。*2026-09-21 已落码待验证(§12.6.10):写盘退出收敛后真的销毁;退出被保留 / 活僵尸在 epoch 跳 ≥2 时丢弃重载(`DiscardDeposedEntityOnReentry`)。*
  - (c) GO-2 回滚让 epoch 退回、之后同值再铸。**本线负责**(§12.3,要改协议,待拍板)。
- **epoch==0**:C++ 存盘不带 guard(`player_lifecycle.cpp` 的 owner_epoch 段,计 `owner_epoch_unknown`),db 按 `legacy_zero` 放行。B4c 会让资产改动类 RPC 在 epoch==0 时回 RETRY(本线同意,fail-closed)。代价:卡在 0 的存量玩家(存量 location + 一直落回同节点)在换一次节点之前资产操作会一直 RETRY。~~**本线待办**:scene_manager 在观察值为 0 时即使同节点也铸造(`enterscenelogic.go` 的 `mint: !samePhysicalNode` 改为 `!samePhysicalNode || observedEpoch == 0`)。安全性:持有节点此刻存盘本就不带 guard,不会被新值拒;路由到达后其 comp 更新为新值。**等首次编译之后再做**,B4c 设计里列为依赖。~~ **已落码(2026-09-21,随 `9cef7b2ec` 提交;Go 未编译、未测试)**,实现比上面的一行改法更保守,偏差三处:
  - (a) **只在 owner_epoch 键与 location 里记的 epoch 同为 0 时才铸造**:`sameNodeZeroMint := samePhysicalNode && observedEpoch == 0 && currentLoc.GetOwnerEpoch() == 0`,`mint: !samePhysicalNode || sameNodeZeroMint`(`go/scene_manager/internal/logic/enterscenelogic.go:542-543`,理由注释 `:477-515`)。键读到 0 而 location 记着 N≠0 = 持有节点缓存着 N、只是键被单独淘汰(deploy Redis 为 allkeys-lfu),此时铸出 1 会让持有节点带 guard=N 的存盘被拒、合法持有者被当废黜销毁。
  - (b) **这种情形先按 location 的 N 用 SETNX 补种键、不铸造**(`enterscenelogic.go:516-540`):补种出错回 8(`ErrRedis`),键已存在回 19(`ErrOwnerEpochConflict`,计 `epoch_conflict`),两种都先退回预占人数;补种成功后以 N 为观察值走不铸造的 CAS。
  - (c) **同节点 epoch 0 铸造后路由失败,只退 location、epoch 保留新值**(`restoreEpoch = placed.epoch`,`enterscenelogic.go:696-704`):没投递到时持有节点缓存仍是 0、存盘不带 guard,新值拒不了它;kafka-go 报错但 broker 已投递时缓存已是新值,退回 "0" 反而会让它之后的存盘被拒。代价:回滚后是"键=1、location 记 0",回滚 Lua 的 already_rolled_back 分支在 `ARGV[4]==ARGV[3]` 时不可达,go-redis 重发会被判 superseded、人数不退 —— 与既有"不铸造落点回滚"同类,不是新残余。
  - 单测(`go/scene_manager/internal/logic/owner_epoch_test.go`):`:614` `TestEnterScene_SameNodeSceneSwitchMintsWhenEpochIsZero`、`:656` `…ReseedsLostEpochKeyInsteadOfMinting`、`:694` `TestEnterScene_SameNodeReseedBlockedByZeroKeyRejectsWithoutErasingLocationEpoch`、`:731` `TestEnterScene_SameNodeZeroEpochMintRouteFailureKeepsMintedEpoch`;观察值非 0 不铸造的 `:582` 原样保留。**均未运行。**
  - 新残余:① 这次铸造不带 handoff 标记,铸造 Lua 没有重放识别;go-redis 在首发已执行、读超时后重发 EVAL 时回 19,目标 / 旧场景人数各漂一次、location 短暂指向目标场景,客户端重试即收口(`enterscenelogic.go:508-513` 注释);② (b) 中键值本身为 "0" 而 location 记着 N 时会持续回 19,直到持有节点带 guard 的存盘或干净登出把两边对齐(按现有写入方构造不出来,拿不准外部有没有单独写 "0" 的);③ 同落点重连(`samePlacement`)在第 4 步提前返回,不走这条铸造,存量 epoch 0 玩家要等一次换图 / 干净登出才会铸造,B4c 的资产 RPC 在此之前仍回 RETRY。
  - `owner_epoch.go:31-37` 文件头"持有者没换就不许推进"一段**没有**同步补"唯一例外"(go-sm 包无权改该文件),注释与代码暂不一致,待有授权的会话补一句"唯一例外:同节点、观察值 0、location 记的 epoch 也是 0 时铸造,见 §12.6.9"。
- **文件边界**:B4c 改 `redis_client.h`、`asset_op_system.cpp`、告警规则与一个连真 Redis 的围栏用例;`HandlePlayerAsyncSaved` / `HandlePlayerSaveRejected` 原则上只读,若必须改会先与本线对齐。本线不碰 `asset_op_system.cpp`;B4c 不碰 Z1、GO-2 与 `go/scene_manager`。
- **durable 判定的约束**(给 B4c 的提醒):存盘被 guard 拒之前活实体仍在服务、仍会改资产,这些改动随 `DestroyDeposedPlayer` 丢弃;且同 key 有更新 pending 值时旧值落地不发布 `save_callback_`(`redis_client.h` 约 :738-744)。资产操作的 durable 只能认"包含这笔操作的那一份"的落地回调。
- **日志耦合(本线承诺)**:`HandlePlayerAsyncSaved` 退出分支的 WARN 原文 `save outran reconnect lease`(`player_lifecycle.cpp` 约 :564,就在 Z1 要重写的防御判定一段里)被帮会 B4c 的值班 LogQL 引用(`docs/ops/cross-zone-failure-test-runbook.md` §9)。**Z1 落码时保留这条日志原文;非改不可时同批改 runbook §9 并知会帮会线。** B4c 在 runbook 末尾追加了 §9(v2.3),不改既有表格与判定,只在场景 E"唯一一处预期非 0"那句补了"限故障注入场景"的括注。**2026-09-21 落码后的实情**:原文 `save outran reconnect lease for player ` 一字未改(`player_lifecycle.cpp:1246`),runbook §9 未动;口径有一处变化需知会帮会线 —— Z1 修复后一次退出可能落地多次(重存轮次、超限保留后的周期存盘 / rekick),代码用意图组件上的 `leaseOverrunWarned` 去重,**每次退出只打一条**(`player_lifecycle.cpp:1235-1250`,`player_exit_intent.h:64-66`),所以 LogQL 计的仍是"事件数"而不是"落地数";意图组件缺失(fail-closed 分支)时无处去重,照旧每次落地都打。
- **B4c 已落的部分**(2026-09-21,未编译,由帮会线提交):`redis_client.h` 的 EnqueueSave 顶替分支(维持"同 key 在途 ≤1、排队 ≤1、排队比在途新")与 `HasUnsettledSave(const MessageKey&) const`(在 `save_callback_` 里调用恒为 false;只看两条存盘队列)——§12.6.3 的 Z1 修复与 M4 直接复用它;用例在 `cpp/tests/currency_test/currency_test.cpp`。门禁写在 `docs/design/guild-phase2/08-save-owner-fence.md` §8.3:共享 / 预发环境开启帮会资产操作前,Z1 与 GO-2 的修复须已落地(第 2、3 条)。
- **同类既有残余(归 login 批次,两线都不改)**:login 预加载 EXISTS → 无条件 SET(`ensure_player_all_data_async.go`、`sync_loader.go` 的 `rc.Set`),Redis 丢键且玩家在线时可用旧数据盖掉已 durable 的 blob;修法为 SetNX。记录在帮会 08 §8.4。

#### 12.6.10 落码记录(2026-09-21;C++ 未重新编译、未测试,Go 未编译、未测试)

代码分四包落码(C++ Z1、C++ A′、Go scene_manager、Go login),每包经"编译级静读 + 语义 / 归属 / 活性"对抗复审后回修;四包复审都已完成并回修,没有"复审未完成"的包。主体随用户手动提交 `9cef7b2ec`(提交说明"按用户明确要求先保存进度")进库,A′ 复审回修的增量在其后的工作树里(`cpp/` 下 8 个文件,未提交)。工作流结束后主会话对 `go/scene_manager` 又做过手工修正(见 §12.6.9 的 (a)(b)(c)),以代码为准。下文行号全部是 2026-09-21 工作树位置。

**落点速查**

| 部分 | 位置 |
|---|---|
| 意图组件 + 纯函数 | `cpp/libs/services/scene/player/system/player_exit_intent.h`:`ExitCause : uint8_t` + `kExitCauseNames` + static_assert `:34-82`;`IsSupersedingSession` `:112`;`MergeExitCause` `:145`;`DecideAfterPersist` `:172`;`ShouldRekickExhaustedExit` `:186`;`IsDeposedOnReentry` `:208`。**路径是 `player/system/`,不是交付说明早先写的 `player/comp/player_exit_intent_comp.h`** |
| A1′ / A2′ 纯判定与 Lua | `cpp/libs/services/scene/player/system/exit_release_mark.h`:`DecideExitReleaseMark` `:112`;`kLuaWriteIfOwnerEpoch` `:167`;开关 `:227-236`;`kLuaInheritClear` `:245`;`kLuaInheritClearUnknownEpoch` `:266`;`DecideAbandonedRewrite` `:378`;`DecideInheritGate` `:414`;重发上限 `:436-449` |
| 退出分支(Z1) | `player_lifecycle.cpp:1222-1364`(`HandlePlayerAsyncSaved`);WARN 原文 `save outran reconnect lease` 在 `:1246` |
| 退出发起 / 快路径 | `player_lifecycle.cpp:1730-1883`(`HandleExitGameNode`,M4 在 `:1827-1842`) |
| A1′ 写标记 | `FinishExitAfterPersist` `:1952-1960`(改派 → A1′ → 摘会话 → 销毁);判定采集 `:716-767`;条件写 `:654-709` |
| A2′ 清标记 | 发起 `cpp/nodes/scene/handler/rpc/scene_handler.cpp:257`(守护段内)→ `BeginInheritedMarkClear` `player_lifecycle.cpp:3397`;发送 `:959`;应答 `:844`;闸门 `:1112-1133`;重发 / 截止 `:3433`,由 `cpp/libs/services/scene/core/system/redis.cpp:43`(重连)与 `:57`(1s 定时器)驱动 |
| 汇总行 | `EnsureExitStatsTimer` `player_lifecycle.cpp:305-342`(`[ExitPersist]` 与 `[ExitRelease]` 两行,30s,有变化才打) |
| 各退出原因调用点 | `player_lifecycle_handler.cpp:38` kClientDisconnect;`main.cpp:163`、`:194` kNodeShutdown;`scene_node_service.cpp:176` kReleasedByTransfer;`player_lifecycle.cpp:2028` kIdentityConflict、`:2050` kSceneDrain;`s2s_player_scene_handler.cpp:45` 保持默认 kUnspecified |
| 单测 | `cpp/tests/cross_zone_test/cross_zone_test.cpp` 共 46 个 `ExitPersist*` / `ExitRelease*`(ECS 级 `ExitPersistEcs.*` 在 `:801-1005`,`ExitReleaseEcs.RouteWithoutOwnerEpochStillWaitsForTheClear` `:1382`) |

**实际实现与规格(12.6.3)的偏差,逐条**

1. **"被取代"分支不再 return**:摘 `UnregisterPlayer` 与意图组件、保留实体,快照照常在函数末尾更新(`player_lifecycle.cpp:1263-1283`、`:1372-1376`),因为那份字节确实落了盘。WARN 文案改为 `superseded by a newer session`(metric=exit_superseded)。**旧文案 `ignoring stale UnregisterPlayer` 只会出现在修复前的二进制里。**
2. **意图组件字段比规格多**:`suppressedBy`(把压制计数拆成 release / identity / unspecified)、`resaveRounds`、`travelHandoffMarkIssued`(M11 的另一条入口,`HandlePlayerAsyncSaved` 的"退出优先"分支摘交接组件时置位,`:1194-1202`)、`leaseOverrunWarned`(见 §12.6.9)。
3. **新增"退出即冻结运动学"**:`StopMotionForExit`(`:1692`,在 `HandleExitGameNode` 第一次存盘之前调用,`:1811`)清零 Velocity / Acceleration。规格没有这一条;不清的话 MovementSystem 逐帧推进 Transform,落地内容永远追不上内存,Z1 会以"超限保留"的形态回来。
4. **超限保留后新增第二个出口**:再来一次退出请求(停机 exitAllPlayers / s2s LeaveScene / 排空 / 疏散),且轮次已到上限、无未落地存盘时,补发一次存盘并清零轮次(`ShouldRekickExhaustedExit`;`:1759-1782`,计 `exit_exhausted_rekick`)。停机时周期存盘已取消,没有它就只剩看门狗。
5. **新增"再入时丢弃被废黜的旧实体"**:`DiscardDeposedEntityOnReentry`(`:2837`,调用点 `scene_handler.cpp:200-205`)。路由带来的 owner_epoch 比实体缓存值**至少大 2**(= 中间有别的持有者)时,不论实体是退出中还是没退出的活僵尸,都不存盘销毁、从 Redis 重载;恰好 +1 照常复用;缓存为 0 不判(兼容窗口)。这是对 §12.6.1(d)"僵尸被复用成真身 → 回档"的直接防线,规格里没有。计 `exit_deposed_on_reentry`。
6. **M4 是"强制写一次",不是"等在途存盘"**:快路径判"不用写"、但该 key 还有未落地存盘时,调 `SavePlayerToRedisImpl(player, false)` 真写一次;真写也发不出去(交接已发起)时不同步收尾,等那笔在途存盘落地(`:1827-1842`,计 `exit_fastpath_deferred`)。
7. **汇总行位置与键名**:不挂在 `redis.cpp` 的 30s 快照定时器上,由 `player_lifecycle.cpp` 自己挂(`EnsureExitStatsTimer`,第一次有玩家退出或第一次发起 A2′ 时挂)。`exit_*` 计数在 **`[ExitPersist]`** 行,不在 `[ExitRelease]` 行。实际键:
   - `[ExitPersist] exit_superseded exit_intent_missing exit_resave exit_resave_capped exit_exhausted_rekick exit_fastpath_deferred exit_client_msg_rejected exit_deposed_on_reentry`(`:333-340`);
   - `[ExitRelease]` 先按判定名表输出 `attempted`(= 判定为"写"的次数,不是写成功数)、`skip_disabled`、`skip_entity_invalid`、`skip_intent_missing`、`skip_cause`、`skip_suppressed_release`、`skip_suppressed_identity`、`skip_suppressed_unspecified`、`skip_relocate`、`skip_handoff_inflight`、`skip_epoch_unknown`、`skip_identity_conflict`,然后 `written epoch_moved failed`,再按 A2′ 结果名表 `inherit_absent inherit_deleted_older inherit_deleted_exact inherit_kept_newer inherit_deleted_malformed inherit_epoch_mismatch inherit_reply_error inherit_reply_lost`,最后 `inherit_failed inherit_refused inherit_rewritten inherit_rewrite_skipped inherit_rewrite_failed inherit_rewrite_dropped_node_holds inherit_rewrite_handed_over`(`:274-303`)。规格的 `skip_suppressed` 拆成了三项。
8. **`save outran reconnect lease` 每次退出只打一次**(原文不变),见 §12.6.9。
9. **A2′ 对不带 owner_epoch 的路由不再跳过**:规格写"`ctx.ownerEpoch == 0` 跳过、闸门放行";复审(归属 major)指出跳过会让上一任的 A1′ 标记在新持有期间一直有效。实现改为发 `kLuaInheritClearUnknownEpoch`(不核对归属、永不返回 -1,只删代际 ≤ Redis 当前 owner_epoch 的标记,`exit_release_mark.h:266-278`;选择逻辑 `player_lifecycle.cpp:996-1001`),闸门同样等它确认(`DecideInheritGate` 去掉了 0 直接放行,`exit_release_mark.h:414-430`)。代价:这类清理删掉当前代际后、载入又被放弃时不补写(不知道 N)。
10. ~~M3 探针比规格弱~~ **已收紧**:探针改用新增的 `EtcdService::IsIdentityConfirmedFresh`(lease 已授予 / 不在重注册中 / 注册流未停 / ACK 在 TTL/2 内),见 §12.6.8。改了 engine 文件 `etcd_service.{h,cpp}`(只加一个 const 只读查询,不改任何既有行为)。
11. **M7 晚到应答补写加了"不覆盖更新一轮的持有者"**:已放弃那一轮的 A2′ 晚到、确认删掉(或可能删掉)epoch==N 的标记时,本节点已有该玩家实体 → 丢弃补写;有更新一轮的待入场条目 → 把补写交给它;都没有 → 当场补写(`DecideAbandonedRewrite`;`player_lifecycle.cpp:875-910`)。同节点重登不铸造,两轮同为 N,只靠 owner_epoch 条件拦不住。
12. **闸门重发参数**:首发 + 3 次重发共 4 次,且从首发起 ≤5s(单调时钟),退避 250→500→1000ms,由 1s 定时器 / 重连回调驱动;在途超过截止同样拒绝(黑洞连接不回空应答)。
13. **退出中实体的客户端消息**:除 ExitGame 外一律丢弃,回 `kFeatureUnavailable`(1006,common 段现成的业务拒绝码;tip 表没有"正在退出"专用码,按规定不新增),计 `exit_client_msg_rejected`(`scene_handler.cpp:488-501`)。战斗结算:退出中视同离线,走 `StorePendingSettlement`、不销账(`player_battle.cpp:1463`、`:1557`)。
14. **开关**:scene 侧 dev 旁路用环境变量 `SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF`(默认关,须与 scene_manager 的 `AllowUnsafeCrossNodeHandoff` 同步打开),`HandleExitGameNode` 合并原因时读它(`:1753`)。A1′ 总开关 `SCENE_EXIT_RELEASE_MARK` 默认开,进程内第一次用到时读一次,并打 INFO `[ExitRelease] SCENE_EXIT_RELEASE_MARK=… -> on|off`(`:533-547`)。
15. **没做的规格项**:
    - M15 要求的"让 A2′ 回调按 ERROR 处理的 dev-only 故障注入开关"**没有实现**。按 M15"做不到就删",验证清单改用 CLIENT PAUSE 撑截止(见下)。
    - M13 的 `redis.call('TIME')` 收敛没做。
    - ~~以下三处没改~~ **已由主会话补齐(2026-09-21)**:M12 要求的告警 `SceneManagerEnterSceneHandoffAnomaly` 描述(`deploy/k8s/scene-manager-alerts.yaml`,`stale_marker` 会混入上一任释放标记的登录重试、先对照 `[ExitRelease]`)、`handoff_mark_withdraw.h` 的键契约注释(A1′ 不进待撤回表、A2′ 按 owner_epoch 清)、`enter-scene-zone-routing.md`(规则 2 与 Offline-Return 改为 GO-5 后的行为,含"干净断线也会写标记")。另补 `owner_epoch.go` 文件头的"唯一例外"(epoch==0 同节点铸造)。
16. **子代理越出包授权的一处**:`redis.cpp:43`、`:57` 的两处 `RetryInheritedMarkClears` 调用不在 C++ Z1 包的授权清单里,但位置正是 12.6.3 第三步规定的,已随 `9cef7b2ec` 提交。**保持现状**(用户已授权本线"按最标准的做法做、不用等我";挪走就得在 `player_lifecycle.cpp` 自挂 1s 定时器,与 §12.1-A 撤回的挂法分家)。

**M1–M15 落实位置**

| # | 落实 | 位置 |
|---|---|---|
| M1 | 比对后才销毁 + 重存(上限 5 轮,超限 fail-closed 保留)+ 关掉客户端消息 / 战斗结算 / 运动学三个改动入口;复用 B4c 的 `HasUnsettledSave` | `player_lifecycle.cpp:1296-1362`、`:478-507`(marshal 与剔探针比对)、`:490-494`、`:1692` / `:1811`;`scene_handler.cpp:488-501`;`player_battle.cpp:1463`、`:1557` |
| M2 | A2′ 先核 owner_epoch,不等返回 -1、一个都不删;闸门只认本 epoch 那一次;-1 拒建、回 `kEnterSceneFailed`、不补发 | `exit_release_mark.h:245-258`、`:414-430`;`player_lifecycle.cpp:932-953`、`:794-817` |
| M3 | 疏散中不写 + `EtcdService::IsIdentityConfirmedFresh` 探针(lease 已授予 / 不在重注册中 / 注册流未停 / ACK 在 TTL/2 内;2026-09-21 收紧,见 §12.6.8) | `player_lifecycle.cpp:558-565`;`main.cpp:83-91`;`etcd_service.{h,cpp}` `IsIdentityConfirmedFresh`;`exit_release_mark.h:154-157` |
| M4 | 快路径有未落地存盘时强制真写 | `player_lifecycle.cpp:1827-1842` |
| M5 | 记残余(GO-2 另案) | §12.6.8 |
| M6 | 生产不对 `kReleasedByTransfer` 粘性压制;dev 开关开着时压制;压制计数拆三项 | `player_exit_intent.h:125-155`;`exit_release_mark.h:130-141`;`player_lifecycle.cpp:550-553`、`:1753` |
| M7 | 结果五分类;`rewriteEpoch` 记"删过 N";三个放弃出口与晚到应答补写,加 `DecideAbandonedRewrite` | `player_lifecycle.cpp:773-790`、`:875-930`、`:1051`、`:1108`;`exit_release_mark.h:361-385` |
| M8 | 记残余(CPP-2 新入口) | §12.6.8 |
| M9 | 意图缺失 fail-closed:保留实体、摘 `UnregisterPlayer`、ERROR + 计数、不写 A1′ | `player_lifecycle.cpp:1284-1294`;`exit_release_mark.h:122-125` |
| M10 | 闸门三态 + 有上限重发(单调时钟、退避、在途截止);失败回调清在途;不对 `enter_gs_type==0` fail-open | `exit_release_mark.h:387-449`;`player_lifecycle.cpp:823-842`、`:1112-1133`、`:3433-3471` |
| M11 | 退出优先且交接标记已写时不写 A1′(两条入口) | `player_lifecycle.cpp:1931-1935`、`:1194-1202`、`:732`;`exit_release_mark.h:146-149` |
| M12 | runbook 已补口径(§2.3、场景 R / Z);告警描述已补(见偏差 15) | `docs/ops/cross-zone-failure-test-runbook.md`;`deploy/k8s/scene-manager-alerts.yaml` `SceneManagerEnterSceneHandoffAnomaly` |
| M13 | 记残余,TIME 收敛未做 | §12.6.8 |
| M14 | 运行期开关,默认开;回滚步骤写在开关注释里 | `exit_release_mark.h:227-236`;`player_lifecycle.cpp:542-547` |
| M15 | 成功日志 INFO(`[ExitRelease] mark written` `:683`、`[ExitRelease][InheritClear] … result=` `:856`);ECS 级 Z1 回归用例;dev-only 故障注入开关未实现,验证步骤已改写;本方案不改 Go 的 `ownerepoch.go` 注释(Go 侧只改了 scene_manager / login 的业务逻辑,见 §12.6.9 / §12.7) | `cross_zone_test.cpp:801-1005` |

**复审回修要点**

C++ 两包共 16 条:改 10 条,补注释 3 条,报待决 3 条。

- 放弃补写的竞态(→ 偏差 11)。
- `save outran reconnect lease` 改为每次退出只打一条。
- 缓存 epoch 为 0 时不判废黜。
- 计数键名统一为规格名并加 `exit_` 前缀(`inherit_deleted_current` → `inherit_deleted_exact`)。
- 旧版路由不跳过 A2′(→ 偏差 9)。
- 活僵尸也按 epoch 跳 ≥2 丢弃;函数改名为 `DiscardDeposedEntityOnReentry` / `IsDeposedOnReentry`。
- 补写计数拆出 `inherit_rewrite_dropped_node_holds` / `inherit_rewrite_handed_over`,`inherit_rewrite_skipped` 回到只表示"条件写时 owner_epoch 已变"。
- WARN 去重用例改成数日志条数(`LeaseOverrunWarnIsLoggedOncePerExit`,`:863`)。
- `HasUnsettledSave` 在落地回调里恒为 false,相关注释改成实情。

Go 两包:scene_manager 的用例 `ZeroZoneOverLocationInGateZoneBehavesLikeExplicitGateZone` 注明只是回归护栏;login 第 4 级去掉读会话 SceneID 的死代码。

**残余风险**

§12.6.8 全部条目,另加:

1. 放弃补写的粘合代码(`:876-910`)只能从 Redis 应答回调进入,测试宿主没有 Redis,只有纯函数单测。
2. `LeaseOverrunWarnIsLoggedOncePerExit` 用 `muduo::Logger::setOutput` 数日志。仓里有 `third_party/muduo` 与 `libs/engine/muduo_windows` 两份,按符号同名推断能生效,拿不准。
3. ECS 用例依赖测试宿主里 `MarshalPlayerForSave` 能跑、`FinishExitAfterPersist` 无副作用,拿不准。
4. 新 Lua 在 uint64 超大值、owner_epoch 被写坏时的行为只靠静读。

**Codex 验证清单**

按 M15 修正;全部未执行。C++ MSBuild 必须串行 `/m:1 /nr:false`。每步失败只保留首个错误及前后 20–30 行,不连续重试,不改断言。

1. **先复现 Z1(修复前二进制)**
   - 用本批之前编出的 `bin/`(没有就先检出 `15212c295` 编一份),单 scene 节点。
   - robot 登录,改一点数据后断线。
   - 期望出现 `ignoring stale UnregisterPlayer … live session <退出会话>`,且**没有** `Destroying player`。保留前后 20 行。
2. **编译**
   - 工作目录:仓库根。
   - 命令:`msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。
   - 通过标准:modules、scene 库、scene 节点、cross_zone_test 0 error。
3. **C++ 单测**
   - 先跑 `cross_zone_test.exe --gtest_filter=ExitPersist*:ExitRelease*:HandoffMarkWithdrawQueue.*:TravelOutcomeReset.*:CrossZone*`,再不带 filter 全量跑一遍。
   - 再跑 `pwsh tools/scripts/run_cpp_tests.ps1 -Build`:bag_test 的 10 个仍绿,其它包含 `player_lifecycle.h` 的测试工程能编过。
   - 重点用例:`SameSessionIsNotSuperseding`、`ExitSessionStillMappedIsDestroyed`、`LeaseOverrunWarnIsLoggedOncePerExit`、`ReentryDiscardsExitingEntityOnlyWhenDeposed`、`DeposedOnlyWhenEpochJumpsByAtLeastTwo`、`GateHasThreeStates`、`UnknownEpochLuaNeverRefusesAndOnlyDeletesUpToCurrent`、`AbandonedLateReplyNeverRewritesOverANewerHolder`、`RouteWithoutOwnerEpochStillWaitsForTheClear`。
4. **Go**
   - `go/scene_manager` 下依次:`gofmt -l internal/logic internal/metrics`(应为空)→ `go build ./...` → `go vet ./internal/logic/... ./internal/metrics/...` → `go test ./internal/logic/... -count=1 -run "TestEnterScene_|TestAwaitingPlacementExpired" -v` → `go test ./... -count=1`。
   - `go/login` 下依次:`go vet ./internal/logic/clientplayerlogin/` → `go test ./internal/logic/clientplayerlogin/ -run TestResolveEnterSceneRoute -count=1 -v` → 整包 → `go build ./...`。
   - `gofmt -l internal/logic/clientplayerlogin` 目前会列出 `deprecation.go`、`legacy_gate_killswitch.go`,均非本批文件。
   - 整包里 appearance / createplayer 相关的失败属于别的会话的在途改动,单独标注,不算本批回归。
5. **修复后复现 Z1**(做法同第 1 步)
   - 期望日志依次:`HandleExitGameNode: Player <P> is exiting the scene node (cause=client_disconnect)` → `HandlePlayerAsyncSaved: Saving complete for player: <P>` → `Player marked for unregistration: <P> (cause=client_disconnect, resave_rounds=0)` → `Destroying player: <P>`。
   - **随后**才出现 `[ExitRelease] mark written player=<P> mark=<E>:<ms> site=exit_release`:它是异步应答,晚于销毁。
   - `GET player:{P}:handoff` = `"E:<ms>"`,E == `GET player:{P}:owner_epoch`,TTL ≤ 300。
   - 30s 汇总:`[ExitRelease]` 的 `attempted` 与 `written` 各 +1;`[ExitPersist]` 行若出现,`exit_superseded` / `exit_intent_missing` / `exit_resave_capped` 应为 0。
6. **重登清标记**
   - 单节点 30s 内重登 → `[ExitRelease][InheritClear] player=<P> epoch=<E> result=inherit_deleted_exact`,实体建成,handoff 键为 nil。
   - 同 zone 两节点、被挑到另一节点 → 无 18,epoch 变成 E+1,新节点上是 `result=inherit_deleted_older`(不是 exact:路由带的是新铸的 E+1)。
7. **A1′ 各类不写**
   - 退出存盘在途时手动 `INCR owner_epoch` → `[ExitRelease] mark not written … owner_epoch moved on`,`epoch_moved` +1。
   - `SCENE_EXIT_RELEASE_MARK=0` 下重做第 5 步 → 汇总行 `skip_disabled` +1,无 handoff 键(逐条日志是 DEBUG)。
   - 交接标记已写之后断线 → INFO `[ExitRelease] not writing release mark … reason=skip_handoff_inflight`。
   - `BeginSceneDrain` → `skip_relocate`。
8. **A2′ 核对不过**(能造就做)
   - 载入前把 owner_epoch 改成不等于路由值。
   - 期望 ERROR `[ExitRelease][InheritClear] refusing entry … reason=owner_epoch no longer matches this route (clear returned -1)`,客户端收到 3023,handoff 标记没被删。
   - 时序难卡,拿不准;造不出记 SKIP。
9. **A2′ fail-closed**(替代 M15 的 dev 开关)
   - **不要**用 `ACL SETUSER default -eval`:它会同时拒掉 scene_manager 的落点 Lua,造成假通过。
   - 重登前执行 `docker exec redis redis-cli CLIENT PAUSE 8000`,让 A2′ 在途超过 5s。
   - 期望 ERROR `refusing entry … reason=inherited-mark clear deadline passed`,`inherit_refused` +1,客户端收到 3023;PAUSE 结束后重登成功。
   - PAUSE 同时卡住载入 GET 与 scene_manager,拒绝原因也可能是别的超时。拿不准,如实记录。
10. **M1 故障注入**
    - a) 用 R-PAUSE 撑住退出存盘时发背包操作 → `[ExitPersist] exit_client_msg_rejected` +1;战斗结算期间退出 → 结算走离线暂存、不销账,玩家下次进场补应用。
    - b) 用 Redis `CONFIG SET` 制造 MISCONF 写错误,期间让一名有退避存盘的玩家断线;恢复后核对盘上是退出那一刻的内容。
11. **停机 drain**
    - 有在线 robot 时停 scene 节点。
    - `Shutdown drain progress: … exit_release_marks=<n>` 归零之后,才出现 `Shutdown persistence barrier complete`。
    - 每名玩家一条 handoff 标记;`[ExitRelease] failed` 为 0。
11b. **M3 身份探针**(2026-09-21 追加)
    - 本地 `base_deploy_config.yaml` 是 keepalive 1s / TTL 180s,TTL/2 = 90s。
    - `docker pause etcd` 约 100s(> TTL/2、< TTL,不会触发租约过期与重注册),期间让一名 robot 干净断线;期望退出照常销毁,`[ExitRelease]` 汇总 `skip_identity_conflict` +1,无 handoff 键。`docker unpause etcd` 后数秒内再断线一名,期望 `written` +1。
    - 能造就做:pause 超过 180s 让租约过期,unpause 后节点走重注册(日志 `Lease keepalive returned TTL=0` → `Node re-registration successful`);两行之间断线的玩家同样 `skip_identity_conflict`。造不出记 SKIP。
    - 失败时保留 scene 日志里 `[ExitRelease]`、`Registration mode`、`Lease` 相关行。
12. **回滚演练(M14)**:`SCENE_EXIT_RELEASE_MARK=0` 滚动重启 → 等 ≥300s → 换回旧二进制。
13. **回归与联调**:`robot login-test` 23/23;双 zone `travel_smoke` 出现 `TRAVEL_SMOKE_OK`;再跑 runbook v2.4 的场景 Z(Z1 复现与修复验证)与场景 R(GO-5 重连落点)。

## 12.7 GO-5 决定:重连窗口内回原处,窗口外或主动登出回家(2026-09-21 用户授权)

**状态:已落码(随 `9cef7b2ec` 提交),Go 未编译、未测试;活性依赖 §12.6 的 A′(C++ 未重新编译、未测试)。**

**决定**:访客(归属区 A,正在访问 B)短暂断线后重登,**在重连窗口内回到原处(B);超出窗口或主动登出,回家(A)**。顶号(ReplaceLogin)同一规则:新设备接管角色当前所在的位置。

它取代了两处旧结论:
- §12.6.8 原先的默认"访客短断线后从归属区入口重登 = 回家";
- §12.6.5 把方案 C 判为"可另议的产品问题"。方案 C 的乒乓问题由下面的"等待落点不牵引"解决。

**窗口的定义**:就是服务端现成的断线租约,不另造。
- gate 断线时,login 以显式的 `LeaseTtlSeconds: 30`(`go/login/internal/logic/pkg/sessionmanager/session_manager.go:115`)把会话置成 DISCONNECTING。
- 租约内同账号重登,由 `DecideEnterGame` / `CanReconnect` 判为 ShortReconnect;别的在线会话判为 ReplaceLogin(`session_manager.go:170-185`)。
- 租约到期后,player_locator 调 `LeaveScene` 删掉 location(`go/player_locator/internal/logic/leasemonitor.go:304`),之后的重登是 FirstLogin。主动登出走 `MarkOffline`(`markofflinelogic.go`),同样删 location。
- 挂机月卡会把租约顺延到 300s(leasemonitor),所以窗口不严格等于 30s。
- **跨 zone 可见(2026-09-21 静态核实)**:从 A 区入口重登能看到玩家在 B 区留下的 DISCONNECTING 会话 —— 会话键 `player:session:{id}` 不带 zone(`go/player_locator/internal/logic/keys.go:7`),存在全服单一的 SharedRedis(`cross-zone-matchmaking.md` D12);`CanReconnect` 只比状态与账号(`session_manager.go:180-185`)。**部署前提**:各 zone 的 player_locator 必须连同一个 Redis;按 zone 拆开会让访客从 A 区重登一律判 FirstLogin、回 A(runbook 场景 R2)。

**为什么由服务端决定、不靠客户端记忆**:客户端崩溃重开、换设备都会丢掉"上次在哪个区"的记忆;location 是归属的唯一权威,scene_manager 本来就要读它过换手门。这样也不需要改 proto 或客户端。

**login 侧**(`go/login/internal/logic/clientplayerlogin/entergamelogic.go`)

新增纯决策函数 `resolveEnterSceneRoute`(`:646`,注释 `:615-645`),按四级优先级命中即止:

1. 持重定向票据且 `target_zone_id == 本 zone`(第二条腿,CZ-8)→ `ZoneId = 本 zone`(`:656-662`)。**必须排在第 2 级之前**:第二条腿登录时源 gate 已把会话置成 DISCONNECTING,decision 多半就是 ShortReconnect。
2. ShortReconnect / ReplaceLogin → `ZoneId = 0`、`SceneId = 0`(`:664-668`),日志 `[travel] EnterGame player=<P> decision=<d> 不指定去向(ZoneId=0),由 scene_manager 按 location 决定 gate_zone=<z>`。SceneId 清零,是为了不让 scene_manager 走 `GetSceneZone(SceneId)` 绕过 node_id 规则。
3. FirstLogin 且 `RedirectOnEnter` 命中 → 归属 zone(不变)。
4. 其余 → 本 zone(不变;能走到这里的只有 FirstLogin)。

其余要点:
- 请求在 `:738` / `:744` 取 `route.sceneID` / `route.zoneID`,GateZoneId 仍是本 zone。
- 单测在 `entergame_route_test.go`:`TestResolveEnterSceneRoute_Priority`(`:60`)、`_DecisionPremise`(`:187`)、`_NilLookupKeepsOwnZone`(`:200`),**均未运行**。
- "请求里的 ZoneId 确实取自 route"这一处接线单测够不到(`SendBindSessionToGate` 要真 Kafka 生产者),只靠读代码确认。
- "只送连接"的重定向返回 ErrorCode 0 + Redirect 非空,login 走成功出口,不推 3023。

**scene_manager 侧**(`go/scene_manager/internal/logic/enterscenelogic.go:323-346`)

`ZoneId == 0`(且没有能解析出 zone 的 SceneId)时:

- **location 落在具体节点上**(`GetNodeId() != ""` 且 `ZoneId != 0`)→ 跟随它的 zone。不是 gate zone 就走跨区重定向,目标就是玩家所在 zone,属 R7a"只送连接"(日志 `Cross-zone redirect only (ownership unchanged)`,`:1030`),归属不动。
- **"等待落点"(node_id 为空,跨 zone 传送途中)不牵引**,按 gate zone 落点。
  - 语义:传送途中断线 = 这次传送作废,回到最后稳定位置。
  - 等待落点没有持有者,在 gate zone 照常铸造 + CAS;第 3b 步仍按传送残留查归属(GO-1 纵深防御)。
  - 牵引的话,第二条腿持续失败时,每次从 gate zone 登录都会被送去目标 zone 再失败一次,来回重定向直到票据过期(复审指出过)。
  - **§10.2 的 R8 由此被取代。**
- **没有 location**、已被 `playerLocationOwnerGone` / `playerLocationOwnerDead` 过滤掉、或是 zone_id 为 0 的历史记录 → GateZoneId(不变)。

单测(`owner_epoch_test.go`,**均未运行**):`:1188` `FreshAwaitingPlacementWithoutZoneLandsInGateZone`、`:1234` `…RejectsUnmappedHomeZone`、`:1133` `CrossZoneRedirectIntoOwnZoneLeavesOwnershipUntouched`、`:1366` `ExpiredAwaitingPlacementNoLongerRedirects`;`:1280` 的 `ZeroZoneOverLocationInGateZoneBehavesLikeExplicitGateZone` 只是回归护栏。

**两侧必须一起上线**:只上 login 时,旧版 scene_manager 在 ZoneId==0 时仍会在票据有效期内牵引"等待落点",访客从归属区重连会被牵回 B,第二条腿持续失败时来回重定向。

**依赖 A′**:回到 B 之后,如果 B 的落点被挑到另一个节点,就是跨节点交接,要出示 handoff 标记。只有 §12.6 的 A1′(干净退出收敛后写 `"E:ms"`)能提供它,否则仍是 18。退出存盘还在途时重登回 18 是正确的拒绝,下一次重试即放行。

**客户端核实结论**(`mmorpg-client` 只读核实;下面"老问题"一条已由主会话修掉)

能跟上:
- msg 124 的唯一处理器 `GameClient.cs:1452-1474` 启动 `RedirectFlow`,它先 `++_pipelineGen`,旧的等进场协程按代次退出,不会误判超时。
- 旧连接的处理器在第一个 yield 前摘掉;新连接上完整重跑 Login + EnterGame,沿用当前角色;`CurrentZoneId` 取票据里的 zone。
- 一次访客重连只用 1 跳(第二条腿走票据分支,不再重定向),碰不到 3 跳熔断。
- robot 已实测能跟随登录期重定向(`docs/design/server-merge-gap-fixes.md:62`)。

拿不准:Unity 实跑登录期重定向没有找到证据。`RedirectOnEnter` 在生产上一直是关的,GO-5 之后这条路径不再受该开关控制。

老问题会更常出现:
- `RedirectFlow` 在 `GameClient.cs:883` 用 `_redirectPlayerId = PlayerId` 取当前角色,而 `PlayerId` 要到 EnterGame 应答所在协程的下一帧(`:679`)才赋值。
- EnterGame 应答与 msg 124 落在同一次 `Poll` 时它为 0,会退回选角,多角色账号可能进错角色。
- **已修(2026-09-21,`mmorpg-client` 未编译、未测试)**:发 EnterGame 前记下 `_enterRequestPlayerId`;`RedirectFlow` 改为 `_redirectPlayerId = ResolveRedirectPlayerId(PlayerId, _enterRequestPlayerId)`(已进游戏取当前角色,应答未到取本次请求的角色,两者皆 0 才回选角);断线清零。EditMode 用例 `CityTravelRequestTests.ResolveRedirectPlayerId_FallsBackToInFlightEnterRequest`。

**验证步骤**(全部未执行;runbook 场景 R 是可执行版本)

1. **同 zone 多节点快速重登**
   - zone 1 起两个 scene 节点,玩家断线后 30s 内重登。
   - login 日志出现 `decision=… ZoneId=0`。
   - 无 18(退出存盘在途时出现一次 18 可以接受,重试即过)。
   - 被挑到别的节点:epoch +1,新节点 `inherit_deleted_older`。挑回原节点:epoch 不变,`inherit_deleted_exact`。两种都不复用旧实体。
2. **访客在 B 区断线,30s 内从 A 区入口重登 → 回 B**
   - scene_manager(A) 出现 `Cross-zone redirect only (ownership unchanged): player=<P> target_zone=2`。
   - 客户端跟随 124 后,scene_manager(B) 出现 `Player <P> entered scene … (zone 2, home_zone 1, owner_epoch …)`。
   - 存盘 topic 仍是 `db_task_zone_1`。
3. **断线超过 30s(租约到期)→ 回 A**:从 A 入口重登是 FirstLogin,落在 zone 1 并铸造新 epoch;A 上 `inherit_deleted_older` 清掉 B 留下的 A1′ 标记。
4. **主动登出 → 回 A**:同第 3 条,不用等 30s。
5. **跨 zone 传送途中断线后重登 → 回 A**:第一条腿已放行(location 为等待落点)时从 A 入口重登,不牵引,在 zone 1 落点并铸造;第一条腿未放行时 location 仍在 A 的节点上,同样落 A。
