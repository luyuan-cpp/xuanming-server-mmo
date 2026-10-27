package barrierrestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	pb "proto/common/database"
	"shared/drainproof"
	"shared/kafkautil"
	"shared/placement"
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

func TestMergeRecoveryAuditRequiresActualCompleteChecksAndFixedInputs(t *testing.T) {
	plan := authorityTestPlan(t)
	tool := proto.Clone(plan.RootIssuers[0]).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	tool.IssuerId = bytes.Repeat([]byte{21}, 16)
	tool.LogicalRole = "MAINTENANCE"
	tool.PhysicalWriterKey = nil
	plan.MaintenanceIssuers = []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{tool}
	closed := proto.Clone(tool).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	closed.AcceptingClosedMs = 11
	closed.AcceptedSettledMs = 12
	closed.AcceptedFrontierDigest = bytes.Repeat([]byte{22}, 32)
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}}
	frozen := &pb.SqlWriterMaintenanceReceipt{RequestId: bytes.Repeat([]byte{23}, 16), ReceiptDigest: bytes.Repeat([]byte{24}, 32), CommittedMs: 10}
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	fact := &pb.SqlWriterMaintenanceReceipt{RunId: plan.RunId, Generation: "fixture", AttemptRevision: 1, PlanProto: manifest, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: plan.RunId, ManifestDigest: digest[:], Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{MaintenancePublication: publication}}}}
	manifestPin := authorityTestPin(t, "merge-manifest.json", []byte(`{"run_id":"merge-1-2","source_zone":1,"target_zone":2,"route_source":"db"}`))
	audit := &pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit{AuditInstanceId: bytes.Repeat([]byte{25}, 16), RunId: plan.RunId, Generation: fact.Generation, AttemptRevision: 1, PlanDigest: digest[:], OriginalFreezeReceiptId: frozen.RequestId, OriginalFreezeReceiptDigest: frozen.ReceiptDigest, MaintenancePublication: publication, MergeManifest: manifestPin, MergeRunId: "merge-1-2", SourceZone: 1, TargetZone: 2, VerifyMerged: true, ObservedStartMs: 13, ObservedEndMs: 14, AuditSourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{plan.SourcePins[0]}}
	for _, name := range []string{"player.name (global)", "friend", "friend_request", "guild_member", "verify:mapping_src", "verify:manifest_mapping", "verify:guild_zone", "verify:guild_rank", "verify:trade_listing", "verify:manifest_rows", "verify:save_barrier", "verify:source_hot_state", "verify:merge_fence", "verify:route_cache"} {
		kind := pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION
		if name == "player.name (global)" {
			kind = pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_RULE
		}
		audit.Checks = append(audit.Checks, &pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck{Name: name, Kind: kind, Status: pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_COMPLETE, Severity: "info", UniqueScope: "global", QuerySourcePins: audit.AuditSourcePins})
	}
	if err := verifyMergeRecoveryAudit(plan, fact, frozen, audit); err != nil {
		t.Fatal("完整正式审计被拒绝", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit)
	}{
		{"未verify", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.VerifyMerged = false }},
		{"旧run", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.RunId = bytes.Repeat([]byte{26}, 16) }},
		{"换清单zone", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.TargetZone = 3 }},
		{"工具未完成先审计", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.ObservedStartMs = 11 }},
		{"漏检查", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.Checks = a.Checks[:len(a.Checks)-1] }},
		{"重复检查", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.Checks[1] = a.Checks[0] }},
		{"规则冒充查询", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) {
			a.Checks[0].Kind = pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION
		}},
		{"警告不可绿化", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.Checks[1].Severity = "warn" }},
		{"查询未完成", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) {
			a.Checks[1].Status = pb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_INCOMPLETE
		}},
		{"存在冲突", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.Checks[1].ConflictCount = 1 }},
		{"缺查询来源", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) { a.Checks[1].QuerySourcePins = nil }},
		{"未知来源空摘要", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) {
			a.Checks[1].QuerySourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: "unknown"}}
		}},
		{"新增未经P0固定来源", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) {
			a.AuditSourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{manifestPin}
		}},
		{"引用预览", func(a *pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) {
			a.MergeManifest = authorityTestPin(t, "dry-run.json", []byte(`{"run_id":"merge-1-2","source_zone":1,"target_zone":2,"dry_run":true,"route_source":"db"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := proto.Clone(audit).(*pb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit)
			tc.change(bad)
			if verifyMergeRecoveryAudit(plan, fact, frozen, bad) == nil {
				t.Fatal("无效合服审计被当作释放依据")
			}
		})
	}
}

func TestRouteRecoveryAuditPreservesFrozenAuditMeaningAndActualWatermark(t *testing.T) {
	plan := authorityTestPlan(t)
	tool := proto.Clone(plan.RootIssuers[0]).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	tool.IssuerId = bytes.Repeat([]byte{31}, 16)
	tool.LogicalRole = "MAINTENANCE"
	tool.PhysicalWriterKey = nil
	plan.MaintenanceIssuers = []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{tool}
	closed := proto.Clone(tool).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	closed.AcceptingClosedMs = 3
	closed.AcceptedSettledMs = 4
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}}
	frozen := &pb.SqlWriterMaintenanceReceipt{RequestId: bytes.Repeat([]byte{32}, 16), ReceiptDigest: bytes.Repeat([]byte{33}, 32), CommittedMs: 2}
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	fact := &pb.SqlWriterMaintenanceReceipt{RunId: plan.RunId, Generation: "abc", AttemptRevision: 1, PlanProto: manifest, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: plan.RunId, ManifestDigest: digest[:], Detail: &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{MaintenancePublication: publication, RouteWatermarkFloorMs: 15}}}}
	repl := string(bytes.Repeat([]byte{'a'}, 40))
	target := plan.RedisTargets[0]
	marker, err := placement.EncodeRouteReadyMarker(placement.RouteReadyMarker{Generation: "abc", RedisRunID: string(target.ServerRunId), MasterReplID: repl, SetAtMs: 1, WatermarkMs: 10, Leader: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	watermark, err := placement.EncodeRouteWatermark(placement.RouteWatermark{Generation: "abc", WatermarkMs: 15})
	if err != nil {
		t.Fatal(err)
	}
	audit := &pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{AuditInstanceId: bytes.Repeat([]byte{34}, 16), RunId: plan.RunId, Generation: fact.Generation, AttemptRevision: 1, PlanDigest: digest[:], OriginalFreezeReceiptId: frozen.RequestId, OriginalFreezeReceiptDigest: frozen.ReceiptDigest, MaintenancePublication: publication, AuthoritySchema: plan.AuthoritySchema, MappingTarget: target, ObservedStartMs: 5, ObservedEndMs: 6, Stats: &pb.SqlWriterMaintenanceReceipt_RouteRecoveryStats{DbRows: 4, DbOnlyUnpublished: 1, FrozenRecovery: true, Completed: true}, ReadyMarkerRaw: []byte(marker), WatermarkRaw: []byte(watermark), InfoServerRaw: []byte("run_id:" + string(target.ServerRunId) + "\r\n"), InfoReplicationRaw: []byte("role:master\r\nmaster_replid:" + repl + "\r\n"), RouteWatermarkFloorMs: 15, AuditSourcePins: []*pb.SqlWriterMaintenanceReceipt_SourcePin{plan.SourcePins[0]}}
	if err := verifyRouteRecoveryAudit(plan, fact, frozen, audit); err != nil {
		t.Fatal("原冻结审核允许未发布SQL行且保实际水位", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit)
	}{
		{"未完成", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.Stats.Completed = false }},
		{"非冻结口径", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.Stats.FrozenRecovery = false }},
		{"真实差异", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.Stats.MissingInDb = 1 }},
		{"活跃合服围栏", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.Stats.RedisFencesActive = 1 }},
		{"审核中变化", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.Stats.TransientCleared = 1 }},
		{"伪完整缺marker", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.ReadyMarkerRaw = nil }},
		{"另一Redis", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) {
			a.InfoServerRaw = []byte("run_id:" + string(bytes.Repeat([]byte{'b'}, 40)) + "\r\n")
		}},
		{"错误水位", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.RouteWatermarkFloorMs = 10 }},
		{"损坏水位原件", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.WatermarkRaw = []byte("{}") }},
		{"换authority", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.AuthoritySchema = "foreign" }},
		{"工具尚未完成", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) { a.ObservedStartMs = 3 }},
		{"未知来源空摘要", func(a *pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) {
			a.AuditSourcePins = []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: "unknown"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := proto.Clone(audit).(*pb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit)
			tc.change(bad)
			if verifyRouteRecoveryAudit(plan, fact, frozen, bad) == nil {
				t.Fatal("无效路由审核被接受")
			}
		})
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
