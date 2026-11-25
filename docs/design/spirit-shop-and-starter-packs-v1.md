# 《五行奇谈》P0 官方灵石百宝阁与绑定/终身限购首套提速包设计（V1）

> 日期：2026-11-25  
> 状态：**设计方案落盘，待负责人确认定价、货架与并行合入时机后进入代码实现**（本轮纯设计文档，未改动任何代码、协议、配置表或并行会话未提交文件）  
> 上位红线：[项目设定](project-vision.md)（好玩优先 / 不肝防多开 / 小小氪金 / 官方 7:3 / 物价稳得住 / 防骗与资金安全）  
> 衔接文档：[经济与官方 7:3 总纲](equipment-economy-charter.md) · [装备品相与率土式制书 V1](equipment-quality-and-skill-crafting-v1.md) · [赛季×强化统一落地](season-pvp-pve-economy-unified-v1.md) · [挂机月卡](afk-monthly-card.md) · [真钱充值设计](payment-recharge.md) · [神秘商人](mystery-merchant.md) · [客户端请求幂等](client-request-idempotency.md)

---

## 1. 需求分析

### 1.1 当前创收闭环的核心卡点（基于 2026-11-25 仓库现状核对）

仓库已经落地了大量高价值基础设施与玩法消耗端，但“玩家付钱 → 拿到对应商品”的**最后一公里货架**仍处于断裂状态：

1. **玩法侧付费配套件已经落码，但游戏内无货架可买**：
   - `data/Item.xlsx` 与场景业务已完成 `3405 定星保底符`（装备 5 档词条打磨必出极品）、`3408 天工秘墨`（率土式稀世绝学定向制书）、`3402 角色形象卡`（10 元永久衣柜买断）与 `3401 改名卡` 的消费逻辑；
   - 玩家充值灵石（`kCurrencyDiamond = 1`）后，场景内目前仅能开通基础版 `300 灵石` 挂机月卡（`BuyAfkPass`），**缺乏一个统一的「官方灵石直购百宝阁」把上述核心商品变现**。
2. **经济总纲拍板的「6–30 元绑定首套提速材料包（终身限购）」尚无载体**：
   - [equipment-economy-charter.md](equipment-economy-charter.md) 与 [gameplay-unified-design-v1.md](gameplay-unified-design-v1.md) 已明确：官方需直售永久绑定的首套修行材料提速包（锚定 `0.1 元/份` 参考价，终身限购总计 300 份，既兑现“上班族小额省时提速”，又作为稳物价的**价格上限锚**防止大商恶意炒高市场材料）；
   - 现役 `java/mall_game` 的 `commerce_sku_grant` 表（`java/mall_game/src/main/resources/db/schema.sql`）仅支持发放单种普通物品或货币，**不支持绑定标记（`bound = true`）、不支持双物品打包直发、也不支持每角色终身/共享组限购**。
3. **外观拉收入边界需同步定死，避免低 ROI 需求分流研发与美术**：
   - 负责人已于 2026-11-25 确认：**砍掉宝宝头像框**；**人物头像框不进商城单卖**（仅作高难首通/赛季免费荣誉及后续月卡/首充附赠排面）；
   - 官方外观创收继续聚焦已有 **10 元永久人物衣柜**（`3402`），二期再扩展**宝宝异色/幻化皮肤**与**招式/擂台入场特效**。

### 1.2 设计目标与硬约束

- **最小工程闭环，最快变现**：彻底复用既有的**真钱充灵石通道**（`go/payment` + `java/mall_game` + `go/trade`）与 **scene 单事务原子交换原语**（`BagService::ExchangeInventoryExact` + `ClientRequestLedgerComp` + `PlayerLifecycleSystem` 直提存盘），**不新建微服务、不改 `java/mall_game` 表结构、不新增线程、不碰当前脏表 `data/Item.xlsx`**。
- **全端天然合规与免商店重审**：App Store / Google Play / 微信小游戏 / 抖音小游戏仅需注册固定几个通用灵石充值档位，游戏内商品上架、调价、限购、绑定直发全部靠配置表热更驱动。
- **零氪同上限 + 防洗白**：百宝阁所有加速与配套道具一律 `bound = true`（仅自用，不可挂集市/摆摊/当面转卖）；所有商品 0 独占战力上限、0 抽卡、0 每日打卡；首套提速包采用**共享组终身总配额封顶**，不叠加、不超发。
- **满足 [项目设定](project-vision.md) 四问准入自检**（见 §2.7）。

---

## 2. 设计分析

### 2.1 总体架构：双段闭环（官方真钱充灵石 → 场景内灵石百宝阁）

沿用 [挂机月卡 §4.2](afk-monthly-card.md#42-真钱走现有充值档位) 与 [真钱充值设计 §0 / §4](payment-recharge.md#0-一句话结论) 的同一套架构边界：

```text
[玩家点击购买缺灵石]
        │
        ▼ (客户端一键引导至对应灵石充值档位)
┌────────────────────────────────────────────────────────┐
│ 第一段：真钱买灵石（100% 复用既有全端合规链路，零改动）     │
│ 客户端 → go/trade(下单) → go/payment(7渠道核验)          │
│       → java/mall_game(订单权威) → go/trade → C++ scene │
│       → kCurrencyDiamond(灵石) 入账 / 撤销走原欠款扣回   │
└────────────────────────────────────────────────────────┘
        │
        ▼ (灵石到账，玩家回到百宝阁点击购买)
┌────────────────────────────────────────────────────────┐
│ 第二段：场景内「灵石百宝阁」（本次 P0 新增核心）            │
│ 客户端 → SceneSpiritShopClientPlayer.BuySpiritShopGoods │
│       → C++ scene 单线程同步原子执行：                    │
│         ① 幂等账本查重 (ClientRequestLedgerComp)         │
│         ② 报价防呆 + 限购/共享组 + 封禁 + 余额 + 空间预检 │
│         ③ ExchangeInventoryExact(发绑定物品)             │
│            └─ beforeCommit 同步：                        │
│               扣灵石 + 记个人/组限购 + 记幂等键           │
│         ④ 同一次 TiDB 直提存盘原子落库（I3 不变量）       │
└────────────────────────────────────────────────────────┘
```

#### 灵石充值档位配套建议（纯运维配置，不改代码）
[payment-recharge.md §4](payment-recharge.md#4-商品与钱包模型) 现有默认 6 档为 `¥6(60) / ¥30(300) / ¥68(680) / ¥128(1280) / ¥328(3280) / ¥648(6480)`。为精准承接 10–24 元档小额消费（减少玩家“买 18 元包被迫充 30 元”的心理阻力），建议在商店上架配置中启用/补齐以下小额友好档位（走现有 `mall-admin` 或 seed 配置，无代码改动）：

| 建议档位 | 标价（分） | 到账灵石 | 典型承接场景 |
|---|---:|---:|---|
| `diamond_60` | 600（¥6） | 60 | 6 元初阶首套提速包（1:1 刚好） |
| `diamond_120` *(可选增补)* | 1200（¥12） | 120 | 1 张定星保底符（120），或 1 张形象卡/改名卡（100）+ 余 20 |
| `diamond_180` *(推荐增补)* | 1800（¥18） | 180 | 18 元中阶首套提速包（1:1 刚好） |
| `diamond_300` | 3000（¥30） | 300 | 30 元半套提速包 / 30 元月卡 / 1 瓶天工秘墨（240）+ 余 60 |
| `diamond_680` / `diamond_1280` | 6800 / 12800 | 680 / 1280 | 多角色收藏 + 多本稀世绝学中氪便捷充值 |

---

### 2.2 首批货架 SKU 与定价清单（8 项核心入口，零新增道具表行）

所有商品**直接引用 `data/Item.xlsx` 既有道具 ID**，并通过表内双物品槽位直接向背包原子发放绑定堆（**无需新增“礼包箱子”道具行，无需新增“开箱使用道具”协议，且零触碰当前并行脏表 `data/Item.xlsx`**）：

| `goods_id` | 分页 `tab_id` | 展示名 | 货币与单价 | 发放物品（全 `bound = true`） | 限购规则 | 商业化定位 |
|---:|---|---|---:|---|---|---|
| **101** | 1 · 首套提速 | **初阶修行提速包** | 灵石 **60**（¥6） | `3001 打造材料`×30 + `3002 百炼石`×30（共 60 份） | 共享组 `group_id = 1`，单份占权重 **60**（组终身封顶 **300**） | 6 元破冰首单、试水省时 |
| **102** | 1 · 首套提速 | **中阶修行提速包** | 灵石 **180**（¥18） | `3001 打造材料`×90 + `3002 百炼石`×90（共 180 份） | 共享组 `group_id = 1`，单份占权重 **180**（组终身封顶 **300**） | 18 元主力省时档 |
| **103** | 1 · 首套提速 | **半套毕业提速包** | 灵石 **300**（¥30） | `3001 打造材料`×150 + `3002 百炼石`×150（共 300 份） | 共享组 `group_id = 1`，单份占权重 **300**（组终身封顶 **300**） | 30 元一键省半套进度 |
| **201** | 2 · 精进配套 | **定星保底符** | 灵石 **120**（¥12） | `3405 定星保底符`×1 | 不限购（单次 1–20） | 装备 5 档品相打磨 70% 官方直收 |
| **202** | 2 · 精进配套 | **天工秘墨** | 灵石 **240**（¥24） | `3408 天工秘墨`×1 | 不限购（单次 1–10） | 率土式稀世绝学制书 70% 官方直收 |
| **301** | 3 · 外观便利 | **角色形象卡** | 灵石 **100**（¥10） | `3402 角色形象卡`×1 | 不限购（单次 1–10） | 10 元永久衣柜买断统一购买入口 |
| **302** | 3 · 外观便利 | **角色改名卡** | 灵石 **100**（¥10） | `3401 改名卡`×1 | 不限购（单次 1–5） | 常规社交便利消费 |
| *(聚合卡片)* | 3 · 外观便利 | **仙途轻享卡（月卡）** | 灵石 **300**（¥30） | 不进百宝阁表，前端置顶卡片直调既有 `BuyAfkPass` | 沿用 180 天叠加上限 | 聚拢所有灵石消费于一窗，P1 升级零改动 |

#### 关键业务规则口径
1. **共享组终身限购机制（`limit_group_id = 1`，默认封顶 `300` 份）**：
   - 玩家先花 ¥6 试水买 `101`（占 60/300，剩 240），觉得好用再花 ¥18 买 `102`（占 180，已用 240/300，剩 60），最后还可再补 1 次 `101` 凑满 300/300；此时 `101 / 102 / 103` 全部显示「已达终身限购上限」，不可再买。
   - 若未来首测数据显示需要放宽至整套 `600` 份，**只需热更 `SpiritShopLimitGroup.xlsx` 第 1 行的 `max_weight`（300 → 600）**，零代码改动、历史已用额度自然继承。
2. **严禁绑定灵石混扣**：
   - 首批 7 款商品 `currency_type` 全部配为 `1`（`CURRENCY_TYPE_DIAMOND`，真实充值灵石），服务端严格按 `row.currency_type()` 扣款，**绝不拿活动/补偿赠送的 `CURRENCY_TYPE_BIND_DIAMOND (2)` 兜底抵扣**，守住官方 70% 真实营收与市场价格锚。
3. **强制 `bound = true` 防洗白**：
   - 灵石百宝阁出售的所有加速材料与配套道具均标 `item_bound = 1`；即使该商品发放的是可交易模板 `3001 / 3002`，入包时也由 `InitItemParam.itemPBComp.set_bound(true)` 强制落入绑定堆，遵循 `item_store` 按 `(config_id, bound)` 严格分堆规则，**绝不与玩法产出的可交易堆合堆洗白**。
   - 角色形象卡（`3402`）与改名卡（`3401`）同样设 `bound = true`（仅自用），杜绝黑卡买卡后挂市场套现。
4. **外观边界红线（永久写入本规范，防止后续跑偏）**：
   - **不做宝宝头像框**；
   - **人物头像框不进灵石百宝阁单卖**，只留作高难首通/赛季荣誉与后续月卡/首充附赠排面；
   - 百宝阁外观分页一期只卖 `3402 角色形象卡`，二期预留接入**宝宝异色/幻化皮肤**与**招式/擂台入场特效**。
5. **不放“首充双倍 / 限时弹窗倒计时 / 抽奖转盘”**：
   - 严格遵守 [项目设定](project-vision.md) 与 [真钱充值设计 §1](payment-recharge.md#1-目标与非目标)，保持体面克制的明码标价货架。

---

### 2.3 配置表 Schema 设计（2 张新表，零修改既有脏表）

遵循 [data/AGENTS.md](../../data/AGENTS.md) 与 [docs/rules/proto-and-tables.md](../rules/proto-and-tables.md)，源表与权威 schema 成对新增：

#### 表 1：`data/schema/spiritshoplimitgroup_table.proto`（源表 `data/SpiritShopLimitGroup.xlsx`）
```proto
syntax = "proto3";
package mmorpg.cfgtable.v1;
import "cfg_options.proto";

message SpiritShopLimitGroupTable {
  option (cfg_sheet)       = "SpiritShopLimitGroup";
  option (cfg_source_file) = "SpiritShopLimitGroup.xlsx";
  option (cfg_primary_key) = "id";
  option (cfg_hot_reload)  = true;

  // 共享限购组 ID（上线后不复用旧 ID，只增不改义）
  uint32 id = 1;
  // 展示名称（如「首套修行提速终身限额」）
  string name = 2;
  // 重置周期：0 = 终身不重置；1 = 每周重置（UTC+8 周一 05:00，与装备材料二阶段同口径）
  uint32 reset_kind = 3;
  // 该组总可用权重上限（如首套提速 = 300 份，必填 >= 1）
  uint32 max_weight = 4;
}
```

#### 表 2：`data/schema/spiritshopgoods_table.proto`（源表 `data/SpiritShopGoods.xlsx`）
```proto
syntax = "proto3";
package mmorpg.cfgtable.v1;
import "cfg_options.proto";

message SpiritShopGoodsTable {
  option (cfg_sheet)       = "SpiritShopGoods";
  option (cfg_source_file) = "SpiritShopGoods.xlsx";
  option (cfg_primary_key) = "id";
  option (cfg_hot_reload)  = true;

  // 商品 ID（上线后只停用不复用旧 ID；停用将 enabled 置 0）
  uint32 id = 1;
  // 分页：1 = 首套提速，2 = 精进配套，3 = 外观便利
  uint32 tab_id = 2;
  // 展示排序（升序）
  uint32 sort_order = 3;
  // 上架开关：0 = 下架/停售，1 = 在售
  uint32 enabled = 4;
  // 商品展示名（留空时仅允许单物品商品回退物品表名）
  string name = 5;
  // 商品简述（明码写清绑定仅自用、不加战力上限等提示）
  string description = 6;
  // 图标资源键（留空回退 item_id_1 的 icon_key）
  string icon_key = 7;
  // 结算货币：仅允许合法 CurrencyType（首批核心创收项统一填 1 = 灵石）
  uint32 currency_type = 8;
  // 单份价格（1..1000000000，防乘法溢出）
  uint64 price = 9;
  // 单次请求最大购买份数（1..99）
  uint32 max_buy_per_request = 10;
  // 物品 1（必填）：FK -> Item.id
  uint32 item_id_1 = 11 [(cfg_fk) = "Item"];
  // 物品 1 每份数量（>= 1）
  uint32 item_count_1 = 12;
  // 物品 1 是否绑定：1 = 绑定仅自用（推荐强制为 1），0 = 非绑定
  uint32 item_bound_1 = 13;
  // 物品 2（可选，0 = 无第二个物品）：非 0 时服务端启动/热更强校 Item.Exists
  uint32 item_id_2 = 14;
  // 物品 2 每份数量（item_id_2 > 0 时必须 >= 1）
  uint32 item_count_2 = 15;
  // 物品 2 是否绑定：1 = 绑定，0 = 非绑定
  uint32 item_bound_2 = 16;
  // 单商品个人限购重置周期：0 = 终身，1 = 每周（UTC+8 周一 05:00）
  uint32 limit_reset_kind = 17;
  // 单商品限购份数（0 = 不限购；>0 = 每周期或终身最多买多少份）
  uint32 limit_count = 18;
  // 所属共享限购组 ID（0 = 不参与共享组；>0 时须存在对应 SpiritShopLimitGroup 行）
  uint32 limit_group_id = 19;
  // 买 1 份消耗的共享组权重（limit_group_id > 0 时必须 >= 1，如 60 / 180 / 300）
  uint32 limit_group_weight = 20;
  // 客户端角标标签（如「终身限额」「热卖」「保底配套」，可为空）
  string badge_text = 21;
}
```

> **启动与热更校验（fail-closed）**：`PlayerSpiritShopSystem::ValidateTables()` 在节点启动及表热更后校验每一行 `enabled == 1` 的商品：货币合法、价格在 `1..10^9`、`max_buy_per_request` 在 `1..99`、`item_id_1` 存在且 `item_count_1 > 0`、`item_id_2` 若非 0 则存在且不能与 `(item_id_1, item_bound_1)` 重复碰撞、若 `limit_group_id > 0` 则组行存在且 `limit_group_weight >= 1`；非法行打英文 `LOG_ERROR` 且运行时**拒绝下发与拒绝购买**（不崩服、不放行坏配置）。

---

### 2.4 持久化组件、幂等账本、流水与 Tip 契约（已核对当前真实空闲号）

所有编号均按 2026-11-25 仓库现役源码核对完毕，落码前仍按当时最新 `HEAD` 复核一次：

1. **玩家限购持久化组件**（新增 `proto/common/component/player_spirit_shop_comp.proto`）：
   ```proto
   syntax = "proto3";
   option go_package = "common/component";

   message SpiritShopGoodsPurchaseRecord {
     uint32 goods_id = 1;
     uint32 lifetime_bought = 2;
     uint64 week_starts_at_ms = 3; // UTC+8 周一 05:00，防时钟回拨
     uint32 weekly_bought = 4;
   }

   message SpiritShopGroupQuotaRecord {
     uint32 group_id = 1;
     uint32 lifetime_used = 2;
     uint64 week_starts_at_ms = 3;
     uint32 weekly_used = 4;
   }

   message PlayerSpiritShopComp {
     // 条目间无序，按 goods_id / group_id 查找（AGENTS.md §11.6）
     repeated SpiritShopGoodsPurchaseRecord goods_records = 1;
     repeated SpiritShopGroupQuotaRecord group_records = 2;
     // 每次百宝阁成交递增 1
     uint64 shop_revision = 3;
   }
   ```
   - 挂载位置：`proto/common/database/mysql_database_table.proto` 的 `player_database` 末尾追加下一个空闲号 **`34`**：
     ```proto
     PlayerSpiritShopComp spirit_shop = 34 [(OptionPersistGroup) = 7]; // WALLET
     ```
   - **归组理由**：放 `WALLET(7)` 组，与 `currency`、`client_request_ledger` 同行，每次购买原本就要写 `WALLET` 行，**零额外行写放大**，且与 `BAG(8)` 在同一次 TiDB 存盘事务提交，严格满足 I3 原子性不变量。
   - **ECS 组件类别**：**持久数据**（`ecs-component-access-rules.md`）——`PlayerDatabaseMessageFieldsUnmarshal` 无条件挂载，旧档缺失视为空合法；存盘缺组件整份拒存（fail-closed）；业务路径一律 `try_get` 判空，严禁懒建空组件清空限购。
   - **容量上限守护**：校验 `goods_records.size() <= 512`、`group_records.size() <= 128` 且无重复 `goods_id` / `group_id`，防止异常存档无限膨胀。

2. **客户端请求幂等账本**：
   - `cpp/libs/services/scene/player/system/client_request_ledger.h` 的 `ClientRequestKind` 末尾追加 **`kBuySpiritShopGoods = 15`**（同时同步 `kClientRequestKindNames` 通过 `static_assert`）。
   - **无需新增 `ClientRequestRecord` 字段**：与 `kBuyMysteryMerchantGoods` 相同，重放时返回最新 `SpiritShopInfo` + 全量 `BagInfo` + `replayed = true`。
   - **强制非 0 幂等键**：新协议没有历史老客户端兼容包袱，与 `BuyAfkPass` 同样强制 `client_request_id != 0`（为 0 直接拒 `kInvalidParameter`）。

3. **审计流水**：
   - `proto/common/rollback/transaction_log.proto` 的 `TransactionType` 末尾追加 **`TX_SPIRIT_SHOP_BUY = 44`**。
   - `correlation_id = client_request_id`，扣灵石流水与发物流水共享同一 `correlation_id`，物品流水 `extra` 写紧凑 JSON：`{"source":"spirit_shop","goods_id":101,"count":1,"unit_price":60,"currency_type":1,"bound":true}`，便于财务对账、官方 7:3 营收统计与回档精查。

4. **Tip 错误码新域**（`data/tip/Tip.xlsx` 追加独立段 `//spirit_shop_error base=37000 width=1000`，36000 已由 `festival_beast_error` 占用）：

   | 码名 | 中文提示文案 | `fault` |
   |---|---|---|
   | `kSpiritShopUnavailable` | 灵石百宝阁暂不可用，请稍后再试 | `1` |
   | `kSpiritShopGoodsOffShelf` | 该商品已下架或暂不出售 | |
   | `kSpiritShopPriceChanged` | 商品价格或内容已更新，请刷新后再试 | |
   | `kSpiritShopInvalidCount` | 购买数量不合法 | |
   | `kSpiritShopLimitReached` | 该商品已达限购上限 | |
   | `kSpiritShopGroupLimitReached` | 该类提速包已达终身限购总额 | |
   | `kSpiritShopCurrencyNotEnough` | 灵石不足，请先充值灵石 | |
   | `kSpiritShopBagFull` | 背包空间不足，请清理后再试 | |
   | `kSpiritShopFrozen` | 角色当前状态无法购买，请稍后再试 | |
   | `kSpiritShopInBattle` | 战斗中无法购买，请待战斗结束后再试 | |
   | `kSpiritShopItemBlocked` | 该物品当前被禁止获取 | |

---

### 2.5 客户端协议（新增 `proto/scene/player_spirit_shop.proto`）

保持极简，仅 2 条 RPC，客户端零配表，所有商品名、描述、绑定提示、价格、个人及共享组限额全由服务端下发：

```proto
syntax = "proto3";
option go_package = "scene";

import "proto/db/proto_option.proto";
import "proto/common/base/tip.proto";
import "proto/scene/player_bag.proto";

option cc_generic_services = true;
option (OptionFileDefaultNode) = NODE_SCENE;

message SpiritShopItemEntry {
  uint32 item_config_id = 1;
  uint32 item_count = 2;
  bool bound = 3;
}

message SpiritShopLimitGroupView {
  uint32 group_id = 1;
  string name = 2;
  uint32 reset_kind = 3;     // 0 = 终身，1 = 每周
  uint32 max_weight = 4;     // 总上限（如 300）
  uint32 used_weight = 5;    // 请求者已用权重
  uint64 next_reset_ms = 6;  // reset_kind == 1 时下一次重置时间戳（UTC 毫秒），终身填 0
}

message SpiritShopGoodsView {
  uint32 goods_id = 1;
  uint32 tab_id = 2;
  uint32 sort_order = 3;
  string name = 4;
  string description = 5;
  string icon_key = 6;
  string badge_text = 7;
  uint32 currency_type = 8;
  uint64 unit_price = 9;
  uint32 max_buy_per_request = 10;
  repeated SpiritShopItemEntry items = 11; // 1..2 项，顺序与配表一致
  uint32 limit_reset_kind = 12;
  uint32 limit_count = 13;     // 0 = 不限购
  uint32 bought_count = 14;    // 本周期或终身已购份数
  uint32 limit_group_id = 15;  // 0 = 无共享组
  uint32 limit_group_weight = 16;
  uint64 next_reset_ms = 17;
}

message SpiritShopInfo {
  uint64 server_time_ms = 1;
  uint64 shop_revision = 2;
  uint64 diamond_balance = 3;       // 当前灵石余额（kCurrencyDiamond = 1），只供展示
  uint64 bind_diamond_balance = 4;  // 当前绑定灵石余额（只供界面区分提示，防玩家误以为绑定灵石可付）
  repeated SpiritShopLimitGroupView limit_groups = 5;
  repeated SpiritShopGoodsView goods = 6;
}

message GetSpiritShopInfoRequest {}
message GetSpiritShopInfoResponse {
  TipInfoMessage error_message = 1;
  SpiritShopInfo info = 2;
}

message BuySpiritShopGoodsRequest {
  uint64 client_request_id = 1;         // 必填非 0 幂等键
  uint32 goods_id = 2;
  uint32 count = 3;                     // 1..max_buy_per_request
  // 报价防呆：与玩家看到的不一致（热更改价/改内容/改绑定态）一律拒回 kSpiritShopPriceChanged
  uint32 expected_currency_type = 4;
  uint64 expected_unit_price = 5;
  repeated SpiritShopItemEntry expected_items = 6;
}

message BuySpiritShopGoodsResponse {
  TipInfoMessage error_message = 1;
  SpiritShopInfo info = 2; // 成功时返回最新货架与余额
  BagInfo bag = 3;         // 成功时返回全量背包
  bool replayed = 4;       // true = 幂等重放命中首次成功记录
}

service SceneSpiritShopClientPlayer {
  option (OptionIsPlayerService) = true;
  option (OptionIsClientProtocolService) = true;

  rpc GetSpiritShopInfo (GetSpiritShopInfoRequest) returns (GetSpiritShopInfoResponse);
  rpc BuySpiritShopGoods (BuySpiritShopGoodsRequest) returns (BuySpiritShopGoodsResponse);
  // ── 以下只许在末尾追加 ──
}
```

---

### 2.6 服务端同步购买原子执行流（零 `[UNPAID_GRANT]` 窗口）

`PlayerSpiritShopSystem::BuyGoods` 在 scene 主 EventLoop 同步单线程执行，严格按以下不可重排顺序：

1. **参数与幂等查重（排在一切状态预检之前）**：
   - 校验实体有效、`client_request_id != 0`、无未知字段；
   - 计算 `ClientRequestFingerprint(request)`，调用 `client_request_dedup::Check(player, clientRequestId, ClientRequestKind::kBuySpiritShopGoods, fingerprint, first)`：
     - `kReplay` → 设 `replayed = true`，填充当前 `SpiritShopInfo` 与 `BagInfo`，返回 `kSuccess`（即使之后余额不足或限购已满仍如实返回已成）；
     - `kMismatch` → 返回 `kInvalidParameter`；
     - `kUnavailable` → 返回 `kServiceUnavailable`（fail-closed）。
2. **可写闸与战斗闸**：
   - `!player_mutability::IsPlayerMutable(player)` → `kSpiritShopFrozen`；
   - `PlayerBattleSystem::IsInBattle(player)` → `kSpiritShopInBattle`（避免战斗快照期间改动背包）。
3. **查表校验与报价防呆**：
   - 查 `SpiritShopGoodsTableManager::FindById(request.goods_id())` 并复用 `ResolveSellableGoods` 校验 `enabled == 1` 与整行合法；不存在或下架 → `kSpiritShopGoodsOffShelf`；
   - `request.count() == 0 || request.count() > goods.maxBuyPerRequest` → `kSpiritShopInvalidCount`；
   - 比对 `expected_currency_type`、`expected_unit_price` 与 `expected_items`（条数、顺序、`item_config_id`、`item_count`、`bound` 逐项严格相等），任一不符 → `kSpiritShopPriceChanged`。
4. **限购预检（含共享组 + 抗时钟回拨）**：
   - `try_get<PlayerSpiritShopComp>(player)` 为空 → 打 `LOG_ERROR` 返回 `kSpiritShopUnavailable`（绝不懒建）；
   - 单商品限购：若 `limit_count > 0`，按 `limit_reset_kind`（终身直接取 `lifetime_bought`；每周走 `ResolveWeek(nowMs)`，遇时钟回拨 `weekStart < record.week_starts_at_ms()` 保守按本周期已用算不重置），超限 → `kSpiritShopLimitReached`；
   - 共享组限购：若 `limit_group_id > 0`，计算 `addWeight = limit_group_weight * count`（64 位防溢出），若 `usedWeight + addWeight > group.max_weight()` → `kSpiritShopGroupLimitReached`。
5. **封禁、余额与背包空间预检**：
   - 逐项校验 `GainBlockService::IsItemGainBlocked(player, item.itemConfigId)` → `kSpiritShopItemBlocked`；
   - 计算 `totalCost = unitPrice * count`（防溢出），`!CurrencySystem::CanAfford(player, currencyType, totalCost)` → `kSpiritShopCurrencyNotEnough`；
   - 构造 `std::vector<InitItemParam> gained`（每项显式设 `item_id = kInvalidGuid`、`size = itemCount * count`、`bound = item.bound`）。
6. **原子兑换提交（`BagService::ExchangeInventoryExact` + `beforeCommit`）**：
   - 预先在临时副本构造 `candidateShop`（更新个人/共享组计数与 `shop_revision + 1`）与 `candidateLedger`（记住 `client_request_id`），先验证 `client_request_ledger::Validate(candidateLedger).empty()`；
   - 调用 `BagService::ExchangeInventoryExact(player, /*consumed=*/{}, gained, /*replacements=*/{}, clientRequestId, extraJson, written, beforeCommit)`：
     - `ExchangeInventoryExact` 先在背包候选副本完成全部分堆空间规划、禁发复核与物品 ID 铸造（放不下直接回 `kBagAddItemBagFull`，转换为 `kSpiritShopBagFull`，零副作用）；
     - 全部就绪后进入 `beforeCommit`：
       1. 复核 `IsPlayerMutable(player)`、未在战、未挂 `ClientRequestLedgerInvalidComp`、`CanAfford(player, currencyType, totalCost)`；
       2. 执行 `CurrencySystem::DeductCurrency(player, currencyType, totalCost, TX_SPIRIT_SHOP_BUY, clientRequestId)`；若异常返回错误，**`beforeCommit` 返回非 `kSuccess`，`ExchangeInventoryExact` 立刻放弃背包候选不发布**（零扣钱、零发物、零 `[UNPAID_GRANT]` 窗口）；
       3. 扣灵石成功后，以不失败的 `Swap` 发布 `candidateShop` 与 `candidateLedger`；
     - `beforeCommit` 返回 `kSuccess` 后，`ExchangeInventoryExact` 发布背包候选与物品获得流水。
7. **关键进度存盘**：
   - 调用 `PlayerLifecycleSystem::RequestCriticalProgressSave(player)`（同 `BuyAfkPass` 1 秒节流直提存盘），将 `WALLET`（扣灵石 + 限购 + 幂等）与 `BAG`（绑定物品）在同一 TiDB 事务落盘。
   - 填充最新 `SpiritShopInfo` 与全量 `BagInfo`，返回 `kSuccess`。

---

### 2.7 上位红线 4 问准入自检（[project-vision.md §二](project-vision.md#二后续所有子文档准入自检写设计前必答-4-问)）

| 准入问题 | 本方案核验结论 |
|---|---|
| **1. 普通零氪玩家** | 0 元可免费走完阶段一 660 份首套 + 阶段二周 100 份精进 + 8 次保底打磨 + 免费长线攒秘墨，与百宝阁付费玩家**共享 100% 相同战力上限**；无每日打卡、无弹窗逼氪。 |
| **2. 个人开发者收入** | 百宝阁全走官方 100% 现金流（灵石充值 → 绑定提速包/定星符/秘墨/形象卡/改名卡），与玩家产出的可交易胚子/绝学残卷（30%）精准互补，直接兑现 **官方 7 : 搬砖 3** 主营收引擎。 |
| **3. 物价与长期循环** | ① 6/18/30 元绑定提速包提供清晰的 `0.1 元/份` **价格上限锚**，大商无法把市场材料炒上天；② 全部产物 `bound = true` 永不回流市场，不冲垮搬砖玩家的可交易底价；③ 共享组终身 300 份封顶，不破坏 60 天后横向消耗节奏。 |
| **4. 防骗与资产安全** | ① 全程服务端权威报价防呆 + 强制非 0 幂等键 + `ExchangeInventoryExact` 单事务提交，杜绝弱网双扣与发物未扣钱；② 全部商品绑定仅自用，彻底封堵黑卡/退款号买材料包或秘墨倒卖洗白套现。 |

---

## 3. 思想分析

1. **为什么不做“在 `java/mall_game` 里建 20 个真钱物品 SKU”，而是坚定走“通用灵石充值 + 场景内百宝阁”？**
   - 个人开发者多端发行（苹果、谷歌、微信小游戏、抖音小游戏、国内安卓）最耗命的是**渠道商品审核与退款纠纷**。把真钱入口收束成固定 6–8 个通用灵石档位，所有玩法商品都落在游戏内灵石百宝阁，既复用已经历四轮对抗审计的 `go/payment + java/mall_game + go/trade` 充值/欠款扣回链，又获得分钟级配表热更上架、组合直发、共享组限购和 `bound = true` 严格隔离的全部灵活性。
2. **为什么首套提速包要做「共享组权重限额（300 份）」而不是“每档限购 1 次”？**
   - 玩家首次消费的心理门槛在 ¥6。如果“每档各限 1 次”，要么三个档位加起来超发到 540 份打破平衡，要么做成“三选一买完 6 元就不能买 18 元”反而把小额试水玩家挡在门外。共享权重池（`60 + 180 + 60 = 300` 或一步 `300`）让玩家**先花 6 元建立信任、再按需补到 300 份封顶**，转化率与经济上限两全其美。
3. **为什么不新建“首套材料礼包箱子”道具，而是直接双物品原子直发？**
   - 遵循最简工程原则：当前工作区 `data/Item.xlsx` 正在被并行会话修改，且仓库没有“使用道具开箱”协议。直接把双物品字段放进 `SpiritShopGoods`、借 `ExchangeInventoryExact` 一次发进背包，省掉整条道具使用链路、省掉 3 个冗余道具图标、零触碰脏表，玩家买完立刻能打造强化。
4. **为什么将「缺道具 → 百宝阁 → 余额不足去充值」做成 2 步短链？**
   - 玩家付费冲动最强的瞬间，不是闲逛主城点商城，而是**在易容衣柜看中角色缺形象卡（3402）、在装备打磨界面缺定星保底符（3405）、在制书界面缺天工秘墨（3408）、或在打造台发现还差 60 份材料时**。把这四个既有界面的一键跳转直接指向百宝阁对应分页/选中行，转化率远高于在主城孤零零摆一个商城图标。

---

## 4. 漏洞分析与防线总表

| 编号 | 潜在漏洞 / 风险场景 | 攻击路径与破坏后果 | 本方案硬防线 |
|---|---|---|---|
| **V-1** | **黑卡/苹果恶意退款买绑定提速包或秘墨洗白套现** | 骗子用黑卡充灵石 → 买提速包/秘墨 → 合成装备或稀世书挂集市卖银两转移给大号 | ① 百宝阁全部商品强制 `bound = true`；② 绑定材料打出的装备与消耗绑定秘墨合出的稀世书均按既有规则永久 `bound = true`；③ 渠道撤单走 `TX_PAYMENT_REVERSAL` 直接扣回灵石/记欠款并封停退款号，绝无套现出口。 |
| **V-2** | **连点/弱网重放导致双扣灵石或绕过限购** | 玩家点买 300 份提速包时断网重发，服务器执行两次突破 300 限额或扣两份 300 灵石 | 强制非 0 `client_request_id` 接入持久化 `ClientRequestLedgerComp`（`kBuySpiritShopGoods = 15`），且限购与幂等账本同在 `WALLET` 行与 `BAG` 同事务提交；重放只回首次结果且零扣费。 |
| **V-3** | **热更调价/改内容瞬间误扣玩家灵石** | 运维把某商品从 120 灵石热更为 240 灵石，玩家按旧界面点下确认被静默多扣一倍 | 请求强制回传 `expected_currency_type`、`expected_unit_price` 与 `expected_items`；任一字段与服务端现算不符直接拒回 `kSpiritShopPriceChanged` 并刷新货架。 |
| **V-4** | **活动赠送的绑定灵石被用来白嫖官方核心商品** | 补偿发了 200 绑定灵石，若服务端自动优先扣绑定灵石则官方 70% 营收被稀释 | 服务端严格只校验并扣除 `goods.currency_type`（首批全配 `1 = kCurrencyDiamond`），界面独立区分展示「灵石」与「绑定灵石」，绝不混扣。 |
| **V-5** | **买双物品提速包时背包只够放打造料、放不下百炼石导致半发/错账** | 背包只剩 1 格空位，发了 3001 放不下 3002 却把钱扣了 | `BagService::ExchangeInventoryExact` 在候选副本先完成整批两项物品的完整分堆空间规划与 ID 铸造，放不下整批回 `kSpiritShopBagFull`，零扣灵石、零发物、零占限购。 |
| **V-6** | **玩家节点时钟回拨或重启导致周限购被刷重置** | 服务器时钟回拨或跨节点切换使 `week_starts_at_ms` 倒退，周限购归零重买 | 复用 `PlayerEquipmentMaterialReward` 的 `ResolveWeek(nowMs)`（UTC+8 周一 05:00），当 `weekStart < record.week_starts_at_ms()` 时**严禁清零**，保守沿用原已购计数；数据随存档持久化，跨节点不丢失。 |
| **V-7** | **单份价格或权重乘法整数溢出绕过扣费/限额** | 恶意客户端发 `count = 0xFFFFFFFF` 导致 `price * count` 或 `weight * count` 32/64 位回绕成极小值 | 入口先强制校验 `1 <= count <= max_buy_per_request <= 99`，表校验锁死 `price <= 10^9`、`limit_group_weight <= 10^6`，全部乘法在 `uint64_t` 上做显式溢出边界守护。 |
| **V-8** | **并行会话脏表冲突（当前工作区实况）** | `data/Item.xlsx`、`data/EquipEnhance.xlsx`、`data/MessageLimiter.xlsx` 当前存在并行未提交修改，直接编辑会引发 `.xlsx` 冲突 | 本方案**零改动 `Item.xlsx` 与 `EquipEnhance.xlsx`**；所有商品数据放进新建独立源表 `SpiritShopGoods.xlsx` / `SpiritShopLimitGroup.xlsx`；仅 `Tip.xlsx` 与 `MessageLimiter.xlsx` 安排在并行批次提交后再做最小尾行追加。 |
| **V-9** | **Java 独立服缺位导致双服契约分裂（`AGENTS.md §12`）** | C++ 服上线百宝阁，Java 独立服无对应 RPC 导致客户端进 Java 服报错 | 同批次同步执行 `ContractSync.java`，在 `xuanming-server-mmo/game-scene` 对齐实现 `SpiritShopRules`、`SpiritShopService`、`PlayerSpiritShopComp` 并更新 `PARITY.md`。 |

---

## 5. 步骤分析（分四阶段小步推进 + 避并行冲突排期）

| 阶段 | 目标与文件范围 | 并行冲突隔离说明 | 验收门槛 |
|---|---|---|---|
| **阶段 0 · 设计定案（本轮已完成）** | 落盘本文 + 注册 `gen_docs_index.py` + 刷新 `docs/README.md` | 仅新增 1 份 `.md` 与 1 行索引规则，零接触任何代码与 `.xlsx` | `python tools/scripts/gen_docs_index.py --check` 退出 0 |
| **阶段 1 · 独立配表、协议与 C++ 权威实现** | ① 新建 `SpiritShopGoods.xlsx`、`SpiritShopLimitGroup.xlsx` 及对应 `data/schema/*.proto`；<br>② 新建 `proto/common/component/player_spirit_shop_comp.proto`、`proto/scene/player_spirit_shop.proto`；<br>③ 最小追加 `Tip.xlsx`（`37000` 段）、`MessageLimiter.xlsx`（2 行限流）、`mysql_database_table.proto`（field 34）、`transaction_log.proto`（`TX_SPIRIT_SHOP_BUY=44`）、`client_request_ledger.h`（`kBuySpiritShopGoods=15`）；<br>④ 实现 `spirit_shop_rules.h`、`player_spirit_shop.{h,cpp}`、`player_spirit_shop_handler.{h,cpp}` 与全套单测 | **待并行会话提交或锁定 `Tip.xlsx` / `MessageLimiter.xlsx` 后再改这两张表**；绝不触碰 `Item.xlsx` 与 `EquipEnhance.xlsx` | ① `dev.bat gen` + `scene.exe --validate-tables` 全绿；<br>② `test_cpp_no_raw_pointer_member.py` & `test_cpp_thread_sources.py` & `check_log_text_english.py` 全绿；<br>③ 串行 `msbuild /m:1` + `bag_test` 单测覆盖首套共享组 60+180+60=300 封顶、300 拦截、报价防呆、幂等重放、满包零扣钱、绑定分堆不洗白、周限购抗回拨、存盘往返 |
| **阶段 2 · Java 独立服对齐（`AGENTS.md §12`）** | `xuanming-server-mmo` 执行 `ContractSync.java` → `game-scene` 新增 `SpiritShopRules`、`SpiritShopService`、`SpiritShopHandler`、`PlayerSpiritShopComp` 与单测 → 更新 `PARITY.md` | 独立子仓，无 C++ 脏工作区干扰 | `ContractSync --check` 退出 0；Maven `game-scene` 定向与回归测试全绿 |
| **阶段 3 · 客户端非 UI 层 + 四大场景快捷入口 + Codex UI 交接** | ① `mmorpg-client` 执行 `gen.bat client` 同步表、C# Proto 与 `MessageIds.cs`；<br>② 新增纯 C# `Assets/Scripts/Game/SpiritShop/{SpiritShopModels,SpiritShopClient}.cs` 挂 `GameClient.SpiritShop` 与 NUnit 单测；<br>③ 将易容缺卡（`3402`）、打磨缺定星符（`3405`）、制书缺天工秘墨（`3408`）、打造缺材料跳转统一指向百宝阁目标行（灵石不足自动引导 `PaymentClient` 充值档位）；<br>④ 交接异机 Codex 制作道家 Q 版「灵石百宝阁」uGUI（含月卡聚合卡片与双余额展示） | 先完成纯 C# 与跳转接口，不阻塞美术/UI 制作 | `client_compile_check.ps1` 0 错误；Roslyn + NUnit `SpiritShopClientTests` 全绿；dev 环境 mock 充值 → 买提速包/秘墨/形象卡端到端跑通 |

---

## 6. 执行步骤（待负责人拍板确认后按序执行）

- [x] **步骤 1（本轮已完成）**：核对 C++/Go 源码、既有支付/商城/神秘商人/月卡/材料账契约及当前工作区并行改动，完成需求分析、设计分析、思想分析、漏洞分析、步骤分析与四问准入自检，落盘至本文件。
- [x] **步骤 2（本轮已完成）**：更新 `tools/scripts/gen_docs_index.py`（归入「玩法系统」分类）并运行生成 `docs/README.md`，通过 `--check` 校验。
- [ ] **步骤 3（待确认拍板点）**：负责人确认下方 3 个快速勾选点，以及当前工作区并行改动是否可以进入落码窗口。
- [ ] **步骤 4（阶段 1 落码）**：编写 `tools/scripts/spirit_shop_xlsx_patch.py`（幂等产出 `SpiritShopGoods.xlsx`、`SpiritShopLimitGroup.xlsx`、补 `Tip.xlsx` `37000` 段与 `MessageLimiter.xlsx`），新增 proto 与 schema，执行 `dev.bat gen`，落地 C++ `PlayerSpiritShopSystem` + handler + GTest 单测，跑只读静态守卫、串行 `msbuild /m:1` 与 `scene.exe --validate-tables`。
- [ ] **步骤 5（阶段 2 双服对齐）**：在 `xuanming-server-mmo` 运行 `ContractSync.java`、实现 Java `SpiritShopService` 与对账单测，并核销 `PARITY.md`。
- [ ] **步骤 6（阶段 3 客户端与联调）**：在 `mmorpg-client` 跑 `gen.bat client`，落地 `SpiritShopModels` / `SpiritShopClient`、NUnit 单测与四大缺料入口跳转，并按需产出给异机 Codex 的「灵石百宝阁」UI 交接规范（附商城充值档位补齐 `¥18=180 灵石` 配置说明）。

---

## 7. 待负责人确认的 3 个拍板点（确认后立即按阶段 1 开工）

1. **首套提速包终身共享总上限**：默认设 **300 份（半套，合计最高 ¥30：支持 ¥6=60 / ¥18=180 / ¥30=300 自由拼单）**，还是直接放开到 **600 份（整套，合计最高 ¥60）**？*(表配置项，以后可随时热更调大)*
2. **精进配套单价确认**：
   - `定星保底符（3405）`：**120 灵石（¥12）**（也可微调为 60 / 100 / 120 灵石）；
   - `天工秘墨（3408）`：**240 灵石（¥24）**（符合既有设计 ¥18–¥30 区间，也可微调为 180 / 240 / 300 灵石）；
   - `角色形象卡（3402）`：**100 灵石（¥10）**、`改名卡（3401）`：**100 灵石（¥10）**。
3. **落码时机（工作区并行改动协调）**：现在直接基于当前工作区开工（仅先新建完全无冲突的独立文件，最后一步再增量追加 `Tip.xlsx` / `MessageLimiter.xlsx`），还是等当前工作区的并行会话先提交收尾后再动工？
