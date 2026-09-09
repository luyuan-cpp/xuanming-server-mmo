# ID 与寻址身份改造 —— 逐文件改动说明(2026-09-08 最终版)

> **本文是当天全部改动的落地清单,以最终状态为准**(05:11 的初版写于晚间修订之前,已整体重写)。
> 配套文档:
> - [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) —— 发号改造方案(§7.5 是用户晚间拍板的修订,覆盖前文冲突处)
> - [routing-identity-audit-20260908.md](./routing-identity-audit-20260908.md) —— 寻址身份对抗审计与八条缺陷的修复
> - [control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md) —— 控制面主题改分区
> - [node-id-overhaul-qa-20260908.md](./node-id-overhaul-qa-20260908.md) —— 问答全记录
> - [snowflake-id-allocation.md](./snowflake-id-allocation.md) —— 现状速查(已按最终状态重写)
>
> 全部改动**未提交**,在 git 工作树里(约 317 个条目,其中约 55 个是协议重生成的产物)。
> 同日另有一个会话在做**合服**([merge-zone-overhaul-20260908.md](./merge-zone-overhaul-20260908.md)),
> 与本文只在 `go/login` 有边界,双方无覆盖。

---

## 0. 这一天做了两件事

| | 发号(minting) | 寻址(addressing) |
|---|---|---|
| **问题** | node_id 兼任 snowflake 的 17 位 worker,而它被立刻复用 → 跨重启撞号 | node_id 被立刻复用,而复用防护只覆盖了 Kafka 一条腿 → 错投、丢失、吞消息 |
| **手段** | 永久 ID 改**号段**(无 worker / 无时钟 / 无 lease);剩余时序 ID 的槽位改"最久未用 + 隔离期 + 水位 + 自 fence" | 给地址加**代次**(Kafka broker epoch 同款);结算改**先落库再投递**;控制面主题改**固定分区** |
| **结果** | scene 彻底退出 snowflake 槽位协议;槽位协议只剩 Go 四种服务 | node_id 回归纯寻址,10 万节点规模下 Kafka 不再按节点数膨胀 |

---

## 1. 协议(proto)与生成代码

| 文件 | 改了什么 | 为什么 |
|---|---|---|
| `proto/common/rollback/transaction_log.proto` | `TransactionLogEntry.zone_id = 15` | 消费者要知道捕获时的 zone,不能回查 Router(玩家可能已迁区) |
| `proto/common/rollback/player_snapshot.proto` | `PlayerSnapshotEntry.zone_id = 9` | 同上 |
| `proto/common/database/mysql_database_table.proto` | 移出 `player_snapshot` / `rollback_audit_log` | zone 库建表清单从本文件的消息成员派生;这两张表只能在全局库 |
| `proto/common/database/rollback_database_table.proto` | 接收上面两个消息;`player_snapshot` 加 `snapshot_guid=9`、`source=10`;`rollback_audit_log` 加 `orphans_cleaned=13`;`transaction_log` 列名 `timestamp`→`timestamp_sec`、四个复合索引、TiDB 方言、**`zone_id=15`**;新增 `id_segment {biz_tag PK, max_id, step, version}` | 手写 DDL 与 proto 早已漂移,全部收回 proto 驱动 |
| `proto/data_service/data_service.proto` | 新 RPC `AllocateIdSegment(biz_tag, step) → {error_code, lo, hi}` | 号段服务端接口,`[lo, hi)` 半开 |
| `proto/common/base/config.proto` | `BaseDeployConfig.cluster_id = 18`;**`IdSegmentConfig id_segment = 19`**(内含 `repeated IdSegmentKindConfig kinds`,每项 `{kind, enabled, initial_step, min_step, max_step}`);Kafka 命令主题的分区数 / 代际 / 旧主题开关 | 每种 GUID 一块配置;**刻意没有 `fallback_to_snowflake`** —— scene 已无槽位,没有可回退的发号器,只保留 fail-closed |
| `proto/common/base/message.proto` | `NodeMessageHeader.target_player_id = 3` | 修 R13:TCP 推送的收方身份 |
| `proto/**`(路由消息) | `BroadcastToPlayersRequest.player_list = 5`(按 session_id 升序逐项对应);`ProcessClientPlayerMessageRequest.player_id = 3` | 广播与**反向面**(gate→scene)的同一道栅栏 |
| `generated/data/mysql_database_table_list.json`、`go/db/data/…` | 11 张 → 9 张 | zone 库不再建 `player_snapshot` / `rollback_audit_log` |
| `cpp/generated/**`、`go/proto/**`、各服务 `generated/pb/game/message_id.go`、`robot/generated/**` | 重生成(新消息 id 180 = `DataServiceAllocateIdSegment`) | `dev_tools.ps1 proto-gen-build` + `proto-gen-run` 产出,不手改 |

---

## 2. 号段(永久 ID 的新发号方式)

### 2.1 服务端 `go/data_service`

| 文件 | 改了什么 |
|---|---|
| `internal/store/schema.go`(新) | proto2mysql 注册并建/补 4 张表;`Schema.AutoMigrate` 控制;拷贝 go/db 的表名断言守卫;**`ensureIndexes`**:proto2mysql 对已存在的表只 ADD/MODIFY 列**从不建索引**,旧表升级必须自己补;**唯一 DDL 例外**:`id_segment` 用专门语句预建(`biz_tag VARCHAR(64)` + `CHECK max_id < 2^55`),因为 proto2mysql 把 string 主键渲染成 MEDIUMTEXT(MySQL 1170) |
| `internal/store/id_segment_store.go`(新) | 一个事务里 `SELECT … FOR UPDATE` + 版本号 CAS;`hi ≥ 2^55` 拒绝;**生产不自动补种**(`AllowAutoSeed` 仅 dev),未知 tag 返回错误码 20 |
| `internal/store/mysql.go`(新) | 统一 DSN,两个库连接都带 `STRICT_TRANS_TABLES` |
| `internal/logic/id_segment_logic.go`(新) | 参数校验(step 1..1000 万)、错误码映射 |
| `internal/config/config.go`、`etc/data_service.yaml` | `Schema`(`AutoMigrate` 改 `*bool`,因为 go-zero 对缺省的 optional struct 不填 default 标签值)、`Kafka`(含 `TopicGeneration`)、`IdSegment`(`AllowAutoSeed` / `BootstrapTags`) |
| 迁移路径 | 按 `BootstrapTags` **幂等建行**;`player` / `guild` 用消费表 `MAX(id)+1` **抬地板**(备份恢复保护);`item`/`txlog`/`snapshot` 无法这样校验,写进运维手册 |

### 2.2 客户端

| 文件 | 改了什么 |
|---|---|
| `go/shared/idsegment/`(新包) | 双 buffer;剩 ≤10% 单飞预取;段校验 `lo≥1 / hi>lo / hi≤2^55 / lo≥上一段 hi`,违反整段拒收;**动态 step(Leaf)**:<15min 用完 ×2、>30min ÷2、`[MinStep, MaxStep]` 钉住;预取尊重退避;`Minter` 统一号段优先 + 可选回退 |
| `cpp/libs/modules/id_segment/`(新) | `guid_segment_client.{h,cpp}` + `guid_segment_registry.{h,cpp}`:**一种 GUID 一个实例**(item / txlog / snapshot,以后 pet / guild 只加一行),各自缓冲 / 预取 / 退避 / 指标,共用一条到 data_service 的 gRPC 路径,按请求编号分发响应 |
| `cpp/nodes/scene/id_segment_bootstrap.{h,cpp}`(新) | 按配置实例化各 kind;`DependencyGate` 加 "id segments ready" —— 拿到首段前不放玩家进来(启动硬依赖,运行期靠双 buffer 弱依赖) |
| 接线点 | login `CreatePlayer`(biz_tag `player`)、guild `CreateGuild`(`guild`)、C++ `ItemStore::MintGuid`(`item`)、`TransactionLogSystem::GenerateTxId`(`txlog`)、`SnapshotSystem`(`snapshot`) |

**值域不相交(不迁数据的依据)**:号段 `[1, 2^55≈3.6e16)`;bwmarrin player_id 自 2024-10-27 起 ≥2^55;17/15 布局的 guild/item guid 自 2026-06-19 起 ≥2^55。早于该日的 dev 行落在号段域内但计数器从 1 起、实际不可达,注释里写明"不可达 ≠ 不相交"。

---

## 3. snowflake 槽位协议(只剩 Go 四种服务)

| 文件 | 改了什么 |
|---|---|
| `go/shared/snowflakealloc/allocator.go`(重写) | key 协议 `/snowflake/<kind>/c<cluster>/{slots,affinity,released,watermark,watermark_ms}`;**选号 = 最久未用 + 隔离期 Q=4h**(替代最小空闲位);水位 Put 是 `If Value(slots)==uuid` 事务且对 etcd **单调**;失租→**重挂**不自杀;亲和复用只在 `released` 存在时允许且事务里删它;`Close()` = fence → 最终水位 → released;**灰度双向兼容**(cluster 0 同时占用/镜像旧命名空间的 key) |
| `layout.go`(新) | **位切分只在这一层**:`ClusterBits=5` / `SlotBits=12`(login 3/10)、`ComposeWorkerID` / `DecodeWorkerID`。`shared/snowflake` 只认不透明的 17 位 worker(用户明确要求) |
| `selection.go`、`cache.go`、`testdata/selection_vectors.json`(新) | 纯选号函数;本地缓存**故障期才写**(稳态零写盘,水位 Txn 超时 1.5s < 前推 2s 的不等式有编译期断言);15 例跨语言向量 |
| `go/shared/snowflake/fenceclock.go`(新) | 按水位年龄自 fence(F=2h),单调钟与墙钟任一超期即拒发(VM 挂起会停单调钟) |
| login / guild / match / scene_manager | 槽位改新协议;新键 `ClusterId`、`SnowflakeCacheDir`;失租不再退出,`Generate()` 返 `ErrWatermarkStale` 直到水位再写成功 |

**C++ 侧**:当天先写的 `SnowflakeSlotClient` 与 `snow_flake.h` 的集群/槽位常量、期限 fence、缓存文件**按 §7.5 全部删除**,`snow_flake.h` / `snow_flake_manager.h` 与 HEAD 零差异,`tlsSnowflakeManager` 生产调用点为零。

---

## 4. 寻址身份(八条缺陷,详见审计文档)

| 文件 | 改了什么 | 修哪条 |
|---|---|---|
| `cpp/libs/engine/core/node/system/node/node_command_topic.h`(新) | 纯契约:`partition = node_id % P`、主题 `<type>-cmd_g<N>`、P=256 / gen=1 | R12 |
| `node_command_route.h`(新) | `ResolveCommandRoute(nodeType, targetNodeId) → {topic, partition}`,生产者寻址的唯一入口 | R12 |
| `node_kafka_command_filter.h`(新) | `ValidateCommandTarget` 与字段读取拆出来,可脱离 `Node` 单测 | — |
| `kafka_partition_assign_policy.h`(新)、`kafka_consumer.{h,cpp}`、`kafka_manager`、`node.{h,cpp}` | **`assign()` 直接指派分区**(无消费组、无重平衡)、分区契约校验、`OFFSET_END`、关自动提交 | R12 / **R05 / R09** |
| `go/shared/kafkacmd/`(新包) | 同一契约的 Go 侧 + `CommandPartitionBalancer` + 跨语言向量 | R12 |
| 各生产者(C++ `battle_room_manager` / `player_battle`;Go friend / guild / match / scene_manager / player_locator / **login**) | 改走新寻址;**armed `target_gate_id`**(此前多数留 0,数值过滤形同虚设);空 instance id / 非数字 / **gate id 为 0** 一律 fail-closed | R12 / R08 加固 |
| `cpp/libs/services/gate/session/system/session_identity_fence.h`(新) | 推送栅栏:三态判定,gate 写 socket 前比对会话当前绑定玩家 | **R13** |
| `cpp/libs/engine/core/network/broadcast_target_codec.h`(新) | 广播的**成对**编解码(两侧实现收编在一处,防漂移),按 session 去重 | R13 |
| `player_message_utils.cpp`、`gate_service_handler.cpp`、`player_lifecycle.cpp` 等 | 所有 scene 侧发送路径填 `target_player_id`;**反向面**(gate→scene)填 `player_id` 并在 scene 侧校验 | R13 |
| `cpp/libs/services/battle/settlement/settlement_outbox.h`(新) | 结算重投判定 + Redis key / Lua 契约 | **R07** |
| `battle_room_manager.cpp`、`player_battle.cpp` | 结算**先写 `battle:settlement:pending:{pid}` 再投递**(顺序是承重的:先投递会让命令超车导致重复发奖);scene 应用后条件销账;未销账每 10s ×12 轮**重新解析目标**重投(instance uuid 留空,靠命令自带的玩家归属校验);`RefreshRoutingFromSession` 在四个入口自愈路由 | R07 / **R10** |
| `scene_handler.cpp`、`zone_utils.cpp`、`node_message_utils.cpp` | 裸 `entt::entity{node_id}` 改 `FindNodeEntityByZoneAndNodeId`。**"最后一处"的旧注释是错的**,全树复查又找出三处,其中 `GetZoneIdFromNodeId` 会返回**别的节点的 zone** | **R03** |

---

## 5. 部署

| 文件 | 改了什么 |
|---|---|
| `deploy/k8s/manifests/infra/etcd.yaml` | Deployment → 3 副本 StatefulSet + PVC + headless + PDB + 反亲和 + 探针;ClusterIP `etcd` 名字不变 |
| `deploy/k8s/manifests/infra/kafka.yaml` | Deployment → StatefulSet + 20Gi PVC + headless + PDB + KRaft 探针;**保持单 broker**(全仓 topic 都是 replication-factor 1,加副本不改 advertised listener 反而误路由);**顺带修**:旧 emptyDir 挂在 `/tmp/kafka-logs` 而镜像写 `/tmp/kraft-combined-logs`(那个卷从来没被用过)、探针继承 1G 堆参数会 OOM、`CLUSTER_ID` 与 `KAFKA_CLUSTER_ID` 环境变量名、文件描述符上限 |
| `deploy/k8s/manifests/infra/kafka-topic-init.yaml`(新)、`docker-compose.yml` | 预建带代际的四个主题并**校验分区数**;命令主题 256 分区 / 保留 1h;审计主题 6 与 3 分区 / 保留 30 天;**创建后再 alter 并回读**(`--create --config` 只在创建时生效,被抢先自动建的主题会静默沿用 broker 默认) |
| `deploy/env/kafka.*.env` | broker 默认保留期 dev 60s→30min、prod-like 300s→1h;`db_task_zone_N` dev 300s→1h、prod-like 600s→6h。**顺带修**:login 与 db 都对 `db_task` 调 `EnsureTopics` 但传值不同,保留期在 15 分钟与 24 小时间横跳,而这是玩家存盘通路 |
| `tools/scripts/k8s_deploy.ps1` | scene-manager ConfigMap 补 `ZoneId` / `LeaseTTL`、删废弃 `NodeID`;`-ClusterId` 贯穿;`SNOWFLAKE_CACHE_DIR` + emptyDir;login 的 `DataServiceRpc` / `IdSegment`;data-service 的 `Schema` / `Kafka` / `IdSegment` 与 `strategy: Recreate`;etcd 与 kafka 的 StatefulSet 切换与就绪等待;topic-init Job |
| `deploy/mysql-init/00_init_zone_dbs.sql`、README、AGENTS.md | 本地补建 `testdb` 并授权;新键、首次切换会丢弃现有数据、恢复全局库前须核对 `id_segment.max_id`、guild 尚无 k8s 清单 |

---

## 6. 测试

| 位置 | 内容 |
|---|---|
| `cpp/tests/routing_identity_test/`(新,19 例) | 推送栅栏放行/**复用 node_id 劫持时丢弃**/同代次 session 回绕丢弃/未绑定丢弃/老发送方放行;广播编解码两种编码的逐项对齐、去重、端到端只丢被劫持的那条;实体槽位与 node_id 刻意解耦,断言 `FindNodeEntityByZoneAndNodeId` 正确**且**裸 `entt::entity{node_id}` 会落到别处;结算重投的四种判定 |
| `cpp/tests/kafka_command_test/`(新,13 例) | 分区映射、主题名、代际、旧主题开关,与 Go 读同一份向量 |
| `cpp/tests/bag_test`(143 例) | 新增号段套件:按 kind 隔离、动态 step 增减、注册表查找、未就绪时 fail-closed;**状态变更套件仍在文件最后** |
| `go/shared/snowflakealloc`(真 etcd 48 例) | 隔离期、失租重挂、released 三进程回归、灰度双向兼容、缓存启动、并发唯一、耗尽 |
| `go/data_service`(真 MySQL) | 建表幂等、**旧表升级**(用 HEAD 的手写 DDL 建表再迁移)、号段并发 32×50 无缝铺满、封顶、去重、读路径过滤、重放幂等 |
| `go/shared/idsegment`、`go/login`、`go/guild` | 动态 step 四种情形、预取尊重退避、64 协程×1 万唯一、建角/建帮的号段路径与 fail-closed |

**最终验证**:C++ 全量编译 0 错误;四个测试套件 202 例全过;Go 各模块编译与测试全绿;部署侧离线渲染断言全过(k8s 68 项 + 早前 79 项)。

---

## 7. 留给运维的动作

1. **`DisableLegacyPerNodeTopic` 待翻**(`bin/etc/base_deploy_config.yaml`):生产者侧前置条件已全部满足(全仓无旧主题生产者),但翻它只关掉旧的**消费**路径,必须等新版本 gate/scene 滚更完成、旧主题写入与积压归零后由运维显式翻。
2. **R13 的兼容位有到期日**:`target_player_id` / `player_id` / `player_list` 为空时收方放行并只记一次 INFO;**灰度一个版本后必须改为丢弃**,否则栅栏等于没加。
3. **首次切换 Kafka 到 PVC 会丢弃现有 topic 数据**,之后必须重跑 `infra-kafka-topics`,否则生产者自动建成 1 分区、gate/scene 的分区契约校验会拒绝启动。选无人时段。
4. **恢复全局库前**须核对每个 tag 的 `id_segment.max_id ≥ 消费侧最大号 + 1`;迁移里的自动地板校验只在同库查得到时兜底。
5. **未做**:真集群故障注入(冻结进程、拔 lease、杀 broker);Kafka 与 Redis 的实际投递链路只有决策函数被单测覆盖。

---

## 8. 顺带发现、未处理

- 协议生成器会往 `D:\luyuan\wuxingqitan\mmorpg-client`(Unity)写代码,那边有既存的行尾漂移与未提交的 handler 桩,与本轮无关。
- `tools/scripts/dev_tools.ps1` 的 k8s 包装器不转发 `-ClusterId` 与 `infra-kafka-topics`。
- guild / friend / chat 没有 k8s ConfigMap 与清单(既有缺口)。
- `image: apache/kafka:latest` 是浮动 tag 且无 `imagePullPolicy`,配持久化日志目录后是版本漂移风险,生产应钉版本。
- `SendBindSessionToGate` 的 `sessionVersion` 参数从未被传输(`GateCommand` 无该字段),行为保持原样。
- C++ 侧没有 `TopicGeneration` 的等价物,审计主题的代际升级需要**手工同步两个常量**,yaml 注释已写明这条义务。
