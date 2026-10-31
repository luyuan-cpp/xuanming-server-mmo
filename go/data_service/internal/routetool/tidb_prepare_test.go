package routetool

// tidb-prepare 的输出与退出码:假 preparer,不连库。语句本身与引擎判定由 store 包的用例钉住。

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"data_service/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// *store.RouteStore 必须满足 tidbPreparer(routetoolcli 直接把它传进来);签名漂移在这里编译期就炸。
var _ tidbPreparer = (*store.RouteStore)(nil)

type cliRun struct {
	code   int
	stdout string
	stderr string
}

// fakeTiDBPreparer 按预置结果回答 PrepareTiDB,并记下入参。
type fakeTiDBPreparer struct {
	rep  store.RouteTiDBPrepareReport
	err  error
	reqs []store.RouteTiDBPrepareRequest
}

func (f *fakeTiDBPreparer) PrepareTiDB(_ context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error) {
	f.reqs = append(f.reqs, req)
	return f.rep, f.err
}

// TestRouteTiDBPrepareOutputAndExitCodes:MySQL 跳过 → 退出 0、stdout 一行统计、stderr 有 SKIP;TiDB dry-run → 每步一行 PLAN、
// executed=0;真跑 → 每步一行 DONE;出错 → 退出 2、stdout 为空、stderr 有 FAILED(已执行的步骤照列)。
func TestRouteTiDBPrepareOutputAndExitCodes(t *testing.T) {
	run := func(p *fakeTiDBPreparer, dryRun bool) cliRun {
		var out, errOut bytes.Buffer
		rep, err := TiDBPrepare(context.Background(), p, TiDBPrepareOptions{
			Zones:    []uint32{2, 1, 2},
			Storages: []uint32{1000000},
			DryRun:   dryRun,
			Target:   "route_tool tidb-prepare: global_db=127.0.0.1:4000/testdb",
			Stdout:   &out,
			Stderr:   &errOut,
		})
		return cliRun{code: TiDBPrepareExitCode(rep, err), stdout: out.String(), stderr: errOut.String()}
	}
	steps := func(executed bool) []store.RouteTiDBPrepareStep {
		return []store.RouteTiDBPrepareStep{
			{SQL: "ALTER TABLE `player_route` ATTRIBUTES 'merge_option=deny'", Executed: executed},
			{SQL: "SPLIT TABLE `player_route` BY (1, 0), (2, 0)", Executed: executed, SplitRegions: map[bool]int64{true: 2}[executed]},
		}
	}

	t.Run("MySQL跳过", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "mysql", Version: "8.0.36", Skipped: true}}
		r := run(p, false)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.Equal(t, 1, strings.Count(r.stdout, "\n"), r.stdout)
		assert.Contains(t, r.stdout, "route_tidb_prepare engine=mysql skipped=true ")
		assert.Contains(t, r.stdout, " zones=2 storages=1 statements=0 executed=0 split_regions=0")
		assert.Contains(t, r.stderr, "SKIP: 8.0.36 is not TiDB")
		assert.True(t, strings.HasPrefix(r.stderr, "route_tool tidb-prepare: global_db="), "stderr 第一行是连接目标")
		require.Len(t, p.reqs, 1)
		assert.Equal(t, []uint32{2, 1, 2}, p.reqs[0].Zones, "入参原样交给 store(规范化在 store 里)")
	})

	t.Run("TiDB_dry-run", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: steps(false)}}
		r := run(p, true)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.True(t, p.reqs[0].DryRun)
		assert.Contains(t, r.stderr, "PLAN ALTER TABLE `player_route` ATTRIBUTES 'merge_option=deny'\n")
		assert.Contains(t, r.stderr, "PLAN SPLIT TABLE `player_route` BY (1, 0), (2, 0)\n")
		assert.NotContains(t, r.stderr, "DONE ")
		assert.Contains(t, r.stdout, "route_tidb_prepare engine=tidb skipped=false dry_run=true ")
		assert.Contains(t, r.stdout, " statements=2 executed=0 split_regions=0")
	})

	t.Run("TiDB_真跑", func(t *testing.T) {
		p := &fakeTiDBPreparer{rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: steps(true)}}
		r := run(p, false)
		assert.Equal(t, ExitClean, r.code, r.stderr)
		assert.Contains(t, r.stderr, "DONE SPLIT TABLE `player_route` BY (1, 0), (2, 0) split_regions=2\n")
		assert.Contains(t, r.stdout, " statements=2 executed=2 split_regions=2")
	})

	t.Run("失败_退出2_stdout为空", func(t *testing.T) {
		failed := steps(false)
		failed[0].Executed = true
		p := &fakeTiDBPreparer{
			rep: store.RouteTiDBPrepareReport{Engine: "tidb", Version: "8.0.11-TiDB-v8.5.2", Steps: failed},
			err: errors.New("tidb-prepare step 2/2: SPLIT TABLE `player_route` BY (1, 0), (2, 0): Error 1105"),
		}
		r := run(p, false)
		assert.Equal(t, ExitFailed, r.code)
		assert.Empty(t, r.stdout)
		assert.Contains(t, r.stderr, "DONE ALTER TABLE `player_route`")
		assert.Contains(t, r.stderr, "FAILED: tidb-prepare step 2/2")
	})

	t.Run("没给writer_退出2", func(t *testing.T) {
		rep, err := TiDBPrepare(context.Background(), &fakeTiDBPreparer{}, TiDBPrepareOptions{Zones: []uint32{1}})
		assert.Equal(t, ExitFailed, TiDBPrepareExitCode(rep, err))
	})
}
