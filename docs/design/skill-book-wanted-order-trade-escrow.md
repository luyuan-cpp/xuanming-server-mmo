# 悬赏求书迁 go/trade 托管合同与未学书架上限(数据覆写审计 r2 第 4 轮 craft#1、#2)

> 日期:2026-11-26
> 状态:**§10 书架上限已落码(未编译,待 Gemini 验证);§1~§9 悬赏求书迁 trade:负责人 2026-11-26 晚批准分三批(A→C→B)做,2026-11-27 三批代码全部写完——批 A(trade 侧合同,§11.1,已独立复核)、批 C(K8s 部署开通,§11.2)、批 B(scene 退役旧揭榜 + 客户端切 trade,§11.2「批 B 落码记录」)。全部未编译、未运行,待 Gemini 按 §11.1 与 §11.2 的命令验证**。落地时与本文 §3 不同的几处以 §11 为准。
> 上位:[装备品相与技能制书 V2–V5 策划](../planning/equipment-quality-and-skill-crafting-v2-v5.md) §2.4 / §4;[聚宝斋与托管合同](jubaozhai-market.md) §6、§15;[通用资产通道](guild-phase2/04-asset-channel.md) §S4;审计 [data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md) §3.12。
> 负责人 2026-11-26 定:悬赏求书改走 go/trade 托管合同,发榜时托管悬赏银两,揭榜时托管技能书,两条发放腿由 trade 交付;沿用现有托管合同的写法,不另造一套;顺带解决悬赏榜只看得到同场景节点的玩家。

## 0. 结论

1. **问题成立(r2-craft#1,medium)**:`PlayerCraftSystem::FulfillWantedOrder` 在同一帧里改两名玩家——删卖家的书(背包封卷或书架)、给卖家加 95% 净银两、删发榜人的悬赏单、往发榜人书架加书——然后对两人各调一次 `RequestKeyProgressSave`。两笔是各自独立的 player_store 提交,先后与成败互不相干;只落一边就多出一份银两或一本书(§2)。没有小改法:两名玩家各存各的盘,原子交换只能靠托管方。
2. **做法**:在 trade 的合同引擎里加第 7 种合同「求购单」(`COMMERCE_ORDER_KIND_SKILL_BOOK_WANTED = 7`),写法照寄售(kind 3)反过来:**买家(发榜人)建单时先托管银两 → 卖家揭榜时托管一枚封卷道籍(3414)→ 先把书交给买家、到账后再把税后 95% 交给卖家,5% 不发给任何人即销毁**。两名玩家的资产都只经通用资产通道由各自所在 scene 在本人存档里改,合同推进、托管与交付的持久状态只在 trade 库,一条合同一个状态机,不再有「两笔存盘」。
3. **顺带**:悬赏榜改由 trade 库查询,全服(与寄售浏览同一口径)可见,不再只看得到同场景节点、且在线可改的玩家。
4. **代价**:悬赏求书从此依赖 trade 资产通道。trade 的 `AssetOp.Enabled` 现在**所有档位都是 false**(`go/trade/etc/trade.yaml`,K8s ConfigMap 由 `tools/scripts/k8s_deploy.ps1` 固定写 false),所以要么与开通 trade 资产通道同批上线,要么接受上线这一批之后悬赏求书暂不可用(§7、§8 批 C)。
5. **现在就做了的**:书架上限(r2-craft#2,§10)。悬赏求书的旧路径在迁移落地之前照旧运行,残余风险见 §7.1。

## 1. 需求分析

| 需求 | 来源 | 本设计的回答 |
|---|---|---|
| 揭榜不能只落一边(书和银两要么都换手,要么都不动) | r2-craft#1,AGENTS §7 第 7 条 | 两边资产都进 trade 托管,交付由同一条合同的腿完成(§3) |
| 发榜只扣非绑定银两,揭榜只收未绑定常规书、可要求宗师署名、禁止自揭、5% 税销毁 | 策划 V5 §2.4 与 §1.2 红线 | 发榜扣流通银两(币种 0);揭榜按 scene 交出的完整实例快照核对(§3.4);税在交付时少发 5%(§3.6) |
| 同时至多 3 条生效悬赏单、悬赏额 100..50000、随时撤单全额退 | 策划 V5 §2.4 | 建单事务内按发榜人计数(§3.2);撤单走合同的取消路径,托管原样退回(§3.7) |
| 悬赏榜看得到全服的单 | 负责人 11-26「顺带解决」 | 列表查 trade 库(§3.8) |
| 沿用现有托管合同写法,不另造一套 | 负责人 11-26 | 复用 `Engine.Create` / `Mutate` / `plan` / `advance`、腿与资产通道、先买家后卖家的交付闸与整单退回(§3.1) |
| 不改 scene 的资产通道 | 本设计选择 | 书以现成的 3414 封卷道籍实例走 `item_uuids` 扣出、`item_instances` 入账,通道零改动(§3.3) |

## 2. 现状核实(按 HEAD `3eb57a09a` 读的代码)

- 揭榜:`cpp/libs/services/scene/player/system/player_craft.cpp` `FulfillWantedOrder`。卖家侧删书 + `AddCurrency(netGold)` + 卖家账本 `Swap`,发榜人侧 `RemoveWantedOrderAt` + `add_skill_books` + 修订号 +1,最后 `RequestKeyProgressSave(poster)` 与 `RequestKeyProgressSave(player)` 各一次。`RequestKeyProgressSave` 按玩家各挂一个 1 秒节流定时器,`SavePlayerRequest` 一次只带一名玩家(审计原文的反驳尝试已核)。
- 发榜:`PostWantedOrder` 在发榜人自己的存档里扣银两、写 `wanted_orders`(同一笔存盘,原子);撤单 `CancelWantedOrder` 同理。所以单人操作没问题,只有揭榜跨两人。
- 悬赏榜:`ListWantedOrders` 遍历本节点 `tlsEcs.playerList` 里**此刻可改**的玩家,别的节点与离线玩家的单看不到。揭榜也要求发榜人在本节点可改(`IsPlayerMutable(poster)`)。
- 封卷道籍:`SealSkillBook` 免费把书架里未绑定常规书变成背包里一枚 3414 不可叠加道具,`ItemComp.sealed_skill_book` 带 `skill_table_id / scroll_id / rarity_tier / crafter_name / crafter_player_id`;`UnsealSkillBook` 反向,也免费。
- 资产通道:scene `asset_op_system.cpp` 的 TRADE_DEBIT 支持按 `item_uuids` 全或无扣出实例(拒绑定、超 8 件、实例超长),回包 `AssetSnapshot.items[].instance_proto` 是完整 `ItemComp`;TRADE_CREDIT 支持 `item_instances` 原 guid 入账。签名 canonical 已覆盖 `u=`(`go/shared/assetop/auth.go`,J-A4 已收口)。交易流允许的流水类型:扣出 `TX_AUCTION_SELL`,入账 `TX_AUCTION_BUY` / `TX_AUCTION_RETURN`。
- trade 合同引擎:`go/trade/internal/commerce/engine.go`。寄售(kind 3)= 卖家先托管(`seller_debit`),买家下单托管货款(`buyer_debit_<n>_<key>`,stage `pay`),交付 `buyer_receive`(货给买家)与 `seller_receive`(钱给卖家);`delivery_order.go` 的闸让卖家所得等买家那条到账才发,买家那条被拒时整单退回。
- trade 资产通道开关:`go/trade/etc/trade.yaml` `AssetOp.Enabled: false`,`Commerce.ItemConfigs: []`、`CurrencyTypes: []`、`AllowInstances: false`;K8s ConfigMap 所有档位固定 `Enabled=false`(trade.yaml 第 156 行注释)。trade 不读任何游戏配表(`shared/generated/table` 只有 guild / login / mail / match / rank / scene_manager 在用)。
- Java 版(`luyuan-java/xuanming-server-mmo`)按源码核:`game-scene` 没有悬赏求书实现(`git grep SkillBookWanted -- '*.java'` 零命中,只有 `game-proto` 里同步来的 proto)。

## 3. 设计

### 3.1 合同形状:求购单 = 反过来的寄售

| 项 | 寄售(kind 3,现有) | 求购单(kind 7,新) |
|---|---|---|
| 建单人 | 卖家,`SellerPlayerId` | 买家(发榜人),`BuyerPlayerId`;建单时 `SellerPlayerId = 0` |
| 先托管 | `seller_debit`:卖家的货 | `buyer_debit`:买家的悬赏银两(币种 0,流通银两) |
| 对方动作 | `buy`:买家下单,`buyer_debit_<n>_<key>` 托管货款 | `fulfill`:卖家揭榜,`seller_debit_<n>_<key>` 托管一枚 3414 |
| 交付 | `buyer_receive`(货)先,`seller_receive`(全额货款)后 | `buyer_receive`(书)先,`seller_receive`(税后 95%)后 |
| 买家那条被拒 | 整单退回:货回卖家、钱回买家 | 整单退回:书回卖家、**全额**悬赏回买家(§3.6) |

- 角色名、腿名沿用现有的 `buyer_* / seller_*`,语义正好对上(发榜人就是买书的人),于是 `delivery_order.go` 的先买家后卖家闸、`leg_tx_type.go` 的退回判定(`seller_return` / `buyer_return` 带 `TX_AUCTION_RETURN`)、人工处置的按腿通用分支都直接适用。
- `SellerPlayerId` 只在一次揭榜进行中非 0(同寄售下单时写 `BuyerPlayerId`、下单失败清零的写法);`advance` 已有「参与者变了就按新行重来」的保护(`staleObservation`),角色锁与 seq 行都按当时的参与者取。
- 新增 workflow 字段(`proto/trade/trade_table.proto` 的 `TradeCommerceWorkflow` 末尾追加一个子消息,字段号新占):

```proto
// 求购单条款(kind 7)。建单时写定,之后不改;揭榜的核对、列表展示与交付金额都只读它。
message TradeWantedSkillBookTerms {
  uint32 skill_table_id = 1;           // 求购的常规永久技能
  uint32 scroll_id = 2;                // 所属卷宗(展示与筛选)
  bool require_master_signature = 3;   // 只收带宗师署名的封卷
  uint64 reward_gold = 4;              // 托管的悬赏银两(100..50000)
  uint64 seller_net_gold = 5;          // 税后交给卖家的银两 = reward - ceil(reward*500/10000),建单时算定
  string poster_name = 6;              // 发榜时的角色名快照(展示)
}
```

- 金额公式与 C++ `equipcraft::ProxyTaxGold / ProxyNetGold` 是同一条业务规则:Go 侧在 trade 里写一份常量(税率 500 bps、悬赏额区间、3 单上限)并配跨语言用例(照 `go/mail/internal/data/battle_sender_crosslang_test.go` 的写法读 C++ 头文件里的常量比对),防止两边漂移。

### 3.2 发榜(`CreateSkillBookWantedOrder`)

1. 入参:`client_request_id`(trade 惯例的字符串请求号,8..80)、`skill_table_id`、`require_master_signature`、`reward_gold`。
2. 无状态校验:悬赏额在 `[100, 50000]`;`skill_table_id` 是常规永久技能——**trade 启动时读 `Skill` 与 `SkillScrollCraft` 两张表**(写法同 `go/match/internal/battlescript/tables.go`;读不到拒绝启动),规则与 C++ `PostWantedOrder` 逐条一致:`season_id == 0 && battle_model == 1 && rarity_tier == 0`,所属卷宗 `enabled != 0 && rarity_tier == 0`。
3. `Engine.Create` 事务里(角色锁已拿到):按 `buyer_player_id` 计本人 kind 7 且未结案(非 Cancelled / Settled)的合同数,`>= 3` 拒绝(新 tip `kTradeWantedOrderLimitReached`);计数与插入在同一把角色锁下,并发发榜不会超。
4. 合同:`Kind=7, Status=Escrowing, BuyerPlayerId=me, UnitPrice=reward_gold, CurrencyType=0, ItemConfigId=3414, ExpiresMs=0`(不过期,同现行「随时可撤」;托管腿没有截止,与材料订单同,撤单走 `CancelRequested` 截止),`Stage = "wanted_escrow"`,`TradeWantedSkillBookTerms` 写定。
5. `plan`:`wanted_escrow` 建 `buyer_debit`(TRADE_DEBIT,`{currencies:[{type:0, amount:reward}]}`);APPLIED → `wanted_open`;REJECTED(银两不足等)→ `done / Cancelled`。
6. 防洗白:只扣币种 0(流通银两),绑定银两不在这条腿里;scene 的通道按币种精确扣(`ApplyCurrencyDebit`),不会混扣绑定银两。

### 3.3 揭榜(`FulfillSkillBookWantedOrder`)

1. 入参:`order_id`、`client_request_id`、`sealed_item_id`(背包里那枚 3414 的 guid)。**书架里的书由客户端先调 scene 现有的 `SealSkillBook`(免费)再揭榜**,界面上仍是一键:封卷成功拿到 `item_id` 立刻揭榜;揭榜失败书留在背包里,可再开卷,不丢。
2. `Mutate(..., "fulfill")`:锁内要求 `Kind == 7 && Stage == "wanted_open" && SellerPlayerId == 0 && !CancelRequested`,`me != BuyerPlayerId`(禁自揭;同账号不同角色按寄售口径不拦,见 §7)。写 `SellerPlayerId = me`、`SellerIdentity`,建 `seller_debit_<n>_<key>`(`{item_uuids:[sealed_item_id]}`),`Stage = "wanted_fill"`。同一张单同时只允许一次揭榜在途;别人此时揭回 `kTradeOrderStateConflict`(界面显示「交易中」)。同一卖家同一请求号重放回同一合同(请求号做腿名后缀,同 `buy`)。
3. `plan`(`wanted_fill`):
   - 扣出被拒(书已不在、已绑定、被战斗预留)→ 这次揭榜作废:`SellerPlayerId` 清零,回 `wanted_open`。
   - 扣出 APPLIED → **按 scene 交回的完整实例快照核对**(§3.4)。不符 → 建 `seller_return_<n>`(原样退书,`TX_AUCTION_RETURN`),等它有结局之后清零 `SellerPlayerId`、回 `wanted_open`(退书期间单子仍是「交易中」,保证退书腿的角色锁与 seq 行按当时的卖家取)。相符 → 进入交付(§3.5)。
4. 为什么不在揭榜请求时就核对书:trade 在扣出之前看不到实例内容,而 scene 在扣出时才交出权威快照;客户端声称的内容不可信。核对放在扣出之后、交付之前,与寄售按 `Policy.Snapshot` 判扣出快照同一个位置。

### 3.4 快照核对(纯函数,单测钉住)

`AssetItemInstance.instance_proto` 解成 `ItemComp`,全部满足才算相符:`config_id == 3414`、`size == 1`、`!bound`、有 `sealed_skill_book`、`sealed_skill_book.skill_table_id == terms.skill_table_id`、`sealed_skill_book.rarity_tier == 0`;`require_master_signature` 时还要 `crafter_name` 非空且 `crafter_player_id != 0`。与 C++ 现行 `FulfillWantedOrder` 的判据逐条一致(稀世书本来就封不了卷、绑定书封不了卷,这里再判一遍是纵深防御)。

### 3.5 交付与税

- 相符后同一步建两条发放腿:`buyer_receive`(TRADE_CREDIT,`{item_instances:[快照]}` 给 `BuyerPlayerId`)与 `seller_receive`(TRADE_CREDIT,`{currencies:[{0, seller_net_gold}]}` 给 `SellerPlayerId`)。`seller_receive` 由现有闸压着,`buyer_receive` 到账之后才发(货银对付:书到了才付钱)。
- 两条都到账 → `done / Settled`。`reward_gold - seller_net_gold` 留在合同里不发给任何人,即 5% 税销毁;合同结案后托管账目里这部分记为销毁(列表与人工工具按 `terms` 能算出来)。
- 买家包满:scene 回 RETRY,通道照常重投(与寄售同口径),卖家所得一直压着;买家在线久不到账时既有告警 `AssetOpOnlineRetryStuck`(1 小时)会响。客户端提示「背包满,腾出一格后到账」。
- 买家被获取封禁(`REJECTED kAssetBlocked`)→ 整单退回:`seller_return` 把书原样退给卖家,`buyer_return` 退**全额**悬赏给买家(§3.6),合同按撤单结案。

### 3.6 整单退回要退全额(对现有闸的唯一改动)

`settleBuyerFirstDelivery` 整单退回时,`buyer_return` 用的是 `seller_receive` 的资产包——寄售里那就是买家付的全额货款。求购单的 `seller_receive` 只有税后 95%,照抄会让买家白丢 5%。所以 kind 7 的 `buyer_return` 改用 `buyer_debit` 托管原件(`Transfer(buyer_debit)`,全额);其余逻辑不动,`delivery_order_test.go` 补一条 kind 7 用例钉住「退回 = 托管原件」。

### 3.7 撤单

- `CancelCommerceOrder`(现有入口)对 kind 7:只有 `BuyerPlayerId` 能撤;`wanted_escrow` 与 `wanted_open` 可撤(`CancelRequested = true`,`ExpireCommerceEscrowDebitsTx` 截止在途扣款,已扣到的按 `buyer_return` 原样退回);`wanted_fill` 与交付中不可撤(回 `kTradeOrderStateConflict`,界面提示「有人正在交书,稍后再试」),揭榜那一步有了结局(退书回 open 或成交)之后再撤。
- 退回腿带 `TX_AUCTION_RETURN`,不判获取封禁(r2-trade#1 的口径),被封禁的发榜人照样拿回自己的悬赏。

### 3.8 悬赏榜

- `ListSkillBookWantedOrders(filter_skill_table_id, only_mine, cursor, limit)`:查 `trade_commerce` 里 `kind=7 AND status=Waiting`(stage `wanted_open`),按悬赏额降序、建单时刻升序;`only_mine` 走 `buyer_player_id` 索引。走 `kind,market_id,item_config_id,side,status,on_book,unit_price,commerce_id` 现成索引(`item_config_id = 3414`、`unit_price = reward_gold`),不加表结构。
- 可见范围与寄售浏览(`BrowseCommerceOffers`)同口径:全服。`only_mine` 时连「交易中 / 交付中」一起列,给发榜人看进度。
- 技能筛选在页内按 `terms.skill_table_id` 过滤(悬赏单总数受每人 3 单约束,单页有上界);以后量大再给 workflow 投影一列,不在本批。

### 3.9 开关与配置

- 新增 `Commerce.WantedSkillBook.Enabled`(缺省 false)。打开它只放开 kind 7,不会因为开了资产通道就放开别的合同种类;kind 7 不走 `Policy.Selection / Policy.Snapshot` 的通用白名单,用 §3.4 的专用判据,所以不需要把 3414 加进 `Commerce.ItemConfigs`、也不需要 `AllowInstances`。
- 前提:`AssetOp.Enabled = true`(trade 资产通道),以及 trade 能读到 `Skill` / `SkillScrollCraft` 两张表。任一不满足而开关为 true 时拒绝启动(fail-closed)。

### 3.10 scene 与客户端

- scene 退役 `PostSkillBookWantedOrder / CancelSkillBookWantedOrder / ListSkillBookWantedOrders / FulfillSkillBookWantedOrder`(消息 560~563)及其账本类型(`kPostWantedOrder = 26`、`kCancelWantedOrder = 27`、`kFulfillWantedOrder = 28`),`PlayerCraftComp.wanted_orders = 16 / next_wanted_order_seq = 17` 与 `SkillCraftingStateSnapshot.my_wanted_orders` 写 `reserved`。项目未上线,上线前清档,不读旧格式;本机开发库里残留的旧悬赏单(银两已扣)需要清档或在旧版本上先撤单。
- 客户端:`SkillCraftClient` 直接调 trade 生成的 `ClientPlayerJubaozhaiRpc`(悬赏写操作留在它自己的待确认写槽里,不经 `CommerceClient`,§11.2 批 B「为什么这样选」第 2 条);书架书揭榜 = 先 `SealSkillBook` 再揭榜;发榜 / 揭榜之后按同一请求号重发原请求查结论(托管中 → 挂榜中 / 交易中 → 已成交或退回);「我的悬赏」读 `only_mine` 列表。
- 两版共享的客户端契约变了(scene 四条 RPC 退役、trade 加三条),按 AGENTS §12 同批改客户端,Java 版只登记待做(它本来没有这个玩法)。

## 4. 思想分析

- **跨两人的原子交换,权威只能在第三方**。两名玩家的存档分属不同的提交,任何「在 scene 里同时改两人」的写法都要两笔都落定,中间崩溃或一边被拒就分叉。托管方先把两边的资产各自扣进自己的账,再按一条状态机发放,每一步都是单方提交 + 幂等重投:这是 trade 现有寄售、拍卖、撮合已经在用的标准做法(escrow + outbox + 幂等 seq),不是新机制。
- **把差异收进合同,不收进通道**。书走现成的 3414 实例,scene 的资产通道一行不改;差异(条款、快照核对、税、退全额)都在 trade 的 kind 7 里,有纯函数单测可钉。
- **先货后款**。沿用 2026-11-12 定的「先买家、后卖家」闸:书到买家手里之后才付卖家,中途任何一步被拒都能原样退回,不必转人工。
- **核对放在扣出之后**。trade 看不到玩家身上的实例,扣出时 scene 交出的完整快照才是权威;客户端传的 guid 只用来点名。

## 5. 漏洞分析

| 风险 | 后果 | 防线 |
|---|---|---|
| 揭榜只落一边 | 多一份书或银两 | 两边资产都在 trade 托管,交付腿幂等重投;合同状态在 trade 库单行推进 |
| 交一本不符合条件的书(错技能、绑定、无署名、稀世) | 骗走悬赏 | 扣出之后按 scene 快照核对(§3.4),不符原样退书、单子回到挂榜 |
| 反复交不符的书占住单子(骚扰) | 别人一时揭不了 | 每次只占到退书腿有结局(秒级);客户端请求限流(`MessageLimiter`);量大再加「同一卖家同一单冷却」,本批不做 |
| 用绑定银两发榜再撤单洗白 | 绑定银两变流通 | 托管腿只扣币种 0;撤单退回同币种 |
| 发榜后改名、过户、删角 | 资产归错人 | 合同记 `BuyerIdentity / SellerIdentity`(账号 + owner_revision),推进时 `checkContractIdentities` 核对,不符按现有规则处理 |
| 买家被获取封禁 | 书交不过去,卖家所得悬空 | 先货后款闸 + 整单退回(书回卖家、全额回买家) |
| 撤单与揭榜并发 | 退了悬赏又交了书 | 都在合同行锁内判 stage;揭榜在途不可撤 |
| 3 单上限被并发绕过 | 超挂 | 计数与插入同在角色锁内 |
| 退回只退 95% | 买家白丢 5% | kind 7 的 `buyer_return` 用托管原件(§3.6),单测钉住 |
| 两版金额公式漂移 | 卖家实得与界面不符 | Go 常量与 C++ 头文件跨语言比对用例 |

## 6. 被否决的做法

- **scene 内「卖家占用书 + 待寄条目」、发榜人单点提交后凭已提交证据消耗占用**(审计原文提的第二种)。能做到不丢不重,但要在 scene 新造一套跨玩家两阶段协议(占用、证据、超时释放、换节点续做),等于再写一个小号托管方;负责人已定走 trade,且 trade 已有完整的托管与人工处置。
- **scene 当门面、客户端协议不变**(客户端仍调 560~563,scene 内部转调 trade)。好处是客户端少改、scene 有配表可校验;坏处是多一跳 scene→trade 的调用(scene 现在直连的 Go 服务里没有 trade,要新接发现、签名与异步回包),且与寄售、拍卖「客户端直连 trade」的写法不一致。不采用。
- **在资产通道里加「书架书」资产种类**,直接扣书架、入书架。要改通道的包形状、签名 canonical、两边 golden、scene 三处校验,牵动所有调用方;3414 封卷道籍已经能表达同一件东西,不值得。
- **trade 不读配表,发榜不验技能,只在成交时按快照验**。省掉 trade 读表的接线,但输入在信任边界上不校验(求一个不存在或稀世的技能也能挂上、永远没人能交),违背 fail-closed;不采用。
- **悬赏单加过期自动退**。现行口径是「随时可撤、不过期」,也不强迫玩家按时上线处理(反「上班」红线);不加。

## 7. 代价与边界

- **依赖 trade 资产通道**:迁移之后悬赏求书只在 `AssetOp.Enabled = true` 且 `Commerce.WantedSkillBook.Enabled = true` 的环境可用。K8s 现在所有档位都关着 trade 资产通道,打开它要改 `tools/scripts/k8s_deploy.ps1`(本批进行时是别的会话的禁区),也会让寄售、拍卖、撮合具备开通条件(它们各自还有白名单闸),这是负责人要拍板的上线项。
- **体验变化**(要同步策划稿):揭榜成交后发榜人收到的是背包里一枚封卷道籍(可一键开卷入书架),不再直接进书架;发榜、揭榜都有「托管中 / 交易中」的过渡态,通常一两秒;买家包满时书等腾出一格才到账,卖家银两随后到账。
- **同账号不同角色可以互揭**:与寄售口径一致(寄售只拦同一角色)。悬赏银两本来就是流通银两,互揭不产生洗白;要拦同账号得在 trade 加账号维度判定,本批不做。
- **列表按页内过滤技能**:单页有上界,极端情况下一页里筛完可能不足一页,客户端按游标继续取。
- **旧路径在迁移落地前照旧运行**(§7.1)。

### 7.1 迁移落地之前的残余

旧的 scene 揭榜仍是两笔存盘,只落一边的前提是两笔之间进程崩溃、某个存档库在这段时间不可用,或其中一方的存盘被确定性拒绝。§10 的书架上限去掉了「把存档顶到行上限、让发榜人那笔被确定性拒绝」这条人为构造的路子。项目未上线,不另加临时补丁。

## 8. 分批计划(预计约 45 个文件,先拿负责人同意)

文件清单按当时 HEAD 估,落码时以 grep 为准;生成物(`*.pb.*`、`go/shared/generated/clientrpc`、robot 生成的 handler、客户端 `Proto/Generated` 与 `Net/Generated`)不手改,不计入。

**批 A:trade 侧合同(只加不删,对客户端不可见,约 18 个文件)**

| 文件 | 改动 |
|---|---|
| `proto/trade/commerce.proto` | `COMMERCE_ORDER_KIND_SKILL_BOOK_WANTED = 7`;三条请求与 `SkillBookWantedOrderView` |
| `proto/trade/jubaozhai.proto` | `ClientPlayerJubaozhai` 加 `CreateSkillBookWantedOrder` / `FulfillSkillBookWantedOrder` / `ListSkillBookWantedOrders` |
| `proto/trade/trade_table.proto` | `TradeCommerceWorkflow` 追加 `TradeWantedSkillBookTerms`(新字段号) |
| `go/trade/internal/commerce/wanted_skill_book.go`(新) | 建单、揭榜、`plan` 的三个 stage、快照核对、金额常量 |
| `go/trade/internal/commerce/wanted_skill_book_test.go`(新) | 纯函数:状态机、快照核对、税、退回全额 |
| `go/trade/internal/commerce/wanted_skill_book_crosslang_test.go`(新) | 与 C++ `equip_craft_quality_rules.h` 常量比对 |
| `go/trade/internal/commerce/engine.go` | `Mutate` 加 `fulfill` 与 kind 7 撤单分支;`plan` 分派;`Get` 把 kind 7 列为公开 |
| `go/trade/internal/commerce/delivery_order.go` | kind 7 整单退回用托管原件(§3.6) |
| `go/trade/internal/commerce/delivery_order_test.go` | kind 7 退回用例 |
| `go/trade/internal/commerce/engine_tidb_test.go` | 真库:发榜→揭榜→成交、不符退书、撤单、3 单上限并发 |
| `go/trade/internal/logic/commerce_logic.go` | 三条 RPC 的逻辑与视图 |
| `go/trade/internal/logic/jubaozhai_logic_test.go` | 入口校验、视图 |
| `go/trade/internal/server/jubaozhai_server.go` | 三条 handler |
| `go/trade/internal/config/config.go` | `Commerce.WantedSkillBook.Enabled` 与表目录;只加自己的行(该文件常有别的会话的改动) |
| `go/trade/internal/config/config_test.go` | 开关前提的 fail-closed |
| `go/trade/internal/svc/servicecontext.go` | 读 `Skill` / `SkillScrollCraft` 表 |
| `go/trade/etc/trade.yaml` | 新键,缺省 false |
| `data/tip/Tip.xlsx`、`data/MessageLimiter.xlsx` | 新 tip(`kTradeWantedOrderLimitReached`、`kTradeWantedBookMismatch`、`kTradeWantedOrderFeatureDisabled`)与三条客户端消息限流,二进制,交 Gemini 改 |

**批 B:scene 退役旧四条 + 客户端切换(必须同批,约 20 个文件)**

| 文件 | 改动 |
|---|---|
| `proto/scene/player_skill.proto` | 删 560~563 四条 RPC 与消息,`SkillCraftingStateSnapshot.my_wanted_orders` 写 `reserved` |
| `proto/common/component/player_craft_comp.proto` | `wanted_orders = 16`、`next_wanted_order_seq = 17` 写 `reserved`,删 `SkillBookWantedOrder` |
| `proto/common/component/client_request_ledger_comp.proto` | 悬赏相关结果字段写 `reserved` |
| `proto/message_id.txt` | 四条消息号退役(按该文件既有退役写法) |
| `cpp/libs/services/scene/player/system/player_craft.{h,cpp}` | 删四个入口与悬赏辅助函数、快照里的我的悬赏 |
| `cpp/libs/services/scene/player/system/client_request_ledger.{h,cpp}` | 账本类型 26~28 退役(枚举值保留不复用) |
| `cpp/libs/services/scene/player/system/equip_craft_quality_rules.h` | 悬赏常量删(已挪到 trade) |
| `cpp/nodes/scene/handler/rpc/player/player_skill_handler.{h,cpp}` | 删四个 handler 的守护段代码 |
| `cpp/nodes/scene/handler/rpc/player/player_handoff_gate.h` | 删四条消息的交接分类 |
| `cpp/tests/bag_test/player_craft_quality_test.cpp` | 删 V5 悬赏段与 §10 用例里的揭榜段 |
| 混版本字段保护台账(`docs/design/mixed-version-persist-field-protection.md` 的台账表) | 退役字段登记 |
| 客户端 `Assets/Scripts/Game/Skills/SkillCraftClient.cs` | 改调 trade |
| 客户端 `Assets/Scripts/Game/Jubaozhai/CommerceClient.cs` | 落码时定为不改(悬赏写操作留在 `SkillCraftClient`,§11.2 批 B「为什么这样选」第 2 条) |
| 客户端 `Assets/Scripts/UI/Ugui/Skills/SkillCraftModels.cs`、`SkillCraftWindow.cs` | 先封卷再揭榜、过渡态、我的悬赏改读 trade |
| 客户端 `Assets/Scripts/Game/GameClient.cs` | 回包分发 |
| 客户端 `Assets/Tests/EditMode/Skills/SkillCraftClientTests.cs` | 用例改写 |

**批 C:部署开通(约 6 个文件,等 `k8s_deploy.ps1` 解禁并经负责人拍板开 trade 资产通道)**

`tools/scripts/k8s_deploy.ps1`(trade ConfigMap 的 `AssetOp.Enabled`、`Commerce.WantedSkillBook.Enabled`、落地恢复键、表目录挂载)、对应 K8s 契约测试(`tools/scripts/tests/k8s_*_contract.tests.ps1` 里钉 trade ConfigMap 的那几份)、`docs/ops/trade-commerce-quarantine-runbook.md`(kind 7 的人工处置口径)、`deploy/k8s/README.md`。

**文档(随批 A / B 写)**:本文 §11 实施记录、[V2–V5 策划](../planning/equipment-quality-and-skill-crafting-v2-v5.md) §2.4 / §4、[jubaozhai-market.md](jubaozhai-market.md) 加 kind 7 一节、Java 仓 `PARITY.md` 登记待做(主会话)。

**顺序**:A → C → B。A 落地不影响现网;C 打开之后 B 才能切,否则 B 一上线悬赏求书就不可用。也可以 A、B、C 一起上(上线前清档时)。

## 9. 执行与验证步骤(落码批次用;现在未编译)

1. 批 A:`proto-gen-run` → `go build ./...`、`go vet`(go/trade)→ `go test ./internal/commerce/... ./internal/logic/...` → 配 `COMMERCE_TEST_DSN`(隔离 TiDB,`COMMERCE_REQUIRE_TIDB=1`)跑真库用例 → 新 tip 导表后 `go test ./internal/constants/...`(`TestNoHandWrittenTipCodes`)。
2. 批 B:`proto-gen-run` → 串行 MSBuild(`/m:1`)scene 与 `bag_test` → `bag_test.exe --gtest_filter=PlayerCraftQualityTest.*` → `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` → 客户端 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1` 与 EditMode 用例。
3. 批 C:K8s 契约测试 → kind 冒烟:发榜、揭榜(书架书与背包封卷两种)、撤单、买家包满、买家获取封禁时整单退回。

## 10. r2-craft#2:未学书架上限(已落码,未编译)

**问题**:`PlayerCraftComp.skill_books`(未学书架)没有条数上限,制书、开卷、揭榜送书三处都直接 `add_skill_books`。存档每行 1 MiB、每次整行重写,一直攒下去所在行会逼近上限,届时存盘被确定性拒绝;它还给 r2-craft#1 提供了一条人为构造「发榜人那笔存盘必败」的路子。违反负责人 11-20 定的「玩家存档里的集合要有上界」。

**方案**:`equip_craft_quality_rules.h` 加 `kMaxBookshelfBooks = 100`。三处加书之前判:
- `CraftSkillBook`:修订号核对之后、扣料扣银之前,满了回新 tip `kSkillBookshelfFull`;提交段原本就核「修订号未变」,书架在两次检查之间不会变多。
- `UnsealSkillBook`:满了回 `kSkillBookshelfFull`,封卷道籍留在背包。
- `FulfillWantedOrder`:发榜人书架满了回新 tip `kSkillWantedPosterBookshelfFull`,拒绝这一次交付(卖家的书与银两、发榜人的悬赏单都不动),发榜人腾出位置后可再揭。

用例 `PlayerCraftQualityTest.BookshelfCapRejectsCraftAndUnsealWithoutSideEffects` 钉住:制书、开卷满了都拒绝且无副作用(揭榜那一处随批 B 删掉,用例里的揭榜段一并删、用例改名)。

**为什么这样选**:书架是玩家主动管理的收纳(研习、拆页、封卷都能腾位置),满了拒绝新增是背包同类的标准做法,不丢任何东西。100 = 95 项永久技能各一本还有余量;按每本约 60 字节(含宗师署名)算,满架约 6 KB,离 1 MiB 很远。加书前判、不在加载时裁:裁掉就是删玩家的书。

**被否决的做法**:①加载时把超出部分裁掉或转邮件——删玩家资产或多出一条跨系统流程,上线前清档后也不会有超限存档;②按字节数而不是条数限——玩家看不懂「还能放几本」;③复用 `kInvalidParameter`——「参数错误」不是玩家能理解的原因,命名第一原则要求语义准确;④揭榜时发榜人满了把书改投邮件——给一条要退役的路径(§3.10)加新流程不值得。

**代价与边界**:书架满了必须先处理才能继续制书、开卷;两条 tip 要在 `data/tip/Tip.xlsx` 的 `//skill_error base=7000` 段末尾(`SkillLoadoutAttributeResetRequired` 之后、`//buff_error` 组头之前)加两行——`SkillBookshelfFull`「书架已满(最多 100 本),请先研习、拆页或封卷后再试」、`SkillWantedPosterBookshelfFull`「发榜人的书架已满,暂时无法交付这张悬赏单」,`fault` 列留空——再导表(Gemini),导表之前 C++ 不能编译。已超限的旧存档(只可能来自本机开发库)照常加载,只是不能再加书。悬赏求书迁 trade 之后(批 B),揭榜不再往书架加书(发榜人收到的是背包里的封卷道籍),第三处判定随旧路径一起删掉。

## 11. 实施记录

### 11.1 批 A:trade 侧求购单合同(2026-11-27,Opus 5.5;未编译,待 Gemini 验证)

**问题**:同 §0 第 1 条(r2-craft#1)。批 A 只加 trade 侧的合同与客户端入口,scene 旧揭榜照旧运行,客户端还没切(批 B)。

**方案(落地的形状)**:
- 合同在 `go/trade/internal/commerce/wanted_skill_book.go`:三个自有阶段 `wanted_escrow`(状态 3)→ `wanted_open`(状态 1,只有这一个阶段是 1,悬赏榜按它查)→ `wanted_fill`(状态 3);交付 `deliver`(4)与退回 `return`(6)走 `plan` 的通用收尾,结案 `done`(11 成交 / 7 撤单)。`engine.go` 只加了分派(`plan` 里 kind 7 先进 `planWantedSkillBook`)、撤单分支(只有发榜人、只在托管与挂榜阶段)、`Get` 把 kind 7 列为公开、`Create` 拆出带「角色锁内钩子」的 `createLocked`、`Mutate` 拆出带封卷道籍号的 `mutate`。
- 揭榜:`FulfillWantedSkillBookOrder` → `mutate(..., "fulfill", sealedItemID, requestID)` → `fulfillWantedLocked`,建 `seller_debit_<揭榜序号>_<请求键>`;`planWantedFill` 等扣出、按快照核对(`wantedBookMatches`,§3.4 判据逐条,另核实例 `item_id` 与扣出的 guid 一致、实例解不开按不符)。相符建 `buyer_receive`(书,托管者 = 揭榜人)与 `seller_receive`(税后银两,托管者 = 发榜人);不符建 `seller_return_<揭榜序号>`(`leg_tx_type.go` 登记为退回腿,`TX_AUCTION_RETURN`),退书到账才清零揭榜人、回挂榜;退书终局没到账转人工(`final_credit_failed`),不回挂榜。书没扣到直接回挂榜。
- 揭榜的结果按**同一请求号重发揭榜**查:当前这一次在途或已成交回合同;已作废的,书没扣到回 `TradeAssetNotTradable`,书不符已退回回新 tip `TradeWantedBookMismatch`;同一请求号换了书回 `TradeRequestConflict`。
- 整单退回退全额:`delivery_order.go` 的 `settleBuyerFirstDelivery` 对 kind 7 用 `buyer_debit` 托管原件,先算好再标被取代(§3.6)。
- 发榜上限:`countActiveWantedOrders` 在建单事务里、发榜人角色锁之下用 `FOR UPDATE` 数未结案的 kind 7(含转人工)。
- 悬赏榜:`ListOpenWantedSkillBookOrders` 走现成索引 `(kind,market_id,item_config_id,side,status,on_book,unit_price,commerce_id)`,按悬赏额降序、同额按 `commerce_id` 降序,游标是「悬赏额:commerce_id」;`ListMyWantedSkillBookOrders` 按 `(created_ms, commerce_id)` 翻页、`OwnsRecord` 筛。技能筛选页内过滤。
- 调度:`schedule.go` 把 `wanted_open` 算作只等玩家操作,排 `idleRecheck`(1 小时)兜底,不每秒空转。
- 开关:`Commerce.WantedSkillBook.Enabled`(缺省 false)+ 顶层 `TableDir`(缺省 `../../generated/tables`)。开着时 `svc` 用 `tablebundle.LoadAtStartup` 读表(校验钩子要求 `Skill` 与 `SkillScrollCraft` 都在,`StrictStartupValidate`),`Engine.WantedSkills = commerce.TableWantedSkills{}` 查全局表管理器的当前快照;关着时 trade 不碰配表。Validate:开关开着而 `AssetOp.Enabled=false` 或 `TableDir` 为空拒启。关掉开关后发榜、揭榜、公开榜回 `TradeFeatureDisabled`,本人的单照常列、照常可撤。
- 协议:`proto/trade/commerce.proto`(kind 7、`WantedSkillBookOrderPhase`、`WantedSkillBookOrderView` 与三对请求 / 响应)、`jubaozhai.proto`(三条 RPC,message_id 582~584)、`trade_table.proto`(`TradeCommerceWorkflow.wanted_skill_book = 28` 与 `TradeWantedSkillBookTerms`)。这三份与生成物已随负责人快照提交 `24c13a311` 进 HEAD。
- 配表:`tools/scripts/trade_wanted_skill_book_xlsx_patch.py`(幂等,已跑):`Tip.xlsx` `//trade_error` 段末加 `TradeWantedOrderLimitReached`、`TradeWantedBookMismatch`;`MessageLimiter.xlsx` 加 582~584 三行(5 次/秒,同 scene 旧的四条),号按名字从 `message_id.txt` 查。**导表之前 go/trade 编不过**(两条新 tip 常量)。
- 用例:`wanted_skill_book_test.go`(纯函数:托管、相符先书后款、九种不符退书、书没扣到、退书没到账转人工、入口约束与重放、撤单退全额、托管中撤单、阶段映射、调度、游标)、`wanted_skill_book_crosslang_test.go`(5% 税与 3414 对 C++ 头文件逐字比对;悬赏额区间与 3 单在 C++ 删掉之前也比)、`delivery_order_test.go` 的 `TestWantedBuyerRejectedRefundsTheFullReward`、`leg_tx_type_test.go` 三个角色名、`wanted_skill_book_tidb_test.go`(真库:不符退书回挂榜再相符成交、撤单退悬赏与交书途中撤不了、真角色锁下并发发榜不超 3 张)、logic 的 `wanted_skill_book_logic_test.go`(开关、入参在碰存储之前校验、视图),并把揭榜并进 `commerce_missing_contract_test.go` 的「点名合同不存在」表、`config_test.go` 加开关前提。

**为什么这样选(与 §3 不同的几处)**:
1. RPC 与消息名用 `WantedSkillBook*` 打头(设计稿写的是 `SkillBookWanted*`):批 A 到批 B 之间 scene 旧消息 `SkillBookWantedOrder*` 还在,客户端两套同时编译,同名会在 C# 侧撞名或歧义。
2. 揭榜结果靠同一请求号重放查,不另加查询 RPC:与寄售购买的重放同口径,而且只有重放分得清「书没扣到」与「书不符已退回」;单子本身的阶段用 `GetCommerceOrder`。
3. 悬赏榜同额按 `commerce_id`(§3.8 写的是建单时刻升序):现成索引最后一列是 `commerce_id`,按建单时刻排要把全部挂榜单回表排序;`commerce_id` 是打散的,只给稳定翻页,不代表先后。
4. 功能关闭复用 `TradeFeatureDisabled`,不新增 §8 列的 `kTradeWantedOrderFeatureDisabled`:同一个语义,玩家看到的文案也一样。
5. 上限计数用加锁读:TiDB 悲观事务里普通读取的是事务开始时的快照,两笔并发发榜在角色锁上串行之后,后一笔的普通读看不到前一笔刚提交的单。
6. 读表经 tablebundle、只在开关打开时读:与 match / mail 同一套两阶段加载,表包模式下跟随热更,scene 与 trade 对「哪门技能能求」不会因热更时差长期不一致;关着时部署不用给 trade 准备表目录。
7. 揭榜次数先按人限、再按单限(2026-11-27 复查改定,寄售的购买同批改):同一角色对同一张单最多 3 次,整单最多 256 次(`engine.go` 的 `attemptAllowed`)。每次揭榜在 workflow 留一两条腿(约 300 字节),整单 256 次约 80 KB,workflow 有界。原先只按单限 32 次:书没扣到的揭榜对揭榜人零成本,几个号几秒就能把一张单用满,真想交书的人再也进不来;按人限次之后,把一张单用满要 86 个实名角色,256 次也远在合同行与角色冻结核对的 1 MiB 上限之内。
8. 揭榜人的一次揭榜作废之后已不是参与方,所以 `Get` 把 kind 7 列为公开(与寄售、拍品同),揭榜人仍查得到单子回到了挂榜。

**被否决的做法(本批落码时)**:①在 `Mutate` 的 `requestID ...string` 变参里塞封卷道籍号——名字与内容不符(命名第一原则);改成内部 `mutate` 多一个 `sealedItemID` 参数、`Mutate` 包一层,调用点零改动。②把作废揭榜的腿从 workflow 删掉省空间——腿是 outbox 行与合同的唯一挂钩,也是人工处置的证据。③为悬赏榜加 `created_ms` 索引——要改表结构,收益只是同额单的先后。④揭榜时 trade 先问 scene「这本书合不合格」——trade 看不到玩家身上的实例,扣出时交回的快照才是权威(§3.3 第 4 条)。

**代价与边界**:
- 揭榜次数(已修,见「为什么这样选」第 7 条):批 A 初稿只按单限 32 次,可被零成本耗尽;11-27 改成每人 3 次、整单 256 次,寄售购买同一判定、同批改。剩下的边界:有人用 86 个以上实名角色专门耗一张单,单子会揭不了,发榜人撤单(全额退回)重发,不丢东西;同一真人对同一张单失败满 3 次之后只能换一张单。被否决:①只对扣到了的揭榜计数——没扣到的尝试照样留腿,workflow 仍然无界;②把作废的揭榜从 workflow 删掉——重放按请求号查结果要靠那条腿,删了重放会被当成新揭榜再扣一次;③每次揭榜拆成独立子合同(同拍卖出价)——最彻底,但要重做父子合同的锁序、唤醒与跨合同转交,本批不值得。
- 人工整单退回(`manual_plan.go` 的 `refundCommon` / `refundReturns`,后台双人复核会话在改,本批不动)对 kind 7 是 fail-closed 的:有过「不符退书」的单,`refundCommon` 见到已到账的退书腿就拒绝整单退回,不会把已退过的书再退一遍;处置改用 reissue / resume。隔离手册的 kind 7 口径随批 C 写。
- 滚动升级:旧 trade 二进制不认识 kind 7,交付阶段遇到发榜人收书被拒会按旧代码只退 95%。项目未上线,上线前清档,不阻塞;上线之后升级 trade 要整批换,或先确认没有 kind 7 合同停在 `deliver`。
- 同账号不同角色互揭不拦(§7)。揭榜人的书在扣出到核对之间(秒级)单子显示「交易中」。
- HEAD 状态:协议、生成物与 `wanted_skill_book.go` 等已随 `24c13a311` 进 HEAD,其余文件在工作区未提交;导表之前 go/trade 编不过。

**Gemini 验证命令**(仓库根;只跑,红了保留首错原文交回):
1. 导表:先 `git status --short data/` 核对别的会话的表改动,再 `dev.bat gen`;`go/shared/generated/pb/table` 里要有 `TradeError_kTradeWantedOrderLimitReached`、`TradeError_kTradeWantedBookMismatch`。
2. 协议:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`(已有人跑过一次,再跑 trade 部分应无 diff);`proto/message_id.txt` 里 582~584 不变。
3. go/trade:`go build ./...`、`go vet ./...`;`go test ./internal/commerce/... ./internal/logic/... ./internal/config/... ./internal/constants/...`,点名 `TestWanted`、`TestLegTxType`、`TestCommerceEntriesAnswerMissingContractWithOrderNotFound`、`TestCommerceEntriesKeepContractReadFaultsAsFaults`、`TestEtcYamlContractValues`、`TestNoHandWrittenTipCodes`。
4. 真库:配隔离 TiDB `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1`,`go test ./internal/commerce/ -run 'TestTiDBWantedSkillBook|TestTiDBConsignmentConcurrentBuyAndWorkerRestart|TestTiDBPlayerTradeRequiresBothEscrowSummariesAndReturnsOnCancel' -count=1`。
5. 客户端:`mmorpg-client` 跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`(批 A 只新增消息,客户端还不调用)。

通过标准:以上全绿;`git diff` 里 `data/tip/Tip.xlsx` 只多两行、`data/MessageLimiter.xlsx` 只多 582~584 三行。

**复核(2026-11-27,会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」,Opus 5.5,只读代码,未编译):通过,没有要改的。**
- 读的是 HEAD 上的 `wanted_skill_book.go` 全文,`engine.go` 的 `mutate`(揭榜与撤单分支)、`createLocked` / `priorRequest`、`Get`、`attemptAllowed`,`delivery_order.go` 的 `settleBuyerFirstDelivery`,`leg_tx_type.go`、`schedule.go` 的改动,logic 的 `wanted_skill_book_logic.go`,以及 config / svc 的开关前提。
- 状态机:托管 → 挂榜 → 揭榜 → 核对 → 交付,每一步都在合同行锁里按 stage 判。揭榜要求挂榜、`SellerPlayerId == 0`、没在撤单,同一张单同时只有一次揭榜在途;撤单只认发榜人、只在托管与挂榜两个阶段,揭榜人在交书途中撤不了。托管中撤单而扣款已到账时,同一次 plan 里先挂榜、再按撤单退全额。
- 核对与退回:书只在扣出之后按 scene 交回的快照核对,`item_id` 与扣出的 guid 对得上才算;不符的书原样退给本人(托管者是他自己,按退回记 `TX_AUCTION_RETURN`,不过获取封禁),退书没到账转人工后可 `reissue`,`currentLeg` 跟到补发腿。
- 钱:悬赏只扣流通银两;先书后款的闸压住卖家所得;发榜人收书被拒时整单退回用托管原件,退全额;5% 税留在合同里即销毁,税额按向上取整与 C++ 同口径(跨语言用例比对)。
- 幂等与上限:建单请求号在 `priorRequest` 里校验长度并按摘要判重放;揭榜按 (揭榜人, 请求号) 的键重放,同一请求号换了书回 `TradeRequestConflict`;发榜上限的计数是角色锁下的加锁读,揭榜次数每人 3 次、整单 256 次。
- 列表:公开榜只列挂榜中的单,按现成索引倒序扫;本人的单按原身份筛,过户之后新主看不到旧主的单;开关关掉之后公开榜回「功能未开放」,本人的单照常列、照常可撤。
- 剩余与取舍照上面「代价与边界」,不另加。

### 11.2 批 C、批 B 换电脑由 Claude Code 写(2026-11-27 负责人改定)

> 剩余工作(批 A 验证、批 C、批 B、文档收尾)的执行入口是 [skill-book-wanted-order-remaining-handoff-20261127.md](../handoff/skill-book-wanted-order-remaining-handoff-20261127.md),逐文件改法、要先问负责人的事与完成判据都在那里;本节保留设计层面的要点。

Claude 全部模型周额度过 80%(81%),按负责人 11-26 口径停在批 A 边界。原定批 C、B 交 Gemini 4 Neon Extra 写;**负责人 11-27 改定:换一台电脑,由那边的 Claude Code 写**(按 AGENTS §10.3 用最高档模型 Fable 5.2 Max),**Gemini 不写这两批的代码**,只做写完之后 §9 的生成、编译、测试、冒烟。写的人在本节补五项记录;写完再按 §9 跑。顺序照旧 C → B(或上线前清档时三批一起上)。那台电脑要先拿到本机 main 的最新内容(含批 A 与还在工作区的文档改动),由负责人同步。

**批 C(部署开通)**,前提②不满足就停下问负责人:①已满足——第 4 轮会话 §7.4 的子代理没有改 `tools/scripts/k8s_deploy.ps1`(r2-tn#3 的 match 接线已由「改名与内容安全第三轮」会话修完,见第 4 轮交接单 §7.4),改这个文件前照常先 `git diff` 看别的会话的未提交改动;②负责人拍板 K8s 各档位是否打开 trade `AssetOp.Enabled`(本文没有定这个取值,不自己定)。
- `k8s_deploy.ps1` 的 trade ConfigMap:`AssetOp.Enabled` 按拍板写,打开时把 trade.yaml 里落地恢复与 `Cleanup*` 键一并补进去(trade.yaml 第 156~158 行注释);`Commerce.WantedSkillBook.Enabled`;`TableDir`。
- 表:核对 trade 镜像里有没有 `generated/tables`;没有就要么镜像带上、要么像 match / mail 一样挂表包 sidecar,照 `docs/design/config-table-hot-reload.md` 的服务接入口径(STATUS.json、POINTER.json 等多方约定照它对齐);trade 的 main 现在没有 `--validate-tables` 入口(`tablebundle.MaybeRunValidateTablesCLI`),接入表包时按 `go/mail/mail.go` 的写法补。
- 契约测试:`tools/scripts/tests/k8s_*_contract.tests.ps1` 里钉 trade ConfigMap 的几份同批改。
- `docs/ops/trade-commerce-quarantine-runbook.md` 加 kind 7 口径:退书腿没到账用 reissue 补 `seller_return_<n>`;有过退书的单不能整单退回(§11.1 代价第 2 条);发榜人收书被拒由引擎自动整单退回、退全额,不用人工。`deploy/k8s/README.md` 一行。

**批 C 落码记录(2026-11-27,会话「复核第 4 轮批 V/W 并补 guild 见证 SQL」,Opus 5.5;未运行,待 Gemini 验证)**:
- 问题:K8s 上 trade 的求购单开关一直是服务 yaml 的缺省 false,ConfigMap 也没有 `TableDir`;表包模式下 trade 不在挂 sidecar 的读表服务里。批 B 切过去之后,集群里的悬赏求书会整个不可用。
- 前提②的拍板:负责人 2026-11-27 定 **K8s 各档位都打开 trade 资产通道**。部署脚本早有 `-AssetOps on|off`(签名密钥注入、`-NetworkPolicies on` 前提、先 zone 后 infra 的顺序、跨命令核对),所以「打开」就是各档位都以 `-AssetOps on` 部署,不改开关本身的缺省与前提。
- 方案:
  - `tools/scripts/k8s_deploy.ps1`:`-AssetOps on` 时 trade ConfigMap 的 `Commerce.WantedSkillBook.Enabled` 改 true(服务 yaml 里那一行必须恰好一处,不猜);trade ConfigMap 恒写 `TableDir: /generated/tables`(镜像自带的表);`$script:TableSyncGoServices` 在 `-AssetOps on` 时加上 trade,表包模式下与别的读表服务一样挂 sidecar、跟随热更。trade ConfigMap 那段「所有档位固定 false」的过时注释改写成现状。
  - `go/trade/trade.go` 补 `--validate-tables` 入口(`tablebundle.MaybeRunValidateTablesCLI`,校验钩子 `svc.ValidateTables`,与启动、热更同一判据)。
  - 契约测试 `k8s_deploy_contract.tests.ps1`:关着时求购单关、`TableDir` 在;打开时求购单开、`TableDir` 在。
  - 隔离手册 §4.2 补 kind 7 的三条口径;`deploy/k8s/README.md` 的 `-AssetOps on` 一节补一行。
- 为什么这样选:求购单只依赖资产通道,跟同一个开关走,开关两头不会出现「求购单开着、通道关着」这种让 trade 拒启的组合;表沿用镜像自带的 `/generated/tables`,表包模式只是在它之上换成热更的包,和 match / mail 同一套。
- 被否决的做法:①把 `-AssetOps` 的缺省改成 on——它的前提(两把密钥、`-NetworkPolicies on`、滚动整个 zone 的 scene)每个环境都要先准备,改缺省会让还没准备好的环境直接部署失败;②另加一个只管求购单的部署开关——多一个开关就多一种「通道关、求购单开」的错配。
- 代价与边界:没有用 `-AssetOps on` 部署的环境,悬赏求书在批 B 之后不可用(发榜、揭榜回「功能未开放」,已有的单照常可撤);trade 进了表包模式的读表名单,`config-table-hot-reload.md` 的 Go 读表服务一条已补;sidecar 的就绪与 STATUS.json 没有为 trade 单独写用例,交 Gemini 在表包模式的 kind 冒烟里一并核对。

**批 B(scene 退役旧揭榜 + 客户端切换,必须同批)**,文件按 §8 批 B 表,另补:
- scene 四条 RPC(560~563)删掉之后生成器自动把号转墓碑(一次 4 条,正好卡在 `maxNewMessageTombstonesPerRun = 4` 上,同一轮生成里不能再删别的 RPC);`MessageLimiter.xlsx` 里 560~563 四行同批删(先核对导表器允不允许限流行指向墓碑号)。
- 玩家存档字段退役(`PlayerCraftComp.wanted_orders = 16`、`next_wanted_order_seq = 17`、`SkillCraftingStateSnapshot.my_wanted_orders = 23`)走混版本字段保护台账:生成器写 `retired=` 行,`proto/persist_schema_gen.txt` 的 diff 只许多退役行;`ClientRequestKind` 26~28 只退役不复用。
- `equip_craft_quality_rules.h` 删悬赏常量之后,trade 的跨语言用例对这三项自动不比;`kProxyTaxBps` 与 `kSealedSkillBookConfigId` 不能删(代打分成与封卷还在用,用例照比)。`kSkillWantedPosterBookshelfFull` 随旧揭榜不再使用,tip 行保留、不复用。
- 客户端(落码时的取舍见下面「批 B 落码记录」):`SkillCraftClient` 直接调 trade;揭榜书架书 = 先 `SealSkillBook` 拿到道具号再 `FulfillWantedSkillBookOrder`;发榜与揭榜之后都按同一请求号重发原请求查结论(发榜不用 `GetCommerceOrder`:托管悬赏与有人交书都是订单状态 3);「我的悬赏」读 `only_mine`;撤单走 `CancelCommerceOrder`;阶段文案按 `WantedSkillBookOrderPhase`。客户端用服务端给的 `seller_net_gold`,不自己算税。
- 策划稿 `docs/planning/equipment-quality-and-skill-crafting-v2-v5.md` §2.4 / §4 同步体验变化(§7 第 2 条:成交后发榜人收到背包里一枚封卷道籍;有托管中 / 交易中过渡态;悬赏榜全服可见)。
- Java 仓 `PARITY.md` 已登记待做(2026-11-27 一节),批 B 落地时补一句「客户端已切」。

**批 B 落码记录(2026-11-27,Opus 5.5;未编译、未运行,待 Gemini 验证)**:

- 问题:批 A 加了 trade 的求购单,但 scene 的旧揭榜(同一帧改两名玩家、各自存盘)还在运行,客户端也还调 scene 的四条;r2-craft#1 只有旧路径整条退役、客户端切到 trade 之后才真正消失。两边必须同批落,否则客户端会调不存在的接口。
- 方案(服务端):
  - 协议:`proto/scene/player_skill.proto` 删 560~563 四条 RPC、八个请求 / 响应与 `SkillBookWantedOrderInfo`,`SkillCraftingStateSnapshot` 写 `reserved 23;`;service 里的退役说明与下一条 RPC 之间留空行,是独立注释,生成器只取紧贴方法的前导注释,不会把它当成 `RentSkillLoadoutSlots` 的说明。`player_craft_comp.proto` 删 `SkillBookWantedOrder`、`reserved 16, 17;`;`client_request_ledger_comp.proto` `reserved 40, 41;`。
  - 生成:别的会话的一轮 `proto-gen` 已按这些 proto 在工作区跑过一次:`proto/message_id.txt` 的 560~563 已转成 `reserved:` 墓碑(恰好 4 条,卡在 `maxNewMessageTombstonesPerRun = 4` 上),`proto/persist_schema_gen.txt` 只多 `retired=ClientRequestRecord#40`、`#41`、`retired=PlayerCraftComp#16`、`#17` 四行;`SkillBookWantedOrder#1..8` 的台账行按生成器既有口径「所在类型已不再从玩家表可达,台账行保留不动」留着(`persistschema/apply.go`)。混版本字段保护台账就是这份生成的 `persist_schema_gen.txt`,`mixed-version-persist-field-protection.md` 里没有另一张手写的退役表,不用改。Gemini 再跑一次不会新增墓碑。
  - C++:`player_craft.{h,cpp}` 删四个入口与只给它们用的 `FillWantedOrderInfo`、`FindWantedOrderIndex`、`RemoveWantedOrderAt`、`CanCreditProxyMaster`(删前逐个 grep 过调用者),以及 `FillStateSnapshot` 里填我的悬赏的部分;`SealSkillBook` / `UnsealSkillBook` 不动。`client_request_ledger.{h,cpp}`:26~28 改名 `kRetiredWantedOrderPost` / `kRetiredWantedOrderCancel` / `kRetiredWantedOrderFulfill`,值保留不复用,删 `ClientRequestResult` 的 `wantedOrderId` / `wantedOrderGold` 及读写。`equip_craft_quality_rules.h` 删悬赏额区间、3 单上限与 `ValidWantedOrderGold`(`kProxyTaxBps`、`ProxyTaxGold`、`ProxyNetGold`、`kSealedSkillBookConfigId` 留着)。`player_skill_handler.cpp` 删四个 handler 整个函数(守护段非空、方法又不在 proto 里时生成器拒绝生成,`handler_gen.go` 的 `readCheckedCodeSections`);`player_handoff_gate.h` 删两条可跨节点重放的分类。`bag_test`:删悬赏段;纯规则用例改名 `EquipCraftQualityRulesTest.V2TitlesAndV4MilestoneBitsPureRules`,V5 用例只留同队免费代打、改名 `V5TeamFreeProxyChargesTeammatesZeroCraftFee`,书架上限用例只留制书、开卷两处满架拒绝、改名 `BookshelfCapRejectsCraftAndUnsealWithoutSideEffects`。
  - 配表:`tools/scripts/scene_wanted_order_retire_xlsx_patch.py`(幂等,已跑,删行后读回核对其余行逐行不变)删 `data/MessageLimiter.xlsx` 的 560~563 四行;号按名字从 `message_id.txt` 查,活号行与墓碑行都认。`kSkillWantedPosterBookshelfFull` 的 Tip 行保留、不复用。
  - trade 跨语言用例 `wanted_skill_book_crosslang_test.go`:删掉悬赏额区间与 3 单的比对(C++ 已没有这几项,规则只剩 trade 一份),5% 税的写法与 3414 照比。
- 方案(客户端 `mmorpg-client`):
  - `SkillCraftClient` 的三种悬赏写操作改发 trade:发榜 `CreateWantedSkillBookOrder`、揭榜 `FulfillWantedSkillBookOrder`、撤单 `CancelCommerceOrder`;仍走它原有的「先存本机恢复记录再发送、结果未确认按原字节重发」待确认写槽,恢复记录换了前缀(`tradewantedpost` / `tradewantedcancel` / `tradewantedfulfill`),按种类核对身份字段齐全。trade 回包与交易行(`CommerceClient`)同口径:故障码、没有订单的空成功都算结果未确认,保留原请求。
  - 书架书揭榜:先发 scene 的 `SealSkillBook`,封卷回包带回道具号就立刻发揭榜(这是同一次点击的第二步,不受 800 毫秒写间隔限制);「封卷后揭榜」只记在内存里,发送前先记(回包可能在发送调用里同步回来),封卷失败、未发出或客户端重启都只剩封卷,书以封卷道籍留在背包,不丢。
  - 过渡态:发榜回「托管中」、揭榜回「交易中 / 交付中」时,每 2 秒按同一请求号重发原请求查结论,最多 15 次(约 30 秒);只在内存里跟,不占待确认写槽。有了结论提示挂榜、银两没托管成、成交实得(服务端给的 `seller_net_gold`)、书不符已原样退回、书没扣下,揭榜结论触发背包重拉(`WantedFulfillConcluded`)。
  - 悬赏榜:公开榜一页 50、按游标续取(列表末行「加载更多悬赏」);「我的悬赏」读 `only_mine`,显示阶段(`WantedSkillBookOrderPhase`),只有托管中与挂榜中能撤;跟进期间悬赏页写按钮锁住。`SkillCraftModels`、`SkillCraftWindow`、`GameClient` 跟着改。
  - 聚宝斋订单名称(复查补,2026-11-27):求购单会出现在发榜人与正在交书的揭榜人的「我的订单」里,`CommerceWindow` 的种类名原先把不认识的种类一律显示成「材料成交」,kind 7 会被错标。改成材料成交(kind 4)显式列出、kind 7 显示「悬赏求书」、其余不认识的新种类显示中性的「交易单」。撤单按钮沿用既有判断(状态 1 才显示,trade 只认发榜人),不另加。被否决:在聚宝斋里给求购单另做一套详情——悬赏页已有完整入口,这里只要名字不误导。
  - EditMode 用例(`SkillCraftClientTests`):V5 段改写为发榜跟到挂榜、撤单、翻页、揭榜跟到成交;新加 `WantedFulfillFromBookshelfSealsFirstAndUncertainReplyReplaysSameRequestIdAcrossRecreate`(书架书先封卷再揭榜、超时后重建客户端按同一请求号重发、书不符退回)。
- 为什么这样选:
  1. 发榜与揭榜的结论都按同一请求号重发原请求查:trade 对同号重发回当前合同(`priorRequest` 读的是当前行),一次就拿到 `WantedSkillBookOrderPhase`。`GetCommerceOrder` 只给 `CommerceOrderState`,托管悬赏(`wanted_escrow`)与有人交书(`wanted_fill`)都是状态 3,分不清;揭榜那边书不符退回与书没扣下都让单子回到挂榜,订单状态一样,只有按请求号重发才分得清。交接单原写「发榜后按 `GetCommerceOrder` 轮询」,按此改。
  2. 悬赏写操作留在 `SkillCraftClient`,不加到 `CommerceClient`:悬赏页的界面、状态与待确认写槽都在 `SkillCraftClient`;`CommerceClient` 只在聚宝斋界面里按需创建(`JubaozhaiUiRoot`),带自己的一份恢复存储。放进 `CommerceClient` 会让同一类请求分在两个存储、有两个重放者;生成的 `ClientPlayerJubaozhaiRpc` 走同一条网关连接,`SkillCraftClient` 直接调。交接单 §4 第 10 条原写「`CommerceClient` 加三条调用」,按此改为不加(加了没人调)。
  3. 跟进不占待确认写槽:交付可能要等发榜人腾背包(可能很久),占着写槽会把制书、研习、拆页全部锁住;结果由服务端照常办完,客户端最多只是看不到提示。
  4. `ClientRequestKind` 26~28 改名留值而不是删:值是存档里的数字,删掉之后有人按顺序补新枚举就会复用;名字带 `Retired` 让任何新调用点一眼看出已退役。
  5. 限流行删掉:导表器不拦(没有引用约束),但墓碑号永不派发,留着是死配置,还会让人以为这几个号仍在用。
- 被否决的做法:①揭榜后用 `GetCommerceOrder` 轮询(见上第 1 条);②把跟进放进待确认写槽、有结论才清(见上第 3 条);③旧 scene 格式的恢复记录(`postwanted:` 等)读出来直接丢弃——上线前清档、不读旧格式,读不出按既有口径锁住写操作并提示(fail-closed),只可能出现在开发机上;④给客户端加按技能筛选——服务端是页内过滤、一页可能不满,界面也没有筛选入口,不加。
- 代价与边界:
  - 悬赏求书只在 trade 资产通道打开的环境可用(K8s 以 `-AssetOps on` 部署,批 C);没打开时发榜、揭榜、公开榜回「功能未开放」,本人的单照常列、照常可撤。
  - 跟进的约 30 秒内悬赏页写按钮锁住;跟满次数仍没结论(多半是发榜人背包满)就提示稍后刷新,不再自动查。客户端重启后不再跟进,结论要玩家自己刷新悬赏榜与背包看;发榜人没有成交推送,书在下次背包刷新时出现。
  - 开发机上残留的旧悬赏单(scene 存档里银两已扣)随清档消失;不清档的话那部分银两回不来(字段已 `reserved`,不读旧格式)。
  - 工作区里那一轮生成已把客户端 `SceneSkillClientPlayerRpc.cs` 更新到「四条已删」,但当时 proto 的退役说明紧贴 `RentSkillLoadoutSlots`,被生成成了它的说明;proto 已改成独立注释,Gemini 重跑生成后去掉。

**Gemini 验证顺序(批 B;仓库根执行,只跑,红了保留首错原文交回 Claude 改)**:
1. 导表:先 `git status --short data/` 看别的会话的表改动,再 `dev.bat gen`;`python tools/scripts/scene_wanted_order_retire_xlsx_patch.py --dry-run` 报 `[ALREADY CURRENT]`;`git diff HEAD -- data/MessageLimiter.xlsx` 相对 HEAD 只少 560~563 四行。批 A 的两条 trade tip 与 r2-craft#2 的两条 skill tip 都要生成出来。
2. 协议:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`;`proto/message_id.txt` 相对 HEAD 只有 560~563 变成墓碑、582~584 不变;`proto/persist_schema_gen.txt` 相对 HEAD 只多四行 `retired=`;`cpp/nodes/scene/handler/rpc/player/player_skill_handler.h` 不再有四个悬赏方法;客户端生成的 `RentSkillLoadoutSlots` 不再带悬赏退役说明。
3. 客户端 `mmorpg-client`:`tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`(含 EditMode 程序集),再跑 EditMode `SkillCraftClientTests`(两条悬赏用例)。
4. C++:串行 MSBuild(`/m:1`)先编库再编 scene 与 `bag_test`;`bag_test.exe --gtest_filter=PlayerCraftQualityTest.*:EquipCraftQualityRulesTest.*`;`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`(本机只读跑过,13 条通过)。
5. Go:`go/trade` 下 `go build ./...`、`go vet ./...`、`go test ./internal/commerce/... ./internal/logic/...`,点名 `TestWantedRulesMatchTheCppConstants` 与批 A 的 `TestWanted*`。仓库里没有 `go/scene`,`go/scene_manager` 不涉及。robot 按 `robot/README.md` 在 `robot/` 下重跑 `go mod vendor`(`robot/vendor/proto/scene/player_skill.pb.go` 还是删四条之前的副本;robot 没用这四条,不刷新也编得过,但协议视图会与服务端不一致),再 `go build -mod=vendor ./...`。
6. 冒烟(§9 第 3 条,环境以 `-AssetOps on` 部署):发榜(托管中 → 挂榜中)、揭榜(背包封卷与书架书两种)、撤单、书不符原样退回、发榜人背包满(书等腾出一格才到账,揭榜人银两随后到账)、发榜人被获取封禁时整单退回且退全额。
