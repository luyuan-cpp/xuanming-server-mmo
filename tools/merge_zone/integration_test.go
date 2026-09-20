//go:build merge_integration

package main

// 对着**真实** MySQL(127.0.0.1:3306)与 Redis(127.0.0.1:6379)跑的集成测试。
//
//	go test -tags merge_integration ./...
//
// 凭据来自 deploy/docker-compose.yml(root / Mmorpg#2026db)。Kafka 不需要:
// 积压检查是可注入的(kafkaLagSource),这里注入假的。
//
// 隔离策略:
//   - MySQL:只用一次性库 zone_901_db / zone_902_db / merge_zone_it_db,
//     TestMain 建、结束时 DROP。真实的 mmorpg / zone_1_db / zone_2_db 一个字节不碰。
//     guild 表建在 merge_zone_it_db 里(DSN 的默认库),所以代码里那些不带库名的
//     `FROM guild` 落在一次性库上。
//   - MySQL(聚宝斋):一次性库 merge_zone_it_trade,经 -trade-schema 指过去。真实的
//     mmorpg_trade 一个字节不碰 —— 这正是 -trade-schema 这个 flag 存在的唯一理由。
//   - MySQL(好友):一次性库 merge_zone_it_friend,经 -friend-schema 指过去,理由同上
//     (friend 2026-09-18 迁到独占库 mmorpg_friend,port-decisions D-14)。
//   - Redis:用 DB 9/10/11(mapping/guild/shared),**且只在它们本来
//     就是空的时候跑**。非空就 skip 而不是 flush —— 谁也不知道那里面是谁的数据。
//     生产默认的 15/2/0 由 merge_unit_test.go 的常量测试守住,这里不碰。
//     (原先还有一个 DB 12 给 friend:online 用;那把键与 -friend-redis-* 参数已随
//     friend 移植 F3 一起退役。)

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

const (
	itMySQLBase = "root:Mmorpg#2026db@tcp(127.0.0.1:3306)/"
	itRedisAddr = "127.0.0.1:6379"

	itSrcZone = uint32(901)
	itDstZone = uint32(902)

	itGuildDB   = "merge_zone_it_db"
	itTradeDB   = "merge_zone_it_trade"
	itFriendDB  = "merge_zone_it_friend"
	itMappingRD = 9
	itGuildRD   = 10
	itSharedRD  = 11
	itSceneRD   = itSharedRD // scene_manager 生产上也是 DB 0,与 shared 同库
)

var itBinary string

func itDSN() string {
	return itMySQLBase + itGuildDB + "?charset=utf8mb4&parseTime=true&loc=Local&multiStatements=true"
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	root, err := sql.Open("mysql", itMySQLBase+"?multiStatements=true")
	if err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: mysql open: %v\n", err)
		os.Exit(0)
	}
	if err := root.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: mysql unreachable: %v\n", err)
		os.Exit(0)
	}
	for _, addr := range []int{itMappingRD, itGuildRD, itSharedRD} {
		c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: addr})
		n, err := c.DBSize(ctx).Result()
		_ = c.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "SKIP: redis db %d unreachable: %v\n", addr, err)
			os.Exit(0)
		}
		if n != 0 {
			fmt.Fprintf(os.Stderr, "SKIP: redis db %d is not empty (%d keys); refusing to touch someone else's data\n", addr, n)
			os.Exit(0)
		}
	}

	itDropAll(root)
	if err := itCreateAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: schema setup: %v\n", err)
		itDropAll(root)
		os.Exit(1)
	}

	// 端到端用例要跑真二进制(runMerge 用 log.Fatalf,不能在进程内调)。
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

func itDropAll(db *sql.DB) {
	for _, s := range []string{zoneDBName(itSrcZone), zoneDBName(itDstZone), itGuildDB, itTradeDB, itFriendDB} {
		_, _ = db.Exec("DROP DATABASE IF EXISTS " + s)
	}
	ctx := context.Background()
	for _, dbi := range []int{itMappingRD, itGuildRD, itSharedRD} {
		c := redis.NewClient(&redis.Options{Addr: itRedisAddr, DB: dbi})
		_ = c.FlushDB(ctx).Err() // 只清我们自己确认过是空的那几个库
		_ = c.Close()
	}
}

// itCreateAll 建三份一次性库。player 表的形状镜像 proto2mysql 的产物:
// player_id 主键 + 若干 MEDIUMBLOB 组件列。列的具体内容对合服无关紧要,
// 关键是「有 player_id 列」与「两库列结构一致」。
func itCreateAll(db *sql.DB) error {
	stmts := []string{
		"CREATE DATABASE " + itGuildDB,
		"CREATE DATABASE " + zoneDBName(itSrcZone),
		"CREATE DATABASE " + zoneDBName(itDstZone),
		"CREATE DATABASE " + itTradeDB,
		"CREATE DATABASE " + itFriendDB,
		// 镜像 deploy/mysql-init/guild_friend_tables.sql(name 全局 UNIQUE)。
		`CREATE TABLE ` + itGuildDB + `.guild (
			guild_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL,
			zone_id INT UNSIGNED NOT NULL DEFAULT 0, score BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (guild_id), UNIQUE KEY uk_name (name), KEY idx_zone (zone_id))`,
		// friend / friend_request 住独占库 mmorpg_friend(port-decisions D-14),这里用一次性库
		// 顶替。只建审计读到的列:完整形状由 go/schemamigrate 按 proto/friend/friend_table.proto
		// 生成,这里不复刻第二份表结构(与 trade_listing 同口径)。
		// 列名按 proto 的字段名:申请表的对端列是 to_player_id(不是旧 mysql-init 里的 target_id)。
		`CREATE TABLE ` + itFriendDB + `.friend (
			player_id BIGINT UNSIGNED NOT NULL, friend_player_id BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (player_id, friend_player_id))`,
		`CREATE TABLE ` + itFriendDB + `.friend_request (
			from_player_id BIGINT UNSIGNED NOT NULL, to_player_id BIGINT UNSIGNED NOT NULL,
			status TINYINT NOT NULL DEFAULT 1, PRIMARY KEY (from_player_id, to_player_id))`,
		`CREATE TABLE ` + itGuildDB + `.guild_member (
			guild_id BIGINT UNSIGNED NOT NULL, player_id BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (guild_id, player_id))`,
		// 只建合服步骤读写的列 + 按 market_zone / 卖家查的索引。完整形状由 go/schemamigrate
		// 按 proto/trade/trade_table.proto 生成,这里不复刻第二份表结构。
		`CREATE TABLE ` + itTradeDB + `.trade_listing (
			listing_id BIGINT UNSIGNED NOT NULL, seller_player_id BIGINT UNSIGNED NOT NULL,
			market_zone INT UNSIGNED NOT NULL DEFAULT 0, seller_zone_at_listing INT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (listing_id), KEY idx_market_zone (market_zone), KEY idx_seller (seller_player_id, listing_id))`,
	}
	for _, zone := range []uint32{itSrcZone, itDstZone} {
		s := zoneDBName(zone)
		stmts = append(stmts,
			`CREATE TABLE `+s+`.player_database (
				player_id BIGINT UNSIGNED NOT NULL, transform MEDIUMBLOB, currency MEDIUMBLOB,
				PRIMARY KEY (player_id))`,
			`CREATE TABLE `+s+`.player_database_1 (
				player_id BIGINT UNSIGNED NOT NULL, stress_test_probe MEDIUMBLOB,
				PRIMARY KEY (player_id))`,
			`CREATE TABLE `+s+`.player_centre_database (
				player_id BIGINT UNSIGNED NOT NULL, scene_info MEDIUMBLOB,
				PRIMARY KEY (player_id))`,
			// 账号表:在建表清单里,但**没有** player_id 列。发现逻辑必须跳过它,
			// 否则 INSERT ... SELECT 会因为列不匹配炸掉(或更糟,搬走别人的账号)。
			`CREATE TABLE `+s+`.user (
				id BIGINT UNSIGNED NOT NULL, display_name VARCHAR(64), PRIMARY KEY (id))`,
		)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
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
	for _, zone := range []uint32{itSrcZone, itDstZone} {
		for _, tbl := range []string{"player_database", "player_database_1", "player_centre_database", "user"} {
			if _, err := db.Exec("DELETE FROM " + zoneDBName(zone) + "." + tbl); err != nil {
				t.Fatal(err)
			}
		}
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
}

// itSeedPlayers 在源 zone 库里造 n 个玩家(三张表都有行)并写 mapping。
func itSeedPlayers(t *testing.T, ids []uint64, withMapping bool) {
	t.Helper()
	db := itOpen(t)
	s := zoneDBName(itSrcZone)
	for _, id := range ids {
		if _, err := db.Exec("INSERT INTO "+s+".player_database (player_id, transform, currency) VALUES (?,?,?)",
			id, []byte(fmt.Sprintf("transform-%d", id)), []byte(fmt.Sprintf("currency-%d", id))); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO "+s+".player_database_1 (player_id, stress_test_probe) VALUES (?,?)",
			id, []byte("probe")); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO "+s+".player_centre_database (player_id, scene_info) VALUES (?,?)",
			id, []byte("scene")); err != nil {
			t.Fatal(err)
		}
	}
	if withMapping {
		ctx := context.Background()
		m := itRedis(t, itMappingRD)
		for _, id := range ids {
			if err := m.Set(ctx, playerZoneKeyPrefix+strconv.FormatUint(id, 10),
				strconv.FormatUint(uint64(itSrcZone), 10), 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func itTableListFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysql_database_table_list.json")
	raw, _ := json.Marshal(tableListFile{Messages: []string{
		"user", "user_oauth", "account_share_database",
		"player_centre_database", "player_database", "player_database_1",
	}})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func itCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// itSeedListing 在一次性 trade 库里造一条商品。
func itSeedListing(t *testing.T, db *sql.DB, listingID, seller uint64, marketZone, sellerZoneAtListing uint32) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO "+itTradeDB+".trade_listing (listing_id, seller_player_id, market_zone, seller_zone_at_listing) VALUES (?,?,?,?)",
		listingID, seller, marketZone, sellerZoneAtListing); err != nil {
		t.Fatal(err)
	}
}

// itListingZones 读一条商品的 market_zone 与 seller_zone_at_listing。
func itListingZones(t *testing.T, db *sql.DB, listingID uint64) (marketZone, sellerZoneAtListing uint32) {
	t.Helper()
	if err := db.QueryRow("SELECT market_zone, seller_zone_at_listing FROM "+itTradeDB+".trade_listing WHERE listing_id = ?",
		listingID).Scan(&marketZone, &sellerZoneAtListing); err != nil {
		t.Fatal(err)
	}
	return marketZone, sellerZoneAtListing
}

// ── 表发现 ───────────────────────────────────────────────────

func TestIT_DiscoverPlayerTables_SkipsTablesWithoutPlayerIdColumn(t *testing.T) {
	itReset(t)
	db := itOpen(t)
	candidates, err := loadTableListJSON(itTableListFile(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := discoverPlayerTables(context.Background(), db, zoneDBName(itSrcZone), candidates)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"player_centre_database", "player_database", "player_database_1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v (user has no player_id; user_oauth does not exist in the zone DB)", got, want)
	}
}

func TestIT_AssertSchemaExists(t *testing.T) {
	db := itOpen(t)
	ctx := context.Background()
	if err := assertSchemaExists(ctx, db, zoneDBName(itDstZone)); err != nil {
		t.Fatalf("existing schema reported missing: %v", err)
	}
	if err := assertSchemaExists(ctx, db, "zone_999999_db"); err == nil {
		t.Fatal("a non-existent target zone DB must be refused — that is the 'target zone exists' pre-flight")
	}
}

// ── 玩家行拷贝(item 2 的核心)──────────────────────────────

func TestIT_CopyPlayerRows_CopiesEveryTableAndVerifiesCounts(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	ids := []uint64{9001, 9002, 9003}
	itSeedPlayers(t, ids, false)
	// 第三个玩家只有主表,没有 _1 / centre 行:合法(没触发过那些功能)。
	if _, err := db.Exec("DELETE FROM " + zoneDBName(itSrcZone) + ".player_database_1 WHERE player_id = 9003"); err != nil {
		t.Fatal(err)
	}

	tables := []string{"player_centre_database", "player_database", "player_database_1"}
	rep, err := copyPlayerRows(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone), tables, ids, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := itCount(t, db, zoneDBName(itDstZone)+".player_database"); got != 3 {
		t.Errorf("player_database rows in target = %d, want 3", got)
	}
	if got := itCount(t, db, zoneDBName(itDstZone)+".player_database_1"); got != 2 {
		t.Errorf("player_database_1 rows in target = %d, want 2", got)
	}
	if rep.MissingRows["player_database_1"] != 1 {
		t.Errorf("missing-row accounting wrong: %+v", rep.MissingRows)
	}
	// blob 列必须原样搬过去 —— 这是「玩家数据没丢」的实际判据。
	var transform []byte
	if err := db.QueryRow("SELECT transform FROM " + zoneDBName(itDstZone) + ".player_database WHERE player_id = 9001").Scan(&transform); err != nil {
		t.Fatal(err)
	}
	if string(transform) != "transform-9001" {
		t.Errorf("blob column not copied verbatim: %q", transform)
	}
	// 源库的行不动 —— 合服不删源数据,回滚要靠它。
	if got := itCount(t, db, zoneDBName(itSrcZone)+".player_database"); got != 3 {
		t.Errorf("source rows were modified: %d", got)
	}
}

func TestIT_CopyPlayerRows_RefusesWhenTargetAlreadyHoldsTheId(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	ids := []uint64{9101}
	itSeedPlayers(t, ids, false)
	// 目标区已经有同号玩家 = ID 安全事件(两个 zone 发出了同一个 player_id,
	// 或者这次合服已经跑过一半)。必须中止且一个字节都不写。
	if _, err := db.Exec("INSERT INTO "+zoneDBName(itDstZone)+".player_database (player_id, transform) VALUES (?,?)",
		9101, []byte("native-target-player")); err != nil {
		t.Fatal(err)
	}
	_, err := copyPlayerRows(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone),
		[]string{"player_database"}, ids, false)
	if err == nil {
		t.Fatal("expected an ID-safety refusal")
	}
	if !strings.Contains(err.Error(), "ID SAFETY") {
		t.Errorf("error should name the ID safety event: %v", err)
	}
	// 目标区原有的那行必须原封不动。
	var transform []byte
	if err := db.QueryRow("SELECT transform FROM " + zoneDBName(itDstZone) + ".player_database WHERE player_id = 9101").Scan(&transform); err != nil {
		t.Fatal(err)
	}
	if string(transform) != "native-target-player" {
		t.Errorf("target row was overwritten: %q", transform)
	}
}

func TestIT_CopyPlayerRows_DryRunWritesNothing(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	ids := []uint64{9201, 9202}
	itSeedPlayers(t, ids, false)
	rep, err := copyPlayerRows(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone),
		[]string{"player_database"}, ids, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SourceRows["player_database"] != 2 {
		t.Errorf("dry-run should still report the real source count: %+v", rep.SourceRows)
	}
	if got := itCount(t, db, zoneDBName(itDstZone)+".player_database"); got != 0 {
		t.Fatalf("dry-run wrote %d rows", got)
	}
}

func TestIT_DeletePlayerRows_OnlyRemovesByteIdenticalCopies(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	ids := []uint64{9301, 9302}
	itSeedPlayers(t, ids, false)
	if _, err := copyPlayerRows(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone),
		[]string{"player_database"}, ids, false); err != nil {
		t.Fatal(err)
	}
	// 9302 在合服后被玩过 —— 目标区那一行已经不是拷贝了。
	if _, err := db.Exec("UPDATE "+zoneDBName(itDstZone)+".player_database SET currency = ? WHERE player_id = 9302",
		[]byte("earned-after-the-merge")); err != nil {
		t.Fatal(err)
	}
	deleted, refused, err := deletePlayerRows(ctx, db, zoneDBName(itSrcZone), zoneDBName(itDstZone),
		"player_database", ids, false)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted=%d want 1 (only the untouched copy)", deleted)
	}
	if len(refused) != 1 || refused[0] != 9302 {
		t.Errorf("refused=%v want [9302] — a row played after the merge must never be deleted", refused)
	}
	var currency []byte
	if err := db.QueryRow("SELECT currency FROM " + zoneDBName(itDstZone) + ".player_database WHERE player_id = 9302").Scan(&currency); err != nil {
		t.Fatal(err)
	}
	if string(currency) != "earned-after-the-merge" {
		t.Errorf("post-merge data destroyed: %q", currency)
	}
}

// ── 共享缓存失效 ─────────────────────────────────────────────

func TestIT_InvalidatePlayerCaches_DeletesLoginAndDbCacheKeys(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	shared := itRedis(t, itSharedRD)
	ids := []uint64{9401, 9402}
	tables := []string{"player_database", "player_database_1", "player_centre_database"}
	for _, id := range ids {
		for _, k := range playerCacheKeys(id, tables) {
			if err := shared.Set(ctx, k, "stale-from-source-zone", 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 别人的键不能被误删。
	if err := shared.Set(ctx, "PlayerAllData:99999", "someone-else", 0).Err(); err != nil {
		t.Fatal(err)
	}
	n, err := invalidatePlayerCaches(ctx, shared, ids, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(ids) * len(playerCacheKeys(ids[0], tables)); n != want {
		t.Errorf("deleted=%d want %d", n, want)
	}
	for _, id := range ids {
		for _, k := range playerCacheKeys(id, tables) {
			if v, _ := shared.Exists(ctx, k).Result(); v != 0 {
				t.Errorf("%s survived — login would keep serving the source-zone snapshot", k)
			}
		}
	}
	if v, _ := shared.Exists(ctx, "PlayerAllData:99999").Result(); v != 1 {
		t.Error("an unrelated player's cache was deleted")
	}
}

// ── mapping ──────────────────────────────────────────────────

func TestIT_CollectAndRemapMapping(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	m := itRedis(t, itMappingRD)
	// 源区 3 个,目标区 2 个,第三个 zone 1 个(绝不能被动)。
	for _, id := range []uint64{9501, 9502, 9503} {
		m.Set(ctx, playerZoneKeyPrefix+strconv.FormatUint(id, 10), "901", 0)
	}
	for _, id := range []uint64{9511, 9512} {
		m.Set(ctx, playerZoneKeyPrefix+strconv.FormatUint(id, 10), "902", 0)
	}
	m.Set(ctx, playerZoneKeyPrefix+"9599", "903", 0)

	ids, err := collectPlayerIDsWithHomeZone(ctx, m, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 9501 || ids[2] != 9503 {
		t.Fatalf("collected %v, want sorted [9501 9502 9503]", ids)
	}

	matched, updated, err := remapPlayerMapping(ctx, m, itSrcZone, itDstZone, true)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 3 || updated != 0 {
		t.Errorf("dry-run: matched=%d updated=%d want 3/0", matched, updated)
	}
	if v, _ := m.Get(ctx, playerZoneKeyPrefix+"9501").Result(); v != "901" {
		t.Fatalf("dry-run wrote: %q", v)
	}

	matched, updated, err = remapPlayerMapping(ctx, m, itSrcZone, itDstZone, false)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 3 || updated != 3 {
		t.Errorf("apply: matched=%d updated=%d want 3/3", matched, updated)
	}
	if v, _ := m.Get(ctx, playerZoneKeyPrefix+"9599").Result(); v != "903" {
		t.Errorf("a third zone's player was remapped: %q", v)
	}
	after, _ := collectPlayerIDsWithHomeZone(ctx, m, itDstZone)
	if len(after) != 5 {
		t.Errorf("target zone now has %d players, want 5", len(after))
	}
}

func TestIT_BackfillHomeZone_NeverOverwritesAnExistingMapping(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	m := itRedis(t, itMappingRD)
	ids := []uint64{9601, 9602, 9603}
	itSeedPlayers(t, ids, false)
	// 9602 已经被合到别的区去了 —— 回填绝不能把他送回坟场。
	m.Set(ctx, playerZoneKeyPrefix+"9602", "902", 0)

	db := itOpen(t)
	rep, err := backfillHomeZone(ctx, m, newMySQLPlayerIDSource(db, itSrcZone), itSrcZone, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RowsScanned != 3 || rep.Created != 2 || rep.Mismatched != 1 {
		t.Fatalf("report=%+v want scanned=3 created=2 mismatched=1", rep)
	}
	if v, _ := m.Get(ctx, playerZoneKeyPrefix+"9602").Result(); v != "902" {
		t.Errorf("backfill overwrote a merged player's mapping: %q", v)
	}
	if v, _ := m.Get(ctx, playerZoneKeyPrefix+"9601").Result(); v != "901" {
		t.Errorf("backfill did not seed 9601: %q", v)
	}
}

// ── guild ────────────────────────────────────────────────────

func TestIT_MergeRankZSET_IsAtomicAndRoundTripsThroughUnmerge(t *testing.T) {
	itReset(t)
	ctx := context.Background()
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

func TestIT_InvalidateGuildCaches_IncrementsGenerationAndDropsTheEntry(t *testing.T) {
	itReset(t)
	ctx := context.Background()
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
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'alpha',901),(21,'beta',902)"); err != nil {
		t.Fatal(err)
	}
	// name 全局 UNIQUE ⇒ 跨 zone 重名不可能存在 ⇒ 探测恒通过。
	if err := assertNoGuildNameCollision(ctx, db, itSrcZone, itDstZone); err != nil {
		t.Fatalf("guild.name is globally unique, so no collision is possible: %v", err)
	}
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (12,'beta',901)"); err == nil {
		t.Fatal("the schema no longer enforces UNIQUE(name); the collision path must be revisited")
	}
}

func TestIT_MigrateGuildZone(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'a',901),(12,'b',901),(21,'c',902)"); err != nil {
		t.Fatal(err)
	}
	gids, err := collectGuildIDsInZone(ctx, db, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(gids) != 2 || gids[0] != 11 {
		t.Fatalf("guild ids = %v", gids)
	}
	if n, err := migrateGuildZone(ctx, db, itSrcZone, itDstZone, true); err != nil || n != 2 {
		t.Fatalf("dry-run: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 2 {
		t.Fatalf("dry-run wrote: %d rows still in 901, want 2", n)
	}
	if n, err := migrateGuildZone(ctx, db, itSrcZone, itDstZone, false); err != nil || n != 2 {
		t.Fatalf("apply: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 0 {
		t.Errorf("%d guilds left in the source zone", n)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 902"); n != 3 {
		t.Errorf("target zone has %d guilds, want 3", n)
	}
}

// ── 聚宝斋 trade_listing ─────────────────────────────────────

func TestIT_TradeMarketZone_RewriteVerifyRestore(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	itSeedListing(t, db, 7001, 9901, itSrcZone, itSrcZone)
	itSeedListing(t, db, 7002, 9902, itSrcZone, 900)       // 更早一次合服搬进 901 的商品
	itSeedListing(t, db, 7101, 9950, itDstZone, itDstZone) // 目标区原住民

	// 门禁:库在且表在才放行;库不在、库在表不在都拒绝。
	if err := assertTradeListingReady(ctx, db, itTradeDB); err != nil {
		t.Fatalf("the trade table exists but was refused: %v", err)
	}
	if err := assertTradeListingReady(ctx, db, "merge_zone_it_no_such_trade"); err == nil {
		t.Error("a missing trade schema must be refused (fail-closed)")
	}
	if err := assertTradeListingReady(ctx, db, itGuildDB); err == nil {
		t.Error("a schema without trade_listing (trade never migrated) must be refused")
	}

	ids, err := collectTradeListingIDsInZone(ctx, db, itTradeDB, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids) != "[7001 7002]" {
		t.Fatalf("source listing ids = %v, want [7001 7002]", ids)
	}

	cfg := auditConfig{db: db, tradeSchema: itTradeDB, src: itSrcZone, dst: itDstZone}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
		t.Errorf("before the rewrite the verifier must block on real data: %+v", r)
	}

	// 清单收集之后才落到源区的商品(P1 trade 不读合服围栏):改写只动清单里的 id,它必须原地不动。
	itSeedListing(t, db, 7003, 9903, itSrcZone, itSrcZone)

	// dry-run 只数清单里的,不写。
	if n, err := migrateTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, true); err != nil || n != 2 {
		t.Fatalf("dry-run: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 3 {
		t.Fatalf("dry-run wrote: %d listings left in the source zone, want 3", n)
	}

	if n, err := migrateTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, false); err != nil || n != 2 {
		t.Fatalf("apply: n=%d err=%v", n, err)
	}
	if mz, _ := itListingZones(t, db, 7003); mz != itSrcZone {
		t.Errorf("listing 7003 is not in the manifest but was rewritten to market_zone=%d", mz)
	}
	// 复查口径:源区剩 1 条,merge_run.go 步骤 3b 据此中止且不标记完成;-verify-merged 也必须拦住。
	if left, err := countTradeListingsInZone(ctx, db, itTradeDB, itSrcZone); err != nil || left != 1 {
		t.Fatalf("source zone left=%d err=%v, want 1 (the listing outside the manifest)", left, err)
	}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "block" || isInfraAudit(r) {
		t.Errorf("a listing left in the source zone must block the verifier: %+v", r)
	}

	// 续跑口径:重新收集并进清单,再改写。
	more, err := collectTradeListingIDsInZone(ctx, db, itTradeDB, itSrcZone)
	if err != nil {
		t.Fatal(err)
	}
	ids = sortedUint64(append(ids, more...))
	if fmt.Sprint(ids) != "[7001 7002 7003]" {
		t.Fatalf("merged manifest ids = %v, want [7001 7002 7003]", ids)
	}
	if n, err := migrateTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, false); err != nil || n != 1 {
		t.Fatalf("resume apply: n=%d err=%v, want 1 row", n, err)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 0 {
		t.Errorf("%d listings left in the source zone", n)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 902"); n != 4 {
		t.Errorf("target zone has %d listings, want 4", n)
	}
	// 空清单什么都不改。
	if n, err := migrateTradeMarketZone(ctx, db, itTradeDB, nil, itSrcZone, itDstZone, false); err != nil || n != 0 {
		t.Errorf("empty manifest: n=%d err=%v, want 0 rows", n, err)
	}
	// seller_zone_at_listing 是审计原值,合服不改。
	if _, at := itListingZones(t, db, 7001); at != itSrcZone {
		t.Errorf("seller_zone_at_listing of 7001 = %d, want %d (must not be rewritten)", at, itSrcZone)
	}
	if _, at := itListingZones(t, db, 7002); at != 900 {
		t.Errorf("seller_zone_at_listing of 7002 = %d, want 900 (must not be rewritten)", at)
	}
	if r := verifyTradeMarketZoneDrained(ctx, cfg); r.Severity != "info" {
		t.Errorf("after the rewrite the verifier must pass: %+v", r)
	}
	// 重跑幂等。
	if n, err := migrateTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, false); err != nil || n != 0 {
		t.Errorf("re-run: n=%d err=%v, want 0 rows", n, err)
	}

	// 撤销:只改清单 id 里当前在 dst 的;原住民 7101 不在清单里,不动。
	if n, err := restoreTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, true); err != nil || n != 3 {
		t.Fatalf("restore dry-run: n=%d err=%v", n, err)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 902"); n != 4 {
		t.Fatalf("restore dry-run wrote: target zone has %d listings", n)
	}
	if n, err := restoreTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, false); err != nil || n != 3 {
		t.Fatalf("restore: n=%d err=%v", n, err)
	}
	for _, id := range []uint64{7001, 7002, 7003} {
		if mz, _ := itListingZones(t, db, id); mz != itSrcZone {
			t.Errorf("listing %d market_zone = %d after restore, want %d", id, mz, itSrcZone)
		}
	}
	if mz, _ := itListingZones(t, db, 7101); mz != itDstZone {
		t.Errorf("target-zone native listing moved by the restore: market_zone=%d", mz)
	}
	if n, err := restoreTradeMarketZone(ctx, db, itTradeDB, ids, itSrcZone, itDstZone, false); err != nil || n != 0 {
		t.Errorf("restore re-run: n=%d err=%v, want 0 rows", n, err)
	}
}

// ── 围栏与锁 ─────────────────────────────────────────────────

func TestIT_MergeFence_RefusesASecondRunAndReleasesOnlyItsOwn(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	m := itRedis(t, itMappingRD)

	f1, err := acquireMergeFence(ctx, m, itSrcZone, itDstZone, "run-A", "m.json", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	// 契约:两个 zone 都被打标。
	for _, z := range []uint32{itSrcZone, itDstZone} {
		raw, gerr := m.Get(ctx, mergeFenceKey(z)).Result()
		if gerr != nil {
			t.Fatalf("fence for zone %d missing: %v", z, gerr)
		}
		var v mergeInProgressValue
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("fence value is not JSON: %v (%s)", err, raw)
		}
		if v.RunID != "run-A" || v.Tool != "tools/merge_zone" {
			t.Errorf("fence value = %+v", v)
		}
		ttl, _ := m.TTL(ctx, mergeFenceKey(z)).Result()
		if ttl <= 0 {
			t.Errorf("fence for zone %d has no TTL (%v) — a killed tool would deadlock the shard forever", z, ttl)
		}
	}

	// 第二个进程必须被拒。
	if _, err := acquireMergeFence(ctx, m, itSrcZone, itDstZone, "run-B", "m2.json", time.Minute, false); err == nil {
		t.Fatal("a concurrent merge was allowed to start")
	}

	// 别人的 run 不能释放我的围栏。
	fake := &mergeFence{rdb: m, keys: []string{mergeFenceKey(itSrcZone)}, runID: "run-B", ttl: time.Minute, stop: make(chan struct{})}
	fake.release(ctx)
	if n, _ := m.Exists(ctx, mergeFenceKey(itSrcZone)).Result(); n != 1 {
		t.Fatal("run-B released run-A's fence")
	}

	f1.release(ctx)
	for _, z := range []uint32{itSrcZone, itDstZone} {
		if n, _ := m.Exists(ctx, mergeFenceKey(z)).Result(); n != 0 {
			t.Errorf("fence for zone %d not released", z)
		}
	}
}

func TestIT_AcquireGuildRankLock_BlocksWhileTheGuildServiceHoldsIt(t *testing.T) {
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

func itPreflightDeps(t *testing.T, lag kafkaLagSource) preflightDeps {
	t.Helper()
	return preflightDeps{
		sceneRdb:   itRedis(t, itSceneRD),
		mappingRdb: itRedis(t, itMappingRD),
		sharedRdb:  itRedis(t, itSharedRD),
		lag:        lag,
	}
}

func itPreflightParams(ids []uint64) preflightParams {
	return preflightParams{src: itSrcZone, dst: itDstZone, kafkaGroup: defaultKafkaGroup, topicGeneration: 1, playerIDs: ids}
}

func TestIT_Preflight_PassesOnACleanZone(t *testing.T) {
	itReset(t)
	if err := runPreflight(context.Background(), itPreflightDeps(t, fakeLagSource{}), itPreflightParams([]uint64{9701})); err != nil {
		t.Fatalf("clean zone rejected: %v", err)
	}
}

func TestIT_Preflight_BlocksOnEachHazard(t *testing.T) {
	ids := []uint64{9711}
	topic := dbTaskTopic(itSrcZone, 1)
	cases := []struct {
		name   string
		lag    kafkaLagSource
		setup  func(t *testing.T)
		expect string
	}{
		{"live scene node", fakeLagSource{}, func(t *testing.T) {
			itRedis(t, itSceneRD).ZAdd(context.Background(), fmt.Sprintf(sceneNodeLoadKeyFmt, itSrcZone), redis.Z{Member: "10"})
		}, "P2"},
		{"no lag source at all", nil, func(*testing.T) {}, "P3"},
		{"kafka backlog", fakeLagSource{lag: 7}, func(*testing.T) {}, "P3"},
		{"kafka lag query failed", fakeLagSource{err: fmt.Errorf("broker down")}, func(*testing.T) {}, "P3"},
		{"retry queue not empty", fakeLagSource{}, func(t *testing.T) {
			itRedis(t, itSharedRD).RPush(context.Background(), dbRetryQueueKey(topic), "payload")
		}, "P4"},
		{"dead queue not empty", fakeLagSource{}, func(t *testing.T) {
			itRedis(t, itSharedRD).RPush(context.Background(), dbDeadQueueKey(topic), "payload")
		}, "P4"},
		{"player lock held", fakeLagSource{}, func(t *testing.T) {
			itRedis(t, itMappingRD).Set(context.Background(), "lock:player:9711", "tok", time.Minute)
		}, "P5"},
		{"session still online", fakeLagSource{}, func(t *testing.T) {
			itRedis(t, itSharedRD).Set(context.Background(), "player:session:9711", "gate-3", time.Minute)
		}, "P6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			itReset(t)
			c.setup(t)
			err := runPreflight(context.Background(), itPreflightDeps(t, c.lag), itPreflightParams(ids))
			if err == nil {
				t.Fatalf("%s did not block the merge", c.name)
			}
			if !strings.Contains(err.Error(), c.expect) {
				t.Errorf("expected %s in %q", c.expect, err)
			}
		})
	}
}

// ── 守卫(item 1)────────────────────────────────────────────

func TestIT_GuardPlayerSet_RefusesEmptyMappingAndIncompleteBackfill(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	base := options{sourceZone: itSrcZone, mappingAddr: itRedisAddr, mappingDB: itMappingRD, expectedSrcPlayers: -1}

	// (a) 一个玩家都没收集到 —— 空库时也算,因为「跑了等于没跑」。
	err := guardPlayerSet(ctx, db, zoneDBName(itSrcZone), nil, base, false)
	if err == nil {
		t.Fatal("an empty player set must be refused (silent no-op merge)")
	}
	if !strings.Contains(err.Error(), "-mapping-redis-db") || !strings.Contains(err.Error(), "backfill") {
		t.Errorf("the refusal must name both likely causes: %v", err)
	}
	// -allow-empty-source 明确放行。
	permissive := base
	permissive.allowEmptySource = true
	if err := guardPlayerSet(ctx, db, zoneDBName(itSrcZone), nil, permissive, false); err != nil {
		t.Fatalf("-allow-empty-source should permit an empty zone: %v", err)
	}

	// (b) 库里 3 个玩家,mapping 只覆盖 2 个 → 必须让人先跑回填。
	itSeedPlayers(t, []uint64{9801, 9802, 9803}, false)
	err = guardPlayerSet(ctx, db, zoneDBName(itSrcZone), []uint64{9801, 9802}, base, false)
	if err == nil {
		t.Fatal("an incomplete mapping must be refused")
	}
	if !strings.Contains(err.Error(), "backfill-home-zone") {
		t.Errorf("the refusal must point at the backfill: %v", err)
	}
	// 覆盖齐了就放行。
	if err := guardPlayerSet(ctx, db, zoneDBName(itSrcZone), []uint64{9801, 9802, 9803}, base, false); err != nil {
		t.Fatalf("a complete mapping was refused: %v", err)
	}

	// (c) T-1 记的人数与现在不一致 = zone-down 不彻底。
	rehearsed := base
	rehearsed.expectedSrcPlayers = 5
	if err := guardPlayerSet(ctx, db, zoneDBName(itSrcZone), []uint64{9801, 9802, 9803}, rehearsed, false); err == nil {
		t.Fatal("a player-count drift since the rehearsal must abort the merge")
	}
}

// ── 端到端 ───────────────────────────────────────────────────

// itRun 跑真二进制,返回合并输出与退出码。
func itRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	base := []string{
		"-mysql-dsn", itDSN(),
		"-redis-addr", itRedisAddr,
		"-redis-db", strconv.Itoa(itGuildRD),
		"-mapping-redis-db", strconv.Itoa(itMappingRD),
		"-notice-redis-db", strconv.Itoa(itSharedRD),
		"-scene-redis-db", strconv.Itoa(itSceneRD),
		"-table-list-json", itTableListFile(t),
		"-assume-kafka-drained",
		"-trade-schema", itTradeDB,
		"-friend-schema", itFriendDB,
	}
	cmd := exec.Command(itBinary, append(base, args...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

func TestIT_EndToEnd_MergeVerifyUnmerge(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	mapRdb := itRedis(t, itMappingRD)
	guildRdb := itRedis(t, itGuildRD)
	shared := itRedis(t, itSharedRD)

	srcIDs := []uint64{9901, 9902, 9903}
	itSeedPlayers(t, srcIDs, true)
	// 目标区原住民:合服全程不得被碰。
	if _, err := db.Exec("INSERT INTO "+zoneDBName(itDstZone)+".player_database (player_id, transform) VALUES (?,?)",
		9950, []byte("native")); err != nil {
		t.Fatal(err)
	}
	mapRdb.Set(ctx, playerZoneKeyPrefix+"9950", "902", 0)

	if _, err := db.Exec("INSERT INTO " + itGuildDB + ".guild (guild_id, name, zone_id) VALUES (11,'src-a',901),(21,'dst-a',902)"); err != nil {
		t.Fatal(err)
	}
	guildRdb.ZAdd(ctx, guildZoneRankKey(itSrcZone), redis.Z{Member: "11", Score: 100})
	guildRdb.ZAdd(ctx, guildZoneRankKey(itDstZone), redis.Z{Member: "21", Score: 70})
	guildRdb.Set(ctx, guildCacheKey(11), `{"zone_id":901}`, 0)
	// 从源库读出来、缓存在共享 DB 0 上的那份必须被清掉。
	shared.Set(ctx, "PlayerAllData:9901", "stale", 0)
	// 聚宝斋:源区两条(7002 是更早一次合服搬进 901 的)+ 目标区原住民一条。
	itSeedListing(t, db, 7001, 9901, itSrcZone, itSrcZone)
	itSeedListing(t, db, 7002, 9902, itSrcZone, 900)
	itSeedListing(t, db, 7101, 9950, itDstZone, itDstZone)

	manifest := filepath.Join(t.TempDir(), "merge.json")
	zoneArgs := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest}

	// 1) dry-run 不写。
	out, code := itRun(t, append(append([]string{}, zoneArgs...), "-dry-run")...)
	if code != 0 {
		t.Fatalf("dry-run exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "3 players with home_zone=901") {
		t.Errorf("dry-run did not report the player count:\n%s", out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 1 {
		t.Fatalf("dry-run wrote player rows: %d", n)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9901").Result(); v != "901" {
		t.Fatalf("dry-run remapped: %q", v)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 2 {
		t.Fatalf("dry-run rewrote trade listings: %d left in the source zone", n)
	}

	// 2) apply。
	out, code = itRun(t, append(append([]string{}, zoneArgs...), "-apply", "-expected-src-players", "3")...)
	if code != 0 {
		t.Fatalf("apply exit=%d\n%s", code, out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 4 {
		t.Errorf("target player_database has %d rows, want 4 (3 merged + 1 native)", n)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9901").Result(); v != "902" {
		t.Errorf("mapping not remapped: %q", v)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 0 {
		t.Errorf("%d guilds left in the source zone", n)
	}
	if n, _ := guildRdb.Exists(ctx, guildZoneRankKey(itSrcZone)).Result(); n != 0 {
		t.Error("source rank ZSET survived")
	}
	if n, _ := guildRdb.ZCard(ctx, guildZoneRankKey(itDstZone)).Result(); n != 2 {
		t.Errorf("target ZCARD=%d want 2", n)
	}
	if n, _ := guildRdb.Exists(ctx, guildCacheKey(11)).Result(); n != 0 {
		t.Error("guild:v2:11 cache not invalidated — the guild would keep its old zone for 30 minutes")
	}
	if n, _ := shared.Exists(ctx, "PlayerAllData:9901").Result(); n != 0 {
		t.Error("the shared login cache was not invalidated")
	}
	// 公告 flag 必须落在 login DB,不是 mapping DB。
	if n, _ := shared.Exists(ctx, "player_merge_notice:9901").Result(); n != 1 {
		t.Error("post-merge notice flag missing from the login Redis DB")
	}
	if n, _ := mapRdb.Exists(ctx, "player_merge_notice:9901").Result(); n != 0 {
		t.Error("notice flag was written to the mapping DB, where login never looks")
	}
	// 围栏必须释放。
	for _, z := range []uint32{itSrcZone, itDstZone} {
		if n, _ := mapRdb.Exists(ctx, mergeFenceKey(z)).Result(); n != 0 {
			t.Errorf("merge fence for zone %d left behind", z)
		}
	}
	// 清单必须存在且步骤全 done。
	man, err := loadManifest(manifest)
	if err != nil || man == nil {
		t.Fatalf("manifest: %v", err)
	}
	for _, s := range []string{stepPlayerRows, stepGuildMySQL, stepGuildRank, stepPlayerMapping, stepPostMerge} {
		if !man.stepDone(s) {
			t.Errorf("step %s not recorded in the manifest", s)
		}
	}
	if len(man.PlayerIDs) != 3 || len(man.GuildIDs) != 1 || len(man.RankMembers) != 1 {
		t.Errorf("manifest scope wrong: %d players %d guilds %d rank", len(man.PlayerIDs), len(man.GuildIDs), len(man.RankMembers))
	}
	// 聚宝斋:源区商品全部改到目标区,seller_zone_at_listing 保持原值,清单记下被搬的 id。
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 0 {
		t.Errorf("%d trade listings left in the source zone", n)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 902"); n != 3 {
		t.Errorf("target zone has %d trade listings, want 3 (2 merged + 1 native)", n)
	}
	if _, at := itListingZones(t, db, 7002); at != 900 {
		t.Errorf("seller_zone_at_listing was rewritten: %d", at)
	}
	if !man.stepDone(stepTradeMySQL) {
		t.Errorf("step %s not recorded in the manifest", stepTradeMySQL)
	}
	if fmt.Sprint(man.TradeListingIDs) != "[7001 7002]" {
		t.Errorf("manifest trade listing ids = %v, want [7001 7002]", man.TradeListingIDs)
	}

	// 3) -verify-merged 全绿。
	out, code = itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902",
		"-verify-merged", "-expected-src-players", "3")
	if code != 0 {
		t.Fatalf("verify exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "0 block(s)") {
		t.Errorf("post-merge verification reported blockers:\n%s", out)
	}
	if !strings.Contains(out, "verify:trade_listing") {
		t.Errorf("post-merge verification did not check trade_listing:\n%s", out)
	}

	// 4) 重跑 -apply 是幂等的(读清单,不重新扫描)。
	out, code = itRun(t, append(append([]string{}, zoneArgs...), "-apply")...)
	if code != 0 {
		t.Fatalf("re-run exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "RESUME") {
		t.Errorf("the re-run did not consume the manifest:\n%s", out)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 4 {
		t.Errorf("the re-run duplicated rows: %d", n)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 902"); n != 3 {
		t.Errorf("the re-run changed trade listings: %d in the target zone", n)
	}

	// 5) unmerge 撤回,只碰清单里的对象。
	out, code = itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply")
	if code != 0 {
		t.Fatalf("unmerge exit=%d\n%s", code, out)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9901").Result(); v != "901" {
		t.Errorf("mapping not restored: %q", v)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9950").Result(); v != "902" {
		t.Errorf("a target-zone native was moved by the unmerge: %q", v)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 901"); n != 1 {
		t.Errorf("guild not restored to the source zone: %d", n)
	}
	if n := itCount(t, db, itGuildDB+".guild WHERE zone_id = 902"); n != 1 {
		t.Errorf("target-zone guild count = %d, want 1 (the native)", n)
	}
	if n, _ := guildRdb.ZCard(ctx, guildZoneRankKey(itDstZone)).Result(); n != 1 {
		t.Errorf("target ZSET after unmerge = %d, want 1", n)
	}
	if n := itCount(t, db, zoneDBName(itDstZone)+".player_database"); n != 1 {
		t.Errorf("target player rows after unmerge = %d, want 1 (only the native)", n)
	}
	if n, _ := shared.Exists(ctx, "player_merge_notice:9901").Result(); n != 0 {
		t.Error("the merge notice survived the unmerge")
	}
	for _, id := range []uint64{7001, 7002} {
		if mz, _ := itListingZones(t, db, id); mz != itSrcZone {
			t.Errorf("trade listing %d not restored to the source zone: market_zone=%d", id, mz)
		}
	}
	if mz, _ := itListingZones(t, db, 7101); mz != itDstZone {
		t.Errorf("a target-zone native trade listing was moved by the unmerge: market_zone=%d", mz)
	}
}

func TestIT_EndToEnd_RefusesEmptySourceAndSkipPlayerRowsWithoutAttestation(t *testing.T) {
	itReset(t)
	// 空 mapping:合服会是静默 no-op,必须拒绝。
	out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-apply",
		"-manifest-path", filepath.Join(t.TempDir(), "m.json"))
	if code == 0 {
		t.Fatalf("an empty source zone was merged:\n%s", out)
	}
	if !strings.Contains(out, "silent no-op") {
		t.Errorf("the refusal should explain why:\n%s", out)
	}

	// -skip-player-rows 不带声明 = 拒绝。
	out, code = itRun(t, "-source-zone", "901", "-target-zone", "902", "-apply", "-skip-player-rows")
	if code == 0 {
		t.Fatalf("-skip-player-rows was accepted without the attestation:\n%s", out)
	}
	if !strings.Contains(out, "i-know-global-player-table") {
		t.Errorf("the refusal should name the attestation flag:\n%s", out)
	}

	// 聚宝斋库不在 = 拒绝(fail-closed),在任何写之前;报错要点名迁移命令与 -skip-trade-mysql。
	// 后给的 -trade-schema 覆盖 itRun 基础参数里的那个。
	out, code = itRun(t, "-source-zone", "901", "-target-zone", "902", "-dry-run",
		"-trade-schema", "merge_zone_it_no_such_trade", "-manifest-path", filepath.Join(t.TempDir(), "m2.json"))
	if code == 0 {
		t.Fatalf("a merge without the trade schema was accepted:\n%s", out)
	}
	if !strings.Contains(out, "-skip-trade-mysql") || !strings.Contains(out, "-migrate") {
		t.Errorf("the trade refusal should name the migration and the skip flag:\n%s", out)
	}
	// 显式 -skip-trade-mysql:越过 trade 门禁,停在后面的空源区守卫上 —— 证明跳过确实生效、且只跳过 trade。
	out, code = itRun(t, "-source-zone", "901", "-target-zone", "902", "-dry-run", "-skip-trade-mysql",
		"-trade-schema", "merge_zone_it_no_such_trade", "-manifest-path", filepath.Join(t.TempDir(), "m3.json"))
	if code == 0 {
		t.Fatalf("an empty source zone was accepted:\n%s", out)
	}
	if !strings.Contains(out, "trade SKIPPED") || !strings.Contains(out, "silent no-op") {
		t.Errorf("-skip-trade-mysql should pass the trade gate and stop at the empty-source guard:\n%s", out)
	}
}

func TestIT_Unmerge_RefusesMissingTradeSchemaBeforeAnyWrite(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	mapRdb := itRedis(t, itMappingRD)
	// 合服之后的状态:玩家已指向目标区,清单里记着一条搬过的聚宝斋商品。
	dstVal := strconv.FormatUint(uint64(itDstZone), 10)
	mapRdb.Set(ctx, playerZoneKeyPrefix+"9961", dstVal, 0)
	manifest := filepath.Join(t.TempDir(), "merge.json")
	m := newMergeManifest("run-trade-preflight", itSrcZone, itDstZone, "it", time.Now())
	m.PlayerIDs = []uint64{9961}
	m.TradeListingIDs = []uint64{7201}
	if err := saveManifest(manifest, m); err != nil {
		t.Fatal(err)
	}

	// -trade-schema 指错:必须在 5' mapping 改回之前就拒绝(先证明,再写)。
	out, code := itRun(t, "-mode", "unmerge", "-manifest-path", manifest, "-apply",
		"-trade-schema", "merge_zone_it_no_such_trade")
	if code == 0 {
		t.Fatalf("an unmerge whose manifest lists trade listings was accepted without the trade schema:\n%s", out)
	}
	if !strings.Contains(out, "trade listings to restore") {
		t.Errorf("the refusal should say why (trade listings in the manifest):\n%s", out)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9961").Result(); v != dstVal {
		t.Errorf("the mapping was restored before the trade preflight refused: %q", v)
	}
	for _, z := range []uint32{itSrcZone, itDstZone} {
		if n, _ := mapRdb.Exists(ctx, mergeFenceKey(z)).Result(); n != 0 {
			t.Errorf("merge fence for zone %d left behind by a refused unmerge", z)
		}
	}
}

// 步骤 3b 复查中止经真二进制走一遍「中止 → 重跑」:runMerge 用 log.Fatal 中止,defer 的
// fence.release 不执行,围栏按设计留在本次 run_id 上。断言:非 0 退出、步骤不标完成、文案给出
// GET/DEL 指引且 run_id 对得上;不 DEL 直接重跑被围栏拒;照文案 DEL 后原命令重跑成功。
//
// 「清单落盘之后才进源区的商品」用触发器确定性制造:步骤 1 往目标库插玩家行时,触发器顺手往
// 源区插一条商品 —— 它必然发生在清单落盘之后、步骤 3b 之前,不靠时序赛跑。
// (「dry-run 写好清单后再插商品」造不出中止:-apply 的清单阶段会把它重新并进清单。)
func TestIT_TradeMarketZone_ResidualAbortKeepsFenceThenResumesAfterDEL(t *testing.T) {
	itReset(t)
	ctx := context.Background()
	db := itOpen(t)
	mapRdb := itRedis(t, itMappingRD)

	itSeedPlayers(t, []uint64{9971}, true)
	itSeedListing(t, db, 7001, 9971, itSrcZone, itSrcZone)

	trigger := zoneDBName(itDstZone) + ".it_trade_late_listing"
	if _, err := db.Exec(fmt.Sprintf("CREATE TRIGGER %s AFTER INSERT ON %s.player_centre_database FOR EACH ROW "+
		"INSERT IGNORE INTO %s.trade_listing (listing_id, seller_player_id, market_zone, seller_zone_at_listing) "+
		"VALUES (7003, NEW.player_id, %d, %d)", trigger, zoneDBName(itDstZone), itTradeDB, itSrcZone, itSrcZone)); err != nil {
		t.Fatalf("create trigger (needs TRIGGER privilege; with binlog on also SUPER or log_bin_trust_function_creators): %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec("DROP TRIGGER IF EXISTS " + trigger) })

	manifest := filepath.Join(t.TempDir(), "merge.json")
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest,
		"-apply", "-expected-src-players", "1"}
	srcFence, dstFence := mergeFenceKey(itSrcZone), mergeFenceKey(itDstZone)

	// 1) 首跑:步骤 1 的触发器落下 7003;3b 只搬清单里的 7001,复查剩 1 条 → 中止。
	out, code := itRun(t, args...)
	if code == 0 {
		t.Fatalf("the merge passed although a listing reached the source zone after the manifest was written:\n%s", out)
	}
	if !strings.Contains(out, "is NOT marked done") || !strings.Contains(out, "DEL "+srcFence+" "+dstFence) {
		t.Errorf("the 3b abort must say how to clear the fence before re-running:\n%s", out)
	}
	if mz, _ := itListingZones(t, db, 7001); mz != itDstZone {
		t.Errorf("manifest listing 7001 market_zone = %d, want %d", mz, itDstZone)
	}
	if mz, _ := itListingZones(t, db, 7003); mz != itSrcZone {
		t.Errorf("listing 7003 is outside the manifest but was rewritten to market_zone=%d", mz)
	}
	man, err := loadManifest(manifest)
	if err != nil || man == nil {
		t.Fatalf("manifest after the abort: %v", err)
	}
	if !man.stepDone(stepPlayerRows) {
		t.Errorf("step %s should be done before 3b (the trigger fires there)", stepPlayerRows)
	}
	if man.stepDone(stepTradeMySQL) || man.stepDone(stepPlayerMapping) {
		t.Errorf("steps after the 3b abort must not be marked done: %+v", man.Steps)
	}
	if fmt.Sprint(man.TradeListingIDs) != "[7001]" {
		t.Errorf("manifest trade listing ids = %v, want [7001]", man.TradeListingIDs)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9971").Result(); v != "901" {
		t.Errorf("mapping flipped although step 3b aborted: %q", v)
	}

	// 2) 围栏按设计留着,挂在本次 run_id 上,且文案里的 run_id 与之一致(运维 GET 核对用)。
	for _, key := range []string{srcFence, dstFence} {
		raw, gerr := mapRdb.Get(ctx, key).Result()
		if gerr != nil {
			t.Fatalf("%s should be left in place after the 3b abort: %v", key, gerr)
		}
		var v mergeInProgressValue
		if uerr := json.Unmarshal([]byte(raw), &v); uerr != nil {
			t.Fatalf("%s value: %v", key, uerr)
		}
		if v.RunID == "" || !strings.Contains(out, "still owned by run_id="+v.RunID) {
			t.Errorf("abort message does not name the run_id held by %s (%q):\n%s", key, v.RunID, out)
		}
	}

	// 3) 不 DEL 直接重跑:被残留围栏拒绝(文案预告的那句),且在任何写之前。
	out, code = itRun(t, args...)
	if code == 0 || !strings.Contains(out, "another merge is already fencing zone") {
		t.Fatalf("a re-run without clearing the fence should be refused by the fence (exit=%d):\n%s", code, out)
	}
	if mz, _ := itListingZones(t, db, 7003); mz != itSrcZone {
		t.Errorf("the refused re-run rewrote listing 7003: market_zone=%d", mz)
	}

	// 4) 照文案:停种子入口(删触发器)→ DEL 两把围栏 → 原命令重跑。
	if _, err := db.Exec("DROP TRIGGER " + trigger); err != nil {
		t.Fatal(err)
	}
	if err := mapRdb.Del(ctx, srcFence, dstFence).Err(); err != nil {
		t.Fatal(err)
	}
	out, code = itRun(t, args...)
	if code != 0 {
		t.Fatalf("re-run after DEL exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "RESUME") {
		t.Errorf("the re-run did not consume the manifest:\n%s", out)
	}
	if n := itCount(t, db, itTradeDB+".trade_listing WHERE market_zone = 901"); n != 0 {
		t.Errorf("%d trade listings left in the source zone after the resumed run", n)
	}
	man, err = loadManifest(manifest)
	if err != nil || man == nil {
		t.Fatalf("manifest after the resumed run: %v", err)
	}
	if fmt.Sprint(man.TradeListingIDs) != "[7001 7003]" {
		t.Errorf("resumed manifest trade listing ids = %v, want [7001 7003]", man.TradeListingIDs)
	}
	if !man.stepDone(stepTradeMySQL) || !man.stepDone(stepPlayerMapping) {
		t.Errorf("resumed run did not finish the remaining steps: %+v", man.Steps)
	}
	if v, _ := mapRdb.Get(ctx, playerZoneKeyPrefix+"9971").Result(); v != "902" {
		t.Errorf("mapping not remapped by the resumed run: %q", v)
	}
	for _, key := range []string{srcFence, dstFence} {
		if n, _ := mapRdb.Exists(ctx, key).Result(); n != 0 {
			t.Errorf("%s left behind by the resumed run", key)
		}
	}
}

func TestIT_Audit_ExitsTwoWhenAHandleIsMissing(t *testing.T) {
	itReset(t)
	// 指向一个没人监听的 Redis:审计跑不成,必须 exit 2(不是 0,也不是 1)。
	cmd := exec.Command(itBinary, "-mode", "audit", "-source-zone", "901", "-target-zone", "902",
		"-mysql-dsn", itDSN(), "-redis-addr", "127.0.0.1:6399", "-table-list-json", itTableListFile(t))
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
	itReset(t)
	ctx := context.Background()
	itSeedPlayers(t, []uint64{9971}, true)
	// player:session 住在 SHARED DB(生产上是 DB 0,由 player_locator 写)。
	// 2026-09-18 起它是在线门禁的唯一证据面:friend:online 那把键全仓没有写者,已随
	// friend 移植 F3 从门禁里删掉(见 audit_checks.go)。
	itRedis(t, itSharedRD).Set(ctx, "player:session:9971", "1", time.Minute)

	out, code := itRun(t, "-mode", "audit", "-source-zone", "901", "-target-zone", "902")
	if code != 1 {
		t.Fatalf("exit=%d want 1 (a player still online must block the merge)\n%s", code, out)
	}
	if !strings.Contains(out, "online_presence") {
		t.Errorf("the online gate did not fire:\n%s", out)
	}
}
