# 跨 zone 场景传送(玩家去别的 zone 的地图游玩)

**状态:** 阶段 1、2、3 的代码**全部已落码并进入 main**(2026-09-18),另打通了生产配置下客户端发起的跨节点换图。**全程未编译、未跑测试**(用户明确要求);导表与 proto 生成**已完成**(§11.4)。落码记录:阶段 1 见 §10,阶段 2/3 见 §11。§8 三个默认值按「不确认即按此执行」生效。收尾两轮见 §12(2026-09-20/21)与 §13(2026-09-28,同样未编译、未测试)。
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
  窗口是「源 scene 销毁实体 → 目标 zone 落点完成」之间,最坏约 60s(存盘与等应答两道看门狗各 30s,串行;
  常量现为 `travel_freeze_cap::kSaveBudget` / `travel_freeze_cap::kReplyBudget`,原 `kTravelReplyBudgetSec` 已删。
  另有 70s 冻结硬上限 `kFreezeCap` 兜住 zone Redis 不可用时的尾巴,见 §13.2)。成因不是本设计的疏漏,而是两个既有契约叠在一起:
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
| R11 | 应答对回玩家用 `EnterSceneResponse.player_id` 回显,不用 gRPC metadata | 少一层管线;去重缓存重放也不丢。*2026-09-28:按请求对应答改用 `EnterSceneResponse.correlation_id`(= 请求字段 10 的原样回显),player_id 只用来找实体;旧版 scene_manager 不回显时退回按 player_id,见 §13.1(未编译)* |

### 10.3 本阶段明确没做(已知限制)

- ~~生产下客户端发起的跨节点换图仍被拒~~ **已在 §11.2 打通**(把 18 当成「请先存盘并出示标记」,只改 C++)。疏散 / 排空本来就先存盘,阶段 1 已接上。
- ~~单节点硬崩遗留的 location 被 18 永久挡住、需运维清理~~ **已在 §11.5 解决**(属主节点已确认死亡 + 再入屏障已过 → 按无持有者落点)。*2026-09-20 复核:死节点的 node_id 被新进程复用并重新注册后这条接管不再触发,见 §12.3 GO-3。*
- `EnterScene` 每次多一跳 `data_service` 同步 RPC(含同落点重连),`data_service` 不可用即拒绝进场景。压测时看 `EnterScene` 分阶段时延;若成瓶颈再议缓存(归属只在合服时变)。
- 两段多键 Lua 依赖三把 `player:{id}:*` 键在同一 Redis 实例。键名无 hash-tag,**不支持 Redis Cluster**(与 CZ-2 的"物理共享单库"契约一致)。db / scene_manager / C++ scene 必须指向同一个 Redis。
- C++ 三个归属计数只进 30s 一行的 `[OwnerEpoch]` WARN,没进 Prometheus,`stress_summarize.ps1` 也不解析;C++ scene 侧新逻辑无单测(redis client 有)。
- 路由发送失败会把 epoch 退回旧值(非单调)。残余窗口:kafka-go 报错但 broker 实际已投递 → 目标节点拿着被收回的值载入,其存盘被拒后自毁,location 已指回源节点,玩家重连恢复,不会双主。*2026-09-20 复核:这段只分析了 Redis CAS,漏了 DBTask —— 被收回的 N+1 会反过来毒化 db 的 applied_epoch 守卫,见 §12.3 GO-2。* *2026-09-28:根治(回滚改为再 INCR、回滚回执、源端原子取证)设计已定稿、落码中,见 §13.7。*
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
- 已知残余风险(均不致双主,去留始终由 owner_epoch 比对裁决):~~应答只回显 player_id,5s 之后才到的迟到应答可能记到下一条请求头上~~ *(2026-09-28 已消除:应答按 `correlation_id` 对号,号对不上的一律丢弃;旧版 scene_manager 不回显号时仍存在,见 §13.1。在途 TTL 也已改为 SceneManager deadline + 1s,见 §13.0 约定 2;均未编译)*;`TeamId` 是 scene 从 Redis 投影来的缓存,刚入队未刷新时拦不住;副本节点上也允许发起跨 zone 传送(未做节点类型校验);交接在途时另一设备顶号落回同一节点的约 1 秒窗口。
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
| C. gate 对"同 scene_id、不同节点"的路由不转发进场(审计 CPP-1) | `ApplyRoute` 只按 `pendingEnterGsType` 与 scene_id 是否变化决定转发;world rebalance 迁频道沿用原 scene_id,规划与执行之间的竞态下玩家会落到"会话指向新节点、新旧节点上都没有实体",只能重登 | `RebindSceneNode` 把"比较旧指向 + 覆盖新指向"收进一个函数并返回 `SceneNodeChange`;节点变了也以 `enterType=0` 转发。旧指向无效(节点被摘除过)一律算换了节点。单测 `SceneRouteEntry` 共 16 个。*2026-09-28(CPP-2,§13.3,未编译):生产路径改为 `CompareSceneNode` + `AttemptPendingSceneEntry`,转发成功后才提交指向;`RebindSceneNode` 只留给已有用例钉住原语语义;`SceneRouteEntry` 现 17 个* |
| D. 告警与指标 | `SceneManagerEnterSceneRejectedSpike` 钉在无人发射的 `reason="scene_gone"`,恒空且 Prometheus 不报错;**另查出两条 critical 的 `*PoolEmpty` 同样恒空**(`nodes_by_role` 这个 gauge 从不发布 0 值,`== 0` 永远匹配不到,两条 pager 从未响过) | 按真实发射的 reason 拆成 5 条告警;`PoolEmpty` 改成 `count … unless on (zone_id) count …`;恢复 destroy-while-entering 分支的 `scene_gone` 发射点并配回原告警;`metrics.go` 的 Help / 注释与发射点对齐。现共 17 条规则,**未经 promtool 校验** |
| F. **scene 从不装 SceneManager 的 gRPC 应答处理器**(端到端闭环核查发现的 P0,详见 §12.5.1) | `InitSceneManagerReply()` 无任何调用点,`AsyncSceneManagerEnterScene/CreateSceneHandler` 恒为空,生成客户端静默跳过每一条应答。后果:同 zone 跨节点换图彻底失效(18 到不了 handler,玩家卡住零提示)、副本 / 镜像场景进入完全失效、跨 zone 传送源实体要冻满 30s、失败 tip 迟到 30s。**与本轮新代码无关,2026-04-16 的一次 regen 丢的** | 在 `cpp/nodes/scene/main.cpp` 装(生成物不能改,且生成器永远不会写它——谓词只收 `cc_generic_services`,scene_manager 是 grpc)。形状照 `id_segment_bootstrap.cpp` 的 `InitDataServiceReply()` |
| E. 文档 | `docs/ops/cross-zone-failure-test-runbook.md` 写着"每个版本上线前必跑",四个场景却全在测已删除的 reaper 链;`enter-scene-zone-routing.md` 教运维合服时手工清 location | runbook 重写为 v2(面向现行链路的 A–F 场景,观测点逐个 grep 核对,文件头声明"静态编写、未实跑");路由文档逐句核对改写,新增"合服后残留 location"小节;约 20 份旧设计文档与 12 个代码文件里把已删行为当现状的文字加了状态标注 / 改成实情(只动注释) |

### 12.2 新增的上线前置

- **对存量号开放跨 zone 传送之前,必须先跑完 `tools/merge_zone -backfill-home-zone`。** 12.1-B 之后,没有 `player:zone` 映射的玩家发起传送会被 20 拒绝(tip 后解冻,不受损)。新建角色由 `createplayerlogic.go` fail-closed 登记,不受影响;受影响的只有 2026-09-08 建角登记修复之前的号,以及映射 Redis 丢失的情形。
- dev 旁路 `AllowGateZoneAsHomeZone=true` 会绕过上述全部检查,在第二条腿上把访客记成目标 zone 归属,**只许单 zone 本地联调用**。
- **(2026-09-21 新增)K8s 的 scene-manager ConfigMap 必须配 zrpc `Timeout` ≥ `KafkaWriteTimeoutSeconds` + 回滚余量(例如 `Timeout: 8000`),或用 `MethodTimeouts` 只放宽 `EnterScene`。** 现状:本地 `go/scene_manager/etc/scene_manager_service.yaml:3` 是 `Timeout: 5000`;`tools/scripts/k8s_deploy.ps1` 生成的 scene-manager ConfigMap 没写 `Timeout`,落到 go-zero 默认 2000ms。两者都不大于 `KafkaWriteTimeoutSeconds: 5`(同一 yaml `:32`)。zrpc 服务端超时后调用方拿到 DeadlineExceeded,C++ 生成的 gRPC 客户端对非 OK 状态只打日志、不回调(`cpp/generated/grpc_client/scene_manager/scene_manager_service_grpc_client.cpp:141-153`)*(2026-09-28 起有失败回调,见 `docs/design/grpc-client-deadline-failure-callback.md`;交接路径上仍不作为证据,所以下面"冻满 30s"的结论对交接不变,见 §13.0 约定 1)*,所以**任何超过 2s 的第一条腿**都会让源 scene 冻满 30s 应答看门狗;若此时 gate(A) 对 gate-cmd 的消费又滞后超过 30s,一次"迟到但成功"的传送会被 §12.5.6 的踢线抢先踢回选服。`tools/scripts/k8s_deploy.ps1` 由有权限的会话去改,~~本轮未改~~ **已改(2026-09-28)**。
  **2026-09-28 已落码、未跑测试**(§13.5):本地与 K8s 统一为 `Timeout: 8000`(= `HomeZoneLookupTimeoutMs` 1500 + `KafkaWriteTimeoutSeconds` 5×1000 + Redis 往返余量 1500)。服务 yaml 是唯一真相,`k8s_deploy.ps1` 镜像 `Timeout` / `KafkaWriteTimeoutSeconds` / `HomeZoneLookupTimeoutMs` 三项(最后一项原是生成器写死的 1500)。守卫只有一处:`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的 `Get-SceneManagerTimeoutBudgetViolations`,对 ConfigMap 与服务 yaml 各判一次:三项须为正整数、满足不等式、**不得使用 MethodTimeouts**(生成器不镜像它,上文"或用 `MethodTimeouts` 只放宽 `EnterScene`"作废)。8000 只覆盖不触发同步建场景的正常路径;放宽后,挂在请求 ctx 上的同步 CreateScene / 选 gate 调用在下游挂住时能跑到 8s。login 的 `SceneManagerRpc` 客户端 Timeout 仍为 5000,未动。
- **(2026-09-28 更新)C++ 生成的 gRPC 客户端已改为"每次调用设 deadline + 非 OK 也回调失败处理器"**(`docs/design/grpc-client-deadline-failure-callback.md`)。本节与 §12.5.6 里"C++ 生成客户端对非 OK 只打日志、不回调""C++ 调用不设 deadline"的表述以此为准更新:
  - SceneManager 的 C++ deadline = `bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.SceneManagerNodeService`(10000),须 ≥ zrpc `Timeout`(8000)+ 2000。
  - **交接**的 EnterScene 传输失败 = 结果未知:`PlayerLifecycleSystem::DispatchEnterSceneTransportFailure` 按请求的 correlation_id 分发,命中交接只记日志,不当失败证据、不提前核实 —— 源 scene 仍由 30s 应答看门狗 / 70s 冻结上限按 owner_epoch 裁决,上面"冻满 30s 看门狗"的后果对交接**不变**。
  - **普通换图**的传输失败:摘在途槽,玩家发起的回 `kServiceUnavailable`(不断言失败,路由事件可能随后到达),队伍跟随只记日志;在途 TTL 改为 SceneManager deadline + 1s。
    - *(2026-09-28 补注)*曾存在、已消除:改之前在途 TTL 是 5s,小于 scene_manager 服务端 Timeout 8s。慢但活着的 scene_manager 下,同一玩家可能两条 EnterScene 并行执行,安全只靠落点 CAS 与关联号丢弃迟到应答(§13.1)。TTL 改为 `SceneChangeInFlightTtlMs()` = deadline 10000 + 1000 之后,这个窗口消除。
  - **镜像 CreateScene** 的传输失败:按请求 `creator_ids` 给本节点上的创建者回 `kServiceUnavailable`。

### 12.3 审计存活、本轮未修(已知限制)

| # | 严重度 | 限制 | 为什么没修 / 修法 |
|---|---|---|---|
| GO-2 | P2 | **路由失败回滚让 owner_epoch 回退(N+1→N),会反过来毒化 db 守卫。** `routePlayerToGate` 报错但 broker 实际已投递时:目标 B 拿着被收回的 N+1 载入,首次存盘 Redis CAS 被拒并自毁——但 C++ 存盘是先发 CAS、紧接着**无条件**发 DBTask(N+1),不等 CAS 结果;db 判 advance 落库并把 `applied_epoch` 记成 N+1(只升不降),此后合法持有者 A 的每笔 DBTask(N) 都被判 stale、ACK 丢弃。MySQL 停在 B 的快照上,与 Redis 分叉,直到下一次铸造(再铸得的 N+1 与 applied 相等 → 自愈)。每笔丢弃都有 `STALE-OWNER-WRITE rejected` 日志与计数,不是静默;真回档还需叠加 Redis 丢数据或有工具直读 MySQL | **2026-09-28:根治设计已定稿、落码中,见 §13.7。**定稿与下面的原设想不同:回滚再 INCR 一格(bump),同时把回滚回执写进 `PlayerLocation.rollback_receipt = 7`、把所凭标记转写到新 epoch;源 scene 用带 `#!lua` 的原子脚本先删本族标记再读,只凭"回执与本次标记原文一致"或"epoch 未变"解冻;`EnterSceneResponse.owner_epoch_after_rollback = 5` 只进日志、**不作采纳凭证**。次选(CAS 成功后再发 DBTask)不做。原记录:根治要让 epoch 严格单调:回滚时不 SET 回旧值而是再 INCR 一次,并经 `EnterSceneResponse` 新增的 epoch 回显字段交给源 scene 采纳——要改 proto + regen + 两端。次选:C++ 在 CAS 成功回调之后才发 DBTask。现有回滚单测断言"退回旧值",要随之改 |
| GO-3 | P2 | **死节点的 node_id 被新进程复用并重新注册后,§11.5 的接管永不触发。** `playerLocationOwnerDead` 要求"已从注册表消失",而 `PlayerLocation` 只记 node_id、不记进程实例;C++ `NodeAllocator` 取最小空号,复用是常态。此后该节点名下的遗留 location 只要解析到别的节点就回 18,指标只记 `no_marker`(不告警)。**不是永久卡死**:玩家断线约 31s 不重登,player_locator 的租约到期会调 `LeaveScene` 无条件删 location;真正卡住的是间隔 <30s 持续重试的客户端(每次 ShortReconnect 都撤销租约) | 需要在 `PlayerLocation` 里记属主进程实例标识(etcd 注册的 uuid / create_revision),属 proto 变更;用"注册时刻晚于 location.UpdateTime"做过渡判据会在 scene_manager 重启首次 fullSync 时误判活节点,不可取。*2026-09-21:断线释放标记(§12.6 的 A′)能在 300s 内缓解"干净退出后同号重注册"这一类,根因(location 不记进程实例)不变;见 §12.6*。**2026-09-28:本轮不落,见 §13.9 的理由**(定稿设计的残余 R1 会让健康的同号新进程在没发 SET 时被判"已替换",违反 I0 / I3;根治要改 C++ 或路由协议,需用户拍板) |
| GO-5 | P2 | **login 恒发 `ZoneId = GateZoneId = 本 zone`,§10.2 的 R7(只送连接)/ R8(等待落点牵引)从 login 路径不可达。** 访客在 B 区掉线后从 A 区入口重登:请求 `{ZoneId=A}`、location 在 B → 同 zone 落点 + 换手门 → 18。此时 B 的源实体早已在断线当刻存盘销毁,挡路的只是没人清的 location;只要客户端还挂在 gate A 上重试,`Reconnect` 就一直撤销断线租约,没人来删。同 zone 多节点下断线重登也有同类问题(`ReserveBestWorldChannelForEnter` 没有玩家亲和)。对应单测用的是不带 ZoneId 的请求形状,与生产脱节 | 二选一:login 在无显式去向时发 `ZoneId=0`;或 scene_manager 在 `ZoneId==GateZoneId` 且 location 在别 zone 时按 R7a 处理。`go/login` 本轮有别的会话在改,未动。*2026-09-21 更正:"B 的源实体早已在断线当刻存盘销毁"多半不成立——真写盘的断线退出会留下僵尸实体(Z1);"卡住"的根因修复(Z1 + 断线释放标记)见 §12.6,待决、未落码*。**2026-09-21:已落码、待验证** —— 用户拍板"重连窗口内回原处,窗口外或主动登出回家",login 侧发 `ZoneId=0`、scene_manager 只跟随落在具体节点上的 location(§12.7);卡在 18 的根因修复(Z1 + A′)见 §12.6 / §12.6.10。C++ 未重新编译,Go 未编译,均未测试 |
| CPP-2 | P2 | **RoutePlayerEvent 在 gate 侧丢失后没有任何补发。** scene_manager 以 Kafka ACK 为提交点就回成功,源 scene 据此不存盘销毁实体;gate 若找不到目标节点(打 ERROR 后 return),或目标节点的 TCP 正在重连(`RpcClient::CallRemoteMethod` 静默丢弃而 `ForwardPlayerToScene` 仍返回 true),玩家在线但无实体、无提示。登录链路同样存在,非传送特有 | **已落码(2026-09-28,未编译),见 §13.3「CPP-2 gate 侧进场转发补发」**:转发前核"已连上 + 在当前连接上握过手",交不出去记欠账,按 250ms→2s 退避补发(20s 预算,找不到节点 3s),超限推 `kEnterSceneFailed` + 踢线 34 + 强关连接;路由只在转发成功后才提交。"路由根本没到 gate"与 kSent 之后丢失仍不覆盖。原记录:需要"未连接如实报失败 + 有上限的补发 + 超限发 `kEnterSceneFailed` 并断开"的完整设计 |
| CPP-3 | P2 | **疏散 / 排空的改派 EnterScene 是 fire-and-forget**:票据发送前就删、发完立刻摘会话销毁实体;被拒(无可用节点 / 屏障未过 / data_service 不可用 / Kafka 失败)或无应答时,gate 会话还连着却指向尸体,没有 tip 也不踢线。换手门与归属查询上线后拒绝面变大了 | **2026-09-28:设计已定稿、落码中,见 §13.8。**原记录:需要"待确认表 + TTL + 用票据里的 sessionId 发 tip 并踢线",且整节点疏散时要并入 15s 收敛谓词 |
| 冻结上限 | P2 | 上一会话报的"交接冻结没有服务端上限"**大部分不成立**:已有存盘 / 应答两道 30s 看门狗,应答丢失、scene_manager 卡住、存盘回调不来都有界(最坏约 60s < 客户端 75s)。真正无上限的只有三段,共同前提是 zone Redis 不可用或半开:`ResolveTravelOutcome` 每 30s 无限重挂(U1);SET 已发但回调永不来(U2);DEL+GET 已发但回调永不来(U3)。另:看门狗计时用的是墙钟(muduo `runAfter`) | **已落码(2026-09-28,未编译),见 §13.2「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」。**下面说的"客户端收到后是否一定断线"(C-5)已由 §12.5.4 解除:客户端收到 34 一定断线回选服。另注:仓库里此前只有这一句摘要,并没有完整设计稿;定稿与落码见 §13.2。原记录:已有完整设计(70s 单调时钟硬上限 + 晚发闸 + "标记已写则销毁不解冻"),但到期后要给客户端发踢线消息,**客户端收到后是否一定断线重登未确认**,且 scene 无法强制 gate 断开(无此 RPC)。需客户端侧确认后再落 |
| 标记残留(12.1-A 的残余) | P3 | `IsSceneChangeBusy` 这道闸只管得住**本节点**替在线玩家发出的 EnterScene;不经本节点的跨节点落点(断线重登被挑到别的节点)在撤回确认前仍只有 TTL 兜底。源实体已销毁时盘上就是最终态,无害;有害的只剩 Abort 后就地解冻、又被顶号挑到别节点的极窄窗口 | 根治在 scene_manager 侧:普通 EnterScene 永不凭标记放行,改由请求显式"出示标记"(proto 变更) |
| GO-6 | P3 | `markNodeDeath` 写 `death_at` 失败后仍无条件 ZREM 负载集,而缺键是放行语义、进程首次 fullSync 又不产生本地观察记录 → 该节点整道再入屏障失效(玩家接管、孤儿副本销毁、世界频道改派同时放行)。需一次只打中 Setex 的 Redis 抖动 | **已落码(2026-09-28,未编译、未测试),见 §13.4「P3 收尾」。**实际做法与下面原文不同:death_at 写不进、或摘负载集失败时**就不摘**,进有截止时间的推迟队列(5s ticker 与每次 fullSync 驱动;写 death_at 满一个屏障后不带标记摘,ZREM 满 10 分钟放弃),`world_init` 两处改派补上 `!IsNodeAlive`,领导者上 `CanReclaimDeadNode` 对推迟态恒拒;**不是**"fullSync 返回 error 走 3s 重试"(理由见 §13.4)。原记录:"写失败就不 ZREM"是错的修法:fullSync 不是周期任务,死节点会永远留在负载集。要显式重试:fullSync 路径返回 error 走 3s 重试;watch DELETE 路径入队由 5s ticker 补写 |
| GO-4 残余 | P3 | `LeaveScene` 是非原子的 GET→比 SceneId→裸 DEL。审计给的主时序被推翻(租约到期路径有 cleanup-pending 屏障 + 单测守着),只剩 MarkOffline 路径上毫秒级、需双端并发的窗口 | **已落码(2026-09-28,未编译、未测试),见 §13.4「P3 收尾」**:按原文比对删除,删 location 与减人数在同一段 Lua 里提交;Redis 失败改回 error。原记录:改成 Lua compare-and-delete 即可,防御性加固 |
| 告警盲区 | P3 | zone 内 scene 节点**全灭**时两条 `PoolEmpty` 仍无从触发(没有任何 `nodes_by_role` 序列可供比较);`handoff_pending_no_marker` 刻意未配告警(它是换图正常第一跳),因此 GO-3 / GO-5 那种"只拒不放"目前没有告警能抓;新告警阈值均无实测基线 | **前者已落码(2026-09-28,未编译、未测试),见 §13.4「P3 收尾」**:已知 zone 与本实例 `ZoneId` 的四种角色都补 0,两条 `PoolEmpty` 追加整 zone 全灭分支,另加 warning `SceneManagerNodeDetachWithoutDeathMark`(规则共 19 条)。后者(no_marker 比值告警)本轮不落、等压测基线,口径与校准步骤见 §13.9。原记录:前者需 Go 侧为已知 zone 发布 0 值;后者可用它对 `enter_scene_stage_seconds_count{stage="update_loc"}` 的比值,口径待定 |
| Hiredis 泄漏 | P3 | `muduo_windows/.../Hiredis.cc` 在 `redisvAsyncCommand` 返回 ERR 时不释放刚 `new` 的 `CommandCallback`。既有问题,12.1-A 让"断开期间发命令"的路径多了一条 | **已落码(2026-09-28,未编译、未跑测试),见 §13.5**:muduo_windows 与 Linux overlay 两份 `Hiredis.cc` 同步(逐字节一致)。核对结论:hiredis 1.2 对非订阅命令的所有 ERR 路径(`async.c` 的 DISCONNECTING / FREEING、未订阅时的 UNSUBSCRIBE、OOM、格式串非法)都不登记 privdata,ERR 分支 delete 不会二次释放。更正"断开期间"的说法:出错断连时 hiredis 先 `_EL_CLEANUP`,适配器清空 channel_,`connected()` 已为假,`command()` 在 new 之前就返回;真正会漏的是 `~Hiredis` → `redisAsyncFree` 回调期间(FREEING)、显式 `disconnect()` 之后、格式串非法、OOM。回归用例:`rpc_controller_test` 的 `HiredisCommandLifecycle.*`(4 个)。原记录:返回 ERR 时 delete 即可,另案 |

观测缺口(runbook v2 §6 的 G1–G10)不在此重复,其中影响验收的两条:C++ 的交接计数没进 Prometheus、`stress_summarize.ps1` 也不解析;travel-smoke 没有"停在交接窗口 / 在窗口内退出"的开关,B / C / F 场景的时序全靠外部撑窗口。

### 12.4 验证清单(在 §9 / §11.4 基础上追加,全部未执行)

C++ MSBuild 必须串行 `/m:1 /nr:false`;这批代码从未编译,失败时先按行号区分是不是本节引入的。

1. `go/scene_manager`:`gofmt -l internal/logic internal/metrics` 无输出 → `go build ./...` → `go vet ./internal/logic/... ./internal/metrics/...` → `go test ./internal/logic/ -run "TestEnterScene_|TestAwaitingPlacementExpired|TestCheckHandoffCommitted|TestPlayerLocationOwner" -count=1` → `go test ./... -count=1`。重点 7 个:`TestEnterScene_TravelFirstLegRejectsUnmappedHomeZoneWithoutSideEffects`(4 子用例)、`…TravelFirstLegWithMappedHomeZoneStillReleases`、`…RedirectOnlyFirstLandingDoesNotQueryHomeZone`、`…TravelSecondLegRejectsUnmappedHomeZone`(2 子用例)、`…HomeZoneUnmappedSameZoneSceneSwitchStillFallsBackToGateZone`、`…ExpiredAwaitingPlacementStillRejectsUnmappedHomeZone`、`…SceneDestroyedWhileEnteringIsCountedAsSceneGone`(失败时区分"计数差值 ≠ 1"与"`Gather()` 报错")。
2. `cpp/libs/services/scene/scene.vcxproj` → scene 节点 → `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone`:`HandoffMarkWithdrawQueue.*` 7 个全绿,原有 `CrossZoneBagMarshal.*` / `CrossZoneFrozen.*` 不变。`cross_zone_test` 自 09-05 后没重跑过;exe 无输出且退出码 0xC0000135 = 缺 DLL。
3. `cpp/tests/routing_identity_test/routing_identity_test.vcxproj` → 运行 `--gtest_filter=SceneRouteEntry.*`(~~16 个~~ 2026-09-28 起 17 个,另有 CPP-2 的 `SceneEntryRetry.*` 11、`SceneLinkReady.*` 4、`SceneEntryAttempt.*` 9、`SceneEntryLateLogin.*` 5,见 §13.3)→ 全量 → `cpp/nodes/gate/gate.vcxproj` 0 error。重点看 `scene_route_helper.h` 新增的 `#include "proto/common/base/node.pb.h"` 在两个工程下能否解析。
4. `promtool check rules`(先取出 `deploy/k8s/scene-manager-alerts.yaml` 的 `spec.groups`):17 条,无语法错误。本机没有 promtool 就如实记录。*(规则数随后续批次增加:§12.5.6 后 18 条,2026-09-28 §13.4 后 19 条,以落码时 `grep -c -- "- alert:"` 为准)*
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
| S3L1-1 | `enterscenelogic.go:909-917`(工作树已移到 `:931-939`) | **已修(§12.5.6,未编译)。下面"双重故障才触发"的前提不准确**:主触发形态下源 scene 根本收不到 7(zrpc 超时不回调,只能等 30s 看门狗 *—— 2026-09-28 起超时会回调失败处理器,但交接路径上只记日志、不作证据,仍等看门狗,结论不变,见 §13.0 约定 1;同日 zrpc Timeout 已统一为 8000,见 §12.2*),且一次 Redis 读超时引发的铸造 EVAL 重放就能到达同一终态,详见 §12.5.6。原文:第一条腿 Kafka 推重定向失败后只有一层补偿:`rollbackPlayerPlacement` 回滚 location 与 epoch。该函数在 **Redis Eval 出错**时只记 Errorf 返回 false,epoch 停在 N+1 且无人重试;而应答里区分不出"已回滚 / 没回滚",源 scene 把 epoch 变化读成"已被放行",`DestroyDeposedPlayer` 不存盘销毁实体且全程不发任何客户端消息。双重故障才触发(Kafka 写失败 + Redis 回滚失败),但没有第二层出口。另一支(exact-value CAS 不过)销毁源实体本属正确,不算缺陷。 |

- **共同修法方向**(需要拍板,且 `go/login/**` 当前有别的会话在改,本轮未动):① 给 login 一条面向客户端的失败通道——最小做法是复用已有的 `KickSessionOnGate` 把会话踢掉,让客户端明确回到登录流程,而不是无声等待;② 或在 gate 侧加"已绑会话但 N 秒内没收到 RoutePlayerEvent 就踢"的看门狗(同时也能兜住 §12.3 的 CPP-2)*(2026-09-28 补注:"路由到了 gate 却没交给 scene"这一半已由 §13.3「CPP-2 gate 侧进场转发补发」覆盖(未编译);"路由根本没到 gate"那一半仍未做)*;③ S7-1 单独可低成本收敛:第一条腿在 `SceneConfId == 0` 时也做一次"目标 zone 有没有任何世界频道"的只读预检。
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
| CL-6 | P3 | `GameClient.cs:922-930` | **已落码(2026-09-28,客户端分支 `crosszone/client-cl6-cl8` 的 `54034fe`;客户端未编译、Unity 未跑;推送状态以客户端远端为准,见 §13.6)。** `DescribeTravelTip` 缺 3007 `kEnterSceneSceneNotFound`、3014 `kEnterSceneChangingScene` 的文案(枚举已生成),以及 `kSceneTransferInProgress`(在 `cross_server_error_tip.proto`,客户端没生成该枚举)。这些是同步拒绝,走响应体,**能正确收场**,只是显示成"传送失败(tip=3007)"。上一会话报过,属实。 | 补三条文案;`gen_proto.ps1` 收进 `cross_server_error_tip.proto` |
| CL-7 | P3 | `Assets/Scripts/Proto/Generated/SceneErrorTip.cs` | **已修(`.meta` 已入库,见 §12.5.6 末尾)。**该目录下唯一没有 `.meta` 的文件。纯枚举无 GUID 引用,编译不受影响,只会在每台机器首次打开 Unity 时各自生成随机 GUID、产生提交噪音。上一会话报过,属实。 | 有 Unity 的机器打开一次工程,提交生成的 `.meta` |
| CL-8 | P3 | 多处 | **已落码(2026-09-28,同上 `54034fe`;总预算改为 195 = 75 + 105 + 15,验票 / 等入场两处被拒或断线时当帧收口,见 §13.6)。** 跨区窗口总预算 120s 小于链路最坏耗时(约 165–180s),中途到期会清掉在途区号,是 CL-3 的又一触发源;验票被拒 / 新连接中途断开时管线协程不提前退出,`_redirecting` 会多挂 10–60s;`ZoneTravelClient.cs` 文件头仍写"跑 gen 之前编不过是预期"。 | 随 CL-3 一并处理 |

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

**对应条目状态**:§12.5.2 的 S5-B1 / S8-1 **已修**(服务端出口);§12.5.4 的 CL-1(⑨ 客户端面)、CL-3 / CL-4(⑩)**已修**;CL-6 部分(3007 / 3014 已补,`kSceneTransferInProgress` 等客户端下次 gen 收进 `cross_server_error_tip.proto`);CL-8 的 `ZoneTravelClient` 文件头已修。**仍未修**:CL-5(手动重进无冷却;根因修复见 §12.6,待决 —— *2026-09-21 已落码待验证,见 §12.6.10 / §12.7*)。*(2026-09-21:原列在这里的 CL-2、S7-1、S3L1-1、CL-7 已落码,见 §12.5.6。)* *(2026-09-28:CL-6 余下的 `kSceneTransferInProgress` 与 1006 文案、CL-8 的预算推导与管线提前收口已落码,见 §13.6。)*

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

1. **主触发形态下源 scene 收不到 7。** scene_manager 的 zrpc 服务端超时:本地 `Timeout: 5000`(`go/scene_manager/etc/scene_manager_service.yaml:3`);K8s ConfigMap 没写 `Timeout`,落到 go-zero 默认 2000ms。两者都**不大于** `KafkaWriteTimeoutSeconds: 5`(同一 yaml `:32`)。Kafka 写超时或 Redis 挂起时,handler 还没走到回 7,调用方已拿到 DeadlineExceeded;C++ 生成的 gRPC 客户端对非 OK 状态只打日志、不回调(`scene_manager_service_grpc_client.cpp:141-153`)。源 scene 只能等 30s 应答看门狗。只有"秒拒"型失败(broker 明确报错、Redis 也秒拒)才会带回显式 7。所以出口不能只认"显式失败应答"。*2026-09-28:本地与 K8s 已统一为 Timeout 8000(§12.2),Kafka 写 / 归属查询在各自超时处返回的失败会以 7 当场送达;超出预算的(kafka-go 元数据查询挂起、go-redis 重试、同步建场景)仍走 30s 看门狗,"出口不能只认显式失败应答"的结论不变。同日起 C++ 生成客户端对非 OK 也会回调失败处理器(`docs/design/grpc-client-deadline-failure-callback.md`),但交接路径上它只记日志、不作证据,结论同样不变(§13.0 约定 1)。*
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
2. **gate-cmd 消费滞后 >30s 时,迟到成功的传送会被踢回选服**:踢线走 scene→gate TCP 直达,先于 Kafka 上的 124 到达;数据不受影响,玩家多重登一次。K8s 下 zrpc 默认 2s 让"迟到"从"应答丢失"扩大到"任何超过 2s 的第一条腿",所以配 `Timeout` 列为上线前置(§12.2)。*(2026-09-28 已配 8000,见 §12.2)*
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

- GO-3 在 300s 标记 TTL 之后的情形原样保留;CPP-2 类"location 指向一个从未载入玩家的节点"(交接在途时退出、M8 的排队换图、M10 新铸落点被拒)不因 A′ 消失。*2026-09-28:gate 侧已收口(§13.3,未编译:转发前核握手、有上限补发、超限推 3023 + 踢线 34 + 强关连接),但"location 指向一个从未载入玩家的节点"这个状态本身不变。GO-3 本轮不落,见 §13.9。*
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

## 13. 收尾第二轮(2026-09-28)

**状态:本节列出的代码全部未编译、未测试**(AGENTS §10.1:Claude 没有运行任何构建、测试或 regen,由 Codex 按各小节的验证清单执行)。已落码的六项都已进入 main。GO-2 根治和 CPP-3 的状态是**设计已定稿,落码中**。GO-3 本轮不落。各项的定稿规格不在仓库里,本节就是它们的落档版本(AGENTS §5)。文中行号取自 2026-09-29 的工作树,会随并行会话漂移,查代码时请按函数名或日志原文检索。

### 13.0 总览、编号对照、跨会话约定

| 小节 | 项 | 状态 | 提交 |
|---|---|---|---|
| 13.1 | EnterScene 应答关联号(由组队线移交) | 已落码 | `9da27f9a4`(每小时自动保存,先带走了 proto 源与 `enterscenelogic.go`)、`4e409c5c3`(其余部分与窄面 regen)、`935ec83b1`(修正 C++ 描述符的 go_package) |
| 13.2 | 交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口(对应 §12.3「冻结上限」) | 已落码 | `09f71f9d5`(自动保存) |
| 13.3 | CPP-2 gate 侧进场转发补发 | 已落码 | `09f71f9d5` |
| 13.4 | P3 收尾:GO-6 / GO-4 残余 / 告警盲区 | 已落码 | `09f71f9d5` |
| 13.5 | Hiredis ERR 泄漏 / scene-manager zrpc Timeout / robot 空文件 | 已落码 | `9da27f9a4` 与 `24dd3e6c5`(均为自动保存)、`6941e7344` |
| 13.6 | 客户端 CL-6 / CL-8 | 已落码(客户端仓) | `mmorpg-client` 分支 `crosszone/client-cl6-cl8` 的 `54034fe`(基于 `8b21583`) |
| 13.7 | GO-2 根治(owner_epoch 严格单调) | 设计已定稿,落码中 | — |
| 13.8 | CPP-3 疏散 / 排空改派的待确认表 | 设计已定稿,落码中 | — |
| 13.9 | GO-3、`handoff_pending_no_marker` 比值告警,以及其余推迟项 | 本轮不落 | — |

**编号对照**。以下是代码和其它文档对本节的引用,落笔时逐条核过:
- **按标题引用的,已能对上**:
  - `travel_freeze_cap.h` 文件头引用的「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」一节,即 §13.2;
  - `pending_scene_entry_comp.h`、`scene_entry_dispatch.h`、`scene_route_helper.h`、`routing_identity_test.cpp` 引用的 "CPP-2 gate 侧进场转发补发",即 §13.3;
  - `deploy/k8s/scene-manager-alerts.yaml` 里 no_marker 注释引用的「P3 收尾」一节,即 §13.4;
  - `player_lifecycle.h` 的 `enter_scene_reply` 注释和 `team-system.md` 引用的「EnterScene 应答关联号」,即 §13.1。
- **按暂定号引用的,与本节编号不一致,需要改**:
  - GO-2 在制的 `proto/scene_manager/storage.proto`(`rollback_receipt` 的注释)、`proto/scene_manager/scene_manager_service.proto`(`owner_epoch_after_rollback` 的注释),以及 `go/scene_manager/internal/logic/owner_epoch.go`、`enterscenelogic.go`、`internal/metrics/metrics.go` 里写的 `cross-zone-scene-travel.md §12.8`,实际对应本文 §13.7;
  - `handoff-crosszone-20260920.md` 里写的「设计文档 §12.5.7」(CL-6 / CL-8),实际对应本文 §13.6。
  - 本文没有 §12.8,也没有 §12.5.7。

**跨会话约定**:
1. **gRPC 失败回调推翻了一个前提。** 本文多处以"C++ 生成的 gRPC 客户端对非 OK 状态只打日志、不回调"作论据,这个前提已被「让 C++ gRPC 客户端失败也回调并设超时」会话推翻(`docs/design/grpc-client-deadline-failure-callback.md`,模板 regen `897ac8241`,第二批 `b85f13c07`)。2026-09-28 起:
   - 每次调用都设 deadline。SceneManager 的 deadline 是 10000ms,取自 `bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.SceneManagerNodeService`,等于 zrpc Timeout 8000 + 2000。
   - 非 OK 时调用失败处理器。scene 侧的 `PlayerLifecycleSystem::DispatchEnterSceneTransportFailure` 按**请求**里的 correlation_id 分发;号为 0 时直接丢弃,不退回按 player_id 匹配。
   - 命中交接:只记日志,不当失败证据,也不提前核实。
   - 命中普通换图:先抄一份、再摘在途槽。玩家自己发起的,回 `kServiceUnavailable`;队伍跟随只记日志。
   - 疏散 / 排空的实体已销毁,只记日志。

   **在交接路径上,失败回调不作为证据**。所以凡是依赖这个前提推出的结论(源 scene 要等 30s 应答看门狗 / 冻结上限按 owner_epoch 裁决),对交接都不变。历史推理原样保留,只在各处加带日期的批注。
2. **在途换图 TTL 小于服务端 Timeout 的窗口,曾存在、已消除。** 在途换图的有效期原是常量 5s,小于 scene_manager 服务端的 Timeout 8s。scene_manager 慢但活着时,同一玩家可能有两条 EnterScene 同时在 scene_manager 里执行,安全只能靠落点 CAS,以及关联号丢弃迟到的应答(§13.1)。`b85f13c07` 把它改成运行期函数 `SceneChangeInFlightTtlMs()` = SceneManager deadline + `kSceneChangeInFlightMarginMs`(1000),即 11s,这个窗口随之消除。
3. **Battle 依赖的不变量 I3。** Battle 结算用 `PlayerLifecycleSystem::IsCrossZoneFrozen` 作"冻结期不结算"的闸,依赖不变量 I3:IsCrossZoneFrozen 为 false 的任何时刻,本节点一定仍是属主。本轮改动都没有改它的签名和语义。冻结上限的两个出口保证 I3 成立:SET 未发出时解冻,SET 已发出时销毁(§13.2)。GO-2 采纳回执另有论证(§13.7)。
4. **交接判定表的骨架**(与帮会线对齐,§13.7 展开):
   - I0:SET 未发出,就不可能被放行。
   - I1:SET 已发出,就有 Redis ⊇ 冻结实体的内存。
   - I2:SET 发出之后,唯一的解冻出口是读到与自己标记原文一致的回滚回执。GO-2 另加了"原子删除本族标记后读到 epoch 未变"一条,见 §13.7 的偏离 1。
   - I4:SET 发出前默认解冻,发出后默认销毁 + 踢线 34。
   - I5:回执 TTL 要大于源端的最大迟到。

### 13.1 EnterScene 应答关联号(correlation_id,未编译)

**问题**
- `scene_manager_response_handler.cpp` 只按 `resp.player_id()` 把应答送进 `HandleTravelEnterSceneReply`。
- 传送在途时,同一玩家的组队跟随、疏散、镜像自动进场、普通换图的应答,都会被当成传送应答处理,造成三件事:
  - 证据被污染;
  - 标记被撤回。真正那条交接请求随后回 18,落进"无交接"分支被丢掉,玩家看到的是传送莫名失败;
  - 跨 zone 且 epoch 已被铸造时,被污染的 kFailed 会让玩家被踢线。
- `requestedAtMs` 在 SET 发出前就已置位。所以"SET 在途、交接的 EnterScene 还没发"这段时间里到达的外来应答,也会被吃掉。
- §11.4 登记的残余"5s 之后才到的迟到应答记到下一条请求头上",根源相同。

**改法**:新增一个只回显的关联字段,不复用 `request_id`。

1. **契约**
   - `EnterSceneRequest.correlation_id = 10`,`EnterSceneResponse.correlation_id = 6`,类型都是 uint64。
   - scene_manager 只在 `EnterScene` 最外层的 defer 里原样回显它,与 `player_id` 放在一起。这个 defer 晚于去重缓存的写入,所以重放时回显的是本次请求的值。
   - `enterSceneRequestFingerprint` 先把副本上的这个字段清零再算指纹。它不参与去重,也不参与路由。
   - 应答字段 5 留给 GO-2 的 epoch 回显(§13.7);请求侧后续新字段从 11 起用。
2. **为什么不复用 `request_id`**
   - 收益为 0:每一代交接只发一条 EnterScene,去重永远不会命中。
   - 有真实代价:每条交接请求多一次 SETNX 和一次 EVAL,而这是热路径。
   - 会造出假的"执行中":go-zero 给 go-redis 设了 MaxRetries=3。读超时后 go-redis 会重发 SETNX,这次重发会对自己的 pending 报 InProgress,还会留下一个 60s 的占位。交接因此拿到一条号码匹配的失败应答,被白白撤回。login 的 request_id 去重也受这个问题影响,已另立任务,不在本节范围。
   - 契约冲突:幂等键要求同一逻辑操作重试时取同一个值;关联号要求每次发送都取新值,两者语义相反。
   - gRPC metadata 方案仍按 R11 不采用。
3. **统一发送出口**
   - scene 进程内所有 EnterScene 都只经 `player_lifecycle.cpp` 匿名命名空间里的 `SendCorrelatedEnterScene` 发出。在 generated 之外,这是唯一直接调用生成发送函数的地方。
   - 号由 `NextEnterSceneCorrelationId()` 分配:thread_local 单调计数,从 1 开始,恒非 0。
   - 公开入口 `PlayerLifecycleSystem::RequestSceneChange(player, smEntity, req, playerRequested)` 取代旧的在途登记函数。旧名直接删除,漏改的调用点在所有编译配置下都会硬报错。
   - 等待者把号记在自己的在途组件上:交接记在 `PlayerTravelHandoffComp.enterSceneCorrelationId`(0 表示交接的 EnterScene 还没发),普通换图记在 `PlayerSceneChangeInFlightComp.correlationId`。
   - 疏散 / 排空也取号,但只写进日志,不登记。
4. **分发**
   - `PlayerLifecycleSystem::DispatchEnterSceneReply` 调纯函数 `enter_scene_reply::Classify(replyTag, handoffInFlight, handoffTag, sceneChangeInFlight, sceneChangeTag)`,结果有七种(名表是 `kRouteNames`):
   - `travel_handoff` / `legacy_travel_handoff`:进交接裁决 `HandleTravelEnterSceneReply`(现已改为 private)。
   - `scene_change` / `legacy_scene_change`:先按值抄一份在途组件,摘掉,再交给 `HandleSceneChangeEnterSceneReply`。其中是原来段一的逻辑,一字不改:跟随只记日志;18 起同 zone 交接;其它错误码回 `kEnterSceneFailed`。
   - `scene_change_during_handoff`:按构造不可达。出现就丢弃并打 LOG_ERROR。
   - `unmatched`:丢弃。只有"有等待者,且应答是拒绝 / 重定向 / 发生在交接期间"的才计数(`IsSuspiciousUnmatched`)。路由先落地之后才到的纯成功迟到应答,只打 DEBUG。
   - `no_waiter`:打 DEBUG。
   - 外来的 redirect 也只记录、不销毁。
5. **滚动升级**
   - 应答号为 0,等价于旧版 scene_manager 没回显。这时退回改动前按 player_id 匹配的行为(`legacy_*`),**不 fail-closed**。
   - 不 fail-closed 的理由:fail-closed 会让窗口期内每次交接失败都冻满 30s;同 zone 放行后,源端冻结实体的 AOI 消息会串到已经在新场景的客户端(gate 栅栏只比对 player_id);看门狗无条件 DEL 会误删新节点的标记;`exit_wins` 也会失真。
   - 与改动前唯一的行为差别:"交接的 EnterScene 还没发"时到达的应答一律丢弃。这是严格的改进。
   - 结论:任意升级顺序、任意单边回滚,都不比改动前差。
6. **计数**
   - `[TravelHandoff]` 行末尾追加两项:
     - `reply_uncorrelated`:scene_manager 全部升级后应恒为 0。非 0 说明有 scene_manager 还是旧版,或者有发送点绕过了统一出口。
     - `reply_unmatched`:被丢弃的可疑外来应答。只看突增;不可达的 `scene_change_during_handoff` 也计在这里。
   - 号为 0 的应答第一次出现时,每个线程打一次 WARN `[EnterSceneReply] scene_manager 未回显 correlation_id,退回按 player_id 对应答…`。
   - `[TravelHandoff]` 的汇总定时器,现在在第一次发起交接时挂上,或者在第一次计这两项时挂上,以先到者为准。
7. **与 GO-2 的约定**
   - correlation_id 属于"这一次发送",在最外层 defer 里回显,不进去重缓存。
   - GO-2 的回显字段 5 只出现在 `ErrKafkaRoute` 错误应答里,本来就不进缓存。
   - GO-2 只读号码匹配(`travel_handoff`)的应答上的回显,而且回显只进日志,不作采纳凭证(§13.7)。
8. **同批修正**
   - `player_team.cpp` 已由组队会话过目:跟随改走 `RequestSceneChange(..., playerRequested=false)`,成功和失败都释放单槽;失败时仍只记日志,不弹 tip,不进传送裁决。
   - `935ec83b1`:`4e409c5c3` 生成 C++ 时把 proto_path 设成了 `generated/proto/_unified`,导致 `scene_manager_service.pb.cc` 内嵌描述符的 go_package 漂成了 `proto/scene_manager`。C++ 窄面 regen 必须用仓库根作 proto_path,与规范生成器一致;Go 那一步用 `_unified` 是对的。

**落在哪**
- `proto/scene_manager/scene_manager_service.proto`。
- `go/scene_manager/internal/logic/enterscenelogic.go`:defer 回显、`enterSceneRequestFingerprint`。
- `cpp/libs/services/scene/player/comp/player_ownership_comp.h`:新增两个字段。
- `cpp/libs/services/scene/player/system/player_lifecycle.{h,cpp}`:`enter_scene_reply` 命名空间、统一出口、`RequestSceneChange`、`DispatchEnterSceneReply`、`HandleSceneChangeEnterSceneReply`、两项计数。
- `cpp/nodes/scene/rpc_replies/scene_manager_response_handler.cpp`:只转发给 Dispatch;镜像自动进场改走 `RequestSceneChange`。
- `cpp/nodes/scene/handler/rpc/player/player_scene_handler.cpp`:只改守护段内。
- `player_team.cpp`。
- 单测:`cpp/tests/cross_zone_test/cross_zone_test.cpp` 的 `EnterSceneReplyRoute.*`(9 个)与 `EnterSceneReplyEcs.*`(8 个);Go 的 `logic_test.go` 新增 4 个,`owner_epoch_test.go` 新增 `TestEnterScene_ResponseEchoesCorrelationID`。
- 窄面 regen 的产物:`go/proto/scene_manager/scene_manager_service.pb.go`、`cpp/generated/proto/scene_manager/scene_manager_service.pb.{h,cc}`,以及 `generated/proto/{_unified,db,login}` 下的三份暂存副本。

**验证清单**(全部未执行,交 Codex)
1. **静态检查**,在仓库根执行:
   - `git grep -n "SendSceneManagerEnterScene" -- cpp ":!cpp/generated"` 应恰好 1 处,位于 `SendCorrelatedEnterScene` 里。2026-09-29 静读结果是 `player_lifecycle.cpp:393`。
   - `git grep -n "NoteSceneChangeRequested" -- cpp` 应为 0 处。
   - `git grep -n "HandleTravelEnterSceneReply" -- cpp/nodes` 应为 0 处。
2. **Go**:
   - 在 `go/scene_manager` 下依次跑 `go test ./internal/logic/ -run "TestEnterScene|TestEnterSceneFingerprint|TestIntegration_" -count=1` 和 `go test ./... -count=1`。
   - 在 `go/login` 下跑 `go build ./... && go test ./internal/logic/clientplayerlogin/... -count=1`。
   - 通过标准:全绿。新用例 `TestEnterScene_ResponseEchoesCorrelationID`、`TestEnterScene_ReplayEchoesCurrentCorrelationNotCached`、`TestEnterSceneFingerprint_IgnoresCorrelationID`、`TestEnterScene_SameRequestIDDifferentCorrelationIsInProgressNotConflict`、`TestEnterScene_CorrelationWithoutRequestIDNeverTouchesDedupe` 通过;既有的 `TestEnterScene_DirectSuccessReplaysFullCachedResponse`、`TestEnterScene_PendingDedupeReturnsRetryableErrorWithoutDeletingOwner`、`TestEnterScene_ResponseEchoesPlayerID` 不退化。
3. **C++**:
   - MSBuild 一律用 `/m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`,顺序为 proto 工程 → core → scene 库 → scene 节点 → cross_zone_test。之后再补编一次 scene 库的 Release|x64:它没开 /WX,用来发现只在 Release 下暴露的问题。
   - 然后跑 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter cross_zone`。
   - 通过标准:退出码为 0 且没有 `[FAILED]`。退出码非 0 但没有 `[FAILED]`,按崩溃处理;0xC0000135 是缺 DLL。
   - `SceneChange18StillStartsSameZoneHandoff` 和 `LegacySceneChange18StillStartsHandoff` 是 `StartTravelHandoff` 第一次在单测宿主里运行。如果 `aborted` 的增量不是 1,先查快路径有没有命中。
4. **集成**:
   - 同 zone 起两个 scene 节点,设 `AllowUnsafeCrossNodeHandoff=false`,做一次跨节点换图。§11.2 的日志序列里,`[ZoneTravel] requested EnterScene … corr=` 的号要与随后 reply 日志里的号一致;`[TravelHandoff]` 行 `reply_uncorrelated=0`、`reply_unmatched=0`;`stale_owner_write_rejected` 恒为 0。
   - 双 zone 跑 `robot -c etc/travel_smoke.yaml`,期望输出 `TRAVEL_SMOKE_OK`。
   - `robot login-test` 应为 23/23。
5. **可选:降级演练**。新 C++ 配旧版 scene_manager 跑同一个换图场景,期望:
   - 每条应答都让 `reply_uncorrelated` 加 1;
   - 18 仍能起交接;
   - 交接应答亚秒内收敛;
   - 不出现 `stale_owner_write_rejected`。

**残余**
- §11.4 的"迟到应答记到下一条请求头上"已消除。**旧版 scene_manager 不回显时仍然存在**(走 `legacy_*` 分支)。
- **未关联证据 1:EnterScene 3.2 的路由落点。** 同 zone 交接落回本节点时,证据来自路由而不是应答,不带号,仍以 `kSucceeded` 进 `ResolveTravelOutcome`。过了 TTL 才以同节点路由落地的跟随或换图,会让本代传送作废:标记被撤回、静默解冻,不回档。
  - 3.2 判断"交接已发起"时仍用 `requestedAtMs != 0`,所以 SET 在途窗口里同代的迟到路由也会走到这里。复审建议收紧为 `enterSceneCorrelationId != 0`,留给 GO-2。
  - 根治要让 RoutePlayerEvent / PlayerEnterGameNodeRequest 携带关联号,需要改 Kafka 与 gate 的契约。
- **未关联证据 2:"应答是我的,归属变化却不是我的"。** 一条在交接开始前发出、在 scene_manager 里拖着的外来请求,可能凭本代刚写的标记过门并铸造 epoch;本代自己的请求随后拿到一条号码匹配的拒绝,而 epoch 已经变了。GO-2 推断因果时,不能假定"号码匹配的应答 ⇒ epoch 变化由本代造成"。
- **3.1 按路由无条件摘在途换图组件**(既有问题):旧请求的迟到路由会摘掉较新请求的记录。较新请求随后收到的 18 变成 `no_waiter` 被丢弃,玩家要等 TTL 过后再重试。
- 号是 thread_local 的,进程重启后会重复使用。排障时要把 player_id 和号一起看。
- 在途换图组件仍然只有一个槽。`IsSceneChangeBusy` 仍然必要,理由从"应答会串号"改为"只有一个槽":第二次登记会覆盖第一次的号。

### 13.2 交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口(未编译)

**问题**。§12.3「冻结上限」一行列出了三段没有上界的冻结,核实后都成立:
- U1:`ResolveTravelOutcome` 在 Redis 未连接、应答形状不对、命令发不出去时,每 30s 重挂一次看门狗,次数和时长都不封顶。
- U2:SET 已发出,但回调一直不来。
- U3:DEL + MGET 已发出,但回调一直不来。

另有三处问题:
- 两道看门狗按墙钟计时:muduo 的 `runAfter` 在 Windows 上走的是 system_clock。
- `RequestTravelEnterScene` 在 SET 已拿到 OK 之后,遇到"无 gate 会话 / 无 SceneManager"两个分支直接 `AbortTravelHandoff` 解冻,违反骨架(SET 发出后只许销毁)。
- `StartTravelHandoff` 的快路径不查同 key 是否还有未落地的存盘,标记写下之后,一笔更旧的存盘仍可能以当前 epoch 落地,破坏 I1。

C-5(客户端收到 34 是否一定断线)已由 §12.5.4 解除。

**改法**
1. **第一刀的判据**
   - 判据是 `travel_freeze_cap::IsHandoffMarkSent(requestedAtMs)`,即 `requestedAtMs != 0`。
   - `requestedAtMs` 在 `command()` 之前就写入。命令没发出去、或 SET 收到 ERROR 应答时,会在同一个调用或同一个回调里同步 Abort 并摘掉组件。所以只要观察到非 0,SET 一定已进缓冲、已执行,或结果未知。
   - 不看 `markEpoch`:它在 `command()` 返回后才写,一旦漏写,用 `requestedAtMs` 判只会落到销毁一侧,属于 fail-closed。
2. **常量**,都在新纯头文件 `cpp/libs/services/scene/player/system/travel_freeze_cap.h`。时钟用 `steady_clock`。

   | 常量 | 值 | 含义 |
   |---|---|---|
   | `kSaveBudget` | 30s | 存盘看门狗 |
   | `kReplyBudget` | 30s | 应答看门狗;原 `kTravelReplyBudgetSec` 的下限约束注释搬到了这里 |
   | `kVerifyMargin` | 5s | 一次 DEL + MGET 核实往返的余量 |
   | `kFreezeCap` | 70s | 冻结硬上限 |
   | `kDispatchWindow` | 70 − 30 − 5 = 35s | 晚发闸 |
   | `kSweepInterval` | 1s | 上限扫描的周期 |
   | `kEarlyFireTolerance` | 1s | 看门狗提前触发的容差 |
   | `kClientAcceptedHandoffBudget` | 75s | 镜像客户端的 `CityTravelRequest.AcceptedHandoffBudgetSeconds`,两边必须同步修改 |

   四条 `static_assert`:
   - 存盘预算 ≤ 晚发窗口;
   - 窗口 + 应答预算 + 核实余量 ≤ 上限;
   - 容差 ≤ 核实余量;
   - 上限 + 扫描周期 < 客户端预算,即 71 < 75,余 4s。

   这 4s 要盖住网络往返。跨区时客户端从发请求之前就开始计时;同区换图是从受理应答到达才开始计,而服务端要等 scene_manager 回 18 之后才冻结,所以还要多盖住这一次往返。
3. **四个收口点**
   - 统一收口函数是 `ConcludeHandoffAfterMarkSent`,`MarkSentSite` 名表同时用作 `DestroyDeposedPlayer` 的 reasonTag,四个点如下:
     - `travel_freeze_cap`:1s 扫描发现冻结满 70s,且 SET 已发出。
     - `travel_dispatch_window`:`RequestTravelEnterScene` 发现冻结已超过 35s。这时 SET 已 OK,但 EnterScene 还没发。
     - `travel_no_gate_session`:`RequestTravelEnterScene` 发现 gate 会话已不在。
     - `travel_no_scene_manager`:`RequestTravelEnterScene` 发现没有可达的 SceneManager。
   - 收口的动作:
     - 会话还活着、且实体不在退出中时,先 tip 再踢线 34。跨区用 `kZoneTravelTargetBusy`,同区用 `kEnterSceneFailed`。34 的 reason.id 就是这个 tip 码。
     - 然后不存盘销毁。
     - 全程不发任何 Redis 命令,也不撤回标记。
   - 为什么不撤回标记:销毁之后,"ms 时刻的状态已落盘、本节点不再持有"已是事实,与断线释放标记 A1′ 同义。跨节点重登可以凭它过门,同节点重登由 A2′ 删掉它。
   - 为什么同 zone 也踢:到上限时 epoch 状态未知。除了"已放行、会话已改绑"这一种,其余情形(路由丢了、没放行、铸造后又回滚)不踢都会留下哑连接。
   - 收口时如果 `HasUnsettledPlayerSave` 为真,就推迟销毁(fail-closed),每次交接只报一次 ERROR,计 `destroy_deferred_unsettled_save`。这是上限**唯一**允许越过的例外。
   - SET 未发出一侧(扫描 kUnfreeze、set_mark 阶段的晚发闸)一律 `AbortTravelHandoff` 解冻并回 tip。
   - `RequestTravelEnterScene` 里原来那两处 Abort 已删除。
4. **快路径补写与 SM 预检**(均在 `StartTravelHandoff` 里)
   - 快路径补写:`SavePlayerToRedis` 判"盘上已是同一份"、但同 key 仍有未落地存盘时,强制调 `SavePlayerToRedisImpl(player, /*allowSkipWhenPersisted=*/false)` 真写一次,计 `handoff_fastpath_forced`。强制存盘也发不出去时,fail-closed Abort,reason 为 `could not force a save before writing the handoff mark`。写法与退出链的 M4 同构。
   - SM 预检:在冻结之前检查 `GetSceneManagerEntity`,为 null 时同步拒绝,打 WARN `[ZoneTravel] handoff not started for player … : no SceneManager node reachable`,不冻结。这样"SET 之后发现 SM 已消失 → 销毁 + 踢"只剩 SET 往返那几毫秒的窗口。
5. **看门狗按单调时钟复核**
   - `ArmTravelSaveWatchdog` / `ArmTravelReplyWatchdog` 改用 `RunAtMonotonic(steady 截止时刻, 原函数体)`。
   - 定时器醒来时,若离截止时刻还差 1s 以上,就按剩余时间重挂,计 `watchdog_early_fire`,打 WARN `[ZoneTravel] handoff watchdog fired …ms before its monotonic deadline`。
   - 设 1s 容差的原因:两只时钟的频率差和微秒截断,会造成毫秒级的伪提前;有了容差,正常运行不会抬高这个计数。
   - 墙钟前跳不会让任何判定提前。墙钟回拨仍会推迟所有 muduo 定时器,见残余。
6. **1s 扫描为什么自己挂定时器**
   - 扫描由 `EnsureTravelFreezeCapTimer` 在第一次 `StartTravelHandoff` 时挂上,回调调 `EnforceTravelFreezeCaps(now)`,先收集到期的玩家,再由 `ExpireTravelFreeze` 逐个处置。
   - 不借 RedisSystem 的节拍,与 §12.6.10 第 16 条的取舍不同。原因:那一条说的是 Redis 命令重试(撤回、A2′),它们必须和重连回调成对驱动;本扫描是生命周期截止时刻,不发 Redis 命令,也不该随 RedisSystem 的 BeginShutdown 一起被取消。
7. **补记冻结起点**
   - `PlayerTravelHandoffComp` 新增 `frozenAtSteady` 与 `destroyDeferralReported`。
   - `ElapsedSinceFreeze` 发现冻结起点未写时,就地补记,计 `freeze_unstamped`,打 ERROR,返回 0。所以漏写起点只会让处置最多晚 70s。
   - 规格里的 `enterSceneSent` 字段没有加,日志里的 `enter_scene_sent` 由 `enterSceneCorrelationId != 0` 推出。
8. **计数与日志**
   - `[TravelHandoff]` 行末尾追加 8 项:
     - `freeze_cap_reached`:恒 0;
     - `dispatch_window_closed`:接近 0;
     - `mark_sent_destroyed`:新终态;
     - `mark_sent_client_reset`:上一项的子集;
     - `destroy_deferred_unsettled_save`:恒 0;
     - `freeze_unstamped`:恒 0;
     - `watchdog_early_fire`:非 0 说明墙钟前跳过;
     - `handoff_fastpath_forced`:趋势值。
   - 终态等式改为 `started == granted + resolved_in_place + aborted + exit_wins + mark_sent_destroyed + 在途 + 交接中被 CAS 拒而销毁的`。
   - 逐条日志:
     - `[ZoneTravel][FreezeCap] player=… action=unfreeze … handoff mark was never sent`;
     - `[ZoneTravel][DispatchWindow] player=… stage=set_mark|enter_scene frozen_ms=…`;
     - `[ZoneTravel][MarkSentDestroy] player=… site=… target_zone=…(cross-zone|same-zone) mark=… enter_scene_sent=… corr=… evidence=… frozen_ms=… exiting=… kick=…: … (ownership UNKNOWN, not confirmed moved)`。site 为 `travel_freeze_cap` 时打 ERROR,其余打 WARN。
   - 随后 `DestroyDeposedPlayer` 会打出 `[<site>] local entity for player <P> destroyed without persisting (ownership moved away)`。这是沿用下来的原文,在这里的实际含义是"归属未知",判读以前一行 MarkSentDestroy 为准。
9. **与相邻机制的交互**
   - I0 的三处机制都没动:`IsSceneChangeBusy`、`ResolveTravelOutcome` 在同一连接上先 DEL 后 MGET、落点 Lua 复核标记原文。本项也不往待撤回表加任何条目。
   - 原有两处"SET 已 OK 却解冻"的违例改走销毁,I2 / I3 的违例面只减不增,`IsCrossZoneFrozen` 没改。
   - 与 GO-2 判定表的分工:ConcludeHandoffAfterMarkSent 在上限到期时**不读回执**。GO-2 定稿的判定表 B3 行也写的是"上限到期不读、判不清、销毁 + 34",不变量清单里"除非有匹配回执"那一句以 §13.7 的表为准。
   - 如果以后有设计要在受理(kTravelAccepted)与冻结之间插入异步等待,只能二选一:在受理之前等;或先 emplace 交接组件、在受理时刻就写 `frozenAtSteady`。否则客户端 75s 的前提会失效。

**落在哪**
- 新文件 `travel_freeze_cap.h`,已登记进 `scene.vcxproj` 与 `.filters`。
- `player_lifecycle.{h,cpp}`:`RunAtMonotonic`、`EnsureTravelFreezeCapTimer`、`ElapsedSinceFreeze`、`HasLiveGateSession`、`EnforceTravelFreezeCaps`、`ExpireTravelFreeze`、`ConcludeHandoffAfterMarkSent`、两处晚发闸、两道看门狗、`StartTravelHandoff` 的三处改动。
- `player_ownership_comp.h`、`player_frozen_comp.h`:注释与字段。
- 单测:`cross_zone_test.cpp` 的 `TravelFreezeCap.*`(8 个)与 `TravelFreezeCapEcs.*`(8 个)。
- 客户端的三处注释并入了 §13.6 的客户端提交。

**验证清单**(全部未执行,交 Codex)
1. **编译**:在仓库根执行 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64`。通过标准:scene 库、scene 节点、cross_zone_test、currency_test、bag_test 都是 0 error。`currency_test/asset_op_system_test.cpp` 与 `bag_test/player_battle_settlement_test.cpp` 逐字段 emplace 了 `PlayerTravelHandoffComp`,要能编过。
2. **单测**:
   - 先跑 `cross_zone_test.exe --gtest_filter=TravelFreezeCap*:TravelOutcomeReset.*:HandoffMarkWithdrawQueue.*:ExitPersist*:ExitRelease*:CrossZone*`,再不带 filter 跑全量。
   - 再跑 `pwsh tools/scripts/run_cpp_tests.ps1 -Build`。
   - 通过标准:新增 16 个全绿,既有用例的数量和结果不变。
3. **目检**(单测覆盖不到的部分):
   - `StartTravelHandoff` 的快路径补写与 `HandleExitGameNode` 的 M4 是否同构;
   - `RequestTravelEnterScene` 里是否已经没有 `AbortTravelHandoff`;
   - `ConcludeHandoffAfterMarkSent` 是否先踢、后调 `DestroyDeposedPlayer`,且全程不发 Redis 命令;
   - `RunAtMonotonic` 的 lambda 是否只捕获值;
   - `EnforceTravelFreezeCaps` 是否先收集、后处置,且没有用 `get_or_emplace`。
4. **回归**:
   - 用 `pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start-zones -Zones 1,2` 起双 zone,在 `robot/` 下执行 `.\robot.exe -c etc/travel_smoke.yaml`,期望 `TRAVEL_SMOKE_OK`。
   - 最后一行 `[TravelHandoff]` 里,上面 8 项除 `handoff_fastpath_forced`(如实记录)外都为 0,并满足 `started == granted + resolved_in_place + aborted + exit_wins + mark_sent_destroyed`。
   - 全程不出现 KickPlayer;runbook B1 仍是"解冻 + tip,不踢线"。
5. **H1:上限销毁**。robot 的预算只有 60s,看不到这一支,要用 Unity 客户端(DevAutoPilot `-travelZone 2` 或地图窗)。
   - 注入:先 `dev_tools.ps1 -Command go-svc-stop -GoServices scene_manager`(K-SM),在 SM 的 etcd 租约内发起跨区传送;scene 日志出现 `requested EnterScene` 约 25s 后,执行 `docker exec redis redis-cli CLIENT PAUSE 60000 ALL`。
   - 期望:
     - 在 `handoff started` 之后 70–71s,出现 ERROR `[ZoneTravel][MarkSentDestroy] player=<P> site=travel_freeze_cap … enter_scene_sent=1 … kick=1`,紧接着出现 `[travel_freeze_cap] … destroyed without persisting (ownership moved away)`;
     - 客户端出现 `[gate] kicked by server reason=<kZoneTravelTargetBusy>` 并回到选服;
     - `[TravelHandoff]` 行 `freeze_cap_reached=1 mark_sent_destroyed=1 mark_sent_client_reset=1`。
   - 恢复后:重登成功、金币等于出发时的值、`owner_epoch` 没被推进、没有 `stale_owner_write_rejected`。
   - 如果日志出现 `handoff not started … no SceneManager node reachable` 或 `site=travel_no_scene_manager`,说明 SM 已先被摘掉,记为 SKIP,调整 K-SM 的时机后重做。
6. **H2:晚发闸**(能造就做,造不出记 SKIP)。
   - 前提:让交接走快路径("handoff started"与"requested EnterScene"之间没有存盘落地)。
   - 注入:发起传送前一刻执行 `docker exec redis redis-cli CLIENT PAUSE 40000 WRITE`。
   - 期望依次出现 `[ZoneTravel][DispatchWindow] … stage=enter_scene`、`[ZoneTravel][MarkSentDestroy] … site=travel_dispatch_window … kick=1`;没有 `[ZoneTravel][WithdrawMark]`;`dispatch_window_closed=1 mark_sent_destroyed=1`。
   - 如果看到的是 `handoff save did not land in time`,说明没走快路径,记 SKIP。
7. **墙钟跳变不做**(改系统时间属于修改系统设置,AGENTS §10.2),只靠 `ShouldRearmEarlyFire` / `RemainingUntil` 的单测覆盖。
8. runbook v2.5 的场景 H 是本清单 5、6 两步的可执行版。

**残余**
- **"服务端最迟约 71s 出结论"有两个例外**:
  - 收口时有未落地存盘,会推迟销毁。redis_client 在 Redis 不可用时无限重试、从不放弃,所以这种推迟没有上界。按推理,快路径补写之后这一支不可达。
  - 墙钟回拨 X 秒,会把包括 1s 扫描在内的所有 muduo 定时器推迟最多 X 秒。这是引擎级问题,本项不修。
- **上限到期一律踢线,可能多踢一次**:
  - 同 zone:交接其实已放行、会话已改绑时,34 会断掉这条合法会话。gate 不核对 34 来自哪个节点,精确修法属于 §12.5.6 残余 1 的另案。
  - 跨 zone:34 可能先于一次迟到但成功的 124 到达。
  - 两种情况数据都安全,代价是玩家多重登一次。
- **行为变化**:"SET 已 OK 之后才发现没有 gate 会话 / 没有 SM"由"解冻 + tip"改为"tip + 34 + 销毁"。SM 预检已把常见情形挡在冻结之前。
- **销毁后迟到的 Redis 命令**:
  - 在途的 DEL(U3)可能才执行,把标记删掉,之后跨节点重登回 18,要等断线租约。只影响活性。
  - 半开连接里滞留的 SET 可能在 A2′ 之后才落地,与 §12.6.4 M13 同类。
- **SET 回调 ERROR 分支的代际问题**(复审发现,既有代码):它只在 `requestedAtMs` 相等时清 `markEpoch`,但随后的 `AbortTravelHandoff(playerId, "handoff mark write failed")` 不校验代际。上限销毁后,若玩家在同节点重登并发起新一代交接,旧 SET 迟到的 ERROR 会把新一代解冻并登记撤回,违反 I2 / I3。上限让这个前提变得可达,交 GO-2 判定表的属主定(§13.7)。
- **`RequestTravelEnterScene` 的会话判断仍比 `HasLiveGateSession` 弱**(既有):会话 id 有效、但 SessionMap 已不映射到本玩家时,照样发 EnterScene。
- **计数双计**:enter_scene 阶段的晚发闸先计 `dispatch_window_closed` 再调 Conclude;Conclude 若推迟,之后还会被上限扫描再计一次 `freeze_cap_reached`。按推理不可达。
- robot 的 60s 预算覆盖不到上限分支;C++ 计数只进汇总行,不进 Prometheus(runbook G1)。
- `cpp/nodes/scene/main.cpp` 里"冻结的超时恢复由它自己的应答看门狗负责"的注释已过时,待 CPP-3 那一步顺手改。

### 13.3 CPP-2 gate 侧进场转发补发(未编译)

**问题**。在 gate 侧,RoutePlayerEvent 丢失后没有任何补发。现状证据:
- `RoutePlayerEventHandler` 找不到目标节点时打 ERROR 就返回,会话仍指向原节点。
- `ForwardPlayerToScene` 调 `RpcClient::CallRemoteMethod` 后无条件返回 true,而后者在未连接时只打一条 ERROR 就丢。
- "TCP 已连上"不等于"可以交付":gate 连上 scene 之后,要过 **0.5s** 才发 NodeHandshake,scene 收到握手才建 RpcSession。缺这个 RpcSession 时,scene 发往客户端的推送(包括 NotifyEnterScene)都会被丢掉。所以连上后约 0.5s 内发出的进场,玩家会被载入,但进场通知会丢。
- 同 uuid 重新注册时,node_connector 直接 destroy 旧实体,不发 `OnNodeRemoveEvent`,会话指向就停在一个已销毁的实体号上。
- BindSession 晚到时,登录类型可能永远挂在会话上。
- 结果是玩家在线,却没有实体,也收不到任何提示。登录链路上同样存在,不是传送特有。

**改法**:转发前先如实判断链路是否可交付,交不出去就在会话上记欠账,按有上限的节奏补发,超限就收口。不改 proto,不改引擎公共接口,也不改客户端。
1. **不变量**
   - I1:一次路由决策最多转发一次。kSent 的含义是"已交给当前连接,且这条连接上已握过手",不代表 scene 已处理。欠账只在本地确定没发出去时续期,一旦 kSent 就清掉欠账,绝不重发。
   - I2:欠账和路由快照都存在 SessionInfo 里,只由主 EventLoop 线程读写;会话 erase 时随之消失。
   - I3:每次尝试都按业务 node_id 重新解析节点实体,不缓存实体号。
   - I4:截止时刻一律按 `steady_clock` 判断,muduo 定时器只负责唤醒。
   - I5:路由(sceneId + 节点指向)只在转发成功那一刻才提交。失败窗口内,会话仍指向上一次成功提交的节点,客户端消息和断线后的 ExitGame 都去旧节点。
   - I6:放弃时只做三件事:推 tip、踢线、关连接。
2. **状态**,在新头文件 `cpp/libs/services/gate/session/comp/pending_scene_entry_comp.h` 里:
   - `SceneForwardResult`,名表为 `sent / node_not_found / no_rpc_client / not_connected / handshake_pending / invalid_route`;
   - `SceneRouteTarget{playerId, nodeId, sceneId}`;
   - `PendingSceneEntry`。
   - SessionInfo 新增两个字段:`lastSceneRoute`(每次路由到达时无条件覆盖,供 BindSession 晚到时建欠账)和 `std::optional<PendingSceneEntry> pendingSceneEntry`。
3. **链路就绪 = 已连上 + 在当前连接上握过手**
   - gate 的握手应答处理器 `OnSceneNodeHandshakeReply` 在守护段内判断:应答为 `kCommon_errorOK` 时调 `gate_scene_entry::OnSceneLinkHandshaken(conn, uuid, now)`。
   - 它先核对应答所在的连接等于该节点 RpcClient 的 `GetConnection()`,不等就打 WARN 并忽略。
   - 核对通过后,在节点实体上 emplace `SceneLinkHandshakeComp{weak_ptr<TcpConnection>}`,打 INFO `[SceneEntry] scene link ready node_id=… uuid=…`,并把指向该节点的欠账提前到下一轮扫描。
   - 纯函数 `ClassifySceneLink` 按连接对象比对:重连之后旧章自然失效;握手失败时永远不就绪,这是 fail-closed。
   - 主会话已亲自核实:RpcClient 把自己 TcpClient 的消息回调绑到 GameChannel,应答原样带回 conn,所以"应答 conn == `RpcClient::GetConnection()`"成立。
4. **一次尝试**:纯逻辑 `gate_scene_route::AttemptPendingSceneEntry`,解析器和转发器都注入,可以单测。
   - 先过玩家围栏,再解析节点。
   - 解析成功后调 `CompareSceneNode`(只比较、不改写)→ `ApplyRoute` → 转发成功才 `SetEntityId` 并清掉欠账。
   - 失败时由 `RecordSceneEntryFailure` 决定重试还是放弃。
   - `RebindSceneNode` 在生产路径上已不再调用,只留给已有用例钉住原语语义。
5. **参数**,都在 `scene_route_helper.h`:
   - `kSceneEntryRetryBudget` 20s:muduo 断线后的重连时刻是 0 / 0.5 / 1.5 / 3.5 / 7.5 / 15.5s,每次连上后还要再等 0.5s 握手,停机 ≤16s 的节点都能赶上。20s 也远小于客户端等进场的 60s。
   - `kSceneEntryNodeDiscoveryBudget` 3s:找不到节点时只给这么多时间。
   - 退避:`kSceneEntryInitialBackoff` 250ms,每次翻倍,`kSceneEntryMaxBackoff` 2s 封顶。
   - `kSceneEntryMaxAttempts` 16,作纵深上限。
   - 扫描周期 `gate_scene_entry::kSweepIntervalSeconds` 0.25s。
   - 不加抖动:每次尝试只是本地判断,真正的远端副作用每个路由决策只有一次。
6. **取消**
   - 会话被 erase 时,欠账随之消失。
   - 每次尝试都先查客户端连接:不可用就撤销欠账,计 `session_closed`。
   - 新路由到达时,整体替换欠账并重新计时,计 `superseded`。
   - 玩家围栏不符时撤销欠账,计 `player_changed`,不踢线。
7. **收口契约**(`GiveUpSceneEntry`):
   - 打 ERROR `[SceneEntry] giving up session_id=… player_id=… entry_player_id=… target_node_id=… scene_id=… attempts=… first_failure=… last_failure=… elapsed_ms=…`。注意 `elapsed_ms` 的值后面紧跟着 `,推送 kEnterSceneFailed 并踢线、关闭会话`,没有空格。
   - 然后依次执行:`SendTipToClient(kEnterSceneFailed)`(msg 23)→ 踢线 34(reason 为 `kEnterSceneFailed`)→ `conn->shutdown()` → `forceCloseWithDelay(1.0)`。
   - 之后的断线回调发出的 ExitGame 落到旧节点,由那边按"SET 是否已发出"的骨架处置。gate 不做任何假设。
8. **入口**,只改守护段:
   - `RoutePlayerEventHandler` 调 `OnRouteDecision`;
   - `BindSessionEventHandler` 调 `OnLoginTypeBound`,晚到时凭 `lastSceneRoute` 建欠账(`EntryForLateLoginBinding`);
   - gate `main.cpp` 用 `sceneEntryRetryTimer` 每 0.25s 调一次 `RetryDueSceneEntries`。
9. **可观测性**
   - `[SceneEntry] forward deferred` / `forward recovered` 按 1/64 采样,第一条必打。
   - `giving up` 不采样。
   - `scene link ready` 每条链路每次握手打一行。
   - 30s 汇总行 `[SceneEntry] deferred= recovered= gave_up= superseded= session_closed= player_changed= links_ready= pending=`,有变化才打。
   - 原来逐条的 `RoutePlayer: scene node not found in registry` 等三条 ERROR 已删除(已核实没有下游依赖)。

**与其它节的关系**
- §12.5.2 方向②:本节覆盖了"路由到了 gate、却没交给 scene"这一半;"路由根本没到 gate"那一半仍然没做。
- §12.5.5 login 的 3023 是 EnterScene 被显式拒绝的出口,这时根本没有路由;§12.5.6 的 3027 + 34 由源 scene 发出。本节由 gate 发出 3023 + 34 并强制关连接,客户端走同一条收口路径。
- §12.6.4 M8 / M10 不在"本地没发出去"的判定范围内,不受影响。
- 与冻结上限的关系:放弃后的 ExitGame 落到旧节点,与"路由没到 gate 就断线"是同一条既有路径,本节没有新增任何解冻或销毁入口。

**落在哪**
- 新增:`pending_scene_entry_comp.h`;`cpp/nodes/gate/handler/event/scene_entry_dispatch.{h,cpp}`,已登记进 `gate.vcxproj`、`.filters` 与 `CMakeLists.txt`。
- 修改:
  - `scene_route_helper.h`:纯逻辑与常量;
  - `gate_event_handler.cpp`、`rpc_replies/scene_response_handler.cpp`:只改守护段;
  - gate `main.cpp`;
  - `session_info_comp.h`。
- 单测:`routing_identity_test.cpp` 新增 30 个:`SceneRouteEntry` 从 16 个增到 17 个,另有 `SceneEntryRetry` 11、`SceneLinkReady` 4、`SceneEntryAttempt` 9、`SceneEntryLateLogin` 5。

**验证清单**(全部未执行,交 Codex)
1. **编译**:
   - 依次执行 `msbuild cpp\libs\services\gate\gate.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64` 和 `msbuild cpp\nodes\gate\gate.vcxproj`(参数相同)。
   - 通过标准:0 error。
   - 重点核对:scene / common / login 三份 error_tip 头在同一编译单元里没有同名冲突;`scene_response_handler.cpp` 能解析 `handler/event/scene_entry_dispatch.h`;steady_clock 的 `std::min` 两侧类型一致。
2. **单测**:
   - 编 `cpp\tests\routing_identity_test\routing_identity_test.vcxproj`,先跑 `--gtest_filter=SceneRouteEntry.*:SceneEntryRetry.*:SceneLinkReady.*:SceneEntryAttempt.*:SceneEntryLateLogin.*`,期望 46 个全绿;再跑全量。
   - 该工程的 Debug|x64 没有 `/utf-8`,在 ACP 为 936 的机器上编译时要留意中文字面量。
3. **联调**,按本地开机 runbook 起全栈:
   - **基线**:每个 scene 节点各出现一条 `[SceneEntry] scene link ready`;`robot login-test` 23/23;gate 日志里没有 `[SceneEntry] giving up`,也没有 `failure=handshake_pending`。
   - **场景 SE**:
     - 操作:zone 1 只起一个 scene,测试号已在游戏内,用 `taskkill /F` 硬杀这个 scene,30s 内让客户端回选服重登。本地 `NodeTTLSeconds` 为 180,location 仍会指向这个节点。
     - 期望依次出现:`forward deferred … failure=not_connected` → 约 20s 后 `giving up … attempts=`(12–16)`last_failure=not_connected elapsed_ms=`(20000–20300)→ `Client disconnected`。客户端显示"进入场景失败,请稍后再试。请重新登录。"并回到选服。
     - login 直接推 3023、gate 没有任何 `[SceneEntry]` 行时,记为未命中。
     - 按启动器重新拉起 scene 后,应出现 `scene link ready`,再登录能正常进场。
   - **场景 SE2**(尽力而为):第二个 scene 刚注册时让一批 robot 登录。若命中 `forward recovered`,同一 session 必须真的进场,新节点日志里不得出现 `RpcSession not found for gate`。
   - runbook v2.5 的场景 SE 是可执行版。

**残余**
- **L1**:kSent 之后在路上丢失,检测不到。例如字节进了发送缓冲后连接断开,或 scene 处理前崩溃。登录时由客户端的 60s 兜底;游戏内没有兜底。二期可以用现成的 `Gate.PlayerEnterGameNode` 做确认看门狗。
- **L2**:kSent 之后被 scene 拒绝(A2′、载入失败、场景不存在),只推 tip、不踢线,会话已提交到新节点。
- **L3**:一条已投递、之后又被 scene_manager 回滚的路由,会在 ≤3s(找不到节点)或 ≤20s(连不上 / 未握手)内被补发,会话改绑到新节点 X,随后被 X 的 A2′ 拒建。若源端已凭回滚回执解冻,活实体会失去输入和 ExitGame,变成卡死加僵尸;不会双主。这一条已交给 GO-2,GO-2 定稿把它列为可接受结局(见 §13.7 残余"gate 迟到改绑"一条)。
- **L4**:跨会话乱序。旧会话还在重试时玩家顶号进了同一个 X,旧会话的迟到转发可能抢走新会话的实体。连接检查把窗口限定在"KickPlayer 迟到"之内。根治在 scene 侧:`PlayerEnterGameNode` 不应无条件摘掉在用会话,遇到过期 epoch 应直接拒绝。本轮不做。
- **L5**:20s 只盖得住短重连。gate→X 单向分区、X 在 etcd 里却仍健康时,会形成"每 20s 被踢一次"的循环。
- **L6**:就绪标记依赖握手应答路径;写不上的话 20s 后全员被踢。防线是每条链路的 `scene link ready` 日志,加上基线 login-test 必须零 give-up。
- **L7**:行为变化五处:
  - scene_id 为 0 的路由改为踢线;
  - 找不到节点改为 3s 后踢线;
  - BindSession 晚到的补发有上限,会踢线;
  - 未握手的转发被推迟;
  - 失败窗口内旧指向无效时,客户端请求收到 `kServiceUnavailable`。
- **L8**:放弃时的 ERROR 不采样,量级等于故障期间的进场数。
- **L9**:同一条连接上换角色的流程没有核实,客户端也没有这种调用。
- **复审补充一:玩家围栏排在覆盖之后。** `OnRouteDecision` 先覆盖 `lastSceneRoute` 并整体替换欠账,围栏要到尝试里才判;`RoutePlayerEventHandler` 在此之前还无条件覆盖了 `homeZoneId` / `ownerEpoch`。一条属于别的角色的迟到路由,会冲掉当前角色的欠账,只能等客户端 60s 超时。当前客户端不会触发(L9)。
- **复审补充二:按 node_id 重新解析,放大了 GO-3 的 R1 窗口。** 每次尝试都按 node_id 解析、按 node_id 唤醒,可能把发给 X 的路由交给复用了同号的新进程,把 GO-3 残余 R1 的窗口延长到 ≤20s(找不到节点时 ≤3s)。修法是记下首次解析成功的 uuid、之后按 uuid 解析,与 GO-3 一起报用户拍板(§13.9)。

### 13.4 P3 收尾:GO-6 / GO-4 残余 / 告警盲区(2026-09-28,未编译、未测试)

三项互不依赖,只改 `go/scene_manager`、告警 yaml 与文档,不碰 proto 和 C++。I0 / I3 依赖的机制一处都不碰;GO-4 只删 location,不碰 owner_epoch 键,与 GO-2 正交。

**GO-6:death_at 没写进,就不把节点摘出负载集**

问题见 §12.3 GO-6 行:`markNodeDeath` 写 `death_at` 失败后,仍无条件 ZREM 负载集。

改法:
- **两条不变量**:
  - G6-I1:节点被摘出负载集时,要么它的 `death_at` 已落地,要么本进程从第一次尝试摘它起(单调时钟)已满一个再入屏障。
  - G6-I2:"已判死、但还留在负载集"的推迟态期间,任何路径都不改写它名下的 scene ownership。
- **G6-I2 靠三层保证**:
  1. 节点在负载集里,`IsNodeAlive` 就为真,接管、改派、紧急迁移都会拒绝;
  2. `world_init` 的两处改派额外要求 `!IsNodeAlive(targetNode)`,不在负载集就不改派。原因是 CreateScene 慢路径会在任意副本上跑 `initWorldScenesForZone`,新领导者在 resync 之前也可能先处理 PUT,所以不能只靠领导者内存里的队列。这一层顺带补上了领导者缺位期间死掉的节点被提前改派的既有漏洞;
  3. 持有推迟队列的领导者上,`CanReclaimDeadNode` 对推迟态恒返回 false;`orphan_cleanup` 也补上了推迟态的判断。
- **推迟摘除的队列与重试**(`reentry_barrier.go`):
  - `markNodeDeath` 改为返回 error。写失败或 ZREM 失败时,进推迟队列 `deferNodeDetach`,由 `retryDeferredNodeDetaches` 在 5s ticker 和每次 fullSync 上驱动。
  - 写 death_at 那一段的截止是一个屏障(默认 20s),到期后不带标记摘除,记 expired。
  - ZREM 那一段的放弃期限是 `deferredNodeDetachGiveUpAfter` = `constants.NodeDeathMarkTTL`(10 分钟),到期记 abandoned。
  - 不加退避和抖动:单写者对每个死节点只有一两条命令,节拍来自现成的固定周期 ticker。
  - 节点在推迟期间重新注册:PUT 时调 `cancelDeferredNodeDetachOnReregister`,把判死时刻的快照交给收尾队列。
  - 失去领导权:丢掉全部任务,不碰 Redis。
- **fullSync 的清扫**:抽成 `sweepStaleLoadSetMembers`,与 watch DELETE 走同一条摘除路径(`removeNodeFromRedis` → `detachDeadNode`)。清扫从不判截止,也从不不带标记摘除。
- **为什么不照 §12.3 原文让 fullSync 返回 error**:
  - (a) fullSync 出错时外层循环不进 watch,ticker、rebalance、PUT/DELETE 处理会一起停住;
  - (c) 两条路径做的是同一个动作,应共用一个队列和一组单测。
  - 原先的理由 (b)(持续拒 SET)已更正为前瞻理由:K8s 目前没设 maxmemory,本地是 allkeys-lfu;将来定上限时必须用 noeviction,那时才会出现拒 SET 而放行 ZREM。
- **可用性代价**:推迟期间死节点仍在负载集(默认 ≤20s,ZREM 失败时 ≤10 分钟):CreateScene 可能选中它;进它名下场景的 EnterScene 会落成 CPP-2 那种哑连接;新频道可能铺到它上面。这与"进程已死、租约未到期"那段既有窗口同性质,按 AGENTS §11 接受。
- **指标与日志**:
  - `scene_manager_node_detach_deferred_total{zone_id,outcome}`,outcome 取 `deferred / recovered / expired / abandoned / dropped`。deferred 时对 expired / abandoned 预建 0 序列。
  - 日志前缀 `[ReentryBarrier][DeferDetach]`。

**GO-4 残余:LeaveScene 改成比对删除**
- 新 Lua `luaDeletePlayerLocationIfUnchanged`:location 仍是调用方 GET 到的原文时才删,并在同一段 Lua 里减场景 / 节点人数,钳 0。
- 返回值:1 = deleted,0 = already_gone,-1 = superseded(被改写)。`LeaveScene` 不再单独调用 `DecrInstancePlayerCount`。
- 删和减绑在一起的理由:go-redis 读超时后会重发同一条 EVAL,重试耗尽后上游也会再调一次。只有"谁删谁减"才能保证人数恰好减一次。
- 错误语义变化:Redis 读或删失败,从回成功改为回 error。唯一调用方 player_locator 出错时保留 claim 重试。location 写坏时不删、回成功(不形成毒任务)。
- **挡得住**:GET 与删除之间有并发 EnterScene 写入新 location;go-redis 重发;上游重试。
- **挡不住**:samePlacement 重连不写 location、值没变,照样会被删;同一场景的新落点在 GET 之前就已写入。
- 根治在 player_locator,另交两条后续:MarkOffline 的同步 LeaveScene 纳入 cleanup-pending 屏障;`session.SceneId==0` 时读到写坏的 location 会形成毒 claim。

**告警盲区**
- **补 0**:`metrics.SetNodesByRole(counts, expectedZones)` 对"有节点的 zone"与本实例配置的 `ZoneId`,四种声明角色(`declaredSceneNodeRoles`)都先写 0 再覆盖实际值。
  - 不做 sticky zone:合服下线的 zone 会永远误报,重启后又回到盲区。
  - zone 0 跳过。
- **两条 critical PoolEmpty**:保留现行 `count … unless on (zone_id) count …`,再追加 `or on (zone_id) (sum by (zone_id) (scene_manager_nodes_by_role) == 0)`,覆盖整 zone 全灭。
  - 旧二进制下右支没有序列,退化为现行行为,两条 critical 不会因此静默失效。
- **新增 warning `SceneManagerNodeDetachWithoutDeathMark`**:`sum by (zone_id) (increase(scene_manager_node_detach_deferred_total{outcome=~"expired|abandoned"}[10m])) > 0`,不设 for。
  - expired:安全,但说明 Redis 在拒 SET。
  - abandoned:死节点留在负载集,可能一直有请求被派到它上面。
  - 健康时恒为 0,不需要基线。
  - 规则总数 18 → **19**。
- **`handoff_pending_no_marker` 比值告警本轮不落**,口径与校准步骤见 §13.9。

**落在哪**:`go/scene_manager/internal/metrics/metrics.go`;`internal/logic/` 下的 `reentry_barrier.go`、`load_reporter.go`、`world_init.go`、`orphan_cleanup.go`、`leavescenelogic.go`;新测试 `node_detach_deferred_test.go`、`leavescene_cas_test.go`、`nodes_by_role_metrics_test.go`;`world_init_unreachable_test.go` 只在准备步骤补一行 ZRem;`deploy/k8s/scene-manager-alerts.yaml`。

**验证清单**(全部未执行,交 Codex;工作目录 `go/scene_manager`,Go 1.26.5,`GOTOOLCHAIN=local`)
1. **静态检查**:`gofmt -l internal/logic internal/metrics` 应无输出 → `go build ./...` → `go vet ./internal/logic/... ./internal/metrics/...`。
2. **新用例与改过的用例**:
   - 命令:`go test ./internal/logic/ -count=1 -v -run "TestRemoveNodeFromRedis_DeathMarkFailureKeepsNodeInLoadSet|TestRetryDeferredNodeDetaches_|TestHandleWatchPut_CancelsDeferredNodeDetachAndHandsOverSnapshot|TestSweepStaleLoadSetMembers_DefersInsteadOfZremOnDeathMarkFailure|TestPlayerLocationOwnerDead_DeathMarkFailureWithoutLocalObservationDoesNotAllowTakeover|TestReassignSceneNode_BlockedWhileDeathMarkDeferred|TestInitWorldScenes_|TestOrphanCleanup_|TestDeletePlayerLocationIfUnchanged_|TestLeaveScene_|TestRefreshLoadScores_ZeroFillsConfiguredZoneAndEmptyRoles"`。
   - 通过标准:全部 PASS,包括改过准备步骤的 `TestInitWorldScenes_NodeGoneFromRegistryIsReassignedToLiveNode`,以及原有的 `TestLeaveScene_DecrementsPlayerCount`。
3. **相邻回归**:`go test ./internal/logic/ -count=1 -run "TestEnterScene_StaleLocation|TestEnterScene_DeadOwner|TestEnterScene_OwnerMissingFromLoadSet|TestPlayerLocationOwnerDead|TestReconcileDeadNode_|TestIntegration_|TestIsNodeAlive|TestDecrPlayerCount_|TestDeletePlayerLocation|TestAutoscale_"`,再跑 `go test ./... -count=1`。有 CGO 时对第 2 步加 `-race`。
4. **先红后绿**:
   - 用 `git worktree add <scratch>/sm-red <本项改动之前的提交>` 建临时树,只放一个只引用旧树已有符号的 `internal/logic/red_p3_test.go`。
   - 其中六条用例的期望失败点:写失败后仍 ZREM;旧 `playerLocationOwnerDead` 返回 true;旧 init 会改派;LeaveScene 删掉了被改写的 location;Redis 失败回成功;zone 7 序列缺失。
   - 回到改动后的代码,对应用例全部通过;最后 `git worktree remove`。
5. **告警规则**:从 `deploy/k8s/scene-manager-alerts.yaml` 抽出 `spec.groups` 跑 `promtool check rules`,应为 **19** 条、无语法错误。本机没有 promtool 就如实记录"未校验"。
6. **可选:death_at 写失败注入**(本地 dev,`AllowUnsafeCrossNodeHandoff=false`)。
   - 操作:`redis-cli CONFIG SET maxmemory <略高于 used_memory>` 并设 `maxmemory-policy noeviction`,杀一个 scene 节点并等租约到期。
   - 期望:出现 `[ReentryBarrier][DeferDetach] 已判死但 death_at 未落地或摘负载集失败,节点暂留负载集…`;约 20s 后出现不带标记摘除(expired),或解除限制后出现 `补写 death_at 成功`(recovered);对应 outcome 计数 +1;期间对该节点名下玩家的接管一直被 18 拒。
   - 结束后恢复原 maxmemory 与 policy。
   - runbook v2.5 的场景 E2 是可执行版。

**残余**
- 本地 allkeys-lfu 下,death_at 写成后仍可能被淘汰,效果等同 GO-6,本方案不覆盖。K8s 是 noeviction、没设 maxmemory,不受影响。
- 领导权切换后、新领导者 sweep 之前,rebalance 的机会迁移不经第 1、2 层。它只搬人数为 0 的空频道。
- 极端残余:推迟期间节点重新注册又再死,而本进程的 PUT 与 DELETE 都漏掉了,会沿用上一次的首次观察时刻。
- abandoned 之后没有自动重扫:fullSync 不是周期任务,死节点会留在负载集,名下玩家一直回 18,直到 watch 中断、领导权变化或重启 SM 领导者。旧代码 ZREM 失败时同样残留,不算回归,新告警覆盖了它。复审建议放弃时调一次 `RequestLoadReporterResync()`,未落。
- expired 分支的重新注册检查,挡不住"PUT 已到达 watchCh、还没被处理"的竞态,可能把新化身不带 death_at 摘出负载集。前提是 Redis 拒 SET 已满一个屏障,概率极低。
- 刚失去领导权的副本在 cancel 时,会把快照交给本地收尾队列,而跟随者上的 drain 不经领导者闸门,会删掉新化身的计数。这是既有问题的一个新入口。
- 同一场景跨节点重写(频道迁移后 NodeId 变了)时,EnterScene 只在 SceneId 变了才减旧场景人数,这个场景的人数会多 1。这是 EnterScene 既有的问题,LeaveScene 的注释"改写它的 EnterScene 已减过旧场景人数"对这种情况不准确,待另开一项。
- `ResolveSceneReentryBarrier` 对配置值没有上限。屏障配到 ≥10 分钟时,death_at 的 TTL 会短于屏障,推迟那一段的截止也会越过 10 分钟。既有问题,建议另开一项做配置校验。
- 告警还有这些盲区:
  - 该 zone 自己的 SM 也全挂,或 ZoneId 配错;
  - 某个 SM 卡在 fullSync 的重试循环时,它最后发布的 >0 旧值会压住 PoolEmpty;
  - 新 zone 开服、整 zone 维护时,两条 critical 会在 2 分钟后响,要按 `docs/ops/scene-node-role-split.md` 的步骤加 silence;
  - 抓取间隔 >20s 时,第一次 expired 仍可能漏报。

### 13.5 Hiredis ERR 泄漏 / scene-manager zrpc Timeout / robot 空文件(未编译、未跑测试)

**Hiredis ERR 泄漏**
- **问题**:`Hiredis::command` 在 `redisvAsyncCommand` 返回 ERR 时,不释放刚 new 出来的回调副本。
- **改法**:返回值不是 REDIS_OK 时当场 `delete p`。改动在两份文件里:`cpp/libs/engine/muduo_windows/src/contrib/hiredis/Hiredis.cc` 和 `tools/archived/muduo_linux_overlay/contrib/hiredis/Hiredis.cc`,两份必须逐字节一致,注释已改成对称写法。
- **核对结论**:
  - hiredis 1.2 对非订阅命令的所有 ERR 路径都不登记 privdata,所以 ERR 分支的 delete 不会造成二次释放。这些路径是:`async.c` 的 DISCONNECTING / FREEING、未订阅时的 UNSUBSCRIBE、OOM、格式串非法。
  - 更正 §12.3 原文"断开期间"的说法:出错断连时 hiredis 先 `_EL_CLEANUP`,适配器清空 channel_,`connected()` 已为假,`command()` 在 new 之前就返回,不会漏。
  - 真正会漏的是四种情形:`~Hiredis` → `redisAsyncFree` 回调期间(FREEING)、显式 `disconnect()` 之后、格式串非法、OOM。
- **不变量**:对调用方来说,"ERR 时回调不会被调用"与改动前相同。I0 机制 2(DEL 与 MGET 走同一条不重放的连接)不受影响。
- **回归用例**:`cpp/tests/rpc_controller_test/hiredis_command_lifecycle_test.cpp`,共 4 个 `HiredisCommandLifecycle.*`:
  - `FormatErrorReleasesCallbackCopy`
  - `CommandFromNullReplyCallbackDuringFreeReleasesCallbackCopy`
  - `CommandWhileDisconnectingReleasesCallbackCopy`
  - `AcceptedCommandCallbackRunsExactlyOnceWithNullReplyOnFree`(成功路径对照组)

  vcxproj 已加一行。

**scene-manager zrpc Timeout**
- **取值**:本地与 K8s 统一为 `Timeout: 8000`,即 `HomeZoneLookupTimeoutMs` 1500 + `KafkaWriteTimeoutSeconds` 5×1000 + Redis 往返余量 1500。
- **单一真相**:服务 yaml `go/scene_manager/etc/scene_manager_service.yaml` 是唯一真相。`tools/scripts/k8s_deploy.ps1` 用 `Get-AuthoritativeScalar` 从它镜像 `Timeout`、`KafkaWriteTimeoutSeconds`、`HomeZoneLookupTimeoutMs` 三项;最后一项原先是生成器写死的 1500。
- **守卫只有一处**:`tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的 `Get-SceneManagerTimeoutBudgetViolations`,余量常量 `$SceneManagerEnterSceneMarginMs = 1500` 全仓只此一处。它对 ConfigMap 和服务 yaml 各判一次:
  - 三项必须是正整数;
  - 满足上面的不等式;
  - **不得使用 MethodTimeouts**。生成器只镜像标量,用它放宽 EnterScene 在 K8s 上会静默退回全局值,所以 §12.2 原文"或用 MethodTimeouts 只放宽 EnterScene"作废。
  - 另有一个自检用例,用合成 yaml 覆盖 5000、2000、7999、0、缺 Timeout 键、Kafka=0、带 MethodTimeouts 七种应报违例的情形,以及 8000 应通过。
- **8000 的覆盖范围**:只覆盖不触发同步建场景的正常路径。放宽之后,挂在请求 ctx 上的同步调用(CreateScene、选 gate、懒改派)在下游挂住时能跑到 8s,在途协程和连接数相应增加。kafka-go 的元数据查询、go-redis 的重试、同步建场景都不在预算内,超出时仍由 C++ 的应答看门狗兜底。
- **与 C++ deadline 的关系**:C++ 侧的 deadline 为 10000(§13.0 约定 1),大于 8000。

**robot/connected()**
- 这是一个被跟踪的 0 字节文件,没有任何引用,来自 `8dc0a4605` "clear code"。删除已随 `9da27f9a4` 提交。

**落在哪**:两份 `Hiredis.cc`、新 TU 与 `rpc_controller_test.vcxproj` 的一行、服务 yaml(Timeout 与注释)、`k8s_deploy.ps1` 的三项镜像、契约测试(相等用例、预算用例、自检用例)、删除 `robot/connected()`。分布在提交 `9da27f9a4`、`24dd3e6c5`、`6941e7344` 中。

**验证清单**(全部未执行,交 Codex。MSBuild 串行 `/m:1 /nr:false`,与其它会话的构建错开)
0. **前置核对**:
   - `cmp` 两份 `Hiredis.cc` 应无输出;`delete p;` 在每份里各 1 处;服务 yaml 里 `^Timeout:` 只有 1 处。
   - `git diff c2c5ec505 -- tools/scripts/k8s_deploy.ps1 | Select-String '^-.*Pass:'` 应无输出,即 Redis 段的 `Pass: "${redisPassword}"` 没被冲掉。
   - **红态基点必须是 `c2c5ec505`(= `9da27f9a4^`),不能用 HEAD。** 两次自动保存已把修复卷进了 HEAD,在 HEAD 上跑是假绿。
1. **C++ 先红后绿**:
   - 记下 `lib/muduo.lib` 的时间戳:2026-09-29 仍是 2026-09-25 09:45,也就是修复前构建的。
   - **不重建 muduo**,只构建 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`(它没有 ProjectReference,直接链接旧 lib)。跑 `pwsh tools/scripts/run_cpp_tests.ps1 -Filter rpc_controller`,再跑 `build/cpp/tests/rpc_controller_test.exe --gtest_filter=HiredisCommandLifecycle.*`。期望前三个 FAIL(use_count 为 2 / 未 expired),对照组 PASS。
   - muduo.lib 若已晚于 2026-09-28 03:41,如实记录"红态不可得"。
   - 然后构建 `cpp/libs/engine/muduo_windows/muduo.vcxproj`,重建测试工程,4 个全部 PASS;再跑全量 exe,`RpcClientLoopback.*` 等既有用例的结果不变。
   - 之后依次构建 scene → gate → battle 节点,都要 0 error。
2. **PowerShell 契约测试**:
   - 在共享工作树上跑 `pwsh -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:预算用例、自检用例、相等用例都 PASS,旧的"+1000ms 余量"用例名不再出现。
   - 红态在临时工作树里取:`git worktree add --detach $env:TEMP\xm-k8s-red c2c5ec505`,先跑该提交自带的测试文件记下改前基线,再用新测试文件覆盖后重跑。
   - 期望:相等用例 FAIL,报"查不到 'data.scene_manager_service.yaml.Timeout'";预算用例 FAIL,报 `K8s ConfigMap go-svc-scene-manager-config:Timeout='' 必须是正整数;KafkaWriteTimeoutSeconds='' 必须是正整数`;自检用例 PASS。
   - 最后 `git worktree remove --force` 这个临时树。
3. **目检 DryRun**(不 apply):`pwsh tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName t -ZoneId 101 -DryRun -GoSvcRegistry registry.invalid/test -JavaSvcRegistry registry.invalid/test`。在 go-svc-scene-manager-config 段里,`Timeout: 8000`、`KafkaWriteTimeoutSeconds: 5`、`HomeZoneLookupTimeoutMs: 1500` 各出现一次,`Pass:` 行仍在。
4. **robot**:`git show --name-status --format= 9da27f9a4 -- 'robot/connected()'` 应为 `D`;`git ls-files robot | Select-String -SimpleMatch '('` 应无输出。
5. **Linux**(可选):`bash tools/archived/setup_dependencies.sh` 应打印 overlay stamp changed。没有环境就记录"Linux 未验证"。

**残余**
- 修法的前提是对照 hiredis 1.2 核过的 ERR 路径。将来升级 hiredis 必须重新核对 `__redisAsyncCommand`。
- 多频道 (P)SUBSCRIBE 中途 OOM 时可能部分登记 privdata。本封装本来就不支持订阅。
- login 的 `SceneManagerRpc` 客户端 Timeout 是 5000(`go/login/etc/login.yaml`),小于服务端的 8000。这是既有的不对称,归 login 的归属会话决定。
- 没有做"Timeout 远小于 C++ 看门狗"的机械上界检查:PS 要去读正在改写的 C++ 常量,违反单一真相,本轮只在 yaml 注释里写了跨配置契约。
- 新 C++ 用例依赖本机回环 TCP,安全软件拦截或端口竞争可能造成假红。失败时先看 SetUp 有没有连上。

### 13.6 客户端 CL-6 / CL-8(客户端未编译、Unity 未跑)

**落点**
- 客户端分支 `crosszone/client-cl6-cl8`,提交 `54034fe`,基于客户端 `origin/main` 的 `8b21583`。落在工作树 `E:/work/mmorpg-client-wt-crosszone`,没有改动主工作树。
- 2026-09-29 磁盘上,该分支之上还有组队线的 `835aa1d`,以及合并提交 `96d36da`(合入了客户端 origin/main 的建角 / 角色名改动)。工作树的远端跟踪引用 `origin/main` 也已指向 `96d36da`。推送状态以客户端远端为准。

**CL-6:补齐传送链上返回码的文案**
- `tools/gen_proto.ps1` 收进了 `generated/code/proto/tip/cross_server_error_tip.proto`。
- 生成物 `Assets/Scripts/Proto/Generated/CrossServerErrorTip.cs`(以及 `.meta`,guid `56e553e72e8d43fdaf6c210df085018b`)只用单个 protoc 生成,**没有跑 gen_proto 全量**:全量会漂动其它会话负责的协议。
- `DescribeTravelTip` 补了 `cross_server_error.KSceneTransferInProgress` 与 `common_error.KFeatureUnavailable` 两条文案。两者都只以同步响应体出现,不进 `IsTravelFailureTip`,也不进 `DescribeKickReason`,否则在途期间一条同码推送会把一次还没结束的传送判死。
- 传送链会回到客户端的码,全部都有文案:
  - 同步拒绝:3024–3027,3007,3014,13000,1006;
  - 异步 msg 23:跨区没成是 3027,同区换图没成和第二条腿进场没成是 3023;
  - 踢线 34:reason 3027(§12.5.6 与冻结上限的跨区出口),3023(CPP-2 的放弃、冻结上限的同区出口)。
- 13000 实际能到达的场景:TravelToZone 的 RPC 在 15s 超时后客户端已经收场,服务端的交接(跨区传送或同 zone 跨节点换图的冻结)仍在进行,这时玩家再点一次。1006 在这条 RPC 上只来自"实体退出中"的闸。

**CL-8:预算推导与管线提前收口**
- **预算**:
  - `GameClient.RedirectFlowWorstCaseSec` = 探测 5 + 验票 10 + 新连接上的 Login 15 + EnterGame 15 + 等入场 60 = 105s。新常量为 `TokenVerifyTimeoutSec`、`RpcTimeoutSec`、`SceneEntryTimeoutSec`。
  - `CityTravelRequest.CrossZoneTimeoutSeconds` = `AcceptedHandoffBudgetSeconds`(75)+ 105 + `CrossZoneSlackSeconds`(15)= 195s,旧值是 120。
  - 75 这一段依赖服务端冻结硬上限:70 + 1s 扫描 = 71 < 75(§13.2)。服务端的 `kClientAcceptedHandoffBudget` 用 `static_assert` 镜像 75,两边要同步改。
  - 195 只是换连接阶段的兜底。常见失败(失败 tip、踢线、断线、底层 75s 到期)都会更早收场。
- **等待点收口**:
  - 验票等待和等入场两处,共用纯函数 `ClassifyPipelineWait(succeeded, rejectedByServer, connectionAlive, now, deadline)`,优先级是成功 > 服务端明确拒绝 > 连接已失 > 到期。
  - 拒绝优先于断线,是因为 gate 拒票后紧跟着 shutdown,拒绝应答和断线哨兵常在同一次 Poll 里派发,原因必须保住。
  - 连接是否存活,只看主线程的 `IsStillCurrentGate(gate)`,即 `ReferenceEquals(_gate, gate)`,**不读 `GateTcpClient.Connected`**:读线程在 EOF 时先翻转运行标志,排在前面的帧还没派发。
  - 新字段 `_tokenRejectedByGate` 只在 AdoptGate 时复位。`DisconnectInternal` 不再清 `_enterFailedTipId`。
- **改前改后对照**:
  - 重定向第二条腿验票被拒:改前等约 10s,文案是"token verify timeout";改后同一帧收口,状态栏为"切换服务器失败,请重新登录"。
  - 等入场时断线或被踢:改前等 60s;改后 ≤1s 内收口。
  - 3023 与断线落在同一次 Poll:改后文案带"进入场景失败"。

**落在哪**(客户端仓):`tools/gen_proto.ps1`、`CrossServerErrorTip.cs`(+`.meta`)、`Assets/Scripts/Game/GameClient.cs`、`Assets/Scripts/Game/WorldTravel/CityTravelRequest.cs`、`ZoneTravelClient.cs`(只改注释)、`Assets/Scripts/App/DevAutoPilot.cs`(只改注释)、`Assets/Tests/EditMode/Tianyong/CityTravelRequestTests.cs`(新增 6 个、改 2 个,共 26 个)、`Assets/Tests/EditMode/Net/GateTcpClientLifecycleTests.cs`(新增 1 个,共 9 个)。

**验证清单**(全部未执行,交 Codex)
1. **生成物一致性**,在服务端仓库根执行:
   - `& third_party\grpc\install_vs2026\bin\protoc.exe --proto_path=. --csharp_out=$env:TEMP\cl68-gen generated/code/proto/tip/cross_server_error_tip.proto`
   - 再用 `git diff --no-index --ignore-cr-at-eol --stat` 与客户端的 `CrossServerErrorTip.cs` 比对,应无差异。
2. **离线编译**:用主工作树的 csproj 做临时副本,跑 `tools/client_compile_check.ps1 -ProjectPath <客户端工作树> -Csproj <副本>`,期望 exit=0、errors=0。
3. **Unity EditMode**(权威):
   - 先把主工作树的 `Library` robocopy 过来,再以 `-batchmode -nographics -runTests -testPlatform EditMode -testFilter "MmorpgClient.Tests.EditMode.Tianyong.CityTravelRequestTests;MmorpgClient.Tests.EditMode.Net.GateTcpClientLifecycleTests"` 运行,不要带 `-quit`。
   - 期望 26 + 9 个全绿,unity.log 里没有 "error CS"。
4. **联调**(可选,本地双 zone):
   - 让 gate(B) 拒票(改它的 `GateTokenSecret`):期望同一秒内先后出现 `[gate] token rejected: …` 与 `… failed after swap: 登录凭证校验未通过,请重新登录`。
   - 第二条腿等入场时结束 gate(B):≤1s 内出现 `[enter] connection lost while waiting for scene entry`。
   - 回归:§12.5.5 用例 a 与 §12.5.6 的踢线文案不变。

**残余**
- 首登验票被拒时,显示哪句文案取决于到达时序("与服务器的连接已断开"或"登录凭证校验未通过"),两句都属实。
- 半开 TCP 只能等各段期限。
- 探测通过之后目标 gate 才变成黑洞时,OpenGate 的同步建连会阻塞约 21s。
- 选角等待没有上界。
- 第二条腿上又来一条 124 会被忽略。按 §12.7 这种情况不可达。
- `Call`(Login / EnterGame)仍在 Tick 之后读 `gate.Connected`,可能丢掉 EOF 之前已排队的帧。不会把失败判成成功,待另改。
- `IsStillCurrentGate` 注释里列出的 `_gate` 变化途径,漏了"发送失败触发的断线"这一条。
- 规格残余里写的"第二条腿上带原因的踢线不可达"已不成立:CPP-2(§13.3)放弃时会在新连接上先推 3023、再发 34。客户端的实际表现是 3023 先被锁存,踢线处理器发出带原因的断线,最终状态栏为"切换服务器失败,请重新登录(进入场景失败,请稍后再试。)"。行为正确。
- `CityTravelRequest.cs` 里"冻结上限另案落码 / 上限落地之前没有上界"的措辞已过时(服务端已落码),`CityTravelUiRoot.cs` 里"最坏一分钟左右"也已过时,待客户端下次统一更新。

### 13.7 GO-2 根治:owner_epoch 严格单调(设计已定稿,落码中)

GO-2 在制代码和 proto 注释里引用的 `cross-zone-scene-travel.md §12.8`,指的就是本小节。落码完成后,本小节要按代码实情补上落点与偏差。

**问题**(§12.3 GO-2 行与 §10.3,核实后确认三条伤害):
1. 路由失败回滚把 owner_epoch 从 N+1 退回 N,而被收回的 N+1 已被目标节点拿去落了 DBTask。db 的 applied_epoch 被毒化,合法持有者之后的每笔 DBTask(N) 都被判 stale。
2. 同一个值会被铸造两次,§12.6.4 M5 的"两节点同持 E+1"就是这样来的。
3. 凭标记的铸造"报错但实际已投递"时,B 已经过了 A2′、建出实体并落过盘,回滚却把归属交回冻结中的 A。A 解冻后用旧内存覆盖了 B 的进度,结果是回档,外加帮会资产复制。

**定稿设计**
1. **Go 侧:单调回滚**
   - 铸造过的落点回滚时,不再 SET 回旧值,而是在同一段 Lua 里再 INCR 一格(N+1 → N+2,称为 bump)。
   - 以下两种情况改用 keep 模式,epoch 保留新值:没铸造过的落点;以及 §12.6.9(c) 的同节点 epoch 0 铸造。
   - 回滚的前提是:本次铸造所凭的 handoff 标记必须原样还在。这一条与目标节点的 A2′ 互斥。两段 Lua 在同一个 Redis 上先后执行:
     - A2′ 先执行:回滚返回 3(marker_gone),一个字节都不动。
     - 回滚先执行:B 的 A2′ 返回 -1,拒绝建实体,也就发不出 DBTask(N+1)。
2. **回执与转写**
   - bump 回滚时,在同一段 Lua 里把以下三样原子写回:旧 location、新 epoch、**回滚回执** `PlayerLocation.rollback_receipt = 7`。回执的值是所凭标记的原文。
   - 同时把所凭标记转写成新 epoch,saved_at_ms 后缀按原文字节保留,例如 "E:t" 转写为 "E+2:t"。
   - 重放识别分支(返回 2)什么都不写。
   - 回滚 Lua 的返回值:-after = bump 已回滚;1 = keep 已回滚;2 = 已回滚过(重放);3 = marker_gone;0 = 被并发推进。rollback_total 的 outcome 集合新增 `marker_gone` 与 `plan_error`。
3. **应答**
   - `EnterSceneResponse.owner_epoch_after_rollback = 5`,只出现在 `ErrKafkaRoute` 应答里,**只用于日志,绝不是采纳凭证**。
   - 原因:回滚之后,第三方可以凭转写标记铸出更新的值,号码匹配的应答却照样带着 E+2,凭它解冻就会双持有。
4. **C++ 源端的原子取证**
   - `ResolveTravelOutcome` 现在的"DEL + MGET 两条命令",换成一段带 `#!lua` 的原子脚本:只删本次交接这一族标记(按 saved_at_ms 等于本次 `requestedAtMs` 识别,原标记与转写标记都覆盖),再在同一脚本里 MGET epoch 与 location。
   - `#!lua` 的作用(Redis ≥7,全环境都是 7.2):在只读副本、MISCONF、OOM 下,整段脚本被拒绝。这样就不会出现"DEL 失败而读成功、带着活标记解冻"。
   - 用 MGET 而不用 GET:遇到类型错误的键时 MGET 返回 nil,不会抛 WRONGTYPE 而让取证永久失败。
   - **关于 I0 机制 2 的注释**:今天"拿到有效 MGET 应答 ⇒ DEL 已执行"依赖两条命令走同一条不重放的 hiredis 连接;任何一侧改成可重放的接口,都会无声地打破它。改成单个原子脚本后,这一点由脚本本身保证。落码时在脚本旁写明,并注明旧的两条命令写法依赖的前提。
   - infra 转交的"DEL 忽略 `command()` 返回值"一项,随之由原子脚本取代。
5. **判定表**
   - 第一刀用 `IsHandoffMarkSent`。
   - A1(SET 未发出):解冻。
   - B 段(SET 已发出):默认不存盘销毁,只有两种正向证据能解冻:
     - B4:脚本删掉本族标记后,读到 epoch 仍等于缓存值。这时清掉 markEpoch,然后 Abort。
     - B5:回执等于本次标记原文,且 markEpoch 等于缓存值,且 redisEpoch = markEpoch + 2 = location.owner_epoch,且 location 指回本节点本 zone。这时先采纳新 epoch,打 WARN `[ZoneTravel][RollbackAdopt]`,再 Abort 解冻,然后强制存盘一次。
   - 其余各行:
     - B6(回执匹配、但交叉校验不成立):调 `ConcludeHandoffAfterMarkSent`,新增 site `travel_receipt_anomaly`,会踢线。
     - B7(epoch 变了、location 指回本节点、回执不是本次的):销毁,不踢线,计 `returned_after_grant`。
     - B8 / B9:走现有的"已放行"分支。
     - B1(SET OK 但 EnterScene 没发出)与 B3(上限到期):交给 §13.2 的收口,**不读回执**。
     - B2(读失败):保持冻结,重挂看门狗,终局交给上限。
   - 新计数 `rolled_back_adopted`、`returned_after_grant`、`rollback_receipt_anomaly`。
6. **疏散标记改为条件写**:`DispatchEmergencyRelocate` 改用 owner_epoch 条件写(`kLuaWriteIfOwnerEpoch`),不再用过期的 E 覆盖转写标记。
7. **与骨架的三处偏离**(都保持 fail-closed):
   - 偏离 1:B4"epoch 未变即解冻"是回执之外的第二种正向证据。回滚不再让 epoch 回到原值,所以同一原子脚本里读到的"未变",同时证明此前从未放行、此后再也放不出去。
   - 偏离 2:B2 读失败时保持冻结,不立即销毁。
   - 偏离 3:B7–B9 不默认踢线,因为同 zone 放行会把同一个会话改绑到目标节点。
8. **I3 论证**:对不在退出中的实体,能摘掉冻结、又保留实体的出口只有 A1、B4、B5 三类。三类都在同一原子脚本删掉本族标记之后才判定,之后铸造 Lua 的标记复核会挡住一切凭本族标记的铸造。其余摘冻结的路径都会同时销毁实体。
9. **兼容与升级顺序**:
   - 先升级全部 scene,确认 gate 转发 owner_epoch、`owner_epoch_unknown` 恒为 0,再升级 scene_manager。
   - scene-manager 是 `replicas:2` + RollingUpdate,新旧副本混跑时三条伤害都还在。建议这次临时缩到 1 副本或改用 Recreate;在全部副本换新之前,帮会资产通道保持关闭(帮会 08 §8.3 第 3 条的门禁)。
   - 二进制回退顺序相反。
10. **go/db 不改**。字段号:回执 7,应答回显 5;GO-3 的新字段从 8 起。

**验证清单**(落码完成后执行,当前不可执行。用例名以落码为准)
1. **窄面 regen**:只重生成 `storage.proto` 与 `scene_manager_service.proto`。Go 用 `--proto_path=generated/proto/_unified`,**C++ 用仓库根作 proto_path**。`*_grpc.pb.*`、`cpp/generated/grpc_client/scene_manager/*`、message_id 不应有变化。
2. **Go**:`go/scene_manager` 下 gofmt / build / vet;跑规格列出的 `TestPlanRouteRollback|TestClassifyRollbackReply|TestEnterScene_RouteFailure|…|TestSourceJudgeScript` 等用例和全量;login / match / player_locator / shared / guild / trade 各跑 `go build ./...`;`go/db` 跑 `go test ./internal/kafka/...`。
3. **C++**:proto → rpc → battle 库 → scene 库 → scene / gate / battle 节点;`run_cpp_tests.ps1 -Build -Filter cross_zone`,新的纯函数用例与既有用例都要绿。
4. **故障注入**(双 zone,`AllowUnsafeCrossNodeHandoff=false`):
   - B4a(同 zone)/ B4b(跨 zone):交接写好标记后 `docker pause kafka`,等 5s 写超时触发回滚,保持 pause 直到源端取证完成。期望:
     - scene_manager 打出 `[RouteRollback] outcome=rolled_back mode=bump … epoch=E+1->E+2`;
     - 源端打出 `[ZoneTravel][RollbackAdopt] … E -> E+2`;
     - Redis 中 owner_epoch 为 E+2、handoff 为 nil,location 里 `rollback_receipt="E:…"`;
     - 不出现 `stale_owner_write_rejected`;
     - 玩家留在原场景且能移动,数据不回档。
   - B5(提前 unpause,让已超时的消息补投到 gate):命中三种可接受结局之一,两侧都不得出现同 epoch 的双存盘。
   - 只读副本 / ACL 拒绝 EVAL:玩家一直冻结到上限,然后被销毁并踢线,全程不解冻。
5. 回归:travel-smoke 仍输出 `TRAVEL_SMOKE_OK`;`robot login-test` 23/23。

**落码后要同步改的旧节**:§12.3 GO-2 行;§12.5.6 的回滚三态(改为 -after / 1 / 2 / 3 / 0)与残余 7;§12.6.4 M5;§12.6.8;§12.6.9(c)。另外 `scene-owner-reentry-barrier.md` §3.3 要补上 CZ-3 的唯一例外(B5 采纳)及其三条前提。

**残余**(设计层面)
- 不凭标记的铸造(首次落点、第二条腿、死节点接管)遇到"报错但已投递"时,没有互斥手段:幽灵 DBTask 可能先落库。这一支做不到 fail-closed,由"账本只读 Redis"契约兜底。根治需要载入认领令牌,或让 C++ 在 CAS 成功后再发 DBTask。
- 新旧 SM 混跑窗口里,三条伤害都还在。
- gate 迟到改绑(§13.3 L3):采纳后客户端"能看不能动",要重登。
- 采纳之后才到的迟到 ReleasePlayer 会让实体退出、会话变哑。根治要让 ReleasePlayer 携带预期 epoch(proto 变更)。
- 比现状差的三处:
  - "疏散撤回在回滚之前 + 路由失败"这一三重巧合下,改派会回 18;
  - B 先消费令牌、又放弃载入时,A 会被销毁,而不是解冻;
  - DEL 型回滚的重放识别,可能因 LeaveScene 误报,导致人数多还一次。
- §13.2 残余里"SET 回调 ERROR 分支不校验代际"一条,由本判定表的属主决定是否收进代际判断。

### 13.8 CPP-3 疏散 / 排空改派的待确认表(设计已定稿,落码中)

**问题**:§12.3 CPP-3 行。改派 EnterScene 是 fire-and-forget:票据发送前就删,发完立刻摘会话、销毁实体。被拒或没有应答时,gate 会话还连着,却指向一个已经没有实体的节点,没有 tip,也不踢线。

**定稿设计**:把"发完即忘"改成"登记、确认、收口"三步。
1. **登记**:票据被消费的那一刻,按 player_id 登记进待确认表(新纯头文件,规格名 `relocate_confirm.h`)。
2. **收口信号**:
   - SM 应答只触发核实,不直接定案;
   - 进场路由回到本节点时,转入"落地等载入";
   - 每个阶段都有单调时钟的截止时刻。
3. **核实**:在 zone Redis 的同一条连接上 MGET owner_epoch 与 location。只看 zone、node、scene,**不看 epoch**,所以在旧回滚语义和 GO-2 的 bump 语义下结论相同,本项不依赖 GO-2 的回执。
4. **踢线**:读到"仍指向源场景或本节点(或已没有 location)",并且 SM 已不可能再处理本次改派时,用票据里的 (sessionId, playerId) 直推 gate:先 tip `kEnterSceneFailed`,再踢线 34。
5. **凭证补写**:踢线之前,若本节点身份确认有效,按同一次 MGET 读到的 owner_epoch 条件补写一份释放标记,保证被踢的玩家重登能过换手门。疏散中不补写(身份闸)。
6. **预算**:写标记 30s、等应答 30s、settle 15s、落地 60s、单轮核实 10s。settle 15s 与 zrpc Timeout 之间有跨配置契约:Timeout 调到约 13s 以上时,必须同步调整。
7. **票据作废**:客户端已断开,或会话已被取代时,票据作废、不发改派,由 A1′ 接手。
8. **收敛谓词**:疏散 / 停机的 15s 收敛谓词都要等待确认表清空,到看门狗期限时只放弃、不踢。

**与 gRPC 失败回调的对齐**:规格写于 `b85f13c07` 之前,是按"非 OK 不回调、只能等 30s kNoReply"推导的。现在改派请求的传输失败会经 `DispatchEnterSceneTransportFailure` 到达;目前实体已销毁,只记日志。落码时要把它接成与 kNoReply 同类的"结果未知 → 触发核实"证据,不当作拒绝。

**与 GO-2 的对齐**:CPP-3 规格担心"GO-2 笼统迁移标记会给就地解冻的源实体开免存盘放行口"。GO-2 定稿采用"只在所凭标记原样还在时转写 + 源端先原子删本族标记再采纳",覆盖了这个担忧。CPP-3 仍按读到的 owner_epoch 条件补写,落码时核对两者不重复写。

**验证**:落码后按规格执行。`cross_zone_test.exe --gtest_filter=RelocateConfirm*` 与全量;runbook 新增场景 V(V1 基线:granted / landed_here;V2 被拒:`docker pause kafka` 后 kick_verified,且 handoff 为 "X:<ms>";V3 无应答;V4 停机 drain;V5 回归)。

**残余**(设计层面):
- SM 比 settle 窗口还慢时可能误踢。
- 放行到别处、但路由丢了时不踢。
- 疏散时 gate 多半已摘掉本节点,推送结果为 push_gate_gone。
- kNoReply / kReplySucceeded 在无法核实时放弃、不踢。

### 13.9 本轮不落

**GO-3(死节点的 node_id 被新进程复用后,§11.5 的接管永不触发):本轮不落,需用户拍板**

- **设计**:定稿规格是在 `PlayerLocation` 里记下属主进程的 `node_uuid` 与发现键的 create_revision,并加一道时序闸(记录写入时刻早于现任进程 launch_time 至少 6s)。
- **不落的原因:设计残余 R1 违反交接不变量。**
  - 滞留在 Kafka 或 gate 里的路由事件,可能在同号新进程注册之后才投递给它。
  - 这会让一个健康的已注册进程,在没发 handoff SET 的情况下被判"已替换"而丢掉归属:
    - 它若正冻结交接、SET 未发出,违反 I0;
    - 未冻结时 `IsCrossZoneFrozen` 为 false,本节点却已不是属主,违反 I3(Battle 结算依赖这一条)。
  - §11.5 的例外只覆盖"属主进程已从注册表消失"。
- **根治**要改 C++ 或协议,需用户拍板:
  - 让 RoutePlayerEvent 携带目标 scene 进程的 uuid,由 gate / scene 校验;
  - 或让持有节点在 A2′ 的 Lua 里自己盖章,scene_manager 只认 epoch 一致的那一份。
- **推迟没有数据风险**:GO-3 是活性问题,现状是回可重试的 18,租约到期后由 LeaveScene 自愈;"间隔 <30s 持续重试"的客户端才会一直卡住,§12.6 的 A′ 在 300s 标记 TTL 内能缓解。
- **其它牵连**:
  - GO-3 要改 `storage.proto`,新字段从 8 起(7 已给 GO-2)。
  - 它与 §13.4 在 `load_reporter.go`、`reentry_barrier.go`、`metrics.go` 的同一批函数上重叠,以后落码要 rebase 到 §13.4 之上,并补一个用例:推迟态下来一条不同 uuid 的 PUT,判定仍是 Replaced,且仍被屏障挡住。
  - §13.3 的"按 node_id 重新解析 / 唤醒"放大了 R1 的窗口,一起报用户。

**`handoff_pending_no_marker` 比值告警:推迟,等压测基线**
- 口径 A(用现有指标):`sum by (zone_id)(rate(scene_manager_enter_scene_rejected_total{reason="handoff_pending_no_marker"}[10m])) / (sum by (zone_id)(rate(scene_manager_enter_scene_stage_seconds_count{stage="update_loc"}[10m])) > 0)`。健康时持续 > 1 即异常,但分母随登录量浮动,抓不到繁忙 zone 里单个卡住的玩家。
- 口径 B:新增"凭标记放行且落点成功"计数作分母,需要改 `enterscenelogic.go`。
- 两种口径的阈值和 for 都没有实测数据。校准步骤:首轮双 zone 压测(`AllowUnsafeCrossNodeHandoff=false`)按 5 分钟窗口导出比值分布,稳态取 p99;再用 runbook 场景 C / R 做故障注入;定下阈值后以 `severity: info` 起步。

**其余推迟项**(只登记,不落):
- scene 侧会话栅栏:`PlayerEnterGameNode` 在 epoch 判定之前就无条件摘会话;EnterScene 遇到过期 epoch 只打 WARN。需要单独设计,与 CPP-3 的挂点、GO-2 协调。
- robot 的 travel-smoke 预算改为可配置(≥75s),用于自动验收 §13.2 的 H 场景。归 robot 属主。
- `SendTipAndKickToClient`(按实体)收敛到 CPP-3 的按会话推送,属于 DRY 重构,本轮不做。
- `handleWatchEvent` 在同一个 etcd 键换了 zone 时,直接 Zrem 旧 zone、不写 death_at,本轮只登记。
- Battle 另有一个与跨区无关的问题:结算应用后当场销账 pending,崩溃会丢奖励,需要持久幂等(port-feasibility §8.1 D1),由 Battle 会话报用户。
