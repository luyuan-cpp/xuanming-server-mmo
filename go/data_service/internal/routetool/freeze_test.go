package routetool

// freeze / unfreeze 的输出、时限与退出码:假 Catalog,不连库。票据、epoch、永久收据由 authoritysql 的真库用例钉住。

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"data_service/internal/store"

	"github.com/stretchr/testify/assert"
	pb "proto/common/database"
)

// unitMaintenanceCatalog 记下调用次数与调用时 ctx 带不带截止时间;err 非 nil 时两个方法都回它。
type unitMaintenanceCatalog struct {
	err          error
	freezeCalls  int
	releaseCalls int
	deadline     bool
}

func (c *unitMaintenanceCatalog) Freeze(ctx context.Context, _ []byte, r *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	_, c.deadline = ctx.Deadline()
	c.freezeCalls++
	return r, c.err
}

func (c *unitMaintenanceCatalog) Release(ctx context.Context, _ []byte, r *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error) {
	_, c.deadline = ctx.Deadline()
	c.releaseCalls++
	return r, c.err
}

var unitTicket = bytes.Repeat([]byte{7}, 16)

func unitFreezeRequest() *pb.SqlWriterMaintenanceReceipt {
	return &pb.SqlWriterMaintenanceReceipt{Kind: pb.SqlWriterMaintenanceReceipt_FREEZE}
}

func unitReleaseRequest() *pb.SqlWriterMaintenanceReceipt {
	return &pb.SqlWriterMaintenanceReceipt{
		Kind: pb.SqlWriterMaintenanceReceipt_RELEASE,
		Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{
			ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{},
		}},
	}
}

// TestFreeze_OutputAndTimeout:带时限调用 Catalog,stdout 一行统计,stderr 第一行是连接目标;时限低于下限、输入不全都不调 Catalog;
// Catalog 出错退出码 2、不出统计行。
func TestFreeze_OutputAndTimeout(t *testing.T) {
	run := func(catalog *unitMaintenanceCatalog, change func(*FreezeOptions)) cliRun {
		var out, errOut bytes.Buffer
		opts := FreezeOptions{
			Catalog: catalog, TicketID: unitTicket, Request: unitFreezeRequest(), Reason: "PITR 2026-10-18",
			Timeout: store.RouteFreezeTimeoutMin, Target: "route_tool freeze: global_db=127.0.0.1:4000/testdb", Stdout: &out, Stderr: &errOut,
		}
		if change != nil {
			change(&opts)
		}
		return cliRun{code: ExitCode(Freeze(context.Background(), opts)), stdout: out.String(), stderr: errOut.String()}
	}

	catalog := &unitMaintenanceCatalog{}
	r := run(catalog, nil)
	assert.Equal(t, ExitClean, r.code, r.stderr)
	assert.Equal(t, "route_freeze changed=true writable=0\n", r.stdout)
	assert.Equal(t, "route_tool freeze: global_db=127.0.0.1:4000/testdb\nFREEZE reason=\"PITR 2026-10-18\"\n", r.stderr)
	assert.Equal(t, 1, catalog.freezeCalls)
	assert.True(t, catalog.deadline, "the freeze runs under its own deadline")

	for name, change := range map[string]func(*FreezeOptions){
		"timeout below the floor": func(o *FreezeOptions) { o.Timeout = store.RouteFreezeTimeoutMin - time.Millisecond },
		"no catalog":              func(o *FreezeOptions) { o.Catalog = nil },
		"bad ticket":              func(o *FreezeOptions) { o.TicketID = unitTicket[:15] },
		"no request":              func(o *FreezeOptions) { o.Request = nil },
		"no writers":              func(o *FreezeOptions) { o.Stdout = nil },
	} {
		t.Run(name, func(t *testing.T) {
			catalog := &unitMaintenanceCatalog{}
			r := run(catalog, change)
			assert.Equal(t, ExitFailed, r.code)
			assert.Zero(t, catalog.freezeCalls, "refused before touching the catalog")
			assert.Empty(t, r.stdout)
		})
	}

	r = run(&unitMaintenanceCatalog{err: errors.New("Error 1205")}, nil)
	assert.Equal(t, ExitFailed, r.code)
	assert.Empty(t, r.stdout)
}

// TestUnfreeze_OutputAndTimeout:同 freeze;另外没有 P8 ReleaseReadiness 原件的请求不调 Catalog。
func TestUnfreeze_OutputAndTimeout(t *testing.T) {
	run := func(catalog *unitMaintenanceCatalog, change func(*UnfreezeOptions)) cliRun {
		var out, errOut bytes.Buffer
		opts := UnfreezeOptions{
			Catalog: catalog, TicketID: unitTicket, Request: unitReleaseRequest(),
			Timeout: store.RouteFreezeTimeoutMin, Target: "route_tool unfreeze: global_db=127.0.0.1:4000/testdb", Stdout: &out, Stderr: &errOut,
		}
		if change != nil {
			change(&opts)
		}
		return cliRun{code: ExitCode(Unfreeze(context.Background(), opts)), stdout: out.String(), stderr: errOut.String()}
	}

	catalog := &unitMaintenanceCatalog{}
	r := run(catalog, nil)
	assert.Equal(t, ExitClean, r.code, r.stderr)
	assert.Equal(t, "route_unfreeze changed=true writable=1\n", r.stdout)
	assert.Equal(t, "route_tool unfreeze: global_db=127.0.0.1:4000/testdb\n", r.stderr)
	assert.Equal(t, 1, catalog.releaseCalls)
	assert.True(t, catalog.deadline, "the release runs under its own deadline")

	for name, change := range map[string]func(*UnfreezeOptions){
		"timeout below the floor": func(o *UnfreezeOptions) { o.Timeout = store.RouteFreezeTimeoutMin - time.Millisecond },
		"no catalog":              func(o *UnfreezeOptions) { o.Catalog = nil },
		"bad ticket":              func(o *UnfreezeOptions) { o.TicketID = unitTicket[:15] },
		"no request":              func(o *UnfreezeOptions) { o.Request = nil },
		"no release readiness":    func(o *UnfreezeOptions) { o.Request = unitFreezeRequest() },
		"no writers":              func(o *UnfreezeOptions) { o.Stderr = nil },
	} {
		t.Run(name, func(t *testing.T) {
			catalog := &unitMaintenanceCatalog{}
			r := run(catalog, change)
			assert.Equal(t, ExitFailed, r.code)
			assert.Zero(t, catalog.releaseCalls, "refused before touching the catalog")
			assert.Empty(t, r.stdout)
		})
	}

	r = run(&unitMaintenanceCatalog{err: errors.New("denied")}, nil)
	assert.Equal(t, ExitFailed, r.code)
	assert.Empty(t, r.stdout)
}
