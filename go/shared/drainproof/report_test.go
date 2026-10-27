package drainproof

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/placement"
	"testing"
)

func TestCloseRetainsAcceptedUntilActualCompletion(t *testing.T) {
	gate := New()
	done, err := gate.Enter()
	if err != nil {
		t.Fatal(err)
	}
	gate.CloseAdmission()
	if _, err = gate.Enter(); !errors.Is(err, ErrClosed) {
		t.Fatal("关闭后仍接受新工作")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = gate.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("未完成必须HOLD: %v", err)
	}
	done()
	done()
	if err = gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestUnknownCannotBecomeSuccessByCancellation(t *testing.T) {
	gate := New()
	gate.Hold()
	gate.CloseAdmission()
	if !errors.Is(gate.Wait(context.Background()), ErrHold) {
		t.Fatal("未知结局被清零")
	}
}
func TestRolesDoNotCloseEachOther(t *testing.T) {
	source, relay := New(), New()
	source.CloseAdmission()
	if _, err := relay.Enter(); err != nil {
		t.Fatal("SOURCE关闭了同boot RELAY")
	}
}

func TestPreparedOpensOnlyFreshGateAndCannotReopenClosed(t *testing.T) {
	gate := NewPrepared()
	if _, err := gate.Enter(); err == nil {
		t.Fatal("prepared尚未open却接根请求")
	}
	if err := gate.OpenPrepared(); err != nil {
		t.Fatal(err)
	}
	done, err := gate.Enter()
	if err != nil {
		t.Fatal(err)
	}
	done()
	gate.CloseAdmission()
	if err := gate.OpenPrepared(); err == nil {
		t.Fatal("永久关闭的旧boot重新open")
	}
}

func TestRuntimeBindsOnlyActualBootRoles(t *testing.T) {
	root := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), PhysicalWriterKey: bytes.Repeat([]byte{3}, 16), LogicalRole: "SOURCE", PermanentRevivalFenceId: bytes.Repeat([]byte{4}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: "fixture-source", Sha256: bytes.Repeat([]byte{5}, 32)}}}
	relay := proto.Clone(root).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	relay.IssuerId = bytes.Repeat([]byte{6}, 16)
	relay.LogicalRole = "RELAY"
	for _, roles := range [][]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{{root}, {relay}, {root, relay}} {
		r := &Runtime{Plan: &pb.SqlWriterMaintenanceReceipt_Plan{RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{root}, QueueMutators: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{relay}}, Request: &pb.SqlWriterMaintenanceReceipt{RunPhase: pb.SqlWriterDrainRun_PLAN_FIXED, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: roles}}}}}
		seeds, err := r.roleSeeds()
		if err != nil || len(seeds) != len(roles) {
			t.Fatalf("真实角色被强迫补齐不存在角色: %d %v", len(seeds), err)
		}
		r.Request.Evidence.GetPublication().LogicalRoles = append(append([]*pb.SqlWriterMaintenanceReceipt_IssuerClosure{}, roles...), roles[0])
		if _, err = r.roleSeeds(); err == nil {
			t.Fatal("重复角色被默默忽略")
		}
	}
}

func TestClosedSessionMergesOnlySettledBeforeProof(t *testing.T) {
	digest := bytes.Repeat([]byte{7}, 32)
	gate := New()
	done, err := gate.Enter()
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gate.MergeClosedSettled(digest), ErrHold) {
		t.Fatal("尚未关入口却合并最终前沿")
	}
	gate.CloseAdmission()
	if !errors.Is(gate.MergeClosedSettled(digest), ErrHold) {
		t.Fatal("仍有已受理工作却合并最终前沿")
	}
	done()
	if err := gate.MergeClosedSettled(digest); err != nil {
		t.Fatal(err)
	}
	got, err := gate.SettledDigest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gate.MergeClosedSettled(digest), ErrHold) {
		t.Fatal("已发前沿被后续合并改写")
	}
	again, err := gate.SettledDigest(context.Background())
	if err != nil || !bytes.Equal(got, again) {
		t.Fatal("关闭前沿不稳定")
	}
}
func TestClosedSessionCannotMergeAwayUnknown(t *testing.T) {
	gate := New()
	gate.CloseAdmission()
	gate.Hold()
	if !errors.Is(gate.MergeClosedSettled(bytes.Repeat([]byte{8}, 32)), ErrHold) {
		t.Fatal("未知结果被下游摘要冲销")
	}
	if _, err := gate.SettledDigest(context.Background()); !errors.Is(err, ErrHold) {
		t.Fatal("未知结果产生前沿")
	}
}

func TestParseConsumerReadinessRequiresFormalOriginal(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte("{}"), []byte(`{"groups":null}`), []byte(`{"groups":[],"ignored":true}`), {0x10, 0x01}} {
		if _, err := ParseConsumerReadiness(raw); err == nil {
			t.Fatalf("非正式原件获准: %q", raw)
		}
	}
	if report, err := ParseConsumerReadiness([]byte(`{"groups":[]}`)); err != nil || len(report.Groups) != 0 {
		t.Fatalf("正式空组无法解析: %v", err)
	}
	raw, err := proto.Marshal(&pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{Groups: []*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{{Group: "g"}}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := ParseConsumerReadiness(raw)
	if err != nil || len(report.Groups) != 1 || report.Groups[0].Group != "g" {
		t.Fatalf("正式跨语言二进制无法解析: %v", err)
	}
	report.Groups[0].ProtoReflect().SetUnknown([]byte{0x78, 0x01})
	raw, err = proto.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseConsumerReadiness(raw); err == nil {
		t.Fatal("嵌套未知字段被忽略")
	}
}

func TestRouteRecoverySnapshotBindsAtomicIdentityAndNonce(t *testing.T) {
	run := bytes.Repeat([]byte{1}, 16)
	nonce := bytes.Repeat([]byte{2}, 16)
	redisRun := string(bytes.Repeat([]byte{'a'}, 40))
	replica := string(bytes.Repeat([]byte{'b'}, 40))
	key, err := RedisMaintenanceNonceKey(run)
	if err != nil || key != "sql:writer:maintenance:"+hex.EncodeToString(run)+":nonce" {
		t.Fatal("nonce键未绑定唯一run", err)
	}
	if _, err := RedisMaintenanceNonceKey(make([]byte, 16)); err == nil {
		t.Fatal("零run被接受")
	}
	marker, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{Generation: "abc", RedisRunID: redisRun, MasterReplID: replica, SetAtMs: 1, WatermarkMs: 2, Leader: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	target := &pb.SqlWriterMaintenanceReceipt_RedisTargetClosure{ServerRunId: []byte(redisRun), MaintenanceNonce: nonce}
	raw := []any{"run_id:" + redisRun + "\r\n", "role:master\r\nmaster_replid:" + replica + "\r\n", marker, nil, hex.EncodeToString(nonce)}
	snapshot, err := ParseRouteRecoverySnapshot(raw, run, target)
	if err != nil || !bytes.Equal(snapshot.ReadyMarkerRaw, []byte(marker)) || snapshot.WatermarkRaw != nil {
		t.Fatal("真实同批原件被拒绝", err)
	}
	for _, tc := range []struct {
		name  string
		index int
		value any
	}{
		{"不同runid", 0, "run_id:" + string(bytes.Repeat([]byte{'c'}, 40)) + "\r\n"},
		{"副本", 1, "role:slave\r\nmaster_replid:" + replica + "\r\n"},
		{"丢完整marker", 2, nil},
		{"缺nonce", 4, nil},
		{"nonce已变化", 4, hex.EncodeToString(bytes.Repeat([]byte{3}, 16))},
		{"类型错误", 4, int64(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]any{}, raw...)
			bad[tc.index] = tc.value
			if _, err := ParseRouteRecoverySnapshot(bad, run, target); err == nil {
				t.Fatal("跨实例或nonce不明的快照被接受")
			}
		})
	}
}
