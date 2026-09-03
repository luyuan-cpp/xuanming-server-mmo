# 全服跨 Zone 匹配(Cross-Zone Matchmaking)

> **状态**: v1 — 2026-09-02
> **需求**: zone1 / zone2 …… 所有大区的玩家互相匹配(排队 / 切磋 / 观战),match 服务**标准水平扩展**(多实例、无单点),排队状态放 **Redis Cluster**,**容错不降级**(不设"退回单 zone"开关,故障由冗余与自愈吸收)。
> **审计深度**: 直接读 go/match、cpp 节点发现 / etcd 分配 / Kafka 路由、scene_manager 位置契约、部署脚本;所有结论带 file:line。
> **核心结论**: 快照战斗模型让跨 zone 匹配**在链路上已经通了**(玩家不搬家,只有快照进全局 battle 池;node_id 全局唯一所以 Kafka topic 不撞)。真正的缺口在 **Redis Cluster 就绪度**(SCAN / 多 key DEL / C++ hiredis 不支持集群)与 **实例崩溃后的票据自愈**。本文把这两块补成生产形态。

---

## 0. TL;DR

| 维度 | 现状(代码事实) | 本轮动作 |
|---|---|---|
| 排队池是否分 zone | ❌ 不分:`match:queue:{mode}:{config}` 全局 list,任意 zone 的 match 实例共享 | 保持全局;key 加 hash tag 以适配集群原子性 |
| match 多实例安全 | ✅ 每队列 SETNX 锁 + Lua 按持有者释放 + snowflake worker 由 etcd 分配 | 保持;补"匹配中票据"TTL 自愈 |
| 跨 zone 定位 scene 节点 | ✅ `player:{id}:location` 带 `zone_id`,watcher 看全 zone 前缀 | 不改 |
| battle 池 | ✅ 全局池不分 zone(`IsZoneScopedNodeType` 不含) | 不改 |
| Kafka 回流 topic(`gate-{id}` / `scene-{id}`) | ✅ node_id 按 `(node_type,node_id)` **全局 CAS 分配**,跨 zone 不撞 | 不改;文档纠正"node_id 仅 zone 内唯一"的过期注释 |
| gate 发现 match | ✅ MatchNodeService 非 zone-scoped,gate 连全 zone 的 match(随机路由) | 不改;纠正 yaml 注释 |
| Redis Cluster 就绪 | ❌ `SCAN match:queue:*` 在集群只扫一个分片;`DEL k1 k2` 跨 slot CROSSSLOT;C++ hiredis 无集群 | **双存储**:match 私有 key 进集群,跨运行时契约 key 留共享库;SCAN → 注册集;多 key → 同 slot 或拆分 |
| 实例崩溃中的票据 | ❌ `matched` 态票据 TTL 6h,实例挂了玩家 6h 不能再排 | `matched` 态 TTL 按组大小取最坏 gather 耗时(2 人 30s / 5 人 48s / 10 人 78s),票据写入全部带 ticket id 的 Lua CAS;**已冻结成员**另受 scene 侧 `battle:lock` 窗口(`BattleMaxDurationSeconds`+60s)约束,见 §5 |
| 多实例本机启动 | ❌ `MetricsListenAddr` 固定 :9170,第二实例绑定失败 | go_services.ps1 按实例/zone 派生 metrics 端口 |
| K8s | ❌ 无 match 部署清单;Redis 单副本 | 加 match Deployment(全局池,replicas≥2)+ Redis Cluster(3 主 3 从) |

---

## 1. 事实审计(为什么"链路已通")

### 1.1 玩家不搬家:快照模型绕开跨 zone 迁移的致命项

`docs/design/cross-zone-readiness-audit.md` 判定"玩家跨 zone 当前不可生产"(实体只 marshal 7 组件、bag/quest/mail 丢失、Kafka migrate 无 ACK)。**那是玩家实体跨 zone 搬家的问题**。回合制战斗的模型是:

- 玩家留在原 zone 的 scene 节点,`PrepareBattle` 只冻结 + 抽快照(`cpp/libs/services/scene/battle/system/player_battle.cpp`);
- 快照进全局 battle 节点(`cpp/nodes/battle/`),战斗零持久化;
- 结算以事件回流原 scene(`battle_room_manager.cpp:135` → `scene-{scene_node_id}`),由原 scene 的 `ApplySettlement` 应用。

跨 zone 匹配全程**没有任何玩家数据离开原 zone**,审计文档的所有致命项都不在这条链路上。

### 1.2 node_id 全局唯一 → Kafka topic 无 zone 也不撞

- Go:`go/match/internal/noderegistry/registry.go:60-63` —— `allocationKey` "是跨 zone 的全局占位 key —— 路径里不带 zone,因为两个 zone 的实例不可能同时拿到同一个 (node_type, node_id)";per-zone 的 `rpcPath` 只是发现路径。
- C++:`cpp/libs/engine/core/node/system/etcd/etcd_service.cpp:253` —— "Lost the race for this (node_type, node_id) globally — another zone …"。
- 压测实证:3 zone 6 个 gate 落成 `gate-group-{0..5}` 六个独立 group(`docs/design/stress-3zone-2026-05-23-postmortem.md:542`)。
- 因此 `node_kafka_command_handler.h:146` 的 `topicPrefix + "-" + node_id` 与 battle 出站的 `"gate-"+gate_node_id` / `"scene-"+scene_node_id` 全局无歧义。`node_connector.cpp:43` "node_id is only unique within a zone" 是防御性注释,与分配器实际行为不符,本轮在设计文档层面纠正,不改代码。

### 1.3 定位与发现已 zone-aware

- `proto/scene_manager/storage.proto:17-24` `PlayerLocation{scene_id,node_id,update_time,zone_id}`,写者 `go/scene_manager/internal/logic/changesceneutil.go:53-70`;
- match `preparePlayer`(`gather.go:190-236`)读位置后 `SceneNodes.EndpointOf(loc.ZoneId, loc.NodeId)`(`node_watcher.go:229`),watcher 订阅 `SceneNodeService.rpc/` 前缀覆盖 `/zone/N/...` 全部 zone;
- battle 池 `BattleNodes.PickRandom()` 全局;`BattleRouting.zone_id`(`battle_data.proto:18`)已随快照携带。
- gate 的 `CanConnectNodeTypeList` 含 MatchNodeService 且它**不在** `IsZoneScopedNodeType`(`node_util.cpp:108-122`),所以 gate 连的是**全 zone** 的 match 实例(uuid 去重,`node_connector.cpp:40-55`),按 NODE_MATCH 随机路由。`go/match/etc/match_service.yaml` 里"gate 按 MatchNodeService.rpc/zone/{zone} 发现本服务"的注释是错的,本轮纠正。

### 1.4 Redis 现状与集群障碍(逐条)

单实例 Redis 被所有 zone 共享(`deploy/docker-compose.yml:107`;K8s `manifests/infra/redis.yaml` replicas=1,`deploy/k8s/AGENTS.md:60` "All zones share one set of infra")。match 的 22 种 Redis 调用里:

| 调用 | 位置 | 集群下 |
|---|---|---|
| `Scan(match:queue:*)` | `matcher.go:52` | ❌ go-redis ClusterClient 对无 key 命令按随机 slot 路由(`osscluster.go:1839`),只扫到一个 master |
| `Del(challengeKey, challengeTargetKey)` | `challengelogic.go:136,229` | ❌ 两 key 不同 slot → CROSSSLOT |
| `Eval(releaseLockScript, [lockKey])` | `matcher.go:105` | ✅ 单 key |
| 其余(Get/Hmset/Hgetall/Lpush/Lpop/Lrem/Rpush/Llen/SetnxEx/Setex/Expire/Exists/Zadd/Zrem/Zcard/Zrange*/Zremrangebyscore) | 各处 | ✅ 全部单 key |

C++ 侧 `cpp/libs/engine/infra/storage/redis_client/redis_client.h` 基于 hiredis `redisContext`,无 MOVED/ASK 处理 —— scene 写的 `battle:lock:{player_id}`、玩家缓存都走这条,**C++ 不能直连集群**。

### 1.5 gate 的随机路由是唯一一道硬编码 zone 门(独立复核 2026-09-02,修正 §1.3 的措辞)

§1.3 说"gate 连全 zone 的 match 实例"只对了一半:gate **连接**了全 zone 的 match(registry 按 uuid 收全),
但**路由**客户端消息时两处随机选节点都硬过滤 `node.zone_id() == 本 zone`:

- `cpp/nodes/gate/handler/rpc/client_message_processor.cpp:50-74` `PickRandomNode`;
- `cpp/libs/engine/core/network/player_message_utils.cpp:267-287` `PickRandomNodeEntity`。

后果:只部署一个 zone_id=1 的 match 时,zone 2 的 gate 把候选全部剔除,JoinQueue/WatchBattle 直接
`kServiceUnavailable`("Node not found ... message id: 157")。这与 D2(battle 全局池)/ §5.4(match 全局池)矛盾,
是本轮唯一需要动 C++ 的点(D11)。

### 1.6 实例崩溃窗口

`popGroup` 弹出即把票据推进 `matched`(`matcher.go:122-126`),票据 TTL 沿用 `TicketTTLSeconds=21600`。若该实例在 gather 期间崩溃,票据留在 `matched` 6 小时,玩家 `JoinQueue` 一直被 `ErrAlreadyQueued` 拒绝,`CancelQueue` 也"太迟" —— 这是"容错不降级"要求下必须补的洞。

---

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **匹配池全局、不分 zone、不设 zone 优先级、不设降级开关** | 用户明确;现状 key 已全局;开关是"降级",与"容错不降级"相悖 |
| D2 | **双存储**:match 私有 key(`match:*` / `challenge:*` / `spectate:*`)→ `MatchRedis`(可配 `Type: cluster`);跨运行时契约 key(`player:{id}:location` / `player:session:{id}` / `battle:lock:{id}`)→ `SharedRedis`(既有共享库,match 只读) | 契约 key 的写者是 scene_manager / player_locator / **C++ scene**,C++ 无集群客户端;按有界上下文拆存储是标准做法,且不动任何其他服务。`MatchRedis` 缺省时回落到 `Redis`(向后兼容,本地单库照跑) |
| D3 | **队列相关 key 共用 hash tag `{mq}` 同 slot**:`match:{mq}:index`(注册集)、`match:{mq}:queue:<mode>:<config>`、`match:{mq}:lock:<mode>:<config>`;入队用 Lua 原子 `SADD index + RPUSH queue` | 用注册集取代 SCAN;同 slot 让"注册集 ⊆ 队列集合"成为不变量(主从异步复制的故障切换不会让队列脱离注册集);队列操作 QPS 极低,单 slot 不是瓶颈(§6) |
| D4 | 票据 `match:ticket:{pid}` 按玩家分布;新增 `zone_id`、`queue_key` 字段 | zone 用于可观测性与对局日志;`queue_key` 让取消 / 回队首不再重算 key(格式演进安全) |
| D5 | **`matched` 态票据短 TTL**:弹组时按组大小计算 `max(MatchedTicketTTLSeconds, n×(removeObserverTimeout+prepareTimeout) + createTimeout + rollbackTimeout + 10s)`(2 人 30s / 5 人 48s / 10 人 78s;超时常量取自 gather.go,不另写数字);gather 失败进补偿前对幸存者按 `prepared×3+10` 续期;**票据所有写入带 ticket id 的 Lua CAS**(matched / ready / 回队首 / 失败删票 / 取消删票五处):票据过期被玩家重排后的新票据不会被迟到的旧 gather 写脏;CAS 失败的成员不进 gather(视为已取消出局);回队首**先 CAS 后 LPUSH**,LPUSH 失败则 CAS 删票不留反向孤儿;JoinQueue 遇 queued 票据先 `LPOS` 探测队列,不在则原地补入队(票据/队列分属两个 master 的故障切换组合丢失可自愈);JoinQueue 遇 **ready** 票据且 `battle:lock` 已不存在(战斗已结束)→ CAS 删票放行(2026-09-02 冒烟复跑:上一局 16s 前结束,ready 票据 TTL 60s 未到把"打完立刻再排"拒了一分钟) | 三视角复审(2026-09-02)逐条推翻了"固定 30s"与"先 LPUSH 后 CAS":gather 链路是 清退×n → Prepare×n → Create → (失败)Destroy+解冻×n,固定 30s 在 5 人组正常路径就会过期;scene 侧冻结由既有 deadline reaper 兜底(见 §5) |
| D5b | **`shared/snowflakealloc` 接管原 worker id 的条件 = (旧 lease 已死)或(前任留下 `released` 标记)**:`Close()` 不撤销 lease(既有契约:≤TTL 的隔离窗吸收跨机时钟偏斜)但会在同一 lease 下写 `<nodeKey>/released` 标记 → 同主机优雅重启仍在 TTL 内复用原 id;lease 活着且无标记(仍在运行 / 刚崩溃)则**不抢**,改用派生键 `hostname#<leaseID>` 继续分配新 id(不能停在原键上:双 key CAS 要求 nodeKey 不存在,会一直失败到 ctx 超时 —— kill 后 60s 内重启也会撞上,本地实测 scene_manager 就这样 `claim txn: context deadline exceeded` 起不来);KeepAlive 句柄按 idKey 里实际写入的亲和键做所有权 watch;match 另把亲和键改为 `hostname#ListenOn` | 原实现按 hostname 亲和"复用"且不校验旧持有者存活(`allocator.go:144-167`)。**本地实证(2026-09-02 11:43)**:按 `-Zone 2` 起第二个 login,它抢走 worker 0 并把 nodeKey 挂到自己的 lease,zone1 login 在 keepalive 里检测到 ownership lost → fence + 退出 → 网关 assign-gate 全线 UNAVAILABLE。这是共享库缺陷,login / scene_manager / player_locator / guild / friend / match 全部受影响,所以修在库里而不是各服务绕开;K8s 每 Pod 独立 hostname 不触发,但不能把正确性押在部署形态上 |
| D5c | `queue_depth` 只由持锁实例上报;队列从注册集剔除时显式归零 | 多实例各自上报同一队列 → 看板 sum 放大 N 倍;队列弹空后 gauge 永不归零 → 积压告警误报 |
| D6 | JoinQueue 读位置取 zone;位置缺失直接拒(新错误码 `ErrNotInScene=8`) | 无位置的玩家 gather 必败(`no_location`),提前拒绝比进队再失败更省 |
| D7 | 挑战记录的双 key `DEL` 拆成两条单 key `DEL` | 两 key 身份不同不能同 tag;非原子可接受(target 锁有 TTL,残留只影响 60s 内重复挑战) |
| D8 | metrics:`queue_depth` 不加 zone 标签(队列本就不分 zone);新增 `gather_zone_mix_total{mode,mix=single|cross}` | 低基数,直接回答"跨 zone 对局占比" |
| D9 | 客户端零改动 | 协议未变;对手 zone 展示留二期(要动 `BattleActorState` + C# regen) |
| D10 | 部署:match 作为**全局池**部署一次(K8s 放 `mmorpg-infra`,replicas≥2);本地 `dev-start-zones` 继续每 zone 起一份(等价于多实例) | 与 battle 同形态;zone 标签只影响 etcd 注册路径 |
| D11 | **gate 随机路由对全局池类型豁免 zone 过滤**:新增 `NodeUtils::IsGlobalPoolNodeType`(仅 BattleNodeService / MatchNodeService),`PickRandomNode` 与 `PickRandomNodeEntity` 对这两类不比对 zone;其余类型(friend/guild/chat/login…)路由语义不变 | §1.5 的硬门与全局池设计矛盾;只豁免明确的全局池类型,不扩大爆炸半径。任一 zone 的 match 实例挂了,该 zone 的 gate 自动落到其他 zone 的实例 —— 这是"容错不降级"在入口层的体现 |
| D12 | **SharedRedis 必须是全服单一实例**(不是 per-zone/per-region 分片):`player:{id}:location`(scene_manager)、`player:session:{id}`(player_locator)、`battle:lock:{id}`(C++ scene)三类契约 key 的写者都以它为准 | 当前本地/K8s 都是一个共享 Redis,满足;data_service 有 Regions→Redis 分片设计草案,若将来采纳,match 需要按 region 路由 SharedRedis 读取(二期,§10)。本轮在 match 启动日志打印 SharedRedis 地址并在文档钉死这条不变量 |
| D13 | **`PrepareBattleRequest.prepare_deadline_ms` = now + 本组 matched TTL**(`matchedTicketTTLFor(组人数)`,2 人 30s / 5 人 48s / 10 人 78s);`deadline_ms` 语义不变(战斗作废期限) | §5 "已冻结成员受 `battle:lock` 窗口约束"的二期项落地(match 侧):备战期限与票据 matched TTL 同一窗口,弹组实例在 gather 中崩溃时,未冻结者靠票据过期、已冻结者靠 scene 在 PREPARING 态按该期限解冻并收窄锁 EX,两边同一时刻放行。scene 侧读取该字段的实现见 turn-based-battle-server.md 对应条目;旧版 scene 忽略该字段(0/未知 = 沿用 `deadline_ms`),向后兼容 |
| D14 | **配表指纹比对**:scene 在 `PrepareBattleResponse.table_fingerprint` 回报本节点六张战斗表内容指纹(解析后确定性序列化的 sha256 前 16 字节 hex),match 收齐全员后比对;**全员非空且两两一致**才写进 `CreateBattleRequest.table_fingerprint`(battle 再与自身核对)。不一致或部分为空按 `TableFingerprintMode`(`off`/`warn`/`enforce`,默认 `warn`):`off` 不比对不透传;`warn` 只记 Error 日志 + `match_table_fingerprint_mismatch_total{mode}`,照常开局、不透传;`enforce` 视为 prepare 失败走既有补偿(已冻结者逐个解冻,肇事者出局删票,幸存者回队首),肇事者 = 与多数派不一致者(多数派只在非空指纹里选,平票取弹出序靠前者;全员皆空记第一位) | §10 "配表版本一致性"落地:灰度/回滚会让同一场战斗按不同数值表入场,快照与判定各按各的表算。灰度期用 `warn` 看指标确认各 zone 表版本对齐后再切 `enforce`;`enforce` 下不回报指纹的旧版 scene 会被一律拒绝,所以它不是默认值 |

---

## 3. 架构与数据流(2 zone 示例)

```
zone1 客户端 ──TCP──▶ z1 gate ─┐                        ┌─▶ z1 scene ──PrepareBattle──▶ 快照
                               │ NODE_MATCH 随机路由     │
zone2 客户端 ──TCP──▶ z2 gate ─┼──gRPC──▶ match×N ──────┤
                               │      (无状态,任意实例)  └─▶ z2 scene ──PrepareBattle──▶ 快照
                               │             │
                               │             ├─ MatchRedis(Cluster):{mq} 队列/注册集/锁,票据,挑战,观战索引
                               │             └─ SharedRedis:player:location / player:session / battle:lock(只读)
                               │             │
                               │             ▼ CreateBattle(全局 battle 池随机)
                               │        battle 节点 ──Kafka gate-{id}──▶ 任意 zone 的 gate(BindBattle / S2C)
                               │                    ──Kafka scene-{id}─▶ 原 zone 的 scene(结算回流)
```

JoinQueue(任一 zone 玩家)→ 票据(含 zone)→ Lua 原子入队 → matcher(持锁实例)LPOP 凑组(**不看 zone**)→ gather 逐人按 `location.zone_id` 找对应 zone 的 scene 冻结抽快照 → CreateBattle → BindBattle 经各自 gate 的 topic → 战斗 → 结算回各自 scene。

---

## 4. Redis 存储契约(修订 turn-based-battle-server.md §6 / §10.4)

### 4.1 MatchRedis(match 独占,可集群)

| key | 类型 | 语义 | slot |
|---|---|---|---|
| `match:{mq}:index` | set | 活跃队列 key 注册集(SCAN 的替代) | `{mq}` |
| `match:{mq}:queue:<mode>:<config>` | list | 排队玩家 id,等待序(队首最久) | `{mq}` |
| `match:{mq}:rank:<mode>:<config>` | zset | 同一队列的评分镜像:member=player_id,score=入队时评分(§11) | `{mq}` |
| `match:{mq}:lock:<mode>:<config>` | string | matcher 凑单临界区(SETNX,值=实例 uuid) | `{mq}` |
| `match:ticket:<player_id>` | hash | 票据:`ticket/mode/config/state/enqueued_at_ms/battle_id/zone_id/queue_key/rating` | 按玩家 |
| `match:rating:<player_id>` | hash | 玩家评分 `rating/games/updated_at_ms`,无 TTL,默认 1500(§11) | 按玩家 |
| `match:rating:applied:<battle_id>` | string | 对局结果已入账标记,TTL 7d(§11 幂等) | 按对局 |
| `challenge:<id>` / `challenge:target:<pid>` | hash / string | 切磋记录 / 目标互斥 | 各自 |
| `spectate:battles:active` / `spectate:battle:<bid>` / `spectate:watching:<pid>` | zset / string / string | 观战索引 | 各自 |

入队 Lua(KEYS=[index, queue, rank],ARGV=[queueKey, playerId, rating]):`SADD KEYS[1] ARGV[1]; ZADD KEYS[3] ARGV[3] ARGV[2]; RPUSH KEYS[2] ARGV[2]`。
注册集剔除:matcher 发现 `LLEN==0 && !EXISTS queue` 时先 `DEL rank`(镜像孤儿)再 `SREM`(懒清理,幂等)。
list 与 rank 的全部 Lua 见 §11.3。

**评分 key 无 TTL 的前提**:`match:rating:*` 不设 TTL,按玩家分布(不带 `{mq}`)。`MatchRedis` 缺省回落到共享库
(§4.2)时,共享库若配了 `allkeys-lfu` 之类的淘汰策略,评分可能被淘汰回落 1500 —— **生产必须配独立 MatchRedis**
(`noeviction` 或足够内存)。

### 4.2 SharedRedis(既有共享库,match 只读)

| key | 写者 | match 用途 |
|---|---|---|
| `player:<id>:location` | scene_manager | JoinQueue 取 zone;gather 定位 scene 节点;观战定位 |
| `player:session:<id>` | player_locator | 挑战推送 / 观战路由取 gate |
| `battle:lock:<id>` | C++ scene | 咨询性"是否在战斗中" |

`MatchRedis` 未配置时两者同一实例(本地开发形态)。**这只适合本地**:评分 `match:rating:*` 无 TTL,共享库
配了 `allkeys-*` 淘汰策略(本地 `deploy/docker-compose.yml:121` 就是 `allkeys-lfu`),评分会被当普通缓存淘汰、
玩家回落 1500;生产必须配独立 MatchRedis(§11.2)。

**SharedRedis 的其他读者(决定它能否集群化的完整约束)**:C++ scene 组队跟随读 `player:{id}:location`
(`player_scene.cpp:86-96`,hiredis 裸连接);player_locator / scene_manager / login 等在同一库上的跨 slot Lua、
MULTI、MGET(§10.0)。所以 SharedRedis **必须保持单实例(或主从哨兵)形态**,直到 §10.0 清单修完。

---

## 5. 容错矩阵(不降级的含义)

| 故障 | 行为 | 保证 |
|---|---|---|
| 一个 match 实例崩溃(空闲) | gate 随机路由到其余实例;matcher 锁 TTL(10s)到期后其他实例接管 | 无感 |
| match 实例在 gather 中崩溃 | **未冻结成员**:票据 `matched` 按组大小 TTL(30/48/78s)过期 → 可重排;**已冻结成员**:scene 侧 `battle:lock` EX = `BattleMaxDurationSeconds`+60s(`player_battle.cpp:359-376`),JoinQueue 对锁 fail-closed,要等 reaper 按 `InBattleComp.deadline_ms` 解冻(默认 ≈5-6 分钟) | 未冻结者 ≤78s 自愈;已冻结者受 C++ 锁窗口约束 —— 这是既有 scene 契约,不是 match 能单方面缩短的(match 侧"遇锁就调 CancelBattlePrepare"被复审判定不安全:scene 只比对 battle_id 不看状态,会把正在打的玩家解冻)。**D13 已落地 match 侧**:PrepareBattle 带 `prepare_deadline_ms`(= now + 本组 matched TTL),scene 在 PREPARING 态按它解冻并缩短锁 EX 后,已冻结者与未冻结者同一窗口自愈 |
| 某 zone 的 scene 配表版本漂移(灰度/回滚) | `PrepareBattleResponse.table_fingerprint` 不一致:`warn`(默认)记日志 + `match_table_fingerprint_mismatch_total` 照常开局;`enforce` 拒开局走 prepare_failed 同款补偿,与多数派不一致者出局,幸存者回队首(D14) | 指标可告警;`enforce` 下不产生"两套数值表"的对局 |
| Redis Cluster 故障切换丢票据/队列组合 | 票据(按玩家 slot)与队列(`{mq}` slot)分属两个 master,异步复制可各自丢最近写:队列丢票据在 → JoinQueue 用 `LPOS` 探测后补入队;票据丢队列在 → popGroup 校验票据缺失静默丢弃,玩家可重排 | 无 6h 卡死 |
| 滚动升级(旧 key 格式在途) | 旧 `match:queue:{mode}:{config}`(无 hash tag)不在注册集:matcher 启动及每 60s 在 SharedRedis `SCAN` 一次把旧队列成员搬进新队列;旧票据无 `queue_key`,CancelQueue 对新旧两个 key 各 `LREM` 一次 | 在途排队者不丢;升级窗口过后可删除迁移逻辑(§10) |
| Redis Cluster 某 master 故障切换 | go-redis 跟随 MOVED/ASK 重刷 slot 表;异步复制可能丢最近写:①队列与注册集同 slot 同 Lua,要丢一起丢;②matcher 锁丢失 → 两实例同时凑单,但 `LPOP` 在单 master 上原子,同一玩家不会被弹两次,最坏是一轮凑不满回队首 | 数据不脱契约;无双开战 |
| SharedRedis 故障 | JoinQueue 读锁失败按"有锁"处理(fail-closed,既有);gather `no_location` 失败走补偿 | 拒绝新排队,不产生错误对局 |
| etcd 失租(snowflake) | 既有:fence 发号器 → flush → 退出重启 | 不撞 id |
| 某 zone 的 scene 全挂 | 该 zone 玩家 gather `prepare_failed` → 肇事者出局删票、幸存者回队首(既有补偿矩阵) | 其他 zone 玩家继续匹配 |
| battle 池空 | `no_battle_node` → 补偿;告警指标 | 排队不丢 |

---

## 6. 水平扩展模型与瓶颈

- **入口**:gate 在全 zone 的 match 实例间随机路由,实例无状态 → 加实例即扩容。
- **凑单**:每个 `(mode,config)` 队列一把锁,同一队列同时只有一个实例弹组,但弹组只是 `LPOP` 级操作(微秒级),gather(3–5 跳 RPC)在弹组实例的 goroutine 里并行,不占锁 → 凑单串行不是瓶颈。
- **单 slot**:`{mq}` 把所有队列压到一个 master。每次 JoinQueue 一条 Lua、matcher 每 500ms 每队列一次 `LLEN` + 若干 `LPOP`。按 1 万玩家/分钟入队算,单 master 负载 < 200 QPS,余量三个数量级;票据 / 挑战 / 观战按玩家分布,吃满集群分片。
- **snowflake**:worker id 17-bit 布局,由 etcd `/match` 前缀独立分配,实例数上限远高于实际需要。

---

## 7. 部署形态

- **本地**:`deploy/docker-compose.yml` 新增 profile `redis-cluster`(6 节点 `redis:7.2`,`--cluster-announce-ip 127.0.0.1` 让宿主机进程能跟随 MOVED;init 容器 `redis-cli --cluster create --cluster-replicas 1`)。
  实现细节(2026-09-02 实测):六节点用 `network_mode: service:redis-cluster-0` 共享网络命名空间,端口 7000-7005/17000-17005 全挂在 redis-cluster-0 上 —— Docker Desktop for Windows 无 host 网络,独立网络下 announce 127.0.0.1 建群必败;共享命名空间让容器内与宿主机看到的 `127.0.0.1:700N` 一致。**只对宿主机进程有效**,容器内进程(未来容器化的 match)不能用这套 profile 直连。`go/match/etc/match_service.yaml` 提供注释掉的 `MatchRedis` 集群样例;`dev-start-zones` 每 zone 起一份 match(等价多实例),`go_services.ps1` 派生 `MetricsListenAddr` 端口。
- **K8s**:`manifests/infra/redis-match-cluster.yaml`(StatefulSet 6 副本 + headless Service + 建群 Job);`manifests/go-svc/match.yaml`(Deployment replicas 2,部署到 `mmorpg-infra`,全局池;ConfigMap 由 `k8s_deploy.ps1` 的 `$GoSvcCatalogue` 新条目生成,`MatchRedis.Host` 指向 `redis-match-cluster-{0..5}.redis-match-cluster.mmorpg-infra:6379`)。

---

## 8. 变更清单

**go/match**
- `internal/config/config.go`:`MatchRedis redis.RedisConf`(optional)、`MatchedTicketTTLSeconds`(default 30);删 ZoneId 错误注释。
- `internal/svc/servicecontext.go`:`MatchRedis` / `SharedRedis` 两句柄(缺省同源);保留 `Redis` 字段作为 `SharedRedis` 别名一版,避免外部引用断裂。
- `internal/logic/keys.go`:`{mq}` 三类 key、注册集 key、契约 key 分组注释。
- `internal/logic/queue.go`:票据加 `ZoneId`/`QueueKey`;`enqueueAtomic`(Lua);`matched` 短 TTL;`requeueFront` 恢复长 TTL。
- `internal/logic/joinqueuelogic.go`:读位置取 zone,缺失拒(`ErrNotInScene`);Lua 入队。
- `internal/logic/matcher.go`:注册集遍历替代 SCAN;空队列剔除;锁 key 同 slot;**battle 池为空则暂停凑单**
  (否则 gather 以 no_battle_node 秒败 → 回队首 → 500ms 后再弹的热循环,本地实测 battle 端口撞车未注册时同一对
  玩家被反复"凑单成功";告警限频 10s,`nobattle_guard_test.go`)。
- `internal/logic/cancelqueuelogic.go`:用票据 `QueueKey` 出队。
- `internal/logic/gather.go`:记录组内 zone 组成(日志 + `gather_zone_mix_total`);`PrepareBattleRequest.prepare_deadline_ms`(D13);收集 `PrepareBattleResponse.table_fingerprint`、`checkTableFingerprints` 按 `TableFingerprintMode` 比对并透传 `CreateBattleRequest.table_fingerprint`(D14,新 gather outcome `fingerprint_mismatch`);scene / battle 四个 gRPC 抽成包级函数变量 `prepareBattleFn` / `cancelBattlePrepareFn` / `createBattleFn` / `destroyBattleFn`,单测可 fake。
- `internal/config/config.go`:`TableFingerprintMode`(`off|warn|enforce`,默认 `warn`)。
- `internal/metrics/metrics.go`:`table_fingerprint_mismatch_total{mode}`。
- `internal/discovery/node_watcher.go`:`NodeWatcher.Upsert`(etcd PUT 落库路径抽出,测试直接灌节点)。
- `internal/logic/challengelogic.go`:双 key DEL 拆分。
- 所有契约 key 读取切到 `SharedRedis`;match 私有 key 切到 `MatchRedis`。
- `internal/metrics/metrics.go`:`gather_zone_mix_total`。
- `internal/constants`:`ErrNotInScene = 8`。
- 测试(miniredis):注册集 / Lua 入队 / 跨 zone 混编 / matched TTL 公式与 5v5 最坏耗时 / 票据 CAS(过期重排后旧 gather 写不脏)/ 回队首先 CAS 后入队(hook 注入交错)/ 弹出后取消者不进 gather / queue_depth 只由持锁者上报并归零 / 取消用 QueueKey 与旧 key 双 LREM / 旧队列迁移 / 挑战 DEL 拆分 / **hash slot 单测**(自实现 CRC16-XMODEM,断言 `{mq}` 三类 key 同 slot、票据/挑战/观战 key 不带 tag)/ **gather 真跑到 CreateBattle**(`gather_fingerprint_test.go`,fake 四个 gRPC):一致指纹透传 + `prepare_deadline_ms` = now + matched TTL 且短于 `deadline_ms`;warn 不一致 / 部分为空照常开局不透传并计数;enforce 不一致 → 不建局、已冻结者全解冻、肇事者删票、幸存者回队首;off 不比对不透传;多数派 / 肇事者选择。
- **升级说明**:先在同源存储上完成二进制升级并排空队列,再切 `MatchRedis` 到独立集群 —— 混跑窗口内旧票据留在 SharedRedis、新实例只读 MatchRedis,同一玩家两边状态不一致。
- **评分匹配(§11,2026-09-03)**:`internal/logic/rating.go`(评分存取 / 容差曲线 / Elo 入账 / 5v5 蛇形分队)、
  `internal/kafka/result_consumer.go`(`match-results` 消费者)、`keys.go`(`rank` / `rating` / `applied` key)、
  `queue.go`(入队 / 回队首 / 摘出 / 快照 Lua 同时维护 list 与 ZSET;票据 `rating` 字段)、`matcher.go`(锚点 + 容差
  弹组重写;剔除空队列顺手清镜像孤儿;旧队列迁移写镜像)、`joinqueuelogic.go`(读评分)、`cancelqueuelogic.go`
  (list+ZSET 原子出队)、`gather.go`(`teamAssignment`)、`config.go` / yaml(`RatingEnabled` / `ResultTopic*` /
  `RatingTolerance*`)、`metrics.go`(三个新指标)、`match_service.go`(消费者装配);测试 `rating_match_test.go`。

**部署与脚本**
- `deploy/docker-compose.yml`:`redis-cluster` profile。
- `deploy/k8s/manifests/infra/redis-match-cluster.yaml`、`deploy/k8s/manifests/go-svc/match.yaml`、`tools/scripts/k8s_deploy.ps1` 目录条目。
- `tools/scripts/go_services.ps1`:`MetricsListenAddr` 派生。
- `bin/etc/base_deploy_config.yaml` / `go/match/etc/match_service.yaml`:注释纠正 + 新配置项。

**robot**
- `battle-smoke` 增加 `cross_zone` 子模式:A 登 zone_a、B 登 zone_b,双双 `JoinQueue(1V1)`,断言同 `battle_id`,自动战斗到终局,输出 `CROSS_ZONE_MATCH_OK`。

**文档**
- 本文;`turn-based-battle-server.md` 新增 §16 指向本文并修订 §5.4/§6/§8;`PROGRESS.md` 追加条目。

---

## 9. 验证清单

1. `go/match`:`go build ./... && go vet ./... && go test ./...` 全绿;
2. 本地单库形态(不配 `MatchRedis`)行为与改前一致:`battle-smoke` 原模式 `BATTLE_SMOKE_OK`;
3. 本地集群形态:`docker compose --profile redis-cluster up` + `MatchRedis.Type: cluster`,`dev-start-zones -Zones 1,2` 起两 zone(match 各一份 = 双实例),robot `battle-smoke` `cross_zone: true` 输出 `CROSS_ZONE_MATCH_OK`,match 日志可见 `zone_mix=cross`;
4. 容错:杀掉持锁的 match 实例,另一实例 10s 内接管凑单;杀掉 gather 中的实例,**未冻结**成员在组大小对应的 TTL 内可重新 JoinQueue,**已冻结**成员按 §5 受 `battle:lock` 窗口约束(默认 ≈5-6 分钟,既有 scene 契约);
5. 集群:`redis-cli --cluster check` 无 CROSSSLOT 错误日志;`match_queue_depth` 指标随入队变化(证明注册集生效)。

---

### 9.1 执行结果(2026-09-02,本机双 zone)

| 项 | 结果 |
|---|---|
| go/match 单测 | 35 个全绿(miniredis;`-race` 因 CGO_ENABLED=0 未跑);`shared/snowflakealloc` 集成测试 21/21(本地 etcd) |
| 单库形态跨区冒烟 | ✅ 11:49 `CROSS_ZONE_MATCH_OK`:A@zone1(gate 10000)+ B@zone2(gate 10010)→ zone1 唯一 match → 0.25s 凑单 → `zone_mix=cross` → 建局 71ms → 24 回合打完,同 battle_id 同结局 |
| Redis Cluster 形态 | ✅ 11:52 `CROSS_ZONE_MATCH_OK`;`MatchRedis` 六节点集群配置生效,票据落在集群(7000 主 / 7005 从),共享库零新键,`--cluster check` 全覆盖;`gather_zone_mix_total{cross}=1` |
| 故障切换(入口层) | ✅ 11:56 起 zone2 match(node 2)、杀 zone1 match:**两个 zone 的 gate 都把 JoinQueue 路由到 zone2 注册的 match**,凑单/建局 52ms(D11)。旧实例 etcd 键随 lease(60s)消失是切换检测时间 |
| 复跑暴露的既有缺陷 | ①上一局阵亡玩家(0 血)在离线结算登录补应用后未复活,带 0 血再入队 → 引擎开局即判负。修:结算阵亡即基础复活 + `PrepareBattle` 拒绝 0 血(`turn-based-battle-server.md` §15.4);②上一局 ready 票据(TTL 60s)拒绝"打完立刻再排" → 无 battle:lock 时 CAS 删票放行;③robot 把登录补推的上一局 `BattleEndS2C` 当新局结束 → 按 battle_id 过滤 |
| 连续复跑 ×2(修后) | ✅ 12:17 两次 `CROSS_ZONE_MATCH_OK`(18 / 8 回合,间隔 30s),双 match 实例(zone1 node 3 / zone2 node 4)均在线 |
| K8s 契约测试 | 26/26(`service_discovery_prefixes` 补齐后) |
| 二期(D13/D14/§11)运行时复验(2026-09-03 11:27,新版 scene/battle/gate + 双 match) | ✅ `CROSS_ZONE_MATCH_OK battle_id=64436612657872896 zone_a=1 zone_b=2` 21 回合(49s)。scene 两侧"备战冻结完成(deadline_ms/prepare_deadline_ms)"→ 开局 <1s "战斗确认 PREPARING->FIGHTING",battle 每 10s `BattleConfirmedEvent 周期补发` 被 scene 幂等忽略;z1/z2 四 scene + battle 指纹同为 `1fd045595a6c22488723ad7bea957a3c`,battle `mode=warn`;match 建 `match-results`(3 分区/7d)并消费,终局 `评分更新 1500→1484/1516`、`对局入账 rounds=21`,集群键 `match:rating:{pid}`(hash:rating/updated_at_ms/recent_battles/games)+ `match:rating:applied:{battle}` 幂等标记落在 MatchRedis(7000-7005),共享库零 match 键;败方结算复活满血。引擎单测 50/50(含演出数据 3 个) |

## 10. 非目标 / 后续

### 10.0 明确不在本轮:共享 Redis(SharedRedis)不切集群

七 agent 审计(2026-09-02)列出的下列 Cluster 阻塞项全部属于**其他服务在共享库上的用法**,双存储设计正是为了
绕开它们。在这些项修完之前,**共享库禁止切 Redis Cluster**(只有 MatchRedis 可以):

| 服务 | 阻塞项 |
|---|---|
| C++ scene | hiredis 裸连接无 MOVED/ASK;存盘 Lua 访问未声明的全局 key `dirty_keys_set`(`redis_client.h:18-22,461-475`)|
| player_locator | 8 个 Lua 把 per-player key 与全局租约容器原子绑定(CROSSSLOT);`claimExpiredLeasesScript` 脚本内拼 key |
| login | 排队/token 4 处跨 slot MULTI/EXEC;gate 排空查询跨 slot MGET |
| scene_manager | 场景生命周期 Lua 7 key/3N key 跨 slot |
| guild / friend | 非 0 号 DB(Cluster 只有 DB0);榜单重建跨 slot TxPipelined + RENAME;cache generation-CAS 2 key |
| db | Kafka 重试三队列 RPOPLPUSH 跨 slot |
| 运维 | `FLUSHALL` 在 Cluster 只清一个节点(CLAUDE.md §6.2 压测口径要改)|

与跨 zone **匹配**无关但被审计顺带指出的既有问题(不在本轮):玩家实体跨 zone 迁移的 `ErrUnsafeCrossNodeHandoff`
门、`player:zone:{id}` home zone 无生产写入方、存盘按进程 zone 而非 home zone 路由、同 zone 多 scene 的
player_migrate 目标判据(`scene_info.guid` uint32)、SceneManager 部署粒度 zone/职责全局的错位。这些都在
"玩家搬家"链路上,快照战斗不经过它们。

- 同 zone 优先 / 延迟加权匹配(用户明确不要);
- 客户端展示对手大区(需 `BattleActorState` 加 zone + C# regen);
- C++ 侧接 hiredis-cluster(本轮用双存储绕开;若未来 SharedRedis 也要集群化再做);
- gather 中实例崩溃时已冻结成员的 `battle:lock` 窗口 = `BattleMaxDurationSeconds`+60s(既有 scene 契约):**match 侧已落地 D13**(`PrepareBattleRequest.prepare_deadline_ms` = now + 本组 matched TTL);scene 在 PREPARING 态按它解冻并缩短锁 EX、CreateBattle 成功后由 battle 经 Kafka 通知 scene 升级为正式 deadline 属 scene / battle 侧工作;
- 旧队列迁移逻辑(`migrateLegacyQueues`,每 60s 对 SharedRedis `SCAN match:queue:*`)只服务升级窗口,下一版本删除;
- battle 节点的 K8s 清单(本轮只补 match;battle 缺清单是既有状态);
- **配表版本一致性**:各 zone 的 scene 用本节点的表算快照、battle 用自己的六张表判定;灰度/回滚会让同一场
  战斗按不同数值表入场。**match 侧已落地 D14**(收集 `PrepareBattleResponse.table_fingerprint`,全员一致才透传
  `CreateBattleRequest.table_fingerprint`,`TableFingerprintMode` 决定不一致时告警还是拒开局);scene 侧算指纹、
  battle 侧与自身指纹核对属各自服务的工作;
- **多实例配置漂移**:`PveTeamSizeByConfigId` / `BattleMaxDurationSeconds` 各实例取本地 yaml,漂移会让同一队列
  按不同人数弹人(二期:凑满人数改查 DungeonTable,已在 §5.4 记为权威来源);
- **对局质量维度**:~~纯 FIFO 无 MMR/等级/战力~~ → **已落地评分匹配(§11,2026-09-03)**:Elo 评分 + 等待时间放宽的
  容差匹配 + 5v5 蛇形分队;等级 / 战力维度与跨 zone 新老区合流的平衡度仍不在本轮(产品决策;用户明确不设 zone 优先级)。

---

## 11. 评分匹配(MMR)+ 等待时间放宽(2026-09-03)

> 任务 B。§10 末条"对局质量维度:纯 FIFO 无 MMR"的落地。只改 `go/match/`;proto 侧
> `contracts/kafka.BattleResultEvent` 与 C++ battle 的 `match-results` 生产者已先行落地。

### 11.1 决策

| # | 决策 | 理由 |
|---|---|---|
| D16 | **评分存 MatchRedis 私有 key `match:rating:{player_id}`**(hash `rating/games/updated_at_ms`,无 TTL,默认 1500,按玩家 slot,不带 `{mq}`) | 评分是 match 的有界上下文数据,不是跨运行时契约;按玩家分布吃满集群分片。无 TTL 意味着**生产必须配独立 MatchRedis**(`allkeys-*` 淘汰会把评分当缓存淘汰,§4.2) |
| D17 | **结果回流走 Kafka `match-results`**(消费组 `match-rating`,key=battle_id,payload 直接是 `BattleResultEvent`),只对 PVP 队列模式(1V1 / 5V5)做 Elo:K=32,队伍用平均分算期望,平局 0.5,同队每人同一增量;PVE / 切磋忽略 | battle 零持久化,结果只在结算时刻存在一次;Kafka 是仓库既有的节点间回流通道。切磋是点名局、PVE 对手是怪物,计分没有意义 |
| D18 | **幂等 = 每局 `match:rating:applied:{battle_id}` SETNX(TTL 7d),标记先于写分** | Kafka at-least-once(rebalance / 提交前崩溃)会重投;标记后崩溃丢一局的分(at-most-once)比同一局算两次可接受 —— 评分是软数据 |
| D19 | **队列在 list 之外增加同 slot 的评分镜像 ZSET `match:{mq}:rank:{mode}:{config}`**,每条改 list 的 Lua 同时改 ZSET(§11.3);评分在 JoinQueue 时读出记进票据并作为 ARGV 传入 | list 表达等待序、ZSET 表达评分,两者同 slot 才能一条 Lua 原子维护;评分 key 不同 slot,Lua 里不能 GET |
| D20 | **锚点 + 容差**:锚点按等待序逐个尝试(队首优先),容差 `tol = min(Max, Base + floor(wait/StepSeconds) × StepDelta)`(默认 100 / 5s / 100 / 1000),候选按与锚点分差就近、同分按等待先后取 `required-1` 个;候选不足锚点继续等,**队列不动**;PVE_TEAM 不看评分(tol=∞,纯等待序) | `Base=100` 而不是 0:两个零历史新号(都 1500)第一轮就互配;上限 1000 保证 45s 后几乎一定能配,不做无限等待。队首凑不到时**不阻塞后面的锚点**(见 §11.4 偏离说明) |
| D21 | **5v5 按评分蛇形分队**:降序排后 0/1/1/0/0/1/1/0/0/1,同分按弹出序稳定 | 等差评分下两队总分只差一个步长(7800/7700),"前 5 后 5"是 8500/7000;`teamIndexFor` 的 5v5 分支保留为无评分语义,生产路径走 `teamAssignment` |
| D22 | `RatingEnabled` 只控制 Kafka 消费(评分更新);评分镜像与容差匹配常开 | 关闭后全员 1500,容差匹配自然退化为等待序;随时可开,不需要迁移 |

### 11.2 Redis 键(补 §4.1)

| key | 类型 | slot | 写者 / 读者 |
|---|---|---|---|
| `match:{mq}:rank:<mode>:<config>` | zset(member=player_id,score=rating) | `{mq}` | 入队 / 回队首 / 取消 / 剔除 / 弹组 Lua;matcher 快照 Lua 读 |
| `match:rating:<player_id>` | hash `rating/games/updated_at_ms`,无 TTL | 按玩家 | Kafka 消费者写(`ratingApplyScript`);JoinQueue / 5v5 分队 / 旧队列迁移读 |
| `match:rating:applied:<battle_id>` | string(值=实例 uuid),TTL 7d | 按对局 | Kafka 消费者 SETNX |
| `match:ticket:<player_id>` | 新增字段 `rating`(入队时评分) | 按玩家 | 回队首按它 ZADD;日志 |

`rankKeyForQueue(queueKey)` 只把队列 key 倒数第三段 `queue` 换成 `rank`,前缀(含 hash tag)原样保留,票据里任何形态的
`queue_key` 都能推出同 slot 的镜像 key(`keys_test.go` 用自实现 CRC16 钉死)。旧格式 key(无 tag)推出的镜像 key 与之
**不同 slot**,所以取消旧 key 只做单 key `LREM`,不进 Lua(旧 list 从未有过镜像)。

### 11.3 Lua(全部 KEYS 同 `{mq}` slot)

| 脚本 | KEYS / ARGV | 语义 |
|---|---|---|
| `enqueueScript` | `[index, queue, rank]` / `[queueKey, pid, rating]` | `SADD` + `ZADD rank rating pid` + `RPUSH` |
| `requeueScript` | `[index, queue, rank]` / `[queueKey, pid, rating]` | `SADD` + `ZADD`(原评分)+ `LPUSH`(回队首 / 旧队列迁移) |
| `removeQueueMembersScript` | `[queue, rank]` / `[pid...]` | 逐个 `LREM 0`(同一玩家重复项一并清)+ `ZREM`,返回**实际从 list 摘出**的成员(取消出队 / 无效成员剔除 / 弹组摘出共用) |
| `queueSnapshotScript` | `[queue, rank]` / `[limit]` | `LRANGE 0 limit-1` + 每人 `ZSCORE`,返回 `[pid, score, ...]`(镜像缺失 score 为空串) |
| `pruneQueueScript` | `[index, queue, rank]` / `[queueKey]` | `LLEN==0 && !EXISTS queue` 时:`ZCARD>0` 先 `DEL rank`(镜像孤儿,list 是权威)再 `SREM` |

不变量:**list 成员集 == ZSET 成员集**(重复项除外:list 允许同一玩家两份,ZSET 天然一份;弹组 `LREM 0` 一次清完)。
每个用例末尾 `assertQueueMirrorConsistent` 断言 `ZCARD==LLEN` 且成员集合相同。

### 11.4 弹组算法(`popGroup`,持锁实例)

```
snapshot = queueSnapshotScript(queue, rank, 256)           // 等待序前缀 + 评分
for anchor in snapshot(按等待序, 最多 32 个):
    校验 anchor(票据 queued / 无 battle:lock;无效者当场 removeQueueMembers,继续)
    镜像缺失 → 按票据 rating ZADD 补写(滚动升级窗口内旧实例只写 list)
    tol = PVP ? ratingToleranceFor(anchor 已等秒数) : ∞
    cands = snapshot 里 anchor 之后、去重、|rating-anchor| <= tol 的成员
            PVP 按 |分差| 升序、同分按等待先后;PVE 纯等待序
    逐个校验 cands 取 required-1 个(无效者摘掉继续)
    不足 → 该锚点继续等(队列不动),换下一个锚点
    removed = removeQueueMembersScript(anchor + cands)      // 一条 Lua 原子摘出
    len(removed) < required(校验与摘出之间被 CancelQueue 摘走)→ 已摘出者 LPUSH 回队首,本轮不弹
    观测 wait_seconds / group_rating_spread;返回 members + 各自 ticket id
```

校验失败 / 摘出前的任何失败都**不需要回滚**(队列没动);Redis 抖动直接结束本轮。票据 CAS(`setTicketMatched`)、
弹出后取消者出局、凑不满回队首等 D5 逻辑不变。

**与任务描述的偏离(有意)**:任务写"锚点 = 队首",本实现是"锚点按等待序逐个尝试、队首优先":队首 A(1500)与
后面的 B/C(1800/1820)超容差时,B 作为锚点与 C 成组,A 继续等自己的容差放宽。纯队首锚点会让一个高分/低分孤点把
整条队列卡到他的容差放到 1000(45s),对后面本可互配的人是无谓等待。先到者仍然先挑(`TestHeadAnchorDoesNotBlockLaterAnchors`)。

容差曲线(默认;`RatingTolerance*` 可配,0 按默认):

| 已等 | 0-4s | 5-9s | 10-14s | 20-24s | 30-34s | >=45s |
|---|---|---|---|---|---|---|
| tol | ±100 | ±200 | ±300 | ±500 | ±700 | ±1000 |

### 11.5 结果回流(`internal/kafka/result_consumer.go` + `logic.ApplyBattleResult`)

- 启动:`RatingEnabled` 时 `kafkautil.EnsureTopics(match-results, partitions=ResultTopicPartitions, retention 7d)`
  (分区数是不可变契约,与 broker 不一致启动即失败),kafka-go `Reader{GroupID: match-rating, StartOffset: First,
  CommitInterval: 0}`;消费者不 import `internal/logic`(`svc → kafka → logic → svc` 会成环),入账逻辑由 `match_service.go` 以回调注入。
- 每条:`proto.Unmarshal` 失败 → 计 `decode_error`、提交跳过;handler 出错(Redis 抖动)重试 3 次(1s 退避)后跳过;
  处理完同步提交(崩溃最多重放一条,幂等标记兜底)。
- `ApplyBattleResult`:非 PVP 队列模式 / outcome 非 A 胜、B 胜、平局 / 队伍不是恰好两支非空 → `ignored`(不写标记);
  `SETNX applied` 失败 → `duplicate`;否则读全员评分、按队伍平均分算 `E_A`,`Δ_A = 32 × (S_A − E_A)`,`Δ_B = −Δ_A`,
  逐人 `ratingApplyScript`(rating / games+1 / updated_at_ms)。评分下限 0。
- C++ 侧契约:`battle_room_manager.cpp` 只在真实打完的局发,`winner_team_index` 0/1 与 `CreateBattle` 的 `TeamIndex` 同口径;
  DestroyBattle / AbortAllRooms 作废路径不发。

### 11.6 可观测

| 指标 | 类型 | 说明 |
|---|---|---|
| `match_rating_update_total{mode,outcome}` | counter | `applied` / `duplicate` / `ignored` / `error` / `decode_error` |
| `match_group_rating_spread{mode}` | histogram | 凑组时组内最高分−最低分(PVP 队列模式);分布右移 = 容差放宽太快或某段位人太少 |
| `match_wait_seconds{mode}` | histogram | 凑组时锚点已等秒数;回答"评分匹配让人多等了多久" |

日志:入队带 `rating=`;成组带 `anchor / wait / tol / spread / ratings`;5v5 分队带每人 `rating@team`;
入账带 `avgA / avgB / deltaA`。

### 11.7 配置(`etc/match_service.yaml`)

`RatingEnabled`(true)/ `ResultTopic`(match-results)/ `ResultTopicPartitions`(3)/ `ResultConsumerGroup`(match-rating)/
`RatingToleranceBase`(100)/ `RatingToleranceStepSeconds`(5)/ `RatingToleranceStepDelta`(100)/ `RatingToleranceMax`(1000)。
Kafka 地址复用既有 `Kafka.Brokers`(任务描述里的 `KafkaBrokers`)。

### 11.8 单测(miniredis,`rating_match_test.go`,12 个;全部 53 个绿)

容差曲线 / 两个新号立即互配(票据与镜像都记 1500)/ 分差 300 起始不配、锚点等 9s 仍不配、10s 配上(队列全程不动)/
容差内候选按分差就近(1500 vs 1520、1700 取 1520)/ 队首凑不到不阻塞后面的锚点 / PVE 组队分差 2000 按等待序立即成组 /
list 与 ZSET 在入队、取消、弹出、回队首(写回原评分)、剔除无效成员、重复项、孤儿镜像剔除后始终一致 / 旧队列迁移写镜像
(有记录 1700、无记录 1500)+ 镜像缺失成员由 matcher 补写后成组 / Elo:胜 +16 / 负 −16、重复投递不入账、平局按期望、
B 胜、PVE / 未结束 / 空队伍忽略不写标记不计局数、`applied` TTL 7d、计数器按 outcome / 5v5 队伍平均分 / 蛇形分队纯函数
(等差、乱序、同分)/ gather 真跑到 CreateBattle 快照 `TeamIndex` 按蛇形且每队 5 人。

现有 4 处"FIFO 弹出序"断言改为"同评分(全员默认 1500)仍按等待时间先后弹出"(`ticket_cas_test.go` 5v5 十人、
`review_fix_test.go` 去重 / 取消后成组 / 迁移后成组、`gather_fingerprint_test.go` popMatchedPair);`keys_test.go` 把
`rank` key、五条 Lua 的 KEYS 列表钉进同 slot 断言,评分 key 钉进"无 hash tag、按玩家分布"断言。

### 11.9 升级说明

1. **存储**:评分 key 无 TTL,切独立 MatchRedis(`noeviction`)后再开 `RatingEnabled`;单库形态下 `allkeys-lfu` 可能把评分淘汰回 1500(不致错,只是分丢)。
2. **滚动升级**:旧实例只写 list 不写 ZSET,新 matcher 对镜像缺失的成员按票据评分(旧票据无该字段 → 1500)补写 ZADD,
   两边混跑期间凑组照常;`migrateLegacyQueues` 搬旧格式队列时也写镜像。**不需要排空队列**。
3. **Kafka**:`match-results` 分区数一旦创建不可改(`EnsureTopics` 契约);改分区要换 topic 名并同步 C++ `kMatchResultsTopic`。
4. **回滚**:回退到无评分版本后 ZSET 成为无人维护的孤儿 —— 旧 `pruneQueueScript` 不删它,需手工 `DEL match:{mq}:rank:*`;
   评分 hash 与 applied 标记留着无害。

### 11.10 非目标 / 后续

- 评分不进客户端协议(`GetQueueStatus` 不回评分,`EstimatedWaitSeconds` 仍为 0);段位 / 赛季 / 衰减不在本轮;
- 预组队(`party_member_ids`)仍一期忽略,队伍评分聚合等预组队接入时再定;
- 3v3 未开放;开放时 `isRatedMode` 加一项、`assignBalancedTeams` 直接复用;
- 队列快照上限 256 / 锚点尝试上限 32 是常量,深度超过时后段成员等前段弹走后自然进入前缀;若某队列真的积压到这个量级,先看 `match_queue_depth` 再调常量。
