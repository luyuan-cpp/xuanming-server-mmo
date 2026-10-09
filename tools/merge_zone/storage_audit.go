package main

// -mode storage-audit -storage N(docs/design/player-storage-placement.md §11.1 第 5 步):只读地报告
//
//	有效落点为 N 的玩家数  = 落点记录指向 N 的(含冻结中的) + 没有记录、home == N 的(N 是 zone 库时)
//	N 库里的冷副本数       = N 库 player_database 里有行、但有效落点不是 N 的玩家(P-1:在线路径不读不写它们)
//
// 退役一个落点库(下线 zone_N_db / 旧全局库)的判据是前者为 0。另外两类行也必须先处理,本模式单独列出:
//   - 无主行:N 库有行,既没有落点记录也没有 player:zone —— 有效落点定不下来(go/db 按处理任务的 zone 回落),
//     多半是从没回填过映射的存量号;库一删,他们就没了;
//   - 畸形:落点记录或 home 畸形 —— go/db 对他们 fail-closed,工具也判断不了他们指向哪。
//
// 口径与 go/db 相同(有记录看记录,无记录看 home);行只看 player_database(每个玩家必有这一行,
// 与 -backfill-home-zone、合服守卫同一个锚)。扫描不加锁,结果是「扫描那一刻」的数;在线跑时数字会动。
//
// 退出码:0 = 审计跑完(无论能不能退役,看最后一行结论);2 = 没查成(连不上 / 库不在),结论不可信。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// storageAuditEntryParams 是 -mode storage-audit 的纯数据输入。
type storageAuditEntryParams struct {
	storage     uint32
	mysqlDSN    string
	mappingAddr string
	mappingPwd  string
	mappingDB   int
	timeout     time.Duration
}

// storageAuditReport 是审计结果。
type storageAuditReport struct {
	Storage        uint32
	ByRecord       int // 落点记录指向 N
	FrozenOnRecord int // 其中冻结中(搬库进行中:数据还在 N)
	ByHome         int // 没有记录、home == N
	// MalformedRecords:全 mapping 里畸形的落点记录(指向哪里不知道,可能就是 N)。
	MalformedRecords int
	MalformedSample  []uint64
	// 以下是 N 库 player_database 的逐行判定。
	Rows             int
	LiveRows         int // 有效落点就是 N
	ColdCopies       int // 有效落点不是 N:冷副本
	ColdSample       []uint64
	UnmappedRows     int // 既无记录也无 home
	UnmappedSample   []uint64
	UnreadableRows   int // 行主人的记录或 home 畸形
	UnreadableSample []uint64
}

// Effective 是有效落点为 N 的玩家数。
func (r storageAuditReport) Effective() int { return r.ByRecord + r.ByHome }

// RetireBlockers 返回退役 N 之前必须处理的事(空 = 可以退役)。
func (r storageAuditReport) RetireBlockers() []string {
	var out []string
	if r.Effective() > 0 {
		out = append(out, fmt.Sprintf("%d players are still placed on storage %d", r.Effective(), r.Storage))
	}
	if r.UnmappedRows > 0 {
		out = append(out, fmt.Sprintf("%d rows have neither a placement record nor a home_zone (first: %v)", r.UnmappedRows, r.UnmappedSample))
	}
	if r.UnreadableRows > 0 {
		out = append(out, fmt.Sprintf("%d rows belong to players whose placement / home_zone is malformed (first: %v)", r.UnreadableRows, r.UnreadableSample))
	}
	if r.MalformedRecords > 0 {
		out = append(out, fmt.Sprintf("%d malformed placement records exist (first: %v) — any of them may point at storage %d",
			r.MalformedRecords, r.MalformedSample, r.Storage))
	}
	return out
}

func (r storageAuditReport) String() string {
	return fmt.Sprintf("storage %d: effective=%d (by_record=%d incl. frozen=%d, by_home=%d) | player_database rows=%d "+
		"(live=%d cold_copies=%d unmapped=%d unreadable=%d) | malformed_records=%d",
		r.Storage, r.Effective(), r.ByRecord, r.FrozenOnRecord, r.ByHome, r.Rows, r.LiveRows, r.ColdCopies,
		r.UnmappedRows, r.UnreadableRows, r.MalformedRecords)
}

// storageRowVerdict 是 N 库里一行的归属判定。
type storageRowVerdict int

const (
	storageRowLive       storageRowVerdict = iota // 有效落点 == N
	storageRowCold                                // 有效落点是别的库:冷副本
	storageRowUnmapped                            // 既无记录也无 home
	storageRowUnreadable                          // 记录或 home 畸形
)

// classifyStorageRow 是纯判定:N 库里有这名玩家的行时,它是不是他的真源。口径同 go/db:记录或 home 任一畸形,
// go/db 对这名玩家的选库整体 fail-closed(parsePlacementSnapshot 报错),他的行在哪都没人读得到 → unreadable;
// 其余按 effectiveStorage(有记录看记录,无记录看 home),回落值 0 表示两者都没有 → 无主行。
func classifyStorageRow(rd placementRead, storage uint32) storageRowVerdict {
	if rd.RecordErr != nil || rd.HomeErr != nil {
		return storageRowUnreadable
	}
	switch effectiveStorage(rd.Record, rd.RecordPresent, rd.Home, rd.HomePresent, 0) {
	case 0:
		return storageRowUnmapped
	case storage:
		return storageRowLive
	default:
		return storageRowCold
	}
}

func appendSample(sample []uint64, id uint64) []uint64 {
	if len(sample) < unmappedSampleSize {
		return append(sample, id)
	}
	return sample
}

// auditStorage 是本模式的本体。
func auditStorage(ctx context.Context, db *sql.DB, rdb *redis.Client, storage uint32) (storageAuditReport, error) {
	rep := storageAuditReport{Storage: storage}
	name, ok := storeDBName(storage)
	if !ok {
		return rep, fmt.Errorf("-storage %d is not a storage id", storage)
	}
	if err := assertSchemaExists(ctx, db, name); err != nil {
		return rep, err
	}
	sp, err := collectStoragePlayers(ctx, rdb, storage)
	if err != nil {
		return rep, err
	}
	rep.ByRecord, rep.FrozenOnRecord, rep.ByHome = len(sp.ByRecord), sp.Frozen, len(sp.ByHome)
	rep.MalformedRecords = len(sp.Malformed)
	rep.MalformedSample = sampleUint64(sp.Malformed)

	rows := &mysqlPlayerIDSource{db: db, table: name + ".player_database"}
	var after uint64
	for {
		ids, err := rows.NextBatch(ctx, after, backfillBatchSize)
		if err != nil {
			return rep, err
		}
		if len(ids) == 0 {
			return rep, nil
		}
		after = ids[len(ids)-1]
		reads, err := readPlacements(ctx, rdb, ids)
		if err != nil {
			return rep, err
		}
		for i, id := range ids {
			rep.Rows++
			switch classifyStorageRow(reads[i], storage) {
			case storageRowLive:
				rep.LiveRows++
			case storageRowCold:
				rep.ColdCopies++
				rep.ColdSample = appendSample(rep.ColdSample, id)
			case storageRowUnmapped:
				rep.UnmappedRows++
				rep.UnmappedSample = appendSample(rep.UnmappedSample, id)
			default:
				rep.UnreadableRows++
				rep.UnreadableSample = appendSample(rep.UnreadableSample, id)
			}
		}
	}
}

func runStorageAuditEntry(p storageAuditEntryParams) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	log.Printf("=== Storage audit for storage %d (mapping=%s db=%d) — read-only ===", p.storage, p.mappingAddr, p.mappingDB)
	infra := func(format string, args ...any) {
		log.Printf("ERROR: "+format, args...)
		log.Printf("Storage audit could not complete. The result is NOT a clean bill of health.")
		os.Exit(2)
	}
	db, err := sql.Open("mysql", p.mysqlDSN)
	if err != nil {
		infra("mysql open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		infra("mysql ping: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: p.mappingAddr, Password: p.mappingPwd, DB: p.mappingDB})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		infra("mapping redis ping (%s db=%d): %v", p.mappingAddr, p.mappingDB, err)
	}
	rep, err := auditStorage(ctx, db, rdb, p.storage)
	if err != nil {
		infra("%v (partial: %s)", err, rep)
	}
	log.Printf("%s", rep)
	if rep.ColdCopies > 0 {
		log.Printf("cold copies (rows whose player now lives elsewhere; online paths never read them), first: %v", rep.ColdSample)
	}
	if blockers := rep.RetireBlockers(); len(blockers) > 0 {
		log.Printf("RETIREMENT CHECK: NOT SAFE to retire storage %d:", p.storage)
		for _, b := range blockers {
			log.Printf("  - %s", b)
		}
		return
	}
	log.Printf("RETIREMENT CHECK: nothing routes to storage %d any more; its %d rows are cold copies.", p.storage, rep.Rows)
}
