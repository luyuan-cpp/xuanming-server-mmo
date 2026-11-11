# 聚宝斋人民币寄售：买卖主流程分步计划（D-17 mall 路线）

> **状态：计划稿 v2（2026-10-19），还没写任何代码。** 用户要求按 D-17 的 mall 路线做聚宝斋买卖主流程（下单 → 支付 → 发货 → 卖家入账），先出分步计划。
>
> - **基线**：服务端 `origin/main` 的 `c63b22dca2`，客户端 `origin/main` 的 `d6338af1`。服务端这一版已经包含快照分支 `origin/snapshot/all-changes-20261018`：另一台机器在 10-19 08:50 用 `fc3ff045ee`「合并全部改动到main并统一邮件排行与恢复协议」合入，提交说明写着"按用户要求跳过测试和服务构建"。快照和 `origin/main` 上的 `go/trade`、`java/mall_game`、`proto/trade` 逐字相同，下文行号对两边都适用。
> - **调研方式**：
>   1. 7 路只读核对：交易引擎、商城、支付与摆摊、客户端、上线闸、Java 版、分支合并；
>   2. 三份不同角度的计划草稿：风险优先、最快演示、复用最多；
>   3. 两名评审打分并逐条核实（资金安全、完整性）；
>   4. 主会话复核关键断言后写成 v1；
>   5. 三路终审：事实核对、资金安全、可读性。v2 已按终审意见修正。
>
>   全程只读，没有编译、没有运行任何东西。
> - 本文是计划，不是设计终稿。§5 的选择题拍板后，由第 1 步写成正式设计。

> **2026-10-20 合并补记**：主工作区的历史合并冲突已收尾，本文与 D-17.6 决策已合入主干；D1–D4 在 2026-10-19 已按推荐拍板。§5 中尚待拍板、旧基线落后及工作树未提交等表述是本计划形成时的历史快照，当前状态以本补记和后续验证为准。独立正式设计稿尚未落入仓库。

---

## 0. 先读这节

### 0.1 一句话

**卖家把东西交给"交易行柜台"保管 → 买家在商城付人民币 → 柜台把同一件东西交给买家 → 商城给卖家记一笔收益，过了冷静期才能提现。**

### 0.2 用一笔具体交易走一遍

| 步骤 | 发生了什么 | 谁说了算 |
|---|---|---|
| ① 上架 | 小明上架一把 +5 青锋剑，标价 ¥12.34。剑从他背包里扣出，交给柜台保管 | 剑还在不在背包：C++ scene；剑是否在柜台：`go/trade` |
| ② 公示 | 公示期（默认 48 小时）内大家都能看到，但还不能买 | 时间以商城数据库时间为准 |
| ③ 下单 | 小红点"购买"，商城给她开一张 ¥12.34 的订单。这件商品库存只有 1，被她锁住 15 分钟，别人买不了 | `java/mall_game`：谁先锁到库存算谁的 |
| ④ 付款 | 小红付款。本机测试阶段用 mock 付款器代替真实支付 | 付款事实：商城，只认带 payment 签名身份的调用方 |
| ⑤ 发货 | 柜台把**同一把剑**交给小红，编号、强化、成长属性都不变 | 剑进背包：C++ scene；存档真正写进数据库后才算发完 |
| ⑥ 卖家入账 | 商城给小明记收益：1234 分 − 手续费 62 分（5%，向上取整）= **1172 分**，状态"冷静中" | 卖家收益账：商城 |
| ⑦ 冷静期满 | 3 天后变成"可提现"。提现本身不在这一期 | 商城 |

为什么不让两个人"一手交钱一手交货"？因为两人的存档各存各的。如果服务器在两次存盘之间崩溃，一把剑可能变成两把（`stall-system.md` §0 有同样的例子）。走柜台的话，每一步只改一个人的存档，而且每一步都带唯一编号；崩溃后照原编号重发，做过的不会再做一遍。最坏的结果是到账晚一点。

### 0.3 现在有什么、缺什么

| | 现在 | 要新做 |
|---|---|---|
| 柜台保管与交货 | **有**：游戏币寄售引擎 `go/trade/internal/commerce`。快照 PROGRESS 记载有真实 TiDB 用例；10-17 在另一台机器上双区实跑过一次：一区装备 GUID 40001 卖到二区，17 金币结算 | 新开一种"人民币寄售"合同（kind 9）。不改游戏币寄售，摆摊依赖它 |
| 商城订单、付款收据、发货待办、退款待办 | **有**：`java/mall_game`，只服务官方商品 | 一次性挂单商品（库存 = 1）、到期关单、回执加固 |
| 买家付人民币 | **没有**：没有任何 payment 签名身份的调用方，也没有 mock 付款 | 本机先用 mock 付款器；以后接 `go/payment`（归「苹果和安卓应用充值」会话） |
| 卖家人民币收益账 | **没有**：两个分支 grep 都是 0 | 收益账、手续费、冷静期 |
| 客户端 | **有**：交易中心 `CommerceWindow` 覆盖 20 个合同方法，只能用游戏币；原生聚宝斋窗口用旧目录的 4 个方法，只能看不能买 | 人民币上架、购买、待付款、卖家收益 |
| 账号维度 | **没有**：`SessionDetails` 只有角色号 `player_id`，没有账号字段（`proto/common/base/session.proto:6-26`） | 拦截同账号小号互买。要改 login → gate C++ → trade，是单独的一步 |

### 0.4 要多久

- **到本机演示**（第 0–11 步，mock 付款跑通"上架 → 下单 → 付款 → 到货 → 卖家看到 ¥11.72 冷静中"）：工作量合计约 **46 人日**。其中约 2 人日由「道具交易系统代码」会话承担，另外还要 Codex 的编译测试机时；第 0 步不计在内。这是工作量合计，不是日历工期：第 4/5、9/10/11 步可以并行。
- **到预发环境**：再加约 18 人日（第 12、15 步）。
- **到真钱上线**：工程上再加约 20–30 人日（第 13、16 步）；资质、商户签约、实名系统、法务意见的周期，工程侧控制不了。
- **Java 版**：AGENTS §12 要求两版都做，但 Java 仓不在这台机器上（`D:\luyuan` 不存在），工作量估不了（第 14 步）。

### 0.5 你现在要做的

1. 回答 §5 里标"阻塞"的 4 道题（D1–D4；D5、D6 已定）。其余题不回答，就按推荐做。
2. 主工作区里有一个不知道是谁发起的 `git pull`，卡在 2 个冲突文件上（见 §1）。请指定一个会话去收尾。

### 0.6 术语表

| 词 | 白话 | 例子 |
|---|---|---|
| 托管 | 东西从卖家背包扣出，由柜台（`go/trade`）保管 | 剑离开小明背包，但还没到小红手里 |
| 腿（资产腿） | 对某一个玩家的一次资产改动，带唯一编号（seq），可以重发 | "从小明身上扣剑"是一条腿，"给小红加剑"是另一条 |
| durable / 已落盘 | scene 确认这次改动已经写进存档 | 只有 durable 的结局才能拿来推进合同 |
| SQL 提交证据 | 证明玩家资产已经写进 MySQL/TiDB，而不只是写进了 Redis | 发货完成必须等到这份证据 |
| `Fencing.Mode=dual` | go/db 的一种写入模式。只有开了它，才会发布上面那份 SQL 提交证据 | 没开 dual，交易会一直停在"发货中" |
| CAS | 先比对再修改，两个人同时抢，只有一个能成功 | "库存还是 1 才改成 0" |
| 墓碑 | 一条"这件商品已下架"的记录，用来挡住迟到的上架请求 | 网络慢，上架请求晚于下架请求到达，也会被拒 |
| 签名身份 | 商城用 HMAC 签名来认调用方是谁：gateway、payment、delivery、listing | 只有 payment 身份能报"已付款" |
| TiDB | 全服共用的分布式数据库，兼容 MySQL | 商城、交易都用它 |
| 人工（MANUAL） | 自动流程没法安全判断时停下来，等人处理 | 物品被封禁，退不回卖家 |
| b 档 / c 档 | 上线闸的档位：b = 开预发或共享环境前；c = 开真钱前 | — |

---

## 1. 现状（2026-10-19 核实，以服务端 `origin/main` 为准）

| 事实 | 证据 | 证据强度 |
|---|---|---|
| 游戏币固定价寄售 Kind=3 的流程：`escrow → open → pay → deliver → done`，到期或撤单走 `return` | `go/trade/internal/commerce/engine.go:248-256`、`:529-567`、`:568-597`、`:606-642` | 快照 PROGRESS 记载有真实 TiDB 用例和一次双区 1/1；本机没有日志 |
| 四条资产腿：`seller_debit`（卖家托管）、`buyer_debit_*`（买家扣款）、`buyer_receive`（把托管原件交给买家）、`seller_receive`（把买家的游戏币交给卖家） | `engine.go:530`、`:569-580`、`:590`、`:593`；`Transfer` 在 `:144-179` | 同上 |
| 寄售只能用游戏币标价：`CreateConsignmentRequest` 只有 `currency_type` 和 `price` | `proto/trade/commerce.proto:116-122`；`engine.go:245-252` | 代码 |
| 客户端可见的 `CommerceOrderKind` 枚举只到 5；浏览只接受 kind 3/5、只列 open/lot_open；订单视图把 `currency_type` 原样当币种输出 | `commerce.proto:17-24`；`go/trade/internal/logic/commerce_logic.go:469`、`:484`、`:31` | 代码 |
| 商城桥只认官方商品：交付意图只有一个 `itemConfigId × itemCount`，接收时永远新建 Kind=1 合同；`Mall.Tick` 每轮重新领取所有待处理意图，收到回执前一直重领 | `mall.go:213-250`、`:253-279`；`DeliveryStore.java:37-42` | 代码；只对 httptest 假商城测过 |
| 商城不区分订单用途：任何订单付款后都会写交付意图 | `CommerceStore.java:137-149` | 代码 |
| 商城没有卖家、手续费、收益、冷静期；不会到期关单（超时仍保留预占） | `db/schema.sql` 所有表都没有卖家列；`CommerceStore.java:69` | 代码 |
| 发货回执的 UPDATE 不检查影响行数；退款待办没有租约 | `DeliveryStore.java:75-84`、`:87-94` | 代码 |
| 同一订单只能记一张付款收据、一个退款意图（唯一约束） | `schema.sql:51`、`:74` | 代码 |
| 启用开关默认全关；开资产通道必须是全局市场加 TiDB；配了白名单或商城却没开 SQL 证据，就拒绝启动 | `go/trade/etc/trade.yaml:98`、`:156`、`:164`、`:194-203`；`config.go:569-584` | 代码 |
| 交易到账必须有资产 SQL 提交证据，这份证据只在 `Fencing.Mode=dual` 下才发布 | `jubaozhai-market.md` §15.1 末条（负责人 10-16 拍板） | 文档 + 代码 |
| 客户端交易方法共 24 个：旧目录 196/197/198/200，新合同 20 个分散在 266–305，与邮件、排行等交错；199 是运营接口 `TradeAdminSeedListing`；神秘商人是 314–316 | `proto/message_id.txt` | 代码 |
| 客户端 main（`d6338af1`）已按 `origin/main` 的编号生成，包括神秘商人 314–316，和服务端逐号一致 | `mmorpg-client/Assets/Scripts/Net/MessageIds.cs` | 代码（终审用脚本比对过） |
| 审计 §2.5 两条 medium 还没修：一张坏单堵住全服交易；卖家资产被扣作人质 | `data-overwrite-rollback-audit-20261018.md:157-158` | 文档 |
| 角色交易零代码：P0-b 账号目录落库、P4 过户都没做；账号目录仍只在 Redis | `go/login/.../loginlogic.go:343-349` | 代码 |
| 实名、防沉迷、未成年人消费限额都没实现 | 全仓检索；`payment-recharge.md` §2.1（未跟踪文件，以 10-19 工作区为准） | 检索 |
| **本机主工作区有一个未完成的 `git pull`**：本机 main ahead 5 / behind 19，冲突文件 2 个（`player_battle.cpp`、`go/player_inbox/go.mod`），不知道是谁发起的 | `.git/MERGE_HEAD = c63b22dca2`，13:30 | 实测 |

### 1.1 三套状态编号（读下文时别混）

| 表.列 | 值 |
|---|---|
| 商城 `oms_order.status` | 0 待付款、1 待发货、2 已发货、4 已取消、5 退款中、6 已退款、7 人工（`CommerceStore.java:171`） |
| 交易 `trade_commerce.status` | 1 等待确认、3 托管中/在售、4 发货中、6 撤销中、7 已取消、8 官方单交付失败转退款、10 人工、11 已结算（`engine.go:26-35`）；`Due` 只扫描 1、3、4、6、8（`commerce_repo.go:70-72`） |
| 客户端 `CommerceOrderState` | 2 待付款、4 待发货、5 已发货、7 已取消、8 退款中、9 已退款、10 人工、11 已结算（`commerce.proto:26-39`；`mall.go:205` 的映射） |

下文提到状态时一律写成"表.列 = 值（中文名）"。

### 1.2 三份草稿共同踩过的坑（主会话已复核，计划里已改正）

1. **被放弃或人工终结的扣款腿不写 durable，这是有意设计，不能改成写 durable 来让合同继续往下走。**
   - 人工终结不写托管快照，只有 `Finalize` 写（`asset_op_repo.go:853-860`、`:884-886`）。
   - `Policy.Snapshot(nil)` 直接返回 true（`engine.go:104-107`）。
   - 后果：如果把人工判为 APPLIED 的卖家托管腿当成功，商品会照样上架；买家付款后，`Transfer` 因"托管完整快照缺失"失败（`engine.go:149-151`），钱收了、货发不出。
2. **背包满、宠物满在 C++ 里返回"稍后重试"（RETRY），而且不记账**（`asset_op_system.cpp:715-716`、`:727`）。入账腿也不会被自动放弃，所以"背包满 → 自动退款退货"这个用例根本造不出来，真实情况是"付了钱，货一直挂着"。
3. **物品封禁（GainBlock）对两个方向都可能持久拒绝**（`asset_op_system.cpp` 的 `ApplyCredit`；2026-11-05 起判定是全服封禁 ∨ 收货方本人被 GM 单独封禁，`player-inbox-service.md` §2.2）。全服封禁时，交付被拒之后如果自动退回卖家，退回也会被同一条检查拒掉，原件两头落空；单玩家封禁只拦被封的那一方，但卖家同样可能被封，所以仍按下文保留托管、转人工。
4. **人民币合同不能走通用的撤单和到期路径。**
   - 入口有两个：`CancelConsignment`（298），以及带纯数字订单号的 `CancelCommerceOrder`（300）。两者都会走到 `cancelCommerce → Mutate("cancel")`（`commerce_logic.go:167-183` → `engine.go:359-380`）。
   - 通用 `plan` 遇到"在售 + 到期或请求取消"时，直接生成 `seller_return`，不经过商城下架（`engine.go:606-628`）。
   - 另外，人民币合同的 `currency_type=0` 和金币同值，任何按 `currency_type` 读取的路径（例如 `engine.go:354` 的买家扣款）都不能对它生效。

---

## 2. 设计要点（拍板前的推荐形态）

### 2.1 谁是什么的唯一权威

| 东西 | 唯一权威 | 不许出现的第二份 |
|---|---|---|
| 玩家身上的物品、货币 | C++ scene，落库以 SQL 提交证据为准 | 商城的"库存"不是玩家背包 |
| 柜台里的原件（完整实例字节） | `go/trade` 的 `trade_commerce` + `trade_asset_op` | 商城只存合同引用，不存原件 |
| 人民币订单、付款收据、退款 | `java/mall_game` | trade 不记"已付" |
| 卖家人民币收益 | `java/mall_game` 的收益账（推荐，见 D3） | `trade_commerce` 的设计约束是"绝不保存玩家余额"（`trade_table.proto:164`） |
| 游戏币收益（摆摊） | 摆摊的 `trade_stall_income` | 两本账不互转 |

### 2.2 人民币寄售合同（kind 9）的阶段

阶段名全部新起，不复用 Kind 3 的 `open` 和 `pay`。**谁是唯一买家由商城裁决**：一次性商品的库存只有 1，复用现有的 `lock_stock` CAS，以及"付款和取消争同一行锁"的逻辑。trade 不设"锁单"阶段。

```
托管 rmb_escrow ─成功─▶ 待挂出 rmb_publishing ─商城建好一次性商品─▶ 在售 rmb_listed
  └失败─▶ 已取消

卖家主动撤单（任何阶段都先调商城 close，close 会写墓碑）:
  CLOSED ─▶ 退回 rmb_returning ─▶ 已取消
  LOCKED ─▶ 拒绝本次撤单（"有人正在付款，请稍后"），合同留在 rmb_listed
  SOLD   ─▶ 拒绝撤单，合同转 已售待发货 rmb_sold

寄售到期: 合同进 rmb_unpublishing，按退避重复 close:
  CLOSED ─▶ rmb_returning
  LOCKED ─▶ 继续等（这期间照样接收付款意图）
  SOLD   ─▶ rmb_sold

商城发来"已付款"意图（必须命中冻结的 commerce_id、金额、卖家）:
  合同在 rmb_listed / rmb_unpublishing / rmb_sold 时都接收 ─▶ 发货中 rmb_delivering

rmb_delivering:
  买家到货且 SQL 证据到手 ─▶ 回执 DELIVERED（商城在同一事务里给卖家记 PENDING）─▶ 已结算
  交付被拒且原因可退（见 2.4）─▶ 退款中 rmb_refunding: seller_return + 回执 REFUND_PENDING
  交付被拒但退不回（封禁等）─▶ 人工
```

**付款意图的幂等规则**：`Mall.Tick` 每轮都会重新领取所有待处理意图，同一张订单可能被重放成千上万次，所以规则要写死。

- 合同第一次接收某张订单时，把订单号 `order_sn` 绑定到合同上，只绑定一次。
- 再收到同一个 `order_sn` 的意图：直接返回当前合同，回执只在合同到达终局后才发。
- 合同已经绑定了别的 `order_sn`，或者合同已退回、金额对不上：说明出了 bug，库存 = 1 时正常不会发生。此时这张**新订单**回执 REFUND_PENDING 并告警。可以安全退款的理由是：没有为这个 `order_sn` 发出过任何 `buyer_receive`。
- **没有"阶段对不上就退款"这种兜底。**

**阶段与 `trade_commerce.status` 的对应**（不变量：只要合同还有没终局的腿，或者还在等商城的结果，它就必须留在 `Due` 扫描范围内）：

| 阶段 | status |
|---|---|
| `rmb_escrow`、`rmb_publishing`、`rmb_listed` | 3 |
| `rmb_unpublishing`、`rmb_returning` | 6 |
| `rmb_sold`、`rmb_delivering` | 4 |
| `rmb_refunding` | 8 |
| 已结算 / 已取消 / 人工 | 11 / 7 / 10（终态，沿用通用值） |

**合同腿只有三种**：`seller_debit`（托管）、`buyer_receive`（= `Transfer(seller_debit)`，交付原件）、`seller_return`（退回原件）。没有买家游戏币扣款腿，也没有卖家游戏币入账腿，用断言测试守住。第 13 步加"扣回腿"时，这条断言改为按阶段放行。

**首批每张挂单只放一件装备（一个 GUID）。** C++ 入账时先发可叠加物、再放原件，后一步失败会记成"部分到账"（`asset_op_system.cpp:739-772`、`:807-827`）。只放一件装备，就从根上消除了部分到账，第 5 步的校验会拒绝混合包。

### 2.3 必须逐格证明的状态

第 1 步要列一张"合同阶段 × `oms_order.status` × 收益账状态"组合表，逐格写明物在哪、钱在哪。以下几条不满足就不合入：

1. **钱已收、物不在买家**：只允许出现在两种情况下。一是"发货中"，即有没终局的 `buyer_receive`；二是"人工"，即 `oms_order.status` = 7（人工）加合同人工，并且有告警和处理时限。其余情况必须已有退款意图。
2. **物已到买家、却没有付款收据**：不可达。
3. **同一个 GUID 在全局出现两份**：不可达。
4. **物在买家、订单却已退款**：必须同时存在扣回待办或欠款记录。第 13 步之前不开放"交付后退款"，所以在第 13 步之前这一格不可达。
5. **金额恒等式**：卖家实得 + 手续费 = 买家实付，并且实得 ≥ 1 分。

### 2.4 交付被拒怎么办（按原因分）

| 原因 | C++ 怎么答 | 处理 |
|---|---|---|
| 买家离线 | NOT_HERE，不记账 | 保持发货中，买家上线后自动补发 |
| 背包满或宠物满 | RETRY，不记账 | 保持发货中，客户端提示"背包满，腾出空间后自动到账"；超过 N 小时告警，可走人工出口（第 8 步）。**本期不做自动中止**，原因见 D13 |
| 物品被封禁（GainBlock：全服，或买家本人被 GM 封禁） | 拒绝并记账 | 退回卖家也可能被拒，所以**保留托管、转人工**；商城订单转 `oms_order.status` = 7（人工） |
| 重复 GUID 或非法包 | 拒绝并记账 | 转人工，告警 |
| 意图绑定冲突（见 2.2 的幂等规则） | — | 新订单回执 REFUND_PENDING，告警 |

**人工终局要按"腿 × 结局"逐格处理**（第 2、7 步落地）：

| 腿 | 人工判 APPLIED | 人工判 REJECTED / ABORTED |
|---|---|---|
| `seller_debit` | 没有托管快照 → 合同转人工，**绝不上架** | 合同按"托管失败"结束 |
| `buyer_receive` | 商城回执"人工"；收益账只能经人工出口确认，不自动记 | 带证据时 → `rmb_refunding`（退回卖家 + 退款）；没有证据 → 人工 |
| `seller_return` | 合同结束，写审计记录 | 合同转人工 |

### 2.5 要你批准的偏离

1. **D-17.2 导语段和第 2 条。** 两处都要求在同一个 TiDB 事务里裁决托管归属、订单和结算意图；第 2 条还要求"同事务写交付 outbox"。本计划里，托管原件在 trade 库，订单和收益在商城库，**做不到同一个事务**。要合成一个事务，只有两条路：两个服务写同一个库，这违反 D-14"每服务一库"；或者把原件搬进商城，这违反"商城不持有游戏资产"。替代机制如下：
   - 唯一买家由商城库存 CAS 裁决；
   - 退货前必须拿到商城的 CLOSED，并写墓碑；
   - 付款意图必须命中冻结快照，并按订单号幂等；
   - 收益账和发货回执在同一个事务里写；
   - 三方之间有常驻对账（第 9 步）。
2. **D-17.4"没有渠道关单证明不自动释放未知支付"。** 本机 mock 阶段和真钱上线前，挂单订单到期由商城直接关单、释放库存，关单后才到的付款只退款。真钱阶段（第 13 步）改为先向渠道查单或关单，再释放。

草稿里曾引用 D-17.2 第 3 条来论证这样做合规。但第 3 条管的是"外部支付渠道与本地账本"之间的关系，不涉及这里。所以这两处都是**偏离**，需要你明确批准；批准后补写进 D-17。

---

## 3. 分步计划

### 3.0 总览

| 步 | 做什么 | 谁做 | 依赖 | 估时（人日） |
|---|---|---|---|---|
| **0** | 本机 main 合并收尾 + 全量编译测试基线 | 收尾人由你指定；Codex | — | Codex 1–2 天机时 |
| **1** | 拍板、三会话号段与文件归属登记、组合表（纯文档） | 你拍板；Claude 起草 | —（与 0 并行） | 1.5 |
| **2** | 交易引擎加固（三会话共用前置，只由一个会话改） | Claude；Codex | 0、1 | 3.5 |
| **3a** | 本机联调底座；游戏币寄售经 gate 跑通 | Claude；Codex | 0 | 2.5 |
| **3b** | 官方商品第一次真实 Go↔Java 闭环 + 可复用的 mock 付款器与签名向量 | 「道具交易系统代码」会话（D6 已定，已在隔离工作树落码，未编译）；Codex | 0、2、3a | 2 |
| **4** | 商城加固，含到期关单扫描器（唯一商品上架前） | Claude；Codex；充值会话评审 | 1、3b | 4.5 |
| **5** | trade：人民币合同 kind 9 前半程（托管、撤单、到期退回；没有买家路径） | Claude；Codex | 2 | 4.5 |
| **6** | 上架桥：一次性挂单商品 + 下架 CAS + 墓碑（下单入口仍关着） | Claude；Codex | 4、5 | 4 |
| **7** | 下单 → 付款 → 发货 → 记收益（同一步、同一个开关）+ 失败回退 | Claude；Codex | 3b、6 | 9 |
| **8** | 冷静期 + 卖家收益查询 + 人工出口 | Claude；Codex | 7 | 3.5 |
| **9** | 三方常驻对账 + 卡单指标 + 告警 | Claude；Codex | 7 | 3 |
| **10** | robot 端到端 + 故障注入矩阵 | Claude 写脚本；Codex 实跑 | 8 | 3.5 |
| **11** | 客户端：人民币寄售界面 | Claude；Codex 或 Claude 跑 Unity（按惯例确认） | 7（联网验收要 8） | 5 |
| — | **里程碑 A：本机 mock 闭环演示** | 你现场验收 | 0–11 | — |
| **12** | 账号身份与风控（会话加账号字段，跨 C++/Go） | Claude；Codex | A | 6 |
| **13** | 接 `go/payment`；GUID 锁与扣回；交付后退款与撤销冲回 | 充值会话主导渠道；Claude 做寄售分支 | A、充值 R1（真实渠道再等 R3） | 6 + 充值会话 |
| **14** | Java 版同批 | Java 线；你提供仓库路径 | A | 待定 |
| **15** | 预发 / 共享环境门禁（b 档） | Claude 写工具与清单；Codex 演练；运维 apply | A | 12 |
| **16** | 真钱上线门禁（c 档） | 你、法务、商务；Claude；Codex 沙箱 | 12–15 | 工程 15–25 + 外部周期 |

**能并行的**：第 0、1 步；第 3a 和第 2 步；第 4 步（只动 `java/mall_game`）和第 5 步（主要动 `go/trade` 的新文件，共享文件 `engine.go` 只加分派，必须在第 2 步合入之后）；第 9、10、11 步。

**写法约定**（每一步都适用）：

- **默认关闭**：新功能（第 5 步起）由 `Commerce.Rmb.Enabled=false` 关着。第 2、4 步是对现有路径的修复，不受这个开关控制，靠回归用例（如 `OfficialSkuUnaffected`）守住兼容。
- **新逻辑写新文件**（`engine_rmb.go`、`mall_listing.go`、`Listing*.java`），共享文件只加分派，方便串行合入和单独回滚。
- **迁移只加表、加列**；每一步单独提交，可以 revert。
- **本地联调用单独的覆盖配置文件**，不改 `go/trade/etc/trade.yaml`，因为 `k8s_deploy.ps1` 会把它原样镜像进集群。
- **所有改动在隔离 worktree 里做**，不碰主工作区。
- **按 AGENTS §10.1**，Claude 只写代码和给 Codex 的命令；没有 Codex 收据之前，一律写"未编译，待 Codex 验证"。
- **新增客户端 RPC 一律走完整套接线**，漏一项就会被静默拒绝或走默认限流：
  1. 发消息号（生成器，316 之后）；
  2. 加进 trade 会话白名单（`go/trade/internal/session/session.go`）；
  3. 重新生成 C++ 路由和 gate 的客户端消息判定，并重编 gate；
  4. 配 MessageLimiter（不配就走默认 3 次/秒）；
  5. 在 `Tip.xlsx` 发新的 tip 码，与摆摊串行导表；
  6. 重新生成客户端 handler（带 `-ProtoRoot`）；
  7. 同步 robot vendor；
  8. 在 `PARITY.md` 登记 Java 版待同步项。Java 版手写的 `TradeTips` 在 Java 仓里，不在本仓。

### 3.1 第 0 步：本机 main 合并收尾 + 全量编译测试基线

- **为什么**：交易代码已经在 `origin/main` 上了，但 D 机器那次合并跳过了测试和构建，而那次合并涉及 2094 个文件。另外，本机 main 落后 19 个提交，主工作区里还卡着一个归属不明的 `git pull`。
- **做什么**：由你指定的会话在主工作区把这次 `git pull` 收尾，其他会话都不碰；然后 Codex 在合并后的 main 上跑全量编译和测试。
- **验收**：
  - C++：`game.sln` 串行 `/m:1` 全量编译 exit 0。`game.sln` 里的 25 个测试工程，加上单独编译的 `cpp/tests` 下 `ecs_context_test`、`rpc_controller_test`、`aoi_delivery_test`、`muduo_grpc_test` 4 个工程，共 29 个测试程序全部通过。
  - Go：全模块 `build`/`vet`/`test` 通过，`go/trade` 的 commerce 真实 TiDB 用例 0 SKIP，并且**第一次在本机**留下日志。
  - Java：`java/mall_game` 的 `mvn verify` 通过，含真实 TiDB。
  - PowerShell 契约测试通过。
  - 客户端 `MessageIds.cs` 与 `proto/message_id.txt` 用脚本逐号比对一致。
  - `robot login-test` 23/23。
- **注意**：收尾之前，本机服务端 main 落后 origin/main 19 个提交，客户端已经是新编号，这时不要拿本机服务端 main 去联调。

### 3.2 第 1 步：拍板、登记、组合表（纯文档）

- **交付**：
  1. §5 选择题的答复；
  2. 正式设计，包括 §2.2 状态机与幂等规则、§2.3 组合表、§2.4 拒收与人工终局表、§2.5 偏离的批准记录；
  3. 三会话共享资源登记表（草案见 §4.4）；
  4. 一页给新手看的流程说明，即 §0.2 那张表。
- **验收**：组合表里 §2.3 的五条逐格有结论和理由；充值、摆摊两个会话在各自的设计稿里引用这张登记表；设计文件在隔离 worktree 里落盘。

### 3.3 第 2 步：交易引擎加固（三会话共用前置）

`engine.go` 的这几处只由本计划修改，摆摊和充值复用，避免三个会话同时改同一个文件。

- **单条出错不拖累全局**：`Commerce.Tick` 里某个合同的 `advance` 出错时，只推迟这一条，并新增 `next_check_ms` 列（经 schemamigrate 加列），其余合同照常推进（`engine.go:768-822`）。`Mall.Tick` 同理，单条意图出错只跳过这一条（`mall.go:258-262`）。
- **被放弃或人工终结的腿**：**不伪造 durable**。在 `LegResult` 里显式识别"放弃"和"人工终局"，按 §2.4 的"腿 × 结局"表推进。需要实例快照的腿遇到 `Snapshot == nil`，**转人工**，不判失败；判失败会走进退回分支，而退回需要的正是同一份快照，结果每个 Tick 都报错重试。
- **官方单失败后的 `trade_commerce.status` = 8**：把 `official_refund` 收口成终态，或者按 kind 过滤。**不要把 8 从 `Due` 里删掉**，因为第 7 步的 `rmb_refunding` 也用 8，还要靠 `Due` 推进退回腿。
- **新增低基数指标** `commerce_tick_row_errors_total{kind,stage}`，不带 player_id。
- **验收**（每条都要先在旧代码上跑红，再在新代码上跑绿）：
  - `TestTickIsolatesFailingContract`：合同 A 返回 `ErrTooManyPending`，同一轮里合同 B 照常推进；
  - `TestMallTickIsolatesReplayIntent`；
  - `TestManualAppliedEscrowWithoutSnapshotGoesManual`：人工判 APPLIED、没有快照的托管腿不上架；
  - `TestAbandonedEscrowDebitEndsContract`；
  - `TestAbandonedBuyerDebitReopensListing`：Kind 3 在 pay 阶段，买家扣款腿被放弃或人工判 REJECTED 后，合同回到在售，卖家的托管不被扣住。这正是审计"人质"场景；
  - `TestKind3NilSnapshotGoesManual`：Kind 3 回归；
  - 故障注入：一个离线买家塞满 16 条未决入账，另一对买卖双方的寄售仍然在 3 个 Tick 内成交。
  - 以上全部通过后，在审计 §2.5 把两条标为已修，附上 Codex 收据。

### 3.4 第 3a 步：本机联调底座 + 游戏币寄售经 gate 跑通

- **trade 本地覆盖配置**：`AssetOp.Enabled=true`、`RequireSQLCommit=true`、`Market.Scope=global`、本地 TiDB；白名单只放 1 件测试装备，并配上名称；`AllowInstances=true`。
- **商城拉起**：`start_game.ps1` 加可选开关拉起 `mall_game`（dev 模式、独立库、执行 `schema.sql`），默认不拉起；扩展 `assetop_dev_secret.ps1`，注入商城的几把密钥。
- **runbook**：
  - 每个 zone 的 go/db 启动日志里都要出现 `save protocol generation: mode=dual`；
  - `AllowUnsafeCrossNodeHandoff=false`；
  - 使用新库和新 robot 账号，因为没有存档屏障的旧存档会被拒绝入场；
  - 内存门槛 4–6 GiB。
- **robot `commerce-consign-smoke`**（成功标记 `COMMERCE_CONSIGN_SMOKE_OK`）：游戏币寄售经 gate 跑通一次。此前只有引擎直调的证据。
- **验收**：冒烟连跑 3 遍全过；`login-test` 不回归。

### 3.5 第 3b 步：官方商品第一次真实 Go↔Java 闭环

由「道具交易系统代码」会话做（D6 已定：用户当面交给了它）。截至 10-19，代码在隔离工作树 `E:/work/xuanming-server-mmo-wt-mallarrival`（分支 `test/mall-official-arrival`），**还没提交，也没编译**。

- **robot `mall-arrival-smoke`**（成功标记 `MALL_ARRIVAL_SMOKE_OK`，账号 `robot_9925`）：下单 → mock 付款 → `Mall.Tick` → 到账 → `oms_order.status` = 2（已发货）。原先用的 `robot_9921` 和摆摊 `stall-smoke` 撞号，已改成 9925；9921/9922 留给摆摊。
- **可复用的 mock 付款器**：`robot/commerce_mock_payer.go` 的 `newCommerceMockPayer` / `RecordPayment`，以 payment 签名身份签名。D5 已和充值会话对齐：作为 dev 冒烟的例外，渠道名统一用 `mock-` 前缀。本计划第 7、10 步直接复用它。
  - "商城在 prod 模式下拒收 `mock-*` 渠道"**目前商城里还没有**，归充值会话第 3 版的渠道白名单负责。在它落地之前，生产包里没有任何东西拦得住 mock 收据，所以 mock 付款器绝不能接触非本机环境。
- **跨语言签名测试向量**：三处取值相同，分别是 `go/trade/internal/commerce/mall_signature_test.go`、`robot/commerce_mock_payer_test.go`、`java/mall_game/src/test/java/com/game/mall/HmacSignatureGoldenTest.java`。
- **故障注入**：
  - 接收落库后、发回执前 kill trade，重启后用同一 seq 恢复，只到账一次；
  - 同一张付款收据重放 3 次，只产生 1 个交付意图；
  - 买家离线时付款，上线后到账一次；
  - delivery 密钥错误时返回 401，Tick 不会整轮中断。

### 3.6 第 4 步：商城加固（唯一商品上架前）

- **回执只改恰好 1 行**：发货、人工、退款三种回执里，对 `oms_order` 和 `commerce_delivery_intent` 的 UPDATE 必须恰好影响 1 行，否则拒绝并转人工（`DeliveryStore.java:75-84`）。
- **退款待办加租约** `lease_until`（`DeliveryStore.java:87-94`），防止多个 payment 副本重复向渠道退款。
- **到期关单扫描器只做一个**，和充值会话共用，按订单用途分支：
  - 充值 SKU：迟到付款让订单复活（充值稿 §10.1）；
  - 挂单商品：迟到付款**只退款、不复活**，因为东西可能已经卖给了别人。
  - 扫描器由谁负责，在第 1 步登记表里定。如果归充值会话，那么**挂单分支已上线**就是第 7 步的硬前置：没有扫描器，未付款的挂单订单会一直占着库存，卖家撤单和到期都会一直卡在 LOCKED。
- **新增"异常付款"表**：同一订单的第二笔付款、金额或币种与订单不符的付款都记进去，再交给支付适配器退款。原来这两种情况会被唯一约束或 409 直接拒掉，钱停在渠道那边。
  - 已落码（未编译），表名 `commerce_abnormal_payment`，带 `reason` 列。两类付款都记成 `order_sn` 为 NULL 的付款回执加异常付款一行，回 200，订单、库存、交付都不动：
    - 第二笔付款：2026-11-05 落码，原因 `DUPLICATE_PAYMENT`；
    - 金额或币种不符：2026-11-10 落码，原因 `AMOUNT_MISMATCH` / `CURRENCY_MISMATCH`，任何订单状态下都不绑定订单。
  - 退款待办与退款回执按被退付款的 `(channel, transaction_id)` 识别，退款回执须带 `paymentTransactionId`。付款应答的 `refundReason` 告诉支付适配器这笔要退及原因，适配器以被退付款的交易号作渠道退款幂等键（充值稿 §8.2 第 1、9 点）。
  - 验收用例 `SecondPaymentRecordedAndRefunded` 对应 `CommerceStoreTest` 的「已付订单再收到另一笔付款」「重复付款的退款回执只终结那一笔」；金额币种不符对应「付款金额与订单不符」「付款币种与订单不符」「金额不符的同一流水重放幂等」及两条并发用例。
- **新增 listing 签名身份和第四把密钥**（`HmacAuthentication.java:36` 的路由集合）。四把密钥必须互不相同，否则拒绝启动。
- **未付单上限**：每个买家最多 5 张未付单（`CommerceProperties.java:17`），要不要按订单用途分开计数，第 1 步定。
- **迁移**：V4（加固），以登记表为准；同步改 `schema.sql` 和 `SchemaVerifier`。
- **验收**：
  - `CloseVsPayRace`：同一订单并发 200 次，每次结局恰好是"已付"或"已关单并有退款意图"之一，库存守恒；
  - `DeliveredReceiptOnNonPendingOrderGoesManual`；
  - `RefundPollLeaseNoDoubleDispatch`；
  - `SecondPaymentRecordedAndRefunded`；
  - `OfficialSkuUnaffected`；
  - 原有真实 TiDB 用例 0 失败、0 跳过。

### 3.7 第 5 步：trade 人民币合同 kind 9 前半程（没有买家路径）

- **协议**：
  - 新开 `CreateRmbConsignment` / `CancelRmbConsignment`，按 §3.0 的接线清单接全；
  - 价格用 `price_fen`（uint64，单位：分），不带 `currency_type`；
  - 客户端可见枚举 `CommerceOrderKind` 加值 9，属于客户端契约，两版同批改；
  - 浏览复用 `BrowseCommerceOffers`，放开 kind 白名单和阶段过滤（`rmb_listed`）；
  - 订单视图对 kind 9 专门输出（单位分、币种 CNY），不输出 `currency_type=0`。
- **引擎**：
  - kind 9 走独立阶段（§2.2）；
  - 通用的 `Mutate`、`plan`、`orderView` 对 kind 9 一律显式拒绝，或走专属分支，`CancelConsignment` 和 `CancelCommerceOrder` 两个入口都要拦住；
  - 配置加载时拒绝把人民币写进 `CurrencyTypes`；
  - 一张挂单只能放一件装备（一个 GUID）。
- **配置**：
  - `Commerce.Rmb.Enabled=false`；
  - 独立的实例白名单；
  - 价格上下限；
  - 每个卖家的在售上限；
  - 公示期、寄售期时长。
- **本步的临时形态**：托管成功后停在 `rmb_publishing`，这一步还不调用商城；撤单或到期直接退回。第 6 步接上商城后，改为任何阶段都必须先 close，拿到 CLOSED 才退。
- **验收**：
  - 单测：`RejectsCurrencyFields`、`PriceBounds`（0、低于下限、高于上限、溢出）、`RejectsMixedBundle`、`DisabledReturnsFeatureDisabled`、`GenericCancelConsignmentOnKind9Rejected`、`GenericCancelCommerceOrderOnKind9Rejected`、`GenericBuyOnKind9Rejected`、`NoStream3BuyerDebitLegForKind9`、`CancelReturnsExactlyOnce`、`ExpiryReturnsExactlyOnce`；
  - 真实 TiDB：用同一请求号重放，seq 和收据都不变；
  - 故障注入：托管腿 applied 之后、落库之前 kill trade，重启后不会重复扣出；
  - 开关关闭时，24 个既有方法和 Kind 3 的行为都不变。

### 3.8 第 6 步：上架桥（下单入口仍关着）

- **商城**新增 `/internal/listings/{publish,close}`，用 listing 签名身份：
  - `publish`：按 `commerce_id` 幂等建一个库存 = 1 的挂单商品，号段与官方商品分开；同时写 `commerce_listing` 扩展表（`commerce_id` 唯一、卖家、价格、费率、公示结束、寄售结束）。publish 时用当前费率校验 `price − ceil(price × bps / 10000) ≥ 1`，不满足就拒绝。
  - `close`：执行下架 CAS（`published=0 WHERE stock=1 AND lock_stock=0`），返回 CLOSED / LOCKED / SOLD 三种结果，并写墓碑，迟到的 publish 一律拒绝。
  - 官方下单接口必须拒绝挂单商品：没有 `commerce_sku_grant` 时下单本来就会回滚，这里加一条显式用例守住。
- **trade**：
  - `rmb_publishing` 阶段可幂等重试；
  - 撤单和到期按 §2.2 处理：卖家撤单遇到 LOCKED 拒绝，到期遇到 LOCKED 继续等，SOLD 转 `rmb_sold`。
- **"我的订单"去重**：同一笔交易的合同和 `mall:` 订单只显示一条。现在 `ListCommerceOrders` 会把两边合在一起列出（`commerce_logic.go:256-265`）。
- **验收**：
  - publish 丢包重放，只产生 1 个商品；
  - close 先到、publish 后到，被墓碑拒绝；
  - 用官方下单接口下挂单商品被拒；
  - 没有订单时撤单，退回恰好一次；
  - trade 和商城两侧各至少 2 个真实 TiDB 用例。

### 3.9 第 7 步：下单 → 付款 → 发货 → 记收益（同一步、同一个开关）

这四件事放在一步里合入，并且共用一个开关，避免出现"能收钱但不能发货"或者"发了货但没记收益"的空窗期。

- **下单**：
  - 新 RPC `BuyRmbConsignment`（按 §3.0 接线）：客户端 → gate → trade → 商城。trade 以 gateway 签名身份、买家作为 subject，调用现有的 `/api/orders`。
  - 商城下单时，挂单商品读 `commerce_listing`，在同一事务里校验：公示期已过、寄售期未过（以商城数据库时间为唯一时钟）、买家 ≠ 卖家（本步按角色号判，按账号判在第 12 步）。
  - 价格、手续费金额、实得金额、卖家、`commerce_id` 一起冻结进 `commerce_order_listing`。
- **付款**：`CommerceStore.recordTrustedPayment` 加挂单分支，交付意图只出现在 `/internal/listings/poll`。官方的 poll 和 receipt 必须拒绝挂单订单；反过来，挂单端点也必须拒绝官方订单。
- **发货**：
  - trade 按 §2.2 的幂等规则接收意图，进入 `rmb_delivering`，发出 `buyer_receive = Transfer(seller_debit)`，交付整件原件；
  - 同一意图被多个 trade 副本领取，也只产生一条 `buyer_receive` seq；
  - 挂单意图的幂等摘要不包含会随配置变化的展示字段（例如现取的标题）。现有官方路径的标题已在下单时冻结，不受影响。
- **记收益**：
  - 拿到 SQL 提交证据后，trade 调 `/internal/listings/receipt`（listing 签名身份）回执 DELIVERED；
  - 商城在**同一事务**里把 `oms_order.status` 从 1 改成 2，必须恰好影响 1 行，并写 `commerce_seller_ledger` 一行 PENDING，金额直接抄下单时冻结的值，不重算。
- **失败回退**：按 §2.4 的两张表实现。
- **迁移**：V5，一次建好 `commerce_listing`、`commerce_order_listing`、`commerce_seller_ledger`；如果第 6 步已建前两张，这里只建收益账表，按登记表执行。
- **验收**：
  - 两个买家同时对同一挂单下单，连跑 100 轮，每轮恰好 1 单进入待付款；
  - 公示期内下单、寄售期后下单、自己买自己，都被拒；
  - **到期前 1 分钟下单、到期后付款**：交付成功、不退款，原件也不退给卖家；
  - **发货途中同一意图重放 3 次**：零退款，只有一条 `buyer_receive`，最终恰好一条 DELIVERED；
  - `buyer_receive` 写入待办后、发回执前 kill trade：只到账一次；
  - 暂停 go/db 消费，让 SQL 证据暂不到达：这期间不发 DELIVERED；
  - 买家物品被封禁：保留托管、转人工，卖家那边不出现退回；
  - 写入收益账行之后注入异常：整个事务回滚；
  - 手续费取整：价格 1 分、2 分、9999 分、上限、溢出；
  - 用官方回执端点处理挂单订单被拒，反之亦然。

### 3.10 第 8 步：冷静期 + 卖家收益查询 + 人工出口

- **冷静期**：只用 CAS 推进 PENDING → AVAILABLE，带版本号；不采用"读取时按时间判定"。之后的撤销冲回以落库状态为准。
- **收益查询**：新 RPC `GetMySellerIncome`，按 §3.0 接线。
- **人工出口**：带审计表，只提供两个出口，并都和账绑定：
  1. **确认交付**：要求 `buyer_receive` 已有 SQL 证据；走 DELIVERED，同一事务记收益账。
  2. **退款**：要求 `buyer_receive` 已被人工终结为 REJECTED（带证据，经 `assetopfix`）；trade 发出 `seller_return`，商城走退款。
  - **禁止只在商城侧把 `oms_order.status` 从 7（人工）改成 5（退款中）**。
- **只读导出**：卖家收益导出，供以后提现使用。
- **验收**：
  - DELIVERED 回执重放不会重复入账；退款中和人工回执不入账；
  - 两个冷静期任务并发时，只迁移一次；
  - 人工出口的两个路径各有真实 TiDB 用例，并写审计记录。

### 3.11 第 9 步：三方常驻对账 + 卡单指标 + 告警

- **周期对账任务**：
  - 每个已发货的挂单订单，恰好对应一个已结算的 kind 9 合同，且该合同的 `buyer_receive` 已 applied；
  - 每个已结算合同，恰好对应一行收益账；
  - 已退款的订单没有 applied 的 `buyer_receive`。第 13 步打开"交付后退款"后，这条要拆成"发货前退款"和"发货后冲回"两类。
  - 每个托管 GUID 在全局恰好一份；
  - 金额守恒：Σ已付（含异常付款）= Σ已退 + Σ退款中 + Σ收益账毛额 + Σ待发货 + Σ人工。
- **卡单指标与告警**：
  - 待发货超过 N 分钟而合同没动；
  - 合同卡在 `rmb_publishing`、`rmb_unpublishing`、`rmb_sold` 或 `rmb_delivering`；
  - 人工订单超过处理时限；
  - 有收益账行却没有 DELIVERED 回执。
- **告警规则**：扩展 `deploy/k8s/asset-channel-alerts.yaml`。该文件 `:9-12` 把 ServiceMonitor/PodMonitor 列为抓取前提，而本仓 `deploy/` 下没有任何 ServiceMonitor 清单，trade 的资产通道默认又是关的，所以规则现在不会触发。第 15 步要一并补上。

### 3.12 第 10 步：robot 端到端 + 故障注入矩阵

- **robot `jubaozhai-rmb-smoke`**（成功标记 `RMB_CONSIGN_SMOKE_OK`）：
  - 账号：`robot_9931` 一区卖家，`robot_9932` 二区买家，`robot_9933` / `robot_9934` 用于抢购和"取消后被别人买下"。这四个号在 robot 和 docs 下都没有被占用。
  - 流程：上架 GUID 装备，标价 ¥12.34 → 公示 5 秒（测试配置）→ 下单 → mock 付款 → 买家背包出现同一个 GUID，实例字节一致，卖家背包里没有 → 卖家收益账 gross=1234、fee=62、net=1172，状态冷静中 → 冷静期 10 秒后变为可提现。
- **故障矩阵**：每项连跑 3 遍，每项都写明预期结局。

| # | 故障 | 预期结局 |
|---|---|---|
| 1 | 两次事务之间 kill trade | 重启后接着推进，物 1 份、钱 1 份 |
| 2 | `buyer_receive` 投递后 kill trade | 只到账一次，最终 DELIVERED |
| 3 | DELIVERED 回执前 kill trade | 回执补发一次，收益账 1 行 |
| 4 | 重复 mock 付款 | 只有 1 张收据、1 个交付意图 |
| 5 | 买家取消后迟到付款 | 这张订单退款；商品回到在售，并被 9934 买下 |
| 6 | 买家离线时付款 | 买家上线后到账一次 |
| 7 | 商城停机时下单 | 客户端收到"服务暂不可用"；合同留在 `rmb_listed`；商城恢复后能正常下单 |
| 8 | 并发抢购 100 轮 | 每轮恰好 1 单进入待付款 |
| 9 | 关单与付款竞态 200 次 | 每次结局恰好是"已付"或"已关单 + 退款意图"之一 |
| 10 | 背包满 | 一直挂在发货中，超过时限告警；腾出空间后自动到账 |
| 11 | 物品被封禁 | 保留托管、订单进人工、卖家那边没有退回 |
| 12 | SQL 证据延迟 | 延迟期间不发 DELIVERED，证据到达后才发 |

- **每项结束后的守恒检查**：按 GUID 查两区存档和托管表，物品恰好 1 份；用 §3.11 的金额守恒式核对各项期望值。
- **测试环境禁止**对有未完成 kind 9 合同的玩家做单人回档。回档闸门目前还不覆盖交易流，第 15 步再补。

### 3.13 第 11 步：客户端人民币寄售界面

- **界面位置**：先在交易中心 `CommerceWindow` 加一个新页签"人民币寄售"，做功能级界面。原生聚宝斋窗口等上架、购买、收益三张设计稿出来再接（J-O5 一直没稿）。
- **页面内容**：
  - 上架：输入元（最多两位小数），换算成分；显示手续费预估和卖家实得。
  - 浏览：¥ 价格、公示期或剩余时间。
  - 购买确认：「需付款 ¥x，15 分钟内完成」。
  - 待付款倒计时：dev 模式下提示运行 mock 付款；每 5 秒刷新一次，保持单个在途请求，忙时不刷新。
  - 背包满待到账、人工处理中：两种状态都要有提示。
  - 我的订单：同一笔交易只显示一条。
  - 卖家收益：冷静中 / 可提现（提现暂未开放）。
- **恢复机制**：客户端"先存原请求再发"的恢复机制要把新方法加进白名单和完整回包判定。**付款参数（链接、签名、令牌）不写进恢复文件**，因为它们有时效，也敏感。
- **验收**：
  - Roslyn 离线编译 0 错误。
  - EditMode 新增用例：元换算成分的边界、回包完整性、忙时不刷新、卖家视图、背包满与人工两种状态的显示；原有用例不回归。
  - 联网 PlayMode 两例并截图目视：客户端当卖家、robot 当买家；客户端当买家、robot 当卖家。
  - 恢复文件里 grep 不到任何付款参数。

### 3.14 里程碑 A：本机 mock 闭环演示

- **冷启动**：清 Redis、新建库、新 robot 账号，按 runbook 一次跑通。
- **冒烟**：`commerce-consign-smoke`、`mall-arrival-smoke`、`jubaozhai-rmb-smoke` 三条冒烟各连跑 3 遍全过；`login-test` 23/23 不回归。
- **现场验收**：你用两个客户端看完整流程：上架 → 下单 → mock 付款 → 到货 → 卖家看到 ¥11.72 冷静中。
- **进度记录**：`docs/PROGRESS.md` 写明三点：这是"本机 mock 闭环，不是真钱，也不是强一致上线"；还有哪些没验证；Java 版的状态。

### 3.15 第 12 步：账号身份与风控

- **会话带账号**：`SessionDetails` 加账号字段，链路是 login → gate C++ 注入 → trade → 商城下单入参。这是跨 C++/Go 的改动，要重新生成并重编 gate。
- **拦截同账号小号互买**；真钱前再加实名维度。
- **限额**：价格上下限、每个账号在售上限、新获得物品的上架冷却、新消息号单独配 MessageLimiter。
- **隐私**：浏览时不暴露卖家的 player_id。原设计 §9 的 P1 就刻意隐藏了卖家。
- **封禁处理**：卖家在售期间被封号；买家付款后被封，交付一直停放。
- **白名单热变更**：物品在售或付款中时被移出白名单，启动时做复查。
- **小游戏包关闭聚宝斋**：微信、抖音小游戏包里关闭聚宝斋。微信小游戏运营规范禁止玩家间定向转移道具（`payment-recharge.md` §12，未跟踪文件，以 10-19 工作区为准）。

### 3.16 第 13 步：接 `go/payment` + 交付后退款的整套前置

- **替换 mock**：加"聚宝斋寄售"订单用途，用 `go/payment` 替换 mock 付款器。
- **渠道关单与查单**：充值稿的 Channel 接口目前没有 Close。真钱阶段挂单订单要先关单或查单，再释放库存（§2.5 第 2 条）。
- **退款执行器**：配合第 4 步加的租约。
- **在打开"交付后退款"和撤销冲回之前，必须先完成以下三项**（D8）：
  1. **GUID 锁**：冷静期内这件 GUID 不能被再寄售或交易。锁要覆盖所有入口：trade 的全部合同种类、摆摊，以及 C++ 的出售、分解、邮寄、强化。
  2. **扣回腿**：按 GUID 从买家身上扣回（流 3），用它自己的截止时间，不能用 `r.ExpiresMs`，否则第一次投递就会变成 Abort（`engine.go:749-755`、`go/shared/assetop/reconcile.go:985-989`）。§2.2 的"只有三种腿"断言改为按阶段放行。
  3. **损失归属**：你拍板由平台垫付还是冲卖家。
- **撤销冲回**：收益账是 PENDING 的扣回；已经 AVAILABLE 的记欠款并转人工。撤销被撤回（REINSTATE）时，收益账从 REVERSED 恢复，同时消掉欠款。
- **渠道选择**：买家自定价、又必须能退款的交易，不适合苹果 IAP。推荐用网页或二维码接微信支付 / 支付宝，由充值会话 R3 来做。iOS 内能否开放、怎么开放，要法务确认。

### 3.17 第 14 步：Java 版

- 需要你给出 Java 仓的位置：本机没有 `D:\luyuan`。快照 PROGRESS 显示，交易和商城是在另一台机器上做的（那边的主仓是 `D:\luyuan\xuanming-server-mmo-cpp`），而且有未提交的改动。
- **节奏**：主仓到里程碑 A 后再移植；在此之前，每一步都在 `PARITY.md` 登记"Java 待做"并写明原因。客户端可见的改动两版同批：`CommerceOrderKind` = 9、新 RPC、tip，以及 Java 版手写的 `TradeTips`。真钱上线（第 16 步）要求两版同时完成。

### 3.18 第 15 步：预发 / 共享环境门禁（b 档）

- **死信工具**：做死信查看与重放工具（L-04 第 2 批，目前没有），并完成演练。
- **告警真正部署**：补上 trade 的 ServiceMonitor；apply `asset-channel-alerts`、`owner-epoch-alerts`、`db-task-retry-alerts`。
- **故障演练**：实跑 `ownership_fault_fullstack`，覆盖旧实例复活后拒写、第二表 SQL ACK 延迟、跨区资产恰好一次。源码已在，但从没运行过。
- **回档闸门**：覆盖交易流和有未完成 kind 9 合同的玩家。另外，PITR 只回退 zone 库、不回退全局交易库，这和人民币订单叠加会有什么后果，要写清楚。
- **网络与部署**：在预发开启 `k8s_deploy.ps1 -NetworkPolicies on`（`scene-grpc-allowlist` 已包含 trade），并实测不在名单里的 Pod 连不上 scene gRPC；`mall_game` 的 K8s 清单，四把密钥走 Secret。
- **GM 验证**：prod 模式下 GM 指令被拒的 robot 用例。
- **帮会门禁**：帮会 08 §8.3 的六条门禁。

### 3.19 第 16 步：真钱上线门禁（c 档）

- **渠道沙箱**：全流程跑通，三方对账差异为 0。
- **实名与防沉迷**：实名、防沉迷、未成年人消费限额（BuyerEligibility）。聚宝斋消费算不算进月累计，由法务定。
- **资质与资金**：平台资质、运营主体、支付签约，以及卖家提现是否涉及资金"二清"，**只能由你和法务来办**。
- **关闭全局强一致边界**：补上玩家归属切换、Redis 回退、冷加载与旧持有者在途写之间的屏障（D-17.2 第 4 条；`jubaozhai-market.md` §15）。
- **记录留存**：交易记录的留存期、不可篡改的审计流水、纠纷证据导出。这些可能有法定要求，待法务确认。
- **Java 版**：同批完成。

---

## 4. 和其他会话的接缝

### 4.1 「道具交易系统代码」会话（同名的另一个会话）

- 它原定做"快照合入 main"，这件事已经作废，D 机器已经合了。
- 改做第 3b 步：官方商品第一次真实 Go↔Java 闭环，包括可复用的 mock 付款器和签名向量。用户已经当面交给它（D6 已定）。代码在隔离工作树 `xuanming-server-mmo-wt-mallarrival`，还没提交，也没编译。
- mock 付款器已经和充值会话对齐（D5 已定）：作为 dev 冒烟的例外，渠道名用 `mock-` 前缀。
- robot 账号：本线用 9931–9934，它用 9925，摆摊用 9921/9922，互不冲突。

### 4.2 「苹果和安卓应用充值」会话

| 接缝 | 约定（待对方确认） |
|---|---|
| `go/payment` 是商城唯一的 payment 签名身份调用方 | 已对齐：robot 的 mock 付款器作为 dev 冒烟例外，渠道名用 `mock-` 前缀；"prod 拒收 mock 渠道"由充值第 3 版的渠道白名单负责（目前商城里还没有） |
| 商城迁移号 | 充值用 V3，本线用 V4/V5，以第 1 步登记为准 |
| 本线会改的共享文件 | 第 4 步改 `DeliveryStore.java`（回执行数检查 `:75-84`、退款租约 `:87-94`）和 `CommerceStore.recordTrustedPayment`；第 2 步改 `mall.go` 的 Tick（`:253-282`）。这几处要在登记表里约定先后和归属。人民币寄售的**新功能**一律走平行的 `/internal/listings/*`（publish、close、poll、receipt），不再往 `DeliveryApi` 里加字段 |
| 到期关单扫描器 | **只做一个**，按订单用途分支。挂单分支是本线第 7 步的硬前置 |
| 撤销与撤回（REVERSED / REINSTATE） | 充值稿在商城加这两个状态；本线第 13 步在同一个事务里加挂单分支，负责冲回或恢复卖家收益账 |
| 订单状态号段 | 商城 `oms_order.status` 和客户端 `CommerceOrderState` 的新值统一分配：充值要加 REVERSED，摆摊要加 `STALL_OPEN=12`；本线目前不需要新值 |
| 每个买家最多 5 张未付单 | 是否按订单用途分开计数，第 1 步定 |

### 4.3 「摆摊系统代码完成度」会话

| 接缝 | 约定（待对方确认） |
|---|---|
| 引擎修复 | 审计 §2.5 第 1 条（Tick 整轮中断）就是摆摊的 P-1b；第 2 条（放弃或人工终结的腿）摆摊稿没覆盖。两条都由本计划第 2 步做，摆摊评审并复用；`engine.go` 同一时间只允许一个会话改 |
| kind 号 | 1–5 和 100 已占用；摆摊用 6、7；人民币寄售用 **9**，避开 8，以防遗留口径。还要和 Java 版核对没被占用 |
| `TradeCommerceRecord` / `Workflow` 字段号 | 按号段划分，在第 1 步登记 |
| `Tip.xlsx`（二进制）和 `message_id.txt` | 由生成器串行发号，两个会话不能同时导表 |
| 收益账边界 | 摆摊的 `trade_stall_income` 是游戏币；聚宝斋收益账是人民币，放在商城。两者不互转 |
| 回档闸门 | 两边共同的前置，归第 15 步 |

### 4.4 共享资源登记表（草案，第 1 步定稿）

| 资源 | 本线申请 |
|---|---|
| 合同 kind，以及客户端枚举 `CommerceOrderKind` 新值 | 9（客户端契约，两版同批改） |
| 合同阶段名及对应的 `trade_commerce.status` | 见 §2.2 的阶段对照表；终态沿用 7 / 10 / 11 |
| 客户端 RPC | `CreateRmbConsignment`、`CancelRmbConsignment`、`BuyRmbConsignment`、`GetMySellerIncome`，消息号由生成器在 316 之后发 |
| `TradeCommerceRecord` / `Workflow` 新字段号 | 按号段划分，第 1 步与摆摊约定 |
| tip | `trade_error` 段（20000 起）新码：公示期内不可买、有人付款中不可撤、价格越界、同账号互买、挂单只能放一件装备等 |
| trade 侧迁移 | `trade_commerce.next_check_ms`（第 2 步）、合同绑定的 `order_sn`（第 7 步） |
| 商城迁移 | V4 加固（第 4 步）；V5 挂单与收益账（第 6、7 步：第 6 步建 `commerce_listing`，第 7 步建 `commerce_order_listing` 和 `commerce_seller_ledger`，必要时拆成 V5/V6） |
| 商城签名身份 | `listing`（第四把密钥） |
| 挂单商品号段 | 与官方 SKU 分开，具体范围第 1 步定 |
| 到期关单扫描器归属 | 第 1 步与充值会话定 |
| 文件归属 | `engine.go` 的加固归本线；`DeliveryStore.java` 加固与充值 V3 的先后，第 1 步定 |
| robot 账号 | 9931–9934（另：`mall-arrival-smoke` 用 9925，摆摊用 9921/9922） |

---

## 5. 需要你拍板的问题

D1–D4 会阻塞开工（D5、D6 已定）；其余不回答就按推荐做。

| # | 问题 | 推荐 | 不选推荐的代价 |
|---|---|---|---|
| **D1**（阻塞） | 主工作区那个卡在 2 个冲突上的 `git pull` 由谁收尾？收尾后由 Codex 跑第 0 步吗？ | 你指定一个会话收尾，其他会话不碰；收尾后 Codex 跑第 0 步 | 本机 main 会一直落后 19 个提交，后面每一步都没有可靠的基线 |
| **D2**（阻塞） | 人民币订单和"唯一买家"放在哪？ | 商城的一次性挂单商品（库存 = 1），复用现有的库存锁，以及付款与取消争同一行锁的代码 | 另开"不占库存的新订单用途"：唯一性只靠 trade 一处裁决，trade 一旦有 bug，商城会收两笔钱 |
| **D3**（阻塞） | 卖家人民币收益账放在哪？是否批准 §2.5 的两处偏离？ | 放商城，和发货回执在同一事务里写；批准两处偏离并补写进 D-17 | 放 trade：违反 `trade_commerce`"绝不保存玩家余额"的约束；而退款、撤销都在商城，冲回要跨库 |
| **D4**（阻塞） | 新开专用 RPC，还是复用游戏币寄售的 281/293/298 再加字段？ | 新开 4 个专用 RPC：通用的撤单、到期路径天然碰不到人民币合同；反正 `GetMySellerIncome` 也要整套接线 | 复用能少发几个号，但通用的 Mutate、plan、浏览要逐处拦住，漏一处就会"一物两卖" |
| ~~D5~~（**已定**） | mock 付款器用 robot 内置的 `newCommerceMockPayer`，作为 dev 冒烟的例外，渠道名用 `mock-` 前缀；「道具交易系统代码」会话已和充值会话对齐 | — | — |
| ~~D6~~（**已定**） | 第 3b 步由「道具交易系统代码」会话做（用户当面交给了它） | — | — |
| D7 | 首批卖什么？ | 只卖装备，一张挂单一件（一个 GUID）。可叠加道具等第二批：混合包会产生"部分到账"。宠物、游戏币、元宝更往后：游戏币和元宝叠加充值，会形成"充值 → 人民币"的套现回路。角色依赖 P0-b/P4，目前零代码 | 首批就放开游戏币：反洗钱风控要提前到第 12 步之前 |
| D8 | 发货后退款或拒付时，物品追不追回？ | 追：冷静期内锁住这件 GUID，退款时按 GUID 扣回，扣不回就转人工。这三项在第 13 步做完之前，不开放"交付后退款"。损失归属（平台垫付或冲卖家）由你定 | 不追回：买家可以"付款 → 拿货 → 申请退款"白拿 |
| D9 | 客户端界面先做在哪里？ | 先做交易中心里的功能级界面；原生聚宝斋窗口等设计稿 | 直接做原生窗口：要先出上架、购买、收益三张设计稿 |
| D10 | 市场范围 | 全球一个市场（`Market.Scope=global`，开资产通道时本来就强制），zone 只作为浏览筛选条件 | — |
| D11 | Java 仓在哪里？节奏怎么安排？ | 请给出路径；主仓到里程碑 A 后再移植 | — |
| D12 | 真钱阶段的付款渠道 | 网页或二维码接微信支付 / 支付宝（由充值会话 R3 做）；iOS 内是否开放待法务确认；小游戏包关闭聚宝斋 | — |
| D13 | 背包满导致货一直到不了，怎么办？ | 本期只挂起、告警，加人工出口，不做自动中止。原因：重投循环一旦过了截止时间就一律改发 Abort，不区分"背包满"和"只是离线"（`reconcile.go:985-989`）；离线买家上线后，收到的第一条会是 Abort，会被错退款。以后要做的话，截止时间只能在 `last_reason=背包满` 时用带锁 CAS 事后设置，并由 Codex 实测 | 直接按 N 小时设截止时间：离线买家会被误退款 |
| D14 | 数值 | 公示 48 小时、寄售 7 天、手续费 5%（向上取整到分）、冷静期 3 天、支付窗口 15 分钟；最低价由费率推导，保证实得 ≥ 1 分（5% 时是 2 分）。全部可配，测试环境调成秒级 | — |

---

## 6. 不在本计划内

- **卖家提现与分账**：要先由法务定方式。本计划只做到收益账"可提现"状态和只读导出。
- **真实支付渠道本身**（微信、支付宝、苹果、谷歌）：归充值会话。
- **角色交易**：P0-b 账号目录落库、P4 过户。
- **人民币竞拍、人民币材料订单簿**：继续用游戏币。
- **摆摊功能本身**：归摆摊会话。
- **完整移植 mall-admin 后台**：只做第 8 步的两个人工出口和审计。
- **生产 K8s 部署与容量结论**：没有 2000 万在线的证据。

## 7. 风险

1. **证据都在另一台机器上。** 交易和商城的"运行证据"只有文档记载，本机没有 `build/` 日志。真实 TiDB 用例的资产终局是夹具注入的；只有一次双区实跑走了真实 C++，结算的还是游戏币；商城桥从没和真实的 `mall_game` 联跑过。第 0、3 步完成之前，不能宣称任何一段可用。
2. **合并跳过了测试和构建。** 试合时统计，有 303 个文件是"自动合并成功"的，但语义未必对，只有编译测试才能发现。
3. **主工作区里有一个归属不明的 `git pull` 卡在冲突上。** 8 个会话共用这个工作区，必须由一个人收尾。
4. **delivery 密钥约等于"给卖家记账的钥匙"。** 回执里的 proof 只校验格式。挂单回执必须走独立的 listing 端点和密钥，并绑定冻结的 `commerce_id`；官方端点也要拒绝挂单订单。预发前还要评估是否再拆一把结算密钥。
5. **回档会造成复制。** 回档闸门不检查交易流：卖家成交后，如果被回档到上架之前，就是一物两得。第 15 步之前，运维必须禁止对有未完成交易的玩家回档。
6. **规模。** `Due` 每秒按 `updated_ms` 取 100 行（查询在 `commerce_repo.go:70-72`，调用在 `engine.go:769`，每秒一次的 ticker 在 `assetchannel.go:418`）。在售合同也在这个队列里空转，每轮还要为每条预取 8 个 OpID 并重写整行（`engine.go:660-666`）。第 2 步的 `next_check_ms` 只能缓解，在售量大时仍需要压测数据。
7. **合规（以下不构成法律意见）。** 玩家之间人民币代收代付可能涉及支付牌照或"二清"；iOS 上卖家自定价的交易可能与 IAP 规则冲突；交易记录可能有法定留存要求。这些工程侧都回答不了。
8. **本机资源。** 可用内存多次记录只有 0.9–1.7 GiB，真实双区故障演练（第 10、15 步）可能排不上。
9. **三个会话改同一批文件。** 不按登记表串行合入，就会撞号，或者冒出两套订单。

## 8. 依据

- 决策：`docs/design/xuanming-port-decisions-20260910.md` D-17（第 731 行起）。
- 聚宝斋原设计与 §15 续批：`docs/design/jubaozhai-market.md`。
- 充值设计（第 2 版，未跟踪，以 10-19 工作区为准）：`docs/design/payment-recharge.md`。
- 摆摊设计（未跟踪，以 10-19 工作区为准）：`docs/design/stall-system.md`。
- 存盘围栏与上线状态：`docs/design/db-write-fencing-mysql.md`。
- 审计：`docs/design/data-overwrite-rollback-audit-20261018.md` §2.5。
- 代码：
  - `go/trade/internal/commerce/{engine,mall}.go`
  - `go/trade/internal/data/{asset_op_repo,commerce_repo}.go`
  - `go/trade/internal/logic/commerce_logic.go`
  - `go/shared/assetop/reconcile.go`
  - `java/mall_game/src/main/java/com/game/mall/{CommerceStore,DeliveryStore,HmacAuthentication}.java`
  - `cpp/libs/services/scene/player/system/asset_op_system.cpp`
  - `proto/trade/commerce.proto`、`proto/common/base/session.proto`
