# 合服（merge_zone）链路整改 —— 全部修复与验证记录（2026-09-08）

> 本文记录「把合服链路修到没有 bug」这一轮的**全部改动、判据与残余风险**。
> 全部改动在 git 工作树里，**未提交**。
>
> 关联文档：
> - [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) —— 号源方案（§7.5 是最终口径）
> - [node-id-overhaul-qa-20260908.md](./node-id-overhaul-qa-20260908.md) —— 号源问答全记录
> - [node-id-overhaul-changelog-20260908.md](./node-id-overhaul-changelog-20260908.md) —— 号源逐文件改动
> - [../ops/merge-zone-runbook.md](../ops/merge-zone-runbook.md) —— 已按本轮实现重写的运维手册
> - [server_merge_design.md](./server_merge_design.md) —— 设计（本轮标注了被取代的段落）
> - [server-merge-gap-fixes.md](./server-merge-gap-fixes.md) —— 缺口清单（本轮更新了开/闭状态）

---

## 0. 一句话结论

合服的失败模式**几乎全是静默的**：写错 Redis 库、ConfigMap 键名对不上、收集到空集合、重置一个不存在的 Kafka topic ——
每一条都报告成功。这一轮把这些静默失败逐条改成**要么正确、要么明确拒绝**，并补齐了真正会丢数据的那一步（玩家主数据迁移）。

---

## 1. 三条会真正丢数据或锁死玩家的

### 1.1 玩家主数据根本没搬（HIGH，会静默丢号）

**事实**：TiDB 决策文档 D7 的「零玩家主数据迁移」属于 **Phase 2，明确未做**。
今天玩家主数据的真实路径是：C++ scene → Kafka `db_task_zone_{N}` → go/db → MySQL `zone_{N}_db`。
而合服工具原来只拷 data_service 的 `player:{id}:*` blob，那批键**没有任何游戏路径读写**
（`LoadPlayerData` / `SavePlayerData` 在 cpp/ 与 go/ 的非 data_service 代码里零调用点）。

**后果链**：合服 remap 后玩家进目标区 → 目标 zone 库无该玩家行 →
go/db 的 `ErrNoRowsFound` 被吞掉 → 返回空 proto 并写进缓存 → **玩家以空号进游戏** →
首次存盘把空数据写死进 `zone_dst_db`，原数据永久孤儿在 `zone_src_db`。
共享缓存 24h TTL 还会让当天的冒烟测试通过、第二天才爆。

**修复**（`tools/merge_zone`，在 mapping remap **之前**的强制步骤）：
- 按 `generated/data/mysql_database_table_list.json` + proto 表选项推导出所有带 `player_id` 的玩家表
  （至少 `player_database` / `player_database_1` / `player_centre_database`），逐表分批
  `INSERT INTO dst.t SELECT * FROM src.t WHERE player_id IN (...)`，每表一个事务。
- **主键重复即中止**：目标库已有该玩家 = ID 安全事件，不是可以忽略的幂等。
- 拷完校验源/目标行数一致。
- 失效共享 Redis（DB 0）里这批玩家的 go/db 与 login 缓存键（`PlayerAllData:{pid}`、`{MsgType}:{pid}`）。
- **前置门禁**：`db_task_zone_{src}` 的 consumer lag 必须为 0，`kafka:retry:queue:*` / `kafka:dead:queue:*` 必须为空，否则拒绝。
- 未来 TiDB Phase 2 落地后才可用 `-skip-player-rows`，且必须同时传 `-i-know-global-player-table`。

### 1.2 生产从未写过 `player:zone:{id}`（HIGH，合服在真实区上是空转）

**事实**：全仓只有 `data_service/cmd/debug_import` 和合服 remap 自己写这张映射。
`login` 的 `CreatePlayer` 只把建角时的 `zone_id` 盖进账号 blob 的角色列表，从不注册归属。

**后果**：在任何真实区上跑合服 → 收集到 **0 个玩家** → blob 步骤什么都不拷 →
`map_matched=0 map_updated=0 notice=0` → 但公会行和公会榜 **确实被搬走了** → exit 0「Done」。
公会说自己在目标区，而每个成员的角色列表和进入路由都还指向已经撤掉的源区。

**修复**：
- `go/login/internal/logic/clientplayerlogin/createplayerlogic.go`：新增步骤 6c，
  在铸号之后、**把角色塞进账号 blob 之前**调用 `RegisterPlayerZone` **fail-closed**；
  失败复用既有 tip `kLoginDataSerializeFailed`（未造新号），账号 blob 与反向索引一个字都不写，账号锁正常释放。
  resolver 为 nil / `DataServiceRpc` 未配置也走同一条拒绝路径。
- `tools/merge_zone`：新增 `-backfill-home-zone -zone <id>`，从 `zone_{N}_db.player_database`
  用 **SETNX** 补齐历史玩家（绝不覆盖已有值）；两个区在首次合服前都要跑。
- 守卫：收集到 0 个玩家时拒绝 `-apply`（除非显式 `-allow-empty-source`）；
  `SELECT COUNT(*) FROM zone_{src}_db.player_database` 大于收集数时也拒绝（映射不全 → 先跑 backfill）。

### 1.3 `RemapHomeZoneForMerge` 按值匹配 + 完全无鉴权（HIGH）

**事实**：该 RPC 用 `value == src` 匹配，文档里的 R2 回滚步骤让运维反向跑 `target → src`
—— 那会把**目标区原住民**一起搬进已经废弃的源区，整个目标区锁死。
同时该 RPC 在 gRPC 端口上**任何客户端都能调**，而设计文档还宣传它可以在服务在线时使用，与手册的停写要求直接矛盾。

**修复**：
- 新增配置 `AdminToken`（json optional）：**留空 = 整个 RPC 禁用**（`codes.PermissionDenied`），
  非空时与 metadata `x-admin-token` 做 `subtle.ConstantTimeCompare`。
- 必须已存在 fence 标记 `merge:in_progress:{src}` 才肯执行（apply 与 dry-run 同样要求），
  否则 `codes.FailedPrecondition`，零改动。
- 每次调用记录调用方身份（peer 地址 + user-agent + `x-operator`，**从不记录 token**）与影响行数。
- 文档：`server_merge_design.md` §5.5 的 R2 **删除**并记录原因，回滚改指向 `merge-zone-unmerge -manifest`。

---

## 2. 两条「从未生效过」

### 2.1 合服通知一次都没触发过（MEDIUM）

工具把 `player_merge_notice:{pid}` 写进 **mapping Redis**，而 login 的 `consumePostMergeFlags`
从**自己的 RedisClient（DB 0）**读。写读两边不是同一个库，所以这个通知**从上线起一次都没弹出过**，
而且没有任何错误——键就在那儿，只是没人读。

**修复**：新增 `-notice-redis-addr` / `-notice-redis-db`（默认 login 的 DB 0），标记写到 login 真正读的地方；
运维手册补上完整的库映射表。

### 2.2 审计唯一的阻断门禁恒绿（MEDIUM）

- `friend:online:{pid}` 被在 **mapping Redis** 上查，而该键实际在 **friend 的 DB 3**
  → 恒查不到 → 恒「源区玩家全部离线 ✅」。也就是说「还有玩家在线就阻断合服」这条门禁**从来没有阻断过任何东西**。
- 映射扫描超时降级成 `warn` 而不是 `block`；文档写的 exit 2（基础设施错误）从未真正返回过。
- `-verify-merged` 只改了个日志标题，手册 §5 承诺的四条断言**一条都没实现**。

**修复**：
- 在线检查改到 friend Redis DB 3（`-friend-redis-db`），并同时查 DB 0 的 `player:session:{pid}`。
- 扫描失败/超时改为 `block`；任一句柄为 nil 返回 exit 2。
- 新增 Kafka 重试/死信队列与 `lock:player:*` / `player:{id}:__lock` 的审计项。
- `-verify-merged` 真正实现四条 block 级断言：源区映射计数为 0；`guild WHERE zone_id=src` 为 0；
  `guild_rank:zone:src` 不存在；`ZCARD guild_rank:zone:dst` 等于目标区公会数；
  目标 zone 库行数不少于已合并玩家数；并接受 T-1 记录的 `-expected-src-players` 做对比。

---

## 3. 我自己制造又拆掉的雷：go-zero 的 RedisConf 没有 DB 字段

**这条推翻了一个审计结论，必须记住。**

`go-zero v1.10.0` 的 `core/stores/redis/conf.go` 中，`RedisConf` 只有
`Host / Type / User / Pass / Tls / NonBlock / PingTimeout` —— **没有 `DB` 字段**。
yaml 里写 `DB: 15` 会被反序列化**静默忽略**。data_service 用
`redis.MustNewRedis(config.MappingRedis)` 建客户端，所以 mapping Redis **永远落在 DB 0**。

早先审计得出「工具默认 0、服务是 15，所以工具扫错库」——**结论是反的**：
工具的 0 才是对的，`data_service.yaml` 里那行 `DB: 15` 是个从未生效的谎。
我按错误结论派活，agent 真把默认改成了 15；data_service 那一路实测发现后我全部改回：

| 位置 | 改动 |
|---|---|
| `tools/merge_zone/main.go` | `defaultMappingRedisDB = 0` + 长注释说明原因；`-mapping-redis-db` 帮助文案 |
| `tools/merge_zone/merge_run.go` | 空集合报错文案里的「data_service MappingRedis 是 DB N」 |
| `tools/merge_zone/merge_unit_test.go` | 断言从 15 改成 0 |
| `tools/merge_zone/{audit_checks,audit_resources,fence,post_merge_stamp}.go` | 注释里所有 `DB 15` |
| `tools/scripts/dev_tools.ps1` | `Get-MergeZoneArgs` 的兜底值 15 → 0；**参数速查表**里的 `mapping DB 15` |
| `go/data_service/etc/data_service.yaml` | 删除那行 inert 的 `DB` |

其中**参数速查表最危险**：运维照抄 `-MergeMappingRedisDB 15` 就会让 fence 与 remap 一起指向
data_service 从不碰的库 —— 审计恒绿、合服报告成功却一个 key 都没改。

**通用判据**：任何「服务 yaml 里的 Redis DB 号」都要先确认那个配置结构体真的有 DB 字段。
go-zero 的 `RedisConf` 没有，go-redis 的 `Options` 有。别看 yaml 写了就信。

---

## 4. 配置形状不对 = 静默失败（C++ 侧）

### 4.1 ConfigMap 发的键名与 C++ 读的对不上（HIGH，组合起来是 P0）

- k8s node ConfigMap 发的是 `GuidSegment: {Enabled, Kinds:[{Tag, Step, MinStep, MaxStep}]}`。
- C++ `cpp/libs/engine/config/config.cpp` 读的是**顶层 `IdSegments:` 列表**，
  键名 `Kind / Enabled / InitialStep / MinStep / MaxStep`。
- 键不存在 = proto 默认值 = `kinds` **为空**，且**不报任何错**。

叠加第二个事实：`GuidSegmentRegistry::AllEnabledReady()` 在**零个种类启用**时**恒为真**。
两者合起来：DependencyGate 照常放玩家进来，但每一次拾取、每一条交易流水、每一个快照全部 fail-closed。
**玩家进得去、什么都拿不到，而配置看上去是配好的。**

**修复**：`tools/scripts/k8s_deploy.ps1` 新增 `Get-AuthoritativeYamlBlock`，
从 `bin/etc/base_deploy_config.yaml` **整块提取** `IdSegments:`（缺文件或缺块直接抛错，fail-closed）；
删除错误的 `GuidSegment:` 与 `AuditTopicGeneration:` 发射。
PyYAML 断言逐项比对渲染结果与权威文件（3 个 kind、5 个键名全等）。

### 4.2 `AuditTopicGeneration` 是个没人读的键

C++ 侧的 Kafka 审计主题代际后缀是**编译期常量**：
`transaction_log_topic_g1`（`cpp/libs/modules/transaction_log/transaction_log_system.h`）、
`player_snapshot_topic_g1`（`cpp/libs/modules/snapshot/snapshot_system.h`）。
`grep "Generation" cpp/libs/engine/config/config.cpp` 零命中——**根本没有这个配置项**。
所以 ConfigMap 之前发的 `AuditTopicGeneration` 比不发更糟：看起来像配好了。已删除。

---

## 5. 登录与进入链路

| 项 | 修复 |
|---|---|
| 角色列表区号是建角快照 | Login 返回列表前用一次 `BatchGetPlayerHomeZone` 批量解析当前归属并覆盖；**返回 proto.Clone 副本，绝不回写账号 blob**（多一份副本就多一处要同步）。失败/缺失一律保留建角值，绝不阻断登录。 |
| EnterGame 不按归属路由 | 新增按 home_zone 覆盖 `ZoneId`（`GateZoneId` 保持本区）以触发 scene_manager 既有的跨区重定向。**默认关闭**（`HomeZone.RedirectOnEnterEnabled`），因为 Unity 客户端还不响应重定向。 |
| 重连/顶号被误重定向 | 新增 `homeZoneOverrideAllowed(decision, existing)`：只有**首次登录且无在场 scene**才允许覆盖。重连与顶号必须回原 scene，否则 scene_manager 的跨节点安全闸会直接拒掉，本来能进的玩家反而进不去。 |
| **`EnterScene` 的 `ErrorCode != 0` 被当成功**（既有 bug，与合服无关） | 以前只记一条 ERROR 就继续 `SetIdempotency` 并清掉登录会话 → 客户端既等不到 `RoutePlayer`、重试又撞 `kLoginSessionNotFound`，**玩家彻底卡死**。现在返回 error，异步链靠 `applyErr != nil` 才会**跳过** `cleanupLoginSessionState`，把会话留给客户端重试。 |
| 缺失映射刷 ERROR 日志 | data_service 改返回 `codes.NotFound`（消息保留 `no home zone mapping` 文案以兼容旧契约）；login 侧新旧两种契约都认，未映射不再按 ERROR 刷屏。 |
| data_service 抖动拖慢登录 | `DataServiceRpc` 打开 go-zero breaker，保持 `NonBlock: true`；角色列表用 500ms 预算、进入用 1.5s、注册用 3s。 |
| **客户端从不响应 RedirectToGate（msg 124）** | robot 实现完整五步：校验目标与 token 期限 → 跳数上限 3（防 A↔B 映射环）→ 拨号探测 → 换连接 → token 校验 → **重跑 Login + EnterGame**（token 只认证 TCP 会话，没有跨区登录会话转移）。**Unity 仍未实现 → 生产必须保持重定向关闭。** |

**约束记录**：`FollowRedirect` 必须跑在客户端自己的 `RecvLoop` 协程上。
vendored 的 muduo 从不关闭 `incoming` 通道，阻塞在已关闭连接上 `Recv()` 的协程会**永远阻塞而不是报错**，
只有让读协程自己换连接才不会留下僵尸。

---

## 6. scene_manager

| 项 | 修复 |
|---|---|
| 重定向判定太晚 | `resolveSceneForEnter` 原本先跑，目标区过渡窗口（频道未建好 / 节点刚换代）会让第一条腿死在 `ErrNoAvailableNode`，重定向根本走不到。现在 `ZoneId != 0 && GateZoneId != 0 && ZoneId != GateZoneId` 时**先决定重定向**，且该分支不做预占。 |
| 源区整体下线后的残留定位 | `player:{id}:location` 的节点不存活 **且** 该区 `node_load` ZSET 为空时，视作「无定位」——整个区都没了，不可能有节点正在写。其余情况保持 `AllowUnsafeCrossNodeHandoff` 原语义。 |

---

## 7. data_service 与 guild

| 项 | 修复 |
|---|---|
| `RegisterPlayerZone` 会覆盖 | 改成 Lua 原子 `SET NX`：不存在则写；同区幂等；**不同区返回冲突并回传已有 zone**（`codes.AlreadyExists` + `ErrCodeZoneMappingConflict`）；值无法解析也拒绝。副作用：`cmd/debug_import` 现在会失败而不是静默改指向。 |
| 合服期间仍可建号/建帮 | 两处都读 fence 标记 `merge:in_progress:{zone}`，存在即拒绝（`codes.FailedPrecondition`）；读取出错也拒绝（fail-closed）。guild 的 `CreateGuild` 把检查放在**铸号之前**——烧掉一个 `guild_id` 是不可逆的。 |
| 公会缓存 30 分钟导致退错榜 | `DisbandGuild` / `RemoveGuildFromRank` 改读 MySQL 权威 `zone_id`（`FOR UPDATE`，与 `UpdateGuildScore` 同款），caller 传的 zone 只当提示；并顺带清扫所有 `guild_rank:zone:*` 里的历史幽灵条目。 |
| 合服未失效公会缓存 | 工具在 guild UPDATE 之后，对每个搬动的公会执行与 `invalidateVersionedCacheScript` 相同的失效（`INCR guild:v2:cache_generation:{id}` + `DEL guild:v2:{id}`）。 |

---

## 8. 工具工程化（原来完全没有的）

| 能力 | 说明 |
|---|---|
| **manifest** | `-manifest-path`（默认 `./merge_<src>_to_<dst>_<ts>.json`），在**第一次写之前**落盘：玩家 id、公会 id、ZSET 成员与分数、已拷表、每步完成标志。重跑读清单而不重新扫描（remap 跑完后源集合已经找不到那批人，这正是清单存在的理由）。 |
| **unmerge** | `-mode unmerge -manifest <file>`：只回滚清单里列出的 id/公会/成员。目标库玩家行仅在与源行逐字节相同时才删，否则拒绝并报告。 |
| **fence** | `merge:in_progress:{src}` 与 `{dst}`，SETNX 带 `run_id`，退出时用 Lua 校验 run_id 后 DEL，TTL 只作进程被杀的兜底。三边契约一致（工具 / data_service / guild）：同键名、同实例 DB 0、**键存在即封锁**、值从不解析。 |
| **原子性** | 公会榜 ZSET 合并改用 `TxPipelined`（MULTI/EXEC），避免 ZADD 失败而 DEL 照样执行导致源榜丢失。 |
| **并发防护** | 前置检查目标区存在、源区 id 无 `lock:player:*` / `player:session:*`；ZSET 步骤持 `guild_rank:maintenance_lock`。 |
| **集群模式 blob 拷贝** | 改用 `ForEachMaster` 迭代（keyless SCAN 在 ClusterClient 上只命中一个随机节点）；跳过 `player:{id}:__lock`（带 TTL 0 拷过去会变成永久锁）；拷贝数不等于 id 数时**中止**而不是告警。 |
| **wrapper 转发** | `dev_tools.ps1` 新增 `merge-zone-unmerge` 命令与 `-MergeBackfillZone`、`-MergeManifestPath`；mapping / notice / friend / scene 四个库参数全部转发；新增 `-MergeKafkaGroup` / `-MergeKafkaTopicGeneration`（默认从 `go/db/etc/db.yaml` 现读，见下）。 |

---

## 9. 回滚链路的同类缺陷（顺带修复）

**不在合服链路，但完全同一类错误。**

`k8s-zone-rollback` 与 `kafka-offset-reset` 的 topic 默认值是 `db_task_topic`，
而**全仓不存在这个 topic** —— go/db 与 go/login 两侧拼的都是 `db_task_zone_<id>`
（`TopicGeneration > 1` 时追加 `_g<gen>`）。
灾难回滚的第 5 步会「成功」地重置一个空 topic，然后 go/db 照样重放目标时间点之后的写入，
**回滚做了等于没做**，且不报任何错。

**修复**：
- `k8s_zone_rollback.ps1`：`-KafkaTopic` 默认改为空，空则按 `-ZoneId` 推导 `db_task_zone_<id>`；
  注释写明 `TopicGeneration > 1` 时推不出后缀，必须显式传。
- `dev_tools.ps1`：`-RollbackKafkaTopic` 默认改空透传。
- `kafka_offset_reset.ps1`：四处示例改成真实名字。

同理，`dev_tools.ps1` 原本也不转发 `-kafka-group` / `-kafka-topic-generation`，
在 `TopicGeneration != 1` 或改过 GroupID 的环境上，合服的积压门禁会去查一个**不存在的 topic**，
lag 恒 0、门禁静默放行。新增 `Get-MergeDbKafka` 从 `go/db/etc/db.yaml` 现读并转发。

> ⚠️ `docs/design/zone_data_rollback.md` 里描述的回滚流程也该照这个 topic 名校一遍，本轮未动。

---

## 10. 权威库映射表

| 用途 | 库 | 典型键 | 来源 |
|---|---|---|---|
| mapping | **DB 0** | `player:zone:{id}` / `lock:player:{id}` / `merge:in_progress:{zone}` | data_service `MappingRedis`（go-zero RedisConf **无 DB 字段** ⇒ 恒 0） |
| guild | DB 2 | `guild_rank:zone:{z}` / `guild:v2:{id}` / `guild_rank:maintenance_lock` | `go/guild/etc/guild.yaml` |
| friend | DB 3 | `friend:online:{pid}` | `go/friend/etc/friend.yaml` |
| shared / login | DB 0 | `player_merge_notice:{pid}` / `player:session:{pid}` / `kafka:{retry,dead}:queue:*` / `PlayerAllData:{pid}` | login / db / player_locator / scene_manager |
| player data | 独立 | `player:{id}:*` | data_service 按 region 分的 Redis Cluster |

---

## 11. 验证记录（全部本机实跑）

| 项 | 结果 |
|---|---|
| C++ 全量 `msbuild game.sln Debug\|x64` | **0 错误 0 警告**（40 个工程产物） |
| C++ `bag_test` | 143 例全过（含新增 `BagSegmentNotReadyTest` 6 例 fail-closed 路径） |
| C++ `currency_test` | 24 例全过 |
| C++ `snow_flake_test` | 27 例全过（不筛选） |
| Go 全部模块 build + vet | login / guild / match / scene_manager / data_service / player_locator / friend / shared×3 / merge_zone / robot **全 OK** |
| Go 全部模块 test | **零 FAIL** |
| `shared/snowflakealloc` 集成测试 | 真 etcd，`ok 69.4s` |
| `data_service` 集成测试 | 真 MySQL，建表幂等 / 号段并发 32×50 无缝铺满 / 封顶 / 去重 / 读路径过滤 / INSERT IGNORE 重放，全过 |
| `guild` 集成测试 | 真 MySQL，权威 zone 退榜三例全过 |
| PowerShell 解析 | `dev_tools.ps1` / `k8s_deploy.ps1` / `k8s_zone_rollback.ps1` / `kafka_offset_reset.ps1` 全部 **0 错误** |
| k8s 渲染断言 | zone-up / infra-up 干跑，PyYAML 逐项断言（含 `IdSegments` 与权威文件全等）**全过** |
| Pester 契约测试 | `k8s_deploy_contract.tests.ps1` **27/27** |
| 行尾/BOM | 所有改过的 ps1 保持 CRLF + BOM，go/md 保持 LF |

---

## 12. 仍然开着的口子（**上线前必须处理**）

| # | 口子 | 后果 | 建议 |
|---|---|---|---|
| B1 | **Unity 客户端不响应 `RedirectToGate`（msg 124）** | 任何服务端重定向都是把玩家卡死 | `HomeZone.RedirectOnEnterEnabled` 生产**必须保持 false**，直到客户端实现（robot 已可作参考实现） |
| B2 | **C++ Kafka 审计主题代际后缀是编译期常量** | 换代需人工同时改两个 `.h` + 服务 yaml 并重打镜像，漏一边 = 生产者写进没人消费的 topic | 要么让 C++ 读配置，要么在 CI 加一致性断言 |
| B3 | **真集群合服演练一次都没跑过** | 本轮全部验证都是本机真 MySQL/Redis + 单元与集成测试，**没有停服窗口的实战** | 上线前按重写后的 runbook 走一次完整演练 |
| B4 | guild 的 `MergeMarkerRedis` 可缺省 | 缺省时合服闸门对 guild **不生效**（启动有日志说明） | 部署时必须配上 |
| B5 | `docs/design/zone_data_rollback.md` 未按新 topic 名校对 | 回滚文档仍可能指向 `db_task_topic` | 单独一轮修 |
| B6 | `GuidSegmentRegistry::AllEnabledReady()` 在零种类时恒真 | 三种 guid 全被显式关闭时，玩家进得去但铸不出号 | 有 `LOG_WARN` 覆盖；ConfigMap 侧已 fail-closed，风险已大幅收窄 |
| B7 | `go/guild/internal/node` 的 `TestAllocDifferentTypeReuse` 在 integration 标签下红 | **既有问题，与本轮无关**：旧 schema 分支扫 NodeInfo 时不按 node_type 过滤，测试用的越界类型映射到同一前缀 | 生产节点类型前缀各不相同，看着是测试脚手架问题 |

---

## 13. 顺带发现并修复的既有缺陷

- **`robot/logic/handler` 与 robot 根包在 HEAD 上就编不过**：`SignalBattleAssigned` / `WaitBattleAssigned`
  的调用点在提交 `c149b7c57` 里加了，但 `gameobject.Player` 的实现**从来没加**。已补（约 52 行，
  照 `battleStart` 的 lazy-channel + `sync.Once` 模式）。
- **k8s scene-manager ConfigMap 缺 `ZoneId`**：Go 侧默认 1 ⇒ **所有 zone 的 SceneManager 都注册成 zone 1**。已补 `ZoneId` 与 `LeaseTTL`，删掉已废弃的 `NodeID`。
- **`tx_id` / `snapshot_id` 两个 Kafka 主题全仓无消费者**，且未进 `EnsureTopics`（按 broker 默认自动建成 1 分区、
  dev 保留 60 秒）⇒ 回滚审计链路实际没落地，每条消息几分钟内被丢掉。已补消费者、主题注册（6/3 分区、保留 30 天）
  与代际后缀。
- **data_service 只注册 go-zero 的 `dataservice.rpc`**，C++ 发现不到 ⇒ scene 永远等不到号段。
  已按 C++ NodeInfo 约定注册到 `DataServiceNodeService.rpc`（端口起来后才注册，注册失败即崩溃退出）。

---

## 14. 改动范围

工作树中非生成物的改动约 **205 个文件**，全部**未提交**。主要落点：

```
go/login/**                     角色列表解析、进入路由、建角写归属、EnterScene 失败语义
go/scene_manager/**             重定向前置、陈旧定位判定
go/data_service/**              NotFound 语义、SETNX 注册、Remap 鉴权与 fence、NodeInfo 注册、
                                Kafka 消费者、号段服务端、proto2mysql 建表
go/guild/**                     建帮 fence、权威 zone 退榜
go/shared/{snowflake,snowflakealloc,idsegment}/**   槽位协议、位切分分层、号段客户端
robot/**                        跟随重定向、补上 HEAD 就缺的 Player 方法
tools/merge_zone/**             全部 13 项修复 + manifest/unmerge/fence/backfill/verify
tools/scripts/*.ps1             ConfigMap 键名对齐、库参数转发、Kafka 参数转发、回滚 topic
cpp/**                          号段客户端与注册表、三处铸号点改走号段、启动门禁
proto/**、bin/etc/*.yaml        配置与表结构
docs/**                         运维手册重写、设计文档标注、缺口清单更新、本文
```
