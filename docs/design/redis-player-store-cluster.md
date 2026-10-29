# 玩家态 Redis 集群化：物理水平扩展设计

> 状态：**设计 v2（2026-10-23）**。v1 经四视角对抗评审（一致性、扩展与运维、可实现性、替代方案），评审结论逐条核对源码后吸收进本版，变更见文末「修订记录」。第 1 批已落码（未编译），其余批次见 §13。所有容量数字除注明 [M] 者外都是推算 [I]，未经本仓实测；结论以 §14 的实测为准。
>
> 需求来源：[Redis 物理水平扩展交接](../handoff/redis-horizontal-scaling-requirements-20261022.md)（用户原话「我就想 redis 可以物理水平无限扩展」，方案由本稿自定）。源码事实来自 2026-10-22 全链只读盘点（10 路，按子系统 + 按模式两种方式扫描），行号以当时工作区为准，落码时按函数名复核。
>
> 本稿是本次方案的**唯一权威**。[共享 Redis 按用途拆分 §20–§25](shared-redis-split-by-purpose.md) 的「单元化 + 单元内 Cluster」路线作为输入材料，不再作为实施路线（G4a 会话 2026-10-23 已在该稿 §25.3 把相关条目指向本稿）。
>
> 证据等级：[C] 读代码或配置确认；[D] 文档记载；[I] 推算；[M] 实测。

> **2026-10-24 方向变更（先读；下文原文保留）**：用户 10-24 明确「玩家数据不要 redis 存储，其他改用的地方还是要用」，记录与键族分类见 [TiDB 决策 §9](global-data-layer-tidb-decision.md)。对本稿的影响：
> - 父快照 `PlayerAllData`、子表缓存、资产 SQL 证据、战斗结算原件属于玩家数据，不再进 Redis Cluster；补投索引与补投器随父快照移出失去现有用途（新存盘路径如果保留异步投递，另行设计）。
> - 第 2b 批（父快照改名）在看到本变更之前已落码（未编译），保留还是回退待用户定；2c、2d 已暂停（§13）；第 4 批的子表缓存与排序锁 / 游标改名、第 8 批的结算索引，等新设计定稿后再定。下文 §1 第 3、4 条、§5.3 存盘脚本改形、§5.4、§5.5 中的子表缓存与资产证据、§10.1 的容量推算待重写。
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
| **控制域 control** | 场景路由、登录会话与令牌、选主锁、重试队列、task:result、协议世代、战斗活动结果、队伍、各服务私有键、工具互斥键 | 现有共享实例（含 redis-ha），名字与地址不变；排队族已由 G4a 第 1a 批拆到按 zone 组的 queue 实例 | 不分片（上限与路线见 §10.2） |
| **映射域 mapping** | `player:zone/placement/maprev`、合服围栏镜像、就绪标记 | 2026-10-23 起改为直读 TiDB，移出 Redis（§5.13） | 不适用 |

**接缝规则（可机械检查）**：玩家域客户端只接受带 hash tag 的键，非玩家域的键一律不带 `{<数字>}`。C++ 路由层与 Go 玩家域客户端的守卫在发送前检查（§5.6、§5.7），本机单实例开发时同样生效。

### 5.2 键形契约

**规则**：把键里的玩家 id 用花括号包起来，其余不动（`X:<pid>:Y` → `X:{<pid>}:Y`）。花括号里是 player_id 的十进制，无前导零。玩家域所有键都带 tag，单键族也不例外——这是 §5.1 接缝检查的前提。

| 族 | 旧键形 | 新键形 | 主要写者 / 读者 |
|---|---|---|---|
| 父快照 | `PlayerAllData:<pid>` | `PlayerAllData:{<pid>}` | C++ 存盘；login 冷加载、种子、身份补齐、外观补齐；data_service 账本；friend、match 只读 |
| 归属代次 | `player:<pid>:owner_epoch` | `player:{<pid>}:owner_epoch` | scene_manager 铸造；C++、login、go/db、player_inbox |
| 交接标记 | `player:<pid>:handoff` | `player:{<pid>}:handoff` | C++；scene_manager |
| 位置 | `player:<pid>:location` | `player:{<pid>}:location` | scene_manager；C++、locator、login、guild、trade、mail、match、player_inbox |
| 位置修订 / 下限 | `player:<pid>:location_revision(_floor)` | `player:{<pid>}:location_revision(_floor)` | scene_manager |
| 离场等待 | `player:<pid>:leave_release_wait` | `player:{<pid>}:leave_release_wait` | scene_manager |
| 换线冷却 | `player:<pid>:channel_switch_cooldown` | `player:{<pid>}:channel_switch_cooldown` | scene_manager |
| 进场去重 | `enter_scene:dedup:<pid>:<req>` | `enter_scene:dedup:{<pid>}:<req>` | scene_manager |
| 离场墓碑（新增，见 §5.10） | — | `player:{<pid>}:leave_tomb` | scene_manager |
| 登录代次 | `player:<pid>:login_generation` | `player:{<pid>}:login_generation` | login |
| 同步摘要 | `player:<pid>:sync_digest` | `player:{<pid>}:sync_digest` | friend / guild / match 写；C++ 读 |
| 存盘屏障 | `player:{<pid>}:save_barrier` | 不变 | C++、go/db、login |
| 资产 SQL 证据 | `player:{<pid>}:asset_sql_commit` | 不变 | go/db；data_service |
| 补投索引 | `player:save_pending:<home_zone>:<pid%64>` | `player:save_pending:{<slottag>}` | C++ 写；go/db 认领 / 回收 |
| 补投段租约（新增） | — | `player:save_outbox_lease:{<slottag>}` | go/db |
| 补投成员登记（新增） | — | `player:save_outbox_members:{afps}`（slot 0 的 tag，单键） | go/db |
| 会话 | `player:session:<pid>` | `player:session:{<pid>}` | player_locator；friend、guild、mail、match、login |
| 旧位置键 | `player:location:<pid>` | **删除**（生产不再写，未上线无存量） | — |
| 会话租约（4 键） | `player:leases`、`:processing`、`:claim_tokens`、`:claim_payloads` | `player:leases:{<slottag>}` 及同 tag 的三个兄弟键 | player_locator |
| gate 索引 | `player:gate_index:sessions:<uuid>`、`:gates` | `player:gate_index:{<slottag>}:sessions:<uuid>`、`player:gate_index:{<slottag>}:gates` | player_locator（B0-1 §11 的既定形状） |
| AFK 通行 | `player:afk_pass:<pid>` | `player:afk_pass:{<pid>}` | player_locator |
| 玩家锁 | `player_locker:<pid>` | `player_locker:{<pid>}` | login |
| 合服提示 | `player_merge_notice:<pid>`、`player_force_rename:<pid>` | `…:{<pid>}` | merge_zone 写；login 读删 |
| 登录幂等 | `login_idempotent:<pid>:<req>` | `login_idempotent:{<pid>}:<req>` | login |
| 战斗锁 / 上下文 | `battle:lock:<pid>`、`battle:ctx:<pid>` | `battle:lock:{<pid>}`、`battle:ctx:{<pid>}` | C++；guild、match 只读 |
| 待结算 | `battle:settlement:pending:<pid>`、`…:pending:id:<pid>` | `battle:settlement:pending:{<pid>}`、`battle:settlement:pending:id:{<pid>}` | battle、scene |
| 结算索引 | `battle:settlement:pending:index`（全局） | `battle:settlement:pending:index:{<slottag>}` | battle、scene（§5.11） |
| 已暂存标记 | `battle:settlement:staged:<battle_id>`（每局一个 SET） | `battle:settlement:staged:{<pid>}:<battle_id>`（每人每局一个串，EX 86400） | battle |
| go/db 排序锁 | `distributed:lock:kafka:ordering:<topic>:<pid>:<msg>` | `…:<topic>:{<pid>}:<msg>` | go/db；merge_zone 镜像 |
| go/db 游标 / epoch | `consumer:applied(_epoch):<topic>:<pid>:<msg>` | `consumer:applied(_epoch):<topic>:{<pid>}:<msg>` | go/db |
| 子表缓存 | `<msgType>:<pid>` | `<msgType>:{<pid>}` | go/db |
| expand 消费锁 | `kafka:consumer:lock:<pid>` | `kafka:consumer:lock:{<pid>}` | go/db |
| data_service 字段 | `player:{<id>}:<field>`、`__lock/__version/__fields` | 不变 | data_service |

控制域里与玩家相关、但**不进**玩家域的键（不带玩家 tag）：`player:save_barrier:maintenance:<zone>`（屏障恢复工具的 zone 互斥键）、`player_to_account:<id>`（login 账号反查）、`assetop:wake:<svc>:<pid>`（guild / trade 的唤醒去重）。它们不与玩家域键出现在同一条命令里（§5.3 对屏障恢复与 debug_import 的改形保证这一点）。

**data_service 命名空间**：`player:{<id>}:<字段>` 是 data_service 的字段命名空间，新进入该前缀的后缀（owner_epoch、handoff、location、location_revision、location_revision_floor、leave_release_wait、channel_switch_cooldown、leave_tomb、login_generation、sync_digest，加上已有的 save_barrier、asset_sql_commit）统一由 Go 共享包 `shared/redisslot` 的保留后缀表登记（第 1 批已落），`isReservedPlayerField` 改为查这张表，不再逐个比键名。今后任何模块新增 `player:{<id>}:` 键，必须登记到这张表（契约测试枚举核对）。

**slot 与 slot-tag**（两种语言逐字一致，金样向量钉住；第 1 批已落 `go/shared/redisslot` 与 `cpp/libs/engine/infra/storage/redis_cluster/redis_slot.h`）：

- `HashSlot(key)`：Redis Cluster 规范——键里第一个 `{` 之后第一个 `}` 之前若非空，只对这段算；CRC16-XMODEM（多项式 0x1021，初值 0）取低 14 位。金样：`HashSlot("123456789") = 12739`（CRC16 = 0x31C3，规范原文例子）；`{user1000}.following → 3443`、`foo{}{bar} → 8363`、`foo{{bar}}zap → 4015`、`foo{bar}{zap} → 5061`、`PlayerAllData:{42} → 8000`、`player:{42}:owner_epoch → 8000`、`18446744073709551615 → 5255`。
- `PlayerSlot(pid) = HashSlot(<pid 十进制>)`；`PlayerTag(pid) = "{" + <pid 十进制> + "}"`。
- `SlotTag(s)`：在字母表 `[a-z]` 上按「先短后长、同长按字典序」逐个枚举字符串 t，第一个满足 `CRC16(t) mod 16384 = s` 的 t 就是 slot s 的 tag。长度 4 以内即可覆盖全部 16384 个 slot（最后填上的是 `cgoq`）。纯字母的 tag 不会被误解析成玩家 id。金样：`SlotTag(0)=afps`、`SlotTag(1)=emj`、`SlotTag(2)=ahwf`、`SlotTag(100)=aotc`、`SlotTag(12739)=aarc`、`SlotTag(16383)=hia`；整张表按 slot 顺序以 `\n` 连接（72467 字节）后 SHA-256 = `b564523fd7c87f294eab3efd4b09e171775388ea14790138c1d4bb988df61d4c`，FNV-1a 64 = `0xf4a7bdc8d20a28e5`。两种语言都在首次使用时生成这张表，不检入生成物。
- 某玩家的按 slot 索引键 = `<族前缀>:{SlotTag(PlayerSlot(pid))}…`，与该玩家的 `{pid}` 键必然同槽。

### 5.3 原子范围与脚本改形

| 脚本 | 改形 | 同槽证明 |
|---|---|---|
| C++ `kSaveWithCommitBarrierLuaScript` | KEYS 换成 `PlayerAllData:{pid}`、`player:{pid}:owner_epoch`、`player:{pid}:save_barrier`、`player:save_pending:{<slottag>}`；键形校验同步更新（父键与 owner 键按新形状逐字比对，索引键按 `^player:save_pending:{[a-z]+}$` 校验）；topic 仍按 `db_task_zone_<z>[_g<n>]` 校验，但不再要求与索引里的 zone 相等（索引已不含 zone）；ZADD 成员与分数不变 | 前三键同 `{pid}`；索引 tag 按构造与 pid 同槽，路由层发送前再核一次 |
| C++ 归属 / 交接 / 改派 / 取证 / SQL 回执 / 战斗 lock-ctx 共 15 段 | 只改调用方传入的键名 | 全部同 `{pid}` |
| C++ 战斗结算 6 段 | 全局索引换成按 slot 索引；stage 脚本把每局的 SET 换成每人每局的串标记（`SET NX EX 86400` 语义） | 同 `{pid}` + 本 slot 索引 |
| C++ `player_team` 的 MGET | 拆成两条 GET：队长位置、自己的战斗锁 | 原 FIFO 依赖是「本人删锁先于本人读锁」，两条命令都在本人 slot，路由层停等保证执行顺序（§5.6） |
| go/db 令牌发布 / 游标 / 释放与续租 | 锁、缓存、游标、epoch 键带 `{pid}`；`RunTokenScript` 把锁键加进 KEYS 的方式不变 | 全部同 `{pid}` |
| go/db saveoutbox 认领 / 推迟 / 计次 / 终态 / 提示 | 桶换成按 slot 索引；**删除跨 zone 迁移脚本**（索引已与 zone 无关） | 屏障 `{pid}` + 本 slot 索引 |
| go/db 屏障恢复 `restoreScript` / `mappingInspectScript` | 维护键（控制域）移出脚本，改为脚本外前后各核一次（它只是工具互斥，业务写者不读它，`barrierrestore/scripts.go` 头注释已说明）；「本 zone 64 桶为空」改为「该玩家不在其 slot 索引中」（同槽 ZSCORE）；映射三键随映射域移出 | 余下键全部同 `{pid}` + 本 slot 索引 |
| data_service `debug_import` 的 stage / commit 脚本 | stage 去掉按 zone 的桶、改用该玩家的 slot 索引；commit 去掉控制域的 `player_to_account:<id>`（脚本没用到它） | 同 `{pid}` |
| login begin / publish / 种子 / 身份补齐 | 只改键名（身份补齐的四键：父快照、玩家锁、会话、位置都属玩家域） | 全部同 `{pid}` |
| login 外观补齐的跨玩家 MGET | 改 `MGetBySlot` | 按 slot 分组 |
| scene_manager 落点 / 回滚 / 快照 / 进场去重 | 只改键名 | 全部同 `{pid}` |
| scene_manager 离场 | 拆成玩家域与控制域两步，人数改为按成员集合计（§5.10） | 第一步同 `{pid}`；第二步只碰控制域 |
| player_locator 9 段会话 Lua 与 3 段 gate 索引脚本 | 租约与 gate 索引按 slot 分桶；认领脚本不再现拼其他玩家的会话键；`""` 哨兵挪进 ARGV（§5.9） | 会话 `{pid}` + 本 slot 的租约与索引 |
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

- 句柄拆成两个：**PlayerStore**（玩家域，`playerstore` 构造）承担排序锁、游标、epoch、子表缓存、屏障 ACK、资产证据、owner_epoch 读、saveoutbox；**RedisClient**（控制域）保留重试队列全族、WAITAOF 组提交、`task:result` 与发布通知、expand 状态、协议世代、能力标记、persist 引导标记。映射句柄 `Placement.Redis` 由映射直读 TiDB 的改造移除。
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

**两个句柄**：`tlsRedis` 保留为控制域连接（世界分线目录、神秘商人、`PUBLISH assetop:wake`、battle 活动结果）；新增 `tlsPlayerRedis` 承担玩家域。gate 节点不访问玩家域，不建玩家域连接。

**依赖说明（§11.2）**：不引入 hiredis-cluster / redis-plus-plus。它们没有 muduo 事件循环适配，接入要么另起线程，要么自写适配层，且无法保证上面的「同 slot 停等」与「未执行类错误不外泄」两条契约；路由层需要的能力（CRC16、`CLUSTER SLOTS` 解析、按 slot 分发与重发）用现有 hiredis 1.2.0 的格式化接口即可实现，代码量与风险更可控。

### 5.7 Go 客户端

- 玩家域句柄统一为 `redis.UniversalClient`，由共享构造器 `shared/playerstore` 按配置产出（第 1 批已落）：`Addrs` 一个且 `Cluster=false` 为单实例；`Cluster=true` 为 `ClusterClient`。每节点池上限显式化（缺省每节点 4 条，各服务可按现值设定，例如 go/db、login 单实例阶段沿用 20），`MinIdleConns=0`，空闲 60s 回收；`ContextTimeoutEnabled` 缺省开（ctx 截止约束套接字读写，player_inbox 500ms、guild 2s 这类预算依赖它）；`PoolTimeout` 可配。
- **不自动重放结果未知的命令**：单实例 `MaxRetries=-1`；Cluster 模式下 go-redis 的 process 循环会在 EOF / 超时后自动重发同一命令（最多 MaxRedirects 次），可能重放已在服务端执行的 Lua。构造器用 `OnNewNode` 给每个节点客户端挂钩子，把命令发出后遇到的网络错误包成不可重试的「结果未知」错误，只保留 MOVED / ASK / TRYAGAIN / CLUSTERDOWN / LOADING 这类「未执行」重试——与 C++ 路由层的语义一致。
- 构造器挂发送前守卫：EVAL / EVALSHA / MGET / DEL 等多键命令的键必须同 slot、玩家域的键必须带 tag，否则直接返回错误，单实例开发时同样生效。
- 跨玩家 MGET 统一走 `playerstore.MGetBySlot`：按 slot 分组后 pipeline（ClusterClient 自动按节点拆批），结果按输入顺序还原。
- 全库扫描分两种：离线全量任务用 `playerstore.ScanEachMaster`——逐主节点扫完，任何一个节点失败就整体失败；扫描前后各取一次拓扑指纹（每个主节点的节点 ID、run_id 与所持 slot 段，以及有无进行中的迁移），扫描所用的主节点集合取自指纹，前后不一致、或看到进行中的迁移，就返回 `ErrTopologyChanged`（fail-closed，调用方不得据此断言「键不存在」）；按请求分页的在线目录用 `playerstore.ScanPage`（第 6 批加）：游标 = 指纹摘要 + 主节点序号 + 节点内游标，指纹变化即游标失效、从头开始。
- go-zero 句柄（scene_manager、friend、match）在玩家域改用 go-redis `UniversalClient`；控制域照旧。

### 5.8 其他服务的玩家域读写

friend（会话、父快照、同步摘要）、guild（会话、位置、战斗锁、同步摘要）、match（会话、战斗锁、父快照、同步摘要）、mail（会话、位置）、trade（位置）、player_inbox（owner_epoch、位置）、login（父快照、玩家锁、登录代次、合服提示、外观补齐）各自新增 `PlayerStore` 配置并改用上节的构造器；跨玩家 MGET 改 `MGetBySlot`，friend 在线目录改 `ScanPage`（游标格式随之变化，是客户端可见的不透明游标，按 [G4a §20.5](shared-redis-split-by-purpose.md) 已登记的两版同批评审处理）。guild 的 `assetop:wake` 订阅、去重键、战斗活动结果仍在控制域。

### 5.9 player_locator

按 [B0-1 设计 §11](player-locator-gate-session-index.md) 的既定形状落地：会话 `player:session:{pid}`；租约四键与 gate 索引按会话 pid 的 slot-tag 分桶，会话 CAS 与本 slot 的租约、索引在同一脚本里原子维护（不变量 I1 不变）。`claimExpiredLeasesScript` 改为按 slot 认领，只返回成员与令牌、不在脚本里读其他玩家的会话；逐人的提交脚本声明会话键后再核状态。`""` 哨兵挪进 ARGV。LeaseMonitor 与死 gate 清扫按 §5.4 同一套「成员登记 + 段租约」分片。补建 / 审计用 `ScanEachMaster`（逐主节点核 run_id 与迁移状态）。就绪 / 代次 / 审计时间这几个全局控制键留在控制域。能力键按设计换 v2 前缀。

### 5.10 scene_manager 离场拆分

原 `luaDeletePlayerLocationIfUnchanged` 把删除位置与递减场景 / 节点人数绑在一次提交里，同时覆盖 go-zero 原样重发与 player_locator 重试（`leavescenelogic.go:34-38`）。分到两个域之后计数器做不到「谁删谁减」：LeaveScene 预读到位置为空就直接回成功（`:207`），第二步若在两步之间失败，人数永久多计；而实例只有人数不大于 0 才会进空闲销毁（`instance_lifecycle.go:113`），多计会让实例永不销毁。

**方向：人数改为按成员集合计。** 控制域 `instance:<scene>:members`、`node:zone:<z>:<node>:members` 为 SET（成员 pid），人数 = SCARD；进场 / 预占 SADD，离场 SREM——天然幂等，原样重发、调用方重试、并发重进场都不会多减或漏减，不再需要墓碑与去重键（v1 的墓碑方案作废，`leave_tomb` 后缀保留在保留表里以防误用）。离场两步：玩家域 CAS 删除位置等键；控制域 SREM。第二步失败时 player_locator 的重试会再次驱动 SREM（SREM 不依赖第一步是否「本次」删除）。

预占语义（`luaReserveBestWorldChannel` 先 INCR 再落点、按人数选线）与销毁判定改用 SCARD 的细节，第 3 批读完 scene_manager 全部计数入口后定稿；在那之前本节是设计方向，不是已核实的方案。

### 5.11 战斗结算

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

- `N_cpp`：访问玩家域的 C++ 进程数（scene + battle；gate 不连），每进程每主节点 1 条。
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
- owner_epoch 的 INCR 丢失会让代次回退，理论上给旧持有者重新放行——今天就存在的风险，建议后续在铸造后同一节点连接上发 `WAITAOF 1 1`（scene_manager 是铸造唯一入口），列为 §16 的独立项，不并入分片批次。**2026-10-24 已落码（未编译）**：失败语义、时间预算、延迟与三种拓扑的取值见 [zone Redis 回退防护 §4.3.3](zone-redis-regression-guard-20261022.md)；本域每主 1 副本时取 `1 1`，代价是该主的副本不可用期间这 1/M 玩家的首登与换节点回 7。第 6 批把 scene_manager 换成玩家域句柄时，铸造通道要按该节列的三处改（按 slot 取主节点再发 pipeline、重定向类重发、切主后的回滚），在那之前它对 cluster 句柄拒启。
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
| 各处「共享库不许集群化」的注释 | 随对应批次改为「玩家域在 PlayerStore（Cluster），控制域仍单实例」 |
| cross_server 原则「同一连接 FIFO」 | 收窄为「同一 slot 内 FIFO」，由路由层停等保证 |
| 「真 Redis 验证 7.2 与 8.x 两版都跑」 | 玩家域只跑 ≥ 8.4（取 8.10.x），原子迁移需要；控制域仍两版 |

**上线后的键形变更**：本稿的改名只在首次上线前直接进行。上线后若再改任何玩家域键形，必须停写切换（全部写者排空后一次切换），禁止双读双写（围栏原语在新旧键名上会各自「成功」，[G4a §20.7](shared-redis-split-by-purpose.md)）。

## 13. 分批落码计划

每批不超过 30 个文件（含测试），批间做语义评审；全部构建、测试、压测由 Codex 执行（AGENTS.md §10.1）。**同一键族的 C++ / Go 镜像、以及读写该键族的工具（merge_zone、barrierrestore、debug_*），在同一批内改完；来不及改的工具在该批改为拒绝运行（fail-closed），不得留到后面批次带着旧键名静默空过。**

| 批 | 内容 | 状态 |
|---|---|---|
| 1 | 基础契约：Go `shared/redisslot`（CRC16、HashSlot、PlayerSlot、SlotTag、保留后缀表）与 `shared/playerstore`（UniversalClient 构造器、每节点池上限、同槽 / 带 tag 守卫、MGetBySlot、ScanEachMaster）；C++ `redis_slot.h`；两侧金样测试；本设计文档 | 已落码，未编译；评审后的修补见本批补丁 |
| 2a | owner_epoch 改名：C++ `player_ownership_comp.h` 与 `role_admission_system.cpp` 的内联拼键、存盘 Lua 的 owner 键形校验；Go `shared/ownerepoch` 与 merge_zone `save_barrier_verify` 镜像、barrierrestore；data_service 改查保留后缀表；相关测试 | 已落码，未编译（2026-10-24；另补 merge_zone blob 拷贝跳过保留后缀，见 PROGRESS） |
| 2b | 父快照改名：C++ `redis_client.h` 父键构造与 Lua 父键校验；login dataloader、外观补齐（同批改 `MGetBySlot`）；data_service 账本与 debug 工具；friend、match 读者；merge_zone `player_rows`；barrierrestore；相关测试 | 已落码，未编译（2026-10-24；共享构造 `savebarrier.ParentKey`，见 PROGRESS）。落码时本会话尚未看到上方 10-24 方向变更；保留还是回退待用户定 |
| 2c | 补投索引按 slot：C++ 索引键与 Lua 索引校验；saveoutbox 契约、认领、回收改按 slot 索引（本批暂保留「只认领 topic 属于本 zone 的意图」，沿用单 topic 发布器）；merge_zone P4 排空检查、barrierrestore 恢复脚本改形、debug_import 脚本改形；相关测试 | 暂停（2026-10-24）：补投索引属 TiDB 决策 §9.3 A 类，待用户按 §9 重评，未开工 |
| 2d | 补投器分片：成员登记 + 段租约、多 topic 发布器与按 topic 熔断、逐意图闸、去掉映射读与跨 zone 迁移脚本、续约前提；相关测试 | 暂停（2026-10-24）：同 2c，未开工 |
| 3 | 归属与位置族改名：handoff、location、修订、离场、去重、冷却、登录代次、同步摘要、玩家锁、合服提示；C++ 与 Go 镜像；scene_manager 离场拆分与成员集合计数（先核实预占语义） | 待做 |
| 4 | go/db 玩家域句柄：排序锁 / 游标 / 缓存键改名，句柄拆分，expand 状态缓存 | 待做 |
| 5a | C++ 两份 `Hiredis` 加 `formattedCommand`；`RedisClusterClient`（停等、错误归类、拓扑、资源上限）与单测；暂无调用方 | 待做 |
| 5b | scene 接线：`tlsPlayerRedis`、`RedisSystem`、`MessageAsyncClient` 改接口 | 待做 |
| 5c | 其余 C++ 调用点换句柄（player_lifecycle、player_battle、battle_room_manager、player_team 拆 MGET 等） | 待做 |
| 6 | 各 Go 服务玩家域句柄：login、scene_manager、player_inbox、friend（含 `ScanPage`）、guild、match、mail、trade、data_service | 待做 |
| 7 | player_locator 按 slot 分桶（§5.9） | 待做 |
| 8 | 战斗结算按 slot（§5.11） | 待做 |
| 9 | 工具收尾：前面批次标为 fail-closed 的工具恢复可用，data_consistency_check 等 | 待做 |
| 10 | 部署：K8s 玩家域 Cluster 清单（≥ 8.4，每 Pod 一个 Service 宣告 IP）与初始化 Job、原子迁移扩缩容手册、各服务配置渲染、连接总账；compose 开发集群（8.10.x）；容量基准工具。G4a 会话 2026-10-23 已落的部署能力要接上：玩家域主节点加入 `k8s_deploy.ps1` 的 `redis-wipe` 遍历；玩家域告警另起 PrometheusRule 组，不并进 `deploy/k8s/redis-alerts.yaml` 的 `redis.control-domain` | 待做 |
| 11 | 登录域（令牌 / 会话 / 栅栏）横向扩展（§10.2），开工前补设计 | 待做 |

映射域不在本稿批次内（§5.13）。

## 14. 验证与验收（交 Codex 执行）

### 14.1 单元与契约

- Go：`cd go/shared && go test ./redisslot/... ./playerstore/...`；C++：`rpc_controller_test` 中的 `RedisSlotTest.*`。两侧金样（§5.2）逐字一致；SlotTag 全表 SHA-256 与 FNV-1a 64 一致。
- 每批对涉及的多键脚本做「全部 KEYS 同 slot」契约断言（按 §5.2 的 HashSlot 计算）。

### 14.2 真集群集成（玩家域 Redis 8.10.x）

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

Java 版玩家存盘是单个 SQL 事务（`player` / `player_state` 两表），Redis 只承载目录、锁、信号与缓存，没有本稿的父快照屏障、补投索引、go/db 发布链。本稿各批只改服务端内部，不改客户端契约，**Java 没有对应待做项**；Java 自身 Redisson 单实例的集群化已在其 PARITY.md 第 274 行登记，本稿不新增行。会影响客户端的两项（friend 在线目录游标格式、不改成转区）按既有两版同批评审处理：前者在第 6 批落码时提交评审，后者本稿已确定不改。

## 16. 风险与未决

1. **owner_epoch 铸造的异步复制丢失**（§7）：现存风险，建议独立立项在铸造后同节点 `WAITAOF`。2026-10-24 已落码（未编译，[回退防护 §4.3.3](zone-redis-regression-guard-20261022.md)）；剩余未决是 R-b 压测下的进场延迟与吞吐上限（组提交是升级路线），以及第 6 批的 Cluster 适配。
2. **离场人数改成员集合**（§5.10）：预占语义与销毁判定的细节第 3 批定稿。
3. **Go 服务按 zone 部署的进程数**：物理 zone 增长时它主导连接数（§6.2），建议 Go 服务改为一进程多 zone，单独立项。
4. **DBTask 按 home_zone 分 topic**：逻辑 zone 达到 10 万时 Kafka topic 数不可行，与 Redis 无关，单独立项。
5. **登录令牌 / 会话仍在控制域单主**（§10.2）：第 11 批之前它决定系统级上限。
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
