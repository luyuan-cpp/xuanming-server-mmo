# 《五行试炼志 V4》卷三季游奇谈（S2–S10）专属名片印戳作图交接

> 用途：为「五行试炼志 · 卷三 季游奇谈」第 2 季至第 10 季（S2–S10）的 4 印里程碑补齐 **9 枚**专属荣誉名片印戳独立图标（第 1 季 `badge_season_s1_trial.png` 与其余 6 枚永久卷印戳已完成入库）。本版本其余界面控件、按钮、列表与底板全部复用既有试炼志界面，无需新增其他界面或整屏图。

---

## 可直接复制给 Codex 的作图正文

```text
任务：为《五行奇谈》「五行试炼志 · 卷三 季游奇谈」补齐第 2 季至第 10 季共 9 枚「赛季名片印戳」独立图标（用于里程碑列表左侧 56×56 展示位与名片荣誉印戳栏；仅负责作图与切图导出，不写任何代码，视觉表现由你按图仓既有同系列印戳自主把握）。

一、开工前必做（核实参考图）
开始绘制前，请先核实图仓中以下已确认的同系列参考图（如缺少某张请先如实告知）：
1. 卷四试炼名片印戳参考：`designs/trial-journal-v3/badges/badge_trial_three_realms.png`
2. 十季赛季主题参考：`designs/season-honor-badges-v1/icons/honor_s2_xuanmai.png` 与 `designs/season-honor-badges-s3-s10-v1/icons/honor_s3_lihuo.png` 至 `honor_s10_hunyuan.png`

二、输出目录（新建独立专题目录）
- 9 枚独立印戳目录：`D:\work\image\designs\trial-journal-v4-badges\badges\`
- 总览验收拼图目录：`D:\work\image\designs\trial-journal-v4-badges\previews\`

三、功能规格与交付硬要求（9 枚均须满足）
1. 文件格式：全部为 PNG-32，完整 RGBA 透明背景（外缘无实色底、无杂边、无棋盘格）。
2. 像素尺寸：每张交付 128×128 正方形，界面显示尺寸为 56×56（不可拉伸，保持 1:1 原比例），主体居中、四周预留 ≥10% 安全留白不裁边。
3. 无文字规则：画面内严禁出现任何汉字、字母、数字或印章文字（印戳名称与季号均由界面文本单独叠加显示）。
4. 可辨识度：缩至 56×56 展示时 9 枚印戳的主体图形轮廓须一眼区分各自赛季主题，并与既有同系列试炼印戳保持统一的视觉重心与占位比例。

四、本次需交付的 9 枚透明 PNG-32 图标清单（文件名必须严格一致）

| 序号 | 输出文件名 | 对应赛季与印戳名（仅供理解题材，勿画进图内） | 主体功能意象（仅作题材区分） | 交付像素 | 显示尺寸 | 拉伸规则 |
|---:|---|---|---|---|---|---|
| 1 | `badge_season_s2_trial.png` | 第二季 · 玄脉之季（玄脉季游） | 灵脉流纹与护盾符环轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 2 | `badge_season_s3_trial.png` | 第三季 · 离火之季（离火季游） | 燃焰刀锋与火羽轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 3 | `badge_season_s4_trial.png` | 第四季 · 青木之季（青木季游） | 青藤新叶与药葫轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 4 | `badge_season_s5_trial.png` | 第五季 · 玄水之季（玄水季游） | 水浪波纹与净瓶轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 5 | `badge_season_s6_trial.png` | 第六季 · 厚土之季（厚土季游） | 重岩山峰与镇盾轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 6 | `badge_season_s7_trial.png` | 第七季 · 庚金之季（庚金季游） | 锐锋双剑与金钟轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 7 | `badge_season_s8_trial.png` | 第八季 · 太阴之季（太阴季游） | 残月圆轮与云影轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 8 | `badge_season_s9_trial.png` | 第九季 · 天雷之季（天雷季游） | 雷枪闪电与法鼓轮廓 | 128×128 | 56×56 | 不可拉伸 |
| 9 | `badge_season_s10_trial.png` | 第十季 · 混元之季（混元季游） | 两仪合环与五点星轨轮廓 | 128×128 | 56×56 | 不可拉伸 |

五、验收预览图与交付自检
1. 全部 9 张透明底 PNG-32 按准确文件名落盘至 `D:\work\image\designs\trial-journal-v4-badges\badges\`。
2. 附 1 张 3×3 总览拼图 `D:\work\image\designs\trial-journal-v4-badges\previews\preview_9_season_trial_badges.png`（含原尺寸与 56×56 缩小态并排对照，仅作人工验收，不替代 9 张独立图标）。
3. 逐张核验：128×128 像素、RGBA 透明底无杂边、无任何文字/数字、主体居中且 56×56 下清晰可辨。
```

---

## 本机研发导入备忘（仅本机研发参考，勿复制给 Codex）

1. 客户端代码已优先加载 `Assets/Resources/UI/Ugui/TrialJournal/Badges/<badge_key>`（`SeasonEconomyUiArt.LoadTrialJournalBadge`），在 Codex 交付上述 9 张 PNG-32 前会自动回退显示已验收落库的 `Assets/Resources/UI/SeasonHonors/honor_s2_xuanmai` 至 `honor_s10_hunyuan`，确保零白框。
2. Codex 交付并经 SHA-256 与视觉验收后，将 9 张 PNG-32 拷入 `mmorpg-client/Assets/Resources/UI/Ugui/TrialJournal/Badges/` 并补齐 `.meta`（Sprite Single、FullRect、MipMaps 关、AlphaIsTransparency 开、0 GUID 冲突），无需修改任何 C# 代码即自动切换为专属印戳。
