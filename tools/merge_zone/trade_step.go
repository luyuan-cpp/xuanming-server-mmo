package main

// 聚宝斋(trade)侧只有一件事:trade_listing.market_zone 改写。
//
// 为什么必须改:market_zone = 上架时卖家的归属区(服务端经 data_service 查,见 proto/trade/trade_table.proto)。
// Market.Scope=zone 时,浏览 / 详情 / 收藏只认 `market_zone = 买家归属区`。合服后源区玩家解析到 dst,如果不改写,
// 源区的全部商品对**所有人**都「不存在」—— 市场被静默下线,而且没有任何报错。
//
// 改什么、不改什么:
//   - 按清单逐条主键点更新 `UPDATE trade_listing SET market_zone = dst WHERE listing_id = ? AND market_zone = src`
//     (rewriteZoneByPrimaryKey)。只改清单里的 id:整区 UPDATE 会改到清单之外的商品,撤销找不回。
//     不用 `market_zone = src AND listing_id IN (清单)` 分批(2026-09-21 死锁审计 #17 同形问题):该条件可被规划成
//     idx_trade_listing_0(market_zone, status, ...)的 range,先锁二级项再回表锁主键;商品状态迁移是
//     「主键 FOR UPDATE → UPDATE status(改同一行的二级项)」,两边反序即成环。
//   - `seller_zone_at_listing` 不改:它是上架时归属区的原值,审计用。
//   - `trade_favorite` 没有 zone 列,不动。
//
// 与 guild 步骤(guild_step.go)的差别,每一条都是「这里确实不需要」而不是「忘了」:
//
//  1. 没有冲突探测。listing_id 是全局唯一主键,trade_listing 上没有包含 market_zone 的唯一键,改写不可能撞行。
//  2. 没有缓存失效。P1 的 trade 服不读写任何 Redis(聚宝斋 P1 规格 P1-10)。
//     **P3 若给商品加缓存,必须在这里补失效**,否则合服后缓存里还是旧 market_zone。
//
// 映射生效、缓存刷新之后,新上架的商品都落到目标区,不需要任何闸门。滞后窗口内落到源区的由 merge_run.go migrateTrade 兜底:
// 步骤开始时把「当前 market_zone=src」并进清单;改写只动清单里的 id;改写后复查源区计数,大于 0 就中止且**不标记步骤完成**,
// 重跑时先把新商品并进清单再搬;-verify-merged 再断言源区计数为 0。
//
// 库与连接:trade 独占库 mmorpg_trade(port-decisions D-14),与 zone_{N}_db 一样经同一个 -mysql-dsn 以「库名.表名」访问。
// 库名要拼进 SQL 标识符(标识符不能用占位符),所以 -trade-schema 只接受 [A-Za-z0-9_]。
//
// 未配置的口径(fail-closed):默认必做。库或表不在 → preflight P1 拒绝,一个字节都不写 ——「查不到」和「没有商品」在这里
// 分不开(-mysql-dsn 指错实例也是同一个症状)。只有显式 -skip-trade-mysql 才跳过,而且 -verify-merged 对应行会标 warn
// 「NOT VERIFIED」,弱结论必须看起来就弱。
//
// 撤销:按清单 TradeListingIDs,只把其中**当前** market_zone = dst 的改回 src;目标区原住民与已经去了第三个 zone 的商品一律不动。
//
// 规模与原子性:改写按清单 id 每行一个短事务,不撞 TiDB 大事务限制;锁冲突按 zoneRewriteRetry 有界重试。行与行之间不原子:
// 中途失败时已改的行留在 dst,步骤未完成,重跑按 `market_zone = src` 幂等补齐剩下的行。dry-run 的计数仍按 idListBatchSize
// 分批 IN —— 非锁定读,没有锁序问题。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

const (
	// defaultTradeSchema 镜像 go/trade/internal/data.DatabaseName。trade 服的配置校验
	// 强制 MySQL.DBName 等于它,所以生产上只有这一个合法值;-trade-schema 只为
	// 集成测试的一次性库留口子。merge_zone 是独立 module,不能 import go/trade,
	// 字面量由 trade_step_test.go 守住。
	defaultTradeSchema = "mmorpg_trade"
	// tradeListingTable 与 proto/trade/trade_table.proto 的 OptionTableName 一致。
	tradeListingTable = "trade_listing"
	// tradeMarketZoneColumn 是本步骤改写的唯一一列。
	tradeMarketZoneColumn = "market_zone"
	// tradeListingPKColumn 是 trade_listing 的主键(OptionPrimaryKey),点更新按它定位。
	tradeListingPKColumn = "listing_id"
)

// validateTradeSchemaName 在任何 SQL 之前验库名形状(形状定义见 merge_run.go 的 schemaNamePattern)。
func validateTradeSchemaName(schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("-trade-schema %q is not a plain identifier ([A-Za-z0-9_], 1-64 chars)", schema)
	}
	return nil
}

func tradeListingQualified(schema string) string { return schema + "." + tradeListingTable }

// ── MySQL ─────────────────────────────────────────────────────

// assertTradeListingReady 证明 schema.trade_listing 存在且带 market_zone 列。
// 库不在、表不在(trade 从没跑过 -migrate)、列不在(表结构不是本工具认识的形状)
// 都返回错误,调用方拒绝继续。
func assertTradeListingReady(ctx context.Context, db *sql.DB, schema string) error {
	if err := validateTradeSchemaName(schema); err != nil {
		return err
	}
	if err := assertSchemaExists(ctx, db, schema); err != nil {
		return err
	}
	cols, err := tableColumns(ctx, db, schema, tradeListingTable)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if c == tradeMarketZoneColumn {
			return nil
		}
	}
	return fmt.Errorf("%s has no %s column (columns: %v) — not the trade_listing shape this tool rewrites",
		tradeListingQualified(schema), tradeMarketZoneColumn, cols)
}

// collectTradeListingIDsInZone 取 market_zone = zone 的商品 id(升序)。必须在 UPDATE
// **之前**调用:改完之后就分不清哪些是搬过来的、哪些是目标区原住民,而撤销只能
// 作用于「搬过来的那些」。
func collectTradeListingIDsInZone(ctx context.Context, db *sql.DB, schema string, zone uint32) ([]uint64, error) {
	table := tradeListingQualified(schema)
	rows, err := db.QueryContext(ctx,
		"SELECT listing_id FROM "+table+" WHERE market_zone = ? ORDER BY listing_id", zone)
	if err != nil {
		return nil, fmt.Errorf("list %s with market_zone=%d: %w", table, zone, err)
	}
	defer rows.Close()
	var out []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// countTradeListingsInZone 数 market_zone = zone 的商品。改写后的复查与 -verify-merged 共用。
func countTradeListingsInZone(ctx context.Context, db *sql.DB, schema string, zone uint32) (int64, error) {
	table := tradeListingQualified(schema)
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE market_zone = ?", zone).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s with market_zone=%d: %w", table, zone, err)
	}
	return n, nil
}

// migrateTradeMarketZone 把清单 ids 里当前 market_zone = src 的商品改成 dst,返回受影响行数;
// dry-run 返回「会被改」的行数,不写。**只动清单里的 id**:不在清单里的源区商品原地不动,
// 由调用方改写后复查源区计数来拒绝继续(见 merge_run.go migrateTrade)。
// 重跑幂等:已经改过的行不再满足 market_zone = src。gate 在每个 listing_id 的单行事务之前查(nil = 不查)。
func migrateTradeMarketZone(ctx context.Context, db *sql.DB, schema string, ids []uint64, src, dst uint32, dryRun bool,
	gate writeGate) (int64, error) {
	table := tradeListingQualified(schema)
	if !dryRun {
		// 错误原样上抛:rewriteZoneByPrimaryKey 已带表.列、方向、出错主键与已改行数,再包一层只会把它们重复一遍。
		return rewriteZoneByPrimaryKey(ctx, db, table, tradeListingPKColumn, tradeMarketZoneColumn, ids, src, dst, gate)
	}
	total, err := countListingsInZoneAmong(ctx, db, schema, ids, src)
	if err != nil {
		return total, fmt.Errorf("count rewritable %s rows: %w", table, err)
	}
	log.Printf("[DRY-RUN] Would rewrite market_zone %d → %d on %d of %d manifest listings in %s",
		src, dst, total, len(ids), table)
	return total, nil
}

// countListingsInZoneAmong 数 ids 里当前 market_zone = zone 的商品(dry-run 用)。非锁定读,
// 按 idListBatchSize 分批 IN,不存在锁序问题。
func countListingsInZoneAmong(ctx context.Context, db *sql.DB, schema string, ids []uint64, zone uint32) (int64, error) {
	table := tradeListingQualified(schema)
	var total int64
	for _, batch := range chunkUint64(ids, idListBatchSize) {
		var n int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE market_zone = ? AND listing_id IN ("+inListLiteral(batch)+")", zone).Scan(&n); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// restoreTradeMarketZone 是撤销:把清单 ids 里当前 market_zone = dst 的改回 src。
// 不在清单里的(目标区原住民)与已经不在 dst 的(去了第三个 zone,不是这次合服造成的)
// 一律不动。dry-run 数出「会被改回」的行数但不写。gate 同 migrateTradeMarketZone。
func restoreTradeMarketZone(ctx context.Context, db *sql.DB, schema string, ids []uint64, src, dst uint32, dryRun bool,
	gate writeGate) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := tradeListingQualified(schema)
	if !dryRun {
		// 与改写同一个点更新函数(锁序见 rewriteZoneByPrimaryKey),方向反过来:dst → src。
		// 错误原样上抛,理由同 migrateTradeMarketZone。
		total, err := rewriteZoneByPrimaryKey(ctx, db, table, tradeListingPKColumn, tradeMarketZoneColumn, ids, dst, src, gate)
		return int(total), err
	}
	total, err := countListingsInZoneAmong(ctx, db, schema, ids, dst)
	if err != nil {
		return int(total), fmt.Errorf("count restorable %s rows: %w", table, err)
	}
	log.Printf("[DRY-RUN] Would restore market_zone=%d on %d of %d manifest listings in %s", src, total, len(ids), table)
	return int(total), nil
}

// ── 合服后验证(-verify-merged)────────────────────────────────

// verifyTradeMarketZoneDrained: trade_listing WHERE market_zone=src 必须是 0。
//
// -skip-trade-mysql 时返回 warn「NOT VERIFIED」而不是 info:没查就不能看起来像通过。
// 库 / 表不在是 INFRA(exit 2):默认口径下 trade 必须在,查不成不等于没有商品。
func verifyTradeMarketZoneDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:trade_market_zone", UniqueScope: "global"}
	if cfg.skipTrade {
		r.Severity = "warn"
		r.Notes = "NOT VERIFIED: -skip-trade-mysql was passed — trade_listing.market_zone was not checked"
		return r
	}
	if cfg.db == nil {
		return infraAudit(r.Name, "no MySQL handle")
	}
	if err := assertTradeListingReady(ctx, cfg.db, cfg.tradeSchema); err != nil {
		return infraAudit(r.Name, "%v (pass -skip-trade-mysql only if the trade service was never deployed here)", err)
	}
	table := tradeListingQualified(cfg.tradeSchema)
	var err error
	if r.SourceCount, err = countTradeListingsInZone(ctx, cfg.db, cfg.tradeSchema, cfg.src); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	if r.TargetCount, err = countTradeListingsInZone(ctx, cfg.db, cfg.tradeSchema, cfg.dst); err != nil {
		return infraAudit(r.Name, "%v", err)
	}
	if r.SourceCount != 0 {
		r.Severity = "block"
		r.Notes = fmt.Sprintf("%d %s rows still carry market_zone=%d — the trade step did not run, or listings "+
			"were created in the source zone after it", r.SourceCount, table, cfg.src)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("market_zone=%d drained; market_zone=%d holds %d listings (seller_zone_at_listing intentionally untouched)",
		cfg.src, cfg.dst, r.TargetCount)
	return r
}
