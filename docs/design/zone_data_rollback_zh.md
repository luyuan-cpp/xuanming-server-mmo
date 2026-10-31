# Zone 数据回档方案

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),回档 RPC 都已删除。本文只作历史保留,不再描述现行行为。

## 概述

Zone 数据回档分两个层面：
1. **应用级回档**（已实现）：通过 `RollbackZone` RPC，从快照恢复玩家数据
2. **灾难恢复级**（Ops 操作）：MySQL PITR + Redis flush + Kafka offset reset

---

## 1. 单玩家回档（应用级）

详见 [single_player_rollback.md](single_player_rollback.md)。

- 踢玩家下线 → 找目标时间点之前最近的快照 → 调用各服务 `ImportPlayerData()` 恢复
- 自动校验交易日志防止道具复制（有正常转移记录的物品不恢复）
- 本质是**补偿式**回档，不是时间倒流

---

## 2. 整 Zone 应用级回档（`RollbackZone` RPC）

### 设计决策：Guild/Friend 不回档

**结论：Guild/Friend 数据不需要回档。**

**原因**：
- Player Redis blob（`player:{id}:player_database`）**不包含** guild_id 或 friend 引用
- Guild 通过 `GuildService` gRPC 独立查询，Friend 通过 `FriendService` gRPC 独立查询
- 两个系统的数据完全解耦，回档 player 数据不会与 guild/friend 产生不一致
- 即使存在时间差异（如玩家回档到加入公会前，但 guild_member 表仍有记录），
  这类不一致通过正常的 guild/friend 操作即可自愈（退出重进、重新申请等）

### 无快照角色处理（fail-closed）

`RollbackZone` **不会再自动删除任何无快照角色**。快照只在 GM、事件和回档
安全点产生，仓库内没有周期性快照任务；因此“目标时间前没有快照”不能证明
“角色是在目标时间后创建的”。把两者等同会删除大量正常老玩家。

| 情况 | 处理 |
|------|------|
| T 之前创建的角色 | 从 player_snapshot 恢复数据 ✅ |
| 目标时间前没有快照的角色 | 仅列入 `orphan_player_ids` 候选名单，不恢复、不删除 |
| 候选角色的 Redis / zone mapping | 保持原样 |
| 候选角色的账号 / guild / friend | 保持原样 |

字段名 `orphan_player_ids` 为保持 wire compatibility 暂不改名；它的当前语义是
“需要人工核对的无快照候选”，**不是已确认的新建角色，更不是已清理结果**。
`orphans_cleaned` 恒为 0。若以后恢复自动清理，必须先引入权威角色创建时间，
并走独立审批、pre-delete 快照和可恢复删除流程。

### 执行流程（2 阶段）

```
Phase 1: 恢复 player 数据
────────────────────────
遍历 zone 内所有有快照的 player
→ 对每个 player: 找 target_time 之前最近的 player_snapshot
→ 创建 pre-rollback 安全快照
→ 用 snapshot 数据覆盖 Redis

Phase 2: 报告无快照候选
──────────────────────
SCAN mapping Redis, 找到该 zone 下所有有 zone mapping 的 player
→ 对比 snapshot 中的 player 列表，得到差集（候选）
→ 将候选写入响应，所有数据和映射保持不变
```

### 代码位置

| 文件 | 作用 |
|------|------|
| `go/data_service/internal/logic/rollback_logic.go` | RollbackZone（player 回档 + 无快照候选报告） |
| `go/data_service/internal/routing/router.go` | GetAllPlayerIDsInZone（SCAN mapping Redis） |
| `go/data_service/internal/store/snapshot_store.go` | player_snapshot + rollback_audit_log CRUD |
| `proto/data_service/data_service.proto` | RollbackZone RPC 定义 |

### 跨服务调用链

```
RollbackZone (data_service)
  ├── Phase 1: 恢复每个 player 的 Redis 数据（从 snapshot）
  └── Phase 2: 返回无快照候选名单（零删除、零账号变更）
```

> **安全边界**：候选名单只能用于人工核对，调用方不得把它解释成“已经清理”。

---

## 3. 整 Zone 灾难恢复级回档（Ops 操作）

### 架构前提

每个 zone 在 K8s 中是独立 namespace（`mmorpg-zone-{zoneName}`），拥有独立的：
- MySQL 实例（数据库名 `game`）
- Redis 实例（`--appendonly yes`）
- Kafka broker / topic（`db_task_topic`）

写路径：`C++ node → Kafka(db_task_topic) → Go db service → MySQL`，Redis 仅为缓存加速层。

### 操作步骤

```
步骤 1: 关闭 zone（停止所有业务写入）
─────────────────────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-down -ZoneName <zone>

步骤 2: MySQL Point-in-Time Recovery (PITR)
────────────────────────────────────────────
# 先恢复到影子库验证
mysqlbinlog --stop-datetime="2026-03-26 10:00:00" binlog.000xxx | mysql -h shadow-host -u root -p game

# 确认无误后替换正式库
# 方式 A: 云 RDS 控制台一键 PITR
# 方式 B: 自建 MySQL 用 binlog 手动恢复

步骤 3: Redis 处理
──────────────────
# Redis 是缓存层，C++ 节点启动时会从 MySQL 重建，直接清空即可
redis-cli -h <redis-host> FLUSHDB

# 或从 AOF/RDB 备份恢复到对应时间点（如果有归档的话）

步骤 4: Kafka 处理
──────────────────
# 清空 db_task_topic，防止回档时间点之后的写入消息被重放
# (2026-10-16 更正,下面两行原文不删:方式 A 不适用于现行管线。topic 名是 db_task_zone_<id>[_g<gen>],分区数是数据顺序协议的一部分,
#  取 go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt(现为 10,不是 5);同名删除重建后 offset 从 0 重新开始,go/db 的永久
#  applied cursor(consumer:applied:<topic>:<key>:<msgType>)仍记着旧 offset,新写会被 orderingForTask 判陈旧(同分区)或不可比(分区数变了)。删除重建等同换代,须按
#  db-task-kafka-partition-contract.md「离线扩容流程」执行;一键脚本 k8s_zone_rollback.ps1 的 Step 5 只用方式 B(-ToDatetime))
# (2026-10-17 更正:kafka_offset_reset.ps1 -DeleteAndRecreateTopic 现在对 db_task topic(db_task_zone_<id>[_g<gen>],判据 lib Test-DbTaskTopicName)
#  一律拒绝,要换一个空 topic 只能把 ServerConfig.Kafka.TopicGeneration 加一换代,不提供同名重建的逃生口。
#  第一行「防止回档时间点之后的写入消息被重放」在 K8s 上也不成立,见下方「2026-10-17 更正」)
# 方式 A: 删除并重建 topic
kafka-topics.sh --delete --topic db_task_topic --bootstrap-server <broker>
kafka-topics.sh --create --topic db_task_topic --partitions 5 --bootstrap-server <broker>

# 方式 B: 重置 consumer group offset 到指定时间戳
# (2026-10-15 更正:组名是本 zone 的 db_rpc_consumer_group_z<id>,topic 是 db_task_zone_<id>,TopicGeneration ≥ 2 时
#  带 _g<gen> 后缀;重置之后还要删掉 legacy 组 db_rpc_consumer_group 在本 topic 上的位点)
kafka-consumer-groups.sh --reset-offsets --to-datetime 2026-03-26T10:00:00.000 \
  --group db_rpc_consumer_group_z<id> --topic db_task_zone_<id> --bootstrap-server <broker> --execute

步骤 5: 重新拉起 zone
─────────────────────
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up \
  -ZoneName <zone> -ZoneId <id> -NodeImage <image> -WaitReady
```

> **2026-10-15 更正(已落码,未编译、未测试,待 Codex 验证)**:go/db 的存盘消费组正逐 zone 从共用的 `db_rpc_consumer_group`(legacy 组)换成
> `db_rpc_consumer_group_z<ZoneId>`(zone 组)。上面的手工步骤已由一键脚本 `tools/scripts/k8s_zone_rollback.ps1`(`dev_tools.ps1 -Command k8s-zone-rollback`,
> 默认 dry-run,`-Apply` 才执行)取代,它的 Kafka 处理现在是:删掉原 Step 2 的排空等待(`-KafkaDrainTimeoutSec` 一并删除);第 0 步 c 对两组在本 topic 上
> 做只读 LAG 快照(只告警);第 0 步 d 在 zone 组还没有位点时于停服前用 dry-run 预演 Step 5;Step 5 把 zone 组重置到目标时间、逐分区复核,再删掉 legacy 组
> 在本 topic 上的位点(`kafka_offset_reset.ps1 -RetireGroup … -Partitions <N>`);Step 6 显式传 `-DbConsumerGroup zone`,`k8s_deploy.ps1` 渲染最终形状
> `GroupID: "db_rpc_consumer_group_z<id>"`,不带 `GroupHandoffFrom`。回档之后该 zone 一律以 zone 组为权威。组名基名与分区数现读 `go/db/etc/db.yaml`
> (`ServerConfig.Kafka.GroupID`、`ServerConfig.Kafka.PartitionCnt`)。细节、剩余风险与未实测的 Kafka CLI 行为见
> [zone_data_rollback.md](zone_data_rollback.md) 的「2026-10-15 修订」与 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md)。

> **2026-10-17 更正(已落码,未编译、未测试,待 Codex 验证)**:R7 复核之后的脚本修复。细节以 [zone_data_rollback.md](zone_data_rollback.md) 的「2026-10-17 修订」
> 与 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md) §5、§6 为准。
> - **第 0 步 e**(`Assert-Step6KafkaFactsReadable`,只在 `-Apply` 下、第 0 步 d 之后、Step 1 之前):按 Step 6 的同一条路径读两组位点,即 `kubectl exec` 进 infra namespace 的
>   `kafka-0`(lib 的 `Get-DbGroupDescribeExecArgs` / `Resolve-DbGroupDescribeExecResult`,同一组 `-KubeContext` / `-KubeConfig`,infra namespace 取 `dev_tools.ps1` 参数块里
>   `-InfraNamespace` 的默认值)。exec 失败(缺 `pods/exec`、kafka-0 未就绪)或判不出,在停服之前拒绝;与本机 CLI(`-KafkaBootstrap`)对「本 topic 上有没有已提交位点」
>   的结论不一致,同样拒绝。原来「Kafka 位点直到 Step 6 才读」这条剩余风险因此已在停服前覆盖,只剩第 0 步 e 与 Step 6 之间(隔着 MySQL PITR)的窗口。
> - **topic 名**:按 `go/db/etc/db.yaml` 的 `ServerConfig.Kafka.TopicGeneration` 用 lib 的 `Get-DbTaskTopicName` 推导(世代 ≤1 为 `db_task_zone_<id>`,否则 `db_task_zone_<id>_g<gen>`);
>   显式给的 `-KafkaTopic` 与推导值不同,入口即拒。
> - **`kafka_offset_reset.ps1` 的参数约束**:`-DeleteAndRecreateTopic` 对 db_task topic 一律拒绝(改走 `TopicGeneration` 换代);对其它 topic,`-ReplicationFactor` 必填且须与现有一致,
>   删除前先 describe 核对(读不到或不一致不删),删除后等待超过 30s 即抛出、不重建。重置模式以 `exit 0` 结束,`Invoke-KafkaToolCaptured` 复位 `LASTEXITCODE`,
>   「组不存在」的 describe 以非 0 退出时不会再把成功的 Step 5 判成失败。
> - **回档语义(当时待负责人裁决;负责人已于 2026-10-17 裁决为 (A) 丢弃,落码见下方「2026-10-18 更正」,本条留作历史)**(只改了注释,行为未变):K8s 上 go/db 的游标(`consumer:applied:<topic>:<key>:<msgType>`、`consumer:applied_epoch:<topic>:<key>:<msgType>`)
>   在共享 Redis DB0,Step 4 的 FLUSHDB 防护必拒,Step 4 被跳过,游标不清。Step 5 拨回 zone 组之后,TargetTime 之后已落库的写被游标判 stale 吸收,
>   而 zone-down 时尚未消费的积压与 DB0 里本 topic 的重试收据照常落进 PITR 之后的 MySQL。所以「从 TargetTime 起重放」在 K8s 上不成立,「丢弃 TargetTime 之后的写」也不完整。
>   两个互斥方案:(A) 丢弃:Step 5 改 `-ToLatest`,在 Step 4 / 5 之间盘点并拦截本 topic 的重试收据;(B) 灾难恢复重放:Step 5 之前只清本 topic 的游标
>   (SCAN `consumer:applied:<topic>:*` 与 `consumer:applied_epoch:<topic>:*` 逐页 UNLINK),并在第 0 步校验 TargetTime 不早于各分区日志起点(现在没有这条校验)。
>   各自的代价见 zone_data_rollback.md。

> **2026-10-18 更正(回档语义已裁决;已落码,未编译、未测试,待 Codex 验证)**:负责人 2026-10-17 裁决「丢弃 TargetTime 之后的写」:回档就是回到 T 那一刻
> MySQL 里已经落库的状态。完整流程、各状态族的处理、代价、残余风险与待拍板事项以 [zone_data_rollback.md](zone_data_rollback.md) 的「2026-10-18 修订」为准,要点:
> - **代价(已接受)**:T 之后的合法存盘一并丢弃,包括 T 之前产生、T 那一刻还在 Kafka 里没落库的存盘;跨服务状态照旧不回退。
> - **Kafka**:Step 5 把 zone 组拨到日志末端(`kafka_offset_reset.ps1 -ToLatest`,不再 `--to-datetime`),逐分区复核后退役 legacy 组在本 topic 上的位点,记下切点 `kafka-cut.json`;
>   go/db 的游标与 owner_epoch 保留(拨到末端之后新写的 seq 恒大于旧游标)。以后对 db_task topic 的 `-ToDatetime` / `-ToEarliest` 不得越过切点。
> - **Redis**:不再 FLUSHDB(共享 DB0)。读 MySQL、写 Redis 全部交给回档工具 `go/db/cmd/zone_rollback_discard`(**尚未落地**,`-Apply` 在它落地之前停在入口校验):
>   Step 4b 把本 topic 的重试收据与死信改名进隔离区(带轮次),按 PITR 之后的库删除受影响玩家的父快照、子表缓存、sync_digest、资产证据与屏障;映射、游标、账号目录不碰。
> - **流程**:入口校验 → 第 0 步 a–f(入口现状与 `entry-state.json`、静态预检、LAG 快照、Step 5 预演、Step 6 的 Kafka 读法、部署事实 + Redis run_id + 工具构建提交 + 工具 preflight)
>   → Step 1 停服 → 1b(Kafka 消费者、两次日志末端、工具 stopped 写停服行指纹基线)→ Step 3 人工 PITR → 4a 盘点(与停服基线比较)+ Step 5 之前复核停服状态
>   → Step 5 → 4b 丢弃 → 5b 复核(切点 + 工具 verify)→ Step 6 `-DbConsumerGroup zone` → Step 7 核对。Step 5 之前的中止 / 放弃路径是**不带** `-DbConsumerGroup` 的 zone-up。
> - **待负责人另行拍板**:#1 zone 维护围栏(落地前工具 preflight 一律拒绝)、#3 严格屏障协议、#4 blocked 玩家、#6 T 之后合入本 zone 的玩家;脚本对这些路径一律 fail-closed。

### 关键安全点

| 要点 | 说明 |
|------|------|
| Redis 可安全清空 | 它是缓存层，节点启动后从 MySQL 重建 |
| Kafka 必须处理 | 不清空 topic/offset 会导致旧消息重放覆盖回档数据 |
| MySQL 必须先恢复 | 它是唯一的持久化真相源（source of truth） |
| 串行保证 | Kafka partition key = `player_id`，同玩家写入串行执行 |

> **2026-10-17 更正**：「Kafka 必须处理」一行的说法在现行管线上不完整。拨回位点之后哪些写会重放，取决于 go/db 按 topic 的已落库游标还在不在（K8s 上它在共享 Redis DB0、回档不清），见上方「2026-10-17 更正」。
>
> **2026-10-18 更正**：语义已裁决为「丢弃 TargetTime 之后的写」，上表前两行都不再成立：Redis 不能整库清空（改由回档工具只丢弃本 zone 在 T 之后的状态），Kafka 不清 topic、不拨回 TargetTime，而是拨到日志末端并保留游标，见上方「2026-10-18 更正」。

---

## 3. 当前缺失基建

| 缺失项 | 说明 | 优先级 |
|--------|------|--------|
| MySQL 定时备份 + binlog 归档 | K8s MySQL pod 未配置自动备份，需加 CronJob 或用云 RDS | **高** |
| Redis RDB/AOF 定期快照归档 | 当前 `--appendonly yes` 保证持久化，但无历史时间点恢复能力 | 低（可 FLUSHDB） |
| Kafka offset 回档工具 | 需脚本将 consumer group offset 重置到指定时间戳（已落地：2026-05-15 起为 `tools/scripts/kafka_offset_reset.ps1`，`dev_tools.ps1 -Command kafka-offset-reset`，以 [zone_data_rollback.md](zone_data_rollback.md) 的同名表为准） | ~~中~~ |
| 一键 zone 回档脚本 | 将上述步骤封装为 `dev_tools.ps1 -Command k8s-zone-rollback`（已落地：2026-05-15 起为 `tools/scripts/k8s_zone_rollback.ps1`，以 [zone_data_rollback.md](zone_data_rollback.md) 的同名表为准；2026-10-15 的 Kafka 步骤修订见上方更正，未编译、未测试，待 Codex 验证；2026-10-18 回档语义已裁决，流程与回档工具见上方「2026-10-18 更正」，工具落地前 `-Apply` 不可执行） | ~~中~~ |

---

## 4. 生产环境建议

- **云 MySQL（RDS）**：自带自动备份和 PITR，回档操作在控制台点几下即可，**强烈推荐**
- **自建 MySQL**：必须配置 `log-bin`、定期 `mysqldump` CronJob、binlog 归档到对象存储
- **Redis**：zone 回档时直接 FLUSHDB，无需额外备份
  - 2026-10-18 更正：不成立。Redis 是共享库，回档不做 FLUSHDB，见上方「2026-10-18 更正」。
- **Kafka**：回档后最安全的做法是清空 `db_task_topic`，避免旧消息重放
  - 2026-10-17 更正：现行管线不能清空或同名重建 db_task topic（go/db 的已落库游标按 topic 名区分、永不过期，同名重建后的新存盘会被判陈旧丢弃；`kafka_offset_reset.ps1 -DeleteAndRecreateTopic` 已拒绝），回档只拨位点；拨位点之后哪些写会重放，见上方「2026-10-17 更正」。
  - 2026-10-18 更正：回档把 zone 组拨到日志末端（`-ToLatest`）、保留游标，见上方「2026-10-18 更正」。
