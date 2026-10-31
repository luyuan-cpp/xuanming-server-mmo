package store

import (
	"bytes"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"shared/placement"
)

// 对账 leader 库原语(route_reconcile.go)不连库的单测:每拍判定表、leader 标识、SQL 文本形状、游标序、低基数 label。
// 「库应答 → 结局 / 语句 / 参数」在 route_reconcile_contract_test.go(假驱动);锁、租约谓词与并发只有真库能证,
// 在 route_reconcile_integration_test.go(-tags=integration)。

const (
	decideLeader = "route_leader@unit:00000001"
	decideOther  = "route_leader@other:00000002"
	decideGen    = "0123456789abcdef0123456789abcdef"
	decideNow    = uint64(1_760_000_100_000)
)

// TestDecideReconcileDB 覆盖每拍判定(decideReconcileDB)的每一格:租约、已冻结、世代、水位倒退(容差 0 与 1000)、正常推进、cvm。
func TestDecideReconcileDB(t *testing.T) {
	held := routeReconcileRow{Leader: decideLeader, LeaseUntilMs: decideNow + 5000, WatermarkMs: decideNow - 1000, CacheVerifiedMs: decideNow - 60_000, NowMs: decideNow}
	writable := RouteMeta{Generation: decideGen, Writable: 1}
	key := func(gen string, wm uint64) *placement.RouteWatermark {
		return &placement.RouteWatermark{V: 1, Generation: gen, WatermarkMs: wm}
	}
	u64 := func(v uint64) *uint64 { return &v }
	with := func(mut func(*routeReconcileRow)) routeReconcileRow {
		r := held
		mut(&r)
		return r
	}

	for _, tc := range []struct {
		name      string
		row       routeReconcileRow
		meta      RouteMeta
		key       *placement.RouteWatermark
		tol       uint64
		cvm       *uint64
		want      ReconcileTickOutcome
		reason    RouteFreezeReason
		wantWM    uint64
		wantCvm   uint64
		setCvm    bool
		clampsCvm bool
	}{
		{name: "租约丢失_他人持有", row: with(func(r *routeReconcileRow) { r.Leader = decideOther }), meta: writable,
			want: ReconcileTickLeaseLost, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "租约丢失_已过期", row: with(func(r *routeReconcileRow) { r.LeaseUntilMs = decideNow - 1 }), meta: writable,
			want: ReconcileTickLeaseLost, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "租约丢失_租约为NULL", row: with(func(r *routeReconcileRow) { r.LeaseUntilMs = 0 }), meta: writable,
			want: ReconcileTickLeaseLost, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "租约丢失_无人持有", row: with(func(r *routeReconcileRow) { r.Leader = "" }), meta: writable,
			want: ReconcileTickLeaseLost, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "租约丢失优先于回档证据", row: with(func(r *routeReconcileRow) { r.Leader = decideOther }), meta: writable,
			key: key("ffffffffffffffffffffffffffffffff", decideNow+1), cvm: u64(1),
			want: ReconcileTickLeaseLost, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "租约恰好到期仍持有", row: with(func(r *routeReconcileRow) { r.LeaseUntilMs = decideNow }), meta: writable,
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "已冻结_零写入", row: held, meta: RouteMeta{Generation: decideGen, Writable: 0},
			want: ReconcileTickAlreadyFrozen, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "已冻结优先于回档证据", row: held, meta: RouteMeta{Generation: decideGen, Writable: 0},
			key: key(decideGen, decideNow+1), cvm: u64(5),
			want: ReconcileTickAlreadyFrozen, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "writable为其它值也算冻结", row: held, meta: RouteMeta{Generation: decideGen, Writable: 2},
			want: ReconcileTickAlreadyFrozen, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "世代不符", row: held, meta: writable, key: key("ffffffffffffffffffffffffffffffff", 1),
			want: ReconcileTickFrozen, reason: RouteFreezeGenerationMismatch, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "世代不符优先于水位", row: held, meta: writable, key: key("ffffffffffffffffffffffffffffffff", decideNow+1),
			want: ReconcileTickFrozen, reason: RouteFreezeGenerationMismatch, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "水位倒退_容差0", row: held, meta: writable, key: key(decideGen, held.WatermarkMs+1),
			want: ReconcileTickFrozen, reason: RouteFreezeWatermarkRegressed, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "水位倒退_冻结时不写cvm", row: held, meta: writable, key: key(decideGen, held.WatermarkMs+1), cvm: u64(7),
			want: ReconcileTickFrozen, reason: RouteFreezeWatermarkRegressed, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "水位相等_容差0_推进", row: held, meta: writable, key: key(decideGen, held.WatermarkMs),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "水位键落后_推进", row: held, meta: writable, key: key(decideGen, held.WatermarkMs-500),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "水位倒退_容差1000_未超", row: held, meta: writable, key: key(decideGen, held.WatermarkMs+1000), tol: 1000,
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "水位倒退_容差1000_超出", row: held, meta: writable, key: key(decideGen, held.WatermarkMs+1001), tol: 1000,
			want: ReconcileTickFrozen, reason: RouteFreezeWatermarkRegressed, wantWM: held.WatermarkMs, wantCvm: held.CacheVerifiedMs},
		{name: "容差极大不溢出", row: with(func(r *routeReconcileRow) { r.WatermarkMs = 10 }), meta: writable,
			key: key(decideGen, math.MaxUint64), tol: math.MaxUint64 - 5,
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "无水位键_只推进", row: held, meta: writable,
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "库水位已超过库时钟_不倒退", row: with(func(r *routeReconcileRow) { r.WatermarkMs = decideNow + 3_600_000 }), meta: writable,
			want: ReconcileTickAdvanced, wantWM: decideNow + 3_600_000, wantCvm: held.CacheVerifiedMs},
		{name: "cvm不给定_不动", row: held, meta: writable, key: key(decideGen, held.WatermarkMs),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: held.CacheVerifiedMs},
		{name: "cvm给定_精确写", row: held, meta: writable, cvm: u64(decideNow - 15_000),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: decideNow - 15_000, setCvm: true},
		{name: "cvm给定_可以变小", row: held, meta: writable, cvm: u64(100),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: 100, setCvm: true},
		{name: "cvm给定为0_写0", row: held, meta: writable, cvm: u64(0),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: 0, setCvm: true},
		{name: "cvm给定_等于库时钟", row: held, meta: writable, cvm: u64(decideNow),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: decideNow, setCvm: true},
		{name: "cvm给定_超过库时钟_按库时钟写", row: held, meta: writable, cvm: u64(decideNow + 1),
			want: ReconcileTickAdvanced, wantWM: decideNow, wantCvm: decideNow, setCvm: true, clampsCvm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decideReconcileDB(tc.row, tc.meta, tc.key, tc.tol, decideLeader, tc.cvm)
			if d.Outcome != tc.want || d.FreezeReason != tc.reason {
				t.Fatalf("decide = (%s, %s), want (%s, %s)", d.Outcome, d.FreezeReason, tc.want, tc.reason)
			}
			if d.WatermarkMs != tc.wantWM || d.CacheVerifiedMs != tc.wantCvm {
				t.Fatalf("decide values = (wm %d, cvm %d), want (wm %d, cvm %d)", d.WatermarkMs, d.CacheVerifiedMs, tc.wantWM, tc.wantCvm)
			}
			if d.SetCacheVerified != tc.setCvm || d.CacheVerifiedClamped != tc.clampsCvm {
				t.Fatalf("decide cvm flags = (set %t, clamped %t), want (set %t, clamped %t)",
					d.SetCacheVerified, d.CacheVerifiedClamped, tc.setCvm, tc.clampsCvm)
			}
			if d.Outcome != ReconcileTickAdvanced && d.SetCacheVerified {
				t.Fatalf("only Advanced may write cvm: %+v", d)
			}
			if d.Outcome != ReconcileTickFrozen && d.FreezeReason != RouteFreezeNone {
				t.Fatalf("FreezeReason must be none unless Frozen: %+v", d)
			}
		})
	}
}

// TestReconcileDecisionUnchangedFrom:R27 受影响 0 行时「值本来就不变」的判据只看水位与 cvm。
func TestReconcileDecisionUnchangedFrom(t *testing.T) {
	row := routeReconcileRow{Leader: decideLeader, LeaseUntilMs: decideNow + 1, WatermarkMs: decideNow + 10, CacheVerifiedMs: 5, NowMs: decideNow}
	meta := RouteMeta{Generation: decideGen, Writable: 1}
	if d := decideReconcileDB(row, meta, nil, 0, decideLeader, nil); !d.unchangedFrom(row) {
		t.Fatalf("watermark already ahead of the clock and cvm not given: must be unchanged: %+v", d)
	}
	five := uint64(5)
	if d := decideReconcileDB(row, meta, nil, 0, decideLeader, &five); !d.unchangedFrom(row) {
		t.Fatalf("cvm given equal to the stored value: must be unchanged: %+v", d)
	}
	six := uint64(6)
	if d := decideReconcileDB(row, meta, nil, 0, decideLeader, &six); d.unchangedFrom(row) {
		t.Fatalf("a different cvm is a change: %+v", d)
	}
	row.WatermarkMs = decideNow - 1
	if d := decideReconcileDB(row, meta, nil, 0, decideLeader, nil); d.unchangedFrom(row) {
		t.Fatalf("a watermark behind the clock moves: %+v", d)
	}
}

// TestRouteReconcileRowHeldBy:持有 ⇔ leader 是自己且 lease_until_ms >= 库时钟(与 R23 的接管条件互为反面)。
func TestRouteReconcileRowHeldBy(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  routeReconcileRow
		id   string
		want bool
	}{
		{"有效", routeReconcileRow{Leader: decideLeader, LeaseUntilMs: decideNow + 1, NowMs: decideNow}, decideLeader, true},
		{"恰好到期", routeReconcileRow{Leader: decideLeader, LeaseUntilMs: decideNow, NowMs: decideNow}, decideLeader, true},
		{"过期1ms", routeReconcileRow{Leader: decideLeader, LeaseUntilMs: decideNow - 1, NowMs: decideNow}, decideLeader, false},
		{"别人", routeReconcileRow{Leader: decideOther, LeaseUntilMs: decideNow + 1, NowMs: decideNow}, decideLeader, false},
		{"空标识不算持有", routeReconcileRow{Leader: "", LeaseUntilMs: decideNow + 1, NowMs: decideNow}, "", false},
	} {
		if got := tc.row.heldBy(tc.id); got != tc.want {
			t.Fatalf("%s: heldBy = %t, want %t", tc.name, got, tc.want)
		}
	}
	lease := RouteLease{Held: true, Holder: decideLeader}
	if !lease.HeldBy(decideLeader) || lease.HeldBy(decideOther) || lease.HeldBy("") || (RouteLease{Holder: decideLeader}).HeldBy(decideLeader) {
		t.Fatal("RouteLease.HeldBy must require Held and the same non-empty holder")
	}
}

// TestNewRouteLeaderIDUniqueAndValid:leader 标识 = route_leader@{洗过的 host}:{8hex},最长 96 字节(含 200 字节非 ASCII 的 host),
// 过 validRouteWriter;唯一性来自随机后缀(同一台机器、不同后缀 → 不同标识);随机源缺失 / 读不满 → 报错。
func TestNewRouteLeaderIDUniqueAndValid(t *testing.T) {
	if routeLeaderIDMaxLen != routeWriterMaxLen {
		t.Fatalf("routeLeaderIDMaxLen = %d, want exactly the column width %d (13 + 74 + 1 + 8)", routeLeaderIDMaxLen, routeWriterMaxLen)
	}
	suffixRe := regexp.MustCompile(`:[0-9a-f]{8}$`)
	check := func(t *testing.T, id string) {
		t.Helper()
		if !strings.HasPrefix(id, routeLeaderIDPrefix) || !suffixRe.MatchString(id) {
			t.Fatalf("leader id %q must look like route_leader@{host}:{8hex}", id)
		}
		if len(id) > routeWriterMaxLen || !validRouteWriter(id) {
			t.Fatalf("leader id %q (len %d) must pass validRouteWriter (<= %d bytes of [A-Za-z0-9._@:-])", id, len(id), routeWriterMaxLen)
		}
	}

	for _, tc := range []struct {
		name, host, wantHost string
	}{
		{"常规", "ds-0.data-service:8080", "ds-0.data-service:8080"},
		{"非ASCII被清洗", "主机-1", "-1"},
		{"host为空", "", "unknown"},
		{"host洗完为空", "主机", "unknown"},
		{"200字节非ASCII_全部洗掉", strings.Repeat("主机名", 22) + "ab", "ab"},
		{"200字节混合_截到74", strings.Repeat("主", 40) + strings.Repeat("h", 80), strings.Repeat("h", routeLeaderHostMaxLen)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := NewRouteLeaderID(tc.host, bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef}))
			if err != nil {
				t.Fatalf("NewRouteLeaderID(%q): %v", tc.host, err)
			}
			check(t, id)
			if want := routeLeaderIDPrefix + tc.wantHost + ":deadbeef"; id != want {
				t.Fatalf("NewRouteLeaderID(%q) = %q, want %q", tc.host, id, want)
			}
		})
	}
	if long := strings.Repeat("主", 40) + strings.Repeat("h", 80); len(long) != 200 {
		t.Fatalf("fixture host must be 200 bytes, got %d", len(long))
	}

	// 同一台机器上的两个进程:后缀不同 → 标识不同;1000 个两两不同的后缀 → 1000 个两两不同的标识。
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id, err := NewRouteLeaderID("same-host", bytes.NewReader([]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}))
		if err != nil {
			t.Fatalf("NewRouteLeaderID #%d: %v", i, err)
		}
		check(t, id)
		if seen[id] {
			t.Fatalf("leader id %q repeated for a different random suffix", id)
		}
		seen[id] = true
	}

	if _, err := NewRouteLeaderID("h", nil); !errors.Is(err, ErrRouteInvalidArgument) {
		t.Fatalf("nil random source: err = %v, want ErrRouteInvalidArgument", err)
	}
	if _, err := NewRouteLeaderID("h", bytes.NewReader([]byte{1, 2})); err == nil {
		t.Fatal("a short random read must be an error, not a shorter suffix")
	}
}

// TestRouteReconcileSQLShapes 钉住 R23–R30 的文本形状:每条 route_reconcile 写都带租约判据(fencing)、库时钟用 dbNowMsSQL、
// normal 不再直接翻 writable；R29 照提案 G1a 模板(片等值 + 键集,不是行比较)、只有 R26 加锁。
func TestRouteReconcileSQLShapes(t *testing.T) {
	contains := func(name, stmt string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if !strings.Contains(stmt, p) {
				t.Fatalf("%s must contain %q: %q", name, p, stmt)
			}
		}
	}
	placeholders := func(name, stmt string, want int) {
		t.Helper()
		if got := strings.Count(stmt, "?"); got != want {
			t.Fatalf("%s has %d placeholders, want %d: %q", name, got, want, stmt)
		}
	}

	contains("R23", routeRenewLeaseSQL,
		"UPDATE route_reconcile SET leader = ?, lease_until_ms = "+dbNowMsSQL+" + ?",
		"WHERE id = 1 AND (leader IS NULL OR leader = ? OR lease_until_ms IS NULL OR lease_until_ms < "+dbNowMsSQL+")")
	placeholders("R23", routeRenewLeaseSQL, 3)

	contains("R24", routePeekLeaseSQL, "SELECT leader, lease_until_ms, "+dbNowMsSQL+" FROM route_reconcile WHERE id = 1")
	placeholders("R24", routePeekLeaseSQL, 0)

	if routeReleaseLeaseSQL != "UPDATE route_reconcile SET leader = NULL, lease_until_ms = NULL WHERE id = 1 AND leader = ?" {
		t.Fatalf("R25 must only clear the caller's own lease: %q", routeReleaseLeaseSQL)
	}

	contains("R26", routeLockReconcileSQL,
		"SELECT leader, lease_until_ms, watermark_ms, cache_verified_ms, "+dbNowMsSQL, "FROM route_reconcile WHERE id = 1 FOR UPDATE")
	if !strings.HasSuffix(routeLockReconcileSQL, " FOR UPDATE") {
		t.Fatalf("R26 must be a locking read: %q", routeLockReconcileSQL)
	}

	contains("R27", routeAdvanceReconcileSQL,
		"watermark_ms = GREATEST(watermark_ms, ?)",
		"cache_verified_ms = COALESCE(?, cache_verified_ms)",
		"WHERE id = 1 AND leader = ? AND lease_until_ms >= "+dbNowMsSQL)
	placeholders("R27", routeAdvanceReconcileSQL, 3)

	// 危险只交付观察；normal 对账 SQL 不拥有 writable 翻转语句。

	contains("R29", routeScanUpdatedSinceSQL,
		"SELECT route_shard, player_id, "+routeRowColumnsSQL+" FROM player_route",
		"WHERE route_shard = ? AND (updated_at_ms > ? OR (updated_at_ms = ? AND player_id > ?))",
		" ORDER BY updated_at_ms, player_id LIMIT ?")
	placeholders("R29", routeScanUpdatedSinceSQL, 5)
	if strings.Contains(routeScanUpdatedSinceSQL, "(updated_at_ms, player_id) >") {
		t.Fatalf("R29 must not use a row comparison (MySQL does not range-optimise it): %q", routeScanUpdatedSinceSQL)
	}

	if routeDBNowSQL != "SELECT "+dbNowMsSQL {
		t.Fatalf("R30 = %q", routeDBNowSQL)
	}

	ws := regexp.MustCompile(`\s+`)
	for name, stmt := range map[string]string{
		"R23": routeRenewLeaseSQL, "R24": routePeekLeaseSQL, "R25": routeReleaseLeaseSQL, "R26": routeLockReconcileSQL,
		"R27": routeAdvanceReconcileSQL, "R29": routeScanUpdatedSinceSQL, "R30": routeDBNowSQL,
	} {
		norm := ws.ReplaceAllString(strings.ToUpper(stmt), " ")
		if strings.Contains(norm, "INSERT IGNORE") || strings.Contains(norm, "REPLACE INTO") {
			t.Fatalf("%s must not use INSERT IGNORE / REPLACE INTO: %q", name, stmt)
		}
		locking := strings.Contains(norm, "FOR UPDATE") || strings.Contains(norm, "FOR SHARE") || strings.Contains(norm, "LOCK IN SHARE MODE")
		if locking != (name == "R26") {
			t.Fatalf("%s locking=%t: only R26 may take a row lock: %q", name, locking, stmt)
		}
	}
}

// TestRouteReconcileFileDoesNotWritePlayerRoute:本文件对 player_route 只读(R29),写入方仍只有 route_store.go。
func TestRouteReconcileFileDoesNotWritePlayerRoute(t *testing.T) {
	write := regexp.MustCompile("(?i)(INSERT\\s+INTO|UPDATE|DELETE\\s+FROM|REPLACE\\s+INTO|TRUNCATE\\s+TABLE)\\s+`?(" +
		PlayerRouteTableName + "|" + ZoneMergedIntoTableName + ")\\b")
	for name, stmt := range map[string]string{
		"R23": routeRenewLeaseSQL, "R24": routePeekLeaseSQL, "R25": routeReleaseLeaseSQL, "R26": routeLockReconcileSQL,
		"R27": routeAdvanceReconcileSQL, "R29": routeScanUpdatedSinceSQL, "R30": routeDBNowSQL,
	} {
		if write.MatchString(stmt) {
			t.Fatalf("%s writes player_route / zone_merged_into: %q", name, stmt)
		}
	}
}

// TestRouteUpdatedCursorOrder:增量游标按 (updated_at_ms, player_id) 比较,与 R29 的 ORDER BY 同序;
// RouteUpdatedSince(s) 是「updated_at_ms >= s」的起点(player_id 为 0 的行不合法)。
func TestRouteUpdatedCursorOrder(t *testing.T) {
	for _, tc := range []struct {
		a, b RouteUpdatedCursor
		less bool
	}{
		{RouteUpdatedCursor{100, 9}, RouteUpdatedCursor{101, 1}, true},
		{RouteUpdatedCursor{100, 1}, RouteUpdatedCursor{100, 2}, true},
		{RouteUpdatedCursor{100, 2}, RouteUpdatedCursor{100, 2}, false},
		{RouteUpdatedCursor{101, 1}, RouteUpdatedCursor{100, 9}, false},
		{RouteUpdatedSince(100), RouteUpdatedCursor{100, 1}, true},
		{RouteUpdatedSince(100), RouteUpdatedCursor{99, math.MaxUint64}, false},
	} {
		if got := tc.a.Less(tc.b); got != tc.less {
			t.Fatalf("%+v.Less(%+v) = %t, want %t", tc.a, tc.b, got, tc.less)
		}
	}
	if got := RouteUpdatedCursorAt(RouteRow{PlayerID: 7, UpdatedAtMs: 123}); got != (RouteUpdatedCursor{UpdatedAtMs: 123, PlayerID: 7}) {
		t.Fatalf("RouteUpdatedCursorAt = %+v", got)
	}
}

// TestReconcileLabelsStableLowCardinality:String() 进日志与指标 label,值必须稳定且低基数。
func TestReconcileLabelsStableLowCardinality(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{ReconcileTickAdvanced.String(), "advanced"},
		{ReconcileTickFrozen.String(), "frozen"},
		{ReconcileTickAlreadyFrozen.String(), "already_frozen"},
		{ReconcileTickLeaseLost.String(), "lease_lost"},
		{ReconcileTickOutcome(0).String(), "unknown"},
		{RouteFreezeNone.String(), "none"},
		{RouteFreezeWatermarkRegressed.String(), "watermark_regressed"},
		{RouteFreezeGenerationMismatch.String(), "generation_mismatch"},
		{RouteFreezeCacheAhead.String(), "cache_ahead"},
		{routeFreezeReasonCount.String(), "unknown"},
	} {
		if tc.got != tc.want {
			t.Fatalf("label = %q, want %q", tc.got, tc.want)
		}
	}
}
