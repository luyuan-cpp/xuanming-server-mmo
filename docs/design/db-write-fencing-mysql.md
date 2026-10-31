# 玩家存盘 MySQL 条件写（围栏）：go/db、C++、proto、schema 详细设计

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),两张表的行内围栏、Redis 屏障和条件写都已删除,写权改由 `player_store` 的写权行判定。文末「上线状态」一节仍然有效(AGENTS.md 引用它),其余只作历史保留。

> **2026-10-23 初始持久基底后继（待正式生成与运行验证）**：正式 Create 目录与双 SQL 初始行同一 TiDB 事务严格 INSERT，持久 marker 只在首次成功写入；保留无 actor 初始提交 `(1,1)`，首任 actor 由 SceneManager 在采纳地板后铸到 2。完整合同与免冷读种子的提交后顺序见[建角初始持久提交](new-player-first-enter-no-cold-read.md#建角初始持久提交)。空 SQL 或零序号旧缓存不能证明新号，任一真实玩家行缺失均拒绝。

> **2026-10-23 建角初始提交合同（实现与验证中）**：仅正常 `CreateAccountRole` 对严格全新 ID 的目录首次 INSERT，可与权威落点的两张玩家 SQL 行在同一个 TiDB 事务中产生保留初始版本 `(1,1)`。此版本表示无 actor 的真实持久基底，不是“空 SQL 可放行”的例外；缺失的历史行不得补种。首次加载沿现有 SQL 采纳原子地建立 owner 地板 1，随后仍由 SceneManager 唯一发号为首任 actor 2；实际存盘的 SaveSeqAllocator 采用载入下限并严格前进。已有更高 owner 不回退，任何并发 pending/ACK/owner 变化仍由原提交 Lua 拒绝。完整的锁序、提交回执与缓存开关合同见 [建角初始持久提交](new-player-first-enter-no-cold-read.md#建角初始持久提交)。本注不代表已经编译或取得运行证据。

> **2026-10-17 实施状态补充（Codex，未提交）**：负责人已授权统一接手并保留各线改动；下文 v0.4 草案的「未落码」和旧会话等待信号属于历史状态。当前工作区已合并 SQL 行内围栏、真实锁定行收据、双表 SQL ACK、父快照持久补投、缺父键强读恢复及交接标记 SQL 屏障；定向 C++ 构建和完整受影响单测通过。隔离 zone 990017/990018 的真实邮件和跨区交易到账测试均已执行通过、0 跳过，含 SQL 行锁负例、原件成长保留和真实重投。证据见 `build/asset-arrival-20261017/fullstack/logs/fullstack-mail-2.result.json`、`fullstack-trade-1.result.json` 与 `docs/PROGRESS.md` 本日追加条目。默认 `legacy`、历史屏障维护补种及停旧写者切换门禁仍在；此状态不表示已生产切换、Redis Cluster 同槽支持、跨洲故障或目标容量验收。

> **状态**：设计稿 v0.4（2026-10-16，Claude）。v0.4 由两件事触发：一是 v0.3 的补审（[fence-v03-review.md](../handoff/landing-fencing-takeover-20261016/fence-v03-review.md)，fix_plan 第二节 1–9 条全部采纳）；二是工作区里出现了一套来源不明的在途围栏实现（[fence-inflight.md](../handoff/landing-fencing-takeover-20261016/fence-inflight.md)，来源是「全域到账续批」写入方）。负责人 2026-10-16 拍板：在途实现由本线「合并并继续，按最标准的做」。**路线以 v0.3 为准，把在途实现收敛过来**（§0.2），保留在途已占的号位 `DBTask.save_seq = 9`、`PlayerAllData.last_save_seq = 7`。§14 的待拍板项按负责人授权取推荐项定稿，只剩两项要负责人亲自确认。**仍未落码、未编译、未测试**（AGENTS.md §10.1）。各版来由见 §17。
> **实现归属**：由「落地恢复与围栏」接手线落码（[接手记录](../handoff/landing-fencing-takeover-20261016/README.md) §3.4、§5 第 3 批），go/db 会话不做。落码前置见 §0.2「合并前置」：① 「全域到账续批」写入方对围栏相关文件停手；② go/db 存盘管线接手会话发来 R2 / R3 落完后的文件清单。L-04 第 1 批（[db-task-store-unavailable-deferral.md](db-task-store-unavailable-deferral.md) v0.3）已在工作区，本文在它的接口上实现。
> **范围**：C++ scene → Kafka `db_task_zone_*` → go/db → MySQL 这条玩家存盘链。给两张玩家表加围栏列 `(fence_owner_epoch, fence_save_seq)`，go/db 改为条件写。v0.4 起还包括在途实现带进来的消费方「资产 SQL 提交证据」（§6.6）。
> **上游材料**：[db-task-pipeline-bottlenecks.md](db-task-pipeline-bottlenecks.md)（下称「瓶颈文档」）§4、§6.4、§6.6；[交接](../handoff/godb-pipeline-handoff-20261013.md) §4.1 第 3 项；[附录](../handoff/godb-pipeline-handoff-20261013-appendix.md) B「mysql-fencing」（其中已作废的条目见交接 §4.0，本文不采用）；[L-04 设计](db-task-store-unavailable-deferral.md) v0.3（下称「L-04」）；v0.4 新增：[接手记录](../handoff/landing-fencing-takeover-20261016/README.md) §3.4、[fence-v03-review.md](../handoff/landing-fencing-takeover-20261016/fence-v03-review.md)、[fence-inflight.md](../handoff/landing-fencing-takeover-20261016/fence-inflight.md)、[fence-decisions.md](../handoff/landing-fencing-takeover-20261016/fence-decisions.md)、[go/db 实现交接](../handoff/godb-pipeline-impl-handoff-20261015.md) §3 R1 与 §4。
> **库原语**：目标是 proto2mysql 分支 `feat/conditional-save`（`D:\luyuan\proto2mysql`，commit `e81a1e7`）的 `SaveConditional`，语义已逐条对源码核对（fence-v03-review.md verified_ok 第 7 条）[C]。go/db 现钉 `v0.2.0`：`go/db/go.mod` 的 require 是 `github.com/luyuancpp/proto2mysql v0.2.0`，replace 到 `github.com/luyuan-cpp/proto2mysql v0.2.0` [C]。启用 `SaveConditional` 需要负责人把该分支合入 main、打 `v0.3.0` 标签并推送，再把 go.mod 的 require 与 replace 两侧升到 `v0.3.0`（Claude 不 push、不打 tag）。在那之前用 v0.2.0 已有的接口做**过渡实现**（§4.8），结局枚举与 `SaveConditional` 路径完全相同，换原语只改一个函数。v0.3 文首的「只合 main、不打 tag、用伪版本钉提交」改为发 `v0.3.0`，这一项列为 §14 第 21 条，需负责人亲自确认。
> **证据口径**：[C] 读代码确认；[D] 文档记载；[I] 推断。v0.2 的引用在 HEAD `0aa1027bc` 上取证；v0.3 新增或改写的引用以 2026-10-14 的工作区（含未提交的 L-04 第 1 批）为准；v0.4 新增或改写的引用以 2026-10-16 的工作区为准（HEAD `a105c3efd`，含在途围栏实现与 go/db R1 的在途改动）。L-04 让 KOC 整体后移约 4～51 行，PL 被另一会话插入约 286 行，所以正文以函数名为主；仍保留的带行号引用一律指 `0aa1027bc`。
> **缩写**：KOC = `go/db/internal/kafka/key_ordered_consumer.go`；PR = `go/db/internal/kafka/placement_route.go`；PL = `cpp/libs/services/scene/player/system/player_lifecycle.cpp`；P2M = proto2mysql 的 `conditional_save.go`；PRF = `go/db/internal/kafka/player_row_fence.go`（在途新增，未跟踪）。

---

### 2026-10-18 历史屏障维护恢复契约（首版已实现，限定形态验收）

`go/db/cmd/save_barrier_restore` 只供停写维护使用，默认生成计划，执行必须指定计划原始文件 SHA-256 并再次声明旧 scene/login/DB 写者已停止、Kafka/retry/dead 已审定排空。工具不能替代部署停写或证明旧进程不会复活。运维先备份并裁决父缓存与未提交意图，再清本次玩家父/子缓存和资产 SQL 证据；工具自己不删除缓存、不丢弃意图、不铸造 owner。

每批至多 1000 名玩家，显式指定实际 SQL schema 和 home zone，禁止扫描全服自动修复。首版仅支持权威路由为 Redis、映射与父缓存同实例 DB0、显式稳定 placement 与 home zone 都等于 `zone_<home_zone>_db` 的玩家；必须显式传 `-routing-mode redis-same-instance`。缺映射、冻结/异区/全局落点、db_first、独立映射实例或非零 Redis DB 均不支持，不能用旧库残留行给已搬走玩家背书。独立映射、db_first 与 player_store 的恢复还需相应权威路由证明，当前不宣称这一部分完成。

每人按固定表序以 PRIMARY 点锁读取两张完整 SQL 行，计划记录 SQL schema、Redis 地址/DB、强读模式、身份、home/placement 原值、非围栏列摘要及旧/目标围栏，不包含资产明文。SQL 始终使用计划中已验证的 schema 限定表名；执行参数、实际 Redis 连接与计划必须相符。默认要求 TiDB 强会话，本地 MySQL 豁免也绑定计划，不能在执行时切换。两行缺失、身份不符或不同版均拒绝；现有 owner 必须是正数且不小于 SQL epoch。非零围栏原样恢复；历史 `(0,0)` 只有显式允许迁移时才将两行元数据改为 `(现有 owner,1)`，不修改任何资产列，不凭空猜测丢失的 owner。

发布前同一 Redis Lua 复核永久维护标记、owner、home/placement 原值、无合服围栏、父/子缓存与资产证据均不存在、该 zone 全 64 桶为空、屏障不存在或仅含精确相同的三项已提交版本；全部通过才写 pending/两表 ACK。映射和 owner 不得带 TTL。SQL 锁持续持有至 Redis 发布之后；旧版数据先提交双表围栏元数据，再新开 RC 事务重新点锁两行，复核资产摘要与目标版本，才能发布；SQL 提交失败绝不发布。中断后可按同一计划幂等续跑，资产摘要、路由或 owner 改变则拒绝。每人独立提交，失败可能已有前缀完成；执行回执逐人记录结果，维护标记与停写闸始终保留。SQL 与 Redis 不是一个事务，发布后的新存盘仍须走正常围栏；未完成整批、冷加载和故障验收不可放流。当前前提仍是同一 Redis 实例，不能在 Cluster 上跨槽执行。

2026-10-18 在自有 TiDB v8.5.2、Redis DB0 上完成四项真实集成：非零围栏恢复重入、历史双表升级且完整资产字节不变、计划后 SQL 资产漂移拒绝、末桶待补投拒绝。同包单元与集成合计 38 项 PASS、0 FAIL、0 SKIP，86 项输入无漂移，随机库和自建键已清理。证据：`build/barrierrestore-linux-20261018-030237-63a67b/final-proof.json`。执行平台为 Linux；Windows Application Control 拒绝启动的记录与早期高位自增 ID 夹具错误均保留，没有修改系统策略或正式表结构。该验证不覆盖高位自增分配器边界、真实旧写者复活、跨洲故障或生产放流。

## 0. 结论速览

### 2026-10-18 历史恢复第二批契约（两版真库接缝已验，真实双区宿主演练待验）

本批扩展现有恢复接缝，不改正常存盘协议：支持 `db_first` 的 SQL 真源路由、独立 mapping Redis、非零父缓存 DB，以及归属区与物理 zone/player_store 库不同的稳定落点。SQL 与 Redis 仍非同一事务。跨实例恢复必须先停止业务与路由写者、审定排空，并用工具 `guard` 在 mapping DB0 建立永久的维护标记与既有 `merge:in_progress:<home>` 广播围栏；该围栏由现有建角、pin/relocate/合服入口在提交点尊重，不以新造的无人读取键代替停写。失败保留所有维护闸和已完成前缀，不自动删闸或放流。

`db_first` 必须显式指定同一 TiDB 实例内的路由 schema。恢复事务按 `route_reconcile → route_meta → player_route(完整聚簇主键) → 两张资产表` 加锁，要求 `writable=0`，锁内绑定库世代、水位、玩家 rev/home/稳定落点；旧零围栏提交后重新按同序取锁再复核。这样路由解冻/反向修库不能穿过提交点。mapping 副本的就绪标记必须已撤下；水位必须与 SQL 同世代且不领先，副本三项要么全无，要么与被锁 SQL 路由一致，不凭缓存缺键推断无玩家。永久广播围栏拦截新建角/pin/搬迁入口；在途搬迁的收尾/撤销及 SETNX 回填仍依赖外部停写，不能称 guard 已自动挡住所有写者。恢复拒绝 frozen，并在独立 mapping 发布前后复核路由及围栏，任何失联/漂移保持维护。人为移除永久围栏、直接改键或绕过维护写 SQL 不在自动恢复承诺内。

新计划使用 v2 格式绑定连接、schema、强读模式与路由证明；v1 同实例本区计划仍按原范围验证。Redis Cluster 尚不是本仓现行父快照/补投协议支持的部署形态，本批仍明确拒绝 MOVED/跨槽，不能仅改恢复工具宣称全链 Cluster 可用。两版与客户端按各自既有权威接缝补相同故障语义，不复制不适用的内部协议。

带独立旧 owner 上界约束的当前库包已在真 TiDB、父缓存 DB2 与独立 mapping DB0 完成 64 项 PASS、0 FAIL、0 SKIP，90 项输入前后无漂移；包含正式路由 DDL、全局 `player_store` 落点、历史双表围栏迁移、同计划重入、阻止路由解冻及 rev 漂移拒绝，以及独立 Redis 权威路由恢复/漂移拒绝、SQL 本区 NULL 落点两项补充部署分支。当前原始收据为 `build/verification/barrier-restore-v2-20261018/final-real-20261018T094145944Z/proof.json`，此前 `real-proof.json` 的62项为追加前历史结果。CLI 五例、两包 vet、Windows 构建与 Java 49 项 H2 已通过。Java 真 TiDB 已覆盖72个唯一用例，包含独立审核上界、旧实例拒写与迟到 ACK：首轮71通过，唯一失败项受宿主16小时休眠后的 schema lease失效影响，经SQL健康核验后只复验该方法并通过；首轮exit1及电源事件保留，不称单轮72全绿。Java汇总见同级Java仓 `build/ownership-recovery-20261018/final-proof.json`。真实旧 scene 复活/延迟 ACK 双区宿主演练仍待验收，不能据此放流或宣称全球容量已通过。

2026-10-19 UTC 03:05 的同口径只读复查发现，Java 历史验证的 2328 项输入已变为 2331 项：23 项修改、3 项新增、无删除，涉及 scene、登录认证、网关与 ContractSync。两次观察区间无漂移，不代表写方冻结；9 个自有文件中7个代码/测试未变、两份文档已变，也不能推断其余改动无影响。原49 H2及72唯一真库覆盖收据保留为原输入下的通过证据，不能当当前整体版本验收。当前生成、构建和受影响验证须另据新输入完成；有效复查收据为 `build/verification/java-current-drift-corrected-20261019T030502074Z/receipt.json`。

1. **围栏是行内两列，按字典序比较。** go/db 把 DBTask 的 `(owner_epoch, save_seq)` 盖到要落库的消息上做条件写：库内围栏更旧才写。目标原语是 `SaveConditional`（判断与写入是同一条 UPDATE）；proto2mysql `v0.3.0` 发版之前用过渡实现（同一事务里 `SELECT … FOR UPDATE` 判定后再写，§4.8），两者结局枚举相同。旧的执行实例恢复后发出的写会被 MySQL 自己拒掉，不再依赖 Redis 排序锁的租约。
2. **首批只实现 `legacy` 与 `dual` 两种模式。** `dual` = 条件写 + 现有全套 Redis 守卫；`legacy`（v0.2 叫 `off`，改名原因见 §4.7）是紧急开关，写入语义等同今天。删 Redis 守卫的 `sql` 模式（附录的 Stage 2）属于性能轨，本文只定契约和门禁（§8.5），不在首批实现。条件写只作用于「围栏表」（§4.1），其余表照旧 `Save`。
3. **被拒的写按拒绝位置分两路，没有任何一种是「不复核就丢」：**
   - 到达 SQL 的：库里已有同版本或更新版本（`equal` / `older_seq`）→ 落点复核、核锁都通过后才 ack，且 `older_seq` 不推进游标与已落库 epoch；归属更老（`stale_owner`）、计数器倒退（`owner_epoch_regressed`）或无法判定（`legacy_after_seq`）→ 进死信并告警，保住载荷。被拒的结局以 `(outcome, nil)` 交回 `handleTask`，不走成功分支（§4.5）。
   - 在 Redis epoch 守卫被判 stale 的：`dual` 下先探测 owner_epoch 计数器。没有倒退 → ack 丢弃（同今天）；发现倒退 → 进死信（§5.1）。
   - 残余：计数器重新追过已落库 epoch 之后，倒退期间发出的写与僵尸写无法区分（§9.1）。
4. **游标判过期时让 epoch 说话（G3）只在 `dual` + `Fencing.EpochOverride` + class-1 + 围栏表上放开。** 放行的安全性来自新二进制把「游标、已落库 epoch」用一条脚本原子推进（§5.5），override 之后游标原子改写到本任务（§5.2）。旧二进制做不到原子推进，所以这个开关有门禁（§8.3）。
5. **save_seq 由 C++ 进程级发号器 `SaveSeqAllocator` 发出**，`Next = max(last + 1, nowMs << 10)`；播种来自 Redis 父键和 MySQL 行三路取 max，墙钟只作下限；`Observe` 的上界是「当前时间 + 60 秒」，超界的值只作该玩家实体上的个人下限，不抬高进程水位；`Next` 饱和时显式失败。在途实现的每实体计数器 `PlayerOwnerEpochComp.lastSaveSeq` 删除（§7、§0.2）。C++ 只写 `PlayerAllData.last_save_seq`，不写子表 body 里的围栏。
6. **子表缓存不加键、不改值格式**：写路径改成「持有排序锁令牌才写」的单条脚本，同时取代现有的核锁（verify），`dual` 下 class-1 写 `player_database` 时在同一次 EVAL 里发布资产 SQL 提交证据；读路径改成 `SET NX`（§6）。缓存只前进不后退，但可能落后于 MySQL；缓存里的围栏只是行内围栏的下限（§6.5）。
7. **规模**：v0.4 按合并后的实际范围重数，手写约 60 个、生成物约 15 个（§12），远超 AGENTS.md §10.2 的 30 文件门槛。跨 C++ / Go / proto / schema；表字段的 DDL 已随 2026-10-15 的先行声明进入迁移流程（§8.1）。负责人已授权实现并合并在途实现；超门槛这一点在落码交付说明里仍要单独报告（§12）。
8. **`equal` 按幂等生产者口径处理**：同一 `(owner_epoch, save_seq)` 视为同一笔写的重放，**不比内容**；撞号由 C++ 发号器的墙钟下限消除，资产证据层保留「同版本不同内容即报错并计数」（§4.4、§6.2、§14 第 12 条）。
9. **在途实现的去留**逐条见 §0.2：保留号位、三路 max 播种、证据这一消费方与它的键；重写 go/db 的条件写与结局处理；删除「被拒写当成功 ack」、读回整行用 `proto.Equal` 比内容、每实体计数器、C++ 写 body 围栏、缓存 GET→比较→CAS 与独立的证据 EVAL。

### 0.1 相对交接与附录的变化

| # | 交接 / 附录的说法 | 本文的做法 | 原因 |
|---|---|---|---|
| 1 | C++ 加 `PlayerSaveSeqComp`，实体内计数 | 进程级发号器，实体不存序号 | 实体在同一 epoch 内会销毁重建（PL:3623-3628）；Redis 父键不是「已发出 DBTask」的上界（§7.1） |
| 2 | 播种取 `max(PlayerAllData.last_save_seq, 同 epoch 的 fence_save_seq)` | 两路都取，不看 epoch；`Observe` 加上界 | 父键存在时 login 不读子表，C++ 拿不到 MySQL 行里的围栏（§7.2）。不看 epoch 有代价：进程级下限会被任意一名玩家带来的值抬高，并经父键和行内围栏在进程之间传播，所以要加上界（§7.2） |
| 3 | 缓存回写带版本，建议放进值封装 | 用排序锁令牌做原子校验（新二进制，`legacy` / `dual` 都生效）；值封装留给 `sql` 模式 | `dual` 还持有排序锁，令牌校验已经够用，且不动 login（§6） |
| 4 | 三种模式一起实现 | 首批只做 `legacy` / `dual` | `sql` 是性能优化，立项判据未满足（瓶颈文档 §6.3） |
| 5 | G3「先判 epoch 再判 seq」 | **部分修复**：只在 `dual` + `EpochOverride` + class-1 + 围栏表上放开；override 后原子改写游标；新二进制原子推进游标与已落库 epoch | 不带 save_seq 的写在同一 epoch 内没有可比的版本；v0.2 的放行在 override 后不改游标、且依赖游标与 epoch 的非原子推进，两头都不成立（§5.2、§5.5，§17 状态机 F1 / F2） |
| 6 | B1：`redis-cli FLUSHALL` 会让全部存盘被判 stale_owner | 触发面比预想小；处置改为「定真源、抬计数器、不归零、不回灌」 | FLUSHALL 同时清掉账号目录，旧 player_id 不会再被用到；归零加回灌会用旧数据覆盖新数据（§9.1） |
| 7 | 能力心跳 `db:capability:zone` 可复用为机器门禁 | 不能复用，`sql` 模式要另建按实例的登记 | 该键每 zone 一把、值是常量，merge_zone 按精确相等判定（§8.5） |
| 8 | 缺省模式 `dual` | 缺省 `legacy`，逐 zone 改配置切 `dual` | 二进制发布与行为切换分开，可以单 zone 金丝雀（§8.3） |

> **2026-10-18 更正**：第 8 行的「缺省 `legacy`」与 §14 第 4 条，都是严格提交屏障出现之前的口径。10-17 起 scene / login 只说严格屏障，「升级所有 DB 为 dual」已是该协议的整批切换前提（文末 10-17「登录冷恢复」附注）。缺省因此改为 `dual`，`legacy` 由机器门禁约束，见 §4.7 更正与文末 2026-10-18 附注。金丝雀仍可以逐 zone 做，但单位换成「整 zone 切世代」：按 10-17 附注停写、迁表，go/db 全部 `dual`、scene / login 全部升级后一起放流。不再有同一 zone 内 `legacy` → `dual` 的运行时切换。

### 0.2 在途实现的合并（v0.4 新增）

**来源与现状**：2026-10-15 22:21–23:12，「全域到账续批」写入方为资产实例过户与托管做「SQL 提交后才算到账」的前置，在工作区里先实现了一版行内围栏（未提交，`docs/design/guild-phase2/04-asset-channel.md` 文末「2026-10-15 全域到账续批：SQL 提交与实例托管（实施中）」一段是它的说明）[C]。它和 v0.3 不是同一条路：被拒的写当成功 ack 并推进游标、读回整行用 `proto.Equal` 比内容、C++ 每实体计数器没有墙钟下限、新 SQL 路径不打 `storeerr` 标记、没有 `legacy` / `dual` 开关（fence-inflight.md F1–F4）。负责人 2026-10-16 拍板由本线合并接管，按标准做法收敛到本文。

**合并前置**（两条都满足之前，本线只改文档，不动下表任何代码文件）：

1. 「全域到账续批」写入方对下表文件停手。由负责人告知它：`player_row_fence.go`、KOC 里的围栏 hunk、`player_cache_publish.go`、C++ 的 save_seq 发号、`go/shared/assetop/sql_commit.go`、`go/data_service/internal/logic/asset_sql_commit.go` 已移交本线。截至 2026-10-15 23:05 它仍在改 `go/shared/assetop`、data_service、mail、rank 与 `player_lifecycle.cpp`，不停手就会互相覆盖。
2. go/db 存盘管线接手会话把 R2（④ 重试只交给分区属主 + WAITAOF）、R3（保留期）落完，发来文件清单；本线按 S1 规格的锚点 rebase，被拒结局走 `quarantineOrderingConflict`，自动继承 S1 的 WAITAOF。在此之前「不碰 `go/db/internal/kafka` 与 `proto_sql`」的约定继续有效，只有 PRF 与 KOC 里的围栏 hunk 归本线。

**逐条去留**（行号以 2026-10-16 工作区为准，在途代码仍在变，引用前重新定位）：

| # | 在途改动 | 做了什么 | 去留 | v0.4 的做法 |
|---|---|---|---|---|
| 1 | `proto/db/db_task.proto` `uint64 save_seq = 9` | 加字段，注释写「同一 owner_epoch 内严格递增」 | **保留号与字段**，改注释 | 注释按 §3.1：按玩家只增不减（进程级发号器 + 墙钟下限，不是每实体计数）；写明 10 号预留给 `reply_instance`；同批订正 8 号字段注释 |
| 2 | `proto/common/database/player_cache.proto` `uint64 last_save_seq = 7` | 加字段 | **保留**，改注释 | 注释按 §3.3：非业务元数据，只用于 C++ 加载播种，不参与任何相等比较 |
| 3 | `proto/common/asset/asset_op.proto` `PlayerAssetSQLCommit`（1–4）；`proto/data_service/data_service.proto` `require_sql_commit = 2`、`sql_committed = 3` | 新增证据消息与读接口字段 | **保留号与语义** | 读路径按 zone 开关启用（§6.6）；字段不动 |
| 4 | PRF `savePlayerRow` | 按表名硬编码；`RunInTransaction` + `FindOneByPKForUpdate` 读整行 + `Save`；未版本化的写一律盖 `(0,0)`；同版本用 `proto.Equal` 比内容；被拒时用库内行替换 `msg` 并返回 `errPlayerRowStale`；错误不打 `storeerr` 标记 | **重写**（文件名保留） | 围栏表启动时按描述符判定并断言（§4.1）；按模式 × 类别盖章（§4.2、§4.3）；单一接缝函数 `fenceSave`，过渡实现按 §4.8（无行走 `Insert`、撞 1062 重读再判、固定隔离级别）；结局归类按 §4.4；删掉 `errPlayerRowStale`、`proto.Equal`、替换 `msg` 的 `Reset` / `Merge` |
| 5 | `player_row_fence_integration_test.go` | `ASSET_FENCE_TEST_DSN` 门控、一次性随机库的真库用例 | **保留夹具，重写用例** | 用例按 §11 第 3 步；「同版本不同载荷必须失败」这一断言删除（§4.4 `equal` 不比内容） |
| 6 | `go/db/internal/kafka/player_cache_publish.go` 及其测试 | 写缓存前 GET → 解析 → 比围栏 → 按现值 CAS，最多三轮；同版本 `proto.Equal` 比内容 | **删除** | 由 §6.2 写路径令牌脚本取代：令牌在缓存所在的 Redis 上原子校验，不需要比较缓存里的围栏；「旧发布不得覆盖新版本」改由令牌脚本的用例覆盖（§11 第 2 步） |
| 7 | KOC import 段（`assetpb`、`shared/assetop`）| 证据发布用 | **保留** | 改为只用 `assetop.SQLCommitKey` / `assetop.SQLCommitVersion` 与 `assetpb.PlayerAssetSQLCommit` |
| 8 | KOC `handleDBWriteOp`：`if handled, err := savePlayerRow(...); handled { return err }` | 围栏表走自建路径，错误原样返回 | **改** | handler 签名扩成 `(writeOutcome, error)`；SQL 错误一律 `storeerr.Tag`（R1 第 2 条，§4.1） |
| 9 | KOC `publishDBWriteResult`：先 `assetop.PublishSQLCommit` 再写缓存 | 证据单独一次 EVAL，取的是 `msg` 里的围栏 | **删** | 证据并入 §6.2 令牌脚本，只在 `dual`、class-1、结局 `applied` / `equal`、`player_database` 时发布，版本取盖章值 `(task.OwnerEpoch, task.SaveSeq)` |
| 10 | KOC `writeBackDBCache`：调 `publishPlayerCache`，非玩家表回落普通 `SET` | — | **删** | 所有写路径的缓存发布统一走 §6.2 令牌脚本 |
| 11 | KOC `runDBOp`：`errPlayerRowStale` 改写成 `nil` 并打中文 INFO | 被拒的写走成功分支：发布库内行、`markAppliedSeq` / `markAppliedOwnerEpoch` 后 ack | **删** | 按 §4.5 的结局分支（R1 第 1、3 条）：被拒结局以 `(outcome, nil)` 交回 `handleTask`，不推进游标与 epoch；新日志用英文并配计数器 |
| 12 | `go/shared/assetop/sql_commit.go` | `SQLCommitKey`；`PublishSQLCommit` 用 Lua 字符串 `>` 比较定宽版本，返回 0 时静默 | **部分保留** | 保留 `SQLCommitKey` 与定宽版本串格式（抽成 `SQLCommitVersion(epoch, seq)`）；删 `publishSQLCommitScript` / `PublishSQLCommit`：证据只有 go/db 一个写入方，按字节比较并入令牌脚本（`>` 依赖 strcoll / locale，n == 0 不计数）。**过渡（2026-10-16）**：令牌脚本归 go/db 线、尚未落地，go/db 仍调用 `PublishSQLCommit`，故先就地改为逐字节比较、证据更新时计数并告警、改用 `Script.Run`，并已抽出 `SQLCommitVersion`；令牌脚本落地后再删（§6.6「落码记录」）|
| 13 | `go/shared/assetop/sql_commit_test.go` | 终结要求证据、托管收据完整、发布单调 | **保留前三类，移走发布用例** | 终结类用例改为「按 zone 开关打开时」的行为；发布单调与同版本冲突的用例移到 go/db 令牌脚本测试 |
| 14 | `go/data_service/internal/logic/asset_sql_commit.go` 及其测试 | 只读证据、缺证据回 `found=false`、不回落 Redis blob | **保留** | 补注释写明前提「证据键与 go/db 的 Redis 是同一实例」，并在 G4a 拆分稿登记（§6.6） |
| 15 | `go/shared/assetop` 其余在途（`ledger_dataservice.go` 恒带 `RequireSqlCommit: true`；`reconcile.go` 的 `verifySQLCommit` / `resultWithReceipt`；`Ledger` 仍是可选）| 资产终结与自动放弃改读 SQL 证据 | **需改，不在移交范围** | 归「全域到账续批」写入方。v0.4 要求按 §6.6 改：`RequireSqlCommit` 按 zone 开关；自动放弃要求 Redis blob 与 SQL 证据都「未见」，或证据版本 ≥ 父键 `last_save_seq`；`Ledger` 必填改启动断言。合并时与该写入方对齐，或由负责人改派本线。**2026-10-16 负责人改派本线，已落码（未编译、未测试），做法与偏差见 §6.6「落码记录」** |
| 16 | `player_ownership_comp.h` `PlayerOwnerEpochComp.lastSaveSeq` | 每实体存盘计数 | **删** | 进程级 `SaveSeqAllocator`，`tlsIdGeneratorManager` 的成员（§7.2）；`PlayerOwnerEpochComp` 回到只管归属 epoch |
| 17 | PL `InitPlayerFromAllData`：`lastSaveSeq = max(last_save_seq, 两张子表 fence_save_seq)` | 三路 max 播种 | **保留算式，换位置** | 移进纯函数 `SaveSeqFloorFrom`，经 `Observe` 进发号器；超界时挂个人下限（§7.2） |
| 18 | PL `SavePlayerToRedisImpl`：`++version->lastSaveSeq`，饱和返回 `kRejected` | 每实体 ++，没有墙钟下限，没有上界 | **改** | `SaveSeqAllocator::Next` + 个人下限；饱和显式失败保留（§7.2） |
| 19 | PL `MarshalPlayerForSave` 与 `SavePlayerToRedisImpl` 把 `(epoch, seq)` 写进两张子表 body 的 `fence_*` | C++ 写围栏 | **删** | C++ 只写 `PlayerAllData.last_save_seq`，子表 `fence_*` 恒为 0，由 go/db 盖章（§3.2、§4.2） |
| 20 | PL `dbTask.set_save_seq(ownerEpoch == 0 ? 0 : seq)` | epoch 为 0 时不带序号 | **保留** | §7.2 |
| 21 | PL 快路径比较从 `dirty_save::IsEqual` 换成 `player_exit::IsPersistedPayloadCurrent` | 每次存盘尝试深拷两份 `PlayerAllData`，且顺带剥离 `stress_test_probe` | **改回** | 快路径仍用 `dirty_save::IsEqual`；`last_save_seq` 在 `PlayerLastPersistedSnapshotComp::Replace` 里剥离一次（§7.3）。快路径是否剥离 `stress_test_probe` 单列给负责人确认（§14 第 22 条，影响压测口径） |
| 22 | PL `IsPersistedPayloadCurrent` 两侧清 `last_save_seq` 与四个 `fence_*` | 退出收敛比较不受序号影响 | **保留** | 退出收敛路径，不在热路径；对 `fence_*` 的清除作为防御保留 |
| 23 | PL `SavePlayerForAssetCommit`（不走快路径的强制存盘）| 资产操作强制整份写 | **保留**（属资产通道）| 交付说明注明 DBTask 量会上升，压测前后对比 |
| 24 | 生成物 | `go/proto`、`cpp/generated` 已含 9 / 7；`robot/vendor/proto/db/db_task.pb.go` 与 `generated/robot/proto/db/db_task.proto` 还没有 | **重生** | 改完注释后按 §11 第 1 步用 no-unity 配置重生，逐个核对 |

> **2026-10-18 更正**：第 1 行「写明 10 号预留给 `reply_instance`」已过时，10 号已由 `reply_instance` 使用，见 §3.1 同日更正。

**在途实现与本文一致、直接沿用的部分**：号位 9 / 7；三路 max 播种；两张子表共用同一个 save_seq；epoch 为 0 时 DBTask 不带序号；写 SQL 用租约的 ctx；证据只在 SQL 成功、复核与核锁通过之后发布；证据键 `player:{id}:asset_sql_commit` 用 `{player_id}` 作 hash tag；版本串 `%020d:%020d` 能容纳 uint64 全范围。

**go/db 接手会话 R1 的三条意见**（对在途实现提出，v0.4 逐条落进正文）：

1. **stale 分支不得跳过 confirm / verify**：在途实现在复核与核锁之前就把被拒的写当成功返回。v0.4 规定终态为 ack 的结局一律先过落点复核、再过核锁；终态为死信的结局不 ack 丢弃、不碰游标与缓存（§4.5）。
2. **handled 分支的错误必须 `storeerr.Tag`**：§4.1、§4.8。
3. **新日志用英文，并配计数器**：在途实现只有一行中文 INFO。v0.4 每个结局都计 `db_fence_write_total`，日志前缀与告警见 §10。

---

## 1. 问题与目标

### 1.1 问题

- MySQL 写是无条件整行覆盖 [C]（KOC:829 `store.Save(msg)`；Save 的语句链是 UPDATE → INSERT → 1062 重试，没有版本条件）。
- 「同一玩家同一时刻只有一个 go/db 在写」只靠 Redis 排序锁的租约（TTL 2 分钟、30 秒续租，KOC:1843-1844）。续租失败只置一个标志，不取消在途 SQL；失租检查在 SQL 与缓存回写都做完之后（KOC:1631-1634）[C]。
- 所以旧实例卡住再恢复时，它手里的旧快照会把 MySQL 和子表缓存整行覆盖回去，没有报错，也不自愈（瓶颈文档 §4.3 场景 3，缺口 G1）。
- 游标先于 epoch 判定，新主的写在重试队列里时可能被老主的迟到写挤掉（缺口 G3）。游标与 epoch 记录缺键时放行（缺口 G5）。

### 1.2 目标（不变量）

| # | 不变量 | 靠什么保证 |
|---|---|---|
| F-1 | 同一玩家、同一张围栏表、同一个库里，行内围栏只进不退；带 save_seq 的写里，行内容永远是已到达的写中围栏最大的那一笔 | 条件与写入在同一条 UPDATE 里，在主键行锁下完成；§5.2 放行的写落库后原子改写游标（§5.2、§5.5）。v0.2 在 override 后不改游标，同 epoch 的较新重试会在游标处被丢，本条当时不成立（§17 状态机 F1） |
| F-2 | 被拒的写不留下任何副作用：不写缓存，不发布资产 SQL 提交证据，不推进游标和已落库 epoch | 结局处理表（§4.4）；被拒结局以 `(outcome, nil)` 交回 `handleTask`，不进成功分支（§4.5）。在途实现正是在这里出错：被拒的写走成功分支、推进了游标（§0.2 第 11 行） |
| F-3 | 不静默丢盘。到达 SQL 的写：只有库里已有不低于本版本的数据、且复核确认该库仍是真源时才 ack，其余进死信并告警。在 Redis epoch 守卫被判 stale 的写：`dual` 下先探测计数器，没有发现倒退才 ack，发现倒退进死信 | §4.4、§5.1、§10。残余：计数器重新追过已落库 epoch 之后，倒退期间发出的写与僵尸写无法区分（§9.1） |
| F-4 | 子表缓存只前进、不后退，不会被比它旧的版本覆盖；SQL 已落而缓存未发布的窗口靠重试补发收敛 | §6.2、§6.5。缓存不保证等于 MySQL（失租、发布失败、跨进程撞号时见 §4.4、§6.5） |
| F-5 | 新旧 go/db、新旧 C++ 并存期间，安全性不低于今天 | 成立条件：§5.5 的原子推进，加 `EpochOverride` 门禁（该 zone 没有旧版 go/db，且旧实例下线后重试排空过一次，§8.3）。新二进制的 `legacy` 与 `dual` 混跑在 §5.5 之下是安全的（§8.4） |

### 1.3 不在范围内

- 删 Redis 守卫、合并 Redis 往返（`sql` 模式与瓶颈文档 §6.3 的 3a / 3b）。
- 读写分区对齐（①）、重试只交给分区属主（④）、消费组按 zone 拆（③）。
- 连接不可用时延后（L-04）与死信工具：见 [L-04](db-task-store-unavailable-deferral.md)。第 1 批已落码（工作区，未提交、未验证），本文在它的接口上实现（§4.1、§4.4）；第 2 批死信工具是 Stage 1b 的前置条件（§8.3），目前未授权。
- 两张子表之间的原子性：仍然是两条独立的 DBTask、两次独立的写。
- 「scene 发 DBTask 失败后脏比较快照不作废」（`SavePlayerToRedisImpl` 的发送失败分支）：负责人已定归存盘管线交接（交接 §5 第 17 项），不在本设计。
- **冷登录与在途写的竞态**（§6.2 残余窗口 3）：父键缺失、而该玩家仍有未落库的写时，login 拼出的父键比在途写旧。NX 和条件写都挡不住，今天同样存在。修法方向（按 key 维护「有待落写」标记）记在 §14，不在本批。

---

## 2. 术语

**围栏**：行内的 `(fence_owner_epoch, fence_save_seq)`，按字典序比较，epoch 是高位。

**任务类别**（按 DBTask 判定，只对 `op == "write"`）：

| 类别 | 条件 | 来源 | 指标 label |
|---|---|---|---|
| class-1 | `owner_epoch > 0` 且 `save_seq > 0` | 新版 C++ | `seq` |
| class-2 | `owner_epoch > 0` 且 `save_seq == 0` | 没有 save_seq 的旧版 C++（滚动升级窗口） | `legacy_epoch` |
| class-3 | `owner_epoch == 0`（不看 save_seq） | 实体没有拿到 epoch 的兼容路径（`SavePlayerToRedisImpl` 里 epoch 为 0 时走无守卫 Save 并计 `owner_epoch_unknown`）；`go/db/cmd/data_stress`（不填 OwnerEpoch）| `legacy_zero` |

当前 write 的生产者只有 C++ scene（`SavePlayerToRedisImpl`，只写 `player_database` / `player_database_1`）和 data_stress（只写 `PlayerDatabase`）[C]。

class-3 在现行代码里很难走到：scene_manager 的路由一律带观察到的或新铸的 epoch，键丢失时同节点按 location 补种、location 也没了就铸造（≥1），gate 原样透传 [C]。对已经落过 class-1 的玩家，只有 CZ-4 之前的旧 gate / scene_manager 还在跑才会产生 epoch 0；另一类是还没铸过 epoch 的存量玩家同落点重连，它写的是 `(0,0)` 行，切 `dual` 后无害。data_stress 的号段从 1,000,000 起，只影响测试环境。

**围栏表**：描述符里有两列围栏字段的表（判定规则见 §4.1）。今天只有 `player_database`、`player_database_1`。§4.2–§4.4、§5.2、§5.3 只作用于围栏表。

**资产 SQL 提交证据**（v0.4 新增，来自在途实现）：go/db 在 `player_database` 的 SQL 写确认之后，把这一版的资产账本连同 `(owner_epoch, save_seq)` 写进 Redis 键 `player:{id}:asset_sql_commit`；资产通道据此判断「这笔资产结局已在 MySQL 里」。它只是 SQL 已提交的缓存证据，不是第二份资产权威（§6.6）。

**模式**（配置 `Fencing.Mode`）：

| 模式 | 含义 |
|---|---|
| `legacy` | 紧急开关（v0.2 叫 `off`，§4.7）。写入语义等同今天：Redis 守卫照旧，SQL 无条件覆盖业务列，不碰已有行的围栏列 |
| `dual` | 条件写 + 现有全套 Redis 守卫 |
| `sql` | 预留。首批二进制读到这个值时拒绝启动 |

子开关 `Fencing.EpochOverride`（缺省 `false`，只在 `dual` 下生效）控制 §5.2 的放行，开启条件见 §8.3。模式名与类别的指标 label（`legacy_epoch`、`legacy_zero`）互不相关。

---

## 3. 数据契约

### 3.1 DBTask（`proto/db/db_task.proto`）

- 新增 `uint64 save_seq = 9;`。**负责人已定**：9 号给 `save_seq`，10 号给全局单点交接的 `reply_instance`（交接 §5 第 13 项）。9、10 号都从未用过 [C]，不存在复用问题；先落码的一方在注释里写明另一个号已预留。
- **2026-10-16 现状**：在途实现已声明 `uint64 save_seq = 9;`（未提交），注释是「同一玩家同一 owner_epoch 内严格递增；与 SQL 行版本一起校验」，没写 10 号预留，8 号注释也没订正 [C]。保留字段，落码时把注释换成下面的口径（§0.2 第 1 行）。
- 语义：生产者按玩家发出的存盘序号，同一玩家只增不减；0 表示生产者没有发号（旧版 C++，或该次存盘的 owner_epoch 为 0）。
- 注释定稿（落码照抄，可调措辞不改语义）：「scene 进程级发号器按玩家发出的存盘序号：同一玩家只增不减（跨实体重建、跨进程靠加载播种与墙钟下限，见 db-write-fencing-mysql.md §7）；0 = 生产者未发号。go/db 把 (owner_epoch, save_seq) 盖成 MySQL 行内围栏。10 号预留给全局单点交接的 reply_instance，不得占用。」
- **2026-10-18 更正**：10 号已由 `reply_instance` 使用（task:result 读回执通知按 login 实例分频道第 1 批：`proto/db/db_task.proto` 已声明 `string reply_instance = 10;` 并写了语义注释，未编译，待 Codex 重生成）。上一条注释定稿的末句「10 号预留给全局单点交接的 reply_instance，不得占用」落码时改为「10 号为 reply_instance」或直接删去，不要再写「预留」，否则 proto 里相邻两段注释自相矛盾。本节第 1 条、§0.2 逐条去留第 1 行与 §14 已定第 1 条里「10 号预留」的说法同此更正。
- 顺带订正 8 号字段的注释（:18-19）：现在比对的是「已落库的最大 epoch」（`consumer:applied_epoch:*`，KOC 的 `appliedEpochKey` / `checkOwnerEpoch` / `guardOwnerEpoch`），不是 `player:{key}:owner_epoch`。

### 3.2 玩家表（`proto/common/database/mysql_database_table.proto`）

| message | 新字段 | 现末尾 |
|---|---|---|
| `player_database` | `uint64 fence_owner_epoch = 18;`、`uint64 fence_save_seq = 19;` | `settlement_ledger = 17`（:154）|
| `player_database_1` | `uint64 fence_owner_epoch = 3;`、`uint64 fence_save_seq = 4;` | `stress_test_probe = 2`（:170）|

- 都声明在 message 末尾（加列纪律，:139-145）。18 / 19 目前没有被占用 [C]，落码前再 grep 一次。
- **2026-10-15 已先行声明**（未提交、未生成）：
  - 四个字段先于条件写落进 proto。原因是玩家收件箱要接着取 20 号（`inbox_ledger`），必须先占住 18 / 19，新老库的列序才一致。
  - G-03 放行清单已同批更新：`90-consistency.md`、`03-names.md` §3.15a、`92-handoff.md` 步骤 4。
  - 在 go/db 条件写上线之前，这四列只被原样读写，不参与判定。迁移照 G-03 纪律，先于任何新版 go/db 和 scene 启动。
  - 本节其余内容（DBTask `save_seq`、PlayerAllData `last_save_seq`、go/db 与 C++ 逻辑）仍按原计划在前置满足后落码。
- 注释写明：存储版本元数据，只由 go/db 按 DBTask 写入；C++ 只在加载时读来播种（§7.2），存盘时不写。在途 C++ 把 `(epoch, seq)` 写进了两张子表的 body（§0.2 第 19 行），v0.4 删掉，否则旧版 go/db 的无条件 `Save` 会把 body 里的值原样写进行，`epoch == 0` 分支还会写出 `(0, s>0)` 这种无法判定的围栏（fence-inflight.md F10）。
- **为什么必须是 proto 字段**：`SaveConditional` 的 `FenceFields` 只认该表 message 的顶层 uint32 / uint64 字段，栅栏值从消息字段读，没有「外部传入栅栏值」的入口（P2M:200-208、:264）[C]。另外三个好处是附带的：`cmd/migrate` 的 Drift 自动生成 ADD COLUMN；启动闸 `assertSchemaUpToDate` 缺列拒启；读路径会把围栏一起读回。
- **「先 DDL 后代码」由机器保证只限本 zone 库**：启动闸只核对配置里的 `DBName`。其它落点库在第一次被指向时才按需打开，打开时过同一道只读 schema 闸，失败包成 `ErrStoreUnavailable`（`store_registry.go`）[C]。所以漏迁的落点库不会拒启，表现为写不耗预算地延后（`db_task_deferred_total{reason="store_open_failed"}`，被 L-04 的 critical 告警 `DbTaskDeferredByStore` 覆盖）和冷读立即失败。补完 DDL 后按需打开在 10 秒退避后自动成功，不用重启；期间写只在 ready 里有一份，压在 noeviction 的 Redis 上。兜底靠 §8.1 的库清单穷举与 §8.2 的观察项。
- `player_centre_database` 也以 player_id 为主键，但没有任何 write 生产者 [C]，不加围栏。
- **纪律**：任何新表成为 DBTask write 目标时，必须在同一次改动里加这两列围栏，并进入 §4.1 的启动断言列表。否则它在 `legacy` / `dual` 下都按非围栏表走 `Save`（§4.1），条件写对它不生效。

### 3.3 PlayerAllData（`proto/common/database/player_cache.proto:30-37`）

- 新增 `uint64 last_save_seq = 7;`（现用 2~6；1 号历史上空着，不占用）。在途实现已声明（未提交），注释是「存盘发号水位，跨重接和跨场景继承，不承载业务数据」[C]；保留字段，注释改为下面两条的口径。
- 语义：这份父键快照发出时用的 save_seq。只用于 C++ 加载时播种（§7.2）。
- 它是非业务元数据，**不得参与任何相等比较**（§7.3）。

### 3.4 帮会二期的加列放行清单

**v0.4 现状：已完成，本批不再改这些清单。** 2026-10-15 先行声明 18 / 19 / 3 / 4 时，G-03 放行清单已同批更新，之后玩家收件箱又取了 20 号（`inbox_ledger`）。G-03 最新一条（`90-consistency.md` G-03 节）是放行与核对口径的唯一来源：`player_database` 六条 ADD COLUMN（`profile_component` / `asset_op_ledger` / `settlement_ledger` / `fence_owner_epoch` / `fence_save_seq` / `inbox_ledger`），`player_database_1` 两条（`fence_owner_epoch` / `fence_save_seq`），只放行尚未迁移的那几条；**帮会剩余批次取号现从 21 起** [C]。`03-names.md` §3.15a 与 `92-handoff.md` 步骤 4 已同步；下表第 4、5 处原文已加注改按最新清单放行（见 G-03 最后一条）；第 6、7 处（G-05 的列数说明、合服 runbook §4.4）落码时再核一次是否已随之更新，没更新就按 G-03 最新清单补注。

以下是 v0.3 的原文，保留作记录：`docs/design/guild-phase2/90-consistency.md` 的「G-03 `player_database` 加列与迁移」（:297-309）要求：给 `player_database` 新增字段的那次改动，必须同时把新列追加进各处放行清单，否则那一批的执行者会在不认识的新列上停下。四个新列要追加到：

| # | 位置 | G-03 是否点名 |
|---|---|---|
| 1 | `guild-phase2/90-consistency.md:306`（G-03 第 3 条） | 是 |
| 2 | `guild-phase2/03-names.md` §3.15a（:598-602、:606-607），以及 §3.20 的验证命令（:708-709） | 是 |
| 3 | `guild-phase2/92-handoff.md` 步骤 4（:306-308、:313、:315） | 是 |
| 4 | `guild-phase2/04-asset-channel.md` §4.42 第 1b 步（:1643） | 否 |
| 5 | `docs/design/turn-battle-gap-closure.md` §9.3 验证清单第 1 步（:355-361） | 否 |
| 6 | `guild-phase2/90-consistency.md:327`（G-05 的列数说明） | 否 |
| 7 | `docs/ops/merge-zone-runbook.md` §4.4（:202-219） | 否 |

同时把 G-03 的「帮会剩余批次取号从 18 起」改为从 20 起（v0.4：已由 G-03 改为「现从 21 起」，此句作废）。

### 3.5 DDL

- 由 `go/db/cmd/migrate` 生成并执行，不手写。Drift 对缺列生成 `ALTER TABLE … ADD COLUMN`，不带 `AFTER`，所以追加在末尾（`internal/migrate/plan.go:149-152`）。
- 围栏四列的语句形如 ``ALTER TABLE `player_database` ADD COLUMN `fence_owner_epoch` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:18'``（uint64 的类型映射在 proto2mysql 的 `proto2mysql.go:486`）[I，未运行]。v0.3 写的「预期 4 条」已过时：四列与收件箱的 20 号列已在 HEAD 声明，按落地恢复交接 §4 第 2 步要在任何新版 go/db / scene 启动前迁完，plan 的放行口径以 G-03 最新清单为准（§3.4、§8.1），已迁过的库 plan 为空。
- 每个 zone 库跑一次；每个落点库加 `-storage-id <id>` 再各跑一次（`-storage-id` 接受 zone 编号）。库清单按 §8.1 第 2 步穷举，不能只靠记忆或 `tools/merge_zone/storage_audit.go`（它只审计给定 `-storage` 的一个库）。
- 存量行的默认值 `(0,0)` 表示「还没见过带版本的写」，不做回填。

---

## 4. go/db：条件写

### 4.1 入口

- **签名**：L-04 第 1 批已把 `dbOpHandler` 改成 `func(ctx context.Context, store *proto2mysql.DB, task *db_proto.DBTask, msg proto.Message) error`。本设计在这个 error 签名上把写 handler 扩成 `(writeOutcome, error)`，`runDBOp` / `processDBTask` 同步扩成返回 `(writeOutcome, error)`。替换 `dbOpHandlers` 或固定 `runDBOp` 行为的测试要一起改：`key_ordered_consumer_test.go` 的 `recordingHarness`、`consumer_shutdown_test.go`（约 :238）、`run_db_op_test.go`（后两者有 go/db 会话的在途改动，按 §0.2 合并前置 rebase）。
- **被拒的结局不编码成 error**：`runDBOp` 在 handler 返回后先调 `route.reportStoreResult(handlerErr)`，收到非 nil 时既不会 `ReportHealthy`，连接类错误还会触发探活（`store_failure.go`）[C]。所以 SQL 已正常应答的拒绝（`equal` / `older_seq` / `stale_owner` / `legacy_after_seq`；`owner_epoch_regressed` 由 `handleTask` 探测后改判，§4.4）一律以 `(outcome, nil)` 返回，`reportStoreResult` 只拿 SQL 这一步自身的错误上报。在途实现把 `errPlayerRowStale` 先当 error 交给 `reportStoreResult`、再改写成 nil 走成功分支（§0.2 第 11 行），两处都违反本条。
- **错误标记（R1 第 2 条）**：`handleDBWriteOp` 对条件写路径返回的错误一律套 `storeerr.Tag`，不论底下是 `SaveConditional` 还是过渡实现的事务（§4.8）。两者都用 `%w` 保留驱动错误，所以 1205 / 1213 / 9007 / 8002 与连接类错误照常按 L-04 分类；条件写自己的哨兵（`ErrConditionalSaveRace` 等）与过渡实现的「不可能分支」错误不是驱动错误，归 `KindNone`，按 §4.4 错误表处理。在途 `if handled, err := savePlayerRow(...); handled { return err }` 原样返回事务错误，MySQL 宕机或 1205 / 1213 时玩家写会耗尽预算进死信、`conn_lost` 不触发探活，等于对两张玩家表回退了 L-04（fence-inflight.md F4）。`storeerr` 标记只用于 SQL 调用点（L-04 §3.2），Redis 侧的新错误不得复用它。
- **写 SQL 的 ctx 不改**：HEAD 已是 `store.WithContext(ctx).Save(msg)`，ctx 是租约的 `sqlCtx`（`lease.beginSQL`，只在失租时取消）[C]。条件写（含 class-2 两步、过渡实现的整个事务）沿用它。v0.3 的「`handleDBWriteOp` 不再丢弃 ctx、改为 `WithContext`」工作项删除（§4.6）。
- 顺序：大字段闸（不变）→ 围栏表判定（见下）→ 按模式与类别给消息盖围栏（§4.2）→ `fenceSave`（§4.3、§4.8）→ 把库的结局归类成 `writeOutcome`（§4.4）。
- **「是否走了 §5.2 的 override」**在 SQL 之前由 `orderingForTask` / `guardOwnerEpoch` 决定，记在 `workerTask` 上（新增字段），由 `handleTask` 尾部的推进脚本（§5.5）读取。不塞进 handler 返回值。
- `route.store` 的类型仍是 `*proto2mysql.DB`，不需要改成 `*GameDB`：状态查询由库自己做，go/db 不再手写 SELECT。

**围栏表判定**（只算一次；§4.2、§4.3、§4.4、§5.2、§5.3 都引用这一个结果，不各自判断）：

- 启动时遍历 go/db 已注册的表（`go/db/data/mysql_database_table_list.json`，9 张），描述符同时含顶层、非 optional 的 uint64 字段 `fence_owner_epoch` 与 `fence_save_seq` 的才算围栏表，结果按 MsgType 缓存。在途 PRF 按表名硬编码、字段缺失时逐条报错，改为本条（新表成为写目标时自动进入判定，已知表缺字段即拒启）。
- 启动时断言 `player_database`、`player_database_1` 在这个集合里，不在就拒启，避免已知表静默退化成无围栏。
- 非围栏表在 `legacy` 和 `dual` 下一律沿用 `store.Save`：不盖章、不分类、不放开游标（§5.2），计 `db_fence_write_total{class="unfenced"}`，每个 MsgType 首次出现时打一行 ERROR。
- 理由：写任务不按表区分，`processTaskWithoutLock` 按 `task.MsgType` 在全局注册表里找描述符，9 张表都能走到 `handleDBWriteOp`，其中 7 张没有围栏列。P2M 在拼计划阶段对不在表里的保留列 / 栅栏列返回 `ErrInvalidSaveOptions`（P2M:214-218、:263-267），不加判定时 `legacy` 的 `PreserveFields` 与 `dual` 的 `FenceFields` 在这 7 张表上必然失败，按 `deferReason=other` 耗完预算后以 `budget_exhausted` 进死信 [C]。目前没有第三张表的写生产者（§2），所以这是潜在问题。
- 盖章只对围栏表做，所以 dynamicpb 按字段名取描述符不会拿到 nil，不存在对 nil 描述符 `Set` 而 panic 的路径。

### 4.2 盖围栏

go/db 在调用前把围栏写进 dynamicpb 消息的两个字段（只对围栏表；两种模式都盖）：

| 模式 / 类别 | `fence_owner_epoch` | `fence_save_seq` |
|---|---|---|
| `dual` class-1 | `task.OwnerEpoch` | `task.SaveSeq` |
| `dual` class-2 | `task.OwnerEpoch` | 0 |
| `dual` class-3 | 0 | 0 |
| `legacy`（任意类别）| 0 | 0 |

- **`legacy` 也置 `(0,0)`**（v0.3 §4.2 与 §8.2 说法矛盾，v0.4 以本条为准）。它只影响插入新行：`legacy` 走 `PreserveFields: [fo, fs]`，更新已有行时根本不写这两列，插入新行时照写消息里的值。不盖章的话，body 里一旦带了非 0 围栏（在途 C++ 就是这样），`legacy` 的插入会把它写成新行的围栏。
- C++ 每次都从组件全新构造消息（PL:3587-3595），v0.4 删掉在途 C++ 对 body 围栏的写入之后，body 里这两个字段恒为 0（§0.2 第 19 行）。**两种模式下**，盖章前发现 body 里任一围栏字段非 0，都说明有别的生产者或生产端有缺陷：打 ERROR（`FENCE-BODY-MISMATCH`），计 `db_fence_body_mismatch_total`，以盖章值为准。

### 4.3 模式 × 类别 → `SaveOptions`

记 `fo = "fence_owner_epoch"`，`fs = "fence_save_seq"`。下表只对围栏表；非围栏表照旧 `Save`（§4.1）。表中的 `SaveOptions` 在 v0.4 里是接缝函数 `fenceSave` 的参数 `fenceSaveOptions`，字段与 P2M 的 `SaveOptions{FenceFields, AllowEqual, PreserveFields}` 同形；`SaveConditional` 与过渡实现（§4.8）对同一组参数给出同样的结局。

| 模式 | 类别 | 调用 | 写入条件 |
|---|---|---|---|
| `legacy` | 任意 | `SaveOptions{PreserveFields: [fo, fs]}` | 无条件。已有行的围栏列不动；新行的围栏为 0 |
| `dual` | class-1 | `SaveOptions{FenceFields: [fo, fs]}` | 库内 `(fo, fs)` < `(E, s)` |
| `dual` | class-2 | 第一步 `{FenceFields: [fo, fs], AllowEqual: true}`；被 `(E, s>0)` 拒绝时第二步 `{FenceFields: [fo], AllowEqual: true, PreserveFields: [fs]}` | 第一步：库内 ≤ `(E, 0)`。第二步：库内 `fo` ≤ `E`，不动 `fs` |
| `dual` | class-3 | `{FenceFields: [fo, fs], AllowEqual: true}` | 库内围栏等于 `(0, 0)` |

说明：

- **`legacy` 不能调 `Save`（对围栏表）。** 新二进制的消息里带着值为 0 的围栏字段，`Save` 会把已有行的围栏清零。`PreserveFields` 的语义是「更新已有行时不写这些列，插入新行时照常写」（P2M:62-65）[C]。只带 `PreserveFields` 时没有任何保护，这正是「等同今天」。
- **class-1 用严格模式。** 同版本重放的结局是 `SaveRejected` 且 `Stored` 等于消息围栏，调用方据此判为 `equal`（P2M:57-59），只看围栏、不读回整行比内容（§4.4）。
- **class-2 的第二步**解决附录复核指出的问题：`<= E` 的条件写 `(E, 0)` 会把同一 epoch 内已有的 `(E, s>0)` 退回 `(E, 0)`。第二步只比 epoch，并保留库内的 seq，围栏不退。class-2 在同一 epoch 内的先后仍然靠 Redis 的排序锁与游标（§5.1）。
- **class-2 第二步的缓存围栏（可选）**：第二步之后缓存里的消息围栏是 `(E, 0)`，行内 `fs` 被保留，缓存围栏低于行内（§6.3）。可选的修补：第一步 `SaveRejected` 时 `Stored` 必定存在且含 `[fo, fs]`（P2M:100-104），第二步把第一步读到的 `fs` 回填进消息再发布缓存。两步之间同一 `fo` 下行内 `fs` 只增不减，所以回填值是下限，安全。收益很小（class-2 只出现在 C++ 滚动或回退窗口，还要叠加冷加载和时钟回拨才有用）。**已定不做**（§14 第 18 条）。
- **class-3 只在行上没有围栏时写**（修 B3）。行上已有围栏说明带版本的写已经落过，这时一笔没有任何版本的写无法判断新旧，不能覆盖。严格模式下全 0 围栏会被库判为非法选项（P2M:209-212），所以 class-3 必须带 `AllowEqual`。
- 不可能出现的组合（例如严格模式被拒但 `Stored` 比消息旧）返回错误、不 ack。

### 4.4 结局处理

| `writeOutcome` | 来自 | 落点复核 `route.confirm()` / 核锁 | 缓存回写 | 资产 SQL 提交证据（§6.6）| 游标 / 已落库 epoch | 终态 |
|---|---|---|---|---|---|---|
| `applied` | `SaveUpdated` / `SaveInserted` / `SaveUnchanged`（过渡实现：inserted / updated，§4.8）| 复核做；核锁由令牌发布脚本完成（§6.2）| 写（令牌脚本）| `dual` + class-1 + `player_database` 时发布（同一次 EVAL）| §5.5 的脚本推进。正常路径：游标只进不退；override 路径（§5.2）：游标改写为本任务的 (partition, seq)，允许后退。两条路径都对已落库 epoch 取 max | ack |
| `equal` | class-1 被拒，`Stored == (E, s)` | 同 `applied` | 写（补发上次可能没发成的缓存）| 同 `applied`；同版本同内容幂等，同版本不同账本字节时脚本拒绝发布（§6.2 返回 -1）| 同 `applied`（失租后重试得到 `equal` 也走同一脚本，§5.4）| ack |
| `older_seq` | class-1 被拒，`Stored.fo == E` 且 `Stored.fs > s` | **复核做，核锁做**（`lease.verify`）| 不写 | 不发布 | **不推进** | 复核与核锁都通过 → ack 丢弃并计数；复核不过 → 按 `errPlacementMoved` 延后（不耗预算）；核锁失败 → `errOrderingLeaseLost` 延后 |
| `stale_owner` | class-1 / class-2 被拒，`Stored.fo > E`，且计数器探测没有发现倒退（`dual`，见下）| 不做（理由见 §4.5）| 不写 | 不发布 | 不推进 | **进死信**（label `fence_stale_owner`），计数，打 `STALE-OWNER-WRITE` |
| `owner_epoch_regressed` | 同上，但 `player:{id}:owner_epoch` 计数器 < `Stored.fo`（含键缺失）| 不做 | 不写 | 不发布 | 不推进 | **进死信**（label `owner_epoch_regressed`），计数，打 `OWNER-EPOCH-REGRESSED` |
| `legacy_after_seq` | class-3 被拒（行上围栏不是 `(0,0)`）| 不做 | 不写 | 不发布 | 不推进 | **进死信**（label `fence_legacy_after_seq`），计数，打 `FENCE-LEGACY-AFTER-SEQ` |

被拒的四种结局都以 `(outcome, nil)` 从 `runDBOp` 交回 `handleTask`，由 `handleTask` 按上表处理，**不进成功分支**：不调推进脚本、不 ack 成「已落库」。死信一律经 L-04 第 1 批带 label 的 `quarantineOrderingConflict`，R2 落地后自动继承 S1 的 WAITAOF（Kafka 来源在死信持久化之后才 ack）。

错误的去向（按 L-04 的词表落定三件事：`deferReason` 的取值、是否进 `retryBudgetExempt`、死信 label）：

| 错误 | 含义 | 去向 |
|---|---|---|
| `ErrConditionalSaveRace` | 严格模式下 UPDATE 影响 0 行、读到的状态又满足条件，两轮都这样（P2M:504-507、:521-522）| 延后，**耗预算**（不进 `retryBudgetExempt`），`deferReason=fence_race`（新增，启动时以 0 建出序列）；用完以 `budget_exhausted` 进死信。理由：`dual` 持有排序锁时，严格模式下的 Race 只可能来自围栏倒退（人工改库、PITR、`debug_import` 并发执行）或 SQL 谓词与 Go 侧 `conditionHolds` 判定不一致，不是瞬时现象；不设上限会无界空转。TiDB 乐观事务的写冲突以 9007 / 8002 / 1213 返回，已由 L-04 归入 `store_busy`，不表现为 Race |
| `ErrSaveMatchMismatch` | 整行比较与 MySQL 的「值是否变化」不一致，多半是表结构问题 | 延后，耗预算（`deferReason=other`），最终进死信 |
| `ErrInvalidSaveOptions`、`ErrSchemaDrift`、`ErrMultipleRowsFound` | 配置或表结构错误 | 同上 |
| 带 `storeerr` 标记的驱动错误（连接数满、连接断开、只读、1205 / 1213 / 9007 / 8002）| 存储侧环境问题 | 按 L-04：写任务延后、不耗预算 |
| 语句已发出后的超时、连接断开 | 结局未知，那条语句可能已经生效 | 按 L-04 的分类延后；重试时 class-1 会得到 `equal` 或 `applied`，天然幂等 |
| 计数器探测读失败（§5.1 与本表 `stale_owner` 行的探测）| Redis 侧 | `deferFailedTask`，耗预算，`deferReason=redis_unavailable`（L-04 §4.3 口径）；用完以 `budget_exhausted` 进死信，载荷不丢。有意 fail-closed，不进 `retryBudgetExempt` |
| §5.5 推进脚本、§6.2 令牌发布脚本返回 0 | 已失租 | 调 `lease.setLost(owner_changed)`，返回包 `errOrderingLeaseLost` 的错误（`deferReason=lease_lost`），耗预算（§5.4） |
| 两条脚本的 Redis 往返错误 | Redis 侧 | 失租判定与 `lease.verify` 同口径：ctx 取消或单步超时不算失租；令牌发布脚本的其余往返错误按 `verify_error` 记失租。都按 `redis_unavailable` 延后、耗预算 |
| §6.2 令牌发布脚本返回 -1 | 资产证据同版本、账本字节不同：同一 `(E, s)` 出现了两份不同内容（§7.4 残余或 dev 旁路下的撞号）| 什么都不写（缓存与证据都不动）。计 `db_fence_cache_publish_total{path="write",result="evidence_conflict"}`，打 `ASSET-SQL-COMMIT-CONFLICT`；延后、**耗预算**，`deferReason=fence_evidence_conflict`（新增，启动时以 0 建出序列），用完以 `budget_exhausted` 进死信。理由同 `fence_race`：确定性冲突，重试不会变，不设上限会空转 |
| 过渡实现的不可能分支（§4.8：撞 1062 后重读仍无行等）| 表结构或并发判定缺陷 | 不带 `storeerr` 标记，延后、耗预算（`deferReason=other`），最终进死信 |

死信入口统一用 L-04 第 1 批带 label 的 `quarantineOrderingConflict(task, dbTask, label, reason)`：Kafka 来源先 LPUSH 到死信再 ack；重试来源把收据原子搬进死信。`go/db/internal/metrics/metrics.go` 新增三个常量 `DeadLetterFenceStaleOwner = "fence_stale_owner"`、`DeadLetterFenceLegacyAfterSeq = "fence_legacy_after_seq"`、`DeadLetterOwnerEpochRegressed = "owner_epoch_regressed"`，追加进 `deadLetterReasons` 让启动时预建序列，同步改 `db_task_dead_letter_total` 的 Help（那里的注释要求 Help 与常量一致）。这三个原因各有 §10 的专用 critical 告警；v0.4 已定在 L-04 的 `DbTaskDeadLetterAdded` 表达式里排除它们，避免同一件事响两次（§10、§14 第 16 条）。

要点：

- **`older_seq` 先复核、再核锁、最后 ack**（附录复核的修正；v0.4 按 R1 第 1 条补上核锁）。判 `older_seq` 只证明「被选中的那个库」里有更新的版本，不证明那个库仍是真源。复核不过时按 `errPlacementMoved` 延后，重投到新库。它不推进游标与已落库 epoch：被丢弃的写没有落库，推进游标会让同分区里 offset 更小、但版本更新的重试被游标判 stale 丢掉（fence-inflight.md F1 (c) 的时序）。
- **`stale_owner` 不 ack 丢弃**（修 B1）。正常情况下真正的僵尸写在前面的 Redis epoch 守卫就被处理了（§5.1）；能走到 SQL 才被判 stale，说明 Redis 里的已落库 epoch 记录缺失或落后。`dual` 下这里再探测一次计数器（`GET player:{id}:owner_epoch`，判据同 §5.1）：计数器 < `Stored.fo` 归为 `owner_epoch_regressed`，否则记 `stale_owner`，两者都进死信保住载荷。handler 不碰 Redis：它只返回 `stale_owner` 与 `Stored`，探测与重新归类在 `handleTask` 拿到结局之后做（Redis 步骤名 `owner_epoch_probe`，ctx 按 §4.6）；探测读失败按上表延后。计数器倒退而已落库 epoch 没有倒退时，写根本到不了 SQL，由 §5.1 的探测接住（§9.1）。
- **`equal` 按幂等生产者口径处理：同一 `(E, s)` 视为同一笔写的重放，不比内容**（§14 第 12 条，2026-10-16 已定，v0.3 方案 1）。复核后补发缓存（与证据），再走 §5.5 推进。
  - 这与幂等生产者的通行做法一致：去重键由生产者保证唯一，消费端按键去重，不回读载荷比对。唯一性的保证在 C++：进程级发号器对本进程内所有存盘严格递增，新进程靠加载播种与墙钟下限越过旧进程（§7.2）。撞号只剩两条路：§7.4 的四个条件同时成立（其中第 4 条要求分钟级的时钟回拨或主机间偏差），以及只允许单机 dev 的 `AllowUnsafeCrossNodeHandoff` 旁路。
  - 不读回比对的理由：proto2mysql 把「置位的空子消息」写成空串、读回为未置位，而 C++ 总是置位全部子消息（`player_database_loader.cpp` 的 `mutable_xxx()->CopyFrom`），`proto.Equal` 在没有资产 / 结算 / 收件箱记录的玩家身上必然判不等。失租、缓存或证据发布失败之后的同版本重试正是失租恢复的正常路径（§5.4、§6.2），读回比对会把它们全部变成「同版本冲突」，耗尽预算进死信（fence-inflight.md F2）。比对还要再读一次整行。
  - 撞号的检测放在资产证据层：同一 `(E, s)` 的账本字节不同时，令牌发布脚本拒绝发布、计数并告警，任务以 `fence_evidence_conflict` 延后（§6.2、上面错误表）。这覆盖了后果最重的资产路径；其余字段的撞号由 `cmd/verifier` 事后核对，是 §7.4 残余风险下的已知代价。
  - 不采用「`equal` 不发布缓存」与「令牌脚本里删缓存键」（§15）：前者让失租恢复后的缓存留在上一版；后者每次 `equal` 多一次冷读，且仍不处理证据。

### 4.5 `runDBOp` 的顺序

v0.3 写这一节时，核锁（verify）还没挪进 `runDBOp`。v0.4 以 go/db 接手会话定稿的步骤顺序为准（R1，2026-10-16 通知；括号里是 `db_task_redis_step_seconds` 的 step 名）[C]：

```
runDBOp（写任务）
  lease.beginSQL                    → sqlCtx（只随失租取消）；失败即返回，一条 SQL 都没发
  SQL                                → handler 返回 (outcome, sqlErr)
  finish(sqlErr)
  失租分支：sqlCtx 已取消            → 按取消原因返回，不报注册表
  route.reportStoreResult(sqlErr)    → 只上报 SQL 这一步；被拒结局 sqlErr == nil
  sqlErr != nil                      → 返回 sqlErr
  被拒结局返回（第一处）：stale_owner / legacy_after_seq → return (outcome, nil)
  route.confirm                      (placement_recheck)
  被拒结局返回（第二处）：older_seq → lease.verify (lease_verify) → return (older_seq, nil)
  applied / equal                    → 令牌发布脚本 (cache_publish)，取代 lease.verify (lease_verify) + 缓存 SET (cache_set)，§6.2
                                     → return (outcome, nil)
handleTask（runDBOp 返回 err == nil 之后）
  applied / equal                    → 推进脚本 (applied_advance)，取代 markAppliedSeq (cursor_set) + markAppliedOwnerEpoch (epoch_mark)，§5.5
                                     → LREM 重试收据 / ack Kafka
  older_seq                          → 计数 → LREM / ack；不推进游标与 epoch
  stale_owner                        → dual 下计数器探测 (owner_epoch_probe) → 归 stale_owner 或 owner_epoch_regressed → quarantineOrderingConflict
  legacy_after_seq                   → quarantineOrderingConflict
```

- 读任务不变：选库 → SQL → `reportStoreResult` → 复核 → 读路径 `SET NX`（`cache_set`）与结果发布。
- 步骤的先后与 go/db 会话定稿的一致，v0.4 只做三件事：在两处插入被拒结局的出口；把 `lease_verify` + `cache_set` 合成一条脚本；把 `cursor_set` + `epoch_mark` 合成一条脚本。step 名的变化见 §10，需要与该会话协调（§12）。
- 被拒的结局都以 `(outcome, nil)` 返回（§4.1），`reportStoreResult` 收到 nil，按「SQL 正常应答」处理。
- 复核 MGET 不能因为有了围栏而省掉：围栏是按库内的行算的，看不出「这个库已经不是真源」（`player-storage-placement.md` §6.3）。
- **往返数**（新二进制、`dual`、class-1、结局 `applied`）：SQL 之后是 `placement_recheck`、`cache_publish`、`applied_advance`、`lock_release` 四次；HEAD 是 `placement_recheck`、`lease_verify`、`cache_set`、`cursor_set`、`epoch_mark`、`lock_release` 六次，在途实现每笔 `player_database` 写还多一次证据 EVAL。v0.4 每笔写少两次往返，证据不另花往返。`older_seq` 路径 SQL 之后是 `placement_recheck`、`lease_verify`、`lock_release`；`stale_owner` 路径多一次 `owner_epoch_probe`。

**R1 第 1 条（stale 分支不得跳过 confirm / verify）的落法**：在途实现把所有被拒的写当成功，在复核与核锁之前返回 nil，随后推进游标并 ack。v0.4 的规则是**只有丢弃需要证明**：

- 终态为 ack 的结局（`applied`、`equal`、`older_seq`）一律先过 `route.confirm`、再过核锁（`older_seq` 用 `lease.verify`；`applied` / `equal` 用取代它的令牌发布脚本）。ack 是不可逆的，必须在「选中的库仍是真源、锁仍归本实例」两个条件下做出。
- 终态为死信的结局（`stale_owner`、`owner_epoch_regressed`、`legacy_after_seq`）不复核、不核锁就交回 `handleTask`：死信保住整份载荷，不碰游标、epoch、缓存与证据，误判的代价是一条要人处置的死信，不是丢盘。另外围栏随行在搬库与合服时一起拷贝（§9.3），旧库上判出的「归属更老 / 无版本写」在新真源上同样成立。这一点没有对所有落点变更路径逐一核实，所以对不可逆的 ack（`older_seq`）仍保留复核，不按它放宽。
- 失租的僵尸实例走到死信分支时可能与新属主各写一条死信，内容相同，只是多一条要人看的证据；不会因此丢盘。

### 4.6 超时与取消

- **连接级超时已由 L-04 落地，直接复用。** 业务连接池 `newServiceMysqlConfig`（`proto_sql/db.go`）在 `newMysqlConfig` 之上设了五项：拨号 5 秒、读 30 秒、写 30 秒，以及会话级 `lock_wait_timeout`、`innodb_lock_wait_timeout` 10 秒 [C]。本设计不新增超时配置键。迁移入口 `NewMysqlConfig` 不带这些，不受影响。过渡实现要在这里加一项会话级隔离级别（§4.8）。
- **写 SQL 用租约的 `sqlCtx`，停机不取消写 SQL。** v0.3 写的「`handleDBWriteOp` 仍丢弃 ctx，改成随停机取消」已过时，并且与 G1 治标冲突。HEAD 现状：`handleDBWriteOp` 已是 `store.WithContext(ctx).Save(msg)`；ctx 来自 `lease.beginSQL`，以 `context.Background` 为父，只在失租时取消；停机、claim 收回、worker ctx 取消都不取消在途写 SQL；失租取消后释放阶段不删锁，留给 TTL（`sqlAbandoned`）（`ordering_lease.go` 文件头第 1 条）[C]。照 v0.3 让写 SQL 随停机取消的后果：Stop 第④步取消 worker ctx 后 `sqlCtx` 本身没被取消，`finishSQL` 不置 `sqlAbandoned`，释放阶段立即删锁，而服务端仍可能提交那条语句；`legacy` 下没有围栏，别的实例写入新版本之后旧语句再提交就是回档（fence-v03-review.md F5）。所以条件写（含 class-2 两步、过渡实现的整个事务）一律沿用 `sqlCtx`。
- **SQL 之后的 Redis 步骤用 `context.WithoutCancel(ctx)` 加单步上限**：复核 MGET、令牌发布脚本、`older_seq` 的 `lease.verify`、§5.5 推进脚本、`owner_epoch_probe`。写法同 `releaseOrderingLock`（`context.WithTimeout(context.WithoutCancel(ctx), orderingLockReleaseTimeout)`，2 秒；落码时可单独定常量，不超过这个量级）。
  - 理由：SQL 已经发出、可能已经提交，这时跟着 worker ctx 取消只会让任务延后、耗预算，每次滚动重启都在 Stop 第④步制造一批 `redis_unavailable` 噪声；单步上限保证 Stop 第④步的等待有界。
  - 失租判定：ctx 取消或单步超时不算失租，任务延后（与 `verify` 现有口径一致）。
  - 与 go/db 会话在途 Stop 注释的关系：它在第②步超时分支里写「SQL 之后的复核、发布、`markAppliedSeq` 以及重试源的读会因 ctx 已取消而失败」。v0.4 落码时这段注释要同步改成「SQL 之后的 Redis 步骤不随 worker ctx 取消，各有单步上限」，第④步的最坏等待随之变成「在途 SQL（业务池读超时 30 秒）+ SQL 之后各步上限之和」。这是该会话的文件，按 §0.2 合并前置与它对齐。
  - 死信入口 `quarantineOrderingConflict` 与 ack 不在此列，沿用现状。
- 语句被取消或超时后，驱动会关掉那条连接，服务端可能仍把语句执行完。这类错误一律按「结局未知」处理：延后重试，靠围栏幂等。**治标措施撤不回已经进入 MySQL 的语句，挡住旧写的是围栏。**

### 4.7 配置

```yaml
Fencing:
  Mode: legacy          # legacy | dual；缺省 legacy
  EpochOverride: false  # 只在 dual 下生效；开启条件见 §8.3
```

- **模式名不用 `off`**：go/db 依赖的 go-zero v1.10.0（`go/db/go.mod`）解析 YAML 用 `gopkg.in/yaml.v2`（YAML 1.1），裸写的 `off` 会被解析成布尔 `false`；`core/mapping` 对 string 字段收到 bool 返回类型不匹配，go/db 的三个入口（`db.go`、`cmd/migrate`、`cmd/verifier`）都用 `conf.MustLoad`，直接退出 [C，读本机 module cache 中 go-zero v1.10.0 的源码确认，未运行]。带引号的 `"off"` 能用，但只能靠纪律保证，而且如果模板在 Stage 1a 省略这个键，问题会拖到紧急回退时才暴露（滚动卡住或 CrashLoop）。`legacy`、`dual`、`sql` 都不撞 YAML 1.1 的布尔字面量。
- go/db 的配置没有独立的校验函数，默认值统一在 `Normalize()` 里补（`internal/config/config.go`）[C]。`Mode` 用 go-zero 的 `options=|legacy|dual` 约束（同 `AllowlistEnforcement` 的 `options=|strict|warn` 写法，允许显式空值），空值在 `Normalize()` 里补成 `legacy`。写了 `sql` 或其它值即拒绝启动。
- `EpochOverride` 是 bool，缺省 `false`；`Mode=legacy` 时忽略（打一行 WARN）。启动日志打印 `Mode` 与 `EpochOverride` 的生效值，切换后用它核对。
- K8s 上 db 的 ConfigMap 模板（`tools/scripts/k8s_deploy.ps1` 的 `New-GoSvcConfigMapYaml` 里 `"db"` 块）不会自动带上 `db.yaml` 的新键 [C]，必须手改模板，并在 `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 加断言：模板输出的 `Mode` 取值、`EpochOverride` 取值都在约定集合内。该文件目前有另一个会话在改，落码前先对齐。
- 配置用例见 §11 第 2 步。
- 在途实现没有任何模式开关，新二进制一上线就等于强制 `dual`，而且 class-2 被盖成 `(0,0)` 后静默丢弃，C++ 滚动或回滚时整批丢盘（fence-inflight.md F1 (a)、fence-v03-review.md F2）。v0.4 照本节加 `Fencing.Mode`（缺省 `legacy`）与 `EpochOverride`。
- **2026-10-16 已先落 `Mode`（未编译）**，与上文的出入以代码为准：
  - 位置按 go/db 会话约定放在 `ServerConfig.Fencing.Mode`（独立结构体，不进 `KafkaConfig`；`db.yaml` 写在 ServerConfig 段末尾，避开脚本正则解析的 Kafka 段）。校验接入 go/db 新增的 `(*Config).Validate()`（大小写敏感，非法拒启），空串由 `EffectiveMode()` 解释为 `legacy`，没有用 `options=` 标签。
  - 注入：`db.go` 在构造消费者之前调一次 `kafka.SetFencingMode`（包级 `atomic.Bool`，启动期一次、运行中不可改）；`legacy` 时 `savePlayerRow` 返回 handled=false、走普通 Save，`publishDBWriteResult` 不发布资产证据。
  - **过渡实现下 `legacy` 的语义与上文不同**：普通 `Save` 会把行内围栏列写回 body 里的值（C++ 新版恒为 0），不是 `SaveConditional` 的 `PreserveFields` 保留。所以 `dual → legacy` 仍随时安全，但 `legacy → dual` 再切回来之前要先过 §8.3 的排空门禁，否则 legacy 期间写过的行围栏为 `(0,0)`，排在重试里的旧版本写可能被接受。换成 `SaveConditional` 后恢复上文语义。
  - 未落：`EpochOverride`（随 §5.2 一起做）；K8s ConfigMap 渲染（go/db 会话的 `k8s_deploy.ps1`，其 R5b 之后补；在此之前 K8s 上恒为缺省 `legacy`）。

> **2026-10-18 更正（存盘协议世代门禁，审计 [data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.2）**：本节的「缺省 `legacy`」与上面「`dual → legacy` 仍随时安全」都已作废。10-17 起 C++ 只要 `owner_epoch≠0` 就走严格提交屏障，login 要求两表 ACK 都等于 pending，而只有 `dual` 写 ACK、跑补投，所以缺省 `legacy` 下玩家第二次登录起全部失败。现行缺省改为 `dual`：`db.yaml` 显式写 `Mode: dual`，`EffectiveMode()` 把空串解释为 `dual`，K8s 原样镜像。`legacy` 改为机器门禁下的显式选项，见文末「2026-10-18 存盘协议世代门禁与补投有界」。

### 4.8 原语与过渡实现（v0.4 新增）

**接缝**：围栏表的写只经过一个函数：

```go
// fenceSave 按 opts 对 msg 做围栏条件写。只返回库的结局，不做模式 / 类别判断，也不碰 Redis。
// 换原语（过渡实现 → SaveConditional）只改这个函数体。
func fenceSave(ctx context.Context, store *proto2mysql.DB, msg proto.Message, opts fenceSaveOptions) (fenceSaveResult, error)

type fenceSaveOptions struct {
    FenceFields    []string // 字典序比较的围栏列，高位在前
    AllowEqual     bool     // true：库内 ≤ 消息即写；false：库内 < 消息才写
    PreserveFields []string // 更新已有行时不写这些列；插入新行时照写
}

type fenceSaveResult struct {
    Kind   fenceSaveKind // inserted | updated | unchanged | rejected
    Stored fenceValue    // Kind == rejected 时必有：库内 FenceFields 的当前值
}
```

- 字段与 P2M 的 `SaveOptions` / `ConditionalSaveResult` 一一对应，§4.3 的模式 × 类别映射、§4.4 的结局归类只看 `fenceSaveResult`，不知道底下是哪种原语。
- 换原语的步骤：负责人合并 `feat/conditional-save`、打 `v0.3.0` 并推送 → `go/db/go.mod` 的 require 与 replace 两侧升到 `v0.3.0` → `fenceSave` 函数体换成 `store.WithContext(ctx).SaveConditional(msg, opts)` 加结果转换 → 跑 §11 第 2、3 步的同一组用例（过渡实现的用例原样作为回归）。

**过渡实现**（proto2mysql `v0.2.0`，现有接口）：在 `store.WithContext(sqlCtx).RunInTransaction` 里依次做

1. `FindOneByPKForUpdate` 读当前行（整行，含全部 MEDIUMBLOB 列）并加行锁。
2. 行存在：取行内 `FenceFields` 的值，按 opts 判条件（严格：库内 < 消息；`AllowEqual`：库内 ≤ 消息；字典序，epoch 为高位）。
   - 不满足：不写，返回 `rejected` 与 `Stored`。
   - 满足：把 `PreserveFields` 列的值从当前行拷进待写消息（与「更新已有行时不写这些列」等价），再 `tx.Save(msg)` 整行写。行锁已在手；class-1 是严格模式，每次写围栏都变，UPDATE 必然影响 1 行。`legacy`、class-2 第二步与 class-3 下整行可能完全相同，`Save` 的 UPDATE 影响 0 行时会走它自己的 INSERT → 1062 → UPDATE → 匹配查询，判为未变化，结果正确，只是多几条语句。`Save` 不区分 updated 与 unchanged，过渡实现一律报 `updated`（指标口径见 §10）。
3. 行不存在：`tx.Insert(msg)`，报 `inserted`。撞 `ErrDuplicateKey`（并发首写，对方先提交）时，在同一事务里重读一次 `FindOneByPKForUpdate`（这时能读到并锁住对方的行），回到第 2 步判定。重读仍无行属不可能分支，返回错误（§4.4 错误表）。
4. 不比内容：`equal` 由 §4.4 按 `Stored` 归类（§4.4）。在途 PRF 的 `proto.Equal` 与「被拒时用库内行替换 `msg`」都删掉。
5. 事务返回的错误一律 `storeerr.Tag`（§4.1）。

与在途 PRF 的差别：无行时用 `Insert` 而不是 `Save`（`Save` 的 UPDATE → INSERT 链在并发首写时会让后到的一方无条件覆盖，fence-inflight.md F5）；撞 1062 重读再判；固定隔离级别（见下）；不比内容；错误打标记；按 opts 支持 class-2 两步与 `legacy` 的保留列。

> **2026-10-16 先行修复（已落在 PRF；同日死锁审计会话真库验证通过：MySQL 26.7.0、全局 RR、一次性容器，`TestIntegrationPlayerSQLFence*` 4 条全 PASS，整轮 `lock_deadlocks = 0`、`lock_timeouts = 0`；go/db 其余包的回归未跑）**：数据库死锁审计在真库复现了在途 PRF 的跨玩家 1213——全局 RR 下对不存在主键的 `FOR UPDATE` 取间隙锁，不同新号并发首存的插入意向锁互等（记录见 `docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md` §9.6.3）。PRF 已先改为：非锁定 `ExistsByPK` 探测 → 无行则自动提交 `Insert`（不同主键的插入意向锁互相兼容）→ 撞 1062 转入事务内 `FOR UPDATE`（此时命中已提交行，只取记录锁）按版本裁决 → 行在两步之间消失返回可重试错误、不在持间隙锁的事务里插入；同版本重放不比内容、发布 SQL 当前行；错误在调用点打 `storeerr` 标记。这样**在 RR 下也不取间隙锁**，下面的隔离级别改造不再是消除死锁的前置，但仍按本节随 go/db 会话的 `db.go` 落地（统一口径、避免以后有人在锁定读事务里插入）。回归用例：`TestIntegrationPlayerSQLFenceConcurrentFirstSavesOfDifferentPlayersDoNotDeadlock`（空表、非空表各一轮）、`…OfSamePlayerKeepMaxVersion`。**残余（死锁审计确认，不算缺陷）**：同一玩家的并发首写中，若首个自动提交 `INSERT` 回滚而后面排着 ≥2 笔，仍会落入事故文档 §9.1 现场二的 1213（先到者回滚，RC / RR 都成环）；生产上同一玩家的写由 key 分片加排序租约串行，到不了，偶发时由重试吸收。

**隔离级别**：

- 现状：go/db 业务池没有设隔离级别。`newServiceMysqlConfig` 只设三项超时与两项会话级锁等待，`newMysqlConfig` 只设 `sql_mode`；`RunInTransaction` → `Transaction` 用 `BeginTx(ctx, nil)`，跟随服务端缺省（MySQL 为 REPEATABLE-READ）[C]。go/db 全仓只有在途 PRF 用到显式事务，其余 SQL 都是自动提交的单语句 [C，grep `RunInTransaction` / `.Transaction(`]。
- 做法：`newServiceMysqlConfig` 加 `myCnf.Params["transaction_isolation"] = "'READ-COMMITTED'"`（与 `sql_mode` 同一写法，驱动在每条连接建立时会话级 SET）；启动时取一条连接执行 `SELECT @@SESSION.transaction_isolation`，不是 `READ-COMMITTED` 即拒启并打印实际值。迁移入口 `NewMysqlConfig` 不加。先例：data_service 的 route store 用同一个 DSN 参数，并在启动时探测会话隔离级别（`go/data_service/internal/store/mysql.go`、`route_store.go`）[C]。
- 为什么是 READ-COMMITTED：行不存在时，REPEATABLE-READ 下两个并发首写的 `FOR UPDATE` 都拿到同一段间隙锁（间隙锁互相兼容），随后各自 INSERT 的插入意向锁互等，形成 1213 死锁（L-04 归 `store_busy`，结果正确，但每次并发首写都要死锁一次）。READ-COMMITTED 下不加间隙锁，后插入的一方等对方提交后得到 1062，按第 3 步重读再判，路径确定。已存在的行两种级别下都是记录锁。
- 换成 `SaveConditional` 后参数保留：它是自动提交的条件 UPDATE 加带 `FOR UPDATE` 的状态查询，不依赖隔离级别，保留无害。
- TiDB：READ-COMMITTED 只在悲观事务模式下生效；乐观模式下写冲突以 9007 / 8002 返回，已由 L-04 归入 `store_busy` [未核实，§11 第 3 步在 TiDB 上覆盖]。
- `newServiceMysqlConfig` 在 `proto_sql/db.go`，属 go/db 会话的文件，按 §0.2 合并前置协调。

**代价（待压测）**：过渡实现每笔围栏表写多一条整行 `SELECT … FOR UPDATE`（读回全部 MEDIUMBLOB 列）和 `BEGIN` / `COMMIT`，行锁跨多次往返；`SaveConditional` 是一条条件 UPDATE，只有影响 0 行时才多一次只读围栏列的状态查询。代价按 AGENTS.md §6 压测，与 R20 基线对比（§11 第 9 步）；没有对比表不宣称性能结论。这也是尽快发 `v0.3.0` 的理由。

---

## 5. go/db：与 Redis 守卫的关系（以 `dual` 为主；§5.5 与模式无关）

### 5.1 保留什么

排序锁、游标（`consumer:applied:*`）、已落库 epoch（`consumer:applied_epoch:*`）全部保留，键名和值格式不变。原因：

- 旧版 go/db 只认这三样，新旧实例并存时靠它们互斥和判新旧。
- class-2 / class-3 在同一 epoch 内没有可比的版本，先后只能靠锁和游标。
- 对 class-1，它们是便宜的去重：重复投递在游标处就被丢掉，不用把整行发到 MySQL。

**Redis epoch 守卫判 stale 时：`legacy` 不变（ack 丢弃）；`dual` 下先做计数器探测。**

现状：`guardOwnerEpoch` 的 stale 分支 ack 重试收据和 Kafka 消息，不进死信，排在选库与 SQL 之前 [C]。计数器倒退（`player:{id}:owner_epoch` 回到旧值）而已落库 epoch 没有倒退时，倒退之后铸出的 epoch 全部小于已落库 epoch，这名玩家之后的写都在这里被丢掉，到不了 SQL，死信里也没有载荷。丢弃不是无声的（`STALE-OWNER-WRITE` 日志、`db_stale_owner_write_rejected_total`、warning 级告警 `DbStaleOwnerWriteRejected`），但告警描述把它解释为「可能是 K1 被正确拦下」，值班大概率当成良性，而载荷确实丢了（§17 状态机 F3、事实 F1）。

`dual` 下的探测：

- 判 stale 时多读一次 `GET player:{id}:owner_epoch`（键名与解析用 `go/shared/ownerepoch` 的 `OwnerEpochKey` / `ParseEpoch`）。
- **计数器 ≥ 已落库 epoch**：判为真旧主，ack 丢弃（同今天）。
- **计数器 < 已落库 epoch（含键缺失）**：判为计数器倒退。经 `quarantineOrderingConflict(label=DeadLetterOwnerEpochRegressed)` 进死信，`db_owner_epoch_guard_total{outcome="regressed"}` 计数，打 `OWNER-EPOCH-REGRESSED` 日志。
- **探测读失败**：按 owner epoch guard unavailable 走 `deferFailedTask`，耗预算（`redis_unavailable`，L-04 §4.3 口径），预算用完以 `budget_exhausted` 进死信，载荷不丢。不进 `retryBudgetExempt`。
- `legacy` 不探测。
- **判据为什么正确**：已落库 epoch 只记录被铸造过、并且有写带着它落了库的值；计数器只升不降（铸造与回滚都用 INCR，`ownerepoch.go` 包注释）。正常运行时任何被铸造的 epoch 都 ≤ 计数器，所以无论两次读的先后顺序如何，都不可能出现「计数器 < 已落库 epoch」。出现就说明计数器倒退了。
- **残余**：计数器重新追过已落库 epoch 之后，探测失效，倒退期间发出的写与僵尸写无法区分。探测只是检测手段，处置与根治见 §9.1。
- **依赖**：探测要求 go/db 能读到 owner_epoch 所在的 Redis。今天 go/db 的 Redis 客户端与 scene_manager 是同一个核心实例（本地 `db.yaml` 与 `scene_manager_service.yaml` 都是 127.0.0.1:6379，K8s 的 db RedisClient 也是核心实例）[C]。G4a 拆分第 2 批计划把 `consumer:applied_epoch:*` 迁到 redis-mapping、owner_epoch 留在核心实例（`shared-redis-split-by-purpose.md`），拆分后 go/db 要单独配一个指向核心实例的客户端。**已定**（§14 第 19 条）：go/db 新增只读客户端 `OwnerEpochRedis`，只做 `GET owner_epoch`，`epoch_raise` 共用它；未配置时回落到现有 `RedisClient`（今天两者是同一实例）。两份设计都要登记这条依赖；同时登记 §6.2 令牌发布脚本在拆分后跨实例的问题（§6.4）。
  - **（2026-10-19 指向注，上条原文保留）** 「`consumer:applied_epoch:*` 迁到 redis-mapping」已被 G4a 2026-10-18 修订稿 §18 取代：控制面改为按 zone 组分实例 `redis-dbctl-g<N>`，不再并入 redis-mapping（这是该稿 §18.7 选 A 时的基线；选 B 时按玩家的控制面键随核心闭包进核心 Cluster、带 `{pid}`，见该稿 §18.7、§20.3，二选一待 §25.2 第 1 条；本括注 10-22 补）。该稿 §18.7 盘点了 go/db 三个句柄承载的键全集：`RedisClient` 今天还承载令牌发布脚本、`save_barrier` ACK 与补投工作器，必须就是核心实例；第 2 批要先把它拆成控制面与核心两个句柄（该稿 §25.2 第 2 条）。本条 `OwnerEpochRedis` 的依赖照旧成立，已登记在该稿 §18.7。
- 不采用「`dual` 下 Redis 守卫判 stale 一律进死信」：真僵尸与迟到的退出存盘（K1）本来就是这道守卫的设计对象，全部进死信会把整行载荷堆进 noeviction 的共享 Redis，告警一直是噪声，而且仍然分不出哪条是计数器倒退（§15）。

### 5.2 游标判过期时让 epoch 说话（G3）

现状：`handleTask` 里 `orderingForTask` 判 `orderingStale` 即 ack 丢弃，之后才轮到 `guardOwnerEpoch` [C]。新主的写在重试队列里、老主的迟到写先落库并推进游标时，新主的写回来会被判过期丢掉。

放开条件（同时满足）：`Mode=dual`、`Fencing.EpochOverride=true`、class-1、MsgType 是围栏表（§4.1）。不加「围栏表」时，无围栏表上游标已判过期的旧写会被放过，送到无条件 `Save`，覆盖更新的数据，比今天差。

| 游标 | 已落库 epoch 的判定 | 处理 |
|---|---|---|
| apply | — | 照旧 |
| stale | `match`（相等）或 `stale`（更小）| 照旧丢弃。理由：同一 epoch、同一分区内 offset 更小就是更早发出的写；override 落库后游标被改写到那笔写（见下），这个前提重新成立 |
| stale | `advance`（任务 epoch 更大）| **不丢**，继续走到 SQL，由围栏裁决（`epoch_override`） |
| stale | `first`（游标在、epoch 记录不在）| 照旧丢弃，计 `db_ordering_guard_total{outcome="stale_no_epoch"}`。游标存在而 epoch 记录不存在什么也证明不了：class-3 的写不推进 epoch，另外还可能是键丢失 |
| incomparable | — | 照旧进死信 |

override 落库之后：

- SQL 结局为 `applied` 或 `equal` 时，用 §5.5 的同一条脚本原子完成三件事：核对排序锁令牌（锁与游标在同一个 Redis）；把游标**改写**为本任务的 (partition, seq)，允许后退；把已落库 epoch 取 max 推进到任务 epoch。脚本失败或返回 0 就延后，不能只记日志。
- **为什么必须改写**：v0.2 写的是「成功落库后不推进游标」。那样游标停在老主迟到写的位置、已落库 epoch 已是新 epoch，同 epoch 的另一笔较新重试（offset 同样小于游标）会落到上表第 2 行被丢。重试是 FIFO（`pushPayloadDurably` 的 LPUSH 加认领脚本的 RPOPLPUSH），两笔同 epoch 的重试排在老主迟到写之后时，最终落库的可能是较旧那笔（§17 状态机 F1）。这比今天好（今天两笔都在游标处被丢），但 F-1 不成立。
- **为什么后退是安全的**：被让出来的 offset 区间里只有两类写。老 epoch 的写，由同一脚本已推进的 epoch 守卫挡住；同 epoch 的更新写，按 apply 进 SQL，由围栏裁决。
- 失租后重试得到 `equal` 时走同一脚本（§5.4）。
- **残余**：游标后退之后，epoch=0 的写（class-3）重投到 `legacy` 实例时会越过游标。切 `dual` 的前置要求 `legacy_zero` 不增长（§8.3），与 §8.5 的进入条件相同。

放行为什么安全：

- **安全性来自 §5.5 的不变量，不依赖围栏精度。** 不变量是「游标被某笔写推进 ⇒ 已落库 epoch ≥ 该写的 epoch」。在它之下，stale + advance 意味着推进过游标的写的 epoch 都小于任务 epoch，任务在 epoch 次序上更新。v0.2 写的「class-1 的围栏是精确的，放过去最多多花两条语句」删除：围栏可能落后（`legacy` 写不推进围栏），落后时放行就是覆盖。
- **旧二进制做不到原子推进**：它先 SET 游标、再单独 EVAL 推进 epoch，后者失败只记日志。它在场时会出现「游标已推进而 epoch 未推进」：一笔写由旧实例落库（不推进围栏），游标 SET 成功、epoch EVAL 失败（v0.2 的新二进制 `off` 也是这两步，v0.3 起新二进制一律走 §5.5）；之后任何实例（重试队列各实例共用）认领一笔更旧的同 epoch 重试，游标判 stale、epoch 判 advance，被落后的围栏放过，覆盖更新的数据。今天这笔会在游标处被丢掉（§17 状态机 F2、事实 F5）。所以 `EpochOverride` 有门禁（§8.3），旧二进制在场时不得开。
- **只对 class-1 放开**：在 §5.5 之下，class-2 的 advance 放行同样成立；本批只开 class-1，理由是控制范围（§15 同步）。class-2 第二步不移动 `fs`，但只有同一 epoch 内先后出现 class-1 与 class-2 两种生产者（同一 epoch 里 C++ 版本发生变化），再叠加一次 epoch 推进失败，才可能被放行路径利用；这跟随 §14 第 8 条的结论，本批不另做处理。
- `legacy` 不放开：没有围栏兜底。
- **计数扩展现有指标，不新建**：`db_ordering_guard_total` 已随 A3 入库（`metrics/pipeline.go`，outcome 为 `apply` / `apply_no_cursor` / `stale` / `incomparable` / `error`，在 `orderingForTask` 内部计数）[C]。v0.3 把它写成新指标，照做会重复注册同名 Vec（panic）。v0.4 只在 `orderingGuardOutcomes` 里追加 `stale_no_epoch`、`epoch_override` 两个取值（启动时预建）。计数规则：`orderingForTask` 照旧在判定处计 `stale`；`handleTask` 读完 epoch 记录之后，stale + first 额外计 `stale_no_epoch`，stale + advance 走 override 时额外计 `epoch_override`。这两个是 `stale` 的子集，看板与告警不得把它们和 `stale` 相加。同步改该指标的 Help 与 `pipeline.go` 文件头的取值个数（5 → 7）。
- ④（重试只交给分区属主）落地后，重试不再由任意实例认领；上面「旧二进制做不到原子推进」一段的 F2 论证在 ④ 之后只对旧二进制成立，结论（`EpochOverride` 要门禁）不变。
- 不采用「stale + match 也交给 SQL」（评审改法 b）：它依赖围栏精确（全 zone 都是 `dual`），并且会把重平衡时成批的 stale + match 重投都送进 MySQL；如果将来要做，只对 `fromRetry` 的任务开放（§15）。

### 5.3 批内合并跟着围栏走（只在 `dual`、只对围栏表）

`processTaskBatch` 对同一 `(key, msgType, originPartition)` 保留 offset 最大的一条，其余标记为被取代并 ack [C]。

改法（只在 `dual`、只对围栏表）：两条都是 class-1 且 epoch 相同时，保留 `save_seq` 更大的那条（相同则保留后到的）；其它情况照旧。

- 正常情况下两种比法结果相同：C++ 生产者开了 `enable.idempotence`（`kafka_producer.cpp:59-76`），同一分区内 offset 顺序就是发送顺序。
- 这条改动是为了让合并与围栏用同一把尺子。否则一旦两者不一致，被保留的那条会被围栏判 `older_seq`，被取代的那条已经 ack，两条都没落库（附录 B2 指出的叠加）。
- `legacy` 下不生效：`legacy` 没有围栏，若按 save_seq 保留，被取代那条的 offset 更大，一旦重投会被无条件写入，与「`legacy` 等同今天」矛盾。

### 5.4 失租

现状（HEAD，A3 的 G1 治标之后）：租约由 `ordering_lease.go` 管理。失租有四个入口：写 SQL 之前 `beginSQL` 发现已过本地截止时间（一条 SQL 都没发）、看门狗续租用尽或到截止时间、续租发现易主、`runDBOp` 里复核之后的 `verify` 失败；失租会取消在途写 SQL，计 `db_ordering_lease_lost_total{reason}` [C]。哨兵 `errOrderingLeaseLost` 由 L-04 第 1 批引入（`store_failure.go`），`deferReason=lease_lost`，`retryBudgetExempt` 里没有它 [C]。v0.3 写的「写完之后才核锁」已过时。

**已定（§14 第 14 条）：复用 L-04 的哨兵，不并入豁免。** §6.2 令牌发布脚本、§5.5 推进脚本返回 0 时调 `lease.setLost(owner_changed)` 并包这个哨兵，失租在 `db_ordering_lease_lost_total{reason="owner_changed"}` 里计数，释放阶段不再按 `not_owner` 误报「MySQL may hold an older snapshot」。

- 不豁免的理由：与 L-04 现有口径一致，L-04 只对存储类失败豁免，Redis 侧环境故障（游标读写、epoch 守卫读、缓存发布）都耗预算，并留到「落点 Redis 独立部署时」整族调整；失租本质上属于这一族，单独开例外等于造第二套规则。豁免还会无界空转，把整行载荷持续堆在 noeviction 的共享 Redis 上。续租已有一轮最多 10 次的有界退避，连续失租 3 次的门槛已经很高。
- **订正 v0.3 的理由**：v0.3 写「失租的写已经在 MySQL 里」，G1 治标落地后不再成立：`beginSQL` 失败时一条 SQL 都没发（`store_failure.go` 注释、`ordering_lease.go` 文件头）[C]。所以因失租耗尽预算进死信的条目不一定已落库，不能当作无害，必须能重放：L-04 第 2 批死信工具是「不豁免」的必要配套（§8.3 第 2 条）。
- 不豁免的代价：同一笔写连续 3 次失租会以 `budget_exhausted` 进死信。若它已落库，缓存可能停在旧值直到 TTL（≤3600 秒）、游标与已落库 epoch 没推进，放大 §6.5 所说的「缓存落后于 MySQL」（父键缺失时 login 会拿旧缓存拼数据）与 §6.3 的围栏落后；若它没落库，要靠死信工具重放。

### 5.5 游标与已落库 epoch 的原子推进（新二进制，与模式无关）

现状（HEAD 与工作区）：`handleTask` 成功分支先 `markAppliedSeq`（普通 SET 游标，不设过期，step `cursor_set`），再 `markAppliedOwnerEpoch`（单独 EVAL `luaMarkAppliedEpoch` 取 max，step `epoch_mark`）。后者失败只打 ERROR，不延后也不回滚；用的是 worker 的 ctx，而 `Stop` 第④步先 cancel 再等待，cancel 恰好落在 SET 返回之后、EVAL 之前就会留下「游标已推进、epoch 未推进」。这个窗口只有一次 Redis 往返，Redis 抖动同理，概率低但真实存在 [C]。go/db 会话确认这两次调用的先后到目前没有变。`luaMarkAppliedEpoch` 已改为拼 `luaDecimalGreaterFn` 做十进制串比较，注释约定条件写等后续脚本要比较 uint64 时直接拼这一段，不再把十进制串转成 Lua 数值比较（KOC `luaDecimalGreaterFn` 注释）[C]。

改法：新二进制无论 `legacy` 还是 `dual`，都用一条脚本取代这两次调用（step `applied_advance`）。顺序是**先核令牌 → 校验 → 写游标 → 写 epoch**：所有可能失败的检查都在第一次写之前做完，脚本中途报错时不会留下部分写。

```lua
-- 前缀：luaDecimalGreaterFn（KOC，提供 is_dec / dec_gt）
-- KEYS[1] 排序锁键   KEYS[2] 游标键（appliedSeqKey）   KEYS[3] 已落库 epoch 键（appliedEpochKey）
-- ARGV[1] 锁令牌     ARGV[2] 游标新值（encodeAppliedCursor，格式不变；空串 = 不写游标）
-- ARGV[3] 任务 epoch（规范十进制串；'0' = 不动 epoch）
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
local e = ARGV[3]
if not is_dec(e) then
  return redis.error_reply('task owner epoch is not a canonical decimal: ' .. tostring(e))
end
local cur = false
if e ~= '0' then
  cur = redis.call('GET', KEYS[3])
  if cur and not is_dec(cur) then
    return redis.error_reply('stored applied owner epoch is not a canonical decimal: ' .. cur)
  end
end
-- 以上是全部检查；以下才写键。
if ARGV[2] ~= '' then
  redis.call('SET', KEYS[2], ARGV[2])
end
if e ~= '0' and (not cur or dec_gt(e, cur)) then
  redis.call('SET', KEYS[3], e)
end
return 1
```

- **为什么不能先写游标后校验**（v0.3 的写法，fence-v03-review.md F4）：Redis 不回滚脚本里已执行的写。v0.3 在 SET 游标之后才校验 epoch 键，校验报错或 Lua 运行期错误会留下「游标已推进、epoch 未推进」，正是本节不变量禁止的状态；之后同 epoch 更旧的重试一旦走 §5.2 override 就会被放行。
- **为什么不转成 Lua 数值比较**（v0.3 的写法）：Redis Lua 的数字是 double，大于 2^53 的相邻整数舍入后相等，`e > cur` 判假，游标前进而 epoch 不动（`debug_import` 带来的大 epoch、`epoch_raise` 加了大余量都可能越过 2^53）；Lua 的数值转换还接受 `'0x10'`、`' 5 '`、`'1e3'`，与 `ReadAppliedEpoch`（`ParseUint`）和 `is_dec` 的口径不一致。改用 `luaDecimalGreaterFn` 后 uint64 全范围精确，非规范值一律报错。v0.3 写的「Lua 的 double 比较是精确的」删除。
- **游标写入**：正常路径（`orderingApply`）在持锁下判定、脚本里再核令牌，所以只进不退，不需要在 Lua 里比较游标（跨分区不可比）；override 路径（§5.2）写入本任务的 (partition, seq)，允许后退。沿用 `markAppliedSeq` 现有的跳过条件：`task.seq == 0` 或非 write 时不写游标（传空串），epoch 照常取 max。
- **不变量**：「游标被某笔写推进 ⇒ 已落库 epoch ≥ 该写的 epoch」。§5.2 的放行只依赖它。
- **只在成功分支调用**：只有结局为 `applied` / `equal` 才推进；`older_seq` 与三种死信结局都不调（§4.4、§4.5）。
- **ctx**：`context.WithoutCancel` 加单步上限（§4.6）。超时或取消不算失租，任务延后。
- **失败即延后**：脚本返回 0 → `lease.setLost(owner_changed)`，包 `errOrderingLeaseLost`；Redis 错误与 `error_reply` 归 `redis_unavailable`；都走 `deferFailedTask`、耗预算（§5.4）。删掉 `markAppliedOwnerEpoch` 注释里「失败只记日志」的口径。重试时 SQL 得到 `equal`，再走同一脚本。
- 键名、值格式、不设过期都不变，旧二进制照常读写。已落库 epoch 键出现非规范值时报错（→ 延后 → 死信），不当缺键处理，与 `ReadAppliedEpoch` 的口径一致。§9.1 `epoch_raise` 的只升不降 Lua 同样拼 `luaDecimalGreaterFn`。
- 推进脚本放在 `handleTask` 成功分支，缓存发布在 `runDBOp` 里（§6.2），两条脚本各自核令牌，本批不合并。
- **WAITAOF**：R2（④ + WAITAOF）落地后，核对推进脚本、LREM / Kafka ack 与 WAITAOF 的先后是否需要调整（例如推进脚本之后、ack 之前是否要等 AOF 确认）。等 go/db 会话的文件清单到达后定，写进本节（fence-v03-review.md F11）。
- **最小替代**（v0.4 不采用，保留作说明）：调换顺序，先推进 epoch、再 SET 游标，任一步失败都延后，同样保住不变量，但不核令牌、做不了 override 的游标改写。**不能**只把 `markAppliedOwnerEpoch` 失败改成延后：失败时游标已经 SET 到本任务的 seq，延后之后的重试会被游标判 stale 丢掉，epoch 永远补不上。
- 三把键（排序锁、游标、已落库 epoch）今天都在同一个 Redis 客户端上 [C]，单脚本可行。负责人已定 Redis 按 player_id 分片（§6.4），分片后三把键必须同一个 hash tag，并入 §14 第 3 条。

### 5.6 G5 的处置

G5（瓶颈文档 §4 的缺口表）：游标与已落库 epoch 记录缺键时放行。

- **class-1、围栏表、`dual`**：由围栏兜住。Redis 记录缺失只会让写多走到 SQL，由围栏裁决。§5.2 对「游标在、epoch 记录不在」照旧丢弃（`stale_no_epoch`），不扩大放行。
- **class-2 / class-3**：`dual` 下仍开着，与今天相同。
- **游标不做 Lua 取最大值**：class-1 的正确性不依赖游标；跨分区不可比；正常路径的单调由持锁判定加 §5.5 的令牌核对保证。
- 这条残余写进 §8.5 的 `sql` 进入条件。瓶颈文档把条件写登记为覆盖 G1 / G3 / G5，要同步订正为「G1；G3 部分；G5 只关 class-1」（§16）。
- 两张子表不原子见 §1.3。

---

## 6. 子表缓存

### 6.1 现状

- 键 `{MsgType}:{Key}`，TTL 3600 秒，普通 `SET`，没有版本也没有条件 [C]。
- 写入点只有 go/db 两处：读路径 `publishDBReadResult`、写路径 `writeBackDBCache`。读取方是 login（`ensure_player_all_data_async.go`；`sync_loader.go` 的 `TriggerPlayerDataPreload` 逻辑相同，但全仓无调用方，2026-10-30 已删除）和 `cmd/verifier`。删除方只有 `tools/merge_zone/player_rows.go` 的 `invalidatePlayerCaches` [C]。
- 写路径现状（HEAD，A3 之后）：`runDBOp` 里复核 → `lease.verify`（带令牌的 Lua 续期，step `lease_verify`）→ `writeBackDBCache` 普通 `SET`（step `cache_set`）[C]。v0.3 写的「缓存回写在核锁之前」已过时。核锁与 SET 仍是两次往返，verify 通过之后、SET 之前失租，旧值照样写进缓存。
- 在途实现在此之上改成：先单独 EVAL 发布资产证据，再对两张玩家表做 GET → 解析 → 比较围栏 → 按现值 CAS（最多三轮，`player_cache_publish.go`），同版本用 `proto.Equal` 比内容；非玩家表回落普通 `SET` [C]。v0.4 删掉这一套（§0.2 第 6、9、10 行），由下面的令牌发布脚本取代。
- 两个缺口：写路径的核锁与缓存写不是一个原子步骤（旧实例恢复后会把缓存写回旧值）；读路径不拿锁、无条件覆盖子表缓存（瓶颈文档 G4，后果是缓存被旧值覆盖）。本节解决这两个缺口，不解决「冷登录拼出旧父键」，后者见 §6.2 残余窗口 3。
- 父键缺失时 login 用子表缓存或读任务的结果拼 `PlayerAllData`，所以缓存变旧会被固化成回档；读任务的结果不经缓存（§6.5）。

### 6.2 新做法（与模式无关，`legacy` 下也生效）

**写路径**：`lease.verify` + 普通 `SET` 两次往返，换成一条**令牌发布脚本**（step `cache_publish`），原子地「核对排序锁令牌 → 续期锁 → 写缓存 →（按条件）发布资产 SQL 提交证据」。所有表的写路径缓存发布都走它（与模式无关）；证据部分只在 `dual`、class-1、结局 `applied` / `equal`、MsgType 为 `player_database` 时带上（§6.6）。

```lua
-- 前缀：luaDecimalGreaterFn（KOC，提供 dec_gt）
-- KEYS[1] 排序锁键  KEYS[2] 子表缓存键  KEYS[3] 资产 SQL 提交证据键（不发布证据时不传）
-- ARGV[1] 锁令牌  ARGV[2] 锁 TTL 毫秒  ARGV[3] 缓存值  ARGV[4] 缓存 TTL 秒
-- ARGV[5] 证据版本（'%020d:%020d'，41 字节，= 盖章值 (task.OwnerEpoch, task.SaveSeq)）
-- ARGV[6] 证据内容（PlayerAssetSQLCommit 的确定性序列化）
local function is_version(s)
  return type(s) == 'string' and #s == 41 and string.match(s, '^%d+:%d+$') ~= nil
end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0                                   -- 已不持锁：什么都不写
end
local writeEvidence = false
local result = 1
if #KEYS == 3 then
  if not is_version(ARGV[5]) then
    return redis.error_reply('asset sql commit version is malformed')
  end
  local old = redis.call('HGET', KEYS[3], 'version')
  if not old then
    writeEvidence = true
  elseif not is_version(old) then
    return redis.error_reply('stored asset sql commit version is malformed')
  elseif old == ARGV[5] then
    if redis.call('HGET', KEYS[3], 'data') ~= ARGV[6] then
      return -1                              -- 同版本不同内容：什么都不写
    end
  elseif dec_gt(ARGV[5], old) then           -- 等长定宽串，dec_gt 即逐字节比较
    writeEvidence = true
  else
    result = 2                               -- 已有证据版本更高：不回退证据
  end
end
-- 以上是全部检查；以下才写键。
redis.call('PEXPIRE', KEYS[1], ARGV[2])
redis.call('SET', KEYS[2], ARGV[3], 'EX', ARGV[4])
if writeEvidence then
  redis.call('HSET', KEYS[3], 'version', ARGV[5], 'data', ARGV[6])
end
return result
```

返回值与 go/db 的处理：

| 返回 | 含义 | 处理 |
|---|---|---|
| 1 | 缓存已写；需要发布的证据已写或已是同版本同内容 | `lease.extendValidUntil(调用开始时刻 + TTL)`（同 `verify` 成功）；计 `db_fence_cache_publish_total{path="write",result="written"}` |
| 2 | 缓存已写；已有证据版本高于本次，证据不回退 | 同上，另计 `result="evidence_ahead"`，打 ERROR `ASSET-SQL-COMMIT-AHEAD`（带 player_id、两边版本）；任务照常 ack。正常运行不会出现：已提交的更新版本必然先发布过证据，所以这通常是回档 / PITR 之后没清证据（§9.2），证据指向已被回滚的提交，必须告警 |
| -1 | 同一 `(E, s)` 的账本字节不同（撞号）| 什么都没写。计 `result="evidence_conflict"`，打 ERROR `ASSET-SQL-COMMIT-CONFLICT`；延后，`deferReason=fence_evidence_conflict`，耗预算（§4.4 错误表）。资产证据层保留「同版本不同内容即报错并计数」，对应 §4.4 `equal` 不比内容的决定 |
| 0 | 已不持锁 | 什么都没写。`lease.setLost(owner_changed)`，返回包 `errOrderingLeaseLost` 的错误，延后（§5.4，耗预算）。重试时拿到新锁，SQL 得到 `equal`，再补发缓存与证据 |
| Redis 错误 | — | 与 `verify` 同口径：ctx 取消或单步超时不算失租；其余往返错误按 `verify_error` 记失租；脚本的 `error_reply`（版本串畸形）不判失租，按 `redis_unavailable` 延后。都耗预算 |

- **为什么可以取代 `verify`**：`verify` 的职责是在发布缓存之前核锁并续期（`ordering_lease.go` 文件头第 2 条）。脚本在同一个原子步骤里核令牌、续期、写缓存，比「verify 之后再 SET」严格更强，中间不再有失租窗口；只有 `older_seq` 路径不写缓存，仍单独调 `verify`（§4.5）。
- **往返数**：`applied` / `equal` 从 `lease_verify` + `cache_set` 两次（在途实现另加证据一次 EVAL）变成一次（§4.5）。脚本用 `redis.NewScript(...).Run`（EVALSHA，NOSCRIPT 时自动回落 EVAL）。
- **令牌不外泄**：令牌在 `locker.TryLockResult` 里是私有字段（`go/db/internal/locker/locker.go`），给它加一个执行「带令牌脚本」的方法，不把令牌暴露出去；`orderingLease` 包一层，负责计时、`extendValidUntil` 与 `setLost`。§5.5 的推进脚本用同一个方式拿令牌。
- **证据版本的比较**：沿用在途的定宽版本串 `%020d:%020d`（41 字节，能容纳 uint64 全范围，字节序与 `(epoch, seq)` 字典序一致）。脚本先校验格式，再用 `luaDecimalGreaterFn` 的 `dec_gt` 比较：两串等长时它就是逐字节比较，不受 strcoll / locale 影响。在途 `PublishSQLCommit` 用 Lua 字符串 `>`（依赖 strcoll），且返回 0 时静默，v0.4 都改掉（fence-inflight.md F7 (4)）。
- **证据版本取盖章值**：`(task.OwnerEpoch, task.SaveSeq)`，与写进行内的围栏相同。在途实现取的是 `msg` 里的围栏，而它在被拒时会被换成库内行（§0.2 第 4 行）。
- **ctx**：`context.WithoutCancel` 加单步上限（§4.6）。
- 为什么这是真栅栏而不是又一层租约：校验与写入在 Redis 里是同一个原子步骤，而缓存本身就在这台 Redis 上。MySQL 做不到这一点（它看不到 Redis 的令牌），所以 MySQL 侧才需要围栏列。
- 被拒的写本来就不发布缓存与证据（§4.4），所以能让缓存变旧的只剩「SQL 已落、缓存迟发」这一种，令牌校验正好挡住它。在途实现「被拒时改为发布 SQL 当前行」想解决的「迟到 offset 越过此前失败的缓存补发」，在 v0.4 下不存在：被拒的写不推进游标，此前发布失败的那笔写会以 `equal` 重试补发（§15）。

**读路径**：`SET` 换成 `SET NX`。

- login 在子表缓存缺失、`GET` 出错或值解析失败时发读任务（`ensure_player_all_data_async.go:79-89`；`sync_loader.go` 的 `TriggerPlayerDataPreload` 逻辑相同，但全仓无调用方，2026-10-30 已删除）[C]。常态下 NX 成功。v0.2 写的「只在缺失时才发」不准确：`GET` 超时时键可能是好的，值解析失败时键里是坏值，这两种情况 NX 都不写。
- **NX 只决定缓存键。** 读任务回给 login 的 `TaskResult.Data` 始终是本次 SELECT 的行（`publishDBReadResult` 用同一份数据做缓存写和结果发布）。父键缺失时 login 用它或子表缓存拼父键（`ensure_player_all_data_async.go:79-89`、:145、:178-186）[C]。
- 读与写并发时，读到的旧行写不进已有的新缓存；即使读先写进去，写路径随后的发布会覆盖它。

**残余窗口**：

1. 读到旧行之后、NX 之前，缓存键恰好过期或被合服工具删掉。这个窗口换成带版本的写法也存在（键不在时无从比较），可以接受。
2. **缓存里是解析不了的值时 NX 不覆盖。** 本次登录仍拿到 MySQL 的行；坏值留到 TTL（≤3600 秒）或下一笔写的令牌脚本覆盖为止。login 随即写回父键，后续登录走快路径，所以代价只是父键再次缺失时多一次读和一行 ERROR。坏值来源很少：值只由 go/db 对同一描述符做 `proto.Marshal` 写入，现实来源是人工 SET，或 §6.4 的值封装在 login 升级之前就被写出（所以 §6.4 规定了上线顺序）。自愈方案记在 §15。
3. **冷登录与在途写的竞态**（今天同样存在，NX 与条件写都挡不住）：
   - 现象：父键缺失、而该玩家仍有未落库的写（在 Kafka 未消费，或在 ready 里延后）时，login 拼出的父键比在途写旧。子表缓存里还是旧版本时 login 连读任务都不发，直接用缓存拼父键；R 先于 W 完成时 NX 也不参与。C++ 加载后用更大的围栏覆盖，在途写的内容丢失（在途写后到时会被游标或 epoch 守卫丢掉）。
   - 入口：父键在 scene 首次存盘后不带 TTL，主 Redis 是 noeviction + AOF；Redis 整体丢失时 ready 一起丢；合服有 P3 / P4 排空门禁。现实入口只有手工删父键，或 Redis 丢失时写还在 Kafka。
   - 窗口：L-04 之后写的存储类失败不耗预算，存储不可用期间写一直留在 ready；冷却期内读立即失败，恢复后读从 Kafka 立即执行，而 ready 每实例每拍至多认领 min(LLEN, 200) 条。窗口约为「停机时长 + 排空时长」（L-04 之前约 3 拍就进死信，那本身就是丢）。
   - 修法方向不是读写分区对齐（①）：W 在 ready 里时不受分区顺序约束；④ 同样不会让读去等同 key 的待重试写。方向是 go/db 在写延后进 ready 时按 key 维护「有待落写」标记、ack 时清除，读任务看到标记就快速失败让 login 重试。**已定**（§14 第 15 条）：单独立项，归 go/db 存盘管线线，排在 ④ 之后；进了死信的写不清标记（fail-closed），由死信工具处置完再清；放在 go/db 读任务侧，不让 login 知道 go/db 的内部键。过渡期 runbook 写明禁止手工删 `PlayerAllData` 父键。

这两处改动与 `Fencing.Mode` 无关，`legacy` 下也生效（只会更严）。瓶颈文档 G1 治标第 ③ 条「缓存回写挪到核锁之后」已随 A3 落地（`verify` 在缓存 SET 之前，§6.1）；令牌发布脚本是它的原子版本，并取代它。

计数：`db_fence_cache_publish_total{path, result}`，`path=write` 时 `result` ∈ written / not_owner / evidence_ahead / evidence_conflict，`path=read` 时 ∈ written / exists（§10）。计数加在 `writeBackDBCache` 与 `publishDBReadResult`，不涉及 L-04 已改的 handler 签名。

### 6.3 值里自带围栏（下限，不是精确值）

go/db 盖过围栏的消息原样进缓存，所以缓存值（序列化的 `player_database`）里带着 18 / 19 号字段。login 不需要改：它把整条子表消息原样拼进 `PlayerAllData`（`ensure_player_all_data_async.go:178-186`）。C++ 冷加载时据此播种（§7.2）。

**缓存值里的围栏是行内围栏的下限，不保证相等**：

- `legacy` 写出 `(0, 0)`（`PreserveFields` 只保留行内值，不修改消息；`SaveConditional` 不改 message）[C]；
- class-2 第二步写出 `(E, 0)`，而行内 `fs` 被保留（回填已定不做，§4.3）；
- SQL 已落而缓存未发布期间（令牌脚本返回 0、`writeBackDBCache` 失败），整条缓存落后，围栏也落后。

只有读路径的发布，以及 class-1 `applied` / `equal` 的发布，与行内相等。**任何地方不得把缓存里的围栏当精确值用。** 方向安全：只会让播种下限偏低，不会误抬。

### 6.4 留给 `sql` 模式的问题（本批不做）

`sql` 模式下 class-1 不再取排序锁，令牌校验用不了，缓存必须自带可比较的版本。方向与交接要求一致：

- **不另开 `{MsgType}:{Key}:fence` 键。** 两个键必须同生同灭，任何只删或只写其中一个的工具都会造成缓存回退；Redis 分片后还要求两个键同槽。
- **首选值封装**：在缓存值前面加定长头 `0x00 | 版本 | epoch(8 字节大端) | seq(8 字节大端)`。合法的 protobuf 编码不会以 `0x00` 开头，新旧格式可以区分。一条脚本用 `GETRANGE` 取头比较后再写，单键、原子。代价是 login 要先能读两种格式，再让 go/db 写新格式，这是一次跨服务的格式升级。
- **上线顺序**：login 能读新格式之前，go/db 不得写新格式。否则每次冷登录都会因解析失败回落读库，而读路径的 NX 修不了坏值（§6.2 残余窗口 2）。
- **与 Redis 分片方案的关系**（负责人已定：以按 player_id 分片为准，交接 §5 第 16 项）：
  - 值封装是单键，不需要 hash tag。
  - §6.2 的脚本同时访问排序锁键、子表缓存键与资产证据键（证据键已用 `{player_id}` 作 tag）；§5.5 的推进脚本同时访问排序锁键、游标键和已落库 epoch 键。今天主 Redis 是单实例，没有问题。按 `{player_id}` 分片后，这些键必须落在同一个 tag 下；排序锁键的改名还要与 `tools/merge_zone/relocate.go` 里的锁键镜像同批改。瓶颈文档 §6.5 的分片修订要把这两条约束算进去。
- **与 G4a 按用途拆分的冲突**（v0.4 新登记，§14 第 19 条）：G4a 拆分第 2 批让 go/db 的 `RedisClient`（排序锁在这里）指向 redis-mapping，新句柄 `CacheRedis`（子表缓存在这里）留在核心实例（`shared-redis-split-by-purpose.md` §3.2.3）。令牌发布脚本要在同一段 Lua 里读排序锁、写子表缓存与证据，键分到两个实例后无法原子执行。解法二选一：第 2 批之前把缓存栅栏换成本节的值封装（单键，不依赖锁），或让排序锁、子表缓存、证据键留在同一实例。G4a 第 2 批要等这一条解决后才能切；§5.5 的推进脚本三把键都去 redis-mapping，不受影响。G4a 稿要登记这条依赖。
  - **（2026-10-19 指向注，上条原文保留）** G4a 稿已登记这条依赖，见 `shared-redis-split-by-purpose.md` §18.7。按 2026-10-18 修订稿：排序锁所在的控制面改去 zone 组实例 `redis-dbctl-g<N>`（§18），不再是 redis-mapping；子表缓存去 redis-cache（§19，必做；选 B 时随核心走、不单独成 redis-cache，是否还写缓存另待拍板，10-22 补）；令牌发布脚本自 2026-10-17 起还写 `save_barrier` ACK。第 2、3 批的硬前置写成二选一（A 值封装；B 排序锁、子表缓存、证据、`save_barrier` 同实例，Cluster 下同 slot），列为该稿 §25.2 第 1 条待拍板；选 A 时证据与屏障 ACK 是否仍要核令牌，由本线裁决。上句「§5.5 的推进脚本三把键都去 redis-mapping」同此改为：选 A（§18 基线）时三把键随控制面去组实例；选 B 时随核心闭包进核心 Cluster、带 `{pid}`（该稿 §18.7、§20.3）；两种都要同 tag（该稿 §21.6）。（本句 10-22 订正为条件式。）

### 6.5 缓存与读结果各自保证什么

| 对象 | 保证 | 不保证 |
|---|---|---|
| 子表缓存（写路径令牌脚本发布）| 只前进不后退：只有持锁者能写，被拒的写不发布 | 不保证等于 MySQL：SQL 已落而缓存未发布（失租、发布失败）期间整条落后，靠重试补发收敛，失租耗尽预算时停到 TTL（§5.4）；跨进程撞号时 `equal` 补发的 body 可能与 MySQL 不同（§4.4） |
| 子表缓存（读路径 NX）| 不覆盖已有值 | 不修坏值（§6.2 残余窗口 2）|
| 缓存值里的围栏 | 行内围栏的下限 | 不精确（§6.3）|
| 读任务结果 `TaskResult.Data` | 本次 SELECT 的行 | 不经缓存、不受 NX 影响；可能比在途写旧（§6.2 残余窗口 3）|

F-4（§1.2）按这张表理解：缓存单调，但可能落后。不采用「NX 失败后改回缓存现值」：SQL 已落而缓存未发布的窗口里，SELECT 读到 v3、缓存是 v2，NX 失败后回 v2 等于新增一条回档路径，而今天回的是 v3（§15）。

### 6.6 资产 SQL 提交证据（v0.4 新增，在途实现带来的消费方）

**是什么**：Redis 键 `player:{id}:asset_sql_commit`（hash，字段 `version` = `%020d:%020d`、`data` = `PlayerAssetSQLCommit` 的确定性序列化，`proto/common/asset/asset_op.proto`）。资产通道（帮会兑换、交易拍卖、寄售、商城到账等的 outbox）据此确认「这笔资产结局已经随玩家行提交进 MySQL」，避免 Redis 丢失后一物两得。它只是 SQL 已提交的缓存证据，不是第二份资产权威。

| 角色 | 谁 | v0.4 |
|---|---|---|
| 写入方 | go/db，**只有一处**：§6.2 令牌发布脚本 | 在途 `assetop.PublishSQLCommit` 删除（§0.2 第 12 行）|
| 读取方 | data_service `GetPlayerAssetOpLedger`，`require_sql_commit = true` 时只读证据，缺证据回 `found = false`、`sql_committed` 只在证据分支为 true（`asset_sql_commit.go`）| 保留；补注释写明前提「证据键与 go/db 的 Redis 是同一实例」 |
| 消费方 | `go/shared/assetop`：`verifySQLCommit`（在线终结前核对证据）、自动放弃判定、离线读账本 | 按下面的开关与放弃条件改（§0.2 第 15 行，归「全域到账续批」写入方）|

**发布条件**：`Fencing.Mode = dual`、class-1、结局 `applied` 或 `equal`、MsgType 为 `player_database`（资产、背包、资产账本与托管收据同在这张表）。证据版本取盖章值 `(task.OwnerEpoch, task.SaveSeq)`。`legacy`、class-2 / class-3、`player_database_1` 都不发布。

**各结局下的语义**：

- `applied` / `equal`：发布；同版本同内容幂等；同版本不同账本字节 → 拒绝、计数、延后（§6.2 返回 -1）；已有证据更高 → 不回退、计数告警（§6.2 返回 2）。
- `older_seq`：不发布。被丢弃的那笔写里的资产结局，要么已被行内更新的版本包含（同一玩家后发出的存盘是全量快照，账本在它的有界窗口内随之累积），要么还在 C++ 侧，会随下一次存盘再次带上；outbox 由之后任一笔 `applied` 写的证据收敛。被丢的若恰是最后一笔（§7.4 残余），outbox 停在等待，直到玩家下次存盘，不会误终结（fence-v03-review.md F9 (a)）。
- `stale_owner` / `owner_epoch_regressed` / `legacy_after_seq`：不发布，载荷在死信里。

**按 zone 的开关**（`verifySQLCommit` 是否要求证据）：

- 开关在资产调用方（guild / trade 经 `go/shared/assetop` 的 Loop 配置），按 zone 配置，缺省全关；配置键名落码时定。
- **关**：行为同 HEAD，按 Redis 父键 blob 判定（`require_sql_commit = false`）。在途 `ledger_dataservice.go` 恒带 `RequireSqlCommit: true`，改为看开关。
- **开**：在线终结与离线读账本都要求 SQL 证据。打开条件：该 zone 已切 `dual`、已上新 C++，且观察到证据在发布（`db_fence_cache_publish_total{path="write",result="written"}` 在该 zone 持续增长）。已离线、新链路上线前没有证据的玩家，待办要等他再次存盘才能终结，打开前要接受这一点。
- `Loop.Ledger` 在开关打开的 zone 存在时为必填：启动时断言，缺 `DataServiceRpc` 即拒启，不再「为 nil 时所有在线终结永远停在 AwaitDurable」（fence-inflight.md F6 (b)）。`go/guild/internal/svc/asset_op.go`、`go/trade/internal/svc/assetchannel.go` 里「离线读账本是优化」的注释同步改。

**自动放弃**（180 天后）：放弃的前提是「这个 seq 不可能已经到账」。v0.4 要求满足其一：

1. Redis 父键 blob 里的账本与 SQL 证据**都**判「未见」；
2. SQL 证据判「未见」，且证据的 `save_seq` ≥ 父键 `PlayerAllData.last_save_seq`：证据不落后于父键，父键里的账本已被证据覆盖。

只看 SQL 证据会出事：玩家在旧版 scene 上、或计数器倒退期间完成一笔兑换，scene 回 APPLIED 并写进父键，对应的 DBTask 没有落库（或进了死信），证据停在之前的版本；180 天后只读证据判「未见」就放弃并退还帮会侧，玩家再上线从父键加载，资产仍在，一物两得（fence-inflight.md F6 (a)）。HEAD 读父键 blob，会判已到账，不放弃。

**键的生命周期与运维**：

- 不设 TTL，与 `PlayerAllData` 父键同寿命（父键在 scene 首次存盘后也不带 TTL）。设了 TTL，过期后已离线玩家的待办会永远等不到证据。代价是每名玩家常驻一份账本（含托管收据里的完整实例字节），在 noeviction 的共享 Redis 上；容量计入 `shared-redis-split-by-purpose.md` 的容量表。
- 登记键族：`shared-redis-split-by-purpose.md` §2.2 加一行（写方 go/db `RedisClient`，读方 data_service `Router.ClientForPlayer`，hash tag `{player_id}`）。
- 合服：`tools/merge_zone/player_rows.go` 的 `invalidatePlayerCaches` 把证据键加进删除清单。删除后证据缺失，待办等下一次存盘重新发布，方向安全；不删则合服改了 home_zone 后，读侧可能换到另一个集群而读不到，或读到旧证据。
- 回档 / PITR：证据会指向已被回滚的提交，回档流程删除该 zone 受影响玩家的证据键（§9.2）。
- 跨实例依赖：写在 go/db 的 `RedisClient`，读走 data_service 的 `Router.ClientForPlayer`（home_zone 集群或 DevRedis）。本地与 K8s 今天指向同一个共享实例（`k8s_deploy.ps1` 的 Redis 注释）[C]；启用 Regions 集群或 G4a 拆分后两边可能分家 [未核实]，G4a 稿登记（§14 第 19 条）。

上线与回退顺序见 §8.7。

**落码记录（2026-10-16，资产通道消费方与证据发布；未编译、未测试，待 Codex 验证）**：

- 开关名：`assetop.LoopConfig.RequireSQLCommit`；guild / trade 的配置键 `AssetOp.RequireSQLCommit`（`etc/guild.yaml`、`etc/trade.yaml`，缺省 `false`）。guild、trade 都是全服服务、一份配置覆盖所有 zone，`Op` 里也没有玩家的 zone，所以「按 zone」落为「该服务会投递到的 zone **全部**满足打开条件后才打开」；真要逐 zone 生效，需要 `Op` 带 home_zone 或由 data_service 回报该 zone 的模式，本批不做。
- 读取：`LedgerReader.ReadPersistedLedger` 回到 HEAD 口径（Redis 父键 blob，`require_sql_commit=false`）；新增可选接口 `SQLCommitLedgerReader.ReadSQLCommittedLedger`（`require_sql_commit=true`，旧 data_service 回 `sql_committed=false` 时报错）。`DataServiceLedger` 两个都实现。开关打开时在线终结（`verifySQLCommit`）、离线读账本只走后者；关闭时都不碰证据，托管扣出另用同一判据核对回包里的原件身份。
- 启动断言：`Loop.CheckWiring()`，开关打开而 `Ledger` 不能读 SQL 证据即返回错误；guild 在 `guild.go` 接上 Ledger 后调用（`logx.Must`），trade 在 `reconcile.New` 里调用，mail 在 `mail.go` 里调用。漏调时 `verifySQLCommit` 每笔计 `assetop_ledger_read_total{result="unwired"}` 并打 ERROR，不再静默。
- 自动放弃：开关打开时先读 SQL 证据——已见按证据终结；确定未见再读父键 blob，blob 也确定未见才放弃；blob 已见即证据落后，跳过并计 `assetop_abandon_skipped_total{reason="sql_evidence_behind"}`、打 ERROR；任一侧读失败或给不出结论都不放弃。**上面第 2 条放行条件没有实现**：`GetPlayerAssetOpLedgerResponse` 不带证据版本与父键 `last_save_seq`，要改 proto 才拿得到；只用第 1 条更保守，代价是父键缺失的玩家不会被自动放弃，留给人工。
- 两个固定要求证据的消费方：mail 的发奖终结强制 `SQLCommitted`，`mail.go` 固定打开开关；trade 的交易合同（ORDER）终结强制 `SQLCommitted`，`config.Validate` 在配了 Commerce 白名单或 `Mall.URL` 而开关关闭时拒启。`assetop.AcknowledgeReceipt` 只读 SQL 证据，与开关无关。
- 证据发布：go/db 仍调用 `assetop.PublishSQLCommit`（并入令牌发布脚本归 go/db 线，本批不碰 go/db）。脚本改为：两边版本先校验定宽格式，再逐字节比较（不用 Lua 字符串 `>`）；同版本不同内容报错；已有证据更新时不回退、计 `assetop_sql_commit_publish_total{result="evidence_ahead"}` 并打 ERROR `ASSET-SQL-COMMIT-AHEAD`（旧版返回 0 时静默）；改用 `Script.Run`。该指标注册在默认 registry，由 go/db 的 `/metrics` 暴露，令牌脚本落地后由 `db_fence_cache_publish_total` 取代。新增 `assetop.SQLCommitVersion(epoch, seq)`。
- 运维登记：`shared-redis-split-by-purpose.md` §2.2 表 E 加键族与拆分约束；`tools/merge_zone/player_rows.go` 的 `playerCacheKeys` 加入证据键（合服与撤销都会删）；`docs/ops/mysql-backup-pitr-runbook.md` §4.1 加第 7c 步。

---

## 7. C++：save_seq 发号

### 7.1 约束

- 同一玩家、同一 epoch 内，后发出的存盘必须拿到更大的 save_seq。
- **实体会在同一 epoch 内销毁重建**：同落点重连不铸造 epoch（`go/scene_manager/internal/logic/owner_epoch.go:41-46`），而实体可能已被清掉后重新加载（PL:3623-3628）。次数没有上界 [C]。
- **Redis 父键不是「已发出 DBTask」的上界**：DBTask 在 `Save()` 入队之后立刻发出，不等 Redis 应答（PL:3683-3741）；Redis 那次写可能还在排队、被后一次存盘顶替、或被 CAS 拒掉 [C]。
- **scene 进程重启后 epoch 不一定推进**：新进程可能复用同一个 node_id，而 location 仍指向它时走「不铸造」分支（`enterscenelogic.go:514-517`、:643）[C]。这条时序在真实部署下是否可达没有核实。node_id 取最小空闲号、不绑主机，新进程可以落在另一台主机；但崩溃后旧的 node_id 占位键要等 etcd 租约过期（`NodeTTLSeconds`，权威值 180 秒，`bin/etc/base_deploy_config.yaml`）才能被复用，所以「同一个 node_id 的新进程」与旧进程之间至少隔着分钟级的停机 [C]。
- **dev 旁路**：`AllowUnsafeCrossNodeHandoff=true` 时跨节点交接无标记放行、不铸造，`ReleasePlayer` 异步派发，新旧两个进程同持一个 epoch 并各自发号，序号先后只靠两台主机的墙钟（`enterscenelogic.go` 的旁路分支）[C]。本地 `scene_manager_service.yaml` 开着它，代码默认 `false`。单机 dev 下两个进程读同一个墙钟，序号先后就是发号先后；只有多主机误开旁路才依赖主机间偏差。只允许单机 dev。
- 今天这条路径按 broker 分配的 offset 定序，与时钟无关。发号不能把「时钟回拨」变成新的静默丢盘（附录 B2）。

### 7.2 发号器

**进程级发号器，不设每玩家计数组件。** 在途实现的 `PlayerOwnerEpochComp.lastSaveSeq`（每实体 `++`，没有墙钟下限、没有上界）正是 §0.1 第 1 行与 §15 否决的形状：父键不是「已发出 DBTask」的上界，scene 崩溃后同一 node_id 的新进程走不铸造分支时，序号会被复用或倒退（fence-inflight.md F3）。v0.4 删除该字段，`PlayerOwnerEpochComp` 回到只管归属 epoch（§0.2 第 16 行）。

```
Next(nowMs)           = 若 last == UINT64_MAX：显式失败（拒绝本次存盘并打 ERROR，不回绕）
                        否则 last = max(last + 1, nowMs << 10)
Observe(floor, nowMs) = 若 floor > (nowMs + 容差) << 10：返回「超界」，不抬高 last，打 ERROR 并计数
                        否则 last = max(last, floor)，返回「已采纳」
容差 = 60 秒（60000 ms）
```

- `last` 是 scene 线程上的一个 `uint64`，整个进程共用。它对这个进程里的所有存盘严格递增，所以同一玩家的任意两次存盘（不管中间实体重建过几次）都有序。
- 发出的值在玩家之间共用一条数轴，每个玩家看到的是有空洞的递增序列。围栏只要求递增，不要求连续。
- **类型与放置**：发号器是普通值类型 `SaveSeqAllocator`（`Next` / `Observe` 两个方法加一个读当前值的 `Current`）。它与纯函数 `NextSaveSeq(last, nowMs)`、`SaveSeqFloorFrom(const PlayerAllData&)`（返回三路的 max，见下）一起放在 `cpp/libs/services/scene/player/system/save_seq.h`，时间显式传入。生产实例作为 `ThreadLocalIdGeneratorManager`（`cpp/libs/services/scene/core/system/id_generator.h`，`thread_local tlsIdGeneratorManager`，与 buff / skill 发号器同处）的一个成员；它是独立的 thread_local，不在 `tlsEcs`（`EcsContext`）里，`node.cpp` 的 `tlsEcs.Clear()` 不触及它 [C，fence-v03-review.md verified_ok 第 11 条]。单测各自构造局部实例，不碰全局实例，所以不需要复位入口。不用 static。
- `nowMs << 10` 约为 2^51，离 uint64 上限很远；有了 `Observe` 的上界，`last` 正常情况下到不了 `UINT64_MAX`，饱和检查只防坏数据。
- **为什么要上界**：`last` 全进程共用，`Observe` 无条件接受时有两种坏法。其一，回绕：一行来自 `debug_import` 或手工改库的 `fence_save_seq = 2^64-3` 被冷加载进来，之后两笔发号得到 2^64-2、2^64-1，第三笔 `last + 1` 回绕成 0 退回墙钟；前两名互不相干的玩家行内围栏停在近 2^64，同 epoch 内之后的存盘全被判 `older_seq`。其二，一台时钟错到几十年后的主机发出的序号写进父键和 MySQL 行，任何加载到这些玩家的进程都被抬到同一量级、长期走 `last + 1`，墙钟下限失效，新旧进程从同一个下限各自 `+1` 还会撞成 `equal`（§4.4）。污染单独存在时不丢数据，丢盘仍要 §7.4 的条件或 dev 旁路。data_stress 写的合成 player_id（从 1,000,000 起）不经 login 与 scene 加载，进不了 `Observe`。
- **容差：60 秒**（§14 第 13 条，已定），写进 scene 配置作缺省值，配置键名落码时定。容差只决定要不要抬高整个进程的水位；下面的个人下限让超界不丢数据，所以容差越小越安全：容差越大，一台时钟超前的主机能把进程水位抬得越高，越接近 §7.4 条件 4 的风险。60 秒远大于正常 NTP 偏差（毫秒级），是「分钟级」的下限。同时加 scene 主机间时钟偏差监控，告警阈值在 1 秒级（§7.4）。
- **超界玩家的处置：个人下限**（§14 第 13 条，已定，v0.3 方案 2）。`Observe` 返回「超界」时，只把这个值挂在该玩家实体上（`PlayerSaveSeqFloorComp { uint64_t floor; }`，只为超界玩家创建；读路径一律 `try_get`）。该玩家发号取 `max(全局 Next, 个人下限 + 1)`，发出后把个人下限更新为发出的值；个人下限为 `UINT64_MAX` 时显式失败，同 `Next` 饱和。不污染其他玩家，也不丢盘。实体重建时加载会重新得到它（父键里的 `last_save_seq` 仍超界），不受 §15「每玩家组件归零」的问题影响。不采用「不采纳、让该玩家本 epoch 内全判 `older_seq`」：那是把坏数据变成这名玩家确定的丢盘，违背 F-3。
  - 纯函数 `ApplyPersonalFloor(globalSeq, personalFloor)` 放在 `save_seq.h`，返回 `max(globalSeq, personalFloor + 1)` 或饱和失败，单测覆盖（§7.5）。

**播种**：`InitPlayerFromAllData` 加载玩家时调用一次

```
floor = SaveSeqFloorFrom(all)
if (Observe(floor, nowMs) == 超界) emplace<PlayerSaveSeqFloorComp>(player, floor)
// SaveSeqFloorFrom = max(all.last_save_seq(),
//                        all.player_database_data().fence_save_seq(),
//                        all.player_database_1_data().fence_save_seq())
```

三路 max 的算式与在途实现相同（在途写在 `InitPlayerFromAllData` 里给 `lastSaveSeq` 赋值），v0.4 把它移进纯函数，经 `Observe` 进发号器。`InitPlayerFromAllData` 里只剩这几行调用。它会连带触发初始化事件、`EnterScene` 与 `SnapshotSystem::CaptureAndSend`，`cross_zone_test` 宿主里没有先例能跑通，所以这几行没有单测覆盖，由代码评审与 §11 第 7 步覆盖；单测只测纯函数 `SaveSeqFloorFrom`、`ApplyPersonalFloor` 与局部 `SaveSeqAllocator`（§7.5）。

两路来源互补：

| 加载路径 | `last_save_seq` | 子表里的 `fence_save_seq` |
|---|---|---|
| 父键存在（常态：C++ 存过一次之后父键没有 TTL）| 上一次落到 Redis 的存盘用的序号 | 0（C++ 存盘的 body 不带围栏）|
| 父键缺失，login 从子表缓存或 MySQL 拼出来（冷路径）| 0 | ≤ 已落库的序号。子表缓存命中时取缓存（§6.3 的下限）；缓存缺失、login 读 MySQL 时才等于行值 |

- 不看 epoch 是否相同：少一个分支。代价是进程级下限会被任意一名玩家带来的值抬高，并经父键和行内围栏在进程之间传播；上界就是为此加的。v0.2 写的「没有坏处」删除。
- 现有的 `PlayerDatabaseMessageFieldsUnmarshal` 只读显式列出的字段（`player_database_loader.cpp:84-138`），读这两个围栏值要新增代码。C++ 只读不写。

**发号**：在 `SavePlayerToRedisImpl` 里，脏比较快路径之后（PL:3633-3642）、`Save()` 之前（PL:3683）：

```
saveSeq = tlsIdGeneratorManager.saveSeq.Next(TimeSystem::NowMillisecondsUTC())   // 饱和 → kRejected + ERROR
if (auto* pf = try_get<PlayerSaveSeqFloorComp>(player))
    saveSeq = ApplyPersonalFloor(saveSeq, pf->floor), pf->floor = saveSeq         // 饱和 → kRejected + ERROR
message->set_last_save_seq(saveSeq)        // Save() 会立即序列化，必须在它之前
…
dbTask.set_save_seq(ownerEpoch == 0 ? 0 : saveSeq)   // 两张子表共用同一个值；epoch 为 0 时不带序号（同在途实现）
```

- 被快路径跳过的存盘不消耗序号；发号在快路径判定之后，所以快路径比较时新消息里还没有 `last_save_seq`（§7.3）。
- **C++ 只写 `PlayerAllData.last_save_seq`**，两张子表 body 里的 `fence_owner_epoch` / `fence_save_seq` 保持 0，由 go/db 盖章（§3.2、§4.2）。在途实现在这里和 `MarshalPlayerForSave` 里把 `(epoch, seq)` 写进了子表 body，删除（§0.2 第 19 行）。
- `ownerEpoch == 0` 时仍发号并写父键的 `last_save_seq`（保住播种来源），只是 DBTask 的 `save_seq` 填 0：go/db 对「有 seq、无 epoch」无法判定，class-3 只看 epoch（§2）。
- 两条 DBTask 各自独立发送，可能一成一败（PL:3731-3741）；两张表各有自己的围栏，互不影响。

### 7.3 不让序号污染相等比较

`last_save_seq` 每次写盘都变。快路径比较的是「这次新构造的消息」对「上次落地的快照」（PL:3633-3635），快照是落地的那份对象（PL:2274-2275）。不处理的话，快照里带着序号、新消息里是 0，快路径永远不命中，49–60% 的存盘跳过率归零。

处理（与压测探针在退出收敛比较里的做法相同，PL:794-804）：

- `PlayerLastPersistedSnapshotComp::Replace`（`last_persisted_snapshot_comp.h`）本来就是深拷贝，在拷贝之后清掉 `last_save_seq`。三个调用点（工作区 PL:2242、:2276、:2331，都是存盘落地回调）都经过它 [C]。剥离只在落地时做一次。
- **快路径的比较保持 `dirty_save::IsEqual(*message, *snap->snapshot)`**（HEAD 的写法）：快照已剥离，新消息在快路径判定时还没有 `last_save_seq`，C++ 也不写子表围栏，两边都不带非业务字段，不需要再剥。在途实现把快路径改成调 `player_exit::IsPersistedPayloadCurrent`，每次存盘尝试都深拷两份 `PlayerAllData`，热路径成本上升，并且顺带剥离了 `stress_test_probe`（见下）；v0.4 改回（§0.2 第 21 行）。
- `player_exit::IsPersistedPayloadCurrent`（PL:814 起）在两份副本上都清掉 `last_save_seq`。否则只要有两笔存盘在途，「落地的」和「当前的」序号不同，退出收敛永远判不相等。它在退出收敛与交接路径上调用，不在每次存盘的热路径上，深拷贝可以接受。在途实现在这里已加了对 `last_save_seq` 与四个 `fence_*` 的清除，保留；`fence_*` 那几行作为防御（C++ 不写它们）。
- 单测必须覆盖：连续两次无改动的存盘，第二次命中快路径。

**`stress_test_probe` 与快路径**：HEAD 现状 [I，未验证]：`STRESS_TEST_PROBE=1` 时探针字段进了快照但不在比较侧，快路径按阅读推断永不命中，与 PL:3604-3612 注释的意图不符。在途实现把快路径换成 `IsPersistedPayloadCurrent` 之后，探针模式下快路径会恢复命中，跳过率回到正常值，与 R20 等历史压测基线不可比（fence-inflight.md F9）。v0.4 先把快路径改回 HEAD 的比较，不在快路径剥离探针；**快路径是否剥离 `stress_test_probe` 单列给负责人确认**（§14 第 22 条）。选「剥离」的话，具体做法以 §14 第 11 条的核实结果为准（探针在快照与新消息两侧各是什么值），约束是不得在快路径上每次深拷贝；并在下一轮压测复盘里写明跳过率口径的变化（AGENTS.md §6）。

### 7.4 残余风险

进程级发号器 + 两路播种之后，墙钟只在「新进程 + epoch 没推进 + 种子落后于已发出的序号」时才起作用。会出错的条件要同时满足四条：

1. scene 进程崩溃，崩溃前有存盘的 DBTask 已发出但 Redis 那次写没落地；
2. 同一个 node_id 的新进程接回这名玩家，且 scene_manager 走了不铸造分支；
3. 播种的两路来源都看不到崩溃前已发出的最大序号：父键没丢（父键是旧快照）；或父键丢了，但那笔 DBTask 还没落库（在 Kafka 或 ready 里；L-04 之后存储不可用期间它会一直留在 ready）；或子表缓存里的围栏偏低（§6.3）。v0.2 把「父键丢了走冷路径」写成安全侧，不成立；
4. 新进程发号时的墙钟早于旧进程最后一次发号的时刻：同机时钟回拨，或新进程落在另一台时钟更慢的主机。量级：崩溃后 node_id 要等 etcd 租约（180 秒）过期才能复用（§7.1），停机时长的下限是分钟级，所以需要的回拨或主机间偏差也在分钟级，属于运维故障，不是几秒偏差就能触发。

另有一条 dev 旁路：`AllowUnsafeCrossNodeHandoff=true` 时新旧两个进程同持一个 epoch 并各自发号，先后只靠墙钟（§7.1）。只允许单机 dev。

后果：新进程的存盘被判 `older_seq`，ack 丢弃，直到序号追上。被丢的若是下线前、或状态不再变化前的最后一笔，快路径不再发 DBTask（`SavePlayerToRedisImpl` 的脏比较快路径），MySQL 停在旧值，直到该实体下一次真正写盘（通常是下次上线后的首存）；go/db 的 ack 丢弃对 C++ 不可见。期间父键丢失即回档。

缓解与观测：

- `db_fence_write_total{outcome="older_seq"}` 建基线告警（§10）。
- 要求 scene 主机的时钟同步用 slew 不用 step（附录 M6，待运维确认），并补 **scene 主机间时钟偏差的监控**（告警阈值 1 秒级，§7.2）：slew 管不到主机间偏差。
- 彻底关掉的办法是让 scene_manager 在进程换代时必定铸造 epoch。**已定（§14 第 8 条）**：直接做，`PlayerLocation` 记录节点实例身份（进程 UUID），实例变了就按「持有者已换」铸造；判断「实例变了」要有正面证据（注册表里该 node_id 的实例 UUID 与 location 里记的不同），并继续走再入屏障，不能把「暂时看不到」当成换代。归跨 zone 归属 / scene_manager 那条线单独立项；它是多主机环境切 `dual` 的前置，单机 dev 不要求。按阅读推断条件 2 可达（`samePhysicalNode` 只比 zone 与 node_id，`PlayerLocation` 没有实例字段；K8s 的 CrashLoopBackOff 可能把重启拖过 180 秒租约），未实测（fence-decisions.md 第 9 项）。
- 「用 `AllowUnsafeCrossNodeHandoff=false` 再跑一遍端到端」不写成 §11 第 7 步的硬性要求：`scene_manager_service.yaml` 注明标记链路尚未编译、未跑过测试，它应作为单独的验收项。

### 7.5 单测

放在 `cpp/tests/cross_zone_test/cross_zone_test.cpp`（单编译单元，只走 MSBuild，没有 CMakeLists）。全部在局部 `SaveSeqAllocator` 实例或纯函数上测，不碰 `tlsIdGeneratorManager`。用例名统一带 `SaveSeq` 前缀，§11 第 6 步按名逐条核对：

- `SaveSeq.NextMonotonic`：单调。
- `SaveSeq.NextMonotonicUnderClockRollback`：时钟回拨仍单调。
- `SaveSeq.NextSameMillisecond`：同一毫秒多次调用。
- `SaveSeq.ObserveRaisesFloor`：`Observe` 之后的下一个值大于下限。
- `SaveSeq.ObserveRejectsBeyondBound`：超界下限不抬高进程级 `last`。
- `SaveSeq.NextSaturatesWithoutWrap`：`last == UINT64_MAX` 时显式失败，不回绕。
- `SaveSeq.FloorFromAllData`：纯函数 `SaveSeqFloorFrom` 对带 `last_save_seq` / `fence_save_seq` 的 `PlayerAllData` 取三路 max（替代 v0.2「加载后发号器的下限」那条：`InitPlayerFromAllData` 在宿主里跑不通，见 §7.2）。
- `SaveSeq.SnapshotStripsLastSaveSeq`：快照不保留 `last_save_seq`。
- `SaveSeq.SecondUnchangedSaveHitsFastPath`：连续两次无改动存盘，第二次命中快路径。做法：用 `HandlePlayerAsyncSaved` 喂一份带 `last_save_seq` 的落地消息；先断言快照里 `last_save_seq` 为 0 且 `dirty_save::IsEqual(新 marshal, 快照)` 为真，再调 `SavePlayerToRedis` 断言返回 false。宿主里 `GetPlayerDataRedis()` 为空，实现出错时先在断言处失败，不会在写盘段对空指针解引用、连带后面的用例不跑。
- `SaveSeq.PersistedPayloadCurrentIgnoresSeq`：`IsPersistedPayloadCurrent` 不受序号影响。
- `SaveSeq.PersonalFloorBeyondBound`：`Observe` 超界时返回「超界」且进程水位不变；`ApplyPersonalFloor` 对同一玩家连续发号严格递增，且不低于个人下限 + 1（§7.2 已定方案 2）。
- `SaveSeq.PersonalFloorSaturates`：个人下限为 `UINT64_MAX` 时显式失败，不回绕。

测试宿主没有 Redis，`SavePlayerToRedisImpl` 的写盘段跑不到（现有用例只走快路径或直接调落地回调）。「DBTask 里带上了 save_seq」这一点靠本地端到端验证（§11 第 7 步）。

`Replace` 是头文件内联实现，`bag_test`（`player_battle_settlement_test.cpp`）与 `currency_test`（`asset_op_system_test.cpp`）直接调用它，改它会直接进这两个测试二进制，所以 §11 第 6 步要跑这两个工程。

---

## 8. 上线分期与兼容

**分期顺序**：Stage 0 → 1a（新 go/db，`legacy`）→ 1c（新 C++）→ 1b（逐 zone 切 `dual`）→（满足门禁后）开 `EpochOverride`。至少金丝雀 zone 必须先上新 C++ 再切 `dual`，金丝雀才覆盖 class-1（`older_seq` / `equal` / `epoch_override`）这条风险最大的路径；class-2 只在 C++ 回滚时出现，由单测和真库用例覆盖。`legacy` 下 go/db 不看 save_seq，先上 C++ 不改变 go/db 的行为。小节编号沿用 v0.2。

### 8.1 Stage 0：proto 与 DDL

**v0.4 现状**：表字段 18 / 19 / 3 / 4 已于 2026-10-15 在 HEAD 声明并生成，收件箱的 20 号随后也已声明。任何按当前 proto 编出的 go/db 都已经要求围栏列存在（启动闸），所以 Stage 0 的 DDL 实际已被列声明提前触发，落地恢复交接 §4 第 2 步要求它在任何新版 go/db / scene 启动之前完成。本节剩下的工作是 DBTask 9 / PlayerAllData 7 的注释与生成物，以及按 G-03 最新口径核对每个库。

1. 改两个 proto 的注释（§3.1、§3.3；号位在途已声明），按 §11 第 1 步重新生成。§3.4 的放行清单已由 G-03 更新，本批不改。
2. **库清单穷举**（Stage 1a 的门禁）：
   - 在每个 MySQL 实例上 `SHOW DATABASES`，取 `zone_%_db`、`player_store_%_db` 两个家族，加上各 zone 的 `AllowedDatabases`。按需打开从不建库，`AdmitStore` 也只放行白名单与这两个家族，所以这个集合覆盖了 go/db 可能打开的全部库。可选：用 `player:placement` 记录里出现过的落点编号交叉核对。
   - 每个库用 `cmd/migrate`（落点库加 `-storage-id`）跑 `-command plan`。**放行口径以 G-03 最新一条为准**（§3.4）：plan 为空（已迁过），或只含 `player_database` 六列（`profile_component` / `asset_op_ledger` / `settlement_ledger` / `fence_owner_epoch` / `fence_save_seq` / `inbox_ledger`）与 `player_database_1` 两列（`fence_owner_epoch` / `fence_save_seq`）中尚未迁移的那几条；出现清单外的语句，或 15 / 17 号列与 18 起的列同时出现（列序会和新库建表不一致，见 G-03），先停下核对。然后 `-command up`，再用 `SHOW COLUMNS` 逐列核对：`player_database` 六列、`player_database_1` 两列各返回 1 行。全部通过才进 Stage 1a。v0.3 写的「plan 只有 4 条 ADD COLUMN」已过时，照它核对会在合法状态下停住或误判漏迁。
   - 原因：启动闸只拦本 zone 库，漏迁的落点库不会拒启，只会让这批玩家的存盘一直延后（§3.2）。合服 runbook §4.4 有同类前置，可作先例。
3. 这一步之后、新二进制上线之前，旧 go/db 照常运行：`Save` 只写它认识的列，围栏保持 0。旧二进制会对每个库持续打「多余列」的 `SCHEMA-DRIFT` ERROR 日志，是预期的噪声。
4. `tools/merge_zone` 按列名拷行，要求两库列集合完全相同（`player_rows.go`）。所有库迁完之前不能合服或搬库；它自己会拒绝，方向安全。

### 8.2 Stage 1a：新 go/db，`Mode=legacy`

- 新二进制带着新 proto，启动闸会要求本 zone 库已有围栏列，否则拒启；围栏表判定（§4.1）要求两张玩家表在集合里，否则拒启。
- **`legacy` 下生效的变化**（与模式无关）：
  - 围栏表的 SQL 从 `Save` 换成只带 `PreserveFields` 的条件写，并盖 `(0,0)`（§4.2）：`SaveConditional` 下语句相同，影响 0 行时多一条存在性查询；过渡实现下多一条整行 `SELECT … FOR UPDATE` 与事务往返（§4.8）。非围栏表仍是 `Save`；
  - §4.8 的会话隔离级别 READ-COMMITTED 与启动探测；
  - §6.2 的缓存写法（写路径令牌发布脚本取代 verify + SET，读路径 NX）；
  - §5.5 的推进脚本（游标与已落库 epoch 原子推进，失败即延后）；
  - §4.6 的 SQL 之后 Redis 步骤用 `WithoutCancel` 加单步上限；
  - `db_fence_body_mismatch_total` 计数（§4.2）与新的 Redis step 名（§10）。
- **只在 `dual` 生效**：§4.4 的结局分类与死信、§5.1 的计数器探测、§5.2（另需 `EpochOverride`）、§5.3、§6.6 的资产证据发布。v0.3 这里写的「§4.2 盖章只在 `dual`」与 §4.2 矛盾，以 §4.2 为准（两种模式都盖，`legacy` 盖 `(0,0)`）。
- 观察项：
  - `db_task_deferred_total{reason="store_open_failed"}` 保持 0，日志里没有 `failed the read-only schema gate`（落点库漏迁，§3.2）。
  - **class-3 观察窗**（Redis 守卫在 `legacy` 下照跑、照计数）：该 zone 各 go/db Pod 的 `db_owner_epoch_guard_total{outcome="legacy_zero"}` 不增长；同期 scene 日志里 `[OwnerEpoch]` 行的 `owner_epoch_unknown` 不增长（它是进程累计值、每 30 秒打一行 WARN，不是 Prometheus 指标，所以写「不增长」，不写「为 0」）。
- 这一步是为了把「换二进制」和「换语义」分开。

### 8.3 Stage 1b：逐 zone 切 `dual`

前置条件（缺一不可）：

1. 该 zone 的 go/db 全部是新二进制。
2. **L-04 第 2 批的死信工具已落地并经 Codex 演练**：至少能按原因、按玩家列出和查看；重新入队时 RetryCount 清零；默认只演练不执行。`dual` 会让 `stale_owner` / `owner_epoch_regressed` / `legacy_after_seq` 进死信，没有工具就没有处置手段；L-04 §3.8 明确禁止用 `redis-cli` 把条目搬回 ready（预算耗尽的条目重试次数已满，下一拍会被立刻送回死信；`redis-cli` 也改不了二进制载荷里的 RetryCount）。**已定（§14 第 9 条）**：第 2 批死信工具由 go/db 存盘管线线来做，排在它的 R2（④）、R3 之后、本线 Stage 1b 之前；重新入队写进 ④ 之后的共享收件箱并走 WAITAOF 入队。R2 里的 `cmd/retry_requeue` 只把 p* 搬回共享列表，不是死信工具，替代不了。§5.4 不豁免失租之后，因失租进死信的条目可能根本没落库，这个工具是必要配套。
3. **条件写进死信的条目带原因**：放进同一个 `kafka:dead:queue:{topic}` 列表，载荷外面套带原因的新信封（原因、时间、行内围栏、任务的 `(E, s)`；L-04 §4.1 已规划「新条目带原因和时间」）。不另开独立的死信键：合服预检 `tools/merge_zone/preflight.go` 和 `db_retry_queue_length{queue="dead"}` 都只看这个列表，独立键会绕过两者。
4. **告警已生效，留证据**：HEAD 的 db ConfigMap 已配 `MetricsListenAddr: ":9160"`（v0.2 写的「目前没有」已过时）；还要确认 ServiceMonitor / PodMonitor，并手工 apply `deploy/k8s/owner-epoch-alerts.yaml` 与 `deploy/k8s/db-task-retry-alerts.yaml`（`docs/ops/godb-pipeline-launch-todo.md` 2.5.1、2.5.2 仍是待确认 / 待做）。配置证明不了告警生效，所以要做一次演练：在金丝雀 zone 用 data_stress 对测试号段制造一条 `legacy_after_seq`，确认 `DbFenceLegacyAfterSeq` 触发、`DbTaskDeadLetterAdded` 不重复触发（v0.4 已定在它的表达式里排除围栏原因，§10），并留证据。
5. **class-3 前置**：Stage 1a 观察窗内 `legacy_zero` 与 `owner_epoch_unknown` 都不增长（§8.2）；gate / scene_manager 全部是 CZ-4 之后的版本。非 0 时先归因：还没铸过 epoch 的存量玩家在同落点重连时写的是 `(0,0)` 行，切 `dual` 后无害；由负责人确认之后可以放行。不写成硬性「恒 0」，否则可能永远卡住。理由：`dual` 把 class-3 从「总能落库」改成「行上有围栏就进死信」。
6. 该 zone 已上新 C++（Stage 1c）。
7. 多主机环境：§14 第 8 条（进程换代必铸造 epoch，已定直接做，归 scene_manager 线）已落地（§7.4）。单机 dev 不要求。
8. 一条金丝雀 zone 先切，观察 §10 的指标至少一个高峰。金丝雀通过标准：`db_fence_write_total{class="seq",outcome=~"updated|inserted"} > 0`；记录 `older_seq` 比例与 `epoch_override` 次数（`EpochOverride` 开启之后才有）；`stale_owner` / `owner_epoch_regressed` / `legacy_after_seq` 为 0（第 4 条演练在测试号段上制造的那一条除外，演练前后各记一次计数）；`db_fence_cache_publish_total{result=~"evidence_conflict|evidence_ahead"}` 为 0。

切换：改配置后滚动重启。只改 ConfigMap 不会让 Pod 滚动，要显式 `rollout restart`，并用启动日志里的生效值核对。

**开 `EpochOverride` 的条件**（在 `dual` 稳定之后，逐 zone）：

- 该 zone 已经没有旧版 go/db（没有 §5.5 原子推进的二进制）。新二进制的 `legacy` 与 `dual` 混跑不影响开启。
- 最后一个旧实例下线后，重试 ready 与各实例 processing 至少**同时为 0** 过一次（看 L-04 的 `db_retry_queue_length`）：旧实例留在重试里的任务可能已经造成「游标推进、epoch 未推进」。④ 落地后重试键分成共享收件箱与按分区的 `kafka:retry:queue:<topic>:p*`，这条门禁与 `db_retry_queue_length` 的采样都要覆盖 p* 队列，键名以 `retry_routing.go` 为准（fence-v03-review.md F11）。
- 将来可以挂到 §8.5 的按实例能力登记上，做成机器门禁；本批靠发布纪律加上述观察。
- L-04 第 2 批的死信回灌受同一门禁约束（§9.4）。

### 8.4 Stage 1c：新 C++

新 C++ 开始发 `save_seq`，class-1 流量出现。按分期顺序排在 Stage 1b 之前（v0.2 写的「可以早于或晚于」改掉：晚于时金丝雀看不到 class-1）；`legacy` 下 go/db 不看 save_seq。

**并存矩阵**：

| 并存情况 | 结果 |
|---|---|
| 旧 go/db + 已加列的库 | 旧实例只写旧列，围栏不动。安全性同今天 |
| 新 go/db（`dual`）+ 旧 go/db | 旧实例的无条件覆盖 MySQL 挡不住，互斥仍靠双方都遵守的排序锁、游标、epoch 键。旧实例不推进围栏，围栏只会落后，不会误拒新写。**旧 go/db 在场时不得开 `EpochOverride`**：旧实例「游标已推进而 epoch 未推进」的窗口会让更旧的重试越过游标、被落后的围栏放行（§5.2，§17 状态机 F2）。不开时安全性同今天 |
| 新 C++ + 旧 go/db | 旧实例不认识 9 号字段，按未知字段原样保留并带进重试载荷。安全性同今天 |
| 旧 C++ + 新 go/db（`dual`）| 走 class-2 |
| 同一 zone 内新二进制的 `legacy` 与 `dual` 实例并存 | 安全：都保留 Redis 守卫，都用 §5.5 原子推进游标与已落库 epoch，§5.2 的放行只依赖原子推进的 epoch 记录，不依赖围栏精度。`legacy` 实例的写不带条件、不推进围栏 |
| epoch=0 的存盘（class-3）+ `dual` | 行上已有围栏时判 `legacy_after_seq` 进死信；今天会落库。之后同一玩家第一笔 class-1 存盘会整行追平。由 §8.3 第 5 条的前置排除 |
| C++ 从新版回滚到旧版 | `dual` 下安全：之后的写是 class-2，走第二步，不退围栏 |

F-5（§1.2）的成立条件就是上表第 2、5 行：没有旧二进制，或者旧二进制在场时 `EpochOverride` 关着。

> **2026-10-18 更正**：上表「同一 zone 内新二进制的 `legacy` 与 `dual` 实例并存：安全」只在严格提交屏障之前成立，现在作废。现行 scene / login 写 pending、要求两表 ACK，混跑时会出两种问题：落到 `legacy` 实例的写不写 ACK，玩家因此卡在登录与交接上；`dual` 补投器重投的旧意图被 `legacy` 实例无条件写进 MySQL（审计 go-db-pipeline#2）。两版混跑现在由机器门禁拦下，不再靠发布纪律：
> - `dual` 启动时写世代标记。
> - `legacy` 在两种情况下拒启：本 zone 有世代标记，或任一补投桶非空。运行中每 5 秒复查一次，发现即停机，并以非零码退出。
> - `legacy` 在线时会续写存活标记，`dual` 的补投见到这个标记就整拍暂停。
>
> 这只拦得住新二进制。不认识门禁的旧 go/db 二进制，仍按第 2 行的口径由发布纪律排除。详见文末 2026-10-18 附注。

### 8.5 `sql` 模式的契约（预留，本批不实现）

将来要做时必须满足：

- **机器门禁**，不能只靠发布纪律：`sql` 实例不维护游标和已落库 epoch，任何还按它们判定的实例（旧二进制、`dual`）读到的是停更的值。
- 现有的 `db:capability:zone:{zone}` **不能复用**：它每 zone 一把键、所有副本用同一个常量覆盖，`tools/merge_zone` 按值精确相等判定（`placement_ops.go:424-459`），改值会让现有的合服门禁判为缺失 [C]。
- `kafka:retry:instances:{topic}` 按实例登记重试租约（score 是 Redis TIME 算出的到期毫秒），不带能力信息。成员在正常停机归还收据后、或租约过期被存活实例搬空收据后，由同一段 Lua 摘除（`retry_ownership.go` 的 `retryReclaimScript`，最迟约 lease + lease/3）[C]。v0.2 写的「过期成员也不自动摘除」与代码相反。它仍不能直接作 `sql` 门禁：成员资格表示「最近持有重试租约」，不表示「正在落库」；活着的实例续租中断一次就会被别的实例回收并摘除，直到下次认领才重新登记；「登记表里所有成员都具备能力」这种判法会把缺席当成不存在，门禁 fail-open。
- **门禁契约**：无论新建一份按实例的能力登记，还是在这张 ZSET 旁边挂一个按 instanceID 的能力 hash、由同一组续租和回收脚本维护，都必须保证「实例的登记失效期间不消费、不落库」，与认领脚本「先续租再认领」是同一类不变量。实例只有看到登记表里的所有成员都具备能力时才真正按 `sql` 执行，否则自动按 `dual` 运行并告警。早于重试归属改造、不登记的旧二进制，靠下面的进入条件与发布门槛排除。
- 进入条件：class-2 / class-3 计数在观察窗内为 0；该 zone 的重试与死信队列清空过；缓存改用 §6.4 的值封装；G5 在 class-2 / class-3 上仍开着的残余（§5.6）已有处置；§5.2 游标后退后 class-3 越过游标的残余同样以「class-3 为 0」排除。
- 回退只能经过 `dual`。
- 立项判据见瓶颈文档 §6.3。

### 8.6 回退

| 回退 | 做法 |
|---|---|
| `dual` → `legacy` | 改配置滚动重启，随时安全。围栏列留着，`legacy` 不碰它们。切 `dual` 后若发现 class-3 进死信，先回退 `legacy` 恢复落库，再查来源 |
| 关 `EpochOverride` | 改配置滚动重启，随时安全 |
| 新 go/db → 旧 go/db | **先关 `EpochOverride`（或全部切 `legacy`），再换二进制**。旧代码忽略围栏列，Redis 守卫一直在维护。重新开启 `EpochOverride` 前要再满足 §8.3 的排空条件 |
| 新 C++ → 旧 C++ | `dual` / `legacy` 下安全（之后的写是 class-2，§4.3 两步写，不退围栏）。资产证据开关打开的 zone 先按 §8.7 关开关：旧 C++ 的写不发布证据 |
| DDL | 不回滚，不 DROP 列 |
| 资产证据相关 | 按 §8.7 的逆序 |

> **2026-10-18 更正**：上表第 1 行「`dual` → `legacy` 改配置滚动重启，随时安全」作废。现行 scene / login 只说严格提交屏障，单把 go/db 切回 `legacy`，玩家第二次登录起全部失败。门禁也会挡住这种切换：本 zone 已有世代标记，`legacy` 实例拒启。只有同时回退写者时才能降级到 `legacy`，按以下顺序做：
> 1. 把该 zone 的 scene / login 回退到不写严格屏障的版本，确认旧写者全部退出。
> 2. 让 `dual` 的补投器把本 zone 64 个补投桶全部回收清零（`ZCARD` 全为 0），停掉全部 `dual` 实例。
> 3. 运维显式删除 `db:save_protocol:zone:<ZoneId>`。
> 4. 起 `legacy`。
>
> 任一步不满足，`legacy` 实例都会在启动门禁处拒启。上表第 4 行「新 C++ → 旧 C++」仍然成立：go/db 保持 `dual`，旧 C++ 的写走 class-2。

`legacy` 运行一段时间后再切回 `dual`：`legacy` 期间的写不推进围栏，围栏落后于真实数据。围栏落后期间，这些行的保护退回到今天的水平，直到下一笔 `dual` 写把围栏推上来；§5.2 的放行不受影响（它依赖 §5.5 的 epoch 记录，不依赖围栏，§5.5 在 `legacy` 下也照做）。另外，`legacy` 期间行内围栏和缓存围栏都落后于已发出的序号，切回 `dual` 后冷加载的播种只剩墙钟（§7.4 条件 3）。`legacy` 期间不发布资产证据，该 zone 的证据开关必须先关（§8.7）。

### 8.7 资产 SQL 提交证据的上线与回退顺序（v0.4 新增）

在途实现让 data_service、go/db、C++、资产调用方（guild / trade）之间多了一条上线顺序依赖，任何文档都没写（fence-inflight.md F6 (c)）。v0.4 定为：

| 步 | 上线 | 为什么排在这里 |
|---|---|---|
| ① | 新 data_service（认识 `require_sql_commit`，回 `sql_committed`）| 旧版 data_service 不认识这个字段，回 `sql_committed = false`，开关打开的调用方会报错、无法终结 |
| ② | 新 go/db，`Mode=legacy`（Stage 1a）| 此时不发布证据；令牌发布脚本已在用 |
| ③ | 新 C++（Stage 1c）| class-1 流量出现 |
| ④ | 逐 zone 切 `dual`（Stage 1b）| 该 zone 的 class-1 写开始发布证据 |
| ⑤ | 打开该 zone 的证据开关（§6.6），配置项落在资产调用方 | 证据在该 zone 持续发布之后才开；开之前接受「已离线、尚无证据的玩家待办要等下次存盘」 |
| ⑥ | guild / trade 带开关的新版本滚动生效 | 开关关闭的 zone 上它们沿用 Redis blob 判定，行为同 HEAD |

回退顺序相反：先关该 zone 的证据开关（guild / trade 回到 Redis blob 判定），再 `dual` → `legacy`，再回退 C++、go/db、data_service。开关打开时不得把该 zone 切回 `legacy` 或回退 C++：之后的写不再发布证据，开着开关的待办会一直等。

**上线门禁：mail 发奖与商城到账（负责人 2026-10-16 拍板）**。mail 的附件发奖 Finalize、trade 的合同 / 官方商城到账（ORDER 终结）**强制**要求 SQL 提交证据，没有 Redis blob 的退化口径；而证据只在 go/db `Fencing.Mode=dual` 时发布。所以：
- 这两项上线前，必须先把它们会投递到的每个 zone 的 go/db 按 §8.3 的门禁切到 `dual`，并走完上表 ①–④（data_service、go/db、C++ 新版本已上线，该 zone 已观察到证据持续发布）。
- 在此之前两项**保持关闭**：mail 不对玩家开放附件发奖（不部署 go/mail 的发奖链路，或不放行其发件入口）；trade 的 `Commerce` 白名单保持为空、`Mall.URL` 留空（trade 的配置校验在配了 Commerce / Mall 而 `AssetOp.RequireSQLCommit` 关闭时会拒启，见 §6.6 落码记录）。
- 任一 zone 回退到 `legacy` 之前，先关闭这两项在该 zone 的入口；否则其后的待办会一直停在 AwaitDurable。

---

## 9. 运维流程

### 9.1 epoch 计数器倒退（B1）

**机制**：owner_epoch 只存在 Redis（`player:{id}:owner_epoch`，`go/shared/ownerepoch`），缺键时铸造脚本把缺键当 `"0"` 后 INCR（`owner_epoch.go`）。围栏把「已落库的最大 epoch」持久在 MySQL，`consumer:applied_epoch:*` 把它记在 Redis。如果计数器倒退，之后的存盘会在 **Redis 守卫或 SQL 处**被拒，取决于已落库 epoch（`applied_epoch`）是否同步倒退：

- `applied_epoch` 同步倒退：写通过 Redis 守卫，到 SQL 被围栏拒（`dual` 下归 `owner_epoch_regressed` 或 `stale_owner`，进死信）。
- `applied_epoch` 没有倒退：写在 Redis 守卫判 stale。今天 ack 丢弃、死信里没有载荷；`dual` 下由 §5.1 的探测进死信。v0.2 的处置只覆盖了前一半（§17 状态机 F3）。

**实际会触发的场景**（按 HEAD 与工作区取证）：

| 场景 | 计数器倒退 | `applied_epoch` 同步倒退 | 在哪里被拒 | 载荷去向（`dual`）| 依据 |
|---|---|---|---|---|---|
| `redis-cli FLUSHALL`，不清 MySQL | 清空 | 清空 | 基本不触发 | — | 账号目录也只在 Redis（`loginlogic.go`），清空后同一账号会重新建角、得到新的 player_id，旧行成为孤儿，不会再被写到 |
| 在线玩家所在的 Redis 被清 | — | — | 不触发 | — | C++ 带守卫的存盘在守卫键缺失时会按节点缓存的值补种（`redis_client.h`）|
| Redis 从旧备份恢复、AOF 丢尾、主从切换丢尾，**恢复时玩家离线** | 是 | 是（同一恢复点）| SQL（围栏）| 死信（`owner_epoch_regressed`）| 父键存在时 login 走快路径载入备份里的旧快照 S0；之后的写判 advance 通过 Redis 守卫，在 SQL 被行内更大的围栏拒。今天这些写会直接落库、静默覆盖 MySQL 里更新的 S1；`dual` 下 S1 被保住，造成丢数据的是 v0.2「归零后回灌」的处置（§17 事实 F1 序列 B）。HA 清单 `redis-ha.yaml` 目前没有接进部署脚本，单实例 AOF 走默认的 everysec |
| 同上，**恢复时玩家在线** | 是 | 先倒退，又被在途的高 epoch 写推回高位 | Redis 守卫 | 死信（§5.1 探测；今天 ack 丢弃）| C++ 先异步写 Redis、再无条件发 DBTask；go/db 收到高 epoch 判 advance 落库并把 `applied_epoch` 推回；C++ 的 CAS 被拒后销毁实体；玩家重进拿到倒退后的 epoch，之后的写在 Redis 守卫判 stale（§17 事实 F1 序列 A）|
| 手工删除个别 `player:{id}:owner_epoch` 键 | 是（从 0 起）| 否 | Redis 守卫 | 死信（§5.1 探测）| 玩家离线时 location 已删，同节点补种走不到，铸造得到 1 |
| G4a 拆分后核心实例单独回档（**未来**）| 是 | 否（`applied_epoch` 已迁到 redis-mapping）| Redis 守卫 | 死信（§5.1 探测）| `shared-redis-split-by-purpose.md` 的拆分第 2 批；今天两者在同一实例，不成立 |
| `k8s_zone_rollback.ps1` 带 `-ConfirmFlushSharedRedis` 的 FLUSHDB | 与 FLUSHALL 相同 | | | | 它清的是共享实例的 DB 0，所有 zone 的账号目录一起没。这本身就是事故 |

> **（2026-10-19 指向注，上表原文保留）** 「G4a 拆分后核心实例单独回档」一行的前提按 G4a 2026-10-18 修订稿改为：不再是 redis-mapping；该稿 §18.7 选 A（§18 基线）时 `applied_epoch` 随控制面迁到 zone 组实例 `redis-dbctl-g<N>`（`shared-redis-split-by-purpose.md` §18），本行结论（计数器倒退、`applied_epoch` 不同步倒退、在 Redis 守卫被拒）不变；选 B 时 `applied_epoch` 随核心闭包进核心 Cluster，核心单独回档时与计数器同步倒退，按上表第 3 行处理（该稿 §18.7、§20.3，二选一待 §25.2 第 1 条）。§14 第 19 条要求拆分稿的回档 runbook 引用本节；该稿 §18.7 硬依赖第 4 条另加了 §9.2。（本段 10-22 订正为条件式，并更正出处。）

> **（2026-10-22 更正注，上表原文保留）**「恢复时玩家离线」一行的「`dual` 下 S1 被保住」只在一种情形下成立：丢掉的尾巴里含有产出 S1 所用 epoch 的那次铸造，且回退后新铸的 epoch 低于 S1 的 epoch。
>
> 更常见的情形是尾巴里只有存盘、没有铸造（S0 与 S1 同为 E 代，只是 seq 不同）。login 快路径载入 S0，下一次进场铸出 E+1，第一次存盘在 SQL 判 advance，整行覆盖 S1。回退后新铸恰好等于 S1 的 epoch 时，按 seq 比较，新进程的 seq 通常更大，同样覆盖。
>
> 另一种结局：go/db 在回退后的屏障上把 ACK 推到 S1，高于回退后的 pending，login 永久拒登。
>
> §14 第 7 条的墙钟地板只能防重铸同值，防不住这条：新持有者本来就有权越过 SQL 行，错在它载入的是旧内容，只能在加载侧堵。合并处置见 [zone-redis-regression-guard-20261022.md](zone-redis-regression-guard-20261022.md)（G1 身份戳与自动采纳、G2 ACK 高于 pending、G3 铸造持久化）。

备份恢复、AOF 丢尾、主从切换丢尾都只丢末尾一段，计数器与 `applied_epoch` 在恢复点上一致；真正让 `applied_epoch` 高于计数器的，是恢复后仍在流动的高 epoch 在途写，所以「在 Redis 守卫被拒」这一行只在恢复时玩家在线时出现。

**处置：定真源、抬计数器、不归零、不回灌**（替换 v0.2 的「归零加回灌」与「排空前置」）：

- a. **定真源**：人工比较 Redis 父键（§3.3 的 `last_save_seq` 与业务字段）和各落点库行内的围栏与业务字段，确定以哪边为准，并记录被放弃一侧的进度（是否补偿由负责人定）。
- b. **踢下线**并等退出存盘落地，确认 `player:{id}:location` 不存在。在线时抬计数器等于废黜当前持有者：缓存值 CAS 失败、实体被销毁，这次存盘的内容丢失。
- c. **抬计数器**：用只升不降的 Lua（拼 `luaDecimalGreaterFn`，§5.5）把计数器抬到 `C' = max(当前计数器, 各表各落点库的 fence_owner_epoch, 各表各 topic 世代的 applied_epoch, 收件箱的 max_fence_epoch, 资产证据里的 owner_epoch) + 余量 K`。K 覆盖恢复点之后铸过、但还没落库的 epoch（仍在 Kafka 积压或重试队列里），否则抬升后新铸的 epoch 会与它们撞号。**已定（§14 第 5、20 条）**：用常数 K（建议 10^6 量级），不要求先排空。epoch 是 uint64，全仓对 epoch 只做大小比较（go/db 守卫、收件箱 `DecideFence`；C++ 的 CAS 是等值比较），跳号无害；先排空要停整个 zone 的存盘管线，代价远大于收益。若 §14 第 7 条的「铸造带墙钟下限」落地，K 可直接取当前毫秒。
- d. **缓存**：真源是 MySQL 时，删除 `PlayerAllData:{id}` 和各 `{MsgType}:{id}` 子表缓存，下次登录冷读。真源是父键时不删：进场后快照为空，第一次存盘必然写，而且每次都是两张子表整行，以 `C'+1` 自然落库。
- e. **死信**：两种情况都把该玩家倒退期间的死信载荷标记作废，不回灌（§9.4）。
- f. **不归零围栏，不动 `applied_epoch`。** 归零加回灌会用旧快照派生的数据覆盖更新的数据（事实 F1 序列 B）。若仍保留归零分支（例如 §9.3 `debug_import` 导入的围栏过大），必须同时 `DEL` 该玩家所有 topic 世代下的 `consumer:applied_epoch:{topic}:{id}:{表}`，并先排空；建议同样改用抬计数器。

**入口**：从 v0.2 的 `fence-reset` 改为 `epoch-raise`。**已定（§14 第 5 条）**：独立的 `go/db/cmd/epoch_raise`（不进 `cmd/migrate`：migrate 管表结构，这是数据修复，职责不同；放 go/db 是因为 `applied_epoch` 的键格式、topic 世代、落点库注册表都是 go/db 的内部知识）。

- 按玩家执行，默认只演练，带 `-confirm` 才写；输出抬升前后的值以及每个来源的取值。
- 工具自己收齐全部来源，任一来源读不到默认拒绝执行：各落点库两张玩家表的 `fence_owner_epoch`、各 topic 世代的 `consumer:applied_epoch`、location（确认已下线）、收件箱的 `max_fence_epoch`（经 `PlayerInboxAdmin.GetSummary` 管理 RPC，不直连收件箱的库；即收件箱遗留项 C4）、资产证据 `player:{id}:asset_sql_commit` 里的 epoch（计数器倒退后新铸的 epoch 会低于它，证据发布脚本会一直返回 2，§6.2）。`-floor` 只作为未知来源的补充入口保留。
- 计数器读写经 go/db 的只读客户端 `OwnerEpochRedis` 所指的核心实例（§5.1；写入用同一地址的写客户端）。
- 脚本和 runbook 只调用这个入口，不各自内嵌 Lua 或 SQL。它让计数器多了一个只升不降的运维写入方，与 `go/shared/ownerepoch/ownerepoch.go` 包注释「只经 scene_manager 的 INCR 前进」以及 `scene-owner-reentry-barrier.md` 里「唯一写入方」的表述冲突，落码时同步改这两处契约。

**seq 超前**：玩家行内的 `fence_save_seq` 超出 §7.2 上界时（坏数据、导入），抬计数器同样是处置：下一次进场铸出的新 epoch 在高位胜出，任何 seq 都不再起作用。

**死信增长**：倒退期间该玩家每次存盘产生两条整行载荷，进 noeviction 的共享 Redis，所以 `DbOwnerEpochRegressed` 是 critical，处置时限按小时计（增长上限的算法见 §9.4）。

**要同步的文档**：

- `AGENTS.md` §6.2：负责人已定「条件写落码时再加」（交接 §5 第 14 项）。内容：「只清 Redis、不清 MySQL 且会复用 player_id 时，先按条件写设计 §9.1 抬计数器」；data_stress `-fenced` 按 §11 第 8 步的规则每轮换 E 时不会触发 `stale_owner`，固定 E 又只清 Redis 时结论无效。另一会话对 §6.2 的修改（清全局库路由表）建议合成一次修改。
- `docs/ops/merge-zone-runbook.md` §9.2「Redis 整体丢失」的恢复步骤、`docs/design/zone_data_rollback.md` 的回档步骤：各加一步「定真源、抬计数器」。G4a 拆分稿的回档 runbook 引用本节流程。
- `go/shared/ownerepoch/ownerepoch.go` 包注释、`docs/design/scene-owner-reentry-barrier.md`：写入方契约（见上）。
- `deploy/k8s/owner-epoch-alerts.yaml` 里 `DbStaleOwnerWriteRejected` 的描述：注明倒退已由 `regressed` 单独区分，这条告警只剩僵尸与 K1（§10）。
- 交接里提到的「Redis HA 故障 runbook」在 `docs/ops/` 下不存在；`dev_tools.ps1` 也没有独立的清 Redis 命令 [C]。

**长期**：§5.1 的探测只是检测手段，计数器追平之后就失效。根治仍靠给 epoch 的铸造加持久下限。交接写的是「并入 D1 映射落库」，但 D1 提案（`player-route-mapping-durability-proposal.md`）里没有对应条目 [C]。**已定（§14 第 7 条）**：铸造带墙钟下限，新 epoch = `max(当前 + 1, 当前毫秒)`，Redis 回退后第一次铸造自动越过所有历史值，不需要新存储；归 scene_manager / 跨 zone 归属线单独立项，不阻塞 Stage 1b，做完之前按探测加 `epoch-raise` 人工处置运行。要点（落码时核对）：`luaMintEpochAndSetLocation` 的重放识别依赖「恰好 +1」，时间要由调用方经 ARGV 传入，重放才保持幂等；回滚用的 INCR 不受影响；值约 1.8e12，远低于 2^53。仓库有同类先例：帮会资产流的纪元就是「建行时写入当前毫秒」（`guild-phase2/04-asset-channel.md`）。

### 9.2 回档（PITR）

- MySQL 恢复到旧时间点后，围栏随数据一起回退。
- 回档流程如果同时清了 Redis，游标和已落库 epoch 也没了，比恢复点更新的在途任务会通过围栏落库，把回档结果冲掉。
- 所以回档 runbook 必须保证：重启 go/db 之前，该 zone 的 db_task 位点已经跳过恢复点之后的全部消息，`kafka:retry:*`（④ 之后含 p* 队列）与死信里该 topic 的条目已清理，子表缓存已删除，**资产 SQL 提交证据 `player:{id}:asset_sql_commit` 已删除**（v0.4：证据会指向已被回滚的提交；不删的话，回档后新写的版本低于证据版本，令牌发布脚本返回 2，资产调用方拿错误的账本终结对侧账，§6.6）。
- 现有脚本和 runbook 都没有这一步 [C]：`k8s_zone_rollback.ps1` 的 Step 5 用 `--to-datetime` 重置位点，方向存疑；共享消费组下这一步通常直接失败。这与交接 §4.2 的「回档 Step 2」是同一件事，建议在那一项里一并改。

### 9.3 合服、搬库、导入导出

- `tools/merge_zone` 按列名拷行，围栏随行一起走；逐列相同的判据（`identicalRowCondition`）自动包含新列。不需要改代码。
- 搬库（relocate）之后落在旧库的写会重投到新库，在新库上按围栏裁决（`player-storage-placement.md` §6.3 的 LWW 语义不变，判据从游标换成围栏）。R2 等排序锁的逻辑在 `dual` 下不变。
- 子表缓存键没有新增，`playerCacheKeys` 不用改。资产 SQL 提交证据键要加进 `tools/merge_zone/player_rows.go` 的 `invalidatePlayerCaches` 删除清单，并补用例（§6.6）。
- **2026-10-20 修复（r2-mz#1）**：前向 copy 合服不再删 `player:{id}:save_barrier`（`invalidatePlayerCachesKeepBarrier`），撤销与 PITR 照删；`-verify-merged` 新增 `verify:save_barrier` 逐人核对，`save_barrier_restore` 新增 plan 期开关 `-allow-home-fallback`。理由与边界见文末「上线状态」之前的同日附注。
- `go/data_service/cmd/debug_import` 会把导出时的围栏值一起写回。从别的环境导入的行如果围栏比本环境的 epoch 大，之后的存盘会进死信；`fence_save_seq` 超出 §7.2 上界时，冷加载会触发 `Observe` 的超界处置（§7.2）。两种情况都按 §9.1 抬计数器处置，不建议归零。
- **2026-10-22 修复（r2-ds#1 / r2-ds#2，未编译、未测试，待 Codex）**：上一条描述的是旧 playerdb 导入；player-bundle 导入不搬线上围栏，在本地重铸版本 E = max(线上, 本地 owner_epoch, 本地两行围栏, 本地屏障令牌) + 1，并在本地行围栏或屏障令牌高于本地 owner_epoch（本地 Redis 丢过状态、Kafka 可能有看不见的更高代次残留）时拒绝导入，所以本地残留 DBTask 的代次都低于 E、会被行内围栏判 stale_owner。写前身份闸对取不到身份的一侧（TiDB、旧版导出包）fail-closed，要目标库带本地标记表或显式 `-allow-unverified-target-identity`。细节与操作见 [online-debug-data-fetch.md](../ops/online-debug-data-fetch.md) §7。
- **2026-10-22 修复评审补充（未编译、未测试，待 Codex）**：Redis 写目标的 `run_id` 与来源不同不再算证明（线上 Redis HA 切主、重启都会换 `run_id`，闸门放行后会在线上删父快照、改屏障与 owner_epoch），Redis 目标也要本地标记键 `debug_import:local_target`；MySQL 身份比对按角色配对（落点库对落点库、全局库对全局库，对应来源没读就要标记）；旧 player / playerdb 模式补上同一道闸（只认标记或显式放行）。同见 online-debug-data-fetch.md §7「评审补充」。

### 9.4 死信里的条件写条目（`fence_stale_owner` / `owner_epoch_regressed` / `fence_legacy_after_seq`）

- 死信目前没有消费者也没有重放工具 [C]。这三类条目需要人看、需要能按原因和按玩家查看，依赖 L-04 第 2 批的死信工具；工具落地并演练是 Stage 1b 的前置（§8.3 第 2 条）。
- **不用手工 Redis 命令处置**（删除 v0.2 的「工具落地之前靠手工 Redis 命令」）：L-04 §3.8 与 `deploy/k8s/db-task-retry-alerts.yaml` 的告警描述都写明「不要用 redis-cli 把条目搬回 ready」，预算耗尽的条目 RetryCount 已满，下一拍认领时会被立刻送回死信，而 `redis-cli` 改不了二进制载荷里的 RetryCount。
- 条目格式：同一个 `kafka:dead:queue:{topic}` 列表，载荷外套带原因的信封（§8.3 第 3 条）。在第 2 批工具落地之前，`owner_epoch_regressed` 只能靠日志前缀与计数区分。
- **`owner_epoch_regressed` 只作证据、不回灌**：按 §9.1 的处置，真源确定之后倒退期的载荷一律作废。这也绕开了「RetryCount 已满、回灌后立刻再进死信」的问题。
- `fence_stale_owner`、`fence_legacy_after_seq` 的回灌，以及 L-04 第 2 批的回灌工具（重试次数清零后搬回 ready）本身，都会经过 §5.2，受 `EpochOverride` 门禁约束：门禁开启之前入死信的写，回灌前要人工核对。
- **增长上限**：速率约等于「受影响的在线玩家数 × 2 ÷ 存盘间隔 × 单行字节数」，单行字节取 `db_blob_row_bytes` 的 p99。noeviction 的共享 Redis 上会同时堆积三类唯一副本：条件写死信、落点库缺列时不耗预算的延后（§3.2）、L-04 的存储类延后。告警统一看 `DbTaskRetryQueueGrowing`、死信长度与 Redis `used_memory`；达到阈值时先止血（定位受影响玩家、按 §9.1 处置倒退，或回退 `legacy`），再清理。
- 按 (玩家, 表) 只保留围栏最大的一条（让死信内存按玩家数封顶）：**已定不做**（§14 第 17 条）。死信是证据库，§9.1「定真源」要看倒退期间到底写了什么；还要换数据结构，并同步改合服预检和死信长度的采样。

---

## 10. 可观测

**指标**（低基数，不带 player_id）：

| 指标 | label | 用途 |
|---|---|---|
| `db_fence_write_total` | `class` ∈ seq / legacy_epoch / legacy_zero / unfenced；`outcome` ∈ updated / inserted / unchanged / equal / older_seq / stale_owner / owner_epoch_regressed / legacy_after_seq / race / error | 围栏结局分布；`unfenced` 是非围栏表的写（§4.1），只记 updated / inserted / unchanged / error。**被拒写的计数并入这一个指标**，不另设在途讨论过的 `db_player_row_stale_total`：拒绝原因本来就是 `outcome` 的取值，拆成两个指标只会让同一件事有两个口径。过渡实现（§4.8）不区分 updated 与 unchanged，一律记 updated；换成 `SaveConditional` 后才有 unchanged，看板注明 |
| `db_ordering_guard_total`（**已存在，扩展**）| `outcome` ∈ apply / apply_no_cursor / stale / incomparable / error（现有）+ stale_no_epoch / epoch_override（新增）| 游标守卫分布；新增两个是 `stale` 的子集，是 §5.2 的新路径，不与 `stale` 相加（§5.2）|
| `db_fence_cache_publish_total` | `path` ∈ write / read；`result`：write 路径 ∈ written / not_owner / evidence_ahead / evidence_conflict，read 路径 ∈ written / exists | §6.2 的令牌发布脚本（含资产证据）与读路径 NX |
| `db_fence_body_mismatch_total` | — | body 里带了非 0 围栏（§4.2），两种模式都计 |
| `db_fence_rejected_bytes_total` | `outcome` ∈ equal / older_seq / stale_owner / owner_epoch_regressed / legacy_after_seq / epoch_override | 到达 SQL 后被拒的写所下发的行字节数（UPDATE 的 SET 带全部列）；`epoch_override` 另起一个 label，统计 §5.2 放行后被围栏拒掉的那部分。§11 第 9 步作为对比项（附录 M2）|

现有指标的变化：

- `db_owner_epoch_guard_total` 的 `outcome` 增加 `regressed`（§5.1），同步改 `metrics.go` 里该指标的 Help（注释要求 Help 与 `OwnerEpoch*` 常量一一对应）。
- `db_task_deferred_total` 的 `reason` 增加 `fence_race`、`fence_evidence_conflict`（§4.4），加进 `deferReasons` 预建。
- `db_ordering_lease_lost_total{reason="owner_changed"}` 多两个来源：令牌发布脚本与推进脚本返回 0（§5.4）。Help 里 `verify_error` 的说明补「或令牌发布脚本的往返错误」。
- **`db_task_redis_step_seconds` 的 step 取值改变**（`metrics/pipeline.go` 的 `RedisSteps`，现 11 个）。新列表按一条写任务的执行顺序：`lock_acquire`、`cursor_get`、`epoch_get`、`owner_epoch_probe`（新，§5.1 与 §4.4 的计数器探测）、`placement_lookup`、`expand_get`、`placement_recheck`、`lease_verify`（保留，写任务只剩 `older_seq` 路径用）、`cache_publish`（新，写路径令牌发布脚本，取代写任务的 `lease_verify` + `cache_set`）、`cache_set`（保留，只剩读路径的 `SET NX`）、`applied_advance`（新，§5.5 推进脚本，取代 `cursor_set` + `epoch_mark`）、`lock_release`，共 12 个；`cursor_set`、`epoch_mark` 删除。
  - 这是压测对比的固定契约：`pipeline.go` 文件头与 Help、`RedisSteps`、`metrics_test.go` 的 `TestRedisStepNamesPinned`、`tools/scripts/stress_summarize.ps1` 的 2e 段（按精确名字取列）、`tools/scripts/test_stress_summarize.ps1` 必须同批改（§12）。这几个文件是 go/db 存盘管线会话的，按 §0.2 合并前置与它协调。
  - 与 R20 基线对比时，`lease_verify` / `cache_set` 两列的含义变了，`cursor_set` / `epoch_mark` 两列消失，复盘文档要写明口径变化，不能把新旧列直接相减。
- `db_task_dead_letter_total` 的 `reason` 增加 `fence_stale_owner`、`fence_legacy_after_seq`、`owner_epoch_regressed`，加进 `deadLetterReasons` 预建，同步改 Help（§4.4）。
- `db_stale_owner_write_rejected_total` 保留；SQL 判出的 `stale_owner` 也计入它；`owner_epoch_regressed` 不计入，单独由 `regressed` 计数。
- 启动日志打印 `Fencing.Mode` 与 `Fencing.EpochOverride` 的生效值（§4.7）。

**告警**（追加到 `deploy/k8s/owner-epoch-alerts.yaml`，写法同现有两条）：

| 告警 | 条件 | 级别 |
|---|---|---|
| `DbFenceStaleOwner` | `outcome="stale_owner"` 有增长 | critical |
| `DbOwnerEpochRegressed` | `db_owner_epoch_guard_total{outcome="regressed"}` 或 `db_fence_write_total{outcome="owner_epoch_regressed"}` 大于 0（绝对值）| critical，处置时限按小时计（§9.1）|
| `DbFenceLegacyAfterSeq` | `outcome="legacy_after_seq"` 大于 0 | critical |
| `DbFenceRace` | `db_fence_write_total{outcome="race"}` 大于 0（绝对值）| warning |
| `DbFenceOlderSeqHigh` | `older_seq` 占 class-1 写的比例超过基线 | warning，阈值待压测确认 |
| `DbFenceBodyMismatch` | 大于 0 | warning |
| `DbAssetSQLCommitAnomaly` | `db_fence_cache_publish_total{path="write",result=~"evidence_conflict\|evidence_ahead"}` 大于 0（绝对值）| critical：`evidence_conflict` 是同一 `(E, s)` 两份内容（撞号），`evidence_ahead` 多半是回档后没清证据，两者都可能让资产通道错账或卡住（§6.6）|

- 该文件里记了一条经验：CounterVec 的序列从无到 1 时 `increase` 会漏第一次，所以关键告警用绝对值。新规则照此处理。
- 进死信的三类条目也会触发 L-04 的 `DbTaskDeadLetterAdded`（warning），与上面的 critical 告警同时响。**已定（§14 第 16 条）**：在 `deploy/k8s/db-task-retry-alerts.yaml` 的 `DbTaskDeadLetterAdded` 表达式里加 `reason!~"fence_stale_owner|fence_legacy_after_seq|owner_epoch_regressed"`，每类死信只归一条告警。仓库里没有 Alertmanager 配置，inhibit 方案没有落点；接受重复会让值班先看到 warning、低估严重度。该文件 go/db 存盘管线线的 R5 也在改，先与它对齐再同批修改。`fence_evidence_conflict` 是延后原因、不是死信原因，以 `budget_exhausted` 进死信时仍由 `DbTaskDeadLetterAdded` 报，另有上表的 critical。
- `DbStaleOwnerWriteRejected` 的描述里注明：计数器倒退已由 `regressed` 单独区分，这条告警只剩僵尸与 K1。
- L-04 的 `DbTaskDeferredByStore` 只统计存储类原因，`DbTaskRetryQueueGrowing` 要求 ready 超过 1000，单条任务因 Race 空转触发不了它们，所以 `DbFenceRace` 必须单独加。

**日志**（R1 第 3 条：新写的运行期日志一律英文，每个结局都有对应计数器；在途 `runDBOp` 里那行中文 INFO 删除）：`stale_owner` 沿用 `STALE-OWNER-WRITE` 前缀并写明来源是 SQL 围栏、行内围栏值；计数器倒退用 `OWNER-EPOCH-REGRESSED`（写明计数器值、已落库 epoch 或行内围栏、拒绝位置）；`legacy_after_seq` 用 `FENCE-LEGACY-AFTER-SEQ`；body 带非 0 围栏用 `FENCE-BODY-MISMATCH`；资产证据用 `ASSET-SQL-COMMIT-CONFLICT` / `ASSET-SQL-COMMIT-AHEAD`（带 player_id 与两边版本，player_id 只进日志不进 label）；`older_seq` 只计数、按 DEBUG 记（量可能大，告警靠比例）；非围栏表每个 MsgType 首次出现打一行 ERROR（§4.1）。

**前提**：仓库不部署 Prometheus，告警文件是手工 apply。HEAD 的 db ConfigMap 已配 `MetricsListenAddr: ":9160"`；ServiceMonitor / PodMonitor 与告警文件的 apply 仍待确认，生效与否以 §8.3 第 4 条的演练为准。

---

## 11. 测试与 Codex 验证

前提：Claude 没有编译、没有运行任何测试，下面全部由 Codex 执行。任一步失败就保留错误和日志摘要后停下，不连续重试、不改断言、不跳过测试。

**0. 前置**：L-04 第 1 批已在工作区。§0.2 的两条合并前置已满足（「全域到账续批」写入方停手；go/db 会话的 R2 / R3 文件清单已到、本线已 rebase）。原语：proto2mysql `v0.3.0` 发布之前用过渡实现（§4.8），不需要改 go.mod；发版之后（§14 第 21 条），`go/db/go.mod` 的 require 与 replace 两侧都升到 `v0.3.0`，换 `fenceSave` 函数体，第 2、3 步原样再跑一遍。

**1. proto 重新生成**（§14 第 6 条，已定）

- 命令（仓库根，PowerShell 7）：生成器源码变化或尚无工具时先 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`，再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。本批不是客户端任务，必须带 no-unity 配置（AGENTS.md §4.1 已于 2026-10-15 订正，`go/build.bat` 不能代替完整生成）。
- **只核对 DBTask 9 与 PlayerAllData 7 相关的生成物**。四个 `fence_*` 字段已于 2026-10-15 生成入库，不在本批核对范围；在途实现又已生成过一次 9 / 7，但不完整。本批改的是两个 proto 的注释，生成物里只应出现注释与（未生成处的）字段变化：
  - `cpp/generated/proto/common/database/player_cache.pb.{cc,h}`、`cpp/generated/proto/db/db_task.pb.{cc,h}`（4 个）；
  - `go/proto/common/database/player_cache.pb.go`、`go/proto/db/db_task.pb.go`（2 个）；
  - `robot/vendor/proto/db/db_task.pb.go`（1 个，在途那次生成后仍没有 `save_seq`，本批必须出现）；
  - `generated/proto/_unified/proto/`、`generated/proto/db/proto/`、`generated/proto/login/proto/`、`generated/robot/proto/` 四处各自的 `common/database/player_cache.proto`、`db/db_task.proto` 副本（8 个；`generated/robot/proto/db/db_task.proto` 在途那次也没有 `save_seq`）。
- 通过标准：上述 15 个文件里 `save_seq` / `last_save_seq` 都存在且注释是 §3.1 / §3.3 的定稿；`git diff` 里没有这 15 个之外、由本批引起的变化（工作区里其他会话的在途生成物先用 `git diff --stat` 记下基线，再对比）。`cpp/generated/proto/db/db_task.grpc.pb.{cc,h}` 不应变化，变化即停下。加字段不产生新文件，CMakeLists 与 vcxproj 不用改 [C]。
- 生成器路径上的三份陈旧 `mysql_database_table.sql`（`go/db/model/`、`go/login/model/`、`go/player_locator/`）本批不碰；生成时若被改动就还原，去留另开小任务（§14 第 6 条）。

**2. Go 单测**（不依赖外部服务；工作目录 `go/db`）

```sh
go vet ./...
go test ./internal/kafka/... ./internal/logic/pkg/proto_sql/... ./internal/config/... ./internal/metrics/... ./internal/locker/... ./internal/storeerr/... -count=1
```

另外三处（资产证据相关，§6.6）：工作目录 `go/shared` 跑 `go test ./assetop/... -count=1`；工作目录 `go/data_service` 跑 `go test ./internal/logic/... -count=1`；工作目录 `tools/merge_zone` 见第 5 步。仓库根跑 `pwsh -File tools/scripts/test_stress_summarize.ps1`（step 名变化，§10）。

要改的既有测试（签名与 step 名变化引起，fence-v03-review.md F7、F11）：`key_ordered_consumer_test.go` 的 `recordingHarness`、`consumer_shutdown_test.go`（替换 `dbOpHandlers` 处）、`run_db_op_test.go`（固定 `runDBOp` 顺序的用例按 §4.5 补被拒结局的两处出口与 `cache_publish`）、`instrumentation_test.go`（step 计时）、`metrics_test.go` 的 `TestRedisStepNamesPinned`（12 个 step，§10）、在途的 `player_cache_publish_test.go`（随文件删除，意图并入令牌脚本用例）。前三个有 go/db 会话的在途改动，rebase 后再改。

新增用例必须覆盖：

- 围栏结局归类全表（§4.4 每一行，含 `owner_epoch_regressed`，含不可能分支报错）；被拒结局以 `(outcome, nil)` 返回，`reportStoreResult` 收到 nil；**被拒结局不调推进脚本、游标与已落库 epoch 都不动**（在途实现的回归：被拒写推进游标后，同分区 offset 更小、版本更新的重试被丢）。
- `equal` 不比内容：同 `(E, s)`、body 里资产 / 结算 / 收件箱子消息「置位但为空」与库内「未置位」时仍判 `equal` 并补发缓存（fence-inflight.md F2 的场景）。
- `older_seq`：复核通过且核锁通过才 ack；复核不过 → `errPlacementMoved` 延后、不耗预算；核锁失败 → `errOrderingLeaseLost` 延后；死信三结局不调复核与核锁（§4.5）。
- 错误标记：条件写路径（过渡实现的事务与 `SaveConditional`）返回的驱动错误都带 `storeerr` 标记，连接类错误触发 `conn_lost` 探活，1205 / 1213 归 `store_busy`、写不耗预算（R1 第 2 条）。
- 过渡实现 `fenceSave`（§4.8，用 `dbtest` 假驱动按语句脚本化应答）：行存在且条件满足 → `Save`；条件不满足 → `rejected` 带 `Stored`、不发写语句；行不存在 → `Insert`；`Insert` 撞 1062 → 同一事务内重读一次再判；重读仍无行 → 不可能分支报错；`PreserveFields` 列从当前行拷入；严格模式与 `AllowEqual` 的边界（相等、epoch 高位）。
- 模式 × 类别的 `SaveOptions`（§4.3），含 class-2 的两步、`legacy` 不清围栏、`legacy` 也把消息围栏置 `(0,0)`（§4.2）；body 带非 0 围栏时两种模式都计 `db_fence_body_mismatch_total`。
- 围栏表判定（§4.1）：非围栏表在 `legacy` 和 `dual` 下都照常 `Save` 落库并计 `unfenced`；非围栏表不触发 `epoch_override`；启动断言列表里的表缺围栏字段时拒启。
- 错误去向（§4.4）：`ErrConditionalSaveRace` 延后、耗预算、`deferReason=fence_race`；`SaveConditional` 返回的驱动错误仍带 `storeerr` 标记（1205 / 1213 / 9007 / 8002 归 `store_busy`、不耗预算）；条件写哨兵不带标记。
- `handleTask` 级别（miniredis）：`older_seq` 先复核、复核不过则延后；`stale_owner` / `owner_epoch_regressed` / `legacy_after_seq` 以对应 label 进死信，且不写缓存、不推进游标；`equal` 补发缓存。
- §5.1 计数器探测：`applied_epoch=7`、计数器缺失或为 1、任务 epoch=1 → 进死信、不 ack、计 `regressed`；`applied_epoch=8`、计数器=8、任务 epoch=7 → ack（行为同今天）；探测读失败 → 延后且耗预算；`legacy` 不探测。
- §5.2：表里每一行；`EpochOverride=false` 或 `legacy` 下不放开；stale + first 丢弃并计 `stale_no_epoch`；**两笔同 epoch 的重试排在老主迟到写之后，最终落库的是较新的一笔**；override 之后老 epoch 的重投被 epoch 挡住；override 路径上 `equal` 的游标改写；推进脚本失败后能自愈（延后 → 重试 → `equal` → 脚本成功）。
- §5.5 推进脚本：令牌不符返回 0 → `setLost(owner_changed)`、延后（`lease_lost`）；正常路径游标前进、epoch 按十进制串取 max（含大于 2^53 的相邻值，确认能推进）；Redis 错误 → 延后，不只记日志。**epoch 键是非规范值（如 `'0x10'`、`' 5 '`、`'1e3'`）或任务 epoch 非规范时脚本报错，且游标键未被改动**（先校验后写，fence-v03-review.md F4）。**混跑对照**（用测试替身模拟旧二进制的两步推进并让 epoch 那一步失败）：旧实例 + 放行 → 复现回退，证明门禁必要；新二进制 `legacy` + `dual` → 不回退。
- **Stop 第④步交错**：worker ctx 在 SQL 已提交之后、令牌发布脚本或推进脚本之前被取消，SQL 之后的 Redis 步骤仍以 `WithoutCancel` 加单步上限完成，任务正常 ack，不产生 `redis_unavailable` 延后；单步超时时任务延后但不记失租（§4.6）。
- §5.3 批内合并按 save_seq；`legacy` 下照旧按 offset。
- 令牌发布脚本（§6.2）：返回 1 / 2 / -1 / 0 四种结果各一例，含「旧发布不得覆盖新版本」（取代在途 `TestPlayerCachePublicationCannotOverwriteNewerVersion` 的意图：失锁实例的发布返回 0、什么都不写）；返回 0 时 `db_ordering_lease_lost_total{reason="owner_changed"}` 加一、释放阶段不报 `not_owner`；成功时 `extendValidUntil` 生效；`-1` 时缓存与证据都未写、`deferReason=fence_evidence_conflict` 耗预算；证据只在 `dual` + class-1 + `applied`/`equal` + `player_database` 时传第三把键；版本串比较跨越 uint64 上限附近（在途 `TestSQLCommitPublicationIsMonotonicAtUint64Limit` 的意图移到这里）；按 §5.4 失锁耗预算；读路径 NX；`db_fence_cache_publish_total{path,result}` 计数正确。miniredis 的 Lua 按字节比较，本身测不出 locale 问题；脚本已不用字符串 `>`，所以不需要真 Redis 专测 strcoll。
- `db_ordering_guard_total`：`stale_no_epoch`、`epoch_override` 启动即以 0 存在，且只在 `stale` 之外额外计一次（子集口径，§5.2）。
- 资产证据消费方（`go/shared/assetop`，§6.6）：开关关闭时行为同 HEAD（读 Redis blob，不带 `require_sql_commit`）；开关打开时要求证据；自动放弃在「blob 已见、证据未见」时不放弃，在「两者都未见」或「证据未见且证据 `save_seq` ≥ 父键 `last_save_seq`」时放弃；开关打开而 `Ledger` 为 nil 时启动断言失败。
- 配置（§4.7）：用 `conf.LoadFromYamlBytes` 分别加载裸写的 `off`、带引号的 `"off"`、`legacy`、`dual`、`sql`、空值，把行为钉住（裸 `off` 类型不匹配报错；空值补成 `legacy`；`sql` 与 `"off"` 拒启）；`EpochOverride` 缺省 false。
- 指标：启动后新增的 `deferReason`（`fence_race`、`fence_evidence_conflict`）与死信 reason 序列都以 0 存在。
- 隔离级别启动探测（§4.8）：会话隔离级别不是 READ-COMMITTED 时拒启（假驱动返回 `REPEATABLE-READ`）。

需要的测试设施：`go/db/internal/dbtest/fakedriver.go` 的影响行数写死为 0，要改成可配，并能按语句返回 1062 与查询结果；现在没有任何测试调用真实的 `handleDBWriteOp`，本批补上。

**3. 真库集成**（新增，环境变量门控；工作目录 `go/db`）

- 用 **go/db 业务池自己的连接配置**（`newServiceMysqlConfig`，含 L-04 的超时与会话级锁等待）连库。proto2mysql 的真库用例强制开了 `InterpolateParams`，走的是客户端内插；go/db 不开，走服务端预处理。库的用例没有覆盖 go/db 实际的参数绑定路径 [C]。
- 夹具沿用在途 `player_row_fence_integration_test.go` 的做法：`ASSET_FENCE_TEST_DSN`（不带库名的 DSN）门控，每次建一个随机名的一次性库、结束时 DROP，不碰已有 zone / 玩家库；`ASSET_FENCE_REQUIRE_TIDB=1` 时要求后端是 TiDB。连接参数要换成 go/db 业务池自己的 `newServiceMysqlConfig`（见上一条），含 §4.8 的隔离级别。
- 场景：两个连接池、20 路并发乱序写同一玩家，最终值是最大围栏；同版本重放得到 `equal`（含空子消息往返，§4.4）；**并发首写**（两个事务同时写不存在的行：一方 inserted，另一方撞 1062 后重读再判，没有 1213 死锁）；`legacy` 写过之后围栏不变；class-2 两步；class-3 在有围栏的行上被拒；非围栏表在两种模式下照常落库；会话隔离级别探测返回 READ-COMMITTED；`db_fence_rejected_bytes_total` 按 outcome 计数。在途的两个用例（拒绝迟到与冲突写、并发串行化）按此改写，「同版本不同载荷失败」的断言删除。
- 过渡实现与 `SaveConditional` 两条路径跑同一组场景；`v0.3.0` 发布之前只跑过渡实现。
- 后端：MySQL 8.4 与 TiDB 8.5 各一遍。Codex 任务单已记负责人同意拉这两个镜像；任务单之外的下载仍先请示。TiDB 上额外记录事务模式（悲观 / 乐观）与 READ-COMMITTED 是否生效（§4.8 未核实项）。
- 通过标准：全绿，零 SKIP（含子用例：按 `--- SKIP` 计数为 0）。

**4. 迁移**（工作目录 `go/db`）

```sh
go run ./cmd/migrate -f etc/db.yaml -command plan
go run ./cmd/migrate -f etc/db.yaml -command up
```

- 库清单按 §8.1 第 2 步穷举；每个落点库加 `-storage-id <id>` 各跑一次。需要 `DB_ALLOWED_DATABASES` 放行目标库。
- 通过标准（按 G-03 最新口径，§3.4、§8.1）：每个库 plan 为空，或只含 `player_database` 六列 / `player_database_1` 两列中尚未迁移的那几条 ADD COLUMN；出现清单外的语句即停下。up 之后 `SHOW COLUMNS` 逐列核对：`player_database` 的 `profile_component`、`asset_op_ledger`、`settlement_ledger`、`fence_owner_epoch`、`fence_save_seq`、`inbox_ledger` 与 `player_database_1` 的 `fence_owner_epoch`、`fence_save_seq` 各返回 1 行。

**5. merge_zone**（工作目录 `tools/merge_zone`）：`go test ./... -count=1`，原有用例保持绿；新增用例覆盖 `invalidatePlayerCaches` 删除资产证据键（§9.3）。

**6. C++**

- 先 `pwsh -File tools/scripts/dev_tools.ps1 -Command cpp-node-stop`。
- 仓库根：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。必须 `/m:1` 串行；生成的 proto 库变了，要全量编译。`game.sln` 的 Debug|x64 配置已包含 `cross_zone_test`、`bag_test`、`currency_test`（排在 scene 等库之后），不需要也不建议再单独编 vcxproj。
- 全量运行：`pwsh tools/scripts/run_cpp_tests.ps1 -Filter cross_zone_test`、`-Filter bag_test`、`-Filter currency_test` 各一次。脚本会先同步 `zlibd.dll` / `rdkafka*.dll` 等运行时 DLL（直接运行 exe 可能因缺 DLL 以 0xC0000135 静默退出），并解析 Running / PASSED / FAILED。
- 过滤运行：`build\cpp\tests\cross_zone_test.exe --gtest_filter=<§7.5 列出的用例名，逐个写出，以冒号分隔>`。
- 新增的 `save_seq.h` 是头文件（`PlayerSaveSeqFloorComp` 若单独成头文件同理）：`cpp/libs/services/scene/CMakeLists.txt` 只登记 `.cpp`，不用改；`scene.vcxproj`（ClInclude）与 `scene.vcxproj.filters` 要登记。测试工程没有 CMakeLists。
- 合并在途实现后要确认：`PlayerOwnerEpochComp` 已无 `lastSaveSeq`（grep 无引用）；`MarshalPlayerForSave` 与 `SavePlayerToRedisImpl` 不再写子表 `fence_*`；快路径比较是 `dirty_save::IsEqual`（§0.2 第 16–21 行）。这是代码评审项，不靠编译发现。
- 通过标准：
  - 编译零错误；
  - 三个工程全量运行时 PASSED 等于 Running，`cross_zone_test` 的原有用例数不减（现 166 个）；
  - 过滤运行的「Running N tests」中 N 不小于 §7.5 新增用例数，且逐条列出的用例名都出现在 PASSED 里（gtest 的过滤器一条都没匹配到时退出码仍为 0，所以不能只看退出码）；
  - 可选：运行前核对 exe 时间戳晚于本次编译开始时间。

**7. 本地端到端**

- 按 AGENTS.md §6.2 清环境后，`Fencing.Mode=dual`、`EpochOverride=true`（本地只有新二进制）起服，跑 robot 冒烟。
- 本地 `scene_manager_service.yaml` 开着 `AllowUnsafeCrossNodeHandoff`（dev 旁路，§7.1）；单机下两个进程读同一个墙钟，不会因此出 `older_seq`。用 `false` 再跑一遍是单独的验收项，不是本步的硬性要求（§7.4）。
- 通过标准：`db_fence_write_total{class="seq"}` 的 updated / inserted 大于 0；`stale_owner`、`owner_epoch_regressed`、`legacy_after_seq` 为 0；同一玩家两张表的 `fence_save_seq` 相同且非 0；`fence_owner_epoch` 等于该玩家的 owner_epoch；Redis 父键里的 `last_save_seq` 非 0、子表缓存里的 `fence_*` 等于行内值（C++ 不写 body 围栏，值来自 go/db 盖章）；做过资产操作的玩家存在 `player:{id}:asset_sql_commit`，版本等于该玩家 `player_database` 行内围栏；`db_fence_cache_publish_total{result=~"evidence_conflict|evidence_ahead"}` 为 0。

**8. 一致性与混沌**

- `go/db/cmd/data_stress` 加 `-fenced` 开关，定义：
  - `OwnerEpoch` 每轮取一个只增不减的值，例如本轮开始时的 Unix 秒，允许用参数覆盖；`SaveSeq = seq`（每玩家 1..N，不需要与生产公式一致：合成号不会被 scene 加载，§7.2）；
  - 额外写一个期望键 `verify:expected_fence:{msgType}:{pid} = E:N`；
  - 每轮必须换 E 或换 `-player-start`，禁止跨轮复用相同或更小的 E。固定 E 又只清 Redis 时，第二轮的写全部判 `older_seq` / `equal`，verifier 读到的仍是第一轮的值，结论无效。
  - data_stress 不铸造计数器（键缺失时按 0 处理），E 必须单调递增，否则 `dual` 下旧 E 的写会被 §5.1 的探测判成倒退、误报 `DbOwnerEpochRegressed`；或者先为测试号段铸好计数器。
- `cmd/verifier` 增加围栏断言，分两种轮次：
  - 全 `dual` 轮：探针 == N，且围栏 == (E_run, N)；
  - `dual` 与旧版混跑轮：探针 == N，且围栏 ≤ (E_run, N)（旧版副本不写围栏）；同时要求至少有一部分玩家的围栏 == (E_run, N)，证明 `dual` 副本真的参与了裁决。
- go/db 开两个副本，中途强杀一个并注入重试，之后跑 `cmd/verifier`。
- 再用一个 `dual` 副本与一个旧版副本混跑一轮，**`EpochOverride=false`**（旧二进制在场，§8.3）。
- 切 `dual` 之后，data_stress 必须带 `-fenced`，或者只用围栏仍为 `(0,0)` 的独立号段；否则 class-3 写会判 `legacy_after_seq` 进死信。
- 通过标准：收敛到最大序号，0 处不一致，围栏断言按轮次通过。

**9. 压测**：按 AGENTS.md §6 全流程，与 R20 基线（交接 §4.1 第 4 项）对比。关注 `dataloader` 等 db 回调的耗时没有回归、`older_seq` 的比例、`db_fence_rejected_bytes_total`（被拒写白白下发的字节数）。v0.4 另要单独记录三件事：

- 过渡实现（§4.8）每笔写多一条整行 `SELECT … FOR UPDATE` 与事务往返的代价（`db_task_stage_seconds{stage="sql"}`、MySQL 行锁等待）；`v0.3.0` 发布、换成 `SaveConditional` 之后再测一轮对比。
- Redis step 列的口径变化（§10）：`cache_publish`、`applied_advance` 是新列，`lease_verify` / `cache_set` 含义变了，`cursor_set` / `epoch_mark` 消失，复盘表里注明，不与 R20 对应列直接比较。
- 存盘跳过率：快路径改回 `dirty_save::IsEqual`（§7.3）后应与 R20 同口径；若负责人决定在快路径剥离 `stress_test_probe`（§14 第 22 条），跳过率口径会变，复盘写明。`SavePlayerForAssetCommit` 让资产操作强制整份写，DBTask 量随资产操作频率上升，一并记录。

没有对比表不宣称性能结论。

---

## 12. 改动清单与规模

| 批 | 文件 | 说明 |
|---|---|---|
v0.4 按「合并在途实现 + 补审修订」重列。归属一列：**本线** = 落地恢复与围栏接手线；**go/db 会话** = 存盘管线接手会话的文件，需按 §0.2 合并前置协调；**资产写入方** = 「全域到账续批」写入方的文件。

| 批 | 文件 | 归属 | 说明 |
|---|---|---|---|
| A proto | `proto/db/db_task.proto`、`proto/common/database/player_cache.proto`（只改注释，号位在途已占）| 本线 | `mysql_database_table.proto` 已在 2026-10-15 改完，本批不动；§3.4 的放行清单已由 G-03 更新，本批不动。生成物 15 个（§11 第 1 步逐一列出）|
| B go/db（本线） | `internal/kafka/player_row_fence.go`（重写：围栏表判定、盖章、`fenceSave` 过渡实现、结局归类）、`key_ordered_consumer.go` 里的围栏 hunk（import、`handleDBWriteOp`、`publishDBWriteResult`、`writeBackDBCache`、`runDBOp` 的结局分支、`handleTask` 的被拒分支与成功分支，含成功分支注释同步）、`internal/kafka/player_cache_publish.go` 及测试（删除）、`player_row_fence_integration_test.go`（改写）、新增围栏单测（如 `player_row_fence_test.go`、令牌发布脚本与推进脚本的 Lua 用例）、`cmd/data_stress/main.go`（`-fenced`）、`cmd/verifier/main.go`（围栏断言）、`cmd/epoch_raise/main.go`（新，§9.1）| 本线 | 发 `v0.3.0` 后再改 `go.mod` / `go.sum`（§14 第 21 条）|
| B' go/db（协调） | `key_ordered_consumer.go` 其余部分（`runDBOp` / `handleTask` 签名、Stop 注释，§4.6）、`placement_route.go`（注释，§16）、`store_failure.go`（`deferReason` 的 `fence_race` / `fence_evidence_conflict`）、`ordering_lease.go`（令牌脚本的租约包装：计时、`extendValidUntil`、`setLost`）、`internal/locker/locker.go`（带令牌执行脚本的方法）、`internal/config/config.go` 与 `etc/db.yaml`（`Fencing.Mode` / `EpochOverride` / `OwnerEpochRedis`）、`internal/logic/pkg/proto_sql/db.go`（隔离级别与启动探测，§4.8）、`internal/metrics/metrics.go`（死信 reason、deferReason、Help）、`internal/metrics/pipeline.go`（`RedisSteps` 12 个、`orderingGuardOutcomes` 7 个、Help）、`internal/metrics/metrics_test.go`（`TestRedisStepNamesPinned`）、`internal/dbtest/fakedriver.go`、`key_ordered_consumer_test.go`、`consumer_shutdown_test.go`、`run_db_op_test.go`、`instrumentation_test.go`、`tools/scripts/stress_summarize.ps1`（2e 段）、`tools/scripts/test_stress_summarize.ps1` | go/db 会话 | step 名与指标取值是压测对比的固定契约（§10），必须同批改；这些文件 R2 / R3 正在改，等清单到达后 rebase |
| B'' 资产证据 | `go/shared/assetop/sql_commit.go` 与 `sql_commit_test.go`（收窄，§0.2 第 12、13 行）、`go/data_service/internal/logic/asset_sql_commit.go`（注释）| 本线 | — |
| B''' 资产消费方 | `go/shared/assetop/ledger_dataservice.go`、`reconcile.go`、`abandon.go` 及相关测试（按 zone 开关、放弃条件、`Ledger` 启动断言）、`go/guild/internal/svc/asset_op.go`、`go/trade/internal/svc/assetchannel.go`（注释与配置）| 资产写入方 | §6.6；合并时对齐，或由负责人改派本线 |
| C C++ | `player_lifecycle.cpp`（播种、发号、删 body 围栏写入、快路径改回）、`player_ownership_comp.h`（删 `lastSaveSeq`）、`save_seq.h`（新：`SaveSeqAllocator`、`SaveSeqFloorFrom`、`ApplyPersonalFloor`、`PlayerSaveSeqFloorComp`）、`core/system/id_generator.h`（发号器实例）、`last_persisted_snapshot_comp.h`（`Replace` 剥离）、scene 配置（容差，键名落码时定）、`scene.vcxproj` 与 `.filters`、`cross_zone_test.cpp` | 本线 | `player_lifecycle.cpp` 里 `SavePlayerForAssetCommit` 等资产 hunk 属资产写入方，保留不动 |
| D 部署与运维 | `tools/scripts/k8s_deploy.ps1`（db ConfigMap）与 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`deploy/k8s/owner-epoch-alerts.yaml`（新告警、`DbStaleOwnerWriteRejected` 描述）、`deploy/k8s/db-task-retry-alerts.yaml`（`DbTaskDeadLetterAdded` 排除围栏原因）、`tools/merge_zone/player_rows.go` 及测试（证据键失效）、回档与 Redis 丢失的 runbook（`docs/ops/merge-zone-runbook.md` §9.2、`docs/design/zone_data_rollback.md`，含证据键清理）| 本线（`k8s_deploy.ps1` 与告警文件有别的会话在改，先对齐）| — |
| E 契约与文档同步 | `go/shared/ownerepoch/ownerepoch.go` 包注释、`scene-owner-reentry-barrier.md`（计数器写入方，§9.1）、瓶颈文档（G1 / G3 / G5 覆盖口径与 §6.5 分片约束）、`shared-redis-split-by-purpose.md`（探测客户端、令牌脚本跨实例、证据键族与容量）、`guild-phase2/04-asset-channel.md`（在途那段注明依赖本文 `dual` 与 §6.6 的开关）、`AGENTS.md` §6.2（负责人已定落码时加）、`docs/PROGRESS.md`（追加一条）| 本线 | 随对应批次提交 |

**规模**：手写约 60 个（A 2、B 约 9、B' 约 20、B'' 3、B''' 约 8、C 约 9、D 约 9、E 约 7，落码前按实际清单重数），生成物 15 个，远超 AGENTS.md §10.2 的 30 文件门槛。负责人已授权实现并合并在途实现（2026-10-14、2026-10-16），超门槛这一点仍要在交付说明里单独报告。拆批建议（提交顺序，每批能独立回退）：A 与 C 不依赖协调，可先做（C 的改动只影响 C++，旧 go/db 原样保留 9 号字段，§8.4）；B、B'' 与 B' 等 §0.2 合并前置满足后一起做；B''' 与资产写入方对齐后做；D 最后。上线顺序与提交顺序无关，见 §8 与 §8.7；`legacy` 缺省让新 go/db 先上而不改变行为（§8.2）。

---

## 13. Java 版（AGENTS.md §12）

**结论：不适用（Java 内部），另有一处待核。** 依据是对 Java 仓库 `D:\luyuan\xuanming-server-mmo`（`d942f74`）的只读核对：

- 带状态的写只由持有该 epoch 的 scene 进程自己执行，中间没有 go/db 这样的独立执行实例，也没有 Kafka 或 Redis 队列承载存盘。围栏就是 UPDATE 的 WHERE（`PlayerMapper.updateStateHeld`：`WHERE player_id = ? AND owner_epoch = ? AND owner_released = 0`），与数据写在同一个事务里。本设计要挡的「另一个执行实例拿着旧快照恢复」在 Java 版没有对应物。
- 交接里「同步写库」的说法要订正：Java 是进程内有界队列加重试的异步执行，快照到 SQL 之间有排队延迟；跨 epoch 靠围栏拒，同 epoch 靠「每个实例对象至多一笔在途的在线存盘」。
- **待核**：同节点收到同一 `(玩家, epoch)` 的再次进场时会换新实例并沿用旧内存（`SceneWorld.java:1403-1424`）。旧实例在途的在线存盘可能晚于新实例的存盘提交，等值围栏放行，库回到旧值。触发条件（gate / login 何时重发进场）没有核实。这是 Java 版自己的缺口，是否修由负责人定。
- 两版的差别：本仓库是单调高水位（异步通道必须接受老主在新 epoch 铸出之后才到的最后一笔），Java 是对权威列做等值比较，没有序号列。
- 新 proto 字段经 Java 仓库的 `tools/ContractSync.java` 进入 `game-proto`（它原样同步 `proto/**`，只排除 `proto/etcd/`）。Java 全仓没有引用 `DBTask` 或这两张表的 message，加字段兼容，不改 Java 代码。三个 proto 都不是客户端可见契约（`proto/README.md:27-31`）。
- PARITY.md 第 103 行记着 proto2mysql v0.2.0 与 `game-pbmysql` 逐方法对齐；proto2mysql 发 `v0.3.0`（含 `SaveConditional`，§14 第 21 条）之后，那一行要补状态。玩家存盘不经 `PbMysql`。
- v0.3 的改动（计数器探测、原子推进、`EpochOverride`、`epoch-raise`）都在 Go 内部与运维流程，不涉及客户端契约，结论不变。
- v0.4 的改动（进程级发号器与个人下限、go/db 结局处理、令牌发布脚本取代 verify、过渡实现、隔离级别）同样都在 C++ / Go 内部，结论不变。新增的 `PlayerAssetSQLCommit`、`require_sql_commit` / `sql_committed` 不是客户端可见契约，经 `ContractSync` 原样进入 `game-proto`，不改 Java 代码。
- 资产 SQL 提交证据本身属于「全域到账续批」那条线的资产实例过户与托管功能，它的两版对齐由那条线负责（Java 仓 `PARITY.md` 已有它的在途行「完整邮箱与通用排行榜（2026-10-15 进行中）」）；本文只负责证据的写入方（go/db）与发布条件。Java 版的资产到账如何确认 SQL 已提交，由那条线在 PARITY 里写明。

**登记**：功能落码后在 Java 仓库 `PARITY.md` 追加一行，状态「不适用（Java 内部）」，备注写上面的差别、待核项与 v0.4 的证据写入方说明。设计阶段不登记。

---

## 14. 已定事项、待负责人确认与未核实

负责人 2026-10-16 授权「各待选点按最标准取推荐项」（[接手记录](../handoff/landing-fencing-takeover-20261016/README.md) §1、§3.4）。v0.3 的 20 条按原编号保留，逐条写明结论；交接 §3.4 的 13 项与本表编号的对应：1 → 12；2 → 13；3 → 14；4 → 9；5 → 15；6 → 5（并入 20 与收件箱 C4）；7 → 6；8 → 16；9 → 8；10 → 19；11 → 7；12 → 17、18；13 → 10、11（fence-decisions.md 文末「编号对照」）。

**已定**（选定内容照 README §3.4 与 fence-decisions.md 的推荐；第 12 条例外，照 README §3.4，理由在该行）：

| # | 事项 | 结论 |
|---|---|---|
| 1 | DBTask 9 号给 `save_seq` 还是 `reply_instance`（交接 §5 第 13 项）| **已定（2026-10-14）**：9 给 `save_seq`，10 给 `reply_instance`（请负责人通知全局单点那个会话）。在途实现已占 9 号，注释按 §3.1 补「10 号预留」|
| 2 | 是否改 AGENTS.md §6.2（交接 §5 第 14 项）| **已定（2026-10-14）**：条件写落码时再加；内容按 §9.1（抬计数器，不是归零），与另一会话对 §6.2 的修改合成一次 |
| 3 | Redis 分片的 hash tag（交接 §5 第 16 项）| **已定（2026-10-14）**：以按 player_id 分片为准。本批不受影响；瓶颈文档 §6.5 修订时要把 §6.4 的约束（令牌发布脚本的锁、缓存、证据三键同 tag）与 §5.5 的约束（排序锁、游标、已落库 epoch 三键同 tag）算进去 |
| 4 | 缺省模式与切换方式（§8.2、§8.3）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：缺省 `legacy`，按 1a → 1c → 1b 逐 zone 切 `dual`，再按门禁开 `EpochOverride`。在途实现没有模式开关，按本条补上 |
| 5 | 计数器抬升入口放哪（§9.1）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：独立的 `go/db/cmd/epoch_raise`；工具自己收齐全部来源（落点库围栏、`applied_epoch`、收件箱经 `PlayerInboxAdmin.GetSummary` 取 `max_fence_epoch`、资产证据里的 epoch），任一来源读不到默认拒绝；`-floor` 只作补充入口；默认只演练，`-confirm` 才写；同步改 ownerepoch 的写入方契约 |
| 6 | proto 重新生成的做法（§11 第 1 步）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：`proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`（需要时先 `proto-gen-build`）；AGENTS.md §4.1 已于 2026-10-15 订正，本文 §11 第 1 步同步。三份陈旧 `mysql_database_table.sql` 本批不碰，另开小任务决定去留 |
| 7 | epoch 铸造的持久下限（§9.1）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：铸造带墙钟下限，新 epoch = `max(当前 + 1, 当前毫秒)`；归 scene_manager / 跨 zone 归属线单独立项，不阻塞 Stage 1b，做完之前按探测加 `epoch-raise` 运行 |
| 8 | 是否让 scene_manager 在进程换代时必定铸造 epoch（§7.4）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：直接做。`PlayerLocation` 记录节点实例身份，实例变了按「持有者已换」铸造，要求正面证据并继续走再入屏障；归跨 zone 归属 / scene_manager 线；多主机环境切 `dual` 的前置，单机 dev 不要求。class-2 第二步被放行路径利用的条件（同 epoch 内生产者版本变化，§5.2）随本条关闭 |
| 9 | Stage 1b 的前置（§8.3）| **已定**：L-04 第 1 批先合入（2026-10-14）；**L-04 第 2 批死信工具（已定，2026-10-16，负责人授权按最标准取推荐项）**：做，归 go/db 存盘管线线，排在其 R2、R3 之后、本线 Stage 1b 之前；重新入队写进 ④ 之后的共享收件箱并走 WAITAOF；`cmd/retry_requeue` 不能替代 |
| 10 | Java 版同 epoch 重复进场的待核项（§13）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：另开任务，交 Java 线按 AGENTS.md §12 登记处理，不阻塞本仓 |
| 11 | `STRESS_TEST_PROBE=1` 时快路径是否失效（§7.3）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：另开只读核实任务，排在下一轮压测之前，复盘写明跳过率口径。快路径是否剥离探针是另一件事，见第 22 条 |
| 12 | `equal` 的处理（§4.4，CPP-3）| **已定（2026-10-16，负责人授权按最标准取推荐项；选定内容照接手记录 §3.4）：方案 1，按幂等生产者口径，同 `(E, s)` 视为重放，不比内容**；撞号靠 C++ 进程级发号器的墙钟下限消除；资产证据层保留「同版本不同内容即报错并计数」（§6.2 返回 -1）。**与 fence-decisions.md 第 1 项的推荐（方案 3：读回整行比内容，不同即判冲突）不同**，理由：① 那条推荐的前提是在途实现的每实体计数器会撞号（崩溃重启后同 node_id 的新进程从落后的父键播种），换成进程级发号器之后撞号只剩分钟级的时钟回拨与单机 dev 旁路（§7.2、§7.4）；② 读回比对在 proto2mysql 的空子消息往返上必然误判（置位的空子消息写成空串、读回为未置位，C++ 总是置位全部子消息），会把失租、缓存或证据发布失败后的正常重试全部变成「同版本冲突」并耗尽预算进死信（fence-inflight.md F2），按逐列口径比对也要另读整行；③ 推荐方案 3 的另一个理由「资产证据不能发布与 SQL 不符的内容」由证据层的冲突检测满足，证据是后果最重的那条路径。方案 2（令牌脚本里删缓存键）不采用：每次 `equal` 多一次冷读，且管不到证据 |
| 13 | `Observe` 超界玩家的处置与容差（§7.2，CPP-2）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：方案 2，超界值只作该玩家实体上的个人下限，发号取 `max(全局 Next, 个人下限 + 1)`；容差 60 秒，写进 scene 配置；加 scene 主机间时钟偏差监控，告警阈值 1 秒级 |
| 14 | 失租是否豁免重试预算（§5.4）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：不豁免，订正理由（失租也可能发生在 SQL 之前，进死信的条目不一定已落库，死信工具是必要配套）；以后与 L-04 §4.3 的「Redis 侧环境故障」整族一起调整，不单独给失租开例外 |
| 15 | 冷登录与在途写的竞态（§6.2 残余窗口 3）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：单独立项，采用「go/db 维护待落写标记 + 读任务快速失败」，归 go/db 存盘管线线，排在 ④ 之后（标记要和 ready 的入队出队在同一段 Lua 里维护）；进了死信的写不清标记（fail-closed），由死信工具处置完再清。过渡期 runbook 写明禁止手工删 `PlayerAllData` 父键。不阻塞本批 |
| 16 | `DbTaskDeadLetterAdded` 与条件写 critical 告警重复（§10）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：在它的表达式里排除 `fence_stale_owner`、`fence_legacy_after_seq`、`owner_epoch_regressed`；先与 go/db 存盘管线线（R5 在改同一文件）对齐再同批改 |
| 17 | 死信按 (玩家, 表) 只保留围栏最大的一条（§9.4）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：不做。死信是证据库，§9.1「定真源」要看倒退期间写了什么；还要换数据结构并改合服预检与死信长度采样；有 critical 告警与按小时处置兜着（YAGNI） |
| 18 | class-2 第二步回填缓存围栏（§4.3）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：不做。class-2 只在 C++ 回滚窗口出现，缓存围栏本来就定义为下限 |
| 19 | 计数器探测在 G4a 拆分后的 Redis 客户端（§5.1）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：go/db 新增只读客户端 `OwnerEpochRedis`，`epoch_raise` 共用；在 G4a 拆分稿登记两条依赖：探测客户端，以及 §6.2 令牌发布脚本会被拆到两个实例（§6.4）；G4a 第 2 批要等后者解决才能切。资产证据键的读写两侧（go/db 写、data_service 读）同样登记；拆分稿的回档 runbook 引用 §9.1 |
| 20 | `epoch-raise` 的余量 K 与「先排空」二选一（§9.1 第 c 步）| **已定（2026-10-16，负责人授权按最标准取推荐项）**：常数 K（建议 10^6 量级），不要求先排空；第 7 条落地后 K 可取当前毫秒 |
| 23 | 资产 SQL 提交证据是否作为资产终结与自动放弃的硬前提，以及 `dual` 之前的退化（fence-inflight.md 新问题，§6.6）| **已定（2026-10-16，README §3.4 资产通道）**：按 zone 开关，缺省关、关时行为同 HEAD；打开条件是该 zone 已 `dual` 且已上新 C++；自动放弃要求 Redis blob 与 SQL 证据都「未见」，或证据版本 ≥ 父键 `last_save_seq`；上线与回退顺序见 §8.7 |

> **2026-10-18 更正**：第 1 条的 10 号已由 `reply_instance` 使用，不再是预留，见 §3.1 同日更正。

**仍需负责人亲自确认**（只剩这两项）：

| # | 事项 | 本线的建议 |
|---|---|---|
| 21 | proto2mysql `v0.3.0` 发版（§4.8、文首「库原语」）| 合并 `feat/conditional-save`（`e81a1e7`）、打 `v0.3.0` 标签并推送，再升 `go/db/go.mod` 两侧（Claude 不 push、不打 tag）。这改变了 2026-10-14「只合 main、不打 tag、用伪版本钉提交」的决定：go.mod 现用 replace 指向 GitHub 上的同名 tag，正式 tag 与现有写法一致，也是 Go 模块的标准做法。发版之前用过渡实现，不阻塞落码 |
| 22 | 快路径是否剥离 `stress_test_probe`（§7.3）| 本批先保持 HEAD 的快路径比较，不剥离；若决定剥离，做法以第 11 条的核实结果为准、不得在快路径上每次深拷贝，并在下一轮压测复盘写明跳过率口径变化。这一项影响压测口径，所以要负责人亲自定 |

**未核实**：

- Drift 实际输出的 DDL 文本（只按源码推断）。
- go-sql-driver v1.9.3 的 `NewConfig()` 默认值；uint64 参数在服务端预处理路径上的绑定（§11 第 3 步用真库用例覆盖）。
- TiDB 上 autocommit 的乐观事务对 `SaveConditional` 的影响：9007 / 8002 / 1213 已由 L-04 归入 `store_busy`（写不耗预算，延后重试）；仍建议玩家库所在集群用悲观事务。TiDB 上 READ-COMMITTED 是否生效、`SELECT … FOR UPDATE` 对不存在的键如何加锁（§4.8，§11 第 3 步记录）。
- 过渡实现的性能代价（§4.8，待压测）。
- scene 主机的时钟同步方式、历史最大回拨量与主机间偏差。
- `data_service` 回档写父键的路径是否只写 Redis。
- librdkafka 幂等生产者在本仓库版本下的保序行为（子模块未检出）。
- 「同一 node_id 的新进程走不铸造分支」在真实部署下是否可达（§7.4 条件 2）：按阅读推断可达，未实测；§14 第 8 条已定直接做。
- `AllowUnsafeCrossNodeHandoff=false` 下的标记链路：yaml 注明尚未编译、未跑过测试。
- go-zero 对裸 `off` 的解析行为只读了 module cache 里的源码，没有运行（§4.7）。
- R2（④ + WAITAOF）落地后，推进脚本、LREM / Kafka ack 与 WAITAOF 的先后；④ 的 p* 键名与 `db_retry_queue_length` 的 label（§5.5、§8.3）。
- 重试源任务在 Stop 第⑤步被归还时 RetryCount 是否回退（影响 §4.6「耗预算」的程度）。
- 启用 Regions 集群或 G4a 拆分后，资产证据键的读写两侧是否仍在同一实例（§6.6）。
- 「全域到账续批」写入方是否已停手：截至 2026-10-15 23:05 仍在改；在途代码在分析期间被 gofmt 重排过，§0.2 的行号引用前重新定位。
- `SaveConditional` 尚未经 Codex 验证、尚未发版；本设计的全部改动未落码、未编译、未测试。

---

## 15. 考虑过而不采用的方案

| 方案 | 不采用的原因 |
|---|---|
| 把 save_seq 放进 body 的围栏字段，不加 DBTask 字段 | go/db 的守卫在解 body 之前就要用到序号；围栏列会多出一个写入方，body 与信封不一致时无从裁决 |
| 每玩家一个 `PlayerSaveSeqComp` | 实体重建后归零，只能靠父键播种，而父键不是已发出序号的上界 |
| 只用墙钟发号 | 时钟回拨即静默丢盘 |
| 用 Kafka offset 当序号 | 游标按分区记，换代后 offset 重置，读写本来就不在同一分区 |
| 多行合并写、`INSERT … ON DUPLICATE KEY UPDATE` 加 `IF` | 分不出哪一行被拒；两列围栏依赖 SET 的赋值顺序；proto2mysql 已提供等价且更清楚的原语 |
| 独立的 `:fence` 缓存键 | 两键必须同生同灭；分片后要求同槽 |
| 在 Lua 里解析 protobuf 取围栏 | 依赖字段编码顺序，脆弱 |
| 写后删缓存，由读路径重建 | 每次存盘后的第一次登录都变成冷读；读路径的回写同样会写入旧值 |
| `dual` 下对所有带 epoch 的写放开游标（§5.2）| v0.2 的理由是「class-2 没有可比的版本」；v0.3 起放行的安全性来自 §5.5 的原子推进，class-2 的 advance 放行在它之下同样成立。本批仍只开 class-1，理由改为控制范围 |
| `stale_owner` 直接 ack 丢弃 | epoch 计数器倒退时会把全部存盘静默丢掉 |
| 复用 `db:capability:zone` 做 `sql` 模式门禁 | 每 zone 一把键，merge_zone 按精确相等判定 |
| §5.2 把 stale + match 也交给 SQL（状态机 F1 评审改法 b）| 依赖围栏精确（必须全 zone 都是 `dual`），并且会把重平衡时成批的 stale + match 重投都送进 MySQL。将来若要做，只对 `fromRetry` 的任务开放 |
| 只把 `markAppliedOwnerEpoch` 失败改成延后（事实 F5 评审改法）| 失败时游标已经 SET 到本任务的 seq，延后之后的重试会被游标判 stale 丢掉，epoch 永远补不上。改用 §5.5 的原子脚本（或「先 epoch 后游标」的过渡写法）|
| `dual` 下 Redis 守卫判 stale 一律进死信（状态机 F3 评审改法 1）| 真僵尸与迟到的退出存盘（K1）是这道守卫的设计对象，全部进死信会把整行载荷堆进 noeviction 的 Redis，告警成为噪声，而且仍区分不出倒退。改用计数器探测（§5.1）|
| 计数器倒退后「围栏归零 + 回灌死信」（v0.2 §9.1 处置 2、3）| 恢复时玩家离线的情形下，归零后回灌会用备份快照派生的数据覆盖 MySQL 里更新的数据；回灌的条目 RetryCount 已满会立刻再进死信。改为定真源、抬计数器、不归零、不回灌 |
| `equal` 不发布缓存（CPP-3 评审改法二）| 失租恢复依赖 `equal` 补发缓存；SQL 已落而缓存没发成时缓存键不是缺失而是留着上一版的值（TTL 3600 秒），读路径的 NX 补不了，违反 F-4 |
| 发号低位放 node_id 防跨进程撞号（CPP-3 评审改法一）| 计数模式下 `last + 1` 会改掉低位的进程身份；C++ 的 node_id 是 17 位，毫秒 41 位加计数 10 位后只剩 13 位；§7.4 的场景恰恰是同一个 node_id 的两代进程，用 node_id 区分不开。若确实要降低撞号，低位放启动时取的随机数、增量按计数字段步进，也只能把概率降到 2^-k，不是保证 |
| data_stress `-fenced` 的 SaveSeq 用与生产相同的公式（CPP-2 评审改法）| 合成 player_id 不经 scene 加载，进不了 `Observe`；每玩家 1..N 就能满足「收敛到最大序号」（§11 第 8 步）|
| 读路径 NX 失败后改回缓存现值（状态机 F4 评审改法 a）| SQL 已落而缓存未发布的窗口里，SELECT 读到 v3、缓存是 v2，回 v2 等于新增一条回档路径。若一定要收窄那个毫秒级交错，只能在 NX 失败后 GET 缓存、按围栏字典序取较大者，相等或解析失败时取 SELECT（缓存围栏只是下限，§6.3）；收益太小，不做 |
| 读路径「先 GET、解析失败再覆盖」修坏值（状态机 F6 评审改法）| 写成 GET → 判断 → SET 三步时，其间写路径发布的新值会被读到的旧行覆盖，等于把 G4 放回来；「长度为 0」也不算解析失败（空字节能解出空消息）。可选的自愈写法是 CAS：NX 失败后 go/db 尝试解析现值，解析失败才 EVAL「`GET(k) == 观察到的字节` 时 SET」，默认不做 |
| 同一任务连续 N 次 Race 后转普通失败（状态机 F8 评审改法）| 需要跨重试持久的计数，而豁免路径会把 RetryCount 减回去；新增计数就要改载荷或 proto，L-04 已否掉改载荷格式。改为 Race 直接耗预算（§4.4）|
| 条件写死信另开独立键（事实 F2 讨论）| 合服预检 `tools/merge_zone/preflight.go` 和 `db_retry_queue_length{queue="dead"}` 都只看 `kafka:dead:queue:{topic}`，独立键会绕过两者。改为同一列表、外套带原因的信封（§8.3）|
| 两轮压测之间清 MySQL 或归零围栏（事实 F4 评审改法 3）| 可作备选，但不如每轮换 E 简单（§11 第 8 步）|
| 单独编 `cross_zone_test.vcxproj`（事实 F3 评审改法 1）| `game.sln` 的 Debug / x64 构建配置已包含它，排在所有库之后；评审「旧产物上假绿」的前提不成立 |
| 模式名沿用 `off`，靠纪律写成 `"off"`（事实 F6）| go-zero 的 YAML 1.1 解析会把裸 `off` 变成 bool，只能靠纪律防住；改名 `legacy` 由机器防住（§4.7）|
| 被 SQL 拒绝的写当成功处理：ack 并推进游标与已落库 epoch（在途实现）| 违反 F-2 / F-3：class-2、class-3 与计数器倒退后的写被静默丢弃；推进游标还会让同分区 offset 更小、版本更新的重试被游标丢掉（fence-inflight.md F1）。改为结局分支（§4.4、§4.5）|
| 被拒时改为发布 SQL 当前行（在途实现）| 想解决「迟到 offset 越过此前失败的缓存补发」，但这个问题来自「被拒写推进游标」本身；被拒写不推进游标之后，发布失败的那笔写会以 `equal` 重试补发。发布库内行还让被拒的写产生了副作用（§6.2）|
| `equal` 时读回整行用 `proto.Equal` 比内容（在途实现；fence-decisions.md 第 1 项推荐的方案 3）| 空子消息往返必然误判，把失租与发布失败的正常恢复变成死信；多读一整行。撞号由发号器消除、由证据层检测（§4.4、§14 第 12 条）|
| 每实体存盘计数器 `PlayerOwnerEpochComp.lastSaveSeq`（在途实现）| 同本表「每玩家一个 `PlayerSaveSeqComp`」：父键不是已发出序号的上界，崩溃重启后序号复用或倒退；还把存盘序号塞进归属组件，违反单一职责（fence-inflight.md F3、F12）|
| C++ 把围栏写进子表 body（在途实现）| 围栏列多出一个写入方；旧版 go/db 的 `Save` 会把 body 里的值无条件写进行，`epoch == 0` 分支写出 `(0, s>0)`（§3.2、fence-inflight.md F10）|
| 快路径改用 `IsPersistedPayloadCurrent` 比较（在途实现）| 每次存盘尝试深拷两份 `PlayerAllData`，并顺带改变压测探针口径；在 `Replace` 里剥离一次即可（§7.3）|
| 写缓存前 GET → 比围栏 → 按现值 CAS（在途 `player_cache_publish.go`）| 多一次 GET 与最多三轮重试；不核令牌，失租的僵尸实例仍能发布；同版本还要 `proto.Equal`。令牌发布脚本一次往返、原子（§6.2）|
| 资产证据单独一次 EVAL 发布，Lua 字符串 `>` 比较版本（在途 `PublishSQLCommit`）| 多一次往返；`>` 依赖 strcoll / locale；证据版本更高时返回 0 静默。并入令牌发布脚本，按字节比较，返回值逐一计数（§6.2）|
| 写 SQL 的 ctx 随停机取消（v0.3 §4.6）| 与 G1 治标冲突：取消后锁立即释放而语句可能仍提交，`legacy` 下就是回档（§4.6）|
| 过渡实现无行时沿用 `Save`（在途实现）| `Save` 的 UPDATE → INSERT 链在 READ-COMMITTED 下让后到的并发首写无条件覆盖，在 REPEATABLE-READ 下每次并发首写都 1213 死锁。改为 `Insert` 加 1062 重读再判、固定 READ-COMMITTED（§4.8）|
| proto2mysql 只合 main、用伪版本钉提交（v0.3 文首）| 伪版本也能用，但正式 tag 是 Go 模块的标准做法，且与 go.mod 现有写法（replace 指向 GitHub 上的同名 tag `v0.2.0`）一致，Java 仓 PARITY 也按版本号对齐。改为发 `v0.3.0`，待负责人确认（§14 第 21 条）|

---

## 16. 交接要求对照

交接 §4.1「条件写设计必须覆盖」逐条对应的章节：

| 要求 | 章节 |
|---|---|
| DBTask 加 `save_seq`，字段号冲突 | §3.1、§14 第 1 条 |
| 两张玩家表加围栏列、声明在末尾、确认未被占用 | §3.2 |
| 帮会二期放行清单 | §3.4 |
| B1：stale_owner 进死信并告警；只清 Redis 的流程；AGENTS.md；长期方案 | §4.4、§5.1（计数器探测）、§9.1（定真源、抬计数器）、§10、§14 第 2、7 条 |
| B2：save_seq 初值不能只靠墙钟 | §7 |
| B3：class-3 只在围栏为 `(0,0)` 时写 | §4.3；切 `dual` 的 class-3 前置见 §8.3 第 5 条 |
| 判 older 之后先复核再 ack | §4.4、§4.5（v0.4 另加核锁，且不推进游标与已落库 epoch）|
| `off`（v0.3 改名 `legacy`）模式不能调 Save | §4.3、§4.7 |
| Stage 2 要有机器门禁 | §8.5 |
| PITR 回档后先清积压 | §9.2 |
| go/db 映射与 `SaveConditional` 语义；先判 epoch 再判 seq（G3）| §4.3、§4.4、§4.8（v0.4：发版前的过渡实现）、§5.2、§5.5（G3 部分修复，成立条件见 §1.2 F-1、F-5）|
| 围栏缓存：不另开键、优先值封装、与分片方案的关系 | §6 |
| MySQL 读写超时 | §4.6（连接级超时已由 L-04 落地；写 SQL 用 G1 治标的租约 ctx，停机不取消写 SQL，v0.4 只给 SQL 之后的 Redis 步骤加 `WithoutCancel` 与单步上限）|
| 构建：proto 重生、C++ 串行、工程文件登记、不手改生成物 | §11、§12（v0.4：proto 生成用 no-unity 配置，只核对 DBTask 9 / PlayerAllData 7 相关生成物）|
| Java | §13 |
| 注释与文档同步（附录「复核要求的修正」中的注释同步清单）| 本批订正 `placement_route.go` 里「整行 REPLACE，重复落地幂等」那句注释（它本来就不准：Save 是 UPDATE → INSERT），改成「`dual` 下按围栏条件写裁决，重复落地得到 equal / older_seq」；`relocate.go` 文件头「按 per-key 游标全序落到 T」补「`dual` 下另有围栏裁决」。claim drain、`subShardIndex`、`placement_route.go`「选库结果必须在持锁期间用掉」、relocate R2、merge-zone-runbook R2 段在 `dual` 下仍成立（排序锁保留），标为随 `sql` 模式那批改 |
| 被拒写的字节数（附录 M2）| §10 `db_fence_rejected_bytes_total{outcome}`；§11 第 9 步列为对比项 |
| G5：缺键放行 | §5.6。class-1 由围栏兜住；class-2 / class-3 在 `dual` 下仍开着；残余写进 §8.5。瓶颈文档把条件写登记为覆盖「G1 / G3 / G5」，要同步订正为「G1；G3 部分；G5 只关 class-1」 |

---

## 17. 修订历史

- v0.1（2026-10-13）：初稿。
- v0.2（2026-10-13）：按附录复核意见修订。文首当时写的「已过一轮对抗评审」不准确：评审结论在 v0.2 里没有并入。
- v0.3（2026-10-14）：并入对抗评审结论。三组评审（条件写-状态机正确性 9 条、条件写-事实与完整性 10 条、条件写-C++ 发号 5 条）共 24 条，逐条按工作区代码（含未提交的 L-04 第 1 批）核实后处置，见下表；同时并入负责人 2026-10-14 已定的事项（DBTask 9 / 10 号、Redis 按 player_id 分片、AGENTS.md §6.2 落码时再加、proto2mysql 只合 main 用伪版本钉提交）与实现归属。主要变化：模式 `off` 改名 `legacy`；新增 `Fencing.EpochOverride` 与 §5.5 原子推进、§5.6 G5 处置、§6.5；§5.1 计数器探测；§9.1 处置改为抬计数器；分期改为 1a → 1c → 1b。**仍未落码、未编译、未测试。**
- v0.4（2026-10-16，Claude）：v0.3 补审 + 在途实现合并。路线以 v0.3 为准，把「全域到账续批」写入方的在途围栏实现收敛过来（负责人拍板「合并并继续，按最标准的做」），保留在途号位 `DBTask.save_seq = 9`、`PlayerAllData.last_save_seq = 7` 与资产 SQL 提交证据这一消费方。并入的意见来源：[fence-v03-review.md](../handoff/landing-fencing-takeover-20261016/fence-v03-review.md)（F1–F11，fix_plan 第二节 1–9 条全部采纳）、[fence-inflight.md](../handoff/landing-fencing-takeover-20261016/fence-inflight.md)（F1–F12 与差异表、合并建议）、[fence-decisions.md](../handoff/landing-fencing-takeover-20261016/fence-decisions.md)（13 项备忘，第 1 项不按其推荐）、[接手记录](../handoff/landing-fencing-takeover-20261016/README.md) §3.4 的选定做法、go/db 存盘管线接手会话的 R1 三条意见与 `runDBOp` 定稿顺序。主要变化：新增 §0.2（在途实现逐条去留）、§4.8（原语接缝与过渡实现、隔离级别）、§6.6（资产证据）、§8.7（证据上线与回退顺序）；§4.4 / §4.5 按定稿的 `runDBOp` 顺序重写结局处理；§4.6 删去「写 SQL 随停机取消」；§5.5 脚本改用 `luaDecimalGreaterFn`、先校验后写；§6.2 令牌发布脚本取代 verify 并并入证据；§5.2 / §10 扩展现有指标、定新 step 名；§3.4 / §8.1 / §11 改按 G-03 最新口径；§7 定个人下限与 60 秒容差、删在途每实体计数器、C++ 不写 body 围栏；§14 定稿，只剩两项待负责人确认。逐条见 §17.3。**仍未落码、未编译、未测试。**

### 17.1 评审意见处置表

核实结论与严重度以核实结果为准（不是原评审的定级）。「不采纳」的理由同时登记在 §15。

| 意见编号 | 核实结论 | 严重度 | v0.3 的处理 |
|---|---|---|---|
| 状态机 F1（override 后游标不动，同 epoch 较新重试被丢）| 成立 | 重要 | §5.2 采用评审改法 (a) 并补全：stale + first 照旧丢弃、计 `stale_no_epoch`；override 落库后用 §5.5 的脚本原子改写游标（允许后退）并推进 epoch，脚本失败即延后；写明后退安全的理由与 class-3 残余；第 2 行理由改写。§4.4 游标列分正常 / override 两路；§5.4 失租后 `equal` 走同一脚本；§11 第 2 步加四个用例；§1.2 F-1、§0.1 第 5 行改为「部分修复」并写成立条件。改法 (b) 不采纳（§15）|
| 状态机 F2（游标与 epoch 非原子推进，放行越过落后的围栏）| 成立 | 重要 | 新增 §5.5：新二进制不分模式用一条脚本核令牌、推进游标、epoch 取 max，失败即延后，写明不变量。§5.2 删除「class-1 围栏精确」的论证，放行安全性改由 §5.5 支撑。新增子开关 `Fencing.EpochOverride`（§2、§4.7），开启门禁写在 §8.3（无旧版 go/db、旧实例下线后重试排空过一次）。§8.4 第 2、5 行与 §8.6 回退改写；§9.4 回灌受同一门禁；§11 第 2 步加混跑对照，第 8 步混跑轮 `EpochOverride=false`；§1.2 F-5 补成立条件。三键同 hash tag 并入 §14 第 3 条 |
| 状态机 F3（计数器倒退而 applied_epoch 未倒退时，写在 Redis 守卫被 ack 丢弃）| 成立 | 重要 | 与事实 F1 合成一处：§5.1 `dual` 下计数器探测（判据与正确性理由、读失败延后耗预算、`legacy` 不探测、对 G4a 拆分的依赖）；§4.4 SQL 处同样探测，新增结局 `owner_epoch_regressed`；§9.1 机制段与触发表改写（加「是否同步倒退 / 在哪被拒 / 载荷去向」三列，拆行，加 G4a 行）；§10 加 `regressed`、`DbOwnerEpochRegressed`、三个死信 reason；§0 第 3 条、§1.2 F-3 按两种拒绝位置分写；§11 加三条用例。评审改法 1 不采纳（§15）。修正评审两处：丢弃有日志、计数与 warning 告警，并非无声；「两个 Redis 不是同一实例」今天不成立，G4a 拆分后成立 |
| 状态机 F4（读路径 NX 后回给 login 的仍是 SELECT 的行）| 部分成立 | 次要 | §6.1 G4 措辞改为「缓存被旧值覆盖」，删去暗示本节解决回档固化的表述；§6.2 加「NX 只决定缓存键，`TaskResult.Data` 是 SELECT 的行」；§6.2 残余窗口 3 与 §1.3 登记「冷登录与在途写的竞态」（现象、入口、L-04 之后的窗口）；§14 第 15 条登记修法方向（按 key 的待落写标记，不是 ①）；§10 publish 计数加 `path`；§6.5 汇总。改法 (a) 不采纳（会新增回档路径，§15）。修正评审：父键缺失且 W 在 ready 现实中只能来自手工删父键；「G4 没有关掉」说重了 |
| 状态机 F5（切 `dual` 改变 class-3 语义却没有门禁）| 成立（触发面比评审窄）| 次要 | §8.3 第 5 条加 class-3 前置（Stage 1a 观察窗内 `legacy_zero` 与 `owner_epoch_unknown` 不增长，gate / scene_manager 为 CZ-4 之后版本，非 0 先归因、不写成硬性恒 0）；§8.2 加观察项；§8.4 矩阵加一行；§8.6 写明先回退 `legacy`；§2 补 class-3 的实际来源；§11 第 8 步要求切 `dual` 后 data_stress 带 `-fenced` 或用独立号段 |
| 状态机 F6（缓存坏值时 NX 修不了）| 成立 | 次要 | §6.2 读路径第 1 条改写（缺失、GET 出错、解析失败都会发读任务；`TriggerPlayerDataPreload` 无调用方）；残余窗口 2 写明 NX 不修坏值及代价；§6.4 补值封装的上线顺序；§10 read 路径计 `exists`。评审给的 GET → 判断 → SET 改法不采纳，CAS 自愈记为可选（§15）。修正评审：「每次登录都重复」偏重，login 随即写回父键 |
| 状态机 F7（非围栏表上 `PreserveFields` / `FenceFields` 必然失败）| 成立 | 次要 | §4.1 新增围栏表判定（启动时按描述符判定并缓存、两张玩家表缺字段即拒启、非围栏表照旧 `Save` 并计 `unfenced`）；§4.2、§4.3、§4.4、§5.3 限定围栏表；§5.2 放开条件加「围栏表」（补评审漏掉的无条件覆盖风险）；§3.2 加新表纪律；§11 第 2 步加三个用例 |
| 状态机 F8（Race 无上限空转且无告警）| 成立 | 次要 | §4.4 Race 改为延后、耗预算、`deferReason=fence_race`，写明理由（持锁下只可能来自围栏倒退或判定不一致）；`SaveConditional` 的错误继续套 `storeerr.Tag`；§10 加 `DbFenceRace`；§5.4 改为复用 L-04 的哨兵；§14 TiDB 未核实项改为「已由 L-04 归入 store_busy」。「连续 N 次 Race 转普通失败」不采纳（§15）。修正评审：TiDB 写冲突不表现为 Race；旧版与 `legacy`（原 `off`）实例同样取排序锁，真正会并发的只有失租后的僵尸实例和外部工具 |
| 状态机 F9（缓存里的围栏低于行内围栏）| 成立 | 次要 | §6.3 改为「缓存围栏是行内围栏的下限」并列出三种落后情形（补评审漏掉的「SQL 已落、缓存未发布」）；§7.2 两路来源表冷路径一行改写；§7.4 条件 3 改写；§8.6 补 `legacy` 期间播种只剩墙钟；class-2 第二步回填列为可选（§4.3、§14 第 18 条）|
| 事实 F1（§9.1 处置「归零加回灌」会用旧数据覆盖新数据）| 成立 | 重要 | 与状态机 F3 合成一处：§9.1 处置改为「定真源、抬计数器、不归零、不回灌」（踢下线、余量 K 或先排空二选一、缓存按真源处理、倒退期死信作废）；入口从 `fence-reset` 改为 `epoch-raise`，并同步 ownerepoch 的写入方契约；写明死信增长与 critical 级别；§9.3 改用抬计数器；长期段点明探测只是检测手段；§14 第 5、20 条。修正评审：序列 B 中围栏本身比今天好，丢数据的是 v0.2 的处置步骤；「不需要排空前置」说过头，改为余量或排空二选一 |
| 事实 F2（死信工具、分期顺序、指标端口、Redis 内存）| 成立（(c) 部分已过时）| 重要 | §8.3 前置改写：L-04 第 2 批死信工具落地并演练；条件写死信用同一列表加带原因的信封；指标端口 `:9160` 已配，ServiceMonitor 与告警 apply 改为演练留证据；分期改为 1a → 1c → 1b，加金丝雀通过标准。§9.4 删除「手工 Redis 命令」，引用 L-04 §3.8 的禁令，写增长上限与处置，可选按 (玩家, 表) 去重（§14 第 17 条）。§14 第 9 条改为「1b 等第 2 批工具」。修正评审：`stale_owner` 在 class-2 上也会出现 |
| 事实 F3（cross_zone_test 不在 game.sln）| 部分成立（核心前提不成立）| 次要 | `game.sln` 已含 `cross_zone_test`，不需要单独编 vcxproj（§15）。§11 第 6 步改为经 `run_cpp_tests.ps1` 全量运行（同步 DLL），再按逐个写出的用例名过滤运行；通过标准按 Running / PASSED 计数与用例名核对（过滤器零匹配时退出码仍为 0）|
| 事实 F4（data_stress 两轮复用同一 (E, s)，混跑结论为空）| 成立（「两轮内容不同」一句不成立）| 次要 | §11 第 8 步定义 `-fenced`（E 每轮单调、`SaveSeq = seq`、期望键），verifier 按「全 `dual` 轮」与「混跑轮」两种断言；每轮必须换 E 或换号段；§9.1 补压测提醒。评审改法 2 的「围栏等于本轮」在混跑轮会误报，改为 ≤ 加至少一部分相等；改法 3 作备选（§15）|
| 事实 F5（`legacy`（原 `off`）/ 旧实例 + epoch 推进失败 + 放行 = 回退）| 部分成立 | 重要（原评次要，上调）| 与状态机 F2 合并：§5.5 原子脚本，写明「先 epoch 后游标、任一失败即延后」的最小替代，以及「只把 `markAppliedOwnerEpoch` 失败改成延后」为何无效（§15）；§8.4、§1.2 F-5 写明成立条件；class-2 第二步在 §5.2 只写一句、跟随 §14 第 8 条。修正评审：分区再平衡不是必要条件（重试队列各实例共用） |
| 事实 F6（`Mode: off` 被 YAML 解析成布尔；off 并非唯一的行为变化）| 成立 | 次要 | §4.7 模式改名 `legacy` / `dual`，附 go-zero v1.10.0 用 yaml.v2 的证据，options 约束允许空值、空值补 `legacy`；§8.2 逐条列出 `legacy` 下生效与只在 `dual` 生效的改动；§5.3 写明只在 `dual`；§11 第 2 步加 `LoadFromYamlBytes` 配置用例，k8s 契约测试断言模板取值；§8.6 用新取值名；§4.6 的现状改为 L-04 已落地连接级超时 |
| 事实 F7（「过期成员也不自动摘除」与代码相反）| 成立 | 次要 | §8.5 改写：成员由 `retryReclaimScript` 在搬空收据后摘除；成员资格不等于正在落库，「所有成员都具备能力」会 fail-open；补门禁契约「登记失效期间不消费、不落库」。§0.1 第 7 行不改 |
| 事实 F8（漏迁的落点库不拒启）| 成立（L-04 之后不再静默）| 次要 | §3.2 把「先 DDL 后代码由机器保证」限定为本 zone 库；§8.1 第 2 步加库清单穷举并作为 Stage 1a 门禁；§8.2 加 `store_open_failed` 与 schema gate 日志观察；§3.5 写明 `storage_audit.go` 不能用来穷举 |
| 事实 F9（附录的注释同步、被拒字节数、G5 未落实）| 成立 | 次要 | §16 补三行；§10 加 `db_fence_rejected_bytes_total{outcome}`，§11 第 9 步列为对比项；新增 §5.6 G5 的处置（评审建议的 §5.5 编号已给原子推进，顺延为 §5.6）。与 §5.5 合并后游标写入进推进脚本，但仍不做 Lua 取最大值；瓶颈文档的「G1 / G3 / G5」口径待同步 |
| 事实 F10（生成物数量）| 成立 | 次要 | §0 第 7 条与 §12 改为「生成物 22 个、手写约 24 个、合计约 46 个；A 批 31 个，已超 30 文件门槛」，保留「无论如何拆都要授权」；§11 第 1 步逐一列出 22 个路径，写明 `db_task.grpc.pb.{cc,h}` 不应变化、三份陈旧的 `mysql_database_table.sql` 的处理，交 §14 第 6 条确认 |
| CPP-1（§7.4 残余风险条件写窄）| 部分成立 | 次要（原评重要，下调）| §7.4 改写：条件 3 不再把冷路径当安全侧；条件 4 改为「新进程墙钟早于旧进程最后一次发号」，并写明 etcd 租约 180 秒带来的分钟级量级（评审「3 秒后新 Pod 拿到同一 node_id」不可达）；补 dev 旁路；后果补「最后一笔被丢时 MySQL 停在旧值」；缓解补主机间时钟偏差监控；§14 第 8 条写明可达时是多主机切 `dual` 的前置；§7.1 补 node_id 与旁路事实。「用旁路 false 再跑一遍」不作 §11 第 7 步的硬性要求，作为单独验收项 |
| CPP-2（`Observe` 无上界、`Next` 无溢出处理）| 部分成立 | 次要 | §0.1 第 2 行、§7.2 删除「没有坏处」并写明代价；`Next` 饱和显式失败；`Observe` 加上界与容差；超界处置二选一列入 §14 第 13 条（建议个人下限）；§7.5 加两条单测；§9.1、§9.3 补 seq 超前的处置（抬计数器）。data_stress 不规定 SaveSeq 公式（来源不成立，§15）|
| CPP-3（`equal` 的「同内容」前提只在进程内成立）| 部分成立 | 次要 | §4.4 `equal` 一段如实写明 (E, s) 的唯一性只在单进程内由 §7 保证，跨进程同 epoch（dev 旁路、node_id 复用）可能撞号，撞号时缓存与 MySQL 可能不同；v0.3 维持补发并由 verifier 事后核对，「令牌脚本里删缓存键」列为 §14 第 12 条。「`equal` 不发布缓存」与「低位放 node_id」两个改法都不采纳（§15）|
| CPP-4（§11 第 6 步的过滤器跑不到回归用例）| 成立 | 次要 | §11 第 6 步改为 `cross_zone_test` 全量运行（现 166 个用例）并加跑 `bag_test`、`currency_test`；通过标准改为「原有用例数不减且全绿，新增用例全绿」；§7.5 写明 `Replace` 被这两个工程直接调用 |
| CPP-5（两条单测在宿主里写不出来）| 部分成立 | 次要 | §7.2 写明发号器是普通值类型，生产实例放在 `tlsIdGeneratorManager`、不受 `tlsEcs.Clear()` 影响（待落码核对），单测用局部实例、不需要复位入口；播种下限做成纯函数 `SaveSeqFloorFrom`，`InitPlayerFromAllData` 那一行无单测、由代码评审与 §11 第 7 步覆盖；§7.5 快路径用例写明先断言 `IsEqual` 再调 `SavePlayerToRedis`。修正评审：「写不出来或不稳」说重了，崩溃也是红 |

共 24 行：成立 17 条（其中 4 条带限定语），部分成立 7 条；严重度重要 6 条、次要 18 条；没有阻断项，没有整条不采纳的意见（不采纳的是部分改法或评审中不成立的论据，见各行与 §15）。

### 17.2 与 L-04 第 1 批接口的衔接（跨条目）

- handler 已是 `error` 签名，本设计扩成 `(writeOutcome, error)`；被拒结局以 `(outcome, nil)` 返回，`reportStoreResult` 只上报 SQL 这一步（§4.1、§4.5）。
- `SaveConditional` 的错误继续套 `storeerr.Tag`；条件写哨兵不打标记、耗预算；`storeerr` 只用于 SQL 调用点（§4.1、§4.4）。
- 每个新增错误或结局都用 L-04 的词表落定 `deferReason`、是否豁免、死信 label：新增 `fence_race`；死信新增 `fence_stale_owner`、`fence_legacy_after_seq`、`owner_epoch_regressed`，追加进 `deadLetterReasons` 并改 Help（§4.4、§10）。
- 失租沿用 L-04 的 `errOrderingLeaseLost`，v0.3 默认不并入 `retryBudgetExempt`；核实意见有分歧，列 §14 第 14 条（§5.4）。v0.4：已定不豁免，理由订正为「失租也可能发生在 SQL 之前」。
- 计数器探测与推进脚本的 Redis 失败计 `redis_unavailable`、耗预算，不进豁免（§4.4、§5.1、§5.5）。
- 连接级超时直接复用 `newServiceMysqlConfig` 的五项；v0.3 写的「本设计只补 `WithContext`」作废：HEAD 已用租约 ctx 写 SQL，v0.4 只在这里加会话隔离级别（§4.6、§4.8）。
- 合入顺序 L-04 在前；Stage 1b 还依赖 L-04 第 2 批死信工具（§8.3、§14 第 9 条）。
- 行号基线：v0.2 引用 `0aa1027bc`；工作区 KOC 整体后移约 4～51 行、PL 插入约 286 行，所以 v0.3 改写处以函数名为主（文首「证据口径」）。

### 17.3 v0.4 的修订来源与处置

| 来源 | 意见 | v0.4 的处理 |
|---|---|---|
| v0.3 补审 F1、F2、F3、F9；fix_plan 一 | 在途实现与 v0.3 正面冲突，请负责人裁决去留 | 负责人 2026-10-16 拍板合并接管；§0.2 逐条去留与合并前置 |
| v0.3 补审 F4；fix_plan 二 1 | §5.5 推进脚本把 epoch 转成 Lua 数值比较、先写游标后校验 | §5.5 改为拼 `luaDecimalGreaterFn`，先核令牌 → 校验 → 写游标 → 写 epoch；删「double 精确」；ctx 用 `WithoutCancel` 加单步上限；§9.1 `epoch-raise` 复用；§11 加「非规范值不改游标」用例 |
| v0.3 补审 F5；fix_plan 二 2 | §4.6 让写 SQL 随停机取消，与 G1 治标冲突 | §4.6、§4.1 改为写 SQL 用租约 `sqlCtx`、停机不取消，删「改为 WithContext」工作项；SQL 之后的 Redis 步骤用 `WithoutCancel` 加单步上限；§11 加 Stop 第④步交错用例 |
| v0.3 补审 F6；fix_plan 二 3 | §6 没按 runDBOp 内的 verify 重写，往返数与失租记账未定义 | §6.2 令牌发布脚本取代 verify + SET（核令牌、PEXPIRE、SET、证据），返回 0 时 `setLost(owner_changed)`，成功后 `extendValidUntil`；§4.5 按定稿顺序重写并给出往返数；§5.4 / §6.1 现状改写 |
| v0.3 补审 F7；fix_plan 二 4 | `db_ordering_guard_total` 已存在；RedisSteps 是固定契约 | §5.2 / §10 改为扩展现有指标（加 `stale_no_epoch`、`epoch_override`，子集口径）；定新 step 名（`cache_publish`、`applied_advance`、`owner_epoch_probe`，删 `cursor_set`、`epoch_mark`，共 12 个）；§12 列入 `metrics/pipeline.go`、`metrics_test.go`、`stress_summarize.ps1` 2e 段、`tools/scripts/test_stress_summarize.ps1`，注明归 go/db 会话、需协调（补审写的 `tools/scripts/tests/` 路径不对，实际在 `tools/scripts/`）|
| v0.3 补审 F8；fix_plan 二 5 | Stage 0 放行口径「只有 4 条」已过时 | §3.4、§8.1、§11 第 1 / 4 步改按 G-03 最新清单（六列 + 两列，plan 为空也合法），取号从 21 起，生成物只核对 DBTask 9 / PlayerAllData 7 相关的 15 个 |
| v0.3 补审 F9；fix_plan 二 6 | 资产 SQL 提交证据是未登记的新消费方 | §4.4 结局表加证据列；新增 §6.6、§8.7；§14 第 23 条 |
| v0.3 补审 F10；fix_plan 二 7 | §4.2 与 §8.2 对 `legacy` 是否盖章矛盾 | §4.2 定为 `legacy` 也置 `(0,0)`，`db_fence_body_mismatch_total` 两种模式都计；§8.2 同步 |
| v0.3 补审 F11；fix_plan 二 8 | 测试计划漏既有测试；④ 后排空门禁要覆盖 p* | §11 第 2 步列出受影响测试与新用例；§8.3 加 p* 覆盖；§5.5 记 WAITAOF 待核 |
| v0.3 补审 fix_plan 二 9 | DBTask 注释没写 10 号预留、8 号注释未订正 | §3.1 给出注释定稿 |
| 在途分析 F1、F4、F8 | 被拒写当成功 ack 并推进游标；错误无 `storeerr` 标记；违反「不碰 internal/kafka」约定 | §4.1、§4.4、§4.5：`(outcome, nil)` 交回 `handleTask`、不推进游标与 epoch、`older_seq` 先复核再 ack、死信经 `quarantineOrderingConflict`（继承 S1 的 WAITAOF）、错误一律 `storeerr.Tag`；§0.2 合并前置 |
| 在途分析 F2 | `proto.Equal` 读回比对必误判 | §4.4 `equal` 不比内容；§14 第 12 条 |
| 在途分析 F3、F10、F12 | 每实体计数器、C++ 写 body 围栏、组件职责混杂 | §7.2 进程级 `SaveSeqAllocator` + 个人下限，删 `lastSaveSeq`；C++ 只写 `last_save_seq`；§3.1 注释 |
| 在途分析 F5 | 无行时 FOR UPDATE 的锁语义 | §4.8：无行走 `Insert`、撞 1062 重读再判、会话固定 READ-COMMITTED 并启动探测 |
| 在途分析 F6、F7 | 自动放弃可能一物两得；证据键生命周期与比较方式 | §6.6：按 zone 开关、放弃条件、`Ledger` 启动断言、证据并入令牌脚本按字节比较、键族登记、合服与回档清理；§8.7 上线顺序 |
| 在途分析 F9 | 每次写读整行；快路径深拷贝；探针口径 | §4.8 代价待压测；§7.3 剥离放 `Replace`、快路径改回；§14 第 22 条 |
| 在途分析 F11 | 无指标、表名硬编码、无测试 | §4.1 启动按描述符判定；§10 计数与告警；§11 用例 |
| fence-decisions.md 13 项 | 逐项推荐 | §14 第 5–20 条定稿；第 1 项（§14 第 12 条）不按其推荐，理由写在该行 |
| go/db 会话 R1 | stale 分支不得跳过 confirm / verify；handled 分支 `storeerr.Tag`；日志英文与计数器 | §0.2 末段、§4.5（「只有丢弃需要证明」）、§4.1、§10「日志」 |
| go/db 会话 `runDBOp` 定稿顺序 | lease.beginSQL → SQL → finish → 失租分支 → reportStoreResult → 被拒结局返回 → confirm → verify → 缓存 SET；成功分支 markAppliedSeq → markAppliedOwnerEpoch → LREM / ack | §4.5 按此插入结局出口、合并两组步骤，不改先后 |


### 2026-10-17 统一接手落码附注（实施中，尚未完成运行验收）

本轮负责人明确授权解除旧交接等待并统一接手 Go 围栏，与 C++ 父快照 pending / 登录冷加载屏障合并同一链路。保留 v0.4 的 SQL 结局、legacy/class-2 兼容、override 门禁及 R2 持久化后 ACK 入口。

`equal` 仍不比较载荷内容，但缓存与 `PlayerAssetSQLCommit` 必须从同一事务已锁定读取的 SQL `current` 行回填后发布。原因：第一笔 SQL 提交后可能在发布证据前失租；相同 `(owner_epoch, save_seq)` 的第二笔即使载荷不同，也只能证明真实已提交的账本。此处理不增加 SQL 往返，不改变 equal 的幂等分类；对应真实 SQL 回归覆盖首笔提交后换令牌、重投异载荷 equal 的证据真实性。

DB 令牌发布脚本另带 `player:{id}:save_barrier`，只在 `dual` / class-1 的 `applied|equal` 后单调 ACK `player_database` 或 `player_database_1`，绝不写 `pending`；同版证据冲突在任何缓存、证据与屏障写入之前拒绝。父快照写者由同一 guard 检查后写 `pending`，且父写成功回调后才投递两表 SQL 任务。新屏障/资产证据有 `{id}`，现有排序锁、缓存和 owner_epoch 旧键未迁移；多键 Lua 当前前提是同一 Redis 实例，不能据此声称 Redis Cluster 同槽已完成。

玩家主池与额外 placement 池复用 `shared/authoritysql` 当前会话初始化；`ServerConfig.Database.RequireTiDB=true` 时每条新物理连接固定 RC、pessimistic、leader、strict、空 snapshot、staleness=0 与 low-resolution TSO=OFF，并在投入使用前探测。false 仅兼容本地 MySQL RC，不承诺 TiDB 全球数据层语义。`ServerConfig.OwnerEpochRedis` 可选，只读核心实例计数器；缺省复用主 Redis，独立连接在消费者停止后关闭。

### 2026-10-17 C++ 父快照提交屏障与 SQL 任务准入

`MessageAsyncClient::SaveWithCommitBarrier` 用同一 Lua 检查旧 owner 键、父缓存类型、屏障 hash 与分桶待投 ZSET,成功才同时 SET 父缓存、HSET `pending` 与完整两条 DBTask 意图、ZADD 待投玩家。版本为 `%020d:%020d` 的 41 字节 `(owner_epoch, save_seq)`（每段 uint64）,逐字节比较且拒绝溢出;已有 pending 不得倒退。旧 owner 键继续使用 `player:123:owner_epoch`,新屏障使用真实花括号 `player:{123}:save_barrier`,当前部署前提仍是同一 Redis 实例,不宣称 Cluster 同槽。owner 缺失属于恢复故障,不自动补种;owner 不等才是废黜。所有参数、TYPE、玩家键身份、存量版本与完整意图检查先于任何写入,避免 Lua 报错留下部分副作用。索引/topic 的 zone 沿用 DB storage 区号 1..999999,topic 世代沿用 uint32 且第一代无后缀;分桶按玩家十进制逐位求余 64,不把 uint64 转 double。

每份完整快照携带自己的提交钩子,每份实际成功写入 Redis 的值都立即执行一次,先投递两张完整子表 DBTask、登记投递票据,然后才发出下一份排队快照;全局 `HandlePlayerAsyncSaved` 仍只对最终最新值发布。只有未下发就被合并的中间值、拒绝、错误和旧连接回调不能投递 SQL 任务。钩子前崩溃仍可由持久意图补投;未下发的中间快照被最新完整状态涵盖。钩子与回包同线程,保留本次在途登记直至钩子结束,允许重入 Save;钩子后重新查 key 与 element 身份,不跨钩子持有 map iterator。两张完整任务必须在写父键前全部序列化成功,严格 API 强制显式非空意图,没有绕过意图的旧重载,参数或序列化失败向调用方返回拒绝。钩子只按值持有不可变任务字节和玩家号、home zone、topic、owner epoch、save seq,不捕获 ECS 引用或 Node 裸指针。epoch=0 的旧兼容路径也在父写成功后才投递 SQL,但不提供严格 pending/outbox 屏障,仍受全 writer 升级门禁约束。

`HandlePlayerAsyncLoadFailed(DataNotFound)` 一律走载入失败通知、会话映射清理与 `AbandonInheritClear`,不能把缓存缺失解释成新号。`HandlePlayerAsyncLoaded` 在消费 pending 或暂存 A2′ 载入结果之前要求两张子表的 player_id 都等于当前非零玩家号,空 ID 或串号均走同一失败链,不再依据异步 key 补 ID。新号的父缓存已由 login `SeedNewPlayerAllData` 预种双 ID,合法种子的 registration_timestamp 仍为 0,继续由 `InitPlayerFromAllData` 完成首次注册;冷加载无法证明 SQL 两行与屏障齐备时拒绝入场。运行验收由 Codex 统一安排,本切片不单独启动 MSBuild。

归属交接不能只等父 Redis 成功:新 epoch 会令尚未落库的旧 epoch 任务被拒绝。`BeginTravelHandoff` 改用同实例三键 Lua（旧 owner、save_barrier、旧 handoff）,仅当 owner 仍同代、pending 合法且属于该 owner、两 SQL ACK 都精确等于 pending 时,才在同一原子段 SET 原格式标记并保留原 300 秒 TTL。双 ACK 未齐返回 0 且不写标记,释放本次请求占位与 markEpoch,按原冻结代次每 250 毫秒重查;截止固定在冻结起点后的 35 秒,不因重试滑动。0 状态仍受原存盘看门狗与 70 秒冻结硬上限约束。只有明确 1 才发 SceneManager;owner 已变则不存盘销毁旧实体,不采纳新 epoch;明确错误不留标记,连接断开或异常回包仍按原未知结果取证,不能直接解冻。timer 与回包都按玩家号和单调冻结代次回查,退出、新代次或另一条标记命令在途时旧 timer 失效。实际 Lua 的连续 0→0→1 / 错型 / 错版 / owner 变化回归与纯单调时间接缝、wiring 契约同批覆盖;ECS 与 muduo timer 贯通运行尚待单独宿主验收,源码契约不替代该证据。

### 2026-10-17 登录冷恢复与恢复运维门禁（实施中）

登录预检通过同一 Lua 读取父快照、旧 owner 键和新提交屏障；已有 pending 时，两张表 ACK 必须都精确等于 pending，缺失、畸形、越界、不齐或 Redis 故障一律拒绝。已有父快照的 player_id 与 last_save_seq 也必须匹配。没有屏障时，只放行 owner 尚未铸造的建角种子及下段明确列出的身份补齐状态；历史快照、历史 owner、零围栏但已有业务数据的 SQL 行都不得冒充新号。

首次身份补齐到首次 owner 之间的中断允许重试：owner=0、无 pending 时，父键还可仅含 login 权威补齐的 class/name/appearance_id/gender。检查在副本中只清除这四个字段，并仅移除由此真正变空的两个组件，最后仍须与完整建角种子 proto.Equal；未知字段、注册时间、任意资产或其他组件均不忽略，原父字节不回写。已铸造 owner 却无屏障仍明确拒绝。组合回归使用真实 Seed→backfill CAS→正式 Ensure，不以手工写身份代替整条路径；正例先红后绿，负例覆盖1金币/已注册/顶层与嵌套unknown。

父键缺失一律通过现有 Kafka DB read/dispatcher 读取真实 SQL 两行，不再从 Redis 子表缓存拼接。两行的身份与围栏必须精确等于本次捕获的 pending；组装父键继承该 save_seq。提交用 Lua 再核 owner 和 pending 未变、两 ACK 仍齐，最后 SET NX，不能覆盖期间写入的新父快照。回调时间原点在发送前固定，消除同步发送与结果回调的读写竞态。发送和回调登记只增加接口测试接缝，不改变 Kafka 协议。

这是需要整批切换的协议：必须先迁完两张 SQL 表、升级所有 DB 为 dual 与所有 scene/login 写者、停止并排空旧 class-2/class-3 与旧 topic/retry，确认旧进程不能恢复写入，再启用资产 SQL 终结。对旧存档，首次上线需在停写期间核对两行同版后建立 pending/双 ACK；不能直接在活跃存档上猜一个版本补种。当前代码遇到尚未迁移的历史数据会明确拒绝，不声称滚动混用旧写者仍可用。

合服/撤销与 PITR 必须清除受影响玩家的父缓存、子缓存、asset_sql_commit 与 save_barrier；旧 ACK 指向旧 SQL，不能保留。清理之后仍须保持维护闸，完成归属计数器恢复并从同一强读权威库核对两行同版、重建屏障，才可放流。两行不同版、在途写尚未排空、旧实例仍可能写入时禁止恢复。删除后仅等待一次新存盘不足以解锁冷加载；当前未提供自动线上补种或已演练的跨区域灾备结论。

> **2026-10-20 更正（r2-mz#1，上段原文保留）**：前向 copy 合服不再删 `save_barrier`（目标库两行是源库的逐字节拷贝，原 pending / ACK 对它们仍成立），由 `-verify-merged` 的 `verify:save_barrier` 逐人核对；撤销与 PITR 仍按上段删除并重建。详见文末「上线状态」之前的同日附注。

### 2026-10-17 收据身份与父快照投递恢复补充（实施契约）

资产账本负责把请求身份、资产改动和结果放进同一份玩家 SQL 快照；邮件、交易、帮会工作器只凭真实 SQL 快照中的相同身份终结业务。所有 APPLIED、部分 APPLIED、REJECTED 与中止均保存已有 `AssetOpReceipt` 的 seq、32 字节摘要和 correlation；不再只给托管实例记摘要。C++/Go 沿用固定 `debit` canonical、保留 caller、时间置零、签名清空的摘要格式，apply 转 abort 不变。Java 使用既有内部 protobuf 摘要与 caller/stream 白名单，两版内部玩家记录不混库。

ACK 只释放大 snapshot，保留窗口内摘要；窗口宽 1024，滑窗或换流纪元按既有规则剪枝，不能称全历史永久收据。摘要由每流唯一有序窗口位置与固定长度限界，原有 256 KiB 玩家额度只计托管 snapshot；按本次换纪元/剪枝后的候选状态预检，不能让已可剪掉的旧快照堵死新操作。旧档只有 bitmap 时保留原结局、不重置账本，但不能替未知请求背书：该请求 UNKNOWN，不重复扣发。已在交易 SQL 保存原件和原 APPLIED 证明的 ACK 待办，滑出窗口后仅允许结束清理，不能据此确认原资产结局。所有终结消费入口必须一起升级，旧写者可能删除摘要，不能混跑并宣称同一身份协议成立。

为补父 Lua 成功但 scene 在投 Kafka 前退出的窗口，沿已有 `save_barrier` 保存持久投递意图，不新增玩家资产权威。严格保存 API 显式接收完整两条既有 DBTask 序列化字节和目标 topic；只构造任务提前，真实发送仍在父提交钩子中。严格 owner 非零但 home_zone 未知时拒绝，不能回退到当前进程 zone。

Lua 契约固定为四个 KEYS（父键、旧 owner 键、barrier hash、分桶 pending ZSET）及七个 ARGV（父 bytes、owner 十进制、41 字节版本、topic、两条 DBTask bytes、player 十进制）。barrier 同时写 `pending` 和 `outbox_version/outbox_topic/outbox_player_database/outbox_player_database_1`，索引键 `player:save_pending:<home_zone>:<player_id % 64>`，member 为 player 十进制，初始 score=0。所有 key TYPE、彼此不别名、版本与载荷约束先验，任何拒绝均无部分写入。同 pending 版本重试还须父 bytes 完全相同；有原意图则 topic 与两条任务 bytes 也须完全相同。父缺失不得拿相同版本重新覆盖组装。

go/db 有界补投工作器读取本 zone 的 64 桶 due 索引，严格校验任务身份、表类型、版本和允许的 topic，不改变原 epoch/seq/业务号，以玩家号作 Kafka key。失败和未齐双 ACK 的项 CAS 重排到下次时间，防止坏前缀饿死其他玩家；进程内预算使用单调截止，索引 score 的毫秒时间只作调度，不参与版本/GUID 比较。仅捕获 pending 未变且两张 SQL ACK 都精确等于它，才原子清意图与索引；不能以 broker ACK 当 SQL 已提交。home_zone 改变留下的旧索引由工作器按当前意图 topic 进行 CAS 迁移/清理，迁移目标须通过命名与世代白名单，不允许任意 topic。

此补投使用与父键同一 Redis 主实例，不能把只读 OwnerEpochRedis 错当父数据存储。父写、barrier 与索引原子范围仍为单实例 Lua，Redis 的 AOF/持久化与恢复门禁必须配置；它不是跨 Redis/Kafka/TiDB 的分布式事务。资产最终成功仍只由 TiDB 强读证据决定。合服/PITR 清 barrier 时也须同步清受影响玩家的 pending 索引；未完成的意图不得在维护恢复时静默丢弃。

DBTask TopicGeneration 换代的排空集合同时包含旧代 Kafka、retry 与本节 pending outbox。离线玩家也可能只留下旧 topic 的完整意图,不能在它未获双 ACK 时先把全体 worker 切到只认新代的白名单;须在旧代完成提交并清理意图 / 索引,或执行明确设计的维护迁移后再切换。当前实现不把任意旧 topic 自动放行,未提供自动跨世代搬运。

合服/撤销的 `merge_zone` P4 同时核对待变更 zone 的全部64桶 ZCARD=0；索引不分世代，因此旧代残留也会拒绝。Redis故障、错型、异常响应均不能当空。检查发生在 SQL 行复制/删除、映射变更和缓存清理之前；停止 scene 后须让原 DB 补投器完成双 ACK 回收，再停止 DB 并执行维护，不能只看 Kafka lag=0 就删 barrier。PITR 则必须先保存并逐笔裁决尚未提交的原意图，再清受影响玩家自己的索引 member；禁止 DEL 整桶误删其他玩家。历史屏障的计划/维护恢复工具已按文首 2026-10-18 契约完成限定形态真库验收；不支持的路由形态继续拒绝，删除屏障后仍保持维护闸，不会自动放流。

账本窗口保留五流各1024个摘要后，不能继续套用普通组件256KiB的列闸。`BlobGuardConfig.WithDefaults`统一补齐`player_database.asset_op_ledger`专列1MiB，普通列仍256KiB，整行仍1MiB；配置显式给出的正值覆盖默认，包括更小的限制。当前实际依赖`github.com/luyuan-cpp/proto2mysql v0.2.0`的SQL二进制参数是裸protobuf；既有BlobGuard仍按base64长度保守计费，本轮保留该算法和指标口径以避免隐式扩大所有玩家行，不能把预算计费值称为真实物理存储量。托管snapshot本身的256KiB上限仍在资产模块执行，不随列预算放宽。满窗口账本与其他组件合计超过整行额度仍明确拒写，不能只看账本单列通过。保留原整行保守上限也维持现有Kafka大小约束：本仓librdkafka默认`message.max.bytes=1000000`，当前producer未覆盖；此轮没有同步提高消息、broker或消费者限制，不能把整行改成2MiB后仍声称发送链支持。对照回归直接调用当前依赖的`pbconv.SerializeFieldValue`核对真实SQL参数字节，另外记录保守预算，禁止用测试里的人工base64推测当前数据库格式。


### 2026-10-18 存盘协议世代门禁与补投有界（审计 §2.2，Claude，未编译、未测试）

来源：[data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.2 的 go-db-pipeline#0、#1，以及同根因的 low #2。三条都已复核成立。

**问题**
- 10-17 起，C++ 只要 `owner_epoch≠0` 就走 `SaveWithCommitBarrier`，写 pending 和完整投递意图。login 冷加载与 `BeginTravelHandoff` 要求两表 ACK 都等于 pending。
- 但只有 `dual` 写 ACK、跑补投，而 `db.yaml` 与 `EffectiveMode()` 的缺省仍是 `legacy`。结果是按缺省配置部署，玩家第二次登录起全部失败，交接只能等冻结上限。
- 补投器每桶 100ms、64 桶一轮，约 6.4s。每次到期都整行重投，没有计数、退避、终态，也不看存储健康。对确定性拒写，每轮都多出一条接近 1MB 的死信。

**协议世代**
- 现行 scene / login 的世代由二进制决定，只有严格屏障这一种。能配置的只有 go/db，所以「三方一致」由 go/db 一侧的机器门禁保证。C++ 与 login 本批不改。
- **缺省 `dual`**：`go/db/etc/db.yaml` 写 `Mode: dual`，`FencingConfig.EffectiveMode()` 把空串解释为 `dual`，`k8s_deploy.ps1` 原样镜像。
  - `k8s_zone_rollback.ps1` 读线上 ConfigMap 时，Fencing 整段缺省也按 `dual` 记。
- **世代证据**：都在父缓存所在的同一 Redis 实例上，任一成立即为严格世代。
  - `db:save_protocol:zone:<ZoneId>` 存在。`dual` 启动时写 `strict`，不设 TTL。已有未知值或类型不对时拒启，不覆盖。
  - 本 zone 64 个补投桶 `player:save_pending:<zone>:<b>` 任一非空。
- **legacy 门禁**（`saveoutbox.CheckLegacyAllowed` / `WatchLegacy`）：
  - 启动前检查一次：有证据，或 Redis 出错、类型不对，一律拒启（fail-closed）。
  - 运行中每 5s 复查一次。第一次发现证据时，经 `db.go` 的 `protocolConflict` 通道走与信号相同的停机路径，最后以非零码退出，日志前缀 `SAVE-PROTOCOL-CONFLICT`。编排重启后停在启动门禁。
  - 复查期间续写 `db:save_protocol:legacy_alive:zone:<ZoneId>`（PX 30s）。冲突后仍续写，直到消费者停完：停机排空期间实例还可能在落库。
- **混跑（#2）**：`dual` 补投每拍先查 legacy 存活标记，存在即整拍跳过，计 `db_save_outbox_total{result="legacy_alive"}`。等 legacy 实例退出、标记过期后再投。
- **残余**：
  - 不认识门禁的旧 go/db 二进制无法被拦，仍靠 §8.4 第 2 行的发布纪律排除。
  - 旧二进制卡顿超过存活 TTL 的场景，归 go-db-pipeline#3（G1）。
  - Redis 被清空后标记会丢，但届时补投桶也一并清空，legacy 能起来。不过只要严格写者一写，运行中复查就会把它停掉。
- **降级**：见 §8.6 的 2026-10-18 更正。

**补投有界**（`saveoutbox/scripts.go`、`replay.go`）

状态都记在 barrier hash 上，按意图版本计。

| 字段 | 写者 | 含义 |
|---|---|---|
| `outbox_attempt_version` / `outbox_attempts` | 补投器 | 该版本已补投的次数 |
| `outbox_terminal` / `outbox_terminal_reason` | go/db 消费侧 | 该版本已被确定性拒写 |
| `outbox_retry_ms` | go/db 消费侧 | 最近一次把该玩家的严格写延后进重试的毫秒时刻（最后一次写入为准；2026-10-19 前取较大值，见下文 r2 修复） |

- 两表 ACK 齐全时，补投器把这些字段与 4 个 `outbox_*` 意图字段一并删除，屏障回到 pending 加两表 ACK 三个字段。`barrierrestore` 的 `HLEN==3` 判定不受影响。
- C++ 父快照 Lua 与 login 冷加载只读各自的固定字段，不受新字段影响。

认领节奏（缺省值）：
1. **首见只登记**：`FirstReplayAfter=30s` 之后才第一次补投，正常 Kafka 路径优先，消费积压时不把每笔存盘都再投一遍。
2. **指数退避**：第 n 次补投后等待 `min(RetryAfter·2^(n-1), MaxRetryAfter)`，`RetryAfter=10s`、`MaxRetryAfter=10min`，再加 `[0, RetryAfter/2]` 的抖动。
3. **次数上限**：`MaxAttempts=8`。达到后到期也不再投，按 `RecheckAfter=10min` 复查，计 `exhausted`，告警 `DbSaveOutboxExhausted`。
   - 意图原字节、pending 与桶成员都保留。
   - 新版本存盘或两表 ACK 齐全时自动离开该状态。
   - 人工重新计数：`HDEL player:{<id>}:save_barrier outbox_attempts`。
4. **终态**：同版本的 `outbox_terminal` 存在时不补投，计 `terminal`，告警 `DbSaveOutboxTerminal`。新版本到来时清掉过期终态。
5. **在途重试**：`outbox_retry_ms` 在 `InflightWindow=2min` 内时只推迟，不计次数（`inflight_deferred`）。
6. **主库冷却**：本 zone 主库冷却期内整拍跳过，不认领、不计次数（`store_cooldown`），与 L-04 §3.5 的重试循环同口径。
7. **读方就绪闸关着**（2026-10-23 补，r2-rr#5）：本实例 go/db 的就绪闸关着时整拍跳过，不认领、不计次数（`route_not_ready`），本拍中途关上时剩下的玩家也不再认领；与消费循环、重试循环看同一把闸。此前补投器不看闸，详见下文「2026-10-23 第二轮审计修复（r2-rr#5 数据侧）」。

整拍跳过的条件按判定顺序共四个：发布失败后的暂停（`publish_paused`，见下文 r2-db#1）、读方就绪闸关着、本 zone 主库冷却、legacy 存活（`legacy_alive`，见上文「混跑」）。四者都不认领、不推迟索引、不计次数，条件消失后从各意图原有的到期时刻接着补投。

**go/db 消费侧的提示**（`kafka/save_outbox_note.go`）
- 只在 `dual` 下写，只针对带 `(owner_epoch, save_seq)` 的两张玩家表写，且只在该版本仍是当前意图时写，绝不凭空创建 barrier。
- **终态**：
  - `quarantineOrderingConflict` 的全部入口：围栏 stale_owner、owner_epoch_regressed、legacy_after_seq、ordering_incomparable、stale_topic、missing_required。
  - Redis 归属守卫判陈旧：`stale_owner_redis`。
  - `deferFailedTask` 收到的两类确定性错误：大字段闸 `*dbguard.ErrBlobTooLarge` 与 `errFenceEvidenceConflict`。
- **在途**：其余 `deferFailedTask` 只记 `outbox_retry_ms`。
- **不打终态的**：预算耗尽（可能来自 Redis 抖动、失租等可自愈原因），只受退避与上限约束，避免把可自愈的意图钉死。
- 提示写失败只记日志，代价是补投器按退避多投几次。

**指标与告警**
- `db_save_outbox_total{result}` 新增 `observed`、`inflight_deferred`、`terminal`、`exhausted`、`store_cooldown`、`legacy_alive`，启动即以 0 建出。terminal / exhausted 按复查间隔重复计数。
- `deploy/k8s/db-task-retry-alerts.yaml` 新增 `db-task.save-outbox` 组三条：`DbSaveOutboxExhausted`（critical）、`DbSaveOutboxTerminal`（warning）、`DbSaveOutboxPausedByLegacyWriter`（critical）。上线时手工 apply；阈值未经 promtool 校验。

**验证与 Java**
- **验证**：未编译、未测试，交 Codex。用例在：
  - `saveoutbox/generation_test.go`、`replay_bounds_test.go`，以及改造过的 `replay_test.go` / `replay_integration_test.go`；
  - `kafka/save_outbox_note_test.go`；
  - `kafka/player_row_fence_mode_test.go`（空串改为 `dual`）；
  - `tools/scripts/tests/k8s_zone_rollback_discard.tests.ps1`（Fencing 缺省改为 `dual`）。
- **Java 版**：不适用。Java 不经 Kafka / go/db 落库，没有父快照提交屏障与补投器（AGENTS §12）。

**2026-10-19 第二轮审计修复（r2-db#1、r2-db#3，Claude，未编译、未测试，待 Codex 验证）**

来源：[data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.1、§3.2。两条都先对照代码复核，确认成立后才修。上文第 3、5 条与字段表以本段为准。

- **r2-db#1：补投把发布失败也计次。**
  - 问题：旧认领脚本先把 `outbox_attempts+1`，再交出意图，`Publish` 失败也不回退。Kafka 生产持续不可用约 21 分钟后，意图停在 exhausted；恢复后没有路径重置计数，实体已销毁的玩家会卡在登录。
  - 计次规则：认领只按本次退避推迟索引（充当跨实例租期），不再计次。两条任务都经 broker 确认后，由 `confirmAttemptScript` 计一次，而且只在版本与计数仍等于认领时读到的值时 +1，并发认领和新版本存盘都不会重复计或串版本。只确认了一条也不计。
  - 发布失败：整个补投器暂停 `RetryAfter`，计 `publish_paused`。这样不会在 broker 故障期间逐拍认领、推迟其他意图并刷 ERROR。
  - 意图本身不合法（身份不符、topic 世代未放行、跨 zone 迁移缺权威 home_zone）仍计次。重投不会让它变好，所以照样走到 exhausted 并告警。
  - 修复后 exhausted 只来自「broker 已确认，但两表 ACK 迟迟不齐」，即消费侧问题；告警 `DbSaveOutboxExhausted` 的归因随之成立。
- **r2-db#3：在途戳取较大值，且用本机时钟。**
  - 问题：时钟超前 Δ 的实例写下的戳，会把补投推迟约 Δ+窗口且不计次；之后正确的时刻也覆盖不了它。
  - 写入：`noteRetryScript` 改为最后一次写入为准。
  - 认领：戳超过 `now + InflightWindow`，或格式不对，都按非法截掉（HDEL）后照常判定。单个未来戳造成的推迟上限因此是两个窗口（缺省 4 分钟）。
  - 没有改用 Redis TIME：调度分数用的是 go/db 时钟，在途比较改用 Redis 时钟只会换成 Redis 与 go/db 之间的偏差，截断已经给出上界。
  - 「C++ 写新版本不清戳」维持不改：戳按玩家计，表示该玩家的写仍有重试链在途，新版本此时补投同样会被挡回。截断后它的影响也有上界。
- **r2-db#2**（legacy 存活暂停挡不住已发到 Kafka 的旧补投副本）只在 dual→legacy 降级或混跑时出现。按下段「上线状态」，上线前不阻塞，本批不改代码。上线后若要降级，§8.6 的步骤需补「该 zone 的 db_task topic 积压为 0」。
- **测试**：
  - 回归用例 `saveoutbox/replay_r2_regression_test.go`：发布失败不计次且恢复后照常补投、部分确认不计次、发布失败暂停、非法意图仍计次、超前戳截断、窗口内未来戳仍推迟、在途戳最后写入为准。
  - 除「非法意图仍计次」「窗口内未来戳仍推迟」两条护栏外，其余五条只用改动前就有的接口写成，可在旧代码上先跑出红。
  - `replay_bounds_test.go` 的「取较大值」断言改为最后一次写入为准。
- **指标**：`db_save_outbox_total{result}` 新增 `publish_paused`。
- **Java 版**：不适用，理由同上。

**2026-10-20 第二轮审计修复（r2-mz#1，Claude，未编译、未测试，待 Codex 验证）**

来源：[data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.3、§5。先对照代码复核成立后才修。上文「2026-10-17 登录冷恢复与恢复运维门禁」末段「合服/撤销与 PITR 必须清除……save_barrier」对**前向 copy 合服**不再成立，以本段为准；撤销与 PITR 不变。

- **问题**：copy 合服在步骤 1 拷行后删掉 `player:{id}:save_barrier`、保留 owner_epoch。曾进过场景（owner_epoch≠0）的被拷玩家冷登录时，`beginParentLoad` 报「历史玩家父快照与提交屏障缺失」，整批进不来。合服手册没有重建步骤，`save_barrier_restore` v1 又拒收没有落点记录的人。copy 撤销同样删屏障。fail-closed，不丢数据。
- **前向 copy 合服保留屏障**（`tools/merge_zone/player_rows.go` 的 `invalidatePlayerCachesKeepBarrier`；父 / 子缓存与资产 SQL 提交证据照删）。成立的前提逐条复核如下：
  - **同实例**：屏障键不带 zone，与父快照、owner_epoch 同在 login 的核心 Redis。`beginParentLoad` 用一段 Lua 读这三把键，要求它们同实例。merge_zone 删父快照用的 `-notice-redis-*` 句柄就是这个实例，所以合服后目标区 login 读的是同一把屏障键。
  - **稳态**：P3 / P4 要求源区 Kafka、retry、processing、dead 与 64 个补投桶全部排空。补投器只在两表 ACK 都等于 pending 时回收桶成员（`saveoutbox` 的 `claimScript`），所以通过 P4 时每名源区玩家都满足 pending == 两表 ACK == 源库两行的行内围栏。
  - **同版**：步骤 1 按列名逐列拷两表整行，`fence_owner_epoch` / `fence_save_seq` / `persist_schema_gen` 随行过去，目标库两行与源库逐字节相同（续跑按 `acceptIdenticalTargetRows` 认回）。login 冷加载在目标库读到的正是这两行，原 pending 与两表 ACK 对它们同样成立。
  - **字段**：hash 里 C++ 严格屏障写的 `schema_gen` / `schema_writer` 原样保留。合服后新加载实体的令牌必然不同，首次存盘照常做一次世代比较（[mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md) §5.2），这是期望行为。
- **机械核对**：前提不只靠推理。`-verify-merged` 新增 `verify:save_barrier`（`tools/merge_zone/save_barrier_verify.go`），对清单里 owner_epoch≠0 的玩家逐人复刻 login 冷加载的拒绝条件：pending 合法且不超过 owner_epoch、两表 ACK 都等于 pending、有效落点库（记录 ?? `zone_<dst>_db`）两张表的行内围栏都等于 pending。任一不成立即 block，Notes 按原因分组；读失败报 INFRA。只取 pending 与两表 ACK 三个字段，不按字段集合判。owner_epoch 为 0 或不存在的人不要求屏障。`-route-source=db` 与 `redis` 走同一路径，落点经同一个读方抽象读取。
- **撤销照删**：改回源区后玩家读源库，合服后在目标库提交的 pending / ACK 不再对应他要读的行。删后保持维护闸，按 [merge-zone-runbook.md](../ops/merge-zone-runbook.md) §10.1「屏障重建」用 `save_barrier_restore` 从源库重建，之后才开服。pin 模式两个方向都不删屏障。
- **`save_barrier_restore -allow-home-fallback`**：plan 期开关，写进 `Plan.allow_home_fallback` 并随计划进 SHA-256；apply 与 guard 拒收该开关，执行以审定计划为准。它只放宽一类玩家：落点记录不存在、home 恰为计划 zone、`-schema` 就是该 zone 的 zone 库，按 go/db 的有效落点（记录 ?? home）恢复。其余原子校验一条不少：
  - 同实例时，恢复 Lua 在写屏障的同一段里用 `EXISTS` 复核落点键仍不存在；拆实例时，mapping 维护检查在前后各核一次。
  - 落点键存在但值为空串，按畸形记录拒绝（go/db 对它 fail-closed），不混同「无记录」。
  - home≠zone、落点指向他库或冻结、屏障与 SQL 不同版、缓存 / 证据 / 交接标记仍在、补投桶非空、合服围栏仍在，照旧拒绝。
  - 缺省 false 时行为与修复前一致；字段 `omitempty`，旧计划照读。
- **测试**（未运行，交 Codex）：
  - `tools/merge_zone/player_rows_barrier_test.go`：前向清单不含屏障、撤销清单含；fake Redis 上前向不删、撤销删、dry-run 不发命令；AST 钉住 `runMerge` / `runUnmerge` 的接线；镜像字面量；`judgeSaveBarrier` 逐条；verify 的通过、block、不要求、INFRA、NOT VERIFIED；屏障含 `schema_gen` / `schema_writer` 时通过，且只 HMGET 三个字段；`-verify-merged` 包含这一行。
  - `tools/merge_zone/save_barrier_integration_test.go`（`merge_integration`）：真 MySQL 读行围栏（含 uint64 上界、缺列报错），真 Redis HMGET 不受多余字段影响。
  - `go/db/internal/barrierrestore/home_fallback_test.go`：无记录且 home==zone 恢复成功、幂等、不补写落点；缺省拒收；home≠zone / 非规范 / 缺失，落点空串 / 他库 / 冻结 / 畸形，一律拒绝；计划后落点出现时，Go 侧复核与 Lua 原子复核分别拒绝；零围栏 SQL 提交后重锁发现落点出现则不发布；其余原子校验仍拒；篡改计划去掉开关被 `Validate` 拒绝；`redis-split` 只回退到本区 zone 库。
  - `go/db/cmd/save_barrier_restore/home_fallback_test.go`：开关只在 plan 生效；计划文件携带开关，去掉后摘要改变且 `Validate` 拒绝。
- **残余**：`verify:save_barrier` 只在 Step 5 跑；Step 5 之后到开服之间的写入不再复核，与其他 verify 行相同。撤销没有对应的 verify 行，靠 `save_barrier_restore` 的逐人回执与受控测试号冷登录确认。
- **Java 版**：不适用。Java 没有 Redis 父快照提交屏障与 `save_barrier_restore`，沿用 player / player_state 同 SQL 事务；Java 合服作业随批次 7.3 待做，届时按其自身存储协议设计，不照抄本条。

**2026-10-22 修复（r2-mz#1 评审跟进，Claude，未编译、未测试，待 Codex 验证）**：上段「机械核对」与「残余」以本段为准。

- **问题**：`verify:save_barrier` 照的是 10-22 之前的 login。现在的冷加载走 `savebarrier.Reconcile`（[zone-redis-regression-guard-20261022.md](zone-redis-regression-guard-20261022.md) §4.1.4），对 V2 / V7 会采纳 SQL 自愈，verify 却一律 block、并按手册要求跑 `save_barrier_restore`：比 login 严，会把合法合服挡在 Step 5。`barrier_missing` 又把「无 SQL 行」（r2-login#1 的受害者，恢复工具救不了）与「零围栏」（要 `-allow-legacy`）混在一起。另有两类假 info：owner_epoch 键类型错（MGET 回 nil，被当成不要求屏障）、owner 缺失或为 0 而屏障存在。
- **改法**（`tools/merge_zone/save_barrier_verify.go`）：`judgeSaveBarrier` 按 login 冷加载的顺序预判（beginParentLoad 的键类型 / 字段规范 / owner 不低于 pending 代次 → route 的在途等待 → 两行缺失 → `reconcileSaveBarrier`，后者逐条镜像 `Reconcile`），结论分三档：V1 一致 info；login 会采纳 SQL 的 `barrier_missing_sql_adoptable` / `sql_rows_ahead_of_barrier` warn；会拒登的 block。`barrier_missing` 按行状态拆成 `barrier_missing_no_sql_row` / `barrier_missing_zero_fence` / `barrier_missing_rows_disagree` 与上面的 adoptable；`pending_malformed` 并入 `save_barrier_malformed`（也覆盖屏障键类型错、ACK 不规范）；新增 `barrier_without_owner`。读法改为每人一条 GET owner_epoch 加一条 HMGET 屏障、同一条 pipeline；WRONGTYPE 归到那名玩家（owner 记 `owner_epoch_malformed`、屏障记 `save_barrier_malformed`），只有连接级错误才整行 INFRA。范围从「owner≠0」扩为「owner≠0、键畸形、或屏障里有 pending / ACK」。
- **有意比 login 严的一格**：屏障在途（`ack_not_equal_pending`）。身份戳等于实例当前身份时 login 只等补投，P4 之后等不来；实例重启过时 login 读 SQL，两行等于 pending 就采纳。verify 看不到实例身份，按前者 block。warn 也不是保证放行：采纳路径上 login 还要过 C4，verify 不读 schema_gen。
- **防漂移**：判定样例表 `go/shared/savebarrier/testdata/merge_copy_cold_load_cases.json`（27 条，含每个结论），`tools/merge_zone` 的 `TestJudgeSaveBarrier_MatchesTheSharedColdLoadTable` 用 `judgeSaveBarrier` 对表，`go/login/internal/logic/pkg/dataloader/merge_copy_cold_load_test.go` 用 login 真实冷加载入口（miniredis 跑正式 Lua，只替换实例身份）对同一张表；约定 info↔admit、warn↔adopt、block↔refuse，两边合起来证明「verify 判通过，login 一定放行」。这张表同时补上了「前向 copy 合服后（屏障保留、父键已删）冷加载走 V1、不写采纳」的 login 侧用例。改 `Reconcile`、login 冷加载或 merge_zone 判定时同步改表。
- **其余**：超时（每个 auditor 的预算由 `runAuditMode` 统一给）时 Notes 明说是预算用完、不是通过。`player_rows.go` 里保留屏障的理由改为：保住 SQLBehind 防护（目标库行比 ACK 旧时拒登，而不是在屏障缺失时被 V7 静默采纳），并让冷加载走 V1、不制造采纳告警。手册 §5 步骤 1、§8 Step 5 断言表、§10.1 屏障重建与 verify 处置同步改写（撤销重建前排除源库缺行的人、零围栏单独 `-allow-legacy`）。
- **测试**：`tools/merge_zone/player_rows_barrier_test.go` 改为对共享表，另补 block 与 warn 并列、只有 warn、缺屏障按行状态分流、WRONGTYPE 与孤儿屏障逐人归类、超时文案；`save_barrier_integration_test.go` 补真 Redis pipeline 里 WRONGTYPE 逐人归类、前向缓存失效后屏障 hash 逐字段不变而撤销删除。
- **残余**：每个 auditor 的时间预算仍是 `runAuditMode` 里的固定值，`verify:manifest_rows` 与本行在几十万人以上的清单可能稳定超时（INFRA，fail-closed）；放宽要改 `runAuditMode`，不在本条范围。端到端用例 `TestIT_EndToEnd_MergeVerifyUnmerge` 的共享夹具没有围栏列，仍只覆盖 info 一支。owner 为 0 且没有屏障的人本行不判，他们的行在不在由 `verify:manifest_rows` 回答（只看 `player_database`）。

**2026-10-23 第二轮审计修复（r2-rr#5 数据侧，Claude，未编译、未测试，待 Codex 验证）**

来源：[data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.7 r2-rr#5。先对照代码复核：`replay.go` 的 `Tick` 只看发布暂停、主库冷却、legacy 存活三个条件，确实不看就绪闸，数据侧成立后才修。本段只修数据侧（补投在停摆期间放大与计次）；停摆时长本身归 D1，见「残余」。上文 r2-db#1 段「跨 zone 迁移缺权威 home_zone 仍计次」与「修复后 exhausted 只来自……」两句以本段为准。

- **问题**：
  - mapping Redis 一次重启或切主后，对账 leader 撤掉就绪标记，全部 zone 的 go/db 就绪闸同时关上：消费循环不从 Kafka 取新消息，重试循环不认领。
  - 补投器不看闸，对每个未 ACK 的意图照常按退避重发两条整行任务，broker 确认后计次，约 21 分钟进 exhausted；Kafka 写入同时放大约 6 倍。
  - `db_task` 的 `retention.bytes` 按「设计写入速率 × 2h」定，闸关得久时最旧的段被裁。停摆早期下线玩家的最后存盘与补投副本都没了，意图又已 exhausted，返场永远报 `parent SQL barrier not committed`。
- **修复**（`saveoutbox/replay.go`、`service.go`，`kafka/route_ready_gate.go`，`db.go`）：
  - 补投器经构造参数显式注入本实例消费者的就绪闸只读视图 `saveoutbox.RouteGate`。生产实现是 `*kafka.KeyOrderedKafkaConsumer` 的 `RouteGateOpen`，读的就是消费循环与重试循环用的那把闸；`Placement.RequireRouteReady=false` 时没有闸、恒放行，与消费者一致。`New` / `Start` 拒收 nil，装配漏传即拒启，不静默当作放行。
  - 闸关着（标记不在、畸形、身份不符，或读脚本出错）时整拍跳过：不认领、不发布、不推迟索引、不计次，计 `db_save_outbox_total{result="route_not_ready"}`。本拍中途关上时，剩下的玩家不再认领，与重试循环每认领一条前看一次闸同口径。
  - 闸重开后，索引到期时刻没动，从各意图原有的到期时刻接着补投；停摆期间新出现的意图按首见宽限登记。
  - 跨 zone 索引迁移读权威 home_zone 时，按错误是否属于该玩家的映射数据分两类（10-23 评审后细化）：
    - **不计次**：mapping Redis 不可用（连接、超时，重启切主期间的 `LOADING` / `MASTERDOWN` 等服务端回复）与权限配置错误（`NOPERM`）。它们与意图无关、会同时波及所有跨 zone 意图，修好后自动恢复，不必逐人清计数。改计 `redis_error`，报错带上原因；认领时已按本次退避推迟了该意图。
    - **计次**：键缺失（`redis.Nil`）、键被写成别的类型（`WRONGTYPE`，用 go-redis 的 `HasErrorPrefix` 判定），或值不符。这是该玩家映射数据本身的确定性问题，重读不会变好；按意图不合法计次，用完进 exhausted 并触发告警。`WRONGTYPE` 另打一条带玩家号的 ERROR 日志。若把 `WRONGTYPE` 也当故障，意图会永远停在基数退避上，返场一直被拒却没有任何告警。
- **不变量**：
  - 停摆时长不推进 `outbox_attempts`；停摆期间意图原字节、pending 与桶成员都不动。
  - exhausted 只有两个来源：一是闸开着时 broker 已确认、两表 ACK 却迟迟不齐；二是意图本身不合法（身份不符、topic 世代未放行、权威 home_zone 缺失 / 类型不对 / 不符），重投不会变好。停摆时长本身不推进计次；闸刷新滞后的窗口内，每次出事每个意图至多多计一次（见「残余」）。告警 `DbSaveOutboxExhausted` 按这两个来源归因。
  - Kafka 段在停摆中被裁时，闸重开后仍能从意图补回，不再出现「原任务与副本都没了、意图又已 exhausted」的组合。前提是闸重开后的追赶期能收敛，见「残余」第二条。
- **同口径核对的其它路径**（结论：都不会在闸关时把意图推向终态，本批不改）：
  - 重试循环：`consumeRetryQueue` 在闸关时不分拣、不认领；shared / owner 两路的认领循环每认领一条前再看一次闸。认领时预算耗尽进死信（`parseClaimedRetry`）因此也不会在闸关时发生。
  - 闸关上那一刻已派进 worker 的任务：逐条判 `errRouteNotReady` 后走 `deferFailedTask`。写不耗预算（`retryBudgetExempt`）；给补投器只记在途戳（`noteSaveOutboxDeferral`），终态只认 `blob_too_large` 与 `asset_evidence_conflict`。
  - `durable_enqueue`：只把 Kafka 源失败任务的唯一副本按 WAITAOF 落进重试 ready；确认失败就不 ack、位点不提交，不计次、不丢弃。
  - `retry_stranded`：只重搬已结束执行的收据；收据已不在 processing 才摘掉登记，不计次、不丢弃。
- **残余**：
  - 闸是进程级的，每 `RouteReadyGateIntervalMs`（缺省 1s）刷新一次，单次刷新最长约一个 Redis 读超时。出事到闸刷新之间的几拍仍可能补投并计次；认领会把意图推迟至少 `RetryAfter`（缺省 10s），缺省配置下每次出事每个意图至多多计一次，与停摆时长无关。各副本的闸各自刷新，彼此的收敛差同为一个刷新间隔。
  - **闸重开后的追赶期仍会放大写入，本条只在追赶期收敛时才切断「丢版本 / 永久拒登」**（10-23 评审补充）。
    - 放大从停摆期挪到了追赶期：闸一开，所有已到期意图立即补投并开始计次（到期时刻在停摆期间没动）；追赶期间新存盘的原任务也排在积压后面，过了 `FirstReplayAfter`（30s）同样按 10/20/40s… 的退避重投。追赶期的 Kafka 写入速率 = 新存盘写入 w + 补投副本写入 r，最坏仍接近审计估计的正常写入的 6 倍。
    - **收敛条件**：w + r 必须低于本 zone 的消费能力 c，积压才会下降。不满足时积压继续涨：重开时投到队尾的副本在被消费前就被保留期裁掉，后面的副本同理，约 21 分钟后意图进 exhausted，「原任务与副本都没了、意图又已 exhausted」在追赶期重演。这不是本条引入的回归（改动前停摆期间就在放大），但本条的保证以它为前提。
    - 收敛条件成立时，追赶超过整段退避（约 21~30 分钟）的意图也会进 exhausted，但原任务或已投出的副本被消费、两表 ACK 齐全后，下一次复查（`RecheckAfter`）照常回收，不需要人工。
    - **后续任务（未立项，入口在此）：补投器按本实例的消费积压暂停。** 由 `ConsumeClaim` 按本实例当前分配的分区暴露「已追平」信号（`HighWaterMarkOffset` 与已处理位点之差低于阈值，分区被收回时摘除）；本实例的分区全部追平之前，补投器整拍跳过、不计次，与 `route_not_ready` 同口径。积压不只来自就绪闸（主库冷却后的追赶、再平衡、单纯消费慢都会造成积压），所以映射直读 TiDB、就绪闸删除后这条仍然需要，届时接在新的暂停条件上，不复用 `RouteGate`。补投器不感知消费积压是 10-18 起就有的限制（`FirstReplayAfter` 只给 30s）。
    - 本批没做「闸由关转开后再整拍跳过 `FirstReplayAfter`」的缓解：它只把重开瞬间那一波补投推迟 30s，不改变收敛条件，长停摆下几乎无效，还要给补投器加一段翻转状态。
  - 停摆本身超过保留缓冲时，未消费的原任务仍会被 Kafka 删除。修复后意图不再 exhausted，闸重开后由补投补回；停摆期间所有返场玩家照旧被拒登（fail-closed）。
  - 不在本条：data_service 对账 leader 重挂标记要等「候选任务开始之后才开始的一整轮围栏镜像 SCAN」扫完全键空间（`routereconcile/reconciler.go` 的 `tryArm`，每拍 2 页），停摆约「修补耗时 + 1~2 × 键数 / 2000 秒」。这条可用性问题与 `GoDbRouteGateClosed` 告警、[player-route-mapping-durability-proposal.md](player-route-mapping-durability-proposal.md) 的 RTO /「不丢盘」说法，由 D1 映射持久化会话负责。
  - `redis_error` 目前没有告警。mapping 的 `NOPERM` 这类持续性配置错误只表现为 `db_save_outbox_total{result="redis_error"}` 持续增长，以及每次到期一条 Run 的 ERROR 日志（已带原因）。建议在 `deploy/k8s/db-task-retry-alerts.yaml` 的 `db-task.save-outbox` 组补一条「redis_error 持续增长」告警；部署清单不在本条可改范围内，未加。
- **退场**：负责人 2026-10-23 已定映射改为直读 TiDB（[d1-review-fix-handoff-20261023.md](../handoff/d1-review-fix-handoff-20261023.md)），go/db 就绪闸届时作废，本条的闸判定随之失去对象。
  - 同批删除：`saveoutbox.RouteGate`、`KeyOrderedKafkaConsumer.RouteGateOpen`（含编译期断言）、`db.go` 的注入、测试夹具 `routeGateStub` 与 `replay_route_gate_test.go` 里的闸用例。不要保留一个恒放行的空实现。
  - 跨 zone 迁移前读 home_zone 的 `GET player:zone:{id}` 改读 TiDB 真源，归新方向会话。本条的错误分类沿用：存储不可用或配置错误不计次，该玩家数据的确定性错误（缺失、形态不对、不符）计次。
- **测试**（未运行，交 Codex）：
  - `saveoutbox/replay_route_gate_test.go`：闸关着的多拍（跨过整段退避、为次数上限的两倍）不碰 broker、不计次、不推迟索引、不进 exhausted，重开后第一拍照常补投并计一次；本拍中途关上时停止认领；mapping 读失败（传输错误、`LOADING`、`NOPERM` 三个子用例）不计次、不迁索引、报错带原因（`TestHomeZoneReadErrorDoesNotConsumeAttempt`）；映射键被写成 hash、GET 回 `WRONGTYPE` 时按意图不合法计一次（`TestHomeZoneWrongTypeConsumesAttempt`）；`New` 拒收 nil 闸。
  - `kafka/route_gate_open_test.go`：`RouteGateOpen` 跟着每次刷新走（标记不在、读脚本出错都关），不记等待次数，nil 闸恒放行。
  - 既有夹具 `replay_test.go`、`replay_bounds_test.go`、`replay_integration_test.go` 改为注入恒放行的闸，断言不变。
  - 新接口改了 `New` / `Start` 的签名，新用例无法在旧代码上编译出红；文件头写了变异核对方法（去掉两处 `routeGateClosed` 判定即红）。
- **指标**：`db_save_outbox_total{result}` 新增 `route_not_ready`，启动即以 0 建出。不加新告警：闸关着由 `GoDbRouteGateClosed` 覆盖。
- **Java 版**：不适用。Java 不经 Kafka / go/db 落库，没有补投器与读方就绪闸（AGENTS §12）。


### 2026-10-26：单次 writer 身份与永久排空收据（v4 实施中）

本节约束本批隔离候选；尚未正规生成、编译、测试或通过真实环境验收，不能据此执行线上维护。两版各自实现内部协议。物理目标为本任务已核 TiDB 8.5.8 维护构建；锁原语必须由该目标实际最小权限资格原件选定。

每个进程启动使用预先固定的 writer key、instance、独立 SQL principal 与 REGISTER 全文。主体和 instance 永久唯一，重启不能复用旧身份。normal 装配一次消费私有 grant，连接池共享同一 WriterConfig；只有独立 Schema/Operator 装配持有预建或维护能力。normal 不建表、不注册、不冻结、不解冻。fresh-prestart 遇已有表或部分失败残留就拒绝，保留现场。Windows 使用真实 ACL、无 reparse 路径与本地 NTFS 的耐久独占创建；该平台候选尚待合并与实际验证，不能把 Mode().Perm() 当作 Windows ACL。

维护职责按逻辑角色划分：P1 永久关闭 SOURCE 并等待已接收集合确定收敛。兼任 RELAY 的同一物理 writer 保留原 SQL handle，直到 P3 的消费、重试、补投、回调和最终存盘全部确定结束。无 RELAY 的 writer 可在 P1 退役；兼任者只能在 P3 同轮退役。退役先锁自身全部 64 个 slot，再锁 meta/run，核对永久 ROLE_CLOSED / RELAY_QUIESCED 原件中的同 boot、同角色、复活围栏和已收敛前沿，最后原子 REVOKE registry/slots、增加 catalog_revision 并追加 RETIRE 全文收据。超时、取消、purge、PID 消失及 UNKNOWN 不生成成功闭合事实。

P0 的 ManifestProto、初始 ACTIVE 全集、E 和 catalog_revision 永久不回写。初始角色的物理 key 并集必须覆盖 ACTIVE 全集。后续按 (catalog_revision, request_id) 有界读取 P0 之后的永久生命周期收据，只接受连续版本、同 run、同原 E、完整摘要匹配、每 key 恰一次的 RETIRE。预期 ACTIVE 等于初始集合减去这些合法退役；出现新 REGISTER、外来 run、版本缺口、重复或未知 key 都拒绝。正常 NON_SQL_CLOSED 要求全部旧 writer 已退役，紧急 HOLD 则可保留尚在途的精确 ACTIVE 集。

冻结固定锁序为 R 的 slots→reconcile→meta→run→ticket、J 的 slots→meta→run→ticket。锁前推导预期 ACTIVE，锁后按同一实际目录版本重新折叠并核精确等集；全部退役后的空集仍须原子推进 E 和 writable。正常票据只能消费同轮 NON_SQL_CLOSED，票据 ID、目标冻结请求及完整时间窗均进入授权请求摘要。冻结原请求的 prior 固定为 grant 之前已知的排空/危险收据；事务通过 ticket.grant_receipt 核验真正的授权收据及其 prior，避免冻结摘要与尚未生成的 grant 收据互相依赖。

正常解冻只消费原 NORMAL frozen ticket、原冻结收据和 RELEASE_READY 的完整恢复审核。Plan 的 E 始终是 P0 值；冻结前 E、解冻前 E+1、解冻后 E+2。R 的 route_watermark_floor_ms 由原审核证据固定，并在已有 reconcile 锁内与 writable 同一事务取 GREATEST；J 无此行时要求该字段为 0。紧急冻结只进入 EMERGENCY_FROZEN_HOLD，不能自动升级为正常成功或自动解冻。

SQL_RELEASED 后只允许同 run 的全新预批准身份注册。新 boot 先准备消费者、保持 SOURCE 关闭；实际 open 产生正式 ACK 后才追加 PUBLICATION_REOPENED。旧 boot 不重新开闸。缺少真实应用 admission/join/ACK 接缝的入口必须继续保留维护状态，不能用部署脚本、日志或进程退出替应用事实。所有未完成接缝统一进入精确范围清单。

本批新增回归覆盖：P1/P3 合法退役、共享 boot 提前退役拒绝、连续目录折叠与空 ACTIVE 冻结、外来目录变化拒绝、票据换参拒绝、UNKNOWN 不返回成功收据，以及 Schema 专用的既有迁移语句边界。这里只记录用例源码；执行证据仍为零。


补充边界：RR 下普通 meta/registry SELECT 可能读取旧快照；事务进入时被 CURRENT 锁住的 writer_slot 状态与 E 才是拒绝旧事务的依据。新增真实 TiDB 回归以可控驱动屏障和实际 DATA_LOCK_WAITS 观测固定交错，不以 sleep 猜测锁等待，分别要求 LOCK_TOUCH/SELECT_ONLY 独立资格。

同 run 的失败重试只能从 ABORTED_CLOSED 进入 DRAINING 并递增 attempt，完整 P0 字节不变。已永久关闭且没有重开的 root/relay 原件可来自较早 attempt；新 NON_SQL 观察与票据仍必须属于当前 attempt。若真实 relay 重新工作，旧闭合原件不得替代新完成事实，已退休 boot 仍不允许复活。


2026-10-26 接线补充（候选实现中，运行证据仍为零）：

- P6 维护工具在 P0 的 maintenance_issuers 中具名，角色为 MAINTENANCE、无 NORMAL writer key；开工前强读同轮 NORMAL consumed ticket、冻结全文和当前 meta/run。消费目录固定到 P0 该工具证据目录，不能换目录重复启动。P7 与实际释放事务均要求这些工具的完整闭合原件，关闭时间不得早于原冻结提交；业务恢复审核仍独立核 SQL 结果，工具退出或闭合不能代替业务审核。
- P8 每个 boot 都必须恰有一个 pin 的 SHA256 等于 ConsumerReadyFrontierDigest。它指向正式 ConsumerReadiness 的原字节；无消费者也必须在真实入口准备后报告显式空组。消费组按真实 generation/member 与完整配置目录聚合各副本 assignment，并集等于配置分区全集；P9 在同消费 owner 锁或线程内复核当前状态并开放 SOURCE。
- C++ 新命令流使用 assign(node_id % P)，不参加消费组，也不写 broker committed。它保持原 target_instance_id 防僵尸语义：P0 记录真实手动 assignment/物理 topic UUID 与位置；P3 poll 到固定尾部后停止并 join，P4 用本窗 leader 实读 end 核其位置。手动分区原件不伪填 generation 或 committed；允许不同 node 同读一个分区。新 boot 的 node_id 可重新分配，P8 按稳定 SOURCE、原 topic UUID/P 核新 node_id % P 与新 instance，实际初始化时读 end 并固定绝对起点后暂停；P8 保证 start == position 且 earliest <= position <= 当前 end，允许暂停期间新增消息。P9 从这个原起点继续，不重新 seek 到已增长的新 end。legacy per-node topic/group 若新 ID 不属于 P0 固定目录则 HOLD；完整采用新命令流的部署应在 P0 固定既有 DisableLegacyPerNodeTopic 配置。
- 不改写 IssuerClosure 的 P0 SourceEvidence。P3 新生成的 manual-closure.report.json 由 NonSqlClosureEvidence 的原件 pin 引用，在固定 boot 私有目录独占创建；P4 leader 原件分别保留采样时刻，不把 P3 时间伪作 P4 观察。P3 relay 前沿包含真实 manual 报告摘要。
- Kafka 手动批处理接口的 ProcessingNotCommitted 只接收已证明未提交的失败，保留原 T/P/O 票据并由同 reader/owner 跨代重放，实际 commit 后才结清。ProcessingOutcomeUnknown 永久 HOLD。调用方不能以重建 group 丢弃旧票据，也不能为提交再复制大 payload。
- SQL inbox 的持久 PENDING/DELIVERED 是待办事实，不是本机未返回调用。P3 等已接收 RPC、轮次、通知 CAS 与资源实际结束，待办留在冻结库供新 boot 幂等续做。Redis PubSub 去重若仅抑制优化唤醒且 SQL ListDue 定时兜底，不被虚构成持久消费组；其线程和回调仍必须真实 join。
- 普通常态工具保留 NORMAL 权限与既有业务门禁，只读命令使用 Reader。有限工具无 run 时的正常退役使用既有非 run RETIRE：先真实结束并关闭资源，再由独立 Operator 原子退役；如 P0 已经建立，必须转同 run 的闭合证据或 HOLD。Seal 只关闭本机 SQL 能力，不能冒充目录 RETIRED。该启动器已在候选脚本中接入固定二进制/参数、真实退出与原请求 RETIRE；仍须实际执行验证。有限工具若绑定 SQL_RELEASED runtime 必须拒绝，不能绕过 prepared/open 流程立即发起业务。

- P7 的合服与路由恢复审核分别使用正式 MergeRecoveryAudit / RouteRecoveryAudit，引用原冻结、固定维护闭合、实际业务查询来源与原始数据；合服始终核完整 14 项当前 SQL/缓存检查，RULE 不替代 OBSERVATION。路由保留 frozen-recovery 的 db_only_unpublished 语义，其他差异须为零；可信缓存标记与水位原件决定 RouteWatermarkFloorMs，不能手填数值或以工具闭合代替审核。
- 消费者从未启动且已确定关闭时，延迟到达的 Run 返回确定成功且保持 Started=false，不打开网络资源；重复启动仍拒绝，防止调用者把零工作终局误记成 UNKNOWN。Kafka delivery callback 的本机活动票据覆盖通知函数及其目标对象析构，只有真正返回后才满足 P1/P3 静止条件。

- Redis 维护 nonce 由同 run 固定键 `sql:writer:maintenance:<run_id 小写十六进制>:nonce` 承载，值为 P0 固定的 16 字节 nonce 小写十六进制，类型必须是 string、PTTL 必须为 -1。P0 在持久独占 attempt 文件之后只执行一次 SET NX；已有 attempt 只允许读回，缺键或 FLUSHDB 后不能重新播种。每个实际 address/DB 恰有一份 BASELINE_V1 原件进 Plan.SourcePins；P4 的 SCAN_V2 在实际扫描前后读取身份、nonce 与 PTTL，并把原件时间窗纳入同轮观察。不能用输入 nonce 回填或旧恢复标记代替实际观察。
- 路由 P7 使用单次 Redis Lua 同时读取 INFO 身份、nonce、ready marker 和 watermark，审核前后的身份、nonce 与标记原件必须一致；SQL 前后也比较完整 run/meta 与原冻结收据，不能只分别接受 FROZEN/RELEASE_READY 阶段。审核来源固定为 P0 已钉的四份实际查询源码。大二进制不作为 16 MiB 证据正文读取；正规构建与实际启动产物的 SHA 由执行记录另行绑定，P7 来源 pin 本身不证明源码与二进制对应。

**上线状态（负责人 2026-10-18 确认：尚未上线）**

没有存量 zone，也没有在跑的旧版 go/db / scene / login，因此下面几条在上线前都不是阻塞项，也不需要迁移或灰度方案：

- **legacy 与 dual 混跑**（§8.4 更正）、**dual → legacy 降级**（§8.6 更正）：没有需要降级或与之混跑的旧部署。
- **不认识门禁的旧 go/db 二进制靠发布纪律排除**：没有旧二进制可排除。
- **回档 `-Apply` 在 #3 裁决前不可用**（`k8s_zone_rollback.ps1`）：没有可回档的线上 zone。

首次上线直接以「go/db `dual` + scene / login 严格提交屏障」整批起步，缺省配置即可正常登录。上面这些机制保留，供上线之后的回退、混跑与回档使用；届时再按 §8.3、§8.6 的门禁执行。上线后本段作废，改以当时的线上状态为准。
