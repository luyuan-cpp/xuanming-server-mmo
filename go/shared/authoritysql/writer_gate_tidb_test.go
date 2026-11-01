package authoritysql_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
)

// 两个随机逻辑库的夹具,进程持两份写资格。需要真实库和权限资格原件,没有就跳过。
func twoLogicalDBFixture(t *testing.T) (context.Context, *sqltest.Fixture, string, string) {
	t.Helper()
	dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" || os.Getenv("MAIL_RANK_TEST_TIDB") != "" {
			t.Fatal("真实权限夹具必须显式提供本任务MMORPG_SQL_FIXTURE_ADMIN_DSN")
		}
		t.Skip("需本任务独占真实库及权限资格原件")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	suffix := hex.EncodeToString(fixtureID(t)[:8])
	first, second := "gate_a_"+suffix, "gate_b_"+suffix
	opts, err := sqltest.OptionsFromEnv(dsn, first, second)
	if err != nil {
		t.Fatal(err)
	}
	opts.AllowMySQL = true
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{first, second} {
		prepare, err := fixture.OpenSchema(ctx, schema)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = prepare.ExecContext(ctx, "CREATE TABLE gate_business(id BIGINT PRIMARY KEY,value BIGINT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err = prepare.ExecContext(ctx, "INSERT INTO gate_business(id,value) VALUES(1,0)"); err != nil {
			t.Fatal(err)
		}
		if err = fixture.GrantTables(ctx, schema, "gate_business"); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, fixture, first, second
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func bumpGateBusiness(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "UPDATE gate_business SET value=value+1 WHERE id=1")
	return err
}

// 冻结后写事务回 ErrWriterFrozen(同时能被 ErrWriterDenied 认出),解冻后恢复;写者不用换身份。
func TestGateFrozenWriterGetsRetryableError(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	db, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = bumpGateBusiness(ctx, db); err != nil {
		t.Fatal(err)
	}
	before, err := catalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runID := fixtureID(t)
	frozen, err := catalog.FreezeWriters(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Writable != 0 || frozen.WriterEpoch != before.WriterEpoch+1 {
		t.Fatalf("冻结后 meta=%+v", frozen)
	}
	if err = bumpGateBusiness(ctx, db); !errors.Is(err, authoritysql.ErrWriterFrozen) || !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("冻结后的自动事务: %v", err)
	}
	if _, err = db.BeginTx(ctx, nil); !errors.Is(err, authoritysql.ErrWriterFrozen) {
		t.Fatalf("冻结后的显式事务: %v", err)
	}
	observed, frozenSeen := authoritysql.ObserveWriterFrozen(ctx)
	if err = bumpGateBusiness(observed, db); !errors.Is(err, authoritysql.ErrWriterFrozen) || !frozenSeen() {
		t.Fatalf("观察位没有置上: %v", err)
	}
	// 重放幂等;换一个 run 既冻不上也解不开。
	replay, err := catalog.FreezeWriters(ctx, runID)
	if err != nil || replay.WriterEpoch != frozen.WriterEpoch {
		t.Fatalf("同 run 重放: %+v %v", replay, err)
	}
	if _, err = catalog.FreezeWriters(ctx, fixtureID(t)); err == nil {
		t.Fatal("另一个 run 冻上了已冻结的闸门")
	}
	if _, err = catalog.ReleaseWriters(ctx, fixtureID(t)); err == nil {
		t.Fatal("另一个 run 解开了闸门")
	}
	released, err := catalog.ReleaseWriters(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Writable != 1 || released.WriterEpoch != before.WriterEpoch+2 || len(released.FreezeRunId) != 0 {
		t.Fatalf("解冻后 meta=%+v", released)
	}
	if err = bumpGateBusiness(ctx, db); err != nil {
		t.Fatalf("解冻后写事务: %v", err)
	}
	if replay, err = catalog.ReleaseWriters(ctx, runID); err != nil || replay.WriterEpoch != released.WriterEpoch {
		t.Fatalf("解冻重放: %+v %v", replay, err)
	}
}

// 冻结窗口里开池不能失败:写资格是一次性的,开池核对撞闸门退出的话这个 Pod 就废了。业务写照样回可重试。
func TestOpenSurvivesFrozenGate(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := fixture.PermissionFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.FreezeWriters(ctx, fixtureID(t)); err != nil {
		t.Fatal(err)
	}
	db, err := authoritysql.Open(ctx, fixture.DSN(schema), schema, fixture.Backend(), 2, permission)
	if err != nil {
		t.Fatalf("冻结窗口里开池: %v", err)
	}
	defer db.Close()
	if err = bumpGateBusiness(ctx, db); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("业务写回可重试: %v", err)
	}
}

// 名册是预先渲染的:cutover 和回滚之后 epoch 变了,REGISTER 照样成功,slot 按当前 epoch 建。
// 冻结期回可重试,什么都没落地,解冻之后同一份请求落地;之后再冻结,新写者的 slot 跟着走。
func TestRegisterDoesNotBindEpochAndWaitsOutFreeze(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = fixture.Operator.QueryRowContext(ctx, "SELECT request_proto FROM writer_lifecycle_receipt WHERE kind=1 LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	request := &pb.SqlWriterLifecycleReceipt_Request{}
	if err = proto.Unmarshal(raw, request); err != nil {
		t.Fatal(err)
	}
	if request.ExpectedEpoch != 0 {
		t.Fatalf("夹具的 REGISTER 不该带 epoch: %d", request.ExpectedEpoch)
	}
	request.RequestId = fixtureID(t)
	request.DeploymentId = "register-after-epoch-change"
	request.ExpectedCatalogRevision = 0
	request.Writer.WriterKey = fixtureID(t)
	request.Writer.InstanceId = fixtureID(t)
	request.Writer.SqlPrincipal = []byte("roster_next@%")
	request.Writer.RegistrationDigest = nil
	if request, err = authoritysql.CanonicalRegistrationRequest(request); err != nil {
		t.Fatal(err)
	}
	if err = catalog.RegistrationReady(ctx, request); err != nil {
		t.Fatalf("闸门开着: %v", err)
	}
	withEpoch := proto.Clone(request).(*pb.SqlWriterLifecycleReceipt_Request)
	withEpoch.ExpectedEpoch = 1
	if withEpoch, err = authoritysql.CanonicalRegistrationRequest(withEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Register(ctx, withEpoch); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("REGISTER 不带 epoch: %v", err)
	}

	run := fixtureID(t)
	if _, err = catalog.FreezeWriters(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err = catalog.RegistrationReady(ctx, request); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("冻结期预检回可重试: %v", err)
	}
	if _, err = catalog.Register(ctx, request); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("冻结期 REGISTER 回可重试: %v", err)
	}
	if _, err = catalog.LifecycleReceipt(ctx, request.RequestId); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("冻结期不能落地: %v", err)
	}
	released, err := catalog.ReleaseWriters(ctx, run)
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := catalog.Register(ctx, request)
	if err != nil {
		t.Fatalf("epoch 变了之后 REGISTER: %v", err)
	}
	if receipt.PreviousEpoch != released.WriterEpoch || receipt.ResultingEpoch != released.WriterEpoch {
		t.Fatalf("收据记实际 epoch: %+v, want %d", receipt, released.WriterEpoch)
	}
	slotsAt := func(epoch uint64) int {
		var count int
		if err := fixture.Operator.QueryRowContext(ctx, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=? AND writer_epoch=? AND state=1", request.Writer.WriterKey, epoch).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := slotsAt(released.WriterEpoch); got != authoritysql.WriterSlotCount {
		t.Fatalf("slot 按当前 epoch 建: %d", got)
	}
	// 重放幂等。
	if _, err = catalog.Register(ctx, request); err != nil {
		t.Fatalf("重放: %v", err)
	}
	frozen, err := catalog.FreezeWriters(ctx, fixtureID(t))
	if err != nil {
		t.Fatalf("新写者不能挡冻结: %v", err)
	}
	if got := slotsAt(frozen.WriterEpoch); got != authoritysql.WriterSlotCount {
		t.Fatalf("冻结要带上新写者的 slot: %d", got)
	}
}

// 两个逻辑库的闸门互不影响:冻结一个,另一个照常写,epoch 也不动。一个进程同时持两份写资格。
func TestGatesOfTwoLogicalDBsAreIndependent(t *testing.T) {
	ctx, fixture, first, second := twoLogicalDBFixture(t)
	firstCatalog, err := fixture.CatalogFor(first)
	if err != nil {
		t.Fatal(err)
	}
	secondCatalog, err := fixture.CatalogFor(second)
	if err != nil {
		t.Fatal(err)
	}
	firstDB, err := fixture.Open(ctx, first, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondDB, err := fixture.Open(ctx, second, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondBefore, err := secondCatalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = firstCatalog.FreezeWriters(ctx, fixtureID(t)); err != nil {
		t.Fatal(err)
	}
	if err = bumpGateBusiness(ctx, firstDB); !errors.Is(err, authoritysql.ErrWriterFrozen) {
		t.Fatalf("被冻结的库: %v", err)
	}
	if err = bumpGateBusiness(ctx, secondDB); err != nil {
		t.Fatalf("没冻结的库不该受影响: %v", err)
	}
	secondAfter, err := secondCatalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if secondAfter.Writable != 1 || secondAfter.WriterEpoch != secondBefore.WriterEpoch {
		t.Fatalf("另一个库的闸门变了: %+v", secondAfter)
	}
}

// 闸门只放行自己库的连接:拿 A 库的写资格连 B 库打不开池。
func TestWriterGrantOnlyOpensItsOwnLogicalDB(t *testing.T) {
	ctx, fixture, first, second := twoLogicalDBFixture(t)
	permission, err := fixture.PermissionFor(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = authoritysql.Open(ctx, fixture.DSN(second), second, fixture.Backend(), 1, permission); err == nil {
		t.Fatal("A 库的写资格连上了 B 库")
	}
}

// mmorpg 没有 Go 服务的 -migrate,闸门表写在 gateway_tables.sql 里:DDL 必须和目录逐字相同(空白除外)。
func TestGatewayTablesCarryWriterGate(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("取不到源文件路径")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "..", "deploy", "mysql-init", "gateway_tables.sql"))
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	script := normalize(string(raw))
	for name, ddl := range authoritysql.CatalogBootstrapStatements() {
		if !strings.Contains(script, normalize(ddl)) {
			t.Fatalf("gateway_tables.sql 的 %s 与目录 DDL 不一致", name)
		}
	}
	if !strings.Contains(script, "INSERT INTO route_meta(id,generation,writable,writer_epoch,catalog_revision,freeze_run_id)") {
		t.Fatal("gateway_tables.sql 没有种 route_meta")
	}
}

// 集合文件的合法形状之外一律拒绝,而且不写消费标记(同一份改好之后还能用)。
func TestLoadWriterConfigsRejectsBadSetBeforeConsuming(t *testing.T) {
	directory := t.TempDir()
	if err := authoritysql.CheckPrivateControlDirectory(directory); err != nil {
		t.Skip("平台临时目录不是私有目录: ", err)
	}
	physical := make([]byte, 32)
	physical[0] = 9
	grantFor := func(schema string, protected ...string) *pb.SqlWriterBootGrant {
		return &pb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: fixtureID(t), InstanceId: fixtureID(t), Generation: "ab", AuthoritySchema: schema, ProtectedSchemas: protected, SqlPrincipal: []byte("writer@%"), RegistrationDigest: make([]byte, 32), DeploymentId: "boot-1", GrantId: fixtureID(t), GrantedMs: 1, Permission: "NORMAL", PhysicalAuthoritySha256: physical, LockPrimitive: pb.SqlWriterBootGrant_LOCK_TOUCH, LockQualificationSha256: make([]byte, 32)}
	}
	load := func(name string, set *pb.SqlWriterBootGrantSet) (map[string]authoritysql.WriterConfig, error) {
		raw, err := protojson.Marshal(set)
		if err != nil {
			t.Fatal(err)
		}
		path := directory + string(os.PathSeparator) + name
		if err = authoritysql.WritePrivateControlFile(path, raw); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"MMORPG_SQL_WRITER_GRANT_FILE": path, "MMORPG_SQL_WRITER_GRANT_SHA256": sha256Hex(raw), "MMORPG_SQL_WRITER_CONSUME_DIR": directory, "MMORPG_SQL_DEPLOYMENT_ID": "boot-1", "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256": hex.EncodeToString(physical)}
		return authoritysql.LoadWriterConfigs(func(key string) string { return env[key] })
	}
	bad := map[string]*pb.SqlWriterBootGrantSet{
		"empty.json":      {Format: authoritysql.GrantSetFormat},
		"format.json":     {Format: "SQL_WRITER_BOOT_V1", Grants: []*pb.SqlWriterBootGrant{grantFor("a", "a")}},
		"two-schema.json": {Format: authoritysql.GrantSetFormat, Grants: []*pb.SqlWriterBootGrant{grantFor("a", "a", "b")}},
		"foreign.json":    {Format: authoritysql.GrantSetFormat, Grants: []*pb.SqlWriterBootGrant{grantFor("a", "b")}},
		"duplicate.json":  {Format: authoritysql.GrantSetFormat, Grants: []*pb.SqlWriterBootGrant{grantFor("a", "a"), grantFor("a", "a")}},
	}
	for name, set := range bad {
		if _, err := load(name, set); err == nil {
			t.Fatalf("%s 被接受了", name)
		}
	}
	good := &pb.SqlWriterBootGrantSet{Format: authoritysql.GrantSetFormat, Grants: []*pb.SqlWriterBootGrant{grantFor("a", "a"), grantFor("b", "b")}}
	configs, err := load("good.json", good)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 || configs["a"].AuthoritySchema() != "a" || configs["b"].AuthoritySchema() != "b" {
		t.Fatalf("返回的写资格不对: %v", configs)
	}
	// 读写资格不写消费标记,第一条写语句之前才写(TestWriterGrantIsConsumedOnlyBeforeFirstWrite):还没写过就退出的进程,重启还能用这份。
	if _, err = load("good-again.json", good); err != nil {
		t.Fatalf("还没写过,不算消费: %v", err)
	}
}
