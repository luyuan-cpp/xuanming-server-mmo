package main

// battle_art_gen —— 程序化生成回合制战斗表现所需的 PNG 美术。
//
//	go run . -out E:/work/mmorpg-client/Assets/Resources/Battle   # -mode battle:Fx / UI / 数字 / buff
//	go run . -mode characters                                      # 22 张立绘 → 角色帧条(默认只出 E 向,统一身高)
//	go run . -mode monsters                                        # 程序化怪物帧条 + 地台(默认只出 E 向)
//
// 规格见 docs/design/battle-art-prompts.md §3/§4 与 turn-battle-presentation.md §5。
// 只写 .png / .json,不生成 .meta(Unity 导入时自动生成)。

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ArtEntry 是 ART_MANIFEST.json 里的一条产物记录。
type ArtEntry struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Frames     int       `json:"frames,omitempty"`
	CellWidth  int       `json:"cell_width,omitempty"`
	CellHeight int       `json:"cell_height,omitempty"`
	FPSHint    int       `json:"fps_hint,omitempty"`
	Pivot      []float64 `json:"pivot,omitempty"`
	Border     []int     `json:"border_9slice_lbrt,omitempty"`
	Desc       string    `json:"desc,omitempty"`
}

// DigitsMeta 同时写到 UI/digits_meta.json 和 manifest 里。
type DigitsMeta struct {
	CellWidth   int               `json:"cell_width"`
	CellHeight  int               `json:"cell_height"`
	CellCount   int               `json:"cell_count"`
	CharsString string            `json:"chars_string"`
	Chars       []string          `json:"chars"`
	CharIndex   map[string]int    `json:"char_index"`
	Font        string            `json:"font"`
	Variants    []DigitsVariantMD `json:"variants"`
	Usage       string            `json:"usage"`
}

type DigitsVariantMD struct {
	Name string `json:"name"`
	File string `json:"file"`
	Note string `json:"note"`
}

// Manifest 是 Battle/ART_MANIFEST.json 的根结构。
type Manifest struct {
	Tool         string         `json:"tool"`
	GeneratedAt  string         `json:"generated_at"`
	Seed         uint64         `json:"seed"`
	Spec         string         `json:"spec"`
	Notes        []string       `json:"notes"`
	SpriteImport map[string]any `json:"sprite_import_hint"`
	FileCount    int            `json:"file_count"`
	Fx           []ArtEntry     `json:"fx"`
	UI           []ArtEntry     `json:"ui"`
	Digits       DigitsMeta     `json:"digits"`
	DigitsFiles  []ArtEntry     `json:"digits_files"`
	Buffs        []ArtEntry     `json:"buff_files"`
	BuffTable    []BuffIconInfo `json:"buff_table_mapping"`
}

func main() {
	mode := flag.String("mode", "battle",
		"battle = 生成 Fx/UI/数字字集/buff 图标;characters = 22 张 qdao_v3 立绘 → 战斗精灵动作帧条;"+
			"monsters = 纯程序化怪物帧条 + 战斗地台")
	out := flag.String("out", `E:/work/mmorpg-client/Assets/Resources/Battle`, "输出根目录(客户端 Resources/Battle)")
	buffTable := flag.String("buff-table", `../../generated/tables/Buff.json`, "Buff 导表 json 路径")
	fontPath := flag.String("font", "", "字体路径;留空自动探测 simhei.ttf / msyhbd.ttc")
	seed := flag.Uint64("seed", 20260903, "随机种子(同一 seed 产出完全一致)")
	buffCount := flag.Int("buff-count", 24, "生成的 buff 图标个数")

	// ── -mode characters 专用参数(全部写进 CHARACTER_MANIFEST.json 的 params,便于复现)──
	charSrc := flag.String("char-src", `E:/work/mmorpg-client/Assets/Resources/UI/qdao_v3/characters`,
		"立绘源目录(*.png)")
	charOut := flag.String("char-out", "", "角色帧条输出目录;留空 = <out>/Characters")
	charActions := flag.String("char-actions", "idle,attack,cast,hit",
		"生成的动作,逗号分隔;可选 idle,attack,cast,hit,die,win")
	charMaxHeight := flag.Int("char-max-height", 232, "角色**身高**上限像素(等比缩放,不拉伸;≤ 256-20 的可用高度)。"+
		"实际统一身高由工具求解(全体取同一个值),见 CHARACTER_MANIFEST.json params.fitted_height")
	charDirs := flag.String("char-dirs", "E",
		"输出朝向,逗号分隔;默认只出 E,W 由客户端 BattleArtCatalog.LoadDirectionalStrip 运行时镜像。"+
			"W 只是 E 的逐格镜像副本(体积翻倍、画面零差异),只在需要非对称专有 W 向素材时才显式加上 W")
	charAlpha := flag.Int("char-alpha", 16, "包围盒判定的 alpha 阈值(0..255)")
	charChroma := flag.Float64("char-chroma", 0.18, "无 alpha 立绘的 chroma 抠图容差(RGB 欧氏距离,0..1.73)")
	charMinComp := flag.Float64("char-min-comp", 0.01, "连通域保留下限(相对最大域面积的比例,小于此值当噪点丢弃)")
	charSS := flag.Int("char-ss", 2, "每轴超采样数(2 = 4×,变换后边缘更干净)")

	// ── -mode monsters 专用参数 ──
	monCount := flag.Int("monster-count", 6,
		"生成花名册前 N 只怪(默认 6 = battle-art-prompts.md §2.2 首批;最多 16)")
	monDirs := flag.String("monster-dirs", "E",
		"怪物帧条输出朝向,逗号分隔;默认只出 E(W 由客户端运行时镜像,同 -char-dirs)")
	flag.Parse()

	var err error
	switch *mode {
	case "battle":
		err = run(*out, *buffTable, *fontPath, *seed, *buffCount)
	case "characters":
		dst := *charOut
		if dst == "" {
			dst = filepath.Join(*out, "Characters")
		}
		err = runCharacters(charOptions{
			SrcDir:      filepath.ToSlash(*charSrc),
			OutDir:      filepath.ToSlash(dst),
			Actions:     splitCSV(*charActions),
			Dirs:        splitCSV(*charDirs),
			MaxHeight:   *charMaxHeight,
			AlphaThres:  *charAlpha,
			ChromaTol:   *charChroma,
			MinCompPct:  *charMinComp,
			Supersample: *charSS,
		})
	case "monsters":
		// 纯程序化怪物帧条 + 战斗地台;不读任何外部素材,只写 Battle/Monsters 与 Battle/UI/ground_band_*。
		genMode = "monsters"
		monsterCount = *monCount
		monsterDirs = splitCSV(*monDirs)
		_, err = runMonsterStage(*out)
	default:
		err = fmt.Errorf("未知 -mode %q(可选 battle / characters / monsters)", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

// splitCSV 拆逗号分隔列表并去空白/空项。
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func run(outRoot, buffTablePath, fontPath string, seed uint64, buffCount int) error {
	dirs := []string{outRoot,
		filepath.Join(outRoot, "Fx"),
		filepath.Join(outRoot, "UI"),
		filepath.Join(outRoot, "Buff"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", d, err)
		}
	}

	mf := &Manifest{
		Tool:        "tools/battle_art_gen",
		GeneratedAt: time.Now().Format(time.RFC3339),
		Seed:        seed,
		Spec:        "docs/design/battle-art-prompts.md §3/§4;turn-battle-presentation.md §5",
		SpriteImport: map[string]any{
			"fx_strip":    map[string]any{"sprite_mode": "Multiple", "grid": 256, "pivot": []float64{0.5, 0.5}, "full_rect": true, "filter": "Bilinear", "mipmap": false},
			"digits":      map[string]any{"sprite_mode": "Multiple", "grid_w": digitCellW, "grid_h": digitCellH, "pivot": []float64{0.5, 0.5}, "full_rect": true},
			"nine_slice":  map[string]any{"sprite_mode": "Single", "mesh_type": "Full Rect", "border": "见各条目 border_9slice_lbrt"},
			"buff_icon":   map[string]any{"sprite_mode": "Single", "pivot": []float64{0.5, 0.5}},
			"compression": "建议 RGBA32 / 关闭 Crunch,特效带真实 alpha 渐变,压缩会出色带",
		},
	}

	fxCount, err := genFx(outRoot, seed, mf)
	if err != nil {
		return err
	}
	uiCount, err := genUI(outRoot, mf)
	if err != nil {
		return err
	}
	digitCount, err := genDigits(outRoot, fontPath, mf)
	if err != nil {
		return err
	}
	buffN, buffNote, catNote, err := genBuffs(outRoot, buffTablePath, buffCount, mf)
	if err != nil {
		return err
	}

	mf.Notes = []string{
		"Fx 横条:8 帧 × 256×256,左→右时间序,真实 alpha 透明背景(NRGBA,直通 alpha)。",
		"Fx 建议 pivot 见各条目:范围型(slash/thrust/fire/ice/lightning/hit_star)用 (0.5,0.5) 贴在目标中心;" +
			"地面型(heal_ring/buff_rise/death_dissolve)用 (0.5,0.1) 贴在脚底。",
		"九宫图的 border_9slice_lbrt 是 Unity Sprite Editor 的 Border(左/下/右/上)。",
		"数字字集为等宽单格,序号 = 字符在 chars_string 中的下标;客户端按下标切图即可。",
		buffNote,
		catNote,
		"本目录未生成 .meta,Unity 首次导入时自动生成;导入设置参考 sprite_import_hint。",
	}
	mf.FileCount = fxCount + uiCount + digitCount + buffN + 2 // +digits_meta.json +ART_MANIFEST.json

	mpath := filepath.Join(outRoot, "ART_MANIFEST.json")
	if err := writeJSON(mpath, mf); err != nil {
		return err
	}

	fmt.Printf("完成:Fx %d 条 / UI %d 张 / 数字字集 %d 套 / buff 图标 %d 个,清单 %s\n",
		fxCount, uiCount, digitCount, buffN, mpath)
	fmt.Printf("总文件数(含 digits_meta.json 与 ART_MANIFEST.json):%d\n", mf.FileCount)
	return nil
}

// ---------------------------------------------------------------------------

func genFx(outRoot string, seed uint64, mf *Manifest) (int, error) {
	defs := buildFxDefs(seed)
	n := 0
	for _, d := range defs {
		stripW := fxSize * d.Frames
		strip := image.NewNRGBA(image.Rect(0, 0, stripW, fxSize))
		for f := 0; f < d.Frames; f++ {
			t := float64(f) / float64(d.Frames-1)
			l := NewLayer(fxSize, fxSize)
			d.Render(l, t, f)
			frame := l.ToNRGBA()
			r := image.Rect(f*fxSize, 0, (f+1)*fxSize, fxSize)
			draw.Draw(strip, r, frame, image.Point{}, draw.Src)
		}
		rel := filepath.ToSlash(filepath.Join("Fx", d.ID+"_strip.png"))
		if err := savePNG(filepath.Join(outRoot, "Fx", d.ID+"_strip.png"), strip); err != nil {
			return n, err
		}
		mf.Fx = append(mf.Fx, ArtEntry{
			Path: rel, Kind: "fx_strip", Width: stripW, Height: fxSize,
			Frames: d.Frames, CellWidth: fxSize, CellHeight: fxSize,
			FPSHint: d.FPS, Pivot: []float64{d.Pivot[0], d.Pivot[1]}, Desc: d.Desc,
		})
		n++
		fmt.Printf("  Fx  %-18s %4d×%d  %d 帧 @%d FPS\n", d.ID, stripW, fxSize, d.Frames, d.FPS)
	}
	return n, nil
}

func genUI(outRoot string, mf *Manifest) (int, error) {
	type uiJob struct {
		name   string
		img    *image.NRGBA
		border []int
		desc   string
	}
	jobs := []uiJob{
		{"panel_9slice", makePanel9Slice(), []int{64, 64, 64, 64}, "圆角金边深棕木纹面板底,四角铆钉,九宫边距 64"},
		{"button_9slice", makeButton9Slice(), []int{22, 22, 22, 22}, "金边玉色按钮底,顶部高光 + 底部内阴影,九宫边距 22"},
		{"bar_slot", makeBarSlot(), []int{16, 15, 16, 15}, "血条槽:深色内凹 + 细金框,左右可拉伸"},
		{"command_ring", makeCommandRing(), nil, "问道式 7 扇区命令环底,含扇区分隔线、内外金框与外沿刻点"},
	}
	n := 0
	for _, j := range jobs {
		p := filepath.Join(outRoot, "UI", j.name+".png")
		if err := savePNG(p, j.img); err != nil {
			return n, err
		}
		b := j.img.Bounds()
		e := ArtEntry{
			Path: filepath.ToSlash(filepath.Join("UI", j.name+".png")), Kind: "ui",
			Width: b.Dx(), Height: b.Dy(), Desc: j.desc,
		}
		if j.border != nil {
			e.Kind = "ui_9slice"
			e.Border = j.border
		}
		mf.UI = append(mf.UI, e)
		n++
		fmt.Printf("  UI  %-18s %4d×%d\n", j.name, b.Dx(), b.Dy())
	}
	return n, nil
}

func genDigits(outRoot, fontPath string, mf *Manifest) (int, error) {
	f, usedPath, err := loadFont(fontPath)
	if err != nil {
		return 0, err
	}
	chars := make([]string, 0, len(digitChars))
	idx := make(map[string]int, len(digitChars))
	for i, r := range digitChars {
		chars = append(chars, string(r))
		idx[string(r)] = i
	}
	meta := DigitsMeta{
		CellWidth: digitCellW, CellHeight: digitCellH, CellCount: len(digitChars),
		CharsString: string(digitChars), Chars: chars, CharIndex: idx,
		Font:  filepath.ToSlash(usedPath),
		Usage: "等宽单格横条,cell i 的像素范围 = [i*cell_width, (i+1)*cell_width);按 char_index 取格拼数字。",
	}
	n := 0
	for _, v := range digitVariants() {
		img, err := renderDigitStrip(f, v)
		if err != nil {
			return n, err
		}
		file := "digits_" + v.Name + ".png"
		if err := savePNG(filepath.Join(outRoot, "UI", file), img); err != nil {
			return n, err
		}
		b := img.Bounds()
		meta.Variants = append(meta.Variants, DigitsVariantMD{Name: v.Name, File: file, Note: v.Note})
		mf.DigitsFiles = append(mf.DigitsFiles, ArtEntry{
			Path: filepath.ToSlash(filepath.Join("UI", file)), Kind: "digits_strip",
			Width: b.Dx(), Height: b.Dy(), Frames: len(digitChars),
			CellWidth: digitCellW, CellHeight: digitCellH, Desc: v.Note,
		})
		n++
		fmt.Printf("  字集 %-17s %4d×%d  %d 格\n", v.Name, b.Dx(), b.Dy(), len(digitChars))
	}
	mf.Digits = meta
	if err := writeJSON(filepath.Join(outRoot, "UI", "digits_meta.json"), meta); err != nil {
		return n, err
	}
	fmt.Printf("  字集字体:%s\n", usedPath)
	return n, nil
}

func genBuffs(outRoot, tablePath string, want int, mf *Manifest) (int, string, string, error) {
	rows, pad, tableNote, err := loadBuffIDs(tablePath, want)
	if err != nil {
		return 0, "", "", err
	}
	catOf, catNote := categorizeBuffTypes(rows)
	fromTable := len(rows) - pad
	n := 0
	for i, r := range rows {
		cat := catOf[r.BuffType]
		if cat == "" {
			cat = buffCategoryOrder[i%3]
		}
		pal := buffPalettes[cat]
		symbol := i % len(buffSymbolNames)
		img := makeBuffIcon(pal, symbol)
		file := fmt.Sprintf("%d.png", r.ID)
		if err := savePNG(filepath.Join(outRoot, "Buff", file), img); err != nil {
			return n, "", "", err
		}
		src := "Buff.json"
		if i >= fromTable {
			src = "padding"
		}
		mf.BuffTable = append(mf.BuffTable, BuffIconInfo{
			ID: r.ID, File: filepath.ToSlash(filepath.Join("Buff", file)),
			BuffType: r.BuffType, Category: cat, Symbol: buffSymbolNames[symbol], Source: src,
		})
		mf.Buffs = append(mf.Buffs, ArtEntry{
			Path:  filepath.ToSlash(filepath.Join("Buff", file)),
			Kind:  "buff_icon",
			Width: buffIconSize, Height: buffIconSize,
			Desc: fmt.Sprintf("buff %d(buff_type=%d)%s 底 + %s 符号", r.ID, r.BuffType, cat, buffSymbolNames[symbol]),
		})
		n++
	}
	fmt.Printf("  Buff %d 个图标 %d×%d(%s)\n", n, buffIconSize, buffIconSize, tableNote)
	return n, tableNote, catNote, nil
}

// ---------------------------------------------------------------------------

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", path, err)
	}
	defer f.Close()
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(f, img); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
