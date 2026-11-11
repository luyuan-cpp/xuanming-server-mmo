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
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=? AND state=2", first.Writer.WriterKey); got != authoritysql.WriterSlotCount {
		t.Fatalf("上一代的 64 个 slot 都要 REVOKED: %d", got)
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
	if got := countRows(t, ctx, fixture.Operator, "SELECT COUNT(*) FROM writer_slot WHERE writer_key=? AND state=2 AND writer_epoch=?", first.Writer.WriterKey, released.WriterEpoch); got != authoritysql.WriterSlotCount {
		t.Fatalf("上一代按解冻后的 epoch 退休: %d", got)
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
}
