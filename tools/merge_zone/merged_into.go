package main

// 「这个 zone 已经被合进哪个区」的映射(docs/design/player-data-tidb-scaling-plan.md §7.1)。
//
// 真源是全局库的 zone_merged_into(route_store.go 的 MarkMergedInto / ClearMergedInto),映射是永久记录:源区合出去之后就一直是
// 合出去的,直到撤销。mapping Redis 的 merge:merged_into:{src} 是它的镜像,值是 JSON {"dst": <zone>, "run_id": "<清单 run_id>"},无 TTL。
//
//	writer : 合服步骤 1,库提交之后写镜像 + WAITAOF(stepMarkMergedInto)。库已写、镜像未确认时步骤不算完成,
//	         重跑走同 dst 的幂等分支、再写一次镜像
//	reader : 合服清单落盘之前的冲突检查(stepCheckMergedIntoBeforeMerge,读库);撤销的前置(checkUnmergeAllowed,读库);
//	         data_service 按库解析归属,对账 leader 把缺失的镜像补回来(只增不改)
//	delete : -mode unmerge 写回业务数据之后(stepClearMergedInto):先删库,再按完整值 compare-and-delete 镜像
//
// 连续合服(5→6→7)沿链解析,所以源区可以有人合进来;但目标区必须还在,不能把玩家合进一个已下线的 zone。

import (
	"context"
	"fmt"
	"log"
	"strconv"
)

// mergedIntoKey 是镜像的键;zone 是**被合走的那个**(源区)。
func mergedIntoKey(zone uint32) string {
	return "merge:merged_into:" + strconv.FormatUint(uint64(zone), 10)
}

// mergedIntoValue 是镜像的 JSON 值。字段只增不改名(shared/placement.EncodeMergedIntoValue 同一形状)。
type mergedIntoValue struct {
	Dst   uint32 `json:"dst"`
	RunID string `json:"run_id"` // 清单的 run_id(续跑沿用首跑的那个),撤销按它认领
}

// validRunID 限定 run_id 只含 [A-Za-z0-9_-]、非空且不超过 64 字节(zone_merged_into.run_id 的列宽与字符集)。
func validRunID(runID string) bool {
	if runID == "" || len(runID) > 64 {
		return false
	}
	for _, c := range runID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// mergedIntoConflict 是合服前置检查的纯判定(单测直接喂读结果)。读失败同样拒绝(fail-closed):证明不了就不动手。
//   - 源区映射指向同一个 dst:放行,是不是这份清单写的由 checkMergedIntoOwnedByManifest 核对。
//   - 源区映射指向别的 zone:源区曾被合进另一个区而没有撤销,拒绝。
//   - 目标区有任何映射:目标区已下线,不能把玩家合过去,拒绝(连续合服要合进链的末端)。
func mergedIntoConflict(src, dst uint32, srcCur *mergedIntoValue, srcRaw string, srcErr error,
	dstCur *mergedIntoValue, dstRaw string, dstErr error) error {
	if srcErr != nil {
		return fmt.Errorf("merged_into: cannot prove zone %d was not merged elsewhere: %w", src, srcErr)
	}
	if srcCur != nil && srcCur.Dst != dst {
		return fmt.Errorf("%s = %s: zone %d was already merged into zone %d; refusing to merge it into %d "+
			"(unmerge that merge first, or merge into %d)", mergedIntoKey(src), srcRaw, src, srcCur.Dst, dst, srcCur.Dst)
	}
	if dstErr != nil {
		return fmt.Errorf("merged_into: cannot prove target zone %d was not merged away: %w", dst, dstErr)
	}
	if dstCur != nil {
		return fmt.Errorf("%s = %s: target zone %d was itself merged into zone %d; refusing to merge into "+
			"a retired zone (merge into %d instead)", mergedIntoKey(dst), dstRaw, dst, dstCur.Dst, dstCur.Dst)
	}
	return nil
}

// routeMergedIntoValue 把库里读到的 zone_merged_into 行还原成读结果(值与原文),供 mergedIntoConflict 复用。
func routeMergedIntoValue(dst uint32, runID string, found bool) (*mergedIntoValue, string) {
	if !found {
		return nil, ""
	}
	v := &mergedIntoValue{Dst: dst, RunID: runID}
	raw, err := encodeRouteMergedIntoValue(dst, runID)
	if err != nil {
		raw = fmt.Sprintf("{dst:%d run_id:%s}", dst, runID)
	}
	return v, raw
}

// stepCheckMergedIntoBeforeMerge 是合服在清单落盘之前(第一次写之前)的映射冲突检查:读库,判定见 mergedIntoConflict。
// 续跑同样要过:链上的目标区在两次运行之间被合走时,继续改写只会把公会改到一个已下线的 zone。
func stepCheckMergedIntoBeforeMerge(ctx context.Context, rb *routeBackend, src, dst uint32) error {
	sd, sr, sf, srcErr := rb.db.ReadMergedInto(ctx, src)
	srcCur, srcRaw := routeMergedIntoValue(sd, sr, sf)
	dd, dr, df, dstErr := rb.db.ReadMergedInto(ctx, dst)
	dstCur, dstRaw := routeMergedIntoValue(dd, dr, df)
	return mergedIntoConflict(src, dst, srcCur, srcRaw, srcErr, dstCur, dstRaw, dstErr)
}

// checkMergedIntoOwnedByManifest 拒绝换清单重跑:映射已经是 src→dst,却不是这份清单写的(或者根本没带清单)。
// 放行的话步骤 1 会把 run_id 改成新清单,而新清单只收到还在源区的对象,撤销就不再是逆操作:旧清单被 checkUnmergeAllowed
// 拒绝,只撤新清单又会删掉映射,旧清单搬走的公会和商品留在目标区,源区玩家看不到。读失败同样拒绝。
func checkMergedIntoOwnedByManifest(ctx context.Context, rb *routeBackend, m *mergeManifest, src, dst uint32) error {
	cur, run, found, err := rb.db.ReadMergedInto(ctx, src)
	if err != nil {
		return err
	}
	if !found || cur != dst || (m != nil && m.RunID == run) {
		return nil // 映射指向别的 dst 由 mergedIntoConflict 拒绝
	}
	return fmt.Errorf("zone_merged_into(%d) -> %d was written by run_id %s: pass that run's -manifest-path to resume or to unmerge; "+
		"a new manifest would take over the mapping and leave the objects moved by that run behind", src, dst, run)
}

// checkResumeMergedInto 拒绝「清单说映射已写、库里却没有」的续跑。这份清单对应的合服已被撤销(或这一行被人工删过),
// 按它续跑只会把每一步都跳过、打出 Done,而什么都没合。要重新合服用一个新的 -manifest-path。
func checkResumeMergedInto(ctx context.Context, rb *routeBackend, m *mergeManifest, src, dst uint32) error {
	if !m.stepDone(stepMergedInto) {
		return nil
	}
	cur, _, found, err := rb.db.ReadMergedInto(ctx, src)
	if err != nil {
		return err
	}
	if !found || cur != dst {
		return fmt.Errorf("the manifest records step %s as done, but zone_merged_into(%d) no longer maps to zone %d: this merge was "+
			"unmerged or the row was removed by hand. Refusing to resume from it (every step would be skipped); "+
			"pass a new -manifest-path to merge again", stepMergedInto, src, dst)
	}
	return nil
}

// checkUnmergeAllowed 是撤销在第一次写之前的前置(读库,读失败同样拒绝):
//   - 目标区之后又被合走(5→6 之后又 6→7):这批公会 / 商品已经在 7 区,这里只写回还在 6 区的,结果是半撤销。必须逆序撤销;
//   - 源区的映射不是这份清单写的:另一次合服的映射不该由这一次撤销来删。
//
// 源区没有映射放行:撤销重跑(上一次已删),或合服没走到步骤 1。
func checkUnmergeAllowed(ctx context.Context, rb *routeBackend, m *mergeManifest) error {
	src, dst := m.SourceZone, m.TargetZone
	next, nextRun, merged, err := rb.db.ReadMergedInto(ctx, dst)
	if err != nil {
		return fmt.Errorf("merged_into: cannot prove target zone %d was not merged away: %w", dst, err)
	}
	if merged {
		return fmt.Errorf("target zone %d was merged into zone %d afterwards (run_id %s): unmerge that merge first, merges are undone in reverse order",
			dst, next, nextRun)
	}
	cur, curRun, found, err := rb.db.ReadMergedInto(ctx, src)
	if err != nil {
		return fmt.Errorf("merged_into: cannot read zone_merged_into(%d): %w", src, err)
	}
	if found && (cur != dst || curRun != m.RunID) {
		return fmt.Errorf("zone_merged_into(%d) = dst %d run_id %s was written by another merge (manifest: dst=%d run_id=%s): "+
			"this manifest cannot undo it", src, cur, curRun, dst, m.RunID)
	}
	return nil
}

// stepMarkMergedInto 写映射(合服步骤 1):库事务提交 → 镜像 SET + WAITAOF。镜像确认之后才返回,调用方才往下走。
// 库已写、镜像未确认时返回错误:库里的映射已经生效,重跑同 dst 走幂等分支、再写一次镜像。
// dry-run 只读库,按同一判定预报。
func stepMarkMergedInto(ctx context.Context, rb *routeBackend, src, dst uint32, markRunID string, dryRun bool) error {
	key := mergedIntoKey(src)
	if dryRun {
		cur, run, found, err := rb.db.ReadMergedInto(ctx, src)
		if err != nil {
			return err
		}
		if found && cur != dst {
			return fmt.Errorf("zone_merged_into(%d) already says zone %d was merged into zone %d (run_id %s) — refusing to overwrite it with %d",
				src, src, cur, run, dst)
		}
		log.Printf("[DRY-RUN] would record zone_merged_into(%d) = dst %d run_id %s and mirror it to %s (no TTL)", src, dst, markRunID, key)
		return nil
	}
	value, err := rb.db.MarkMergedInto(ctx, src, dst, markRunID)
	if err != nil {
		return err
	}
	if err := rb.pub.setMergedIntoMirror(ctx, src, value); err != nil {
		return fmt.Errorf("zone_merged_into(%d) is committed, but its mirror %s was not confirmed (re-run: the row takes the "+
			"idempotent branch and the mirror is written again): %w", src, key, err)
	}
	log.Printf("merged_into recorded in zone_merged_into and mirrored: %s = %s", key, value)
	return nil
}

// stepClearMergedInto 删映射(撤销的最后一步):库事务按 (src, dst, run_id = 当初合服的清单 run_id) 删 → 提交 → 镜像按完整值
// compare-and-delete。库里没删到 = 本来不在(撤销重跑);镜像照样按本次的完整值删(上一次删了库、没删镜像的也补上;
// 值不是本次的不会被删)。返回库里是否删了一行。dry-run 只读库预报。
func stepClearMergedInto(ctx context.Context, rb *routeBackend, src, dst uint32, markRunID string, dryRun bool) (bool, error) {
	key := mergedIntoKey(src)
	value, err := encodeRouteMergedIntoValue(dst, markRunID)
	if err != nil {
		return false, err
	}
	if dryRun {
		cur, run, found, err := rb.db.ReadMergedInto(ctx, src)
		if err != nil {
			return false, err
		}
		would := found && cur == dst && run == markRunID
		if would {
			log.Printf("[DRY-RUN] would DELETE zone_merged_into(%d) and compare-and-delete its mirror %s", src, key)
		}
		return would, nil
	}
	deleted, err := rb.db.ClearMergedInto(ctx, src, dst, markRunID)
	if err != nil {
		return false, err
	}
	mirrorDeleted, err := rb.pub.deleteMergedIntoMirror(ctx, src, value)
	if err != nil {
		return deleted, fmt.Errorf("zone_merged_into(%d) cleared=%v, but the compare-and-delete of its mirror %s failed (re-run: the "+
			"delete is idempotent): %w", src, deleted, key, err)
	}
	log.Printf("merged_into: zone_merged_into(%d) cleared=%v, mirror %s removed=%v", src, deleted, key, mirrorDeleted)
	return deleted, nil
}
