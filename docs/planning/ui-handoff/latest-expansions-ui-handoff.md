# 近期新玩法增量 UI 切片需求清单（五行扩展 / 灵石百宝阁 / 青云游历手记 / 周常词缀与赛季荣誉）

> 用途：汇总除《群仙问道赛》V3+V4（见 [season-team-tournament-v3-ui-handoff.md](season-team-tournament-v3-ui-handoff.md)）外，当前 Unity 客户端代码中已预留加载路径、但尚未产出正式切图的 **4 组共 50 枚增量 PNG 切片**。
> **视觉风格、画法、配色、材质与字体完全以 `D:\work\image` 仓库既有参考图与规范为准，本文档不定义任何美术风格。**

---

## 1. 通用交付技术规范（不含风格要求）

1. **格式**：全部为 PNG-32 透明底（边缘 `alpha = 0`，无棋盘格、无白边、无杂底）。
2. **无字原则**：本文清单中 **全部 50 枚切片均不含任何文字、字母或数字**；所有标题、数值、状态、按钮文案均由 Unity 运行时动态叠加，底板/按钮/标签中部必须保留清晰可读的文本承载区。
3. **拉伸规范**：
   - 标明「九宫格拉伸」的窗框、面板、卡片，需保持四边边框均匀、中心区域平缓，便于配置 Unity `spriteBorder`；
   - 标明「横向三段拉伸」的进度条轨与填充条，需保持左右端帽清晰、中段可水平平铺/拉伸；
   - 标明「等比图标」的切片按 `preserveAspect = true` 展示，主体居中并保留约 8%～12% 安全留边。
4. **交互与安全区**：适配 `1920×1080` 与 `2400×1080` 横屏，右上角预留 `140px` 小游戏胶囊避让区，不依赖鼠标悬停展示信息。

---

## 2. 清单 A · 五行玩法三大扩展（18 枚）

- 对应代码：`Assets/Scripts/UI/Ugui/CoreLoop/WuxingExpansionsUiArt.cs`
- 输出目录：`D:\work\image\designs\wuxing-expansions-v1\assets\`
- Unity 导入路径：`Assets/Resources/UI/Ugui/WuxingExpansionsV1/`

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `icons/element_metal.png` | `128×128` | `32×32` / `88×88` 等比图标 | 五行元素图标：金（用于五行幻境符印条与三选一符印卡） |
| 2 | `icons/element_wood.png` | `128×128` | `32×32` / `88×88` 等比图标 | 五行元素图标：木 |
| 3 | `icons/element_water.png` | `128×128` | `32×32` / `88×88` 等比图标 | 五行元素图标：水 |
| 4 | `icons/element_fire.png` | `128×128` | `32×32` / `88×88` 等比图标 | 五行元素图标：火 |
| 5 | `icons/element_earth.png` | `128×128` | `32×32` / `88×88` 等比图标 | 五行元素图标：土 |
| 6 | `icons/formation_1_tianfu.png` | `128×128` | `60×60` 等比图标 | 战外阵法图标 1：天覆阵 |
| 7 | `icons/formation_2_dizai.png` | `128×128` | `60×60` 等比图标 | 战外阵法图标 2：地载阵 |
| 8 | `icons/formation_3_fengyang.png` | `128×128` | `60×60` 等比图标 | 战外阵法图标 3：风扬阵 |
| 9 | `icons/formation_4_yunchui.png` | `128×128` | `60×60` 等比图标 | 战外阵法图标 4：云垂阵 |
| 10 | `icons/formation_5_huyi.png` | `128×128` | `60×60` 等比图标 | 战外阵法图标 5：虎翼阵 |
| 11 | `icons/duel_rule_polarity.png` | `96×96` | `40×40` 等比图标 | 切磋坊规则开关图标：物法易势 |
| 12 | `icons/duel_rule_blitz.png` | `96×96` | `40×40` 等比图标 | 切磋坊规则开关图标：快节奏回合上限（8/12/20 回合） |
| 13 | `icons/duel_rule_ban_cc.png` | `96×96` | `40×40` 等比图标 | 切磋坊规则开关图标：禁控对决 |
| 14 | `icons/duel_rule_spectate.png` | `96×96` | `40×40` 等比图标 | 切磋坊规则开关图标：允许公开观战 |
| 15 | `ui/sigil_card_common.png` | `520×420` | `520×420` / `276×92` 九宫格 | 幻境爬塔符印卡底板（1 档·凡品），中下部大面积留空给符印名与效果说明 |
| 16 | `ui/sigil_card_spirit.png` | `520×420` | `520×420` / `276×92` 九宫格 | 幻境爬塔符印卡底板（2 档·灵品），视觉层级高于凡品 |
| 17 | `ui/sigil_card_immortal.png` | `520×420` | `520×420` / `276×92` 九宫格 | 幻境爬塔符印卡底板（3 档·仙品），视觉层级最高 |
| 18 | `ui/formation_compass_bg.png` | `512×512` | `480×480` 等比底图 | 赛季/阵法页右侧背景装饰阵盘（运行时叠加半透明，不喧宾夺主） |

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
| 13 | `pack_starter_60.png` | `128×128` | `112×112` 等比图标 | 商品图标：初阶修行提速包（60 份装，含材料与修炼补给） |
| 14 | `pack_starter_180.png` | `128×128` | `112×112` 等比图标 | 商品图标：中阶修行提速包（180 份装，分量高于初阶） |
| 15 | `pack_starter_300.png` | `128×128` | `112×112` 等比图标 | 商品图标：半套毕业提速包（300 份装，三档中规格最高） |
| 16 | `icons_global/item_rename_card.png` | `128×128` | `112×112` 等比图标 | 通用道具图标：角色改名卡（导入 `Assets/Resources/UI/Icons/item_rename_card.png`） |

> 注：百宝阁其余 3 件商品（定星保底符、天工秘墨、角色形象卡）客户端已有成图，无需重画。

---

## 4. 清单 C · 青云游历手记 TravelPass V1（9 枚）

- 对应代码：`Assets/Scripts/UI/Ugui/TravelPass/TravelPassWindow.cs`、`GameplayWindow.Activities.cs`
- 输出目录：`D:\work\image\designs\travel-pass-v1\assets\`
- Unity 导入路径：`Assets/Resources/UI/Ugui/TravelPassV1/`

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `window_frame.png` | `1860×960` | `1860×960` 九宫格 | 青云游历手记主窗框 |
| 2 | `title_plate.png` | `520×110` | `520×110` 九宫格 | 窗口左上标题牌底图，中间留空给程序写「青云游历手记」 |
| 3 | `content_panel.png` | `1748×584` | `1748×584` 九宫格 | 中部档位奖励滚动列表底板 |
| 4 | `travel_pass_banner.png` | `1748×148` | `1748×148` 九宫格 | 顶部游历说明与进度总览横幅底板（左侧留承诺与进度区，右侧留一键领取/解锁按钮区） |
| 5 | `progress_track.png` | `564×28` | `1128×28` 横向三段 | 游历点进度条底槽 |
| 6 | `progress_fill.png` | `562×24` | `1124×24` 横向三段 | 游历点进度条填充条 |
| 7 | `tier_row_reached.png` | `1704×92` | `1704×92` 九宫格 | 已达成档位行底板（左档位号 / 中免费轨 / 右典藏轨） |
| 8 | `tier_row_locked.png` | `1704×92` | `1704×92` 九宫格 | 未达成档位行底板（视觉权重弱于已达成态） |
| 9 | `travel_pass_card_icon.png` | `128×128` | `128×128` 等比图标 | 活动中心列表内「青云游历手记」入口封面图标 |

---

## 5. 清单 D · 首领周常词缀与赛季永久荣誉徽章（7 枚）

- 对应代码：`BossTrialPrepPanel.cs`、`SeasonSealPanel.cs`、`SeasonV1UiArt.cs`
- 输出目录：`D:\work\image\designs\weekly-affix-and-season-honors\assets\`
- Unity 导入路径：前 5 枚放 `Assets/Resources/UI/Ugui/CoreLoopV2/`，后 2 枚放 `Assets/Resources/UI/SeasonHonors/`

| # | 相对路径 | 像素尺寸 | 展示尺寸 / 拉伸 | 用途说明（全部无字） |
|---|---|---:|---|---|
| 1 | `coreloopv2/icon_weekly_affix_metal.png` | `128×128` | `52×52` 等比图标 | 首领试炼周常词缀图标：金 · 锋芒破阵（破绽爆发与金甲护盾） |
| 2 | `coreloopv2/icon_weekly_affix_wood.png` | `128×128` | `52×52` 等比图标 | 首领试炼周常词缀图标：木 · 灵脉涤尘（驱散净化与首领回血） |
| 3 | `coreloopv2/icon_weekly_affix_water.png` | `128×128` | `52×52` 等比图标 | 首领试炼周常词缀图标：水 · 蓄势聚灵（蓄力毒火与蓄势叠加） |
| 4 | `coreloopv2/icon_weekly_affix_fire.png` | `128×128` | `52×52` 等比图标 | 首领试炼周常词缀图标：火 · 烈焰连携（灵宠追击灼伤与提前二阶段） |
| 5 | `coreloopv2/icon_weekly_affix_earth.png` | `128×128` | `52×52` 等比图标 | 首领试炼周常词缀图标：土 · 守御反震（化劲赐盾与强化重劈） |
| 6 | `season_honors/honor_s1_pozhen.png` | `128×128` | `48×48` / `64×64` 等比图标 | 赛季满级永久纪念徽章（零数值）：S1「破阵宗师」 |
| 7 | `season_honors/honor_s2_xuanmai.png` | `128×128` | `48×48` / `64×64` 等比图标 | 赛季满级永久纪念徽章（零数值）：S2「玄脉行者」 |
