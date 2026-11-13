package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"schemamigrate"
)

// TestSchemaGate 钉住启动 / 迁移门禁与它对应的退出码(D-14:需人工 4、其余失败 1,迁移 Job 的 podFailurePolicy 按它判)。
//
// 最要紧的一条是「缺普通索引 → 需人工(4)」:schemamigrate 只把缺索引记成 Warning、ExitCode 算出来是 0,
// rank 与 mail / friend / guild 一样升级成需人工。2026-11-12 之前这里返回的是普通错误,实际以 1 退出,
// 错误里也不带是哪条索引。
func TestSchemaGate(t *testing.T) {
	logtest.Discard(t)
	const missingIndexMsg = "missing index (not created automatically): table rank_score lacks index idx_rank_score_0 declared in proto"
	const extraColumnMsg = "extra column (never dropped): rank_score.legacy_col is not declared in proto"
	missingIndex := schemamigrate.Report{Warnings: []string{missingIndexMsg}, Findings: []schemamigrate.Finding{
		{Kind: schemamigrate.FindingMissingIndex, Table: "rank_score", Index: "idx_rank_score_0", Message: missingIndexMsg}}}
	extraColumn := schemamigrate.Report{Warnings: []string{extraColumnMsg}, Findings: []schemamigrate.Finding{
		{Kind: schemamigrate.FindingExtraColumn, Table: "rank_score", Column: "legacy_col", Message: extraColumnMsg}}}
	manual := schemamigrate.Report{Manual: []string{"column type drift (not modified automatically): rank_score.player_id"}}
	pending := schemamigrate.Report{Statements: []string{"CREATE TABLE rank_score ..."}}

	cases := []struct {
		name     string
		report   schemamigrate.Report
		migrate  bool
		wantCode int    // 0 表示放行
		wantText string // 拒绝时错误里必须带的原文
	}{
		{name: "迁移后库干净", migrate: true},
		{name: "常驻启动库干净"},
		{name: "迁移后只有多余列告警", report: extraColumn, migrate: true},
		{name: "常驻启动只有多余列告警", report: extraColumn},
		{name: "迁移后缺索引按需人工", report: missingIndex, migrate: true, wantCode: schemamigrate.ExitManual, wantText: missingIndexMsg},
		{name: "常驻启动缺索引拒启", report: missingIndex, wantCode: schemamigrate.ExitManual, wantText: missingIndexMsg},
		{name: "迁移后有需人工项", report: manual, migrate: true, wantCode: schemamigrate.ExitManual, wantText: "rank_score.player_id"},
		{name: "常驻启动有需人工项", report: manual, wantCode: schemamigrate.ExitManual, wantText: "rank_score.player_id"},
		{name: "常驻启动还有待执行语句", report: pending, wantCode: schemamigrate.ExitFailed, wantText: "-migrate"},
		{name: "迁移刚执行过语句照常放行", report: pending, migrate: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := schemaGate(tc.report, tc.migrate)
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("应放行,得到 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应拒绝")
			}
			if got := exitCode(err); got != tc.wantCode {
				t.Fatalf("退出码 = %d,期望 %d(%v)", got, tc.wantCode, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("错误应带上 %q,实际: %v", tc.wantText, err)
			}
		})
	}

	// 判断只看结构化类别:只有文案、没有缺索引项的报告不拒启,文案改措辞不会改变拒启行为。
	if err := schemaGate(schemamigrate.Report{Warnings: []string{missingIndexMsg}}, true); err != nil {
		t.Fatalf("不能按告警文案判断缺索引: %v", err)
	}
}

// TestExitCode 钉住 run 的错误到退出码的映射:锁忙 3 可重试,需人工 4,其余 1。
func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"需人工", fmt.Errorf("%w: x", errManualSchema), schemamigrate.ExitManual},
		{"迁移锁忙可重试", fmt.Errorf("wrap: %w", schemamigrate.ErrLockBusy), schemamigrate.ExitLockBusy},
		{"其余失败", errors.New("dial tcp: connection refused"), schemamigrate.ExitFailed},
	}
	for _, tc := range cases {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: 退出码 = %d,期望 %d", tc.name, got, tc.want)
		}
	}
}
