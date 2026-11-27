# 《五行奇谈》不肝不氪可持续增收体系升级总方案（V2：货架纵深 × 轻享卡升级 × 社交排面 × 合规 IAA × 海外/Steam）

> **日期**：2026-11-27  
> **状态**：**负责人 2026-11-27 拍板全部接受（B1–B5 全量纳入正式演进路线，按批次四仓闭环推进）**  
> **上位宪法**：[项目设定 project-vision.md](project-vision.md)（严格守住七条红线、三类禁区与五问准入自检）  
> **配套 UI / 美术交接单**：[fair-monetization-expansion-v2-ui-handoff.md](ui-handoff/fair-monetization-expansion-v2-ui-handoff.md)  
> **衔接基线**：[经济与官方 7:3 总纲](equipment-economy-charter.md) · [官方灵石百宝阁 V1](spirit-shop-and-starter-packs-v1.md) / [V2](spirit-shop-v2-festival-and-bundles.md) · [游历手记 V1](activity-travel-pass-and-showcase-v1.md) · [挂机月卡 / 仙途轻享卡](../design/afk-monthly-card.md) · [角色永久衣柜](character-appearance-switch.md) · [灵宠幻化与技能特效 V1](wardrobe-pet-morph-and-skill-vfx-v1.md) · [7 套三合一共享流派席位](skill-build-loadout-7slots-v1.md) · [零拒审激励视频与双银两 V1](rewarded-ad-compliance-and-economy-v1.md) · [全面反「上班」整治 V1](anti-chore-remediation-v1.md) · [江湖恩怨与生死状 V1](battle-jianghu-grudge-duel-bounty-v1.md) · [五行幻境 V2 轻肉鸽](wuxing-roguelike-realm-v2.md) · [十季无限循环 V4](season-ten-seasons-infinite-cycle-v4.md)

---

## 一、需求分析

### 1.1 现状诊断：不肝不氪骨架已健康，但存在三处营收天花板

基于 2026-11-27 实表与四仓现役代码核查，当前商业化体系已打通灵石百宝阁 V1/V2、仙途轻享卡、先玩后买的游历手记、三页签永久衣柜与默认关闭的绑银激励视频，但在长线经营上存在三处明确瓶颈：

1. **货架太浅，忠实玩家两周即买穿**：
   - `CharacterAppearance.xlsx` 现役可卡换角色仅 6 位（¥60），游历手记首期典藏送 2 张形象卡 + 帮会商店终身换 1 张后，实际仅剩 ¥30 缺口；
   - `PetCosmetic.xlsx` 可买断灵宠幻化仅 2 款（¥24），`SkillVfxCosmetic.xlsx` 可买断技能特效仅 2 款（¥36）；
   - `TravelPassSeason.xlsx` 仅配置了第 1 期（10–15 场即可满点），后续无多期书架接力；
   - 已落码且已有图标的官方 7:3 配套道具 `3409 解封剥离符` 与 `3410 流派副套继承符` 尚未上架 `SpiritShopGoods.xlsx`。
2. **¥30 仙途轻享卡对“前两周散人/独狼”缺少即时体感**：
   - 现役三项权益（离线跟队托管、三合一 7 套流派席位 +2 套、阶段二周 150 胜局可交易门票）均偏组队/中后期/搬砖，纯休闲独狼首月难以产生下单冲动，且缺少 90 天高性价比中档承接。
3. **高曝光低成本外观、社交情绪复购与国内无版号 IAA 场景均偏单一**：
   - 画一整套 40+ 帧灵宠动作产能重，而战场阵纹、入场/胜者演出、聊天气泡、分享卡底纹、主城社交烟花/灵灯/仙茶席与好友赠礼等**美术成本低 10 倍、全服曝光高 10 倍**的模块尚未落地；
   - 国内无版号阶段若微信/抖音小游戏只能开流量主（IAA），仅靠次级界面角落 1 个「观影小礼（300 绑银）」难以撑起人均有效观看次数；同时海外与 Steam PC 端（第一天即可合规收真钱）缺少明确的支持者包/DLC 发行规格。

### 1.2 本次全量升级范围（10 项需求收口为 5 个独立闭环批次）

> **去重校验（遵循 KISS / 零重复收费）**：核查实码确认「装备 + 战法 + 加点 7 套三合一共享席位（[skill-build-loadout-7slots-v1.md](skill-build-loadout-7slots-v1.md) / [equipment-loadout-presets-v1.md](equipment-loadout-presets-v1.md)）」与「帮会资金改名 `RenameGuild`（`proto/guild/guild.proto`）」均已完整落码。本次扩容便利**绝不重复收费或推翻 7 套三合一席位**，仅补齐尚未覆盖的仓库格、灵宠编队预设、精彩战报永久收藏位与帮会徽记/旗帜外观定制。

| 批次 | 包含需求项 | 核心业务目标 |
|---|---|---|
| **批次 B1（最小改动、最快拉收）** | ① 游历手记「永不绝版书架制」（2–6 期接力 + 自由切换在研期次）<br>② 仙途轻享卡前期体感升级 + 90 天「季享卡（¥78）」<br>③ 衣柜系列「Steam 式按缺口 8.5 折补齐包」+ 百宝阁上架 `3409 解封剥离符` / `3410 流派副套继承符` | 复用现成表与组件骨架，立刻拉深货架、提升月卡首月转化与客单价 |
| **批次 B2（低成本高曝光外观 + 社交情绪王牌）** | ④ 战斗排面三件套（五行阵法脚底阵纹 + 擂台/会武入场与胜利演出 + 聊天气泡与 `#TJC`/`#PUZ` 分享卡底纹）<br>⑤ 「全城同乐」主城社交气氛耗材（天墉灵灯 / 庆功烟花 / 仙茶席，全图特效 + 路人领绑定银两）<br>⑥ 好友间「纯外观 / 手记典藏券」真实灵石赠礼（带防黑卡退款清位图回收硬闸） | 建立可持续的高毛利外观池与跨账号复购引擎，打破单号买穿上限 |
| **批次 B3（无焦虑扩容 + 横向内容复购）** | ⑦ 多流派/收藏党扩容便利（仓库格、灵宠编队预设、战报/配装收藏夹扩容、帮会徽记定制）<br>⑧ 按《率土》思路新增第 8/9 卷横向机制技能卷宗（稀世硬锁 ≤ 10%） | 承接中后期铁粉的深度整理与构筑消费，自然带动「天工秘墨 / 定星保底符」复购 |
| **批次 B4（国内无版号小游戏纯 IAA 增收）** | ⑨ 激励视频新增 3 个按需触发爽点位（五行幻境 V2 每局 1 次奇策免费重随、衣柜续 1 小时试穿、神秘商人每周免费刷新个人货架），严守首审关闭与零通胀 | 在国内未获版号空窗期，不强弹、不打卡即可把小游戏 IAA 流水拉至健康水平 |
| **批次 B5（海外 + Steam PC 发行变现底座）** | ⑩ Steam PC 端（免费本体 + 透明 DLC 创始支持者典藏包 / 设定集原声包）与港澳台/东南亚 App Store & Google Play 支付与合规落盘规格 | 绕开国内版号空窗，第一天上线即可全量开启合规官方直售现金流 |

### 1.3 上位红线五问准入自检（[project-vision.md §二](project-vision.md#二后续所有子文档准入自检写设计前必答-5-问)）

| 准入问题 | 核验结论 |
|---|---|
| **1. 普通零氪 / 反「上班」** | 全 5 批零每日签到、零每日领月卡灵石、零日限购、零固定钟点；往期游历手记永不绝版彻底消灭 FOMO；全城同乐绑定银两设个人周领上限防蹲点；0 战力上限突破，零氪与付费永久共享 100% 相同上限。**通过**。 |
| **2. 个人开发者收入** | 全部新增商品、季享卡、系列补齐、排面外观、社交烟花、好友赠礼、扩容与 Steam DLC 100% 走官方真实灵石/平台收款直入官方口袋；IAA 激励视频全走官方小游戏流量主，严守官方 7:3 格局。**通过**。 |
| **3. 物价与长期循环** | 全部外观/礼遇/手记/赠礼均永久绑定；全城同乐红包与激励视频严格隔离于 `CURRENCY_TYPE_BIND_GOLD = 3` 或局内/纯展示权益，0 流通银两增发、0 可交易货源增发；`3409` 解封严守 `origin_tradeable == true` 不洗白 B 料。**通过**。 |
| **4. 防骗与资产安全** | 全部灵石消耗强制非 0 `client_request_id` 持久化幂等账本 + 报价防呆 + 原子提交；好友赠礼仅限绑定纯外观/手记券且挂钩 `TX_PAYMENT_REVERSAL` 精准回收受赠位图并封黑卡号，杜绝黑卡套现与站外引流。**通过**。 |
| **5. 商店与平台过审/防下架** | 国内无版号小游戏端硬隔绝一切内购、赠礼与 DLC 入口；激励视频新增 3 槽默认 `enabled = 0`、首审零暴露、全等权双按钮、中性文案零“支持作者/捐赠”诱导词；自定义文本走 `ContentGuard`；绝不引入外链或切支付。**通过**。 |

---

## 二、设计分析（按批次给出精确契约与表/字段号规划）

> **现役实码号段核对基线（2026-11-27）**：`player_database` 尾号 `37`（下空 **`38`**）；`PlayerProfileComp` 尾号 `15`（下空 **`16`**）；`PlayerUint64Comp` 尾号 `11`（下空 **`12`**）；`PlayerTravelPassComp` 尾号 `4`（下空 **`5`**）；`TransactionType` 尾号 `51`（下空 **`52`**）；`ClientRequestKind` 尾号 `30`（下空 **`31`**）；`ClientRequestRecord` 尾号 `44`（下空 **`45`**）；`Tip.xlsx` 尾域 `40000`（下空 **`41000`**）；`Item.xlsx` 34xx 尾号 `3416`（下空 **`3417`**）。落码前仍以当时 `HEAD` 复核。

---

### 2.1 批次 B1：永不绝版手记书架 × 轻享卡升级/季享卡 × 系列补齐 × 3409/3410 上架

#### B1-1. 百宝阁上架既有官方 7:3 配套符（零代码、纯配表热更）
在 `SpiritShopGoods.xlsx`（`tab_id = 2 · 精进配套`）新增 2 行（复用已存在的 `Item.xlsx` ID 与已导入图标）：
- **`goods_id = 203` · 解封剥离符**：`item_id_1 = 3409`, `item_count_1 = 1`, `item_bound_1 = 1`, `currency_type = 1`, `price = 60`（¥6），不限购（单次 1..10），复用 `UI/Ugui/EconomyTradeV1/icon_unseal_talisman`；严守 `UNSEAL_TO_TRADABLE` 既有 `origin_tradeable == true` 且清空绑定养成的防洗白逻辑。
- **`goods_id = 204` · 流派副套继承符**：`item_id_1 = 3410`, `item_count_1 = 1`, `item_bound_1 = 1`, `currency_type = 1`, `price = 120`（¥12），不限购（单次 1..10），复用 `UI/Ugui/EconomyTradeV2/icon_inherit_talisman`。
- 客户端装备页在执行解封/继承缺少 `3409 / 3410` 时，接入既有 `SpiritShopUiRoot.OpenToGoods(203 / 204)` 一键直达。

#### B1-2. 游历手记「永不绝版书架制」（V2）
- **核心规则**：
  1. 解除 `TravelPassSeason.xlsx`「同一时段至多 1 条启用」的旧约束：允许同时存在多条 `enabled = 1` 且 `starts_at_ms <= nowMs` 的手记（**`ends_at_ms = 0` 表示永不下架**，或配 `archive_after_ms` 仅用于标记「最新一期 / 往期典藏」，往期始终可切换积点、可领免费轨、可随时 ¥18 解锁典藏轨）。
  2. 每名角色同一时刻选定 **1 本在研手记（`active_pass_id`）**：战斗结算的游历印（+20/+30/+15）自动注入当前选定手记；若当前手记已满 `max_points` 且玩家未手动切换，系统自动顺延至玩家尚未满点的最新一期（若有）。
  3. 玩家可在手记顶部「手记书架」抽屉/切换条中 **0 消耗、无冷却自由切换**任意已开售期次，已有进度与领取掩码按 `pass_id` 独立永久保留（`kMaxSeasons` 由 `32` 放宽至 `64`）。
  4. **首批预置 6 期书架内容（随赛季节奏逐步亮起，或按 `starts_at_ms` 定时自动上架，首测可同时开放 1–2 期验证书架切换）**：
     - 第 1 期（既有）：青云游历手记（180 灵石）
     - 第 2–6 期（对标十季循环主题：玄霜问剑 / 炽焰丹心 / 沧澜听雨 / 灵木问道 / 厚土镇岳，各 180 灵石 = ¥18，每期 10 档均严格 0 战力上限、0 可交易货源、0 打造/百炼石；奖励改为**当期限定纪念头像框 + 称谓 + 阵纹/聊天气泡兑换券 + 定星保底符 + 天工秘墨 + 绑定重铸砂 + 1 张形象/幻化/特效自选或单券**，避免 6 期白送 12 张形象卡把衣柜直接送穿）。
- **契约增量（零新表、零新 DB 行）**：
  - `PlayerTravelPassComp` 追加 `uint32 active_pass_id = 5;`（`0` 表示默认最新可用期次）。
  - `TravelPassSeasonTable` 末尾追加 `uint32 sort_order = 16;`、`uint32 is_archived = 17;`（纯展示角标「当期推荐 / 往期典藏」，不拦截积点与解锁）。
  - `proto/scene/player_activity.proto`：
    - `TravelPassSnapshot` 末尾追加 `uint32 active_pass_id = 33;`、`repeated TravelPassCatalogEntry bookshelf = 34;`（轻量目录项：`pass_id / name / icon_key / progress_points / max_points / premium_unlocked / claimable_count / is_active / is_archived`）。
    - `SceneActivityClientPlayer` 末尾追加 1 条零扣费幂等 RPC：`rpc SelectActiveTravelPass (SelectActiveTravelPassRequest) returns (SelectActiveTravelPassResponse);`。

#### B1-3. 仙途轻享卡前期体感升级 + 90 天「季享卡（¥78）」
- **零打卡、零战力四项升级**：
  1. **多档位规则表（月卡 30 天 / 300 灵石 + 季享卡 90 天 / 780 灵石）**：
     - 将单行 `AfkPassRule.xlsx` 升级为多行套餐表（行 `id = 1` 保持 `30 天 / 300 灵石`，向下 100% 兼容旧客户端 `plan_id = 0`；新增行 `id = 2`：`90 天 / 780 灵石`，省 120 灵石 = ¥12；共用同一个 `afk_pass_expires_at` 与 `max_remaining_days = 180` 叠加上限，绝不做买断终身卡）。
     - `BuyAfkPassRequest` 末尾追加 `uint32 plan_id = 4;`（`0` 兼容默认 `1`）与 `uint32 expected_period_days = 5;`（防热更改天数）。
  2. **开卡一次性直赠礼遇（绝不拆成每日登录领！）**：
     - 规则表 `AfkPassRule` 末尾追加 `uint32 bonus_item_id = 8;`、`uint32 bonus_item_count = 9;`、`string bonus_frame_key = 10;`（月卡开通一次性发绑定 `3405 定星保底符 × 1` + 永久纪念人物头像框 `frame_lightcard_s1`；季享卡一次性发绑定 `3405 定星保底符 × 3` + 季享专属名片底纹/头像框 `frame_lightcard_season_s1`，重复开通自动跳过已拥有外观不重复占用位，物品按 `ExchangeInventoryExact` 同事务预检空间防满包扣钱；严格遵守“头像框不进商城单卖、仅作月卡/荣誉附赠”红线）。
  3. **兑现「市场便利权益」（离线摆摊时长翻倍 + 集市挂单栏位扩容）**：
     - 复用既有 Redis 投影 `player:afk_pass:{<player_id>}`（`go/trade` 直接读取，零新增跨服务 RPC，读失败 fail-safe 按普通玩家基础档降级不阻断挂单）：
       - 集市同时在售挂单上限：普通玩家 `10` 单 → 轻享卡生效期 **`20` 单**；
       - 离线摆摊单次营业时长（`stall-system.md` 实施时同步挂载）：普通 `48 小时` → 轻享卡生效期 **`96 小时`**；
       - `AfkPassInfo` 末尾透出 `uint32 market_listing_limit = 13;`、`uint32 stall_duration_hours = 14;`、`uint32 monthly_free_trial_quota = 15;`、`uint32 monthly_free_trial_used = 16;` 与 `repeated AfkPassPlanView plans = 17;`。
  4. **月卡期「衣柜随心试穿」特权**：
     - 普通玩家每个未拥有角色终身 1 次 1 小时免费试穿（`appearance_trial_used_bits`）；
     - 轻享卡生效期间，角色每自然周（UTC+8 周一 05:00，非日清防打卡）额外享有 **3 次「随心试穿（1 小时）」**，可对任意已试用过但尚未拥有的角色形象再次发起 1 小时试穿（**不授予永久权**，到期照常还原并温和弹出单行“喜欢可在衣柜永久收藏”提示），既让轻享卡第 1 天就有强体感，又直接拉动衣柜永久买断。
     - `PlayerProfileComp` 追加 `uint64 light_card_trial_week_start_ms = 16;`、`uint32 light_card_trial_used = 17;`；`StartAppearanceTrialRequest` 复用既有 RPC，服务端当常规试穿位已用且轻享卡生效、本周尚有余量时自动走随心试穿分支。

#### B1-4. 衣柜系列「Steam 式补齐捆绑包（Complete the Set）」
- **定价与防呆规则**：
  - 针对 `AppearanceSeries.xlsx`（当前 3 册：留灯院旧友 / 水火冰锋 / 清音济世，后续新册自动生效），在 `AppearanceSeriesTable` 末尾追加 `uint32 bundle_discount_bps = 10;`（默认 `8500` = **8.5 折**，`0` 表示该册不支持补齐包）与 `uint32 unit_price_diamond = 11;`（默认 `100` 灵石，对齐单张角色形象卡）。
  - 服务端实时计算该系列下所有 **`enabled == 1 && card_unlockable == 1 && !IsOwned`** 的缺失成员数 `missingCount`：
    - 若 `missingCount == 0`（已集齐）：按钮隐藏/置灰「已集齐」；
    - 若 `missingCount == 1` 且该系列总成员 ≥ 2 且已拥有 ≥ 1：**同样享受 8.5 折补齐价**（如差最后 1 个 = 85 灵石，专门撬动“差 1 个集齐合篇”的临门一脚）；
    - 总价公式（整数向上取整防 0 元漏洞）：`totalDiamond = (missingCount * unitPrice * discountBps + 9999) / 10000`，仅扣真实充值灵石 `CURRENCY_TYPE_DIAMOND (1)`，严禁混扣绑定灵石。
- **协议与幂等**：
  - `CharacterAppearanceSeriesProgress` 末尾追加 `uint32 missing_unlockable_count = 11;`、`uint64 bundle_original_diamond = 12;`、`uint64 bundle_discount_diamond = 13;`、`uint32 bundle_discount_bps = 14;`。
  - `SceneAppearanceClientPlayer` 末尾追加 `rpc BuyAppearanceSeriesBundle (BuyAppearanceSeriesBundleRequest) returns (BuyAppearanceSeriesBundleResponse);`，带非 0 `client_request_id`（`ClientRequestKind` 新值）+ `series_id` + `expected_missing_ids` + `expected_price_diamond`（报价防呆：若期间另获某角色导致缺口变小，拒回价格变化并刷新，绝不多扣 1 灵石！）；原子扣灵石 + 批量置位 `appearance_owned_bits` + 记 `TX_APPEARANCE_SERIES_BUNDLE`。

---

### 2.2 批次 B2：战斗排面三件套 × 全城同乐社交耗材 × 好友纯外观/手记赠礼

#### B2-1. 战斗排面三件套（零数值、低美术成本、全服高频曝光）
不新建 3 个碎片窗口，统一收口为 **衣柜第 4 页签「排面装饰」**（或衣柜顶部子页签扩展），共用一套通用轻量外观位图表 `CosmeticShowcase.xlsx`（`cfg_ids_permanent = true`, `cfg_bit_index = true`），彻底贯彻“数据驱动 + 单一衣柜”：
- **表结构 `CosmeticShowcaseTable`**：`id (cfg_bit_index)`、`cosmetic_key`、`slot_kind`（`1 = 五行阵法脚底阵纹`, `2 = 擂台/会武入场与胜利演出`, `3 = 聊天气泡样式`, `4 = 战报/残局分享卡片底纹`, `5 = 个人名片底纹`）、`name`、`asset_key`、`secondary_asset_key`、`card_item_id`（`3417 排面装饰券`，百宝阁定价 **80–120 灵石**，或 `0` 表示手记/月卡/荣誉专属）、`card_item_count`、`spirit_shop_goods_id`、`enabled`、`sort_order`、`desc`。
- **持久化**：
  - `PlayerProfileComp` 追加 `bytes showcase_cosmetic_owned_bits = 18;`、`map<uint32, uint32> equipped_showcase_cosmetics = 19;`（`slot_kind -> CosmeticShowcase.id`）；
- **全场景透传（零重型改造）**：
  - **战斗快照**（`BattlePlayerSnapshot` & `BattleActorState` 尾插字段）：透传 `formation_skin_id`（阵纹，全场 20 回合脚底显示给对手/队友/L2 全服观战）与 `entrance_victory_skin_id`（进场旗帜/胜利结算徽章，兼容生死擂/人宠会武/群仙问道赛）；
  - **聊天消息**（`ChatMessage` 尾插 `uint32 bubble_skin_id = 6;`、`uint32 share_card_skin_id = 7;`）：由服务端权威校验填充（绝不信客户端自报 skin_id），世界/帮会/私聊与 `#TJC` / `#PUZ` 卡片天然生效。

#### B2-2. 「全城同乐」社交氛围耗材（天墉灵灯 / 庆功烟花 / 仙茶席）
- **玩法闭环（最健康、零逼氪的 MMO 复购引擎）**：
  - 在 `SpiritShopGoods.xlsx`（`tab_id = 3 · 外观便利`）新增 3 款绑定社交气氛道具（`Item.xlsx` 新增 `3418 天墉灵灯` 20 灵石 / `3419 庆功烟花` 50 灵石 / `3420 仙茶同乐席` 60 灵石，均 `bound = true`）：
  - 玩家在主城和平线（归元城/天墉城）使用后：
    1. 原子销毁 1 个道具，记 `TX_SOCIAL_CELEBRATION_CAST`；
    2. 在当前场景分线生成 1 个轻量运行时共享触点（持续 120 秒，单分线同屏并发上限 5 个防视觉污染，不落库不新增 DB 表），向同分线 AOI 广播特效演出，并由 `chat` 白名单模板发 1 条温和同乐播报（如 `【归元同乐】<玩家名> 燃放了庆功烟花，邀同城道友共沾喜气！`）；
    3. 同分线 ≥ 10 级玩家（含释放者本人）主动点击即可每人领取 1 次小额 **`CURRENCY_TYPE_BIND_GOLD = 3`（绑定银两）**（例如灵灯 150 / 烟花 300 / 仙茶席 400 绑银；每场每角色幂等限领 1 次）；
    4. **反「上班」与防刷双重铁闸**：个人每周通过全城同乐领取的绑定银两设**账号级周上限（默认 3,000 绑银/周，UTC+8 周一 05:00 重置）**，领满后仍可观赏特效点赞但不再发绑银，彻底杜绝“挂机蹲主城等别人放烟花”或小号互刷。
- **契约**：新增独立轻量 RPC（归属 `SceneAppearanceClientPlayer` 或 `SceneActivityClientPlayer` 末尾追加 `CastCityCelebration` / `ClaimCityCelebrationGift` + S2C `NotifyCityCelebration`），完全复用主 EventLoop 内存状态 + `PlayerTravelPassComp` 同组或 `PlayerJianghuComp`（PROFILE 组）记周领计数。

#### B2-3. 好友间「纯外观 / 手记典藏券」真实灵石赠礼（打破单号消费上限）
- **允许赠送白名单（严格收窄，绝不触碰经济红线）**：
  - **仅允许**赠送以下 4 类 100% 永久绑定、0 数值、0 经济产出的目标：① 角色永久形象（`CharacterAppearance` 且 `card_unlockable == 1`）、② 灵宠幻化（`PetCosmetic` 可购项）、③ 技能特效（`SkillVfxCosmetic` 可购项）、④ 游历手记指定期次典藏权（`TravelPassSeason`）或排面装饰（`CosmeticShowcase`）。
  - **永久禁止赠送**：任何材料包、定星保底符、天工秘墨、银两、可交易道具、月卡（防低价黑卡代充洗钱）。
- **四重防黑卡与防骚扰硬闸（核心安全设计）**：
  1. **好友门槛 + 双向未拉黑 + 受赠方未拥有校验**：必须经 `go/friend` 校验双方互为好友满 **7 天**且无拉黑；受赠方已拥有该外观/已解锁该期手记时直接拒绝，绝不浪费灵石；
  2. **仅扣真实充值灵石 + 平台渠道闸**：仅扣 `CURRENCY_TYPE_DIAMOND (1)`；且受赠物在 `GiftOrder`（落 `go/trade` 或 `data_service` 轻量审计表 `player_cosmetic_gift_log`）中绑定送礼方近期充值来源窗口；
  3. **退款精准连坐回收（彻底封死黑卡代送）**：当送礼方触发 `TX_PAYMENT_REVERSAL`（渠道拒付/恶意退款）且其自身灵石余额不足抵扣欠款时，风控/客服撤销链可按 `gift_id` 向受赠方 `scene`（通过既有 `player_inbox` 可靠投递域！）发送一条 **`REVOKE_GIFTED_COSMETIC`** 指令，**直接清零受赠方该外观位图位（或回退未领手记典藏态）**并邮件告知“因赠礼方支付撤单，该赠品已按规则收回”，同时封停送礼黑卡账号——因纯外观不涉及第三方银两流通，做到 100% 无损止损、零三角坏账；
  4. **单日/单周赠礼频控**：单角色每周最多送出 5 笔（防盗号清光灵石），且收礼方未手动点「收下」或有拒收陌生赠礼开关时零打扰。
- **跨服/离线架构（遵循 Actor 边界，绝不双人直接改存档）**：
  - 送礼方 `scene` 扣灵石落盘 → 通过既有可靠 `player_inbox`（或 `go/trade` 托管单）向受赠方投递 `GIFT_COSMETIC_CREDIT`；受赠方在线立即置位到账、离线记入收件箱上线自动入账；若竞态发现受赠方期间已自己买过同一外观，自动通过 `player_inbox` 原路全额退还送礼方灵石（幂等键防双退）。

---

### 2.3 批次 B3：无焦虑扩容便利 × 帮徽定制 × 横向新技能卷宗

#### B3-1. 无焦虑扩容便利（零重复收费、严守 7 套三合一既有方案）
| 扩容项 | 零氪免费基础 | 付费扩容上限与单价（真实灵石） | 技术复用与一致性说明 |
|---|---:|---|---|
| **人物仓库格扩展** | 现有默认容量（足够自用毕业） | 每次 +20 格，最多扩 5 次（+100 格），**60 灵石/次** | `BagAllData.capacities[kWarehouse=1]` **已原生持久化**！零 DB 迁移，仅加 `ExpandBagCapacity` RPC + 幂等键 + `TX_BAG_EXPAND` |
| **灵宠编队快捷预设** | 2 套免费（日常 PVE + 会武 PVP） | 第 3–5 套永久解锁，**60 灵石/套**（或随轻享卡暂赠 +1 套） | 在 `PlayerPetComp` 尾插轻量 `repeated PetSquadPreset squad_presets`（只存 `pet_id` 引用，遵循实例与布局分层，放生/交易自动过滤） |
| **精彩战报 & 配装收藏位** | 现有 12 条免费（`PlayerSeasonComp.saved_builds`） | 每次 +12 条，最多扩至 48 条，**60 灵石/次**；同时支持收藏夹锁定战报录像 `ref_count` 永久置顶不随 30 天淘汰 | 复用 `PlayerSeasonComp` 与 `match_battle_report.ref_count`（与残局坊标准解同源，删角 `RoleFencePurge` 同步释放引用） |
| **帮会徽记 / 旗帜定制外观** | 现有默认免费帮徽 | 帮主自愿以个人真实灵石买断（**180–300 灵石/款**，解锁后归该帮会永久使用，0 属性，绝不扣帮会资金、绝不催帮众捐钱） | `go/guild` 帮会表追加 `emblem_skin_id` + `unlocked_emblem_bits`；扣费走 `ASSET_OP_STREAM_GUILD_DEBIT`（个人真实灵石），零新增微服务 |

#### B3-2. 《率土》式横向新主题技能卷宗（第 8 卷常规 + 第 9 卷稀世）
- 严格遵循 [equipment-quality-and-skill-crafting-v1.md §2.2](equipment-quality-and-skill-crafting-v1.md) 四条保值铁律：
  - **绝不稀释老卷宗 `1..7`**，新开 `scroll_id = 8`《五行合纵卷》（10 项横向协作/反制常规战法，0 纯倍率上位替代）与 `scroll_id = 9`《合纵绝学卷》（1 项稀世绝学，占比 `1 / 11 = 9.1% <= 10%`）；
  - 全部通过既有 `Skill.xlsx` + `SkillScrollCraft.xlsx` 配表驱动，零新系统代码，直接自然带动中后期老玩家复购「天工秘墨（180 灵石）」「定星保底符（60 灵石）」与 7 套流派席位续租。

---

### 2.4 批次 B4：国内无版号小游戏纯 IAA 激励视频 3 大按需爽点位

> **零拒审红线（一票否决）**：全部 4 个激励视频槽位在 `RewardedAdSlot.xlsx` 默认 **`enabled = 0`**，首审包 100% 不可见；App 首审包继续保持 `MMORPG_ENABLE_APP_AD_SDK` 关闭（0 广告 SDK / 0 ATT 残留）；所有触发点均**永不自动弹窗、永不倒计时、永不写“支持作者/帮点广告”、确认窗强制等权「观看广告领取」与「关闭」双按钮、未就绪彻底隐藏**。

在 `RewardedAdSlotTable` 末尾追加 `uint32 reward_kind = 12;`（枚举防歧义：`1 = 绑定银两 [既有]`, `2 = 五行幻境 V2 本局奇策免费重随`, `3 = 衣柜额外 1 小时免费试穿`, `4 = 神秘商人个人货架免费刷新`），4 槽位全走 UTC+8 周一 05:00 周上限（反「上班」无日清）：

| `slot_id` | 业务名 | `reward_kind` | 触发场景（仅按需静默出现） | 周上限 | 经济与防刷硬边界 |
|---:|---|---:|---|---:|---|
| **1**（既有） | 观影绑定银两小礼 | `1` | 百宝阁/绑定养成缺绑银静默角标 | 14 次/周 | 仅发 `CURRENCY_TYPE_BIND_GOLD = 3`（300/次），0 流通通胀 |
| **2**（新增） | 幻境奇策重随 | `2` | 五行幻境 V2 三选一奇策弹窗底部静默小按钮 | 7 次/周（且**每局幻境限 1 次**） | 仅重随当次三选一 `pending_offer_sigil_ids`（确定性盐值 `event_reroll_count + 1`），出图清零，0 资产产出 |
| **3**（新增） | 衣柜续时试穿 | `3` | 已用过 1 小时试穿的未拥有角色页静默小按钮 | 7 次/周 | 授予指定未拥有角色 1 小时临时试穿，**绝不置位永久拥有位图**，0 资产产出 |
| **4**（新增） | 神秘商人个人换货 | `4` | 与神秘商人同线打开商店且想换一批货时静默小按钮 | 5 次/周（且每轮商人限 1 次） | 仅按 `(player_id, round_id, personal_reroll_seq)` 确定性重抽该玩家本轮可见货架（仍严格遵循神秘商人原币种与限购，不刷可交易套利漏洞） |

- **协议最小增量**：`ClaimRewardedAdRequest` 末尾追加 `uint32 target_param_u32 = 5;`、`string target_param_str = 6;`（槽位 2 校验当前 `run_id` 在待选策态；槽位 3 传目标 `appearance_id`；槽位 4 校验 `round_id` 且同线在 `interact_range` 内），完全复用现有 `BeginRewardedAdSession` + `ClaimRewardedAd` 落盘抗断线重连闭环。

---

### 2.5 批次 B5：海外 + Steam PC 发行变现底座（绕开国内版号空窗）

1. **渠道支付与发行形态隔离矩阵**：
   | 发行通道 | 适用阶段 | 允许开启的变现模块 | 强制关闭的模块 |
   |---|---|---|---|
   | **微信 / 抖音小游戏（无版号期）** | 国内首测/无版号期 | 仅二版达标后开官方流量主 IAA（B4 四槽位） | **彻底隐藏全部 IAP 充值、百宝阁灵石充值引导、赠礼与 DLC** |
   | **港澳台 / 东南亚 / 全球 iOS & Google Play** | 立即可上（免国内版号） | 全量开启 B1–B3（灵石充值走 Apple IAP / Google Play Billing，绝无第三方切支付）；可选开启 B4 或保持零广告纯净版 | 绝不含微信/支付宝/外链捐赠 |
   | **Steam PC 版（Windows / macOS）** | 立即可上（免国内版号） | Steam Microtransactions 充灵石 + **透明 DLC 支持者包**（详见下表）+ 社区口碑自然流量 | 绝不放任何广告（Steam 玩家极反感内置广告） |
2. **Steam 专属透明 DLC 清单（100% 复用现有灵石/衣柜/手记资产，0 战力加成）**：
   - **「留灯院创始支持者典藏包」DLC（参考价 ¥48 / \$6.99）**：一次性发放 90 天仙途季享卡 + 3 张绑定角色形象卡 + 1 套限定阵纹「星河初启」+ 1 枚限定称谓「初灯同道」（经 Steam AppOwnership 校验 → `go/payment` 新增 `steam_dlc` 核验器 → 走既有 `java/mall_game` 幂等单次发放，退款联动撤销）；
   - **「五行奇谈 · 电子设定集与原声音乐」DLC（参考价 ¥18 / \$2.99）**：纯局外音频与美术画册，零服务端改动。

---

## 三、思想分析

1. **为什么把 10 项需求按 B1→B5 拆成 5 个独立可验收批次，而不是一口气一把梭改 4 个仓库？**
   - 每一批都有独立、可量化的营收目标和独立回滚边界：B1 几乎不改底层大架构，最快解决“买穿与月卡无感”；B2 打开外观与社交复购天花板；B3 靠横向内容和整理便利托底中后期；B4 专门解决国内无版号空窗现金流；B5 直接打开海外与 Steam 增量大盘。小步闭环既能随时真机验收，又能避开多会话并发改表的合并灾难。
2. **为什么选“永不绝版手记书架”而不是“限时绝版 FOMO 战令”？**
   - 独立/小团队 MMO 买量预算有限，最宝贵的资产是玩家自发口碑和老玩家回流。限时绝版战令能骗首月一波恐慌氪金，却会把所有第 2 个月入坑的新人和出差归来的老玩家推走；永不绝版书架把每一期开发投入沉淀成**复利型内容资产**（运营越久，新玩家面前的优质 ¥18 书单越厚），同时 100% 忠实于“有空就玩、随时能停”的项目灵魂。
3. **为什么外观优先做阵纹 / 入场演出 / 聊天气泡 / 分享卡底纹，而不是继续只堆 40 帧灵宠幻化？**
   - 单位美术工时的营收效率差一个数量级：1 套灵宠幻化需要 50+ 张 1024×1024 帧图且仅出战该宠可见；1 张阵法脚底阵纹、1 套聊天气泡或 1 张分享卡底纹只需 1–3 张静态透明切图，却在每局 20 回合、L2 全服观战、世界频道和 `#TJC`/`#PUZ` 分享里高频曝光。
4. **为什么“全城同乐烟花”必须发 `CURRENCY_TYPE_BIND_GOLD = 3` 且设周软顶？**
   - 如果发流通银两，就会立刻沦为工作室跨号转移银两的洗钱通道；如果不设周上限，就会把主城变成“每天挂机蹲大佬放烟花”的上班打卡点。用绑定银两 + 周软顶把经济与反肝风险一次性焊死，剩下的全是纯粹的社交爽感与复购。

---

## 四、漏洞分析与强制防线总表

| 编号 | 潜在漏洞 / 风险场景 | 危害 | 强制防线（单测必覆盖） |
|---|---|---|---|
| **M-1** | **手记书架同时开多期，打 1 场战斗给所有期次同时加点** | 10 场刷满 6 期奖励，崩坏进度节奏 | 战斗结算只读取唯一的 `active_pass_id` 注入点数；满点后仅顺延单次剩余或下次结算切至下一未满期次，绝不同时多期并发加点。 |
| **M-2** | **月卡/季享卡反复叠加刷无限开卡直赠定星保底符** | 300 灵石叠买 6 次变相无限买打折定星符，且满包丢奖励 | ① 叠加上限继续硬锁 `max_remaining_days = 180`；② 每次购卡前用 `ExchangeInventoryExact` 整批预检背包空间（满包零扣灵石）；③ 纪念头像框按位图幂等授形，不折现、不重复发。 |
| **M-3** | **衣柜系列补齐包在玩家并发刚抽/刚换得其中 1 人后仍按旧缺口扣费** | 玩家多花冤枉钱 | 请求必须携带 `expected_missing_ids`（排序去重）与 `expected_price_diamond`；服务端现算缺失列表或总价任一不符即拒回价格变化并刷新快照；价格计算向上取整防 0 灵石漏洞。 |
| **M-4** | **黑卡/苹果恶意退款买外观赠礼，把黑钱洗白成大号永久皮肤** | 平台背负渠道坏账，黑灰产低价代充横行 | ① 仅允许赠绑定纯外观/手记券，严禁赠材料/银两/月卡；② 强制互加好友 ≥ 7 天且双向未拉黑；③ `player_cosmetic_gift_log` 登记赠礼单，触发 `TX_PAYMENT_REVERSAL` 欠款时通过 `player_inbox` 发送 `REVOKE_GIFTED_COSMETIC` 清除受赠方外观位图位并封停送礼黑卡号。 |
| **M-5** | **全城同乐烟花/灵灯被用于小号刷绑银或刷屏炸毁主城帧率** | 视觉卡顿、客户端崩溃或小号挂机刷绑银 | ① 同分线同时活跃同乐特效硬上限 5 个（超出提示稍后再放、不扣道具）；② 仅发 `CURRENCY_TYPE_BIND_GOLD = 3`；③ 每账号每场限领 1 次 + 每周硬封顶 3,000 绑银（UTC+8 周一 05:00 重置）。 |
| **M-6** | **客户端伪造未拥有的阵纹/聊天气泡/分享卡底纹 ID 广播给全服** | 白嫖外观或发送非法 ID 导致他人客户端报错 | 进战快照（`Scene.PrepareBattle`）与聊天发包（`Chat` 查权威装饰投影或由 `Scene` 签发）严格按 `showcase_cosmetic_owned_bits` 白名单二次过滤，未拥有或已下架项无声回落 `0`（默认样式）。 |
| **M-7** | **激励视频肉鸽奇策重随（Slot 2）反复看广告 SL 刷同一局直到全仙品** | 破坏肉鸽构筑平衡与榜首公平性 | ① 每局 `WuxingTower`（按 `run_id`）硬锁最多重随 **1 次**（存档增加 `ad_reroll_used_run_id`）；② 使用确定性盐值推进，不可回滚；③ 全周上限 7 次。 |
| **M-8** | **国内无版号小游戏包体误露出百宝阁充值引导、赠礼入口或 Steam DLC** | **触发微信/抖音小游戏直接拒审或永久下架** | 服务端按 `client_platform` + 区服发行模式开关下发功能可见位，无版号小游戏端物理关闭全部真钱入口与赠礼入口；激励视频全 4 槽默认 `enabled = 0`，继续走 `RewardedAdBuildGuard` 与静态违禁词单测门禁。 |
| **M-9** | **并行会话脏工作区冲突（2026-11-27 实况）** | 当前 `xuanming-server-mmo-cpp`（`client_request_ledger.*` / `player_handoff_gate.h` / `player_skill.proto`）、`xuanming-server-mmo`（`game-guild` / `SkillCraftService`）与 `mmorpg-client`（`BattleSpectateChat*`）均有并行会话未提交改动，强行生成协议会混入或覆盖他人进度 | **本轮先 100% 无冲突落盘总设计 + Codex 纯功能 UI 交接单**；代码实现按 B1→B5 顺序，在确认工作区合并边界或取得负责人拍板后再逐批动代码并跑全仓生成/编测。 |

---

## 五、步骤分析（5 批次分阶段推进与验收门槛）

| 批次 | 改动范围 | 前置依赖 | 核心验收门槛（四仓对齐） |
|---|---|---|---|
| **阶段 0（本轮已完成）** | 落盘本总方案 + [Codex UI 交接单](ui-handoff/fair-monetization-expansion-v2-ui-handoff.md) + 更新 `gen_docs_index.py` & `docs/README.md` | 无 | `python tools/scripts/gen_docs_index.py --check` 退出 0；零触碰并行脏文件 |
| **批次 B1** | ① `SpiritShopGoods.xlsx` 上架 `203(3409)`、`204(3410)`；<br>② 游历手记多期永不绝版书架（`active_pass_id` + `SelectActiveTravelPass` + 预置 2–6 期配表）；<br>③ 仙途轻享卡多档（30天/90天季享卡）+ 开卡一次性直赠 + `go/trade` 挂单/摆摊倍率 + 月卡周 3 次随心试穿；<br>④ 衣柜系列 8.5 折按缺口补齐包（`BuyAppearanceSeriesBundle`） | 确认 `client_request_ledger.h` / `player_handoff_gate.h` 并行改动收敛 | C++ 静态门禁 + 串行 `msbuild /m:1` + `bag_test`（书架切换/满点顺延、季享卡叠加 180 天、满包零扣钱、补齐包防多扣、随心试穿不授永久权）；Go `go/trade` 单测；Java `ContractSync` + `PARITY.md` + Maven；Python `pytest`；Unity 编译 + NUnit |
| **批次 B2** | ① `CosmeticShowcase.xlsx` + 战斗阵纹/入场胜利演出/聊天气泡/分享卡底纹全链路透传；<br>② 「全城同乐」灵灯/烟花/仙茶席（同线限量 5 + 绑银周上限 3,000）；<br>③ 好友纯外观/手记赠礼 + `player_inbox` 离线到账与 `TX_PAYMENT_REVERSAL` 精准回收 | B1 验收通过 + Codex 交付 B2 静态切图 | 四仓单测覆盖未拥有外观回落、烟花并发/周顶拦截、7 天好友/防重复送/退款清位图回收 |
| **批次 B3** | ① 仓库扩容（复用 `BagAllData.capacities`）、灵宠编队预设、战报收藏扩容（联动 `ref_count`）、帮会徽记定制（`go/guild` 个人灵石扣费）；<br>② 新增第 8/9 卷横向机制技能卷宗（稀世 ≤ 10%） | B2 验收通过 | 四仓单测覆盖扩容上限与幂等、放生灵宠编队清理、删角释放战报引用、稀世占比 ≤ 10% 校验 |
| **批次 B4** | 扩展 `RewardedAdSlot` 支持 `reward_kind = 1..4`（默认全 `enabled = 0`），接入幻境 V2 单局 1 次重随、衣柜续试穿、神秘商人个人换货 | B1 验收通过 | `RewardedAdModelAndComplianceTests` 零诱导词与首审全隐藏通过；幻境单局限 1 次重随与周上限四仓单测全绿 |
| **批次 B5** | 新增 `go/payment` `steam_dlc` 核验器与海外/Steam 发行配置模板、Steam 支持者典藏包 SKU 映射与合规发包清单 | 海外/Steam 开发者账号就绪 | `go/payment` mock 发放与退款撤回集成测试全绿；国内无版号小游戏端功能开关隔离验证通过 |

---

## 六、执行步骤（逐批开工标准流水线）

1. **阶段 0（本轮已执行）**：
   - [x] 核查全四仓现役表/协议/C++ 规则与 `git status` 并行脏文件边界，排除与既有 7 套三合一席位及帮会资金改名的重复设计；
   - [x] 核实并查看图仓全部 7 张长期标准参照图，落盘本总方案与异机 Codex 纯功能作图交接单；
   - [x] 更新 `tools/scripts/gen_docs_index.py` 分类规则并运行生成/校验 `docs/README.md`。
2. **等待并行工作区收敛 / 负责人确认开工信号后，立即启动批次 B1 编码**：
   - 先执行 B1 权威配表补丁脚本与 `.proto` 追加（`player_travel_pass_comp.proto`、`player_activity.proto`、`afkpassrule_table.proto`、`player_afk_pass.proto`、`appearanceseries_table.proto`、`player_appearance.proto`、`player_comp.proto`、`Tip.xlsx`、`MessageLimiter.xlsx`），跑 `dev.bat gen`；
   - 按 C++/Go（`scene` + `trade`）→ Java（`ContractSync` + `game-scene`/`game-trade` + `PARITY.md`）→ Python（增量同步 + `pytest`）→ Unity 客户端（`.\gen.bat client` + `*Models/*Client/*Window` + NUnit）完成 B1 四仓闭环并追加 `docs/PROGRESS.md`；
   - 随后按同样门禁纪律依次闭环 B2 → B3 → B4 → B5。
