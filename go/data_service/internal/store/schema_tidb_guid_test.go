//go:build integration

package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"data_service/internal/store"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql/sqltest"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// 显式本地TiDB目标，仅创建随机自有schema；主体、归属原件与控制目录由共享fixture统一持有。
func openTiDBGuidFixture(t *testing.T) (*sql.DB, store.MySQLConfig, *sqltest.Fixture) {
	t.Helper()
	dsn := os.Getenv("DATA_SERVICE_GUID_TIDB_DSN")
	if dsn == "" {
		if os.Getenv("DATA_SERVICE_GUID_REQUIRE_TIDB") == "1" {
			t.Fatal("真实TiDB GUID验收要求显式DSN")
		}
		t.Skip("未启用独立TiDB GUID验收")
	}
	require.Equal(t, "dev", os.Getenv("DATA_SERVICE_GUID_MODE"))
	require.Equal(t, "1", os.Getenv("DATA_SERVICE_GUID_DEDICATED"))
	parsed, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	host, _, err := net.SplitHostPort(parsed.Addr)
	require.NoError(t, err)
	require.True(t, net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback(), "只允许本地专用TiDB")
	var nonce [12]byte
	_, err = rand.Read(nonce[:])
	require.NoError(t, err)
	name := "guid_it_" + hex.EncodeToString(nonce[:])
	opts, err := sqltest.OptionsFromEnv(dsn, name, name)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, name, store.MigrateOptions{}))
	require.NoError(t, fixture.GrantProtoTables(ctx, name, []proto.Message{&pb.PlayerSnapshot{}, &pb.TransactionLog{}}))
	normal, err := mysql.ParseDSN(fixture.DSN(name))
	require.NoError(t, err)
	cfg := store.MySQLConfig{Host: normal.Addr, User: normal.User, Password: normal.Passwd, DBName: name, MaxOpenConn: 4, MaxIdleConn: 1, Permission: fixture.Permission}
	var version string
	require.NoError(t, fixture.Schema.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	require.Contains(t, strings.ToUpper(version), "TIDB")
	for _, table := range []string{"player_snapshot", "transaction_log"} {
		var count int
		require.NoError(t, fixture.Schema.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
		require.Zero(t, count, "自有验收开始前业务行必须为空")
	}
	return fixture.Schema, cfg, fixture
}

func TestTiDBSnapshotGuidMigrationAndStore(t *testing.T) {
	db, cfg, fixture := openTiDBGuidFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// 仅测试专用迁移入口验证同一正式迁移引擎的严格形状及幂等；生产fresh门禁另验。
	require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}))
	require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}))
	ss, err := store.NewSnapshotStore(cfg)
	require.NoError(t, err)
	ss.UseRowIDs(store.RowIDsForTest, store.RowIDsForTest)
	t.Cleanup(func() { require.NoError(t, ss.Close()) })
	require.True(t, ss.UniqueGuidKeyForTest(), "真实 store 必须启用数据库唯一约束")
	marker := fmt.Sprintf("tidb-guid-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := db.Exec("DELETE FROM player_snapshot WHERE reason = ? AND operator = ?", marker, marker)
		require.NoError(t, err)
		_, err = db.Exec("DELETE FROM transaction_log WHERE extra = ?", marker)
		require.NoError(t, err)
	})
	row := func(guid uint64) *store.SnapshotRow {
		return &store.SnapshotRow{PlayerID: 42, ZoneID: 990017, CreatedAt: 1, Reason: marker,
			Operator: marker, Data: []byte("{}"), SnapshotGuid: guid, Source: store.SnapshotSourceSceneKafka}
	}
	for range 2 {
		gm := row(0)
		gm.Source = store.SnapshotSourceDataService
		_, err := ss.InsertSnapshot(ctx, gm)
		require.NoError(t, err, "零 GUID 的 GM 行必须共存")
	}
	const maximum = ^uint64(0)
	id, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, row(maximum))
	require.NoError(t, err)
	require.True(t, inserted)
	again, inserted, err := ss.InsertSnapshotIfGuidAbsent(ctx, row(maximum))
	require.NoError(t, err)
	require.False(t, inserted)
	require.Equal(t, id, again)
	_, err = db.ExecContext(ctx, store.InsertSnapshotSQLForTest, store.SnapshotInsertArgsForTest(row(maximum))...)
	var duplicate *mysql.MySQLError
	require.ErrorAs(t, err, &duplicate, "绕过应用去重仍须被真实数据库拒绝")
	require.EqualValues(t, 1062, duplicate.Number)
	var wins atomic.Int32
	var writers sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, won, err := ss.InsertSnapshotIfGuidAbsent(ctx, row(maximum-1))
			if won {
				wins.Add(1)
			}
			errors <- err
		}()
	}
	writers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, wins.Load(), "并发同 GUID 只能插入一行")
	var gm, nonzero int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT SUM(snapshot_guid=0), SUM(snapshot_guid<>0) FROM player_snapshot WHERE reason=?", marker).Scan(&gm, &nonzero))
	require.Equal(t, 2, gm)
	require.Equal(t, 2, nonzero)
	// 流水仍由原 tx_id 主键去重；不能因快照方言适配改变另一条审计入口。
	tx, err := store.NewTransactionLogStore(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Close()) })
	logs := []*store.TransactionLogRow{{TxID: maximum, Extra: marker, ZoneID: 990017}}
	n, err := tx.InsertBatchIgnore(ctx, logs)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = tx.InsertBatchIgnore(ctx, logs)
	require.NoError(t, err)
	require.Zero(t, n)
}

// 这些DDL只施加于本fixture持有归属原件的随机schema，并逐例恢复正式结构。
// 迁移和建池必须拒绝“名字正确但不能约束同 GUID”的结构，不能退回 TiDB 无效的间隙锁去重。
func TestTiDBSnapshotGuidRejectsInvalidShape(t *testing.T) {
	db, cfg, fixture := openTiDBGuidFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}))
	for _, tc := range []struct {
		name      string
		columnDDL string
		indexDDL  string
	}{
		{name: "missing_key"},
		{name: "composite_unique", indexDDL: "ADD UNIQUE KEY uk_snapshot_guid_nz (snapshot_guid_nz, id)"},
		{name: "wrong_unique_column", indexDDL: "ADD UNIQUE KEY uk_snapshot_guid_nz (id)"},
		{name: "non_unique", indexDDL: "ADD KEY uk_snapshot_guid_nz (snapshot_guid_nz)"},
		{name: "ordinary_column", columnDDL: "ADD COLUMN snapshot_guid_nz BIGINT UNSIGNED NULL", indexDDL: "ADD UNIQUE KEY uk_snapshot_guid_nz (snapshot_guid_nz)"},
		{name: "wrong_expression", columnDDL: "ADD COLUMN snapshot_guid_nz BIGINT UNSIGNED GENERATED ALWAYS AS (CASE WHEN snapshot_guid=0 THEN NULL ELSE snapshot_guid-1 END) VIRTUAL", indexDDL: "ADD UNIQUE KEY uk_snapshot_guid_nz (snapshot_guid_nz)"},
		{name: "wrong_type", columnDDL: "ADD COLUMN snapshot_guid_nz DECIMAL(20,0) GENERATED ALWAYS AS (CASE WHEN snapshot_guid=0 THEN NULL ELSE snapshot_guid END) VIRTUAL", indexDDL: "ADD UNIQUE KEY uk_snapshot_guid_nz (snapshot_guid_nz)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := func(ddl string) {
				require.NoError(t, func() error { _, err := db.ExecContext(ctx, ddl); return err }())
			}
			t.Cleanup(func() {
				var indexes int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME='player_snapshot' AND INDEX_NAME='uk_snapshot_guid_nz'", cfg.DBName).Scan(&indexes))
				if indexes != 0 {
					exec("ALTER TABLE player_snapshot DROP INDEX uk_snapshot_guid_nz")
				}
				if tc.columnDDL != "" {
					var columns int
					require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='player_snapshot' AND COLUMN_NAME='snapshot_guid_nz'", cfg.DBName).Scan(&columns))
					if columns != 0 {
						exec("ALTER TABLE player_snapshot DROP COLUMN snapshot_guid_nz")
					}
				}
				require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}), "恢复正式结构")
			})
			exec("ALTER TABLE player_snapshot DROP INDEX uk_snapshot_guid_nz")
			if tc.columnDDL != "" {
				exec("ALTER TABLE player_snapshot DROP COLUMN snapshot_guid_nz")
				exec("ALTER TABLE player_snapshot " + tc.columnDDL)
			}
			if tc.indexDDL != "" {
				exec("ALTER TABLE player_snapshot " + tc.indexDDL)
			}
			ss, err := store.NewSnapshotStore(cfg)
			if ss != nil {
				require.NoError(t, ss.Close())
			}
			require.Error(t, err, "TiDB 不得在唯一约束缺失或失真时启用写池")
			if tc.name == "missing_key" {
				require.NoError(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}), "正确生成列已存在时可补键重入")
			} else {
				require.Error(t, store.MigrateOwnedFixtureSchema(ctx, fixture, cfg.DBName, store.MigrateOptions{}), "坏形状必须人工确认，迁移不能静默修复")
			}
		})
	}
}
