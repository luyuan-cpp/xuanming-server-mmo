package main

// ui.go —— 战斗 UI 底板:九宫面板 / 按钮 / 血条槽 / 命令环。
// 统一风格:厚重深色描边 + 金边斜切高光 + 内部柔和渐变,拉伸区保持横/纵向均匀,九宫不糊。

import (
	"image"
	"math"
)

var (
	goldLight = rgb8(255, 231, 152)
	goldMid   = rgb8(216, 163, 55)
	goldDark  = rgb8(124, 82, 22)
	inkDark   = rgb8(26, 15, 10)
	woodTop   = rgb8(92, 57, 35)
	woodBot   = rgb8(56, 32, 20)
	jadeTop   = rgb8(222, 248, 232)
	jadeMid   = rgb8(150, 214, 180)
	jadeBot   = rgb8(84, 158, 130)
)

func noise2(x, y float64) float64 {
	s := math.Sin(x*127.1+y*311.7) * 43758.5453
	return s - math.Floor(s)
}

// goldBevel 给金边一个"外暗 → 中亮 → 内暗"的斜切质感。k 为从外沿向内的归一化深度。
func goldBevel(k, vertical float64) RGB {
	b := math.Pow(math.Sin(math.Pi*clamp01(k)), 0.75)
	c := mixRGB(goldDark, goldLight, b)
	c = mixRGB(c, goldMid, 0.35)
	// 顶部受光、底部压暗
	return c.Mul(1.10 - 0.30*clamp01(vertical))
}

// makePanel9Slice 256×256,圆角金边 + 深棕木纹底,九宫边距 64。
func makePanel9Slice() *image.NRGBA {
	const S = 256
	l := NewLayer(S, S)

	const (
		x0, y0, x1, y1 = 5.0, 5.0, 251.0, 251.0
		radius         = 26.0
		bw             = 11.0 // 金边宽度
	)
	outer := NewMask(S, S)
	outer.RoundRect(x0, y0, x1, y1, radius)
	inner := NewMask(S, S)
	inner.RoundRect(x0+bw, y0+bw, x1-bw, y1-bw, radius-bw*0.55)
	innerInset := NewMask(S, S)
	innerInset.RoundRect(x0+bw+4, y0+bw+4, x1-bw-4, y1-bw-4, math.Max(radius-bw*0.55-4, 2))

	// 厚描边
	l.FillFlat(outer.Dilate(3.0), inkDark, 1.0)

	// 金边
	band := outer.Subtract(inner)
	cx, cy := (x0+x1)/2, (y0+y1)/2
	hw, hh := (x1-x0)/2, (y1-y0)/2
	l.FillOver(band, func(x, y int) (RGB, float64) {
		sd := sdRoundRect(float64(x)+0.5, float64(y)+0.5, cx, cy, hw, hh, radius)
		k := clamp01(-sd / bw)
		return goldBevel(k, float64(y)/S), 1.0
	})

	// 木纹底
	l.FillOver(inner, func(x, y int) (RGB, float64) {
		fy := float64(y) / S
		base := mixRGB(woodTop, woodBot, smoothstep(0.05, 0.95, fy))
		grain := 0.5 + 0.5*math.Sin(float64(y)*0.62+1.7*math.Sin(float64(y)*0.11))
		n := noise2(float64(y)*0.37, 7.3)
		k := 1.0 + 0.055*(grain-0.5)*2 + 0.030*(n-0.5)*2
		return base.Mul(k), 1.0
	})

	// 内嵌暗线(木框内的凹槽)
	l.FillOver(inner.Subtract(innerInset), func(x, y int) (RGB, float64) {
		return rgb8(34, 19, 12), 0.85
	})

	// 四角铆钉(落在 64px 角区内,九宫拉伸不会变形)
	for _, p := range [][2]float64{{28, 28}, {228, 28}, {28, 228}, {228, 228}} {
		rv := NewMask(S, S)
		rv.Circle(p[0], p[1], 7.5)
		l.FillFlat(rv.Dilate(2.2), inkDark, 1.0)
		l.FillOver(rv, func(x, y int) (RGB, float64) {
			d := math.Hypot(float64(x)+0.5-p[0], float64(y)+0.5-p[1]) / 7.5
			return mixRGB(goldLight, goldDark, clamp01(d*d)), 1.0
		})
		hl := NewMask(S, S)
		hl.Circle(p[0]-2, p[1]-2.4, 3.0)
		l.FillFlat(hl, rgb8(255, 250, 220), 0.55)
	}

	return l.ToNRGBA()
}

// makeButton9Slice 160×64,金边 + 玉色面,九宫边距 22。
func makeButton9Slice() *image.NRGBA {
	const W, H = 160, 64
	l := NewLayer(W, H)
	const (
		x0, y0, x1, y1 = 4.0, 4.0, 156.0, 60.0
		radius         = 16.0
		bw             = 6.0
	)
	outer := NewMask(W, H)
	outer.RoundRect(x0, y0, x1, y1, radius)
	inner := NewMask(W, H)
	inner.RoundRect(x0+bw, y0+bw, x1-bw, y1-bw, radius-bw*0.6)

	l.FillFlat(outer.Dilate(2.6), inkDark, 1.0)

	cx, cy := (x0+x1)/2, (y0+y1)/2
	hw, hh := (x1-x0)/2, (y1-y0)/2
	l.FillOver(outer.Subtract(inner), func(x, y int) (RGB, float64) {
		sd := sdRoundRect(float64(x)+0.5, float64(y)+0.5, cx, cy, hw, hh, radius)
		return goldBevel(clamp01(-sd/bw), float64(y)/H), 1.0
	})

	l.FillOver(inner, func(x, y int) (RGB, float64) {
		fy := clamp01((float64(y) - y0 - bw) / (y1 - y0 - 2*bw))
		var c RGB
		if fy < 0.5 {
			c = mixRGB(jadeTop, jadeMid, fy/0.5)
		} else {
			c = mixRGB(jadeMid, jadeBot, (fy-0.5)/0.5)
		}
		return c, 1.0
	})

	// 上半部高光
	gloss := NewMask(W, H)
	gloss.Ellipse(80, 16, 66, 12)
	gloss = gloss.Intersect(inner).Blur(2)
	l.FillFlat(gloss, rgb8(255, 255, 255), 0.34)

	// 底部内阴影
	shadow := NewMask(W, H)
	shadow.RoundRect(x0+bw, y1-bw-7, x1-bw, y1-bw, 4)
	shadow = shadow.Intersect(inner).Blur(2)
	l.FillFlat(shadow, rgb8(38, 88, 74), 0.35)

	return l.ToNRGBA()
}

// makeBarSlot 320×32 血条槽:深色内凹 + 细金框,九宫边距 16/15。
func makeBarSlot() *image.NRGBA {
	const W, H = 320, 32
	l := NewLayer(W, H)
	const (
		x0, y0, x1, y1 = 2.0, 2.0, 318.0, 30.0
		radius         = 14.0
		bw             = 3.5
	)
	outer := NewMask(W, H)
	outer.RoundRect(x0, y0, x1, y1, radius)
	inner := NewMask(W, H)
	inner.RoundRect(x0+bw, y0+bw, x1-bw, y1-bw, radius-bw)

	l.FillFlat(outer.Dilate(2.0), inkDark, 1.0)

	cx, cy := (x0+x1)/2, (y0+y1)/2
	hw, hh := (x1-x0)/2, (y1-y0)/2
	l.FillOver(outer.Subtract(inner), func(x, y int) (RGB, float64) {
		sd := sdRoundRect(float64(x)+0.5, float64(y)+0.5, cx, cy, hw, hh, radius)
		return goldBevel(clamp01(-sd/bw), float64(y)/H), 1.0
	})

	// 内凹槽:顶部最暗,底部略提亮,形成"陷进去"的错觉
	l.FillOver(inner, func(x, y int) (RGB, float64) {
		fy := clamp01((float64(y) - y0 - bw) / (y1 - y0 - 2*bw))
		c := mixRGB(rgb8(18, 11, 10), rgb8(66, 44, 34), math.Pow(fy, 0.75))
		return c, 1.0
	})

	// 顶部投影线
	sh := NewMask(W, H)
	sh.RoundRect(x0+bw, y0+bw, x1-bw, y0+bw+5, 3)
	sh = sh.Intersect(inner).Blur(1)
	l.FillFlat(sh, rgb8(0, 0, 0), 0.45)

	return l.ToNRGBA()
}

// makeCommandRing 512×512,7 扇区命令环底(问道式圆盘命令环)。
func makeCommandRing() *image.NRGBA {
	const S = 512
	const cx, cy = 256.0, 256.0
	const rOut, rIn = 238.0, 96.0
	l := NewLayer(S, S)

	outerC := NewMask(S, S)
	outerC.Circle(cx, cy, rOut)
	innerC := NewMask(S, S)
	innerC.Circle(cx, cy, rIn)
	annulus := outerC.Subtract(innerC)

	// 厚描边
	l.FillFlat(annulus.Dilate(3.2), inkDark, 1.0)

	// 环底
	l.FillOver(annulus, func(x, y int) (RGB, float64) {
		d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
		k := clamp01((d - rIn) / (rOut - rIn))
		c := mixRGB(rgb8(70, 44, 28), rgb8(38, 22, 14), math.Pow(k, 0.8))
		return c, 0.94
	})

	// 7 个扇区面
	const sectors = 7
	step := 2 * math.Pi / sectors
	start := -math.Pi/2 - step/2
	gap := 0.026
	for i := 0; i < sectors; i++ {
		a0 := start + float64(i)*step + gap
		a1 := start + float64(i+1)*step - gap
		sec := NewMask(S, S)
		sec.AnnulusSector(cx, cy, rIn+8, rOut-10, a0, a1)
		mid := (a0 + a1) / 2
		l.FillOver(sec, func(x, y int) (RGB, float64) {
			px := float64(x) + 0.5 - cx
			py := float64(y) + 0.5 - cy
			d := math.Hypot(px, py)
			k := clamp01((d - rIn) / (rOut - rIn))
			// 扇区中线略亮,靠近分隔线压暗
			ang := math.Atan2(py, px)
			da := math.Abs(math.Remainder(ang-mid, 2*math.Pi)) / (step / 2)
			c := mixRGB(rgb8(104, 68, 42), rgb8(52, 31, 20), math.Pow(k, 0.7))
			c = c.Mul(1.06 - 0.22*clamp01(da))
			n := noise2(px*0.09, py*0.09)
			return c.Mul(0.97 + 0.06*n), 1.0
		})
	}

	// 分隔线(从内环拉到外环)
	for i := 0; i < sectors; i++ {
		a := start + float64(i)*step
		sx, sy := cx+math.Cos(a)*(rIn+2), cy+math.Sin(a)*(rIn+2)
		ex, ey := cx+math.Cos(a)*(rOut-2), cy+math.Sin(a)*(rOut-2)
		dl := NewMask(S, S)
		dl.Line(sx, sy, ex, ey, 7.0)
		dl = dl.Intersect(annulus)
		l.FillFlat(dl, inkDark, 0.85)
		gl := NewMask(S, S)
		gl.Line(sx, sy, ex, ey, 2.6)
		gl = gl.Intersect(annulus)
		l.FillFlat(gl, goldMid, 0.8)
	}

	// 外圈 / 内圈金框
	rims := []struct{ r0, r1 float64 }{{rOut - 11, rOut}, {rIn, rIn + 11}}
	for _, rm := range rims {
		a := NewMask(S, S)
		a.Circle(cx, cy, rm.r1)
		b := NewMask(S, S)
		b.Circle(cx, cy, rm.r0)
		ring := a.Subtract(b)
		l.FillOver(ring, func(x, y int) (RGB, float64) {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			k := clamp01((d - rm.r0) / (rm.r1 - rm.r0))
			return goldBevel(math.Sin(math.Pi*k)*0.5+0.25, float64(y)/S), 1.0
		})
	}

	// 扇区外沿刻点
	for i := 0; i < sectors; i++ {
		a := start + (float64(i)+0.5)*step
		px := cx + math.Cos(a)*(rOut-22)
		py := cy + math.Sin(a)*(rOut-22)
		d := NewMask(S, S)
		d.Circle(px, py, 5.0)
		l.FillFlat(d.Dilate(1.6), inkDark, 0.9)
		l.FillFlat(d, goldLight, 0.9)
	}

	return l.ToNRGBA()
}
