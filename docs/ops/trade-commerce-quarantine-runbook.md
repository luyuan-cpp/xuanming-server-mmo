# 交易合同隔离与角色冲突处置手册

> **状态**：v0.2。v0.1 是 2026-10-22 随 [聚宝斋设计](../design/jubaozhai-market.md) §15.3「2026-10-22 修复」一起写的；2026-11-11~12 按设计 §15.7.1 改了 §1、§2、§3.3、§4.2~§4.4、§6（换主当场转人工、四类残余的处置动作、处置工具改走管理 RPC）。**代码未编译、未测试，本手册未在任何环境演练**，待 Codex 验证。以代码为准：代码行为变了，回来同步这里。
> **依据**：`go/trade/internal/commerce/tick_isolation.go`（分类、推迟、隔离）、`engine.go`（`Tick` / `advance`）、`market.go`（坏对手单移出盘口）；资产指令人工终结口径见 [资产通道](../design/guild-phase2/04-asset-channel.md) 第 10 部分 4.38。
> **告警**：`deploy/k8s/asset-channel-alerts.yaml` 的 `trade-commerce.contracts` 组。

---

## 1. 什么时候看这份手册

| 告警 / 信号 | 含义 | 看哪节 |
|---|---|---|
| `TradeCommerceQuarantined` | 有合同被隔离为 Manual（`status=10`）：确定性坏数据，或 2026-11-11 起参与方角色已换主 / 已删（原因 `owner_changed`）。托管资产扣在合同里，没有自动出口 | §2、§3、§4.1、§4.2、§4.3 |
| `TradeCommerceRoleConflict` | 有合同因参与角色被另一条挂单冻结、正在投票（出售、删除）而停住，每 5 分钟复查一次。换主、删角不再停在这里，当场转人工 | §3.3、§4.3 |
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
| 最终发放腿没有成功（`deliver` / `return` 阶段的发放腿终局不是 APPLIED） | 无专门日志，按 op 行判断（§3.2） | `manual`（2026-11-05 之前是 `done`） |
| 人民币寄售退回失败 | 无专门日志 | `manual` |
| 参与方角色已换主或已删（2026-11-11 起；以前按角色冲突每 5 分钟空转） | `[Commerce] Contract quarantined as manual because a party role changed owner or was deleted commerce_id=… stage=…: seller player=… owner_revision=… (contract …) same_account=…` | `manual`，原因 `owner_changed` |

2026-11-11 起成交子单转人工不再连带它的母单：母单剩下的挂单量照常成交或撤单，撤单之后停在 `market_wait` 等子单，子单处理完结案时母单被唤醒（设计 §15.7.1 第 4 类）。以前留下的原因 `market_fill_manual` 不会再出现。

隔离时引擎做的事（2026-10-22 起）：

- 把合同名下仍在途（`status=1`）的托管扣款（`stream=3`）截止时间提前到隔离时刻。资产通道随后用**同一个 seq** 改发 Abort：scene 已经扣到的照常回 APPLIED 留下证据，没扣到的不会再扣。所以隔离之后不会再有玩家的资产被扣进这张人工单。
- 隔离之前已经排定的发放腿（`stream=4`）照常投递，因为它们来自隔离之前合法的状态。
- 不发出、不退回任何资产，不把 op 行标为业务已消费，证据不会被清理。

**原 stage 记在合同里**（2026-11-05 起）：`workflow_proto` 的 `quarantine` 记下转人工之前的 stage、status、原因类别和错误原文，用 §4.2 的 `commercefix show` 查看。`workflow_proto` 是 protobuf 二进制，SQL 读不出来。解不开的 workflow 没有这份记录，日志仍是唯一来源，要把日志行连同 `commerce_id` 记进工单。

**转人工之后玩家撤不了单**：通用合同与人民币寄售的撤单都按可撤阶段白名单判，`manual` 不在其中。玩家收到 `kTradeOrderStateConflict`，合同行不变，只能走人工处置（§4.2；人民币寄售要和商城订单一起处理）。人民币寄售这一条 2026-11-12 才补上（设计 §15.7，未编译、未运行）：之前卖家撤一张转人工的寄售，会把 `status` 改成 6 而 stage 仍是 `manual`，合同从 §3.1 的列表里消失，`commercefix` 拒绝处理，商城也收不到 `MANUAL`。

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
SELECT player_id, account, owner_revision, trade_listing_id, prepare_token
FROM <角色目录schema>.account_role
WHERE player_id IN (?, ?);
```

- `trade_listing_id ≠ 0`：角色正被一条人民币挂单冻结。这是暂时状态，冻结解除后合同最多再等约 5 分钟会自动恢复推进。
- `prepare_token ≠ 0`：角色正在准备冻结或删除，参与者库还在投票。约 30 秒内有结局：通过就转成 `trade_listing_id`，超时由目录撤销。合同最多再等约 5 分钟会重判。
- 两列都是 0，合同 `kind=6` 且卖的就是这个角色：目录拒绝了这次冻结，多半是角色在某个参与者库还有挂起（判定见 [`horizontal-scale-logical-db.md`](../design/horizontal-scale-logical-db.md) §3.5）。合同每约 5 分钟复查一次，到 `expires_ms` 自动走取消，见 §4.3。
- 两列都是 0，其余情况：原主已换（`owner_revision` 只增不减，合同里存的原归属身份永远对不上），或者目录行本身不合格。2026-11-11 起换主、删角当场转人工（原因 `owner_changed`，§2），不会再停在角色冲突里；处置见 §4.3。

## 4. 处置

### 4.1 不要做的事

- **不要手改** `trade_commerce.status` 或 `workflow_proto` 把合同拉回推进。版本条件、锁序和「补偿腿按角色名只建一次」的保证，都建立在引擎自己的写路径上。
- **不要手改** `trade_asset_op` 行。seq、纪元和终局的不变量由资产通道维护。
- **不要人工终结**隔离后仍 PENDING 的托管扣款，要等 Abort 定论。人工终结只用于资产通道 §4.38 列出的情形（UNKNOWN 等），用下面 §4.2 的工具做。证据只认玩家**已提交**的账本，工具自己读、自己限定能选的结局（判据与 guild 工具共用 `go/shared/assetop/manual_evidence.go`）。scene 流水的 correlation_id（等于 `op_id`）只是线索：2026-11-05 起流水在包含这笔改动的存盘提交之后才发，「流水里有」最多说明已提交；「流水里没有」不能证明从没应用（提交之后发送可能失败）。2026-11-05 之前的流水早于存盘提交，有流水的那笔也可能从没落库。账本判不了的行工具一律不终结，留在 PENDING 逐件核对。

### 4.2 Manual 合同

卡死的资产指令用 `go/trade/cmd/assetopfix` 终结（2026-11-01 写，未编译，未演练）：

```bash
cd go/trade && go run ./cmd/assetopfix -f etc/trade.yaml list
```

```bash
cd go/trade && go run ./cmd/assetopfix -f etc/trade.yaml resolve -op <op_id> -as applied -operator <名字> -reason <工单号和流水结论> -confirm <op_id> -txlog-checked
```

只处理 PENDING、创建超过 30 分钟、反复 UNKNOWN（`attempts >= 10`）的行，`-force` 只跳过最后一条，证据照查。结局只有 `applied`、`aborted`。不带 `-txlog-checked` 时只打印核对清单（含工具读到的证据）。

工具按 trade 配置里的 `DataServiceRpc` 读这名玩家已提交的账本（2026-11-06 与 guild 工具统一判据，未编译），按 `assetop.JudgeManual` 下结论，写进 `list` 的 `evidence` 列和审计行的 `evidence=`：

| 证据 | 能选的结局 |
|---|---|
| `applied`：账本记已应用（全额），收据是这一条请求 | 只许 `applied` |
| `rejected`：账本记已拒绝（含中止占位），收据是这一条请求 | 只许 `aborted` |
| `never_applied`：写权行已释放、账本没见过这个 seq，且这一行安静（此刻没人持租约，距最后一次可能的投递超过 30 分钟） | 只许 `aborted` |
| `partial`：账本记部分发放，或行上 `last_reason` 是部分发放 | 都不许：逐件核对 |
| `none`：账本判不了（滑出窗口、纪元不符、没有写权行）、收据不是这一条请求、还有 scene 持有这名玩家、行不安静 | 都不许：留在 PENDING，卡死行告警进入逐件核对 |

终结用条件写：读证据时的 `lease_until_ms`、`last_send_ms` 必须没变，读完之后重投循环领过或发过这一行，本次作废（退出码 3），重跑重新取证。读不到账本（data_service 不可达）就不终结，稍后重跑。写库要带写者身份（`authoritysql.LoadWriterConfig` 读的环境变量），和服务进程同一套；维护排空期间不要运行。终结之后合同下一轮推进按结局继续：`aborted` 走与 scene 判拒相同的补偿，实例扣款判 `applied` 时没有托管原件，合同隔离为 Manual。

转人工的合同用 `go/trade/cmd/commercefix` 处理（2026-11-05 写，2026-11-12 改走管理 RPC；未编译，未演练）。六种动作的语义和拒绝条件见设计 [§15.7、§15.7.1](../design/jubaozhai-market.md)：

| 动作 | 什么时候用 | 参数 |
|---|---|---|
| `resume` | 转人工的原因已经消除（数据修好了、卡死的腿已终结、拍品已结算），回到原阶段由引擎重新判定 | — |
| `reissue` | 最终发放没到账（含整单退回之后退回腿没到账），原样补发 | — |
| `refund` | 还没有任何发放到账，把托管退回给扣出它的人，合同按取消结案；已部分成交的市场单只退剩余部分；拍品不引用的出价可以单独退 | — |
| `partial` | 有腿部分发放 / 部分扣出 | `-part <腿名>=<缺口>`，可重复，一次给出全部部分发放的腿 |
| `redirect` | 参与方的角色已换主或已删（原因 `owner_changed`） | `-party seller\|buyer -to <原账号名下的目标角色>` |
| `void-auction` | 有出价的拍品确定成交不了，拍品与它引用的出价一起退回 | `-linked <出价号> -linked-version <show 里出价的版本>` |

**开启入口**（缺省关闭，与 match 的 `formationfix` 同一做法）：

1. 生成一把至少 32 字节的随机密钥，放进 K8s Secret，不与别的调用方共用；
2. trade 配置写 `Commerce.ManualResolutionSecretEnv: MMORPG_TRADE_COMMERCE_FIX_SECRET` 与 `Commerce.ManualResolutionEnvironment: <本部署的环境名>`（1~64 个 ASCII 字母、数字或 `.` `_` `-`，例如 `prod-global`，每个环境不同）。Pod 注入同名环境变量，滚动重启 trade。环境名就是工具 `-expect-target` 要填的值，写进工单；
3. 启动日志里没有 `Manual contract resolution disabled` 即已开启。变量缺失、密钥不足 32 字节、环境名不合规、资产通道没开时会打这行 ERROR，入口保持关闭，不影响服务；
4. 处置完成后撤掉环境变量（或配置项）再滚动重启，入口回到关闭。

工具不连数据库，只调 trade 的 gRPC（`-endpoint`，即 trade 的 `ListenOn`），在网络策略允许访问 trade gRPC 的运维 Pod 里运行。密钥只从环境变量读，不进命令行，也不进输出；签名时间戳窗口为 5 分钟，运行工具的机器时钟要准。

```bash
export MMORPG_TRADE_COMMERCE_FIX_SECRET=<与 trade 同值>
```

```bash
commercefix -endpoint <trade Pod IP>:<端口> list
```

```bash
commercefix -endpoint <trade Pod IP>:<端口> show -commerce <commerce_id>
```

```bash
commercefix -endpoint <trade Pod IP>:<端口> resolve -commerce <commerce_id> -action refund -expect-version <show 里的版本> -expect-target <工单上的环境名> -operator <名字> -reason "<工单号>: <核对结论>" -confirm <commerce_id>
```

处理顺序：

1. 取证：`show` 打印转人工记录、每条腿与它 op 行的状态（终态行带终结时刻 `final_ms`）、两方角色现在的归属（只说修订号与账号是否相同，不打账号原文）、关联的拍品或出价，以及每种动作现在能否处理；再看 §3.2 的全部 op 行。
2. 在途的腿等结局；卡死的单条指令先用上面的 `assetopfix` 终结。还有在途的腿时所有动作都会拒绝。
3. 按情形选动作：
   - 部分发放（`APPLIED_PARTIAL`）：按 scene 的 `[AssetOp] partial ... failed=<哪一件>` ERROR 日志、流水（correlation_id = op_id）与玩家当前背包，写出每条腿没到账 / 没扣到的部分。缺口只能是 `instance:<guid>`、`pet`、`currency:<类型>`：按数量的物品是一整批，不会只缺一部分；一条货币也是整条到或整条没到。工具按 scene 的应用顺序校验，不符合的拒绝（多半是证据读错了）。发放腿只补缺口；托管扣款腿按整单退回结案，只退扣到的部分，扣到的部分里有实例或宝宝时退不了，要按 `player_snapshot` 人工补发。
   - 原主已换（`owner_changed`）：见 §4.3。
   - 拍品转人工、它引用的出价还在 `bid_held`：成交不了的用 `void-auction`（`show` 打印 `linked:` 一行的出价号与版本）；卖家已收款或出价人已收到拍品时不能作废，用 `reissue` 把另一半补完。出价转人工时：拍品不引用它（`show` 的拍品 `winning_bid` 不是它）可以 `refund`；拍品已结案的用 `resume`，由引擎把冻结款算作已付。
   - 已部分成交的市场单：`refund` 只退剩余部分；成交子单照常交付，不受影响。
4. 不带 `-execute` 跑一次 `resolve`：服务端在与执行相同的锁下算计划，不写。核对计划里的新腿、被取代的腿、改投的腿，把 `[CommerceManual]` 行贴进工单。
5. 加 `-execute` 再跑一次。退出码 3 说明合同（或联动的出价）被处理或推进过，重新 `show` 再决定；退出码 1 且提示提交结果未知时，先 `show`，看到这一条留痕（`history:` 行里有这个操作人）即已完成，不要重跑。
6. 下一轮引擎给新腿发号、投递。用 §3.2 的查询确认新 op 行出现并到账，合同结案。

工具与入口只改合同行，不碰 op 行，不发资产；新腿由引擎发号投递，与玩家操作同一条写路径。官方商城交付与人民币寄售的人工处理要和商城订单一起做，不用这个工具。

### 4.3 角色冲突

- 暂时冻结、还在投票：不用处理，等结局。
- 角色寄售被目录拒绝：不用处理，到 `expires_ms` 自动取消。这时角色没有冻结过，托管也还没进商城。
- 原主已换（合同转人工，原因 `owner_changed`）：在途资产属于原账号，不随角色过户；交易只能把资产发给角色，所以按原账号的申请改绑到它名下的另一名角色（`redirect`，设计 §15.7.1 第 1 类）。正常运行时参与方有未终局合同就换不了主，出现这种合同说明投票被绕过、交易库回档或目录被人工改过，要同时排查原因。步骤：
  1. 客服核实原账号的申请，工单写明原账号同意由哪名角色接手这张合同（此后该付的从它扣、该收的发给它）；
  2. 用 GM 查询 `GetItemGainBlocks` 确认目标角色没被封禁获取：改绑之后的退回类发放按退回记，不判获取封禁；
  3. `show` 看两方归属（`changed=true` 的一方）与每条已到账发放腿的 `final_ms`，对照角色过户时刻：过户之后才到账、发进原角色的，资产已经到了新主手里，工具管不了，要在交易之外补偿并登记；
  4. `resolve -action redirect -party <seller|buyer> -to <目标角色>`，先不带 `-execute` 核对计划（改投的腿、补发给目标角色的腿），再执行。目标角色被冻结、在投票、已删、不属于原账号、是合同另一方时拒绝；
  5. 两方都换了主时逐方改绑：改完一方合同仍是人工，改完第二方才回到原阶段。

### 4.4 盘口

- 坏对手单会被自动隔离并移出盘口（日志见 §2），taker 下一轮按新的最优价重撮，不需要人工处理盘口。坏对手单本身按 §4.2 处理（已部分成交的只退剩余部分）。它已成交的部分由成交子单（`kind=4, side=0`）独立交付。
- 成交子单转人工不影响它的两张母单：母单照常成交、撤单，撤单之后停在 `market_wait`，子单按 §4.2 处理完结案时母单收尾。
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

- 管理后台的页面与审批流（两人复核）：现在入口是管理 RPC 加命令行（§4.2），请求里的操作人名字证明不了身份；有管理后台的登录身份之后在入口里接。
- 官方商城交付、人民币寄售的人工处理：要和商城订单一起做，不在本工具。
- 部分扣出的实例、宝宝：没有托管原件，只能按 `player_snapshot` 人工补发。
- Java 版对齐，见设计 §15.3「2026-10-22 修复」的残余项与 §15.7.1。
