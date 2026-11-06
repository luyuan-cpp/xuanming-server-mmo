package main

// checkGuildZoneFK 的单测(第二轮审计 r2-dcc#1):读不到 guild 表不能报成「没有公会」,逐行扫描失败不能被跳过,
// 死区行数数不全时不能冒充精确值。语句经 route_source_test.go 的 openFakeSQL 假驱动按原文回结果,不连真库。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	testGuildSchema      = "mmorpg_guild"
	testGuildCountSQL    = "SELECT COUNT(*) FROM mmorpg_guild.guild"
	testGuildDistinctSQL = "SELECT DISTINCT zone_id FROM mmorpg_guild.guild"
	testGuildPerZoneSQL  = "SELECT COUNT(*) FROM mmorpg_guild.guild WHERE zone_id = ?"
)

// fakeGuildTable 是假 guild 表对三条语句的回答:countErr / distinctErr 非 nil 时对应语句报错;
// distinct 是 DISTINCT zone_id 的各行(nil = NULL);perZone 按 zone_id 回行数,缺键的区报错。
type fakeGuildTable struct {
	count       int64
	countErr    error
	distinct    []driver.Value
	distinctErr error
	perZone     map[int64]int64
}

func openFakeGuildDB(t *testing.T, g fakeGuildTable) (*sql.DB, *fakeSQLServer) {
	t.Helper()
	return openFakeSQL(t, func(query string, args []any) ([]string, [][]driver.Value, error) {
		switch query {
		case testGuildCountSQL:
			if g.countErr != nil {
				return nil, nil, g.countErr
			}
			return []string{"n"}, [][]driver.Value{{g.count}}, nil
		case testGuildDistinctSQL:
			if g.distinctErr != nil {
				return nil, nil, g.distinctErr
			}
			rows := make([][]driver.Value, len(g.distinct))
			for i, v := range g.distinct {
				rows[i] = []driver.Value{v}
			}
			return []string{"zone_id"}, rows, nil
		case testGuildPerZoneSQL:
			if len(args) != 1 {
				return nil, nil, fmt.Errorf("per-zone count args = %v", args)
			}
			zone, ok := args[0].(int64)
			if !ok {
				return nil, nil, fmt.Errorf("per-zone count arg type %T", args[0])
			}
			n, ok := g.perZone[zone]
			if !ok {
				return nil, nil, fmt.Errorf("Error 2013: lost connection while counting zone %d", zone)
			}
			return []string{"n"}, [][]driver.Value{{n}}, nil
		}
		return nil, nil, fmt.Errorf("unexpected query %q", query)
	})
}

func guildFKConfig(db *sql.DB, live ...uint32) runConfig {
	known := map[uint32]struct{}{}
	for _, z := range live {
		known[z] = struct{}{}
	}
	return runConfig{db: db, knownZones: known, guildSchema: testGuildSchema}
}

func queriesOf(srv *fakeSQLServer) []string {
	out := make([]string, 0, len(srv.queries))
	for _, q := range srv.queries {
		out = append(out, q.query)
	}
	return out
}

// TestGuildZoneFKUnreadableTableIsNotChecked:2026-11-05 之前读不到表报 info「not present」,和「没有公会」分不出来;
// 现在报 warn「NOT CHECKED」并带上原错误,不再往下查。
func TestGuildZoneFKUnreadableTableIsNotChecked(t *testing.T) {
	db, srv := openFakeGuildDB(t, fakeGuildTable{countErr: errors.New("Error 1146 (42S02): Table 'mmorpg_guild.guild' doesn't exist")})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Name != "guild.zone_id" || r.Severity != "warn" || r.BadCount != 0 || r.TotalCount != 0 {
		t.Fatalf("result = %+v, want warn with zero counts", r)
	}
	if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: mmorpg_guild.guild unreadable") || !strings.Contains(r.SampleNotes, "1146") {
		t.Fatalf("notes = %q, want NOT CHECKED with the original error", r.SampleNotes)
	}
	if got := queriesOf(srv); len(got) != 1 {
		t.Fatalf("queries = %v, want to stop after the failed count", got)
	}
}

// TestGuildZoneFKEmptyTableIsInfo:表在、没有公会才是 info,与上一条在报告里长得不一样。
func TestGuildZoneFKEmptyTableIsInfo(t *testing.T) {
	db, _ := openFakeGuildDB(t, fakeGuildTable{})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Severity != "info" || r.BadCount != 0 || r.TotalCount != 0 || strings.Contains(r.SampleNotes, "NOT CHECKED") {
		t.Fatalf("result = %+v, want info for an empty guild table", r)
	}
}

func TestGuildZoneFKDistinctQueryFailureIsNotChecked(t *testing.T) {
	db, _ := openFakeGuildDB(t, fakeGuildTable{count: 3, distinctErr: errors.New("Error 1054 (42S22): Unknown column 'zone_id'")})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Severity != "warn" || r.BadCount != 0 {
		t.Fatalf("result = %+v, want warn", r)
	}
	if !strings.HasPrefix(r.SampleNotes, "NOT CHECKED: distinct zone_id query failed") || !strings.Contains(r.SampleNotes, "1054") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
}

// TestGuildZoneFKScanErrorFailsWholeCheck:2026-11-05 之前扫不出的行被 continue 跳过,剩下的都是活区就报干净;
// 现在整条检查判 fail(进程退出 1),也不再去数死区。
func TestGuildZoneFKScanErrorFailsWholeCheck(t *testing.T) {
	db, srv := openFakeGuildDB(t, fakeGuildTable{count: 2, distinct: []driver.Value{int64(1), nil}})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Severity != "fail" || r.BadCount != 0 {
		t.Fatalf("result = %+v, want fail with BadCount 0", r)
	}
	if !strings.HasPrefix(r.SampleNotes, "CHECK FAILED") || !strings.Contains(r.SampleNotes, "scan zone_id after 1 distinct values") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
	if got := queriesOf(srv); fmt.Sprint(got) != fmt.Sprint([]string{testGuildCountSQL, testGuildDistinctSQL}) {
		t.Fatalf("queries = %v, want no per-zone count after the failed scan", got)
	}
	if n := printReport([]CheckResult{r}, "route source: fake"); n != 1 {
		t.Fatalf("printReport counted %d failing rows, want 1 (fail must exit non-zero)", n)
	}
}

func TestGuildZoneFKCountsDeadZones(t *testing.T) {
	db, srv := openFakeGuildDB(t, fakeGuildTable{
		count:    6,
		distinct: []driver.Value{int64(1), int64(7), int64(9)},
		perZone:  map[int64]int64{7: 2, 9: 1},
	})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Severity != "block" || r.BadCount != 3 || r.TotalCount != 6 {
		t.Fatalf("result = %+v, want block with 3 of 6", r)
	}
	if !strings.Contains(r.SampleNotes, "[7 9]") || strings.Contains(r.SampleNotes, "lower bound") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
	want := []fakeSQLQuery{
		{testGuildCountSQL, []any{}},
		{testGuildDistinctSQL, []any{}},
		{testGuildPerZoneSQL, []any{int64(7)}},
		{testGuildPerZoneSQL, []any{int64(9)}},
	}
	if fmt.Sprint(srv.queries) != fmt.Sprint(want) {
		t.Fatalf("queries =\n%v\nwant\n%v", srv.queries, want)
	}
}

// TestGuildZoneFKPerZoneCountFailureIsLowerBound:2026-11-05 之前死区计数的错误被丢掉,少数的行数当成精确值报出;
// 现在仍报 block(死区已确定),但写明行数只是下界、哪个区没数到。
func TestGuildZoneFKPerZoneCountFailureIsLowerBound(t *testing.T) {
	db, _ := openFakeGuildDB(t, fakeGuildTable{
		count:    6,
		distinct: []driver.Value{int64(1), int64(7), int64(9)},
		perZone:  map[int64]int64{7: 2},
	})
	r := checkGuildZoneFK(context.Background(), guildFKConfig(db, 1))

	if r.Severity != "block" || r.BadCount != 2 {
		t.Fatalf("result = %+v, want block with lower-bound count 2", r)
	}
	if !strings.Contains(r.SampleNotes, "lower bound") || !strings.Contains(r.SampleNotes, "zone 9") || !strings.Contains(r.SampleNotes, "2013") {
		t.Fatalf("notes = %q", r.SampleNotes)
	}
}
