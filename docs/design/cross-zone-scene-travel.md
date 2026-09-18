# 跨 zone 场景传送(玩家去别的 zone 的地图游玩)

**状态:** 阶段 1(归属 + 两道换手门)**已落码,未 regen、未编译、未测试**(2026-09-18,见 §10;Codex 清单见 §9)。阶段 2(`TravelToZone` 入口 + 访客识别)、阶段 3 未开工。§8 三个默认值按"不确认即按此执行"生效。
**关联:** [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md) D1/D2(跨区 = 客户端重连,数据不搬家)、[enter-scene-zone-routing.md](./enter-scene-zone-routing.md)(Cross-Zone Flow / Offline-Return / Login-side)、[scene-owner-reentry-barrier.md](./scene-owner-reentry-barrier.md) §3.3(owner_epoch)、[scene-switch-release-design.md](./scene-switch-release-design.md)、[cross-zone-readiness-audit.md](./cross-zone-readiness-audit.md)、[team-system.md](./team-system.md) DV-6 / D.3、[turn-based-battle-server.md](./turn-based-battle-server.md) §19。

> 用户 2026-09-16 拍板:客户端直连 battle 与跨 zone 场景传送"两个都做",先直连。本文只管传送。
> 一句话:**玩家仍归属 home_zone,只是人换到目标 zone 的 gate/scene 上玩;数据不搬家,存盘按 home_zone 路由;换手必须过"源已落盘 + epoch"两道门。**

## 1. 目标与非目标

- 目标:A 区玩家可以传送到 B 区的地图,与 B 区玩家同场景互动(观光、PVE、进副本、打战斗),之后可回家或下线后回家。
- 非目标:合服(另有 [server_merge_design.md](./server_merge_design.md));跨区组队/帮会/榜单归属变更(仍按 home_zone,见 CZ-6);TiDB Phase 2 全局库(本设计是它的前置,不等它)。

## 2. 现状(2026-09-16 摸底,引用为工作树位置)

- **运输层已齐**:`scene_manager` `handleCrossZoneRedirect` → `AssignGateForZone` 签 5 分钟 HMAC 票据 → Kafka `RedirectToGateEvent` → gate 推 msg 124 → Unity `GameClient.RedirectFlow`(探测 / 先连新后关旧 / 原样转票据 / 完整重跑 Login + EnterGame / 3 跳熔断)。跨区匹配 9-05 实机跑通过这条链。
- **生产 fail-closed**:`enterscenelogic.go:278 / :333`——玩家 Redis 里有 `player:{id}:location` 时跨区/跨节点一律 `ErrUnsafeCrossNodeHandoff`;`AllowUnsafeCrossNodeHandoff` 生产 false(本地 yaml true 仅供联调)。只有干净登出后的"首次落点"才会被重定向。
- **login 侧**:`HomeZone.RedirectOnEnter` 只会把人"送回 home_zone",没有"去别的 zone"的意图表达;打开它,目标 zone 的 login 会把访客再弹回家(与 RedirectFlow 的 3 跳熔断互撞)。
- **数据归属**:scene 只读本 zone `zone_redis` 的 `PlayerAllData:{id}`(`scene_handler.cpp:198`),NIL 直接当新号(`player_lifecycle.cpp:126`);存盘固定 `GetDbTaskTopic(GetZoneId())`(`player_lifecycle.cpp:1309`)→ `zone_{本zone}_db`。K8s 脚本让所有 zone 指向同一个 infra Redis,但 MySQL 一定分库。
- **交接屏障**:再入屏障(节点判死后 20s)已落码;per-player `owner_epoch` 在 proto / Go / C++ 三处均未落码(`RoutePlayerEvent` 无 `owner_epoch`,`changesceneutil.go:66` 裸 SET,C++ 无 CAS)。
- **player_migrate 搬数据链**:代码齐全(Frozen + ACK + reaper,已订阅),`PlayerAllData` 已含 bag / mission(2026-05 审计的"7 组件"已部分过时;mail 按决策走独立 Go 服务),但**全仓无任何写 `to_zone_id` / `is_cross_zone` 的触发点**,且目的端按空 enterInfo 建实体、不改 location、存盘打到目的 zone 库——与 D2 冲突,不作为路径。
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
| CZ-7 | **意图入口 = 客户端 RPC `ScenePlayer.TravelToZone{target_zone_id, scene_config_id}`**(走 scene,不走 login):scene 校验 CZ-6 规则与目标 zone 存在性(`zone_config` 经 scene_manager)→ 触发 CZ-5。传送点 / 地图选择 UI 由客户端决定何时调用;GM 命令同一入口 | 传送是场景内行为,状态(冻结、存盘)只有 scene 知道;login 的 `RedirectOnEnter` 只负责"登录时送回 home",两者互不干扰 |
| CZ-8 | **目标 zone 识别访客**:重定向票据 `GateTokenPayload` 增加 `player_id` 与 `target_zone_id`(绑定持票者,顺手堵掉"持票者可冒用"的既有缺口);目标 zone 的 login `EnterGame` 看到票据 `target_zone_id == 本 zone` 时**不按 home_zone 弹回**,直接 `EnterScene(ZoneId=本 zone)`;票据 TTL 内未落地则视为传送失败,下次登录按 Offline-Return 规则回家 | 消除重定向乒乓;访客身份有据可查 |
| CZ-9 | **回家 = 同一条链反向**(`TravelToZone{target=home_zone}`);离线 / 崩溃后按 enter-scene-zone-routing.md Offline-Return:干净登出回家,崩溃遗留的异 zone location 由 CZ-4 的 handoff 标记决定能否直接接管(标记齐则放行、否则运维清理) | 不新增第二套回家机制 |
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
- proto:`ScenePlayer.TravelToZone` 客户端 RPC + tip 码(`Tip.xlsx` 新段或 scene 段:目标区不存在 / 战斗中不可传送 / 组队中不可传送 / 目标区繁忙);
- C++ scene:`TravelToZone` handler 走 CZ-5;`HandleCrossZoneTransfer` 的在途校验复用;
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
5. **双 zone 联调**(`dev-start-zones 1,2`,`AllowUnsafeCrossNodeHandoff=false`):阶段 1 没有 `TravelToZone` 入口,传送往返要等阶段 2;本阶段可验:①跨节点换图被 18(`ErrHandoffPending`)拒绝且不改状态;②`BeginSceneDrain` / 整节点疏散的玩家能被改派(疏散链现在会写 handoff 标记);③`db_stale_owner_write_rejected_total == 0`、`scene_manager_home_zone_lookup_total{outcome="mapped"}` 在涨、`zone_B_db.player_database` 不出现 A 区玩家。
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
| R7 | 跨 zone 重定向分两种:目标 zone 就是玩家所在 zone(或无位置记录)→ **只送连接**,不过门、不写 location、不铸造;真正离开所在 zone 才过门并写「等待落点」 | 访客掉线后从别区 gate 登录时,持有节点没在交接、永远写不出标记,会被永久挡在门外 |
| R8 | 「等待落点」只在票据有效期(300s)内牵引去向,过期按 gate zone 常规落点 | CZ-8 / CZ-9:票据过期 = 传送失败,回家 |
| R9 | guard 键**缺失**时 C++ 存盘放行并补种期望值 | Redis 被清 / 重启后,fail-closed 会把全服在线玩家逐个判成被废黜、丢掉内存里唯一完好的状态 |
| R10 | `DataServiceRpc` 未配置默认 **fail-closed**;单 zone 联调显式 `AllowGateZoneAsHomeZone: true`;K8s 模板已补 | 多 zone 下按 gate zone 当归属 = 访客落错库且零报错(不变量 2) |
| R11 | 应答对回玩家用 `EnterSceneResponse.player_id` 回显,不用 gRPC metadata | 少一层管线;去重缓存重放也不丢 |

### 10.3 本阶段明确没做(已知限制)

- **生产下客户端发起的跨节点换图仍被拒**(从 14 变成 18):那条路径是"先请求、后由 scene_manager 通知旧节点释放",没有"先存盘后请求"的顺序,写不出标记。要打通需把 `EnterSceneC2S` 跨节点分支改成与传送同一条释放链(另立任务,见 scene-switch-release-design.md)。疏散 / 排空本来就先存盘,本阶段已接上。
- 单节点硬崩(zone 内还有别的活节点)遗留的 location:标记永远不会出现,再入屏障过后仍被 18 挡,需运维清理(CZ-9 原话)。"判死 + 屏障已过 = 无持有者"是后续项。
- `EnterScene` 每次多一跳 `data_service` 同步 RPC(含同落点重连),`data_service` 不可用即拒绝进场景。压测时看 `EnterScene` 分阶段时延;若成瓶颈再议缓存(归属只在合服时变)。
- 两段多键 Lua 依赖三把 `player:{id}:*` 键在同一 Redis 实例。键名无 hash-tag,**不支持 Redis Cluster**(与 CZ-2 的"物理共享单库"契约一致)。db / scene_manager / C++ scene 必须指向同一个 Redis。
- C++ 三个归属计数只进 30s 一行的 `[OwnerEpoch]` WARN,没进 Prometheus,`stress_summarize.ps1` 也不解析;C++ scene 侧新逻辑无单测(redis client 有)。
- 路由发送失败会把 epoch 退回旧值(非单调)。残余窗口:kafka-go 报错但 broker 实际已投递 → 目标节点拿着被收回的值载入,其存盘被拒后自毁,location 已指回源节点,玩家重连恢复,不会双主。
- 阶段 2 契约缺口(不在本阶段):login `EnterGame` 识别票据 `target_zone_id`、gate 验签绑定 `player_id`、`TravelToZone` RPC 与 tip 码、客户端调用点。
