package main

// 工具的状态机。全程假 etcd、假 SQL、假复制:核的是每种故障的结局和步骤的先后。真库用例在 integration_test.go。

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/dbplacement"
)

const (
	testDB       = "mmorpg_guild"
	testRun      = "0123456789abcdef0123456789abcdef"
	testOtherRun = "ffffffffffffffffffffffffffffffff"
	testSource   = "a"
	testTarget   = "b"
)

var errEtcdDown = errors.New("etcd is down")

var testFeed = dbplacement.FeedIdentity{Changefeed: "feed-1", StartTs: 100, CreateTime: "2026-10-31 10:00:00"}

type eventLog struct{ events []string }

func (l *eventLog) add(event string) { l.events = append(l.events, event) }

// indexOf 返回事件第一次出现的位置,没有时是 -1。
func (l *eventLog) indexOf(event string) int {
	for i, e := range l.events {
		if e == event {
			return i
		}
	}
	return -1
}

func (l *eventLog) has(event string) bool { return l.indexOf(event) >= 0 }

func (l *eventLog) mustPrecede(t *testing.T, first, second string) {
	t.Helper()
	a, b := l.indexOf(first), l.indexOf(second)
	if a < 0 || b < 0 || a >= b {
		t.Fatalf("%s 必须在 %s 之前: %v", first, second, l.events)
	}
}

type fakeStore struct {
	log     *eventLog
	rev     int64
	records map[string]dbplacement.Record
	getErr  error
	listErr error
	// beforePut 在写之前调,模拟并发改映射和 etcd 故障:回错误就不再往下走。
	beforePut func(expected int64, p dbplacement.Placement) error
	// defaultCluster 是 etcd 里记下的默认集群,空串表示还没有部署写过。
	defaultCluster string
}

// onFlip 只拦翻转那一次写:窗口里它前面还有一次写目标位置。
func (s *fakeStore) onFlip(hook func(expected int64, p dbplacement.Placement) error) {
	s.beforePut = func(expected int64, p dbplacement.Placement) error {
		if p.Move == nil || p.Move.FlippedMs == 0 {
			return nil
		}
		return hook(expected, p)
	}
}

func (s *fakeStore) DefaultCluster(context.Context) (string, bool, error) {
	if s.getErr != nil {
		return "", false, s.getErr
	}
	return s.defaultCluster, s.defaultCluster != "", nil
}

func (s *fakeStore) Get(_ context.Context, name string) (dbplacement.Record, error) {
	if s.getErr != nil {
		return dbplacement.Record{}, s.getErr
	}
	rec := s.records[name]
	rec.Placement = clonePlacement(rec.Placement)
	return rec, nil
}

func (s *fakeStore) List(context.Context) (map[string]dbplacement.Record, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make(map[string]dbplacement.Record, len(s.records))
	for name, rec := range s.records {
		out[name] = rec
	}
	return out, nil
}

func (s *fakeStore) CompareAndPut(_ context.Context, name string, expected int64, p dbplacement.Placement) error {
	s.log.add("store.put")
	if p.Move != nil && p.Move.FlippedMs != 0 {
		s.log.add("store.flip")
	}
	if s.beforePut != nil {
		if err := s.beforePut(expected, p); err != nil {
			return err
		}
	}
	return s.apply(name, expected, p)
}

func (s *fakeStore) apply(name string, expected int64, p dbplacement.Placement) error {
	if s.records[name].ModRevision != expected {
		return dbplacement.ErrConflict
	}
	s.rev++
	s.records[name] = dbplacement.Record{Placement: clonePlacement(p), ModRevision: s.rev}
	return nil
}

type fakeGate struct {
	name        string
	log         *eventLog
	meta        *databasepb.RouteMeta
	sentinels   map[string]bool
	freezeErr   error
	releaseErr  error
	sentinelErr error
	probeErr    error
	metaErr     error
	principals  []string
}

func cloneMeta(meta *databasepb.RouteMeta) *databasepb.RouteMeta {
	return proto.Clone(meta).(*databasepb.RouteMeta)
}

func (g *fakeGate) CurrentMeta(context.Context) (*databasepb.RouteMeta, error) {
	if g.metaErr != nil {
		return nil, g.metaErr
	}
	return cloneMeta(g.meta), nil
}

// FreezeWriters、ReleaseWriters 和 Catalog.changeGate 同样的幂等规则。
func (g *fakeGate) FreezeWriters(_ context.Context, runID []byte) (*databasepb.RouteMeta, error) {
	g.log.add(g.name + ".freeze")
	if g.freezeErr != nil {
		return nil, g.freezeErr
	}
	if g.meta.Writable == 0 && bytes.Equal(g.meta.FreezeRunId, runID) {
		return cloneMeta(g.meta), nil
	}
	if g.meta.Writable != 1 || len(g.meta.FreezeRunId) != 0 {
		return nil, authoritysql.ErrWriterDenied
	}
	g.meta.WriterEpoch++
	g.meta.Writable = 0
	g.meta.FreezeRunId = bytes.Clone(runID)
	return cloneMeta(g.meta), nil
}

func (g *fakeGate) ReleaseWriters(_ context.Context, runID []byte) (*databasepb.RouteMeta, error) {
	g.log.add(g.name + ".release")
	if g.releaseErr != nil {
		return nil, g.releaseErr
	}
	if g.meta.Writable == 1 && len(g.meta.FreezeRunId) == 0 {
		return cloneMeta(g.meta), nil
	}
	if g.meta.Writable != 0 || !bytes.Equal(g.meta.FreezeRunId, runID) {
		return nil, authoritysql.ErrWriterDenied
	}
	g.meta.WriterEpoch++
	g.meta.Writable = 1
	g.meta.FreezeRunId = nil
	return cloneMeta(g.meta), nil
}

func (g *fakeGate) WriteSentinel(_ context.Context, nonce, _ []byte, _ uint64) error {
	g.log.add(g.name + ".sentinel")
	if g.sentinelErr != nil {
		return g.sentinelErr
	}
	g.sentinels[string(nonce)] = true
	return nil
}

func (g *fakeGate) HasSentinel(_ context.Context, nonce []byte) (bool, error) {
	return g.sentinels[string(nonce)], nil
}

func (g *fakeGate) ProbeControl(context.Context) error { return g.probeErr }

func (g *fakeGate) ActivePrincipals(context.Context) ([]string, error) {
	return append([]string(nil), g.principals...), nil
}

func (g *fakeGate) SchemaCharset(context.Context) (string, string, error) {
	return "utf8mb4", "utf8mb4_bin", nil
}

func (g *fakeGate) frozenBy(run []byte) bool {
	return g.meta.Writable == 0 && bytes.Equal(g.meta.FreezeRunId, run)
}

func (g *fakeGate) writable() bool {
	return g.meta.Writable == 1 && len(g.meta.FreezeRunId) == 0
}

type fakeAdmin struct {
	log         *eventLog
	backend     string
	schemas     map[string]bool
	accounts    map[string]bool
	frozen      bool
	dropErr     error
	locked      map[string]bool
	lockErr     error
	grants      map[string][]string
	grantsErr   error
	connections []accountConnection
	connErr     error
	flipMarker  bool
	markerErr   error
	lockedErr   error
	seeAllErr   error
	// gate 是这一侧逻辑库的闸门,GateState 读它。
	gate    *fakeGate
	gateErr error
}

func (a *fakeAdmin) ServerBackend(context.Context) (string, error) { return a.backend, nil }
func (a *fakeAdmin) SchemaExists(_ context.Context, schema string) (bool, error) {
	return a.schemas[schema], nil
}
func (a *fakeAdmin) CreateSchema(_ context.Context, schema, _, _ string) error {
	a.log.add("admin.create")
	a.schemas[schema] = true
	return nil
}
func (a *fakeAdmin) DropSchema(_ context.Context, schema string) error {
	a.log.add("admin.drop")
	if a.dropErr != nil {
		return a.dropErr
	}
	delete(a.schemas, schema)
	return nil
}
func (a *fakeAdmin) AccountExists(_ context.Context, principal string) (bool, error) {
	return a.accounts[principal], nil
}
func (a *fakeAdmin) GateFrozenBy(context.Context, string, []byte) (bool, error) {
	return a.frozen, nil
}
func (a *fakeAdmin) SetAccountLocked(_ context.Context, principal string, locked bool) error {
	if locked {
		a.log.add("admin.lock")
	} else {
		a.log.add("admin.unlock")
	}
	if a.lockErr != nil {
		return a.lockErr
	}
	a.locked[principal] = locked
	return nil
}
func (a *fakeAdmin) AccountGrants(_ context.Context, principal string) ([]string, error) {
	return a.grants[principal], a.grantsErr
}
func (a *fakeAdmin) Connections(context.Context) ([]accountConnection, error) {
	return a.connections, a.connErr
}
func (a *fakeAdmin) HasFlipMarker(context.Context, string, []byte) (bool, error) {
	return a.flipMarker, a.markerErr
}
func (a *fakeAdmin) AccountLocked(_ context.Context, principal string) (bool, error) {
	return a.locked[principal], a.lockedErr
}
func (a *fakeAdmin) CheckSeesAllConnections(context.Context) error { return a.seeAllErr }
func (a *fakeAdmin) GateState(context.Context, string) (gateState, error) {
	if a.gateErr != nil {
		return gateState{}, a.gateErr
	}
	if a.gate == nil {
		return gateState{}, nil
	}
	return gateState{Found: true, Writable: a.gate.meta.Writable == 1, WriterEpoch: a.gate.meta.WriterEpoch}, nil
}

// fakeRepl:默认复制正常,CaughtUp 把源库的闸门和哨兵复制到目标。
type fakeRepl struct {
	log         *eventLog
	source      *fakeGate
	target      *fakeGate
	lagErr      error
	caughtUp    func() (bool, error)
	markErr     error
	point       verifyPoint
	pointErr    error
	recheckErr  error
	feed        dbplacement.FeedIdentity
	identityErr error
	targetPoint string
}

func (r *fakeRepl) replicate() {
	r.target.meta = cloneMeta(r.source.meta)
	for nonce := range r.source.sentinels {
		r.target.sentinels[nonce] = true
	}
}

func (r *fakeRepl) CheckLag(context.Context, time.Duration) error {
	r.log.add("repl.lag")
	return r.lagErr
}

func (r *fakeRepl) Mark(context.Context) (string, error) {
	r.log.add("repl.mark")
	return "mark", r.markErr
}

func (r *fakeRepl) CaughtUp(context.Context, string) (bool, error) {
	if r.caughtUp != nil {
		return r.caughtUp()
	}
	r.replicate()
	return true, nil
}

func (r *fakeRepl) TargetPoint(context.Context) (string, error) { return r.targetPoint, nil }

func (r *fakeRepl) Identity(context.Context) (dbplacement.FeedIdentity, error) {
	return r.feed, r.identityErr
}

func (r *fakeRepl) VerifyPoint(context.Context) (verifyPoint, error) {
	return r.point, r.pointErr
}

func (r *fakeRepl) RecheckVerifyPoint(context.Context, verifyPoint) error { return r.recheckErr }

type fakeView struct {
	tables   map[string]tableDigest
	snapshot string
	pointErr error
	closed   bool
	gate     gateRows
	gateErr  error
}

func (v *fakeView) Tables(context.Context) ([]string, error) {
	var out []string
	for name := range v.tables {
		out = append(out, name)
	}
	return out, nil
}
func (v *fakeView) TableDigest(_ context.Context, table string) (tableDigest, error) {
	return v.tables[table], nil
}
func (v *fakeView) CheckPoint(context.Context, string) error { return v.pointErr }
func (v *fakeView) GateRows(context.Context) (gateRows, error) {
	return v.gate, v.gateErr
}
func (v *fakeView) Close() error { v.closed = true; return nil }

type harness struct {
	m           *mover
	log         *eventLog
	store       *fakeStore
	source      *fakeGate
	target      *fakeGate
	sourceAdmin *fakeAdmin
	targetAdmin *fakeAdmin
	repl        *fakeRepl
	sourceView  *fakeView
	targetView  *fakeView
	out         *bytes.Buffer
	now         time.Time
	metrics     map[string]uint64
	// clusterless 是不认识目标集群的进程;cannotScrape 是读不到指标的进程。
	clusterless  map[string]bool
	cannotScrape map[string]bool
	run          []byte
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := &eventLog{}
	run, err := hex.DecodeString(testRun)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{log: log, run: run, out: &bytes.Buffer{}, now: time.UnixMilli(1_800_000_000_000), metrics: map[string]uint64{}, clusterless: map[string]bool{}, cannotScrape: map[string]bool{}}
	h.store = &fakeStore{log: log, records: map[string]dbplacement.Record{}, defaultCluster: testSource}
	h.source = &fakeGate{name: "source", log: log, sentinels: map[string]bool{}, principals: []string{"writer@%"},
		meta: &databasepb.RouteMeta{Id: 1, Generation: "ab", Writable: 1, WriterEpoch: 5, CatalogRevision: 9}}
	// 目标的闸门表要等复制过去。
	h.target = &fakeGate{name: "target", log: log, sentinels: map[string]bool{}, meta: &databasepb.RouteMeta{Id: 1}}
	h.sourceAdmin = &fakeAdmin{log: log, backend: "tidb", schemas: map[string]bool{testDB: true}, accounts: map[string]bool{}, frozen: true, locked: map[string]bool{}}
	h.targetAdmin = &fakeAdmin{log: log, backend: "tidb", schemas: map[string]bool{}, accounts: map[string]bool{"writer@%": true, "maintenance@%": true}, locked: map[string]bool{}, gate: h.target}
	h.repl = &fakeRepl{log: log, source: h.source, target: h.target, feed: testFeed, targetPoint: "target-point", point: verifyPoint{Source: "100", Target: "200", Detail: "syncpoint", Feed: testFeed}}
	matching := map[string]tableDigest{"guild": {Columns: "id", Rows: 3, Digest: "abc"}}
	h.sourceView = &fakeView{tables: matching}
	h.targetView = &fakeView{tables: matching}
	h.m = &mover{
		o: moverOptions{
			db: testDB, targetCluster: testTarget, runID: testRun, runIDBytes: run, operator: "maintenance@%", backend: "tidb",
			config: dbplacement.Config{DefaultCluster: testSource, EtcdHosts: []string{"e:2379"}, SqlClusters: map[string]dbplacement.SqlCluster{
				testSource: {Hosts: "10.0.0.1:4000"}, testTarget: {Hosts: "10.0.0.2:4000"},
			}},
			maxLag: 3 * time.Second, budget: 2 * time.Second, rollbackBudget: time.Second, preflight: time.Second,
			verifyMaxAge: time.Hour, retention: 24 * time.Hour, pollInterval: time.Millisecond, metricsTargets: []string{"game:9101"},
		},
		store: h.store, source: h.source, target: h.target, sourceAdmin: h.sourceAdmin, targetAdmin: h.targetAdmin, repl: h.repl,
		openView: func(_ context.Context, role, snapshot string) (checksumView, error) {
			view := h.sourceView
			if role == roleTarget {
				view = h.targetView
			}
			view.snapshot = snapshot
			return view, nil
		},
		scrape: func(_ context.Context, target, _ string) (uint64, bool, error) {
			version, found := h.metrics[target]
			return version, found, nil
		},
		knownCluster: func(_ context.Context, target, _ string) (bool, error) {
			if h.cannotScrape[target] {
				return false, errors.New("connection refused")
			}
			return !h.clusterless[target], nil
		},
		now:   func() time.Time { return h.now },
		sleep: sleepContext,
		log:   &journal{out: h.out, now: time.Now},
	}
	return h
}

// seedMoving 把映射放到 prepare、verify 都做过的状态。
func (h *harness) seedMoving() dbplacement.Record {
	rec := dbplacement.Record{ModRevision: 7, Placement: dbplacement.Placement{Cluster: testSource, Version: 2, State: dbplacement.StateMoving, Move: &dbplacement.Move{
		RunID: testRun, SourceCluster: testSource, TargetCluster: testTarget, VerifiedMs: uint64(h.now.UnixMilli()), VerifiedFeed: &dbplacement.FeedIdentity{Changefeed: testFeed.Changefeed, StartTs: testFeed.StartTs, CreateTime: testFeed.CreateTime},
		LockedAccounts: []string{"writer@%"},
	}}}
	h.store.records[testDB] = rec
	h.store.rev = 7
	return rec
}

func (h *harness) seedFlipped(audited bool) {
	move := &dbplacement.Move{RunID: testRun, SourceCluster: testSource, TargetCluster: testTarget, FlippedMs: uint64(h.now.Add(-48 * time.Hour).UnixMilli()), TargetPoint: "target-point"}
	if audited {
		move.AuditedMs = move.FlippedMs + 1
	}
	h.store.records[testDB] = dbplacement.Record{ModRevision: 8, Placement: dbplacement.Placement{Cluster: testTarget, Version: 3, State: dbplacement.StateActive, Move: move}}
	h.store.rev = 8
}

func (h *harness) placement() dbplacement.Placement { return h.store.records[testDB].Placement }

func requireRefused(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("应拒绝且提到 %q: %v", want, err)
	}
}

func TestPlanChecksWithoutWriting(t *testing.T) {
	h := newHarness(t)
	if err := h.m.plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.log.events) != 0 || h.store.rev != 0 {
		t.Fatalf("plan 不写任何东西: %v rev=%d", h.log.events, h.store.rev)
	}
}

func TestPlanPrintsRunIDWhenMissing(t *testing.T) {
	h := newHarness(t)
	h.m.o.runID = ""
	if err := h.m.plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "run id") {
		t.Fatalf("没给 run id 时要给一个: %s", h.out)
	}
}

func TestPlanRefusals(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"后端不同", func(h *harness) { h.targetAdmin.backend = "mysql" }, "same backend"},
		{"目标已有这个库", func(h *harness) { h.targetAdmin.schemas[testDB] = true }, "already has a database"},
		{"写者账号不存在", func(h *harness) { delete(h.targetAdmin.accounts, "writer@%") }, "writer@%"},
		{"维护账号不存在", func(h *harness) { delete(h.targetAdmin.accounts, "maintenance@%") }, "maintenance@%"},
		{"源库闸门已冻结", func(h *harness) { h.source.meta.Writable = 0 }, "frozen"},
		{"维护账号没有权限", func(h *harness) { h.source.probeErr = errors.New("denied") }, "cannot control"},
		{"目标集群不认识", func(h *harness) { h.m.o.targetCluster = "z" }, "not in SqlClusters"},
		{"已经在目标上", func(h *harness) { h.m.o.targetCluster = testSource }, "already on cluster"},
		{"连接串指错集群", func(h *harness) { h.m.o.targetAddr = "10.9.9.9:4000" }, "the DSN points at"},
		{"别的库正在搬", func(h *harness) {
			h.store.records["mmorpg_mail"] = dbplacement.Record{ModRevision: 1, Placement: dbplacement.Placement{Cluster: testSource, State: dbplacement.StateMoving}}
		}, "one logical database at a time"},
		{"etcd 里没有默认集群", func(h *harness) { h.store.defaultCluster = "" }, "deploy the services first"},
		{"配置的默认集群和 etcd 不一致", func(h *harness) { h.store.defaultCluster = "c" }, "DefaultCluster"},
		{"写者账号还写目标上别的库", func(h *harness) {
			h.targetAdmin.grants = map[string][]string{"writer@%": {"GRANT SELECT ON `player_store_2_db`.`player_store` TO 'writer'@'%'"}}
		}, "-allow-shared-writer-accounts"},
		{"上一次搬迁的源库没清", func(h *harness) {
			h.store.records[testDB] = dbplacement.Record{ModRevision: 1, Placement: dbplacement.Placement{Cluster: "c", Version: 1, State: dbplacement.StateActive, Move: &dbplacement.Move{RunID: testOtherRun, SourceCluster: testSource, TargetCluster: "c", FlippedMs: 1}}}
		}, "not cleaned up"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			test.change(h)
			requireRefused(t, h.m.plan(context.Background()), test.want)
		})
	}
}

func TestPlanAllowsTunnelAddressWhenAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.m.o.targetAddr = "127.0.0.1:14000"
	h.m.o.allowAddrMismatch = true
	if err := h.m.plan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareMarksMovingThenCreatesTarget(t *testing.T) {
	h := newHarness(t)
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.placement()
	if p.State != dbplacement.StateMoving || p.Cluster != testSource || p.Version != 0 {
		t.Fatalf("映射: %+v", p)
	}
	if p.Move == nil || p.Move.RunID != testRun || p.Move.TargetCluster != testTarget || p.Move.SourceCluster != testSource {
		t.Fatalf("搬迁记录: %+v", p.Move)
	}
	if !h.targetAdmin.schemas[testDB] {
		t.Fatal("目标上要建空库")
	}
	h.log.mustPrecede(t, "store.put", "admin.create")
	// 重跑幂等:库已经建了也不拒绝,映射不再写。
	puts := h.store.rev
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if h.store.rev != puts {
		t.Fatal("重跑不该再写映射")
	}
}

func TestPrepareRefusesAnotherRun(t *testing.T) {
	h := newHarness(t)
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.m.o.runID = testOtherRun
	requireRefused(t, h.m.prepare(context.Background()), "already being moved")
}

func TestPrepareKeepsVersionOfMovedDatabase(t *testing.T) {
	h := newHarness(t)
	h.store.records[testDB] = dbplacement.Record{ModRevision: 3, Placement: dbplacement.Placement{Cluster: "c", Version: 4, State: dbplacement.StateActive, Move: &dbplacement.Move{RunID: testOtherRun, FlippedMs: 1, CleanedMs: 2}}}
	h.store.rev = 3
	if err := h.m.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.placement()
	if p.Version != 4 || p.Cluster != "c" || p.Move.SourceCluster != "c" {
		t.Fatalf("prepare 不动版本,源是当前集群: %+v %+v", p, p.Move)
	}
}

func TestVerifyRecordsStartTime(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	started := uint64(h.now.UnixMilli())
	h.m.now = func() time.Time {
		defer func() { h.now = h.now.Add(time.Minute) }()
		return h.now
	}
	if err := h.m.verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.placement().Move.VerifiedMs; got != started {
		t.Fatalf("新鲜度从开始时刻算: %d, want %d", got, started)
	}
	if h.sourceView.snapshot != "100" || h.targetView.snapshot != "200" {
		t.Fatalf("两边要站在同一对快照上: %s %s", h.sourceView.snapshot, h.targetView.snapshot)
	}
	if !h.sourceView.closed || !h.targetView.closed {
		t.Fatal("视图要关")
	}
}

func TestVerifyMismatchRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.targetView.tables = map[string]tableDigest{"guild": {Columns: "id", Rows: 2, Digest: "abc"}}
	err := h.m.verify(context.Background())
	if !errors.Is(err, errMismatch) || exitCodeOf(err) != exitRefused {
		t.Fatalf("不一致: %v", err)
	}
	if h.placement().Move.VerifiedMs != 0 {
		t.Fatal("不一致不能记成功")
	}
}

func TestVerifyFailsWhenReplicaMoved(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = 0
	h.repl.recheckErr = refusedf("the source replica moved")
	if err := h.m.verify(context.Background()); !errors.Is(err, errRefused) {
		t.Fatalf("副本动过,比出来的不算数: %v", err)
	}
	if h.placement().Move.VerifiedMs != 0 {
		t.Fatal("不能记成功")
	}
}

func TestVerifyRequiresMoving(t *testing.T) {
	h := newHarness(t)
	requireRefused(t, h.m.verify(context.Background()), "not being moved")
	h.seedMoving()
	h.m.o.runID = testOtherRun
	requireRefused(t, h.m.verify(context.Background()), "being moved by run")
}

func TestCutoverHappyPathOrderAndResult(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.placement()
	if p.Cluster != testTarget || p.Version != 3 || p.State != dbplacement.StateActive {
		t.Fatalf("翻转结果: %+v", p)
	}
	if p.Move.FlippedMs == 0 || p.Move.TargetPoint != "target-point" {
		t.Fatalf("搬迁记录: %+v", p.Move)
	}
	if !h.source.frozenBy(h.run) {
		t.Fatal("翻转之后源库保持冻结:旧进程写不进去")
	}
	if !h.target.writable() {
		t.Fatal("目标闸门要打开")
	}
	// 目标的 epoch 接着冻结点往后走,写者不用换身份。
	if h.target.meta.WriterEpoch != h.source.meta.WriterEpoch+1 {
		t.Fatalf("目标 epoch=%d 源 epoch=%d", h.target.meta.WriterEpoch, h.source.meta.WriterEpoch)
	}
	for _, order := range [][2]string{
		{"repl.lag", "source.freeze"},
		{"source.freeze", "source.sentinel"},
		{"source.sentinel", "repl.mark"},
		{"repl.mark", "target.release"},
		{"repl.mark", "store.put"},    // 目标位置先写进映射
		{"store.put", "admin.unlock"}, // 再解锁账号
		{"admin.unlock", "target.release"},
		{"target.release", "store.flip"},
		{"store.flip", "target.sentinel"}, // 翻转之后写标记
	} {
		h.log.mustPrecede(t, order[0], order[1])
	}
	if h.targetAdmin.locked["writer@%"] {
		t.Fatal("翻转之后账号要解锁")
	}
	if !h.target.sentinels[string(flipMarkerNonce(testRun))] {
		t.Fatal("翻转之后目标上要有标记")
	}
	if h.log.has("source.release") {
		t.Fatal("成功时不解冻源库")
	}
}

func TestCutoverIsIdempotentAfterFlip(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	version, events := h.placement().Version, len(h.log.events)
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	// 重跑只把账号再解锁一次(幂等):并发的 rollback、prepare 可能在翻转之后把账号锁回去,不解锁新连接全部失败,
	// 见 TestCutoverAlreadyDoneUnlocksAccounts。不再冻结、不再翻转,标记已经写过也不再写。
	if rerun := h.log.events[events:]; h.placement().Version != version || !slices.Equal(rerun, []string{"admin.unlock"}) {
		t.Fatalf("翻转之后重跑只解锁账号: version=%d %v", h.placement().Version, rerun)
	}
}

func TestCutoverRefusesBeforeFreezing(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"没有 verify", func(h *harness) { h.store.records[testDB].Placement.Move.VerifiedMs = 0 }, "no successful verify"},
		{"verify 过期", func(h *harness) {
			h.store.records[testDB].Placement.Move.VerifiedMs = uint64(h.now.Add(-2 * time.Hour).UnixMilli())
		}, "run verify again"},
		{"延迟过高", func(h *harness) { h.repl.lagErr = errLagTooHigh }, "replication is not ready"},
		{"复制断了", func(h *harness) { h.repl.lagErr = errReplicationBroken }, "replication is not ready"},
		{"别的 run", func(h *harness) { h.m.o.runID = testOtherRun }, "being moved by run"},
		{"闸门被别的 run 冻结", func(h *harness) {
			h.source.meta.Writable = 0
			h.source.meta.FreezeRunId = bytes.Repeat([]byte{9}, 16)
		}, "another run"},
		{"源库没权限", func(h *harness) { h.source.probeErr = errors.New("denied") }, "source gate"},
		{"目标没权限", func(h *harness) { h.target.probeErr = errors.New("denied") }, "target gate"},
		{"连接串指错集群", func(h *harness) { h.m.o.targetAddr = "10.9.9.9:4000" }, "the DSN points at"},
		{"管理员连接串指错集群", func(h *harness) { h.m.o.targetAdminAddr = "10.9.9.9:4000" }, "the DSN points at"},
		{"复制流重建过", func(h *harness) { h.repl.feed.CreateTime = "2026-10-31 11:00:00" }, "run verify again"},
		{"复制流用了新的起点", func(h *harness) { h.repl.feed.StartTs = 999 }, "run verify again"},
		{"读不到复制流", func(h *harness) { h.repl.identityErr = errReplicationBroken }, "replication is not ready"},
		{"映射里没有复制流", func(h *harness) { h.store.records[testDB].Placement.Move.VerifiedFeed = nil }, "run verify again"},
		{"名册里的账号目标上没有", func(h *harness) {
			h.m.o.rosterPrincipals = []string{"writer_next@%"}
			h.store.records[testDB].Placement.Move.LockedAccounts = []string{"writer@%", "writer_next@%"}
		}, "writer_next@%"},
		{"名册里的账号 prepare 之后才有", func(h *harness) {
			h.targetAdmin.accounts["writer_next@%"] = true
			h.m.o.rosterPrincipals = []string{"writer_next@%"}
		}, "re-run prepare"},
		{"注册账号目标上没有", func(h *harness) { h.m.o.controlPrincipals = []string{"registrar@%"} }, "registrar@%"},
		{"写者缺授权", func(h *harness) {
			h.sourceAdmin.grants = map[string][]string{"writer@%": {"GRANT SELECT,INSERT ON `" + testDB + "`.`guild` TO 'writer'@'%'"}}
			h.targetAdmin.grants = map[string][]string{"writer@%": {"GRANT SELECT ON `" + testDB + "`.`guild` TO 'writer'@'%'"}}
		}, "INSERT on " + testDB + ".guild"},
		{"读不到授权", func(h *harness) { h.targetAdmin.grantsErr = errors.New("denied") }, "cannot read the grants"},
		{"进程不认识目标集群", func(h *harness) { h.clusterless["game:9101"] = true }, "do not know cluster b"},
		{"读不到进程的指标", func(h *harness) { h.cannotScrape["game:9101"] = true }, "cannot read metrics"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedMoving()
			test.change(h)
			requireRefused(t, h.m.cutover(context.Background()), test.want)
			if h.log.has("source.freeze") || h.log.has("store.put") {
				t.Fatalf("拒绝时什么都不能动: %v", h.log.events)
			}
		})
	}
}

func TestCutoverAcceptsFreshVerifyInsideMaxAge(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.records[testDB].Placement.Move.VerifiedMs = uint64(h.now.Add(-30 * time.Minute).UnixMilli())
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// requireRolledBack:回滚之后源库可写、映射还是 MOVING、翻转被作废、退出码 2。
func requireRolledBack(t *testing.T, h *harness, err error) {
	t.Helper()
	if err == nil || errors.Is(err, errUnknown) || exitCodeOf(err) != exitFailed {
		t.Fatalf("应回滚,退出码 2: %v", err)
	}
	if !h.source.writable() {
		t.Fatalf("回滚之后源库要解冻: %+v", h.source.meta)
	}
	p := h.placement()
	if p.Cluster != testSource || p.Version != 2 || p.State != dbplacement.StateMoving {
		t.Fatalf("映射不能翻转: %+v", p)
	}
	if p.Move.AbortedMs == 0 {
		t.Fatal("回滚之前要先作废翻转")
	}
}

func TestCutoverOverBudgetRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) { return false, nil }
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("要说明超预算: %v", err)
	}
	h.log.mustPrecede(t, "store.put", "source.release")
}

func TestCutoverCaughtUpButNoSentinelKeepsWaiting(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	// checkpoint 过了,目标上读不到哨兵:不能翻转。
	h.repl.caughtUp = func() (bool, error) { return true, nil }
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if h.log.has("target.release") {
		t.Fatal("没读到哨兵不能开目标闸门")
	}
}

func TestCutoverFreezeFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.source.freezeErr = errors.New("lock wait timeout")
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if h.log.has("source.sentinel") {
		t.Fatal("没冻结不能写哨兵")
	}
}

func TestCutoverFreezeRetriesInsideBudget(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	// 第一次锁等待超时,第二次成功。
	failed := false
	h.m.source = &flakyFreeze{fakeGate: h.source, fail: func() error {
		if failed {
			return nil
		}
		failed = true
		return errors.New("lock wait timeout")
	}}
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.source.frozenBy(h.run) || h.placement().Cluster != testTarget {
		t.Fatal("重试之后要完成")
	}
}

type flakyFreeze struct {
	*fakeGate
	fail func() error
}

func (f *flakyFreeze) FreezeWriters(ctx context.Context, runID []byte) (*databasepb.RouteMeta, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.fakeGate.FreezeWriters(ctx, runID)
}

func TestCutoverSentinelFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.source.sentinelErr = errors.New("denied")
	requireRolledBack(t, h, h.m.cutover(context.Background()))
}

func TestCutoverReplicationBrokenRollsBackAtOnce(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = time.Hour
	h.repl.caughtUp = func() (bool, error) { return false, errReplicationBroken }
	started := time.Now()
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !errors.Is(err, errReplicationBroken) {
		t.Fatalf("要带上原因: %v", err)
	}
	if time.Since(started) > time.Minute {
		t.Fatal("复制断开不该等满预算")
	}
}

func TestCutoverTargetGateMismatchRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	// 复制追上了,闸门表却没过去(过滤规则漏了)。
	h.repl.caughtUp = func() (bool, error) {
		h.target.sentinels = h.source.sentinels
		return true, nil
	}
	requireRolledBack(t, h, h.m.cutover(context.Background()))
	if h.log.has("target.release") {
		t.Fatal("闸门表没复制过去不能开")
	}
}

func TestCutoverTargetGateFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.target.releaseErr = errors.New("denied")
	requireRolledBack(t, h, h.m.cutover(context.Background()))
}

func TestCutoverContextCancelRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	ctx, cancel := context.WithCancel(context.Background())
	h.repl.caughtUp = func() (bool, error) {
		cancel()
		return false, nil
	}
	requireRolledBack(t, h, h.m.cutover(ctx))
}

// 翻转 CAS 冲突:映射被同一个 run 的另一个进程翻了。这次算成功,源库不解冻。
func TestCutoverFlipConflictWithSameRunFlipIsSuccess(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.onFlip(func(_ int64, p dbplacement.Placement) error {
		h.store.beforePut = nil
		return h.store.apply(testDB, h.store.records[testDB].ModRevision, flippedCopy(p))
	})
	// beforePut 回 nil 之后原来的 CAS 带的是旧版本,撞 ErrConflict。
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("同 run 已翻转算成功: %v", err)
	}
	if h.log.has("source.release") {
		t.Fatal("已翻转绝不能解冻源库")
	}
	if !h.source.frozenBy(h.run) || h.placement().Cluster != testTarget {
		t.Fatalf("结果: %+v", h.placement())
	}
}

func flippedCopy(p dbplacement.Placement) dbplacement.Placement { return clonePlacement(p) }

// 翻转 CAS 冲突:映射被改了但还是这个 run、还没翻转(比如并发的 verify 写了时间)。回滚,重跑就好。
func TestCutoverFlipConflictStillOursRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.onFlip(func(int64, dbplacement.Placement) error {
		h.store.beforePut = nil
		cur := h.store.records[testDB]
		return h.store.apply(testDB, cur.ModRevision, clonePlacement(cur.Placement))
	})
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	if !errors.Is(err, dbplacement.ErrConflict) {
		t.Fatalf("要带上 CAS 冲突: %v", err)
	}
}

// 翻转 CAS 冲突:映射被别的东西改得认不出了。不动,退出码 3。
func TestCutoverFlipConflictForeignLeavesSourceFrozen(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.onFlip(func(int64, dbplacement.Placement) error {
		h.store.beforePut = nil
		cur := h.store.records[testDB]
		return h.store.apply(testDB, cur.ModRevision, dbplacement.Placement{Cluster: "c", Version: 9, State: dbplacement.StateActive})
	})
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || exitCodeOf(err) != exitUnknown {
		t.Fatalf("结局未知: %v", err)
	}
	if !h.source.frozenBy(h.run) || h.log.has("source.release") {
		t.Fatal("认不出映射时不能解冻源库")
	}
}

// rollback 命令抢先作废了这个 run:窗口看到后解冻是安全的(幂等)。
func TestCutoverAbortSeesRollbackCommand(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) {
		cur := h.store.records[testDB]
		if cur.Placement.State == dbplacement.StateMoving {
			next := clonePlacement(cur.Placement)
			next.State = dbplacement.StateActive
			next.Move.RolledBackMs = 1
			if err := h.store.apply(testDB, cur.ModRevision, next); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	err := h.m.cutover(context.Background())
	if err == nil || errors.Is(err, errUnknown) {
		t.Fatalf("回滚: %v", err)
	}
	if !h.source.writable() {
		t.Fatal("rollback 作废之后要解冻")
	}
}

// 翻转时 etcd 不通:结局未知,绝不解冻。解冻之后翻转才落地,两边就各写一半。
func TestCutoverEtcdDownOnFlipKeepsSourceFrozen(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.rollbackBudget = 30 * time.Millisecond
	h.store.onFlip(func(int64, dbplacement.Placement) error {
		h.store.getErr = errEtcdDown
		return errEtcdDown
	})
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || exitCodeOf(err) != exitUnknown {
		t.Fatalf("结局未知: %v", err)
	}
	if !h.source.frozenBy(h.run) || h.log.has("source.release") {
		t.Fatal("翻转结局未知时不能解冻源库")
	}
	if !h.target.writable() {
		t.Fatal("目标闸门留着,重跑时幂等")
	}
	// etcd 恢复后重跑:翻转没落地,接着做完。
	h.store.getErr, h.store.beforePut = nil, nil
	h.m.o.rollbackBudget = time.Second
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if p := h.placement(); p.Cluster != testTarget || p.Version != 3 || !h.source.frozenBy(h.run) {
		t.Fatalf("重跑结果: %+v", p)
	}
}

// 翻转回了错误,其实已经落地:重读看到已翻转,算成功。
func TestCutoverFlipLandedDespiteError(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.onFlip(func(expected int64, p dbplacement.Placement) error {
		h.store.beforePut = nil
		if err := h.store.apply(testDB, expected, p); err != nil {
			t.Fatal(err)
		}
		return errors.New("context deadline exceeded")
	})
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("落地了算成功: %v", err)
	}
	if h.log.has("source.release") || !h.source.frozenBy(h.run) {
		t.Fatal("已翻转不能解冻")
	}
}

// 回错误且没落地、etcd 通:作废成功之后解冻,还在路上的翻转带的是旧版本,落不了地。
func TestCutoverFlipErrorThenInvalidatedRollsBack(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	var flipRevision int64
	h.store.onFlip(func(expected int64, _ dbplacement.Placement) error {
		h.store.beforePut = nil
		flipRevision = expected
		return errors.New("context deadline exceeded")
	})
	err := h.m.cutover(context.Background())
	requireRolledBack(t, h, err)
	// 迟到的翻转带着翻转那一刻的版本,必须落不了地。
	late := clonePlacement(h.placement())
	late.Cluster = testTarget
	if err := h.store.apply(testDB, flipRevision, late); !errors.Is(err, dbplacement.ErrConflict) {
		t.Fatalf("迟到的翻转落地了: %v", err)
	}
}

// 还没发过翻转时 etcd 不通:没有人翻转得了,解冻恢复服务。state 留在 MOVING。
func TestCutoverEtcdDownBeforeFlipUnfreezes(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.m.o.rollbackBudget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) {
		h.store.getErr = errEtcdDown
		return false, nil
	}
	err := h.m.cutover(context.Background())
	if err == nil || errors.Is(err, errUnknown) || exitCodeOf(err) != exitFailed {
		t.Fatalf("没发过翻转,解冻后是干净的失败: %v", err)
	}
	if !h.source.writable() {
		t.Fatal("要解冻恢复服务")
	}
	if p := h.placement(); p.Cluster != testSource || p.State != dbplacement.StateMoving {
		t.Fatalf("映射: %+v", p)
	}
}

// 解冻失败:源库还冻着,退出码 3,让人重跑 rollback。
func TestCutoverReleaseFailureIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.m.o.rollbackBudget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) { return false, nil }
	h.source.releaseErr = errors.New("connection refused")
	err := h.m.cutover(context.Background())
	if !errors.Is(err, errUnknown) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("要提示重跑 rollback: %v", err)
	}
}

// 工具崩在目标闸门打开之后、翻转之前:源库冻着,目标闸门开着。重跑做完。
func TestCutoverRerunAfterCrashBeforeFlip(t *testing.T) {
	h := newHarness(t)
	rec := h.seedMoving()
	// 上一次窗口打开目标闸门之前记下的位置。
	h.store.records[testDB].Placement.Move.TargetPoint = "point-before-open"
	if _, err := h.source.FreezeWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	h.repl.replicate()
	if _, err := h.target.ReleaseWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	// 这时再读位置,读到的已经包含那次打开。
	h.repl.targetPoint = "point-after-open"
	h.repl.caughtUp = func() (bool, error) {
		// 冻结没有变化,复制过去的只有新的哨兵;目标闸门表不被覆盖。
		for nonce := range h.source.sentinels {
			h.target.sentinels[nonce] = true
		}
		return true, nil
	}
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if p := h.placement(); p.Cluster != testTarget || p.Version != rec.Placement.Version+1 {
		t.Fatalf("结果: %+v", p)
	}
	if !h.source.frozenBy(h.run) || !h.target.writable() {
		t.Fatal("闸门状态不对")
	}
	if got := h.placement().Move.TargetPoint; got != "point-before-open" {
		t.Fatalf("重跑要复用打开之前记下的位置: %q", got)
	}
}

// 回滚之后再切一次:epoch 接着涨,目标闸门表被复制覆盖,照常走完。
func TestCutoverAgainAfterRollback(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.budget = 30 * time.Millisecond
	h.repl.caughtUp = func() (bool, error) {
		h.repl.replicate()
		return false, nil
	}
	if err := h.m.cutover(context.Background()); err == nil {
		t.Fatal("第一次应该超预算")
	}
	h.repl.replicate() // 源库解冻的那一步也复制过去了
	h.repl.caughtUp = nil
	h.m.o.budget = 2 * time.Second
	if err := h.m.cutover(context.Background()); err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if p := h.placement(); p.Cluster != testTarget || p.Version != 3 {
		t.Fatalf("结果: %+v", p)
	}
}

func TestTargetGateReady(t *testing.T) {
	run := bytes.Repeat([]byte{1}, 16)
	frozen := &databasepb.RouteMeta{Generation: "ab", Writable: 0, WriterEpoch: 6, CatalogRevision: 9, FreezeRunId: run}
	tests := []struct {
		name   string
		target *databasepb.RouteMeta
		want   bool
	}{
		{"复制过来的冻结点", &databasepb.RouteMeta{Generation: "ab", Writable: 0, WriterEpoch: 6, CatalogRevision: 9, FreezeRunId: run}, true},
		{"上一次重跑已打开", &databasepb.RouteMeta{Generation: "ab", Writable: 1, WriterEpoch: 7, CatalogRevision: 9}, true},
		{"复制落后", &databasepb.RouteMeta{Generation: "ab", Writable: 1, WriterEpoch: 5, CatalogRevision: 9}, false},
		{"别的 run 的冻结", &databasepb.RouteMeta{Generation: "ab", Writable: 0, WriterEpoch: 6, CatalogRevision: 9, FreezeRunId: bytes.Repeat([]byte{2}, 16)}, false},
		{"世代不符", &databasepb.RouteMeta{Generation: "cd", Writable: 0, WriterEpoch: 6, CatalogRevision: 9, FreezeRunId: run}, false},
		{"注册表落后", &databasepb.RouteMeta{Generation: "ab", Writable: 0, WriterEpoch: 6, CatalogRevision: 8, FreezeRunId: run}, false},
	}
	for _, test := range tests {
		if got := targetGateReady(test.target, frozen, run); got != test.want {
			t.Fatalf("%s: %v", test.name, got)
		}
	}
}

func TestRollbackBeforeFlip(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	if _, err := h.source.FreezeWriters(context.Background(), h.run); err != nil {
		t.Fatal(err)
	}
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.source.writable() {
		t.Fatal("要解冻")
	}
	p := h.placement()
	if p.State != dbplacement.StateActive || p.Cluster != testSource || p.Version != 2 || p.Move.RolledBackMs == 0 {
		t.Fatalf("映射: %+v %+v", p, p.Move)
	}
	// 先作废再解冻:反过来的话,解冻之后并发的翻转还能落地。
	h.log.mustPrecede(t, "store.put", "source.release")
	// 重跑幂等。
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if h.placement().Version != 2 {
		t.Fatal("重跑不动版本")
	}
}

func TestRollbackWithoutFreezeIsFine(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	if err := h.m.rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.placement().State != dbplacement.StateActive {
		t.Fatal("状态要回 ACTIVE")
	}
}

func TestRollbackRefusedAfterFlip(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	err := h.m.rollback(context.Background())
	requireRefused(t, err, "other direction")
	if h.log.has("source.release") || h.log.has("store.put") {
		t.Fatalf("翻转之后 rollback 什么都不能动: %v", h.log.events)
	}
}

func TestRollbackRefusesForeignRun(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.runID = testOtherRun
	requireRefused(t, h.m.rollback(context.Background()), "no move with run id")
}

func TestRollbackRefusesWrongSourceAddress(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.m.o.sourceAddr = "10.9.9.9:4000"
	requireRefused(t, h.m.rollback(context.Background()), "the DSN points at")
	if h.log.has("store.put") || h.log.has("source.release") {
		t.Fatalf("连接串指错时什么都不能动: %v", h.log.events)
	}
}

func TestRollbackCASFailureDoesNotUnfreeze(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	h.store.beforePut = func(int64, dbplacement.Placement) error { return errEtcdDown }
	err := h.m.rollback(context.Background())
	if !errors.Is(err, errUnknown) {
		t.Fatalf("结局未知: %v", err)
	}
	if h.log.has("source.release") {
		t.Fatal("映射没作废不能解冻")
	}
}

func TestAuditSuccessRecordsAndUsesTargetPoint(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.metrics["game:9101"] = 3
	if err := h.m.audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.placement().Move.AuditedMs == 0 {
		t.Fatal("要记下成功")
	}
	if h.targetView.snapshot != "target-point" || h.sourceView.snapshot != "" {
		t.Fatalf("目标读翻转之前的位置,源库读当前: %q %q", h.targetView.snapshot, h.sourceView.snapshot)
	}
}

func TestAuditProblemsExitNonZero(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"表内容不一致", func(h *harness) {
			h.metrics["game:9101"] = 3
			h.targetView.tables = map[string]tableDigest{"guild": {Columns: "id", Rows: 3, Digest: "zzz"}}
		}, "guild"},
		{"进程版本落后", func(h *harness) { h.metrics["game:9101"] = 2 }, "at version 2"},
		{"进程没上报", func(h *harness) {}, "reports no db_placement_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedFlipped(false)
			test.change(h)
			err := h.m.audit(context.Background())
			if !errors.Is(err, errMismatch) || exitCodeOf(err) != exitRefused || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("应报告不一致 %q: %v", test.want, err)
			}
			if h.placement().Move.AuditedMs != 0 {
				t.Fatal("不一致不能记成功")
			}
		})
	}
}

func TestAuditRefusedBeforeFlip(t *testing.T) {
	h := newHarness(t)
	h.seedMoving()
	requireRefused(t, h.m.audit(context.Background()), "audit runs after cutover")
}

func TestAuditReplicaAtWrongPointFails(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.metrics["game:9101"] = 3
	h.targetView.pointErr = refusedf("the audit replica is not at the recorded target point")
	requireRefused(t, h.m.audit(context.Background()), "recorded target point")
}

func TestCleanupGuards(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
		want   string
	}{
		{"没翻转", func(h *harness) { h.seedMoving() }, "has not flipped"},
		{"保留期内", func(h *harness) {
			h.seedFlipped(true)
			h.m.o.retention = 7 * 24 * time.Hour
			h.m.o.confirmDrop = true
		}, "retention"},
		{"没有 audit", func(h *harness) {
			h.seedFlipped(false)
			h.m.o.confirmDrop = true
		}, "no successful audit"},
		{"没有 confirm", func(h *harness) { h.seedFlipped(true) }, "-confirm-drop"},
		{"闸门不是这个 run 冻结的", func(h *harness) {
			h.seedFlipped(true)
			h.m.o.confirmDrop = true
			h.sourceAdmin.frozen = false
		}, "not frozen by run"},
		{"连接串指错集群", func(h *harness) {
			h.seedFlipped(true)
			h.m.o.confirmDrop = true
			h.m.o.sourceAddr = "10.9.9.9:4000"
		}, "the DSN points at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			test.change(h)
			requireRefused(t, h.m.cleanup(context.Background()), test.want)
			if h.log.has("admin.drop") {
				t.Fatal("拒绝时不能删库")
			}
		})
	}
}

func TestCleanupDropsSourceAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(true)
	h.m.o.confirmDrop = true
	if err := h.m.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.sourceAdmin.schemas[testDB] {
		t.Fatal("源库要删")
	}
	if h.placement().Move.CleanedMs == 0 {
		t.Fatal("要记下")
	}
	if err := h.m.cleanup(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
}

// 删完库崩了,映射没记上:重跑时库已经不在,跳过删库,补记。
func TestCleanupRerunAfterDropRecordsOnly(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(true)
	h.m.o.confirmDrop = true
	h.store.beforePut = func(int64, dbplacement.Placement) error {
		h.store.beforePut = nil
		return errEtcdDown
	}
	if err := h.m.cleanup(context.Background()); err == nil {
		t.Fatal("记不上要报错")
	}
	h.sourceAdmin.frozen = false // 库不在了,这个判定不该再走到
	if err := h.m.cleanup(context.Background()); err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if h.placement().Move.CleanedMs == 0 {
		t.Fatal("要补记")
	}
}

func TestCleanupAcknowledgeNoAudit(t *testing.T) {
	h := newHarness(t)
	h.seedFlipped(false)
	h.m.o.confirmDrop = true
	h.m.o.acknowledgeNoAudit = true
	if err := h.m.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 走一遍整条路。
func TestFullMoveLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.m.plan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.m.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.m.cutover(ctx); !errors.Is(err, errRefused) {
		t.Fatalf("没 verify 不能切: %v", err)
	}
	if err := h.m.verify(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.m.cutover(ctx); err != nil {
		t.Fatal(err)
	}
	if p := h.placement(); p.Cluster != testTarget || p.Version != 1 || p.State != dbplacement.StateActive {
		t.Fatalf("翻转之后: %+v", p)
	}
	h.metrics["game:9101"] = 1
	if err := h.m.audit(ctx); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(48 * time.Hour)
	h.m.o.confirmDrop = true
	if err := h.m.cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	// 反向再搬一次:从新的集群出发,版本接着涨。
	h.m.o.runID = testOtherRun
	h.m.o.targetCluster = testSource
	h.targetAdmin.schemas = map[string]bool{}
	if err := h.m.prepare(ctx); err != nil {
		t.Fatalf("反向 prepare: %v", err)
	}
	if p := h.placement(); p.Cluster != testTarget || p.Version != 1 || p.Move.SourceCluster != testTarget || p.Move.TargetCluster != testSource {
		t.Fatalf("反向 prepare 之后: %+v %+v", p, p.Move)
	}
}

func TestExitCodes(t *testing.T) {
	for _, test := range []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{refusedf("x"), exitRefused},
		{errMismatch, exitRefused},
		{errors.New("boom"), exitFailed},
		{unknownf("x"), exitUnknown},
	} {
		if got := exitCodeOf(test.err); got != test.want {
			t.Fatalf("%v: %d", test.err, got)
		}
	}
}
