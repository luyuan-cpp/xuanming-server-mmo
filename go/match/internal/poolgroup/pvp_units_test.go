package poolgroup

import (
	"reflect"
	"testing"
)

func TestPickPvpUnitsAllSideSizes(t *testing.T) {
	for size := 1; size <= 5; size++ {
		indices, sides := PickPvpUnits([]int{size, size}, size)
		if !reflect.DeepEqual(indices, []int{0, 1}) || !reflect.DeepEqual(sides, []uint32{0, 1}) {
			t.Fatalf("%dv%d完整队伍必须分在两边: indices=%v sides=%v", size, size, indices, sides)
		}
	}
}

func TestPickPvpUnitsKeepsPartiesAndCanFillWithSolo(t *testing.T) {
	indices, sides := PickPvpUnits([]int{3, 2, 4, 1}, 5)
	if !reflect.DeepEqual(indices, []int{0, 1, 2, 3}) {
		t.Fatalf("应使用两个完整队伍与路人填满双方: %v", indices)
	}
	var counts [2]int
	for i, index := range indices {
		counts[sides[i]] += []int{3, 2, 4, 1}[index]
	}
	if counts != [2]int{5, 5} || sides[0] != sides[1] || sides[2] != sides[3] || sides[0] == sides[2] {
		t.Fatalf("队伍不能拆分且双方必须相等: sides=%v counts=%v", sides, counts)
	}
}

func TestPickPvpUnitsSkipsUnplaceableUnits(t *testing.T) {
	indices, sides := PickPvpUnits([]int{3, 4, 3}, 3)
	if !reflect.DeepEqual(indices, []int{0, 2}) || !reflect.DeepEqual(sides, []uint32{0, 1}) {
		t.Fatalf("不能让放不下的单位挡住可成局的队伍: %v %v", indices, sides)
	}
}

func TestPickPvpUnitsDoesNotSplitOrInventOpponents(t *testing.T) {
	for _, input := range []struct {
		sizes  []int
		target int
	}{
		{[]int{5}, 5}, {[]int{3, 3, 2}, 4}, {[]int{2, 2}, 3},
		{nil, 1}, {[]int{0, 1}, 1}, {[]int{1, 1}, 0}, {[]int{1, 1}, 6},
	} {
		if indices, sides := PickPvpUnits(input.sizes, input.target); indices != nil || sides != nil {
			t.Fatalf("无法按完整单位分成双方时必须继续等待: %v target=%d got=%v/%v", input.sizes, input.target, indices, sides)
		}
	}
}
