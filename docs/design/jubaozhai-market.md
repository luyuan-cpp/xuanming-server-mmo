# 聚宝斋(玩家间人民币寄售交易)设计 — 2026-09-14

> 状态:**P1 已落码(2026-09-15),未编译、未运行,待 Codex 按 PROGRESS 同日条目验证**;P0 / P2 起未开工。客户端界面已拼好(`mmorpg-client` 的 `Assets/Scripts/UI/Ugui/Jubaozhai/`、`Docs/JUBAOZHAI_UI.md`),本文件定义服务端与接线。
> 调研依据:2026-09-14 三路只读核查(资产原语 / 新服务接线 / 账号与 zone 模型),结论与证据摘要见 §2。

## 0. 结论(先读这节)

1. 聚宝斋 = 一个**全局 Go 服务 `trade`**(产品名聚宝斋)+ **C++ scene 上新增的三条资产 RPC** + **login 上的角色过户**。按 zone 分区还是全服,由服务端配置 `Market.Scope` 决定,商品上记 `market_zone`(卖家 home_zone,服务端查,不信客户端)。
2. 难点不在列表和界面,在"**东西和钱怎么安全换手**"。今天仓库里**没有**托管扣出、幂等发放、角色过户、登录冻结这四样原语(§2),必须先补。
3. 结算是人民币(J-1)。服务端只做**订单状态机 + 支付渠道接口**;本地和冒烟用 `mock` 渠道跑通,真实渠道、实名/防沉迷、卖家提现后接(P5)。
4. 首批四类商品都在范围(J-2),但按依赖分期:游戏币/装备道具/宠物共用一套资产通路(P2→P3);角色交易额外依赖"账号数据持久化"(P0-b→P4)。
5. **两道上线闸,不过不许开真钱交易**:GM 客户端消息无鉴权(能直接刷钱再卖钱);账号数据只在 Redis(角色过户无持久化落点)。

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
| J-O1 | 公示期 / 寄售期时长(默认 公示 48h、寄售 7d,可配) | **待产品确认** |
| J-O2 | 平台手续费率(默认 5%,向上取整到分),卖家收入冷静期(默认 3 天) | **待产品确认** |
| J-O3 | 角色交易:是否强制离帮(默认是)、好友关系是否清空(默认保留) | **待产品确认** |
| J-O4 | 可交易货币种类(默认只放 `kCurrencyGold`;元宝 `kCurrencyDiamond` 默认关,绑定元宝永不) | **待产品确认** |
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
| Go → scene 同步调用 | **已有**:`SceneNodeGrpc`(match `PrepareBattle` 范式,读 `player:%d:location` 寻址) | `proto/scene_manager/scene_node_service.proto`;`go/match/internal/logic/gather.go:282` |
| scene → Go | 只允许 SceneManager / DataService | `cpp/nodes/scene/main.cpp:78` |
| 账号 ↔ 角色 | 账号角色列表**只在 Redis `account:{account}`,TTL 12h,找不到 MySQL 写入**;反查键 `player_to_account:{id}` 永不过期且 EnterGame 会据此自愈回写 | `go/login/etc/login.yaml:68`;`entergamelogic.go:144-190` |
| 登录冻结 / 封禁 | **没有**任何字段或闸;踢人 `KickPlayerEvent` 只有 session_id,提示写死"顶号" | `gate_event_handler.cpp:160` |
| 角色名 / 等级对 Go 可见 | **无角色名**;等级只在 `level_component` blob,Go 读不到 | `entergamelogic.go:428-433` |
| GM 客户端消息 | `GmAddCurrency` / `GmGrantPet` / `GmSetPlayerLevel` 作为客户端消息开放、**无鉴权** | `proto/scene/player_currency.proto:65-73`;`player-pet.md:191-194` |
| 新服务客户端可达 | 只在路由服模式(`GATE_CLIENT_RPC_ROUTER=1`)可达;K8s 路由服 manifest 与 POD_IP 通告已补齐,默认仍为 0,待 K8s 路由模式 battle-smoke 通过后再切换 | `cpp/nodes/gate/main.cpp:206-209`;[zone 契约 §14](microservice-zone-contract-20260914.md) |
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
  - `zone`:浏览只返回 `market_zone = 买家 home_zone`;下单时服务端再校验一致(`kTradeZoneMismatch`)。
  - `global`:浏览不按分区过滤;客户端可传 `zone_filter`(0 = 全部)仅作筛选;允许跨区购买。
- 不照搬 guild `GetGuildRank` "客户端传 0 即全服"的做法:市场范围是服务端策略,不是客户端参数。
- 跨区交付天然成立:资产经 `player:%d:location` 找到买家当前所在 scene;角色过户不改 home_zone(映射是 SETNX 永不覆盖),买家选该区进入即可。
- 合服(**P1 已落**,`tools/merge_zone` 步骤 3b / 撤销 3b' / `verify:trade_listing`):按合服清单里的 listing_id 分批 `UPDATE trade_listing SET market_zone=dst WHERE market_zone=src AND listing_id IN (…)`(不用整区 UPDATE,否则清单落盘后新进源区的商品会被改写却不在清单里,撤销找不回);改写后复查源区计数仍 > 0 则中止不标完成;`seller_zone_at_listing` 不改。trade 库缺失时合服 fail-closed,只有显式 `-skip-trade-mysql`(dev_tools `-MergeSkipTradeMySql`)跳过。订单、流水、审计表保留原 zone 列(`server_merge_design.md` §6.4 口径)。
- 合服闸:上架、下单、交付前查 `merge:in_progress:{zone}`,读失败按"合服中"处理(fail-closed),照抄 `go/guild/internal/logic/merge_fence.go`。

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
   - 两条独立 seq 流:`DEBIT`(托管扣出)与 `CREDIT`(发放)。满足 D1 硬边界"扣减不进同一条 seq 流"。
   - Go 在写商品/订单的**同一个事务**里为 `(player_id, stream)` 分配单调 seq 并写 outbox 行(`trade_asset_op`)。
   - scene 持久化每条流的 `watermark` + 1024 位位图,落 `player_database` 下一个空闲字段号;加载、存盘、跨 zone 快照三处一起接。**不能按时间裁剪、不能只存内存**(否则重放双发)。
   - DEBIT 另存最近 8 条结果环(含资产快照),用于"scene 已扣但回包丢了"时重试取回快照。
4. **结局一旦决定就固定**:scene 见过某 seq,结局(`APPLIED` / `REJECTED`)落盘,重试返回同一结局。暂时性条件(背包满、战斗中、跨区冻结、玩家不在本节点)返回 `RETRY` / `NOT_HERE`,**不**记账。
5. **`TradeAbortDebit(seq)`**:scene 未见过该 seq 则记为 `REJECTED` 占位;见过则返回原结局。Go 靠它确定性收口任何悬挂的托管。
6. **只在玩家在线时应用**。Go 的 reconcile 循环扫描未决 outbox,按 `player:%d:location` 批量查在线者重投;离线玩家的发放等其上线,延迟 ≤ 扫描周期(默认 2s)。v1 不做登录时 scene 主动拉取;若日后要做,按契约 §8 走直连 gRPC(专用 READY 选择器 + `nodeTypeNameMap` + 前缀表 + scene 白名单),**不能借路由服**。
7. 重复投递安全(scene 幂等),所以多副本 reconcile 用行级租约只为省流量,不为正确性。

### 6.2 各类商品

| 类别 | 托管(DEBIT) | 交付(CREDIT) | 需要新写的原语 |
|---|---|---|---|
| **游戏币** | `DeductCurrency(kCurrencyGold, n)`,tx=`TX_AUCTION_SELL`,correlation=listing_id | `AddCurrency`,tx=`TX_AUCTION_BUY`;照常经过补缴抵扣(统一入口);GM 封禁 → 终局拒绝 → 退款 + 告警 | Currency 增减接口加 txType + correlation_id;区分"余额不足/冻结"错误码;可交易币种白名单(J-O4) |
| **装备道具** | 按 `item_uuid` 列表全或无扣出(一条商品最多 8 件,用于"套装");只允许主背包、非穿戴中;Item 表 `tradable=false` 一律拒(默认 false,fail-closed) | 以快照 `ItemEntry` 预设 guid 回包,保留原 `item_uuid` | `Bag::ReserveForBatchRemove` + `BagService::RemoveItemsByGuid`(全或无、拦冻结与战斗、带流水);Item 表加 `tradable` / `market_category` / `market_subcategory` 三列 |
| **宠物** | 按 `pet_id` 移出;出战中拒绝(`kTradePetActive`,提示先收回) | `GrantPet` 以快照 `PetInstance` 预设 `pet_id` 回填 | `PetSystem::RemovePetForTrade`;`GrantPet` 支持预设 id + 撞号检查;Pet 表加 `market_subcategory` 列(界面的 普通/灵兽/变异/神兽/元灵 与表里 quality 1-4 不同轴) |
| **角色** | 见 §6.3 | login `TransferPlayer` | 见 §6.3 |

scene 侧所有 DEBIT/CREDIT 入口统一拦:跨区冻结、`InBattleComp`(局中挡一切背包写,`moba-battle-target-architecture.md:113` 红线)、玩家不在本节点。

### 6.3 角色交易

- **交易的是角色,不是账号**:把角色从卖家账号挪到买家账号。账号级找回/改密与已售角色无关,规避"卖号后找回"。
- 前置 **P0-b**:账号角色列表今天只在 Redis(TTL 12h),必须先有持久化权威存储;冻结标记与归属写在**同一条记录**(`AccountSimplePlayer.trade_lock_listing_id`),过户时原子一起改。**不放共享 Redis 单独键**(共享库 allkeys-lfu 会淘汰,冻结丢失 = 公示期内卖家上线改角色)。
- 上架流程:
  1. 卖家以该角色在线发起;条件:非帮主且已离帮(J-O3)、不在战斗、角色数据已加载。
  2. Go 事务:商品 `ESCROWING` + DEBIT op(kind=`CHARACTER_LOCK`)。
  3. Go 调 login `LockPlayerForTrade(player_id, listing_id)`(幂等;此后 EnterGame 拒绝 `kTradeCharacterLocked`)。
  4. Go 调 scene `TradeDebit(CHARACTER_LOCK)`:scene 挂交易锁组件(拒绝该玩家一切客户端写消息)→ 存盘 → 从落盘后状态生成展示快照(职业、等级、属性摘要、宠物数、游戏币、装备摘要)→ 带原因踢下线。
  5. 快照入库,商品转 `PUBLIC_NOTICE`。
- 下架/过期:login `UnlockPlayerForTrade`。
- 交付:login `TransferPlayer(tx_id, player_id, from_account, to_account)`:
  - 按账号字典序拿两把 `account_lock:create:{acc}`,再拿 `player_locker:{id}`;
  - 校验卖家仍拥有且锁的是本 listing、买家角色数 < `MaxPlayersPerAccount`;
  - 同时改两账号记录 **和 `player_to_account:{id}`**(不改的话卖家 EnterGame 会自愈把角色写回卖家);清锁;按 tx_id 幂等。
  - 买家角色位满:下单时就拒;交付时满则保持 `DELIVERING` 并提示腾位,超过期限(默认 3 天)退款。
- 需要顺带改:`KickPlayerEvent` 加原因字段,gate 不再写死"顶号"提示;`RemovePlayersFromAccounts` 补账号锁(与过户/建角并发会互相覆盖)。
- 买卖双方身份取**账号**:`player:session:{id}` 的 `account` 字段;同账号不能自买。

### 6.4 `SceneNodeGrpc` 新增 RPC(草案)

```proto
rpc TradeDebit(TradeDebitRequest) returns (TradeDebitResponse) {}
rpc TradeAbortDebit(TradeAbortDebitRequest) returns (TradeDebitResponse) {}
rpc TradeCredit(TradeCreditRequest) returns (TradeCreditResponse) {}

enum TradeAssetOpOutcome { TRADE_OP_UNKNOWN = 0; TRADE_OP_APPLIED = 1; TRADE_OP_REJECTED = 2; TRADE_OP_RETRY = 3; TRADE_OP_NOT_HERE = 4; }

message TradeDebitRequest {
  uint64 player_id = 1;
  uint64 seq = 2;            // DEBIT 流 seq,Go 分配
  uint64 correlation_id = 3; // listing_id,进 transaction_log
  oneof asset {
    TradeCurrencyAmount currency = 10;
    TradeItemGuids items = 11;     // repeated uint64 item_uuid,≤8
    uint64 pet_id = 12;
    TradeCharacterLock character = 13;
  }
}
message TradeDebitResponse {
  TipInfoMessage error_message = 1;
  TradeAssetOpOutcome outcome = 2;
  TradeAssetSnapshot snapshot = 3; // APPLIED 时必填;重试从结果环取回
}
message TradeCreditRequest {
  uint64 player_id = 1;
  uint64 seq = 2;            // CREDIT 流 seq
  uint64 correlation_id = 3; // order_id(退回卖家时为 listing_id)
  TradeAssetSnapshot asset = 4;
}
```

`TradeAssetSnapshot` 为 oneof `{ currency; repeated ItemEntry items; PetInstance pet; TradeCharacterSummary character }`,定义放 `proto/common/`,Go 与 C++ 共用,避免并行 struct(AGENTS §3)。

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
- **D-14 第 9 条上线清单**:`trade_listing.market_zone` 带 home_zone 语义 → `tools/merge_zone` 需改写步骤(P1 同批落);audit auditor、data_consistency_check、TiDB BR 按库恢复清单列为 **P3 上线前**必做项(P1 只有 dev 种子数据,无玩家资产)。
- 客户端可达只承诺路由服模式;K8s 路由服 manifest 与 POD_IP 通告已补齐,但尚未在 K8s 上以路由模式跑通 battle-smoke,默认仍为 0。
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

1. **GM 客户端消息鉴权**(P0-a):`GmAddCurrency` / `GmGrantPet` / `GmSetPlayerLevel` 等要么关闭、要么鉴权。不收口 = 刷出资产直接卖人民币。
2. **账号数据持久化**(P0-b):角色交易的前置,见 §6.3。
3. **实名 / 防沉迷 / 未成年人消费限制**:`BuyerEligibility` 接入真实实现。
4. **经营与支付合规**:人民币虚拟物品交易平台涉及的资质、运营主体与支付签约由业务/法务确认,工程侧不能替代。
5. **风控**:同账号自买自卖拒绝;价格上下限;单账号在售数上限;新获得物品上架冷却(需要 `ItemEntry` 获得时间,P3 后);异常高频下单限速(gate 每消息号 3 次/秒是第一道)。
6. **回档交互**:`single_player_rollback` "有合法交易流水的转移排除在恢复范围之外";所有托管/交付写 transaction_log,correlation_id = listing_id / order_id。

## 13. 分期与验收

| 期 | 内容 | 依赖 | 验收(由 Codex 执行) |
|---|---|---|---|
| **P0-a** | GM 客户端消息鉴权收口 | — | robot 发 `GmAddCurrency` 被拒;GM 通道仍可用 |
| **P0-b** | 账号角色列表持久化(login) | — | 清空 Redis 后角色列表不丢;并发建角/删除不互相覆盖 |
| **P1** | proto(客户端 4 方法 + 内部 SeedListing + 表结构)+ tip 段 + `go/schemamigrate`(D-14 抽取 + 修缺陷)+ `go/trade`(浏览/详情/收藏/货架/dev 种子)+ `-migrate` 与 K8s Job + 登记 + `tools/merge_zone` 改写步骤 + 客户端服务端分页接线 + robot `trade-smoke` | — | schemamigrate 单测含"后加表被建出";trade 单测含"带会话调 TradeAdmin 被拒";`TestNoHandWrittenTipCodes` 通过;robot `trade-smoke` 在 `zone` 与 `global` 各跑一次输出 `TRADE_SMOKE_OK`;客户端按 U 打开拉到服务端种子商品,收藏重进角色后仍在 |
| **P2** | C++ 资产原语:账本组件 + 持久化;`TradeDebit/AbortDebit/Credit`;Bag 按 guid 全或无;Currency txType;Pet 移出/预设回填;Item/Pet 表新列 | P1 proto | 单测覆盖:重复 seq 返回同一结局、APPLIED 后响应丢失重试取回快照、Abort 占位、战斗中/跨区冻结返回 RETRY、全或无不部分扣 |
| **P3** | 游戏币/装备道具/宠物 端到端:上架→公示→寄售→下单→mock 支付→交付→卖家入账→冷静期;下架/过期回退;reconcile | P2 | robot:卖家在 zone1、买家在 zone2 完成三类交易;买家离线付款后上线自动到账;重复支付回调不重复交付;kill trade 进程中途重启后状态收敛 |
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
