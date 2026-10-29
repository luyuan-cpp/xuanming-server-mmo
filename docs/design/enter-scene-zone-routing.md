# EnterScene 选区路由设计

> 核实日期：2026-10-23。基线为本仓库 HEAD `2c07f2c28` 加核实时的工作区源码，涵盖尚未提交的改动。
>
> 本文说明 C++ / Go 版的进场路由、位置投影与持有者交接。下文“已实现”表示已核对实际调用路径，不代表当前工作区已完成编译、真库或双区故障验收；本次更新只做静态核对。部分实现和待落地方案单列于文末。
>
> 项目上线前范围以 [存盘围栏设计](db-write-fencing-mysql.md) 文末“上线状态”为准。本文不对独立 Java 版作实现或验收结论。

## 1. 职责与三类身份

Login 决定进场意图、建立会话并携带角色授权；SceneManager 解析目标区和场景，裁决位置与持有者变更，通过 Kafka 向 Gate 下发路由或重定向；C++ Scene 持有实体、存盘并发布适用的释放凭证。位置记录是调度投影，不能替代账号角色目录的授权或持久化数据归属。

| 概念 | 当前来源与作用 |
|------|----------------|
| 入口区 `GateZoneId` | 本次连接进入的 Gate / Login 所在区，用于判断是否替换 Gate 连接 |
| 目标区 `ZoneId` | 本次请求希望进入的运行区；重连可传 0，由已有位置决定 |
| 归属区 `home_zone` | data_service 提供的数据归属，随 `RoutePlayerEvent.HomeZoneId` 下发；跨区游玩不据此改变存盘归属 |
| 角色所有权修订 `owner_revision` | SQL 账号角色目录的授权修订，防止角色过户后旧请求继续取得路由资格 |
| 登录代次 `login_generation` | 与 `session_id` 一起标识登录会话，防止陈旧登录覆盖新会话 |
| 持有归属代次 `owner_epoch` | Scene 持有权与存盘围栏代次，防止旧持有者继续提交写入 |

三种代次不可互换，也不能用当前目录中的新修订为旧请求补权。

当前正式配置中，data_service 的 RouteStore 为 `db_first`、要求 TiDB 和路由就绪证明。归属与落点以 SQL 为持久真源，读取仍使用经过就绪身份校验的 Redis 副本，副本不可信时 SQL 回源；归属写入仍包含 SQL 提交、Redis 副本发布和发布确认。Login 与 SceneManager 都经 data_service RPC 访问归属，并非自行直连 TiDB，也尚未撤销 Redis 映射副本。依据见 [route_read.go](../../go/data_service/internal/routing/route_read.go)、[route_publish.go](../../go/data_service/internal/routing/route_publish.go) 和 [路由持久化设计](player-route-mapping-durability-proposal.md)。

## 2. 已实现：登录侧选区解析

`resolveEnterSceneRoute` 按以下顺序命中即止，Login 发起的请求始终清零 `SceneId`：

1. **重定向票据锁定本区**：票据中的非零目标区等于本区时，传本区 `ZoneId`，跳过 home_zone 查询，完成跨区旅行第二段。
2. **短线重连或顶号登录**：`ShortReconnect` / `ReplaceLogin` 传 `ZoneId=0, SceneId=0`，让 SceneManager 跟随玩家实际位置（GO-5）。
3. **按归属区引导首次登录**：仅在 `HomeZone.RedirectOnEnterEnabled=true`、首次登录且没有场景内会话时查询 home_zone；返回非零异区才用其作为目标区。
4. **其余情况**：使用 Login 本区。

`GateZoneId` 始终表示 Login 所在入口区。票据绑定了其他玩家时，在写会话和获取玩家锁之前拒绝。按归属区查询失败或明确无映射时，Login 的选区意图回退本区并记日志；后续 SceneManager 的授权、归属和持有者门禁仍可能拒绝进场，不能据此声称 data_service 故障不影响登录。

角色列表通过一次 `BatchGetPlayerHomeZone` 刷新响应副本中的区号；无映射或 RPC 失败保留旧值，不回写账号数据块。`AccountSimplePlayer.zone_id` 仍只是建角提示，不是数据路由依据。

| Login 配置 | 默认语义 |
|------------|----------|
| `HomeZone.RefreshRoleListDisabled=false` | 默认刷新角色列表中的归属区 |
| `HomeZone.RedirectOnEnterEnabled=false` | 按 home_zone 引导进场需显式启用，客户端须能处理 `RedirectToGate` |
| `HomeZone.RoleListLookupTimeout` | 角色列表批查预算，默认 500 毫秒 |
| `HomeZone.EnterLookupTimeout` | 首次进场归属单查预算，默认 1.5 秒 |

实现入口：[entergamelogic.go](../../go/login/internal/logic/clientplayerlogin/entergamelogic.go) 的 `resolveEnterSceneRoute` / `buildEnterSceneRequest`，以及 [homezone.go](../../go/login/internal/logic/pkg/homezone/homezone.go)。

## 3. 已实现：进场授权、幂等与目标解析

### 3.1 授权先于幂等缓存

`EnterScene` 要求请求携带非零的原始 `OwnerRevision`，并通过带独立签名的 `LookupAccountRole` 强读角色目录，确认记录有效、未交易冻结且修订与请求相等。缺修订回 gRPC `InvalidArgument`，修订失效回 `FailedPrecondition`，依赖或读取失败回 `Unavailable`。该检查在去重缓存读取、场景预占和正常落点之前执行。

SceneManager 正常启动已要求 `DataServiceRpc` 和角色目录签名能力。`home_zone.go` 中仍有无 RPC 回退分支，但不能把它当作正规部署能够绕过角色修订核验的能力。

修订通过后，维护 `location_revision_floor` 这一“已观察 SQL 修订下界”，并原子读取位置原文与 `location_revision` 标签。标签由修订和原始 protobuf 的 SHA-256 绑定：已知旧修订的位置投影可条件清理；新于本请求的投影拒绝；无标签的旧记录不自动补成当前修订，仍走原持有者证明协议。后续落点、身份写入、回滚和离场提交继续校验原文、标签及修订地板。

### 3.2 请求幂等与会话身份

非空 `request_id` 使用 `enter_scene:dedup:<player_id>:<request_id>`，TTL 为 60 秒。确定性请求指纹保留 `owner_revision`，排除每次发送的 `correlation_id` 和每条登录链的 `login_generation`。同指纹请求仍在执行时回 15，复用 ID 却改变内容时回 16；只缓存完整成功响应，重放不补发 Kafka 路由。错误释放本次 pending，完成和释放均按精确原值 CAS。

读取位置后，再按 `(session_id, login_generation)` 裁决会话：陈旧或冲突的跨会话请求回 25；同有效会话保留最大代次。兼容无代次请求的分支仍存在，但不能降低已有的非零身份。

`correlation_id` 用于对应某次异步发送，响应原样回显；它不决定路由。C++ Scene 自发进场经 `RequestSceneChange` / `SendCorrelatedEnterScene` 统一出口发送并携带非零关联号，Login 的关联号为 0。镜像创建完成的入口是 `player_scene.cpp` 的 `PlayerSceneSystem::HandleMirrorCreateDone`，旧 `scene_manager_response_handler.cpp` 已由它取代。

### 3.3 目标区解析链

在过滤已确认失去持有者的位置记录后，按以下顺序确定 `targetZoneId`：

1. 调用方显式传入的 `in.ZoneId`。
2. 仍为 0 且 `SceneId!=0` 时，查询 `scene:<id>:zone`。
3. 仍为 0 时，仅跟随 `node_id` 非空且记录自身 `zone_id` 非零的 `PlayerLocation`；这一级不使用从位置的 scene_id 反查出的区补足记录。
4. 最后使用 `in.GateZoneId`。

**待落点记录不牵引目标区**：`node_id="" && owner_epoch!=0` 表示没有持有者，无论是否超过 300 秒，都不能把 `ZoneId=0` 重连拉向上次旅行目标。

300 秒只限制待落点记录中的地图提示：记录目标区等于本次落点区、请求未指定实例和地图、记录尚未过期时，才尝试采用 `pending_scene_conf_id`。该提示无法解析时可回落默认大世界；调用方显式指定的地图或实例无法解析时拒绝。默认大世界由 `defaultWorldConfID()` 读取 World 表首行，预检与实际分配共用这一入口。

指定实例时检查已知所属区与目标区是否一致，并核对实例映射及节点身份。自动选取世界频道由 `ReserveBestWorldChannelForEnter` 预占名额，后续拒绝须退还本次预占。

以上实现见 [enterscenelogic.go](../../go/scene_manager/internal/logic/enterscenelogic.go)、[placement_identity.go](../../go/scene_manager/internal/logic/placement_identity.go) 和 [location_revision.go](../../go/scene_manager/internal/logic/location_revision.go)。

## 4. 已实现：跨区重定向与数据归属门禁

`GateZoneId != 0 && targetZoneId != 0 && GateZoneId != targetZoneId` 时，在解析目标场景及预留名额之前走重定向。因此重定向第一段不占用目标场景名额。

| 当前状态 | 第一段处理 |
|----------|------------|
| 没有位置记录，或目标区就是记录中的当前区 | 只移动连接，不因重定向写位置或生成新 epoch |
| 已有持有者，确实离开其所在区 | 预检目标地图、校验数据归属与合服围栏、检查当前 epoch 的交接标记；获准后 CAS 写待落点记录 |
| 已有待落点记录 | 没有持有者，无须再证明旧持有者释放；按本次目标和待落点规则处理 |

离开已有位置所在区时，`SceneConfId=0` 也要预检默认大世界，不再只预检显式地图。`rejectTravelToUnopenedMap` 读到目标频道集合为空时拒绝；**集合读取出错的当前分支会继续第二段解析**，该只读预检不是对全部 Redis 错误都拒绝的门禁。纯连接重定向不做这项地图预检。

`AssignGateForZone` 为目标区选择新 Gate，优先过滤排空节点；当前排空标记读取失败或所有候选都在排空时，会回退原候选继续选择，不能视为严格拒绝排空节点。找不到 Gate 时拒绝，成功后签发绑定玩家和目标区的票据。有源 Gate 的请求通过共享命令寻址，向**当前入口 Gate** 发送 `RedirectToGateEvent`，`TargetInstanceId` 取请求中的 `GateInstanceId`，事件内另带新 Gate 地址及票据，以玩家 ID 为消息 key；必须收到 Kafka broker 确认才能返回并缓存成功。当前控制命令发送器是同步 `RequireOne`，不能写成客户端已收到或目标场景已加载成功。排空选择边界见 [gate_redirect.go](../../go/scene_manager/internal/logic/gate_redirect.go) 的 `drainingGateIDs` / `filterDrainingTargets`。

`resolveHomeZone` 的作用是确定下发给 Scene 的数据归属：

- 归属查询传输失败或超时：拒绝，返回 `ErrHomeZoneUnavailable`（20）。
- 已映射但归属区处于合服围栏：拒绝，返回 `ErrHomeZoneMerging`（21）。
- 明确无映射：SceneManager 仍保留首次落点或同区换图回退入口区的分支；跨区两段均拒绝。该兼容分支不等于正式 data_service 可以跳过角色准入、创建资格或持久化屏障。

**当前覆盖边界**：跨区离区第一段无论是否带 `GateId` 都检查归属；普通落点路径只在 `GateId` 非空时查询。无 GateId 的内部请求仍可能写位置或 epoch，不能声称所有 EnterScene 都经过同一合服围栏。检查时机、缺口与指标见 [home_zone.go](../../go/scene_manager/internal/logic/home_zone.go)。

拒绝正常落点不等于“Redis 一个字节都没变化”：入口可能已记录修订地板、修复旧投影，或已进行本次预占再退还；部分 18 响应前还会请求旧持有者释放。

## 5. 已实现：交接凭证、epoch 与失败回滚

### 5.1 交接标记的两类写入路径

标记格式为 `<epoch>:<saved_at_ms>`，TTL 为 300 秒。SceneManager 在放行需要换手的请求时比对当前 `owner_epoch`；标记过期、畸形或代次不符不能证明释放。`saved_at_ms` 用于关联本次交接，不以时间新旧代替归属证明。

| 标记来源 | 当前执行路径与确认边界 |
|----------|------------------------|
| 主动跨区旅行、同区跨节点旅行交接 | `BeginTravelHandoff` 调用 `handoff_sql_barrier::kPublishMarkLua`：同一个 Lua 核对旧 epoch，并要求 `save_barrier` 中 `player_database`、`player_database_1` 两张表的 ACK 都等于 `pending` 后才写标记；未齐则在原冻结代次和截止预算内等待 |
| 正常断线退出、关机或排空的 A1′，A2′清理后放弃载入时的标记补写，以及相关凭证补写 | 共用 `SendConditionalMarkWrite` / `exit_release_mark::kLuaWriteIfOwnerEpoch`，原子核 epoch 后写标记；不能概括成与主动旅行相同的双 SQL ACK 屏障 |

退出链的 `FinishExitOrAwaitDbTaskAck` 等待的是 DBTask 的 Kafka broker 回执，不是 go/db 的 SQL 提交 ACK；部分永久拒绝处理分支仍可能在 SQL 落后时完成退出释放。此实现边界须保留，不能把所有 handoff 标记解释为“两表 SQL 已提交”。代码依据：[handoff_sql_barrier.h](../../cpp/libs/services/scene/player/system/handoff_sql_barrier.h)、[exit_release_mark.h](../../cpp/libs/services/scene/player/system/exit_release_mark.h)、[player_lifecycle.cpp](../../cpp/libs/services/scene/player/system/player_lifecycle.cpp)。

需要换手且没有当前代标记时，安全路径返回可重试的 `ErrHandoffPending`（18）；开发旁路见 §5.2。旧码 `ErrUnsafeCrossNodeHandoff`（14）已退役。`ReleasePlayer` 请求本身不是持久化确认。

### 5.2 当前落点提交与代次生成

实际落点仍由 `placePlayerLocation` 调用原 Redis 客户端执行 Lua。Lua 原子复核观察到的 epoch、位置原文、角色修订地板与标签；凭标记换手时还要原样复核该标记，然后写入位置投影。

- 首次落点、安全跨节点换手、跨区放行和待落点第二段生成新 epoch；**当前执行仍为 `INCR`，位置中写 `observedEpoch + 1`**。
- 同节点换图、同落点重连保持 epoch，避免把仍合法持有实体的节点提前废黜。
- 同物理节点的旧记录和 epoch 键都为 0 时有补铸分支；仅键缺失而位置记录了非零 epoch 时，按记录值条件补种。
- 新位置记录同时携带本次会话身份；跨区第一段获准时写入无持有者的待落点记录及 `pending_scene_conf_id`。

`AllowUnsafeCrossNodeHandoff=true` 的开发旁路**仍存在于实际分支**：有合法标记先走安全路径；无标记则可放行且不生成新 epoch。结构默认值为 false，但仓库本地 `scene_manager_service.yaml` 核实时显式为 true。代码没有在该分支强制判断部署环境；不能把“仅供开发”写成生产二进制已机械禁止。

### 5.3 发送失败与单调回滚

路由或重定向发送失败后，条件回滚只处理本请求写入的原文、修订标签和 epoch；不覆盖后来的落点。已生成新 epoch 的一般回滚再次递增，不退回旧值；不生成新代次的落点，以及同物理节点 epoch 0 补铸的特殊回滚走 keep 分支。

凭标记换手的回滚，还要求原标记原样存在，恢复旧位置时写入 `rollback_receipt` 并把标记转写到回滚后的 epoch。因此 SceneManager 并非只读 handoff。源 Scene 必须通过 `ResolveTravelOutcome` 原子核实与本次交接匹配的回执才能采纳新代次；`owner_epoch_after_rollback` 响应字段只供诊断，不能单独授予持有权。投递成功、票据送达、客户端第二段落点是不同阶段，未收到应答也不能直接当作未提交。

实现入口：[changesceneutil.go](../../go/scene_manager/internal/logic/changesceneutil.go) 的 `placePlayerLocation`、[owner_epoch.go](../../go/scene_manager/internal/logic/owner_epoch.go) 的落点与回滚 Lua，以及 [跨区旅行设计](cross-zone-scene-travel.md) §12.8。

## 6. 已实现：离场、重连与死节点接管

### 6.1 离场需要释放正证

`LeaveScene` 先核对角色修订及原会话身份，再证明当前持有者已释放：当前 epoch 的释放标记、经确认的节点死亡，或位置本身明确无持有者。最终删除 Lua 再次核对原位置、修订标签及适用的 epoch / 标记，成功后才归还人数。

拿不到证据时保留位置与人数，返回 gRPC `FailedPrecondition`（`holder_release_pending`），需要时催促原持有者按被覆盖会话退出。`LeaveSceneHolderReleaseWaitSeconds` 默认 600 秒，现在只是诊断预算；等待超时、非正配置、旧开发旁路或 epoch 缺失都不构成删除权限。因此“正常登出调用 LeaveScene 就立即删 location”已不成立，清理须等待协议收敛。见 [leavescenelogic.go](../../go/scene_manager/internal/logic/leavescenelogic.go)、[leave_release.go](../../go/scene_manager/internal/logic/leave_release.go) 和 [位置释放绑定设计](location-release-binding.md) §15–§16。

普通落点在换手门返回 18 时，对身份严格更新、原位置与 epoch 有效、持有者可寻址且请求链仍存活的 Login 请求，`requestHolderReleaseForLogin` 可同步发送会话结束释放，最多占用 1 秒且不超过请求剩余预算，本次仍返回 18。Scene 自发请求不走此补发；跨区重定向第一段的拒绝分支也未调用它，不能概括成所有登录路径均会主动释放旧持有者。

### 6.2 已有位置与断线返回

- 断线租约默认 30 秒，AFK 通行证可延长。租约内重连或顶号由 GO-5 跟随有持有者的位置；从其他区入口接入时可能先替换 Gate 连接。
- 登出或租约到期后，player_locator 发起离场；只有释放证明及删除 CAS 完成后，下一次才按无位置的首次落点处理。
- 需要跨节点或离区换手、尚未满足死亡证明且无当前代交接标记时，安全路径保持拒绝；同节点换图、同落点重连不要求此标记，开发旁路见 §5.2。
- 待落点无持有者，后续登录不被旧旅行目的区牵引；地图提示的 300 秒有效期按 §3.3 处理。

### 6.3 陈旧位置的正面死亡证明

整区消失的 `playerLocationOwnerGone` 要求：本副本已完成 etcd 首次全量同步，注册表确认该区没有任何场景节点；适用的本地观察屏障已过；负载集合为空；节点不被判定为存活；适用的 `death_at` 再入屏障已过。**负载集合为空本身不足以证明死亡**，丢键不能让活持有者失去资格。

单节点接管由 `playerLocationOwnerDead` 另行验证节点身份、注销证据、负载与屏障；无法确定所属区、节点身份歧义、Redis 故障或屏障未过时保持拒绝。再入屏障默认 20 秒，用于覆盖旧进程退出时仍可能进行的紧急存盘窗口。

确认失主后，可忽略旧投影并在新落点生成新 epoch。它不恢复死亡节点从未成功保存的内存进度，也不能绕过 Login 冷加载及存盘提交屏障。依据见 `enterscenelogic.go` 的 `playerLocationOwnerGone` / `playerLocationOwnerDead`、[load_reporter.go](../../go/scene_manager/internal/logic/load_reporter.go)、[reentry_barrier.go](../../go/scene_manager/internal/logic/reentry_barrier.go)。

## 7. 当前 Redis 键与部署前提

下表的尖括号是占位符，实际键例如 `player:42:location`，不带字面花括号。这组位置与归属键尚未整体改成 Redis Cluster 的同槽键。

| 键 | 当前内容与约束 |
|----|----------------|
| `scene:<id>:zone` | 场景实例所属区；世界场景与按需场景创建时写入，用于反查 |
| `world_channels:zone:<zone>:<conf>` | 按运行区和地图配置维护的世界频道集合 |
| `player:<id>:location` | `PlayerLocation`：scene_id、node_id、update_time、zone_id、owner_epoch、pending_scene_conf_id、rollback_receipt、session_id、login_generation |
| `player:<id>:owner_epoch` | 持有归属代次；不随离场删除，不能通过删键重置 |
| `player:<id>:handoff` | `<epoch>:<saved_at_ms>`；源 Scene 条件写入，特定失败回滚可由 SceneManager 转写，TTL 300 秒 |
| `player:<id>:location_revision` | SQL 角色修订与位置原文摘要的绑定标签 |
| `player:<id>:location_revision_floor` | 已观察角色修订下界，不是授权真源 |
| `player:<id>:leave_release_wait` | 离场释放等待的诊断记录，不因到期而授予删除权限 |
| `enter_scene:dedup:<id>:<request_id>` | 请求执行中与成功结果的幂等记录，TTL 60 秒 |
| `node:zone:<zone>:<node>:death_at` | 节点注销后的再入屏障证据之一 |

位置、归属、修订及相关多键 Lua 当前仍要求共享实例内的原子操作。服务主链尚未接入新的玩家域 Cluster，不能因 `shared/playerstore` 或 C++ 集群路由基础代码已经存在，就声称本流程已支持分片部署。`PlayerLocation` 的源契约见 [storage.proto](../../proto/scene_manager/storage.proto)，请求契约见 [scene_manager_service.proto](../../proto/scene_manager/scene_manager_service.proto)。

## 8. 当前合服边界

归属区变更走 `tools/merge_zone` 及其路由存储、合服围栏和核验协议；`DataService/RemapHomeZoneForMerge` 已退役，不能通过该 RPC 改映射。工具必须显式选择与 data_service 模式匹配的 `-route-source`，不会自动读取服务配置。正式 `db_first` 对应 `-route-source=db`：先在全局 `player_route` 核围栏、锁行并 CAS 修改归属、推进路由修订，SQL 提交后发布完整 Redis 路由副本，经 `WAITAOF` 确认才算本批完成；发布失败可重跑补发布。旧 `redis` 来源仅适用于仍全部采用旧 Redis 模式的环境。

只改归属区不等于搬迁所有玩家存储，原行的 storage placement 可保持不变。公会、榜单、持久化屏障、数据搬迁和 Kafka 排空按 [合服操作手册](../ops/merge-zone-runbook.md) 执行，不能把合服简化成手动改 `player:zone`。

源区完整下线并满足 §6.3 的死亡证明后，SceneManager 可自行忽略陈旧位置；清理工具不是解除持有者门禁的替代品。`tools/merge_zone -clear-source-hot-state`（脚本参数 `-MergeClearSourceHotState`）核对源区负载集合非空时拒绝执行；该工具的这项门禁本身没有完整证明 etcd 注册表及再入屏障，不能将它写成等价的自动安全证明。运行前仍须完成手册中的源区下线、屏障和存盘收敛步骤。

清理先处理可确定属于源区的位置，再删场景、节点及频道状态（包括 `death_at`）；无法判断所属区或解码失败的位置保留。当前保留 `owner_epoch`、`handoff`、`location_revision` 和 `location_revision_floor`。不要通过手动删除位置修复交接拒绝：位置缺失会使后续按首次落点取得新 epoch，可能废黜仍有未存盘状态的旧持有者。所属区未知或投影损坏也需要受控核验，不能仅因无法判断就授权删除。

`owner_epoch`、修订地板和 `consumer:applied_epoch:*` 等围栏不能当普通缓存单独清空。合服清理与数据恢复流程以操作手册及 [scene_hot_state.go](../../tools/merge_zone/scene_hot_state.go) 为准；归属在源区但仍作为访客在线于其他区的玩家，也不能仅靠清源区热状态证明已安全迁移。

## 9. 部分实现与待落地方案

下表与前文的当前执行路径分开，不能作为已经完成运行验收的能力使用。

| 项目 | 核实时状态 | 尚需完成的接缝 |
|------|------------|----------------|
| epoch 墙钟地板与 `WAITAOF` | **部分实现**：`OwnerEpochMinter` 执行器、配置、专用客户端和启动探针已有源码并装配；落点业务尚未调用它 | `placePlayerLocation` 仍 `observedEpoch+1` 后直接 `Redis.Eval`，Lua 仍 `INCR`；不能据 `owner_epoch.go` 文首的先行注释宣称主链已具备墙钟地板或铸造持久确认 |
| 玩家态 Redis Cluster | **分批实施**：`shared/redisslot`、`shared/playerstore` 等基础代码已存在 | 位置、handoff、修订等键改名、服务句柄、Lua 原子范围和部署验收尚未全链接通；当前运行前提仍见 §7 |
| 撤销 Redis 映射副本、归属与落点直读 TiDB | **2026-10-23 方向，待落地** | 当前仍有可信副本读、SQL 回源、Redis 发布、就绪标记和对账路径；不能将选定方向写成现状 |
| 全部释放标记统一双 SQL ACK 保障 | **当前存在覆盖差异** | 主动旅行已有双 ACK 门；退出及凭证补写路径见 §5.1，不能由前者推定后者已覆盖 |
| 全部内部进场请求统一归属 / 合服门禁 | **当前存在覆盖差异** | 普通落点的无 `GateId` 分支见 §4，尚不能声称统一覆盖 |

epoch 目标方案与实际接线状态对照 [zone-redis-regression-guard-20261022.md](zone-redis-regression-guard-20261022.md)、[owner_epoch_mint.go](../../go/scene_manager/internal/svc/owner_epoch_mint.go) 和实际落点调用点。玩家域最新实施路线见 [redis-player-store-cluster.md](redis-player-store-cluster.md)：全服逻辑玩家域按 player_id 分片、控制域保留现有共享实例；该稿已替代旧 G4a 中“单元化 + 单元内 Cluster”的实施路线，具体批次状态以该稿为准。映射直读方向也须等待 [路由持久化设计](player-route-mapping-durability-proposal.md) 的对应方案与代码接线，不按方向变更推断已经完成。

## 10. 核验与维护入口

| 主题 | 源码或设计入口 |
|------|----------------|
| 登录意图、原始角色修订与请求组装 | `entergamelogic.go` 的 `resolveEnterSceneRoute` / `buildEnterSceneRequest`；`account_directory.go` |
| 目标区、地图预检、跨区重定向 | `enterscenelogic.go` 的 `EnterScene` / `rejectTravelToUnopenedMap` / `handleCrossZoneRedirect` / `resolveSceneForEnter` |
| 位置与持有者原子提交 | `changesceneutil.go`、`owner_epoch.go`、`location_revision.go`、`placement_identity.go` |
| 离场与登录催释放 | `leavescenelogic.go`、`leave_release.go`、`enter_holder_release.go`；[位置释放绑定设计](location-release-binding.md) |
| 源 Scene 存盘、旅行标记与回滚采纳 | `player_lifecycle.cpp`、`handoff_sql_barrier.h`、`exit_release_mark.h`；[跨区旅行设计](cross-zone-scene-travel.md) |
| 地图与场景所属区登记 | `world_init.go`、`createscenelogic.go` 的 `allocateScene` |

本次已做源码调用核对；未运行编译、单元测试、真 Redis / TiDB、跨区或合服故障用例。后续实现变更应复核 `owner_epoch_test.go`、`placement_identity_test.go`、`enter_holder_release_test.go`、离场释放测试和 C++ 交接测试等对应行为，并按 [PROGRESS](../PROGRESS.md) 的实际输入与运行结果记录证据。既有测试源码、历史通过结果及本次静态核对都不能代替最新工作区的运行验收。
