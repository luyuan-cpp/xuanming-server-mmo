# 全仓数据问题与覆盖回档审计·第二轮（2026-10-19）

> 只读静态审计，**未编译、未运行**。对象是 2026-10-19 的工作区，包括其他会话尚未提交的改动。
> 本文只登记问题和修复方向，不含代码修改。修复要按 AGENTS.md §10.1 交 Codex 验证，并按 §12 同步 Java 版。
> 第一轮见 [data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md)。本轮按该文 §8 第 3 步执行。
> **状态：进行中。** 第一波（增量对抗评审 4 路）、第二波（新落码增量评审 6 路，其中 4 路因周额度停线提前收尾）已完成。2026-10-20～22 换账号续做：第三波两批共 10 路已完成（§3.6、§3.7），第四波的第一轮 low 复核已完成（§3.8）。2026-10-23～24 负责人要求把发现修完：第三波 6 条 medium 已分给各会话修，本会话修了 r2-lc#1/#2 与 r2-rr#5 数据侧，每条都经独立评审与修一轮，状态见 §5。**换视角查找尚未跑**：周额度已过 80%，按会话组约定推迟到 2026-10-29 20:00 UTC 重置之后，见 §6。

## 0. 前置检查（§8 第 1、2 步）

| 步骤 | 状态（截至 2026-10-19） | 依据 |
|---|---|---|
| 第 1 步：验证已落的修复 | **未完成。** §2.1：协议生成、6 个 Go 服务编译、C++ 增量构建已过；player_locator 测试、`routing_identity_test`、`cross_zone_test` 运行和真实环境故障演练没做；`EnterFenceEcs.LoadCallbackBindsNewestAdmitted` 夹具失败等负责人决定。§2.2：7 步验证一步都没跑。§2.3：login 两个包 249 例通过，没跑真实多 gate 环境 | 第一轮 §7；PROGRESS 2026-10-19 各 Codex 条目 |
| 第 2 步：修剩余 medium | 第一波时**未开始**。第二波时：§2.4 三处已落码（随 `53361d055` 提交，未编译、未测试），第二波按"可能有缺陷"审了；§2.5 仍未动手 | 第一轮 §7；PROGRESS「审计 §2.4 战斗结算三处：丢数据修复」 |
| 字段保护设计 | 第一波时**未落码**。第二波时：第 1 批（台账、生成器、冻结 option）与第 2 批 go/db 部分随 `53361d055` 提交，设计升到 v0.4；C++ 携带的三个头文件（`unknown_field_carry.h`、`player_persist_carry_comp.h`、`player_persist_marshal.h`）还是工作区未跟踪文件，HEAD 上的 `player_database_loader.cpp` 不含携带逻辑。第二波审了已落部分，见 §3.3 | [mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md) §9；工作区观察 |

由于前置没完成，(a) 增量评审把 §2.1、§2.2、§2.3 的修复都当作"可能有缺陷"来查。

## 1. 结论

**截至第二波：** 新增 3 条 medium、22 条 low，没有 high。3 条 medium 在缺省配置下都不触发：两条在缺省关闭的功能里（全球匹配、合服 copy 模式），一条只在上线后的滚动发布混版窗口出现。前两条主会话抽查了代码路径，都属实；三条的对抗复核都排在第四波。

**第一波：**

- 第一波 4 路增量对抗评审，覆盖 §2.1 修复的 Go 侧和 C++/gate 侧、go/db 世代门禁与补投、data_service 读路由与调试导入。**没有新的 high 或 medium**，新增 **9 条 low**。
- 其中 3 条是第一轮修复本身的缺陷：
  - §2.1 的 R5 在"LeaveGame 先于本次登录的 Bind 到达 gate"时不成立；
  - §2.2 的补投把发布失败也计入次数，Kafka 故障约 21 分钟后意图停在 exhausted，不会自愈；
  - §2.1 的 LeaveScene 把"负载表里查不到持有节点"当作释放证据。
- 第一轮的 2 个重点点名项复核成立、无新缺陷：
  - data_service `LoadPlayerData` 字段与版本同读；
  - data-service#1（debug_import 的 uint64 精度）的修复。
- 600s 兜底强删会退回 §2.1 原问题，这一点设计已登记为有意 fail-open（[location-release-binding.md](location-release-binding.md):173-181），本轮不重报。L2、L3 是让它比设计预期更早、更常触发的两个新入口。

**第二波**（第一波之后又合入 7 个提交，含字段保护、§2.4 修复、路由自动对账，以及从别的分支合入的神秘商人、邮件清扫、组队匹配）：

- 6 路：字段保护、§2.4 战斗结算修复、神秘商人、路由自动对账、合服工具、login。新增 3 条 medium、13 条 low。
- **r2-ps#1（medium，仅混版窗口）**：字段保护的台账在 bootstrap 期间把新增持久字段一律记为世代 1。上线时若没把台账切到 active，之后第一次带新持久列的滚动发布里，旧 go/db 会把新列静默丢掉或读成旧值，C1 / C1b / C2 都拦不住。查找员的两份输出对严重度有分歧（medium / low），待复核裁决。修复只需上线前切 active 并写进发布清单。
- **r2-battle#1（medium，§2.4 修复缺陷）**：全球对局个人结算的落库重试轮会无条件重写全部成员的 pending。已被 scene 应用并销账的成员，pending 会复活，下次登录重复发奖。门控在 `GlobalMatch.Enabled`（缺省 false）。
- **r2-mz#1（medium）**：合服 copy 模式在拷行后删掉 `save_barrier`、保留 owner_epoch，以前登录过的被拷玩家全部无法登录，直到人工跑屏障恢复；合服手册里没有这一步。fail-closed，不丢数据。门控在 `-player-rows-mode copy`（缺省 pin，但多 data Redis 集群部署只能用 copy）。
- §2.4 的另外两处修复（cpp-economy#0 号段预检、cpp-economy#1 区分封禁与冻结）推翻不成，只发现 1 条预检不催续段的 low。
- 字段保护在缺省配置下 go/db 侧实际不生效：产物世代为 1、台账在 bootstrap，C1/C1b/C2 都走不到，C0 要等人工写引导关闭标记。C++ 携带还只在工作区未跟踪文件里；只提交已跟踪文件会断构建，停在 HEAD 时第一轮 low「存盘不保留未知字段」仍成立。
- 神秘商人：购买三步同线程同步完成、同行同次落盘，没有共享库存，冻结闸与退出闸都覆盖，只有 1 条限购可被绕过的 low（付费购买，不复制资产）。
- login：第一轮 §4 的「账号 blob 有两个写者不走建角锁」在当前代码里已不成立，四个写者同锁加令牌 CAS。loginqueue 与 dataloader 因额度未读。

## 2. 方法与范围

第一轮按子系统分区，每区一名查找员。本轮换成四种查法：

| 查法 | 内容 | 状态 |
|---|---|---|
| (a) 增量对抗评审 | 基线 `bf3c60632` 到 HEAD，加工作区里涉及存盘、加载、会话、资产的改动，逐个尝试推翻 | 第一波 4 路、第二波 6 路已完成（第二波有 4 路提前收尾，未读部分见 §6）；C++ 收件箱 / 号段引导 / trade / friend / 邮件清扫 1 路待做 |
| (b) 补覆盖 | 第一轮 §1 的未读段；各业务系统持久组件写入口是否绕过冻结闸或退出闸 | 待做 |
| (c) 复核 | 第一轮 §4 的 23 条未复核 low | 待做 |
| (d) 换视角连续查找 | 按资产追踪、按故障、按入口，连续两轮无新发现才停 | 待做 |

第一波四路的范围：

| 路 | 范围 |
|---|---|
| A1 | go/scene_manager 的 leave_release / leavescenelogic / enter_holder_release / enterscenelogic；go/player_locator 的 markoffline / leasemonitor / session_cas / session_reconciler / gate_session_index |
| A2 | C++ scene 的 release_fence.h、player_lifecycle 改动段、player_scene、scene_manager_response_handler；gate 的 scene_route_helper、scene_entry_dispatch、client_session_handler、client_request_lane、login_admission |
| A3 | go/db 的 saveoutbox（generation / replay / scripts）、kafka（save_outbox_note / route_ready_gate / key_ordered_consumer / task_result_notify）、barrierrestore 与 save_barrier_restore 工具、shared/placement/route_ready；顺带 retry_stranded、durable_enqueue 的主路径 |
| A4（data_service） | data_logic、routing、store/route_reads、route_reconcile、routetool、debug_fetch / debug_import / debugutil |

第二波六路的范围（基线 `8767c5027` 到 HEAD `c63b22dca`，加工作区；合服与 login 从 `bf3c60632` 起）：

| 路 | 范围 | 完成度 |
|---|---|---|
| persist-schema | 字段保护：台账与生成器、go/db 的 persist_schema / player_row_fence / key_ordered_consumer、barrierrestore 的 schema_gen、C++ 携带（工作区未跟踪文件）、debug_import 世代检查、告警 | 提前收尾 |
| battle-settlement | §2.4 三处修复：player_battle、bag_service、result_commit、settlement_outbox、battle_room_manager | 完整 |
| mystery-merchant | 神秘商人：C++ 系统与 handler、proto、scene_manager 的 mystery_merchant.go | 完整 |
| route-reconcile | data_service 路由自动对账 routereconcile（reconciler / cache）、配置接线、shared/placement | 提前收尾 |
| merge-zone | tools/merge_zone（merge_run、preflight、player_rows、unmerge 调用点）、guild merge_fence | 提前收尾，只读完主流程 |
| login | loginsession、sessionfence、账号目录四个写者、kill switch、deprecation | 提前收尾，loginqueue 与 dataloader 未读 |

- **只读约束**：每个代理的提示都写明不建、不改、不删文件，`git show` 不重定向，不跑构建和测试。两波结束后都检查了代理转录：没有 Edit 或 Write 调用，也没有重定向成文件（第二波唯一命中是 `2>&1`）。
- **第二波的两份输出**：周额度触线时，我给 4 路发了"立即收尾"消息。消息是另起一个续跑实例送达的，工作流里原来的实例照样跑完，所以这几路各有一份结构化结果、一份收尾报告，内容互有增减。本文取两份的并集，严重度不一致的地方逐条注明。
- **严重度口径**同第一轮。项目尚未上线，只在混版窗口出现的问题降一级并标注。

## 3. 发现（本审计均未做对抗复核；第一波有 6 条已由修复会话复核并落码，见 §5）

### 3.1 第一波：第一轮修复本身的缺陷

| 编号 | 问题 | 位置 | 触发时序 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|---|
| r2-gate#1 | **§2.1 的 R5 有缝**：LeaveGame 先于本次登录的 Bind 到达 gate 时，重置记下的是旧代次。之后到达的 Bind(gE) 不被过滤并清掉标记；session_ended 释放把 location 删掉后，gate 仍留着旧路由和 `ownerEpoch=E`。同一连接再次 EnterGame 时，gate 按旧路由把旧 epoch 补发到原节点 X，scene_manager 同时铸造 E+1 并落到 Y | 钩子 `cpp/nodes/gate/handler/rpc/client_session_handler.cpp:1144`；重置 `cpp/nodes/gate/handler/event/scene_route_helper.h:153`；判据 `:176`；补发 `:367`、`scene_entry_dispatch.cpp:359`；login 先 Bind 后复查 `go/login/internal/logic/clientplayerlogin/entergamelogic.go:791`、`:813` | ① EnterGame 已受理，Bind(gE) 还在 Kafka；客户端发 LeaveGame，gate 收包即重置，记下 g0。② 后台链已过 `:813` 复查，EnterScene 照常执行；Bind(gE) 到达，gE>g0，转发到 X 并清标记。③ LeaveScene 派发 session_ended，X 存盘、写 A1′、销毁，location 被删。④ 同一连接 EnterGame(gF)，gate 按旧路由带 E 补发到 X，scene_manager 铸 E+1 落到 Y。⑤ X 若先于铸造完成核对，会建出幽灵实体：gate 改绑到 Y 之前，客户端操作落在 X 上，随后被 CAS 拒掉（毫秒级丢失）；幽灵实体长期挂在 AOI 里。若铸造在前，X 回 3023，登录类型被这次补发消耗，Y 收到 0 型进场 | 默认路由模式可达，无开关。改包客户端可稳定复现；普通客户端需要在加载中点"返回"，并恰好落在 Bind 的 Kafka 消费延迟里 | 未登记；[location-release-binding.md](location-release-binding.md):153-158 声称这种情形已覆盖 | 很可能 |
| r2-db#1 | **§2.2 补投把发布失败也计次**：Lua 先把 attempts+1 再交出意图，Publish 失败不回退。go/db 到 Kafka 的生产持续不可用、且实体已销毁时，约 21 分钟后 attempts 到 8，意图进入 exhausted；Kafka 恢复后没有路径重置计数，玩家登录在父提交屏障处报 not committed，无法产生新版本 | `go/db/internal/saveoutbox/scripts.go:90-92`、`:76-79`；`replay.go:288-293`；MaxAttempts 缺省 8（`replay.go:75-77`）；登录卡点 `go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go:42` | ① Kafka 生产不可用。② 期间场景节点崩溃或排空看门狗销毁实体（`player_lifecycle.cpp:3410-3423`），最后版本只剩 outbox 一份。③ 每拍先计数、后 publish_error，约 30s + 10…600s 退避，合计约 21 分钟到上限。④ 恢复后不再投，人工 HDEL 前该玩家无法登录 | 默认 dual 可达；需要"Kafka 故障 ≥ 约 21 分钟"和"实体已销毁"同时成立。数据不丢（仍在父快照和意图里），但故障后会大面积锁登录 | 部分登记：`db-write-fencing-mysql.md:1628-1631` 和告警 `deploy/k8s/db-task-retry-alerts.yaml:487-501` 写了 exhausted 要人工处理，但归因只写了消费侧 | 很可能 |
| r2-sm#3 | **LeaveScene 把"负载表里查不到持有节点"当作释放证据**：`scene_nodes:zone:{z}:load` 键丢失或被淘汰时，ZCARD=0、ZSCORE 为空、没有 death_at，会判 `owner_dead`。之后每次登出或断线清理都会按 any 模式立即删 location，下次进场铸造 E+1，持有节点未落盘的部分回档 | `go/scene_manager/internal/logic/leavescenelogic.go:314`；`enterscenelogic.go:1048-1068`；`reentry_barrier.go:149-152` | ① load 键丢失。② leader 下一拍 ZADD 补回之前，有玩家登出。③ 判 owner_dead，立即删 location。④ 进场铸造 E+1，持有节点未落盘的进度被拒 | 需要 load 键丢失；leader 每拍 ZADD，窗口约一拍。EnterScene 本来就用同一判据，这次修复把它扩到了每次登出都会走的路径 | 未登记（设计 §3.1 证据 2 只写"同判据"） | 很可能 |

### 3.2 第一波：其他新发现

| 编号 | 问题 | 位置 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|
| r2-sm#1 | forced 计数和 ERROR 日志在删除 Lua 的结果出来之前就记了。Lua 返回 -1（GET 之后有并发 EnterScene 改写）或 0（已被别的调用删掉）时没有真正删除，却照样计 forced，触发告警 `SceneManagerLeaveReleaseForced` 误报；released 同样多计 | `go/scene_manager/internal/logic/leavescenelogic.go:241-245`、`:260-268`；告警 `deploy/k8s/scene-manager-alerts.yaml:658` | 默认开启（`LeaveSceneHolderReleaseWaitSeconds` 缺省 600）；需要恰好并发，很少见。只影响观测 | 未登记 | 确定 |
| r2-sm#2 | 600s 等待计时用的是各 scene_manager 副本的墙钟。副本 A 写 first_ms，副本 B 时钟快 Δ 或 NTP 跳变，算出的等待时长就多 Δ，提前 forced；forced 只比对 location 原文，若持有节点仍在重试存盘，就按 §2.1 原路径回档 | `go/scene_manager/internal/logic/leave_release.go:178-192`；`leavescenelogic.go:324` | 默认配置可达，但要持有节点已卡住，且副本间有时钟偏差 | 未登记（设计 §3.2、§3.6 没写时钟偏差语义，AGENTS §11.3） | 确定；影响面小 |
| r2-db#2 | legacy 存活暂停只挡之后的补投。已经发到 Kafka 的旧补投副本 V1，在 dual→legacy 降级后会被 legacy 无条件写入，盖掉 V2 | `go/db/internal/saveoutbox/replay.go:173-182`；`generation.go:69-95`；§8.6 降级步骤 `db-write-fencing-mysql.md:1012-1014` 没要求积压清零 | **仅混版或降级窗口**；缺省 dual（`db.yaml:177`、`config.go:132`） | 未登记；是第一轮 go-db-pipeline#2 的新变种 | 很可能，窗口窄 |
| r2-db#3 | 在途重试戳只取较大值，且用本机时钟。某个 go/db 实例时钟超前 Δ 时，补投会推迟约 Δ+2 分钟且不计次，期间玩家被锁登录；C++ 写新版本时不清这个戳 | `go/db/internal/saveoutbox/scripts.go:134-143`、`:86-89`；`go/db/internal/kafka/save_outbox_note.go:73` | 默认 dual；需要时钟故障加原件丢失 | 未登记 | 代码路径确定，影响取决于偏差幅度 |
| r2-ds#1 | debug_import bundle 模式"目标≠导出来源"的身份闸对 TiDB 等于没有：取不到 `@@server_uuid` 就返回空串，空的一侧直接跳过比较；地址闸只认回环地址。把线上 TiDB port-forward 到 127.0.0.1 并混用配置时，`--dry-run=false` 会把线上两张玩家行 REPLACE 成导出时的旧状态，并把行内围栏抬高，之后在线 scene 的存盘全被判 stale_owner | `go/data_service/cmd/debugutil/connect.go:101-113`、`:153-171`；`cmd/debug_import/bundle_import.go:451-465`、`:381-390`；`go/db/internal/kafka/player_row_fence.go:160-161` | 运维误用才触发；`-dry-run` 缺省 true（`main.go:63`），还需混配 | 未登记；`docs/ops/online-debug-data-fetch.md:57`、`:88` 写"按 server_uuid 拒绝"，没提 TiDB 例外 | 存疑（TiDB 对该变量的实际行为未核实） |
| r2-ds#2 | bundle 导入重铸 epoch 只取 max(线上, 本地 owner)+1，不看本地两张表的行内围栏。本地 Redis 被 FLUSHALL 而 MySQL 和 Kafka 没清时，epoch 更大的残留 DBTask 会冲掉导入的数据，之后本地新登录的存盘被判 stale_owner | `go/data_service/cmd/debug_import/bundle_import.go:381`、`:487-493`（文件头 `:39-41` 的说法在此情形不成立）；`player_row_fence.go:157-173` | 只影响本地调试环境；需要 `-dry-run=false`、`-replace` 和"清了 Redis 没清 MySQL/Kafka" | 未登记 | 很可能 |

**附带观察（非数据问题）**：客户端可以直接发 Disconnect（消息号 58），它在 `IsClientMessageId` 白名单里，和 `cpp/nodes/gate/handler/rpc/login_admission.h` 注释里"gate 合成"的说法不符。影响只限本人掉线，§2.1 修复之后不会回档。

### 3.3 第二波：medium

| 编号 | 问题 | 位置 | 触发时序 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|---|
| r2-battle#1 | **§2.4 social-match-battle#2 修复缺陷：落库重试轮无条件重写全部成员的 pending。** 一轮里只要有一名成员没拿到确认，下一轮就对全部成员再 SET 一遍；已被 scene 应用并销账的成员，pending 复活，下次登录重复发奖 | 每轮遍历全部成员、不记成员级状态：`cpp/nodes/battle/logic/battle_room_manager.cpp:1475-1503`（成功判定 `:1482`，未发出按失败计 `:1501`）；整轮全成功才置 staged：`cpp/libs/services/battle/system/result_commit.h:118-126`；无条件 SET：`cpp/libs/services/battle/settlement/settlement_outbox.h:429-432`；销账删 pending 与锁：`settlement_outbox.h:461-468`，回调摘账本：`cpp/libs/services/scene/battle/system/player_battle.cpp:329-330`；离线暂存锁不在就不动：`player_battle.cpp:528-538`；登录钩子只看账本不看锁：`:2207`→`:2077`→`:1727` | ① 全球确认模式对局收尾，第 1 轮落库给 A、B 各 EVAL。② A 写成功；B 回 ERROR、空应答（连接中途断开）或命令未发出，整轮判失败，退避 1~30s。③ 退避期间 A 的 scene 读到 pending（reaper 在 deadline 后补读、A 重登的登录钩子或 RunLoginRechecks），应用、登记账本、落盘，销账 Lua 删 pending 与锁，账本条目被摘。④ 第 2 轮对全部成员无条件 SET，A 的 pending 复活，此时锁已不在、账本里也没有这一局。⑤ 结果发出并回执，派发给 A：A 离线或退出中时，离线暂存因锁不在什么都不做，pending 留下；或第 2 轮之后、回执之前节点停机，pending 同样留下。⑥ A 下次登录，登录钩子读到 pending、账本无记录，整笔再发一次（金币、掉落、任务进度翻倍）。A 在线时派发路径会丢弃并条件删除，能自愈 | 门控：只有确认模式（CreateBattle 带 `placement_operation_id`，`battle_room_manager.cpp:655`）会触发，该字段只由全球编队填写（`go/match/internal/logic/gather.go:338-341`），`GlobalMatch.Enabled` 缺省 false（`go/match/etc/match_service.yaml:6`）。开通后需"一轮部分失败"加"该成员在退避窗口内被应用并销账"；deadline 强制平局时 reaper 补读与重试大致同时，更容易撞上。加重变种：积压索引键类型被写坏时每轮都部分执行又判失败，落库无次数上限（r2-battle#4），可反复刷奖 | 未登记。第一轮 §2.4「修复」、`docs/design/cross-zone-matchmaking.md` §13.6 更正（:556-576）、`result_commit.h:64-67` 只论证了"回执后不改写" | 很可能。主会话抽查：落库循环与无条件 SET 属实；scene 侧离线暂存早已因同类缺陷改成"锁匹配才写"（`settlement_outbox.h:434-443` 的注释），battle 侧落库没有跟上 |
| r2-mz#1 | **合服 copy 模式删掉提交屏障、保留 owner_epoch，被拷的老玩家整批无法登录，且恢复工具拒收这批人** | 删键清单含 `player:{pid}:save_barrier`：`tools/merge_zone/player_rows.go:702-705`、`:725`；调用点：`tools/merge_zone/merge_run.go:493`（copy 合服）、`tools/merge_zone/unmerge.go:289`（copy 撤销）；owner_epoch 刻意不删：`tools/merge_zone/scene_hot_state.go:50`；登录卡点：`go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go:110-111`（及 `:153-155`）；恢复工具拒收无落点记录者：`go/db/internal/barrierrestore/restore.go:430-438` | ① 以 `-player-rows-mode copy` 合服，P2~P6 通过。② 步骤 1 按列名逐字节拷两表行（带行内围栏），随后删父缓存、子缓存、资产证据与 `save_barrier`，owner_epoch 保留。③ 按 runbook 直接开服，全程没有重建屏障的步骤。④ 曾进过场景（owner_epoch≠0）的被拷玩家登录，`beginParentLoad` 报"历史玩家父快照与提交屏障缺失，需要停写恢复"。⑤ pending 只在 scene 存盘时写，进不了场就不会自愈。⑥ 按设计跑 `save_barrier_restore`：v1 要求显式落点记录，copy 合服者没有，被拒。copy 撤销对全部清单玩家同样删屏障 | 门控：`-player-rows-mode copy`。缺省 pin（不调 `invalidatePlayerCaches`），但 runbook §5.1 要求多 data Redis 集群部署只能用 copy。一用即必现，与故障无关。fail-closed，不丢资产 | 部分登记：`db-write-fencing-mysql.md:1562`、`shared-redis-split-by-purpose.md:136` 写了"合服 / 撤销必须删屏障、之后重建才能放流、当前无自动补种"；但合服 runbook 全文没有屏障重建步骤（grep 无结果），恢复工具拒收这批人也未登记 | 很可能。主会话抽查：删键清单、调用点、登录报错分支、runbook 无相关步骤均属实 |
| r2-ps#1 | **字段保护台账在 bootstrap 期把新增持久字段一律记为世代 1。** 上线后若台账仍是 bootstrap，滚动发布时旧 go/db 会把新列静默丢掉或读成旧值，C1 / C1b / C2 只比世代号，拦不住 | `tools/proto_generator/protogen/internal/persistschema/apply.go:43-46`；`proto/persist_schema_gen.txt:7`（state=bootstrap）、`:212`（mystery_merchant 已落在世代 1）；`go/db/internal/kafka/persist_schema.go:173`（C1）、`:186`（C1b）、`:195`（C2）；`key_ordered_consumer.go:515-519`；`player_row_fence.go:188`；`deploy/k8s/README.md:2396`（"最后由负责人改为 active"） | ① 版本 R2 新增顶层持久列 F（例如限购或幂等账本），生成器仍记世代 1。② 滚动发布，R1 go/db（不认识 F）与 R2 并存。③ R2 scene 存盘戳为 1，落在 R1 go/db：C1 不拦，`tx.Save` 只写认识的列，F 停在旧值，照常 ACK。④ 或冷加载落到 R1：只 SELECT 认识的列，C2 不拦，父快照缺 F。⑤ scene 加载后再存盘，F 被写成零值或旧值。F 若是限购或幂等账本，就会重复发放 | 仅混版窗口。项目未上线，首次上线整批起步不触发；上线后第一次带新持久列的滚动发布即可触发，前提是台账没切 active。没有任何机器门禁把台账状态与上线绑定，`docs/ops/release-checklist.md` 没有这一步 | 部分登记：`mixed-version-persist-field-protection.md:402`（§8.1 第 2 步）、`:606`（v0.4）写了发号规则；没写"同为世代 1 的两个二进制字段集合可以不同"，也没写上线前要切 active | 很可能。严重度两份输出不一致：结构化结果按"默认滚动发布可达的回档或丢资产，混版降一级"定 medium；收尾报告定 low |

### 3.4 第二波：low

| 编号 | 问题 | 位置 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|
| r2-battle#2 | §2.4 修复缺陷：落库进行到一半时停机或崩溃，同一局只有部分成员的 pending 落地，结果永不发出。A 拿到奖励，B 的消耗与 HP 不结算，评分与帮会历练都不计 | `cpp/nodes/battle/logic/battle_room_manager.cpp:1707`（AbortAllRooms 先停落库再清房）、`:1475`；`result_commit.h:127`（未 staged 不发）；`docs/design/cross-zone-matchmaking.md:569` | `GlobalMatch.Enabled` 门控；需一轮部分失败后在退避窗口内停机或崩溃 | 部分登记：:569 接受"奖励有、评分无"，没写同局成员不对称 | 确定 |
| r2-battle#3 | §2.4 cpp-economy#0 修复缺陷：号段预检只读 `Available()`，不触发续段。"所需新实例数 > 当前剩余 > 预取阈值、且没有下一段"时，结算一直延后，最终可能被下一局覆盖单槽 pending | `cpp/libs/services/scene/battle/system/player_battle.cpp:774`、`:1764`；`cpp/libs/modules/id_segment/guid_segment_client.cpp:155`、`:259`；`guid_segment_client.h:88`；`bin/etc/base_deploy_config.yaml:213` | 缺省 item 号段已启用；step 缩到下限 1000 时阈值为 100。现有掉落表每只怪最多 2 个掉落槽，几乎不可达，取决于配表。资产通道 `ApplyCredit` 预检同根因（`asset_op_system.cpp:675-693`，AssetOp 门控） | 未登记 | 很可能 |
| r2-battle#4 | 个人结算落库轮次没有上限：持续 ERROR（OOM、NOAUTH/ACL、键类型错）时房间永久滞留、结果永不发出、节点无法排空，并放大 r2-battle#1 | `result_commit.h:118-126`（失败只用于算退避）；`battle_room_manager.cpp:1420-1424`、`:1453-1455` | `GlobalMatch.Enabled` 门控 | 未登记（§13.6 只写"Redis 不可用时等恢复"）；违反 AGENTS §11.3 重试须有上限 | 确定（仅收尾报告） |
| r2-ps#2 | 字段保护修复缺陷：欠款守卫遇到"已有欠款带未知字段"返回 `kFeatureUnavailable`，收件箱执行器把非 kSuccess 一律当终态 REJECTED，GM 追缴永久丢失（守卫注释说"由新版节点来挂"，实际不会再投）。变种：将来 `reserved` 退役欠款字段后，残留字节永远是未知字段，同币种每次挂欠款都被拒 | `cpp/libs/modules/currency/system/currency_system.cpp:357-367`；`cpp/libs/services/scene/player/system/gm_sanction_inbox_executor.cpp:31-34`、`:124`；`player_inbox_system.cpp:880-883`；`go/player_inbox/internal/inbox/decide.go:68-69`；物品同源 `cpp/libs/modules/bag/item_store.cpp:325-328` | 收件箱消费缺省关（`bin/etc/game_config.yaml:35`）；变种 A 仅混版，变种 B 要将来退役字段。开收件箱或开始退役字段后应复评为 medium。代码是工作区未提交的字段保护第 3a 批半成品 | 未登记（设计 §4.4 `:243` 只写"GM 操作返回错误"；`:133` 称退役残留"无害"，与守卫判据矛盾） | 很可能 |
| r2-ps#3 | 引导关闭标记的"只关不开"只在单个进程内成立：标记丢失（备份恢复、主从切换、误删）后重启的 go/db 读到 Nil 判为开放，C0 失效，各 Pod 状态不一致且无告警 | `go/db/internal/kafka/persist_schema.go:48-50`、`:59-69`、`:74-85`、`:108-121`；`go/db/db.go:194-205`；`deploy/k8s/README.md:2396`；`deploy/k8s/persist-schema-alerts.yaml:95-105` | 仅混版窗口，需丢键加重启。世代 ≥2 后 C1b 能兜住 | 未登记；注释 `persist_schema.go:49` 与 README:2396"误删也不放宽"与重启后的行为相反 | 确定 |
| r2-ps#4 | C0 / C1b 进死信后，文档给出的恢复路径都走不通：重放必然再被拒；`save_barrier_restore` 因 `outbox_*` 字段或 zone 待补投索引残留而拒绝；单表死信时 `readPair` 拒绝。玩家被父提交屏障永久卡在登录，只能手改 Redis 与 SQL | `go/db/internal/kafka/key_ordered_consumer.go:2324-2333`；`persist_schema.go:169-171`；`go/db/internal/saveoutbox/scripts.go:63-67`；`go/db/internal/barrierrestore/restore.go:329-331`、`scripts.go:35-44`、`:52-56`；`parent_commit_barrier.go:42`；`deploy/k8s/README.md:2399-2402` | 需 C0 或 C1b 真的触发（误部署，或关闭引导期时仍残留未盖戳意图）；缺省未关闭引导期、世代为 1，不触发 | 部分登记：设计 `:346`、`:370`（L-G）给出的处置是重放或恢复工具，没写二者都不可用；同根因见第一轮 §4"DBTask 被永久拒绝后被登录预检锁死" | 很可能（仅结构化结果） |
| r2-ps#5 | 合堆守卫只改了一部分：`IsIndependentInstance` 已用于 3 处，但入包规划与装备预检仍只看 `has_equip`，头注释却声称三处共用。带未知字段的非装备实例入包时规划与提交分叉，Debug 下 `assert(false)`，Release 返回 `kInvalidTableData`，批次零改动 | 已改：`cpp/libs/modules/bag/item_store.cpp:182`、`:322`、`:416`；注释 `item_store.h:159-162`；未改：`bag_system.cpp:647-648`、`cpp/libs/services/scene/player/system/player_equip.cpp:442` | 仅混版窗口，且需有调用方把带实例数据的 ItemComp 交给 `AddItems`（未找到）。不丢资产 | 未登记 | 存疑（仅收尾报告；工作区半成品） |
| r2-mz#2 | runbook 称 copy 合服后源区库是"冷副本、可按备份口径归档"，但落点记录指向源区的玩家不拷行，源区库仍是他们的真源。照 runbook 归档后，这批人存盘无限延后、登录读失败 | `docs/ops/merge-zone-runbook.md:335`、`:1097-1098`；`tools/merge_zone/merge_run.go:472-476`；`placement_ops.go:137-140`；`docs/design/player-storage-placement.md:328`、`:332` | copy 模式加"源区玩家已有指向 src 的落点记录"（PinOnCreate 缺省 false，但设计终态是全部钉完）加运维下线源区库。人为动作，最高 low | 未登记（runbook §5.1 与设计 §10.2 矛盾） | 很可能（仅结构化结果） |
| r2-rr#1 | 路由对账 leader 内存里的未决行在换届时丢失：cvm 已越过这些行，新 leader 标记可信时不预热，增量扫不到，缓存一直缺键或错值，读方仍信任就绪标记，带非 home 落点的玩家会写错库 | `go/data_service/internal/routereconcile/reconciler.go:736-738`、`:1309-1321`、`:1224-1225`、`:1700-1706`、`:1797-1817`、`:984-986`；`cache.go:436-438` | 门控：`RouteStore.Mode=db_first`（缺省 redis，`data_service.yaml:185`）、`ArmReadyMarker=true`、读方开关打开，再加 noeviction 拒写与换届同时出现 | 未登记 | 代码路径确定，受害面推断（仅收尾报告） |
| r2-rr#2 | `ArmReadyMarker=false` 下 mapping Redis 身份变化后，追平起点只取 cvm−60s，不看随旧备份恢复的水位键；从旧备份恢复或提升落后副本时，中间这段变动的映射永不补回 | `reconciler.go:1679-1692`（对照 Arm=true 路径 `:1709` 用 min(标记水位, cvm)）；`store/route_reconcile.go:419` | 门控 `db_first` 且 Arm=false；需从旧备份恢复。P1 冻结窗口内无合服搬库，以缺键 fail-closed 为主 | 部分登记（文件头 `:42-43`） | 很可能 |
| r2-rr#3 | Arm=false 时，Redis 整体丢失后的全量追平一旦中断，就永久退化为只回看 60s：leader 同一拍用 SET NX 重建了水位键，抹掉了"水位键缺席即全量"这个唯一信号，cvm 也没归零；之后的追平只补最近一分钟，却照样记 catch_up completed 并置 VI | `reconciler.go:1188`、`:1528`、`:1571-1597`、`:1679-1692`、`:1409-1431`、`:709-719`、`:1293-1321`、`:616-627`；`cache.go:298-303`；`go/db/internal/kafka/placement_route.go:78`、`:184` | 门控 `db_first` 且 Arm=false；需 mapping Redis 整体丢数据，且分钟级全量追平被打断（换届、单批出错、令牌写失败）。Arm=true 不受影响 | 未登记（提案 `player-route-mapping-durability-proposal.md:302` 要求"预热完成再起 go/db"，退化后完成日志与全量一样，会误导运维） | 确定（仅结构化结果） |
| r2-rr#4 | 合走标记镜像只增、写入前不复读库：unmerge 先删库行、再按值 CAD 镜像，对账 leader 在两步之间读到旧行并 SET NX，会把刚删的 `merge:merged_into:{src}` 永久写回；之后回档 preflight 被拒，`reconcile-from-cache` 还会把过期标记写回库 | `reconciler.go:1059-1085`；`cache.go:488-551`；`tools/merge_zone/merged_into.go:251-287`；`go/data_service/internal/routetool/repair.go:30`、`:323`；`tools/scripts/k8s_zone_rollback.ps1:183-184` | 门控 `db_first` 且执行 unmerge；毫秒级窗口，命中即永久。fail-closed，不直接丢资产 | 未登记（围栏镜像删前会复读库 `reconciler.go:1968`，合走标记镜像没有） | 很可能（仅结构化结果） |
| r2-mm#1 | 神秘商人限购记录只保留当前一轮。运维调大出现间隔且新停留期盖过旧轮结束时，scene_manager 强制覆盖写新轮，已缓存旧轮的节点继续卖旧轮、新起节点采纳新轮，两轮同时在售；玩家两线往返，每次换轮整份清空记录，限购反复清零 | `cpp/libs/services/scene/player/system/mystery_merchant_rules.h:119-133`、`:143-149`、`:178-182`；`player_mystery_merchant.cpp:265-273`、`:581-585`、`:650`；`go/scene_manager/internal/logic/mystery_merchant.go:531-535`、`:542`、`:570`；`proto/common/component/player_mystery_merchant_comp.proto:14-15` | 缺省不触发（Override 缺省 0，`go/scene_manager/internal/config/config.go:455-456`）；需运维调大间隔加窗口内新起 scene 节点。付费购买，不复制资产 | 未登记（`docs/design/mystery-merchant.md:95-101`、`:165-166`；`mystery_merchant.go:291` 与 comp proto:14-15 断言不串轮） | 很可能 |

### 3.5 第二波附带观察

- **字段保护在缺省配置下 go/db 侧实际不生效**：两份生成产物都是世代 1（`go/shared/generated/persistschema/persistschema.go:9`、`cpp/generated/persist_schema/persist_schema_gen.h:12`），C1 / C1b / C2 走不到；C0 要等人工写引导关闭标记；C3 / C4 未落码，login 没有 `schema_gen` 的读者。
- **C++ 携带只在工作区**：`unknown_field_carry.h`、`player_persist_carry_comp.h`、`player_persist_marshal.h` 是未跟踪文件。只提交已跟踪文件会断构建；停在 HEAD 时第一轮 low「存盘不保留未知字段」仍成立。
- **路由对账 catch_up 收尾不查未决就置 VI**（`reconciler.go:742-744`），偏离规格 must-1（`spec_D1_batch1b2_2_3.md:94`）。
- **login 待查疑点**：`go/data_service/cmd/debugutil/bundle.go:399` 把 `account:{account}` 列为 bundle 条目，debug_import 可能不拿账号锁就整块写回账号目录，盖掉并发建出的角色。运维工具、缺省 dry-run，最高 low，建议与 r2-ds#1、r2-ds#2 一起查。
- **神秘商人字段号文档与代码不一致**：设计 §2、§9.1 写 `player_database.mystery_merchant = 21`，代码是 22（`proto/common/database/mysql_database_table.proto:184`）。文档问题。

### 3.6 第三波第一批（基线 `c63b22dca` 加 2026-10-19/20 工作区）

四路：资产杂项（assets-misc）、lifecycle 未读段（lifecycle-unread）、写入口与闸（write-entries-gates）、login 余项（login-rest）。行号是当时工作区的行号，之后主干又合入大量提交，引用前请按函数名重新定位。字段保护第 3a 批那一路两次撞额度失败，挪到第二批重跑。

**medium：**

| 编号 | 问题 | 位置 | 触发时序 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|---|
| r2-lc#1 | **交接标记的写入在旧连接上迟到，晚于新连接上的取证脚本。** 解冻后留下一份当前代际的有效 handoff 标记，下一次跨节点落点不存盘就过换手门，回档 | 写标记 `cpp/libs/services/scene/player/system/player_lifecycle.cpp` 的 `BeginTravelHandoff`（`kPublishMarkLua`，约 :5709-5732）；取证判 kUnchanged 后清 markEpoch、解冻、不登记撤回（约 :6055-6064、:6665）；`IsSceneChangeBusy` 只看待撤回表（约 :5014）；取证脚本只删当时已存在的标记（`player_lifecycle.h` 约 :802）；标记守卫只校验 owner、pending、两表 ACK，没有时效（`handoff_sql_barrier.h:30-80`）；存活探测 15s 无应答就摘连接，`redisAsyncFree` 正常 close，内核继续重传（`cpp/libs/engine/thread_context/redis_manager.cpp:402-423`、`:72-81`）；换手门见当前代际标记即放行（`go/scene_manager/internal/logic/enterscenelogic.go:891-898`） | ① 冻结存盘落地后，在连接 C1 上发 `kPublishMarkLua`（"E:t"）。② EVAL 到达 Redis 前网络分区，15–20s 后存活探测摘下 C1；SET 回调收到空应答，走 `kMarkWriteUnknown`，重挂 30s 看门狗；内核继续重传这条 EVAL。③ 分区恢复，重连得到 C2；看门狗到期，在 C2 上发取证脚本：标记还不存在，owner 仍是 E，判 kUnchanged，解冻，不登记撤回。④ C1 重传的 EVAL 到达，owner、pending、ACK 都还满足，SET handoff "E:t" EX 300。⑤ 300s 内玩家换图去别的节点：换手门放行并铸 E+1，Y 读到冻结时的父快照，X 的退出存盘被 CAS 拒绝，解冻后的进度全丢；或者玩家断线，LeaveScene 把这份标记当作释放证据立刻删 location，重开第一轮 §2.1 | 缺省配置可达，无开关；存活探测缺省开（`redis_manager.h:21-22`）。需要 scene 到 Redis 的网络分区持续 15–20s 以上、并在内核放弃重传前恢复，且分区开始时恰有一条标记写在途。Redis 进程卡住或主从切换不触发 | 未登记。`BeginTravelHandoff` 空应答分支的注释只考虑了标记已落地；`shared-redis-split-by-purpose.md:1698` 的同连接 FIFO 清单只在 Cluster 迁移语境下讨论；`cross-zone-scene-travel.md:546` 的 M13 只讲迟到的 A1 | 很可能 |
| r2-lc#2 | **按 owner_epoch 条件写的释放与凭证标记（A1′、改派、改派踢线前补写、会话结束凭证、A2′ 放弃补写）在旧连接上迟到，晚于同节点重载时的 A2′。** 新持有期间留下有效标记，后续时序同 r2-lc#1 | `SendConditionalMarkWrite`（`player_lifecycle.cpp` 约 :1125-1170，注释依赖同连接 FIFO、不重试）；A1′ `FinishExitAfterPersist`（约 :3332）；改派 :3852、:1633；会话结束凭证 :5502；A2′ 放弃补写 :1256、:1390；A2′ 载入前清继承标记 :2686-2689；`exit_release_mark.h:180-183`（只比 owner） | ① X 上退出存盘落地，在 C1 上发 A1′ "E:t1" 并销毁实体。② 分区，存活探测摘下 C1，回调计失败、不重试，内核继续重传。③ 分区恢复，玩家 30s 内短重连回同一节点，同节点不铸造，epoch 仍是 E。④ 在 C2 上发 A2′（无标记，返回 0），载入建实体。⑤ C1 的 A1′ 迟到落地，新持有期间出现当前代际标记。⑥ 之后换图或断线，同 r2-lc#1 的 ⑤ | 缺省可达：A1′ 缺省开（`player_lifecycle.cpp:969-972`，`SCENE_EXIT_RELEASE_MARK`），存活探测缺省开。比 r2-lc#1 多一个"同节点短重连"的前提 | 部分登记：`cross-zone-scene-travel.md:546`（M13）登记了身份冲突或分区下迟到的 A1，归为活性 P3，`:662` 写明收敛方案没做；同进程、身份正常、因存活探测摘连接后同节点重载的情形未登记，后果是回档而不只是活性 | 很可能 |
| r2-login#1 | **新号首次进场时 owner_epoch 已铸、首次存盘之前出任一故障，该角色此后永久无法登录，屏障恢复工具也拒收** | 登录卡点 `go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go`（父键在、无 pending、owner≠0 拒绝，当时 :112，HEAD `c63b22dca` :101；父键不在时 :126 / :110）；铸 epoch 写 location `go/scene_manager/internal/logic/enterscenelogic.go:687`、`:771`，路由失败回滚 epoch 只进不退 `:801-811`、`owner_epoch.go:472`；gate 会话已断时直接丢弃 RoutePlayer `cpp/nodes/gate/handler/event/gate_event_handler.cpp:55-65`；新号注册分支不立即存盘 `player_lifecycle.cpp:4211-4219`，周期存盘 300s（`dirty_save_stats.h:13`）；恢复工具对没有 SQL 行的玩家拒绝 `go/db/internal/barrierrestore/restore.go:349` | ① 新号首次 EnterGame：owner=0、无 pending，冷读得到空行，发布父键骨架。② scene_manager 判无 location，铸 owner_epoch=1 并写 location。③ 首次存盘之前出任一故障，save_barrier 都不会被创建：(a) 推 RoutePlayer 到 Kafka 失败，回滚 location、epoch 再 +1；(b) 路由到达 gate 前客户端断线（包括 login 等 EnterScene 超过 5s 后推 kEnterSceneFailed、客户端收到即断开），gate 丢弃路由，scene 从未建实体；(c) 实体建好，但 scene 在首次周期存盘或退出存盘之前崩溃，或加载失败、进场被拒。④ 此后每次 EnterGame 都卡在预加载："历史父快照缺少提交屏障"或"历史玩家父快照与提交屏障缺失"。⑤ 屏障只能由 scene 存盘写出，进不了场就不会自愈；`save_barrier_restore` 对没有 SQL 行的玩家报"SQL 玩家行缺失" | 缺省配置可达，无开关；`NewPlayerSeed.Enabled` 开或关都触发。(a) 一次瞬时 Kafka 生产失败即可；(b) 开服高峰 EnterScene 长尾超过 5s 就会诱发；(c) 一次 scene 崩溃会影响该节点 300s 内首次进场的全部新号。fail-closed，新号本身没有进度可丢，但角色永久不可用 | 部分登记：`db-write-fencing-mysql.md:1556`"已铸造 owner 却无屏障仍明确拒绝"是有意规则，但没分析新号会经正常故障路径进入这个状态，也没给恢复办法；同一卡点与恢复工具缺口见 r2-mz#1 | 很可能 |

**low：**

| 编号 | 问题 | 位置 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|
| r2-mail#1 | `mailadmin` 不带 `-request-id` 时每次随机生成请求号。结果未知后原样重跑同一条命令会得到新键，带附件的个人邮件重复发放 | `go/mail/cmd/mailadmin/main.go:59`、`:116-128`、`:148`、`:182-184`；判重只认 (sender_kind, request_id) `go/mail/internal/data/repo.go:139-156` | 运维工具，要人为重试，且首发实际成功、应答丢失；缺省随机号放大了误用面。M2 的 Send 没有 Mode 闸，生产 admin 也能发带附件的个人邮件 | 部分登记：`main.go:2`、`mail-system.md:117`、`:133` 写了重试要用首次的 request-id，没登记"缺省随机号让原样重跑即重复发放" | 确定 |
| r2-mail#2 | rank 奖励把过期时间写死为封榜 +30 天，与 mail 个人邮件可配的 TTL 不联动。调小 `Mail.MaxPersonalMailTtlDays` 时奖励被判非法，按分钟级无上限重投，直到落进窗口；收件箱一直满到 +30 天时奖励永远投不出去，重投也没有上限 | `go/rank/internal/reward/worker.go:87`、`:95-107`；`go/rank/internal/data/rewards.go:53-57`；`go/mail/internal/data/repo.go:162-174`、`:231-236`；`go/mail/internal/data/policy.go:9`、`:29`；`go/mail/etc/mail.yaml:34` | 调小 TTL 需运维改配置（缺省 30 天恰好对齐）；收件箱满 30 天在缺省配置下可达；rank 发奖受部署门控，仓内没有 Freeze 调用方 | 已登记变种：[一致性核对](data-consistency-guarantee-map-20261020.md) §2.3 social-global#2 已登记"30 天固定过期、无终态"；新增的是"TTL 可配不联动"与"重投无上限违反 AGENTS §11.3" | 很可能 |

**写入口与闸的覆盖结论（write-entries-gates，没有发现绕过）：**

- 冻结闸不在客户端统一入口，由各业务系统入口自判 `PlayerFrozenComp`；它与 `PlayerTravelHandoffComp` 只在 `StartTravelHandoff` 一起挂、在同几处一起摘，没有只有其一的窗口。退出闸对客户端消息统一（`scene_handler.cpp:273` 只放行 ExitGame），其他入口各自判，再由退出收敛重存兜底：`HandlePlayerAsyncSaved` 比对落地内容与当前内存，不一致就重存，等 DBTask 回执期间再比对一次。
- 逐入口核对过：背包整理、装备 / 强化 / 镶嵌、宠物、属性、任务、神秘商人购买、GM 货币、回档桩、移动、技能（含相位定时器）、跨场景与跨 zone 传送；gRPC 资产通道（Decide 第 5 步判冻结 / 交接 / 退出 / epoch0 / 战斗）；Kafka 的战斗结算、收件箱通知、组队刷新；World tick 里的移动、buff、挂机、属性计算与同步（都 `entt::exclude<PlayerFrozenComp>`）；收件箱 Tick、战斗 reaper；邮件附件领取走资产通道，GM 处置走收件箱。
- `PrepareBattle` 不判退出，但只挂运行时 `InBattleComp` 和 Redis 锁、不写持久组件；`PlayerMissionSystem::OnCondition` 冻结时静默丢进度，但两个调用方都先拒冻结，当前不可达，属于将来新增调用方时的隐患。

### 3.7 第三波第二批（基线 HEAD `940ef9746` 加 2026-10-22 工作区）

六路：字段保护 C3 / C4（3b、第 4 批）、字段保护 C++ 携带（3a、3a′）、帮会与回档脚本、匹配排行与 go/db 余项、合服余项、路由读写余项。字段保护 C3 / C4 一路没有新发现。

**可达性更正（r2-match#1，medium）：staging / prod 部署强制开全球匹配。** `tools/scripts/k8s_deploy.ps1:7462-7467` 的 `Get-GlobalMatchDeploymentSettings` 在 Profile≠dev 且没给 `-MatchTiDBHost` 时直接报错，`:8440-8446` 渲染 ConfigMap 时用计算值覆盖成 `Enabled: true`；这项强制从 `e76f6f747`（2026-10-16）起就有，早于两轮审计。仓内 `go/match/etc/match_service.yaml:12` 的 `GlobalMatch.Enabled: false` 只对 dev 和本地生效。因此两轮按"`GlobalMatch.Enabled` 缺省关、开通前修"降级的条目，在上线配置下都是缺省可达：

- 第一轮 §2.5 social-match-battle#0（全球票据与 battle 实例 uuid 强绑定、节点替换后永久卡住）：当前代码仍未修（`go/match/internal/authority/store.go:908-921`、`global_match.go:360-373` 只重排 retry_at，没有到期出口）。
- r2-battle#1、r2-battle#4：已修（未编译）；按"默认可达的资产复制"口径，验证通过前应计 high。r2-battle#2（同局成员结算不对称）仍开着。
- 一致性核对 cross-service-assets#1 的全球确认模式变种。
- ~~要负责人二选一~~ **负责人 2026-10-23 拍板：首发就开全球匹配。** r2-battle#1/#4 的 Codex 验证列为上线门禁；social-match-battle#0 与 r2-battle#2 改为上线阻塞。没选的另一条路是改 `k8s_deploy.ps1` 允许 staging / prod 关闭全球匹配，并把 Redis 兼容路径纳入上线审计。同日负责人确认：项目未上线，玩家数据可以完全清空，修复不需要考虑存量数据迁移。

**路由默认模式也变了（route-reconcile-rest 的更正）：** 全球配置强制 `db_first` + `ArmReadyMarker` + `ReadRequiresReady` + 对账开启（`go/data_service/internal/config/global_authority.go:27-29`、`data_service.yaml:212-240`），go/db 强制 `Placement.Required` 与 `RequireRouteReady`（`go/db/internal/config/config.go:732-737`），`k8s_deploy.ps1:741-745` 拒绝其他形态。所以 r2-rr#1、r2-rr#4 由"门控"变为缺省可达；r2-rr#2、r2-rr#3 与 §3.5「catch_up 置 VI」在全球服务里已不可达；第一轮 data-service#3 的前提（映射只在 Redis）已被 db_first 取代。r2-rr#1 的受害面：W1 发布失败时 login 拒绝建角、该 id 作废，孤儿行由 leader 下一次增量补发；剩下的只有工具写入（merge_zone、relocate、pin、backfill）后发布失败又没重跑的行，读方对这类行是 fail-closed（go/db 判 missing_required 进死信），不会写错库。

**medium：**

| 编号 | 问题 | 位置 | 触发时序 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|---|
| r2-guild#1 | **帮会历练开局成功（formation=4）后战斗不出结果，`guild_trial_launch` 与参与者 ACTIVE 占用永不解除。** 这批角色永久不能再参加历练、不能上架交易、删除或导入角色，全球匹配下还进不了任何匹配 | `go/guild/internal/logic/trial_launch.go:93`、`:100`（recoverOne 只认 BattleId==0 的取消或 formation=5）；`go/guild/internal/data/trial_launch_repo.go:195`、`:270`（解散时释放是空函数）、`:305`；`activity_repo.go:1756`（ExpireTrialBattle 只改对局行，不碰 launch）；`trial_match_guard.go:53`；`go/data_service/internal/store/account_role_asset_guard.go:159`；`go/match/internal/authority/entry_guard.go:208`、`store.go:812`（checkWork 要求 state<4，4 到 5 无迁移）；battle 停机作废 `cpp/nodes/battle/main.cpp:363`→`battle_room_manager.cpp:1884`，活动结果单发失败 `:2614` | ① 帮会确认开战，launch 进 state2，match 编队 Finish(true) 到 formation=4。② 战斗中 battle 节点收到 SIGTERM（Agones 删 Pod、驱逐、缩容）走 AbortAllRooms，注释写明"不结算、战斗作废"；或进程崩溃，或活动结果落库 / 单发失败。③ guild 等不到结果：巡检 3600s 后只把对局行改 EXPIRED；recoverOne 每 10s 发一次签名取消，match 回 formation=4，永远到不了 5。终结 launch 的路径只有结算、标毒、帮会已解散（都要结果事件）和 FinishCancelledTrialLaunch（要零 battle 墓碑或 formation=5），全部走不到。④ 名单里每个人被三处占用守卫永久拒绝；没有自动解除，仓内也没有人工释放工具 | 门控：资产通道开（默认历练行带物品奖励，通道关时建房即拒，`activity_trial.go:505`）。`guild.yaml:154` 本地与 K8s dev 档默认开，staging / prod 由 `k8s_deploy.ps1:8236` 改为 false，上线前按 08 §8.3 打开。打开后一局历练碰上 battle 停机或崩溃即触发，不需要误用 | 未登记。`docs/design/guild-phase2/06-activities.md:968` 的 R1（战斗中崩溃→巡检判 EXPIRED）写于 launch 占用引入之前，现已不成立；`account-role-authority.md:101`、`cross-zone-matchmaking.md:604` 都没分析 formation=4 之后永不出结果 | 很可能 |
| r2-match#1 | **staging / prod 强制开全球匹配，已登记的"全球匹配门控"条目在上线配置下缺省可达**（见本节开头） | `tools/scripts/k8s_deploy.ps1:7462-7467`、`:8440-8446`；`go/match/etc/match_service.yaml:9`、`:12`、`:138` | 按 K8s 流程部署 staging / prod 即生效 | 上线配置缺省可达 | 未登记为"上线配置强制开启"；两轮文档与一致性核对都写"缺省关" | 确定 |
| r2-rr#5 | **mapping Redis 一次重启或切主，go/db 就绪闸让全服存盘长时间停摆。** 重新挂标记要等全键空间 SCAN 走完；补投器不看就绪闸，同时放大 Kafka 积压，超过保留缓冲后，停摆早期下线玩家的最后存盘被 Kafka 裁掉，补投又已 exhausted，返场永久拒登 | `go/data_service/internal/routereconcile/reconciler.go:330-331`、`:702`、`:1873-1878`（挂标记要求一整轮在任务开始后才开始的 SCAN 扫完）、`:2060-2066`；`cache.go:50`、`:583`（每拍 2 页 × COUNT 1000）；`data_service.yaml:243`（拍间隔 1s）；`k8s_deploy.ps1:9055-9058`（K8s 上 mapping Redis 就是共享 Redis 单实例）、`:741-745`；`global_authority.go:27-29`；`go/db/internal/config/config.go:735-737`；`go/db/internal/kafka/route_ready_gate.go:9-11`、`:407-418`；`go/db/internal/saveoutbox/replay.go:165-185`（补投 Tick 不看就绪闸）、`:37-41`、`:74-77`；`scripts.go:78-100`；`parent_commit_barrier.go:49`；`deploy/k8s/db-task-retry-alerts.yaml:430-433`（retention.bytes 按"设计写入速率 × 2h"）；`deploy/k8s/data-service-route-alerts.yaml:242-258` | ① 共享 Redis 的 Pod 重启、升级、驱逐或切主。② 读方判身份不符，leader 撤就绪标记、排修补；全部 zone 的 go/db 同时关闸，不从 Kafka 取消息。③ 修补完后 tryArm 还要等一整轮新 SCAN 扫完全部键，停摆约"修补耗时 + 1~2 × 键数 / 2000 秒"。④ 停摆期间在线玩家照常存盘进 Kafka；补投器对每个未 ACK 的意图按退避重发两条整行任务，Kafka 写入放大约 6 倍，broker 确认后照常计次，约 21 分钟进 exhausted。⑤ 按放大后的速率，设计负载下约 20 分钟开始裁最旧的段。⑥ 停摆早期下线的玩家，最终版本和补投副本都在被裁段里；闸重开后读不到，意图已 exhausted，返场永远报 `parent SQL barrier not committed`，只能人工重置计次或按 SQL 版本做屏障恢复（回档） | 缺省配置可达，全球门禁三处禁止关闸，设计里"先关读方开关"的回退路径已被禁止。停摆期间所有返场玩家被拒登是必然的；⑤⑥ 是否丢版本取决于键数与负载，按每秒约 2000 键估，约 120 万键以上的共享 Redis 在设计负载下可达。数据仍在 outbox，默认结局是 fail-closed 拒登，不是静默回档 | 未登记。提案 `player-route-mapping-durability-proposal.md:302` 估 RTO 为"全量预热时长"并写"不丢盘"，没计入挂标记前要扫完全键空间；告警 GoDbRouteGateClosed 写"积压不丢"，与保留期说明矛盾；r2-db#1 的修复只考虑了发布失败 | 很可能（门槛为估算） |

**low：**

| 编号 | 问题 | 位置 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|
| r2-ps#6 | 欠款自动扣还（`AddCurrency`）对带未知字段的欠款不设守卫：旧版本按 owed−paid 扣、扣满即整条删除，新版本欠款字段（设计示例里的 interest）随之消失 | `cpp/libs/modules/currency/system/currency_system.cpp:170-197`；`player_currency_comp.h:24`、`:51`；`mixed-version-persist-field-protection.md:124`、`:244` | 仅混版窗口；欠款唯一来源是收件箱执行器，收件箱消费缺省关 | 未登记；§4.4 只守合并 | 很可能 |
| r2-ps#7 | 神秘商人换轮是逐字段清空（`clear_purchases` + `set_round_id`），没有 `Clear()`，新版本加的按轮作废字段会跨轮存活 | `mystery_merchant_rules.h:143-149`；`player_mystery_merchant.cpp:650`；对照 `asset_op_ledger.cpp:317-325` | 仅混版窗口，且要新版本给该组件加按轮作废的字段 | 未登记；设计 §4.2 L7 要求逻辑重置一律 `Clear()`，§4.4 只改了资产通道纪元重置 | 确定 |
| r2-ps#8 | `SanitizeSkillList` 加载时把职业技能等级压回当前表的 max_rank、把通用技能等级清零、删 rank 0 的职业技能，并落盘。金丝雀或回滚后已付费的修习等级永久丢失。运行期 `EffectiveRank` 本来就按 max_rank 夹住，存储层压回不是正确性需要 | `player_skill.cpp:375-403`、`:414-420`；`class_skill_rules.h:80-83` | 仅混版或回滚窗口，条件是新版本提高 max_rank（纯配置） | 已登记变种：第一轮 §4 第 1 条只修了"查不到表的技能"（`94dd15382`）；设计 §12 未核对行不含技能等级，`release-checklist.md` 没有持久值域门禁 | 确定 |
| r2-ps#9 | 3a′ 只修了 r2-ps#2 的变种 A：退役字段的残留字节仍被当作版本错位，字段退役后该玩家的 GM 处置域永久停在 `kVersionSkew`；物品侧同源，退役残留让相关物品永不合堆 | `currency_system.cpp:398-410`；`gm_sanction_inbox_executor.cpp:136-140`；`player_inbox_system.cpp:907-920`；`persistschema/render.go:11-33`（不产出退役字段号）；设计 `:133` | 收件箱消费缺省关，且要将来真的退役欠款字段；不是混版专属 | 已登记：r2-ps#2 变种 B，修复方向已要求区分退役残留，3a′ 没实现 | 很可能 |
| r2-ps#10 | 台账发号识别不了主干上的并发分配：两个改动各自拿到同一个 N+1，先发布一个后再合入另一个，同为世代 N+1 的两个二进制认识的字段集合不同 | `persistschema/apply.go:43-46`、`:91-93`；`ledger.go:62-68`、`:115-117`；`release-checklist.md:119` | 仅混版窗口，且在台账切 active 之后 | 已登记变种：与 r2-ps#1 同根，字段集合指纹门禁仍是待办；"主干并发发号"这一来源未登记 | 很可能 |
| r2-dcc#1 | `data_consistency_check` 的两条帮会检查会假通过：榜单孤儿检查扫的是映射 Redis（缺省 DB 0），公会榜实际在 guild Redis DB 2；guild 表读不到时按 info「not present」报，与"没有公会"无法区分 | `tools/data_consistency_check/main.go:119-121`、`:187`、`:338-342`、`:356-358`、`:387`；`go/guild/etc/guild.yaml:32-35` | 缺省参数即触发；只影响检测，不改数据 | 未登记；文件头称"读失败绝不折算成通过"，与 info 分支矛盾 | 确定 |

**附带观察（混版或回滚窗口，未单列）：** `Bag::InsertItemForRestore` 三条退路都放不下时会丢物品（`bag_system.cpp:1492-1499`），配置把占格改小再回滚时可能命中，属于设计 §12"未核对"行的一个具体删数据点；`ConvergeOverAllocation` 在新版本新增加点来源时会整池返还（R5）；设计 §8.3 与 §13 第 7 条要求发布单写明"没有扩展持久值域"，`release-checklist.md` 没有对应检查项。字段保护 C3 / C4 在缺省配置下（warn 模式、台账 bootstrap、世代 1）不改变存盘与登录语义，写方令牌在同节点重载、跨节点交接、复用实体、首存在途、首存被顶替、回包丢失六类情形下都判对；`schema_regressed` 字段与 restore 的 HDEL 在审计时仍是工作区半成品，冻结后增量审计要重新核对。

### 3.8 第四波：第一轮 §4 的 22 条 low 复核

按当前 HEAD（2026-10-22）逐条核对，两名 Sonnet 复核员。结论：**已修复 2 条、推翻 1 条、部分成立 3 条、仍成立 16 条**，全部维持 low；另补 2 条 low。

| 第一轮 §4 编号 | 条目 | 复核结论 | 要点 |
|---|---|---|---|
| 1 | `SanitizeSkillList` 按当前技能表永久删除技能 | 部分成立 | "查不到表的技能被删除"已由 `94dd15382` 修掉（保留、不生效）；按表压回 rank、清零、删 rank 0 仍在，另列为 r2-ps#8 |
| 2 | 父快照提交前不检查行大小，DBTask 被永久拒绝后锁死登录 | 成立 | C++ 提交前没有大小闸；go/db BlobGuard 判确定性拒绝并打终态。组件体量有界，几乎不可达 |
| 3 | 严格屏障取消 owner 缺键补种，Redis 丢数据后在线玩家被判废黜 | 部分成立 | 不再误销毁：缺键返回 error_reply，走封顶退避无限重试、实体被钉住（fail-closed）。`scene-owner-reentry-barrier.md` §6.2 与 `db-write-fencing-mysql.md:1058` 仍写补种，与代码相反；缺专用告警（另补 r2-low#1） |
| 4 | 存盘不保留未知字段 | 已修复 | 3a 已随 `c35e1133d` 入库；3a 有 Codex 通过记录（`PersistRoundTripTest.*` 等），3b、第 4 批没有 |
| 5 | `AddCurrency` 没有 uint64 溢出检查 | 成立 | 入参 int64，要回绕需余额接近 1.8e19，缺省玩法不可达 |
| 6 | `AttachDebt` 合并时让已过期的旧欠款复活 | 成立 | 另有冻结旧欠款被继承、审计字段被覆盖两个变种；收件箱消费缺省关 |
| 7 | 交易流水早于落盘发出 | 成立 | 今天没有消费者把流水当账本回放（BatchRecall 未实现），只造成对账噪音；接线前要补约束 |
| 8 | 账号 blob 有两个写者不走建角锁 | 推翻 | 账号角色目录已迁成全局 SQL 权威，login 只经 AccountDirectory 读写，前提已不存在 |
| 9 | 会话栅栏的 player 绑定随 TTL 过期，同一连接可绑第二个角色 | 成立 | TTL 6 分钟、没有续期；gate 也不挡 |
| 10 | 收件箱 Ack 以 Redis 落盘为依据 | 成立 | 已登记（`player-inbox-service.md:1125-1129`），并入一致性核对的 Redis 回退一族 |
| 11 | 已打完的战斗被全球匹配恢复误判为未建成，票据回队 | 成立 | QueryBattleCreation 没有 FINISHED 状态；按 r2-match#1，上线配置下缺省可达 |
| 12 | 评级来源停写后结果消费卡死 | 成立 | 停写后整个 match-results 消费者被堵；StopRankSource 目前无调用方 |
| 13 | 合服 P3 只取一次积压快照 | 成立 | 另补 r2-low#2（P3 覆盖校验用 describe 时刻的 LAG） |
| 14 | `-skip-player-rows` 不校验落点 | 成立 | 三重人为声明，最高 low |
| 15 | 撤销时榜单 ZADD 导致分数回退 | 部分成立 | 按 runbook 撤销范围（dst 未开服）不可达；GM 改分或违规开服后撤销可达 |
| 16 | merge_zone 不识别 db_first | 已修复 | `-route-source` 必须显式选，并核对库与 Redis 成对；无 Codex 全包测试记录 |
| 17 | `kafka_offset_reset` 不检查回档切点 | 成立 | 已登记为运维禁令，回档 `-Apply` 当前不可执行 |
| 18 | 区服 PUT 整行覆盖，漏传字段会改成 OPEN | 成立 | 有意保留的整行语义，陷阱仍在 |
| 19 | 商城第二笔不同支付回执被拒且不生成退款 | 成立 | 缺省无支付渠道，接真实渠道前修 |
| 20 | FenceClock 在生产上退化为单调钟 | 成立 | 两个带单调读数的 Time 做差只用单调读数；单测用 `time.Date` 掩盖了 |
| 21 | reclaim 在 slots 键过期时绕过隔离期 | 成立 | 需失联超 4h、继任者恰好断开，概率极小 |
| 22 | login 毫秒水位前推量 2000ms 不满足契约 | 成立 | 缺省号段开启、不回退 snowflake，只在关号段时可达 |

**复核中补的 low：**

| 编号 | 问题 | 位置 | 可达性 | 已登记 | 自信度 |
|---|---|---|---|---|---|
| r2-low#1 | 核心 Redis 丢 owner 键后，在线玩家存盘永久报错重试、实体被钉住，进度只在内存，节点排空时丢失；`missing owner` 错误回复和其他 Lua 错误混在同一行 ERROR，没有专用指标与告警；两份设计文档仍写"自动补种" | `cpp/libs/engine/infra/storage/redis_client/redis_client.h:131-139`、`:984-1025`、`:1073-1096`；`player_lifecycle.cpp:2096-2125`；`db-write-fencing-mysql.md:1058`、`:1549`；`scene-owner-reentry-barrier.md:108-131` | 需核心 Redis 数据丢失，属一致性核对 §2.2 的 Redis 回退一族 | 部分登记：行为是设计意图（`:1549`），后果与缺告警未登记 | 很可能 |
| r2-low#2 | 合服 P3 覆盖校验用 describe 时刻的 LAG，不比较 CURRENT-OFFSET 与新取的 latest；三次读之间有写入时会放行 | `tools/merge_zone/preflight.go:92-116`、`:307-321` | 要 P3 期间仍有生产者往 src 的 db_task 写，zone-down 前提下没有 | 未登记 | 很可能 |

## 4. 已排除（尝试推翻成功）



**第二波：**

- **§2.4 cpp-economy#0 预检到铸号之间被别的系统耗号**：从 `HasItemGuidsForDrops`（`player_battle.cpp:1764`）到 `AddItems`（`:877`）在 scene 单逻辑线程上同步执行，期间只有 AddCurrency（用 txlog 号段）、HP 与宠物回写、扣药，都不铸 item 号；收件箱、资产通道、商店、神秘商人插不进来。
- **`MaxNewInstancesFor` 上界不够**：主背包放不下时在 PlanEviction 就拒，早于铸号（`bag_system.cpp:665-689`）；不可堆叠按件计，多 config 求和，同 config 多行在 uint32 上饱和。
- **cpp-economy#1**：封禁判断在动账之前（`currency_system.cpp:135-150`）；解封后不补发是设计取舍。冻结期间 `PrepareBattle` 拒绝带 `PlayerFrozenComp` 的玩家（`player_battle.cpp:1187`），开不了下一局，不会覆盖 pending。
- **正常路径重复派发**：发件箱去重键 `{battle_id, player_id}`（`battle_room_manager.cpp:2190`），scene 侧账本判重，回执后 `DispatchStagedSettlement` 不再写 pending（`:2119-2125`）。
- **结算部分应用后崩溃重投**：账本与货币、背包、任务、宠物在同一次 Marshal（`player_database_loader.cpp:252-274`），同生共死。
- **神秘商人购买三步原子性与共享库存**：发物、扣钱、记名额同线程同步完成，三者同在 player_database 一行同次落盘；scene_manager 只写 Redis 读模型，没有跨服务扣减，不存在超卖或"扣了钱库存失败"。冻结闸（`IsCrossZoneFrozen`）与退出闸（`scene_handler.cpp:273`）都覆盖；时钟回拨与 Redis 键丢失都不会重置限购。
- **合服时玩家在线或有在途 DBTask**：P2~P6 在围栏之下检查场景节点、Kafka LAG、retry / processing / dead 队列、`player:save_pending` 64 桶、`lock:player`、`player:session`，任一不过即拒（`merge_run.go:279-292`、`preflight.go:832-924`）。先拷数据后改映射的顺序成立；copy 拷行对目标库已有行拒绝写入，撤销只删与源库逐字节相同的行。
- **login 账号目录写者不一致**：建角、入场自愈、外观修复、运维摘孤儿四个写者都先拿 `account_lock:create:{account}`，提交点用 Lua 核对令牌（`createplayerlogic.go:122`、`account_self_heal.go:43`、`player_class_backfill.go:114`、`admin/remove_players_from_accounts.go:100`）。**第一轮 §4「账号 blob 有两个写者不走建角锁」在当前代码里已不成立**，第三波 low 复核时据此关闭。
- **login kill switch 先产生副作用再拒绝**：拒绝点在 `resolveAccount`、账号锁和全部 Redis 写之前（`loginlogic.go:69-76`）。
- **login_session 新键跨账号继承**：带代次的连接只读 `{gate_instance_id}:{incarnation}` 键，不回落旧键（`loginsession.go:169-174`）；旧版 gate 兼容键属已登记的混版残余。
- **路由对账用旧映射盖新映射、双 leader**：leader 只写 Redis 副本，从不写库映射；发布按 rev set-if-newer（`shared/placement/route.go:350-359`）；库侧写全部带租约谓词。只按库补 Redis，不从 Redis 补库。
- **字段保护的 C1 延后造成旧盖新**：dual 下行内围栏判 writeOlder；C1b 不误拒合法存盘（两表盖同一戳、只升不降）；两表同 Kafka key 同分区；merge_zone 用 INSERT…SELECT 并校验两库列集合相等。`save_barrier_restore` 接受 `schema_gen` 后仍不会恢复旧屏障。

**第一波：**

- **data_service `LoadPlayerData` 同读**：指定字段时版本和字段在同一条 MGET 里读（`data_logic.go:144-166`）；已建索引时全量读在一段 Lua 里（`:186-212`）；SCAN 回退时版本变了就重读（`:362-366`）；一次 RPC 只解析一次 client。默认部署配了 DevRedis，`ClientForPlayer` 第一行直接返回（`router.go:495-498`），路由切换类问题今天走不到。
- **data-service#1 修复**：uint64 字段按整型解析，行值走 `json.Number`，`-player` 换号时一并改写匹配列，bundle 模式禁止 `-player`。修得对。
- **route_reconcile 的 leader 原语**（AdvanceReconcile / FreezeByLeader / ScanRoutesUpdatedSince）在仓库里没有调用方；unfreeze 与 leader 再冻结的竞态方向是 fail-closed，且未接线。freeze / unfreeze / rewarm 都是单步提交或原子 DEL，中途取消最多留下"冻结"。
- **go/db 世代门禁**：dual 只写标记、运行中不复查，Redis 丢键、首启、新建 zone 都不会让 dual 拒启或停机；legacy 拒启是有意的 fail-closed。
- **补投旧快照**：dual 行内围栏判 writeOlder 后只确认，不发布缓存、证据和 ACK；ACK 只升不降（`player_cache_publish.go:46`）。补投与在线新存盘并发时新版本赢。
- **终态误标**：MarkTerminal 要求 pending、outbox_version、任务版本三者相等，合法的当前版本标不上终态。
- **route_ready_gate**：缺省 `RequireRouteReady: false`；关闸时不取新消息，位点只按连续前缀提交。
- **barrierrestore**：只从 SQL 围栏取版本；Lua 原子校验父子缓存、证据、handoff 不存在、64 桶全空、屏障缺失或三字段相等、owner 未变且无 TTL；缺省 plan 只读，apply 要计划 SHA。不会用旧值盖新屏障。
- **task_result_notify**：只用于读回执的通知，写入是否提交只看屏障 ACK。
- **session_reconciler**：只改会话（整字节 CAS 转 DISCONNECTING，`session_reconciler.go:705`）和 gate 索引（字节守卫，SCARD==0 才 DEL），不碰 location、owner_epoch、handoff。
- **"持有者永不释放、只能等 600s"**：旧会话的清理与更新的会话不能并存（`session_cas.go:39-62`、`markofflinelogic.go:166-170`），不可达；代次 0 只在混版窗口。
- **C++ 收到释放时实体在加载、交接或退出中**：加载中和改派待确认判 `kSkipLocalHolder`（`player_lifecycle.cpp:5408`），交接中判 `kSkipHandoffInFlight`，退出中只合并原因；A1′ 只在落地后写，与 DestroyPlayer 在同一个函数里。
- **CAS 口径**：存盘 Lua 用 `owner ~= ARGV[2]` 严格相等（`redis_client.h:153`），与 `release_fence::Decide` 一致。
- **EnterScene / CreateScene 应答**：改成每次调用按值捕获 playerId 和关联号，回显值逐字段核对，实体重建后应答会被丢弃。
- **登录准入**：`loginSucceeded` 按会话代次核对，断线重连是新会话，没找到绕过方式。
- **dev yaml 的 `AllowUnsafeCrossNodeHandoff: true`**（`scene_manager_service.yaml:62`）会关掉整个修复；K8s ConfigMap 模板不写这个键，取代码缺省 false；已登记（事故报告 §7.2）。

## 5. 修复方向

| 编号 | 修复方向 |
|---|---|
| r2-gate#1 | LeaveGame 回包成功时再执行一次 `ResetSceneRoute`，`loginGenerationAtReset` 取回包携带的 login 侧当前代次；或在 lane 里有在途或排队的 EnterGame 时延后钩子或拒绝这次 LeaveGame。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：两种建议都没用。回包是客户端可见的 `LoginEmptyResponse`，带代次就要改客户端契约；只看 lane 盖不住 login 后台链，因为 EnterGame 的回包早于 Bind 发出。改为只改 gate：login 只在处理 EnterGame 时发 Bind，所以重置之后、本连接下一次 EnterGame 收包之前到达的 Bind / 路由一律丢弃（Bind 入口新增过滤）。之后按被离开的那条链认旧链：角色加代次，代次随丢弃抬高，另加「比会话已见过的更新登录旧」一条。残余：旧链的 Bind 与路由都晚于下一次 EnterGame 收包、又早于新登录任何事件时认不出。回归用例 `cpp/tests/routing_identity_test/routing_identity_test.cpp` `SceneRouteEntry.OldChainBindArrivingAfterLeaveGameIsDroppedAndNeverReplayed` 等 5 条；说明见 [location-release-binding.md](location-release-binding.md) §3.5「旧链的 Bind 晚于 LeaveGame 到达」与 §14。Java 版：不适用，结构上不可达（2026-10-19 只读核对 Java HEAD `0e4a613`，未改代码）。Java 的绑定只随同一会话 EnterGame 的 login 同步应答下发，gate 按会话串行调 login，LeaveGame 排在 EnterGame 应答之后，没有异步 Bind 或路由事件；超时的迟到应答由 Dubbo 丢弃。迟到的 scene 帧按 (节点, 链路代次, 玩家, epoch) 被丢弃，加载中离开会取消进场。Java 没有待做项，`PARITY.md` 不登记；推导见 [location-release-binding.md](location-release-binding.md) §14.4 |
| r2-db#1 | broker 确认之后才计数（失败回退计数），或发布失败时整拍暂停认领。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：两种都做。认领不再计次，两条任务都经 broker 确认后才由 `confirmAttemptScript` 按版本加计数 CAS 计一次；发布失败不计，并让补投器暂停 `RetryAfter`；意图本身不合法仍计次。回归用例 `go/db/internal/saveoutbox/replay_r2_regression_test.go`；说明见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「2026-10-19 第二轮审计修复」。Java 版不适用：不经 Kafka / go/db 落库，没有补投器 |
| r2-sm#3 | LeaveScene 只认带 etcd 正面死亡证据的 `playerLocationOwnerDead`。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：照此修。`judgeHolderRelease` 只用 `playerLocationOwnerDead`，真正下线的 zone 在 etcd 里已注销，照样立刻删；拿不准的落到 600s 上限。EnterScene 陈旧位置判定里的 `playerLocationOwnerGone` 不在本批，登记备查；**同日另批复核成立并已修（未编译、未测试，待 Codex）**：改为先要 etcd 注册表里该 zone 已无任何节点的正面证据，合服后源区照常放行，见 [location-release-binding.md](location-release-binding.md) §14.5。回归用例 `go/scene_manager/internal/logic/leave_release_test.go` `TestLeaveScene_MissingLoadSetIsNotOwnerDead`、`TestLeaveScene_OwnerGoneFromRegistryDeletes`；说明见 [location-release-binding.md](location-release-binding.md) §3.1、§14。Java 版不适用：没有 scene_manager LeaveScene 释放判定 |
| r2-sm#1 | forced / released 指标与 ERROR 只在 `outcome==deleted` 之后记。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：照此修。返回 0 / -1 时只打 INFO，不计判定、不打 forced ERROR；等待类照旧每次回拒都计。回归用例 `TestLeaveScene_ReleaseVerdictCountedOnlyWhenDeleted`（forced→superseded、released→already_gone）；说明见 [location-release-binding.md](location-release-binding.md) §3.2、§14。Java 版不适用 |
| r2-sm#2 | 等待计时改用 Redis 侧时间，例如带过期的计时键。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：读值、比 scope、登记和求差合进一段 Lua（`luaNoteLeaveReleaseWait`），时钟取 Redis `TIME`，不再读任何副本的墙钟；键名、值格式与 TTL 不变。Redis 时钟回拨时钳 0，只会多等。回归用例 `TestLeaveScene_WaitTimerUsesRedisClockNotReplicaClock`、`TestNoteLeaveReleaseWait_UsesRedisClock`；说明见 [location-release-binding.md](location-release-binding.md) §3.6「时钟语义」、§14。Java 版不适用 |
| r2-db#2 | 降级步骤加"该 zone 的 topic 积压为 0"；或 legacy 收到带 (owner_epoch, save_seq) 的两表写时按冲突停机。**延后（2026-10-19）**：只在 dual→legacy 降级或混跑窗口出现。按 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「上线状态」，项目尚未上线，没有可降级或混跑的旧部署，上线前不阻塞，本批不改代码。上线后若要降级，先按本行补 §8.6 的「topic 积压为 0」步骤。Java 版不适用 |
| r2-db#3 | 戳超过 now+窗口视为非法并截断；或改用 Redis TIME 打戳。**已修（2026-10-19，Claude，复核成立，未编译，待 Codex）**：取前者。在途戳改为最后一次写入为准，认领时超过 now+`InflightWindow` 或格式不对的戳截掉，单个未来戳的推迟上限为两个窗口。没用 Redis TIME，理由是调度分数用 go/db 时钟，换钟只会把偏差转移到 Redis 与 go/db 之间。「C++ 写新版本不清戳」维持现状：戳按玩家表示有重试链在途，截断后影响有上界。回归用例与说明同 r2-db#1。Java 版不适用 |
| r2-ds#1 | 任一侧身份取不到时 fail-closed，除非显式加 flag；TiDB 改用可取到的集群身份，或要求目标库带本地环境标记。**已修（2026-10-22，Claude，复核成立，未编译，待 Codex）**：取「本地标记」。每个写目标都要证明不是导出来源：两侧身份都取得到且不同 → 目标带本地标记（库为 `debug_import_local_target` 表，Redis 为 `debug_import:local_target`）→ 显式 `-allow-unverified-target-identity` 并逐个打 WARNING；都没有就拒绝，身份相同一律拒绝。TiDB 一律按身份未知（集群级身份未核实，不猜）；评审补严：Redis 的 run_id 只标识进程，HA 切主后会变，不再算证明；MySQL 按 zone 库 / 全局库角色配对比对；`player` / `playerdb` 旧模式同样要求标记。判定在 `go/data_service/cmd/debug_import/target_identity.go`，手册 `docs/ops/online-debug-data-fetch.md` §4、§7。Java 版：TiDB 闸已有等价，Redis 侧仍用 run_id，Java 待做，已登记 PARITY |
| r2-ds#2 | E 取 max(线上, 本地 owner, 本地两行围栏, 本地屏障令牌)+1；或本地行围栏大于 owner 时拒绝导入。**已修（2026-10-22，Claude，部分成立，未编译，待 Codex）**：本地两行围栏已由 `7d9208237` 并进公式；本次补本地屏障令牌（pending / 两表 ACK / outbox_version），并在本地痕迹高于本地 owner 时拒绝导入（FLUSHALL 后 Kafka 残留 DBTask 的代次可能 ≥ max+1，单靠抬高 E 盖不住）。§3.5 疑点「debug_import 不拿账号锁整块写回 `account:{account}`」**不成立**：该键只在 debug_fetch 只读导出，导入只经 `directory.ImportRole` 与 login 建角同一个 `createRole` 事务（账号行 `FOR UPDATE`、只写本 player_id 一行）。Java 版不适用（owner_epoch 在 SQL 行里，无 Redis owner 与 write-behind） |
| r2-battle#1 | ResultCommit 按成员记落库结果，后续轮次只重写没确认的成员；落库脚本改成与 `kStorePendingSettlementIfLockMatchScript` 同一纪律，`battle:lock:{player}==battle_id` 才写，锁不在或属于别的局按"该成员已收尾"计成功（销账与删锁在同一条 Lua 里，销账之后的重写必然看到锁不在）。补单测：第 1 轮部分失败、期间对 A 销账，第 2 轮之后 A 的 pending 必须不存在。全球匹配开通前必修。**已修（2026-10-19 落码，10-22、10-23 两轮评审跟进；Claude，复核成立，未编译，待 Codex）**：(1) 照做，`ResultCommit` 按成员记结论、重试只写没落定的成员；(2) **没有采用纯锁纪律**：锁 TTL=deadline+60s、续期 fire-and-forget，落库前可能合法缺席，「锁不在=已收尾」会静默丢合法结算。改用本局落库标记 `battle:settlement:staged:{battle_id}` 判重（标记与 pending 同一条 Lua 写下，销账脚本不碰它，已落定成员返回 2 不重写）；锁属别局返回 3 不写；10-23 评审补槽位守卫：锁不在而 `pending:id` 属于别的局时同样返回 3，防旧局覆盖新局。真 Redis 回归 `PendingSettlementContract.ConfirmedStagingRetryNeverRevivesAcknowledgedPending`、`ConfirmedStagingWithoutLockNeverOverwritesAnotherBattlesPending` 等。残余：低频阶段落定的局对在线成员需 cross-service-assets#1（另一会话修 player_battle.cpp 在线无组件分支）落地后才闭合。说明见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6。Java：确认模式落库本身待做，已并入 PARITY §2.4 行 |
| r2-mz#1 | 前向 copy 合服不删 `save_barrier`（两表逐字节拷贝、带行内围栏，原 pending / ACK 对 dst 行仍成立），只删父缓存、子缓存与资产证据；撤销保留删除，但 runbook §10.1 补"保持维护闸 → `save_barrier_restore` 重建 → 再开服"，并让恢复工具接受"无落点记录且 home==zone"的玩家；`-verify-merged` 断言 owner≠0 的清单玩家屏障存在且与两表 ACK 同版。首次 copy 合服前必修。**已修（2026-10-20，2026-10-22 评审跟进；Claude，复核成立，未编译，待 Codex）**：照此修。前向 copy 改调 `invalidatePlayerCachesKeepBarrier`，撤销仍删；`save_barrier_restore` 新增只在 plan 生效、计入计划 SHA 的 `-allow-home-fallback`（同实例时写屏障的同一段 Lua 原子复核落点记录仍不存在）；runbook §5、§8 Step 5、§10.1 同步。评审跟进：10-22 起 login 冷加载会按 `savebarrier.Reconcile` 采纳 SQL（V2 / V7），`verify:save_barrier` 改为照它预判，分 info / warn / block 三档，`barrier_missing` 按行状态拆分（无 SQL 行即 r2-login#1、零围栏需 `-allow-legacy`），新增共享判定样例表 `go/shared/savebarrier/testdata/merge_copy_cold_load_cases.json` 由 merge_zone 与 login 两侧单测各跑一遍；保留屏障的真正价值改写为「目标库行比 ACK 旧时判 SQLBehind 拒绝，而不是被静默采纳」。残余：每个 auditor 30s 预算在 `runAuditMode`（D1 维护），大清单可能报 INFRA（fail-closed）。说明见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末 2026-10-20 / 10-22 附注。Java 版不适用（无 Redis 父快照屏障，合服作业随 7.3 待做） |
| r2-ps#1 | 首次上线前把 `proto/persist_schema_gen.txt` 切到 `state=active`，写进 `docs/ops/release-checklist.md`；或让生成器在 bootstrap 状态下遇到新增字段时失败。纵深防御：在世代号之外再带持久字段集合指纹，世代相同、指纹不同时延后或失败。**已修（文档门禁，2026-10-20，Claude；与混版本会话商定只改文档）**：没有现在就切 `active`（切换时机由负责人在首次上线、关闭引导期之后决定，设计 §13 已拍板），也没有让生成器在 bootstrap 下失败（会阻断上线前的正常开发）。改为两道人工门禁：[release-checklist.md](../ops/release-checklist.md) 新增 §6.2「首次上线后：关闭持久 schema 引导期，台账切到 `active`」，§2.1 ⑦ 加发布前台账比对（有新增字段行而仍是 `bootstrap` 就停止发布）；[mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md) §8.1 追加第 6 条说明「同为世代 1 的两个二进制字段集合可以不同」。**机器门禁仍待办**（字段集合指纹，或发布预检比对线上与待发布台账）。只改文档，无需 Codex 验证。Java 版不适用：Java 无 go/db 列写与世代台账（混版本设计 §11），PARITY 不登记。**严重度裁决（2026-10-24，主会话）：medium。** 后果是上线后滚动发布时静默丢新列、可能重复发放限购或幂等账本，按"默认可达的丢资产"口径本应 high，只在混版窗口出现降一级；现在由发布清单人工挡住，机器门禁未落地前不降为 low。同根因的"主干并发发号"另列 r2-ps#10 |
| r2-battle#2 | 停机路径对没落完库的房间补发一轮落库并有界等待；或在 §13.6 明确接受同局成员不对称，并按 battle_id 给出对账或告警口径。**已修（2026-10-23，Claude，复核成立，未编译，待 Codex；独立评审因周额度推迟）**：工作区并行 v1 终局接缝轨道（全球局只发 v1；match 单事务 COMPLETE + 发件箱；截止后 `abortTerminalTx` 原子 ABORT）已让「A 已领奖、B 永不结算」不可达；本批补优雅停机有界补提交：`AbortAllRooms` 对已冻结终局、未拿到 SQL 决定的房间续发 `CommitBattleOutcome`，`Node::SetShutdownDrainComplete` 有界等待，停止闸移到 `SetBeforeTeardown`，仍未落定的局打 `metric=battle_sql_outcome_unflushed_at_shutdown`；崩溃路径由 match 发件箱补完或截止后 ABORT。**前提**：并行 v1 轨道必须整批验收、整批提交，否则退回 HEAD 形态。残余：停机时还在打的局整局作废；battle 仍接受 protocol-0 全球 Create（match 已不发，未改）；停止闸移位与 v1 轨道验收探针口径冲突，待裁决。说明见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6「2026-10-23 修复」。Java 待做，已登记 PARITY |
| r2-battle#3 | 预检判出不足时调用号段客户端的续段入口（例如新增 `RequestTopUp(needed)`，没有下一段、没有在途或退避时立即取段）；资产通道 `ApplyCredit` 预检同改 |
| r2-battle#4 | 落库轮次设上限，到上限按"终局未知"保留房间并打告警指标或转人工；不要"放弃落库直接发结果"，那会退回原缺陷。**已修（2026-10-19 落码，10-22 评审改进；Claude，未编译，待 Codex）**：快 24 轮（1→30s）后每 5 分钟低频重试，6 小时总跨度兜底（与 24h 落库标记 TTL 保持 4 倍余量，static_assert），用尽后保留房间、不发结果；滞留房间每 60s 报 `metric=battle_personal_settlement_staging_exhausted_rooms`（含 sql_unconfirmed / placeholder 两类），处置为排空三步（`mmorpg.io/drain` → 等其余房间结束 → 删 gs）。说明见 [cross-zone-matchmaking.md](cross-zone-matchmaking.md) §13.6「2026-10-22 修复」 |
| r2-ps#2 | 收件箱执行器在调用 `AttachDebt` 前先查同币种已有欠款是否带未知字段，带则 Deferred；或 `AttachDebt` 返回专用码、由 `FromTip` 映射成 kDefer。另需区分"新版本字段"与"已退役字段残留"，否则字段退役后守卫永久误拒 |
| r2-ps#3 | 把引导关闭事实持久化到不随 zone Redis 丢失的地方（etcd、zone 配置或 SQL 元数据表），任一来源为 closed 即 closed；告警加各 Pod 不一致规则；更正注释与 README:2396 |
| r2-ps#4 | 提供单玩家恢复工具：确认写方已下线后，按 task_id 豁免 C0 或丢弃该版本，并原子地把屏障 pending 回退到 SQL 已提交版本、清理 `outbox_*` 与该玩家的索引条目；关闭引导期前扫描待补投索引、重试队列与死信里的未盖戳任务；更正设计 :346、:370 |
| r2-ps#5 | `bag_system.cpp:647-648` 与 `player_equip.cpp:442` 改用 `IsIndependentInstance`，让头注释所说的"三处共用"成立 |
| r2-mz#2 | runbook §5.1、§11 改为：copy 模式下只有 `merge-zone-storage-audit -MergeAuditStorage <src>` 判可退役，源区库才能归档；合服结束时 `placed_skipped>0` 且有记录指向 src 时打印显式警告并写进清单 |
| r2-rr#1 | cvm 只推进到本轮最早未决行的 `updated_at_ms` 之前，或把未决 id 落库；换届后有未决记录时先复读再信任标记；增量出现未决时按策略撤标记 |
| r2-rr#2 | Arm=false 的追平起点改为 `satSub(min(水位键水位, cvm), lookback)`，或身份变化时一律从 0 全量 |
| r2-rr#3 | 检测到三键清空或水位键缺席的那一拍，先经 `AdvanceReconcile` 把 `cache_verified_ms` 写成 0，再 SET NX 水位键；或 Arm=false 下 VI 为空时暂不 SET NX，等全量追平完成再写。补用例：清空 → 追平中途换届或推送报错 → 下一拍起点仍为 0 |
| r2-rr#4 | `MirrorMergedInto` 对 SET NX 成功的键再复读一次库，库里已无此行就按刚写的完整值 CAD 撤掉；或由 leader 对"库里没有、值为本方案编码"的镜像做受控删除 |
| r2-mm#1 | 间隔不一致且旧轮仍在停留期内时，scene_manager 不强制覆盖，等旧轮结束再写新轮；或玩家记录按轮次保留最近 2~3 轮，`RecordPurchase` 不再整份清空。Java 版实现时一并规避 |
| r2-lc#1 / r2-lc#2 | 在存储侧用围栏令牌拒绝迟到的旧标记写，不依赖连接顺序或墙钟。**已修（2026-10-20～24，未编译、未测试，待 Codex）**：围栏主体由其他会话随 `9d2784fbf` / `526d0895f` / `693781d90` 提交，做法是每玩家永久资格 `save_barrier.handoff_fence`（E\|S\|N\|A\|K\|T）。发布、A1′、A2′ 放弃补写、疏散改派、改派补写只消费缓存的原资格做 CAS；取证在同一原子步骤把资格撤成 R，恢复为新资格后才解冻；同节点重载按 prior 做 CAS 并原子清标记。本会话复核成立，在其上补了：会话结束凭证改为按观察到的原件做带围栏的 CAS（评审必修项，原先"一律拒发"会让 location 永不释放）；Go 与 C++ 回归，Go 按 Redis 实际执行顺序重放两种迟到序列，C++ 用两条真实连接、旧帧最后才发出；去掉代码与文档里"依赖同连接 FIFO"的说法。说明见 [cross-zone-scene-travel.md](cross-zone-scene-travel.md) 文末写入口对照节、[location-release-binding.md](location-release-binding.md)、[shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §20.6。残余：Redis 自身回退丢掉已确认的资格写（归 zone Redis 回退防护）；围栏脚本 KEYS 跨 hash tag，上 Cluster 前要改形。Java 版不适用：Java 交接是一笔 MySQL 交出事务，没有这条标记链 |
| r2-login#1 | login 在"无 pending、owner≠0"时不信父键、走 SQL 核验；SQL 两行为空且父键是建角种子或缺失时，按"铸过 owner、从未存过盘"的新号放行。**已修（2026-10-23，「设计 zone Redis 回退时的存档防护」会话，未编译、未测试，待 Codex）**：`go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go` 新增快照字段 `unsavedNewPlayer`，历史缺口一律走 SQL 核验；SQL 有非零围栏时按 `savebarrier.Reconcile` 判定（同版本采纳 SQL，版本不同可重试拒绝，ACK 高于 SQL 时 fail-closed）。用例在 `parent_load_identity_test.go`。说明见 [zone-redis-regression-guard-20261022.md](zone-redis-regression-guard-20261022.md) §4.1。恢复工具对没有 SQL 行的玩家仍拒绝，登录侧放行后不再需要它 |
| r2-guild#1 | 给 formation=4 且确认无结果的局一条可证明的终局：match 在确认战斗已不存在时把编队迁到"已开局但作废"的终态并签进取消回执，guild 据此终结 launch；或 battle 停机作废时对带活动上下文的房间落一份作废结果。另补带审计的人工释放 CLI，更正 `06-activities.md` §6.21 的 R1 / R2。**归「数据覆盖审计交接文档」会话修**（与 social-match-battle#0、r2-battle#2 同一批文件），修复状态以该会话在本表的标注为准。**已修主路径（2026-10-23，Claude，复核成立，未编译，待 Codex；独立评审因周额度推迟）**：formation 4→5 的可证明终局由并行 v1 终局接缝轨道的 `abortTerminalTx`（截止后原子 ABORT，原票置 4）与 `CancelActivity` 提供；本批把 guild 释放规则收成一处 `data.TrialCancelReceiptIsTerminal`（只认零 battle 取消墓碑，或 formation=5 且不是 COMPLETE，矛盾回执 fail-closed），recoverOne 与 `FinishCancelledTrialLaunch` 共用；battle 停机补提交见 r2-battle#2。按「只依据权威终态释放」不改：解散释放仍为空函数、`ExpireTrialBattle` 不碰 launch、`checkWork` 不放宽。**未做**：带审计的人工释放 CLI。`06-activities.md` §6.21 R1 / R2、`account-role-authority.md` 已更正。真 TiDB 回归 `TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort`、`TestStartedActivityFormationTiDBCancelSignsAbortOnlyAfterDeadline` 等。前提同 r2-battle#2。Java 待做，已登记 PARITY |
| r2-match#1 | 负责人 2026-10-23 拍板首发开全球匹配（见 §3.7）。r2-battle#1/#4 的 Codex 验证列为上线门禁，social-match-battle#0 与 r2-battle#2 列为上线阻塞，归「数据覆盖审计交接文档」会话 |
| r2-rr#5 | 数据侧与可用性侧分开修。**数据侧已修（2026-10-23，本会话，未编译、未测试，待 Codex）**：saveoutbox 补投器经显式注入的只读接口 `RouteGate` 看本实例的就绪闸，闸关时整拍跳过，不认领、不发布、不计次，计 `db_save_outbox_total{result="route_not_ready"}`；本拍中途关上时剩下的玩家不再认领；闸重开后从原到期时刻接着补投。跨 zone 迁移读 home_zone 时，传输类错误不计次，键缺失与 WRONGTYPE 照常计次。评审修一轮补了追赶期收敛条件（重开后放大写入速率须低于消费能力）与退场说明（负责人 10-23 定映射改为直读 TiDB 后，`RouteGate` 接缝随就绪闸同批删除）。说明见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「2026-10-23 第二轮审计修复（r2-rr#5 数据侧）」。**可用性侧仍开**：对账 leader 重挂标记要扫完全键空间（`routereconcile/reconciler.go` 的 tryArm）、GoDbRouteGateClosed 与 RTO 说法，归 D1 映射持久化会话；映射直读 TiDB 后随就绪闸一起作废。后续任务入口：补投器按本实例消费积压暂停。Java 版不适用 |
| r2-mail#1 | 附件非空时强制要求显式 `-request-id` 与 `-expire-ms`，否则拒绝；或在结果未知时打印带这两个参数的完整重试命令 |
| r2-mail#2 | rank 发件时取 min(封榜 +30 天, now + mail 当前 TTL)；对 BoxFull 与"已超出可投窗口"设终态并计指标；mail 的 TTL 下调写进发布检查。与一致性核对 social-global#2 一起修 |
| r2-ps#6 | `AddCurrency` 遇到带更新版本字段的欠款时本版本不扣还、不删除（按冻结处理），打 WARN 与计数；或在设计 §4.4 登记"欠款自动扣还：暂无守卫"。混版本会话第 6 批的"按数量扣除守卫"可能已覆盖，以该会话 PROGRESS 为准 |
| r2-ps#7 | `RecordPurchase` 换轮改为 `record.Clear(); record.set_round_id(roundId);`，并在设计 §4.4"逻辑重置"一行补上 |
| r2-ps#8 | 加载时不改存储的 rank（运行期 `EffectiveRank` 已夹住），只打 WARN；设计 §12 补"技能等级 / 技能分类"一行，release-checklist 增加持久值域与 scene 回滚门禁。「核对 §12 未核对的持久值域删数据点」会话在处理同一族问题 |
| r2-ps#9 | **已修（混版本会话第 6 批 6c，未编译，需 Codex 先跑 proto-gen-build / proto-gen-run）**：生成器把台账 retired 行输出成 `persist_schema::kRetiredFields`，`unknown_field_carry::CarriesNewerVersionFields` 只认非退役字段，合堆（`ItemStore::IsIndependentInstance`）与欠款合并（`CurrencySystem::DebtCarriesUnknownFields`）都改用它。PROGRESS 条目「2026-10-23 Claude：混版本持久字段保护第 6 批」 |
| r2-ps#10 | 补 r2-ps#1 的字段集合指纹门禁；release-checklist §2.1 ⑦ 增加"新增行的世代必须大于线上最大世代"；台账头注释与设计 §3.1 写明"台账冲突时丢弃本分支新增行，在合入后的主干上重新生成" |
| r2-dcc#1 | 新增 `-guild-redis-addr` / `-guild-redis-db`（缺省 DB 2），榜单检查改用这个句柄；guild 表读不到时报 fail 或 warn「NOT CHECKED」，不再报 info |
| r2-low#1 | 更正 `scene-owner-reentry-barrier.md` §6.2 与 `db-write-fencing-mysql.md:1058`；把 `missing owner` 错误回复单独计数并配告警；写在线玩家受控恢复手册。并入 zone Redis 回退防护的统一设计 |
| r2-low#2 | P3 把 `row.current != high` 也记为问题；或把 describe 放到 get-offsets 之后再跑一次 |

## 6. 后续各波（待周额度重置）

**2026-10-24 进度**：medium 的复核改由各修复会话按"先复核成立才修、修后独立评审"完成：r2-lc#1/#2、r2-rr#5 本会话修复与评审都判成立；r2-login#1 由 zone Redis 回退会话修；r2-guild#1、social-match-battle#0、r2-battle#2 由「数据覆盖审计交接文档」会话修；r2-match#1 已由负责人拍板；r2-ps#1 由主会话裁决为 medium（§5）。所以下面"对抗复核"一项只剩换视角查找新出的 high / medium。**剩下的只有换视角查找**：周额度 81% 时停下（会话组约定超过 80% 只做风险最高的事，这一轮给了改到一半的 r2-lc 续做），等 2026-10-29 20:00 UTC 重置后用下面的脚本跑，`args` 为 `{"dRound": 0, "seenTitles": [...]}`，已知问题清单在 PROGRESS 2026-10-24 条目所指的 §3、§5 里，跑完一轮看新发现再决定下一轮。

**2026-10-22 进度**：第三波 10 路与第一轮 22 条 low 复核已做完（§3.6–§3.8）。还剩两项，停在 5 小时窗口 89%（与约 10 个会话共用），等窗口重置后续跑：

- **换视角查找**：下面原表 (d) 的 5 轮视角（脚本 `D_ROUNDS`），每次运行一轮，连续两轮无新发现才停。
- **对抗复核**（每条两个视角：代码路径与时序；可达性、上游守卫与严重度裁决）：r2-lc#1、r2-lc#2、r2-login#1、r2-guild#1、r2-match#1、r2-rr#5，以及 r2-ps#1 的严重度裁决（medium 还是 low），再加换视角查找新出的 high / medium。r2-battle#1、r2-mz#1 已由修复会话按成立处理并落码（未编译），不再对原发现做对抗复核，修复本身留给冻结后增量审计。
- 续跑脚本：[handoff 目录的 audit-workflow-wave34.js](../handoff/data-overwrite-audit-r2-20261019/audit-workflow-wave34.js)，按 `args` 选路（`wave3Keys` / `lowReview` / `dRound` + `seenTitles` / `verify`），单次最多 6 个代理。

**节奏**：第二波结束时周额度 93%，要到 **2026-10-24 22:00 UTC** 才重置。按会话组约定，周额度超过 80% 时只跑风险高的一波，第三、四波等重置后再跑；每波不超过 6 个代理，开跑前和每个代理返回后都查用量。

| 波 | 内容 |
|---|---|
| 三（补覆盖与余项增量） | (a) C++ 收件箱、`id_segment_bootstrap`、`asset_op_system`、trade `asset_op_repo` 与 engine、friend 清扫、邮件清扫与 `mailadmin`（原第二波计划，未跑）。(a) 第二波提前收尾留下的未读部分：merge_zone 的 `route_store.go`、`route_publish.go`、`fence.go`、`relocate_run.go`、`unmerge.go` 正文、帮会合服围栏、合服后 ID 撞号与号段续接、`data_consistency_check`；login 的 loginqueue、dataloader、`node_cache` / `watcher`、`entergamelogic.go` :420 之后；字段保护的生成器其余部分、C++ 运行期整体替换持久组件的路径、`bag_marshal`；路由对账的 W1 发布失败语义、`route_read` / `route_reads`、`RegisterHome` 等正文。(b) player_lifecycle 未读段（交接取证、改派待确认表、EnterScene 应答分派、`HandlePlayerAsyncSaved` 等）与 `redis_manager` 改动；各业务持久组件写入口 vs 冻结闸 / 退出闸；帮会逻辑正文与 `k8s_zone_rollback.ps1`；组队匹配、评分排行与 go/db 的 `durable_enqueue` / `retry_stranded` 余项 |
| 四（复核与换视角） | (c) 第一轮 §4 的 low 复核（SanitizeSkillList 两区重复报告算一条，共 22 条；login 第 1 条已由第二波证实不成立，见 §4）；(d) 换视角连续查找（资产追踪、按故障、按入口，连续两轮无新发现才停）；对抗复核 r2-battle#1、r2-mz#1、r2-ps#1 及新出现的 high / medium |

- **从第四波撤下**：原计划对 r2-gate#1、r2-db#1、r2-sm#3 三条修复缺陷的复核。这三条与 r2-sm#1、r2-sm#2、r2-db#3 已由两个修复会话各自复核并落码（见 §5，未编译），改为在代码冻结后的增量审计（第一轮 §8 第 5 步）里查。
- **避开在途文件**：上述两个修复会话正在改 scene_manager、gate、go/db saveoutbox；第三、四波在它们交付前不审这些文件。
- **续跑方法**：见交接文档 [handoff/data-overwrite-audit-r2-20261019](../handoff/data-overwrite-audit-r2-20261019/README.md)。工作流脚本副本在同一目录（`audit-workflow.js`）。换账号或换会话后不能用 `resumeFromRunId`，要全新运行，`args` 的填法见交接文档 §5。
- **待办（不在本会话范围）**：Java 版（`D:\luyuan\xuanming-server-mmo`）同类审计，按该仓库 `AGENTS.md` 执行。

## 7. 证据与额度

- 第一波 4 个代理约 137.9 万子代理 token。开跑前 5 小时窗口 75%、周额度 74%；结束时 5 小时窗口 93%、周额度 78%（含同时在跑的其他会话）。
- 第二波：工作流 `wf_12bc0724-9b9`，6 个代理（只查找），工作流计 225.6 万子代理 token；另有 5 个收尾续跑实例约 136 万（见 §2"第二波的两份输出"），合计约 362 万。开跑前 5 小时窗口 21%、周额度 86%；结束时 5 小时窗口 45%、周额度 93%（含其他会话）。
- **教训**：给工作流里正在跑的代理发消息，不会让它提前收尾，而是另起一个续跑实例并行跑，费用翻倍。以后要压缩规模，应在开跑前就限定代理数，或直接停掉整条工作流。
- 代理完整返回保存在本会话转录里（工作流结果见该 run 的 `journal.jsonl`）；中间结论同步记在会话组共享笔记 `audit-r2-progress.md`。


### 2026-10-23 r2-lc#1/#2：迟到消息实际反证与修复状态

本轮在本机专用 Redis 上用两个真实连接及完整 RESP 帧屏障确定复现：原 publish/outcome 与 conditional/A2 两种交错均在撤标记确认后复活同 owner 的旧 `E:t`。原 7 场景已全部跑完，缺陷合同严格红；其余 owner 推进/串行/其它族反证保留。此结果证明 Redis 消息交错，不冒称已重现完整 Scene 资产回档。

当前源码修复采用 `save_barrier.handoff_fence` 永久 E/S/N/A/K/T 下限和资格转换，覆盖原 SQL 入场确认→Activate、新旧加载、复用实体、交接发布、撤销、退出/疏散/弃载补写、B4/B5 和离线屏障恢复。特别是旧加载的读 prior 本身迟到时，提交点仍核原入场 generation；只加 UUID 或 expected-prior CAS 不足以闭合。撤销不依赖 mark 已经存在，不以 TTL 解冻；丢回包只重放发送前固定候选，旧回调不修改新实体。

状态分类：**真实缺陷已复现；生产与回归正在按完整合同修复，修后当前源尚未运行验证**。旧文中“只有 mark TTL 残余”与“先解冻后 best-effort 撤销安全”的描述不再作为当前设计保证。准确状态转换、同 E 新 instance 的可用性限制和恢复停写前提以 [cross-zone-scene-travel.md](cross-zone-scene-travel.md) 本轮专节为准。原红、历史 C++ 2632 与 Unity Guild 234 原件各自保留，均不能转换成此修复的通过证明。

**2026-10-20～24 续做会话的证据与额度**：第三波第一批 `wf_25afca3a-e49`（4 路完成，字段保护 3a 一路两次撞额度失败后拆成两路重跑）；第二批 `wf_04bdef4c-8c2`（6 路）；第一轮 low 复核 `wf_1a59a221-163`（2 个 Sonnet）；修复 `wf_175199ca-4fb`（rr5 实现）、`wf_9e7dd193-c2e`（rr5 评审与修一轮）、`wf_f5a8989d-6d9`（lc 实现与评审，修一轮撞额度中断）、`wf_6be36517-646`（lc 修一轮续做）。各代理返回在本会话转录的 `journal.jsonl`。代理转录都核对过：查找与评审代理没有 Edit / Write 调用，也没有把 `git show` 重定向成文件；修复代理只改了各自范围内的文件，外加会话草稿目录里的一个 Lua 比对脚本。额度：换了三个账号，按会话组约定周额度过 80% 时停下，换视角查找推迟。
