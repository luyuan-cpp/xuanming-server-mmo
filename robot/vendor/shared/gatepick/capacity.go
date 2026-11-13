// capacity.go — 一批 gate 到水位为止一共能接多少人,给区级准入(login 排队的 FreeSlots)的区容量封顶。
//
// 为什么要封顶(2026-11-10):区容量是运维配的一个数(网关区服目录 zone_config.capacity,缺省 45000),gate 台数写在另一份
// 清单里(deploy/k8s/zones*.json / *.yaml,每区 1~3 台,每台 GateMaxConnections 20000),两者之间没有任何机械关联。
// 容量比 gate 承载大时,排队照样按容量放人,玩家拿着票连上 gate 才被 GateMaxConnections 拒掉;gate 拒连是 forceClose,
// 不回任何提示,玩家看到的只是断线。封顶之后,在接客的 gate 都到水位时排队就开始拦人,gate 的硬上限退回兜底。
//
// 水位就是挑 gate 用的那条(FullPercent):一台 gate 到水位就不再优先接新玩家,区级准入也只放到这条线,
// 两处对「满」的口径一致;余下 10% 照旧留给人数上报滞后。
package gatepick

// WatermarkCapacity 返回自报上限为 maxConnections 的 gate 到水位时的人数,即让 Full(n, maxConnections) 成立的最小 n
// (ceil(maxConnections × FullPercent / 100),整数算)。maxConnections 为 0(没声明上限)时返回 0,调用方按「不设上限」处理。
func WatermarkCapacity(maxConnections uint32) uint64 {
	if maxConnections == 0 {
		return 0
	}
	return (uint64(maxConnections)*FullPercent + 99) / 100
}

// AdmissionCapacity 返回一批 gate 到水位为止一共能接多少人(总量,不扣已经在线的人);bounded 为 false 表示不设上限。
//
// 只数在接新玩家的那一档。分档与 Eligible 相同,只是不看人数:到水位的 gate 只是已经接满,它的水位容量照样算。
//  1. 没停写、没排空的;
//  2. 没有时退到没停写的:Eligible 这时会把新玩家分给排空中的 gate,容量也要数上它们;
//  3. 全部停写时数全部,理由同 Eligible 退到全部(一律当零容量会把一次可能的误判放大成整区进不去)。
//
// 数进来的 gate 里有没声明上限的(MaxConnections 为 0:dev 下不设上限的 gate、旧版 gate)时整体不设上限,返回 (0, false);
// 没数进来的(停写、排空中的)不看它们的上限。items 为空时返回 (0, true):没有 gate 就没有容量,调用方先报「没有候选」。
//
// 调用方数在线人数时仍要数全部候选:排空中、停写的 gate 上的人还占着连接,稍后会挪到在接客的 gate 上,
// 所以容量只数在接客的、在线数全部,两边一减正好给他们留出要挪过去的位置。
func AdmissionCapacity[T any](items []T, state func(T) State) (capacity uint64, bounded bool) {
	serving, reachable := 0, 0
	for _, it := range items {
		s := state(it)
		if s.Stale {
			continue
		}
		reachable++
		if !s.Draining {
			serving++
		}
	}

	counted := func(State) bool { return true } // 全部停写:数全部
	switch {
	case serving > 0:
		counted = func(s State) bool { return !s.Stale && !s.Draining }
	case reachable > 0:
		counted = func(s State) bool { return !s.Stale }
	}

	for _, it := range items {
		s := state(it)
		if !counted(s) {
			continue
		}
		if s.MaxConnections == 0 {
			return 0, false
		}
		capacity += WatermarkCapacity(s.MaxConnections)
	}
	return capacity, true
}
