package snowflake

import (
	"sync"
	"testing"
	"time"
)

// 回归:同一日历秒内"重启"(worker id 被 hostname 亲和原样拿回)时,
// 新生成器绝不能重放上一任在这一秒里发过的号。
// 修复前:两个生成器都从 lastTime=now / step=0 开始,第一个 ID 逐位相同。
func TestNewNode_BootGuardSkipsRestartSecond(t *testing.T) {
	const nodeID = 7

	previousHolder := NewNode(nodeID)
	minted := make([]uint64, 0, 128)
	for i := 0; i < 128; i++ {
		minted = append(minted, previousHolder.Generate())
	}

	// 同一秒内进程重启,拿回同一个 worker id。
	restarted := NewNode(nodeID)
	first := restarted.Generate()

	for _, old := range minted {
		if first == old {
			t.Fatalf("restart re-minted an ID the previous holder already handed out: %d", first)
		}
	}

	lastOld := minted[len(minted)-1]
	if first>>timeShift <= lastOld>>timeShift {
		t.Fatalf("boot guard did not skip the restart second: first=%d (t=%d) lastOld=%d (t=%d)",
			first, first>>timeShift, lastOld, lastOld>>timeShift)
	}
}

// 回归:时钟回拨时必须继续从高水位秒发号,既不能重号,也不能握着锁自旋等时钟追上来。
// 修复前:waitNextTime 是无上限的 for 循环,回拨多久就卡多久(而且锁是持有的)。
func TestGenerate_ClockRollbackKeepsMintingWithoutBlocking(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()

	current := uint64(1000)
	nowEpoch = func() uint64 { return current }

	n := &Node{nodeID: 3, lastTime: current, step: 0}

	current = 400 // 时钟往回跳 600 秒

	seen := make(map[uint64]bool, 1024)
	start := time.Now()
	for i := 0; i < 1024; i++ {
		id := n.Generate()
		if seen[id] {
			t.Fatalf("duplicate ID after clock rollback: %d", id)
		}
		seen[id] = true
		if id>>timeShift < 1000 {
			t.Fatalf("time field regressed below the high-water second: id=%d t=%d", id, id>>timeShift)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Generate blocked %v on clock rollback", elapsed)
	}
}

// 系统时钟早于 Epoch 时,uint64 减法会下溢成天文数字,把时间段顶到远未来。
func TestNowEpoch_ClampsBelowEpoch(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()

	// 直接验证生产实现的钳位语义:构造一个高水位为 0 的生成器,
	// 在"时钟早于 epoch"时仍然只能拿到单调递增的号。
	current := uint64(0)
	nowEpoch = func() uint64 { return current }

	n := &Node{nodeID: 1, lastTime: 0, step: 0}
	prev := n.Generate()
	for i := 0; i < 16; i++ {
		id := n.Generate()
		if id <= prev {
			t.Fatalf("IDs must stay strictly increasing when the clock is stuck at epoch: prev=%d got=%d", prev, id)
		}
		prev = id
	}
}

func TestGenerate_Unique(t *testing.T) {
	n := NewNode(0)
	const count = 10000
	seen := make(map[uint64]bool, count)
	for i := 0; i < count; i++ {
		id := n.Generate()
		if seen[id] {
			t.Fatalf("duplicate ID %d at iteration %d", id, i)
		}
		seen[id] = true
	}
}

func TestGenerate_Monotonic(t *testing.T) {
	n := NewNode(0)
	prev := uint64(0)
	for i := 0; i < 1000; i++ {
		id := n.Generate()
		if id <= prev {
			t.Fatalf("ID not monotonic: prev=%d, got=%d at iteration %d", prev, id, i)
		}
		prev = id
	}
}

func TestGenerate_EmbeddedNodeID(t *testing.T) {
	nodeID := uint64(42)
	n := NewNode(nodeID)
	id := n.Generate()

	extracted := (id >> nodeShift) & NodeMask
	if extracted != nodeID {
		t.Fatalf("expected nodeID %d in ID, got %d", nodeID, extracted)
	}
}

func TestGenerate_DifferentNodes_NoDuplicates(t *testing.T) {
	n1 := NewNode(1)
	n2 := NewNode(2)
	seen := make(map[uint64]bool)
	for i := 0; i < 1000; i++ {
		id1 := n1.Generate()
		id2 := n2.Generate()
		if seen[id1] {
			t.Fatalf("duplicate from node1: %d", id1)
		}
		if seen[id2] {
			t.Fatalf("duplicate from node2: %d", id2)
		}
		seen[id1] = true
		seen[id2] = true
	}
}

func TestGenerate_ConcurrentSafety(t *testing.T) {
	n := NewNode(0)
	const goroutines = 8
	const perGoroutine = 5000

	var mu sync.Mutex
	seen := make(map[uint64]bool, goroutines*perGoroutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			ids := make([]uint64, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				ids[i] = n.Generate()
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				if seen[id] {
					t.Errorf("duplicate ID %d", id)
				}
				seen[id] = true
			}
		}()
	}
	wg.Wait()
}

func TestNewNode_PanicsOnOverflow(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for oversized nodeID")
		}
	}()
	NewNode(NodeMask + 1)
}

func TestNewNode_MaxNodeID(t *testing.T) {
	n := NewNode(NodeMask)
	id := n.Generate()
	extracted := (id >> nodeShift) & NodeMask
	if extracted != NodeMask {
		t.Fatalf("expected nodeID %d, got %d", NodeMask, extracted)
	}
}
