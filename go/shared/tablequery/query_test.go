package tablequery

import (
	"slices"
	"testing"

	pb "shared/generated/pb/table"
)

// query_test.go — 通用投影查询测试。重点钉三件事:
// 顺序契约(IDs/Values 加载序、DistinctValues 升序)、返回值是调用方独占副本、空表零值安全。

func items(ids ...uint32) []*pb.ItemTable {
	out := make([]*pb.ItemTable, 0, len(ids))
	for _, id := range ids {
		out = append(out, &pb.ItemTable{Id: id, MaxStackSize: 99})
	}
	return out
}

// TestIDsLoadOrder IDs 必须按加载序,而不是主键序、更不是 map 迭代序。
// (从 kvData map 迭代出来每次顺序都不同 → 下发客户端的列表抖动、测试 flaky。)
func TestIDsLoadOrder(t *testing.T) {
	rows := items(9, 6, 7)
	if got := IDs(rows); !slices.Equal(got, []uint32{9, 6, 7}) {
		t.Fatalf("IDs=%v,应按加载序 [9 6 7]", got)
	}
	// 反复调用必须完全一致(若实现改成 map 迭代,这里会随机红)
	for i := 0; i < 20; i++ {
		if got := IDs(rows); !slices.Equal(got, []uint32{9, 6, 7}) {
			t.Fatalf("第 %d 次 IDs=%v,顺序不稳定", i, got)
		}
	}
}

// TestIDsReturnsCallerOwnedCopy 返回值必须是调用方独占副本:
// 调用方 sort / 改写不得影响源行切片,也不得影响下一次调用。
func TestIDsReturnsCallerOwnedCopy(t *testing.T) {
	rows := items(9, 6, 7)

	got := IDs(rows)
	slices.Sort(got) // 调用方随意 sort
	got[0] = 12345   // 调用方随意改写

	if again := IDs(rows); !slices.Equal(again, []uint32{9, 6, 7}) {
		t.Fatalf("调用方改写污染了源数据,再次 IDs=%v", again)
	}
	if rows[0].GetId() != 9 {
		t.Fatalf("行被改动,rows[0].Id=%d", rows[0].GetId())
	}
}

// TestValuesProjection Values 是纯投影:不去重、长度与行数一致、保持加载序。
func TestValuesProjection(t *testing.T) {
	rows := []*pb.TestMultiKeyTable{
		{Id: 1, MStringKey: "b", MUint32Key: 20},
		{Id: 2, MStringKey: "a", MUint32Key: 10},
		{Id: 3, MStringKey: "b", MUint32Key: 20},
	}
	got := Values(rows, (*pb.TestMultiKeyTable).GetMStringKey)
	if !slices.Equal(got, []string{"b", "a", "b"}) {
		t.Fatalf("Values(MStringKey)=%v,应保留重复且按加载序", got)
	}
	if len(got) != len(rows) {
		t.Fatalf("Values 长度 %d 应等于行数 %d", len(got), len(rows))
	}
}

// TestDistinctValues 去重 + 升序;重复值只留一个,且与行的排列无关。
// 三种键类型(string / uint32 / int32)都要覆盖,证明泛型约束够宽。
func TestDistinctValues(t *testing.T) {
	rows := []*pb.TestMultiKeyTable{
		{Id: 1, MStringKey: "b", MUint32Key: 20, MInt32Key: -3},
		{Id: 2, MStringKey: "a", MUint32Key: 10, MInt32Key: 5},
		{Id: 3, MStringKey: "b", MUint32Key: 20, MInt32Key: -3},
	}

	if got := DistinctValues(rows, (*pb.TestMultiKeyTable).GetMStringKey); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("DistinctValues(MStringKey)=%v", got)
	}
	if got := DistinctValues(rows, (*pb.TestMultiKeyTable).GetMUint32Key); !slices.Equal(got, []uint32{10, 20}) {
		t.Fatalf("DistinctValues(MUint32Key)=%v", got)
	}
	// 有符号列:升序必须是 -3 < 5,而不是按无符号回绕
	if got := DistinctValues(rows, (*pb.TestMultiKeyTable).GetMInt32Key); !slices.Equal(got, []int32{-3, 5}) {
		t.Fatalf("DistinctValues(MInt32Key)=%v", got)
	}

	// 换个行序,键集合必须完全一致(升序的意义)
	shuffled := []*pb.TestMultiKeyTable{rows[2], rows[1], rows[0]}
	a := DistinctValues(rows, (*pb.TestMultiKeyTable).GetMStringKey)
	b := DistinctValues(shuffled, (*pb.TestMultiKeyTable).GetMStringKey)
	if !slices.Equal(a, b) {
		t.Fatalf("键集合应与行序无关:%v vs %v", a, b)
	}
}

// TestQueryEmptyTable 空表零值安全:一律返回空切片而非 nil panic。
func TestQueryEmptyTable(t *testing.T) {
	var rows []*pb.ItemTable
	if got := IDs(rows); len(got) != 0 {
		t.Fatalf("空表 IDs=%v", got)
	}
	if got := Values(rows, (*pb.ItemTable).GetMaxStackSize); len(got) != 0 {
		t.Fatalf("空表 Values=%v", got)
	}
	if got := DistinctValues(rows, (*pb.ItemTable).GetId); len(got) != 0 {
		t.Fatalf("空表 DistinctValues=%v", got)
	}
}

// TestQueryAcrossTables 泛型对不同表的行类型都成立(加新表零成本的实证)。
func TestQueryAcrossTables(t *testing.T) {
	if got := IDs(items(10001, 10002)); !slices.Equal(got, []uint32{10001, 10002}) {
		t.Fatalf("IDs(item)=%v", got)
	}
	monsters := []*pb.MonsterTable{{Id: 2001}, {Id: 2002}}
	if got := IDs(monsters); !slices.Equal(got, []uint32{2001, 2002}) {
		t.Fatalf("IDs(monster)=%v", got)
	}
	missions := []*pb.MissionTable{
		{Id: 1, MissionType: 3}, {Id: 2, MissionType: 1}, {Id: 3, MissionType: 3},
	}
	// 「表里一共有哪几类任务」——本包要解决的原始问题
	if got := DistinctValues(missions, (*pb.MissionTable).GetMissionType); !slices.Equal(got, []uint32{1, 3}) {
		t.Fatalf("DistinctValues(mission.MissionType)=%v,应为 [1 3]", got)
	}
}
