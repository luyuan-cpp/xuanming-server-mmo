package snowflakealloc

// 申领算法(设计稿 §3.2),Go / C++ 共用同一套语义,由
// testdata/selection_vectors.json 里的向量对拍。
//
//	used     = slots/* 下有 lease 的槽
//	wm[i]    = watermark/i(缺省 0 = 从没人用过)
//	now      = 本机墙钟秒(snowflake.Epoch 口径)
//	候选      = { i ∉ used, i ≤ maxSlot, now − wm[i] ≥ Q }   # wm=0 视为无穷久
//	选择      = 候选中 wm 最小者;并列取最小 i
//	候选为空  = fail-closed 报错
//
// 为什么不是"最小空闲":活着的节点都占着低位,唯一空出来的低位一定是**刚死或刚冻**
// 的那一个 —— 最小空闲位挑出来的槽,恰好是前任最可能还活着的槽(设计稿 §1.1)。
// 隔离期 Q 让"前任解冻后继续发号"与"继任者申领同一槽"在时间上永不重叠:
// 前任最晚停发时刻 T_ack + F < 继任者最早申领时刻 wm + Q(F = Q/2)。

// selectSlot 是纯函数:不碰 etcd,只做选择。返回 (slot, true);无候选返回 (0, false)。
//
// wm 里 now 之后的水位(跨机时钟偏差、前任前推量)同样不满足 now − wm ≥ Q,
// 一律跳过 —— 用有符号比较避免 uint 下溢把"未来"算成"无穷久"。
func selectSlot(used map[uint64]bool, wm map[uint64]uint64, now, quarantineSec, maxSlot uint64) (uint64, bool) {
	var (
		best   uint64
		bestWm uint64
		found  bool
	)
	for s := uint64(0); s <= maxSlot; s++ {
		if used[s] {
			continue
		}
		w := wm[s]
		if w != 0 {
			if now < w || now-w < quarantineSec {
				continue
			}
		}
		if !found || w < bestWm {
			best, bestWm, found = s, w, true
		}
		if w == 0 {
			// 0 是最小可能水位,且按 s 升序扫,第一个从没用过的槽已经是全局最优。
			break
		}
	}
	return best, found
}

// mergeWatermarks 把多来源的水位按槽取 max(新布局 key 与旧布局 key 并存的迁移期)。
// dst 为 nil 时新建。
func mergeWatermarks(dst, src map[uint64]uint64) map[uint64]uint64 {
	if dst == nil {
		dst = make(map[uint64]uint64, len(src))
	}
	for slot, w := range src {
		if w > dst[slot] {
			dst[slot] = w
		}
	}
	return dst
}
