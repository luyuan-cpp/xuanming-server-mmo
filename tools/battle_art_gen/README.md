# battle_art_gen —— 回合制战斗美术程序化生成器

把回合制战斗表现所需的一批 PNG **算出来**(不依赖生图模型),直接写进客户端
`mmorpg-client/Assets/Resources/Battle/`。规格以
[`docs/design/battle-art-prompts.md`](../../docs/design/battle-art-prompts.md) §3/§4 与
[`docs/design/turn-battle-presentation.md`](../../docs/design/turn-battle-presentation.md) §5 为准。

风格目标:问道 / 梦幻 Q 版手游感 —— 饱和明快、厚描边、柔和发光、**真实 alpha 透明背景**。

## 用法

```powershell
. E:\work\tools\buildenv.ps1          # GOROOT / GOPATH / GOPROXY(goproxy.cn)
cd E:\work\xuanming-server-mmo\tools\battle_art_gen
go build ./...                        # 会在本目录落一个 battle_art_gen.exe(已在 tools/.gitignore 里忽略)
go run . -out E:/work/mmorpg-client/Assets/Resources/Battle          # -mode battle(默认):Fx / UI / 数字 / buff
go run . -mode characters                                            # 22 张 qdao_v3 立绘 → 战斗精灵帧条
go run . -mode monsters                                              # 6 只程序化怪物帧条 + 两条战斗地台
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-mode` | `battle` | `battle` = Fx/UI/数字字集/buff;`characters` = 玩家战斗精灵(见下节);`monsters` = 程序化怪物 + 地台(见「`-mode monsters`」) |
| `-out` | `E:/work/mmorpg-client/Assets/Resources/Battle` | 输出根目录,不存在会自动创建 |
| `-buff-table` | `../../generated/tables/Buff.json` | buff id 来源导表 |
| `-font` | 空(自动探测) | 数字字集字体;默认依次尝试 `simhei.ttf` / `msyhbd.ttc` / `msyh.ttc` / `simsun.ttc`,`.ttc` 取第一个字体 |
| `-seed` | `20260903` | 随机种子。**同 seed 产出逐字节一致**,特效里的火星/碎片/雷电分叉全部由它派生 |
| `-buff-count` | `24` | buff 图标个数 |

只写 `.png` / `.json`,**不写 `.meta`** —— Unity 首次导入时自动生成。
不触碰客户端其他任何文件。

## `-mode characters`:玩家战斗精灵

把 `Assets/Resources/UI/qdao_v3/characters/*.png` 的 22 张 1024 立绘,变成战斗里能直接用的
**256 单格 / 8 帧 / 横条**动作序列 —— 让同一场战斗里不同玩家长得不一样(问道观感的第一要素)。

```powershell
go run . -mode characters                       # 全默认:22 角色 × 4 动作 × 1 朝向(E)= 88 张 strip
go run . -mode characters -char-actions idle,attack,cast,hit,die,win   # 连可选的 die/win 一起出
go run . -mode characters -char-dirs E,W        # 只在需要非对称专有 W 向素材时才加 W(体积翻倍,画面零差异)
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-char-src` | `.../Resources/UI/qdao_v3/characters` | 立绘源目录(`*.png`) |
| `-char-out` | 空 = `<out>/Characters` | 帧条输出目录 |
| `-char-actions` | `idle,attack,cast,hit` | 逗号分隔;可选 `idle,attack,cast,hit,die,win` |
| `-char-dirs` | `E` | 输出朝向。默认只出 `E`;`W` 由客户端 `BattleArtCatalog.LoadDirectionalStrip` 缺图降级取 `E` 并置 `Mirrored=true` 运行时翻转(见下方「为什么不落盘 W」)。不含 `W` 时会顺手删掉目录里残留的 `*_W_strip.png(.meta)` |
| `-char-max-height` | `232` | 角色**身高上限**像素(等比,不拉伸)。实际取值见下方「统一身高」 |
| `-char-alpha` | `16` | 包围盒判定的 alpha 阈值(0..255) |
| `-char-chroma` | `0.18` | 无 alpha 立绘的 chroma 抠图容差(RGB 欧氏距离) |
| `-char-min-comp` | `0.01` | 连通域保留下限(相对最大域面积),小于此值当噪点丢弃 |
| `-char-ss` | `2` | 每轴超采样(2 = 4×),变换后边缘更干净 |

### 管线

1. **去背**:原图带 alpha 就直接用;四边边框不透明占比 > 50% 判定为「无 alpha」,
   改用四角 8×8 均值作参考色做 chroma 容差抠图(容差内全透明,1~2 倍容差线性过渡留软边)。
   *22 张 qdao_v3 立绘实测全部走 alpha 分支,chroma 分支目前是兜底。*
2. **裁边**:8 邻域连通域标记,丢掉面积 < 最大域 1% 的碎片(扫描线噪点),
   保留占比可观的独立块(悬浮法宝 / 飘带),取合并包围盒。
3. **归一化**:等比缩放到统一**身高**(`scale = 身高 / 包围盒高`,宽只随等比走)→ 水平居中 → **最低不透明行压在第 235 行**
   (= `FeetPivot.y 0.08 × 256` 的脚底基线)贴入 256×256 画布。
4. **动作**:对同一张底图做几何(平移 / 旋转 / 错切 / 缩放)+ 色彩(亮度 / 闪白 / 拖影)
   + 程序化叠加(cast 的脚底光晕),**不逐帧重绘**。旋转与错切都以脚底锚点为中心,脚不飘。
5. **打包**:8 帧横向拼成 2048×256,默认只写 `E`;`-char-dirs E,W` 时 `W` = `E` 的**逐格**水平镜像
   (整条翻转会让帧序反过来)。

### 「统一身高」是算出来的,不是拍的

`-char-max-height` 只是上限。工具先量出全部 22 张的**真实轮廓**(逐行取左右极值,64 条带),
再二分求「所有动作、所有帧、所有拖影层变换后仍完整落在 256 画布内」的最大**身高**,
**全体取同一个值**(同场战斗身高一致)。当前默认动作集下解出 **175px**,受
`01_ice_sword_girl` / `10_crimson_spear_girl`(横向持剑/持矛,包围盒 850 宽 > 765/793 高)约束。
实际值记在 `CHARACTER_MANIFEST.json` 的 `params.fitted_height`,每张立绘单独能放下的最大身高记在
各自 `meta.json` 的 `fit_height`(01/10 = 175、17 = 180、其余 189~219)。

统一的是**身高**而不是长边,这一点是复审改过来的:早先按长边(`max(宽, 高)`)归一,横向持矛/持剑的
01/10/17 被按**宽**缩放,idle 身高只有 170/176/179px,而其余角色 189px —— 同排里差 19px(10%),
和后排 0.88 的近大远小是同一量级,阵型深度线索被角色自身身高噪声抵消。现在全体 175px,
自检断言 22 个 idle 首帧头顶行离差 ≤ 4px(实测 0px,全部第 60 行)。代价是原先 189px 的 16 个角色
矮了 14px(7% 分辨率),换来的是同排等高;真要更高就得收 `attack` / `hit` 的横向幅度。

用真实轮廓而不是包围盒矩形是有原因的:立绘顶部两角基本是空的,拿矩形角去判,
`hit` 的后仰会把一个**空角**甩出画布,把可用尺寸从 188 压到 157。

动作的位移/倾角是绝对像素与角度,而角色只有 ~190px 高 —— 倾角在高处会被身高放大
(rot 7° 对 190px 高的角色 = 顶部横移 23px),与错切叠加后很容易顶出画布,
反过来逼着统一身高变小。所以 `attack` / `hit` 的幅度按「留得住画布」调过一轮。
**改这两套动作的幅度会直接改变所有角色的成图尺寸**,改完请重跑并看统一身高的输出行。

### 为什么不落盘 W

客户端 `BattleArtCatalog.LoadDirectionalStrip` 缺 `W` 就取 `E` 并置 `Mirrored=true`,
`BattleUnitView` 按它翻 `localScale.x`(残影同样翻)—— 与工具落盘的逐格镜像副本**逐像素一致**。
而 `Assets/Editor/QdaoCharacterSpriteImporter.cs` 对 `Assets/Resources/Battle/` 强制 Uncompressed,
每张 2048×256 帧条进包固定 2 MiB;22 角色 × 4 动作 的 W 向 = 88 张 = 176 MiB 纯副本,
磁盘 PNG 也多 35 MB。规格本来也允许(`battle-art-prompts.md`「W 缺失时用 E 镜像」)。
所以默认 `-char-dirs E`;只有将来出**非对称专有** W 向素材(逐帧重绘、不是镜像)时才加 `W`。

### 自检

每张 strip 写盘后立刻重新解码并断言:尺寸 2048×256、`NRGBA`(直通 alpha)、
切格帧数 8、每帧非全透明、`idle` 首帧脚底恰好落在第 235 行;
全部角色跑完再断言 **idle 首帧头顶行离差 ≤ 4px**(统一身高的直接证据,结果写进
`CHARACTER_MANIFEST.json` 的 `verify`);画布边缘出现实体像素会打 `!` 告警(说明变换把角色推出了画布)。
任何一张立绘失败都会记进 `CHARACTER_MANIFEST.json` 的 `skipped` 并让进程非 0 退出,
**不会静默少出**。

## 产出

```
Battle/
  Characters/
    CHARACTER_MANIFEST.json             # characterId 列表 + 动作清单 + 性别/职业标签 + 生成参数
    <characterId>/                      # 22 个,id = 立绘文件名去 .png 与 _v3
      {idle,attack,cast,hit}_E_strip.png       # 8 帧 × 256×256 → 2048×256(W 由客户端镜像;-char-dirs E,W 才有 *_W_strip.png)
      meta.json                         # 源文件、包围盒、缩放比、身高 / 单张可放下的最大身高、头顶行、帧数、单格尺寸、主色
  MONSTER_MANIFEST.json                 # -mode monsters:花名册、装箱参数、自检(含剪影两两 IoU)
  Monsters/<monsterTableId>/            # 6 只:{idle,attack,hit}_E_strip.png + meta.json(同样只出 E)
  UI/ground_band_{far,near}.png         # 两条羽化地台光带 1024×512
  ART_MANIFEST.json                     # 全部产物清单:尺寸、帧数、建议帧率、pivot、九宫 border
  Fx/<id>_strip.png                     # 9 条,每条 8 帧 × 256×256 → 2048×256
  UI/panel_9slice.png    256×256        # 圆角金边深棕木纹面板,九宫边距 64
  UI/button_9slice.png   160×64         # 金边玉色按钮,九宫边距 22
  UI/bar_slot.png        320×32         # 血条槽(深色内凹),九宫边距 16/15
  UI/command_ring.png    512×512        # 7 扇区命令环底
  UI/digits_{normal,crit,heal,miss}.png # 等宽 48×64 × 15 格 → 720×64
  UI/digits_meta.json                   # 格宽高 / 字符序 / 字符下标 / 所用字体
  Buff/<buffTableId>.png 64×64          # 24 个
```

Fx id:`slash_arc` `thrust_line` `fire_burst` `ice_shard` `lightning_strike`
`heal_ring` `buff_rise` `hit_star` `death_dissolve`。

数字字集字符序固定为 `0123456789-+闪暴击`(15 格),客户端按字符在该串中的下标切格。

## 实现结构

| 文件 | 职责 |
|---|---|
| `canvas.go` | 预乘 alpha 浮点画布 `Layer`:加法辉光 / source-over 两种混合;软点、双色径向渐变、软椭圆、**距离场路径描边**(沿路径参数 u 变化线宽/颜色/强度)、多边形超采样填充;弧线 / 椭圆 / 三次贝塞尔 / 子路径截取 |
| `mask.go` | 单通道覆盖率遮罩 `Mask`:圆角矩形 SDF、椭圆、粗线、多边形、环形扇区;**保边膨胀**(生成厚描边)、盒式模糊、并/差/交;把遮罩以渐变着色器刷到画布 |
| `fx.go` | 9 套特效逐帧渲染。每套在构造期一次性生成随机量(火星、碎片、雷电分叉、消散粒子),渲染时只按 `t` 求值,保证跨帧连贯且可复现 |
| `ui.go` | 面板 / 按钮 / 血条槽 / 命令环。金边用"外暗→中亮→内暗"斜切;九宫拉伸区保持单方向均匀,不会糊 |
| `digits.go` | 字体光栅化:两遍定尺(先用 64px 探测字形的最大宽/升部/降部,再按格内可用框缩放)。**ASCII 与汉字分两组各自定尺**(否则方块汉字会把数字压得很小),组内共用一条基线、格内水平居中;假粗体 = 遮罩膨胀,厚描边 = 再膨胀一次填深色 |
| `buffs.go` | buff 图标:圆角徽章 + 厚描边 + 顶部高光 + 底部内阴影 + 中央符号(8 种循环) |
| `characters.go` | `-mode characters`:立绘抠图 / 裁边 / 归一化、统一身高求解(轮廓 + 二分)、动作变换表、逐帧仿射渲染(双线性 + 超采样)、横条打包与自检(含全体头顶行离差断言) |
| `monsters.go` | `-mode monsters`:6 套形态模板(beast4 / humanoid / golem / serpent / spirit / insect)+ **逐只形体参数**(`beastFormOf` / `humanFormOf`:体长比、腿长、头径、耳/角/尾/武器模块),统一着色管线,自动装箱 |
| `monsters_verify.go` | 怪物产物回读自检:尺寸 / 帧数 / 真 alpha / 脚底行 / 越界;**任意两只 idle 首帧归一化剪影 IoU ≤ 0.6**(24×32,alpha≥128);含 W 时逐像素镜像比对 |
| `ground.go` | 两条羽化椭圆地台光带 |
| `main.go` | 参数、目录、拼横条、写 PNG / JSON、清单 |

依赖:标准库 `image` / `image/draw` / `image/png` + `golang.org/x/image`(仅用于字体:
`font`、`font/opentype`、`math/fixed`)。

## buff 底色是怎么定的

`generated/tables/Buff.json` 有 `buff_type` 字段,但**没有"增益 / 减益 / 控制"的极性语义**
(proto 里也没有对应 enum)。所以用一条确定性规则:把出现过的 `buff_type` 去重升序排列,
按序号 `%3` 依次分配 增益绿 / 减益红 / 控制紫,映射结果写进 `ART_MANIFEST.json` 的
`notes` 与 `buff_table_mapping`。表里补上极性字段后,只需改 `buffs.go` 的
`categorizeBuffTypes`。

`Buff.json` 目前只有 20 行,`-buff-count 24` 时用连续 id(21..24)补 4 个占位图标,
清单里标 `"source": "padding"`。

## 客户端接线要点

- Fx 横条:Sprite(2D and UI)、Multiple、Grid 256×256、Full Rect、Bilinear、关 mipmap。
  pivot 按 `ART_MANIFEST.json` 每条的 `pivot`:范围型 `(0.5,0.5)` 贴目标中心,
  地面型(`heal_ring` / `buff_rise` / `death_dissolve`)`(0.5,0.1)` 贴脚底。
- 九宫图:Sprite Single + Border(清单里的 `border_9slice_lbrt` = 左/下/右/上)。
- 特效带大面积 alpha 渐变,贴图压缩建议 RGBA32 / 关 Crunch,否则会出色带。
- **角色帧条不需要任何 Sprite 导入设置**:`BattleArtCatalog.LoadStrip` 按 `Texture2D` 读入后
  运行时 `Sprite.Create` 切格(`cellSize=0` → 格宽 = 贴图高 = 256,帧数 = 2048/256 = 8),
  pivot 走 `FeetPivot (0.5,0.08)`。`Assets/Editor/QdaoCharacterSpriteImporter.cs` 已覆盖
  `Assets/Resources/Battle/`,保证 Bilinear / 无 mip / 无压缩 / `alphaIsTransparency` / maxSize 4096。
- 角色与怪物帧条**只出 `E` 向**,`W` 走 `LoadDirectionalStrip` 的镜像降级分支(`Mirrored=true`);这是有意为之,见「为什么不落盘 W」。

## `-mode monsters`:程序化怪物 + 地台

```powershell
go run . -mode monsters                         # 花名册前 6 只(battle-art-prompts.md §2.2 首批)× 3 动作 × E 向 = 18 张 + 2 条地台
go run . -mode monsters -monster-count 16       # 连扩展花名册一起出
go run . -mode monsters -monster-dirs E,W       # 同 -char-dirs,默认只出 E
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-monster-count` | `6` | 生成花名册前 N 只(最多 16) |
| `-monster-dirs` | `E` | 输出朝向;不含 `W` 时删掉残留的 `*_W_strip.png(.meta)`,自检也会拒绝目录里残留的 W |

同一模板的怪**不只是换色**:野狼(高腿、背鬃、下垂单尾、长吻)vs 狐妖(低长躯干、短腿、大耳、三条高扬蓬尾),
山鬼(矮胖佝偻、大头双角、高举骨棒、裸腿宽站)vs 山贼(高瘦直立、头巾飘带、短裙、平持单刀 + 背后刀鞘)。
复审前这两对归一化剪影 IoU 0.85 / 0.81(只靠颜色区分,受击闪白时不可辨),现在 0.30 / 0.41;
自检对任意两只断言 IoU ≤ 0.6,超过即非 0 退出。

⚠ 扩展花名册(7~16 号)目前**过不了这条断言**:石魔/山神像 = 石灵换色(IoU 0.99)、铁甲蟒 = 蛇妖(0.99)、
蚀骨虫 = 毒蜂(0.97)、怨灵 = 幽魂(0.97)、狼王 ≈ 野狼(0.80)、山魈 ≈ 山鬼(0.72),另外石灵这种"填满包围盒"的方块
剪影与任何紧凑剪影(幽魂/阴兵/毒蜂)也会到 0.62~0.73。所以 `-monster-count` 调大之前要先给这些怪补
`beastForm` / `humanForm` 式的逐只形体参数(golem / serpent / spirit / insect 模板还没参数化),这是有意让工具拒绝出"换色克隆"。
