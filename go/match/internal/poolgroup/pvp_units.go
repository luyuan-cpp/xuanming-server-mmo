package poolgroup

// PickPvpUnits 将FIFO单位装入两个等大的阵营，第一个单位固定在己方。
// indices和sides逐项对应；没有真人对手或完整单位无法装箱时返回nil。
// 每方最多5人，状态只有36个，单位绝不被拆成玩家重新分边。
func PickPvpUnits(sizes []int, teamSize int) (indices []int, sides []uint32) {
	if teamSize < 1 || teamSize > 5 || len(sizes) == 0 || sizes[0] < 1 || sizes[0] > teamSize {
		return nil, nil
	}
	type choice struct {
		index int
		side  uint32
	}
	var states [6][6][]choice
	states[sizes[0]][0] = []choice{{index: 0, side: 0}}
	for index, size := range sizes[1:] {
		if size < 1 || size > teamSize {
			continue
		}
		next := states
		for left := 0; left <= teamSize; left++ {
			for right := 0; right <= teamSize; right++ {
				current := states[left][right]
				if current == nil {
					continue
				}
				if left+size <= teamSize && next[left+size][right] == nil {
					next[left+size][right] = append(append([]choice(nil), current...), choice{index: index + 1, side: 0})
				}
				if right+size <= teamSize && next[left][right+size] == nil {
					next[left][right+size] = append(append([]choice(nil), current...), choice{index: index + 1, side: 1})
				}
			}
		}
		states = next
		if solution := states[teamSize][teamSize]; solution != nil {
			for _, member := range solution {
				indices = append(indices, member.index)
				sides = append(sides, member.side)
			}
			return indices, sides
		}
	}
	return nil, nil
}
