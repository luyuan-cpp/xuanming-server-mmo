package main

// characters.go —— 玩家战斗精灵(-mode characters)
//
// 把 Assets/Resources/UI/qdao_v3/characters/*.png 的 22 张 1024 立绘,
// 变成战斗里能直接用的 256 单格 8 帧动作横条:
//
//	去背/裁边 → 归一化(脚底基线 + 居中 + 统一身高) → 逐帧几何/色彩变换 → 横条打包 → 默认只出 E 向
//
// 权威口径以客户端 BattleArtCatalog.cs 为准:
//   - LoadStrip(path, cellSize=0, FeetPivot, fps):cellSize=0 → 单格边长 = 贴图高,帧数 = 宽 / 格宽;
//     所以横条必须是 (N×H)×H 的方格条,本工具取 H=256、N=8 → 2048×256。
//   - FeetPivot = (0.5, 0.08):脚底基线在画布底边上方 0.08×256 ≈ 20px(不是 8px,见 README「与规格的偏差」)。
//   - LoadDirectionalStrip:缺 W 会用 E 镜像(BattleUnitView 按 Mirrored 翻 localScale.x)。
//     W 若由本工具出也只是 E 的逐格镜像副本,画面零差异但体积翻倍(每张 2048×256 未压缩 = 2 MiB 进包),
//     所以默认 -char-dirs=E 只出 E;需要非对称专有 W 向素材时再显式 -char-dirs E,W。
//
// 全部动作帧由**同一张底图**做几何(平移/旋转/错切/缩放)+ 色彩(亮度/闪白/拖影)+
// 程序化叠加(cast 的脚底光晕)生成,不做逐帧重绘。

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// charCell 单格边长。必须等于横条高度(BattleArtCatalog 用 tex.height 当格宽)。
	charCell = 256
	// charFrames 每个动作的帧数。
	charFrames = 8
	// charFeetFromBottom 脚底基线距画布底边的像素 = round(FeetPivot.y 0.08 × 256)。
	charFeetFromBottom = 20
)

// charBaselineRow 脚底所在的行号(图像坐标,y 向下);底图最低不透明像素必须落在这一行。
const charBaselineRow = charCell - 1 - charFeetFromBottom // 235

// charOptions 是 -mode characters 的全部可调参数(写进 CHARACTER_MANIFEST.json 便于复现)。
type charOptions struct {
	SrcDir  string   `json:"src_dir"`
	OutDir  string   `json:"out_dir"`
	Actions []string `json:"actions"`
	// Dirs 输出朝向(E / W)。默认只有 E:W 是 E 的逐格镜像,交给客户端运行时翻转。
	Dirs []string `json:"directions"`
	// MaxHeight 身高上限(px)。统一的是**身高**而不是长边:横向持矛/持剑的立绘包围盒宽 > 高,
	// 按长边归一会让它们比别人矮 10%,把后排 0.88 缩放的近大远小线索抵消掉。
	MaxHeight   int     `json:"max_height"`
	AlphaThres  int     `json:"alpha_threshold"`
	ChromaTol   float64 `json:"chroma_tolerance"`
	MinCompPct  float64 `json:"min_component_pct"`
	Supersample int     `json:"supersample"`
	// FittedHeight 是实际采用的统一身高(由 charFitHeight 求解,<= MaxHeight);写进 manifest 便于复现。
	FittedHeight int `json:"fitted_height"`
}

// charWantsDir 判断 opt.Dirs 里是否包含某个朝向(大小写不敏感)。
func charWantsDir(opt charOptions, dir string) bool {
	for _, d := range opt.Dirs {
		if strings.EqualFold(d, dir) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1. 去背 / 裁边
// ---------------------------------------------------------------------------

// charSource 是一张立绘的抠图与归一化结果。
type charSource struct {
	ID           string
	File         string
	SrcW         int
	SrcH         int
	Keying       string  // alpha | chroma
	ChromaRef    [3]int  // chroma 抠图时的参考色(四角中位)
	BBox         [4]int  // x, y, w, h(源图坐标)
	Scale        float64 // 裁剪框 → 画布的等比缩放(= height / bbox 高)
	Placed       [2]int  // 缩放后贴入画布的像素尺寸;Placed[1] == 统一身高
	DroppedComps int     // 被丢弃的小连通域个数(灰尘/噪点)
	Dominant     RGB     // 主色(cast 光晕着色 + manifest 记录)
	DominantU8   [3]int
	Base         *Layer // 256×256 预乘 alpha 底图
}

// charKeyed 是「解码 + 抠图 + 裁边」的中间结果(直通 alpha 缓冲 + 包围盒)。
// 单独拆出来是因为要先量出全部 22 张的包围盒,才能算出统一的、不会被画布裁到的缩放。
type charKeyed struct {
	ID             string
	File           string
	W, H           int
	R, G, B, A     []float64
	Keying         string
	ChromaRef      [3]int
	X0, Y0, X1, Y1 int
	Dropped        int
}

// keyCharImage 解码一张立绘并做抠图与主连通域裁边(不做缩放与置位)。
func keyCharImage(path string, opt charOptions) (*charKeyed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开失败: %w", err)
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("解码失败: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("尺寸非法 %dx%d", w, h)
	}

	// 直通 alpha 的 RGBA8 缓冲
	rr := make([]float64, w*h)
	gg := make([]float64, w*h)
	bb := make([]float64, w*h)
	aa := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cr, cg, cb, ca := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := y*w + x
			a := float64(ca) / 65535.0
			if a > 1e-6 {
				// RGBA() 返回预乘值,还原成直通色
				rr[i] = float64(cr) / 65535.0 / a
				gg[i] = float64(cg) / 65535.0 / a
				bb[i] = float64(cb) / 65535.0 / a
			}
			aa[i] = a
		}
	}

	k := &charKeyed{
		ID: charIDFromFile(filepath.Base(path)), File: filepath.ToSlash(path),
		W: w, H: h, R: rr, G: gg, B: bb, A: aa, Keying: "alpha",
	}

	// 判定:四边边框上不透明像素占比 > 50% 说明原图不带 alpha,改走 chroma 容差抠图
	border, opaqueBorder := 0, 0
	for x := 0; x < w; x++ {
		for _, y := range []int{0, h - 1} {
			border++
			if aa[y*w+x] > 0.5 {
				opaqueBorder++
			}
		}
	}
	for y := 0; y < h; y++ {
		for _, x := range []int{0, w - 1} {
			border++
			if aa[y*w+x] > 0.5 {
				opaqueBorder++
			}
		}
	}
	if border > 0 && float64(opaqueBorder)/float64(border) > 0.5 {
		k.Keying = "chroma"
		refR, refG, refB := charCornerRef(rr, gg, bb, w, h)
		k.ChromaRef = [3]int{int(refR*255 + 0.5), int(refG*255 + 0.5), int(refB*255 + 0.5)}
		for i := range aa {
			d := math.Sqrt((rr[i]-refR)*(rr[i]-refR) + (gg[i]-refG)*(gg[i]-refG) + (bb[i]-refB)*(bb[i]-refB))
			// 容差内全透明,1~2 倍容差之间线性过渡(留一圈软边)
			switch {
			case d <= opt.ChromaTol:
				aa[i] = 0
			case d <= opt.ChromaTol*2:
				aa[i] = (d - opt.ChromaTol) / opt.ChromaTol
			default:
				aa[i] = 1
			}
		}
	}

	// 主连通域:丢掉面积 < 最大域 MinCompPct 的碎片(扫描线噪点/孤立像素),
	// 但保留悬浮挂件(法宝/飘带)这类占比可观的独立块。
	thres := float64(opt.AlphaThres) / 255.0
	x0, y0, x1, y1, dropped, err := charBBox(aa, w, h, thres, opt.MinCompPct)
	if err != nil {
		return nil, err
	}
	k.X0, k.Y0, k.X1, k.Y1 = x0, y0, x1, y1
	k.Dropped = dropped

	// 把裁剪框外的像素直接清零,避免缩放时把碎片带进来
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < x0 || x >= x1 || y < y0 || y >= y1 {
				aa[y*w+x] = 0
			}
		}
	}
	return k, nil
}

// charShapeRows 求解统一缩放时,轮廓按多少行采样。行数越多越贴合,64 行对 1024 立绘足够。
const charShapeRows = 64

// charShape 是一张立绘的「归一化轮廓」:包围盒尺寸 + 若干轮廓点。
// 点用 (s, t) 表示,s = 相对包围盒宽的横向位置 [0,1],t = 相对包围盒高的纵向位置 [0,1](0 = 顶)。
// 用真实轮廓而不是包围盒矩形来判是否出画布,是因为立绘顶部两角基本是空的:
// 拿矩形角去判,hit 的后仰(rot -12° + 错切)会把一个空角甩出画布,把可用尺寸压掉三成。
type charShape struct {
	CW, CH int
	Pts    [][2]float64
}

// probeCharShape 量包围盒并逐行提取左右轮廓点(丢掉像素缓冲,只留几百个点)。
func probeCharShape(path string, opt charOptions) (charShape, error) {
	k, err := keyCharImage(path, opt)
	if err != nil {
		return charShape{}, err
	}
	x0, y0, x1, y1 := k.X0, k.Y0, k.X1, k.Y1
	cw, ch := x1-x0, y1-y0
	sh := charShape{CW: cw, CH: ch}
	thres := float64(opt.AlphaThres) / 255.0

	for r := 0; r < charShapeRows; r++ {
		ya := y0 + r*ch/charShapeRows
		yb := y0 + (r+1)*ch/charShapeRows
		if yb <= ya {
			yb = ya + 1
		}
		minX, maxX := x1, x0-1
		for y := ya; y < yb && y < y1; y++ {
			row := y * k.W
			for x := x0; x < x1; x++ {
				if k.A[row+x] >= thres {
					if x < minX {
						minX = x
					}
					if x > maxX {
						maxX = x
					}
				}
			}
		}
		if maxX < minX {
			continue // 该带全透明(例如头顶上方的空隙)
		}
		sMin := float64(minX-x0) / float64(cw)
		sMax := float64(maxX+1-x0) / float64(cw)
		tTop := float64(ya-y0) / float64(ch)
		tBot := float64(yb-y0) / float64(ch)
		// 每条带取四角,保证仿射变换后该带的极值被覆盖
		sh.Pts = append(sh.Pts,
			[2]float64{sMin, tTop}, [2]float64{sMax, tTop},
			[2]float64{sMin, tBot}, [2]float64{sMax, tBot})
	}
	if len(sh.Pts) == 0 { // 兜底:退回包围盒矩形
		sh.Pts = [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}}
	}
	return sh, nil
}

// loadCharSource 在抠图结果上做等比缩放 + 脚底对齐贴入 256 画布。
// height 是角色**身高**的目标像素(由 charFitHeight 统一求解,保证任何一帧都不会被画布裁到);
// 宽度只随等比缩放走,不单独约束——是否出画布已由 charFitsAt 用真实轮廓判过。
func loadCharSource(path string, opt charOptions, height int) (*charSource, error) {
	k, err := keyCharImage(path, opt)
	if err != nil {
		return nil, err
	}
	rr, gg, bb, aa := k.R, k.G, k.B, k.A
	x0, y0, x1, y1 := k.X0, k.Y0, k.X1, k.Y1
	cs := &charSource{
		ID: k.ID, File: k.File, SrcW: k.W, SrcH: k.H,
		Keying: k.Keying, ChromaRef: k.ChromaRef,
		BBox: [4]int{x0, y0, x1 - x0, y1 - y0}, DroppedComps: k.Dropped,
	}
	w := k.W

	cw, ch := x1-x0, y1-y0
	scale := charScaleForHeight(cw, ch, height)
	dw := maxInt(1, int(float64(cw)*scale+0.5))
	dh := maxInt(1, int(float64(ch)*scale+0.5))
	cs.Scale = scale
	cs.Placed = [2]int{dw, dh}

	// 预乘 → box 重采样(下采样时 box 均值最干净,不会像 Catmull-Rom 那样在硬边产生振铃)
	small := resampleBoxPremul(rr, gg, bb, aa, w, x0, y0, cw, ch, dw, dh)

	// 贴入 256 画布:水平居中,最低不透明行落在 charBaselineRow
	base := NewLayer(charCell, charCell)
	px := (charCell - dw) / 2
	py := charBaselineRow - dh + 1
	for y := 0; y < dh; y++ {
		ty := py + y
		if ty < 0 || ty >= charCell {
			continue
		}
		for x := 0; x < dw; x++ {
			tx := px + x
			if tx < 0 || tx >= charCell {
				continue
			}
			si := y*dw + x
			di := ty*charCell + tx
			base.R[di] = small.R[si]
			base.G[di] = small.G[si]
			base.B[di] = small.B[si]
			base.A[di] = small.A[si]
		}
	}
	cs.Base = base

	// 主色:对饱和度加权求均值(避免被黑白线稿拉灰)
	var sr, sg, sb, sw2 float64
	for i := range base.A {
		a := base.A[i]
		if a < 0.6 {
			continue
		}
		r, g, bl := base.R[i]/a, base.G[i]/a, base.B[i]/a
		mx := math.Max(r, math.Max(g, bl))
		mn := math.Min(r, math.Min(g, bl))
		sat := mx - mn
		wgt := a * (0.08 + sat)
		sr += r * wgt
		sg += g * wgt
		sb += bl * wgt
		sw2 += wgt
	}
	if sw2 > 0 {
		cs.Dominant = RGB{sr / sw2, sg / sw2, sb / sw2}
	} else {
		cs.Dominant = rgb8(200, 220, 255)
	}
	cs.DominantU8 = [3]int{
		int(clamp01(cs.Dominant.R)*255 + 0.5),
		int(clamp01(cs.Dominant.G)*255 + 0.5),
		int(clamp01(cs.Dominant.B)*255 + 0.5),
	}
	return cs, nil
}

// charCornerRef 取四角 8×8 区域的均值当 chroma 参考色。
func charCornerRef(rr, gg, bb []float64, w, h int) (float64, float64, float64) {
	var sr, sg, sb float64
	n := 0
	corners := [][2]int{{0, 0}, {w - 8, 0}, {0, h - 8}, {w - 8, h - 8}}
	for _, c := range corners {
		for y := c[1]; y < c[1]+8 && y < h; y++ {
			for x := c[0]; x < c[0]+8 && x < w; x++ {
				if x < 0 || y < 0 {
					continue
				}
				i := y*w + x
				sr += rr[i]
				sg += gg[i]
				sb += bb[i]
				n++
			}
		}
	}
	if n == 0 {
		return 1, 0, 1
	}
	return sr / float64(n), sg / float64(n), sb / float64(n)
}

// charBBox 对 alpha>=thres 的像素做 8 邻域连通域标记,保留面积 >= 最大域 minPct 的域,
// 返回它们的合并包围盒(x0,y0,x1,y1 半开)与被丢弃的域数。
func charBBox(aa []float64, w, h int, thres, minPct float64) (int, int, int, int, int, error) {
	label := make([]int32, w*h)
	for i := range label {
		label[i] = -1
	}
	type compInfo struct {
		area           int
		x0, y0, x1, y1 int
	}
	var comps []compInfo
	stack := make([]int32, 0, 4096)
	for start := 0; start < w*h; start++ {
		if label[start] != -1 || aa[start] < thres {
			continue
		}
		id := int32(len(comps))
		ci := compInfo{x0: w, y0: h, x1: 0, y1: 0}
		stack = stack[:0]
		stack = append(stack, int32(start))
		label[start] = id
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x := int(p) % w
			y := int(p) / w
			ci.area++
			if x < ci.x0 {
				ci.x0 = x
			}
			if y < ci.y0 {
				ci.y0 = y
			}
			if x+1 > ci.x1 {
				ci.x1 = x + 1
			}
			if y+1 > ci.y1 {
				ci.y1 = y + 1
			}
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					q := int32(ny*w + nx)
					if label[q] != -1 || aa[q] < thres {
						continue
					}
					label[q] = id
					stack = append(stack, q)
				}
			}
		}
		comps = append(comps, ci)
	}
	if len(comps) == 0 {
		return 0, 0, 0, 0, 0, fmt.Errorf("抠图后全透明(alpha 阈值 %.3f 太高?)", thres)
	}
	maxArea := 0
	for _, c := range comps {
		if c.area > maxArea {
			maxArea = c.area
		}
	}
	minArea := int(float64(maxArea) * minPct)
	if minArea < 32 {
		minArea = 32
	}
	x0, y0, x1, y1, dropped := w, h, 0, 0, 0
	for _, c := range comps {
		if c.area < minArea {
			dropped++
			continue
		}
		if c.x0 < x0 {
			x0 = c.x0
		}
		if c.y0 < y0 {
			y0 = c.y0
		}
		if c.x1 > x1 {
			x1 = c.x1
		}
		if c.y1 > y1 {
			y1 = c.y1
		}
	}
	if x1 <= x0 || y1 <= y0 {
		return 0, 0, 0, 0, dropped, fmt.Errorf("包围盒为空(连通域全部被丢弃)")
	}
	return x0, y0, x1, y1, dropped, nil
}

// resampleBoxPremul 在源图上按裁剪框做预乘 box 均值下采样,输出预乘 Layer。
func resampleBoxPremul(rr, gg, bb, aa []float64, srcW, cx, cy, cw, ch, dw, dh int) *Layer {
	out := NewLayer(dw, dh)
	sx := float64(cw) / float64(dw)
	sy := float64(ch) / float64(dh)
	for dy := 0; dy < dh; dy++ {
		fy0 := float64(dy) * sy
		fy1 := fy0 + sy
		iy0 := int(math.Floor(fy0))
		iy1 := int(math.Ceil(fy1))
		for dx := 0; dx < dw; dx++ {
			fx0 := float64(dx) * sx
			fx1 := fx0 + sx
			ix0 := int(math.Floor(fx0))
			ix1 := int(math.Ceil(fx1))
			var ar, ag, ab, al, wsum float64
			for y := iy0; y < iy1; y++ {
				wy := math.Min(float64(y+1), fy1) - math.Max(float64(y), fy0)
				if wy <= 0 || y+cy < 0 {
					continue
				}
				for x := ix0; x < ix1; x++ {
					wx := math.Min(float64(x+1), fx1) - math.Max(float64(x), fx0)
					if wx <= 0 {
						continue
					}
					si := (y+cy)*srcW + (x + cx)
					if si < 0 || si >= len(aa) {
						continue
					}
					wgt := wx * wy
					a := aa[si]
					ar += rr[si] * a * wgt
					ag += gg[si] * a * wgt
					ab += bb[si] * a * wgt
					al += a * wgt
					wsum += wgt
				}
			}
			if wsum <= 0 {
				continue
			}
			i := dy*dw + dx
			out.R[i] = ar / wsum
			out.G[i] = ag / wsum
			out.B[i] = ab / wsum
			out.A[i] = al / wsum
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. 动作定义(几何 + 色彩变换表)
// ---------------------------------------------------------------------------

// ghostDef 一层拖影(同一张底图的错位半透明副本,画在主体后面)。
type ghostDef struct {
	Dx, Dy float64
	RotDeg float64
	Sx, Sy float64
	Alpha  float64
	Tint   RGB
	TintK  float64
}

// frameDef 一帧的变换。Dx 为「朝向前方」为正(E 向即 +x),Dy 向下为正,
// 旋转/错切都以脚底锚点 (128, charBaselineRow) 为中心,保证脚不飘。
type frameDef struct {
	Dx, Dy float64
	RotDeg float64 // 正 = 上半身向前倾
	Sx, Sy float64
	Shear  float64 // 水平错切:正 = 头部相对脚底前移
	Flash  float64 // 向纯白插值(受击闪白)
	Bright float64 // 亮度乘子
	Alpha  float64
	Glow   float64 // cast 光晕强度 0..1
	Ghosts []ghostDef
}

func fd(dx, dy, rot, sx, sy, shear float64) frameDef {
	return frameDef{Dx: dx, Dy: dy, RotDeg: rot, Sx: sx, Sy: sy, Shear: shear, Bright: 1, Alpha: 1}
}

type charAction struct {
	Name   string
	FPS    int
	Desc   string
	Frames []frameDef
}

// buildCharActions 构造 6 套动作。全部 8 帧,与 charFrames 一致。
// FPS 取 BattleArtCatalog.ActionFps 的同一口径。
func buildCharActions() map[string]charAction {
	acts := map[string]charAction{}

	// idle:呼吸。整体上下 ±2px + 1.5% 纵向缩放脉动(横向反向 0.8% 做挤压),首尾无缝循环。
	idle := make([]frameDef, charFrames)
	for f := 0; f < charFrames; f++ {
		bob := math.Sin(2 * math.Pi * float64(f) / float64(charFrames))
		x := fd(0, -2*bob, 0.6*bob, 1-0.008*bob, 1+0.015*bob, 0.006*bob)
		idle[f] = x
	}
	acts["idle"] = charAction{"idle", 6, "站立呼吸:±2px 上下 + 1.5% 纵向脉动,8 帧无缝循环", idle}

	// attack:前冲预备 → 前倾挥击(命中帧 = 3,与 SkillPresentation.HitFrame 缺省一致)→ 回位。
	ghostTint := rgb8(210, 240, 255)
	// 位移/倾角是**绝对像素与角度**,而角色实际只有 ~200px 高:
	// 倾角在高处会被身高放大(rot 7° 对 200px 高的角色 = 顶部横移 24px),
	// 与 shear 叠加后很容易把人甩出 256 画布,逼着 charFitHeight 把所有人缩得更小。
	// 所以这里的幅度按「留得住画布」调过一轮:观感上的前冲/后仰仍然明显,但不再双倍累加。
	atk := []frameDef{
		fd(0, 0, 0, 1.00, 1.00, 0.00),
		fd(-4, 1, -3.0, 1.010, 0.990, -0.022),
		fd(-6, 3, -5.0, 1.020, 0.968, -0.038),
		fd(9, -2, 5.0, 0.990, 1.022, 0.050),
		fd(13, 1, 3.6, 1.000, 1.000, 0.036),
		fd(8, 2, 1.4, 1.000, 0.998, 0.014),
		fd(4, 1, 0.4, 1.000, 1.000, 0.004),
		fd(0, 0, 0, 1.00, 1.00, 0.00),
	}
	atk[2].Bright = 1.04
	atk[3].Bright = 1.16
	atk[3].Ghosts = []ghostDef{
		{Dx: -6, Dy: 1, RotDeg: 0.8, Sx: 1, Sy: 1, Alpha: 0.30, Tint: ghostTint, TintK: 0.45},
		{Dx: -12, Dy: 2, RotDeg: -2.2, Sx: 1, Sy: 1, Alpha: 0.16, Tint: ghostTint, TintK: 0.60},
	}
	atk[4].Bright = 1.08
	atk[4].Ghosts = []ghostDef{
		{Dx: -8, Dy: 0, RotDeg: 3.0, Sx: 1, Sy: 1, Alpha: 0.20, Tint: ghostTint, TintK: 0.50},
	}
	atk[5].Ghosts = []ghostDef{
		{Dx: -6, Dy: 0, RotDeg: 2.0, Sx: 1, Sy: 1, Alpha: 0.10, Tint: ghostTint, TintK: 0.45},
	}
	acts["attack"] = charAction{"attack", 12, "前冲预备(后引/下蹲)→ 第 3 帧前倾挥击(拖影 + 提亮)→ 收势回位", atk}

	// cast:下蹲蓄力 → 抬手(错切让上半身打开)→ 光晕从脚底升起。
	cast := []frameDef{
		fd(0, 0, 0, 1.000, 1.000, 0.000),
		fd(0, 3, -1.0, 1.028, 0.952, -0.018),
		fd(-1, 6, -2.0, 1.048, 0.918, -0.030),
		fd(0, 2, 0.5, 1.010, 0.992, 0.020),
		fd(0, -5, 1.6, 0.978, 1.052, 0.048),
		fd(0, -4, 1.4, 0.986, 1.042, 0.040),
		fd(0, -2, 0.6, 0.996, 1.014, 0.018),
		fd(0, 0, 0, 1.000, 1.000, 0.000),
	}
	castGlow := []float64{0.00, 0.10, 0.35, 0.70, 1.00, 0.80, 0.45, 0.12}
	for i := range cast {
		cast[i].Glow = castGlow[i]
		cast[i].Bright = 1 + 0.16*castGlow[i]
	}
	acts["cast"] = charAction{"cast", 10, "下蹲蓄力 → 抬手 → 脚底光晕升起(程序化加法辉光,着色取角色主色)", cast}

	// hit:后仰 + 闪白 + 抖动。Dx 为负 = 被推离朝向。
	// 后仰同样是 rot 与 shear 叠加,幅度按画布余量收过一轮(原 -12°/-0.092 会让 850px 宽的立绘缩到 157px)。
	hit := []frameDef{
		fd(0, 0, 0, 1.000, 1.000, 0.000),
		fd(-5, -2, -5.0, 0.992, 1.000, -0.038),
		fd(-7, -1, -6.5, 0.984, 1.000, -0.050),
		fd(-5, 0, -4.5, 0.994, 1.000, -0.034),
		fd(-3, 0, -2.8, 0.998, 1.000, -0.021),
		fd(-2, 0, -1.6, 1.000, 1.000, -0.012),
		fd(-1, 0, -0.6, 1.000, 1.000, -0.004),
		fd(0, 0, 0, 1.000, 1.000, 0.000),
	}
	// 峰值不取 1.0:全白会把角色整帧抹成剪影,12FPS 下连着两帧看不出是谁。
	// 0.85 仍然「炸」得出来,但轮廓与配色还留得住。
	flash := []float64{0.00, 0.85, 0.58, 0.34, 0.19, 0.09, 0.03, 0.00}
	jitter := []float64{0, 2, -2, 1.5, -1, 0.5, 0, 0}
	for i := range hit {
		hit[i].Flash = flash[i]
		hit[i].Dx += jitter[i]
	}
	acts["hit"] = charAction{"hit", 12, "受击后仰 + 闪白衰减 + 左右抖动", hit}

	// die:踉跄 → 侧倒 → 淡出(可选动作,默认不生成)。
	die := []frameDef{
		fd(0, 0, 0, 1.000, 1.000, 0.000),
		fd(-6, -1, -10.0, 0.996, 0.996, -0.070),
		fd(-9, 3, -22.0, 0.992, 0.960, -0.120),
		fd(-11, 8, -40.0, 0.990, 0.920, -0.150),
		fd(-12, 14, -62.0, 0.996, 0.880, -0.150),
		fd(-12, 18, -80.0, 1.004, 0.860, -0.130),
		fd(-12, 20, -86.0, 1.010, 0.855, -0.110),
		fd(-12, 21, -88.0, 1.014, 0.850, -0.100),
	}
	dieAlpha := []float64{1, 1, 1, 1, 0.94, 0.74, 0.44, 0.16}
	dieBright := []float64{1, 1.05, 0.98, 0.92, 0.86, 0.80, 0.74, 0.68}
	for i := range die {
		die[i].Alpha = dieAlpha[i]
		die[i].Bright = dieBright[i]
	}
	die[1].Flash = 0.55
	acts["die"] = charAction{"die", 8, "受击踉跄 → 侧倒 → 变暗淡出(可选)", die}

	// win:下蹲蓄力 → 起跳举臂 → 落地(可选动作,默认不生成)。
	win := []frameDef{
		fd(0, 0, 0, 1.000, 1.000, 0.000),
		fd(0, 4, -1.0, 1.035, 0.940, -0.020),
		fd(0, -8, 2.0, 0.972, 1.062, 0.040),
		fd(0, -14, 3.0, 0.960, 1.080, 0.055),
		fd(0, -12, 2.4, 0.966, 1.070, 0.048),
		fd(0, -6, 1.4, 0.982, 1.038, 0.030),
		fd(0, 2, -0.6, 1.018, 0.972, -0.012),
		fd(0, 0, 0, 1.000, 1.000, 0.000),
	}
	winBright := []float64{1, 1.02, 1.12, 1.18, 1.14, 1.06, 1.00, 1.00}
	for i := range win {
		win[i].Bright = winBright[i]
	}
	acts["win"] = charAction{"win", 8, "屈膝 → 腾起举臂(提亮)→ 落地回中立(可选)", win}

	return acts
}

// ---------------------------------------------------------------------------
// 3. 逐帧渲染
// ---------------------------------------------------------------------------

// mat2 是 2×2 仿射线性部分。
type mat2 struct{ A, B, C, D float64 }

func (m mat2) mul(n mat2) mat2 {
	return mat2{
		A: m.A*n.A + m.B*n.C, B: m.A*n.B + m.B*n.D,
		C: m.C*n.A + m.D*n.C, D: m.C*n.B + m.D*n.D,
	}
}

func (m mat2) inv() (mat2, bool) {
	det := m.A*m.D - m.B*m.C
	if math.Abs(det) < 1e-9 {
		return mat2{}, false
	}
	k := 1 / det
	return mat2{A: m.D * k, B: -m.B * k, C: -m.C * k, D: m.A * k}, true
}

// buildXform 组装 缩放 → 错切 → 旋转 的正向矩阵(作用在「相对脚底锚点」的坐标上)。
func buildXform(sx, sy, shear, rotDeg float64) mat2 {
	scale := mat2{A: sx, B: 0, C: 0, D: sy}
	// v(向上为负)乘 -shear 加到 u:正 shear ⇒ 头部前移
	sh := mat2{A: 1, B: -shear, C: 0, D: 1}
	th := rotDeg * math.Pi / 180
	cs, sn := math.Cos(th), math.Sin(th)
	rot := mat2{A: cs, B: -sn, C: sn, D: cs}
	return rot.mul(sh.mul(scale))
}

// sampleBilinear 在预乘 Layer 上做双线性采样,越界返回全 0。
func sampleBilinear(src *Layer, x, y float64) (float64, float64, float64, float64) {
	fx := math.Floor(x)
	fy := math.Floor(y)
	tx := x - fx
	ty := y - fy
	x0, y0 := int(fx), int(fy)
	var r, g, b, a float64
	for j := 0; j < 2; j++ {
		yy := y0 + j
		if yy < 0 || yy >= src.H {
			continue
		}
		wy := ty
		if j == 0 {
			wy = 1 - ty
		}
		for i := 0; i < 2; i++ {
			xx := x0 + i
			if xx < 0 || xx >= src.W {
				continue
			}
			wx := tx
			if i == 0 {
				wx = 1 - tx
			}
			w := wx * wy
			if w <= 0 {
				continue
			}
			k := yy*src.W + xx
			r += src.R[k] * w
			g += src.G[k] * w
			b += src.B[k] * w
			a += src.A[k] * w
		}
	}
	return r, g, b, a
}

// drawXformed 把 base 按仿射变换绘制到 dst(source-over),ss 为每轴超采样数。
func drawXformed(dst, base *Layer, dx, dy, rotDeg, sx, sy, shear float64,
	alphaMul, flash, bright float64, tint RGB, tintK float64, ss int) {

	m := buildXform(sx, sy, shear, rotDeg)
	inv, ok := m.inv()
	if !ok {
		return
	}
	ax := float64(charCell) * 0.5
	ay := float64(charBaselineRow) + 0.5
	if ss < 1 {
		ss = 1
	}
	step := 1.0 / float64(ss)
	nsub := float64(ss * ss)
	white := RGB{1, 1, 1}

	for y := 0; y < dst.H; y++ {
		for x := 0; x < dst.W; x++ {
			var pr, pg, pb, pa float64
			for j := 0; j < ss; j++ {
				for i := 0; i < ss; i++ {
					px := float64(x) + (float64(i)+0.5)*step
					py := float64(y) + (float64(j)+0.5)*step
					u := px - ax - dx
					v := py - ay - dy
					su := inv.A*u + inv.B*v
					sv := inv.C*u + inv.D*v
					r, g, b, a := sampleBilinear(base, su+ax, sv+ay)
					pr += r
					pg += g
					pb += b
					pa += a
				}
			}
			pa /= nsub
			if pa <= 0.0008 {
				continue
			}
			pr /= nsub
			pg /= nsub
			pb /= nsub
			c := RGB{pr / pa, pg / pa, pb / pa}
			if bright != 1 {
				c = c.Mul(bright)
			}
			if tintK > 0 {
				c = mixRGB(c, tint, tintK)
			}
			if flash > 0 {
				c = mixRGB(c, white, flash)
			}
			dst.OverPixel(x, y, c, clamp01(pa*alphaMul))
		}
	}
}

// drawCastGlowBack 脚底升起的光柱(画在角色后面)。
func drawCastGlowBack(l *Layer, g float64, dom RGB) {
	if g <= 0.001 {
		return
	}
	cx := float64(charCell) * 0.5
	cy := float64(charBaselineRow) + 1
	warm := mixRGB(dom, rgb8(255, 236, 176), 0.55)
	l.AddEllipseGlow(cx, cy, 54+36*g, 15+11*g, warm, 0.55*g, 1.7)
	l.AddEllipseGlow(cx, cy, 30+18*g, 9+6*g, rgb8(255, 253, 236), 0.48*g, 2.2)
	h := 26 + 156*g
	for i := 0; i < 28; i++ {
		u := float64(i) / 27.0
		y := cy - u*h
		r := (25 + 24*g) * (1 - 0.55*u)
		l.AddEllipseGlow(cx, y, r, r*0.30, mixRGB(warm, dom, u*0.65), 0.085*g*(1-0.85*u), 1.9)
	}
}

// drawCastGlowFront 上升的光点(画在角色前面),相位由帧号决定,确定性可复现。
func drawCastGlowFront(l *Layer, g float64, dom RGB, frame int) {
	if g <= 0.001 {
		return
	}
	cx := float64(charCell) * 0.5
	cy := float64(charBaselineRow) + 1
	core := mixRGB(rgb8(255, 255, 242), dom, 0.32)
	for i := 0; i < 10; i++ {
		ph := math.Mod(float64(frame)/float64(charFrames)+float64(i)*0.103, 1.0)
		ang := float64(i) * 2.39996 // 黄金角,分布均匀且确定
		x := cx + math.Cos(ang)*(16+16*math.Abs(math.Sin(float64(i)*1.7)))
		y := cy - ph*(118+46*g)
		a := 0.62 * g * (1 - ph) * (0.35 + 0.65*math.Abs(math.Sin(ang)))
		l.AddDot(x, y, 2.6+2.4*(1-ph), core, a, 1.7)
	}
}

// renderCharFrame 渲染一帧(E 向)。
func renderCharFrame(base *Layer, f frameDef, action string, frame int, dom RGB, ss int) *Layer {
	l := NewLayer(charCell, charCell)
	if action == "cast" {
		drawCastGlowBack(l, f.Glow, dom)
	}
	for _, gh := range f.Ghosts {
		sx, sy := gh.Sx, gh.Sy
		if sx == 0 {
			sx = 1
		}
		if sy == 0 {
			sy = 1
		}
		drawXformed(l, base, f.Dx+gh.Dx, f.Dy+gh.Dy, f.RotDeg+gh.RotDeg, sx, sy, 0,
			gh.Alpha, 0, 1, gh.Tint, gh.TintK, 1)
	}
	drawXformed(l, base, f.Dx, f.Dy, f.RotDeg, f.Sx, f.Sy, f.Shear,
		f.Alpha, f.Flash, f.Bright, RGB{}, 0, ss)
	if action == "cast" {
		drawCastGlowFront(l, f.Glow, dom, frame)
	}
	return l
}

// charFitMargin 画布四周保留的安全边(px):双线性 + 超采样会让边缘外扩约 1px。
const charFitMargin = 1.5

// charScaleForHeight 按身高统一的缩放比:scale = height / bbox 高。
// 宽向不参与(只做「不出画布」约束,见 charFitsAt),所以持矛横向超宽的立绘不会被按宽压矮。
func charScaleForHeight(cw, ch, height int) float64 {
	_ = cw
	return float64(height) / float64(maxInt(1, ch))
}

// charFitsAt 判断:把轮廓按身高 height 归一化后,选定动作的**每一帧、每一层拖影**
// 变换后是否仍完整落在 256 画布内。变换是仿射的,逐轮廓点取像即可。
func charFitsAt(sh charShape, height int, actions []charAction) bool {
	scale := charScaleForHeight(sh.CW, sh.CH, height)
	dw := maxInt(1, int(float64(sh.CW)*scale+0.5))
	dh := maxInt(1, int(float64(sh.CH)*scale+0.5))
	ax := float64(charCell) * 0.5
	ay := float64(charBaselineRow) + 0.5
	fdw, fdh := float64(dw), float64(dh)

	type layer struct{ dx, dy, rot, sx, sy, shear float64 }
	for _, act := range actions {
		for _, f := range act.Frames {
			layers := []layer{{f.Dx, f.Dy, f.RotDeg, f.Sx, f.Sy, f.Shear}}
			for _, g := range f.Ghosts {
				sx, sy := g.Sx, g.Sy
				if sx == 0 {
					sx = 1
				}
				if sy == 0 {
					sy = 1
				}
				layers = append(layers, layer{f.Dx + g.Dx, f.Dy + g.Dy, f.RotDeg + g.RotDeg, sx, sy, 0})
			}
			for _, lay := range layers {
				m := buildXform(lay.sx, lay.sy, lay.shear, lay.rot)
				for _, p := range sh.Pts {
					// (s,t) → 脚底锚点坐标系:横向居中,纵向底边压在基线上
					u := fdw * (p[0] - 0.5)
					v := fdh*(p[1]-1) + 0.5
					px := m.A*u + m.B*v + ax + lay.dx
					py := m.C*u + m.D*v + ay + lay.dy
					if px < charFitMargin || px > float64(charCell)-charFitMargin {
						return false
					}
					// 下边界放宽到画布底:die 这类倒地动作本来就该贴着底边
					if py < charFitMargin || py > float64(charCell) {
						return false
					}
				}
			}
		}
	}
	return true
}

// charFitHeight 二分求「不会被画布裁到」的最大身高像素,上界为 -char-max-height。
// 返回 0 表示连最小尺寸都放不下(动作位移幅度配得太大)。
func charFitHeight(sh charShape, actions []charAction, maxHeight int) int {
	if charFitsAt(sh, maxHeight, actions) {
		return maxHeight
	}
	lo, hi, best := 8, maxHeight, 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if charFitsAt(sh, mid, actions) {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// mirrorStripCells 逐格水平镜像(不能整条翻转,否则帧序会反过来)。
func mirrorStripCells(src *image.NRGBA, cell int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	n := w / cell
	for c := 0; c < n; c++ {
		ox := c * cell
		for y := 0; y < h; y++ {
			for x := 0; x < cell; x++ {
				si := src.PixOffset(ox+x, y)
				di := out.PixOffset(ox+cell-1-x, y)
				copy(out.Pix[di:di+4], src.Pix[si:si+4])
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 4. manifest / 标签
// ---------------------------------------------------------------------------

// charIDFromFile 去扩展名与 _v3 后缀:01_ice_sword_girl_v3.png → 01_ice_sword_girl
func charIDFromFile(name string) string {
	id := strings.TrimSuffix(name, filepath.Ext(name))
	return strings.TrimSuffix(id, "_v3")
}

// charClassRule 按文件名关键词推断职业标签(第一条命中的生效)。
var charClassRules = []struct {
	Key   string
	Class string
	Role  string
}{
	{"healer", "healer", "治疗"},
	{"lotus", "healer", "治疗"},
	{"assassin", "assassin", "刺客"},
	{"shadow", "assassin", "刺客"},
	{"archer", "archer", "远程"},
	{"bamboo", "archer", "远程"},
	{"tamer", "summoner", "召唤"},
	{"summoner", "summoner", "召唤"},
	{"guardian", "guardian", "防御"},
	{"monk", "guardian", "防御"},
	{"fist", "guardian", "防御"},
	{"spear", "spear", "近战"},
	{"saber", "blade", "近战"},
	{"sword", "blade", "近战"},
	{"blade", "blade", "近战"},
	{"talisman", "caster", "法系"},
	{"caster", "caster", "法系"},
	{"alchemy", "caster", "法系"},
	{"scholar", "caster", "法系"},
	{"calligrapher", "caster", "法系"},
	{"formation", "caster", "法系"},
	{"musician", "support", "辅助"},
	{"dancer", "support", "辅助"},
	{"bell", "support", "辅助"},
	{"cook", "civilian", "杂役"},
	{"waiter", "civilian", "杂役"},
}

func charTags(id string) (gender, class, role string) {
	low := strings.ToLower(id)
	switch {
	case strings.Contains(low, "_girl"):
		gender = "female"
	case strings.Contains(low, "_boy"):
		gender = "male"
	default:
		gender = "unknown"
	}
	for _, r := range charClassRules {
		if strings.Contains(low, r.Key) {
			return gender, r.Class, r.Role
		}
	}
	return gender, "unknown", "未知"
}

// CharActionEntry manifest / meta 里的一条动作记录。
type CharActionEntry struct {
	Action  string   `json:"action"`
	FPS     int      `json:"fps"`
	Frames  int      `json:"frames"`
	Cell    []int    `json:"cell"`
	Strip   []int    `json:"strip"`
	Files   []string `json:"files"`
	HitHint int      `json:"hit_frame_hint,omitempty"`
	Desc    string   `json:"desc"`
}

// CharMeta 每个角色目录下的 meta.json。
type CharMeta struct {
	CharacterID  string            `json:"character_id"`
	Index        int               `json:"index"`
	Source       string            `json:"source"`
	SourceSize   []int             `json:"source_size"`
	Keying       string            `json:"keying"`
	ChromaRef    []int             `json:"chroma_ref_rgb,omitempty"`
	AlphaThres   int               `json:"alpha_threshold"`
	BBoxXYWH     []int             `json:"bbox_xywh"`
	DroppedComps int               `json:"dropped_components"`
	Height       int               `json:"height"`     // 统一身高(= placed_size[1])
	FitHeight    int               `json:"fit_height"` // 这张立绘单独能放下的最大身高(全体取 min 得统一值)
	Scale        float64           `json:"scale"`
	PlacedSize   []int             `json:"placed_size"`
	IdleTopRow   int               `json:"idle_top_row"` // idle 首帧头顶所在行(自检回读,alpha>=16)
	Cell         []int             `json:"cell"`
	Frames       int               `json:"frames"`
	FeetFromBot  int               `json:"feet_baseline_px_from_bottom"`
	BaselineRow  int               `json:"feet_baseline_row"`
	Pivot        []float64         `json:"pivot"`
	DominantRGB  []int             `json:"dominant_rgb"`
	Gender       string            `json:"gender"`
	Class        string            `json:"class_tag"`
	Role         string            `json:"role_cn"`
	Actions      []CharActionEntry `json:"actions"`
	GeneratedAt  string            `json:"generated_at"`
	Tool         string            `json:"tool"`
}

// CharManifestEntry CHARACTER_MANIFEST.json 里的一个角色。
type CharManifestEntry struct {
	CharacterID string   `json:"character_id"`
	Index       int      `json:"index"`
	Source      string   `json:"source"`
	Gender      string   `json:"gender"`
	Class       string   `json:"class_tag"`
	Role        string   `json:"role_cn"`
	Dir         string   `json:"dir"`
	Actions     []string `json:"actions"`
	Directions  []string `json:"directions"`
	DominantRGB []int    `json:"dominant_rgb"`
	Scale       float64  `json:"scale"`
	Height      int      `json:"height"`
	FitHeight   int      `json:"fit_height"`
	IdleTopRow  int      `json:"idle_top_row"`
	BBoxXYWH    []int    `json:"bbox_xywh"`
	PlacedSize  []int    `json:"placed_size"`
	Meta        string   `json:"meta"`
}

// CharSkipEntry 未能产出的立绘及原因(不静默少出)。
type CharSkipEntry struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// CharManifest 顶层 CHARACTER_MANIFEST.json。
type CharManifest struct {
	Tool         string              `json:"tool"`
	Command      string              `json:"command"`
	GeneratedAt  string              `json:"generated_at"`
	Spec         string              `json:"spec"`
	Cell         []int               `json:"cell"`
	Frames       int                 `json:"frames"`
	Pivot        []float64           `json:"pivot"`
	FeetFromBot  int                 `json:"feet_baseline_px_from_bottom"`
	StripLayout  string              `json:"strip_layout"`
	Actions      []CharActionSpec    `json:"actions"`
	Directions   []string            `json:"directions"`
	Params       charOptions         `json:"params"`
	SpriteImport map[string]any      `json:"sprite_import_hint"`
	Notes        []string            `json:"notes"`
	CharacterIDs []string            `json:"character_ids"`
	Characters   []CharManifestEntry `json:"characters"`
	Skipped      []CharSkipEntry     `json:"skipped"`
	Verify       map[string]any      `json:"verify"`
	FileCount    int                 `json:"file_count"`
	TotalBytes   int64               `json:"total_bytes"`
}

// charTopRowTolerance 22 个角色 idle 首帧头顶行允许的最大离差(px)。
// 统一身高后理论上全部相等,只剩 box 重采样 + alpha 阈值的 ±1 抖动;超过就说明有人被单独缩了。
const charTopRowTolerance = 4

// CharActionSpec manifest 顶层的动作规格。
type CharActionSpec struct {
	Name    string `json:"name"`
	FPS     int    `json:"fps"`
	Frames  int    `json:"frames"`
	HitHint int    `json:"hit_frame_hint,omitempty"`
	Desc    string `json:"desc"`
}

// ---------------------------------------------------------------------------
// 5. 主流程
// ---------------------------------------------------------------------------

// charResult 单个角色的产出。
type charResult struct {
	Meta    CharMeta
	Entry   CharManifestEntry
	Files   int
	Bytes   int64
	Err     error
	SrcFile string
	Checks  []string
	IdleTop int // idle 首帧头顶行(自检回读)
}

func runCharacters(opt charOptions) error {
	files, err := filepath.Glob(filepath.Join(opt.SrcDir, "*.png"))
	if err != nil {
		return fmt.Errorf("扫描 %s 失败: %w", opt.SrcDir, err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("%s 下没有 png 立绘", opt.SrcDir)
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", opt.OutDir, err)
	}

	allActions := buildCharActions()
	var order []charAction
	for _, name := range opt.Actions {
		a, ok := allActions[name]
		if !ok {
			return fmt.Errorf("未知动作 %q(可选:idle,attack,cast,hit,die,win)", name)
		}
		order = append(order, a)
	}

	if len(opt.Dirs) == 0 {
		opt.Dirs = []string{"E"}
	}
	if !charWantsDir(opt, "E") {
		return fmt.Errorf("-char-dirs 必须包含 E(W 只是 E 的镜像,单独出 W 没有意义)")
	}
	for _, d := range opt.Dirs {
		if !strings.EqualFold(d, "E") && !strings.EqualFold(d, "W") {
			return fmt.Errorf("未知朝向 %q(可选 E,W)", d)
		}
	}

	// ── 第 1 遍:量包围盒,求出全体统一的**身高** ──────────────────
	// 统一(而非逐角色)缩放,是为了让同一场战斗里所有人身高一致;取全体的最小可行值,
	// 保证最"胖"的那张(横向持矛、包围盒宽 > 高)在 attack 前冲 / hit 后仰时也不会被裁到。
	// 统一的是身高而不是长边:按长边归一会把宽立绘压矮 10%,和后排 0.88 的近大远小量级相当。
	nproc := maxInt(1, runtime.NumCPU())
	probes := make([]struct {
		sh  charShape
		err error
	}, len(files))
	var pwg sync.WaitGroup
	psem := make(chan struct{}, nproc)
	for i, p := range files {
		pwg.Add(1)
		go func(i int, p string) {
			defer pwg.Done()
			psem <- struct{}{}
			defer func() { <-psem }()
			probes[i].sh, probes[i].err = probeCharShape(p, opt)
		}(i, p)
	}
	pwg.Wait()

	fitted := opt.MaxHeight
	tightest := ""
	perFit := make([]int, len(files))
	for i, pr := range probes {
		if pr.err != nil {
			continue // 真正的失败在第 2 遍统一记进 skipped
		}
		perFit[i] = charFitHeight(pr.sh, order, opt.MaxHeight)
		if perFit[i] < fitted {
			fitted, tightest = perFit[i], filepath.Base(files[i])
		}
	}
	if fitted <= 0 {
		return fmt.Errorf("动作位移幅度过大,任何缩放都会被 %d 画布裁到", charCell)
	}
	opt.FittedHeight = fitted
	if fitted < opt.MaxHeight {
		fmt.Printf("统一身高 %dpx(上限 %d;受 %s 约束,留出 attack/hit 的位移余量)\n",
			fitted, opt.MaxHeight, tightest)
	} else {
		fmt.Printf("统一身高 %dpx(上限内所有动作均不出画布)\n", fitted)
	}
	for i, pr := range probes {
		if pr.err == nil {
			fmt.Printf("    单张可放下的最大身高 %3dpx  bbox %4d×%-4d  %s\n",
				perFit[i], pr.sh.CW, pr.sh.CH, filepath.Base(files[i]))
		}
	}

	// ── 第 2 遍:按统一长边出图 ─────────────────────────────
	results := make([]charResult, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, nproc)
	for i, p := range files {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = genOneCharacter(i+1, p, order, opt, fitted, perFit[i])
		}(i, p)
	}
	wg.Wait()

	mf := CharManifest{
		Tool:        "tools/battle_art_gen",
		Command:     "go run . -mode characters",
		GeneratedAt: time.Now().Format(time.RFC3339),
		Spec:        "BattleArtCatalog.cs(权威);docs/design/battle-art-prompts.md §1/§3;turn-battle-presentation.md §5",
		Cell:        []int{charCell, charCell},
		Frames:      charFrames,
		Pivot:       []float64{0.5, 0.08},
		FeetFromBot: charFeetFromBottom,
		StripLayout: fmt.Sprintf("横条 = %d 帧 × %d×%d,左→右时间序;客户端 LoadStrip(cellSize=0) 用贴图高当格宽自动切成 %d 帧",
			charFrames, charCell, charCell, charFrames),
		Directions:   opt.Dirs,
		Params:       opt,
		CharacterIDs: []string{},
		Characters:   []CharManifestEntry{},
		Skipped:      []CharSkipEntry{},
		SpriteImport: map[string]any{
			"path":        "Assets/Resources/Battle/Characters/<id>/<action>_E_strip.png(W 由客户端镜像;-char-dirs E,W 时才有 <action>_W_strip.png)",
			"importer":    "Assets/Editor/QdaoCharacterSpriteImporter.cs 已覆盖 Assets/Resources/Battle/(Default 贴图 + Bilinear + 无 mip + 无压缩 + alphaIsTransparency + maxSize 4096)",
			"sprite_mode": "运行时 Sprite.Create 切格(BattleArtCatalog.LoadStrip),不依赖 Sprite 导入设置",
			"grid":        charCell,
			"pivot":       []float64{0.5, 0.08},
			"filter":      "Bilinear",
			"compression": "Uncompressed(帧条有真实 alpha 软边,压缩会出脏边)",
			"max_size":    4096,
		},
	}
	for _, a := range order {
		spec := CharActionSpec{Name: a.Name, FPS: a.FPS, Frames: charFrames, Desc: a.Desc}
		if a.Name == "attack" || a.Name == "cast" {
			spec.HitHint = 3
		}
		mf.Actions = append(mf.Actions, spec)
	}

	okCount := 0
	topMin, topMax := charCell, -1
	for _, r := range results {
		if r.Err != nil {
			mf.Skipped = append(mf.Skipped, CharSkipEntry{
				File: filepath.ToSlash(r.SrcFile), Reason: r.Err.Error(),
			})
			fmt.Printf("  跳过 %-42s %v\n", filepath.Base(r.SrcFile), r.Err)
			continue
		}
		okCount++
		mf.CharacterIDs = append(mf.CharacterIDs, r.Entry.CharacterID)
		mf.Characters = append(mf.Characters, r.Entry)
		mf.FileCount += r.Files
		mf.TotalBytes += r.Bytes
		if r.IdleTop < topMin {
			topMin = r.IdleTop
		}
		if r.IdleTop > topMax {
			topMax = r.IdleTop
		}
		fmt.Printf("  角色 %-30s bbox=%v scale=%.4f placed=%v 头顶行=%d 主色=%v 文件 %d\n",
			r.Entry.CharacterID, r.Entry.BBoxXYWH, r.Entry.Scale, r.Entry.PlacedSize,
			r.IdleTop, r.Entry.DominantRGB, r.Files)
		for _, c := range r.Checks {
			fmt.Printf("       ! %s\n", c)
		}
	}
	mf.Notes = []string{
		fmt.Sprintf("单格 %d×%d、每动作 %d 帧、横条 %d×%d;客户端切格口径见 BattleArtCatalog.LoadStrip(cellSize=0 → 格宽=贴图高)。",
			charCell, charCell, charFrames, charCell*charFrames, charCell),
		fmt.Sprintf("脚底基线固定在距底边 %dpx(= FeetPivot.y 0.08 × %d)的第 %d 行;idle 首帧最低不透明像素严格落在该行。",
			charFeetFromBottom, charCell, charBaselineRow),
		charDirsNote(opt),
		fmt.Sprintf("全体按**身高**统一到 %dpx(不是长边):22 个 idle 首帧头顶行离差 ≤ %dpx 由自检断言;"+
			"横向超宽(持矛/持剑)的立绘也和别人一样高,近大远小只由 BattleStage 的排深缩放表达。",
			opt.FittedHeight, charTopRowTolerance),
		"所有动作帧都是同一张底图的几何/色彩变换(平移·旋转·错切·缩放·闪白·拖影)+ cast 的程序化脚底光晕,不含逐帧重绘。",
		"character_id 稳定 = 立绘文件名去 .png 与 _v3;客户端按 actor_id 取模选 character_ids 即可保证同一玩家每场一致。",
		"gender / class_tag 由文件名关键词推断,仅作选择与降级的提示,不是服务端权威数据。",
		"本目录未生成 .meta,Unity 首次导入时自动生成。",
	}

	// 统一身高的自检:所有角色 idle 首帧头顶行离差必须 ≤ charTopRowTolerance
	spread := topMax - topMin
	mf.Verify = map[string]any{
		"idle_top_row_min":       topMin,
		"idle_top_row_max":       topMax,
		"idle_top_row_spread":    spread,
		"idle_top_row_tolerance": charTopRowTolerance,
		"feet_baseline_row":      charBaselineRow,
		"assertions": []string{
			fmt.Sprintf("每张 strip 尺寸 %d×%d、NRGBA 直通 alpha、切格 %d 帧、每帧非全透明", charCell*charFrames, charCell, charFrames),
			fmt.Sprintf("idle 首帧最低不透明行 == %d(脚底基线)", charBaselineRow),
			fmt.Sprintf("全部角色 idle 首帧头顶行(alpha>=16)最大离差 ≤ %dpx(统一身高)", charTopRowTolerance),
			"若 -char-dirs 含 W:W 与 E 逐格水平镜像逐像素一致",
		},
	}
	if okCount > 0 && spread > charTopRowTolerance {
		mf.Skipped = append(mf.Skipped, CharSkipEntry{
			File:   "(全体)",
			Reason: fmt.Sprintf("统一身高失败:idle 首帧头顶行离差 %dpx(%d..%d)> 容差 %d", spread, topMin, topMax, charTopRowTolerance),
		})
	}

	mpath := filepath.Join(opt.OutDir, "CHARACTER_MANIFEST.json")
	if err := writeJSON(mpath, &mf); err != nil {
		return err
	}
	mf.FileCount++

	fmt.Printf("\n完成:角色 %d/%d,strip %d 张(%d 动作 × %d 向),清单 %s\n",
		okCount, len(files), mf.FileCount-okCount-1, len(order), len(opt.Dirs), mpath)
	fmt.Printf("统一身高自检:idle 首帧头顶行 %d..%d(离差 %dpx,容差 %d)\n", topMin, topMax, spread, charTopRowTolerance)
	fmt.Printf("产物总字节:%d(%.1f MB)\n", mf.TotalBytes, float64(mf.TotalBytes)/1024/1024)
	if len(mf.Skipped) > 0 {
		return fmt.Errorf("有 %d 项未通过,详见 CHARACTER_MANIFEST.json 的 skipped", len(mf.Skipped))
	}
	return nil
}

// charDirsNote 写进 manifest notes 的朝向说明。
func charDirsNote(opt charOptions) string {
	if charWantsDir(opt, "W") {
		return "本次 -char-dirs 含 W:W = E 的逐格水平镜像(与客户端 LoadDirectionalStrip 的镜像降级结果逐像素一致,只是省掉运行时翻转,代价是体积翻倍)。"
	}
	return "只出 E 向;W 由客户端 BattleArtCatalog.LoadDirectionalStrip 缺图降级取 E 并置 Mirrored=true,BattleUnitView 翻 localScale.x。" +
		"不再落盘 W:它只是 E 的逐格镜像副本,画面零差异,却让 Resources 里的未压缩纹理体积翻倍。"
}

func genOneCharacter(index int, path string, actions []charAction, opt charOptions, height, fitHeight int) charResult {
	res := charResult{SrcFile: path}
	cs, err := loadCharSource(path, opt, height)
	if err != nil {
		res.Err = err
		return res
	}
	dir := filepath.Join(opt.OutDir, cs.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Err = fmt.Errorf("创建目录失败: %w", err)
		return res
	}

	gender, class, role := charTags(cs.ID)
	meta := CharMeta{
		CharacterID: cs.ID, Index: index, Source: cs.File,
		SourceSize: []int{cs.SrcW, cs.SrcH}, Keying: cs.Keying,
		AlphaThres: opt.AlphaThres, BBoxXYWH: cs.BBox[:], DroppedComps: cs.DroppedComps,
		Height: height, FitHeight: fitHeight,
		Scale: roundTo(cs.Scale, 6), PlacedSize: cs.Placed[:],
		Cell: []int{charCell, charCell}, Frames: charFrames,
		FeetFromBot: charFeetFromBottom, BaselineRow: charBaselineRow,
		Pivot: []float64{0.5, 0.08}, DominantRGB: cs.DominantU8[:],
		Gender: gender, Class: class, Role: role,
		GeneratedAt: time.Now().Format(time.RFC3339),
		Tool:        "tools/battle_art_gen -mode characters",
	}
	if cs.Keying == "chroma" {
		meta.ChromaRef = cs.ChromaRef[:]
	}

	stripW := charCell * charFrames
	actionNames := make([]string, 0, len(actions))
	for _, act := range actions {
		strip := image.NewNRGBA(image.Rect(0, 0, stripW, charCell))
		for f := 0; f < charFrames; f++ {
			l := renderCharFrame(cs.Base, act.Frames[f], act.Name, f, cs.Dominant, opt.Supersample)
			frame := l.ToNRGBA()
			draw.Draw(strip, image.Rect(f*charCell, 0, (f+1)*charCell, charCell), frame, image.Point{}, draw.Src)
		}
		type dirImg struct {
			dir string
			img *image.NRGBA
		}
		pairs := []dirImg{{"E", strip}}
		if charWantsDir(opt, "W") {
			pairs = append(pairs, dirImg{"W", mirrorStripCells(strip, charCell)})
		}
		// 不再默认落盘 W:客户端 LoadDirectionalStrip 缺 W 取 E 并镜像,画面与落盘的镜像副本逐像素一致。
		// 旧产物里若还残留 *_W_strip.png(及 .meta)一并清掉,避免客户端优先命中陈旧的 W。
		if !charWantsDir(opt, "W") {
			for _, stale := range []string{
				filepath.Join(dir, act.Name+"_W_strip.png"),
				filepath.Join(dir, act.Name+"_W_strip.png.meta"),
			} {
				if err := os.Remove(stale); err == nil {
					res.Checks = append(res.Checks, fmt.Sprintf("%s:已删除陈旧的 %s", cs.ID, filepath.Base(stale)))
				}
			}
		}

		files := make([]string, 0, len(pairs))
		for _, pair := range pairs {
			name := fmt.Sprintf("%s_%s_strip.png", act.Name, pair.dir)
			p := filepath.Join(dir, name)
			if err := saveCharPNG(p, pair.img); err != nil {
				res.Err = err
				return res
			}
			// 自检:重新解码断言尺寸/帧数/不透明度/脚底行,顺带回读 idle 首帧头顶行(统一身高断言用)
			chk, top, err := verifyCharStrip(p, act.Name, pair.dir, cs.ID)
			if err != nil {
				res.Err = err
				return res
			}
			if act.Name == "idle" && pair.dir == "E" {
				res.IdleTop = top
				meta.IdleTopRow = top
			}
			res.Checks = append(res.Checks, chk...)
			if st, err := os.Stat(p); err == nil {
				res.Bytes += st.Size()
			}
			files = append(files, name)
			res.Files++
		}
		entry := CharActionEntry{
			Action: act.Name, FPS: act.FPS, Frames: charFrames,
			Cell: []int{charCell, charCell}, Strip: []int{stripW, charCell},
			Files: files, Desc: act.Desc,
		}
		if act.Name == "attack" || act.Name == "cast" {
			entry.HitHint = 3
		}
		meta.Actions = append(meta.Actions, entry)
		actionNames = append(actionNames, act.Name)
	}

	if err := writeJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		res.Err = err
		return res
	}
	res.Files++
	if st, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
		res.Bytes += st.Size()
	}

	res.Meta = meta
	res.Entry = CharManifestEntry{
		CharacterID: cs.ID, Index: index, Source: cs.File,
		Gender: gender, Class: class, Role: role,
		Dir:         "Characters/" + cs.ID,
		Actions:     actionNames,
		Directions:  opt.Dirs,
		DominantRGB: cs.DominantU8[:],
		Scale:       roundTo(cs.Scale, 6),
		Height:      height,
		FitHeight:   fitHeight,
		IdleTopRow:  res.IdleTop,
		BBoxXYWH:    cs.BBox[:],
		PlacedSize:  cs.Placed[:],
		Meta:        "Characters/" + cs.ID + "/meta.json",
	}
	return res
}

func saveCharPNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", path, err)
	}
	defer f.Close()
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, img); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// verifyCharStrip 重新解码产物并断言:尺寸 / 通道 / 帧数 / 每帧非全透明 / idle 首帧脚底对齐。
// 返回「非致命告警」(例如角色边缘被画布裁到)与第 0 帧头顶行(alpha>=16 的最高行,统一身高断言用)。
func verifyCharStrip(path, action, dir, id string) ([]string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("自检打开 %s 失败: %w", path, err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("自检解码 %s 失败: %w", path, err)
	}
	nrgba, ok := img.(*image.NRGBA)
	if !ok {
		return nil, 0, fmt.Errorf("%s 不是 NRGBA(直通 alpha),实际 %T", path, img)
	}
	b := nrgba.Bounds()
	if b.Dx() != charCell*charFrames || b.Dy() != charCell {
		return nil, 0, fmt.Errorf("%s 尺寸 %dx%d,期望 %dx%d", path, b.Dx(), b.Dy(), charCell*charFrames, charCell)
	}
	if b.Dx()/b.Dy() != charFrames {
		return nil, 0, fmt.Errorf("%s 切格后帧数 %d,期望 %d", path, b.Dx()/b.Dy(), charFrames)
	}
	var warns []string
	top0 := charCell
	for f := 0; f < charFrames; f++ {
		ox := f * charCell
		opaque, bottom := 0, -1
		for y := 0; y < charCell; y++ {
			for x := 0; x < charCell; x++ {
				a := nrgba.Pix[nrgba.PixOffset(ox+x, y)+3]
				if a >= 8 {
					opaque++
					if y > bottom {
						bottom = y
					}
				}
				if f == 0 && a >= 16 && y < top0 {
					top0 = y
				}
			}
		}
		if opaque == 0 {
			return nil, 0, fmt.Errorf("%s 第 %d 帧全透明", path, f)
		}
		if action == "idle" && f == 0 && bottom != charBaselineRow {
			return nil, 0, fmt.Errorf("%s idle 首帧脚底在第 %d 行,期望第 %d 行", path, bottom, charBaselineRow)
		}
	}
	// 溢出告警:左右/上边缘有实体像素说明变换把角色推出了画布
	edge := 0
	for y := 0; y < charCell; y++ {
		for _, x := range []int{0, charCell*charFrames - 1} {
			if nrgba.Pix[nrgba.PixOffset(x, y)+3] >= 16 {
				edge++
			}
		}
	}
	for f := 0; f < charFrames; f++ {
		for x := 0; x < charCell; x++ {
			if nrgba.Pix[nrgba.PixOffset(f*charCell+x, 0)+3] >= 16 {
				edge++
			}
		}
	}
	if edge > 0 {
		warns = append(warns, fmt.Sprintf("%s/%s_%s:画布边缘有 %d 个实体像素(角色被裁到边)", id, action, dir, edge))
	}
	return warns, top0, nil
}

// ---------------------------------------------------------------------------

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func roundTo(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// 让 encoding/json 参与编译期检查(CharManifest 里用了 any)。
var _ = json.Marshal
