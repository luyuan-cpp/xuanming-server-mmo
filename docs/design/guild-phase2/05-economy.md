> ## 决策覆盖(2026-09-17,效力高于本节正文与 90 清单)
>
> **D2 = 资金照记给帮会,不退款。** 本节正文按"退款"方案写,以下按用户决策作废并替换:
>
> 1. **删除退款分支**:不新增 `GUILD_ASSET_OP_KIND_DONATE_REFUND = 4`,不新增 `TX_GUILD_DONATE_REFUND`,`kAssetOpStreamRules` 的 GUILD_CREDIT 一行**不改**(B4a 的 scene 白名单与 `static_assert` 均不动)。`Finalize` 的 §5.22 表删去 DONATE_REFUND 的三行与"插退款指令"SQL(§5.22 末)、删去 §5.21 第 1 步的退款准备(`refundOpID` / `EnsureSeqRow(GUILD_CREDIT)` / `errRefundNotPrepared` / `ErrRefundBlocked` / `guild_asset_refund_blocked_total` / `guild_asset_refund_total`)。
> 2. **R5 改为**:捐献结算时**不再检查捐献者是否仍是成员**。`DONATE + APPLIED` 一律按 `op.guild_id`(请求时绑定的帮会)记资金与帮贡;若该帮会行已不存在(已解散),资金与帮贡都记不上,计 `guild_asset_orphan_total{kind="donate",what="guild_gone"}` 并 INFO,不做补偿。成员行不存在时只跳过帮贡、资金照记。
> 3. **R8 改为**:今日次数在提交 PENDING 时占用;只有 REJECTED / ABORTED 退回,APPLIED 保留(退款一档取消)。
> 4. **保留**:离帮 / 被踢 / 解散三个事务仍把该玩家未结算捐献的 `deadline_ms` 提前到"现在"(B2 的三条 UPDATE 保留),让还没扣款的指令尽快中止、次数退回;这是唯一保留的离帮处理。
> 5. **B5a 文件数 −1**(不改 `transaction_log.proto`),B5b 逻辑相应变简;§5.43 偏差 7 作废。
>
> 其余(U1 与本节无关,货币三项改名照做)不变。

> ## B4b 落地后的接口终稿(2026-09-19,效力高于本节 §5.15–§5.19 正文)
>
> `go/shared/assetop` 已由聚宝斋会话按 S4 第二轮 + 90 清单 X-03/Y-06 落码并提交(`795ea9f6a`,未编译)。
> **B5b 的 `GuildAssetStore` 按下面三条实现,正文里与之冲突的写法作废**;签名以磁盘上的
> `go/shared/assetop/reconcile.go` 为准,落码前先读一遍。
>
> 1. **`ListDue` 是两段非锁读**(X-03 防饿死,契约注释在 `reconcile.go`):
>    第一段 `attempts < assetop.FreshAttemptLimit`(**用导出常量,不许写 3**)取满 limit;
>    第二段只补缺口、第一段取满就**不发**第二条 SQL;两段都 `ORDER BY next_attempt_ms ASC, op_id ASC`;
>    按 `op_id` 去重,第一段的 id 排前面。`go/trade` 已有同形实现,可直接照抄。
>    **反向代价**(对方补充,一并接受):新行满额时老行拿不到名额(scene 故障恢复期正是这个形状),
>    靠 `assetop_pending_oldest_age_seconds` 告警兜底;要改成"给老行保底"必须先改 X-03。
> 2. **`Claim` 是六参**:`(ctx, opID, nowMs, leaseUntilMs, poisonUntilMs, token)`。
>    毒行延迟由循环算好**绝对时刻**传进来,`PoisonDelay` 不再是死配置 ——
>    **Store 实现不许再自写一份 1h 常量**(那正是两份值分叉的来源)。
> 3. **`Reschedule` 在 `RowsAffected == 0` 时必须回 `assetop.ErrLeaseLost`**(可 `%w` 包一层)。
>    不回的话,"我这一轮的结果被另一个副本丢弃了"在指标里恒为 0,是看不见的静默失败。
>
> 另外三条与 B5 直接相关:
>
> - **隔离级不用显式传**:`assetop.WithTxRetry` 的默认就是 READ COMMITTED(对方保留了这个默认,
>   理由记在 `DefaultTxRetryConfig` 注释里:会丢钱的路径不接受"语义由 DSN 决定")。与 D7 一致。
> - **签名串 canonical 扩了 `;u=;p=` 两段**(空则 `;u=;p=0`)。帮会走 GUILD_* 流,这两段恒为空,
>   **行为不受影响**;但两边 golden 字面量都改了,若日后在帮会侧写签名用例,别抄旧 golden。
> - **本机开发密钥改成随机生成**,落 `run/secrets/assetop-dev.env`。B5b 给 guild 注入
>   `MMORPG_ASSET_OP_SECRET_GUILD` 时**必须从该文件取**,90 清单 G-04 里原来写死的
>   `change-me-dev-asset-op-guild-secret-000000` 已作废 —— 写死值会被人抄进预发环境。

# S5 捐献、帮会升级与帮会商店(B5a-d)
> 本节由 9 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s5_economy_part1.md -->

# S5 捐献、帮会升级与帮会商店(批次 B5)— 第 1 部分:结论、决策、依赖、状态机

> 状态:设计稿,未落码、未编译,待 Codex 验证。遵守 `00_contract.md`;依赖 S1(B1 表)、S4(B4a/B4b 资产通道,**以 `s4_asset_part7.md` 勘误为准**)、S2(B2 管理/推送/表校验)。2026-09-16 按对抗评审修订,逐条结论见 `s5_economy_review.md`。全节共 9 部分。行号为 2026-09-15/16 核对值,并行会话在改同一工作树,落码前按函数名重新定位。

## 5.0 结论(先读这段)

- **捐献**:玩家点"捐献"→ guild 在 `mmorpg_guild` 开一个 READ COMMITTED 事务:读帮会行里的 zone_id 查合服闸门 → 锁本人成员行 → 分 seq → 写一行 PENDING(1) 资产指令(绑定当时的 guild_id)→ 占今日次数。提交后**同步**调一次 scene `AssetDebit`(预算 ≤2.5s)。拿到 `APPLIED && durable`,才在第二个事务里把指令改成 APPLIED(2),同时加帮会资金和个人帮贡。scene 判拒(`REJECTED && durable`)→ 改成 REJECTED(3) 并退回次数。其余情况(战斗中、不在线、超时)保持 PENDING,由后台重投。超过 600 秒(`GuildRule.asset_op_deadline_seconds`,B2 默认行)还没结算,就改发 `AssetAbortDebit`;结局为中止(ABORTED(4))时同样退回次数。
- **离帮保护**:离帮、被踢、解散这三种事务,会把该玩家在本帮的未结算捐献的截止时间提前到"现在",重投循环随即改发中止。万一 scene 已经扣了款(中止回 APPLIED),结算时发现捐献者已经不在该帮:**不记资金,也不记帮贡**;同一事务里插一条 GUILD_CREDIT 退款指令(kind=DONATE_REFUND),把货币原数退回,次数也退回。
- **类比**:像在银行柜台给社团转账。社团先在账本上记一笔"某某正在转 1 万,流水号 7"(PENDING);柜台(scene)扣款并盖章入库(durable)之后,社团才把钱记进公款、给你记功劳。柜台拒绝,就撤销这笔记录,次数还给你。柜台一直排不上队,10 分钟后社团去柜台声明"流水号 7 作废"。你要是中途退社,社团马上去柜台作废;柜台若说"已经扣了",社团就开一张退款单,把钱退给你。
- **升级**:只动 guild 的 MySQL。帮主或长老点升级:事务里锁帮会行 → 用行里的 zone_id 复核闸门 → 校验职位 → 按 `GuildLevel` 扣资金 → 等级 +1 → 改 `max_members`。请求带 `expected_level`,重复点击不会连升两级。
- **商店**:一个事务里扣可用帮贡、占限购、写一行 PENDING 发物指令,提交后同步调 `AssetCredit`。`APPLIED` 就算完成。背包满或战斗中时保持 PENDING("待发放"),腾出背包后自动到账,**不设超时**。scene 永久拒绝时,才退帮贡、退限购。scene 回 UNKNOWN 导致卡住的行,由运维工具人工终结(§5.23)。
- **数值**:银两 = `kCurrencyGold(0)`,灵石 = `kCurrencyDiamond(1)`。默认每天最多捐 8 次:银两小捐 5 次、大捐 2 次、灵石 1 次。一人一天最多给帮会 49,000 资金、给自己 490 帮贡;1→2 级需要 20,000 资金。成员上限和长老上限沿用 B2 的 `GuildLevel` 行,B5 只改升级花费这一列(§5.10)。
- **客户端**:捐献页和商店页沿用"3 面板 + 分页"几何;总览改成 4 列指标,再加一个"升级帮会"按钮;背包面板的"金币"改名"银两"。异步结算完成时会推送 FUNDS_CHANGED 或 DELIVERY_DONE,客户端收到后自动重拉捐献页或商店页,并重拉背包。两页都显示本人 10 分钟内的最近结果,断线重连后,玩家也能看到上一笔有没有到账。

## 5.1 用户决策与本节自定规则

| # | 规则 | 来源 |
|---|---|---|
| R1 | 银两 = kCurrencyGold,灵石 = kCurrencyDiamond;绑定灵石不能捐;"建设物资"本期保持"暂未开放" | 契约 §0-1 |
| R2 | 捐献、升级、兑换都查合服闸门,失败回 `GuildZoneMerging`。闸门用的 zone **在写事务里从 MySQL `guild.zone_id` 读**,不用缓存:缓存 TTL 30 分钟,合服后会指向旧 zone(`guild_repo.go:958-961` 注释) | 契约 §2 |
| R3 | 三个写 RPC、两个读 RPC **只接受客户端调用**(无会话回 `PermissionDenied`),身份一律取自会话 | 本节 |
| R4 | 经济类 RPC **不查** data_service 归属区。理由:S2 §8.5 的申请事务已校验 `guild.zone_id == 申请人归属 zone`,两者只会被合服一起改写;合服期间,R2 的闸门在事务内按权威 zone 拦截所有写。所以"是成员"就意味着归属区一致。省下的 1.5s 同步调用留给 scene 同步投递 | 本节 |
| R5 | 捐献结算时,若捐献者已不是绑定帮会的成员(离帮、被踢、解散):资金和帮贡**都不记**。scene 已扣款的,插 DONATE_REFUND 指令退回原额货币,并退回次数;scene 未扣款的,由截止时间提前触发中止,结局 ABORTED、退回次数 | 评审 #5 |
| R6 | 兑换 PENDING 期间离帮:物品照发。若最终被 scene 永久拒绝,只有仍是该帮成员时才退帮贡(离帮本来就会清空帮贡),限购次数照退 | 本节 |
| R7 | 捐献指令 `deadline_ms = created_ms + asset_op_deadline_seconds×1000`;商店、活动奖励、退款指令 `deadline_ms = 0`,永不中止 | 契约偏差 3 |
| R8 | 今日次数和限购在**提交 PENDING 时占用**;REJECTED、ABORTED、退款时退回,APPLIED 保留 | 任务要求 |
| R9 | 游戏日 = UTC+8 每日 05:00;周 = ISO 周,周一 05:00 切换(`go/shared/gameday`,B5 新建) | 契约 §2 |
| R10 | 帮贡分两列:`contribution_total`(累计,只增,用于展示和排名)、`contribution_balance`(可消费)。捐献两列同加,兑换只扣 balance | 契约 §3.1/§3.2 |
| R11 | 升级只能由帮主或长老发起,只花资金,不要求人数;等级只升不降 | 契约 §3.1 |
| R12 | 经济类事务(T-D/T-S/T-U/Finalize/ClaimDue/清理)一律用 READ COMMITTED,避免间隙锁死锁。锁冲突或超时时回可重试的 `GuildAssetPending`,**不回 gRPC 错误**(客户端遇传输错误会强制重连) | 评审 #2 |

## 5.2 依赖与前置(落码前逐条核对)

| 依赖 | 需要的东西 | 核对命令 |
|---|---|---|
| B1 | `mmorpg_guild` 8 张表;`GuildData.Funds`、`MemberData.ContributionTotal/ContributionBalance`;`GuildAssetOpRecord` 21–25 列与 `idx_guild_asset_op_3`(S4 E2 推荐由 B1 建全) | `rg -n "Funds\|ContributionBalance" go/guild/internal/data/guild_repo.go`;`rg -n "last_reason = 25\|lease_token = 22\|player_id,stream,status,seq" proto/guild/guild_db.proto`(缺列时按 §5.6 由 B5a 补) |
| B2 | `GuildInfo` 字段 10–15、`GuildMember` 8–9;`constants.Rank`;`GuildRule`/`GuildLevel` 表与 `validateGuildTables`;`push.go` 的 `GuildNotifier.Notify`、`l.notify`、`WithNotifier`/`Option`;`guildInfoFor`;`guild_manage_repo.go` 的 LeaveGuild/Kick/DisbandGuild;客户端 `HandleGuildChanged`/`DrainQueued` | `rg -n "func \(l \*GuildLogic\) (notify\|guildInfoFor)\|func validateGuildTables\|func WithNotifier" go/guild/internal/logic`;`rg -n "func \(r \*GuildRepo\) (LeaveGuild\|KickMember\|DisbandGuild)" go/guild/internal/data` |
| B4a | 三个 SceneNodeGrpc RPC;`TX_GUILD_DONATE/SHOP/ACTIVITY_REWARD`;`kAssetOpStreamRules`;`//asset_error` tip | `rg -n "TX_GUILD_ACTIVITY_REWARD" proto/common/rollback/transaction_log.proto`;`rg -n "kAssetOpStreamRules" cpp/libs/services/scene/player/system/player_asset_op.cpp` |
| B4b | `go/shared/scenenode`、`go/shared/assetop`(Status 按 E1 为 1..4;`Loop.ProcessOne`、`AllocateSeq`、`EnsureSeqRow`) | `rg -n "StatusPending +Status = 1" go/shared/assetop` |
| 已有 | guild 已加载全部配表(`servicecontext.go:43`);`idsegment.Minter{Name, Segment, Fallback}` + `Mint(ctx)`(`shared/idsegment/minter.go:28,45`);`PlayerLocatorRedisClient` | — |

任何一项不满足,就停下报告,不要在 B5 里补做其他批次的内容。唯一例外是 B1 缺 21–25 列:那就按 §5.6 在 B5a 里补上。

## 5.3 资产指令状态机(`guild_asset_op.status`,数值同 S1 与 `assetop.Status`)

```
                    (T-D / T-S 提交,status=1)
                             │
                             ▼
             ┌── PENDING(1) ───────────────────────────────────────────────┐
             │   scene: RETRY / NOT_HERE / 超时 / 未 durable → 退避重投(同 seq)
 Debit/Credit│   ├─ APPLIED && durable ─► APPLIED(2)
 返回        │   │     DONATE:仍是绑定帮会成员 → 加资金、加帮贡
             │   │             已不是 → 不记账 + 插 DONATE_REFUND 指令 + 退次数
             │   │     SHOP / ACTIVITY_REWARD / DONATE_REFUND:只改状态
             │   ├─ REJECTED && durable(rpc≠Abort 或 reason≠0)─► REJECTED(3)
             │   │     DONATE:退次数;SHOP:退帮贡(仍在帮时)+ 退限购;
             │   │     ACTIVITY_REWARD:只改状态;DONATE_REFUND:只改状态 + ERROR 告警
 到期(仅捐献)│   └─ now ≥ deadline_ms → 改发 AssetAbortDebit(同 seq)
             │          ├─ REJECTED && durable && reason=0 ─► ABORTED(4)[退次数]
             │          └─ APPLIED && durable(中止前已扣款)─► 走上面的 APPLIED 分支
             └──────────────────────────────────────────────────────────────┘
 离帮 / 被踢 / 解散事务:该玩家本帮 PENDING 捐献的 deadline_ms、next_attempt_ms 设为 now(不改状态)
 运维工具 assetopfix:PENDING → APPLIED(2) 或 ABORTED(4),走同一个 Finalize
```

终态只能由 `UPDATE … WHERE op_id=? AND status=?pending` 一次性写入;再终结一次时 RowsAffected=0,不会重复记账。**SQL 里不写状态字面量**,一律绑定 `uint32(assetop.StatusPending)` 等常量,下文用 `?P`、`?A`、`?R`、`?X` 分别表示 PENDING、APPLIED、REJECTED、ABORTED 的绑定参数。

**幂等**:同一个 op 永远用同一个 `(player_id, stream, seq)` 调 scene,scene 账本保证只应用一次(S4 I2);guild 侧用"status=?P 的 CAS"保证对侧账只记一次(S4 C4)。

## 5.4 各结果对客户端的呈现

| 场景 | RPC 的 `error_message` | 视图 `status` | 客户端文案(状态栏) |
|---|---|---|---|
| 捐献同步拿到 APPLIED | 无 | APPLIED | "捐献成功:帮贡 +120,帮会资金 +12,000" |
| 捐献同步拿到 PENDING | 无 | PENDING,`reason_tip_id`=最近一次暂时原因 | "捐献结算中:战斗中暂不结算,战斗结束后自动继续" 等 |
| 余额不足(durable) | `GuildCurrencyInsufficient` | REJECTED | "银两或灵石不足,无法捐献。" |
| 余额不足但尚未 durable | 无 | PENDING,reason=27000 | "余额不足,正在确认结算结果" |
| 其它永久拒绝 | `GuildAssetRejected` | REJECTED | "资产结算失败,本次操作已撤销。" |
| 兑换同步 APPLIED | 无 | APPLIED | "兑换成功,物品已放入背包" |
| 兑换时背包满 | 无 | PENDING,reason=27001 | "背包已满,腾出空间后自动发放" |
| 未决指令过多(S4 I5)/ 数据库锁冲突或超时 | `GuildAssetPending` | 无视图 | "还有未结算的帮会操作,请稍后再试。" |
| 异步结算完成(推送后自动拉取) | — | 列表里消失,出现在 `recent_*` | "捐献已入账…" / "捐献未成功:…" / "捐献超时已撤销,次数已退回" / "兑换的物品已发放" |

说明:PENDING 不是错误。`GuildClient.Accept` 遇到任何非 0 tip 都返回 false,并中断后续刷新(`GuildClient.cs:161-181`),所以 PENDING 必须用视图字段表达,不能塞进 `error_message`。

<!-- s5_economy_part2.md -->

# S5 经济(B5)— 第 2 部分:协议与表结构增量

## 5.5 `proto/guild/guild.proto`

### 5.5.1 新增枚举与视图消息(放在 B2 的视图消息之后)

```proto
// 客户端可见的资产订单状态(guild_db.proto 的 GuildAssetOpStatus 不下发客户端,另起一个;数值恰好与之相同)。
enum GuildAssetOrderStatus {
  GUILD_ASSET_ORDER_STATUS_UNSPECIFIED = 0;
  GUILD_ASSET_ORDER_STATUS_PENDING = 1;   // 结算中 / 待发放
  GUILD_ASSET_ORDER_STATUS_APPLIED = 2;
  GUILD_ASSET_ORDER_STATUS_REJECTED = 3;
  GUILD_ASSET_ORDER_STATUS_ABORTED = 4;   // 超时或人工撤销
}

// 一笔捐献(DonateToGuild 的回包;GetGuildDonateOptions 的待结算列表与最近结果)。
message GuildDonationView {
  uint64 op_id = 1;
  uint32 donate_id = 2;               // GuildDonate.id
  GuildAssetOrderStatus status = 3;
  uint32 currency_type = 4;           // 0 银两 / 1 灵石
  uint64 cost_amount = 5;
  uint64 contribution_gain = 6;
  uint64 funds_gain = 7;
  uint32 reason_tip_id = 8;           // PENDING:最近一次暂时原因(27xxx,0=刚提交);REJECTED:拒绝原因
  uint64 created_ms = 9;
}

// 捐献页的一个选项(配表行 + 本人今日用量)。
message GuildDonateOptionView {
  uint32 donate_id = 1;
  string name = 2;
  uint32 currency_type = 3;
  uint64 cost_amount = 4;
  uint64 contribution_gain = 5;
  uint64 funds_gain = 6;
  uint32 daily_limit = 7;
  uint32 used_today = 8;              // 含结算中的占用
  uint32 min_guild_level = 9;
  bool unlocked = 10;                 // 帮会等级 >= min_guild_level
}

message GuildShopGoodsView {
  uint32 goods_id = 1;
  string name = 2;
  uint32 category = 3;                // 1 修行补给 / 2 帮会珍藏 / 3 节庆好礼
  uint32 item_id = 4;
  uint32 item_count = 5;              // 每份物品数
  uint64 cost_contribution = 6;       // 每份帮贡
  uint32 required_guild_level = 7;
  bool unlocked = 8;
  uint32 limit_period = 9;            // 0 不限 / 1 日 / 2 周
  uint32 limit_count = 10;            // limit_period=0 时为 0
  uint32 used_count = 11;             // 本周期已兑换份数(含待发放)
  uint32 max_buy_count = 12;          // 单次最多份数(§5.11 规则 4 算出;堆叠 1 的物品恒为 1)
}

message GuildShopOrderView {
  uint64 op_id = 1;
  uint32 goods_id = 2;
  uint32 count = 3;                   // 份数
  GuildAssetOrderStatus status = 4;
  uint64 cost_contribution = 5;       // 本单总帮贡
  uint32 reason_tip_id = 6;
  uint64 created_ms = 7;
}
```

### 5.5.2 请求/响应消息

```proto
message GetGuildDonateOptionsRequest {}
message GetGuildDonateOptionsResponse {
  TipInfoMessage error_message = 1;
  repeated GuildDonateOptionView options = 2;         // 按 donate_id 升序
  repeated GuildDonationView pending_donations = 3;   // 本人本帮 PENDING,按 seq 升序,<=16
  uint64 contribution_total = 4;
  uint64 contribution_balance = 5;
  uint64 next_daily_reset_ms = 6;
  repeated GuildDonationView recent_results = 7;      // 本人 10 分钟内进入终态的捐献,<=5,新的在前
}

message DonateToGuildRequest { uint32 donate_id = 1; }
message DonateToGuildResponse {
  TipInfoMessage error_message = 1;
  GuildDonationView donation = 2;     // 已写入指令时必填(含 REJECTED)
  GuildInfo guild = 3;                // 请求者仍在帮时必填(含 PENDING),按 guildInfoFor 装配
}

// expected_level:客户端看到的当前等级。与库里不一致 = 已被他人升级,回成功 + 最新 GuildInfo,不再扣资金。
message UpgradeGuildRequest { uint32 expected_level = 1; }
message UpgradeGuildResponse {
  TipInfoMessage error_message = 1;
  GuildInfo guild = 2;                // 请求者仍在帮时必填(含业务失败,如资金不足),按 guildInfoFor 装配
}

message GetGuildShopRequest {}
message GetGuildShopResponse {
  TipInfoMessage error_message = 1;
  repeated GuildShopGoodsView goods = 2;              // 按 category、goods_id 升序
  uint64 contribution_balance = 3;
  repeated GuildShopOrderView pending_orders = 4;     // 本人 SHOP 类 PENDING,<=16
  uint64 next_daily_reset_ms = 5;
  uint64 next_weekly_reset_ms = 6;
  repeated GuildShopOrderView recent_orders = 7;      // 本人 10 分钟内进入终态的兑换,<=5,新的在前
}

message BuyGuildShopGoodsRequest { uint32 goods_id = 1; uint32 count = 2; }
message BuyGuildShopGoodsResponse {
  TipInfoMessage error_message = 1;
  GuildShopOrderView order = 2;
  uint64 contribution_balance = 3;                    // 提交后的余额(REJECTED 时已退回)
}
```

### 5.5.3 service 追加(`GuildService`,顺序即 message id 分配顺序)

```proto
  rpc GetGuildDonateOptions(GetGuildDonateOptionsRequest) returns (GetGuildDonateOptionsResponse);
  rpc DonateToGuild(DonateToGuildRequest) returns (DonateToGuildResponse);
  rpc UpgradeGuild(UpgradeGuildRequest) returns (UpgradeGuildResponse);
  rpc GetGuildShop(GetGuildShopRequest) returns (GetGuildShopResponse);
  rpc BuyGuildShopGoods(BuyGuildShopGoodsRequest) returns (BuyGuildShopGoodsResponse);
```

五个都加进 `session.ClientMethods`(`go/guild/internal/session/session.go:42-52`)。`GetGuildDonateOptions` 不在契约的 RPC 表里:客户端不加载配表(map_tables:106),捐献页的选项和今日用量只能由服务端下发(契约偏差 2)。

### 5.5.4 `GuildInfo` 的填充

B2 §12.4 已在 `toProtoGuild` 里填 `funds`、`max_officers`、`upgrade_cost_funds`,B5 只需核对。**所有经济写 RPC 回包一律用 `l.guildInfoFor(ctx, g, playerID)`**,不直接用 `toProtoGuild`:否则长老看到的待审数会被清零,客户端 `Apply` 后申请角标就消失了(评审 #11)。

## 5.6 `proto/guild/guild_db.proto` 增量

**状态枚举不改**,沿用 S1:0 UNSPECIFIED、1 PENDING、2 APPLIED、3 REJECTED、4 ABORTED,与 S4 勘误 E1 的 `assetop.Status` 数值一致。UNSPECIFIED=0 的好处是:漏写状态列的行不会被当成未决行。

`GuildAssetOpKind` 追加一项:
```proto
  GUILD_ASSET_OP_KIND_DONATE_REFUND = 4;    // 捐献已扣款但捐献者已离帮:退回货币(GUILD_CREDIT)
```

`GuildAssetOpRecord` 需要以下列和索引。**推荐 B1 一次建全**(S4 E2 / 偏差 #18);只有 B1 落地时漏掉了,才由 B5a 追加:
```proto
  option(OptionIndex) = "status,next_attempt_ms;guild_id,op_id;player_id,stream,status,seq";
  // idx_guild_asset_op_3:AllocateSeq 加锁读未决行、离帮时提前截止、查本人待结算列表,都只扫未决行
  ...
  uint64 lease_until_ms = 21;   // 领取租约截止;0 = 未被领取
  uint64 lease_token = 22;      // 领取令牌(crypto/rand);Reschedule 须匹配
  uint32 tx_type = 23;          // TransactionType 数值,重投时原样带上
  uint32 last_outcome = 24;     // 最近一次 AssetOpOutcome(诊断)
  uint32 last_reason = 25;      // 最近一次 scene 回的 reason(任何结局都写;PENDING 时展示暂时原因)
  // 追加区:从 26 起(B6a)。
```

**两个原因列的分工**(同步写进 S4 Store 注释):`last_reason(25)` 由 Reschedule 和 Finalize 每次覆盖,表示"最近一次 scene 说了什么";`reason_tip_id(18)` 只由 Finalize 写一次,表示终态原因(APPLIED 与 ABORTED 写 0,REJECTED 写 scene 的 reason,人工终结写 0)。视图规则:PENDING 取 `last_reason`,终态取 `reason_tip_id`。

其它列的语义补充(不改 S1 字段号):
- `payload(12)`:`AssetBundle` 序列化字节;
- `ref_id(13)`:DONATE、DONATE_REFUND 填 donate_id,SHOP 填 goods_id;
- `ref_count(14)`:捐献恒为 1,商店为份数,退款为 0;
- `period_key(15)`:捐献填 `DayKey`,商店填日/周键,不限购与退款填 0(0 表示"没有占计数器行",退回时跳过);
- `next_attempt_ms(10)`:Finalize 时同时写成终结时刻,供 §5.22 的清理按它判龄。

**开发库**:新增普通索引不会被 schemamigrate 自动补建(S1 §6.4)。若由 B5a 追加列,交付说明必须写明:停 guild → `DROP DATABASE mmorpg_guild` → root 重跑 S1 §3 的两句 → 启动 guild 自动建表。项目未上线,不保留数据。

## 5.7 `proto/common/rollback/transaction_log.proto`

在 `TX_GUILD_ACTIVITY_REWARD` 之后追加 `TX_GUILD_DONATE_REFUND = <落码时枚举末尾 + 1>;  // 帮会捐献退款(GUILD_CREDIT)`,预期为 27。交易会话也在追加这个枚举,**数值以落码时为准,Go 和 C++ 一律引用生成的常量,不写数字**。

scene 白名单(`cpp/libs/services/scene/player/system/player_asset_op.cpp` 的 `kAssetOpStreamRules`)中,GUILD_CREDIT 一行加上 `TX_GUILD_DONATE_REFUND`。表的行数不变,`static_assert(size == 5)` 不受影响。

## 5.8 号段 biz_tag `guild_asset_op`

`op_id` 走 data_service 号段(契约 §3.2),同时用作 `AssetOpRequest.correlation_id`。要在 4 处同步登记(`config.go:94-99` 注释要求):
1. `go/data_service/internal/config/config.go` 的 `DefaultIdSegmentBootstrapTags` 追加 `"guild_asset_op"`;
2. `go/data_service/internal/store/id_segment_store.go:45` 的同一份清单追加;
3. `go/data_service/etc/data_service.yaml:119` 的 `BootstrapTags` 追加;
4. `tools/scripts/k8s_deploy.ps1:1688` 的 data-service ConfigMap 追加。

`guild_asset_op` 的消费表在独占库 `mmorpg_guild` 中,**不进** data_service `schema.go` 的水位地板校验(与 `trade_listing` 同理,`config.go:98` 已有先例注释,追加一句即可)。退款指令同样从这个号段取 id。

<!-- s5_economy_part3.md -->

# S5 经济(B5)— 第 3 部分:配表、默认行、启动校验、Tip 与限流

## 5.9 配表 schema

两张新表按 `data/AGENTS.md` 流程建:xlsx 第 1 行写英文列名(首列 `id`),第 2–4 行留空,第 5 行由导表器按 schema 注释回填,数据从第 6 行开始。`GuildRule`、`GuildLevel` 的 schema 和校验器归 B2(`s2_management_part1.md` §3.1–§3.3),**B5 不改 schema,也不另写校验器**,只改 `GuildLevel.xlsx` 的 `upgrade_cost_funds` 这一列,并往 B2 的 `validateGuildTables` 里追加规则(§5.11)。

### 5.9.1 `data/schema/guilddonate_table.proto`(新建)

```proto
syntax = "proto3";

// GuildDonate 表的权威 schema。源表 data/GuildDonate.xlsx。
// 本文件不参与 protoc 编译;导表器直接文本解析。字段号只增不改、删字段用 reserved(data/AGENTS.md)。
// 帮会捐献选项:一行 = 一个捐献按钮。只由 go/guild 读取(C++ 节点照例全量加载)。

package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message GuildDonateTable {
  option (cfg_sheet)       = "GuildDonate";
  option (cfg_source_file) = "GuildDonate.xlsx";
  option (cfg_primary_key) = "id";

  // 捐献选项 id
  uint32 id = 1;
  // 展示名
  string name = 2;
  // 货币类型:0 银两(kCurrencyGold) / 1 灵石(kCurrencyDiamond);其它值启动拒绝
  uint32 currency_type = 3;
  // 每次扣除的货币数量
  uint64 cost_amount = 4;
  // 每次获得的个人帮贡(累计与可用同加)
  uint64 contribution_gain = 5;
  // 每次增加的帮会资金
  uint64 funds_gain = 6;
  // 每游戏日次数上限(>=1)
  uint32 daily_limit = 7;
  // 需要的帮会等级(>=1)
  uint32 min_guild_level = 8;
}
```

### 5.9.2 `data/schema/guildshop_table.proto`(新建)

```proto
syntax = "proto3";

// GuildShop 表的权威 schema。源表 data/GuildShop.xlsx。规则同 guilddonate_table.proto。
// 帮会商店商品:一行 = 一件可兑换商品,花费可用帮贡(contribution_balance)。

package mmorpg.cfgtable.v1;

import "cfg_options.proto";

message GuildShopTable {
  option (cfg_sheet)       = "GuildShop";
  option (cfg_source_file) = "GuildShop.xlsx";
  option (cfg_primary_key) = "id";

  // 商品 id
  uint32 id = 1;
  // 展示名(Item 表没有名字列,商店以本列为准)
  string name = 2;
  // 分类:1 修行补给 / 2 帮会珍藏 / 3 节庆好礼
  uint32 category = 3;
  // 发放的物品
  uint32 item_id = 4 [(cfg_fk) = "Item"];
  // 每份物品数量(1..物品 max_stack_size)
  uint32 item_count = 5;
  // 每份消耗帮贡(1..1000000000)
  uint64 cost_contribution = 6;
  // 需要的帮会等级(1..最高级)
  uint32 required_guild_level = 7;
  // 限购周期:0 不限 / 1 每游戏日 / 2 每周
  uint32 limit_period = 8;
  // 周期内限购份数:limit_period=0 时必须为 0,否则 >=1
  uint32 limit_count = 9;
}
```

## 5.10 默认行(真实数值)

### GuildRule(沿用 B2 的行,B5 不改)

`1, 72, 3, 50, 600, 1000, 5`:捐献截止 600 秒,重投基准 1000 毫秒。

### GuildLevel(B2 的 5 行;B5 只改 `upgrade_cost_funds` 一列)

| id | max_members | max_officers | upgrade_cost_funds(B2 原值 → B5) |
|---|---|---|---|
| 1 | 50 | 2 | 10000 → **20000** |
| 2 | 60 | 3 | 30000 → **60000** |
| 3 | 70 | 4 | 80000 → **150000** |
| 4 | 80 | 5 | 200000 → **400000** |
| 5 | 100 | 6 | 0 |

升到满级累计需要 630,000 资金。估算:一个 50 人帮会人均每天给 10,000 资金,约 2 天满级;人均 2,000,约 7 天。这是开发期数值,上线前由策划调行,不改 schema。冒烟里帮主捐两次"银两大捐"(24,000),就能升到 2 级。

改这一列可能打破 B2 的测试或冒烟断言,落码前先查:`rg -n "UpgradeCostFunds|upgrade_cost_funds|10000" go/guild/internal robot/guild_smoke_scenario.go`,命中了就同步改断言(计入 B5b 名额)。`CreateGuild` 的 `max_members` 由 B2 从 `GuildLevel[1]` 读取(仍是 50),B5 不动。

### GuildDonate

| id | name | currency_type | cost_amount | contribution_gain | funds_gain | daily_limit | min_guild_level |
|---|---|---|---|---|---|---|---|
| 1 | 银两小捐 | 0 | 10000 | 10 | 1000 | 5 | 1 |
| 2 | 银两大捐 | 0 | 100000 | 120 | 12000 | 2 | 1 |
| 3 | 灵石捐献 | 1 | 100 | 200 | 20000 | 1 | 1 |

### GuildShop(11 件;物品 id 均在 `generated/tables/item.json` 中,`max_stack_size` 已核:15–23 为 999,12、13 为 1)

| id | name | category | item_id | item_count | cost_contribution | required_guild_level | limit_period | limit_count | 单次上限(算出) |
|---|---|---|---|---|---|---|---|---|---|
| 101 | 培元丹 | 1 | 15 | 5 | 30 | 1 | 1 | 10 | 20 |
| 102 | 回灵散 | 1 | 16 | 5 | 30 | 1 | 1 | 10 | 20 |
| 103 | 精炼石 | 1 | 17 | 1 | 80 | 2 | 1 | 5 | 20 |
| 104 | 修行秘录残页 | 1 | 18 | 1 | 150 | 3 | 2 | 5 | 20 |
| 201 | 帮会令牌 | 2 | 19 | 1 | 300 | 3 | 2 | 3 | 20 |
| 202 | 玄铁护符 | 2 | 12 | 1 | 800 | 4 | 2 | 1 | 1 |
| 203 | 灵兽口粮 | 2 | 20 | 10 | 120 | 2 | 1 | 3 | 20 |
| 204 | 藏经阁手札 | 2 | 13 | 1 | 1500 | 5 | 2 | 1 | 1 |
| 301 | 花灯 | 3 | 21 | 1 | 50 | 1 | 1 | 5 | 20 |
| 302 | 月饼礼盒 | 3 | 22 | 1 | 100 | 1 | 2 | 7 | 20 |
| 303 | 同心结 | 3 | 23 | 1 | 200 | 5 | 0 | 0 | 20 |

204 的等级要求从 6 改为 5,因为 B2 表只有 5 级。"单次上限"不是配表列,由 §5.11 的 `MaxBuyCount(row)` 算出,随视图下发。限购份数小于单次上限时,实际上以限购为准。

## 5.11 启动校验(fail-closed)

### 5.11.1 追加到 B2 的 `validateGuildTables`(`logic/guild_manage_logic.go`)

在 B2 的规则之后追加三条:
- `GuildRule.asset_op_deadline_seconds ∈ [60, 86400]`;
- `GuildRule.asset_op_retry_base_ms ∈ [100, 60000]`;
- 每行 `GuildLevel.upgrade_cost_funds ≤ 1_000_000_000_000`,防止误填超大值。

B2 已有的 `2 ≤ max_members ≤ 500`、`max_officers < max_members`、"cost==0 当且仅当最后一级"不重复写。

### 5.11.2 新文件 `go/guild/internal/logic/economy_config.go`

纯函数只吃切片,便于单测。`guild.go` 紧跟在 B2 的 `ValidateGuildTables()` 之后调用 `ValidateEconomyTables()`,失败时 `logx.Must`,与 B2 同级:

```go
const MaxShopBuyCount = 20 // 一次最多 20 份(放 constants.go)

type EconomyTables struct {
    MaxLevel     uint32                        // GuildLevel 行数(B2 校验保证从 1 连续)
    Donates      []*tablepb.GuildDonateTable
    Shops        []*tablepb.GuildShopTable
    ItemMaxStack func(itemID uint32) (uint32, bool) // 取 ItemTable.max_stack_size
}
func validateEconomyTables(t EconomyTables) error
func ValidateEconomyTables() error                // 从 table.*TableManagerInstance.FindAll() 组装后调用上面
// MaxBuyCount:max_stack_size==1 → 1;否则 min(MaxShopBuyCount, max_stack_size / item_count)。
func MaxBuyCount(row *tablepb.GuildShopTable, maxStack uint32) uint32
```

逐条规则(规则 1、2 是 B2 校验器里的 GuildRule、GuildLevel 规则;任一失败,返回带表名和行 id 的错误):
3. `GuildDonate`:`currency_type ∈ {0,1}`;`1 ≤ cost_amount ≤ math.MaxInt64`(scene Debit 的要求);`contribution_gain`、`funds_gain` 至少一个 `> 0`;`daily_limit ≥ 1`;`min_guild_level ∈ [1, MaxLevel]`;**每种货币至多 2 行**(客户端一个面板只放两个按钮)。
4. `GuildShop`:`category ∈ {1,2,3}`;物品存在(取得到 `max_stack_size` 且 ≥1);`1 ≤ item_count ≤ max_stack_size`;`max_stack_size == 1` 时 `item_count == 1`(单次份数恒为 1,每份占 1 格);`max_stack_size > 1` 时 `MaxBuyCount(row) ≥ 1`;`cost_contribution ∈ [1, 1e9]`(保证 `cost × 20` 不溢出);`required_guild_level ∈ [1, MaxLevel]`;`limit_period ∈ {0,1,2}`;`(limit_period == 0) == (limit_count == 0)`;每个 category 至少 1 行。

B2 未合入前,`ValidateEconomyTables` 不依赖它;`MaxLevel` 取 `len(GuildLevelTableManagerInstance.FindAll())`。

## 5.12 Tip(`data/tip/Tip.xlsx` `//guild_error`,名字按契约 §4)

B2 可能已加全部契约 tip。B5 核对下列行是否存在、文案是否如下;不存在就按契约顺序补(fault 列全部留空):

| 码名 | 文案 |
|---|---|
| GuildZoneMerging | 区服合并维护中,请稍后再试 |
| GuildRankTooLow | 当前职位无权进行此操作 |
| GuildFundsInsufficient | 帮会资金不足 |
| GuildMaxLevel | 帮会已达最高等级 |
| GuildDonateLimit | 今日该项捐献次数已用完 |
| GuildCurrencyInsufficient | 货币不足,无法捐献 |
| GuildAssetPending | 有未结算的帮会资产操作,请稍后再试 |
| GuildAssetRejected | 资产结算失败,本次操作已撤销 |
| GuildShopGoodsNotFound | 兑换商品不存在 |
| GuildShopLevelTooLow | 帮会等级不足 |
| GuildShopLimit | 已达限购数量 |
| GuildContributionInsufficient | 可用帮贡不足 |

Go 常量照 `constants.go:30-51` 的写法 `ErrXxx = uint32(table.GuildError_kGuildXxx)`,并同步 `constants_test.go` 的 `tipCodes()`(受 `TestNoHandWrittenTipCodes` 守护)。scene 原因码同样用生成常量,`constants.go` 追加 `AssetReasonCurrencyInsufficient = uint32(table.AssetError_kAssetCurrencyInsufficient)`,**业务代码里不出现 27000 这样的字面量**;`tipCodes()` 同步加上。枚举的 Go 名以 B4a 导表结果为准(`rg -n "kAssetCurrencyInsufficient" go/shared/generated/table`)。

## 5.13 MessageLimiter

proto-gen 分配号之后,在 `data/MessageLimiter.xlsx` 追加 5 行(照现有帮会行):读 `GetGuildDonateOptions`、`GetGuildShop` 每 1s 10 次;写 `DonateToGuild`、`UpgradeGuild`、`BuyGuildShopGoods` 每 1s 5 次;tip 列填 1000,与现有行一致。

<!-- s5_economy_part4.md -->

# S5 经济(B5)— 第 4 部分:游戏日、事务通则、预留与升级事务

## 5.14 `go/shared/gameday`(新包,B5 首个使用者)

`go/shared/gameday/gameday.go`:
```go
package gameday

// 全服游戏日:UTC+8、每日 05:00 切日;周 = ISO 周,周一 05:00 切周。
// 固定时区,不依赖 tzdata;时钟取服务进程时钟(各副本 NTP 偏差须 < 1s,跨切点 1s 内的请求可能记到相邻周期,可接受)。
var Zone = time.FixedZone("UTC+8", 8*3600)
const ResetHour = 5

func shifted(t time.Time) time.Time { return t.In(Zone).Add(-ResetHour * time.Hour) }
func DayKey(t time.Time) uint32  { y, m, d := shifted(t).Date(); return uint32(y*10000 + int(m)*100 + d) }
func WeekKey(t time.Time) uint32 { y, w := shifted(t).ISOWeek(); return uint32(y*100 + w) }
// NextDailyReset:严格晚于 t 的下一个 05:00(UTC+8)。
func NextDailyReset(t time.Time) time.Time
// NextWeeklyReset:严格晚于 t 的下一个周一 05:00(UTC+8)。
func NextWeeklyReset(t time.Time) time.Time
// PeriodKey:limitPeriod 0 → 0, true;1 → DayKey;2 → WeekKey;其它 → 0, false。
func PeriodKey(limitPeriod uint32, t time.Time) (uint32, bool)
```

`gameday_test.go` 的固定用例(用 `time.Date(..., gameday.Zone)` 构造):
- 2026-09-16 04:59:59 → DayKey 20260915;05:00:00 → 20260916;
- 2026-09-14(周一)05:00 → WeekKey 202638;04:59:59 → 202637;
- 2027-01-01 05:00 → WeekKey 202653(2026 年有 53 周);
- NextDailyReset(2026-09-16 05:00:00) = 2026-09-17 05:00;NextWeeklyReset(2026-09-16 12:00) = 2026-09-21 05:00;
- 输入 UTC 时间 2026-09-15T21:00:00Z(即 UTC+8 05:00)→ DayKey 20260916;
- `PeriodKey(3, t)` 返回 false。

## 5.15 事务通则(`internal/data/economy_repo.go`)

- **隔离级别 READ COMMITTED**:`r.db.BeginTx(txCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})`,用于 T-D、T-S、T-U、Finalize、ClaimDue 和清理。
  - 原因:在 RR 下,对不存在的计数器主键做 `FOR UPDATE`、`AllocateSeq` 对非唯一索引 idx_3 做范围 `FOR UPDATE`,都会拿到间隙锁。不同玩家首次捐献时,玩家 id 相邻就可能落进同一个间隙:间隙锁彼此兼容,但随后各自 INSERT 的插入意向锁会互相等待,形成 1213 死锁。
  - RC 下,InnoDB 只锁已存在的记录,不加间隙锁(唯一键冲突检查除外)。
  - 正确性不依赖间隙锁:同一玩家的请求由成员行锁(T-D/T-S)和 seq 行锁(AllocateSeq)串行化;计数器主键里含 player_id,不同玩家永远不会碰同一行。
  - 前提:MySQL `binlog_format=ROW`(MySQL 8 默认值;STATEMENT 格式下 RC 写入会报 1665)。TiDB 悲观事务支持 RC。Codex 第 3 步核对。
- **锁序**(S1 §2.1):`guild → guild_member → guild_application → guild_player_op_seq → guild_asset_op → guild_daily_counter`。每个事务都按这个顺序第一次触碰各表的行;同一行之后再 UPDATE,不算新加锁。
- **预算**:T-D/T-S/T-U 用 `txCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)`;Finalize 用 2000ms,ClaimDue 用 1000ms。B2 §6.2a 已在 DSN 上把 `innodb_lock_wait_timeout` 设为 1 秒,锁等待会在 1s 内以 1205 返回,不会拖满 ctx;ctx 先到期时,驱动关闭连接,服务端随之回滚。
- **重试助手** `withTxRetry(ctx, op string, fn)`:遇到 1213(死锁)或 1062(计数器 INSERT 撞上并发插入)时整事务重做,最多再试 2 次,每次先等 `20ms×attempt`。
- **忙错误** `ErrEconomyBusy`:以下情况都返回它,**logic 映射为 tip `GuildAssetPending`**(可重试,客户端不会被强制重连),指标记 `result="busy"`:重试用尽、1205 锁等待超时、`txCtx` 到期。其它数据库错误(连接拒绝等)按 B2 惯例 `return nil, err`。
- **提交结果不明**:若 COMMIT 本身出错(超时或断连),事务可能已经提交,同样返回 `ErrEconomyBusy`。已提交的行带 10s 租约,到期后由重投循环接手;客户端下次刷新时会在待结算列表里看到它。
- **合服闸门在事务内**:repo 方法接收 `fence data.FenceFunc`(`type FenceFunc func(ctx context.Context, zoneID uint32) error`)。logic 传入 `l.economyFence`:`l.mergeFence == nil || zoneID == 0` → nil;`merging, err := l.mergeFence.MergeInProgress(ctx, zoneID)`(`merge_fence.go:36`);`err != nil` → 记 ERROR 并返回 `data.ErrZoneMerging`(fail-closed,与 B2 §11.1 同口径);`merging` → `data.ErrZoneMerging`。zone 一律取事务里读到的 `guild.zone_id`。闸门只做一次 Redis 读,持锁期间多花几毫秒,可以接受。
- **EnsureSeqRow** 在事务外以 autocommit 执行:`assetop.EnsureSeqRow(ctx, db, seqTables, playerID, stream)`,其中 `seqTables = assetop.NewSeqTables("guild_player_op_seq", "guild_asset_op")`。
- **时间**:`nowMs := uint64(now.UnixMilli())`,一次请求只取一次,period_key 用同一个 `now`。
- **不写数值字面量**:状态、kind、stream、counter_kind、tx_type 全部作为绑定参数,取自生成常量(`?P` 等记号见 §5.3;`?KIND_DONATE` = `uint32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE)`(guild_db.proto 与 guild.proto 同在 `proto/guild`,包别名以落码时的 import 为准),`?TX_GUILD_DONATE` = `uint32(rollbackpb.TransactionType_TX_GUILD_DONATE)`,依此类推)。

## 5.16 捐献预留事务 T-D

`func (r *EconomyRepo) ReserveDonation(ctx, in DonationReserve) (DonationReserved, error)`

输入:`OpID, PlayerID, GuildID, DonateID, MinGuildLevel, ContributionGain, FundsGain, DailyLimit, PeriodKey, DeadlineMs, LeaseToken, NowMs, Payload, Fence`。`Payload` 由调用方生成:`proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType, Amount: CostAmount}}})`。

```sql
-- 事务外(EnsureSeqRow)
INSERT IGNORE INTO guild_player_op_seq (player_id, stream, next_seq, updated_ms) VALUES (?, ?GUILD_DEBIT, 1, ?now);

BEGIN;  -- READ COMMITTED
SELECT zone_id, level FROM guild WHERE guild_id = ?;            -- 不加锁;RC 读最新已提交版本
  -- 无行 → ROLLBACK,ErrEconomyGuildGone;level < MinGuildLevel → ErrEconomyLevelTooLow
  -- fence(ctx, zone_id) 返回 ErrZoneMerging → ROLLBACK,原样返回
SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE;
  -- 无行 → ROLLBACK,ErrEconomyNotMember
-- assetop.AllocateSeq(tx, seqTables, playerID, GUILD_DEBIT, DefaultLimits)(SQL 在 B4b,状态按 E1 绑定):
--   SELECT next_seq … FOR UPDATE;SELECT seq … status=PENDING ORDER BY seq LIMIT 17 FOR UPDATE;UPDATE next_seq+1
--   ErrTooManyPending → ROLLBACK,ErrEconomyTooManyPending
INSERT INTO guild_asset_op
  (op_id, player_id, stream, seq, guild_id, kind, status, durable, attempts, next_attempt_ms, deadline_ms,
   payload, ref_id, ref_count, period_key, contribution_delta, funds_delta, reason_tip_id, created_ms, updated_ms,
   lease_until_ms, lease_token, tx_type, last_outcome, last_reason)
VALUES (?, ?, ?GUILD_DEBIT, ?seq, ?, ?KIND_DONATE, ?P, 0, 0, ?now+10000, ?DeadlineMs,
   ?payload, ?donate_id, 1, ?dayKey, ?ContributionGain, ?FundsGain, 0, ?now, ?now,
   ?now+10000, ?token, ?TX_GUILD_DONATE, 0, 0);
SELECT used_count FROM guild_daily_counter
 WHERE player_id = ? AND counter_kind = ?COUNTER_DONATE AND ref_id = ? AND period_key = ? FOR UPDATE;
  -- 无行:INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms)
  --       VALUES (?, ?COUNTER_DONATE, ?, ?, 1, ?now)
  -- used_count >= DailyLimit → ROLLBACK,ErrEconomyDailyLimit
  -- 否则:UPDATE guild_daily_counter SET used_count = used_count + 1, updated_ms = ?now WHERE <主键>
COMMIT;
```
返回 `DonationReserved{Seq}`。`DeadlineMs = now + GuildRule.asset_op_deadline_seconds×1000`。

## 5.17 兑换预留事务 T-S

`func (r *EconomyRepo) ReserveShopOrder(ctx, in ShopReserve) (ShopReserved, error)`。Payload = `AssetBundle{Items: [{ConfigId: item_id, Count: item_count × count}]}`;`cost = cost_contribution × count`(校验保证不溢出)。

```sql
INSERT IGNORE INTO guild_player_op_seq (player_id, stream, next_seq, updated_ms) VALUES (?, ?GUILD_CREDIT, 1, ?now);  -- 事务外

BEGIN;  -- READ COMMITTED
SELECT zone_id, level FROM guild WHERE guild_id = ?;
  -- 无行 → ErrEconomyGuildGone;level < required_guild_level → ErrEconomyLevelTooLow;fence → ErrZoneMerging
SELECT contribution_balance FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE;
  -- 无行 → ErrEconomyNotMember;balance < cost → ErrEconomyContributionInsufficient
-- AllocateSeq(GUILD_CREDIT),同 T-D
INSERT INTO guild_asset_op (…同 T-D 的列…) VALUES (?, ?, ?GUILD_CREDIT, ?seq, ?, ?KIND_SHOP, ?P, 0, 0, ?now+10000, 0,
   ?payload, ?goods_id, ?count, ?periodKeyOr0, ?cost, 0, 0, ?now, ?now, ?now+10000, ?token, ?TX_GUILD_SHOP, 0, 0);
-- limit_period != 0 时:
SELECT used_count FROM guild_daily_counter
 WHERE player_id = ? AND counter_kind = ?COUNTER_SHOP AND ref_id = ? AND period_key = ? FOR UPDATE;
  -- 无行:count > limit_count → ErrEconomyShopLimit;否则 INSERT used_count = count
  -- 有行:used_count + count > limit_count → ErrEconomyShopLimit;否则 UPDATE used_count = used_count + ?count
UPDATE guild_member SET contribution_balance = contribution_balance - ?cost
 WHERE guild_id = ? AND player_id = ? AND contribution_balance >= ?cost;
  -- RowsAffected != 1 → ROLLBACK,ErrEconomyContributionInsufficient(理论上不可达,行已锁)
COMMIT;
```
返回 `ShopReserved{Seq, BalanceAfter}`。

## 5.18 升级事务 T-U

`func (r *EconomyRepo) UpgradeGuild(ctx, guildID, playerID uint64, expectedLevel uint32, levels LevelLookup, fence FenceFunc) (UpgradeResult, error)`

```sql
BEGIN;  -- READ COMMITTED
SELECT level, funds, zone_id FROM guild WHERE guild_id = ? FOR UPDATE;        -- 无行 → ErrEconomyGuildGone
SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE; -- 无行 → ErrEconomyNotMember
-- constants.Rank(role) < constants.Rank(RoleOfficer) → ErrEconomyRankTooLow
-- fence(ctx, zone_id) → ErrZoneMerging
-- expectedLevel != 0 && expectedLevel != level → COMMIT(无改动),UpgradeResult{Changed:false}
-- cur := levels(level);cur.UpgradeCostFunds == 0 → ErrEconomyMaxLevel
-- next := levels(level+1)(B2 启动校验保证存在);funds < cur.UpgradeCostFunds → ErrEconomyFundsInsufficient
UPDATE guild SET level = ?level+1, funds = funds - ?cost, max_members = ?next.MaxMembers
 WHERE guild_id = ? AND level = ?level AND funds >= ?cost;                     -- RowsAffected 必为 1,否则回滚并返回内部错误
SELECT player_id FROM guild_member WHERE guild_id = ? ORDER BY player_id;      -- 不加锁,作推送收件人
COMMIT;
-- 提交后 r.guildRepo.invalidateAfterCommit("upgrade", guildID)(B2 §6 助手;失效失败只打 ERROR)
```
返回 `UpgradeResult{Changed, NewLevel, MemberIDs}`。注意:扣的是**当前等级行**的 `upgrade_cost_funds`(B2 语义:"升到下一级所需资金"),新上限取下一级行的 `max_members`。

<!-- s5_economy_part5.md -->

# S5 经济(B5)— 第 5 部分:资产 Store、离帮提前截止、读查询、清理与运维工具

## 5.19 资产 Store:`internal/data/asset_store.go`

`type GuildAssetStore struct{ db *sql.DB; repo *GuildRepo; opIDs OpIDMinter; seqTables assetop.SeqTables; OnFinalized func(context.Context, FinalizedOp) }`,实现 `assetop.Store` 与可选的 `OldestPendingCreatedMs`。`OpIDMinter` 即 `interface{ Mint(ctx) (uint64, error) }`,由 `*idsegment.Minter` 满足。

### 5.19.1 ClaimDue(两档领取,防新行被饿死)

一个 READ COMMITTED 事务(1000ms),令牌由 Loop 生成:
```sql
-- 第一档:新行(attempts < 3,含同步投递跳过的、刚提交就崩溃遗留的)
UPDATE guild_asset_op SET lease_until_ms = ?leaseUntil, lease_token = ?token
 WHERE status = ?P AND next_attempt_ms <= ?now AND lease_until_ms < ?now AND attempts < 3
 ORDER BY next_attempt_ms LIMIT ?batch;
-- n1 = RowsAffected;n1 < batch 时第二档(老行,如长期背包满的商店行):
UPDATE guild_asset_op SET lease_until_ms = ?leaseUntil, lease_token = ?token
 WHERE status = ?P AND next_attempt_ms <= ?now AND lease_until_ms < ?now
 ORDER BY next_attempt_ms LIMIT ?(batch - n1);
SELECT op_id, player_id, stream, seq, tx_type, payload, attempts, deadline_ms
  FROM guild_asset_op WHERE status = ?P AND lease_token = ?token;   -- 走 idx_0 的 status 前缀,只扫未决行
COMMIT;
```
`payload` 反序列化失败时:同一事务里 `UPDATE … SET lease_until_ms = 0, next_attempt_ms = ?now+60000, updated_ms = ?now WHERE op_id = ?`,不放进返回列表,打 ERROR 并计 `guild_asset_bad_payload_total`(fail-closed,不下发)。

### 5.19.2 Reschedule(autocommit,单行主键更新)

```sql
UPDATE guild_asset_op SET attempts = attempts + 1, next_attempt_ms = ?, lease_until_ms = 0,
       durable = ?, last_outcome = ?, last_reason = ?, updated_ms = ?
 WHERE op_id = ? AND status = ?P AND lease_token = ?;
```

### 5.19.3 Finalize(op, status, res, nowMs) (bool, error)

**第 0 步(事务外,普通读不可变列)**:`SELECT guild_id, kind, ref_id, ref_count, period_key, contribution_delta, funds_delta, payload FROM guild_asset_op WHERE op_id = ?`。无行 → ERROR,返回 `false, nil`。

**第 1 步(仅 DONATE + APPLIED,准备退款)**:普通读 `SELECT 1 FROM guild_member WHERE guild_id = ? AND player_id = ?`。无行时:`assetop.EnsureSeqRow(GUILD_CREDIT)`,再 `refundOpID, err = s.opIDs.Mint(ctx)`;`opIDs == nil` 或发号失败 → 返回 `false, err`(行保持 PENDING,租约到期后重试)。

**第 2 步 BEGIN(RC,2000ms),按锁序加锁**:
- DONATE + APPLIED:`SELECT funds FROM guild WHERE guild_id = ? FOR UPDATE` → `guildOK`;`SELECT contribution_total FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE` → `memberOK`。令 `stillMember = guildOK && memberOK`。
  - `!stillMember && refundOpID == 0`(第 1 步之后才离帮)→ ROLLBACK,返回 `false, errRefundNotPrepared`(下一轮会走第 1 步)。
  - `!stillMember`:`refundSeq, err := assetop.AllocateSeq(tx, seqTables, playerID, GUILD_CREDIT, DefaultLimits)`;`ErrTooManyPending` → ROLLBACK,返回 `false, ErrRefundBlocked`,计 `guild_asset_refund_blocked_total`。
- SHOP + (REJECTED | ABORTED):`SELECT contribution_balance FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE` → `memberOK`。
- 其它组合:不加锁。

**第 3 步 CAS**:
```sql
UPDATE guild_asset_op SET status = ?status, durable = 1, last_outcome = ?res.Outcome, last_reason = ?res.Reason,
       reason_tip_id = ?terminalReason, lease_until_ms = 0, next_attempt_ms = ?now, updated_ms = ?now
 WHERE op_id = ? AND status = ?P;
-- terminalReason:REJECTED 写 res.Reason,其余写 0。RowsAffected == 0 → ROLLBACK,返回 false, nil
```

**第 4 步 对侧账,按 kind 显式 switch**:

| kind | 终态 | 动作 |
|---|---|---|
| DONATE | APPLIED 且 stillMember | `UPDATE guild SET funds = funds + ?funds_delta WHERE guild_id = ?`;`UPDATE guild_member SET contribution_total = contribution_total + ?c, contribution_balance = contribution_balance + ?c WHERE guild_id = ? AND player_id = ?` |
| DONATE | APPLIED 且 !stillMember | 插退款指令(下方 SQL);退次数(n=1);INFO `[GuildAsset] donate refund donate_op=… refund_op=… player_id=… guild_id=…`;计 `guild_asset_refund_total{kind="donate"}` |
| DONATE | REJECTED / ABORTED | 退次数(n=1) |
| SHOP | APPLIED | 无 |
| SHOP | REJECTED / ABORTED | memberOK → `UPDATE guild_member SET contribution_balance = contribution_balance + ?contribution_delta WHERE …`;否则计 `guild_asset_orphan_total{kind="shop",what="refund_member_gone"}`。period_key≠0 时退限购(n=ref_count) |
| ACTIVITY_REWARD | 任意 | 无(S6 语义:帮贡和资金在入队事务里已记完) |
| DONATE_REFUND | APPLIED | 无 |
| DONATE_REFUND | REJECTED | ERROR `[GuildAsset] donate refund rejected, manual compensation op_id=… reason=…`,计 `guild_asset_orphan_total{kind="donate_refund",what="rejected"}`;按 §5.23 人工补偿 |
| 其它 | 任意 | ERROR `unknown asset op kind`,只做 CAS |

```sql
-- 退款指令(GUILD_CREDIT;立即可领,不设租约)
INSERT INTO guild_asset_op (…同 T-D 的 25 列…)
VALUES (?refundOpID, ?player, ?GUILD_CREDIT, ?refundSeq, ?guild_id, ?KIND_DONATE_REFUND, ?P, 0, 0, ?now, 0,
   ?payload /*原捐献包*/, ?ref_id, 0, 0, 0, 0, 0, ?now, ?now, 0, 0, ?TX_GUILD_DONATE_REFUND, 0, 0);
-- 退次数 / 退限购(n 见上表;counter_kind 由 kind 映射:DONATE→COUNTER_DONATE,SHOP→COUNTER_SHOP)
UPDATE guild_daily_counter SET used_count = IF(used_count >= ?n, used_count - ?n, 0), updated_ms = ?now
 WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ?;   -- 行已被清理则影响 0 行,无害
```

锁序核对(退款路径最长):guild → guild_member → guild_player_op_seq(AllocateSeq)→ guild_asset_op(未决读、CAS、INSERT)→ guild_daily_counter,符合 S1。

**第 5 步 COMMIT 后**:改过 guild 或成员行时调 `repo.invalidateAfterCommit("asset_finalize", guildID, playerID)`;然后若 `OnFinalized != nil`,调 `OnFinalized(ctx, FinalizedOp{OpID, PlayerID, GuildID, Kind, Status, Refunded})`。

**OldestPendingCreatedMs(stream)**:`SELECT MIN(created_ms) FROM guild_asset_op WHERE status = ?P AND stream = ?`。

## 5.20 离帮、被踢、解散时提前截止(改 B2 的 `guild_manage_repo.go`)

三个 B2 事务都在删除成员行之后、提交之前各追加一条语句(沿用 B2 的 `inTx`,不改其隔离级别):

```sql
-- LeaveGuild:第 4 步 DELETE member 之后;KickMember:删除目标成员行之后(player_id = 目标)
UPDATE guild_asset_op SET deadline_ms = ?now, next_attempt_ms = LEAST(next_attempt_ms, ?now), updated_ms = ?now
 WHERE player_id = ? AND stream = ?GUILD_DEBIT AND status = ?P
   AND guild_id = ? AND kind = ?KIND_DONATE AND deadline_ms > ?now;
-- DisbandGuild:第 3 步 DELETE guild_application 之后、第 4 步之前,player_id 换成第 2 步读到的成员列表
--   WHERE player_id IN (?, …) AND …(同上);成员数上限 500,单条语句即可
```

- **锁序**:guild → guild_member →(guild_application)→ guild_asset_op,符合 S1。
- **死锁分析**:该语句走 idx_3,只扫未决行(每人 ≤16)。B2 事务是 RR,会在 idx_3 上留下短暂的 next-key 锁。其他玩家 T-D 插入落进这个间隙时,只会等 B2 事务提交(毫秒级),B2 事务此后不再等待任何 T-D 持有的锁,不构成环。同一玩家的 T-D 与离帮在成员行上串行。
- **不抢租约**:同步投递还持有租约的行,要等租约到期(≤10s)才被领取。已在重投中的行,若 Reschedule 把 `next_attempt_ms` 推后,最多晚一个 MaxBackoff(60s)才发中止。
- 帮会解散后,DONATE_REFUND、SHOP、ACTIVITY_REWARD 行不动:物品和退款属于玩家,照常投递。

## 5.21 读查询

- 本人今日捐献用量:`SELECT ref_id, used_count FROM guild_daily_counter WHERE player_id = ? AND counter_kind = ?COUNTER_DONATE AND period_key = ?`。
- 本人商店用量:`SELECT ref_id, period_key, used_count FROM guild_daily_counter WHERE player_id = ? AND counter_kind = ?COUNTER_SHOP AND period_key IN (?day, ?week)`(日键 8 位、周键 6 位,数值域不重叠)。
- 待结算列表:`SELECT op_id, guild_id, kind, ref_id, ref_count, contribution_delta, funds_delta, last_reason, created_ms, payload FROM guild_asset_op WHERE player_id = ? AND stream = ? AND status = ?P ORDER BY seq LIMIT 16`(走 idx_3)。在 Go 里过滤:stream 1 只留 `kind == DONATE && guild_id == 当前帮`,stream 2 只留 `kind == SHOP`。
- 最近结果:`SELECT op_id, guild_id, kind, status, ref_id, ref_count, contribution_delta, funds_delta, reason_tip_id, created_ms, updated_ms, payload FROM guild_asset_op WHERE player_id = ? AND stream = ? ORDER BY seq DESC LIMIT 20`(沿 `uk_guild_asset_op` 倒序,最多扫 20 行,与历史行数无关)。在 Go 里过滤 `status ∈ {?A,?R,?X} && updated_ms ≥ now−600000 && kind 同上`,取前 5 条。
- 帮贡:`SELECT contribution_total, contribution_balance FROM guild_member WHERE guild_id = ? AND player_id = ?`(直读 MySQL,不走缓存)。
- 单行状态回读:`SELECT status, last_reason, reason_tip_id FROM guild_asset_op WHERE op_id = ?`。

## 5.22 清理任务(`GuildAssetStore.Cleanup`,goroutine `guild.asset_op_cleanup`)

每个副本都跑:启动后先随机延迟 `[0, CleanupIntervalMinutes)`,之后每 `CleanupIntervalMinutes` 跑一次。删除是幂等的,多副本同时跑无害。每条语句自成一个 RC 事务,单批 500 行,批间 sleep 100ms;每轮每条语句最多 20 批,`RowsAffected < 500` 即停。
```sql
DELETE FROM guild_asset_op WHERE status IN (?A, ?R, ?X) AND next_attempt_ms < ?opCutoffMs LIMIT 500;      -- idx_0
DELETE FROM guild_daily_counter WHERE period_key BETWEEN 19700101 AND ?dayCutoff LIMIT 500;               -- 日键
DELETE FROM guild_daily_counter WHERE period_key BETWEEN 100000 AND ?weekCutoff LIMIT 500;                -- 周键
```
`opCutoffMs = now − TerminalRetentionDays×86400000`(默认 30 天);`dayCutoff = DayKey(now − CounterRetentionDays×24h)`、`weekCutoff = WeekKey(同一时刻)`(默认 30 天)。终态行的 `next_attempt_ms` 等于终结时刻(§5.19.3 第 3 步),PENDING 行永不删除。计数器行按玩家计、只影响当期,30 天前的行删了不会让"解散重建再领一次"成立(S6 §6.12 的顾虑只涉及当期)。指标 `guild_asset_cleanup_deleted_total{table}`。

增长估算:保留 30 天时,每个活跃玩家约 8×30 条捐献指令、≤90 条计数行,外加兑换行;万人规模约 300 万行,可以接受。

## 5.23 运维工具 `go/guild/cmd/assetopfix/main.go`

用途:scene 回 UNKNOWN(`kBehindWindow` 或信封非法,S4 §4.9 第 1、4 步)时,Loop 会走 Alert,每 60s 重排一次,永不终结;该玩家累计 16 行后,新兑换、活动奖励和退款全部被 `GuildAssetPending` 挡住。UNKNOWN 的行,scene **以后也不会再应用**(窗口已过或信封非法),唯一的疑问是"以前是否已经应用过",所以人工查清流水后终结是安全的。

- `assetopfix -f etc/guild.yaml list [-min-age-min 60] [-limit 50]`:`SELECT op_id, player_id, stream, seq, kind, attempts, last_outcome, last_reason, created_ms FROM guild_asset_op WHERE status = ?P AND created_ms < ? ORDER BY created_ms LIMIT ?`。
- `assetopfix -f etc/guild.yaml resolve -op <op_id> -as applied|aborted -operator <名字> -confirm <op_id> -txlog-checked`:
  1. 读行,必须同时满足:PENDING;`created_ms` 早于 30 分钟前;`last_outcome == UNKNOWN(0) && attempts ≥ 10`。任一不满足即拒绝(带 `-force` 才跳过第三条)。
  2. `-confirm` 必须等于 `-op`;缺 `-txlog-checked` 就打印核对清单后退出:在 Loki 查 `[AssetOp] … corr=<op_id>`,并在交易流水里查 correlation_id=op_id。查到已应用 → `-as applied`;确认从未应用 → `-as aborted`。
  3. DONATE_REFUND 禁止 `-as aborted`:应先用 GM 发放补偿,再 `-as applied`。
  4. 构造 `assetop.Result{Outcome: APPLIED 或 REJECTED, Reason: 0, Durable: true}`,状态为 APPLIED 或 ABORTED,调用同一个 `GuildAssetStore.Finalize`(`OnFinalized` 为 nil;号段客户端用 `svc.NewAssetOpIDSegment(conf)` 构造,DONATE 已离帮时要用它插退款指令)。CAS 返回 false 时打印"已被自动终结"。
  5. 审计:`logx.Errorf("[AssetOpManual] operator=%s op_id=%d player_id=%d stream=%d seq=%d kind=%d as=%s")`,ERROR 级确保进 Loki;同时要求操作者把输出贴到运维工单。

告警(写进 `docs/design/guild-phase2.md` 运维节):`increase(assetop_unknown_total{service="guild"}[10m]) > 0` 报警;`assetop_pending_oldest_age_seconds{service="guild",stream="2"} > 86400` 提示。stream 1 不设年龄告警:离线玩家的过期捐献会一直 PENDING,直到上线后才中止,属于正常情况。

<!-- s5_economy_part6.md -->

# S5 经济(B5)— 第 6 部分:logic 流程、接线、推送、指标、崩溃窗口

## 5.24 logic 结构(`internal/logic/economy_logic.go`)

与 B2 的 `WithNotifier` 用同一种 Option 写法,**不另声明推送接口**,推送一律走 B2 的 `l.notify(kind, guildID, actor, target, recipients)`(`push.go` §13.5):
```go
type EconomyDeps struct {
    Repo          *data.EconomyRepo
    Loop          *assetop.Loop       // 同步投递与重投共用 ProcessOne
    OpIDs         data.OpIDMinter     // nil = 号段关闭 → 经济写 RPC 回 ErrIDGenUnavailable
    Now           func() time.Time
    SyncBudget    time.Duration       // 2500ms
    HandlerBudget time.Duration       // 3500ms:从进入 handler 起算,留 500ms 给编码与路由
}
func WithEconomy(d EconomyDeps) Option { return func(l *GuildLogic) { l.economy = &d } }
```
`l.economy == nil` 时,五个 RPC 一律回 `status.Error(codes.Unavailable, "guild economy not enabled")`(只会出现在配置错误时,启动日志已告警)。

**公共前置** `l.economyCaller(ctx) (playerID uint64, guild *data.GuildData, tip uint32, err error)`:
1. `playerID, fromClient := session.ClientPlayerID(ctx)`;`!fromClient` → `PermissionDenied`。
2. `guildID := repo.GetPlayerGuildID`;为 0 → tip `ErrNotInGuild`。
3. `guild := repo.GetGuild`;为 nil → 先 `RefreshPlayerGuildID`,再回 `ErrNotInGuild`(同 LeaveGuild 处理悬挂映射的做法,`guild_logic.go:361-373`)。
4. 缓存里没有本人成员 → 同样回 `ErrNotInGuild`(事务内还会复核)。

**repo 错误的统一映射** `economyTip(err)`:`ErrEconomyNotMember` → `ErrNotInGuild`(并 `RefreshPlayerGuildID`);`ErrEconomyGuildGone` → `ErrGuildNotFound`;`ErrZoneMerging` → `GuildZoneMerging`;`ErrEconomyTooManyPending`、`ErrEconomyBusy` → `GuildAssetPending`;`ErrEconomyLevelTooLow` → `GuildShopLevelTooLow`;`ErrEconomyDailyLimit` → `GuildDonateLimit`;`ErrEconomyShopLimit` → `GuildShopLimit`;`ErrEconomyContributionInsufficient` → `GuildContributionInsufficient`;`ErrEconomyRankTooLow` → `GuildRankTooLow`;`ErrEconomyMaxLevel` → `GuildMaxLevel`;`ErrEconomyFundsInsufficient` → `GuildFundsInsufficient`;其它错误 → `return nil, err`。

## 5.25 DonateToGuild

```
start := Now()
1  公共前置(5.24)
2  row := GuildDonate[donate_id];找不到 → error=GuildAssetRejected
3  缓存预判 guild.Level < row.min_guild_level → GuildShopLevelTooLow(事务内复核)
4  OpIDs == nil → ErrIDGenUnavailable;opID, err := OpIDs.Mint(ctx);出错 → ErrIDGenUnavailable
5  token := cryptoRandUint64();rule := GuildRule[1]
6  EnsureSeqRow(GUILD_DEBIT);ReserveDonation(PeriodKey=DayKey(start), DeadlineMs=now+rule.asset_op_deadline_seconds*1000, Fence=l.economyFence)
     出错 → economyTip(err)
7  op := assetop.Op{OpID, PlayerID, Stream: GUILD_DEBIT, Seq, CorrelationID: opID,
                    TxType: uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), Bundle, DeadlineMs, LeaseToken: token}
8  budget := min(SyncBudget, HandlerBudget − since(start));budget < 300ms → 跳过(计 guild_asset_sync_skipped_total{kind="donate"}),
     行在 10s 租约到期后由重投循环领取;否则 _, _ = Loop.ProcessOne(withSyncDelivery(ctx, budget), op)   // 错误只打日志,结局以回读为准
9  st := repo.OpState(opID);回读失败 → 视图 PENDING、reason 0,打 ERROR
10 视图:status 1→PENDING(reason=last_reason)、2→APPLIED、3→REJECTED(reason=reason_tip_id)、4→ABORTED
11 error_message:REJECTED && reason == constants.AssetReasonCurrencyInsufficient → GuildCurrencyInsufficient;
     其它 REJECTED → GuildAssetRejected;否则不填
12 重读 guild(APPLIED 时缓存已失效);本人仍是成员 → guild = l.guildInfoFor(ctx, g, playerID);返回
```
`withSyncDelivery` 往 ctx 里放一个标记,`OnAssetFinalized` 看到它就不推送(调用方自己会拿到回包)。

## 5.26 UpgradeGuild

```
1 公共前置
2 res, err := repo.UpgradeGuild(ctx, guildID, playerID, req.ExpectedLevel, levelLookup, l.economyFence);出错 → economyTip(err)
3 res.Changed → l.notify(LEVEL_UP, guildID, playerID, 0, 除 playerID 外的 res.MemberIDs)   // B2 §14:操作者不推
4 回 l.guildInfoFor(ctx, 最新 guild, playerID)(Changed=false 时同样回成功 + 最新 GuildInfo)。
  业务失败(RankTooLow / MaxLevel / FundsInsufficient / ZoneMerging / AssetPending)也带上最新 GuildInfo,
  资金不足时客户端能顺带刷新资金显示;只有 ErrNotInGuild / ErrGuildNotFound 不带
```

## 5.27 BuyGuildShopGoods

```
1 公共前置
2 row := GuildShop[goods_id];找不到 → GuildShopGoodsNotFound
3 count := req.Count;为 0 按 1 处理;count > MaxBuyCount(row, maxStack(row.item_id)) → GuildShopLimit
4 缓存预判 guild.Level < row.required_guild_level → GuildShopLevelTooLow(事务内复核)
5 缓存预判 本人 ContributionBalance < cost → GuildContributionInsufficient(省一次发号;事务内复核)
6 Mint opID;pk, _ := gameday.PeriodKey(row.limit_period, start)
7 EnsureSeqRow(GUILD_CREDIT);ReserveShopOrder(Fence=l.economyFence);出错 → economyTip(err)
8 同 5.25 第 7–10 步(Stream GUILD_CREDIT、TxType TX_GUILD_SHOP、DeadlineMs 0)
9 REJECTED(任何 reason)→ error=GuildAssetRejected;balance 重新直读 MySQL(已退回)
10 回 order + contribution_balance
```

## 5.28 两个读 RPC(不查合服闸门)

**GetGuildDonateOptions**:公共前置 → 今日用量 map(5.21)→ 按 id 升序组装 options(`unlocked = guild.Level >= min_guild_level`)→ 待结算捐献(解析 payload 取 currency/amount;`contribution_gain`/`funds_gain` 取行里的 delta;reason 取 `last_reason`)→ `recent_results`(reason 取 `reason_tip_id`)→ 帮贡直读 → `next_daily_reset_ms`。

**GetGuildShop**:公共前置 → 日键、周键用量 → goods(`used_count` 按该行的 limit_period 取对应键,0 周期填 0;`max_buy_count = MaxBuyCount(row, maxStack)`)→ 待发放订单(`goods_id=ref_id, count=ref_count, cost_contribution=contribution_delta`)→ `recent_orders` → balance 直读 → 两个重置时刻。

## 5.29 接线

**`internal/svc/asset_op.go`**(新):
- `const AssetOpBizTag = "guild_asset_op"`;导出 `NewAssetOpIDSegment(c config.Config) (*idsegment.Client, error)`,照 `initGuildIDSegment`(`guild_id_minter.go:35-66`)的写法,运维工具复用它;`IdSegment.Enabled=false` 时返回 nil, nil。
- `NewAssetPipeline(sc, etcdCli *clientv3.Client, store assetop.Store) (*AssetPipeline, error)`:按 S4 §4.22 的接线示意建 `scenenode.Watcher`(`SceneNodeRpcPrefix`)、`ConnCache`、`Locator{Reader: GoRedisReader{sc.PlayerLocatorRedisClient}}`、`assetop.Caller{CallTimeout: 800ms, Requery: DefaultRequery}`,再 `assetop.NewLoop(loopConfigFrom(config.AppConfig.AssetOp), store, caller, assetop.NewMetrics(prometheus.DefaultRegisterer, "guild"), time.Now)`。
- `loopConfigFrom`:`Interval=ReconcileIntervalMs, Batch=ReconcileBatch, Lease=LeaseMs, OpBudget=OpBudgetMs, BaseBackoff=GuildRule[1].asset_op_retry_base_ms, MaxBackoff=MaxBackoffMs, AwaitDurableDelay=500ms`。

**`internal/svc/servicecontext.go`**:在 `GuildIDSegment`(`:35`)旁加字段 `AssetOpIDSegment *idsegment.Client`;`:107` 的 `sc.initGuildIDSegment()` 之后调 `sc.initAssetOpIDSegment()`;`Stop()`(`:136`)里仿照 GuildIDSegment 做 Close。

**`internal/config/config.go`**:
```go
type AssetOpConf struct {
    Enabled                bool `json:",default=true"`
    ReconcileIntervalMs    int  `json:",default=1000"`
    ReconcileBatch         int  `json:",default=10"`
    LeaseMs                int  `json:",default=30000"`
    OpBudgetMs             int  `json:",default=2500"`
    MaxBackoffMs           int  `json:",default=60000"`
    CleanupEnabled         bool `json:",default=true"`
    CleanupIntervalMinutes int  `json:",default=10"`
    TerminalRetentionDays  int  `json:",default=30"`
    CounterRetentionDays   int  `json:",default=30"`
}
AssetOp AssetOpConf `json:"AssetOp,optional"`
```
`Validate` 追加:`ReconcileIntervalMs ∈ [200,60000]`;`ReconcileBatch ∈ [1,100]`;`OpBudgetMs ∈ [1000,2500]`;**`LeaseMs ≥ ReconcileBatch×OpBudgetMs + 5000`** 且 `≤ 600000`;`MaxBackoffMs ∈ [5000,600000]`;`CleanupIntervalMinutes ∈ [1,1440]`;两个保留天数 `∈ [7,365]`。理由:S4 的 `Tick` 串行处理一批(§4.19),最坏每行耗尽 OpBudget;10×2.5s=25s < 30s,整批处理完之前租约不会过期,不会被其他副本重领。正常情况下每行几毫秒,每秒一轮、一轮 10 行,重投吞吐 ≥10 行/秒,正常流量走同步路径,够用。`etc/guild.yaml` 把整块显式写出。

**`guild.go`**(B2 的 `ValidateGuildTables` 之后;`etcdCli` 若在 `NewGuildLogic` 之后才创建,把本段整体挪到 `etcdCli` 建好之后):
```go
logx.Must(logic.ValidateEconomyTables())
assetMinter := opIDMinterOrNil(svcCtx)   // AssetOpIDSegment==nil → 返回 nil 接口;否则 &idsegment.Minter{Name: svc.AssetOpBizTag, Segment: svcCtx.AssetOpIDSegment}(不设 Fallback)
store := data.NewGuildAssetStore(svcCtx.DB, repo, assetMinter)
pipe, err := svc.NewAssetPipeline(svcCtx, etcdCli, store); logx.Must(err)
guildLogic := logic.NewGuildLogic(repo, guildIDs, onlineResolver, mergeFence, homeZones,
    logic.WithNotifier(/* B2 原样 */),
    logic.WithEconomy(logic.EconomyDeps{Repo: data.NewEconomyRepo(svcCtx.DB, repo), Loop: pipe.Loop, OpIDs: assetMinter,
        Now: time.Now, SyncBudget: 2500 * time.Millisecond, HandlerBudget: 3500 * time.Millisecond}))
store.OnFinalized = guildLogic.OnAssetFinalized          // 在循环和 server 启动之前赋值,无并发
pipeCtx, pipeCancel := context.WithCancel(context.Background()); defer pipeCancel()   // 写在 defer etcdCli.Close() 之后
safego.Go("guild.scene_node_watch", func() { pipe.Watcher.Run(pipeCtx, etcdCli) })
if c.AssetOp.Enabled { safego.Go("guild.asset_op_reconcile", func() { pipe.Loop.Run(pipeCtx) }) }
if c.AssetOp.CleanupEnabled { safego.Go("guild.asset_op_cleanup", func() { store.RunCleanup(pipeCtx, cleanupConfFrom(c.AssetOp)) }) }
```
`AssetOp.Enabled=false` 只停重投循环,同步投递照常(排障用)。合服闸门不经 EconomyDeps 注入:logic 直接用 B2 已注入的 `l.mergeFence`,包装为 `l.economyFence`(§5.15)。

**server / session**:`guild_server.go` 加 5 个一行委托;`session.ClientMethods` 加 5 个方法全名。

## 5.30 推送(`OnAssetFinalized`)

```go
func (l *GuildLogic) OnAssetFinalized(ctx context.Context, op data.FinalizedOp) {
    if isSyncDelivery(ctx) { return }
    var kind pb.GuildChangeKind
    switch op.Kind {
    case kindDonate:
        if op.Refunded { return }                         // 退款指令到账时再推 DELIVERY_DONE
        kind = pb.GuildChangeKind_FUNDS_CHANGED
    case kindShop, kindActivityReward, kindDonateRefund:
        kind = pb.GuildChangeKind_DELIVERY_DONE
    default:
        logx.Errorf("[GuildAsset] finalized unknown kind=%d op_id=%d", op.Kind, op.OpID); return
    }
    l.notify(kind, op.GuildID, 0, op.PlayerID, []uint64{op.PlayerID})
}
```
只推给本人。捐献不广播全帮(避免 500 人帮会的推送风暴,其他成员打开界面时自己拉);升级推送给除操作者外的全员(5.26)。推送至多一次,客户端收到只做拉取。B2 `push.go` 的 `changeKindLabel` 追加 `funds_changed`、`level_up`、`delivery_done`。

## 5.31 指标(不带 player_id / guild_id)

- `guild_economy_requests_total{rpc, result}`:result ∈ applied / pending / rejected / limit / insufficient / pending_guard / busy / fence / not_member / level / error;
- `guild_asset_orphan_total{kind, what}`、`guild_asset_refund_total{kind}`、`guild_asset_refund_blocked_total`;
- `guild_asset_sync_skipped_total{kind}`、`guild_asset_cleanup_deleted_total{table}`、`guild_asset_bad_payload_total`;
- 其余复用 `assetop_*`、`scenenode_*`(service="guild")。

## 5.32 崩溃与并发窗口

| # | 场景 | 结果 |
|---|---|---|
| W1 | T-D 提交后、调 scene 前 guild 崩溃 | 行 PENDING、租约 10s;客户端收到传输失败被迫重连,重连后自动拉取,页面显示"结算中";重投循环按同 seq 下发 ✓ |
| W2 | scene 已 durable 应用,guild 终结前崩溃 | 重投同 seq → scene 只读答复 APPLIED durable → 终结一次 ✓ |
| W3 | 终结事务 COMMIT 结果不明 | Loop 视为 store 错误,下轮再投;`status=?P` 的 CAS 保证资金只加一次 ✓ |
| W4 | 同步投递与重投同时处理一行 | 插入租约 10s > 同步预算 2.5s;循环租约 30s > 批 10×2.5s。即便真撞上,scene 串行、只应用一次,CAS 只有一边生效 ✓ |
| W5 | 截止时刻与迟到的应用赛跑 | scene 在同一 loop 线程串行;Abort 发现已应用就回 APPLIED → 按 APPLIED 分支终结 ✓ |
| W6 | 玩家带着未结算捐献下线 | Debit/Abort 都回 NOT_HERE,退避到 60s 一次;上线时已过截止 → 发 Abort → ABORTED、次数退回;**不会**上线后突然扣钱 ✓ |
| W7 | 未结算时离帮或被踢 | 截止提前到 now;scene 未扣 → ABORTED;已扣 → APPLIED + 退款指令 + 次数退回;资金、帮贡都不记 ✓ |
| W8 | 未结算时帮会解散 | 同 W7,对每个成员生效;退款指令在帮会删除后照常投递 ✓ |
| W9 | 两位长老同时升级 | T-U 锁帮会行串行;第二个请求 `expected_level` 不符 → 成功、无改动 ✓ |
| W10 | T-D 04:59:59 占 20260915 行,05:00 后被拒 | 退回时按行里的 period_key 减旧日的行,新日的行不受影响 ✓ |
| W11 | 背包长期满 | 商店行一直 PENDING,不退款;第二档领取不会挤掉新行;达 16 行后新兑换回 GuildAssetPending ✓ |
| W12 | 退款被 16 行未决 CREDIT 挡住 | 捐献行保持 PENDING,每次租约到期后重试;CREDIT 行消化后插入退款;计 refund_blocked ✓ |
| W13 | 结算后缓存失效失败 | 帮贡、资金直读 MySQL;`GuildInfo.funds` 最坏滞后一个缓存 TTL(30m),打 ERROR ✓ |
| W14 | 提交后传输失败,玩家重连再点一次 | 会生成新指令(不加唯一键,D-14 每表至多一个 UNIQUE);重连后页面自动拉取,并显示上一笔的待结算状态或最近结果,降低误点概率。残余风险接受 |
| W15 | data_service 回档玩家 | 与 S4 C10 相同:接受 + 审计。运维手册(随 B5 写进 guild-phase2.md)要求回档前导出该玩家 `guild_asset_op` 中 `updated_ms >` 快照时间的 APPLIED 行,人工补偿。不改 data_service 代码注释 |

<!-- s5_economy_part7.md -->

# S5 经济(B5)— 第 7 部分:客户端(mmorpg-client)

所有坐标沿用 `QdaoUguiFactory.CreateRect` 的左上角锚点、y 轴向下(`GuildWindow.cs:42-77`);`_body` 为 1508×610。控件名必须稳定且唯一(Render 按名字恢复焦点)。**B2 也在改 `GuildClient.cs`、`GuildWindow.cs`,本节以 B2 落地后的版本为基线写 diff。**

## 5.33 生成与消息号

- `tools/gen_messageids.ps1` 白名单(`:35-43` 帮会段)追加 5 项:`"GuildServiceGetGuildDonateOptions" = "GetGuildDonateOptions"`、`"GuildServiceDonateToGuild" = "DonateToGuild"`、`"GuildServiceUpgradeGuild" = "UpgradeGuild"`、`"GuildServiceGetGuildShop" = "GetGuildShop"`、`"GuildServiceBuyGuildShopGoods" = "BuyGuildShopGoods"`。该脚本有聚宝斋会话的未提交改动,只在帮会段内追加。
- `tools/gen_proto.ps1` 不改。客户端不生成 `asset_error` 枚举,按 `AttributeClient.cs:103` 的先例,在 `GuildClient` 里定义私有数字常量:`AssetCurrencyInsufficient = 27000, AssetBagFull = 27001, AssetInBattle = 27002, AssetFrozen = 27003, AssetBlocked = 27005, AssetPlayerNotHere = 27006`(数值按契约 §4 的组内顺序;B4a 导表后用 `rg -n "kAsset" generated` 核对)。

## 5.34 `Assets/Scripts/Game/Guild/GuildClient.cs`

**新增状态与事件**:
```csharp
public GetGuildDonateOptionsResponse Donations { get; private set; }
public GetGuildShopResponse Shop { get; private set; }
public bool DonationsQueued { get; private set; }   // 推送要求重拉捐献页
public bool ShopQueued { get; private set; }        // 推送要求重拉商店页
/// <summary>捐献/兑换/退款可能改变了背包或货币;UI 层据此重拉背包(scene 无余额推送)。</summary>
public event Action AssetsChanged;
// 资金是否足够交给服务端判(其他长老的 Info.Funds 可能是旧值)
public bool CanUpgrade => Info != null && (Role == 1 || Role == 3) && Info.UpgradeCostFunds > 0;
private HashSet<ulong> _pendingDonationIds = new HashSet<ulong>(), _pendingShopIds = new HashSet<ulong>();
```
`Reset()` 以及 `Apply()` 检测到 `GuildId` 变化时:`Donations = null; Shop = null; DonationsQueued = ShopQueued = false; _pendingDonationIds.Clear(); _pendingShopIds.Clear();`。

**改 B2 的 `HandleGuildChanged`**:算出 `mine`、`aboutMe` 之后、if 链之前插入
```csharp
bool asset = aboutMe && (change.Kind == GuildChangeKind.FundsChanged || change.Kind == GuildChangeKind.DeliveryDone);
if (asset)
{
    AssetsChanged?.Invoke();                                   // 含已离帮后的捐献退款到账
    if (change.Kind == GuildChangeKind.FundsChanged) DonationsQueued |= Donations != null;
    else ShopQueued |= Shop != null;
}
```
并把 if 链末尾的 `else return;` 改为 `else if (!asset) return;`。`mine` 分支仍按 B2 的规则置 `RefreshQueued`,总览的资金和帮贡随之刷新。

**改 B2 的 `DrainQueued`**(每个分支发一个请求就 return):
```csharp
if (Busy || RequiresReconnect || !_net.IsReady) return;
if (RefreshQueued) { Refresh(); return; }
if (ApplicantsQueued && applicantsVisible && IsOfficerOrLeader) { ApplicantsQueued = false; LoadApplications(); return; }
if (MyApplicationsQueued) { MyApplicationsQueued = false; LoadMyApplications(); return; }
if (DonationsQueued) { DonationsQueued = false; if (Donations != null) RefreshDonations(); return; }
if (ShopQueued) { ShopQueued = false; if (Shop != null) RefreshShop(); }
```

**新方法**(全部走现有 `Request<T>`,一次只有一个在途;不引入 LINQ):
```csharp
public void RefreshDonations() => RefreshDonations(null);       // Info == null → Reject("请先加入帮会。")
private void RefreshDonations(string keepStatus)
  → MessageIds.GetGuildDonateOptions;回包:if (!Accept(...)) return;
     string settled = null;
     foreach (id in _pendingDonationIds) 若 response.PendingDonations 中没有 id:
         settled ??= DonationResultText(在 response.RecentResults 里按 OpId 找到的视图,找不到传 null);
     _pendingDonationIds = 本次 PendingDonations 的 OpId 集合;Donations = response;
     Status = keepStatus ?? settled ?? (PendingDonations.Count > 0
         ? $"{Count} 笔捐献结算中:" + AssetReasonText(PendingDonations[0].ReasonTipId) : "捐献信息已更新");
     if (settled != null) AssetsChanged?.Invoke();
public void Donate(uint donateId)
  → MessageIds.DonateToGuild;回包:
     if (response.Guild != null && response.Guild.GuildId != 0) Apply(response.Guild);
     if (!Accept(response.ErrorMessage)) { if (tip == KGuildAssetPending) DonationsQueued = Donations != null; return; }
     d = response.Donation;
     APPLIED → text = $"捐献成功:帮贡 +{d.ContributionGain},帮会资金 +{d.FundsGain:N0}";AssetsChanged?.Invoke();
     PENDING → _pendingDonationIds.Add(d.OpId);
               text = d.ReasonTipId == AssetCurrencyInsufficient ? "余额不足,正在确认结算结果" : "捐献结算中:" + AssetReasonText(d.ReasonTipId);
     然后 RefreshDonations(text)(回调里 Busy 已复位,与 B2 中成功后 Refresh 的写法相同)
public void Upgrade()                                           // !CanUpgrade → Reject("仅帮主或长老可升级;帮会已满级时不可升级。")
  → MessageIds.UpgradeGuild,ExpectedLevel = Info.Level;回包:
     if (response.Guild != null && response.Guild.GuildId != 0) Apply(response.Guild);   // 资金不足时也带最新资金
     if (!Accept(response.ErrorMessage)) return;
     Status = response.Guild.Level > oldLevel ? $"帮会已升至 Lv.{response.Guild.Level}" : "帮会等级已是最新";
public void RefreshShop() => RefreshShop(null);
private void RefreshShop(string keepStatus)                     // 与 RefreshDonations 对称:_pendingShopIds、RecentOrders、ShopResultText
public void Buy(uint goodsId, uint count = 1)
  → MessageIds.BuyGuildShopGoods;回包:if (!Accept(...)) { if (tip == KGuildAssetPending) ShopQueued = Shop != null; return; }
     APPLIED → text = "兑换成功,物品已放入背包";AssetsChanged?.Invoke();
     PENDING → _pendingShopIds.Add(order.OpId);text = "兑换已受理:" + AssetReasonText(order.ReasonTipId);
     然后 RefreshShop(text)
```

**文案函数**(`public static`,窗口复用):
- `AssetReasonText(uint id)`:27000 "银两或灵石不足";27001 "背包已满,腾出空间后自动发放";27002 "战斗中暂不结算,战斗结束后自动继续";27003 "角色迁移中,稍后自动继续";27005 "该物品或货币暂被限制";27006 "正在确认角色位置,稍后自动继续";0 "正在结算,稍后自动完成";其它 "稍后自动继续"。
- `DonationResultText(GuildDonationView v)`:null → "有捐献已结算,请查看背包与帮会资金。";APPLIED → `$"捐献已入账:帮贡 +{v.ContributionGain},帮会资金 +{v.FundsGain:N0}"`;REJECTED → "捐献未成功:" + AssetReasonText(v.ReasonTipId);ABORTED → "捐献超时未结算,已撤销,次数已退回。"。
- `ShopResultText(GuildShopOrderView v)`:null → "有兑换已结算,请查看背包。";APPLIED → "兑换的物品已发放,请查看背包。";REJECTED → "兑换失败,帮贡与限购已退回:" + AssetReasonText(v.ReasonTipId);ABORTED → "兑换已撤销,帮贡与限购已退回。"。

**`Accept` 追加**(`guild_error` 生成的 C# 枚举名按 `KGuildXxx` 规则):
| 枚举 | 文案 |
|---|---|
| KGuildZoneMerging | 区服合并维护中,请稍后再试。 |
| KGuildRankTooLow | 当前职位无权进行此操作。 |
| KGuildFundsInsufficient | 帮会资金不足,暂时无法升级。 |
| KGuildMaxLevel | 帮会已达最高等级。 |
| KGuildDonateLimit | 今日该项捐献次数已用完,明日 05:00 重置。 |
| KGuildCurrencyInsufficient | 银两或灵石不足,无法捐献。 |
| KGuildAssetPending | 还有未结算的帮会操作,请稍后再试。 |
| KGuildAssetRejected | 资产结算失败,本次操作已撤销。 |
| KGuildShopGoodsNotFound | 该商品已下架,请刷新商店。 |
| KGuildShopLevelTooLow | 帮会等级不足。 |
| KGuildShopLimit | 已达限购数量。 |
| KGuildContributionInsufficient | 可用帮贡不足。 |

## 5.35 `Assets/Scripts/UI/Ugui/Guild/GuildWindow.cs`

**新事件**:`public event Action DonationsRequested, ShopRequested, UpgradeRequested; public event Action<uint> DonateRequested, ShopBuyRequested;`

**刷新按钮**(`RefreshGuild` 回调):`Page == Donate` → `DonationsRequested`;`Page == Shop` → `ShopRequested`;其余不变。

**自动拉取**:字段 `bool _autoDonations, _autoShop`,在 `Render()` 的**最后一行**(焦点恢复之后)调用;放在最后,是因为事件会同步触发 `Changed → Render`,嵌套重建要发生在本次构建完成之后。
```csharp
private void MaybeAutoRequest()
{
    if (_client == null || _client.RequiresReconnect) { _autoDonations = _autoShop = false; return; }   // 重连后允许再拉一次
    if (_client.Donations != null) _autoDonations = false;
    if (_client.Shop != null) _autoShop = false;
    if (_client.Busy || _client.Info == null) return;
    if (Page == GuildPage.Donate && _client.Donations == null && !_autoDonations) { _autoDonations = true; DonationsRequested?.Invoke(); }
    else if (Page == GuildPage.Shop && _client.Shop == null && !_autoShop) { _autoShop = true; ShopRequested?.Invoke(); }
}
```

**Render 分支**:`GuildPage.Donate → RenderDonate()`;`GuildPage.Shop → RenderShop()`;Activities 仍调 `RenderUnavailable`(B6)。

### 5.35.1 RenderDonate(三面板几何不变)

`Info == null` → 调 `RenderOverview()` 后返回(与 `RenderMembers` 一致)。
- 标题 `Text(_body, "帮会捐献", 12, 0, 1450, 64, 42)`;副标题 `(12, 80, 1450, 58, 30, Muted)`:"聚沙成塔,同心兴帮。每日 05:00 重置次数。"
- `Donations == null`:三个 `GuildField` 照画;中间写"正在读取捐献信息…"(Busy 时)或"点击右下角刷新读取捐献信息。",不画按钮。
- 面板 i(x = i×512):`GuildField("GuildDonatePanel_" + i, x, 166, 484, 395)`;图标 `GuildIcon(key, x+24, 186, 72)`(furnace / crest / scroll);标题 `(x+110, 186, 350, 56, 34)`:"银两捐献" / "灵石捐献" / "建设物资"。
- 面板 0 放 `options` 中 `currency_type==0` 的行(≤2),面板 1 放 `currency_type==1` 的行(≤2)。第 j 个选项(j=0,1)`y0 = 262 + j×104`:
  - `Text(x+24, y0, 436, 40, 27)`:"银两大捐 · 100,000 银两"
  - `Text(x+24, y0+40, 436, 36, 24, Muted)`:"帮贡 +120 · 资金 +12,000 · 今日 1/2"(未解锁时改为 "帮会 Lv.N 解锁")
- 按钮 y=484:两个选项 → `(x+24, 484, 212, 64)`、`(x+248, 484, 212, 64)`;一个选项 → `(x+66, 484, 352, 64)`。名字 `"GuildDonate_" + donate_id`;标签取选项名去掉前两个字("小捐"/"大捐";灵石行固定为"捐献");`primary: true`;`enabled: unlocked && used_today < daily_limit && !Busy`。点击 → `Confirm("确认捐献", $"{name}:消耗 {cost:N0} {货币名},获得帮贡 +{gain},帮会资金 +{funds:N0}。", () => DonateRequested?.Invoke(id))`。
- 面板 2:"建设物资",描述 `(x+34, 270, 416, 120, 28, Muted, wrap)` "物资捐献将在后续版本开放。",按钮 `NamedButton("GuildUnavailable_Donate_2", "暂未开放", x+66, 484, 352, 64, null, enabled:false)`(保留旧名,测试沿用)。
- 页脚 `(14, 574, 1480, 35, 27, Muted)`,按优先级:有待结算 → "{N} 笔捐献结算中,完成后自动入账";否则有 `RecentResults` → "最近一笔:" + `DonationResultText(RecentResults[0])`;否则 → "可用帮贡 {balance:N0}(累计 {total:N0}) · 帮会资金 {Info.Funds:N0}"。

### 5.35.2 RenderShop

`Info == null` → `RenderOverview()` 后返回。字段 `int _shopCategory = 1, _shopPage = 0`(`ResetSession` 时复位);`public const int GoodsPerPage = 6`。
- 分类按钮 y=0:`NamedButton("GuildShopCategory_" + c, 名, (c-1)×315, 0, 295, 66, …, primary: c==_shopCategory, fontSize: 27)`,名依次为 修行补给 / 帮会珍藏 / 节庆好礼;切换时 `_shopPage = 0; Render()`。
- 余额 `Text(960, 3, 548, 58, 29, Gold, alignment: MidlineRight)`:"可用帮贡 {balance:N0}"。
- `Shop == null` → 居中提示,写法同捐献页,返回。
- 本分类商品(服务端已排序)分页显示,卡片 k(0..5):`col = k % 3, row = k / 3`,`x = col×512`,`y = 86 + row×230`:
  - `GuildField("GuildShopCard_" + id, x, y, 484, 218)`;图标 `GuildIcon(pill|talisman|scroll 按分类, x+20, y+20, 72)`;
  - 名称 `(x+108, y+14, 356, 48, 30)`;物品行 `(x+108, y+62, 356, 36, 24, Muted)`:"物品 #{item_id} ×{item_count}";
  - 价格 `(x+20, y+104, 444, 40, 27, Gold)`:"帮贡 {cost:N0}";
  - 限购 `(x+20, y+150, 270, 36, 24, Muted)`:未解锁 "帮会 Lv.{n} 解锁";`limit_period=1` "今日 {used}/{limit}";`=2` "本周 {used}/{limit}";`=0` "不限购";
  - 按钮 `NamedButton("GuildShopBuy_" + id, "兑换", x+300, y+146, 164, 58, …, primary: true, enabled: unlocked && (limit_period==0 || used<limit) && balance >= cost && !Busy, fontSize: 27)` → `Confirm("确认兑换", $"消耗帮贡 {cost:N0} 兑换 {name} ×{item_count}?", () => ShopBuyRequested?.Invoke(id))`(本期每次 1 份)。
- 分页 `Pager(_body, "Shop", _shopPage+1, pages, …, true)`(y=545,x≥820)。左侧 `(0, 545, 800, 64, 26, Muted)`:有待发放 → "待发放 {N} 单:" + 第一单的原因文案;否则有 `RecentOrders` → "最近一单:" + `ShopResultText(RecentOrders[0])`;否则留空。

### 5.35.3 总览(`RenderOverview`,当前 `:173-215`,以 B2 改后的版本为基线)

- `OverviewMetric` 签名改为 `(string name, string label, string value, float x, int valueFontSize = 43)`,两处宽度 460 → 350;4 列 x = 0 / 377 / 754 / 1131(分隔线仍按 `x-24` 规则):
  - `GuildMemberCount` "帮会成员";`GuildOnlineCount` "当前在线";
  - **`GuildFunds`** "帮会资金" = `info.Funds.ToString("N0")`;
  - **`GuildMyContribution`**(沿用旧名)标签 "可用 / 累计帮贡",值 `balance:N0 + " / " + total:N0`,`valueFontSize: 34`(14 个字符约 270px,放得进 350)。取值由 B2 的 `member.ContributionTotal` 扩展为同时取 `ContributionBalance`。
- 公告行:"帮会公告"标题宽 620→590;`_client.IsOfficerOrLeader` 时新增 `NamedButton("UpgradeGuild", Info.UpgradeCostFunds == 0 ? "已满级" : "升级帮会", 680, 240, 258, 64, ShowUpgrade, enabled: _client.CanUpgrade && !Busy, fontSize: 27)`(右端 938,不压 958 处的"查看全文")。
- `ShowUpgrade()` → `Confirm("升级帮会", $"需要帮会资金 {cost:N0}(当前 {funds:N0})。\n升级后帮会升至 Lv.{level+1},成员上限提升。", () => UpgradeRequested?.Invoke())`。

## 5.36 `GuildUiRoot.cs`

`Awake` 事件接线(与 B2 §19 同一写法,带 `_available` 守卫):`DonationsRequested → _client?.RefreshDonations()`、`DonateRequested → _client?.Donate(id)`、`UpgradeRequested → _client?.Upgrade()`、`ShopRequested → _client?.RefreshShop()`、`ShopBuyRequested → _client?.Buy(id)`。
`Update` 里创建/销毁 `_client` 的地方(`:72-78`)订阅/退订 `AssetsChanged += OnAssetsChanged`;`private static void OnAssetsChanged() { var f = PlayerFeaturesClient.Instance; f?.RequestBag(f.RequestedBagType); }`(`PlayerFeaturesClient.cs:14,35,56`)。

## 5.37 货币改名

- `Assets/Scripts/UI/Ugui/Gameplay/GameplayWindow.cs:155`:改为 `{ "银两", "灵石", "绑定灵石" }`。
- `Assets/Scripts/Game/Attribute/AttributeClient.cs:103`、`Assets/Scripts/Game/Pet/PetClient.cs:101`:"金币不足" → "银两不足";两个文件里的注释"扣金币"同步改为"扣银两"。

<!-- s5_economy_part8.md -->

# S5 经济(B5)— 第 8 部分:测试(EditMode / Go / C++)与 robot 冒烟

## 5.38 EditMode 测试(`Assets/Tests/EditMode/Guild/GuildUiTests.cs`)

`GuildClientTests` 追加(推送用 B2 测试里已有的通知注入方式构造 `GuildChangedS2C`):
1. `DonateSendsOptionIdAndAppliesReturnedGuild`:回 APPLIED + Guild(Funds 12000)→ `Info.Funds == 12000`,AssetsChanged 触发 1 次,随后发出 GetGuildDonateOptions。
2. `PendingDonationIsNotAnError`:回 PENDING、reason 27002 → Status 含"战斗中",AssetsChanged 未触发,Info 未清空。
3. `RejectedDonationShowsCurrencyText`:error=KGuildCurrencyInsufficient → Status 为对应文案,不发跟随刷新。
4. `UpgradeIsSentByOfficerEvenWhenCachedFundsLookLow`:Role 0 不发请求;Role 1、`Funds < UpgradeCostFunds` 时照样发请求,且 `ExpectedLevel == Info.Level`;回 KGuildFundsInsufficient 并带 Guild(Funds 30000)→ `Info.Funds == 30000`,Status 为资金不足文案。
5. `FundsChangedPushQueuesDonationRefreshAndRaisesAssetsChanged`:已有 Donations 快照,推送 `{FundsChanged, target=本人}` → AssetsChanged 1 次、`DonationsQueued`;连调两次 `DrainQueued` → 先发 GetPlayerGuild,再发 GetGuildDonateOptions。
6. `DeliveryDonePushWhileNotInGuildRaisesAssetsChanged`:`Info == null`,推送 `{DeliveryDone, target=本人}` → AssetsChanged 1 次,不排任何请求(已离帮后的退款到账)。
7. `SettledDonationShowsRecentResult`:第一次 RefreshDonations 返回待结算 op 11;第二次待结算为空、`recent_results=[op 11 REJECTED 27000]` → Status 含"捐献未成功"与"不足",AssetsChanged 1 次。
8. `SettledShopOrderRaisesAssetsChanged`:待发放 op 21 → 下次不在列表中、`recent_orders=[op 21 APPLIED]` → Status "兑换的物品已发放…",事件 1 次。
9. `GuildChangeClearsDonationAndShopSnapshots`。

`GuildWindowTests`:
- `UnsupportedActionsAreClearlyDisabled` 只保留 `[TestCase(GuildPage.Activities)]`。
- `DonatePageRendersOptionsAndKeepsMaterialsDisabled`:fixture 3 行 → 存在 `GuildDonate_1/2/3`;`GuildUnavailable_Donate_2` 不可交互;`used_today == daily_limit` 的按钮不可交互。
- `DonatePageAutoRequestsAgainAfterReconnect`:Page=Donate、`RequiresReconnect=true` → 不触发 `DonationsRequested`;恢复连接(Donations 仍为 null)后 Render → 触发 1 次;再 Render → 不重复触发。
- `ShopPageFiltersByCategoryAndPages`:11 件 fixture → 分类 1 有 4 个 `GuildShopBuy_*`,`ShopNext` 不可交互;点 `GuildShopCategory_2` 后出现 `GuildShopBuy_201`。
- `LockedOrUnaffordableGoodsCannotBeBought`。
- `OverviewShowsFundsAndUpgradeOnlyForOfficers`:Role 0 无 `UpgradeGuild`;Role 1 且资金不足时 `UpgradeGuild` **可交互**;`UpgradeCostFunds == 0` 时文字为"已满级"、不可交互;`GuildFunds` 文本 "12,000";`GuildMyContribution` 文本 "440 / 440"。
- `MemberPaginationAndOnlineFilterConsumeTheRealSnapshot` 若 B2 已改名字显示,则不动。

`Assets/Editor/Guild/GuildUiVerification.cs` 的 `Fixture()` 补 Donations/Shop 快照,供离线截图(可选,不计手改名额)。

## 5.39 Go 与 C++ 测试

| 文件 | 类型 | 用例 |
|---|---|---|
| `go/shared/gameday/gameday_test.go` | 单测 | §5.14 全部固定用例 |
| `go/guild/internal/logic/economy_config_test.go` | 单测 | §5.10 默认行通过;逐条破坏各一例:currency_type=2、同货币 3 行、daily_limit=0、item_count=0、物品 12(堆叠 1)配 item_count=2、物品 15 配 item_count=1000、limit_period=0 但 limit_count=3、required_guild_level=6、物品不存在、category=4 → 错误信息含表名与行 id;`MaxBuyCount`:101→20、202→1、203→20;`validateGuildTables` 追加规则:deadline=59、retry_base=99 各拒一次 |
| `go/guild/internal/logic/economy_logic_test.go` | 单测(假 repo / 假 Loop / B2 的假 GuildNotifier) | 无会话 → PermissionDenied;不在帮 → ErrNotInGuild;donate_id 不存在 → GuildAssetRejected;repo 回 ErrZoneMerging → GuildZoneMerging;ErrEconomyBusy → GuildAssetPending(非 gRPC 错误);OpIDs=nil → ErrIDGenUnavailable;回读 PENDING/27002 → 无 error、视图 PENDING;REJECTED/AssetReasonCurrencyInsufficient → GuildCurrencyInsufficient;REJECTED/kAssetBlocked → GuildAssetRejected;剩余预算 <300ms → ProcessOne 未调用、计数 +1;升级 expected_level 不符 → 成功、未推送;升级成功 → notifier 收到除操作者外的全员、kind=LEVEL_UP;升级资金不足 → 回包 guild 非空;商店 count=21 → GuildShopLimit;goods 202 count=2 → GuildShopLimit;长老发起写 RPC → 回包 `pending_application_count` 非 0(证明用了 guildInfoFor);`OnAssetFinalized`:带同步标记不推;DONATE 推 FUNDS_CHANGED 给本人;DONATE 且 Refunded 不推;SHOP / ACTIVITY_REWARD / DONATE_REFUND 推 DELIVERY_DONE;未知 kind 不推 |
| `go/guild/internal/config/config_test.go`(无则新建) | 单测 | 默认 AssetOp 块通过;`LeaseMs=20000, Batch=10, OpBudgetMs=2500` → Validate 报错;`ReconcileBatch=101` 报错 |
| `go/guild/internal/data/economy_repo_test.go` | 需 `GUILD_TEST_MYSQL_DSN`(用 B1 的 schemamigrate 建表助手),未设则 Skip | 见下 |
| `go/guild/internal/logic/economy_flow_integration_test.go` | `//go:build integration`,同 DSN | 见下 |
| `cpp/tests/currency_test/asset_op_system_test.cpp` | gtest | `TEST_F(AssetOpSystemTest, GuildCreditAcceptsDonateRefundTx)`:Credit(GUILD_CREDIT, seq 1, 银两 100, TX_GUILD_DONATE_REFUND)→ APPLIED、余额 +100;Debit(GUILD_DEBIT, tx=TX_GUILD_DONATE_REFUND)→ UNKNOWN + kAssetInvalidBundle |

`economy_repo_test.go` 用例:
1. `TestReserveDonationDailyLimitConcurrent`:同一玩家 10 个协程并发 `ReserveDonation(daily_limit=5)` → 恰好 5 个成功、5 个 `ErrEconomyDailyLimit`;seq 为 1..5 且不重复。
2. `TestReserveConcurrentFirstTimeAdjacentPlayers`:50 个无任何历史、player_id 连续的玩家同在一帮,并发各做一次 ReserveDonation 和一次 ReserveShopOrder → 100 次全部成功,0 个 `ErrEconomyBusy`,测试钩子 `economyTxRetries` 为 0(若 >0 只记录,不判失败;判失败的是出现错误)。
3. `TestFinalizeDonationAppliedOnce`:Finalize(APPLIED) 两次 → 第一次 true、第二次 false;funds +12000、total/balance +120 各只加一次。
4. `TestFinalizeDonationRejectedRollsBackCounter`:占 1 次后 REJECTED → used_count 为 0、funds 不变、`reason_tip_id` 与 `last_reason` 都等于 AssetReasonCurrencyInsufficient。
5. `TestFinalizeDonationMemberLeftRefunds`:删掉成员行后 APPLIED → 捐献 op 为 APPLIED;funds 与帮贡不变;新增一行 kind=DONATE_REFUND、stream=2、status=PENDING、tx_type=TX_GUILD_DONATE_REFUND、payload 与原行相同、next_attempt_ms ≤ now;used_count 退回;`OnFinalized` 收到 `Refunded=true`。
6. `TestFinalizeDonationGuildGoneRefunds`:删掉帮会与成员行后同上。
7. `TestFinalizeDonationRefundBlocked`:先插 16 行未决 CREDIT → Finalize 返回 `ErrRefundBlocked`,捐献 op 仍为 PENDING,没有退款行。
8. `TestReserveShopOrderChecks`:等级不足 / 余额不足 / 限购(limit 3,先买 2 再买 2)/ 不限购不写计数器行,各一例;闸门返回封锁 → ErrZoneMerging 且无任何写入。
9. `TestFinalizeShopRejectedRefunds`、`TestFinalizeShopAbortedRefunds`:balance 退回、used_count 退回;成员行已删时不退帮贡,op 仍终结。
10. `TestFinalizeActivityRewardAndRefundOnlyCAS`:ACTIVITY_REWARD 的 APPLIED/REJECTED、DONATE_REFUND 的 APPLIED → guild/member/counter 都不变;DONATE_REFUND REJECTED → orphan 计数 +1。
11. `TestTooManyPendingGuard`:插 16 行未决 → 第 17 次返回 `ErrEconomyTooManyPending`。
12. `TestClaimDueLeaseExclusive`:两个令牌先后领同一批到期行 → 第二次 0 行;用错误令牌 Reschedule → RowsAffected=0。`TestClaimDuePrefersFreshRows`:30 行 attempts=10 且更早到期,加 5 行 attempts=0,batch=10 → 领到 5 行新行 + 5 行老行。
13. `TestUpgradeGuild`:资金不足 / 职位不足 / 满级 / 成功(level 2、funds 扣 20000、max_members 60)/ 两协程并发都带 expected_level=1 → 只升一级 / 闸门封锁 → ErrZoneMerging 且无改动。
14. `TestLeaveKickDisbandAcceleratePendingDonation`:未决捐献 deadline=now+600s → 分别经 LeaveGuild、KickMember、DisbandGuild 后 `deadline_ms ≤ now`;同一玩家的 SHOP 行和别帮的 DONATE 行不受影响。
15. `TestCleanupDeletesOnlyOldTerminalRows`:31 天前的终态行删除;31 天前的 PENDING 行保留;1 天前的终态行保留;40 天前的日键与周键计数行删除,当天的保留。
16. `TestRecentResultsWindow`:11 分钟前终结的行不返回;最多 5 条,新的在前。

`economy_flow_integration_test.go`(真 MySQL + 假 Applier,驱动 `Loop.Tick`):
- ① RETRY → RETRY → APPLIED durable,Tick 3 次 → 资金只加一次;
- ② 假时钟越过 deadline → 下一次调用是 Abort,回 REJECTED reason 0 durable → ABORTED、次数退回;
- ③ APPLIED 但未 durable → AwaitDurable 后 500ms 重排;
- ④ 商店连续 10 次 RETRY(27001)→ 仍 PENDING,帮贡未退;
- ⑤ 捐献 RETRY 后调 LeaveGuild → 下一次 Tick 的 RPC 为 Abort;假 Applier 回 APPLIED durable → 生成退款行 → 再 Tick 对退款行调 Credit 并回 APPLIED → 退款行 APPLIED,`OnFinalized` 依次收到 DONATE(Refunded)与 DONATE_REFUND;
- ⑥ 租约:假 Applier 每行睡 2.5s,10 行到期,`LeaseMs=30000`,两个 Loop 并发 Tick → 每个 op 的 `Do` 恰好被调用一次。

## 5.40 robot 冒烟(`GUILD_ECONOMY_SMOKE_OK`)

**账号**:A=`robot_9214`(帮主)、B=`robot_9215`(成员)。契约 §6 的段是 9211–9219:B2 用 9211–9213,S6 用 9216–9219,B5 取剩下的 9214–9215。两个账号都须**首次在 zone_a 建角**(同 B2 §冒烟头注释),登录列表加进 `sc.ZoneA`。

**前置**:按"本地开机 runbook"起全栈;scene 经 `cpp_nodes.ps1` 启动时运行模式兜底 dev,GM 客户端消息放行(**不是**靠 `MMORPG_ALLOW_CLIENT_GM`,该变量不存在;见 `cpp/nodes/gate/SECURITY.md` §3);B2 的申请/审批冒烟已通过。

**改动**:新文件 `robot/guild_economy_smoke.go`;`robot/config/config.go` 的 `GuildSmokeConfig` 加 `Economy bool yaml:"economy"`、`EconomyLeader string yaml:"economy_leader"`(默认 `robot_9214`)、`EconomyMember string yaml:"economy_member"`(默认 `robot_9215`);`robot/etc/guild_smoke.yaml` 加 `economy: true`;`guild_smoke_scenario.go` 的 `guildSmokeIsGuildMessage` 加 5 个新消息号,`RunGuildSmoke` 末尾加 `if sc.Economy { runGuildEconomySmoke(cfg, stats) }`。GM 助手 `econGmAdd`/`econGmDeduct(bot, currencyType uint32, amount int64)` 照 `gmAddGold`(`currency_crash_window_scenario.go:77-97`)分别发 `GmAddCurrencyRequest`/`GmDeductCurrencyRequest`;查背包照 `features_smoke_scenario.go:451`。原因码比较用 `table.AssetError_kAssetCurrencyInsufficient`,不写 27000。

**共同步骤**(任一步不符,就打印步骤号与实际值,然后 `os.Exit(1)`):
1. A、B 登录进场,各自 `leaveAnyGuild`。A `CreateGuild`(名为 `经济测` + nonce)→ Level 1、Funds 0、MaxMembers 50。B `ApplyJoinGuild`,A `ReviewGuildApplication(approve)` → B 入帮。
2. A 调 `GetGuildDonateOptions`(3 个选项、pending 为空)与 `GetGuildShop`(11 件)。**模式判定**:A 的 `used_today[2]==0 && used_today[3]==0 && used_count[101]==0 && used_count[301]==0` → `full`,否则 → `degraded`。打印 `economy mode=…`。
3. 查 B 背包:灵石 > 0 时 `econGmDeduct(B, 1, 余额)` 清零。

**full 模式**:
4. A 记下背包初值;GM 加银两 300,000、灵石 100。
5. A `DonateToGuild(2)`:若为 PENDING,每 500ms 调一次 `GetGuildDonateOptions`,直到 pending 为空且 `recent_results[0].status == APPLIED`(上限 10s)。然后 `GetPlayerGuild` → Funds 12000,本人 total/balance 均为 120。
6. A 再捐 `(2)` → Funds 24000;第三次 → `GuildDonateLimit`,Funds 不变。
7. A `DonateToGuild(3)` → Funds 44000、balance 440;背包银两 = 初值 + 300000 − 200000,灵石 = 初值 + 100 − 100。
8. B `DonateToGuild(3)` → `GuildCurrencyInsufficient`、donation.status 为 REJECTED;B 的 `GetGuildDonateOptions` 中 id 3 的 used_today=0,且 `recent_results[0]` 为 REJECTED、原因为货币不足。
9. B `UpgradeGuild(expected 1)` → `GuildRankTooLow`,回包 guild 非空。A `UpgradeGuild(expected 1)` → Level 2、Funds 24000、MaxMembers 60;A 再调一次 `UpgradeGuild(expected 1)` → 无 error,Level 仍为 2,Funds 仍为 24000。
10. A `GetGuildShop` → 103 已解锁、104 未解锁;balance 440;goods 202 的 `max_buy_count == 1`。
11. A `BuyGuildShopGoods(101, 1)` → order APPLIED(若 PENDING,就轮询 `GetGuildShop` 至 pending 为空);balance 410;背包中物品 15 数量 +5。
12. A `BuyGuildShopGoods(104, 1)` → `GuildShopLevelTooLow`;B `BuyGuildShopGoods(101, 1)` → `GuildContributionInsufficient`;A `BuyGuildShopGoods(202, 2)` → `GuildShopLimit`。
13. A 连续 5 次 `BuyGuildShopGoods(301, 1)` 成功(balance 160),第 6 次 → `GuildShopLimit`。
14. 打印 `GUILD_ECONOMY_SMOKE_OK mode=full guild_id=… funds=24000 level=2 balance=160`;A `DisbandGuild` 清理。

**degraded 模式**(同一游戏日重跑,或 full 中途失败后重跑;计数器按玩家记,解散清不掉):
4'. A 的 `used_today[2] == 2` → A `DonateToGuild(2)` → `GuildDonateLimit`;否则打印 `SKIP step4' used_today=…`。
5'. 执行 full 模式第 8 步(可重复)。
6'. B `UpgradeGuild(1)` → `GuildRankTooLow`;A `UpgradeGuild(1)` → `GuildFundsInsufficient`,回包 guild.Funds == 0。
7'. A `GetGuildShop` → balance 0;执行 full 模式第 12 步的三个断言。
8'. 打印 `GUILD_ECONOMY_SMOKE_OK mode=degraded guild_id=…`;A `DisbandGuild`。

完整路径每个游戏日只能跑一次;Codex 在第 6 步重建开发库之后首跑必为 `full`,再跑一次应为 `degraded`。可选的手动项:第 5 步前强杀 guild 进程再重启,A 再查 → 捐献由重投循环完成、资金只加一次;scene 日志 `[AssetOp] rpc=debit … outcome=1` 对每个 seq 恰有一条非只读记录。

<!-- s5_economy_part9.md -->

# S5 经济(B5)— 第 9 部分:文件清单、Codex 验证、契约偏差

## 5.41 文件清单(三个子批次;每批开工前需用户授权)

### B5a 协议、配表、C++ 白名单、游戏日、号段登记(手改 21)

| # | 文件 |
|---|---|
| 1 | `proto/guild/guild.proto` |
| 2 | `proto/guild/guild_db.proto`(kind 追加 DONATE_REFUND;B1 缺 21–25 列/idx_3 时一并补) |
| 3 | `proto/common/rollback/transaction_log.proto`(TX_GUILD_DONATE_REFUND) |
| 4–5 | `data/GuildDonate.xlsx`、`data/schema/guilddonate_table.proto`(新) |
| 6–7 | `data/GuildShop.xlsx`、`data/schema/guildshop_table.proto`(新) |
| 8 | `data/GuildLevel.xlsx`(只改 upgrade_cost_funds 列) |
| 9 | `data/tip/Tip.xlsx`(核对/补 §5.12) |
| 10 | `data/MessageLimiter.xlsx` |
| 11–13 | `cpp/generated/table/CMakeLists.txt`、`table.vcxproj`、`table.vcxproj.filters`(登记 guilddonate_table.cpp、guildshop_table.cpp、guildshop_table_fk.cpp、两个 .pb.cc 与头) |
| 14 | `cpp/libs/services/scene/player/system/player_asset_op.cpp`(GUILD_CREDIT 白名单) |
| 15 | `cpp/tests/currency_test/asset_op_system_test.cpp` |
| 16–17 | `go/shared/gameday/gameday.go`、`gameday_test.go` |
| 18–21 | `go/data_service/internal/config/config.go`、`go/data_service/internal/store/id_segment_store.go`、`go/data_service/etc/data_service.yaml`、`tools/scripts/k8s_deploy.ps1` |

### B5b guild 服务(手改 23,另有 ≤2 个 B2 断言修正)

| # | 文件 |
|---|---|
| 1–3 | `go/guild/internal/data/economy_repo.go`、`asset_store.go`、`economy_repo_test.go` |
| 4 | `go/guild/internal/data/guild_manage_repo.go`(B2 文件:Leave/Kick/Disband 提前截止) |
| 5–9 | `go/guild/internal/logic/economy_config.go`、`economy_config_test.go`、`economy_logic.go`、`economy_logic_test.go`、`economy_flow_integration_test.go` |
| 10 | `go/guild/internal/logic/guild_logic.go`(`economy` 字段) |
| 11 | `go/guild/internal/logic/guild_manage_logic.go`(`validateGuildTables` 追加规则) |
| 12 | `go/guild/internal/logic/push.go`(`changeKindLabel` 追加) |
| 13 | `go/guild/internal/server/guild_server.go` |
| 14 | `go/guild/internal/session/session.go` |
| 15–16 | `go/guild/internal/constants/constants.go`、`constants_test.go` |
| 17–18 | `go/guild/internal/svc/asset_op.go`(新)、`servicecontext.go` |
| 19–20 | `go/guild/internal/config/config.go`、`config_test.go` |
| 21 | `go/guild/guild.go` |
| 22 | `go/guild/etc/guild.yaml`(`AssetOp` 块显式写出) |
| 23 | `go/guild/cmd/assetopfix/main.go`(新) |
| (24–25) | §5.10 核对命令命中的 B2 测试/冒烟断言(无命中则不计) |

`go/guild/go.mod` 不改:`go.etcd.io/etcd/client/v3` 已是直接依赖(`go.mod:13`)。

### B5c robot、客户端(手改 12)

`robot/guild_economy_smoke.go`、`robot/guild_smoke_scenario.go`、`robot/config/config.go`、`robot/etc/guild_smoke.yaml`(4);客户端 `tools/gen_messageids.ps1`、`GuildClient.cs`、`GuildWindow.cs`、`GuildUiRoot.cs`、`GameplayWindow.cs`、`AttributeClient.cs`、`PetClient.cs`、`GuildUiTests.cs`(8)。文档 `docs/design/guild-phase2.md` §S5(含运维节:告警规则、assetopfix 用法、回档补偿)与 `PROGRESS.md` 不计名额。

**顺序与部署**:B5a → B5b → B5c。B5a 的 4 个 data_service/k8s 文件必须与 B5b 同次或更早部署,否则 guild 取不到 `guild_asset_op` 号段,经济写 RPC 全部回 `ErrIDGenUnavailable`。B5a 单独提交时,Codex 只验证第 1–5 步;`go/guild` 编译在 B5b 完成后一起验证(proto 新增 RPC 后,若 server 桩要求实现方法,单独的 B5a 可能编不过 guild)。

## 5.42 Codex 验证(串行,工作目录 `E:\work\xuanming-server-mmo`)

0. 加载 buildenv;`git status --short proto data generated go/proto go/shared/generated cpp/generated robot tools/data_table_exporter/state > pre-b5-status.txt`,客户端仓同样存一份。并行会话(trade/team/chat)的未提交生成物在生成前后必须保持不变,有冲突先停下报告。
1. **前置核对**:逐条运行 §5.2 的核对命令;再运行 §5.10 的断言核对命令并记录命中。
2. **导表**:`dev.bat gen`(或 `py tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml`,protoc-gen-go 须在 PATH)。通过标准:`generated/tables/guilddonate.json` 3 行、`guildshop.json` 11 行、`guildlevel.json` 中 id=1 的 `upgrade_cost_funds` 为 20000;`go/shared/generated/table/guildshop_table.go` 存在;`guild_error_tip.proto` 含 `kGuildShopLimit`;运行 `py tools/data_table_exporter/tools/gen_schema_index.py` 后,`data/AGENTS.md` 索引出现两张新表。
3. **proto-gen**:`cd go; .\build.bat`。通过标准:`rg -n "GuildServiceDonateToGuild|GuildServiceGetGuildDonateOptions" proto/message_id.txt` 各 1 行;`go/proto/guild/guild.pb.go` 含 `GuildShopOrderView` 与 `RecentOrders`;`transaction_log.pb.go` 含 `TX_GUILD_DONATE_REFUND`。记下新消息号,补 MessageLimiter 行后重跑第 2 步。
4. **C++**:MSBuild Debug x64 `/m:1` 先编 `cpp/generated/table/table.vcxproj`,再编 scene 与 `currency_test`;运行 `currency_test.exe --gtest_filter=AssetOp*:CurrencyTest*` 全绿;`Select-String cpp/nodes/scene/handler/grpc/scene_node_service.cpp -Pattern AcquireCreatePermitBlocking` 必须命中(S4 E13);启动一个 scene,确认没有缺表的 `LOG_FATAL`。Linux:`build_linux.sh` 只编 table 目标。
5. **data_service 与 gameday**:`cd go/data_service; go test ./internal/config/... ./internal/store/... -count=1`;`cd go/shared; go test ./gameday/... -count=1`。
6. **开发库重建**:`SELECT @@global.binlog_format` 必须为 `ROW`(否则停下报告,RC 事务依赖它);停 guild;root 执行 `DROP DATABASE mmorpg_guild;` 与 S1 §3 的两句;data_service `-migrate`;启动 guild。通过标准:`SHOW CREATE TABLE mmorpg_guild.guild_asset_op` 含 `lease_token`、`last_reason`、`KEY idx_guild_asset_op_3 (player_id,stream,status,seq)`;全局库 `SELECT biz_tag FROM id_segment WHERE biz_tag='guild_asset_op'` 返回 1 行;guild 日志无表校验 panic,并出现 `guild.asset_op_reconcile`、`guild.asset_op_cleanup` 启动日志。
7. **Go**:`cd go/guild; go vet ./...; go test ./... -count=1`(DSN 用例应为 SKIP);`go build ./cmd/assetopfix`;设 `$env:GUILD_TEST_MYSQL_DSN`(本地 dev 账号,不写进文件)后运行 `go test ./internal/data -run "Reserve|Finalize|TooMany|ClaimDue|UpgradeGuild|Accelerate|Cleanup|Recent" -count=1 -v`,以及 `go test -tags integration ./internal/logic -run EconomyFlow -count=1 -v`。字面量守卫:`rg -n "\b2700[0-6]\b" go/guild/internal --glob "!*_test.go"` 为 0 行;`rg -n "status\s*=\s*[0-4]\b" go/guild/internal/data` 为 0 行。
8. **robot**:`cd robot; go build ./...`;若报 inconsistent vendoring,停下报告,不要擅自 `go mod vendor`(S4 E6)。第 6 步之后首次运行帮会冒烟(`guild_smoke.yaml`,`economy: true`)→ 日志出现 `GUILD_SMOKE_OK` 与 `GUILD_ECONOMY_SMOKE_OK mode=full`;立即再跑一次 → `mode=degraded`。
9. **客户端**:在客户端仓运行 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`client_compile_check.ps1`;EditMode:`Unity.exe -batchmode -projectPath E:\work\mmorpg-client -runTests -testPlatform EditMode -testFilter "GuildClientTests|GuildWindowTests" -testResults <scratchpad>\guild-b5-editmode.xml`(不带 `-quit`),结果全绿;编辑器被占用时,按"离线编译验证"备忘改走 Roslyn。
10. 保留证据:导表/生成器末 200 行、第一个编译错误的上下文、失败用例的输出、冒烟 `-v` 输出;没跑到的步骤标"未验证"。

## 5.43 契约偏差(需主设计确认)

1. **outbox 状态与列**:状态数值跟 S1 与 S4 E1(1 PENDING / 2 APPLIED / 3 REJECTED / 4 ABORTED),本节原先的 0..3 方案作废。21–25 列与 `idx_guild_asset_op_3` 推荐由 B1 建全(S4 #18);B5 额外要求 `last_reason = 25`,与 `reason_tip_id` 的分工见 §5.6。**请主设计把 25 列写回 S1,并同步 S4 E2 与 S6 契约偏差 6**。
2. **新增 RPC `GetGuildDonateOptions`**:客户端不加载配表。
3. **截止时间只用于捐献**:商店、活动奖励、退款指令 `deadline_ms=0`(S4 偏差 8)。
4. **`UpgradeGuildRequest.expected_level`**;`UpgradeGuildResponse.guild` 在业务失败时也带上。
5. **tip 复用**:捐献选项不存在 → `GuildAssetRejected`;捐献等级不足 → `GuildShopLevelTooLow`;数据库锁冲突 → `GuildAssetPending`。若要精确,建议新增 `GuildDonateNotFound`,并把 `GuildShopLevelTooLow` 改名为 `GuildLevelTooLow`。
6. **经济 RPC 不查归属区**(R4);合服闸门在事务内按 MySQL 的 zone_id 判定。
7. **离帮后捐献退款(需用户拍板的产品规则)**:"离帮、被踢、解散时尚未入账的捐献一律不入账,已扣的货币原数退回、次数退回"。为此新增 `GuildAssetOpKind.DONATE_REFUND=4`(改 S1 枚举)、`TX_GUILD_DONATE_REFUND`(改 B4a 的 proto 与 scene 白名单,放在 B5a),并改 B2 的 `guild_manage_repo.go`(三个事务各加一条 UPDATE)。若用户选择"资金照记给帮会、不退款",可删掉退款分支,只保留提前截止。
8. **货币标签**:除契约要求的"金币→银两"外,"钻石→灵石"、"绑定钻石→绑定灵石"一并改;不同意时只回退 `GameplayWindow.cs:155` 的后两项。
9. **推送范围**:捐献与兑换的异步结算只推本人(FUNDS_CHANGED / DELIVERY_DONE);退款、活动奖励也推 DELIVERY_DONE;升级推送给除操作者外的全员。
10. **`GuildRule`/`GuildLevel` 归 B2**:B5 不改 schema,也不另写校验器;只改 `upgrade_cost_funds` 列(10000/30000/80000/200000 → 20000/60000/150000/400000),`GuildRule` 截止时间沿用 B2 的 600 秒;GuildShop 204 的等级要求 6 → 5。
11. **视图字段超出契约**:`recent_results`、`recent_orders`、`max_buy_count`。
12. **重投循环参数**:Batch 10、Lease 30s、Interval 1s,并由 guild 配置强制 `Lease ≥ Batch×OpBudget + 5s`。S4 默认值 100/10s 在串行 Tick 下不安全,建议 B4b 的 `LoopConfig` 注释改为同一约束;以后可考虑在 `Tick` 里并发处理。
13. **清理任务落在 B5**:终态 op 与计数器保留 30 天。S6 §6.12 的"v1.1 再清理"由此提前,S6 的 ACTIVITY_CLAIM 计数行也会在 30 天后删除。
14. **运维工具 `assetopfix`** 与告警规则(新 cmd,放在 go/guild)。
15. **经济事务用 READ COMMITTED**;B2 的 `inTx` 仍为 RR。
16. **批次拆成 B5a/B5b/B5c**(21/23/12)。
17. **robot 账号 9214/9215**;冒烟分 full/degraded 两种模式。
18. **给 S6 的接口**:Finalize 的 ACTIVITY_REWARD 分支(只做 CAS,推 DELIVERY_DONE)、logic 包内的 `withSyncDelivery`、outbox 插入的列约定(§5.16 列序)均已提供;S6 插入时 `status` 绑定 `assetop.StatusPending`。

---

## 附录:对抗评审处理记录

# S5 经济(B5)— 对抗评审处理记录(2026-09-16)

结论:18 条全部核实成立,全部采纳。其中 4 条的修法与评审建议不同:#2 的锁等待封顶、#6 的并发方式、#8 的默认值取舍、#17 的标签写法,理由写在各条里。修订后全节共 9 部分(新增 `s5_economy_part8.md`、`s5_economy_part9.md`),章节号重排为 5.0–5.43。

核对依据:
- **代码**:`guild_repo.go:958-961`(缓存 zone 注释)、`svc/servicecontext.go:35,107,136`、`go/guild/go.mod:13`、`shared/idsegment/minter.go:28-45`(`Minter{Name,Segment,Fallback}`、`Mint`)、`logic/merge_fence.go:36`(`MergeInProgress`)、`GuildClient.cs` 的 `Request` 错误回调置 `RequiresReconnect`、`GuildWindow.cs:173-215`(总览 3 列、宽 460)、`transaction_log.proto`(当前末尾为 TX 20,24–26 由 B4a 追加)。
- **设计稿**:`s4_asset_part7.md` E1/E2、`s4_asset_part5.md`(Tick 串行)、`s2_management_part1.md` §3、`part3` §6.2a、`part3b` §7.2、`part5` §12.4、`part6` §13、`part7` §17.3、`s6_activities_part2/3/7/8`。

| # | 级别 | 问题 | 处理 | 落点 |
|---|---|---|---|---|
| 1 | 阻断 | outbox 状态编号与 S4 E1 相反;列归属冲突 | **已采纳**。状态沿用 S1 与 E1 的 1..4(UNSPECIFIED=0),删除本节原 0..3 方案;SQL 一律绑定 `assetop.Status*`(记号 ?P/?A/?R/?X),不写字面量。21–25 列与 idx_3 推荐由 B1 建全,B1 漏建时 B5a 补;写明 `last_reason`(最近一次)与 `reason_tip_id`(终态)的分工,并请主设计回写 S1、S4 E2 与 S6 偏差 6 | §5.3、§5.6、§5.43-1 |
| 2 | 主要 | RR 间隙锁使不同玩家首捐死锁;错误抛成 gRPC 错误,客户端被迫重连 | **已采纳**。T-D/T-S/T-U/Finalize/ClaimDue/清理全部改用 `sql.LevelReadCommitted`,并论证正确性只靠成员行锁和 seq 行锁;1213/1062 整事务重试,用尽后与 1205、txCtx 到期一起归为 `ErrEconomyBusy` → `GuildAssetPending`;补 50 个相邻新玩家并发的集成测试。锁等待不在事务里 SET:B2 §6.2a 已在 DSN 上设为 1s,在事务里 SET 会污染连接池,所以直接引用 B2 的设置;Codex 增加 `binlog_format=ROW` 核对 | §5.15、§5.39-2、§5.42-6 |
| 3 | 主要 | 重复声明 GuildNotifier,`notifier` 变量不存在 | **已采纳**。删除 S5 的接口和 `EconomyDeps.Notifier`,改为 B2 同款 `WithEconomy(EconomyDeps)` Option,推送统一走 `l.notify(...)`;升级推送按 B2 §14 排除操作者;`changeKindLabel` 追加三个值 | §5.24、§5.26、§5.30 |
| 4 | 主要 | 异步结算后客户端不刷新;异步 REJECTED 玩家看不到 | **已采纳**。改 B2 的 `HandleGuildChanged`:推送 FUNDS_CHANGED/DELIVERY_DONE 且目标是本人时,触发 AssetsChanged,并置 DonationsQueued/ShopQueued(已离帮也刷背包)。`DrainQueued` 依次发出。两个读 RPC 增加 `recent_results`/`recent_orders`(10 分钟内、≤5 条,沿唯一键倒序扫 20 行)。客户端按 op_id 比对"消失的待结算单",显示终态文案;`AssetReasonText` 补 27000;补 EditMode 用例 5–8 | §5.5.2、§5.21、§5.34、§5.38 |
| 5 | 主要 | 离帮/被踢/解散后照扣货币,资金作废 | **已采纳**。B2 的 Leave/Kick/Disband 事务按锁序追加"未决捐献截止提前到 now";Finalize 时若捐献者已不是绑定帮会的成员(含帮会已解散),资金和帮贡都不记,同一事务插入 `DONATE_REFUND` 退款指令并退回次数。新增 kind=4、`TX_GUILD_DONATE_REFUND` 与 scene 白名单,放在 B5a;退款被 16 行上限挡住时保持 PENDING 重试。产品规则列为契约偏差 7,交用户拍板 | §5.1 R5、§5.19.3、§5.20、§5.43-7 |
| 6 | 主要 | 批大小/单笔预算/租约矛盾;配置未接线;老行饿死新行 | **已采纳(改法不同)**。不改 B4b 的串行 Tick,由 guild 配置强制 `LeaseMs ≥ Batch×OpBudgetMs + 5000`,默认 Batch 10、Lease 30s、Interval 1s;`loopConfigFrom` 从 `config.AppConfig.AssetOp` 取值。ClaimDue 分两档:先领 attempts<3 的行,再补老行。补租约并发集成测试(⑥)和两档领取测试;并发 worker 作为给 B4b 的后续建议 | §5.19.1、§5.29、§5.39、§5.43-12 |
| 7 | 主要 | 冒烟同日不能重跑;账号与 S6/S4 冲突 | **已采纳**。账号改为 robot_9214/9215(B2 占 9211–9213,S6 占 9216–9219)。第 2 步读用量判定 full/degraded;degraded 只跑可重复的断言,并打印 `mode=degraded`。Codex 在重建开发库后首跑必须是 full,再跑一次应为 degraded | §5.40、§5.42-8 |
| 8 | 次要 | 默认行与校验器和 B2 冲突 | **已采纳(取 B2 为基线)**。只保留 B2 的 `validateGuildTables`,B5 往里追加 3 条规则,B5 自己的校验器只管 Donate/Shop。沿用 B2 的 5 级行、1 级 50 人、截止 600s,只改 `upgrade_cost_funds` 列;GuildShop 204 等级 6→5;`GuildLevel.xlsx` 进入 B5a 清单;附 B2 断言核对命令 | §5.9、§5.10、§5.11 |
| 9 | 次要 | 漏 servicecontext.go;多列 go.mod;filters;SegmentMinter 不存在 | **已采纳**。B5b 加入 `servicecontext.go`(字段 :35、初始化 :107、Stop :136),删掉 go.mod;B5a 加入 `table.vcxproj.filters`;改用 `&idsegment.Minter{Name, Segment}`(不设 Fallback),并做 nil 接口处理 | §5.29、§5.41 |
| 10 | 次要 | 闸门 zone 取自缓存,合服后可能被绕过 | **已采纳**。T-D/T-S 事务的第一句读 `guild.zone_id`,T-U 从加锁的帮会行取;闸门 `l.economyFence` 在事务内、提交前检查,读失败按封锁处理。R4 补充合服窗口下的论证(S2 §8.5 申请时已校验 zone) | §5.1 R2/R4、§5.15–§5.18 |
| 11 | 次要 | 回包用 toProtoGuild 会清零待审数 | **已采纳**。所有经济写 RPC 回包改用 `guildInfoFor(ctx, g, playerID)`;补单测断言 `pending_application_count` 不为 0 | §5.5.4、§5.25、§5.26、§5.39 |
| 12 | 次要 | 写死 tip 码与交易类型数值 | **已采纳**。新增常量 `constants.AssetReasonCurrencyInsufficient`(取生成常量),SQL 中的 kind、stream、tx_type、status 全部改为绑定参数;`tipCodes()` 同步;Codex 增加 `rg` 字面量守卫 | §5.12、§5.15、§5.42-7 |
| 13 | 次要 | Finalize/OnAssetFinalized 缺 ACTIVITY_REWARD 分支 | **已采纳**。两处都按 kind 显式 switch:DONATE、SHOP、ACTIVITY_REWARD(只做 CAS,推 DELIVERY_DONE)、DONATE_REFUND、未知 kind(打 ERROR,只做 CAS,不推送);补测试 10 | §5.19.3、§5.30、§5.39 |
| 14 | 次要 | 卡死的发物行无恢复路径;没校验最大堆叠 | **已采纳**。新增 `cmd/assetopfix`(list/resolve,限 UNKNOWN 且 attempts≥10 的行,要求二次确认、核对流水,打 ERROR 审计日志,复用同一个 Finalize);告警规则写进 guild-phase2.md。规则 4 改为按 `max_stack_size` 计算 `MaxBuyCount`(堆叠为 1 时恒为 1),随视图下发 `max_buy_count` | §5.11、§5.23、§5.27 |
| 15 | 次要 | 终态行与计数行没有清理 | **已采纳**。B5 实现分批清理:终态 op 与计数行各保留 30 天(可配);Finalize 把 `next_attempt_ms` 写成终结时刻,清理走 idx_0;计数行按日键/周键数值域分别删除;每条语句一个 RC 事务。S6"v1.1 再清理"因此提前,列为偏差 13 | §5.22、§5.43-13 |
| 16 | 次要 | 其他长老的升级按钮一直禁用 | **已采纳**。`CanUpgrade` 只看职位与 `UpgradeCostFunds>0`,资金交给服务端判;UpgradeGuild 在业务失败时也回最新 GuildInfo,客户端顺带刷新资金显示;补 EditMode 用例 4 | §5.26、§5.34、§5.38 |
| 17 | 次要 | 总览"我的贡献"一格与 B2 冲突 | **已采纳(标签写法不同)**。以 B2 改后的版本为基线,4 列宽 350;格名沿用 `GuildMyContribution`,标签"可用 / 累计帮贡",值为 "balance / total"。值字号 34,因为评审建议的长标签在 350 宽、26 号字下放不下 | §5.35.3 |
| 18 | 次要 | 传输失败后重试会重复捐献/兑换 | **已采纳**。重连后窗口自动重拉(`MaybeAutoRequest` 在重连状态下清除标记);两页页脚显示"最近一笔/一单"。按 D-14 每表至多一个 UNIQUE 的约定,不加客户端幂等键;残余风险写入 W14 | §5.32 W14、§5.35 |

**新增的跨批影响**(已写进 §5.43,需主设计确认):S1 枚举加 kind 4、列 25;B4a 的 TransactionType 与 scene 白名单;B2 的 `guild_manage_repo.go`、`guild_manage_logic.go`、`push.go`、`GuildClient.HandleGuildChanged/DrainQueued`;S6 的计数行被 30 天清理;批次拆为 B5a/B5b/B5c(21/23/12 个手改文件)。
