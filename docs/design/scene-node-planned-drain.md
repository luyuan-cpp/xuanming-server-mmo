# scene 节点计划排空：重启、缩容、滚动升级时把玩家搬到别的节点（2026-11-11）

> **状态**：方案已定，批 1~8 全部落码（§9），查出的残余都已修完，数字按标准做法定案（§4.7）。
> - 批 1~5（2026-11-11）：协议已生成，C++ 与 Go 随 Codex 11-12 的全量构建编译通过（`build/verification/docker-rebuild-all-20261112T040700.029055Z`），部署后双机器人登录进场通过；本文新增的单测、脚本契约测试与计划排空冒烟还没有运行证据。
> - 批 6（2026-11-12）：gate 与 Go 服务的收尾，未编译、未运行。
> - 批 7（2026-11-12 晚）：Agones Fleet 也按模板修订分代、模板不变的整批重启（§4.6、§6），以及演练清单（§11）；只有脚本与文档，未运行。
> - 批 8（2026-11-13）：Go 服务的两处交接空窗——选主竞选间隔与锁 TTL 脱钩、login 与 guild 停机时等后台收尾（§9），未编译。
> - Java 版待做（§10）。
> **需求来源**：负责人 2026-11-11：「scene 重启之前是把当前的节点玩家传送到已有 scene 节点（新版本节点优先），或者是收缩的时候，这样玩家就像无感一样」；同日要求「按最标准的方式做：数据强一致、代码易懂、可水平扩展，Java 留待做」。总需求与逐节点现状见 [全类型节点平滑停机需求](node-lifecycle-elasticity-release-20261023.md) §3.7。
> **核对基线**：HEAD `e0790ad40` 加 11-11 工作区。十几个会话并行在改，行号会漂，按函数名找。
> **上线状态**：未上线，玩家数据可以整体清空，不为旧部署做兼容。新旧版本只在滚动窗口里并存，按 §6 的发布顺序。

## 1. 目标与边界

计划下线一台 scene 时（K8s 滚动升级、缩容、`kubectl delete pod`、Agones 排空），玩家的体验应是一次切线：

| 玩家所在 | 去向 | 保留什么 |
|---|---|---|
| 主世界频道（含跨服主世界） | 另一台节点上的**同一个频道**（同 scene_id） | 地图、频道、存盘那一刻的坐标；gate 连接不断 |
| 镜像 | 同一张地图里最空的频道 | 地图与坐标；镜像本身按需重建 |
| 副本（含跨服副本） | 先等副本自然结束；到期限仍没走的送回默认大世界 | 角色数据；副本进度不搬 |

- 落点**优先选不是这次要换下的那个构建**的节点（滚动升级时就是新版本），尽量一次搬到位，§4.6。
- 数据不丢不重：完全复用现有「存盘落地 → 交接标记 → EnterScene 换手门 → 新节点从 TiDB 载入」链路，不新增提交点，§5。
- 有界：超过预算还没搬走的玩家按现有停机路径存盘下线（数据安全，需要重登），并告警。
- **不承诺**：施法、战斗中状态、怪物与 NPC 状态、副本进度跟着搬；scene 内存状态没有跨进程迁移，搬走的是人，不是场景。
- 宽限期、预算、限速等数字按标准做法推导定案，§4.7 写了每个数的依据（负责人 2026-11-11：不等压测，先按最标准的定）。压测只用来核对：不够时调参数，不改机制。

客户端不用改：同一张图的进场通知只清空并重建场景里的角色、不重装地图（客户端 `TianyongMapRuntime.EnterScene` 对同图走 `PrepareActiveMap`），与玩家主动切线同一条路径。

## 2. 现状（11-11 核对）

1. **普通停机是存盘下线。** SIGTERM → `Node::RequestShutdown` → `ShutdownInLoop`：先停 Kafka 消费、关 RPC 入口，再跑 scene 的 `exitAllPlayers`（`ExitCause::kNodeShutdown`），15 秒排空看门狗后退出。玩家全部要重新登录（[容量模型](capacity-model-hok-scale.md) V6 就按「停机后全员重登」估）。
2. **整节点疏散已有，只在身份冲突时用。** `PlayerLifecycleSystem::BeginEmergencyRelocateAll`：抄会话 → 存盘落地 → 写交接标记 → `EnterScene` 改派 → 待确认表收口（`relocate_confirm.h`）。单场景版 `BeginSceneDrain` 被 `DestroyScene` 调用（频道缩容、镜像级联、频道迁移拆老实体、归属对账）。
3. **改派落点固定是默认大世界。** `SendEmergencyRelocateEnterScene` 发 `scene_id = 0, scene_conf_id = 0`，scene_manager `resolveScene` 取 `defaultWorldConfID()`（World 表第一行）最空的频道。后果：
   - 非默认地图上的玩家被换了地图。频道缩容同样如此，与 `world_autoscale.go` 文件头「改派到同图的其它频道」的说法不符；
   - 不排除源节点：身份冲突的节点若恰好托着默认大世界最空的频道，玩家会被派回这台将死的节点。
4. **频道迁移有现成的「先建后改再拆」**（`migrateWorldChannel`：占 Agones 名额 → 分配放置代次 → 目标上 `CreateScene` → CAS 改归属 → 老节点 `DestroyScene`），但规划器拒绝迁移有人的频道（「老进程还在册就不热迁移」）。
5. **scene_manager 不知道哪台 scene 要下线。** `NodeInfo.admission`（字段 14）目前只有 battle 填；选点（`getNodesForPurpose`）只看负载集与角色镜像。
6. **Agones 形态**：Fleet 的滚动更新与缩容只删 Ready 的 GameServer；高密度下托着房间的 scene GameServer 是 Allocated，旧构建的会一直留着。lifecycle 已有排空标签能力（D83），battle 开了，scene 没开。Fleet 模板已带 `mmorpg.io/build` 标签。
7. **Deployment 形态**（缺省）：`maxUnavailable: 0`、`maxSurge` 缺省 25%，新 Pod 就绪后才给旧 Pod 发 SIGTERM；scene 的 `terminationGracePeriodSeconds` 缺省 60 秒。
8. **gate 已支持「scene_id 不变、节点变了」重新进场**（`scene_route_helper.h` 第 3 条），scene_manager 的 EnterScene 把它当跨节点交接（`samePlacement` 要求场景与节点都相同），成员身份按 location 保留重叠部分（`removeSceneMember`）。

## 3. 方案总览

标准做法是「先摘流，再搬工作，最后停进程」（同 K8s 的 cordon / drain）：

```text
触发：SIGTERM（Deployment）或 Agones 排空标签
  scene 进入计划排空，进程照常服务（Kafka、RPC、gRPC 都不停）
    ├─ 关建场景准入；etcd 里本节点 NodeInfo.admission = CLOSED
    └─ 等：本节点没有玩家、没有在途改派；或到预算
  scene_manager 领导者经 etcd watch 看到 CLOSED
    ├─ 这台不再接新频道、新副本、新玩家，也不当疏散落点
    └─ 排空迁移：把它托着的世界频道逐个迁到别的节点（允许有人）
         目标节点 CreateScene(同 scene_id, 新代次) → CAS 改归属 → 老节点 DestroyScene(代次)
  老节点 DestroyScene → BeginSceneDrain → 每个住户（限速）：
         存盘落地 → 写交接标记 → EnterScene(evacuation, scene_id=原频道, conf=原地图)
  scene_manager 解析疏散请求：原频道已归新节点 → 派过去；gate 同 scene_id 换节点重新进场；
         新节点从 TiDB 载入，坐标保留
  过半预算还有人（scene_manager 慢或不可用、副本没结束）：节点自己逐场景疏散
  排空完成或到预算 → 走现有停机路径（剩下的人存盘下线）
```

## 4. 详细设计

### 4.1 触发

- **Node 通用的计划排空阶段**（C++ core，`Node::SetPlannedDrain` / `Node::RequestPlannedStop`）。节点登记了计划排空才生效；没登记的节点 SIGTERM 行为不变。
  - `RequestPlannedStop`：任意线程可调，投递到 loop。已在停机、或维护流程已关源（`maintenanceControl_->sourceClosed`）时退回普通停机。
  - 进入阶段：调业务的 `begin`，之后每 200ms 查业务的 `isComplete`；完成或到预算就进现有 `ShutdownInLoop`。重复触发幂等。
  - 预算取环境变量 `MMORPG_PLANNED_DRAIN_BUDGET_SECONDS`：不设用节点自己的缺省值；`0` 关闭计划排空（SIGTERM 直接停机，留作应急开关）；非法值拒启。部署按「Pod 宽限期 − 25 秒」注入（25 秒 = 停机排空看门狗 15 秒 + gRPC 两段 GOAWAY 约 3 秒 + 拆除余量），两者出自同一个参数，不会对不上。
- **信号**：SIGTERM → 计划排空；SIGINT（人工 Ctrl+C）→ 立即停机。Windows 控制台事件维持立即停机（只在本机开发）。
- **Agones**：scene 打开 lifecycle 的排空标签（`mmorpg.io/drain`，与 battle 同键）。标签出现 → 计划排空；排空完成后进程自己调 SDK `Shutdown`（这是进程自己决定的终止，不是 SIGTERM 的回应，见 `exitAllPlayers` 注释），Agones 删掉它、Fleet 按当前模板补一台。部署侧给旧构建的 Allocated GameServer 打标签的步骤见 §6。
- GM 停服（`Scene.GmGracefulShutdown`）不变：它用于整服维护，那时没有可去的节点。

### 4.2 摘流

- scene 关建场景准入：复用 `SceneNodeGrpcImpl::CloseCreationAdmission`，排空期间 `CreateScene` 回 `UNAVAILABLE`，scene_manager 按建场景失败回滚。
- etcd 发布 `NodeInfo.admission = CLOSED`：把 battle 的对账发布（`battle_admission_advertise.h`）提成 core 的通用模块，scene 与 battle 共用。只在不一致时写、按 watch 回读对账，稳态零写入，丢写自愈。
- scene_manager 从 etcd 镜像（`knownNodes`）读出 `admission` 与 `build`：
  - `getNodesForPurpose` 排除排空中的节点：不放新频道、新副本，也不作为迁移目标；
  - 选频道（`ReserveBestWorldChannelForEnter`）把排空节点上的频道排在最后，只在别无选择时用；
  - `IsNodeAlive` 不变：排空中的节点仍然活着、仍是它场景的属主。
- 提示过时（watch 还没到）最多让 scene_manager 白发一次 `CreateScene`，被 scene 的准入拒掉后回滚。

### 4.3 世界频道：先迁频道再搬人

- scene_manager 领导者看到某节点变成 CLOSED（watch PUT），为它起一个排空迁移协程（同一 (zone, node) 只起一个）：反复规划 → 逐个 `migrateWorldChannel(reason=node_draining)`，直到它名下没有世界频道、节点不再排空或已消失、或本副本不再是领导者。周期 rebalance 也把排空节点上的频道列为紧急项，协程挂了也能收敛。
- 规划器的改动只有一处：属主排空中（活着、CLOSED）的频道列为紧急迁移，**有人也迁**。理由：老进程的那份实体在住户走光之前一直保留（`SceneLifecycle::DestroyScene` 有绑定就不销毁），新节点建的是同一个频道的新实例，住户从存档重新载入。这不是内存状态热迁移。
- 排空迁移不占 `MaxRebalanceMigrationsPerTick`：下线有时限。对后端的保护在玩家粒度（§4.7 限速），而不是频道粒度。
- Agones 下排空迁移与死节点一样允许回落分配（计划的目标满了让 GSA 另挑），频道必须搬走。
- 迁移之后，老节点收到带代次的 `DestroyScene`，`BeginSceneDrain` 疏散住户；归属对账（`scene_ownership_reconciler`）宽限期过后会再调一次，作为兜底。
- 两份实例并存的窗口：从改归属到最后一个住户离开，同一 scene_id 在新旧两台各有一份，两边的人彼此看不见。窗口长度受限速约束（例：2000 人的频道按 200 人/秒约 10 秒）。窗口里按 scene_id 共享的三样东西都已区分实例：
  - **gate 场景广播**（`gate_push_delivery.cpp` `DeliverToScene`）：RPC 路径是 scene 节点直接调 gate，gate 认得出发送方 scene 节点，只推会话当前指向它的玩家，两份实例的广播不互串；认不出（连接不是已核验身份的当前链路）时退回只按 scene_id。Kafka 路径来自 Go 服务，没有实例之分，按 scene_id 推给两份实例上的全部玩家——这就是「整个场景」广播的语义。
  - **成员集合**：`removeSceneMember` 按 location 保留重叠部分。从旧实例下线的玩家可能残留在旧节点的成员集合里（`removeScenePlayer` 按场景当前属主摘），成员对账（`scene_member_reconcile.go`，逐个活节点核 location）过宽限期后摘掉；旧节点先退出的话由死节点收尾整键删除。只影响负载分，不影响归属与存档。
  - **归属**：频道属主此时已是新节点，新玩家与疏散都只派到新节点。

### 4.4 搬人：疏散请求说清楚从哪来、想去哪

- C++ 疏散票据（`EmergencyRelocateTicket`）在抄会话时一并记下去向提示，规则是纯函数（`relocate_target.h`）：
  - 副本（`dungeon_config_id != 0`）：`scene_id = 0, conf = 0`（默认大世界）；
  - 镜像（`mirror_config_id != 0`）：`scene_id = 0, conf = 本图`；
  - 其它（世界频道）：`scene_id = 原频道, conf = 本图`。
- `EnterSceneRequest` 新增 `bool evacuation = 15`：源节点要把玩家搬走。scene_manager 对它按顺序解析，**一律排除 location 记的源节点**：
  1. `scene_id` 的属主活着、不是源节点、没在排空 → 派过去（同一个频道）；
  2. `conf` 是世界地图 → 同图最空的频道（排除源节点与排空节点上的频道，优先非外出构建）；
  3. 默认大世界最空的频道（同样的排除与优先）；
  4. 都没有 → `ErrNoAvailableNode`，C++ 待确认表按「改派没生效」核实后踢线（现有行为）。
- 整节点疏散（身份冲突）、频道缩容、归属对账走同一套解析，顺带修掉 §2 第 3 条的两个问题。
- 换手门、owner_epoch 铸造、location 写入、路由下发全部是现有跨节点交接的路径（同 scene_id、不同节点不算同落点）。
- **离线托管中的角色**（服务器替它挂着，没有客户端、没有 gate 会话，如组队跟随）同样抄票据，按托管落点发疏散请求：`hosted_placement = true`、不带 gate，身份取断开的那次登录（与组队跟随的托管落点同一契约，`enter_hosted_placement.go`），落点按上面同一套疏散解析，到了新节点接着托管。抄票据要在进退出流程之前（进了退出流程保留就被摘掉，不再认得出托管身份）。这类票据与在线票据一样进待确认表（条目带 `hosted`，2026-11-12 起，取代批 5 的「不进表」）：派发按登录身份判（断线是它的前提，不按 `clientDisconnected` 作废；批 5 那一版派发时去读已被退出入口摘掉的保留组件，每张托管票据都被作废）；核实没生效时不推送、只结清（没有在场的客户端，旧会话号可能已被新登录拿回）；能证明 scene_manager 没放行（没发出去、可重试的拒绝、已过 settle 的结果未知）时等 5 秒 / 20 秒各重发一次。到了目标节点却没建起来时，由目标节点有界重试同一条落点。细节与不变量见 [player-connection-lost-hold.md](player-connection-lost-hold.md) §4.3 / §4.4。

### 4.5 副本、镜像与跨服场景

- 副本不能搬（状态在内存）。计划排空先等：副本结束后玩家自己走、空副本由 scene_manager 回收。到「自疏散时刻」（缺省预算的一半，`SCENE_PLANNED_DRAIN_SELF_EVACUATE_AFTER_SECONDS`）还没走的，按 §4.4 送回默认大世界。想让副本打完，就给副本池更长的宽限期。
- 镜像随源频道迁移被级联拆掉（`cascadeMirrorsOnSourceMigration`），住户回同图频道。
- 跨服主世界与跨服副本分别按主世界、副本处理（同一套角色判定）。
- 自疏散时刻同样兜住「scene_manager 慢或不可用」：节点自己对还有人的场景调 `BeginSceneDrain`，疏散请求按 §4.4 落到同图别的频道；scene_manager 完全不可用时请求失败，到预算后按现有停机路径存盘下线。

### 4.6 新版本优先

- `NodeInfo` 新增 `string build = 16`：这台节点属于哪一代部署。C++ Node 注册时由两样拼成（`ComposeBuildLabel`），选点的一方只比相等、不解析：
  - **构建标识**：部署把与 Pod 标签 `mmorpg.io/build` 同一个值（`Get-ImageBuildLabel`）经环境变量 `MMORPG_BUILD_LABEL` 注入；
  - **工作负载修订**（批 6）：Deployment 的 `pod-template-hash`、StatefulSet 的 `controller-revision-hash`，部署经 downward API 注入 `MMORPG_POD_REVISION`。同一个镜像只改配置或 `kubectl rollout restart` 时构建标识不变、修订会变，拼上它之后，同镜像滚动重启也分得出哪一代 Pod 要留下。
  - **Agones Fleet 的修订**（批 7）：Fleet 的 Pod 没有上面那种标签，部署自己算一个等价的值——Fleet 清单里 GameServer 模板那一段的内容哈希（`Get-SceneFleetTemplateRevision`，形如 `r1a2b3c4d5e`），写进 GameServer 模板的标签 `mmorpg.io/revision` 与 Pod 的 `MMORPG_POD_REVISION`。副本数不在哈希范围内，扩缩容不换代；按行排序、换行归一后再算，行的先后与检出的换行符都不影响结果。`MMORPG_POD_REVISION` 后面再拼上重启令牌（Pod 模板注解 `mmorpg.io/restarted-at`，经 downward API 读，没打过是空串），见下面「整批重启」。
  - 两样都没有 = 空串（本机开发），不参与偏好。
- scene_manager 把「本 zone 里至少有一台排空中节点的构建」视为**外出构建**。选落点时：
  - 迁频道的目标按一致性哈希在**非外出构建的开放节点**里选，没有才退回全部开放节点；
  - 疏散与新玩家选频道时，属主是非外出构建的频道优先；
  - 新副本选节点同理。
- 不需要知道「哪个是新版本」：滚动升级时被排空的永远是被换下的那一代，回滚时也一样（被换下的是新版本，偏好自然落到旧版本）。缩容与单台重启（`kubectl delete pod`）时新旧 Pod 同一代，没有偏好，也不需要。
- **整批重启（模板没有任何变化）**要先让修订变一次，重启出来的才是「新一代」，否则已经重启过的与还没重启的分不出来，玩家可能被搬不止一次。各形态的做法：
  - Deployment（scene、podip 形态的 gate、battle）：`kubectl rollout restart deployment/<名字>`。它给 Pod 模板加一个注解，`pod-template-hash` 随之变化，K8s 按滚动策略逐个替换，旧 Pod 收到 SIGTERM 后按本文排空。不要手工逐个 `kubectl delete pod`。
  - gate 的 StatefulSet（`OnDelete`）：直接用 `k8s_gate_drain.ps1 -DeletePod` 逐台排空并删除。脚本发现被排空的 Pod 已经是 StatefulSet 的当前修订时，会先自动 `kubectl rollout restart statefulset/gate`（只改模板、不重启任何 Pod），重建出来的 Pod 带新的 `controller-revision-hash`；细节见 [gate 排空迁移](gate-drain-session-migration.md) §7.3。
  - scene 的 Agones Fleet：`k8s_scene_roll.ps1 -Restart`。它给 Fleet 的 GameServer 模板与 Pod 模板打重启令牌 `mmorpg.io/restarted-at`（相当于 rollout restart），Agones 自己替换 Ready 的 GameServer，脚本再逐台排空 Allocated 的。这个注解不在 `zone-up` 渲染的清单里，之后再 `zone-up` 不会把它抹掉。
- 滚动期间，外出构建上没人的频道会被 rebalance 顺带迁到新构建（哈希环只含非外出构建），以后排空它们时没有人要搬。
- `NodeInfo.build` 是每种 C++ 节点都自报的通用属性（部署给 gate、scene、battle 都注入 `MMORPG_BUILD_LABEL`）。gate 排空迁移选目标用的是同一个「外出构建」口径：login 优先把连接迁到不在外出构建上的 gate（[gate 排空迁移](gate-drain-session-migration.md) §7.3）。

### 4.7 预算、限速与兜底

| 配置 | 定值 | 依据 |
|---|---|---|
| scene `terminationGracePeriodSeconds` | 120（原 60） | `-SceneTerminationGracePeriodSeconds`，Deployment 与 Fleet 共用。= 预算 95 + 停机 25；单个 scene 进程在线按[容量模型](capacity-model-hok-scale.md) `c_scene` 上沿 11.25k 计，按 200 人/秒约 56 秒搬完，落在预算内并留出余量 |
| `MMORPG_PLANNED_DRAIN_BUDGET_SECONDS` | 宽限期 − 25 | 部署注入，与宽限期出自同一个参数；25 秒 = 停机排空看门狗 15 + gRPC 两段 GOAWAY 约 3 + 拆除余量。没注入时（本机直接起进程）用进程缺省 35 = 原 60 秒宽限期 − 25 |
| `SCENE_PLANNED_DRAIN_SELF_EVACUATE_AFTER_SECONDS` | 预算的一半 | 前一半留给正常路径（scene_manager 先迁频道、再按频道疏散，人落回同一个频道）；后一半给兜底（节点自己逐场景疏散，同图另选频道）。进程缺省，可覆盖 |
| `SCENE_PLANNED_DRAIN_RELOCATIONS_PER_SECOND` | 200 | 每台排空节点每秒最多发起多少次疏散；0 = 不限。每次疏散 = 一次存盘落地 + 一次 EnterScene + 新节点一次载入，限的是后端突发；滚动时同时排空的台数受 `maxSurge` 25% 约束，后端总压力 = 200 × 同时排空台数 |
| battle `terminationGracePeriodSeconds` | 330 | `-BattleTerminationGracePeriodSeconds`，三种 battle 编排共用。= 刚开的一局在缺省模板下的成局期限 300 秒（备战 120 + 战斗 180，`catalog.DefaultBattleSeconds`）+ 停机 25 + 余量 5；预算 305，等在打的局打完再停。模板把战斗时长配长了（上限 `MaxBattleSeconds` 3600）就跟着调大；到预算没打完的局照旧走停机收尾、match 过权威截止判 ABORT。Agones 形态的排空标签不受宽限期限制，房间归零才停 |

- 限速只在计划排空期间生效（频道缩容、身份冲突疏散保持原样）：排空把住户放进队列，100ms 一拍按令牌取出发起疏散，出队时按玩家当时的场景重新判定去向。
- 提前结束：本 zone 里连续 3 秒看不到任何开放的、能托主世界的同类节点（整 zone 停机、全部都在排空），疏散无处可去，直接进停机。
- 到预算仍有人：进现有停机路径（`kNodeShutdown` 存盘下线），打 ERROR 并计数。

### 4.8 观测

- C++：`scene_planned_drain` 日志每秒一行进度（剩余玩家、队列、在途改派、待确认）；开始、完成、到期各一行。复用 `relocate_confirm_stats` 的票据与结局计数。
- scene_manager：`scene_manager_evacuation_resolve_total{zone,result}`，result 取 `same_channel / same_map / default_world / none`；迁移计数沿用 `world_rebalance` 指标，reason 多一个 `node_draining`。
- 不加 player_id 标签。

## 5. 数据一致性

- 不新增提交点：疏散沿用整节点疏散的链路——存盘落地之后才写交接标记（owner_epoch 条件写）、才发 `EnterScene`；scene_manager 凭标记过换手门、铸 epoch、CAS 写 location；新节点载入前按 A2′ 核对并清继承标记。新节点读到的一定是落地后的存档。
- 频道归属改写走现有 CAS 与放置代次：晚到的旧建场景、旧拆场景都被节点按代次拒掉。
- 老节点的那份实例在住户走光前不销毁；它此刻不再是频道属主，新玩家只会被派到新节点。
- 疏散失败（被拒、没发出去、落回本节点后载入被放弃）由待确认表核实后踢线，玩家重登，数据不受影响。
- 节点在排空中途崩溃：与今天的崩溃一样，走失租、再入屏障与死节点接管。
- 外出构建、admission 都只是选点提示，不参与任何提交判定。

## 6. 部署与发布顺序

- **Deployment（缺省）**：`zone-up` 换镜像即可。K8s 先起新 Pod、就绪后给旧 Pod 发 SIGTERM，旧 Pod 按本文排空。缩容（减副本）同理。部署脚本给 scene 注入 `MMORPG_BUILD_LABEL` 与 `MMORPG_PLANNED_DRAIN_BUDGET_SECONDS`，宽限期缺省改为 120 秒。
- **Agones**：Fleet 模板更新只替换 Ready 的 GameServer。旧一代的 Allocated GameServer 要逐台打 `mmorpg.io/drain` 标签，等它排空并自己退出、Fleet 按当前模板补出新的之后再打下一台。「旧一代」= 构建标识、模板修订、重启令牌三样里任何一样与 Fleet 的 GameServer 模板不同（§4.6），所以只改配置的换版和 `-Restart` 的整批重启同样会被滚掉：`zone-up` 换完 Fleet 模板后对每个 scene Fleet 跑 `pwsh -File tools/scripts/k8s_scene_roll.ps1 -Namespace <zone ns> -Fleet <fleet>`（缺省每次 1 台，`-MaxConcurrent` 可调，`-DryRun` 只列名字）；`zone-up` 在 Agones 形态结尾把每个 Fleet 的这条命令打出来。不放进 `zone-up` 自动跑：它等一台疏散完才打下一台，耗时与台数成正比。Fleet 缩容时 Agones 的 allocationOverflow 打的是同一个标签，scene 同样按计划排空处理。
- **顺序**：scene_manager 先于 scene 升级（它要先认得 CLOSED、`build` 与 `evacuation`），与发布轨道的「保护先随 stable 上线」同一纪律。首次上线整批带上即可。

## 7. 契约

### 7.1 协议

| 字段 | 类型 | 语义 |
|---|---|---|
| `NodeInfo.admission = 14`（已有，扩用） | enum | 原只由 battle 填。scene 在计划排空开始时写 CLOSED，平时留 UNSPECIFIED（scene_manager 按开放处理） |
| `NodeInfo.build = 16`（新增） | string | 这台节点属于哪一代部署：`mmorpg.io/build` 标签值，Deployment / StatefulSet 形态再拼上 `@<工作负载修订>`；不透明，只比相等；空串 = 没上报 |
| `EnterSceneRequest.evacuation = 15`（新增，scene_manager 服务内部） | bool | 源节点疏散；解析按 §4.4，排除 location 记的源节点 |

客户端协议不变。

### 7.2 代码

- C++ core：`Node::SetPlannedDrain` / `RequestPlannedStop`、信号拆分（`node_entry.h`）、`node_admission_advertise.h`（从 battle 提出的通用对账）、注册时写 `build`。
- C++ scene：`scene_planned_drain.{h,cpp}`（排空编排：关准入、发布 CLOSED、自疏散时刻、提前结束判定）；`PlayerLifecycleSystem` 的疏散去向、限速队列与 `IsPlannedDrainSettled`；`relocate_target.h`（去向纯函数）。
- scene_manager：`node_drain.go`（排空与构建镜像、外出构建、排空迁移协程）、`load_reporter.go`（解析与触发）、`node_selection.go`、`world_rebalance.go`、`world_init.go`（选频道分层）、`enterscenelogic.go`（疏散解析）、metrics。
- C++ gate：`gate_push_delivery` 的场景广播多一个发送方 scene 节点参数，RPC 入口认出发送方后按它过滤（§4.3）。
- 部署：`k8s_deploy.ps1`（scene 与 battle 的宽限期、`MMORPG_BUILD_LABEL` 与预算注入）、`k8s_scene_roll.ps1`（Agones 旧构建逐台排空）。

## 8. 不做的事

- 不做场景内存状态的跨进程迁移（怪物、施法、副本进度）。
- 不在 gate 上做连接迁移：gate 的计划下线是 [gate 排空迁移](gate-drain-session-migration.md)。
- 不改 GM 停服与整服维护流程。
- 不引入「目标版本」配置：外出构建由排空状态推出来（§4.6）。

## 9. 实施分批与验证

| 批 | 内容 | 主要文件 | 状态 |
|---|---|---|---|
| 1 | 协议字段；scene_manager：排空与构建镜像、选点排除与偏好、排空迁移协程、疏散解析、指标、单测 | `proto/common/base/common.proto`、`proto/scene_manager/scene_manager_service.proto`、`go/scene_manager/internal/logic/{node_drain,enter_evacuation,load_reporter,node_selection,world_rebalance,world_init,enterscenelogic,safe_points}.go`、`node_drain_test.go`、`internal/metrics/{planned_drain,metrics}.go` | 11-11 落码，未编译 |
| 2 | C++：Node 计划排空阶段与信号拆分、注册写 build、通用 admission 对账（battle 改用）；scene 排空编排、疏散去向与限速、单测 | `cpp/libs/engine/core/node/system/node/{node.h,node.cpp,node_entry.h,node_planned_drain.h,node_admission_advertise.h,node_admission_publisher.h}`、`core.vcxproj{,.filters}`、`cpp/nodes/scene/{main.cpp,scene_planned_drain.h,scene_planned_drain.cpp,scene.vcxproj{,.filters}}`、`cpp/libs/services/scene/player/system/{player_lifecycle.h,player_lifecycle.cpp,relocate_target.h}`、`cpp/libs/services/scene/world/scene_planned_drain_rules.h`、`cpp/nodes/battle/{battle_admission_advertise.h,main.cpp}`、`cpp/tests/cross_zone_test/{planned_drain_test.cpp,cross_zone_test.vcxproj{,.filters}}` | 11-11 落码，未编译 |
| 3 | 部署：scene 宽限期 120、两种编排注入 `MMORPG_BUILD_LABEL` 与预算；契约测试 | `tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/scene_planned_drain_deploy.tests.ps1` | 11-11 落码，未运行 |
| 4 | Agones 旧构建 GameServer 逐台排空脚本；battle 计划排空（关准入 → 等房间清零 → 普通停机） | `tools/scripts/k8s_scene_roll.ps1`、`tools/scripts/tests/k8s_scene_roll.tests.ps1`、`cpp/nodes/battle/main.cpp` | 11-11 落码，未编译；battle 进程缺省关闭，部署侧在批 5 接上 |
| 5 | 第一轮残余全部修掉：gate 场景广播按发送节点过滤（§4.3）；离线托管角色按托管落点疏散（§4.4；这一项 2026-11-12 由托管搬迁会话重写为进待确认表、按登录身份派发、有界重发，批 5 原写法派发时恒作废，见 player-connection-lost-hold.md §4.3）；battle 三种编排的宽限期 330 与预算注入（§4.7）；`zone-up` 在 Agones 形态打出逐台排空命令（§6）；拿 broker ACK 当提交点的 Kafka 写入器改 acks=all（见下） | `cpp/nodes/gate/handler/{gate_push_delivery.h,gate_push_delivery.cpp,rpc/gate_service_handler.cpp}`、`cpp/nodes/gate/tests/gate_push_delivery_test.cpp`、`cpp/libs/services/scene/player/system/player_lifecycle.{h,cpp}`、`cpp/nodes/battle/main.cpp`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/scene_planned_drain_deploy.tests.ps1`、`go/scene_manager/internal/svc/servicecontext.go`、`go/player_locator/internal/svc/servicecontext.go` | 11-11 落码，未编译、未运行 |
| 6 | 其余节点的收尾（2026-11-12）：gate 排空迁移默认开、选目标优先新构建、客户端进场后跳数清零（[gate 排空迁移](gate-drain-session-migration.md) §19）；gate 与 battle 注入构建标识；`NodeInfo.build` 拼上工作负载修订，同镜像滚动重启也分得出新旧（§4.6）；Go 服务停机时限盖住在途请求、data-service 宽限期对齐（见下）；login goctl 包装的守卫用例 | `cpp/libs/engine/core/node/system/node/node_planned_drain.h`、`cpp/tests/cross_zone_test/planned_drain_test.cpp`；客户端 `Assets/Scripts/Game/GameClient.cs`、`Assets/Tests/EditMode/Net/GameClientReconnectTests.cs`；`go/login/internal/{config,logic/pkg/loginqueue,svc}`、`go/login/goctl_wrappers_test.go`、`go/login/etc/login.yaml`；`go/shared/shutdownbudget/`、`go/{scene_manager,player_locator,client_rpc_router}` 的入口文件；`deploy/k8s/manifests/go-svc/data-service.yaml`；`tools/scripts/{k8s_deploy.ps1,k8s_gate_drain.ps1,lib/k8s_client_entry.ps1}` 与对应测试 | 11-12 落码，未编译、未运行 |
| 7 | 新旧分代补到底（2026-11-12 晚）：scene Fleet 的模板修订（内容哈希）与重启令牌；`k8s_scene_roll.ps1` 按「构建标识 + 模板修订 + 重启令牌」认旧一代，新增 `-Restart`；模板不变的整批重启各形态的做法（§4.6）；演练清单（§11） | `tools/scripts/{k8s_deploy.ps1,k8s_scene_roll.ps1}`、`tools/scripts/tests/{scene_planned_drain_deploy.tests.ps1,k8s_scene_roll.tests.ps1}` | 11-12 落码，未运行 |
| 8 | Go 服务的两处交接空窗（2026-11-13）：选主让位后跟随者的竞选间隔与锁 TTL 脱钩（见下「选主交接」）；login 停机时等已受理的 EnterGame 后台链、guild 等自己的有序收尾（见下表） | `go/shared/leader/{leader.go,leader_test.go}`、`go/login/internal/logic/pkg/loginqueue/{dispatcher.go,dispatcher_campaign_test.go}`、`go/scene_manager/internal/logic/{world_channel_directory.go,world_channel_directory_test.go}`、`go/scene_manager/internal/config/config.go`（注释）、`go/shared/shutdownbudget/`、`go/login/{login.go,internal/logic/clientplayerlogin/entergamelogic.go}`、`go/guild/guild.go`、`deploy/k8s/manifests/go-svc/login.yaml`（注释）、`docs/design/world-channel-switch.md` | 11-13 落码，未编译 |

验证命令与通过标准在 PROGRESS 的落码条目里（AGENTS.md §10.1：协议生成、编译、单测、冒烟由 Codex 执行）。

**Go 服务停机时限**（批 6）：Go 服务没有玩家长连接要搬，计划重启的标准做法是「先从 etcd 注销、再等在途请求结束、最后退出」，go-zero 自带这三步，但缺省从收到 SIGTERM 起 5.5 秒就强杀进程，在途请求只有 4.5 秒。逐个服务核对「进程自己的停机时限」与「服务端请求超时」「Pod 宽限期」：

| 服务 | 停机方式 | 进程硬截止 | 核对结果 |
|---|---|---|---|
| friend、trade、chat、player_inbox | 自己接管信号（`internal/lifecycle`） | 24 秒 | 宽限期 30 ≥ preStop 5 + 24，已对齐 |
| mail、rank | `shared/domainhost` | 24 秒 | 宽限期 35，已对齐 |
| db | 自己接管信号，等在途存盘 | 按 `MethodTimeouts` 12 秒 | 宽限期 45，已有契约测试。存档已改为直接提交（[存盘直接提交](player-save-direct-commit.md)），db 不再是 Kafka 消费组成员，滚动时没有再平衡停顿 |
| match | 自己接管信号（`shutdownDrainTimeout` 5 秒） | 约 5 秒 | 请求超时 4 秒，盖得住，不改 |
| login | go-zero，外加 main 末尾等已受理的 EnterGame 后台链 | 批 8 起 21 秒 | EnterGame 应答只表示「已受理」，后台链（写会话、绑 gate、进场）应答之后才跑完。原 5.5 秒强杀时，跑到一半的链被砍断，玩家既收不到进场也收不到失败提示，要等客户端 60 秒超时再重新登录。批 8 按 `EnterGameShutdownDrain` 15 秒放宽（wrap-up 1 + 请求 4 + 15 + 余量 1），宽限期 30 ≥ preStop 5 + 21 |
| guild | go-zero，外加 main 末尾的 `StopAndReport`（关入口、中继排空、结果消费者提交位点、号段收尾） | 批 8 起 24 秒 | 原 5.5 秒常常做不完这段有序收尾，位点没提交的消费者重启后重投一遍（幂等兜住，但白做一轮）。批 8 按 18 秒放宽到 24 秒，与上面几个服务的硬截止同一口径，宽限期 35 |
| scene_manager、player_locator、client_rpc_router | go-zero 缺省 | 5.5 秒 | 请求超时 8 / 10 / 6 秒 **超过** 4.5 秒：计划重启会砍掉还在限时内的请求（包括排空中的 scene 发来的疏散请求）。批 6 用 `shared/shutdownbudget` 把强退时限放宽到「wrap-up + 最长请求超时 + 1 秒」（10 / 12 / 8 秒），仍在 30 秒宽限期内 |
| data_service | 自己接管信号（go-zero 的强退不生效） | 60 秒（`shutdownRuntime`） | 宽限期原为 30：排空超过 25 秒会被 K8s SIGKILL。批 6 改为 70（preStop 5 + 60 + 余量 5） |

规则收在一处（`shared/shutdownbudget`）：强退时限 ≥ wrap-up + 最长服务端超时 + 后台收尾 + 余量；Pod 宽限期 ≥ preStop + 强退时限 + 余量。只放宽、不收紧。被强杀的请求等同于进程崩溃时的在途请求，数据由各业务的幂等与围栏兜住，这两批改的是「计划重启不比崩溃更糟」。

**选主交接**（批 8）：选主任务（scene_manager 的排空迁移、死节点接管、世界频道铺设，login 的排队放行，guild / trade / player_inbox / player_locator 的维护循环）在现任让位到新领导者当选之间全部停摆。现任停机时会主动释放锁（`Elector.Stop`、login 放行者的 `Release`），但跟随者两次竞选之间的间隔原来跟着锁 TTL 走（TTL / 3，缺省 30 秒的 TTL 是 10 秒），所以每次滚动重启都会白等最多 10 秒。批 8 把两者分开，取 client-go leaderelection 的口径：

- 心跳间隔仍是 TTL / 3（TTL 内留 2~3 次续期机会，自我降级门槛按它算，锁的安全性不变）；
- 竞选间隔缺省 2 秒（`leader.DefaultRetryPeriod`，`leader.CampaignInterval`），不超过心跳间隔。现任让位后最迟 2 秒有新领导者；现任崩溃来不及放锁时仍要等锁过期，再加最多 2 秒；
- 代价只是跟随者每 2 秒一次 SETNX（每个锁一个跟随者），可以忽略；
- login 排队放行者有自己的竞选循环（`loginqueue.Dispatcher`），同一口径（`campaignRetry`）；
- 按竞选间隔推算的地方同批改：scene_manager 分线目录的 TTL 是「3 × 发布周期 + 一个竞选间隔」，缺省值由 16 秒变成 8 秒，改为直接调 `leader.CampaignInterval`，不再自己写 TTL / 3。

**Kafka 滚动重启**（批 5）：Kafka broker 本身按副本 3、`min.insync.replicas` 2 部署，滚动重启时分区 leader 切到同步副本，消费方按位点续读。剩下的风险在生产方：acks=1 时 leader 写进自己的日志就回 ACK，切 leader 那一刻还没复制的消息会被新 leader 截掉，而生产方已经当它送达。两个把 ACK 当提交点的写入器因此改 acks=all——scene_manager 的路由 / 重定向命令（写完就当 gate 会照做）、player_locator 的清理命令（写完就结清 claim、不再重试）。guild / friend / mail / match / player_inbox / chat 发的是提示，丢了有拉取或 SQL 后台循环兜底，保持 acks=1（[断线重连核对](infra-reconnect-overview.md) R19）。

**已知残余**：没有本方案范围内的残余。范围之外、按设计不做的见 §8；Java 版见 §10。

## 10. Java 版

待做。Java 版（0.1.0-SNAPSHOT）按自己的 Netty、Dubbo 与部署形态实现同一套规则：SIGTERM 先摘流再疏散、疏散请求带去向且排除源节点、外出构建优先级、预算到期退回存盘下线。已在 Java `PARITY.md` 的全类型平滑停机条目下登记待做。

## 11. 演练清单（尚未执行）

按 [AGENTS.md](../../AGENTS.md) §10.1 交给 Codex 在 kind 或 dev 集群执行，不在生产做。故障注入类的演练（断网、杀进程、切主）在 [断线重连核对](infra-reconnect-overview.md) §5，这里只列**计划内**的重启、缩容与换版。

**环境**：一个 zone；scene（Deployment 形态）、gate、login、scene_manager、player_locator、db 各至少 2 个副本；robot 保持若干账号在线并持续做移动、聊天、背包操作。每条都记录各组件的版本与副本数。

**每条都要满足**：演练期间 robot 没有掉回登录；没有 `Planned drain budget expired` 之类到预算强停的 ERROR；演练结束后抽查角色存档与演练前一致或更新（不回档）。

| # | 场景 | 步骤 | 应观察到 |
|---|---|---|---|
| 1 | scene 换版（Deployment） | `zone-up` 换镜像；或镜像不变时 `kubectl -n <zone ns> rollout restart deployment/<scene 池>` | 旧 Pod 日志 `[PlannedDrain] scene drain started` 到 `Planned drain complete; shutting down.` 在预算内；scene_manager 日志 `[Drain] ... migrating its world channels`；`scene_manager_evacuation_resolve_total{result="same_channel"}` 增长、`result="none"` 为 0；robot 收到同一个 scene_id 的进场、坐标不变；同一个 robot 整轮只被搬一次（落点优先新一代 Pod） |
| 2 | scene 缩容 | `kubectl -n <zone ns> scale deployment/<scene 池> --replicas=<少一台>` | 被缩掉的那台按第 1 条排空，上面的玩家落到其余节点的同一个频道 |
| 3 | scene 换版（Agones） | `zone-up` 换 Fleet 模板后跑 `k8s_scene_roll.ps1 -Namespace <ns> -Fleet <fleet>`；再做一次只改配置的换版；再做一次 `-Restart` | 三次都逐台打标签、等上一台消失才打下一台，结束时输出 `done: every game server ... is on the current generation`；新 Pod 里 `printenv MMORPG_POD_REVISION` 形如 `r<10 位十六进制>.`，`-Restart` 之后点号后面是时间戳；玩家体验同第 1 条 |
| 4 | gate 逐台换版 | 起两台以上 gate，用 `k8s_gate_drain.ps1 -DeletePod` 逐台排空并删除；换镜像做一轮，模板不变再做一轮（第一台时脚本应打出 `[gate-drain] pod is already at the current revision; bumped the StatefulSet revision`） | login 日志 `[GateDrain] gate N: migration lease sent`；robot 日志 `redirect complete`；scene 日志 `[ConnectionLostHold] resumed by a new session`；`gate:{gm:<zone>}:<id>:drained` 的理由是 `below_threshold`；同一个账号连续被迁 4 次以上不出现 `hop limit reached`；从第二台起连接只迁到已经换好的 gate |
| 5 | battle 停机 | 有一局在打时 `kubectl -n <ns> delete pod <battle Pod>` | 日志 `[PlannedDrain] battle admission closed; waiting for live rooms to finish`；这一局正常结算，不是 ABORT；删除期间新开的局落到别的 battle |
| 6 | Go 服务逐个滚动 | 对 login、scene-manager、player-locator、client-rpc-router、db、data-service、guild、friend、mail、rank、trade、chat、player-inbox、match 依次 `kubectl -n <ns> rollout restart deployment/<名字>`（StatefulSet 形态的用 `statefulset/<名字>`），每个等 `kubectl rollout status` 完成再做下一个 | robot 的操作没有失败，或只出现一次可重试的错误后自愈；被重启的服务日志里没有 go-zero 的 `going to force kill the process`；scene-manager、player-locator、client-rpc-router 起服日志有 `[shutdown] force-quit wait time raised`；重启 scene-manager 的领导者副本时，另一个副本在一个竞选周期内接任，正在排空的 scene 继续被迁频道；data-service 的 Pod 在 70 秒内退出，日志里没有 `HOLD` |
| 7 | etcd 成员滚动 | 按 [etcd 运维手册](../ops/etcd-maintenance-runbook.md) 逐个重启 3 个成员 | 没有节点被误判下线，没有场景被接管，gate 与 scene 的节点列表不抖动 |
| 8 | Kafka broker 滚动 | 按 [Kafka 生产手册](../ops/kafka-cluster-production-runbook.md) 逐个重启 broker，期间让 robot 持续登录、切场景 | 进场不卡（scene_manager 的路由命令 acks=all，切 leader 时不丢）；消费延迟在 broker 回来后回落 |
| 9 | TiDB 滚动重启 | 外部集群按 TiDB 自己的滚动升级流程；期间让 robot 持续存盘、做帮会与交易写 | 没有成批失败，`authoritysql` 的 Hold 计数不涨（同 [断线重连核对](infra-reconnect-overview.md) R2） |
| 10 | Redis 切主 | 核心 Redis 做一次 Sentinel 手动切主；玩家域集群按 [玩家域集群手册](../ops/redis-player-cluster-runbook.md) 先切主再删 Pod | 记录无领导窗口与双领导窗口的时长（R7、R8）；在线玩家不掉线，登录在切主完成后恢复 |

第 1~6 条对应本文与 [gate 排空迁移](gate-drain-session-migration.md) 的改动，是这两份设计的验收；第 7~10 条是基础设施自己的滚动，仓库里没有对应的新代码，只需要记录观察结果。
