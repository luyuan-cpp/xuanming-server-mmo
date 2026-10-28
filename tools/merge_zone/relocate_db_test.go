package main

// -route-source=db 的搬库接缝 sqlRelocatePlacements(relocate_run.go,规格 §6.3 W11、§13.3 第 2 条)的单测。
//
// 库一侧是内存替身 memRouteDB:RelocateCAS 按 route_store.go relocateCASOne 的同一判定顺序(记录 → home → 围栏 → 写),
// 同一批全成或全不成(事务语义),稳定态 version 不许回退(P-2)。Redis 一侧用真 *routePublisher 挂 fakeRouteRedis
// (route_publish_test.go 的钩子),由 memRouteCache 按发布脚本的 set-if-newer 语义应答 MGET / EVAL / WAITAOF:
// 发布层(catch-up、WAITAOF 份数、整批重发)走的是生产代码,不是替身。不连库、不拨号、不睡(退避与时钟都注入)。
// 真库 + 真 Redis 的端到端在 relocate_integration_db_test.go。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── 库替身 ────────────────────────────────────────────────────────────────────

// memRouteDB 是 player_route / zone_merge_fence 的内存替身,同时满足 relocateRouteStore 与 routeDBReader。
type memRouteDB struct {
	rows   map[uint64]mergeRouteRow
	fenced map[uint32]bool            // RelocateCAS 的围栏(63 号片生效)
	fences map[uint32]routeFenceState // ReadFence 的应答
	// casSizes 记下每次 RelocateCAS 的批大小;casErr 非 nil 时 RelocateCAS 一条都不写、返回它。
	casSizes []int
	casErr   error
	readErr  error
	// writableErr 是 RequireWritable 的应答(nil = 可写);writableChecks 记下它被调了几次。
	writableErr    error
	writableChecks int
	// afterCAS 可选:每次 RelocateCAS 提交之后调用(模拟「提交之后、发布之前」发生的事)。
	afterCAS func()
}

func newMemRouteDB(rows ...mergeRouteRow) *memRouteDB {
	d := &memRouteDB{rows: map[uint64]mergeRouteRow{}, fenced: map[uint32]bool{}, fences: map[uint32]routeFenceState{}}
	for _, r := range rows {
		d.rows[r.PlayerID] = r
	}
	return d
}

func (d *memRouteDB) ReadRoutes(_ context.Context, ids []uint64) (map[uint64]mergeRouteRow, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	out := make(map[uint64]mergeRouteRow, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, fmt.Errorf("%w: player_id 0", errRouteInvalidArgument)
		}
		if r, ok := d.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

func (d *memRouteDB) RelocateCAS(_ context.Context, ops []placementCAS) ([]routeCASResult, error) {
	d.casSizes = append(d.casSizes, len(ops))
	if d.casErr != nil {
		return nil, d.casErr
	}
	if len(ops) > routeWriteBatchLimit {
		return nil, fmt.Errorf("%w: %d ops in one batch", errRouteInvalidArgument, len(ops))
	}
	next := maps.Clone(d.rows)
	seen := map[uint64]bool{}
	out := make([]routeCASResult, len(ops))
	for i, op := range ops {
		if err := op.validate(); err != nil {
			return nil, err
		}
		if seen[op.PlayerID] {
			return nil, fmt.Errorf("%w: player %d twice", errRouteInvalidArgument, op.PlayerID)
		}
		seen[op.PlayerID] = true
		newRec, err := parsePlacement(op.NewValue)
		if err != nil {
			return nil, err
		}
		row, present := next[op.PlayerID]
		cur, curPresent, err := row.placement()
		if err != nil {
			return nil, err
		}
		curRaw := ""
		if curPresent {
			curRaw = cur.Encode()
		}
		res := routeCASResult{Row: row, RowPresent: present}
		res.CurrentRecord, res.CurrentHome = curRaw, row.homeValue()
		switch {
		case op.ExpectAbsent && curPresent, !op.ExpectAbsent && curRaw != op.ExpectRecord:
			res.Outcome = placementCASRecordChanged
		case op.ExpectHome != "" && row.homeValue() != op.ExpectHome:
			res.Outcome = placementCASHomeChanged
		case op.FenceZone != 0 && d.fenced[op.FenceZone]:
			res.Outcome = placementCASFenced
		default:
			if !present {
				return nil, fmt.Errorf("%w: player %d has no row", errRouteInvariant, op.PlayerID)
			}
			if !newRec.Frozen && curPresent && newRec.Version < cur.Version {
				return nil, fmt.Errorf("%w: player %d P-2", errRouteInvariant, op.PlayerID)
			}
			w := row.withPlacement(newRec)
			w.Rev++
			next[op.PlayerID] = w
			res.Outcome, res.Row = placementCASWritten, w
		}
		out[i] = res
	}
	d.rows = next
	if d.afterCAS != nil {
		d.afterCAS()
	}
	return out, nil
}

// scan 按聚簇键序返回满足 match、键大于 after 的至多 limit 行(键集翻页的替身)。
func (d *memRouteDB) scan(after routeScanCursor, limit int, match func(mergeRouteRow) bool) []mergeRouteRow {
	var all []mergeRouteRow
	for _, r := range d.rows {
		if match(r) && after.less(routeCursorOf(r.PlayerID)) {
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool { return routeCursorOf(all[i].PlayerID).less(routeCursorOf(all[j].PlayerID)) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (d *memRouteDB) ScanByHome(_ context.Context, zone uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	return d.scan(after, limit, func(r mergeRouteRow) bool { return r.HomeZone == zone }), nil
}

func (d *memRouteDB) ScanByStorage(_ context.Context, storage uint32, after routeScanCursor, limit int) ([]mergeRouteRow, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	return d.scan(after, limit, func(r mergeRouteRow) bool { return r.StorageID == storage }), nil
}

func (d *memRouteDB) ReadFence(_ context.Context, zone uint32) (routeFenceState, error) {
	return d.fences[zone], nil
}

func (d *memRouteDB) RequireWritable(context.Context) error {
	d.writableChecks++
	return d.writableErr
}

// ── mapping Redis 替身(挂在 fakeRouteRedis 上)───────────────────────────────

// memRouteCache 按 routePublishScript 的语义应答:MGET 读 kv;EVAL set-if-newer(缓存 rev ≥ 本次回 {0,…},无 maprev 而遗留值
// 不同回 {2,…},否则写三键回 {1, rev});WAITAOF 回 acks。down=true 时每条命令都是网络错误(什么都没写)。
type memRouteCache struct {
	kv   map[string]string
	acks []any
	down bool
}

func newMemRouteCache() *memRouteCache {
	return &memRouteCache{kv: map[string]string{}, acks: []any{int64(1), int64(0)}}
}

func (c *memRouteCache) reply(_ int, cmd redis.Cmder) (any, error) {
	if c.down {
		return nil, errors.New("fake mapping redis: connection refused")
	}
	switch cmd.Name() {
	case "mget":
		var out []any
		for _, k := range cmd.Args()[1:] {
			if v, ok := c.kv[k.(string)]; ok {
				out = append(out, v)
			} else {
				out = append(out, nil)
			}
		}
		return out, nil
	case "eval":
		return c.publish(cmd), nil
	case "waitaof":
		return c.acks, nil
	}
	return nil, fmt.Errorf("fake mapping redis: unexpected %s", cmd.Name())
}

func (c *memRouteCache) publish(cmd redis.Cmder) any {
	a := cmd.Args()
	zk, pk, rk := a[3].(string), a[4].(string), a[5].(string)
	rev, home, plc := evalArgs(cmd)
	cur, hasCur := c.kv[rk]
	curHome, hasHome := c.kv[zk]
	curPlc, hasPlc := c.kv[pk]
	if hasCur {
		if cmp, err := compareRouteRev(cur, rev); err == nil && cmp >= 0 {
			return []any{int64(0), cur, curHome, curPlc}
		}
	} else if (hasHome && curHome != home) || (hasPlc && curPlc != plc) {
		return []any{int64(2), "", curHome, curPlc}
	}
	for k, v := range map[string]string{zk: home, pk: plc} {
		if v == "" {
			delete(c.kv, k)
		} else {
			c.kv[k] = v
		}
	}
	c.kv[rk] = rev
	return []any{int64(1), rev}
}

// cached 返回一名玩家缓存三键的值(不存在写 "-"),断言用。
func (c *memRouteCache) cached(id uint64) string {
	get := func(k string) string {
		if v, ok := c.kv[k]; ok {
			return v
		}
		return "-"
	}
	return get(playerZoneKey(id)) + "|" + get(placementKey(id)) + "|" + get(mapRevKey(id))
}

// seed 把一行按库值写进缓存(模拟它早先已被发布并确认过)。
func (c *memRouteCache) seed(row mergeRouteRow) {
	if h := row.homeValue(); h != "" {
		c.kv[playerZoneKey(row.PlayerID)] = h
	}
	if p, _ := row.placementValue(); p != "" {
		c.kv[placementKey(row.PlayerID)] = p
	}
	c.kv[mapRevKey(row.PlayerID)] = formatRouteRev(row.Rev)
}

// newMemRelocatePlacements 把库替身与缓存替身接成 sqlRelocatePlacements(发布层是生产的 routePublisher)。
func newMemRelocatePlacements(t *testing.T, db *memRouteDB, cache *memRouteCache) (sqlRelocatePlacements, *fakeRouteRedis) {
	t.Helper()
	rdb, f := newFakeRouteRedis(t, cache.reply)
	return sqlRelocatePlacements{store: db, pub: newTestRoutePublisher(rdb, 1, 0)}, f
}

// 编译期确认接缝:生产实现与替身都满足接口(接口一改,这里先报错)。
var (
	_ relocatePlacements     = sqlRelocatePlacements{}
	_ relocateRouteStore     = (*mergeRouteStore)(nil)
	_ relocateRouteStore     = (*memRouteDB)(nil)
	_ relocateRoutePublisher = (*routePublisher)(nil)
	_ routeDBReader          = (*mergeRouteStore)(nil)
	_ routeDBReader          = (*memRouteDB)(nil)
	_ routeReadSide          = routeReadSideDB{}
	_ routeReadSide          = routeReadSideRedis{}
)

// 落在 S=901 上的几名玩家(relocate_test.go 的 relocTestS / relocTestT / relocTestRun)。
func relocDBHomeRow(id uint64, rev uint64) mergeRouteRow {
	return mergeRouteRow{PlayerID: id, HomeZone: relocTestS, Rev: rev}
}

// ── read ──────────────────────────────────────────────────────────────────────

// TestSQLRelocateRead_PublishesWhenTheDBIsAheadBeforeReturning:库 rev 领先缓存(或缓存没有 maprev)的行,read 先按库值
// 发布 + WAITAOF,再把与 Redis 同编码的值串交出去;缓存已是这一版的行不发。
func TestSQLRelocateRead_PublishesWhenTheDBIsAheadBeforeReturning(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	frozen := mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 3, FrozenRunID: relocTestRun, Rev: 5}
	current := mergeRouteRow{PlayerID: 2, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, Rev: 2}
	homeOnly := relocDBHomeRow(3, 1)
	db := newMemRouteDB(frozen, current, homeOnly)
	cache := newMemRouteCache()
	cache.seed(mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 3, Rev: 4}) // 落后一版:还没冻结
	cache.seed(current)
	p, f := newMemRelocatePlacements(t, db, cache)

	reads, err := p.read(ctx, []uint64{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := reads[0].RecordRaw; got != frozenValue(relocTestS, 3, relocTestRun) || !reads[0].Record.Frozen {
		t.Fatalf("player 1 record = %q (%+v), want the DB's frozen value", got, reads[0].Record)
	}
	if !reads[2].HomePresent || reads[2].Home != relocTestS || reads[2].RecordPresent {
		t.Fatalf("player 3 = %+v, want home 901 and no record", reads[2])
	}
	if reads[3].RecordPresent || reads[3].HomePresent {
		t.Fatalf("player 4 has no route row: %+v, want both keys absent", reads[3])
	}
	if got := cache.cached(1); got != "901|901:3:frozen:"+relocTestRun+"|5" {
		t.Fatalf("player 1 cache after read = %s, want the frozen value published at rev 5", got)
	}
	if got := cache.cached(3); got != "901|-|1" {
		t.Fatalf("player 3 cache after read = %s, want home published at rev 1", got)
	}
	// 两条 pipeline:先 catch-up(没有 maprev 的 3;已是这一版的 2 不发),再确认冻结行 1;每条末尾一个 WAITAOF。
	if f.pipelineCount() != 2 || !reflect.DeepEqual(f.names(0), []string{"eval", "waitaof"}) ||
		!reflect.DeepEqual(f.names(1), []string{"eval", "waitaof"}) {
		t.Fatalf("publish pipelines = %d, want [eval waitaof] twice", f.pipelineCount())
	}
	if got := f.pipelines[0][0].Args()[3]; got != playerZoneKey(3) {
		t.Fatalf("catch-up published %v first, want player 3", got)
	}
	if got := f.pipelines[1][0].Args()[3]; got != playerZoneKey(1) {
		t.Fatalf("the frozen-row confirmation published %v, want player 1", got)
	}
}

// TestSQLRelocateRead_FrozenRowIsConfirmedEvenWhenTheCacheIsCurrent:缓存已是库里这一版的冻结值(上一次 EVAL 执行了、
// WAITAOF 没确认),read 仍发布 + WAITAOF;确认不了即 error —— R2 之前必须证明冻结已落盘,不能信「它已经在那里」。
func TestSQLRelocateRead_FrozenRowIsConfirmedEvenWhenTheCacheIsCurrent(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	row := mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, FrozenRunID: relocTestRun, Rev: 2}
	cache := newMemRouteCache()
	cache.seed(row)
	cache.acks = []any{int64(0), int64(0)}
	p, f := newMemRelocatePlacements(t, newMemRouteDB(row), cache)
	if _, err := p.read(ctx, []uint64{1}); !errors.Is(err, errRoutePublishNotDurable) {
		t.Fatalf("read = %v, want errRoutePublishNotDurable (the current frozen value must still be confirmed)", err)
	}
	if f.pipelineCount() == 0 || f.names(0)[len(f.names(0))-1] != "waitaof" {
		t.Fatal("the frozen row was not re-published with a WAITAOF")
	}
	cache.acks = []any{int64(1), int64(0)}
	if reads, err := p.read(ctx, []uint64{1}); err != nil || !reads[0].Record.Frozen {
		t.Fatalf("read once WAITAOF acks = (%+v, %v)", reads, err)
	}
}

// TestSQLRelocateRead_UnconfirmedPublishIsAnError:发布确认不了(WAITAOF 份数不足 / 连不上)或缓存与库冲突时,read 一个结果都不给。
func TestSQLRelocateRead_UnconfirmedPublishIsAnError(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	row := mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, FrozenRunID: relocTestRun, Rev: 2}

	cache := newMemRouteCache()
	cache.acks = []any{int64(0), int64(0)}
	p, _ := newMemRelocatePlacements(t, newMemRouteDB(row), cache)
	if reads, err := p.read(ctx, []uint64{1}); !errors.Is(err, errRoutePublishNotDurable) || reads != nil {
		t.Fatalf("WAITAOF never acked: (%v, %v), want (nil, errRoutePublishNotDurable)", reads, err)
	}

	cache = newMemRouteCache()
	cache.down = true
	p, _ = newMemRelocatePlacements(t, newMemRouteDB(row), cache)
	if reads, err := p.read(ctx, []uint64{1}); err == nil || reads != nil {
		t.Fatalf("mapping Redis down: (%v, %v), want an error and no result", reads, err)
	}

	// 缓存比库新、值不同(库回档的征兆):发布被拒,不把库值交给判定。
	cache = newMemRouteCache()
	cache.seed(mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestT, PlacementVersion: 2, Rev: 7})
	p, _ = newMemRelocatePlacements(t, newMemRouteDB(row), cache)
	if _, err := p.read(ctx, []uint64{1}); !errors.Is(err, errRoutePublishRejected) {
		t.Fatalf("cache ahead of the DB with another value: %v, want errRoutePublishRejected", err)
	}
}

// TestSQLRelocateRead_MalformedRowIsNotPublished:落点三列不自洽的行交给判定(RecordErr,decideFreeze 跳过),绝不发布。
func TestSQLRelocateRead_MalformedRowIsNotPublished(t *testing.T) {
	bad := mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, FrozenRunID: "bad run", Rev: 3}
	cache := newMemRouteCache()
	p, f := newMemRelocatePlacements(t, newMemRouteDB(bad), cache)
	reads, err := p.read(context.Background(), []uint64{1})
	if err != nil || len(reads) != 1 || reads[0].RecordErr == nil {
		t.Fatalf("read = (%+v, %v), want one malformed record", reads, err)
	}
	if d := decideFreeze(relocatePlayer{PlayerID: 1}, reads[0], relocTestS, relocTestT, relocTestRun, map[uint32]bool{relocTestS: true}); d.kind != freezeSkip {
		t.Fatalf("a malformed DB row must be skipped: %+v", d)
	}
	if f.pipelineCount() != 0 || len(cache.kv) != 0 {
		t.Fatalf("a malformed row was published: %d pipelines, cache %v", f.pipelineCount(), cache.kv)
	}
}

// TestSQLRelocateRead_ReadOnlyDoesNotPublish:dry-run 只读库,不碰 mapping Redis;cas 拒绝。
func TestSQLRelocateRead_ReadOnlyDoesNotPublish(t *testing.T) {
	cache := newMemRouteCache()
	p, f := newMemRelocatePlacements(t, newMemRouteDB(relocDBHomeRow(1, 1)), cache)
	p.readOnly = true
	reads, err := p.read(context.Background(), []uint64{1})
	if err != nil || !reads[0].HomePresent {
		t.Fatalf("read-only read = (%+v, %v)", reads, err)
	}
	if f.pipelineCount() != 0 || len(f.singles) != 0 {
		t.Fatalf("a dry-run read touched the mapping Redis: %d pipelines, %d commands", f.pipelineCount(), len(f.singles))
	}
	op := placementCAS{PlayerID: 1, ExpectAbsent: true, ExpectHome: "901", NewValue: frozenValue(relocTestS, 1, relocTestRun)}
	if _, err := p.cas(context.Background(), []placementCAS{op}); err == nil {
		t.Fatal("a read-only backend must refuse CAS")
	}
}

// TestSQLRelocateRead_FrozenWritersRefuseToPublish(R8-5):route_meta.writable != 1(回档恢复中)时 read 不发布、一个结果都不给,
// 错误包着 errRouteNotWritable 并指向 runbook §9.4;读不到开关同样不发布、原因原样带出。没有可发布的行、dry-run 不查开关。
func TestSQLRelocateRead_FrozenWritersRefuseToPublish(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	db := newMemRouteDB(relocDBHomeRow(1, 1))
	db.writableErr = fmt.Errorf("%w (writable=0)", errRouteNotWritable)
	cache := newMemRouteCache()
	p, f := newMemRelocatePlacements(t, db, cache)

	reads, err := p.read(context.Background(), []uint64{1})
	if !errors.Is(err, errRouteNotWritable) || reads != nil || !strings.Contains(err.Error(), "runbook §9.4") {
		t.Fatalf("frozen writers: (%v, %v), want (nil, errRouteNotWritable naming runbook §9.4)", reads, err)
	}
	if f.pipelineCount() != 0 || len(f.singles) != 0 || len(cache.kv) != 0 {
		t.Fatalf("a frozen route DB was published: %d pipelines, %d commands, cache %v", f.pipelineCount(), len(f.singles), cache.kv)
	}

	db.writableErr = errors.New("read route_meta.writable: connection refused")
	if _, err := p.read(context.Background(), []uint64{1}); err == nil || errors.Is(err, errRouteNotWritable) ||
		!strings.Contains(err.Error(), "connection refused") || f.pipelineCount() != 0 {
		t.Fatalf("unreadable switch = %v, want the cause and no publish", err)
	}

	// 没有可发布的行(库里没有这名玩家)与 dry-run:不查开关,照常读。
	db.writableChecks = 0
	if reads, err := p.read(context.Background(), []uint64{2}); err != nil || reads[0].HomePresent || reads[0].RecordPresent {
		t.Fatalf("nothing to publish = (%+v, %v), want a plain absent read", reads, err)
	}
	p.readOnly = true
	if reads, err := p.read(context.Background(), []uint64{1}); err != nil || !reads[0].HomePresent {
		t.Fatalf("dry-run read while frozen = (%+v, %v)", reads, err)
	}
	if db.writableChecks != 0 {
		t.Fatalf("writable was checked %d times without anything to publish", db.writableChecks)
	}

	// 解冻之后照常先发布再交出结果。
	p.readOnly = false
	db.writableErr = nil
	if _, err := p.read(ctx, []uint64{1}); err != nil || cache.cached(1) != "901|-|1" {
		t.Fatalf("after unfreeze: err=%v cache=%s, want home published at rev 1", err, cache.cached(1))
	}
	if db.writableChecks != 1 {
		t.Fatalf("writable checks = %d, want 1 (once, before the publish)", db.writableChecks)
	}
}

// TestSQLRelocate_PublishRejectionNamesTheRollbackRunbook(R8-5 文案):发布被确定性拒绝时,read 与 cas 的错误都带上回档提示
// (指向 runbook §9.4);WAITAOF 确认不了之类的非拒绝错误不带。
func TestSQLRelocate_PublishRejectionNamesTheRollbackRunbook(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	if got := routePublishRollbackHint(fmt.Errorf("%w: conflict", errRoutePublishRejected)); !strings.Contains(got, "runbook §9.4") {
		t.Fatalf("hint for a rejection = %q, want the rollback runbook", got)
	}
	if got := routePublishRollbackHint(errRoutePublishNotDurable); got != "" {
		t.Fatalf("hint for a non-rejection = %q, want none", got)
	}

	// read:缓存比库新、值不同,冻结行的确认发布被拒。
	row := mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, FrozenRunID: relocTestRun, Rev: 2}
	cache := newMemRouteCache()
	cache.seed(mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestT, PlacementVersion: 2, Rev: 7})
	p, _ := newMemRelocatePlacements(t, newMemRouteDB(row), cache)
	if _, err := p.read(ctx, []uint64{1}); !errors.Is(err, errRoutePublishRejected) || !strings.Contains(err.Error(), "runbook §9.4") {
		t.Fatalf("rejected read = %v, want errRoutePublishRejected with the rollback hint", err)
	}

	// cas:提交之后的发布被拒(缓存 rev 领先、值不同)。
	db := newMemRouteDB(relocDBHomeRow(2, 1))
	cache = newMemRouteCache()
	cache.seed(mergeRouteRow{PlayerID: 2, HomeZone: relocTestS, StorageID: relocTestT, PlacementVersion: 2, Rev: 9})
	p, _ = newMemRelocatePlacements(t, db, cache)
	op := placementCAS{PlayerID: 2, ExpectAbsent: true, ExpectHome: "901", NewValue: frozenValue(relocTestS, 1, relocTestRun)}
	if _, err := p.cas(ctx, []placementCAS{op}); !errors.Is(err, errRoutePublishRejected) || !strings.Contains(err.Error(), "runbook §9.4") {
		t.Fatalf("rejected cas publish = %v, want errRoutePublishRejected with the rollback hint", err)
	}

	// 非拒绝(WAITAOF 份数不足):不带提示。
	cache = newMemRouteCache()
	cache.acks = []any{int64(0), int64(0)}
	p, _ = newMemRelocatePlacements(t, newMemRouteDB(relocDBHomeRow(3, 1)), cache)
	if _, err := p.read(ctx, []uint64{3}); !errors.Is(err, errRoutePublishNotDurable) || strings.Contains(err.Error(), "runbook §9.4") {
		t.Fatalf("not durable read = %v, want errRoutePublishNotDurable without the rollback hint", err)
	}
}

// ── cas ───────────────────────────────────────────────────────────────────────

// TestSQLRelocateCAS_OnlyConfirmedWritesAreWritten:提交后发布确认了才回 Written;确认不了时返回「本批第一个写成的 op 之前」的
// 前缀(那些结局是确定的)+ error,库里已提交的行由下一次 read 补发布。
func TestSQLRelocateCAS_OnlyConfirmedWritesAreWritten(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	db := newMemRouteDB(
		mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: 902, PlacementVersion: 1, Rev: 4}, // 有效落点不是 S
		relocDBHomeRow(2, 1),
		relocDBHomeRow(3, 1),
	)
	cache := newMemRouteCache()
	cache.acks = []any{int64(0), int64(0)} // WAITAOF 永远确认不了
	p, _ := newMemRelocatePlacements(t, db, cache)
	freezeOp := func(id uint64) placementCAS {
		return placementCAS{PlayerID: id, ExpectAbsent: true, ExpectHome: "901", FenceZone: relocTestS, NewValue: frozenValue(relocTestS, 1, relocTestRun)}
	}
	ops := []placementCAS{freezeOp(1), freezeOp(2), freezeOp(3)}

	results, err := p.cas(ctx, ops)
	if !errors.Is(err, errRoutePublishNotDurable) {
		t.Fatalf("cas with an unconfirmed publish: %v, want errRoutePublishNotDurable", err)
	}
	if len(results) != 1 || results[0].Outcome != placementCASRecordChanged || results[0].CurrentRecord != "902:1" {
		t.Fatalf("prefix = %+v, want only player 1's record_changed (the first write is unconfirmed)", results)
	}
	for _, id := range []uint64{2, 3} {
		if r := db.rows[id]; r.FrozenRunID != relocTestRun || r.Rev != 2 {
			t.Fatalf("player %d row = %+v, want the freeze committed in the DB (rev 2)", id, r)
		}
	}

	// 发布恢复之后:同一个 read 先把已提交的冻结补发布,再交出冻结值(续跑据此认领)。
	cache.acks = []any{int64(1), int64(0)}
	reads, err := p.read(ctx, []uint64{2, 3})
	if err != nil || reads[0].RecordRaw != frozenValue(relocTestS, 1, relocTestRun) {
		t.Fatalf("read after recovery = (%+v, %v)", reads, err)
	}
	if got := cache.cached(2); got != "901|901:1:frozen:"+relocTestRun+"|2" {
		t.Fatalf("player 2 cache = %s, want the committed freeze published", got)
	}

	// 确认得了:全部结局原样交出,写成的行已在缓存。
	unfreeze := placementCAS{PlayerID: 2, ExpectRecord: frozenValue(relocTestS, 1, relocTestRun), NewValue: stableValue(relocTestS, 1)}
	results, err = p.cas(ctx, []placementCAS{unfreeze})
	if err != nil || len(results) != 1 || results[0].Outcome != placementCASWritten {
		t.Fatalf("confirmed cas = (%+v, %v)", results, err)
	}
	if got := cache.cached(2); got != "901|901:1|3" {
		t.Fatalf("player 2 cache after the unfreeze = %s, want 901:1 at rev 3", got)
	}
}

// TestSQLRelocateCAS_DBRefusalWritesNothingAndPassesThePrefix:库拒绝(写者冻结、P-2、锁冲突用尽)时这一批一条都没提交,
// 返回此前各批已确认的结果 + error;不发布任何东西。
func TestSQLRelocateCAS_DBRefusalWritesNothingAndPassesThePrefix(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	var rows []mergeRouteRow
	var ops []placementCAS
	for id := uint64(1); id <= routeWriteBatchLimit+1; id++ {
		rows = append(rows, relocDBHomeRow(id, 1))
		ops = append(ops, placementCAS{PlayerID: id, ExpectAbsent: true, ExpectHome: "901", NewValue: frozenValue(relocTestS, 1, relocTestRun)})
	}
	db := newMemRouteDB(rows...)
	cache := newMemRouteCache()
	p, _ := newMemRelocatePlacements(t, db, cache)
	calls := 0
	db.afterCAS = func() {
		calls++
		if calls == 1 {
			db.casErr = errRouteNotWritable // 第二批之前写者被冻结
		}
	}
	results, err := p.cas(ctx, ops)
	if !errors.Is(err, errRouteNotWritable) {
		t.Fatalf("cas = %v, want errRouteNotWritable from the second batch", err)
	}
	if !reflect.DeepEqual(db.casSizes, []int{routeWriteBatchLimit, 1}) {
		t.Fatalf("batches = %v, want one transaction per %d ops", db.casSizes, routeWriteBatchLimit)
	}
	if len(results) != routeWriteBatchLimit || results[0].Outcome != placementCASWritten {
		t.Fatalf("prefix = %d results, want the %d confirmed results of the first batch", len(results), routeWriteBatchLimit)
	}
	if r := db.rows[routeWriteBatchLimit+1]; r.FrozenRunID != "" {
		t.Fatalf("the refused batch wrote player %d: %+v", r.PlayerID, r)
	}
	if got := cache.cached(routeWriteBatchLimit + 1); got != "-|-|-" {
		t.Fatalf("the refused batch was published: %s", got)
	}
}

// ── 引擎 + 库接缝 ─────────────────────────────────────────────────────────────

// relocDBEngine 用 sqlRelocatePlacements 装配状态机(锁 / 行用 relocate_test.go 的替身,时钟不走墙钟)。
func relocDBEngine(m *relocateManifest, places relocatePlacements, rows *fakeRows) *relocateEngine {
	clock := &fakeClock{t: time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)}
	e := newRelocateEngine(m, func(*relocateManifest) error { return nil }, places, &fakeLocks{held: map[string]int{}}, rows,
		map[uint32]bool{relocTestS: true, 902: true}, 100, time.Second)
	e.lockPoll = 100 * time.Millisecond
	e.sleep, e.now = clock.sleep, clock.now
	return e
}

// TestRelocateEngineDB_SwitchesThroughTheDB:R1–R5 经库接缝跑通:库里落点三列与 rev、缓存三键都到 "{T}:{v+1}"。
func TestRelocateEngineDB_SwitchesThroughTheDB(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	db := newMemRouteDB(relocDBHomeRow(1, 1),
		mergeRouteRow{PlayerID: 2, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 3, Rev: 4})
	cache := newMemRouteCache()
	cache.seed(db.rows[1])
	cache.seed(db.rows[2])
	p, _ := newMemRelocatePlacements(t, db, cache)
	m := relocTestManifest(1, 2)
	if err := relocDBEngine(m, p, newFakeRows()).run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := relocStates(m); got != "1:switched 2:switched" {
		t.Fatalf("states = %s", got)
	}
	for id, want := range map[uint64]string{1: "901|1000000:2|3", 2: "901|1000000:4|6"} {
		if got := cache.cached(id); got != want {
			t.Errorf("player %d cache = %s, want %s (freeze and switch each bump rev once)", id, got, want)
		}
		if r := db.rows[id]; r.StorageID != relocTestT || r.FrozenRunID != "" {
			t.Errorf("player %d DB row = %+v, want stable on %d", id, r, relocTestT)
		}
	}
}

// TestRelocateResumeAfterRedisLossClaimsFromSQL:R1 冻结已提交并发布,随后 mapping Redis 整体丢失、清单也没来得及记下冻结;
// 续跑按库里的 frozen_run_id 认出本次冻结的人,先把冻结补发布,再走 R2–R5。
func TestRelocateResumeAfterRedisLossClaimsFromSQL(t *testing.T) {
	db := newMemRouteDB(relocDBHomeRow(1, 1), relocDBHomeRow(2, 1))
	cache := newMemRouteCache()
	p, _ := newMemRelocatePlacements(t, db, cache)
	ctx, _ := testMaintenanceContext()

	first := relocTestManifest(1, 2)
	if err := relocDBEngine(first, p, newFakeRows()).freeze(ctx, []int{0, 1}); err != nil {
		t.Fatalf("R1: %v", err)
	}
	if got := cache.cached(1); got != "901|901:1:frozen:"+relocTestRun+"|2" {
		t.Fatalf("after R1 cache = %s", got)
	}
	cache.kv = map[string]string{} // Redis 整体丢失(FLUSHALL / 空实例重启)

	resumed := relocTestManifest(1, 2) // 清单落后:两人都还是 pending
	rows := newFakeRows()
	rows.afterCopy = func(id uint64) {
		// R3 拷贝时 go/db 必须已经看得到冻结:缓存里是本次的冻结值。
		if got := cache.cached(id); !strings.Contains(got, ":frozen:"+relocTestRun) {
			t.Errorf("player %d copied while the cache showed %s (the freeze was not re-published first)", id, got)
		}
	}
	if err := relocDBEngine(resumed, p, rows).run(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := relocStates(resumed); got != "1:switched 2:switched" {
		t.Fatalf("states = %s, want both adopted from the DB and switched", got)
	}
	if got := relocPlayer(resumed, 1); got.Version != 1 || got.HomeZone != relocTestS || got.Final != "1000000:2" {
		t.Fatalf("player 1 = %+v, want version 1 / home 901 adopted from the DB", got)
	}
	if got := cache.cached(2); got != "901|1000000:2|3" {
		t.Fatalf("player 2 cache = %s, want the switch published", got)
	}
}

// TestRelocateEngineDB_UnpublishedFreezeIsPublishedBeforeR2:R1 冻结已提交、发布没确认(Redis 连不上)—— 引擎拿不到 Written,
// 停在 R1、不进 R2;续跑先把库里的冻结发布确认,才开始等锁与拷贝(关掉「已提交未发布 → 认领 → 直接 R2」的窗口)。
func TestRelocateEngineDB_UnpublishedFreezeIsPublishedBeforeR2(t *testing.T) {
	db := newMemRouteDB(relocDBHomeRow(1, 1))
	cache := newMemRouteCache()
	cache.seed(db.rows[1])
	p, _ := newMemRelocatePlacements(t, db, cache)
	ctx, _ := testMaintenanceContext()
	db.afterCAS = func() { cache.down = true } // 提交之后、发布之前 Redis 断了

	m := relocTestManifest(1)
	rows := newFakeRows()
	if err := relocDBEngine(m, p, rows).run(ctx); err == nil {
		t.Fatal("an unconfirmed freeze must stop the run")
	}
	if got := relocStates(m); got != "1:pending" || len(rows.copied) != 0 {
		t.Fatalf("after the failed R1: states=%s copied=%v, want pending and nothing copied", got, rows.copied)
	}
	if r := db.rows[1]; r.FrozenRunID != relocTestRun {
		t.Fatalf("DB row = %+v, want the freeze committed", r)
	}
	if got := cache.cached(1); got != "901|-|1" {
		t.Fatalf("cache = %s, want still unfrozen (go/db keeps writing S, which is still right)", got)
	}

	db.afterCAS = nil
	cache.down = false
	rows.afterCopy = func(id uint64) {
		if got := cache.cached(id); got != "901|901:1:frozen:"+relocTestRun+"|2" {
			t.Errorf("player %d copied while the cache showed %s", id, got)
		}
	}
	if err := relocDBEngine(m, p, rows).run(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := relocStates(m); got != "1:switched" || cache.cached(1) != "901|1000000:2|3" {
		t.Fatalf("after the resume: states=%s cache=%s", got, cache.cached(1))
	}
}

// TestRelocateAbortDB_UnfreezesThroughTheDB:撤销经库接缝解冻,发布确认之后才计 unfrozen;dry-run 只读库。
func TestRelocateAbortDB_UnfreezesThroughTheDB(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	db := newMemRouteDB(mergeRouteRow{PlayerID: 1, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 3, FrozenRunID: relocTestRun, Rev: 7})
	cache := newMemRouteCache()
	cache.seed(db.rows[1])
	p, f := newMemRelocatePlacements(t, db, cache)
	m := relocTestManifest(1)
	m.Players[0].State, m.Players[0].Version = relocFrozen, 3

	dry := p
	dry.readOnly = true
	if rep, err := relocDBEngine(m, dry, nil).abort(context.Background(), true); err != nil || rep.Unfrozen != 1 {
		t.Fatalf("dry-run abort = (%s, %v)", rep, err)
	}
	if f.pipelineCount() != 0 || db.rows[1].FrozenRunID == "" {
		t.Fatal("a dry-run abort wrote something")
	}
	rep, err := relocDBEngine(m, p, nil).abort(ctx, false)
	if err != nil || rep.Unfrozen != 1 || relocPlayer(m, 1).State != relocUnfrozen {
		t.Fatalf("abort = (%s, %v), state %s", rep, err, relocPlayer(m, 1).State)
	}
	if got := cache.cached(1); got != "901|901:3|8" {
		t.Fatalf("cache after the abort = %s, want 901:3 at rev 8", got)
	}
}

// TestRelocateAbortDB_CacheOnlyFreezeFailsClosed:db 源撤销按库判定。缓存里有本次的冻结值、库里却没有(两份分叉:同 rev、值不同,
// 例如绕过发布脚本的写者)时,read 先按库值发布被拒,这一批一个人都不记、库与缓存都不动 —— 不按缓存那一份去解冻,也不把分叉盖过去。
// (清单来源与本次不一致的撤销在入口就被 relocateRouteSourceRefusal 拒绝,走不到这里,B2-1。)
func TestRelocateAbortDB_CacheOnlyFreezeFailsClosed(t *testing.T) {
	ctx, _ := testMaintenanceContext()
	diverged := mergeRouteRow{PlayerID: 3, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, Rev: 3}
	db := newMemRouteDB(diverged)
	cache := newMemRouteCache()
	cache.seed(mergeRouteRow{PlayerID: 3, HomeZone: relocTestS, StorageID: relocTestS, PlacementVersion: 1, FrozenRunID: relocTestRun, Rev: 3})
	before := cache.cached(3)
	p, _ := newMemRelocatePlacements(t, db, cache)
	m := relocTestManifest(3)
	m.RouteSource = routeSourceDB
	m.Players[0].State, m.Players[0].Version = relocFrozen, 1
	if _, err := relocDBEngine(m, p, nil).abort(ctx, false); !errors.Is(err, errRoutePublishRejected) {
		t.Fatalf("diverged copies: abort = %v, want errRoutePublishRejected (fail closed)", err)
	}
	if got := relocStates(m); got != "3:frozen" {
		t.Fatalf("diverged copies: states = %s, want the player left as frozen (nothing recorded)", got)
	}
	if got := cache.cached(3); got != before || db.rows[3] != diverged || len(db.casSizes) != 0 {
		t.Fatalf("diverged copies: cache %s (was %s), DB %+v, CAS batches %v — want nothing written", got, before, db.rows[3], db.casSizes)
	}
}
