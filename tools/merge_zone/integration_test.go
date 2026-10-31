//go:build merge_integration

package main

// 受控本机TiDB与真实Redis集成测试。
//
// 运行入口：go test -tags merge_integration -count=1 -v ./...
// MERGE_ZONE_IT_TIDB_DSN：显式注入已核任务所有的数字loopback TCP实例基址，无库名/参数；不默认连接任何端口。
// MERGE_ZONE_IT_MYSQL_DSN：旧参数别名，仍必须通过真实TiDB与CHECK探针，不能回退MySQL引擎。
// MERGE_ZONE_IT_REDIS_ADDR：Redis地址，DB9/10/11必须原本为空；要求Redis>=7.2且AOF已启用。
// MERGE_ZONE_IT_ROUTE_DB_DSN：旧参数仅接受同TiDB入口的merge_zone_it_route独占库，库由本套按DS权威DDL创建。
// MERGE_ZONE_IT_REQUIRE=1：依赖缺失逐例FAIL；默认逐例SKIP。验收必须设1，不能把SKIP报通过。
// 凭据只从环境变量传入，不写日志。夹具不修改GLOBAL变量，不启动/停止基础设施。
//
// TestMain只在探针全部通过后创建、结束时删除固定一次性库：zone_901_db、zone_902_db、
// merge_zone_it_db/_guild/_trade/_friend/_route。所有业务真实库均不触碰。
// 路由表直接读取go/data_service/internal/store/schema.go权威DDL，保持聚簇主键、CHECK与64行片一致。
// MERGE_ZONE_IT_SQL_DOMAIN_ONLY=1时只运行TestIT_SQLDomain*，使用真实sqltest自有随机schema；不连Redis、不构建CLI。
//
// 实际写都要TOOL身份与真实NORMAL冻结：公会 / 商品 / zone_merged_into 的写走 sqltest 夹具的真实 Freeze(itSQLMaintenanceDomain);
// 只碰 Redis 的写用本机 Session 接缝(testMaintenanceContext)。端到端用例在进程内跑真实步骤(TestIT_EndToEnd_MergeVerifyUnmerge):
// 真二进制的 -apply 要正式 P0 原件,这里只跑它的 -dry-run / 拒绝路径 / 审计,不以假原件补票。
// 每个集成测试第一句requireIT(t)，普通构建中的AST守卫检查此约束；未运行任何用例不得声称通过。
import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/drainproof"
)

// itDomain 是 SQL 域夹具:真实自有 schema / 注册 / 退休 / NORMAL 冻结;TOOL 仅注入本机 Session 接缝。
// 不构造完整 P0,不发布 MAINTENANCE 原件,也不代替全 CLI runner。
type itDomain struct {
	ctx      context.Context
	owner    *maintenanceTool
	freeze   *normalFreezeProof
	db       *sql.DB          // 工具的业务写池(fixture.Operator),以「库名.表名」访问
	operator *sql.DB          // 强读冻结收据用,与 db 同一个池
	seed     *sql.DB          // 造数据的池,默认库 = biz
	store    *mergeRouteStore // 默认库 = route(player_route / zone_merged_into)
	biz      string           // 业务表所在 schema:guild / guild_member / trade_listing
	route    string
}

// gate 是与生产 runMerge 同形的写判据:工具身份有效、冻结收据仍是当前结局。
func (d *itDomain) gate() writeGate {
	return func() error {
		if err := checkMaintenanceTool(d.ctx); err != nil {
			return err
		}
		return d.freeze.check(d.ctx, d.operator)
	}
}

func (d *itDomain) exec(t *testing.T, stmt string, args ...any) {
	t.Helper()
	if _, err := d.seed.ExecContext(d.ctx, stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func itSQLMaintenanceDomain(t *testing.T) *itDomain {
	t.Helper()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "merge_domain_" + hex.EncodeToString(nonce[:])
	authority, biz, route := prefix+"_a", prefix+"_s", prefix+"_t"
	opts, err := sqltest.OptionsFromEnv(itMySQLBase, authority, biz, route)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	fixture, err := sqltest.New(base, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PrepareOwnedSchema(base, biz, func(ctx context.Context, db *sql.DB) error {
		for _, ddl := range itBusinessTablesDDL("") {
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.PrepareOwnedSchema(base, route, func(ctx context.Context, db *sql.DB) error {
		for _, name := range []string{"playerRouteBootstrapDDL", "zoneMergedIntoBootstrapDDL"} {
			ddl, err := itRouteDDL(name)
			if err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seed, err := fixture.OpenSchema(base, biz)
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := fixture.OperatorDSN(route)
	if err != nil {
		t.Fatal(err)
	}
	routeDB, err := authoritysql.Open(base, dsn, route, authoritysql.BackendTiDB, 4, fixture.OperatorPermission())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = routeDB.Close() })
	frozen, err := fixture.Freeze(base)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Evidence == nil || frozen.Evidence.GetTicket() == nil {
		t.Fatal("缺正式NORMAL ticket evidence")
	}
	proof := &normalFreezeProof{runID: frozen.RunId, ticketID: frozen.Evidence.GetTicket().TicketId,
		requestID: frozen.RequestId, receiptDigest: frozen.ReceiptDigest, epoch: frozen.AfterEpoch, authoritySchema: authority}
	owner := &maintenanceTool{session: drainproof.New(), check: func(ctx context.Context) error { return proof.check(ctx, fixture.Operator) }}
	ctx := context.WithValue(base, maintenanceContextKey{}, owner)
	ctx = withNormalFreezeProof(ctx, proof)
	owner.context = ctx
	if err := checkMaintenanceTool(ctx); err != nil {
		t.Fatal(err)
	}
	store := &mergeRouteStore{db: routeDB, sleep: sleepCtx, backoff: routeLockRetryBackoff}
	return &itDomain{ctx: ctx, owner: owner, freeze: proof, db: fixture.Operator, operator: fixture.Operator, seed: seed, store: store, biz: biz, route: route}
}

// ── 公会 / 商品 / 映射的写:真实冻结 ─────────────────────────────

func TestIT_SQLDomainMaintenanceGuildRewriteUsesRealNormalFreeze(t *testing.T) {
	requireIT(t)
	d := itSQLMaintenanceDomain(t)
	ctx, db := d.ctx, d.db
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (11,'a',901),(12,'b',901),(21,'c',902)")
	gids, err := collectGuildIDsInZone(ctx, db, d.biz, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(gids) != "[11 12]" {
		t.Fatalf("guild ids = %v, want [11 12]", gids)
	}

	// 清单收集之后才进源区的公会:改写只动清单里的 id,它必须原地不动,由改写后的复查(countGuildsInZone)拦住 ——
	// merge_run.go migrateGuilds 据此中止且不标记完成。
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (13,'late',901)")

	if n, err := migrateGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, true, nil); err != nil || n != 2 {
		t.Fatalf("dry-run: n=%d err=%v, want 2 (only the manifest guilds)", n, err)
	}
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 901"); n != 3 {
		t.Fatalf("dry-run wrote: %d rows still in 901, want 3", n)
	}
	if n, err := migrateGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 2 {
		t.Fatalf("apply: n=%d err=%v", n, err)
	}
	if d.owner.session.Pending() != 0 {
		t.Fatal("单行事务提交/回滚返回前不能settle")
	}
	if left, err := countGuildsInZone(ctx, db, d.biz, itSrcZone); err != nil || left != 1 {
		t.Fatalf("source zone left=%d err=%v, want 1 (guild 13 is outside the manifest)", left, err)
	}

	// 续跑口径:重新收集并进清单,再改写;已改过的 11 / 12 不再命中。
	more, err := collectGuildIDsInZone(ctx, db, d.biz, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	gids = sortedUint64(append(gids, more...))
	if n, err := migrateGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 1 {
		t.Fatalf("resume apply: n=%d err=%v, want 1 row", n, err)
	}
	if left, err := countGuildsInZone(ctx, db, d.biz, itSrcZone); err != nil || left != 0 {
		t.Errorf("source zone left=%d err=%v, want 0", left, err)
	}
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 902"); n != 4 {
		t.Errorf("target zone has %d guilds, want 4", n)
	}
	// 重跑幂等。
	if n, err := migrateGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 0 {
		t.Errorf("re-run: n=%d err=%v, want 0 rows", n, err)
	}

	// 撤销:只改清单 id 里当前在 dst 的;原住民 21 不在清单里,不动。
	if n, err := restoreGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, true, nil); err != nil || n != 3 {
		t.Fatalf("restore dry-run: n=%d err=%v, want 3", n, err)
	}
	if n, err := restoreGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 3 {
		t.Fatalf("restore: n=%d err=%v, want 3", n, err)
	}
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 901"); n != 3 {
		t.Errorf("source zone has %d guilds after restore, want 3", n)
	}
	var zone21 uint32
	if err := db.QueryRowContext(ctx, "SELECT zone_id FROM "+d.biz+".guild WHERE guild_id = 21").Scan(&zone21); err != nil {
		t.Fatal(err)
	}
	if zone21 != itDstZone {
		t.Errorf("target-zone native guild moved by the restore: zone_id=%d", zone21)
	}
	if n, err := restoreGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 0 {
		t.Errorf("restore re-run: n=%d err=%v, want 0 rows", n, err)
	}

	d.owner.session.CloseAdmission()
	if err := d.owner.session.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := migrateGuildZone(ctx, db, d.biz, gids, itSrcZone, itDstZone, false, nil); !errors.Is(err, drainproof.ErrClosed) {
		t.Fatalf("TOOL闭合后不得新BEGIN: %v", err)
	}
}

func TestIT_SQLDomainMaintenanceRejectsDriftBeforeWrite(t *testing.T) {
	requireIT(t)
	d := itSQLMaintenanceDomain(t)
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (11,'a',901)")
	proof := *d.freeze
	proof.receiptDigest = append([]byte(nil), proof.receiptDigest...)
	proof.receiptDigest[0] ^= 1
	bad := withNormalFreezeProof(d.ctx, &proof)
	if _, err := migrateGuildZone(bad, d.db, d.biz, []uint64{11}, itSrcZone, itDstZone, false, nil); !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("事务内真实冻结原件漂移必须拒写: %v", err)
	}
	if n := itCount(t, d.db, d.biz+".guild WHERE zone_id = 902"); n != 0 {
		t.Fatalf("漂移后必须零写: %d", n)
	}
	d.owner.session.CloseAdmission()
	if err := d.owner.session.Wait(d.ctx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("拒绝/rollback不得清除HOLD: %v", err)
	}
}

// TestIT_SQLDomainMergedIntoRoundTrip:zone_merged_into 的写在真实冻结下走完:写入、幂等、拒绝规则、连续合服、按 run_id 删除。
func TestIT_SQLDomainMergedIntoRoundTrip(t *testing.T) {
	requireIT(t)
	d := itSQLMaintenanceDomain(t)
	ctx, st := d.ctx, d.store
	const run = "901-902-20261030T000000Z-aaaaaaaa"
	value, err := st.MarkMergedInto(ctx, itSrcZone, itDstZone, run)
	if err != nil || value != `{"dst":902,"run_id":"`+run+`"}` {
		t.Fatalf("MarkMergedInto = (%q, %v)", value, err)
	}
	if _, err := st.MarkMergedInto(ctx, itSrcZone, itDstZone, run); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	dst, gotRun, found, err := st.ReadMergedInto(ctx, itSrcZone)
	if err != nil || !found || dst != itDstZone || gotRun != run {
		t.Fatalf("ReadMergedInto = (%d, %q, %v, %v)", dst, gotRun, found, err)
	}
	// 源区不能再合进别的区;已被合走的区不能当目标。
	if _, err := st.MarkMergedInto(ctx, itSrcZone, 903, run); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("another target: %v", err)
	}
	if _, err := st.MarkMergedInto(ctx, 900, itSrcZone, run); !errors.Is(err, errRouteMergedIntoConflict) {
		t.Fatalf("a merged-away target: %v", err)
	}
	// 连续合服:目标区可以再往下合。
	if _, err := st.MarkMergedInto(ctx, itDstZone, 903, "902-903-20261030T000000Z-bbbbbbbb"); err != nil {
		t.Fatalf("chained merge: %v", err)
	}
	// 只删 (src, dst, run_id) 都对得上的那一行。
	if deleted, err := st.ClearMergedInto(ctx, itSrcZone, itDstZone, "901-902-20200101T000000Z-00000000"); err != nil || deleted {
		t.Fatalf("clear with another run id = (%v, %v)", deleted, err)
	}
	if deleted, err := st.ClearMergedInto(ctx, itSrcZone, itDstZone, run); err != nil || !deleted {
		t.Fatalf("clear = (%v, %v)", deleted, err)
	}
	if _, _, found, err := st.ReadMergedInto(ctx, itSrcZone); err != nil || found {
		t.Fatalf("the row must be gone: found=%v err=%v", found, err)
	}
	if _, _, found, _ := st.ReadMergedInto(ctx, itDstZone); !found {
		t.Fatal("the other merge's row must stay")
	}
}

// TestIT_SQLDomainMigrateGuildZone_ConcurrentDisbandDoesNotDeadlock 是审计 #17 的确定性交错回归(2026-09-21)。
//
// 修复前的环:T2 = guild 服 DisbandGuild(12):RC 事务,`SELECT ... WHERE guild_id = 12 FOR UPDATE` 持主键 12 的 X;T1 = 合服整区
// `UPDATE guild SET zone_id = dst WHERE zone_id = src` 沿 idx_guild_0 锁到 (901,12) 的二级项 → 回表请求主键 12,被 T2 挡住;
// T2 `DELETE FROM guild WHERE guild_id = 12` 要改 idx_guild_0 的 (901,12),撞上 T1 的显式 X → 1213。
//
// 修复后 T1 逐条按主键改:改完 11 即提交,轮到 12 时在主键上排队、此前什么都不持有;T2 的 DELETE 立即完成,提交后 T1 等到的是
// 已删除的行,影响 0 行。编排:T2 先锁住 12 → 起 T1 → 在 data_lock_waits 里**看见** T1 排进 guild 表的锁等待队列 → T2 DELETE + 提交 → 两边都不得 1213。
func TestIT_SQLDomainMigrateGuildZone_ConcurrentDisbandDoesNotDeadlock(t *testing.T) {
	requireIT(t)
	d := itSQLMaintenanceDomain(t)
	ctx, mergeDB := d.ctx, d.db
	guild := guildQualified(d.biz, guildTable)
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (11,'a',901),(12,'b',901),(21,'c',902)")
	gids, err := collectGuildIDsInZone(ctx, mergeDB, d.biz, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}

	// T2:guild 服写事务是 RC(guild_manage_repo.go inTx)。任何提前失败的路径都必须放掉主键锁,否则 T1 会挂到超时。
	disband, err := d.seed.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer disband.Rollback()
	var zone uint32
	if err := disband.QueryRowContext(ctx, "SELECT zone_id FROM "+guild+" WHERE guild_id = ? FOR UPDATE", 12).Scan(&zone); err != nil {
		t.Fatalf("fixture: lock guild 12: %v", err)
	}

	type result struct {
		n   int64
		err error
	}
	mergeDone := make(chan result, 1)
	go func() {
		n, err := migrateGuildZone(ctx, mergeDB, d.biz, gids, itSrcZone, itDstZone, false, nil)
		mergeDone <- result{n, err}
	}()
	if err := itAwaitLockWaiters(ctx, d.seed, d.biz, guildTable, 1, 10*time.Second); err != nil {
		_ = disband.Rollback()
		<-mergeDone
		t.Fatalf("fixture: the merge rewrite should queue behind guild 12's primary-key lock: %v", err)
	}

	// 修复前成环的时刻:T2 删行、顺带删 idx_guild_0 的二级项。
	if _, err := disband.ExecContext(ctx, "DELETE FROM "+guild+" WHERE guild_id = ?", 12); err != nil {
		_ = disband.Rollback()
		<-mergeDone
		if isITDeadlock(err) {
			t.Fatalf("DisbandGuild's DELETE deadlocked with the merge rewrite (1213) — did the rewrite go back to "+
				"walking idx_guild_0? %v", err)
		}
		t.Fatalf("disband DELETE: %v", err)
	}
	if err := disband.Commit(); err != nil {
		<-mergeDone
		t.Fatalf("disband commit: %v", err)
	}

	r := <-mergeDone
	if isITDeadlock(r.err) {
		t.Fatalf("the merge rewrite was chosen as a deadlock victim (1213): %v", r.err)
	}
	if r.err != nil {
		t.Fatalf("merge rewrite: %v", r.err)
	}
	if r.n != 1 {
		t.Errorf("rewrote %d rows, want 1 (guild 11; guild 12 was disbanded while the merge waited)", r.n)
	}
	if left, err := countGuildsInZone(ctx, mergeDB, d.biz, itSrcZone); err != nil || left != 0 {
		t.Errorf("source zone left=%d err=%v, want 0", left, err)
	}
	if n := itCount(t, mergeDB, guild+" WHERE guild_id = 12"); n != 0 {
		t.Errorf("guild 12 survived its disband")
	}
	if n := itCount(t, mergeDB, guild+" WHERE zone_id = 902"); n != 2 {
		t.Errorf("target zone has %d guilds, want 2 (11 merged + 21 native)", n)
	}
}

func TestIT_SQLDomainTradeMarketZoneRewriteVerifyRestore(t *testing.T) {
	requireIT(t)
	d := itSQLMaintenanceDomain(t)
	ctx, db := d.ctx, d.db
	seed := func(id, seller uint64, marketZone, sellerZone uint32) {
		t.Helper()
		itSeedListing(t, d.seed, d.biz, id, seller, marketZone, sellerZone)
	}
	seed(7001, 9901, itSrcZone, itSrcZone)
	seed(7002, 9902, itSrcZone, 900)       // 更早一次合服搬进 901 的商品
	seed(7101, 9950, itDstZone, itDstZone) // 目标区原住民

	// 门禁:库在且表在才放行;库不在、库在表不在都拒绝。
	if err := assertTradeListingReady(ctx, db, d.biz); err != nil {
		t.Fatalf("the trade table exists but was refused: %v", err)
	}
	if err := assertTradeListingReady(ctx, db, "merge_zone_it_no_such_trade"); err == nil {
		t.Error("a missing trade schema must be refused (fail-closed)")
	}
	if err := assertTradeListingReady(ctx, db, d.route); err == nil {
		t.Error("a schema without trade_listing (trade never migrated) must be refused")
	}

	ids, err := collectTradeListingIDsInZone(ctx, db, d.biz, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids) != "[7001 7002]" {
		t.Fatalf("source listing ids = %v, want [7001 7002]", ids)
	}

	cfg := auditConfig{db: db, tradeSchema: d.biz, guildSchema: d.biz, src: itSrcZone, dst: itDstZone}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
		t.Errorf("before the rewrite the verifier must block on real data: %+v", r)
	}

	// 清单收集之后才落到源区的商品:改写只动清单里的 id,它必须原地不动。
	seed(7003, 9903, itSrcZone, itSrcZone)

	// dry-run 只数清单里的,不写。
	if n, err := migrateTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, true, nil); err != nil || n != 2 {
		t.Fatalf("dry-run: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, d.biz+".trade_listing WHERE market_zone = 901"); n != 3 {
		t.Fatalf("dry-run wrote: %d listings left in the source zone, want 3", n)
	}

	if n, err := migrateTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 2 {
		t.Fatalf("apply: n=%d err=%v", n, err)
	}
	if mz, _ := itListingZones(t, db, d.biz, 7003); mz != itSrcZone {
		t.Errorf("listing 7003 is not in the manifest but was rewritten to market_zone=%d", mz)
	}
	// 复查口径:源区剩 1 条,merge_run.go migrateTrade 据此中止且不标记完成;-verify-merged 也必须拦住。
	if left, err := countTradeListingsInZone(ctx, db, d.biz, itSrcZone); err != nil || left != 1 {
		t.Fatalf("source zone left=%d err=%v, want 1 (the listing outside the manifest)", left, err)
	}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
		t.Errorf("a listing left in the source zone must block the verifier: %+v", r)
	}

	// 续跑口径:重新收集并进清单,再改写。
	more, err := collectTradeListingIDsInZone(ctx, db, d.biz, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	ids = sortedUint64(append(ids, more...))
	if fmt.Sprint(ids) != "[7001 7002 7003]" {
		t.Fatalf("merged manifest ids = %v, want [7001 7002 7003]", ids)
	}
	if n, err := migrateTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 1 {
		t.Fatalf("resume apply: n=%d err=%v, want 1 row", n, err)
	}
	if n := itCount(t, db, d.biz+".trade_listing WHERE market_zone = 901"); n != 0 {
		t.Errorf("%d listings left in the source zone", n)
	}
	if n := itCount(t, db, d.biz+".trade_listing WHERE market_zone = 902"); n != 4 {
		t.Errorf("target zone has %d listings, want 4", n)
	}
	// 空清单什么都不改。
	if n, err := migrateTradeMarketZone(ctx, db, d.biz, nil, itSrcZone, itDstZone, false, nil); err != nil || n != 0 {
		t.Errorf("empty manifest: n=%d err=%v, want 0 rows", n, err)
	}
	// seller_zone_at_listing 是审计原值,合服不改。
	if _, at := itListingZones(t, db, d.biz, 7001); at != itSrcZone {
		t.Errorf("seller_zone_at_listing of 7001 = %d, want %d (must not be rewritten)", at, itSrcZone)
	}
	if _, at := itListingZones(t, db, d.biz, 7002); at != 900 {
		t.Errorf("seller_zone_at_listing of 7002 = %d, want 900 (must not be rewritten)", at)
	}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "info" {
		t.Errorf("after the rewrite the verifier must pass: %+v", r)
	}
	// 重跑幂等。
	if n, err := migrateTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 0 {
		t.Errorf("re-run: n=%d err=%v, want 0 rows", n, err)
	}

	// 撤销:只改清单 id 里当前在 dst 的;原住民 7101 不在清单里,不动。
	if n, err := restoreTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, true, nil); err != nil || n != 3 {
		t.Fatalf("restore dry-run: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, d.biz+".trade_listing WHERE market_zone = 902"); n != 4 {
		t.Fatalf("restore dry-run wrote: target zone has %d listings", n)
	}
	if n, err := restoreTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 3 {
		t.Fatalf("restore: n=%d err=%v", n, err)
	}
	for _, id := range []uint64{7001, 7002, 7003} {
		if mz, _ := itListingZones(t, db, d.biz, id); mz != itSrcZone {
			t.Errorf("listing %d market_zone = %d after restore, want %d", id, mz, itSrcZone)
		}
	}
	if mz, _ := itListingZones(t, db, d.biz, 7101); mz != itDstZone {
		t.Errorf("target-zone native listing moved by the restore: market_zone=%d", mz)
	}
	if n, err := restoreTradeMarketZone(ctx, db, d.biz, ids, itSrcZone, itDstZone, false, d.gate()); err != nil || n != 0 {
		t.Errorf("restore re-run: n=%d err=%v, want 0 rows", n, err)
	}
}

// ── 真实Redis工厂 / 返回边界(SQL资格读取通过测试接缝注入,不声称覆盖正式P0/NORMAL票据)────

func TestIT_MaintenanceRedisFactoryRejectsAfterPermanentClose(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, owner := testMaintenanceContext()
	client := mustDial(ctx, "it", itRedisAddr, "", itMappingRD)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	key := "merge:it:tool:closed"
	if err := client.Set(context.Background(), key, "before", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if owner.session.Pending() != 0 {
		t.Fatal("命令真实返回后必须settle")
	}
	owner.session.CloseAdmission()
	if err := client.Set(context.Background(), key, "after", 0).Err(); !errors.Is(err, drainproof.ErrClosed) {
		t.Fatalf("闭合后必须拒新命令: %v", err)
	}
	control := itRedis(t, itMappingRD)
	value, err := control.Get(context.Background(), key).Result()
	if err != nil || value != "before" {
		t.Fatalf("被拒命令不得到达真实Redis: %q %v", value, err)
	}
	if err := owner.session.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIT_MaintenanceRedisPipelineErrorHoldsActualPartialResult(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, owner := testMaintenanceContext()
	client := mustDial(ctx, "it", itRedisAddr, "", itMappingRD)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := client.Set(ctx, "merge:it:tool:wrongtype", "string", 0).Err(); err != nil {
		t.Fatal(err)
	}
	pipe := client.Pipeline()
	good := pipe.Set(ctx, "merge:it:tool:partial", "sent", 0)
	bad := pipe.HSet(ctx, "merge:it:tool:wrongtype", "field", "value")
	if _, err := pipe.Exec(ctx); err == nil || bad.Err() == nil || good.Err() != nil {
		t.Fatalf("必须保真实逐命令返回: top=%v good=%v bad=%v", err, good.Err(), bad.Err())
	}
	if owner.session.Pending() != 0 {
		t.Fatal("真实pipeline返回后才能settle")
	}
	owner.session.CloseAdmission()
	if err := owner.session.Wait(ctx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("部分成功不能生成工具成功: %v", err)
	}
	if value, err := itRedis(t, itMappingRD).Get(ctx, "merge:it:tool:partial").Result(); err != nil || value != "sent" {
		t.Fatalf("保留实际部分副作用: %q %v", value, err)
	}
}

// 真实TiDB与Redis测试地址；旧MySQL环境变量仅保留参数名兼容。
const (
	itMySQLEnv   = "MERGE_ZONE_IT_MYSQL_DSN"
	itRedisEnv   = "MERGE_ZONE_IT_REDIS_ADDR"
	itRouteDBEnv = "MERGE_ZONE_IT_ROUTE_DB_DSN"
	itRequireEnv = "MERGE_ZONE_IT_REQUIRE"
)

var (
	// itMySQLBase / itRedisAddr 由 TestMain 按环境变量改写(itProbeDependencies)。
	itMySQLBase string
	itRedisAddr string
	// itUnavailable 是 TestMain 探测到的依赖不可达原因;空串 = 依赖就绪、一次性库已建好(requireIT 据此 SKIP / FAIL)。
	itUnavailable string
)

const (
	itSrcZone = uint32(901)
	itDstZone = uint32(902)

	// SQL Team放默认测试库；帮会/聚宝斋/好友继续独占schema，默认库缺guild表的反面用例仍成立。
	itDefaultDB = "merge_zone_it_db"     // -mysql-dsn默认库：SQL Team成员原件
	itGuildDB   = "merge_zone_it_guild"  // -guild-schema 指过去:guild / guild_member
	itTradeDB   = "merge_zone_it_trade"  // -trade-schema 指过去:trade_listing
	itFriendDB  = "merge_zone_it_friend" // -friend-schema 指过去:friend / friend_request
	itMappingRD = 9
	itGuildRD   = 10
	itSharedRD  = 11
	itSceneRD   = itSharedRD // scene_manager 生产上也是 DB 0,与 shared 同库
)

// itZoneSchemas 是两个 zone 库:合服只核对它们存在(P1),不读写里面的表。
func itZoneSchemas() []string {
	return []string{zoneDBName(itSrcZone), zoneDBName(itDstZone)}
}

var itBinary string
var itSQLDomainOnly bool

func itDSN() string {
	return itSchemaDSN(itDefaultDB)
}

// TestMain 只探测依赖、记下原因(规格 §13.3 第 7 条)。以前不可达时直接 os.Exit(0):SKIP 与 PASS 的退出码相同,
// `go test` 也只打一行 ok,看不出一个用例都没跑。现在不可达时照样 m.Run(),每个用例由 requireIT 逐条 SKIP
// (MERGE_ZONE_IT_REQUIRE=1 时 FAIL)。依赖就绪之后的建库 / 编译失败仍是真失败,退出码 1。
func TestMain(m *testing.M) {
	if os.Getenv("MERGE_ZONE_IT_SQL_DOMAIN_ONLY") == "1" {
		itSQLDomainOnly = true
		var err error
		raw := firstNonEmpty(os.Getenv(itTiDBEnv), os.Getenv(itMySQLEnv))
		itMySQLBase, err = itNormalizeMySQLBase(raw)
		if raw == "" || err != nil {
			itUnavailable = "SQL域runner必须显式注入已核loopback TiDB基址"
		}
		os.Exit(m.Run())
	}
	root, reason := itProbeDependencies()
	if reason != "" {
		itUnavailable = reason
		verdict := "SKIP"
		if itRequired() {
			verdict = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "merge_zone integration dependencies unavailable (%s): every integration test will %s "+
			"(%s=1 turns SKIP into FAIL)\n", reason, verdict, itRequireEnv)
		os.Exit(m.Run())
	}

	itDropAll(root)
	if err := itCreateAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: schema setup: %v\n", err)
		itDropAll(root)
		os.Exit(1)
	}

	// CLI 用例要跑真二进制(runMerge 用 log.Fatalf,不能在进程内调)。
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("merge_zone_it_%d.exe", os.Getpid()))
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: build test binary: %v\n%s\n", err, out)
		itDropAll(root)
		os.Exit(1)
	}
	itBinary = bin

	code := m.Run()

	itDropAll(root)
	_ = root.Close()
	_ = os.Remove(bin)
	os.Exit(code)
}

// itProbeDependencies在建库/删库前校验本机TiDB、CHECK与Redis空库/AOF契约，不修改基础设施配置。
// 返回可用的 root 连接,或不可达的原因(原因里不带口令)。
func itProbeDependencies() (*sql.DB, string) {
	if v := firstNonEmpty(os.Getenv(itTiDBEnv), os.Getenv(itMySQLEnv)); v != "" {
		base, err := itNormalizeMySQLBase(v)
		if err != nil {
			return nil, fmt.Sprintf("%s: %v", itTiDBEnv, err)
		}
		itMySQLBase = base
	} else {
		return nil, "必须显式注入已核任务所有的TiDB地址，不探测4000等默认端口"
	}
	if err := itValidateRouteOverride(); err != nil {
		return nil, err.Error()
	}
	if v := os.Getenv(itRedisEnv); v != "" {
		itRedisAddr = v
	} else {
		return nil, "必须显式注入已核任务所有的Redis地址，不探测6379等默认端口"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root, err := sql.Open("mysql", itMySQLBase+"?multiStatements=true")
	if err != nil {
		return nil, fmt.Sprintf("mysql open: %v", err)
	}
	if err := root.PingContext(ctx); err != nil {
		_ = root.Close()
		return nil, fmt.Sprintf("mysql unreachable: %v", err)
	}
	if err := itProbeTiDB(ctx, root); err != nil {
		_ = root.Close()
		return nil, fmt.Sprintf("TiDB dependency: %v", err)
	}
	for _, dbi := range []int{itMappingRD, itGuildRD, itSharedRD} {
		c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: dbi})
		n, err := c.DBSize(ctx).Result()
		_ = c.Close()
		if err != nil {
			_ = root.Close()
			return nil, fmt.Sprintf("redis %s db %d unreachable: %v", itRedisAddr, dbi, err)
		}
		if n != 0 {
			_ = root.Close()
			return nil, fmt.Sprintf("redis %s db %d is not empty (%d keys); refusing to touch someone else's data", itRedisAddr, dbi, n)
		}
	}
	c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: itMappingRD})
	defer c.Close()
	if err := c.Do(ctx, "WAITAOF", 1, 0, 100).Err(); err != nil {
		_ = root.Close()
		return nil, "db_first集成依赖需要Redis >= 7.2且AOF已启用（夹具不修改配置）"
	}
	return root, ""
}

// itNormalizeMySQLBase 校验 MERGE_ZONE_IT_MYSQL_DSN 是「实例基址」(不带库名、不带参数:一次性库与参数由用例拼,
// 沿用原来「基址 + 库名」的拼法),返回规范形式 "user:pass@tcp(host:port)/"。错误信息不回显 DSN(含口令)。
func itNormalizeMySQLBase(v string) (string, error) {
	return itControlledTiDBBase(v)
}

// itRequired 报告是否设了 MERGE_ZONE_IT_REQUIRE=1(依赖缺失时 FAIL 而不是 SKIP)。
func itRequired() bool {
	return os.Getenv(itRequireEnv) == "1"
}

// itMissing 报告一项集成依赖缺失:默认 SKIP 并写明原因;MERGE_ZONE_IT_REQUIRE=1 时 FAIL(杜绝把 SKIP 报成通过)。
func itMissing(t *testing.T, reason string) {
	t.Helper()
	if itRequired() {
		t.Fatalf("integration dependency missing and %s=1: %s", itRequireEnv, reason)
	}
	t.Skipf("integration dependency missing: %s (set %s=1 to fail instead of skipping)", reason, itRequireEnv)
}

// requireIT 是每个集成用例的第一句(it_guard_test.go 钉住):TestMain 探测到依赖不可达时 SKIP / FAIL。
func requireIT(t *testing.T) {
	t.Helper()
	if itSQLDomainOnly {
		if !strings.HasPrefix(t.Name(), "TestIT_SQLDomain") {
			t.Fatal("SQL域runner不能代替全CLI/Redis夹具；请显式选择TestIT_SQLDomain")
		}
		if itUnavailable != "" {
			t.Fatal(itUnavailable)
		}
		return
	}
	if itUnavailable != "" {
		itMissing(t, itUnavailable)
	}
}

func itDropAll(db *sql.DB) {
	for _, s := range append(itZoneSchemas(), itDefaultDB, itGuildDB, itTradeDB, itFriendDB, itRouteDB) {
		_, _ = db.Exec("DROP DATABASE IF EXISTS " + s)
	}
	ctx := context.Background()
	for _, dbi := range []int{itMappingRD, itGuildRD, itSharedRD} {
		c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: dbi})
		_ = c.FlushDB(ctx).Err() // 只清我们自己确认过是空的那几个库
		_ = c.Close()
	}
}

// itBusinessTablesDDL 是合服步骤读写的业务表,schema 为空时不带库名限定。只建合服步骤读写的列:完整形状由 go/schemamigrate
// 按 proto/guild/guild_db.proto、proto/trade/trade_table.proto 生成,这里不复刻第二份表结构。
// name_norm 是 go/guild 侧算好写进来的规范化名(NFKC → TrimSpace → 小写),唯一键 uk_guild 建在它上面;这里用生成列顶替那段
// 规范化,只为让「插入重名必然失败」这条不变量在测试里也成立。
func itBusinessTablesDDL(schema string) []string {
	prefix := ""
	if schema != "" {
		prefix = schema + "."
	}
	return []string{
		`CREATE TABLE ` + prefix + `guild (
			guild_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL,
			name_norm VARCHAR(191) AS (LOWER(name)) STORED,
			zone_id INT UNSIGNED NOT NULL DEFAULT 0, score BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (guild_id), UNIQUE KEY uk_guild (name_norm), KEY idx_guild_0 (zone_id))`,
		`CREATE TABLE ` + prefix + `guild_member (
			guild_id BIGINT UNSIGNED NOT NULL, player_id BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (guild_id, player_id))`,
		`CREATE TABLE ` + prefix + `trade_listing (
			listing_id BIGINT UNSIGNED NOT NULL, seller_player_id BIGINT UNSIGNED NOT NULL,
			market_zone INT UNSIGNED NOT NULL DEFAULT 0, seller_zone_at_listing INT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (listing_id), KEY idx_market_zone (market_zone), KEY idx_seller (seller_player_id, listing_id))`,
	}
}

// itCreateAll 建固定一次性库。
func itCreateAll(db *sql.DB) error {
	stmts := []string{
		"CREATE DATABASE " + itDefaultDB,
		"CREATE DATABASE " + itGuildDB,
		"CREATE DATABASE " + zoneDBName(itSrcZone),
		"CREATE DATABASE " + zoneDBName(itDstZone),
		"CREATE DATABASE " + itTradeDB,
		"CREATE DATABASE " + itFriendDB,
		"CREATE DATABASE " + itRouteDB,
		`CREATE TABLE ` + itDefaultDB + `.team_membership(player_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,team_id BIGINT UNSIGNED NOT NULL,epoch BIGINT UNSIGNED NOT NULL)`,
		// friend / friend_request 住独占库 mmorpg_friend(port-decisions D-14),这里用一次性库顶替。只建审计读到的列。
		`CREATE TABLE ` + itFriendDB + `.friend (
			player_id BIGINT UNSIGNED NOT NULL, friend_player_id BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (player_id, friend_player_id))`,
		`CREATE TABLE ` + itFriendDB + `.friend_request (
			from_player_id BIGINT UNSIGNED NOT NULL, to_player_id BIGINT UNSIGNED NOT NULL,
			status TINYINT NOT NULL DEFAULT 1, PRIMARY KEY (from_player_id, to_player_id))`,
	}
	for _, ddl := range itBusinessTablesDDL(itGuildDB)[:2] {
		stmts = append(stmts, ddl)
	}
	stmts = append(stmts, itBusinessTablesDDL(itTradeDB)[2])
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return itCreateRouteTables(db)
}

// ── 夹具 ─────────────────────────────────────────────────────

func itOpen(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", itDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

// itOpenRouteDB 为直接 SQL 断言与持锁事务打开独立强读连接，不穿透路由后端接口。
func itOpenRouteDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := openCurrentSession(ctx, itRouteDSN(), authoritysql.BackendTiDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func itRedis(t *testing.T, dbIndex int) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: dbIndex})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// itReset 把所有一次性状态清空,让每个用例从干净的起点开始。
func itReset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	db := itOpen(t)
	if _, err := db.Exec("DELETE FROM team_membership"); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"guild", "guild_member"} {
		if _, err := db.Exec("DELETE FROM " + itGuildDB + "." + tbl); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"friend", "friend_request"} {
		if _, err := db.Exec("DELETE FROM " + itFriendDB + "." + tbl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("DELETE FROM " + itTradeDB + ".trade_listing"); err != nil {
		t.Fatal(err)
	}
	for _, dbi := range []int{itMappingRD, itGuildRD, itSharedRD} {
		if err := itRedis(t, dbi).FlushDB(ctx).Err(); err != nil {
			t.Fatal(err)
		}
	}
	itResetRouteTables(t)
}

func itCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// itSeedListing 在 schema 里造一条商品。
func itSeedListing(t *testing.T, db *sql.DB, schema string, listingID, seller uint64, marketZone, sellerZoneAtListing uint32) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO "+schema+".trade_listing (listing_id, seller_player_id, market_zone, seller_zone_at_listing) VALUES (?,?,?,?)",
		listingID, seller, marketZone, sellerZoneAtListing); err != nil {
		t.Fatal(err)
	}
}

// itListingZones 读一条商品的 market_zone 与 seller_zone_at_listing。
func itListingZones(t *testing.T, db *sql.DB, schema string, listingID uint64) (marketZone, sellerZoneAtListing uint32) {
	t.Helper()
	if err := db.QueryRow("SELECT market_zone, seller_zone_at_listing FROM "+schema+".trade_listing WHERE listing_id = ?",
		listingID).Scan(&marketZone, &sellerZoneAtListing); err != nil {
		t.Fatal(err)
	}
	return marketZone, sellerZoneAtListing
}

// ── guild 的 Redis 面 ─────────────────────────────────────────

// ── guild ────────────────────────────────────────────────────

func TestIT_MergeRankZSET_IsAtomicAndRoundTripsThroughUnmerge(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	src, dst := guildZoneRankKey(itSrcZone), guildZoneRankKey(itDstZone)
	g.ZAdd(ctx, src, redis.Z{Member: "11", Score: 100}, redis.Z{Member: "12", Score: 50})
	g.ZAdd(ctx, dst, redis.Z{Member: "21", Score: 70})

	members, err := readZoneRankMembers(ctx, g, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("read %d members", len(members))
	}
	if _, err := mergeRankZSET(ctx, g, itSrcZone, itDstZone, members, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := g.Exists(ctx, src).Result(); n != 0 {
		t.Error("source ZSET survived the merge")
	}
	if n, _ := g.ZCard(ctx, dst).Result(); n != 3 {
		t.Errorf("target ZCARD=%d want 3", n)
	}
	if s, _ := g.ZScore(ctx, dst, "11").Result(); s != 100 {
		t.Errorf("score not preserved: %v", s)
	}

	// 撤销:成员回源区,目标区只掉这两个,原住民 21 留下。
	if _, err := unmergeRankZSET(ctx, g, itSrcZone, itDstZone, members, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := g.ZCard(ctx, src).Result(); n != 2 {
		t.Errorf("source ZCARD after unmerge=%d want 2", n)
	}
	if n, _ := g.ZCard(ctx, dst).Result(); n != 1 {
		t.Errorf("target ZCARD after unmerge=%d want 1 (the native guild)", n)
	}
	if s, _ := g.ZScore(ctx, dst, "21").Result(); s != 70 {
		t.Errorf("native member disturbed: %v", s)
	}
}

// TestIT_Gap_MergeGuildRank_UsesTheSourceReadUnderTheLock(A8):榜单步骤 以维护锁内重读的源榜为准,清单阶段的
// 快照(分数旧、少了之后进榜的成员)从不写进目标榜;写之前先把撤销依据交给清单。
func TestIT_Gap_MergeGuildRank_UsesTheSourceReadUnderTheLock(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	src, dst := guildZoneRankKey(itSrcZone), guildZoneRankKey(itDstZone)
	// 清单阶段的快照:只有 11,分数 90。之后 guild 服把 11 涨到 100,12 进了源榜。
	snapshot := []manifestRankMember{{Member: "11", Score: 90}}
	g.ZAdd(ctx, src, redis.Z{Member: "11", Score: 100}, redis.Z{Member: "12", Score: 50})
	g.ZAdd(ctx, dst, redis.Z{Member: "21", Score: 70})

	var recorded []manifestRankMember
	record := func(ms []manifestRankMember) error {
		// 清单先于写:这一刻源榜必须还在(MULTI/EXEC 还没跑)。
		if n, _ := g.Exists(ctx, src).Result(); n != 1 {
			t.Error("recordBeforeWrite ran after the rank was already merged")
		}
		recorded = ms
		return nil
	}
	members, sourceGone, err := mergeGuildRank(ctx, g, itSrcZone, itDstZone, snapshot, false, false, record)
	if err != nil {
		t.Fatal(err)
	}
	if sourceGone || len(members) != 2 || fmt.Sprint(recorded) != fmt.Sprint(members) {
		t.Fatalf("members=%v sourceGone=%v recorded=%v, want the 2 re-read members recorded before the write",
			members, sourceGone, recorded)
	}
	if s, _ := g.ZScore(ctx, dst, "11").Result(); s != 100 {
		t.Errorf("guild 11 score in the target rank = %v, want the re-read 100 (not the stale snapshot 90)", s)
	}
	if _, err := g.ZScore(ctx, dst, "12").Result(); err != nil {
		t.Errorf("guild 12 joined the source rank after the snapshot and must be merged too: %v", err)
	}
	if n, _ := g.Exists(ctx, src).Result(); n != 0 {
		t.Error("the source rank survived the merge")
	}

	// 续跑(上一次 MULTI/EXEC 成功、persist 之前中止):这期间 guild 服已把目标榜上 11 的分数更新到 120。
	// 源榜已不在 —— 不写,目标榜不被上一次记下的快照回退;撤销依据沿用上一次写入之前落盘的那一份。
	g.ZAdd(ctx, dst, redis.Z{Member: "11", Score: 120})
	members, sourceGone, err = mergeGuildRank(ctx, g, itSrcZone, itDstZone, recorded, true, false,
		func(ms []manifestRankMember) error { recorded = ms; return nil })
	if err != nil || !sourceGone || len(members) != 2 || len(recorded) != 2 {
		t.Fatalf("resume: members=%v sourceGone=%v recorded=%v err=%v", members, sourceGone, recorded, err)
	}
	if s, _ := g.ZScore(ctx, dst, "11").Result(); s != 120 {
		t.Errorf("guild 11 score in the target rank = %v after the resume, want 120 (not rolled back by the snapshot)", s)
	}
	if n, _ := g.ZCard(ctx, dst).Result(); n != 3 {
		t.Errorf("target ZCARD after the resume = %d, want 3", n)
	}

	// 清单落盘失败 = 一个字节都不写。
	g.ZAdd(ctx, src, redis.Z{Member: "13", Score: 1})
	boom := fmt.Errorf("disk full")
	if _, _, err := mergeGuildRank(ctx, g, itSrcZone, itDstZone, nil, false, false, func([]manifestRankMember) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a failed manifest write must abort the step: %v", err)
	}
	if _, err := g.ZScore(ctx, dst, "13").Result(); err == nil {
		t.Error("the rank was merged although the manifest write failed")
	}
	if n, _ := g.Exists(ctx, guildRankLockKey).Result(); n != 0 {
		t.Error("the maintenance lock must be released on every path")
	}
}

// TestIT_Gap_MergeGuildRank_EmptiedSourceNeverResurrects:清单阶段之后、榜单步骤 之前源榜被 guild 服合法清空
// (公会全部解散,或 RebuildRanks 按 MySQL 重建、目标榜里已是新分数)。清单快照从没写过,榜单步骤 不得把它写进目标榜,
// 撤销依据记为空(撤销不再把可能已解散的公会 ZADD 回源区)。
func TestIT_Gap_MergeGuildRank_EmptiedSourceNeverResurrects(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	dst := guildZoneRankKey(itDstZone)
	// 清单阶段的快照:11(之后被 RebuildRanks 按 MySQL 挪进目标榜、分数 130)与 12(之后解散)。
	snapshot := []manifestRankMember{{Member: "11", Score: 90}, {Member: "12", Score: 50}}
	g.ZAdd(ctx, dst, redis.Z{Member: "21", Score: 70}, redis.Z{Member: "11", Score: 130})

	recordCalled := false
	var recorded []manifestRankMember
	members, sourceGone, err := mergeGuildRank(ctx, g, itSrcZone, itDstZone, snapshot, false, false,
		func(ms []manifestRankMember) error { recordCalled, recorded = true, ms; return nil })
	if err != nil || !sourceGone {
		t.Fatalf("sourceGone=%v err=%v", sourceGone, err)
	}
	if !recordCalled || len(recorded) != 0 || len(members) != 0 {
		t.Errorf("an unwritten snapshot must be dropped from the unmerge basis: called=%v recorded=%v members=%v",
			recordCalled, recorded, members)
	}
	if s, _ := g.ZScore(ctx, dst, "11").Result(); s != 130 {
		t.Errorf("guild 11 score in the target rank = %v, want the rebuilt 130 (not the stale snapshot 90)", s)
	}
	if _, err := g.ZScore(ctx, dst, "12").Result(); err == nil {
		t.Error("disbanded guild 12 was resurrected into the target rank from the stale snapshot")
	}
	if n, _ := g.ZCard(ctx, dst).Result(); n != 2 {
		t.Errorf("target ZCARD = %d, want 2", n)
	}
}

// TestIT_Gap_MergeGuildRank_ResumeDoesNotRewriteADisbandedMember:续跑前某个已并进目标榜的成员被 ZREM(解散),
// 续跑不得把它写回目标榜。
func TestIT_Gap_MergeGuildRank_ResumeDoesNotRewriteADisbandedMember(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	dst := guildZoneRankKey(itDstZone)
	recorded := []manifestRankMember{{Member: "11", Score: 100}, {Member: "12", Score: 50}}
	// 上一次 MULTI/EXEC 已合走(源榜不在),之后 12 解散被 ZREM。
	g.ZAdd(ctx, dst, redis.Z{Member: "21", Score: 70}, redis.Z{Member: "11", Score: 100})
	if _, _, err := mergeGuildRank(ctx, g, itSrcZone, itDstZone, recorded, true, false,
		func([]manifestRankMember) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ZScore(ctx, dst, "12").Result(); err == nil {
		t.Error("the resume wrote disbanded guild 12 back into the target rank")
	}
}

// TestIT_Gap_MergeRankZSET_KeepsNewerTargetScores:步骤 3 之后 guild 服已往目标榜写了某个源区公会的新分数,
// 源榜里还是旧的 —— ZADD NX 不覆盖目标榜里的新分数。
func TestIT_Gap_MergeRankZSET_KeepsNewerTargetScores(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	src, dst := guildZoneRankKey(itSrcZone), guildZoneRankKey(itDstZone)
	g.ZAdd(ctx, src, redis.Z{Member: "11", Score: 100}, redis.Z{Member: "12", Score: 50})
	g.ZAdd(ctx, dst, redis.Z{Member: "11", Score: 150})
	members, err := readZoneRankMembers(ctx, g, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mergeRankZSET(ctx, g, itSrcZone, itDstZone, members, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := g.ZScore(ctx, dst, "11").Result(); s != 150 {
		t.Errorf("guild 11 score = %v, want the newer target score 150", s)
	}
	if s, _ := g.ZScore(ctx, dst, "12").Result(); s != 50 {
		t.Errorf("guild 12 score = %v, want 50", s)
	}
	if n, _ := g.Exists(ctx, src).Result(); n != 0 {
		t.Error("the source rank survived the merge")
	}
}

func TestIT_InvalidateGuildCaches_IncrementsGenerationAndDropsTheEntry(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, _ := testMaintenanceContext()
	g := itRedis(t, itGuildRD)
	g.Set(ctx, guildCacheKey(11), `{"zone_id":901}`, 0)
	g.Set(ctx, guildCacheGenerationKey(11), "4", 0)
	g.Set(ctx, guildCacheKey(12), `{"zone_id":901}`, 0) // 没有 generation 键

	n, err := invalidateGuildCaches(ctx, g, []uint64{11, 12}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("invalidated=%d want 2", n)
	}
	if v, _ := g.Exists(ctx, guildCacheKey(11)).Result(); v != 0 {
		t.Error("guild:v2:11 survived — the guild service would serve the old zone for 30 minutes")
	}
	if v, _ := g.Get(ctx, guildCacheGenerationKey(11)).Result(); v != "5" {
		t.Errorf("generation=%q want 5 — without the INCR an in-flight reader refills the stale snapshot", v)
	}
	if v, _ := g.Get(ctx, guildCacheGenerationKey(12)).Result(); v != "1" {
		t.Errorf("generation for a guild with no prior generation key = %q want 1", v)
	}
}

func TestIT_AssertNoGuildNameCollision(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'alpha',901),(21,'beta',902)"); err != nil {
		t.Fatal(err)
	}
	// name_norm 全局 UNIQUE(uk_guild)⇒ 跨 zone 重名不可能存在 ⇒ 探测恒通过。
	if err := assertNoGuildNameCollision(ctx, db, itGuildDB, itSrcZone, itDstZone); err != nil {
		t.Fatalf("guild.name_norm is globally unique, so no collision is possible: %v", err)
	}
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (12,'beta',901)"); err == nil {
		t.Fatal("the schema no longer enforces UNIQUE(name_norm); the collision path must be revisited")
	}
	// 大小写只差的名字规范化后是同一个 name_norm,同样进不来。
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (13,'BETA',901)"); err == nil {
		t.Fatal("uk_guild is on name_norm, so a case-only variant must be rejected too")
	}
}

// TestIT_AssertGuildTablesReady:库在 + 两张表在才放行。库不在、库在表不在都必须拒绝 ——
// 这两种情况和「这个区一个公会都没有」在查询结果上长得一样,放过去就是静默漏搬。
func TestIT_AssertGuildTablesReady(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	if err := assertGuildTablesReady(ctx, db, itGuildDB); err != nil {
		t.Fatalf("the guild tables exist but were refused: %v", err)
	}
	if err := assertGuildTablesReady(ctx, db, "merge_zone_it_no_such_guild"); err == nil {
		t.Error("a missing guild schema must be refused (fail-closed)")
	}
	if err := assertGuildTablesReady(ctx, db, itDefaultDB); err == nil {
		t.Error("a schema without the guild tables (guild never migrated) must be refused")
	}
	if err := assertGuildTablesReady(ctx, db, "merge_zone_it_guild;DROP DATABASE x"); err == nil {
		t.Error("a schema name that is not a plain identifier must be refused before any SQL")
	}
}

// ── 锁序(2026-09-21 死锁审计 #17)────────────────────────────

// itOpenReadCommitted 与 mustOpenMySQL 同口径打开句柄(readCommittedDSN),合服写路径的用例都走它。
func itOpenReadCommitted(t *testing.T) *sql.DB {
	t.Helper()
	dsn, err := readCommittedDSN(itDSN(), authoritysql.BackendTiDB)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestIT_MySQLSessionIsReadCommitted:readCommittedDSN 改出来的 DSN 真的让驱动在建连时把会话设成 RC ——
// 单测只能证明 DSN 字符串长对了,这里证明服务器端确实生效(包括 DSN 里原有的 multiStatements 等参数照常工作)。
func TestIT_MySQLSessionIsReadCommitted(t *testing.T) {
	requireIT(t)
	db := itOpenReadCommitted(t)
	var level string
	if err := db.QueryRow("SELECT @@SESSION.transaction_isolation").Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != "READ-COMMITTED" {
		t.Fatalf("session isolation = %q, want READ-COMMITTED — the merge's lock-order reasoning assumes RC", level)
	}
}

// itInlineUintArgs 把 ? 依次替换成数字,供 EXPLAIN 使用(EXPLAIN 不走预编译,且参数只有无符号整数)。
func itInlineUintArgs(t *testing.T, stmt string, args ...uint64) string {
	t.Helper()
	if strings.Count(stmt, "?") != len(args) {
		t.Fatalf("placeholder count %d != arg count %d in %q", strings.Count(stmt, "?"), len(args), stmt)
	}
	for _, a := range args {
		stmt = strings.Replace(stmt, "?", strconv.FormatUint(a, 10), 1)
	}
	return stmt
}

// TestIT_ZonePointUpdatesArePrimaryKeyPointLookups 钉住 rewriteZoneByPrimaryKey 的执行计划:对生产代码
// **同一个** zonePointUpdateSQL的产物做TiDB原生EXPLAIN，必须是完整主键Point_Get且不能有扫描算子。
// 改回 `zone = ? AND pk IN (...)` 或整区 `WHERE zone = ?`,优化器就可能挑 zone 二级索引(先锁二级项再回表),
// 与在线写者「主键 → 二级」反序成环 —— 这类问题只取决于执行计划,EXPLAIN 每次都答得出来。
// 先各插一行，要求估计点取一行；不能套用MySQL的key_len断言而漏掉真实TiDB执行计划。
func TestIT_ZonePointUpdatesArePrimaryKeyPointLookups(t *testing.T) {
	requireIT(t)
	itReset(t)
	db := itOpenReadCommitted(t)
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'a',901),(12,'b',901),(21,'c',902)"); err != nil {
		t.Fatal(err)
	}
	itSeedListing(t, db, itTradeDB, 7001, 9901, itSrcZone, itSrcZone)
	itSeedListing(t, db, itTradeDB, 7002, 9902, itSrcZone, itSrcZone)
	itSeedListing(t, db, itTradeDB, 7101, 9950, itDstZone, itDstZone)

	guild := guildQualified(itGuildDB, guildTable)
	trade := tradeListingQualified(itTradeDB)
	cases := []struct {
		name string
		stmt string
		args []uint64 // 已存在的行;顺序与 rewriteZoneByPrimaryKey 的绑定一致
	}{
		// UPDATE 的参数:(to, pk, from)。
		{"guild zone_id update", zonePointUpdateSQL(guild, guildPKColumn, guildZoneColumn), []uint64{uint64(itDstZone), 11, uint64(itSrcZone)}},
		{"trade_listing market_zone update", zonePointUpdateSQL(trade, tradeListingPKColumn, tradeMarketZoneColumn),
			[]uint64{uint64(itDstZone), 7001, uint64(itSrcZone)}},
		// 单行事务的第一句(A15):SELECT … FOR UPDATE 的参数只有 (pk)。
		{"guild zone_id lock select", zoneLockSelectSQL(guild, guildPKColumn, guildZoneColumn), []uint64{11}},
		{"trade_listing market_zone lock select", zoneLockSelectSQL(trade, tradeListingPKColumn, tradeMarketZoneColumn), []uint64{7001}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stmt := itInlineUintArgs(t, c.stmt, c.args...)
			itAssertTiDBPrimaryPoint(t, itExplainRows(t, db, stmt), stmt)
		})
	}
}

// itExplainRows读取TiDB原生brief计划的全部行，保留实际算子与访问对象。
func itExplainRows(t *testing.T, db *sql.DB, stmt string) []map[string]string {
	t.Helper()
	rows, err := db.Query("EXPLAIN FORMAT='brief' " + stmt)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", stmt, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		plan := make(map[string]string, len(cols))
		for i, c := range cols {
			plan[c] = vals[i].String
		}
		out = append(out, plan)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// itLockWaitersQuery 通过TiDB等待图观察正在等待目标表FOR UPDATE原行锁的事务。
// 帮会表不在 DSN 的默认库里,所以库名显式传,不用 DATABASE()。
const itLockWaitersQuery = `
	SELECT COUNT(*) FROM information_schema.data_lock_waits
	WHERE JSON_UNQUOTE(JSON_EXTRACT(key_info,'$.db_name')) = ?
	AND JSON_UNQUOTE(JSON_EXTRACT(key_info,'$.table_name')) = ?
	AND LOWER(COALESCE(sql_digest_text,'')) LIKE '%for update%'`

// itAwaitLockWaiters 轮询直到 schema.table 上至少有 want 个事务在等行锁,或 budget 用尽(返回错误)。
// 靠观察而不是 sleep 估时间:两次轮询之间的短停顿只为不空转打爆 TiDB。
func itAwaitLockWaiters(ctx context.Context, db *sql.DB, schema, table string, want int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		var waiters int
		if err := db.QueryRowContext(ctx, itLockWaitersQuery, schema, table).Scan(&waiters); err != nil {
			return fmt.Errorf("read TiDB information_schema.data_lock_waits (requires PROCESS; unobservable contention fails): %w", err)
		}
		if waiters >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d/%d transactions queued on %s.%s within %s", waiters, want, schema, table, budget)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isITDeadlock 报告 err 是否是 InnoDB 死锁(1213)。
func isITDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDeadlock
}

// ── 榜单维护锁 ───────────────────────────────────────────────

func TestIT_AcquireGuildRankLock_BlocksWhileTheGuildServiceHoldsIt(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	g := itRedis(t, itGuildRD)
	// 模拟 guild 服正在跑榜单重建。
	if err := g.Set(ctx, guildRankLockKey, "guild-service-token", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := acquireGuildRankLock(short, g, false); err == nil {
		t.Fatal("the merge grabbed the rank lock while the guild service held it")
	}
	// guild 服释放后能拿到,而且释放不会误删别人的 token。
	if err := g.Del(ctx, guildRankLockKey).Err(); err != nil {
		t.Fatal(err)
	}
	release, err := acquireGuildRankLock(ctx, g, false)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := g.Exists(ctx, guildRankLockKey).Result(); n != 1 {
		t.Fatal("lock not held after a successful acquire")
	}
	release()
	if n, _ := g.Exists(ctx, guildRankLockKey).Result(); n != 0 {
		t.Fatal("lock not released")
	}
}

// ── preflight ────────────────────────────────────────────────

// ── preflight ────────────────────────────────────────────────

func itPreflightDeps(t *testing.T) preflightDeps {
	t.Helper()
	reader, err := openTeamAuthority(context.Background(), itDSN(), itDefaultDB, authoritysql.BackendTiDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.db.Close() })
	return preflightDeps{
		sceneRdb:      itRedis(t, itSceneRD),
		sceneNodes:    zoneDownRegistry{},
		sharedRdb:     itRedis(t, itSharedRD),
		teamAuthority: reader,
	}
}

func itPreflightParams(ids []uint64) preflightParams {
	return preflightParams{readOnly: true, zone: itSrcZone, scope: "source", playerIDs: ids}
}

func TestIT_Preflight_PassesOnACleanZone(t *testing.T) {
	requireIT(t)
	itReset(t)
	if err := runPreflight(context.Background(), itPreflightDeps(t), itPreflightParams([]uint64{9701})); err != nil {
		t.Fatalf("clean zone rejected: %v", err)
	}
}

func TestIT_Preflight_BlocksOnEachHazard(t *testing.T) {
	requireIT(t)
	ids := []uint64{9711}
	cases := []struct {
		name   string
		setup  func(t *testing.T)
		expect string
	}{
		{"live scene node", func(t *testing.T) {
			itRedis(t, itSceneRD).ZAdd(context.Background(), fmt.Sprintf(sceneNodeLoadKeyFmt, itSrcZone), redis.Z{Member: "10"})
		}, "P2"},
		{"session still online", func(t *testing.T) {
			itRedis(t, itSharedRD).Set(context.Background(), "player:session:9711", "gate-3", time.Minute)
		}, "P6"},
		{"player still in a live team", func(t *testing.T) {
			if _, err := itOpen(t).Exec("INSERT INTO team_membership VALUES(9711,880001,1)"); err != nil {
				t.Fatal(err)
			}
		}, "P7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			itReset(t)
			c.setup(t)
			err := runPreflight(context.Background(), itPreflightDeps(t), itPreflightParams(ids))
			if err == nil {
				t.Fatalf("%s did not block the merge", c.name)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Errorf("expected %s in %q", c.expect, err)
			}
		})
	}
}

// P7只信SQL成员原件；旧Redis残留既不能拦无队玩家，也不能清掉SQL有效成员。
func TestIT_Preflight_IgnoresStaleTeamIndexes(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	shared := itRedis(t, itSharedRD)
	// 9721:离队后 tid 置 "0"、key 保留。
	shared.HSet(ctx, "team:player:9721", "tid", "0", "epoch", "5")
	// 9722:索引仍指向 880002,但 team:rec:880002 已不存在(队伍过期后的孤儿索引)。
	shared.HSet(ctx, "team:player:9722", "tid", "880002", "epoch", "7")
	if _, err := itOpen(t).Exec("INSERT INTO team_membership VALUES(9721,0,5),(9722,0,8)"); err != nil {
		t.Fatal(err)
	}
	// 9723:从没组过队,没有索引。
	if err := runPreflight(ctx, itPreflightDeps(t), itPreflightParams([]uint64{9721, 9722, 9723})); err != nil {
		t.Fatalf("stale team indexes must not block the merge: %v", err)
	}
}

// ── 真二进制:-dry-run、拒绝路径与审计 ────────────────────────

// itEtcdStub 是一个空的场景节点注册表(etcd v3 JSON 网关的 range 应答)。
func itEtcdStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"kvs":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// itRun 跑真二进制,返回合并输出与退出码。
func itRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := itCommand(t, args...)
	out, err := cmd.CombinedOutput()
	return itCommandResult(t, out, err)
}

func itCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	base := []string{
		"-route-db-dsn", itRouteDSN(),
		"-mysql-dsn", itDSN(),
		"-team-schema", itDefaultDB,
		"-redis-addr", itRedisAddr,
		"-redis-db", strconv.Itoa(itGuildRD),
		"-mapping-redis-db", strconv.Itoa(itMappingRD),
		"-notice-redis-db", strconv.Itoa(itSharedRD),
		"-scene-redis-db", strconv.Itoa(itSceneRD),
		"-trade-schema", itTradeDB,
		"-guild-schema", itGuildDB,
		"-friend-schema", itFriendDB,
		"-etcd-endpoints", itEtcdStub(t),
	}
	return exec.Command(itBinary, append(base, args...)...)
}

func itCommandResult(t *testing.T, out []byte, err error) (string, int) {
	t.Helper()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("启动合区测试二进制失败（参数可能含凭据，不回显）: %v", err)
	}
	return string(out), code
}

// itSeedBusiness 造一份 901 → 902 的业务数据,返回 mapping / guild 两个控制句柄。
func itSeedBusiness(t *testing.T) (*sql.DB, *redis.Client, *redis.Client) {
	t.Helper()
	ctx := context.Background()
	db := itOpen(t)
	itSetRouteHome(t, 9901, itSrcZone)
	itSetRouteHome(t, 9902, itSrcZone)
	itSetRouteHome(t, 9950, itDstZone)
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'src-a',901),(21,'dst-a',902)"); err != nil {
		t.Fatal(err)
	}
	guildRdb := itRedis(t, itGuildRD)
	guildRdb.ZAdd(ctx, guildZoneRankKey(itSrcZone), redis.Z{Member: "11", Score: 100})
	guildRdb.ZAdd(ctx, guildZoneRankKey(itDstZone), redis.Z{Member: "21", Score: 70})
	itSeedListing(t, db, itTradeDB, 7001, 9901, itSrcZone, itSrcZone)
	itSeedListing(t, db, itTradeDB, 7101, 9950, itDstZone, itDstZone)
	return db, itRedis(t, itMappingRD), guildRdb
}

// TestIT_CLI_DryRunWritesNothingAndItsPreviewIsNeverConsumed:dry-run 一个字节不写(只写预览),预览不能被当成清单读。
func TestIT_CLI_DryRunWritesNothingAndItsPreviewIsNeverConsumed(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	db, mapRdb, guildRdb := itSeedBusiness(t)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "merge.json")
	preview := dryRunManifestPath(manifest)
	zoneArgs := []string{"-source-zone", "901", "-target-zone", "902"}

	out, code := itRun(t, append(append([]string{}, zoneArgs...), "-manifest-path", manifest, "-dry-run")...)
	if code != 0 {
		t.Fatalf("dry-run exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "2 players with home_zone=901") {
		t.Errorf("dry-run did not report the player count:\n%s", out)
	}
	if n := itCount(t, itOpenRouteDB(t), "zone_merged_into"); n != 0 {
		t.Fatalf("dry-run wrote zone_merged_into: %d rows", n)
	}
	if n, _ := mapRdb.Exists(ctx, mergedIntoKey(itSrcZone)).Result(); n != 0 {
		t.Fatal("dry-run wrote the merged_into mirror")
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 1 {
		t.Fatalf("dry-run rewrote guilds: %d left in the source zone", n)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 1 {
		t.Fatalf("dry-run rewrote trade listings: %d left in the source zone", n)
	}
	if n, _ := guildRdb.Exists(ctx, guildZoneRankKey(itSrcZone)).Result(); n != 1 {
		t.Fatal("dry-run merged the rank")
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote -manifest-path itself (stat err=%v)", err)
	}
	raw, err := os.ReadFile(preview)
	if err != nil || !strings.Contains(string(raw), `"dry_run": true`) {
		t.Fatalf("dry-run preview missing or not marked (err=%v):\n%s", err, raw)
	}

	// 预览路径本身:按后缀拒。
	for _, args := range [][]string{
		append(append([]string{}, zoneArgs...), "-manifest-path", preview, "-dry-run"),
		{"-mode", "unmerge", "-manifest-path", preview, "-dry-run"},
	} {
		if out, code := itRun(t, args...); code == 0 || !strings.Contains(out, "reserved for dry-run previews") {
			t.Errorf("%v must refuse the preview path (exit=%d):\n%s", args, code, out)
		}
	}
	// 被改名放到普通路径上:按内容里的 dry_run 拒。
	renamed := filepath.Join(dir, "renamed.json")
	if err := os.WriteFile(renamed, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := itRun(t, append(append([]string{}, zoneArgs...), "-manifest-path", renamed, "-dry-run")...); code == 0 ||
		!strings.Contains(out, "dry-run preview") {
		t.Errorf("a renamed preview must be refused (exit=%d):\n%s", code, out)
	}
	// 合服后验证拿到预览:无从核对 —— INFRA(exit 2),不是通过。
	if out, code := itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902", "-verify-merged",
		"-manifest-path", renamed); code != 2 {
		t.Errorf("-verify-merged with a preview must exit 2 (exit=%d):\n%s", code, out)
	}
	// 没有 -manifest-path 直接拒绝。
	if out, code := itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902", "-verify-merged"); code == 0 ||
		!strings.Contains(out, "requires -manifest-path") {
		t.Errorf("-verify-merged without -manifest-path must be refused (exit=%d):\n%s", code, out)
	}
}

// TestIT_CLI_ApplyWithoutTheFreezeReceiptWritesNothing:没有正式 NORMAL 冻结收据的 -apply 在任何写之前拒绝。
func TestIT_CLI_ApplyWithoutTheFreezeReceiptWritesNothing(t *testing.T) {
	requireIT(t)
	itReset(t)
	db, mapRdb, _ := itSeedBusiness(t)
	manifest := filepath.Join(t.TempDir(), "merge.json")
	out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply")
	if code == 0 {
		t.Fatalf("-apply without a freeze receipt must be refused:\n%s", out)
	}
	if n := itCount(t, itOpenRouteDB(t), "zone_merged_into"); n != 0 {
		t.Fatalf("a refused run wrote zone_merged_into: %d rows", n)
	}
	if n, _ := mapRdb.Exists(context.Background(), mergedIntoKey(itSrcZone)).Result(); n != 0 {
		t.Fatal("a refused run wrote the merged_into mirror")
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 1 {
		t.Fatalf("a refused run rewrote guilds: %d left in the source zone", n)
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatalf("a refused run wrote the manifest (stat err=%v)", err)
	}
}

// TestIT_CLI_ChainedMergeRefusals:连续合服的拒绝规则对着真库走 -dry-run:目标区已被合走、源区已合进别处都在第一次写之前拒绝;
// 有别的区合进了源区(连续合服)放行。
func TestIT_CLI_ChainedMergeRefusals(t *testing.T) {
	requireIT(t)
	for _, tc := range []struct {
		name         string
		src, dst     uint32
		want         string
		wantExitIsOK bool
	}{
		{name: "目标区已被合走", src: itDstZone, dst: 903, want: "itself merged into zone 903"},
		{name: "源区已合进别处", src: itSrcZone, dst: 903, want: "already merged into zone 903"},
		{name: "有别的区合进了源区", src: 900, dst: itSrcZone, wantExitIsOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			itReset(t)
			itSeedRouteMerged(t, tc.src, tc.dst, "seed-run")
			manifest := filepath.Join(t.TempDir(), "merge.json")
			out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-dry-run")
			if tc.wantExitIsOK {
				if code != 0 {
					t.Fatalf("must pass (exit=%d):\n%s", code, out)
				}
				return
			}
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("exit=%d, want a refusal mentioning %q:\n%s", code, tc.want, out)
			}
		})
	}
}

func TestIT_Audit_ExitsTwoWhenAHandleIsMissing(t *testing.T) {
	requireIT(t)
	itReset(t)
	// 指向一个没人监听的 Redis:审计跑不成,必须 exit 2(不是 0,也不是 1)。
	cmd := exec.Command(itBinary, "-mode", "audit", "-source-zone", "901", "-target-zone", "902", "-route-db-dsn", itRouteDSN(),
		"-mysql-dsn", itDSN(), "-redis-addr", "127.0.0.1:6399",
		"-guild-schema", itGuildDB, "-trade-schema", itTradeDB, "-friend-schema", itFriendDB)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 2 {
		t.Fatalf("exit=%d want 2 (infrastructure failure must be distinguishable from a real finding)\n%s", code, out)
	}
	if !strings.Contains(string(out), "NOT a clean bill of health") {
		t.Errorf("the operator must be told the audit did not run:\n%s", out)
	}
}

func TestIT_Audit_OnlineGateActuallyBlocks(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	itSetRouteHome(t, 9971, itSrcZone)
	// player:session 住在 SHARED DB(生产上是 DB 0,由 player_locator 写)。
	itRedis(t, itSharedRD).Set(ctx, "player:session:9971", "1", time.Minute)

	out, code := itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902")
	if code != 1 {
		t.Fatalf("exit=%d want 1 (a player still online must block the merge)\n%s", code, out)
	}
	if !strings.Contains(out, "online_presence") {
		t.Errorf("the online gate did not fire:\n%s", out)
	}
}

// ── 端到端(进程内跑真实步骤,真实冻结 + 真实 Redis)─────────────

// TestIT_EndToEnd_MergeVerifyUnmerge:清单 → 映射 → 业务合并 → 验证 → 重跑 → 撤销,全程只碰清单里的对象,玩家路由行不变。
func TestIT_EndToEnd_MergeVerifyUnmerge(t *testing.T) {
	requireIT(t)
	itReset(t)
	d := itSQLMaintenanceDomain(t)
	ctx, db := d.ctx, d.db

	// player_route:源区 2 人 + 目标区原住民 1 人。合服全程不改这三行。
	const insertRoute = "INSERT INTO `%s`.player_route (route_shard, player_id, home_zone, rev, updated_by, created_at_ms, updated_at_ms) VALUES (?, ?, ?, 1, 'merge-zone-it', 0, 0)"
	for id, zone := range map[uint64]uint32{9901: itSrcZone, 9902: itSrcZone, 9950: itDstZone} {
		d.exec(t, fmt.Sprintf(insertRoute, d.route), uint64(routeRowShard(id)), id, zone)
	}
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (11,'src-a',901),(21,'dst-a',902)")
	itSeedListing(t, d.seed, d.biz, 7001, 9901, itSrcZone, itSrcZone)
	itSeedListing(t, d.seed, d.biz, 7002, 9902, itSrcZone, 900)
	itSeedListing(t, d.seed, d.biz, 7101, 9950, itDstZone, itDstZone)

	mapRdb := mustDial(ctx, "mapping", itRedisAddr, "", itMappingRD)
	guildRdb := mustDial(ctx, "guild", itRedisAddr, "", itGuildRD)
	t.Cleanup(func() { _ = mapRdb.Close(); _ = guildRdb.Close() })
	guildRdb.ZAdd(ctx, guildZoneRankKey(itSrcZone), redis.Z{Member: "11", Score: 100}, redis.Z{Member: "13", Score: 40})
	guildRdb.ZAdd(ctx, guildZoneRankKey(itDstZone), redis.Z{Member: "21", Score: 70})
	guildRdb.Set(ctx, guildCacheKey(11), `{"zone_id":901}`, 0)

	dur, err := newRouteDurability(1, 0, 1500)
	if err != nil {
		t.Fatal(err)
	}
	rb := &routeBackend{db: d.store, pub: newRoutePublisher(mapRdb, dur)}
	o := options{guildSchema: d.biz, tradeSchema: d.biz}
	path := filepath.Join(t.TempDir(), "merge.json")
	runID := newRunID(itSrcZone, itDstZone, time.Now())

	// 第一次写之前:冲突检查 + 清单落盘。
	if err := stepCheckMergedIntoBeforeMerge(ctx, rb, itSrcZone, itDstZone); err != nil {
		t.Fatal(err)
	}
	m := newMergeManifest(runID, itSrcZone, itDstZone, "it", time.Now())
	if err := fillMergeManifest(ctx, m, db, guildRdb, o); err != nil {
		t.Fatal(err)
	}
	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	// 清单落盘之后、映射生效之前(旧缓存窗口)才进源区的公会:步骤开始时重收,不漏。
	d.exec(t, "INSERT INTO guild (guild_id, name, zone_id) VALUES (13,'late',901)")

	persist := func(step, detail string) error {
		m.markStep(step, detail)
		return saveManifest(path, m)
	}
	ops := &liveMergeSteps{o: o, src: itSrcZone, dst: itDstZone, m: m, manifestPath: path, db: db, guildRdb: guildRdb, rb: rb, gate: d.gate()}
	if err := runMergeSteps(ctx, mergeStepPlan(ops, o), m, d.gate(), false, persist); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// 映射与镜像。
	wantValue, _ := encodeRouteMergedIntoValue(itDstZone, runID)
	if dst, run, found, err := d.store.ReadMergedInto(ctx, itSrcZone); err != nil || !found || dst != itDstZone || run != runID {
		t.Fatalf("zone_merged_into = (%d, %q, %v, %v)", dst, run, found, err)
	}
	if v, err := itRedis(t, itMappingRD).Get(ctx, mergedIntoKey(itSrcZone)).Result(); err != nil || v != wantValue {
		t.Fatalf("mirror = (%q, %v), want %q", v, err, wantValue)
	}
	// 业务数据。
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 901"); n != 0 {
		t.Errorf("%d guilds left in the source zone (the late guild must have been collected)", n)
	}
	if n := itCount(t, db, d.biz+".trade_listing WHERE market_zone = 902"); n != 3 {
		t.Errorf("target zone has %d listings, want 3", n)
	}
	if _, at := itListingZones(t, db, d.biz, 7002); at != 900 {
		t.Errorf("seller_zone_at_listing was rewritten: %d", at)
	}
	if n, _ := guildRdb.Exists(ctx, guildZoneRankKey(itSrcZone)).Result(); n != 0 {
		t.Error("source rank ZSET survived")
	}
	if n, _ := guildRdb.ZCard(ctx, guildZoneRankKey(itDstZone)).Result(); n != 3 {
		t.Errorf("target ZCARD=%d want 3", n)
	}
	if n, _ := guildRdb.Exists(ctx, guildCacheKey(11)).Result(); n != 0 {
		t.Error("guild:v2:11 cache not invalidated")
	}
	// 玩家路由行一行不动:home_zone 还是源区。
	players, err := collectPlayerIDsWithHomeZoneDB(ctx, d.store, itSrcZone)
	if err != nil || fmt.Sprint(players) != "[9901 9902]" {
		t.Fatalf("players with home_zone=901 = (%v, %v): the merge must not touch player_route", players, err)
	}
	// 清单。
	saved, err := loadRunManifest(path)
	if err != nil || saved == nil {
		t.Fatalf("manifest: (%v, %v)", saved, err)
	}
	for _, s := range []string{stepMergedInto, stepMergedIntoSettle, stepGuildMySQL, stepTradeMySQL, stepGuildRank, stepMergeDone} {
		if !saved.stepDone(s) {
			t.Errorf("step %s not recorded in the manifest", s)
		}
	}
	if fmt.Sprint(saved.GuildIDs) != "[11 13]" || fmt.Sprint(saved.TradeListingIDs) != "[7001 7002]" {
		t.Errorf("manifest scope: guilds=%v listings=%v", saved.GuildIDs, saved.TradeListingIDs)
	}

	// 验证:P7 的四行全 info。
	cfg := auditConfig{db: db, mappingDB: mapRdb, rankRDB: guildRdb, src: itSrcZone, dst: itDstZone, verify: true, manifest: saved,
		tradeSchema: d.biz, guildSchema: d.biz, routes: d.store}
	rows := runAuditMode(ctx, cfg)
	if err := validateMergeRecoveryRows(rows); err != nil {
		t.Fatalf("post-merge verification = %+v: %v", rows, err)
	}

	// 重跑幂等:什么都不做。
	if err := runMergeSteps(ctx, mergeStepPlan(ops, o), saved, d.gate(), false, func(step, _ string) error {
		t.Errorf("a finished merge must not persist %s again", step)
		return nil
	}); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	// 撤销。
	if err := checkUnmergeAllowed(ctx, rb, saved); err != nil {
		t.Fatal(err)
	}
	undo := &liveUnmergeSteps{o: o, src: itSrcZone, dst: itDstZone, m: saved, db: db, guildRdb: guildRdb, rb: rb, gate: d.gate()}
	if err := runUnmergeSteps(ctx, undo, d.gate()); err != nil {
		t.Fatalf("unmerge: %v", err)
	}
	if _, _, found, err := d.store.ReadMergedInto(ctx, itSrcZone); err != nil || found {
		t.Fatalf("zone_merged_into must be gone: found=%v err=%v", found, err)
	}
	if n, _ := itRedis(t, itMappingRD).Exists(ctx, mergedIntoKey(itSrcZone)).Result(); n != 0 {
		t.Error("the mirror must be gone")
	}
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 901"); n != 2 {
		t.Errorf("guilds not restored to the source zone: %d", n)
	}
	if n := itCount(t, db, d.biz+".guild WHERE zone_id = 902"); n != 1 {
		t.Errorf("target-zone guild count = %d, want 1 (the native)", n)
	}
	if n, _ := guildRdb.ZCard(ctx, guildZoneRankKey(itDstZone)).Result(); n != 1 {
		t.Errorf("target ZSET after unmerge = %d, want 1", n)
	}
	for _, id := range []uint64{7001, 7002} {
		if mz, _ := itListingZones(t, db, d.biz, id); mz != itSrcZone {
			t.Errorf("trade listing %d not restored: market_zone=%d", id, mz)
		}
	}
	if mz, _ := itListingZones(t, db, d.biz, 7101); mz != itDstZone {
		t.Errorf("a target-zone native listing was moved: market_zone=%d", mz)
	}
	// 撤销重跑幂等;这份清单不能再拿来续跑(每一步都会被跳过而什么都没合)。
	if err := runUnmergeSteps(ctx, undo, d.gate()); err != nil {
		t.Fatalf("unmerge re-run: %v", err)
	}
	if err := checkResumeMergedInto(ctx, rb, saved, itSrcZone, itDstZone); err == nil {
		t.Fatal("resuming from an unmerged manifest must be refused")
	}
	d.owner.session.CloseAdmission()
	if err := d.owner.session.Wait(ctx); err != nil {
		t.Fatalf("a clean run must not leave the tool in HOLD: %v", err)
	}
}
