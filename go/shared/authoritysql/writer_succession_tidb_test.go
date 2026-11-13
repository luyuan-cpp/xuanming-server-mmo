package authoritysql_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
)

// 写者名册按槽位换代(缺口清单第 22 条,docs/design/writer-roster-exhaustion-20261106.md §5)的真库回归,MySQL 与 TiDB 都跑。
// 夹具的写者账号就是一个槽位,夹具起来时注册了它的第 1 代;这里在同一账号上注册后面的代。

// latestRegistration 读本逻辑库最新的一份 REGISTER 原件,也就是夹具这个槽位当前一代的请求。
func latestRegistration(t *testing.T, ctx context.Context, operator *sql.DB) *pb.SqlWriterLifecycleReceipt_Request {
	t.Helper()
	var raw []byte
	if err := operator.QueryRowContext(ctx, "SELECT request_proto FROM writer_lifecycle_receipt WHERE kind=1 ORDER BY catalog_revision DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	request := &pb.SqlWriterLifecycleReceipt_Request{}
	if err := proto.Unmarshal(raw, request); err != nil {
		t.Fatal(err)
	}
	return request
}

// nextBoot 照上一代的原件生成同一账号的下一代:新 writer_key、instance、request_id,号是上一代 + 1,由新 Pod 发起。
func nextBoot(t *testing.T, previous *pb.SqlWriterLifecycleReceipt_Request, deployment string) *pb.SqlWriterLifecycleReceipt_Request {
	t.Helper()
	request := proto.Clone(previous).(*pb.SqlWriterLifecycleReceipt_Request)
	request.RequestId = fixtureID(t)
	request.DeploymentId = deployment
	request.ExpectedCatalogRevision = 0
	request.MaintenanceRunId = nil
	request.Writer.WriterKey = fixtureID(t)
	request.Writer.InstanceId = fixtureID(t)
	request.Writer.RegisteredMs = uint64(time.Now().UnixMilli())
	request.Writer.BootSeq = previous.Writer.BootSeq + 1
	request.Writer.RegistrationDigest = nil
	canonical, err := authoritysql.CanonicalRegistrationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func countRows(t *testing.T, ctx context.Context, operator *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := operator.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// fixPlan 在本逻辑库拍一个排空 run 的 P0:ACTIVE 全集只有 writer 这一代,它是唯一的 SOURCE。
func fixPlan(t *testing.T, ctx context.Context, catalog *authoritysql.Catalog, schema string, writer *pb.SqlWriterLifecycleReceipt_Request) (*pb.SqlWriterMaintenanceReceipt, error) {
	t.Helper()
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := fixtureID(t)
	sourcePath := filepath.Join(t.TempDir(), "succession-inventory.txt")
	source := []byte("本用例只拍 P0,不启动发布器或队列消费者。")
	if err = os.WriteFile(sourcePath, source, 0600); err != nil {
		t.Fatal(err)
	}
	sourceDigest := sha256.Sum256(source)
	seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: fixtureID(t), InstanceId: writer.Writer.InstanceId, PhysicalWriterKey: writer.Writer.WriterKey, LogicalRole: "SOURCE",
		PermanentRevivalFenceId: fixtureID(t), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: sourcePath, Sha256: sourceDigest[:]}}}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{PhysicalClusterIdentity: writer.DeploymentDigest, AuthoritySchema: schema, ProtectedSchemas: []string{schema}, RunId: run,
		BeforeEpoch: meta.WriterEpoch, CatalogRevision: meta.CatalogRevision, ActiveWriterKeys: [][]byte{writer.Writer.WriterKey}, RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
	planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	manifest := sha256.Sum256(planRaw)
	input := &pb.SqlWriterMaintenanceReceipt{RequestId: fixtureID(t), RunId: run, Generation: writer.Writer.Generation, BeforeEpoch: meta.WriterEpoch, AfterEpoch: meta.WriterEpoch,
		CatalogRevision: meta.CatalogRevision, PlanProto: planRaw, Kind: pb.SqlWriterMaintenanceReceipt_PHASE_EVENT, RunPhase: pb.SqlWriterDrainRun_PLAN_FIXED, AttemptRevision: 1,
		Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: run, ManifestDigest: manifest[:]}}
	sealed, err := authoritysql.SealMaintenanceRequest(input)
	if err != nil {
		t.Fatal(err)
	}
	next := &pb.SqlWriterDrainRun{OperationId: run, RequestDigest: sealed.RequestDigest, Generation: writer.Writer.Generation, AttemptRevision: 1, ManifestProto: planRaw,
		ManifestDigest: manifest[:], Phase: pb.SqlWriterDrainRun_PLAN_FIXED}
	return catalog.Advance(ctx, next, sealed)
}

// 同一账号的下一代注册时,在同一事务里退休上一代:上一代的下一笔写被拒(资格不符,不是可重试的冻结);
// 收据是 REGISTER(r+1)和指回它的换代 RETIRE(r+2);重放同一请求只落地一次;同号的陈旧请求被拒;换代退休不能从 Retire 发起。
func TestSlotSuccessionRetiresPreviousGeneration(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	old, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = bumpGateBusiness(ctx, old); err != nil {
		t.Fatalf("上一代原本能写: %v", err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	if first.Writer.BootSeq != 1 {
		t.Fatalf("夹具注册的是槽位第 1 代: %d", first.Writer.BootSeq)
	}
	before, err := catalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := nextBoot(t, first, "succession-pod-1")
	if err = catalog.RegistrationReady(ctx, next); err != nil {
		t.Fatalf("预检: %v", err)
	}
	receipt, err := catalog.Register(ctx, next)
	if err != nil {
		t.Fatalf("换代: %v", err)
	}
	after, err := catalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CatalogRevision != before.CatalogRevision+1 || after.CatalogRevision != before.CatalogRevision+2 || receipt.PreviousEpoch != before.WriterEpoch {
		t.Fatalf("REGISTER 在 r+1,换代退休在 r+2: receipt=%d before=%d after=%d", receipt.CatalogRevision, before.CatalogRevision, after.CatalogRevision)
	}
	var retirementID []byte
	if err = fixture.Operator.QueryRowContext(ctx, "SELECT retirement_request_id FROM writer_registry WHERE writer_key=? AND state=2", first.Writer.WriterKey).Scan(&retirementID); err != nil {
		t.Fatalf("上一代置 REVOKED: %v", err)
	}
	retire, err := catalog.LifecycleReceipt(ctx, retirementID)
	if err != nil {
		t.Fatalf("换代退休的收据要能按原 ID 读回并通过全文校验: %v", err)
	}
	retireRequest := &pb.SqlWriterLifecycleReceipt_Request{}
	if err = proto.Unmarshal(retire.RequestProto, retireRequest); err != nil {
		t.Fatal(err)
	}
	if retire.Kind != pb.SqlWriterLifecycleReceipt_RETIRE || retire.CatalogRevision != after.CatalogRevision || retire.PreviousEpoch != before.WriterEpoch ||
		!bytes.Equal(retireRequest.SuccessorRequestId, next.RequestId) || retireRequest.DeploymentId != next.DeploymentId || len(retireRequest.MaintenanceRunId) != 0 ||
		!bytes.Equal(retireRequest.Writer.WriterKey, first.Writer.WriterKey) || retireRequest.Writer.BootSeq != first.Writer.BootSeq {
		t.Fatalf("换代退休的全文: %+v", retireRequest)
	}
	// 一代结束时它的 slot 行随结束它的事务删掉(§5.8);登记行留作墓碑(上面已核 REVOKED)。
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.Writer.WriterKey); got != 0 {
		t.Fatalf("上一代的 slot 行要随换代删掉: %d", got)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=? AND state=1 AND writer_epoch=?", next.Writer.WriterKey, after.WriterEpoch); got != authoritysql.WriterSlotCount {
		t.Fatalf("新一代 64 个 slot 在当前 epoch: %d", got)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_registry WHERE sql_principal=? AND state=1", first.Writer.SqlPrincipal); got != 1 {
		t.Fatalf("一个账号只有一代 ACTIVE: %d", got)
	}
	if err = bumpGateBusiness(ctx, old); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("上一代的写要被拒,而且不是可重试的冻结: %v", err)
	}
	// slot 行已删,旧进程再试也只收到拒绝:闸门找不到自己的行,和读到 REVOKED 同一结局。
	if err = bumpGateBusiness(ctx, old); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("上一代再写仍要被拒: %v", err)
	}
	// 重放同一请求(init 重跑、上一次提交结局未知):原样返回,不再落地。
	again, err := catalog.Register(ctx, next)
	if err != nil || !proto.Equal(again, receipt) {
		t.Fatalf("重放: %v", err)
	}
	if replayed, err := catalog.CurrentMeta(ctx); err != nil || replayed.CatalogRevision != after.CatalogRevision {
		t.Fatalf("重放不推进目录版本: %v", err)
	}
	// 同号的另一份请求:同序号另一个 Pod 拿着旧号,被 CAS 拒绝。
	stale := nextBoot(t, first, "succession-pod-stale")
	if err = catalog.RegistrationReady(ctx, stale); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("预检挡住陈旧的号: %v", err)
	}
	if _, err = catalog.Register(ctx, stale); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("陈旧的号: %v", err)
	}
	// 换代退休只由注册事务写,Retire 不接。
	forged := proto.Clone(retireRequest).(*pb.SqlWriterLifecycleReceipt_Request)
	forged.RequestId = fixtureID(t)
	forged.Writer = proto.Clone(next.Writer).(*pb.SqlWriterRegistry)
	forged.ExpectedCatalogRevision = after.CatalogRevision
	if _, err = catalog.Retire(ctx, forged); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("Retire 不接受换代退休: %v", err)
	}
	// 已被换代的一代不能再人工退休:它的 slot 行已删,回确定的拒绝(不把「没有行」交给调用方),什么都不写。
	late := proto.Clone(first).(*pb.SqlWriterLifecycleReceipt_Request)
	late.RequestId = fixtureID(t)
	late.Kind = pb.SqlWriterLifecycleReceipt_RETIRE
	late.ExpectedEpoch = after.WriterEpoch
	late.ExpectedCatalogRevision = after.CatalogRevision
	late.DeploymentId = "operator-late-retire"
	if _, err = catalog.Retire(ctx, late); !errors.Is(err, authoritysql.ErrWriterDenied) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("退休已被换代的一代: %v", err)
	}
	if unchanged, err := catalog.CurrentMeta(ctx); err != nil || unchanged.CatalogRevision != after.CatalogRevision {
		t.Fatalf("被拒的退休不写目录: %v", err)
	}
	latest, err := catalog.LatestBoot(ctx, first.Writer.SqlPrincipal)
	if err != nil || latest == nil || latest.BootSeq != first.Writer.BootSeq+1 || latest.State != pb.SqlWriterRegistry_ACTIVE || !bytes.Equal(latest.WriterKey, next.Writer.WriterKey) {
		t.Fatalf("槽位当前是新一代: %+v %v", latest, err)
	}
}

// 缺口清单第 22 条的真库回归:同一个槽位连续换代,账号不变,ACTIVE 恒为一代,本逻辑库的写者全集不随启动次数增长。
func TestSlotSuccessionChainKeepsOneActiveGeneration(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := catalog.ActiveWriterKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := latestRegistration(t, ctx, fixture.Operator)
	registersBefore := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_lifecycle_receipt WHERE kind=1")
	retiresBefore := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_lifecycle_receipt WHERE kind=2")
	const boots = 20
	for boot := 0; boot < boots; boot++ {
		next := nextBoot(t, current, "succession-chain")
		if _, err = catalog.Register(ctx, next); err != nil {
			t.Fatalf("第 %d 次换代: %v", boot+1, err)
		}
		current = next
	}
	active, err := catalog.ActiveWriterKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != len(initial) {
		t.Fatalf("写者全集不随启动次数增长: %d -> %d", len(initial), len(active))
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_registry WHERE sql_principal=? AND state=1", current.Writer.SqlPrincipal); got != 1 {
		t.Fatalf("一个账号只有一代 ACTIVE: %d", got)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_registry WHERE sql_principal=?", current.Writer.SqlPrincipal); got != boots+1 {
		t.Fatalf("每一代一行注册,退休的留作永久收据: %d", got)
	}
	// slot 行只属于在役的一代(§5.8):整张表恰好是 ACTIVE 写者数 × 64,不随启动次数增长。
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot"); got != len(active)*authoritysql.WriterSlotCount {
		t.Fatalf("writer_slot 只有在役写者的行: %d 行, ACTIVE %d 个", got, len(active))
	}
	// 收据一份不删:每次换代一份 REGISTER、一份 RETIRE。
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_lifecycle_receipt WHERE kind=1"); got != registersBefore+boots {
		t.Fatalf("REGISTER 收据: %d want %d", got, registersBefore+boots)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_lifecycle_receipt WHERE kind=2"); got != retiresBefore+boots {
		t.Fatalf("RETIRE 收据: %d want %d", got, retiresBefore+boots)
	}
	latest, err := catalog.LatestBoot(ctx, current.Writer.SqlPrincipal)
	if err != nil || latest.BootSeq != boots+1 || !bytes.Equal(latest.WriterKey, current.Writer.WriterKey) {
		t.Fatalf("最新一代: %+v %v", latest, err)
	}
}

// 冻结期间换代回可重试,什么都没写,上一代还 ACTIVE;解冻之后同一请求落地,上一代按解冻后的 epoch 退休。
func TestSlotSuccessionWaitsOutFreeze(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	next := nextBoot(t, first, "succession-frozen")
	run := fixtureID(t)
	if _, err = catalog.FreezeWriters(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err = catalog.RegistrationReady(ctx, next); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("冻结期预检回可重试: %v", err)
	}
	if _, err = catalog.Register(ctx, next); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("冻结期换代回可重试: %v", err)
	}
	if _, err = catalog.LifecycleReceipt(ctx, next.RequestId); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("冻结期什么都没落地: %v", err)
	}
	if latest, err := catalog.LatestBoot(ctx, first.Writer.SqlPrincipal); err != nil || !bytes.Equal(latest.WriterKey, first.Writer.WriterKey) || latest.State != pb.SqlWriterRegistry_ACTIVE {
		t.Fatalf("上一代还在: %+v %v", latest, err)
	}
	released, err := catalog.ReleaseWriters(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := catalog.Register(ctx, next)
	if err != nil {
		t.Fatalf("解冻之后换代: %v", err)
	}
	if receipt.PreviousEpoch != released.WriterEpoch {
		t.Fatalf("收据记解冻后的 epoch: %d want %d", receipt.PreviousEpoch, released.WriterEpoch)
	}
	// 上一代按解冻后的 epoch 退休:它的 slot 行按这个 epoch 删掉(删除带 epoch 条件、必须正好 64 行),退休收据记的也是它。
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.Writer.WriterKey); got != 0 {
		t.Fatalf("上一代的 slot 行随换代删掉: %d", got)
	}
	var retirementID []byte
	if err = fixture.Operator.QueryRowContext(ctx, "SELECT retirement_request_id FROM writer_registry WHERE writer_key=? AND state=2", first.Writer.WriterKey).Scan(&retirementID); err != nil {
		t.Fatalf("上一代置 REVOKED: %v", err)
	}
	if retire, err := catalog.LifecycleReceipt(ctx, retirementID); err != nil || retire.PreviousEpoch != released.WriterEpoch {
		t.Fatalf("上一代按解冻后的 epoch 退休: %+v %v", retire, err)
	}
}

// 换代与排空 run(批 3):run 之前换代的新一代就是 P0 的 ACTIVE 全集,按换代之前的全集拍 P0 被拒;
// run 开着时换代被拒(带不带同一个 run 都一样),什么都不写,目录版本不动。
func TestSlotSuccessionAndDrainRun(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	second := nextBoot(t, first, "succession-before-run")
	if _, err = catalog.Register(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err = fixPlan(t, ctx, catalog, schema, first); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("按换代之前的 ACTIVE 全集拍的 P0 要被拒: %v", err)
	}
	if _, err = fixPlan(t, ctx, catalog, schema, second); err != nil {
		t.Fatalf("按换代之后的全集拍 P0: %v", err)
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil || len(meta.FreezeRunId) != 16 {
		t.Fatalf("run 开着: %+v %v", meta, err)
	}
	third := nextBoot(t, second, "succession-during-run")
	if err = catalog.RegistrationReady(ctx, third); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("run 开着时预检拒绝换代: %v", err)
	}
	if _, err = catalog.Register(ctx, third); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("run 开着时拒绝换代: %v", err)
	}
	inRun := proto.Clone(third).(*pb.SqlWriterLifecycleReceipt_Request)
	inRun.RequestId = fixtureID(t)
	inRun.MaintenanceRunId = meta.FreezeRunId
	inRun.Writer.RegistrationDigest = nil
	if inRun, err = authoritysql.CanonicalRegistrationRequest(inRun); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Register(ctx, inRun); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("带同一个 run 也不能在排空期间换代: %v", err)
	}
	unchanged, err := catalog.CurrentMeta(ctx)
	if err != nil || unchanged.CatalogRevision != meta.CatalogRevision {
		t.Fatalf("run 期间的拒绝什么都不写: %v", err)
	}
	active, err := catalog.ActiveWriterKeys(ctx)
	if err != nil || len(active) != 1 || !bytes.Equal(active[0], second.Writer.WriterKey) {
		t.Fatalf("ACTIVE 还是 P0 里的那一代: %v %v", active, err)
	}
	// run 之前那次换代已删掉第 1 代的 slot 行;run 期间被拒的换代一行不动,P0 里那一代的 64 行还在役。
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.Writer.WriterKey); got != 0 {
		t.Fatalf("run 之前换代已删上一代的 slot 行: %d", got)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=? AND state=1 AND writer_epoch=?", second.Writer.WriterKey, meta.WriterEpoch); got != authoritysql.WriterSlotCount {
		t.Fatalf("被拒的换代不碰在役一代的 slot 行: %d", got)
	}
}

// 换代要等上一代在途的写事务结束才删它的 slot 行(§5.8「没有在途事务」):在途那笔照常提交、不丢,
// 换代随后落地,上一代下一笔写被拒。锁等待按库里的锁视图观测,不靠 sleep 推定已经卡住。
func TestSlotSuccessionWaitsForInFlightTransaction(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	old, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	tx, err := old.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("上一代开事务: %v", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE gate_business SET value=value+1 WHERE id=1"); err != nil {
		t.Fatalf("上一代在途的写: %v", err)
	}
	next := nextBoot(t, first, "succession-in-flight")
	done := make(chan error, 1)
	go func() {
		_, e := catalog.Register(ctx, next)
		done <- e
	}()
	waitWriterSlotLockWait(t, ctx, fixture, schema, done)
	if err = tx.Commit(); err != nil {
		t.Fatalf("在途事务在换代之前提交: %v", err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("在途事务提交之后换代落地: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var value int64
	if err = fixture.Operator.QueryRowContext(ctx, "SELECT value FROM gate_business WHERE id=1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("换代之前提交的那笔写要保留: %d %v", value, err)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.Writer.WriterKey); got != 0 {
		t.Fatalf("上一代的 slot 行随换代删掉: %d", got)
	}
	if err = bumpGateBusiness(ctx, old); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("上一代下一笔写被拒,不是可重试的冻结: %v", err)
	}
}

// waitWriterSlotLockWait 等到本用例库里有事务在等锁(用例里只有换代会等,等的是上一代在途事务持着的 slot 行);
// done 先回来说明没等锁就结束了,用例失败。TiDB 看 information_schema.DATA_LOCK_WAITS(与 writer_tidb_test.go 同口径,按随机库名过滤),
// MySQL 看 performance_schema.data_locks 里 writer_slot 上 WAITING 的锁。每次查询本身就是节拍,不 sleep。
func waitWriterSlotLockWait(t *testing.T, ctx context.Context, fixture *sqltest.Fixture, schema string, done <-chan error) {
	t.Helper()
	query, args := "SELECT COUNT(*) FROM performance_schema.data_locks WHERE OBJECT_SCHEMA=? AND OBJECT_NAME='writer_slot' AND LOCK_STATUS='WAITING'", []any{schema}
	if fixture.Backend() == authoritysql.BackendTiDB {
		query, args = "SELECT COUNT(*) FROM information_schema.DATA_LOCK_WAITS WHERE KEY_INFO LIKE ?", []any{"%" + schema + "%"}
	}
	for {
		var waits int
		if err := fixture.Schema.QueryRowContext(ctx, query, args...).Scan(&waits); err != nil {
			t.Fatalf("读不到锁等待视图: %v", err)
		}
		if waits > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("换代没等上一代的在途事务就结束了: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
	}
}

// 人工退休(缩容留下的末代,非 run 的 RETIRE,writer 原样复制 REGISTER 里的、带 bootSeq)也结束这一代:slot 行删掉、登记行 REVOKED,
// 重放同一请求原样返回;旧进程写被拒;同一槽位的下一代之后照常注册(没有在役的上一代,只注册不换代),slot 行仍只属于在役的一代;
// 已退休的一代换个 request_id 再退一次,回确定的拒绝,不是「没有行」。
func TestManualRetireEndsGenerationAndSlotRegistersAgain(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	old, err := fixture.Open(ctx, schema, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = bumpGateBusiness(ctx, old); err != nil {
		t.Fatalf("退休之前能写: %v", err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retire := proto.Clone(first).(*pb.SqlWriterLifecycleReceipt_Request)
	retire.RequestId = fixtureID(t)
	retire.Kind = pb.SqlWriterLifecycleReceipt_RETIRE
	retire.ExpectedEpoch = meta.WriterEpoch
	retire.ExpectedCatalogRevision = meta.CatalogRevision
	retire.DeploymentId = "operator-scale-down"
	receipt, err := catalog.Retire(ctx, retire)
	if err != nil {
		t.Fatalf("人工退休: %v", err)
	}
	if receipt.CatalogRevision != meta.CatalogRevision+1 || receipt.PreviousEpoch != meta.WriterEpoch {
		t.Fatalf("退休只推进一次目录版本、不动 epoch: %+v", receipt)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=?", first.Writer.WriterKey); got != 0 {
		t.Fatalf("退休删掉这一代的 slot 行: %d", got)
	}
	var retirementID []byte
	if err = fixture.Operator.QueryRowContext(ctx, "SELECT retirement_request_id FROM writer_registry WHERE writer_key=? AND state=2", first.Writer.WriterKey).Scan(&retirementID); err != nil || !bytes.Equal(retirementID, retire.RequestId) {
		t.Fatalf("登记行置 REVOKED 并指回这份 RETIRE: %x %v", retirementID, err)
	}
	// 重放同一请求(人工重跑、上一次结局未知):按收据原样返回,不受 slot 行已删影响,不再推进目录。
	again, err := catalog.Retire(ctx, retire)
	if err != nil || !proto.Equal(again, receipt) {
		t.Fatalf("重放退休: %v", err)
	}
	if current, err := catalog.CurrentMeta(ctx); err != nil || current.CatalogRevision != receipt.CatalogRevision {
		t.Fatalf("重放不推进目录版本: %v", err)
	}
	if err = bumpGateBusiness(ctx, old); !errors.Is(err, authoritysql.ErrWriterDenied) || authoritysql.IsWriterFrozen(err) {
		t.Fatalf("退休之后的写被拒,不是可重试的冻结: %v", err)
	}
	// 同一槽位再扩回来:没有在役的上一代,只注册(目录版本 +1,没有换代退休),号接着往上走。
	next := nextBoot(t, first, "after-manual-retire")
	registered, err := catalog.Register(ctx, next)
	if err != nil {
		t.Fatalf("退休之后同一槽位注册下一代: %v", err)
	}
	after, err := catalog.CurrentMeta(ctx)
	if err != nil || registered.CatalogRevision != receipt.CatalogRevision+1 || after.CatalogRevision != registered.CatalogRevision {
		t.Fatalf("只注册、不换代: %+v %v", registered, err)
	}
	active, err := catalog.ActiveWriterKeys(ctx)
	if err != nil || len(active) != 1 || !bytes.Equal(active[0], next.Writer.WriterKey) {
		t.Fatalf("在役的是新一代: %v %v", active, err)
	}
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot"); got != authoritysql.WriterSlotCount {
		t.Fatalf("slot 行只属于在役的一代: %d", got)
	}
	// 已退休的一代换个 request_id 再退:它没有 slot 行,回确定的拒绝,什么都不写。
	late := proto.Clone(retire).(*pb.SqlWriterLifecycleReceipt_Request)
	late.RequestId = fixtureID(t)
	late.ExpectedEpoch = after.WriterEpoch
	late.ExpectedCatalogRevision = after.CatalogRevision
	if _, err = catalog.Retire(ctx, late); !errors.Is(err, authoritysql.ErrWriterDenied) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("再退已退休的一代: %v", err)
	}
	if current, err := catalog.CurrentMeta(ctx); err != nil || current.CatalogRevision != after.CatalogRevision {
		t.Fatalf("被拒的退休不写目录: %v", err)
	}
}

// Registration 按登记行读回这一代的 REGISTER 原件(缩容收尾拿它看这一代是谁登记的,§5.9):registered_by 原样读回,
// 在役的和已经结束的一代都读得到(收据永久保留);登记行和收据对不上、找不到收据都回确定的拒绝。
func TestRegistrationReadsBackTheRegisterRequest(t *testing.T) {
	ctx, fixture, schema, _ := twoLogicalDBFixture(t)
	catalog, err := fixture.CatalogFor(schema)
	if err != nil {
		t.Fatal(err)
	}
	first := latestRegistration(t, ctx, fixture.Operator)
	firstRow, err := catalog.LatestBoot(ctx, first.Writer.SqlPrincipal)
	if err != nil || firstRow == nil {
		t.Fatalf("夹具的一代: %+v %v", firstRow, err)
	}
	got, err := catalog.Registration(ctx, firstRow)
	if err != nil || !proto.Equal(got, first) || got.RegisteredBy != "" {
		t.Fatalf("夹具在集群外登记,REGISTER 原样读回、不带身份: %+v %v", got, err)
	}
	next := proto.Clone(nextBoot(t, first, "registration-read-back")).(*pb.SqlWriterLifecycleReceipt_Request)
	next.RegisteredBy = "k8s-sa:" + schema + "/default/0f8fad5b-d9cb-469f-a165-70867728950e"
	next.Writer.RegistrationDigest = nil
	if next, err = authoritysql.CanonicalRegistrationRequest(next); err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.Register(ctx, next); err != nil {
		t.Fatalf("带身份的一代换代: %v", err)
	}
	nextRow, err := catalog.LatestBoot(ctx, next.Writer.SqlPrincipal)
	if err != nil || nextRow == nil || !bytes.Equal(nextRow.WriterKey, next.Writer.WriterKey) {
		t.Fatalf("新一代的登记行: %+v %v", nextRow, err)
	}
	if got, err = catalog.Registration(ctx, nextRow); err != nil || !proto.Equal(got, next) || got.RegisteredBy != next.RegisteredBy {
		t.Fatalf("registered_by 原样读回: %+v %v", got, err)
	}
	// 上一代已被换代结束,它的 REGISTER 收据还在。
	if got, err = catalog.Registration(ctx, firstRow); err != nil || !proto.Equal(got, first) {
		t.Fatalf("已结束的一代也读得到: %+v %v", got, err)
	}
	// 登记行的序号对不上收据里的 writer:拒绝。
	forged := proto.Clone(nextRow).(*pb.SqlWriterRegistry)
	forged.BootSeq++
	if _, err = catalog.Registration(ctx, forged); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("登记行与收据对不上要拒绝: %v", err)
	}
	// 没有这份摘要的 REGISTER 收据:拒绝,不把「没有行」交出去。
	unknown := proto.Clone(nextRow).(*pb.SqlWriterRegistry)
	unknown.RegistrationDigest = bytes.Repeat([]byte{9}, 32)
	if _, err = catalog.Registration(ctx, unknown); !errors.Is(err, authoritysql.ErrWriterDenied) || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("找不到收据要拒绝: %v", err)
	}
}
