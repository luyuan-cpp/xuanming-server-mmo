# 不肝不氪增收体系 V2（B1–B4）异机 Codex 纯功能作图交接单

> 说明：本交接单对应总方案 [fair-monetization-expansion-v2.md](../fair-monetization-expansion-v2.md) 中确需新增的透明切图、图标与静态展示贴图（复用既有弹窗底板、按钮及 `3409 / 3410` 既有图标，不做重复素材）。下方「给异机 Codex 的可直接复制作图正文」严格不含任何画风、色号、引擎或代码词汇；文末「本机研发导入备忘」仅供本机研发入库使用，**绝不复制给 Codex**。

---

## 给异机 Codex 的可直接复制作图正文

```text
请为新专题「fair-monetization-v2」产出以下透明背景 PNG 切图与静态素材（全部为 PNG-32，背景完全透明 Alpha=0，严禁出现任何文字、字母、数字或水印，不出现抽奖转盘或累充返利元素）。

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
- 交付包含正式透明切图目录 assets/ 与逐图清单 manifest.json（记录文件名、尺寸、透明校验与拉伸留白）。

【三、通用规格】
- 文件格式：PNG-32，背景完全透明（Alpha=0），主体边缘干净无白边、不裁切。
- 文字规则：全部图片严格不带任何文字、字母、数字，文字区域留空用于界面动态叠加。

【四、分批功能作图清单】

（A）批次 B1 · 游历手记书架 / 季享卡 / 系列补齐包（8 张）
1. assets/b1/banner_travel_pass_s2.png | 1200×260（界面显示 600×130）| 不可拉伸 | 游历手记第 2 期顶部横幅底图（左中留空用于叠放期次标题与进度说明）
2. assets/b1/banner_travel_pass_s3.png | 1200×260（界面显示 600×130）| 不可拉伸 | 游历手记第 3 期顶部横幅底图（左中留空用于叠放期次标题与进度说明）
3. assets/b1/banner_travel_pass_s4.png | 1200×260（界面显示 600×130）| 不可拉伸 | 游历手记第 4 期顶部横幅底图（左中留空用于叠放期次标题与进度说明）
4. assets/b1/banner_travel_pass_s5.png | 1200×260（界面显示 600×130）| 不可拉伸 | 游历手记第 5 期顶部横幅底图（左中留空用于叠放期次标题与进度说明）
5. assets/b1/banner_travel_pass_s6.png | 1200×260（界面显示 600×130）| 不可拉伸 | 游历手记第 6 期顶部横幅底图（左中留空用于叠放期次标题与进度说明）
6. assets/b1/icon_season_pass_90d.png | 512×512（界面显示 96×96）| 不可拉伸 | 90 天季享卡专属令牌图标（四周留 8% 安全边距）
7. assets/b1/badge_series_bundle_tag.png | 280×80（界面显示 140×40）| 横向可拉伸（左右端头各留白 32px）| 角色收藏系列补齐折扣角标无字底板
8. assets/b1/frame_lightcard_season_s1.png | 512×512（界面显示 112×112）| 不可拉伸 | 季享卡附赠人物头像框（中心圆形完全透明留空用于叠放人物头像，内径约占总宽 72%）

（B）批次 B2 · 战斗排面三件套 / 全城同乐社交耗材 / 好友赠礼（18 张）
9. assets/b2/formation_skin_metal.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底五行阵法外观贴图（样式一：金行主题，中心半透不遮挡角色脚底）
10. assets/b2/formation_skin_wood.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底五行阵法外观贴图（样式二：木行主题，中心半透不遮挡角色脚底）
11. assets/b2/formation_skin_water.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底五行阵法外观贴图（样式三：水行主题，中心半透不遮挡角色脚底）
12. assets/b2/formation_skin_fire.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底五行阵法外观贴图（样式四：火行主题，中心半透不遮挡角色脚底）
13. assets/b2/formation_skin_earth.png | 1024×512（地面透视椭圆显示 512×256）| 不可拉伸 | 战斗脚底五行阵法外观贴图（样式五：土行主题，中心半透不遮挡角色脚底）
14. assets/b2/battle_banner_entrance_01.png | 720×220（界面显示 360×110）| 横向可拉伸（左右端头各留白 90px）| 擂台/会武入场与胜利横幅样式一（中间留空叠放角色名与胜负文案）
15. assets/b2/battle_banner_entrance_02.png | 720×220（界面显示 360×110）| 横向可拉伸（左右端头各留白 90px）| 擂台/会武入场与胜利横幅样式二（中间留空叠放角色名与胜负文案）
16. assets/b2/battle_banner_entrance_03.png | 720×220（界面显示 360×110）| 横向可拉伸（左右端头各留白 90px）| 擂台/会武入场与胜利横幅样式三（中间留空叠放角色名与胜负文案）
17. assets/b2/chat_bubble_skin_01.png | 360×140（界面最小显示 180×70）| 四边可拉伸（左 48px / 右 48px / 上 36px / 下 36px 端头保护）| 聊天发言气泡底板样式一
18. assets/b2/chat_bubble_skin_02.png | 360×140（界面最小显示 180×70）| 四边可拉伸（左 48px / 右 48px / 上 36px / 下 36px 端头保护）| 聊天发言气泡底板样式二
19. assets/b2/share_card_skin_01.png | 640×240（界面显示 320×120）| 四边可拉伸（四边各留白 44px）| 战报与残局分享卡片底纹样式一（中间大面积留白叠放战绩/题面文字）
20. assets/b2/share_card_skin_02.png | 640×240（界面显示 320×120）| 四边可拉伸（四边各留白 44px）| 战报与残局分享卡片底纹样式二（中间大面积留白叠放战绩/题面文字）
21. assets/b2/icon_city_lantern.png | 512×512（界面显示 88×88）| 不可拉伸 | 社交耗材「天墉灵灯」道具图标（四周留 8% 边距）
22. assets/b2/icon_city_firework.png | 512×512（界面显示 88×88）| 不可拉伸 | 社交耗材「庆功烟花」道具图标（四周留 8% 边距）
23. assets/b2/icon_city_tea_table.png | 512×512（界面显示 88×88）| 不可拉伸 | 社交耗材「仙茶同乐席」道具图标（四周留 8% 边距）
24. assets/b2/prop_city_lantern_world.png | 512×512（场景显示 160×160）| 不可拉伸 | 主城地面摆放「天墉灵灯」可点击场景物件（底端脚点对齐底部中线）
25. assets/b2/prop_city_tea_table_world.png | 512×512（场景显示 180×180）| 不可拉伸 | 主城地面摆放「仙茶同乐席」可点击场景物件（底端脚点对齐底部中线）
26. assets/b2/icon_friend_cosmetic_gift.png | 512×512（界面显示 64×64）| 不可拉伸 | 好友外观赠礼按钮与礼盒图标（四周留 8% 边距）

（C）批次 B3 · 帮会徽记定制与扩容便利图标（9 张）
27. assets/b3/guild_emblem_01.png ~ assets/b3/guild_emblem_06.png（共 6 张）| 各 512×512（界面显示 128×128）| 不可拉伸 | 6 款互不重复的帮会徽记圆章外观（完整居中，四周留 6% 边距，无文字）
28. assets/b3/icon_warehouse_expand.png | 512×512（界面显示 72×72）| 不可拉伸 | 仓库扩容功能图标
29. assets/b3/icon_pet_squad_preset.png | 512×512（界面显示 72×72）| 不可拉伸 | 灵宠编队预设功能图标
30. assets/b3/icon_replay_bookmark_expand.png | 512×512（界面显示 72×72）| 不可拉伸 | 精彩战报永久收藏扩容功能图标

（D）批次 B4 · 合规激励视频通用角标（1 张）
31. assets/b4/icon_rewarded_video_badge.png | 256×256（界面显示 36×36）| 不可拉伸 | 高对比度清晰视频播放小角标，用于可选激励视频入口旁标识广告属性（严格无文字）
```

---

## 本机研发导入备忘（不属于给 Codex 的作图正文，交付时不复制）

- **Unity 客户端落位目录**：
  - `assets/b1/*` → `mmorpg-client/Assets/Resources/UI/Ugui/TravelPassV2/` 与 `Assets/Resources/UI/Ugui/AfkPassV2/`；
  - `assets/b2/*` → `mmorpg-client/Assets/Resources/UI/Ugui/ShowcaseCosmeticsV1/` 与 `Assets/Resources/World/CelebrationProps/`；
  - `assets/b3/*` → `mmorpg-client/Assets/Resources/UI/Ugui/GuildEmblemsV1/` 与 `Assets/Resources/UI/Ugui/UtilityExpandV1/`；
  - `assets/b4/icon_rewarded_video_badge.png` → `mmorpg-client/Assets/Resources/UI/Ugui/RewardedAdV1/`。
- **Sprite 导入参数**：`TextureImporterType.Sprite`、`SpriteImportMode.Single`、`FullRect`、`PPU=100`、`alphaIsTransparency=true`、`mipmapEnabled=false`；拉伸图按清单标注端头像素同步写入 `spriteBorder` 与 `.meta`。
- **零新窗兜底**：上述素材未交付前，B1–B4 程序均先复用现有 `GameplayUiArt` / `TravelPassV1` / `SpiritShopV1` 优雅回落跑通全链路功能与单测，收图落位后零代码改动自动换肤。
