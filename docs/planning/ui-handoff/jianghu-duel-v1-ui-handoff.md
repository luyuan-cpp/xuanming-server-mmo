# 江湖恩怨与天墉生死状一期（JianghuDuelV1）UI 纯功能作图交接单

> 日期：2026-11-26  
> 关联策划：[battle-jianghu-grudge-duel-bounty-v1.md](../battle-jianghu-grudge-duel-bounty-v1.md)  
> 边界约束：仅描述需要制作的 UI 控件、用途、像素尺寸、文件名、透明底与拉伸规则，**严禁指定任何画风、风格词、材质词或色号/配色**；输出目录为图仓 `D:\work\image\designs\jianghu-duel-v1-20261126\`。

## 一、直接复制给异机 Codex 的纯功能作图正文

```text
请制作「天墉生死擂与江湖恩怨录」20 张运行时无字透明 UI 与道具素材，输出目录统一新建在：
D:\work\image\designs\jianghu-duel-v1-20261126\

通用功能规范：
1. 全部 20 张图均为 PNG-32 格式，四周真实透明背景（Alpha=0），无白边、无杂底、无多余外框。
2. 全部 20 张图严禁包含任何文字、字母、数字或角标（所有标题、页签字样、战绩数值、按钮文案均由运行时动态叠放）。
3. 标明“九宫格拉伸”的窗口/面板/卡片/横幅：四边边框均匀、中间文本承载区平滑清晰，并在 manifest.json 中标注四边切片边距（左、上、右、下像素值）。
4. 标明“横向三段拉伸”的按钮/页签/状态标签：左右端帽对称清晰、中段可水平拉伸、中间留足文字安全区，并在 manifest.json 中标注左右端帽像素宽度。
5. 标明“等比图标”的图标：主体居中、四周留边约 8%–10%，缩放到展示尺寸时轮廓清晰易辨识。
6. 产出完成后生成 manifest.json，列出全部 20 个文件的相对路径、像素尺寸、拉伸边距与 SHA-256。

【素材清单（共 20 张，输出到 slices/）】
1. slices/hud_entry_jianghu.png | 96x96 | 等比图标（展示 96x96）| 主界面「天墉生死擂 / 江湖恩怨」入口图标
2. slices/window_frame.png | 1860x960 | 九宫格拉伸 | 主窗口外框（顶部留标题与功能按钮区、上部留状态横幅与 3 页签区、中部留主内容区）
3. slices/title_plate.png | 520x110 | 九宫格拉伸 | 窗口左上标题牌底板（中部留空叠放窗口标题）
4. slices/status_banner.png | 1748x104 | 九宫格拉伸 | 顶部个人江湖状态与战败印记横幅底板（右侧留洗尘按钮位）
5. slices/content_panel.png | 1748x566 | 九宫格拉伸 | 中部主内容列表/双栏容器底板
6. slices/popup_frame.png | 860x480 | 九宫格拉伸 | 二级弹窗底板（复用为「清风洗尘确认窗」与「收到战书即时弹窗」）
7. slices/tab_selected.png | 240x64 | 横向三段拉伸 | 顶部 3 页签选中态底板（下达战书 / 江湖恩怨录 / 帮派恩怨墙）
8. slices/tab_normal.png | 240x64 | 横向三段拉伸 | 顶部 3 页签未选中态底板
9. slices/btn_primary.png | 320x70 | 横向三段拉伸 | 主行动按钮底板（下达战书 / 应战开打 / 清风洗尘）
10. slices/btn_secondary.png | 244x64 | 横向三段拉伸 | 次级按钮底板（闭关免战 / 刷新 / 婉拒 / 回放 / 一笔勾销）
11. slices/writ_card_silver.png | 374x152 | 九宫格拉伸 | 战书档位选择卡片底板：普通战书（常规档）
12. slices/writ_card_gold.png | 374x152 | 九宫格拉伸 | 战书档位选择卡片底板：金帖生死状（高规格档，视觉层级高于普通战书）
13. slices/invite_row_bg.png | 850x168 | 九宫格拉伸 | 在途生死状列表单条记录卡片底板
14. slices/grudge_row_bg.png | 1704x122 | 九宫格拉伸 | 个人江湖恩怨录单条宿敌行底板
15. slices/badge_streak_hot.png | 176x36 | 横向三段拉伸 | 宿敌高光连胜状态标签底板
16. slices/badge_revenge_ready.png | 176x36 | 横向三段拉伸 | 宿敌待复仇/雪耻状态标签底板
17. slices/guild_rival_card.png | 536x102 | 九宫格拉伸 | 帮派恩怨墙左侧宿敌帮派卡片底板
18. slices/guild_record_row.png | 1116x92 | 九宫格拉伸 | 帮派恩怨墙右侧纯帮派生死状战绩单行底板
19. slices/item_gold_duel_writ.png | 512x512 | 等比图标（展示 112x112 / 64x64）| 道具 3415 / 百宝阁商品 305「金帖生死状」图标：用于背包、百宝阁与约战界面，表达高规格公开约战战书凭证语义（严禁带文字）
20. slices/item_clear_dust_charm.png | 512x512 | 等比图标（展示 112x112 / 64x64）| 道具 3416 / 百宝阁商品 306「清风洗尘符」图标：用于背包、百宝阁与洗尘弹窗，表达消除战败印记、清风拂尘的符令语义，外形轮廓需与 item_gold_duel_writ.png 一眼区分（严禁带文字）
```

## 二、本机研发导入备忘（严禁复制给 Codex）

- Codex 交付后核对 SHA-256 与透明通道，按 `slices/<文件名>` 同名导入 `mmorpg-client/Assets/Resources/UI/Ugui/JianghuDuelV1/` 并补齐 `.meta` 与拉伸边距；`JianghuDuelWindow.cs`、`JianghuUiRoot.cs` 与 `WorldHudNavigation.cs` 均已预埋同名加载路径，无需改动 C# 代码。
