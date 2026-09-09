package snowflakealloc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shared/snowflake"
)

func newTestHandle(slot uint64) *Handle {
	return &Handle{
		Kind:     "unit",
		Slot:     slot,
		WorkerID: slot,
		UUID:     "uuid-unit",
		lost:     make(chan struct{}),
		fence:    snowflake.NewFenceClock(time.Minute),
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type fakeFencer struct{ fenced bool }

func (f *fakeFencer) Fence() { f.fenced = true }

// 槽被别的 uuid 接管 = 真的失去所有权,必须关闭 Lost(),并先把登记的发号器全部 fence
// (防御纵深:不等消费方的 <-Lost() 协程被调度)。
func TestHandle_OwnershipLostSignalsLostAndFences(t *testing.T) {
	h := newTestHandle(7)
	f := &fakeFencer{}
	h.AttachFencer(f)

	h.onOwnershipLost("slot key now holds uuid other")

	if !isClosed(h.Lost()) {
		t.Fatal("ownership loss must close Lost()")
	}
	if !f.fenced {
		t.Fatal("attached generators must be fenced before Lost() closes")
	}
}

// 正常 Close() 期间的归属判定不是身份丢失,不能误报 ——
// 否则每次优雅停机都会触发调用方的"停服 + os.Exit(1)"分支。
func TestHandle_CloseIsNotReportedAsLost(t *testing.T) {
	h := newTestHandle(7)
	h.closing.Store(true) // Close() 做的第一件事

	h.onOwnershipLost("watch event during shutdown")

	if isClosed(h.Lost()) {
		t.Fatal("graceful Close() must not be reported as lost ownership")
	}
}

// 重复触发必须幂等(close 一个已关闭的 channel 会 panic)。
func TestHandle_LostIsIdempotent(t *testing.T) {
	h := newTestHandle(7)

	h.onOwnershipLost("first")
	h.onOwnershipLost("second")

	if !isClosed(h.Lost()) {
		t.Fatal("Lost() should stay closed")
	}
}

// nil Handle 上取信号不应崩(调用方可能在分配失败后仍然监听)。
func TestHandle_NilLostIsSafe(t *testing.T) {
	var h *Handle
	if h.Lost() != nil {
		t.Fatal("nil handle should report a nil channel")
	}
	h.Close() // 也不应崩
}

// 日志三字段是排障契约:kind / worker=c<cluster>:<slot> / inc=<revision>。
func TestHandle_LogFields(t *testing.T) {
	h := newTestHandle(5)
	h.Cluster = 3
	h.incarnation.Store(1234)
	h.registered.Store(true)
	if got := h.LogFields(); got != "kind=unit worker=c3:5 inc=1234" {
		t.Fatalf("LogFields=%q", got)
	}
	h.registered.Store(false)
	if got := h.LogFields(); !strings.Contains(got, "unregistered") {
		t.Fatalf("未注册状态必须在日志里可见: %q", got)
	}
}

// Options 默认值与校验:Q=4h、F=2h、F 必须 < Q、cluster 不得越过位宽、MaxSlot 不得越过 SlotBits。
func TestOptions_ResolveDefaultsAndValidation(t *testing.T) {
	r, err := Options{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if r.ttl != 60 || r.quarantine != 4*time.Hour || r.fenceAfter != 2*time.Hour {
		t.Fatalf("defaults: ttl=%d Q=%v F=%v", r.ttl, r.quarantine, r.fenceAfter)
	}
	if r.clusterBits != 5 || r.slotBits != 12 || r.maxSlot != 4095 {
		t.Fatalf("defaults: cluster%d slot%d max=%d", r.clusterBits, r.slotBits, r.maxSlot)
	}

	if _, err := (Options{Quarantine: time.Hour, FenceAfter: time.Hour}).resolve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("F >= Q 必须拒绝, got %v", err)
	}
	// F 必须 ≤ Q/2,不是"只要 F < Q"。Q − F 是吃时钟偏差 / 排空 / 冻结感知延迟的余量,
	// F=0.6Q 在数学上仍满足 F<Q 却几乎没有余量(设计稿 §1.2 直接定 F = Q/2)。
	if _, err := (Options{Quarantine: time.Hour, FenceAfter: 36 * time.Minute}).resolve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("F > Q/2 必须拒绝, got %v", err)
	}
	if r, err := (Options{Quarantine: time.Hour, FenceAfter: 30 * time.Minute}).resolve(); err != nil || r.fenceAfter != 30*time.Minute {
		t.Fatalf("F == Q/2 必须接受, got F=%v err=%v", r.fenceAfter, err)
	}
	if _, err := (Options{ClusterID: 32}).resolve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("cluster 32 越过 5 位必须拒绝, got %v", err)
	}
	if _, err := (Options{MaxSlot: 4096}).resolve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("MaxSlot 4096 越过 12 位必须拒绝, got %v", err)
	}

	// login 的 13 位布局 [cluster3][slot10]
	lr, err := (Options{ClusterBits: 3, SlotBits: 10, ClusterID: 7}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if lr.maxSlot != 1023 {
		t.Fatalf("login MaxSlot=%d, 期望 1023", lr.maxSlot)
	}
	if _, err := (Options{ClusterBits: 3, SlotBits: 10, ClusterID: 8}).resolve(); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("login cluster 8 越过 3 位必须拒绝, got %v", err)
	}
}

// 缓存路径带 kind 与亲和键,同主机多实例(host#port / host_port)各自一份;非法字符替换。
func TestDefaultCachePath(t *testing.T) {
	if DefaultCachePath("", "guild", "h") != "" {
		t.Fatal("dir 为空必须关闭缓存")
	}
	got := DefaultCachePath("d", "login-player", "host#127.0.0.1:50500")
	want := filepath.Join("d", "snowflake-login-player-host_127.0.0.1_50500.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
