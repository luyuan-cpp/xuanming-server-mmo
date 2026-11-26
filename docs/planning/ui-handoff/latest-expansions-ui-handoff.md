# 近期新玩法剩余增量 UI 切片需求清单（论剑 V4 / 灵石百宝阁 / 青云游历手记 / 周常词缀与赛季荣誉 / 野外栖息地）

> 用途：汇总除《群仙问道赛》V3+V4（29 枚，见 [season-team-tournament-v3-ui-handoff.md](season-team-tournament-v3-ui-handoff.md)）外，当前 Unity 客户端已预留加载路径、尚未产出正式切图的 **4 组共 47 枚增量 PNG 切片**（`WuxingExpansionsV1` 18 枚、`PaymentCashierV1` 12 枚、`CombatPetSkillUi` 54 枚与 `guild-newplay-review-20261125` 69 枚均已导入客户端，不再列出）。
> **视觉风格、画法、配色、材质与字体完全以 `D:\work\image` 仓库既有参考图与规范为准，本文档不定义任何美术风格。**

---

## 1. 通用交付技术规范（不含风格要求）

1. **格式**：全部为 PNG-32 透明底（边缘 `alpha = 0`，无棋盘格、无白边、无杂底）。
2. **无字原则**：本文清单中 **全部 47 枚切片均不含任何文字、字母或数字**；所有标题、数值、状态、按钮文案均由 Unity 运行时动态叠加，底板/按钮/卡片/标签中部必须保留清晰可读的文本承载区。
3. **拉伸规范**：
   - 标明「九宫格拉伸」的窗框、面板、卡片，需保持四边边框均匀、中心区域平缓，便于配置 Unity `spriteBorder`；
   - 标明「横向三段拉伸」的进度条轨与填充条，需保持左右端帽清晰、中段可水平拉伸；
   - 标明「等比图标」的切片按 `preserveAspect = true` 展示，主体居中并保留约 8%～12% 安全留边。
4. **交互与安全区**：适配 `1920×1080` 与 `2400×1080` 横屏，右上角预留 `140px` 小游戏胶囊避让区，不依赖鼠标悬停展示信息。

---

## 2. 清单 A · 论剑 / 人宠会武 V4（12 枚）

- 对应代码：`BattleScreen.cs`、`BattleUnitView.cs`、`RankedSettlementPanel.cs`、`RankedModels.cs`、`SeasonEconomyUiArt.cs`
- 输出目录：`D:\work\image\designs\ranked-play-v4\assets\`
- Unity 导入路径：`Assets/Resources/UI/Ugui/RankedPlayV4/`

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `ping_bar_base.png` | `420×84` | `420×84` 九宫格/定宽 | 战内左下队内战术标点浮条底板（容纳 4 枚标点按钮 + 1 枚屏蔽按钮） |
| 2 | `ping_focus.png` | `96×96` | `32×32` 等比图标 | 队内战术标点图标：集火（标记敌方优先击火目标） |
| 3 | `ping_control.png` | `96×96` | `32×32` 等比图标 | 队内战术标点图标：封控（标记敌方优先控制目标） |
| 4 | `ping_protect.png` | `96×96` | `32×32` 等比图标 | 队内战术标点图标：保核（标记己方需要保护/治疗目标） |
| 5 | `ping_dispel.png` | `96×96` | `32×32` 等比图标 | 队内战术标点图标：驱散（标记敌我需要驱散/净化目标） |
| 6 | `streak_banner_base.png` | `320×64` | `320×64` 定宽底板 | 战前变阵与开局连胜势横幅底板（运行时叠字「连胜势 ×N」） |
| 7 | `streak_break_stamp.png` | `324×68` | `324×68` 定宽底板 | 结算页终结对手连胜印章底板（运行时叠字「破势终结」） |
| 8 | `highlight_focus.png` | `128×128` | `64×64` 等比图标 | 战后复盘高光徽记 1：凝锋破核（主目标集火输出突出） |
| 9 | `highlight_control.png` | `128×128` | `64×64` 等比图标 | 战后复盘高光徽记 2：妙手控场（控制与驱散贡献突出） |
| 10 | `highlight_support.png` | `128×128` | `64×64` 等比图标 | 战后复盘高光徽记 3：仁心济世（治疗与护盾支援突出） |
| 11 | `highlight_guard.png` | `128×128` | `64×64` 等比图标 | 战后复盘高光徽记 4：铁壁护主（承伤与前排抗压突出） |
| 12 | `highlight_comeback.png` | `128×128` | `64×64` 等比图标 | 战后复盘高光徽记 5：绝境翻盘（逆风反胜） |

---

## 3. 清单 B · 官方灵石百宝阁 V1（16 枚）

- 对应代码：`Assets/Scripts/UI/Ugui/SpiritShop/SpiritShopWindow.cs`、配表 `SpiritShopGoods.xlsx`
- 输出目录：`D:\work\image\designs\spirit-shop-v1\assets\`
- Unity 导入路径：`Assets/Resources/UI/Ugui/SpiritShopV1/`（其中第 16 枚放 `Assets/Resources/UI/Icons/item_rename_card.png`）

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `ui/window_frame.png` | `1860×940` | `1860×940` 九宫格 | 灵石百宝阁主窗框（顶部留标题牌、双余额与按钮区，中部留内容框） |
| 2 | `ui/title_plate.png` | `480×110` | `480×110` 九宫格 | 窗口左上标题牌底图，中间留空给程序写「灵石百宝阁」 |
| 3 | `ui/content_panel.png` | `1748×590` | `1748×590` 九宫格 | 中部商品列表内容区内框底板 |
| 4 | `ui/tab_active.png` | `260×72` | `260×72` 横向三段 | 顶部分页选中态底图（3 个分页：首套提速 / 精进配套 / 外观便利） |
| 5 | `ui/tab_inactive.png` | `260×72` | `260×72` 横向三段 | 顶部分页未选中态底图 |
| 6 | `ui/card_bg_normal.png` | `1696×166` | `1696×166` 九宫格 | 常规商品横向卡片底板（左留 `112×112` 图标区、中留名称描述区、右留数量与购买区） |
| 7 | `ui/card_bg_featured.png` | `1696×166` | `1696×166` / `1696×112` 九宫格 | 重点/急需高亮商品卡及顶部月卡聚合卡底板 |
| 8 | `ui/quota_bar_bg.png` | `828×24` | `1656×24` 横向三段 | 首套提速终身 300 份共享限额进度条底槽 |
| 9 | `ui/quota_bar_fill.png` | `826×20` | `1652×20` 横向三段 | 共享限额进度条填充条 |
| 10 | `ui/btn_buy_primary.png` | `224×76` | `224×76` 横向三段 | 商品卡片右侧主购买按钮底图 |
| 11 | `ui/btn_recharge_gold.png` | `240×68` | `240×68` 横向三段 | 窗口顶部「充值灵石」强调按钮底图 |
| 12 | `ui/hud_entry_spirit_shop.png` | `96×96` | `96×96` 等比图标 | 主界面「灵石百宝阁」入口按钮图标 |
| 13 | `pack_starter_60.png` | `128×128` | `112×112` 等比图标 | 商品图标：初阶修行提速包（60 份装） |
| 14 | `pack_starter_180.png` | `128×128` | `112×112` 等比图标 | 商品图标：中阶修行提速包（180 份装，规格高于初阶） |
| 15 | `pack_starter_300.png` | `128×128` | `112×112` 等比图标 | 商品图标：半套毕业提速包（300 份装，三档中规格最高） |
| 16 | `icons_global/item_rename_card.png` | `128×128` | `112×112` 等比图标 | 通用道具图标：角色改名卡（导入 `Assets/Resources/UI/Icons/item_rename_card.png`） |

---

## 4. 清单 C · 青云游历手记 TravelPass V1（9 枚）

- 对应代码：`Assets/Scripts/UI/Ugui/TravelPass/TravelPassWindow.cs`、`GameplayWindow.Activities.cs`（并见 [activity-travel-pass-ui-handoff.md](activity-travel-pass-ui-handoff.md)）
- 输出目录：`D:\work\image\designs\travel-pass-v1\assets\`
- Unity 导入路径：`Assets/Resources/UI/Ugui/TravelPassV1/`

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `window_frame.png` | `1860×960` | `1860×960` 九宫格 | 青云游历手记主窗框 |
| 2 | `title_plate.png` | `520×110` | `520×110` 九宫格 | 窗口左上标题牌底图，中间留空给程序写「青云游历手记」 |
| 3 | `content_panel.png` | `1748×584` | `1748×584` 九宫格 | 中部档位奖励滚动列表底板 |
| 4 | `travel_pass_banner.png` | `1748×148` | `1748×148` 九宫格 | 顶部游历说明与进度总览横幅底板 |
| 5 | `progress_track.png` | `564×28` | `1128×28` 横向三段 | 游历点进度条底槽 |
| 6 | `progress_fill.png` | `562×24` | `1124×24` 横向三段 | 游历点进度条填充条 |
| 7 | `tier_row_reached.png` | `1704×92` | `1704×92` 九宫格 | 已达成档位行底板 |
| 8 | `tier_row_locked.png` | `1704×92` | `1704×92` 九宫格 | 未达成档位行底板（视觉权重弱于已达成态） |
| 9 | `travel_pass_card_icon.png` | `128×128` | `128×128` 等比图标 | 活动中心列表内「青云游历手记」入口封面图标 |

---

## 5. 清单 D · 首领周常词缀、赛季永久荣誉与野外灵宠栖息地小补件（10 枚）

- 对应代码：`BossTrialPrepPanel.cs`、`SeasonSealPanel.cs`、`CityTravelUiRoot.cs`（并见 [world-map-habitat-ui-handoff.md](world-map-habitat-ui-handoff.md)）
- 输出目录：`D:\work\image\designs\weekly-affix-honors-and-habitat\assets\`

| # | 相对路径 | 像素尺寸 | Unity 导入目录 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `coreloopv2/icon_weekly_affix_metal.png` | `128×128` | `Assets/Resources/UI/Ugui/CoreLoopV2/` | 首领试炼周常词缀：金 · 锋芒破阵（展示 52×52） |
| 2 | `coreloopv2/icon_weekly_affix_wood.png` | `128×128` | `Assets/Resources/UI/Ugui/CoreLoopV2/` | 首领试炼周常词缀：木 · 灵脉涤尘（展示 52×52） |
| 3 | `coreloopv2/icon_weekly_affix_water.png` | `128×128` | `Assets/Resources/UI/Ugui/CoreLoopV2/` | 首领试炼周常词缀：水 · 蓄势聚灵（展示 52×52） |
| 4 | `coreloopv2/icon_weekly_affix_fire.png` | `128×128` | `Assets/Resources/UI/Ugui/CoreLoopV2/` | 首领试炼周常词缀：火 · 烈焰连携（展示 52×52） |
| 5 | `coreloopv2/icon_weekly_affix_earth.png` | `128×128` | `Assets/Resources/UI/Ugui/CoreLoopV2/` | 首领试炼周常词缀：土 · 守御反震（展示 52×52） |
| 6 | `season_honors/honor_s1_pozhen.png` | `128×128` | `Assets/Resources/UI/SeasonHonors/` | 赛季满级永久纪念徽章（零数值）：S1「破阵宗师」（展示 48/64px） |
| 7 | `season_honors/honor_s2_xuanmai.png` | `128×128` | `Assets/Resources/UI/SeasonHonors/` | 赛季满级永久纪念徽章（零数值）：S2「玄脉行者」（展示 48/64px） |
| 8 | `worldmapv2/wild_habitat_bar.png` | `800×76` | `Assets/Resources/UI/Ugui/WorldMapV2/` | 野外地图底部「寻宠遇敌」栖息地浮条底板（九宫格，左图标/中文案/右按钮） |
| 9 | `worldmapv2/wild_habitat_icon.png` | `96×96` | `Assets/Resources/UI/Ugui/WorldMapV2/` | 野外栖息地浮条左侧灵宠生态图标（展示 48×48） |
| 10 | `worldmapv2/wild_habitat_btn.png` | `190×50` | `Assets/Resources/UI/Ugui/WorldMapV2/` | 野外栖息地浮条右侧「寻宠遇敌」按钮底图（横向三段） |
