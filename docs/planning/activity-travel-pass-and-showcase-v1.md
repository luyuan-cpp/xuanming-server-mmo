# 《五行奇谈》活动界面拉收入体系：「游历手记（不肝不逼氪双轨）」与体验种草设计（V1）

> 日期：2026-11-25  
> 状态：**负责人已拍板全面落地（权威 C++/Go 服务端 + Unity 客户端 + Java/Python 对齐 + 异机 Codex 美术作图交接）**  
> 上位红线：[项目设定](project-vision.md)（好玩优先 / 真不肝 / 小小氪金零氪同上限 / 官方 7:3 / 物价稳得住 / 防骗与资金安全）  
> 衔接文档：[核心循环 V1](core-gameplay-loop.md) · [经济与官方 7:3 总纲](equipment-economy-charter.md) · [官方灵石百宝阁 V1](spirit-shop-and-starter-packs-v1.md) · [角色永久衣柜](character-appearance-switch.md) · [瑞兽迎春 V1](battle-festival-beast-v1.md) · [活动中心合同](../design/player-features-ui.md)

---

## 1. 需求分析

### 1.1 问题背景与核心矛盾
1. **活动界面已有壳子但缺少高频、体面的创收承载**：
   - 客户端活动中心（`GameplayWindow.Activities.cs`）与 `GetActivityList` 已具备完整卡片列表、筛选与活跃进度条骨架，但目前仅挂接了未启用的旧占位任务 `15/16/17` 与限时活动「瑞兽迎春」，常态打开几乎为空。
   - [官方灵石百宝阁 V1](spirit-shop-and-starter-packs-v1.md) 已落码明码直购货架（6–30 元首套提速包、定星保底符、天工秘墨、10 元角色形象卡、改名卡），但玩家缺少一个**不逼氪、不肝、自然游玩即可获得正向反馈、并在体验到爽点后心甘情愿掏 18 元支持作者**的活动引擎。
2. **传统手游活动直接照搬会踩碎全项目底线**：
   - 累充返利、首充双倍、连登 7 天付费补签、限时倒计时弹窗、抽卡转盘、每日 100 活跃打卡均直接违背「**不要强制、不要肝氪**」与 [project-vision.md](project-vision.md) 红线。
3. **本设计要同时兑现的四件事**：
   - **就放在现有活动界面里**：复用 `GetActivityList` 入口与卡片流，保持既有活动界面原图还原测试 100% 不破。
   - **真不肝、不强制**：零每日任务、零连续登录、零周清欠账、零未购买催命红点；玩任意正式战斗自然累积进度；**先玩后买、解锁后永不过期**。
   - **真不逼氪、零氪同上限**：免费轨送满绑定实用好礼与定星保底体验装；典藏轨（¥18 = 180 真实灵石）100% 零战力上限突破、0 可交易货源、0 打造/百炼石（不打破 660 首套与百宝阁 300 提速锚）。
   - **真能拉起官方 7:3 收入**：将「青云游历手记（¥18 典藏轨）」作为活动界面主创收活动，搭配「留灯风物 · 免费试穿（1 小时试用种草 10 元永久衣柜）」「灵石百宝阁 · 轻便利导览」「瑞兽迎春（3 胜免费领神兽）」形成自然种草闭环。

---

## 2. 设计分析

### 2.1 方案选型对比（Approaches）
- **方案 A（纯体验种草引流）**：只放免费体验卡导流至衣柜与百宝阁；改动最小，但缺少专属 18 元情绪承接单品，单次活动拉动偏温和。
- **方案 B（无日清、先玩后买的「游历手记」双轨活动）**：正常战斗自动积点（无日限、可先打满再补票、买断永不过期），免费轨给绑定实用礼 + 保底体验，¥18 典藏轨给限定头像框/称谓 + 永久形象卡 + 改名卡 + 绑定配套道具；不逼氪赛道付费率与口碑最优。
- **方案 C（活动主题限时直购卡）**：活动页并列 6–18 元主题直购包；虽见钱快但易像“把商城搬进活动页”，长线复购弱于 B。
- **最终拍板**：**方案 B（青云游历手记）为主引擎 + 方案 A（留灯风物免费试穿 / 百宝阁轻便利导览 / 瑞兽迎春）为免费人气与种草矩阵**，全部聚合在现有活动界面里。

### 2.2 核心活动矩阵（聚合进 `GetActivityList`）

| `activity_id` | `entry_kind` | 分类 | 卡片名称 | 行为与红点规则 |
|---:|---|---|---|---|
| `900001` | `PLAYER_ACTIVITY_ENTRY_TRAVEL_PASS (3)` | 福利 (`3`) | **青云游历手记 · 第一期** | 展示当前点数 `X/300` 与典藏状态；**仅当存在可免费领或已解锁可领的典藏档位时 `has_claimable_reward = true`**（未买典藏绝不刷假红点）；点击打开独立「游历手记」主窗 |
| `900002` | `PLAYER_ACTIVITY_ENTRY_APPEARANCE_SHOWCASE (4)` | 福利 (`3`) | **留灯风物 · 免费试穿** | 实时展示当前已拥有现役角色数（如 `2/9`）与「每位未拥有角色享 1 小时免费试用」说明；`has_claimable_reward = false`；点击直达永久衣柜 `AppearanceUiRoot` |
| `900003` | `PLAYER_ACTIVITY_ENTRY_SPIRIT_SHOP_SHOWCASE (5)` | 福利 (`3`) | **灵石百宝阁 · 轻便利** | 仅在 `SpiritShopGoods` 有启用在售商品时出现，实时展示商品数与首套限购余量，写明「绑定仅自用 · 0 战力加成」；`has_claimable_reward = false`；点击直达 `SpiritShopUiRoot` |
| 既有 `202601` | `PLAYER_ACTIVITY_ENTRY_FESTIVAL_BEAST (2)` | 限时 (`2`) | **瑞兽迎春** | 保持既有逻辑，排期开启时自动并列显示 |

### 2.3 「青云游历手记 · 第一期」规则与档位清单

#### (1) 进度获取（复用正式战斗权威结算，彻底不肝）
- 触发点：scene 侧战斗结算原子提交（`PlayerBattle` 中与 `PlayerSeasonSystem::PrepareBattleCredit` 并列），**绝不另设每日打卡或登录签到任务**。
- 合法性校验：逃跑、作废、切磋、零回合均得 `0` 点；PVP 负/平沿用既有四重防挂机门槛（`total_rounds >= 3 && manual_action_rounds >= 2 && effective_combat_contribution`）：
  - 合格普通 PVE / 首领试炼普通 / 护卫救援 / 瑞兽迎春 / 帮会战胜局：**+20 点**
  - 合格困难首领试炼胜局 / 正式 PVP（含物法易势）胜局：**+30 点**
  - 合格达标 PVP 平局 / 负局：**+15 点**
- 封顶与节奏：总需求 **`300` 点**（10～15 场正常战斗即可拿满，约 1.5～2.5 小时；无每日上限、无每周清空）。
- **已解锁永不过期防套牢规则**：
  - 玩家未解锁典藏轨时，在期次开放窗口内（`starts_at_ms == 0` 或 `now >= starts_at_ms`，且 `ends_at_ms == 0` 或 `now < ends_at_ms`）正常积点与领免费轨；期次结束后，**已达成但尚未领取的免费档位仍永久可补领**。
  - 玩家一旦解锁典藏轨（`premium_unlocked = true`），即便期次已过 `ends_at_ms`，只要该期尚未满点或尚有未领档位，**继续正常战斗仍为该期积点，且永久保留全部领取资格**。

#### (2) 典藏解锁（¥18 = 180 真实灵石，先玩后买）
- 价格：`premium_currency_type = 1`（真实充值灵石 `CURRENCY_TYPE_DIAMOND`，**严禁绑定灵石混扣**），`premium_price = 180`。
- 允许在任意进度（`0/300` 直到 `300/300`）随时解锁；解锁后先前已达成的典藏档位立即全部变为「可领取」，支持「一键全部领取」。

#### (3) 首期 10 档奖励明细（全部物品强制 `bound = true`，0 可交易货源，0 战力上限突破）

| 档位 (`tier_index`) | 所需点数 | 免费轨奖励（人人可领） | 典藏进阶轨奖励（180 灵石解锁，全绑定） |
|---:|---:|---|---|
| 1 | 30 | 银两 × 5,000 | **限定头像框「青云游历框」** (`frame_travel_qingyun_s1`) + 定星保底符 (`3405`) × 1 |
| 2 | 60 | 绑定重铸砂 (`3403`) × 2 | 银两 × 10,000 |
| 3 | 90 | 绑定悟道残页 (`3406`) × 10 | 绑定角色改名卡 (`3401`) × 1 |
| 4 | 120 | 银两 × 8,000 | 绑定重铸砂 (`3403`) × 3 |
| 5 | 150 | **绑定定星保底符 (`3405`) × 1**（免费体验种草） | **绑定角色形象卡 (`3402`) × 1**（衣柜任选买断 1 位角色） |
| 6 | 180 | 绑定重铸砂 (`3403`) × 2 | 绑定定星保底符 (`3405`) × 1 |
| 7 | 210 | 银两 × 10,000 | 绑定绝学残卷 (`3407`) × 1 |
| 8 | 240 | 绑定悟道残页 (`3406`) × 15 | 绑定重铸砂 (`3403`) × 3 |
| 9 | 270 | 绑定重铸砂 (`3403`) × 2 | 绑定天工秘墨 (`3408`) × 1 |
| 10 | 300 | 银两 × 15,000 + 免费称谓「游历初成」(`title_travel_free_s1`) | **绑定角色形象卡 (`3402`) × 1** + 典藏称谓「青云雅客」(`title_travel_qingyun_s1`) |

### 2.4 配表、持久化与协议契约（已核对 2026-11-25 现役编号）

1. **新增 2 张权威配置表**（零触碰已有脏表）：
   - `data/schema/travelpassseason_table.proto` + `data/TravelPassSeason.xlsx`（期次、时间窗、点值、解锁货币与价格、配置修订）
   - `data/schema/travelpasstier_table.proto` + `data/TravelPassTier.xlsx`（`pass_id` 外键指向 `TravelPassSeason`，`tier_index 1..64` 用于每期 64 位领奖掩码，遵循 [bit-storage-and-table-ids.md](../rules/bit-storage-and-table-ids.md) 高频/分期活动隔离存储规范）
2. **新增持久化组件**：
   - `proto/common/component/player_travel_pass_comp.proto`（`PlayerTravelPassComp`，含按 `pass_id` 隔离的 `TravelPassSeasonProgress`、每期 `uint64 free_claimed_mask / premium_claimed_mask`、已解锁纪念外观键列表与当前佩戴头像框/称谓）。
   - 挂载到 `proto/common/database/mysql_database_table.proto` 的 `player_database` 末尾空闲号 **`35`**，归属 **`WALLET (7)`** 组：与货币、幂等账本同行，与背包同事务直提落盘（I3 不变量）；加载无条件挂载、旧档缺失视为空合法、存盘缺组件 fail-closed 拒存。
3. **客户端协议扩展**（`proto/scene/player_activity.proto` 末尾追加，不改变既有消息号与 method index）：
   - `PlayerActivityEntryKind` 追加 `PLAYER_ACTIVITY_ENTRY_TRAVEL_PASS = 3`、`PLAYER_ACTIVITY_ENTRY_APPEARANCE_SHOWCASE = 4`、`PLAYER_ACTIVITY_ENTRY_SPIRIT_SHOP_SHOWCASE = 5`。
   - `service SceneActivityClientPlayer` 末尾追加 4 个 RPC：
     - `GetTravelPassState`（只读快照：期次、点数、典藏状态、各档领取态、双余额、已解锁与已佩戴纪念头像框/称谓）
     - `UnlockTravelPassPremium`（强制非 0 `client_request_id` 幂等 + 报价防呆 `expected_currency_type / expected_price / expected_config_revision` + 扣真实充值灵石）
     - `ClaimTravelPassReward`（强制非 0 `client_request_id` 幂等；`tier_index == 0` 表示一键全领当前所有可领免费/典藏档位，`tier_index > 0` 按指定轨领单档；原子预检背包容量后统一发绑定物品、加银两、解锁纪念外观键）
     - `EquipTravelPassCosmetic`（零消耗切换佩戴已解锁的纪念头像框或称谓；传空串卸下）
4. **幂等账本、流水与 Tip 码**：
   - `ClientRequestKind` 追加 `kUnlockTravelPassPremium = 16`、`kClaimTravelPassReward = 17`（同步 `kClientRequestKindNames`）。
   - `TransactionType` 追加 `TX_TRAVEL_PASS_UNLOCK = 45`、`TX_TRAVEL_PASS_REWARD = 46`。
   - `data/tip/Tip.xlsx` 新开独立域 `//travel_pass_error base=38000 width=1000`（13 个语义明确错误码）。

### 2.5 四问准入自检（严格对照 `project-vision.md §二`）
1. **普通零氪玩家**：不花钱、不建小号、隔周回来打两场首领试炼，能否拿到完整 10 档免费好礼与 100% 相同战力上限？——**能**。无每日打卡、无漏签损失、免费轨直接送 38,000 银两 + 6 绑砂 + 25 悟道残页 + 1 定星保底符。
2. **个人开发者收入**：这份改动有没有把核心付费流向白送给工作室？——**没有**。180 真实充值灵石 100% 走全端合规官方灵石链直入官方口袋，并顺带通过免费定星符与免费试穿卡片拉动百宝阁 7 款商品与 10 元永久衣柜转化。
3. **物价与长期循环**：有没有零成本刷可交易货源的口子？——**零口子**。手记所有物品均强制 `bound = true`（仅自用），0 可交易材料、0 打造/百炼石，绝不冲垮市场与首套 300 份提速锚。
4. **防骗与资产安全**：骗子能否利用此入口诱导站外转账或手滑毁装？——**不能**。全部发放为绑定仅自用物品，不涉及玩家间交易；灵石解锁带报价防呆 + 非 0 幂等键 + 客户端二次确认框防误触。

---

## 3. 思想分析

1. **为什么把「先玩后买 + 解锁后永不过期」设为硬机制？**  
   传统通行证令人反感的核心原因不是“18 元太贵”，而是“先交钱再被强迫每天打卡 60 天，漏几天血亏”。把顺序反过来——**平时随便打打试炼/会武就顺手满 300 点，看到 10 格典藏礼全亮再自愿掏 18 元一键领走，且买断后绝不作废**——既守住“不强制、不肝氪”，又把购买心理阻力降到最低。
2. **为什么不直接魔改旧的 `ActivityVitality` 底部活跃条，而是用独立 `TravelPass` 卡片 + 独立面板？**  
   既有 `GameplayWindowTests.cs` 锁定了旧活动面板的原图像素与 `ActivityVitality` 协议边界；沿用 `FestivalBeast` 已经验证过的「`GetActivityList` 入口卡 → 独立 `TravelPassUiRoot` 面板」模式，既零破坏既有像素回归测试，又能给游历手记宽敞清晰的双轨卡片视觉空间，同时在 `GameplayWindow.Activities` 一眼可见实时进度与可领红点。
3. **为什么每期档位领奖用 `uint64` 期次内掩码，而不是全局 `cfg_bit_index`？**  
   遵循 [bit-storage-and-table-ids.md](../rules/bit-storage-and-table-ids.md) §1：按期次迭代的运营活动不应塞进永久单调全局位图；每期独立一条 `TravelPassSeasonProgress` + 两个 `uint64` 掩码（支持最多 64 档），O(1) 判定且未来换期不膨胀全局位图。

---

## 4. 漏洞分析与防线清单

| 潜在漏洞 / 风险 | 硬防线 |
|---|---|
| **用活动赠送的绑定灵石抵扣 ¥18 典藏解锁** | `UnlockTravelPassPremium` 严格按表配 `premium_currency_type`（首期强制为 `1 = CURRENCY_TYPE_DIAMOND`）扣款，绝不用 `BIND_DIAMOND` 兜底。 |
| **弱网重放导致重复扣 180 灵石或重复发奖** | `Unlock` 与 `Claim` 强制 `client_request_id != 0` 并接入 `ClientRequestLedgerComp`（先查重、后执行、成功后同步记账）；重放返回 `replayed = true` 与最新快照，零双扣、零双发。 |
| **配置热更改价瞬间玩家按旧价点确认** | `UnlockTravelPassPremiumRequest` 携带 `expected_currency_type`、`expected_price`、`expected_config_revision`，任一不符拒回 `kTravelPassPriceChanged`。 |
| **一键全领部分档位时背包满导致半发半漏** | `ClaimTravelPassReward` 先聚合所有待领档位的全部物品与银两，通过 `BagService::ExchangeInventoryExact` 整批原子预检并入包（全 `bound = true`），失败返回 `kTravelPassBagFull`，零状态修改。 |
| **挂机送死脚本刷手记点数** | 复用战斗结算权威事实，逃跑/作废/切磋/未达防挂机门槛的 PVP 负局加 `0` 点，且单期严格封顶 `max_points`。 |
| **解锁典藏后遇档期结束血亏** | 服务端对 `premium_unlocked == true` 的历史期次持续允许结算加点与补领，绝不随 `ends_at_ms` 吞掉已购权益。 |
| **未购买典藏却给活动入口亮红点制造焦虑** | `HasClaimableReward` 严格只统计「已达点数且未领的免费档」及「已解锁典藏且已达点数未领的典藏档」，未解锁典藏永不触发红点。 |
| **佩戴未解锁的纪念头像框/称谓** | `EquipTravelPassCosmetic` 严格核对 `unlocked_cosmetic_keys` 白名单集合，未拥有直接拒回 `kTravelPassCosmeticNotOwned`。 |

---

## 5. 步骤分析

1. **步骤 1 · 契约与配表生成**：新建 `TravelPassSeason` / `TravelPassTier` schema 与补丁脚本 `tools/scripts/travel_pass_xlsx_patch.py`，补齐 `Tip.xlsx` `38000` 段、`player_travel_pass_comp.proto`、`mysql_database_table.proto`（字段 35）、`transaction_log.proto`（45/46）、`player_activity.proto`（3 新入口枚举 + 4 新 RPC），执行 `dev.bat gen` + `message-limiter` + `gen_schema_index.py`。
2. **步骤 2 · C++ 权威服务端**：新增 `travel_pass_rules.h`、`player_travel_pass.{h,cpp}`，接通 `player_activity_handler.cpp`、`player_feature_snapshot.cpp`（活动目录 3 张新卡）、`player_database_loader.cpp`、`player_battle.cpp`（战斗结算加点）、`client_request_ledger.h`、`main.cpp`（启动校验），登记 `.vcxproj`/`CMakeLists.txt` 并编写 `player_travel_pass_test.cpp`。
3. **步骤 3 · Unity 客户端全闭环**：运行 `.\gen.bat client` 同步表/协议/消息号，新增 `TravelPassModels.cs`、`TravelPassClient.cs`、`TravelPassUiArt.cs`、`TravelPassWindow.cs`、`TravelPassUiRoot.cs`，接线 `GameClient.cs`、`GameplayWindow.Activities.cs`、`GameplayUiRoot.cs`，新增 `TravelPassClientAndUiTests.cs`。
4. **步骤 4 · Java 与 Python 服务端对齐**：按各仓规范检查未提交改动边界，完成对应游历手记业务逻辑对齐、单测与 `PARITY.md` 登记。
5. **步骤 5 · 门禁与验证**：跑 C++ 静态预检（`test_cpp_no_raw_pointer_member.py` / `test_cpp_thread_sources.py` / `check_log_text_english.py`）、串行 `msbuild /m:1`、`bag_test.exe`、`scene.exe --validate-tables`、Unity `client_compile_check.ps1` 与测试、Java/Python 测试，追加 `docs/PROGRESS.md`。

---

## 6. 执行步骤与异机 Codex 作图交接规格

- 服务端与客户端程序逻辑及默认可用 UGUI（复用既有 Qdao 玉绿/米白金边切图 + `Resources.Load` 可选精修素材钩子）由本轮一次性落码闭环。
- 精修视觉素材路径约定（Codex 交付后直接放进 `mmorpg-client/Assets/Resources/UI/Ugui/TravelPassV1/` 即自动生效，零改代码）：
  - `travel_pass_banner.png`（1080×240，游历手记顶部青云画卷横幅）
  - `travel_pass_card_icon.png`（256×256，活动中心「青云游历手记」入口卡主图标）
  - `showcase_appearance_icon.png`（256×256，活动中心「留灯风物 · 免费试穿」入口卡主图标）
  - `showcase_spirit_shop_icon.png`（256×256，活动中心「灵石百宝阁 · 轻便利」入口卡主图标）
  - `frame_travel_qingyun_s1.png`（256×256 透明中空，「青云游历框」限定纪念头像框）
  - `badge_travel_premium.png`（180×64，典藏轨「典藏」玉金徽章）
  - `tier_card_free.png` / `tier_card_premium.png`（240×170，免费轨素雅宣纸卡底与典藏轨金边玉青卡底）
