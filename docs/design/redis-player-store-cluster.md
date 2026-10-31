# 玩家态 Redis 集群化：物理水平扩展设计

> **部分过期(2026-10-30)**:存档相关批次(父快照、补投索引与补投器、子表缓存与 go/db 句柄)作废;位置、会话、战斗锁等协调键的分片仍要做([player-data-tidb-scaling-plan.md](./player-data-tidb-scaling-plan.md) §8.4)。

> 状态：**设计 v2（2026-10-23）**。v1 经四视角对抗评审（一致性、扩展与运维、可实现性、替代方案），评审结论逐条核对源码后吸收进本版，变更见文末「修订记录」。第 1 批已落码（未编译），其余批次见 §13。所有容量数字除注明 [M] 者外都是推算 [I]，未经本仓实测；结论以 §14 的实测为准。
>
> 需求来源：[Redis 物理水平扩展交接](../handoff/redis-horizontal-scaling-requirements-20261022.md)（用户原话「我就想 redis 可以物理水平无限扩展」，方案由本稿自定）。源码事实来自 2026-10-22 全链只读盘点（10 路，按子系统 + 按模式两种方式扫描），行号以当时工作区为准，落码时按函数名复核。
>
> 本稿是本次方案的**唯一权威**。[共享 Redis 按用途拆分 §20–§25](shared-redis-split-by-purpose.md) 的「单元化 + 单元内 Cluster」路线作为输入材料，不再作为实施路线（G4a 会话 2026-10-23 已在该稿 §25.3 把相关条目指向本稿）。
>
> 证据等级：[C] 读代码或配置确认；[D] 文档记载；[I] 推算；[M] 实测。

> **2026-10-24 方向变更（先读；下文原文保留）**：用户 10-24 明确「玩家数据不要 redis 存储，其他改用的地方还是要用」，记录与键族分类见 [TiDB 决策 §9](global-data-layer-tidb-decision.md)。对本稿的影响：
> - 父快照 `PlayerAllData`、子表缓存、资产 SQL 证据、战斗结算原件属于玩家数据，不再进 Redis Cluster；补投索引与补投器随父快照移出失去现有用途（新存盘路径如果保留异步投递，另行设计）。
> - 第 2b 批（父快照改名）在看到本变更之前已落码，2026-10-29 定为保留；2c、2d 作废（§13）；第 4 批的子表缓存与排序锁 / 游标改名、第 8 批的结算索引，等新设计定稿后再定。下文 §1 第 3、4 条、§5.3 存盘脚本改形、§5.4、§5.5 中的子表缓存与资产证据、§10.1 的容量推算待重写。
> - owner_epoch（2a，已落码）、归属与位置族（3）、player_locator（7）、战斗锁等协调键仍在 Redis，按本稿分片不变；集群规模要按剩下的键重算。
> - 只定了方向，存盘与加载的新路径未设计、未落码。

## 1. 结论（先读）

1. **玩家态单独成一个存储域，部署为 Redis Cluster，按 `{player_id}` 分片。** 全服只有一个逻辑键空间：任何 Scene、任何服务都能寻址任何玩家，跨 zone 自由游玩与位置透明不变。分片身份是 `slot = CRC16(player_id 十进制) mod 16384`，与业务 zone、home_zone、运行位置都无关，热点 zone 的玩家天然散到全部主节点。
2. **玩家域钉 Redis ≥ 8.4（与本地 compose、Java 版一致，取 8.10.x），扩缩容用原子 slot 迁移（`CLUSTER MIGRATION IMPORT`）。** 迁移期间源节点照常服务、目标节点后台追平，交接时短暂停写、之后统一回 MOVED，不出现逐键迁移那种整个窗口内多键脚本持续 TRYAGAIN、源主故障后同键两份的问题（§8）。
3. **一次存盘的原子范围不变，只是换了承载方式。** 一名玩家的父快照、owner_epoch、存盘屏障、两条原始 DBTask 意图，加上**该玩家所在 slot 的补投索引**，仍在同一段 Lua 里原子提交；go/db 的排序锁、子表缓存、资产 SQL 证据、两表 ACK、游标与已落库 epoch 同一 slot。不引入跨分片事务。
4. **补投索引按 slot 建（16384 个），与 zone 解耦。** 用预计算的 slot-tag 让索引键与成员玩家同槽；补投器按 slot 段租约分片，存活成员单独登记、按成员数算公平份额，任何存活副本都会接管无主段。
5. **C++ 自研集群路由层，同一 slot「停等」：每个 slot 至多一条在途命令。** 同 slot 的执行顺序因此严格等于 `command()` 调用顺序，与重定向类型和迁移方式无关；不同 slot 之间照常并发。重定向与「未执行」类服务端错误在路由层内按时间预算重试，耗尽按「连接断开」（空应答）上报，并让同 slot 已排队的后续命令一起空应答收尾，与单连接断开的语义一致。
6. **扩容就是加主节点、迁 slot。** 数据与读写真实搬到新节点；slot 交接后旧节点对它回 MOVED，不存在新旧两处都能写的窗口。增加副本或接入代理不算扩容证据（§14 的验收以主节点数、slot 分布与主线程饱和度变化为准）。
7. **扩展边界**：Redis Cluster 设计目标约 1000 个节点（主 + 副本都算）。每主 1 副本时玩家域按 2000 万 PCU 推算约需 50–250 个主、100–500 个节点 [I]，余量约 2–10 倍；节点数达到约 600 时启动多集群联邦（§10.3），键形不变。
8. **系统级上限目前不由玩家域决定。** 控制域（原共享实例）里登录令牌 / 会话随在线人数增长，仍是单主；排队已由 G4a 会话按 zone 组拆实例（2026-10-23 第 1a 批）。登录令牌 / 会话的横向扩展列为本稿第 11 批（§10.2），在它落地前，系统的 Redis 扩展上限由控制域单主决定，本稿不把第 7 条写成系统级边界。
9. **映射态移出 Redis**（负责人 2026-10-23 拍板直读 TiDB，§5.13），不在本稿范围。
10. **不做**：单元化与跨单元转区（违反自由跨区需求）、默认部署接入代理（条件触发的下一阶段，§6.4）、读副本。

## 2. 需求与保证对照

| 需求（交接 §1、§5） | 本稿的回应 | 位置 |
|---|---|---|
| 物理节点增加，容量与读写吞吐持续增长 | 玩家域按 slot 分布在主节点上；加主节点 + 原子迁 slot 后，迁过去的键的内存、读写与 Lua 执行都在新节点 | §5.1、§8 |
| 覆盖大量 zone 与自由跨 zone | 分片身份与 zone 无关；全服一个逻辑空间，任意 Scene 可寻址任意玩家 | §5.1 |
| 控制连接数、热点、路由开销、故障面 | 每进程每主节点 C++ 1 条、Go 有上限的小池；玩家域没有全局热键；路由是一次 CRC16 加数组查表；一个主节点故障只影响 1/M 的玩家 | §5.6、§6、§7 |
| 归属围栏有效，旧写者不能覆盖新状态 | 围栏脚本只改键名不改语义；所有围栏键同玩家同 slot；交接后旧节点回 MOVED | §9 |
| 重试幂等、存盘意图可恢复 | 意图与索引同槽原子写入；补投段租约保证全覆盖且随副本数扩展；路由层与 Go 客户端都不自动重放「结果未知」的命令，沿用既有幂等重放 | §5.4、§5.6、§5.7、§9 |
| SQL 提交确认可靠 | 两表 ACK、证据、屏障仍同槽、同脚本；回收只在两表 ACK 都等于 pending 时于同一脚本内发生 | §5.5、§9 |
| 跨区、重登、扩容迁移不丢资产、不读旧数据 | 不读副本；原子迁移；冷恢复仍过提交屏障与实例身份戳，身份变化走 SQL 核验与采纳 | §7、§9 |

## 3. 现状事实（2026-10-22 盘点摘要）

### 3.1 拓扑与客户端

- **全仓一个共享 Redis**：`redis.<infra>:6379`，DB 0。核心玩家态、映射、排队、go/db 控制面、登录会话、场景路由、战斗全在里面 [C]（`deploy/k8s/manifests/infra/redis.yaml:1-3`）。HA 形态是哨兵 + haproxy、1 主 2 从（`redis-ha.yaml:697`），复制异步 [C]。
- **C++**：每进程 1 条 hiredis 连接（`node.cpp:710` 的 `tlsRedis.Connect`），单 host:port，启动时解析一次；scene 的 `MessageAsyncClient` 引用同一条连接（`redis.cpp:22`）。业务直发 43 处，`GetZoneRedis()` 引用 88 处，散在约 20 个文件；没有 MOVED / ASK 处理，没有 SCAN、MULTI、WAIT [C]。muduo 的 `hiredis::Hiredis` 封装只有 `command(cb, fmt, ...)`，Windows（`cpp/libs/engine/muduo_windows/src/contrib/hiredis`）与 Linux（`tools/archived/muduo_linux_overlay/contrib/hiredis`）各一份 [C]。
- **Go**：核心链路上所有句柄都是单地址 `redis.NewClient` 或 go-zero node，全仓没有 `UniversalClient`；go-redis 的 `ClusterClient` 按第一个键路由 MGET、不拆跨槽键，并在 EOF / 超时后自动重试同一命令（v9.16 `osscluster.go` 的 process 循环与 `error.go` 的 `shouldRetry`）[C]。
- **连接总账**：单实例 `maxclients 40000`，HA 下单代理 13000；峰值 ≈ 264·Z + 1075（Z 为 zone 数），漏记 saveoutbox 与 data_service 发布池 [C][I]（`k8s_deploy.ps1` `Get-RedisConnectionLedger`）。

### 3.2 玩家态的多键原子操作

- C++ 存盘屏障四键：`PlayerAllData:<pid>`、`player:<pid>:owner_epoch`、`player:{<pid>}:save_barrier`、`player:save_pending:<home_zone>:<pid%64>`；Lua 逐字校验键形与桶号（`redis_client.h:85-220`）[C]。
- C++ 归属与交接 8 段脚本、战斗 lock/ctx 5 段、`kReadSqlAck`、`kPublishMarkLua`：都只碰同一玩家的键，但键大多没有 tag [C]。
- 跨玩家或含全局键：战斗结算 6 段脚本（全局 `battle:settlement:pending:index`，stage 还带每局的 `staged:<battle_id>`）、`player_team` 的跨玩家 MGET [C]。
- go/db：令牌发布脚本（排序锁 + 子表缓存 + 资产证据 + 屏障）、游标脚本（锁 + 游标 + epoch）只有证据与屏障带 tag；saveoutbox 认领 / 迁移脚本跨 zone 桶；屏障恢复工具一段脚本 72–75 键（含 zone 维护键与 64 桶）[C]。
- login：冷加载 begin / publish、新号种子、身份补齐（父快照 + `player_locker` + 会话 + 位置）都按玩家，未带 tag；外观补齐对同账号多名玩家的父快照 MGET [C]。冷加载的快路径要求屏障里的**实例身份戳**（INFO 的 `run_id:master_replid`）等于当前执行节点，不等时读两张 SQL 行核验并按需采纳（`parent_commit_barrier.go:20-30`、`shared/savebarrier/instance_identity.go`）[C]。
- scene_manager：落点五键（owner、location、handoff、location_revision、floor）按玩家；离场脚本把玩家键与场景 / 节点人数键绑在一起，目的是让 go-zero 原样重发与 player_locator 重试都只减一次（`leavescenelogic.go:34-38`）[C]。
- player_locator：9 段会话 Lua 全部跨 4 个全局 lease 键与按 gate 聚合的索引，`claimExpiredLeasesScript` 在脚本里现拼其他玩家的会话键，另有 `""` 哨兵键 [C]。
- 其他服务的跨玩家读：friend、guild、match、login 对会话、父快照、战斗锁的 MGET；friend 在线目录按请求分页的游标 SCAN、locator 补建的全库 SCAN [C]。

### 3.3 依赖「同一连接 FIFO」的位置（C++）

握手 AUTH、存活 PING、战斗与收件箱「存盘后紧跟 PING」的快路径、战斗销账 / 续锁先于读锁（`player_battle.cpp:808-813` 条件删锁先于摘组件触发的跟随读锁）、`SendConditionalMarkWrite`、放弃补写先于 A2′、改派核实读与凭证补写、A2′ 先于 AsyncLoad、重连后的补发顺序、battle 节点孤儿清理三步 [C]（盘点共 15 处）。**除跨玩家 MGET 与全局战斗索引外，全部是「同一玩家的键之间的顺序」，而且前后两条命令的键集合往往不同**——这决定了 §5.6 必须做到「同 slot 内执行顺序 = 调用顺序」，只保回调顺序不够。

### 3.4 上线状态

负责人 2026-10-18 确认项目尚未上线，2026-10-22 再次确认（`docs/PROGRESS.md`「P1 事故登记」），2026-10-23 补充玩家数据可以全部清空（G4a 会话转述）。没有存量 zone，没有旧版二进制在跑：**键改名直接以新键名起步，不做双读双写窗口**（[存盘围栏](db-write-fencing-mysql.md)「上线状态」）。上线后本节失效，键形变更须按 §12 的停写切换规则。

## 4. 候选方案与取舍

| 维度 | A. Redis Cluster 直连（**选定**） | B. 应用层虚拟桶分片 | C. 接入代理层收敛连接 | D. 单元化（每单元一集群，跨单元转区） |
|---|---|---|---|---|
| 迁移期旧写者 | 原子 slot 迁移：交接前只有源节点受理写，交接后源节点回 MOVED | 要自建：每段脚本都多核一把「桶归属」键，迁移要冻结、拷贝、翻转、清理，枚举一个桶的键还需要额外索引或全库 SCAN | 同 A（后端仍是 Cluster） | 跨单元搬迁要自建转区协议 |
| 实现量 | 键改名 + 跨域脚本拆分 + C++ 路由层 + Go 客户端替换 | A 的全部 + 迁移协议 + 每段脚本的归属检查 | C++ 侧最少；但见下一行 | A 的全部 + 转区协议 |
| 错误语义 | 路由层把重定向与「未执行」类错误耗尽统一成空应答，保住「错误应答 = 业务结论」的现有契约 | 自定 | **代理把上游断连变成错误应答**（如 Envoy 的 upstream failure），而部分 C++ 调用方把非预期应答当业务结论处理——battle 结算外发箱会把它误读成已销账（`redis_manager.h:75-76` 的注释已记录同类风险） | 同 A |
| 原子脚本 | 多键同槽即可 | 同 A | 代理按第一个键路由 EVAL，同槽前提相同；WAITAOF、INFO 等不经代理 | 同 A |
| 连接 | 进程数 × 主节点数（§6，目标规模内可接受） | 同 A | 收敛到代理数 × 主节点数 | 单元内同 A |
| 热点与扩展 | 按玩家散列，无 zone 热点 | 同 A | 同 A | 热点单元拆不开 |
| 与需求冲突 | 无 | 无 | 无 | **违反「自由跨 zone、不改成转区体验」** |

选 A 的理由：Redis Cluster（8.4 起含原子 slot 迁移）已经提供了 B 要自建的东西，而且是成熟实现；C 的连接收益在目标规模内不必要，代价是错误语义的系统性变化；D 与需求直接冲突。C 保留为条件触发的下一阶段（§6.4）。现成 C++ 集群库（hiredis-cluster、redis-plus-plus）不选的理由见 §5.6 末段。

## 5. 选定架构

### 5.1 存储域与分片身份

| 域 | 内容 | 部署 | 分片 |
|---|---|---|---|
| **玩家域 player** | 一切以 player_id 为身份的键，以及由玩家派生、必须与玩家同槽的按 slot 索引（§5.2 表） | 新建 Redis Cluster，Redis ≥ 8.4（取 8.10.x），每主 1 副本（可选 2 副本，§7），AOF everysec，PVC 持久化；每个 Pod 一个 ClusterIP Service，以 `cluster-announce-ip/port/bus-port` 宣告 Service IP（§5.6「连接」） | `slot = CRC16(<pid 十进制>) mod 16384`；slot → 主节点由集群管理 |
| **登录域 login** | 连接栅栏、登录态、设备集、账号索引、令牌、令牌家族、口令配额（第 11 批从控制域拆出，§10.2） | K8s 缺省与玩家域共用一个 Redis Cluster，配置上能拆开（`LoginRedis`） | 连接键按 `{<gate>:<inc>}`，账号键按 `{AccountTag(account)}` |
| **控制域 control** | 场景路由、账号锁、选主锁、重试队列、task:result、协议世代、战斗活动结果、队伍、各服务私有键、工具互斥键 | 现有共享实例（含 redis-ha），名字与地址不变；排队族已由 G4a 第 1a 批拆到按 zone 组的 queue 实例 | 不分片（上限与路线见 §10.2） |
| **映射域 mapping** | `player:zone/placement/maprev`、合服围栏镜像、就绪标记 | 2026-10-23 起改为直读 TiDB，移出 Redis（§5.13） | 不适用 |

**接缝规则（可机械检查）**：玩家域客户端只接受带 hash tag 的键，非玩家域的键一律不带 `{<数字>}`。C++ 路由层与 Go 玩家域客户端的守卫在发送前检查（§5.6、§5.7），本机单实例开发时同样生效。

### 5.2 键形契约

**规则**：把键里的玩家 id 用花括号包起来，其余不动（`X:<pid>:Y` → `X:{<pid>}:Y`）。花括号里是 player_id 的十进制，无前导零。玩家域所有键都带 tag，单键族也不例外——这是 §5.1 接缝检查的前提。

| 族 | 旧键形 | 新键形 | 主要写者 / 读者 |
|---|---|---|---|
| 父快照 | `PlayerAllData:<pid>` | **删除**（存档只在 TiDB `player_store`） | — |
| 归属代次 | `player:<pid>:owner_epoch` | `player:{<pid>}:owner_epoch` | scene_manager 铸造；C++、player_inbox |
| 交接标记 | `player:<pid>:handoff` | `player:{<pid>}:handoff` | C++；scene_manager |
| 交接资格 | — | `player:{<pid>}:handoff_fence` | C++ 写；scene_manager 读；go/db 维护阶段排空核对 |
| 位置 | `player:<pid>:location` | `player:{<pid>}:location` | scene_manager；C++、locator、login、guild、trade、mail、match、player_inbox |
| 位置修订 / 下限 | `player:<pid>:location_revision(_floor)` | `player:{<pid>}:location_revision(_floor)` | scene_manager |
| 离场等待 | `player:<pid>:leave_release_wait` | `player:{<pid>}:leave_release_wait` | scene_manager |
| 换线冷却 | `player:<pid>:channel_switch_cooldown` | `player:{<pid>}:channel_switch_cooldown` | scene_manager |
| 进场去重 | `enter_scene:dedup:<pid>:<req>` | `enter_scene:dedup:{<pid>}:<req>` | scene_manager |
| 离场墓碑 | — | **删除**（方案作废，§5.10；后缀留在保留表防误用） | — |
| 登录代次 | `player:<pid>:login_generation` | `player:{<pid>}:login_generation` | login |
| 同步摘要 | `player:<pid>:sync_digest` | `player:{<pid>}:sync_digest` | friend / guild / match 写；C++ 读 |
| 存盘屏障 | `player:{<pid>}:save_barrier` | **删除** | — |
| 资产 SQL 证据 | `player:{<pid>}:asset_sql_commit` | **删除**（生产无读写，后缀留在保留表） | — |
| 补投索引 / 段租约 / 成员登记 | `player:save_pending:*`、`player:save_outbox_*` | **删除** | — |
| 会话 | `player:session:<pid>` | `player:session:{<pid>}` | player_locator；friend、guild、mail、match、login |
| 旧位置键 | `player:location:<pid>` | **删除**（读者已删，未上线无存量） | — |
| 会话租约（4 键） | `player:leases`、`:processing`、`:claim_tokens`、`:claim_payloads` | `player:leases:{<slottag>}` 及同 tag 的三个兄弟键 | player_locator |
| gate 索引 | `player:gate_index:sessions:<uuid>`、`:gates` | `player:gate_index:{<slottag>}:sessions:<uuid>`、`player:gate_index:{<slottag>}:gates` | player_locator（B0-1 §11 的既定形状） |
| 分片登记 / 段租约 | — | `player:locator_shard:<role>:members:{afps}`、`player:locator_shard:<role>:segment:{<SlotTag(64c)>}`，role 是 `lease_monitor` 或 `gate_sweep` | player_locator（§5.4 同一套办法，§5.9） |
| AFK 通行 | `player:afk_pass:<pid>` | `player:afk_pass:{<pid>}` | player_locator |
| 玩家锁 | `player_locker:<pid>` | `player_locker:{<pid>}` | login |
| 合服提示 | `player_merge_notice:<pid>`、`player_force_rename:<pid>` | **删除**（生产无读写） | — |
| 登录幂等 | `login_idempotent:<pid>:<req>` | `login_idempotent:{<pid>}:<req>` | login |
| 战斗锁 / 上下文 | `battle:lock:<pid>`、`battle:ctx:<pid>` | `battle:lock:{<pid>}`、`battle:ctx:{<pid>}` | C++；guild、match 只读 |
| 待结算 / 结算索引 / 已暂存标记 | `battle:settlement:*` | **删除** | — |
| go/db 排序锁 / 游标 / epoch | `distributed:lock:kafka:ordering:*`、`consumer:applied(_epoch):*` | **删除** | — |
| 子表缓存 / expand 消费锁 | `<msgType>:<pid>`、`kafka:consumer:lock:<pid>` | **删除** | — |
| data_service 字段 | `player:{<id>}:<field>`、`__lock/__version/__fields` | 不变 | data_service |

每个键只有一个构造函数：Go 在 `shared/redisslot/player_keys.go`（owner_epoch、handoff 在 `shared/ownerepoch`，handoff_fence 在 `shared/handofffence`，sync_digest 在 `shared/playersync`）；C++ 在 `player_ownership_comp.h`、`battle_lock_rules.h`、`player_landing.h`。两边的键形由 `redisslot/player_keys_test.go` 与 `currency_test` 的金样钉住。

控制域里与玩家相关、但**不进**玩家域的键（不带玩家 tag）：`player_to_account:<id>`（login 账号反查）、`assetop:wake:<svc>:<pid>`（guild / trade 的唤醒去重）、`instance:<scene>:members`、`node:zone:<z>:<node>:members`（场景与节点成员，§5.10）。它们不与玩家域键出现在同一条命令里，`scene_keys_contract_test.go` 钉住 scene_manager 这一侧。

**data_service 命名空间**：`player:{<id>}:<字段>` 是 data_service 的字段命名空间，新进入该前缀的后缀（owner_epoch、handoff、handoff_fence、location、location_revision、location_revision_floor、leave_release_wait、channel_switch_cooldown、leave_tomb、login_generation、sync_digest，加上已有的 save_barrier、asset_sql_commit）统一由 Go 共享包 `shared/redisslot` 的保留后缀表登记（第 1 批已落），`isReservedPlayerField` 改为查这张表，不再逐个比键名。今后任何模块新增 `player:{<id>}:` 键，必须登记到这张表（契约测试枚举核对）。

**slot 与 slot-tag**（两种语言逐字一致，金样向量钉住；第 1 批已落 `go/shared/redisslot` 与 `cpp/libs/engine/infra/storage/redis_cluster/redis_slot.h`）：

- `HashSlot(key)`：Redis Cluster 规范——键里第一个 `{` 之后第一个 `}` 之前若非空，只对这段算；CRC16-XMODEM（多项式 0x1021，初值 0）取低 14 位。金样：`HashSlot("123456789") = 12739`（CRC16 = 0x31C3，规范原文例子）；`{user1000}.following → 3443`、`foo{}{bar} → 8363`、`foo{{bar}}zap → 4015`、`foo{bar}{zap} → 5061`、`PlayerAllData:{42} → 8000`、`player:{42}:owner_epoch → 8000`、`18446744073709551615 → 5255`。
- `PlayerSlot(pid) = HashSlot(<pid 十进制>)`；`PlayerTag(pid) = "{" + <pid 十进制> + "}"`。
- `SlotTag(s)`：在字母表 `[a-z]` 上按「先短后长、同长按字典序」逐个枚举字符串 t，第一个满足 `CRC16(t) mod 16384 = s` 的 t 就是 slot s 的 tag。长度 4 以内即可覆盖全部 16384 个 slot（最后填上的是 `cgoq`）。纯字母的 tag 不会被误解析成玩家 id。金样：`SlotTag(0)=afps`、`SlotTag(1)=emj`、`SlotTag(2)=ahwf`、`SlotTag(100)=aotc`、`SlotTag(12739)=aarc`、`SlotTag(16383)=hia`；整张表按 slot 顺序以 `\n` 连接（72467 字节）后 SHA-256 = `b564523fd7c87f294eab3efd4b09e171775388ea14790138c1d4bb988df61d4c`，FNV-1a 64 = `0xf4a7bdc8d20a28e5`。两种语言都在首次使用时生成这张表，不检入生成物。
- 某玩家的按 slot 索引键 = `<族前缀>:{SlotTag(PlayerSlot(pid))}…`，与该玩家的 `{pid}` 键必然同槽。

### 5.3 原子范围与脚本改形

> **2026-10-31 现状**：存盘屏障、战斗结算 6 段、go/db 与 saveoutbox、屏障恢复、debug_import、login begin / publish / 种子 / 身份补齐这些行随存档移出 Redis 作废，下表原文保留，不再是计划。第 3 批已落的是「C++ 归属 / 交接 / 改派 / 取证 / 战斗 lock-ctx」「`player_team` 的 MGET」「scene_manager」三类；player_locator 第 3 批只改了会话键形，第 7 批改了结构（§5.9）。

| 脚本 | 改形 | 同槽证明 |
|---|---|---|
| C++ `kSaveWithCommitBarrierLuaScript` | KEYS 换成 `PlayerAllData:{pid}`、`player:{pid}:owner_epoch`、`player:{pid}:save_barrier`、`player:save_pending:{<slottag>}`；键形校验同步更新（父键与 owner 键按新形状逐字比对，索引键按 `^player:save_pending:{[a-z]+}$` 校验）；topic 仍按 `db_task_zone_<z>[_g<n>]` 校验，但不再要求与索引里的 zone 相等（索引已不含 zone）；ZADD 成员与分数不变 | 前三键同 `{pid}`；索引 tag 按构造与 pid 同槽，路由层发送前再核一次 |
| C++ 归属 / 交接 / 改派 / 取证 / SQL 回执 / 战斗 lock-ctx 共 15 段 | 只改调用方传入的键名 | 全部同 `{pid}` |
| C++ 战斗结算 6 段 | 全局索引换成按 slot 索引；stage 脚本把每局的 SET 换成每人每局的串标记（`SET NX EX 86400` 语义） | 同 `{pid}` + 本 slot 索引 |
| C++ `player_team` 的 MGET | 拆成两跳 GET：先读自己的战斗锁，回调里再读队长位置（已落） | 原 FIFO 依赖是「本人删锁先于本人读锁」，读锁在本人 slot，路由层停等保证执行顺序（§5.6）；队长位置单独一条，不依赖它 |
| go/db 令牌发布 / 游标 / 释放与续租 | 锁、缓存、游标、epoch 键带 `{pid}`；`RunTokenScript` 把锁键加进 KEYS 的方式不变 | 全部同 `{pid}` |
| go/db saveoutbox 认领 / 推迟 / 计次 / 终态 / 提示 | 桶换成按 slot 索引；**删除跨 zone 迁移脚本**（索引已与 zone 无关） | 屏障 `{pid}` + 本 slot 索引 |
| go/db 屏障恢复 `restoreScript` / `mappingInspectScript` | 维护键（控制域）移出脚本，改为脚本外前后各核一次（它只是工具互斥，业务写者不读它，`barrierrestore/scripts.go` 头注释已说明）；「本 zone 64 桶为空」改为「该玩家不在其 slot 索引中」（同槽 ZSCORE）；映射三键随映射域移出 | 余下键全部同 `{pid}` + 本 slot 索引 |
| data_service `debug_import` 的 stage / commit 脚本 | stage 去掉按 zone 的桶、改用该玩家的 slot 索引；commit 去掉控制域的 `player_to_account:<id>`（脚本没用到它） | 同 `{pid}` |
| login begin / publish / 种子 / 身份补齐 | 只改键名（身份补齐的四键：父快照、玩家锁、会话、位置都属玩家域） | 全部同 `{pid}` |
| login 外观补齐的跨玩家 MGET | 改 `MGetBySlot` | 按 slot 分组 |
| scene_manager 落点 / 回滚 / 快照 / 进场去重 | 只改键名（已落）；KEYS 收进 `placementKeys` 等函数，契约测试与生产共用 | 全部同 `{pid}`，`scene_keys_contract_test.go` 按 `HashSlot` 断言，并走一遍真实进场离场核对每条 EVAL |
| scene_manager 离场、撤销无资格持有者 | 拆成玩家域与控制域两步，人数改为按成员集合计（§5.10，已落） | 第一步同 `{pid}`；第二步只碰控制域 |
| player_locator 9 段会话 Lua 与 3 段 gate 索引脚本 | 租约与 gate 索引按 slot 分桶；认领脚本不再现拼其他玩家的会话键；`""` 哨兵挪进 ARGV（第 7 批已落，§5.9） | 会话 `{pid}` + 本 slot 的租约与索引 |
| data_service 字段脚本 | 不变（已同 `{pid}`）；脚本内按索引现拼的字段键仍是同 tag，该接口无生产写者（`data_logic.go:180`） | 同 `{pid}` |

**Lua 失败顺序**：所有改形保留「全部校验先于任何写入」的结构；Redis Lua 不回滚报错前的写入，改形不新增可能在写入后报错的分支。uint64 版本比较仍按字节，不经 double。

### 5.4 补投索引与补投器

- **索引**：每个 slot 一个 ZSET `player:save_pending:{<slottag>}`，成员是 pid 十进制，C++ 写入分数仍为 0（立即可见，补投器首次认领只登记，`FirstReplayAfter` 之后才投）。索引键数量恒为 16384，与 zone 数无关。
- **成员登记**：每个补投器每 10s 在 `player:save_outbox_members:{afps}`（ZSET，成员 = 实例 UUID，分数 = 到期毫秒）上续一次心跳并清掉过期成员；存活成员数 N 取这里的计数，没有持段的副本也被计入。
- **段租约**：16384 个 slot 分成 256 段（每段 64 个连续 slot）。段 c 的租约键是 `player:save_outbox_lease:{SlotTag(64c)}`，值为实例 UUID，`SET NX PX 30000`，续约用令牌脚本。每个补投器每 10s 扫一遍全部 256 个租约键：公平份额 `share = ceil(256 / N)`；持有量不足 share 时，每次最多接管 `share − 已持有` 个无主或已过期的段；超过 share 时逐段释放。只要有一个副本存活，所有无主段都会在一个周期内被接管（N=1 时 share=256）。
- **续约前提**：续约只在「本副本最近一拍确实完成了扫描」时进行；因实例级闸（Kafka 全局故障、存储冷却等）持续停扫超过一个租约周期时主动释放所持段，让其他副本接管。
- **扫描**：每拍对已持有段内的 slot 批量（缺省 64 个 slot 一个 pipeline）执行 `ZRANGEBYSCORE -inf now LIMIT 0 Batch`，再逐成员认领。单副本持有全部 256 段时，一轮约 16384/64 × 100ms ≈ 26s [I]；N 个副本时约 26s/N。
- **与 zone 解耦后的逐条闸**：意图里的 topic 决定投到哪里（与 Scene 正常路径相同）；只核 topic 形状与世代（全局 `TopicGeneration`，三方同值由部署脚本钉住 [C]）。本 zone 主库冷却、legacy 存活这两个闸改为按意图的 zone 逐条判（带短缓存）。**不再读映射**：home_zone 是否仍等于意图的 zone 由消费端 go/db 判（`placement_route.go` 的 stale_topic 是权威），补投与 Scene 的正常投递语义一致。**前提**：消费端读不到归属（超时、断连、库不可用）必须走可重试的延后，不能与「确定不符」同走 stale_topic 死信，也不能经 `save_outbox_note` 触发 markTerminal；只有读到且不符才判 stale（映射直读 TiDB 的改造会话 2026-10-23 已确认按三态实现并补回归用例）。
- **发布器**：从「只许本实例 topic」改为「只许当前世代的 DBTask topic」，仍是 broker acks=all 才算成功。**按 topic 熔断**：某个 topic 发布失败只暂停该 topic 一个退避周期，其他 topic 照常投递（原来的「整个补投器暂停」在多 topic 下会让一个 zone 的 Kafka 故障冻结全部 zone 的补投）。
- **合服排空**：`merge_zone` 的 P4 检查改为遍历 16384 个 slot 索引，按成员屏障里的 `outbox_topic` 判是否属于源 zone；工具路径，代价 O(在途意图数)。
- **后续优化（不在本轮）**：go/db 发布脚本写 ACK 时若两表 ACK 都等于 pending，可在同一脚本里回收意图（索引与屏障同槽后可行），省掉每次存盘一到两次认领 EVAL（G4a 会话提出，方向认同）。

### 5.5 go/db

- 句柄拆成两个：**PlayerStore**（玩家域，`playerredis` 构造）承担排序锁、游标、epoch、子表缓存、屏障 ACK、资产证据、owner_epoch 读、saveoutbox；**RedisClient**（控制域）保留重试队列全族、WAITAOF 组提交、`task:result` 与发布通知、expand 状态、协议世代、能力标记、persist 引导标记。映射句柄 `Placement.Redis` 由映射直读 TiDB 的改造移除。
- saveoutbox 的「owner 与父缓存同实例」门禁换成「二者都在 PlayerStore」——在新拓扑下由构造保证（都从同一个句柄取），门禁改为核对句柄身份。
- 每任务往返数不变（每表 7 次玩家域 [C]），只是这 7 次随主节点数扩展。`kafka:expand:status:<topic>` 每任务一次 GET 是控制域单键热点，改为进程内 1s 缓存（该功能按 10-13 拍板是保留的死代码，缓存不改变语义）。

### 5.6 C++ 集群路由层

**组件**：`RedisClusterClient`（每 loop 线程一个，与 `tlsRedis` 同样 thread_local），配置 `standalone` 或 `cluster` 两种模式；standalone 就是现在的单地址行为，开发环境可以继续用一个实例。

**连接**：每个主节点一个 `RedisManager`（`unique_ptr` 持有，沿用其握手、AUTH、存活探测、重连与 §11.7 生命周期写法；回调捕获节点身份；节点移出拓扑时在 `queueInLoop` 里排队 Shutdown）。拓扑来自 `CLUSTER SLOTS`（8.x 仍可用；改用 `CLUSTER SHARDS` 只需替换解析函数），启动时向种子请求，之后在收到 MOVED、连接断开、CLUSTERDOWN 时与每 30s 刷新（限速每秒最多一次）。端点必须是 IP 字面量（运行期不做阻塞 DNS）：K8s 上每个 Redis Pod 配一个 ClusterIP Service，用 `cluster-announce-ip/port/bus-port` 宣告 Service IP，Pod 重建不换地址；种子在启动时解析一次，与现状相同。

**命令接口**：`command(cb, fmt, ...)` 与 `hiredis::Hiredis::command` 同形。内部 `redisvFormatCommand` 格式化一次（hiredis 1.2.0 `hiredis.h:140`），解析出命令名与键位置（GET/SET/DEL/EXISTS/HGET/HGETALL/ZADD/ZRANGE/ZREM/EXPIRE/MGET 的键从第 1 个参数起；EVAL/EVALSHA 按 numkeys 取 KEYS），算出 slot，经新增的 `Hiredis::formattedCommand`（内部 `redisAsyncFormattedCommand`，`async.h:146`）发往该 slot 的主节点；格式化后的缓冲一直保留到拿到最终应答，供重发使用。`formattedCommand` 要在 Windows 与 Linux 两份 `Hiredis` 封装里同时加，沿用 `command()` 的 privdata 释放约定。**多键命令的全部键必须同 slot、且都带 hash tag，否则当场返回 REDIS_ERR 并打 ERROR**（standalone 模式同样检查）。不带键的命令须用 `commandForKey(routingKey, …)` 显式指定路由（例如「存盘后紧跟 PING」的快路径，PING 必须与那次存盘同 slot）；`SCRIPT LOAD` 允许发往任一已连接节点。

**顺序保证（核心契约）：同 slot 停等。** 每个 slot 至多一条在途命令；同 slot 的后续命令在本地按调用顺序排队，前一条拿到最终应答（含路由层内部的重定向重试）之后才发下一条。不同 slot 之间照常并发、流水线。由此同 slot 的**执行顺序**与**回调顺序**都等于 `command()` 调用顺序，与重定向类型、迁移方式、Redis 版本无关。代价：同一玩家的连续命令每条多等一个机房内 RTT；一个 scene 进程约 2000 名玩家散在 16384 个 slot 上，平均每 slot 不到一名玩家，`MessageAsyncClient` 本来就是每人一笔存盘在途，吞吐基本不受影响 [I]。

**错误语义**：两类应答意味着命令**没有执行**——重定向（MOVED / ASK / TRYAGAIN / CLUSTERDOWN）与服务端暂不可用（NOREPLICAS / LOADING / MASTERDOWN / BUSY / OOM / MISCONF / READONLY）。路由层对它们按**时间预算**重试：MOVED / ASK 立即重发（ASK 先发 ASKING），其余退避 50ms 起、封顶 1s，单条命令总预算 10s（覆盖原子迁移交接停写与 `cluster-node-timeout` 5s 的故障转移）。耗尽后按「连接断开」回调空应答，**同 slot 本地排队的后续命令一并空应答收尾**——与单连接断开时「前一条空应答、后面全部空应答」的语义一致，保证「后一条执行了，前一条一定已执行或已明确失败」。空应答本来就表示「结果未知」，所有调用方都已能承受（存盘回重试队列，围栏脚本同版本重放幂等）。**这两类错误绝不以 `REDIS_REPLY_ERROR` 交给业务**，保住「错误应答 = 业务结论」的现有契约（例如 `AsyncLoad` 遇 ERROR 直接 fail-closed、凭证条件写遇 ERROR 不重试，`redis_client.h` / `player_lifecycle.cpp`）。节点连接断开时在途命令空应答回调、该节点 slot 的排队命令一并空应答、新命令 `command()` 直接返回 REDIS_ERR，与今天单连接的行为一致。

**资源上限**：每 slot 排队 ≤ 256 条，全进程排队 ≤ 65536 条，超出返回 REDIS_ERR（背压，调用方走既有重试）。连接数 = 主节点数。重连带 [0, 1s) 随机抖动，避免主切换时全体同时回连。

**重连回调**：任一节点完成握手都触发一次（带节点身份）。`RedisSystem` 的补发逻辑本身幂等（注释已声明正确性不靠补发顺序，靠闸门）；`MessageAsyncClient::OnReconnected` 改为只强制冲刷到期的待重试项，不再挪动其他节点上仍在途的元素（单连接时代挪动在途是防御性的，断连时 hiredis 已对在途命令空应答回调）。

**两个句柄**：`tlsRedis` 保留为控制域连接（世界分线目录、神秘商人、`PUBLISH assetop:wake`）；新增 `tlsPlayerRedis` 承担玩家域。gate、battle 节点不访问玩家域，不建玩家域连接（battle 不写 Redis，结算只走 match SQL 与收件箱）。

**依赖说明（§11.2）**：不引入 hiredis-cluster / redis-plus-plus。它们没有 muduo 事件循环适配，接入要么另起线程，要么自写适配层，且无法保证上面的「同 slot 停等」与「未执行类错误不外泄」两条契约；路由层需要的能力（CRC16、`CLUSTER SLOTS` 解析、按 slot 分发与重发）用现有 hiredis 1.2.0 的格式化接口即可实现，代码量与风险更可控。

**第 5a 批实现补充（2026-10-31，代码在 `cpp/libs/engine/infra/storage/redis_cluster/`，未编译）**。以下是上文没有写明、落码时定下的细节：

- **预算从成为队头起算**：排队不消耗预算，否则一次长迁移会让后面的命令还没发出就超时。
- **MOVED / ASK 立即重发设上限（5 次）**，超过按退避走。只有两个节点互相指向时才碰得到，不设上限会以 RTT 为周期空转到预算耗尽。
- **命令表 fail-closed**：只认按当前 C++ 代码核对过的命令（`redis_command_route.cpp` 的 `kCommandKeySpecs`），未登记的一律拒绝，`commandForKey` 也不例外。`PUBLISH` 属控制域，不进表。
- **单键命令不强制 hash tag**：§5.1 的「玩家域只接受带 tag 的键」目前只在多键路径上检查。第 3 批改名之前现有单键大多没有 tag，强制就接不上线；改名完成后再决定是否打开。（第 5c 批已打开，见下。）
- **维护前沿**：`Hiredis::MaintenanceState` 把任何错误应答记成 UNKNOWN，MOVED 会让进程永远过不了维护排空，所以 `formattedCommand` 多一个可选的 `NotExecutedPredicate`，判定为「确定没有执行」的错误不算 UNKNOWN。路由层接受的每条命令也单独计入 `ProcessMaintenance()` 直到最终结果：排队与退避中的命令不在任何连接上，否则排空会把它们漏掉。
- **`Hiredis.h` 实际有三份**：`muduo_windows`（编译 muduo.lib）、`tools/archived/muduo_linux_overlay`（Linux 覆盖件）、`third_party/muduo/contrib/hiredis`（业务代码 `#include <muduo/contrib/hiredis/Hiredis.h>` 实际命中的就是这份）。改接口要三处同改；`.cc` 只有前两份在编译。
- **重连抖动**靠给每个节点的 `reconnectIntervalSec` 加 `[0, 1s)` 的随机量实现，`RedisManager` 未改。
- **断连发现有延迟的一种情况**：退避中的队头要等下一次重试才发现节点断了（最迟 1s）；在途命令是立刻空应答。`RedisManager` 没有断线回调，这条延迟换来不改它。
- **种子回退**：全部已知节点连不上持续 5s（`seedFallbackDelay`）才把种子连回来。

**第 5b、5c 批实现补充（2026-10-31，未编译）**：

- **句柄放在哪**：`tlsPlayerRedis` 的声明与定义在 `redis_cluster_client.{h,cpp}`，`redis_manager.h` 只留一行指引。`redis_cluster_client.h` 依赖 `redis_manager.h`（`RedisConnectOptions`），`infra` 依赖 `thread_context`；句柄放进 `thread_context` 就成了头文件与静态库的循环。
- **配置**：`game_config.yaml` 的 `player_redis`（`GameConfig.player_redis = 9`）：`mode`（standalone | cluster）、`seeds`（host:port 列表）、`password`，环境变量 `PLAYER_REDIS_PASSWORD` 覆盖口令。整段缺失 = standalone，地址与口令沿用 `zoneredis`。写了 `seeds` 就是另一个实例，口令不继承。加载器只读入与补默认（`Node` 不看 `readGameConfig` 的返回值，gate、battle 也不该被这段配置拖累）；取值错误与主机名解析失败由 `RedisSystem::Initialize` 拒绝启动。建连也在那里，所以只有 scene 连玩家域。
- **键的归属**：玩家域是 `player:{id}:owner_epoch`、`handoff`、`handoff_fence`、`location`、`sync_digest`，`battle:lock:{id}`、`battle:ctx:{id}`。控制域是 `world_channel_directory:zone:*`、`mystery_merchant:zone:*` 和 `PUBLISH assetop:wake`。上文提到的「存盘后紧跟 PING」「battle 节点孤儿清理」「`MessageAsyncClient`」都已随存档移出 Redis 删除，C++ 现在没有无键业务命令，`commandForKey` 暂无生产调用方。
- **接缝守卫打开**：`tlsPlayerRedis` 用 `HashTagPolicy::kAllKeys`，单键命令也必须带 tag。两个句柄在单实例开发时指向同一个实例，错发的键照样读写成功，要到上集群才变成读写落在两边；守卫让它当场 `REDIS_ERR`。`RedisClusterClient` 的缺省策略仍是只查多键。
- **`connected()` 为真时 `command()` 也可能 `REDIS_ERR`**（这个 slot 的节点断了、排队满了）。忽略返回值的 10 处（`player_battle.cpp` 8 处、`player_team.cpp` 2 处）补了检查，走与「Redis 断开」相同的降级。其余调用点本来就检查。
- **调用点里没有针对 MOVED 或连接地址的假设。**「ERROR = 确定没执行」的分支（如 `BeginTravelHandoff`）继续成立：READONLY、LOADING 这类错误到不了业务，预算耗尽走空应答，落到各处「结果未知」的分支，那是偏保守的一侧。
- **重连回调**：任一主节点握手都触发，表里的条目不一定在这个节点上。补发幂等：撤回是条件写，A2′ 与核实只重发没有在途的条目。主节点多时切主或网络恢复会连着触发很多次，所以限成每秒一轮，否则最坏是「主节点数 × 待撤回表上限」条带整段 Lua 的命令堆在 loop 线程上。
- **可观测**：`RedisSystem` 的 30s 快照多一行 `[PlayerRedis]`，有变化才打，守卫拒绝、排队满、预算耗尽有增长时是 WARN。

**同一连接 FIFO 依赖的逐处结论**（§3.3 的清单，按当前代码核对）：

| 依赖 | 结论 |
|---|---|
| 条件删锁先于跟随链读锁；reaper 删锁先于登录复查读锁；`SET battle:lock` 先于 `SET battle:ctx` | 同玩家同 slot，路由层停等，仍成立 |
| A2′ 先于载入 | 载入已是 go/db 的 gRPC，本来就没有连接顺序。`HandlePlayerAsyncLoaded` 的闸门在 A2′ 确认前暂存载入结果 |
| 放弃补写、凭证补写、取证 | 靠 `handoff_fence` 的 CAS 或一段 Lua，不靠顺序（r2-lc#2） |
| 跟随链：读本人锁 → 读队长位置 | 跨玩家、跨 slot，回调链已是显式的（第 3 批） |
| 存盘后紧跟 PING、battle 节点孤儿清理三步 | 代码已删 |
| 条件删锁（玩家域）→ `PUBLISH assetop:wake`（控制域） | 原来同连接，现在没有顺序。不依赖：消费端重投的指令到 scene 只看 `InBattleComp`（发布时已摘），不读锁 |
| 登录读锁（玩家域）→ 进场唤醒 `PUBLISH`（控制域） | 原来同连接，现在没有顺序。slot 迁移或切主时读锁可能卡到 10s，重投的指令可能先于冻结重建到达。不改：周期重投本来就能落进这个窗口，读锁空应答时窗口更长（登录复查 30s），战斗闸对这个窗口不是硬保证 |

### 5.7 Go 客户端

- 玩家域句柄统一为 `redis.UniversalClient`，由共享构造器 `shared/playerredis` 按配置产出（第 1 批已落）：`Addrs` 一个且 `Cluster=false` 为单实例；`Cluster=true` 为 `ClusterClient`。每节点池上限显式化（缺省每节点 4 条，各服务可按现值设定，例如 go/db、login 单实例阶段沿用 20），`MinIdleConns=0`，空闲 60s 回收；`ContextTimeoutEnabled` 缺省开（ctx 截止约束套接字读写，player_inbox 500ms、guild 2s 这类预算依赖它）；`PoolTimeout` 可配。
- **不自动重放结果未知的命令**：单实例 `MaxRetries=-1`；Cluster 模式下 go-redis 的 process 循环会在 EOF / 超时后自动重发同一命令（最多 MaxRedirects 次），可能重放已在服务端执行的 Lua。构造器用 `OnNewNode` 给每个节点客户端挂钩子，把命令发出后遇到的网络错误包成不可重试的「结果未知」错误，只保留 MOVED / ASK / TRYAGAIN / CLUSTERDOWN / LOADING 这类「未执行」重试——与 C++ 路由层的语义一致。
- 构造器挂发送前守卫：EVAL / EVALSHA / MGET / DEL 等多键命令的键必须同 slot、玩家域的键必须带 tag，否则直接返回错误，单实例开发时同样生效。
- 跨玩家 MGET 统一走 `playerredis.MGetBySlot`：按 slot 分组后 pipeline（ClusterClient 自动按节点拆批），结果按输入顺序还原。
- 全库扫描分两种：离线全量任务用 `playerredis.ScanEachMaster`——逐主节点扫完，任何一个节点失败就整体失败；扫描前后各取一次拓扑指纹（每个主节点的节点 ID、run_id 与所持 slot 段，以及有无进行中的迁移），扫描所用的主节点集合取自指纹，前后不一致、或看到进行中的迁移，就返回 `ErrTopologyChanged`（fail-closed，调用方不得据此断言「键不存在」）；按请求分页的在线目录用 `playerredis.ScanPage`（第 6 批加）：游标 = 指纹摘要 + 主节点序号 + 节点内游标，指纹变化即游标失效、从头开始。
- go-zero 句柄（scene_manager、friend、match）在玩家域改用 go-redis `UniversalClient`；控制域照旧。

**第 6 批实现补充（2026-10-31，未编译）。**

- 包名从 `shared/playerstore` 改为 `shared/playerredis`：它是玩家域 Redis 的客户端，和 TiDB 的 `player_store` 表、`go/db/internal/playerstore` 不是一回事。
- **配置**：各服务统一用 `playerredis.Config`，配置键 `PlayerRedis`（`Addrs`、`Cluster`、`Username`、`Password`、`PasswordEnv`、`TLS`、`PoolSizePerNode`、`ConnMaxIdleSeconds`、`DialTimeoutMs`、`ReadTimeoutMs`、`WriteTimeoutMs`、`PoolTimeoutMs`、`DisableContextTimeout`）。没写 `Addrs` 时指向这个服务现有的 Redis 单实例（`OrDefault`），`etc/*.yaml` 只把连接池上限写明，开发环境和还没渲染这一段的 K8s ConfigMap 都不用改。`ValidateSection` 拒绝 `Cluster: true` 却没写种子：那样会静默落到现有实例上。
- **`ScanPage` 的指纹与 `ScanEachMaster` 不同**：它串在在线请求上，逐主节点读 run_id 是每页 2 × 主节点数次往返，随规模线性增长。所以只读一个节点的 `CLUSTER NODES`（一次往返），指纹取主节点 ID 与 slot 段。代价：主节点原地重启看不出来，旧游标在新进程上可能漏掉或重复一些键。在线目录是展示面，可以接受；要「扫完 = 没有遗漏」的任务用 `ScanEachMaster`。单实例指纹是常量，不多一次往返。
- **`friend` 在线目录游标**：`v2:<摘要>.<主节点序号>.<节点内游标>:<这一批消费到哪>`，最长 43 字节（上限 48 不变）。客户端不解析，`v1` 游标判参数错误，从空游标重拉即可（未上线，不留兼容）。
- **铸造通道**：`playerredis.NewPinned` 不挂守卫的管道规则（EVAL + WAITAOF 要进同一条 pipeline，只发给节点客户端，不会自动重发），键的规则由 `EvalAndConfirm` 发送前调 `CheckKeys`。按第一个键取主节点、重定向类重发、切主后的回滚见[回退防护 §4.3.3](zone-redis-regression-guard-20261022.md)。启动探针逐主节点发。
- **守卫**：玩家域句柄一律 `playerredis.New`，guild 的摘要专用客户端同样如此。guild、player_inbox、trade 的控制域客户端（选主锁、`assetop:wake`）不经守卫，它们的键不带 tag。
- **没有 go-zero 的熔断器**：玩家域句柄不挂熔断，失败面由超时和 ctx 截止约束。Redis 整个不可用时，每个请求要等到超时才失败，而不是概率性快速失败。

### 5.8 其他服务的玩家域读写

friend（会话、父快照、同步摘要）、guild（会话、位置、战斗锁、同步摘要）、match（会话、战斗锁、父快照、同步摘要）、mail（会话、位置）、trade（位置）、player_inbox（owner_epoch、位置）、login（父快照、玩家锁、登录代次、合服提示、外观补齐）各自新增 `PlayerRedis` 配置并改用上节的构造器；跨玩家 MGET 改 `MGetBySlot`，friend 在线目录改 `ScanPage`（游标格式随之变化，是客户端可见的不透明游标，按 [G4a §20.5](shared-redis-split-by-purpose.md) 已登记的两版同批评审处理）。guild 的 `assetop:wake` 订阅、去重键、战斗活动结果仍在控制域。

**第 6 批核对（2026-10-31）。** 父快照已不在 Redis，上面那几项随之不存在。data_service 与 chat 不读玩家域键（只有 `player:name:<id>` 私有缓存和 `LoginRedis`），不用改。login 的玩家键由它自己的批次处理，读登录态的 `LoginRedis` 句柄属第 11 批。各服务的分工：

| 服务 | 玩家域句柄（`PlayerRedis`） | 没写 `Addrs` 时指向 | 控制域留下的 |
|---|---|---|---|
| scene_manager | location、修订、离场等待、冷却、去重、owner_epoch、handoff、handoff_fence | `Redis` | `Redis`：场景路由、成员集合、世界频道、选主锁 |
| friend | 会话、同步摘要 | `Redis` | `FriendRedis`：私有键。`SharedRedis` 字段删除，`Redis` 段只作回落目标 |
| match | 会话、位置、战斗锁、同步摘要 | `Redis` | `MatchRedis`：私有键。`SharedRedis` 字段删除，`Redis` 段只作回落目标 |
| guild | 会话、位置、战斗锁、同步摘要 | `PlayerLocatorRedis` | `PlayerLocatorRedis`：`assetop:wake` 与去重键；`RedisClient`：私有键、选主锁 |
| trade | 位置 | `SharedRedis` | `SharedRedis`：`assetop:wake` 与去重键、选主锁 |
| player_inbox | owner_epoch、位置 | `CoreRedis` | `CoreRedis`：选主锁 |
| mail | 会话、位置 | `SharedRedisAddr` | 无 |

`playersync` 的 go-zero 适配器没有调用方了，随之删除。

### 5.9 player_locator

按 [B0-1 设计 §11](player-locator-gate-session-index.md) 的既定形状落地：会话 `player:session:{pid}`；租约四键与 gate 索引按会话 pid 的 slot-tag 分桶，会话 CAS 与本 slot 的租约、索引在同一脚本里原子维护（不变量 I1 不变）。`claimExpiredLeasesScript` 改为按 slot 认领，只返回成员与令牌、不在脚本里读其他玩家的会话；逐人的提交脚本声明会话键后再核状态。`""` 哨兵挪进 ARGV。LeaseMonitor 与死 gate 清扫按 §5.4 同一套「成员登记 + 段租约」分片。补建 / 审计用 `ScanEachMaster`（逐主节点核 run_id 与迁移状态）。就绪 / 代次 / 审计时间这几个全局控制键留在控制域。能力键按设计换 v2 前缀。

**第 7 批实现补充（2026-10-31，代码在 `go/player_locator/internal/logic/`，未编译）**。详细状态机见 [B0-1 设计](player-locator-gate-session-index.md) §2–§4，这里记总设计层面定下的事：

- **两套分片**：`lease_monitor` 只依赖 Redis，`gate_sweep` 还依赖 etcd，各自一套成员登记与段租约。合成一套的话，etcd 不可达时清扫让出段，断线租约也跟着没人扫。代价是每个副本每 10s 多读 256 把键，2Z = 890 个副本时全服约 4.6 万 GET/s，散在各主节点上 [I]。
- **读失败的段这一周期不算持有**：不续、不扫，到期后归能连上它的副本。**有段读失败的副本这一周期也不算成员**（撤销登记，照常持有读得到的段）：否则它占着一份份额却接不了那些段，别的副本又持满了自己的份额，那些段就一直没人扫。这两条比 §5.4 多，只要有一个副本读得全，它的份额就够接下全部无主段；所有副本都读不全时成员数归零按 1 算，各自接管读得到的。键类型不对不算读不全，只跳过那一段。
- **认领分两步**：按 slot 认领脚本只碰本 slot 的租约键，只返回成员与令牌；逐人的钉载荷脚本声明会话键后核令牌、把会话字节钉进 `claim_payloads`。commit / rearm 同样逐人。
- **`""` 哨兵**：KEYS 恒定长度，「这一侧没有索引」的位置放本 slot 的登记集占位，脚本凭 ARGV 里的 gate 判断。
- **写入顺序**：可能因键类型不对而报错的簿记排前面，会话字节最后写，Redis 不回滚报错前的写入，报错时会话原样。
- **登记集收紧**：一把索引键空了就把 gate 移出本 slot 的登记集（`redis.pcall`，不是不变量）。不收紧的话，一个长寿 gate 迟早碰遍 16384 个 slot。
- **清扫不选主，写者检查与补建 / 审计仍由一个 leader 跑**，就绪、代次、审计时间、选主锁留在控制域。leader 只有这些事，一个 leader 也读不动全部登记集。
- **LeaseMonitor 探测量是常数**：每拍取「持有 slot 数 × 拍长 / `ScanRoundSeconds`」个 slot（默认 16s），全服约 2048 条 ZCOUNT/s，与副本数无关。代价是租约到期后最迟晚 16s 才被认领。登出清理的首拍不等这个：同账号首次登录在清理期间会被拒，所以 MarkOffline 入队后到点叫醒本进程处理那个 slot（认领脚本才是互斥手段，没有段也能认领），叫不醒时常规轮转兜底。
- **gauge 按索引键数计，不逐键 SCARD**：目标规模下索引键约等于在线人数（一个 gate 约 4500 人散在约 3900 个 slot 上 [I]），逐键 SCARD 每拍是千万条命令。`gate_index_members`、`dead_gate_members` 因此变成每副本一份的键数，跨副本求和约等于人数。
- **容量**：索引约 1.75e7 把键加同样多的登记成员，约 3–4 GB [I]，比全局索引多一倍多，是分桶的代价。一遍审计接近 19 小时（第 1 阶段 SCAN 约 9 小时，第 2 阶段逐把键约 10 小时）[I]，日审计已偏紧，后续可把第 2 阶段按批走 pipeline。
- **`ScanEachMaster` 的代价**：SCAN 失败不重试，玩家域客户端又不自动重放，一次超时就作废一遍。SCAN 是只读的，后续可以在 `ScanEachMaster` 里对同一游标重试。

### 5.10 scene_manager 离场拆分

**定稿（第 3 批，2026-10-31，已读完全部计数入口，未编译）。** 原 `luaDeletePlayerLocationIfUnchanged` 把删除位置与递减场景 / 节点人数绑在一次提交里。分到两个域之后计数器做不到「谁删谁减」：第二步在两步之间失败，人数永久多计，实例永不销毁。所以人数改按成员集合计，v1 的墓碑方案作废。

**键。** 控制域 `instance:<scene>:members`、`node:zone:<z>:<node>:members`，SET，成员是十进制 player_id，人数 = SCARD。`instance:<scene>:player_count`、`node:zone:<z>:<node>:player_count` 已删。

**不变量。** location 指向 (scene, node) ⇔ 玩家是这两个集合的成员。由三条约定收敛，任意交错都成立（`scene_membership.go`）：

1. 预占时 SADD，让并发选线看到在途请求。
2. 落点提交后再 SADD 一次：预占之后、落点之前，别的请求可能已经把它摘掉。
3. 摘成员时，location 此刻指向这里的部分不摘；其余 SREM 之后再读一次 location，这时指向这里就加回去：SREM 之前并发进场可能已经落点。

**为什么要第 3 条。** 计数器的「+1 再 -1」对重叠是安全的，集合不是：

- 自动选线碰巧选中玩家已在的频道，这次进场又被拒：预占和已有的成员身份是同一个成员，释放不能把人移出去。
- 同节点换图：摘旧场景时节点集合不能动。
- 死节点接管落回同一个频道。
- 并发请求的释放可能摘掉已落点的成员。

有了第 3 条，调用方不用判断旧新场景、旧新节点是否相同，也不用区分预占是不是本请求加的。

**残留与对账。** 请求路径摘不掉的成员由领导者后台对账收掉(`scene_member_reconcile.go`)。三种来源:

- scene_manager 在预占之后、落点之前崩溃,或落点之后、摘旧场景之前崩溃。
- 离场第二步失败:player_locator 保留清理任务重试,但这时 location 已删、生产上 `session.SceneId` 恒 0,重试解析不出场景号,直接 ack。
- 强制销毁之后才离场:`scene:<id>:node` 已删,离场查不到节点。强制销毁时,场景里剩的成员逐个走 `removeSceneMember` 摘节点集合,location 仍指向这个节点的留着(玩家可能还在节点上),离场之后由对账摘。

在途预占的 location 同样不指向这里。它是实例里唯一的成员时,摘掉会让空闲回收销毁实例,玩家落到一个已经没了的场景。所以成员要**连续 2 分钟**(`sceneMemberStaleGrace`,长过一次进场请求)不指向这里才摘;期间再指向这里就重新计时。记录只在进程内存,换领导者重新计时,一遍里没再看到的记录作废,上限 65536 条,记不下的这一遍不动。指标 `scene_manager_scene_member_reconcile_members_total{outcome="skipped"}` 含宽限内的成员。

**入口。**

| 入口 | 动作 |
|---|---|
| 自动选线 `luaReserveBestWorldChannel` | 按 SCARD 选人最少的线并 SADD；同一玩家重复预占不累加 |
| 指定场景 `luaAtomicAddSceneMember` | `scene:<id>:node` 还在才 SADD，否则回 -1、不建键 |
| 落点提交、同落点重连、回滚恢复旧落点 | `addSceneMember` |
| 拒绝时释放预占、离开旧场景、接管收尾、回滚时摘新落点 | `removeSceneMember`；这些路径的主结果已定，失败只记日志 |
| 离场 | 第一步玩家域 `luaDeletePlayerLocationIfUnchanged`，「键已不在」排在最前，go-redis 原样重发回 0 而不是 -1；第二步控制域摘成员。除「未释放」之外的结局都做第二步，失败回错误，player_locator 保留清理任务重试 |
| 离场重试 | LeaveScene 读到 location 已不在或已换场景时，同样摘请求里的场景，这是第二步失败后的恢复路径 |
| 撤销无资格持有者 | 7 键玩家域 Lua，结果为「已撤销」「已不在」时摘成员 |
| 空闲销毁 `luaAtomicDestroyInstance` | SCARD 大于 0 放弃，等于 0 才删场景键 |
| 强制销毁 `luaAtomicDestroyInstanceForce` | 脚本返回残留成员，调用方把它们 SREM 出节点集合 |
| 死节点收尾 `luaFinishDeadNodeReconcile` | 与任务出队同一段脚本删节点集合 |
| 负载刷新 | pipeline 里 SCARD；键不存在读作 0，WRONGTYPE、网络错误、熔断不按 0 写 |

**代价与残余。**

- 每次摘成员多读一次 location（共两次）。第 3 条先读再摘是为了不让人数下探：排空收尾（`world_autoscale.go`）、机会迁移（`world_rebalance.go`）只读一次人数就做决定，看到 0 就会动一个有人的频道。只有「第一次读之后、SREM 之前恰好有并发进场落到这里」时才下探一次往返。
- scene_manager 在预占之后、落点之前崩溃，或落点之后、摘旧场景之前崩溃，留下一个 location 不指向这里的成员，请求路径上没有人会再摘它。后果与计数器时代多计一次相同（实例不销毁、选线躲着它），由下面的对账收掉。
- 场景被改派过时，玩家当初进的是旧节点的集合，而改派只发生在旧节点已死之后，它的集合由死节点收尾删掉。

**句柄（第 6 批，2026-10-31，未编译）。** `svcCtx.PlayerRedis` 是玩家域，`svcCtx.Redis` 是控制域。`removeSceneMember` 读 location 走玩家域，写成员走控制域；离场两步分别在两个句柄上。`scene_keys_contract_test.go` 把两个域放到两个 miniredis 上走一遍进场、重连、换图、回滚、离场：哪条路径用错句柄，流程就走不通。

**成员集合对账（第 6 批，`scene_member_reconcile.go`）。** 只有领导者跑（`isLeader`，降级即停并丢掉遍历位置）。

- **集合来源**：登记过的 zone ∪ 有活节点的 zone → 负载集里的节点 → 节点的成员集合，以及它名下场景的成员集合。全部点查，不 SCAN 键空间。死节点的集合由死节点收尾删，不归这里管。
- **预算**：每轮最多读 `MembersPerRound`（缺省 2000）个成员的 location、发 `ScansPerRound`（缺省 512）条 SSCAN，间隔 `IntervalSeconds`（缺省 10s），用完就停，下一轮接着走。遍历位置只在进程内存里，换领导者从头开始。小集合（intset / listpack）SSCAN 一次回完、可能超过预算，余下的成员留在内存里下一轮接着核，不丢。走完一遍约 成员总数 ÷ 2000 × 10s：100 万在线（每人两个集合）约 2.8 小时。
- **判据**：`MGetBySlot` 读 location，按 `locationOverlap` 判是否指向这个集合。指向就不动；不指向就走 `removeSceneMember`（第 3 条：SREM 之后再读一次，指向这里就加回去）。location 写坏、读 location 失败都不动，不把「不知道」当成「不指向」。成员不是合法 player_id 时直接 SREM，并打 Error。
- **与在途请求的交错**：预占还没落点时被摘掉是安全的，落点提交之后会再 SADD（第 2 条），预占期间人数少算一个。
- **指标**：`scene_manager_scene_member_reconcile_members_total{outcome=ok|removed|skipped|malformed|error}`、`scene_manager_scene_member_reconcile_passes_total`。`removed` 长期不为 0 说明 scene_manager 经常在进场中途崩溃，或有摘成员的路径漏了。没加告警（`deploy/` 不在这批）。

### 5.11 战斗结算

> **2026-10-31**：`battle:settlement:*` 已全部删除，本节作废，原文保留。战斗锁与上下文（`battle:lock:{<pid>}`、`battle:ctx:{<pid>}`）第 3 批已带 tag，C++ 的 7 段 lock-ctx 脚本同 slot。

结算索引按 slot 分桶，与该玩家的 pending / lock / ctx 同槽；stage 去重从「每局一个跨玩家 SET」改为「每人每局一个标记串」，battle 节点结束时按名单删除或交给 TTL。battle 节点的积压扫描与孤儿清理按 slot 遍历：每个 battle 节点按节点号对 slot 取模负责一份，另以低速全量轮转兜底（每秒 1 个 slot，约 4.5 小时一轮），保证节点数变化期间不会有 slot 永久无人扫。三步清理在同一 slot 内由路由层停等保序。

### 5.12 Pub/Sub

`assetop:wake` 留在控制域（C++ 经 `tlsRedis` 发布，guild / trade 在控制域订阅）。`task:result` 通知同在控制域。玩家域不使用 PUBLISH（Cluster 下普通 PUBLISH 会全集群广播）。

### 5.13 映射域：移出 Redis（2026-10-23 方向变更）

据「账号登录逻辑/物理服瓶颈」会话转述，负责人 2026-10-23 拍板：玩家归属 / 落点映射改为**直读 TiDB**，撤掉 Redis 映射副本——`player:zone` / `player:placement` / `player:maprev` 三族、`mapping:route:ready` / `warm` / `watermark`、data_service 对账 leader 与 W1 发布 + WAITAOF、`merge:in_progress` 的 Redis 镜像都将从 Redis 移除，登录、进场、go/db 直接 SQL 点查，go/db 在同一 TiDB 事务里核落点。具体设计以 [player-route-mapping-durability-proposal.md §15](player-route-mapping-durability-proposal.md)「2026-10-23 负责人拍板：映射改为直读 TiDB，撤掉 Redis 副本」为准：go/db 在写事务里 `FOR UPDATE` 锁读全局库 `player_route`（锁序「路由行 → 玩家数据行」，§15.3），`db:capability:zone:<z>` 一并删除，§5.4 要求的「路由读失败走可重试延后、只有确定不符才判 stale」写在其 §15.5 第 1 条并配回归用例；落码按其 §15.7 分批。本稿不再为映射域设计分片；对本稿的影响只有两点：saveoutbox 改为不读映射（§5.4）；go/db 的映射句柄由该改造移除，本稿第 4 批只拆「玩家域 / 控制域」两个句柄。

## 6. 连接拓扑与预算

### 6.1 公式

每个玩家域主节点的客户端连接数：

```
C_master = N_cpp + Σ_svc ( R_svc × p_svc )
```

- `N_cpp`：访问玩家域的 C++ 进程数（只有 scene；gate、battle 不连），每进程每主节点 1 条。
- `R_svc`：Go 服务副本数；`p_svc`：该服务每节点实际活跃连接（池上限缺省 4，空闲 60s 回收；按往返率估算，绝大多数服务稳定在 1–2 条）。
- 每个进程的玩家域连接数 = 主节点数 M（C++）或 ≤ 池上限 × M（Go）。副本不接客户端连接（不读副本）。

### 6.2 目标规模下的数值 [I]

按 2000 万 PCU 口径（[容量模型](capacity-model-hok-scale.md)、[G4a §16](shared-redis-split-by-purpose.md)）：scene 1,778–8,889 个、物理 zone 约 200 个、每 zone Go 服务约 10 个进程。则 `N_cpp ≈ 2,000–9,000`，Go 侧约 2,000 进程 × 1–2 条 ≈ 2,000–4,000，**每主节点约 4,000–13,000 条**。

- Redis 单节点在 `maxclients` 显式调高（40000）与 nofile 足够时可承受；空闲连接的开销是几 KB 的客户端结构，epoll 只为活跃 fd 付费（§14.3 第 6 项实测每条空闲连接的内存）。C++ 进程侧 M=250 时多 250 个 fd，在 `kNonClientReserve=4096`（`open_file_limit.h:45`）之内。
- 主切换时约 1 万连接在数秒内回连到新主：重连抖动（§5.6）把它摊开；AUTH 每条一次，开销小（§14.3 实测回连时长与错误率）。

### 6.3 与今天的对比

今天所有进程挤一个实例（峰值 ≈ 264·Z + 1075，Z≈100 时触及 32000 告警线 [I]）。分片后控制域只剩非玩家流量；玩家域每节点的连接数与「访问玩家域的进程数」成正比、与主节点数无关，**加主节点不增加单节点连接数**，只增加每个客户端进程的连接数。

### 6.4 下一阶段：接入层（条件触发）

当「每主节点连接数」预计超过 20,000，或客户端进程数超过 2 万时，引入接入层把扇入收敛到 `接入层副本数 × M`。准入条件：接入层必须把上游断连报告成连接断开（不能变成错误应答），必须保同 slot 停等语义，必须能把 WAITAOF 落到实际写入的节点。现成代理不满足第一条时就自研。触发前不部署（YAGNI）。另一个更便宜的方向是让 Go 服务按「一个进程服务多个 zone」部署，直接减少进程数——它与物理 zone 数挂钩，不在本稿范围，列入 §16。

## 7. 持久性与故障转移

- **副本与写入门槛**：缺省每主 1 副本，`min-replicas-to-write 0`。理由：集群模式下与多数派失联的主在 `cluster-node-timeout` 后自行停止服务（`cluster-allow-reads-when-down no`），防脑裂主要靠这一点；而每主 1 副本时若保留 `min-replicas-to-write 1`，任一副本重启或全量同步、或故障转移后新主暂时没有副本，整片都会回 NOREPLICAS 拒写，持续到副本同步完成（每主 25 GB 量级是分钟级）——比今天 1 主 2 从的 redis-ha（`redis-ha.yaml:37-38`）可用性更差。需要更强持久性的部署选**每主 2 副本 + `min-replicas-to-write 1`**，代价是节点数 ×1.5（§10.1 同步给出两种口径）。AOF `appendfsync everysec`，PVC 持久化；显式 `maxmemory` + `noeviction`（玩家态不允许淘汰），内存告警先于上限。
- **复制是异步的，与今天相同**：主节点应答之后、复制之前宕机，最后一段写入可能随故障转移丢失；被隔离的旧主在 node-timeout 内收到的写同样会丢。分片不改变这一点，但把影响面从「全部玩家」缩小到「该主节点的 1/M 玩家」。
- **丢尾的后果是整体回到较旧的一致版本**：两表 ACK 与 pending 同在一个 save_barrier hash，且 ACK 总在 pending 之后写，异步复制保留的是写入流的前缀，所以不会出现「ACK 超前于 pending」，而是父快照、pending、ACK 一起回到旧版本。**防线是实例身份戳**：屏障里记录写入时执行节点的 `run_id:master_replid`，login 冷加载要求它等于当前执行节点，不等就读两张 SQL 行核验、必要时按 SQL 采纳（`parent_commit_barrier.go:20-30`）。故障转移会换 `master_replid`，所以丢尾的玩家必然走核验路径，读到的是 SQL 里的新版本，不会读回旧档。**玩家域不变量**：身份戳按执行节点取值，任何改动不得删除或绕过这条核验。
- **迁移同样触发核验**：键随 slot 搬到别的主之后身份戳必然对不上，迁过去的玩家下一次冷加载都会走两行 SQL 核验加提交脚本。扩容迁 X% 的 slot，就有约 X% 的玩家在下次登录时多两次 TiDB 点读 [I]（§8.4 计入并限速）。
- owner_epoch 的 INCR 丢失会让代次回退，理论上给旧持有者重新放行——今天就存在的风险，建议后续在铸造后同一节点连接上发 `WAITAOF 1 1`（scene_manager 是铸造唯一入口），列为 §16 的独立项，不并入分片批次。**2026-10-24 已落码（未编译）**：失败语义、时间预算、延迟与三种拓扑的取值见 [zone Redis 回退防护 §4.3.3](zone-redis-regression-guard-20261022.md)；本域每主 1 副本时取 `1 1`，代价是该主的副本不可用期间这 1/M 玩家的首登与换节点回 7。第 6 批把 scene_manager 换成玩家域句柄时，铸造通道已按该节列的三处改完（按 slot 取主节点再发 pipeline、重定向类重发、切主后的回滚），未编译。
- **不可写窗口**：主节点宕机到副本提升约 `cluster-node-timeout`（建议 5s）加选举时间；该主的 slot 在窗口内不可写。C++ 路由层按 10s 预算重试后空应答，Go 侧按错误重试或失败。

## 8. 扩容、缩容与迁移

### 8.1 扩容流程（运维手册摘要，Redis ≥ 8.4 原子迁移）

1. 新主节点（及其副本）以集群模式启动并 `CLUSTER MEET` 加入。
2. 预检：`cluster_state:ok`、全部 slot 已覆盖、没有进行中的迁移、新节点版本 ≥ 8.4。
3. 在目标节点执行 `CLUSTER MIGRATION IMPORT <slot 段>`（或 `redis-cli --cluster rebalance`，全部节点 ≥ 8.4 时它自动走原子迁移）：源节点 fork 快照经 RDB 通道发给目标，增量写经主通道持续转发；目标追平后源节点对这些 slot 短暂停写，目标排空缓冲并接管，配置经集群总线原子更新；源节点随后清理已迁出的键（`third_party/redis/src/cluster_asm.c` 头注释的 8 步）。
4. 观察：迁移期间客户端只会看到交接瞬间的 MOVED 与极短的停写；C++ 路由层的重定向计数与 Go 错误率不应出现非重定向类错误。完成后核对每个主节点的 slot 数、键数、内存与 ops 分布。
5. 中断：原子迁移失败时任务回滚，slot 仍归源节点（交接前源节点一直保有全部键），用 `CLUSTER MIGRATION STATUS` 核对后重试。

### 8.2 迁移期的正确性

- 交接前：读写都在源节点，目标节点只是后台追平；源主若在交接前宕机，被提升的副本拥有完整数据（交接前源节点没有删除任何键），迁移任务失败回滚，不会出现同键两份。
- 交接时：源节点对迁移中的 slot 短暂停写，写命令被阻塞或回可重试错误；路由层在 10s 预算内重试。
- 交接后：源节点对该 slot 回 MOVED，客户端刷新拓扑后直连目标。不存在「旧拓扑的客户端在旧节点写成功」的窗口。
- 玩家的全部键同槽，脚本在交接前完整地在源节点执行、交接后完整地在目标节点执行。
- 在途任务与补投：索引与屏障同槽一起迁；补投段租约键与成员登记键也是普通键，随所在 slot 迁移。

### 8.3 逐键迁移（Redis < 8.4 或应急）的危险，仅作应急手段

`redis-cli --cluster rebalance` 在 < 8.4 时逐键 `MIGRATE`：迁移窗口内，**声明了缺失键（例如平时不存在的 handoff、首次存盘前的 save_barrier）的多键脚本在整个 slot 迁移期间持续 TRYAGAIN**（不只是「键分处两地」时）；`MIGRATE` 同步阻塞源、目标两个实例；迁移中源主故障转移，被提升的副本不知道迁移状态，已迁到目标的键在它那里视为不存在，会产生同键两份与 owner_epoch 回退。因此玩家域禁止在 < 8.4 的节点上做常规扩容；万不得已用逐键迁移时，参与节点发生故障转移须立即暂停，先在新主上补发 `CLUSTER SETSLOT <s> MIGRATING <dst>` 恢复 ASK 语义或对该 slot 停写后再 `--cluster fix`，且必须接受迁移窗口内相关玩家的进场、存盘、交接持续失败。

### 8.4 缩容、回退与代价 [I]

- 缩容是反向迁移（把待下线节点的 slot 全部迁走后 `del-node`）。回退等于再迁一次 slot，没有键形变化，不需要停写。
- 代价：常驻 1–2 TB / 16384 ≈ 每 slot 60–120 MB、约 15 万键。原子迁移按 RDB 快照传输，速度远高于逐键 MIGRATE（官方称最高约 30 倍，本仓未测）；单 slot 时长、交接停写时长、迁移期间源节点 fork 的内存与 CPU 开销都以 §14.3 实测为准，运维按实测控制并发迁移的 slot 数。
- 迁移后的核验读放大（§7）：迁走 X% 的 slot，约 X% 的玩家下次冷加载多两次 TiDB 点读；扩容宜在低峰期分批进行，单批迁移比例按 TiDB 余量定。

## 9. 必须保留的保证逐条论证（交接 §5）

| # | 保证 | 论证 |
|---|---|---|
| 1 | 唯一有效写者 | owner_epoch、handoff、location 及修订键同 `{pid}`，所有围栏脚本只改键名不改判定；原子迁移交接前只有源节点、交接后只有目标节点受理写，任何脚本都只会在恰好一个节点上整段执行。go/db 排序锁仍是令牌锁，与 owner_epoch 是两种权限（不变） |
| 2 | 可恢复提交 | 意图（屏障字段）与索引同槽、同一段 Lua 写入；补投成员登记 + 段租约保证每段任何时刻至少一个（有存活副本时）扫描者，且扫描吞吐随副本数增长；停扫的副本释放所持段；回收只在两表 ACK 都等于 pending 时于同一脚本内发生 |
| 3 | 幂等与版本 | 版本仍是 41 字节定宽串按字节比较；C++ 路由层与 Go 客户端都不自动重放结果未知的命令，未执行的才重试；补投仍用原任务身份 |
| 4 | SQL 确认真实 | 两表 ACK 由 go/db 令牌发布脚本写入，与证据、屏障同槽同脚本；跨区放行与冷恢复的判定脚本不变；身份戳变化必走 SQL 核验 |
| 5 | 路由可信 | 逻辑归属（home_zone）、SQL 落点（placement，直读 TiDB）、玩家域落点（slot，由 player_id 唯一确定，**不需要查表**）、运行位置（location）四者分别定义；玩家域寻址不依赖任何目录，所以不存在「玩家域路由缺失 / 畸形 / 未就绪」这一类新错误 |
| 6 | 原子步骤安全 | 改形只改键名与少量键形校验，保留全部校验先于写入；跨域步骤拆分时每步自身原子且幂等（§5.10 成员集合） |
| 7 | 完整生命周期 | 加载、保存、离线、重连、跨区、扩缩容、合服、清理、恢复全部用同一个寻址函数 `PlayerSlot(pid)`；工具与它读写的键族同批改或 fail-closed（§13）；没有新旧键共存（未上线、直接以新键名起步） |
| 8 | 资源有界 | 路由层每 slot 排队、全进程排队、重试时间预算有上限；Go 每节点池有上限；补投每拍批量有上限；全部后台扫描按成员登记 + 段租约分片、速率有界 |

## 10. 容量模型与扩展边界

### 10.1 玩家域规模 [I]（2000 万 PCU 口径）

| 项 | 数值 | 来源 |
|---|---|---|
| 玩家域 ops | 核心约 119–127 万/s + go/db 每任务 7 次 × 8.9–22.2 万任务/s ≈ 62–155 万/s，合计约 180–280 万/s（多为 Lua） | G4a §16、盘点 go/db §5 |
| 在线热集 | 200–420 GB | G4a §16 |
| 常驻（不冷卸载） | 1–2 TB 起，30 天 2–6 TB | 同上 |
| 主节点数 | 按 ops（每主 5–10 万）约 18–56 个；按热集内存（每主 25 GB）约 8–17 个；按常驻内存约 40–240 个 → **约 50–250 个主** | 推算 |
| 节点数 | 每主 1 副本：100–500 个；每主 2 副本：150–750 个 | 主 ×(1 + 副本数) |
| 按 slot 索引 | 正常每 slot 在途约 30 个成员；go/db 停摆 1 小时约 1,200 个 | N_save × T_ack / 16384 |
| 每 slot 写入频率 | 补投索引约 3–7 次/s；结算索引更低 | 同上 |

### 10.2 控制域的上限

- 排队轮询（约 300 万往返/s）已由 G4a 会话按 zone 组拆到独立的 queue 实例（2026-10-23 第 1a 批），横向随 zone 组增加。
- **登录令牌 / 会话随在线人数增长，仍压在控制域单主上**，是控制域到达 2000 万口径前的主要瓶颈 [I]（G4a §24）。路线：第 11 批把登录域拆成自己的 Redis Cluster——令牌族按 `{acc}` tag，会话栅栏与设备集的同脚本问题（连接键与账号键同在 `commitLoginScript`）需要先改形，设计在第 11 批开工前补齐。在它落地前，系统级 Redis 上限由控制域单主决定。
- 映射域原有的「每条写任务两次就绪读、每次两次 INFO」热点随直读 TiDB 一并消失（§5.13）。

**第 11 批设计(2026-10-31,开工前补齐)**

目标:令牌、登录态、连接栅栏不再压在控制域单主上,加主节点就能扩。

- **部署**:登录域和玩家域同样用 `shared/playerredis` 的 `UniversalClient`,配置独立成 `LoginRedis`。K8s 缺省和玩家域共用一个 Redis Cluster(只有一种东西要运维,加主节点就扩);配置上能拆开,登录量大到要隔离时改地址即可。
- **账号 tag**:账号是用户输入,可能带花括号,不能直接当 hash tag。`AccountTag(account) = SlotTag(CRC16(account) mod 16384)`,放进 `shared/redisslot`。SlotTag 是纯字母,没有解析歧义,同一账号的键必然同槽。
- **键形**:

| 族 | 旧键形 | 新键形 |
|---|---|---|
| 连接栅栏 | `login:sessfence:<gate>:<inc>` | `login:sessfence:{<gate>:<inc>}` |
| 登录态 | `login_session:<gate>:<inc>` | `login_session:{<gate>:<inc>}` |
| 设备集 | `login_account_sessions:<account>` | `login_account_sessions:{<AccountTag>}:<account>` |
| 账号索引 | `account_refresh:<account>` | `account_refresh:{<AccountTag>}:<account>` |
| 令牌 | `access_token:<随机>`、`refresh_token:<随机>` | `access_token:{<AccountTag>}:<随机>`、`refresh_token:{<AccountTag>}:<随机>` |
| 令牌家族 | `refresh_family:<id>` | `refresh_family:{<AccountTag>}:<id>` |
| 密码配额 | `login:password:quota:v1:<指纹>` | `login:password:quota:v1:{<指纹>}` |

- **令牌字符串**:`<AccountTag>.<随机>`。令牌对客户端是不透明串,客户端不解析。校验时先取 tag 再拼键;格式不对按无效令牌。令牌的发放、轮换、作废脚本的全部键都在账号这个槽里,脚本结构不变。
- **登录提交拆两步**:`commitLoginScript` 现在同时碰连接栅栏、登录态和设备集,设备集按账号分布,不同槽。改成:
  1. 脚本只碰栅栏和登录态(同一个连接 tag):连接没关才写登录态。「检查通过、Disconnect 插进来、再写入」的窗口仍然没有。
  2. 脚本成功后再 `SADD` + `PEXPIRE` 设备集。
  两步之间的结局都能自愈:第 2 步失败,这次登录按 Redis 故障回失败,登录态随 TTL 过期;Disconnect 抢在第 2 步之前,设备集留一个陈旧成员,下一次 `PruneStaleFromDeviceSet` 发现登录态不存在就剔除。设备数上限本来就是软限制,不影响资产和写权。
- **`PruneStaleFromDeviceSet`** 跨连接读登录态,改 `MGetBySlot`。
- **删旧 gate 兼容**:`RefOf` 的 legacy 分支、`Ref.WriteKeys` 双写、`loginsession.Save` 两步写、`loginsession_legacy_key_total` 全部删除。没有会话代次的连接直接拒绝。
- **读者**:chat、friend、guild、match 经 `shared/loginsession` 读登录态,改成同一个键构造函数,客户端类型换 `UniversalClient`。
- **排队**:`queue:zone:*`、`admitted:zset:zone:*` 已按 zone 组拆实例,不动。
- **玩家键**:`player_locker`、`login_idempotent`、`login_generation` 已在第 3 批带 `{pid}`,属于玩家域,不属于登录域。

**第 11 批实现补充(2026-10-31,未编译)。**

- **句柄**:login 的 `ServiceContext` 三个句柄。`PlayerRedis` 管上面三族玩家键,`LoginRedis` 管设计表里的七族键,`RedisClient` 只剩控制域。两段配置都是 `playerredis.Config`,没写 `Addrs` 时指向 `Node.RedisClient` 那一个实例。口令配额的专用小池地址、口令、集群形态随 `LoginRedis`,连接数(每节点 4)和超时(250ms)是它自己的。
- **`playerredis.Config` 加了 `DB`**:只对单实例有意义,回落到现有实例时要落在它原来的库号上,否则 `Node.RedisClient.DB` 非 0 的部署,登录键会悄悄换库。`Cluster: true` 时写非 0 拒启。
- **`account_lock:*` 也进登录域**:设计表没列,实现时补上。它的压力随登录速率增长,留在控制域单主上仍是瓶颈。键形 `account_lock:login:{<AccountTag>}:<account>`、`account_lock:create:{<AccountTag>}:<account>`。
- **令牌里的 secret 与 head**:账号索引的成员和家族的 `head` 存 secret,不存完整令牌。脚本校验 `#head == 43`,并按「前缀 + secret」现拼 refresh 键,前缀带账号 tag。四段脚本一个字没改,现拼的键与声明的键同槽;`KEYS` 之外的键在 Cluster 上能访问的前提是同槽且在本节点,这里成立。
- **校验顺序**:先拆令牌字符串。tag 必须是规范的 SlotTag,secret 必须是 32 字节随机数的规范 base64url,否则按无效令牌,不访问 Redis。读到载荷后再核对 `AccountTag(账号) == tag`:账号索引键按账号算槽,家族键按 tag 算槽,两者不一致的载荷进不了同一段脚本。
- **`KeyOf` 拒绝之后**:Login、CreatePlayer、EnterGame 回 `kLoginSessionIdNotFound`,Login 的拒绝在认证之前,第三方 code 不被消费。Disconnect、LeaveGame 没有 tip 可回,记 ERROR 后直接返回:这种连接过不了 Login,没有状态要收尾。gate 每条连接都带实例号与代次(`client_session_handler.cpp`、`client_request_lane.cpp`),走到这里是接线缺陷。
- **其余脚本逐段核对**:`close`、`checkEnterGame`、`bindPlayer`、`leave` 都只碰栅栏一个键,不用改。Disconnect 清理本来就是 `Close` → `GET` → `SREM` → `DEL` 分开发的,跨槽不影响;`SREM` 之后 `DEL` 之前中断,登录态随 TTL 过期,连接已关闭,没人再读它。`mintLoginGeneration` 单键。
- **`LegacyGateLoginEnabled` 没删**:它是 gate 上口令 / 第三方首认证的开关(ARCH §12),和「没有会话代次的 gate」是两回事,gate 上的 `access_token` 绑定、弃用遥测的分类都还要它。`legacy_gate_killswitch_*_test.go` 留着,只删 `fenced=false` 的子用例。
- **读者**:除设计写的 chat、friend、guild、match 之外,mail、trade 也经 `shared/loginsession` 读登录态,一并换。六个服务共用 `playerredis.LoginReaderConfig`,各自只给超时和缺省连接池。etc 下的 yaml、本机脚本和 `k8s_deploy.ps1` 都写 `Addrs`、`PoolSizePerNode`,只有这一种形状。`KeyFromMember` 删除,`shared/loginsession` 只留 `ConnectionKey`,键形只在 `redisslot/login_keys.go`。
- **Java 网关**:`java/gateway_node` 不读这些键,令牌只经 gRPC 透传。令牌长度从 43 变成至多 48 字符,网关、gate、robot 都不解析、不限长。

**部署要改的键(第 10 批)**:`login` 的 `PlayerRedis.{Addrs,Cluster,Password|PasswordEnv,PoolSizePerNode}`、`LoginRedis.{同上}`;chat、friend、guild、match 的 `LoginRedis.{Addrs,Cluster,DB,Password|PasswordEnv,PoolSizePerNode}`(Cluster 下 `DB` 必须是 0)。mail 和 trade 也读登录态:mail 的 ConfigMap 原来写 `Host` 指向控制域,trade 的 `Rmb.LoginRedis` 原来没渲染,第 10 批都补了(未运行),否则上 Cluster 后这两个服务读不到登录态,客户端请求一律被拒。连接总账:login 每个 Pod 多两个池(单实例回落时各按 `Node.RedisClient.PoolSize`,etc 里写的是 20),口令配额池(每节点 4)改连登录域;mail 读登录态的池固定 8。

### 10.3 再往上：多集群联邦

Redis Cluster 的设计目标约 1000 个节点（集群总线 gossip，主与副本都算）。按 §10.1 每主 1 副本还有约 2–10 倍余量，每主 2 副本约 1.3–6.7 倍。**节点数达到约 600 时立项联邦**：`partition = hash64(pid) mod 2^k`，版本化的分区表（存 etcd）把分区映射到集群，客户端先选集群再按 slot 选节点。**键形不变**；跨集群搬分区需要自建「冻结分区 → 复制 → 翻转 → 清理」的迁移协议，届时单独设计。

## 11. 失败模式

| 场景 | 现象 | 处理与结果 |
|---|---|---|
| Redis 提交后 Scene 退出 | 意图在屏障里、成员在本 slot 索引里 | 补投器（段持有者）在 FirstReplayAfter 后按原任务身份补投 |
| Kafka 整体不可用 | 补投发布失败 | 不计次，失败 topic 熔断一个退避周期；长时间停扫则释放段 |
| 单个 topic 不可用 | 该 zone 的意图发布失败 | 只熔断该 topic，其他 zone 照常补投 |
| 只有一表 SQL 成功 | 一个 ACK 等于 pending、另一个落后 | 回收条件不满足，意图保留并按退避补投；冷恢复与跨区放行拒绝 |
| SQL 后、ACK 前 go/db 退出 | 令牌锁过期 | 重投后 SQL 行内围栏判 equal，发布脚本补写 ACK |
| 重复任务 / 补投与正常投递并发 | 同版本到达两次 | SQL 行内围栏与发布脚本按版本幂等 |
| 旧 owner 写入 | owner_epoch 不等 | 存盘脚本返回 0，C++ 走废黜路径 |
| 原子迁移交接 | 短暂停写，随后 MOVED | 路由层预算内重试，同 slot 停等保序 |
| 原子迁移中断 / 源主宕机 | 任务回滚，slot 仍归源（或其提升的副本，数据完整） | 核对 `CLUSTER MIGRATION STATUS` 后重试 |
| 主节点宕机 | 该节点 slot 短暂不可用，可能丢最后一段异步复制 | 副本提升；身份戳变化 → 冷加载走 SQL 核验；owner_epoch 风险见 §7 |
| 副本宕机或重启 | 缺省配置下主照常可写 | 副本恢复后自动同步；2 副本配置下保留一副本仍可写 |
| 补投副本全部退出 | 段租约过期 | 下一个启动的副本立即接管全部无主段 |
| 补投副本扩容 | 新副本登记成员 | 公平份额下降，老副本逐段释放，新副本接管 |
| 段租约脑裂（续约失败但仍在扫） | 两个副本扫同一段 | 认领脚本把分数推到未来，互斥由认领本身保证（今天多副本就是这样工作的 [C]） |
| 节点换地址 | Pod 重建 | 每 Pod 一个 ClusterIP Service，宣告地址不变；只有删除重建 Service 才需要重启 C++（与今天相同） |

## 12. 对现有设计的修订

| 契约 | 修订 |
|---|---|
| CZ-2（`cross-zone-scene-travel.md:35`）「`PlayerAllData` 所在 Redis 物理共享，不支持 Cluster」 | 改为「玩家态在一个逻辑空间、物理按 `{player_id}` 分片」；第 2b 批（§13）落码后生效 |
| CZ-4「标记与 epoch 同一原子域、键无 hash tag」 | 同一原子域保留，改为同 `{pid}` slot |
| D12（`cross-zone-matchmaking.md:103`）SharedRedis 全服单一实例 | 契约键（location、session、battle:lock）移入玩家域，仍是全服一个逻辑空间 |
| G4a §20「单元化 + 单元内 Cluster」、§25.2 第 20 条 | 不采用单元化；跨区自由与位置透明保持。§25.2 第 3、13、15、22 条由本稿答复（按 slot 分桶；首次上线前直接改名；C++ 自研路由层；`player:{<id>}:*` 键名与 data_service 保留后缀表）。G4a 会话 2026-10-23 已在其 §25.3 记录 |
| 存盘围栏「父快照屏障部署前提是同一 Redis 实例」 | 改为「同一 `{pid}` slot」；§18.7 的二选一按 B（同 slot）落地 |
| saveoutbox 同实例门禁、`BucketCount=64` | 改为按 slot 索引与成员登记 + 段租约；门禁改为核对玩家域句柄 |
| B0-1 §11 | 按原设计落地（§5.9） |
| 各处「共享库不许集群化」的注释 | 随对应批次改为「玩家域在 PlayerRedis（Cluster），控制域仍单实例」 |
| cross_server 原则「同一连接 FIFO」 | 收窄为「同一 slot 内 FIFO」，由路由层停等保证 |
| 「真 Redis 验证 7.2 与 8.x 两版都跑」 | 玩家域只跑 ≥ 8.4（取 8.10.x），原子迁移需要；控制域仍两版 |

**上线后的键形变更**：本稿的改名只在首次上线前直接进行。上线后若再改任何玩家域键形，必须停写切换（全部写者排空后一次切换），禁止双读双写（围栏原语在新旧键名上会各自「成功」，[G4a §20.7](shared-redis-split-by-purpose.md)）。

## 13. 分批落码计划

每批不超过 30 个文件（含测试），批间做语义评审；全部构建、测试、压测由 Codex 执行（AGENTS.md §10.1）。**同一键族的 C++ / Go 镜像、以及读写该键族的工具（merge_zone、barrierrestore、debug_*），在同一批内改完；来不及改的工具在该批改为拒绝运行（fail-closed），不得留到后面批次带着旧键名静默空过。**

| 批 | 内容 | 状态 |
|---|---|---|
| 1 | 基础契约：Go `shared/redisslot`（CRC16、HashSlot、PlayerSlot、SlotTag、保留后缀表）与 `shared/playerredis`（UniversalClient 构造器、每节点池上限、同槽 / 带 tag 守卫、MGetBySlot、ScanEachMaster）；C++ `redis_slot.h`；两侧金样测试；本设计文档 | 已落码，未编译；评审后的修补见本批补丁 |
| 2a | owner_epoch 改名：C++ `player_ownership_comp.h` 与 `role_admission_system.cpp` 的内联拼键、存盘 Lua 的 owner 键形校验；Go `shared/ownerepoch` 与 merge_zone `save_barrier_verify` 镜像、barrierrestore；data_service 改查保留后缀表；相关测试 | 已落码，未编译（2026-10-24；另补 merge_zone blob 拷贝跳过保留后缀，见 PROGRESS） |
| 2b | 父快照改名：C++ `redis_client.h` 父键构造与 Lua 父键校验；login dataloader、外观补齐（同批改 `MGetBySlot`）；data_service 账本与 debug 工具；friend、match 读者；merge_zone `player_rows`；barrierrestore；相关测试 | 已落码，未编译（2026-10-24；共享构造 `savebarrier.ParentKey`，见 PROGRESS）。保留（2026-10-29：用户确认未上线、玩家数据可全部清空；纯改名不改语义，父快照随 TiDB 决策 §9 新存盘路径移出 Redis 时这些调用点一并删除）。Codex 10-24 全量 C++ 构建与各 Go 模块单测已覆盖这些文件 |
| 2c | 补投索引按 slot：C++ 索引键与 Lua 索引校验；saveoutbox 契约、认领、回收改按 slot 索引（本批暂保留「只认领 topic 属于本 zone 的意图」，沿用单 topic 发布器）；merge_zone P4 排空检查、barrierrestore 恢复脚本改形、debug_import 脚本改形；相关测试 | 作废（2026-10-29）：补投索引与补投器只服务于 Redis 里的父快照与 outbox 载荷；TiDB 决策 §9 把它们移出 Redis 后（§9.4 两种存盘路径都不再在 Redis 留意图）失去用途，不为将删除的组件做分片。未开工、无代码改动 |
| 2d | 补投器分片：成员登记 + 段租约、多 topic 发布器与按 topic 熔断、逐意图闸、去掉映射读与跨 zone 迁移脚本、续约前提；相关测试 | 作废（2026-10-29）：同 2c。未开工、无代码改动 |
| 3 | 协调键改名：handoff、location、修订、离场、去重、冷却、登录代次、同步摘要、玩家锁、登录幂等、会话、AFK、战斗锁与上下文；C++ 与 Go 镜像、脚本、告警、手册；scene_manager 离场拆分与成员集合计数；跨玩家 MGET 改按 slot 分组；多键脚本同槽契约测试 | 已落码，未编译（2026-10-31，定稿见 §5.10）。player_locator 只改了会话键形，跨槽结构留给第 7 批。合服提示无生产读写，表里标删除 |
| 4 | go/db 玩家域句柄：排序锁 / 游标 / 缓存键改名，句柄拆分，expand 状态缓存 | 作废（2026-10-30）：存盘直接提交 TiDB，go/db 不再连 Redis |
| 5a | C++ 两份 `Hiredis` 加 `formattedCommand`；`RedisClusterClient`（停等、错误归类、拓扑、资源上限）与单测；暂无调用方 | 已落码，未编译（2026-10-31，实现补充见 §5.6 末段） |
| 5b | scene 接线：`tlsPlayerRedis`、`RedisSystem`、`MessageAsyncClient` 改接口 | 已落码，未编译（2026-10-31，实现补充见 §5.6 末段）。`MessageAsyncClient` 已随存档移出 Redis 删除，不用改 |
| 5c | 其余 C++ 调用点换句柄（player_lifecycle、player_battle、battle_room_manager、player_team 拆 MGET 等） | 已落码，未编译（2026-10-31，FIFO 依赖逐处结论见 §5.6 末段）。battle_room_manager 已不访问 Redis；MGET 第 3 批已拆 |
| 6 | 各 Go 服务玩家域句柄：scene_manager、player_inbox、friend（含 `ScanPage`）、guild、match、mail、trade；包改名 `shared/playerredis`；scene_manager 分句柄与成员集合对账 | 已落码，未编译（2026-10-31，分工见 §5.8，实现补充见 §5.7，对账见 §5.10）。data_service、chat 不读玩家域键，不用改；login 不在这批 |
| 7 | player_locator 按 slot 分桶（§5.9） | 已落码，未编译（2026-10-31，实现补充见 §5.9 末段） |
| 8 | 战斗结算按 slot（§5.11） | 作废（2026-10-30）：结算只走 match SQL 加收件箱，`battle:settlement:pending` 已删除（总体方案 §5.4） |
| 9 | 工具收尾：前面批次标为 fail-closed 的工具恢复可用，data_consistency_check 等 | 已落码，未运行（2026-10-31，实现补充见本节末） |
| 10 | 部署：K8s 玩家域 Cluster 清单（≥ 8.4，每 Pod 一个 Service 宣告 IP）与初始化 Job、原子迁移扩缩容手册、各服务配置渲染、连接总账；compose 开发集群（8.10.x）；容量基准工具。G4a 会话 2026-10-23 已落的部署能力要接上：玩家域主节点加入 `k8s_deploy.ps1` 的 `redis-wipe` 遍历；玩家域告警另起 PrometheusRule 组，不并进 `deploy/k8s/redis-alerts.yaml` 的 `redis.control-domain` | 已落码，未运行（2026-10-31，实现补充见本节末；手册 [redis-player-cluster-runbook.md](../ops/redis-player-cluster-runbook.md)） |
| 11 | 登录域（令牌 / 会话 / 栅栏）横向扩展（§10.2），顺带 go/login 玩家域句柄 | 已落码，未编译（2026-10-31，实现补充见 §10.2 末段） |

映射域不在本稿批次内（§5.13）。

**第 10 批实现补充(2026-10-31,未运行)。**

- **参数**:`k8s_deploy.ps1` 的 `-PlayerRedisMasters`(dev 缺省 0 不建集群,staging、prod 必须显式、至少 3)、`-PlayerRedisReplicasPerMaster`、`-PlayerRedisMaxMemory`、`-PlayerRedisStorage`,口令 `MMORPG_PLAYER_REDIS_PASSWORD`。都不粘滞。登录域和玩家域共用这一个集群;Go 配置上能拆,部署脚本目前只渲染共用形态。
- **清单**:`redis-player-cluster.yaml`。镜像摘要取自 Java 版 `deploy/compose/infra.yaml` 的 `redis:8.10.2`。Docker Hub 同一个标签 10-22 重建过,摘要不同:钉摘要就是防这个。
- **`updateStrategy: OnDelete`**:改模板不自动重启节点。`RollingUpdate` 不知道谁是主,一次 apply 可能连着打掉一对主从。代价是 `rollout status` 不可用,就绪看建群 Job。
- **建群 Job**:全部节点空白才 `--cluster create`,否则只核对。一半空白一半有状态可能是扩容新起的节点,也可能是 PVC 丢了,脚本分不清,所以不碰。Job 从不跑 `--cluster fix`。
- **总账**:部署脚本按池上限记,不按 §6.1 的活跃连接估算,偏保守。登录域的池算进同一行。不建集群时这些池都记在控制域那一份上。
- **口令**:集群模式下 `PlayerRedis`、`LoginRedis` 的 `PasswordEnv` 都指向容器环境变量 `PLAYER_REDIS_PASSWORD`,来自 Secret `redis-player-auth`。scene 的 Pod 模板带种子加口令的指纹,变了自动滚动;Go 服务没有指纹,口令轮换要手工重启,写在手册里。
- **trade 的 `Rmb.LoginRedis`**:ConfigMap 各档位都写。这一段以前完全没有渲染,`AssetOp.Enabled=true` 时 `config.Validate` 直接拒启,与集群无关。资产通道关着时不读也不连,总账按打开计。
- **`deploy/k8s/manifests/go-svc/player-inbox.yaml` 补 `MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE`**:`NewConsumerVerifier` 缺它拒启,K8s 上 player-inbox 原本起不来。与集群无关,验证部署时会卡在这里,所以一起补。
- **docker 调试栈**:加 `player-inbox` 常驻容器、`player-inbox-sql-boot`、`player-inbox-migrate`。`prepare_docker_debug.py` 把调试栈的 `player_inbox.consumer_enabled` 打开:只入箱不消费,结算到不了玩家。源配置的 `false` 是线上分步打开用的,不变。

**第 9 批实现补充(2026-10-31,未运行)。**

- **`go/db/internal/maintenancephase` 没有换成 `ScanEachMaster`**:这个包不自己扫 Redis,只核对 `maintenance_drain.ps1` 采集的证据。改法是计划里每个主节点列一个目标,collector 记录取样首尾的拓扑指纹,`verifyAuthorityRedisClusterViews` 核对目标集合正好等于持有 slot 的主节点、取样期间拓扑没变。保证相同,任何一条不成立都按未闭合拒绝。nonce 键尾带 hash tag,落在各主节点自己持有的 slot 上。
- **`tools/data_consistency_check`** 只读 `guild_rank:zone:*`,不碰玩家域,不用改。
- **清库**:`tools/scripts/lib/redis_local_flush.ps1` 统一本机清库(单实例加开发集群的每个主节点),`dev_tools.ps1 -Command redis-wipe-local`、`currency_crash_window.ps1`、`stress_round19.ps1` 共用。`deploy-staging.sh`、`stress-linux-tier.sh` 用 `REDIS_PLAYER_NODES` 列节点,逐主节点清。
- **压测**:`stress_snap.ps1` 的 target 加 `k8s-cluster:<ns>/<sts>`、`docker-cluster:<container>:<port>`,每个时刻现列主节点。`stress_summarize.ps1` 的 R1、R2 逐主节点出行,再合成 `<name>-all`。
- **`k8s_zone_rollback.ps1`**:部署事实 Schema 5 加 `PlayerRedis`,线上是集群时缺种子或主节点数在停服之前拒绝。回档工具 `zone_rollback_discard` 本身还没有落地,脚本写了它要接的参数契约。

## 14. 验证与验收（交 Codex 执行）

### 14.1 单元与契约

- Go：`cd go/shared && go test ./redisslot/... ./playerredis/...`；C++：`rpc_controller_test` 中的 `RedisSlotTest.*`。两侧金样（§5.2）逐字一致；SlotTag 全表 SHA-256 与 FNV-1a 64 一致。
- 每批对涉及的多键脚本做「全部 KEYS 同 slot」契约断言（按 §5.2 的 HashSlot 计算）。

### 14.2 真集群集成（玩家域 Redis 8.10.x）

> 2026-10-31：存盘闭包、补投、go/db 发布 / 游标和第 7 项的用例，随存盘直接提交 TiDB 作废；第 6 项的「切主丢尾后冷加载」改为核对玩家存档不受 Redis 切主影响。其余照跑。

- compose 起 3 主 3 从，跑存盘闭包、补投、go/db 发布 / 游标、login 冷加载、scene_manager 落点 / 离场、locator 会话的集成用例；断言无 CROSSSLOT、无守卫拒绝。
- 迁移与故障，每项断言：意图不丢（最终被回收或停在可解释的终态）、版本不回退、ACK 单调、没有非重定向类错误：
  1. 用例运行中原子迁移 slot；
  2. 迁移中 kill 源主；
  3. kill 一个副本（缺省配置下主应持续可写）；
  4. kill 主且旧主不恢复；
  5. 迁移中同一玩家「先写 A 键、后读 B 键」（停等保序）；
  6. 切主丢尾后冷加载读到 SQL 新版本、迁移后冷加载走核验路径（身份戳）；
  7. 单个 topic 不可用时其他 zone 的意图按时补投；补投副本 1→N 扩容后段分布收敛、认领吞吐增长。

### 14.3 容量实证（不以副本或代理数量作为证据）

1. **环境**：每个主（连同其副本）独占主机或独占 CPU 核（K8s 反亲和），压测端容量远大于集群容量；记录主机 CPU、网卡、Redis 主线程 CPU。
2. **基线**：3 主，基准工具按真实脚本组合（存盘屏障 Lua + 认领 + 发布 + 游标）施加过量负载；必须证明主线程已饱和（ops 平台期、主线程 CPU≈100%），同时客户端池等待（go-redis PoolStats 的 WaitCount / Timeouts）≈0。
3. **扩容中**：同负载下加 3 主并原子迁移一半 slot，记录单 slot 迁移时长、交接停写时长、源节点 fork 内存、重定向计数、错误率。
4. **扩容后**：每主 slot 数、键数、ops、内存分布，与基线二维对比；预期吞吐接近 2 倍、每主内存近半。
5. **端到端**：按 AGENTS.md §6 的压测纪律，3 主对 6 主施加同一份过量负载，用 `stress_summarize.ps1` 出对比表。
6. **连接规模**：模拟连接数达到目标量级，测每条空闲连接的内存、kill 主之后的回连时长与错误率，核对 §6.1 公式。

未拿到上述实测前，本稿 §10 的数字只作规划，不作结论。

## 15. Java 版（AGENTS.md §12）

Java 版玩家存盘是单个 SQL 事务（`player` / `player_state` 两表），Redis 只承载目录、锁、信号与缓存，没有本稿的父快照屏障、补投索引、go/db 发布链。本稿各批只改服务端内部，不改客户端契约，**Java 没有对应待做项**；Java 自身 Redisson 单实例的集群化已在其 PARITY.md 第 274 行登记，本稿不新增行。会影响客户端的两项（friend 在线目录游标格式、不改成转区）按既有两版同批评审处理：前者第 6 批已落码，游标对客户端是不透明串，客户端不解析，Java 的游标是自己的实现，都不用改；后者本稿已确定不改。

## 16. 风险与未决

1. **owner_epoch 铸造的异步复制丢失**（§7）：现存风险，建议独立立项在铸造后同节点 `WAITAOF`。2026-10-24 已落码（未编译，[回退防护 §4.3.3](zone-redis-regression-guard-20261022.md)）；剩余未决是 R-b 压测下的进场延迟与吞吐上限（组提交是升级路线）。第 6 批的 Cluster 适配已落码，未编译，真集群上的切主演练按 §14.2。
2. **离场人数改成员集合**（§5.10）：第 3 批已定稿，崩溃留下的成员由第 6 批的后台对账摘除。剩余：对账的预算是推算值，规模上来后要看走完一遍的时长。
3. **Go 服务按 zone 部署的进程数**：物理 zone 增长时它主导连接数（§6.2），建议 Go 服务改为一进程多 zone，单独立项。
4. **DBTask 按 home_zone 分 topic**：逻辑 zone 达到 10 万时 Kafka topic 数不可行，与 Redis 无关，单独立项。
5. **登录令牌 / 会话仍在控制域单主**（§10.2）：第 11 批之前它决定系统级上限。第 11 批代码已切到 `LoginRedis`（2026-10-31，未编译），第 10 批部署脚本把它指向 Cluster（2026-10-31，未运行），真集群验证之后这条才解除。
6. **迁移后的 SQL 核验读放大**（§7、§8.4）：扩容须分批、低峰执行。
7. **data_service 字段脚本**在迁移交接瞬间可能被短暂阻塞（无生产写者，§5.3）。

## 修订记录

- **v1（2026-10-23）**：初稿。
- **v2（2026-10-23）**：吸收四视角评审（逐条核对源码后采纳）：
  - C++ 路由层由「流水线 + 重定向后串行」改为**同 slot 停等**——旧做法在 ASK / TRYAGAIN 下，排在被重定向命令之后、已发出且键在源端的命令会先执行（`player_battle.cpp:808-813` 删锁与跟随读锁即受影响）；耗尽时同 slot 排队命令一并空应答；未执行类服务端错误（NOREPLICAS / LOADING / OOM 等）纳入同一归类；预算改按时间。
  - 玩家域钉 **Redis ≥ 8.4，用原子 slot 迁移**——逐键迁移下声明了缺失键的多键脚本整个窗口持续 TRYAGAIN、`MIGRATE` 阻塞两端、迁移中源主故障会产生同键两份（`third_party/redis/src/cluster.c`、`cluster_asm.c`）；§8 重写，逐键迁移降为应急并写明危险。
  - **副本与写入门槛**：每主 1 副本时 `min-replicas-to-write 1` 会让任一副本故障或故障转移后整片持续拒写；改为缺省 `min-replicas-to-write 0`，可选每主 2 副本。
  - **§7 更正**：ACK 与 pending 同在一个 hash，丢尾只会整体回退，防线是实例身份戳 + SQL 核验；迁移同样触发核验，计入读放大。
  - **补投器**：成员单独登记、按成员数算公平份额（旧做法新副本永远拿不到段）；按 topic 熔断；停扫释放段。
  - **离场拆分**：墓碑 + 去重键方案丢了「谁删谁减」，改为成员集合计数（方向，第 3 批定稿）。
  - **端点**：每 Pod 一个 ClusterIP Service 宣告 IP（Pod 按 IP 宣告会在重建时换地址）。
  - **Go 客户端**：不自动重放结果未知的命令（`OnNewNode` 钩子）、`ContextTimeoutEnabled` 缺省开；ScanEachMaster 的拓扑指纹加入逐节点 run_id 与迁移状态；在线目录用分页 `ScanPage`。
  - **批次**：第 2 批实际约 55–60 个文件，拆为 2a–2d；第 5 批拆为 5a–5c；工具随键族同批或 fail-closed。
  - **清单补漏**：屏障恢复脚本与 debug_import 脚本的改形、`player:save_barrier:maintenance:<zone>` 与 `player_to_account:<id>` 归控制域、login 外观补齐的 MGET。
  - **边界口径**：1000 节点含副本，余量重算；控制域里登录令牌 / 会话随在线增长，系统级上限在第 11 批前由控制域单主决定。
- **第 3 批（2026-10-31）**：§5.2 表按实际情况标注已删除的键族，§5.10 定稿。
- **第 11 批（2026-10-31）**：§10.2 补实现细节与部署要改的键；§13 第 11 行更新状态。
- **第 7 批（2026-10-31）**：§5.2、§5.3 去掉「未改」标注，加分片键；§5.9 补实现细节。
- **第 6 批（2026-10-31）**：包改名 `shared/playerredis`；§5.7 补实现细节（配置、`ScanPage` 指纹取舍、铸造通道）；§5.8 补各服务分工；§5.10 补分句柄与成员集合对账。
- **第 5b、5c 批（2026-10-31）**：§5.6 补实现细节与 FIFO 依赖逐处结论；§6.1 的 `N_cpp` 改为只算 scene。
- **第 9、10 批（2026-10-31）**：§13 第 9、10 行更新状态并补实现细节；§10.2 与 §16 第 5 条更新部署状态。
