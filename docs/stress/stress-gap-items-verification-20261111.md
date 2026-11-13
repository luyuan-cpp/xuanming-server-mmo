# 缺口清单压测项的复核方案(2026-11-11,未运行)

状态:**方案已写,未运行**。交 Codex 按 AGENTS.md §6 执行;Claude 不跑压测、不宣称任何性能结论。

## 0. 这份方案管什么

[缺口清单](../handoff/server-gap-audit-20261101.md) 里第 3、14、15、24、30、33 条原写「等压测证据再定」。负责人 2026-11-11 改口径
「不用等压测数据了,先写完」,这几条已按标准做法定案(第 17、18、20、25 条归另一个会话)。本方案不再是定案的前提,
而是**复核定案依据**:每条写明定案靠的是哪个估计、压测里看哪个数、数不对时怎么办。

| 条目 | 11-11 定案 | 依据里的估计值 | 设计出处 |
|---|---|---|---|
| 3 | 审计 topic 每分区 retention.bytes:prod / prod-like 4GiB,目标扛住 2 小时消费中断 | 一个 45k zone 峰时单分区进水 ≤ 0.5MB/s(快照 15KB) | `deploy/k8s/README.md`「审计 topic 的 retention.bytes」 |
| 14 | 名次语义不改;服务端按名次拉长 `RetryAfterMs`(前 2000 名 2s,之后与名次成正比,上限 min(30s, AdmitTTL/2)) | 10 万人排队时轮询约降到固定 2s 的八分之一;客户端遵守 `retry_after_ms` | `login-queue-2026-05.md` §14 第 10 条 |
| 15 | `snapshot_guid` 不分桶(本来不单调);不做按天分区 | 清理按桶随副本线性扩,能追上写入 | `audit-pipeline-horizontal-scale-20261106.md` §2.C、§2.D |
| 24 | 定了拆法方向(按首选机房拆本机房阶段,单机房再分 K 个子池);11-12 已落码(未编译,「缺口第 24 条:热池按机房与子池拆写者」会话) | 单写者在热池上会先到顶 | `global-match-hierarchical-pools.md` §5.2、§5.2.1 |
| 30 | 命令 topic 不换代;场景亲和不做 | 每节点命令解码量 ≈ 总速率 ÷ 256;每次广播至多 G_z 份 | `connection-fanout-subsetting-20261106.md` §1、§3.5、§3.6 |
| 33 | 住户预算保留 200 名 / 10 秒 / 节点 | 与 R18 每个 scene 节点约 17.5 次进场/秒同量级 | `scene-node-ownership-reconcile.md` §4.7 |

## 1. 纪律(AGENTS.md §6,任何一步漏了都重来)

1. 跑测前把上一轮 `stress_summarize.ps1` 的输出存成 `prev-summary.txt`(放在本文同目录,文件名带轮次),作为对比基线;没有基线不开跑。
   第 10 条的现状基线(G1~G3、PL1 表)至今没跑过,本轮第一次跑就是基线,复核结论只能写「本轮实测值」,不能写「提升」。
2. 跑测前清空:
   - `robot/logs/stress-*` 只留最近一个;`bin/log/*`;`go-svc-stop` 后删 `tools/scripts/.run/` 的 pid / log;
   - 全部 Redis:`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`(单实例与玩家域开发集群的每个主节点;Cluster 的 FLUSHALL 只清本节点);
   - 全部 MySQL / TiDB 与 etcd 数据(§6.2 第 6 条:每次登录压测前全部删除);
   - Kafka:`pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`;
   - 新建 `robot/logs/stress-<name>-<ts>/prom-snapshots/`。
   - C++ 节点要开指标出口(第 30、33 条看 N1 表):起节点前设 `$env:MMORPG_METRICS_BASE_PORT = '19400'`(经 `dev_tools.ps1 -Command cpp-node-start` /
     `dev-start` 起也认),或直接 `pwsh -File tools/scripts/cpp_nodes.ps1 -Command start ... -MetricsBasePort 19400`。每个实例按
     `tools/scripts/lib/cpp_metrics_ports.ps1` 的规则拿一个端口(gate 19401…、scene 19501…、battle 19601…,`-Zone N` 再加 1000 × N),启动输出逐个打印。
3. 压测中至少在 ramp 完成、稳态中段、稳态末三个时刻拉快照:
   `pwsh tools/scripts/stress_snap.ps1 -RunDir robot/logs/stress-<name>-<ts> -StartTime '<yyyy-MM-dd HH:mm:ss>' -Stages 0,2,5,10,15,18 -RedisTargets @{ core = 'docker:redis'; queue = '<排队实例>' } -CppMetricsBasePort 19400`
   (Redis 求差要基线,`-Stages` 带上 0;排队开了独立实例时把它加进 `-RedisTargets`;`-CppMetricsBasePort` 与起节点时的基准相同,
   按 `run/pids/cpp_nodes.pid.json` 的实例键逐个抓,存成 `t<N>m_cpp_<实例键>.txt`;K8s 上 port-forward 之后改用 `-CppEndpoints @{ scene_1 = 'http://127.0.0.1:<端口>/metrics' }`)。
4. 跑完 `pwsh tools/scripts/stress_summarize.ps1 -RunDir <RunDir>`,与 `prev-summary.txt` 二维对比写进复盘文档 `docs/stress/stress-<round>-<date>.md`。
5. 压测期间不上传任何日志。没有对比表不写「性能提升」。

## 2. 场景与规模档

| 档 | 内容 | 机器人配置 | 复核哪几条 |
|---|---|---|---|
| A 稳态 | 1 zone、45k 机器人、18 分钟,同 R18 口径(4 个 scene) | `robot/etc/robot.stress-3zone-z1.yaml`(profile=stress) | 3、15、30、33 的基线 |
| B 排队 | 同 A,但把 zone 容量调到 5000、开排队,让约 4 万人进队;排队阶段至少 10 分钟 | 同 A;login `Queue.Enabled=true` | 14 |
| C 匹配 | `match-stress` 档,按 robot 配置样例放大到单池热点 | `robot/etc/robot.match-stress-100.yaml` 放大 | 24 |
| D 控制域清空演练 | A 的稳态中段,清空本 zone 的控制域 Redis(只清 ctl 实例或本机控制域,不动玩家域),观察 10 分钟 | 同 A | 33 |
| E 审计消费中断 | A 的稳态中段停 data_service 10 分钟再起 | 同 A | 3 |

D、E 是故障演练,各单独一轮,不和别的档混跑;跑之前在复盘文档写明起止时刻。

## 3. 逐条复核口径

每条:看什么数 → 定案成立的判据 → 不成立时的动作。「汇总表」一列写现有 `stress_summarize.ps1` 能不能直接出;不能的见 §4。

### 第 3 条 审计 topic 的 retention.bytes

- 看:data_service(:9260,`t<N>m_ds.txt`)的 `data_service_kafka_consumer_message_bytes`(快照平均字节)、两条消费组的消息条数增量;
  E 档里 `data_service_kafka_consumer_lag_max_partition_messages` 的增长斜率与 `data_service_kafka_consumer_retention_gap_messages`。
- 算:单分区进水 = 稳态进水字节/s ÷ 分区数(快照 3、流水 6);T_safe = 4GiB ÷ 单分区进水。
- 判据:45k 一个 zone 时快照、流水单分区进水都 ≤ 0.5MB/s(T_safe ≥ 2 小时);E 档没有 `retention_gap`;`DataServiceAuditLagHigh`(> 4000 条)在停机约几分钟内响。
- 不成立:快照平均远大于 15KB 时,先用 `bin/etc/game_config.yaml` 的 `audit_snapshot` 抽样或关一个触发点,再按实测放大 `-AuditKafkaRetentionBytes`,
  同批改 `DataServiceAuditLagHigh` 阈值(单分区删除线的 30%)与 README 的推导;流水超了就按 zone 数加流水分区。
- 汇总表:A1(2026-11-12 起;消息条数、每条字节数、单分区最大积压、`gap` 每条流一行)。Kafka 段另在跑完时 `docker exec` 读消费组积压。

### 第 14 条 排队轮询间隔

- 看:B 档排队实例的 R1 / R2 表(ops/s、`EVALSHA` calls/s、main%);login(:9101)的 `login_queue_size`、`login_queue_admit_total`、`login_queue_expired_total{reason}`;
  robot 的放行到进 gate 的耗时。
- 判据:排队约 4 万人时,排队实例的脚本调用约为「前 2000 名 × 0.5 次/s + 其余按名次递减」,明显低于「人数 ÷ 2」;
  `expired_total{reason="admit_window_passed"}`(放行了没来取)与改动前同量级,即拉长间隔没让人错过放行槽。
- 不成立:错过放行槽的变多 → 调低 `MaxRetryAfterMs` 或调大 `AdmitTTL`;脚本调用没降 → 核对客户端 / robot 是否按 `retry_after_ms` 轮询(robot 契约是按它,≤0 时 2s)。
- 汇总表:R1 / R2(排队实例的脚本调用)与 Q1(2026-11-12 起;`login_queue_*`,`adm_win` 一列就是放行了没人来取)。

### 第 15 条 审计表热点与清理

- 看:TiDB Dashboard 的 KeyViz 与 Top SQL(只本机 TiDB 有),`player_snapshot` 的 `uk_snapshot_guid_nz`、`snapshot_guid` 两条索引;
  `data_service_audit_retention_deleted_rows_total` 增量与快照、流水的写入行数增量;`data_service_audit_retention_failures_total`。
- 判据:两条 guid 索引的写入散在多个 Region,没有单一热点;清理期把保留期临时调短(例如快照 10 分钟)时,删除行数/s 跟得上写入行数/s,失败为 0。
- 不成立:guid 索引出现尾部热点 → 说明号源不是打散的,先查 `snapshot_system.cpp` 的号段配置;清理跟不上 → 先加 data_service 副本,再调批量,仍不够再按设计稿 §2.D 换按天分区。
- 汇总表:清理看 A1 的 `del` / `del_err`(2026-11-12 起),写入行数看同表 `ins`;索引热点只能看 TiDB Dashboard,没有汇总表。

### 第 24 条 热池

- 看:C 档 G1 表的 `rd/int`(轮耗时 p99 ÷ 轮间隔)、`conflict%`、队列深度;G3 表最热模式。
- 判据:这条只复核「单写者会先到顶」这个前提。最热池 `rd/int` 持续 > 0.8 且冲突 < 5% 就说明单写者已到顶(设计 §5.6 第 1 条)。
- 11-12 拆分已落码(设计 §5.2.1,未编译):G1 的 owned 列变成本副本负责的撮合段数(`match_pool_owned{role}` 之和);跨机房段与机房段争同一批老票的冲突看 G4 表(按撮合段角色,`match_pool_owned{role}` 与 `match_pool_segment_claim_total{role,outcome}`,11-12 已加)的 cross 行。拆分前后各跑一轮同口径的 C 档,才能说吞吐有没有涨。
- 汇总表:G1~G4 有。

### 第 30 条 命令 topic 与广播扇出

- 看:C++ gate、scene 节点 `/metrics` 的 `node_command_consumed_total{node_type,topic,target}`(own / foreign)与 `scene_broadcast_gate_fanout{kind}`。
- 判据:单节点 foreign 每秒远低于 50 条(告警阈值);广播扇出 p99 不超过本 zone 的 gate 数,gate 入站 CPU 没有因广播吃满。
- 不成立:foreign 持续超阈值 → 查是哪个分区被打偏(node_id 分配不均),必要时按 `node_command_topic.h` 的规则换代号、换更大的 P;广播扇出成瓶颈 → 记录数据,随万人同屏重做,不在本轮加亲和。
- 汇总表:N1(2026-11-12 起;`cmd_own` / `cmd_for` / `for/own` 与 `fan_*`)。要按 §1 第 2、3 步给 C++ 节点开出口、给 `stress_snap.ps1` 传 `-CppMetricsBasePort`。

### 第 33 条 住户预算

- 看:D 档每个 scene 节点的 `scene_ownership_releases_total{stage="drain_started"}` 增速、`scene_ownership_release_deferred_scenes`;
  同时段 S1 表(scene_manager EnterScene 子阶段)、P1 / P2 表(go/db 存盘与认领)的 p99。
- 判据:每节点开始改派约 20 名/秒;EnterScene 与 go/db 的 p99 不超过稳态的 2 倍;等预算的场景 15 分钟内清零(`SceneOwnershipReleaseBacklog` 不持续响)。
- 不成立:下游 p99 超过 2 倍 → 调小 `kMaxRelocationsPerSweep`;积压不清 → 调大;两者同时出现 → 下游要扩副本,预算不动。
- 汇总表:S1、P1、P2 有;`scene_ownership_*` 在 N1 的 `rel` / `dest` 与 `track` / `relg` / `defer` / `super` 四列(2026-11-12 起,开出口同上)。

## 4. 汇总脚本的覆盖(2026-11-12 已补,未运行)

11-11 先定案时没改脚本,11-12 补齐。都是已有的低基数指标,没有改任何服务代码;脚本、表与钉名测试都没跑过,交 Codex:

1. **C++ 节点抓取**:`tools/scripts/lib/cpp_metrics_ports.ps1` 定端口规则(基准 + 1000 × zone + 节点偏移 gate 0 / scene 100 / battle 200 + 序号),
   `cpp_nodes.ps1 -MetricsBasePort`(或环境变量 `MMORPG_METRICS_BASE_PORT`)按它给每个实例设 `MMORPG_METRICS_PORT`,
   `stress_snap.ps1 -CppMetricsBasePort` 按 PID 文件的实例键推出同样的地址,`-CppEndpoints` 可手传或覆盖;文件是 `t<N>m_cpp_<实例键>.txt`。
   `stress_summarize.ps1` 按这个形状认 C++ 快照,不再把它们当成 legacy 的 login 快照。
2. **A1**(`*_ds`):每个快照 × 审计流(transaction_log / player_snapshot)一行:消息条数(全部 / inserted / duplicate / 坏结局)、
   每条字节数 n / 均值 / p99、单分区最大积压、`gap`、同名表的清理删除行数与失败数。
3. **Q1**(login 快照):跨 zone 求和的 `size` / `inflight` / `cap`、`admit`、`expired_total` 每个 reason、等待 n / p50 / p95。
4. **N1**(`*_cpp_*`):每个快照 × 实例一行:命令 topic(`-cmd` 结尾)的 own / foreign 与倍数、按收件人广播的扇出 n / p50 / p99、
   场景归属对账的开始排空 / 已销毁与四个 gauge。
5. **钉名**:Go 用例 `TestLoginReadNamesMatchStressSummarize`(加了 `$dsKafkaOutcomes`)、`TestAuditStreamNamesMatchStressSummarize`、
   `TestExpireReasonsMatchStressSummarize`;C++ 一侧没有 Go 那样的常量可比,由 `tools/scripts/tests/stress_cpp_metrics.tests.ps1` 按源码文本对 label 取值与族名,
   同一个测试还跑端口规则、抓取地址推导,并用一组最小快照跑一遍汇总脚本核对三张表的行。

## 5. 结果回填

跑完由 Claude 读 summarize 输出逐条核对 §3 的判据,成立的在缺口清单对应行补「压测复核通过」,不成立的按「不成立」一栏改码或改参数,
两种都在 `docs/PROGRESS.md` 追加一条。
