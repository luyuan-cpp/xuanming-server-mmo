package main

// 聚宝斋(trade)侧只有一件事:trade_listing.market_zone 改写。
//
// 为什么必须改:market_zone = 上架时卖家的 home_zone(服务端经 data_service 查,
// 见 proto/trade/trade_table.proto)。Market.Scope=zone 时,浏览 / 详情 / 收藏只认
// `market_zone = 买家 home_zone`。合服后源区玩家的 home_zone 变成 dst,如果不改写,
// 源区的全部商品对**所有人**(包括刚合过来的原源区买家)都「不存在」—— 市场被静默下线,
// 而且没有任何报错。
//
// 改什么、不改什么:
//   - `UPDATE trade_listing SET market_zone = dst WHERE market_zone = src AND listing_id IN (清单)`,
//     按 playerRowsBatchSize 分批。比规格原文多了 `listing_id IN (清单)`:整区 UPDATE 会改到
//     清单落盘之后才落到源区的商品,违反 merge_run.go 的「清单先于写」—— 撤销找不回、
//     -verify-merged 也查不出来(源区计数已经是 0)。清单完整时两种写法改到的行完全相同。
//   - `seller_zone_at_listing` 不改:它是上架时 home_zone 的原值,审计用。
//   - `trade_favorite` 没有 zone 列,不动。
//
// 与 guild 步骤(guild_step.go)的差别,每一条都是「这里确实不需要」而不是「忘了」:
//
//  1. 没有冲突探测。listing_id 是 data_service 号段发的全局唯一主键,trade_listing
//     上没有包含 market_zone 的唯一键(只有主键 + 普通索引),改写不可能撞行。
//  2. 没有缓存失效。P1 的 trade 服不读写任何 Redis(聚宝斋 P1 规格 P1-10)。
//     **P3 若给商品加缓存,必须在这里补失效**,否则合服后缓存里还是旧 market_zone。
//  3. 没有围栏读者。P1 的 trade 服不读 merge:in_progress:{zone}(P1-10),写入口只有
//     dev/test 档的 SeedListing。**P3 正式上架前必须接合服围栏**(与 data_service /
//     guild 同契约),否则合服窗口内新上架到源区的商品会漏搬。本工具的兜底(fail-closed):
//     清单阶段把「当前 market_zone=src」并进清单;改写只动清单里的 id;改写后复查源区计数,
//     大于 0 就中止且**不标记步骤完成**,重跑时先把新商品并进清单再搬;-verify-merged 再断言
//     源区计数为 0(复查之后到 mapping 翻转之间落进来的,由它拦住)。
//     这条中止**故意不释放合服围栏**,重跑前要人工核对 run_id 后 DEL,理由与文案见
//     tradeResidualAbortMessage。
//
// 库与连接:trade 独占库 mmorpg_trade(port-decisions D-14),与 zone_{N}_db 一样经同一个
// -mysql-dsn 以「库名.表名」访问。库名要拼进 SQL 标识符(标识符不能用占位符),所以
// -trade-schema 只接受 [A-Za-z0-9_]。
//
// 未配置的口径(fail-closed):默认必做。库或表不在 → preflight P1 拒绝,一个字节都不写 ——
// 「查不到」和「没有商品」在这里分不开(-mysql-dsn 指错实例也是同一个症状)。只有显式
// -skip-trade-mysql 才跳过,而且 -verify-merged 对应行会标 warn「NOT VERIFIED」,
// 弱结论必须看起来就弱。
//
// 撤销:按清单 TradeListingIDs,只把其中**当前** market_zone = dst 的改回 src;
// 目标区原住民与已经去了第三个 zone 的商品一律不动(与 restoreMappingForIDs 同口径)。
//
// 规模与原子性:改写按清单 id 每 playerRowsBatchSize 条一条 UPDATE(自动提交),单条事务
// 大小有上限,不撞 TiDB 大事务限制。批与批之间不原子:中途失败时已改的批留在 dst,步骤
// 未完成,重跑按 `WHERE market_zone = src` 幂等补齐剩下的批。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
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
)

// tradeSchemaNamePattern:MySQL 未加引号标识符里最保守的子集,长度上限 64 同 MySQL。
var tradeSchemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// validateTradeSchemaName 在任何 SQL 之前验库名形状。库名直接拼进语句,
// 这是这条拼接唯一的注入防线。
func validateTradeSchemaName(schema string) error {
	if !tradeSchemaNamePattern.MatchString(schema) {
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

// tradeResidualAbortMessage 是步骤 3b「改写后源区仍有商品」的中止文案(merge_run.go)。
//
// 这条中止是设计内、可恢复的,但它走 log.Fatal → os.Exit,runMerge 里 defer 的
// fence.release 不会执行:merge:in_progress:{src,dst} 两把围栏仍挂在**本次** run_id 上,
// 直到 TTL(-timeout+30m,下限 1h)过期。重跑会生成新 run_id,SETNX 撞上残留围栏被拒 ——
// 所以只写「原命令重跑」是错的指引。
//
// **故意不在中止前释放围栏**:走到 3b 时玩家行 / 公会已经搬过,续跑读清单、不重扫玩家。
// 此时放开围栏 = 放开源区建号与建帮:新号的 mapping 会在步骤 5 被整体翻到 dst,而它的行
// 从没拷过;新公会留在源区。围栏必须一直挂到运维确认种子入口已停、手工 DEL、立刻重跑为止,
// 把「无围栏」的窗口压到人工可控的几秒。
//
// 文案必须写全:本次 run_id(供 GET 核对,不误删别人的围栏)、两把键名、mapping Redis 位置、
// 「不 DEL 直接重跑会被拒」。字面量由 trade_step_test.go 与集成用例守住。
func tradeResidualAbortMessage(rows int64, manifestListings int, left int64, src, dst uint32,
	runID, mappingAddr string, mappingDB int) string {
	srcKey, dstKey := mergeFenceKey(src), mergeFenceKey(dst)
	return fmt.Sprintf("trade MySQL: rewrote %d of %d manifest listings, but %d listing(s) still carry market_zone=%d — "+
		"they reached the source zone after the manifest was written (P1 trade does not read the merge fence). "+
		"Nothing outside the manifest was touched and step %s is NOT marked done. "+
		"The merge fences %s and %s are deliberately LEFT IN PLACE, still owned by run_id=%s: releasing them here "+
		"would reopen account/guild creation mid-merge, and a resumed run does not re-scan players. To resume: "+
		"(1) stop whatever is creating listings (P1: dev/test SeedListing); "+
		"(2) confirm no merge_zone process is alive; "+
		"(3) on the mapping Redis (%s db=%d) run GET %s and check its run_id is %s; "+
		"(4) DEL %s %s; "+
		"(5) immediately re-run the same command — it adds the new listings to the manifest before rewriting. "+
		"Re-running without (4) is refused (\"another merge is already fencing zone\") until the fence TTL expires",
		rows, manifestListings, left, src, stepTradeMySQL,
		srcKey, dstKey, runID,
		mappingAddr, mappingDB, srcKey, runID,
		srcKey, dstKey)
}

// migrateTradeMarketZone 把清单 ids 里当前 market_zone = src 的商品改成 dst,返回受影响行数;
// dry-run 返回「会被改」的行数,不写。**只动清单里的 id**:不在清单里的源区商品原地不动,
// 由调用方改写后复查源区计数来拒绝继续(见 merge_run.go 步骤 3b)。
// 重跑幂等:已经改过的行不再满足 market_zone = src。
func migrateTradeMarketZone(ctx context.Context, db *sql.DB, schema string, ids []uint64, src, dst uint32, dryRun bool) (int64, error) {
	table := tradeListingQualified(schema)
	var total int64
	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		in := inListLiteral(batch)
		if dryRun {
			var n int64
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM "+table+" WHERE market_zone = ? AND listing_id IN ("+in+")", src).Scan(&n); err != nil {
				return total, fmt.Errorf("count rewritable %s rows: %w", table, err)
			}
			total += n
			continue
		}
		res, err := db.ExecContext(ctx,
			"UPDATE "+table+" SET market_zone = ? WHERE market_zone = ? AND listing_id IN ("+in+")", dst, src)
		if err != nil {
			return total, fmt.Errorf("rewrite %s market_zone %d → %d: %w", table, src, dst, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rows affected for %s market_zone rewrite: %w", table, err)
		}
		total += n
	}
	if dryRun {
		log.Printf("[DRY-RUN] Would rewrite market_zone %d → %d on %d of %d manifest listings in %s",
			src, dst, total, len(ids), table)
	}
	return total, nil
}

// restoreTradeMarketZone 是撤销:把清单 ids 里当前 market_zone = dst 的改回 src。
// 不在清单里的(目标区原住民)与已经不在 dst 的(去了第三个 zone,不是这次合服造成的)
// 一律不动。dry-run 数出「会被改回」的行数但不写。
func restoreTradeMarketZone(ctx context.Context, db *sql.DB, schema string, ids []uint64, src, dst uint32, dryRun bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := tradeListingQualified(schema)
	total := 0
	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		in := inListLiteral(batch)
		if dryRun {
			var n int
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM "+table+" WHERE market_zone = ? AND listing_id IN ("+in+")", dst).Scan(&n); err != nil {
				return total, fmt.Errorf("count restorable %s rows: %w", table, err)
			}
			total += n
			continue
		}
		res, err := db.ExecContext(ctx,
			"UPDATE "+table+" SET market_zone = ? WHERE market_zone = ? AND listing_id IN ("+in+")", src, dst)
		if err != nil {
			return total, fmt.Errorf("restore %s market_zone %d ← %d: %w", table, src, dst, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rows affected for %s market_zone restore: %w", table, err)
		}
		total += int(n)
	}
	if dryRun {
		log.Printf("[DRY-RUN] Would restore market_zone=%d on %d of %d manifest listings in %s", src, total, len(ids), table)
	}
	return total, nil
}

// ── 合服后验证(-verify-merged)────────────────────────────────

// verifyTradeMarketZoneDrained: trade_listing WHERE market_zone=src 必须是 0。
//
// -skip-trade-mysql 时返回 warn「NOT VERIFIED」而不是 info:没查就不能看起来像通过。
// 库 / 表不在是 INFRA(exit 2):默认口径下 trade 必须在,查不成不等于没有商品。
func verifyTradeMarketZoneDrained(ctx context.Context, cfg auditConfig) ResourceAudit {
	r := ResourceAudit{Name: "verify:trade_listing", UniqueScope: "global"}
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
			"were created in the source zone during the merge window (P1 trade does not read the merge fence)",
			r.SourceCount, table, cfg.src)
		return r
	}
	r.Severity = "info"
	r.Notes = fmt.Sprintf("market_zone=%d drained; market_zone=%d holds %d listings (seller_zone_at_listing intentionally untouched)",
		cfg.src, cfg.dst, r.TargetCount)
	return r
}
