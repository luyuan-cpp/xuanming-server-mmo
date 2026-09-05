package main

// fx.go —— 9 套技能特效序列帧(每套 8 帧 × 256×256,左→右时间序)。
// 统一手法:加法辉光 + 径向渐变 + 贝塞尔/弧线描边 + 噪声散点,逐帧走"出现 → 高潮 → 消散"。

import (
	"math"
	"math/rand/v2"
)

const fxSize = 256
const fxFrames = 8

// FxDef 一套特效的定义。
type FxDef struct {
	ID     string
	Frames int
	FPS    int
	Pivot  [2]float64
	Desc   string
	Render func(l *Layer, t float64, frame int)
}

func newRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
}

func rnd(r *rand.Rand, lo, hi float64) float64 { return lo + r.Float64()*(hi-lo) }

// buildFxDefs 构造全部特效定义。所有随机量在闭包构造期一次性生成,保证跨帧连贯且可复现。
func buildFxDefs(seed uint64) []FxDef {
	return []FxDef{
		fxSlashArc(seed + 1),
		fxThrustLine(seed + 2),
		fxFireBurst(seed + 3),
		fxIceShard(seed + 4),
		fxLightningStrike(seed + 5),
		fxHealRing(seed + 6),
		fxBuffRise(seed + 7),
		fxHitStar(seed + 8),
		fxDeathDissolve(seed + 9),
	}
}

// ---------------------------------------------------------------------------
// 1. slash_arc —— 弧形斩击(白 → 青,带残影)
// ---------------------------------------------------------------------------

func fxSlashArc(seed uint64) FxDef {
	rng := newRNG(seed)
	white := rgb8(255, 255, 255)
	cyan := rgb8(120, 240, 255)
	deep := rgb8(30, 150, 235)

	// 弧心在画面左外,弧身向右鼓出,形成一道月牙刀光
	arc := func(r float64) [][2]float64 { return arcPoints(6, 128, r, -0.88, 0.88, 96) }
	main := arc(162)
	ghostA := arc(146)
	ghostB := arc(178)

	// 刀光两侧的细碎风刃
	type sliver struct{ r, a, len, w, ph float64 }
	slivers := make([]sliver, 7)
	for i := range slivers {
		slivers[i] = sliver{
			r:   rnd(rng, 126, 176),
			a:   rnd(rng, -0.70, 0.70),
			len: rnd(rng, 0.10, 0.26),
			w:   rnd(rng, 1.0, 2.6),
			ph:  rnd(rng, 0.0, 0.35),
		}
	}

	colAt := func(u float64) RGB {
		if u < 0.55 {
			return mixRGB(deep, cyan, u/0.55)
		}
		return mixRGB(cyan, white, (u-0.55)/0.45)
	}

	return FxDef{
		ID: "slash_arc", Frames: fxFrames, FPS: 16, Pivot: [2]float64{0.5, 0.5},
		Desc: "弧形斩击:白芯青边的月牙刀光横扫,后方两层残影 + 碎风刃",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.06, 0.14, t) * (1 - smoothstep(0.52, 1.04, t))
			if amp <= 0.001 {
				return
			}
			head := clamp01(easeOut(0.10 + t*1.42))
			tail := clamp01(easeIn(math.Max(0, (t-0.26)/0.70)) * 1.05)

			drawBlade := func(path [][2]float64, hs, ts, maxW, gain float64) {
				seg := subPath(path, ts, hs)
				if len(seg) < 2 {
					return
				}
				l.StrokePath(seg, PathStyle{
					Width: func(u float64) float64 {
						return maxW * math.Pow(u, 0.65) * (1 - 0.22*u)
					},
					Glow:      func(u float64) float64 { return 6 + 18*u },
					Color:     colAt,
					GlowColor: func(u float64) RGB { return mixRGB(deep, cyan, u) },
					Alpha:     func(u float64) float64 { return amp * gain * (0.30 + 0.70*u) },
					GlowGain:  0.55,
				})
			}

			// 残影(更早、更淡、更细)
			drawBlade(ghostB, clamp01(head-0.10), clamp01(tail-0.06), 8.0, 0.30)
			drawBlade(ghostA, clamp01(head-0.18), clamp01(tail-0.12), 6.5, 0.22)
			// 主刀光
			drawBlade(main, head, tail, 16.0, 1.0)

			// 碎风刃
			for _, s := range slivers {
				p := clamp01((t - s.ph) / 0.45)
				if p <= 0 || p >= 1 {
					continue
				}
				a0 := s.a - s.len/2
				a1 := s.a + s.len/2
				pts := arcPoints(6, 128, s.r+18*p, a0, a1, 10)
				l.StrokePath(pts, PathStyle{
					Width:     constW(s.w),
					Glow:      constW(5),
					Color:     constC(white),
					GlowColor: constC(cyan),
					Alpha:     constA(amp * 0.55 * math.Sin(math.Pi*p)),
				})
			}

			// 刀锋前端的爆亮点
			if head > 0.02 && head < 0.995 {
				hp := interpPath(main, head*float64(len(main)-1))
				l.AddRadial(hp[0], hp[1], 34*amp+8, 6, white, cyan, 0.95*amp, 2.1)
				l.AddDot(hp[0], hp[1], 10, white, amp, 1.4)
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 2. thrust_line —— 刺击直线冲击
// ---------------------------------------------------------------------------

func fxThrustLine(seed uint64) FxDef {
	rng := newRNG(seed)
	white := rgb8(255, 255, 255)
	pale := rgb8(190, 228, 255)
	steel := rgb8(90, 160, 235)

	type speedLine struct{ dy, x0, len, w, ph float64 }
	lines := make([]speedLine, 9)
	for i := range lines {
		lines[i] = speedLine{
			dy:  rnd(rng, -46, 46),
			x0:  rnd(rng, 10, 120),
			len: rnd(rng, 45, 130),
			w:   rnd(rng, 0.8, 2.2),
			ph:  rnd(rng, 0, 0.3),
		}
	}

	return FxDef{
		ID: "thrust_line", Frames: fxFrames, FPS: 16, Pivot: [2]float64{0.5, 0.5},
		Desc: "刺击直线冲击:枪尖白芯穿刺,后拖速度线,命中处爆出竖向冲击波",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.12, 0.14, t) * (1 - smoothstep(0.55, 1.05, t))
			if amp <= 0.001 {
				return
			}
			hx := 18 + 226*easeOut(clamp01(t*1.35))
			ln := 40 + 150*smoothstep(0, 0.3, t)
			ln *= 1 - 0.55*smoothstep(0.55, 1, t)
			tx := math.Max(2, hx-ln)

			// 速度线
			for _, s := range lines {
				p := clamp01((t - s.ph) / 0.55)
				if p <= 0 {
					continue
				}
				sx := s.x0 + 130*easeOut(p)
				ex := math.Min(sx+s.len*(1-0.4*p), 252)
				if ex-sx < 4 {
					continue
				}
				y := 128 + s.dy*(1-0.25*p)
				l.StrokePath([][2]float64{{sx, y}, {ex, y}}, PathStyle{
					Width:     constW(s.w),
					Glow:      constW(4),
					Color:     constC(pale),
					GlowColor: constC(steel),
					Alpha:     constA(amp * 0.35 * math.Sin(math.Pi*p)),
				})
			}

			// 枪身:尾细头粗,尾青头白
			l.StrokePath([][2]float64{{tx, 128}, {(tx + hx) / 2, 128}, {hx, 128}}, PathStyle{
				Width:     func(u float64) float64 { return 1.2 + 8.5*math.Pow(u, 2.0) },
				Glow:      func(u float64) float64 { return 7 + 20*u },
				Color:     func(u float64) RGB { return mixRGB(pale, white, u) },
				GlowColor: func(u float64) RGB { return mixRGB(steel, pale, u) },
				Alpha:     func(u float64) float64 { return amp * (0.25 + 0.75*u) },
				GlowGain:  0.5,
			})

			// 枪尖爆亮 + 十字星
			l.AddRadial(hx, 128, 40, 8, white, pale, amp, 2.0)
			for _, d := range [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				sp := 28.0
				if d[1] != 0 {
					sp = 17
				}
				l.StrokePath([][2]float64{{hx, 128}, {hx + d[0]*sp, 128 + d[1]*sp}}, PathStyle{
					Width:     func(u float64) float64 { return 3.2 * math.Pow(1-u, 1.4) },
					Glow:      constW(6),
					Color:     constC(white),
					GlowColor: constC(pale),
					Alpha:     constA(amp * 0.9),
				})
			}

			// 命中处的竖向冲击波
			shock := clamp01((t - 0.42) / 0.5)
			if shock > 0 {
				rx := 8 + 26*easeOut(shock)
				ry := 30 + 78*easeOut(shock)
				pts := ellipsePoints(206, 128, rx, ry, 60)
				l.StrokePath(pts, PathStyle{
					Width:     constW(2.4 * (1 - 0.6*shock)),
					Glow:      constW(12),
					Color:     constC(white),
					GlowColor: constC(steel),
					Alpha:     constA(amp * 0.75 * (1 - shock)),
				})
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 3. fire_burst —— 火球爆裂(橙红)
// ---------------------------------------------------------------------------

func fxFireBurst(seed uint64) FxDef {
	rng := newRNG(seed)
	hot := rgb8(255, 248, 205)
	orange := rgb8(255, 150, 45)
	red := rgb8(196, 42, 22)

	type lobe struct{ ang, rf, sf, ph float64 }
	lobes := make([]lobe, 34)
	for i := range lobes {
		lobes[i] = lobe{
			ang: rnd(rng, 0, 2*math.Pi),
			rf:  math.Sqrt(rnd(rng, 0.05, 1.0)),
			sf:  rnd(rng, 0.5, 1.4),
			ph:  rnd(rng, 0, 1),
		}
	}
	type tongue struct{ ang, w, ln, ph, bd float64 }
	tongues := make([]tongue, 9)
	for i := range tongues {
		a := rnd(rng, 0, 2*math.Pi)
		// 火焰上窜:朝上的火舌更长
		up := (1 - math.Sin(a)) * 0.5
		tongues[i] = tongue{
			ang: a,
			w:   rnd(rng, 0.48, 1.30),
			ln:  rnd(rng, 0.20, 1.30) * (0.65 + 0.65*up),
			ph:  rnd(rng, 0, 0.26),
			bd:  rnd(rng, 0.32, 0.60),
		}
	}
	type ember struct{ ang, spd, size, ph float64 }
	embers := make([]ember, 46)
	for i := range embers {
		embers[i] = ember{
			ang:  rnd(rng, 0, 2*math.Pi),
			spd:  rnd(rng, 0.75, 1.55),
			size: rnd(rng, 1.6, 4.2),
			ph:   rnd(rng, 0, 0.22),
		}
	}

	return FxDef{
		ID: "fire_burst", Frames: fxFrames, FPS: 12, Pivot: [2]float64{0.5, 0.5},
		Desc: "火球爆裂:白黄内芯向外翻滚成橙红火舌,四散飞火星,尾帧上飘消散",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.10, 0.13, t) * (1 - smoothstep(0.48, 1.06, t))
			if amp <= 0.001 {
				return
			}
			R := 16 + 104*easeOut(clamp01(t*1.05))
			rise := 26 * easeIn(t)

			for _, lo := range lobes {
				turb := 0.72 + 0.42*math.Sin(lo.ph*6.2831+t*4.1)
				rr := R * lo.rf * turb
				cx := 128 + math.Cos(lo.ang)*rr
				cy := 128 + math.Sin(lo.ang)*rr - rise*(0.3+0.7*lo.rf)
				rad := (11 + 27*lo.sf) * (0.55 + 0.85*easeOut(t))
				k := clamp01(lo.rf*turb + 0.15)
				var ci, co RGB
				if k < 0.5 {
					ci = mixRGB(hot, orange, k/0.5)
					co = orange
				} else {
					ci = mixRGB(orange, red, (k-0.5)/0.5)
					co = red
				}
				a := amp * 0.42 * (0.45 + 0.55*(1-k))
				l.AddRadial(cx, cy, rad, rad*0.22, ci, co, a, 1.9)
			}

			// 火舌:给火球一圈有轮廓的翻滚火焰
			for _, tg := range tongues {
				tp := clamp01((t - tg.ph) / 0.62)
				if tp <= 0 {
					continue
				}
				ta := amp * math.Pow(math.Sin(math.Pi*tp), 0.55)
				a := tg.ang + 0.30*math.Sin(tg.ph*6.2831+t*4.6)
				dx, dy := math.Cos(a), math.Sin(a)
				px, py := -dy, dx
				bd := R * tg.bd
				td := R * (0.80 + 0.62*tg.ln) * (0.60 + 0.55*easeOut(tp))
				side := R * 0.17 * tg.w
				bx, by := 128+dx*bd, 128+dy*bd-rise*0.35
				tx, ty := 128+dx*td, 128+dy*td-rise*0.75
				ex, ey := dx*(td-bd)*0.32, dy*(td-bd)*0.32
				left := bezier3(
					[2]float64{bx + px*side, by + py*side},
					[2]float64{bx + px*side*1.35 + ex, by + py*side*1.35 + ey},
					[2]float64{tx + px*side*0.30, ty + py*side*0.30},
					[2]float64{tx, ty}, 12)
				right := bezier3(
					[2]float64{tx, ty},
					[2]float64{tx - px*side*0.30, ty - py*side*0.30},
					[2]float64{bx - px*side*1.35 + ex, by - py*side*1.35 + ey},
					[2]float64{bx - px*side, by - py*side}, 12)
				poly := append(append([][2]float64{}, left...), right...)
				l.FillPolyAdd(poly, mixRGB(orange, red, 0.48), ta*0.26)
				outline := append(append([][2]float64{}, poly...), poly[0])
				l.StrokePath(outline, PathStyle{
					Width:     constW(1.3),
					Glow:      constW(8),
					Color:     constC(mixRGB(hot, orange, 0.35)),
					GlowColor: constC(orange),
					Alpha:     constA(ta * 0.34),
				})
			}

			// 内芯
			core := (1 - 0.55*easeIn(t))
			l.AddRadial(128, 128-rise*0.4, (34+30*easeOut(t))*core, 10, rgb8(255, 255, 245), hot, amp*1.05, 1.7)

			// 飞火星
			for _, e := range embers {
				p := clamp01((t - e.ph) / 0.75)
				if p <= 0 {
					continue
				}
				d := R*1.02 + 78*e.spd*easeOut(p)
				ex := 128 + math.Cos(e.ang)*d
				ey := 128 + math.Sin(e.ang)*d - 34*p*p
				a := amp * (1 - p) * 0.95
				// 拖尾
				bx := 128 + math.Cos(e.ang)*(d-16*e.spd)
				by := 128 + math.Sin(e.ang)*(d-16*e.spd) - 24*p*p
				l.StrokePath([][2]float64{{bx, by}, {ex, ey}}, PathStyle{
					Width:     func(u float64) float64 { return e.size * 0.45 * u },
					Glow:      constW(5),
					Color:     constC(hot),
					GlowColor: constC(orange),
					Alpha:     constA(a * 0.7),
				})
				l.AddRadial(ex, ey, e.size*2.6, e.size*0.5, rgb8(255, 250, 220), orange, a, 1.8)
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 4. ice_shard —— 冰锥破碎(青白)
// ---------------------------------------------------------------------------

func fxIceShard(seed uint64) FxDef {
	rng := newRNG(seed)
	iceW := rgb8(238, 253, 255)
	iceC := rgb8(125, 222, 255)
	iceD := rgb8(46, 132, 214)

	type shard struct{ ang, ln, hw, spin, ph float64 }
	shards := make([]shard, 10)
	for i := range shards {
		base := float64(i)/10*2*math.Pi + rnd(rng, -0.16, 0.16)
		shards[i] = shard{
			ang:  base,
			ln:   rnd(rng, 46, 82),
			hw:   rnd(rng, 7, 15),
			spin: rnd(rng, -0.5, 0.5),
			ph:   rnd(rng, 0, 0.12),
		}
	}
	type crack struct{ ang, ln, jit float64 }
	cracks := make([]crack, 7)
	for i := range cracks {
		cracks[i] = crack{ang: rnd(rng, 0, 2*math.Pi), ln: rnd(rng, 55, 110), jit: rnd(rng, -0.4, 0.4)}
	}

	const flashT = 0.42

	return FxDef{
		ID: "ice_shard", Frames: fxFrames, FPS: 12, Pivot: [2]float64{0.5, 0.5},
		Desc: "冰锥破碎:冰晶向心汇聚,中段爆闪,随后碎片四射并扩出霜环裂纹",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.10, 0.12, t) * (1 - smoothstep(0.55, 1.05, t))
			if amp <= 0.001 {
				return
			}
			for _, s := range shards {
				var dist, sa, sc float64
				if t < flashT {
					p := clamp01(t / flashT)
					dist = 74*(1-easeIn(p)) + 26
					sa = amp * (0.35 + 0.5*p)
					sc = 0.75 + 0.25*p
				} else {
					p := clamp01((t - flashT) / (1 - flashT))
					dist = 26 + 96*easeOut(p)
					sa = amp * (1 - 0.75*p)
					sc = 1 - 0.35*p
				}
				ang := s.ang + s.spin*t
				dx, dy := math.Cos(ang), math.Sin(ang)
				px, py := -dy, dx
				tipD := dist + s.ln*sc*0.55
				backD := dist - s.ln*sc*0.45
				tip := [2]float64{128 + dx*tipD, 128 + dy*tipD}
				back := [2]float64{128 + dx*backD, 128 + dy*backD}
				mid := [2]float64{128 + dx*dist, 128 + dy*dist}
				hw := s.hw * sc
				poly := [][2]float64{
					tip,
					{mid[0] + px*hw, mid[1] + py*hw},
					back,
					{mid[0] - px*hw, mid[1] - py*hw},
				}
				l.FillPolyAdd(poly, mixRGB(iceD, iceC, 0.55), sa*0.5)
				outline := append(append([][2]float64{}, poly...), poly[0])
				l.StrokePath(outline, PathStyle{
					Width:     constW(1.6),
					Glow:      constW(9),
					Color:     constC(iceW),
					GlowColor: constC(iceC),
					Alpha:     constA(sa * 0.95),
				})
			}

			// 爆闪
			flash := 1 - clamp01(math.Abs(t-flashT)/0.24)
			if flash > 0 {
				l.AddRadial(128, 128, 40+90*flash, 12, rgb8(255, 255, 255), iceC, amp*flash*1.1, 1.9)
			}

			// 霜环 + 裂纹(闪后)
			if t > flashT {
				p := clamp01((t - flashT) / (1 - flashT))
				r := 26 + 112*easeOut(p)
				l.StrokePath(ellipsePoints(128, 128, r, r*0.94, 72), PathStyle{
					Width:     constW(2.6 * (1 - 0.55*p)),
					Glow:      constW(11),
					Color:     constC(iceW),
					GlowColor: constC(iceD),
					Alpha:     constA(amp * 0.7 * (1 - p)),
				})
				for _, c := range cracks {
					ln := c.ln * (0.4 + 0.6*p)
					mx := 128 + math.Cos(c.ang+c.jit*0.5)*ln*0.55
					my := 128 + math.Sin(c.ang+c.jit*0.5)*ln*0.55
					ex := 128 + math.Cos(c.ang)*ln
					ey := 128 + math.Sin(c.ang)*ln
					l.StrokePath([][2]float64{{128, 128}, {mx, my}, {ex, ey}}, PathStyle{
						Width:     func(u float64) float64 { return 2.2 * (1 - 0.85*u) },
						Glow:      constW(6),
						Color:     constC(iceW),
						GlowColor: constC(iceC),
						Alpha:     constA(amp * 0.55 * (1 - p)),
					})
				}
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 5. lightning_strike —— 雷击竖闪(紫白)
// ---------------------------------------------------------------------------

func makeBolt(rng *rand.Rand, x0, y0, x1, y1 float64, iters int, disp float64) [][2]float64 {
	pts := [][2]float64{{x0, y0}, {x1, y1}}
	for it := 0; it < iters; it++ {
		next := make([][2]float64, 0, len(pts)*2)
		for i := 0; i+1 < len(pts); i++ {
			a := pts[i]
			b := pts[i+1]
			mx := (a[0] + b[0]) / 2
			my := (a[1] + b[1]) / 2
			dx := b[0] - a[0]
			dy := b[1] - a[1]
			ln := math.Hypot(dx, dy)
			if ln > 1e-6 {
				o := (rng.Float64()*2 - 1) * disp
				mx += -dy / ln * o
				my += dx / ln * o
			}
			next = append(next, a, [2]float64{mx, my})
		}
		next = append(next, pts[len(pts)-1])
		pts = next
		disp *= 0.55
	}
	return pts
}

func fxLightningStrike(seed uint64) FxDef {
	rng := newRNG(seed)
	white := rgb8(255, 255, 255)
	violet := rgb8(196, 132, 255)
	deepV := rgb8(112, 52, 214)

	type variant struct {
		main     [][2]float64
		branches [][][2]float64
	}
	variants := make([]variant, 3)
	for i := range variants {
		m := makeBolt(rng, rnd(rng, 106, 150), -6, 128, 198, 5, 34)
		var brs [][][2]float64
		for b := 0; b < 3; b++ {
			idx := int(rnd(rng, float64(len(m))*0.25, float64(len(m))*0.8))
			if idx >= len(m) {
				idx = len(m) - 1
			}
			p := m[idx]
			ex := p[0] + rnd(rng, -70, 70)
			ey := p[1] + rnd(rng, 24, 62)
			brs = append(brs, makeBolt(rng, p[0], p[1], ex, ey, 3, 14))
		}
		variants[i] = variant{main: m, branches: brs}
	}
	// 逐帧闪烁强度(雷击不是平滑衰减,是"爆闪 + 余光抖动")
	flick := []float64{0.18, 1.0, 0.92, 0.62, 0.42, 0.24, 0.12, 0.05}

	return FxDef{
		ID: "lightning_strike", Frames: fxFrames, FPS: 20, Pivot: [2]float64{0.5, 0.5},
		Desc: "雷击竖闪:紫白主雷自上贯下,分叉支雷,落点炸开地面电环与余光抖闪",
		Render: func(l *Layer, t float64, f int) {
			g := flick[f%len(flick)]
			if g <= 0.01 {
				return
			}
			v := variants[f%len(variants)]

			// 竖向紫色雾柱
			l.StrokePath([][2]float64{{128, 0}, {128, 198}}, PathStyle{
				Width:     constW(3),
				Glow:      constW(46),
				Color:     constC(deepV),
				GlowColor: constC(deepV),
				Alpha:     constA(g * 0.22),
				GlowGain:  0.8,
			})

			if f >= 1 && f <= 5 {
				for _, br := range v.branches {
					l.StrokePath(br, PathStyle{
						Width:     func(u float64) float64 { return 1.8 * (1 - 0.7*u) },
						Glow:      constW(12),
						Color:     constC(white),
						GlowColor: constC(violet),
						Alpha:     constA(g * 0.65),
					})
				}
				l.StrokePath(v.main, PathStyle{
					Width:     func(u float64) float64 { return 4.4 - 1.6*u },
					Glow:      func(u float64) float64 { return 20 + 10*u },
					Color:     constC(white),
					GlowColor: constC(violet),
					Alpha:     constA(g),
					GlowGain:  0.7,
				})
			}

			// 落点
			l.AddRadial(128, 198, 40+52*(1-g), 10, white, violet, g*0.95, 1.9)
			l.AddEllipseGlow(128, 202, 78, 24, deepV, g*0.5, 2.2)

			// 地面电环
			p := clamp01((t - 0.12) / 0.8)
			if p > 0 {
				r := 24 + 96*easeOut(p)
				l.StrokePath(ellipsePoints(128, 202, r, r*0.30, 64), PathStyle{
					Width:     constW(2.6 * (1 - 0.6*p)),
					Glow:      constW(10),
					Color:     constC(white),
					GlowColor: constC(violet),
					Alpha:     constA(g * 0.8 * (1 - p)),
				})
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 6. heal_ring —— 治疗光环上升(翠绿)
// ---------------------------------------------------------------------------

func fxHealRing(seed uint64) FxDef {
	rng := newRNG(seed)
	emerald := rgb8(64, 232, 142)
	pale := rgb8(212, 255, 226)
	deep := rgb8(18, 148, 92)

	type mote struct{ ang, rad, ph, size, sway float64 }
	motes := make([]mote, 30)
	for i := range motes {
		motes[i] = mote{
			ang:  rnd(rng, 0, 2*math.Pi),
			rad:  math.Sqrt(rnd(rng, 0.05, 1)) * 72,
			ph:   rnd(rng, 0, 0.55),
			size: rnd(rng, 1.5, 3.6),
			sway: rnd(rng, -10, 10),
		}
	}

	return FxDef{
		ID: "heal_ring", Frames: fxFrames, FPS: 10, Pivot: [2]float64{0.5, 0.1},
		Desc: "治疗光环:两道翠绿光圈自脚下升起收束,伴随上浮光点与柔和绿柱",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.08, 0.14, t) * (1 - smoothstep(0.62, 1.05, t))
			if amp <= 0.001 {
				return
			}
			// 柔和绿柱
			l.AddEllipseGlow(128, 150, 74, 96, deep, amp*0.16, 2.4)

			ring := func(p float64, gain float64) {
				if p <= 0 || p >= 1 {
					return
				}
				y := 214 - 156*easeOut(p)
				rx := 82 * (1 - 0.32*p)
				ry := rx * 0.30
				a := amp * gain * math.Pow(math.Sin(math.Pi*p), 0.6)
				l.StrokePath(ellipsePoints(128, y, rx, ry, 80), PathStyle{
					Width:     constW(3.4),
					Glow:      constW(14),
					Color:     func(u float64) RGB { return mixRGB(emerald, pale, 0.35+0.4*math.Sin(u*6.2831)) },
					GlowColor: constC(emerald),
					Alpha:     constA(a),
					GlowGain:  0.7,
				})
			}
			ring(clamp01(t/0.86), 1.0)
			ring(clamp01((t-0.26)/0.86), 0.72)

			for _, m := range motes {
				p := clamp01((t - m.ph) / 0.62)
				if p <= 0 {
					continue
				}
				x := 128 + math.Cos(m.ang)*m.rad*(1-0.35*p) + m.sway*p
				y := 216 - 170*easeOut(p) + math.Sin(m.ang)*m.rad*0.22*(1-p)
				a := amp * math.Sin(math.Pi*p) * 0.95
				l.AddRadial(x, y, m.size*3.2, m.size*0.6, pale, emerald, a, 1.8)
			}

			// 底部起手光斑
			base := 1 - smoothstep(0.1, 0.55, t)
			if base > 0 {
				l.AddEllipseGlow(128, 216, 86, 26, emerald, amp*base*0.5, 2.0)
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 7. buff_rise —— 增益上升光柱(金黄)
// ---------------------------------------------------------------------------

func fxBuffRise(seed uint64) FxDef {
	rng := newRNG(seed)
	gold := rgb8(255, 214, 92)
	amber := rgb8(255, 158, 32)
	paleG := rgb8(255, 250, 214)

	type streak struct{ dx, ph, len, w float64 }
	streaks := make([]streak, 11)
	for i := range streaks {
		streaks[i] = streak{
			dx:  rnd(rng, -30, 30),
			ph:  rnd(rng, 0, 0.5),
			len: rnd(rng, 26, 62),
			w:   rnd(rng, 1.0, 2.6),
		}
	}
	type spark struct{ dx, ph, size float64 }
	sparks := make([]spark, 16)
	for i := range sparks {
		sparks[i] = spark{dx: rnd(rng, -34, 34), ph: rnd(rng, 0, 0.6), size: rnd(rng, 1.4, 3.2)}
	}

	return FxDef{
		ID: "buff_rise", Frames: fxFrames, FPS: 10, Pivot: [2]float64{0.5, 0.1},
		Desc: "增益光柱:金黄光柱自地面拔起,内部流光上冲,底环外扩,顶端散出金星",
		Render: func(l *Layer, t float64, f int) {
			amp := smoothstep(-0.08, 0.16, t) * (1 - smoothstep(0.58, 1.05, t))
			if amp <= 0.001 {
				return
			}
			const baseY = 228.0
			top := baseY - 196*easeOut(clamp01(t*1.12))
			hw := 32 * (1 - 0.3*t)

			// 光柱本体
			y0 := int(math.Max(0, math.Floor(top-14)))
			y1 := int(math.Min(fxSize-1, math.Ceil(baseY+6)))
			for y := y0; y <= y1; y++ {
				fy := clamp01((baseY - float64(y)) / math.Max(baseY-top, 1))
				vert := (1 - 0.55*fy) * smoothstep(0, 0.10, 1-fy) * (1 - smoothstep(0.86, 1.02, fy))
				if vert <= 0 {
					continue
				}
				for x := int(128 - hw*2.2); x <= int(128+hw*2.2); x++ {
					dx := (float64(x) + 0.5 - 128) / hw
					fx := math.Exp(-dx * dx * 1.45)
					a := amp * fx * vert * 1.15
					if a <= 0.002 {
						continue
					}
					c := mixRGB(paleG, amber, clamp01(math.Abs(dx)*0.62))
					l.AddPixel(x, y, c, a)
				}
			}

			// 内部流光
			for _, s := range streaks {
				p := clamp01((t - s.ph) / 0.62)
				if p <= 0 {
					continue
				}
				ey := baseY - (baseY-top)*easeOut(p)
				sy := math.Min(baseY, ey+s.len*(1-0.4*p))
				x := 128 + s.dx*(1-0.35*p)
				l.StrokePath([][2]float64{{x, sy}, {x, ey}}, PathStyle{
					Width:     func(u float64) float64 { return s.w * (0.4 + 0.6*u) },
					Glow:      constW(6),
					Color:     constC(paleG),
					GlowColor: constC(gold),
					Alpha:     constA(amp * 0.8 * math.Sin(math.Pi*p)),
				})
			}

			// 底环
			bp := clamp01(t / 0.85)
			r := 34 + 74*easeOut(bp)
			l.StrokePath(ellipsePoints(128, baseY, r, r*0.30, 72), PathStyle{
				Width:     constW(3.2 * (1 - 0.5*bp)),
				Glow:      constW(15),
				Color:     constC(paleG),
				GlowColor: constC(amber),
				Alpha:     constA(amp * 0.85 * (1 - 0.7*bp)),
				GlowGain:  0.7,
			})
			l.AddEllipseGlow(128, baseY, 72, 22, gold, amp*0.45, 2.0)

			// 顶端金星
			for _, s := range sparks {
				p := clamp01((t - s.ph) / 0.5)
				if p <= 0 {
					continue
				}
				x := 128 + s.dx*(0.6+0.8*p)
				y := top + 16 - 40*p
				l.AddRadial(x, y, s.size*3.0, s.size*0.5, rgb8(255, 255, 235), gold, amp*math.Sin(math.Pi*p)*0.9, 1.8)
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 8. hit_star —— 命中星爆(白黄)
// ---------------------------------------------------------------------------

func fxHitStar(seed uint64) FxDef {
	rng := newRNG(seed)
	white := rgb8(255, 255, 255)
	yellow := rgb8(255, 236, 128)
	warm := rgb8(255, 186, 58)

	type spark struct{ ang, spd, size, ph float64 }
	sparks := make([]spark, 22)
	for i := range sparks {
		sparks[i] = spark{
			ang:  rnd(rng, 0, 2*math.Pi),
			spd:  rnd(rng, 0.65, 1.5),
			size: rnd(rng, 1.3, 3.4),
			ph:   rnd(rng, 0, 0.16),
		}
	}
	spikeAng := make([]float64, 8)
	spikeLen := make([]float64, 8)
	for i := 0; i < 8; i++ {
		spikeAng[i] = float64(i)/8*2*math.Pi + 0.30
		if i%2 == 0 {
			spikeLen[i] = rnd(rng, 96, 122)
		} else {
			spikeLen[i] = rnd(rng, 44, 62)
		}
	}

	return FxDef{
		ID: "hit_star", Frames: fxFrames, FPS: 16, Pivot: [2]float64{0.5, 0.5},
		Desc: "命中星爆:白黄星芒瞬闪,四长四短星角外扩,冲击圆环 + 火星四溅",
		Render: func(l *Layer, t float64, f int) {
			var amp float64
			if t < 0.16 {
				amp = 0.45 + 0.55*smoothstep(0, 0.16, t)
			} else {
				amp = 1 - smoothstep(0.16, 1.0, t)
			}
			if amp <= 0.001 {
				return
			}
			grow := easeOut(clamp01(t * 1.5))

			l.AddRadial(128, 128, 26+58*grow, 10, white, yellow, amp*1.05, 1.8)

			for i := 0; i < 8; i++ {
				ln := spikeLen[i] * (0.35 + 0.75*grow)
				ex := 128 + math.Cos(spikeAng[i])*ln
				ey := 128 + math.Sin(spikeAng[i])*ln
				mw := 8.0
				if i%2 == 1 {
					mw = 4.5
				}
				l.StrokePath([][2]float64{{128, 128}, {ex, ey}}, PathStyle{
					Width:     func(u float64) float64 { return mw * math.Pow(1-u, 1.5) },
					Glow:      func(u float64) float64 { return 10 * (1 - 0.6*u) },
					Color:     func(u float64) RGB { return mixRGB(white, yellow, u) },
					GlowColor: constC(warm),
					Alpha:     constA(amp * 0.95),
				})
			}

			rp := clamp01(t / 0.9)
			rr := 26 + 90*easeOut(rp)
			l.StrokePath(ellipsePoints(128, 128, rr, rr, 72), PathStyle{
				Width:     constW(2.6 * (1 - 0.65*rp)),
				Glow:      constW(9),
				Color:     constC(white),
				GlowColor: constC(warm),
				Alpha:     constA(amp * 0.6 * (1 - rp)),
			})

			for _, s := range sparks {
				p := clamp01((t - s.ph) / 0.8)
				if p <= 0 {
					continue
				}
				d := 24 + 106*s.spd*easeOut(p)
				x := 128 + math.Cos(s.ang)*d
				y := 128 + math.Sin(s.ang)*d
				l.AddRadial(x, y, s.size*2.8, s.size*0.5, white, yellow, amp*(1-p)*0.9, 1.8)
			}
		},
	}
}

// ---------------------------------------------------------------------------
// 9. death_dissolve —— 消散粒子(灰紫)
// ---------------------------------------------------------------------------

func fxDeathDissolve(seed uint64) FxDef {
	rng := newRNG(seed)
	grayP := rgb8(174, 158, 200)
	deepP := rgb8(92, 76, 126)
	paleP := rgb8(228, 218, 246)

	// 粗略人形轮廓:头 + 躯干 + 双腿
	inSilhouette := func(x, y float64) bool {
		// 头
		if math.Hypot(x-128, (y-58)*1.06) < 21 {
			return true
		}
		// 躯干
		dx := (x - 128) / 34
		dy := (y - 120) / 40
		if dx*dx+dy*dy < 1 {
			return true
		}
		// 双臂
		for _, ax := range []float64{93, 163} {
			ddx := (x - ax) / 11
			ddy := (y - 118) / 33
			if ddx*ddx+ddy*ddy < 1 {
				return true
			}
		}
		// 双腿
		for _, lx := range []float64{113, 143} {
			ddx := (x - lx) / 12
			ddy := (y - 184) / 44
			if ddx*ddx+ddy*ddy < 1 {
				return true
			}
		}
		return false
	}

	type part struct{ x, y, delay, drift, size, bright float64 }
	parts := make([]part, 0, 320)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for len(parts) < 320 {
		x := rnd(rng, 74, 182)
		y := rnd(rng, 32, 232)
		if !inSilhouette(x, y) {
			continue
		}
		minY = math.Min(minY, y)
		maxY = math.Max(maxY, y)
		parts = append(parts, part{
			x: x, y: y,
			drift:  rnd(rng, -36, 36),
			size:   rnd(rng, 1.4, 3.4),
			bright: rnd(rng, 0.45, 1.0),
		})
	}
	for i := range parts {
		// 自下而上消散:底部先化,顶部后化
		k := clamp01((maxY - parts[i].y) / math.Max(maxY-minY, 1))
		parts[i].delay = k*0.30 + rnd(rng, 0, 0.32)
	}

	return FxDef{
		ID: "death_dissolve", Frames: fxFrames, FPS: 8, Pivot: [2]float64{0.5, 0.1},
		Desc: "死亡消散:灰紫剪影自下而上碎成粒子飘散,残留一缕紫雾",
		Render: func(l *Layer, t float64, f int) {
			amp := 1 - smoothstep(0.72, 1.02, t)
			if amp <= 0.001 {
				return
			}
			// 底部残雾
			l.AddEllipseGlow(128, 214, 62, 22, deepP, amp*0.35*(1-t*0.6), 2.2)

			for _, p := range parts {
				rel := clamp01((t - p.delay) / 0.42)
				x := p.x + p.drift*rel*rel
				y := p.y - (24+34*p.bright)*easeOut(rel)
				a := amp * math.Pow(1-rel, 1.6) * (0.42 + 0.58*p.bright)
				if a <= 0.004 {
					continue
				}
				sz := p.size * (1 + 0.7*rel)
				c := mixRGB(grayP, paleP, rel*0.7)
				l.AddRadial(x, y, sz*2.6, sz*0.55, c, deepP, a*0.85, 1.9)
			}

			// 上飘的一缕紫雾
			wisp := clamp01((t - 0.25) / 0.6)
			if wisp > 0 {
				pts := bezier3([2]float64{128, 200 - 60*wisp}, [2]float64{112, 150 - 70*wisp},
					[2]float64{146, 108 - 70*wisp}, [2]float64{128, 56 - 60*wisp}, 32)
				l.StrokePath(pts, PathStyle{
					Width:     func(u float64) float64 { return 5 * (1 - u) },
					Glow:      constW(16),
					Color:     constC(grayP),
					GlowColor: constC(deepP),
					Alpha:     constA(amp * 0.4 * math.Sin(math.Pi*wisp)),
					GlowGain:  0.7,
				})
			}
		},
	}
}
