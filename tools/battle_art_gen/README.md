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
go run . -out E:/work/mmorpg-client/Assets/Resources/Battle
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-out` | `E:/work/mmorpg-client/Assets/Resources/Battle` | 输出根目录,不存在会自动创建 |
| `-buff-table` | `../../generated/tables/Buff.json` | buff id 来源导表 |
| `-font` | 空(自动探测) | 数字字集字体;默认依次尝试 `simhei.ttf` / `msyhbd.ttc` / `msyh.ttc` / `simsun.ttc`,`.ttc` 取第一个字体 |
| `-seed` | `20260903` | 随机种子。**同 seed 产出逐字节一致**,特效里的火星/碎片/雷电分叉全部由它派生 |
| `-buff-count` | `24` | buff 图标个数 |

只写 `.png` / `.json`,**不写 `.meta`** —— Unity 首次导入时自动生成。
不触碰客户端其他任何文件。

## 产出

```
Battle/
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
