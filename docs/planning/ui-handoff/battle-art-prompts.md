# 战斗美术生图提示词包与资产契约 v1

> 配合 `turn-battle-presentation.md` §5.2。生成模式沿用 walk v1 的做法(内置 imagegen:stylized-concept 锁定定稿页,
> 逐动作生成分镜,precise-object-edit 修正;确定性工具抠图 / 去洋红边 / 主连通域 / 脚底基线对齐 / 打包)。
> 本 CLI 会话没有生图通道:提示词与契约在此,生成产物按 §3 路径放入客户端即自动生效(缺图用程序化占位)。

## 1. 统一规格

- 单格 512×512(高清母版),运行时 256×256;横条 = 帧数 × 单格,左→右为时间序。
- 只做 **E(右侧面)与 W(左侧面)**两向:战斗视角为侧视,我方在下朝上/右,敌方在上朝下/左;
  客户端按阵营选向,W 缺失时用 E 镜像(角色挂件不对称可接受)。
- 背景 chroma-magenta(#FF00FF),脚底基线统一,pivot (0.5, 0.08),无投影、无文字、无网格。
- 帧率:idle 6 FPS、attack 12 FPS、cast 10 FPS、hit 12 FPS、die 8 FPS、win 8 FPS。

| 动作 | 帧数 | 相位(按序) |
|---|---|---|
| idle | 4 | 站立呼吸:中立 → 微沉 → 中立 → 微抬 |
| attack | 6 | 预备后引 → 前冲蓄力 → 挥击(命中帧=第 4 帧)→ 挥击过肩 → 收势 → 回中立 |
| cast | 6 | 抬手 → 聚气(手心亮)→ 施放顶点(命中帧=第 4 帧)→ 释放余势 → 收手 → 中立 |
| hit | 3 | 受击后仰 → 最大后仰(闪白帧)→ 回弹 |
| die | 6 | 受击 → 踉跄 → 单膝 → 倒地 → 倒地(淡)→ 消散 |
| win | 4 | 抬臂 → 举高 → 定格 → 回中立 |

## 2. 提示词

### 2.1 角色(以 QdaoHeadbandBoy 为例;新角色先出八方向定稿页再出动作)

定稿页(已有:`E:/work/output/imagegen/qdao_headband_boy_8dir_turnaround_anchor_v1.png`)。

每动作模板(将 ACTION / FRAMES / PHASES / DIR 替换):

> Use the approved character reference and eight-direction turnaround as strict identity, costume, proportion and
> camera-angle references. Create exactly FRAMES full-body frames of one ACTION animation facing DIR (side view),
> arranged in a strict single row, left to right in time order. Frame phases: PHASES. Keep the facing angle locked,
> the same headband, hairstyle, face, teal-white-gold clothing, boots, pouch and asymmetric hanging accessories,
> stable head size, stable body scale and stable foot baseline. Weapon: none (empty hands, martial-arts strike) for
> attack; glowing talisman in hand for cast. No turning, no extra characters, no text, no grid lines, no cast shadow.
> Clean high-contrast chroma-magenta background for deterministic extraction.

### 2.2 怪物(首批 6 只,MonsterTable id 见 `generated/tables/Monster.json` 前 6 行)

定稿页提示词模板(将 NAME / DESC 替换):

> Create one clean orthographic two-view character sheet (right side view and left side view, side by side) of a
> NAME monster for a cute-chibi Chinese Taoist fantasy MMORPG (问道 style): DESC. Round exaggerated proportions,
> thick clean outlines, bright saturated colors, hand-painted thick-paint look, no realism. Full body, feet visible,
> identical scale and baseline in both views, neutral pose, no labels, no shadows, chroma-magenta background.

动作提示词同 §2.1 模板,把 identity reference 换成怪物定稿页;attack 用该怪物的自然攻击(咬/抓/撞/吐息)。

首批建议:野狼(灰狼,红眼)、山鬼(青面小鬼,骨棒)、狐妖(三尾赤狐)、石灵(青苔圆石人)、
蛇妖(翠绿大蛇)、山贼(布衣刀客)。

### 2.3 场景背景(可选补充)

> 2560×1080 cute-chibi Taoist fantasy battle arena background, side-scrolling stage composition: empty flat ground
> band across the lower 45% for character placement, decorative scenery only in the upper half, soft depth haze,
> bright morning light, hand-painted thick-paint look, no characters, no text, no UI.

## 3. 资产契约(客户端 `BattleArtCatalog` 读取路径)

```
Assets/Resources/Battle/
  Characters/<characterId>/{idle,attack,cast,hit,die,win}_{E,W}_strip.png   # 256 单格横条;缺 W 用 E 镜像;缺动作用程序化动作
  Monsters/<monsterTableId>/{idle,attack,hit,die}_{E,W}_strip.png            # 缺图用剪影占位
  Monsters/<monsterTableId>/portrait.png                                      # 头像(行动预告条)
  Fx/<fxId>_strip.png                                                         # 256 单格 8 帧;程序化生成的一批见 §4
  UI/{panel_9slice,button_9slice,bar_slot,command_ring}.png
  UI/digits_{normal,crit,heal,miss}.png                                       # 数字字集:0-9 - + 闪 暴击,等宽 48×64
  Buff/<buffTableId>.png                                                      # 64×64;缺图用字母块
  Backgrounds/<name>.png
```

- 帧条 meta:Sprite(2D and UI)、Multiple、Grid 256、pivot (0.5,0.08)、Full Rect、Bilinear、无 mip;
  客户端 `QdaoCharacterSpriteImporter` 已按同规则处理 `World/Characters`,扩展其路径匹配到 `Battle/`。
- 特效 fx id 与技能的映射在客户端表 `skill_presentation.json`(导表工具输出:skill_id → fx_id / action(attack|cast)/ hit_frame)。

## 4. 本轮程序化生成清单(`tools/battle_art_gen`,Go,输出到上述路径)

- Fx:`slash_arc`、`thrust_line`、`fire_burst`、`ice_shard`、`lightning_strike`、`heal_ring`、`buff_rise`、`hit_star`、`death_dissolve`(各 8 帧 256)。
- UI:`panel_9slice`(64 边距)、`button_9slice`、`bar_slot`、`command_ring`(7 扇区底)。
- 数字字集 4 套;buff 图标 24 个(增益绿底/减益红底/控制紫底 + 符号)。
- 出生光环 / 入场云层沿用已有图。
