package routetool

import (
	"context"
	"errors"
	"fmt"
	"io"

	"data_service/internal/store"
)

// tidb-prepare(TiDB-only,2026-10-12 G1a):对全局库的路由表设防合并并按片 / 按 zone 预切 Region
// (docs/design/player-route-mapping-durability-proposal.md §11.2「第 1a 批热点修复」)。
//
// 只碰全局库,不连 mapping Redis。MySQL 上整步跳过(没有 Region),退出码 0。语句与执行都在
// store.RouteStore.PrepareTiDB;这里只做输出与退出码。
//
// 真执行只能使用独立 schema 主体，在服务启动前完成；normal / operator 不借用 DDL 能力。
// 它属于Schema装配，不输出MAINTENANCE发行者closure；SQL池关闭失败仍由CLI返回失败。
// TiDB 建表、恢复或重建后，先预切再启动业务 writer。
// 漏跑只影响性能(回到单尾热点),不影响正确性。全部语句幂等,重跑即可。

// tidbPreparer 是本子命令对全局库的全部依赖,*store.RouteStore 天然满足(编译期断言在 routetool_test.go)。
type tidbPreparer interface {
	PrepareTiDB(ctx context.Context, req store.RouteTiDBPrepareRequest) (store.RouteTiDBPrepareReport, error)
}

// TiDBPrepareOptions 是 tidb-prepare 的参数(cmd/route_tool 的命令行一一对应)。
type TiDBPrepareOptions struct {
	// Zones 是要预切的 zone(必填);Storages 是 idx_route_storage 要预切的落点值(可空)。
	Zones, Storages []uint32
	// DryRun 只判引擎、核对聚簇、列出语句,一条 ALTER / SPLIT 都不发。命令行默认 true。
	DryRun bool
	// Target 非空时打在 stderr 第一行(连接目标,只含 host 与库名,不含 DSN / 口令)。
	Target string
	// Stdout 只写一行统计(给脚本解析);Stderr 写每一步与失败原因。
	Stdout, Stderr io.Writer
}

// TiDBPrepare 跑一次 tidb-prepare。只有完整跑完(含 MySQL 跳过)才往 Stdout 写统计行;出错时退出码为 2。
//
// stderr:Target;MySQL 时一行 SKIP;TiDB 时每步一行 PLAN <sql>(dry-run)或 DONE <sql> split_regions=<n>;
// 失败时先列出已经 DONE 的步骤,再一行 FAILED(错误里带第几步与语句)。
func TiDBPrepare(ctx context.Context, p tidbPreparer, opts TiDBPrepareOptions) (store.RouteTiDBPrepareReport, error) {
	if opts.Stdout == nil || opts.Stderr == nil {
		return store.RouteTiDBPrepareReport{}, errors.New("stdout / stderr writers are required")
	}
	if opts.Target != "" {
		fmt.Fprintln(opts.Stderr, opts.Target)
	}
	rep, err := p.PrepareTiDB(ctx, store.RouteTiDBPrepareRequest{Zones: opts.Zones, Storages: opts.Storages, DryRun: opts.DryRun})
	if err != nil {
		for _, step := range rep.Steps {
			if step.Executed {
				fmt.Fprintf(opts.Stderr, "DONE %s split_regions=%d\n", step.SQL, step.SplitRegions)
			}
		}
		fmt.Fprintf(opts.Stderr, "FAILED: %v\n", err)
		return rep, err
	}

	if rep.Skipped {
		fmt.Fprintf(opts.Stderr, "SKIP: %s is not TiDB; nothing to split (MySQL has no Regions)\n", rep.Version)
	}
	executed := 0
	for _, step := range rep.Steps {
		if step.Executed {
			executed++
			fmt.Fprintf(opts.Stderr, "DONE %s split_regions=%d\n", step.SQL, step.SplitRegions)
		} else {
			fmt.Fprintf(opts.Stderr, "PLAN %s\n", step.SQL)
		}
	}
	fmt.Fprintf(opts.Stdout, "route_tidb_prepare engine=%s skipped=%t dry_run=%t zones=%d storages=%d statements=%d executed=%d split_regions=%d\n",
		rep.Engine, rep.Skipped, opts.DryRun, distinctCount(opts.Zones), distinctCount(opts.Storages),
		len(rep.Steps), executed, rep.SplitRegions())
	return rep, nil
}

// TiDBPrepareExitCode 把一次 tidb-prepare 的结果映射成退出码:出错 2;完成或 MySQL 跳过 0。
func TiDBPrepareExitCode(_ store.RouteTiDBPrepareReport, err error) int {
	if err != nil {
		return ExitFailed
	}
	return ExitClean
}

// distinctCount 是去重后的个数(统计行与 store 规范化之后的口径一致:重复的 zone 只切一次)。
func distinctCount(ids []uint32) int {
	seen := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return len(seen)
}
