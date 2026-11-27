# 「局外人观战交流与个人禁交开关 V1」纯功能 UI 精修作图交接单（可选视觉升级）

> **关联策划**：[battle-spectate-chat-v1.md](../battle-spectate-chat-v1.md)  
> **是否阻塞上线**：**不阻塞**。当前客户端已复用既有战斗底板与按钮九宫格完成全功能与全 UI 接入（可立即完整游玩与测试）；本交接单仅为**可选视觉精修**（4 张轻量切片 + 1 张排版预览图），用于让战斗内左侧「观战交流 / 静默观战」入口牌及展开抽屉在视觉上与通用战斗按钮一目了然地区分开来。  
> **使用说明**：下方「一、可直接整段复制给异机 Codex 的作图正文」严格遵守 `AGENTS.md §10.4`——仅写功能用途、布局内容、尺寸、文件名、透明底、无字规则、拉伸留白与图仓输出目录，**不含任何画风词、材质词、色号/配色指令及任何引擎/程序术语**；文末「二、本机研发导入备忘」仅供本机程序使用，切勿复制给 Codex。

---

## 一、可直接整段复制给异机 Codex 的作图正文

```text
请只负责制作并导出「局外人观战交流模式」战斗内悬浮入口与交流抽屉所需的 4 张透明底运行切片与 1 张排版预览图（不需要改代码、不需要改协议、不需要做编译和测试）。

一、开工前强制核实并逐张展示 7 张长期标准参照图
开始绘制前，请先在图仓核实并实际查看以下全部 7 张标准成品图，并在回复中逐张展示图片、标明名称和准确的图仓相对路径（帮会界面仍为首要标准；若图仓缺图请先如实报告，勿用文字或其他图片代替）：
1. 邮件界面：designs/mail-ui-v1/01-mail-event.png
2. 组队界面：designs/team-ui-v2/team-ui-v2.png
3. 帮会界面（首要标准）：designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png
4. 天墉生死擂 · 下达战书：designs/ui-standard-references/jianghu-duel/01-challenge-approved.png
5. 江湖恩怨录：designs/ui-standard-references/jianghu-duel/02-grudges-approved.png
6. 帮派恩怨墙：designs/ui-standard-references/jianghu-duel/03-guild-wall-approved.png
7. 江湖战书 · 三种弹窗：designs/ui-standard-references/jianghu-duel/04-popups-approved.png

二、输出目录（新建独立专题目录，勿改动其他目录）
- 图仓输出目录：D:\work\image\designs\battle-spectate-chat-v1\
  - 运行切片目录：D:\work\image\designs\battle-spectate-chat-v1\slices\（4 张）
  - 排版预览目录：D:\work\image\designs\battle-spectate-chat-v1\previews\（1 张，仅作人工验收参考）
  - 同目录生成 manifest.json，列出 4 张正式切片的相对路径、像素尺寸、拉伸留白边距与 SHA-256。

三、通用硬性规格
1. 格式：全部切片与预览图均为 PNG-32 真透明背景（RGBA，四角与主体外完全透明 Alpha = 0，无纯色底块、无白边杂边）。
2. 无文字规则：slices/ 下全部 4 张运行切片严禁包含任何汉字、英文字母、数字或水印（所有按钮文案、未读数字角标、发言正文与身份标签均由程序运行时动态叠放）；仅 previews/ 排版预览图可包含下方业务样例文案并标明“UI 演示数据”。
3. 拉伸与缩放规则：
   - 标明“九宫格拉伸”的底板：四周边框均匀、中心文字承载区平整干净，并标明四边不可拉伸边距像素值（左、上、右、下）；
   - 标明“横向三段拉伸”的胶囊/状态牌底板：左右端头对称、中段平滑可拉伸、中间留足短字区，并标明左右端头像素宽度。

四、交付清单 1：4 张正式透明 PNG-32 切片（输出至 slices/）

1. slices/spectate_chat_drawer_bg.png | 680×470 | 九宫格拉伸（建议留白：左 24、上 54、右 24、下 24）| 不含文字
   - 用途：实时战斗与观战界面左侧展开的「观战交流（全房公开）」悬浮抽屉主底板（顶部预留约 46px 标题与关闭按钮承载带，中部预留滚动消息区，底部预留状态提示条与输入框/发送按钮区，抽屉不遮挡核心战场）。

2. slices/spectate_chat_entry_open_bg.png | 220×56 | 横向三段拉伸（建议左右端头各 22px）| 不含文字
   - 用途：战斗/观战界面左侧「本局已开启观战交流」折叠入口按钮底板（运行时中央叠放「观战交流」或「观战交流 (3)」未读提示文案；需与普通战斗操作按钮有清晰可辨的交流入口辨识度）。

3. slices/spectate_chat_entry_silent_bg.png | 220×56 | 横向三段拉伸（建议左右端头各 22px）| 不含文字
   - 用途：观战界面左侧「本局关闭观战交流（仍可正常观战）」只读状态牌底板（运行时中央叠放「静默观战·禁交流」文案；需传达温和禁言/只读状态，与可点击交流入口形成明确状态区分）。

4. slices/spectate_chat_send_btn_bg.png | 124×56 | 横向三段拉伸（建议左右端头各 18px）| 不含文字
   - 用途：观战交流抽屉右下角「发送」主操作按钮底板（运行时中央叠放「发送」二字，支持正常态与冷却置灰态区分）。

五、交付清单 2：1 张验收排版预览图（输出至 previews/，1920×1080，窗外透明，标注“UI 演示数据”）

1. previews/01-spectate-chat-drawer.png | 1920×1080
   - 展示战斗界面左侧悬浮态的完整对照效果：
     - 左侧主抽屉（680×470）：顶部标题「观战交流（全房公开 · 不含队内私密）」与右上关闭符号「×」；中部消息区展示 4 条样例（「[观战] 看客甲：这回合别急，留一手解控」「[参战] 青冥剑客：收到，先保宝宝」「[观战] 云游道人：对面速度更快，小心被先手封」「[参战] 玄水真人：哈哈你们别剧透」）；下方状态行「全房公开交流 · 双方与观众同步可见」；底部输入框占位文案「发句话和大家聊聊（≤60字）…」与右侧「发送」按钮。
     - 右侧空白演示区同步并排展示两枚折叠入口状态样例：「观战交流 (3)」（使用 spectate_chat_entry_open_bg）与「静默观战·禁交流」（使用 spectate_chat_entry_silent_bg），便于一次验收两态区分度。
```

---

## 二、本机研发导入备忘（仅本机研发参考，严禁复制给 Codex）

1. Codex 交付并经目视验收后，核对 `D:\work\image\designs\battle-spectate-chat-v1\manifest.json` 的 SHA-256、尺寸、RGBA 透明通道与拉伸边距，将 `slices/` 下 4 张 PNG 按原基名导入客户端目录 `mmorpg-client/Assets/Resources/UI/Ugui/BattleSpectateChatV1/`，并补齐唯一 GUID 的 Unity Sprite `.meta`（按 `manifest.json` 设置 `spriteBorder`）。
2. 在 `mmorpg-client/Assets/Scripts/UI/Ugui/Battle/BattleSpectateChatPanel.cs` 中将 `panel_9slice` / `button_9slice` 优先指向 `UI/Ugui/BattleSpectateChatV1/` 对应切片（资源缺失时继续回落至现役战斗 9-slice 底板，零风险）。
3. 协议生成、导表、C++/Go/Java/Unity 编译与自动化测试均由本机研发 / Gemini 完成，不交由 Codex。
