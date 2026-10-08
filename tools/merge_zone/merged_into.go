package main

// merge:merged_into:{src} —— 「这个 zone 已经被合进哪个区」的标记(player-storage-placement.md §12 A10)。
//
// 为什么要有:合服把源区玩家的 player:zone 改成 dst 之后,「源库有行 = 归属源区」就不成立了 —— copy 模式下
// 源区库里的行成了冷副本;pin 模式(player-storage-placement.md §10.1)下行还是真源,但靠 player:placement
// 记录指过去,归属已经是 dst。此时若 mapping Redis 丢了数据(整体丢失 / 误删),照 runbook 的老习惯对源区跑
// -backfill-home-zone,回填把这些人 SETNX 回源区 —— 钉回一个已下线的 zone,登不上,而且没有任何报错。标记让
// 回填认得出「这个 zone 已经合走了」并拒绝;映射丢失的正确恢复是按清单重放(runbook),不是回填。
//
// 契约(写者、读者、销毁者都在本文件):
//
//	key    : merge:merged_into:{src}
//	where  : mapping Redis(DB 0),与 player:zone / merge:in_progress 同库 —— 回填读写的就是这个库
//	value  : JSON {"dst": <zone>, "run_id": "<清单 run_id>"}
//	TTL    : 无。源区合出去之后就一直是合出去的,直到撤销
//	writer : 合服步骤 5 改映射**之前**(markMergedInto);改到一半中止时标记已在,回填同样被挡住
//	reader : -backfill-home-zone(refuseBackfillOfMergedZone,存在即拒绝);合服清单落盘之前
//	         (checkMergedIntoBeforeMerge:源区已合进别处 / 目标区自己已被合走 即拒绝)
//	delete : -mode unmerge 把映射改回源区之后(clearMergedInto),只删 dst 与 run_id 都对得上的那一把

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// mergedIntoKey 是标记的键;zone 是**被合走的那个**(源区)。
func mergedIntoKey(zone uint32) string {
	return "merge:merged_into:" + strconv.FormatUint(uint64(zone), 10)
}

// mergedIntoValue 是标记的 JSON 值。字段只增不改名(回填的拒绝文案会原样打出它)。
type mergedIntoValue struct {
	Dst   uint32 `json:"dst"`
	RunID string `json:"run_id"` // 清单的 run_id(续跑沿用首跑的那个),撤销按它认领
}

// readMergedInto 读标记。键不存在返回 (nil, "", nil);值不是本工具写的形状时返回原文与解析错误,
// 由调用方决定 fail-closed 的方式。
func readMergedInto(ctx context.Context, rdb *redis.Client, zone uint32) (*mergedIntoValue, string, error) {
	key := mergedIntoKey(zone)
	raw, err := rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("get %s: %w", key, err)
	}
	var v mergedIntoValue
	if jerr := json.Unmarshal([]byte(raw), &v); jerr != nil {
		return nil, raw, fmt.Errorf("%s holds %q, which is not a merged_into value: %w", key, raw, jerr)
	}
	return &v, raw, nil
}

// checkMergedIntoBeforeMerge 是合服在清单落盘之前(第一次写之前)的只读前置:与 markMergedInto 同一判定,
// 另加「目标区自己已被合走」。读失败 / 值读不懂同样拒绝(fail-closed):证明不了就不动手。
func checkMergedIntoBeforeMerge(ctx context.Context, rdb *redis.Client, src, dst uint32) error {
	srcCur, srcRaw, srcErr := readMergedInto(ctx, rdb, src)
	dstCur, dstRaw, dstErr := readMergedInto(ctx, rdb, dst)
	return mergedIntoConflict(src, dst, srcCur, srcRaw, srcErr, dstCur, dstRaw, dstErr)
}

// mergedIntoConflict 是 checkMergedIntoBeforeMerge 的纯判定(单测直接喂读结果)。
//   - 源区标记指向同一个 dst:续跑 / 撤销后重合同一对 zone,放行(步骤 5 会覆盖成本次 run_id)。
//   - 源区标记指向别的 zone:源区曾被合进另一个区而没有撤销,拒绝。
//   - 目标区有任何标记:目标区已下线,不能把玩家归属改过去,拒绝。
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
		return fmt.Errorf("%s = %s: target zone %d was itself merged into zone %d; refusing to re-home players into "+
			"a retired zone (merge into %d instead)", mergedIntoKey(dst), dstRaw, dst, dstCur.Dst, dstCur.Dst)
	}
	return nil
}

// markMergedInto 在改映射之前写标记。已有标记且指向同一个 dst(续跑 / 撤销后重合同一对 zone)就覆盖成
// 本次 run_id;指向别的 zone 或读不懂就拒绝 —— 那说明源区曾被合进另一个区而没有撤销,不是这次能决定的事。
// 写者只有本工具,且调用方持有 src 的合服围栏,所以「读 → 判 → 写」之间没有别的写者。
func markMergedInto(ctx context.Context, rdb *redis.Client, src, dst uint32, runID string, dryRun bool) error {
	key := mergedIntoKey(src)
	cur, raw, err := readMergedInto(ctx, rdb, src)
	if err != nil {
		return err
	}
	if cur != nil && cur.Dst != dst {
		return fmt.Errorf("%s already says zone %d was merged into zone %d (%s) — refusing to overwrite it with %d",
			key, src, cur.Dst, raw, dst)
	}
	payload, err := json.Marshal(mergedIntoValue{Dst: dst, RunID: runID})
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	if dryRun {
		log.Printf("[DRY-RUN] would SET %s %s (no TTL) before remapping", key, payload)
		return nil
	}
	if err := rdb.Set(ctx, key, payload, 0).Err(); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	log.Printf("merged_into marker set: %s = %s", key, payload)
	return nil
}

// refuseBackfillOfMergedZone 是回填的前置:zone 已被合走(标记存在)就拒绝。读失败同样拒绝(fail-closed):
// 证明不了「没合走」就不能往 mapping 里 SETNX。
func refuseBackfillOfMergedZone(ctx context.Context, rdb *redis.Client, zone uint32) error {
	key := mergedIntoKey(zone)
	raw, err := rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w — cannot prove zone %d was not merged away; refusing to backfill", key, err, zone)
	}
	return fmt.Errorf("zone %d was merged away (%s = %s): its players were re-homed by that merge (copy mode left cold copies "+
		"of their rows here; pin mode keeps their live rows here behind player:placement records), and backfilling would pin "+
		"those players back to a dead zone. If the mapping Redis lost player:zone keys, restore them by replaying the merge "+
		"manifest (runbook), not by backfill. If the merge was reverted by hand without -mode unmerge, DEL %s after confirming it",
		zone, key, raw, key)
}

// clearMergedInto 是撤销的收尾:映射改回源区之后删标记。只删 dst 与 run_id 都与清单一致的那一把 ——
// 对不上说明这把标记是另一次合服写的,撤销这一次不该越权删它(打 WARN,留给人判断)。
// 返回是否删了(dry-run 返回「会不会删」)。
func clearMergedInto(ctx context.Context, rdb *redis.Client, src, dst uint32, runID string, dryRun bool) (bool, error) {
	key := mergedIntoKey(src)
	cur, raw, err := readMergedInto(ctx, rdb, src)
	if err != nil {
		// 读不懂的值同样不删:它不是本工具按这份清单写的。
		if raw != "" {
			log.Printf("WARN: %v — left untouched", err)
			return false, nil
		}
		return false, err
	}
	if cur == nil {
		// 合服没走到步骤 5(或旧版工具合的),本来就没有标记。
		return false, nil
	}
	if cur.Dst != dst || cur.RunID != runID {
		log.Printf("WARN: %s = %s was written by another merge (manifest: dst=%d run_id=%s) — left untouched; "+
			"-backfill-home-zone -zone %d stays refused until someone who knows that merge removes it", key, raw, dst, runID, src)
		return false, nil
	}
	if dryRun {
		log.Printf("[DRY-RUN] would DEL %s", key)
		return true, nil
	}
	if err := rdb.Del(ctx, key).Err(); err != nil {
		return false, fmt.Errorf("del %s: %w", key, err)
	}
	return true, nil
}
