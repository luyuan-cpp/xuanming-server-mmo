package snowflake

import (
	"errors"
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
		id, err := previousHolder.Generate()
		if err != nil {
			t.Fatal(err)
		}
		minted = append(minted, id)
	}

	// 同一秒内进程重启,拿回同一个 worker id。
	restarted := NewNode(nodeID)
	first, err := restarted.Generate()
	if err != nil {
		t.Fatal(err)
	}

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
		id, err := n.Generate()
		if err != nil {
			t.Fatal(err)
		}
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
	prev, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		id, err := n.Generate()
		if err != nil {
			t.Fatal(err)
		}
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
		id, err := n.Generate()
		if err != nil {
			t.Fatal(err)
		}
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
		id, err := n.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("ID not monotonic: prev=%d, got=%d at iteration %d", prev, id, i)
		}
		prev = id
	}
}

func TestGenerate_EmbeddedNodeID(t *testing.T) {
	nodeID := uint64(42)
	n := NewNode(nodeID)
	id, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}

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
		id1, err := n1.Generate()
		if err != nil {
			t.Fatal(err)
		}
		id2, err := n2.Generate()
		if err != nil {
			t.Fatal(err)
		}
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
				v, err := n.Generate()
				if err != nil {
					panic(err)
				}
				ids[i] = v
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
	id, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	extracted := (id >> nodeShift) & NodeMask
	if extracted != NodeMask {
		t.Fatalf("expected nodeID %d, got %d", NodeMask, extracted)
	}
}

// 启动 guard 把 step 池预置为满,好让首个 ID 必然落在构造秒之后 —— 于是**每个进程启动时**
// 第一次 Generate() 都必然走进"step 池耗尽"分支。那是设计动作,不是撞容量墙:
// 若它也报 ERROR,运维会被每次启动的噪声训练成忽略这条告警,真正撞到 32768/s 上限时反而看不见。
// 本用例钉住"guard 预置只被消化一次、且不会复活",即容量告警的抑制范围恰好是那一次。
func TestNewNode_BootGuardExhaustionIsNotACapacityAlert(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()

	current := uint64(1000)
	nowEpoch = func() uint64 { return current }

	n := NewNode(3)
	if !n.bootGuardPending {
		t.Fatal("NewNode 必须标记 guard 预置,否则首次发号会被误报成容量耗尽")
	}

	// 首个 Generate:走耗尽分支(step 池被 guard 置满),消化掉标记。
	// 时钟同时前进一秒,避免 waitNextTime 真的空转。
	current = 1001
	first, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if n.bootGuardPending {
		t.Fatal("首个 Generate 必须消化掉 guard 标记")
	}
	if got := first >> timeShift; got <= 1000 {
		t.Fatalf("首个 ID 的时间段=%d,必须严格晚于构造秒 1000", got)
	}

	// 之后真正把本秒发满,必须重新被视为容量事件(标记不得复活)。
	n.step = stepMask
	current = 1002
	if _, err := n.Generate(); err != nil {
		t.Fatal(err)
	}
	if n.bootGuardPending {
		t.Fatal("guard 标记不得复活,否则真实容量耗尽会被永久静音")
	}
}

// 失去 worker id 租约后必须**一个号都发不出去**。
//
// 这条不能靠"收到 Lost() 就停服"兜底:go-zero 的 zrpc.RpcServer.Stop() 实测
// (v1.9.2 / v1.10.0 同)只有一行 logx.Close(),既不拒新请求也不排空在途 ——
// 调它等于什么都没做。而且 scene_manager 有一半发号点在后台 ticker 上,
// 根本不经过 gRPC。所以正确性只能由发号器自身 fail-closed 保证。
func TestFence_RefusesToMintAfterLeaseLoss(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()
	current := uint64(2000)
	nowEpoch = func() uint64 { return current }

	n := NewNode(11)
	current = 2001
	if _, err := n.Generate(); err != nil {
		t.Fatalf("fence 前必须能正常发号: %v", err)
	}

	n.Fence()

	if !n.IsFenced() {
		t.Fatal("Fence() 后 IsFenced 必须为真")
	}
	for i := 0; i < 3; i++ {
		current += 1 // 时钟继续前进,证明拒发与时间无关
		id, err := n.Generate()
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("第 %d 次:fence 后必须返回 ErrFenced, got id=%d err=%v", i, id, err)
		}
		if id != 0 {
			t.Fatalf("第 %d 次:拒发时必须返回 0,不能交出一个看起来合法的号, got %d", i, id)
		}
	}
}

// Fence 幂等,且可以从别的 goroutine 调(失租是 keepalive goroutine 发现的,
// 而发号可能正阻塞在 waitNextTime 里,不能要求它先拿到 mu)。
func TestFence_IsIdempotentAndConcurrencySafe(t *testing.T) {
	n := NewNode(12)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.Fence()
		}()
	}
	wg.Wait()

	if _, err := n.Generate(); !errors.Is(err, ErrFenced) {
		t.Fatalf("并发 Fence 后必须稳定拒发, got %v", err)
	}
}

// SetGuardTime 是**地板**语义,不是"设成这个值"。
// 它存在的理由:NewNode 的启动 guard 只做"不在构造那一秒发号"的点排除,以本机墙钟为基准,
// 顶不住"前任高水位 > 本机当前秒"——跨机接管时新持有者时钟落后、本机被 NTP 回拨、
// 或前任发满 step 池借过逻辑秒(水位跑到墙钟前面),三者都会让新进程重发前任已发出的号。
func TestSetGuardTime_IsAFloorNotAnAssignment(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()
	current := uint64(1000)
	nowEpoch = func() uint64 { return current }

	n := NewNode(5)

	// 地板低于当前高水位 ⇒ 不生效,绝不把高水位往回写。
	n.SetGuardTime(500)
	current = 1001
	id, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if got := id >> timeShift; got < 1000 {
		t.Fatalf("低地板不得把高水位往回写: 时间段=%d", got)
	}

	// 地板高于当前高水位 ⇒ 生效,之后的号必须严格晚于地板。
	n.SetGuardTime(5000)
	current = 5001
	id2, err := n.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if got := id2 >> timeShift; got <= 5000 {
		t.Fatalf("高地板未生效: 时间段=%d, 必须 > 5000", got)
	}
}

// 跨机接管 + 新持有者时钟落后:没有持久水位时会静默重发前任的号;
// 注入前任高水位作地板后必须完全不重叠。这是 snowflakealloc 持久水位(<prefix>/snowflake_guard/<id>)
// 存在的唯一理由,也是 C++ SetGuardTime(max(now,lastTs)) 的等价保证。
func TestSetGuardTime_PreventsReplayOnClockSkewedTakeover(t *testing.T) {
	restore := nowEpoch
	defer func() { nowEpoch = restore }()

	// 前任:时钟 1000..1010,发一批号,高水位落在 1010。
	current := uint64(1000)
	nowEpoch = func() uint64 { return current }
	prev := NewNode(7)
	minted := map[uint64]bool{}
	for current = 1001; current <= 1010; current++ {
		for i := 0; i < 5; i++ {
			id, err := prev.Generate()
			if err != nil {
				t.Fatal(err)
			}
			minted[id] = true
		}
	}
	prevWatermark := current - 1 // 前任最后发号所在的秒,即会被写进 etcd 的水位

	// 接管者:同一个 worker id,但**本机时钟落后 5 秒**。
	current = 1005
	taker := NewNode(7)
	taker.SetGuardTime(prevWatermark) // 注入前任水位作地板

	for current = 1005; current <= 1012; current++ {
		for i := 0; i < 5; i++ {
			id, err := taker.Generate()
			if err != nil {
				t.Fatal(err)
			}
			if minted[id] {
				t.Fatalf("接管者重发了前任已发出的 ID %d(本机时钟落后 %d 秒):"+
					"地板没生效,持久水位失去意义", id, prevWatermark-current)
			}
		}
	}
}
