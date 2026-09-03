package main

// digits.go —— 伤害数字字集:等宽 48×64 单格横条,顺序 "0123456789-+闪暴击"(15 格)。
// 用 Windows 系统字体(simhei.ttf / msyhbd.ttc)光栅化,加粗(遮罩膨胀)+ 深色厚描边。

import (
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	digitCellW = 48
	digitCellH = 64
)

var digitChars = []rune("0123456789-+闪暴击")

// digitVariant 一套配色 / 尺寸风格。
type digitVariant struct {
	Name      string
	FillTop   RGB
	FillBot   RGB
	Stroke    RGB
	PadX      float64 // 字形外留白(同时是描边可用空间)
	PadY      float64
	OutlineR  float64
	BoldR     float64
	InnerGlow RGB
	GlowA     float64
	Note      string
}

func digitVariants() []digitVariant {
	return []digitVariant{
		{
			Name: "normal", FillTop: rgb8(255, 255, 255), FillBot: rgb8(214, 220, 232),
			Stroke: rgb8(16, 14, 18), PadX: 6, PadY: 7, OutlineR: 3.2, BoldR: 1.0,
			InnerGlow: rgb8(255, 255, 255), GlowA: 0.0,
			Note: "普通伤害:纯白字 + 黑边",
		},
		{
			// 暴击:金黄 + 深红边,缩放更大(留白更小 = 撑满格)
			Name: "crit", FillTop: rgb8(255, 236, 130), FillBot: rgb8(255, 158, 34),
			Stroke: rgb8(104, 14, 10), PadX: 2.5, PadY: 3, OutlineR: 2.6, BoldR: 1.5,
			InnerGlow: rgb8(255, 210, 90), GlowA: 0.45,
			Note: "暴击:金黄渐变 + 深红厚边,字形放大约 1.15 撑满格",
		},
		{
			Name: "heal", FillTop: rgb8(206, 255, 224), FillBot: rgb8(70, 224, 138),
			Stroke: rgb8(8, 68, 40), PadX: 6, PadY: 7, OutlineR: 3.2, BoldR: 1.0,
			InnerGlow: rgb8(140, 255, 190), GlowA: 0.30,
			Note: "治疗:翠绿渐变 + 深绿边",
		},
		{
			Name: "miss", FillTop: rgb8(232, 235, 242), FillBot: rgb8(158, 164, 178),
			Stroke: rgb8(64, 68, 78), PadX: 8, PadY: 10, OutlineR: 2.8, BoldR: 0.6,
			InnerGlow: rgb8(255, 255, 255), GlowA: 0.0,
			Note: "闪避 / 未命中:灰白字 + 灰边,字号略小",
		},
	}
}

// loadFont 加载字体;.ttc 取第一个字体。
func loadFont(explicit string) (*opentype.Font, string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = []string{
			`C:/Windows/Fonts/simhei.ttf`,
			`C:/Windows/Fonts/msyhbd.ttc`,
			`C:/Windows/Fonts/msyh.ttc`,
			`C:/Windows/Fonts/simsun.ttc`,
		}
	}
	var lastErr error
	for _, p := range candidates {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.EqualFold(filepath.Ext(p), ".ttc") {
			col, err := opentype.ParseCollection(data)
			if err != nil {
				lastErr = err
				continue
			}
			f, err := col.Font(0)
			if err != nil {
				lastErr = err
				continue
			}
			return f, p, nil
		}
		f, err := opentype.Parse(data)
		if err != nil {
			lastErr = err
			continue
		}
		return f, p, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用字体候选")
	}
	return nil, "", fmt.Errorf("加载字体失败: %w", lastErr)
}

type glyphMetric struct {
	minX, maxX, minY, maxY float64
}

func measureGlyphs(face font.Face, chars []rune) (metrics []glyphMetric, maxW, ascent, descent float64) {
	metrics = make([]glyphMetric, len(chars))
	for i, r := range chars {
		b, _, ok := face.GlyphBounds(r)
		if !ok {
			continue
		}
		m := glyphMetric{
			minX: float64(b.Min.X) / 64,
			maxX: float64(b.Max.X) / 64,
			minY: float64(b.Min.Y) / 64,
			maxY: float64(b.Max.Y) / 64,
		}
		metrics[i] = m
		if w := m.maxX - m.minX; w > maxW {
			maxW = w
		}
		if a := -m.minY; a > ascent {
			ascent = a
		}
		if d := m.maxY; d > descent {
			descent = d
		}
	}
	return
}

func drawGlyphMask(face font.Face, r rune, m *Mask, penX, baseline float64) {
	dot := fixed.Point26_6{
		X: fixed.Int26_6(math.Round(penX * 64)),
		Y: fixed.Int26_6(math.Round(baseline * 64)),
	}
	dr, gm, maskp, _, ok := face.Glyph(dot, r)
	if !ok || gm == nil {
		return
	}
	for y := dr.Min.Y; y < dr.Max.Y; y++ {
		for x := dr.Min.X; x < dr.Max.X; x++ {
			_, _, _, a := gm.At(maskp.X+(x-dr.Min.X), maskp.Y+(y-dr.Min.Y)).RGBA()
			if a > 0 {
				m.Max(x, y, float64(a)/65535.0)
			}
		}
	}
}

// digitLayout 一组字符共用的字号与基线。
type digitLayout struct {
	face     font.Face
	metrics  map[rune]glyphMetric
	baseline float64
}

// buildDigitLayout 两遍定尺:先用 64px 探测该组字形的最大宽 / 升部 / 降部,再按格内可用框缩放。
func buildDigitLayout(f *opentype.Font, runes []rune, fitW, fitH float64) (*digitLayout, error) {
	const probe = 64.0
	pf, err := opentype.NewFace(f, &opentype.FaceOptions{Size: probe, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	_, pw, pa, pd := measureGlyphs(pf, runes)
	_ = pf.Close()
	if pw <= 0 || pa+pd <= 0 {
		return nil, fmt.Errorf("字体度量失败(缺字形?)")
	}
	size := probe * math.Min(fitW/pw, fitH/(pa+pd))
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	ms, _, asc, desc := measureGlyphs(face, runes)
	m := make(map[rune]glyphMetric, len(runes))
	for i, r := range runes {
		m[r] = ms[i]
	}
	return &digitLayout{
		face:     face,
		metrics:  m,
		baseline: (float64(digitCellH)-(asc+desc))/2 + asc,
	}, nil
}

func isCJK(r rune) bool { return r > 0x2E80 }

// renderDigitStrip 生成一套字集横条。
//
// ASCII(数字与 +/-)与汉字分成两组各自定尺:方块汉字的字宽会把数字压得很小,
// 分组后数字能撑满格子,汉字也不会溢出。组内共用一条基线,格内水平居中。
func renderDigitStrip(f *opentype.Font, v digitVariant) (*image.NRGBA, error) {
	fitW := float64(digitCellW) - 2*v.PadX
	fitH := float64(digitCellH) - 2*v.PadY

	var ascii, cjk []rune
	for _, r := range digitChars {
		if isCJK(r) {
			cjk = append(cjk, r)
		} else {
			ascii = append(ascii, r)
		}
	}
	la, err := buildDigitLayout(f, ascii, fitW, fitH)
	if err != nil {
		return nil, err
	}
	defer la.face.Close()
	lc, err := buildDigitLayout(f, cjk, fitW, fitH)
	if err != nil {
		return nil, err
	}
	defer lc.face.Close()

	stripW := digitCellW * len(digitChars)
	l := NewLayer(stripW, digitCellH)

	for i, r := range digitChars {
		lay := la
		if isCJK(r) {
			lay = lc
		}
		gm := lay.metrics[r]
		gw := gm.maxX - gm.minX
		penX := (float64(digitCellW)-gw)/2 - gm.minX

		cell := NewMask(digitCellW, digitCellH)
		drawGlyphMask(lay.face, r, cell, penX, lay.baseline)
		if v.BoldR > 0 {
			cell = cell.Dilate(v.BoldR)
		}
		outline := cell.Dilate(v.OutlineR)

		ox := i * digitCellW
		// 描边
		for y := 0; y < digitCellH; y++ {
			for x := 0; x < digitCellW; x++ {
				if a := outline.At(x, y); a > 0 {
					l.OverPixel(ox+x, y, v.Stroke, a)
				}
			}
		}
		// 字面(竖向渐变)
		for y := 0; y < digitCellH; y++ {
			fy := clamp01((float64(y) - v.PadY) / math.Max(float64(digitCellH)-2*v.PadY, 1))
			c := mixRGB(v.FillTop, v.FillBot, math.Pow(fy, 0.85))
			for x := 0; x < digitCellW; x++ {
				if a := cell.At(x, y); a > 0 {
					l.OverPixel(ox+x, y, c, a)
				}
			}
		}
		// 内发光(暴击 / 治疗更"亮")
		if v.GlowA > 0 {
			glow := cell.Blur(2)
			for y := 0; y < digitCellH; y++ {
				for x := 0; x < digitCellW; x++ {
					a := glow.At(x, y) * cell.At(x, y)
					if a > 0 {
						l.AddPixel(ox+x, y, v.InnerGlow, a*v.GlowA)
					}
				}
			}
		}
	}
	return l.ToNRGBA(), nil
}
