package data

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

func TestDiffGuildIDSets_ReportsMissingAndGhostIDs(t *testing.T) {
	missing, ghosts := diffGuildIDSets(
		[]uint64{11, 22, 33, 44},
		[]uint64{11, 33, 55},
	)
	if !slices.Equal(missing, []uint64{22, 44}) || !slices.Equal(ghosts, []uint64{55}) {
		t.Fatalf("unexpected diff: missing=%v ghosts=%v", missing, ghosts)
	}
}

func TestDiffGuildIDSets_EmptyLegacySnapshotFailsForNonEmptyMySQL(t *testing.T) {
	missing, ghosts := diffGuildIDSets([]uint64{7, 8}, nil)
	if !slices.Equal(missing, []uint64{7, 8}) || len(ghosts) != 0 {
		t.Fatalf("unexpected diff: missing=%v ghosts=%v", missing, ghosts)
	}
}

func TestDiffGuildIDSets_MatchingSetsIgnoreOrder(t *testing.T) {
	missing, ghosts := diffGuildIDSets([]uint64{3, 1, 2}, []uint64{2, 3, 1})
	if len(missing) != 0 || len(ghosts) != 0 {
		t.Fatalf("matching sets produced diff: missing=%v ghosts=%v", missing, ghosts)
	}
}

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
	resetGuildAnnouncementIntegrationSchema(t, ctx, db)

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
		 (guild_id, name, leader_id, level, announcement, create_time_ms, max_members, zone_id, score)
		 VALUES (?, 'auth-test', 9001, 1, 'before', 1, 50, 1, 0)`, guildID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO guild_member
		 (guild_id, player_id, role, join_time_ms, last_active_ms, contribution)
		 VALUES (?, ?, ?, 1, 1, 0)`, guildID, playerID, constants.RoleOfficer)
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

func resetGuildAnnouncementIntegrationSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		"DROP TABLE IF EXISTS guild_member",
		"DROP TABLE IF EXISTS guild",
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
	for i, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("reset guild integration schema step %d failed: %v", i, err)
		}
	}
}
