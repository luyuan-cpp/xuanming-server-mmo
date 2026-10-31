package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/logx/logtest"
	"match/internal/authority"
)

// 2026-10-30(评审 B-4 / B-6):每分钟的全球终局巡检除了原有的事件计数,还要写两组 gauge:到期滞留成局的存量,
// 与终局发件箱三类积压。用替身权威直接调生产函数,不起 TiDB。

type outcomeAuditorFake struct {
	stranded    uint64
	strandedErr error
	backlog     authority.OutcomeOutboxBacklog
	backlogErr  error
}

func (f *outcomeAuditorFake) CountStrandedExpiredFormations(context.Context) (uint64, error) {
	return f.stranded, f.strandedErr
}

func (f *outcomeAuditorFake) ReadOutcomeOutboxBacklog(context.Context) (authority.OutcomeOutboxBacklog, error) {
	return f.backlog, f.backlogErr
}

type backlogReading struct {
	kind   string
	rows   uint64
	oldest float64
}

type recordedAuditGauges struct {
	stranded []uint64
	backlog  []backlogReading
}

func (r *recordedAuditGauges) sink() globalAuditGauges {
	return globalAuditGauges{
		strandedExpired: func(n uint64) { r.stranded = append(r.stranded, n) },
		outboxBacklog: func(kind string, rows uint64, oldest float64) {
			r.backlog = append(r.backlog, backlogReading{kind, rows, oldest})
		},
	}
}

// 存量 gauge 每轮都写,0 也写(处置之后读数要能回落);读失败保留上一次读数,不伪造 0。
// 只有存量 > 0 才打 ERROR(原有的事件计数口径不变)。
func TestGlobalOutcomeAuditWritesStrandedStockEvenWhenZero(t *testing.T) {
	for _, tc := range []struct {
		name      string
		auditor   *outcomeAuditorFake
		wantGauge []uint64
		wantError bool
	}{
		{"没有滞留", &outcomeAuditorFake{}, []uint64{0}, false},
		{"三局滞留", &outcomeAuditorFake{stranded: 3}, []uint64{3}, true},
		{"读失败", &outcomeAuditorFake{strandedErr: errors.New("fixture TiDB down")}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)
			var got recordedAuditGauges
			auditStrandedExpired(context.Background(), tc.auditor, got.sink())
			require.Equal(t, tc.wantGauge, got.stranded)
			out := logs.String()
			require.Equal(t, tc.wantError, strings.Contains(out, "cannot reach expired-outcome recovery") ||
				strings.Contains(out, "audit failed"), out)
		})
	}
}

// 终局发件箱积压按 kind 逐类写 gauge(result / first / waiting,封闭集合);读失败一个都不写。
func TestGlobalOutcomeAuditReportsOutboxBacklogPerKind(t *testing.T) {
	var got recordedAuditGauges
	auditor := &outcomeAuditorFake{backlog: authority.OutcomeOutboxBacklog{
		Result:  authority.OutboxBacklog{Rows: 2, OldestAgeSeconds: 3.5},
		First:   authority.OutboxBacklog{},
		Waiting: authority.OutboxBacklog{Rows: 40, OldestAgeSeconds: 86400},
	}}
	auditGlobalOutcomes(context.Background(), auditor, got.sink())
	require.Equal(t, []uint64{0}, got.stranded)
	require.Equal(t, []backlogReading{
		{authority.OutboxKindResult, 2, 3.5},
		{authority.OutboxKindFirst, 0, 0},
		{authority.OutboxKindWaiting, 40, 86400},
	}, got.backlog)

	logs := logtest.NewCollector(t)
	got = recordedAuditGauges{}
	auditOutcomeOutboxBacklog(context.Background(), &outcomeAuditorFake{backlogErr: errors.New("fixture TiDB down")}, got.sink())
	require.Empty(t, got.backlog, "读失败保留上一次读数")
	require.Contains(t, logs.String(), "backlog read failed")
}
