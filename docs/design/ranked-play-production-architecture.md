# 《论剑 / 人宠会武》生产级强一致性与水平扩展架构设计

> 本文记录《论剑 / 人宠会武》（V1–V7）在 **数据强一致性、无死锁锁序、多副本水平扩展、Redis Cluster 分片防热点、大事务拆分与真注销合规** 上的核心工程设计、根因分析与落地实现，作为长期维护与 Java 双版对齐的架构事实源。

## 1. 总体拓扑与无状态水平扩展原则

`go/match` 与 C++ `battle` 节点均为**无状态多副本水平扩展架构**（按 CPU / 房间负载扩缩容，禁止任何单机权威状态）：

| 层级 | 权威/协调介质 | 水平扩展与防热点策略 |
|---|---|---|
| 排位赛季、积分、宿敌、名局、换季状态机 | TiDB / MySQL（`mmorpg_match` 权威库） | 全副本直连权威库，状态迁移由行锁 + CAS 保护；后台任务按 etcd 成员表 HRW（`OwnsWork`）分工 |
| 排行榜投递（`match_rank_source` / `match_rank_outbox`） | SQL 64 分片发件箱 | 按 `battle_id % 64` 拆成 64 片，多 `match` 副本按片并发投递，彻底消除单游标写热点 |
| 高段排位实时直播索引（V7 擂台直播） | Match 私有 Redis Cluster（16 桶分片） | 独立 16 桶 ZSET `spectate:ranked:active:{srk:0..15}`，hash tag 分散到 16 个不同 slot + 单副本 1s `SingleFlight` 读缓存 |
| 问道名局 Top 5 热门流派读路径（V6） | 单副本 5s `SingleFlight` 缓存 + 只读快照事务 | 将万级并发大厅读压成每副本每 5 秒最多 1 次 TiDB 查询（`N/5 QPS`），且按 `season_id` 自动失效 |
| 赛季换季 8 阶段状态机 | SQL 状态机 + 200 人/批可中断续跑 | 摆脱 5s 投榜预算与单事务体积上限；所有外部 RPC 严禁放入 SQL 事务内 |

---

## 2. 问题诊断与核心工程方案（非显而易见的生产级解法）

### 2.1 排位结算事务 AB-BA 死锁根治与确定性锁序总表
- **问题分析**：早期实现在 `ApplyRankedRating` 的 `for _, pid := range members` 玩家循环内部直接对 `match_ranked_featured_build(season_id, build_key)` 执行 `SELECT ... FOR UPDATE`。高分段版本答案高度集中，当对局 1（玩家 100 用流派 B、玩家 200 用流派 A）与对局 2（玩家 300 用流派 A、玩家 400 用流派 B）并发结算时，两笔事务会以相反顺序加锁 `build_key` 导致必然 **AB-BA 死锁**。
- **解决办法（内存预聚合 + 全表严格偏序加锁）**：
  1. 玩家循环内仅在内存中通过 `collectFeaturedBuildDelta` 将同局相同 `build_key` 的出场数、胜场、高段胜场与最高分胜者（同分按最小 `player_id` 决胜）合并为一条 `featuredBuildMatchDelta`；
  2. 玩家循环全部结束后，将所有 `build_key` 按字典序升序排序（`sort.Strings`），再逐行 `FOR UPDATE` 更新 `match_ranked_featured_build`；
  3. 整笔 `ApplyRankedRating` 事务形成严格无环全序锁层级：
     ```
     match_rating_applied(battle_id)
       → match_rank_source(source_epoch, mode, shard)
       → match_rating(player_id ASC, mode)
       → match_ranked_player_season(season_id, player_id ASC)
       → match_ranked_rivalry(season_id, (player_id, opponent_id) ASC)
       → match_ranked_featured_build(season_id, build_key ASC)
       → match_battle_report(battle_id ASC)
     ```
  4. 代码落点：`go/match/internal/authority/ranked.go`（`collectFeaturedBuildDelta`、`applyFeaturedBuildDeltasTx`）。

### 2.2 「问道名局」代表战报 `ref_count` 强引用护航与换季回收
- **问题分析**：玩家个人战报列表 `match_player_battle_report` 按每人最近 100 场 FIFO 淘汰（`evictBattleReportsTx`），每次挤出都会对 `match_battle_report.ref_count` 减一、归零即删正文；此外若战斗未录播或回放被合法丢弃，也不存在正文。早期 `upsertFeaturedBuildTx` 未校验正文存在且未增加 `ref_count`，导致高段选手再打满 100 场或删角后，全服玩家点击「观摩名局」一律报 `ErrBattleReportNotFound`。
- **解决办法（原子 ref_count Pin + 换季释放 + 零死锁保证）**：
  1. `applyFeaturedBuildDeltasTx` 仅在事务内确认 `SELECT 1 FROM match_battle_report WHERE battle_id=?` 命中正文时，才允许将本局晋升/替换为 `featured_battle_id`；
  2. 若 `oldBattleID != battleID`，记入 `reportRefDelta[battleID]++`、且当 `oldBattleID != 0` 时记入 `reportRefDelta[oldBattleID]--`；
  3. 在事务最末尾调用 `adjustBattleReportRefCountsTx`（`battle_report.go`），**按 `battle_id` 升序**一次性完成加引用与减引用（归零删正文）。由于 `CommitBattleOutcome` 也在事务末尾按 `battle_id` 升序碰 `match_battle_report`，两条并发链路严格无环、零死锁；
  4. **防跨季存储泄漏**：赛季换季 Stage 7→8（`rolloverOpenNextSeason`）将旧季置为 `CLOSED` 的同一事务内，调用 `releaseSeasonFeaturedReplaysTx` 清零旧季 `featured_battle_id` 并按 `battle_id` 升序释放全部护航引用，确保往季大回放不会永久堆积。

### 2.3 V7「擂台直播」高段排位抗 PVE 稀释：16 桶 Redis Cluster 独立索引 + 单飞缓存
- **问题分析**：通用观战活跃索引 `spectate:battles:active:{sba:0..15}` 按 `created_at_ms` 倒序混放全服 PVE、帮战、生死状与高段排位。上线后高频 PVE 副本几秒钟内就会把正在激战的高段排位局挤出前 20 甚至前 100，导致「擂台直播」横幅长期显示空态。如果把所有高段排位塞进单个 Redis Key，又会在 Redis Cluster 上形成单分片热点。
- **解决办法（水平扩展 16 桶分片 + 1 秒 SingleFlight + 优先配额窗）**：
  1. **16 桶分片键**：新增 `spectate:ranked:active:{srk:0..15}`（按 `battle_id % 16` 落 16 个不同 hash slot，天然满足 `clusterredis.MatchPrivateKeys` 守卫前缀 `spectate:` + hash tag，加 Redis 主节点即可线性分摊写压）；
  2. **分数与生命周期设计**：ZSET score 采用 `created_at_ms` 而非评分，与 `spectateActiveIndex` 复用完全相同的 TTL 过期分界（`cleanupExpiredSpectateIndex` 定期 16 桶 pipeline `ZREMRANGEBYSCORE` 兜底清扫），杜绝进程崩溃导致的死键永久泄漏；
  3. **读路径抗冲击**：`SpectateLobbyCache` 将 `Featured` / `Ranked` / `Active` 三路 16 桶 pipeline 合并结果统一纳入每副本 **1 秒 `SingleFlight` 缓存**，无论大厅 QPS 多高，每个 `match` 副本每秒最多回源 Redis 一次；
  4. **防稀释优先配额**：`ListWatchableBattles` 按「运营赛事局 → 高段排位直播（优先占 `min(remaining, max(limit/2, 10))` 席，按 `ranked_peak_rating_centi DESC` 排序）→ 通用活跃局去重补齐」三段组装，既保证高段排位永不被万级 PVE 稀释，又不饿死普通观战大厅。
  5. 代码落点：`go/match/internal/svc/spectate_lobby_cache.go`、`go/match/internal/logic/{spectate_index,spectate,gather,listwatchablebattleslogic}.go`。

### 2.4 论剑大厅高并发读压降：5 秒 `SingleFlight` 流派缓存 + 无写事务快读
- **问题分析**：每场论剑结算确认后客户端会自动刷新 `GetRankedProfile`，叠加大厅切页流量，此前每次调用都开读写事务 `BeginTx` 并实时跑 `match_ranked_featured_build` 的 `ORDER BY ... LIMIT 5`。
- **解决办法**：
  1. 当前赛季走无事务快读 `readCurrentSeasonFast`（仅首次空表冷启 S1 时回落写事务）；
  2. 全服 Top 5 热门流派经 `Store.featuredCache`（5 秒 TTL + `syncx.SingleFlight` + 返回深拷贝防污染）按 `season_id` 缓存，换季提交点主动 `Invalidate()`，将全服并发读压到每副本常数级 `0.2 QPS`；
  3. 玩家个人进度、权益、宿敌账本在 `sql.TxOptions{ReadOnly: true}` 只读快照事务内一次取齐，不占写锁；
  4. `CheckCandidateIndexes`（`schema.go`）在启动与迁移后强制核验 `match_formation(ranked_season_id,state,battle_id)`、`match_ranked_battle`、`match_ranked_featured_build`、`match_ranked_rivalry` 全部热路径索引，缺索引直接拒绝接流。

### 2.5 8 阶段可中断分批续跑赛季换季状态机（突破大事务与 5s 预算瓶颈）
- **问题分析**：
  1. 原 `RunRankPublisher` 传给 `AdvanceRankedSeasonRollover` 的 `rankReader` 与 `lookupName` 均为 `nil`，导致生产环境换季无法向 `rank` 核对封榜第 1 名，且 `champion_name` 恒为空串；
  2. 原 `rolloverSnapshotHonors` 在 `RunRankPublisher` 共用的 **5 秒**超时上下文内，于单个 SQL 事务中一次性更新全服数万名合格玩家，**且在持有 `match_ranked_season FOR UPDATE` 锁的事务内调用外部查名 RPC**（违反「事务内不调 RPC」不变量），线上必然触发超时回滚并 `relay.Hold()` 卡死换季。
- **解决办法**：
  1. **生产全链路接线**：`RunRankPublisher` 复用既有 gRPC 连接装配 `ClientPlayerRankClient` 作为 `RankedBoardReader`；`ServiceContext` 通过 `BindRankedNameLookup` 注入 `DataServiceClient.BatchGetPlayerName`（2 秒超时，失败降级空串不阻断换季）；
  2. **预算解耦**：换季单步给予独立 `rankRolloverStepTimeout = 20s` 超时，不与 5 秒投榜预算相互挤压；
  3. **事务外 RPC + 200 人分批幂等续跑**：Stage 6（`BOARD_SEALED`）之后旧季合格名单完全冻结不可变，因此先在**任何 SQL 事务外**读取冻结名单、向 `rank` 核对榜首并向 `data_service` 查询榜首昵称；再按 **`honorsSnapshotBatchSize = 200` 人/事务**分批提交 `match_ranked_reward`（`INSERT IGNORE` 幂等）与 `final_rank/honor_bits`（已对齐行重试直接跳过）；全部批次完成后，以一笔极短事务锁 `match_ranked_season FOR UPDATE` 原子推进至 Stage 7。任意节点崩溃或 HRW 换主后可无损续跑。
  4. 代码落点：`go/match/internal/authority/{ranked_rollover,rank_publisher}.go`、`go/match/internal/svc/servicecontext.go`。

### 2.6 删角真注销 6 张论剑表清理：利用宿敌对称性的零全表扫描方案
- **问题分析**：此前 `RoleFencePurge` 遗漏全部 6 张论剑排位表，导致已注销角色不仅残留个人数据与昵称，甚至仍保留在活跃赛季 `match_ranked_player_season` 中参与封榜排名与冠军评定。同时 `match_ranked_rivalry` 的主键与索引均以 `season_id` 打头，若直接 `UPDATE match_ranked_rivalry SET opponent_name='' WHERE opponent_id=?` 会引发全表扫描。
- **解决办法（对称主键点查，零全表扫描、零新增索引）**：
  1. 利用 `match_ranked_rivalry` 的**业务对称性**（只要存在 `(season_id, P, O)` 则必存在对端主键 `(season_id, O, P)`）：
     - 先收集被删角色参与过的赛季并集（`match_ranked_player_season` 索引 `(player_id, season_id)` ∪ 极小表 `match_ranked_season`）；
     - 逐赛季用主键前缀 `(season_id, player_id)` 查出该角色全部对手 `opponent_id`；
     - 逐对手用**精确主键** `WHERE season_id=? AND player_id=? AND opponent_id=?` 将对手镜像行中的 `opponent_name` 置空（保留对手自身胜负统计、彻底清除被删角色 PII）；
     - 再用主键前缀删掉被删角色自己的宿敌行；
  2. 同循环按 `(season_id, featured_player_id)` 清空被删角色在 `match_ranked_featured_build` 中的代表展示名；
  3. 随 `rolePurgeDeletes` 整删 `match_ranked_player_prefs`、`match_ranked_player_season`、`match_ranked_reward`、`match_ranked_battle` 个人行。
  4. 代码落点：`go/match/internal/authority/role_fence_purge.go`。

### 2.7 V7「不服再战」服务端原成局 O(1) 防伪校验
- **问题分析**：客户端发送 `ChallengePlayerRequest` 时可直接构造 `CustomDuelRules.rematch_source_battle_id`，若服务端不验真，任意篡改客户端可向全服陌生人伪造「论剑不服再战」弹窗或重放陈年旧局。
- **解决办法**：
  1. `validateCustomDuelRules` 拒绝 `rematch_source_battle_id != 0 && use_free_preset == true`；
  2. `Store.ValidateRankedRematchSource` 在创建切磋邀请前执行两次 O(1) 主键只读点查（零锁）：核对 `match_formation` 为正式自养 1V1 排位（`PVP_TEAM`、`ranked_season_id > 0`、`state != 5`、恰好 2 名敌对成员匹配 `{challenger, target}`），且 `match_battle_completion` 已落定 `COMPLETE` 且终局归档时间距今不超过 **10 分钟**（`rankedRematchMaxWindowMs`）；不合法或超期返回非故障业务拒绝 `ErrChallengeExpired`（不触发排空 `HOLD`）。

### 2.8 组队排位入队消除事务内嵌套连接池死锁
- **问题分析**：`team/sql_launch.go` 在已持有写事务 `tx` 与全员行锁的状态下调用 `s.authority.EnsureActiveSeason(ctx)`，后者内部重新 `s.DB.BeginTx` 借第二条 DB 连接。当并发量达到连接池上限（16）时，16 个协程各持 1 条连接等待第 2 条连接，触发经典连接池自死锁。
- **解决办法**：改为在既有事务内调用 `s.authority.EnsureActiveSeasonTx(ctx, tx, stamp)`，并统一版本不符返回码为 `constants.ErrRankedRuleVersionMismatch`。

### 2.9 客户端直播加载态异常收敛与版本回退统一
- **问题分析**：`RankedUiRoot.cs` 刷新 V7「擂台直播」时仅订阅 `SpectateClient.OnListUpdated`，若网络超时或限流触发 `OnError`，横幅会永久卡在「正在刷新当前高段排位激战…」；同时 `PresetSelected` 与 `SavePreset` 存在硬编码版本 `1u`/`2u` 差异。
- **解决办法**：在 `RankedUiRoot.cs` 对称绑定/解绑 `SpectateClient.OnError` 及时复位加载态并展示失败提示，并将未拉取档案前的 `ruleVersion` 回退统一为 `0u`（由服务端按当前生效规则版本判定）。

---

## 3. 生产运行参数基线（上线前标准定值）

| 参数 | 定值 | 说明 |
|---|---|---|
| `spectateIndexBuckets` | `16` | Redis Cluster 观战/高段直播分片桶数（建键后不可在线修改） |
| `SpectateLobbyCacheTTL` | `1s` | 单副本观战大厅列表 `SingleFlight` 缓存 |
| `featuredBuildsCacheTTL` | `5s` | 单副本「问道名局」Top 5 `SingleFlight` 缓存 |
| `honorsSnapshotBatchSize` | `200` | 换季荣誉快照单事务玩家批量 |
| `rankRolloverStepTimeout` | `20s` | 换季状态机单步超时（与 5s 投榜预算解耦） |
| `rankedRematchMaxTTLSeconds` | `15s` | 1V1「不服再战」邀请应答有效期 |
| `rankedRematchMaxWindowMs` | `10 min` | 1V1「不服再战」战后合法发起窗口 |
| `rankSourceShards` | `64` | 排位投榜发件箱 SQL 分片数 |
