# 官方灵石百宝阁 V2 策划与工程方案（定时自动上下架 + 3–4 物品大礼包/多选一自选包 + 原价划线折扣 + 角色阶段解锁）

> 状态：2026-11-26 已完成三仓落地与全量编译测试验收（由负责人明确拍板，专为春节礼包、节假日礼盒与新赛季阶段礼盒设计，100% 向下兼容 V1，零数据库迁移、零新增 RPC）。  
> 上位总纲：[project-vision.md](project-vision.md) · V1 基础文档：[spirit-shop-and-starter-packs-v1.md](spirit-shop-and-starter-packs-v1.md)。

---

## 1. 需求分析

1. **个人开发者无值守节庆运营**：春节、中秋、周年庆、新赛季开启时，个人开发者不想熬夜盯点手动改表热更；需要提前在 Excel 配好 `start_time_ms` / `end_time_ms`，到点自动开售、过期自动下架。
2. **高诚意节庆礼盒形态**：
   - **3–4 件物品大礼包**（如春节 4 合 1 纪念礼盒：一次原子发放 4 种绑定物品）；
   - **多选一自选包**（如新春秘宝自选匣：从 2–4 件高价值道具中任选 1 件直接购买发进背包，避免“礼包里有不需要的凑数项”，也省掉额外开箱道具链路）。
3. **限时实惠清晰传达与阶段精准承接**：
   - **限时折扣原价划线**（如原价 300 灵石、春节特惠 180 灵石，明码标出真实折数，绝不虚假抬高标价）；
   - **角色等级阶段解锁**（`min_level` / `max_level`：避免 1 级新手满屏看到满级礼盒，也避免老玩家看到过期新手包）；
   - **独立「节庆特惠」分页（`tab_id = 4`）**：平时无在售限时礼盒时前端自动隐藏该分页，保持界面干净零打扰；活动期内自动浮现并展示实时倒计时。

---

## 2. 设计分析

### 2.1 零破坏式扩展原则
- **零数据库迁移**：复用既有 `PlayerSpiritShopComp`（field `34`, WALLET 组）与 Java 第 14 代存储台账 `0014.schema`，限时礼盒沿用 `goods_id` 个人限购或 `limit_group_id` 共享组限购。
- **零新增 RPC**：继续复用 `509 GetSpiritShopInfo` 与 `510 BuySpiritShopGoods`，仅在 `SpiritShopGoodsTable`、`SpiritShopGoodsView`、`BuySpiritShopGoodsRequest` 末尾纯追加字段，新老字段语义零变更。

### 2.2 配表字段扩展（`data/schema/spiritshopgoods_table.proto` 末尾追加 22..33）

| 字段号 | 字段名 | 类型 | 口径与启动/热更强校验（fail-closed） |
|---:|---|---|---|
| 22 | `start_time_ms` | `uint64` | 定时开售 UTC 毫秒（`0` = 立即开售） |
| 23 | `end_time_ms` | `uint64` | 定时下架 UTC 毫秒（`0` = 永不下架；若与 `start_time_ms` 均非 0，须满足 `end_time_ms > start_time_ms`） |
| 24 | `grant_mode` | `uint32` | `0 = ALL`（1..4 项全发），`1 = CHOOSE_ONE`（2..4 项任选 1 项） |
| 25–27 | `item_id_3 / item_count_3 / item_bound_3` | `uint32` | 物品 3（可选；非 0 时 `item_id_2` 必须非 0 禁跳槽，须存在且 `count >= 1`、`bound ∈ {0,1}`） |
| 28–30 | `item_id_4 / item_count_4 / item_bound_4` | `uint32` | 物品 4（可选；非 0 时 `item_id_3` 必须非 0 禁跳槽，校验同上） |
| 31 | `original_price` | `uint64` | 划线原价（`0` = 无划线；非 0 时须满足 `price < original_price <= 10^9`，纯展示绝不参与扣费） |
| 32 | `min_level` | `uint32` | 最低可见/可购有效等级（`0` = 不限） |
| 33 | `max_level` | `uint32` | 最高可见/可购有效等级（`0` = 不限；双非 0 时须 `max_level >= min_level`） |

**候选项防呆规则**：
- `grant_mode == 0`（全发）：允许 1..4 项，不允许任意两项 `(item_config_id, bound)` 重复；
- `grant_mode == 1`（多选一）：必须至少 2 项（2..4 项），且不允许任意两项 `(item_config_id, item_count, bound)` 完全相同。

### 2.3 协议与 Tip 增量
- `SpiritShopGoodsView` 末尾追加：`start_time_ms = 18`、`end_time_ms = 19`、`grant_mode = 20`、`original_price = 21`、`min_level = 22`、`max_level = 23`。
- `BuySpiritShopGoodsRequest` 末尾追加：`chosen_item_index = 7`（`grant_mode == 0` 时必须为 `0`；`grant_mode == 1` 时必须在 `[0, items.size())`，且 `expected_items` 必须恰好包含所选的那 1 项物品，防止热更调换顺序误领）。
- `Tip.xlsx` `//spirit_shop_error` 末尾追加 3 条非故障业务提示（`37011..37013`）：
  - `kSpiritShopNotInSaleWindow`（37011）：`该商品暂未开售或活动已结束`
  - `kSpiritShopLevelNotMet`（37012）：`角色等级未满足该商品购买条件`
  - `kSpiritShopInvalidChoice`（37013）：`所选礼盒物品无效，请重新选择`

### 2.4 预置 2027 春节礼盒样例行（默认定时潜伏，到点自动开售）

| `goods_id` | 分页 | 名称 | 现价 / 原价 | 模式与发放内容（全部 `bound = true`） | 限购 / 等级 | 自动开售时间窗（UTC+8） |
|---:|---|---|---:|---|---|---|
| **401** | 4 · 节庆特惠 | **新春四喜大礼盒** | **180** / ~~300~~（6折） | `ALL` 4 合 1：`3001`×60 + `3002`×60 + `3405 定星保底符`×2 + `3402 角色形象卡`×1 | 终身限购 1 份 / ≥10 级 | `2027-02-05 00:00` ~ `2027-02-20 23:59:59` |
| **402** | 4 · 节庆特惠 | **新春秘宝自选匣** | **120** / ~~180~~（6.7折） | `CHOOSE_ONE` 四选一：`3408 天工秘墨`×1 / `3412 技能特效卷`×1 / `3411 灵宠幻化符`×1 / `3405 定星保底符`×3 | 终身限购 2 份 / ≥10 级 | `2027-02-05 00:00` ~ `2027-02-20 23:59:59` |

### 2.5 上位红线 4 问准入自检
1. **普通零氪玩家**：春节礼盒全部为省时/外观/保底绑定道具，0 战力上限突破，平时无活动时分页自动隐藏，绝无弹窗骚扰。
2. **个人开发者收入**：通过春节/节假日高诚意礼盒（4 合 1 大礼盒 + 自选匣）与 18/12 元友好价格带承接节庆消费高峰，全走官方 100% 真实灵石现金流，且提前配好时间戳免人工熬夜守点。
3. **物价与长期循环**：全强制 `bound = true` 严禁流入自由市场，不冲击搬砖玩家可交易物价；限购 1–2 份守住长期消耗节奏。
4. **防骗与资产安全**：左闭右开时间窗严格按服务端 `nowMs` 裁决、自选下标纳入幂等指纹与报价单物品核对、4 物品原子入包失败零扣费、黑卡退款仍走既有 `TX_PAYMENT_REVERSAL` 闭环。

---

## 3. 思想分析

1. **为什么把「多选一」直接做在购买请求 `chosen_item_index` 里，而不是先发一个“自选箱道具”再写一套开箱协议？**
   - 玩家在百宝阁确认弹窗点选心仪奖励后一步到位直接拿到绑定成品，减少一次背包开箱操作，同时避免新增道具类型与使用协议，双端实现最简、回滚与审计最干净。
2. **为什么平时隐藏「节庆特惠」分页（`tab_id = 4`），有在售限时商品才显示？**
   - 避免平时玩家点进一个空荡荡的“敬请期待”标签页产生廉价感；只要配表里任意一条 `tab_id = 4` 商品进入 `[start_time_ms, end_time_ms)` 且满足角色等级，客户端零发版自动亮出该分页。
3. **为什么客户端倒计时必须基于 `server_time_ms` + 本机单调时钟差值，而不能信玩家本机系统时间？**
   - 玩家修改手机时钟既不能提前买到未开售礼盒（服务端以自身 `nowMs` 裁决），也不会让前端倒计时显示错乱负数。

---

## 4. 漏洞分析与防线

| 编号 | 风险场景 | 硬防线 |
|---|---|---|
| **V2-1** | 玩家改本机时钟或重放旧货架缓存，在未开售/已下架瞬间抢买春节礼盒 | 服务端 `Buy` 强制按自身 `nowMs` 判定 `[start_time_ms, end_time_ms)`，窗口外一律拒回 `kSpiritShopNotInSaleWindow(37011)`；`GetInfo` 窗口外不下发。 |
| **V2-2** | 买多选一自选包时篡改 `chosen_item_index` 越界，或热更调换选项顺序导致领错道具 | 服务端校验 `chosen_item_index < items.size()`（否则回 `kSpiritShopInvalidChoice(37013)`），且强制 `expected_items` 仅 1 项并与目标候选项逐字段相等（不符回 `kSpiritShopPriceChanged`）。 |
| **V2-3** | 同 `client_request_id` 第一次买选项 0，弱网重发时改成选项 1 企图白嫖两种奖励 | `ClientRequestFingerprint(request)` 覆盖含 `chosen_item_index` 在内的完整请求体，同键换选项直接按 `kMismatch` 拒回 `kInvalidParameter`；同键同选项即使已过期仍幂等重放成功。 |
| **V2-4** | 4 件大礼包背包只剩 3 格导致前 3 件入包、第 4 件失败但灵石已扣 | C++ `BagService::ExchangeInventoryExact` 与 Java 克隆候选 `PlayerBags` 均先完成全部 4 项完整规划，空间不足整批回滚 `kSpiritShopBagFull`，零扣费、零发物。 |
| **V2-5** | 配表填错时间倒挂（`end <= start`）、原价低于现价、跳槽填 `item_id_3` 或单选项设为自选包 | 启动与热更 `ValidateTables()` / `SpiritShopTables` 严格 fail-closed 拦截坏行并打英文 `LOG_ERROR`，拒绝放行坏配置。 |
| **V2-6** | 等级不够（或超出新手包 `max_level`）强发 RPC 购买 | `GetInfo` 按有效等级过滤；`Buy` 二次校验有效等级，不符拒回 `kSpiritShopLevelNotMet(37012)`。 |

---

## 5. 步骤分析与执行清单

- [x] 1. 落盘本 V2 策划方案并自动归入 `docs/README.md` 索引。
- [x] 2. 扩展 `spiritshopgoods_table.proto`（22..33）、`player_spirit_shop.proto`、`Tip.xlsx`（37011..37013）、`spirit_shop_xlsx_patch.py` 与 `SpiritShopGoods.xlsx`（含 401/402 2027 春节礼盒样例）。
- [x] 3. Gemini 执行导表、协议生成与客户端同步。
- [x] 4. 实现 C++ `spirit_shop_rules.h` / `player_spirit_shop.cpp` 与全套 V2 GTest（定时窗、4 物大礼包满包回滚、多选一自选与篡改/错选拦截、原价划线、等级门槛、过期后旧幂等重放）。
- [x] 5. Java 独立服执行契约同步并同构实现 `SpiritShopTables` / `SpiritShopService` 与全套 V2 JUnit 单测，更新 `PARITY.md`。
- [x] 6. Unity 客户端更新 `SpiritShopModels` / `SpiritShopClient` / `SpiritShopWindow`（动态第 4 分页「节庆特惠」、原价划线与折数、基于服务器时间的实时倒计时、4 件大礼包展示、确认窗多选一单选交互）与 `SpiritShopClientTests`。
- [x] 7. Gemini 全量编译、验表与跑通 C++ / Java / Unity 三仓单测。
