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
		{"mysql 8 name collision", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '青云门' for key 'guild.uk_guild'"}, true},
		{"mysql 5.7 name collision", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '青云门' for key 'uk_guild'"}, true},
		{"wrapped", fmt.Errorf("insert guild: %w", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'guild.uk_guild'"}), true},
		{"primary key", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry '7' for key 'guild.PRIMARY'"}, false},
		{"name containing the index name", &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry 'a.uk_guild'' for key 'guild_member.uk_guild_member'"}, false},
		{"other mysql error", &mysqlDriver.MySQLError{Number: 1452, Message: "for key 'uk_guild'"}, false},
		{"not a mysql error", errors.New("Duplicate entry for key 'uk_guild'"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDuplicateKeyOn(tc.err, guildNameUniqueKey))
		})
	}
}

// openGuildIntegrationRepo 连 GUILD_TEST_MYSQL_DSN 指向的**测试库**并重建 guild 全部表;没设就跳过。
//
// DSN 必须先经 WithLockWaitTimeout:生产接线(svc/servicecontext.go)也是这么开的池,
// 测试不走这一步就等于在验一个与线上不同的数据库会话 —— 锁等待封顶(§6.2a)那条用例
// 会在 InnoDB 默认的 50s 上等待,既测不出封顶是否生效,也会把整包测试拖垮。
func openGuildIntegrationRepo(t *testing.T) (context.Context, *sql.DB, *GuildRepo) {
	t.Helper()
	dsn := os.Getenv("GUILD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GUILD_TEST_MYSQL_DSN 未设置，跳过公会 zone / 重名集成测试")
	}
	dsn, err := WithLockWaitTimeout(dsn)
	require.NoError(t, err)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, db.PingContext(ctx))
	resetGuildSchemaViaMigrate(t, ctx, db)

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

// TestApplyToGuildRejectsGuildFromAnotherZone:入帮的第一道 zone 防线搬到了申请这一步
// (直接入帮的 AddMemberInZone 已随申请制删除)。
//
// 归属区不符时不能只是"不加成员",连申请行都不许留下:留下的话,合服回滚之后那条跨区申请
// 仍躺在表里,被审批就造出一个跨区成员 —— ReviewApplication 锁内的 zone 复核是第二道防线,
// 但两道防线都该各自成立,不能互相当借口。
func TestApplyToGuildRejectsGuildFromAnotherZone(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID   uint64 = 7101
		applicant uint64 = 8101
		internal  uint64 = 8102
	)
	_, err := db.ExecContext(ctx,
		`INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		 VALUES (?, 'zone-two-guild', 'zone-two-guild', 9101, 1, '', 1, 50, 2, 0, 0)`, guildID)
	require.NoError(t, err)

	_, err = repo.ApplyToGuild(ctx, guildID, applicant, 3, testNowMs, testRules)
	assert.ErrorIs(t, err, ErrGuildZoneMismatch)
	assert.Zero(t, playerApplicationCount(t, ctx, db, applicant), "zone 不符时不能留下申请行")

	res, err := repo.ApplyToGuild(ctx, guildID, applicant, 2, testNowMs, testRules)
	require.NoError(t, err)
	assert.True(t, res.Inserted)
	assert.Equal(t, 1, playerApplicationCount(t, ctx, db, applicant))

	// requiredZone=0 保持内部调用(无会话、拿不到归属区)的旧语义:不校验 zone。
	res, err = repo.ApplyToGuild(ctx, guildID, internal, 0, testNowMs, testRules)
	require.NoError(t, err)
	assert.True(t, res.Inserted)
	assert.Equal(t, 1, playerApplicationCount(t, ctx, db, internal))
}

func TestCreateGuildMapsNameCollisionAcrossZones(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	founding := func(guildID, leaderID uint64, zoneID uint32, name string) *GuildData {
		return &GuildData{
			GuildID: guildID, Name: name, LeaderID: leaderID, Level: 1, MaxMembers: 50, ZoneID: zoneID,
			Members: []MemberData{{PlayerID: leaderID, Role: constants.RoleLeader, JoinTimeMs: 1, LastActiveMs: 1}},
		}
	}
	require.NoError(t, repo.CreateGuild(ctx, founding(7201, 8201, 1, "同名帮")))

	// 帮名全局唯一:另一个区建同名帮同样撞 uk_guild(name_norm),且事务整体回滚、会长行不残留。
	err := repo.CreateGuild(ctx, founding(7202, 8202, 2, "同名帮"))
	assert.ErrorIs(t, err, ErrGuildNameTaken)
	assert.Zero(t, memberCount(t, ctx, db, 8202))
}

// TestCreateGuildNameNormCollision:重名判定按 name_norm(NFKC → TrimSpace → 小写),
// 不依赖 MySQL 排序规则。大小写、全角、尾部空格三种写法都算同一个名字。
func TestCreateGuildNameNormCollision(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	founding := func(guildID, leaderID uint64, zoneID uint32, name string) *GuildData {
		return &GuildData{
			GuildID: guildID, Name: name, LeaderID: leaderID, Level: 1, MaxMembers: 50, ZoneID: zoneID,
			Members: []MemberData{{PlayerID: leaderID, Role: constants.RoleLeader, JoinTimeMs: 1, LastActiveMs: 1}},
		}
	}
	require.NoError(t, repo.CreateGuild(ctx, founding(7301, 8301, 1, "Qing云")))

	variants := []struct {
		guildID, leaderID uint64
		name              string
	}{
		{7302, 8302, "qing云"},
		{7303, 8303, "ＱＩＮＧ云"},
		{7304, 8304, "Qing云 "},
	}
	for _, v := range variants {
		err := repo.CreateGuild(ctx, founding(v.guildID, v.leaderID, 2, v.name))
		assert.ErrorIs(t, err, ErrGuildNameTaken, v.name)
		assert.Zero(t, memberCount(t, ctx, db, v.leaderID), v.name)
	}
}
