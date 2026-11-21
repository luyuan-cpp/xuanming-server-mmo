# 系统不变量（数据一致性与安全）

> 对应根索引 [AGENTS.md §7](../../AGENTS.md#7-不变量数据一致性--安全)。跨服务必须保持的不变量，违反 → PR review 直接拒。

## 1. 永久 ID 与 SnowFlake 隔离

每种 SnowFlake ID 只能由一种 kind（一种服务 / node 类型）生产；不同 kind 各有一套槽池，worker id 会重叠，两种服务共用一种 SnowFlake ID 必然撞号。

全系统只有两种互不兼容的 ID，**严禁混用解析函数**（login 原先的 `bwmarrin/snowflake` 毫秒布局 2026-11-12 已随 login、guild 槽位申领代码一起删除，本地库若残留旧 `player_id` 需清库重建）：

1. **`shared/snowflake` 与 C++ `SnowFlake`**（17-bit worker / 15-bit step / 秒级 epoch `1773446400`）：
   - Go 侧申领槽位的只剩 `scene_manager` 的 `scene_id`，worker id 来自 `shared/snowflakealloc` 按 kind 申领的槽位（`[cluster5][slot12]`），与路由 `node_id` 无关。
   - 仍由这套号源生成的 `scene_id` 等值可用 C++ `ParseSnowflakeGuid` 解，不能仅凭字段名选择解析器。
   - 路由 `node_id` 按 `node_type` 分配（同一类型跨 zone 不重复，不同类型可以同值），只用于服务发现与命令分区，不喂给任何发号器，也不作任何 ID 唯一性来源。
2. **发号器发的永久 ID**（`player_id`、`guild_id`、物品 `guid`、流水号 `tx_id`、`snapshot_id`、`battle_id`、`challenge_id`、`team_id` 等，`data_service.AllocateIdSegment`）：
   - 打散过的计数器值，63 位以内，不单调、不连续、没有位布局。
   - **不能用 `ParseSnowflakeGuid` 解析，不能靠大小判断先后**（排序和翻页用时间列），Lua 里只当字符串，不进有序集合的 score。没有回退号源，发号失败就是操作失败。要新增全局唯一 ID，只在发号器加 `biz_tag`，不再造 SnowFlake。
   - 详见 [发号器设计](../design/player-data-tidb-scaling-plan.md) §6 与 [发号器运维手册](../ops/id-issuer-runbook.md)。
   - **强规定（负责人 2026-10-31 定）**：发号器的预留记录必须写两套独立的 etcd，两份都写成功才发号。任何生产环境、任何预算档都不许省掉第二套，也不许把两份放在同一套；只有 `Mode=dev`、`test` 例外。
   - 2026-11-05 起另有第三份记录在全世界共用的全局号源登记处，三份都写成功才发号；它同时把每个地域码唯一绑定到一个 K8s 集群（运行期读 `kube-system` 的 UID）里的一对本地 etcd。登记处只走双向 TLS（世界 CA 签的证书），地域码只能按顺序认领；换码只换发号码（`-IdIssuerRegion`），存档库编号 `-Region` 不变。登记处不许从备份恢复，首次初始化和事故恢复是两个入口。

## 2. Kafka 防僵尸与顺序键

- **防僵尸**：节点命令使用共享分区 topic（`gate-cmd_g<N>`、`scene-cmd_g<N>`，按目标 `node_id` 定分区；`{type}-{node_id}` 仅是迁移期兼容路径，由 `disable_legacy_per_node_topic` 控制；Centre 已退役）。每条定向节点命令必须填目标节点的 `target_instance_id`（目标节点 UUID），空值会关闭过滤；共享分区也不得省略此字段。发送方通过共享寻址工具选 topic 与分区，不手拼旧 topic 名。
- **顺序键**：`kafka topic key = 业务实体 ID`（同一玩家 / 同一对局事件有序）。
- topic 保留、分区数、换代和节点号复用约束见 [control-plane-topic-partitioning-20260908.md](../design/control-plane-topic-partitioning-20260908.md) 与 [kafka-topic-retention-strategy.md](../design/kafka-topic-retention-strategy.md)。

## 3. proto 字段编号

- proto 字段编号与配置表 schema 字段编号上线后**不复用**（见 [proto-and-tables.md](proto-and-tables.md)）。

## 4. tip 错误码单轴

- **tip 错误码只能由导表器发号，不许手写数字**。`TipInfoMessage.id` 是客户端可见契约（客户端按 id 查文案），全仓一条数轴。
- 段由 `data/tip/Tip.xlsx` 的组头行声明（如 `//guild_error base=14000 width=1000`），发号器按段分配并在生成期自检（段不重叠 / 码不越段 / 枚举名全局唯一）。
- 历史教训：`go/match` 曾手写私有段从 1 重数，20 个码全部压在 common / login 段上，客户端弹出无关文案且全程零报错。guild / friend / match 的 `constants_test.go` 里有 `TestNoHandWrittenTipCodes` 机械守住这条。
- 「哪个码算服务端故障」也是表的一列（`fault`），生成到 `tip.Faults` 供 `serverbase.TipVerdict` 消费，服务里不许再手写故障码集合。详见 [tip-code-axis.md](../design/tip-code-axis.md)。

## 5. ECS 组件访问

- 修改或评审组件创建、读取、存盘与快照前，先按 [ecs-component-access-rules.md](../design/ecs-component-access-rules.md) 区分持久数据、按需状态和派生运行时数据；创建职责与缺失语义以该文档为准，读路径只读，跨实体查询用 `try_get`。

## 6. 逻辑库自治（负责人 2026-10-31 定）

- 规格见 [horizontal-scale-logical-db.md](../design/horizontal-scale-logical-db.md)：一个逻辑库只归一个服务，服务之间**不许跨库 SQL**，只走 RPC（`CrossDatabase` 契约测试守着）。
- 每个逻辑库自带一份写者闸门；服务提交点锁本库 `role_fence`，不碰目录；事务里不调 RPC。
- 目录分片数 `Directory.LogicalShards` 上线后不能改，扩容靠把逻辑库搬到新集群（`tools/dbmove`）。

## 7. 数据一致性底线

- 保证修改代码的正确性与数据一致性，不留静默数据损坏窗口。
