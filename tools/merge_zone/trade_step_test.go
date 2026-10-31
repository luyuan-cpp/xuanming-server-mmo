package main

// 聚宝斋合服步骤的纯单测(不连库)。改写 / 撤销 SQL 本身对着真库跑,在 integration_test.go
// (build tag merge_integration)的 TestIT_TradeMarketZone_* 与端到端用例里。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTradeNamesMatchTradeService(t *testing.T) {
	// merge_zone 是独立 module,不能 import go/trade:库名镜像 go/trade/internal/data.DatabaseName,
	// 表名镜像 proto/trade/trade_table.proto 的 OptionTableName。漂移 = 改写落到不存在的表上。
	if defaultTradeSchema != "mmorpg_trade" {
		t.Fatalf("defaultTradeSchema drifted from go/trade data.DatabaseName: %q", defaultTradeSchema)
	}
	if tradeListingTable != "trade_listing" || tradeMarketZoneColumn != "market_zone" {
		t.Fatalf("trade table/column drifted from trade_table.proto: %q.%q", tradeListingTable, tradeMarketZoneColumn)
	}
	if got := tradeListingQualified(defaultTradeSchema); got != "mmorpg_trade.trade_listing" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateTradeSchemaName(t *testing.T) {
	for _, ok := range []string{"mmorpg_trade", "merge_zone_it_trade", "Trade1", strings.Repeat("a", 64)} {
		if err := validateTradeSchemaName(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	// 库名直接拼进 SQL:任何带点、引号、空白、分号、连字符的都必须拒绝。
	for _, bad := range []string{
		"", "mmorpg_trade.trade_listing", "x;DROP DATABASE y", "a b", "`mmorpg_trade`",
		"mmorpg-trade", "mmorpg_trade\n", strings.Repeat("a", 65),
	} {
		if err := validateTradeSchemaName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestStepTradeMySQLNameIsStable(t *testing.T) {
	// 步骤名存进清单,发布后不能改字面量(见 manifest.go)。
	if stepTradeMySQL != "trade_mysql" {
		t.Fatalf("got %q", stepTradeMySQL)
	}
}

func TestManifestWithoutTradeListingIDsStillLoads(t *testing.T) {
	// 升级前写下的清单没有 trade_listing_ids:必须照常读出(不升版本号),
	// 读出来为空、trade 步骤未完成 —— 续跑会重新收集,撤销自然跳过。
	path := filepath.Join(t.TempDir(), "old.json")
	raw, err := json.Marshal(map[string]any{
		"version": manifestVersion, "run_id": "r-old", "source_zone": 901, "target_zone": 902,
		"steps": map[string]any{stepGuildMySQL: map[string]any{"done": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("an old manifest must still load: %v", err)
	}
	if len(m.TradeListingIDs) != 0 {
		t.Errorf("old manifest produced trade ids %v", m.TradeListingIDs)
	}
	if m.stepDone(stepTradeMySQL) {
		t.Error("trade step must not look done on a manifest written before the step existed")
	}
	if !m.stepDone(stepGuildMySQL) {
		t.Error("existing step flags were lost")
	}
}

func TestManifestTradeListingIDsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	m := newMergeManifest("run-t", 901, 902, "op", time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC))
	m.TradeListingIDs = []uint64{7001, 7002}
	m.markStep(stepTradeMySQL, "rows=2 manifest_listings=2")
	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.TradeListingIDs, m.TradeListingIDs) {
		t.Errorf("trade ids lost: %v", got.TradeListingIDs)
	}
	if !got.stepDone(stepTradeMySQL) {
		t.Error("trade step flag lost")
	}
}

func TestVerifyTradeListing_SkipIsAWarningNotAPass(t *testing.T) {
	r := verifyTradeMarketZoneDrained(context.Background(), auditConfig{skipTrade: true, src: 901, dst: 902})
	if r.Severity != "warn" {
		t.Errorf("a skipped check must look weak (warn), got %q", r.Severity)
	}
	if isInfraAudit(r) {
		t.Error("an explicit skip is an operator decision, not an infrastructure failure")
	}
	if !strings.Contains(r.Notes, "NOT VERIFIED") {
		t.Errorf("notes must say it was not verified: %q", r.Notes)
	}
}

func TestVerifyTradeListing_NoMySQLHandleIsInfra(t *testing.T) {
	r := verifyTradeMarketZoneDrained(context.Background(),
		auditConfig{tradeSchema: defaultTradeSchema, src: 901, dst: 902})
	if r.Severity != "block" || !isInfraAudit(r) {
		t.Fatalf("no MySQL handle must be an INFRA block (exit 2), got %+v", r)
	}
}

func TestRunAuditMode_TradeVerifierOnlyInVerifyMode(t *testing.T) {
	// 全部句柄为 nil:每个 auditor 都应返回 INFRA 行而不是 panic。这里只关心 trade 行登记在哪一组 ——
	// 它是合服**后**的断言,合服前源区本来就该有商品。
	has := func(rs []ResourceAudit) bool {
		for _, r := range rs {
			if r.Name == "verify:trade_market_zone" {
				return true
			}
		}
		return false
	}
	ctx := context.Background()
	if !has(runAuditMode(ctx, auditConfig{verify: true, tradeSchema: defaultTradeSchema, src: 901, dst: 902})) {
		t.Error("-verify-merged must include verify:trade_market_zone")
	}
	if has(runAuditMode(ctx, auditConfig{verify: false, tradeSchema: defaultTradeSchema, src: 901, dst: 902})) {
		t.Error("the pre-merge audit must not assert the source market is drained")
	}
}
