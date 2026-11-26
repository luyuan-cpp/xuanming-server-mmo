# 《五行奇谈》装备品相与典籍制书 V2–V5 增量图标纯功能交接单

> 日期：2026-11-26  
> 关联策划：[equipment-quality-and-skill-crafting-v2-v5.md](../equipment-quality-and-skill-crafting-v2-v5.md)  
> 边界说明：下方「可复制作图正文」严格遵守 `AGENTS.md §10.4`——只列本次新增的 5 张小图标/角标的功能语义、尺寸、文件名、透明底、无字留白与标准参照路径，**不含任何画风词、材质词、色号/配色指令，也不含任何客户端引擎或程序内容**。

---

## 可直接复制给异机 Codex 的作图正文

```text
请在本机 D:\work\image 仓库中，先核实并实际查看以下三张仓库标准原图，再在当前聊天中逐张展示图片、标明名称和准确的图仓相对路径（帮会界面为首要标准）；同时核实并补充展示本专题已确认的同功能成图：
- 邮件界面：designs/mail-ui-v1/01-mail-event.png
- 组队界面：designs/team-ui-v2/team-ui-v2.png
- 帮会界面（首要标准）：designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png
- 同功能既有已确认成图（可核实后补充展示）：designs/craft-quality-skill-ui-20261124/screens/08-city-master-proxy-stall.png 与 designs/sealed-skill-book-icon-20261126/icons/item_sealed_skill_book.png

全部视觉风格、配色、材质、描边与画面细节完全遵守 D:\work\image 仓库现有规范与现存参考图（不要自定新风格）。做完后按仓库规范补齐 64×64 缩略辨识对照图 preview-64.png、逐图来源记录与 manifest.json，并直接提交 Push 到 Azure image 仓库 main。

输出目录：
- D:\work\image\designs\craft-quality-skill-v2-v5-icons-20261126\

交付清单（共 5 张独立透明底 PNG-32 + 1 张 64×64 缩略辨识对照图 preview-64.png）：

一、4 张功能入口/标识独立图标（输出至 icons/ 目录，均为 512×512，窗外/背景真实透明 Alpha，主体居中留边约 8%–10%，图标内严禁出现任何文字、数字或角标）：
1. icon_master_honor.png — 名匠口碑勋章
   用途：展示宗师在主城西市的累计代工口碑、极品出品战绩与零属性宗师雅号印款。
2. icon_batch_polish.png — 智能定档连磨
   用途：工坊内一键连续打磨装备词条直至目标品相或保底即停的快捷操作图标。
3. icon_scroll_archive.png — 典籍藏经阁
   用途：查看各卷宗永久技能收集进度、领取卷宗小成/圆满残页里程碑以及流派缺书体检的图标。
4. icon_wanted_board.png — 西市悬赏求书榜
   用途：主城西市悬赏求购可交易封卷技能书与揭榜交卷功能页签图标。

二、1 张无字透明角标底板（输出至 ui/ 目录，严格零文字、真实透明 Alpha，横向可拉伸，左右各预留约 20px 不可拉伸端头留白）：
5. badge_team_free_proxy.png — 同队情义免税代打角标底板（尺寸 220×56）
   用途：名匠摊位列表与委托确认栏中承载“同队免费代打”提示文字的轻量横条角标底板。

硬性要求：
- 本次只需交付上述 5 张正式图、preview-64.png、manifest.json 及配套来源记录，不需要重画整屏界面，也不要读取任何程序代码。
- 全部 5 张图必须为 PNG-32（RGBA），四周具备真实透明 Alpha 通道，不得带有纯色背景板或直角底框；图标内不写任何文字。
```

---

## 本机研发导入备忘（仅本机研发留档，回复时严禁复制给 Codex）

- Codex 在 `D:\work\image\designs\craft-quality-skill-v2-v5-icons-20261126\` 交付并 Push 后，由本机研发导入客户端：
  - `icons/icon_master_honor.png` → `mmorpg-client/Assets/Resources/UI/Ugui/CraftQualitySkill/Icons/icon_master_honor.png`
  - `icons/icon_batch_polish.png` → `mmorpg-client/Assets/Resources/UI/Ugui/CraftQualitySkill/Icons/icon_batch_polish.png`
  - `icons/icon_scroll_archive.png` → `mmorpg-client/Assets/Resources/UI/Ugui/CraftQualitySkill/Icons/icon_scroll_archive.png`
  - `icons/icon_wanted_board.png` → `mmorpg-client/Assets/Resources/UI/Ugui/CraftQualitySkill/Icons/icon_wanted_board.png`
  - `ui/badge_team_free_proxy.png` → `mmorpg-client/Assets/Resources/UI/Ugui/CraftQualitySkill/badge_team_free_proxy.png`
- **已验收导入（2026-11-26，来源 `image@346227407d9abe5aa5a5143b83132b2eb6926d80`）**：全部 5 张 PNG-32 均已从 `D:\work\image\designs\craft-quality-skill-v2-v5-icons-20261126\` 原样导入客户端并逐字节校验 SHA-256 一致、补齐唯一 GUID 的 Unity `.meta`（其中 `badge_team_free_proxy.png.meta` 配 `spriteBorder: {x: 20, y: 0, z: 20, w: 0}` 横向三段拉伸），客户端已补齐 `badge_team_free_proxy` 的运行时 `"同队免税"` 文字叠加与图标-文案防重叠排布，并通过 `client_compile_check.ps1`（814 文件 0 错误）与 `SeasonEconomyUiArtTests.CraftQualitySkillV2V5IncrementalAssets_All5SpritesAndMetasMatchContract`（7 套 NUnit 157/157 全绿）。
