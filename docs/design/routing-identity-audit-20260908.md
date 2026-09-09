# 路由身份(routing node_id)对抗审计 —— 结论与修复清单(2026-09-08)

> 背景:同日的 [node-id-overhaul-plan-20260908.md](./node-id-overhaul-plan-20260908.md) 把**发号**职责从
> routing node_id 上摘走(永久 ID 走号段,scene 退出 snowflake 槽位协议)。本文审计的是**剩下的那一半**:
> node_id 作为**寻址身份**还在承担的四件事,在"node_id 立刻复用 + 10 万节点 + 每天重启 + 全国同服"
> 这组前提下会不会出事。
>
> 方法:4 路独立审计 + 12 路对抗验证(每条中危以上由另一个 agent 试图证伪)。
> **2 路审计被 API 403 中断**(会话反查 UC2、规模与临时 id UC4),其结论部分由 r13/r14 覆盖,
> 未覆盖部分见[第 6 节](#gap)。

---

## 0. 一句话结论

**routing node_id 没有变没用,它回到了唯一该干的事:寻址。** 但寻址身份被立刻复用,而复用的防护
(`target_instance_id` 代次校验)**只覆盖了 Kafka 这一条腿**,TCP 推送那条腿完全裸露。
四条高危里三条是"复用 + 缺代次",一条是"寻址不可达时没有重试"。

---

## 1. node_id 现在承担的四件事

| # | 用途 | 代表位置 | 审计结论 |
|---|---|---|---|
| 1 | Kafka 控制面主题 `gate-N` / `scene-N` + 消费组 `<type>-group-N` | `node_kafka_command_handler.h:146,151` | 代次校验有效,但**消费组同名**导致僵死节点吞消息;10 万节点主题数量塌 |
| 2 | 会话反查:`session_id` 高位嵌 gate 的 node_id | `network_utils.cpp:16-56` | **无代次校验**,错投到别人 socket |
| 3 | 跨节点路由载荷 `SessionDetails.gate_node_id`、`BattleRouting` | `battle_data.proto`、`player_battle.cpp` | Kafka 腿有代次(丢弃),TCP 腿无(错投);结算无重试(丢失) |
| 4 | buff/skill 进程内临时 id 的高位 | `id_generator.h` | **安全**,不跨进程比较,不落库 |

---

## 2. 已确认为真的缺陷(12 路对抗验证的结论)

### 高危

| ID | 缺陷 | 失败序列 | 现状 |
|---|---|---|---|
| **R13** | **scene→gate→客户端的 TCP 推送不带任何身份** | gate G(node_id 3)重启,新 G' 仍拿 3 号。scene 用旧 `session_id`(高位 3)反查,按 (zone, 3) 找到 G' 的连接,把 A 玩家的战斗结算/提示/广播写进 G' 上**另一个玩家**的 socket。`gate_service_handler.cpp:58` 只认 session_id;`message.proto:27-31` 的 `NodeMessageHeader` 连可校验的字段都没有 | ✅ 已修(2026-09-08):`NodeMessageHeader.target_player_id` + `BroadcastToPlayersRequest.player_list`(按 session_id 升序逐项对应),gate 写 socket 前经 `session_identity_fence.h` 比对会话当前绑定玩家,失配丢弃 + WARN(带两个 id)。**反向面同修**:`ProcessClientPlayerMessageRequest.player_id`,scene 侧比对 `SessionMap` 解析出的玩家。0 = 老发送方放行并只记一次 INFO,灰度一版后转必填 |
| **R07** | **战斗结算按位置寻址且发完不管** | battle 在结算时向 `scene-{7}` 发 `SceneCommand{target_instance_id=旧 scene uuid}`;7 号已被新进程接手,命令被代次校验丢弃并提交 offset,**不重投**。待结算的 Redis 键由 scene 写(`player_battle.cpp:1074`),而那个 scene 从未跑过 → **结算永久丢失**,玩家冻结到超时+60s | ✅ 已修(2026-09-08,按方案 ①):battle 先写 `battle:settlement:pending:{pid}` + 伴生 `:id:{pid}`(共享 Redis,TTL 7 天)**再**投递;scene 应用后条件销账 = ACK;未销账的每 10s ×12 轮重投,重投前按 `player:{id}:location` **重新解析**目标(不复用开局抓的 scene uuid,instance uuid 留空 = "谁现在持有这个 node_id 就谁执行");次数用尽记 `metric=battle_settlement_undelivered`,记录仍在,由 scene 登录钩子兜底应用 |
| **R05** | **冲突停机的排空流程从不停 Kafka 消费者** | 失去身份的节点在 15s 排空窗口里继续 fetch 并提交 offset,吃掉继任者的命令 | ✅ 已修(2026-09-08,由分区改造消除):改 `assign()` 后不再有消费组,僵死节点无从"抢走"继任者的分区 |
| **R09** | **失租且 etcd 不可达的节点永不自杀**,继续消费自己的 `scene-N`/`gate-N`,黑洞掉继任者的命令 | 同上 | ✅ 已修(同上) |

> **为什么分区改造能顺带修 R05/R09**:改用 `assign()` 直接指派分区、不再加入消费组之后,
> 僵死的 A 与继任的 B 各自独立读同一个分区、各自维护 offset,A **不可能再从消费组里抢走** B 的分区。
> B 照常收到全部命令,A 收到后被代次校验丢弃。"吞消息"这个失败模式在新模型里不存在。

### 中危

| ID | 缺陷 | 说明 | 现状 |
|---|---|---|---|
| **R12** | topic-per-node + group-per-node 在进程数上无上界 | 10 万节点 = 10 万主题 / 10 万分区 / 10 万消费组,单 broker(2 CPU、1G 堆、emptyDir)上还要扛 ~3.3 万次/秒心跳 + ~3.3 万次/秒 offset 提交、30 万+ 文件描述符 | ✅ 已修(2026-09-08):`gate-cmd_g1`/`scene-cmd_g1` 各 256 个不可变分区,`partition = node_id % 256`,`assign()` 直接指派、无消费组;全仓已无旧主题生产者,`DisableLegacyPerNodeTopic` 待滚更完成后由运维翻 |
| **R03** | `scene_handler.cpp:582` 残留一处裸 `entt::entity{node_id}` | uuid 化迁移没做完的最后一处;当前调用路径不可达,但是真实错投向量 | ✅ 已修(2026-09-08):改走 `FindNodeEntityByZoneAndNodeId`。**"最后一处"这句是错的** —— 全树复查又找出三处同类残留,一并修:`zone_utils.cpp:6`(`GetZoneIdFromNodeId`,会把别的节点的 zone 当答案返回)、`node_message_utils.cpp:14/29`(`ResolveNodeSession` / `ResolveNodeClient`,当前零调用点)。`gate_event_handler.cpp:29` 与 `client_message_processor.cpp:197/661` 经核对传的是**实体整数**不是 node_id(参数命名有误导性,行为正确),不改 |
| **R10** | `BattleRouting` 在 CreateBattle 时抓一次,全程不刷新 | 战中换 gate 后 Kafka→gate 的兜底推送失效;主线直连会自愈,结算仍从 scene 落地 | ✅ 已修(2026-09-08,取"会话变更时刷新"而非"推送时解析"):gate 在每条转发的客户端 RPC 上都会把自己的 session_id / node_id / 实例 uuid 盖进 `SessionDetails`,battle 的四个 `Handle*` 入口顺手比一次、不同就更新 `routingByPlayer` / `routingByObserver`。选它是因为 battle 在推送时**无法**解析 gate:它是全局池节点,本地注册表只有同 zone 的 gate,拿不到跨 zone gate 的实例 uuid;而按会话刷新用的是已经在线上的数据,零新增链路。只采信 `gate_instance_id` 非空的那份(直连面合成的 SessionDetails 没有 gate 身份,拿它刷新等于把旧值再写一遍) |
| **R06** | Kafka 跑在 emptyDir 上 | broker 重启丢掉全部每节点主题与 offset;消费者会自动重建空主题并从 earliest 恢复,但停机期间产出的命令在 librdkafka 5 分钟消息超时后丢失 | ✅ 已修(2026-09-08):改 StatefulSet + 20Gi PVC + headless Service(ClusterIP `kafka` 名字不变)+ PDB + KRaft 探针;保持单 broker(全仓 topic 都是 replication-factor 1,加副本不改 advertised listener 反而误路由)。**顺带发现旧 emptyDir 挂在 `/tmp/kafka-logs` 而镜像默认写 `/tmp/kraft-combined-logs`,那个卷从来没被用过**;另修探针 OOM(脚本继承 1G 堆参数)与 `CLUSTER_ID` 环境变量名 |

### 低危

| ID | 缺陷 | 说明 |
|---|---|---|
| **R11** | 命令主题继承 broker 的短保留期(300s),落后的 gate 会丢 BindSession/RoutePlayer 而不是追上 —— ✅ 已修(2026-09-08):命令主题显式 1h,且**创建后再 alter 并回读校验**(`--create --config` 只在创建时生效,被生产者抢先自动建的主题会静默沿用 broker 默认);broker 默认 dev 60s→30min、prod-like 300s→1h;`db_task_zone_N` dev 300s→1h / prod-like 600s→6h。**顺带发现 `db_task` 保留期在 15 分钟与 24 小时之间横跳** —— login 与 db 都对同一主题调 `EnsureTopics` 但传值不同,谁后启动听谁的,而这是玩家存盘通路;已对齐 |
| **R14** | `session_id` 序号 17 位,单个 gate 内累计 13.1 万连接后回绕;代次 uuid 挡不住**同一代次内**的 session 复用 —— 已由 R13 的 `target_player_id` 校验覆盖(2026-09-08) |

### 被驳回(不必改)

| ID | 原报告 | 驳回理由 |
|---|---|---|
| R04 | lease TTL(180s)与 `max.poll.interval.ms`(900s)次序错配 | poll 跑在独立线程,该参数在所述场景里不起作用;整进程冻结时先触发的是 `session.timeout.ms`=45s,比 etcd 失租早 135 秒 |
| R08 | login 的 KickSessionOnGate/SendBindSessionToGate 缺空 `gate_instance_id` 守卫 | 是潜在加固点而非活 bug,且 login 并非唯一有此缺口的生产者 |

---

## 3. 审计确认**安全**的部分(别再重查)

- `node_uuid` 是每进程新生成的随机 boost uuid(`node.cpp:506`),不由 pod 名或 node_id 派生 —— 整套反僵尸机制的承重不变量,成立。
- 全部 C++ 生产者对空 instance id **fail-closed**:`battle_room_manager.cpp:60-64/134-140`、`player_battle.cpp:1190-1196`,宁可丢弃也不发无法过滤的命令。
- Go 共享推送路径四个入口全部 fail-closed(`go/shared/kafkautil/gate_push.go`);`BroadcastToPlayers` 按 (gate_id, instance_id) 分组而非只按 gate_id,两代 gate 不会被折进同一条命令。
- `enterscenelogic.go:110` 在最高频的 gate 生产者上拒绝空 `GateInstanceId`。
- gate 在两条路径上都把自己的 uuid 盖进 `SessionDetails`(`client_message_processor.cpp:121` 与 `:722`)。
- **node_id 全局唯一而非按 zone 唯一**:分配键 `MakeNodeAllocationPrefix` 刻意不带 zone(`etcd_manager.cpp:60-68`),所以 zone1 与 zone2 的 `scene-5` 不会撞车。`node_connector.cpp:43` 那句"只在 zone 内唯一"是过期注释。
- 正常 SIGTERM 停机次序正确:先 `StopConsumers()`(`node.cpp:994`)再释放 node_id(`:1069`),干净重启不会重叠。**只有冲突路径跳过这一步**(即 R05)。
- buff/skill 临时 id 不跨进程比较、不落库,安全。

---

## 4. 修复方案

> **落码状态(2026-09-08)**:R13(含反向面)/ R07 / R03 / R10 已落码,回归测试见
> `cpp/tests/routing_identity_test`(19 条,与 `bag_test` 同形)。判定逻辑刻意拆成三个纯头
> 供单测直接盯:`services/gate/session/system/session_identity_fence.h`(推送栅栏)、
> `engine/core/network/broadcast_target_codec.h`(广播的成对编解码,收编两侧实现)、
> `services/battle/settlement/settlement_outbox.h`(结算重投判定 + Redis key/Lua 契约)。
> **R13 的兼容位有到期日**:`target_player_id`/`player_id`/`player_list` 为空时收方放行并
> 只记一次 INFO;**灰度一个版本之后必须改为丢弃**,否则栅栏等于没加。

### 4.1 已在做:控制面主题改分区(修 R12,顺带 R05/R09)

见 [control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md)。
要点:`gate-cmd`/`scene-cmd` 固定 P 个分区(默认 256),节点按 `node_id % P` **直接指派**分区(不用消费组),
生产者投确定分区,过滤仍靠 `target_node_id` + `target_instance_id`。
**明确否决了"每个小区一套 Kafka"**:全国同服要求任意小区之间可玩,全局池的 battle/match 必须能触达
任意 gate/scene,控制面必须保持单一可达集群。读放大可接受:控制面消息量是每秒数百条量级。

> **以下 4.2–4.4 是当时的方案设计,现已全部落码**;实际落法与差异见第 2 节各条的「现状」列。

### 4.2 给 TCP 推送补身份(修 R13)

标准做法与 Kafka 的 broker epoch 同构:消息里带上目标的代次,收方不匹配即丢弃。
最省的落法是在 `NodeMessageHeader` 加 `target_player_id`(或 gate 代次 uuid),gate 在写 socket 前
校验该 session 当前确实属于这个玩家。加 `player_id` 比加 uuid 更好:它同时挡住 R14 的序号回绕
(同一代次内 session_id 复用),而 uuid 只挡跨代次。

### 4.3 结算改为可重投(修 R07)

结算不能是"发一次、丢了就算"。两种标准写法:
① **落库再投递**:battle 结算先写一条持久待结算记录(全局库),再发命令;scene 处理后销账;
未销账的由定时任务重投。
② **按玩家寻址而非按进程寻址**:结算发给 `player_id`,由 scene_manager/player_locator 解析当前
真正持有该玩家的进程,进程换了自动跟着换。
推荐 ①,因为它同时解决"目标进程此刻不存在"的情况。

### 4.4 其余

- R03:`scene_handler.cpp:582` 改走 `FindNodeEntityByZoneAndNodeId`,与 `gate_service_handler.cpp:133-136` 已修的那处对齐。
- R10:`BattleRouting` 在每次会话变更时刷新,或改为按 player_id 解析(与 4.3 ② 同源)。
- R06:Kafka 从 emptyDir 换 PVC(与今日 etcd 换 StatefulSet+PVC 同一理由)。
- R11:命令主题显式设保留期,不继承 broker 默认的 300s。
- R14:由 4.2 的 `player_id` 校验覆盖。

---

<a name="gap"></a>
## 5. 本次未覆盖(审计被 403 中断)

- **UC2 会话反查的完整路径**:`session_id` 位宽、`node_id > 32767` 时 15 位 node 段的 LOG_FATAL 边界、
  seq 回绕后已发给其它服务的 id 的命运,只由 R13/R14 部分覆盖。
- **UC4 的规模面**:routing 分配器在 10 万节点下的 etcd 负载、每个节点持有的 `ServiceNodeList` 快照体积、
  冷启动 CAS 争用、17 位 id 空间(131071)对 10 万节点 + 每日churn 的余量。
  已知的定性结论是:**先塌的是 Kafka 主题数量**(4.1 正在修),其次才是 etcd watch 扇出。

---

## 6. 与发号改造的关系

发号侧今天做了"最久未用 + 4 小时隔离期 + 水位 + 自 fence",寻址侧**刻意没做**同样的事,原因:
寻址只要求"同一时刻不重复",重启换号完全没关系;而发号要求"跨重启跨时间都不重复"。
给寻址加隔离期只会让 node_id 池提前耗尽,换不来任何东西 —— 寻址的正确解法是**代次校验**,不是隔离期。
这也是 Kafka 用 broker epoch、而不是用"broker id 冷却期"的原因。
