# 悬赏求书迁 go/trade 托管合同与未学书架上限(数据覆写审计 r2 第 4 轮 craft#1、#2)

> 日期:2026-11-26
> 状态:**§10 书架上限已落码(未编译,待 Gemini 验证);§1~§9 悬赏求书迁 trade 是定稿设计,代码未动**。按 AGENTS §10.2,预计改动约 45 个文件(§8),先停下等负责人对分批的明确同意再落码。
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
- 客户端:`SkillCraftClient` 改调 trade(`CommerceClient` 加三条调用);书架书揭榜 = 先 `SealSkillBook` 再揭榜;发榜 / 揭榜之后按合同状态轮询(托管中 → 挂榜中 / 交易中 → 已成交),与寄售界面同一套轮询;「我的悬赏」改读 `only_mine` 列表。
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
| 客户端 `Assets/Scripts/Game/Jubaozhai/CommerceClient.cs` | 三条调用 |
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

用例 `PlayerCraftQualityTest.BookshelfCapRejectsCraftUnsealAndFulfillWithoutSideEffects` 钉住:三处满了都拒绝且无副作用,腾出一格后揭榜成交、书架回到上限而不越过。

**为什么这样选**:书架是玩家主动管理的收纳(研习、拆页、封卷都能腾位置),满了拒绝新增是背包同类的标准做法,不丢任何东西。100 = 95 项永久技能各一本还有余量;按每本约 60 字节(含宗师署名)算,满架约 6 KB,离 1 MiB 很远。加书前判、不在加载时裁:裁掉就是删玩家的书。

**被否决的做法**:①加载时把超出部分裁掉或转邮件——删玩家资产或多出一条跨系统流程,上线前清档后也不会有超限存档;②按字节数而不是条数限——玩家看不懂「还能放几本」;③复用 `kInvalidParameter`——「参数错误」不是玩家能理解的原因,命名第一原则要求语义准确;④揭榜时发榜人满了把书改投邮件——给一条要退役的路径(§3.10)加新流程不值得。

**代价与边界**:书架满了必须先处理才能继续制书、开卷;两条 tip 要在 `data/tip/Tip.xlsx` 的 `//skill_error base=7000` 段末尾(`SkillLoadoutAttributeResetRequired` 之后、`//buff_error` 组头之前)加两行——`SkillBookshelfFull`「书架已满(最多 100 本),请先研习、拆页或封卷后再试」、`SkillWantedPosterBookshelfFull`「发榜人的书架已满,暂时无法交付这张悬赏单」,`fault` 列留空——再导表(Gemini),导表之前 C++ 不能编译。已超限的旧存档(只可能来自本机开发库)照常加载,只是不能再加书。悬赏求书迁 trade 之后(批 B),揭榜不再往书架加书(发榜人收到的是背包里的封卷道籍),第三处判定随旧路径一起删掉。
