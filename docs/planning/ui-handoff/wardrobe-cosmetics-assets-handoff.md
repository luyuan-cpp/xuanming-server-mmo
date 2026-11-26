# 衣柜外观券、专属技能特效帧条与百宝阁切片纯功能作图交接清单

> 落盘日期：2026-11-26  
> 关联设计：[wardrobe-pet-morph-and-skill-vfx-v1.md](../wardrobe-pet-morph-and-skill-vfx-v1.md) · [spirit-shop-and-starter-packs-v1.md](../spirit-shop-and-starter-packs-v1.md)  
> 现状说明：衣柜主界面「人物 / 灵宠 / 特效」三页签的 12 张无字 UI 切片（`wardrobe-cosmetics-ui-20261125`）已全部接入 Unity 客户端 `Assets/Resources/UI/Ugui/WardrobeCosmetics/`，**无需重做衣柜框架与页签**。本清单仅列出尚未交付的新增商业化道具图标与可选生产级特效/百宝阁切片。

---

## 一、可直接整段复制发给异机 Codex 的纯功能作图清单

```text
请制作以下透明背景 PNG-32 美术资产（全部背景完全透明 Alpha=0，严禁包含任何文字、字母、数字或水印，所有文字由程序运行时叠放）：

【第一批 · 必做（2 枚商品道具图标，接入百宝阁与背包/衣柜）】
落盘目录：D:\work\image\designs\wardrobe-cosmetics-assets-20261126\icons\
（程序接入路径：Assets/Resources/UI/Ugui/WardrobeCosmetics/）

1. item_pet_morph_charm.png
   - 用途：道具「灵宠幻化符」图标，用于灵石百宝阁商品卡（112×112 显示）、背包格子与衣柜解锁提示；语义为用于永久解锁灵宠外观幻化的道具符令。
   - 尺寸：512×512 PNG-32，背景完全透明，非拉伸等比缩放，四周保留 8% 安全留白，严禁带文字。

2. item_skill_vfx_scroll.png
   - 用途：道具「技能特效卷」图标，用于灵石百宝阁商品卡（112×112 显示）、背包格子与衣柜解锁提示；语义为用于永久解锁技能战斗特效的道具卷轴。
   - 尺寸：512×512 PNG-32，背景完全透明，非拉伸等比缩放，四周保留 8% 安全留白，严禁带文字。

【第二批 · 推荐补齐（首批 3 套专属技能特效序列帧条，替换当前复用底图变色占位）】
落盘目录：D:\work\image\designs\wardrobe-cosmetics-assets-20261126\skill_vfx\
（程序接入路径：Assets/Resources/Battle/Fx/；每张均为 8 帧横向等宽序列帧条，单帧 384×384，整图 3072×384，PNG-32 透明底，无文字，每帧主体居中不裁边、不跨帧溢出）

3. vfx_xuanfu_alt_impact_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「玄符落曜」专属替换命中特效，用于战斗命中播放与衣柜「特效」页签 384×384 预览框循环播放。
4. vfx_xuanfu_alt_projectile_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「玄符落曜」专属替换飞行投射特效，用于战斗施法弹道飞行段（水平朝右飞行，程序自动镜像左向）。
5. vfx_zixiao_alt_strike_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「紫霄雷阵」专属替换落雷命中特效，用于战斗命中播放与衣柜预览框循环播放。
6. vfx_zixiao_alt_telegraph_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「紫霄雷阵」专属替换地面阵法预示特效，用于战斗目标脚下蓄力提示段。
7. vfx_hanxing_alt_impact_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「寒星凝魄」专属替换命中特效（赛季荣誉专属外观），用于战斗命中播放与衣柜预览框循环播放。
8. vfx_hanxing_alt_projectile_strip.png（3072×384，8 帧横向序列）
   - 用途：技能「寒星凝魄」专属替换飞行投射特效，用于战斗施法弹道飞行段（水平朝右飞行）。

【第三批 · 同步可选（灵石百宝阁专属 UI 切图，当前目录缺失、客户端正回退通用弹窗控件）】
落盘目录：D:\work\image\designs\spirit-shop-ui-20261126\
（程序接入路径：Assets/Resources/UI/Ugui/SpiritShopV1/，全部 PNG-32 透明底无文字，严禁出现抽奖转盘或 VIP 充值返利元素）

9.  ui/window_frame.png | 1860×940 参考（9-slice 可拉伸，四边端头留白 64px）| 百宝阁主弹窗外框底板，中心留空给程序布局
10. ui/title_plate.png | 480×110（横向 9-slice 可拉伸，左右端头留白 64px）| 弹窗顶部「灵石百宝阁」标题牌匾无字底板
11. ui/content_panel.png | 1748×590 参考（9-slice 可拉伸，四边端头留白 36px）| 主内容区底板
12. ui/tab_active.png & ui/tab_inactive.png | 260×68（横向 9-slice 可拉伸，左右留白 36px）| 顶部分页按钮选中态与未选中态无字底板
13. ui/card_bg_normal.png & ui/card_bg_featured.png | 460×210 参考（9-slice 可拉伸，四边留白 32px）| 普通商品卡与重点推荐礼包/月卡底板
14. ui/quota_bar_bg.png & ui/quota_bar_fill.png | 600×28（横向拉伸，左右端头留白 16px）| 首套 300 份终身共享限额进度条底槽与填充条
15. ui/btn_buy_primary.png & ui/btn_recharge_gold.png | 240×68（横向 9-slice 可拉伸，左右留白 32px）| 购买主按钮与灵石不足充值按钮无字底板
16. pack_starter_60.png、pack_starter_180.png、pack_starter_300.png | 各 512×512（非拉伸，放在根目录）| 初阶 / 中阶 / 半套三档修行提速礼包图标，三档体量逐级递进
```

---

## 二、后续新上「专属灵宠幻化皮肤」规格模板（按需单只追加）

当前首批 3 套灵宠幻化复用既有灵宠动作集已可完整运行；后续每次新增独立售卖的专属灵宠幻化时，按每只单独建目录 `Assets/Resources/World/Pets/QdaoPets20261005/<art_key>/` 产出以下透明底 PNG-32（全部无文字，脚底基线一致）：

- `portrait.png`：`512×512`（立绘与头像展示）
- `idle/E/01.png`、`idle/W/01.png`：`1024×1024`（左右朝向待机）
- `attack/E/01.png`～`12.png`、`attack/W/01.png`～`12.png`：`1024×1024`（12 帧普攻）
- `cast/E/01.png`～`16.png`、`cast/W/01.png`～`16.png`：`1024×1024`（16 帧施法）
- `hit/E/01.png`～`06.png`、`hit/W/01.png`～`06.png`：`1024×1024`（6 帧受击）
