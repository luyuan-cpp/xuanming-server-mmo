package main

// buffs.go —— 64×64 buff 图标:圆角徽章底 + 厚描边 + 顶部高光 + 中央符号。
// id 取自 generated/tables/Buff.json;底色按 buff_type 归类(见 categorizeBuffTypes 注释)。

import (
	"encoding/json"
	"fmt"
	"image"
	"math"
	"os"
	"sort"
)

const buffIconSize = 64

type buffRow struct {
	ID       int `json:"id"`
	BuffType int `json:"buff_type"`
}

type buffTableFile struct {
	Data []buffRow `json:"data"`
}

// BuffIconInfo 写进 manifest 的单个图标信息。
type BuffIconInfo struct {
	ID       int    `json:"id"`
	File     string `json:"file"`
	BuffType int    `json:"buff_type"`
	Category string `json:"category"`
	Symbol   string `json:"symbol"`
	Source   string `json:"source"`
}

type buffPalette struct {
	name   string
	top    RGB
	bottom RGB
	rim    RGB
}

var buffPalettes = map[string]buffPalette{
	"buff":    {"buff", rgb8(120, 226, 134), rgb8(26, 116, 62), rgb8(186, 255, 196)},
	"debuff":  {"debuff", rgb8(238, 118, 98), rgb8(138, 26, 30), rgb8(255, 192, 172)},
	"control": {"control", rgb8(180, 134, 240), rgb8(72, 38, 140), rgb8(224, 198, 255)},
}

var buffCategoryOrder = []string{"buff", "debuff", "control"}

var buffSymbolNames = []string{"arrow_up", "arrow_down", "chain", "flame", "ice", "shield", "sword", "leaf"}

// loadBuffIDs 读表取前 want 个 id;表里不足时用连续 id 补齐(补齐项在 manifest 里标 source=padding)。
func loadBuffIDs(path string, want int) (rows []buffRow, pad int, tableNote string, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		// 表读不到:退化成 1..want
		for i := 1; i <= want; i++ {
			rows = append(rows, buffRow{ID: i, BuffType: i})
		}
		return rows, want, fmt.Sprintf("未能读取 %s(%v),id 退化为 1..%d", path, rerr, want), nil
	}
	var tf buffTableFile
	if err = json.Unmarshal(data, &tf); err != nil {
		return nil, 0, "", fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	n := len(tf.Data)
	if n > want {
		n = want
	}
	rows = append(rows, tf.Data[:n]...)
	maxID := 0
	for _, r := range rows {
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	for len(rows) < want {
		maxID++
		rows = append(rows, buffRow{ID: maxID, BuffType: 0})
		pad++
	}
	tableNote = fmt.Sprintf("Buff.json 共 %d 行,取前 %d 行 id;不足 %d 个,用连续 id 补齐 %d 个占位图标",
		len(tf.Data), n, want, pad)
	return rows, pad, tableNote, nil
}

// categorizeBuffTypes 决定每个 buff_type 属于哪种底色。
//
// Buff.json 有 buff_type 字段,但**没有"增益/减益/控制"的极性语义**(proto 里也没有对应 enum),
// 因此这里用一个确定性映射:把出现过的 buff_type 去重升序排列,按序号 %3 依次分配
// 增益绿 / 减益红 / 控制紫。规则写进 ART_MANIFEST.json 的 note,后续表里补了极性字段再改这里即可。
func categorizeBuffTypes(rows []buffRow) (map[int]string, string) {
	seen := map[int]bool{}
	var types []int
	for _, r := range rows {
		if !seen[r.BuffType] {
			seen[r.BuffType] = true
			types = append(types, r.BuffType)
		}
	}
	sort.Ints(types)
	m := make(map[int]string, len(types))
	var pairs []string
	for i, t := range types {
		c := buffCategoryOrder[i%3]
		m[t] = c
		pairs = append(pairs, fmt.Sprintf("%d→%s", t, c))
	}
	note := "Buff.json 无极性字段(buff_type 仅为数值),底色按 distinct buff_type 升序 %3 分配:" +
		joinStrings(pairs, ", ")
	return m, note
}

func joinStrings(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}

// makeBuffIcon 画一张 64×64 图标。
func makeBuffIcon(pal buffPalette, symbol int) *image.NRGBA {
	const S = buffIconSize
	l := NewLayer(S, S)

	badge := NewMask(S, S)
	badge.RoundRect(5, 5, 59, 59, 15)

	// 厚描边
	l.FillFlat(badge.Dilate(3.0), rgb8(24, 17, 28), 1.0)

	// 底色竖向渐变
	l.FillOver(badge, func(x, y int) (RGB, float64) {
		fy := clamp01((float64(y) - 5) / 54)
		c := mixRGB(pal.top, pal.bottom, math.Pow(fy, 0.8))
		n := noise2(float64(x)*0.21, float64(y)*0.21)
		return c.Mul(0.97 + 0.06*n), 1.0
	})

	// 内圈亮边
	innerCut := NewMask(S, S)
	innerCut.RoundRect(8.5, 8.5, 55.5, 55.5, 12)
	l.FillFlat(badge.Subtract(innerCut), pal.rim, 0.55)

	// 顶部高光
	gloss := NewMask(S, S)
	gloss.Ellipse(32, 17, 22, 9)
	gloss = gloss.Intersect(innerCut).Blur(2)
	l.FillFlat(gloss, rgb8(255, 255, 255), 0.30)

	// 底部内阴影
	shade := NewMask(S, S)
	shade.Ellipse(32, 62, 26, 11)
	shade = shade.Intersect(innerCut).Blur(3)
	l.FillFlat(shade, rgb8(0, 0, 0), 0.28)

	// 中央符号
	sym := symbolMask(symbol)
	l.FillFlat(sym.Dilate(2.6), rgb8(26, 18, 30), 0.92)
	l.FillOver(sym, func(x, y int) (RGB, float64) {
		fy := clamp01((float64(y) - 12) / 42)
		return mixRGB(rgb8(255, 255, 255), rgb8(226, 232, 240), fy), 1.0
	})
	l.FillAdd(sym.Blur(2).Intersect(sym), rgb8(255, 255, 255), 0.25)

	return l.ToNRGBA()
}

// symbolMask 生成中央符号遮罩(0..7 循环)。
func symbolMask(kind int) *Mask {
	const S = buffIconSize
	m := NewMask(S, S)
	switch kind % 8 {
	case 0: // 上箭头
		m.Poly([][2]float64{{32, 13}, {47, 31}, {39, 31}, {39, 51}, {25, 51}, {25, 31}, {17, 31}})
	case 1: // 下箭头
		m.Poly([][2]float64{{32, 51}, {47, 33}, {39, 33}, {39, 13}, {25, 13}, {25, 33}, {17, 33}})
	case 2: // 锁链
		ringMask(m, 24, 26, 13, 4.8)
		ringMask(m, 41, 40, 13, 4.8)
	case 3: // 火焰:斜尖顶 + 鼓腹 + 底部深 V 双瓣
		var flame [][2]float64
		flame = append(flame, bezier3([2]float64{37, 6}, [2]float64{41, 15}, [2]float64{48, 27}, [2]float64{45, 39}, 14)...)
		flame = append(flame, bezier3([2]float64{45, 39}, [2]float64{44, 49}, [2]float64{38, 55}, [2]float64{33, 56}, 10)...)
		flame = append(flame, [2]float64{30, 40})
		flame = append(flame, [2]float64{25, 56})
		flame = append(flame, bezier3([2]float64{25, 56}, [2]float64{16, 51}, [2]float64{13, 38}, [2]float64{20, 27}, 12)...)
		flame = append(flame, bezier3([2]float64{20, 27}, [2]float64{25, 21}, [2]float64{29, 15}, [2]float64{37, 6}, 10)...)
		m.Poly(flame)
	case 4: // 冰晶
		for i := 0; i < 3; i++ {
			a := float64(i)/3*math.Pi + math.Pi/2
			dx, dy := math.Cos(a)*21, math.Sin(a)*21
			m.Line(32-dx, 32-dy, 32+dx, 32+dy, 5.0)
			// 分叉
			for _, s := range []float64{1, -1} {
				bx, by := 32+dx*0.62, 32+dy*0.62
				ba := a + s*0.9
				m.Line(bx, by, bx+math.Cos(ba)*8, by+math.Sin(ba)*8, 3.4)
				bx2, by2 := 32-dx*0.62, 32-dy*0.62
				ba2 := a + math.Pi + s*0.9
				m.Line(bx2, by2, bx2+math.Cos(ba2)*8, by2+math.Sin(ba2)*8, 3.4)
			}
		}
	case 5: // 盾
		m.Poly([][2]float64{{32, 12}, {49, 19}, {49, 34}, {32, 53}, {15, 34}, {15, 19}})
		notch := NewMask(S, S)
		notch.Line(32, 20, 32, 46, 3.0)
		res := m.Subtract(notch)
		copy(m.A, res.A)
	case 6: // 剑
		m.Poly([][2]float64{{32, 9}, {38, 20}, {38, 40}, {26, 40}, {26, 20}})
		m.RoundRect(18, 40, 46, 46, 2.5)
		m.RoundRect(28.5, 46, 35.5, 56, 2.5)
	default: // 叶:斜置叶片 + 叶柄 + 叶脉
		lf := bezier3([2]float64{16, 50}, [2]float64{14, 26}, [2]float64{30, 12}, [2]float64{50, 12}, 18)
		rf := bezier3([2]float64{50, 12}, [2]float64{50, 32}, [2]float64{38, 48}, [2]float64{16, 50}, 18)
		m.Poly(append(lf, rf...))
		m.Line(10, 56, 20, 46, 3.4)
		rib := NewMask(S, S)
		rib.Polyline([][2]float64{{18, 48}, {30, 36}, {46, 16}}, 2.4)
		for _, s := range []float64{0.34, 0.55, 0.76} {
			bx := 18 + (46-18)*s
			by := 48 + (16-48)*s
			rib.Line(bx, by, bx+9*(1-s)+4, by-2, 1.8)
			rib.Line(bx, by, bx-3, by+8*(1-s)+3, 1.8)
		}
		res := m.Subtract(rib)
		copy(m.A, res.A)
	}
	return m
}

func ringMask(m *Mask, cx, cy, r, w float64) {
	outer := NewMask(m.W, m.H)
	outer.Circle(cx, cy, r)
	inner := NewMask(m.W, m.H)
	inner.Circle(cx, cy, r-w)
	ring := outer.Subtract(inner)
	for i := range m.A {
		if ring.A[i] > m.A[i] {
			m.A[i] = ring.A[i]
		}
	}
}
