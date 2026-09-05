package main

// monsters_verify.go —— 产物自检:把刚写到磁盘上的 PNG **重新解码**再断言,
// 不跑 Unity 也能保证「尺寸 / 通道 / 帧数 / 透明度 / 脚底基线 / 镜像一致性」都对。
//
// 致命项 → 返回 error(工具退出码非 0);非致命项 → 进 warnings 列表并写进 MONSTER_MANIFEST.json。

import (
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
)

// monMaxSilhouetteIoU 任意两只怪 idle 首帧归一化剪影允许的最大 IoU。
// 复审实测:同模板只换色时 野狼/狐妖、山鬼/山贼 = 0.80,其余配对 0.33~0.48;阈值取 0.6 把"同模板换色"挡掉。
const monMaxSilhouetteIoU = 0.60

// monSilW / monSilH 剪影归一化重采样的网格(与复审用的口径一致:24×32)。
const (
	monSilW = 24
	monSilH = 32
)

// silhouetteGrid 取一张帧条第 0 帧 alpha>=128 的 mask,按其包围盒归一到 monSilW×monSilH(面积覆盖率 ≥ 0.5 记 1)。
func silhouetteGrid(img *image.NRGBA) ([]bool, int, error) {
	x0, y0, x1, y1 := monFrameSize, monFrameSize, -1, -1
	for y := 0; y < monFrameSize; y++ {
		for x := 0; x < monFrameSize; x++ {
			if img.Pix[img.PixOffset(x, y)+3] >= 128 {
				if x < x0 {
					x0 = x
				}
				if x > x1 {
					x1 = x
				}
				if y < y0 {
					y0 = y
				}
				if y > y1 {
					y1 = y
				}
			}
		}
	}
	if x1 < 0 {
		return nil, 0, fmt.Errorf("第 0 帧没有 alpha>=128 的像素")
	}
	bw, bh := float64(x1-x0+1), float64(y1-y0+1)
	grid := make([]bool, monSilW*monSilH)
	filled := 0
	for gy := 0; gy < monSilH; gy++ {
		sy0 := y0 + int(float64(gy)*bh/monSilH)
		sy1 := y0 + int(float64(gy+1)*bh/monSilH)
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for gx := 0; gx < monSilW; gx++ {
			sx0 := x0 + int(float64(gx)*bw/monSilW)
			sx1 := x0 + int(float64(gx+1)*bw/monSilW)
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			on, tot := 0, 0
			for y := sy0; y < sy1 && y <= y1; y++ {
				for x := sx0; x < sx1 && x <= x1; x++ {
					tot++
					if img.Pix[img.PixOffset(x, y)+3] >= 128 {
						on++
					}
				}
			}
			if tot > 0 && float64(on)/float64(tot) >= 0.5 {
				grid[gy*monSilW+gx] = true
				filled++
			}
		}
	}
	return grid, filled, nil
}

func gridIoU(a, b []bool) float64 {
	inter, union := 0, 0
	for i := range a {
		if a[i] && b[i] {
			inter++
		}
		if a[i] || b[i] {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// verifySilhouettes 两两比对本批怪 idle 首帧的归一化剪影,任一对 IoU > monMaxSilhouetteIoU 即失败。
func verifySilhouettes(outRoot string, roster []monsterSpec) (map[string]any, error) {
	type sil struct {
		sp   monsterSpec
		grid []bool
	}
	var sils []sil
	for _, sp := range roster {
		p := filepath.Join(outRoot, "Monsters", fmt.Sprintf("%d", sp.ID), "idle_E_strip.png")
		img, err := decodeNRGBA(p)
		if err != nil {
			return nil, err
		}
		g, _, err := silhouetteGrid(img)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		sils = append(sils, sil{sp, g})
	}
	pairs := []map[string]any{}
	worst, worstPair := 0.0, ""
	var bad []string
	for i := 0; i < len(sils); i++ {
		for j := i + 1; j < len(sils); j++ {
			v := gridIoU(sils[i].grid, sils[j].grid)
			name := fmt.Sprintf("%d %s vs %d %s", sils[i].sp.ID, sils[i].sp.Name, sils[j].sp.ID, sils[j].sp.Name)
			pairs = append(pairs, map[string]any{
				"pair": name, "iou": fmt.Sprintf("%.3f", v),
				"same_template": sils[i].sp.Template == sils[j].sp.Template,
			})
			if v > worst {
				worst, worstPair = v, name
			}
			if v > monMaxSilhouetteIoU {
				bad = append(bad, fmt.Sprintf("%s IoU=%.3f", name, v))
			}
		}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a]["iou"].(string) > pairs[b]["iou"].(string) })
	if len(bad) > 0 {
		return nil, fmt.Errorf("怪物剪影过于相似(归一化 %d×%d 剪影 IoU > %.2f):%v", monSilW, monSilH, monMaxSilhouetteIoU, bad)
	}
	fmt.Printf("  剪影自检:%d 对两两 IoU 全部 ≤ %.2f,最相似 %s = %.3f\n", len(pairs), monMaxSilhouetteIoU, worstPair, worst)
	return map[string]any{
		"grid":       fmt.Sprintf("%dx%d", monSilW, monSilH),
		"alpha_min":  128,
		"max_iou":    monMaxSilhouetteIoU,
		"worst_pair": worstPair,
		"worst_iou":  fmt.Sprintf("%.3f", worst),
		"pairs":      pairs,
	}, nil
}

// monVerifyStat 一张帧条的统计。
type monVerifyStat struct {
	File        string `json:"file"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Frames      int    `json:"frames"`
	MinOpaque   int    `json:"min_opaque_px_per_frame"` // 帧内 alpha≥8 的像素数的最小值
	FullyOpaque int    `json:"fully_opaque_px"`         // alpha==255 的像素数(证明不是一张半透明糊图)
	FullyClear  int    `json:"fully_clear_px"`          // alpha==0 的像素数(证明是真透明背景,不是黑底)
	BottomRow0  int    `json:"bottom_row_frame0"`       // 第 0 帧最低不透明行
}

// decodeNRGBA 打开并解码 PNG,强制要求是直通 alpha 的 NRGBA。
func decodeNRGBA(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("自检打开 %s 失败: %w", path, err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("自检解码 %s 失败: %w", path, err)
	}
	n, ok := img.(*image.NRGBA)
	if !ok {
		return nil, fmt.Errorf("%s 不是 NRGBA(直通 alpha),实际 %T", path, img)
	}
	return n, nil
}

// verifyMonsterStrip 校验一张 8 帧帧条。
func verifyMonsterStrip(path string, wantBottom int, isIdle bool) (monVerifyStat, []string, error) {
	var st monVerifyStat
	var warns []string
	img, err := decodeNRGBA(path)
	if err != nil {
		return st, nil, err
	}
	b := img.Bounds()
	st.File = filepath.ToSlash(path)
	st.Width, st.Height = b.Dx(), b.Dy()

	// ① 尺寸 / 帧数
	if b.Dx() != monFrameSize*monFrames || b.Dy() != monFrameSize {
		return st, nil, fmt.Errorf("%s 尺寸 %dx%d,期望 %dx%d",
			path, b.Dx(), b.Dy(), monFrameSize*monFrames, monFrameSize)
	}
	st.Frames = b.Dx() / b.Dy() // 客户端 LoadStrip(cell=0) 就是这么切的
	if st.Frames != monFrames {
		return st, nil, fmt.Errorf("%s 按贴图高切格得 %d 帧,期望 %d", path, st.Frames, monFrames)
	}

	st.MinOpaque = 1 << 30
	edge := 0
	for f := 0; f < monFrames; f++ {
		ox := f * monFrameSize
		opaque, bottom := 0, -1
		for y := 0; y < monFrameSize; y++ {
			for x := 0; x < monFrameSize; x++ {
				a := img.Pix[img.PixOffset(ox+x, y)+3]
				switch {
				case a == 0:
					st.FullyClear++
				case a == 255:
					st.FullyOpaque++
				}
				if a >= 8 {
					opaque++
					if y > bottom {
						bottom = y
					}
					// ③ 越界:贴到画布四边说明被裁
					if x == 0 || x == monFrameSize-1 || y == 0 || y == monFrameSize-1 {
						edge++
					}
				}
			}
		}
		// ② 非全透明
		if opaque == 0 {
			return st, nil, fmt.Errorf("%s 第 %d 帧全透明", path, f)
		}
		if opaque < st.MinOpaque {
			st.MinOpaque = opaque
		}
		if f == 0 {
			st.BottomRow0 = bottom
		}
	}
	// ④ 真实 alpha:既要有全透明背景,也要有实心像素
	if st.FullyClear == 0 {
		return st, nil, fmt.Errorf("%s 没有一个 alpha=0 的像素(不是透明背景)", path)
	}
	if st.FullyOpaque == 0 {
		return st, nil, fmt.Errorf("%s 没有一个 alpha=255 的像素(整张半透明,描边没落实)", path)
	}
	// ⑤ 脚底基线:idle 第 0 帧(dx=dy=0 的中立姿势)最低实体行应落在该怪的目标行上,容差 ±1px
	if isIdle {
		if d := st.BottomRow0 - wantBottom; d < -1 || d > 1 {
			return st, nil, fmt.Errorf("%s idle 首帧脚底在第 %d 行,期望第 %d 行(±1)",
				path, st.BottomRow0, wantBottom)
		}
	}
	if edge > 0 {
		warns = append(warns, fmt.Sprintf("%s:画布边缘有 %d 个实体像素(造型被裁到边)",
			filepath.ToSlash(path), edge))
	}
	return st, warns, nil
}

// verifyMirrorPair 断言 W 向就是 E 向的逐格水平镜像(逐像素比对,允许 0 误差)。
func verifyMirrorPair(east, west string) error {
	e, err := decodeNRGBA(east)
	if err != nil {
		return err
	}
	w, err := decodeNRGBA(west)
	if err != nil {
		return err
	}
	if e.Bounds() != w.Bounds() {
		return fmt.Errorf("镜像对尺寸不一致:%s vs %s", east, west)
	}
	for c := 0; c < monFrames; c++ {
		x0 := c * monFrameSize
		for y := 0; y < monFrameSize; y++ {
			for x := 0; x < monFrameSize; x++ {
				eo := e.PixOffset(x0+x, y)
				wo := w.PixOffset(x0+monFrameSize-1-x, y)
				for k := 0; k < 4; k++ {
					if e.Pix[eo+k] != w.Pix[wo+k] {
						return fmt.Errorf("%s 不是 %s 的逐格水平镜像(帧 %d,像素 %d,%d)",
							filepath.Base(west), filepath.Base(east), c, x, y)
					}
				}
			}
		}
	}
	return nil
}

// verifyGroundBand 校验一条地台:尺寸 / 四边留白 / 半透明 / 上暗下亮。
func verifyGroundBand(path string) (map[string]any, error) {
	img, err := decodeNRGBA(path)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() != groundW || b.Dy() != groundH {
		return nil, fmt.Errorf("%s 尺寸 %dx%d,期望 %dx%d", path, b.Dx(), b.Dy(), groundW, groundH)
	}
	maxA, nz := 0, 0
	// 四边 2px 边框必须全透明,否则拉伸时会出硬边
	borderMax := 0
	for y := 0; y < groundH; y++ {
		for x := 0; x < groundW; x++ {
			a := int(img.Pix[img.PixOffset(x, y)+3])
			if a > maxA {
				maxA = a
			}
			if a > 0 {
				nz++
			}
			if x < 2 || y < 2 || x >= groundW-2 || y >= groundH-2 {
				if a > borderMax {
					borderMax = a
				}
			}
		}
	}
	if nz == 0 {
		return nil, fmt.Errorf("%s 整张全透明", path)
	}
	if borderMax > 2 {
		return nil, fmt.Errorf("%s 外边框 2px 内有 alpha=%d 的像素(羽化没收干净,拉伸会出硬边)", path, borderMax)
	}
	if maxA >= 250 {
		return nil, fmt.Errorf("%s 峰值 alpha=%d,不是半透明光带", path, maxA)
	}

	// 上暗下亮:取带体竖向中线上 1/4 与下 1/4 的平均亮度对比
	lum := func(y0, y1 int) float64 {
		sum, n := 0.0, 0.0
		for y := y0; y < y1; y++ {
			for x := groundW / 4; x < groundW*3/4; x++ {
				o := img.PixOffset(x, y)
				if img.Pix[o+3] < 24 {
					continue
				}
				sum += 0.299*float64(img.Pix[o]) + 0.587*float64(img.Pix[o+1]) + 0.114*float64(img.Pix[o+2])
				n++
			}
		}
		if n == 0 {
			return 0
		}
		return sum / n
	}
	top := lum(groundH/2-140, groundH/2-40)
	bot := lum(groundH/2+40, groundH/2+140)
	if bot <= top {
		return nil, fmt.Errorf("%s 上亮下暗(上 %.1f / 下 %.1f),与'上暗下亮'规格相反", path, top, bot)
	}
	return map[string]any{
		"file": filepath.ToSlash(path), "width": groundW, "height": groundH,
		"max_alpha": maxA, "border2px_max_alpha": borderMax,
		"nonzero_alpha_px": nz,
		"luma_top_quarter": fmt.Sprintf("%.1f", top),
		"luma_bot_quarter": fmt.Sprintf("%.1f", bot),
	}, nil
}

// verifyMonsterOutput 遍历本次产出的全部帧条 + 两条地台做自检。
func verifyMonsterOutput(outRoot string, roster []monsterSpec) (map[string]any, error) {
	var warns []string
	stats := make([]monVerifyStat, 0, len(roster)*len(monActions)*2)
	strips, mirrors := 0, 0

	for _, sp := range roster {
		dir := filepath.Join(outRoot, "Monsters", fmt.Sprintf("%d", sp.ID))
		wantBottom := monBottomTarget(sp)
		for _, act := range monActions {
			east := filepath.Join(dir, act.Name+"_E_strip.png")
			west := filepath.Join(dir, act.Name+"_W_strip.png")
			paths := []string{east}
			if monWantsDir("W") {
				paths = append(paths, west)
			} else if _, err := os.Stat(west); err == nil {
				return nil, fmt.Errorf("%s 不该存在:-monster-dirs 未含 W 时 W 由客户端镜像,陈旧 W 会被优先命中", west)
			}
			for _, p := range paths {
				st, w, err := verifyMonsterStrip(p, wantBottom, act.Name == "idle")
				if err != nil {
					return nil, err
				}
				stats = append(stats, st)
				warns = append(warns, w...)
				strips++
			}
			if monWantsDir("W") {
				if err := verifyMirrorPair(east, west); err != nil {
					return nil, err
				}
				mirrors++
			}
		}
		// meta.json 必须存在且可读
		if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
			return nil, fmt.Errorf("怪物 %d 缺 meta.json: %w", sp.ID, err)
		}
	}

	silhouettes, err := verifySilhouettes(outRoot, roster)
	if err != nil {
		return nil, err
	}

	grounds := make([]map[string]any, 0, 2)
	for _, s := range groundBandSpecs() {
		g, err := verifyGroundBand(filepath.Join(outRoot, "UI", s.File+".png"))
		if err != nil {
			return nil, err
		}
		grounds = append(grounds, g)
	}

	// 汇总
	minOpaque, maxEdgeWarn := 1<<30, len(warns)
	bottoms := map[string]int{}
	for _, s := range stats {
		if s.MinOpaque < minOpaque {
			minOpaque = s.MinOpaque
		}
		bottoms[filepath.ToSlash(s.File)] = s.BottomRow0
	}
	fmt.Printf("  自检:%d 张帧条(全部 %d×%d / 8 帧 / 真 alpha)、%d 组镜像逐像素一致(只在含 W 时比)、%d 条地台;告警 %d 条\n",
		strips, monFrameSize*monFrames, monFrameSize, mirrors, len(grounds), maxEdgeWarn)

	return map[string]any{
		"strips_checked":          strips,
		"directions":              monsterDirs,
		"mirror_pairs_checked":    mirrors,
		"ground_bands_checked":    len(grounds),
		"expected_strip_size":     fmt.Sprintf("%dx%d", monFrameSize*monFrames, monFrameSize),
		"frames_per_strip":        monFrames,
		"min_opaque_px_any_frame": minOpaque,
		"feet_baseline_row":       monBaselineRow,
		"feet_baseline_tolerance": 1,
		"spirit_hover_px":         monHoverPx("spirit"),
		"assertions": []string{
			"尺寸 = 2048×256(8 帧 × 256)",
			"宽 / 高 = 8(客户端 LoadStrip(cell=0) 的切帧口径)",
			"每帧 alpha≥8 的像素数 > 0(不是空帧)",
			"同时存在 alpha=0 与 alpha=255 的像素(真透明背景 + 实心厚描边)",
			fmt.Sprintf("idle 首帧最低实体行 = %d±1(灵体模板整体抬高 %.0fpx)", monBaselineRow, monHoverPx("spirit")),
			"含 W 时:W 向 = E 向逐格水平镜像(逐像素含 RGB,零误差);不含 W 时:目录里不得残留 W",
			fmt.Sprintf("任意两只怪 idle 首帧归一化剪影(%d×%d,alpha>=128)IoU ≤ %.2f", monSilW, monSilH, monMaxSilhouetteIoU),
			"每只怪的 meta.json 存在",
			"地台 1024×512 / 外 2px 全透明 / 峰值 alpha<250 / 下 1/4 亮于上 1/4",
		},
		"warnings":                   warns,
		"silhouette":                 silhouettes,
		"ground":                     grounds,
		"per_strip_bottom_row_of_f0": bottoms,
		"strip_stats":                stats,
	}, nil
}
