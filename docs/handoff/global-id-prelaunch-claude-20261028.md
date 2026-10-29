# 交接：全球同服 ID 发号上线前决策与修复（交 Claude Code，2026-10-28）

> 状态：需求与审计交接，最终实现方案由 Claude Code 决定并修复。本次未修改实现、未清库。
> 主仓基线：`c370cf6cf` 及本会话工作区；进入实施前重新核对源码与其他会话改动。
> 用户最新前提：项目尚未上线，可以清理开发数据，不考虑旧 ID、历史数据兼容或迁移。

## 1. 用户已确定的要求

用户明确：“我是全球同服 MMO，同类型间不管集群不集群不能重号，Player_id 跨服合服、重新生产 player id 都不能重复。”随后明确：“我都没上线，可以清库，不要管以前的……让 Claude Code 去决定和修复。”

实施必须满足：

1. 同一类业务身份在整个游戏世界、全部 zone、全部物理集群及运行历史内唯一。物理集群不是缩小保证范围的理由。
2. 同一角色跨服、迁库、合服始终保留原 `player_id`；创建新角色必须获得从未使用过的新 ID。其他需要全局唯一的业务身份同规。
3. 删除实体、回档业务数据、进程退出、集群下线、合服或号段尚未用完，都不能使已分配的 ID 再次变成可分配资源。
4. 无法证明本次发号安全时拒发；恢复、断网和号源故障不能触发独立计数器或未经协调的回退号源。
5. 各身份类型之间允许数值相同。这是既定规则，不是缺陷，见 [现有身份域说明](../design/snowflake-id-allocation.md#cross-id-kind-isolation)。
6. 不要求连续、无空洞、跨节点按时间递增，也不要求从 ID 解析归属区或创建时间。号段浪费不等于重号。
7. 容量有明确上界与耗尽拒发语义；“永久不重用”不能通过溢出、掩码或循环计数实现。

## 2. 本任务范围与排除项

按首次上线的新形态设计和实施。允许为正确的新模型清理开发数据；本次交接没有执行清理。实际操作前核实具体 dev 目标、占用与其他会话状态，避免误删共享资源或验证证据。

不做旧 Snowflake ID 兼容、历史重号重映射、新旧号源混跑、线上灰度或为保留旧实现增加兼容层。现有 `<2^55` 上限及为旧号保留的值域是当前实现事实，不是用户本次要求；Claude 可以重新预算，说明容量、协议和各语言表示约束。

但“没有上线历史”不等于免做未来恢复设计：新系统产生第一批正式 ID 后，就必须抵抗进程重启、业务回档、旧号源快照恢复和克隆部署引起的重复发号。不得每次复现恢复问题都清库后声称解决。

不把所有名为 `id` 的局部句柄机械改成永久号段。先按实际创建、传递、持久化与销毁路径证明作用域；也不能未经证明将业务 GUID 降级为局部编号来规避全局唯一要求。

## 3. 已核对的代码事实

以下是源码检查结果，不是完整运行环境验收。路径和行号可能随其他会话变化，实施时以函数与调用链复核。

| 范围 | 当前事实与入口 |
|---|---|
| Go 号段权威 | `go/data_service/internal/store/id_segment_store.go` 的 `tryAllocate`（约 171 行）在同一 SQL 事务中 `SELECT ... FOR UPDATE`，以 version 条件推进 `max_id`，提交后交付 `[lo, hi)`。同一实际库、同一 `biz_tag` 的并发段不重叠；tag 不包含 zone。 |
| 权威库装配 | `go/data_service/internal/svc/servicecontext.go` 用 `SnapshotMySQL` 装配号段 store。K8s `tools/scripts/k8s_deploy.ps1` 的 `Get-GlobalAuthoritySqlSettings`、`New-GoSvcConfigMapYaml` 将服务绑定到指定 TiDB 入口和 `mmorpg_global`；仅配置入口字符串一致不能单独证明物理权威不会被克隆或回退，需结合现有 SQL 权威机制决定补法。 |
| 玩家 / 公会 | `go/login/internal/svc/player_id_minter.go` 使用 `player` tag；login/guild 当前 yaml 显式开启号段并关闭 Snowflake 回退。共享 `idsegment.Conf` 在整块配置缺失时仍可关闭号段，不能说所有配置下都已禁止其他号源。 |
| 其他永久域 | 当前 bootstrap 有 `player`、`guild`、`item`、`txlog`、`snapshot`、`trade_listing`、`guild_asset_op`、`friend_request`、`trade_asset_op`、`mail`、`mail_asset_op`、`rank_board`、`rank_settlement`、`rank_reward`。按实际调用者复核，不能仅凭清单认定覆盖完整。 |
| C++ 永久身份 | `cpp/nodes/scene/id_segment_bootstrap.cpp` 用 `GuidKindName` 作为 tag；物品 `item_store.cpp` 与宠物 `player_pet.cpp` 共用 `item`，流水与快照分别用 `txlog`、`snapshot`。当前生产调用未发现偷偷回退旧 C++ Snowflake 的路径。 |
| 恢复缺口 | `AllowAutoSeed=false` 只拦缺行补种，不能发现已有行水位来自旧备份。`schema.go` 的 `idSegmentFloorSources` 当前仅自动核 player/guild，`raiseIdSegmentFloor` 只查询当前库，消费表不在同库时记录 Info 后跳过。它不是完整恢复安全证明。 |
| 客户端历史 | Go `go/shared/idsegment/client.go` 的 highWater 和 C++ `guid_segment_client.cpp` 的 `highWater_` 仅在客户端生命周期内有效；重启或 Reset 后不能证明历史未发过。 |
| 运行时身份 | scene_manager 的 scene、match 的 battle/challenge/team 走 `shared/snowflakealloc`。C++ actor/skill/buff 有节点复合编号或局内计数。需按实际用途确认哪些属于本要求下的全局身份，不能因“不长期保存”就跳过跨进程使用的业务 ID。 |
| 合服 / 迁库 | `tools/merge_zone/player_rows.go` 复制前检查目标撞号，普通 INSERT 的 1062 导致拒绝；`relocate.go` / `relocate_run.go` 保留原 ID、冻结落点后搬行。既有工具按统一身份体系设计，不负责把已经独立重号的世界自动变成唯一；首次上线方案应从源头避免产生这种数据。 |

相关源码与设计：

- [号段 store](../../go/data_service/internal/store/id_segment_store.go)、[schema](../../go/data_service/internal/store/schema.go)、[Go 客户端](../../go/shared/idsegment/client.go)、[发号策略](../../go/shared/idsegment/minter.go)。
- [C++ 引导](../../cpp/nodes/scene/id_segment_bootstrap.cpp)、[C++ 客户端](../../cpp/libs/modules/id_segment/guid_segment_client.cpp)。
- [现有 ID 设计](../design/node-id-overhaul-plan-20260908.md)、[ID 现状速查](../design/snowflake-id-allocation.md)、[全区数据层](../design/global-data-layer-tidb-decision.md)。
- [落点设计](../design/player-storage-placement.md)、[合服设计](../design/server_merge_design.md)、[存盘围栏的上线状态](../design/db-write-fencing-mysql.md)。
- [现有号源恢复手册](../../deploy/k8s/README.md) 的“恢复全局库前须先核对 id_segment.max_id”。其中关于旧建角必然静默覆盖的说法不能直接当作当前事实；当前目录与建角已有冲突拒绝，仍不替代号源防重。

## 4. 请 Claude 决定并记录的设计

先在现有相关设计文档收口下面的决定，再实施。用户已给定正确性目标与上线前范围，不必重新询问是否保留旧 ID；算法、位宽和内部布局尚未由用户拍板。

### 4.1 全球身份域与号源

为每个实际业务身份列出：创建者、唯一权威、全局作用域、分配范围、耗尽行为、跨服/合服/迁库是否保持身份，以及两版实现。

优先评估沿用统一权威号段、多个 data_service 共享一份有一致性保证的分配状态。它不是要求一个进程，更不是每生成一个 ID 都远程访问。若选全球协调的命名空间/大范围委派，也必须证明同类型在全部集群间终身不重叠；命名空间不可复用，且不能与会改变的 `home_zone` 混用。不要把“各集群各自从 1 开始”作为允许的正式部署。

明确怎样防止错误库、克隆权威、两套独立可写计数状态、重复命名空间和隔离环境误接正式号源。优先复用已有 `shared/authoritysql` 与物理权威绑定、部署准入和维护机制；不要新造一套无人读取的 UUID/标记来假装已经阻止错误入口。只加 cluster_id 配置、比较 endpoint 字符串或相信备份内自带的同一 UUID，不足以证明克隆后没有重复授权。

### 4.2 请求、缓存段和故障

明确分配提交点、耐久性、超时与未知提交结果。对外交付前必须完成不会被当作“未分配”回收的范围预留；崩溃、丢响应和重复请求可以留下空洞，不能使同段给不同持有者或从头重装已消费段。可以选择不重试而浪费号段，不强制添加请求去重存储；若重试并返回旧段，必须定义请求身份、持有者 boot 与客户端消费语义。

合法缓存段耗尽后，无法领取新段就拒发。列清并移除与选定方案冲突的隐式回退、自行计数和运行期补种入口。初始化空的新世界与恢复一个已有世界必须具有可区分、可验证的权限与状态；正常启动不得把恢复问题当初始化处理。

### 4.3 恢复与回档

业务回档不回退发号权威。号源自身必须恢复时，可以选择可信且不倒退的预留水位，或者为后续新号取得全新、全球唯一且不可复用的命名空间/范围；后者只影响新发号，恢复出来的实体仍保留原 ID。独立外部账本只是候选，不是已拍板的唯一实现，也不能增加无必要的第二份业务真相。

采用单计数空间时，安全新起点满足：`new_lo >= 历史全部已预留区间的最大 hi`。`hi` 为不含上界，不需要再加 1；现存实体的 `MAX(id)+1` 不能覆盖已预领未消费、已删除和在途消息中的历史身份。

若使用额外预留记录，该记录必须真正参与范围授权、原子拒绝重叠，并不随本次恢复一起回退；事后双写一份日志或更新 `GREATEST(hi)` 不等于已防止重复授权。号源和可信恢复证据同时丢失时，应拒绝续发，不能猜起点。

恢复过程要固定预留全集、停止旧分配入口、处理在途请求，并隔离旧 boot 的业务写入及错误恢复的缓存段。只提高 SQL 水位不能使旧客户端内存中的 Next 自动失效。若已完整记录旧段且新号从最大 hi 之后分配，旧已知段与新段在数字上并不重叠；不要用错误反例代替需要的恢复围栏。

### 4.4 两版服务器

独立 Java 仓库在 `D:/luyuan/xuanming-server-mmo`，先读该仓 `AGENTS.md`、`PARITY.md` 和相关设计。两版都要满足同样的全局唯一与恢复要求，内部实现可不同；不因本任务擅自引入两版混服能力。若两版按实际部署共用同一身份空间，号源也必须协调，不能各自独立发相同号码。

Java 已确认的静态反例：`game-common/.../id/Snowflake.java` 的 `lastMs` 仅进程内，`game-discovery/.../NodeIdLease.java` 退出即释放 worker、新进程可复用。旧机器时钟快、新机器复用同 worker，时间与序号可能与旧实例历史组合重复。有效租约与进程内回拨检查不能证明跨进程安全；应在选定新方案中解决，不能只补一条进程内时钟判断。未上线，不为兼容旧 Java ID 维持不安全号源。

## 5. 实施顺序与完成条件

1. **重新核实现状。** 完成仓库启动门禁、工作区检查，按实际调用链完成身份域清单；标明业务 GUID 与局部句柄，列出遗漏入口和两版差异。
2. **记录方案决定。** 在现有 `docs/design/` 相关权威文档说明选择、容量、故障/恢复契约、真实权威绑定、需要清理的开发数据与成本。删除已不适用的历史兼容前提，保持事实只有一个权威入口；本交接不代替最终设计。
3. **先写回归，再修复。** 覆盖已识别的错误部署/恢复/重启反例，沿现有模块与接缝改完生产创建、配置、迁移、恢复和销毁路径。预计 30+ 文件等范围门禁按根 AGENTS 执行；不绕过既有安全机制。
4. **两版对齐与交付。** 按真实变更更新相关设计、运维、`docs/PROGRESS.md` 和 Java `PARITY.md`。C++/Go 按主仓 `AGENTS.md §10.1` 把构建、测试和压测交 Codex，Claude 未取得实际结果前如实标注“未编译，待 Codex 验证”；Java 按该仓 `AGENTS.md §4` 的既有授权执行，Claude 可以在 Java 仓自行构建和测试，分别报告两版实际证据，不能把主仓限制扩大到 Java 仓。

最低行为验收如下，使用隔离、确定、可重复的测试；不是只检查配置字符串或返回重复键错误：

| 场景 | 通过标准 |
|---|---|
| 多 zone / 多物理集群 / 多发号实例并发 | 同类型成功分配的区间不重叠、ID 集合无重复；各类型允许同数。 |
| 进程重启、崩溃、预取、重复响应和提交结果未知 | 已分配范围不重新从头消费；未完成操作可失败或留空洞，不能复用已授权区间。 |
| 删除实体后再创建、业务数据回档 | 新 ID 不命中任何历史已分配身份；不能只与现存行比较。 |
| 号段库恢复旧快照，其他消费库更新、旧缓存段仍存在 | 恢复门禁拒发或取得经证明不重叠的新范围；不能从旧 max_id 继续。 |
| 克隆权威库、错接新空库、重复范围/命名空间、跨集群错误配置 | 在可对外发号前被拒绝，不能以两个集群各自内部唯一冒充全球唯一。 |
| 网络分区、权威不可达、缓存耗尽、水位证据缺失 | 停止无法证明安全的发号；没有不受控 fallback、自动补种或猜起点。 |
| 值域边界、溢出、命名空间耗尽 | 明确拒发，不发生截断、回绕、归零或重用。 |
| 跨服、迁库、合服 | 同一实体 ID 及引用逐项保持原值；之后创建的新实体不重号。 |
| Java worker 复用与换机时钟偏差 | 用可控时钟和持有者切换稳定复现旧反例；修后不重复或明确拒发。 |

向 Codex 交付每一验证的工程/服务、工作目录、精确命令、环境变量名（不写 secret 值）、隔离/清理目标、先后顺序、期望产物、通过标准和失败日志位置。C++ MSBuild 串行 `/m:1`。需要真库、etcd、跨集群或完整迁库工具的场景，明确前置与现场验证步骤；单元绿不能替代这些证据。

## 6. 本次已取得证据与未做事项

主代理实际执行过：在 `D:/luyuan/xuanming-server-mmo-cpp/go/shared`，仅本命令环境设置 `GOPROXY=off` 后运行：

```powershell
go test -mod=readonly -p 1 -count=1 -timeout 60s ./idsegment ./snowflake ./snowflakealloc
```

结果三个包均通过：`shared/idsegment` 0.647s、`shared/snowflake` 11.426s、`shared/snowflakealloc` 27.446s，命令 exit 0。未启用 integration 标签；这不是实际 TiDB 并发领段、etcd 故障、号源恢复、Java、跨服迁库合服或上线容量验收。

当前功能实现没有因本交接改变，未清理任何数据库或缓存，未提交、未推送。Java 版本仍为现有 `0.1.0-SNAPSHOT`，本轮未修改 Java 实现或 PARITY，不宣称已对齐；真正修复由接手方决定和实施。
