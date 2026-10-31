package maintenancephase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	pb "proto/common/database"
	"shared/drainproof"
	"shared/handofffence"
	"shared/kafkautil"
	"shared/redisslot"
	"strings"
	"testing"
)

func authorityTestPin(t *testing.T, name string, raw []byte) *pb.SqlWriterMaintenanceReceipt_SourcePin {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]}
}
func authorityTestPlan(t *testing.T) *pb.SqlWriterMaintenanceReceipt_Plan {
	t.Helper()
	topic := "db_task_zone_1"
	cluster := []byte("真实fixture-cluster")
	id := bytes.Repeat([]byte{2}, 16)
	config := map[string]string{"retention.ms": "60000", "retention.bytes": "1024", "cleanup.policy": "delete", "segment.ms": "1000", "segment.bytes": "1024", "delete.retention.ms": "1000", "min.insync.replicas": "1", "unclean.leader.election.enable": "false"}
	catalog := kafkautil.DrainKafkaCatalog{Schema: "SQL_DRAIN_KAFKA_CATALOG_V2", ObservedStartMS: 1, ObservedEndMS: 2, ClusterID: cluster, TopicNames: []string{"__consumer_offsets", topic}, GroupNames: []string{"db-group"}, TopicIDs: map[string][]byte{topic: id, "__consumer_offsets": bytes.Repeat([]byte{3}, 16)}, TopicPartitions: map[string]int{topic: 1, "__consumer_offsets": 1}, TopicConfigs: map[string]map[string]string{topic: config, "__consumer_offsets": config}, BrokerConfigs: map[string]map[string]string{"0": {"log.retention.ms": "60000", "log.retention.bytes": "1024", "log.segment.bytes": "1024", "log.retention.check.interval.ms": "1000", "unclean.leader.election.enable": "false", "address": "127.0.0.1:19092"}}}
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalogPin := authorityTestPin(t, "catalog.json", raw)
	row := &pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{ClusterId: cluster, TopicId: id, TopicName: topic, Group: "db-group", Partition: 0, ProtectedCommittedFloor: 0, InitialEarliest: 0, FinalEarliest: 0, FinalExclusiveEnd: 0, FinalCommitted: 0}
	baseline := kafkautil.DrainKafkaBaseline{Schema: "SQL_DRAIN_KAFKA_BASELINE_V2", CatalogSHA256: catalogPin.Sha256, ObservedMS: 2, Partitions: []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{row}}
	raw, err = json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	baselinePin := authorityTestPin(t, "baseline.json", raw)
	source := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{4}, 16), InstanceId: bytes.Repeat([]byte{5}, 16), PhysicalWriterKey: bytes.Repeat([]byte{6}, 16), LogicalRole: "SOURCE", PermanentRevivalFenceId: bytes.Repeat([]byte{7}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{catalogPin}}
	relay := proto.Clone(source).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	relay.IssuerId = bytes.Repeat([]byte{8}, 16)
	relay.LogicalRole = "RELAY"
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{PhysicalClusterIdentity: bytes.Repeat([]byte{9}, 32), AuthoritySchema: "fixture_routes", ProtectedSchemas: []string{"fixture_routes", "zone_1_db"}, ActiveWriterKeys: [][]byte{source.PhysicalWriterKey}, BeforeEpoch: 1, CatalogRevision: 1, RunId: bytes.Repeat([]byte{10}, 16), RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{source}, QueueMutators: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{relay}, KafkaPartitions: []*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{row}, RedisTargets: []*pb.SqlWriterMaintenanceReceipt_RedisTargetClosure{{Role: "core", Address: "127.0.0.1:16379", Db: 0, ServerRunId: []byte("0123456789012345678901234567890123456789"), MaintenanceNonce: bytes.Repeat([]byte{11}, 16), Zones: []uint32{1}}}, SourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{catalogPin, baselinePin}, Launchers: []*pb.SqlWriterMaintenanceReceipt_LaunchClosure{{LauncherId: bytes.Repeat([]byte{12}, 16), PermanentFenceId: bytes.Repeat([]byte{13}, 16), SourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{catalogPin}}}}
	nonceKey, err := drainproof.RedisMaintenanceNonceKey(plan.RunId)
	if err != nil {
		t.Fatal(err)
	}
	target := plan.RedisTargets[0]
	redisBaseline := authorityRedisScan{Schema: "SQL_DRAIN_REDIS_BASELINE_V1", Role: target.Role, Address: target.Address, DB: target.Db, RunID: target.ServerRunId, ObservedStartMS: 1, ObservedEndMS: 2, NonceKey: nonceKey, NonceBefore: target.MaintenanceNonce, NonceAfter: target.MaintenanceNonce, NoncePTTLBeforeMS: -1, NoncePTTLAfterMS: -1}
	redisRaw, err := json.Marshal(redisBaseline)
	if err != nil {
		t.Fatal(err)
	}
	plan.SourcePins = append(plan.SourcePins, authorityTestPin(t, "redis-baseline.json", redisRaw))
	return plan
}
func TestAuthorityPlanRequiresActualProtectedFloor(t *testing.T) {
	plan := authorityTestPlan(t)
	if err := ValidateAuthorityPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.KafkaPartitions[0].ProtectedCommittedFloor = 1
	if err := ValidateAuthorityPlan(plan); err == nil {
		t.Fatal("手填C0越过原实际保护floor")
	}
}
func TestAuthorityPlanRejectsPartialHistoricalPartitionSet(t *testing.T) {
	plan := authorityTestPlan(t)
	plan.KafkaPartitions[0].Partition = 1
	if err := ValidateAuthorityPlan(plan); err == nil {
		t.Fatal("同COUNT换分区未拒绝")
	}
}
func TestAuthorityScanRequiresEveryCursorChain(t *testing.T) {
	plan := authorityTestPlan(t)
	target := plan.RedisTargets[0]
	nonceKey, err := drainproof.RedisMaintenanceNonceKey(plan.RunId)
	if err != nil {
		t.Fatal(err)
	}
	source := authorityRedisScan{Schema: "SQL_DRAIN_REDIS_SCAN_V2", Role: target.Role, Address: target.Address, DB: target.Db, RunID: target.ServerRunId, ObservedStartMS: 3, ObservedEndMS: 4, NonceKey: nonceKey, NonceBefore: target.MaintenanceNonce, NonceAfter: target.MaintenanceNonce, NoncePTTLBeforeMS: -1, NoncePTTLAfterMS: -1}
	for _, pattern := range authorityRedisPatterns {
		source.Pages = append(source.Pages, authorityRedisScanPage{Pattern: pattern, Input: 0, Output: 0})
	}
	raw, _ := json.Marshal(source)
	target.ScanSourceEvidence = []*pb.SqlWriterMaintenanceReceipt_SourcePin{authorityTestPin(t, "scan.json", raw)}
	if _, err := authorityScanKeys(target, 3, 4, plan.RunId); err != nil {
		t.Fatal(err)
	}
	source.Pages[0].Output = 1
	raw, _ = json.Marshal(source)
	target.ScanSourceEvidence = []*pb.SqlWriterMaintenanceReceipt_SourcePin{authorityTestPin(t, "incomplete.json", raw)}
	if _, err := authorityScanKeys(target, 3, 4, plan.RunId); err == nil {
		t.Fatal("cursor未回0却被当完整扫描")
	}
}
func TestAuthorityPhaseRejectsMissingOwnerWithoutSQL(t *testing.T) {
	if _, err := AuthorityPhase(context.Background(), nil, nil, "advance", nil, nil); err == nil {
		t.Fatal("缺真实Catalog仍允许阶段动作")
	}
}

func consumerReadinessFixture(t *testing.T, plan *pb.SqlWriterMaintenanceReceipt_Plan, member byte, generation int32, assigned []uint32, change func(*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness)) *pb.SqlWriterMaintenanceReceipt_IssuerReadiness {
	t.Helper()
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	manifestSum := sha256.Sum256(manifest)
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{OperationId: plan.RunId, ManifestDigest: manifestSum[:], IssuerId: bytes.Repeat([]byte{member}, 16), InstanceId: bytes.Repeat([]byte{member + 10}, 16), Group: "db-group", GenerationId: generation, MemberId: string([]byte{'m', member}), ExpectedPartitions: plan.KafkaPartitions}
	for _, partition := range assigned {
		found := false
		for _, planned := range plan.KafkaPartitions {
			if planned.Partition == partition {
				row := proto.Clone(planned).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure)
				row.FinalExclusiveEnd = 20
				row.FinalCommitted = 10
				report.AssignedPartitions = append(report.AssignedPartitions, row)
				found = true
			}
		}
		if !found {
			t.Fatal("fixture分区不在目录")
		}
	}
	if change != nil {
		change(report)
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{Groups: []*pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness{report}})
	if err != nil {
		t.Fatal(err)
	}
	pin := authorityTestPin(t, "consumer-ready.json", raw)
	return &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{IssuerId: report.IssuerId, InstanceId: report.InstanceId, ConsumerReadyFrontierDigest: pin.Sha256, SourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}}
}
func TestConsumerReadinessRequiresRealPartitionUnion(t *testing.T) {
	plan := authorityTestPlan(t)
	second := proto.Clone(plan.KafkaPartitions[0]).(*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure)
	second.Partition = 1
	plan.KafkaPartitions = append(plan.KafkaPartitions, second)
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	first := consumerReadinessFixture(t, plan, 'a', 7, []uint32{0}, nil)
	secondBoot := consumerReadinessFixture(t, plan, 'b', 7, []uint32{1}, nil)
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{first, secondBoot}, manifest); err != nil {
		t.Fatalf("合法双副本分摊被拒绝: %v", err)
	}
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{first}, manifest); err == nil {
		t.Fatal("实际分配遗漏却获准开放")
	}
	duplicate := consumerReadinessFixture(t, plan, 'b', 7, []uint32{0}, nil)
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{first, duplicate}, manifest); err == nil {
		t.Fatal("同分区重复分配被计作完整")
	}
	stale := consumerReadinessFixture(t, plan, 'b', 8, []uint32{1}, nil)
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{first, stale}, manifest); err == nil {
		t.Fatal("两代次被拼接成准备完成")
	}
}
func TestConsumerReadinessRejectsForeignIdentityAndDigest(t *testing.T) {
	plan := authorityTestPlan(t)
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	bad := consumerReadinessFixture(t, plan, 'a', 7, []uint32{0}, func(r *pb.SqlWriterMaintenanceReceipt_ConsumerGroupReadiness) {
		r.AssignedPartitions[0].TopicId = bytes.Repeat([]byte{99}, 16)
	})
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{bad}, manifest); err == nil {
		t.Fatal("重建同名topic被当原物理分区")
	}
	changed := consumerReadinessFixture(t, plan, 'a', 7, []uint32{0}, nil)
	changed.ConsumerReadyFrontierDigest = bytes.Repeat([]byte{2}, 32)
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{changed}, manifest); err == nil {
		t.Fatal("准备原件摘要未绑定SOURCE")
	}
}

func TestConsumerReadinessEveryBootRequiresUniqueFormalPin(t *testing.T) {
	plan := authorityTestPlan(t)
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	emptyRaw := []byte(`{"groups":[]}`)
	pin := authorityTestPin(t, "empty-consumer-ready.json", emptyRaw)
	boot := &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{ConsumerReadyFrontierDigest: pin.Sha256, SourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}}
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{boot}, manifest); err != nil {
		t.Fatalf("正式空组应可表达无consumer: %v", err)
	}
	missing := proto.Clone(boot).(*pb.SqlWriterMaintenanceReceipt_IssuerReadiness)
	missing.SourcePins = nil
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{missing}, manifest); err == nil {
		t.Fatal("无consumer原件被当作空组")
	}
	duplicate := proto.Clone(boot).(*pb.SqlWriterMaintenanceReceipt_IssuerReadiness)
	duplicate.SourcePins = append(duplicate.SourcePins, pin)
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{duplicate}, manifest); err == nil {
		t.Fatal("多个相同摘要原件被接受")
	}
	generic := authorityTestPin(t, "generic.json", []byte("{}"))
	boot.SourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{generic}
	boot.ConsumerReadyFrontierDigest = generic.Sha256
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{boot}, manifest); err == nil {
		t.Fatal("普通空JSON被冒认为consumer就绪")
	}
}
func TestConsumerReadinessAcceptsBinaryOriginalAcrossLanguages(t *testing.T) {
	plan := authorityTestPlan(t)
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	boot := consumerReadinessFixture(t, plan, 'a', 7, []uint32{0}, nil)
	raw, err := drainproof.ReadPin(boot.SourcePins[0], 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	report, err := drainproof.ParseConsumerReadiness(raw)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := (proto.MarshalOptions{Deterministic: true}).Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	pin := authorityTestPin(t, "consumer-ready.pb", binary)
	boot.SourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}
	boot.ConsumerReadyFrontierDigest = pin.Sha256
	if err := verifyConsumerReadiness(plan, []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{boot}, manifest); err != nil {
		t.Fatalf("同正式协议二进制原件被拒绝: %v", err)
	}
}

func TestManualReadinessUsesActualNewNodePartitionAndFixedPhysicalTopic(t *testing.T) {
	old := &pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{IssuerId: bytes.Repeat([]byte{1}, 16), InstanceId: bytes.Repeat([]byte{2}, 16), ConsumerId: "scene-cmd-9", ClusterId: []byte("cluster"), TopicId: bytes.Repeat([]byte{3}, 16), TopicName: "scene-cmd_g1", PartitionCount: 4, Partition: 1, TargetNodeId: 9, TargetInstanceId: bytes.Repeat([]byte{2}, 16), AssignmentRevision: 1, StartOffset: 0, Position: 10, Earliest: 0, ExclusiveEnd: 10, ObservedMs: 1}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: bytes.Repeat([]byte{4}, 16), ManualAssignments: []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{old}}
	boot := &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{IssuerId: old.IssuerId, InstanceId: bytes.Repeat([]byte{5}, 16)}
	row := proto.Clone(old).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	row.InstanceId = boot.InstanceId
	row.TargetInstanceId = boot.InstanceId
	row.TargetNodeId = 10
	row.Partition = 2
	row.ConsumerId = "scene-cmd-10"
	row.StartOffset = 20
	row.Position = 20
	row.ExclusiveEnd = 20
	row.ObservedMs = 2
	digest := bytes.Repeat([]byte{6}, 32)
	proof := &pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{OperationId: plan.RunId, ManifestDigest: digest, Assignments: []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{row}, AcceptingClosed: true, CallbacksJoined: true}
	if err := verifyManualReadiness(plan, boot, []*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{proof}, digest); err != nil {
		t.Fatalf("真实新node分区被旧ID限制误拒: %v", err)
	}
	backlog := proto.Clone(proof).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
	backlog.Assignments[0].ExclusiveEnd = 25
	if err := verifyManualReadiness(plan, boot, []*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{backlog}, digest); err != nil {
		t.Fatal("暂停后新增尾部不应导致重新seek或拒绝准备", err)
	}
	for _, change := range []func(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence){
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p.Assignments[0].ExclusiveEnd = 19 },
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p.Assignments[0].TopicId = bytes.Repeat([]byte{7}, 16)
		},
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p.Assignments[0].TargetInstanceId = old.InstanceId
		},
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p.Assignments[0].Partition = 1 },
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p.Assignments[0].StartOffset = 19 },
		func(p *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p.Accepted = 1; p.Settled = 1 },
	} {
		bad := proto.Clone(proof).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
		change(bad)
		if verifyManualReadiness(plan, boot, []*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{bad}, digest) == nil {
			t.Fatal("换物理topic、旧目标、错误分区或已消费准备获准")
		}
	}
	if verifyManualReadiness(plan, boot, nil, digest) == nil {
		t.Fatal("真实manual消费者被省略为空组")
	}
}

// 夹具只验证原件链协议，不代表运行过Kafka或业务排空。
func manualEvidenceFixture(t *testing.T, change func(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)) (*pb.SqlWriterMaintenanceReceipt_Plan, *kafkautil.DrainKafkaCatalog, *pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) {
	t.Helper()
	plan := authorityTestPlan(t)
	source := plan.RootIssuers[0]
	directory := filepath.Dir(source.SourceEvidence[0].AbsolutePath)
	write := func(name string, raw []byte) *pb.SqlWriterMaintenanceReceipt_SourcePin {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]}
	}
	encoded := func(value proto.Message) []byte {
		raw, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	jsonBytes := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	catalog := &kafkautil.DrainKafkaCatalog{}
	catalogRaw, err := drainproof.ReadPin(plan.SourcePins[0], 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(catalogRaw, catalog); err != nil {
		t.Fatal(err)
	}
	group := plan.KafkaPartitions[0]
	row := &pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{IssuerId: source.IssuerId, InstanceId: source.InstanceId, ConsumerId: "scene-command", ClusterId: group.ClusterId, TopicId: group.TopicId, TopicName: group.TopicName, PartitionCount: 1, Partition: 0, TargetNodeId: 1, TargetInstanceId: source.InstanceId, AssignmentRevision: 1, StartOffset: 0, Position: 5, Earliest: 0, ExclusiveEnd: 6, ObservedMs: 2}
	plan.ManualAssignments = []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{row}
	native := &pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{OperationId: plan.RunId, Assignments: []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{row}, SourcePins: source.SourceEvidence}
	nativePin := write("manual-baseline.report.json", encoded(native))
	leaderRow := proto.Clone(row).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	leaderRow.ObservedMs = 4
	manualCatalog := *catalog
	manualCatalog.ObservedStartMS = 3
	manualCatalog.ObservedEndMS = 4
	catalogPin := write("manual-baseline.catalog.json", jsonBytes(&manualCatalog))
	baseline := &kafkautil.DrainManualKafkaBaseline{Schema: "SQL_DRAIN_MANUAL_KAFKA_BASELINE_V1", CatalogSource: catalogPin, CatalogSHA256: catalogPin.Sha256, ObservedStartMS: 3, ObservedEndMS: 4, Assignments: []json.RawMessage{encoded(leaderRow)}}
	plan.SourcePins = append(plan.SourcePins, nativePin, write("manual-baseline.leader.json", jsonBytes(baseline)))
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	closedRow := proto.Clone(row).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
	closedRow.Position = 10
	closedRow.ExclusiveEnd = 10
	closedRow.ObservedMs = 8
	closed := &pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{OperationId: plan.RunId, ManifestDigest: manifestDigest[:], Assignments: []*pb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{closedRow}, AcceptingClosed: true, CallbacksJoined: true, Accepted: 2, Settled: 2, SourcePins: source.SourceEvidence}
	sampled := proto.Clone(closed).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
	sampled.Assignments[0].ObservedMs = 10
	if change != nil {
		change(closed, sampled)
	}
	p3Pin := write("manual-closure.report.json", encoded(closed))
	sampled.SourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{p3Pin}
	firstCatalog := *catalog
	firstCatalog.ObservedStartMS = 9
	firstCatalog.ObservedEndMS = 9
	lastCatalog := *catalog
	lastCatalog.ObservedStartMS = 10
	lastCatalog.ObservedEndMS = 10
	firstPin := write("manual-first.catalog.json", jsonBytes(&firstCatalog))
	lastPin := write("manual-last.catalog.json", jsonBytes(&lastCatalog))
	leader := &authorityManualLeader{Schema: "SQL_DRAIN_MANUAL_KAFKA_LEADER_V1", ObservedStartMS: 9, ObservedEndMS: 10, CatalogFirstSHA256: firstPin.Sha256, CatalogLastSHA256: lastPin.Sha256, Consumers: []json.RawMessage{encoded(sampled)}}
	leaderPin := write("manual-leader.json", jsonBytes(leader))
	published := proto.Clone(sampled).(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
	published.SourcePins = append(published.SourcePins, leaderPin)
	closure := &pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence{ObservedStartMs: 9, ObservedEndMs: 10, ManualConsumers: []*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{published}, SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{firstPin, lastPin, leaderPin}}
	return plan, catalog, closure
}
func TestManualBaselineRequiresNativeOwnerAndIndependentLeaderOriginal(t *testing.T) {
	plan, catalog, _ := manualEvidenceFixture(t, nil)
	if err := verifyManualBaseline(plan, catalog); err != nil {
		t.Fatal("完整手动原件链被拒绝", err)
	}
	if _, err := authorityKafkaBaseline(plan); err != nil {
		t.Fatal("手动与group目录不能共存", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.SqlWriterMaintenanceReceipt_Plan)
	}{
		{"缺leader原件", func(p *pb.SqlWriterMaintenanceReceipt_Plan) { p.SourcePins = p.SourcePins[:len(p.SourcePins)-1] }},
		{"冒写原生position", func(p *pb.SqlWriterMaintenanceReceipt_Plan) { p.ManualAssignments[0].Position++ }},
		{"换旧boot", func(p *pb.SqlWriterMaintenanceReceipt_Plan) {
			p.ManualAssignments[0].InstanceId = bytes.Repeat([]byte{99}, 16)
		}},
		{"重复配置", func(p *pb.SqlWriterMaintenanceReceipt_Plan) {
			p.ManualAssignments = append(p.ManualAssignments, p.ManualAssignments[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := proto.Clone(plan).(*pb.SqlWriterMaintenanceReceipt_Plan)
			tc.change(bad)
			if verifyManualBaseline(bad, catalog) == nil {
				t.Fatal("无效P0原件链被接受")
			}
		})
	}
}
func TestManualClosureRequiresJoinedOwnerAndUnchangedTail(t *testing.T) {
	plan, _, closure := manualEvidenceFixture(t, nil)
	if err := verifyManualClosure(plan, closure); err != nil {
		t.Fatal("完整P3/P4原件链被拒绝", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
	}{
		{"未join", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p3.CallbacksJoined = false }},
		{"UNKNOWN", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p3.UnresolvedPublications = 1
			p4.UnresolvedPublications = 1
		}},
		{"leader尾部增长", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p4.Assignments[0].ExclusiveEnd++ }},
		{"retention越过P0", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p4.Assignments[0].Earliest = 6 }},
		{"旧owner换代", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p3.Assignments[0].AssignmentRevision++
			p4.Assignments[0].AssignmentRevision++
		}},
		{"采样改原生位置", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p4.Assignments[0].Position++
			p4.Assignments[0].ExclusiveEnd++
		}},
		{"另一run", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) {
			p3.OperationId = bytes.Repeat([]byte{99}, 16)
			p4.OperationId = p3.OperationId
		}},
		{"P3遗漏固定pin", func(p3, p4 *pb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence) { p3.SourcePins = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, c := manualEvidenceFixture(t, tc.change)
			if verifyManualClosure(p, c) == nil {
				t.Fatal("无效P3/P4原件链被接受")
			}
		})
	}
	missing := proto.Clone(closure).(*pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence)
	missing.SourceEvidence = missing.SourceEvidence[1:]
	if verifyManualClosure(plan, missing) == nil {
		t.Fatal("缺首目录原件被接受")
	}
}

// handoffClosureFixture 给 authorityTestPlan 的唯一 Redis 目标配一份 P4 原件:SCAN 列出 keys,观察由 observations 给出。
func handoffClosureFixture(t *testing.T, plan *pb.SqlWriterMaintenanceReceipt_Plan, pattern string, keys []string, observations ...*pb.SqlWriterMaintenanceReceipt_RedisKeyClosure) *pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence {
	t.Helper()
	target := proto.Clone(plan.RedisTargets[0]).(*pb.SqlWriterMaintenanceReceipt_RedisTargetClosure)
	nonceKey, err := drainproof.RedisMaintenanceNonceKey(plan.RunId)
	if err != nil {
		t.Fatal(err)
	}
	scan := authorityRedisScan{Schema: "SQL_DRAIN_REDIS_SCAN_V2", Role: target.Role, Address: target.Address, DB: target.Db, RunID: target.ServerRunId, ObservedStartMS: 3, ObservedEndMS: 4, NonceKey: nonceKey, NonceBefore: target.MaintenanceNonce, NonceAfter: target.MaintenanceNonce, NoncePTTLBeforeMS: -1, NoncePTTLAfterMS: -1, Pages: []authorityRedisScanPage{{Pattern: pattern, Keys: keys}}}
	raw, err := json.Marshal(scan)
	if err != nil {
		t.Fatal(err)
	}
	target.ScanSourceEvidence = []*pb.SqlWriterMaintenanceReceipt_SourcePin{authorityTestPin(t, "scan.json", raw)}
	target.Observations = observations
	return &pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence{RedisTargets: []*pb.SqlWriterMaintenanceReceipt_RedisTargetClosure{target}, ObservedStartMs: 3, ObservedEndMs: 4}
}

func handoffObservation(key string, fields ...string) *pb.SqlWriterMaintenanceReceipt_RedisKeyClosure {
	dump := sha256.Sum256([]byte(key))
	o := &pb.SqlWriterMaintenanceReceipt_RedisKeyClosure{Key: key, Kind: "handoff_fence", RedisType: "hash", PttlMs: -1, Count: uint64(len(fields)), DumpSha256: dump[:]}
	for _, field := range fields {
		o.Members = append(o.Members, []byte(field))
		o.Values = append(o.Values, []byte("value"))
	}
	return o
}

// P4 只认交接资格 hash;SCAN 目录和观察必须等集,hash 里不能残留旧存盘字段。
func TestAuthorityRedisClosureOnlyAcceptsHandoffFenceKeys(t *testing.T) {
	plan := authorityTestPlan(t)
	key := handofffence.Key(42)
	good := handoffClosureFixture(t, plan, "player:{*}:handoff_fence", []string{key}, handoffObservation(key, handofffence.HandoffFenceField))
	if err := verifyAuthorityRedis(plan, good); err != nil {
		t.Fatal(err)
	}
	empty := handoffClosureFixture(t, plan, "player:{*}:handoff_fence", nil)
	if err := verifyAuthorityRedis(plan, empty); err != nil {
		t.Fatalf("没有交接资格键的目标也是完整闭合: %v", err)
	}
	for _, tc := range []struct {
		name    string
		closure *pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence
	}{
		{"退役的retry键族", handoffClosureFixture(t, plan, "kafka:retry:queue:*", []string{"kafka:retry:queue:task"}, handoffObservation("kafka:retry:queue:task"))},
		{"退役的pending键族", handoffClosureFixture(t, plan, "player:save_pending:*", []string{"player:save_pending:1:0"}, handoffObservation("player:save_pending:1:0"))},
		{"SCAN列出却没观察", handoffClosureFixture(t, plan, "player:{*}:handoff_fence", []string{key})},
		{"观察了SCAN没列出的键", handoffClosureFixture(t, plan, "player:{*}:handoff_fence", nil, handoffObservation(key))},
		{"残留旧存盘字段", handoffClosureFixture(t, plan, "player:{*}:handoff_fence", []string{key}, handoffObservation(key, "outbox_db_task"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyAuthorityRedis(plan, tc.closure) == nil {
				t.Fatal("无效P4原件被接受")
			}
		})
	}
}

func TestReleaseRedisObservationRejectsBadHandoffFenceFacts(t *testing.T) {
	key := handofffence.Key(42)
	keySum := sha256.Sum256([]byte(key))
	dumpSum := sha256.Sum256([]byte("dump"))
	emptySum := sha256.Sum256(nil)
	good := maintenanceReleaseRedisKeyObservation{Key: key, KeySHA: hex.EncodeToString(keySum[:]), Kind: "handoff_fence", Type: "hash", DumpSHA: hex.EncodeToString(dumpSum[:]), PTTL: -1, Count: 1, Members: []string{handofffence.HandoffFenceField}, Values: []string{"value"}}
	if err := releaseRedisObservation(good); err != nil {
		t.Fatal(err)
	}
	missing := maintenanceReleaseRedisKeyObservation{Key: key, KeySHA: good.KeySHA, Kind: "handoff_fence", Type: "none", DumpSHA: hex.EncodeToString(emptySum[:]), PTTL: -2}
	if err := releaseRedisObservation(missing); err != nil {
		t.Fatalf("取样时键已不在也是合法事实: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*maintenanceReleaseRedisKeyObservation)
	}{
		{"键摘要不符", func(o *maintenanceReleaseRedisKeyObservation) { o.KeySHA = hex.EncodeToString(dumpSum[:]) }},
		{"玩家号不规范", func(o *maintenanceReleaseRedisKeyObservation) {
			o.Key = "player:{042}:handoff_fence"
			sum := sha256.Sum256([]byte(o.Key))
			o.KeySHA = hex.EncodeToString(sum[:])
		}},
		{"角色不是交接资格", func(o *maintenanceReleaseRedisKeyObservation) { o.Kind = "retry" }},
		{"带topic", func(o *maintenanceReleaseRedisKeyObservation) { o.Topic = "task" }},
		{"类型不是hash", func(o *maintenanceReleaseRedisKeyObservation) { o.Type = "list" }},
		{"有TTL", func(o *maintenanceReleaseRedisKeyObservation) { o.PTTL = 1000 }},
		{"字段数不符", func(o *maintenanceReleaseRedisKeyObservation) { o.Count = 2 }},
		{"字段值缺失", func(o *maintenanceReleaseRedisKeyObservation) { o.Values = nil }},
		{"字段重复", func(o *maintenanceReleaseRedisKeyObservation) {
			o.Count, o.Members, o.Values = 2, []string{"a", "a"}, []string{"1", "2"}
		}},
		{"残留pending", func(o *maintenanceReleaseRedisKeyObservation) { o.Members = []string{"pending"} }},
		{"残留玩家行", func(o *maintenanceReleaseRedisKeyObservation) { o.Members = []string{"player_database"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			tc.change(&bad)
			if releaseRedisObservation(bad) == nil {
				t.Fatal("无效交接资格原件被接受")
			}
		})
	}
}

// 三个主节点的拓扑:扫完这三个才算扫完玩家域。
const clusterFixtureFingerprint = "aaa|0-5460;bbb|5461-10922;ccc|10923-16383"

func clusterFixtureViews(ids ...string) []*authorityRedisClusterEvidence {
	views := []*authorityRedisClusterEvidence{}
	for _, id := range ids {
		views = append(views, &authorityRedisClusterEvidence{NodeID: id, FingerprintBefore: clusterFixtureFingerprint, FingerprintAfter: clusterFixtureFingerprint})
	}
	return views
}

func TestAuthorityRedisClusterViewsRequireEveryMasterAndStableTopology(t *testing.T) {
	if err := verifyAuthorityRedisClusterViews(nil); err != nil {
		t.Fatalf("全是单实例时不该受这条规则影响: %v", err)
	}
	if err := verifyAuthorityRedisClusterViews(clusterFixtureViews("aaa", "bbb", "ccc")); err != nil {
		t.Fatal(err)
	}
	moved := clusterFixtureViews("aaa", "bbb", "ccc")
	moved[1].FingerprintAfter = "aaa|0-5460;bbb|5461-10000;ccc|10001-16383"
	otherView := clusterFixtureViews("aaa", "bbb", "ccc")
	otherView[2].FingerprintBefore = "aaa|0-5460;bbb|5461-10000;ccc|10001-16383"
	otherView[2].FingerprintAfter = otherView[2].FingerprintBefore
	stranger := clusterFixtureViews("aaa", "bbb", "ccc")
	stranger[2].NodeID = "ddd"
	partial := "aaa|0-5460;bbb|5461-10922"
	for _, tc := range []struct {
		name  string
		views []*authorityRedisClusterEvidence
	}{
		{"漏了一个主节点", clusterFixtureViews("aaa", "bbb")},
		{"同一主节点取样两次", clusterFixtureViews("aaa", "bbb", "ccc", "ccc")},
		{"取样期间拓扑变化", moved},
		{"目标之间拓扑不一致", otherView},
		{"目标不是持有slot的主节点", stranger},
		{"指纹没有覆盖全部slot", []*authorityRedisClusterEvidence{{NodeID: "aaa", FingerprintBefore: partial, FingerprintAfter: partial}, {NodeID: "bbb", FingerprintBefore: partial, FingerprintAfter: partial}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyAuthorityRedisClusterViews(tc.views) == nil {
				t.Fatal("不能断言扫完了玩家域,却被接受")
			}
		})
	}
}

func TestRedisNonceKeyMustLandOnTheMastersOwnSlots(t *testing.T) {
	base := "sql:writer:maintenance:" + strings.Repeat("a", 32) + ":nonce"
	if !redisNonceKeyMatches(&authorityRedisScan{NonceKey: base}, base) {
		t.Fatal("单实例就是 run 派生的那一把")
	}
	if redisNonceKeyMatches(&authorityRedisScan{NonceKey: base + ":{n0}"}, base) {
		t.Fatal("单实例不许带 tag")
	}
	view := &authorityRedisClusterEvidence{NodeID: "aaa", FingerprintBefore: clusterFixtureFingerprint, FingerprintAfter: clusterFixtureFingerprint}
	foundOwn, foundForeign := false, false
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("%s:{n%d}", base, i)
		slot := redisslot.HashSlot(key)
		matched := redisNonceKeyMatches(&authorityRedisScan{NonceKey: key, Cluster: view}, base)
		if matched != (slot <= 5460) {
			t.Fatalf("key=%s slot=%d matched=%v", key, slot, matched)
		}
		foundOwn = foundOwn || matched
		foundForeign = foundForeign || !matched
	}
	if !foundOwn || !foundForeign {
		t.Fatal("夹具前提:64 个候选里两种落点都要有")
	}
	if redisNonceKeyMatches(&authorityRedisScan{NonceKey: base, Cluster: view}, base) {
		t.Fatal("Cluster 目标不带 tag 的键落不到指定主节点")
	}
}

func TestRedisNonceEvidenceRejectsMissingBaselineAndReset(t *testing.T) {
	plan := authorityTestPlan(t)
	if err := verifyAuthorityRedisBaseline(plan); err != nil {
		t.Fatal(err)
	}
	missing := proto.Clone(plan).(*pb.SqlWriterMaintenanceReceipt_Plan)
	missing.SourcePins = missing.SourcePins[:len(missing.SourcePins)-1]
	if verifyAuthorityRedisBaseline(missing) == nil {
		t.Fatal("输入nonce被当作实际P0读回")
	}
	target := plan.RedisTargets[0]
	key, err := drainproof.RedisMaintenanceNonceKey(plan.RunId)
	if err != nil {
		t.Fatal(err)
	}
	base := authorityRedisScan{Schema: "SQL_DRAIN_REDIS_SCAN_V2", Role: target.Role, Address: target.Address, DB: target.Db, RunID: target.ServerRunId, ObservedStartMS: 3, ObservedEndMS: 4, NonceKey: key, NonceBefore: target.MaintenanceNonce, NonceAfter: target.MaintenanceNonce, NoncePTTLBeforeMS: -1, NoncePTTLAfterMS: -1}
	if err := verifyRedisNonceObservation(&base, target, plan.RunId); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*authorityRedisScan)
	}{
		{"缺开始nonce", func(p *authorityRedisScan) { p.NonceBefore = nil }},
		{"结束时已清库", func(p *authorityRedisScan) { p.NonceAfter = nil }},
		{"nonce换值", func(p *authorityRedisScan) { p.NonceAfter = bytes.Repeat([]byte{99}, 16) }},
		{"临时key", func(p *authorityRedisScan) { p.NoncePTTLBeforeMS = 1000 }},
		{"终点key已消失", func(p *authorityRedisScan) { p.NoncePTTLAfterMS = -2 }},
		{"跨run键", func(p *authorityRedisScan) { p.NonceKey = "sql:writer:maintenance:foreign:nonce" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := base
			tc.change(&bad)
			if verifyRedisNonceObservation(&bad, target, plan.RunId) == nil {
				t.Fatal("重置或未知nonce被接受")
			}
		})
	}
}
