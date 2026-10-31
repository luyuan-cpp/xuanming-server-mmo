// Package routetool 是 route_tool 各子命令的逻辑(docs/design/player-route-mapping-durability-proposal.md §15):
// tidb-prepare(tidb_prepare.go)与 freeze / unfreeze(本文件)。
//
// 分层:routetoolcli 只解析参数、装配、把结果映射成退出码;本包只依赖小接口(tidbPreparer、FreezeCatalog、ReleaseCatalog),
// 单测注入假实现,不连库。stdout 只写一行统计(给脚本解析),其余进 stderr。
package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"data_service/internal/store"

	pb "proto/common/database"
	"shared/authoritysql"
)

// route_tool 的退出码。
const (
	// ExitClean 完成。
	ExitClean = 0
	// ExitFailed 失败:参数、配置、连接、查询或写入出错(含 1205),或超时。
	ExitFailed = 2
)

// ExitCode 把一次 freeze / unfreeze 的结果映射成退出码。
func ExitCode(err error) int {
	if err != nil {
		return ExitFailed
	}
	return ExitClean
}

// freeze / unfreeze 经正式维护原件冻结 / 解冻映射写者(route_meta.writable),只碰全局库。
// 同一份请求重跑是幂等的:Catalog 回永久收据,不会多翻一次。所以统计行的 changed 恒为 true,它表示这份请求已经生效。

// FreezeCatalog 只暴露正式冻结提交;生产由独立 Operator 的 authoritysql.Catalog 实现。
type FreezeCatalog interface {
	Freeze(context.Context, []byte, *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error)
}

// ReleaseCatalog 只暴露同票据及完整 ReleaseReadiness 的正式释放。
type ReleaseCatalog interface {
	Release(context.Context, []byte, *pb.SqlWriterMaintenanceReceipt) (*pb.SqlWriterMaintenanceReceipt, error)
}

// MaintenanceCatalog 是 CLI 维护连接的完整 typed 能力,普通业务 Store 不持有它。
type MaintenanceCatalog interface {
	FreezeCatalog
	ReleaseCatalog
}

var _ MaintenanceCatalog = (*authoritysql.Catalog)(nil)

// FreezeOptions 是 freeze 的参数。
type FreezeOptions struct {
	// 仅接受已固定摘要的正式请求;调用方不能从当前状态拼造排空证据。
	Catalog  FreezeCatalog
	TicketID []byte
	Request  *pb.SqlWriterMaintenanceReceipt
	// Reason 只打到 stderr(route_meta 没有记原因的列),给事后对日志。
	Reason string
	// Timeout 是冻结写本身的时限,至少 store.RouteFreezeTimeoutMin(见它的注释)。
	Timeout time.Duration
	// Target 非空时作为 stderr 第一行。
	Target string
	// Stdout 只写一行统计;Stderr 写说明。
	Stdout, Stderr io.Writer
}

// Freeze 冻结映射写者。成功时 stdout 一行 `route_freeze changed=true writable=0`;出错时不出统计行。
func Freeze(ctx context.Context, opts FreezeOptions) error {
	if opts.Catalog == nil || len(opts.TicketID) != 16 || opts.Request == nil {
		return errors.New("冻结缺少正式 Catalog、票据或固定请求")
	}
	if opts.Stdout == nil || opts.Stderr == nil {
		return errors.New("stdout / stderr writers are required")
	}
	if opts.Timeout < store.RouteFreezeTimeoutMin {
		return fmt.Errorf("freeze timeout %s is below %s", opts.Timeout, store.RouteFreezeTimeoutMin)
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	fmt.Fprintf(opts.Stderr, "FREEZE reason=%q\n", opts.Reason)
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if _, err := opts.Catalog.Freeze(wctx, opts.TicketID, opts.Request); err != nil {
		return err
	}
	// 成功原件已经包含全部角色、历史非 SQL 副作用和槽锁排空;不以延迟猜测完成。
	fmt.Fprintln(opts.Stdout, "route_freeze changed=true writable=0")
	return nil
}

// UnfreezeOptions 是 unfreeze 的参数。
type UnfreezeOptions struct {
	// 仅接受带 P8 ReleaseReadiness 原件的正式请求。
	Catalog  ReleaseCatalog
	TicketID []byte
	Request  *pb.SqlWriterMaintenanceReceipt
	// Timeout 是解冻写本身的时限,至少 store.RouteFreezeTimeoutMin。
	Timeout time.Duration
	// Target 非空时作为 stderr 第一行。
	Target string
	// Stdout 只写一行统计;Stderr 写说明。
	Stdout, Stderr io.Writer
}

// Unfreeze 解冻映射写者。成功时 stdout 一行 `route_unfreeze changed=true writable=1`;出错时不出统计行。
func Unfreeze(ctx context.Context, opts UnfreezeOptions) error {
	if opts.Catalog == nil || len(opts.TicketID) != 16 || opts.Request == nil || opts.Request.Evidence.GetReleaseReadiness() == nil {
		return errors.New("解冻缺少正式 Catalog、票据或 P8 ReleaseReadiness 原件")
	}
	if opts.Stdout == nil || opts.Stderr == nil {
		return errors.New("stdout / stderr writers are required")
	}
	if opts.Timeout < store.RouteFreezeTimeoutMin {
		return fmt.Errorf("unfreeze timeout %s is below %s", opts.Timeout, store.RouteFreezeTimeoutMin)
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if _, err := opts.Catalog.Release(wctx, opts.TicketID, opts.Request); err != nil {
		return err
	}
	fmt.Fprintln(opts.Stdout, "route_unfreeze changed=true writable=1")
	return nil
}
