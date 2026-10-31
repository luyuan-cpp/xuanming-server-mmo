# 全服跨 Zone 匹配(Cross-Zone Matchmaking)

> **状态**: v1 历史基线；2026-10-16 全球 SQL 权威实现、客户端同步与验收边界见 §13
> **后续（2026-10-24）**: 按模板分池、zone → 大区 → 全服逐级扩大范围、每池单写者与一轮多局见 [global-match-hierarchical-pools.md](global-match-hierarchical-pools.md)；冲突处以该文为准（含：PVE 组队不再按 home zone 硬切池、单人档首选机房无节点立即改投）
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
| 实例崩溃中的票据 | ❌ `matched` 态票据 TTL 6h,实例挂了玩家 6h 不能再排 | `matched` 态 TTL 按组大小取最坏 gather 耗时(~~2 人 30s / 5 人 48s / 10 人 78s~~ **(2026-09-29 更正)1 / 2 / 5 / 10 人 = 42 / 48 / 66 / 96s**,公式见 D5 更正),票据写入全部带 ticket id 的 Lua CAS;**已冻结成员**另受 scene 侧 `battle:lock` 窗口(`BattleMaxDurationSeconds`+60s)约束,见 §5 |
| 多实例本机启动 | ❌ `MetricsListenAddr` 固定 :9170,第二实例绑定失败 | go_services.ps1 按实例/zone 派生 metrics 端口 |
| K8s | ❌ 无 match 部署清单;Redis 单副本 | 加 match Deployment(全局池,replicas≥2)+ Redis Cluster(3 主 3 从) |

---

## 1. 事实审计(为什么"链路已通")

### 1.1 玩家不搬家:快照模型绕开跨 zone 迁移的致命项

`docs/design/cross-zone-readiness-audit.md` 判定"玩家跨 zone 当前不可生产"(实体只 marshal 7 组件、bag/quest/mail 丢失、Kafka migrate 无 ACK)。**那是玩家实体跨 zone 搬家的问题**。回合制战斗的模型是:

> *(2026-09-20 标注)* 上面引用的是 2026-05 的判定:那份审计已被 [cross-zone-scene-travel.md](cross-zone-scene-travel.md) 取代,Kafka migrate 搬数据链已在阶段 3 整条删除,bag / mission 也已进普通存盘链。论据过期,但本节论点(快照战斗不经过玩家换 zone 的链路)不受影响。

- 玩家留在原 zone 的 scene 节点,`PrepareBattle` 只冻结 + 抽快照(`cpp/libs/services/scene/battle/system/player_battle.cpp`);
- 快照进全局 battle 节点(`cpp/nodes/battle/`),战斗零持久化;
- 结算以事件回流原 scene(`battle_room_manager.cpp:135` → `scene-{scene_node_id}`),由原 scene 的 `ApplySettlement` 应用。

跨 zone 匹配全程**没有任何玩家数据离开原 zone**,审计文档的所有致命项都不在这条链路上。

### 1.2 node_id 全局唯一 → Kafka topic 无 zone 也不撞

- Go:`go/match/internal/noderegistry/registry.go:60-63` —— `allocationKey` "是跨 zone 的全局占位 key —— 路径里不带 zone,因为两个 zone 的实例不可能同时拿到同一个 (node_type, node_id)";per-zone 的 `rpcPath` 只是发现路径。
- C++:`cpp/libs/engine/core/node/system/etcd/etcd_service.cpp:253` —— "Lost the race for this (node_type, node_id) globally — another zone …"。
- 压测实证:3 zone 6 个 gate 落成 `gate-group-{0..5}` 六个独立 group(`docs/stress/stress-3zone-2026-05-23-postmortem.md:542`)。
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
- `cpp/libs/engine/core/network/player_message_utils.cpp:267-287` `PickRandomNodeEntity`。(2026-09-29 已删:它只被零调用方的 `SendMessageToPlayerOnGrpcNode` 使用,二者一起删除;C++ 侧只剩 gate 的 `PickRandomNode`。)

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
| D2 | **双存储**:match 私有 key(`match:*` / `challenge:*` / `spectate:*`)→ `MatchRedis`(可配 `Type: cluster`);跨运行时契约 key(`player:{id}:location` / `player:session:{id}` / `battle:lock:{id}`)→ `SharedRedis`(既有共享库,match 只读)。**例外(2026-09-15,[team-system.md](team-system.md) DV-2 / J-2)**:match 进程内的 team 模块(`go/match/internal/team`)写 SharedRedis 的 `team:rec:*` / `team:<id>` / `team:player:*` / `team:invite:*` 四类 key;match 原有代码(队列 / 票据 / 切磋 / 观战)对 SharedRedis 仍只读。**2026-10-12 更正**:唯一的写是 matcher 启动时与周期 sweep 的 `migrateLegacyQueues`(`go/match/internal/logic/matcher.go`),在 SharedRedis 上 SCAN + RPOP 改造前实例写下的旧格式队列并搬进 MatchRedis;写的是 match 自己的旧 key,不碰任何跨运行时契约 key。**2026-10-20 更正**:项目未上线、没有改造前实例留下的旧格式队列,`migrateLegacyQueues` / `migrateLegacyQueue` / `legacyQueueSweepInterval`、`keys.go` 的 `legacyMatchQueueScanPattern` / `legacyMatchQueueKey`、CancelQueue 与 `dequeueMemberCtx` 对旧格式键的单 key LREM 已整体删除,`ticketQueueKey` 只返回新格式键;match 原有代码(队列 / 票据 / 切磋 / 观战)对 SharedRedis 恢复为只读,上面的 team 例外仍是唯一的写 | 契约 key 的写者是 scene_manager / player_locator / **C++ scene**,C++ 无集群客户端;按有界上下文拆存储是标准做法,且不动任何其他服务。`MatchRedis` 缺省时回落到 `Redis`(向后兼容,本地单库照跑)。team 例外的理由:C++ scene 用 hiredis `GetZoneRedis()` 读 `team:<id>` 投影(无集群客户端),所以投影只能放 SharedRedis;权威记录、投影、玩家索引、邀请反查必须在同一条 Lua 里原子提交,四类 key 只能同库。写入范围限定这四类前缀,不扩大到其他契约 key |
| D3 | **队列相关 key 共用 hash tag `{mq}` 同 slot**:`match:{mq}:index`(注册集)、`match:{mq}:queue:<mode>:<config>`、`match:{mq}:lock:<mode>:<config>`;入队用 Lua 原子 `SADD index + RPUSH queue` | 用注册集取代 SCAN;同 slot 让"注册集 ⊆ 队列集合"成为不变量(主从异步复制的故障切换不会让队列脱离注册集);队列操作 QPS 极低,单 slot 不是瓶颈(§6) |
| D4 | 票据 `match:ticket:{pid}` 按玩家分布;新增 `zone_id`、`queue_key` 字段 | zone 用于可观测性与对局日志;`queue_key` 让取消 / 回队首不再重算 key(格式演进安全) |
| D5 | **`matched` 态票据短 TTL**:弹组时按组大小计算 `max(MatchedTicketTTLSeconds, n×(removeObserverTimeout+prepareTimeout) + createTimeout + rollbackTimeout + 10s)`(2 人 30s / 5 人 48s / 10 人 78s;超时常量取自 gather.go,不另写数字)。**(2026-09-29 更正,原公式与数值保留作历史)现行公式** `max(MatchedTicketTTLSeconds, ⌈n×(removeObserverTimeout+prepareBattleTimeout) + gatherCreateStageWorst + rollbackTimeout⌉ + 10s)`:原来的 `createTimeout` 项换成 `gatherCreateStageWorst = 2×(spectateRecordWriteWorst + createBattleTimeout) = 2×(6.1s+5s) = 22.2s`,覆盖「建房前先写观战记录(fail-closed)+ 首选节点被准入拒绝(`battle_not_allocatable`)后改写记录、换节点再建一次」的最坏链路(k8s-client-entry D82);常量 3s / 3s / 5s / 3s / 6.1s 取自 `gather.go:27-29`、`spectate.go:35`、`:81`。**1 / 2 / 5 / 10 人 = 42 / 48 / 66 / 96s**(`go/match/internal/logic/queue.go:342`、`:367-380`)。同一值也是 `prepare_deadline_ms` 的窗口(D13),下游:scene 备战锁 EX = 该期限 + 60s(十人组最长到 gather 起点 +156s),battle 确认事件补发窗口 `kConfirmResendWindowMs` = 180s ≥ 156s(`battle_room_manager.cpp:267`)。未编译、未测试,待 Codex 验证。**(2026-10-31 更正)**观战记录键已删,`gatherCreateStageWorst = 2×createBattleTimeout = 10s`,**1 / 2 / 5 / 10 人 = 30(配置下限,公式值 29)/ 35 / 53 / 83s**,十人组锁最长到 gather 起点 +143s,仍 ≤ 180s;未运行,待 Codex 验证;gather 失败进补偿前对幸存者按 `prepared×3+10` 续期;**票据所有写入带 ticket id 的 Lua CAS**(matched / ready / 回队首 / 失败删票 / 取消删票五处):票据过期被玩家重排后的新票据不会被迟到的旧 gather 写脏;CAS 失败的成员不进 gather(视为已取消出局);回队首**先 CAS 后 LPUSH**,LPUSH 失败则 CAS 删票不留反向孤儿;JoinQueue 遇 queued 票据先 `LPOS` 探测队列,不在则原地补入队(票据/队列分属两个 master 的故障切换组合丢失可自愈);JoinQueue 遇 **ready** 票据且 `battle:lock` 已不存在(战斗已结束)→ CAS 删票放行(2026-09-02 冒烟复跑:上一局 16s 前结束,ready 票据 TTL 60s 未到把"打完立刻再排"拒了一分钟) | 三视角复审(2026-09-02)逐条推翻了"固定 30s"与"先 LPUSH 后 CAS":gather 链路是 清退×n → Prepare×n → Create → (失败)Destroy+解冻×n,固定 30s 在 5 人组正常路径就会过期;scene 侧冻结由既有 deadline reaper 兜底(见 §5) |
| D5b | **`shared/snowflakealloc` 接管原 worker id 的条件 = (旧 lease 已死)或(前任留下 `released` 标记)**:`Close()` 不撤销 lease(既有契约:≤TTL 的隔离窗吸收跨机时钟偏斜)但会在同一 lease 下写 `<nodeKey>/released` 标记 → 同主机优雅重启仍在 TTL 内复用原 id;lease 活着且无标记(仍在运行 / 刚崩溃)则**不抢**,改用派生键 `hostname#<leaseID>` 继续分配新 id(不能停在原键上:双 key CAS 要求 nodeKey 不存在,会一直失败到 ctx 超时 —— kill 后 60s 内重启也会撞上,本地实测 scene_manager 就这样 `claim txn: context deadline exceeded` 起不来);KeepAlive 句柄按 idKey 里实际写入的亲和键做所有权 watch;match 另把亲和键改为 `hostname#ListenOn` | 原实现按 hostname 亲和"复用"且不校验旧持有者存活(`allocator.go:144-167`)。**本地实证(2026-09-02 11:43)**:按 `-Zone 2` 起第二个 login,它抢走 worker 0 并把 nodeKey 挂到自己的 lease,zone1 login 在 keepalive 里检测到 ownership lost → fence + 退出 → 网关 assign-gate 全线 UNAVAILABLE。这是共享库缺陷,login / scene_manager / player_locator / guild / friend / match 全部受影响,所以修在库里而不是各服务绕开;K8s 每 Pod 独立 hostname 不触发,但不能把正确性押在部署形态上 |
| D5c | `queue_depth` 只由持锁实例上报;队列从注册集剔除时显式归零 | 多实例各自上报同一队列 → 看板 sum 放大 N 倍;队列弹空后 gauge 永不归零 → 积压告警误报 |
| D6 | JoinQueue 读位置取 zone;位置缺失直接拒(新错误码 `ErrNotInScene=8`) | 无位置的玩家 gather 必败(`no_location`),提前拒绝比进队再失败更省 |
| D7 | 挑战记录的双 key `DEL` 拆成两条单 key `DEL` | 两 key 身份不同不能同 tag;非原子可接受(target 锁有 TTL,残留只影响 60s 内重复挑战) |
| D8 | metrics:`queue_depth` 不加 zone 标签(队列本就不分 zone);新增 `gather_zone_mix_total{mode,mix=single|cross}` | 低基数,直接回答"跨 zone 对局占比" |
| D9 | 客户端零改动 | 协议未变;对手 zone 展示留二期(要动 `BattleActorState` + C# regen) |
| D10 | 部署:match 作为**全局池**部署一次(K8s 放 `mmorpg-infra`,replicas≥2);本地 `dev-start-zones` 继续每 zone 起一份(等价于多实例) | 与 battle 同形态;zone 标签只影响 etcd 注册路径 |
| D11 | **gate 随机路由对全局池类型豁免 zone 过滤**:新增 `NodeUtils::IsGlobalPoolNodeType`(仅 BattleNodeService / MatchNodeService),`PickRandomNode` 与 `PickRandomNodeEntity`(后者 2026-09-29 已随死代码删除)对这两类不比对 zone;其余类型(friend/guild/chat/login…)路由语义不变 | §1.5 的硬门与全局池设计矛盾;只豁免明确的全局池类型,不扩大爆炸半径。任一 zone 的 match 实例挂了,该 zone 的 gate 自动落到其他 zone 的实例 —— 这是"容错不降级"在入口层的体现 |
| D12 | **SharedRedis 必须是全服单一实例**(不是 per-zone/per-region 分片):`player:{id}:location`(scene_manager)、`player:session:{id}`(player_locator)、`battle:lock:{id}`(C++ scene)三类契约 key 的写者都以它为准 | 当前本地/K8s 都是一个共享 Redis,满足;data_service 有 Regions→Redis 分片设计草案,若将来采纳,match 需要按 region 路由 SharedRedis 读取(二期,§10)。本轮在 match 启动日志打印 SharedRedis 地址并在文档钉死这条不变量 |
| D13 | **`PrepareBattleRequest.prepare_deadline_ms` = now + 本组 matched TTL**(`matchedTicketTTLFor(组人数)`,2 人 30s / 5 人 48s / 10 人 78s;**(2026-09-29 更正)现为 1 / 2 / 5 / 10 人 = 42 / 48 / 66 / 96s**,公式见 D5 更正);`deadline_ms` 语义不变(战斗作废期限) | §5 "已冻结成员受 `battle:lock` 窗口约束"的二期项落地(match 侧):备战期限与票据 matched TTL 同一窗口,弹组实例在 gather 中崩溃时,未冻结者靠票据过期、已冻结者靠 scene 在 PREPARING 态按该期限解冻并收窄锁 EX,两边同一时刻放行。scene 侧读取该字段的实现见 turn-based-battle-server.md 对应条目;旧版 scene 忽略该字段(0/未知 = 沿用 `deadline_ms`),向后兼容 |
| D14 | **配表指纹比对**:scene 在 `PrepareBattleResponse.table_fingerprint` 回报本节点六张战斗表内容指纹(解析后确定性序列化的 sha256 前 16 字节 hex),match 收齐全员后比对;**全员非空且两两一致**才写进 `CreateBattleRequest.table_fingerprint`(battle 再与自身核对)。不一致或部分为空按 `TableFingerprintMode`(`off`/`warn`/`enforce`,默认 `warn`):`off` 不比对不透传;`warn` 只记 Error 日志 + `match_table_fingerprint_mismatch_total{mode}`,照常开局、不透传;`enforce` 视为 prepare 失败走既有补偿(已冻结者逐个解冻,肇事者出局删票,幸存者回队首),肇事者 = 与多数派不一致者(多数派只在非空指纹里选,平票取弹出序靠前者;全员皆空记第一位) | §10 "配表版本一致性"落地:灰度/回滚会让同一场战斗按不同数值表入场,快照与判定各按各的表算。灰度期用 `warn` 看指标确认各 zone 表版本对齐后再切 `enforce`;`enforce` 下不回报指纹的旧版 scene 会被一律拒绝,所以它不是默认值 |

> **(2026-10-19 指向注,D12 原文保留)** 负责人 2026-10-17 拍板核心走「单元化 + 单元内 Redis Cluster 按 `{player_id}` 分片」,D12 的「SharedRedis 必须是全服单一实例」改写为「逻辑单一、物理按 `{player_id}` 分片」:同一玩家的契约键(含 `player:{id}:location`、`player:session:{id}`、`battle:lock:{id}`)只有一个权威存储,即所在单元核心 Cluster 里该玩家的 slot;跨玩家的多键操作要拆开。改写内容与待拍板项见 [shared-redis-split-by-purpose.md](shared-redis-split-by-purpose.md) §20.1、§20.5、§25.2 第 20 条(纯设计,第 4 批未落码)。**第 4 批落码之前,D12 照旧有效。**

---

## 3. 架构与数据流(2 zone 示例)

```
zone1 客户端 ──TCP──▶ z1 gate ─┐                        ┌─▶ z1 scene ──PrepareBattle──▶ 快照
                               │ NODE_MATCH 随机路由     │
zone2 客户端 ──TCP──▶ z2 gate ─┼──gRPC──▶ match×N ──────┤
                               │      (无状态,任意实例)  └─▶ z2 scene ──PrepareBattle──▶ 快照
                               │             │
                               │             ├─ MatchRedis(Cluster):{mq} 队列/注册集/锁,票据,挑战,观战索引
                               │             └─ SharedRedis:player:{id}:location / player:session:{id} / battle:lock:{id}(只读)
                               │             │
                               │             ▼ CreateBattle(全局 battle 池随机)
                               │        battle 节点 ──Kafka gate-{id}──▶ 任意 zone 的 gate(BindBattle / S2C)
                               │             ▲      ──Kafka scene-{id}─▶ 原 zone 的 scene(结算回流)
                               │             │
   各 zone 客户端 ──TCP 票据直连(每局第二条连接)──┘   ← 2026-09-29 补:战斗唯一通路;gate 那条 Kafka 边
                                                       只剩大厅公告 Assigned / Start(无直连时回落),BindBattle 已删
```

JoinQueue(任一 zone 玩家)→ 票据(含 zone)→ Lua 原子入队 → matcher(持锁实例)LPOP 凑组(**不看 zone**)→ gather 逐人按 `location.zone_id` 找对应 zone 的 scene 冻结抽快照 → CreateBattle → BindBattle 经各自 gate 的 topic → 战斗 → 结算回各自 scene。

> **(2026-09-29 更正)上图 battle → gate 一行与上段「BindBattle 经各自 gate 的 topic」已被 turn-based §22 D66–D68 取代**,原文保留作历史:
> - Kafka `BindBattleEvent` / `UnbindBattleEvent` 契约已删(D67),gate 两种路由模式都不中继战斗、不维护战斗会话绑定(D66)。
> - battle → gate 命令 topic 只剩**大厅公告** `NotifyBattleAssigned`(带票据与 battle 客户端可达地址)/ `NotifyBattleStart`,两者都经 `PushLobbyAnnouncement` 出口(`battle_room_manager.cpp:1505`、`:621`),且只在该玩家没有活直连时回落(D68);其余 S2C(回合结果、终局、观战帧)只走直连,无直连即丢弃。
> - topic 记法:本节图与原文的 `gate-{id}` / `scene-{id}` 是一节点一 topic 的旧名,只在迁移窗口里仍被消费;现行 battle 经 `node::kafka::ResolveCommandRoute` 发往共享分区 topic `gate-cmd_g<N>` / `scene-cmd_g<N>`,分区 = 目标 node_id % P(`node_command_route.h:49-68`,`battle_room_manager.cpp:91-94`、`:169-171`,[control-plane-topic-partitioning-20260908.md](./control-plane-topic-partitioning-20260908.md))。
> - 入口路由:上图「zN gate ──gRPC──▶ match×N(NODE_MATCH 随机路由)」是 gate 直连模式的画法。本地(`start_game.ps1`)与 K8s(`k8s_deploy.ps1`)部署默认路由模式,gate 经 `client_rpc_router` 转到 match(gate 白名单收成 `{Scene, ClientRpcRouter}`,`gate/main.cpp:216-219`;[client-rpc-router.md](./client-rpc-router.md),[xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-12 修订);C++ 进程默认仍是直连。
> - 各 zone 的客户端收到 Assigned 后,凭票据**直连**全局池里的 battle 节点(每局第二条连接,与所在 zone 无关;跨 zone 对局双方连同一台 battle);上行战斗消息只走这条直连。
> - 更正后的流程:… → CreateBattle(battle 预签全员票据,任一失败不建房,D70)→ battle 经各自 gate 的命令 topic 推 Assigned / Start(无活直连时回落;有活直连则直发)→ 客户端直连 battle → 战斗 → 结算回各自 scene(Kafka scene 命令 topic,路径不变)。
> - 以上已落码、**未编译、未测试**,待 Codex 验证;集群外客户端能否连到 battle 取决于入口形态(D76–D93,k8s-client-entry.md)。

---

## 4. Redis 存储契约(修订 turn-based-battle-server.md §6 / §10.4)

### 4.1 MatchRedis(match 独占,可集群)

| key | 类型 | 语义 | slot |
|---|---|---|---|
| `match:{mq}:index` | set | 活跃队列 key 注册集(SCAN 的替代) | `{mq}` |
| `match:{mq}:queue:<mode>:<config>` | list | 排队玩家 id,等待序(队首最久) | `{mq}` |
| `match:{mq}:rank:<mode>:<config>` | zset | 同一队列的评分镜像:member=player_id,score=入队时评分(§11) | `{mq}` |
| `match:{mq}:lock:<mode>:<config>` | string | matcher 凑单临界区(SETNX,值=实例 uuid) | `{mq}` |
| `match:{mq}:size<N>:{queue,rank,lock}:<mode>:<config>` | list / zset / string | (2026-10-09 补)PVE 人数档,N = 2..副本上限,每档各一套队列 / 评分镜像 / 凑单锁;`PVE_TEAM` 新入队一律写带人数段的 key,见 [pve-team-size-matching.md](pve-team-size-matching.md) §3.2。排查某一档用 `SMEMBERS match:{mq}:index`,`match:{mq}:queue:*` 的通配匹配不到它 | `{mq}` |
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
| `player:{<id>}:location` | scene_manager | JoinQueue 取 zone;gather 定位 scene 节点;观战定位 |
| `player:session:{<id>}` | player_locator | 挑战推送 / 观战路由取 gate |
| `battle:lock:{<id>}` | C++ scene | 咨询性"是否在战斗中" |

"match 只读"有唯一例外:team 模块写 `team:rec:*` / `team:<id>` / `team:player:*` / `team:invite:*`(D2 例外,key 全表与 Lua 见 [team-system.md](team-system.md) §C.1 / §C.5)。
**2026-10-12 更正**:"唯一例外"不准确,还有第二处写 —— matcher 启动时与周期 sweep 的 `migrateLegacyQueues`(`go/match/internal/logic/matcher.go`)在 SharedRedis 上 SCAN + RPOP 改造前实例写下的旧格式队列 `match:queue:*`,搬进 MatchRedis(见 D2 更正)。它只碰 match 自己的旧 key;本节标题的"只读"应理解为"对跨运行时契约 key 只读"。
**2026-10-20 更正**:`migrateLegacyQueues` 已随旧格式队列兼容整体删除(项目未上线,不存在旧格式 key;见 D2 的 2026-10-20 更正)。第二处写不复存在,team 模块重新是唯一例外;GlobalMatch 关闭时每个副本每分钟一次的 `SCAN match:queue:*` 也随之消失。

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
| match 实例在 gather 中崩溃 | **未冻结成员**:票据 `matched` 按组大小 TTL(~~30/48/78s~~ **(2026-09-29 更正)1/2/5/10 人 42/48/66/96s**,D5 更正)过期 → 可重排;**已冻结成员**:scene 侧 `battle:lock` EX = `BattleMaxDurationSeconds`+60s(`player_battle.cpp:359-376`),JoinQueue 对锁 fail-closed,要等 reaper 按 `InBattleComp.deadline_ms` 解冻(默认 ≈5-6 分钟) | 未冻结者 ~~≤78s~~ **≤96s(2026-09-29 更正,十人组最坏值)** 自愈;已冻结者受 C++ 锁窗口约束 —— 这是既有 scene 契约,不是 match 能单方面缩短的(match 侧"遇锁就调 CancelBattlePrepare"被复审判定不安全:scene 只比对 battle_id 不看状态,会把正在打的玩家解冻)。**D13 已落地 match 侧**:PrepareBattle 带 `prepare_deadline_ms`(= now + 本组 matched TTL),scene 在 PREPARING 态按它解冻并缩短锁 EX 后,已冻结者与未冻结者同一窗口自愈 |
| 某 zone 的 scene 配表版本漂移(灰度/回滚) | `PrepareBattleResponse.table_fingerprint` 不一致:`warn`(默认)记日志 + `match_table_fingerprint_mismatch_total` 照常开局;`enforce` 拒开局走 prepare_failed 同款补偿,与多数派不一致者出局,幸存者回队首(D14) | 指标可告警;`enforce` 下不产生"两套数值表"的对局 |
| Redis Cluster 故障切换丢票据/队列组合 | 票据(按玩家 slot)与队列(`{mq}` slot)分属两个 master,异步复制可各自丢最近写:队列丢票据在 → JoinQueue 用 `LPOS` 探测后补入队;票据丢队列在 → popGroup 校验票据缺失静默丢弃,玩家可重排 | 无 6h 卡死 |
| 滚动升级(旧 key 格式在途) | 旧 `match:queue:{mode}:{config}`(无 hash tag)不在注册集:matcher 启动及每 60s 在 SharedRedis `SCAN` 一次把旧队列成员搬进新队列;旧票据无 `queue_key`,CancelQueue 对新旧两个 key 各 `LREM` 一次 | 在途排队者不丢;升级窗口过后可删除迁移逻辑(§10) |
| Redis Cluster 某 master 故障切换 | go-redis 跟随 MOVED/ASK 重刷 slot 表;异步复制可能丢最近写:①队列与注册集同 slot 同 Lua,要丢一起丢;②matcher 锁丢失 → 两实例同时凑单,但 `LPOP` 在单 master 上原子,同一玩家不会被弹两次,最坏是一轮凑不满回队首 | 数据不脱契约;无双开战 |
| SharedRedis 故障 | JoinQueue 读锁失败按"有锁"处理(fail-closed,既有);gather `no_location` 失败走补偿 | 拒绝新排队,不产生错误对局 |
| etcd 失租(snowflake) | 既有:fence 发号器 → flush → 退出重启 | 不撞 id |
| 某 zone 的 scene 全挂 | 该 zone 玩家 gather `prepare_failed` → 肇事者出局删票、幸存者回队首(既有补偿矩阵) | 其他 zone 玩家继续匹配 |
| battle 池空 | `no_battle_node` → 补偿;告警指标 | 排队不丢 |
| 节点 gRPC 端口被占(如杀 battle 后立刻重拉,旧进程 50100 未释放) | **2026-09-04 前 fail-open**:带空 `grpc_endpoint` 注册,gate 循环 `grpc_endpoint is empty`、match 报 battle 池空、JoinQueue 超时;**现 fail-closed**(`node_allocator.cpp`):gRPC 端口不可用不发布、退回 TCP 端口、`AcquirePortWithRetry` 退避重试(预设端口等释放,扫描路径换下一对) | 不产生"活着但连不上"的节点;重拉前给旧进程 ≥5s 释放端口 |

---

## 6. 水平扩展模型与瓶颈

- **入口**:gate 在全 zone 的 match 实例间随机路由,实例无状态 → 加实例即扩容。
- **凑单**:每个 `(mode,config)` 队列一把锁,同一队列同时只有一个实例弹组,但弹组只是 `LPOP` 级操作(微秒级),gather(3–5 跳 RPC)在弹组实例的 goroutine 里并行,不占锁 → 凑单串行不是瓶颈。
- **单 slot**:`{mq}` 把所有队列压到一个 master。每次 JoinQueue 一条 Lua、matcher 每 500ms 每队列一次 `LLEN` + 若干 `LPOP`。按 1 万玩家/分钟入队算,单 master 负载 < 200 QPS,余量三个数量级;票据 / 挑战 / 观战按玩家分布,吃满集群分片。
- **snowflake**:worker id 17-bit 布局,由 etcd `/match` 前缀独立分配,实例数上限远高于实际需要。(2026-10-30 更正:battle_id 已迁到号段,match 不再申领 snowflake 槽位,上面故障表里「etcd 失租」一行也不再适用。见 [总体方案 §6.4](player-data-tidb-scaling-plan.md)。)

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
| K8s kind A 档实跑(2026-09-04) | ✅ kind mmorpg 上 infra-up 全 Ready:etcd/kafka/mysql/redis + redis-match-cluster 0..5(cluster create OK)+ match ×2(MatchRedis 集群 + 契约 key 单机 Redis 双存储配置生效、snowflake worker 0/1、自建 match-results、etcd 注册 zone/101);修 mysql.yaml binlog 目录进 datadir 的初始化拒启。B 档阻塞:go/db 的仓库外 replace(proto2mysql)进不了 build context;C++ 节点无 Linux 镜像(C 档) |

## 10. 非目标 / 后续

### 10.0 明确不在本轮:共享 Redis(SharedRedis)不切集群

七 agent 审计(2026-09-02)列出的下列 Cluster 阻塞项全部属于**其他服务在共享库上的用法**,双存储设计正是为了
绕开它们。在这些项修完之前,**共享库禁止切 Redis Cluster**(只有 MatchRedis 可以):

| 服务 | 阻塞项 |
|---|---|
| C++ scene | hiredis 裸连接无 MOVED/ASK;存盘 Lua 访问未声明的全局 key `dirty_keys_set`(`redis_client.h:18-22,461-475`)。(2026-10-20 更正:B0-2 已随 `e76f6f747` 删掉这项写入,两段存盘脚本 `kSaveLuaScript` / `kSaveIfGuardLuaScript` 现在只访问 KEYS[] 声明的键,未编译、待 Codex 验证。本行剩下的阻塞是:无 MOVED/ASK,以及守卫脚本的两个键没有共同 hash tag)|
| player_locator | 8 个 Lua 把 per-player key 与全局租约容器原子绑定(CROSSSLOT);`claimExpiredLeasesScript` 脚本内拼 key |
| login | 排队/token 4 处跨 slot MULTI/EXEC;gate 排空查询跨 slot MGET |
| scene_manager | 场景生命周期 Lua 7 key/3N key 跨 slot |
| guild / friend | 非 0 号 DB(Cluster 只有 DB0);榜单重建跨 slot TxPipelined + RENAME;cache generation-CAS 2 key |
| db | Kafka 重试三队列 RPOPLPUSH 跨 slot |
| match(team 模块,2026-09-15 登记,[team-system.md](team-system.md) §C.5 / J-25) | **S_COMMIT**(唯一写脚本)一条 Lua 同时写 `team:rec:<tid>`、`team:<tid>`、全部增删成员的 `team:player:<pid>`、全部增删邀请的 `team:invite:<pid>`(跨 slot,CROSSSLOT);S_TOUCH(记录 + 投影 + 成员索引)、S_HEAL_ORPHAN(`team:player:<pid>` + `team:rec:<tid>`)、S_READ_MEMBERS 同样多 key。`team:<tid>` 是 C++ scene 既有契约 key 格式(不带花括号),集群化时要么全部改 hash tag 并同步改 scene,要么拆脚本并重新论证原子性 |
| 运维 | `FLUSHALL` 在 Cluster 只清一个节点(CLAUDE.md §6.2 压测口径要改)|

与跨 zone **匹配**无关但被审计顺带指出的既有问题(不在本轮):玩家实体跨 zone 迁移的 `ErrUnsafeCrossNodeHandoff`
门、`player:zone:{id}` home zone 无生产写入方、存盘按进程 zone 而非 home zone 路由、同 zone 多 scene 的
player_migrate 目标判据(`scene_info.guid` uint32)、SceneManager 部署粒度 zone/职责全局的错位。这些都在
"玩家搬家"链路上,快照战斗不经过它们。

> **状态说明(2026-09-20)**:上面这段列的是审计当时的既有问题,其中多项已被 [cross-zone-scene-travel.md](cross-zone-scene-travel.md) 阶段 1–3 改变(代码已进 main,尚未编译 / 测试),原文保留为历史:
> - `ErrUnsafeCrossNodeHandoff` 门 → 已被**换手门**取代:源 scene 为当前 `owner_epoch` 写出 `player:{id}:handoff` 落盘标记即放行,否则回可重试的 18(`ErrHandoffPending`);14 是历史码,不再发出(CZ-4)。
> - 存盘按进程 zone 路由 → 已改为 `GetDbTaskTopic(home_zone)`(CZ-2 / CZ-3);2026-10-30 起存盘直接提交 `player_store`,不再有这个 topic。
> - `player_migrate` 目标判据 → 随搬数据链在阶段 3 整条删除而消失(§11.3)。
> - `player:zone:{id}` 无生产写入方 → login 建角现在会调 `RegisterPlayerZone` 写入(`go/login/internal/logic/pkg/homezone`);该处归 login 会话维护,细节以其代码为准。
> - 「SceneManager 部署粒度错位」一项本次未重新核实。
> 本段的论点不变:这些都在"玩家换 zone"链路上,快照战斗不经过它们。

- 同 zone 优先 / 延迟加权匹配(用户明确不要);
- 客户端展示对手大区(需 `BattleActorState` 加 zone + C# regen);
- C++ 侧接 hiredis-cluster(本轮用双存储绕开;若未来 SharedRedis 也要集群化再做);
- gather 中实例崩溃时已冻结成员的 `battle:lock` 窗口 = `BattleMaxDurationSeconds`+60s(既有 scene 契约):**match 侧已落地 D13**(`PrepareBattleRequest.prepare_deadline_ms` = now + 本组 matched TTL);scene 在 PREPARING 态按它解冻并缩短锁 EX、CreateBattle 成功后由 battle 经 Kafka 通知 scene 升级为正式 deadline 属 scene / battle 侧工作;
- 旧队列迁移逻辑(`migrateLegacyQueues`,每 60s 对 SharedRedis `SCAN match:queue:*`)只服务升级窗口,下一版本删除;
  **2026-10-20 已删除**:项目未上线、没有存量旧格式队列,迁移代码、旧格式键的拼法与 CancelQueue 的旧键回落连同相关用例一并删掉,不再支持回滚到加 hash tag 之前的版本;
- battle 节点的 K8s 清单(本轮只补 match;battle 缺清单是既有状态);
- **配表版本一致性**:各 zone 的 scene 用本节点的表算快照、battle 用自己的六张表判定;灰度/回滚会让同一场
  战斗按不同数值表入场。**match 侧已落地 D14**(收集 `PrepareBattleResponse.table_fingerprint`,全员一致才透传
  `CreateBattleRequest.table_fingerprint`,`TableFingerprintMode` 决定不一致时告警还是拒开局);scene 侧算指纹、
  battle 侧与自身指纹核对属各自服务的工作;
- **多实例配置漂移**:`PveTeamSizeByConfigId` / `BattleMaxDurationSeconds` 各实例取本地 yaml,漂移会让同一队列
  按不同人数弹人(二期:凑满人数改查 DungeonTable,已在 §5.4 记为权威来源);
  (2026-10-09 更正:PVE 人数档队列的凑满人数来自队列 key,`PveTeamSizeByConfigId` 现在是人数**上限**。漂移的表现改为
  「上限较低的实例对超出的人数档不弹组、只告警」与「不填 `team_size` 的请求在不同实例落进不同人数档」;
  只有存量的不带人数段的 `PVE_TEAM` 队列仍是「按不同人数弹人」。见 pve-team-size-matching.md §3.2)
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
   **2026-10-20 更正**:`migrateLegacyQueues` 已删除,"搬旧格式队列时也写镜像"一句作废;matcher 对镜像缺失成员的补写仍在,原先挂在迁移用例后半段的断言改为单列的 `TestMatcherBackfillsMissingRankMirror`。§11.8 列的 `review_fix_test.go`「迁移后成组」用例同批删除。
3. **Kafka**:`match-results` 分区数一旦创建不可改(`EnsureTopics` 契约);改分区要换 topic 名并同步 C++ `kMatchResultsTopic`。
4. **回滚**:回退到无评分版本后 ZSET 成为无人维护的孤儿 —— 旧 `pruneQueueScript` 不删它,需手工 `DEL match:{mq}:rank:*`(2026-10-09 起还有人数档的 `match:{mq}:size*:rank:*`);
   评分 hash 与 applied 标记留着无害。

### 11.10 非目标 / 后续

- 评分不进客户端协议(`GetQueueStatus` 不回评分,`EstimatedWaitSeconds` 仍为 0);段位 / 赛季 / 衰减不在本轮;
- 预组队(`party_member_ids`)仍一期忽略,队伍评分聚合等预组队接入时再定;
- 3v3 未开放;开放时 `isRatedMode` 加一项、`assignBalancedTeams` 直接复用;
- 队列快照上限 256 / 锚点尝试上限 32 是常量,深度超过时后段成员等前段弹走后自然进入前缀;若某队列真的积压到这个量级,先看 `match_queue_depth` 再调常量。

## 12. 全球同服与区域优先策略（2026-10-15，用户确认的待实现契约）

全球同服表示全球玩家可以共享身份与组队关系并参与同一玩法的匹配规则；按延迟分池是一种调度策略。用户确认选择区域内优先、等待后跨区域兜底，完整决定与范围见 xuanming-port-decisions-20260910.md D-16。此节覆盖旧文档中可能被理解为始终只用一个无地域限制全局队列的口径；不提前宣称下述能力已接线。

| 阶段 | 必须保持的契约 | 验收场景 |
|---|---|---|
| 探测与组队 | 按每位玩家到候选机房的实测延迟判断；校验探测时效/可信性，不能只按队长或玩家ID路由；预组队作为整体 | 队长低延迟而队员超限、跨区队伍、缺失/过期探测 |
| 本区候选 | 同玩法与人数档内优先选择共同低延迟机房；保留现有公平性、取消、人数和版本规则 | 本区人数足够不无故扩区；本区后续有效票据不被队首永久阻塞 |
| 等待扩展 | 从最初入队时间按配置逐步扩大地域/评分窗口；跨区仍逐成员守延迟上限；候选扫描有界 | 冷门/高段位等待后可成局；重试不重置等待时长；超延迟组合仍拒绝 |
| 原子成局 | 校验票据代次、队伍版本及全成员状态；同一玩家最多拥有一场有效成局；失租写者失败关闭 | 取消与成局并发、双实例争抢、旧leader继续运行、重复提交 |
| 实际放置 | 选择的候选机房进入分配请求与执行端验证；分配失败/结果未知有稳定幂等键和恢复入口 | 机房无容量、响应丢失、迟到成功、重试/切换不重复开局 |
| 客户端与两版 | 探测、状态、取消、重接和错误语义贯通独立客户端及两版服务 | 多地域预组队端到端、重接补拉、跨版本接缝验证 |

原仓静态对比：services/matchmaking/matchmaker/internal/biz/match.go 的 formMatchesInPool 已有同区优先和超时溢出；ticketRegion 依队长逻辑归属，不能当成物理 RTT。battlePlacement 结果当前仅记录日志（:2689），分配协议闭环未完成。其单写者/租约/全局队列布局也需结合真实分片键审查，算法存在不能证明多地域接线或故障安全。

本仓现有跨 zone 快照战斗、票据 CAS、Redis hash tag、双存储和会话/资产防护继续保留；本次不迁移被排除的 scene_manager。机房放置所需最小接口适配须与已有 battle 池责任边界一致，不能借匹配重写场景管理。等待、成局、逐成员延迟、扩区比例与依赖故障须留低基数指标和同口径压测证据，player_id 不作指标 label。当前尚无该策略的地域端到端或 2000 万 CCU 验收证据。

## 13. 全球权威成局与未知结果恢复（2026-10-16）

本节为第 12 节的实现补充，覆盖第 1～11 节 Redis 票据/成局作为生产权威的旧口径。2026-10-30 起 `GlobalMatch.Enabled` 与 Redis 队列、票据已删除（未编译，待 Codex 验证），开发与生产同一份代码，都连接同一逻辑 SQL 库（TiDB，或 `Backend: mysql` 的 MySQL 主库，见 `player-data-tidb-scaling-plan.md` §5.4）。故障时拒绝新提交并查询原意图，不回落 Redis 建票/随机机房。队伍名单、邀请展示、在线位置继续使用既有服务；成局票据、全员占用、原业务意图、执行目标和创建请求以 SQL 为权威。

### 13.1 逐成员延迟与调度

客户端取得绑定玩家、连接代次、机房与过期时间的签名探测计划，直连 battle 探测端执行三次随机挑战/回显。battle 按服务端单调时钟测 RTT，签名完整回执；客户端提交原回执，不提供自报延迟。match 校验签名域、连接绑定、机房唯一性、样本数和有效期。逻辑 zone 不能代替物理机房。

排队先选择全体成员共同合格的本区候选；只有全体成员从最初入队时刻达到 `CrossRegionWaitSeconds` 后才允许跨区候选，每个成员仍受 `HardLatencyMs` 硬上限约束。默认分别为 30 秒、180 毫秒，配置值是策略而非容量证明。预组队不拆开；候选采用有界分页游标。重试保留原票号、入队时间和等待窗口。指定整队、活动和点名切磋已有完整名单，直接选择全员可达的共同机房；这些入口不会假造一个排队成功的 Redis 票据。

2026-10-20 提交点补修：候选扫描之后可能等待玩家行锁，两版必须在持锁成局事务中以数据库时间重新检查整张票据的 `expires_ms`，不能只检查所选机房的回执。一份 profile 的到期时间取全部回执的最早到期值，所选机房回执仍有效不代表整张票仍有效。Go `claimTx` 补齐此项，Java 原有等价校验；两版新增确定性真 TiDB 回归，在扫描原票后把其中一票置为已到期，断言全组均未占用且未创建 formation。本次未执行构建或真库回归，不能把用例存在当作通过证据；当前宿主资源与串行门禁不满足。

### 13.2 原请求与提交点

- 排队与一人档：客户端发包前将完整 Join 请求原子写入本机单槽，再使用完整规范 UUID 的两段 uint64 作为 SQL 意图主键。重复请求回原票，不能因为旧票已结束而生成新票。未知 Join 的 `NOT_QUEUED` 不能证明没有迟到写入；Cancel 携带原 UUID，在与 Join 相同玩家行锁事务内写永久撤销墓碑。客户端必须查询到 `request_cancelled=true` 且不在队列后才放弃原请求。
- 整队：`StartTeamMatch.request_id` 为发起端落盘的稳定 UUID。源队伍开战 CAS 后，在成功回包前，将原始请求、全员 SQL 占用和固定 formation 一次事务提交；GetMyTeam 携原 UUID 补拉 `TeamMatchIntent`。空/非法 UUID 明确拒绝，不能按旧客户端空请求号假成功。后台布尔 gather 结果不构成未知成局失败的证据：SQL 终态 4/5 才清源开战锁并推送最终结果，1～3 或查询失败保留原意图。
- 活动：原持久 guild lobby_id 作为 `StartActivityBattle.request_id`，同 guild/同 lobby 重试返回原 formation；不能在后台才发号建票。
- 切磋：原 challenge_id 与应战者组成稳定业务键，同一 SQL 决策锁中争抢接受/拒绝。接受包含全员占用和成局，拒绝写永久决策墓碑；迟到另一决定不能反转。邀请 Redis 投影在 SQL 决定之后清理，投影故障不能改写 SQL 结局。

上述直接入口的规范原始请求保留在 `match_direct_intent.payload`（Java 为 native 同义表），重复键但不同参数拒绝。SQL 多行锁按玩家 ID 排序；任何成员已被占用时整组回滚。成功回包先于后台执行只表示同一 formation 已提交，不表示 battle 已完成创建。

### 13.3 多副本恢复、租约与执行端围栏

`match_formation.retry_at_ms`（Java `retry_ms`）同时承载工作租约的代次与到期时间。各副本通过 SQL 行锁领取 120 秒工作代次；Prepared、SaveIntent、Unknown、Finish 均复核同代次且尚未到期。Unknown 不提前释放原租约；到期接管的原 state=1 只恢复/中止原操作，不再次备战。不是仅靠进程内 running 集合或随机 operation 去重。

冻结 RPC 前先持久精确 scene 端点。PrepareBattle 与 CreateBattle 准入期限不晚于原工作租约截止减 1 秒；scene 在入口和冻结提交点拒过期备战，Java 异步写锁回来后也复核。battle 在插房之前完成确定性原请求序列化并复核准入期限。跨进程绝对期限依赖数据库和节点时钟偏差小于这 1 秒保护间隔，部署必须监控校时与偏差；它不等于已完成跨洲时钟/分区故障演练。

创建意图持久完整 seed、快照、物理机房、battle_id、operation UUID、node instance 和端点。恢复只查询/重发这份原请求。执行端实例不符、内容不同、原准入已过期或已撤销墓碑均拒绝。恢复撤销使用 `DestroyBattle.reason=global_match_abort_if_absent`：在 battle 逻辑线程内仅对尚无房间的精确原操作立墓碑；已经存在的同操作房间保持原成功结局。match 随后查询到 ABORTED 才取消已记录冻结、回队或结束票据；PRESENT 按成功恢复，UNKNOWN 保留。这个条件避免旧 worker 在暂停后误杀另一副本已经建成的同一房间。

**2026-10-22 修复（第一轮审计 §2.5 social-match-battle#0：Battle 实例重启或被替换后票据永久卡住）。** 上一段"查询到 ABORTED 才释放"只适用于已持久 Create 原件的成局。原件持久之前（领租约、备战、探测目标能力阶段）目标实例一旦消失、拨号或查询失败、或同端点换成新 UUID（目标 UUID 不符时 Battle 空回包），就永远拿不到 ABORTED，state 2 票据无出口；已持久原件的成局虽有同日 `RecoverExpiredOutcomes` 的到期 SQL ABORT，但每轮恢复或重放开局都会重新领租约、把 `retry_at_ms` 推后 120 秒，到期 ABORT 要求租约已过期，于是被一直推迟。修法分两条，都只认 SQL 正面证据：

- **无原件**：Create 只在同一租约下 `SaveSupportedIntent` 提交成功、发送前 `CheckWork` 仍有效之后才发出；租约被接管后旧 worker 的写入与发送都会被拒。所以本代租约下没有原件（`Store.CreateIntentPersisted` 返回 false）就证明原 operation 从未、也不会再建房，`globalDestroy` 不再等目标 Battle 应答，直接走 `globalAbortPrepared → Finish(false)` 解冻并回队或结束原票；`Finish` 在行锁内复核同一租约且仍无原件，多副本只有持租者能提交。
- **有原件**：一次查询超时、拨号失败或实例消失只记 UNKNOWN，截止前绝不释放（Create 可能在途，提前释放会让同一玩家进两局）。截止后 `AcquireWork` 回 `ErrAwaitingExpiredOutcome`，`Pending` 也不再列出，原租约最多 120 秒后到期，由 `RecoverExpiredOutcomes` 在原 P→formation 锁内争唯一 SQL ABORT（`match_battle_completion` 主键保证只有一份）；迟到的 COMPLETE 只能读到 ABORT。

不采用"注册中心里目标 UUID 消失超过宽限期"作证据：etcd 租约到期不等于进程已死（分区时旧进程可能仍在跑这局），同端点出现新 UUID 也只说明端点被复用；两者都不能排除旧进程迟到提交。释放后玩家冻结与 `battle:lock` 仍按原路径解除：发房前收尾逐人 `CancelBattlePrepare`，失败时 scene 按备战期限兜底；到期 ABORT 不调用解冻，scene reaper 按备战期限（未确认开局）或同一权威截止（已确认开局）自行解除。Battle 侧不需要改：`QueryBattleCreation` 对不符的目标 UUID 空回包，match 按 UNKNOWN 处理即是 fail-closed。第一轮 §4 第 11 条（已打完的战斗被恢复误判为未建成、票据回队）已由同日「已开局后丢失终局的恢复接缝」覆盖：v1 房间先取得 COMPLETE 再删房，`Finish(false)` 拒绝有原件的失败收尾，本修复没有改那条路径。回归在 `go/match/internal/authority/formation_recovery_exit_test.go`（真 TiDB）；未编译、未测试，待 Codex 验证。Java 版同类入口（`GlobalMatchService.abort` 无原件时同样要求固定执行端回 ABORTED）待做。

**2026-10-22 修复（评审补，social-match-battle#0 评审意见；Go，未编译、未测试，待 Codex 验证）。**

- **回归测试补到 logic 层**：上段的 Store 层用例只证明了 `CreateIntentPersisted` 与 `Finish(false)` 的既有前提（`Finish(false)` 无原件时修复前就能成功，`AcquireWork` 本来就互斥），把 `globalDestroy` 改回旧代码它们照样全绿。`globalDestroy` 的依赖收窄成接口 `formationDestroyAuthority`（`CreateIntentPersisted` / `RecordBattleAdopted` / `Finish`，生产实现仍是 `*authority.Store`），`go/match/internal/logic/global_destroy_test.go` 用替身直接调生产函数，目标端点指向不监听的 `127.0.0.1:1`，不起 TiDB、不起 Battle：
  - 无原件必须返回 true，且不调用任何提交。改回旧代码会去拨号，`DestroyBattle` 失败返回 false，用例转红；
  - 有原件、目标不可达返回 false（fail-closed）；租约失效或读取失败返回 false。
- **计数挪到提交之后**：`global_flow_total{stage="abort",outcome="no_create_intent"}` 原在 `globalDestroy` 判定时就计，之后租约变化、`CheckWork` 或 `Finish(false)` 失败都没有释放，值班会把「尝试」读成「已释放」。改为在 `globalAbortPrepared` 的 `Finish(false)` 返回后计：成功 `no_create_intent_released`（同时打 Info 日志），失败 `no_create_intent_unconfirmed`。`no_create_intent` 这个取值不再出现。
- **被门禁挡住、进不了到期恢复的成局要有信号**：`AcquireWork` 的门禁只看「v1 + 有原件 + 已过截止」，`RecoverExpiredOutcomes` 还要求完整的 `recoverableOutcomeShapeSQL`（能力证据齐全、`preflight_instance_id = target_instance_id`、operation / target 非空、`(adopted_outcome_observed, adopted_outcome_protocol_version)` 为 (0,0) 或 (1,1)）。形状不合格的成局过截止后领不到租约、不进 Pending、也不会被到期恢复选中，票据静默停在 state 2/3。修复前同样没有出口，但每 120 秒的租约轮回至少会出 `recover/unknown` 计数。现在每个 match 副本每分钟跑一次只读巡检（`logic/global_expired_audit.go` → `Store.CountStrandedExpiredFormations`），不为 0 就计 `global_flow_total{stage="recover",outcome="expired_unrecoverable"}` 并打 ERROR，只带数量，不带 battle_id。Battle 明确采用旧协议（adopted 为 (1,0)）的成局按设计保持 UNKNOWN，同样落在这一类。
  - 值班查询：`sum(increase(match_global_flow_total{stage="recover",outcome="expired_unrecoverable"}[10m])) > 0`（各副本各自计数，用 sum）。
  - 处置：在全局 TiDB 只读查明细，`SELECT battle_id,state,operation_id,target_instance_id,preflight_instance_id,preflight_supported_version,adopted_outcome_observed,adopted_outcome_protocol_version,deadline_ms FROM match_formation WHERE state IN(2,3) AND outcome_protocol_version=1 AND LENGTH(create_request)>0 AND deadline_ms>0 AND deadline_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)`，逐局对照 `recoverableOutcomeShapeSQL`（`ready_recovery.go`）找出不合格的字段。形状不合格说明原件或能力证据有问题，不能自动作废：这一局可能已经打完。先到目标 Battle 实例确认房间与结果，按事故处理；不得手改 `match_formation` / `match_authority_ticket` / `match_battle_completion` 伪造终局。
  - 回归：`authority/formation_recovery_exit_test.go` 的 `TestFormationExitTiDBStrandedExpiredFormationsAreCounted`（真 TiDB）。
- **残余**：
  - 形状合格、但 Create 正文与成局字段对不上（`supportedOutcomeOriginal` 失败）的成局，`RecoverExpiredOutcome` 回 UNKNOWN、不报错，SQL 计数判不出，仍然静默。
  - `runGlobalFormation` 遇 `ErrAwaitingExpiredOutcome` 静默返回，仍没有 logic 层自动化覆盖（要真库编排）；门禁本身由 Store 层 `TestFormationExitTiDBExpiredIntentLeavesOnlyThroughSQLAbort` 覆盖。
  - **截止竞态（评审 should_fix，本批未做，归并行 v1 终局接缝轨道）**：权威截止与 Battle 房间 deadline 是同一个值。打满时限的强制平局，以及截止前几秒打完、但 `CommitBattleOutcome` 因 match 短暂不可达而晚到的正常结果，都会与截止时刻的到期 ABORT 抢 `match_battle_completion` 同一主键。上段门禁让 state 2/3 也是截止一到即可 ABORT（原先反复续租意外地把 ABORT 往后拖）。不会重复发放，但合法终局会被作废，玩家少一局奖励。建议在并行轨道验收前修：给 `abortTerminalTx` 与 `RecoverExpiredOutcomes` 候选 SQL 的截止加一段终局提交宽限（deadline + 分钟级上界，参照 Battle 侧 v1 终局重试节奏）；门禁仍以截止为界，截止到宽限结束之间不发租约、也不 ABORT，只等 COMPLETE。`direct_cancel.go` 同样调用 `abortTerminalTx`，改时要一并核对取消入口的语义。同批补「截止后宽限内 COMPLETE 优先」的真 TiDB 用例。
- **Java 版**：同类入口仍待做（见上段），实现时一并满足上面三条。

### 13.4 评级与连续投榜

评级、对局摘要去重和连续来源 outbox 游标同一 SQL 事务；同对局不同内容拒绝。来源停止与新评级争同一 source 行锁，返回真实最终游标；停写后不再产生该代新 outbox。发送只从已确认游标的下一项顺发，重启沿原 request_id 重投。封榜前同时要求来源已停止且 committed_cursor=produced_cursor，再交通用排行榜验证预期游标。Java 使用原生 RankAdminService / Triple 20917、group rank 和按 full method 绑定的服务签名；不借用 Go gRPC 地址。

全球模式的 Kafka 结果位点只在 SQL 入账确认之后推进。Go 每轮最多重试原消息三次，仍未确认则关闭 reader、退避重建并从 broker 原提交位点恢复；损坏协议同样保留位点。Java 数据错误（含 SQLState 22/23）暂停分区并重试原记录，损坏协议关闭该轮，由既有监督器恢复。提交前崩溃由同一 SQL 入账摘要收据去重。显式旧兼容测试保留原“软评分跳过”语义，生产默认不可选择该策略；不能因一条未入账记录失败而提交后面的位点。

执行端创建收据在房间存活期间不能清理，包括超过原 deadline 仍等待结果确认的房间；离房后至少保留到 `max(deadline_ms, admit_before_ms) + 60秒`，两版同口径。原准入窗口结束不表示房间已结束，下一次准入清理不能遗忘仍活房间的 operation 和实例身份。

### 13.5 已有运行证据与明确边界

Go 全包真实 TiDB 运行见 `build/match-client-global-20261015/go-match-lease-5.log`（authority 57.481 秒、logic 54.996 秒、team 38.369 秒均通过）；包括原请求墓碑、Join/Cancel 并发、全组占用、原直接意图重放、失租后所有提交拒绝、评级事务与停写屏障。后续接受/拒绝并发和 Team 未知结果收尾的增量证据另附最终日志。

Java 隔离快照 `D:/luyuan/xuanming-server-mmo/build/match-global-20261015/tests-lease-focused-6.log`：16 模块构建通过，真 TiDB 9/9、PrepareBattle 45/45、CreationLedger 3/3，无跳过这些指定用例；完整模块回归单独登记，不把指定测试替代整包。客户端 `unity-team-file-3-results.xml`：59/59、0 跳过，包含 RegionProbe 7、真实 loopback GateTcp 生命周期 10、原子 Join 恢复文件 3、TeamClient 35、Commerce 恢复文件 4。恢复文件使用同目录原子提交、WriteThrough/Flush(true)、按原字符串 CAS 转换/删除；坏记录保留保护态。

这些是本机隔离依赖与确定性故障用例。尚未执行跨洲多进程完整匹配/发奖链、实机逐成员 RTT/断网恢复、持久数据升级灰度、机房容量/分区故障或两千万 CCU 压测；不据单元测试宣称全球上线容量或完整端到端强一致性已验收。首次切换须升级两版服务和客户端契约、明确停旧写者/排空旧 Redis 队列与评级迁移方案，旧权威与新 SQL 权威不能混写。

结果源的旧缺陷是 `BattleRoomManager::DispatchActivityResultDurably` 在 Redis 不可用或持久化失败时只尝试投递一次，Java 普通结果失败也只有兜底日志。第 13.6 节为全球模式增加独立于 Redis 投影的 broker 确认屏障；显式本地旧兼容模式不作为全球生产端口。Kafka 保留期仍为 7 天，须在保留窗口内消除消费积压；消费侧不跳位点和单元测试不能替代完整多进程发奖恢复验收。


2026-10-16 后续验证补充：Java最终指定真TiDB `tests-match-sql-final-9.log` 共12/12、0跳过，其中SQL10/10（含接受/拒绝并发互斥与失租写拒绝）、规范UUID128校验2/2；Go `go-authority-team-final-7.log` authority124.395秒、team50.382秒均通过，`go-global-entry-validation-8.log` 在入口SQL读取前拒绝空/非法/nil UUID，`go-vet-entry-final-8.log` 无诊断。两版源码默认生产端口保持SQL权威；原Redis故障模型的既有测试显式选择兼容端口，保留原断言，另增默认durable分支终态/UNKNOWN回归。

直接活动的客户端探测接线：GuildClient.StartTrial 与 RespondTrialInvite(accept=true) 都先经 GuildUiRoot 注入的 RegionProbe.Prepare 提交本人实测回执；发起者与每位同意者均不依赖曾打开排队页或加入普通队伍。换连接/Reset后的旧探测完成不会发业务RPC。普通队伍成员已有 TeamClient 后台探测，挑战发起/同意两侧均显式探测；GameClient每帧驱动测量。两版match均在全员profile有效前拒绝直接成局；过期或缺失不能用队长延迟顶替。Java帮会五活动原占位接缝由独立续批补齐，match入口本身的SQL验收不能替代该源端链路。


客户端直接活动补验：`unity-guild-final-5-results.xml` 实际201/201、0跳过（GuildClient142含新增2条，及前述匹配/Team/文件59）。相同轮次编译当前隔离runtime无错误；前一轮复制过程中遇到共享工作区SyncDigest构造接缝的中间快照，保留`unity-guild-final-4.log`，仅刷新外部最新原源后绿，没有覆盖/回退外部工作。

### 13.6 战斗结果提交点（2026-10-16，结果源修复实施中）

全球对局的结果日志提交点是 Kafka `acks=all` 的逐消息成功回执，不是客户端收到了胜负、producer 本地入队成功或 Redis SET 返回。结果 topic 部署必须三副本、`min.insync.replicas=2`，producer 幂等及 all ACK 不得静默降级；Kafka 提交只确认耐久结果日志，评级与帮会账本仍分别由 TiDB 事务和原 battle_id 摘要收据负责。

最终回合一次计算并固定原事件（包括 finished_at_ms）及原终局帧。成功回执之前保留原房间、拒绝新行动、按有限退避重试同一份字节；139/150、观战终态、个人结算的派发命令、房间删除均延后。个人结算的 pending 在结果第一次发送之前已落库，见本节末「2026-10-19 更正」。GetState/新观众首帧只能读取上一个非终态快照。回合终局和 deadline DRAW 共用此提交接缝，不能只修活动分支。未确认的旧发送可能实际已落 broker，重投允许重复但不能改结果；丢失响应不是输赢失败证明。

明确销毁不能在结果未知时先推 ABORTED 后再让原结果生效；停机断开待提交房间的连接，保留客户端 UNKNOWN 语义。进程在结果提交前崩溃仍可按未完成战斗处置；broker 已确认的原事件，其评分与帮会账本由 SQL 消费者幂等恢复，个人结算不由 SQL 恢复，靠发送前已落的 pending（2026-10-19 更正，原句「broker 已确认的原事件由 SQL 消费者幂等恢复」对个人结算不成立）。Redis 活动副本作为补拉投影保留，但单次无耐久投递不能构成生产成功。跨洲完整进程链和超过 Kafka 保留窗口的运营恢复仍须另验。

Go 全球启动在 matcher 和 RPC 准入之前同步核对结果 topic，独立于 RatingEnabled，失败拒启。Java match 在装配阶段做相同强核验，关闭评分消费也不绕过；battle 生产者只有核验后才发送。既有 RF1 topic 拒绝启用并要求人工迁副本，不自动降低要求；owner 显式固定 topic 级 minISR 至少 2（保留更高值）和 unclean election=false。Guild 消费者只验证既有 topic，不抢先建 RF1。

本机结果源回归：`tests-global-result-red-16.log` 在真实 Java RoomService+引擎上稳定复现“未确认已删房”（1条失败），`tests-global-result-focused-17.log` 46/46通过；扩展 `tests-global-result-policy-18.log` 85/85、0跳过，包括真实房间4条、Kafka传输29条、活动投影19条、topic策略15条、消费运行时18条。Go `go-match-topic-final-12.log` 全包及对应 build/vet 通过，覆盖生产入口预检；完整 Java 模块和 C++ 构建另附最终证据。个人 BattleSettlementData 的完整资产载荷不在 BattleResultEvent 中，这个结果确认屏障不等于普通掉落与玩家存盘整链的原子提交证明。

2026-10-16 Java最终模块补验：隔离副本 `tests-global-result-all-final-26.log` 的12模块test reactor成功，5313总项中4862实际通过、0失败/错误，451项既有门控集成未运行；match为1468通过/303未运行，battle为525通过/19未运行。结果屏障4、活房创建收据5、topic策略15、消费运行时19、Kafka发送29、活动投影19共91项实际通过，952源文件与当前Java主仓SHA对账一致。`java-match-result-final-proof-20261016.json`记录逐模块数字与源码摘要。前序完整回归保留RF1旧fixture、API fork异常和EVALSHA旧mock的红日志；API最小5/5、四个mock夹具37/37绿后才恢复完整回归，没有放宽生产RF3/ISR或请求截止断言。本轮没有执行真实3broker战斗结果完整进程链，默认门控未运行不算通过；普通R07个人结算与多区域故障、容量边界维持本节上述限制。

**2026-10-19 更正：个人结算的耐久提交点前移到结果发送之前**（审计 social-match-battle#2，见 [data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.4）

- **原结论与代码不符。** 上文说「broker 已确认的原事件由 SQL 消费者幂等恢复」，这只对评分与帮会账本成立。`match_battle_settlement_source` 只归档个人结算来源，没有任何读者，不会补发个人结算。
- **旧代码的走向：**
  - 个人 pending 只在 all ACK 之后由 `FinishBattle` → `DispatchSettlementDurably` 写入。
  - 停机时 `AbortAllRooms` 先 `StopResultCommits` 再清房，框架随后 flush producer。结果到了 broker，评分与帮会历练照常入账，个人结算却永不派发。
  - 回执处理前崩溃，同理。
- **修复（C++，2026-10-19，未编译、未测试，待 Codex 验证）：**
  - 结果第一次发往 broker 的前置条件，改为全部成员的个人结算已写进 R07 pending。由 `ResultCommit::BeginStaging` / `FinishStaging` 把关，`BattleRoomManager::StagePersonalSettlements` 按成员 EVAL 原 `kSetPendingSettlementScript`。
  - 字节统一取 `BuildPersonalSettlementPayloads(冻结原件)`。
  - 回执之后，`FinishBattle` 走 `DispatchStagedSettlement`：只投 SceneCommand、登记发件箱，不再改写 pending。原因是回执前 scene 可能已应用并销账，改写会让 pending 复活，玩家重复领奖。
  - 停机与崩溃都不删已落的 pending，由 scene 登录钩子或冻结到期补读应用，账本与锁照常判重。
- **语义变化（有意接受的代价）：**
  - 结果最终没有进入 broker 时，个人奖励照常应用，评分与帮会历练不计，即「奖励有、评分无」。这包括发送前崩溃，以及一直未确认之后的停机或崩溃。
  - 反方向的「评分有、奖励无」，原先每次带在途结果的停机都会发生，现在不再出现。
  - scene 可能先于回执应用个人结算（玩家重登、冻结到期补读）。回执后的派发命令按账本与 `battle:lock` 判重，不会重复发奖。
- **可用性变化：** 确认模式在 zone Redis 不可用时，不再降级为「不落库直接发」，结果发送等 Redis 恢复后再进行。这期间房间保留、拒绝新行动，与等 broker 回执同一形态。
- **未变的残余：**
  - pending 每玩家单槽，下一局覆盖上一局（turn-battle-gap-closure.md §9.3 残余）。
  - battle 节点的 Redis / Kafka 编排没有单测接缝。停机与崩溃场景只有纯状态机单测，以及 PROGRESS 同日条目里的可选冒烟步骤。
- **Java 版**（0.1.0-SNAPSHOT）：只读核实，`BattleRoomServiceImpl` 同样在回执后才落 pending，`abortAll` 不派发，**待做**。

**2026-10-19 修复：落库重试只写没落定的成员，轮次设上限**（第二轮审计 r2-battle#1、r2-battle#4，见 [data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.3、§3.4；C++，未编译、未测试，待 Codex 验证）

- **r2-battle#1（上一段修复的缺陷）**：落库重试轮原来对全部成员无条件 SET。第 1 轮已写进去、随后被 scene 应用并销账的成员，pending 复活，下次登录重复领奖。
  - `ResultCommit` 按成员记结论。`UnstagedMembers` 只剩没拿到确定结论的成员，`FinishStaging` 只移出本轮落定的成员，后续轮次只写其余成员。
  - 结论未知（空应答、ERROR、命令没发出）的成员下一轮照写。命令其实执行过的情形，由新脚本 `kStagePendingSettlementOnceScript` 判出：同一条 Lua 先查本局落库标记 `battle:settlement:staged:{battle_id}`（SET，成员为 player_id），标记在就返回 2、不重写。标记与 pending 两键在同一条脚本里写下，销账不碰它。全部落定后 battle 立即删除标记；停机、崩溃、轮次用尽留下的 24 小时后过期。
  - 没有采用「`battle:lock == battle_id` 才写、锁不在按已收尾」。全球模式下锁在落库前就可能合法缺席：续期没落地时战斗中途自然过期；整场期限到时 scene reaper 先条件删锁；重试轮拖过期限 + 60s。按已收尾处理会静默丢掉合法结算。所以锁不在时照写，与改造前一致。只有锁属于别的局时不写（返回 3）：scene 的派发路径（在线时 InBattleComp 不符、或无组件时锁值不符 → 销账；离线暂存锁不符 → 销账）本来都会丢弃这一局的结算；不看锁的登录钩子（`CheckPendingSettlementOnEnter` → `ApplyPendingSettlement`）与冻结到期补读，改造前在旧局写下、新局覆盖之前原本可能应用它，现在为了不让旧局覆盖新局的单槽 pending 而有意放弃（2026-10-22 措辞更正：原句「scene 每条路径本来都会丢弃」不准确）。
- **r2-battle#4**：落库轮次上限 `kMaxStagingRounds = 24`，只数 Redis 连得上时开出的轮，配合 1→30s 退避约 9.5 分钟。（2026-10-22 起改为快慢两段重试 + 6 小时总跨度，见下段「2026-10-22 修复」；本条以下三点里「到上限」均读作「到总跨度上限」。）
  - 到上限后不放弃落库直接发结果（那会退回上一段的原缺陷），终局保持未知。打一次 `metric=battle_personal_settlement_staging_exhausted`，并逐个列出没落定的成员。
  - 房间移出提交 pump，但留在房间表里：结果不发、DestroyBattle 不作废、新行动照旧拒绝。
  - 节点因此不会自然排空。值班排除 zone Redis 故障后按下段「2026-10-22 修复」的三步收尾（先排空、等其余房间打完、再删 GameServer；原写的「直接删除该 Pod」会误伤同节点正在打的房间），最终由 `AbortAllRooms` 清房：已落定成员照常应用，没落定的按作废处理，与 r2-battle#2 是同一个残余。值班查询登记在 `deploy/k8s/asset-channel-alerts.yaml` 文件头。
- **残余**：battle 节点单边连不上 Redis 超过 24 小时，标记会过期（2026-10-22 已由 6 小时总跨度堵上，见下段）；按命令粒度的 ACL 误配置（放行 SET、拒绝 SADD）；Redis 主从异步复制丢掉已确认的写，与销账丢失同属既有一类。
- **Java 版**：确认模式落库尚未实现（见上一段「待做」），实现时要同时满足这两条。

**2026-10-22 修复：落库重试先快后慢、6 小时总跨度兜底，滞留房间周期提醒**（上段 r2-battle#1/#4 修复的评审改进项；C++，未编译、未测试，待 Codex 验证）

- **墙钟跨度上限（堵住真实缺口）**：上段的上限只数 Redis 连得上时开出的轮，没有墙钟上限，标记 TTL 能否兜住取决于连通性。时序：某成员上一轮已执行但应答丢了，标记里有他；随后 battle 单边断连或 Redis 反复闪断，凑满 24 轮的跨度超过 24 小时；期间 scene 应用并销账；重连后标记已过期、锁也已随销账删掉，下一轮照写，pending 复活，登录时重复发奖。
  - `ResultCommit` 在第一次问起落库时（`ExhaustStagingIfOverdue` / `BeginStaging` 的第一次调用，用调用方传入的 steady_clock 时间点，不读墙钟）记下跨度起点；满 `kMaxStagingSpan = 6h` 即不再开轮。
  - `StagePersonalSettlements` 先判跨度、再查 Redis 连通性：Redis 一直断着也会到点判用尽、出告警。有轮次在途时不判，等它的回复到齐（可能恰好落定）。
  - 推导：标记 TTL（`kStagedSettlementMembersTtlSec` = 24h）从最后一次写入算起，写入不早于跨度起点，所以跨度只要小于 TTL，任何一轮开出时标记必然还在。取 6h，留 4 倍余量（Redis 主机墙钟跳变、命令在输入缓冲里滞留）。`battle_room_manager.cpp` 的 `static_assert(kMaxStagingSpan * 4 <= TTL)` 与单测 `StagingSpanLeavesStagedMarkerTtlMargin` 守住这个关系。
- **有界自动恢复**：上段 24 轮快速重试用尽即永久停住，Redis 恢复后也只能人工删 Pod 收尾（没落定的成员作废、结果不发）。有了本局落库标记，重写不会复活 pending，停住保证的只是有界，不是正确性。
  - 现在前 `kFastStagingRounds = 24` 轮按 1→30s 退避（第 24 轮在第 1 轮之后约 571s 开出），之后每 `kSlowStagingRetryInterval = 5min` 一轮，到 6h 总跨度才最终判用尽。6 小时里共 94 轮（快速 24 + 低频 70），每轮只写没落定的成员，对 Redis 的压力可以忽略；次数、退避、总时长都有界，仍满足 AGENTS §11.3。
  - zone Redis 故障排除后结果自动发出，不需要人工处置。
- **周期提醒**：上段的 `staging_exhausted` 每房间只打一行，值班查询窗口一过就再无信号。积压报数定时器（60s，只绑 `weak_ptr<自己>`）每轮先调 `ReportExhaustedStagingRooms`：从房间表现数总跨度用尽的房间，不为 0 就打 `metric=battle_personal_settlement_staging_exhausted_rooms count=N node_rooms=M`，battle_id 列表只放正文、不作 label。它不依赖 Redis；判用尽时会补装定时器，Redis 从没连上、没开过轮的节点也照报。值班查询改为按这一条判，房间清掉之前一直会响。
- **处置改为三步**（原写的「排除 Redis 故障后删除该 Pod」会把同节点上正在打的房间一起作废）：先排除 zone Redis 故障，然后
  1. `kubectl -n mmorpg-infra label gs <name> mmorpg.io/drain=true`（`cpp/nodes/battle/main.cpp` 的 `kAgonesDrainLabelKey`；进程 5s 内读到后拒绝新房间）；
  2. 等提醒行的 `node_rooms` 降到与 `count` 相等，即其余房间都已打完（单局上限 = match `BattleMaxDurationSeconds`，默认 300s）；
  3. `kubectl -n mmorpg-infra delete gs <name>`，优雅停机由 `AbortAllRooms` 清房。
  - 非 Agones 的 Deployment 形态（只用于验证 / 回退）没有排空标签，新房间会继续进来，第 2 步等不到，只能择低峰删 Pod。
  - 同一说明写在 `deploy/k8s/asset-channel-alerts.yaml` 文件头与 `battle_room_manager.h` 的 `AbortAllRooms` 注释。
- **措辞更正**：上段与 `settlement_outbox.h` 里「锁属于别的局时 scene 每条路径都会丢弃这一局的结算」已改准。派发路径会丢弃（在线时 InBattleComp 不符、或无组件时锁值不符 → 销账；离线暂存锁不符 → 销账）；登录钩子（`CheckPendingSettlementOnEnter` → `ApplyPendingSettlement`）与冻结到期补读不看锁，改造前旧局写下、新局覆盖之前原本可能应用，现在为了不覆盖新局的单槽 pending 而有意放弃。battle 的 `battle_personal_settlement_superseded` 日志同步改写。
- **测试**（注入时间点，不依赖墙钟）：
  - `turn_battle_engine_test`：`BattleResultCommit.StagingContinuesWithSlowRetriesAfterFastRounds`、`StagingStopsAtTotalSpanWithoutReleasingResult`、`StagingSpanExpiresWithoutOpeningRedisRounds`、`StagingRoundsStayBoundedAcrossTheWholeSpan`、`StagingSpanLeavesStagedMarkerTtlMargin`；编排接缝 `StagingRoundTally`（每轮成员在开轮时定下、每名成员只记第一次回复、恰好收口一次）由 `BattleStagingRoundTally.*` 覆盖，其中一条把它与 `ResultCommit` 按生产编排组合：只对没落定的成员开下一轮、全部落定才放行。
  - `bag_test` 真 Redis：`PendingSettlementContract.ConfirmedStagingPartialWriteOnBrokenIndexSettlesNextRound`。积压索引被写成字符串时 ZADD 报 WRONGTYPE，脚本回 ERROR，但 pending 两键与标记已写；下一轮返回 2，pending 不变。这是脚本「ZADD 放最后」的运行证据。
- **残余**：
  - 全球局落库晚于冻结到期时，在线成员的结算会在派发时被删除：在线且无 InBattleComp、锁不在 → `AckSettlementPending` 规则 2 → `ClearPendingSettlementIfMatch`（`player_battle.cpp` 约 :2094-2100）。上段「锁不在照写以免丢合法结算」只对离线玩家成立。与 [data-consistency-guarantee-map-20261020.md](data-consistency-guarantee-map-20261020.md) 的 cross-service-assets#1 同一分支、同一根因；另有会话在修，截至本段写入时 `player_battle.cpp` 这段未改。后续可评估该分支在 `pending:id` 等于本局时改走补应用、不再直接销账。
  - 在途轮次一直不回复（连接挂死而没断开）时既不开新轮也不判用尽、不出提醒，靠 hiredis 连接的存活探测摘连接、回调拿到空应答后收口。
  - 落库编排本身（按成员 EVAL 与 KEYS/ARGV 顺序、落定后 DEL 标记、用尽时移出 pump、周期提醒）仍在两个单例里，没有端到端单测；替代验证是上面的状态机与接缝单测、真 Redis 脚本契约，以及 PROGRESS 同日条目里的可选冒烟。
  - 序列化失败 / 成员对不上的「终局未知」占位房间同样会滞留在节点上，只有一次性日志，不在周期提醒里（程序缺陷类，非本条范围）。
- **Java 版**：确认模式落库仍待做（Java `PARITY.md` 已把 §2.4 登记为待做，不另起一行），实现时一并满足跨度上限、低频重试与周期提醒。

**2026-10-22 修复（评审补）：锁不在时看 pending 槽归属；滞留提醒覆盖所有排不空的房间；低频阶段落定额外报数**（上段 r2-battle#1/#4 改进项的评审意见；C++，未编译、未测试，待 Codex 验证）

- **锁不在照写会旧盖新（评审 must_fix）**：「锁不在照写」不看 pending 槽当前属于哪一局。时序：B1 落库时成员 X 一直没写进去（battle 单边连不上 zone Redis，或该节点命令持续报错）；X 的冻结在 B1 截止时解除，接着打完 B2 并下线；B2 的 pending 两键已写、还没消费，锁 B2 在期限 + 60s 后自然过期；B1 恢复后再开一轮，锁为 nil，照写，B2 的 pending 被 B1 覆盖；B2 的发件箱探测到 `pending:id ≠ B2`，按已销账停投，B2 奖励永久丢失。上段的低频阶段正是为长时间故障后自动恢复设计的，它把这个窗口从约 9.5 分钟拉长到 6 小时，恢复后最常见的情形就是覆盖。
  - 修法：`kStagePendingSettlementOnceScript` 在锁判定之后、第一条 SET 之前加一条守卫，锁不在且 `pending:id` 属于别的局时返回 3、不写。锁 == 本局时照旧覆盖：锁证明本局是当前局，槽里如果是别的局只会更旧。锁不在而槽空、或槽已属于本局（scene 离线暂存写过同一份）时照写。
  - 返回 3 之后本局对 X 的派发都碰不到 B2：在线派发的条件销账只删 `pending:id ==` 本局的记录，离线暂存锁不在时什么都不写，发件箱按 `pending:id ≠` 本局停投。
  - 代价：锁不在而槽里恰好是更旧一局（已应用、等落盘销账；或被冻结 / 封禁暂缓应用）时，本局对该成员的奖励放弃。单槽 pending 下两局只能留一份（turn-battle-gap-closure.md §9.3 残余），这里选不覆盖，资产路径 fail-closed。battle 日志 `battle_personal_settlement_superseded` 的说明同步改写。
  - 上两段里「锁不在时照写」都读作「锁不在且槽空或属于本局时照写」。这条路径在 HEAD 已提交代码里是全球主路径；并行 v1 终局接缝落地后，只剩 `outcome_protocol` 0 的房间可达。
  - 测试：`bag_test` 真 Redis 新增 `PendingSettlementContract.ConfirmedStagingWithoutLockNeverOverwritesAnotherBattlesPending`：锁不在、`pending:id = 999`、blob 已在时返回 3，blob、id、索引成员都不变，标记里没有该成员；锁 == 本局时覆盖；锁不在而槽属于本局时照写。修复前第一步返回 1、三条值断言全红。静态用例 `ConfirmedStagingDecidesBeforeAnyWriteAndMarksAfterPending` 增加「槽位守卫早于两条 SET」与「只有锁判定、槽位守卫两处 return 3」的断言。
- **滞留提醒覆盖所有排不空的房间（评审 should_fix）**：上段的周期提醒只数落库用尽的旧协议房间。另两类同样滞留，节点同样排不空，却只有一次性日志：
  - 并行 v1 轨道的房间：SQL 终局重试到 6 小时上限（或终局原件生成失败）后 `outcomeExhausted = true`、移出 `sqlOutcomeRooms_`，`HandleDestroyBattle` 对 v1 直接返回；
  - 序列化失败 / 成员对不上的占位房间。

  同节点只要有这两类房间，处置第 2 步「等 node_rooms 降到与 count 相等」永远等不到。并行 v1 改动落地后新全球局一律走 v1，原提醒只剩旧协议房间。现在 `ReportExhaustedStagingRooms` 按 `turnbattle::ClassifyStuckTerminal`（`result_commit.h`）一起数：同一行 `metric=battle_personal_settlement_staging_exhausted_rooms`（名字不改，值班查询不变）增加 `sql_unconfirmed=`、`placeholder=`、`stuck_total=`（三者之和）；`count=` 仍是落库用尽的房间数；正文每个 battle_id 后带类别。处置第 2 步改为等 `node_rooms` 降到与 `stuck_total` 相等。v1 判用尽、原件无效与占位创建时都补装报数定时器：v1 房间不走 R07 落库，只跑 v1 的节点此前从不装填。`deploy/k8s/asset-channel-alerts.yaml` 文件头与 `battle_room_manager.h` 的 `AbortAllRooms` 注释同步改。上段残余最后一条（占位房间不在周期提醒里）随之关闭。
  - 测试：`turn_battle_engine_test` 新增 `BattleStuckTerminal.ClassifiesEveryRoomThatCannotDrain`；改回只看 `StagingExhausted` 时，v1 与占位两条断言转红。房间表循环与日志拼接仍在单例里，没有端到端单测。
- **低频阶段落定（评审 should_fix）：cross-service-assets#1 是低频阶段生效的前置条件**。低频阶段从第 1 轮之后约 571s 起，而整场期限不晚于成局后 300s，所以低频阶段才落定的局，结果派发时各成员的冻结必然已解除。派发时仍在线、没有重登的成员（无 InBattleComp、锁已不在），已落库的 pending 会被 `AckSettlementPending` 规则 2 直接删掉、不应用（上段残余第 1 条）。改造前这种房间会停住，已落定成员的 pending 留到下次登录再应用。所以 cross-service-assets#1 的修复（`pending:id ==` 本局时改走补应用、不再直接销账）落地之前，低频阶段对在线成员是净损失。
  - 那条修复落地之前，`OnPersonalSettlementsStaged` 在低频阶段落定（`StagingFailures() >= kFastStagingRounds`）的那一刻打一行 `metric=battle_personal_settlement_staged_late battle_id=… failed_rounds=…`，正文列出整局成员的 player_id（派发对所有成员同时发生，风险不只在最后落定的几人），供事后按 scene 账本对账补偿。值班查询登记在 `deploy/k8s/asset-channel-alerts.yaml` 文件头。
- **本批未做**：评审 test_gaps 里「battle 编排没有端到端单测」（`StagePersonalSettlements` 改为遍历 `personalSettlementPayloads`、删掉 `RetireExhaustedStaging` 里的 `EnsureSettlementBacklogReporter` 都不会红）仍是残余：两处编排都在单例里，要先把 Redis / Kafka 接缝抽出来，超出本批范围。真 Redis 用例在没设 `MMORPG_TEST_REDIS_ADDR` 时 SKIP，Redis 7.2 与 8.x 两轮验收里 SKIPPED 算未通过。
- **Java 版**：确认模式落库仍待做，实现时一并满足槽位守卫、三类滞留提醒与低频落定报数。


帮会历练先在全员原账号／归属修订的角色首锁与帮会锁下持久launch原件，之后才能调用全球Match。全球`StartActivityBattle`要求非零`request_id`与服务端`activity_context.launch_request_id`相等；规范完整Start正文继续作为kind=2、owner=原guild、request_low=原request的永久`match_direct_intent.payload`，不得在超时后换请求、名单或周期。

`CancelActivityBattle`覆盖完整原Start，独立`MMORPG_MATCH_INTERNAL_SECRET_GUILD`配置在main显式注入。请求以caller=guild、方法`/match.MatchInternal/CancelActivityBattle`签名；回复以caller=match、同方法加`/reply`签名，且`request_digest`绑定清空外层auth后的原Cancel摘要。缺密钥、权威SQL或有效证明拒绝；客户端会话入口也拒绝。Start的未签名formation_state仅供信息，不能授权帮会清角色锁。

Cancel与ReserveDirect在同一direct主键事务锁上二选一提交：Cancel先赢在零battle行写永久cancelled墓碑，迟到Start无论预检是否完成都不得越过；Reserve先赢后Cancel只返回原固定battle_id和真实formation状态，不调用运行时、不把取消请求冒充战斗已经中止。相同请求重放只认原完整正文，不同正文拒绝。Commit未知不签成功回执，重放同一原件消歧。帮会只有验证后的零battle取消墓碑，或formation=5的安全中止收据，才同本地原件事务持久回执并终结ACTIVE参与索引；formation=1..4继续保留或等待真实结果。既有EXPIRED允许迟到结果，因此不能当安全中止。新回归源码区分纯鉴权、真实TiDB墓碑／并发与完整运行链；本批尚未运行编译和这些回归。

**2026-10-23 修复：停机 / 崩溃后同局成员结算对称、帮会历练占用有出口**（第二轮审计 r2-battle#2、r2-guild#1，见 [data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.4、§3.7；负责人 2026-10-23 拍板首发开全球匹配后两条均为上线阻塞；C++ / Go，未编译、未测试，待 Codex 验证）

- **复核（按当前工作区）**：两条同根，battle 实例在「终局已定、还没可靠发出」之间消失，下游没有出口。审计依据的是 HEAD 已提交代码：全球局走旧协议确认模式（R07 pending 逐成员落库后才发 Kafka），落库做到一半停机或崩溃即同局不对称；match 编队到 formation=4 之后没有任何迁移。当前工作区里，下文「2026-10-23：已开局后丢失终局的恢复接缝」一节（并行轨道，未提交、未验收）已把全球局改为终局协议 v1，结构上闭合了两条的主路径：
  - **提交点**：battle 在任何个人副作用之前，把冻结的完整原件（含全员个人结算）交给 match `CommitBattleOutcome`。match 在原 P→formation 锁内用一个 SQL 事务写下 COMPLETE、全员个人结算来源、结果与个人两个发件箱，formation 置 4（`completion.go`）。发件箱逐行按 `next_retry_ms` CAS 认领，多副本对同一行同一时刻只有一方投递；个人结算经 PlayerInbox 永久收据去重，只有 APPLIED 才销账；评分与帮会按 battle_id 幂等消费。v1 房间不写 R07 pending，也不发个人直投（`FinishBattle` 对 v1 只推公共终局帧）。所以没有「A 已领奖、B 永不结算」：要么全员都有持久来源、由存活的 match 副本补完，要么全员作废。
  - **作废**：没有 COMPLETE 的局，权威截止之后由 `RecoverExpiredOutcomes`（state 2/3/4，各副本都跑）或帮会重放原 Start 的 `CancelActivityBattle`（`direct_cancel.go` → `abortTerminalTx`），在同一主键上原子提交 ABORT：formation 4→5、全员原票置 4，同一事务。迟到的 COMPLETE 只能读到 ABORT。正面证据是「数据库时间已过权威截止，且该主键下没有 COMPLETE」，不依赖 battle 进程，也不看注册中心（与 social-match-battle#0 同一判据，不把实例 UUID 消失当证据）。截止前的任何一次超时、失联都只记 UNKNOWN。
  - **match 只发 v1**：`gather.go` 发 Create 前固定 `OutcomeProtocolVersion=1`，只经 `SaveSupportedIntent` 持久 v1 原件（`SaveIntent` 已没有生产调用方）；`global_match.go` 的恢复也只重放 v1 原件。审计所指的旧协议确认模式房间，match 侧不再产生。
- **本批补的缺口**：
  1. **battle 优雅停机补一轮有界提交**（`battle_room_manager.cpp` 的 `AbortAllRooms` / `SqlOutcomesFlushed` / `StopSqlOutcomes`，`main.cpp`）。原来 `AbortAllRooms` 一进来就装 v1 停止闸、清空待提交集合，停机前一刻打完、又恰好没有在途提交的局再没有机会提交，只能等 match 截止后判 ABORT，全员白打一局（结果对称，但丢了一局合法终局）。现在：
     - 终局已冻结、还没拿到 SQL 决定、也没判用尽的房间（`turnbattle::NeedsShutdownOutcomeFlush`）立即续发一次，不打断在途的那次；
     - Node 排空谓词（`SetShutdownDrainComplete`）等它们拿到 COMPLETE / ABORT，有界于 Node 排空预算 15s（`node.cpp` `kShutdownDrainBudget`），battle 的 `terminationGracePeriodSeconds`（`tools/scripts/lib/k8s_client_entry.ps1` 的 `New-BattleFleetYaml` 默认 30s）覆盖这段等待；没有待提交终局的停机不多等；
     - 停止闸移到拆除前钩子（`SetBeforeTeardown`：出站通道已关、在途调用的失败回调已跑完），并对仍没落定的局逐局打 `metric=battle_sql_outcome_unflushed_at_shutdown battle_id=…`（battle_id 只进正文）。值班按 battle_id 查 `match_battle_completion`：有 COMPLETE 由发件箱补完，没有则截止后 ABORT；
     - 排空窗口内到达的 COMPLETE 按原回复处理收尾删房（lifecycle 已 Stop，删房只记账、不回 Ready）。还在打的 v1 房间没有终局可交，照旧作废，由 match 截止后判 ABORT。
     - 崩溃路径不依赖本进程：COMPLETE 已落库的由 match 发件箱补完，没落库的截止后 ABORT。
     - 与下文并行轨道「停机后的迟到回包」一段的接缝：那段的验收探针是「排空窗口内先装停止闸，再放行真实 COMPLETE 回复，核对进入停止分支且无副作用」。停止闸改到拆除前钩子之后，排空窗口内的 COMPLETE 走正常收尾（v1 收尾只推公共终局帧、关直连、删房，没有业务副作用）；停止闸之后出站通道已关，真实回复不会再到达，停止分支只剩防御意义。该探针要改为核对「排空窗口内 COMPLETE 正常收尾、拆除前钩子之后无回调副作用」，由该轨道与主会话裁决；同时本修复在有待提交终局时会把排空拉长到最多 15s 预算，与该段「不延长生产排空时间」的表述不同。按上下文那句读作「不为验收而改排空」，本修复是为业务补提交；如该轨道另有约束，以主会话裁决为准。
  2. **帮会释放规则收成一处**（`go/guild/internal/data/trial_launch_repo.go` 的 `TrialCancelReceiptIsTerminal`，`recoverOne` 与 `FinishCancelledTrialLaunch` 共用）：只认零 battle 取消墓碑，或 formation=5 且不是 COMPLETE（发房前中止，或截止后的 SQL ABORT）。formation=5 却带 COMPLETE 的自相矛盾回执，原来会被当作废按 LOSS 收尾，现在 fail-closed 拒绝、等结果事件，恢复器对它报错、不登记。
- **按设计不改的三处**（审计行 r2-guild#1 把它们列为缺口）：
  - `releaseGuildTrialLaunches`（解散）保持空函数：解散不是 match 的终局证明，立即释放会让这批角色在这一局仍可能 COMPLETE 时进入下一局。解散后恢复器照常重放取消，`FinishCancelledTrialLaunch` 容忍帮会已解散；COMPLETE 的局由结算 / 标毒的「帮会已解散」分支终结。
  - `ExpireTrialBattle`（巡检 3600s 判 EXPIRED）不碰 launch：它是 guild 本地超时，不是权威终态，EXPIRED 仍允许迟到结果。常态下权威截止（不晚于成局后 300s）之后的下一轮恢复（10s 一轮）就拿到 ABORT，远早于 3600s；到 3600s 仍占用，说明 match 不可达或成局停在 UNKNOWN（形状不合格，见 §13.3 `expired_unrecoverable`），这时保持占用才是 fail-closed。
  - `checkWork` 仍要求 state<4：4→5 只走 `abortTerminalTx`（截止后、原 P→formation 锁内、与 COMPLETE 争主键），不复用工作租约。
- **测试**（未运行）：
  - C++ `turn_battle_engine_test`：`BattleShutdownOutcomeFlush.OnlyFrozenUnconfirmedV1OutcomesAreFlushed`。停机编排本身在单例里（Node 钩子加 gRPC 注册表），没有单测接缝，替代验证是下面的冒烟。
  - Go match 真 TiDB（`MATCH_IT_TIDB`）：`authority/started_formation_exit_test.go`。`TestStartedFormationTiDBNoResultLeavesOnlyThroughSQLAbortAfterDeadline`：formation=4 截止前巡检两轮不释放；截止后两个副本并发恢复只写一份 ABORT，4→5、票 3→4；迟到 COMPLETE 读到 ABORT，不产生个人来源；再巡检不变。`TestStartedActivityFormationTiDBCancelSignsAbortOnlyAfterDeadline`：帮会取消截止前回 formation=4 / UNKNOWN、不动票；截止后两个副本并发取消只写一份 ABORT，两份回执都证明 ABORT；重放不变。`TestTerminalTiDBCommittedOutcomeIsDeliveredByOneReplicaPerLease`：COMPLETE 落库后 battle 崩溃，两个 match 副本并发投递，同一认领租约内结果只发一次、每名成员只入箱一次；认领者投递后没来得及标记即崩溃时，租约到期由另一副本重投同一份原字节（至少一次，消费方按 battle_id 去重）。
  - Go guild：`logic/trial_launch_recovery_test.go` 的 `TestTrialLaunchRecoveryReleasesStartedBattleOnlyOnSQLAbort`（超时、formation=4、COMPLETE、矛盾回执四轮都不释放，ABORT 一轮恰好释放一次，之后不再重放）；`data/trial_launch_abort_tidb_test.go` 的纯表 `TestTrialCancelReceiptIsTerminalOnlyForAuthoritativeEnds`，以及真 TiDB（`GUILD_ROLE_TEST_DSN`）`TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort`（帮会在 / 已解散两种：非终态回执不改任何行；ABORT 回执同事务释放 launch、两名参与者与大厅占位，并存下证明；重放不变）。
- **冒烟（停机补提交，可选，本机全球栈）**：
  1. 起 match（全球）、battle、scene、player_inbox，开一局 1v1；打完之前停掉 match，让 battle 的 `CommitBattleOutcome` 进入重试；
  2. 重启 match，在 battle 下一次 5s 重试之前给 battle 发 SIGTERM（或删 GameServer）。期望日志依次有 `event=battle_outcome_shutdown_flush pending=1`、COMPLETE 回复后的 `Battle ended`、`Before-shutdown drain complete.`；TiDB `match_battle_completion` 该局 decision=1，两名成员的个人来源都在、随后 dispatch_state=2；
  3. 重来一局，第 2 步不重启 match：期望约 15s 后 `Before-shutdown drain budget exceeded` 与一行 `metric=battle_sql_outcome_unflushed_at_shutdown battle_id=…`；重启 match、过权威截止后，该局 decision=2、formation=5、两人原票 state=4，个人来源为 0。
- **残余**：
  - battle 仍接受「带 placement_operation 但 outcome_protocol 0」的 Create，旧协议确认模式代码还在，成了 match 侧不可达的死代码。建议在 `HandleCreateBattle` 拒绝这类请求，并删掉旧确认模式落库代码。本批没做：`cpp/nodes/battle/tests/battle_observer_contract_test.cpp` 的夹具正好用这种请求建全球房间，而 v1 房间不能被 `DestroyBattle` 清掉，改夹具要先给测试补 v1 清房途径；该文件同时正由观战会话修改。
  - 截止竞态（§13.3 已登记，归并行 v1 轨道）：打满时限的强制平局、截止前后才到的 COMPLETE，会与到期 ABORT 抢同一主键；帮会恢复器每 10s 一次的取消在截止一到也能提交 ABORT。不会重复发放、不会不对称，但合法终局可能被作废。
  - 停机时还在打的局整局作废：玩家冻结到截止才解（scene reaper），原票与历练占用到截止后的下一轮恢复才释放。battle 若能在停机时签一份「本实例不会再提交这一局」的放弃证明，match 可以提前 ABORT；那要新 RPC 与签名，超出本批。
  - 15s 排空预算内 match 不可达时，补提交的局仍按上条作废，逐局有 `battle_sql_outcome_unflushed_at_shutdown` 留痕。
  - 关闭全球匹配的 dev / 本地形态下，guild 取消要求 match SQL 权威，缺则拒绝，停机作废的历练局占用没有出口；上线配置强制开全球匹配（审计 §3.7 r2-match#1），dev 数据可清空。
- **Java 版**：待做。Java 全球结果仍是确认模式（`BattleRoomServiceImpl` 回执后落 pending、`abortAll` 不派发，见上文 2026-10-19 段），v1 终局、停机补提交与帮会释放规则都要对齐，范围见下文并行轨道一节。

### 2026-10-21：常驻账号与显式迁移分离（源码候选，待运行）

旧`authority.Open`每次启动都调用`schemamigrate.Up`；即使所有业务表已迁移，迁移器仍执行`CREATE TABLE IF NOT EXISTS schema_migrations`，因此无DDL权限的合法常驻账号会在接流前失败。正常启动现在仅用同一正式proto清单调用`Plan`：待执行语句、人工项、dirty或读取失败一律拒启，候选索引检查仍保留；不会用启动实例补表、改列或修台账。

建库仍是现有部署前置。之后单独执行`match -f <同库全球配置> -migrate`，只连接配置中的`mmorpg_match`及原TiDB强读池，不构造ServiceContext，不访问etcd、Redis、Kafka，不申请号源、不监听RPC。该CLI必须从`MATCH_MIGRATION_USER`与`MATCH_MIGRATION_PASSWORD`取得非空独立凭据，不回落到常驻账号；`-allow-modify`仅与`-migrate`同时有效，沿迁移器人工审阅语义，不自动授权历史类型漂移。退出码沿用0成功、1失败、3锁忙、4人工项。

K8s沿既有迁移门禁接`match-migrate`：ConfigMap先于Job，全球模式所有档位都等Job成功才部署常驻实例；失败、未知或超时保留Job证据并阻断，重跑不删除在途Job。Job只引用`domain-service-secrets`中的`match-migration-user/password`，不注入探测、榜单或登录密钥，常驻Deployment不引用迁移凭据。显式旧dev兼容关闭SQL时不运行此Job。本机启动器不后台迁移；先以同一配置显式运行上述CLI，再清掉仅用于该子命令的迁移环境变量，最后运行`start-exe`。

接口回归覆盖缺表／人工项／dirty拒启与CLI隔离及退出码；新增真实TiDB有限DML账号用例核对Plan、索引可读，CREATE/ALTER真实权限拒绝，管理员删除表后常驻检查拒启且不补建。该真库用例及当前Match重新构建、单测、vet与Runtime EXE均待Codex实际执行，原598项普通回归仅保留原输入范围，不能作为本次新增启动接缝证据。

### 2026-10-23：已开局后丢失终局的恢复接缝（已授权实施，尚未验收）

当前缺陷不是取消 RPC 没有被调用：两版 Guild 都会重放原 Start 的取消，但 formation=4 只表示开房成功，不表示结果已持久化。Battle 崩溃后原内存创建账本也消失，不能据旧实例不可达、Redis 空键或 EXPIRED 推断无奖励。此前原请求墓碑只解决开局前的取消，未解决这一阶段。

目标模块由 Match 持久化层拥有唯一终局决定，调用者为固定原 operation/instance 的 Battle 与重放原 Start 的 Guild。接口不变量是：同一局 COMPLETE 与 ABORT 至多一个提交；COMPLETE 包括全体不可变个人结算来源及其重投资格；ABORT 之后旧写者不能再产生合法奖励；个人效果与永久消费水位同一玩家存盘提交，不以消息投递成功代替到账。

用户先授权本项最多 70 个手写文件，逐路径补齐必要入口后授权调整至 75，随后授权第 76 个旧 Java 测试文件，仅将非法 protobuf 夹具更新为合法且不同的旧版建房请求、保留原断言，继续完成两版实现与验证；正规生成产物不计入该上限。沿现有模块接线，不实现第二套资产账本：

1. 新全球成局显式固定终局协议版本、原 operation、目标 instance 与权威截止。发出任何新版 Create 前先 Query 同一目标 UUID 的能力，并将该能力证据和版本 1 的完整 Create 正文持久化；旧节点、缺少回显或能力未知时不发 Create。Create／PRESENT 回显的实际采用版本另存，不能用“支持版本”伪造“已采用版本”。因此 Create 回包丢失且 Battle 随后崩溃时，仍有发起前的持久围栏证据；历史无证据的旧局不得追认。Battle 在任何个人 Redis staging、派发或终局公开之前，提交已经冻结的完整原事件。Match 在原角色首锁与原 formation 锁下核名单、配置、活动原件、目标身份和摘要，逐人成员载荷均不超过 Inbox 的 16 KiB 上限后，原子保存 COMPLETE、全员原结算及待投工作。待投工作同时包括原全局结果的 Kafka 重投和个人 Inbox 重投，防止 COMPLETE 后、首次 Kafka 发送前崩溃而漏掉评级／帮会账本。未知提交重放同一字节，不能重算结果，也不能将 RPC 来源冒成 Kafka offset。原 Kafka 来源归档保留其真实来源语义。
2. Guild 的原请求取消可在新协议的权威截止后，与 COMPLETE 竞争同一持久行；只有确定没有 COMPLETE 且旧协议写者已排空的原件才可提交 ABORT/formation=5。截止只是请求持久围栏决定的条件，不是“超时就当没奖励”。已 COMPLETE 返回原结果并恢复结果投递；旧协议或缺原件继续 UNKNOWN，不能用新规则追认旧局已安全中止。
3. 个人来源复用现 PlayerInbox：新增 Match 生产方、专用 Battle 域与类型，载荷复用 `BattleSettlementData`，附原账号／归属修订。固定 `(player, producer, battle_id)`、原 issuedAt、完整载荷与摘要；Inbox 在同 P 锁内复核原身份，永久唯一收据的重放不得分配新 seq。Match 持久待入箱行不能因离线或队首页失败而停止游标；未确认的跨服务入箱只重放同一原件。可信持久来源不能因超过 GM 的七天签发窗而改 issuedAt 或丢弃。
4. Scene 新执行器复用结算效果入口，受 Inbox 永久域高水位去重。专用域固定 `requires_out_of_battle=false`、`max_pending=1`，允许原 battle 的结算在其战斗冻结下执行；跨区交接、退出和归属围栏仍保留。只有 APPLIED 可以终结该类型及扣减待处理数，通用 REJECTED／VOID／UNKNOWN 不能清掉奖励。缺持久组件、无法铸号或背包条件暂不满足保持待执行；不能按 Redis 锁消失直接丢弃，也不能让旧 HP/消耗落到下一局。Match 的 ready 票在本人的 Inbox 实际持久 APPLIED 收据前保持占用；仅归档来源、入箱成功、Kafka ACK 或 SETTLED_UNKNOWN 都不能作为已到账证明。Scene 收到该次 APPLIED 的确认前不能推进挤出本次回执，避免最近回执环的出环与未知状态形成永久等待。新协议关闭 Battle 原个人直投，避免两条入口分别记账造成重复。终局通知与摘战斗冻结也须跟随同一次可恢复流程。
5. 复用 Inbox 前必须补主 C++ 的 SQL 确认：当前 `AllDecisionsDurable` 看 `PlayerLastPersistedSnapshotComp`，而该快照仅证明 Redis 写成功；紧跟 PING 不能证明两张玩家 SQL 行提交。业务快照为 dirty 比较清掉 `last_save_seq`，子表 fence 字段也不是 C++ 的保存版本来源，不能从当前 owner 或可变下限拼原版本。新确认须由原次不可变存盘成功 hook 绑定精确 owner/seq、原实体和同一快照，再核两张 SQL ACK；旧回调不得给新实体或替换快照补证明。owner 变更但业务字节相同时也须能触发真实新版本保存，不能因 dirty 快路径永久停在旧证明。Java 的 `StoragePlayerRepository → PlayerStore.saveStateHeld → SceneWorld.SAVED → Inbox.onPersisted` 已以 SQL 事务成功为前提，保留该实现。
6. 不另造永久 battle-id 集合：现 BattleLedger 在销账后可裁剪，任意 Redis 丢失后不能仅凭它启用 SQL 重投；Inbox 已有不会裁剪的域水位和永久 SQL 原收据。新 Battle 类型应保护已投递奖励不被通用运维 VOID 或超时清理绕过；未知／损坏状态保留原件并告警。

当前已实现的金币、消耗、掉落等效果必须在副作用前整笔规划；主包和临时格都不能接收时保留待执行，不能沿旧路径记一条丢失日志就登记 APPLIED。经验值组件／升级曲线尚未实现，是现有 `player-attribute-allocation.md` 与 `turn-based-battle-server.md` 已记录的产品边界，本项不另造经验系统；完整来源保留 `exp_gain`，但不得将该字段的日志留痕报告为经验已到账。

涉及的接线范围为主仓 proto、Go Match/Inbox、C++ Battle/Scene、独立 Java Match/Battle/Inbox/Scene 及对应配置、迁移、两版回归，按用户已授权的 76 个手写路径上限统一核对。Go 负责唯一正规协议源和生成；各实现保留修改前原件，基于当前最新代码接线，不修改客户端 CurrentMaps。C++ 已补单次存盘的 SQL 确认，其余新链路继续收口；已有普通构建证据单独保留，不能覆盖本项新源。

逐路径实施复核补出必要装配与接口后，当前计划为 Go 26、C++ 18、独立 Java 29、共享既有文档 3，共 76 个唯一手写路径。必要入口包括实际开房编排、背包候选访问边界、Match 请求日志脱敏的部署模板、Battle 节点订阅全局 Match，以及 Java 重投任务的装配／关闭和带原 Create 身份的结果提交端口。第 76 路径是 `GlobalMatchStoreIntegrationTest.java` 的旧夹具合同适配。用户已明确授权，额外入口继续实施；不得为凑文件数删除回归、隐藏依赖或复制第二份业务规则。

通过标准必须包含：COMPLETE/ABORT 并发仅一终局、提交回包丢失按原字节消歧、COMPLETE 后尚未入箱即崩溃、部分入箱后崩溃、离线或故障成员不挡后续成员、Redis 全丢、玩家效果存盘前后崩溃、最近回执环出环后的旧来源重投、旧账号／旧 Scene／旧 Battle 拒绝、到账后才释放 ready 票，以及两版真实双区链。两版源码现已接线，既有普通构建结果不覆盖这些新源，也没有据此声明已恢复或恰好一次。

停机后的迟到回包须核实际回调，不仅是代理尝试释放回复。主 C++ 保留 weak 生命周期及停止闸，在原排空窗口内用专用探针持有尚未上传完整的合法入站请求，等待停止闸安装后再释放已验证的真实 COMPLETE 回复；只有实际成功回复进入停止分支且没有业务副作用，才覆盖该时序。传输取消、对象已销毁分别记录其真实结果，不冒认成功回调；不延长生产排空时间，也不为验收改传输语义。

实际采用版本还须带独立的“已经观察到回执”标记：没有收到原 Create／PRESENT 回执，与明确收到同原 UUID 的 adopted=0，不能都用零值表示。前者仍可依赖发 Create 前持久的原 UUID、能力证据和固定版本请求，在截止后竞争安全 ABORT；后者是冲突证据，必须先保存真实观察，再拒绝新协议的 COMPLETE／ABORT。首个实际观察不能被后续不同版本回显覆盖，更不能以支持版本 1 替换实际采用版本。该区分复用已计入范围的两版原持久协议文件；本项主仓 no-unity 正规生成和 Java 官方合同同步／检查已实际执行，运行回归仍待完成，不能用生成结果替代。

取消回包同样区分开房前与开房后：当前全球路径均先持久完整 Create 意图再发请求；从未产生该意图且已持久中止的 formation=5，可保留旧开房前安全取消合同。已经有 Create 原件时，状态 5 自身不能证明无奖励，必须有唯一 SQL ABORT；缺证据不签安全成功，不修改 Guild 调用方的原安全断言。具备完整 v1 原件、但原运行时销毁流程已将 formation 置为 5 的局，仍可在权威截止后与 COMPLETE 竞争同一 SQL 终局，防止恢复永久卡在 UNKNOWN。

部署前置沿既有用户预置 `domain-service-secrets` 合同：Match 与 Battle 各自部署命名空间提供 `match-outcome-battle`，Match 与 Inbox 所在命名空间提供 `player-inbox-match`。同一调用两端须同值，两种密钥互相独立；引用保持 mandatory，缺键拒启。不另造密钥管理器，本任务不写远端 Secret，也尚未验证实际部署。


### 2026-10-24：旧 Scene 维护后的显式解除（设计候选，未实现／未验收）

> **2026-10-30:本节作废。** `save_barrier_restore` 的 guard / plan / apply 和旧 `admission_recover` 都已删除,崩溃实例由 `player_store` 写权行判定([player-save-direct-commit.md](./player-save-direct-commit.md) §9 第 11 条),不需要维护解除流程。

> 下述初版严格验收方案作为历史保留。它把本地调试 marker、Codex 构建和 Windows 进程原件直接接进了维护入口；这些限制不构成生产运维授权。2026-10-24 负责人已批准本节后面的正式维护合同取代这部分生产依赖。历史实测与原失败原件不重写；新合同尚未完成实现和运行验收。

旧 Scene 恢复分为两个独立结果：A 只证明原写者已停止、真实队列排空、永久撤资格与 SQL 恢复／退休收据成立，维护仍关闭；B 才是受控解除后由新 SM 铸造 owner、新 Scene 以新入场世代重新接流。Ctmp 的连接边界、队列观察和故障编排是验收工具，不是生产业务入口，源码或工具编译成功不等于 A／B 已通过。

`save_barrier_restore` 原 `guard / plan / apply` 继续均不自动放流。拟在同一维护工具增加独立的 `release-plan / release-apply`：前者只读生成新计划；后者必须显式提供完整原文件 SHA、原维护 nonce、本地 SQL／Redis 永久实例 marker 和当次排他维护窗。新能力不改 DataService Cancel 的领域真相，不新增 RPC、迁移或另一份 proto 模型。计划直接引用原生成的 promotion／admission 原件；每个成员和物理目标自行携带身份，不用数组下标表达业务对应关系。

解除前的事实分别由真实接口核验：promotion 必须是原 SQL 永久 `BARRIER_CONFIRMED`；旧 holder 退休查原 admission 恢复收据的完整正文／摘要及当前全列 SQL；交易退出由正式 `TradeClient.CancelAccountRoleTrade` 产生原 listing、角色、卖方及归属修订的 `CANCELLED` 回包，保留当前编译来源、原进程／端点身份、原生命令和完整回包原件，随后只读复核数据库仍等于该领域动作后的原件。DB 解除模块不替代 Cancel、不直接更新 P 或交易状态。停写查原句柄退出、全部实际 SM 创建目录与代理覆盖的等集、代理 join 和精确 Redis CLIENT ID 关闭原件；队列查实际 Kafka topics／groups 的完整已审世代集合、committed 与 end，以及 Redis retry／processing／dead／64 桶 pending 和 outbox 的真实只读观察。只有 `passed`、`writersStopped` 或 `queuesDrained` 布尔值而没有这些严格引用和可机核原件，一律不算证明。缺生产者、未知成员或未知待办继续拒绝。

不可变解除计划与每次现场证明分开：原 intent、维护 nonce、成员全集、原永久收据、SQL／Redis 实例 nonce、原 guard 全集与正文 SHA 固定；每次新显式调用另取得绑定该计划的短时排他窗和运行观察。窗口过期、未知连接／写者、原件变化就停止，不能自动续窗或换 nonce。SQL 事务只做原永久收据、全列业务摘要和冻结路由的复核，结束并释放锁后才访问 Redis；不持 SQL 锁调用 Redis、Kafka 或 Scene。

提交点按物理 Redis 实例划分。固定 core 先、mapping 后；同 addr／DB 去重走同一路径。每实例的一条 Lua 在首写前同时核全部键的 TYPE、无 TTL 的原 marker／实例 nonce、guard 精确永久值以及本次永久解除收据；随后在同一命令内保存该实例的解除收据并解除它的全部固定 guard。收据键以 zone 和原维护 nonce 的摘要命名，不覆盖旧维护收据。当前 core／mapping 同 `26379 DB0`，因此两物理 guard 和一份解除收据是同一个原子提交，不能拆成两次 DEL。owner、永久 R、玩家缓存、pending、outbox、业务列和原 SQL 恢复收据都不在解除写集合内。

原计划重入允许两种且仅两种目标状态：尚未提交时，解除收据不存在且该实例全部原 guard 精确存在；已经提交时，永久解除收据完整等于原 intent／正文／实例 nonce，且该实例全部原 guard 已缺失。无收据但 guard 缺失、收据与部分 guard 并存、不同正文／新维护 nonce、错误类型或 TTL 都拒绝。split 部署若只完成前缀，后续同原件显式重入可识别已完成目标，不再要求它的旧 guard 仍在；未完成目标仍要求全部旧 guard。批次完成前 SQL 必须始终 `writable=0`，新入口保持关闭且旧写者不可复活；失败或回包未知保留已有前缀，不自动回滚或继续。全部解除完成后仍只报告解除结果，不据此宣布已接流。后续须另按既有 `route_tool audit -frozen-recovery` 与 `unfreeze -confirm-audited` 合同取得真实结果，再由新 SM／Scene 重登入场。已经进入后续正常运行后，旧解除计划不能再修改数据或授予新的放流资格；原永久收据仍可只读追溯。

必要回归包括：只读计划零写；缺领域成功原件、遗漏／重复／未知成员拒绝；坏类型／TTL／错实例／改 nonce 在首写前零副作用；同物理目标全 guard 原子解除；split 前缀故障仍闭门；实际 Redis 提交后客户端丢回复，以同原件和永久收据消歧；超时无后续副作用；默认旧模式不新增自动解除。B 还必须真实验证新 owner／新入场世代可重登、业务可用、旧 Scene／旧世代迟到写仍拒绝，未执行的场景不能由设计或验收工具代计通过。

#### 正式维护目标和操作审核合同（2026-10-24，已授权，待实现与验收）

`save_barrier_restore` 与 `admission_recover` 的权限来自有权运维者显式执行维护命令，以及只覆盖审定 SQL／Redis 范围的受限凭据。plan 默认只读；apply 必须提供原 plan、审核原件的完整 SHA 和显式维护确认。SHA 只约束文件完整性，不认证操作人，也不证明停写或排空。工具不登录远端控制面、不创建目标标记、不替运维停进程或撤凭据，不提供 `allow-any-remote` 或调试导入开关。

目标身份使用独立的 `maintenance-recovery` 命名空间。SQL 的每个实际全局库和行库由运维显式预置 `maintenance_recovery_target` 表：`purpose VARBINARY(16)`、`zone_id INT UNSIGNED`、`schema_name VARBINARY(64)`、`target_nonce VARBINARY(32)`、`physical_addr VARBINARY(255)`，主键为 `(purpose, zone_id, schema_name)`。`purpose` 仅允许 `promotion`、`admission`、`release`；nonce 为非全零的 32 位小写 hex，地址等于该次明确配置的规范 host:port。行没有 TTL，工具凭据不拥有此表的建表、插入、更新或删除权限。promotion／release 在涉及的全局库和全部行库内复核同一 SQL nonce；admission 在全局库及显式 storage 白名单对应的行库复核。每次 SQL 提交都用原事务的 `FOR UPDATE` 锁住这些标记行，直到业务提交或只读回滚结束。标记表示受控目标资格，不冒充数据库服务器 UUID；不能从名称、端口或调试标记推导资格。

Redis 在每个实际 addr／DB 上由运维预置 `maintenance_recovery:target:<purpose>:<zone>:<schema>:<role>` 及其 `:nonce` 两个永久 string。role 是 `core` 或 `mapping`；scope 原值为 `maintenance-recovery-v1|<purpose>|<role>|<zone>|<route_schema>|<schema>|<addr>|<db>`，各字段不含分隔符，nonce 固定为该物理目标的非全零 32 位小写 hex。promotion 的 schema 为原行库；release 的 schema 为原 route schema，所有成员行库仍逐一由 SQL 标记和领域快照核验。Lua 在首写前复核每个标记的 TYPE、PTTL=-1、scope 原值和 nonce。core／mapping 同物理目标时仍核两种角色的标记，并在一个 Lua 内同时提交全部原 guard 和一份永久解除收据；split 目标保持 core 先、mapping 后和未知前缀显式重入。nonce 不因 Redis 重启而换；丢失、TTL、错 role／zone／schema／addr／DB、换 nonce 或只存在 `debug_import` 标记都拒绝。`debugutil`、`debug_import_local_target` 和 `debug_import:*` 的源码、loopback 规则及授权值完全不变；专用维护标记不能让调试导入取得写权。运维预置表属于操作前置，不纳入自动服务迁移，本轮不新增 proto 或生成物。历史 `LocalTarget` 字段／`-local-target-id` 参数现只表示这个受审维护 nonce，不再含 loopback 语义；`-sql-target-address` 必须逐字匹配实际 DSN 与 SQL 标记，原计划一并固定该无凭据地址，不改任何领域 proto 字段。

解除审核改为与操作系统和构建工具无关的 `maintenance-release-operator-review-v1`。它固定原成员输入、逻辑发行方全集、每个发行方的原 instance／撤销范围、Kafka 全部历史 topic／group／partition 清单、Redis addr／DB／zone／nonce 清单，以及原领域 Cancel 和资产基线。发行方停止事实按稳定逻辑身份逐项记录停止时刻、停止／撤销原件和禁止再次发行的维护范围；每次短窗必须覆盖该原全集，不能只报数量、`writersStopped` 或 `passed`。本次队列事实必须落在不超过 60 秒的新窗内：逐分区满足 `earliest <= committed == end`，实际 topic、group、订阅和分区集合等于审核全集；Redis 必须覆盖每个原 zone 的 64 个 pending 桶、各历史 topic 的 retry／processing／dead／instance 元数据及所有实际 barrier／outbox，按原键和实际条目逐项核空或核正式已收敛版本，未知世代、键、成员和待投均拒绝。原始行、计数、类型、版本及内容摘要保留，COUNT 不替代等集。

便携原件字段固定为以下关系，生产不接受旧私有 schema：review 的 `operation_id/target_nonce/physical_sql_addr/route_schema` 连到 `issuer_inventory` 和原成员输入；目录每项是 `issuer_id/role/instance_id`。每个退休原件含 `publication_closed_ms <= stopped_ms <= window.issued_ms`、同一 `revival_fence_id` 和非空原始 `source_evidence`。window 必须逐项引用原退休文件，实际观察时刻处于本窗，`revival_blocked_until_ms >= expires_ms`；漏项、重复或同数量换身份拒绝。`maintenance-release-attempt-v1` 只关联原 review／plan、新窗与 Redis／Kafka 两类事实；不会给原计划延期。Redis 事实使用 `maintenance-release-redis-queues-v1`，显式保留 `key_names` 和每键原 `key/type/pttl_ms/count/members/values`，并核 key 摘要与观察摘要；缺键须是 `none/-2/0`，存在的维护队列及屏障须永久，barrier 原 pending 与双 ACK 必须是同一规范版本且无任意 outbox 字段。Kafka 事实使用 `maintenance-release-kafka-queues-v1`，保全逐订阅分区的原 earliest／committed／end／lag。两类事实都固定本窗内的采样开始／结束时刻和实际采集源原件。生产仅机械验证这些受限事实；采集者的授权、完整发现范围及禁发／不可复活的真实外部执行仍由受审运维流程承担。
停写与不可复活是运维控制面的显式责任，凭据受限的维护进程不具备跨主机进程真相的自动证明能力。生产读取器机器核验上述稳定事实的身份、全集、时序、数值、原件完整性及其与原计划的关系；不会把文件里的布尔或摘要解释成权限。采集者须用实际编排／凭据撤销／进程与连接证据建立这些事实，并在整个维护窗禁止新发行。私有本地验收先按原 source／PE／原句柄／完整创建目录／实际队列原件作严格校验，再转换为这些稳定操作事实；原 Windows FILETIME、Codex proof schema、PE 和 native argv 解析留在验收侧，不由生产工具依赖。缺真实源证据时验收转换器不得产出可执行审核原件，不能以便携格式掩盖尚未验收。

领域裁决保持原实现：只能在正式同身份 Cancel 得到 `CANCEL_PENDING` 后恢复旧 holder，常规 `AckTradeSeal` 的 FREEZING 规则不变；恢复后 Confirm 必须明确拒绝，再重放同一 unsigned Cancel 正文得到 SQL／回包 `CANCELLED`。解除仍核完整生成的 promotion／admission 永久 PB、原域摘要、原 holder／Seal、双 SQL 全列与 NULL、原 owner/save_seq、路由冻结 generation／rev／watermark／placement，以及整个 zone 的原成员等集（上限 1000）。SQL 事务内不访问 Redis、Kafka 或 Scene。原 intent 与原 plan 固定，每次新窗独立，单调截止不自动续期；两存储前缀、提交回复丢失与同原件重放规则均保持。解除成功仍不自动 unfreeze 或重启，必须再执行正式 route audit／unfreeze，并实际证明新世代重登及保存可用、旧世代真实迟到写仍拒绝。

新回归须包含非 loopback 的精确专用目标、仅调试 marker 拒绝、错物理 scope／nonce／TTL 在首副作用前拒绝、Linux 无 Codex／PE／FILETIME 输入、缺发行方／重复或同 COUNT 换成员、完整队列漏世代、所有 SQL／Lua 并发与未知提交前缀。现有本地旧 marker 和私有审核原件仅保留历史证据，不能称为新合同已通过；本条目前没有新的编译、单测、真库或 A／B 验收结果。

#### 2026-10-30 修订：promotion 固定前复核、同维护单份原件与解除的 owner 计数器判据（未编译，待 Codex 验证）

- **promotion**：首次写 PLAN_FIXED 之前，在 SQL 锁外完成三项只读复核——缓存仍与双 SQL 同内容、映射、Lua `check`；任一项不成立都不留收据，避免撤销失败后的孤儿原件永久阻断显式解除。计划时在、执行时已按 TTL 过期的缓存不再拒绝；重新出现且 bytes 不同仍拒。同一维护号内同一玩家只允许一份 promotion 原件：PreparePromotion 提前拒绝，固定原件时在维护标记行锁内再核一次。续跑以 SQL 里的原 bytes 为准，核原摘要后按语义比较，不要求重新编码后逐字节一致。
- **显式解除**：成员新增 `redis_owner_epoch`，即审定时观测到的 owner 计数器原值，执行时逐字节钉死。非恢复成员要求它不低于双 SQL 围栏的 `owner_epoch`；恢复成员要求它等于 promotion 的 TargetOwner；屏障 ACK 仍须等于双 SQL 围栏版本。原「计数器等于 SQL 围栏」在真实 zone 必然失败，原因有两个：玩家进场后未存盘就退出时，计数器本就高于 SQL 围栏；scene_manager 铸代写的是 `max(观测+1, 墙钟毫秒)`。格式同批升版：计划升为 `player-save-explicit-maintenance-release-v3`，成员输入升为 `maintenance-release-member-input-v2`，Redis 解除收据前缀改为 `release-v3`。v2 计划与 v1 输入一律拒绝，验收工具须同步。
- **旧 holder 恢复**：跨 zone 传送后，旧 holder 可能在别区 Scene 上。恢复事务对 Scene zone 和收据 home zone 两个 zone 都持锁核验 admission 维护标记，并删除原「home zone 必须等于 seal.zone_id」判据。确定拒绝返回固定 ASCII 原因码（`RecoveryRejectError`，`errors.Is(ErrConflict)` 仍成立），CLI 只打印原因码。plan 遇到已提交的原请求时，返回原件与 `ErrAdmissionRecoveryAlreadyCommitted`，提示「已提交、apply 只读重放」。Java 版 PlayerTradeStore／RecoverTradeAdmission 同批对齐。
- **业务全列摘要**：协议收敛到 `go/shared/businessdigest`，go/db 的计划／解除与 data_service 的证明共用，并由 golden 测试固定字节。
- **仍待负责人拍板**：
  - 孤儿 PLAN_FIXED 收据的正式放弃阶段；
  - 全 zone 等集的 1000 成员上限；
  - 非恢复成员「无挂单、admission 全关」的判据是否收窄。崩溃 Scene 上的非交易在线玩家，现有工具都关不掉其 admission，一次崩溃就会阻断解除。

### 2026-10-24：终局提交宽限与永久不适用子效果的恢复边界

本条为本轮已授权既有路径内的评审修复合同。Battle房间的原deadline不变，Match在deadline到达后继续禁止续领开房工作租约；SQL ABORT候选与持锁提交点统一改为经过60秒终局提交宽限后才能竞争终局。宽限内接受符合原Create、成员、身份与正文的COMPLETE，COMPLETE／ABORT仍由原主键和锁序决定唯一永久结果，不因临时RPC失败另造结果。两版目前使用单一具名60秒常量，未新增配置文件路径；这是有限恢复窗口，不保证任意长网络分区后仍能采用迟到结果。边界比较须避免deadline加法溢出，直接取消入口同样不能绕过该宽限。

个人结算仍以原永久来源及整笔候选为提交单位。已转出、不再归本角色的宠物，以及按既有PetService语义永久不存在的宠物配置，只跳过不再适用的该宠物子效果；其余金币、掉落和任务效果继续按原整笔校验与存盘。缺必要持久组件、损坏或unknown字段、重复实体和未知schema不能当作正常缺宠物；背包容量、货币上限、暂缺ID号段及存盘在途仍应保留整笔待执行。

BattleItemEntry原载荷只有config/count，不得在恢复时杜撰原GUID。权威已记录的合法消耗不能因当前battleUsable热更再次判为永久等待；复用既有完整实际实例候选与GUID／原字节核验，按原消耗不足夹紧合同执行并明确记录不足。相关未知版本或损坏实例不能按普通数量消耗，不能为了放票默默丢失正常资产效果。两版按同一边界补回归，源码修改与实际运行证据分别记录。

Scene退出时的异步SQL确认必须继续核原实体和原入场身份。原实体已经销毁时迟到回调不得补发Ack；永久Inbox水位保留已应用事实，新世代落地后按原来源重放并取得本次真实SQL确认，再Ack且不重复做资产效果。同步测试必须说明同步条件，不能把它当作生产退出前必定补发成功的证明。

上述只是当前实现与验收合同，尚未据此声明新增源码已编译或真实故障通过。CurrentMaps、客户端与其他既有工作不在写集合内。
