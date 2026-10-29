# 交易合同隔离与角色冲突处置手册

> **状态**：v0.1，2026-10-22 随 [聚宝斋设计](../design/jubaozhai-market.md) §15.3「2026-10-22 修复」一起写。**代码未编译、未测试，本手册未在任何环境演练**，待 Codex 验证。以代码为准：代码行为变了，回来同步这里。
> **依据**：`go/trade/internal/commerce/tick_isolation.go`（分类、推迟、隔离）、`engine.go`（`Tick` / `advance`）、`market.go`（坏对手单移出盘口）；资产指令人工终结口径见 [资产通道](../design/guild-phase2/04-asset-channel.md) 第 10 部分 4.38。
> **告警**：`deploy/k8s/asset-channel-alerts.yaml` 的 `trade-commerce.contracts` 组。

---

## 1. 什么时候看这份手册

| 告警 / 信号 | 含义 | 看哪节 |
|---|---|---|
| `TradeCommerceQuarantined` | 有合同因确定性坏数据被隔离为 Manual（`status=10`），托管资产扣在合同里，没有自动出口 | §2、§3、§4.1、§4.2 |
| `TradeCommerceRoleConflict` | 有合同因参与角色换主、被另一条挂单冻结或目录行不合格而停住，每 5 分钟复查一次 | §3.3、§4.3 |
| `TradeCommerceMarketMatchFailing` | 撮合持续失败，盘口可能被挡住 | §4.4 |
| `TradeCommerceRetryFailuresPersistent` / `TradeCommerceTickTruncated` | 推进持续暂时失败，或每轮预算都用完 | §5 |
| `AssetOpAppliedPartial`（service=trade） | 某条合同腿部分发放，合同随之转 Manual | §2、§4.2 |

## 2. 合同为什么会是 Manual

`trade_commerce.status = 10` 的合同不再进入 `Due`，引擎不会再推进它。来源有这几种，日志关键字可以用来区分：

| 来源 | 日志（英文原文） | workflow 的 stage |
|---|---|---|
| 合同自身的持久数据坏了（workflow 解不开、已发出的腿缺 outbox 行、腿状态非法、托管快照解不开、`plan` 判错、已扣出的托管转不动） | `[Commerce] Contract quarantined as manual (deterministic data error) commerce_id=… stage="原 stage": 原因` | `manual`；解不开时不改 workflow，日志里 `stage="<undecodable>"` |
| 盘口上的对手单不可能成交（workflow 解不开、缺原归属身份、原主已换） | `[Commerce] Market counterparty quarantined as manual and taken off the book counterparty_id=… taker_id=… stage=…` | 同上 |
| 某条腿 `APPLIED_PARTIAL` | 告警 `AssetOpAppliedPartial` | `manual` |
| 最终发放腿没有成功（`deliver` / `return` 阶段的发放腿终局不是 APPLIED） | 无专门日志，按 op 行判断（§3.2） | `done` |
| 市场单名下有 Manual 的成交子单 | 无专门日志 | `manual` |
| 人民币寄售退回失败 | 无专门日志 | `manual` |

隔离时引擎做的事（2026-10-22 起）：

- 把合同名下仍在途（`status=1`）的托管扣款（`stream=3`）截止时间提前到隔离时刻。资产通道随后用**同一个 seq** 改发 Abort：scene 已经扣到的照常回 APPLIED 留下证据，没扣到的不会再扣。所以隔离之后不会再有玩家的资产被扣进这张人工单。
- 隔离之前已经排定的发放腿（`stream=4`）照常投递，因为它们来自隔离之前合法的状态。
- 不发出、不退回任何资产，不把 op 行标为业务已消费，证据不会被清理。

**原 stage 只在日志里。** 合同行的 stage 已被改成 `manual`，`workflow_proto` 是 protobuf 二进制，SQL 读不出来。收到告警后要先把日志行连同 `commerce_id` 和原 stage 记进工单，不要等日志过了保留期。

## 3. 只读取证

以下 SQL 都是只读查询，在 trade 库上执行，使用只读账号。

### 3.1 列出 Manual 合同

```sql
SELECT commerce_id, kind, status, seller_player_id, buyer_player_id, parent_id, counterparty_id,
       unit_price, quantity, filled_quantity, currency_type, expires_ms, updated_ms, version
FROM trade_commerce
WHERE status = 10
ORDER BY updated_ms;
```

`kind`：1 官方商城交付，2 双方交易，3 寄售，4 市场单（`side=0` 是成交子单，`parent_id` 为买单、`counterparty_id` 为卖单），5 拍品，6 人民币寄售，100 竞价。`updated_ms` 约等于隔离时刻。

### 3.2 查一条合同的全部资产指令

```sql
SELECT op_id, player_id, stream, stream_epoch, seq, kind, status, durable, deadline_ms, next_attempt_ms,
       last_outcome, last_reason, resolved_by, business_consumed, receipt_ack_pending
FROM trade_asset_op
WHERE ref_kind = 2 AND ref_id = ?   -- commerce_id
ORDER BY op_id;
```

按 `stream` 和 `status` 判断资产在哪：

| stream | status | 资产在哪 |
|---|---|---|
| 3 托管扣款 | 2 APPLIED | 已从玩家身上扣出，现在在合同托管里 |
| 3 | 3 REJECTED / 4 ABORTED / 6 ABANDONED | 没扣到，仍在玩家身上 |
| 3 | 1 PENDING | 在途。隔离后 `deadline_ms` 应 ≤ 隔离时刻，等资产通道的 Abort 定论，**不要人工终结** |
| 4 发放 | 2 APPLIED | 已发到 `player_id` |
| 4 | 1 PENDING | 在途，照常投递 |
| 4 | 其它 | 没发到，资产仍算在合同托管里 |
| 任意 | 5 APPLIED_PARTIAL | 部分发放，按资产通道 §4.33 人工补偿 |

`resolved_by` 非空表示这一行是人工终结的；`status=6` 表示自动放弃。

### 3.3 角色冲突

先用日志 `[Commerce] Contract blocked by a party role` 取 `commerce_id`，再用 §3.1 的查询把 `WHERE status = 10` 换成 `WHERE commerce_id = ?` 查出参与者，然后到账号角色目录库里查这些角色（schema 是 trade 配置里的角色目录 schema）：

```sql
SELECT player_id, account, owner_revision, trade_listing_id
FROM <角色目录schema>.account_role
WHERE player_id IN (?, ?);
```

- `trade_listing_id ≠ 0`：角色正被一条人民币挂单冻结。这是暂时状态，冻结解除后合同最多再等约 5 分钟会自动恢复推进。
- `trade_listing_id = 0`：原主已换（`owner_revision` 只增不减，合同里存的原归属身份永远对不上），或者目录行本身不合格。合同不会自己恢复，见 §4.3。

## 4. 处置

### 4.1 不要做的事

- **不要手改** `trade_commerce.status` 或 `workflow_proto` 把合同拉回推进。版本条件、锁序和「补偿腿按角色名只建一次」的保证，都建立在引擎自己的写路径上。
- **不要手改** `trade_asset_op` 行。seq、纪元和终局的不变量由资产通道维护。
- **不要人工终结**隔离后仍 PENDING 的托管扣款，要等 Abort 定论。人工终结只用于资产通道 §4.38 列出的情形（UNKNOWN 等），而且必须凭 scene 流水的 correlation_id（等于 `op_id`）。

### 4.2 Manual 合同

**目前没有补发、退回或恢复推进的管理入口。** 入口落地前按这个顺序处理：

1. 取证：§3.1 的合同行、§3.2 的全部 op 行、隔离日志（原 stage 和原因）。
2. 冻结现场：确认在途扣款都已有结局（§3.2 里 stream=3 没有 PENDING）。
3. 登记工单：写上 `commerce_id`、`kind`、原 stage、每条 op 的 `op_id` / `stream` / `status`、参与者 `player_id`。
4. 等管理入口上线后按工单补发或退回。

资产没有丢：已经扣出的托管仍记在合同和 op 行里，只是暂时没有自动出口。

### 4.3 角色冲突

- 暂时冻结：不用处理，等冻结解除。
- 原主已换：托管资产是从原归属身份扣出的。退回角色就等于交给新主，所以引擎按设计拒绝发放。谁应该拿到这份资产，需要产品或运营做决定，本手册不替他们决定。按 §4.2 的流程取证并登记工单。

### 4.4 盘口

- 坏对手单会被自动隔离并移出盘口（日志见 §2），taker 下一轮按新的最优价重撮，不需要人工处理盘口。坏对手单本身按 §4.2 处理。它已成交的部分由成交子单（`kind=4, side=0`）独立交付。
- `TradeCommerceMarketMatchFailing` 且日志里 `class=role_conflict`：多半是盘口上对手单的角色被另一条挂单冻结。这种单**不会**被自动移出盘口（冻结是暂时的，而且从错误里分不出是哪一方被冻结），会挡住它所在的价位，直到冻结解除。确认之后登记工单。

## 5. 观察指标

| 指标 | 说明 |
|---|---|
| `trade_commerce_quarantined_total` | 隔离为 Manual 的合同数，包括被移出盘口的对手单 |
| `trade_commerce_tick_row_failures_total{class}` | 单行推进失败。`retry` 推迟 10s±20%；`too_many_pending` 推迟 60s±20%，日志只记 INFO；`role_conflict` 推迟 5min±20%；`corrupt` 隔离；`conflict` 不推迟，下一轮立即重试，日志只记 INFO |
| `trade_commerce_market_match_failures_total{class}` | taker 推进成功之后撮合失败。不推迟，下一轮重撮 |
| `trade_commerce_tick_truncated_total` | 预算用完时 Due 里还有合同没开始推进的轮数 |

每轮合同推进的预算是 3s（`svc.AssetChannel.Start`）。单行子预算从这 3s 的截止时刻推出来：先留 0.5s 给收据 ACK、0.3s 给失败后的推迟或隔离，剩下的一行最多用一半。卡住的行（锁等待、依赖 RPC 不回包）会撞上自己的子预算并被推迟，不会再吃满整轮。有一种例外：别的事务一直持有某条合同行的行锁时，推迟也拿不到这把锁。这时那一行每轮仍排在队头，但每轮只花它自己的子预算和推迟预算，同一轮里其余合同照常推进。

## 6. 待补

- 管理入口：Manual 合同的列表、恢复推进、补发和退回。需要另立设计，覆盖鉴权、操作留痕，以及沿用原腿和原 seq 的幂等。
- 把原 stage 和隔离原因持久化进合同。目前只在日志里，要改 `TradeCommerceWorkflow`。
- Java 版对齐，见设计 §15.3「2026-10-22 修复」的残余项。
