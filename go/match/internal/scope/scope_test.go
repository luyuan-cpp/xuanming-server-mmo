package scope

import "testing"

var testSchedule = Schedule{GroupAfterMs: 10_000, GlobalAfterMs: 20_000}

func TestCommonLevel(t *testing.T) {
	cases := []struct {
		name  string
		nodes []Node
		want  Level
	}{
		{"同 zone", []Node{{Zone: 5, Group: 1}, {Zone: 5, Group: 1}}, LevelZone},
		{"单个节点", []Node{{Zone: 5, Group: 1}}, LevelZone},
		{"同大区不同 zone", []Node{{Zone: 5, Group: 1}, {Zone: 7, Group: 1}}, LevelGroup},
		{"不同大区", []Node{{Zone: 5, Group: 1}, {Zone: 205, Group: 2}}, LevelGlobal},
		{"都没登记大区且不同 zone", []Node{{Zone: 5}, {Zone: 7}}, LevelGlobal},
		{"一个登记一个没登记", []Node{{Zone: 5, Group: 1}, {Zone: 7}}, LevelGlobal},
		{"没登记大区但同 zone", []Node{{Zone: 5}, {Zone: 5}}, LevelZone},
	}
	for _, c := range cases {
		got, err := CommonLevel(c.nodes)
		if err != nil || got != c.want {
			t.Errorf("%s: got (%v,%v) want %v", c.name, got, err, c.want)
		}
	}
}

func TestCommonLevelRejectsMissingZone(t *testing.T) {
	if _, err := CommonLevel(nil); err != ErrInvalidNode {
		t.Fatalf("空集合应拒绝, got %v", err)
	}
	if _, err := CommonLevel([]Node{{Zone: 5, Group: 1}, {Zone: 0, Group: 1}}); err != ErrInvalidNode {
		t.Fatalf("缺 home zone 应拒绝, got %v", err)
	}
}

func TestScheduleAllowedByWait(t *testing.T) {
	cases := []struct {
		wait uint64
		want Level
	}{
		{0, LevelZone}, {9_999, LevelZone}, {10_000, LevelGroup}, {19_999, LevelGroup}, {20_000, LevelGlobal}, {1 << 40, LevelGlobal},
	}
	for _, c := range cases {
		if got := testSchedule.AllowedByWait(c.wait); got != c.want {
			t.Errorf("wait=%d got %v want %v", c.wait, got, c.want)
		}
	}
	if got := (Schedule{}).AllowedByWait(0); got != LevelGlobal {
		t.Errorf("阈值全 0 应入队即全服, got %v", got)
	}
	for level := LevelZone; int(level) < LevelCount; level++ {
		if got := testSchedule.AllowedByWait(testSchedule.MinWaitMs(level)); got < level {
			t.Errorf("等满 MinWaitMs(%v) 后应至少允许该层, got %v", level, got)
		}
	}
}

func TestScheduleValidate(t *testing.T) {
	if err := testSchedule.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Schedule{GroupAfterMs: 2, GlobalAfterMs: 1}).Validate(); err == nil {
		t.Fatal("大区阈值大于全服阈值应拒绝")
	}
}

func TestWaitMsClampsClockSkew(t *testing.T) {
	if WaitMs(1_000, 500) != 0 || WaitMs(1_000, 1_000) != 0 || WaitMs(1_000, 1_500) != 500 {
		t.Fatal("WaitMs 计算错误")
	}
}

func TestUnitAllowedTakesShortestWaitAndOwnSpan(t *testing.T) {
	now := uint64(100_000)
	fresh := Member{Node: Node{Zone: 5, Group: 1}, EnqueuedMs: now}
	old := Member{Node: Node{Zone: 5, Group: 1}, EnqueuedMs: now - 15_000}
	got, err := UnitAllowed(testSchedule, []Member{old, fresh}, now)
	if err != nil || got != LevelZone {
		t.Fatalf("成员等待取最短：got (%v,%v)", got, err)
	}
	// 跨 zone 的预组队自身已在大区层，不必等待。
	mixed := []Member{{Node: Node{Zone: 5, Group: 1}, EnqueuedMs: now}, {Node: Node{Zone: 7, Group: 1}, EnqueuedMs: now}}
	got, err = UnitAllowed(testSchedule, mixed, now)
	if err != nil || got != LevelGroup {
		t.Fatalf("跨 zone 预组队自身范围：got (%v,%v)", got, err)
	}
	if _, err = UnitAllowed(testSchedule, nil, now); err != ErrInvalidNode {
		t.Fatal("空单位应拒绝")
	}
}

func TestAdmitMutualConsent(t *testing.T) {
	now := uint64(100_000)
	unit := func(zone, group uint32, waitedMs uint64) []Member {
		return []Member{{Node: Node{Zone: zone, Group: group}, EnqueuedMs: now - waitedMs}}
	}
	cases := []struct {
		name  string
		units [][]Member
		ok    bool
		level Level
	}{
		{"同 zone 刚入队即可成组", [][]Member{unit(5, 1, 0), unit(5, 1, 0)}, true, LevelZone},
		{"同大区双方都等满大区阈值", [][]Member{unit(5, 1, 12_000), unit(7, 1, 10_000)}, true, LevelGroup},
		{"同大区但对方刚入队：不成组", [][]Member{unit(5, 1, 12_000), unit(7, 1, 0)}, false, 0},
		{"跨大区双方等满全服阈值", [][]Member{unit(5, 1, 25_000), unit(205, 2, 20_000)}, true, LevelGlobal},
		{"跨大区一方只到大区层：不成组", [][]Member{unit(5, 1, 25_000), unit(205, 2, 15_000)}, false, 0},
		{"刚入队的同 zone 人不能被拖进跨 zone 组", [][]Member{unit(5, 1, 12_000), unit(5, 1, 0), unit(7, 1, 12_000)}, false, 0},
		{"单个单位不受范围约束", [][]Member{{{Node: Node{Zone: 5, Group: 1}, EnqueuedMs: now}, {Node: Node{Zone: 205, Group: 2}, EnqueuedMs: now}}}, true, LevelGlobal},
		{"成员缺 home zone", [][]Member{unit(5, 1, 0), unit(0, 1, 0)}, false, 0},
		{"空单位", [][]Member{unit(5, 1, 0), {}}, false, 0},
	}
	for _, c := range cases {
		level, ok := Admit(testSchedule, c.units, now)
		if ok != c.ok || (ok && level != c.level) {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, level, ok, c.level, c.ok)
		}
	}
}

func TestDirectory(t *testing.T) {
	dir, err := NewDirectory([]Range{
		{GroupID: 2, ZoneBegin: 101, ZoneEnd: 200},
		{GroupID: 1, ZoneBegin: 1, ZoneEnd: 100},
		{GroupID: 1, ZoneBegin: 301, ZoneEnd: 310}, // 同一大区的第二段
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[uint32]uint32{1: 1, 100: 1, 101: 2, 200: 2, 201: 0, 300: 0, 301: 1, 310: 1, 311: 0, 0: 0}
	for zone, want := range cases {
		if got := dir.GroupOf(zone); got != want {
			t.Errorf("zone %d: got %d want %d", zone, got, want)
		}
	}
	if node := dir.NodeOf(150); node != (Node{Zone: 150, Group: 2}) {
		t.Errorf("NodeOf: %+v", node)
	}
	var nilDir *Directory
	if nilDir.GroupOf(5) != 0 {
		t.Error("空目录应视为全部未登记")
	}
}

func TestNewDirectoryRejectsBadRows(t *testing.T) {
	bad := [][]Range{
		{{GroupID: 0, ZoneBegin: 1, ZoneEnd: 2}},
		{{GroupID: 1, ZoneBegin: 0, ZoneEnd: 2}},
		{{GroupID: 1, ZoneBegin: 3, ZoneEnd: 2}},
		{{GroupID: 1, ZoneBegin: 1, ZoneEnd: 10}, {GroupID: 2, ZoneBegin: 10, ZoneEnd: 20}},
	}
	for i, rows := range bad {
		if _, err := NewDirectory(rows); err == nil {
			t.Errorf("case %d 应拒绝", i)
		}
	}
}

func TestLevelString(t *testing.T) {
	want := []string{"zone", "group", "global"}
	for i, name := range want {
		if Level(i).String() != name {
			t.Errorf("Level(%d).String()=%q", i, Level(i).String())
		}
	}
	if Level(LevelCount).String() != "invalid" {
		t.Error("越界层级应为 invalid")
	}
}
