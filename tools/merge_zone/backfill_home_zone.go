package main

// 存量玩家 player:zone 映射回填(-backfill-home-zone -zone N)。
//
// 背景(2026-09-08):生产在此之前从未在建号时写 player:zone:{id}(login 的
// CreatePlayer 注册映射另行修复)。data_service Router / login 的跨区重定向 /
// 合服 remap 全都以这个映射为准,存量玩家没有映射就意味着:
//   - login 查 GetPlayerHomeZone 得到 0,只能回退到账号 blob 里的建角 zone;
//   - 合服 RemapHomeZoneForMerge 按「值==source」改写,没有键的玩家**根本
//     不会被合过去**,合服后仍留在已下线的源区 → 登不上。
// 所以第一次合服之前,每个存量 zone 都必须跑一遍回填。
//
// 真源是 zone 自己的 MySQL:`zone_N_db.player_database`(库名规则与
// go/db/internal/config.ZoneDBName 一致,表由 proto2mysql 从
// mysql_database_table.proto 生成,主键 player_id)。有行 = 这个玩家的归属
// zone 就是 N,这是比账号 blob 更硬的证据。
//
// 写入方式与 remapPlayerMapping 同一把键(playerZoneKeyPrefix + id,值为十进制
// zone 字符串),但用 SET NX:**绝不覆盖**已有映射 —— 已存在的值可能是合服
// remap 后的新归属,覆盖回旧 zone 就是把玩家送回坟场。
//
// 幂等 / dry-run / 分批:keyset 分页读 MySQL(player_id > last ORDER BY
// player_id LIMIT batch),每批一条 pipeline;dry-run 用 EXISTS 统计将写入数。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const backfillBatchSize = 1000

// backfillEntryParams 是 -backfill-home-zone 的纯数据输入(main 管 flag,
// 本文件管语义;与 auditEntryParams 同一分层)。
type backfillEntryParams struct {
	zone        uint32
	mysqlDSN    string
	mappingAddr string
	mappingPwd  string
	mappingDB   int
	dryRun      bool
}

// runBackfillEntry 是 -backfill-home-zone 的入口。
//
// **两个 zone 都要在第一次合服之前各跑一遍**:回填只写自己 zone 库里有行的
// 玩家,目标区玩家没有映射的话,合服后 data_service 一样路由不到他们。
func runBackfillEntry(p backfillEntryParams) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	log.Printf("=== Backfill player:zone for zone %d from %s.player_database (dry-run=%v, mapping=%s db=%d) ===",
		p.zone, zoneDBName(p.zone), p.dryRun, p.mappingAddr, p.mappingDB)

	db, err := sql.Open("mysql", p.mysqlDSN)
	if err != nil {
		log.Fatalf("mysql open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("mysql ping: %v", err)
	}
	if err := assertSchemaExists(ctx, db, zoneDBName(p.zone)); err != nil {
		log.Fatalf("backfill: %v", err)
	}

	mapRdb := redis.NewClient(&redis.Options{Addr: p.mappingAddr, Password: p.mappingPwd, DB: p.mappingDB})
	defer mapRdb.Close()
	if err := mapRdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("mapping redis ping: %v", err)
	}

	rep, err := backfillHomeZone(ctx, mapRdb, newMySQLPlayerIDSource(db, p.zone), p.zone, p.dryRun)
	if err != nil {
		log.Fatalf("backfill: %v (partial progress: %s)", err, rep)
	}
	log.Printf("=== Backfill done (%s): %s ===", verbWrite(p.dryRun), rep)
	if rep.Mismatched > 0 {
		log.Printf("NOTE: %d players already map to a DIFFERENT zone and were left untouched. "+
			"That is expected for players who have already been merged out of this zone.", rep.Mismatched)
	}
}

// zoneDBName 镜像 go/db/internal/config.ZoneDBName;merge_zone 是独立 module,
// 不引主工程包。改一处必须同步另一处。
func zoneDBName(zone uint32) string {
	return fmt.Sprintf("zone_%d_db", zone)
}

// backfillReport 是回填计数汇总。
type backfillReport struct {
	RowsScanned   int // MySQL 读到的 player 行数
	Created       int // 新写入的映射数(dry-run 恒 0)
	WouldCreate   int // dry-run 下将写入的数;apply 下等于 Created
	AlreadyMapped int // 键已存在且值==zone(幂等跳过)
	Mismatched    int // 键已存在但值!=zone(保留不动,打 WARN)
}

func (r backfillReport) String() string {
	return fmt.Sprintf("rows_scanned=%d would_create=%d created=%d already_mapped=%d mismatched=%d",
		r.RowsScanned, r.WouldCreate, r.Created, r.AlreadyMapped, r.Mismatched)
}

// playerIDBatch 是纯分页函数的输入面:测试用切片替身,生产用 MySQL。
type playerIDSource interface {
	// NextBatch 返回 player_id > after 的下一批(升序),空切片表示结束。
	NextBatch(ctx context.Context, after uint64, limit int) ([]uint64, error)
}

// mysqlPlayerIDSource 从 zone_N_db.player_database 按主键 keyset 分页。
type mysqlPlayerIDSource struct {
	db    *sql.DB
	table string // 已全限定:zone_N_db.player_database
}

func newMySQLPlayerIDSource(db *sql.DB, zone uint32) *mysqlPlayerIDSource {
	return &mysqlPlayerIDSource{db: db, table: zoneDBName(zone) + ".player_database"}
}

func (s *mysqlPlayerIDSource) NextBatch(ctx context.Context, after uint64, limit int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT player_id FROM "+s.table+" WHERE player_id > ? ORDER BY player_id LIMIT ?", after, limit)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", s.table, err)
	}
	defer rows.Close()
	out := make([]uint64, 0, limit)
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// sliceSource 是测试用的内存分页源。
type sliceSource []uint64 // 必须升序

func (s sliceSource) NextBatch(_ context.Context, after uint64, limit int) ([]uint64, error) {
	var out []uint64
	for _, id := range s {
		if id > after {
			out = append(out, id)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// backfillPlanBatch 是纯规划:给定一批 id 与它们当前的映射值(""=不存在),
// 决定哪些要写、哪些跳过、哪些是冲突。抽出来是为了在没有 miniredis 的
// 独立 module 里也能单测规则本身。
func backfillPlanBatch(ids []uint64, existing []string, zoneVal string, rep *backfillReport) (toCreate []uint64) {
	for i, id := range ids {
		switch {
		case existing[i] == "":
			toCreate = append(toCreate, id)
		case existing[i] == zoneVal:
			rep.AlreadyMapped++
		default:
			rep.Mismatched++
			log.Printf("WARN: player %d already mapped to zone %s (backfill zone %s) — kept as-is", id, existing[i], zoneVal)
		}
	}
	rep.WouldCreate += len(toCreate)
	return toCreate
}

// backfillHomeZone 逐批读 id → 读现有映射 → SET NX 写缺席的。
func backfillHomeZone(ctx context.Context, mapRdb *redis.Client, src playerIDSource, zone uint32, dryRun bool) (backfillReport, error) {
	var rep backfillReport
	zoneVal := strconv.FormatUint(uint64(zone), 10)
	var after uint64
	for {
		ids, err := src.NextBatch(ctx, after, backfillBatchSize)
		if err != nil {
			return rep, err
		}
		if len(ids) == 0 {
			break
		}
		rep.RowsScanned += len(ids)
		after = ids[len(ids)-1]

		// 读现有映射(MGET 在 cluster 下会跨 slot;mapping Redis 是单实例,
		// 但为了与 remapPlayerMapping 的逐键风格一致、并让 dry-run 与 apply
		// 走同一条读路径,这里用 pipeline GET)。
		pipe := mapRdb.Pipeline()
		gets := make([]*redis.StringCmd, len(ids))
		for i, id := range ids {
			gets[i] = pipe.Get(ctx, playerZoneKeyPrefix+strconv.FormatUint(id, 10))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return rep, fmt.Errorf("pipeline get mappings: %w", err)
		}
		existing := make([]string, len(ids))
		for i := range ids {
			v, err := gets[i].Result()
			if err == nil {
				existing[i] = v
			}
		}
		toCreate := backfillPlanBatch(ids, existing, zoneVal, &rep)
		if dryRun || len(toCreate) == 0 {
			continue
		}

		wp := mapRdb.Pipeline()
		sets := make([]*redis.BoolCmd, len(toCreate))
		for i, id := range toCreate {
			// NX:读与写之间若 login 刚好注册了这个玩家,以 login 写的为准。
			sets[i] = wp.SetNX(ctx, playerZoneKeyPrefix+strconv.FormatUint(id, 10), zoneVal, 0)
		}
		if _, err := wp.Exec(ctx); err != nil {
			return rep, fmt.Errorf("pipeline setnx mappings: %w", err)
		}
		for _, c := range sets {
			if c.Val() {
				rep.Created++
			} else {
				rep.AlreadyMapped++
				rep.WouldCreate--
			}
		}
	}
	return rep, nil
}
