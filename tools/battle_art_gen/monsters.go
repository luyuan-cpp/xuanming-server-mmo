package main

// monsters.go —— 纯程序化怪物战斗精灵(`-mode monsters`)。
//
// 不依赖任何外部素材:每只怪按"形态模板"用 mask.go / canvas.go 的图元拼出剪影,
// 再套一层统一的着色管线(厚描边 → 分部位竖向渐变 → 顶部亮边 → 底部压深 → 眼睛高光),
// 保证跟 Fx 一批产物同一套观感(厚描边、饱和主色、暗部压深)。
//
// 规格(与角色帧条一致,见 BattleArtCatalog.LoadStrip / FeetPivot):
//   - 单帧 256×256,横条 = 8 帧 × 256 → 2048×256,左→右时间序;
//   - 脚底(含描边)对齐底边上方 20px(= FeetPivot.y 0.08 × 256,与角色帧条同一基线;
//     规格书里的 8px 见 monFeetPx 注释);
//   - 默认只出 E 向(-monster-dirs E);W 由客户端 BattleArtCatalog.LoadDirectionalStrip 缺图降级
//     取 E 并置 Mirrored=true 运行时翻转。落盘的 W 只是 E 的逐格镜像副本,画面零差异但体积翻倍,
//     只在 -monster-dirs E,W 时才写。
//   - 同模板的怪不能只靠换色区分:形体参数(beastForm / humanForm)按怪逐只给,
//     monsters_verify.go 断言任意两只 idle 首帧归一化剪影 IoU ≤ monMaxSilhouetteIoU。
//
// 输出:
//   Battle/Monsters/<monsterTableId>/{idle,attack,hit}_E_strip.png(含 W 时再加 *_W_strip.png)
//   Battle/Monsters/<monsterTableId>/meta.json
//   Battle/MONSTER_MANIFEST.json

import (
	"fmt"
	"image"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 常量与模式开关
// ---------------------------------------------------------------------------

const (
	monFrameSize = 256  // 单格边长
	monFrames    = 8    // 每个动作帧数
	monOutline   = 3.2  // 厚描边半径
	monBaseX     = 118. // 脚底基点 x(略偏左,给前扑留出空间)

	// monFeetPx 脚底(含描边)距画布底边的像素数。
	//
	// ⚠ 规格书写的是 8px,但**权威是 BattleArtCatalog.FeetPivot = (0.5, 0.08)**:
	// 0.08 × 256 = 20.48px。角色帧条(characters.go / charFeetFromBottom)已经按 20 出,
	// 怪物若按 8 出会比同场的玩家整整高出 12px(脚离地),所以这里同样取 20。
	monFeetPx = 20.0
)

// monBaselineRow 脚底基线所在的像素行(自检断言用)= 256-1-20 = 235,与 charBaselineRow 一致。
const monBaselineRow = monFrameSize - 1 - int(monFeetPx)

// genMode 由 main.go 的 -mode 绑定:all(默认,全部产物)/ fx(只跑原有 Fx/UI/数字/buff)/ monsters(只跑怪物+地台)。
var genMode = "all"

// monsterCount 由 main.go 的 -monster-count 绑定:生成花名册前 N 只。
var monsterCount = 6

// monsterDirs 由 main.go 的 -monster-dirs 绑定:输出朝向(默认只有 E)。
var monsterDirs = []string{"E"}

// monWantsDir 判断 monsterDirs 是否包含某个朝向。
func monWantsDir(dir string) bool {
	for _, d := range monsterDirs {
		if strings.EqualFold(d, dir) {
			return true
		}
	}
	return false
}

// monActions 三套动作及其建议帧率(与 BattleArtCatalog.ActionFps 对齐)。
var monActions = []struct {
	Name string
	FPS  int
	Desc string
}{
	{"idle", 6, "呼吸/漂浮循环(整周期 8 帧无缝循环)"},
	{"attack", 12, "后引 → 前扑(带拖影)→ 命中顶点(第 5 帧)→ 收势回中立"},
	{"hit", 12, "后仰闪白 → 最大后仰 → 回弹归位"},
}

// ---------------------------------------------------------------------------
// 花名册
// ---------------------------------------------------------------------------

// monsterSpec 一只怪的美术定义。ID 对应 generated/tables/Monster.json 的 id。
type monsterSpec struct {
	ID       uint32
	Name     string  // 中文名(Monster 表无 name 列,取自 battle-art-prompts.md §2.2 / 工具内置扩展)
	Template string  // 形态模板
	Scale    float64 // 模型空间 → 像素的整体缩放
	Main     RGB     // 主色
	Dark     RGB     // 远侧肢体 / 暗部
	Belly    RGB     // 腹部 / 浅色区
	Accent   RGB     // 角、爪、武器、苔藓等点缀
	Eye      RGB     // 眼睛高光色
	Note     string
	Source   string // "battle-art-prompts.md §2.2" | "工具内置扩展"
}

// monsterRoster 前 6 只与 docs/design/battle-art-prompts.md §2.2 的首批清单一一对应;
// 7 号之后是工具内置扩展(Monster 表同样没有名字),用来覆盖全部 6 套形态模板。
var monsterRoster = []monsterSpec{
	{1, "野狼", "beast4", 1.00,
		rgb8(126, 138, 158), rgb8(68, 78, 98), rgb8(198, 206, 216), rgb8(238, 243, 250), rgb8(255, 92, 72),
		"灰狼、红眼、竖耳长吻、单尾", "battle-art-prompts.md §2.2"},
	{2, "山鬼", "humanoid", 0.94,
		rgb8(98, 170, 112), rgb8(48, 104, 66), rgb8(188, 226, 172), rgb8(234, 222, 188), rgb8(255, 214, 64),
		"青面小鬼、双角、骨棒", "battle-art-prompts.md §2.2"},
	{3, "狐妖", "beast4", 0.96,
		rgb8(234, 108, 64), rgb8(170, 58, 38), rgb8(255, 240, 218), rgb8(255, 198, 122), rgb8(126, 240, 255),
		"赤狐、三尾、大耳尖吻、妖异青瞳", "battle-art-prompts.md §2.2"},
	{4, "石灵", "golem", 1.06,
		rgb8(130, 134, 142), rgb8(72, 76, 86), rgb8(170, 174, 182), rgb8(104, 178, 96), rgb8(140, 230, 255),
		"棱角岩块堆叠、青苔斑、冰蓝眼缝", "battle-art-prompts.md §2.2"},
	{5, "蛇妖", "serpent", 1.00,
		rgb8(74, 184, 112), rgb8(34, 110, 72), rgb8(216, 240, 168), rgb8(255, 122, 152), rgb8(255, 200, 60),
		"翠绿大蛇、三段盘身、分叉信子", "battle-art-prompts.md §2.2"},
	{6, "山贼", "humanoid", 0.99,
		rgb8(152, 114, 78), rgb8(92, 64, 44), rgb8(208, 182, 142), rgb8(216, 224, 234), rgb8(255, 122, 82),
		"布衣刀客、头巾、单刀", "battle-art-prompts.md §2.2"},

	// —— 以下为扩展花名册(默认不生成,-monster-count 调大即可)——
	{7, "毒蜂", "insect", 0.90,
		rgb8(226, 186, 62), rgb8(140, 100, 26), rgb8(250, 232, 168), rgb8(96, 74, 60), rgb8(180, 60, 220), "六足、螯钳、尾针", "工具内置扩展"},
	{8, "幽魂", "spirit", 0.98,
		rgb8(126, 168, 226), rgb8(58, 88, 148), rgb8(214, 234, 255), rgb8(180, 214, 255), rgb8(150, 255, 236), "漂浮、无足、下摆化雾", "工具内置扩展"},
	{9, "铁甲蟒", "serpent", 1.08,
		rgb8(118, 128, 150), rgb8(58, 66, 88), rgb8(196, 204, 222), rgb8(232, 178, 72), rgb8(255, 120, 60), "重甲巨蟒", "工具内置扩展"},
	{10, "山魈", "beast4", 1.06,
		rgb8(178, 92, 132), rgb8(104, 44, 74), rgb8(238, 208, 224), rgb8(250, 240, 210), rgb8(255, 230, 90), "四足灵长", "工具内置扩展"},
	{11, "石魔", "golem", 1.12,
		rgb8(106, 96, 122), rgb8(58, 50, 72), rgb8(154, 144, 172), rgb8(224, 96, 72), rgb8(255, 150, 90), "熔纹岩魔", "工具内置扩展"},
	{12, "阴兵", "humanoid", 1.02,
		rgb8(92, 106, 128), rgb8(44, 54, 72), rgb8(160, 176, 198), rgb8(206, 218, 232), rgb8(120, 255, 200), "披甲鬼卒", "工具内置扩展"},
	{13, "蚀骨虫", "insect", 0.96,
		rgb8(158, 196, 96), rgb8(84, 116, 48), rgb8(226, 244, 176), rgb8(238, 234, 214), rgb8(255, 96, 96), "甲壳巨虫", "工具内置扩展"},
	{14, "怨灵", "spirit", 1.04,
		rgb8(168, 108, 200), rgb8(92, 50, 122), rgb8(230, 200, 250), rgb8(212, 172, 255), rgb8(255, 236, 120), "怨气化形", "工具内置扩展"},
	{15, "黑风狼王", "beast4", 1.14,
		rgb8(74, 82, 104), rgb8(36, 40, 56), rgb8(152, 162, 184), rgb8(226, 236, 250), rgb8(255, 60, 60), "狼群首领", "工具内置扩展"},
	{16, "山神像", "golem", 1.18,
		rgb8(150, 128, 92), rgb8(88, 72, 48), rgb8(196, 176, 138), rgb8(236, 198, 96), rgb8(120, 240, 200), "镇山石像", "工具内置扩展"},
}

// ---------------------------------------------------------------------------
// 模型空间变换
// ---------------------------------------------------------------------------

// xf 把"模型空间"(原点在脚底中心,x 向右为正前方,y 向上)映射到图像空间(y 向下)。
// 顺序:缩放 → 绕脚底基点前倾 rot(rot>0 = 上身朝 +x 倾)→ 平移。
type xf struct {
	ox, oy float64
	sx, sy float64
	rot    float64
}

func (t xf) pt(x, y float64) [2]float64 {
	px, py := x*t.sx, y*t.sy
	c, s := math.Cos(t.rot), math.Sin(t.rot)
	rx := px*c + py*s
	ry := -px*s + py*c
	return [2]float64{t.ox + rx, t.oy - ry}
}

func (t xf) lineScale() float64 { return (math.Abs(t.sx) + math.Abs(t.sy)) * 0.5 }

// pen 在模型空间下笔,直接落到某个 Mask 上。
type pen struct {
	m *Mask
	t xf
}

// ellipse 模型空间椭圆(rot 为椭圆自身倾角),用 44 边形近似,保证变换后仍抗锯齿。
func (p pen) ellipse(cx, cy, rx, ry, rot float64) {
	const n = 44
	pts := make([][2]float64, 0, n)
	ca, sa := math.Cos(rot), math.Sin(rot)
	for i := 0; i < n; i++ {
		a := 2 * math.Pi * float64(i) / n
		ex := rx * math.Cos(a)
		ey := ry * math.Sin(a)
		pts = append(pts, p.t.pt(cx+ex*ca-ey*sa, cy+ex*sa+ey*ca))
	}
	p.m.Poly(pts)
}

// poly 模型空间多边形。
func (p pen) poly(pts [][2]float64) {
	out := make([][2]float64, len(pts))
	for i, q := range pts {
		out[i] = p.t.pt(q[0], q[1])
	}
	p.m.Poly(out)
}

// bar 模型空间圆头粗线(肢体/棍棒)。
func (p pen) bar(x0, y0, x1, y1, w float64) {
	a := p.t.pt(x0, y0)
	b := p.t.pt(x1, y1)
	p.m.Line(a[0], a[1], b[0], b[1], w*p.t.lineScale())
}

// taper 沿折线画渐细的粗线(尾巴/触角/蛇颈)。
func (p pen) taper(pts [][2]float64, w0, w1 float64) {
	if len(pts) < 2 {
		return
	}
	// 先细分,避免关节处出现台阶
	dense := make([][2]float64, 0, (len(pts)-1)*8+1)
	for i := 0; i+1 < len(pts); i++ {
		for k := 0; k < 8; k++ {
			u := float64(k) / 8
			dense = append(dense, [2]float64{
				pts[i][0] + (pts[i+1][0]-pts[i][0])*u,
				pts[i][1] + (pts[i+1][1]-pts[i][1])*u,
			})
		}
	}
	dense = append(dense, pts[len(pts)-1])
	n := float64(len(dense) - 1)
	for i := 0; i+1 < len(dense); i++ {
		u := float64(i) / n
		p.bar(dense[i][0], dense[i][1], dense[i+1][0], dense[i+1][1], w0+(w1-w0)*u)
	}
}

// rotAbout 把一组模型空间点绕 (cx,cy) 旋转 ang(正 = 朝 +x 压下)。
func rotAbout(pts [][2]float64, cx, cy, ang float64) [][2]float64 {
	c, s := math.Cos(ang), math.Sin(ang)
	out := make([][2]float64, len(pts))
	for i, q := range pts {
		dx, dy := q[0]-cx, q[1]-cy
		out[i] = [2]float64{cx + dx*c + dy*s, cy - dx*s + dy*c}
	}
	return out
}

// ---------------------------------------------------------------------------
// 姿势曲线
// ---------------------------------------------------------------------------

// pose 一帧的整体姿势参数。
type pose struct {
	dx, dy float64 // 图像空间位移(dx 正 = 朝正前方)
	lean   float64 // 前倾弧度
	scaleY float64 // 竖向压缩(呼吸)
	limb   float64 // 0..1 肢体相位
	open   float64 // 张嘴 / 挥武器 / 张螯 0..1
	flash  float64 // 闪白 0..1
	ghost  float64 // 拖影强度 0..1
}

func lerpKeys(keys [monFrames]float64, f int) float64 { return keys[f%monFrames] }

// poseFor 按动作 + 帧号求姿势。floaty=true 的模板(灵体/蛇)用更大的漂浮幅度。
func poseFor(action string, f int, floaty bool) pose {
	p := pose{scaleY: 1, limb: float64(f) / monFrames}
	switch action {
	case "idle":
		ph := 2 * math.Pi * float64(f) / monFrames // 整周期 → 首尾无缝
		if floaty {
			// 漂浮:整段位移都 ≤ 0(只往上浮,不往下沉),否则脚底会掉到基线以下
			p.dy = -4.5 + 4.5*math.Cos(ph)
			p.lean = 0.05 * math.Sin(ph+1.0)
			p.scaleY = 1 + 0.02*math.Sin(ph*2)
		} else {
			p.dy = -2.2 * math.Sin(ph)
			p.lean = 0.016 * math.Sin(ph)
			p.scaleY = 1 + 0.028*math.Sin(ph)
		}
		p.open = 0.08 + 0.06*math.Sin(ph)
	case "attack":
		dxK := [monFrames]float64{-5, -11, 4, 18, 26, 20, 9, 0}
		dyK := [monFrames]float64{0, 2, -5, -9, -6, -1, 1, 0}
		lnK := [monFrames]float64{-0.05, -0.10, 0.06, 0.18, 0.24, 0.15, 0.05, 0}
		opK := [monFrames]float64{0.05, 0.22, 0.55, 0.90, 1.00, 0.68, 0.28, 0.08}
		ghK := [monFrames]float64{0, 0, 0.35, 0.80, 1.00, 0.55, 0.15, 0}
		syK := [monFrames]float64{1, 0.965, 1.03, 1.04, 1.0, 0.98, 1.0, 1.0}
		p.dx, p.dy = lerpKeys(dxK, f), lerpKeys(dyK, f)
		p.lean, p.open = lerpKeys(lnK, f), lerpKeys(opK, f)
		p.ghost, p.scaleY = lerpKeys(ghK, f), lerpKeys(syK, f)
	case "hit":
		dxK := [monFrames]float64{0, -12, -18, -14, -9, -5, -2, 0}
		dyK := [monFrames]float64{0, -3, -1, 0, 1, 0, 0, 0}
		lnK := [monFrames]float64{0, -0.16, -0.22, -0.17, -0.10, -0.05, -0.02, 0}
		// 闪白峰值压到 0.9 以下并快速回落:再高整只怪就糊成一团白,剪影读不出来了
		flK := [monFrames]float64{0.70, 0.88, 0.46, 0.26, 0.13, 0.05, 0, 0}
		syK := [monFrames]float64{1, 0.955, 0.94, 0.97, 0.99, 1, 1, 1}
		opK := [monFrames]float64{0.55, 0.75, 0.60, 0.35, 0.18, 0.10, 0.05, 0.05}
		p.dx, p.dy = lerpKeys(dxK, f), lerpKeys(dyK, f)
		p.lean, p.flash = lerpKeys(lnK, f), lerpKeys(flK, f)
		p.scaleY, p.open = lerpKeys(syK, f), lerpKeys(opK, f)
	}
	return p
}

// ---------------------------------------------------------------------------
// 形态模板
// ---------------------------------------------------------------------------

// monBody 一帧的分部位遮罩(按绘制顺序:back → body → belly → head → mouth → front → accent)。
type monBody struct {
	back, body, belly, head, mouth, front, accent *Mask
	eyes                                          [][2]float64 // 图像空间
	eyeR                                          float64
}

func newBody(size int) *monBody {
	return &monBody{
		back: NewMask(size, size), body: NewMask(size, size), belly: NewMask(size, size),
		head: NewMask(size, size), mouth: NewMask(size, size), front: NewMask(size, size),
		accent: NewMask(size, size), eyeR: 4.2,
	}
}

func (b *monBody) ordered() []*Mask {
	return []*Mask{b.back, b.body, b.belly, b.head, b.mouth, b.front, b.accent}
}

func (b *monBody) union() *Mask {
	u := NewMask(b.back.W, b.back.H)
	for _, m := range b.ordered() {
		for i, v := range m.A {
			if v > u.A[i] {
				u.A[i] = v
			}
		}
	}
	return u
}

// floatyTemplate 漂浮类模板(idle 用更大的上下摆幅)。
func floatyTemplate(tpl string) bool { return tpl == "spirit" || tpl == "serpent" }

// monHoverPx 该模板的"离地高度":灵体是真的飘在半空,其余(含盘身的蛇)都算落地。
func monHoverPx(tpl string) float64 {
	if tpl == "spirit" {
		return 22
	}
	return 0
}

// monBottomTarget 该怪的最低实体像素应该落在哪一行。
func monBottomTarget(sp monsterSpec) int {
	return monBaselineRow - int(monHoverPx(sp.Template))
}

// ---------------------------------------------------------------------------
// 自动适配(脚底钉线 + 画布装箱)
// ---------------------------------------------------------------------------

// monFit 一只怪最终用的落笔参数:整只怪的**全部动作全部帧**都待在画布内,且脚底钉在目标行。
//
// 为什么要自动量而不是手调常量:
//  1. 肢体用的是圆头粗线(pen.bar → Mask.Line),圆头会在落地端点下方多出半个线宽,
//     再叠 monOutline 的厚描边,每个模板多出来的量都不一样(狼约 10px、石灵约 3px)——
//     按 gy 硬摆的话每只怪脚底各差好几像素;
//  2. attack 的前扑(dx +26、前倾 0.24 rad)会把长吻/刀尖甩出右边界,hit 的后仰
//     (dx −18、后倾 −0.22 rad)会把尾巴甩出左边界,而"甩多远"取决于造型本身。
//
// 于是量一遍:先把 idle 第 0 帧(中立姿势)的最低实体行钉到 monBottomTarget,
// 再看 24 帧的总包围盒有没有出框 —— 出了就等比缩小 / 横向平移,最多迭代 8 轮。
// 同一只怪所有帧共用同一组 (Scale, OX, OY),所以动画位移不受影响。
type monFit struct {
	Scale  float64 `json:"scale"`      // 最终缩放(≤ spec.Scale)
	OX     float64 `json:"origin_x"`   // 最终脚底基点 x
	OY     float64 `json:"origin_y"`   // 最终脚底基点 y(已把脚底钉到目标行)
	Iters  int     `json:"fit_iters"`  // 收敛用的迭代轮数
	Bottom int     `json:"bottom_row"` // idle 首帧最低实体行(= monBottomTarget)
}

var monFitCache = map[uint32]monFit{}

// maskBBox 求遮罩的实体包围盒(阈值 0.02)。
func maskBBox(m *Mask) (x0, y0, x1, y1 float64, empty bool) {
	ix0, iy0, ix1, iy1 := m.W, m.H, -1, -1
	for y := 0; y < m.H; y++ {
		row := y * m.W
		for x := 0; x < m.W; x++ {
			if m.A[row+x] <= 0.02 {
				continue
			}
			if x < ix0 {
				ix0 = x
			}
			if x > ix1 {
				ix1 = x
			}
			if y < iy0 {
				iy0 = y
			}
			if y > iy1 {
				iy1 = y
			}
		}
	}
	if ix1 < 0 {
		return 0, 0, 0, 0, true
	}
	return float64(ix0), float64(iy0), float64(ix1), float64(iy1), false
}

// monMeasure 在给定落笔参数下量出「全部动作 × 全部帧」并集的包围盒(已含厚描边余量),
// 外加 idle 第 0 帧的最低行。为了不被画布裁掉真实外扩量,这里在 2× 的临时画布上量。
func monMeasure(sp monsterSpec, ox, oy, scale float64, canvas int) (x0, y0, x1, y1, idleBottom float64, ok bool) {
	pad := math.Ceil(monOutline) + 1 // 厚描边把包围盒往外推的量
	x0, y0 = math.Inf(1), math.Inf(1)
	x1, y1 = math.Inf(-1), math.Inf(-1)
	floaty := floatyTemplate(sp.Template)
	for _, act := range monActions {
		for f := 0; f < monFrames; f++ {
			ps := poseFor(act.Name, f, floaty)
			t := xf{ox: ox + ps.dx, oy: oy + ps.dy, sx: scale, sy: scale * ps.scaleY, rot: ps.lean}
			bx0, by0, bx1, by1, empty := maskBBox(buildBody(sp, ps, t, canvas).union())
			if empty {
				continue
			}
			ok = true
			x0, y0 = math.Min(x0, bx0-pad), math.Min(y0, by0-pad)
			x1, y1 = math.Max(x1, bx1+pad), math.Max(y1, by1+pad)
			if act.Name == "idle" && f == 0 {
				idleBottom = by1 + pad
			}
		}
	}
	return
}

// fitOf 迭代求解 monFit(结果按怪 id 缓存,一只怪只量一次)。
func fitOf(sp monsterSpec, size int) monFit {
	if v, ok := monFitCache[sp.ID]; ok {
		return v
	}
	const margin = 3.0       // 画布四边至少留这么多空白
	canvas := size * 2       // 量测用的大画布
	off := float64(size) / 2 // 画布坐标 = 量测坐标 − off

	scale, ox := sp.Scale, monBaseX
	oy := float64(size) - monFeetPx
	target := float64(monBottomTarget(sp))
	fit := monFit{Scale: scale, OX: ox, OY: oy, Bottom: monBottomTarget(sp)}

	for iter := 1; iter <= 8; iter++ {
		mx0, my0, mx1, _, ib, ok := monMeasure(sp, ox+off, oy+off, scale, canvas)
		if !ok {
			break
		}
		x0, y0, x1, ibot := mx0-off, my0-off, mx1-off, ib-off

		// ① 先把脚底钉到目标行(纯平移,不影响横向)
		dy := target - ibot
		oy += dy
		y0 += dy

		changed := false
		// ② 顶部出框 → 以脚底为锚等比缩小
		if y0 < margin {
			if k := (target - margin) / (target - y0); k < 0.999 {
				scale *= k
				changed = true
			}
		}
		// ③ 横向:装不下就缩,装得下就平移进框
		avail := float64(size) - 2*margin
		if w := x1 - x0; w > avail {
			scale *= avail / w * 0.998
			changed = true
		} else {
			if x0 < margin {
				ox += margin - x0
				changed = true
			}
			if x1 > float64(size)-margin {
				ox -= x1 - (float64(size) - margin)
				changed = true
			}
		}
		fit = monFit{Scale: scale, OX: ox, OY: oy, Iters: iter, Bottom: monBottomTarget(sp)}
		if !changed && math.Abs(dy) < 0.5 {
			break
		}
	}

	// ④ 收尾:上面装箱时用的是"厚描边外扩量"的保守估计(ceil(r)+1),会比真实膨胀多 1~2px。
	// 这里在真实画布上做一次真膨胀,把脚底精确钉到目标行(只差 ±2px,不会把造型顶出框)。
	ps := poseFor("idle", 0, floatyTemplate(sp.Template))
	t := xf{
		ox: fit.OX + ps.dx, oy: fit.OY + ps.dy,
		sx: fit.Scale, sy: fit.Scale * ps.scaleY, rot: ps.lean,
	}
	if _, bot, empty := maskBounds(buildBody(sp, ps, t, size).union().Dilate(monOutline)); !empty {
		fit.OY += float64(monBottomTarget(sp)) - bot
	}

	monFitCache[sp.ID] = fit
	return fit
}

// buildBody 按模板拼出一帧的分部位遮罩。
func buildBody(sp monsterSpec, ps pose, t xf, size int) *monBody {
	b := newBody(size)
	switch sp.Template {
	case "beast4":
		buildBeast4(b, sp, ps, t)
	case "humanoid":
		buildHumanoid(b, sp, ps, t)
	case "golem":
		buildGolem(b, sp, ps, t)
	case "serpent":
		buildSerpent(b, sp, ps, t)
	case "spirit":
		buildSpirit(b, sp, ps, t)
	case "insect":
		buildInsect(b, sp, ps, t)
	default:
		buildBeast4(b, sp, ps, t)
	}
	return b
}

const gy = 4.0 // 模型空间地面高度(留出描边余量,保证脚底恰好落在底边上方 8px)

// beastForm 兽形四足模板的形体参数(模型空间:脚底原点,x 向前,y 向上)。
//
// 同模板的怪以前只换色不换形(野狼 vs 狐妖归一化剪影 IoU 0.80),受击闪白或色弱时根本分不清。
// 这里把体长比 / 腿长 / 头径 / 吻长 / 耳型 / 尾巴模块 / 背鬃全部参数化,每只怪单独给一组。
type beastForm struct {
	LegLen, LegW       float64 // 腿长(脚底到肩/髋)/ 腿粗
	LegFront, LegBack  float64 // 前腿 / 后腿落脚点 x
	BodyCX, BodyCY     float64 // 躯干中心
	BodyRX, BodyRY     float64 // 躯干半轴(体长比 = BodyRX / BodyRY)
	ChestX, ChestY     float64 // 胸块中心
	ChestR             float64 // 胸块半径
	NeckW              float64 // 颈粗
	HeadX, HeadY       float64 // 头中心
	HeadRX, HeadRY     float64 // 头半轴
	MuzzleLen, MuzzleH float64 // 吻长 / 吻高
	EarH, EarW         float64 // 耳高 / 耳宽
	EarSpread          float64 // 双耳间距
	Tails              int     // 尾巴条数
	TailX, TailY       float64 // 尾根
	TailAng            float64 // 尾巴主方向(弧度;0 = 水平向后,正 = 向上翘,负 = 下垂)
	TailFan            float64 // 多尾时相邻两尾的夹角
	TailLen            float64 // 尾长
	TailW0, TailW1     float64 // 尾根 / 尾尖粗
	TailCurl           float64 // 尾尖回勾量(正 = 向上勾)
	Ridge              int     // 背鬃刺个数(0 = 无)
	RidgeH             float64 // 背鬃刺高
	Mane               bool    // 颈鬃(狼王)
	Belly              bool    // 腹部浅色块
}

// beastFormOf 按怪 id 取形体参数;未列出的兽形怪走野狼形。
func beastFormOf(sp monsterSpec) beastForm {
	wolf := beastForm{
		LegLen: 54, LegW: 13, LegFront: 24, LegBack: -26,
		BodyCX: -4, BodyCY: 82, BodyRX: 44, BodyRY: 22,
		ChestX: 28, ChestY: 88, ChestR: 24, NeckW: 20,
		HeadX: 66, HeadY: 122, HeadRX: 22, HeadRY: 18,
		MuzzleLen: 28, MuzzleH: 15,
		EarH: 26, EarW: 11, EarSpread: 16,
		Tails: 1, TailX: -44, TailY: 84, TailAng: -0.62, TailLen: 54, TailW0: 13, TailW1: 6, TailCurl: 0,
		Ridge: 5, RidgeH: 9, Belly: true,
	}
	switch sp.Name {
	case "狐妖":
		// 狐:躯干更低更长、腿短、头小吻尖、大耳、三条蓬尾高高扬起
		return beastForm{
			LegLen: 32, LegW: 10, LegFront: 30, LegBack: -34,
			BodyCX: -10, BodyCY: 50, BodyRX: 60, BodyRY: 19,
			ChestX: 34, ChestY: 54, ChestR: 19, NeckW: 15,
			HeadX: 62, HeadY: 78, HeadRX: 18, HeadRY: 14,
			MuzzleLen: 24, MuzzleH: 10,
			EarH: 32, EarW: 14, EarSpread: 18,
			Tails: 3, TailX: -58, TailY: 56, TailAng: 0.58, TailFan: 0.34, TailLen: 78, TailW0: 22, TailW1: 5, TailCurl: 8,
			Ridge: 0, Belly: true,
		}
	case "山魈":
		// 山魈:灵长类——躯干短而高、前肢长、大头、无尾
		f := wolf
		f.LegLen, f.LegW, f.LegFront, f.LegBack = 62, 15, 30, -18
		f.BodyCX, f.BodyCY, f.BodyRX, f.BodyRY = -2, 96, 34, 30
		f.ChestX, f.ChestY, f.ChestR = 24, 100, 26
		f.HeadX, f.HeadY, f.HeadRX, f.HeadRY = 56, 134, 26, 24
		f.MuzzleLen, f.MuzzleH = 16, 14
		f.EarH, f.EarW = 14, 12
		f.Tails, f.Ridge, f.Mane = 0, 0, true
		return f
	case "黑风狼王":
		f := wolf
		f.BodyRX, f.BodyRY, f.ChestR = 48, 25, 27
		f.Ridge, f.RidgeH, f.Mane = 7, 13, true
		f.TailLen, f.TailW0 = 62, 16
		return f
	}
	return wolf
}

// buildBeast4 兽形四足:野狼 / 狐妖 / 山魈 / 狼王,形体由 beastFormOf 给。
func buildBeast4(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	ph := pen{b.head, t}
	pf := pen{b.front, t}
	pa := pen{b.accent, t}
	pk := pen{b.mouth, t}

	fm := beastFormOf(sp)
	sw := math.Sin(2 * math.Pi * ps.limb) // 步态摆动
	hip := fm.LegLen

	// 远侧两腿(暗色)
	pb.bar(fm.LegBack+2+3*sw, gy, fm.LegBack-4, hip, fm.LegW)
	pb.bar(fm.LegFront-4-3*sw, gy, fm.LegFront-8, hip, fm.LegW-1)

	// 尾巴:从尾根沿 TailAng 方向甩出,多尾按 TailFan 扇开;TailCurl 让尾尖回勾
	for i := 0; i < fm.Tails; i++ {
		a := fm.TailAng + (float64(i)-float64(fm.Tails-1)/2)*fm.TailFan
		dir := func(u float64) [2]float64 {
			// u ∈ [0,1] 沿尾巴;尾尖附近加回勾与步态摆动
			ang := a + u*u*fm.TailCurl*0.06
			return [2]float64{
				fm.TailX - math.Cos(ang)*fm.TailLen*u,
				fm.TailY + math.Sin(ang)*fm.TailLen*u + 4*sw*u,
			}
		}
		pts := [][2]float64{dir(0), dir(0.35), dir(0.7), dir(1)}
		// 蓬尾:中段最粗,尾根略细(狐);狼尾则线性渐细
		if fm.Tails > 1 {
			pb.taper(pts[:3], fm.TailW0*0.7, fm.TailW0)
			pb.taper(pts[2:], fm.TailW0, fm.TailW1)
		} else {
			pb.taper(pts, fm.TailW0, fm.TailW1)
		}
	}

	// 躯干 / 胸 / 颈
	pm.ellipse(fm.BodyCX, fm.BodyCY, fm.BodyRX, fm.BodyRY, -0.05)
	pm.ellipse(fm.ChestX, fm.ChestY, fm.ChestR, fm.ChestR*0.95, 0)
	pm.bar(fm.ChestX+8, fm.ChestY+6, fm.HeadX-10, fm.HeadY-6, fm.NeckW)
	if fm.Mane {
		pm.ellipse(fm.ChestX+4, fm.ChestY+18, fm.ChestR*1.05, fm.ChestR*0.8, 0.3)
	}
	// 背鬃(狼):沿背脊一排三角刺
	for i := 0; i < fm.Ridge; i++ {
		u := (float64(i) + 0.5) / float64(fm.Ridge)
		x := fm.BodyCX - fm.BodyRX*0.85 + u*fm.BodyRX*1.7
		y := fm.BodyCY + fm.BodyRY - 2
		pm.poly([][2]float64{{x - 6, y}, {x - 2 + 2*sw, y + fm.RidgeH}, {x + 5, y}})
	}
	// 腹部浅色
	if fm.Belly {
		pl.ellipse(fm.BodyCX, fm.BodyCY-fm.BodyRY*0.62, fm.BodyRX*0.8, fm.BodyRY*0.42, -0.04)
	}

	// 头 + 吻
	hx, hy := fm.HeadX, fm.HeadY
	ph.ellipse(hx, hy, fm.HeadRX, fm.HeadRY, 0.12)
	mx := hx + fm.HeadRX*0.4
	ph.poly([][2]float64{{mx, hy + 3}, {mx + fm.MuzzleLen, hy - 4}, {mx + fm.MuzzleLen, hy - 4 - fm.MuzzleH}, {mx - 2, hy - fm.HeadRY*0.55}})
	// 耳
	ex := hx - fm.HeadRX*0.55
	ey := hy + fm.HeadRY*0.7
	ph.poly([][2]float64{{ex - fm.EarW*0.5, ey}, {ex + 2, ey + fm.EarH}, {ex + fm.EarW, ey + 2}})
	ph.poly([][2]float64{{ex + fm.EarSpread - 2, ey + 2}, {ex + fm.EarSpread + 6, ey + fm.EarH}, {ex + fm.EarSpread + fm.EarW, ey}})

	// 嘴(张开度随 open)
	jaw := ps.open * 9
	pk.poly([][2]float64{{mx, hy - 4}, {mx + fm.MuzzleLen, hy - 6 + jaw*0.4}, {mx + fm.MuzzleLen, hy - 10 - jaw}, {mx - 2, hy - 10}})
	// 獠牙
	fx := mx + fm.MuzzleLen*0.45
	pa.poly([][2]float64{{fx, hy - 6}, {fx + 4, hy - 14 - jaw*0.6}, {fx + 7, hy - 6}})
	pa.poly([][2]float64{{fx + 6, hy - 6 + jaw*0.3}, {fx + 3, hy + 1 + jaw*0.7}, {fx, hy - 6 + jaw*0.3}})

	// 近侧两腿(亮色,在最前)
	pf.bar(fm.LegBack-3*sw, gy, fm.LegBack+4, hip, fm.LegW+2)
	pf.bar(fm.LegFront+3*sw+ps.open*4, gy, fm.LegFront-2, hip, fm.LegW+2)
	// 爪
	cx := fm.LegFront + 6
	pa.poly([][2]float64{{cx + ps.open*6, gy + 5}, {cx + 8 + ps.open*10, gy - 1}, {cx + 1 + ps.open*6, gy + 11}})

	b.eyes = [][2]float64{t.pt(hx+fm.HeadRX*0.35, hy+fm.HeadRY*0.3)}
	b.eyeR = 4.6
}

// humanForm 人形模板的形体参数(模型空间:脚底原点,x 向前,y 向上)。
// 山鬼 vs 山贼以前同一副骨架只换色(剪影 IoU 0.80);现在体型(矮胖佝偻 vs 高瘦直立)、
// 头饰(双角 vs 头巾飘带)、武器(高举骨棒 vs 平持单刀)、下装(裸腿 vs 短裙)各自独立。
type humanForm struct {
	LegLen, LegW       float64 // 腿长 / 腿粗
	LegFront, LegBack  float64 // 双脚落点 x(站距)
	TorsoCY            float64 // 躯干中心高
	TorsoRX, TorsoRY   float64 // 躯干半轴
	Hunch              float64 // 佝偻前倾(躯干椭圆倾角,弧度)
	BellyR             float64 // 肚皮浅色块半径(0 = 无)
	NeckLen            float64 // 脖长
	HeadCY             float64 // 头中心高
	HeadR              float64 // 头半径
	Horns              bool    // 双角
	HornLen            float64
	Headband           bool    // 头巾 + 飘带
	Helmet             bool    // 兜鍪(阴兵)
	Skirt              bool    // 短裙 / 战裙
	SkirtTop, SkirtHem float64 // 裙腰高 / 裙摆高
	SkirtW             float64 // 裙摆半宽
	Weapon             string  // club(骨棒,高举)/ saber(单刀,平持)/ spear(长枪,竖持)
	Shoulders          bool    // 护肩(阴兵)
	EyeSpread          float64 // 双眼间距
}

// humanFormOf 按怪 id 取形体参数;未列出的人形怪走山贼形。
func humanFormOf(sp monsterSpec) humanForm {
	bandit := humanForm{
		LegLen: 58, LegW: 14, LegFront: 12, LegBack: -10,
		TorsoCY: 98, TorsoRX: 23, TorsoRY: 30, Hunch: 0.02, BellyR: 0,
		NeckLen: 10, HeadCY: 150, HeadR: 21,
		Headband: true, Skirt: true, SkirtTop: 78, SkirtHem: 52, SkirtW: 30,
		Weapon: "saber", EyeSpread: 15,
	}
	switch sp.Name {
	case "山鬼":
		// 山鬼:矮胖佝偻、大头、双角、高举骨棒、裸腿宽站
		return humanForm{
			LegLen: 34, LegW: 17, LegFront: 20, LegBack: -20,
			TorsoCY: 62, TorsoRX: 38, TorsoRY: 28, Hunch: 0.18, BellyR: 22,
			NeckLen: 4, HeadCY: 110, HeadR: 30,
			Horns: true, HornLen: 34,
			Weapon: "club", EyeSpread: 20,
		}
	case "阴兵":
		f := bandit
		f.Helmet, f.Shoulders, f.Headband = true, true, false
		f.TorsoRX, f.TorsoRY = 27, 32
		f.Weapon = "spear"
		return f
	}
	return bandit
}

// buildHumanoid 人形:山鬼(骨棒)/ 山贼(单刀)/ 阴兵(长枪),形体由 humanFormOf 给。
func buildHumanoid(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	ph := pen{b.head, t}
	pf := pen{b.front, t}
	pa := pen{b.accent, t}

	fm := humanFormOf(sp)
	sw := math.Sin(2 * math.Pi * ps.limb)
	hip := fm.LegLen
	shoulderY := fm.TorsoCY + fm.TorsoRY*0.75
	shoulderX := 6.0
	swing := ps.open * 0.55

	// 远侧腿 + 后臂
	pb.bar(fm.LegBack-4, gy, fm.LegBack-2+2*sw, hip, fm.LegW)
	pb.bar(shoulderX-14, shoulderY, shoulderX-32, shoulderY-26-4*sw, fm.LegW-3)

	// 躯干(Hunch 让上身前倾)
	pm.ellipse(0, fm.TorsoCY, fm.TorsoRX, fm.TorsoRY, fm.Hunch)
	pm.bar(shoulderX-6, shoulderY, shoulderX+2, shoulderY+fm.NeckLen, 18) // 脖子
	if fm.BellyR > 0 {
		pl.ellipse(6, fm.TorsoCY-6, fm.BellyR, fm.BellyR*0.72, 0.05)
	} else {
		pl.ellipse(2, fm.TorsoCY-10, fm.TorsoRX*0.72, fm.TorsoRY*0.42, 0.05)
	}
	// 短裙 / 战裙:梯形,盖住大腿根
	if fm.Skirt {
		pm.poly([][2]float64{{-fm.TorsoRX + 2, fm.SkirtTop}, {fm.TorsoRX + 2, fm.SkirtTop},
			{fm.SkirtW + 4, fm.SkirtHem}, {-fm.SkirtW, fm.SkirtHem}})
	}

	// 头 + 头饰
	hx, hy, hr := shoulderX, fm.HeadCY, fm.HeadR
	ph.ellipse(hx, hy, hr, hr*0.96, 0)
	if fm.Helmet {
		ph.poly([][2]float64{{hx - hr - 4, hy + 2}, {hx - hr + 2, hy + hr*0.9}, {hx, hy + hr + 16}, {hx + hr - 2, hy + hr*0.9}, {hx + hr + 4, hy + 2}})
		pa.poly([][2]float64{{hx - 4, hy + hr + 10}, {hx, hy + hr + 30}, {hx + 4, hy + hr + 10}})
	} else {
		// 头发 / 头巾块
		ph.poly([][2]float64{{hx - hr, hy + hr*0.25}, {hx - hr + 4, hy + hr*0.95}, {hx + 6, hy + hr*1.1}, {hx + hr - 2, hy + hr*0.7}, {hx + hr, hy + hr*0.15}})
	}
	if fm.Horns {
		// 双角:朝上外弯的粗刺
		pa.taper([][2]float64{{hx - 16, hy + hr*0.7}, {hx - 24, hy + hr*0.7 + fm.HornLen*0.55}, {hx - 20, hy + hr*0.7 + fm.HornLen}}, 11, 3)
		pa.taper([][2]float64{{hx + 14, hy + hr*0.7}, {hx + 26, hy + hr*0.7 + fm.HornLen*0.55}, {hx + 30, hy + hr*0.7 + fm.HornLen}}, 11, 3)
		// 尖耳
		ph.poly([][2]float64{{hx - hr + 2, hy}, {hx - hr - 16, hy + 8}, {hx - hr + 4, hy + 12}})
	}
	if fm.Headband {
		// 头巾:一条带 + 两根向后飘的结带
		pa.bar(hx-hr, hy+hr*0.55, hx+hr, hy+hr*0.55, 7)
		pa.taper([][2]float64{{hx - hr + 2, hy + hr*0.55}, {hx - hr - 18, hy + hr*0.5 + 4*sw}, {hx - hr - 40, hy + hr*0.2}}, 7, 2)
		pa.taper([][2]float64{{hx - hr + 2, hy + hr*0.45}, {hx - hr - 16, hy + hr*0.1 - 4*sw}, {hx - hr - 34, hy - hr*0.5}}, 6, 2)
	}
	if fm.Shoulders {
		pa.ellipse(shoulderX-20, shoulderY+4, 13, 8, 0.3)
		pa.ellipse(shoulderX+20, shoulderY+4, 13, 8, -0.3)
	}

	// 近侧腿
	pf.bar(fm.LegFront, gy, fm.LegFront+2-2*sw, hip+2, fm.LegW)

	// 前臂 + 武器(都绕肩点 (shoulderX, shoulderY) 随 swing 前压)
	px, py := shoulderX, shoulderY
	switch fm.Weapon {
	case "club":
		// 骨棒高举过头,attack 时抡下来
		hand := rotAbout([][2]float64{{px + 26, py + 22}}, px, py, swing*1.6)[0]
		pf.bar(px, py, hand[0], hand[1], 15)
		club := rotAbout([][2]float64{{px + 26, py + 22}, {px + 46, py + 70}}, px, py, swing*1.6)
		knob := rotAbout([][2]float64{{px + 50, py + 78}}, px, py, swing*1.6)[0]
		pa.bar(club[0][0], club[0][1], club[1][0], club[1][1], 12)
		pa.ellipse(knob[0], knob[1], 15, 13, 0.2)
	case "spear":
		// 长枪竖持,attack 时向前刺
		hand := rotAbout([][2]float64{{px + 24, py - 8}}, px, py, swing)[0]
		pf.bar(px, py, hand[0], hand[1], 13)
		sp := rotAbout([][2]float64{{px + 28, py - 40}, {px + 28, py + 84}}, px, py, swing)
		pa.bar(sp[0][0], sp[0][1], sp[1][0], sp[1][1], 6)
		tip := rotAbout([][2]float64{{px + 24, py + 82}, {px + 28, py + 100}, {px + 32, py + 82}}, px, py, swing)
		pa.poly(tip)
	default: // saber
		// 单刀平持在腰侧偏前,刀身长而薄;attack 时整臂前劈
		hand := rotAbout([][2]float64{{px + 30, py - 22}}, px, py, swing)[0]
		pf.bar(px, py, hand[0], hand[1], 13)
		blade := rotAbout([][2]float64{{px + 30, py - 18}, {px + 96, py - 2}, {px + 100, py - 8}, {px + 34, py - 26}}, px, py, swing)
		pa.poly(blade)
		// 护手
		g := rotAbout([][2]float64{{px + 32, py - 22}}, px, py, swing)[0]
		pa.ellipse(g[0], g[1], 6, 9, 0)
		// 背后刀鞘
		pb.bar(-18, fm.TorsoCY-8, -40, fm.TorsoCY-40, 7)
	}

	eyeY := hy + 5.0
	b.eyes = [][2]float64{t.pt(hx-fm.EyeSpread*0.4, eyeY), t.pt(hx+fm.EyeSpread*0.6, eyeY+2)}
	b.eyeR = 4.0
	if fm.Horns {
		b.eyeR = 5.0
	}
}

// buildGolem 器物/岩石:石灵 / 石魔 / 山神像。
func buildGolem(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	ph := pen{b.head, t}
	pf := pen{b.front, t}
	pa := pen{b.accent, t}

	lift := ps.open * 14 // 抬臂

	// 远侧臂
	pb.poly([][2]float64{{-44, 82 + lift*0.4}, {-66, 86 + lift}, {-72, 36 + lift}, {-50, 32}})
	// 基座 + 躯干
	pm.poly([][2]float64{{-44, gy}, {44, gy}, {36, 36}, {-38, 36}})
	pm.poly([][2]float64{{-40, 32}, {42, 34}, {48, 94}, {-44, 90}})
	// 胸口浅色岩板
	pl.poly([][2]float64{{-22, 46}, {24, 48}, {28, 78}, {-26, 76}})
	// 头
	ph.poly([][2]float64{{-22, 90}, {24, 92}, {28, 126}, {-26, 122}})
	// 近侧臂
	pf.poly([][2]float64{{42, 82 + lift*0.4}, {64, 86 + lift}, {70, 34 + lift}, {48, 30}})
	// 青苔 / 熔纹
	pa.ellipse(-18, 74, 15, 8, -0.2)
	pa.ellipse(13, 90, 12, 6, 0.15)
	pa.ellipse(-28, 44, 11, 5, 0.1)
	pa.ellipse(20, 52, 10, 5, -0.1)
	pa.ellipse(-4, 128, 14, 5, 0.05)

	b.eyes = [][2]float64{t.pt(-8, 108), t.pt(13, 109)}
	b.eyeR = 4.6
}

// buildSerpent 蛇形:蛇妖 / 铁甲蟒。
func buildSerpent(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	ph := pen{b.head, t}
	pa := pen{b.accent, t}
	pk := pen{b.mouth, t}

	sw := math.Sin(2 * math.Pi * ps.limb)

	// 颈后的颈冠(远层)
	pb.ellipse(40, 130, 30, 26, 0.2)
	// 三段盘身(最底一圈的下沿对齐 gy,盘身即"落地"部分)
	pm.ellipse(-8, 24.5, 52, 19, 0.03)
	pm.ellipse(2, 46, 44, 17, -0.04)
	pm.ellipse(8, 68, 34, 14, 0.05)
	// 颈
	pm.taper([][2]float64{{10, 80}, {22 + 3*sw, 106}, {42, 130}}, 26, 20)
	// 腹鳞
	pl.ellipse(-8, 12, 44, 7, 0.02)
	pl.ellipse(2, 36, 36, 6, -0.03)
	pl.ellipse(8, 58, 28, 5, 0.04)
	// 头
	ph.ellipse(50, 138, 24, 17, 0.22)
	jaw := ps.open * 10
	ph.poly([][2]float64{{62, 144}, {84, 134}, {82, 126 - jaw*0.3}, {58, 128}})
	// 张口
	pk.poly([][2]float64{{62, 140}, {86, 133}, {86, 127 - jaw}, {60, 130}})
	// 信子 + 毒牙
	pa.taper([][2]float64{{84, 132}, {92, 128}}, 4, 2)
	pa.poly([][2]float64{{74, 130}, {78, 120 - jaw*0.4}, {80, 130}})

	b.eyes = [][2]float64{t.pt(57, 145)}
	b.eyeR = 4.4
}

// buildSpirit 灵体:幽魂 / 怨灵(漂浮、下摆化雾)。
func buildSpirit(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	pf := pen{b.front, t}
	pa := pen{b.accent, t}

	sw := math.Sin(2 * math.Pi * ps.limb)

	// 下摆雾尾
	pb.poly([][2]float64{{-30, 74}, {30, 74}, {22, 40 + 4*sw}, {8, 16}, {-6, 38}, {-18, 14 - 4*sw}, {-26, 42}})
	// 身
	pm.ellipse(0, 104, 38, 42, 0.03)
	pl.ellipse(2, 88, 26, 18, 0)
	// 双臂(袖状)
	pb.taper([][2]float64{{-28, 106}, {-44, 92 + 4*sw}, {-52, 74}}, 14, 7)
	pf.taper([][2]float64{{26, 106}, {44 + ps.open*8, 92 - 4*sw}, {52 + ps.open*12, 76}}, 14, 7)
	// 头顶灵焰
	pa.poly([][2]float64{{-8, 142}, {2, 168}, {12, 142}})
	pa.ellipse(0, 130, 20, 8, 0)

	b.eyes = [][2]float64{t.pt(-12, 112), t.pt(15, 112)}
	b.eyeR = 5.4
}

// buildInsect 虫形:毒蜂 / 蚀骨虫。
func buildInsect(b *monBody, sp monsterSpec, ps pose, t xf) {
	pb := pen{b.back, t}
	pm := pen{b.body, t}
	pl := pen{b.belly, t}
	ph := pen{b.head, t}
	pf := pen{b.front, t}
	pa := pen{b.accent, t}

	sw := math.Sin(2 * math.Pi * ps.limb)

	// 远侧三足
	pb.bar(-40, gy, -26+2*sw, 44, 6)
	pb.bar(-16, gy, -4, 46, 6)
	pb.bar(8, gy, 20-2*sw, 44, 6)
	// 尾针 / 尾节
	pa.poly([][2]float64{{-66, 58}, {-88, 48}, {-64, 44}})
	// 腹节 + 胸
	pm.ellipse(-40, 58, 32, 25, 0.10)
	pm.ellipse(-2, 62, 26, 22, 0)
	pl.ellipse(-40, 44, 24, 9, 0.08)
	// 头
	ph.ellipse(30, 66, 20, 17, 0)
	// 螯钳(随 open 张开)
	sp1 := ps.open * 7
	pa.poly([][2]float64{{44, 62 + sp1}, {66, 54 + sp1*1.6}, {66, 48 + sp1*1.6}, {42, 54 + sp1}})
	pa.poly([][2]float64{{44, 70 - sp1}, {66, 76 - sp1*1.6}, {66, 70 - sp1*1.6}, {42, 64 - sp1}})
	// 触角
	pa.taper([][2]float64{{34, 80}, {50, 100 + 4*sw}, {62, 112}}, 5, 2)
	pa.taper([][2]float64{{28, 82}, {38, 104}, {44, 120 - 4*sw}}, 5, 2)
	// 近侧三足
	pf.bar(-30, gy, -18-2*sw, 40, 7)
	pf.bar(-6, gy, 6, 42, 7)
	pf.bar(18, gy, 30+2*sw, 40, 7)

	b.eyes = [][2]float64{t.pt(36, 72), t.pt(29, 60)}
	b.eyeR = 4.0
}

// ---------------------------------------------------------------------------
// 着色 / 合成
// ---------------------------------------------------------------------------

// maskShift 把遮罩整体平移(dy>0 = 向下),用来取顶缘亮边 / 底缘暗部。
func maskShift(m *Mask, dx, dy int) *Mask {
	out := NewMask(m.W, m.H)
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			out.A[y*out.W+x] = m.At(x-dx, y-dy)
		}
	}
	return out
}

func maskBounds(m *Mask) (minY, maxY float64, empty bool) {
	lo, hi := m.H, -1
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			if m.A[y*m.W+x] > 0.02 {
				if y < lo {
					lo = y
				}
				if y > hi {
					hi = y
				}
				break
			}
		}
	}
	if hi < 0 {
		return 0, 0, true
	}
	return float64(lo), float64(hi), false
}

// renderMonsterFrame 画一帧(E 向)。
func renderMonsterFrame(sp monsterSpec, action string, f, size int) *image.NRGBA {
	l := NewLayer(size, size)
	floaty := floatyTemplate(sp.Template)
	ps := poseFor(action, f, floaty)

	fit := fitOf(sp, size)
	mk := func(p pose) xf {
		return xf{ox: fit.OX + p.dx, oy: fit.OY + p.dy, sx: fit.Scale, sy: fit.Scale * p.scaleY, rot: p.lean}
	}

	ink := mixRGB(sp.Dark.Mul(0.32), RGB{0.04, 0.03, 0.05}, 0.55)

	// 1) 拖影:取前 1~2 帧的剪影,加法叠一层主色残影
	if ps.ghost > 0.01 {
		for k := 1; k <= 2 && f-k >= 0; k++ {
			gp := poseFor(action, f-k, floaty)
			gb := buildBody(sp, gp, mk(gp), size)
			l.FillAdd(gb.union(), sp.Main.Mul(0.75), 0.16*ps.ghost/float64(k))
		}
	}

	t := mk(ps)
	b := buildBody(sp, ps, t, size)
	u := b.union()
	topY, botY, empty := maskBounds(u)
	if empty {
		return l.ToNRGBA()
	}

	// 2) 厚描边
	l.FillFlat(u.Dilate(monOutline), ink, 1.0)

	// 3) 分部位填充:竖向渐变(上亮下暗)+ 轻微斑驳
	shadeOf := func(base RGB) func(x, y int) (RGB, float64) {
		return func(x, y int) (RGB, float64) {
			k := clamp01((float64(y) - topY) / math.Max(botY-topY, 1))
			c := mixRGB(base.Mul(1.22), base.Mul(0.52), k)
			n := 0.955 + 0.09*noise2(float64(x)*0.21, float64(y)*0.19)
			c = c.Mul(n)
			if ps.flash > 0 {
				// 只提到 0.62 上限:保住分部位明暗差,受击帧仍看得出是谁
				c = mixRGB(c, RGB{1, 1, 1}, ps.flash*0.62)
			}
			return c, 1
		}
	}
	type layerJob struct {
		m       *Mask
		col     RGB
		inedge  bool // 是否先描一圈内轮廓(与身后部位分开)
		addGlow float64
	}
	jobs := []layerJob{
		{b.back, sp.Dark, false, 0},
		{b.body, sp.Main, false, 0},
		{b.belly, sp.Belly, false, 0},
		{b.head, sp.Main, true, 0},
		{b.mouth, mixRGB(ink, sp.Dark, 0.35), false, 0},
		{b.front, mixRGB(sp.Main, RGB{1, 1, 1}, 0.10), true, 0},
		{b.accent, sp.Accent, true, 0.10},
	}
	for _, j := range jobs {
		if _, _, e := maskBounds(j.m); e {
			continue
		}
		if j.inedge {
			l.FillFlat(j.m.Dilate(1.7).Subtract(j.m), ink, 0.72)
		}
		l.FillOver(j.m, shadeOf(j.col))
		if j.addGlow > 0 {
			l.FillAdd(j.m, j.col, j.addGlow)
		}
	}

	// 4) 顶缘亮边 / 底缘压深(45° 俯视的受光方向:上方)
	rim := u.Subtract(maskShift(u, 0, 3))
	l.FillAdd(rim, mixRGB(sp.Main, RGB{1, 1, 1}, 0.72), 0.30)
	deep := u.Subtract(maskShift(u, 0, -4))
	l.FillFlat(deep, ink, 0.26)

	// 5) 眼睛:暗窝 + 亮芯 + 外发光 + 高光点
	for _, e := range b.eyes {
		r := b.eyeR
		socket := NewMask(size, size)
		socket.Circle(e[0], e[1], r*1.55)
		l.FillFlat(socket, ink, 0.92)
		core := NewMask(size, size)
		core.Circle(e[0], e[1], r)
		l.FillFlat(core, mixRGB(sp.Eye, RGB{1, 1, 1}, 0.25), 1.0)
		l.AddDot(e[0], e[1], r*2.9, sp.Eye, 0.50, 2.0)
		l.AddDot(e[0]-r*0.32, e[1]-r*0.36, r*0.52, RGB{1, 1, 1}, 0.95, 1.1)
	}

	// 6) 受击闪白:整体加一层白(叠加量同样收着给,厚描边要留得住)
	if ps.flash > 0 {
		l.FillAdd(u, RGB{1, 1, 1}, ps.flash*0.20)
	}
	return l.ToNRGBA()
}

// mirrorCells 把横条按格水平镜像(帧序不变)→ W 向。
func mirrorCells(src *image.NRGBA, cell int) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	n := b.Dx() / cell
	for c := 0; c < n; c++ {
		x0 := c * cell
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < cell; x++ {
				so := src.PixOffset(x0+x, y)
				do := dst.PixOffset(x0+cell-1-x, y)
				copy(dst.Pix[do:do+4], src.Pix[so:so+4])
			}
		}
	}
	return dst
}

// ---------------------------------------------------------------------------
// 产出 / 清单 / 自检
// ---------------------------------------------------------------------------

type monsterActionMeta struct {
	Action string    `json:"action"`
	Frames int       `json:"frames"`
	FPS    int       `json:"fps_hint"`
	Cell   int       `json:"cell"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
	Pivot  []float64 `json:"pivot"`
	Files  []string  `json:"files"`
	Desc   string    `json:"desc"`
}

type monsterMeta struct {
	ID        uint32              `json:"id"`
	Name      string              `json:"name"`
	Template  string              `json:"template"`
	MainColor string              `json:"main_color"`
	EyeColor  string              `json:"eye_color"`
	Scale     float64             `json:"scale_requested"` // 花名册里写的缩放
	Fit       monFit              `json:"fit"`             // 装箱后实际用的缩放 / 落笔基点
	HoverPx   float64             `json:"hover_px"`        // 离地高度(只有灵体 > 0)
	Note      string              `json:"note"`
	NameFrom  string              `json:"name_source"`
	Actions   []monsterActionMeta `json:"actions"`
}

// MonsterManifest = Battle/MONSTER_MANIFEST.json。
type MonsterManifest struct {
	Tool        string         `json:"tool"`
	Mode        string         `json:"mode"`
	GeneratedAt string         `json:"generated_at"`
	Spec        string         `json:"spec"`
	Cell        int            `json:"cell"`
	Frames      int            `json:"frames"`
	FeetMargin  float64        `json:"feet_margin_px"`
	Pivot       []float64      `json:"pivot_hint"`
	Notes       []string       `json:"notes"`
	Monsters    []monsterMeta  `json:"monsters"`
	Ground      []ArtEntry     `json:"ground_bands"`
	Verify      map[string]any `json:"verify"`
}

func hexOf(c RGB) string {
	return fmt.Sprintf("#%02X%02X%02X",
		int(clamp01(c.R)*255+0.5), int(clamp01(c.G)*255+0.5), int(clamp01(c.B)*255+0.5))
}

// runMonsterStage 由 main.go 调用。返回 done=true 表示 -mode monsters,主流程不再跑其余产物。
func runMonsterStage(outRoot string) (done bool, err error) {
	switch genMode {
	case "fx":
		return false, nil
	case "all", "monsters":
	default:
		return false, fmt.Errorf("未知 -mode %q(可选 all / fx / monsters)", genMode)
	}

	n := monsterCount
	if n < 1 {
		n = 1
	}
	if n > len(monsterRoster) {
		n = len(monsterRoster)
	}

	mm := &MonsterManifest{
		Tool:        "tools/battle_art_gen -mode monsters",
		Mode:        genMode,
		GeneratedAt: time.Now().Format(time.RFC3339),
		Spec:        "docs/design/battle-art-prompts.md §2.2/§3;turn-battle-presentation.md §1/§5;BattleArtCatalog(权威)",
		Cell:        monFrameSize,
		Frames:      monFrames,
		FeetMargin:  monFeetPx,
		Pivot:       []float64{0.5, 0.08},
	}

	monRoot := filepath.Join(outRoot, "Monsters")
	if err := os.MkdirAll(monRoot, 0o755); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Join(outRoot, "UI"), 0o755); err != nil {
		return false, err
	}

	stripW := monFrameSize * monFrames
	files := 0
	for _, sp := range monsterRoster[:n] {
		dir := filepath.Join(monRoot, fmt.Sprintf("%d", sp.ID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		meta := monsterMeta{
			ID: sp.ID, Name: sp.Name, Template: sp.Template,
			MainColor: hexOf(sp.Main), EyeColor: hexOf(sp.Eye), Scale: sp.Scale,
			Fit: fitOf(sp, monFrameSize), HoverPx: monHoverPx(sp.Template),
			Note: sp.Note, NameFrom: sp.Source,
		}
		for _, act := range monActions {
			east := image.NewNRGBA(image.Rect(0, 0, stripW, monFrameSize))
			for f := 0; f < monFrames; f++ {
				frame := renderMonsterFrame(sp, act.Name, f, monFrameSize)
				draw.Draw(east, image.Rect(f*monFrameSize, 0, (f+1)*monFrameSize, monFrameSize),
					frame, image.Point{}, draw.Src)
			}
			ef := fmt.Sprintf("%s_E_strip.png", act.Name)
			wf := fmt.Sprintf("%s_W_strip.png", act.Name)
			if err := savePNG(filepath.Join(dir, ef), east); err != nil {
				return false, err
			}
			files++
			outFiles := []string{ef}
			if monWantsDir("W") {
				if err := savePNG(filepath.Join(dir, wf), mirrorCells(east, monFrameSize)); err != nil {
					return false, err
				}
				files++
				outFiles = append(outFiles, wf)
			} else {
				// 默认不落盘 W(客户端运行时镜像);清掉旧产物残留的 W 及其 .meta,避免客户端优先命中陈旧 W
				for _, stale := range []string{filepath.Join(dir, wf), filepath.Join(dir, wf+".meta")} {
					if err := os.Remove(stale); err == nil {
						fmt.Printf("    已删除陈旧的 %s\n", filepath.ToSlash(stale))
					}
				}
			}
			meta.Actions = append(meta.Actions, monsterActionMeta{
				Action: act.Name, Frames: monFrames, FPS: act.FPS, Cell: monFrameSize,
				Width: stripW, Height: monFrameSize, Pivot: []float64{0.5, 0.08},
				Files: outFiles, Desc: act.Desc,
			})
		}
		if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
			return false, err
		}
		mm.Monsters = append(mm.Monsters, meta)
		fmt.Printf("  怪物 %2d %-6s %-9s %d 动作 × %d 向 × %d 帧(%d×%d)\n",
			sp.ID, sp.Name, sp.Template, len(monActions), len(monsterDirs), monFrames, stripW, monFrameSize)
	}

	ground, err := genGroundBands(outRoot)
	if err != nil {
		return false, err
	}
	mm.Ground = ground

	mm.Notes = []string{
		fmt.Sprintf("帧条 = %d 帧 × %d×%d → %d×%d;客户端 BattleArtCatalog.LoadStrip 以贴图高为格宽自动切帧,不依赖 Sprite 导入设置。",
			monFrames, monFrameSize, monFrameSize, stripW, monFrameSize),
		fmt.Sprintf("脚底(含 %.1fpx 厚描边)钉在第 %d 行 = 底边上方 %.0fpx = BattleArtCatalog.FeetPivot (0.5, 0.08) × %d,"+
			"与 Battle/Characters 的角色帧条同一基线,直接用 FeetPivot 贴不用改任何客户端常量。"+
			"(battle-art-prompts.md 里写的 8px 与该 pivot 对不上,以 pivot 为准。)",
			monOutline, monBaselineRow, monFeetPx, monFrameSize),
		fmt.Sprintf("灵体模板(spirit)整体抬高 %.0fpx 表示漂浮;其余模板脚底落在基线上。", monHoverPx("spirit")),
		"每只怪的 (scale, origin_x, origin_y) 是自动量出来的(见 meta.json 的 fit):先把 idle 首帧脚底钉到基线," +
			"再检查 3 动作 × 8 帧的总包围盒有没有出框,出框就等比缩小 / 横向平移。所以同一只怪所有帧共用一组落笔参数,动画位移不受影响。",
		monDirsNote(),
		fmt.Sprintf("同模板的怪形体各不相同(beastForm / humanForm 按怪逐只给体长比、腿长、头径、耳/角/尾/武器模块),"+
			"不只是换色;自检断言任意两只 idle 首帧归一化剪影 IoU ≤ %.2f(24×32 重采样),受击闪白或色弱也分得清。", monMaxSilhouetteIoU),
		"idle 为整周期采样(相位 = 2π·f/8),首尾无缝循环;attack 命中帧 = 第 5 帧(0 基 index 4);hit 前两帧闪白最强。",
		"Monster 表(generated/tables/Monster.json)只有 id + 战斗数值,没有 name / level 列;名字取自 battle-art-prompts.md §2.2 首批清单,按表内 id 升序一一对应。",
		fmt.Sprintf("ground_band_* 是羽化椭圆光带,**不能九宫**(边缘羽化会被九宫切开),用 Image.Type = Simple 直接拉伸;"+
			"推荐尺寸 %.0f×%.0f 设计像素(2560×1080 设计坐标),此时贴图里烘的 %.2f° 正好等于 BattleStage.RowStep 的斜率,RectTransform 不用旋转。",
			groundFitW, groundFitH, groundTiltDeg),
		"地台摆放锚点(按 BattleStage 常量算出的 10 个槽位包围盒中心,2560×1080 设计坐标,y 向下):" +
			"ground_band_far 放敌方 (1310, 499)(槽位跨度 890×204)、ground_band_near 放我方 (1350, 766)(910×174);" +
			"sizeDelta 都用 1120×340,siblingIndex 排在竞技场背景之后、全部单位之前。",
	}
	mpath := filepath.Join(outRoot, "MONSTER_MANIFEST.json")

	// 自检:回读磁盘上的 PNG
	vr, err := verifyMonsterOutput(outRoot, monsterRoster[:n])
	if err != nil {
		return false, err
	}
	mm.Verify = vr
	if err := writeJSON(mpath, mm); err != nil {
		return false, err
	}

	fmt.Printf("完成:怪物 %d 只 × %d 动作 × %d 向 = %d 张帧条,地台 %d 张,清单 %s\n",
		n, len(monActions), len(monsterDirs), files, len(ground), mpath)
	return genMode == "monsters", nil
}

// monDirsNote 写进 MONSTER_MANIFEST.json notes 的朝向说明。
func monDirsNote() string {
	if monWantsDir("W") {
		return "本次 -monster-dirs 含 W:W 向 = E 向逐格水平镜像(几何对称、受光方向为正上方,镜像不破坏明暗);自检做过逐像素零误差比对。"
	}
	return "只出 E 向;W 由客户端 BattleArtCatalog.LoadDirectionalStrip 缺图降级取 E 并置 Mirrored=true 运行时翻转。" +
		"落盘 W 只是 E 的逐格镜像副本(受光正上方,镜像不破坏明暗),画面零差异却让未压缩纹理体积翻倍,故默认不写。"
}
