# 90 跨节一致性修正清单(效力高于各节原文)

<!-- 90_consistency.md -->

# 90 跨节一致性修正清单(第 1 部分:用法、待拍板、阻断级问题)

> 2026-09-16 跨节评审。读完契约与 S1–S6 全部分部(含各节 review)后整理。**本清单对所列条目的效力高于各节原文**;未列到的内容仍以各节"最高优先级分部"为准(S4:part11>10>9>8>7>1–6)。落码时各批次先读本清单再读本节。
> 分部:本文件(待拍板 + 阻断级 X-01~X-16)、`90_consistency_part2.md`(资产表完整定义与事务规则)、`90_consistency_part3.md`(重要/次要不一致 Y-xx)、`90_consistency_part4.md`(无人认领的全局缺口 G-xx)。批次与验证见 `91_batches_and_codex*.md`。
> 严重度:**阻断** = 照原文落码会编译失败、测试必挂或资产算错;**重要** = 行为不一致或违背 AGENTS;**次要** = 文案、注释、引用。

## A. 开工前需用户 / 主设计拍板(给出推荐)

| # | 事项 | 冲突来源 | 推荐 |
|---|---|---|---|
| D1 | 默认配表行 | S2 GuildLevel 10 级(1 级 30 人,升级花费 20000…1300000)、GuildRule `1,72,3,50,300,1000,3`;S5 仍按旧 B2 稿 5 级(1 级 50 人)、GuildRule `…,600,1000,5` | 按 S2 自定的"列归属批次拍板":GuildLevel 全表取 **S2 的 10 级**;GuildRule 第 5 列归 B5 取 **600**、第 7 列归 B6 取 **3**,第 8–10 列(S6)取 `0,30,10`。最终行 `1,72,3,50,600,1000,3,0,30,10`。B5a 不再改 GuildLevel.xlsx |
| D2 | 离帮后捐献退款(S5 偏差 7) | 产品规则 | 采纳 S5(退款 + 不记资金帮贡);否则删退款分支只留提前截止 |
| D3 | 回档与帮会资产 | S4 4.39 要求 fail-closed;S5 W15、S6 C14 写"接受 + 审计" | 按 AGENTS 资产 fail-closed 采纳 S4;新增子批 **B5d**(见 91);B5d 落地前手册禁止对有帮会资产操作的 zone 批量回档 |
| D4 | 卡死行人工终结 | S4 4.38 要 guild 管理 RPC `ResolveAssetOp`;S5 §5.23 做 CLI `assetopfix` | v1 只做 CLI(少一个带签名的网络面);CLI 经 `GuildAssetStore` 实现 `assetop.ManualResolver`,写 `resolved_by/resolve_reason`;`ResolveAssetOp` RPC 不做 |
| D5 | S6 U1(当期=档期)、U2(阵亡不得奖) | S6 part11 | 按 S6 默认 |
| D6 | 写冲突统一回 tip `GuildBusyRetry`(契约外) | S2 用它;S5 用 `GuildAssetPending`;S6 回 gRPC err(客户端会被强制重连) | 采纳,见 Y-02 |
| D7 | 事务隔离级别 | S2 默认 RR 并依赖间隙锁死锁来守申请上限;S5 经济事务 RC;S6 未写 | 全部帮会写事务统一 **READ COMMITTED**;B2 申请上限改靠 `guild_player_state` 锁行(X-10)。前提 `binlog_format=ROW` |
| D8 | B4c 存盘属主围栏 | S4 K1 | B5 代码可先落;任何共享/预发环境开启帮会资产操作前必须先落 B4c |
| D9 | 契约外新增 tip 共 8 个 | S2 `GuildBusyRetry`;S4 `AssetPartialApplied`、`AssetAuthFailed`;S6 `GuildActivityJoinTooRecent`、`GuildTrialInviteExpired/Declined/Cooldown`、`GuildTrialServiceBusy` | 一次性批准,顺序见 part3 Y-07 |
| D10 | `guild.proto` 时间戳类型 | 契约 §2 要 uint64;S2 保留 int64(`GuildMember.join_time_ms/last_active_ms`、`GuildInfo.create_time_ms`) | B2s 同号改 uint64(非负 varint 线格式兼容);B2c 客户端同步类型 |

## B. 阻断级问题

**X-01 资产三表无人给出完整定义(阻断)**。S1 修订后把 `guild_asset_op`、`guild_player_op_seq`、`guild_daily_counter` 移交 B5;S5 §5.6 只写"追加 21–25 列"增量并引用"S1 的 1–20 号字段";S4 E1 引用的 `s1_storage_part2.md:32-38` 状态枚举已不存在。结果:没有任何一节给出这三张表与 `GuildAssetOpStatus/Kind`、计数器种类枚举的完整 proto。**修正**:以 `90_consistency_part2.md` §1 的完整 message 为准,由 B5a 写入 `guild_db.proto`。

**X-02 索引名错位(阻断)**。proto2mysql 的 `idx_<表>_<n>` 从 0 起(S1 §1 第 2 行)。S5 §5.6/§5.19/§5.21、§5.42 第 6 步与 S6 §6.8 写的 `idx_guild_asset_op_3` 不存在,第三组应为 **`idx_guild_asset_op_2`**,且按 S4 第二轮列序为 `(player_id,stream,stream_epoch,status,seq)`。S5 第 6 步 `SHOW CREATE TABLE` 断言照此改。S2 §8.1 的 `idx_guild_application_1/2` 应为 `_0(player_id)/_1(expire_ms)`(只在注释里,改文档)。

**X-03 S5 没吸收 S4 第二轮修订(阻断)**。S5 仍按 S4 第 5 部分旧接口写:`NewSeqTables(seq, op)` 两参、`EnsureSeqRow` 无 `nowMs`、`AllocateSeq` 返回裸 seq、`ClaimDue` 单条 UPDATE 领取、串行 Tick、无 `stream_epoch` 列、无 `Signer`、无 `StatusAppliedPartial`、无 `LedgerReader`。**修正**(全部落 B5a/B5b):按 S4 4.36–4.38 的签名改写 §5.15–§5.19、§5.25、§5.29;op 插入写 `stream_epoch = Alloc.Epoch`;Store 实现 `ListDue + Claim`(删 §5.19.1 两档 ClaimDue,"新行防饿死"改为 `ListDue` 先按 `attempts < 3` 取一批、不足再取老行,两条非锁读);`Finalize` 包 `assetop.WithTxRetry`;`FinalStatus(rpc,res,op)` 得到 `AppliedPartial` 时只做 CAS、不做对侧账、不退款;guild 启动读 `MMORPG_ASSET_OP_SECRET_GUILD` 建 `assetop.NewSigner("guild", …)`,`AssetOp.Enabled=true` 且密钥缺失/过短 → `logx.Must` 拒启。

**X-04 S5 数值与 D1 冲突(阻断)**。按 D1 改 S5:§5.10 删 GuildLevel 改列表与"630,000"估算(10 级累计 4,050,000);GuildShop 204 `required_guild_level` 回到 6;§5.39 `economy_config_test` 的越界例 `required_guild_level=6` 改 11;`TestUpgradeGuild` 升 2 级后 `max_members` 35;robot §5.40 第 1 步 `MaxMembers 30`、第 9 步 `MaxMembers 35`;B5a 清单删 `data/GuildLevel.xlsx`。

**X-05 客户端 `Accept` 重复分支(阻断,C# CS8510)**。B2c 已在 switch 表达式里加 `KGuildZoneMerging`、`KGuildRankTooLow`;S5 §5.34 表再加一次会编译失败。**修正**:B5c 只加 `KGuildFundsInsufficient` 起的 10 个;文案沿用 B2。S6 §6.37 的 `KGuildAssetPending` 同理跳过。

**X-06 `GuildClient._pendingNotice` 重复声明(阻断)**。B2 §17.2 已声明(推送提示,被踢/解散);S6 §6.37 再声明一次且语义不同。**修正**:S6 改名 `_writeNotice`,`Refresh()` 成功分支先落 `_writeNotice`;NotInGuild 分支仍按 B2 优先 `_pendingNotice`。

**X-07 `DrainQueued` 三个互斥版本(阻断:B2 测试 `ApplicationReceivedReloadsListOrBadge(false)` 会挂)**。S5 §5.34 的重写删掉了 B2 "申请列表不可见时 Refresh 刷角标"分支;S6 改签名。**最终版本**(B5c 写入,B6a-cli 追加活动分支):
```csharp
public void DrainQueued(bool applicantsVisible, bool activitiesVisible = false)
{
    if (Busy || RequiresReconnect || !_net.IsReady) return;
    if (RefreshQueued) { Refresh(); return; }
    if (ApplicantsQueued && IsOfficerOrLeader) { ApplicantsQueued = false; if (applicantsVisible) LoadApplications(); else Refresh(); return; }
    if (MyApplicationsQueued) { MyApplicationsQueued = false; LoadMyApplications(); return; }
    if (ActivitiesQueued && activitiesVisible && Info != null) { RefreshActivities(); return; }   // B6a-cli 加
    if (DonationsQueued) { DonationsQueued = false; if (Donations != null) RefreshDonations(); return; }
    if (ShopQueued) { ShopQueued = false; if (Shop != null) RefreshShop(); }
}
```
B5c 先以默认参数形式改签名,B6a-cli 只插一行,两批的 `GuildUiRoot` 调用点各改一次。

**X-08 Go 推送枚举名写错(阻断)**。S2 枚举值带前缀,生成的 Go 常量是 `pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED`;S5 §5.30 写 `pb.GuildChangeKind_FUNDS_CHANGED`、S6 正文写 `ACTIVITY_CHANGED`,均按前缀名落码。

**X-09 B1 测试写死表数量,后续批次必挂(阻断)**。S1 `TestSchemaOptionsTargetsGuildDatabase` 断言 `len(Tables)==4`,`TestDropListCoversTables` 与 `TestSchemaMigrateProducesExpectedGuildShape` 依赖手写 `guildTestDropTables`;而 B5/B6 的文件清单都没列 `guild_test.go`、`guild_repo_test.go`(B5 连 `tables.go` 也没列)。**修正在 B1**:`guildTestDropTables` 改为由 `Tables()` 经 protoreflect 读 `OptionTableName` 逆序生成再加 `schema_migrations`;`len==4` 改为 `len(opts.Tables)==len(data.Tables())`;形状测试的 `STATISTICS` 只比对 B1 自己的 4 张表。后续批次在各自集成测试里断言新表形状;**B5b 清单加 `go/guild/internal/data/tables.go`**。

**X-10 B2 申请没用 `guild_player_state`(阻断:TiDB 上上限可被突破)**。S2 §8.2 靠 RR 间隙锁/插入意向锁死锁串行化同一玩家的并发申请,与 S1 §2.1 规则 3/4 冲突。**修正**(B2s,同文件):ApplyToGuild 事务外先 `INSERT IGNORE INTO guild_player_state (player_id, updated_ms)`;事务内锁序 `guild FOR UPDATE → SELECT player_id FROM guild_player_state WHERE player_id=? FOR UPDATE → 非锁定成员检查 → 删过期 → 加锁计数 → INSERT`;ReviewApplication 通过分支与 CreateGuild 在插成员行前同样锁申请人/建帮者的 `guild_player_state`(事务外先 INSERT IGNORE)。§8.9 并发表按"同一玩家的申请/审批/建帮在状态行上串行、不再死锁"改写;`TestConcurrentApproveSameApplicant` 失败集合去掉 `ErrWriteConflict` 的期望依赖。

**X-11 NUnit 空参数方法(阻断)**。`UnsupportedActionsAreClearlyDisabled` 现有 3 个 `[TestCase]`;B5c 删 2 个,S6 再删最后一个会留下带参数无用例的方法(NotRunnable)。B6a-cli 改为**删除整个方法**。

**X-12 B2 总览测试断言被 B5 改掉(阻断)**。S5 §5.35.3 把 `GuildMyContribution` 文本改成"可用 / 累计";B2 `OverviewStatisticsUpdateOnlyFromTheGuildSnapshot` 断言 `"101"/"987"`。B5c 同步改为 `"0 / 101"`、`"0 / 987"`(fixture balance 0)。

**X-13 计数器"锁不存在的行再插入"(阻断:违反 S1 规则 3)**。S5 §5.16/§5.17 先 `SELECT … FOR UPDATE` 计数行、无行再 INSERT 并靠 1062 重试。改用 S6 §6.11.2 d 步的带上限 upsert;商店:`INSERT … VALUES(…, ?count, …) ON DUPLICATE KEY UPDATE updated_ms=IF(used_count+?c<=?lim,?now,updated_ms), used_count=IF(used_count+?c<=?lim,used_count+?c,used_count)`,首次插入前在 Go 里先判 `count > limit_count`;RowsAffected=0 → 限购。重试集合去掉 1062。

**X-14 解散事务的位置与步骤(阻断:改错文件)**。B2 已把解散实现为 `guild_manage_repo.go` 的 `DisbandGuild`;S6 §6.14 与 B6a-srv #18 写 `guild_repo.go deleteGuildFromMySQL`。最终步骤:锁 guild → 锁全部成员 → 删 `guild_application`(本帮 + I3)→ **B5** 提前截止捐献 → 删 `guild_member` → **B6a** 删 `guild_activity_progress` → **B6b** 删 `guild_trial_battle` → 删 `guild`。成员 IN 占位符上限统一 100(S5 写 500 作废)。

**X-15 部分发放没有客户端状态(阻断:视图丢信息)**。S4 新增状态 APPLIED_PARTIAL,S5 `GuildAssetOrderStatus` 只有 0–4。B5a 追加 `GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL = 5`;B5c 文案"已部分发放,客服将补偿";客户端 reason 常量补 27007、27008。

**X-16 表校验器位置与上限(阻断:两份校验 + 上限矛盾)**。以 S2 §3.3 为准:`validateGuildTables` 只 B2 写;`max_members ≤ 100`;B5 的 `asset_op_deadline_seconds ∈ [60,86400]`、`asset_op_retry_base_ms ∈ [100,60000]`、`upgrade_cost_funds ≤ 1e12` 写进 `ValidateEconomyTables`(其开头先调 `validateGuildTables`);S6 三列写进 `activity.ValidateTables`。B5b 清单删 `guild_manage_logic.go`。

<!-- 90_consistency_part2.md -->

# 90 跨节一致性修正清单(第 2 部分:资产三表完整定义、统一事务规则、查询修正)

## 1. `proto/guild/guild_db.proto` 资产相关定义(B5a 落码,取代 S5 §5.6 增量写法;修 X-01)

字段号 1–25 与 S5 §5.16 INSERT 列序逐一对应(S5 引用的 next_attempt_ms=10、payload=12、ref_id=13、ref_count=14、period_key=15、reason_tip_id=18、21–25 全部吻合),26–28 取 S4 第二轮。TiDB 三项选项照 S1 §5 写法,下文省略但必须写上。

```proto
// ---- B5a ----
enum GuildAssetOpStatus {           // 与 assetop.Status 语义一一映射,数值由本枚举定(S4 偏差 #23 裁决:1 基)
  GUILD_ASSET_OP_STATUS_UNSPECIFIED = 0;
  GUILD_ASSET_OP_STATUS_PENDING = 1;
  GUILD_ASSET_OP_STATUS_APPLIED = 2;
  GUILD_ASSET_OP_STATUS_REJECTED = 3;
  GUILD_ASSET_OP_STATUS_ABORTED = 4;
  GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL = 5;   // 只终结、不做对侧账、不自动清理,转人工补偿
}
enum GuildAssetOpKind {
  GUILD_ASSET_OP_KIND_UNSPECIFIED = 0;
  GUILD_ASSET_OP_KIND_DONATE = 1;
  GUILD_ASSET_OP_KIND_SHOP = 2;
  GUILD_ASSET_OP_KIND_ACTIVITY_REWARD = 3;     // S6 I8 断言 kind=3
  GUILD_ASSET_OP_KIND_DONATE_REFUND = 4;       // D2
}
enum GuildDailyCounterKind {
  GUILD_DAILY_COUNTER_KIND_UNSPECIFIED = 0;
  GUILD_DAILY_COUNTER_KIND_DONATE = 1;
  GUILD_DAILY_COUNTER_KIND_SHOP = 2;
  GUILD_DAILY_COUNTER_KIND_ACTIVITY = 3;       // S6 的 counter_kind=3,SQL 一律绑定本常量
}

message GuildPlayerOpSeqRecord {    // 锁序位置 5
  option(OptionTableName) = "guild_player_op_seq";
  option(OptionPrimaryKey) = "player_id,stream";
  uint64 player_id = 1;
  uint32 stream = 2;                // AssetOpStream 数值(本文件不 import asset_op.proto)
  uint64 next_seq = 3;
  uint64 updated_ms = 4;
  uint64 epoch = 5;                 // 建行毫秒,>0;库恢复手册显式抬高(S4 4.31)
}

message GuildAssetOpRecord {        // 锁序位置 6
  option(OptionTableName) = "guild_asset_op";
  option(OptionPrimaryKey) = "op_id";
  option(OptionUniqueKey) = "player_id,stream,stream_epoch,seq";   // uk_guild_asset_op
  option(OptionIndex) = "status,next_attempt_ms;guild_id,op_id;player_id,stream,stream_epoch,status,seq";
  // idx_guild_asset_op_0 = 领取/清理/回档检查;_1 = 按帮查;_2 = AllocateSeq 未决加锁读、本人待结算列表
  uint64 op_id = 1;                 // biz_tag guild_asset_op;同时作 correlation_id
  uint64 player_id = 2;
  uint32 stream = 3;
  uint64 seq = 4;
  uint64 guild_id = 5;              // 发起时绑定的帮会
  GuildAssetOpKind kind = 6;
  GuildAssetOpStatus status = 7;
  uint32 durable = 8;               // 0/1
  uint32 attempts = 9;
  uint64 next_attempt_ms = 10;      // 终态时 = 终结时刻(清理与回档检查按它判龄)
  uint64 deadline_ms = 11;          // 只有 DONATE 非 0
  bytes payload = 12;               // AssetBundle
  uint32 ref_id = 13;               // donate_id / goods_id / activity_id
  uint32 ref_count = 14;
  uint32 period_key = 15;           // 0 = 未占计数行
  uint64 contribution_delta = 16;
  uint64 funds_delta = 17;
  uint32 reason_tip_id = 18;        // 只在终结时写
  uint64 created_ms = 19;
  uint64 updated_ms = 20;
  uint64 lease_until_ms = 21;
  uint64 lease_token = 22;
  uint32 tx_type = 23;
  uint32 last_outcome = 24;
  uint32 last_reason = 25;          // 每次 Reschedule/Finalize 覆盖;兼作部分发放粘性标记
  uint64 stream_epoch = 26;
  string resolved_by = 27;          // assetopfix 写,≤64
  string resolve_reason = 28;       // ≤191
  // 追加区:从 29 起。
}

message GuildDailyCounterRecord {   // 锁序位置 7
  option(OptionTableName) = "guild_daily_counter";
  option(OptionPrimaryKey) = "player_id,counter_kind,ref_id,period_key";
  option(OptionIndex) = "period_key";   // idx_guild_daily_counter_0:§5.22 清理用,否则全表扫
  uint64 player_id = 1;
  GuildDailyCounterKind counter_kind = 2;
  uint32 ref_id = 3;
  uint32 period_key = 4;            // DayKey(8 位)或 WeekKey(6 位)
  uint32 used_count = 5;
  uint64 updated_ms = 6;
}
```

- `data.Tables()` 最终顺序 = 锁序:guild、guild_player_state、guild_member、guild_application、guild_player_op_seq、guild_asset_op、guild_daily_counter(B5b 追加)、guild_activity_progress(B6a)、guild_trial_battle、guild_trial_reward_owed(B6b)。
- Store 用一个 `statusToDB/statusFromDB` switch 在 `assetop.Status` 与本枚举之间映射,单测覆盖 5 个值;`assetop.NewSeqTables("guild_player_op_seq","guild_asset_op", uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING))`。
- Codex 核对 `OptionUniqueKey` 复合列是否被 proto2mysql 支持:`SHOW CREATE TABLE` 必须出现 `UNIQUE KEY uk_guild_asset_op (player_id,stream,stream_epoch,seq)`;不支持就停下报告,不得退化成单列。

## 2. 统一事务规则(取代 S1 §2.1 规则 5、S2 §6.2 重试部分、S5 §5.15 重试与忙错误、S6 §6.10 重试;修 D6/D7)

1. **唯一助手** `func (r *GuildRepo) inTx(ctx, op string, fn func(*sql.Tx) error) error`,B2s 写在 `guild_manage_repo.go`;经济(`EconomyRepo`)与活动(`ActivityRepo`)持有 `*GuildRepo` 并调用它。删除 S5 `withTxRetry`、S6 `withActivityTxRetry`。
2. **隔离级别**:`BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})`。B2s Codex 增查 `SELECT @@global.binlog_format` = `ROW`。
3. **重试分类** `retryableTx(err)`:MySQL 1213、TiDB 9007、哨兵 `errRetryTx`(S6 结算 e 步)→ 整事务重跑,最多 3 次,10–50ms 随机退避。1205 不重试。1062 不重试(计数器一律 upsert,X-13)。
4. **忙错误**:重试耗尽、1205、事务子预算 ctx 到期(父请求 ctx 仍有效)、COMMIT 结果不明(另打 ERROR `commit outcome unknown`)→ `ErrWriteConflict`。父请求 ctx 已取消 → 原样返回 ctx 错误。
5. **错误到 tip**:`ErrWriteConflict` → `kGuildBusyRetry`(B2/B5/B6 全部写 RPC,不回 gRPC 错误);`assetop.ErrTooManyPending` → `kGuildAssetPending`;S5 `ErrEconomyBusy` 删除,用 `ErrWriteConflict`。
6. **后台写**(Store.Finalize、Claim 之后的状态写、OwedRewardLoop、结算消费者):`assetop.WithTxRetry(ctx, db, 3, guildRetryableBackground, fn)`,分类 = 1213/1205/9007;同样 RC。
7. **锁等待**:B2s 在 DSN 强制 `innodb_lock_wait_timeout=1`,作用于整个连接池(含后台与 B1 启动期 ensureSchema;DDL 等的是元数据锁 `lock_wait_timeout`,不受影响)。
8. **子预算**:业务写事务 `txCtx = 1500ms`;Finalize 2000ms;Claim 1000ms;handler 总预算 3500ms、同步投递 2500ms(S5/S6 一致,保留)。
9. **全库锁序**(S1 §2.1 表追加第 10 行 `guild_trial_reward_owed`,B6b);规则 2 注释追加 S6 的"多玩家结算按 (player_id, 表位置) 交错"。
10. **合服闸门取 zone**:经济与活动写 RPC 一律在事务内用 `SELECT … zone_id FROM guild` 读到的值调 `FenceFunc`(S5 §5.15);S6 prelude 第 6 步的缓存 zone 闸门删去,改为事务内闸门。管理类写 RPC 保持 B2 的"请求者归属 zone"闸门。

## 3. 查询修正(纪元加入唯一键后)

| 位置 | 原写法 | 改为 |
|---|---|---|
| S5 §5.21 待结算、S6 §6.8 7a | `… status=?P ORDER BY seq LIMIT 16` | `ORDER BY stream_epoch, seq LIMIT 16`(走 idx_2 的 (player_id,stream) 前缀后过滤) |
| S5 §5.21 最近结果、S6 7b | `ORDER BY seq DESC` | `ORDER BY stream_epoch DESC, seq DESC`(沿 uk 倒序) |
| S5 §5.22 清理 op | `status IN (?A,?R,?X)` | 不变,**不含** APPLIED_PARTIAL |
| S5 §5.22 清理计数 | 无索引 | 走 `idx_guild_daily_counter_0` |
| S5/S6 SQL 字面量 | `stream=2`、`counter_kind=3`、`status=?P` 以外的数字 | 一律绑定生成常量;S5 §5.42 字面量守卫追加 `rg -n "(stream|counter_kind|kind)\s*=\s*[0-9]" go/guild/internal/data` 为 0 |
| B5d `ListAppliedAssetOpsSince` | S4 写 `updated_ms > since_ms`(无索引) | `status IN (?A,?AP) AND next_attempt_ms > ?since` 走 idx_0,再按 `guild_id` 关联 `guild.zone_id`;`since_ms` 早于 `now − TerminalRetentionDays` 时无法证明,默认拒绝回档 |

<!-- 90_consistency_part3.md -->

# 90 跨节一致性修正清单(第 3 部分:重要 / 次要不一致 Y-01 ~ Y-20)

**Y-01 操作者身份、帮会定位与自愈(重要)**。B2 §11.3 引入 `operatorGuild`(缓存读到 0 时用 MySQL 复核)与 `callerOf(ctx,0)`;S5 §5.24 用 `session.ClientPlayerID` + 纯缓存 `GetPlayerGuildID`,遇 `ErrEconomyNotMember` 调 `RefreshPlayerGuildID`;S6 §6.7 同样纯缓存。刚被批准、映射失效失败的玩家会在 30 分钟内捐献/点灯全部回"未入帮"。**修正**:S5 `economyCaller` 与 S6 `activityPrelude` 第 1、3 步改为 `callerOf` + `l.operatorGuild`;事务回"不是成员/帮会不存在"时调 `VerifyPlayerGuildID`(B2 §11.3 口径)。S6 写 RPC 同 S5 R4 不再查归属区(省 1.5s,闸门在事务内,见 part2 §2 第 10 条);`GetGuildActivities` 读路径保留 `clientZone + visibleIn`。

**Y-02 logic 依赖注入写法(重要)**。只保留 B2 的函数式 Option,`guild.go` 一次装配:
```go
guildLogic := logic.NewGuildLogic(repo, guildIDs, onlineResolver, mergeFence, homeZones,
    logic.WithNotifier(...), logic.WithApplyPushGate(repo.TryMarkApplyPush), // B2s
    logic.WithPlayerNames(names),                                             // B3b(S3 原写 SetPlayerNameResolver)
    logic.WithEconomy(logic.EconomyDeps{...}),                                // B5b
    logic.WithActivities(logic.ActivityDeps{...}))                            // B6a-srv(S6 原写 EnableActivities)
```
S6 `ActivityDeps` 删 `Notifier`(用 `l.notify`),`OpIDs` 类型改 `data.OpIDMinter`、调用 `Mint`(S6 写 `IDMinter.Next`)。S6 §6.46 前置核对命令 `EnableEconomy` 改为 `func WithEconomy`。

**Y-03 推送标签(次要)**。B2s 的 `changeKindLabel` 一次写全 13 个值(补 `funds_changed`、`level_up`、`activity_changed`、`delivery_done`);B5b 清单删 `push.go`,B6 不改它。

**Y-04 缓存失效入口(重要)**。B2 规定提交后失效一律走 `invalidateAfterCommit` 且 `op` 为固定集合。B2s 集合追加 `upgrade`、`asset_finalize`、`activity`、`trial_settle`;S6 §6.11.3、§6.29 的 `invalidateGuildCache` 改为 `invalidateAfterCommit("activity"|"trial_settle", gid)`。

**Y-05 Tip 行顺序与文案归属(重要)**。`//guild_error` 在 `GuildHomeZoneUnknown` 之后按批次追加,文案以**首个加行的批次**为准,后批只核对存在:
- B2s:GuildZoneMerging、GuildTargetNotMember、GuildCannotTargetSelf、GuildRankTooLow、GuildOfficerLimit、GuildApplicationNotFound、GuildApplicationLimit、GuildApplicationQueueFull、GuildBusyRetry。
- B5a:GuildFundsInsufficient、GuildMaxLevel、GuildDonateLimit、GuildCurrencyInsufficient、GuildAssetPending、GuildAssetRejected、GuildShopGoodsNotFound、GuildShopLevelTooLow、GuildShopLimit、GuildContributionInsufficient(S5 §5.12 表里前两行不改 B2 文案)。
- B6a-srv:GuildActivityNotOpen、GuildActivityAlreadyClaimed、GuildActivityThresholdNotReached、GuildTrialTeamInvalid、GuildActivityLevelTooLow、GuildActivityJoinTooRecent、GuildTrialInviteExpired、GuildTrialInviteDeclined、GuildTrialInviteCooldown、GuildTrialServiceBusy(fault=1)。
- B3a-1:`//login_error` 尾部 RoleNameInvalid、RoleNameTaken、RoleNameSensitive。B4a-1:新组 `//asset_error base=27000` 9 行(S4 4.3.6)。
每批 Codex 核对 `tip_enum_ids.json` 只增不改,且 `go/shared/generated/tip/segments.go` 中 guild_error 组宽度容纳全部 29 个新码。

**Y-06 重投循环参数(重要)**。S5 §5.29 的 Batch 10 / Lease 30s / 串行 Tick 与 `LeaseMs ≥ Batch×OpBudget+5000` 建立在 S4 旧版上。按 S4 4.37:`AssetOpConf` 字段与默认 `ReconcileIntervalMs 2000、ReconcileBatch 100、Workers 8、LeaseMs 10000、OpBudgetMs 2500、MaxBackoffMs 60000、PoisonDelayMs 3600000、LedgerReadMinAttempts 3`;Validate 与 `NewLoop` 相同(`1≤Workers≤64`、`Batch≥Workers`、`OpBudget+2000≤Lease`、`Base≤Max`),删串行乘积约束;S5 W4、§5.39 config_test 用例、§5.43 偏差 12 同步改。`Loop.Ledger` 在 B5b 置 nil,B5d 接 data_service 实现。

**Y-07 B3b 漏了申请视图的名字(重要)**。B2 的 `ListGuildApplications`(`GuildApplicantView.name`)与 `ListMyGuildApplications`(`GuildApplicationView.leader_name`)在 `guild_manage_logic.go`;S3 §3.21 说要复用 resolver,但 B3b 清单没有该文件。B3b 清单 +1(共 20)。

**Y-08 帮会成员页文案与 B2c 衔接(次要)**。B2c 已把标签改"按编号查找"(316,3,230,58)并在成员行、申请行内联了"道友 · id"兜底;B3b 用 `GuildWindow.MemberDisplayName` 替换这两处内联,标签改"按名字或编号查找"(字号不变,宽 230 放不下时改 26 号),输入框占位"输入名字或编号"。S6 `GuildClient.MemberDisplayName(ulong)` 内部改调该静态方法,避免两份兜底规则。

**Y-09 robot 账号与清理(次要)**。账号登记:B2c 9211–9213、B5c 9214–9215、B6a-cli 9216–9219、B4b 资产冒烟 9501(契约 §6 补一行"95xx 归资产通道冒烟")。S3 §3.20 端到端清理的 `DEL account:*` 追加 9211–9219、9501;S3 §3.21 `member-names` 失败文案追加 9211–9219。S5 默认值已是 9214/9215(S6 的提醒已满足)。

**Y-10 robot vendor 规则冲突(重要)**。S3/S2 要求 `go mod vendor`,S4/S5 禁止擅自 vendor。统一:改了 robot 会 import 的 proto 或 shared 包的批次(B2s、B3a-1、B3a-2、B5a、B6a-srv、B6b-srv1)执行 `go mod vendor`,随后 `git status --short robot/vendor` 只允许出现本批 proto/shared 生成路径,出现他人路径即停;B4b(robot 不 import scenenode/assetop)不 vendor,编译报 inconsistent vendoring 就停。

**Y-11 客户端文件交叉(次要)**。`PetClient.cs`、`AttributeClient.cs` 被 B4a-client(27000–27008 镜像)与 B5c(金币→银两)先后修改,按批次串行;S5 `GuildClient` 私有资产码常量补 27007(部分发放)、27008(校验失败);S5 说"不引入 LINQ",S6 §6.37 用了 `FirstOrDefault/Distinct/Select`,B6 改为 for 循环,保持同文件风格一致。

**Y-12 部署重启顺序(重要)**。B5a 缺"新消息号 → 路由服 → gate(重载 MessageLimiter)→ guild"的重启顺序(S2 §15.4、S6 §6.46 有)。所有新增客户端消息号的批次(B2s、B5a、B6a-srv)统一执行该顺序。

**Y-13 回档口径(重要)**。按 D3 改 S4 C10 已是 fail-closed;S5 W15、S6 C14 删除"接受 + 审计",改为"见 B5d:回档前查帮会已应用资产操作,默认拒绝"。

**Y-14 data_service 同文件多批修改(重要)**。B3a-1(`config.go`、`etc/data_service.yaml`、`svc/servicecontext.go`、server、`error_codes.go`)、B5a(`config.go`、`id_segment_store.go`、`data_service.yaml`、`k8s_deploy.ps1`)、B5d(proto、server、`rollback_logic.go`、调 guild 的客户端配置)串行;错误码一律"落码时末码 +1",不写死 25/26。聚宝斋会话也在 data_service 附近(`TransferPlayer` 需带 name,S3 偏差 13),动手前 `git status`。

**Y-15 guild 集成测试库名(次要)**。B1 测试助手只许 `guild_test` 与 `guild_it_<pid>_<n>`。S5 `economy_repo_test.go` 用 `GUILD_TEST_MYSQL_DSN`(库 guild_test);S6 `activity_repo_integration_test.go` 用 `GUILD_IT_MYSQL_DSN`(root,需自建 `guild_it_<pid>_<n>` 再调助手)。两节照此写 DSN,不得指向 `mmorpg_guild`。

**Y-16 S5 前置核对命令失效(次要)**。§5.2:`rg "last_reason = 25"`(B1 不建该表,必然不命中)删掉;`rg "StatusPending +Status = 1"`(S4 第二轮已改语义枚举)改为 `rg -n "StatusAppliedPartial|func NewSeqTables" go/shared/assetop`。

**Y-17 文档旧引用(次要,合并 `guild-phase2.md` 时清理)**。S2 part1 "s1 第 5 部分偏差 3" → S1 part7 §21 #7;S4 E1/4.37 引用 `s1_storage_part2.md:32-38` 的状态枚举(已不存在)→ 本清单 part2 §1;S5 §5.2 "B1 建 8 张表" → 4 张;S5 §5.6 "沿用 S1 状态枚举"→ part2;S6 §6.10 "B2 withTxRetry 只重试 1213 一次"与 §6.49 #22 → part2 §2;S1 §2.1 规则 5 文本 → part2 §2。

**Y-18 文件计数口径(次要)**。S4 B4b 把 3 个文档计入 25,其它节文档不计。统一不计文档:B4b 代码 22。

**Y-19 契约 §3.1 字段补登(次要)**。汇总到 `guild-phase2.md` 协议节:`SetAnnouncementResponse.guild=2`(S2)、`GetGuildDonateOptions` RPC 与 `recent_*`、`max_buy_count`、`UpgradeGuildRequest.expected_level`(S5)、`RespondGuildTrialInvite`、`GuildTrialLobbyView` 等(S6)、`GuildAssetOrderStatus.APPLIED_PARTIAL=5`(X-15)。

**Y-20 S6 `clientFoundRows` 断言与 B2 DSN 改写(次要)**。`config.Validate` 断言 DSN 不含 `clientFoundRows=true`(B6a-srv);B2s 的 `WithLockWaitTimeout` 同时删除该参数(防御,B2s 同文件),两处互不矛盾。

<!-- 90_consistency_part4.md -->

# 90 跨节一致性修正清单(第 4 部分:无人认领或认领不全的全局事项 G-01 ~ G-11)

## G-01 MessageLimiter 行(`data/MessageLimiter.xlsx`,列 `id,max_requests,time_window,tip_message`)

| 批次 | 读 `10,1,1000` | 写 `5,1,1000` | 备注 |
|---|---|---|---|
| B2c | ListMyGuildApplications、ListGuildApplications | SetGuildMemberRole、KickGuildMember、TransferGuildLeader、ApplyJoinGuild、CancelGuildApplication、ReviewGuildApplication | 原 19 号(JoinGuild)按 S2 §28 B2c 第 1 步处理;NotifyGuildChanged 不加 |
| B5a | GetGuildDonateOptions、GetGuildShop | DonateToGuild、UpgradeGuild、BuyGuildShopGoods | 按 proto-gen 实际号填,填后重导表 |
| B6a-srv | GetGuildActivities | LightGuildLantern、ClaimGuildReunion、StartGuildTrial、RespondGuildTrialInvite | 同上 |
| 不加行 | DataService 三个名字 RPC(B3a-1)、SceneNodeGrpc 三个资产 RPC(B4a-1)、MatchInternal(B6b-srv1)、B5d 两个内部 RPC | — | Codex 核对路由表 `ClientProtocol: false` |

## G-02 客户端白名单与生成清单

- `tools/gen_messageids.ps1` 帮会段:B2c 删 JoinGuild、加 9 项(含 NotifyGuildChanged,只收不发);B5c 加 5 项;B6a-cli 加 5 项。其它批次不改。
- `GuildClient` 只在 B2c 注册 `NotifyGuildChanged` 一次。
- `tools/gen_proto.ps1`:只有 B3b 加 `generated/code/proto/tip/login_error_tip.proto`;`guild_db.proto`、`asset_op.proto`、`asset_op_ledger_comp.proto`、`match_internal.proto`、`guild_internal.proto`(B5d)永不进客户端。
- 每个客户端批次 Codex 断言:`rg -n "AssetDebit|MatchInternal|ReservePlayerName|ListAppliedAssetOpsSince|GetPlayerAssetOpLedger" Assets/Scripts/Net/MessageIds.cs` 为 0。

## G-03 `player_database` 加列与迁移

- 字段号按 S3 规则"落码时下一个空闲号、声明在末尾"。按 91 的批次顺序:B3a-1 `profile_component = 15`,B4a-1 `asset_op_ledger = 16`;S4 4.3.4 原文"= 15"作废。
- 两批各自在生成物落地后、任何新 go/db / scene 启动前,对每份 zone 的 db 配置跑 `cmd/migrate plan → up → SHOW COLUMNS`(S3 §3.15a、S4 4.42 第 1b 步);若两批同一天连续落地,可合并为一次迁移,但 plan 必须只含这一或两条 ADD COLUMN。
- B3a-2 的 merge_zone"按列名拷贝"必须先于任何带新列的合服演练。

## G-04 本地启动脚本与运行环境

| 项 | 归属 | 状态 |
|---|---|---|
| `start_game.ps1` 预检 `mmorpg_guild` | B1 | 已认领 |
| `cpp_nodes.ps1` 给 scene 注入 `MMORPG_ALLOW_CLIENT_GM=1` 与两把开发资产密钥,`-NoClientGm` | B4a-1 | 已认领 |
| `dev_mprocs_proc.ps1` 同上 | B4a-2 | 已认领 |
| **`go_services.ps1` 给 guild 注入 `MMORPG_ASSET_OP_SECRET_GUILD` 开发值**(未设时 `change-me-dev-asset-op-guild-secret-000000`,与 scene 一致) | **B5b(S4 指派,S5 漏列)** | 新增 1 个文件 |
| data_service `BootstrapTags` 加 `guild_asset_op`(yaml、config、id_segment_store、k8s_deploy.ps1) | B5a | 已认领 |
| `login.yaml` `AccountLockTTL: 20` | B3a-2 | 已认领 |
| Kafka `match-results` 消费组 `guild-trial`(yaml 不加引号) | B6b-srv2 | 已认领 |
| **Redis 同源核对**:guild `PlayerLocatorRedis` 必须与 scene 写 `player:{id}:location` 的实例相同(critic 未决项);battle zone Redis 与它相同 | 前者 **B5b Codex 新增**;后者 B6b-srv1 | 前者新增 |
| **`cpp/generated/table/CMakeLists.txt` 缺 ActivitySchedule**(critic 未决,S2 拒收,S6 甩给 B5) | **B2s 顺带补**(同文件,不增文件数),B2s Codex 加 Linux table 目标构建 | 新增认领 |

## G-05 K8s 上线批 BK8s(本期不落地,单独授权;各节零散"上线项"汇总)

guild:Deployment + ConfigMap(DSN 库 `mmorpg_guild`、appuser、`Schema.AutoMigrate=false`、`Timeout 4000`、`DataServiceRpc.Timeout 2000`、`AssetOp`、`MatchRpc`、`Activity`、`PlayerLocatorRedis` 指 SharedRedis)、`guild-migrate` Job(照 trade-migrate);密钥 `MMORPG_ASSET_OP_SECRET_GUILD/_TRADE`(`Resolve-InjectedSecret -MinLength 32`,注入 guild、trade、scene);NetworkPolicy(scene gRPC 只放 scene_manager/match/guild/trade;match gRPC 只放路由服与 guild);db migrate Job 先于 db/scene 滚动(两条新列);data-service ConfigMap 的 BootstrapTags;mysql-init ConfigMap 含 `mmorpg_guild`;TiDB BR 按库恢复清单加 `mmorpg_guild`;scene 永不设 `MMORPG_ALLOW_CLIENT_GM`;告警规则上线(G-07)。前置:B4c 已落地(D8)。

## G-06 proto-gen 容量

累计新增服务方法约 27 个(B2 +9−1、B3a 3、B4a 3、B5a 5、B5d 2、B6a 5、B6b 1),加上聚宝斋、组队会话的新增。每次 proto-gen 记录生成的 `kMaxRpcMethodCount`;生成器报错就停下报告,不手改上限(S2 §5.6)。

## G-07 文档落点(文档不计手改名额,随所属批次提交)

| 文档 | 批次与内容 |
|---|---|
| `docs/design/guild-phase2.md` | **B1 新建**:契约摘要 + 本清单 D1–D10 裁决 + §S1;之后每批追加本节正文(清理 Y-17 旧引用) |
| 同上"运维"节 | B5b:assetopfix、库恢复/抬纪元手册(S4 4.31)、告警 `assetop_unknown_total`、`assetop_pending_oldest_age_seconds`、`assetop_partial_total`、`guild_asset_refund_blocked_total`、`guild_cache_invalidate_failed_total`;B5d:回档拒绝与 `accept_guild_divergence`;B6b-srv2:毒消息、EXPIRED、owed 积压、`guild_trial_result_overdue`、`battle_activity_result_undelivered/not_durable`;B4c:`save outran reconnect lease`;B3a-2:名字孤儿 `login_create_player_name_orphan_total` |
| `xuanming-port-decisions-20260910.md` D-14 | B1(§5/§7/§8,S1 §20);B5a 补 §9 biz_tag `guild_asset_op` |
| `guild-zone-client-access.md` | B1 存储段;B2c §5 客户端写 RPC 清单(删 JoinGuild) |
| `jubaozhai-market.md` §6.4 | B4b(先 diff 交易会话未提交内容);§6.3 `TransferPlayer` 带 name 由交易会话合入 |
| `team-system.md` §G.2 名字来源、§E.1 与 MatchInternal 关系 | 组队会话合入(S3 #15、S6 §6.47),本期只发备注 |
| `docs/ops/merge-zone-runbook.md` | B1b(`-guild-schema`)、B3a-2(§4.4 名字、`profile_component` 前置检查) |
| `deploy/k8s/README.md:351`、`friend-persistence-architecture.md`、`guild_friend_service_notes*.md` | B1 之后的文档批(S1 §17.3) |
| `mmorpg-client/Docs/GuildUI.md` | B2c;**B5c、B6a-cli、B6b-cli 各补捐献/商店、活动、历练段(S5/S6 漏列)** |
| `data/AGENTS.md` 表索引 | 导表后 `gen_schema_index.py` 生成(B2s、B3a-1、B5a、B6a-srv) |
| `PROGRESS.md` | 每批在文件末尾追加一条,不改他人条目 |

## G-08 与并行会话的协调点

| 资源 | 冲突方 | 规则 |
|---|---|---|
| `go/schemamigrate` 的 proto2mysql replace(未提交) | 聚宝斋 | **B1 硬前置**:请用户让交易会话先提交,或打 proto2mysql v0.1.2 tag |
| `proto/message_id.txt` 与各服务生成号(196–200 未提交) | 聚宝斋、组队 | proto-gen 前确认对方号已提交;生成后 diff 只允许本批新增 |
| `Tip.xlsx`、`MessageLimiter.xlsx`(二进制无法合并) | 聚宝斋 | 任一时刻只允许一方有未提交修改;本批提交后才交给下一方 |
| `transaction_log.proto` TransactionType | 聚宝斋 | 只在末尾追加,代码引用生成常量;S4 的 24–26、S5 的 27 以生成结果为准 |
| `cpp/generated/table` 工程文件、`scene.vcxproj`、`player_database_loader.cpp` | 多方未提交 | 只合并;每次 proto-gen 后核对 `scene_node_service.cpp` 仍含 `AcquireCreatePermitBlocking`(所有批次,不止 B4a/B5a) |
| `go/guild/internal/svc/servicecontext.go`、`gen_messageids.ps1`、`robot/vendor` | 多方 | 只改本节段落;vendor 规则见 Y-10 |
| `battle_data.proto`、`gather.go` | 属性、组队 | B6b-srv1 只在末尾追加 / 只加包装函数 |
| MSBuild | 所有会话 | 同一时刻一个构建方,`/m:1` |

## G-09 EditMode 用例数基线(`-testFilter "GuildClientTests;GuildWindowTests"`)

B2c = 65(S2);B3b +2 = 67;B5c +9(client)+5(window)−2(Unsupported 删两例)= 79;B6a-cli 估 +21(删 Unsupported 方法 −1)= 100;B6b-cli 估 +5 = 105。B6 两批须在交付说明里给出精确增量;Codex 以"上一批实测总数 + 本批声明增量"核对,且 0 失败。Role 程序集(B3b)单独统计。

## G-10 B5d 内部 RPC 的放置(D3 采纳时)

新文件 `proto/guild/guild_internal.proto`:`service GuildInternal { rpc ListAppliedAssetOpsSince(...) }`,不标客户端协议选项(照 `trade_admin.proto`);guild 会话拦截器对带会话 metadata 的 `GuildInternal.*` 回 `PermissionDenied`;data_service 新增 `GetPlayerAssetOpLedger` 放 `DataService`(内部)。两者占消息号,不进 MessageLimiter、不进客户端白名单。

## G-11 字段与视图契约回写

主设计把 part2 §1、X-15、Y-19 与各节偏差汇总回写到 `guild-phase2.md` 协议节,作为落码时唯一字段表;`00_contract.md` 不再修改(已被各节偏差与本清单覆盖)。
