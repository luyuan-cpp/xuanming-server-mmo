package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
)

func TestIsDuplicateKeyOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"mysql 8 name collision", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '青云门' for key 'guild.uk_name'"}, true},
		{"mysql 5.7 name collision", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '青云门' for key 'uk_name'"}, true},
		{"wrapped", fmt.Errorf("insert guild: %w", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'guild.uk_name'"}), true},
		{"primary key", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '7' for key 'guild.PRIMARY'"}, false},
		{"name containing the index name", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry 'a.uk_name'' for key 'guild_member.uk_player'"}, false},
		{"other mysql error", &mysqlDriver.MySQLError{Number: 1452, Message: "for key 'uk_name'"}, false},
		{"not a mysql error", errors.New("Duplicate entry for key 'uk_name'"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDuplicateKeyOn(tc.err, guildNameUniqueKey))
		})
	}
}

// openGuildIntegrationRepo 连 GUILD_TEST_MYSQL_DSN 指向的**测试库**并重建 guild 两张表;没设就跳过。
func openGuildIntegrationRepo(t *testing.T) (context.Context, *sql.DB, *GuildRepo) {
	t.Helper()
	dsn := os.Getenv("GUILD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GUILD_TEST_MYSQL_DSN 未设置，跳过公会 zone / 重名集成测试")
	}
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, db.PingContext(ctx))
	resetGuildAnnouncementIntegrationSchema(t, ctx, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return ctx, db, NewGuildRepo(rdb, db, time.Minute)
}

func memberCount(t *testing.T, ctx context.Context, db *sql.DB, playerID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM guild_member WHERE player_id=?", playerID).Scan(&n))
	return n
}

func TestAddMemberInZoneRejectsGuildFromAnotherZone(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const guildID uint64 = 7101
	_, err := db.ExecContext(ctx,
		`INSERT INTO guild (guild_id, name, leader_id, level, announcement, create_time_ms, max_members, zone_id, score)
		 VALUES (?, 'zone-two-guild', 9101, 1, '', 1, 50, 2, 0)`, guildID)
	require.NoError(t, err)

	err = repo.AddMemberInZone(ctx, guildID, 8101, constants.RoleMember, 3)
	assert.ErrorIs(t, err, ErrGuildZoneMismatch)
	assert.Zero(t, memberCount(t, ctx, db, 8101), "zone 不符时不能留下成员行")

	require.NoError(t, repo.AddMemberInZone(ctx, guildID, 8101, constants.RoleMember, 2))
	assert.Equal(t, 1, memberCount(t, ctx, db, 8101))

	// requiredZone=0(AddMember)保持内部调用的旧语义:不校验 zone。
	require.NoError(t, repo.AddMember(ctx, guildID, 8102, constants.RoleMember))
	assert.Equal(t, 1, memberCount(t, ctx, db, 8102))
}

func TestCreateGuildMapsNameCollisionAcrossZones(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	founding := func(guildID, leaderID uint64, zoneID uint32) *GuildData {
		return &GuildData{
			GuildID: guildID, Name: "同名帮", LeaderID: leaderID, Level: 1, MaxMembers: 50, ZoneID: zoneID,
			Members: []MemberData{{PlayerID: leaderID, Role: constants.RoleLeader, JoinTimeMs: 1, LastActiveMs: 1}},
		}
	}
	require.NoError(t, repo.CreateGuild(ctx, founding(7201, 8201, 1)))

	// 帮名全局唯一:另一个区建同名帮同样撞 uk_name,且事务整体回滚、会长行不残留。
	err := repo.CreateGuild(ctx, founding(7202, 8202, 2))
	assert.ErrorIs(t, err, ErrGuildNameTaken)
	assert.Zero(t, memberCount(t, ctx, db, 8202))
}
