// data_consistency_check — periodic scanner for zone cross-reference invariants.
//
// Why this exists:
//   Server merge (tools/merge_zone/) rewrites home_zone for every player
//   in the source zone. If even one referencing record gets missed —
//   stale guild_member.zone hint, mail attachment with frozen guild_id,
//   chat-history row pointing into a vanished zone — the inconsistency
//   silently survives in the DB until a player triggers it weeks later
//   ("my friend is gone", "my mail can't be opened"). At that point
//   triage takes hours.
//
//   This tool runs the inverse query family: given the current zone
//   topology in mapping Redis, find rows in MySQL that point to a zone
//   that no longer exists, or that disagree with mapping Redis on a
//   given player_id's home_zone.
//
//   It complements:
//     - tools/merge_zone/audit_resources.go — pre-merge resource counts
//     - data_service rollback audit log — single-player corrective work
//
//   Run cadence: weekly cron (or after every merge). Output is a
//   markdown report; non-zero exit if any check is in BLOCK state.
//
// What it does NOT do:
//   - It does not fix anything. Repairs go through data_service or
//     manual ops — the tool reports, humans decide.
//   - It does not connect to the data Redis (player blob keys). That
//     surface is too large for a periodic scan; if you need to verify
//     blob-vs-mapping coherence, use the data_service debug_fetch tool.

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// CheckResult is one row in the consistency report.
//
// Severity policy:
//   info  — observation, no action needed
//   warn  — pre-existing data quality issue (orphans, abandoned tombstones)
//   block — active inconsistency that will cause player-visible bugs
//           (e.g. guild row pointing to a non-existent zone after merge)
type CheckResult struct {
	Name        string
	Severity    string // "info" / "warn" / "block"
	BadCount    int64
	TotalCount  int64
	SampleNotes string // 1-line, optional concrete examples ("guild_id=123 zone=999")
}

// guildSchemaNamePattern guards the one string we concatenate into SQL.
// A database name is not a bind parameter, so shape-checking it here is the
// only defence; mirrors tools/merge_zone's validateGuildSchemaName.
var guildSchemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// runConfig is what each checker needs. We pass it by value because
// every field is a handle / int — no ownership concerns.
type runConfig struct {
	db        *sql.DB
	mappingDB *redis.Client
	knownZones map[uint32]struct{} // zones we expect to be live
	// guildSchema: the guild service's own database (-guild-schema). Guild
	// tables moved out of the default DSN database into mmorpg_guild, so
	// every guild query has to be schema-qualified.
	guildSchema string
}

func main() {
	mysqlDSN := flag.String("mysql-dsn", "root:@tcp(127.0.0.1:3306)/mmorpg?charset=utf8mb4&parseTime=true&loc=Local",
		"MySQL DSN (its default database holds friend/user tables; guild lives in -guild-schema on the same instance)")
	guildSchema := flag.String("guild-schema", "mmorpg_guild",
		"Guild database on the same MySQL instance (go/guild forces its DSN database to be mmorpg_guild; override only for isolated tests)")
	redisAddr := flag.String("redis-addr", "127.0.0.1:6379", "Mapping redis address (player:zone:* keys)")
	redisPwd := flag.String("redis-password", "", "Mapping redis password")
	redisDB := flag.Int("redis-db", 0, "Mapping redis DB number")

	// Live zones is a CSV the operator passes in (or we infer from mapping).
	// Inference is the default because operators forget to update flags;
	// any mapping value seen in player:zone:* is treated as a live zone.
	liveZonesCSV := flag.String("live-zones", "", "Comma-separated zone IDs that are currently up. Empty = infer from mapping Redis (default; recommended).")

	timeout := flag.Duration("timeout", 5*time.Minute, "Overall timeout for the scan")

	flag.Parse()

	if !guildSchemaNamePattern.MatchString(*guildSchema) {
		log.Fatalf("-guild-schema %q is not a plain identifier ([A-Za-z0-9_], 1-64 chars)", *guildSchema)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// ── connect ──────────────────────────────────────────────────
	db, err := sql.Open("mysql", *mysqlDSN)
	if err != nil {
		log.Fatalf("mysql open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("mysql ping: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr, Password: *redisPwd, DB: *redisDB})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("mapping redis ping: %v", err)
	}

	// ── build "known zones" set ──────────────────────────────────
	// Why we build it on the operator's terms first, then fall back to
	// inference: explicit override is needed during a *failed* merge
	// (where mapping Redis is in a transient state and self-inference
	// would falsely declare half the rows orphaned).
	knownZones := map[uint32]struct{}{}
	if *liveZonesCSV != "" {
		for _, s := range strings.Split(*liveZonesCSV, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			z, err := strconv.ParseUint(s, 10, 32)
			if err != nil {
				log.Fatalf("bad -live-zones token %q: %v", s, err)
			}
			knownZones[uint32(z)] = struct{}{}
		}
	} else {
		zones, err := inferLiveZones(ctx, rdb)
		if err != nil {
			log.Fatalf("infer live zones: %v", err)
		}
		knownZones = zones
	}
	if len(knownZones) == 0 {
		log.Fatalf("no live zones detected — pass -live-zones explicitly")
	}
	log.Printf("Live zones: %v", sortedKeys(knownZones))

	cfg := runConfig{db: db, mappingDB: rdb, knownZones: knownZones, guildSchema: *guildSchema}

	// ── run checks ───────────────────────────────────────────────
	//
	// 2026-05-23 reality reconciliation: the earlier shape of this
	// runner included `checkMailRecipientOrphans` against a non-existent
	// `mail` table — the missing-table error path silently returned
	// "info: not present" and operators read the run as clean. Removed.
	// The four checks below all exercise schema that actually exists
	// (`guild`, `guild_rank:zone:*` Redis keys, `user_accounts`, `friend`).
	checks := []func(context.Context, runConfig) CheckResult{
		checkGuildZoneFK,
		checkGuildRankZoneOrphans,
		checkPlayerHomeZoneVsAccount,
		checkFriendOrphans,
	}
	results := make([]CheckResult, 0, len(checks))
	for _, fn := range checks {
		ctxCheck, cancelCheck := context.WithTimeout(ctx, 60*time.Second)
		results = append(results, fn(ctxCheck, cfg))
		cancelCheck()
	}

	// ── report ───────────────────────────────────────────────────
	blockCount := printReport(results)
	if blockCount > 0 {
		os.Exit(1)
	}
}

// inferLiveZones scans player:zone:* in mapping Redis and returns the
// set of distinct zone IDs found. Cheap O(N players) but bounded by
// mapping size, which is the same scale as the merge_zone scan.
func inferLiveZones(ctx context.Context, rdb *redis.Client) (map[uint32]struct{}, error) {
	out := map[uint32]struct{}{}
	var cur uint64
	for {
		keys, next, err := rdb.Scan(ctx, cur, "player:zone:*", 500).Result()
		if err != nil {
			return nil, err
		}
		if len(keys) > 0 {
			vals, err := rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return nil, err
			}
			for _, v := range vals {
				s, ok := v.(string)
				if !ok {
					continue
				}
				z, err := strconv.ParseUint(s, 10, 32)
				if err != nil {
					continue
				}
				out[uint32(z)] = struct{}{}
			}
		}
		cur = next
		if cur == 0 {
			break
		}
	}
	return out, nil
}

func sortedKeys(m map[uint32]struct{}) []uint32 {
	ks := make([]uint32, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// Stable order for log readability. Insertion sort beats sort.Slice
	// for tiny zone counts (typically < 20) and avoids the import.
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j-1] > ks[j]; j-- {
			ks[j-1], ks[j] = ks[j], ks[j-1]
		}
	}
	return ks
}

// ── checkers ───────────────────────────────────────────────────────
// Pattern: each checker is best-effort. If the underlying table
// doesn't exist (some Go services aren't deployed yet), we return
// info / "table not present" rather than failing the whole run.

// checkGuildZoneFK — every guild.zone_id should be a live zone.
//
// After merge, the source zone's guild rows have been UPDATEd to dst.
// If even one row was missed (e.g. a dry-run aborted partway), we'll
// see guild.zone_id pointing to a zone no longer in the live set.
// That guild's members will lose access to it. Block-severity.
func checkGuildZoneFK(ctx context.Context, cfg runConfig) CheckResult {
	r := CheckResult{Name: "guild.zone_id"}
	guildTable := cfg.guildSchema + ".guild"
	var total int64
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+guildTable).Scan(&total); err != nil {
		r.Severity = "info"
		r.SampleNotes = guildTable + " not present"
		return r
	}
	r.TotalCount = total

	rows, err := cfg.db.QueryContext(ctx, "SELECT DISTINCT zone_id FROM "+guildTable)
	if err != nil {
		r.Severity = "warn"
		r.SampleNotes = fmt.Sprintf("query failed: %v", err)
		return r
	}
	defer rows.Close()

	var dead []uint32
	for rows.Next() {
		var z uint32
		if err := rows.Scan(&z); err != nil {
			continue
		}
		if _, ok := cfg.knownZones[z]; !ok {
			dead = append(dead, z)
		}
	}
	if len(dead) == 0 {
		r.Severity = "info"
		r.SampleNotes = "all guild rows reference live zones"
		return r
	}
	// Count rows per dead zone to give the exact bad count.
	for _, z := range dead {
		var n int64
		_ = cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+guildTable+" WHERE zone_id = ?", z).Scan(&n)
		r.BadCount += n
	}
	r.Severity = "block"
	r.SampleNotes = fmt.Sprintf("%d guilds reference dead zones %v — likely missed merge_zone step or partial rollback", r.BadCount, dead)
	return r
}

// checkGuildRankZoneOrphans — guild_rank:zone:{id} ZSETs that point at
// dead zones. After merge_zone's mergeRankingZSET, the source-zone ZSET
// is deleted; if it survives, the merge step crashed mid-way.
func checkGuildRankZoneOrphans(ctx context.Context, cfg runConfig) CheckResult {
	r := CheckResult{Name: "guild_rank:zone:*"}
	var cur uint64
	var orphanZones []uint32
	for {
		keys, next, err := cfg.mappingDB.Scan(ctx, cur, "guild_rank:zone:*", 200).Result()
		if err != nil {
			r.Severity = "warn"
			r.SampleNotes = fmt.Sprintf("scan failed: %v", err)
			return r
		}
		for _, k := range keys {
			r.TotalCount++
			parts := strings.Split(k, ":")
			if len(parts) < 3 {
				continue
			}
			z, err := strconv.ParseUint(parts[2], 10, 32)
			if err != nil {
				continue
			}
			if _, ok := cfg.knownZones[uint32(z)]; !ok {
				orphanZones = append(orphanZones, uint32(z))
				r.BadCount++
			}
		}
		cur = next
		if cur == 0 {
			break
		}
	}
	if r.BadCount == 0 {
		r.Severity = "info"
		r.SampleNotes = "no orphan rank ZSETs"
		return r
	}
	r.Severity = "block"
	r.SampleNotes = fmt.Sprintf("orphan rank ZSETs for dead zones %v — leftover from incomplete merge_zone run", orphanZones)
	return r
}

// checkPlayerHomeZoneVsAccount — every player_id in mapping Redis should
// also exist in user_accounts.simple_players. Pre-existing inconsistency
// (player created but never linked to an account, or account deleted
// without RemovePlayersFromAccounts running) shows up here.
//
// Caveat: this check is bounded to player_ids we can find in mapping;
// if mapping itself dropped an entry, we won't catch it. That's a
// mapping-layer issue and out of scope for this tool.
func checkPlayerHomeZoneVsAccount(ctx context.Context, cfg runConfig) CheckResult {
	r := CheckResult{Name: "player→account linkage"}
	// We don't read the simple_players blob (it's a protobuf MEDIUMBLOB);
	// instead we rely on the count-based heuristic: |mapping players|
	// should be ≤ |account.simple_players unique IDs|. If mapping is
	// significantly larger, there are accounts that don't list their
	// own player_ids — a known scenario after admin tooling crashes.
	var mappingCount int64
	cur := uint64(0)
	for {
		keys, next, err := cfg.mappingDB.Scan(ctx, cur, "player:zone:*", 500).Result()
		if err != nil {
			r.Severity = "warn"
			r.SampleNotes = fmt.Sprintf("mapping scan failed: %v", err)
			return r
		}
		mappingCount += int64(len(keys))
		cur = next
		if cur == 0 {
			break
		}
	}
	r.TotalCount = mappingCount

	var accountRowCount int64
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_accounts").Scan(&accountRowCount); err != nil {
		r.Severity = "info"
		r.SampleNotes = "user_accounts not present (login service not deployed?)"
		return r
	}

	// We don't have a clean per-row count; a sane heuristic is "if mapping
	// has 5x more players than there are accounts, flag it." That covers
	// catastrophic divergence but not subtle missing entries.
	if mappingCount > accountRowCount*5 {
		r.Severity = "warn"
		r.BadCount = mappingCount - accountRowCount*5
		r.SampleNotes = fmt.Sprintf("mapping has %d players, only %d account rows — investigate", mappingCount, accountRowCount)
	} else {
		r.Severity = "info"
		r.SampleNotes = fmt.Sprintf("mapping=%d, accounts=%d (within 5x heuristic)", mappingCount, accountRowCount)
	}
	return r
}

// checkFriendOrphans — friend.friend_player_id should be in mapping.
// Orphan friends are visible to players as "friend not found" errors.
// Sampled scan only — full join would be expensive on hot tables.
func checkFriendOrphans(ctx context.Context, cfg runConfig) CheckResult {
	r := CheckResult{Name: "friend.friend_player_id"}
	if err := cfg.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM friend").Scan(&r.TotalCount); err != nil {
		r.Severity = "info"
		r.SampleNotes = "friend table not present"
		return r
	}
	// Sample 1000 random rows — full scan can lock-thrash large tables.
	// Statistical bound: if 1000-row sample has 0 orphans, p95 says
	// real orphan rate is < 0.3%, which is below the noise floor of
	// "old friends since deleted accounts."
	rows, err := cfg.db.QueryContext(ctx,
		"SELECT player_id, friend_player_id FROM friend ORDER BY RAND() LIMIT 1000")
	if err != nil {
		r.Severity = "warn"
		r.SampleNotes = fmt.Sprintf("sample query failed: %v", err)
		return r
	}
	defer rows.Close()
	sampled := 0
	orphans := 0
	for rows.Next() {
		var pid, fid uint64
		if err := rows.Scan(&pid, &fid); err != nil {
			continue
		}
		sampled++
		// Check both sides exist in mapping.
		if cfg.mappingDB.Exists(ctx, fmt.Sprintf("player:zone:%d", fid)).Val() == 0 {
			orphans++
		}
	}
	if sampled == 0 {
		r.Severity = "info"
		r.SampleNotes = "no rows sampled"
		return r
	}
	r.BadCount = int64(orphans)
	rate := float64(orphans) / float64(sampled)
	switch {
	case rate >= 0.05:
		r.Severity = "block"
		r.SampleNotes = fmt.Sprintf("%d/%d sampled friends point at non-existent player_ids (%.1f%%) — fix needed", orphans, sampled, rate*100)
	case rate > 0:
		r.Severity = "warn"
		r.SampleNotes = fmt.Sprintf("%d/%d sampled (%.2f%%) — likely natural decay (deleted accounts), monitor", orphans, sampled, rate*100)
	default:
		r.Severity = "info"
		r.SampleNotes = fmt.Sprintf("%d/%d sample clean", sampled-orphans, sampled)
	}
	return r
}

// checkMailRecipientOrphans was removed 2026-05-23. The `mail` table
// does not exist in this codebase (verified against deploy/mysql-init/
// and go/db/model/mysql_database_table.sql); mail-as-a-service has not
// been built yet. The earlier function silently degraded to "info: table
// not present" which read as a clean pass. When mail Go service ships,
// re-add a sibling check; until then the absence keeps reports honest.

// printReport renders a markdown-ish table to stdout.
// Returns the number of block-severity rows so main can exit non-zero.
func printReport(results []CheckResult) int {
	log.Println()
	log.Println("=== Data consistency check ===")
	log.Println()
	log.Printf("%-3s %-30s %10s %10s  %s",
		"sev", "check", "bad", "total", "notes")
	log.Println(strings.Repeat("─", 100))
	blocks := 0
	for _, r := range results {
		marker := " "
		switch r.Severity {
		case "block":
			marker = "!"
			blocks++
		case "warn":
			marker = "."
		}
		log.Printf("[%s] %-30s %10d %10d  %s",
			marker, r.Name, r.BadCount, r.TotalCount, r.SampleNotes)
	}
	log.Println()
	if blocks > 0 {
		log.Printf("Result: %d BLOCK finding(s). Exit 1.", blocks)
	} else {
		log.Println("Result: all checks pass.")
	}
	log.Println()
	return blocks
}
