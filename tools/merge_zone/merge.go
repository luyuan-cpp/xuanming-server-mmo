package main

// 合服的判定与三个模式的流程。判定是纯函数,流程只依赖 mappingStore 接口,单测用内存替身;SQL 在 route_store.go。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"shared/placement"
)

// errRefused 是校验拒绝:什么都没写,重跑同一条命令结论不变。对应退出码 1。
var errRefused = errors.New("refused")

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRefused, fmt.Sprintf(format, args...))
}

// mappingRow 是 zone_merged_into 的一行。
type mappingRow struct {
	Src, Dst   uint32
	RunID      string
	MergedAtMs uint64
}

// mappingTx 是持锁事务里能做的事。
type mappingTx interface {
	// Rows 读全部映射。调用方持着 route_meta 的行锁,所有映射写者都串行在它上面,读到的就是提交时的状态。
	Rows(ctx context.Context) ([]mappingRow, error)
	Insert(ctx context.Context, src, dst uint32, runID string) error
	// Delete 按 (src, dst) 删一行,返回是否删到了。
	Delete(ctx context.Context, src, dst uint32) (bool, error)
}

// mappingStore 是全局库上的映射存取。生产实现 *routeStore,单测用内存替身。
type mappingStore interface {
	// Snapshot 不加锁读 route_meta.writable 和全部映射。只给 dry-run 和审计用,不能作为写的依据。
	Snapshot(ctx context.Context) (writable bool, rows []mappingRow, err error)
	// Locked 在一个事务里锁 route_meta 那一行和 zones 对应的映射行,复核 writable 后跑 body,body 成功才提交。
	// writable=0 回 errRefused;body 的错误原样返回,事务回滚。
	Locked(ctx context.Context, zones []uint32, body func(mappingTx) error) error
}

// mergesOf 把映射行转成 src → dst。
func mergesOf(rows []mappingRow) map[uint32]uint32 {
	merges := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		merges[row.Src] = row.Dst
	}
	return merges
}

// refuseFrozen 是 dry-run 对提交点那条 writable 复核的预报。
func refuseFrozen(writable bool) error {
	if writable {
		return nil
	}
	return refusedf("route_meta.writable=0: the route catalog is frozen for maintenance, finish or unfreeze it first")
}

// decideMerge 判定「把 src 合进 dst」:返回 true = 映射本来就是这样,不用写;error = 拒绝。
//
//   - 同 dst 已存在:幂等成功,哪怕 dst 后来又被合走了(那一行还在,src 照样解析到新的最终区)。
//   - 源区已合进别的区:拒绝。要改目标区先撤销那一次。
//   - 目标区已被合走:拒绝,直接合进它的最终区。src → dst → final 也解析得通,但链会无谓变长,
//     以后撤销 dst 那一跳时 src 也会被带回一个已经退役的区。最终区就是 src 自己时是成环,单独报。
//   - 写入后的整张表过一遍 NewMergedZones:成环或链长超过 MaxMergedZoneHops 都拒绝。表里已有坏行时同样拒绝:
//     各服务的缓存正因为这张坏表刷新失败,这时候不该再往上加东西。
func decideMerge(merges map[uint32]uint32, src, dst uint32) (alreadyMerged bool, err error) {
	if cur, merged := merges[src]; merged {
		if cur == dst {
			return true, nil
		}
		return false, refusedf("zone %d is already merged into zone %d: unmerge that first if the target should change", src, cur)
	}
	if next, merged := merges[dst]; merged {
		zones, err := placement.NewMergedZones(merges)
		if err != nil {
			return false, refusedf("target zone %d was merged into zone %d, and the mapping does not resolve: %v", dst, next, err)
		}
		final := zones.Final(dst)
		if final == src {
			return false, refusedf("zone %d -> zone %d would form a cycle: zone %d already resolves to zone %d", src, dst, dst, src)
		}
		return false, refusedf("target zone %d was merged into zone %d: merge into its final zone %d instead", dst, next, final)
	}
	after := maps.Clone(merges)
	if after == nil {
		after = map[uint32]uint32{}
	}
	after[src] = dst
	if _, err := placement.NewMergedZones(after); err != nil {
		return false, refusedf("zone %d -> zone %d would leave a mapping that does not resolve: %v", src, dst, err)
	}
	return false, nil
}

// newRunID 是这一行的审计线索:源-目标-时间戳。只用来查「这一行是哪次操作写的」,没有任何逻辑读它。
func newRunID(src, dst uint32, now time.Time) string {
	return fmt.Sprintf("%d-%d-%s", src, dst, now.UTC().Format("20060102T150405Z"))
}

// execute 分发到三个模式。
func execute(ctx context.Context, store mappingStore, o options, now time.Time, out io.Writer) error {
	switch o.mode {
	case modeMerge:
		return runMerge(ctx, store, o, now, out)
	case modeUnmerge:
		return runUnmerge(ctx, store, o, out)
	case modeAudit:
		return runAudit(ctx, store, out)
	default:
		return refusedf("unknown mode %q", o.mode)
	}
}

// runMerge 写映射。真写时判定在锁内完成,事务外的任何读都不算数;映射提交即合服完成,没有后续步骤。
func runMerge(ctx context.Context, store mappingStore, o options, now time.Time, out io.Writer) error {
	src, dst := o.sourceZone, o.targetZone
	if o.dryRun {
		writable, rows, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		if err := refuseFrozen(writable); err != nil {
			return err
		}
		already, err := decideMerge(mergesOf(rows), src, dst)
		if err != nil {
			return err
		}
		if already {
			fmt.Fprintf(out, "[DRY-RUN] zone %d is already merged into zone %d: a real run writes nothing\n", src, dst)
			return nil
		}
		fmt.Fprintf(out, "[DRY-RUN] would record zone %d -> zone %d (run_id %s)\n", src, dst, newRunID(src, dst, now))
		return nil
	}

	var already bool
	err := store.Locked(ctx, []uint32{src, dst}, func(tx mappingTx) error {
		rows, err := tx.Rows(ctx)
		if err != nil {
			return err
		}
		already, err = decideMerge(mergesOf(rows), src, dst)
		if err != nil || already {
			return err
		}
		return tx.Insert(ctx, src, dst, newRunID(src, dst, now))
	})
	if err != nil {
		return err
	}
	if already {
		fmt.Fprintf(out, "already merged: zone %d -> zone %d; nothing written\n", src, dst)
		return nil
	}
	fmt.Fprintf(out, "merged: zone %d -> zone %d. Services pick it up within two refresh periods; check the final zones with -mode audit\n", src, dst)
	return nil
}

// runUnmerge 按 (src, dst) 删一行。不要求逆序撤销:没有搬过数据,删掉哪一行,这一行之后的解析就回到哪里。
// 没有这一行是幂等成功;源区如果映射到了别的区,照样什么都不删,多打一句提示。
func runUnmerge(ctx context.Context, store mappingStore, o options, out io.Writer) error {
	src, dst := o.sourceZone, o.targetZone
	if o.dryRun {
		writable, rows, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		if err := refuseFrozen(writable); err != nil {
			return err
		}
		merges := mergesOf(rows)
		if cur, merged := merges[src]; merged && cur == dst {
			fmt.Fprintf(out, "[DRY-RUN] would delete zone %d -> zone %d\n", src, dst)
			return nil
		}
		fmt.Fprintf(out, "[DRY-RUN] %s\n", nothingToUnmerge(merges, src, dst))
		return nil
	}

	var note string
	err := store.Locked(ctx, []uint32{src, dst}, func(tx mappingTx) error {
		deleted, err := tx.Delete(ctx, src, dst)
		if err != nil || deleted {
			return err
		}
		rows, err := tx.Rows(ctx)
		if err != nil {
			return err
		}
		note = nothingToUnmerge(mergesOf(rows), src, dst)
		return nil
	})
	if err != nil {
		return err
	}
	if note != "" {
		fmt.Fprintln(out, note)
		return nil
	}
	fmt.Fprintf(out, "unmerged: zone %d -> zone %d deleted. Guilds and listings created in the target zone meanwhile stay there; "+
		"restart guild once so its ranks are rebuilt from the current mapping\n", src, dst)
	return nil
}

func nothingToUnmerge(merges map[uint32]uint32, src, dst uint32) string {
	if cur, merged := merges[src]; merged {
		return fmt.Sprintf("nothing to delete: zone %d maps to zone %d, not zone %d", src, cur, dst)
	}
	return fmt.Sprintf("nothing to delete: zone %d is not merged", src)
}

// runAudit 只读:列出所有映射和每个源区的最终区。解析不通(成环、链太长)报拒绝退出码,让人先去看这张表:
// 这种表各服务的缓存也刷新不了。
func runAudit(ctx context.Context, store mappingStore, out io.Writer) error {
	writable, rows, err := store.Snapshot(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "route_meta.writable: %t\nmerged zones: %d\n", writable, len(rows))
	zones, resolveErr := placement.NewMergedZones(mergesOf(rows))
	if len(rows) > 0 {
		fmt.Fprintf(out, "%-10s %-10s %-10s %-24s %s\n", "src_zone", "dst_zone", "final_zone", "merged_at_utc", "run_id")
	}
	finals := map[uint32]bool{}
	for _, row := range rows {
		final := "unresolved"
		if resolveErr == nil {
			finals[zones.Final(row.Src)] = true
			final = fmt.Sprint(zones.Final(row.Src))
		}
		mergedAt := time.UnixMilli(int64(row.MergedAtMs)).UTC().Format(time.RFC3339)
		fmt.Fprintf(out, "%-10d %-10d %-10s %-24s %s\n", row.Src, row.Dst, final, mergedAt, row.RunID)
	}
	if resolveErr != nil {
		return refusedf("zone_merged_into does not resolve, services cannot refresh their caches until it is repaired: %v", resolveErr)
	}
	for _, final := range slices.Sorted(maps.Keys(finals)) {
		fmt.Fprintf(out, "final zone %d includes zones %v\n", final, zones.Members(final))
	}
	return nil
}
