package data

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	"schemamigrate"
)

func TestCanSetAnnouncementOnlyAcceptsKnownPrivilegedRoles(t *testing.T) {
	tests := []struct {
		role uint32
		want bool
	}{
		{role: constants.RoleMember, want: false},
		{role: constants.RoleOfficer, want: true},
		{role: 2, want: false},
		{role: constants.RoleLeader, want: true},
		{role: 4, want: false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, canSetAnnouncement(tt.role), "role=%d", tt.role)
	}
}

func TestUpdateAnnouncementAuthorizedRejectsStalePrivilegedCache(t *testing.T) {
	dsn := os.Getenv("GUILD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GUILD_TEST_MYSQL_DSN 未设置，跳过公告权威授权测试")
	}

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	resetGuildSchemaViaMigrate(t, ctx, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := NewGuildRepo(rdb, db, time.Minute)

	const (
		guildID  uint64 = 7001
		playerID uint64 = 8001
	)
	_, err = db.ExecContext(ctx,
		`INSERT INTO guild
		 (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		 VALUES (?, 'auth-test', 'auth-test', 9001, 1, 'before', 1, 50, 1, 0, 0)`, guildID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO guild_member
		 (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
		 VALUES (?, ?, ?, 1, 1, 0, 0)`, guildID, playerID, constants.RoleOfficer)
	require.NoError(t, err)

	// 先填入含 officer 的 Redis 快照，再绕过 repo 直接在权威库降权，模拟缓存陈旧。
	cached, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, constants.RoleOfficer, cached.Members[0].Role)
	_, err = db.ExecContext(ctx,
		"UPDATE guild_member SET role=? WHERE guild_id=? AND player_id=?",
		constants.RoleMember, guildID, playerID)
	require.NoError(t, err)
	stillCachedOfficer, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, constants.RoleOfficer, stillCachedOfficer.Members[0].Role,
		"测试前提：Redis 仍保留降权前的 officer 快照")

	err = repo.UpdateAnnouncementAuthorized(ctx, guildID, playerID, "must-not-publish")
	assert.ErrorIs(t, err, ErrAnnouncementForbidden)
	var announcement string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT announcement FROM guild WHERE guild_id=?", guildID).Scan(&announcement))
	assert.Equal(t, "before", announcement)

	_, err = db.ExecContext(ctx,
		"UPDATE guild_member SET role=? WHERE guild_id=? AND player_id=?",
		constants.RoleOfficer, guildID, playerID)
	require.NoError(t, err)
	require.NoError(t, repo.UpdateAnnouncementAuthorized(ctx, guildID, playerID, "published"))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT announcement FROM guild WHERE guild_id=?", guildID).Scan(&announcement))
	assert.Equal(t, "published", announcement)

	// 成功写会失效旧缓存；重新填入 officer 快照后直接从权威库退会，仍必须拒绝。
	reloaded, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, constants.RoleOfficer, reloaded.Members[0].Role)
	_, err = db.ExecContext(ctx,
		"DELETE FROM guild_member WHERE guild_id=? AND player_id=?", guildID, playerID)
	require.NoError(t, err)
	departedButCached, err := repo.GetGuild(ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, constants.RoleOfficer, departedButCached.Members[0].Role,
		"测试前提：Redis 仍保留退会前的 officer 快照")

	err = repo.UpdateAnnouncementAuthorized(ctx, guildID, playerID, "departed-must-not-publish")
	assert.ErrorIs(t, err, ErrAnnouncementForbidden)
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT announcement FROM guild WHERE guild_id=?", guildID).Scan(&announcement))
	assert.Equal(t, "published", announcement)
}

// guildTestDropTables:本服务全部表 + schemamigrate 台账。新增表时同步追加(TestDropListCoversTables 守数量)。
var guildTestDropTables = []string{
	"guild_daily_counter", "guild_asset_op", "guild_player_op_seq", // B5a 资产三表
	"guild_application", "guild_member", "guild_player_state", "guild", "schema_migrations",
}

// guildTestDBNamePattern:测试只许碰这两类一次性库。appuser 对 testdb / zone_N_db / mmorpg_trade 也有 ALL 权限,
// 黑名单挡不住 DSN 指错,所以用白名单。
var guildTestDBNamePattern = regexp.MustCompile(`^(guild_test|guild_it_\d+_\d+)$`)

// resetGuildSchemaViaMigrate:DROP 全部表后经 schemamigrate.Up 重建 —— 与生产同一条建表路径,
// 表结构只来自 proto/guild/guild_db.proto,测试里不再手写 DDL。
// 建表后紧接着建全局插入守卫哨兵行(EnsureGlobalInsertGuard),与 guild.go 启动顺序一致:建帮、审批通过、
// 首次建状态行都要锁它,缺了一律 fail-closed。要验"哨兵缺失"的用例自己删掉它。
func resetGuildSchemaViaMigrate(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var dbName string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName))
	if !guildTestDBNamePattern.MatchString(dbName) {
		t.Fatalf("测试 DSN 指向库 %q:只允许 guild_test 或 guild_it_<pid>_<n>"+
			"(防止误删 data_service / trade / go/db 的表与台账)", dbName)
	}
	for _, table := range guildTestDropTables {
		_, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`")
		require.NoError(t, err, table)
	}
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{Database: dbName, Tables: Tables(), Logf: t.Logf})
	require.NoError(t, err)
	require.Empty(t, report.Manual, "新建库不应出现需人工项")
	// EnsureGlobalInsertGuard 只用 db,不碰 Redis。
	require.NoError(t, NewGuildRepo(nil, db, time.Minute).EnsureGlobalInsertGuard(ctx, testNowMs), "建全局插入守卫哨兵行")
}

// TestDropListCoversTables:新增表却忘了加进清理清单时,后续用例会在残留表上跑,必须红。
func TestDropListCoversTables(t *testing.T) {
	assert.Equal(t, len(Tables())+1, len(guildTestDropTables), "guildTestDropTables = 全部表 + schema_migrations")
}

func TestGuildTestDBNamePattern(t *testing.T) {
	for _, ok := range []string{"guild_test", "guild_it_123_4"} {
		assert.True(t, guildTestDBNamePattern.MatchString(ok), ok)
	}
	for _, bad := range []string{"mmorpg_guild", "mmorpg", "testdb", "zone_1_db", "mmorpg_trade", "guild_test2", ""} {
		assert.False(t, guildTestDBNamePattern.MatchString(bad), bad)
	}
}

// TestGuildNameNorm 钉住帮名唯一键的规范化公式(NFKC → TrimSpace → 小写)。
func TestGuildNameNorm(t *testing.T) {
	cases := map[string]string{
		"青云门":       "青云门",
		"  ABC ":    "abc",
		"ＡＢＣ":       "abc",
		"Ab　": "ab",
	}
	for in, want := range cases {
		got, ok := GuildNameNorm(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "   "} {
		_, ok := GuildNameNorm(bad)
		assert.False(t, ok, bad)
	}
	_, ok := GuildNameNorm(strings.Repeat("帮", constants.MaxGuildNameRunes))
	assert.True(t, ok, "24 个汉字必须允许")
	// ㍿ 经 NFKC 展开成 4 个字符,13 个即 52 rune,超过 MaxGuildNameNormRunes(48)。
	_, ok = GuildNameNorm(strings.Repeat("㍿", 13))
	assert.False(t, ok, "NFKC 展开后超长必须拒绝")
}
