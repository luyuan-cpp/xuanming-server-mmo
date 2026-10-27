package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"data_service/cmd/debugutil"
	comp "proto/common/component"
	dbpb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/generated/persistschema"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func importRowsTiDB(t *testing.T, capture ...func(*sqltest.Fixture, string)) (*sql.DB, *proto2mysql.DB) {
	t.Helper()
	dsn := os.Getenv("ACCOUNT_DIRECTORY_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNT_DIRECTORY_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少本线程专用 ACCOUNT_DIRECTORY_TEST_DSN")
		}
		t.Skip("未配置专用导入 TiDB")
	}
	var nonce [8]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	name := "role_import_it_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts, err := sqltest.OptionsFromEnv(dsn, name+"_writer", name)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	install, err := fixture.OpenSchema(ctx, name)
	require.NoError(t, err)
	models := []proto.Message{&dbpb.PlayerDatabase{}, &dbpb.PlayerDatabase_1{}}
	prepare := proto2mysql.NewDB()
	require.NoError(t, prepare.OpenDB(install, name))
	for _, row := range models {
		prepare.RegisterTable(row)
		require.NoError(t, prepare.CreateOrUpdateTable(row))
	}
	require.NoError(t, fixture.GrantProtoTables(ctx, name, models))
	db, err := fixture.Open(ctx, name, 8)
	require.NoError(t, err)
	model := proto2mysql.NewDB()
	require.NoError(t, model.OpenDB(db, name))
	for _, row := range models {
		model.RegisterTable(row)
	}
	if len(capture) == 1 && capture[0] != nil {
		capture[0](fixture, name)
	}
	return db, model
}

func importRowsPlan(balance uint64) *importPlan {
	currency := &comp.CurrencyComp{Values: []uint64{balance}}
	currency.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 91, protowire.BytesType), []byte("future-currency")))
	return &importPlan{playerID: 17, seq: 3, rows: []planRow{{table: "player_database", msg: &dbpb.PlayerDatabase{PlayerId: 17, Currency: currency, PersistSchemaGen: persistschema.Gen}}, {table: "player_database_1", msg: &dbpb.PlayerDatabase_1{PlayerId: 17, PersistSchemaGen: persistschema.Gen}}}}
}
func requireImportedRows(t *testing.T, ctx context.Context, db *sql.DB, epoch uint64, expected *importPlan) {
	t.Helper()
	for _, row := range expected.rows {
		want := proto.Clone(row.msg)
		require.NoError(t, debugutil.SetRowFence(want, epoch, expected.seq))
		got, found, err := debugutil.ReadPlayerRow(ctx, db, func() proto.Message { return want.ProtoReflect().Type().New().Interface() }, 17)
		require.NoError(t, err)
		require.True(t, found)
		require.True(t, proto.Equal(want, got), "完整玩家原件与未知PB字段必须保留")
	}
}
func TestTiDBBundleImportFencesRollbackBothRowsAndPreserveUnknownFields(t *testing.T) {
	db, model := importRowsTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	original := importRowsPlan(100)
	for _, r := range original.rows {
		row := proto.Clone(r.msg)
		require.NoError(t, debugutil.SetRowFence(row, 20, 3))
		require.NoError(t, model.Insert(row))
	}
	_, err := db.ExecContext(ctx, "UPDATE player_database_1 SET fence_owner_epoch=22 WHERE player_id=17")
	require.NoError(t, err)
	incoming := importRowsPlan(200)
	err = writeLocalRows(ctx, db, incoming, 21)
	require.Error(t, err, "第二表较新必须回滚第一表已做的替换")
	got, found, err := debugutil.ReadPlayerRow(ctx, db, func() proto.Message { return &dbpb.PlayerDatabase{} }, 17)
	require.NoError(t, err)
	require.True(t, found)
	want := proto.Clone(original.rows[0].msg)
	require.NoError(t, debugutil.SetRowFence(want, 20, 3))
	require.True(t, proto.Equal(want, got))
	require.NoError(t, writeLocalRows(ctx, db, incoming, 23))
	requireImportedRows(t, ctx, db, 23, incoming)
	require.Error(t, writeLocalRows(ctx, db, original, 22))
	requireImportedRows(t, ctx, db, 23, incoming)
	_, err = db.ExecContext(ctx, "UPDATE player_database_1 SET persist_schema_gen=4294967295 WHERE player_id=17")
	require.NoError(t, err)
	require.Error(t, writeLocalRows(ctx, db, original, 24), "不删除较新覆盖世代行")
	got, found, err = debugutil.ReadPlayerRow(ctx, db, func() proto.Message { return &dbpb.PlayerDatabase{} }, 17)
	require.NoError(t, err)
	require.True(t, found)
	want = proto.Clone(incoming.rows[0].msg)
	require.NoError(t, debugutil.SetRowFence(want, 23, 3))
	require.True(t, proto.Equal(want, got), "第二表世代拒绝时第一表也回滚")
}

func TestTiDBBundleConcurrentImportsCannotDeleteNewerSQLFence(t *testing.T) {
	db, model := importRowsTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	original := importRowsPlan(100)
	for _, r := range original.rows {
		row := proto.Clone(r.msg)
		require.NoError(t, debugutil.SetRowFence(row, 20, 3))
		require.NoError(t, model.Insert(row))
	}
	type result struct {
		epoch uint64
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, epoch := range []uint64{24, 25} {
		wg.Add(1)
		go func(e uint64) {
			defer wg.Done()
			<-start
			results <- result{e, writeLocalRows(ctx, db, importRowsPlan(e*100), e)}
		}(epoch)
	}
	close(start)
	wg.Wait()
	close(results)
	count := 0
	for r := range results {
		count++
		if r.epoch == 25 {
			require.NoError(t, r.err)
		} else if r.err != nil {
			var duplicate *mysql.MySQLError
			require.True(t, errors.As(r.err, &duplicate))
			require.Equal(t, uint16(1062), duplicate.Number)
		}
	}
	require.Equal(t, 2, count)
	requireImportedRows(t, ctx, db, 25, importRowsPlan(2500))
}

func TestNormalImportPreviewDoesNotConsumeWriterGrant(t *testing.T) {
	t.Setenv("MMORPG_SQL_WRITER_GRANT_FILE", "invalid-preview-grant")
	calls := 0
	require.NoError(t, runNormalImport(context.Background(), true, "", "", "", func(permission authoritysql.Permission) error {
		calls++
		require.Equal(t, "reader", permission.Kind())
		return nil
	}))
	require.Equal(t, 1, calls)
	require.Error(t, runNormalImport(context.Background(), true, "runtime", "", "", func(authoritysql.Permission) error {
		calls++
		return nil
	}))
	require.Equal(t, 1, calls, "预览不得伪造绑定到维护run的收据")
}

func TestNormalImportRejectsMissingBootBeforeEffects(t *testing.T) {
	calls := 0
	require.Error(t, executeNormalImport(context.Background(), authoritysql.WriterConfig{}, nil, func(authoritysql.Permission) error {
		calls++
		return nil
	}))
	require.Zero(t, calls)
}

func TestTiDBNormalImportSealsOnlyAfterSynchronousWritesAndCleanup(t *testing.T) {
	var fixture *sqltest.Fixture
	var schema string
	db, _ := importRowsTiDB(t, func(f *sqltest.Fixture, name string) { fixture = f; schema = name })
	require.NoError(t, db.Close())
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	closed := false
	err := executeNormalImport(ctx, fixture.Config, nil, func(permission authoritysql.Permission) (callErr error) {
		pool, err := authoritysql.Open(ctx, fixture.DSN(schema), schema, true, 1, permission)
		if err != nil {
			return err
		}
		defer func() { callErr = errors.Join(callErr, pool.Close()); closed = true }()
		incoming := importRowsPlan(200)
		if err := writeLocalRows(ctx, pool, incoming, 23); err != nil {
			return err
		}
		requireImportedRows(t, ctx, pool, 23, incoming)
		return nil
	})
	require.NoError(t, err)
	require.True(t, closed, "返回前实际连接池必须已关闭")
	_, err = fixture.Open(ctx, schema, 1)
	require.Error(t, err, "完成后同boot Permission必须永久拒绝重开")
}
