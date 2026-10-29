package main

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 存量雪花 player_id 约 2.8e17,超过 2^53;经 float64 会被改成别的号(审计 data-service#1)。
func TestResolveColumnValueKeepsLargeIntegers(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{"288230376151711745", int64(288230376151711745)},
		{"18446744073709551615", uint64(math.MaxUint64)},
		{"-5", int64(-5)},
		{"1e21", float64(1e21)},
		{"3.14", 3.14},
	}
	for _, c := range cases {
		got, err := resolveColumnValue(json.RawMessage(c.raw))
		if err != nil || got != c.want {
			t.Fatalf("%s -> %v (%T), %v; want %v (%T)", c.raw, got, got, err, c.want, c.want)
		}
	}
}

func TestOverridePlayerIDRewritesMatchColumn(t *testing.T) {
	data := &exportFile{Mode: "playerdb", PlayerID: 7, Matches: []tableMatch{{
		Table: "player_centre_database", MatchColumn: "player_id",
		Rows: []map[string]json.RawMessage{{"player_id": json.RawMessage("7"), "other": json.RawMessage(`"x"`)}},
	}}}
	overridePlayerID(data, 288230376151711745)
	if data.PlayerID != 288230376151711745 {
		t.Fatalf("player id = %d", data.PlayerID)
	}
	got, err := resolveColumnValue(data.Matches[0].Rows[0]["player_id"])
	if err != nil || got != int64(288230376151711745) {
		t.Fatalf("match column = %v, %v", got, err)
	}
	if string(data.Matches[0].Rows[0]["other"]) != `"x"` {
		t.Fatal("non-match column changed")
	}
}

func legacyExport(table string, truncated *bool) *exportFile {
	return &exportFile{Mode: "playerdb", PlayerID: 7, ZoneID: 1, Matches: []tableMatch{{
		Table: table, MatchColumn: "player_id", Truncated: truncated,
		Rows: []map[string]json.RawMessage{{"player_id": json.RawMessage("7")}},
	}}}
}

// 这些拒绝都发生在 dry-run 判断与连库之前,不需要数据库。
func TestImportPlayerDBRefusesUnsafeExports(t *testing.T) {
	no, yes := false, true
	cases := map[string]struct {
		data *exportFile
		want string
	}{
		"legacy export without truncated flag": {legacyExport("player_centre_database", nil), "truncated"},
		"truncated export":                     {legacyExport("player_centre_database", &yes), "截断"},
		"fenced table":                         {legacyExport("player_database", &no), "player-bundle"},
	}
	errored := legacyExport("player_centre_database", &no)
	errored.Errors = []map[string]string{{"table": "player_database", "error": "timeout"}}
	cases["export with table errors"] = struct {
		data *exportFile
		want string
	}{errored, "查询失败"}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := importPlayerDB(context.Background(), "", c.data, legacyImportOptions{dryRun: true})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want contains %q", err, c.want)
			}
		})
	}
	if err := importPlayerDB(context.Background(), "", legacyExport("player_centre_database", &no), legacyImportOptions{dryRun: true}); err != nil {
		t.Fatalf("clean dry run refused: %v", err)
	}
	if err := importPlayerDB(context.Background(), "", legacyExport("player_database", &no), legacyImportOptions{dryRun: true, allowFencedTables: true}); err != nil {
		t.Fatalf("-allow-fenced-tables dry run refused: %v", err)
	}
}
