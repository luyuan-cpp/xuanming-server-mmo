package main

// Server-merge player notice + force-rename stamping (post-merge).
//
// Why this is its own file:
//   The 5-step merge in main.go (guild MySQL / rank ZSET / mapping
//   remap / blob copy / name conflict probe) is structurally about
//   moving rows. The post-merge stamping below is structurally about
//   *flagging* the source players so client/login can show them a
//   one-shot UI. Different lifecycle, different failure mode (a stamp
//   miss is annoying — a row miss is data-loss), so kept separate.
//
// Wire format:
//   Two Redis keys per stamped player, written to the **LOGIN** Redis:
//
//     player_merge_notice:{playerId}     value = "<merge_ts_ms>"
//     player_force_rename:{playerId}     value = "<merge_ts_ms>"
//
//   ⚠️ 2026-09-08 修正:这两把键此前被写进 **mapping Redis**,而读它们的
//   go/login/internal/logic/clientplayerlogin/entergamelogic.go::consumePostMergeFlags
//   用的是 svcCtx.RedisClient —— go/login/etc/login.yaml 的 `Node.RedisClient.DB: 0`。
//   写 15 读 0,合服公告 UI **从来没有触发过一次**,而且失败是完全静默的
//   (consumePostMergeFlags 把 redis.Nil 当作「这个玩家没有 flag」的正常情况)。
//   现在由 -notice-redis-addr / -notice-redis-db 显式指定,默认 DB 0。
//   DB 分布表见 audit_resources.go 顶部的 auditConfig 注释。
//
//   We deliberately do NOT serialize PlayerMergeStateComp into the
//   player_database protobuf blob from this tool — doing so would
//   require pulling the entire codegen Go module into tools/merge_zone
//   (and keeping it in sync with the protoc-gen output), which is a
//   maintenance load disproportionate to the value. The login service
//   reads these flag keys directly (see go/login/internal/logic/
//   clientplayerlogin/entergamelogic.go) and copies them into
//   PlayerMergeStateComp on first login, then DELETEs them so the
//   notice/rename UI fires exactly once.
//
//   The flag-key approach also makes the "merge happened, blob lives
//   in target Redis" handoff clean: blob_migrate copies player_database
//   to target *as-is* (no merge_state field set yet), and the flag
//   keys live in the login Redis (DB 0), which is global — both the
//   source and the target zone's login instances read the same DB.
//
// Idempotency:
//   Re-running this stamp step (-apply twice) is safe — SETNX semantics
//   would reject the second write, but plain SET keeps the latest
//   timestamp, which is also fine because login reads timestamp purely
//   for audit and treats key-exists as the "show notice" signal.

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Key prefixes consumed by go/login/internal/logic/clientplayerlogin
	// — keep these strings in sync with the login-side reader. A
	// constants file would be ideal but the merge_zone tool has its own
	// go.mod and crossing it is a heavier change than the duplication
	// is worth.
	mergeNoticeKeyPrefix = "player_merge_notice:"
	forceRenameKeyPrefix = "player_force_rename:"
)

// stampPostMergeFlags writes one notice key per source-zone player.
//
// Force-rename stamping is currently a no-op: the codebase has no
// `player(name, zone_id)` table for SQL-based collision detection
// (see audit_resources.go::auditPlayerNameConflicts for the full
// reasoning). The `forceRenamePlayerIDs` parameter remains in the
// signature so this site is the single seam where future protobuf-
// blob-decoding logic plugs in once that work lands. Callers pass
// nil today; the function silently skips the rename pipeline.
//
// Inputs:
//
//	noticeRdb            — LOGIN Redis (go/login RedisClient, DB 0).
//	                       NOT the mapping Redis — see the ⚠️ note above.
//	playerIDs            — set of source-zone players (already collected).
//	forceRenamePlayerIDs — subset that should also be flagged for rename.
//	                       Pass nil until SQL-free conflict detection lands.
//	mergeTimestamp       — unix-millis to write into the value (audit).
//	dryRun               — when true, only count, do not write.
//
// Failure mode:
//
//	Best-effort: a partial stamp run leaves some players un-flagged.
//	Those players miss the one-shot notice but otherwise function
//	normally; ops can re-run -apply with -only-stamp (TODO if needed)
//	to fill the gap. We log the first error then continue rather
//	than aborting mid-stamp, because aborting halfway through is
//	strictly worse than stamping a partial set.
func stampPostMergeFlags(
	ctx context.Context,
	noticeRdb *redis.Client,
	playerIDs []uint64,
	forceRenamePlayerIDs []uint64,
	mergeTimestamp int64,
	dryRun bool,
) (noticeStamped int, renameStamped int, firstErr error) {
	if dryRun {
		// Even in dry-run we report what would be written, so ops can
		// reconcile counts before pulling the trigger.
		return len(playerIDs), len(forceRenamePlayerIDs), nil
	}
	if noticeRdb == nil {
		return 0, 0, fmt.Errorf("no notice Redis handle (-notice-redis-addr): post-merge flags not stamped")
	}

	tsStr := strconv.FormatInt(mergeTimestamp, 10)

	// 分批 pipeline:一次性把十万个 SET 塞进一条 pipeline 会让客户端把整批
	// 命令与整批回复同时留在内存里(几十 MB),而且任何一条网络错误都会让
	// 整批的成败无法区分。stampBatchSize 条一批,失败只影响这一批。
	stamp := func(prefix string, ids []uint64) (int, error) {
		done := 0
		var first error
		for _, batch := range chunkUint64(ids, stampBatchSize) {
			pipe := noticeRdb.Pipeline()
			for _, pid := range batch {
				pipe.Set(ctx, prefix+strconv.FormatUint(pid, 10), tsStr, 0)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				// 已写进去的键不回滚 —— 重复打标是幂等的(plain SET),
				// 重跑 -apply 会把漏掉的补上。
				if first == nil {
					first = fmt.Errorf("%s stamp pipeline: %w", prefix, err)
				}
				log.Printf("WARN: %s batch of %d failed: %v (continuing; re-run -apply to fill the gap)", prefix, len(batch), err)
				continue
			}
			done += len(batch)
		}
		return done, first
	}

	noticeStamped, firstErr = stamp(mergeNoticeKeyPrefix, playerIDs)
	if len(forceRenamePlayerIDs) > 0 {
		n, err := stamp(forceRenameKeyPrefix, forceRenamePlayerIDs)
		renameStamped = n
		if firstErr == nil {
			firstErr = err
		}
	}
	return noticeStamped, renameStamped, firstErr
}

// stampBatchSize 是一条 pipeline 里的 SET 条数。500 条 ≈ 20KB 请求,
// 与 scene_hot_state 的 hotStatePipelineBatch 同量级。
const stampBatchSize = 500

// stampNowUnixMs returns the current unix-millis. Wrapped so tests
// can stub if we ever add unit tests for the stamp helpers.
func stampNowUnixMs() int64 {
	return time.Now().UnixMilli()
}
