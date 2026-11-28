# 不肝不氪增收体系 V2（B5 · Steam 创始支持者纪念包）异机 Codex 纯功能作图交接单

> 说明：B1–B4 共 36 张切片均已收图落位并接入运行时 UGUI；本补充交接单仅涵盖 B5 阶段「Steam 创始支持者纪念包（DLC）」尚缺的 **2 张**透明背景切图（用于替换 `cosmeticshowcase.json` `#13` 临时占位图及 Steam 典藏包顶部横幅）。下方「给异机 Codex 的可直接复制作图正文」严格不含任何画风、色号、引擎或代码词汇；文末「本机研发导入备忘」仅供本机研发入库使用，**绝不复制给 Codex**。

---

## 给异机 Codex 的可直接复制作图正文

```text
请为专题「fair-monetization-v2」补充产出以下 2 张透明背景 PNG 切图素材（全部为 PNG-32，背景完全透明 Alpha=0，严禁出现任何文字、字母、数字或水印，不出现抽奖转盘或累充返利元素）。

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
- 本机图仓输出目录：D:\work\image\designs\fair-monetization-v2\（换电脑时定位图仓根目录 designs/fair-monetization-v2/）
- 新图放入子目录 assets/b5/，并同步追加更新同目录 manifest.json（记录文件名、尺寸、透明校验与不可拉伸说明）。

【三、通用规格】
- 文件格式：PNG-32，背景完全透明（Alpha=0），主体边缘干净无白边、不裁切。
- 文字规则：全部图片严格不带任何文字、字母、数字，文字区域留空用于界面动态叠加。

【四、批次 B5 功能作图清单（共 2 张）】
1. assets/b5/formation_skin_starlight.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底纪念阵法外观贴图（星河初启主题，中心半透不遮挡角色脚底，与前 5 款五行阵法同规格透视）
2. assets/b5/banner_founder_supporter_pack.png | 1200×320（界面显示 600×160）| 不可拉伸 | 创始支持者纪念典藏包顶部横幅底图（左中大面积留空用于叠放标题与权益说明文案）
```

---

## 本机研发导入备忘（不属于给 Codex 的作图正文，交付时不复制）

- **Unity 客户端落位目录**：
  - `assets/b5/formation_skin_starlight.png` → `mmorpg-client/Assets/Resources/UI/Ugui/ShowcaseCosmeticsV1/formation_skin_starlight.png`（同时将 `cosmeticshowcase.json` / `CosmeticShowcase` 行 `#13` 的 `asset_key` 从临时占位 `UI/Ugui/ShowcaseCosmeticsV1/formation_skin_metal` 切回 `UI/Ugui/ShowcaseCosmeticsV1/formation_skin_starlight`，重跑配表导出即可零代码改动切换）；
  - `assets/b5/banner_founder_supporter_pack.png` → `mmorpg-client/Assets/Resources/UI/Ugui/ShowcaseCosmeticsV1/banner_founder_supporter_pack.png`（供 Steam PC 典藏包入口横幅展示）。
- **Sprite 导入参数**：`TextureImporterType.Sprite`、`SpriteImportMode.Single`、`FullRect`、`PPU=100`、`alphaIsTransparency=true`、`mipmapEnabled=false`、`spriteBorder=(0,0,0,0)`。
