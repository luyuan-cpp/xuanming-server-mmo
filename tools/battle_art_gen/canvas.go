package main

// canvas.go —— 绘制底座:预乘 alpha 的浮点画布 + 加法辉光 / 常规叠加两种混合,
// 以及特效常用的软点、径向渐变、路径描边、多边形填充等图元。

import (
	"image"
	"math"
)

// RGB 是 0..1 的线性颜色;加法叠加时允许 >1,输出阶段再钳位(高光溢出成白芯)。
type RGB struct{ R, G, B float64 }

func rgb8(r, g, b int) RGB {
	return RGB{float64(r) / 255.0, float64(g) / 255.0, float64(b) / 255.0}
}

// Mul 整体缩放亮度。
func (c RGB) Mul(s float64) RGB { return RGB{c.R * s, c.G * s, c.B * s} }

func mixRGB(a, b RGB, t float64) RGB {
	return RGB{a.R + (b.R-a.R)*t, a.G + (b.G-a.G)*t, a.B + (b.B-a.B)*t}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func smoothstep(e0, e1, x float64) float64 {
	if e1 == e0 {
		if x < e0 {
			return 0
		}
		return 1
	}
	t := clamp01((x - e0) / (e1 - e0))
	return t * t * (3 - 2*t)
}

func easeOut(t float64) float64 { return 1 - math.Pow(1-clamp01(t), 3) }
func easeIn(t float64) float64  { t = clamp01(t); return t * t * t }

// ---------------------------------------------------------------------------
// Layer:预乘 alpha 的浮点 RGBA 画布
// ---------------------------------------------------------------------------

type Layer struct {
	W, H       int
	R, G, B, A []float64
}

func NewLayer(w, h int) *Layer {
	n := w * h
	return &Layer{
		W: w, H: h,
		R: make([]float64, n),
		G: make([]float64, n),
		B: make([]float64, n),
		A: make([]float64, n),
	}
}

// AddPixel 做加法辉光:颜色按预乘累加,alpha 走 screen(不会硬切边)。
func (l *Layer) AddPixel(x, y int, c RGB, a float64) {
	if a <= 0 || x < 0 || y < 0 || x >= l.W || y >= l.H {
		return
	}
	if a > 1 {
		a = 1
	}
	i := y*l.W + x
	l.R[i] += c.R * a
	l.G[i] += c.G * a
	l.B[i] += c.B * a
	l.A[i] = l.A[i] + a - l.A[i]*a
}

// OverPixel 是标准预乘 source-over,给 UI / 图标这类不透明绘制用。
func (l *Layer) OverPixel(x, y int, c RGB, a float64) {
	if a <= 0 || x < 0 || y < 0 || x >= l.W || y >= l.H {
		return
	}
	if a > 1 {
		a = 1
	}
	i := y*l.W + x
	inv := 1 - a
	l.R[i] = c.R*a + l.R[i]*inv
	l.G[i] = c.G*a + l.G[i]*inv
	l.B[i] = c.B*a + l.B[i]*inv
	l.A[i] = a + l.A[i]*inv
}

// ToNRGBA 把预乘缓冲还原成直通 alpha 的 NRGBA(PNG 真实透明)。
func (l *Layer) ToNRGBA() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, l.W, l.H))
	for y := 0; y < l.H; y++ {
		for x := 0; x < l.W; x++ {
			i := y*l.W + x
			a := clamp01(l.A[i])
			o := img.PixOffset(x, y)
			if a <= 0 {
				img.Pix[o+0], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = 0, 0, 0, 0
				continue
			}
			img.Pix[o+0] = uint8(clamp01(l.R[i]/a)*255 + 0.5)
			img.Pix[o+1] = uint8(clamp01(l.G[i]/a)*255 + 0.5)
			img.Pix[o+2] = uint8(clamp01(l.B[i]/a)*255 + 0.5)
			img.Pix[o+3] = uint8(a*255 + 0.5)
		}
	}
	return img
}

// ---------------------------------------------------------------------------
// 加法图元
// ---------------------------------------------------------------------------

// AddDot 画一个软圆点(径向衰减),power 越大越"收心"。
func (l *Layer) AddDot(cx, cy, r float64, c RGB, intensity, power float64) {
	if r <= 0 || intensity <= 0 {
		return
	}
	x0, x1 := int(math.Floor(cx-r)), int(math.Ceil(cx+r))
	y0, y1 := int(math.Floor(cy-r)), int(math.Ceil(cy+r))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			d := math.Hypot(dx, dy)
			if d >= r {
				continue
			}
			a := intensity * math.Pow(1-d/r, power)
			l.AddPixel(x, y, c, a)
		}
	}
}

// AddRadial 画一个双色径向渐变球:core 半径内是核心色,到 r 渐变为 edge 色并淡出。
func (l *Layer) AddRadial(cx, cy, r, core float64, inner, outer RGB, intensity, power float64) {
	if r <= 0 || intensity <= 0 {
		return
	}
	x0, x1 := int(math.Floor(cx-r)), int(math.Ceil(cx+r))
	y0, y1 := int(math.Floor(cy-r)), int(math.Ceil(cy+r))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			d := math.Hypot(dx, dy)
			if d >= r {
				continue
			}
			k := clamp01((d - core) / math.Max(r-core, 1e-6))
			c := mixRGB(inner, outer, k)
			a := intensity * math.Pow(1-d/r, power)
			l.AddPixel(x, y, c, a)
		}
	}
}

// AddEllipseGlow 画一个软椭圆(地面光斑用)。
func (l *Layer) AddEllipseGlow(cx, cy, rx, ry float64, c RGB, intensity, power float64) {
	x0, x1 := int(math.Floor(cx-rx)), int(math.Ceil(cx+rx))
	y0, y1 := int(math.Floor(cy-ry)), int(math.Ceil(cy+ry))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx := (float64(x) + 0.5 - cx) / math.Max(rx, 1e-6)
			dy := (float64(y) + 0.5 - cy) / math.Max(ry, 1e-6)
			d := math.Hypot(dx, dy)
			if d >= 1 {
				continue
			}
			l.AddPixel(x, y, c, intensity*math.Pow(1-d, power))
		}
	}
}

// PathStyle 描述沿路径参数 u∈[0,1] 变化的笔形。
type PathStyle struct {
	Width     func(u float64) float64 // 核心半宽(px)
	Glow      func(u float64) float64 // 核心之外的辉光厚度
	Color     func(u float64) RGB
	GlowColor func(u float64) RGB
	Alpha     func(u float64) float64
	GlowGain  float64 // 辉光强度系数,默认 0.6
}

func constW(v float64) func(float64) float64 { return func(float64) float64 { return v } }
func constC(c RGB) func(float64) RGB         { return func(float64) RGB { return c } }
func constA(v float64) func(float64) float64 { return func(float64) float64 { return v } }

// StrokePath 用距离场描边,避免密集点叠加导致 alpha 硬切边;支持沿路径渐变。
func (l *Layer) StrokePath(pts [][2]float64, st PathStyle) {
	if len(pts) < 2 {
		return
	}
	if st.Width == nil {
		st.Width = constW(1)
	}
	if st.Glow == nil {
		st.Glow = constW(0)
	}
	if st.Color == nil {
		st.Color = constC(RGB{1, 1, 1})
	}
	if st.GlowColor == nil {
		st.GlowColor = st.Color
	}
	if st.Alpha == nil {
		st.Alpha = constA(1)
	}
	gain := st.GlowGain
	if gain == 0 {
		gain = 0.6
	}

	maxR := 0.0
	for i := 0; i <= 32; i++ {
		u := float64(i) / 32
		if v := st.Width(u) + st.Glow(u); v > maxR {
			maxR = v
		}
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX = math.Min(minX, p[0])
		minY = math.Min(minY, p[1])
		maxX = math.Max(maxX, p[0])
		maxY = math.Max(maxY, p[1])
	}
	x0 := int(math.Floor(minX - maxR - 2))
	x1 := int(math.Ceil(maxX + maxR + 2))
	y0 := int(math.Floor(minY - maxR - 2))
	y1 := int(math.Ceil(maxY + maxR + 2))
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > l.W-1 {
		x1 = l.W - 1
	}
	if y1 > l.H-1 {
		y1 = l.H - 1
	}
	nseg := float64(len(pts) - 1)

	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			best := math.Inf(1)
			bestU := 0.0
			for i := 0; i < len(pts)-1; i++ {
				ax, ay := pts[i][0], pts[i][1]
				bx, by := pts[i+1][0], pts[i+1][1]
				vx, vy := bx-ax, by-ay
				wx, wy := px-ax, py-ay
				den := vx*vx + vy*vy
				t := 0.0
				if den > 1e-9 {
					t = clamp01((wx*vx + wy*vy) / den)
				}
				dx := wx - t*vx
				dy := wy - t*vy
				d := math.Hypot(dx, dy)
				if d < best {
					best = d
					bestU = (float64(i) + t) / nseg
				}
			}
			w := st.Width(bestU)
			g := st.Glow(bestU)
			amp := st.Alpha(bestU)
			if amp <= 0 {
				continue
			}
			if best > w+g+1.5 {
				continue
			}
			if g > 0 {
				k := clamp01(1 - (best-w)/g)
				if best < w {
					k = 1
				}
				ga := amp * gain * k * k
				l.AddPixel(x, y, st.GlowColor(bestU), ga)
			}
			core := smoothstep(w+0.75, w-0.75, best)
			if core > 0 {
				l.AddPixel(x, y, st.Color(bestU), amp*core)
			}
		}
	}
}

// FillPolyAdd 用 3x3 超采样加法填充凸/凹多边形。
func (l *Layer) FillPolyAdd(pts [][2]float64, c RGB, alpha float64) {
	if len(pts) < 3 || alpha <= 0 {
		return
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX = math.Min(minX, p[0])
		minY = math.Min(minY, p[1])
		maxX = math.Max(maxX, p[0])
		maxY = math.Max(maxY, p[1])
	}
	x0, y0 := int(math.Floor(minX)), int(math.Floor(minY))
	x1, y1 := int(math.Ceil(maxX)), int(math.Ceil(maxY))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			cov := polyCoverage(pts, x, y)
			if cov > 0 {
				l.AddPixel(x, y, c, alpha*cov)
			}
		}
	}
}

func polyCoverage(pts [][2]float64, x, y int) float64 {
	hit := 0
	const n = 3
	for sy := 0; sy < n; sy++ {
		for sx := 0; sx < n; sx++ {
			px := float64(x) + (float64(sx)+0.5)/n
			py := float64(y) + (float64(sy)+0.5)/n
			if pointInPoly(pts, px, py) {
				hit++
			}
		}
	}
	return float64(hit) / float64(n*n)
}

func pointInPoly(pts [][2]float64, px, py float64) bool {
	in := false
	j := len(pts) - 1
	for i := 0; i < len(pts); i++ {
		xi, yi := pts[i][0], pts[i][1]
		xj, yj := pts[j][0], pts[j][1]
		if (yi > py) != (yj > py) {
			xc := xi + (py-yi)/(yj-yi)*(xj-xi)
			if px < xc {
				in = !in
			}
		}
		j = i
	}
	return in
}

// ---------------------------------------------------------------------------
// 路径生成小工具
// ---------------------------------------------------------------------------

// arcPoints 在圆上采样一段弧。角度用弧度,y 轴向下。
func arcPoints(cx, cy, r, a0, a1 float64, n int) [][2]float64 {
	pts := make([][2]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		a := a0 + (a1-a0)*t
		pts = append(pts, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return pts
}

// ellipsePoints 采样一整圈椭圆(用于地面光环)。
func ellipsePoints(cx, cy, rx, ry float64, n int) [][2]float64 {
	pts := make([][2]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		a := 2 * math.Pi * float64(i) / float64(n)
		pts = append(pts, [2]float64{cx + rx*math.Cos(a), cy + ry*math.Sin(a)})
	}
	return pts
}

// bezier3 三次贝塞尔采样。
func bezier3(p0, p1, p2, p3 [2]float64, n int) [][2]float64 {
	pts := make([][2]float64, 0, n+1)
	for i := 0; i <= n; i++ {
		t := float64(i) / float64(n)
		mt := 1 - t
		w0 := mt * mt * mt
		w1 := 3 * mt * mt * t
		w2 := 3 * mt * t * t
		w3 := t * t * t
		pts = append(pts, [2]float64{
			w0*p0[0] + w1*p1[0] + w2*p2[0] + w3*p3[0],
			w0*p0[1] + w1*p1[1] + w2*p2[1] + w3*p3[1],
		})
	}
	return pts
}

// subPath 截取路径的 [s,e] 参数段(s/e ∈ [0,1]),用于"残影拖尾"。
func subPath(pts [][2]float64, s, e float64) [][2]float64 {
	if len(pts) < 2 || e <= s {
		return nil
	}
	s = clamp01(s)
	e = clamp01(e)
	n := len(pts) - 1
	out := make([][2]float64, 0, n+2)
	fs := s * float64(n)
	fe := e * float64(n)
	i0 := int(math.Floor(fs))
	i1 := int(math.Ceil(fe))
	if i1 > n {
		i1 = n
	}
	out = append(out, interpPath(pts, fs))
	for i := i0 + 1; i < i1; i++ {
		out = append(out, pts[i])
	}
	out = append(out, interpPath(pts, fe))
	if len(out) < 2 {
		return nil
	}
	return out
}

func interpPath(pts [][2]float64, f float64) [2]float64 {
	n := len(pts) - 1
	if f <= 0 {
		return pts[0]
	}
	if f >= float64(n) {
		return pts[n]
	}
	i := int(math.Floor(f))
	t := f - float64(i)
	return [2]float64{
		pts[i][0] + (pts[i+1][0]-pts[i][0])*t,
		pts[i][1] + (pts[i+1][1]-pts[i][1])*t,
	}
}
