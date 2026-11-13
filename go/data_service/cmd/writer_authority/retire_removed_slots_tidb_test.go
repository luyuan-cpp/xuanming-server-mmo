package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
)

// retire-removed-slots 的真库回归(缺口清单第 22 条残余,writer-roster-exhaustion-20261106.md §5.8),MySQL 与 TiDB 都跑。
// 目录是真实的 Catalog,集群是内存里的 fakeStatefulSet(retire_removed_slots_test.go)。夹具的写者账号就是名册的槽位 0,
// 夹具起来时在两个逻辑库各注册了它的第 1 代;名册按夹具的选项拼出这个槽位的 REGISTER 模板,和注册器读的名册同一形状。
// 写者 StatefulSet 叫 db,序号 0 的 Pod 名是 db-0。没有 MMORPG_SQL_FIXTURE_ADMIN_DSN 时跳过。

// realRetirerFixture 是一个槽位、两个逻辑库的真库夹具。
type realRetirerFixture struct {
	ctx     context.Context
	fixture *sqltest.Fixture
	schemas []string
	roster  *dbpb.SqlWriterBootRoster
	// schemaPools 是各逻辑库的 schema 账号连接,只用来数行和建业务表。
	schemaPools map[string]*sql.DB
}

const realRetirerDeployment = "sql-writer-slot-retirer/db/real-db-job"

func realRetirerID(t *testing.T) []byte {
	t.Helper()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return id
}

// newRealRetirerFixture 的夹具那一代是本 namespace 的工作负载登记的(registered_by = workloadIdentityForTest),
// 内存集群里的 ServiceAccount default 就是那个 UID:收尾认它是自己的。
func newRealRetirerFixture(t *testing.T) *realRetirerFixture {
	t.Helper()
	return newRealRetirerFixtureRegisteredBy(t, workloadIdentityForTest)
}

// newRealRetirerFixtureRegisteredBy 让夹具那一代的 REGISTER 记下 registeredBy 这个登记身份(空串 = 集群外登记)。
func newRealRetirerFixtureRegisteredBy(t *testing.T, registeredBy string) *realRetirerFixture {
	t.Helper()
	dsn := os.Getenv("MMORPG_SQL_FIXTURE_ADMIN_DSN")
	if dsn == "" {
		// 与 shared/authoritysql 的真库用例同一口径:要求必跑时缺 DSN 就失败,不静默跳过。
		if os.Getenv("MAIL_RANK_REQUIRE_TESTS") == "1" || os.Getenv("MAIL_RANK_TEST_TIDB") != "" {
			t.Fatal("真实权限夹具必须显式提供本任务 MMORPG_SQL_FIXTURE_ADMIN_DSN")
		}
		t.Skip("需本任务独占真实库及权限资格原件(MMORPG_SQL_FIXTURE_ADMIN_DSN)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	suffix := hex.EncodeToString(realRetirerID(t)[:8])
	opts, err := sqltest.OptionsFromEnv(dsn, "retire_a_"+suffix, "retire_b_"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	opts.AllowMySQL = true
	opts.RegisteredBy = registeredBy
	fixture, err := sqltest.New(ctx, t, opts)
	if err != nil {
		t.Fatal(err)
	}
	f := &realRetirerFixture{ctx: ctx, fixture: fixture, schemas: opts.ProtectedSchemas, schemaPools: map[string]*sql.DB{}}
	for _, schema := range f.schemas {
		pool, err := fixture.OpenSchema(ctx, schema)
		if err != nil {
			t.Fatal(err)
		}
		f.schemaPools[schema] = pool
		if _, err = pool.ExecContext(ctx, "CREATE TABLE retire_business(id BIGINT PRIMARY KEY,value BIGINT NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.ExecContext(ctx, "INSERT INTO retire_business(id,value) VALUES(1,0)"); err != nil {
			t.Fatal(err)
		}
		if err = fixture.GrantTables(ctx, schema, "retire_business"); err != nil {
			t.Fatal(err)
		}
	}
	// 夹具在每个库注册的是同一代(同一个账号、writer key):这个账号就是槽位 0。
	var principal []byte
	if err = f.schemaPools[f.schemas[0]].QueryRowContext(ctx, "SELECT sql_principal FROM writer_registry WHERE state=1").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	entry := &dbpb.SqlWriterBootRoster_Entry{RuntimeConfigPath: "/run/sql-writer/input/runtime/db.yaml", RuntimeConfigSha256: bytes.Repeat([]byte{7}, 32)}
	for _, schema := range f.schemas {
		entry.Requests = append(entry.Requests, &dbpb.SqlWriterLifecycleReceipt_Request{Kind: dbpb.SqlWriterLifecycleReceipt_REGISTER,
			Writer:          &dbpb.SqlWriterRegistry{Generation: fixture.Generation, SqlPrincipal: bytes.Clone(principal), State: dbpb.SqlWriterRegistry_ACTIVE},
			AuthoritySchema: schema, ProtectedSchemas: []string{schema}, DeploymentDigest: bytes.Clone(opts.PhysicalAuthoritySHA256),
			LockPrimitive: opts.LockPrimitive, LockQualificationSha256: bytes.Clone(opts.LockQualificationSHA256)})
	}
	f.roster = &dbpb.SqlWriterBootRoster{Format: slotRosterFormat, Entries: []*dbpb.SqlWriterBootRoster_Entry{entry}}
	// 拼出来的名册要过注册器自己的名册核对,证明它就是生产名册的形状。
	if err = validateSlotRoster(f.roster, opts.PhysicalAuthoritySHA256); err != nil {
		t.Fatalf("夹具名册不是合法的槽位名册: %v", err)
	}
	return f
}

func (f *realRetirerFixture) catalog(t *testing.T, schema string) *authoritysql.Catalog {
	t.Helper()
	catalog, err := f.fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// latest 读槽位 0 在这个库的最新一代。
func (f *realRetirerFixture) latest(t *testing.T, schema string) *dbpb.SqlWriterRegistry {
	t.Helper()
	latest, err := f.catalog(t, schema).LatestBoot(f.ctx, f.roster.Entries[0].Requests[0].Writer.SqlPrincipal)
	if err != nil || latest == nil {
		t.Fatalf("读 %s 槽位 0 的最新一代: %+v %v", schema, latest, err)
	}
	return latest
}

func (f *realRetirerFixture) meta(t *testing.T, schema string) *dbpb.RouteMeta {
	t.Helper()
	meta, err := f.catalog(t, schema).CurrentMeta(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func (f *realRetirerFixture) count(t *testing.T, schema, query string, args ...any) int {
	t.Helper()
	var count int
	if err := f.schemaPools[schema].QueryRowContext(f.ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// retirer 是 StatefulSet db 的收尾;wrap 不为 nil 时按逻辑库包一层目录,用来在读目录之后、退休之前插入别的事务。
// 收尾每读一个候选就开一次目录(slotRetirer.run),所以每个候选各有自己的一层包装。
func (f *realRetirerFixture) retirer(t *testing.T, workload statefulSetState, wrap func(schema string, catalog *authoritysql.Catalog) retirementCatalog) slotRetirer {
	return slotRetirer{statefulSet: "db", namespace: namespaceForTest, deploymentID: realRetirerDeployment,
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (retirementCatalog, error) {
			catalog, err := f.fixture.CatalogFor(request.AuthoritySchema)
			if err != nil || wrap == nil {
				return catalog, err
			}
			return wrap(request.AuthoritySchema, catalog), nil
		},
		workload: workload, random: rand.Reader}
}

func scaledDownTo(replicas []int, pods ...string) *fakeStatefulSet {
	present := map[string]bool{}
	for _, pod := range pods {
		present[pod] = true
	}
	return &fakeStatefulSet{log: &retirementLog{}, replicas: replicas, pods: present}
}

func resultsBySchema(t *testing.T, report retirementReport) map[string]slotRetirementResult {
	t.Helper()
	results := map[string]slotRetirementResult{}
	for _, result := range report.results {
		if _, duplicate := results[result.schema]; duplicate || result.ordinal != 0 {
			t.Fatalf("每个库槽位 0 只有一个结局: %+v", report.results)
		}
		results[result.schema] = result
	}
	return results
}

func bumpRetireBusiness(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "UPDATE retire_business SET value=value+1 WHERE id=1")
	return err
}

// 缩容之后的完整一圈:Pod 还在、读完目录又扩回来时都不动在役的一代;Pod 没了才在两个库各结束它 ——
// 非 run 的 RETIRE(writer 原样是注册行、带 boot_seq,deployment_id 是收尾 Job),登记行置 REVOKED 并指回这份 RETIRE,
// 64 个 slot 行删掉,旧进程的写被拒(不是可重试的冻结)。重跑什么都不写。同一槽位再扩回来只注册,不换代。
func TestRetireRemovedSlotsEndsLastGenerationOnRealCatalog(t *testing.T) {
	f := newRealRetirerFixture(t)
	writers := map[string]*sql.DB{}
	before := map[string]*dbpb.RouteMeta{}
	for _, schema := range f.schemas {
		db, err := f.fixture.Open(f.ctx, schema, 2)
		if err != nil {
			t.Fatal(err)
		}
		if err = bumpRetireBusiness(f.ctx, db); err != nil {
			t.Fatalf("结束之前 %s 能写: %v", schema, err)
		}
		writers[schema] = db
		before[schema] = f.meta(t, schema)
	}
	first := f.latest(t, f.schemas[0])
	if first.State != dbpb.SqlWriterRegistry_ACTIVE {
		t.Fatalf("夹具的一代在役: %+v", first)
	}

	// 序号 0 的 Pod 对象还在(优雅退出没走完),或者读完目录之后副本数又涨回 1:这一轮都不动它。
	for name, workload := range map[string]*fakeStatefulSet{slotPodPresent: scaledDownTo([]int{0}, "db-0"), slotScaledBackUp: scaledDownTo([]int{0, 1})} {
		report, err := f.retirer(t, workload, nil).run(f.ctx, f.roster)
		if err != nil {
			t.Fatalf("%s 不算失败: %v", name, err)
		}
		for schema, result := range resultsBySchema(t, report) {
			if result.outcome != name || len(result.requestID) != 0 {
				t.Fatalf("%s 在 %s 的结局: %+v", name, schema, result)
			}
		}
	}
	for _, schema := range f.schemas {
		if meta := f.meta(t, schema); meta.CatalogRevision != before[schema].CatalogRevision {
			t.Fatalf("没结束就不写 %s 的目录: %d -> %d", schema, before[schema].CatalogRevision, meta.CatalogRevision)
		}
		if err := bumpRetireBusiness(f.ctx, writers[schema]); err != nil {
			t.Fatalf("%s 的这一代还在役: %v", schema, err)
		}
	}

	// Pod 没了:两个库的这一代都结束。
	report, err := f.retirer(t, scaledDownTo([]int{0}), nil).run(f.ctx, f.roster)
	if err != nil {
		t.Fatalf("结束槽位 0: %v", err)
	}
	for _, schema := range f.schemas {
		result := resultsBySchema(t, report)[schema]
		if result.outcome != slotRetired || result.bootSeq != first.BootSeq || len(result.requestID) != 16 {
			t.Fatalf("%s 的结局: %+v", schema, result)
		}
		receipt, err := f.catalog(t, schema).LifecycleReceipt(f.ctx, result.requestID)
		if err != nil {
			t.Fatalf("%s 的 RETIRE 收据: %v", schema, err)
		}
		request := &dbpb.SqlWriterLifecycleReceipt_Request{}
		if err = proto.Unmarshal(receipt.RequestProto, request); err != nil {
			t.Fatal(err)
		}
		if receipt.Kind != dbpb.SqlWriterLifecycleReceipt_RETIRE || receipt.CatalogRevision != before[schema].CatalogRevision+1 || receipt.PreviousEpoch != before[schema].WriterEpoch ||
			request.DeploymentId != realRetirerDeployment || len(request.MaintenanceRunId) != 0 || len(request.SuccessorRequestId) != 0 ||
			!bytes.Equal(request.Writer.WriterKey, first.WriterKey) || request.Writer.BootSeq != first.BootSeq {
			t.Fatalf("%s 的 RETIRE:非 run、没有继任者、退的就是快照里那一代: %+v %+v", schema, receipt, request)
		}
		if latest := f.latest(t, schema); latest.State != dbpb.SqlWriterRegistry_REVOKED || !bytes.Equal(latest.WriterKey, first.WriterKey) {
			t.Fatalf("%s 的登记行置 REVOKED: %+v", schema, latest)
		}
		if got := f.count(t, schema, "SELECT COUNT(*) FROM writer_registry WHERE writer_key=? AND state=2 AND retirement_request_id=?", first.WriterKey, result.requestID); got != 1 {
			t.Fatalf("%s 的登记行指回这份 RETIRE: %d", schema, got)
		}
		if got := f.count(t, schema, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.WriterKey); got != 0 {
			t.Fatalf("%s 结束这一代时删掉它的 slot 行: %d", schema, got)
		}
		if err = bumpRetireBusiness(f.ctx, writers[schema]); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
			t.Fatalf("%s 结束之后旧进程的写被拒,不是可重试的冻结: %v", schema, err)
		}
	}

	// 重跑(下一轮 CronJob):槽位 0 没有在役的一代,什么都不写。
	again, err := f.retirer(t, scaledDownTo([]int{0}), nil).run(f.ctx, f.roster)
	if err != nil || len(again.results) != 0 {
		t.Fatalf("重跑什么都不做: %+v %v", again.results, err)
	}
	for _, schema := range f.schemas {
		if meta := f.meta(t, schema); meta.CatalogRevision != before[schema].CatalogRevision+1 {
			t.Fatalf("重跑不推进 %s 的目录版本: %d", schema, meta.CatalogRevision)
		}
	}

	// 同一槽位再扩回来:新 Pod 按名册模板生成下一代(与注册器同一段代码),没有在役的上一代,只注册(目录版本 +1),
	// slot 行只属于新一代。副本数涨回 1 之后,收尾不再读这个槽位。
	boot, err := mintBoot(f.roster.Entries[0], "db-0-scaled-up", workloadIdentityForTest, first.BootSeq+1, time.Now(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range boot.Requests {
		catalog := f.catalog(t, request.AuthoritySchema)
		registered, err := catalog.Register(f.ctx, request)
		if err != nil {
			t.Fatalf("%s 扩回来之后注册下一代: %v", request.AuthoritySchema, err)
		}
		if registered.CatalogRevision != before[request.AuthoritySchema].CatalogRevision+2 {
			t.Fatalf("%s 只注册、不换代: %+v", request.AuthoritySchema, registered)
		}
		active, err := catalog.ActiveWriterKeys(f.ctx)
		if err != nil || len(active) != 1 || !bytes.Equal(active[0], request.Writer.WriterKey) {
			t.Fatalf("%s 在役的是新一代: %x %v", request.AuthoritySchema, active, err)
		}
		if got := f.count(t, request.AuthoritySchema, "SELECT COUNT(*) FROM writer_slot"); got != authoritysql.WriterSlotCount {
			t.Fatalf("%s 的 slot 行只属于在役的一代: %d", request.AuthoritySchema, got)
		}
	}
	inService, err := f.retirer(t, scaledDownTo([]int{1}), nil).run(f.ctx, f.roster)
	if err != nil || len(inService.results) != 0 {
		t.Fatalf("副本数以内的槽位不动: %+v %v", inService.results, err)
	}
}

// 一个库的闸门冻结(搬库、排空)时,这个库这一轮跳过、不算失败,另一个库照常结束;解冻之后的下一轮把它也结束,
// RETIRE 绑解冻之后的 epoch。
func TestRetireRemovedSlotsWaitsOutFrozenGateOnRealCatalog(t *testing.T) {
	f := newRealRetirerFixture(t)
	frozen, open := f.schemas[0], f.schemas[1]
	first := f.latest(t, frozen)
	run := realRetirerID(t)
	if _, err := f.catalog(t, frozen).FreezeWriters(f.ctx, run); err != nil {
		t.Fatal(err)
	}
	report, err := f.retirer(t, scaledDownTo([]int{0}), nil).run(f.ctx, f.roster)
	if err != nil {
		t.Fatalf("冻结不算失败: %v", err)
	}
	results := resultsBySchema(t, report)
	if results[frozen].outcome != slotGateFrozen || results[open].outcome != slotRetired {
		t.Fatalf("冻结的库跳过,另一个库结束: %+v", report.results)
	}
	if latest := f.latest(t, frozen); latest.State != dbpb.SqlWriterRegistry_ACTIVE || !bytes.Equal(latest.WriterKey, first.WriterKey) {
		t.Fatalf("冻结的库这一代还在役: %+v", latest)
	}
	if latest := f.latest(t, open); latest.State != dbpb.SqlWriterRegistry_REVOKED {
		t.Fatalf("没冻结的库这一代已结束: %+v", latest)
	}
	released, err := f.catalog(t, frozen).ReleaseWriters(f.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.retirer(t, scaledDownTo([]int{0}), nil).run(f.ctx, f.roster)
	if err != nil {
		t.Fatalf("解冻之后的下一轮: %v", err)
	}
	if len(next.results) != 1 || next.results[0].schema != frozen || next.results[0].outcome != slotRetired {
		t.Fatalf("只剩解冻的库要结束: %+v", next.results)
	}
	receipt, err := f.catalog(t, frozen).LifecycleReceipt(f.ctx, next.results[0].requestID)
	if err != nil || receipt.PreviousEpoch != released.WriterEpoch {
		t.Fatalf("RETIRE 绑解冻之后的 epoch %d: %+v %v", released.WriterEpoch, receipt, err)
	}
	if got := f.count(t, frozen, "SELECT COUNT(*) FROM writer_slot"); got != 0 {
		t.Fatalf("解冻的库这一代的 slot 行按解冻后的 epoch 删掉: %d", got)
	}
}

// supersedingCatalog 在第一次退休之前,先让同序号的新 Pod 在这个库注册下一代(读目录之后、退休之前换代)。
type supersedingCatalog struct {
	*authoritysql.Catalog
	next *dbpb.SqlWriterLifecycleReceipt_Request
	done bool
}

func (c *supersedingCatalog) Retire(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error) {
	if !c.done {
		c.done = true
		if _, err := c.Catalog.Register(ctx, c.next); err != nil {
			return nil, fmt.Errorf("the new pod registers its boot first: %w", err)
		}
	}
	return c.Catalog.Retire(ctx, request)
}

// 收尾只结束目录快照里那一代:读完目录之后,同序号的新 Pod 先换了代(上一代由换代结束),这里的退休读不到 slot 行、被拒,
// 重读得知那一代已结束,记 already-ended、不算失败;新 Pod 的一代照常在役,写得进去。
func TestRetireRemovedSlotsNeverEndsANewerGenerationOnRealCatalog(t *testing.T) {
	f := newRealRetirerFixture(t)
	first := f.latest(t, f.schemas[0])
	boot, err := mintBoot(f.roster.Entries[0], "db-0-new-pod", workloadIdentityForTest, first.BootSeq+1, time.Now(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nextFor := map[string]*dbpb.SqlWriterLifecycleReceipt_Request{}
	for _, request := range boot.Requests {
		nextFor[request.AuthoritySchema] = request
	}
	report, err := f.retirer(t, scaledDownTo([]int{0}), func(schema string, catalog *authoritysql.Catalog) retirementCatalog {
		return &supersedingCatalog{Catalog: catalog, next: nextFor[schema]}
	}).run(f.ctx, f.roster)
	if err != nil {
		t.Fatalf("别处先结束了这一代不算失败: %v", err)
	}
	for schema, result := range resultsBySchema(t, report) {
		if result.outcome != slotAlreadyEnded || result.bootSeq != first.BootSeq {
			t.Fatalf("%s 的结局: %+v", schema, result)
		}
		next := nextFor[schema]
		latest := f.latest(t, schema)
		if latest.State != dbpb.SqlWriterRegistry_ACTIVE || !bytes.Equal(latest.WriterKey, next.Writer.WriterKey) || latest.BootSeq != next.Writer.BootSeq {
			t.Fatalf("%s 新 Pod 的一代照常在役: %+v", schema, latest)
		}
		if got := f.count(t, schema, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", next.Writer.WriterKey); got != authoritysql.WriterSlotCount {
			t.Fatalf("%s 新一代的 slot 行都在: %d", schema, got)
		}
		// 上一代是换代结束的:它的 RETIRE 指回新一代的 REGISTER,不是收尾发的那份。
		if got := f.count(t, schema, "SELECT COUNT(*) FROM writer_registry WHERE writer_key=? AND state=2 AND retirement_request_id=?", first.WriterKey, result.requestID); got != 0 {
			t.Fatalf("%s 收尾的 RETIRE 没有落地: %d", schema, got)
		}
		if _, err := f.catalog(t, schema).LifecycleReceipt(f.ctx, result.requestID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s 被拒的退休没有收据: %v", schema, err)
		}
	}
}

// 只结束本集群登记的一代(设计稿 §5.9「归属」),真库:夹具那一代是别处登记的 —— 另一个集群(同名 namespace 与 ServiceAccount,
// UID 不同),或者集群外登记(空身份)。这里副本数是 0、序号 0 的 Pod 也不存在,收尾照样不动它:从真实的收据表读回 REGISTER,
// 发现不是自己登记的,记 registered-elsewhere、这一轮算失败;目录一个字节不写,那一代照常在役、写得进去。
// 同一份名册和注册账号被带到第二个集群(备份恢复、备用集群缩到 0)时,那边的收尾走的就是这条路。
func TestRetireRemovedSlotsLeavesGenerationRegisteredElsewhereOnRealCatalog(t *testing.T) {
	for name, registeredBy := range map[string]string{
		"另一个集群": workloadIdentity{namespace: namespaceForTest, serviceAccount: serviceAccountForTest, uid: "11111111-2222-3333-4444-555555555555"}.String(),
		"集群外登记": "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealRetirerFixtureRegisteredBy(t, registeredBy)
			writers := map[string]*sql.DB{}
			before := map[string]*dbpb.RouteMeta{}
			for _, schema := range f.schemas {
				db, err := f.fixture.Open(f.ctx, schema, 2)
				if err != nil {
					t.Fatal(err)
				}
				writers[schema] = db
				before[schema] = f.meta(t, schema)
			}
			first := f.latest(t, f.schemas[0])
			report, err := f.retirer(t, scaledDownTo([]int{0}), nil).run(f.ctx, f.roster)
			if !errors.Is(err, errRegisteredElsewhere) {
				t.Fatalf("别处登记的一代要报出来,不能悄悄结束或悄悄跳过: %v", err)
			}
			results := resultsBySchema(t, report)
			for _, schema := range f.schemas {
				if result := results[schema]; result.outcome != slotRegisteredElsewhere || result.bootSeq != first.BootSeq || len(result.requestID) != 0 {
					t.Fatalf("%s 的结局: %+v", schema, result)
				}
				if meta := f.meta(t, schema); meta.CatalogRevision != before[schema].CatalogRevision {
					t.Fatalf("%s 的目录不能动: %d -> %d", schema, before[schema].CatalogRevision, meta.CatalogRevision)
				}
				if latest := f.latest(t, schema); latest.State != dbpb.SqlWriterRegistry_ACTIVE || !bytes.Equal(latest.WriterKey, first.WriterKey) {
					t.Fatalf("%s 别处登记的一代照常在役: %+v", schema, latest)
				}
				if got := f.count(t, schema, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.WriterKey); got != authoritysql.WriterSlotCount {
					t.Fatalf("%s 它的 slot 行都在: %d", schema, got)
				}
				if err := bumpRetireBusiness(f.ctx, writers[schema]); err != nil {
					t.Fatalf("%s 别处登记的一代照常写得进去: %v", schema, err)
				}
			}
		})
	}
}
