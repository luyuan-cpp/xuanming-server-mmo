# 聚宝斋(玩家间人民币寄售交易)设计 — 2026-09-14

> **2026-10-15 最新决定：采用 mall 适配聚宝斋、游戏道具买卖与拍卖行，见 [移植决定 D-17](xuanming-port-decisions-20260910.md#d-17-采用-mall分批适配全球游戏经济2026-10-15覆盖-d-16-的比较边界)。** 原 `go/trade` 的客户端门面及已落浏览/收藏保留，后续经济写面接入一个权威订单/托管/结算域，不能再并行维护第二份订单与钱包。所有地域共享同一逻辑 TiDB 市场；C++ 在线资产权威不迁。当前首批 mall 官方 SKU 订单实现不代表本文 P3/P4/P6 已完成；唯一实例托管、C++ SQL 持久确认、角色过户、支付渠道与卖家结算仍须逐项接线验收。下方历史时间点状态不能作为当前持久性保证。

> **2026-10-19 人民币寄售买卖主流程**(下单 → 付款 → 发货 → 卖家入账)的现有设计与分步计划见 [jubaozhai-rmb-consignment-plan.md](jubaozhai-rmb-consignment-plan.md),两处已批准的偏离见 D-17.6。本文 §5.2 订单状态机、§7 支付、§8 `trade_seller_ledger` 的旧规划以新计划为准:收益账改放商城、唯一买家由商城一次性挂单商品裁决；独立正式设计稿尚未落入仓库。

> 状态:**P1 本地自动化功能验收通过(2026-09-17):Go / C++ 构建、D-14 迁移、zone/global 双区 robot、真实客户端浏览/详情/收藏重登均通过;U 实体按键待人工检查(自动化走生产入口按钮的同一 Toggle)**;**P2 通用资产通道 2026-09-18/19 已落码、未编译**(与帮会 B4a/B4b 合并实现,见 §6.4 与 `docs/design/guild-phase2/04-asset-channel.md`);P0-b / P3 起未开工。客户端界面已拼好(`mmorpg-client` 的 `Assets/Scripts/UI/Ugui/Jubaozhai/`、`Docs/JUBAOZHAI_UI.md`),本文件定义服务端与接线。
> 调研依据:2026-09-14 三路只读核查(资产原语 / 新服务接线 / 账号与 zone 模型),结论与证据摘要见 §2。

## 0. 结论(先读这节)

1. 聚宝斋 = 一个**全局 Go 服务 `trade`**(产品名聚宝斋)+ **C++ scene 上的三条通用资产 RPC**(`AssetDebit/AssetAbortDebit/AssetCredit`,与帮会共用,非聚宝斋专属)+ **login 上的角色过户**。按 zone 分区还是全服,由服务端配置 `Market.Scope` 决定,商品上记 `market_zone`(卖家 home_zone,服务端查,不信客户端)。
2. 难点不在列表和界面,在"**东西和钱怎么安全换手**"。今天仓库里**没有**托管扣出、幂等发放、角色过户、登录冻结这四样原语(§2),必须先补。
3. 结算是人民币(J-1)。服务端只做**订单状态机 + 支付渠道接口**;本地和冒烟用 `mock` 渠道跑通,真实渠道、实名/防沉迷、卖家提现后接(P5)。
4. 首批四类商品都在范围(J-2),但按依赖分期:游戏币/装备道具/宠物共用一套资产通路(P2→P3);角色交易额外依赖"账号数据持久化"(P0-b→P4)。
5. **两道上线闸,不过不许开真钱交易**:~~GM 客户端消息无鉴权(能直接刷钱再卖钱)~~(**P0-a 2026-09-18 已落码未编译**,见 §12 第 1 条);账号数据只在 Redis(角色过户无持久化落点)。

## 1. 已拍板 / 待拍板

| 编号 | 决策 | 状态 |
|---|---|---|
| J-1 | 结算币种 = 人民币。价格以**分**(uint64 `price_fen`)存储与传输 | 用户 2026-09-14 拍板 |
| J-2 | 首批商品 = 游戏币、装备道具、宠物、角色 | 用户 2026-09-14 拍板 |
| J-3 | 按 zone 区分、可切全服(本文 §4 给出形态) | 用户需求;形态为本文提议 |
| J-4 | 服务域名 `trade`:proto 放 `proto/trade/`、`package trade;`,复用已存在的 `TradeNodeService=12` / `NODE_TRADE=17`,不改 C++ 枚举 | 本文提议 |
| J-5 | 资产幂等账本与 D1(`xuanming-port-feasibility-20260902.md` §8.1)**合并为同一组件**,不做两套 | 本文提议 |
| J-6 | 存储落独立全局库 `mmorpg_trade`,DDL 带 TiDB `/*T!*/` 方言。契约 §4 规定首个建表的全局服务开工前**必须先拍 D4**(库归属 + 迁移器 vs 手写 `deploy/mysql-init`),本文推荐"每服务一库 + 迁移器" | **已拍:按 [port-decisions D-14](xuanming-port-decisions-20260910.md)**(每服务一库、表以 proto 为源、`go/schemamigrate` + `-migrate` Job;string 唯一 / 索引列限长 ≤191、每表至多一个唯一键) |
| J-7 | tip 段 `//trade_error base=20000 width=1000`(17000–19999 留给 mail/chat/rank) | 本文提议 |
| J-O1 | 公示期 / 寄售期时长(默认 公示 48h、寄售 7d,可配) | **负责人 2026-10-21 确认：采用默认值，允许配置调整** |
| J-O2 | 平台手续费率(默认 5%,向上取整到分),卖家收入冷静期(默认 3 天) | **负责人 2026-10-21 确认：采用默认值，允许配置调整** |
| J-O3 | 角色交易:出售前必须离帮、好友关系保留 | **负责人 2026-10-21 确认：采用默认规则，允许配置调整** |
| J-O4 | 可交易货币种类(默认只放 `kCurrencyGold`;元宝 `kCurrencyDiamond` 默认关,绑定元宝永不) | **负责人 2026-10-21 确认：采用默认规则，允许配置调整；绑定货币仍禁止** |
| J-O5 | 侧栏「货架」语义(本文假设 = 我的商品 + 我的订单)、上架界面(现有 7 张设计稿无上架流程) | **待产品/美术确认** |

## 2. 现状核实(2026-09-14)

| 能力 | 结论 | 证据 |
|---|---|---|
| 背包持久化 | **已有**。`player_database.bag_component = 13`,登录/存盘/跨 zone/快照均有调用点(`xuanming-port-feasibility` §8.1 的"零调用点"已过时) | `proto/common/database/mysql_database_table.proto:129`;`player_database_loader.cpp:97,124` |
| 按 guid 批量扣物品(全或无) | **没有**。`BagService::RemoveItem` 单件、零生产调用;`ReserveForBatchRemove` 不存在;失败一律 `kInvalidParameter` | `cpp/libs/modules/bag/bag_service.h:44-81` |
| 物品预设 guid 回包 | **已有**(`InitItemParam.itemPBComp.item_id` + 撞号预检) | `item_system.h:7-11`;`bag_system.cpp:293-314` |
| 物品可交易/绑定列 | **没有**。Item 表只有 `id/max_stack_size/equip_kind`;`ItemEntry` 9/10 号字段只是注释 | `data/schema/item_table.proto`;`bag_quest_mail_data.proto:82-83` |
| 货币 | 已持久化,**只能在线改**,接口无 txType/correlation;入账会被补缴扣、被 GM 封禁拒 | `currency_system.h:36-47`;`currency_system.cpp:88-147` |
| 宠物 | 已持久化、有 `pet_id`;**无移除接口**,`GrantPet` 不支持预设 id;出战用标量 `active_pet_id` | `player_pet_comp.proto:17,35`;`player-pet.md` §8.4 |
| 幂等发放 / 托管(D1/D1b) | **没有落码**,只在文档里 | `cpp/ go/ proto/` 零命中 |
| transaction_log 交易类型 | 枚举有 `TX_TRADE/TX_AUCTION_SELL/TX_AUCTION_BUY`,**零写入方**;`LogItemTransfer` 零调用 | `transaction_log.proto:22-65` |
| 战斗中闸 | Bag/Currency 写入口**不拦** `InBattleComp`(只拦跨 zone 冻结) | `bag_service.cpp:82` 等 |
| Go → scene 同步调用 | **已有**:`SceneNodeGrpc`(match `PrepareBattle` 范式,读 `player:{%d}:location` 寻址) | `proto/scene_manager/scene_node_service.proto`;`go/match/internal/logic/gather.go:282` |
| scene → Go | 只允许 SceneManager / DataService | `cpp/nodes/scene/main.cpp:78` |
| 账号 ↔ 角色 | 账号角色列表**只在 Redis `account:{account}`,TTL 12h,找不到 MySQL 写入**;反查键 `player_to_account:{id}` 永不过期且 EnterGame 会据此自愈回写 | `go/login/etc/login.yaml:68`;`entergamelogic.go:144-190` |
| 登录冻结 / 封禁 | **没有**任何字段或闸;踢人 `KickPlayerEvent` 只有 session_id,提示写死"顶号" | `gate_event_handler.cpp:160` |
| 角色名 / 等级对 Go 可见 | **无角色名**;等级只在 `level_component` blob,Go 读不到 | `entergamelogic.go:428-433` |
| GM 客户端消息 | 6 条(37/49/94/95/175/187)作为客户端消息开放;**2026-09-18 起按运行模式关闭**(`GATE_RUN_MODE`/`SCENE_RUN_MODE` 非 dev/test 即拒,默认 prod),proto 未动、消息号仍在 `IsClientMessageId` 里 | `cpp/nodes/gate/gate_gm_client_messages.h`;`cpp/nodes/gate/SECURITY.md` §3;`proto/scene/player_currency.proto:65-73` |
| 新服务客户端可达 | 只在路由服模式(`GATE_CLIENT_RPC_ROUTER=1`)可达;K8s 路由服 manifest 与 POD_IP 通告已补齐,默认仍为 0,待 K8s 路由模式 battle-smoke 通过后再切换。**(2026-09-29 更正)** K8s 默认已切为 `"1"`(turn-based §22 D75,`tools/scripts/k8s_deploy.ps1:163-164`),battle-smoke 改为事后补验;trade 在 K8s 默认部署下**按设计可达、待补验**(前置:infra 已部署 `client-rpc-router`;集群外玩家另需 `-ClientEntryMode external`) | `cpp/nodes/gate/main.cpp:206-209`;[zone 契约 §14 / §18](microservice-zone-contract-20260914.md) |
| 新 Go 服务接入口径 | 以 `microservice-zone-contract-20260914.md` 为准(§2 注册 / §3 客户端入口 / §4 数据归属 / §7 部署登记,chat v1 为首个样板) | 该文档 2026-09-14 由并行会话落地 |

## 3. 术语与界面映射

- **商品(listing)**:卖家上架的一份资产 + 一口价。**订单(order)**:买家对商品的一次购买。
- **托管(escrow / debit)**:上架时把资产从卖家身上扣出,快照存到交易库。**交付(credit)**:把快照里的资产发给买家;下架/过期时发回卖家(**回退**)。
- **公示期**:商品可见、可收藏、**不可买**;**寄售期**:可买。
- 界面映射(`JubaozhaiTab` / `JubaozhaiSection`):
  - 顶部「公示列表」= `LISTING_TAB_PUBLIC_NOTICE`;「寄售列表」= `LISTING_TAB_ON_SALE`(含 P3 的锁定中商品,显示"交易中")。
  - 侧栏「寄售」= `LISTING_SECTION_CONSIGNMENT`;「拍卖」= `LISTING_SECTION_AUCTION`(P6,当前回 `TradeFeatureDisabled`);「货架」不是浏览分区,走 `GetMyShelf`(P1 = 我的商品;P3 追加我的订单,J-O5)。
- **类目编码**(协议 `ListingCategory`):1 角色 / 2 宠物 / 3 武器 / 4 防具 / 5 套装 / 6 法宝 / 7 首饰 / 8 召唤令,与客户端 `JubaozhaiCategory` 同序(客户端值 = 协议值 − 1);9 游戏币(客户端暂无页签,收到跳过)。
- **子类编码**:0 = 全部 / 无子类;k(1 起)= 客户端 `JubaozhaiCatalog.SubcategoriesFor(类目)` 第 k 个标签。服务端按下表校验上限;**两边任一侧增删标签,必须同时改另一侧**(这是协议契约,不是展示细节)。P3 物品上架时由 Item / Pet 表的 `market_subcategory` 列给出编码(§6.2)。

| 类目 | 上限 | 标签顺序 |
|---|---|---|
| 1 角色 | 4 | 破军 玄霄 逐风 丹心(门派) |
| 2 宠物 | 6 | 普通 灵兽 变异 神兽 元灵 其他 |
| 3 武器 | 5 | 枪 爪 剑 扇 锤 |
| 4 防具 | 5 | 男帽 女帽 男衣 女衣 鞋子 |
| 5 套装 / 6 法宝 / 7 首饰 / 9 游戏币 | 0 | — |
| 8 召唤令 | 2 | 神兽召唤令 元灵召唤令 |

## 4. zone 模型

- `market_zone`:商品所属的**逻辑市场分区** = 上架时卖家的 home_zone,由服务端调 `DataService.BatchGetPlayerHomeZone` 写入,**不接受客户端传值**;未映射时 fail-closed 拒绝上架/下单(契约 §4)。无论当前是否全服都写,切换不补数据。
- 配置 `Market.Scope`:
  - `zone`:浏览只返回 `market_zone` 与买家 home_zone 解析到同一个最终区的商品(`market_zone IN (Members(z))`);下单时服务端再校验一致(`kTradeZoneMismatch`)。
  - `global`:浏览不按分区过滤;客户端可传 `zone_filter`(0 = 全部)仅作筛选;允许跨区购买。
- 不照搬 guild `GetGuildRank` "客户端传 0 即全服"的做法:市场范围是服务端策略,不是客户端参数。
- 跨区交付天然成立:资产经 `player:{%d}:location` 找到买家当前所在 scene;角色过户不改 home_zone(映射是 SETNX 永不覆盖),买家选该区进入即可。
- 合服(2026-10-30 改为不停服,[总体方案 §7.1](player-data-tidb-scaling-plan.md)):合服只写一条 `zone_merged_into` 映射,`market_zone` 和 `seller_zone_at_listing` 都不改写。trade 启动时从 `data_service.GetMergedZones` 取映射(`placement.MergedZoneCache`,读不到拒绝启动),可见性判定两边解析到最终区再比,按区查询用 `market_zone IN (Members(z))`,客户端看到的 `market_zone` 是最终区。撤销合服只删映射:合服期间上架的商品记的是目标区,留在目标区。人民币寄售不按区过滤(`RmbList` 和商城都不看 `market_zone`),这个字段只是合同条款和回显,不解析。订单、流水、审计表保留原 zone 列。
- 合服闸:已删除。合服不动商品行,没有需要挡的窗口。

## 5. 状态机

### 5.1 商品

```
ESCROWING ──托管成功──▶ PUBLIC_NOTICE ──公示期满──▶ ON_SALE ──下单──▶ LOCKED ──支付成功──▶ SOLD(终态,交付归订单)
    │                        │                         │  ▲                 │
    │托管被拒                │卖家下架                 │  └──订单关闭(未付)─┘
    ▼                        ▼                         ▼
ESCROW_REJECTED(终态)     RETURNING ◀──卖家下架/寄售期满──┘
                             │回退交付成功
                             ▼
                          RETURNED(终态)
```

- `LOCKED` 期间不可下架;订单关闭时若寄售期已过,直接转 `RETURNING`。
- 每个卖家同时最多 1 条 `ESCROWING`(保证 scene 结果环不被挤掉,§6.1);在售条数上限可配。
- **落库口径(P1 起)**:图中的 `PUBLIC_NOTICE` / `ON_SALE` 不是存储状态。库里只存 `LISTING_STATUS_LISTED`,展示阶段 `ListingPhase` 由同一次 `now` 与 `notice_end_ms` / `sale_end_ms` 推导(`now < notice_end_ms` → 公示;`now < sale_end_ms` → 寄售;否则 ENDED)。这样公示期满不需要后台任务改状态,也没有"时间到了但状态没改"的窗口。寄售期满后的回退(`RETURNING`)仍需 P3 的扫描任务。存储枚举见 `proto/trade/trade_table.proto` 的 `ListingStatus`。

### 5.2 订单

```
CREATED ──向渠道下单──▶ PAYING ──验签回调 + 主动查单双确认──▶ PAID ──▶ DELIVERING ──交付成功──▶ DELIVERED ──冷静期满──▶ SETTLED
   │                      │                                     │            │
   └──支付窗口超时(先向渠道关单/查单,确认未付)──▶ CLOSED_UNPAID    └─交付永久失败─▶ REFUNDING ──▶ REFUNDED
                                                                          (商品转 RETURNING)
```

- 支付窗口默认 15 分钟;关单后才到的支付回调 → 自动退款(`LATE_PAID_REFUNDING` 分支,记审计)。
- **"已支付"只认渠道**:验签回调 + 主动查单,金额/币种/商户单号三项一致;客户端任何"我付了"都不算。
- 渠道交易号唯一索引,同一回调重复到达幂等。
- 交付的暂时失败(背包满、战斗中、离线)不算失败,保持 `DELIVERING` 重试;只有 scene 返回终局拒绝才转退款并告警人工。

## 6. 资产通路(核心)

### 6.1 原则

1. 资产**在玩家身上时**权威在 C++ player blob;**托管期间**权威在交易库的快照。
2. 所有资产移动只走 `SceneNodeGrpc` 同步 RPC 到玩家当前所在 scene,scene 在 loop 线程**同一次处理**内完成"校验 → 应用 → 记账 → 写 transaction_log"。
3. **玩家资产指令账本 `PlayerAssetOpLedgerComp`**(J-5,与 D1 合并):
   - 两条独立 seq 流:`ASSET_OP_STREAM_TRADE_DEBIT`(托管扣出)与 `ASSET_OP_STREAM_TRADE_CREDIT`(发放)。枚举 `AssetOpStream` 定义在 `proto/common/asset/asset_op.proto`,与帮会的 `GUILD_DEBIT/GUILD_CREDIT`、预留的 `SYSTEM_CREDIT` 并列;**每个服务独占自己的流,各自分配 seq**。满足 D1 硬边界"扣减不进同一条 seq 流"。
   - Go 在写商品/订单的**同一个事务**里为 `(player_id, stream)` 分配单调 seq 并写 outbox 行(`trade_asset_op`)。
   - scene 持久化每条流的 `watermark` + 1024 位位图,落 `player_database` 下一个空闲字段号;加载、存盘、跨 zone 快照三处一起接。**不能按时间裁剪、不能只存内存**(否则重放双发)。窗口语义与 Go 端守卫(每流未决 <=16、跨度 <512)见 `docs/design/guild-phase2/04-asset-channel.md` §S4;seq 分配与重投循环复用 `go/shared/assetop`,不自写第二份。
   - DEBIT 另存最近 8 条结果环(含资产快照),用于"scene 已扣但回包丢了"时重试取回快照。
     - **P2 实际状态:通用通道 v1 没有结果环**(开放项 J-A1)。重试靠的是账本位图给出同一结局,不靠环里的快照;快照字段号已占好但零写者零读者,详见 §6.4。
4. **结局一旦决定就固定**:scene 见过某 seq,结局(`APPLIED` / `REJECTED`)落盘,重试返回同一结局。暂时性条件(背包满、战斗中、跨区冻结、玩家不在本节点)返回 `RETRY` / `NOT_HERE`,**不**记账。
5. **`AssetAbortDebit(seq)`**:scene 未见过该 seq 则记为 `REJECTED` 占位;见过则返回原结局。Go 靠它确定性收口任何悬挂的托管。
6. **只在玩家在线时应用**。Go 的 reconcile 循环扫描未决 outbox,按 `player:{%d}:location` 批量查在线者重投;离线玩家的发放等其上线,延迟 ≤ 扫描周期(默认 2s)。v1 不做登录时 scene 主动拉取;若日后要做,按契约 §8 走直连 gRPC(专用 READY 选择器 + `nodeTypeNameMap` + 前缀表 + scene 白名单),**不能借路由服**。
7. 重复投递安全(scene 幂等),所以多副本 reconcile 用行级租约只为省流量,不为正确性。

### 6.2 各类商品

| 类别 | 托管(DEBIT) | 交付(CREDIT) | 需要新写的原语 |
|---|---|---|---|
| **游戏币** | `DeductCurrency(kCurrencyGold, n)`,tx=`TX_AUCTION_SELL`,correlation=listing_id | `AddCurrency`,成交所得 tx=`TX_AUCTION_BUY`,退回本人托管的 tx=`TX_AUCTION_RETURN`;照常经过补缴抵扣(统一入口);获取封禁的处置见下方「获取封禁与托管交易」 | Currency 增减接口加 txType + correlation_id;区分"余额不足/冻结"错误码;可交易币种白名单(J-O4);帮会 B4a 已落 txType + correlation_id 与 `kAsset*` 错误码,本项已不需新写 |
| **装备道具** | 按 `item_uuid` 列表全或无扣出(一条商品最多 8 件,用于"套装");只允许主背包、非穿戴中;Item 表 `tradable=false` 一律拒(默认 false,fail-closed) | 以快照 `ItemEntry` 预设 guid 回包,保留原 `item_uuid` | `Bag::ReserveForBatchRemove` + `BagService::RemoveItemsByGuid`(全或无、带流水;**拦跨区冻结但刻意不拦战斗**—— D48 红线:局中拦一切背包写的闸只许放在资产 RPC 入口层,下沉到 BagService 会把战斗结算自己的扣物也拦住,结算永久卡死);Item 表加 `tradable` / `market_category` / `market_subcategory` 三列 |
| **宠物** | 按 `pet_id` 移出;出战中拒绝(`kTradePetActive`,提示先收回) | `GrantPet` 以快照 `PetInstance` 预设 `pet_id` 回填 | `PetSystem::RemovePetForTrade`;`GrantPet` 支持预设 id + 撞号检查;Pet 表加 `market_subcategory` 列(界面的 普通/灵兽/变异/神兽/元灵 与表里 quality 1-4 不同轴) |
| **角色** | 见 §6.3 | login `TransferPlayer` | 见 §6.3 |

scene 侧所有 DEBIT/CREDIT 入口统一拦:跨区冻结、`InBattleComp`(局中挡一切背包写,`moba-battle-target-architecture.md:113` 红线)、玩家不在本节点。

**获取封禁与托管交易**(2026-11-10,数据覆写审计 r2-trade#1;原先这里写的「GM 封禁 → 终局拒绝 → 退款 + 告警」与引擎行为不符,已改):

- **退回不算获得。** 退回腿是把玩家自己托管的资产还给他,包括 seller_return、buyer_return、bid_return、人工整单退回的 `<扣出腿>/refund` 腿,以及它们的补发(`#k` 后缀)。这些腿带 `TX_AUCTION_RETURN`(`go/trade/internal/commerce/leg_tx_type.go`),scene 的资产转入、`AddCurrency`、`BagService::AddItems` 都按 `GainBlockService::CountsAsGain` 放行。所以被封禁的卖家照常取回自己托管的物品,买家的退款照常入账。
  - 原先退回腿也带 `TX_AUCTION_BUY`,会被判成终局拒绝:合同只能转人工,人工退款又被已到账的腿挡住。
- **成交所得照常判封禁。** 买家被封禁获取所购物品,或卖家被封禁获取所收币种时,这条交付腿记 `REJECTED kAssetBlocked`。另一条交付腿是同时发出的,可能已经到账,所以合同转人工(`final_credit_failed`),解封后用 `commercefix reissue` 补发。在这之前,钱和货分别停在两边,不丢,也不复制。
- **待做**(打开 GM 封禁处置,或交易资产通道之前做):
  - 交付改为先发买家那条,到账之后再发卖家那条;买家那条被拒时,自动退回买家的钱和卖家的物品,不转人工。卖家那条被拒时,所得留在托管里,等解封后补发。
  - 下单前经 data_service `GetPlayerItemGainBlock` 预检。这只能降低发生的频率,不能代替提交点的判定。
  - 这两项改在 `engine.go`、`market.go`、`manual.go` 的四条交付路径上。这几个文件正有缺口清单第 16 条的在途改动,所以等那批落地之后再做(r2 文档 §6「2026-11-10」批 T)。

### 6.3 角色交易

- **交易的是角色,不是账号**:把角色从卖家账号挪到买家账号。账号级找回/改密与已售角色无关,规避"卖号后找回"。
- 前置 **P0-b**:账号角色列表今天只在 Redis(TTL 12h),必须先有持久化权威存储;冻结标记与归属写在**同一条记录**(`AccountSimplePlayer.trade_lock_listing_id`),过户时原子一起改。**不放共享 Redis 单独键**(共享库 allkeys-lfu 会淘汰,冻结丢失 = 公示期内卖家上线改角色)。
- 上架流程:
  1. 卖家以该角色在线发起;条件:非帮主且已离帮(J-O3)、不在战斗、角色数据已加载。
  2. Go 事务:商品 `ESCROWING` + DEBIT op(kind=`CHARACTER_LOCK`)。
  3. Go 调 login `LockPlayerForTrade(player_id, listing_id)`(幂等;此后 EnterGame 拒绝 `kTradeCharacterLocked`)。
  4. Go 调 scene `AssetDebit(CHARACTER_LOCK)`:scene 挂交易锁组件(拒绝该玩家一切客户端写消息)→ 存盘 → 从落盘后状态生成展示快照(职业、等级、属性摘要、宠物数、游戏币、装备摘要)→ 带原因踢下线。
  5. 快照入库,商品转 `PUBLIC_NOTICE`。
- 下架/过期:login `UnlockPlayerForTrade`。
- 交付:login `TransferPlayer(tx_id, player_id, from_account, to_account)`:
  - 按账号字典序拿两把 `account_lock:create:{acc}`,再拿 `player_locker:{id}`;
  - 校验卖家仍拥有且锁的是本 listing、买家角色数 < `MaxPlayersPerAccount`;
  - 同时改两账号记录 **和 `player_to_account:{id}`**(不改的话卖家 EnterGame 会自愈把角色写回卖家);清锁;按 tx_id 幂等。
  - 买家角色位满:下单时就拒;交付时满则保持 `DELIVERING` 并提示腾位,超过期限(默认 3 天)退款。
- 需要顺带改:`KickPlayerEvent` 加原因字段,gate 不再写死"顶号"提示;`RemovePlayersFromAccounts` 补账号锁(与过户/建角并发会互相覆盖)。
- 买卖双方身份取**账号**:`player:session:{id}` 的 `account` 字段;同账号不能自买。

### 6.4 `SceneNodeGrpc` 资产 RPC(通用名,帮会 B4a 落地)

聚宝斋**不自造** `TradeDebit/TradeCredit/TradeAbortDebit`,而是复用帮会 B4a 落地的**通用资产通道**,
交易只是独占其中两条流。契约在 `proto/common/asset/asset_op.proto` +
`proto/scene_manager/scene_node_service.proto`,完整语义见 `docs/design/guild-phase2/04-asset-channel.md` §S4。
本节原先那版 `Trade*` 草案(含 `TradeAssetOpOutcome`、`TradeDebitRequest/Response`)**已作废**。

```proto
rpc AssetDebit(AssetOpRequest) returns (AssetOpResponse) {}
rpc AssetAbortDebit(AssetOpRequest) returns (AssetOpResponse) {}
rpc AssetCredit(AssetOpRequest) returns (AssetOpResponse) {}
```

- 交易走 `ASSET_OP_STREAM_TRADE_DEBIT` / `ASSET_OP_STREAM_TRADE_CREDIT`;`tx_type` 受 scene 白名单
  `kAssetOpStreamRules` 约束(当前 TRADE_DEBIT: `TX_AUCTION_SELL`;TRADE_CREDIT: `TX_AUCTION_BUY`、`TX_TRADE`),
  加玩法时只改那张表,不改通道。
- 结局枚举是 `AssetOpOutcome`(`ASSET_OP_OUTCOME_*`)。
- 请求必须带 `stream_epoch` 与 `auth`(caller = `trade`,密钥取环境变量 `MMORPG_ASSET_OP_SECRET_TRADE`),
  验签见 04 文档 §4.32。`correlation_id` 取 outbox 行的 `op_id`;listing/order 另记在 `ref_kind` / `ref_id`。
- 资产载荷统一装进 `AssetBundle`:货币走 `currencies`,**可叠加**物品走 `items`(config_id + count),
  **不可叠加**物品按 guid 走 `item_uuids`(全或无),宝宝走 `pet_id`。角色锁不经本通道,见 §6.3。

**与草案的差异(P2 落码后的实际状态,不是待议项)**

- **J-A1 结果环与资产快照:通用通道 v1 没有结果环。** 快照的字段号已在 P2 一并占好
  (`AssetBundle.item_uuids = 3`、`pet_id = 4`、`AssetOpResponse.snapshot = 5`,配
  `AssetSnapshot`/`AssetItemInstance`/`AssetPetInstance`),但**至今零写者零读者**:scene 不填、Go 不读。
  占号只为避免将来抢号,**不等于"响应里已经有快照"**——空 message 与"本次没动任何库存"在 proto3 上
  不可区分,照着接线会静默走错分支。
- **J-A2 持久化等级(已收口,2026-11-06 改写)**:`durable` 现在就是 TiDB 提交确认——该 seq 的结局进了
  "最后一次提交成功的玩家存档"(go/db `Save` 回 COMMITTED / EQUAL,资产与资产账本在同一笔事务里提交),
  不再有"只到 Redis、回退 MySQL 时重复"的等级(04 文档 C5 已随玩家存档移出 Redis 作废)。
  剩下的同类风险只有整库按时间点恢复(PITR)到更早时刻:账本和资产一起回退,而 Go 侧可能已终结,会重复;
  处置按 `docs/ops/mysql-backup-pitr-runbook.md` §4.1a(只回档存档库时,回档点之后已终局的资产结果要核账,
  能全库回档就不单回存档库)。人民币寄售接入真金交付前,只需确认接受这一条 PITR 核账口径。
- **J-A3 已解决**:`correlation_id` = `op_id`;按 guid 扣物与宝宝的字段形状见上。

**J-A4 签名未覆盖 `item_uuids` / `pet_id`(硬前置,未做完不得启用 TRADE_* 流)**

§4.32 的 canonical 串当前只覆盖货币与 config_id 物品两段。scene 的 gRPC 是
`InsecureServerCredentials()`(`node.cpp:623`),集群内任意进程可连可嗅:攻击者截下一条合法
TRADE_DEBIT,只把 `item_uuids` 换成该玩家的其它装备再发出去,这个 seq scene 没见过、签名照样通过,
于是扣掉的是被换的那件。**seq 幂等与 300s 时间窗都挡不住这种"同 seq 抢跑改载荷"。**
启用前必须把 canonical 扩成 `…;i=<config_id:count,…>;u=<item_uuid,…>;p=<pet_id>`(空则 `;u=;p=0`),
并**同批**改 C++ `AssetOpCanonical`、Go `assetop.Canonical` 与两边共用的 golden 字面量。
帮会走 GUILD_* 流、这两个字段一律留空,不受影响。

## 7. 支付(人民币)

- Go 接口(`internal/payment`):
  ```go
  type Provider interface {
      CreatePayment(ctx context.Context, o PayOrder) (PayIntent, error) // 返回 pay_url / 二维码内容
      ParseNotify(r *http.Request) (Notify, error)                      // 验签;失败不落任何状态
      Query(ctx context.Context, orderID uint64) (PayStatus, error)
      Close(ctx context.Context, orderID uint64) error
      Refund(ctx context.Context, req RefundRequest) (RefundStatus, error)
  }
  ```
- `mock` 渠道:**只在 `Mode: dev` 下允许**,配置校验不通过即拒启动;确认支付走内部 `TradeAdmin.MockConfirmPayment`(非客户端协议,robot/GM 用)。
- 真实渠道(P5):微信支付 / 支付宝适配同一接口;回调 HTTP 监听独立端口经 ingress 暴露;商户密钥只经环境变量 / K8s Secret 注入,不进 git、不进日志(AGENTS §9)。
- 客户端**永不处理支付凭证**:服务端返回 `pay_url` / 二维码内容,客户端打开系统浏览器或展示二维码,然后轮询订单状态。
- 金额:`fee_fen = ceil(price_fen × fee_bps / 10000)`,`seller_proceeds_fen = price_fen − fee_fen`;价格上下限可配。
- 卖家收入进 `trade_seller_ledger`:`PENDING`(冷静期,留给拒付/风控回收)→ `AVAILABLE` → `WITHDRAWN`。v1 只记账 + 管理端导出,提现是 P5。
- `BuyerEligibility` 接口(实名、防沉迷、未成年人消费限制):v1 mock 放行;**真钱上线前必须接入**。

## 8. 数据(库 `mmorpg_trade`,J-6)

**建表方式按 [port-decisions D-14](xuanming-port-decisions-20260910.md)**:表结构的唯一事实源是 `proto/trade/trade_table.proto`(每张表一个 message,`OptionTableName` 锁表名,TiDB 方言只用 proto 选项 500021-24 表达,业务代码不手写 DDL);建表 / 加列经 `go/schemamigrate`(trade 是首个建表服务,本批同批落地)。受 proto2mysql 表达力约束:**主键只用整数列、string 列为 MEDIUMTEXT 且进索引只按 191 前缀、每表至多一个唯一键**;下表 P3 规划里的 `provider_trade_no` 唯一、`(provider, provider_event_id)` 唯一各占其表唯一的 UNIQUE,字符串须应用层限长 ≤191。主键全部 NONCLUSTERED + `SHARD_ROW_ID_BITS=4`(TiDB 决策 D3)。id 由 `shared/idsegment` 发号(业务键 `trade_listing` / `trade_order`,§7.1 单一生产者 = trade 服务),业务键须登记进 data_service `BootstrapTags`(代码默认清单、`data_service.yaml`、K8s ConfigMap 三处)。

**P1 实际建的表**只有 `trade_listing` 与 `trade_favorite`(`TradeListingRecord` / `TradeFavoriteRecord`):P1 不含 `kind` / `active_order_id` / `snapshot` 列,P2/P3 按需在 proto 里追加字段,由 schemamigrate 的 ADD COLUMN 加列。下表是全期规划:

| 表 | 用途 | 关键列 / 索引 |
|---|---|---|
| `trade_listing` | 商品 | `listing_id` PK;`seller_player_id`、`seller_account`、`market_zone`、`seller_zone_at_listing`、`kind`(1 币/2 物品/3 宠物/4 角色)、`category`、`subcategory`、`title`、`level`、`price_fen`、`status`、`notice_end_ms`、`sale_end_ms`、`active_order_id`、`snapshot`(`TradeAssetSnapshot` 二进制)、`version`;索引 `(market_zone,status,category,subcategory,price_fen)`、`(status,sale_end_ms)`、`(seller_player_id,status)` |
| `trade_order` | 订单 | `order_id` PK;`listing_id`、`buyer_player_id`、`buyer_account`、`buyer_zone_at_order`、`price_fen`、`fee_fen`、`status`、`provider`、`provider_trade_no` 唯一、`pay_expire_ms`、`paid_ms`、`delivered_ms`、`version`;索引 `(buyer_player_id,status)`、`(status,pay_expire_ms)` |
| `trade_player_op_seq` | 每玩家每流 seq | PK `(player_id, stream)`,`next_seq`;事务内 `FOR UPDATE` |
| `trade_asset_op` | 资产指令 outbox | PK `(player_id, stream, seq)`;`op_kind`、`ref_id`、`payload`、`outcome`、`attempts`、`next_attempt_ms`、`lease_until_ms`、`result`;索引 `(outcome,next_attempt_ms)` |
| `trade_seller_ledger` | 卖家收入 | `order_id` 唯一;`amount_fen`、`status`、`available_at_ms` |
| `trade_payment_event` | 渠道回调去重与审计 | 唯一 `(provider, provider_event_id)`;只存摘要,不存原文敏感字段 |
| `trade_favorite` | 收藏 | PK `(player_id, listing_id)` |
| `trade_audit_log` | 商品/订单状态迁移流水(只追加) | `ref_kind`、`ref_id`、`from_status`、`to_status`、`actor`、`reason`、`zone_at_event`、`at_ms` |

- 所有状态迁移:`SELECT … FOR UPDATE` + `version` CAS,迁移与审计、outbox 同事务。
- 无 Redis 权威数据。浏览 v1 直接走索引查 MySQL(类目必选,分页页长上限 20);搜索只做名称/编号匹配,流量上来再加检索引擎。
- 不写入 zone 库(`db_task_zone_N`),不碰玩家主数据表。

## 9. 协议(`proto/trade/jubaozhai.proto`,`package trade;`)

协议文件:`proto/trade/jubaozhai.proto`(客户端)、`proto/trade/trade_admin.proto`(内部,**独立文件**,不进客户端生成清单)、`proto/trade/trade_table.proto`(表结构)。三者都是 `package trade;`,生成器按包名匹配到 `TradeNodeService`。消息名刻意不带 `Jubaozhai` 前缀,避免与客户端模型类同名。

**会话口径(P1 定稿,修订原"照 chat")**:照 `go/guild/internal/session` 的方法白名单式——无会话 metadata 视为内部调用放行;metadata 损坏 → `Unauthenticated`;带会话调用白名单外方法(`TradeAdmin/*`)→ `PermissionDenied`。客户端方法在逻辑层取不到会话 → in-band `kInvalidParameter`(不信请求体,请求里本来就没有 player_id)。原因:trade 同进程挂着内部服务,chat 式"坏头放行、不按方法准入"会让带会话的客户端请求打到内部方法。另有两道生成闸兜底:gate `IsClientMessageId` 不收非客户端服务的消息号(直接丢弃,客户端表现为超时);路由服对 `ClientProtocol=false` 回信封拒绝。

**P1 已声明**(每新增一个消息号都要重生成、先部署路由服再重编 gate,所以只声明本期要用的):

| 方法 | 说明 |
|---|---|
| `BrowseListings` | tab / section / category(必填)/ subcategory / search(≤64 字符,名称包含匹配,纯数字时同时按编号精确匹配)/ sort / page / page_size(上限 20)/ zone_filter(仅 global)/ favorites_only → 摘要列表、total_count、钳制后的 page / page_size、page_count、`market_scope`、`server_now_ms`(客户端算剩余时间用,不信本地钟)。section=AUCTION → `TradeFeatureDisabled` |
| `GetListingDetail` | 摘要 + 描述;不暴露卖家 player_id / 账号;zone scope 下别区商品对调用者"不存在"(`TradeListingNotFound`),卖家本人总能看自己的 |
| `SetFavorite` | 收藏需商品对调用者可见;取消幂等不查商品;每人收藏上限 `Market.MaxFavoritesPerPlayer`(**软上限**:同一玩家并发请求最多超出在途请求数,收藏不涉及资产,可接受) |
| `GetMyShelf` | 我上架的商品(任意状态,新的在前);P3 追加我的订单 |
| `TradeAdmin.SeedListing`(内部) | 只在 `Mode ∈ {dev,test}` 可用,否则 gRPC `PermissionDenied`;market_zone 由服务端按卖家查 home_zone 写入,与正式上架同路径;robot 经 gRPC 直连调用 |

**P3 起规划**(未声明):`CreateListing`(oneof 资产 + `price_fen` + `client_request_id`)、`CancelListing`、`CreateOrder`(→ 支付意图 `pay_mode` = MOCK / URL / QR)、`GetOrder`;内部 `MockConfirmPayment`(dev)、`ReconcileNow`、`FreezeListing`(GM 风控,须加运维口令)。login 侧 `LockPlayerForTrade` / `UnlockPlayerForTrade` / `TransferPlayer` 挂 LoginAdmin。

**tip 段 `trade_error`**(J-7,`//trade_error base=20000 width=1000`,已加入 `data/tip/Tip.xlsx`)。注意 Tip.xlsx A 列码名**不带 k**(模板自动加,Go 侧为 `table.TradeError_kTradeXxx`、C# 为 `trade_error.KTradeXxx`)。
- P1 已加:`TradeListingNotFound`、`TradeHomeZoneUnknown`、`TradeFavoriteLimitReached`、`TradeFeatureDisabled`(均非故障)。
- P1 复用通用码:`kInvalidParameter`(无会话、参数非法)、`kServiceUnavailable`(MySQL / data_service / 发号故障,fault)。
- P3 起按需再加(未加):`TradeListingNotOnSale` `TradeListingInPublicNotice` `TradeListingLocked` `TradeListingBusy` `TradeCannotBuyOwn` `TradeZoneMismatch` `TradeAssetNotTradable` `TradeAssetInsufficient` `TradeAssetBusy` `TradePetActive` `TradeCharacterLocked` `TradeCharacterInGuild` `TradeAccountSlotsFull` `TradePriceOutOfRange` `TradeTooManyListings` `TradeOrderNotFound` `TradeOrderExpired` `TradeBuyerNotEligible` `TradeMergeInProgress` `TradePaymentUnavailable`(fault)。

## 10. 服务骨架与接线(照 `go/chat`)

- `go/trade`:`trade.go`、`etc/trade.yaml`(gRPC **50800**、metrics **9230**——9220 已被 guild 占用,不写 go-zero `Etcd.Key`,Timeout ≤ 4000)、`internal/{config,constants,session,server,svc,logic,data,payment,reconcile}`。
- 拦截器链 `grpcstats → killswitch → session → serverbase`;注册用 `go/shared/noderegistry`,失租 `ReallocateNewID`。
- **建表(D-14)**:服务二进制 `-migrate` flag 只连 MySQL 跑 `schemamigrate.Up` 后按退出码退出(0 成功 / 1 失败 / 3 锁忙 / 4 需人工);`Schema.AutoMigrate` 不写 = true(dev,启动期迁移,锁保护多实例安全);K8s 非 dev 档固定 false,由 `trade-migrate` Job(同镜像同 ConfigMap,Deployment 之前 delete + apply,等 Complete)执行,启动路径只跑只读 plan,有待执行语句或需人工项就拒启动。建库只登记 `deploy/mysql-init/00_init_zone_dbs.sql`;已初始化的卷 / PVC 手工补建(`deploy/k8s/README.md`)。`go/schemamigrate` 是独立 module(go 1.26.5,require proto2mysql ≥ v0.1.1),镜像构建需 COPY 该目录。
- **`Mode: dev` 必须显式写**:go-zero 默认 `pro`,不写则本地 SeedListing 不可用;K8s 非 dev 档锁 `pro`。
- **`DataServiceRpc.Etcd.Key: dataservice.rpc` 是允许的**:D-13 禁的是"指向全局服务的 RpcClient Key",data_service 按 zone 部署不在其列(`xuanming-port-decisions-20260910.md` D-13 原文);契约 §7 "不声明任何 RpcClient 的 Etcd.Key" 措辞过宽。本地 `-Zone N` 会把它改写成 `.zN`,连本 zone 的 data_service;映射存储是全局的,结果等价。
- 登记:`tools/proto_generator/protogen/etc/proto_gen.yaml` 的 `domain_meta.trade`;`go_services.ps1`、`start_game.ps1`(可选服务)、`go_svc_image.ps1`、`deploy/k8s/Dockerfile.go-svc`(COPY schemamigrate)、`.github/workflows/go-modules-ci.yml`、`k8s_deploy.ps1`(目录、ConfigMap、data-service BootstrapTags、migrate Job)、`deploy/k8s/manifests/go-svc/trade.yaml`、`deploy/mysql-init/00_init_zone_dbs.sql`、data_service 号段 BootstrapTags。
- **显式改列授权(D-14)**:`trade -f <yaml> -migrate -allow-modify` 才允许按 proto 修改已存在列的类型;单独 `-allow-modify` 在读取配置前拒绝,常驻启动与默认迁移均不自动改列。默认 Job 不带该参数,类型漂移以 4 退出,经人工审查后再显式执行。
- **D-14 第 9 条上线清单**:`trade_listing.market_zone` 带 home_zone 语义 → `tools/merge_zone` 需改写步骤(P1 同批落);audit auditor、data_consistency_check、TiDB BR 按库恢复清单列为 **P3 上线前**必做项(P1 只有 dev 种子数据,无玩家资产)。
- 客户端可达只承诺路由服模式;K8s 路由服 manifest 与 POD_IP 通告已补齐,但尚未在 K8s 上以路由模式跑通 battle-smoke,默认仍为 0。
  (2026-09-29 更正:K8s 默认已为 `"1"`(D75),trade 按设计可达;battle-smoke 仍未跑,改为事后补验,补验前不宣称 K8s 上已验证可达。以 `"0"` 回退运行的环境仍不可达。)
- 指标(低基数,不带 player_id):商品/订单状态迁移计数(按 kind、to_status)、渠道回调结果、outbox 积压 gauge、交付延迟直方图、reconcile 轮次耗时。

## 11. 客户端改动

- 新增 `Assets/Scripts/Game/Jubaozhai/JubaozhaiClient.cs`,照 `GuildClient` 的会话代次隔离(换角/断线丢弃旧回包)。与帮会的两处差异(P1 定稿):
  - **同时只发一个请求、只补发最新查询**:在途时筛选变化只记脏,回包后补发一次;回包按发送时的 `QueryVersion` 注入,过期自动丢。原因是 `GameClient` 按消息号先进先出匹配 gRPC 回包,同一消息号不能并发。
  - **只有 `"rpc timeout"` 才进入需重连隔离**:信封错误(路由服在 trade 不在线或被 killswitch 关停时回的就是这种)、解析失败、未连接等只写文案不隔离。否则 trade 作为可选服务缺席时,聚宝斋会一直不可用直到断线重连。
- `JubaozhaiState` 保留"整份快照 + 本地筛选分页"的离线演示模式,新增**服务端分页模式**(`EnterServerPaging` / `ApplyServerPage` / `QueryChanged` 等):筛选、翻页、收藏、详情都只发事件由 client 请求,`GetPage` 原样返回服务端注入页。不动已拼好的皮肤布局。
- 侧栏「货架」走 `GetMyShelf`,不按当前类目 / 页签过滤;「拍卖」收到 `TradeFeatureDisabled` 显示空页与"拍卖尚未开放"。
- 客户端生成清单额外纳入 `generated/code/proto/tip/common_error_tip.proto`(映射 `kInvalidParameter` / `kServiceUnavailable` 文案,不手写数字)。
- 价格:`Price = price_fen / 100m`;剩余时间用 `server_now_ms` 校正。
- 「购买」仅在 `ON_SALE` 且非自己的商品时可点:`CreateOrder` → 支付弹窗(dev 显示"模拟支付",正式显示二维码/跳转)→ 轮询 `GetOrder`。
- 「货架」接 `GetMyShelf`;**上架界面缺设计稿**(J-O5),P3 前需要补。
- 「联系卖家」依赖 chat 私聊,保持未开放;「估价」「规则」文案后续由服务端配置下发。
- 生成脚本 `tools/gen_proto.ps1` / `gen_messageids.ps1` 需显式 `-ProtoRoot E:\work\xuanming-server-mmo`(默认值按旧子模块布局,会解析错)。

## 12. 安全与合规上线闸

1. **GM 客户端消息鉴权**(P0-a):~~`GmAddCurrency` / `GmGrantPet` / `GmSetPlayerLevel` 等要么关闭、要么鉴权。不收口 = 刷出资产直接卖人民币。~~
   **2026-09-18 已落码(未编译)**:选的是"关掉客户端面",不是"给客户端面加鉴权"。
   实际暴露的是 **6 条**(37 `GmAddCurrency` / 49 `GmDeductCurrency` / 94 `GmBlockCurrency` /
   95 `GmUnblockCurrency` / 175 `GmSetPlayerLevel` / 187 `GmGrantPet`);
   gate 按消息号闸(`cpp/nodes/gate/gate_gm_client_messages.h` 清单 +
   `gate_security.h::ClassifyGmClientMessage`),scene 再拦一次
   (`handler/rpc/player/player_gm_guard.h`,防绕开 gate 直连)。
   判据是运行模式 `GATE_RUN_MODE` / `SCENE_RUN_MODE`,**默认(未设置)= prod = 拒绝**,
   部署链从不注入这两个变量;本机启动器兜底 dev,robot 冒烟不受影响。
   细节与运维须知见 `cpp/nodes/gate/SECURITY.md` §3。
   *残留*:这是开关不是鉴权 —— 线上没有"带身份的 GM 改数据"通道,真要改走
   `scene_admin` / `data_service` 的签名运维面;若将来确实需要客户端面的 GM,
   要给这些请求加 `gm_envelope` 字段走 `VerifyGmRequestFromEnv`(动 proto,要 regen)。
2. **账号数据持久化**(P0-b):角色交易的前置,见 §6.3。
3. **资产通道的两道自闸**(P2 落码后新增;**解除前任何共享 / 预发环境不得开启资产操作**):
   - **J-A4 签名未覆盖 `item_uuids` / `pet_id`**(见 §6.4):scene gRPC 是明文凭据,集群内可嗅可改载荷。
     不补完 canonical 就开 TRADE_* 流,等于把"按 guid 扣任意装备"开放给集群内任意进程。
   - **B4c 对账闸门未落地**:目前没有"Go 认为已发放 vs scene 账本实际记了什么"的对账与告警,
     一旦两边不一致,只能等玩家来报。
   `go/trade` 的 `AssetOp.Enabled` **默认 false** 就是给这两条兜底(fail-closed);
   把本地配置抄进任何非本机环境之前,先确认这两项都已解除。
4. **实名 / 防沉迷 / 未成年人消费限制**:`BuyerEligibility` 接入真实实现。
5. **经营与支付合规**:人民币虚拟物品交易平台涉及的资质、运营主体与支付签约由业务/法务确认,工程侧不能替代。
6. **风控**:同账号自买自卖拒绝;价格上下限;单账号在售数上限;新获得物品上架冷却(需要 `ItemEntry` 获得时间,P3 后);异常高频下单限速(gate 每消息号 3 次/秒是第一道)。
7. **回档交互**(2026-11-05 按现状改写):GM 单人 / 整区 / 全服回档 RPC 已于 2026-10-30 删除,回档只剩 PITR([PITR 手册](../ops/mysql-backup-pitr-runbook.md) §4.0):默认整个地域的逻辑库回到同一时刻 T,T 之后的写全部丢弃。所以不存在「只回档一名玩家、按交易流水把合法转移排除在外」的路径,原先引用的 `single_player_rollback` 排除规则不适用。
   - 交易库 `mmorpg_trade` 与玩家存档回到同一个 T,合同、订单与托管资产在 T 时刻一致,不需要按流水排除。
   - 只回档其中一个库时两边对不上:T 之后已提交的合同与资产指令按 PITR 手册 §4.1a 的表人工核账,挂起的腿不能按旧主继续发资产。
   - 托管与交付经资产通道写进 scene 的 transaction_log,`correlation_id` 是资产指令的 `op_id`(§6.4 J-A3:同一件商品的托管和回退是两条 op,按它各自定位 outbox 里唯一的一行),listing / order 记在 outbox 的 `ref_kind` / `ref_id`。这些流水是人工核账的线索,不是回档的排除依据。

## 13. 分期与验收

| 期 | 内容 | 依赖 | 验收(由 Codex 执行) |
|---|---|---|---|
| **P0-a** | GM 客户端消息鉴权收口 —— **2026-09-18 已落码,未编译** | — | `GATE_RUN_MODE=prod` 重拉 gate 后,robot 发 37/175/187 收到 `kFeatureUnavailable` tip 且 scene 无对应业务日志;`GATE_RUN_MODE=dev`(启动器默认)下 `attribute-smoke` / `pet-smoke` / `currency-crash-snapshot` 照常通过;`gate_security_test` 三个新用例绿 |
| **P0-b** | 账号角色列表持久化(login) | — | 清空 Redis 后角色列表不丢;并发建角/删除不互相覆盖 |
| **P1** | proto(客户端 4 方法 + 内部 SeedListing + 表结构)+ tip 段 + `go/schemamigrate`(D-14 抽取 + 修缺陷)+ `go/trade`(浏览/详情/收藏/货架/dev 种子)+ `-migrate` 与 K8s Job + 登记 + `tools/merge_zone` 改写步骤 + 客户端服务端分页接线 + robot `trade-smoke` | — | schemamigrate 单测含"后加表被建出";trade 单测含"带会话调 TradeAdmin 被拒";`TestNoHandWrittenTipCodes` 通过;robot `trade-smoke` 在 `zone` 与 `global` 各跑一次输出 `TRADE_SMOKE_OK`;客户端按 U 打开拉到服务端种子商品,收藏重进角色后仍在 |
| **P2** | 通用资产通道(与帮会 B4a/B4b 合并实现)—— **2026-09-18/19 已落码,未编译**:C++ 账本组件 `PlayerAssetOpLedgerComp` + 持久化;`AssetDebit/AssetAbortDebit/AssetCredit`;`go/shared/assetop` + `go/shared/scenenode`;`trade_player_op_seq` / `trade_asset_op` 两表;Bag 按 guid 全或无;Currency txType;Pet 移出/预设回填 | P1 proto | 单测覆盖:重复 seq 返回同一结局、Abort 占位、战斗中/跨区冻结返回 RETRY、全或无不部分扣、canonical 串 C++ 与 Go 逐字一致。**两条启用前置:J-A4 签名扩 `;u=;p=`(§6.4)、B4c 对账闸门;在那之前 `AssetOp.Enabled` 保持 false,共享/预发环境不得开启** |
| **P3** | Item/Pet 表新列(`tradable` / `market_category` / `market_subcategory`)+ 游戏币/装备道具/宠物 端到端:上架→公示→寄售→下单→mock 支付→交付→卖家入账→冷静期;下架/过期回退;reconcile | P2 | robot:卖家在 zone1、买家在 zone2 完成三类交易;买家离线付款后上线自动到账;重复支付回调不重复交付;kill trade 进程中途重启后状态收敛 |
| **P4** | 角色交易:login 锁/过户/踢人原因;scene 交易锁 + 快照 | P0-b、P3 | 公示期内卖家无法登录该角色;过户后买家账号可进入、卖家列表消失且卖家 EnterGame 不能自愈写回 |
| **P5** | 真实支付渠道、实名防沉迷、提现、对账 | 商务/合规 | 渠道沙箱全流程;对账差异为 0 |
| **P6** | 拍卖(竞价)、联系卖家、估价 | chat 私聊 | — |

粗估(不含 P5 商务周期):P1 ≈ 10–12 人日;P2+P3 ≈ 35–45 人日;P4 ≈ 8–10 人日。

- **robot `trade-smoke` 固定账号 `robot_9401`(卖家 A,zone_a)/ `robot_9402`(买家 B,zone_a)/ `robot_9403`(C,zone_b)**,三者须首次在对应 zone 建角。刻意不用 93xx:`team-system.md` §I.5 已把 9301–9304 预留给 team-smoke,且 9303 须在 zone_a 建角,与本场景冲突(归属区在首次建角时定死)。
- P1 验收的消息号:`196 BrowseListings / 197 GetListingDetail / 198 SetFavorite / 199 TradeAdmin.SeedListing / 200 GetMyShelf`(`proto/message_id.txt`);MessageLimiter 已给 196/197/200 配 10 次/秒、198 配 5 次/秒,199 是内部方法不配。

## 14. 与其他文档的关系

- 取代 `xuanming-port-feasibility-20260902.md` 中 auction(24 人日)条目的实现路线;其 §8.1 D1 发物入口与本文 §6.1 账本合并实现(J-5),落码时两边文档同步改。
- `xuanming-port-decisions-20260910.md:144` 计划中的 `trade` tip 段由本文 J-7 落实。
- `server_merge_design.md` §3.1a / §6.4 与 `docs/ops/merge-zone-runbook.md` 已追加步骤 3b、撤销 3b' 与 `verify:trade_listing`(2026-09-14)。
- 建表方式以 `xuanming-port-decisions-20260910.md` D-14 为准;trade 是首个建表服务,`go/schemamigrate` 与 `trade-migrate` Job 随本服务同批落地。

## 15. 2026-10-16 续批实施口径（进行中，未整体验收）

- 本轮范围是官方商品、玩家双方确认交易、固定价寄售、同质材料限价订单簿和限时竞拍；市场合同与材料目录使用新增的 `commerce` 协议。旧 P1 播种商品没有真实资产托管，不能把旧 `listing_id` 当成新托管合同购买。角色过户、真实渠道签约与提现不在本批实现范围。
- 全球同服指所有区域共用同一份 SQL 合同、订单簿、幂等请求和资产操作账。区域只决定网络入口与战斗落点，不生成互相独立的可交易库存或货币账户。TiDB 的实际拓扑、故障切换与容量必须另行实测；本批没有两千万在线的压测证据。
- 资产操作的成功响应、Redis 玩家快照中的结局、SQL 已提交的结局是不同事实。交易和邮件只有取得 `require_sql_commit` 且应答明确标记 `sql_committed` 的账本证据后才能结束到账；旧版服务忽略新增请求字段的应答不能充当证据。证据丢失时保留同一待办，重新查询及强制保存，不推测成功或失败。
- 已有装备与宠物通过原实例 protobuf 字节托管，保留 GUID、成长与未知字段；签名覆盖完整字节。托管原件与业务状态在交易 SQL 事务提交后才可发送收据 ACK；ACK 待办还须确认玩家 SQL 账本已经移除收据。未确认原件保留整个 1024 操作窗口，调用方未决跨度不超过 512，单个实例 16 KiB、单笔快照 64 KiB、单玩家收据总量 256 KiB。达到上限即等待或拒绝，不能截断原件。
- SQL 中的 APPLIED 位图不能代替 SQL 托管收据：到账确认先丢弃在线 RPC 原件，只有 SQL receipt 的 seq、correlation、稳定 debit 请求摘要和全部实例身份一致才接收原件；缺收据时继续等待。不能因为 RPC 碰巧携带快照就把它算成已提交快照。
- 玩家交易采用卖方发起并托管、买方核对卖方可信摘要后确认并托管、卖方核对买方可信摘要后最终确认的顺序。双方原始选择条款不可变；卖方不得在买方托管确认前提前完成最终确认。取消与确认超时通过原件退款待办恢复。部分应用属于人工核查，不能自动按完整预检快照给对侧入账或全量退款。
- 客户端在发送写请求前保存玩家、消息号和原始 protobuf 请求。超时后沿用原请求号和载荷恢复，换角不重放另一玩家的记录，故障或缺少合同的空成功应答不清除待确认记录；故障分类来自 `Tip.xlsx` 导表结果。客户端不自行扣钱、发货或声称已付款。
- 本机恢复记录按网关来源与玩家隔离，落盘完成后才发请求；回执按发送时原记录比较清理，旧进程或旧回包不能清除后续请求。合同缓存与给 UI 的响应事件共同保持版本不倒退，避免旧回包重新展示已经终结的确认按钮。
- **仍未关闭的全局强一致边界**：SQL 行版本围栏与 SQL 提交证据不能单独证明玩家归属切换、Redis 回退和冷加载之间的完整屏障；旧持有者未提交保存与新持有者冷加载的竞态须与玩家围栏主线一起关闭。两张玩家表也不是同一事务，本批资产与账本必须保持在同一张 `player_database` 记录。此边界未验证前，不能把本批标为全球强一致上线完成。
- 当前代码、生成、单测、真实 TiDB 测试与两版端到端验收分别记录；Java 版对应 `0.1.0-SNAPSHOT`，由 `PARITY.md` 登记实际状态。不得把接口已生成、某一版已编译或测试替身保存成功代替两版真实 SQL 与故障恢复验收。

### 15.1 交易和官方商城的实际落地边界

- Go `internal/commerce` 与 Java `game-trade/commerce` 已装配到正式客户端入口，共 24 个交易方法（旧目录 4 个、新合同 20 个）。材料目录只来自服务器白名单和可信名称；默认空白名单拒绝交易。Go 材料订单簿使用 `Commerce.CurrencyTypes` 首项作为结算币种，Java 每种材料目录也只能声明一个币种；深度查询同时过滤币种。不能把同材料不同货币聚合成一个价格。
- P2P 合同 15 分钟确认超时，卖方先托管、买方确认并托管、卖方最后确认。双方看到的材料名称、数量与实例摘要由服务器产生，不接受客户端提供实例原件。寄售购买把 `client_request_id` 保存在该合同的扣款腿角色中：同请求被拒后重试不能重新分配 seq；显式新请求才允许再买。同一寄售最多 32 次扣款尝试，超过后由卖方撤单、重新上架。
- 同质材料撮合只读取已托管 SQL 订单（SQL 里按 `on_book=1` 过滤，盘口深度同一条件，见 §15.9），按最优价格、创建时间、ID 决胜，使用 maker 价成交并退买方差价。成交意图与双边预留数量在同一事务提交；部分撤单只退未成交数量，所有子成交交付结束前仍显示撤单处理中。竞拍只接受已冻结资金的有效出价，超价者通过原币待办退回；到期复核采用数据库时间。
- 卖家撤单的窗口包含托管阶段（寄售、材料订单、限时拍卖；2026-10-30，混版本持久字段保护第 6 批）。原因：托管扣款可能一直回 RETRY 而不记终态，例如按数量扣出遇到带更新版本字段的物品（`mixed-version-persist-field-protection.md` §4.4）；材料订单的托管腿又没有截止，不许撤就没有任何出口。撤单与上架后撤单同一写法：`ExpireCommerceEscrowDebitsTx` 把在途托管扣款截止到当下，资产通道改发 Abort。没扣到的按取消收口；Abort 输给已落地的扣款时，照常进入挂出阶段后立刻按撤单整单退回，不进盘口撮合。用例 `TestTiDBSellerCancelsMarketOrderStillInEscrow`（真实 TiDB，未运行）。Java 版待做。
- 所有材料堆叠数量合计最多 4096，不能只逐模板检查。已有装备、宝宝从扣出转换到入账时，先逐个核对 GUID、原件数量和宠物身份，再清除 `item_uuids/pet_id` 扣出选择字段，只携带完整实例原件；未知字段与成长字节保留。部分应用进入人工状态，不继续对侧，也不按全量预检快照自动退款。
- 官方商品沿用钉版 mall 的 PMS/OMS 存储与库存锁，不把完整电商平台塞进游戏服。`commerce_sku_grant` 是受信 SKU→游戏材料、数量、名称映射；下单事务把它冻结到 `commerce_order_grant`，事后修改目录不改变已有订单。缺少有效映射直接回滚下单。正式支付适配器使用 payment 角色报告真实渠道回执；gateway、payment、delivery 三把签名密钥独立，客户端不能提交支付结果。
- 支付落库后创建持久 `commerce_delivery_intent`；多 trade worker 可以重复领取同意图，但按官方订单 UUID 只创建一个游戏交付合同、一个原 seq。只在资产 SQL 提交证明已记录后回报 DELIVERED；不可重试的拒绝产生 REFUND_PENDING 与独立退款意图，部分应用进入 MANUAL。支付渠道只有报告真实退款成功后才标 REFUNDED。取消订单后的迟到付款也保留支付事实并进入退款，不发游戏资产。回执丢失由原合同恢复，不能用新 seq 重发。
- 官方订单 UUID 是客户端始终可查询的稳定编号。`ListCommerceOrders` 先用 `mall:<UUID>` 游标列出商城持久历史，再切换游戏合同数字游标，游戏查询排除官方内部合同，未付款订单也能在重启后找回。旧 P1 商品和真实托管合同保持独立入口。
- Go 本地 `trade.yaml` 的 Mall.URL 留空关闭桥接，启用时分别配置 GatewaySecretEnv/DeliverySecretEnv；Java 使用 `game.trade.mall.*`，默认 disabled。生产部署必须显式 TiDB 地址、global 市场、启用资产通道，并镜像完整恢复、清理及白名单参数；本地 dev 旧目录配置不构成全球交易验收。实际密钥由环境/secretRef 注入，不进仓库。
- 商城新安装用完整 schema（Java 为 V1 安装脚本）；V2 只给旧 v1 库增加交付映射、回执、退款表和待办索引，不要在已包含新结构的空库安装后重复执行。升级前已有而未冻结游戏发物映射的旧订单必须人工核实，禁止按当前 SKU 映射猜补历史订单。

- **上线门禁（负责人 2026-10-16 拍板）**：合同 / 官方商城到账（ORDER 终结）强制要求资产 SQL 提交证据，证据只在 go/db `Fencing.Mode=dual` 时发布。上线前必须先把交易会投递到的每个 zone 的 go/db 按 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) §8.3 切到 `dual` 并走完 §8.7 的上线顺序；在此之前 `Commerce` 白名单保持为空、`Mall.URL` 留空（配了二者而 `AssetOp.RequireSQLCommit` 关闭时 trade 拒启）。任一 zone 回退 `legacy` 之前先关该 zone 的交易与商城入口。

### 15.2 2026-10-16 本地验证证据

- Go `go/trade` 全包测试、`go build ./...`、`go vet ./...` 通过：`build/commerce-contracts-20261015/trade-all-tests-tidb-v8.log`、`trade-build-v8.log`、`trade-vet-v8.log`。真实 TiDB 用例在每例新建随机库后清理，覆盖寄售并发唯一买家/恢复同 seq、P2P 双方摘要与取消、被拒购买请求幂等、订单簿价格时间和部分撤单、竞价退款与迟到出价、partial 人工隔离、Redis durable 不能代替 SQL 证明、商城丢回执恢复。资产终局由交易测试夹具注入，验证的是交易 SQL 状态机，不冒充 scene 多进程端到端。
- 两版边界缺陷均先红后绿：`trade-boundaries-{red,green}.log`、Java `commerce-boundaries-{red,green}.log`；4097 混合材料和 credit 混带 debit 字段的旧行为已稳定复现。Java 最后交易回归 27 项、0 失败、0 跳过（含 4 项真 TiDB），版本 `0.1.0-SNAPSHOT`。
- Java 独立源码快照 `build/commerce-finish-java-20261016/source-v5` 的 16 模块反应堆成功；交易 25、商城 18、scene 39、Gate 24 项通过，商城包含 7 项真 TiDB。交易随后修边界并补跑上述 27 项。真实玩家 SQL 回归 `commerce-regression-v3.log` 的 `PlayerStoreSqlTest` 42 项通过，包含完整物品/宝宝与收据同记录提交、新读取者恢复、旧 epoch 写入拒绝、收据删除持久化。
- C++/Go 侧独立商城 `java/mall_game` 的真实 TiDB 运行 24 项、0 失败、0 跳过：`build/commerce-contracts-20261015/mall-game-tidb-v4.log`。此前 fixture DDL 的请求超时、测试漂移构造与清理失败日志保留；安装连接单独使用 30 秒预算，生产请求池仍为 6 秒，未跳过断言。唯一已知自有残留随机库已精确清理，见 `mall-owned-fixture-cleanup.log`。
- 这些证据不覆盖真实渠道支付/退款沙箱、多区域完整 Gate→scene→SQL 故障切换、全局玩家归属围栏关闭或两千万在线压测。上线状态仍受 §15 的全局强一致边界约束。

### 15.3 2026-10-20 修复：合同推进单行隔离、放弃与人工终结的扣款腿

对应第一轮审计（[data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.5）的 go-asset-services#0、#1，门控 `AssetOp.Enabled`。**状态：已落码，未编译、未测试，待 Codex 验证。** 实现在 `go/trade/internal/commerce/tick_isolation.go`、`engine.go`（`advance` / `Tick`）、`auction.go`，以及 `go/trade/internal/data/commerce_repo.go`。

**#0：一条合同出错，全服交易停摆。**

- 原问题：`Engine.Tick` 遇到任何一行出错就整轮返回。失败行回滚后 `updated_ms` 不变，下一轮仍排在 `Due` 队头。最容易触发的是对方离线、未决 credit 攒满 16 条后的 `ErrTooManyPending`。持有中的竞价（`bid_held`）在拍品结算前也不改 `updated_ms`；有出价的在拍拍品数一到 `Due` 上限，队头就被它们占满。
- `updated_ms` 兼作调度键：`Save` 写当前时刻（立即到期），新增的 `CommerceRepo.Defer` 写将来时刻（推迟）；`Due` 只取 `updated_ms <= now` 的行。`updated_ms` 不进任何客户端视图，不动对外契约。`acceptBid` 写 `updated_ms` 改用引擎时钟，截止判定仍用库时钟。
- 每行有子预算（原为固定 5s，2026-10-22 改为从父截止时刻推出，见本节末「2026-10-22 修复」），失败按类处理。指标 `trade_commerce_tick_row_failures_total{class}`（2026-10-22 起另有 `role_conflict`、`conflict` 两类，见本节末）：
  - `retry`：存储超时、死锁重试用尽等。按 10s ±20% 推迟。
  - `too_many_pending`：按 60s ±20% 推迟。要等那位玩家的未决指令终结，日志只记 INFO。
  - `corrupt`：合同自身持久数据坏了，包括 workflow 解不开、已发出的腿缺 outbox 行、腿状态非法、托管快照解不开、`plan` 判错、已扣出的托管转不动。这类失败隔离为 `Manual`，移出 `Due`，计 `trade_commerce_quarantined_total`。
- 推迟与隔离都以 `Due` 读到的 `version` 为条件，锁序与 `Save` 相同（主键点锁合同行，再带版本条件点改）。推迟不改 `version`，所以并发的 `Save` 不会被挡住：它在同一行锁上排在后面，照常提交并把 `updated_ms` 改回现在，推进优先于推迟。行在 `Due` 之后被推进过，两者都落空，不会用旧观察覆盖新行。
- 隔离不发出、不退回任何资产（2026-10-22 起同时截止合同名下在途的托管扣款，见本节末）。workflow 解得开时 `stage` 改成 `manual`，与 partial 隔离同形，玩家操作与撮合都不会再把它拉回推进；解不开时只改状态列（`SaveStatus`），坏字节原样留作取证。op 行不标业务消费，证据不被清理。
- 其余循环：
  - `bid_held`：拍品未结算时推迟到拍品截止，至少 30s、至多 1h。被超价时接受新出价的那次 `Save` 会把它拉回当前时刻。
  - `acceptBid`：`bid_ready` 的出价还没被拍品接受，拍品不引用它。拍品行缺失或解不开时按不合格原币退款，不再每轮报错。原最高价出价读不到或不在 `bid_held` 时，按暂时失败推迟；拍品截止后这条出价不再合格，自动退款。
  - 撮合失败：`advance` 已提交，下一轮重撮（2026-10-22 起不再走推迟 / 隔离，单独计数；坏对手单移出盘口，见本节末）。
  - 收据 ACK：按 `op_id` 翻页（游标归驱动 `Tick` 的单个 goroutine）。payload 解不开的行跳过并打 ERROR，不清 ACK 待办。依赖 RPC/SQL 错误与 ACK 清理 SQL 错误继续处理同页后续行，页末汇总交现有调用方记录；正常离线的 `done=false` 不当作故障。
  - 商城交付意图：单条失败不挡同批其余意图，错误汇总返回。

**#1：放弃或人工终结的托管扣款腿，合同永远读不到。**

- 原问题：`ResolveManually` 与 `Abandon` 刻意不置 `durable`，用来区分「scene 答过」与「人判定 / 自动放弃」。合同推进却只认 `durable=1`，这两种终局永远读不到。合同停在 pay / escrow，卖家托管的资产被扣住；买家点击购买后下线，就能把卖家资产扣作人质。
- 修复：`CommerceRepo.LegResult` 改回 `data.LegOutcome`，终局只看 `status`，不看 `durable` 与来源。来源（scene / manual / abandoned）只进 INFO 日志。
- 补偿语义：
  - `ABANDONED`、`REJECTED`、`ABORTED` 一律按「没扣到」处理，走与 scene 判拒同一条 `plan` 路径：退回另一方已托管的资产，寄售重新开放，竞价冻结失败即结案。
  - `APPLIED_PARTIAL` 照旧转人工。
  - 人工判 `APPLIED` 按已扣到处理。实例扣款被人工判 `APPLIED` 时没有托管原件：`ResolveManually` 不写 `escrow_snapshot`，自动路径缺原件根本不终结。`advance` 在记下结局时即用 `Transfer` 核对已扣出的托管转得动，转不动就隔离。这样不会先上架，等买家付了款才在发货时撞错。
- 不误退：`ABANDONED` 只出现在 TRADE_DEBIT。[landing-recovery-sync-digest.md](landing-recovery-sync-digest.md) §6.3 的六条证据保证 scene 不可能再收到这条 seq，即「确实没扣」。人工终结的结论只认玩家已提交的账本(`go/shared/assetop/manual_evidence.go` 的 `JudgeManual`,trade 与 guild 共用一份判据),scene 流水 correlation_id 只是线索;终结用带读证据时租约与发送时刻的条件写,并留痕(手册 `trade-commerce-quarantine-runbook.md` §4.2)。
- 补偿只一次：
  - 腿的 `Outcome` 写进合同 workflow，与合同版本 CAS 同一事务提交；`Outcome != 0` 的腿不再读。
  - 补偿腿按角色名 `addLeg` 只建一次。
  - 多副本推进同一合同时，在合同行锁上串行。
- 与放弃 / 终结入口的并发：`Finalize`、`ResolveManually`、`Abandon` 都只主键点锁 op 行，从不碰合同行。合同推进先锁合同行，再普通读 op 行。两边不成环。三个写者都以 `status = PENDING` 为 CAS 条件，一行只终结一次，终态之后不再变化：合同这一轮读到 PENDING 就等下一轮，下一轮必然读到终态。
- 残余：
  - 买家下单后离线不归时，卖家托管仍要被占到自动放弃（`AssetOp.AbandonAfterDays`，默认 180 天、最小 30 天）或人工终结。这条现在有界，但不短。缩短它要另立设计，例如「已过截止的扣款腿凭已落盘账本证据提前放弃」。I7 的窄例外要重新论证，不在本修复范围。
  - 旧 P1 商品（`ref_kind=LISTING`）的放弃对侧账仍是 `Abandon` 里的 P3 TODO。

**回归测试**（真库用例每例新建随机库）：

- 单测：
  - `go/trade/internal/commerce/tick_isolation_test.go`：失败分类、退避有界且带抖动。
  - `go/trade/internal/data/commerce_leg_outcome_test.go`：终局判定不看来源；枚举里每个终态都必须被合同认作终局。
- 真库：`go/trade/internal/commerce/tick_isolation_db_test.go`。
  - 门控：`COMMERCE_TEST_MYSQL_DSN` + `COMMERCE_REQUIRE_MYSQL=1`，或沿用 `COMMERCE_TEST_DSN` + `COMMERCE_REQUIRE_TIDB=1`。
  - 覆盖：
    - 队头 `ErrTooManyPending` 不挡其余合同，被推迟且推迟期内不重试，积压消化后照常推进；
    - 坏数据隔离且不再被 `Due` 取到；
    - 买家扣款被放弃后寄售重新开放，到期只退卖家一次；
    - 人工终结的腿推进合同；
    - 人工判 `APPLIED` 的实例扣款立即隔离；
    - 拍品缺失的 `bid_ready` 出价只退一次；
    - 持有竞价不占队头；
    - 收据 ACK 翻页越过卡住的表头，并跳过坏 payload。

**2026-10-22 修复（按复核意见）**。**状态：已落码，未编译、未测试，待 Codex 验证。** 人工处理见 [交易合同隔离与角色冲突处置手册](../ops/trade-commerce-quarantine-runbook.md)，告警见 `deploy/k8s/asset-channel-alerts.yaml` 的 `trade-commerce.contracts` 组。

- **单行子预算从父截止时刻推出（必须修）。** 原问题：7d9208237 起恢复循环每轮只给合同推进 3s（`svc.AssetChannel.Start`），而单行子预算固定 5s，两者同时到期。队头那一行卡住时（锁等待、目录事务持锁、依赖 RPC 不回包），`Tick` 只能直接返回，来不及推迟它；下一秒它又排在 `Due` 队头，再吃满 3s，全服合同推进与收据 ACK 跟着停住。现在由 `rowBudgetFor` 计算：先给本轮收据 ACK 留 0.5s（`tickAckReserve`），再给这一行失败后的推迟 / 隔离留 0.3s（`tickDeferBudget`，`deferCommerce` 自带这段超时），剩下的一行最多用一半，上限 5s；不足 0.2s 时不再开新行。按 3s 计算，第一行最多 1.1s，卡住并推迟之后父 ctx 还剩至少 1.6s，下一行最多 0.4s。卡住的行一定先撞上自己的子预算，父 ctx 还来得及把它推迟。没轮到的行仍到期，计入 `trade_commerce_tick_truncated_total`，不再每秒报一次 deadline exceeded。例外：别的事务一直持有这条合同行的行锁时，推迟也拿不到这把锁，这一行每轮仍在队头，但每轮只花它的子预算加推迟预算，同一轮其余合同照常推进。
- **失败分类新增两类。** 以前 `ErrConflict` 一律算 `retry`，现在用包装区分（包装里仍带着 `ErrConflict`，客户端 tip 码不变）：
  - `conflict`（`staleObservation`）：锁内复核发现 Due 或预读之后合同、拍品或对手方已被别的写者推进，是多副本的正常现象。不推迟，下一轮立即重试，日志只记 INFO。以前推迟 10s，临近截止时会把一条已冻结的有效高价出价拖过截止，变成退款。来源有四处：`advance` 的参与者变化、`acceptBid` 的拍品买家变化、撮合的最优对手变化、人民币寄售 `save` 的版本变化。
  - `role_conflict`（`roleConflict` / `asRoleConflict`）：参与角色已换主（`owner_revision` 只增不减）、被另一条挂单冻结（`trade_listing_id`），或目录行不合格。这是长期状态，按 5min ±20% 推迟，单独计数并告警。人工出口见手册 §4.3：冻结解除后最多约 5 分钟自动恢复；已换主的合同需要产品或运营决定托管资产归谁，目前没有自动出口。
- **隔离截止在途托管扣款。** 原问题：隔离只改合同行，在途的托管扣款照常投递，隔离之后还会把别的玩家的资产扣进人工单。现在 `quarantineLockedTx`（合同自身隔离与坏对手单共用）与 `APPLIED_PARTIAL` 转人工都先调用 `ExpireCommerceEscrowDebitsTx`，与玩家取消同一写法、同一锁序（合同行 → 按 op_id 升序点锁 op 行）。截止之后资产通道用同一个 seq 改发 Abort：已扣到的照常回 APPLIED 留证据，没扣到的不再扣。隔离之前已排定的发放腿照常投递。
- **拍品不可接受出价时退款。** `acceptBid` 中拍品自己的原归属身份校验失败（缺身份的存量数据、原主已换），与拍品缺失、workflow 解不开一样按不合格原币退款。只有出价自身的数据坏了，这条出价才算 corrupt；存储或目录读取失败照常返回。以前被隔离为人工的是这条出价，冻结的钱一起进了人工单。
- **坏对手单移出盘口。** 撮合事务里对手单 workflow 解不开、缺原归属身份或原主已换时，在同一事务里把它隔离（移出盘口与 `Due`，计入 `trade_commerce_quarantined_total`），taker 不报错，下一轮按新的最优价重撮。以前返回的是 taker 的错误，taker 每轮报一条 ERROR，坏单一直停在最优价位上，挡住整个物品的盘口。对手单的角色被另一条挂单冻结时（`lockRoles` 阶段），无法分辨是哪一方被冻结，冻结本身也是暂时的，所以不移出盘口，这一条列为残余。
- **撮合失败不走推迟 / 隔离。** `advance` 已提交，旧版本的推迟与隔离必然落空。现在单独计入 `trade_commerce_market_match_failures_total{class}` 并记日志（`conflict` 只记 INFO），下一轮重撮。
- 残余：
  - 别的事务长期持有某条合同行锁时，那一行无法推迟，每轮仍占用它的子预算加推迟预算（约 1.4s/3s）。队头连续有两条这样的行时，同一轮其余行就排不上，要等锁释放。
  - 人民币寄售 `rmb_freezing` 的封存循环（每个持有方 350ms、最多 8 个）在 1.1s 子预算内一轮只能走完约 3 个不可达的持有方，然后按 `retry` 推迟 10s。游标按 listing 记在内存里，进度能延续，但比以前慢。
  - 对手单角色被冻结时会挡住它所在的价位，直到冻结解除。
  - 被隔离合同的原 stage 只写在隔离日志里，没有持久化进合同；也没有列表、恢复、补发或退回的管理入口。手册 §6 列为待补。
  - Java 版待做：`CommerceRuntime.tick` 的单行预算仍是 `min(5000, 剩余)`，整轮 10s 的末段同样会与父截止时刻重合；冲突分类、隔离截止在途扣款、拍品身份退款、坏对手单移出盘口都还没有对齐。
- 回归测试（未运行）：
  - 单测：`tick_isolation_test.go` 新增 `TestTickConflictClassification`、`TestRowBudgetDerivesFromParentDeadline`，并在退避用例里加上 `role_conflict`。
  - 真库：`tick_isolation_db_test.go` 新增 9 个用例：
    - 按生产 3s 预算：队头卡住时被自己的子预算截断并推迟，后面的合同照常推进；队头合同行被别的事务锁住时其余合同照常推进，锁放开后队头照常推进；
    - 旧版本的推迟与隔离落空；
    - 隔离截止在途买家扣款；
    - 角色换主按分钟级推迟；
    - 拍品买家竞态不推迟、下一轮成为最高价；
    - 拍品缺身份或解不开时出价退款一次；
    - 坏对手单移出盘口后按次优价成交；
    - 两个副本同时 `Tick`，扣款、补偿、退回各只建一次。

### 15.4 2026-10-20 原拍卖对照与竞拍重放修复（未编译、未运行）

本轮重新按负责人给出的原仓 `D:/luyuan/xuanming-server/services/economy/auction` 核对实际实现。本仓没有单独的 `go/auction` 服务，已移植的材料订单簿与新增限时竞拍在 `go/trade/internal/commerce/{market,auction}.go`，不能由目录同名与否判断是否迁完：

- 原仓 `AuctionRepo.ReserveMatch` 在同库事务内按订单 ID 升序锁两单，复核市场、物品、买卖方向和剩余量，以 maker 价原子写 PENDING 成交意图与双方数量；之后由 `SettlementLedger.Settle(match_id)` 幂等完成资产。Redis 市场锁只提供工作互斥，不是最终成交证据。
- 本仓 `matchMarket` 在全局 TiDB 内先锁市场/物品/币种 guard，再按 ID 升序主键点锁两单，复核已托管阶段、撤单状态、剩余量、价格与不同玩家；成交合同、双方预留量同事务提交。交付由该合同的可靠资产腿推进，收到玩家 SQL 终局才结案。原仓通过 inventory 的双方原子转移没有照搬成两个独立 RPC 的“原子”承诺；本仓保留可恢复待交付/补偿状态，不另建玩家钱包。
- 原仓的按市场分片、最多两片拓扑与 Redis 路由观测不是本仓的全球市场路由。当前材料订单入口只接受配置白名单与 `market_id=1`，所有区域指向同一 SQL 权威域，不能把它描述成任意多市场分片已接完或达到容量目标。

查出并修复两版共有的可观察幂等缺口：出价被接受为最高价后，原金额已经不满足“下一笔加价”；此前 Go `PlaceBid` 和 Java `CommerceMatching.bid` 先检查当前最高价/截止，再查幂等请求，导致丢回执后的原请求被拒。本次复用引擎的权威 SQL 请求号与完整载荷摘要检查，在拍品观察之后、易变状态校验之前返回原合同。观察在前保证如果并发 worker 已用该出价推进最高价，随后的请求查询必能看到已提交的出价合同；没有已有请求的新出价仍按当前状态拒绝。并发首次创建仍由原唯一请求键及冲突后读回收口，不改变锁与事务边界。同请求号换金额或拍品字段仍拒绝，不分配第二个合同或扣款 seq。

另修 Go 出价合同漏填 `ExpiresMs`：现在与 Java 一致继承拍品截止，后续托管扣款 outbox 的 `deadline_ms` 不再为 0。首次扣款落地晚于截止仍由既有 `acceptBid` 使用库时钟复核，并走原币退款；不把超时猜成未扣到。其余会发起扣款的合同创建路径已有明确截止，材料订单虽无到期时间也已明确采用撤单收口，不在本批改规则。

两版新增真实 TiDB 回归分别在 `engine_tidb_test.go` 和 `CommerceTiDbTest.java`：八个并发重复请求返回同一合同、最高价后与截止后原请求可读回、换金额/拍品摘要拒绝、截止后新请求拒绝、只保留原扣款与 seq，并核扣款截止。夹具只注入 SQL 资产终局，仍不冒充多进程到账。**本批仅执行 gofmt 与 diff 静态检查，尚未编译或运行上述用例，不能将此记录作红/绿证据。**

mall 的已定路线继续按 D-17，不重新选型。本机两版均有固定上游 `dcaa93b3150352e5044708d7211b5bed0af4509f` 的 PMS/OMS 模型、来源摘要与许可证；实际 `Mall.Tick → 官方交付合同 → SQL资产腿终局 → delivery receipt` 桥接已经存在。官方项目仍把自身定义为前后台电商，见 [上游仓库](https://github.com/macrozheng/mall) 与 [本项目固定版本 README](https://github.com/macrozheng/mall/blob/dcaa93b3150352e5044708d7211b5bed0af4509f/README.md)。完整管理后台、人民币渠道关单/退款/提现、角色过户仍不能由这条桥推导已完成，真实双区/旧归属实例故障验收边界保持。

产品口径同时核对 [真钱充值第 4 版](payment-recharge.md) 与 [摆摊拍板记录](stall-system.md#1-需要你拍板的选择题)，不把较早 J-O 段当作其后决定未发生：充值已定国内/海外都要，先苹果/谷歌骨架、无账号阶段先 mock，订单继续属于 mall、钻石继续属于玩家资产；摆摊已定支持离线、只收银两、成交税 5%、天墉城固定点，其余按推荐项。充值的 `go/payment`、带修订号的付款事实、钻石扣回/欠款及两版契约仍是新实现批次，当前源码中没有这些已落地证据；第 4 版仍列 30+ 文件分批门禁及钻石档位/合规上限/测试白名单等具体待决项。其“snapshot 分支未合入”是 10 月 19 日的历史前提，当前已能读到原合同/交付引擎，不能照旧把它当代码缺失；是否满足当前正式生成、构建与故障验收仍分别核原件。本次不擅自启动充值或摆摊的新实现批次。

### 15.5 2026-10-20 Java 推进隔离对齐（未编译、未运行）

本轮实际读 Java `game-trade/commerce` 发现 §15.3 尚未对齐，不能按旧整体对齐表推断它已经有 Go 的单行隔离。现按本版 JDBC/Deadline 接缝补齐：

- `CommerceRuntime.tick` 为单个工作线程驱动，`Due` 只取 `updated_ms <= 引擎时钟`；一行采用最多 5 秒子预算。暂时失败按 10 秒 ±20% 推迟，未决窗口背压按 60 秒 ±20% 推迟，确定的坏合同转 `Manual`。日志类别固定为 `retry / too_many_pending / corrupt`，携带合同号与版本，不把玩家身份做指标标签。未决资产投递的单行错误同样保留原 seq、记录并退避，不终结未知结果。
- 推迟先主键点锁、比较观察版本，只改调度时间，不增加业务版本；隔离也先锁与比较版本，能解码的 workflow 置 `manual`，坏字节不能解码时原样保留，只更新状态/版本。业务推进已改变版本时旧推迟/隔离落空，不覆盖新状态。取消、结案标记按 op_id 升序点锁后逐行写，与资产终结锁序对齐。
- `bid_held` 等待拍品时推迟到截止（至少 30 秒、至多 1 小时），不再永久占住队头。未接受的 `bid_ready` 遇到缺失/损坏拍品时只退自己的冻结款；原最高价合同损坏不误隔离当前出价。截止仍由数据库时钟裁决，调度时间沿引擎时钟。
- ACK 每页 50 条，按无符号 op_id 向后翻页，到表尾绕回。坏载荷、RPC/SQL 确认错误与 ACK 清理失败均保留原待办并记录，继续本页其它玩家；普通离线未完成不假报成功。游标只属于驱动 tick 的工作线程，不用于确认资产。官方商城也逐意图处理，单条失败汇总到页末，不能阻断同页后续交付回执。
- `advance` 按最终状态识别人工终结/放弃，不再要求它们冒写 scene 的 `durable`；坏状态/缺待办/坏快照以及不可转交的已扣实例原件立即隔离。没有 SQL 或人工明确终局的待办仍继续等待，不放宽到账证据。

新增 `CommerceRuntimeTest` 经真实 tick/商城 HTTP 接口测试队头背压、坏合同、held 延期、离线首页/坏载荷 ACK 翻页及商城重复依赖错误不挡后项。商城页末错误汇总允许依赖重复返回同一异常对象，不对它自身调用 `addSuppressed`，避免汇总动作截断后续交付；用例要求前两项同一 SQL 错误、第三项照常回报原合同，页末仍抛原错误。`CommerceTiDbTest` 另加真实随机库上的延期/版本 CAS、坏原件保留、放弃补偿与人工实例缺原件隔离回归。两版竞拍重放用例也仍待实际运行。本段只记录源码与静态检查，不把 Mockito/待运行真库用例称作已验收，不关闭 §15.2 末条的真实双区/归属围栏边界。

**跨页缺口已按负责人授权补齐源码，仍未编译或运行**：原来两版 `DeliveryStore.poll` 固定取最早 20 条 PENDING，20 个长期离线买家会挡住第 21 个官方订单。负责人已明确授权本批从 23 个源码／测试／文档文件扩至约 32 个，两版同步修改内部 poll 协议、存储与 worker，不改客户端契约或资产事实源。

内部请求为 `Poll{limit,cursor}`，响应为 `PollPage{entries:[{intent,cursor}]}`。空游标开始新一轮，从 PENDING 最大 `(created_ms,order_sn)` 固定上界；后续只查询 `after < key <= upper`，按同一复合键排序并多取一条判尾，复用既有 `(status,created_ms,order_sn)` 索引。规范游标为 `v1:<afterMs>:<afterUUID>:<upperMs>:<upperUUID>`，非规范数字、UUID、版本或越界值整体拒绝；持续新增不能延长当前轮次。尾项游标为空，表示该项尝试后绕回；上界订单已完成导致的空尾页同样绕回。分页不持有跨请求事务，也不承诺数据库快照。

trade 先校验整页包装，再逐项尝试、逐项更新游标。单项依赖失败保留原待办并允许扫描后项，预算不足时不越过尚未开始的项；HTTP、解码或包装错误不前移。游标由单个恢复 worker 持有，重启可以丢弃；多个 worker 重读依靠原订单唯一键、原合同与原资产 seq 幂等。包装中的游标不进入原 Intent 字段或摘要。尾项空游标没有携带完整排序键，客户端不独立证明该项的 `created_ms` 边界，信任同版商城按上述查询生成包装；游标只用于公平调度，资产终局仍依赖既有 SQL 提交证明。

新增 Go `TestTiDBMallPaging*`、HTTP 拒错组，主仓商城 `DeliveryStoreTest`，Java `CommerceRuntimeTest` 与 `MallTiDbIntegrationTest` 回归，覆盖同毫秒翻页、前 20 项未终局时后 5 项继续交付、原摘要／原 seq 重放、预算／中断、请求错误／坏包装、上界完成、页间插入、多 worker 和重启。SQL 测试对资产终局的注入只验证合同与交付回执状态机，不能冒充真实 scene 到账或双区故障验收；所有新增用例仍待实际运行，缺少 TiDB 配置导致的 Skip 不算验收通过。**内部 HTTP 响应形状已变，同版 mall 与 trade 必须配套部署；当前 Docker／运行产物未重建，本节不表示已经部署或全域迁移完成。**

### 15.6 2026-10-21 负责人决定与继续实施范围

负责人要求把剩余功能及验证继续做完，资源不足时先实现，满足门禁后再编译、真库和双区验收。该要求覆盖原32路径之外的必要完整批次：先补[账号角色SQL权威](account-role-authority.md)及全部读写／工具入口，再接冻结、角色过户和人民币结算；不再把只写完源码当全部完成。

J-O1至J-O4已由负责人明确采用设计默认值且允许配置：公示48小时、寄售7天、费率5%向上取整到分、收入冷静期3天；角色出售前必须离帮、好友保留；可售货币默认仅金币，钻石关闭，绑定货币仍禁止。负责人另明确“先完成mock全链路，真实渠道默认关闭”，随后再次说明“接渠道的话以后接”。因此本轮支付、退款、提现按本地mock实现和验收；不将商户／开发者账号或真实渠道作为本轮实现前置，也不声称已经接通外部资金。

两版已补支付角色的只读查询源码：`POST /internal/payments/order-view {orderId}`返回原Order完整视图；`POST /internal/payments/skus {afterSkuId,limit}`返回现有Skus。复用原payment主体、签名与nonce，并在Controller再次核角色，未知内部路由404、gateway/delivery越权401。只读指业务订单、库存、付款、交付和退款表不写；认证nonce仍按原协议消耗。SKU查询仅含现有已发布、可售且有物品映射的官方物品，不等于钻石包或付款事实修订已完成。新增Store、真实HTTP及部署前缀／转发回归仍待执行。

后台受限目录／库存命令的当前实施合同见[商城后台设计](mall-administration.md)；它不能直接改已付款、已交付或历史订单快照。完整后台、人民币托管成交、卖家收入、mock提现和角色过户仍需逐项接线及实际验收，本条不是完成声明。

### 15.7 2026-11-05 转人工合同的恢复入口（未编译、未运行）

对应 [缺口清单](../handoff/server-gap-audit-20261101.md) 第 2 条后半。**状态：已落码，未编译、未测试，待 Codex 验证。** 实现在 `go/trade/internal/commerce/manual.go`、命令行 `go/trade/cmd/commercefix`，手册 [§4.2](../ops/trade-commerce-quarantine-runbook.md)。

**原问题**：合同转人工（`status=10`）之后引擎不再推进，托管的物品和货币没有出口，原 stage 只在日志里。手册只能让运维取证、登记工单、等入口。

**转人工记录**：`TradeCommerceWorkflow.quarantine` 记下转人工之前的 stage、status、原因类别和错误原文（截到 256 字节）。所有转人工的地方都改走 `markManual`：tick 隔离（`corrupt`）、坏对手单（`bad_counterparty`）、部分发放（`applied_partial`）、最终发放没到账（`final_credit_failed`）、市场单名下有转人工的子单（`market_fill_manual`）、人民币寄售退回失败（`rmb_return_failed`）。已经在 manual 的不覆盖第一次的记录。workflow 解不开的合同仍只改状态列，没有记录。

**行为变化**：最终发放没到账以前写 `stage=done`，op 行随即被标业务消费、过保留期会被清掉；现在写 `stage=manual`，op 行留到人工处理完。

**转人工之后玩家撤不了单**（2026-11-12 修人民币寄售这一条，未编译、未运行）：转人工的合同只由人工处置入口改。玩家撤单的两个入口都按可撤阶段白名单判，`manual` 不在其中，撤单回 `kTradeOrderStateConflict`，合同行不变：

- 通用合同：`Engine.Mutate` 的 `cancel`，一直是白名单。
- 人民币寄售：`RmbConsignments.Cancel`，白名单是 `rmbCancellable`（各阶段见 §16），锁外的预检与合同行锁内的事务体用同一份判定。
- 人民币寄售以前是排除式判定，只排除已结算、`rmb_delivery`、`rmb_refund`、已取消，漏了 `manual`。退回腿没到账或交付腿部分发放（这时买家已付款）转人工之后，卖家一撤，`status` 被改成 6 而 stage 仍是 `manual`，后果有四个：
  - 合同从人工列表（`status=10`）里消失；
  - 人工处置入口因 `status` 不是 10 拒绝；
  - 引擎对 `manual` 不推进，这一行还一直排在 `Due` 队头；
  - `deliver` 不再向商城回报 `MANUAL`，托管的资产或已付款的订单没有出口。
- 回归测试 `go/trade/internal/commerce/rmb_cancel_test.go`：白名单逐阶段、被拒的撤单只读一次合同不开事务（不连库）；真库上转人工的合同撤单被拒且合同行逐字段不变、仍在人工列表、人工补发计划照常能算、商城仍收到 `MANUAL`，白名单里的阶段照常撤单并按取消结案。

**三种处理**，都只改合同行，不碰 op 行，不发资产：

| 动作 | 什么时候用 | 做什么 | 拒绝 |
|---|---|---|---|
| `resume` | 转人工的原因已经消除（数据修好了、卡死的腿已终结、子单已处理） | 回到记录里的 stage、status，由引擎重新判定；原因还在会再次转人工 | 原因是 `final_credit_failed`（重新判定还是转人工）或 `owner_changed`（先 `redirect`） |
| `reissue` | 最终发放没到账 | 每条没到账的发放腿补发一条：原样的资产、原样的收件人，名字是原名加 `#2`、`#3`；原腿标 `superseded_by`，不再算作最终发放；回到记录里的 stage | 原因不是 `final_credit_failed` 或 `rmb_return_failed` |
| `refund` | 还没有任何发放到账 | 每条已扣进托管的腿各退一条给扣出它的人（`<原名>/refund`；已部分成交的市场单只退剩余部分），没到账的发放腿标 `superseded_by=refund`；stage 改 `return`，按取消结案 | 官方商城、人民币寄售、被拍品引用的竞价、成交子单、全部成交的市场单、已有出价的拍品（用 `void-auction`）；已有发放到账；已经整单退回过（退回没到账用 `reissue`）；实例托管没有原件；原因是 `owner_changed` |
| `partial` | 有腿部分发放 / 部分扣出 | 见 §15.7.1 第 2 类 | 见 §15.7.1 |
| `redirect` | 参与方的角色已换主或已删 | 见 §15.7.1 第 1 类 | 见 §15.7.1 |
| `void-auction` | 有出价的拍品成交不了 | 见 §15.7.1 第 3 类 | 见 §15.7.1 |

共同的前置：合同在 manual、有转人工记录、所有腿都有结局。转人工之后才有结局的腿（隔离时被截止的托管扣款）在处理前按 `advance` 同一判定补记；在途的腿、查不到 outbox 行的腿拒绝，要先等结局或取证；部分发放的腿只有 `partial` 接受，还没发出的腿只有 `redirect` 接受。一次新建的腿不超过引擎单步上限 8。

**写路径与幂等**：

- `commerce.ResolveManual` 在 `CommerceRepo.Tx` 里：主键点锁合同行 → 核对操作人给的版本 → 补记结局 → `planManualAction`（纯函数）→ 追加 `manual_actions` 留痕 → `Save`（带版本条件，版本加一，`updated_ms` 写成现在）。锁序与引擎相同：出价和作废拍卖先锁拍品的 guard 行、再锁拍品、最后锁出价（与 `acceptBid` 相同）；`redirect` 先按 player_id 升序锁角色护栏副本、再锁合同（与 `advance` 相同）。
- 新腿不带 `op_id`。合同重新到期之后，发号、写 outbox、投递、记结局都由引擎的 `advance` 做，和玩家操作同一条写路径：原归属身份核对、按角色名只建一次的腿、单副本串行都不变。原腿和原 seq 不动，留作证据。
- 版本条件让同一条命令只生效一次：重跑时版本已变，回「未改动」。不加 `-execute` 时只算计划，同样在锁下算。

**鉴权与留痕**：2026-11-12 起入口是 trade 的管理 RPC `TradeAdmin.ResolveManualContract`，由运行中的副本执行；运维工具 `commercefix` 只签名、调用、打印，不连库(原先的直连库命令行写法已删,原因见 §15.7.1「管理 RPC」)。调用方证明用 serviceauth(调用方 `ops-commerce`),操作人、依据、二次确认、版本、目标环境名都必须给。留痕写进合同（`manual_actions`：动作、操作人、依据、时刻、处理前版本、转人工记录、新腿、被取代的腿；2026-11-11 起另有各动作的专用字段，见 §15.7.1），服务端同时打 `[CommerceManual]` 行并回给工具，原样贴进工单。

**残余**（2026-11-11 更新）：

- 原主已换、部分发放、竞价与拍品互相引用、已部分成交的市场单：处置见 §15.7.1。
- 实例托管被人工判定 APPLIED 时没有原件，退回不了，要按 `player_snapshot` 人工补发；部分扣出的实例、宝宝同理（§15.7.1 第 2 类）。
- 管理 RPC 2026-11-12 已加（§15.7.1「管理 RPC」）；管理后台的页面与审批流不做，理由见 §15.7.1「仍不做的」。
- Java 版待做，记在 `PARITY.md`。

**回归测试**（都没有运行）：

- 单测 `go/trade/internal/commerce/manual_test.go`：转人工记录（含只记第一次、按字截断）、补发和补发链、恢复推进、整单退回及六种拒绝、单步上限、补记结局的五种拒绝、请求校验。
- 命令行 `go/trade/cmd/commercefix/main_test.go`：退出码、默认只算计划、参数校验、show 列出每种动作能否处理。
- 真库 `go/trade/internal/commerce/manual_db_test.go`（门控同 `tick_isolation_db_test.go`）：隔离后的寄售整单退回，计划不写库、执行留痕、重跑落空、引擎发号且只退一次、到账后按取消结案；没转人工和不存在的合同拒绝。

#### 15.7.1 2026-11-11 四类残余的标准处置（缺口清单第 2 条残余，未编译、未运行）

**状态**：已落码，未生成协议、未编译、未测试，待 Codex 验证。实现在：

- `go/trade/internal/commerce/manual.go`（入口、锁序、留痕、show）与 `manual_plan.go`（六种动作的判定，纯函数）；
- `tick_isolation.go`（换主当场转人工）、`role_authority.go`（归属判定）；
- `market.go` 与 `schedule.go`（子单转人工不再连带母单，子单收尾唤醒两张母单）、`engine.go`（`marketRemainder`）；
- 管理 RPC `TradeAdmin.ListManualContracts` / `ResolveManualContract`（`proto/trade/trade_admin.proto`，入口 `go/trade/internal/logic/manual_contract_ops.go`）；
- 运维工具 `go/trade/cmd/commercefix` 改为只调这两个 RPC，不再直连库；

手册 [§4.2、§4.3](../ops/trade-commerce-quarantine-runbook.md)。

**四类共用的原则**（与 §15.7 相同，这里写成判据）：

- 不丢资产：每份托管要么交给有权拿它的人，要么原样退回扣出它的人；证据判不清的留在合同里，不猜。
- 强一致：在合同行锁和版本条件下改。涉及两张合同的（拍品与出价）在同一事务里按引擎锁序锁两行，两个版本都核；涉及角色归属的先按 player_id 升序锁角色护栏副本，再锁合同。
- 只改合同行、只加还没发出的新腿：原腿、原 seq、op 行都不动，新腿由引擎下一轮发号投递。
- 每个动作可审计：`manual_actions` 追加一条，`[CommerceManual]` ERROR 行原样贴工单；默认只算计划，加 `-execute` 才写。
- 重放幂等：同一条命令重跑时版本已变，回「未改动」（退出码 3）；两个操作人拿同一个版本并发执行，只有一个生效。

| 情形 | 能由规则自动判的 | 要人工的动作 |
|---|---|---|
| 1 原主已换 | 换主、删角当场转人工（原因 `owner_changed`），不再每 5 分钟空转 | `redirect`：改绑到原账号名下的另一名角色 |
| 2 部分发放 | 不变（缺口只在 scene 日志与流水里） | `partial`：按人认定的缺口补发；托管扣款部分扣出则按实际扣到的退回 |
| 3 竞价与拍品互相引用 | 拍品不引用的出价可以单独退回 | `void-auction`：拍品与它引用的出价在同一事务里一起作废 |
| 4 已部分成交的市场单 | 成交子单转人工不再连带母单 | `refund` 只退剩余部分 |

**1. 原主已换**

真实状态：

- 角色换主只有两条路：删角（账号清空、`owner_revision` 加一）和角色过户（`owner_revision` 加一）。两条都先经参与者投票，trade 的 `RolePending` 在这名角色还有未终局合同或资产指令时投反对票。所以正常运行时，参与方有未终局合同就换不了主；出现这种合同，只可能是投票被绕过、交易库回档，或目录被人工改过。
- 以前引擎把它和「角色被冻结」都算 `role_conflict`，每 5 分钟复查一次。合同永远停在原阶段，托管一直扣着，也不进人工列表。

自动规则（新）：`role_conflict` 推迟之前，先在一个事务里按 player_id 升序读参与方的角色护栏副本（`Engine.quarantineOwnerChanged`）。以下三种算不可逆的换主，当场转人工，原因 `owner_changed`，detail 写是哪一方：

- 副本修订号已超过合同原身份；
- 修订号相同但账号不同；
- 角色已删（账号清空）。

冻结、投票中照旧 5 分钟复查。副本缺行或落后于合同原身份时判不了，照旧推迟。人民币寄售（kind 6）卖的就是角色本身，换主是它的正常流程，不走这条规则。

为什么：`owner_revision` 只增不减，复查不会让它变回来，这与「确定性失败当场隔离」是同一条规则。转人工同时截止在途托管扣款，告警走 `TradeCommerceQuarantined`，`commercefix list` 看得到。

人工动作 `redirect -party seller|buyer -to <player_id>`：把换了主的一方改绑到原账号名下的另一名角色，也可以是被原账号买回的同一名角色。

- 锁内核对：
  - 这一方确实已不归原身份（判据同上）；
  - 目标角色现在属于原账号，没被冻结、没删，也不是合同的另一方；
  - 腿都有结局（没发出的腿允许，见下），没有部分发放的腿；
  - 不是官方商城、人民币寄售。
- 效果：
  - 合同上这一方的 player_id 换成目标角色，原身份换成「原账号 + 目标角色现在的修订号」；拍品的买方同时改最高出价人；
  - 这一方没发出的腿直接改投目标角色；
  - 发往原角色、没到账的发放腿，由发往目标角色的新腿取代；
  - 已经到账的腿和已扣出的托管不动，以后的退回、交付都按合同上的新一方发；
  - 回到转人工之前的阶段，由引擎接着推进；
  - 留痕记 `party`、原角色、目标角色、改前改后的身份、改投了哪些腿。
- 为什么：
  - 在途资产属于原账号，不随角色过户（`RolePending` 的口径）。交易只能把资产发给角色，没有账号级钱包，所以只能发到原账号指定的角色。
  - 改绑等于「这一方整体换成另一名角色」：此后该付的从新角色扣，该收的发到新角色。由客服按原账号的申请执行，工单里写明原账号同意；工具只核对目标角色确实属于原账号。
  - 改绑之后，这名角色在合同结束前不能出售或删除，因为 `RolePending` 看得到这张合同。
- 管不了的：换主之后才到账、发进原角色的发放腿，资产已经到了新主手里。`show` 列出每条已到账发放腿的终局时刻，操作人与过户时刻对照，属于这种的按手册在交易之外补偿。

**2. 部分发放**

真实状态：

- scene 的发放顺序固定（[资产通道](guild-phase2/04-asset-channel.md) §4.33）：按数量的物品整批（失败就零改动）→ 实例逐件 → 宝宝 → 货币逐条，有改动之后再失败才记 APPLIED_PARTIAL。所以按数量的物品不会缺；缺的是「实例、宝宝、货币」这个顺序上的一个后缀。
- 扣款的顺序是按数量的物品 → 实例 → 宝宝 → 货币。可预知的失败都在扣之前复核，只有提交阶段内部不变量损坏时才会部分扣出。
- 账本只记哪个 seq 是部分发放，缺了哪部分只在 scene 的 ERROR 日志（`[AssetOp] partial ... failed=`）和流水里。部分扣出的托管原件不进 `escrow_snapshot`（只有 APPLIED 才写）。
- 引擎遇到部分发放即转人工（原因 `applied_partial`），不入账、不退款；原工具一律拒绝。

自动规则：不变。缺口只有人能从证据里认出来，引擎不猜。

人工动作 `partial -part <腿名>=<缺口>`：可以重复，一次给出这张合同全部部分发放的腿。缺口写 `instance:<guid>`、`pet`、`currency:<类型>`，逗号分隔，每项都按原腿里的整件、整条算。

- 锁内核对：
  - 点名的腿确实是 APPLIED_PARTIAL，合同里其余部分发放的腿也都点名了；
  - 缺口是原腿资产的真子集：非空，不是全部，不点名原腿里没有的东西；
  - 缺口符合 scene 的应用顺序：发放缺的是后缀；扣款里有实例没扣到时，宝宝和货币也没扣到；货币缺的是后缀；
  - 不是官方商城、人民币寄售。
- 发放腿：补发一条只含缺口的新腿给原收件人（`<原名>#k`），原腿记下部分发放的结局并标被它取代，回到转人工之前的阶段。补发再被拒时按最终发放没到账转人工，用 `reissue`。
- 托管扣款腿：交易没有托管齐，不能继续，按 `refund` 整单退回。这条腿只退实际扣到的部分（原资产减缺口），其余已托管的腿全额退回，`refund` 的其余前置条件照旧。扣到的部分里有实例或宝宝时拒绝：部分扣出没有托管原件，要按 `player_snapshot` 人工补发。
- 发放腿和扣款腿不能在一次里混着处理。实际也不会同时出现：扣款没托管齐，合同走不到发放。
- 为什么：只补缺口，已到账的部分不重复发。缺口由人从证据里填，工具按子集与应用顺序校验，留痕写清每条腿的缺口。填错时（比如把已到账的货币当成缺的），应用顺序能挡住一部分；挡不住的由留痕追责。

**3. 竞价与拍品互相引用**

真实状态：

- 出价（kind 100）冻结出价人的钱。被拍品接受时，拍品记 `winning_bid_id` 和 `highest_bidder`，出价停在 `bid_held`。
- 结拍时，拍品托管发给出价人，另铸一条货款发给卖家，金额就是这条出价的冻结款；拍品结案之后出价才结案，冻结款算已付。被超价的、截止后的出价走 `bid_refund` 原币退回。
- 所以出价的冻结款与拍品的交付是一笔交易的两半：被拍品引用的出价，冻结款归拍品结算；没被引用的，冻结款只属于出价人。
- 以前拍品有出价、或合同本身是出价，`refund` 一律拒绝。拍品转人工时，被引用的出价每小时复查一次，一直等，没有出口。

自动规则（新）：出价可以单独 `refund`，条件是锁内读到的拍品不引用它（拍品不存在，或 `winning_bid_id` 不是它）。转人工的出价不会再被拍品接受（`acceptBid` 只接 `bid_ready`），所以「不引用」确认之后不会再变。

被拍品引用的出价不能单独退回：

- 拍品已结案的，用 `resume` 回 `bid_held`，由引擎按「拍品已结算」把它结案；
- 拍品还在推进的，等它；
- 拍品转人工的，按拍品处理。

人工动作 `void-auction -linked <出价号> -linked-version <出价版本>`，对拍品执行，作废有出价、确定成交不了的拍卖。

- 锁序 guard → 拍品 → 出价，与 `acceptBid` 相同；两个版本都核。
- 锁内核对：
  - 拍品在人工状态，原因不是 `owner_changed`（先 `redirect`）；
  - 拍品引用的就是这条出价，出价停在 `bid_held`，或转人工前停在 `bid_held`；
  - 两张合同的腿都有结局，没有部分发放；
  - 拍品没有任何发放到账：卖家没收到货款，出价人也没收到拍品；
  - 拍品的托管原件转得动。
- 效果：
  - 拍品没到账的发放腿标被退回取代，托管退回卖家，按取消结案；
  - 出价改成 `bid_refund`，与被超价同一条路，由引擎退回冻结款；
  - 两张合同各记一条留痕，互相写对方的号。
- 有发放已经到账时不能作废：卖家已收款的，只能把拍品补发给出价人（`reissue`）；出价人已收到拍品的，只能补发货款。

**4. 已部分成交的市场单**

真实状态：

- 母单（kind 4、side 1/2）托管全部数量。每次撮合在同一事务里新建成交子单（side 0），并把成交量记进双方母单的 `filled_quantity`。
- 成交的那部分从母单托管里划给子单，由子单自己的腿交付（买家收货、卖家收款、买家差价退回）；母单只对剩下的部分负责。
- 转人工的母单不在盘口上（`on_book` 按 stage 算），不会再成交，`filled_quantity` 不再变。
- 以前子单转人工会让母单也转人工（`market_fill_manual`）：母单剩下的挂单量被冻住，玩家撤不了单；母单的 `refund` 只要已部分成交就拒绝。

自动规则（新）：子单转人工不再连带母单。

- 母单在子单都收尾之前不结案（撤单后的 `market_wait`、全部成交的 `market_open` 本来就等子单），转人工的子单算没收尾；剩下的挂单量照常成交或撤单。
- 子单人工处理完、引擎把它推进到结案的那一步，唤醒两张母单（`filledParentsToWake`：`parent_id` 是买单、`counterparty_id` 是卖单）；子单转人工本身不再唤醒母单。以前只唤醒买单，卖单要等 `inFlightRecheck` 兜底复查才发现子单收尾，这次一起修了。
- 为什么：子单的资产在撮合时已从两边母单的托管里划出，母单自己的资产没有问题。以前一张子单出事就冻住母单剩下的挂单量，还多一张人工单。代价是母单在子单转人工期间每 30 秒复查一次（`inFlightRecheck`），量只取决于转人工的子单数。

人工动作 `refund` 对已部分成交的母单放行：只退剩余部分，卖单退剩余数量的物品，买单退剩余数量 × 单价的货币，与玩家撤单同一个算法（`marketRemainder`）；已成交的部分归子单，不动。全部成交的母单拒绝：没有可退的托管，用 `resume`。成交子单仍不能单独退回（它的资产来自两张母单），按发放腿的规则用 `reissue`、`partial`、`redirect`。

**顺带修的缺陷**：`refund` 跑两次会丢托管。第一次退回没到账（转人工 `final_credit_failed`）之后，如果再跑一次 `refund`，旧代码先把没到账的退回腿标「被退回取代」，再按同名加退回腿；名字已经存在，什么也没加上，合同按「没有发放腿」直接结案，托管再也不退。现在合同里已有整单退回的腿时 `refund` 拒绝，退回没到账用 `reissue`。

**留痕的新字段**（`TradeCommerceManualAction`，`proto/trade/trade_table.proto`）：`party`、`from_player_id`、`to_player_id`、`identity_before`、`identity_after`、`retargeted_legs`（`redirect`）；`linked_commerce_id`（`void-auction`，两张合同互指）；`partial_parts`（`partial`，每条腿与它的缺口）。都在 `workflow_proto` 里，不加表列，不用迁移。

**管理 RPC**（2026-11-12 负责人要求查出的问题现在修、不留到以后，原定「有后台再加」改为这一轮做）：

- 两个方法挂在 `TradeAdmin` 上：`ListManualContracts`（按 `(updated_ms, commerce_id)` 翻页，一页最多 500）与 `ResolveManualContract`（模式 SHOW / DRY_RUN / EXECUTE）。做法照 match 的 `formationfix` 入口：
  - 调用方证明用 serviceauth，调用方只有 `ops-commerce`，密钥来自 `Commerce.ManualResolutionSecretEnv` 指名的环境变量，不与别的调用方共用。没配是关闭；配了但密钥不足 32 字节、环境名不合规或资产通道没开（没有合同引擎），启动日志打 ERROR，入口同样关闭。
  - DRY_RUN / EXECUTE 要带 `expect_target`，与本部署的环境名 `Commerce.ManualResolutionEnvironment` 逐字相等，连错环境回 `target_mismatch`。
  - 带客户端会话的调用被会话拦截器挡住，入口里再挡一层。
  - 审计行 `[CommerceManual]` 由服务端打（执行成功打 ERROR，其余 INFO）并回给工具；操作人、依据、缺口这些自由文本一律 `%q`，防止在行里伪造字段。
  - 只有 EXECUTE 提交结果未知时回 gRPC Unavailable，其余结论都是带内 verdict。
- 为什么工具不再直连库：写者身份、角色护栏的权威读（改绑要读目录、要自愈副本）与本库写者闸门都在服务进程里。交给运行中的副本执行，人工处置与引擎同一进程模型，留痕时间也用引擎的时钟（`Due` 按它取到期行）。以前工具用自己机器的时钟写 `updated_ms`，时钟快了合同会晚到期。
- `commerce.ResolveManual` / `ShowManual` 是唯一的判定入口，RPC 与将来的管理后台都调它，不另写一份。

**仍不做的**：

- 审批流（两人复核）：请求里的第二个名字证明不了身份，要有后台的登录身份才有意义；等有管理后台时在入口里接，判定不用改。
- 官方商城、人民币寄售：它们的人工处理要和商城订单一起改，商城记的 MANUAL 不会因为交易侧补发自动变成已交付，不在这个工具。

**回归测试**（都没有运行）：

- 单测 `manual_test.go`：
  - 缺口解析与校验（子集、非空、不是全部、应用顺序）；部分发放补发缺口、多条一起处理、漏点名拒绝、扣款部分扣出按扣到的退回、实例没有原件拒绝；
  - 改绑的腿改投与取代、拍品买方连带改最高出价人、目标是另一方拒绝；
  - 出价按拍品是否引用放行或拒绝；作废拍卖改两张合同、有发放到账拒绝；
  - 部分成交母单只退剩余、全部成交拒绝；整单退回不能跑两次；`owner_changed` 不能恢复、不能退回；`marketRemainder` 的四种情形。
- 单测 `manual_test.go` 另有归属判定表(`TestPartyChangeTable`):同一身份、副本落后、过户、买回、同修订号换账号、删角、副本缺行。
- 单测 `schedule_test.go`:子单收尾唤醒买单与卖单,子单转人工不唤醒。
- 单测 `go/trade/internal/logic/manual_contract_ops_test.go`:入口装配(关闭、密钥太短、环境名不合规、没有合同引擎)、准入(入口关闭、密钥不对、调用方不对、签名换方法、带客户端会话)、连错环境、结论映射、DRY_RUN 不写、参数原样传入、审计行引号防伪造、SHOW、列表翻页。
- 命令行 `commercefix/main_test.go`:参数写错不签名不拨号、密钥缺失只报变量名、请求带可验的签名、各结论的退出码、`show` 与 `list` 的输出。
- 单测 `data/commerce_repo_test.go`:`Save` 的语句多了 `seller_player_id`。
- 真库 `manual_db_test.go`,每类都有正常路径、重跑落空、并发三种:
  - 换主当场转人工,改绑后撤单退给目标角色;目标被冻结时拒绝;两个操作人并发改绑只生效一次;
  - 部分发放只补缺口,两个操作人并发只生效一次;
  - 作废拍卖期间引擎推迟持有出价不影响版本;出价被引用时不能单退,接受之前转人工的可以退;
  - 部分成交母单只退剩余,子单推进不碰母单;子单转人工时母单照常撤单、收尾。
- 真库 `tick_isolation_db_test.go`:原「角色换主按分钟级推迟」改成「角色被冻结按分钟级推迟」(换主改由 `manual_db_test.go` 覆盖)。

### 15.8 2026-11-06 合同推进按片分工、没变化不重存（缺口清单第 16 条，未编译、未运行）

**问题**（11-05 全链路水平扩展盘点，[缺口清单](../handoff/server-gap-audit-20261101.md)第 16 条）：

- 每个 stable 副本每秒跑一次 `Engine.Tick`，都从全局 `Due` 取前 100 行。各副本读到的是同一个队头，只能在合同行锁上互撞，加副本吞吐不涨。收据 ACK 同理，每个副本各翻一遍全部待 ACK 的行。
- `advance` 不论有没有推进都 `Save(updated_ms=现在)`。在架的寄售、在拍的拍品、等确认的角色交易、挂着的市场单一直停在推进状态里，每轮被重存一次，带来三个后果：
  - 版本号每秒加一，玩家按旧版本提交的购买、取消会撞上乐观冲突；
  - 这些行每轮都排回队尾，活跃合同越多，新成交排得越靠后；
  - 每行每轮多一次整行写，包括 workflow。

**改法**：

1. **按片分工**（新包 `go/shared/workshard`）。片号是 `commerce_id % 64`，收据 ACK 用 `op_id % 64`。每片的负责副本在成员表上按最高随机权重哈希选。`Due` 和 `AckDue` 把「只取本副本负责的片」写进 SQL，放在 `LIMIT` 之前：一个副本积压的行占不住别的副本的名额。
   - 成员表是 etcd 里本 zone 注册的 stable trade 实例（`TradeNodeService.rpc/zone/<z>/`），每 5 秒整表读一次。canary 副本不跑全局后台（[灰度设计](release-track-gray-canary.md) §2.5），也不进成员表，否则落在它名下的片没人推进。
   - 成员表只按 zone 圈，因为同一个 zone 的 trade 副本连的是同一个库。将来如果不同 zone 共用一个库，最坏情况是各 zone 各推进一遍全部片，和改动前一样，不会有片没人做。
   - 2026-11-10 起资产指令重投（`trade_asset_op` 的到期扫描）复用这份成员表：类别键 `trade-assetop`，片号 = `player_id % 64`，成员表的整表读不再只在有合同引擎时起（缺口清单第 25 条，见 [assetop-redelivery-sharding-20261110.md](assetop-redelivery-sharding-20261110.md)；未编译）。
   - 分工不承担正确性。推进、推迟、隔离仍以合同行锁和版本条件为准。成员表变化的几秒里，某一片可能两个副本都做（多几次冲突），也可能暂时没人做（下一拍补上）。
   - 成员表为空时，本副本做全部片，保活优先。为空包括还没读到，以及连续 30 秒读不到 etcd。成员表不空、本副本不在其中（还没注册完）时，本副本什么都不做。规则与 match 的 `OwnsWork` 相同。
   - trade 关机时先注销 etcd 再排空（`trade.go` 收尾顺序），副本退出后它名下的片 5 秒内（另加租约过期时间）就由别的副本接手。维护排空期间仍然注册着的副本，名下的片会停到它注销为止。维护排空是全服动作，这里不单独处理。
2. **没变化不重存**。`advance` 在锁内读到合同和 workflow 后先各留一份副本，推进完用 `proto.Equal` 比较。两样都没变就不 `Save`，只在同一个事务里把 `updated_ms` 改成下次到期（`RescheduleTx`，不改版本）。下次到期由纯函数 `nextDueMs` 决定：
   - **只等时间或玩家操作的**，排到截止时刻，最远 1 小时复查一次：
     - 寄售 `open`、拍品 `lot_open`、角色交易 `confirm`，都没有在途腿：排到 `expires_ms`；
     - 挂着、未全部成交的市场单 `market_open`，官方退款 `official_refund`：没有截止推进，只按 1 小时复查。
   - **等别的行上的事件的，30 秒兜底复查**（`inFlightRecheck`，2026-11-10 起）：有腿在途（等资产指令终结），或市场母单等成交子单收尾（撤单后的 `market_wait`、已全部成交的 `market_open`）。事件发生时合同会被唤醒，见第 3 点；唤醒丢了才等到兜底。
   - **其余照旧，下次到期仍是现在**：人民币寄售走到 `advance` 的资产段、没列出的 stage。和改动前一样每轮复查，只是不改版本、不重写 workflow。
   - **为什么能这样排**：玩家操作（购买、取消、确认）、竞价刷新最高价、撮合成交、人工恢复都会 `Save` 这一行，`updated_ms` 写成现在，立刻到期。所以只等时间或玩家操作的合同不必每秒空转。撮合由新单那一方发起：单子第一次进入 `market_open` 的那一步，`step` 在推进之后就撮一次，同一本盘口由 guard 行锁串行，所以挂着的单不需要每秒重撮。
   - **撮合失败**：`matchMarket` 失败时用 `Wake` 把这一行提前到现在（只提前不推后，不改版本），下一轮重撮，与改动前一样。
   - **不改推进判定**：`plan` 仍按锁内读到的合同和数据库时刻推进。下次到期排早了只多空转一次，排晚了上限是 1 小时，与 `bidHeldMaxWait` 是同一个兜底。
   - **并发**：玩家的 `Save` 带版本条件，`RescheduleTx` 不改版本，所以挡不住它。两者在同一把合同行锁上串行，玩家后提交时把 `updated_ms` 写回现在。所有事务都是 RC 隔离（`BuildDSN`、`WithTxRetry`）。别的事务在推进拿到合同行锁之前已提交的变化，推进读得到，会按「有变化」重存；拿到锁之后才来的写要等锁，在推进之后落地。两种情况下推进都不会把别人的唤醒覆盖掉。
3. **按事件唤醒**（三处，`Wake` 只把下次到期提前、不推后，不改版本）：
   - **撮合失败**：`step` 里 `matchMarket` 失败时把 taker 提前到现在，下一轮重撮（11-06）。
   - **资产指令终结之后**（2026-11-10）：`data.AssetOpRepo` 的 `Finalize`、`Abandon`、人工终结（`resolveManually`）在事务**提交之后**按这一行的 `ref_id` 唤醒 `ref_kind=2` 的合同（`wakeContractOf`）。不能放在同一个事务里：合同推进的锁序是「合同行 → op 行」，终结在 op 行锁里再锁合同行就成环。
   - **成交子单收尾之后**（2026-11-10）：`advance` 提交了一张市场成交子单从推进中变成已成交、已取消或转人工（`filledParentToWake`），就在提交之后唤醒它的母单。母单在 `marketBarrier` 里等子单收尾，同样不在子单的事务里锁母单行。
   - 三处都是尽力而为：唤醒失败只记日志，合同最晚在兜底复查时自己到期（在途腿与等子单的母单 30 秒，其余按上面的排期）。进程在提交与唤醒之间退出时同理。
   - 唤醒丢不了已提交的变化：唤醒发生在别的事务提交之后；推进在拿到合同行锁之后读腿结局（RC，读到最新已提交），读不到的那次终结一定还没提交，它提交之后的唤醒在推进放锁之后落地，把下次到期提前回来。
   - 测试夹具 `settleLegs` 直接改资产待办行，按同一口径唤醒对应合同；直接改待办行的其他用例同样补了 `Wake`。

**HRW 打分收口（2026-11-06）**：match 的 `internal/poolowner`、player_inbox 的 `internal/shardowner` 已委托 `shared/workshard.Owner`，trade 原本就使用这一入口。打分改为 `SHA-256(成员 + NUL 字节 + 键)` 的前 8 字节按大端序取 `uint64`，分数最高者负责，同分取字典序较小的成员；修复旧 FNV-1a 对短数字后缀扩散不足的问题。此次算法切换会重新分配现有键，项目尚未上线，归属不写入持久数据，无需数据迁移。固定新算法后的成员增删才遵守最小迁移：增加成员只把部分键交给它，删除成员只重分配原属它的键；不承诺任意成员集合、任意键都均匀分配。

本次只共用纯归属函数，各服务的成员生命周期和未同步、空表策略不变，因此不增加策略开关：trade 与 match 成员表为空时做全部工作；player_inbox 未完成首次同步或本进程未注册时跳过对账，watch 重连期间沿用上次成员表。数据正确性仍由各自的提交点保证，对账保持只读。

**不在这一轮**：

- 商城、人民币寄售两个循环轮询的是外部接口，不扫合同表。
- friend 的 `sweep.go` 重复执行另记在[消除单节点总纲](no-single-node-horizontal-scaling-20261001.md) §9。

**回归测试**（都没有运行）：

- 单测 `go/shared/workshard/workshard_test.go`：归属只取决于成员集合、增减一个成员只挪走它的片、SQL 条件的全有 / 全无 / 部分三种、注册解析跳过占位键 / 坏值 / canary / 未知轨道、成员表为空或过期时做全部片、不在表里时什么都不做。
- 单测 `go/trade/internal/commerce/schedule_test.go`：各 stage 的下次到期，有腿在途一律立即复查，截止时刻与 1 小时兜底取早的那个，人民币寄售不改节奏。
- 真库 `tick_isolation_db_test.go`（门控不变）新增两例：在架寄售推进一轮不改版本、下次到期排到截止，买家购买照常立刻到期且不撞版本；两个副本按片分工，Due 合起来不漏不重。原「两个副本同时推进只补偿一次」用 SQL 把截止改到过去时，同步把 `updated_ms` 改到过去：按到期时刻排期之后，只改截止时间不会让行重新到期，生产里截止和下次到期由同一个排期写入。

**Java 版**：待做，记在 `PARITY.md`。

### 15.9 2026-11-10 盘口只收已托管订单（未编译、未运行）

**原问题**：撮合选对手单（`market.go` 的 `marketCounterparty`）只按 `status=3` 过滤。材料订单下单后先进托管阶段（`stage=escrow`），status 是 3；托管扣款落地、进入 `market_open` 之后 status 还是 3。阶段只记在 `workflow_proto` 里，SQL 分不出两者。托管中的订单若是最优价，就会被选成对手单，撮合事务在锁内发现阶段不对，本轮不成交，也不报错。后果有三条：

- 排在它后面、已经上盘、价格可成交的订单一单也撮不上。新单只在进入 `market_open` 的那一步发起一次撮合（§15.8），之后按 1 小时复查，所以买卖两边会交叉挂着。
- 材料订单的托管腿没有截止。托管扣款一直回 RETRY 时（见 §15 开头「卖家撤单的窗口包含托管阶段」一条），这本盘口一直撮不动，直到卖家撤单。
- 深度（`GetMarketDepth`）同样只按 status=3 聚合，把撮合不到的托管中订单显示成可成交的挂单量。

这与 §15 开头「同质材料撮合只读取已托管 SQL 订单」的口径不符。

**修复**：

- `trade_commerce` 新增列 `on_book`（`proto/trade/trade_table.proto` 字段 22），含义是「kind=4 的买卖单，托管已完成，stage=market_open」。
  - 它是 workflow 的投影，判定只在 `data.OnBook` 一处。`CommerceRepo.Insert` / `Save` 按要写的 workflow 现算，与 `workflow_proto` 在同一条语句里写出，不取记录上带来的旧值，所以两者不会分叉。
  - `SaveStatus` 不动它。这条路只在 workflow 解不开时把状态改成转人工，状态列已经把这一行挡在盘口外。
- 选对手单（预读与锁内重读是同一条 SQL）和深度都加 `on_book=1`。盘口索引改为 `kind,market_id,item_config_id,side,status,on_book,unit_price,commerce_id`。
- taker 预检：不在盘口上的订单不发起撮合，不发成交号，不抢盘口 guard 行锁。以前托管中的订单有腿在途、每轮复查，每轮都要白走一遍撮合。
- 提交点不变：锁内仍按 workflow 复核两边都在 `market_open`、没有撤单、还有剩余量（AGENTS §11.3：预检不能代替提交校验）。
- 为什么不改 status：status 是客户端可见的 `CommerceOrderState`，托管中与已上盘都显示「托管中」是现有契约。加新状态值要两版同批改客户端契约，还要改 `dueStatuses` 等所有按状态取行的地方。新列只在服务端内部，客户端看到的 `MarketOrderView.state` 仍是 stage 原文。
- 为什么不在 Go 里多取几行再按 stage 过滤：最优价位上托管中的订单数没有上限，取几行都可能全是托管中的。

**已有库**：项目未上线，开发库清库重建即可。不清库时，`schemamigrate` 会补出 `on_book` 列，存量行都是 0；同名的盘口索引不会被改成新列序（漂移只核索引名）。存量挂单要等下一次重存（成交、撤单）才回到盘口上。

**回归测试**（都没有运行）：

- 单测 `go/trade/internal/data/commerce_repo_test.go`：`OnBook` 的判定表；`Save`、`Insert` 按 workflow 现算并写出 `on_book`，不信记录上的旧值；列清单、占位符、扫描三处对齐。
- 真库 `engine_tidb_test.go` 的 `TestTiDBOrderBookSkipsOrdersStillInEscrow`：价格更优的卖单还在托管时，买单照常与已上盘的卖单按 maker 价成交；托管中的订单自己不发起撮合、不发成交号；它托管完成、进盘口的那一步再发起撮合。
- 原有 `TestTiDBOrderBookPriceTimePartialCancelWaitsForChildren`、`TestRealDBUntradableCounterpartyIsTakenOffTheBook` 不用改：前者的订单都经 `advance` 进盘口，后者的坏对手单用 `Insert` 直接插入 `market_open` 的行，`on_book` 照样算出 1。

**Java 版**：待做，记在 `PARITY.md`。Java 的 `CommerceMatching` 选单与 `CommerceClientService` 深度查询是同样的写法。

## 16. 人民币寄售的商城权威实现批次（2026-10-21，实施中）

原 `CreateConsignment` 的游戏币价格协议保持原语义，人民币寄售另设明确入口，不能把原price悄悄解释为人民币分。托管与实例快照继续由trade可靠合同及scene SQL终局负责；商城只在已认证trade提交原托管合同引用／摘要后登记寄售条款。登记不等于新写一次玩家扣款，商户、买家或后台不能自报快照取得托管资格。

本批继续使用同一 `oms_order`／`oms_order_item`、payment receipt、delivery/refund intent及receipt。寄售专表只保存listing、原资产合同引用、冻结人民币条款和订单关联，不另造订单或游戏钱包。官方SKU的库存仍在PMS；唯一托管资产不伪造成可补库存的官方SKU。寄售订单数量恒1、CNY分、冻结卖家账号、价格、费率和冷静期；自买按账号拒绝，不凭角色号不同放行。账号由trade从已认证会话及SQL目录获得，不接受普通客户端声明归属。

自买的判定只在商城下单事务里（`RmbConsignmentStore.reserve`，先于建订单与占用listing），trade的购买入口不重复判。trade交付（`deliver`）里也比了买卖账号，那是纵深防御：买卖账号相同的订单建不出来，已付款意图正常走不到它。万一走到（商城那道判断被放开或绕过），`deliver`按「不能发货」处理，与买家角色已换主、买家账号已满同一条退款分支：记下退款决定，向商城回`REFUND_PENDING`，不发货给卖家自己的另一个角色，钱也不压在商城。2026-11-12之前这里每轮回`ErrConflict`，既不发货也不退款。商城`RmbConsignmentStoreTest`有专门用例守着下单入口；trade的回归测试是真库`TestTiDBRmbDeliverRefundsSelfPurchaseInsteadOfStalling`（未编译、未运行）。

上架后默认公示48小时、随后寄售7天。首次登记按服务端时钟冻结时限；同listing／托管引用换条款拒绝，重复登记返回原原件。购买锁listing并在同一商城事务写OMS与关联、复核公示/到期/卖家账号/当前占用；买家请求号重放返回原订单，不重新判断已变化的当前售价。取消未付单释放该listing，已经到期则进入RETURNING。已取消后才确认的付款沿原REFUND_PENDING分支收口，不把已重新占用的listing交给迟到付款者。

卖家撤单（`CancelRmbConsignment`）只记请求（`cancel_requested`、`status=6`），托管的退回由引擎推进。trade只在可撤阶段收（白名单`rmbCancellable`，2026-11-12起，未编译、未运行）；不能撤的回`kTradeOrderStateConflict`，合同行不变；白名单没列的阶段一律不能撤，新加的阶段默认不可撤。

| 阶段 | 能否撤 | 说明 |
|---|---|---|
| `rmb_escrow`、`rmb_freezing`、`rmb_ready`、`rmb_registering`、`rmb_listed` | 能 | 买家付款之前。`rmb_registering`先重放原登记再撤；`rmb_listed`要商城先把listing改成RETURNING，买家抢先付了款就以商城收款为准，撤单请求作废 |
| `rmb_return` | 能 | 已经在退回，重复请求照收 |
| `rmb_returned` | 算成功 | 已取消，不再写 |
| `rmb_delivery`、`rmb_refund` | 不能 | 买家已付款，结局由交付或商城退款决定 |
| `manual` | 不能 | 已转人工，只能由人工处置入口处理（§15.7） |
| `rmb_delivered` | 不能 | 已成交 |

交付仅接受原trade合同的SQL终局回执。确认DELIVERED的同一商城事务登记卖家收入：`fee=ceil(gross*fee_bps/10000)`、`net=gross-fee`，默认500bps；计算分解商／余数避免乘法溢出。冷静期默认3天，期间不可提现。永久失败退款不能同时登记收入；人工状态不猜结局，不自动夺回唯一资产。后续真实拒付／损失归属另按实际渠道方案，不把mock退款宣称为这项已实现。

卖家收入是每单不可变的人民币结算账，不复制C++游戏货币。提现以账号guard串行、锁定可用收入逐单分配，在同事务写唯一requestId的提现单和分配明细；预留后其它并发提现不能重复消费。渠道回执与原提现金额／币种绑定，成功只结一次，失败原路解冻分配。mock提现需要独立的持久mock付款事实，prod及未启用mock时拒绝；真实渠道默认关闭。提现、收入、原订单与各类回执均保留审计关联，页面或客户端无“直接置成功”入口。

先完成商城域、原订单接线和mock适配器，再补两版trade门面、角色冻结协同、客户端与真实到账验证。本节仅确立当前实现契约，不表示人民币寄售、提现或角色整链已经完成。V3已用于目录管理，V4用于mock支付退款，V5用于本批寄售／结算结构，后续迁移不复用这些编号。
