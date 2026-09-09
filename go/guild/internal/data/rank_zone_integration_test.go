//go:build integration

package data

// 集成测试:公会归属 zone 的**权威来源**是 MySQL,不是 guild:v2:{id} 缓存。
//
// 跑法:  go test -tags=integration ./internal/data/...
// 连接参数默认取 etc/guild.yaml 的 MySQL.DataSource(本地 docker:
// root / deploy/docker-compose.yml 里的 MYSQL_ROOT_PASSWORD @ 127.0.0.1:3306),
// 可用 GUILD_IT_MYSQL_DSN 覆盖。库名**刻意不用 yaml 里的**:每个用例建一个
// guild_it_<pid>_<n> 一次性库,跑完 DROP,mmorpg 库一行都不碰。
// MySQL 连不上 / 没有建库权限 → Skip(与 data_service 的 storetest 同一约定)。
//
// 为什么必须打真库:被验的行为就是"在删除事务里 FOR UPDATE 读 zone_id",
// 用假 DB 替身写出来的测试只会验到替身自己的返回值。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// envDSN 覆盖 etc/guild.yaml 的 MySQL.DataSource。
	envDSN = "GUILD_IT_MYSQL_DSN"
	// envYaml 覆盖 guild.yaml 的路径。
	envYaml = "GUILD_IT_YAML"

	itDBPrefix        = "guild_it_"
	itConnTimeout     = 5 * time.Second
	errAccessDenied   = 1045
	errDBAccessDenied = 1044
)

var itDBCounter atomic.Uint32

// newRepoOnThrowawayDB 建一次性库 + 建表 + 起 miniredis,返回接好的 repo。
func newRepoOnThrowawayDB(t *testing.T) (*GuildRepo, *sql.DB, *miniredis.Miniredis) {
	t.Helper()

	baseDSN := resolveDSN(t)
	adminDSN, err := dsnWithDB(baseDSN, "")
	require.NoError(t, err)
	admin, err := sql.Open("mysql", adminDSN)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), itConnTimeout)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		if isMySQLErrNo(err, errAccessDenied) {
			t.Skipf("MySQL 拒绝该账号(设 %s 指向能 CREATE DATABASE 的账号): %v", envDSN, err)
		}
		t.Skipf("MySQL 不可达,跳过集成测试: %v", err)
	}

	name := fmt.Sprintf("%s%d_%d", itDBPrefix, os.Getpid(), itDBCounter.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		admin.Close()
		if isMySQLErrNo(err, errDBAccessDenied) {
			t.Skipf("该账号无法建库;用 %s 指向 root(本地口令见 deploy/docker-compose.yml): %v", envDSN, err)
		}
		t.Fatalf("建一次性库 %s 失败: %v", name, err)
	}

	itDSN, err := dsnWithDB(baseDSN, name)
	require.NoError(t, err)
	db, err := sql.Open("mysql", itDSN)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), itConnTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("DROP DATABASE %s 失败(请手工清理): %v", name, err)
		}
		admin.Close()
	})
	createGuildTables(t, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	return NewGuildRepo(rdb, db, time.Minute), db, mr
}

func createGuildTables(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE guild (
			guild_id BIGINT UNSIGNED NOT NULL,
			name VARCHAR(64) NOT NULL,
			leader_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
			level INT UNSIGNED NOT NULL DEFAULT 1,
			announcement TEXT,
			create_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
			max_members INT UNSIGNED NOT NULL DEFAULT 50,
			zone_id INT UNSIGNED NOT NULL DEFAULT 0,
			score BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (guild_id),
			UNIQUE KEY uk_name (name)
		) ENGINE=InnoDB`,
		`CREATE TABLE guild_member (
			guild_id BIGINT UNSIGNED NOT NULL,
			player_id BIGINT UNSIGNED NOT NULL,
			role TINYINT UNSIGNED NOT NULL DEFAULT 0,
			join_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
			last_active_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
			contribution BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (guild_id, player_id),
			UNIQUE KEY uk_player (player_id)
		) ENGINE=InnoDB`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("建表第 %d 步失败: %v", i, err)
		}
	}
}

func seedGuild(t *testing.T, db *sql.DB, guildID uint64, name string, zone uint32) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO guild (guild_id, name, leader_id, level, announcement, create_time_ms, max_members, zone_id, score)
		 VALUES (?, ?, ?, 1, '', 1, 50, ?, 0)`, guildID, name, guildID+1, zone)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution)
		 VALUES (?, ?, 3, 1, 1, 0)`, guildID, guildID+1)
	require.NoError(t, err)
}

// TestRemoveGuildFromRank_UsesAuthoritativeZoneNotCallerHint 是合服后那个
// "幽灵公会"的直接复现:调用方拿着(合服前的)源 zone 来清榜,而 MySQL 里公会
// 已经属于目标 zone。修复前 ZREM 打在源 zone 的空 ZSET 上,目标 zone 榜里的条目
// 永远留着 —— 榜首出现一个查不到名字的公会。
func TestRemoveGuildFromRank_UsesAuthoritativeZoneNotCallerHint(t *testing.T) {
	repo, db, _ := newRepoOnThrowawayDB(t)
	ctx := context.Background()

	const (
		guildID    uint64 = 5001
		sourceZone uint32 = 10 // 合服前的 zone,也是陈旧缓存里的值
		targetZone uint32 = 20 // MySQL 里的权威 zone(合服已改写)
	)
	seedGuild(t, db, guildID, "ghost-candidate", targetZone)

	// 两个分区榜里都放上它:目标 zone 是真实条目,源 zone 模拟历史残留。
	require.NoError(t, repo.UpdateGuildScore(ctx, guildID, targetZone, 999))
	require.NoError(t, repo.rdb.ZAdd(ctx, zoneRankKey(sourceZone),
		redis.Z{Score: 999, Member: guildID}).Err())

	// 调用方传的是**陈旧**的源 zone —— 修复前这一步只会删源 zone 的条目。
	require.NoError(t, repo.RemoveGuildFromRank(ctx, guildID, sourceZone))

	assertNotRanked(t, ctx, repo, guildRankKey, guildID)
	assertNotRanked(t, ctx, repo, zoneRankKey(targetZone), guildID)
	assertNotRanked(t, ctx, repo, zoneRankKey(sourceZone), guildID)
}

// TestDisbandPath_RemovesFromAuthoritativeZoneAfterRowIsGone 覆盖真实解散顺序:
// 先 DeleteGuild(行没了),再清榜。此时 MySQL 已经读不到 zone,唯一可信的来源
// 就是 DeleteGuild 从删除事务里带出来的那个值。
func TestDisbandPath_RemovesFromAuthoritativeZoneAfterRowIsGone(t *testing.T) {
	repo, db, _ := newRepoOnThrowawayDB(t)
	ctx := context.Background()

	const (
		guildID    uint64 = 5002
		staleZone  uint32 = 11
		actualZone uint32 = 21
	)
	seedGuild(t, db, guildID, "disband-me", actualZone)
	require.NoError(t, repo.UpdateGuildScore(ctx, guildID, actualZone, 500))
	require.NoError(t, repo.rdb.ZAdd(ctx, zoneRankKey(staleZone),
		redis.Z{Score: 500, Member: guildID}).Err())

	zoneFromDelete, err := repo.DeleteGuild(ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, actualZone, zoneFromDelete,
		"DeleteGuild 必须返回删除事务里 FOR UPDATE 读到的权威 zone —— 清榜只有它可信")

	require.NoError(t, repo.RemoveGuildFromRank(ctx, guildID, zoneFromDelete))
	assertNotRanked(t, ctx, repo, guildRankKey, guildID)
	assertNotRanked(t, ctx, repo, zoneRankKey(actualZone), guildID)
	assertNotRanked(t, ctx, repo, zoneRankKey(staleZone), guildID)

	// 行确实没了(解散是真的发生了,不是被闸门挡下)。
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM guild WHERE guild_id=?", guildID).Scan(&n))
	assert.Zero(t, n)
}

// TestRemoveGuildFromRank_OtherGuildsUntouched:全分区榜清扫只针对这一个 guild_id。
func TestRemoveGuildFromRank_OtherGuildsUntouched(t *testing.T) {
	repo, db, _ := newRepoOnThrowawayDB(t)
	ctx := context.Background()

	seedGuild(t, db, 5003, "victim", 30)
	seedGuild(t, db, 5004, "bystander", 30)
	require.NoError(t, repo.UpdateGuildScore(ctx, 5003, 30, 100))
	require.NoError(t, repo.UpdateGuildScore(ctx, 5004, 30, 200))

	require.NoError(t, repo.RemoveGuildFromRank(ctx, 5003, 30))

	assertNotRanked(t, ctx, repo, zoneRankKey(30), 5003)
	score, err := repo.rdb.ZScore(ctx, zoneRankKey(30), "5004").Result()
	require.NoError(t, err)
	assert.Equal(t, float64(200), score, "同区其它公会必须原封不动")
}

func assertNotRanked(t *testing.T, ctx context.Context, repo *GuildRepo, key string, guildID uint64) {
	t.Helper()
	_, err := repo.rdb.ZScore(ctx, key, fmt.Sprintf("%d", guildID)).Result()
	assert.ErrorIs(t, err, redis.Nil, "guild %d 不该还在 %s 里", guildID, key)
}

// ── DSN 解析 ────────────────────────────────────────────────────

// resolveDSN 取 guild.yaml 的 MySQL.DataSource,可被 GUILD_IT_MYSQL_DSN 覆盖。
// 不用 go-zero conf 加载整份 Config(那会触发 RpcServerConf 的必填校验),
// 直接从 yaml 里正则抓那一行 —— 这里只需要一个 DSN。
func resolveDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(envDSN); v != "" {
		return v
	}
	path := findGuildYaml(t)
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "读 %s", path)
	m := regexp.MustCompile(`(?m)^\s*DataSource:\s*"?([^"\r\n]+)"?\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s 里找不到 MySQL.DataSource;用 %s 指定 DSN", path, envDSN)
	}
	return strings.TrimSpace(string(m[1]))
}

func findGuildYaml(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(envYaml); v != "" {
		return v
	}
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "etc", "guild.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("在 %s 之上找不到 etc/guild.yaml;设 %s 或 %s", dir, envYaml, envDSN)
	return ""
}

// dsnWithDB 换掉 DSN 里的库名(空 = 不选库,用于建库/删库的管理连接)。
// 用驱动自己的解析器,免得手写字符串切割在带 # 的口令上出错。
func dsnWithDB(dsn, dbName string) (string, error) {
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	cfg.DBName = dbName
	return cfg.FormatDSN(), nil
}

func isMySQLErrNo(err error, number uint16) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == number
}
