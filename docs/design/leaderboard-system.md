# 排行榜(leaderboard / rank)v1 设计

> **本文只是设计,未落码、未编译。** 本文不改任何 proto / yaml / 配表 / 代码;文中出现的文件改动全部是"将来那一批要改什么"的清单。
> 日期:2026-09-20(同日经三视角评审回修一轮,见 §10)。前情:[`friend-handoff-20260920.md`](friend-handoff-20260920.md) §4.2(leaderboard 一节)、§4.4(A 仓服务对照表中 `runtime/leaderboard` 一行)。
> 约束来源:[`microservice-zone-contract-20260914.md`](microservice-zone-contract-20260914.md)(下称"契约")§3 / §4 / §6 / §7 / §8;[`xuanming-port-decisions-20260910.md`](xuanming-port-decisions-20260910.md) D-9 / D-11 / D-12 / D-13 / D-14;仓根 `AGENTS.md` §4 / §7 / §11。
> **读者**:零上下文的接手人。**先读 §2**,它是全文唯一的硬阻塞;§2 与 §9.1 没拍板,后面各节都不能开工。§9.1 的七项已于 2026-09-20 全部由用户拍板(结果见 §9.1 与 §6),正文里原"待拍板"标记已改为"已拍板"。

---

## 目录

1. 背景与范围
2. ⚠ 硬阻塞:写分来源这条东西向边不存在
3. 数据模型与键设计(含 §3.7 告警)
4. 协议与客户端读
5. 部署与登记清单
6. 决策记录 L-1…L-21
7. 分批建议(R0 / R1p / R1 / R2a-读 / R2a-写 / R2b / R3)
8. 验证清单
9. 未核实项 / 拍板记录
10. 评审记录

命名约定:**"leaderboard"是功能名,"rank"是工程名**。进程、目录、proto 包、端口登记一律用 `rank`(理由见 L-3:节点枚举已经叫 `RankNodeService = 13`,生成器靠目录名推节点类型)。

---

## 1. 背景与范围

### 1.1 为什么要做

- A 仓的 `runtime/leaderboard` 在 B 仓没有对应物(`friend-handoff §4.4` 表中标"缺 / 待做")。B 仓现有的唯一的榜是**公会榜**,在 `go/guild/internal/data/guild_repo.go`,只服务 GUILD 这一个维度。
- 玩家侧天然存在一个可排的量:match 的 Elo 评分(`go/match/internal/logic/rating.go`,Redis hash `match:rating:{player_id}`)。它今天只在 match 内部用于匹配,**没有任何对外视图**,玩家看不到自己排第几。
- 基础设施已经半就绪:`proto/common/base/node.proto` 的 `RankNodeService = 13` 已存在;`proto/db/proto_option.proto` 的 `NODE_RANK = 18` 已存在;`data/tip/Tip.xlsx` 已给 `rank_error 21000` 留了预留注释。缺的是服务本体、协议、写分来源。

### 1.2 v1 做什么

| 项 | 内容 |
|---|---|
| 榜 | **一个**全服榜:match 评分榜(`RANK_BOARD_MATCH_RATING`)。不分模式、不分 zone |
| 存储 | Redis ZSET(派生视图,权威在 match),见 §3 |
| 写 | 消费 match **新增**的评分快照事件(§2 推荐方案 a),只有这一个写入口 |
| 读 | 客户端一个 RPC:拉一页 + 顺带"我的名次"(§4) |
| 进程 | 新建全局 Go 服务 `go/rank`,无状态,端口 `51100` / 指标 `:9250`(§5) |

### 1.3 v1 不做什么(以及解禁条件)

| 不做项 | 理由 | 解禁条件 |
|---|---|---|
| **发奖 / 赛季结算(A 仓 `SettleBoard`)** | 发奖要走资产通道的 `ASSET_OP_STREAM_SYSTEM_CREDIT` 流(`proto/common/asset/asset_op.proto:33`,注释"预留:邮件/GM 发物")。`cpp/tests/currency_test/asset_op_auth_test.cpp:362` 明写"SYSTEM_CREDIT 在 v1 没有合法调用方,任何签名都过不去";`docs/design/guild-phase2/04-asset-channel.md` I6 把这条流独占给"将来的邮件/GM 发物服务"。rank 自己去发物会成为这条流的第二个 seq 分配者,违反 I6 | SYSTEM_CREDIT 白名单开放(mail 服务落地后),且 rank 发奖改为"结算出名单 → 交给 mail 发"而不是自己发物 |
| **赛季 / 快照 / 历史榜** | 需要永久快照 id(要开号段 biz_tag)和持久化(要开库,触发 D-14 全套),v1 没有需求方 | 产品给出赛季规则;届时同批开 `mmorpg_rank` 库与 biz_tag |
| **按模式分榜(1V1 / 5V5 各一榜)** | match 的评分**不分模式**:`keys.go` 的 `matchRatingKey` 只有 `playerId` 一个参数,1V1 与 5V5 共用一个 rating。rank 侧无论怎么做都拆不出两份分 | match 先把评分按 mode 拆开(match 的领域变更,不是 rank 的) |
| **按 zone 分区榜** | match 入账路径上拿不到 zone(`BattleResultEvent` 无 zone 字段;匹配池全局,D1);归属 zone 要额外查 data_service 的 `BatchGetPlayerHomeZone`,且合服时 `RemapHomeZoneForMerge` 会改写归属,事件里写死的 zone 会过期(详见 §3.4) | 产品确认"本区榜"需求,并拍板合服时分区榜的迁移口径 |
| **GUILD 维度** | 公会榜已经在 `go/guild` 里完整实现了两层榜 + MySQL 权威 + 启动重建,搬过来只增加风险,且 GUILD 分值写入今天没有调用方(见 §3.1) | 不计划解禁。rank 的榜类型枚举**不预留** GUILD 值(L-15) |
| **等级 / 战力榜** | 写分方是 C++ scene,要么走 Kafka 要么直连 gRPC(契约 §8 的六处改动),v1 不背这个成本 | 有需求时另立一批,写分通道按 §2 的同一套判据选 |
| **全量对账 / 重建** | match 的 `match:rating:*` 放在可集群的 MatchRedis,没有索引集,只能逐分片 SCAN;而跨服务读对方存储违反 D-14 第 6 条。正确做法是 match 提供导出 gRPC,这是对 match 的第二处侵入 | 线上观测到榜漏人比例超出容忍(看 §3.6 的指标),或需要"冷启动一次到位"。7 天内的恢复已有零成本路径(§3.5 offset 重放) |
| **S2C 推送("你的名次变了")** | 契约 §5:推送 at-most-once,必须有全量拉取兜底;榜是拉模型天然满足,推送没有收益 | 不计划 |
| **客户端 UI** | 修改客户端仓(`../mmorpg-client/`)**没有授权**(`friend-handoff §4.3` 只授权只读),本文只给客户端规格(§4.6) | 用户授权客户端改仓 |

---

## 2. ⚠ 硬阻塞:写分来源这条东西向边不存在

### 2.1 事实

- 早期说法"leaderboard 消费 match 的 `PlayerRatingChangedEvent`"**是错的**:`RatingChanged|rating_changed|rating-changed` 在 `go/`、`proto/`、`cpp/` 全部零命中,只出现在 `friend-handoff-20260920.md` 的两行。`proto/contracts/kafka/` 下没有任何评分相关 message。**match 不对外发布任何评分变更。**
- match 的评分链(`go/match/internal/logic/rating.go`):
  - 输入:topic `match-results`(全局、无 zone 段、key=battle_id、payload 直接是 `BattleResultEvent` 不套信封),消费组 `match-rating`(`go/match/etc/match_service.yaml` 的 `ResultConsumerGroup`),3 分区,保留 7 天。
  - `ApplyBattleResult(svcCtx, *kafkapb.BattleResultEvent) (outcome string, err error)`,outcome ∈ applied / duplicate / ignored / partial / error;`match_service.go` 注入的回调**丢弃 outcome**,只返回 err。
  - 只有 1V1 / 5V5、outcome 为胜/负/平、恰好两支非空队伍才计分(`isRatedMode`、`splitResultTeams`);回合打满(`total_rounds >= RatingDrawRoundCapFor(battle_config_id)`,yaml `RatingDrawRoundCap: 30`)按平局。
  - Δ 是**队伍级**:`deltaA = 32 × (scoreA − eloExpected(avgA, avgB))`,`deltaB = −deltaA`;赛前分由 `loadRatings` 读取,读失败按 1500。`now := nowMs()` 在每次 `ApplyBattleResult` 调用时重新取(续写时比真实写分时间晚)。
  - 逐人写:Lua `ratingApplyScript`,KEYS=[`match:rating:{pid}`]:
    - `recent_battles`(窗口 8)已含本局 → 返回 `{0, HGET rating, HGET games or 0}`。**这里的 games 是 HGET 的 bulk string**,Go 侧拿到的是 `string`("5");不存在时是整数 0。
    - 否则 `rating = max(0, 当前 + Δ)` 按 `%.2f` 存、**无条件** `HSET updated_at_ms = ARGV[2]`(分数没变也写)、`HINCRBY games 1`、返回 `{1, 新rating, 新games}`,**这里的 games 是 HINCRBY 的整数**(Go 侧 `int64`)。
    - 两条分支都**不返回** `updated_at_ms`。**Go 侧 `applyRatingDelta` 只取 `arr[0].(int64)` 与 `arr[1].(string)`,丢掉了 games**。
  - 两层幂等:对局级 SETNX `match:rating:applied:{battle_id}` = `applying|Δ_A`(TTL 7 天),续写时用标记里的 Δ 不重算;玩家级靠 `recent_battles`。写到一半失败不回滚。
  - 消费者重试 3 次(间隔 1s)后**跳过并提交 offset**(注释"评分是软数据")—— partial 的对局剩余玩家永远补不上。
  - `match:rating:{pid}` **会丢**:`match_service.yaml` 注释写明 MatchRedis 回落共享库且库配 `allkeys-lfu` 时"评分可能被淘汰回落 1500"(`deploy/docker-compose.yml` 的共享 Redis 就是 `--maxmemory-policy allkeys-lfu`);`AGENTS.md §6.2` 要求每次登录压测前清空全部 Redis。键一丢,`games` 从 1 重新计数 —— **games 只在键存活期间单调**(§2.4 的 `rating_gen` 就是为此而加)。
- 包依赖:`svc` import `match/internal/kafka`;`kafka` 只 import `match/internal/metrics`;`logic` import `svc`。**`kafka` 包不能 import `svc` / `logic`(`svc → kafka → logic → svc` 成环)**,反过来 `logic` import `kafka` 不成环(`result_consumer.go` 注释)。
- 可测试性接缝的仓内先例:`go/match/internal/team/notify.go` 的包级变量 `sceneRefreshFn = writeSceneCommand`(测试在 `team/service_test.go` 里替换);`rating.go` 的 `ratingAfterLoadHook`(`rating_review_fix_test.go` 替换并 `t.Cleanup` 还原)。`svcCtx.Kafka` 是具体类型 `*kafka.Writer`(同步、`MaxAttempts=1`、RequireOne),不可在测试里拦截。
- 东西向合法通道只有两条(契约 §8):**Kafka(带幂等键)** 或 **直连 gRPC(专用 READY 选择器)**;路由服不是东西向通道(`go/client_rpc_router/internal/logic/forwardlogic.go` 缺会话元数据直接回 `Unauthenticated`)。

### 2.2 两个候选

- **(a) match 新增出站事件**:match 在入账成功后,对每个参赛玩家发一条**评分快照事件**到新 topic;rank 消费它写 ZSET。
- **(b) rank 自己消费 `match-results` 重算**:rank 用独立消费组读 `BattleResultEvent`,在 rank 里复刻 Elo 计算与入账状态。

(直连 gRPC 的第三种形态 "match 调 rank 的写分 RPC" 被排除:它会第一次触发 D-13 的待拍项 —— go-zero key 叫什么、`-Zone` 的 `.z<N>` 怎么豁免 —— 而且 match 自己的 `match_service.yaml` 仍注册 `Key: matchservice.rpc`、会被 `-Zone` 切分;不想走 go-zero 就得第三次复制 NodeInfo watcher。它相对 Kafka 没有任何收益,却把 rank 的可用性绑进 match 的评分循环。)

### 2.3 逐条对比

| 维度 | (a) match 新增出站事件 | (b) rank 消费 `match-results` 重算 |
|---|---|---|
| **改动面** | **match 侧**:`proto/contracts/kafka/match_event.proto`(加一个 message,见 §2.4);`go/match/internal/logic/rating.go`(`ratingApplyScript` 扩返回值 + `HSETNX rating_gen`;`applyRatingDelta` 改签名透出快照;`ApplyBattleResult` 在全员写完、置 done 之前经可替换接缝批量发布);新文件 `go/match/internal/kafka/rating_snapshot.go`(只做 proto 构造与 `*kafkago.Writer` 参数化写入,**不 import svc/logic**);`internal/metrics`;`internal/config` + `etc/match_service.yaml`(topic 名、分区数、保留期、发布开关);`match_service.go`(启动时对新 topic 调 `kafkautil.EnsureTopics`);`tools/scripts/k8s_deploy.ps1` 的 match ConfigMap case;测试。约 10–11 个手写文件(§7 R1)。**rank 侧**:一个普通消费者 + 一条 Lua | **match 侧零改动**。**rank 侧**:要复制 `eloK=32`、`defaultRating=1500`、0 分下限、`%.2f` 精度、`isRatedMode`、`splitResultTeams`、`RatingDrawRoundCap` / `RatingDrawRoundCapByConfigId`(这两项在 match 的 yaml 里,rank 要么抄一份配置要么跨服务读配置)、`applied` 标记状态机、`recent_battles` 窗口、`teamAverageRating`。rank 还必须自己持有每个玩家的赛前分 —— 等于 rank 维护一份完整的 `rating` 存储 |
| **幂等键** | `(player_id, rating_gen, games)`:games 由 `HINCRBY` 维护,**在同一代际内**严格单调;`rating_gen` 是评分 hash 首次写入时间(`HSETNX`),键丢失重建后代际变大。按 `(rating_gen, games)` 字典序比较:同一局重放 → no-op;不同局乱序 → 小者被拒;match 侧丢键 → 新代际覆盖。battle_id 作为诊断字段随行,不参与判定 | `battle_id` 对局级标记(照 match 的 SETNX)+ 玩家级窗口。rank 的标记 TTL 必须 ≥ `match-results` 保留期 7 天(契约 §6) |
| **失败与重放语义** | 发布在 Lua 之后:Redis 写成功、发布失败 → `ApplyBattleResult` 返回 error → 消费者按现有逻辑重试该对局 → 续写路径上所有 Lua 返回 `written=0` **但仍带回当前 rating / games / updated_at_ms / rating_gen**(§2.4 要求 Lua 扩返回值、Go 侧两种类型都能解析)→ 重新发布**当前快照**(不依赖 written=1)。重试用尽 → 该局的快照丢失,但**下一局会带着完整的最新快照覆盖**,自愈。**评分本身不受发布失败影响**(Lua 在前) | rank 自己消费失败 → 自己重试 / 跳过。但与 match **相互独立地**跳过:match 跳过某局而 rank 成功(或反之),两边从此**永久分叉**,没有任何机制能收敛,因为 Elo 是路径相关的 |
| **"两份真相"位置** | 只有一份:`match:rating:{pid}`。rank 的 ZSET 是它的**派生视图**,事件携带的是结果(rating/games)而不是规则 | 规则两份(`rating.go` 常量 vs rank 复刻);配置两份(`match_service.yaml` 的 `RatingDrawRoundCap*`);状态两份(match 的 `match:rating:*` vs rank 自己的评分存储);顺序两份:`match-results` 按 battle_id 分区,同一玩家的两局可能落在不同分区,match 实例与 rank 实例的处理顺序不同,Elo 结果就不同。`RatingEnabled=false` 时 match 停在 1500 而 rank 照算 |
| **与成环约束的关系** | 发布调用点只能在 `logic`(只有 `ApplyBattleResult` 同时拿得到 battle_id 与每人快照);构造器放 `kafka` 包、以参数接收 writer,**不 import svc**。`logic → kafka` 方向已被允许,不成环。照 `team/notify.go` 的 `sceneRefreshFn = writeSceneCommand` 写法,在 logic 里放包级接缝变量 `ratingSnapshotPublishFn`(§2.4),生产实现拿 `svcCtx.Kafka` 调构造器 | 与 match 的包结构无关;rank 自己的包按 friend 样板排 |
| **对 match 的侵入** | 中等:改 `ratingApplyScript` 的返回值 + 一个 `HSETNX` + 一个函数签名 + 一个发布点 + 配置;复用已有的 `svcCtx.Kafka` writer(`Hash` 回落按 Key 分区,不必另建)。**评分计算逻辑不动**(Δ、下限、精度、幂等判定一行不改) | 零侵入 —— 但代价是把 match 的内部规则变成 rank 的隐式依赖,match 以后改 K 值、改平局规则,rank **零报错地**算出不同的分 |
| **回滚方式** | match 加开关 `RatingSnapshotPublishEnabled`(建议 v1 默认 `false`,rank 上线后再翻 `true`;K8s 上由 `k8s_deploy.ps1` 显式参数控制,§5.4);回滚 = 翻回 `false` 重启 match,评分链不受影响,榜停止更新但仍可读。rank 整体下线不影响 match | rank 停掉消费者即可;match 不受影响。但回滚后再恢复时,rank 自己的评分存储已与 match 分叉,只能清空重算(且 7 天保留期外的对局永远算不回) |

### 2.4 选定:(a),附带设计要点 —— **已拍板(用户 2026-09-20 拍板)**

**推荐 (a)。** 理由按 `AGENTS.md §11` 的优先级排:

1. **正确性 / 数据一致性**:(b) 在规则、配置、状态、顺序四个维度同时产生第二份真相,其中"顺序"这一条**无解**(两个消费组对同一分区的相对进度不可控),分叉是必然而非偶然。(a) 只有一份真相,rank 只是它的投影。
2. **自愈性**:(a) 的事件是**快照**而不是增量,任何一条丢失都会被该玩家的下一条快照覆盖;(b) 丢一局就永远错。
3. **侵入的代价可控**:(a) 对 match 的改动是"加出口",不动计算;带开关,可以独立回滚。

(a) 的具体形状(落码时照此写,字段名可再议):

- **事件**:`PlayerRatingSnapshotEvent`(**刻意不叫** `PlayerRatingChangedEvent`:续写路径上 `written=0` 时也要发,那时分数并没有"变",发的是当前状态)。字段:
  - `player_id uint64`
  - `rating_centi uint32`:分×100 的**精确**整数。match 存的是 `%.2f` 字符串,**必须按十进制字符串解析**:按 `.` 拆成整数部分与小数部分(小数不足两位补 0),`整数×100 + 小数`;或者 `uint32(math.Round(f*100))`。**禁止** `uint32(f*100)` 截断 —— `"0.29"` 会得到 28、`"1516.29"` 会得到 151628,展示少 0.01 并与真差 0.01 的玩家变成同分,零报错。解析失败 → 返回 error(走重试),不发布。
  - `games uint64`:Lua 返回的第 3 个元素。**两条分支类型不同**(写入分支 `int64`、续写分支 bulk `string`),Go 侧必须同时接受 `int64` 与 `string`(`strconv.ParseUint`);解析失败**或结果为 0** → 返回 error,**不许回落成 0**(否则续写路径发出 games=0,已在榜的玩家被当 stale 拒掉、新玩家以 0 局入榜,§2.3 的唯一补偿手段静默失效)。落码时同时把续写分支改成 `tonumber(redis.call("HGET", KEYS[1], "games")) or 0`,让两条分支都返回整数;Go 侧双类型解析仍保留,作纵深防御。
  - `updated_at_ms uint64`:**必须与 rating / games 同源** —— 由 `ratingApplyScript` 在同一次 Lua 里 `HGET updated_at_ms` 原子读出(第 4 个返回元素),**禁止**用 Go 侧的 `now` 或 `finished_at_ms` 代替(续写时 `now` 是重试时刻;续写读到的 rating/games 可能含另一 match 实例后写的更晚一局,`finished_at_ms` 对不上)。
  - `rating_gen uint64`:评分 hash 的**代际**,由 `ratingApplyScript` 开头 `HSETNX KEYS[1] rating_gen ARGV[2]`(nowMs)建立,第 5 个返回元素 `HGET rating_gen`。键被淘汰 / 清空后首次写入会建立更大的代际,rank 据此覆盖旧条目而不是永久拒写(§3.2)。存量玩家(升级前已有 hash、无 `rating_gen`)在升级后的第一次写入时补上。
  - `battle_id uint64`(诊断)、`finished_at_ms uint64`(诊断 + `rank_consume_lag_seconds`)。
  - **不放 zone**(§3.4)、**不放 Δ**(0 分下限使实际变化可能小于 Δ;续写路径拿不到可靠的赛前分;快照语义不需要 Δ)、**不放 match_mode**(评分跨模式累计,§1.3;带上会让将来的消费者误以为是"该模式的分";诊断要对局信息用 `battle_id` 回查)。字段号上线后不复用(`AGENTS.md §4`),所以无消费方的字段一律不加。
  - 放 `proto/contracts/kafka/match_event.proto`,与 `BattleResultEvent` 同文件:生产方是 match,契约归属跟生产方走。注释写明 match 对外承诺:**同一 `rating_gen` 内 games 严格单调;`rating_gen` 只增不减**。
- **`ratingApplyScript` 的返回形态**(只改返回值与一个 HSETNX,不动计算):两条分支统一返回 `{written, rating, games, updated_at_ms, rating_gen}`。`applyRatingDelta` 的形态检查从 `len(arr) < 2` 改为 `< 5`。
- **topic**(**已拍板,2026-09-20,见 §9.1#5**):`match-rating-snapshot`(照 `match-results` 的 kebab-case、全局、无 zone 段、不套信封);**key = player_id**(`AGENTS.md §7` #3:同一玩家有序);推荐分区数 3、保留 7 天(与 `match-results` 对齐)。
  - **保留期只由生产方 match 声明**:match 调 `kafkautil.EnsureTopics` 时带显式 `RetentionMs`(7d,从 match 配置读);**rank 调 `EnsureTopics` 时传 `RetentionMs: -1`**(`topic_init.go` 的语义:用 broker 默认、不改写),只校验分区契约。理由:`EnsureTopics` 在 `RetentionMs > 0` 时每次启动都 `IncrementalAlterConfigs` 改写 `retention.ms`;两边各写一份数,保留期就会随重启顺序来回改(`deploy/k8s/README.md` "Kafka 保留期"一节记过 `db_task_zone_<N>` 的同一个坑),且零报错。
  - 两边都 Ensure 分区契约:match 的 writer `AllowAutoTopicCreation=true`,若没人 Ensure,broker 会按默认分区数自动建 topic,破坏契约。
  - 不适用 `target_instance_id`:`AGENTS.md §7` #2 只约束 `{type}-{id}` 形态的 topic;本 topic 无目标实例语义(同 `match-results`,见 `cpp/nodes/battle/logic/battle_room_manager.cpp` 注释)。
- **发布点**:`ApplyBattleResult` 在**全员 Lua 写完之后、`Setex(appliedKey, done)` 之前**,把本局所有人的快照**一次**发出。
  - **接缝(写死)**:`logic` 包加 `var ratingSnapshotPublishFn = publishRatingSnapshots`,签名 `func(ctx context.Context, svcCtx *svc.ServiceContext, events []*kafkapb.PlayerRatingSnapshotEvent) error`;生产实现调 `kafka` 包的构造器、再 `svcCtx.Kafka.WriteMessages` 一次批量写。测试照 `rating_review_fix_test.go` 替换 `ratingAfterLoadHook` 的写法替换它并 `t.Cleanup` 还原 —— R1 的发布类用例因此只用 miniredis + 替身,不连 Kafka。
  - 为什么批量:现有 writer 是同步、`MaxAttempts=1`、`WriteTimeout=5s`;5V5 逐人同步发最坏 10×5s,把 Kafka 延迟整个塞进评分循环。一次批量写最坏 5s。
  - 为什么在置 done 之前:发布失败 → 返回 error → 标记仍是 applying → 重试走续写路径 → 所有人 `written=0` 但带回当前快照 → 重新批量发布。置 done 之后再发就没有重试机会了。
  - 续写路径的 rating/games 可能已经包含后来的对局(同一玩家另一局由另一 match 实例入账),这**不是错**:快照就是"当前状态",updated_at_ms 与之同源(同一次 Lua 读出),games 更大,rank 照收。
  - 开关 `false` 时不调用接缝、行为与旧版逐字一致。
- **不做 outbox(v1)**:outbox 要 Lua 同时写一条待发记录;MatchRedis 可集群、`match:rating:{pid}` 按玩家分布,待发记录要么与评分 key 同 slot(每玩家一个待发队列 + 后台扫描,又要 SCAN),要么跨 slot(违反契约 §4)。收益只是把"重试 3 次后丢一条快照"降到零,而快照语义已经让这条丢失在下一局自愈。**代价**:长期不再打排位的玩家,若最后一局的快照丢了,榜上停在倒数第二局的分,直到下一局。
- **match 需要改的签名**:`applyRatingDelta` 现返回 `(written bool, rating string, err)`,改为返回 `(written bool, snap ratingSnapshot, err)`,`ratingSnapshot` 含按上面规则解析好的 `ratingCenti / games / updatedAtMs / ratingGen`。match 现有用 `rating string` 的日志 / 指标照旧从 snap 里取原串。

**已拍板(用户 2026-09-20 拍板)**:选 (a);三个附属决定一并按本文推荐 —— 发布开关默认 `false`、v1 不做 outbox、在 match 评分 hash 里新增 `rating_gen` 字段(§9.1#1)。

### 2.5 这条边上游仍然会丢的口子(两个方案都一样)

| 丢失点 | 位置 | 后果 | 处置 |
|---|---|---|---|
| battle 发 `match-results` 至多一次 | `battle_room_manager.cpp` `SendBattleResultEvent` 失败只 `LOG_ERROR` | 评分本身没入账,榜与评分**一致地**缺这一局 | 不归 rank 管 |
| match 消费者重试 3 次后跳过 | `result_consumer.go` `runResultConsumer` | partial 对局剩余玩家的评分没入账;(a) 下榜与评分仍一致 | 不归 rank 管;在 match 侧另立待办 |
| `RatingEnabled=false` | `match_service.go` | 不消费、评分恒 1500、不发快照;榜冻结 | 设计内行为 |
| match 评分键被淘汰 / 清空 | MatchRedis 回落共享库 + `allkeys-lfu`;压测前 `FLUSHALL` | 该玩家评分回到 1500、games 从 1 重计;有 `rating_gen` 时 rank 按新代际覆盖(§3.2),榜与评分重新一致;没有 `rating_gen` 时 rank 会**永久拒写**该玩家 | `rating_gen` + 运维规则(§3.5)+ `rank_write_total{outcome="gen_reset"}` 告警(§3.7) |

---

## 3. 数据模型与键设计

### 3.1 与公会榜的边界(为什么不能照抄 `guild_repo.go`)

| 公会榜做法(`go/guild/internal/data/guild_repo.go`) | rank 为什么不照抄 |
|---|---|
| 键 `guild_rank` / `guild_rank:zone:<id>` / `guild_rank:maintenance_lock` / `guild_rank:rebuild:<uuid>:*` | `RebuildRanks` 会 `SCAN guild_rank:zone:*` 再 DEL / RENAME。任何以 `guild_rank` 开头的键(例如 `guild_rank:zone:player`)都会在公会启动重建时被删。**rank 的所有键不得以 `guild_rank` 开头**;同理避开 `match:*`(match 已有 `match:{mq}:rank:*`) |
| MySQL `guild.score` 是权威,ZSET 是读加速 | rank v1 **没有**持久化副本,权威在 match(§3.5) |
| 每次写分拿全局 SETNX 锁(等 ≤5s,TTL 5min) | 玩家评分每局每人一次,全局锁会把写路径串行化;rank 单条写是一条 Lua,本身原子 |
| 覆盖写绝对值,无版本号 | Kafka 会乱序与重放,必须带单调版本(`rating_gen`, `games`) |
| 同分依赖 Redis member 字节序(十进制 id 字符串) | 既不是先到先得也不按数值,rank 显式定义同分规则(§3.3) |
| `RebuildRanks` 用 SCAN + 多键 RENAME | 契约 §4:私有 Redis 不用 SCAN、不做跨 slot 多键操作 |
| 分页越界返回**空页**(`rank_page_test.go` 的"超过末页""页码为零"断言空) | rank 越界**钳到末页**、page=0 当 1(§4.2),语义相反,测试不能照抄期望值(T5) |

顺带核实的事实:公会榜今天**没有写分来源** —— 除 `CreateGuild` 写 0 分(`guild_logic.go` 里 `UpdateGuildScore(ctx, guildID, zoneID, 0)`)外,全仓无手写调用方。这是 GUILD 不值得搬的另一个理由。

### 3.2 键

所有键用独立根前缀 `lb:`,并用 hash tag 把"同一榜的 ZSET 与版本表"钉在同一 slot(Lua 要同时读写两者,契约 §4:"跨 key 原子才收 hash tag"):

| 键 | 类型 | 内容 | TTL |
|---|---|---|---|
| `lb:{rating}:z` | ZSET | member = player_id 十进制串;score = 复合分(§3.3) | 无 |
| `lb:{rating}:v` | HASH | field = player_id;value = `"<rating_gen>:<games>"` | 无 |

- **键只由构造函数产生**:rank 的 data 包只有一个根前缀常量(`lb:`),所有键都经构造函数生成,禁止在别处拼字符串(T9 靠这一点做表驱动断言)。
- **为什么版本表单独放 HASH 而不是编码进 score**:score 已经用满 53 位(§3.3),放不下版本。
- **为什么 ZSET 与 HASH 同 slot**:写入必须"比较版本 → 更新 ZSET → 更新版本"原子完成,否则两个 rank 实例并发消费同一玩家(再均衡窗口)时会互相覆盖。代价:整张榜落在单个 slot,集群下无法横向切分。按"全服有排位记录的玩家数"估,单 ZSET 数十万到百万成员,单 slot 可承受;超出时再按分片拆(那时也需要重新设计分页)。
- **无 TTL**:榜是长期视图;没有赛季概念时不设过期(赛季是不做项)。
- **写入 Lua**(单条,KEYS=[`lb:{rating}:z`, `lb:{rating}:v`],ARGV=[pid, rating_gen, games, rating_centi, compositeScore]):
  1. `HGET v pid` → 解出 `(ogen, ogames)`;若存在且 `(rating_gen, games) <= (ogen, ogames)`(字典序)→ 返回 0(**stale**:幂等重放 / 乱序旧事件 / 旧代际残留)。
  2. 若存在且 `rating_gen > ogen` → 本次记为 **gen_reset**(match 侧评分键丢过,新代际直接覆盖,不管 games 大小)。
  3. **同分保留时间分量**:`ZSCORE z pid` 存在且 `floor(old / 2^32) == rating_centi` → **不改 ZSET**(保留"最初达到该分"的时间);否则 `ZADD z compositeScore pid`。这样 §3.3 的同分规则真正实现为"先达到该分者在前",而不是"最近一局更早者在前"(match 的 Lua 每局都无条件改 `updated_at_ms`,0 分下限钳住、Δ=0 的平局、|Δ|<0.005 都会出现"分没变、时间前移")。
  4. `HSET v pid "<rating_gen>:<games>"` → 返回 1(written)或 3(gen_reset)。
  - Lua 里 `floor(old/2^32)` 在 double 下精确(score < 2^53)。
- **幂等标记 TTL ≥ 保留期**(契约 §6):本设计的"标记"就是 `lb:{rating}:v`,无 TTL,天然满足。

### 3.3 score 编码、精度与同分排序

**同分规则(L-9;已拍板(用户 2026-09-20 拍板),见 §9.1#3)**:分数高者在前;**分数相同,先达到该分者在前**;名次连续(1,2,3,不做 1,1,3 的并列名次)。

- 为什么连续名次:客户端分页需要"第 N 页从第几名开始"可直接计算;并列名次要在每页首条额外做一次 `ZCOUNT`,且"并列第 5"占几行在翻页时语义模糊。Elo 保留两位小数,真正同分的概率本来就低,"先到先得"对玩家也可解释。
- 为什么"先达到":这是唯一有业务含义、且能从快照里拿到依据(`updated_at_ms`,与 rating 同源)的决胜键。"先达到"的语义由 §3.2 写入 Lua 第 3 步(同分不改时间分量)保证 —— 光靠 `updated_at_ms` 做不到,因为 match 在分数不变时也会推进它。
- "达到"的定义:分数**变成**该值的那一刻。玩家从 1500 掉到 1490 再回到 1500,"达到 1500"的时间是回来的那一刻。

**编码**(Redis ZSET score 是 float64,整数 **< 2^53** 才精确):

```
compositeScore = rating_centi × 2^32 + (2^32 − 1 − (updated_sec − EPOCH))
  rating_centi : 分×100 的精确整数(§2.4 的字符串解析),取值 [0, 2^21−1] = [0, 20971.51]
  updated_sec  : updated_at_ms / 1000(updated_at_ms 与 rating 同源,§2.4)
  EPOCH        : 1773446400(沿用 shared/snowflake 的秒级 epoch,仓内已有常量)
  最大值       : (2^21−1)×2^32 + (2^32−1) = 2^53 − 1   —— 恰好不越界
```

- 时间取补数,使同分时"更早"的值更大,ZREVRANGE 排在前面。32 位秒可覆盖 EPOCH 之后约 136 年。
- **越界处置(fail-closed 到可观测)**:`rating_centi ≥ 2^21` 或 `updated_sec < EPOCH` 时**拒写该条**并计 `rank_write_total{outcome="out_of_range"}`,不截断 —— 截断会让两个不同分数排出错误顺序且零报错。K=32 的 Elo 实际不可能逼近 20971,这条是防御。
- 读回时 `rating_centi = floor(score / 2^32)`;展示值由服务端解码后下发(客户端永不见复合分)。
- 代价:同分秒级分辨率;同一秒达到同分的两人仍按 member 字节序排 —— 可接受。

### 3.4 zone 维度:v1 只做全服榜

- match 匹配池全局(`config.go` `ZoneId` 注释"设计决策 D1"),评分也不分 zone;全服榜是评分的自然形状。
- 要做分区榜,zone 的唯一合法来源是 data_service 的 home zone 映射(契约 §0:全局服务**禁止**读 `cfg.ZoneId` 做分支;契约 §4:`zone_id` 唯一写入路径是 `BatchGetPlayerHomeZone`)。`player:{id}:location` 的 `zone_id` 是**当前场景所在 zone**,不是归属,不能用。
- 若由 match 在发布时查 zone 写进事件:合服 `RemapHomeZoneForMerge` 之后事件里的 zone 全部过期,rank 需要迁移分区榜;若由 rank 消费时查:每条事件多一次 RPC,且 data_service 不可用时要 fail-closed(拒写)。两者都要先定合服口径,v1 不背。
- **v1 全服榜与合服无关**:`tools/merge_zone` 只处理 `guild_rank:zone:*`、`guild_rank:maintenance_lock`、zone 库里的玩家行与 home_zone 映射(`main.go`、`merge_run.go`、`fence.go`、`audit_resources.go` 的 Redis 分布表),全目录对 `match:rating` 与 `lb:` **零命中**;player_id 是 login 的全局 snowflake,合服不改写。所以合服前后无需任何榜操作。将来开分区榜时,要把 `lb` 分区键纳入 merge_zone 的步骤与 `audit_resources.go` 的 Redis 分布表。

### 3.5 持久性:Redis-only 的风险与处置

- **事实**:公会榜有 MySQL 权威副本,能 `RebuildRanks`;rank v1 **没有**。`lb:{rating}:*` 丢了就没了。
- **权威定位(L-6)**:ZSET 是 `match:rating:*` 的**派生视图**,不是任何数据的唯一权威 —— 丢失的是视图,不是玩家资产。
- **丢失后的恢复路径(v1)**,两条叠加:
  1. **offset 重放(零成本 runbook,首选)**:快照 topic 保留 7 天,写入按 `(rating_gen, games)` 版本幂等,版本表丢了也就没有旧版本挡着,重放一遍即得正确终态。步骤:① 确认 `lb:{rating}:z` 与 `lb:{rating}:v` **一起**清空(只丢了一个时,先把另一个也 `DEL`,否则残留的版本表会把重放全部判成 stale、或残留的 ZSET 留下无版本的旧条目);② 停掉所有 rank 实例的消费者(缩容到 0 或停进程 —— 消费组必须无活跃成员才能重置);③ 把消费组 `rank-rating` 的 offset 重置到 earliest(`kafka-consumer-groups.sh --group rank-rating --topic match-rating-snapshot --reset-offsets --to-earliest --execute`);④ 重启 rank。7 天内打过排位的玩家全部恢复。
  2. **增量自愈**:7 天前最后一次打排位的玩家,等他打下一局时带着完整快照重新入榜。**代价**:这部分不活跃玩家在回来打一局之前不在榜上。
- **match 评分存储清空时的运维规则**:清空 MatchRedis(或其中的 `match:rating:*`,例如压测前 `FLUSHALL`)时,**必须同时** `DEL lb:{rating}:z lb:{rating}:v`,让榜与评分同口径重置。`rating_gen` 是忘了这一步时的兜底(新代际会覆盖旧条目),不是替代:不清的话,清空后没再打排位的玩家会以旧分一直挂在榜上。
- **为什么 v1 不做全量重建**:全量重建只有两条路 —— SCAN match 的 Redis(违反 D-14 第 6 条 + 契约 §4 不用 SCAN),或让 match 新增导出 gRPC(对 match 的第二处侵入 + 第一个 Go→Go 全局直连,触发 D-13 待拍项)。有 offset 重放兜 7 天,在没有线上数据证明"榜漏人"是问题之前不做。
- **部署侧减损(已拍板(用户 2026-09-20 拍板),见 §9.1#4)**:虽然 ZSET 不是唯一权威,仍按契约 §4 的严格档部署 —— staging / prod 的 `RankRedis` 用独立实例、`maxmemory-policy=noeviction`。理由:被淘汰是**静默**的(榜上少人没有任何报错),而这类 key 无 TTL,在 `allkeys-lfu` / `volatile-lru` 的共享库上会被悄悄驱逐;§3.7 告警④是事后发现手段,不是预防。本地 compose 缺省回落共享库时启动打 **WARN**(照 `go/chat/internal/svc/servicecontext.go`)。
- **不做维护锁 / 重建切换**:没有重建就没有锁;将来若加重建,锁必须带 fencing 或续期(公会榜的锁 TTL 5 分钟不续期,重建超过 5 分钟会并发重建 —— 已知坑,不照抄)。

### 3.6 可观测性(低基数,不含 player_id)

| 指标 | label | 用途 |
|---|---|---|
| `rank_write_total` | `outcome` ∈ written / stale(版本不新) / gen_reset(match 侧代际变了,覆盖写) / out_of_range / error | 写入健康;`stale` 比例反映重放 / 乱序程度;`gen_reset` 非零说明 match 评分键丢过(§3.5) |
| `rank_consume_errors_total` | 无 | 消费失败(反序列化 / Redis 写失败)次数 |
| `rank_last_consume_unixtime` | 无 | gauge,每**成功处理**一条消息写入 `time.Now().Unix()`;消费者卡死 / 再均衡卡住时它停住,告警①靠它 |
| `rank_consume_lag_seconds` | 无 | `now − finished_at_ms`,榜的新鲜度。**只在收到消息时更新**,卡死时停在最后一次正常值,**不能**用来发现卡死 |
| `rank_read_total` | `method`、`outcome` | 读路径 |
| `rank_read_duration_seconds` | `method` | 读延迟 |
| `rank_name_lookup_total` | `outcome` ∈ ok / error | 名字补全 fail-open 的可观测性(§4.3) |
| `rank_board_size` | `board` | ZCARD 定时采样(每个副本各自采样,聚合一律 `max()`);配合 match 侧的评分人数可粗略估漏人 |

match 侧新增 `match_rating_snapshot_publish_total{outcome}`(ok / error;**按消息条数**计,批量写失败时整批计 error)。

### 3.7 告警

照 `deploy/k8s/scene-manager-alerts.yaml` 的 PrometheusRule 形状新建 `deploy/k8s/rank-alerts.yaml`,放进 R2b。所有表达式只用上表的低基数 label,不加 player_id;多副本采样的 gauge 一律 `max()` 聚合。

| # | 名称 | 表达式(草案) | 级别 | 说明 |
|---|---|---|---|---|
| ① | RankConsumerStalled | `sum(increase(match_rating_snapshot_publish_total{outcome="ok"}[15m])) > 0 and (time() - max(rank_last_consume_unixtime)) > 900` | critical | **必须以"match 这 15 分钟确实发过"为前提**:只看 `time() - rank_last_consume_unixtime` 会在深夜没人打排位、或开关为 false 时误报。rank 看不到 match 的开关,用 match 的发布计数代替"开关打开且有流量" |
| ② | RankWriteFailing | `sum(rate(rank_write_total{outcome=~"error\|out_of_range"}[5m])) > 0` | warning | out_of_range 理论上不可能出现,出现即 bug |
| ③ | MatchRatingSnapshotPublishFailing | `sum(rate(match_rating_snapshot_publish_total{outcome="error"}[5m])) / sum(rate(match_rating_snapshot_publish_total[5m])) > 0.05` | warning | 发布失败会被重试吸收,持续 >5% 说明 Kafka 侧有问题 |
| ④ | RankBoardShrunk | `max(rank_board_size) < 0.9 * max_over_time(max(rank_board_size)[1h:1m])` | critical | 榜只增不减(v1 无删除),缩水 = Redis 丢失 / 淘汰。注意 PromQL 子查询必须写 `[1h:1m]`,`[1h]` 对聚合结果是语法错误 |
| ⑤ | RankReadFaults | `sum(rate(rank_read_total{outcome="fault"}[5m])) / sum(rate(rank_read_total[5m])) > 0.05` | warning | outcome 取值落码时与 `serverbase.TipVerdict` 的分类对齐 |
| ⑥ | MatchRatingGenReset | `sum(increase(rank_write_total{outcome="gen_reset"}[15m])) > 0` | warning | match 评分键丢过;非压测环境出现即查 MatchRedis 的淘汰策略 |

---

## 4. 协议与客户端读

### 4.1 服务与文件

- 文件:`proto/rank/rank.proto`,`package rankpb`,文件级 `option (OptionFileDefaultNode) = NODE_RANK;`(`NODE_RANK = 18` 已存在;friend 不写是因为没有 `NODE_FRIEND`,rank 没有这个问题)。
- 服务:`service ClientPlayerRank`,标 `option (OptionIsClientProtocolService) = true`。
- **目录必须叫 `proto/rank/`**:protogen `NodeServiceForCpp`(`tools/proto_generator/protogen/internal/model.go`)三级回落 —— `ClientPlayerRankNodeService` 不在枚举 → `Rankpb` + NodeService 不在枚举 → 回落目录名 `Rank` → `RankNodeService = 13`。目录若叫 `leaderboard`,会生成不存在的 `base.ENodeType_LeaderboardNodeService`,`route_table.go` 编不过。
- **服务名必须含 `ClientPlayer`**:`unity_client_handler.go` / `robot_case.go` 的 `isRelevantService` 只认 `ClientPlayer` / `GamePlayer`,不含就不出 robot handler,robot 冒烟无法做。代价:默认 `enable_unity_client: true` 会往客户端仓写 `ClientPlayerRank*Handler.cs` 桩并重写 `HandlerRegistry.cs`,客户端 `tools/gen_proto.ps1` 必须同批收录 `proto/rank/rank.proto`,否则客户端 CS0246(team / friend / jubaozhai 都踩过)。
- **写接口不存在**:写分只走 Kafka 消费,`ClientPlayerRank` 里**只有读方法**。这比公会的做法("写方法同 service、靠 session 白名单挡")更干净:服务级开关把同 service 的所有方法带进 gate 白名单与路由表,不放进去就不用挡。session 仍保留 fail-closed 方法白名单作纵深防御(L-12)。

### 4.2 方法与消息形状

**一个 RPC:`GetRankPage`**,把"拉一页"和"我的名次"合并。

- 为什么合并:客户端 gRPC 回包 id 为 0,只能按 message_id FIFO 匹配(客户端 `GameClient.DispatchInbound` Tier 2),同一功能只能单请求在途;拆成两个 RPC 就得串行两次往返。公会榜拆了,结果客户端从未调用过 `GetGuildRankByGuild`(客户端仓 grep 只命中常量)。

形状(草案,落码时字段号从 1 起;遵守 `AGENTS.md §4` 类型约束:`player_id` uint64,时间戳 uint64):

```proto
enum RankBoard {
  RANK_BOARD_UNSPECIFIED  = 0;
  RANK_BOARD_MATCH_RATING = 1;
}

message GetRankPageRequest {
  RankBoard board     = 1;
  uint32    page      = 2;  // 从 1 起;0 视为 1;超过末页按末页(照 jubaozhai BrowseListingsRequest)
  uint32    page_size = 3;  // 0 取默认 20;超过上限按上限 50
  // 刻意没有 player_id(D-9:我是谁只从会话取),也没有 zone_id(v1 只有全服榜)
}

message RankEntry {
  uint32 rank          = 1;  // 从 1 起,连续名次(§3.3)
  uint64 player_id     = 2;
  string name          = 3;  // 展示名;补全失败为空(§4.3)
  uint32 rating_centi  = 4;  // 分×100;客户端自己格式化两位小数
  uint64 games         = 5;
}

message GetRankPageResponse {
  TipInfoMessage     error_message = 1;
  repeated RankEntry entries       = 2;
  uint32             total_count   = 3;
  uint32             page          = 4;  // 钳制后
  uint32             page_size     = 5;  // 钳制后
  uint32             page_count    = 6;  // 至少为 1
  RankEntry          self          = 7;  // 未上榜时 self.rank = 0,其余字段为空(不是错误)
}
```

- **不带 `server_now_ms`**:§4.6 客户端规格里没有用途;字段号上线后收不回(`AGENTS.md §4`)。等客户端出现"最后更新时间"之类的需求再 append,无兼容问题。
- **未上榜不回 tip**:公会榜用 `kGuildNotRanked` tip 表达,对"拉一页 + 我的名次"合并形状不合适 —— 一页数据正常而 self 未上榜不是错误,回 tip 会让客户端把整页当失败。`self.rank = 0` 给客户端一个不报错的空状态。
- **分页防溢出**:照 `GetGuildRankPage` 在 uint64 下算 `start` 并先与榜长比较(防 uint32 / int64 乘法回卷),**但期望值与公会相反**:公会越界返回空页,rank 越界钳到末页(照 jubaozhai),响应里的 `page` 是钳制后的值。空榜时 `page_count = 1`、`page = 1`、entries 为空。
- **非原子**:ZCARD、ZREVRANGE、ZREVRANK 三步不加锁,页间可能重复或漏一条 —— 与公会榜同口径,读的是软数据,接受。推荐用一条只读 Lua 把 ZCARD + ZREVRANGE + ZREVRANK + ZSCORE 合成一次往返,同时消除页内不一致(单 key 同 slot)。
- **不用游标**:榜是"按名次看",页码是用户心智;游标在榜变动时的语义也不比页码好。

### 4.3 名字补全

- 数据源:`data_service` 的 `BatchGetPlayerName`(`proto/data_service/data_service.proto`),真源是 data_service **全局库**的 `player_name` 表(全服唯一,`rollback_database_table.proto` 注释),所以跨 zone 玩家的名字都能查到。
- 整页 + self **一次批量**(上限 500,page_size 上限 50 远低于此),不逐条回表(公会榜逐条 `GetGuild` 是 N+1)。
- **fail-open**:超时 800ms(照 `go/guild/internal/logic/player_name_resolver.go` 的 `DefaultPlayerNameLookupTimeout`),失败时名字留空、榜照返回。依据:名字是展示数据,`player_comp.proto` 注释对战斗快照也是同一口径;风险由 `rank_name_lookup_total{outcome="error"}` 承担(`AGENTS.md §11.3` 要求 fail-open 必须配指标)。
- rank → data_service 是 `RpcClient` + `Key: dataservice.rpc`:data_service 按 zone 部署,**D-13 不禁止**(`go/trade/etc/trade.yaml`、`go/guild/etc/guild.yaml` 已有先例)。本地 `-Zone N` 下会被改写成 `dataservice.rpc.z<N>`,只连本 zone 的 data_service;因为名字表是全局库,查询结果与连哪个 zone 无关 —— **这一点是冒烟 R2 要实测的,不是假设**。
- 预算:zrpc `Timeout 4000` ≥ Redis 读 + 名字 800ms + 余量,够用。

### 4.4 身份、拦截器、超时

- **D-9**:会话从 `x-session-detail-bin` 解码进 ctx(照 `go/friend/internal/session/session.go`);self 名次用会话里的 player_id;请求体**不设** player_id 字段。首个提交就带拦截器,不交付"信请求体"的版本。
- **两种"没身份"分开处理**(与 friend 同口径):
  - **无会话元数据** → 拦截器**放行**(friend `UnaryServerInterceptor` 在 `len(values)==0` 时直接 `return handler(ctx, req)`)→ 逻辑层 `ClientPlayerID` 返回 ok=false → 回 in-band `kInvalidParameter`(非故障码),entries 为空,**不查 Redis**。**不能**用 `kPlayerNotFoundInSession`(1012,fault 码,契约 §3)。
  - **会话头无法解码** → 拦截器回 gRPC `codes.Unauthenticated`,handler 不被调用。
- **拦截器链**:`grpcstats → killswitch → session 解码 → serverbase(TipVerdict) → handler`,以 `go/friend/friend.go` 的 `buildUnaryInterceptors` 为模板,配三个守链测试(`TestBuildUnaryInterceptorsOrder` / `TestKillSwitchWiredIntoUnaryChain` / `TestKillSwitchFailOpenWithoutRules`)。
- **超时**:zrpc `Timeout: 4000`(路由服 `ForwardTimeoutMs 5000 − 1000`),`config.Validate` 拒 0 与 >4000(照 `go/chat`)。
- **幂等**:读方法无副作用,契约 §3 的幂等键要求不适用。

### 4.5 限频与 tip

- **MessageLimiter**:不配档位就吃 gate 默认"1 秒窗口 3 次"。榜是查询类,照 team 的查询档配 **5 次/秒**(`friend-handoff §4.2` 记 team 查询类 5 次/秒)。**必须排在 proto-gen 之后**,按 `proto/message_id.txt` 里新分配的号填 `data/MessageLimiter.xlsx`。被限频时客户端不得自动重试(gate `IllegalPacketCounter` 会累计并 `forceClose`)。
- **tip 段 21000:v1 不开**(L-14)。v1 的错误码全部复用 common 段(已核 `generated/code/proto/tip/common_error_tip.proto` 与 `go/shared/generated/tip/faults.go`):

  | 情形 | 码 | fault | 常量写法(`internal/constants`) |
  |---|---|---|---|
  | 无会话 / 未知榜类型 | `kInvalidParameter = 1005` | 否(不在 `tip.Faults`) | `ErrInvalidParameter = uint32(table.CommonError_kInvalidParameter)` |
  | Redis 故障 / 读路径故障 | `kServiceUnavailable = 1003` | **是**(`tip.Faults` 含 1003,`serverbase.TipVerdict` 计入服务端故障) | `ErrServiceUnavailable = uint32(table.CommonError_kServiceUnavailable)` |

  枚举 Go 名以生成物为准。rank 服务里**不另写故障码集合**(`AGENTS.md §7` #5);`constants_test.go` 照 guild / friend 带 `TestNoHandWrittenTipCodes`。rank 的 21000 段在生成物里零占用。
  - 为什么不开:`Tip.xlsx` 多会话共写,每次改动都有零报错丢失风险(见 memory "共用工作树的多会话纪律");开一个空段还要同步 C++ 表工程三件套(`cpp/generated/table/CMakeLists.txt`、`table.vcxproj`、`table.vcxproj.filters`)与客户端 `gen_proto.ps1`。没有码就没有收益。
  - **开段时机**:第一个 rank 专属码出现的那一批(例如赛季结算"未到结算期")。开段方式:**新插一行**组头 `//rank_error base=21000 width=1000`(Tip.xlsx 只许 insert_rows);**不要**按预留注释说的"把本行改成组头" —— 那一行是 `rank / dialogue / battle_result / grant` 四个域共用的注释,改掉会抹掉别人的预留。

### 4.6 客户端规格(只是规格,改客户端仓需要授权)

- 客户端仓三处手改(只跑生成器不够):① `tools/gen_proto.ps1` 的 `$files` 加 `proto/rank/rank.proto`(v1 无 rank tip proto);② `tools/gen_messageids.ps1` 的 `$whitelist` 登记 `"ClientPlayerRankGetRankPage" = "GetRankPage"`(漏配只出 Warning,常量静默不生成);③ 手写 `RankClient`,照 `GuildClient.Request` 用 `_net.Call(MessageIds.GetRankPage, …)`,单请求在途,出错置 `RequiresReconnect`。生成的桩不是接线点(`HandlerRegistry.Register` 客户端全仓零调用)。
- UI:照 `GuildWindow.RenderRanking` + 私有 `Pager`;客户端单页约 5 行,请求 `page_size = 5`。self 条目固定显示在列表下方,`rank = 0` 显示"暂未上榜"。
- ⚠ 客户端仓工作区有一处**未提交**改动(`tools/gen_proto.ps1` 新增 friend.proto 一行),给 rank 补清单时不要冲突,也不要夹带。
- 服务端验收在客户端开工前用 robot `rank-smoke` 代替(§8)。

---

## 5. 部署与登记清单

### 5.1 端口:gRPC `51100` / 指标 `:9250`(已复核)

- **全仓 grep(排除 `third_party/`,`\b51100\b|\b9250\b`,回修时 2026-09-20 再跑一次)**:命中只有 3 个文件 —— `friend-handoff-20260920.md`(建议文字)、`PROGRESS.md`(旧排批记录)、本文。`third_party/zlib/crc32.h` 的 `0x…851100…` 与 `generated/tables/manifest.json` 的 sha256 片段是十六进制子串误报(按词边界 grep 不命中)。
- **旧号 51000 为什么不能用**:`deploy/docker-compose.login-stack.yml`(`"51000:51000"`)与 `deploy/login-stack.linux/login.yaml`(`ListenOn: 0.0.0.0:51000`)是 login 的 staging 端口;本地 `-Zone 3` 位移后 51000 → 53000 撞 zone 1 的 login。
- **本地位移核验**(`tools/scripts/go_services.ps1`:`port = Port + (Zone−1)×ZonePortShift + (Index−1)×PortStride`,`ZonePortShift=1000`、`PortStride=1`,再经 `Resolve-BindablePort` 避开 Windows 保留区):

| zone | rank gRPC | rank 指标 | 最近邻(不撞) |
|---|---|---|---|
| 1 | 51100 | 9250 | match z2 51500、login-stack 51000(非本地栈) |
| 2 | 52100 | 10250 | data_service z2 10000(gRPC,不同端口号) |
| 3 | 53100 | 11250 | login z1 53000(+100)、player_locator z1 53200(−100) |
| 4 | 54100 | 12250 | login z2 54000、player_locator z2 54200 |

  - 对照 `$ServiceCatalogue` 全部 11 个基址(db 6000、data_service 9000、player_locator 53200、login 53000、scene_manager 60300、match 50500、client_rpc_router 50600、chat 50700、guild 50300、trade 50800、friend 50400)在 zone 1–4 的位移值,**全部不撞**;现有基址没有以 `100` 结尾的,多实例步长 1 要开到第 101 个 login 实例才会碰到 rank。
  - 指标端口:顶层 `MetricsListenAddr` 的服务(match 9170 / friend 9180 / db 9160 / scene_manager 9150 / chat 9210 / trade 9230)位移后落在 x150–x230,9250 系列不撞;login 9101 / player_locator 9190 / guild 9220 是嵌套 `Prometheus: Port:`,`-Zone` 不改写它们,也不撞。
  - K8s 不按 zone 位移,`$GoSvcCatalogue` 端口集(6000 / 9000 / 50000 / 50100 / 60000 / 50500 / 50700 / 50600 / 50800 / 50400)与 51100 不撞;K8s 已用指标端口 9150 / 9170 / 9180 / 9200 / 9210 / 9230。
  - Windows 保留区每次开机随机(历史观测 51573–51872;机器 B 2026-09-20 实测 51840–51939),51100 / 52100 / 53100 / 54100 都不在已观测区间内,但**不能保证**;落进去也由 `Resolve-BindablePort` 自动上挪,对等方经 etcd 发现实际端口。

### 5.2 五处登记(契约 §7,端口逐字一致)

| # | 文件 | 内容 |
|---|---|---|
| 1 | `tools/scripts/go_services.ps1` `$ServiceCatalogue` | `rank = @{ Dir = "rank"; Entry = "rank.go"; Port = 51100; Desc = "Rank (全局排行榜,路由服可达)"; ConfigFlag = "-f"; ConfigFile = "etc/rank.yaml"; AllowMultiInstance = $true; Tier = 1 }`。**Tier 1** 的依据与 trade 相同(该文件 trade 条目上方注释):只依赖基础设施(Redis / etcd / Kafka)与 Tier 0 的 data_service,且 `DataServiceRpc` 是 NonBlock 拨号,不拨 login 等服务 |
| 2 | `tools/scripts/go_svc_image.ps1` `$Catalogue` | `rank = @{ Dir = "rank"; Entry = "rank.go"; ImageName = "mmorpg-rank" }`,ImageName 与 #3 逐字一致 |
| 3 | `tools/scripts/k8s_deploy.ps1` | **目录条目**:`rank = @{ ConfigMap = "go-svc-rank-config"; Manifest = "rank.yaml"; Port = 51100; ConfigFlag = "-f"; ConfigFile = "rank.yaml"; ImageName = "mmorpg-rank"; Global = $true }`(**无 `MigrateJob`**)。**`New-GoSvcConfigMapYaml` 的 `rank` case**,逐段:`Name`;`ListenOn: 0.0.0.0:51100`;`Timeout`(`Get-AuthoritativeScalar` 读 `go/rank/etc/rank.yaml`);`Mode`(dev 档取 yaml,staging/prod 固定 `pro`,照 friend);`Etcd.Hosts: etcd.${InfraNamespace}:2379` + `Key: ""`;`ZoneId: ${CurrentZoneId}`;`MetricsListenAddr: ":9250"`;`LeaseTTL`;`KillSwitchPrefix: ""`(照 friend);`Kafka.Brokers: kafka.${InfraNamespace}:9092`;快照 topic 名、分区数、`RatingConsumerGroup`(均 `Get-AuthoritativeScalar`,**不写保留期** —— 保留期只由 match 声明,§2.4);`DataServiceRpc` 段照 trade(`Etcd.Hosts=etcd.${InfraNamespace}:2379`、`Key: dataservice.rpc`、`NonBlock: true`、`Breaker: false`,并注明"调用方发现键,不是 D-13 禁止的注册 Key");`RankRedis` 段(取值按 §9.1#4 的拍板结果,**不写 DB**)。rank **不需要 SharedRedis**;私有句柄必须叫 `RankRedis`,**不得占用 zrpc 自带的 `Redis` 段名**。万一用到 `Redis` 段,必须显式写 `Key: ""`,否则加载期 `"Redis.Key" is not set`、Pod CrashLoop(friend case 注释记过这个坑) |
| 4 | `deploy/k8s/manifests/go-svc/rank.yaml` | 照 `friend.yaml`:Service + Deployment(replicas 2、podAntiAffinity、`POD_IP` Downward API)+ PDB;Service `port`/`targetPort` 51100、`containerPort` 51100 与 9250、探针 51100、`prometheus.io/port: "9250"`;不写 namespace |
| 5 | `tools/scripts/start_game.ps1` | **三行**,照 trade / friend:① `$services += 'rank'`;② `$optionalServices += 'rank'`(理由照该文件头注释:本机可能还没有 exe,为一个服务拒启会让登录、匹配一起不可用;rank 又是纯展示功能);③ 在 friend 那行 `if ('friend' -notin $skippedServices) { Start-LocalGoServices @('friend') }` 之后追加 `if ('rank' -notin $skippedServices) { Start-LocalGoServices @('rank') }`。**不做库预检**(无 MySQL)。只加 ① 的话 rank 不会被启动 —— `$services` 只用于缺 exe 检查,启动靠逐个 `Start-LocalGoServices`;缺 ② 则缺 exe 时整个启动中止 |

本地 `go/rank/etc/rank.yaml` 的锚点(形状一变 `-Zone` 派生就静默失效):顶层单行 `ListenOn: 127.0.0.1:51100`;顶层唯一 `ZoneId: 1`,写在所有嵌套段之前、行尾不带注释;顶层单行 `MetricsListenAddr: ":9250"`(**不能**写成 login/guild 那种嵌套 `Prometheus: Port:`,否则双 zone 撞同一指标端口);`Etcd:` 写 `Hosts` + `Key: ""`;消费组键写 `RatingConsumerGroup: rank-rating`,**不得**出现 `GroupID:`(否则 `-Zone` 加 `_z<N>`,全局 topic 被各 zone 各消费一遍);启动横幅用 `=====` 框住 `RANK SERVICE STARTED SUCCESSFULLY`。

另外几处不属于"五处"但同批要做:
- 契约 §7 的**指标端口分工表**补 `9230 trade` 与 `9250 rank`(该表今天停在 `9220 guild`)—— 改契约文档不属于本轮,只列。
- match 的 K8s ConfigMap case 加 `RatingSnapshotTopic` / 分区 / 保留期,以及开关 `RatingSnapshotPublishEnabled`(取值来源见 §5.4,**不镜像 yaml**)。
- `deploy/k8s/README.md` "Kafka 保留期"一节的**现状表**加一行:`match-rating-snapshot | match EnsureTopics | 显式 7d(match 侧) | rank(组 rank-rating) | ✅`。该节规则要求"任何消费者可能落后的 topic 必须显式声明保留期"。

### 5.3 其余登记

| 项 | 结论 | 依据 |
|---|---|---|
| `proto_gen.yaml` `domain_meta` | 加 `rank` 块,**只出 Go 产物**(`proto` / `handler` / `grpc` 到 `go/generated/rank/...`),形状照 `friend` 块;漏掉此块 = `rank.proto` 根本不被解析,没有消息号也没有路由表条目(team 块注释) | v1 无 C++ 调用方,不出 cpp 产物 |
| C++ `nodeTypeNameMap` / `base_deploy_config.yaml` / K8s C++ 前缀 / scene 白名单 / READY 选择器 | **v1 不改**:这是契约 §8"C++ 直连 gRPC"那条路的清单,v1 写分走 Kafka、客户端走路由服,均不需要 | friend 登记了前两处但依据未核到(§9.2#1) |
| 路由服 | 无配置改动:`TargetNodeTypes(game.RouteTable)` 从生成的路由表自动派生要 watch 的 `RankNodeService.rpc/`;重生成后重部署即可 | `go/client_rpc_router/internal/svc/servicecontext.go` |
| gate 直连白名单 | **不补**(D-12);客户端只承诺路由服模式 | `cpp/nodes/gate/main.cpp` |
| **K8s 可达性** | **K8s 上 rank 默认对客户端不可达**:`k8s_deploy.ps1 -GateRouterMode` 默认 `"0"`(直连),翻成 1 的前置(D-12:K8s 上以路由模式跑通一次 battle-smoke、路由服 manifest 落地、POD_IP 通告)尚未满足。在那之前 rank 在 K8s 上只能消费写榜,Pod Ready、指标正常但玩家请求一个都到不了 —— 与 chat / trade / friend 同一已知缺口,是设计内行为。上线公告与验收**不能**拿 K8s 当可达环境 | D-12;`k8s_deploy.ps1` 目录注释 |
| 库归属(D-14) | **不触发**:D-14 适用于"要建表的新全局服务";v1 零 MySQL,照 chat v1 先例。因此**不需要** `mmorpg_rank` 库、`00_init_zone_dbs.sql`、`start_game.ps1` 库预检、`rank-migrate` Job、`MigrateJob` 字段。私有 Redis 按契约 §4 走独立句柄 `RankRedis` | 契约 §4 "chat v1 零 MySQL" |
| 号段 biz_tag | **不开**:榜条目的身份就是 player_id,没有需要永久 guid 的新实体 | 四处 tag 清单(`DefaultIdSegmentBootstrapTags` 等)不动 |
| `Etcd.Key`(D-13) | 顶层 `Etcd.Key: ""`(显式空串,不能省略整行,否则 go-zero `conf.MustLoad` Fatal);`config.Validate` 拒绝非空;唯一的 `RpcClient` 是 `DataServiceRpc`(`Key: dataservice.rpc`,按 zone 部署,D-13 允许) | `go/chat/etc/chat.yaml` |
| 失租(D-11) | `shared/noderegistry`,`OnReclaimFailed = noderegistry.ReallocateNewID`,`LeaseTTL: 60`;`RegisterAfterListening → KeepAlive`,退出先 `Close` 再停 gRPC。rank 无发号器、无 `{type}-{id}` topic,**不适用** `ExitProcess` | chat / friend / trade 同形 |
| K8s 私有 Redis | **已拍板(用户 2026-09-20 拍板):独立实例、noeviction**(§3.5)。不暂借 `redis-match-cluster` | 契约 §4 |
| killswitch | `GetRankPage` 配规则占位 | D-12 |

### 5.4 发布顺序与开关(契约 §3 + 本服务的 Kafka 边)

1. **R1p 的 proto-gen 只跑一次**(`rank.proto` + `match_event.proto` 的新 message 一起);路由服、gate、robot、客户端桩的生成物必须同源。跑之前客户端仓 `gen_proto.ps1` 已收录 `proto/rank/rank.proto`(否则 CS0246;需要客户端改仓授权,§9.1#6)。
2. 按 `proto/message_id.txt` 填 `data/MessageLimiter.xlsx`(必须在 proto-gen 之后)。
3. 部署 **rank** 与 **match**(新版本,开关 `false`)。**两者谁先起都安全**:双方都 `EnsureTopics` 分区契约,保留期只由 match 声明(§2.4),rank 先起时 topic 暂用 broker 默认保留期、无生产者、空转无害;match 先起时 topic 由 match 建好。K8s 上两者都是 `Global`,由 `Apply-GlobalGoSvcManifests` 在同一个 infra-up 里遍历**无序** hashtable 部署,本来也没有机制保证先后 —— 所以本设计**不依赖**部署顺序。
4. 部署**路由服**,再部署 **gate**(新消息号要重编 gate)。
5. **唯一的上线闸门**:翻 match 的 `RatingSnapshotPublishEnabled = true`,滚动重启 match。
6. 回滚:第 5 步翻回 `false` 即停止写榜(榜仍可读);其余逆序。

**开关的来源**:
- **K8s**:match ConfigMap 的 `RatingSnapshotPublishEnabled` **不镜像** `match_service.yaml`,改为 `k8s_deploy.ps1` 的显式参数(建议 `-MatchRatingSnapshotPublish '0'|'1'`,默认 `'0'`),写法照该文件 trade case 里 `AssetOp.Enabled` 的"所有档位固定值、不镜像服务 yaml"。翻开 / 回滚都是用同一个参数重跑部署,不需要改提交进仓的 yaml(参数形态已拍板(用户 2026-09-20 拍板),§9.1#7)。
- **本地**:`go_services.ps1` 的派生 yaml 来自源 yaml,所以本地开关就是 `go/match/etc/match_service.yaml` 里的值(仓内默认 `false`)。rank-smoke 的步骤说明写明:冒烟前把该值改为 `true`、跑完改回,**不提交**这一改动;或者另加 dev_tools 参数(不在 v1 范围)。

---

## 6. 决策记录

| # | 结论 | 理由 | 代价 |
|---|---|---|---|
| L-1 | 写分来源选 **(a) match 新增出站快照事件**(**已拍板(用户 2026-09-20 拍板)**) | (b) 在规则 / 配置 / 状态 / 顺序四维产生第二份真相,顺序维无解;(a) 只有一份真相且快照可自愈(§2.3) | 要改 `go/match`(约 10–11 文件);match 对外承诺 `(rating_gen, games)` 单调语义;match 那批组队代码未编译,R1 开工前先编译一次(§7 R1 前置) |
| L-2 | 事件是**快照**(rating_centi + games + updated_at_ms + rating_gen),不是增量 Δ;名叫 `PlayerRatingSnapshotEvent`;**updated_at_ms 与 rating/games 同源**(同一次 Lua 读出);不带 match_mode | 丢一条可被下一条覆盖;Δ 在续写路径与 0 分下限下不可靠;Go 侧 `now` 在续写路径上是重试时刻 | 事件略大;消费端必须做版本比较;match 的 Lua 返回值扩到 5 元素 |
| L-3 | 工程名 `rank`:目录 `proto/rank/`、`go/rank/`、端口登记名 `rank` | `RankNodeService = 13`、`NODE_RANK = 18` 已存在;protogen 靠目录名回落出节点类型,叫 leaderboard 会编不过 | 功能名与工程名不一致,文档里要反复说明 |
| L-4 | v1 只做一个榜:match 评分,全服,不分模式 | 评分本身不分模式;zone 拿不到且合服口径未定 | 产品若要"本区榜 / 1V1 榜"都要等前置 |
| L-5 | 发布点在 `ApplyBattleResult` 全员写完、置 done 之前,经包级接缝 `ratingSnapshotPublishFn` **一次批量**写;失败返回 error 走现有重试。topic `match-rating-snapshot` / 3 分区 / 7 天 **(已拍板,见 §9.1#5)** | 置 done 前失败才能重试;批量把最坏延迟从 N×5s 降到 5s;接缝让发布类用例不连 Kafka | 评分循环多一次 Kafka 往返;重试用尽的那局快照丢失(下局自愈) |
| L-6 | ZSET 是派生视图,**不是唯一权威**;v1 无 MySQL、无全量重建;丢失后先 offset 重放(7 天内全恢复),再靠增量自愈 **(恢复口径已拍板(用户 2026-09-20 拍板),见 §9.1#2)** | 权威在 `match:rating:*`;重建需 SCAN 他人存储或 match 导出 RPC,均无当前需求 | 7 天前最后一次打排位的玩家从榜上消失,直到再打一局;重放需要停 rank 消费者 |
| L-7 | staging/prod 的 `RankRedis` 用独立 noeviction 实例 **(已拍板(用户 2026-09-20 拍板))** | 淘汰是静默的,无 TTL 的键在 LRU/LFU 库上会被悄悄驱逐 | 多一个 Redis 实例 |
| L-8 | 键根前缀 `lb:`,hash tag `{rating}` 把 ZSET 与版本 HASH 钉在同一 slot;键只由构造函数生成 | 避开 `guild_rank*`(会被 `RebuildRanks` 扫掉)与 `match:*`;写入 Lua 要跨这两个键原子 | 单榜落单 slot,不能横向切 |
| L-9 | 同分:先达到者在前,名次连续 **(已拍板(用户 2026-09-20 拍板))** | 唯一有业务含义且可从快照得到的决胜键;连续名次让分页可算 | 同秒同分仍按字节序;不提供"并列第 N"展示 |
| L-10 | score = `rating_centi × 2^32 + (2^32−1−(sec−1773446400))`,上限 2^53−1;越界拒写 + 计数;**分数不变时 rank 写入 Lua 保留旧时间分量**,只更新版本 | float64 只在 2^53 内精确;截断会零报错地排错序;match 每局都推进 `updated_at_ms`,不保留就变成"最近一局更早者在前" | rating 上限 20971.51;时间分辨率 1 秒;epoch 后 136 年;写入多一次 ZSCORE |
| L-11 | 幂等 / 乱序靠 `(player_id, rating_gen, games)` 字典序版本比较,单条 Lua 条件写;无全局锁 | games 由 `HINCRBY` 在代际内严格单调;公会榜的全局锁会串行化高频写 | 依赖 match 的两条承诺(代际内 games 单调、代际只增);**失效模式**:没有 `rating_gen` 时,match 评分键被淘汰 / 清空后 games 回退,rank 会**永久拒写**该玩家(榜冻结在旧分);有 `rating_gen` 后由 gen_reset 覆盖并计数告警。match 多实例时钟回拨超过"键丢失到重建"的间隔时代际可能不增 —— 可接受 |
| L-12 | `ClientPlayerRank` 只放读方法;session 仍保留 fail-closed 方法白名单 | 服务级客户端开关会把同 service 的所有方法带进路由表;不放写方法是根治,白名单是纵深 | 将来若加内部写 RPC 必须另建不带开关的 service(照 `trade_admin.proto`) |
| L-13 | 一个 RPC `GetRankPage` 合并"一页 + self";响应不带 `server_now_ms` | 客户端 gRPC 回包 FIFO,单请求在途;公会榜拆开后客户端从未用过查名次;无消费方的字段号收不回 | 每次翻页都附带 self 查询(一次 ZREVRANK + ZSCORE,可并入同一 Lua) |
| L-14 | tip 段 21000 v1 不开,只用通用码:`kInvalidParameter`(1005,非故障)/ `kServiceUnavailable`(1003,故障) | 无专属码需求;Tip.xlsx 多会话共写有丢失风险,开段还要同步 C++ 表工程三件套 | 客户端无法区分"rank 专属错误",v1 也没有 |
| L-15 | 榜类型枚举不预留 GUILD | GUILD 不计划搬;留一个永不实现的枚举值会让人以为它可用 | 将来真要统一时枚举要加值(append-only,无兼容问题) |
| L-16 | 名字补全 fail-open,800ms 预算,整页一次 `BatchGetPlayerName` | 展示数据;名字表是全局库,连任一 zone 的 data_service 都查得到 | 失败时整页空名;由指标暴露 |
| L-17 | 端口 51100 / 指标 9250 | 全仓复核零占用;zone 1–4 位移全部不撞(§5.1) | 契约 §7 分工表需补登 |
| L-18 | D-14 不触发:无库、无 migrate Job、无 biz_tag | v1 零 MySQL,照 chat v1 先例 | 加赛季 / 历史时一次性补齐 D-14 全套 |
| L-19 | 失租用 `ReallocateNewID`,`LeaseTTL 60` | 无发号器、无 per-node topic,无状态服务 | 无 |
| L-20 | match 侧发布带开关,v1 默认 `false`,是**唯一的上线闸门**;K8s 上由 `k8s_deploy.ps1` 显式参数控制、不镜像 yaml;**topic 保留期只由 match 声明**,rank Ensure 时 `RetentionMs: -1` **(topic 参数与开关参数形态均已拍板(用户 2026-09-20 拍板),见 §9.1#5 / #7)** | 独立回滚;K8s 全局服务部署无序,不能靠部署顺序;两处声明保留期会随重启顺序来回改写(`db_task` 的前车之鉴) | 上线多一步翻开关 |
| L-21 | 新增告警文件 `deploy/k8s/rank-alerts.yaml`(§3.7),消费者卡死以"match 有发布而 rank 无消费"判定 | `rank_consume_lag_seconds` 只在收到消息时更新,发现不了卡死;仓内无 Kafka lag exporter | 多一个 PrometheusRule;告警依赖 match 指标 |

---

## 7. 分批建议

`AGENTS.md §10.2`:预计改动 30+ 文件要停下报告。下面的文件数只计**手写文件**;proto-gen 生成物(`go/generated/rank/**`、各服务 `message_id.go`、路由表、robot / 客户端桩)不在手写计数内,但会进入 diff,评审时单独看。

依赖关系:`R0 → R1p →(R1 ∥ R2a-读)→ R2a-写 → R2b`;R3 需授权,排在 R2b 冒烟之后。

| 批 | 前置 | 内容(逐文件) | 手写文件 | 验收判据 |
|---|---|---|---|---|
| **R0**(本批) | — | 本设计文档 | 1 | **§9.1 每一项在 §6 对应行都有用户确认的结论,并去掉"待拍板"标记**;否则 R1p 不能开工。**✅ 已完成(2026-09-20)** |
| **R1p 契约批** | R0 | ① `proto/contracts/kafka/match_event.proto`(新 message `PlayerRatingSnapshotEvent`,字段见 §2.4);② `proto/rank/rank.proto`(§4.2);③ `proto_gen.yaml` 的 `domain_meta` 加 `rank` 块 | 3 | Codex 跑**唯一一次** proto-gen(与 friend 的 11 个待生成桩同一次,§9.1#6);确认 `go/generated` 里有 `PlayerRatingSnapshotEvent` 与 `rankpb` 两个类型,friend 的待生成桩一起出来;路由表出现 `RankNodeService` 条目 |
| **R1 match 出口** | R1p 生成完成;**match 组队代码先编译通过一次**(`friend-handoff §4.2`) | ① `go/match/internal/logic/rating.go`(Lua 返回 5 元素 + `HSETNX rating_gen` + 续写分支 `tonumber` + `applyRatingDelta` 新签名与双类型解析 + `ratingSnapshotPublishFn` 接缝 + 发布点);② `go/match/internal/kafka/rating_snapshot.go`(构造器,不 import svc/logic);③ `go/match/internal/logic/rating_snapshot_test.go`(新,miniredis + 替身);④ `go/match/internal/kafka/rating_snapshot_test.go`(构造器 / key = player_id);⑤ `go/match/internal/metrics/metrics.go`(`match_rating_snapshot_publish_total`);⑥ `go/match/internal/config/config.go`(topic、分区、保留期、开关、Validate);⑦ `go/match/internal/config/config_test.go`(新建;该目录今天无测试);⑧ `go/match/etc/match_service.yaml`;⑨ `go/match/match_service.go`(EnsureTopics,显式保留期);⑩ `tools/scripts/k8s_deploy.ps1`(match case + `-MatchRatingSnapshotPublish` 参数);⑪ `deploy/k8s/README.md`(保留期现状表一行) | ~11 | 开关 `false` 时行为与旧版逐字一致(已有 rating 测试全绿,且接缝替身调用计数为 0);开关 `true` 时:正常局发出 N 条、各字段与 Lua 返回一致;模拟发布失败 → 返回 error 且标记仍 applying;重放同一局 → 续写路径仍发布当前快照(T10);rating_centi 精确(T4b);rating_gen 行为(T13);**发布类用例只用 miniredis + 替身函数、不连 Kafka,SKIP 数必须为 0**;`go vet` 无 import 环 |
| **R2a-读 rank 服务骨架 + 读路径** | R1p 生成完成 | `go/rank/`:① `go.mod` ② `go.sum` ③ `rank.go`(装配 + `buildUnaryInterceptors`)④ `rank_test.go`(三个守链测试)⑤ `etc/rank.yaml` ⑥ `internal/config/config.go` ⑦ `internal/config/config_test.go` ⑧ `internal/constants/constants.go` ⑨ `internal/constants/constants_test.go`(`TestNoHandWrittenTipCodes`)⑩ `internal/svc/servicecontext.go`(RankRedis 句柄 + 回落 WARN、DataServiceRpc)⑪ `internal/session/session.go` ⑫ `internal/session/session_test.go` ⑬ `internal/lifecycle/lifecycle.go` ⑭ `lifecycle_unix.go` ⑮ `lifecycle_windows.go` ⑯ `internal/data/keys.go`(根前缀常量 + 键构造函数)⑰ `internal/data/keys_test.go`(T9)⑱ `internal/data/read.go`(只读 Lua)⑲ `internal/data/read_test.go` ⑳ `internal/logic/get_rank_page.go` ㉑ `internal/logic/name_resolver.go` ㉒ `internal/logic/get_rank_page_test.go` ㉓ `internal/metrics/metrics.go` ㉔ `internal/server/rank_server.go`(照 friend 的 `internal/server`) | ~24 | T3(读侧)、T5、T6、T7、T7b、T8、T9、T11 通过;三个守链测试;`config.Validate` 拒绝 Timeout 0 / >4000 与非空 Etcd.Key |
| **R2a-写 消费者 + 写路径** | R2a-读 | ① `go/rank/internal/kafka/consumer.go`(消费组 `rank-rating`,EnsureTopics `RetentionMs: -1`)② `internal/kafka/consumer_test.go` ③ `internal/data/write.go`(写 Lua,§3.2)④ `internal/data/write_test.go` ⑤ `rank.go`(接消费者,追改)⑥ `internal/config/config.go`(topic / 分区 / 组,追改)⑦ `internal/metrics/metrics.go`(写侧指标,追改) | ~7(其中 3 个是追改) | T1、T2、T3、T3′、T4、T10(消费端部分)、T12 通过 |
| **R2b 部署登记** | R2a-写;R1 | ① `go_services.ps1` ② `go_svc_image.ps1` ③ `k8s_deploy.ps1`(rank 目录 + case)④ `deploy/k8s/manifests/go-svc/rank.yaml` ⑤ `start_game.ps1`(三行)⑥ `deploy/k8s/rank-alerts.yaml`(§3.7)⑦ `data/MessageLimiter.xlsx` ⑧–⑨ robot `rank-smoke` 场景 + 配置 ⑩ `PROGRESS.md` 追加 | ~10 | 端口登记用**限定范围的精确集合**判:`rg -l '\b51100\b' tools/scripts deploy go/rank/etc` 的结果**恰等于** {`tools/scripts/go_services.ps1`, `tools/scripts/k8s_deploy.ps1`, `deploy/k8s/manifests/go-svc/rank.yaml`, `go/rank/etc/rank.yaml`};`rg -l '\b9250\b' tools/scripts deploy go/rank/etc` 恰等于 {`tools/scripts/k8s_deploy.ps1`, `deploy/k8s/manifests/go-svc/rank.yaml`, `go/rank/etc/rank.yaml`}。多一个或少一个都算红(`rank-alerts.yaml` 注释引用端口时写"见 manifest",不写数字)。双 zone 本地栈起得来;`rank-smoke` 按 §8.2 R 系列通过 |
| **R3 客户端**(需授权) | R2b 冒烟通过 | 客户端仓 `gen_proto.ps1`、`gen_messageids.ps1`、`RankClient.cs`、窗口 + 入口 | ~5 | 客户端编译无 CS0246;EditMode 用例 |

- 每一批都只计手写文件,均低于 30;生成物只在 R1p 进 diff。若某批落码时超过估算 5 个以上,停下报告而不是继续堆。

---

## 8. 验证清单

Claude 不跑任何构建 / 测试(`AGENTS.md §10.1`),以下由 Codex 执行。每条写清"怎么算过";⚠ 标的是**结构性不可能失败的假绿陷阱**。

### 8.1 单元 / 集成(R1、R2a)

| # | 用例 | 怎么算过 | 假绿陷阱 |
|---|---|---|---|
| T1 | 版本比较:同一代际先写 games=5 再写 games=3 | 第二次返回 0,ZSET 分数仍是 games=5 那次的 | ⚠ 若测试只写一次或两次分数相同,"没被覆盖"无法区分;两次必须用**不同分数** |
| T2 | 重放同一事件 | 返回 0,`rank_write_total{outcome="stale"}` +1 | — |
| T3 | 同分决胜 | 两人 rating 相同、updated 早的排前 | ⚠ 若两人 player_id 的字节序恰好与时间序一致,测试恒绿;必须构造**早达到者的 id 字节序更大** |
| T3′ | 分数不变的再写一局 | 玩家 A 在 t1 达到 1500.00,玩家 B 在 t2>t1 达到 1500.00;A 再写一次**同分**快照(games+1、updated_at 为 t3>t2)→ A 的名次**不下降**(仍在 B 前),`lb:{rating}:v` 里 A 的 games 已更新 | ⚠ 只写一次测不出;必须有第三方 B 的时间夹在中间,否则名次不变也可能是巧合 |
| T4 | score 编码边界 | rating_centi = 2^21−1 且 sec = EPOCH 时解码回原值;2^21 被拒,`out_of_range` +1 | ⚠ 只测小数值时 float64 永远精确;必须测上界 |
| T4b | rating_centi 精确解析(match 侧) | `"0.29"`→29、`"1516.29"`→151629、`"1523.45"`→152345、`"0.15"`→15、`"1500.00"`→150000;非法串 → error、不发布 | ⚠ 用 1500.00 这类整数值永远测不出截断;必须用 x.29 / x.15 这类二进制不可精确表示的小数 |
| T5 | 分页越界 | **复用 `rank_page_test.go` 的溢出输入**(page=214748366/size=20、page=size=MaxUint32),**期望值按 §4.2 重写**:page=0 返回第 1 页;超过末页与溢出起点都返回末页,且响应 `page` 等于钳制后的值;空榜 page_count=1 | ⚠ 照抄公会测试的期望值(空页)会逼实现违反 §4.2 |
| T6 | self 未上榜 | `self.rank == 0`,`error_message` 为空,entries 正常 | ⚠ 若测试数据集里"自己"本来就在第一页,self 永远上榜;必须有一个**确定不在榜**的会话 |
| T7 | 无会话元数据 | 拦截器**放行** → `GetRankPage` 回 in-band `error_message.id == kInvalidParameter`,entries 为空,**不查 Redis**(Redis 替身调用计数断言为 0) | ⚠ 只断言错误码不断言"没查 Redis",先查后拒的实现也会绿 |
| T7b | 会话头无法解码 | gRPC `codes.Unauthenticated`,handler 未被调用(与 friend `session_test` 同口径) | — |
| T8 | 名字补全失败 | entries 照返回、name 为空、指标 +1 | ⚠ 若 mock 从不返回错误,恒绿;必须注入失败 |
| T9 | 键前缀 | 表驱动枚举 data 包**全部**键构造函数(另有一条断言"表长 == 构造函数个数",加新构造函数不进表就红),对每个结果断言 `HasPrefix(k, "lb:")`、`!HasPrefix(k, "guild_rank")`、`!HasPrefix(k, "match:")`、含 `{rating}` hash tag。**Codex 额外做一次变异验证**:临时把根前缀改成 `guild_rank:x` 跑一次,必须变红,再改回 | ⚠ 不做变异验证,无法证明断言真的在查前缀。**不要**尝试在 go/rank 里跑 guild `RebuildRanks`:`go/guild` 是独立 module 且在 `internal/` 下不可导入,它的数据层测试无 `GUILD_TEST_MYSQL_DSN` 一律 SKIP,而它只 SCAN `guild_rank:zone:*`,提供不了前缀断言之外的信息 |
| T10 | match 发布失败 + 续写重发 | 替身第一次返回 error → `ApplyBattleResult` 返回 error、标记仍 `applying`;替身改为成功后重试 → 每人收到快照,且:**games > 0 且等于 `HGET match:rating:{pid} games`**;**updated_at_ms 等于首次写入时的 `HGET updated_at_ms`**(不是重试时刻);rating_gen 等于 `HGET rating_gen` | ⚠ 只测"写成功就发布"时,`written=0` 路径的 games 字符串 → 0、时间戳取 now 两个 bug 都测不出;替身时钟必须让重试时刻与首次写入时刻不同 |
| T11 | Redis 集成测试 | 实际执行且 PASS | ⚠ **SKIP 不等于 PASS**:无 Redis 时集成测试会 SKIP,Codex 报告必须列出 SKIP 数且为 0 才算过 |
| T12 | 代际重置(rank 侧) | 先写 (gen=G1, games=50, 分 X);再写 (gen=G2>G1, games=1, 分 Y≠X) → 返回 gen_reset、ZSET 分数为 Y、`gen_reset` +1;再来一条 (gen=G1, games=51) → stale | ⚠ 若 G2 事件的 games 也 >50,没有代际也会写进,测不出;新代际的 games 必须**小于**旧值 |
| T13 | rating_gen 建立(match 侧) | 首次写入后 `HGET rating_gen` 非空;第二局不变;`DEL` 该键后再写一局,新 rating_gen **大于**旧值(替身时钟推进);存量无 rating_gen 的 hash 写一局后补上 | — |

### 8.2 冒烟(R2b,robot `rank-smoke`)

**R 系列只在本地、路由服模式下验收**;K8s 默认 `GateRouterMode="0"`,rank 在 K8s 上对客户端不可达(§5.3),不能拿 K8s 当验收环境。

**双 zone 栈怎么起**:`start_game.ps1` 固定 `-Zone 1`,**不能**用它起双 zone。用 `$env:GATE_CLIENT_RPC_ROUTER = '1'` 后 `pwsh tools/scripts/dev_tools.ps1 -Command dev-start-zones -Zones 1,2`(每个 zone 经 `go_services.ps1 -Zone <N>` 派生 yaml / 端口,gate 从父 shell 继承路由模式)。`dev-start-zones` 不起 Java 网关,网关与基础设施按既有 friend 双 zone 冒烟的步骤起(确切步骤见 §9.2#5)。本地开关按 §5.4 改 `match_service.yaml` 为 `true`,跑完改回、不提交。

| # | 步骤 | 怎么算过 | 假绿陷阱 |
|---|---|---|---|
| R1 | 起双 zone 路由服模式栈,开关 `true`,让 ≥4 个账号打 ≥2 局 1V1 | `lb:{rating}:z` 的 ZCARD ≥ 4 | ⚠ **先断言榜非空**再做后续断言;空榜上"排序正确""无重复"全部天然成立 |
| R2 | zone 2 的账号拉一页 | 页里**包含 zone 1 账号的条目**,且该条目 `name` 非空、等于 zone 1 登录时的名字;冒烟前后 `rank_name_lookup_total{outcome="error"}` 没有增长 | ⚠ 只断言"两边看到同一张榜"恒绿:本地 RankRedis 回落共享库,两个 zone 的 rank 读的是同一个 ZSET。真正会跨 zone 出错的是名字补全(zone 2 的 rank 连 `dataservice.rpc.z2` 查 zone 1 玩家的名字),必须断言它 |
| R3 | 胜者名次高于负者 | 一局 1V1 后胜者 rating 高、名次在前 | ⚠ 三账号数据集下"某人不在前 2"的排除断言可能恒成立(friend 的教训);断言要写成**具体名次等于期望值** |
| R4 | self | 未打过排位的账号 `self.rank == 0`;打过的 `self.rank` 与在页中的位置一致 | — |
| R5 | 限频 | 1 秒内连发 6 次,第 6 次被 gate 拒 | ⚠ MessageLimiter 未填时默认 3 次也会拒,要确认拒绝发生在**第 6 次**而不是第 4 次 |
| R6 | 杀一个 rank 实例 | 另一实例接管消费,榜继续更新;路由服把读请求转给存活实例 | — |
| R7 | 端口 | 双 zone 下 rank 监听 51100 / 52100、指标 9250 / 10250(或 `Resolve-BindablePort` 上挪后的值,日志可见) | — |
| R8 | 开关 `false` | 选一个**已上榜**账号,记录 `HGET lb:{rating}:v <pid>` 与 `HGET match:rating:<pid> games`;开关翻 `false` 重启 match 后让它打一局;断言前者**不变**、后者 **+1** | ⚠ 断言 ZCARD 不变恒绿:R1 之后账号都已上榜,开关失效照样写榜时 ZCARD 也不变。必须比较**已上榜账号的版本值**,且前后 games 不同才能区分 |
| R9 | offset 重放恢复 | 记录 ZCARD = N;`DEL lb:{rating}:z lb:{rating}:v`;停 rank → 按 §3.5 重置 `rank-rating` 到 earliest → 起 rank;ZCARD 恢复到 N,且抽查一个账号的分数与 `match:rating` 一致 | ⚠ 若重置前没停消费者,重置命令会失败,但榜也可能被"下一局"慢慢补回看似成功;断言要在**不打新局**的前提下做 |

### 8.3 失败时保留

rank 与 match 的 stdout / stderr 摘要、`:9250/metrics` 与 `:9170/metrics` 快照、`lb:{rating}:v` 的 HLEN、Kafka 消费组 `rank-rating` 的 lag(`kafka-consumer-groups.sh --describe --group rank-rating`)。

---

## 9. 未核实项 / 拍板记录

### 9.1 拍板记录(2026-09-20,用户逐项确认,七项全部定稿)

| # | 问题 | 结论 | 连带影响 |
|---|---|---|---|
| 1 | §2 写分来源 (L-1) | **(a) match 发评分快照事件**。附属三项一并按推荐:发布开关 v1 默认 `false`;v1 不做 outbox;match 评分 hash 新增 `rating_gen` 代际字段(`HSETNX`,随快照下发) | R1 要改 `go/match`(约 10–11 个手写文件,§7);match 那批组队代码从未编译,R1 开工前先编译通过一次 |
| 2 | Redis 丢失后的恢复口径 (L-6) | **接受**:offset 重放恢复 7 天内打过排位的玩家,其余等他们下一局增量补回 | v1 不做 match 全量导出 gRPC,D-13 的两项待定不被触发 |
| 3 | 同分规则 (L-9) | **先达到该分者在前,名次连续**(由 L-10"分数不变时保留旧时间分量"实现) | 不提供"并列第 N";T3′ 必须落地 |
| 4 | K8s 私有 Redis 落点 (L-7) | **独立实例,`maxmemory-policy=noeviction`**,不暂借 `redis-match-cluster` | R2b 的 K8s 登记要多一个 Redis 实例(manifest + ConfigMap 的 `RankRedis` 段) |
| 5 | 新 topic 参数 (L-5 / L-20) | **`match-rating-snapshot`、3 分区、保留 7 天**,照 `match-results` 对齐 | 分区数上线后不可变;真要改走"代号 +1"换代(`deploy/k8s/README.md`「改分区数 = 代号 +1,绝不原地扩分区」)。7 天即 §3.5 的自动恢复窗口 |
| 6 | 客户端改仓授权 | **授权**(R3 的排行榜 UI,以及 R1p 时往客户端 `gen_proto.ps1` 收录 `rank.proto`) | "rank 的 proto-gen 与 friend 的待生成桩同一次跑"**已不可能**:friend 那次 proto-gen 在 2026-09-20 晚已经在进行,rank 的 proto 还没写。R1p 单独跑一次,届时客户端只多出 rank 的桩 |
| 7 | K8s 开关参数形态 (L-20) | **`k8s_deploy.ps1 -MatchRatingSnapshotPublish '0'|'1'`,默认 `'0'`** | 翻开 / 回滚都用同一个参数重跑部署,不改提交进仓的 yaml |

**开工闸门(与拍板无关,仍然成立)**:R1p 起的任何一批都**排在 friend 的全量 proto-gen 与编译通过之后**(`friend-handoff-20260920.md` §4.0 / §4.5:后续新服务都排在 friend 之后)。原因有二:① R1p 要改 `proto/` 并跑一次全量 proto-gen,与正在进行的 friend 那次混在一起会让两批的生成物无法分开验收;② R1 改 `go/match`,而 match 里组队那批代码从未编译过,叠加改动前要先有一次干净的编译基线。

### 9.2 未核实(落码前要自己核)

1. **friend 为什么登记了 C++ `nodeTypeNameMap` 与 `base_deploy_config.yaml` 前缀**:friend 只走路由服,依据未核到(是 gate 发现需要,还是为东西向预留)。若 gate 路由模式下的某条路径依赖它,rank 也要登记;本文 §5.3 暂定不登记。
2. **`IsGlobalPoolNodeType` 要不要加 `RankNodeService`**:只在 C++ 走通用选择路径时有影响,v1 无 C++ 调用方,未深入。
3. **`go_services.ps1` 多实例 `PortStride`**:侦察读到默认值 1;Windows 保留区只能靠 `Resolve-BindablePort` 兜底,不作保证。
4. **消费组初始 offset**:rank 新消费组从 `FirstOffset` 起,首次上线会回放 7 天内的全部快照(按版本比较幂等,结果正确),回放量未估。开关默认 `false` 时首次上线 topic 为空,不存在这个问题。
5. **双 zone 冒烟栈的完整步骤**:`dev-start-zones` 不起 Java 网关与基础设施;friend 双 zone 冒烟用的确切命令序列本文未核,R2b 写 rank-smoke 步骤时照它补全。

(原"通用服务不可用码未核实""rank 的 Tier / 是否进 `$optionalServices`""match 组队代码未编译"三条已在回修中定稿或并入 §7 前置,见 §10。)

### 9.3 文档不一致(本轮只列,不改)

- `docs/design/jubaozhai-market.md` J-7 写"17000–19999 留给 mail/chat/rank",`Tip.xlsx` 把 rank 钉在 **21000**;`docs/design/tip-code-axis.md` 约第 213 行又把 trade 列进移植预留。**以 `Tip.xlsx` 为准**(导表器输入)。
- 契约 §7 指标端口分工表缺 `9230 trade`,落码批补 `9250 rank` 时一并补。
- `friend-handoff §4.1` 记号段 tag "6 个",工作树里已是 7 个(帮会会话未提交的 `guild_asset_op`);rank 不开 tag,不受影响,但接手 mail 的人要注意。
- `PROGRESS.md:5361` 的"rank 51000/:9250"已被本文 L-17 取代(PROGRESS 只追加,不改旧条目)。

---

## 10. 评审记录

**做法**:2026-09-20 初稿后由三个视角(correctness / ops / feasibility)各自独立评审,共提交 **28 条**发现。回修人默认发现可能是错的,逐条对着磁盘取证(`go/match/internal/logic/rating.go` 的 Lua 与 `applyRatingDelta`、`go/shared/kafkautil/topic_init.go`、`deploy/k8s/README.md` 保留期一节、`tools/scripts/start_game.ps1` / `go_services.ps1` / `k8s_deploy.ps1`、`generated/code/proto/tip/common_error_tip.proto` 与 `go/shared/generated/tip/faults.go`、`go/guild/internal/data/rank_page_test.go`、`go/friend/internal/session/session.go`、`tools/merge_zone/`、`go/match/etc/match_service.yaml` 与 `deploy/docker-compose.yml`、`deploy/k8s/scene-manager-alerts.yaml`,并按词边界重跑端口 grep),成立的就地改正文。

**结果**:28 条**全部确认**,0 条整条驳回。其中 3 组是跨视角重复(#19 的三点分别与 #1、#2、#5 相同;#8/#16/#28 相同;#11/#23 相同),去掉 4 条重复后是 **24 个独立问题**。2 条是**部分采纳**(修法有误,按取证改写):

- #4 的"最低方案②:games 小于已存值记为 `regressed` 并告警" —— 不采纳。同一玩家两局由两个 match 实例并发入账时,后写的 games 可能先发布,"games 小于已存值"在正常路径上就会出现,无法与"match 丢键回退"区分,告警会持续误报。改为采纳推荐方案 `rating_gen`,由 `gen_reset` 这一可区分的 outcome 承担告警(§3.6 / §3.7⑥);最低方案①(运维同步清榜)与③(L-11 代价栏)采纳。
- #12 的告警① `time() - max(rank_last_consume_unixtime) > 900` —— 按原式会在深夜无排位、或 match 开关为 false 时误报(rank 看不到 match 的开关)。改为以"match 15 分钟内确实发布过"为前提。告警④原式 `max_over_time(max(rank_board_size)[1h])` 是 PromQL 语法错误,改为子查询 `[1h:1m]`。

**会让接手人做错事的更正**(照初稿落码会静默出错的几条):

1. **续写路径的 games 是字符串**:`ratingApplyScript` 幂等分支 `HGET games` 返回 bulk string,照 `arr[2].(int64)` 取会得到 0,唯一的补偿手段(续写重发)静默失效。现要求 Lua 两支都返回整数 + Go 侧双类型解析、0 即报错(§2.4、T10)。
2. **updated_at_ms 必须由 Lua 同源读出**,不能用 Go 的 `now`(续写时是重试时刻)或 `finished_at_ms`(§2.4、L-2、T10)。
3. **同分规则原写法实现的是"最近一局更早者在前"**:match 每局都无条件推进 `updated_at_ms`,分数不变也推进。现由 rank 写入 Lua 在分数不变时保留旧时间分量(§3.2 第 3 步、L-10、T3′)。
4. **match 评分键丢失后 games 回退,rank 会永久拒写该玩家**:新增 `rating_gen` 代际与字典序比较 + 清榜运维规则(§2.4、§3.2、§3.5、L-11、T12/T13)。
5. **rating_centi 不能 `uint32(f*100)` 截断**,`"0.29"` 会变 28(§2.4、T4b)。
6. **保留期只由 match 声明,rank Ensure 传 `RetentionMs: -1`**,否则保留期随重启顺序来回改写(§2.4、L-20、§5.2 README 表)。
7. **start_game.ps1 只加 `$services` 不会启动 rank**,必须三行都加(§5.2 #5)。
8. **R1 与 R2a 都被同一次 proto-gen 卡住**,拆出 R1p 契约批先行(§7)。
9. **测试不能照抄**:T5 期望值与公会分页相反;T7 无会话是拦截器放行 + in-band 码,不是"handler 前被拒";T9 不能在 go/rank 里跑 guild 重建;R2 / R8 原判据结构性恒绿(§8)。
10. **K8s 上 rank 默认对客户端不可达**(`GateRouterMode="0"`),不能拿 K8s 当验收环境(§5.3、§8.2)。

其余已定稿:错误码 `kInvalidParameter`(1005,非故障)/ `kServiceUnavailable`(1003,故障)(§4.5);Tier = 1、进 `$optionalServices`(§5.2);K8s ConfigMap 形状补全(§5.2 #3);发布顺序不再依赖部署先后、开关改为 `k8s_deploy.ps1` 显式参数(§5.4);合服无关结论(§3.4);offset 重放恢复 runbook(§3.5、R9);告警文件(§3.7、L-21);事件删 `match_mode`、响应删 `server_now_ms`(§2.4、§4.2);R0 验收改为"§9.1 全部拍板";R2b 端口判据改为限定范围的精确集合;R1 / R2a 文件数按逐文件清单重估并拆分。
