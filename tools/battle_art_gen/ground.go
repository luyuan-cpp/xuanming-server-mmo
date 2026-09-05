package main

// ground.go —— 战斗地台:两条 45° 等距的椭圆光带(敌方远带 / 我方近带)。
//
// 作用:让 BattleStage 的"两排交错 + 近大远小"有个视觉落脚点 —— 单位不再悬在纯背景上,
// 而是站在一条被压扁的光带上,压扁比例本身就是 45° 俯视的透视暗示。
//
// 产出(1024×512,真实 alpha,四边留白):
//   Battle/UI/ground_band_far.png   敌方(画面左上)那条,冷色、暗、窄
//   Battle/UI/ground_band_near.png  我方(画面右下)那条,暖色、亮、宽
//
// —— 倾角是怎么定的(见 README「地台怎么摆」)——
// BattleStage.RowStep = (200, -30)(设计坐标 y 向下)→ 排方向在屏幕上是"右上→左下",
// 斜率 dy/dx = -0.15。推荐把本贴图以 Image(Type = Simple)拉伸到 1120×340 设计像素,
// 于是横向被拉 1120/1024 = 1.09375、纵向被压 340/512 = 0.6641。
// 要让拉伸**之后**的长轴斜率正好等于 -0.15,贴图里烘焙的斜率必须是
//
//	-0.15 × 1.09375 / 0.6641 = -0.2471  →  倾角 -13.88°
//
// 所以贴图里直接烘了 -13.88°:按推荐尺寸摆放时 RectTransform 不需要任何旋转。
// 如果以别的长宽比使用,自己按上式反算一个 Z 旋转差值补上即可。

import (
	"fmt"
	"image"
	"math"
	"path/filepath"
)

const (
	groundW = 1024 // 贴图宽
	groundH = 512  // 贴图高

	// groundTiltDeg 烘焙进贴图的长轴倾角(度,负 = 向右上抬)。见文件头推导。
	groundTiltDeg = -13.88

	// groundFitW / groundFitH 推荐的设计像素尺寸(2560×1080 设计坐标),写进清单与 README。
	groundFitW = 1120.0
	groundFitH = 340.0
)

// groundBandSpec 一条光带的参数。
type groundBandSpec struct {
	File    string
	Team    string  // "enemy" / "ally"
	RX, RY  float64 // 椭圆半轴(贴图像素,倾斜前)
	Feather int     // 羽化半径(盒式模糊)
	Top     RGB     // 上沿颜色(暗)
	Bottom  RGB     // 下沿颜色(亮)
	Rim     RGB     // 下沿高光描边色
	Peak    float64 // 中心峰值不透明度(整张图的 alpha 上限,保证"半透明")
	Core    float64 // 内芯加法辉光强度
	RimGain float64 // 下沿高光强度
	Center  []float64
	Desc    string
}

func groundBandSpecs() []groundBandSpec {
	return []groundBandSpec{
		{
			File: "ground_band_far", Team: "enemy",
			RX: 424, RY: 104, Feather: 26,
			// 远带:偏冷的青灰,整体压暗、压薄 —— 远处的地面本来就该灰一点
			Top:    rgb8(28, 40, 62),
			Bottom: rgb8(120, 168, 196),
			Rim:    rgb8(176, 226, 244),
			Peak:   0.44, Core: 0.16, RimGain: 0.22,
			Desc: "敌方(画面左上)那条:冷青灰、窄而暗,配合 0.88 后排缩放读作'远'",
		},
		{
			File: "ground_band_near", Team: "ally",
			RX: 440, RY: 122, Feather: 30,
			// 近带:暖玉金,更亮更厚 —— 近处受光足
			Top:    rgb8(52, 44, 28),
			Bottom: rgb8(224, 196, 128),
			Rim:    rgb8(255, 238, 190),
			Peak:   0.58, Core: 0.24, RimGain: 0.32,
			Desc: "我方(画面右下)那条:暖玉金、宽而亮,压住前排单位的脚底",
		},
	}
}

// rotEllipsePoly 生成一圈倾斜椭圆的多边形点(图像坐标,y 向下;deg<0 = 右端抬高)。
func rotEllipsePoly(cx, cy, rx, ry, deg float64, n int) [][2]float64 {
	a := deg * math.Pi / 180
	ca, sa := math.Cos(a), math.Sin(a)
	pts := make([][2]float64, 0, n)
	for i := 0; i < n; i++ {
		th := 2 * math.Pi * float64(i) / float64(n)
		ex, ey := rx*math.Cos(th), ry*math.Sin(th)
		pts = append(pts, [2]float64{cx + ex*ca - ey*sa, cy + ex*sa + ey*ca})
	}
	return pts
}

// makeGroundBand 画一条光带。
//
// 构成(自下而上):
//  1. 羽化的椭圆本体 —— source-over 上一层"上暗下亮"的竖向渐变;
//  2. 内芯 —— 更小更糊的椭圆做加法辉光,让带心亮起来;
//  3. 下沿高光 —— 椭圆环带(外圈减内圈)再模糊,只在下半圈给增益,读作"受光的地面边缘"。
func makeGroundBand(s groundBandSpec) *image.NRGBA {
	cx, cy := float64(groundW)/2, float64(groundH)/2
	l := NewLayer(groundW, groundH)

	// 1) 本体
	shape := NewMask(groundW, groundH)
	shape.Poly(rotEllipsePoly(cx, cy, s.RX, s.RY, groundTiltDeg, 192))
	soft := shape.Blur(s.Feather)

	// 竖向渐变的取值范围:用倾斜后椭圆的实际上下界,保证渐变正好铺满带体
	a := groundTiltDeg * math.Pi / 180
	halfH := math.Hypot(s.RX*math.Sin(a), s.RY*math.Cos(a))
	y0, y1 := cy-halfH, cy+halfH

	l.FillOver(soft, func(_, y int) (RGB, float64) {
		k := smoothstep(0, 1, clamp01((float64(y)+0.5-y0)/math.Max(y1-y0, 1)))
		return mixRGB(s.Top, s.Bottom, k), s.Peak
	})

	// 2) 内芯加法辉光
	core := NewMask(groundW, groundH)
	core.Poly(rotEllipsePoly(cx, cy+s.RY*0.12, s.RX*0.74, s.RY*0.52, groundTiltDeg, 160))
	coreSoft := core.Blur(s.Feather + 14)
	l.FillAdd(coreSoft, mixRGB(s.Bottom, RGB{1, 1, 1}, 0.25), s.Core)

	// 3) 下沿高光:环带 × 只取下半圈
	inner := NewMask(groundW, groundH)
	inner.Poly(rotEllipsePoly(cx, cy, s.RX-9, s.RY-7, groundTiltDeg, 192))
	rim := shape.Subtract(inner).Blur(5)
	for y := 0; y < groundH; y++ {
		// 下半圈权重:上沿 0 → 下沿 1
		w := smoothstep(0.42, 0.96, clamp01((float64(y)+0.5-y0)/math.Max(y1-y0, 1)))
		if w <= 0 {
			continue
		}
		for x := 0; x < groundW; x++ {
			if v := rim.A[y*groundW+x]; v > 0 {
				l.AddPixel(x, y, s.Rim, v*w*s.RimGain)
			}
		}
	}
	return l.ToNRGBA()
}

// genGroundBands 写出两条地台,并返回 manifest 条目。
func genGroundBands(outRoot string) ([]ArtEntry, error) {
	var out []ArtEntry
	for _, s := range groundBandSpecs() {
		img := makeGroundBand(s)
		rel := filepath.ToSlash(filepath.Join("UI", s.File+".png"))
		if err := savePNG(filepath.Join(outRoot, "UI", s.File+".png"), img); err != nil {
			return nil, err
		}
		out = append(out, ArtEntry{
			Path: rel, Kind: "ground_band", Width: groundW, Height: groundH,
			Pivot: []float64{0.5, 0.5},
			Desc: fmt.Sprintf("%s;倾角 %.2f°(按 %.0f×%.0f 设计像素拉伸后 = BattleStage.RowStep 斜率 -0.15,免旋转);"+
				"Image.Type=Simple 拉伸,勿九宫", s.Desc, groundTiltDeg, groundFitW, groundFitH),
		})
		fmt.Printf("  地台 %-18s %4d×%d  倾角 %.2f°\n", s.File, groundW, groundH, groundTiltDeg)
	}
	return out, nil
}
