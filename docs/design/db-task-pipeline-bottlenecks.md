# 玩家存盘管线(go/db)的瓶颈、不停服转移与修复计划

> 状态:分析完成,修复分批进行。第 0 批的小修已落码(§7),未编译,待 Codex 验证;其余各批待负责人拍板(§5.3)。
> 日期:2026-10-12
> 范围:玩家存盘管线 C++ scene / login → Kafka `db_task_zone_*` → go/db → MySQL / Redis,以及 guild 的扩展形态。
> 来源:四轮"多代理调研 + 对抗复核",全部是只读分析与联网核实,**没有编译、测试或压测**(AGENTS.md §10.1)。
> 证据口径:[C] 读代码确认,[D] 文档记载,[M] 压测实测,[I] 推断。
> 行号:按 2026-10-12 工作区。`key_ordered_consumer.go`(下称 KOC)在 1306 行之后比 HEAD 73b852f32 多 21 行(§7 的 subShardIndex)。

---

## 0. 结论速览

1. **不引入 actor 框架。** ergo 与 protoactor-go 都不引入(§1)。本项目 actor 能替代的只有"进程内同 key 串行",这部分已有现成机制。
2. **效率瓶颈在 IO,不在派发。** 换哪个框架都测不出差别(§2)。
3. **水平扩展上,原来的 Kafka 消费组方案对按 key 分片的有状态写反而最稳**,但有 6 处真实短板(§3.2)。
4. **go/db 现在可以不停服滚动发版,但有一条缝会让玩家静默回档。** 原因是 MySQL 写是无条件整行覆盖,存储侧没有栅栏(§4)。根治靠 `(owner_epoch, save_seq)` 条件写。
5. 修复分 6 批,先补正确性、再补扩展。超过 30 个文件的批次需要授权(§5)。
6. **写路径 Redis 往返能从 11 次降到 2 次,但现在不改。** go/db 分段耗时还没实测、R19 lag 为 0;先补测量,正确性先做条件写(§6)。

---

## 1. actor 框架评估:ergo vs protoactor-go

### 1.1 对比(2026-10-12 联网核实,每条经独立复核)

| 维度 | ergo(`ergo.services/ergo`) | protoactor-go |
|---|---|---|
| 发版 | v3.0.0 2024-09-04 → v3.3.0 2026-09-04;v331 分支提交到 2026-10-09 | 最后 release 0.4.0(2024-04-03);dev 最后提交 2026-01-18,之后约 9 个月零提交 |
| 版本号 | v3.x 的 Go 模块 tag 是 `v1.999.330`,没有 `/v3`。v3.3.0 有破坏性改动,semver 上只是 patch 位,`go get -u=patch` 会直接拉进来 | 没有合法 semver,proxy 的 `@v/list` 为空,只能固定伪版本;README 自称 beta |
| 安全与缺陷 | 修复攒到大版本才发,#258 拖约 5 个月 | grpc v1.77.0 受 CVE-2026-33186 影响,修复 PR #1303 未合并;#1133(etcd 断线后不能重新入群)自 2024-12 未修 |
| 许可证 | 核心与扩展全部 MIT(Erlang 协议栈 2025-09、Saturn 2026-09 由 BSL 改 MIT) | Apache-2.0 |
| 集群里同 key 单持有者 | 不保证。Grid 文档原话 "AP"、"not a distributed lock" | 有 grain,但身份查找只剩 disthash,恰好是成员恢复有缺陷的那块 |
| 传输 | 私有 ENP/EDF,收发两端都要注册类型,不走 protobuf/gRPC | gRPC + protobuf;Remote 默认明文、不鉴权 |
| 维护人力 | 实际约 1 人(halturin) | 实际约 1 人(rogeralsing) |
| star | 4676 | 5515 |

### 1.2 决策:不引入

- 只看框架本身,ergo 更健康;protoactor-go 功能更对口,但已停更。
- 放到本项目两个都不引入:
  - actor 能替代的"进程内同 key 串行"已经由 go/db 的 SubShard + claimAcker、guild 的 MySQL 行锁、C++ scene 的单线程承担。
  - 真正的缺口是跨进程的存储侧 fencing(§4),两个框架都不提供。
  - 会与 Kafka 消费组的分区独占形成两套归属;C++ 节点接不进来;Java 版要另写一套(AGENTS §12)。
  - ergo 的 EDF 类型会与 proto 形成两份契约,违反 AGENTS §3。
- 以后重新评估的前提:§5 的条件写已落地;有 guild 压测证明行锁是瓶颈;资产路径仍保持同步、fail-closed。
- 已同步到 [guild-actor-architecture.md](guild-actor-architecture.md) §5.3、§16 #2。

---

## 2. 效率:瓶颈在 IO,不在派发

| 证据 | 数值 | 口径 |
|---|---|---|
| R19 第 18 分钟,login 侧派发 | 0.0ms | [M] [prev-summary.txt:30](../stress/prev-summary.txt) |
| 同时刻 kafka_send / 等 db 回调 | 4.1ms / 31.8ms | [M] 同上 |
| go/db 一条写任务的外部往返 | 约 11 次 Redis + 1 次 MySQL upsert(OwnerEpoch=0 时 9 次 Redis) | [C] KOC handleTask / runDBOp |
| 框架派发开销 | 约 0.5µs/跳,10 跳约 5µs,占 31.8ms 的约 0.016% | [I],框架数字全是官方自测 |

- 历史瓶颈是**每分区单 worker 串行 + MySQL 时延**,已靠 SubShard 与调大连接池处理(R5→R7)。
- 真正能动的杠杆:保序并行度、同 key 写合并(脏存盘跳过 49–60%,[M] R18/R19)、MySQL 时延、写路径 Redis 往返数(§6)。
- **数据可信度:**
  - 最新压测是 R19(06-18)。之后加的排序锁(08-04)、applied_epoch(09-18)、落点 MGET(09-29)都没压测过,当前单任务成本没有实测。
  - `stress_summarize.ps1` 的 db 分段一直显示 "-",是脚本按错误的 label 顺序精确查找,已修(§7)。
  - 早期几轮的部分吞吐数字是估算,例如 R7 的 "~190/s" 没有推导过程,不作为结论依据。

---

## 3. 水平扩展

### 3.1 三方对比

| | ergo | protoactor-go | 原方案(Kafka 消费组) |
|---|---|---|---|
| 加机器 | 不按 key 放置,按权重选节点,新节点只分到新流量 | rendezvous 哈希自动放置,约 1/(N+1) 的 key 换主 | 消费组自动重分分区 |
| 减机器 / 宕机 | Grid 只登记不迁移;AP,同 key 可能短暂两个 owner | 旧 actor 直接 Poison 不交接;被判死的节点不自停,有双激活与僵尸风险 | 全组暂停,从已提交 offset 重放;排序锁 + 游标 + epoch 兜底 |
| 转移期间的正确性 | 业务自己做条件写 | 同左 | 同左(条件写尚未落地,见 §4) |
| 实测 | 无 | 无 | 单 zone 45k 压测 |

### 3.2 原方案的真实短板

| # | 短板 | 证据 |
|---|---|---|
| S1 | 并行上限写死:每 zone 10 分区 × 8 子 shard = 80 路;第 11 个实例不增吞吐,却会消费共享重试队列、与分区属主并发执行同 key 重试;加分区要离线换 TopicGeneration | [C] go/db/etc/db.yaml:19,22;KOC 1027-1031、1141-1225 |
| S2 | 再平衡是 eager 全组暂停,且 K8s 上所有 zone 共用 `db_rpc_consumer_group`,一个 zone 的 Pod 进出会让所有 zone 一起停 | [C] tools/scripts/k8s_deploy.ps1:3437 |
| S3 | 同一玩家的读与写落在不同分区:login 主生产者没设 Partitioner,一致性哈希环从未生效,sarama 默认 FNV-1a;C++ 用 librdkafka 默认 CRC32 | [C] go/login/internal/kafka/key_ordered_producer.go:58-71 |
| S4 | 子 shard 倾斜:直接 `Key % 8`,09-08 之前的 bwmarrin PlayerId 低位大多为 0,挤进 0 号子 shard | [C] KOC routeToSubShard,**已修**(§7) |
| S5 | guild 只有 MySQL 行锁 + 全局哨兵行;actor 化与 jump hash 多实例未落地;没有 K8s 清单,没有压测 | [C] go/guild;[D] guild-actor-architecture.md |
| S6 | 单库 MySQL 连接预算:2 个 zone 时 2·210+80=500,正好等于 max_connections;滚动发版 maxSurge=1 时临时超限 | [D] PROGRESS.md:7178-7181;mysql.yaml:74 |

---

## 4. 不停服滚动时的正确性(归属转移)

### 4.1 结论

- 可以按"每个 zone 一次只换一个 Pod、SIGTERM 优雅停机"的方式滚动 go/db。
- 能保证的:Kafka 侧不丢,重放不倒退 [C];在线玩家不受影响,因为在线数据以 Redis 父键为准,scene 存盘不等 MySQL 落库(player_lifecycle.cpp:3681-3741)[C]。
- **保证不了的:MySQL 不被旧数据覆盖。** 只有 Redis 排序锁租约这一层,MySQL 侧没有任何栅栏;03-28 定下的 MySQL 版本条件写一直没落地(PROGRESS.md:7151)[D]。

### 4.2 各保证层挡住什么

| 层 | 挡住什么 | 挡不住什么 | 证据 |
|---|---|---|---|
| Kafka 分区独占 | 同一分区同一时刻只有一个成员拉取 | 重试不受分区归属约束,新 Pod 入组前就执行重试;读写不在同一分区;共用消费组导致全组暂停 | KOC:942-944,1034-1047,1196-1218 |
| claimAcker | 只提交"从头连续完成"的那段 offset;失败的写先进重试队列才 ack,所以不丢 | ack 后唯一副本在单实例 Redis 列表里;进死信就没人消费 | KOC:211-240,2110-2124 |
| 排序锁(TTL 2 分钟,30 秒续租) | 租约有效期间同一玩家的写跨进程互斥 | 续租失败一次就放弃;已发出的 SQL 不取消(handler 不接收 ctx);只有写拿锁 | KOC:1843-1844,1909-1931,811,829 |
| 游标 `v2:分区:offset+1` | 重放和重复投递时丢弃过期写 | 只能同分区比较,跨分区判"不可比"进死信;键缺失放行;无条件 SET 可能倒退 | KOC:482-519 |
| applied_epoch / owner_epoch | 被替换的旧 scene 的迟到写 | 两个 go/db 实例处理的是同一个 epoch,挡不住;epoch=0 或缺键放行 | KOC:530-541,617-641 |
| 写后 verify | 发现失锁时不推进游标 | 发生在 SQL 与缓存回写**之后**,数据已写成旧版 | runDBOp(KOC:2035-2050)先于 handleTask 的 verify |
| 重试队列归属 | processing 按实例拆,租约 30 秒;SIGTERM 归还,崩溃后 30–40 秒被回收 | 同 key 可能被多个实例同时执行;3 次重试只豁免四类错误 | retry_ownership.go:41-46,259-293 |

合起来看:前两层保证"不丢",游标保证"重放不倒退",epoch 挡住被废黜的 scene;**"go/db 执行实例换人"这个维度只剩排序锁租约一层。**

### 4.3 四个场景

**场景 1:正常滚动(2 副本,maxSurge 1,maxUnavailable 0)**
1. 新 Pod 启动,异步入组;readiness 只探 gRPC,显示 Ready 时不一定已入组 [C](KOC:1034-1047;db.yaml:70-74)。
2. 旧 Pod preStop sleep 5 后收到 SIGTERM。`Stop()` 第一行是 `cancel()`,在途任务作废交给新属主重放,**不是排空** [C](KOC:2223-2225)。
3. 有积压时必然等满 10 秒排空上限;该实例名下分区停摆约 10–20 秒 [I]。
4. 全组再平衡,**所有 zone 的落库暂停数秒**;新属主从已提交 offset 重放,游标丢弃已落库的写 [C+I]。
5. 玩家感知:在线无感;落库延迟十几秒;冷缓存玩家登录可能失败一次(预加载只等 8 秒,entergamelogic.go:380)。

**场景 2:kill -9 / OOMKill**
- 不发 LeaveGroup、不释放锁、不还收据;约 10 秒后被判离组,幸存实例重放,消息不丢 [C+I]。
- 崩溃时在途写持有的锁要 90–120 秒才过期,这些玩家的落库推迟约 2 分钟,期间空转不耗重试次数 [C]。
- 缝隙:SQL 已执行、游标未写时崩溃,若同 key 更旧的写在重试队列里先执行,MySQL 与子表缓存短暂回退;父键缺失时 login 会用回退的子表缓存拼出父键,回档被固化 [I]。

**场景 3:假死(会回档的那条路)** —— A 是卡住的旧实例,B 是接管者;S1 旧快照,S2 新快照。
1. A 拿到锁、通过全部预检,在 SQL 发出前卡住(虚拟机暂停、节点挂起)[C](KOC:1562-1610)。
2. 约 10 秒后 B 接管;120 秒后 A 的锁过期,B 依次落库 S1、S2,此时数据正确 [C]。
3. A 恢复:SQL 不受 ctx 约束直接发出,**MySQL 整行回到 S1**,子表缓存也写回 S1 [C](KOC:811,829)。
4. A 发现失锁,任务重新入队后被游标判过期丢弃;MySQL 停在 S1,直到玩家下一次真实改动 [C]。
5. epoch 挡不住(同一个 epoch);`stale_owner_write_rejected` 不增长,只有一行 `Release lock failed` 日志 [C]。
- 触发门槛:进程卡住 90–120 秒以上且没被杀;或 Redis 抖动导致续租失败,同时 SQL 卡住 90 秒以上(MySQL 连接没设读写超时,可能排在 DDL 元数据锁后)。kubelet 正常时 liveness + 45 秒宽限期大约能兜住,主要风险是虚拟机暂停、节点挂起 [I]。

**场景 4:新旧版本并存与上游重启**
- go/db 有四个格式分代点:08-04(排序锁、v2 游标)、09-18(epoch 守卫)、09-29(落点选库)、10-08(按实例拆 processing,未编译)。与 08-04 之前的版本混跑没有跨进程互斥;与 09-29 之前的混跑可能把空白角色写回真实行 [C]。
- 新 MsgType 由 C++ 先上线时,旧 go/db 三次全失败,进死信 [C]。
- scene 重启:排空最多 15 秒,再等 20 秒入屏障,EnterScene 推进 epoch;旧节点迟到写由 Redis CAS 与 epoch 守卫挡住 [C]。
- gate 滚动会断全部会话(2026-04-04 实测,之后没复测)[D],不在本文范围。
- 没有"暂停存盘消费"的维护开关;killswitch 只拦 gRPC,且 etcd 失联时自动放开 [C]。

### 4.4 缺口清单

| # | 严重度 | 缺口 | 后果 | 修法 |
|---|---|---|---|---|
| G0 | 严重 | login 生产者在 Kafka 选主时自锁:持 `p.mu` 调 `markPartitionUnavailable` 再加同一把锁 | Kafka broker 重启/选主时 login 读档全部挂起,只能重启 login | **已修**(§7) |
| G1 | 严重 | MySQL 写无条件整行覆盖,存储侧没有栅栏 | 静默回档,不自愈,无告警 | 根治:`(owner_epoch, save_seq)` 条件写;治标:SQL 读写超时、ctx 随租约取消、缓存回写挪到 verify 之后、续租失败退避重试(治标已落码,未编译未测试,见 §7) |
| G2 | 严重 | 死信没有消费者;重试豁免太窄(1040、未知 MsgType、MySQL 重启都会耗尽 3 次) | 存盘不落库,父键一丢就是回档 | 修复项 ② 批 A;死信告警与重放工具 |
| G3 | 高 | 先判 seq 后判 epoch | 新主的写在重试队列时,老主迟到写先落库并推进游标,MySQL 停在老主状态 [I] | 按 `(epoch, seq)` 字典序比较,与 G1 同批 |
| G4 | 高 | 重试不绑分区属主;读不拿锁且无条件写子表缓存;读写不同分区 | 子表缓存被旧值覆盖(TTL 3600 秒) | 修复项 ①④;缓存改 CAS 写 |
| G5 | 中 | 游标/epoch 缺键放行;游标可能倒退;两张子表不原子 | 旧版本重新通过守卫 | Lua 取最大值单调推进(纯 SETNX 会让游标推不动) |
| G6 | 中(可用性) | 共用消费组;Stop 先 cancel 导致必等 10 秒;readiness 不反映入组 | 所有 zone 暂停;冷登录可能失败 | 修复项 ③;Stop 改成先撤拉取再等 worker;minReadySeconds(治标已落码,未编译未测试,见 §7;`minReadySeconds: 20` 已进 `deploy/k8s/manifests/go-svc/db.yaml`,随 `e76f6f747` 入库,下一次 db 滚动时生效) |
| G7 | 中 | 停机路径没有自动化测试;两副本改动未编译 | 结论只是静态推断 | Codex 按 no-single-node-horizontal-scaling-20261001.md §3.5 做双实例杀进程、kind 删 Pod 实测 |
| G8 | 中 | gate 滚动断会话;没有运行时版本握手 | 违背"玩家在线不受影响" | gate 单独设计;发版说明写最低可混跑版本 |
| G9 | 低 | go_services.ps1 用 `Stop-Process -Force` 停进程(旧版 `tools/scripts/go_services.ps1` 的 `Invoke-Stop`) | 本机停 db 等同崩溃 | 先发 Ctrl+C,超时再强杀(治标已落码,未编译未测试,见 §7) |

**条件写的两个要点 [I]:**
- `save_seq` 必须由 C++ 生产端按玩家单调发号,不能用分区 offset(游标按分区记、换代后 offset 重置、读写本不同分区)。
- 只带 `owner_epoch` 不够:僵尸实例与新属主用的是同一个 epoch。Java 版的 `WHERE owner_epoch=?`(PROGRESS.md:7169)对这种情况推断同样无效,待 Java 侧核实。
- 治标措施撤不回已进入 MySQL 的语句,不能当栅栏。

**G6 停机顺序:Stop 五步与 ③④ 隐式互等(2026-10-16 补,未编译、未测试,待 Codex 验证):**
- 五步(`KeyOrderedKafkaConsumer.Stop`,S4 落码):① 撤拉取(关 `pullStop`)→ ② 等消费循环退出:各 claim 把已派发的任务做完并标记(`drainClaimForShutdown`,每个 claim 最多 `shutdownDrainTimeout` 10s),sarama 释放会话时最后一次提交位点,本步上限 10s + `consumerShutdownGrace` 5s → ③ 离组(`consumer.Close`)→ ④ 取消 worker 并等退出 → ⑤ 归还重试收据、收掉交接闸。R2 合入后追加 ⑥ `enqueuer.close()`。原则是先提交、后离组。
- 缺口 [C]:sarama v1.43.1 的 `Close` 经 `leave()` 要拿 `consumerGroup.lock`;`Consume` 从入口持有这把锁,直到会话 `release` 返回;`release` 依次等全部 ConsumeClaim 返回 → Cleanup → `offsets.Close`(最后一次提交)→ 停心跳。修前 ConsumeClaim 的派发 select 没有撤拉取一路:worker 通道(容量 1000)满时,只能等 worker 腾出槽位,或者等 `w.ctx` 取消(也就是 `c.ctx`,第④步才取消)。停机时 claimCtx 又不随会话取消。MySQL 卡住时每条语句可能等满 30s 读超时,于是 ② 等满 15s 超时 → ③ 的 Close 等锁 → 会话等 claim → claim 等 ④,而 ④ 排在 ③ 之后。这不算严格死锁,但实际会一直拖到 45s SIGKILL,④⑤ 都不执行:重试收据没归还,要等租约过期才被回收。
- 修法有三处,都在 KOC。
  - 主修:派发 select 加撤拉取一路,用 `claimAcker.untrack` 只摘掉手里这条没派发出去的消息,然后走 `drainClaimForShutdown`。这条消息一定在 `order` 末尾:track 只在 claim 自己的 goroutine 里按 offset 递增追加,压缩保留尾部。不能用 `fail`,因为它会取消 claimCtx,把已派发在途任务的位点一起作废。被摘掉的这条和它之后的消息,由下一任属主从已提交位点重放。
  - 辅修:② 超时分支里先 `c.cancel()` 再走 ③,解开会话里只认 `c.ctx` 的等待:毒消息落 DLQ、`drainClaimForShutdown` 的 consumerDone 一路。这样仍是先提交、后离组,因为 leave 要等会话释放完才拿得到锁。写 SQL 的 ctx 来自排序锁租约,不受影响;SQL 之后的复核、发布、游标推进会失败延后,效果与 ④ 相同,只是提前了。正常路径(② 按时结束)不提前取消。
  - 补修(R1 语义复核):消息通道关闭分支的排空 select 补撤拉取一路,`session.Context().Done()` 与 `yieldDone` 两路先判撤拉取,再走原来的 `drainAfterStop`,写法与顶层 select 相同。sarama v1.43.1 的分区消费者遇到 `ErrOffsetOutOfRange` 会自行关闭消息通道,会话还在。修前这时停机,Stop ① 取消 consumeCtx 带出的会话取消被当成再平衡:`acker.stop` 作废已完成的位点,再等满 `claimDrainTimeout`。这既不符合「已派发的做完并标记」,也突破了「每个 claim 最多 `shutdownDrainTimeout`」。撤拉取之前已经按再平衡或失败收尾的 claim 仍走原路径,最多等 `claimDrainTimeout`。
- 与 R2(S1 修订稿 §1.6、§1.7、§3)的接缝 [I]:R2 的租约 acquire 是非阻塞登记,不在辅修要解开的等待之列。R2 把 `drainClaimForShutdown` 的 `acker.wait()` 换成 `claimDrained(acker, lease)` 时,必须保留 consumerDone 和 `shutdownDrainTimeout` 计时器两路:`lease.idle()` 与入队层的异步确认都不随 `c.ctx` 取消(入队层第⑥步才关),只能靠排空时限兜底。两路都没有时,③④ 互等会在 R2 里重新出现。
- 剩余风险 [I]:
  - sarama 自身的网络调用(提交、心跳、JoinGroup、LeaveGroup)慢时,Close 照样要等,只受它的 Net 超时(默认 30s)和 `terminationGracePeriodSeconds: 45` 兜底,不受我们控制;
  - ④ 要等在途写 SQL 的读超时;②③④ 累加可能超过 45s,超出部分由 SIGKILL 截断,没标记的位点由新属主重放。
- 另一处缺口(未修,建议单独立项):`routeToSubShard` 在 `subShardCount ≤ 1` 时对子 shard 通道做阻塞发送,没有 `w.ctx` 出口。④ 取消之后,如果子 shard 先退出而通道已满,路由会永久阻塞,`c.wg.Wait` 随之挂住。生产 `SubShardCount: 8`(go/db/etc/db.yaml)碰不到。
- 读方就绪闸(D1 第 3 批;2026-10-20 核对补记,未编译、未测试,待 Codex 验证):`Placement.RequireRouteReady=true` 时 ConsumeClaim 顶层 select 多一个等待点。
  - 闸只替换 select 里的通道值(`gatedMessages`):
    - 闸关时消息通道为 nil,另带「下一次刷新完成」一路;
    - 闸放行时消息通道照常,也带这一路:刷新把闸关上时立即醒来、停在闸前,空闲分区不会在闸关之后再漏取。忙分区在关闸那一刻,刷新通道与消息可能同时就绪,select 随机选一路,每个 claim 至多多取一条,由逐条判(`lookupReady`)兜住。
  - 它与 `pullStop`、会话 ctx、`yieldDone`、`failureCh` 同在一个 select。撤拉取照常转 `drainClaimForShutdown`;c.ctx 取消经 consumeCtx 传到会话 ctx。
  - 约束:
    - 闸不得挪进 Start 的入组循环:被闸住的成员不该离组,而且那样会和重新入组退避(R7)互相绕开;
    - 也不得写成 select 之外的阻塞等待:那样 Stop 会被闸卡住。
  - 重试循环的闸检查不阻塞。刷新 goroutine 随 c.ctx 退出,不在 `c.wg` 里,Stop 不等它。
  - 规格 `spec_D1_batch1b2_2_3.md` §13.2 第 1 条说的「消费循环」指 ConsumeClaim 的主循环,不是 Start 的入组循环。
- 回归测试在 `go/db/internal/kafka/consumer_shutdown_test.go`,替身 `fakeConsumerGroup` 已按 sarama 的锁语义改写:Consume 持锁直到会话释放,Close 先关 closed 再拿锁。这个文件里的闸全是 nil;开关打开的情形在 `route_ready_gate_shutdown_test.go`,覆盖 Stop、再平衡、交接让出、停机排空、退避、闸由开转关。
  - claimAcker:`TestClaimAcker_UntrackRemovesOnlyUndispatchedTail`、`TestClaimAcker_UntrackConcurrentWithAck`;
  - ConsumeClaim:`TestConsumeClaim_ShutdownWhileDispatchBlockedUntracks`、`…DispatchBlockedReturnsWithinDrainTimeout`、`…DrainStopsWhenWorkersCanceled`、`…DrainIgnoresHandoffYield`、`…WhileDrainingClosedMessagesMarks`、`…WhileDrainingClosedMessagesReturnsWithinDrainTimeout`(后两条用 `probedSession` / `probedClaim` 计 select 求值次数,确定地停在关闭分支的排空 select 上);
  - Stop:`TestStop_FullWorkerQueueDoesNotStallLeave`、`TestStop_DrainTimeoutCancelsWorkersBeforeLeave`、`TestStop_WhileHandoffGateWaitingReturnsPromptly`、`TestStop_MemberWithoutClaimsReturnsPromptly`。

### 4.5 缺口修好之前的发版操作

| 时机 | 做法 |
|---|---|
| 发版前 | 线上 go/db 版本都不早于 be6b4ab1d,最低不早于 cb2c25334;更老的不混跑,走"停写 → 排空 → 先起 db、再起 login、最后起 scene"的离线流程(db-task-kafka-partition-contract.md:55-62) |
| 发版前 | MySQL 连接余量不少于每 zone (60+8E)+35(E 为额外落点库数) |
| 顺序 | 只加列的迁移 → go/db 全部换新 → 会产生新 MsgType 的 C++;落点相关:data_service、scene_manager → go/db → C++;epoch 相关:先升全部 scene,再升 scene_manager |
| 窗口约束 | 同一窗口不重启 Redis/MySQL,不跑阻塞 DML 的 DDL;gate 不纳入;不改 GroupID |
| 节奏 | 低峰期;多个 zone 依次滚(共用消费组) |
| 每换一个 Pod | 消费组 lag 回落;重试队列、processing 列表、`kafka:dead:queue:*` 归零不增长(命令见 [merge-zone-runbook.md](../ops/merge-zone-runbook.md):476-506);日志无 `claim drain timed out`、`DATA-LOSS PREVENTED`、`retry reschedule failed`、`Release lock failed: lock expired` |
| 异常 | 某 Pod 被强杀时,至少等 2 分 40 秒(锁 TTL 2 分钟 + 收据回收 40 秒)再换下一个 |

---

## 5. 修复计划

### 5.1 总览

| # | 短板 | 修法一句话 | 规模 | 优先级 | 复核 |
|---|---|---|---|---|---|
| 子 shard | S4 | 取模前 splitmix64 打散 | 2 文件 | P0 | **已落码**(§7) |
| G0 | login 自锁 | 失败分支改用已持锁版本 | 2 文件 | P0 | **已落码**(§7) |
| ⓪ | 共享组下 zone 回档的位点重置静默失效 | `kafka_offset_reset.ps1` 输出有 `Error:` 就失败 | 2 文件 | P0 | **已落码**(§7);"回档 Step 2 只统计本 topic"未做 |
| ① | S3 读写不同分区 | 以 C++ 实际在用的 CRC-32 mod N 为契约,只把 login 的读搬过去;删掉从未生效的哈希环与"不可用分区重映射" | 约 23 文件;C++ 可选 5 个 | P0 | sound-with-fixes |
| ② | G2 1040 进死信;S6 连接预算 | 批 A:连接不可用时写任务延后、不耗重试预算,导出 DBStats;批 B:按真实并发上界压缩各池,加部署前预算门禁 | 37–40 文件,**超 30** | A P0 / B P1 | **未经独立复核**。2026-10-13 订正:另一会话的 G2b 已落「导出 DBStats(仅 go/db、data_service)+ MySQL 抬到 1800 / 5Gi」,与本行批 B 的池压缩路线冲突,批 B 作废待定;批 A 剩下的「1040 不耗重试预算」= L-04,G2b 不做。见交接文档 §2.3、§4.0 |
| ③ | S2 共用消费组 | 每 zone 一个组;交接闸放在入组前;交接后删除旧组在本 topic 的位点 | 28–33 文件,**可能超 30** | P1 | sound-with-fixes(6 个阻断项已并入) |
| ④ | S1 重试跨实例并发 | 按分区拆 ready 列表,只在持有该分区 claim 的进程里执行重试;扩容只调 SubShardCount,不加分区 | 批 1 27–29 文件 | P1 | sound-with-fixes |
| ⑤ | S5 guild | 不做 actor。阶段 0 补生命周期、连接池上限、指标、K8s 清单、压测;阶段 1 按压测判据逐项打开 | 超 35 文件,拆 4 个 PR | P2 | sound-with-fixes(5 个阻断项已并入) |
| 条件写 | G1/G3/G5 | `(owner_epoch, save_seq)` 条件写,方案要点与复核修正见 §6.3–6.4 | 20–25 文件(约 3~4 人周),跨 C++/Go/proto/schema + Java 核实 | P0(正确性) | 库原语已落码(§6.6);go/db 侧详细设计待出 |

复核对 ④ 前提的修正:非属主执行重试仍受排序锁与游标约束,会被串行化;真正会写坏数据的只有"排序锁租约丢失"。④ 降低它的概率,根治靠条件写。

### 5.2 落地顺序

1. **第 0 批(已落码,待验证):** 子 shard、G0、⓪、汇总脚本修复;另需把 Kafka 镜像从 `apache/kafka:latest` 钉到具体版本(未做)。
   - 2026-10-16 更新:Kafka 镜像已钉版(③ PR-A,2026-10-15 落码,未验证):`deploy/docker-compose.yml` 两处、`deploy/k8s/manifests/infra/kafka.yaml`、`kafka-topic-init.yaml` 四处都是 `apache/kafka:4.3.1@sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837`;上线前的 V0 核对与升级步骤见 `deploy/k8s/README.md`「镜像钉版本」。
2. **第 1 批(正确性):** ① 与 ② 批 A,并行开发、串行合入。分别对应"读到旧档"和"丢存盘",都不依赖条件写,也不需要换代 topic。
3. **第 2 批:** ③。拆组后每次 go/db 发布只停本 zone,后续各批的滚动都受益;共享组下 ④ 的 owner 模式会因任一 zone 再平衡而全服出现重试撤回尖峰。
4. **第 3 批:** ④(a)(c)。
5. **第 4 批:** ② 批 B,等批 A 的 DBStats 观察一段时间再定值;go/db 主池压到 40 要在 ④ 之后。
6. **第 5 批:** ⑤ 阶段 0,必须排在 ② 批 A 与预算门禁之后(guild 会新增全局 MySQL 连接)。
7. **条件写:** 与 ①②③④(a)、⑤阶段 0 正交;建议在 ③ 之后上线,这样它的逐 zone 滚动只停本 zone。硬依赖它的只有 ④B2 在线换代与 ⑤ 打开 AssetOp。

**合并顺序:** ①②③④ 都改 KOC,按"子 shard → ②A → ① 的观测 → ③ → ④"合入。

### 5.3 需要拍板的项

1. 超过 30 个文件的项分批授权:②、③、⑤阶段 0;④ 两批合计 35–38 个(AGENTS §10.2)。
2. 换代 topic(④B2)建议现在不做:按现有数据 P=10 够用到单 zone 峰值约 8.5k task/s;真要做须先落条件写,并先修订 TiDB 决策 D5。
3. 改 C++ 只有 ① 的可选批(把分区器显式写成默认值,加 golden 测试),运行时零变化。
4. ④ 与 TiDB D5"重试队列不动"字面冲突,需要与 TiDB 迁移分批排期。
5. Z≥3 且 TiDB 未就绪时,改 MySQL Pod 内存 limit 会触发 Recreate,等于停服。
6. ⑤ 上线后是部分可用:AssetOp 关闭、历练不开放;NetworkPolicy 需要 CNI 支持。
7. ① 带来的行为变化:db 积压超过 8 秒时,表现从"静默读到旧档"变成"进场失败"。

### 5.4 逐项要点与不停服上线步骤

**三条通用纪律:**
- 只改 ConfigMap 不会让 Pod 滚动。所有"改配置生效"都要 `kubectl rollout restart` + `rollout status`,以 Pod 启动日志里的生效值核对。
- ③ 完成之前,任何 go/db 滚动都会让全服一起再平衡:选低峰、一次一个 zone、先核对 MySQL 连接余量。
- kill -9、暂停 MySQL 等注入演练只在 kind 或 staging 做,生产只被动观察。

**① 读写分区对齐**

| 改动 | 位置 |
|---|---|
| 契约唯一实现:DBTaskKey、DBTaskPartition、NewDBTaskPartitioner(= sarama.NewConsistentCRCHashPartitioner),附 golden 表 | 新增 go/shared/kafkautil/dbtask_partition.go 及测试 |
| 主生产者设置 Partitioner(必须在 NewClient 之前) | go/login/internal/kafka/key_ordered_producer.go:58-71 |
| 删除哈希环与"不可用分区重映射"(重映射本身也破坏同 key 有序),删除 pkg/consistent | 同文件 |
| `db_task_zone_` 前缀 topic 统一走契约分区器;SendToTopic 按前缀拒绝 | command_partitioner.go:28-34 |
| go/db 只计数不拒绝(写不符告警) | KOC ConsumeClaim 插一行;新增 producer_partition_audit.go |

| 步 | 做什么 | 看什么 | 回滚 |
|---|---|---|---|
| 1 | 只读门槛:kcat 抽样(不提交位点),离线复核每 zone 的写消息是否符合 CRC(key) mod 10 | 写不符 = 0,否则先找出不守契约的写入方 | 无需 |
| 2 | staging 全流程 + 门控集成测试 | 同上 | 回滚 staging |
| 3 | 生产逐副本滚动 login | EnterGame 成功率、preload 失败数、读阶段耗时持平 | 回滚 login 镜像(G0 已单独修,回滚不会带回自锁) |
| 4 | go/db 观测随 ②A 一起上 | 写不符告警静默 | 删除规则 |

新旧并存安全:读不推进游标、不拿排序锁、不进 epoch 守卫,写的分区函数没变,混跑只会单调变好。覆盖范围:子表缓存还在时 login 根本不发读(ensure_player_all_data_async.go:82-87);访客与合服后跨 topic 的读写不在覆盖范围。

2026-10-16 落码说明(① 已落码,未编译、未测试,待 Codex 验证):契约、两侧实现、告警与滚动步骤以 [db-task-kafka-partition-contract.md](db-task-kafka-partition-contract.md)「分区函数契约」「滚动对齐步骤与观测」为准,与上表的差异:第 1 步改用 Kafka 镜像自带的 console consumer 只读抽样(kcat 的 `%s` 会被载荷换行打断,只能用来核对分区),离线复核用 `TestDBTaskPartitionAuditDump`;第 4 步的 go/db 观测随下一次例行 go/db 发版上,不再绑 ②A;C++ 可选批也已落码(显式 `consistent_random`、`DbTaskKafkaKey`、golden 测试),随下次 scene / battle 例行发版;读规则告警等 login 全部滚完再 apply。屏障的五个缺口与放大效应见契约文档「屏障覆盖范围与剩余缺口」。

**② 1040 延后与连接预算(未经独立复核)**
- 批 A:handler 返回 error,识别 1040、1203、ErrBadConn、ErrInvalidConn、net.Error 并包成 errStoreConnUnavailable;写任务豁免重试预算、读任务照常消耗;新增 `db_task_deferred_total{reason}`;导出 DBStats(go/db、data_service 5 个池、friend、trade);同批加"重试队列持续增长""deferred 持续大于 0"告警。
- 批 B:go/db 主池按 ceil(10/2)×8 推导为 40,访客池 4/1,名字池 8/2,gateway Hikari 4/1;部署前 `Assert-MySqlConnectionBudget` 门禁。Z=2、E=1 时:稳态 364 + 单 zone 滚动增量 71 + 预留 30 = 465 ≤ 500。
- 临时余量一律 `SET GLOBAL`,同时改 ConfigMap;禁止 `SET PERSIST`。
- 上线:只读基线(新脚本 mysql_conn_report.ps1)→ 内存余量够时 SET GLOBAL 临时加约 110 → 逐 zone 发布批 A → 观察一个高峰定参数 → 逐 zone 发布批 B(gateway → data-service → db)→ 收回临时余量。
- 风险:DSN 没设读超时,MySQL 卡住时 SQL 只挂住不报错,延后分支覆盖不到。

**③ 消费组按 zone 拆**
- 组名 `db_rpc_consumer_group_z{id}` 直接写进 ConfigMap(旧二进制回退时也进 zone 组)。
- 交接闸挪到入组之前,Setup 只负责播种:本组没有位点时 `MarkOffset(对方 committed)`(不加 1),已有位点不动;守望要让出分区,必须同时看到"对方持有分区"与"对方已提交位点在前进"。
- 收尾:新组 10 个分区位点都 ≥ 播种值后,删除旧组在本 topic 上的位点并 describe 复核;再滚一次去掉 GroupHandoffFrom。组归属以 Kafka 为准,两组都有位点就 throw。
- 回档:组判定挪到 Step 0;merge_zone 只认唯一权威组(原来的"多组取最小 LAG"是 fail-open)。
- 上线:发布带闸二进制、配置不变(记录 rebalance_gap 基线)→ 发布脚本工具 → 金丝雀 zone 切组(冻结其它 zone 发布;看 seeded=10、conflict=0、gap<15 秒、播种值等于切换前 CURRENT-OFFSET)→ 浸泡 24 小时并做一次 delete pod → 其余 zone 每天切 1–2 个。回滚:单 zone 反向交接。
- 如实说明:闸 + 守望做不到绝对互斥,兜底靠按 topic 的游标;"没有重叠就没有并发写者"要等 ① 落地后才成立。
- 2026-10-16 落码说明(③ 已落码,未编译、未测试,待 Codex 验证;设计、决策表与 runbook 以 [db-consumer-group-per-zone.md](db-consumer-group-per-zone.md) 为准)。相对原方案的四处偏离:
  - **最终形状 + 必填键证明**(取代 OCI label 等镜像能力校验):go-svc-db-config 只有四种形状——最终形状 `GroupID: "G"`(任何二进制都能起),交接形状 `ConsumerGroup` + `GroupHandoffFrom`、不写 `GroupID`(交接闸之前的二进制把 `GroupID` 当必填,在 `conf.MustLoad` 就拒启);回档 Step 6、全新 namespace、收尾之后的 keep 都只渲染最终形状。
  - **收尾后去掉 `GroupHandoffFrom`**:`kafka_offset_reset.ps1 -FinalizeHandoff` 删掉旧组在本 topic 的位点之后,以 keep 再跑一次 zone-up,`k8s_deploy.ps1` 读到「交接形状 + 对方组已无位点」就渲染最终形状(source=handoff-finalized),db 滚动一次;收尾之后进程里没有交接闸,不再依赖 legacy 组的 coordinator。
  - **运行时 R7 保留**:两组在同一分区都有位点且对方更大时闸不放(`wait_own_behind_peer`,告警 `DbConsumerGroupHandoffOwnBehindPeer`),作为手工部署、旧脚本的纵深防御;部署期由 `k8s_deploy.ps1` 读 Kafka 事实(`Read-DbGroupTopicFacts`,`kubectl exec` 进 kafka-0)做 F2 / F4 的拒绝。
  - **回档永远落 zone 组**:`k8s_zone_rollback.ps1` 删掉 Step 2 的等待循环,Step 5 重置 zone 组并用 `-RetireGroup` 删 legacy 组在本 topic 的位点,Step 6 固定 `-DbConsumerGroup zone`,落到 final(Z)。

**④ 重试只交给分区属主**
- 属主表:claim 的 ctx 还活着就是属主;新增 `kafka:retry:queue:{topic}:p{N}`,分拣用单条 Lua;`RetryRouting` 开关(shared/owner);"认领之后、执行之前"的每个出口都要原子搬走收据。
- p* 搬回工具与三条告警;merge_zone 预检用 SCAN 查 p*,SCAN 失败就拒绝;`replicas ≤ PartitionCnt` 门禁。
- 前置:按 [placement-reverify-checklist](../handoff/placement-reverify-checklist-20261012.md) 跑通双 db 实例那几项(be6b4ab1d 从未编译运行)。
- 上线:kind 上确定性注入演练 → 发布批 1(shared)→ 单 zone `DB_RETRY_ROUTING=owner` 被动观察(owned 分区求和 = 10、executed_nonowner = 0)→ db.yaml 改 owner 逐 zone restart。回滚到旧镜像前必须先让 p* 归零或执行搬回工具。

**⑤ guild 多实例上 K8s**
- 原 actor/write-behind 方案作废:它违反资产路径 fail-closed。
- 阶段 0:照 friend.go:205-262 先起服再注册(现在 etcd 注册 guild.go:152 早于 gRPC 起服);advertisedHost 优先 POD_IP(现在登记 0.0.0.0);停机预算重算、注销加超时;连接池 16/8;RebuildRanks 等锁超时不 panic;K8s 清单、migrate Job、NetworkPolicy,AssetOp 恒为 false;guild-stress 压测 A–F 六个场景,stress_snap / stress_summarize 增加 Guild 段。
- 阶段 1:按压测判据逐项打开;在 TiDB 上去掉全局哨兵。
- 上线:dev 双实例 → SET GLOBAL 提高上限 → migrate Job → apply guild → data-service 补 GuildInternalRpc → kind 滚动重启演练 → G0 压测决定是否做阶段 1。

### 5.5 交给 Codex 的验证(各批落码后执行)

| 项 | 命令 | 通过标准 |
|---|---|---|
| ① | go/shared 下 `go test ./kafkautil/ -run DBTask -v`;login、go/db 的 build 与 test;`pwsh go/test.ps1` | golden 表与 Go crc32 独立重算一致;全仓查不到 pkg/consistent |
| ② | 四个服务各自 vet、build、test;`all-up -DryRun -WaitReady`;本地把 max_connections 压到当前连接数逼出 1040 | 账本 ≤ 500;死信不增长且 deferred > 0 |
| ③ | `go test -run "GroupHandoff\|ClaimAcker\|OrderingGuard" -v`;merge_zone 测试;本地反复正向/反向交接 | 每次 seeded=10 且等于对方 CURRENT-OFFSET;反向交接空窗 < 15 秒 |
| ④ | `go test -race -run 'TestRetryRouting\|TestOwnedPartitions\|TestConsumeClaim\|TestRetryOwnership'`;本地双实例确定性注入 | strict 模式 0 mismatch;所有列表归零 |
| ⑤ | gofmt、Linux 交叉编译 + vet、`go test ./...`;真库 `-p 1` 跑锁序回归;kind 演练 | 无 SKIP;无 DEADLOCK;输出 GUILD_SMOKE_OK |

---

## 6. 写路径 Redis 往返优化

### 6.1 结论

- **能优化。** 同实例部署下,每条写任务的 Redis 往返最多能从 11 次降到 2 次(落点 Redis 独立部署时降到 4 次),MySQL 仍是 1 次。
- **但现在不直接改。** go/db 自己的分段耗时还没有任何实测;R19 实测吞吐只有 55~62 task/s、Kafka lag 为 0;写任务也不在玩家能感知的路径上(在线数据以 Redis 父键为准)。按 AGENTS §11.3,先补测量;正确性上先做条件写;往返合并等突发排空确实超过 SLO 再做。
- **口径订正:**
  - 11 次是"Kafka 来源且 owner_epoch>0 的写":main-redis 9 次 + placement-redis 2 次(dev 与 K8s 下两者是同一个 client、同一台 Redis)[C]。重试来源是 13 次;一次玩家存盘发两条 DBTask,实际是 22 次。
  - 11 次里有 7 次是 R19 之后加的(排序锁、applied_epoch、选库与复核两次 MGET),从没压测过。
  - "1 次 MySQL"没有核实:Save 可能是 REPLACE、ODKU,也可能是 UPDATE→INSERT→1062 这条链(1~4 条语句);连接没开 InterpolateParams,带占位符的语句可能还多一次 prepare 往返。

### 6.2 往返清单(Kafka 来源写)

| # | 命令 | 客户端 | 守什么 | 能否合并 / 删除 |
|---|---|---|---|---|
| 1 | SET NX EX120 排序锁 | main | 跨实例同 (topic,key,msgType) 互斥;merge_zone R2 等它消失 | 并进 SQL 前的 pipeline(下称 PRE);条件写之后对一类任务可删 |
| 2 | GET 游标 | main | 同分区 offset 只进不退 | 并进 PRE;条件写替代后可删 |
| 3 | GET applied_epoch | main | 挡被废黜节点的迟到写 | 同上 |
| 4 | MGET 落点 + zone(选库) | placement | 按处理那一刻的落点选库 | 同实例可并进 PRE;**不能改成进程内缓存** |
| 5 | GET `kafka:expand:status` | main | 在线扩分区 | **死代码**:生产代码没有任何地方写入 expanding(只有测试与 KOC:1832 写 normal),恒读到默认值;删除需单独决策 |
| — | MySQL Save | mysql | 业务落库 | 不能删;改条件写;不做多行合并 |
| 6 | MGET 复核 | placement | 选库后被搬库的检查与使用时间差(player-storage-placement.md §6.3) | 必须在 SQL 之后;同实例可并进 SQL 后的 Lua(下称 POST),改为对选库时原始字节做 CAS;做了条件写也不能删 |
| 7 | SET 子表缓存 | main | login 预加载命中最新快照 | 并进 POST,并挪到 token 校验之后 |
| 8 | EVAL verify / Extend | main | 推进游标前确认仍持锁 | 并进 POST;条件写之后可删 |
| 9 | SET 游标 | main | 记录已落库 offset | 同上 |
| 10 | EVAL epoch 取 max | main | 推进已落库 epoch | 同上 |
| 11 | EVAL 释放锁 | main | 释放互斥 | 并进 POST 末尾 |
| 重试 +2 | EVAL 认领、LREM 收据 | main | 收据原子认领与确认 | 认领不合并;LREM 并进 POST |

### 6.3 分阶段

| 步骤 | 改动 | 写任务往返(Redis / MySQL) | 前置 |
|---|---|---|---|
| 0 补测量(单独 PR,只加观测) | ① 汇总脚本 DB 段已修(§7),若 R17~R19 快照还在,用修好的脚本重跑出基线。② go/db 打点:`db_task_redis_step_seconds{step}`(11 个固定取值);新 stage:sql、task_total、queue_wait、kafka_e2e;`db_ordering_guard_total`、`db_subshard_batch_size`;go-redis PoolStats 与 sql.DBStats。0.1ms、0.25ms 两档桶放在新指标里,不改原 stageBuckets;共享 client 只挂一个 Hook;label 不带 player_id。③ Redis commandstats / SLOWLOG;MySQL 用 performance_schema 核实 Save 的实际语句与条数。④ Codex 按 AGENTS §6.2 跑 R20(基线 R19),加"突发全量存盘"场景 | 不变 11 / 1 | 由负责人定排空 SLO(现在没有);确认生产是否配了独立 Placement.Redis |
| 1 低成本 | 显式设置 go-redis PoolSize;retryClaim / Release / Extend 改走 EVALSHA;删 expand 死代码(单独决策) | 删 expand 后 11→10 | 第 0 步数据显示有连接池等待 |
| 2 正确性轨:条件写 | DBTask 加 save_seq;两张玩家表加 fence 列;proto2mysql 提供带 ctx 的条件 UPDATE(外部仓库发版需授权);写库改按 (epoch, seq) 条件写,缓存改带版本写入;补 MySQL 读写超时 | Redis 仍 11;MySQL 稳态 1 条 UPDATE | K8s 2 副本上线前完成;20~25 个文件,约 3~4 人周,每个库都要 DDL |
| 3a 性能轨:条件写之后删守卫 | 一类任务跳过排序锁、游标、applied_epoch | 11→4(删 expand 后 3) | 满足立项判据;能力心跳做机器门禁;回退只能先切回双轨 |
| 3b 性能轨:往返合并 | SQL 前普通 pipeline 发 PRE;SQL 后一段 Lua 发 POST(token 校验→落点 CAS→缓存→游标→epoch 取 max→LREM→DEL) | 同实例 11→2,独立实例 11→4;推算每条写本地省 5~13ms、K8s 省 2~9ms [I] | 满足立项判据,且 Redis 段确实是排空瓶颈;主 Redis 单分片 |
| 4 按批调度 | 在 3b 基础上按波调度 | 每任务再省 3(1−1/N);稳态 N≈1 等于没省 | 突发排空批大小 p50≥4,且 Redis 段占比≥40% |

**先后顺序:** 先测量;正确性轨(条件写)排在性能之前,立项不靠性能数据;性能轨先 3a 后 3b(3a 删掉的正是 3b 要合并的那几次,反过来做 3b 的脚本要整段重写);按批调度放最后。

**立项判据(满足任一条才进 3a / 3b):** 突发排空时长超过 SLO 或 Kafka lag 积压;go/db 在共享 Redis 上的命令量或 CPU 占比显著,同时 login 的 cache_check、sub_cache 变慢;Redis 段占写任务 task_total 50% 以上且排空不受 SQL 或连接池约束。

### 6.4 复核要求带上的修正

| 方案 | 结论 | 必须带上的修正 |
|---|---|---|
| 往返合并(3b) | sound-with-fixes | 共享 client 只挂一个 Hook,否则每条命令记两次;传输错误继续计入 lookup_error / read_error(合服 runbook 拿它当门禁);"同一实例"按两个 client 是否同一对象判断,不按配置;读任务仍走原 MGET;MOVED 解析失败 fail-closed;脚本返回值显式带"是否存在";PRE 用普通 pipeline 不用 Lua |
| 按批调度(4) | sound-with-fixes,现在不值得 | pipeline 里的 EVALSHA 遇 NOSCRIPT 不会自动回落,Redis 重启后存盘会持续进死信,须先 SCRIPT LOAD 或 Exec 后重发;每行 SQL 前检查租约;go-redis 自动重发 pipeline 时 SETNX 会被误判为锁被占,取锁要幂等并释放本波发出的全部 token |
| 条件写(2) | sound-with-fixes | stale_owner 进死信,不能直接 ack;清 Redis 的流程要同步把围栏归零;save_seq 从 PlayerAllData 与 fence 列取初值,不能只靠墙钟;判 older 之后先复核再 ack;off 模式不能调 Save(会把围栏清零);删守卫那一步必须有机器门禁 |

### 6.5 不推荐做的事

- 跳过测量直接改(违反 AGENTS §11.3)。
- 多行 upsert:分不出哪一行被围栏拒掉,与条件写结构冲突。
- 整批一个 Lua,或把判定规则写进 Lua:前者长时间阻塞登录同步路径也在用的 Redis,后者造出两份真相。
- 缓存落点省 MGET、把复核挪到 SQL 之前、做了条件写后删复核:违反 player-storage-placement.md 的 P-3 与 §6.3。
- 为 Redis Cluster 改 hash tag 键名:破坏滚动升级兼容与 merge_zone 的锁键镜像。
  - **(2026-10-19 指向注,上条原文保留)** 本条不再作为否决理由:负责人 2026-10-13 已定 Redis 分片 hash tag「以按 player_id 分片为准」(PROGRESS「2026-10-14 存盘管线接手」条目 §5 拍板记录第 16 条),2026-10-17 又拍板核心走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」。上面两点改为分片迁移必须满足的约束:排序锁改名与 `tools/merge_zone/relocate.go` 的 `orderingLockKey` 镜像同批改;核心闭包键改名不做双读:首次上线前改名的直接以新键名起步,没有混跑窗口(项目尚未上线;[shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §20.7 首条、§25.2 第 22 条);上线后才走停写切换或版本门禁(§20.7;本句 10-22 订正)。分片时还要算进 db-write-fencing-mysql.md §6.4 的同 tag 约束:令牌发布脚本里的排序锁、子表缓存、资产证据与 `save_barrier` 同 tag(若该稿 §18.7 选 A 值封装,缓存可以脱离);围栏 §5.5 推进脚本里的排序锁、游标、已落库 epoch 同 tag。详见该稿 §20、§21.6(纯设计,未落码)。

### 6.6 proto2mysql 条件写原语(2026-10-13,已推分支,未验证)

条件写在库里的那一半已经落码,由负责人授权在 proto2mysql 仓库改动并推送:
[luyuan-cpp/proto2mysql `feat/conditional-save`](https://github.com/luyuan-cpp/proto2mysql/tree/feat/conditional-save),commit `e81a1e7`。**未合入 main、未打 tag**;go/db 仍钉 v0.2.0,不受影响。

- **事实订正**:ctx 不需要库改——`pbDB.WithContext(ctx)` 早就把 ctx 带到每条语句;go/db 的 handleDBWriteOp 忽略了 ctx,这是 go/db 侧要改的。RTT C 方案设想的 `UpdateByPrimaryKeyContext` / `InsertContext` 两个低层原语不再需要。
- **API**:`DB.SaveConditional(msg, SaveOptions{FenceFields, AllowEqual, PreserveFields}) (ConditionalSaveResult, error)`,GormDB 同名同义。结局 Updated / Inserted / Unchanged / Rejected,被拒时 `Stored` 是库内栅栏值,由调用方区分 equal / older_seq / stale_owner。
- **go/db 映射**(设计评审定稿):class-1 严格模式、栅栏 `[fence_owner_epoch, fence_save_seq]`;class-2 用 AllowEqual,dual 模式遇到 Stored=(E, s>0) 时再发一次 `Fence=[epoch] + AllowEqual + Preserve=[seq]`;class-3 在 dual 与 sql 都用 AllowEqual(消息栅栏 (0,0));off 只带 `PreserveFields`(不清零栅栏)。不可能出现的分支报错、不 ack。
- **关键性质**:状态查询与整行比较一律 FOR UPDATE(读写分离层留在主库,防止从库滞后把旧属主重放误判成已生效);最多两轮,`ErrConditionalSaveRace` 可重试、`ErrSaveMatchMismatch` 指向表结构问题;栅栏参数一律 uint64 绑定;Save 的语句一字未改。
- **评审与复核**:设计经三视角对抗评审(去掉任意上界 FenceBelow、改先读状态再 INSERT 等),实现经两轮代码复核(无编译错误、无必然失败用例)。本机没有 Go 工具链,**未编译、未运行**。
- **Codex 验证**(在 `D:\luyuan\proto2mysql`、分支 `feat/conditional-save`):`go vet ./...`;`go test -count=1 ./...`;`cd tools/proto2sql && go test -count=1 ./...`;`PROTO2MYSQL_INTEGRATION=1 go test -count=1 -run SaveConditional -v .` 分别对 MySQL 8.x 与 TiDB 8.5;`go test -race -count=1 -run SaveConditional .`。通过标准:全绿、真库用例零 SKIP。通过后合入 main、打 v0.3.0(打 tag 需负责人确认),再改 go/db 的 go.mod 与 replace。

---

## 7. 本轮已落码(已随本地提交 d5cbb8a8e 入库、未 push;未编译、未测试,待 Codex 验证)

| 项 | 改动 | 测试 |
|---|---|---|
| 子 shard 倾斜(S4) | KOC 新增 `subShardIndex` / `spreadKey`(splitmix64 终混步,与 C++ `SpreadPlayerId` 同算法同常量);同 key 恒落同一子 shard;子 shard 只是进程内划分,新旧实例混跑不影响正确性 | 新增 go/db/internal/kafka/subshard_route_test.go:splitmix64 公开向量对拍、同 key 稳定、step=0 的 bwmarrin ID 与连续号段 ID 都能均分、routeToSubShard 按新函数投递 |
| login 自锁(G0) | key_ordered_producer.go 新增 `markPartitionUnavailableLocked`,`SendTask` / `SendTasks` 的失败分支(已持 `p.mu`)改调它。sarama v1.46.2 的 `SyncProducer.SendMessage` 返回底层错误,`ErrLeaderNotAvailable` 文案为 "...no leader for this partition and hence it is unavailable for writes",命中 `isPartitionUnavailableErr` 的兜底匹配,所以单条发送在 Kafka 选主时必然自锁;批量发送返回 `ProducerErrors`,文案不含 "partition",不触发 | 新增 go/login/internal/kafka/key_ordered_producer_lock_test.go:用 sarama/mocks 注入 ErrLeaderNotAvailable,断言两次 SendTask 都能在 5 秒内返回、锁已释放 |
| 回档位点重置静默失效(⓪) | tools/scripts/kafka_offset_reset.ps1 收集 kafka-consumer-groups 的 stdout+stderr,新增 `Assert-KafkaToolOutputClean`:退出码非 0 或任一行以 `Error:` 开头即失败 | 新增 tools/scripts/tests/kafka_offset_reset_output_guard.tests.ps1(7 例) |
| 压测汇总 db 分段显示 "-" | tools/scripts/stress_summarize.ps1:client_golang 输出时按 label 名排序(`op` < `stage`),脚本按 `{stage=..,op=..}` 精确查永远查不到;改为 `{op=..,stage=..}` | tools/scripts/test_stress_summarize.ps1 补 DB 段用例:按真实 label 顺序伪造 t18m_db 快照,断言 6 个平均值都算得出来 |

Codex 验证命令:

```bash
# go/db
cd go/db && go test ./internal/kafka/ -run 'TestSpreadKey|TestSubShardIndex|TestRouteToSubShard' -count=1 -v && go test ./internal/kafka/... -count=1
# go/login
cd go/login && go vet ./internal/kafka/ && go test ./internal/kafka/ -run TestSendTaskLeaderNotAvailableDoesNotDeadlock -count=1 -v && go test ./internal/kafka/... -count=1
# 脚本
pwsh -File tools/scripts/tests/kafka_offset_reset_output_guard.tests.ps1
pwsh -File tools/scripts/test_stress_summarize.ps1
```

通过标准:新增用例全绿,包内既有用例无新增失败。login 的回归用例在改动前的代码上应当以"deadlocked"超时失败;需要红/绿证据时,在临时 worktree 里只回退 key_ordered_producer.go 跑一次,不要动当前工作区。

### S4 落码(G1 治标与 go/db 小项)

> 2026-10-16 补。状态:**已落码,未编译、未测试,待 Codex 验证**。go/db 部分随 `de4960bee` 入库(其后 R1 语义复核的补修——§4.4「G6 停机顺序」那三处与对应用例——以及 R2(④)对 KOC、`store_failure.go` 等同批文件的改动,随 `e76f6f747` 入库);
> 脚本随 `a7c510d65` / `0f6690604` 入库;`deploy/k8s/manifests/go-svc/db.yaml` 的 `minReadySeconds: 20` 与 `deploy/k8s/db-task-retry-alerts.yaml` 的 `db-task.ordering-lease` 组随 `e76f6f747` 入库。
> 规格:`docs/handoff/godb-pipeline-impl-20261015/S4-g1-and-small-items-spec.md`(没有单独复核);落码阶段 A3 因额度中断没有报告,由 R1 对照代码核实补完。验证与上线清单见[上线待做](../ops/godb-pipeline-launch-todo.md) §4。

| 项 | 文件 / 符号 | 要点 |
|---|---|---|
| G1 ① 业务池超时 | `go/db/internal/logic/pkg/proto_sql/db.go` 的 `newServiceMysqlConfig`(L-04 已做) | 本项不改代码,只补单测(上线待做 §2.3 第 9、10 条) |
| G1 ② SQL 接 ctx | KOC 的 `handleDBWriteOp` / `handleDBReadOp` 改用 `store.WithContext(ctx)` | `dbOpHandler` 签名不变;写的 ctx 来自租约,读的 ctx 随 claim |
| G1 ③ 核锁前移 | KOC 的 `runDBOp` | 写任务固定顺序:`beginSQL` → handler → `finish` →(sqlCtx 未被本进程取消才 `reportStoreResult`)→ 落库后复核 `confirm` → 核锁 `verify` → `publishDBOpResult`;`handleTask` 里原来那段 verify 删除。`markAppliedSeq` / `markAppliedOwnerEpoch` 的调用与顺序不变 |
| G1 ④ 续租有界退避 | `go/db/internal/kafka/ordering_lease.go` 的 `orderingLease.watch` | 本地单调时钟截止时间 = validUntil − `orderingLeaseSafetyMargin`(30s);首次 500ms、逐次翻倍、封顶 5s、±20% 抖动;单次 3s;每轮故障最多 10 次;用尽记 `renew_exhausted`,到截止记 `renew_deadline`,发现易主立即 `owner_changed`;刻意不监听 worker ctx |
| G1 ⑤ 释放计数 | 同文件 `releaseOrderingLock` | `db_ordering_lock_release_total{outcome=released\|not_owner\|error\|skipped_sql_abandoned}`;not_owner 打 ERROR「ordering lock already expired or taken over at release; MySQL may hold an older snapshot until the next save」 |
| G1 告警 | `deploy/k8s/db-task-retry-alerts.yaml` 组 `db-task.ordering-lease` | `DbOrderingLockReleasedNotOwned`(critical,`increase(db_ordering_lock_release_total{outcome="not_owner"}[10m]) > 0`);`DbOrderingLeaseLost`(warning,按 reason:`owner_changed` / `renew_exhausted` / `renew_deadline` / `verify_error`);`db_ordering_lease_renew_retry_total` 只进看板 |
| margin 自检 | `ordering_lease.go` 的 `leaseMarginCoversLockWaits`,在 `NewKeyOrderedKafkaConsumer` 里调用 | 两个会话锁等待之和 + 5s 超过 30s margin,或任一值 ≤0(关闭)时打 ERROR,不拒启 |
| G6 停机顺序 | KOC 的 `Stop` / `stop` | R2(④)合入后为六步:① 撤拉取 ② 等消费循环退出 ③ 离组 ④ 取消 worker 并等退出 ⑤ 归还重试收据、收掉交接闸 ⑥ `c.enqueuer().close()`。② 超时的处理与 ③④ 互等的修法见 §4.4「G6 停机顺序」 |
| G6 minReadySeconds | `deploy/k8s/manifests/go-svc/db.yaml` | `minReadySeconds: 20`;属于 Deployment spec,改它不触发 Pod 重建,下一次滚动生效;`terminationGracePeriodSeconds` 仍为 45 |
| G9 优雅停 | `tools/scripts/go_services.ps1` 的 `Invoke-Stop`(`Send-ConsoleCtrlC` / `Wait-ProcessExit` / `Stop-ProcessTree`) | 先对全部目标发 Ctrl+C,再按剩余时间逐个等(db 35s,其余 25s,`-StopTimeoutSeconds` 可覆盖),超时或发不出去时 `taskkill /T /F` 强杀进程树;`-ForceStop` 走旧行为;启动不得加 `-NoNewWindow` |
| 测量打点 | `go/db/internal/metrics/pipeline.go`;`db_task_stage_seconds` 新 stage | `db_task_redis_step_seconds{op,step}`(11 个 step,`RedisSteps`)、`db_ordering_guard_total{outcome}`、`db_subshard_batch_size`、`db_redis_pool_*`(共享 client 只导出一份);stage 新增 `sql` / `task_total` / `queue_wait` / `kafka_e2e`;`stress_summarize.ps1` 新增 2d「DB write path」、2e「DB Redis steps」、2f「DB retry routing」三段 |
| Lua tonumber | KOC 的 `luaDecimalGreaterFn` / `luaMarkAppliedEpoch` | applied_epoch 改为十进制串比较(先比长度再逐字节),uint64 全范围精确;Redis 里的值格式不变;条件写要比较 uint64 时复用它 |
| 过时注释 | `placement_route.go`、`proto_sql/store_registry.go`、`migrate/plan.go`(含 `plan_test.go`) | REPLACE / ODKU 改为 proto2mysql Save 的实际语句链(按主键 UPDATE → 0 行再 INSERT → 1062 再 UPDATE);保留「无主键会追加新行」 |
| L-04 单测 | `l04_deferral_test.go`、`run_db_op_test.go`、`proto_sql/service_pool_test.go`、`metrics_test.go` | 上线待做 §2.3 的 11 条,用例对照见那里 |

三句语义,任何改动都不要回退:
1. **不是栅栏**:撤不回已经进了 MySQL 的语句。go-sql-driver 取消 ctx 只关客户端连接、不发 KILL QUERY;`owner_changed` 时锁已不归本实例,窗口只是缩小。根治靠条件写([db-write-fencing-mysql.md](db-write-fencing-mysql.md))。
2. **只有失租才取消写 SQL**:写 SQL 的 ctx 由 `beginSQL` 交出(以 `context.Background` 为父、`WithCancelCause` 派生),只在 `setLost` 时取消;claim 收回、进程停机、worker ctx 取消都不取消正在跑的写 SQL——取消之后锁随任务释放,而服务端可能还在执行它,晚于别的实例的新版本提交就是回档。不用带 deadline 的 ctx:`context.DeadlineExceeded` 会被 storeerr 归成 conn_lost,误触发冷却。
3. **在途取消后锁留给 TTL**:SQL 在途时因失租被取消的(`sqlAbandoned`),`releaseOrderingLock` 不删锁、计 `skipped_sql_abandoned`,留给 2 分钟 TTL;该玩家这段时间的写报 `errOrderingLockBusy` 排队,不耗预算。

A3 落码相对规格的三处偏离(以代码为准):
1. **Consume 用随撤拉取取消的 consumeCtx**(KOC `Start` 的消费循环):`consumeCtx` 派生自 `c.ctx`,Stop 第①步关 `pullStop` 时取消,交接闸 `await` 与 `Consume` 都用它。规格只让循环条件判 `pullStopped()`、Consume 仍用 `c.ctx`;那样交接闸前的等待会卡住 Stop,没分到分区的成员(副本数多于分区数)要等满 ② 的超时。停机时 claim 的收尾不看会话 ctx(`drainClaimForShutdown`),先提交、后离组不变(用例 `TestStop_WhileHandoffGateWaitingReturnsPromptly`、`TestStop_MemberWithoutClaimsReturnsPromptly`)。
2. **`runDBOp` 在 sqlCtx 已取消时返回 `context.Cause(sqlCtx)`**:handler 失败且 SQL 的 ctx 已被本进程取消时,返回 `fmt.Errorf("%w (SQL error: %v)", context.Cause(sqlCtx), handlerErr)`——写即包着 `errOrderingLeaseLost` 的失租错误,原错误只作文本附上、不进错误链。驱动这时常给出「连接已关」一类错误,带着 storeerr 标记原样返回会被记成 conn_lost、写还会豁免重试预算。规格只写了「sqlCtx 未取消才 reportStoreResult」与「handlerErr 非空时返回」。
3. **`errOrderingLeaseLost` 的文本改为 "ordering lock ownership lost"**(`go/db/internal/kafka/store_failure.go`,原文是 "ordering lock ownership lost after DB write"):失租现在可能发生在 SQL 之前、之中或之后,续租放弃、本地截止时间已过、核锁发现易主都用它;规格只要求改注释。按 `errors.Is` 判断的调用方不受影响,按旧文本 grep 日志的要去掉 "after DB write"。

---

## 8. 未核实项

- sarama v1.43.1(go/db)的默认值:会话超时 10 秒、心跳 3 秒、Range + eager、无静态成员。文中所有"暂停几秒"都依赖这些值;再平衡停顿时长从没实测。
- ~~proto2mysql 的 Save 实际是 REPLACE 还是 INSERT…ODKU~~ 已核实(2026-10-13,proto2mysql v0.2.0 源码):都不是。Save 是「按完整主键 UPDATE → 0 行则 INSERT → 1062 则重试 UPDATE → 仍 0 行则 FOR UPDATE 归类」,1~4 条语句,没有版本条件。store_registry.go:294 与 migrate/plan.go:177 的说法都过时。
- go-sql-driver 默认超时;客户端断开后 MySQL 是否仍执行已排队的语句;Redis 的 appendfsync 实际值;topic 实际保留期。
- `--reset-offsets` 对非 Empty 组在钉死的 Kafka 版本上的退出码;`--delete-offsets` 的成功条件。
  - 2026-10-16 更新:版本已钉 `apache/kafka:4.3.1`,这两项改为「待 Codex 在 4.3.1 上核实(V3)」,清单与回填位置见 db-consumer-group-per-zone.md §11 与[上线待做](../ops/godb-pipeline-launch-todo.md) §6。
- 生产 C++ 是否全部走 CRC32(等 ① 第 1 步的 kcat 抽样);librdkafka 子模块本机未检出,golden 表要 Codex 双向复核。
  - 2026-10-16 更新:源码静态核对已闭合(db_task 只有 `SavePlayerToRedisImpl` 一个生产点,`createProducer` 显式钉 `consistent_random`);线上分布改由 console consumer 只读抽样证实;子模块在 C1 落码时已检出;golden 表改为 Go `crc32`、sarama、librdkafka 三方机械复核(契约文档「golden 向量」)。
- be6b4ab1d 双副本基线从没运行过;线上实际镜像与副本数未知。
- ② 整体没有经过独立复核;MySQL 每条连接内存、Hikari 默认值未核实。
- 假设值:guild 在 TiDB 上每笔事务 20–30ms;单 zone 峰值约 2000 task/s。
- 2026-04-04 "gate 滚动会断开全部会话"是否仍成立。

---

## 9. Java 版(AGENTS §12)

- 框架评估(§1):Java 版并发模型是 JDK 21 虚拟线程(Java 仓库 docs/design/tech-stack.md),不涉及。
- ①③④、子 shard、G0、⓪:Java 没有 Kafka 存盘链,**不适用**,待在 Java 仓库 PARITY.md 登记。
- ②:Druid 连接总账门禁,**待做**。
- ⑤:guild 的指标、压测模式、Dubbo 注册与下线顺序核对,**待做**。
- 条件写:Java 已是 `WHERE owner_epoch=?`,但推断挡不住"同 epoch 的僵尸写",需要同样补 `save_seq`,**待核实**。
