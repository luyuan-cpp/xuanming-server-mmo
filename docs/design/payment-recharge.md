# 真钱充值设计(苹果 IAP / Google Play / 国内安卓 / 小游戏)

> **状态:设计稿第 5 版(2026-10-20),未实现、未编译。** 本稿经过四轮只读对抗评审,共 27 个评审或核实 agent,每轮发现数如下:
>
> | 轮次 | 新发现 | 其中 P0 |
> |---|---|---|
> | 第一轮 | 47 条 | — |
> | 第二轮 | 15 条 | 4 条 |
> | 第三轮 | 15 条 | 2 条 |
> | 第四轮 | 23 条 | 3 条 |
>
> 每轮核实成立的发现都已改进本稿,修订记录见 §20。**第 5 版的修改没有再经过评审。** R1 起分批落码,每批按 AGENTS §10.1 交 Codex 编译测试。
>
> - **用户拍板(2026-10-19)**:国内、海外都要。先做苹果 + 谷歌骨架,国内渠道作为第二批。目前**没有任何开发者账号**,先用本地 `mock` 渠道跑通。
> - **分工**(各会话协商,用户已在「微信抖音充值接入」会话确认):
>   - 共用骨架与本文:会话「苹果和安卓应用充值」;
>   - 微信 / 抖音小游戏插件与小游戏登录:会话「微信抖音充值接入」(用户已定走小游戏形态);
>   - 官方物品商品冒烟:会话「道具交易系统代码」,允许它在开发环境用 robot mock 付款器(§7)。
> - **依赖**:商城发货 / 退款契约原在 `origin/snapshot/all-changes-20261018`,**已于 2026-10-19 由 `fc3ff045ee` 合入 main**。合入时跳过了编译测试,已核对与快照一致。
> - **接缝**:与聚宝斋人民币寄售的接缝见 §21。寄售计划是 `docs/design/jubaozhai-rmb-consignment-plan.md`,在另一个会话的工作树里,未合入。

## 0. 一句话结论

**充值 = 买商城里的一个"官方固定价钻石包"。** 订单与钱包各只有一份权威(D-17):订单在 `java/mall_game`,钻石在 C++ `kCurrencyDiamond`。

四条规则保证"不丢单、不重复发、已撤销必扣、撤回必补":

1. **付款事实带修订号。**
   - go/payment 为每笔平台交易维护单调修订号:偶数 = 已付,奇数 = 已撤销。
   - 观测按乐观并发写入,过时的观测盖不住新事实。
   - 只有苹果允许"撤销又撤回"。
   - 商城按 `(channel, transaction_id)` 的守卫行只接受更大的修订号。**每次修订号前进,商城都按新修订号写一条权益待办**,不会因为历史状态漏写。
2. **trade 按订单收敛。** 每张充值订单在 trade 只有一条记录,记着三个量:该不该有这笔钻石(entitled)、已净发多少(net_granted,由腿的结局推导)、人工核销多少(written_off,只对当前修订号有效)。用"至多一条在途腿"补发或扣回,账平后持久地回执商城。
3. **扣回可以线性叠加。** 扣回 = 先扣余额,不够的差额记成充值欠款;补发 = 普通入账,先抵充值欠款。
4. **每条路径都有出口和期限。** 孤儿、拒绝、超限、暂扣、长期停放的扣回,都有人工入口(肯定与否定两个方向),并有终审期限。

`go/payment` 是新增的 Go 全局服务,也是商城唯一的 `payment` 角色调用方(开发环境 robot 冒烟例外)。它负责:

- 付款参数;
- 核实;
- 付款事实;
- 推进商城;
- 退款通知与补漏;
- 主动退款与关单;
- 合规闸。

发钻、扣钻只由 `go/trade` 执行。

## 1. 目标与非目标

**目标**:

- G1 iOS:App Store 消耗型 IAP。
- G2 安卓海外:Google Play 一次性消耗型商品。
- G3 国内安卓与小游戏:插件接入(R3)。
- G4 在以下情况下,**不丢单、不重复发货、已撤销必扣、撤回必补**,每笔钱都有终局:崩溃、超时、重试、乱序、多副本、重复通知、客户端掉线、玩家长期离线。
- G5 本地无账号也能用 mock 端到端联调。

**非目标(v1)**:

- 首充双倍、月卡、订阅、礼包。
- 平台游戏币(微信 `wx.requestMidasPayment` / `mode="coins"`、抖音游戏币)。
- 聚宝斋寄售收款:见 §21 接缝,由寄售计划第 13 步实现。
- 提现、卖家结算。
- 客户端改 WebSocket。

## 2. 现状(2026-10-20 在 main 上核实)

### 2.1 通用

| 事实 | 证据 |
|---|---|
| 商城:幂等下单、预占、可信支付回执、取消;付款后只到 `DELIVERY_PENDING` | `java/README.md`;D-17.4 |
| 错误体只有 `{"error": 中文}`,冲突一律回 409,没有机器码 | `CommerceController.java:28-30`;`NonceStore.java:51` |
| channel 在任何模式下都只做正则校验,没有白名单;`RUN_MODE` 只用来禁止 prod 搭配 `REQUIRE_TIDB=false` | 会话「道具交易系统代码」核实 |
| 签名为 `HMAC-SHA256(secret, "v1\nrole\nsubject\nts\nnonce\nMETHOD\n原始URI\nhex(sha256(body))")`;**角色按路径硬编码**,未登记的路径算 `gateway`(要求 subject 为玩家号) | `HmacAuthentication.java:36-71` |
| SKU 库存是有限 INT;取消订单和定时关单都写 status=4,**没有关单原因列**;每个买家最多 5 张未付单;支付窗口 15 分钟 | `CommerceStore.java:48-111,171` |
| 官方订单视图的 `version` 直接取状态枚举值(其它订单种类用记录版本号) | `mall.go:211`;`commerce_logic.go:31` |
| 钻石为 `kCurrencyDiamond = 1` | `currency.h:8-10` |
| `AddCurrency` 先抵欠款,所有入账都走它;`DeductCurrency` 不看欠款;`AttachDebt` 只记欠款、不扣余额,还会覆盖 reason / expiresAt;运行时欠款表以币种为唯一键 | `currency_system.cpp:170-414`;`player_currency_comp.h:25,45` |
| 货币封禁:单玩家封禁在存档 `CurrencyComp.blocked_types`;全局 `GainBlockService` 是 scene 线程内集合,Go 读不到 | `gain_block_service.h` |
| 资产通道调用方:`guild`、`trade`、`mail`(mail 独占 `SYSTEM_CREDIT`)。`TRADE_CREDIT` 放行 `TX_AUCTION_BUY/TX_TRADE`;`TRADE_DEBIT` 放行 `TX_AUCTION_SELL/TX_TRADE/TX_AUCTION_BUY`;**每条流的上限 `kAssetOpMaxTxPerStream=3` 已用满** | `asset_op_auth.cpp:44-55`;`asset_op_system.cpp:79-99` |
| 资产通道离线:scene 回 NOT_HERE,Go 侧停放、上线唤醒 | `caller.go:97-110`;`reconcile.go:670` |
| **go/trade 只要开资产通道,就强制 `Market.Scope=global` 且用 TiDB**,否则拒启;Mall 只在资产通道开启时装配;`trade.yaml` 现在写的是 `Scope: zone` | `go/trade/internal/config/config.go:569-572`;`servicecontext.go:143-162`;`trade.yaml:98` |
| `PaymentNodeService = 19`、`NODE_PAYMENT = 24` 没有使用;`CreateEventSnapshot` 零调用方 | `node.proto:24`;`data_service.proto:438-450` |
| 实名 / 防沉迷 / 年龄零实现;没有"按区关闭功能"的能力 | 全仓检索 |
| `TransactionType` 最大值为 29,30–32 空闲(以落地时为准) | `transaction_log.proto` |

### 2.2 合入 main 的商城 / trade 契约(本文的直接基础)

| 内容 | 位置 |
|---|---|
| 已取消订单收到付款时回 200,state=`REFUND_PENDING`,写 `commerce_refund_intent`,订单置 5 | `CommerceStore.java:126-136` |
| 发货待办:poll 不出队、按 `created_ms` 取前 20 条,没有游标、租约或退避;receipt 只认 {DELIVERED, REFUND_PENDING, MANUAL},订单更新带 `WHERE status=1`;第二次回执只要 state 或 proof 不同就回 409 | `DeliveryStore.java:34-86` |
| `commerce_delivery_intent` / `commerce_delivery_receipt` / `commerce_refund_intent` / `commerce_refund_receipt` 都以 `order_sn` 为主键;`refunds()` 按 order_sn 联查订单已绑定的付款回执;`refunded()` 要求订单状态为 5 | `schema.sql:53-76`;`DeliveryStore.java:87-126` |
| 发放物只支持物品(create 要求恰好 1 条;poll 用 INNER JOIN;`/api/skus` 要求 `stock>lock_stock`) | `CommerceStore.java:78`;`DeliveryStore.java:27-42` |
| `SchemaVerifier` 写死 version==1,其中 `commerce_delivery_intent` 的唯一键 = `(order_sn)` | `SchemaVerifier.java` |
| `Mall.accept` 用 `RequestKey("official", player, orderId)` 建记录(预读不加锁);出错时整批 Tick 中断 | `mall.go:132-281`;`commerce_repo.go:33-34` |
| Engine:<br>- 腿按 role 去重;<br>- 腿没 applied 就走 `official_refund`;<br>- **advance 只读 durable=1 的腿结局**,自动放弃和人工终结都刻意不置 durable;<br>- Due = status IN (1,3,4,6,8) ORDER BY updated_ms LIMIT 100,所有 kind 共用,advance 无条件刷新 updated_ms;拍卖、寄售、撮合靠每轮重扫推进;<br>- tx_type 按流写死 | `engine.go:406-460,606-608,680-686,744-756`;`asset_op_repo.go:884-885,1016-1021`;`commerce_repo.go:71` |
| 自动放弃只针对 `TRADE_DEBIT`,默认 180 天 | `trade.yaml:173`;`config.go:207` |

## 3. 总体架构

```
              ┌──────────── 平台(苹果 / 谷歌 / 微信 / 抖音 / 渠道)────────────┐
              │  核验 / 查单 / 关单 / 通知历史 API         通知(公网 HTTPS)     │
              └──────▲──────────────────────────────────────┬────────────────┘
 ┌────────┐ ②建意图 ┌─┴──────────────────────────────┐       ▼
 │ 客户端 │───────▶│ go/payment(新,全局)            │ ④付款事实同步(带修订号)、无原单入账、关单回执
 │        │ ③交收据 │ 渠道插件 · 付款事实库 · 推进任务  │────────────(payment 角色)────────▶┌───────────────┐
 └───┬────┘───────▶│ 补漏 / 重评 · 合规闸 · 管理面     │◀─ 只读查单/SKU/HELD、退款待办、关单待办 │ java/mall_game │
     │              └─────────────────────────────────┘                                 └──────┬────────┘
     │ ①下单(经 gate)                                                                          │ 权益待办(领取 / nack / 隔离)
     ▼                                                                                          ▼
 ┌───────────────┐ ⑤ 收敛:差额 > 0 → TRADE_CREDIT/TX_RECHARGE;< 0 → TRADE_DEBIT/TX_RECHARGE_REVERSAL ┌──────────┐
 │ go/trade Mall  │─────────────────────────────────────────────────────────────────────────────▶│ C++ scene │
 │ + 人工管理面   │◀─── 持久化的待回执 → 商城回执(按修订号)                                        └──────────┘
 └───────────────┘
```

| 组件 | 职责 | 不负责 |
|---|---|---|
| `go/payment`(新,全局) | 客户端支付协议;渠道核验;通知与补漏;付款事实库;推进;退款与关单执行;合规闸;支付侧管理面(孤儿、超限、暂扣) | 订单、发货、扣回、钱包 |
| `java/mall_game` | 订单权威;交易守卫;充值 SKU;权益待办;按回执推导状态;退款待办与关单待办;只读接口 | 核验收据、判断发没发 |
| `go/trade` | 下单门面;**每单收敛**;trade 侧人工管理面(重排、终结腿、核销) | 判断付没付 |
| C++ scene | `AddCurrency`;扣回、核销原语;充值欠款;账本幂等 | — |

支付服务用 Go:网关只转发 gRPC,路由和部署门禁是 Go 设施;D-17.2 第 3 条要求独立的可信边界。苹果没有官方 Go 库,验签按 §5.3 自己实现,再用官方 Java 库生成 golden 向量对拍。

## 4. 商品与钱包模型

- **钻石包 = 官方 SKU,同时满足:货币发放物、无限库存、数量恒 1。** 默认档位如下(用户可改):

  | 标价(分) | 钻石 | 商品 ID |
  |---|---|---|
  | 600 | 60 | `diamond_60` |
  | 3000 | 300 | `diamond_300` |
  | 6800 | 680 | `diamond_680` |
  | 12800 | 1280 | `diamond_1280` |
  | 32800 | 3280 | `diamond_3280` |
  | 64800 | 6480 | `diamond_6480` |

- 发放物**物品与货币恰好一种**,下单时快照。
- 充值 SKU 的约束:
  - `quantity != 1` 回 400;
  - 不预占库存;
  - 不计入未付上限;
  - 超时由商城定时关单(状态 4)。关单只影响展示,不影响额度(§12);迟到付款会复活订单(§10.1)。
- **客户端复用订单以意图状态为准**:只复用意图状态为"有效"的订单;收到 `PAY_INTENT_USED` 时重新下单;遇到 Ask to Buy / PENDING 不主动取消。
- **映射核对**(只针对充值 SKU):`sku_id ↔ (channel, store_product_id, 期望基准价)`。启动时核对以下四项,任一不符就拒启:存在、已上架、是充值 SKU、标价等于期望价。能拿到商店价格时再核对中国店面价格(谷歌查 API;苹果的 App Store Connect API 放进部署前检查脚本),不一致至少阻断发布。
- 只发 `kCurrencyDiamond`,不发绑定钻石。
- **封禁预检**(尽力而为):
  - `CreatePayIntent` 经 data_service `GetPlayerField` 读存档里的 `CurrencyComp.blocked_types`(落盘副本,可能比在线态滞后),查到封禁就拒绝;读失败时放行,并记指标。
  - 全局 GainBlock 只在 scene 内存里,不做预检。
  - 最终防线是 credit 腿被拒后转 MANUAL(§10.6)。

## 5. 主流程:苹果 / 谷歌(收据式)

### 5.1 步骤

| 步 | 谁 | 做什么 | 幂等 / 失败 |
|---|---|---|---|
| ① | 客户端 → gate → go/trade | `CreateOfficialOrder{sku_id, quantity=1, client_request_id}` | `(buyer, request_id)` 唯一 |
| ② | 客户端 → go/payment | `CreatePayIntent{order_id, channel, client_params}`:<br>- 用 order-view 核对买家、状态、数量、映射;<br>- 做封禁预检;<br>- 过合规闸;<br>- 意图已用 → 回 `PAY_INTENT_USED`;<br>- 意图过期 → 在合规行锁下重新预占,再 CAS 回有效 | `order_key` 保证一单一意图、一个渠道 |
| ② 返回 | | 苹果:`app_account_token = UUID(HMAC(K_bind, "apple:" + order_sn))`。<br>谷歌:`obfuscated_account_id = base64url(HMAC(K_obf, player_id))`;`obfuscated_profile_id = base64url(HMAC(K_bind, "google:" + order_sn))`。<br>响应带意图状态 | 不含个人信息 |
| ③ | 客户端 | Unity IAP 付款。苹果先 `SetAppAccountToken`;购买串行 | |
| ③' | 客户端 → go/payment | `SubmitStoreReceipt`(苹果 JWS / 谷歌 token);记下 `submit_player_id` | MessageLimiter |
| ③核实 / 落库 | go/payment | §5.3 核实;§8.1 乐观并发落库;按 §5.5 决定 finish | 平台不可达 → finish=false |
| ④ | 推进任务 → 商城 | 扫描 `BOUND` 且 `mall_synced_revision < fact_revision` 的交易,按 §8.0 同步。调用前持久置 `post_attempted=1`;同一意图下多笔交易,按核实先后同步 | 超时 / 5xx 退避重试,每次换新 nonce;应答按 §8.0 对齐 |
| ④' | 推进任务 → 谷歌 | 对已绑定交易调 `purchases.products.consume` | 3 天内必须完成 |
| ⑤⑥ | go/trade → scene | 领取权益待办,按单收敛(§8.3) | 每单一条记录;至多一条在途腿 |
| 回执 | go/trade → 商城 | 持久化的待回执,按修订号发送(§8.3.6) | 商城按 `(order_sn, revision)` 幂等 |
| 展示 | 客户端 | 轮询,直到出现终态:`DELIVERED / REVERSED / MANUAL` | |

### 5.2 三条到账路径

三条路径都汇入 `ConfirmStoreTransaction`,先观测平台事实,再按 §8.1 对齐:

1. 客户端交收据;
2. 平台通知(苹果 `ONE_TIME_CHARGE`、谷歌 RTDN);
3. 客户端启动时 `FetchPurchases()` 重交。

通知只在事实落库、事务提交后才回 200 或 ack;验签失败或落库失败回非 2xx。

### 5.3 核实清单

**苹果**:

- 客户端 JWS 只用来取 transactionId 和做预检。**入账依据是 GTI 返回的 `signedTransactionInfo`**,以下字段都以它为准:`transactionId / productId / appAccountToken / bundleId / environment / quantity / storefront / price / currency / revocationDate / signedDate`。与客户端 JWS 不一致就拒绝并告警。
- 验签清单:
  1. `alg` 只认 ES256;
  2. `x5c` 恰好 3 张;
  3. 根证书锚定 Apple Root CA - G3,写死指纹;
  4. 叶证书 OID `1.2.840.113635.100.6.11.1`,中间证书 OID `1.2.840.113635.100.6.2.1`;
  5. 用 `signedDate` 判断证书有效期;
  6. 核对 bundleId、environment,生产环境再核对 appAppleId;
  7. 不得放宽成 `ExtKeyUsageAny`。
- 价格:`storefront=CHN` 时,要求 `currency=CNY` 且 `price/10` 等于标价(分);其它店面按档位表比对。不符时进人工并告警。

**谷歌**(`productsv2.getproductpurchasev2`):

| 条件 | 事实 |
|---|---|
| `purchaseState=PENDING` | Pending,不产生修订号 |
| `CANCELLED`,或 `refundableQuantity < quantity`,或已有持久化 voided 证据 | REVOKED |
| 其余 `PURCHASED` | PAID |

- **voided 证据**:收到 RTDN 后,按时间窗查 Voided Purchases API 确认,每日补漏也算。写入 `voided_evidence_ms` 后不可推翻(RTDN 消息体没有签名)。
- 其余核对项:
  - 商品与意图一致;
  - profile id 能定位到意图;
  - account id 等于**意图玩家**的派生值(会话玩家不一致时只记日志);
  - `quantity == 1`;
  - 有 `testPurchaseContext` 的是测试交易。
- 实收金额:用 Orders API 异步补写并比对价格;不符就冻结该映射。没有 orderId 的,`PaidKnown=false`。
- R2 实测:退款后 `purchaseState` 与 `refundableQuantity` 的取值,以及 `orders.refund` 的 revoke 参数语义。核实前,`Channel.Refund` 默认 `revoke=false`(§7)。

**通用**:

- token 没命中或商品不一致 → 孤儿;
- 数量不为 1 → 人工;
- 事实为 Pending → 只记录,等通知或重评。

### 5.4 孤儿、第二笔钱与人工出口

**孤儿**(`resolution=ORPHAN`):不自动入账。`ResolveOrphan` 以 ORPHAN 为起点,要求:

- 实时复核未撤销;谷歌孤儿必须在 3 天确认期限内完成绑定,并由服务端 consume;
- `txn_key` 唯一;
- 双人审批,同一事务写审计,审批 id 随请求传给商城;
- 归属证据按渠道:谷歌看混淆账号号;苹果看 `submit_player_id`,绑定后调 Set App Account Token 写回;经通知路径进来、没有会话的苹果孤儿,走申诉 + 人工;
- 绑定后走无原单入账。

**第二笔钱**:token 派生自该玩家订单、商品一致时,走 `EXTRA`:

- 原单第一笔还没绑定 → 商城回 `STATE_CONFLICT`,退避重试;
- 超出结构上限 → 进 HELD。

**管理面**:

| 服务 | 方法 | 作用 |
|---|---|---|
| go/payment `payment_admin.proto` | `ListManual` | ORPHAN / OVER_LIMIT / REJECTED_* / TEST_DENIED / ABNORMAL_REFUND,以及商城 HELD 待办(经 `/internal/payments/held`) |
| | `ResolveOrphan` | 绑定孤儿 |
| | `RetryManual` | 用当前配置重跑 ConfirmStoreTransaction(REJECTED_*、OVER_LIMIT) |
| | `ReleaseHeld` / `DenyHeld` | 放行暂扣 / 拒绝暂扣(§8.2.4) |
| | `FinalizeManual` | 终审:"拒绝 + 退款或引导平台退款",终态见下 |
| go/trade `trade_admin.proto`(内部) | `ListManualCommerce` | trade 侧 Manual 记录与停放的扣回腿 |
| | `ReplanCommerce` | 解封后经收敛入口重排 |
| | `ResolveRechargeLeg` | 凭证据把腿记为 APPLIED / ABORTED(走现有 `Pipeline.ResolveManually`) |
| | `WriteOffReversal` | 核销(§8.3.7) |

所有人工动作都要双人审批、写审计(go/payment 写 `payment_audit`,trade 写 trade 库的审计表)。两边管理面用各自独立的令牌,经 NetworkPolicy 只放行运维工具。

**定时重评**:扫描 `fact=PAID` 且 `resolution ∈ {ORPHAN, REJECTED_RETRYABLE}` 的交易重评。OVER_LIMIT 不自动重评。

**终审期限**(§14):以下几类到期后都进入人工终审,二选一:

| 类别 | 期限 |
|---|---|
| ORPHAN | `OrphanFinalReviewDays` |
| REJECTED_* | `OrphanFinalReviewDays` |
| HELD | `HeldFinalReviewDays` |
| OVER_LIMIT | `OverLimitFinalReviewDays` |
| 停放的扣回腿 | 30 天 |

1. **放行 / 绑定**;
2. **拒绝**:能退款的渠道调 `Channel.Refund`;不能退款的渠道转入终态 `DENIED`,不发货,由客服引导玩家走平台退款。

### 5.5 finish_transaction

| 渠道 | 结果 | finish |
|---|---|---|
| 苹果 | 任何结果 | true(不 finish 会导致同一商品无法再买) |
| 谷歌 | 已绑定(含 EXTRA,含 HELD) | true(客户端 ConfirmPurchase 会 consume) |
| 谷歌 | 孤儿、超限、拒绝 | **false**,服务端也不 consume(3 天后自动退款) |
| 两段式渠道 | — | 不适用 |

谷歌 HELD 的钱已经被确认,如果最终判为拒绝,由 `DenyHeld` 调 `orders.refund` 退款。平台不可达或还没落库时,一律 false。

## 6. 两段式渠道(小游戏 / 微信支付 / 支付宝 / 国内渠道)

接口按会话「微信抖音充值接入」核实的结果预留,平台事实见附录 A。

- **服务端签名**:客户端在 `client_params` 里带 `wx.login` 的 code 等,服务端返回 `channel_params`。
  - 微信用 `wx.requestMidasPaymentGameItem`(`mode="goods"`),由服务端生成整串 `signData`、`paySig`、`signature`;
  - 抖音安卓用 `tt.requestGamePayment`(`goodType=2`)。
- **session_key 现取即弃**,不落库;拿到的 openid 必须与 `payment_channel_identity` 一致。
- **商户单号按尝试派生**:`订单派生值 + 尝试序号`,≤ 32 字符,首字符不能是下划线。只有有效意图才能生成新尝试;旧单号经 `payment_intent_ref` 仍能定位到订单。
- **到账以推送为准**:
  - 每个渠道一个回调地址 `/notify/<channel>/<appid>`;
  - 可以"只回包、不产生事件";
  - 按平台格式与时限回包;重复推送回相同的成功;
  - 路由不写死成"只有支付"。
- **不能查单的渠道**:修订号由事件类型决定,只靠推送 + 日对账兜底。
- **只做道具直购。**

## 7. 渠道插件接口(Go)

```go
type Channel interface {
    Name() string
    RevocationReversible() bool // 只有 apple 返回 true
    CreateIntent(ctx context.Context, in IntentInput) (IntentOutput, error)
    VerifyReceipt(ctx context.Context, in ReceiptInput) (VerifiedTxn, error)
    // events 可为空(握手、模拟推送、非支付事件);Ack 在事件落库提交后才发出;验签失败返回 ErrBadSignature。
    ParseNotification(r *http.Request) (events []Event, ack Ack, err error)
    // 需要用户签名才能查的渠道返回 ErrQueryUnsupported。
    Query(ctx context.Context, ref TxnRef) (VerifiedTxn, error)
    // 谷歌 consume、华为消耗;微信是推送回包失败后的补通知(待核实)。
    Finalize(ctx context.Context, txn VerifiedTxn) error
    // req.IdempotencyKey 由被退那笔付款的交易键确定性派生(微信 out_refund_no、谷歌按 orderId),一笔付款一个键,不按订单;
    // 调用前先按它查退款状态:已退款或退款处理中,直接返回结果,不再发起。
    // 苹果、微信小游戏、抖音返回 ErrRefundUnsupported;谷歌 orders.refund 在 R2 核实前默认 revoke=false。
    Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
    // 关单:只用于有限库存订单(§21),对该订单的全部尝试单号执行;
    // 收据式渠道不参与有限库存订单,返回 ErrCloseUnsupported。
    Close(ctx context.Context, ref TxnRef) (CloseResult, error)
}

type VerifiedTxn struct {
    Channel, TransactionID, StoreProductID string
    Quantity                               int64
    IntentRef, AccountBinding, Storefront  string
    ListAmount, PaidAmount                 int64
    PaidCurrency                           string
    PaidKnown                              bool
    Environment                            string // production / sandbox
    Fact                                   Fact   // Paid / Revoked / Pending
    ObservedAtMs, RevokedAtMs              int64
}
```

**`mock` 渠道**:

- 只在 go-zero 顶层 `Mode ∈ {dev, test}` 时允许启用,`MockNotify` 运行时再核对一次;
- 收据格式为 `mock:<order_sn>:<nonce>`。

**robot mock 付款器(冒烟例外)**:

- 只连 loopback、只认 dev 模式的商城,渠道名 `mock-dev`,签名测试向量在三种语言间共用。
- 现状:商城还不拒收 mock 前缀,要等 §8.2.11 的渠道白名单。
- go/payment 落码后,改为走它的 mock 渠道。

## 8. 数据与改动面

### 8.0 付款事实同步协议

**修订号**:偶数 = 已付,奇数 = 已撤销,分配规则见 §8.1。

**调用**:请求都带 `revision`、`amountMinor`、`currency`。

| 调用 | 何时 |
|---|---|
| `POST /internal/payments` | 本地 `BOUND` 的交易,任意修订号,带 `orderId` |
| `POST /internal/payments/unbound` | 无原单入账(EXTRA / ORPHAN_RESOLVED),任意修订号,带 `buyerId, skuId, source, originOrderId/approvalId` |

本地 `post_attempted=0` 的交易被撤销时,只记本地,不调商城。

**商城交易守卫**:`commerce_payment_receipt` 扩展为守卫行:主键 `(channel, transaction_id)`,`order_sn` 可空,加 `revision`、`fact` 列。所有付款路径都先锁守卫行,按以下规则处理。

1. **请求修订号更大,或还没有守卫行:应用。按订单用途分支(§21)**:
   - **充值 SKU**:
     - **已付、守卫已绑定订单**:每次都按新修订号写一条 `entitled=1` 的权益待办。若该订单有未释放的 HELD 待办,新待办也置 HELD,并把同单修订号更低的 HELD 行置 SUPERSEDED;否则置 PENDING。不再以"撤销前是否放行过"作条件。
     - **已付、守卫未绑定**:绑定请求里的订单(订单为 4 时同一事务重开到 1),记回执,写 `entitled=1` 待办。如果订单已被别的守卫行绑定,回 `DIFFERENT_RECEIPT`。
     - **已撤销、已绑定**:订单进入 `REVERSED`,写一条 `entitled=0` 待办。
     - **已撤销、没有守卫行**:
       - 请求带 orderId 时:建守卫并绑定,订单(0 或 4)置 REVERSED,写 `entitled=0` 待办。
       - 请求是 `/unbound` 时:建单并绑定,订单直接为 REVERSED,写 `entitled=0` 待办。
       - 以后的撤回仍按"已付、已绑定"处理。
   - **其它 SKU**(官方物品、寄售挂单):
     - 订单已被别的守卫行绑定(重复付款),或金额、币种与订单冻结值不符:**不回 409**。同一事务里写一条不绑定订单的守卫行(`order_sn` 为 NULL)、一行异常付款和一条同原因的退款待办,回 200,应答 `refundReason` 为 `DUPLICATE_PAYMENT` / `AMOUNT_MISMATCH` / `CURRENCY_MISMATCH`(已落码,见 §8.2.1)。
     - 撤销:只更新守卫行的修订号与事实。订单已经是 REFUND_PENDING 或 REFUNDED 的不改成 REVERSED,也不写 `entitled=0` 待办。已发货订单的冲回交给寄售计划或官方物品的专门流程。
2. **修订号相等**:幂等,回 200 + 当前状态。
3. **修订号更小**:回 200 + 当前状态。
4. **奇偶与事实不符**:回 400。唯一键冲突按约束分别映射成 `TXN_USED` 或 `DIFFERENT_RECEIPT`。

**应答体**:`{order, refundReason, code?, revision, fact, held}`。`order` 是订单视图(含 orderId 与 state),`refundReason` 说明这笔钱是否已排进退款、为什么;这两项 2026-11-10 已落码(`CommerceApi.PaymentOutcome`),其余随 V3 加。本稿原用一个 `code=ABNORMAL_REFUND` 标异常付款,适配器分不出重复付款与金额 / 币种不符,改为按原因给值。go/payment 按下表处理:

| 应答 | go/payment 本地动作 |
|---|---|
| 200,`revision ≥ 本次请求的 revision` | `mall_synced_revision = max(…)`;不覆盖本地 fact;`held=true` 时记本地 held 标记(供指标与 ListManual) |
| 200,`revision < 本次请求的 revision` | 告警(不应出现),继续推进 |
| 200 且 `refundReason` 非空 | `resolution=ABNORMAL_REFUND`;不进 EXTRA,不计合规已付;等退款待办。付款当时只会给出 `CANCELLED_LATE`(订单已取消,这笔随订单退)、`DUPLICATE_PAYMENT`、`AMOUNT_MISMATCH`、`CURRENCY_MISMATCH`;后两种说明渠道实收与订单冻结值对不上,另行告警 |
| `DIFFERENT_RECEIPT` | 充值 SKU 改走 EXTRA |
| `STATE_CONFLICT` | 退避重试 |
| `TXN_USED` | `REJECTED_FINAL` + 原因,进人工 |
| `NONCE_REPLAY` | 换新 nonce 立即重试 |
| **400 或未知 4xx** | `REJECTED_FINAL` + 原因,告警进 ListManual,**不重试**,由 RetryManual 处理 |
| 超时 / 5xx / 网络错误 | 退避重试 |

**全部安全的情形**:

- 在途推进与撤销,任意先后;
- 推进超时而商城其实已提交;
- 推进超时而商城确实没提交,随后撤销、再撤回:撤回时会按新修订号写 `entitled=1`,发满 A;
- 商城回 200 之后、go/payment 写本地之前崩溃。

### 8.1 go/payment 的库 `mmorpg_payment`

遵守 D-14:整数主键、每表一个唯一键、键列整列 `VARCHAR(N) utf8mb4_0900_bin`。

| 表 | 用途 | 唯一键 |
|---|---|---|
| `payment_intent` | 意图:order_sn、player_id、channel、sku_id、用途、预占额与月键 / 日键、状态、过期时间、尝试序号 | `order_key = "order:" + HMAC(K_bind, order_sn)`(≤ 50) |
| `payment_intent_ref` | 渠道引用 → 意图 + 尝试序号 | `ref_key`(≤ 100) |
| `payment_store_txn` | 平台交易事实 | `txn_key`(≤ 161) |
| `payment_notification` | 通知去重 | `notification_key`(≤ 161) |
| `payment_channel_identity` | openid ↔ player | `identity_key`(≤ 191) |
| `payment_spend` | 合规累计 | `spend_key`(≤ 96) |
| `payment_audit` | 审计 | 无 |

`payment_store_txn` 的列:交易信息(含 `submit_player_id`);`fact / fact_revision / last_observed_ms / voided_evidence_ms`;`mall_synced_revision / post_attempted / held / resolution / reject_reason / finalized`;`spend_identity / counted_month_key / counted_day_key / counted_amount`。

`resolution` 的取值:`BOUND / ORPHAN / OVER_LIMIT / REJECTED_RETRYABLE / REJECTED_FINAL / TEST_DENIED / ABNORMAL_REFUND / DENIED`。

**观测与对齐**:

1. **乐观并发**:
   - 先读基线 `(fact, fact_revision, last_observed_ms)`,再查平台;
   - 提交时 `FOR UPDATE`,只有 `fact_revision` 仍等于基线才应用;否则重查,最多 3 次,超过则延后并告警;
   - 首次 INSERT 撞键时回到重查;
   - 苹果观测的 `signedDate` 不大于 `last_observed_ms` 时丢弃。
2. **翻转**:观测事实不同,就翻转 fact,并让 `fact_revision + 1`。
   - 只有苹果允许 REVOKED → PAID,而且要求 GTI 实时显示 `revocationDate` 已消失。
   - 谷歌按 voided 证据规则处理。
   - 不能查单的渠道,修订号由事件类型决定:到账 = 0,退款 = 1。
3. **推进**:满足 `BOUND` 且 `mall_synced_revision < fact_revision` 就同步。
4. **重评**:ORPHAN / REJECTED_RETRYABLE 在新事件或定时重评时可转为 BOUND;OVER_LIMIT 只能经人工 RetryManual;撤销时只更新事实。

### 8.2 java/mall_game(V3)

形态照 V2 先例:`schema.sql` 与 `V3__recharge.sql` 同批改。`SchemaVerifier` 的 version 保持 1,核对清单**明确增删**:

- `commerce_delivery_intent` 的 `unique(order_sn)` 换成 `unique(order_sn, revision)`;
- 列核对增加:intent 的 `revision / entitled / trade_order_id / attempts / next_poll_ms`;receipt(守卫)的 `revision / fact`;
- 新增 `commerce_delivery_receipt unique(order_sn, revision)`;
- 新增 `commerce_refund_intent / commerce_refund_receipt unique(channel, transaction_id)`;
- 新表 `commerce_sku_currency_grant / commerce_order_currency_grant / commerce_sku_unlimited / commerce_close_intent` 加入存在性核对;
- `commerce_payment_receipt` 保留 `unique(channel, transaction_id)` 和 `unique(order_sn)`(order_sn 可空);
- `commerce_abnormal_payment unique(channel, transaction_id)`,列核对含 `reason`(2026-11-10 已落码,见第 1 点)。

这样 V3 程序在 V2 库上会因缺列拒启,V2 程序在 V3 库上会因缺唯一键拒启。理由见 D-17.1;Java 仓 game-mall 放到 R4。

1. **表结构**:
   - 新表:货币发放物两张、`commerce_sku_unlimited`、`commerce_close_intent`。
   - `commerce_payment_receipt` 改为守卫行:order_sn 可空,加 revision / fact。
   - `commerce_delivery_intent`:主键 `(order_sn, revision)`,加 `entitled`、`trade_order_id`、`attempts`、`next_poll_ms`;状态集合 `PENDING / ACCEPTED / HELD / SUPERSEDED / QUARANTINE / DENIED / 终态`。
   - `commerce_delivery_receipt`:加 revision,唯一键 `(order_sn, revision)`,state 取 `DELIVERED / VOIDED / MANUAL`。
   - `oms_order`:加单调递增的 `version` 列,每次状态变化 +1。
   - **异常付款**(已落码,未编译):钱进了渠道、却不能算作订单的付款,不回 409,同一事务里写三行:
     - `commerce_payment_receipt` 一行,`order_sn` 为 NULL(不绑定订单),与绑定的回执共用交易键主键,一笔交易全局只记一次;金额币种是渠道实收值;
     - `commerce_abnormal_payment` 一行:冲的是哪张订单、`reason`;每条 `order_sn` 为 NULL 的回执恰有一行;
     - 一条退款待办,`source` 与 `reason` 相同;订单状态、库存、交付意图都不动。

     `reason` 现有三种。`DUPLICATE_PAYMENT` 是订单已由另一笔付清,2026-11-05 落码,第一轮数据审计 §4 第 19 条;本稿原称 `ABNORMAL_SECOND` 的「非充值订单第二笔钱」即此。`AMOUNT_MISMATCH` / `CURRENCY_MISMATCH` 是金额或币种与订单冻结值不符,2026-11-10 落码,第二轮数据审计评审 R3;本稿原称的 `ABNORMAL_AMOUNT` 拆成这两个,因为适配器要分得清是金额错还是币种错。判定顺序:交易键已记过走重放;币种不同记 `CURRENCY_MISMATCH`(不同币种的金额不可比,不再比金额);币种同、金额不同记 `AMOUNT_MISMATCH`;金额币种都对、订单已付记 `DUPLICATE_PAYMENT`。金额币种先于订单状态判定,对不上的钱在任何状态下都不绑定订单:未付单仍待付款、保留预占,已取消单不转 REFUND_PENDING。
   - **重放**:同一交易键、同一订单、同一金额币种,回原结论(`refundReason` 取这笔付款的退款待办),不写任何东西;报给别的订单,或改了金额币种,回 409。同一订单的并发回调在订单行锁上排队,后到者走重放;两张订单并发报同一交易键,在回执主键上撞键,只有一张记下,另一个回 409。
   - **409 与 503 分开**:409 只表示适配器报来的这笔与已记下的冲突,适配器可当终局;存储不变量被破坏一律回 503,这笔钱没有记下,适配器保留、退避重试并告警。503 的情形:库存预占或订单状态对不上;已记下的回执、异常付款行、退款待办互相对不上;撞上唯一键,回滚后重读,交易键却不属于别的订单。
   - **退款待办与退款回执**:以被退那笔付款的 `(channel, transaction_id)` 为主键,带 `order_sn`、`source`(2026-11-05 已落码,未编译)。来源现有 `CANCELLED_LATE / DELIVERY_REJECTED`(退订单绑定的那笔)与上面三种异常付款原因,取值集中在 `CommerceApi.RefundReason`。待办不另存金额币种,取那笔付款回执的实收值:异常付款退的是玩家实际付的钱,不是订单价。回执表主键 `(channel, payment_transaction_id)`,退款流水 `(channel, transaction_id)` 另有唯一键:一笔付款只认一条退款流水,一条退款流水也只能了结一笔付款,复用回 409。仍待做:`OVER_LIMIT / HELD_DENIED / ADMIN` 来源与 `lease_until`。
   - 表改名:11-05 的 `commerce_duplicate_payment` 改为 `commerce_abnormal_payment`,加 `reason` 列。没写迁移,项目未上线,旧库按 `schema.sql` 新建重装。`SchemaVerifier` 在旧库上因缺表拒启;存储合同戳同时换新,防止有人在旧库上重跑 `schema.sql` 补出新表、却留着只认三种来源的退款待办 CHECK(闸门只核表、列和唯一键,测不出 CHECK)。
   - TiDB 上改主键:建新表 → 搬数据 → 改名。
2. **发放物二选一**:
   - `create()` 要求恰好一种;充值 SKU 的 `quantity=1`,跳过预占与未付上限;`cancel()` 和付款对无限库存跳过库存运算;
   - `skus()` LEFT JOIN 两种发放物,无限库存去掉 `stock>lock_stock` 过滤;
   - `poll()` LEFT JOIN 并校验,不在 mapper 里抛错,坏行用独立事务置 `QUARANTINE`;
   - `DeliveryApi.Intent` 增加 `currencyType / currencyAmount / revision / entitled`;
   - `DeliveryApi.Sku` 与 `OfficialSkuView` 带上发放物类型和无限库存标记(§19)。
3. **迟到付款复活**:见 §8.0 规则 1。只对充值 SKU;物品 SKU 保持现有语义。
4. **无原单入账**:`/unbound`(payment 角色,只对充值 SKU)。
   - `EXTRA` 必须带 `originOrderId`,核对:原单买家与 SKU 相同、原单已被另一条守卫行绑定、渠道相同。原单首笔尚未绑定时回 `STATE_CONFLICT`。
   - `ORPHAN_RESOLVED` 必须带审批 id。
   - **HELD**:同一原单的 EXTRA 超过 3 笔,或超出限速(按买家每日、全局每小时),照常建单、回 200,但待办置 HELD,应答 `held:true`,并告警。
   - **`POST /internal/payments/release-held`**(带审批 id):在守卫行锁下,按守卫**当前**的修订号与事实处理该订单所有未释放的 HELD:
     - fact = 已付:写或改写 `(order_sn, 当前 revision, entitled=1)` 为 PENDING,其余 HELD 置 SUPERSEDED;
     - fact = 已撤销:解除 HELD,旧行置 SUPERSEDED;如果 trade 还没收到当前修订号的 `entitled=0` 待办,补写一条 PENDING。
   - **`POST /internal/payments/deny-held`**(带审批 id):HELD 行置 DENIED,订单保持 MANUAL。能退款的渠道写退款待办(source=HELD_DENIED);不能退款的由 go/payment 记 `resolution=DENIED`。
   - **`GET /internal/payments/held`**(带游标):供 ListManual 使用。
5. **订单状态推导**,优先级从高到低:
   1. 事实为已撤销 → `REVERSED`(`CommerceOrderState = 12`,终态);
   2. 有未释放的 HELD 或 DENIED → `MANUAL`;
   3. 按 trade 对当前修订号的回执:DELIVERED → 2;MANUAL → 7;还没有回执 → 1。

   回执处理:
   - 同一 `(order_sn, revision)` 重放时比较结局与净额,不比较 proof;内容不同回 409;
   - 修订号更大的回执覆盖推导结果,更小的只记录;
   - VOIDED 只在事实为已撤销时有效;
   - 收到修订号 r 的终局回执时,同单 revision < r 的 ACCEPTED 一律置 SUPERSEDED;
   - **官方订单视图的 `version` 改用 `oms_order.version`**,不再取状态值,`mallAnswer` 同步修改。
6. **领取、退避、隔离**:
   - `poll()` 条件:`WHERE status='PENDING' AND next_poll_ms<=now ORDER BY next_poll_ms, created_ms`。
   - `POST /internal/deliveries/ack`:trade 提交后调用,PENDING → ACCEPTED。
   - `POST /internal/deliveries/nack`:处理失败时调用,`attempts+1`,`next_poll_ms` 按指数退避。
   - `POST /internal/deliveries/quarantine`:连续失败 5 次时调用,置 QUARANTINE 并告警;人工修复后放回 PENDING。
   - `receipt()` 接受从 PENDING 或 ACCEPTED 转为终态。
7. **机器码**(拒绝时的 `code`):`DIFFERENT_RECEIPT / STATE_CONFLICT / TXN_USED / NONCE_REPLAY`。金额或币种不符不再是拒绝码,与重复付款一样回 200 并排进退款,原因写在 `refundReason`;原 `ABNORMAL_REFUND` 码由 `refundReason` 取代(见 §8.0 应答体)。
8. **payment 角色只读接口**:
   - `/internal/payments/order-view`:返回 buyer / sku / 用途 / 数量 / 冻结金额与币种 / 状态 / version;
   - `/internal/payments/skus`;
   - `/internal/payments/held`。
9. **退款待办**:
   - `refunds()` 直接读待办自身的交易列(不再按 order_sn 联查,2026-11-05 已落码),每项带 `reason`(2026-11-10 已落码,`CommerceApi.PendingRefund`);带游标与租约(未做);
   - **适配器契约**:向渠道发起退款时,用被退那笔付款的交易号(待办项的 `transactionId`)作渠道退款的幂等键,渠道限制长度或字符集时由它确定性派生(如取摘要),发起前先按这个键查退款状态,已退或退款中就沿用结果、不再发起(§7 `Channel.Refund`)。不能用订单号:一张订单可能同时有本单那笔和几笔异常付款要退,按订单去重会让后几笔永远退不出去;
   - `refunded()` 按被退付款的 `(channel, transaction_id)` 幂等,回执体须带 `paymentTransactionId`(2026-11-05 已落码)。只有订单本身的退款(`CANCELLED_LATE`,以及物品 SKU 的 `DELIVERY_REJECTED`)才推进订单 5 → 6,异常付款不碰订单状态,也不碰已绑定的交易。退款流水已了结别的付款时回 409(2026-11-10 起;之前撞唯一键以 500 冒出,适配器会一直重试);
   - 新增关闭回执"渠道不支持退款,已转发货";
   - 只自动执行白名单里的来源;
   - 充值 SKU 拒收 trade 的 `REFUND_PENDING` 回执。
10. **路由 → 角色**:
    - payment:`payments`、`payments/unbound|order-view|skus|held|release-held|deny-held`、`refunds/*`、`payments/close-poll|close-receipt`;
    - delivery:`deliveries/poll|ack|nack|quarantine|receipt`;
    - Controller 再核对一次角色,并加 404 / 401 测试。
11. **渠道白名单**:按环境配置,生产环境不收 `mock*`。
12. **定时关单**:
    - 充值订单超时直接置 4。
    - 有限库存订单走关单握手(§21):扫描器把订单 CAS 为 `CLOSE_PENDING`(仍占 lock_stock),同一事务写 `commerce_close_intent`;go/payment 经 `close-poll` 领取后执行 Close 或 Query,回 `close-receipt{orderId, CLOSED|PAID}`。CLOSED 时置 4 并释放库存;PAID 时按 §8.0 绑定。

### 8.3 go/trade:每单收敛

1. **记录**:充值订单 `RequestKey("official", player, order_sn)`,workflow 里保存以下字段:
   - `entitled`、`mall_revision`、`grant_amount A`;
   - `written_off` 与 `written_off_revision`;
   - `leg_seq` 与各腿的结局,扣回腿另记本单欠款份额 `owed_i`;
   - `receipted_revision` 与待回执 `(revision, state)`。

   `net_granted` 在锁下由腿的结局推导:已 applied 的 credit 之和 − 已 applied 的 debit 之和 + 核销额 X 之和。
2. **事件**:权益待办 `(order_sn, revision, entitled)`。
   - 先 INSERT,撞键时在新事务里 `Lock(commerce_id)` 再处理,不得把撞键当成已处理。
   - `revision <= mall_revision` 的只 ack。
   - 否则更新 entitled 与 mall_revision,记录若已 Settled 改回 Delivering,提交后 ack。
   - 没有记录时收到撤销,建墓碑(`entitled=0`),墓碑不是终态。
   - accept 的两条路径(预读与撞键)走同一套逻辑。
3. **收敛**:
   - `target = entitled ? A : (written_off_revision == mall_revision ? written_off : 0)`;`diff = target − net_granted`。
   - 有在途腿时等待。
   - `diff > 0` 建 credit 腿(`TX_RECHARGE`);`diff < 0` 建 debit 腿(`TX_RECHARGE_REVERSAL`)。腿的 role 为 `recharge#<leg_seq>`。
   - `diff == 0` 且没有在途腿时,在锁事务里写待回执(target=A 为 DELIVERED,否则为 VOIDED),记录置 Settled。
4. **腿的结局**(**advance 判定结局时不只看 durable**,终态 status 都算:APPLIED / REJECTED / ABORTED / ABANDONED / APPLIED_PARTIAL;人工终结由 `resolved_by` 区分):
   - APPLIED:计入 net_granted;
   - ABORTED / ABANDONED:视为没有发生;
   - 带原因码的 REJECTED:转 Manual,写待回执 MANUAL;
   - Manual 记录收到新修订号时:已收敛就按新修订号写 DELIVERED / VOIDED 待回执并转 Settled;否则按新修订号写 MANUAL 待回执。
   - 解封后经 `ReplanCommerce` 重排。
5. **调度**:`next_check_ms` **只对 kind=1(官方/充值)生效**。Due 分两段,各有名额:
   - kind≠1:`status IN (…) ORDER BY updated_ms`,行为不变;
   - kind=1:`status IN (…) AND next_check_ms<=now ORDER BY next_check_ms`,另外加上 `receipted_revision < mall_revision` 的记录(不论 status,按退避)。

   其它规则:
   - 有在途腿时,`next_check_ms = min(退避, 1 小时)` 兜底复查。如需立即唤醒:Finalize、ResolveManually、Abandon 在事务外读出 ref_id,事务内按"先 Lock(commerce)、再锁 op 行"的锁序,同一事务置 `next_check_ms=now`。
   - 按记录隔离错误:单条出错就跳过、退避、告警;workflow 损坏的转 Manual。
   - op id 等到要建腿时才取。
6. **持久化回执**:
   - 收敛或转 Manual 时,只在锁事务里写待回执;
   - 提交后发回执(商城按 `(order_sn, revision)` 幂等);
   - 成功后 CAS `receipted_revision=r`,记录才离开 Due;
   - 商城回 409 时转人工并告警。
7. **扣回腿与核销**:
   - 放弃候选排除 `tx_type NOT IN (TX_RECHARGE_REVERSAL, TX_RECHARGE_WAIVE)`,Abandon 按同一集合再拒一次;`ExpiresMs` 与 `DeadlineMs` 都为 0。
   - 人工终结二选一:
     - `ResolveRechargeLeg`:凭证据记 APPLIED 或 ABORTED;
     - `WriteOffReversal`:要求 `entitled==0`。**在同一审批、同一锁事务里**先终结停放中的扣回腿(deadline 置为现在,改发 Abort,或人工记 ABORTED),再置 `written_off = net_granted`、`written_off_revision = mall_revision`。
   - 扣回已应用并产生了充值欠款时,核销通过核销腿执行(`TX_RECHARGE_WAIVE`):
     - 请求额 = 本单 `owed_i` 之和 − 本单已核销额;
     - 建腿时记下当时的 mall_revision;
     - 落地时 X 一律计入 net_granted;只有腿的修订号等于当前 mall_revision 时,才计入 written_off 并更新 written_off_revision;
     - APPLIED 却拿不到 X 时,转 Manual。
   - 停放超过 30 天的扣回腿进日对账与人工队列。
8. **可选**:entitled 变为 0 时,如果 credit 腿还在停放,把它的 deadline 提前到现在,改发 Abort。
9. **tx_type 下沉到腿**:`TradeCommerceLeg` 加 `tx_type` 字段;摘要兼容新字段。
10. 首次收敛为 DELIVERED 时调用 `CreateEventSnapshot(EVENT_RECHARGE)`。

### 8.4 proto 与 C++

- **交易类型**:`TX_RECHARGE = 30`、`TX_RECHARGE_REVERSAL = 31`、`TX_RECHARGE_WAIVE = 32`。
- **流规则**:
  - `kAssetOpMaxTxPerStream` 从 3 提到 5,所有行的 allowedTx 同步补齐;
  - `TRADE_CREDIT` 放行 `TX_RECHARGE`;`TRADE_DEBIT` 放行 `TX_RECHARGE_REVERSAL` 与 `TX_RECHARGE_WAIVE`;
  - C++ 单测覆盖:接受上述三种,其它流拒绝它们。
  - 不新增调用方,也不新增流。
- **扣回原语**:先扣 `d = min(余额, 应扣)`,差额记为充值欠款,永远 APPLIED。**`(d, 本次新增欠款)` 持久返回**:写进 AssetOpResponse 和账本结局记录(StoreReceipt 保留纯货币结局,或加专用字段)。重投经 AnswerSeenSeq 也拿得回同样的值。trade 把新增欠款记为该腿的 `owed_i`。
- **核销原语**:核销该玩家的充值欠款,上限为请求额;核销额 X 同样持久返回。
- **补发**:普通 `TX_RECHARGE`,线性。例:充 6480 后花掉 6000、退款 → 扣 480,欠 6000;再充 1000 被抵,欠 5000;撤回补发 6480 → 抵 5000,余额 +1480。
- **充值欠款单独成条**:
  - 欠款键为 `(currency_type, source)`(或另开 `recharge_debts`);Load 时重复判为损坏,fail-closed;
  - AddCurrency 先抵充值欠款,不受 frozen / expiresAt 影响;
  - GM 系列操作只作用于 `source=GM`;
  - GM 发放、邮件发放的响应里回报"被抵扣额";
  - 欠款大于 0 时余额必为 0(没有绕过 AddCurrency 写余额的旁路)。
- **proto**:
  - `payment.proto`:客户端 service;tip `PAY_INTENT_USED`。
  - `payment_admin.proto`:`MockNotify / ListManual / ResolveOrphan / RetryManual / ReleaseHeld / DenyHeld / FinalizeManual`。
  - `trade_admin.proto` 新增内部 RPC(不进客户端):`ListManualCommerce / ReplanCommerce / ResolveRechargeLeg / WriteOffReversal`,用独立令牌,配 NetworkPolicy,审计写 trade 库。
  - `payment_table.proto`。
  - `payment_error` tip 段。

### 8.5 部署与接入清单

| # | 项 | 契约测试 |
|---|---|---|
| 1 | `proto_gen.yaml` 与 `no-unity.yaml` 加 payment 块 | `dev_tools_proto_gen_exit.tests.ps1` |
| 2 | `$ProtoGenPostHooks` 加 `payment_postgen.py` | 同上 |
| 3 | D-14 建库、迁移 Job、连接总账、proto2mysql | `k8s_deploy_contract.tests.ps1` |
| 4 | 号段 biz_tag 四处同步 | 同上 |
| 5 | 路由表、`Assert-RouterForwardBudget`、`go_services.ps1`、镜像、manifest | `go_services_ports.tests.ps1`、`k8s_deploy_contract.tests.ps1` |
| 6 | `start_game.ps1` | `start_game_command_contract.tests.ps1` |
| 7 | **staging / prod:go/payment 固定 `Mode: pro`;go/trade 固定 `Market.Scope=global`、`AssetOp.Enabled=true`、`AssetOp.RequireSQLCommit=true`、`MySQL.RequireTiDB=true`**。修正 `trade.yaml` 与 `k8s_deploy.ps1` 里的 `Scope: zone` 镜像,列为 R1 前置项 | `k8s_deploy_contract.tests.ps1` |
| 8 | 通知入口单独端口和 Ingress,微信只能用 80/443 | 同上 |
| 9 | go/payment → data_service `GetPlayerField`(封禁预检)、`GetPlayerHomeZone`(白名单)的调用权限与超时预算 | 同上 |
| 10 | 发布顺序:基础设施 → payment-migrate → go/payment → 路由服 → gate;trade 先于商城 V3 | 发布说明 |
| 11 | `java/mall_game` 的 K8s 部署(R1 前置) | 新增 |

**本地联调**:`recharge-smoke` 需要 TiDB(`docker-compose.tidb.yml`,端口 4000),trade 用 global + 资产通道配置。

### 8.6 密钥

| 进程 | 商城密钥 | 其它 |
|---|---|---|
| go/payment | 只有 `payment` | 苹果 / 谷歌凭据、`K_bind`、`K_obf`、渠道商户密钥(按环境)、管理面令牌 |
| go/trade | `gateway` + `delivery` | 资产通道密钥、trade 管理面令牌 |

`payment` 密钥能给任意待付订单记已付。缓解措施:NetworkPolicy、K8s Secret、每日对账;`unbound` 另有结构核对、限速与 HELD。绑定路径没有限速,可选按买家设每日金额告警。

## 9. 退款与扣回

平台事件只当触发:go/payment 先观测、对齐,再同步商城;trade 收敛。

| 来源 | 事件 | 补漏 |
|---|---|---|
| 苹果 | ONE_TIME_CHARGE / REFUND / REFUND_REVERSED → 调 GTI 核实 | Notification History,每小时 |
| 苹果 | REFUND_DECLINED(触发超限终审);CONSUMPTION_REQUEST(v1 只记录) | — |
| 谷歌 | RTDN → 查 Voided Purchases 确认 → 写 voided 证据 | 每日 Voided Purchases(带 `includeQuantityBasedPartialRefund=true`) |
| 小游戏 | 退款推送;修订号按事件类型定 | 日对账 |
| 商城退款待办 | 只执行白名单来源;按幂等号先查再退 | — |

- **对账自动纠偏**:发现"平台已撤销、守卫已付"时,自动重查。
- **扣回语义**:先扣余额,差额记充值欠款。

## 10. 边界决定

- **10.1 迟到付款**:充值 SKU 一律复活发货;其它 SKU 按 §21 处理。
- **10.2 价格**:
  - 商店定价的渠道:商城的金额校验是恒等式,真正的保护来自 §4 启动核对和 §5.3 逐笔核对。
  - 我们定价的渠道:微信核对 `OrigPrice`(`ActualPrice` 只存不校验);抖音 CNY 核对 `amount_cent`,DIAMOND 单待核实,先进人工;微信支付 / 支付宝的实收必须等于标价。
- **10.3 一一对应**:`order_key`、`ref_key`、`txn_key` 加上商城守卫,共同保证一一对应。
- **10.4 交易号规范化**:

  | 渠道 | 交易号 |
  |---|---|
  | 苹果 | transactionId 原样 |
  | 谷歌 | `hex(sha256(purchaseToken))` |
  | 微信 | OutTradeNo |
  | 抖音 | cp_orderno |

- **10.5 测试交易与审核区**:
  - 生产服务器也能核验沙盒交易(审核员用正式包)。
  - 环境判定:苹果看 environment;谷歌看 `testPurchaseContext`;微信看 Env。
  - **只对白名单入账**:依据是意图玩家的 home_zone(经 `GetPlayerHomeZone`,查不到就拒绝)必须等于 `ReviewZoneId`。白名单项有有效期,有单日和累计上限;每笔都告警、写审计;使用独立渠道名(`apple_sandbox` 等),对账时排除。
  - **审核区隔离**:trade 强制全球市场,不能靠市场分区隔离。各转移入口在提交点按双方 home_zone 拒绝**审核区玩家与其它区之间的任何资产转移**(钻石、物品、宠物、游戏币),覆盖:
    - 聚宝斋(游戏币寄售与人民币寄售,审核区玩家既不能当买家也不能当卖家);
    - 拍卖、玩家交易、摆摊、求购;
    - 邮件附件;
    - 帮会捐献与仓库。

    查不到 home_zone 时 fail-closed。这是 R2 工作项,必须早于提审;排不进 R2 时,如实写明"测试钻石以及用它买到的物品可能流入正式经济,总价值由上限封顶",交用户拍板(§18.6)。
  - 白名单外的测试交易记为 `TEST_DENIED`;`testNotification` 只回 200。
- **10.6 入账失败**:credit 腿被拒时回执 MANUAL,**绝不退款**。

## 11. 安全

- 不信客户端;token 由订单确定性派生;密钥只放在环境变量或 Secret 里,日志只写变量名。
- **通知验签**:
  - 苹果按 §5.3 的清单;
  - 谷歌验 OIDC Bearer(签名、`iss`、受众、邮箱、`email_verified`、`exp`),RTDN 只当线索;
  - 微信、抖音按附录 A;
  - 验签失败不落任何状态。
- **mock 与管理面**:mock 只在 dev/test;两套管理面各用独立令牌,配 NetworkPolicy,双人审批。
- **日志**:玩家 id 不进指标 label;交易号只记后 6 位;原文不进日志。
- **限流**:客户端协议走 MessageLimiter;`unbound` 按 §8.2.4。

## 12. 合规

- **国内收费**:必须有版号(2016 年 44 号文)和实名(2021 年 14 号文),目前零实现。
- **未成年人限额**(2019 年通知,同一企业合并计算):

  | 年龄 | 单次 | 每月 |
  |---|---|---|
  | < 8 | 不得充值 | — |
  | 8–16 | 50 元 | 200 元 |
  | 16–18 | 100 元 | 400 元 |

  另外,《未成年人网络保护条例》第 44 条要求设单日上限,具体数字由用户定。

**闸的实现**:

- **主体**:实名身份(pi 或身份哈希);适用范围按实名状态 + 注册地区判断;storefront 只用于付款后核对;月键、日键按北京时间。
- **预占**:
  - `CreatePayIntent` 在 `payment_spend` 行锁下检查 `已付 + 全部在途预占 + 本次 ≤ 上限`,通过才预占,并把月键、日键记在意图上;允许多条在途意图。
  - 意图有效期:收据式渠道 72 小时;两段式渠道 = 支付窗口。
  - **商城关单和玩家取消都不释放额度**。额度只在两种情况下变化:意图过期、意图转为已用。理由:充值订单取消后,迟到的付款仍会让它复活。
  - 过期意图被复用时,在锁下重新预占。
- **转已付**:与释放在同一行锁下,用意图状态 CAS 二选一;从原键扣预占,在到账键计入,写 `counted_*`;到账周期与预占周期不同时,在到账周期重新核对。
- **其它入账**:复活、EXTRA、人工绑定都要计入并核对,超限转 OVER_LIMIT。
- **超限处理**:
  - 能退款的渠道:Refund;
  - 谷歌:不确认,等平台自动退款;
  - 苹果、小游戏:不发货,通知监护人,全程审计。
  - **终审**(REFUND_DECLINED 或满 30 天):
    - (a) 经 `RetryManual`,在**发货时刻所在周期**的行锁下重新核对单次、日、月上限,通过才发货并计入。监护人确认只是发起重试的前提,**不能豁免上限**。
    - (b) 不发货(终态 DENIED),由客服跟进平台退款。
    - **未满 8 周岁,或单笔超过该年龄段单次上限的,只能走 (b)。**
    - 审计记录写明所用周期键、年龄段和核对结果。
- **退款与撤回**:撤销按 `counted_*` 回退,最低为 0;撤回时重新计入并核对,超限则记合规事件并进人工(商城仍正常同步)。
- **启动闸**:开放国内充值但实名未接入时,拒绝启动。

**平台规则**:

- 苹果中国区游戏币必须走 IAP;佣金全球 30% / 15%,中国区自 2026-03-15 起为 25% / 12%。
- 谷歌 Billing 库 ≥ 8。
- 微信小游戏禁止定向转移道具(微信包要关闭聚宝斋、交易、拍卖),禁止付费抽奖诱导。

## 13. 客户端(Unity)

- Unity IAP 5.4.x 的 v5 流程,按 `finish_transaction` 决定是否 ConfirmPurchase。
- iOS:StoreKit 2,`SetAppAccountToken` 每次购买前设置,购买串行化。
- 安卓:设置混淆号;不开多数量购买。
- 启动时 `FetchPurchases` 重交;订单复用以意图状态为准。
- **订单状态以 `CommerceOrderView.version` 判断新旧**:同一订单单调递增,不表示状态。
- UI 提示:
  - `REVERSED` → "已退款";
  - `MANUAL` → "处理中,请联系客服";
  - 国内包在实名接入前隐藏充值入口;
  - 微信不支持虚拟支付时连商城都不显示。
- 国内安卓包不用 Unity IAP(R3)。

## 14. 配置与开关

```yaml
Mode: pro
Payment:
  Enabled: false
  Channels:
    mock:   { Enabled: false }
    apple:  { Enabled: false, BundleId: "", AppAppleId: 0, AllowedEnvironments: [Production, Sandbox],
              Production: { IssuerIdEnv: PAYMENT_APPLE_ISSUER_ID, KeyIdEnv: PAYMENT_APPLE_KEY_ID, PrivateKeyEnv: PAYMENT_APPLE_P8 },
              Sandbox:    { IssuerIdEnv: PAYMENT_APPLE_ISSUER_ID, KeyIdEnv: PAYMENT_APPLE_KEY_ID, PrivateKeyEnv: PAYMENT_APPLE_P8 },
              PriceTiers: {} }
    google: { Enabled: false, PackageName: "", ServiceAccountEnv: PAYMENT_GOOGLE_SA_JSON, PubSubAudience: "", PubSubServiceAccount: "", RefundRevoke: false }
    # wechat_minigame / douyin_minigame:由「微信抖音充值接入」会话补(production / sandbox 两组)
    # wechat_pay / alipay:R3,寄售等有限库存订单只能用这类渠道(§21)
  TestAccounts: { ListEnv: PAYMENT_TEST_ACCOUNTS, ReviewZoneId: 0, DailyCapMinor: 0, TotalCapMinor: 0 }
  Skus: [ { SkuId: 1001, ExpectedPriceMinor: 600, Apple: diamond_60, Google: diamond_60 } ]
  Mall: { BaseUrl: "http://127.0.0.1:18120", PaymentSubject: payment-adapter, PaymentSecretEnv: COMMERCE_PAYMENT_SECRET }
  Binding: { BindKeyEnv: PAYMENT_BIND_KEY, ObfKeyEnv: PAYMENT_OBF_KEY }
  AdminTokenEnv: PAYMENT_ADMIN_TOKEN
  Rescan: { OrphanEveryMinutes: 60, ReconcileDailyAt: "04:00" }
  FinalReview: { OrphanDays: 14, HeldDays: 7 }
  Compliance: { RealNameProvider: none, DailyLimitMinor: 0, PendingReserveHours: 72, OverLimitFinalReviewDays: 30 }
# 商城:commerce.channels-allowed(按环境);commerce.unbound.per-buyer-daily: 3;global-hourly: 200;extra-per-origin: 3;
#       commerce.delivery.nack-max: 5(超过即 QUARANTINE)
# trade:AdminTokenEnv: TRADE_ADMIN_TOKEN;官方记录兜底复查上限 1h;扣回停放告警 30 天
```

## 15. 可观测性、告警与对账

**指标**:

- 核验、通知、推进;
- 观测冲突重查次数;
- 孤儿、人工、HELD、DENIED 的数量;
- 谷歌未 consume 的最长时间;
- 苹果通知最后成功时间、补漏新事件数;
- trade 未收敛数、待回执数、扣回最长停放时间、单条推进连续失败次数;
- 商城待办的 nack 与 QUARANTINE 数量。

**告警**:

1. 已核实超过 10 分钟未被商城确认;
2. 当前修订号的待办领取后 30 分钟仍未收敛;
3. 谷歌超过 48 小时未 consume;
4. 验签失败率突增;
5. 人工队列非空;
6. 出现测试交易;
7. 补漏拉到新事件;
8. 长时间没有收到苹果通知;
9. unbound 达到阈值;
10. 扣回停放超过 30 天;
11. 单条推进连续失败;
12. 观测冲突重查超限;
13. 待回执超过 30 分钟未发出;
14. 出现 QUARANTINE;
15. 终审期限到期。

**对账**:

- 每日做三方差异核对:平台 ↔ 商城守卫 ↔ trade 收敛。测试渠道和核销额单列;"平台已撤销、守卫已付"自动重查。
- 苹果以 Notification History 为准;谷歌以 Voided Purchases 和 Orders 为准。
- 每月核对财务报表。

## 16. 测试与验收

**单测**:

- 苹果验签反例(含缺 OID、x5c 数量、alg、嵌套 JWS、价格不符);
- 谷歌事实映射(含 voided 证据不可推翻、测试购买、quantity);
- 观测乐观并发;
- 合规:
  - 边界、总量核对、跨周期、过期与到账并发、过期复用;
  - 撤销按 counted_* 回退、撤回重新核对;
  - **迟到款超限转 OVER_LIMIT(意图已过期或已释放后到账、关单后复活超限)**;
  - **玩家取消后 72 小时内 Ask to Buy 到账:预占仍在,不进 OVER_LIMIT**;
  - **终审 (a) 遇到超单次上限被拒;未满 8 岁只能走 (b)**;
- 商城守卫:修订号规则;撤销没有守卫行时绑定并置 REVERSED;`DIFFERENT_RECEIPT`;**非充值订单第二笔钱回 ABNORMAL_REFUND**;
- trade 收敛:
  - diff 正 / 负 / 零;role 唯一;ABORTED、ABANDONED 视为没有发生;
  - **人工终结的腿(无 durable)能被 advance 看到**;
  - Manual 记录收到新修订号;
  - **written_off 只对当前修订号有效:核销腿停放期间撤回,再退款时全额扣回**;**entitled=1 时拒绝人工核销**;
- **核销**:两单都退款、只核销其中一单时,另一单的欠款不变;核销应答丢失后重投,X 不丢;核销后撤回,只补 d;
- 充值欠款:与 GM 欠款共存、抵扣顺序、Waive 不触及;
- 商城回执与待办:同修订号幂等;SUPERSEDED;QUARANTINE;**nack 退避后按 next_poll_ms 排序**;
- **`oms_order.version` 在复活、Manual 收敛、撤回后仍递增**;
- **resolution 迁移边**:
  - ResolveOrphan 把 ORPHAN 转为 BOUND(谷歌凭混淆号,苹果凭 submit_player_id 并回写 token);
  - 定时重评把 ORPHAN / REJECTED_RETRYABLE 转为 BOUND;
  - OVER_LIMIT 在额度重置后不会自动转换;
  - ORPHAN / OVER_LIMIT 遇到撤销只更新事实;
  - 起点不符的人工动作被拒;
- C++ 流规则单测(每条流上限 5)。

**集成**(真实 TiDB,mock):

- 并发与崩溃:
  - 并发重交;两副本旧观测后提交;各崩溃点;
  - **推进超时且商城未提交 → 撤销 → 撤回 → 发满 A**;
  - **多副本 rev1 先落、rev0 后到 → 撤回 → 发满 A**;
  - **关单后先撤销再撤回 → 复活并发满 A**;
  - 微信退款推送先于发货重试到达 → 不发货、REVERSED;
- 订单与暂扣:
  - 复活;Ask to Buy 关单后到账;第二笔钱走 EXTRA;第 4 笔进 HELD;
  - **HELD → 退款 → 撤回 → 释放 → 发满 A**;**HELD → 退款 → 释放 → 不发货、REVERSED**;
  - **DenyHeld:谷歌退款回流后 REVERSED;苹果转 DENIED**;
  - 攒满 5 张未付单仍可下单;
- 收敛:
  - 撤销落在离线停放期间,净额为 0;墓碑后撤回;扣回挂起时撤回;退款 → 撤回 → 再退款;
  - 撤销 → DELIVERED → 撤回;扣回余额的三种情况;扣回离线超过放弃门槛;
  - **长期离线 + 人工核销 → 收敛为 VOIDED**;**核销腿超过放弃门槛也不被放弃**;
- 持久回执:
  - **回执超时、提交后发回执前崩溃 → 订单最终是 DELIVERED / REVERSED / MANUAL**;
  - **Manual 转换后回执失败 → 重发 MANUAL**;
- 规模:
  - 1000 条停放 + 1 条坏记录;
  - **21 条必失败的待办 + 1 条正常待办,正常的在 SLO 内发出**;
  - **1000 条 kind=1 停放 + 101 条以上寄售挂单 → 双方都在 SLO 内推进**;
  - **拍卖到期、寄售到期、市场撮合回归**;
- 异常付款与退款:
  - **同一订单两笔付款(非充值):第二笔退款,第一笔不受影响,订单保持已发货**;
  - **退款成功后、回执前崩溃 → 按幂等号查询,不重复退款**;
- 有限库存关单:**有限库存订单关单握手:CLOSED 释放库存;PAID 走绑定**;
- 审核区:**审核区玩家与其它区之间的钻石、物品转移一律被拒**(R2)。

**robot `recharge-smoke`**:mock 付款 → +60 → 退款 → 扣回 → 撤回 → 补回(需要本地 TiDB 与 trade global 配置)。

**沙盒**(R2):苹果沙盒账号、TestFlight;谷歌 License testers;谷歌退款字段与 `orders.refund` 的 revoke 实测。

## 17. 分期

| 期 | 内容 | 需要用户做的 | 依赖 |
|---|---|---|---|
| R0 | 本文 + 四轮评审;**第 5 版建议再复核一轮** | 拍板 §18 | — |
| R1 | go/payment 骨架 + mock;商城 V3(守卫、权益待办、领取 / nack / 隔离、HELD / 否决、退款待办改键、关单握手、order version);trade 收敛 + 持久回执 + 调度分段 + 人工管理面;C++ 扣回 / 核销 / 充值欠款 / 流规则上限;接入清单;recharge-smoke | **批准 30+ 文件的分批计划**(AGENTS §10.2);每批 proto 源与生成物同批,能独立编译,交 Codex | 合入代码的**编译基线**;**官方物品 SKU 冒烟**;**trade 配置改成 global + TiDB**(§8.5 第 7 项);mall_game 的 K8s 部署 |
| R2 | 苹果 / 谷歌真实接入 + 客户端 IAP + **审核区资产隔离** | 账号与商品(附录 B) | R1 |
| R3 | 国内实名、防沉迷、渠道;微信支付 / 支付宝(寄售也要用);小游戏插件 | 版号、bizID、商户号 | R1 |
| R4 | Java 版对齐 | — | R1 |

## 18. 待决问题

1. R1 是否等"编译基线 + 官方物品 SKU 冒烟"都通过再开工(推荐等)。
2. 钻石档位。
3. 是否回传苹果 `CONSUMPTION_REQUEST`。
4. 退款扣回之外的处罚,以及已托管钻石的处理。
5. Java 排期,以及客户端契约同批的做法。
6. 审核区与白名单:上限、审核区资产隔离能否在 R2 完成、"审核员新注册账号买不了"的风险。
7. 未成年人单日上限。
8. 限速阈值,以及孤儿 / 暂扣的终审天数(§14 的默认值)。

## 19. 双版本(AGENTS §12)

**两版共享的客户端契约**:

- `payment.proto` 的客户端 service;
- `payment_error` tip 段,含 `PAY_INTENT_USED`;
- `CommerceOrderState` 新增 `REVERSED = 12`;
- `CommerceOrderView.version` 的语义改为"同一订单单调递增,只用于比较新旧";
- `OfficialSkuView` 新增四个字段:`grant_kind`(枚举:物品 / 货币,字段号 6)、`unlimited`(7)、`currency_type`(8)、`currency_amount`(9)。`unlimited=true` 时 `available` 填固定上限,客户端不得据此显示库存。

这些契约按规则应与 Java 版同批落地,做不到时在 PARITY / PROGRESS 登记"待做 + 原因"并请用户确认。服务端实现放到 R4。

## 20. 修订记录

- **v1–v3**(2026-10-19):初稿;第一、二轮评审后加入修订号、每单收敛、线性欠款、领取式拉取等。
- **v4**(2026-10-19):第三轮评审后加入观测乐观并发、按渠道的可逆性、人工出口、谷歌事实映射、HELD、价格核对、审核区、合规跨周期。
- **v4.1**(2026-10-20):快照合入 main;R1 前置更新;Close;退款租约;§21 寄售接缝。
- **v5**(2026-10-20):按第四轮评审(23 条新发现,含 3 条 P0)修订:
  - **撤回时总是按新修订号写 entitled=1 待办**,HELD 由新待办继承;release-held 按守卫当前修订号重写;unbound 首个请求即撤销时建单并置 REVERSED(CC-01、CC-02,P0)。
  - **删掉 Market.Scope=zone**:trade 开资产通道时强制 global + TiDB,改为部署断言并修正 trade.yaml;审核区改为在各转移入口按 home_zone 拒绝全部资产转移(F1、MAIN-01,P0)。
  - advance 接受非 durable 的终态;调度兜底 ≤ 1 小时 + 立即唤醒的锁序;放弃排除扩到 WAIVE;核销与终结停放腿同事务(CC-03、MAIN-02)。
  - 扣回 / 核销结局持久返回,核销请求额按本单 owed_i(CC-04)。
  - written_off_revision(CC-05)。
  - 持久化的待回执与 receipted_revision(CC-06)。
  - 商城待办 nack / 退避 / 隔离(CC-07)。
  - next_check_ms 只对 kind=1 生效,Due 分段(CC-08)。
  - DenyHeld、FinalizeManual 与终审期限(F2)。
  - 超限终审 (a) 必须重新核对上限,未满 8 岁或超单次上限只能走 (b)(F3)。
  - 关单与取消都不释放额度(F4)。
  - trade 侧管理面,payment 侧 held 只读接口(F5)。
  - 非充值订单的意图路径与 ABNORMAL_REFUND(F6)。
  - 退款待办改为按交易键,带来源;退款幂等号;撤销按用途分支(F7、MAIN-03)。
  - 关单握手(F8)。
  - 资产通道现状更正,每条流上限提到 5(MAIN-04)。
  - oms_order.version(MAIN-05)。
  - SchemaVerifier 明确增删(MAIN-06)。
  - 封禁预检数据来源(MAIN-07)。
  - OfficialSkuView 新字段进入双版本契约(MAIN-08)。
  - 应答表补"400 / 未知 4xx",修正误告警条件;补 resolution 迁移边、迟到款超限用例;orders.refund 的 revoke 列入 R2 实测,默认 false。
- **v5.1**(2026-11-10):按商城落码对齐。金额或币种不符改为与重复付款同形处理,记异常付款(`commerce_abnormal_payment`,带 reason),回 200;付款应答用 `refundReason` 取代 `code=ABNORMAL_REFUND`,适配器能分清重复付款与金额 / 币种不符;退款待办项带原因;退款幂等键按被退那笔付款写进适配器契约。改动在 §7、§8.0、§8.2 第 1、7、9 点、§21。

## 21. 与聚宝斋人民币寄售的接缝

寄售计划(另一会话的工作树 `xuanming-server-mmo-wt-rmbconsign`,未合入)第 13 步用 go/payment 替换 mock 付款器。约定如下:

- **按订单用途分流**:order-view 返回用途。
  - 充值 SKU 走 §5–§10。
  - **其它 SKU 走"非充值意图路径"**:
    - 不查 §14 的静态映射,金额与币种取 order-view 冻结的 `pay_amount / currency`;
    - **渠道只允许"我们定价 + 有开发者退款接口 + 支持关单或按商户单号查单"的两段式渠道**,用显式白名单(`wechat_pay`、`alipay`),排除 apple、google、wechat_minigame、douyin_minigame;
    - 不过 §4 的启动价格核对。
- **异常付款**:非充值订单收到第二笔钱、关单后才到账、金额或币种不符时,商城按 §8.0 写退款待办(按交易键,带来源),回 200,`refundReason` 依次为 `DUPLICATE_PAYMENT`、`CANCELLED_LATE`、`AMOUNT_MISMATCH` / `CURRENCY_MISMATCH`;第二笔钱与金额币种不符的另写异常付款行,不绑定订单。go/payment 以被退那笔付款的交易号作幂等号调 `Channel.Refund`。
- **关单握手**:见 §8.2.12。收据式渠道不参与。
- **撤销**:按 §8.0 的"其它 SKU"分支,只更新守卫,不写权益待办。物品与实例的冲回由寄售计划定义;trade 收敛只覆盖货币。
- **开工顺序**:充值 R1 先落地骨架(含非充值意图路径、异常付款、关单握手、退款待办改键);寄售第 13 步再接入。微信支付 / 支付宝渠道在 R3。

## 附录 A 平台事实(2026-10-19 核实,带官方链接)

**苹果**

- **核验**:用 GTI `GET https://api.storekit.apple.com/inApps/v1/transactions/{id}`,沙盒为 `api.storekit-sandbox.apple.com`。[文档](https://developer.apple.com/documentation/appstoreserverapi/get-transaction-info)
- **鉴权**:ES256 JWT,带 `kid / iss / aud=appstoreconnect-v1 / bid`,有效期不超过 60 分钟,用 `.p8` 签名。[文档](https://developer.apple.com/documentation/appstoreserverapi/generating-json-web-tokens-for-api-requests)
- **验签**:规格对齐官方库的 `AppleExtensionCertPathChecker` 和 `ChainVerifier`。官方库只有 Swift / Java / Python / Node 版本;`verifyReceipt` 已废弃。[Java 库](https://github.com/apple/app-store-server-library-java)
- **appAccountToken**:必须是 UUID;可用 Set App Account Token 写回。[文档](https://developer.apple.com/documentation/appstoreserverapi/appaccounttoken)
- **通知 V2**:ONE_TIME_CHARGE / REFUND / REFUND_DECLINED / REFUND_REVERSED / CONSUMPTION_REQUEST(须 12 小时内回应)。生产环境重试 5 次,沙盒只发 1 次;漏掉的用 Notification History 补。[类型](https://developer.apple.com/documentation/appstoreservernotifications/notificationtype) [重试](https://developer.apple.com/documentation/appstoreservernotifications/responding-to-app-store-server-notifications)
- **审核**:审核员用沙盒付款,生产服务器需要能核验沙盒交易。[文档](https://developer.apple.com/documentation/storekit/validating-receipts-with-the-app-store)
- **中国区佣金**:2026-03-15 起为 25% / 12%。[公告](https://developer.apple.com/news/?id=dadukodv)

**谷歌**

- **Billing 库**:最新 9.1.0;2026-08-31 起必须 ≥ 8。[发布记录](https://developer.android.com/google/play/billing/release-notes)
- **productsv2**:不含价格;字段有 `purchaseState`、`testPurchaseContext`、`quantity / refundableQuantity`、两个 obfuscated 字段。文档没有承诺退款会改 purchaseState。实收用 Orders API;退款用 `orders.refund`。[文档](https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.productsv2)
- **consume**:同时算确认;3 天内不确认会自动退款。[文档](https://developer.android.com/google/play/billing/lifecycle/one-time)
- **服务账号**:不需要关联 Cloud 项目。[文档](https://developers.google.com/android-publisher/getting_started)
- **RTDN 与 Voided Purchases**:[RTDN](https://developer.android.com/google/play/billing/rtdn-reference) [Voided](https://developers.google.com/android-publisher/voided-purchases)
- **费用**:开户 25 美元;多数市场首 100 万美元收 15%。[费率](https://support.google.com/googleplay/android-developer/answer/112622)

**Unity IAP**:5.4.4,StoreKit 2,Billing 9.0.0,`SetAppAccountToken`;只支持 App Store / Google Play。[API](https://docs.unity3d.com/Packages/com.unity.purchasing@5.4/api/UnityEngine.Purchasing.IAppleStoreExtendedService.html)

**微信小游戏**(会话「微信抖音充值接入」核实)

- **道具直购**:
  - 接口 `wx.requestMidasPaymentGameItem`,服务端在 `/wxa/game/*`;
  - `signData` 字段:mode=goods、offerId、buyQuantity、env、currencyType=CNY、platform、zoneId、productId、goodsPrice、outTradeNo、attach;
  - outTradeNo 不超过 32 字符,字符集 `[0-9A-Za-z_-|*@]`,不能以下划线开头,只能用一次。
- **签名**:
  - `paySig = hex(HMAC-SHA256(AppKey[env], "requestMidasPaymentGameItem&" + signData))`;
  - `signature = hex(HMAC-SHA256(session_key, signData))`;
  - AppKey 分正式和沙箱两把;iOS 没有沙箱。
- **推送**:
  - 事件 `minigame_game_pay_goods_deliver_notify`,验签串为 `Event + "&" + Payload`;
  - Payload 字段:OpenId / Env / OutTradeNo / GoodsInfo{…OrigPrice, ActualPrice…} / PayChannel;
  - 回包为明文;3 秒超时;只能用 80/443 端口;推送 15 次,约 24 小时;`IsMock` 推送只回成功。
- **退款**:有 `minigame_pay_refund_succ_notify`;没有主动退款接口。
- **查单**:uri 未核实,可能需要用户签名。
- **iOS**:2026-01 起经苹果 Mini Apps Partner Program 开放(iOS 15+、基础库 3.10.3+、微信 8.0.68+)。
- **分成与开通**:iOS 70%(限时约 88%),安卓 60%。开通需要已认证主体、实名系统、版号。

**抖音小游戏**(同上会话核实)

- **安卓**:`tt.requestGamePayment{mode:"game", goodType:2, orderAmount, customId…}`,没有服务端签名。
- **iOS**:`tt.openAwemeCustomerService`,`DIAMOND`,1 元 = 10 钻;`amount_cent` 的口径待核实。
- **回调**:
  - token / timestamp / nonce / msg 按字典序排序后拼接,做 SHA1;
  - msg 含 appid / cp_orderno / order_no_channel / amount_cent / currency;
  - 回 200,否则重试 16 次。
- **查单**:`queryPayState`,返回 `unsuccess` 不等于终局。
- **其它**:没有沙箱,没有主动退款接口;需要版号并在 wlc.nppa.gov.cn 添加渠道;开发者分成一般为 60%。

**国内其它**

- **华为 IAP**:`tokens/verify` 接口,Basic APPAT 鉴权,RSA 验签;rootUrl 未核实。
- **微信支付 APIv3**:
  - APP 下单;
  - 回调验签,用 AEAD_AES_256_GCM 解密;
  - 5 秒内回包,重试 15 次,注意探测包;
  - 退款用 `out_refund_no` 做幂等。
- **支付宝**:`rsaCheckV1` 验签;回包规则未核实。
- **QuickSDK**:仍在更新;常见分成约 5:5,未核实。
- **法规**:[版号](https://www.nppa.gov.cn/xxfb/zcfg/gfxwj/201606/t20160602_4492.html)、[实名](https://www.gov.cn/zhengce/zhengceku/2021-09/01/content_5634661.htm)、[充值限额](https://www.gov.cn/zhengce/2019-11/05/content_5448848.htm)。两份征求意见稿均未生效。

## 附录 B 用户需要亲自办的事(Claude 不能代办)

1. **苹果**:
   - 加入 Developer Program(688 元/年);
   - 签付费协议,填银行与税务信息;
   - 建 6 个消耗型商品;
   - 生成 `.p8`;
   - 建沙盒账号;
   - 配置通知 V2 地址;
   - 提审时在审核备注里提供审核区演示账号。
2. **谷歌**:
   - 注册 Play Console(25 美元);
   - 建 6 个一次性商品,**不开多数量购买**;
   - 建服务账号并授权;
   - 配置 Pub/Sub;
   - 添加 License testers。
3. **国内(R3)**:版号、bizID、渠道账号、小游戏虚拟支付开通、微信支付 / 支付宝商户号(寄售也要用)。
4. 密钥**不要发到聊天里**。写进环境变量或 Secret,告诉 Claude 变量名即可。
