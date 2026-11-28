# 不肝不氪增收体系 V2（B3 第 8/9 卷技能卷宗与排面装饰券补图）异机 Codex 纯功能作图交接单

> 说明：实查确认 B1–B5 服务端、客户端逻辑与 UGUI 面板及前期 38 张切图均已全部接入；唯一残余视觉缺口为 B3 新增的第 8/9 卷技能卷宗图标（`scroll_8` / `scroll_9`）、11 个新战法专属图标（`16001`–`16011`）与「排面装饰券」专属道具图标（替换当前暂借的好友礼盒图标），共计 **14 张**透明背景 PNG-32 图标。下方「给异机 Codex 的可直接复制作图正文」严格不含任何画风、色号、引擎或代码词汇；文末「本机研发导入备忘」仅供本机研发入库使用，**绝不复制给 Codex**。

---

## 给异机 Codex 的可直接复制作图正文

```text
请为新专题「fair-monetization-b3-icons」产出以下 14 张透明背景 PNG 图标素材（全部为 PNG-32，背景完全透明 Alpha=0，严禁出现任何文字、字母、数字或水印，不出现抽奖转盘或累充返利元素）。

【一、开工前置要求：先核实并逐张展示 7 张长期标准参照实图】
开工前请先在图仓核实并实际查看以下 7 张长期标准参照图，并在聊天中逐张展示实图、标明名称和准确相对路径（以帮会界面为首要标准）：
1. 邮件界面：designs/mail-ui-v1/01-mail-event.png
2. 组队界面：designs/team-ui-v2/team-ui-v2.png
3. 帮会界面（首要标准）：designs/ranked-play-v2-ui-20261125/assets/approved-guild-reference.png
4. 天墉生死擂 · 下达战书：designs/ui-standard-references/jianghu-duel/01-challenge-approved.png
5. 江湖恩怨录：designs/ui-standard-references/jianghu-duel/02-grudges-approved.png
6. 帮派恩怨墙：designs/ui-standard-references/jianghu-duel/03-guild-wall-approved.png
7. 江湖战书 · 三种弹窗：designs/ui-standard-references/jianghu-duel/04-popups-approved.png

【二、输出目录与交付结构】
- 本机图仓输出目录：D:\work\image\designs\fair-monetization-b3-icons\（换电脑时定位图仓根目录 designs/fair-monetization-b3-icons/）
- 交付包含正式透明切图目录 assets/ 与逐图清单 manifest.json（记录文件名、尺寸、透明校验与不可拉伸说明）。

【三、通用规格】
- 文件格式：PNG-32，背景完全透明（Alpha=0），主体完整居中、四周留 8% 安全边距、边缘干净无白边、不裁切。
- 文字规则：全部图片严格不带任何文字、字母、数字或印章字样。
- 拉伸规则：全部 14 张均为不可拉伸图标。

【四、功能作图清单（共 14 张）】

（A）技能卷宗页签图标（2 张，128×128，界面显示 64×64）
1. assets/scrolls/scroll_8.png | 128×128（界面显示 64×64）| 不可拉伸 | 常规技能卷宗「五行合纵卷」卷轴图标（主题：多方协作与攻防反制）
2. assets/scrolls/scroll_9.png | 128×128（界面显示 64×64）| 不可拉伸 | 稀世绝学卷宗「合纵绝学卷」典藏卷轴图标（主题：开场阵法庇护全队，需与常规卷宗明显区分层级）

（B）新战法专属技能图标（11 张，512×512，界面显示 64×64 ~ 96×96，小尺寸轮廓清晰可辨）
3. assets/skills/16001.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「断盾剑气」图标（功能：单体物理斩击并击碎/驱散目标护盾与增益）
4. assets/skills/16002.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「破咒焚心」图标（功能：法术打击敌方并降低其行动速度）
5. assets/skills/16003.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「灵犀共济」图标（功能：为友方同时回复生命与法力续航）
6. assets/skills/16004.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「清流洗尘」图标（功能：群体净化友方身上的中毒与控制等减益状态）
7. assets/skills/16005.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「守御联璧」图标（功能：为多名友方展开联合防御护盾抵挡爆发）
8. assets/skills/16006.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「引火毒爆」图标（功能：法术引爆目标身上的持续毒伤与灼烧）
9. assets/skills/16007.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「借势反震」图标（功能：给友方施加物理伤害反弹反震状态）
10. assets/skills/16008.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「伏兵协击」图标（功能：普攻追击并削弱目标防御、配合队友集火）
11. assets/skills/16009.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「灵脉回响」图标（功能：被动技能，承受伤害时回流回复法力）
12. assets/skills/16010.png | 512×512（界面显示 64×64）| 不可拉伸 | 技能「护主灵契」图标（功能：灵宠被动技能，受击时触发防御反击）
13. assets/skills/16011.png | 512×512（界面显示 64×64）| 不可拉伸 | 稀世开场绝学「合纵星阵」图标（功能：开场展开覆盖全队的星阵护盾与减伤庇护）

（C）百宝阁道具专属图标（1 张，512×512，界面显示 88×88）
14. assets/items/icon_showcase_voucher.png | 512×512（界面显示 88×88）| 不可拉伸 | 道具「排面装饰券」专属兑换券图标（用于解锁脚底阵纹、入场横幅、聊天气泡与分享卡底纹，需与好友赠礼礼盒图标明显区分）
```

---

## 本机研发导入备忘（不属于给 Codex 的作图正文，交付时不复制）

- **Unity 客户端落位目录**：
  - `assets/scrolls/scroll_8.png`、`scroll_9.png` → `mmorpg-client/Assets/Resources/UI/Ugui/Skills/Scrolls/`（`SeasonEconomyUiArt.LoadSkillScrollIcon(8/9)` 零代码改动自动加载）；
  - `assets/skills/16001.png` ~ `16011.png` → `mmorpg-client/Assets/Resources/UI/Skills/`（`SkillCraftWindow` 与 `BattleScreen` 按 `skill.json` 的 `icon_key = UI/Skills/16001..16011` 零代码改动自动加载）；
  - `assets/items/icon_showcase_voucher.png` → `mmorpg-client/Assets/Resources/UI/Ugui/ShowcaseCosmeticsV1/icon_showcase_voucher.png`（并将 `SpiritShopGoods.xlsx` `#307` 的 `icon_key` 切为 `UI/Ugui/ShowcaseCosmeticsV1/icon_showcase_voucher` 后重导并四仓同步）。
- **Sprite 导入参数**：`TextureImporterType.Sprite`、`SpriteImportMode.Single`、`FullRect`、`PPU=100`、`alphaIsTransparency=true`、`mipmapEnabled=false`、`spriteBorder=(0,0,0,0)`。
