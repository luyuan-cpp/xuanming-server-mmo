package authoritysql

import (
	"bytes"
	"context"
	"errors"
	"google.golang.org/protobuf/proto"
	"net"
	pb "proto/common/database"
	"strconv"
	"testing"
)

func TestOpenRejectsWrongDatabaseBeforeConnecting(t *testing.T) {
	if _, err := Open(context.Background(), "root@tcp(127.0.0.1:1)/other", "expected", BackendTiDB, 4, Reader()); err == nil {
		t.Fatal("不应连接错误库")
	}
}

func TestMigrationRejectsWrongDatabaseBeforeConnecting(t *testing.T) {
	if _, err := OpenForMigration(context.Background(), "root@tcp(127.0.0.1:1)/other", "expected", BackendTiDB, Schema("schema_fixture@%")); err == nil {
		t.Fatal("迁移不应连接错误库")
	}
}
func physicalTiDBFixtureEndpoint(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("物理连接夹具必须显式数值loopback端口")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("物理连接夹具仅接受数值loopback与合法非零端口")
	}
	return nil
}

func TestPhysicalTiDBFixtureEndpointRejectsForeignOrInvalidPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:24000", "[::1]:24000"} {
		if err := physicalTiDBFixtureEndpoint(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"localhost:24000", "10.0.0.1:24000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:mysql", "127.0.0.1"} {
		if physicalTiDBFixtureEndpoint(address) == nil {
			t.Fatal("非法夹具端点未拒绝")
		}
	}
}

func TestMaintenancePublicationRequiresFrozenSameBootClosure(t *testing.T) {
	seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), LogicalRole: "MAINTENANCE"}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{MaintenanceIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
	inventory := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers}
	digest, err := messageDigest(inventory)
	if err != nil {
		t.Fatal(err)
	}
	closed := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	closed.AcceptingClosedMs = 100
	closed.AcceptedSettledMs = 101
	closed.AcceptedFrontierDigest = bytes.Repeat([]byte{3}, 32)
	ready := &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{MaintenancePublication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}, IssuerInventoryDigest: digest, SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: "tool.issuer.json", Sha256: bytes.Repeat([]byte{4}, 32)}}}}
	if err := validateMaintenanceClosure(plan, ready, 100); err != nil {
		t.Fatal("同冻结后的完整闭合被拒绝", err)
	}
	for _, change := range []func(*pb.SqlWriterMaintenanceReceipt_ReleaseReadiness){
		func(r *pb.SqlWriterMaintenanceReceipt_ReleaseReadiness) {
			r.MaintenancePublication.LogicalRoles[0].AcceptingClosedMs = 99
		},
		func(r *pb.SqlWriterMaintenanceReceipt_ReleaseReadiness) {
			r.MaintenancePublication.LogicalRoles[0].InstanceId = bytes.Repeat([]byte{5}, 16)
		},
		func(r *pb.SqlWriterMaintenanceReceipt_ReleaseReadiness) {
			r.MaintenancePublication.LogicalRoles[0].UnresolvedPublications = 1
		},
		func(r *pb.SqlWriterMaintenanceReceipt_ReleaseReadiness) {
			r.MaintenancePublication.SourceEvidence = nil
		},
	} {
		bad := proto.Clone(ready).(*pb.SqlWriterMaintenanceReceipt_ReleaseReadiness)
		change(bad)
		if validateMaintenanceClosure(plan, bad, 100) == nil {
			t.Fatal("冻结前、换boot或不完整闭合获准")
		}
	}
	if validateMaintenanceClosure(&pb.SqlWriterMaintenanceReceipt_Plan{}, ready, 100) == nil {
		t.Fatal("空P0目录容纳了事后增加的工具")
	}
	if err := validateMaintenanceClosure(&pb.SqlWriterMaintenanceReceipt_Plan{}, &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{}, 100); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRuntimeBindsFreshPreparedBootToPermanentRelease(t *testing.T) {
	id := func(value byte) []byte { return bytes.Repeat([]byte{value}, 16) }
	hash := func(value byte) []byte { return bytes.Repeat([]byte{value}, 32) }
	sourcePin := &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: "source.seed.json", Sha256: hash(11)}
	seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: id(1), InstanceId: id(2), LogicalRole: "SOURCE", PermanentRevivalFenceId: id(3), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{sourcePin}}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: id(4), RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}
	raw, err := deterministic(plan)
	if err != nil {
		t.Fatal(err)
	}
	released := &pb.SqlWriterMaintenanceReceipt{RunId: id(4), Generation: "generation", AttemptRevision: 1, AfterEpoch: 3, CommittedMs: 100, PlanProto: raw, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: id(4), ManifestDigest: hash(5)}}
	fresh := proto.Clone(seed).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	fresh.InstanceId = id(6)
	runtime := proto.Clone(released).(*pb.SqlWriterMaintenanceReceipt)
	runtime.RunPhase = pb.SqlWriterDrainRun_SQL_RELEASED
	runtime.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{fresh}}}
	ready := &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{IssuerId: id(1), InstanceId: id(6), LogicalRole: "SOURCE", PreparedClosedMs: 101, ConsumerReadyFrontierDigest: hash(7), SourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{sourcePin, {AbsolutePath: "consumer-readiness.report.json", Sha256: hash(7)}}}
	if err := validatePublicationRuntime(runtime, ready, plan, released); err != nil {
		t.Fatal("真实非SQL新boot准备原件被拒绝", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.SqlWriterMaintenanceReceipt, *pb.SqlWriterMaintenanceReceipt_IssuerReadiness)
	}{
		{"旧boot", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.InstanceId = id(2)
			r.Evidence.GetPublication().LogicalRoles[0].InstanceId = id(2)
		}},
		{"跨run", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			r.RunId = id(9)
		}},
		{"旧epoch", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			r.AfterEpoch = 2
		}},
		{"未知issuer", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.IssuerId = id(9)
			r.Evidence.GetPublication().LogicalRoles[0].IssuerId = id(9)
		}},
		{"冻结前准备", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.PreparedClosedMs = 99
		}},
		{"缺原始pin", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.SourcePins = b.SourcePins[1:]
		}},
		{"交换pin", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.SourcePins[0], b.SourcePins[1] = b.SourcePins[1], b.SourcePins[0]
		}},
		{"报告摘要变化", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.ConsumerReadyFrontierDigest = hash(8)
		}},
		{"重复报告摘要", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.SourcePins[0].Sha256 = hash(7)
			r.Evidence.GetPublication().LogicalRoles[0].SourceEvidence[0].Sha256 = hash(7)
		}},
		{"非SQL伪注册", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			b.RegistrationReceiptId = id(8)
		}},
		{"缺SOURCE", func(r *pb.SqlWriterMaintenanceReceipt, b *pb.SqlWriterMaintenanceReceipt_IssuerReadiness) {
			r.Evidence.GetPublication().LogicalRoles = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proto.Clone(runtime).(*pb.SqlWriterMaintenanceReceipt)
			b := proto.Clone(ready).(*pb.SqlWriterMaintenanceReceipt_IssuerReadiness)
			tc.change(r, b)
			if validatePublicationRuntime(r, b, plan, released) == nil {
				t.Fatal("无效发布准备被接受")
			}
		})
	}
	// SQL发行者只接受另一个boot与全新writer，真正永久REGISTER及全部64槽由事务内强读校验。
	sqlPlan := proto.Clone(plan).(*pb.SqlWriterMaintenanceReceipt_Plan)
	sqlPlan.RootIssuers[0].PhysicalWriterKey = id(12)
	sqlRaw, err := deterministic(sqlPlan)
	if err != nil {
		t.Fatal(err)
	}
	sqlReleased := proto.Clone(released).(*pb.SqlWriterMaintenanceReceipt)
	sqlReleased.PlanProto = sqlRaw
	sqlRuntime := proto.Clone(runtime).(*pb.SqlWriterMaintenanceReceipt)
	sqlRuntime.PlanProto = sqlRaw
	sqlRuntime.Evidence.GetPublication().LogicalRoles[0].PhysicalWriterKey = id(13)
	sqlReady := proto.Clone(ready).(*pb.SqlWriterMaintenanceReceipt_IssuerReadiness)
	sqlReady.PhysicalWriterKey = id(13)
	sqlReady.RegistrationReceiptId = id(14)
	sqlReady.RegistrationReceiptDigest = hash(15)
	if err := validatePublicationRuntime(sqlRuntime, sqlReady, sqlPlan, sqlReleased); err != nil {
		t.Fatal(err)
	}
	sqlRuntime.Evidence.GetPublication().LogicalRoles[0].PhysicalWriterKey = id(12)
	sqlReady.PhysicalWriterKey = id(12)
	if validatePublicationRuntime(sqlRuntime, sqlReady, sqlPlan, sqlReleased) == nil {
		t.Fatal("旧物理writer复用未拒绝")
	}
}
