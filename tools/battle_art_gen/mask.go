package main

// mask.go —— 单通道覆盖率遮罩:UI / buff 图标先做形状,再膨胀出厚描边、填渐变。

import "math"

type Mask struct {
	W, H int
	A    []float64
}

func NewMask(w, h int) *Mask { return &Mask{W: w, H: h, A: make([]float64, w*h)} }

func (m *Mask) At(x, y int) float64 {
	if x < 0 || y < 0 || x >= m.W || y >= m.H {
		return 0
	}
	return m.A[y*m.W+x]
}

// Max 把 v 并入遮罩(取并集)。
func (m *Mask) Max(x, y int, v float64) {
	if x < 0 || y < 0 || x >= m.W || y >= m.H || v <= 0 {
		return
	}
	i := y*m.W + x
	if v > m.A[i] {
		if v > 1 {
			v = 1
		}
		m.A[i] = v
	}
}

func (m *Mask) Clone() *Mask {
	n := NewMask(m.W, m.H)
	copy(n.A, m.A)
	return n
}

// Subtract 返回 m 减去 o 的结果(用于挖空、做圆环)。
func (m *Mask) Subtract(o *Mask) *Mask {
	n := NewMask(m.W, m.H)
	for i := range m.A {
		v := m.A[i] - o.A[i]
		if v < 0 {
			v = 0
		}
		n.A[i] = v
	}
	return n
}

// Intersect 返回交集。
func (m *Mask) Intersect(o *Mask) *Mask {
	n := NewMask(m.W, m.H)
	for i := range m.A {
		n.A[i] = math.Min(m.A[i], o.A[i])
	}
	return n
}

func sdRoundRect(px, py, cx, cy, hw, hh, r float64) float64 {
	qx := math.Abs(px-cx) - (hw - r)
	qy := math.Abs(py-cy) - (hh - r)
	ax := math.Max(qx, 0)
	ay := math.Max(qy, 0)
	return math.Hypot(ax, ay) + math.Min(math.Max(qx, qy), 0) - r
}

// RoundRect 以 (x0,y0)-(x1,y1) 为外框、r 为圆角画抗锯齿圆角矩形。
func (m *Mask) RoundRect(x0, y0, x1, y1, r float64) {
	cx := (x0 + x1) / 2
	cy := (y0 + y1) / 2
	hw := (x1 - x0) / 2
	hh := (y1 - y0) / 2
	if r > hw {
		r = hw
	}
	if r > hh {
		r = hh
	}
	ix0 := int(math.Floor(x0 - 2))
	ix1 := int(math.Ceil(x1 + 2))
	iy0 := int(math.Floor(y0 - 2))
	iy1 := int(math.Ceil(y1 + 2))
	for y := iy0; y <= iy1; y++ {
		for x := ix0; x <= ix1; x++ {
			sd := sdRoundRect(float64(x)+0.5, float64(y)+0.5, cx, cy, hw, hh, r)
			m.Max(x, y, clamp01(0.5-sd))
		}
	}
}

// Ellipse 画抗锯齿椭圆(实心)。
func (m *Mask) Ellipse(cx, cy, rx, ry float64) {
	ix0 := int(math.Floor(cx - rx - 2))
	ix1 := int(math.Ceil(cx + rx + 2))
	iy0 := int(math.Floor(cy - ry - 2))
	iy1 := int(math.Ceil(cy + ry + 2))
	for y := iy0; y <= iy1; y++ {
		for x := ix0; x <= ix1; x++ {
			dx := (float64(x) + 0.5 - cx) / math.Max(rx, 1e-6)
			dy := (float64(y) + 0.5 - cy) / math.Max(ry, 1e-6)
			d := math.Hypot(dx, dy)
			// 近似把归一化距离换回像素距离,保证 1px 抗锯齿带
			scale := math.Min(rx, ry)
			m.Max(x, y, clamp01((1-d)*scale+0.5))
		}
	}
}

func (m *Mask) Circle(cx, cy, r float64) { m.Ellipse(cx, cy, r, r) }

// Line 画带圆头的粗线段。
func (m *Mask) Line(x0, y0, x1, y1, w float64) {
	hw := w / 2
	minX := math.Min(x0, x1) - hw - 2
	maxX := math.Max(x0, x1) + hw + 2
	minY := math.Min(y0, y1) - hw - 2
	maxY := math.Max(y0, y1) + hw + 2
	vx, vy := x1-x0, y1-y0
	den := vx*vx + vy*vy
	for y := int(math.Floor(minY)); y <= int(math.Ceil(maxY)); y++ {
		for x := int(math.Floor(minX)); x <= int(math.Ceil(maxX)); x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			t := 0.0
			if den > 1e-9 {
				t = clamp01(((px-x0)*vx + (py-y0)*vy) / den)
			}
			d := math.Hypot(px-x0-t*vx, py-y0-t*vy)
			m.Max(x, y, clamp01(hw-d+0.5))
		}
	}
}

// Polyline 顺序连线。
func (m *Mask) Polyline(pts [][2]float64, w float64) {
	for i := 0; i+1 < len(pts); i++ {
		m.Line(pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1], w)
	}
}

// Poly 填充多边形(3x3 超采样)。
func (m *Mask) Poly(pts [][2]float64) {
	if len(pts) < 3 {
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
	for y := int(math.Floor(minY)); y <= int(math.Ceil(maxY)); y++ {
		for x := int(math.Floor(minX)); x <= int(math.Ceil(maxX)); x++ {
			if c := polyCoverage(pts, x, y); c > 0 {
				m.Max(x, y, c)
			}
		}
	}
}

// AnnulusSector 画一个环形扇区(命令环用):角度弧度,y 轴向下。
func (m *Mask) AnnulusSector(cx, cy, rIn, rOut, a0, a1 float64) {
	ix0 := int(math.Floor(cx - rOut - 2))
	ix1 := int(math.Ceil(cx + rOut + 2))
	iy0 := int(math.Floor(cy - rOut - 2))
	iy1 := int(math.Ceil(cy + rOut + 2))
	const n = 3
	for y := iy0; y <= iy1; y++ {
		for x := ix0; x <= ix1; x++ {
			hit := 0
			for sy := 0; sy < n; sy++ {
				for sx := 0; sx < n; sx++ {
					px := float64(x) + (float64(sx)+0.5)/n - cx
					py := float64(y) + (float64(sy)+0.5)/n - cy
					d := math.Hypot(px, py)
					if d < rIn || d > rOut {
						continue
					}
					a := math.Atan2(py, px)
					// 归一化到 [a0, a0+2pi)
					for a < a0 {
						a += 2 * math.Pi
					}
					for a >= a0+2*math.Pi {
						a -= 2 * math.Pi
					}
					if a <= a1 {
						hit++
					}
				}
			}
			if hit > 0 {
				m.Max(x, y, float64(hit)/float64(n*n))
			}
		}
	}
}

// Dilate 以半径 r 做保边膨胀(用来生成厚描边)。
func (m *Mask) Dilate(r float64) *Mask {
	out := NewMask(m.W, m.H)
	ir := int(math.Ceil(r)) + 1
	type off struct {
		dx, dy int
		fall   float64
	}
	offs := make([]off, 0, (2*ir+1)*(2*ir+1))
	for dy := -ir; dy <= ir; dy++ {
		for dx := -ir; dx <= ir; dx++ {
			d := math.Hypot(float64(dx), float64(dy))
			if d > r+1 {
				continue
			}
			offs = append(offs, off{dx, dy, math.Max(0, d-r)})
		}
	}
	for y := 0; y < m.H; y++ {
		for x := 0; x < m.W; x++ {
			best := 0.0
			for _, o := range offs {
				v := m.At(x+o.dx, y+o.dy) - o.fall
				if v > best {
					best = v
				}
			}
			out.A[y*out.W+x] = clamp01(best)
		}
	}
	return out
}

// Blur 两次盒式模糊(近似高斯),用于柔光。
func (m *Mask) Blur(r int) *Mask {
	if r <= 0 {
		return m.Clone()
	}
	tmp := NewMask(m.W, m.H)
	out := NewMask(m.W, m.H)
	boxH(m, tmp, r)
	boxV(tmp, out, r)
	boxH(out, tmp, r)
	boxV(tmp, out, r)
	return out
}

func boxH(src, dst *Mask, r int) {
	for y := 0; y < src.H; y++ {
		for x := 0; x < src.W; x++ {
			sum, n := 0.0, 0.0
			for dx := -r; dx <= r; dx++ {
				sum += src.At(x+dx, y)
				n++
			}
			dst.A[y*dst.W+x] = sum / n
		}
	}
}

func boxV(src, dst *Mask, r int) {
	for y := 0; y < src.H; y++ {
		for x := 0; x < src.W; x++ {
			sum, n := 0.0, 0.0
			for dy := -r; dy <= r; dy++ {
				sum += src.At(x, y+dy)
				n++
			}
			dst.A[y*dst.W+x] = sum / n
		}
	}
}

// FillOver 用 shade 回调把遮罩以 source-over 方式画到画布上。
func (l *Layer) FillOver(m *Mask, shade func(x, y int) (RGB, float64)) {
	for y := 0; y < m.H && y < l.H; y++ {
		for x := 0; x < m.W && x < l.W; x++ {
			a := m.A[y*m.W+x]
			if a <= 0 {
				continue
			}
			c, ca := shade(x, y)
			l.OverPixel(x, y, c, a*ca)
		}
	}
}

// FillFlat 纯色填充遮罩。
func (l *Layer) FillFlat(m *Mask, c RGB, alpha float64) {
	l.FillOver(m, func(int, int) (RGB, float64) { return c, alpha })
}

// FillAdd 用遮罩做加法叠加(高光/发光)。
func (l *Layer) FillAdd(m *Mask, c RGB, alpha float64) {
	for y := 0; y < m.H && y < l.H; y++ {
		for x := 0; x < m.W && x < l.W; x++ {
			a := m.A[y*m.W+x]
			if a > 0 {
				l.AddPixel(x, y, c, a*alpha)
			}
		}
	}
}
