# zone Redis 回退防护：「Redis 回退一族」6 条 low 的合并设计

> **2026-10-23 初始持久基底后继（待正式生成与运行验证）**：正式 Create 目录与双 SQL 初始行同一 TiDB 事务严格 INSERT，持久 marker 只在首次成功写入；保留无 actor 初始提交 `(1,1)`，首任 actor 由 SceneManager 在采纳地板后铸到 2。完整合同与免冷读种子的提交后顺序见[建角初始持久提交](new-player-first-enter-no-cold-read.md#建角初始持久提交)。空 SQL 或零序号旧缓存不能证明新号，任一真实玩家行缺失均拒绝。

- **2026-10-24 方向变更（先读）**：用户 10-24 明确「玩家数据不要 redis 存储，其他改用的地方还是要用」，见 [TiDB 决策 §9](global-data-layer-tidb-decision.md)。本稿里只服务于 Redis 父快照与资产证据的部分（父快照回退检测、`verified_identity` 实例身份核验、资产放弃的 G5 闸读 Redis 副本）随新存盘路径重写，在途落码开工前先向用户确认是否还做；owner_epoch 铸造等协调键的防回退不受影响。只定了方向，未设计、未落码。
- 日期：2026-10-22
- 状态：**2026-10-23 起落码中，未编译、未测试**。负责人 2026-10-22 授权「按最标准的方式做完」，§13 各项按推荐取；项目未上线、玩家数据可以完全清空，§6 的混版与回退兼容不再实现。编译、测试、压测按 AGENTS §10.1 交 Codex。落码相对本稿的取舍见 §14。
- 来源：[数据一致性等级核对](data-consistency-guarantee-map-20261020.md) §2.2 的 6 条 low（player-save#1、identity-routing-redis-v#2、cross-service-assets-v#1、player-save#2 / identity-routing-redis-v#1、identity-routing-redis#1）。
- 关联：[存盘围栏](db-write-fencing-mysql.md) §9.1（B1）与 §14 第 7、15 条；[再入屏障与 owner_epoch](scene-owner-reentry-barrier.md)；[共享 Redis 按用途拆分](shared-redis-split-by-purpose.md)；`deploy/k8s/manifests/infra/redis-ha.yaml`；路由就绪标记的身份判据 `go/shared/placement/route_ready.go`。
- 取证方式：只读源码，未运行任何命令。行号以 2026-10-22 工作区为准。

## 0. 结论速览

**根因一句话**：zone 核心 Redis 同时是玩家存档的**加载真源**和 owner_epoch 的**唯一存放处**，但系统既没有给铸造设持久下限，也没有任何判据回答「这份 Redis 副本有没有回退过」。

**三处需要先更正的判断：**

1. **铸造地板堵不住「新 epoch 越过 SQL 新行」。** 不论地板取 SQL `fence_owner_epoch` 还是墙钟，新铸的 epoch 都大于 SQL 里的旧 epoch。新持有者本来就有权越过 SQL 行，错在它载入的是回退后的旧内容。这一条只能在**加载侧**堵（§4.1）。核对稿 §2.2「合并修复方向」第 1 条说它「能堵住新 epoch 越过 SQL 新行」，这个说法不成立；它只能堵住重铸同值。
2. **C++ 存盘 Lua 不能加 WAITAOF。** C++ 玩家存盘走 hiredis 单连接异步管线（`redis_client.h`）。appendfsync everysec 下，WAITAOF 要等下一次刷盘，按源码推断最长约 1 秒。这段时间整条连接都被阻塞，全节点的存盘吞吐会塌掉。WAITAOF 只适合加在有专用连接、频率低的 Go 侧铸造上（§4.3）。
3. **全仓用 WAITAOF 的不止路由发布。** go/db 的重试、死信入队（`durable_enqueue.go`，`go/db/internal/config/config.go:318-342`）也用了。核对稿 §0 那句「全仓只有路由发布用了 WAITAOF」应改为「玩家存档链路上只有 go/db 的重试入队用了」。

**方案：五件**

| 件 | 做什么 | 堵哪几条 | 改哪里 |
|---|---|---|---|
| G1 | 登录加载带 **Redis 实例身份戳**。身份变了，先去冷路径和 SQL 行围栏交叉校验；SQL 严格更新时**自动采纳 SQL** | player-save#1（两种结局）、identity-routing-redis-v#2、cross-service-assets-v#1 | go/login、go/shared/savebarrier |
| G2 | **ACK > pending** 当作回退证据：go/db 发布 ACK 时计数、告警，login 遇到就走 G1 的采纳路径 | 第 4 问；player-save#1 主结局的检测 | go/db、go/login |
| G3a | 落实已定的**墙钟地板**（§14 第 7 条）：新 epoch = `max(当前 + 1, 调用方毫秒)` | identity-routing-redis#1（重铸同值） | go/scene_manager |
| G3b | 铸造后先等 **WAITAOF** 确认，再发路由事件 | player-save#2 / identity-routing-redis-v#1（铸造丢失，现任持有者被当作废黜） | go/scene_manager、部署 |
| G4 | C++ 存盘被拒时细分出「计数器比缓存小」，**只检测、不修复** | 让上一条残余时可观测 | C++ |
| G5 | 资产**自动放弃**判「确定未见」之前，要求 Redis 副本已经过当前实例身份核验 | cross-service-assets-v#1 的同族（放弃会造成复制或丢失） | go/shared/assetop |

**建议分批**（§11）：
- P1 = G1 + G2 + G5，纯 Go，不改协议。
- P2 = G3a + G4 检测码，没有性能代价。
- P3 = G3b，有延迟代价，必须先拿到压测证据（AGENTS §6）。

P1 + P2 做完之后，回退**不再造成覆盖或永久拒登**，残余只剩「铸造丢失时丢 ≤300s 进度」。P3 把这条残余也消掉。

**Java 版不同构**（§12）：归属 epoch 和存档加载真源都在 MySQL，Redis 只存位置投影。

## 1. 范围与前提

### 1.1 回退从哪来

| 来源 | 丢多少 | 实例身份会不会变 |
|---|---|---|
| 进程崩溃 / OOM / 节点失联，AOF everysec | 约 1s（最坏约 2s） | 会（run_id 每次启动都换） |
| 哨兵切主（异步复制） | 通常毫秒级。`min-replicas-max-lag 10` 允许从库落后 ≤10s 时仍接受写入，理论上限是 10s | 会（被提升的从库换 master_replid） |
| 从旧备份恢复 | 任意长 | 会 |
| 正常 SHUTDOWN 重启 | 0（关闭时会 fsync AOF） | **会**：G1 会保守地多做一次冷读，见 §4.1.6 |
| `FLUSHALL` / 手工删键 | 整键消失 | **不会**：键没了会走现有缺键路径，见 §4.1.4 V6/V7 |
| `DEBUG RELOAD` 原地换数据 | 任意 | 不会。两份清单都已用 `--enable-debug-command no` 堵住（`redis-ha.yaml:282`、`redis.yaml:137-139`），沿用路由就绪标记的同一前提 |

「zone Redis」指核心 `redis` StatefulSet：dev 是单实例，其他档位是 1 主 2 从加哨兵（`redis-ha.yaml:1-5`）。玩家父键、owner、屏障、location、handoff 都在这一个实例上。这是 `redis_client.h:66` 与 `parent_commit_barrier.go` 头注释写明的部署前提。另一份 [Redis 水平扩展需求交接](../handoff/redis-horizontal-scaling-requirements-20261022.md)正在评估分片；本稿**不新增**跨键原子约束（§4.1.5）。

### 1.2 不在范围

- MySQL/TiDB 回档（PITR）：见 §9.2 的 runbook。G1 遇到「SQL 比 Redis 旧」时 fail-closed（V5），不处置。
- Kafka 丢消息、补投耗尽：见 L-04。
- node_id 复用和 etcd 单边分区下的双写者：核对稿 §2.3。
- 帮会通道、收件箱、本地战斗结算把「写进 Redis」当作落盘证据（已登记的 low）。G1 会收窄这条（§3 最后一段），但不根治。

### 1.3 上线状态

负责人 2026-10-18 确认项目**尚未上线**。没有存量数据要迁移；混版只需要保证滚动发布期间不出错（§6）。

## 2. 现状机制（取证）

| 环节 | 现状 | 位置 |
|---|---|---|
| 铸造 | 等值 CAS 之后普通 `INCR`，没有下限。重放识别依赖「恰好 +1」 | `go/scene_manager/internal/logic/owner_epoch.go:133-155`（INCR 在 :150） |
| login 快路径 | `inspect()` 只比对 Redis 内部的 pending 和两张表的 ACK（要求相等）。父键在就返回，不读 SQL | `go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go` 的 `inspect`；`ensure_player_all_data_async.go:84-87` |
| login 冷路径 | 父键缺失时读两张 SQL 行，要求行内围栏 == pending。pending 为空、SQL 有历史时报「需要停写恢复」 | `parent_commit_barrier.go` 的 `validateSQLSub` |
| go/db 行围栏 | `epoch > 库内` 走 default 分支整行写，只拦「比库里旧」的版本 | `go/db/internal/kafka/player_row_fence.go:166-189` |
| ACK 发布 | `writeBarrier = not old or version_gt(新, 旧ACK)`，不和 pending 比 | `go/db/internal/kafka/player_cache_publish.go:41-47` |
| 补投 | 两表 ACK 都等于 pending 才清意图并 ZREM；意图缺失返回 -1，按 RecheckAfter 复查 | `go/db/internal/saveoutbox/scripts.go` 的 `claimScript` |
| C++ 存盘 CAS | `owner ~= ARGV[2]` 一律返回 0，不区分大小 | `cpp/libs/engine/infra/storage/redis_client/redis_client.h:182` |
| C++ 被拒处置 | 被拒值等于实体当前缓存时，按废黜处理：`DestroyDeposedPlayer`，不存盘 | `cpp/libs/services/scene/player/system/player_lifecycle.cpp:4644-4708` |
| go/db 计数器倒退探测 | 只判「计数器 < 已落库 epoch」 | `player_row_fence.go:215-243` |
| 资产自动放弃 | 条件 5「确定未见」读 Redis 父键 blob；开关打开时再加 Redis 里的 SQL 证据键。两份都是 Redis 副本 | `go/shared/assetop/abandon.go:352`（`judgePlayerBlob`）、`:388`（`judgeWithSQLCommit`） |
| 身份判据先例 | 同一段 Lua 里取 `INFO server` 的 run_id 和 `INFO replication` 的 role / master_replid，与标记里记下的身份比较 | `go/shared/placement/route_ready.go:186-200` |

## 3. 逐条时序与归属

记号：版本 `V = (E, s)`，按 41 字节定长串比较（`savebarrier.Version`）。

| 编号 | 时序要点 | 现状结局 | 谁来堵 | 堵住后的结局 |
|---|---|---|---|---|
| player-save#1 主结局 | 存盘 V_new 写进 Redis，DBTask 发出 → Redis 丢尾，pending 回到 V_old → go/db 落 SQL，把 ACK 推到 V_new | ACK > pending，login 永久报 `not committed`。意图早已清掉，补投与告警都不触发 | G2 检测，G1 采纳 | go/db 计数告警；下次登录采纳 SQL 的 V_new，正常进场 |
| player-save#1 次结局 | 同上，但 ACK 也一起丢了 → 快路径载入 V_old → 铸出 E+1 → 第一次存盘越过 SQL 里的 V_new | 静默回档 | G1 | 身份变了，走交叉校验；SQL 两行都是 V_new，严格新于 pending → 采纳 |
| identity-routing-redis-v#2 | 丢尾窗口里退出的玩家：实体已销毁，ACK 领先 pending；意图若还在，补投重放 V_old，被判 writeOlder 后转终态 | 永久锁登录，要人工跑 `save_barrier_restore` | G1 + G2 | 同上，登录时自动采纳 |
| cross-service-assets-v#1 | 邮件、交易、商城以 SQL 证据终结；次结局的覆盖把玩家侧退回 V_old | 外部已终结、玩家侧回退（丢失或复制） | G1；G5 堵放弃路径 | 不再覆盖。放弃路径对未核验副本 fail-closed |
| player-save#2 / identity-routing-redis-v#1 | 丢的是铸造：B 缓存 E+1，Redis 回到 E → B 下一次存盘被等值 CAS 拒，按废黜销毁 | 丢 ≤300s 进度 | G3b 消除；G4 让残余可观测 | 铸造先落盘再发路由事件，持有者不会拿到会丢的 epoch |
| identity-routing-redis#1 | 铸造丢失后，源 A 遗留的交接标记仍在 TTL 内；B 的第一跳被放行，重铸出同值 N+1 给 C，同 epoch 双持有者 | 可能旧盖新，零告警 | G3a | 重铸取墙钟，值必然不同。B 的存盘被判废黜，只丢不盖 |

**三点补充推导：**

- **交接路径不需要 G1。** 源端冻结存盘、写标记、SM 铸造，这三步在 AOF 里有先后。铸造留下来，就说明它之前的存盘和标记也留下来了。铸造丢了，目标节点的存盘过不了 CAS，只丢不盖。所以交接路径的「载入旧内容」只会表现为铸造丢失（G3b）。登录路径不一样：载入和铸造之间隔着整段回退前的历史，必须 G1 来堵。
- **G3b 的确认覆盖它之前的所有写。** WAITAOF 确认的是一个全局偏移：AOF 和复制流都是单一有序日志，偏移 X 已刷盘，意味着 X 之前所有连接的写都已刷盘。所以铸造确认落盘时，源端的冻结存盘、G1 采纳写入的父键也一起落盘了。
- **把「写进 Redis」当作落盘证据的通道（已登记 low）。** 只要那次存盘的 DBTask 已经发出、最终落进 SQL，G1 采纳 SQL 后玩家侧就能追回。只有「Redis 丢尾，同时 DBTask 发送也失败」才会真正丢。

## 4. 设计

### 4.1 G1：加载侧身份戳 + 交叉校验 + 自动采纳

#### 4.1.1 身份与戳

- **实例身份** = `INFO server` 的 `run_id` + `INFO replication` 的 `master_replid`，且 `role` 必须是 `master`。口径与 `placement.ParseRedisInstanceIdentity` 相同。
  - 两段 INFO 在 Lua 里用 `string.match` 取字段（同 `RouteReadyReadScript`）。抽成共用片段放进 go/shared，**不复制第二份**。
  - 未核实：Redis 7.2 / 8.x 是否允许脚本调 INFO。`route_ready.go:184` 记了同一个未核实项。若不允许，退回「脚本前后各读一次 INFO、两次相同才认」的做法，即路由对账的 `identity_stable` 口径。
- **戳**：在 `player:{id}:save_barrier` hash 里新增字段 `verified_identity`，值为 `<run_id>:<master_replid>`（81 字节）。
- **只有两个写入方**：
  - login 的核验发布脚本；
  - `save_barrier_restore`（人工恢复完成时）。
- **C++ 存盘、go/db ACK 发布、补投脚本都不写戳。**
  - go/db 若写戳，会把一份回退过、尚未核验的副本误盖成可信。主结局里 go/db 正是在回退后的副本上推 ACK。
  - C++ 写戳要改热路径 Lua，还要每次存盘调 INFO；收益只是「身份变化后少做一次冷读」。不值得（§10）。
- 其他读者都按字段名 HGET / HMGET，不受新字段影响：
  - C++ 严格屏障 Lua；
  - `claimScript`；
  - 合服核对 `tools/merge_zone/save_barrier_verify.go`（:18 写明只按 pending 和两表 ACK 判，不按字段集合判）；
  - 恢复工具。

**为什么「戳 == 当前身份」就能信快路径**：Redis 只会在崩溃、切主、恢复时丢已确认的写，而这些事件都会换身份。戳是在身份 X 下核验后写的；之后只要身份一直是 X，就没有任何已确认的写丢失过。核验时 SQL ≤ Redis 成立，之后的每一步写入都还在，所以这个关系继续成立。

- 戳本身也可能随回退丢掉或回到旧值，但那时身份已经变了，比较必然不等，走核验路径。
- 身份值是随机 40 位十六进制，不同实例之间不会撞。
- 把 hash 拷到别的实例（未来拆分、合服）时，戳会对不上，同样走核验。这正是我们要的行为。

#### 4.1.2 快路径条件

现有 `beginParentLoad` 的全部校验都通过，**并且**戳 == 本次 Lua 读到的身份，**并且** role == master，才走快路径。三条缺任何一条都转核验路径（§4.1.3）。

`beginParentLoadScript` 多返回三个值：本实例身份、role、戳原文。

#### 4.1.3 核验路径

1. 发两张 SQL 行的读任务。复用冷路径机制，**不采信子表缓存**（`validateSQLSub` 的现有口径）。
2. ~~同时取资产 SQL 证据的版本作为第三个证人~~（落码时取消，见 §14：证据与 player_database 的 ACK 在 go/db 同一段 Lua、同一版本原子写入，不提供额外信息）。
3. 按 §4.1.4 判定。
4. 判为一致或采纳时，执行**核验发布脚本**（新）：
   - 原子复核：owner、pending、两表 ACK 的原文与第 1 步读到的一致；INFO 身份仍是本次读到的身份；role 是 master。
   - 任何一项变了，返回冲突，按可重试失败处理（与现有 `publishParentLoadScript` 的复核同一模式）。
5. 采纳成功后，本次登录按快路径继续：父键已经是 SQL 的内容。

判定逻辑写成**纯函数**，放进 `go/shared/savebarrier`：输入 pending、两表 ACK、两张 SQL 行围栏、证据版本，输出判定。login 调用它；合服核对和恢复工具以后可以复用。业务知识只留一份（DRY）。

#### 4.1.4 判定表

记 `P` = pending，`A1` / `A2` = 两表 ACK，`S1` / `S2` = 两张 SQL 行的行内围栏 `(fence_owner_epoch, fence_save_seq)`，`C` = 证据版本（缺省按最小值）。

| 判定 | 条件 | 动作 | 返回 |
|---|---|---|---|
| V1 一致 | `S1 == S2 == P == A1 == A2`，且 `C ≤ P` | 只写戳，不动父键 | 成功，载入 Redis 父键 |
| V2 Redis 落后（回退证据） | `S1 == S2 == Vs`，`Vs > P`（P 缺失也算），`Vs ≥ A1、A2`，且 `C ≤ Vs` | **采纳**（§4.1.5） | 成功，载入采纳后的父键 |
| V3 在途 | `P > min(S1, S2)`，且 `A1、A2 ≤ P` | 不写戳 | 现有「not committed」可重试失败，补投会追平 |
| V4 两行不一致 | `S1 ≠ S2`，且 `max(S1, S2) > P` | 不写 | 新的可重试失败 `barrier_reconciling`：另一张表的任务多半还在 Kafka 积压或重试队列里 |
| V5 SQL 落后于证据 | `max(A1, A2, C) > max(S1, S2)`，或 Redis 有历史而 SQL 行缺失 | 不写，critical | fail-closed：SQL 侧回档或损坏，走 §9.2 |
| V6 无历史 | P 空、owner 为 0、父键是建角种子，两张 SQL 行为空 | 只写戳 | 成功 |
| V6′ 首存候选形状（r2-login#1，2026-10-23 安全裁决） | P 空、owner 已铸、父键是建角种子或缺失，两张 SQL 行为空或缺失 | 拒绝 historyGap，不补空父键、不写身份戳、不降低 owner | 空 SQL 不能证明从未存盘。新的建角链必须先完成真实初始 SQL 提交，按下述初始持久基底与既有 Reconcile 恢复；历史缺行不能借新号旁路放行 |
| V7 P 空，SQL 有历史 | P 空，`S1 == S2` 非零 | 按 V2 采纳 | 成功。今天这种情形报「需要停写恢复」；它正是回退越过首次存盘、或整键丢失的样子 |

V2 / V7 自动采纳的依据：
- 采纳的内容来自**已提交**的 SQL 两行，且两行同版本，严格新于 Redis 记录的一切版本。
- 这就是 §9.1 第 a 步「定真源」在这种无歧义情形下的机械化。

有歧义的情形（V4、V5）一律不自动处理。

> **2026-10-22 修复（r2-mz#1 评审跟进，未编译、未测试，待 Codex 验证）**：合服工具 `tools/merge_zone` 的 `-verify-merged` 有一行 `verify:save_barrier`，在合服 Step 5 按本表预判每名玩家开服后的第一次冷加载（独立 go.mod，镜像 `savebarrier.Reconcile` 而不引用它）。两份判定共用样例表 `go/shared/savebarrier/testdata/merge_copy_cold_load_cases.json`，login 一侧由 `merge_copy_cold_load_test.go` 走真实冷加载入口对表。**改本表或 `Reconcile` 时同步改样例表**，否则 merge_zone 或 login 的单测会变红。细节见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md)「2026-10-22 修复（r2-mz#1 评审跟进）」。

#### 4.1.5 采纳脚本

KEYS 与 `parentBarrierKeys` 相同：父键、owner、屏障，**不新增**跨键原子范围。在复核通过之后：

1. `SET` 父键 = SQL 两行组装出的 PlayerAllData，`LastSaveSeq = Vs.s`，TTL 同现有发布。
   - 发布前先跑一遍 C4（`checkParentSchemaRegression`），按现有 warn / enforce 模式处理。
2. `HSET pending = A1 = A2 = Vs`，`verified_identity = 本次身份`。
   - `schema_gen` 只升不降。
   - `schema_writer` 不动：旧令牌永远不等于新实体，C3 会照常比较。
3. `HDEL` outbox 意图字段。判 V2 时 `outbox_version` 只可能 ≤ P < Vs；如果脚本发现 `outbox_version > Vs`，说明判定前提已被推翻，返回冲突。
   - 索引 ZSET 成员不动：下一次 `claimScript` 见到 complete，会自己 HDEL + ZREM。
4. owner 只升不降：`owner < Vs.E` 时 `SET owner = Vs.E`。这是 `beginParentLoad` 要求的「owner ≥ pending 的代次」。
   - **只抬到 Vs.E，不加余量**。仍持有更高缓存值的节点（铸造丢失的那种）状态不变，不会被这次采纳额外废黜。
   - 下一次铸造由 G3a 取墙钟，自然越过一切在途 epoch（§9.1 的 K 不再需要）。
5. 脚本之外尽力而为：DEL 两个子表缓存 `{MsgType}:{id}`。它们不是权威，失败只计数。放在脚本外，是为了不扩大原子键集合。

#### 4.1.6 代价与残余

**代价：**
- 每次 Redis 身份变化之后，每名玩家的下一次登录多一次冷读（两个 Kafka 读任务）。规模等于父键 TTL 过期后本来就要走的冷读，但会**集中**在切主或重启之后。必须压测（§8.3 R-a）。
- 快路径每次多两次 INFO 调用：输出几 KB，在 `cache_check` 阶段。压测时看这一阶段有没有变化。
- 正常 SHUTDOWN 也会换 run_id，也会触发一轮冷读。这是保守代价，换来的是不需要区分「干净重启」和「崩溃」。后者在 Redis 外部无法可靠判断。

**残余：**
- **R-G1-1**：采纳之后，Kafka 积压里如果还有一笔比 Vs 更新的任务 Vk，它晚到时会照常写进 SQL（Vk > Vs），并把 ACK 推高于 P。这笔 ACK 只会触发 G2 告警，不会让刚核验过的戳失效（戳 == 当前身份时快路径不看 SQL 版本）。
  - 新持有者如果在此之前已经进场，它的存盘按 `(E_new, s)` 覆盖 Vk 的增量。
  - 这和冷登录今天就有的残余同源（§6.2 残余窗口 3），由 §14 第 15 条已定、**尚未落码**的「go/db 待落写标记 + 读任务快速失败」关闭。
  - 触发条件：回退恰好丢了 `P = Vk`，同时 Vk 的任务还在积压里，且玩家在积压消化之前重新登录。
  - 上线前门禁：第 15 条落码，或负责人接受这条残余。
- **R-G1-2**：身份读不到（INFO 被禁止或格式异常）时一律走核验路径。核验路径本身失败就 fail-closed，登录失败、可重试，不会退回旧的快路径。

### 4.2 G2：ACK > pending

**不变量 I-ACK**：同一屏障 hash 里，每张表的 ACK ≤ pending。

go/db 只在 SQL 提交了版本 V 之后才发布 `ACK = V`，而 V 来自 C++ 屏障 Lua 先写下的 `P = V`，P 又只增不减（`redis_client.h:183-184` 拒绝倒退）。所以 `ACK > P` 只可能来自 Redis 回退、手工改写，或合服把屏障拷错。

- **go/db 发布脚本**（`player_cache_publish.go`）：
  - 在 `writeBarrier` 判定之后加一条：pending 存在且新版本 > pending，或 pending 不存在，仍然写 ACK。它是真实的 SQL 事实，也是 G1 需要的证据。
  - 同时在回复里带回 `ack_ahead` 标志，计 `db_save_barrier_ack_ahead_total`（不带 player_id），日志 `SAVE-BARRIER-ACK-AHEAD playerID=… ack=… pending=…`。
  - 回复形状变化随同一个二进制一起发布（脚本按 SHA 加载），项目未上线，不另做兼容。
- **login `inspect`**：
  - `ACK > P` 从「not committed」里拆出来，新报错 `parent SQL barrier ahead of pending`；
  - 「P 缺失但 ACK 存在」（现报 `parent pending barrier missing`）同样处理；
  - 两者都**无视戳、直接转核验路径**。它们本身就是回退证据。
- **离线不再登录的玩家**不需要后台修复：除资产放弃（G5 拦住）外，没有人读他们的父键；人工路径 `save_barrier_restore` 保留。
  - 后台全量对账扫描列为后续项，只在 G5 的跳过计数持续增长时再做（YAGNI）。

### 4.3 G3：铸造持久化

#### 4.3.1 G3a：墙钟地板（落实 §14 第 7 条）

- `luaMintEpochAndSetLocation` 加 `ARGV` 地板毫秒，由 SM 传入 `time.Now().UnixMilli()`。`minted = max(cur + 1, floor)` 写成十进制串比较，**不经 Lua double**。
- **重放识别**：把「`cur == ARGV[1] + 1`」改成「`cur == expected`」，其中 `expected = max(ARGV[1] + 1, floor)`，在 Lua 里用同一组 ARGV 算出。重发的 EVAL 参数完全相同，算出的值也相同，所以幂等识别不变。
  - 本请求的落点已被 bump 回滚后（`cur == expected + 1`），原样重发仍回 0。这条性质不变。
- 回滚 bump 仍是 `INCR`，「after == ARGV[3] + 1」的交叉校验不变。
- 取值范围：当前毫秒约 1.8e12，远低于 2^53。`decodeMintReply` 负值区间不变（新值 ≥ 2）。
- C++ `PlayerOwnerEpochComp.epoch` 是 `uint64_t`（`player_ownership_comp.h:103`）；`DBTask.owner_epoch` 是 uint64（`db_task.proto:26`）；屏障版本是 20 位定长，都放得下。
- 落码前要 grep 一遍对 epoch 数值大小的隐含假设：格式化宽度、uint32 截断、`epoch==0` 特判。
- **保证**：回退之后的第一次铸造大于回退前的一切铸造，前提是 SM 各副本之间的时钟偏差小于「回退前最后一次铸造」到「回退后第一次铸造」的间隔。切主恢复至少要秒级，实际总能满足。
  - 时钟反向跳变超过这段间隔时，`max` 退化为 `cur + 1`，不比今天差。
  - SM 主机纳入 §14 第 13 条已定的「主机间时钟偏差监控」。
- **不保证**：铸造丢失后现任持有者被废黜（G3b）；载入旧内容（G1）。
- `go/shared/ownerepoch/ownerepoch.go` 包注释和本节关联文档里「只经 INCR 前进」的表述，落码时同步改为「只升不降：铸造取 max(当前+1, 墙钟)，回滚 INCR」。

#### 4.3.2 G3b：铸造后 WAITAOF

- SM 新建一个**专用** go-redis 客户端，模式同 `go/data_service/internal/routing/publish_redis.go` 和 go/db 的 `durable_enqueue.go`。
  - 同一条连接、同一个 pipeline 里：铸造 `EVAL` 之后紧跟 `WAITAOF numlocal numreplicas timeout`。
  - 副本数按拓扑渲染：单实例 `1 0`，HA `1 1`。沿用 `k8s_deploy.ps1` 的 `Get-RouteStoreWaitAOFNumReplicas`，扩到 scene_manager 的 ConfigMap。
- **顺序**：铸造 → 确认落盘 → 推路由 / 重定向。
- **确认不足或超时**：按「推送失败」处理。走现有单调回滚（bump），给客户端回可重试错误。
  - 理由：这个 epoch 在 Redis 内存里已经可见，但还没落盘，不能交给目标节点去缓存。
  - 回滚本身也没有确认落盘。如果回滚丢了而铸造留下，终态等于今天的「推送失败 + 回滚失败」，沿用现有处置。AOF 是单一有序日志，回滚留下则铸造必然留下，不存在「回滚在、铸造不在」的组合。
- **覆盖哪些调用**：`luaMintEpochAndSetLocation` 的全部调用点（首次落点、跨节点交接、跨 zone、等待落点的第二条腿）。不铸造的 `luaSetLocationIfEpoch` 和回滚不加。
- **加了之后还会丢吗**：
  - 单实例 `numlocal=1`：扛得住进程崩溃，扛不住磁盘损坏。
  - HA `numreplicas=1`：扛得住主节点 Pod 丢失。哨兵在可达的从库里选复制偏移最大的提升；确认过的那个从库若也同时丢了，三个哨兵和 Redis 同 Pod，只剩一个，凑不够 2/3 多数，**不会自动切主**（不可用，但不回退）。
  - 残余：`redis-start.sh` 在「没有哨兵能指认主」时按约定让 redis-0 当主（`redis-ha.yaml:233`），这可能把落后的节点拉起来当主。此时身份已变，G1 兜加载、G3a 兜重铸、G4 让持有者被拒可观测。
- **代价**（未实测，不得据此宣称结论）：everysec 下 WAITAOF 要等下一次后台刷盘，按源码阅读推断平均约 0.5s、最坏约 1s；HA 还要加从库刷盘。现有 WAITAOF 调用的超时缺省都是 1500ms（go/db、data_service），与此一致。
  - 只在持有者变化时付出：首次登录、跨节点交接、跨 zone。同节点换图、同落点重连不铸造，不受影响。
  - 必须按 §8.3 R-b 压测。
- **配置**：`OwnerEpochMint.WaitAOFNumLocal` / `WaitAOFNumReplicas` / `WaitAOFTimeoutMs`，口径同 go/db（`config.go:318-342`）：两项都写 0 等于不发 WAITAOF，启动时打一行 ERROR；开启时启动先探一次，失败拒启。
- **连接总账**：在 `k8s_deploy.ps1` 的池表里给 scene_manager 加 Fixed 连接，写法同 db 行的 `Fixed 2`。

#### 4.3.3 G3b 落码定稿（2026-10-24，未编译、未测试，待 Codex 验证）

本节补齐 §4.3.2 落码时定下的细节，同时答复[玩家态 Redis 集群化](redis-player-store-cluster.md) §7 与 §16 第 1 条「铸造后同一节点连接上发 WAITAOF」。G3a 墙钟地板**不在本次**，见本节末。

**落码前的现状**：10-23 的快照提交 `9d2784fbf` 已带上 svc 层（`go/scene_manager/internal/svc/owner_epoch_mint.go`：专用 go-redis 客户端、EVAL 与 WAITAOF 同一 pipeline、启动探针）、配置 `OwnerEpochMint`、指标与告警（`deploy/k8s/owner-epoch-alerts.yaml`），但 logic 没接线：铸造仍走 go-zero 的 `svcCtx.Redis.Eval`，WAITAOF 一次都没发；K8s ConfigMap 不渲染 `OwnerEpochMint`，Redis 连接总账 scene-manager 行 `Fixed = 0`；`owner_epoch.go` 头注释写着 G3a 已生效，与代码不符。本次补齐，改动见 PROGRESS 同日条目。

**同一节点、同一连接**

- 铸造 EVAL 与 `WAITAOF <numlocal> <numreplicas> <timeout>` 放在专用客户端的同一个 pipeline 里（go-redis 一条 pipeline 只占一条连接）。WAITAOF 等的是本连接的 `woff`：Redis `call()` 只在命令产生传播时把它更新为当时的 `master_repl_offset`（`third_party/redis/src/server.c` call 末尾）。换一条连接，或本连接没写过东西，WAITAOF 会立即回满份数，确认不到这次铸造。
- 不用 go-zero 客户端的理由见 `owner_epoch_mint.go` 头注释：按地址共享一个底层客户端、不暴露 Pipelined；WAITAOF 平均占住连接约 0.5s，会饿死共享池；熔断器把确定性的脚本错误也计成失败。
- `MaxRetries=-1`：go-redis 在 WAITAOF 读失败时会把整条 pipeline 连同已生效的 EVAL 重发一遍。重发只由 `EvalAndConfirm` 在**EVAL 结果未知**（传输错误）时做，最多 3 次，10–50ms 抖动退避。EVAL 有回复之后不再重发，WAITAOF 的结论如实上报。
- **重放识别分支写回原值**：重发走新连接。凭标记的铸造被脚本认出「本请求已生效」（返回 `-epoch`）时，若只读不写，新连接的 `woff` 是 0，WAITAOF 立即满足，首发那次铸造的落盘其实没确认。所以脚本在识别分支里 `SET owner_epoch <原值>`：该键没有 TTL，值不变，本连接的偏移越过首发的写，WAITAOF 的确认才覆盖它。CAS 不过（回 0 / -1）的分支不写，此时 WAITAOF 的结论也不使用。

**失败语义**（只针对铸造；不铸造的 CAS 与回滚 Lua 不发 WAITAOF）

| WAITAOF 结论 | Redis 状态 | 处置 | 应答 |
|---|---|---|---|
| `confirmed`：份数满足 | 已推进，已落盘 | 照常推路由 / 重定向 | 0 |
| `not_durable`：份数不足，含服务端等满超时 | 内存里已推进，落盘未知 | **不推**路由 / 重定向；用这次落点走现有单调回滚（bump；同节点 epoch 0 的铸造走 keep）；人数随回滚结果退回 | 7 `ErrKafkaRoute`，回显同推路由失败 |
| `error`：WAITAOF 被拒、回复形状不对、回复读失败 | 同上 | 同上 | 7 |
| 没发送：剩余预算不足 1ms | 一个字节都没写 | 不推路由，退回预占 | 3 `ErrUpdateLocation` |
| EVAL 结果未知，重发 3 次耗尽 | 首发可能已生效 | 同以前 go-zero 自动重发耗尽 | 3 |
| 关闭：两项份数都写 0 | — | 不确认，照常推路由，启动时打 ERROR | 0 |

- **复用 7，不新增码**：C++ 对 7 的处置（源端原子取证，凭回执采纳 N+2；`SmRejectedBeforeAnyPlacementWrite` 不收 7）与这里的终态完全一致，C++ 不用改。3 同样不在「一定没写」名单里，「没发送」回 3 只会少踢、不会误踢。
- **回滚本身不等 WAITAOF**：AOF 与复制流是单一有序日志，回滚留下则铸造必然留下；回滚丢了而铸造留下，等于今天的「推送失败 + 回滚失败」终态。残余：确认不足之后又发生切主，铸造与回滚一起丢失，而源端在丢失之前已经凭回执采纳了 N+2。此时源端缓存 N+2 大于 Redis 的 N，存盘被拒、按废黜销毁（丢 ≤300s，G4 让它可观测）。只有「确认不足」与「切主」叠加时才会出现。
- 不凭标记的铸造（首次落点、等待落点的第二条腿、同节点 epoch 0）遇到「首发结果未知、重发回 0」时仍回 19，这是已登记的残余。现在会打一行 `[OwnerEpochMint] mint EVAL resent after an unknown first result …`，带重发次数。
- 同落点重连、同节点换图下发的是观察值，不铸造也不确认。正常流程里这个值在铸造当时已确认落盘；只有上一条残余、或 G3b 关闭期间铸出的值，才可能未落盘就被下发。

**时间预算**

- 铸造截止 = 请求截止 −（`KafkaWriteTimeoutSeconds` + 300ms），见 `owner_epoch.go` 的 `ownerEpochMintContext`。WAITAOF 实际等待 = min(`WaitAOFTimeoutMs`, 铸造截止前剩余 − 100ms)，先截断成整毫秒，不足 1ms 不发（WAITAOF 的 timeout 0 是无限阻塞）。这样铸造之后的同步推路由（writer 自己的 5s）与回滚 Lua 一定还在 zrpc `Timeout`（8000ms）之内，失败应答回得到 C++ 源端，不会退化成 30s 看门狗。
- 按现值：归属查询很快时等满 `WaitAOFTimeoutMs`；归属查询用满 1500ms 时只剩约 1100ms。`Timeout ≥ 归属查询 + Kafka + 1500` 这条不等式不变：WAITAOF 只用剩下的时间，会被截断，不会越界。
- 专用客户端 `ReadTimeout = WaitAOFTimeoutMs + 1s`，服务端超时先到；取连接的排队同样受铸造截止约束。
- **`WaitAOFTimeoutMs` 取 2000（配置上限），不取 go/db 的 1500**。`numreplicas=1` 时，要等副本自己按 everysec 刷盘，还要等副本上报：副本只在每秒一次的 `replicationCron` 里、或收到主节点 GETACK 时上报已刷盘偏移，而 GETACK 只在有 WAIT / WAITAOF 进来时发一次（`replication.c` `replicationRequestAckFromSlaves`、`replconfCommand` getack 分支）。低负载时，副本刷盘时刻 F 与上报相位 P 近似独立均匀分布于 [0, 1s)：等待 = P（P ≥ F）或 P + 1s（P < F），平均约 1s、最坏近 2s。1500 会让约 1/8 的铸造回 7 [I]。高负载时别的 WAITAOF 带来的 GETACK 会让上报提前。单实例只等本地刷盘（≤ 约 1s），上限调高对它没有影响。

**对进场延迟与吞吐的影响 [I，未实测]**

- 只在持有者变化时付出：首次登录、跨节点交接、跨 zone 第一条腿、等待落点的第二条腿、同节点 epoch 0。同节点换图、同落点重连不铸造，不受影响。
- 单实例：本地确认要等下一次后台 fsync，平均约 0.5s、最坏约 1s。高可用：平均约 1s、最坏近 2s（上一条）。`EnterSceneStageUpdateLoc` 子阶段的耗时含这段等待；`scene_manager_owner_epoch_mint_durable_seconds` 单独给出整条 pipeline 的往返。
- **吞吐上限**：每条连接在 WAITAOF 期间被占住。单副本约 32 条 ÷ 每次占用时长：单实例约 64 次/s，高可用约 32–64 次/s；每 zone 2 个副本，加倍。超出时排队等连接，等待受铸造截止约束，超时回 3（可重试）。登录 ramp 超过这个量级时，**升级路线是组提交**，不是简单调大池（调大池会推高总账、maxclients 与代理 maxconn）：由一个确认协程在粘连连接上，对一批已经拿到回复的铸造做一次「写围栏键 + WAITAOF」，一次 fsync 覆盖整批，写法同 go/db `durable_enqueue.go`。组提交在 Cluster 下要每个主节点一个确认协程，围栏键落在该节点的 slot 上，届时与第 6 批一起设计。
- 是否上组提交、`ownerEpochMintPoolSize` 取多少，以 §8.3 R-b 为准：开、关 G3b 各跑一轮登录 ramp + 跨节点换图，按 AGENTS §6 出对比表。拿到对比表之前，不对延迟下结论。

**三种拓扑下的取值**

| 拓扑 | numlocal | numreplicas | 保证什么 | 代价 / 残余 |
|---|---|---|---|---|
| 单实例（本机 compose；K8s dev 的 `redis.yaml`） | 1 | 0 | 进程崩溃、OOM、重启之后铸造仍在 | 扛不住磁盘损坏；要求 `appendonly yes`（两处都开着），否则启动探针拒启 |
| redis-ha（1 主 2 从 + 哨兵，`redis-ha.yaml`） | 1 | 1 | 主 Pod 丢失：哨兵在可达的副本里选复制偏移最大的提升，确认过的那个副本只要可达就必然当选 | 一个副本宕机仍可铸造（2 选 1）；两个副本都不可达时铸造全部回 7（不可用，但不回退）；`redis-start.sh` 兜底拉 redis-0 当主的残余见 §4.3.2 |
| 未来玩家域 Redis Cluster（[集群化设计](redis-player-store-cluster.md) §7：缺省每主 1 副本，可选 2 副本） | 1 | 1 | 每主 1 副本时它是唯一的提升候选，确认过就必然留在新主上 | 该主的副本宕机或全量同步期间，这 1/M 玩家的首登、换节点全部回 7，直到副本追上（分钟级），在线玩家不受影响。每主 2 副本时 `1 1` 只是「大概率」：集群选举按复制偏移排名错开发起时间，不是硬保证。要硬保证得用 `1 2`，代价是任一副本宕机就拒绝铸造。缺省取 `1 1`，与 §7 的 2 副本选项一起评估 |

K8s 上 numreplicas 由 `tools/scripts/k8s_deploy.ps1` 按共享 Redis 的形态渲染（`Get-RouteStoreWaitAOFNumReplicas`，与 go/db、data_service 同源）；numlocal 与 timeout 原样镜像 `scene_manager_service.yaml`，yaml 里 numreplicas 保持 0 给本机。ConfigMap 改了不会自动滚动：单实例切高可用之后必须 `rollout restart deploy/scene-manager`。

**Cluster 落地时（集群化第 6 批）必须改的三处**。在那之前，专用客户端对 `Redis.Type=cluster` 直接拒启（fail-closed）。

1. **不能用 `ClusterClient.Pipelined`**：它按键分组发送，无键的 WAITAOF 会被发到任意节点，确认的是另一台机器的偏移。要先用 `MasterForKey(ctx, keys[0])` 取到持有该 slot 的主节点 `*Client`，再在它上面发 pipeline。
2. 对 EVAL 而言，MOVED / ASK / TRYAGAIN / CLUSTERDOWN / LOADING 都表示「没执行」：刷新拓扑后到新节点重发（ASK 要在同一 pipeline 里前置 `ASKING`）。这一类重发不计入「结果未知」的 3 次。
3. EVAL 之后、WAITAOF 之前发生切主：WAITAOF 落到已降级的旧主上，回 `cannot be used with replica instances` → `error` → 回滚。回滚 Lua 在新主上做 CAS：铸造随切主丢了就回 superseded、什么都不改（状态已是铸造之前）；没丢就正常回滚。两种终态都自洽。

**G3a 不在本次**：墙钟地板会让铸造不再恰好 +1，而现有两处判据都依赖「恰好 +1」：C++ `travel_outcome::ClassifyOwnership` 采纳回滚要求 `redisEpoch == markEpoch + 2`（`player_lifecycle.h`）；铸造 Lua 的重放识别比的是 `cur == ARGV[1] + 1`。G3a 要与这两处、以及 §4.3.1 列出的数值假设排查同批改，C++ 需要串行编译。svc 层的 `NowMs` 与 `owner_epoch_mint_clock_behind_total` 已经为它留好；`owner_epoch.go` 头注释已改回如实描述。

**测试（本次新增，未运行）**

- logic `owner_epoch_mint_test.go`：确认先于推路由；`not_durable` / `error` 在同 zone 交接、跨 zone 第一条腿、首次落点、同节点 epoch 0 四条路径上的回滚终态与人数；关闭时照常放行；没发送时零写入、回 3；缺铸造通道时 fail-closed；不铸造的 CAS 不经铸造通道；截止预算；识别分支写回原值。
- svc `owner_epoch_mint_test.go`：EVAL 与 WAITAOF 在同一 pipeline；关闭时只发 EVAL；脚本错误不重发；结果未知时整条重发及次数上限；预算不足不发；启动探针；WAITAOF 回复判定、预算、客户端参数三个纯函数。
- `k8s_deploy_contract.tests.ps1` 两条：总账 Fixed 与源码常量一致；ConfigMap 镜像 yaml 并按拓扑渲染 numreplicas。
- 真 Redis 上的 AOF 与切主演练仍按 §8.1 G3b、§8.2 D2。§9 提到的 `k8s_deploy.ps1` 重启提示原文是「在存盘关键写补上 WAITAOF 之前」，C++ 存盘 Lua 至今没加，提示仍然成立，不改。

### 4.4 G4：C++ 存盘被拒细分（只检测）

- 严格屏障 Lua 在 `if owner ~= ARGV[2] then return 0 end`（`redis_client.h:182`）之前，加十进制比较：存储值 < 缓存值时返回新码（例如 4），**仍然不写**。存储值 > 缓存值维持返回 0。owner 缺失维持 error_reply（现有「禁止自动补种」的口径）。
- `HandlePlayerSaveRejected` 收到新码：照旧 `DestroyDeposedPlayer`，原因记 `owner_counter_behind`；`[OwnerEpoch]` 统计行加计数；ERROR 日志带上存储值与缓存值。C++ 节点没有 Prometheus 出口，按现有 LogQL 口径（`metric=`）。
- **为什么不做「前推到缓存值后照写」**（§10 详述）：回退丢掉源端冻结存盘、却留下了铸造时，目标节点载入的是更旧的父键。前推会让这份旧内容以更高的 epoch 写穿 SQL 行围栏，正是要防的旧盖新。持有者在本地分不清自己载入的是新内容还是旧内容，只能 fail-closed。
- 旧二进制自带旧脚本（ScriptSlot 按脚本文本算 SHA），混跑时互不影响。

### 4.5 G5：资产自动放弃闸

- `abandon.go` 的条件 5「已落盘账本确定未见」：开关关闭时只读 Redis 父键 blob（`judgePlayerBlob`）；开关打开时再读 Redis 里的 SQL 证据键（`judgeWithSQLCommit`）。回退之后两份都可能「确定未见」，而 SQL 里其实已经应用。放弃会给对侧退账：道具复制，或者扣款白扣。
- **闸**：在判条件 5 之前，读该玩家屏障的 `verified_identity` 和本实例身份（同一段 Lua 或同一个 pipeline）。不相等就跳过，计 `reason="redis_unverified"`，还租约。fail-closed，符合 AGENTS §11.3。
- 「已见」走正常 Finalize 不受影响：回退只会删掉后来的写，不会凭空造出「已见」。
- **活性代价**：离线超过 180 天、期间又经历过 Redis 身份变化的玩家，放弃会一直跳过，直到他登录（G1 写戳）。这是活性损失，不是正确性问题。若跳过计数持续增长，再做后台对账扫描（§4.2 后续项）。
- 开关：`LoopConfig.RequireVerifiedRedisCopy`，缺省 true。**G1 关闭时戳不会再刷新，G5 会挡住全部放弃**，运维关 G1 时要同时知情。写进两边配置注释和 runbook。

## 5. 失败语义汇总

| 环节 | Redis 出错 / 身份读不到 | SQL 读超时 | 原子复核冲突 | 判定有歧义 |
|---|---|---|---|---|
| G1 login | 登录失败，可重试（现有 EnterGame 失败路径，不新增 tip） | 同左 | 同左 | V3 / V4 可重试；V5 fail-closed + critical |
| G2 go/db | 发布失败按现有 `errFenceRedis` 处理（延后重试） | — | — | 只计数，不拒写 |
| G3a SM | 铸造失败按现有 19 / 重试路径 | — | 同现有 epoch 冲突 | — |
| G3b SM | WAITAOF 出错或确认不足 → 回滚 + 可重试 | — | — | — |
| G4 C++ | 不变 | — | — | 新码只用于计数和日志 |
| G5 assetop | 跳过并还租约 | — | — | 跳过 |

所有新路径都不把错误伪装成成功，不静默降级（AGENTS §11.3）。

## 6. 兼容、混版与回退

- **滚动发布混跑：**
  - 旧 login 忽略戳，行为同今天；新 login 更严格。两者共存不出错，只是旧实例仍有原缺陷。
  - 旧 go/db 不带 `ack_ahead` 标志，只是少一个计数。
  - 旧 SM 铸 `+1`，新 SM 铸墙钟，两种值都单调。新值铸出之后，旧实例从高位 `+1`，同样单调。
  - 旧 C++ 对新码无感，因为脚本随二进制一起走。
- **回退（降级）：**
  - G1 有开关 `ParentLoad.VerifyOnRedisIdentityChange`，缺省 true；false 等于今天的行为。用于冷读风暴时止血。
  - G3b 把两项副本数都写 0 即关闭。
  - G3a 无开关：纯单调；回退旧二进制后，旧二进制从高位 `INCR`，安全。
  - G4 无开关。
  - G5 有开关，关 G1 时的注意事项见 §4.5。
- **上线前门禁**（负责人确认后写进开服清单）：
  - P1、P2 落码，Codex 验证通过；
  - §8.3 R-a 冷读突发压测有对比表；
  - R-G1-1 的处置（§14 第 15 条落码，或接受）有结论；
  - P3 是否上线以 R-b 压测结论为准。

## 7. 监控

指标都不带 player_id。

| 指标 | 来源 | label |
|---|---|---|
| `login_parent_load_verify_total` | login | `verdict` ∈ fast_verified / consistent / adopted / inflight / reconciling / sql_behind / no_history / identity_unavailable / conflict / error |
| `login_redis_identity_change_total` | login | 无。每个进程首次见到新身份时 +1，并打 INFO 日志 |
| `db_save_barrier_ack_ahead_total` | go/db | `kind` ∈ above_pending / pending_missing |
| `scene_manager_owner_epoch_mint_clock_behind_total` | SM | 无。存储值 ≥ 墙钟、地板没起作用（时钟偏差信号） |
| `scene_manager_owner_epoch_mint_durable_total` / `_seconds` | SM | `result` ∈ confirmed / not_durable / error |
| `asset_abandon_skipped_total`（沿用现有跳过计数） | assetop | 新增 `reason="redis_unverified"` |
| C++ `[OwnerEpoch]` 统计行 `owner_counter_behind=` | scene | LogQL |

告警追加到 `deploy/k8s/owner-epoch-alerts.yaml` 和相应服务的告警文件。关键告警按该文件已有经验用绝对值。

| 告警 | 条件 | 级别 |
|---|---|---|
| `DbSaveBarrierAckAhead` | 绝对值 > 0 | warning。描述写明：多半由 Redis 回退引起，玩家登录时自动采纳；同时看 `login_redis_identity_change_total` |
| `LoginParentLoadSqlBehind` | `verdict="sql_behind"` 绝对值 > 0 | critical：SQL 侧回档或损坏 |
| `LoginParentLoadReconcilingPersistent` | `verdict="reconciling"` 在 30 分钟内持续增长 | critical：转人工 `save_barrier_restore` |
| `SceneManagerMintNotDurable` | `result="not_durable"` 占比超过阈值 | warning，阈值待压测 |
| `SceneOwnerCounterBehind` | LogQL `owner_counter_behind` > 0 | critical |

## 8. 回归测试清单（交 Codex）

### 8.1 单元与真库

INFO 相关的 Lua 只能在真 Redis 上测（miniredis 不支持 `INFO server` / `INFO replication`，`route_ready.go:185`），**7.2 与 8.x 各跑一轮**。

- **G1**：
  1. 戳 == 身份，走快路径，行为与今天一致；
  2. 戳缺失，判 V1，只写戳；
  3. V2：Redis 是 V_old 的父键和屏障，SQL 两行都是 V_new → 采纳后父键等于 SQL，`P = A1 = A2 = V_new`，owner 只在更低时抬到 E_new，意图字段清空，子表缓存删除；
  4. ACK > P（主结局）→ 无视戳转核验 → 采纳；
  5. V4 两行版本不同 → `reconciling`，不写任何键；
  6. V5：证据或 ACK 高于 SQL → fail-closed；
  7. V6 新号种子 → 只写戳；
  8. V7：P 空而 SQL 有历史 → 采纳（今天报「需要停写恢复」）；
  9. 读到采纳之间，并发存盘改了 pending → 冲突、可重试、零写入；
  10. 读到采纳之间实例身份变化（真库重启容器）→ 冲突；
  11. role 不是 master → 拒绝；
  12. 判定纯函数的表驱动用例：20 位定长版本串、uint64 最大值、2^53 相邻值。
- **G2**：新版本 > pending、pending 缺失两种情形都写 ACK 并带回标志，计数正确；正常情形不带标志。
- **G3a**：
  1. 地板 > 当前 + 1，铸出地板值；
  2. 地板 ≤ 当前，铸出当前 + 1；
  3. 首发成功、应答丢失后原样重发 → 回负值、识别命中；
  4. bump 回滚之后原样重发 → 回 0；
  5. 回滚仍是 +1；
  6. 存储值 > 墙钟时计 clock_behind；
  7. 十进制比较不经 double。
- **G3b**：
  1. 确认之后才推路由（注入一个推送桩，断言调用顺序）；
  2. 确认不足 → 回滚 + 可重试；
  3. 启动探针失败拒启；
  4. 两项都为 0 时不发 WAITAOF；
  5. 铸造和 WAITAOF 走同一条连接。
- **G4**：存储值 < 缓存值回新码且零写入；存储值 > 缓存值回 0；owner 缺失仍是 error_reply；C++ 处置计 `owner_counter_behind`。
- **G5**：戳不等，跳过并还租约；戳相等，照旧判定；「已见」的 Finalize 不受影响。

### 8.2 故障演练（真实环境，按[跨区故障注入手册](../ops/cross-zone-failure-test-runbook.md)的形式补章节）

- **D1**：单实例。玩家存一次盘后立即 `kill -9` Redis，玩家离线后再登录 → `adopted`，背包和货币等于 SQL。
- **D2**：HA。用 `tc` 给复制链路加延迟后切主。不开 G3b 时复现铸造丢失（`owner_counter_behind`）；开 G3b 后同一时序不再出现。
- **D3**：恢复一份旧的 RDB / AOF → 登录全部走核验；下一次铸造取墙钟值。

### 8.3 压测（AGENTS §6，先存 prev-summary）

- **R-a**：Redis 重启后集中重新登录，对比基线的 entergame_total、dataloader 阶段、DB 读任务 QPS 和 Kafka lag。
- **R-b**：开、关 G3b 各跑一轮登录 ramp + 跨节点换图，对比 SM EnterScene 子阶段。子阶段要新增 `mint_durable`，`stress_summarize.ps1` 对应段要同批改。

没有对比表之前，不对 R-a、R-b 下任何性能结论。

## 9. 落码时要同步的文档与配置

- 本次已在 `db-write-fencing-mysql.md` §9.1 B1 表下加 2026-10-22 更正注，在核对稿 §2.2 加指向注（两处原文都保留）。
- G3a 落码时：
  - 改 `go/shared/ownerepoch/ownerepoch.go` 包注释和 `owner_epoch.go` 头注释里「只经 INCR 前进」的表述；
  - 改 `scene-owner-reentry-barrier.md` §3.3；
  - 把 §9.1 第 c 步的 K 改为「当前毫秒」（该处已预告）。
- G1 落码时：`save_barrier_restore` 恢复成功后写戳；§9.1 第 d 步「真源是 MySQL 时删父键」可改为「交给 G1 在登录时采纳」，runbook 同步。
- G1 依赖「原地换数据必换身份」，前提是 `--enable-debug-command no`：两份清单都已显式写出（`redis-ha.yaml:277-282`、`redis.yaml:118-139`），落码时在 G1 的启动自检里沿用路由对账的 `CONFIG GET enable-debug-command` 检查。
- `k8s_deploy.ps1:3028` 的「补上 WAITAOF 之前，重启前先停写」提示：G3b 落地后改写为只针对存盘 Lua。存盘 Lua 仍不加 WAITAOF，干净 SHUTDOWN 本身不丢数据。

## 10. 考虑过而不采用

| 方案 | 不采用的理由 |
|---|---|
| 每次登录都和 SQL 交叉校验 | 快路径失去意义：每次登录多两个 Kafka 往返读任务。身份戳只在可能回退之后才付这份代价 |
| C++ 存盘 Lua 加 WAITAOF | hiredis 单连接异步管线，WAITAOF 会把整条连接阻塞到下一次刷盘（§0 第 2 条） |
| 核心实例改 `appendfsync always` | 全实例每条写都刷盘，代价未测，影响所有用途的键。可作为 G4a 拆分后、存盘控制面小实例的候选，不在本稿 |
| 以 SQL `fence_owner_epoch` 作铸造地板 | SM 不直连 zone 玩家库。EnterScene 已有的 DS `LookupAccountRole` 强读（`location-release-binding.md` §16）读的是全局账号目录，不含 zone 库行围栏，加 SQL 往返代价更高。而且它和墙钟地板一样堵不住旧内容覆盖 |
| C++ 存盘被拒时前推计数器到缓存值再写 | 回退丢掉源端冻结存盘、却留下铸造时，目标节点载入的是旧父键；前推让旧内容以更高 epoch 写穿 SQL 行围栏（§4.4） |
| 被拒持有者把内存态直接发一笔「救援 DBTask」进 SQL | 同上：持有者无法自证载入的内容不旧，救援会把旧内容写穿行围栏 |
| C++ 存盘或 go/db 发布时也写身份戳 | go/db 写戳会把回退后、尚未核验的副本误判为可信（主结局正是 go/db 在回退副本上推 ACK）；C++ 写戳要改热路径 Lua，并且每次存盘调 INFO |
| 实例级「回退嫌疑」全局标志 | 区分不了哪些玩家已经核验过，只能整批放行或整批冷读 |
| 身份变化后立即后台全量对账 | 一次切主要扫全 zone 玩家，没有调用方需要它马上完成，懒核验已足够（YAGNI）。只在 G5 跳过计数增长时再做 |
| ACK > pending 时 go/db 拒写 ACK | ACK 是真实的 SQL 事实，拒写会丢掉 G1 需要的证据，并且让补投永远等不到 complete |

## 11. 分批与改动面估计

| 批 | 内容 | 改动面（估） | 依赖 / 证据 |
|---|---|---|---|
| P1 | G1 + G2 + G5 | `go/shared/savebarrier`（判定纯函数、身份 Lua 片段，2 个文件加测试）；go/login dataloader（屏障脚本、加载流程、指标、配置加测试，约 6 个）；go/db 发布脚本、指标加测试（约 3 个）；`go/shared/assetop/abandon.go` 加测试（约 2 个）；告警 yaml 1-2 个；文档。合计约 16 个文件 | R-a 压测；R-G1-1 结论 |
| P2 | G3a + G4 检测码 | `owner_epoch.go` 及其测试（约 3 个）；C++ `redis_client.h` Lua、`player_lifecycle.cpp` 处置、统计（约 3 个）；ownerepoch 包注释；文档。约 8 个 | 无性能代价；C++ 串行编译 |
| P3 | G3b | SM 配置、专用客户端、铸造路径、指标、`k8s_deploy.ps1` 渲染与总账、`scene_manager.yaml`，加测试，约 8 个 | R-b 压测；D2 演练 |

每批都在 AGENTS §10.2 的 30 文件阈值以内。

## 12. Java 版（AGENTS §12）

只读核对 `D:\luyuan\xuanming-server-mmo`，结论是**不同构**：

- owner_epoch 存在 MySQL `player` 行里，夺权、交出、存盘都是 SQL 事务内的 epoch 围栏：`game-player-store/.../PlayerMapper.java:78` 的 `owner_epoch = owner_epoch + 1`；`PlayerStore.java` 类注释的归属协议。
- scene 按 epoch 从 MySQL 加载：`PlayerStore.loadForScene(playerId, expectedEpoch, …)`。
- Redis `game:location:{player_id}` 只是持有者按 `(epoch, 序号)` 写的位置投影（`game-discovery/.../PlayerLocationDirectory.java:26-44`）。它回退只影响路由可用性，不造成存档回档，也不会重铸同值的 epoch。

按负责人约定（不适用的项不往 Java `PARITY.md` 加行），本稿不登记 PARITY。G5 的同类问题（Java 资产放弃是否读 Redis 副本）本次未核，留给 Java 线的数据一致性审计。

## 13. 待负责人拍板

1. **分批顺序**：建议 P1 → P2 → P3，P3 以压测为准。
2. **G1 判 V2 / V7 时自动采纳 SQL**，还是只告警，由人工跑 `save_barrier_restore`？建议自动采纳：条件限定在「SQL 两行同版本、严格新于 Redis 记录的一切版本、证据不高于 SQL」，内容全部来自已提交 SQL，是 §9.1「定真源」在无歧义情形下的机械化；有歧义的 V4 / V5 仍然转人工。
3. **G3b 做不做**：取决于 R-b 压测的 EnterScene 延迟，延迟预算由负责人给。不做时，残余「铸造丢失丢 ≤300s」由 G4 检测码可观测。
4. **R-G1-1**：上线前把 §14 第 15 条「go/db 待落写标记」落码，还是接受这条残余。
5. **G5 的活性代价**（跨 Redis 身份变化、离线 ≥180 天的玩家放弃会推迟到他下次登录）是否可接受。
