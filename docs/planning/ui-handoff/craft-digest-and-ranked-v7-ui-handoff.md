# 西市名匠离线营业战报 + 论剑 V7 最后 12 张无字透明 UI 切图交接

> 日期：2026-11-26  
> 状态：客户端 C# 与双服逻辑均已接入并带默认回退；本批为全仓最后 2 个缺图目录（`UI/Ugui/CraftProxyDigest/` 8 张 + `UI/Ugui/RankedPlayV7/` 4 张）。  
> 提示：下文代码块可直接复制发送给异机 Codex 作图窗口，不包含任何风格/色号限定或程序引擎路径。

---

## 一、可直接复制给 Codex 的作图正文

```text
请制作以下 12 张游戏运行时无字透明 UI 素材（PNG-32 格式，四周真实透明背景 Alpha=0，无白边、无杂底、无多余外框），输出目录统一新建在：
D:\work\image\designs\craft-digest-and-ranked-v7-12-20261126\

通用功能要求：
1. 全部 12 张图严禁包含任何文字、字母、数字或角标（所有标题、名次数字、数值、按钮文案均由程序运行时动态叠放）。
2. 标明“九宫格拉伸”的窗口/面板/卡片/横幅：四边边框均匀、中间文本承载区平滑清晰，并在 manifest.json 中标注四边切片边距（左、上、右、下像素值）。
3. 标明“横向三段拉伸”的按钮/横幅：左右端帽对称清晰、中段可水平拉伸、中间留足文字安全区，并在 manifest.json 中标注左右端帽像素宽度。
4. 标明“等比图标”的图标：主体居中、四周留边约 8%–10%，缩放到展示尺寸时轮廓清晰易辨识。
5. 产出完成后请生成 manifest.json，列出全部 12 个文件的相对路径、像素尺寸、拉伸边距与 SHA-256。

【分组 1：西市名匠离线营业战报与 24h 极品热榜（8 张，输出到 craft-proxy-digest/）】
1. craft-proxy-digest/panel_proxy_digest.png | 1300x540 | 九宫格拉伸 | 名匠上线弹出「离线营业战报」主弹窗底板（顶部预留图标与标题位、中部预留战报汇总与新解锁雅号行、底部预留双按钮位）
2. craft-proxy-digest/banner_hot_masters.png | 1400x96 | 九宫格拉伸 | 西市名匠页「24小时极品热榜 Top 3」整段横向容器底板（左侧留标题区、右侧并排容纳 3 张名匠热榜卡片）
3. craft-proxy-digest/card_hot_master.png | 520x136 | 九宫格拉伸 | 单张极品热榜名匠卡片底板（左侧预留名次徽标位、中部留名匠称谓与战绩文字区、右侧留快捷代打按钮位）
4. craft-proxy-digest/badge_hot_rank_1.png | 128x128 | 等比图标（展示 86x86）| 西市热榜第 1 名（榜首）纪念徽标（严禁出现数字“1”或文字）
5. craft-proxy-digest/badge_hot_rank_2.png | 128x128 | 等比图标（展示 86x86）| 西市热榜第 2 名（榜眼）纪念徽标（严禁出现数字“2”或文字）
6. craft-proxy-digest/badge_hot_rank_3.png | 128x128 | 等比图标（展示 86x86）| 西市热榜第 3 名（探花）纪念徽标（严禁出现数字“3”或文字）
7. craft-proxy-digest/badge_title_unlocked.png | 128x128 | 等比图标（展示 92x92）| 营业战报内「新解锁名匠雅号」喜报徽标（严禁包含文字）
8. craft-proxy-digest/icon_digest_scroll.png | 256x256 | 等比图标（展示 112x112）| 名匠离线营业账册/战报顶部象征图标（严禁包含文字）

【分组 2：论剑 V7 战后不服再战与擂台直播横幅（4 张，输出到 ranked-play-v7/）】
9. ranked-play-v7/btn_rematch_duel.png | 240x68 | 横向三段拉伸 | 1V1 战后结算页「不服再战」复仇切磋按钮底板（左侧留 28x28 小图标位、中右居中叠字）
10. ranked-play-v7/icon_rematch_swords.png | 96x96 | 等比图标（展示 28x28）| 「不服再战」按钮左侧切磋复仇小徽标（缩放至 28x28 时轮廓清晰）
11. ranked-play-v7/live_ranked_spectate_banner.png | 760x76 | 横向三段拉伸 | 论剑名局页顶部「高段排位擂台直播」实时围观横幅底板（左侧留 36x36 图标与双行文字区、右侧留 3 个操作按钮位）
12. ranked-play-v7/icon_live_spectate.png | 96x96 | 等比图标（展示 36x36）| 擂台直播横幅左侧「实时围观」小徽标（缩放至 36x36 时轮廓清晰）
```

---

## 二、本机研发导入映射（不发给 Codex）

- `craft-proxy-digest/*.png`（8 张）→ `mmorpg-client/Assets/Resources/UI/Ugui/CraftProxyDigest/`（对应 `SkillCraftWindow.cs:702-811`，其中 `badge_title_unlocked` 代码实际按 `92×92` 正方等比图标绘制）。
- `ranked-play-v7/*.png`（4 张）→ `mmorpg-client/Assets/Resources/UI/Ugui/RankedPlayV7/`（对应 `RankedSettlementPanel.cs:165-166`、`RankedSharedDrawer.cs:687-688`；导入后顺带检查 `rImg.type` 与 `liveBg.type` 切为按 `border` 判断 `Image.Type.Sliced`）。
