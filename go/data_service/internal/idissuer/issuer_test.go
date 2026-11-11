package idissuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testAuthority 是直接改值时本地记录默认的世系(dev 形态下第一次预留会照它认领登记处)。
const testAuthority = "0123456789abcdef0123456789abcdef"

// mintCounter 让所有 harness 生成的世系互不相同,两个部署环境不会碰巧拿到同一个。
var mintCounter atomic.Uint64

func mintAuthority() (string, error) {
	return fmt.Sprintf("%032x", mintCounter.Add(1)), nil
}

// deploymentCounter 让每个 harness 代表的部署环境(一个 K8s 集群)身份互不相同。
var deploymentCounter atomic.Uint64

func mintDeployment() string {
	return fmt.Sprintf("k8s:cluster-%d", deploymentCounter.Add(1))
}

// memStore 是内存里的一份本地预留记录,可以注入回退、丢失和写失败。
type memStore struct {
	mu        sync.Mutex
	value     uint64
	authority string
	revision  int64
	exists    bool
	readErr   error
	writeErr  error
	// beforeWrite 在 CAS 比较之前调用,用来插入并发写。
	beforeWrite func()
	closed      bool
	clusterID   uint64
}

func (m *memStore) Read(context.Context) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return Record{}, m.readErr
	}
	return Record{Value: m.value, Authority: m.authority, Revision: m.revision, Exists: m.exists, ClusterID: m.clusterID}, nil
}

func (m *memStore) CompareAndSwap(_ context.Context, prev Record, value uint64, authority string) (bool, error) {
	m.mu.Lock()
	hook := m.beforeWrite
	m.beforeWrite = nil
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return false, m.writeErr
	}
	if m.exists != prev.Exists || m.revision != prev.Revision {
		return false, nil
	}
	if value == 0 || !ValidAuthority(authority) {
		return false, fmt.Errorf("invalid record value=%d authority=%q", value, authority)
	}
	m.value, m.authority, m.exists = value, authority, true
	m.revision++
	return true, nil
}

func (m *memStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// set 直接改值(模拟旧备份覆盖或别的副本写入),世系不变;原来没有世系时用 testAuthority。
func (m *memStore) set(value uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.authority == "" {
		m.authority = testAuthority
	}
	m.value, m.exists = value, true
	m.revision++
}

func (m *memStore) drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value, m.authority, m.exists = 0, "", false
	m.revision++
}

func (m *memStore) get() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value
}

func (m *memStore) has() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exists
}

func (m *memStore) setErrors(readErr, writeErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readErr, m.writeErr = readErr, writeErr
}

// memSnap 是一份本地记录在某一时刻的内容,用来模拟 etcd 快照与回档。
type memSnap struct {
	value     uint64
	authority string
	exists    bool
}

func (m *memStore) snapshot() memSnap {
	m.mu.Lock()
	defer m.mu.Unlock()
	return memSnap{value: m.value, authority: m.authority, exists: m.exists}
}

// restore 把记录恢复成快照里的内容。版本照样推进:回档之后,回档之前读到的版本不能再 CAS 成功。
func (m *memStore) restore(s memSnap) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value, m.authority, m.exists = s.value, s.authority, s.exists
	m.revision++
}

// clone 是一套新 etcd 从这份快照恢复出来的记录:内容相同,集群号不同。
func (s memSnap) clone(clusterID uint64) *memStore {
	return &memStore{value: s.value, authority: s.authority, exists: s.exists, revision: 1, clusterID: clusterID}
}

type memClaim struct {
	claim    Claim
	revision int64
}

// memRegistry 是内存里的登记处,语义和 EtcdRegistry 一致。
type memRegistry struct {
	mu          sync.Mutex
	entries     map[uint8]memClaim
	opened      bool
	next        uint32
	revision    int64
	clusterID   uint64
	readErr     error
	writeErr    error
	beforeWrite func()
	closed      bool
}

func newMemRegistry(clusterID uint64) *memRegistry {
	return &memRegistry{entries: map[uint8]memClaim{}, clusterID: clusterID}
}

func (m *memRegistry) runHook() {
	m.mu.Lock()
	hook := m.beforeWrite
	m.beforeWrite = nil
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (m *memRegistry) Read(_ context.Context, region uint8) (ClaimRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return ClaimRecord{}, m.readErr
	}
	e, ok := m.entries[region]
	if !ok {
		return ClaimRecord{ClusterID: m.clusterID}, nil
	}
	return ClaimRecord{Claim: e.claim, Revision: e.revision, Exists: true, ClusterID: m.clusterID}, nil
}

func (m *memRegistry) CompareAndSwap(_ context.Context, region uint8, prev ClaimRecord, next Claim) (bool, error) {
	m.runHook()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return false, m.writeErr
	}
	e, ok := m.entries[region]
	if ok != prev.Exists || (ok && e.revision != prev.Revision) {
		return false, nil
	}
	if !validClaim(next) {
		return false, fmt.Errorf("invalid claim %+v", next)
	}
	m.revision++
	m.entries[region] = memClaim{claim: next, revision: m.revision}
	return true, nil
}

func (m *memRegistry) ClaimNew(_ context.Context, region uint8, claim Claim) (bool, error) {
	m.runHook()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return false, m.writeErr
	}
	if !m.opened {
		return false, ErrRegistryNotOpened
	}
	if uint32(region) < m.next {
		return false, fmt.Errorf("%w: code %d is below the next claimable code %d", ErrRegionClaimed, region, m.next)
	}
	if uint32(region) > m.next {
		return false, fmt.Errorf("%w: the next claimable code is %d, this deployment is configured with %d", ErrRegionOutOfOrder, m.next, region)
	}
	if _, ok := m.entries[region]; ok {
		return false, nil
	}
	if claim.State != ClaimActive || !validClaim(claim) {
		return false, fmt.Errorf("invalid claim %+v", claim)
	}
	m.revision++
	m.entries[region] = memClaim{claim: claim, revision: m.revision}
	m.next = uint32(region) + 1
	return true, nil
}

func (m *memRegistry) Open(_ context.Context, next uint32) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return false, m.writeErr
	}
	if m.opened {
		return false, nil
	}
	for code := range m.entries {
		if uint32(code) >= next {
			return false, fmt.Errorf("%w: code %d already has an entry", ErrRegionClaimed, code)
		}
	}
	m.opened, m.next = true, next
	return true, nil
}

func (m *memRegistry) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memRegistry) get(region uint8) (Claim, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[region]
	return e.claim, ok
}

func (m *memRegistry) highWater(region uint8) uint64 {
	c, _ := m.get(region)
	return c.HighWater
}

func (m *memRegistry) setHighWater(region uint8, value uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[region]
	e.claim.HighWater = value
	m.revision++
	e.revision = m.revision
	m.entries[region] = e
}

// setDeployment 直接改一个码绑定的部署身份,模拟本字段出现之前写的记录。
func (m *memRegistry) setDeployment(region uint8, deployment string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[region]
	e.claim.Deployment = deployment
	m.revision++
	e.revision = m.revision
	m.entries[region] = e
}

func (m *memRegistry) drop(region uint8) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, region)
	m.revision++
}

// wipe 模拟登记处整个丢了数据:所有码的记录和「下一个可认领的码」都没了。
func (m *memRegistry) wipe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[uint8]memClaim{}
	m.opened, m.next = false, 0
	m.revision++
}

func (m *memRegistry) snapshot() map[uint8]Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uint8]Claim, len(m.entries))
	for code, e := range m.entries {
		out[code] = e.claim
	}
	return out
}

func (m *memRegistry) restore(snap map[uint8]Claim) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = map[uint8]memClaim{}
	for code, c := range snap {
		m.revision++
		m.entries[code] = memClaim{claim: c, revision: m.revision}
	}
}

func (m *memRegistry) setErrors(readErr, writeErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readErr, m.writeErr = readErr, writeErr
}

// 测试用的时刻:纪元后约 300 天。
var testNow = time.Unix(epochUnix+26_000_000, 0)

const testBlock = uint64(1) << 26 // 大于 MaxStep,比默认小,方便测试跨块;一块 = 4 秒的序号预算

// harness 是一个部署环境:一个 K8s 集群(deployment 是它的身份)、两套本地 etcd,加一个登记处(可以和别的 harness 共用)。
type harness struct {
	t          *testing.T
	primary    *memStore
	secondary  *memStore
	registry   *memRegistry
	deployment string
	now        time.Time
	mu         sync.Mutex
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, primary: &memStore{clusterID: 1}, secondary: &memStore{clusterID: 2}, registry: newMemRegistry(3),
		deployment: mintDeployment(), now: testNow}
}

// sibling 是另一个 K8s 集群里的部署环境:自己的两套本地 etcd(集群号由参数给),和 h 共用同一个登记处。
func (h *harness) sibling(primaryCluster, secondaryCluster uint64) *harness {
	return &harness{t: h.t, primary: &memStore{clusterID: primaryCluster}, secondary: &memStore{clusterID: secondaryCluster},
		registry: h.registry, deployment: mintDeployment(), now: h.clock()}
}

// elsewhere 是同一份清单部署到另一个 K8s 集群的环境。两套本地 etcd 是空的,集群号却和 h 完全相同:
// etcd 集群号只由成员 peer 地址和引导令牌算出,清单相同就相同。只有部署身份不同。和 h 共用同一个登记处。
func (h *harness) elsewhere() *harness {
	return h.sibling(h.primary.clusterID, h.secondary.clusterID)
}

// identity 是生产形态的 Options.Deployment:读本部署环境的身份。
func (h *harness) identity(context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deployment, nil
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) setClock(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = at
}

// issuer 是 dev 形态(AllowBootstrap=true)、地域 0 的发号器。
func (h *harness) issuer(mut func(*Options)) *Issuer {
	h.t.Helper()
	opt := Options{
		Primary:        h.primary,
		Secondary:      h.secondary,
		Registry:       h.registry,
		BlockSize:      testBlock,
		AllowBootstrap: true,
		Tags:           []string{"player", "item"},
		Now:            h.clock,
		NewAuthority:   mintAuthority,
	}
	if mut != nil {
		mut(&opt)
	}
	iss, err := New(opt)
	if err != nil {
		h.t.Fatal(err)
	}
	return iss
}

// prodIssuer 是生产形态(AllowBootstrap=false)的发号器。不做任何 IO。
func (h *harness) prodIssuer(region uint8, mut func(*Options)) *Issuer {
	h.t.Helper()
	return h.issuer(func(o *Options) {
		o.AllowBootstrap = false
		o.Region = region
		o.Deployment = h.identity
		if mut != nil {
			mut(o)
		}
	})
}

// production 按生产部署步骤准备好 region:登记处没开就开启(第一个可认领的码就是 region),再初始化。
func (h *harness) production(region uint8) *Issuer {
	h.t.Helper()
	iss := h.prodIssuer(region, nil)
	if _, err := iss.OpenRegistry(context.Background(), uint32(region)); err != nil {
		h.t.Fatalf("open registry: %v", err)
	}
	created, err := iss.Init(context.Background())
	if err != nil || !created {
		h.t.Fatalf("init region %d: created=%v err=%v", region, created, err)
	}
	return iss
}

// observer 收集 OnReserve。
type observer struct {
	mu   sync.Mutex
	seen map[string]int
}

func newObserver() *observer { return &observer{seen: map[string]int{}} }

func (o *observer) record(result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen[result]++
}

func (o *observer) count(result string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen[result]
}

// allocated 记下发过的所有段,断言互不重叠。
type allocated struct {
	t      *testing.T
	mu     sync.Mutex
	ranges [][2]uint64
}

func (a *allocated) add(lo, hi uint64) {
	a.t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if lo == 0 || hi <= lo {
		a.t.Fatalf("invalid range [%d, %d)", lo, hi)
	}
	for _, r := range a.ranges {
		if lo < r[1] && r[0] < hi {
			a.t.Fatalf("range [%d, %d) overlaps earlier [%d, %d)", lo, hi, r[0], r[1])
		}
	}
	a.ranges = append(a.ranges, [2]uint64{lo, hi})
}

func (a *allocated) take(iss *Issuer, step uint32) (uint64, uint64) {
	a.t.Helper()
	lo, hi, err := iss.Allocate(context.Background(), "player", step)
	if err != nil {
		a.t.Fatal(err)
	}
	a.add(lo, hi)
	return lo, hi
}

// exhaust 让发号器手里的块全部用完,下一次 Allocate 必须预留新块。
func exhaust(iss *Issuer) {
	iss.mu.Lock()
	defer iss.mu.Unlock()
	iss.cur.pos = iss.cur.end
	iss.next = nil
}

// ── 独立性 ──────────────────────────────────────────────────────────────

// 两份本地记录在同一套 etcd 上:生产拒绝预留,也拒绝初始化,什么都不写。比的是 etcd 报的集群号,换一种地址写法绕不过去。
func TestSameClusterIsRefusedInProduction(t *testing.T) {
	h := newHarness(t)
	h.primary.clusterID, h.secondary.clusterID = 7, 7
	iss := h.prodIssuer(1, nil)
	defer iss.Close()
	if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 1); !errors.Is(err, ErrSameCluster) {
		t.Fatalf("allocate: want ErrSameCluster, got %v", err)
	}
	if created, err := iss.Init(context.Background()); created || !errors.Is(err, ErrSameCluster) {
		t.Fatalf("init: want ErrSameCluster, got created=%v err=%v", created, err)
	}
	if h.primary.has() || h.secondary.has() {
		t.Fatal("nothing may be written when the two records share a cluster")
	}
	if _, claimed := h.registry.get(1); claimed {
		t.Fatal("the code must not be claimed")
	}
}

// 登记处和某一份本地记录在同一套 etcd 上:一次整套回档会同时打穿两份,生产同样拒绝。
func TestRegistrySharingALocalClusterIsRefused(t *testing.T) {
	for _, shared := range []uint64{1, 2} {
		h := newHarness(t)
		h.registry.clusterID = shared
		iss := h.prodIssuer(1, nil)
		if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := iss.Init(context.Background()); !errors.Is(err, ErrSameCluster) {
			t.Fatalf("registry on cluster %d: want ErrSameCluster, got %v", shared, err)
		}
		if _, _, err := iss.Allocate(context.Background(), "player", 1); !errors.Is(err, ErrSameCluster) {
			t.Fatalf("registry on cluster %d: want ErrSameCluster, got %v", shared, err)
		}
		iss.Close()
	}
}

// 三套不同的 etcd 正常发号;dev 允许三份记录在同一套上。
func TestClusterCheckAllowsIndependentClustersAndDev(t *testing.T) {
	independent := newHarness(t)
	production := independent.production(1)
	defer production.Close()
	if _, _, err := production.Allocate(context.Background(), "player", 1); err != nil {
		t.Fatalf("independent clusters: %v", err)
	}

	shared := newHarness(t)
	shared.primary.clusterID, shared.secondary.clusterID, shared.registry.clusterID = 7, 7, 7
	dev := shared.issuer(nil)
	defer dev.Close()
	if _, _, err := dev.Allocate(context.Background(), "player", 1); err != nil {
		t.Fatalf("dev may share a cluster: %v", err)
	}
}

// ── 切段 ────────────────────────────────────────────────────────────────

func TestAllocate_ContiguousWithinBlock(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	lo1, hi1, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if hi1-lo1 != 100 {
		t.Fatalf("segment length = %d, want 100", hi1-lo1)
	}
	lo2, _, err := iss.Allocate(context.Background(), "item", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo2 != hi1 {
		t.Fatalf("different tags must share one counter: lo2=%d hi1=%d", lo2, hi1)
	}
}

func TestAllocate_TruncatesAtBlockEnd(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 1)
	if err != nil {
		t.Fatal(err)
	}
	// 把当前块消耗到只剩 10 个号:发号器可以截短,调用方不要求 hi-lo==step。
	iss.mu.Lock()
	iss.cur.pos = iss.cur.end - 10
	iss.next = &block{pos: iss.cur.end + 1000, end: iss.cur.end + 2000, halfway: iss.cur.end + 1500}
	end := iss.cur.end
	iss.mu.Unlock()
	lo2, hi2, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if hi2 != end || hi2-lo2 != 10 {
		t.Fatalf("segment [%d, %d), want truncated at block end %d (first lo=%d)", lo2, hi2, end, lo)
	}
}

func TestAllocate_RejectsBadArguments(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	cases := []struct {
		tag  string
		step uint32
		want error
	}{
		{"Bad Tag", 1, ErrInvalidTag},
		{"", 1, ErrInvalidTag},
		{"unknown", 1, ErrUnknownTag},
		{"player", MaxStep + 1, ErrInvalidStep},
	}
	for _, tc := range cases {
		if _, _, err := iss.Allocate(context.Background(), tc.tag, tc.step); !errors.Is(err, tc.want) {
			t.Errorf("Allocate(%q,%d) err=%v, want %v", tc.tag, tc.step, err, tc.want)
		}
	}
}

// ── 预留 ────────────────────────────────────────────────────────────────

// 一块只有三份记录都写成功才能发。
func TestReserve_AllRecordsWrittenBeforeIssuing(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h.primary.get() < hi || h.secondary.get() < hi || h.registry.highWater(1) < hi {
		t.Fatalf("records (%d,%d,%d) must cover the issued range end %d", h.primary.get(), h.secondary.get(), h.registry.highWater(1), hi)
	}
	if lo < uint64(1)<<regionShift|uint64(26_000_000)<<seqBits {
		t.Fatalf("lo=%d below the time floor", lo)
	}
	if c, _ := h.registry.get(1); c.Blocks != 1 {
		t.Fatalf("blocks=%d, want 1", c.Blocks)
	}
}

// 主记录写成功、副记录写失败:这块作废,登记处不动;之后的块从主记录之上开始。
func TestReserve_PrimaryWrittenSecondaryFailedDiscardsBlock(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	registryBefore := h.registry.highWater(1)

	h.secondary.setErrors(nil, errors.New("secondary down"))
	failing := h.prodIssuer(1, nil)
	if _, _, err := failing.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("must not issue when the secondary record cannot be written")
	}
	if h.primary.get() <= registryBefore {
		t.Fatal("the primary record is expected to be written first")
	}
	if h.registry.highWater(1) != registryBefore {
		t.Fatal("the registry must not move when the secondary write failed")
	}
	failing.mu.Lock()
	remaining := failing.cur.remaining()
	failing.mu.Unlock()
	if remaining != 0 {
		t.Fatal("the block must be discarded")
	}

	h.secondary.setErrors(nil, nil)
	primaryAfterFailure := h.primary.get()
	lo, _ := a.take(h.prodIssuer(1, nil), 100)
	if lo < primaryAfterFailure {
		t.Fatalf("lo=%d must start above the primary record %d left by the failed reservation", lo, primaryAfterFailure)
	}
}

// 登记处写失败:本地两份已经推高,这块照样作废;下一块从本地记录之上开始,登记处跟上。
func TestReserve_RegistryWriteFailureDiscardsBlock(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	registryBefore := h.registry.highWater(1)

	h.registry.setErrors(nil, errors.New("registry unreachable"))
	failing := h.prodIssuer(1, nil)
	if _, _, err := failing.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("must not issue when the registry cannot be written")
	}
	if h.registry.highWater(1) != registryBefore || h.primary.get() <= registryBefore {
		t.Fatalf("local records must be ahead (%d) of the unchanged registry (%d)", h.primary.get(), h.registry.highWater(1))
	}

	h.registry.setErrors(nil, nil)
	obs := newObserver()
	localAfterFailure := h.primary.get()
	lo, hi := a.take(h.prodIssuer(1, func(o *Options) { o.OnReserve = obs.record }), 100)
	if lo < localAfterFailure {
		t.Fatalf("lo=%d must start above the local records %d", lo, localAfterFailure)
	}
	if h.registry.highWater(1) < hi {
		t.Fatal("the registry must catch up")
	}
	if obs.count(ReserveRegistryBehind) != 1 {
		t.Fatalf("observed %v", obs.seen)
	}
}

// 第二份记录冲突时不能认领,必须作废重来。
func TestReserve_SecondaryConflictRestarts(t *testing.T) {
	h := newHarness(t)
	h.secondary.set(uint64(26_000_000) << seqBits)
	h.primary.set(uint64(26_000_000) << seqBits)
	h.secondary.beforeWrite = func() {
		// 另一个副本抢先写了更高的值。
		h.secondary.set(uint64(26_000_001) << seqBits)
		h.primary.set(uint64(26_000_001) << seqBits)
	}
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo < uint64(26_000_001)<<seqBits {
		t.Fatalf("lo=%d must restart above the other replica's record", lo)
	}
}

// 登记处冲突(另一个副本先提交了一块)同样作废重来。
func TestReserve_RegistryConflictRestarts(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	other := uint64(1)<<regionShift | uint64(26_000_100)<<seqBits
	h.registry.beforeWrite = func() { h.registry.setHighWater(1, other) }
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo < other {
		t.Fatalf("lo=%d must restart above the block another replica committed (%d)", lo, other)
	}
}

func TestReserve_PersistentConflictGivesUp(t *testing.T) {
	h := newHarness(t)
	var keepConflicting func()
	keepConflicting = func() {
		h.primary.set(h.primary.get() + 1)
		h.primary.mu.Lock()
		h.primary.beforeWrite = keepConflicting
		h.primary.mu.Unlock()
	}
	h.primary.beforeWrite = keepConflicting
	iss := h.issuer(nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrContended) {
		t.Fatalf("err=%v, want ErrContended", err)
	}
}

// ── 恢复:下面的用例对应设计 §6.2、§6.3 的故障表,全部断言发出去的段不重叠 ─────────────

func TestRecovery_RestartReservesAboveOldBlock(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.production(1)
	a.take(first, 100)
	// 进程崩溃:不调 Close,手里的块作废。新进程只能拿到更高的块。
	second := h.prodIssuer(1, nil)
	a.take(second, 100)
	a.take(first, 100) // 崩溃前的进程假如还活着,手里的块仍然独占
}

// 只回退一套:主记录被旧备份覆盖,副记录和登记处还在。时钟也倒回去,确保靠的是记录。
func TestRecovery_PrimaryRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	before := h.primary.get()
	h.primary.set(before - testBlock*10)
	h.setClock(testNow.Add(-time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
	if h.primary.get() <= before {
		t.Fatalf("primary=%d must be restored above %d", h.primary.get(), before)
	}
}

func TestRecovery_SecondaryRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.secondary.set(h.secondary.get() - testBlock*10)
	h.setClock(testNow.Add(-time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
}

func TestRecovery_PrimaryWiped(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.primary.drop()
	h.setClock(testNow.Add(-time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
	if !h.primary.has() {
		t.Fatal("the wiped record must be restored")
	}
}

func TestRecovery_SecondaryWiped(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.secondary.drop()
	h.setClock(testNow.Add(-time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
	if !h.secondary.has() {
		t.Fatal("the wiped record must be restored")
	}
}

// 评审的反例(问题 1):先领段,再把两套本地 etcd 恢复到两条记录都还在的旧快照,重启发号器,时钟完全正常。
// 计数器因为多次重启比时钟超前,只看两份记录和时间下限的旧设计会从旧快照的水位接着发,和快照之后预留的块重叠。
// 现在登记处盖住了全部历史预留,新块必须在它之上。
func TestRecovery_BothLocalRestoredToOldSnapshotWithCorrectClock(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	snapP, snapS := h.primary.snapshot(), h.secondary.snapshot()
	if !snapP.exists || !snapS.exists {
		t.Fatal("the snapshot must contain both records")
	}
	// 快照之后又重启了几次,每次作废一块:计数器跑到了时钟前面。
	for restart := 0; restart < 5; restart++ {
		a.take(h.prodIssuer(1, nil), 100)
	}
	historical := h.registry.highWater(1)
	// 所有预留过的块都在 historical 之下:每块只有登记处写成功才发。
	floor := uint64(1)<<regionShift | uint64(26_000_000)<<seqBits
	if oldDesignStart := max(snapP.value, snapS.value, floor); oldDesignStart >= historical {
		t.Fatalf("the scenario must reproduce the counterexample: the old design would restart at %d, history ends at %d", oldDesignStart, historical)
	}

	h.primary.restore(snapP)
	h.secondary.restore(snapS)
	obs := newObserver()
	restarted := h.prodIssuer(1, func(o *Options) { o.OnReserve = obs.record })
	lo, hi := a.take(restarted, 100)
	if lo < historical {
		t.Fatalf("lo=%d overlaps the reservations made after the snapshot (they end at %d)", lo, historical)
	}
	if obs.count(ReserveLocalBehind) != 1 {
		t.Fatalf("restoring both local records must be reported, observed %v", obs.seen)
	}
	if h.primary.get() < hi || h.secondary.get() < hi {
		t.Fatal("the restored local records must be raised")
	}
}

// 两份本地记录都没了(两套 etcd 同时丢数据):生产拒绝发号,不写任何东西;按登记处水位恢复,要显式 rebind。
func TestRecovery_BothLocalMissingRefusedUntilRecovered(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	historical := h.registry.highWater(1)
	h.primary.drop()
	h.secondary.drop()

	iss := h.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("err=%v, want ErrCounterMissing", err)
	}
	if created, err := iss.Init(context.Background()); created || !errors.Is(err, ErrCounterMissing) {
		t.Fatalf("init must not treat a lost region as new: created=%v err=%v", created, err)
	}
	if _, err := iss.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover without rebind: err=%v, want ErrRebindRequired", err)
	}
	if h.primary.has() || h.secondary.has() || h.registry.highWater(1) != historical {
		t.Fatal("refusals must not write anything")
	}
	if _, err := iss.Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if h.primary.get() != historical || h.secondary.get() != historical {
		t.Fatalf("records must be rebuilt at the registry high water %d, got (%d,%d)", historical, h.primary.get(), h.secondary.get())
	}
	// 新进程:iss 刚才的预留失败过,还在退避期内。
	lo, _ := a.take(h.prodIssuer(1, nil), 100)
	if lo < historical {
		t.Fatalf("lo=%d below the registry high water %d", lo, historical)
	}
}

// 登记处被旧快照覆盖(本地两份完好):按本地记录继续,登记处跟上。
func TestRecovery_RegistryRestoredToOldSnapshot(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	snap := h.registry.snapshot()
	for restart := 0; restart < 3; restart++ {
		a.take(h.prodIssuer(1, nil), 100)
	}
	local := h.primary.get()
	h.registry.restore(snap)

	obs := newObserver()
	lo, hi := a.take(h.prodIssuer(1, func(o *Options) { o.OnReserve = obs.record }), 100)
	if lo < local {
		t.Fatalf("lo=%d below the local records %d", lo, local)
	}
	if obs.count(ReserveRegistryBehind) != 1 || h.registry.highWater(1) < hi {
		t.Fatalf("the registry must be reported and raised: observed %v high_water=%d", obs.seen, h.registry.highWater(1))
	}
}

// 登记处丢了这个码的记录(本地两份完好):预留和初始化都拒绝;Recover 按本地记录重新登记。
func TestRecovery_RegistryEntryLostReRegisteredFromLocal(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	local := h.primary.get()
	h.registry.drop(1)

	iss := h.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegistryMissing) {
		t.Fatalf("allocate: err=%v, want ErrRegistryMissing", err)
	}
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegistryMissing) {
		t.Fatalf("init: err=%v, want ErrRegistryMissing", err)
	}
	if _, err := iss.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	c, ok := h.registry.get(1)
	if !ok || c.HighWater != local || c.Blocks == 0 || c.PrimaryCluster != 1 || c.SecondaryCluster != 2 {
		t.Fatalf("re-registered claim %+v (exists=%v), want high_water=%d bound to (1,2)", c, ok, local)
	}
	lo, _ := a.take(h.prodIssuer(1, nil), 100)
	if lo < local {
		t.Fatalf("lo=%d below the local records %d", lo, local)
	}
}

// 号源和可信恢复证据同时丢失:本地两份和登记处这个码的记录都没了。预留、恢复、重新初始化全部拒绝,不猜起点。
func TestRecovery_EverythingLostIsRefused(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.primary.drop()
	h.secondary.drop()
	h.registry.drop(1)

	iss := h.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegistryMissing) {
		t.Fatalf("allocate: err=%v, want ErrRegistryMissing", err)
	}
	for _, rebind := range []bool{false, true} {
		if _, err := iss.Recover(context.Background(), rebind); !errors.Is(err, ErrNoTrustedBound) {
			t.Fatalf("recover(rebind=%v): err=%v, want ErrNoTrustedBound", rebind, err)
		}
	}
	// 这个码小于登记处里下一个可认领的码:它被认领过,不能当成新码重来。
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: err=%v, want ErrRegionClaimed", err)
	}
	if h.primary.has() || h.secondary.has() {
		t.Fatal("refusals must not write anything")
	}
	if _, claimed := h.registry.get(1); claimed {
		t.Fatal("refusals must not write anything")
	}
}

// 登记处整个丢了(所有码和「下一个可认领的码」):两个地域都拒绝;各自按本地记录重新登记;开启时下一个可认领的码
// 必须在已登记的码之上;用过的码不能重新初始化;之后两个地域的号都不和历史重叠。
func TestRecovery_WholeRegistryLostRebuiltFromRegions(t *testing.T) {
	east := newHarness(t)
	west := east.sibling(11, 12)
	a := &allocated{t: t}
	a.take(east.production(1), 100)
	a.take(west.production(2), 100)
	east.registry.wipe()

	eastIss, westIss := east.prodIssuer(1, nil), west.prodIssuer(2, nil)
	for _, iss := range []*Issuer{eastIss, westIss} {
		if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegistryMissing) {
			t.Fatalf("allocate: err=%v, want ErrRegistryMissing", err)
		}
	}
	fresh := east.sibling(21, 22)
	if _, err := fresh.prodIssuer(3, nil).Init(context.Background()); !errors.Is(err, ErrRegistryNotOpened) {
		t.Fatalf("init on a wiped registry: err=%v, want ErrRegistryNotOpened", err)
	}
	for _, iss := range []*Issuer{eastIss, westIss} {
		if _, err := iss.Recover(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := eastIss.OpenRegistry(context.Background(), 2); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("open below a registered code: err=%v, want ErrRegionClaimed", err)
	}
	if _, err := eastIss.OpenRegistry(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := east.sibling(31, 32).prodIssuer(1, nil).Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("a used code must never be initialized again: err=%v", err)
	}
	a.take(east.prodIssuer(1, nil), 100)
	a.take(west.prodIssuer(2, nil), 100)
	created, err := fresh.prodIssuer(3, nil).Init(context.Background())
	if err != nil || !created {
		t.Fatalf("a new code after the rebuild: created=%v err=%v", created, err)
	}
}

// 评审给出的反例:时钟跑快把计数器抬到未来,之后一份记录回退、时钟恢复,再等真实时间追上来。
func TestRecovery_FastClockThenRollback(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	h.production(1)
	// 时钟快 1 天,发出未来的号。
	h.setClock(testNow.Add(24 * time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
	// 时钟恢复,主记录回退到时钟跑快之前。
	h.setClock(testNow)
	h.primary.set(uint64(1)<<regionShift | uint64(26_000_000)<<seqBits)
	second := h.prodIssuer(1, nil)
	a.take(second, 100)
	// 真实时间走到当时那一刻,再领一块。
	h.setClock(testNow.Add(24 * time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
	a.take(second, 100)
}

// 恢复不用先停旧发号进程:它们手里的块都在登记处水位之下,下一次预留也只能落在水位之上。
func TestRecovery_OldProcessesNeedNoIsolation(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	first := h.production(1)
	a.take(first, 100)
	snapP, snapS := h.primary.snapshot(), h.secondary.snapshot()
	second := h.prodIssuer(1, nil)
	a.take(second, 100)

	h.primary.restore(snapP)
	h.secondary.restore(snapS)
	third := h.prodIssuer(1, nil)
	a.take(third, 100)
	// 旧进程接着发手里的块。
	a.take(first, 100)
	a.take(second, 100)
	// 旧进程预留下一块。
	exhaust(first)
	a.take(first, 100)
}

// ── 初始化 ──────────────────────────────────────────────────────────────

// 登记处没开启(新世界还没走开启步骤,或者登记处整个丢了):初始化拒绝,不写。
func TestInit_RequiresOpenedRegistry(t *testing.T) {
	h := newHarness(t)
	iss := h.prodIssuer(1, nil)
	if created, err := iss.Init(context.Background()); created || !errors.Is(err, ErrRegistryNotOpened) {
		t.Fatalf("created=%v err=%v, want ErrRegistryNotOpened", created, err)
	}
	if h.primary.has() || h.secondary.has() {
		t.Fatal("refusal must not write anything")
	}
}

// 初始化认领一个码、建两份本地记录;再跑一次什么也不做;下一个可认领的码推过去。
func TestInit_ClaimsTheCodeOnceAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	c, ok := h.registry.get(1)
	if !ok || c.State != ClaimActive || c.Authority == "" || c.PrimaryCluster != 1 || c.SecondaryCluster != 2 || c.Blocks != 0 {
		t.Fatalf("claim %+v", c)
	}
	if h.primary.snapshot().authority != c.Authority || h.secondary.snapshot().authority != c.Authority {
		t.Fatal("local records must carry the claimed authority")
	}
	if created, err := iss.Init(context.Background()); created || err != nil {
		t.Fatalf("second init: created=%v err=%v", created, err)
	}
	if h.registry.next != 2 {
		t.Fatalf("next claimable code = %d, want 2", h.registry.next)
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err != nil {
		t.Fatal(err)
	}
}

// 认领之后、建本地记录之前中断:这个码还没发过号,再跑一次 init(或 recover)接着做完。
func TestInit_ResumesAfterAnInterruptedClaim(t *testing.T) {
	for _, viaRecover := range []bool{false, true} {
		h := newHarness(t)
		iss := h.prodIssuer(1, nil)
		if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		h.primary.setErrors(nil, errors.New("primary down"))
		if _, err := iss.Init(context.Background()); err == nil {
			t.Fatal("init must fail while the primary record cannot be written")
		}
		if _, claimed := h.registry.get(1); !claimed {
			t.Fatal("the claim is expected to be written first")
		}
		h.primary.setErrors(nil, nil)
		if viaRecover {
			if _, err := iss.Recover(context.Background(), false); err != nil {
				t.Fatal(err)
			}
		} else if created, err := iss.Init(context.Background()); err != nil || !created {
			t.Fatalf("resume: created=%v err=%v", created, err)
		}
		if !h.primary.has() || !h.secondary.has() {
			t.Fatal("both local records must exist after the resume")
		}
		if _, _, err := iss.Allocate(context.Background(), "player", 100); err != nil {
			t.Fatal(err)
		}
	}
}

// 码小于下一个可认领的码:之前被认领过(哪怕记录已经不在),初始化拒绝。
func TestInit_RefusesACodeBelowTheNextClaimableOne(t *testing.T) {
	h := newHarness(t)
	iss := h.prodIssuer(3, nil)
	if _, err := iss.OpenRegistry(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("err=%v, want ErrRegionClaimed", err)
	}
}

// 码大于下一个可认领的码(比如 5 写成 50):拒绝,什么都不写,下一个可认领的码不动,中间的码一个都没烧掉。
func TestInit_RefusesACodeAboveTheNextClaimableOne(t *testing.T) {
	h := newHarness(t)
	iss := h.prodIssuer(5, nil)
	if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if created, err := iss.Init(context.Background()); created || !errors.Is(err, ErrRegionOutOfOrder) {
		t.Fatalf("init: created=%v err=%v, want ErrRegionOutOfOrder", created, err)
	}
	if _, claimed := h.registry.get(5); claimed || h.primary.has() || h.secondary.has() {
		t.Fatal("a refused claim must not write anything")
	}
	if created, err := h.prodIssuer(1, nil).Init(context.Background()); err != nil || !created {
		t.Fatalf("the next claimable code 1 must still be claimable: created=%v err=%v", created, err)
	}
}

// ── 地域码绑定(问题 2)────────────────────────────────────────────────────

// 另一个 K8s 集群里的部署(部署身份不同,本地 etcd 的集群号也不同)配了同一个地域码:最多一套取得发号资格。
// 另一套初始化、发号、恢复都拒绝,不写任何东西。集群号相同的默认部署形态见 AnotherKubernetesClusterWithIdenticalEtcd。
func TestBinding_SecondDeploymentWithTheSameCodeIsRefused(t *testing.T) {
	first := newHarness(t)
	second := first.sibling(11, 12)
	a := &allocated{t: t}
	a.take(first.production(1), 100)
	before, _ := first.registry.get(1)

	dup := second.prodIssuer(1, nil)
	if created, err := dup.Init(context.Background()); created || !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: created=%v err=%v, want ErrRegionClaimed", created, err)
	}
	if _, _, err := dup.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := dup.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if second.primary.has() || second.secondary.has() {
		t.Fatal("the duplicate deployment must not write local records")
	}
	if after, _ := first.registry.get(1); after != before {
		t.Fatalf("the duplicate deployment changed the claim: %+v -> %+v", before, after)
	}
	a.take(first.prodIssuer(1, nil), 100)
}

// 克隆旧配置和快照:新环境从原环境的 etcd 快照恢复出两份记录(世系相同,集群号和部署身份都不同)。拿不到第二份资格。
// 数据目录原样恢复、集群号也相同的情形见 BackupRestoredIntoAnotherKubernetesCluster。
func TestBinding_ClonedConfigAndSnapshotIsRefused(t *testing.T) {
	origin := newHarness(t)
	a := &allocated{t: t}
	a.take(origin.production(1), 100)
	clone := origin.sibling(0, 0)
	clone.primary = origin.primary.snapshot().clone(11)
	clone.secondary = origin.secondary.snapshot().clone(12)
	before, _ := origin.registry.get(1)

	iss := clone.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: err=%v, want ErrRegionClaimed", err)
	}
	if _, err := iss.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if after, _ := origin.registry.get(1); after != before {
		t.Fatal("the clone must not change the claim")
	}
	a.take(origin.prodIssuer(1, nil), 100)
}

// 同一份清单部署到另一个 K8s 集群:本地 etcd 的集群号和原环境完全相同,只有部署身份不同(审核交接 §6.2 的默认部署形态)。
// 码发过号之后,另一个集群初始化报「已被认领」,而不是「本地两份记录都丢了」;发号和不带 rebind 的恢复都拒绝,什么都不写。
func TestBinding_AnotherKubernetesClusterWithIdenticalEtcdIsRefused(t *testing.T) {
	first := newHarness(t)
	second := first.elsewhere()
	if second.primary.clusterID != first.primary.clusterID || second.secondary.clusterID != first.secondary.clusterID {
		t.Fatal("fixture: the other cluster must report the same etcd cluster ids")
	}
	a := &allocated{t: t}
	a.take(first.production(1), 100)
	before, _ := first.registry.get(1)

	dup := second.prodIssuer(1, nil)
	if created, err := dup.Init(context.Background()); created || !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: created=%v err=%v, want ErrRegionClaimed", created, err)
	}
	if _, _, err := dup.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := dup.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if second.primary.has() || second.secondary.has() {
		t.Fatal("the other cluster must not write local records")
	}
	if after, _ := first.registry.get(1); after != before {
		t.Fatalf("the other cluster changed the claim: %+v -> %+v", before, after)
	}
	a.take(first.prodIssuer(1, nil), 100)
}

// 码认领了但还没发过号(Blocks=0):另一个 K8s 集群不能借「接着做完没做完的初始化」拿到资格。
func TestBinding_AnotherKubernetesClusterCannotFinishAnUnusedClaim(t *testing.T) {
	first := newHarness(t)
	first.production(1)
	before, _ := first.registry.get(1)
	if before.Blocks != 0 {
		t.Fatalf("fixture: the code must not have issued yet, blocks=%d", before.Blocks)
	}
	second := first.elsewhere()
	dup := second.prodIssuer(1, nil)
	if created, err := dup.Init(context.Background()); created || !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: created=%v err=%v, want ErrRegionClaimed", created, err)
	}
	if _, err := dup.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if second.primary.has() || second.secondary.has() {
		t.Fatal("the other cluster must not write local records")
	}
	if after, _ := first.registry.get(1); after != before {
		t.Fatalf("the other cluster changed the claim: %+v -> %+v", before, after)
	}
}

// 集群级备份恢复到另一个 K8s 集群:etcd 数据目录原样恢复,世系和集群号都和原环境相同,只有部署身份不同。拿不到第二份资格。
func TestBinding_BackupRestoredIntoAnotherKubernetesClusterIsRefused(t *testing.T) {
	origin := newHarness(t)
	a := &allocated{t: t}
	a.take(origin.production(1), 100)
	restored := origin.elsewhere()
	restored.primary = origin.primary.snapshot().clone(origin.primary.clusterID)
	restored.secondary = origin.secondary.snapshot().clone(origin.secondary.clusterID)
	before, _ := origin.registry.get(1)

	iss := restored.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: err=%v, want ErrRegionClaimed", err)
	}
	if _, err := iss.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if after, _ := origin.registry.get(1); after != before {
		t.Fatal("the restored copy must not change the claim")
	}
	a.take(origin.prodIssuer(1, nil), 100)
}

// 灾备:原 K8s 集群还在跑,新集群用同一份清单部署(集群号相同)。rebind 之后资格转到新集群,原集群发完手里的块,
// 再预留就被拒;新集群从全部历史预留之上开始,两边发过的段互不重叠。
func TestBinding_RebindToAnotherKubernetesClusterFencesTheOldOne(t *testing.T) {
	old := newHarness(t)
	a := &allocated{t: t}
	running := old.production(1)
	a.take(running, 100)
	historical := old.registry.highWater(1)

	standby := old.elsewhere()
	if _, err := standby.prodIssuer(1, nil).Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if c, _ := old.registry.get(1); c.Deployment != standby.deployment {
		t.Fatalf("binding not moved to the standby cluster: %+v", c)
	}
	a.take(running, 100)
	exhaust(running)
	if _, _, err := running.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("old cluster after the rebind: err=%v, want ErrBindingMismatch", err)
	}
	lo, _ := a.take(standby.prodIssuer(1, nil), 100)
	if lo < historical {
		t.Fatalf("lo=%d below the high water %d", lo, historical)
	}
}

// 没有部署身份的码(本字段出现之前写的记录):生产预留拒绝,不带 rebind 的恢复也拒绝;rebind 之后补上身份,照常发号,不低于原水位。
func TestBinding_ClaimWithoutDeploymentNeedsRebind(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.registry.setDeployment(1, "")
	historical := h.registry.highWater(1)

	iss := h.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := iss.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if _, err := iss.Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.registry.get(1); c.Deployment != h.deployment {
		t.Fatalf("rebind must record the deployment: %+v", c)
	}
	lo, _ := a.take(h.prodIssuer(1, nil), 100)
	if lo < historical {
		t.Fatalf("lo=%d below the high water %d", lo, historical)
	}
}

// 同一对 etcd 的多个副本是合法的,并发发号互不重叠。
func TestBinding_ReplicasOfOneRegionKeepIssuing(t *testing.T) {
	h := newHarness(t)
	h.production(1)
	segs := concurrentTake(t, []*Issuer{h.prodIssuer(1, nil), h.prodIssuer(1, nil), h.prodIssuer(1, nil), h.prodIssuer(1, nil)}, 300)
	a := &allocated{t: t}
	for _, s := range segs {
		a.add(s[0], s[1])
	}
	if len(a.ranges) == 0 {
		t.Fatal("no segment was issued")
	}
}

// 不同的合法地域码并发发号:同类型 id 无重复。
func TestBinding_DifferentCodesIssueConcurrentlyWithoutOverlap(t *testing.T) {
	east := newHarness(t)
	west := east.sibling(11, 12)
	east.production(1)
	west.production(2)
	issuers := []*Issuer{east.prodIssuer(1, nil), east.prodIssuer(1, nil), west.prodIssuer(2, nil), west.prodIssuer(2, nil)}
	segs := concurrentTake(t, issuers, 300)
	a := &allocated{t: t}
	regions := map[uint64]int{}
	for _, s := range segs {
		a.add(s[0], s[1])
		regions[s[0]>>regionShift]++
	}
	if regions[1] == 0 || regions[2] == 0 || len(regions) != 2 {
		t.Fatalf("segments by region: %v", regions)
	}
}

// 退役的码永不再用:原环境发完手里的块就停,任何环境都不能重新初始化或恢复它。
func TestBinding_RetiredCodeCannotBeReused(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	running := h.production(1)
	a.take(running, 100)
	retired, err := running.Retire(context.Background(), false)
	if err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	if again, err := running.Retire(context.Background(), false); again || err != nil {
		t.Fatalf("second retire: %v %v", again, err)
	}
	a.take(running, 100) // 手里的块照样发完
	exhaust(running)
	if _, _, err := running.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegionRetired) {
		t.Fatalf("allocate after retire: err=%v", err)
	}
	other := h.sibling(11, 12).prodIssuer(1, nil)
	if _, err := other.Init(context.Background()); !errors.Is(err, ErrRegionRetired) {
		t.Fatalf("init a retired code: err=%v", err)
	}
	if _, err := running.Recover(context.Background(), true); !errors.Is(err, ErrRegionRetired) {
		t.Fatalf("recover a retired code: err=%v", err)
	}
}

// 登记处里没有的码也能写墓碑(重建登记处时按台账补退役码),之后不能被认领。
func TestRetire_WritesATombstoneForAnUnknownCode(t *testing.T) {
	h := newHarness(t)
	iss := h.prodIssuer(4, nil)
	if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if retired, err := iss.Retire(context.Background(), false); err != nil || !retired {
		t.Fatalf("retire: %v %v", retired, err)
	}
	if _, err := iss.Init(context.Background()); !errors.Is(err, ErrRegionRetired) {
		t.Fatalf("init: err=%v, want ErrRegionRetired", err)
	}
}

// 在用的码只能从绑定它的部署环境退役(审核交接 §6.3)。另一个 K8s 集群(集群号相同也一样)、或者复制了别的地域配置、
// 只把地域码改掉的环境,都拒绝,登记处不变,原地域照常发号。
func TestRetire_ACodeInUseIsRefusedFromAnUnboundDeployment(t *testing.T) {
	east := newHarness(t)
	a := &allocated{t: t}
	a.take(east.production(1), 100)
	west := east.sibling(11, 12)
	a.take(west.production(2), 100)
	before, _ := east.registry.get(1)

	for name, iss := range map[string]*Issuer{
		"another kubernetes cluster with identical etcd": east.elsewhere().prodIssuer(1, nil),
		"another region's config with the code changed":  west.prodIssuer(1, nil),
	} {
		if retired, err := iss.Retire(context.Background(), false); retired || !errors.Is(err, ErrBindingMismatch) {
			t.Fatalf("%s: retired=%v err=%v, want ErrBindingMismatch", name, retired, err)
		}
	}
	if after, _ := east.registry.get(1); after != before {
		t.Fatalf("a refused retire changed the claim: %+v -> %+v", before, after)
	}
	a.take(east.prodIssuer(1, nil), 100)
}

// 绑定它的地域已经不在:运维确认之后用 unbound 放行,码照样永久退役;原环境(假如还在)发完手里的块就停。
func TestRetire_UnboundNeedsTheExplicitSwitch(t *testing.T) {
	h := newHarness(t)
	running := h.production(1)
	a := &allocated{t: t}
	a.take(running, 100)
	elsewhere := h.elsewhere().prodIssuer(1, nil)
	if retired, err := elsewhere.Retire(context.Background(), true); err != nil || !retired {
		t.Fatalf("retire with the explicit switch: %v %v", retired, err)
	}
	if c, _ := h.registry.get(1); c.State != ClaimRetired {
		t.Fatalf("the code must be retired: %+v", c)
	}
	exhaust(running)
	if _, _, err := running.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegionRetired) {
		t.Fatalf("allocate after retire: err=%v, want ErrRegionRetired", err)
	}
}

// etcd 换成了新集群(数据从备份恢复进新集群,集群号变了):预留拒绝;显式 rebind 之后在原水位之上继续。
func TestBinding_RebindAfterReplacingTheEtcdClusters(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	historical := h.registry.highWater(1)
	h.primary.clusterID, h.secondary.clusterID = 21, 22

	iss := h.prodIssuer(1, nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: err=%v, want ErrBindingMismatch", err)
	}
	if _, err := iss.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: err=%v, want ErrRebindRequired", err)
	}
	if _, err := iss.Recover(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.registry.get(1); c.PrimaryCluster != 21 || c.SecondaryCluster != 22 {
		t.Fatalf("binding not moved: %+v", c)
	}
	lo, _ := a.take(h.prodIssuer(1, nil), 100)
	if lo < historical {
		t.Fatalf("lo=%d below the high water %d", lo, historical)
	}
}

// 本地记录属于另一条世系:dev 也拒绝,不会把两条世系混在一个计数器上。
func TestBinding_ForeignAuthorityIsRefusedEvenInDev(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	if _, _, err := iss.Allocate(context.Background(), "player", 1); err != nil {
		t.Fatal(err)
	}
	h.primary.mu.Lock()
	h.primary.authority = "ffffffffffffffffffffffffffffffff"
	h.primary.revision++
	h.primary.mu.Unlock()
	exhaust(iss)
	if _, _, err := iss.Allocate(context.Background(), "player", 1); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("err=%v, want ErrBindingMismatch", err)
	}
}

// Recover 在记录完好时什么也不做。
func TestRecover_NothingToRepair(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	before, _ := h.registry.get(1)
	if _, err := iss.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if after, _ := h.registry.get(1); after != before {
		t.Fatal("recover must not touch consistent records")
	}
}

// ── 时钟 ────────────────────────────────────────────────────────────────

// 时钟回拨或跑慢:计数器只增不减,不会重号。
func TestClock_BackwardIsHarmless(t *testing.T) {
	h := newHarness(t)
	a := &allocated{t: t}
	a.take(h.production(1), 100)
	h.setClock(testNow.Add(-30 * 24 * time.Hour))
	a.take(h.prodIssuer(1, nil), 100)
}

// 时钟早于纪元:不采用时间下限,但不影响发号。
func TestClock_BeforeEpoch(t *testing.T) {
	h := newHarness(t)
	h.primary.set(5000)
	h.secondary.set(5000)
	h.setClock(time.Unix(epochUnix-100, 0))
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 5000 {
		t.Fatalf("lo=%d, want 5000", lo)
	}
}

// 记录落后时钟很多:时间下限照样生效,不拒绝。一块号能用几个月,这在用量小的时候是常态。
func TestClock_FloorAppliesHoweverFarAhead(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1)<<regionShift | uint64(26_000_000)<<seqBits; lo != want {
		t.Fatalf("lo=%d, want the time floor %d", lo, want)
	}
}

// 时钟超出可表示的范围:不能挡住发号,按记录继续。
func TestClock_BeyondRangeIgnored(t *testing.T) {
	h := newHarness(t)
	h.primary.set(5000)
	h.secondary.set(5000)
	h.setClock(time.Unix(epochUnix+int64(1)<<timeBits+100, 0))
	iss := h.issuer(nil)
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != 5000 {
		t.Fatalf("lo=%d, want 5000", lo)
	}
}

// 计数器比时钟超前很多(时钟曾跳到未来):照常发号,只上报。
func TestClock_CounterFarAheadIsReported(t *testing.T) {
	h := newHarness(t)
	h.production(1)
	ahead := uint64(1)<<regionShift | (uint64(26_000_000)+maxAheadSeconds+1)<<seqBits
	h.primary.set(ahead)
	h.secondary.set(ahead)
	h.registry.setHighWater(1, ahead)
	obs := newObserver()
	iss := h.prodIssuer(1, func(o *Options) { o.OnReserve = obs.record })
	lo, _, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo != ahead {
		t.Fatalf("lo=%d, want %d", lo, ahead)
	}
	if obs.count(ReserveAheadOfClock) != 1 || obs.count(ReserveOK) != 1 || len(obs.seen) != 2 {
		t.Fatalf("observed %v", obs.seen)
	}
}

// ── 退避、预取 ──────────────────────────────────────────────────────────

// 预留失败之后,退避期内快速失败;过了退避期自动恢复,且发出的号仍在记录之下。
func TestReserve_RecoversAfterBackoff(t *testing.T) {
	h := newHarness(t)
	h.secondary.setErrors(nil, errors.New("secondary down"))
	iss := h.issuer(nil)
	mono := time.Unix(1000, 0)
	var monoMu sync.Mutex
	iss.monoNow = func() time.Time {
		monoMu.Lock()
		defer monoMu.Unlock()
		return mono
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("first allocate must fail")
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 100); err == nil {
		t.Fatal("must fail fast inside the backoff window")
	}
	h.secondary.setErrors(nil, nil)
	monoMu.Lock()
	mono = mono.Add(reserveRetryBackoff)
	monoMu.Unlock()
	_, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h.primary.get() < hi || h.secondary.get() < hi || h.registry.highWater(0) < hi {
		t.Fatalf("records (%d,%d,%d) must cover %d", h.primary.get(), h.secondary.get(), h.registry.highWater(0), hi)
	}
}

// 用到一半时后台预取下一块,切块时不用等存储;预取之后三份存储都坏了,手里的号也发得完。
func TestAllocate_PrefetchesNextBlockAtHalfway(t *testing.T) {
	h := newHarness(t)
	iss := h.production(1)
	a := &allocated{t: t}
	a.take(iss, 1)
	iss.mu.Lock()
	iss.cur.pos = iss.cur.halfway
	iss.mu.Unlock()
	a.take(iss, 1)
	iss.mu.Lock()
	res := iss.inflight
	iss.mu.Unlock()
	if res != nil {
		<-res.done
	}
	iss.mu.Lock()
	ready := iss.next != nil
	iss.mu.Unlock()
	if !ready {
		t.Fatal("the next block must be prefetched")
	}
	down := errors.New("down")
	h.primary.setErrors(down, down)
	h.secondary.setErrors(down, down)
	h.registry.setErrors(down, down)
	iss.mu.Lock()
	iss.cur.pos = iss.cur.end - 1
	iss.mu.Unlock()
	a.take(iss, 1)
	a.take(iss, 1)
}

// ── 部署身份 ────────────────────────────────────────────────────────────

// 读不到部署身份(K8s API 不可达、没有权限):这次预留什么都不写,单独上报 identity_error;读到之后照常预留,
// 而且成功一次就缓存,之后的预留不再读。
func TestReserve_DeploymentIdentityFailureWritesNothingAndSuccessIsCached(t *testing.T) {
	h := newHarness(t)
	h.production(1)
	primaryBefore, secondaryBefore, registryBefore := h.primary.get(), h.secondary.get(), h.registry.highWater(1)
	down := errors.New("kubernetes api unreachable")
	var reads atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	obs := newObserver()
	iss := h.prodIssuer(1, func(o *Options) {
		o.OnReserve = obs.record
		o.Deployment = func(ctx context.Context) (string, error) {
			reads.Add(1)
			if failing.Load() {
				return "", down
			}
			return h.identity(ctx)
		}
	})
	mono := time.Unix(1000, 0)
	var monoMu sync.Mutex
	iss.monoNow = func() time.Time {
		monoMu.Lock()
		defer monoMu.Unlock()
		return mono
	}

	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, down) {
		t.Fatalf("allocate: err=%v, want the identity error", err)
	}
	if h.primary.get() != primaryBefore || h.secondary.get() != secondaryBefore || h.registry.highWater(1) != registryBefore {
		t.Fatal("nothing may be written without the deployment identity")
	}
	if obs.count(ReserveIdentityError) != 1 || obs.count(ReserveOK) != 0 {
		t.Fatalf("observed %v", obs.seen)
	}

	failing.Store(false)
	monoMu.Lock()
	mono = mono.Add(reserveRetryBackoff)
	monoMu.Unlock()
	a := &allocated{t: t}
	a.take(iss, 100)
	exhaust(iss)
	a.take(iss, 100)
	if got := reads.Load(); got != 2 {
		t.Fatalf("identity reads=%d, want 2: one failure, then cached after the first success", got)
	}
}

// 读到的部署身份格式不对:初始化拒绝,什么都不写。
func TestDeployment_MalformedIdentityIsRefused(t *testing.T) {
	h := newHarness(t)
	iss := h.prodIssuer(1, func(o *Options) {
		o.Deployment = func(context.Context) (string, error) { return "Not A Valid Identity", nil }
	})
	if _, err := iss.OpenRegistry(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := iss.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "malformed deployment identity") {
		t.Fatalf("init: err=%v, want a malformed identity error", err)
	}
	if _, ok := h.registry.get(1); ok || h.primary.has() || h.secondary.has() {
		t.Fatal("nothing may be written with a malformed identity")
	}
}

// ── 地域、构造、并发、关闭 ──────────────────────────────────────────────

func TestRegion_InHighBits(t *testing.T) {
	h := newHarness(t)
	iss := h.production(5)
	lo, hi, err := iss.Allocate(context.Background(), "player", 100)
	if err != nil {
		t.Fatal(err)
	}
	if lo>>regionShift != 5 || hi>>regionShift != 5 {
		t.Fatalf("range [%d, %d) must carry region 5", lo, hi)
	}
	if hi >= 1<<63 {
		t.Fatalf("hi=%d must stay below 2^63", hi)
	}
}

func TestRegion_MismatchRefused(t *testing.T) {
	h := newHarness(t)
	h.primary.set(uint64(2)<<regionShift | 100)
	h.secondary.set(uint64(2)<<regionShift | 100)
	iss := h.issuer(func(o *Options) { o.Region = 3 })
	if _, _, err := iss.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegionMismatch) {
		t.Fatalf("err=%v, want ErrRegionMismatch", err)
	}
}

// 登记处这个码的水位带着别的地域:同样拒绝。
func TestRegion_RegistryMismatchRefused(t *testing.T) {
	h := newHarness(t)
	h.production(3)
	h.registry.setHighWater(3, uint64(2)<<regionShift|100)
	if _, _, err := h.prodIssuer(3, nil).Allocate(context.Background(), "player", 100); !errors.Is(err, ErrRegionMismatch) {
		t.Fatalf("err=%v, want ErrRegionMismatch", err)
	}
}

func TestNew_RejectsBadOptions(t *testing.T) {
	p, s, g := &memStore{}, &memStore{}, newMemRegistry(0)
	deployment := func(context.Context) (string, error) { return "k8s:cluster-a", nil }
	cases := []Options{
		{Secondary: s, Registry: g, Tags: []string{"a"}, Deployment: deployment},
		{Primary: p, Registry: g, Tags: []string{"a"}, Deployment: deployment},
		{Primary: p, Secondary: s, Tags: []string{"a"}, Deployment: deployment},
		{Primary: p, Secondary: s, Registry: g, Deployment: deployment},
		{Primary: p, Secondary: s, Registry: g, Tags: []string{"Bad Tag"}, Deployment: deployment},
		{Primary: p, Secondary: s, Registry: g, Tags: []string{"a"}, Region: MaxRegion + 1, Deployment: deployment},
		{Primary: p, Secondary: s, Registry: g, Tags: []string{"a"}, BlockSize: 1, Deployment: deployment},
		// 生产形态没有部署身份:地域码绑定退回只比 etcd 集群号,分不出两个 K8s 集群。
		{Primary: p, Secondary: s, Registry: g, Tags: []string{"a"}},
	}
	for i, opt := range cases {
		if _, err := New(opt); err == nil {
			t.Errorf("case %d must be rejected", i)
		}
	}
	if _, err := New(Options{Primary: p, Secondary: s, Registry: g, Tags: []string{"a"}, AllowBootstrap: true}); err != nil {
		t.Fatalf("dev may run without a deployment identity: %v", err)
	}
	iss, err := New(Options{Primary: p, Secondary: s, Registry: g, Tags: []string{"a"}, Deployment: deployment})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iss.OpenRegistry(context.Background(), MaxNextRegion+1); err == nil {
		t.Fatal("next region beyond the code space must be rejected")
	}
}

// concurrentTake 让每个发号器并发领段,收集成功的段。冲突重试有上限,偶发失败是预期内的,调用方会重试。
func concurrentTake(t *testing.T, issuers []*Issuer, perIssuer int) [][2]uint64 {
	t.Helper()
	results := make(chan [2]uint64, len(issuers)*perIssuer)
	var wg sync.WaitGroup
	for _, iss := range issuers {
		wg.Add(1)
		go func(iss *Issuer) {
			defer wg.Done()
			for n := 0; n < perIssuer; n++ {
				lo, hi, err := iss.Allocate(context.Background(), "player", uint32(testBlock/64))
				if err != nil {
					time.Sleep(time.Millisecond)
					continue
				}
				results <- [2]uint64{lo, hi}
			}
		}(iss)
	}
	wg.Wait()
	close(results)
	var out [][2]uint64
	for r := range results {
		out = append(out, r)
	}
	return out
}

// dev 形态多个副本同时第一次预留(顺带认领登记处),全部段不重叠。
func TestConcurrent_DevReplicasDoNotOverlap(t *testing.T) {
	h := newHarness(t)
	segs := concurrentTake(t, []*Issuer{h.issuer(nil), h.issuer(nil), h.issuer(nil), h.issuer(nil)}, 300)
	a := &allocated{t: t}
	for _, s := range segs {
		a.add(s[0], s[1])
	}
	if len(a.ranges) == 0 {
		t.Fatal("no segment was issued")
	}
}

func TestClose_StopsAllocating(t *testing.T) {
	h := newHarness(t)
	iss := h.issuer(nil)
	if err := iss.Close(); err != nil {
		t.Fatal(err)
	}
	if !h.primary.closed || !h.secondary.closed || !h.registry.closed {
		t.Fatal("Close must close all three stores")
	}
	if _, _, err := iss.Allocate(context.Background(), "player", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("err=%v, want ErrClosed", err)
	}
}

// ── 存储格式(不连 etcd)────────────────────────────────────────────────

func TestDecodeStrict(t *testing.T) {
	var r recordJSON
	if err := decodeStrict([]byte(`{"counter":12,"authority":"`+testAuthority+`"}`), &r); err != nil || r.Counter != 12 {
		t.Fatalf("valid record: %+v %v", r, err)
	}
	for _, raw := range []string{
		`42`,
		`{"counter":12,"authority":"x","extra":1}`,
		`{"counter":12} {"counter":13}`,
		`{"counter":-1}`,
		`{"counter":1.5}`,
	} {
		var out recordJSON
		if err := decodeStrict([]byte(raw), &out); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
	var big recordJSON
	if err := decodeStrict([]byte(`{"counter":9223372036854775807,"authority":"`+testAuthority+`"}`), &big); err != nil || big.Counter != 1<<63-1 {
		t.Fatalf("63-bit counters must round-trip exactly: %d %v", big.Counter, err)
	}
}

func TestValidClaim(t *testing.T) {
	for _, c := range []Claim{
		{State: ClaimActive, Authority: testAuthority, HighWater: 1},
		{State: ClaimActive, Authority: testAuthority, Deployment: "k8s:5b1e6c4a-0d2f-4c3b-9a8e-7f6d5c4b3a21", HighWater: 1},
		{State: ClaimRetired},
		{State: ClaimRetired, Authority: testAuthority, HighWater: 9},
	} {
		if !validClaim(c) {
			t.Errorf("%+v must be valid", c)
		}
	}
	for _, c := range []Claim{
		{State: ClaimActive, HighWater: 1},
		{State: ClaimActive, Authority: testAuthority},
		{State: "paused", Authority: testAuthority, HighWater: 1},
		{State: ClaimRetired, Authority: "not-hex"},
		{State: ClaimActive, Authority: testAuthority, Deployment: "Not A Valid Identity", HighWater: 1},
		{State: ClaimRetired, Deployment: "k8s:" + strings.Repeat("a", 200)},
	} {
		if validClaim(c) {
			t.Errorf("%+v must be invalid", c)
		}
	}
	if RegistryKey(7) != "/id_registry/v1/region/07" {
		t.Fatalf("registry key %q", RegistryKey(7))
	}
}

// 登记处记录的 JSON 带上部署身份,读回来逐字段相同;本字段出现之前写的记录没有它,照样能读,身份为空。
func TestClaimJSON_CarriesTheDeployment(t *testing.T) {
	want := Claim{State: ClaimActive, Authority: testAuthority, Deployment: "k8s:cluster-a", PrimaryCluster: 1,
		SecondaryCluster: 2, HighWater: 9, Blocks: 3}
	raw, err := json.Marshal(toClaimJSON(want))
	if err != nil {
		t.Fatal(err)
	}
	var body claimJSON
	if err := decodeStrict(raw, &body); err != nil || body != toClaimJSON(want) {
		t.Fatalf("round trip: %s -> %+v %v", raw, body, err)
	}
	var legacy claimJSON
	old := `{"state":"active","authority":"` + testAuthority + `","primary_cluster":1,"secondary_cluster":2,"high_water":9,"blocks":3}`
	if err := decodeStrict([]byte(old), &legacy); err != nil || legacy.Deployment != "" || legacy.HighWater != 9 {
		t.Fatalf("a record without the deployment field: %+v %v", legacy, err)
	}
}
