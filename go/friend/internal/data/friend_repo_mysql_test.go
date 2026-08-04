package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcceptFriend_ConcurrentHardLimit 需要一个可破坏的隔离 MySQL schema。
// 默认跳过；显式设置 FRIEND_TEST_MYSQL_DSN 后会重建其中的 friend/migration 表。
// 这条测试验证真实 InnoDB 并发，不用 mock 伪造锁语义。
func TestAcceptFriend_ConcurrentHardLimit(t *testing.T) {
	dsn := os.Getenv("FRIEND_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("FRIEND_TEST_MYSQL_DSN 未设置，跳过真实 MySQL 并发上限测试")
	}

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	resetFriendIntegrationSchema(t, ctx, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := NewFriendRepo(rdb, db, time.Minute)

	const (
		targetID       uint64 = 9000
		requesterCount        = 20
		maxFriends     uint32 = 5
	)
	for i := 0; i < requesterCount; i++ {
		fromID := uint64(1000 + i)
		_, err := db.ExecContext(ctx,
			"INSERT INTO friend_request (from_player_id, to_player_id, request_time_ms, status) VALUES (?, ?, ?, 1)",
			fromID, targetID, time.Now().UnixMilli())
		require.NoError(t, err)
	}

	start := make(chan struct{})
	results := make(chan error, requesterCount)
	var wg sync.WaitGroup
	for i := 0; i < requesterCount; i++ {
		fromID := uint64(1000 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- repo.AcceptFriend(ctx, fromID, targetID, maxFriends)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	full := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAcceptorFriendsFull):
			full++
		default:
			t.Fatalf("unexpected AcceptFriend result: %v", err)
		}
	}
	assert.Equal(t, int(maxFriends), succeeded)
	assert.Equal(t, requesterCount-int(maxFriends), full)

	var targetRows, targetCapacity, acceptedRequests, pendingRequests int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend WHERE player_id = ?", targetID).Scan(&targetRows))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT friend_count FROM friend_capacity WHERE player_id = ?", targetID).Scan(&targetCapacity))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id = ? AND status = 2", targetID).Scan(&acceptedRequests))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend_request WHERE to_player_id = ? AND status = 1", targetID).Scan(&pendingRequests))

	assert.Equal(t, int(maxFriends), targetRows)
	assert.Equal(t, int(maxFriends), targetCapacity)
	assert.Equal(t, int(maxFriends), acceptedRequests)
	assert.Equal(t, requesterCount-int(maxFriends), pendingRequests,
		"满员失败必须回滚 pending->accepted，申请仍可在腾出名额后处理")

	var acceptedFrom uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT from_player_id FROM friend_request WHERE to_player_id = ? AND status = 2 LIMIT 1",
		targetID).Scan(&acceptedFrom))
	assert.ErrorIs(t, repo.AcceptFriend(ctx, acceptedFrom, targetID, 100), ErrRequestNotFound,
		"已接受申请不能被二次消费")

	var rejectedFrom uint64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT from_player_id FROM friend_request WHERE to_player_id = ? AND status = 1 LIMIT 1",
		targetID).Scan(&rejectedFrom))
	_, err = db.ExecContext(ctx,
		"UPDATE friend_request SET status=3 WHERE from_player_id=? AND to_player_id=?",
		rejectedFrom, targetID)
	require.NoError(t, err)
	assert.ErrorIs(t, repo.AcceptFriend(ctx, rejectedFrom, targetID, 100), ErrRequestNotFound,
		"已拒绝申请不能建立好友关系")

	const neverRequested uint64 = 777777
	assert.ErrorIs(t, repo.AcceptFriend(ctx, neverRequested, targetID, 100), ErrRequestNotFound,
		"从未申请的玩家不能建立好友关系")
	var strayCapacityRows int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id = ?", neverRequested).Scan(&strayCapacityRows))
	assert.Zero(t, strayCapacityRows, "无申请调用也不能制造无界 capacity 空行")
}

func TestInvalidateCaches_PropagatesRedisFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	repo := NewFriendRepo(rdb, nil, time.Minute)
	mr.Close()

	err := repo.invalidateCaches(context.Background(), "friends:1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalidate friend cache")
}

func TestFriendCapacityMigrationStateRequiresExplicitReady(t *testing.T) {
	tests := []struct {
		name  string
		state string
		found bool
		ok    bool
	}{
		{name: "ready", state: "ready", found: true, ok: true},
		{name: "pending", state: "pending", found: true},
		{name: "missing", found: false},
		{name: "legacy done is not ready", state: "done", found: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := friendCapacityMigrationStateError(tt.state, tt.found)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, ErrFriendCapacityMigrationNotReady)
		})
	}
}

func TestFriendCapacityHalfMigrationFailsClosedAndMissingRowUsesAuthoritativeCount(t *testing.T) {
	dsn := os.Getenv("FRIEND_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("FRIEND_TEST_MYSQL_DSN 未设置，跳过容量迁移门禁测试")
	}

	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))
	resetFriendIntegrationSchema(t, ctx, db)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := NewFriendRepo(rdb, db, time.Minute)

	const playerID uint64 = 4242
	for _, friendID := range []uint64{5001, 5002, 5003} {
		_, err := db.ExecContext(ctx,
			"INSERT INTO friend (player_id, friend_player_id, since_ms) VALUES (?, ?, 1)",
			playerID, friendID)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx,
		"UPDATE guild_schema_migration SET state='pending', completed_at=NULL WHERE migration_key=?",
		friendCapacityMigrationKey)
	require.NoError(t, err)

	err = repo.ensureFriendCapacityRows(ctx, playerID)
	assert.ErrorIs(t, err, ErrFriendCapacityMigrationNotReady)
	var rows int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM friend_capacity WHERE player_id=?", playerID).Scan(&rows))
	assert.Zero(t, rows, "pending 半迁移不得创建伪 0 capacity 行")

	_, err = db.ExecContext(ctx,
		"UPDATE guild_schema_migration SET state='ready', completed_at=CURRENT_TIMESTAMP WHERE migration_key=?",
		friendCapacityMigrationKey)
	require.NoError(t, err)
	require.NoError(t, repo.ensureFriendCapacityRows(ctx, playerID))

	var capacity int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT friend_count FROM friend_capacity WHERE player_id=?", playerID).Scan(&capacity))
	assert.Equal(t, 3, capacity, "ready 后缺行必须从 friend 权威边计数，不能初始化为 0")
}

func TestVersionedCache_RejectsStaleFillAfterWriteInvalidation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := NewFriendRepo(rdb, nil, time.Minute)
	ctx := context.Background()
	key := friendListKey(42)

	// reader 在 DB 读取前观察到 generation=0；writer 随后提交并失效缓存。
	require.NoError(t, repo.invalidateCaches(ctx, key))
	result, err := fillFriendCacheScript.Run(
		ctx,
		rdb,
		[]string{friendCacheGenerationKey(key), key},
		"0",
		`[{"friend_player_id":99}]`,
		time.Minute.Milliseconds(),
	).Int()
	require.NoError(t, err)
	assert.Equal(t, 0, result)
	assert.False(t, mr.Exists(key), "写入前读到的旧快照不得在失效之后回填")
}

func resetFriendIntegrationSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		"DROP TABLE IF EXISTS friend_capacity",
		"DROP TABLE IF EXISTS friend_request",
		"DROP TABLE IF EXISTS friend",
		"DROP TABLE IF EXISTS guild_schema_migration",
		`CREATE TABLE guild_schema_migration (
            migration_key VARCHAR(96) NOT NULL,
            state VARCHAR(16) NOT NULL DEFAULT 'pending',
            completed_at TIMESTAMP NULL DEFAULT NULL,
            PRIMARY KEY (migration_key)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend (
            player_id BIGINT UNSIGNED NOT NULL,
            friend_player_id BIGINT UNSIGNED NOT NULL,
            since_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id, friend_player_id)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend_request (
            from_player_id BIGINT UNSIGNED NOT NULL,
            to_player_id BIGINT UNSIGNED NOT NULL,
            request_time_ms BIGINT UNSIGNED NOT NULL DEFAULT 0,
            status TINYINT UNSIGNED NOT NULL DEFAULT 1,
            PRIMARY KEY (from_player_id, to_player_id),
            KEY idx_to_player (to_player_id, status)
        ) ENGINE=InnoDB`,
		`CREATE TABLE friend_capacity (
            player_id BIGINT UNSIGNED NOT NULL,
            friend_count INT UNSIGNED NOT NULL DEFAULT 0,
            PRIMARY KEY (player_id)
        ) ENGINE=InnoDB`,
		`INSERT INTO guild_schema_migration (migration_key, state, completed_at)
         VALUES ('friend_capacity_backfill_v1', 'ready', CURRENT_TIMESTAMP)`,
	}
	for i, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("reset friend integration schema step %d failed: %v\n%s", i, err, fmt.Sprintf("%.120s", statement))
		}
	}
}
