package main

// 纯函数单测。Redis / MySQL 相关的行为在 integration_test.go(build tag
// merge_integration)里对着真实的 127.0.0.1 实例跑。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/drainproof"
)

func TestMergeRecoveryAuditRequiresCompleteActualToolFiles(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "actual-source")
	raw := []byte("真实读取的审核测试源")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{RunId: bytes.Repeat([]byte{1}, 16)}
	publication := &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{}
	for i := byte(2); i < 4; i++ {
		seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: bytes.Repeat([]byte{i}, 16), InstanceId: bytes.Repeat([]byte{i + 2}, 16),
			LogicalRole: "MAINTENANCE", PermanentRevivalFenceId: bytes.Repeat([]byte{i + 4}, 16), SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{{AbsolutePath: source, Sha256: sum[:]}}}
		plan.MaintenanceIssuers = append(plan.MaintenanceIssuers, seed)
		session := drainproof.New()
		finish, err := session.Enter()
		if err != nil {
			t.Fatal(err)
		}
		finish()
		session.CloseAdmission()
		closed, err := session.Closure(context.Background(), seed)
		if err != nil {
			t.Fatal(err)
		}
		publication.LogicalRoles = append(publication.LogicalRoles, closed)
		raw, err := protojson.Marshal(closed)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("tool-%d.json", i))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		publication.SourceEvidence = append(publication.SourceEvidence, &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]})
	}
	inventory, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: plan.MaintenanceIssuers})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(inventory)
	publication.IssuerInventoryDigest = digest[:]
	planRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	frozen := &pb.SqlWriterMaintenanceReceipt{RunId: plan.RunId, PlanProto: planRaw, Kind: pb.SqlWriterMaintenanceReceipt_FREEZE, Phase: pb.SqlWriterMaintenanceReceipt_FROZEN, RunPhase: pb.SqlWriterDrainRun_SQL_FROZEN, CommittedMs: 1}
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence){
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.LogicalRoles = p.LogicalRoles[:1] },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[1].InstanceId = bytes.Repeat([]byte{9}, 16)
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) {
			p.LogicalRoles[1].UnresolvedPublications = 1
		},
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.SourceEvidence = p.SourceEvidence[:1] },
		func(p *pb.SqlWriterMaintenanceReceipt_PublicationEvidence) { p.SourceEvidence[1] = p.SourceEvidence[0] },
	} {
		bad := proto.Clone(publication).(*pb.SqlWriterMaintenanceReceipt_PublicationEvidence)
		mutate(bad)
		if err := verifyClosedMaintenanceForAudit(plan, frozen, bad); err == nil {
			t.Fatal("全集/boot/UNKNOWN/实际文件缺口不得开始业务审核")
		}
	}
	if err := os.WriteFile(source, []byte("源漂移"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyClosedMaintenanceForAudit(plan, frozen, publication); err == nil {
		t.Fatal("实际源pin漂移必须拒绝")
	}
}

func TestMergeRecoveryAuditRejectsUnknownSkippedAndMissingRows(t *testing.T) {
	rows := []ResourceAudit{}
	for _, name := range mergeRecoveryRowNames() {
		rows = append(rows, ResourceAudit{Name: name, Severity: "info", UniqueScope: "global"})
	}
	if err := validateMergeRecoveryRows(rows); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]ResourceAudit) []ResourceAudit{
		func(r []ResourceAudit) []ResourceAudit { return r[:len(r)-1] },
		func(r []ResourceAudit) []ResourceAudit { r[1] = r[0]; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Severity = "warn"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "SKIPPED"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "UNKNOWN"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].Notes = "NOT VERIFIED"; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].SourceCount = -1; return r },
		func(r []ResourceAudit) []ResourceAudit { r[1].ConflictCount = 1; return r },
	} {
		bad := mutate(append([]ResourceAudit(nil), rows...))
		if err := validateMergeRecoveryRows(bad); err == nil {
			t.Fatal("不完整审核不能解冻")
		}
	}
}

func TestMaintenanceWriteNeedsFixedToolOwner(t *testing.T) {
	if err := checkMaintenanceTool(context.Background()); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("NORMAL冻结或空上下文不能冒TOOL: %v", err)
	}
	ctx, owner := testMaintenanceContext()
	if err := checkMaintenanceTool(ctx); err != nil {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	calls := 0
	owner.check = func(context.Context) error { calls++; return nil }
	if err := checkMaintenanceTool(ctx); !errors.Is(err, drainproof.ErrClosed) || calls != 0 {
		t.Fatalf("永久闭合后必须在实际读取/写入前拒绝: %v calls=%d", err, calls)
	}
}

func TestMaintenanceStrongReadFailurePermanentlyHolds(t *testing.T) {
	ctx, owner := testMaintenanceContext()
	readErr := errors.New("SQL_FROZEN强读未知")
	owner.check = func(context.Context) error { return readErr }
	if err := checkMaintenanceTool(ctx); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	owner.check = func(context.Context) error { return nil }
	if err := checkMaintenanceTool(ctx); err != nil {
		t.Fatal(err)
	}
	owner.session.CloseAdmission()
	if err := owner.session.Wait(ctx); !errors.Is(err, drainproof.ErrHold) {
		t.Fatalf("后续强读成功不能洗掉先前未知: %v", err)
	}
}

// ── Kafka lag 解析 ────────────────────────────────────────────

const sampleConsumerGroupOutput = `
GROUP                  TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID
db_rpc_consumer_group  db_task_zone_901  0          120             120             0    c-1          /1.2.3.4  x
db_rpc_consumer_group  db_task_zone_901  1          77              77              0    c-1          /1.2.3.4  x
db_rpc_consumer_group  db_task_zone_902  0          10              15              5    c-2          /1.2.3.5  y
`

func TestOperatorAttestationNeverProvidesDrainEvidence(t *testing.T) {
	for _, o := range []options{{assumeKafkaDrained: true}, {assumeKafkaDrained: true, kafkaCLI: "fixture-cli"}} {
		source := buildLagSource(o)
		if _, _, err := source.TotalLag(context.Background(), "fixture", []string{"fixture"}); !errors.Is(err, errNormalFreezeProof) {
			t.Fatalf("人工声明必须拒绝且不能触发CLI: %v", err)
		}
	}
	if err := runPreflight(context.Background(), preflightDeps{lag: fakeLagSource{}}, preflightParams{}); !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("lag=0不得绕过正式冻结原件: %v", err)
	}
}

func TestParseConsumerGroupLag_SumsOnlyRequestedTopic(t *testing.T) {
	lag, err := parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_901")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 0 {
		t.Fatalf("lag=%d want 0", lag)
	}
	lag, err = parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_902")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 5 {
		t.Fatalf("lag=%d want 5", lag)
	}
}

func TestParseConsumerGroupLag_UnknownTopicIsAnError(t *testing.T) {
	// 「这个 group 没消费过这个 topic」绝不能被当成 lag=0 放行。
	if _, err := parseConsumerGroupLag(sampleConsumerGroupOutput, "db_task_zone_999"); err == nil {
		t.Fatal("expected an error for a topic the group never consumed")
	}
}

func TestParseConsumerGroupLag_DashLagIsAnError(t *testing.T) {
	out := `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
g db_task_zone_901 0 - 120 -`
	if _, err := parseConsumerGroupLag(out, "db_task_zone_901"); err == nil {
		t.Fatal("expected an error when LAG is '-' (no committed offset)")
	}
}

func TestParseConsumerGroupLag_ColumnOrderIsReadFromHeader(t *testing.T) {
	// 列顺序被打乱(不同 Kafka 版本会插列):必须按表头名定位,不按下标。
	out := `LAG TOPIC GROUP PARTITION
3 db_task_zone_901 g 0
4 db_task_zone_901 g 1`
	lag, err := parseConsumerGroupLag(out, "db_task_zone_901")
	if err != nil {
		t.Fatal(err)
	}
	if lag != 7 {
		t.Fatalf("lag=%d want 7", lag)
	}
}

func TestDbTaskTopicAndQueueKeys(t *testing.T) {
	if got := dbTaskTopic(901, 1); got != "db_task_zone_901" {
		t.Fatalf("gen1 topic=%q", got)
	}
	if got := dbTaskTopic(901, 3); got != "db_task_zone_901_g3" {
		t.Fatalf("gen3 topic=%q", got)
	}
	if got := dbRetryQueueKey("db_task_zone_901"); got != "kafka:retry:queue:db_task_zone_901" {
		t.Fatalf("retry key=%q", got)
	}
	if got := dbRetryProcessingKey("t"); got != "kafka:retry:processing:t" {
		t.Fatalf("processing key=%q", got)
	}
	if got := dbDeadQueueKey("t"); got != "kafka:dead:queue:t" {
		t.Fatalf("dead key=%q", got)
	}
	// go/db 多实例之后的键(镜像 go/db/internal/kafka/retry_ownership.go)。
	if got := dbRetryInstancesKey("t"); got != "kafka:retry:instances:t" {
		t.Fatalf("instances key=%q", got)
	}
	if got := dbRetryInstanceProcessingKey("t", "db-0:6000"); got != "kafka:retry:processing:t:db-0:6000" {
		t.Fatalf("per-instance processing key=%q", got)
	}
}

// P4 / 审计必须把每个 go/db 实例各自的 processing 列表都查到:只查旧的共享键,多实例下门禁恒为 0。
func TestDbTaskQueueKeysCoverEveryInstance(t *testing.T) {
	got := dbTaskQueueKeysFor("t", []string{"db-0:6000", "db-1:6000"}, nil)
	want := []string{
		"kafka:retry:queue:t",
		"kafka:retry:processing:t",
		"kafka:dead:queue:t",
		"kafka:retry:processing:t:db-0:6000",
		"kafka:retry:processing:t:db-1:6000",
	}
	if len(got) != len(want) {
		t.Fatalf("queue keys=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue keys[%d]=%q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	if legacyOnly := dbTaskQueueKeysFor("t", nil, nil); len(legacyOnly) != 3 {
		t.Fatalf("with no registered instance the three shared keys must still be checked, got %v", legacyOnly)
	}
}

// S1 ④:按原始分区拆开的 ready 列表 kafka:retry:queue:{topic}:p{N} 也必须为空;它们排序去重后追加在最后。
func TestDbTaskQueueKeysIncludePartitionedReadyLists(t *testing.T) {
	scanned := []string{
		"kafka:retry:queue:t:p3",
		"kafka:retry:queue:t:p0",
		"kafka:retry:queue:t:p3", // SCAN 跨页可能重复返回同一键
	}
	got := dbTaskQueueKeysFor("t", []string{"db-0:6000"}, scanned)
	want := []string{
		"kafka:retry:queue:t",
		"kafka:retry:processing:t",
		"kafka:dead:queue:t",
		"kafka:retry:processing:t:db-0:6000",
		"kafka:retry:queue:t:p0",
		"kafka:retry:queue:t:p3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queue keys=%v, want %v", got, want)
	}
	// 纯函数不改调用方的切片(排序在副本上做)。
	if scanned[0] != "kafka:retry:queue:t:p3" || scanned[1] != "kafka:retry:queue:t:p0" {
		t.Fatalf("dbTaskQueueKeysFor mutated the caller's slice: %v", scanned)
	}
}

func TestDbRetryPartitionQueuePattern(t *testing.T) {
	const topic = "db_task_zone_1"
	if got := dbRetryPartitionQueuePrefix(topic); got != "kafka:retry:queue:db_task_zone_1:p" {
		t.Fatalf("prefix=%q", got)
	}
	pattern := dbRetryPartitionQueuePattern(topic)
	if pattern != "kafka:retry:queue:db_task_zone_1:p*" {
		t.Fatalf("pattern=%q", pattern)
	}
	// path.Match 与 Redis glob 在不含 '/' 的键上口径一致(* ? [] 与反斜杠转义),用它核对 MATCH 的边界。
	mustMatch := func(key string, want bool) {
		t.Helper()
		ok, err := path.Match(pattern, key)
		if err != nil {
			t.Fatalf("path.Match(%q, %q): %v", pattern, key, err)
		}
		if ok != want {
			t.Fatalf("pattern %q matching %q = %v, want %v", pattern, key, ok, want)
		}
	}
	mustMatch(dbRetryPartitionQueuePrefix(topic)+"0", true)
	mustMatch(dbRetryPartitionQueuePrefix(topic)+"9", true)
	// 别的 zone(zone_11)、别的世代(_g2)、共享收件箱本身都不能被扫进来。
	mustMatch(dbRetryPartitionQueuePrefix("db_task_zone_11")+"0", false)
	mustMatch(dbRetryPartitionQueuePrefix("db_task_zone_1_g2")+"0", false)
	mustMatch(dbRetryQueueKey(topic), false)
	if strings.HasPrefix(dbRetryPartitionQueuePrefix("db_task_zone_11"), dbRetryPartitionQueuePrefix(topic)) ||
		strings.HasPrefix(dbRetryPartitionQueuePrefix("db_task_zone_1_g2"), dbRetryPartitionQueuePrefix(topic)) {
		t.Fatal("zone_1's partitioned prefix must not be a prefix of zone_11 / zone_1_g2 keys")
	}

	// glob 元字符原样转义:* ? [ ] \。
	if got := escapeRedisGlob(`a*b?c[d]e\f`); got != `a\*b\?c\[d\]e\\f` {
		t.Fatalf("escapeRedisGlob=%q", got)
	}
	// topic 名里带元字符时,模式只匹配字面量本身,不会扩大到别的键。
	odd := dbRetryPartitionQueuePattern("t*")
	if ok, _ := path.Match(odd, "kafka:retry:queue:t*:p0"); !ok {
		t.Fatalf("escaped pattern %q must match the literal topic's key", odd)
	}
	if ok, _ := path.Match(odd, "kafka:retry:queue:tX:p0"); ok {
		t.Fatalf("escaped pattern %q must not match another topic's key", odd)
	}
}

// queueKeyReaderStub 回放 ZRANGE / SCAN 的回复(merge_zone 不引 miniredis,见 gap_fixes_test.go 头注释)。
type queueKeyReaderStub struct {
	members   []string
	zrangeErr error
	// pages[cursor] 是该游标的一页回复;scanErr 非 nil 时第 failAtCall 次(从 1 起)SCAN 返回它。
	pages      map[uint64]queueKeyScanPage
	scanErr    error
	failAtCall int
	scanCalls  []queueKeyScanCall
}

type queueKeyScanPage struct {
	keys []string
	next uint64
}

type queueKeyScanCall struct {
	cursor uint64
	match  string
	count  int64
}

func (s *queueKeyReaderStub) ZRange(_ context.Context, _ string, _, _ int64) *redis.StringSliceCmd {
	return redis.NewStringSliceResult(s.members, s.zrangeErr)
}

func (s *queueKeyReaderStub) Scan(_ context.Context, cursor uint64, match string, count int64) *redis.ScanCmd {
	s.scanCalls = append(s.scanCalls, queueKeyScanCall{cursor: cursor, match: match, count: count})
	if s.scanErr != nil && len(s.scanCalls) == s.failAtCall {
		return redis.NewScanCmdResult(nil, 0, s.scanErr)
	}
	page, ok := s.pages[cursor]
	if !ok {
		return redis.NewScanCmdResult(nil, 0, fmt.Errorf("stub: unexpected cursor %d", cursor))
	}
	return redis.NewScanCmdResult(page.keys, page.next, nil)
}

func TestDbTaskQueueKeysScanPagesAggregated(t *testing.T) {
	const topic = "db_task_zone_901"
	p := dbRetryPartitionQueuePrefix(topic)
	stub := &queueKeyReaderStub{
		members: []string{"db-0:6000"},
		pages: map[uint64]queueKeyScanPage{
			0:  {keys: []string{p + "3", p + "1"}, next: 17},
			17: {keys: []string{p + "1", p + "0"}, next: 0}, // 跨页重复的 p1 去重
		},
	}
	got, err := dbTaskQueueKeys(context.Background(), stub, topic)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		dbRetryQueueKey(topic),
		dbRetryProcessingKey(topic),
		dbDeadQueueKey(topic),
		dbRetryInstanceProcessingKey(topic, "db-0:6000"),
		p + "0", p + "1", p + "3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queue keys=%v, want %v", got, want)
	}
	if len(stub.scanCalls) != 2 {
		t.Fatalf("SCAN must iterate until the cursor returns to 0, calls=%v", stub.scanCalls)
	}
	for i, c := range stub.scanCalls {
		if c.match != dbRetryPartitionQueuePattern(topic) || c.count != dbRetryPartitionScanCount {
			t.Fatalf("scan call %d = %+v, want match=%q count=%d", i, c, dbRetryPartitionQueuePattern(topic), dbRetryPartitionScanCount)
		}
	}
	if stub.scanCalls[1].cursor != 17 {
		t.Fatalf("second SCAN must continue from cursor 17, got %d", stub.scanCalls[1].cursor)
	}
}

// SCAN 任一页失败、或登记表读不到,都必须报错 —— 「没扫完」不能当成「没有 p 列表」放行。
func TestDbTaskQueueKeysScanFailureFailsClosed(t *testing.T) {
	const topic = "db_task_zone_901"
	p := dbRetryPartitionQueuePrefix(topic)
	pages := map[uint64]queueKeyScanPage{
		0: {keys: []string{p + "0"}, next: 5},
		5: {keys: []string{p + "1"}, next: 0},
	}
	for _, failAt := range []int{1, 2} {
		stub := &queueKeyReaderStub{pages: pages, scanErr: errors.New("connection reset"), failAtCall: failAt}
		keys, err := dbTaskQueueKeys(context.Background(), stub, topic)
		if err == nil {
			t.Fatalf("SCAN failure on call %d must fail closed, got keys %v", failAt, keys)
		}
		if keys != nil {
			t.Fatalf("SCAN failure on call %d must not return a partial key list, got %v", failAt, keys)
		}
	}
	stub := &queueKeyReaderStub{zrangeErr: errors.New("READONLY"), pages: pages}
	if _, err := dbTaskQueueKeys(context.Background(), stub, topic); err == nil {
		t.Fatal("a failed ZRANGE of the instance registry must fail closed")
	}
}

// ── P3 唯一权威组(S2 ③,go/db 消费组按 zone 拆分)──────────────────

// 两个候选组的 describe 输出样例。列与 Kafka 3.x/4.x 的 --describe 一致;CLI 原文由 Codex 在钉版 4.3.1 上实测后补 fixture。
const (
	zoneGroupDescribe = `
GROUP                       TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST      CLIENT-ID
db_rpc_consumer_group_z901  db_task_zone_901  0          120             123             3    c-1          /1.2.3.4  db-z901-a
db_rpc_consumer_group_z901  db_task_zone_901  1          77              77              0    c-1          /1.2.3.4  db-z901-a
`
	// legacy 组仍被别的 zone 使用(Stable),但本 topic 已收尾:只有别的 topic 的行。
	legacyGroupOtherTopicOnly = `
GROUP                  TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST      CLIENT-ID
db_rpc_consumer_group  db_task_zone_902  0          10              15              5    c-2          /1.2.3.5  db-z902-b
`
	// legacy 组在本 topic 上还留着冻结的位点,而且比 zone 组更高(没收尾就切回来又切走的典型形状)。
	legacyGroupHigherStale = `
Consumer group 'db_rpc_consumer_group' has no active members.

GROUP                  TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID
db_rpc_consumer_group  db_task_zone_901  0          123             123             0    -            -     -
db_rpc_consumer_group  db_task_zone_901  1          77              77              0    -            -     -
`
	groupMissingKafka3 = "Error: Consumer group 'db_rpc_consumer_group' does not exist.\n"
)

func TestResolveAuthoritativeLag_PicksOnlyGroupWithOffsets(t *testing.T) {
	const topic = "db_task_zone_901"
	cases := []struct {
		name   string
		legacy groupDescribeResult
	}{
		{"legacy group does not exist", groupDescribeResult{group: defaultKafkaGroup, output: groupMissingKafka3}},
		// 有的版本对不存在的组以非 0 退出:命中白名单就不算 CLI 失败。
		{"legacy missing with non-zero exit", groupDescribeResult{group: defaultKafkaGroup,
			output: "Error: Executing consumer group command failed due to org.apache.kafka.common.errors.GroupIdNotFoundException: " +
				"The group id does not exist.\n", runErr: errors.New("exit status 1")}},
		{"legacy holds only other topics", groupDescribeResult{group: defaultKafkaGroup, output: legacyGroupOtherTopicOnly}},
		{"legacy empty without header", groupDescribeResult{group: defaultKafkaGroup,
			output: "\nConsumer group 'db_rpc_consumer_group' has no active members.\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lag, group, err := resolveAuthoritativeLag(topic, []groupDescribeResult{
				{group: dbConsumerGroupForZone(901), output: zoneGroupDescribe},
				c.legacy,
			})
			if err != nil {
				t.Fatal(err)
			}
			if group != "db_rpc_consumer_group_z901" || lag != 3 {
				t.Fatalf("got group=%q lag=%d, want db_rpc_consumer_group_z901 lag=3", group, lag)
			}
		})
	}
}

// F7 指定:非活跃组(legacy)的位点更高时,不能拿它的 LAG=0 替 zone 组担保,必须拒绝并提示先收尾。
func TestResolveAuthoritativeLag_InactiveGroupHigherIsRefused(t *testing.T) {
	_, _, err := resolveAuthoritativeLag("db_task_zone_901", []groupDescribeResult{
		{group: "db_rpc_consumer_group_z901", output: zoneGroupDescribe},
		{group: defaultKafkaGroup, output: legacyGroupHigherStale},
	})
	if err == nil {
		t.Fatal("two groups holding offsets on the topic must be refused (the stale one reports lag 0)")
	}
	for _, want := range []string{"FinalizeHandoff", "db_rpc_consumer_group_z901", defaultKafkaGroup, "-kafka-group"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q: %v", want, err)
		}
	}
}

func TestResolveAuthoritativeLag_BothEqualIsRefused(t *testing.T) {
	same := strings.ReplaceAll(zoneGroupDescribe, "db_rpc_consumer_group_z901 ", "db_rpc_consumer_group      ")
	_, _, err := resolveAuthoritativeLag("db_task_zone_901", []groupDescribeResult{
		{group: "db_rpc_consumer_group_z901", output: zoneGroupDescribe},
		{group: defaultKafkaGroup, output: same},
	})
	if err == nil || !strings.Contains(err.Error(), "FinalizeHandoff") {
		t.Fatalf("equal offsets in both groups must still be refused with the FinalizeHandoff hint, got %v", err)
	}
}

func TestResolveAuthoritativeLag_NoGroupHasOffsetsIsRefused(t *testing.T) {
	dashOnly := `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
db_rpc_consumer_group_z901 db_task_zone_901 0 - 120 -`
	cases := map[string][]groupDescribeResult{
		"both missing": {
			{group: "db_rpc_consumer_group_z901", output: "Error: Consumer group 'db_rpc_consumer_group_z901' does not exist.\n"},
			{group: defaultKafkaGroup, output: groupMissingKafka3},
		},
		"only dash rows": {
			{group: "db_rpc_consumer_group_z901", output: dashOnly},
			{group: defaultKafkaGroup, output: groupMissingKafka3},
		},
		"other topics only": {
			{group: "db_rpc_consumer_group_z901", output: "Consumer group 'db_rpc_consumer_group_z901' has no active members.\n"},
			{group: defaultKafkaGroup, output: legacyGroupOtherTopicOnly},
		},
		"no candidates": nil,
	}
	for name, results := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolveAuthoritativeLag("db_task_zone_901", results); err == nil {
				t.Fatal("no group holding offsets on the topic must be refused — nothing proves the backlog is drained")
			}
		})
	}
}

func TestResolveAuthoritativeLag_CLIFailureIsRefused(t *testing.T) {
	cases := map[string]groupDescribeResult{
		"non-zero exit with an unrelated error": {group: defaultKafkaGroup,
			output: "Error: Executing consumer group command failed due to Timed out waiting for a node assignment.\n",
			runErr: errors.New("exit status 1")},
		"Error line with zero exit": {group: defaultKafkaGroup,
			output: "Error: Executing consumer group command failed due to org.apache.kafka.common.errors.CoordinatorNotAvailableException\n"},
		"unparseable output": {group: defaultKafkaGroup, output: "something unexpected\n"},
		"empty output":       {group: defaultKafkaGroup, output: ""},
	}
	for name, legacy := range cases {
		t.Run(name, func(t *testing.T) {
			// zone 组本身是好的:另一个候选组读不出来也不能放行(读不到 ≠ 没有位点)。
			_, _, err := resolveAuthoritativeLag("db_task_zone_901", []groupDescribeResult{
				{group: "db_rpc_consumer_group_z901", output: zoneGroupDescribe},
				legacy,
			})
			if err == nil {
				t.Fatal("an unreadable candidate group must fail closed")
			}
		})
	}
}

func TestResolveAuthoritativeLag_DashLagOnAuthoritativeGroupIsRefused(t *testing.T) {
	out := `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
db_rpc_consumer_group_z901 db_task_zone_901 0 120 120 0
db_rpc_consumer_group_z901 db_task_zone_901 1 - 50 -`
	_, _, err := resolveAuthoritativeLag("db_task_zone_901", []groupDescribeResult{
		{group: "db_rpc_consumer_group_z901", output: out},
		{group: defaultKafkaGroup, output: groupMissingKafka3},
	})
	if err == nil {
		t.Fatal("a '-' LAG row on the authoritative group must fail closed (that partition has no committed offset)")
	}
}

func TestCommittedOffsetPresent_ColumnOrderFromHeader(t *testing.T) {
	// 列顺序被打乱(不同 Kafka 版本会插列):按表头名定位 TOPIC / CURRENT-OFFSET,不按下标。
	shuffled := `LAG CURRENT-OFFSET TOPIC GROUP PARTITION
3 120 db_task_zone_902 g 0
- - db_task_zone_901 g 0`
	if present, err := committedOffsetPresent(shuffled, "db_task_zone_901"); err != nil || present {
		t.Fatalf("only '-' rows for this topic: present=%v err=%v, want false nil", present, err)
	}
	if present, err := committedOffsetPresent(shuffled, "db_task_zone_902"); err != nil || !present {
		t.Fatalf("numeric row for this topic: present=%v err=%v, want true nil", present, err)
	}
	if present, err := committedOffsetPresent(legacyGroupHigherStale, "db_task_zone_901"); err != nil || !present {
		t.Fatalf("'has no active members' followed by a table: present=%v err=%v, want true nil", present, err)
	}
	if present, err := committedOffsetPresent("Consumer group 'g' has no active members.\n", "db_task_zone_901"); err != nil || present {
		t.Fatalf("empty group without header: present=%v err=%v, want false nil", present, err)
	}
	if present, err := committedOffsetPresent(groupMissingKafka3, "db_task_zone_901"); err != nil || present {
		t.Fatalf("missing group: present=%v err=%v, want false nil", present, err)
	}
	if _, err := committedOffsetPresent("no header and no keyword", "db_task_zone_901"); err == nil {
		t.Fatal("output without a header and without a known keyword must be an error")
	}
	if _, err := committedOffsetPresent("Error: Timed out waiting for a node assignment.\n", "db_task_zone_901"); err == nil {
		t.Fatal("an Error: line that is not a missing-group message must be an error")
	}
	bad := `TOPIC PARTITION CURRENT-OFFSET
db_task_zone_901 0 abc`
	if _, err := committedOffsetPresent(bad, "db_task_zone_901"); err == nil {
		t.Fatal("a CURRENT-OFFSET that is neither an integer nor '-' must be an error")
	}
}

// 「组不存在」白名单与 tools/scripts/lib/kafka_consumer_groups.ps1 的 Get-KafkaGroupMissingPatterns 同一份。
func TestConsumerGroupMissing_Whitelist(t *testing.T) {
	for _, out := range []string{
		"Error: Consumer group 'db_rpc_consumer_group' does not exist.",
		"The consumer group 'db_rpc_consumer_group' does not exist.",
		"org.apache.kafka.common.errors.GroupIdNotFoundException: The group id does not exist.",
		"Error: GROUP_ID_NOT_FOUND",
	} {
		if !consumerGroupMissing(out) {
			t.Errorf("must be recognized as a missing group: %q", out)
		}
	}
	for _, out := range []string{
		"Consumer group 'db_rpc_consumer_group' has no active members.",
		zoneGroupDescribe,
		"Error: Timed out waiting for a node assignment.",
	} {
		if consumerGroupMissing(out) {
			t.Errorf("must not be recognized as a missing group: %q", out)
		}
	}
}

// R7 复核 [8]:「组不存在」白名单在整段输出上匹配。白名单命中却又列出本 topic 的数值位点是矛盾输出,必须报错;
// 命中白名单但表里本 topic 没有数值位点时不认定 missing(两种读法都是「没有位点」),交给常规判定。
// 与 tools/scripts/lib/kafka_consumer_groups.ps1 Resolve-KafkaGroupTopicOffsets 同口径(kafka_offset_reset_output_guard.tests.ps1 有对应用例)。
func TestGroupMissingInDescribe_HeaderWinsOverWhitelist(t *testing.T) {
	const topic = "db_task_zone_901"
	warn := "[2026-10-17 03:00:00,000] WARN describe fell back to the classic API after " +
		"org.apache.kafka.common.errors.GroupIdNotFoundException (org.apache.kafka.clients.admin.KafkaAdminClient)\n"
	contradictory := warn + legacyGroupHigherStale
	if _, err := committedOffsetPresent(contradictory, topic); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("whitelist hit plus numeric rows on the topic must be refused as contradictory, got %v", err)
	}
	if _, _, err := resolveAuthoritativeLag(topic, []groupDescribeResult{
		{group: "db_rpc_consumer_group_z901", output: zoneGroupDescribe},
		{group: defaultKafkaGroup, output: contradictory},
	}); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("P3 must not treat a group with listed offsets as missing (it would vouch with the other group's LAG), got %v", err)
	}

	// 白名单命中、有表头、本 topic 没有数值位点:不是 missing,按表判为没有位点。
	otherTopicOnly := warn + legacyGroupOtherTopicOnly
	if missing, err := groupMissingInDescribe(otherTopicOnly, topic); err != nil || missing {
		t.Fatalf("header present without numeric rows on the topic: missing=%v err=%v, want false nil", missing, err)
	}
	if present, err := committedOffsetPresent(otherTopicOnly, topic); err != nil || present {
		t.Fatalf("header present without numeric rows on the topic: present=%v err=%v, want false nil", present, err)
	}

	// 没有表头时白名单照旧认定 missing(不论退出码)。
	if missing, err := groupMissingInDescribe(groupMissingKafka3, topic); err != nil || !missing {
		t.Fatalf("missing-group message without a header: missing=%v err=%v, want true nil", missing, err)
	}
}

// R7 复核 [3]:Empty 组的 describe 只列出有已提交位点的分区。分区 2 非空却从未提交过,表里根本没有它,
// 其余分区 LAG=0 时合计为 0 —— 必须靠 kafka-get-offsets 的日志边界拒绝。
func TestVerifyAuthoritativeCoverage(t *testing.T) {
	const topic = "db_task_zone_901"
	emptyGroup := `
Consumer group 'db_rpc_consumer_group_z901' has no active members.

GROUP                       TOPIC             PARTITION  CURRENT-OFFSET  LOG-END-OFFSET  LAG  CONSUMER-ID  HOST  CLIENT-ID
db_rpc_consumer_group_z901  db_task_zone_901  0          120             120             0    -            -     -
db_rpc_consumer_group_z901  db_task_zone_901  1          77              77              0    -            -     -
`
	if lag, err := parseConsumerGroupLag(emptyGroup, topic); err != nil || lag != 0 {
		t.Fatalf("precondition: the summed LAG is 0 (lag=%d err=%v)", lag, err)
	}
	cases := []struct {
		name     string
		describe string
		earliest map[int]int64
		latest   map[int]int64
		wantErr  string // 空串 = 必须通过
	}{
		{"never-committed non-empty partition is refused", emptyGroup,
			map[int]int64{0: 0, 1: 0, 2: 0}, map[int]int64{0: 120, 1: 77, 2: 5}, "partition 2: non-empty"},
		{"never-written partition passes", emptyGroup,
			map[int]int64{0: 0, 1: 0, 2: 0}, map[int]int64{0: 120, 1: 77, 2: 0}, ""},
		{"fully expired partition passes", emptyGroup,
			map[int]int64{0: 0, 1: 0, 2: 9}, map[int]int64{0: 120, 1: 77, 2: 9}, ""},
		{"get-offsets missing a described partition is refused", emptyGroup,
			map[int]int64{0: 0}, map[int]int64{0: 120}, "partition 1: in the describe output but missing from kafka-get-offsets"},
		{"earliest and latest disagree on the partition set", emptyGroup,
			map[int]int64{0: 0, 1: 0}, map[int]int64{0: 120, 1: 77, 2: 0}, "partition 2: kafka-get-offsets gave a latest offset but no earliest offset"},
		{"negative LAG on one partition is refused even if the sum is 0", `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
g db_task_zone_901 0 115 120 5
g db_task_zone_901 1 82 77 -5`,
			map[int]int64{0: 0, 1: 0}, map[int]int64{0: 120, 1: 77}, "LAG is not 0"},
		{"dash CURRENT-OFFSET on a non-empty partition is refused", `GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG
g db_task_zone_901 0 120 120 0
g db_task_zone_901 1 - 77 -`,
			map[int]int64{0: 0, 1: 0}, map[int]int64{0: 120, 1: 77}, "partition 1: non-empty (earliest=0 latest=77) but CURRENT-OFFSET is '-'"},
		{"no header is an error", "Consumer group 'g' has no active members.\n",
			map[int]int64{0: 0}, map[int]int64{0: 0}, "could not find TOPIC/PARTITION/CURRENT-OFFSET/LAG"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyAuthoritativeCoverage(topic, c.describe, c.earliest, c.latest)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestParseGetOffsets(t *testing.T) {
	const topic = "db_task_zone_901"
	got, err := parseGetOffsets("WARN something\n"+topic+":0:120\n"+topic+":1:0\ndb_task_zone_9011:0:7\n"+topic+"_g2:0:9\n", topic)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]int64{0: 120, 1: 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (topics sharing a prefix must not leak in)", got, want)
	}
	for name, out := range map[string]string{
		"error line":       topic + ":0:1\nError: Timed out waiting for a node assignment.\n",
		"no partition":     "db_task_zone_902:0:1\n",
		"empty output":     "",
		"malformed offset": topic + ":0:\n",
	} {
		if _, err := parseGetOffsets(out, topic); err == nil {
			t.Errorf("%s: want an error (cannot read the log boundaries)", name)
		}
	}
}

func TestKafkaGetOffsetsCmdFor(t *testing.T) {
	cases := map[string]string{
		"/opt/kafka/bin/kafka-consumer-groups.sh": "/opt/kafka/bin/kafka-get-offsets.sh",
		"kafka-consumer-groups.sh":                "kafka-get-offsets.sh",
		"kafka-consumer-groups":                   "kafka-get-offsets",
	}
	for in, want := range cases {
		got, err := kafkaGetOffsetsCmdFor(in)
		if err != nil || got != want {
			t.Errorf("kafkaGetOffsetsCmdFor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := kafkaGetOffsetsCmdFor("/opt/kafka/bin/kafka-run-class.sh"); err == nil {
		t.Fatal("a path that is not kafka-consumer-groups must be an error, not a guess")
	}
}

func TestKafkaGroupCandidates(t *testing.T) {
	if got := dbConsumerGroupForZone(901); got != "db_rpc_consumer_group_z901" {
		t.Fatalf("zone group=%q", got)
	}
	cases := []struct {
		name     string
		explicit string
		legacy   string
		want     []string
	}{
		{"default: zone group plus legacy", "", defaultKafkaGroup, []string{"db_rpc_consumer_group_z901", defaultKafkaGroup}},
		{"explicit override only", "custom_group", defaultKafkaGroup, []string{"custom_group"}},
		{"legacy disabled", "", "", []string{"db_rpc_consumer_group_z901"}},
		{"legacy equal to zone group is deduped", "", "db_rpc_consumer_group_z901", []string{"db_rpc_consumer_group_z901"}},
	}
	for _, c := range cases {
		if got := kafkaGroupCandidates(c.explicit, c.legacy, 901); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// ── 缓存键 ───────────────────────────────────────────────────

type saveOutboxFixture struct {
	counts map[string]int64
	failAt string
	err    error
	keys   []string
}

func (f *saveOutboxFixture) ZCard(_ context.Context, key string) *redis.IntCmd {
	f.keys = append(f.keys, key)
	if key == f.failAt {
		return redis.NewIntResult(0, f.err)
	}
	return redis.NewIntResult(f.counts[key], nil)
}

func TestSaveOutboxPreflightRequiresAllBucketsDrained(t *testing.T) {
	fixture := &saveOutboxFixture{}
	if err := assertSaveOutboxDrained(context.Background(), fixture, 901); err != nil {
		t.Fatal(err)
	}
	if len(fixture.keys) != 64 || fixture.keys[0] != "player:save_pending:901:0" || fixture.keys[63] != "player:save_pending:901:63" {
		t.Fatalf("未覆盖与正式补投器相同的64桶: %v", fixture.keys)
	}
	for _, tc := range []struct {
		name  string
		count int64
		err   error
	}{
		{"首桶有待提交意图", 1, nil},
		{"末桶有旧世代意图", 3, nil},
		{"Redis错误不能当空", 0, errors.New("Redis不可达")},
		{"错型不能当空", 0, errors.New("WRONGTYPE")},
		{"异常nil响应不能当空", 0, redis.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := "player:save_pending:901:63"
			if tc.name == "首桶有待提交意图" {
				key = "player:save_pending:901:0"
			}
			fixture := &saveOutboxFixture{counts: map[string]int64{key: tc.count}, failAt: key, err: tc.err}
			if tc.err == nil {
				fixture.failAt = ""
			}
			if err := assertSaveOutboxDrained(context.Background(), fixture, 901); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("应拒绝并定位未排空/不可证明的桶 %s，实际 %v", key, err)
			}
		})
	}
}

func TestPlayerCacheKeys_CoversParentSubAndDiscoveredTables(t *testing.T) {
	keys := playerCacheKeys(42, []string{"player_database", "player_database_1", "player_centre_database"})
	want := map[string]bool{
		"PlayerAllData:{42}":        true,
		"BagAllData:42":             true,
		"QuestAllData:42":           true,
		"MailAllData:42":            true,
		"player_database:42":        true,
		"player_database_1:42":      true,
		"player_centre_database:42": true,
		// 资产 SQL 提交证据(不是缓存,但同在 DB 0、同样不带 zone;字面量必须与 go/shared/assetop.SQLCommitKey 一致)。
		"player:{42}:asset_sql_commit": true,
		"player:{42}:save_barrier":     true,
	}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys %v, want %d", len(keys), keys, len(want))
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("missing keys %v", want)
	}
}

func TestPlayerCacheKeys_DeduplicatesTableAndMsgTypeOverlap(t *testing.T) {
	// 表名与 MsgType 同名是常态;重复的键会让 pipeline 白跑一次 DEL。
	keys := playerCacheKeys(7, []string{"BagAllData", "player_database"})
	seen := map[string]int{}
	for _, k := range keys {
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %q appears %d times", k, n)
		}
	}
}

// ── 批次 / 列表工具 ──────────────────────────────────────────

func TestChunkUint64(t *testing.T) {
	got := chunkUint64([]uint64{1, 2, 3, 4, 5}, 2)
	want := [][]uint64{{1, 2}, {3, 4}, {5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := len(chunkUint64(nil, 10)); n != 0 {
		t.Fatalf("nil input produced %d chunks", n)
	}
	// size<=0 不能死循环。
	if n := len(chunkUint64([]uint64{1, 2}, 0)); n != 2 {
		t.Fatalf("size 0 produced %d chunks", n)
	}
}

func TestInListLiteral(t *testing.T) {
	if got := inListLiteral([]uint64{1, 20, 300}); got != "1,20,300" {
		t.Fatalf("got %q", got)
	}
	if got := inListLiteral(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSortedUint64_SortsAndDedups(t *testing.T) {
	got := sortedUint64([]uint64{5, 1, 5, 3, 1})
	want := []uint64{1, 3, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if sortedUint64(nil) != nil {
		t.Fatal("nil should stay nil")
	}
}

// ── 玩家行拷贝的列对齐(03-names.md §3.16 / §3.19)──────────────

func TestBuildCopyColumns_SameSetDifferentOrderUsesTargetOrder(t *testing.T) {
	// 两个 zone 库分别跑迁移,同一批新列落在不同的 ORDINAL_POSITION 上是常态:
	// 这不是错误,按列名对位照常拷贝,返回目标库的列序。
	src := []string{"player_id", "asset_op_ledger", "profile_component", "bag"}
	dst := []string{"player_id", "bag", "profile_component", "asset_op_ledger"}
	got, err := buildCopyColumns(src, dst)
	if err != nil {
		t.Fatalf("same column set in a different order must not be an error: %v", err)
	}
	if !reflect.DeepEqual(got, dst) {
		t.Fatalf("got %v want target order %v", got, dst)
	}
	// 返回值是副本:调用方改它不能反过来改到入参。
	got[0] = "mutated"
	if dst[0] != "player_id" {
		t.Fatal("buildCopyColumns returned the caller's dst slice instead of a copy")
	}
}

func TestBuildCopyColumns_ExtraColumnOnEitherSideIsAnError(t *testing.T) {
	base := []string{"player_id", "bag"}
	cases := []struct {
		name      string
		src, dst  []string
		wantInErr []string
	}{
		{
			name:      "源库多一列(目标库没跑迁移)",
			src:       []string{"player_id", "bag", "profile_component"},
			dst:       base,
			wantInErr: []string{"only_in_src=[profile_component]", "only_in_dst=[]"},
		},
		{
			name:      "目标库多一列(源库没跑迁移)",
			src:       base,
			dst:       []string{"player_id", "asset_op_ledger", "bag"},
			wantInErr: []string{"only_in_src=[]", "only_in_dst=[asset_op_ledger]"},
		},
		{
			name:      "两边各多一列(列数相同,SELECT * 时代会静默串列)",
			src:       []string{"player_id", "bag", "profile_component"},
			dst:       []string{"player_id", "bag", "asset_op_ledger"},
			wantInErr: []string{"only_in_src=[profile_component]", "only_in_dst=[asset_op_ledger]"},
		},
	}
	for _, c := range cases {
		cols, err := buildCopyColumns(c.src, c.dst)
		if err == nil {
			t.Errorf("%s: expected an error, got columns %v", c.name, cols)
			continue
		}
		if cols != nil {
			t.Errorf("%s: a mismatch must not return a usable column list, got %v", c.name, cols)
		}
		for _, want := range c.wantInErr {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", c.name, err, want)
			}
		}
	}
}

func TestBuildCopyColumns_RejectsBackquoteInColumnName(t *testing.T) {
	// 列名会被反引号包裹后直接拼进 SQL;含反引号的名字能从引号里逃出去。
	// 两边集合相同也必须拒绝 —— 这条检查不能被「集合相等」短路。
	evil := "bag` FROM x; --"
	for _, c := range []struct{ src, dst []string }{
		{[]string{"player_id", evil}, []string{"player_id", evil}},
		{[]string{"player_id", evil}, []string{"player_id"}},
		{[]string{"player_id"}, []string{"player_id", evil}},
	} {
		if cols, err := buildCopyColumns(c.src, c.dst); err == nil {
			t.Errorf("src=%v dst=%v: backquote must be rejected, got %v", c.src, c.dst, cols)
		}
	}
}

func TestAuditPlayerNameConflicts_IsGlobalInfoRow(t *testing.T) {
	// 名字全服唯一(data_service player_name),合服没有重名可撞:这一行只能是 info。
	// 它若变成 block,每次合服审计都会 exit 1,运维只能学会跳过门禁。
	r := auditPlayerNameConflicts(context.Background(), auditConfig{})
	if r.Name != "player.name (global)" || r.UniqueScope != "global" {
		t.Errorf("name/scope = %q/%q, want player.name (global)/global", r.Name, r.UniqueScope)
	}
	if r.Severity != "info" {
		t.Errorf("severity = %q, want info", r.Severity)
	}
	if r.ConflictCount != 0 {
		t.Errorf("conflicts = %d, want 0 (global uniqueness leaves nothing to collide)", r.ConflictCount)
	}
	if isInfraAudit(r) {
		t.Error("the naming note must not be misread as an INFRA finding")
	}
}

// ── 清单 ─────────────────────────────────────────────────────

func TestManifestRoundTripAndStepFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	now := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	m := newMergeManifest("run-1", 901, 902, "host/pid=1", now)
	m.PlayerIDs = []uint64{3, 1, 2}
	m.GuildIDs = []uint64{9}
	m.RankMembers = []manifestRankMember{{Member: "9", Score: 12.5}}
	m.Tables = []string{"player_database"}
	m.markStep(stepPlayerRows, "copied=3")

	if err := saveManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.stepDone(stepPlayerRows) {
		t.Error("player_rows should be marked done")
	}
	if got.stepDone(stepGuildRank) {
		t.Error("guild_rank should not be done")
	}
	if got.RunID != "run-1" || got.SourceZone != 901 || got.TargetZone != 902 {
		t.Errorf("identity lost: %+v", got)
	}
	if !reflect.DeepEqual(got.RankMembers, m.RankMembers) {
		t.Errorf("rank members lost: %v", got.RankMembers)
	}
	// 落盘必须是原子的 —— .tmp 不能留下。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file left behind")
	}
}

func TestLoadManifest_MissingFileIsNotAnError(t *testing.T) {
	got, err := loadManifest(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || got != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
	}
}

func TestLoadManifest_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	raw, _ := json.Marshal(map[string]any{"version": manifestVersion + 1})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("expected a version mismatch error rather than a guess")
	}
}

func TestValidateManifestForRun_RejectsWrongZones(t *testing.T) {
	m := newMergeManifest("r", 901, 902, "op", time.Now())
	if err := validateManifestForRun(m, 901, 902); err != nil {
		t.Fatalf("matching zones rejected: %v", err)
	}
	if err := validateManifestForRun(m, 903, 902); err == nil {
		t.Fatal("expected a mismatch error for a different source zone")
	}
	if err := validateManifestForRun(nil, 1, 2); err != nil {
		t.Fatalf("nil manifest should be fine: %v", err)
	}
}

func TestDefaultManifestPathShape(t *testing.T) {
	got := defaultManifestPath(901, 902, time.Date(2026, 9, 8, 3, 4, 5, 0, time.UTC))
	if got != "merge_901_to_902_20260908T030405Z.json" {
		t.Fatalf("got %q", got)
	}
}

// ── 围栏 ─────────────────────────────────────────────────────

func TestMergeFenceKeyAndValueShape(t *testing.T) {
	if got := mergeFenceKey(901); got != "merge:in_progress:901" {
		t.Fatalf("got %q", got)
	}
	v := mergeInProgressValue{
		Tool: "tools/merge_zone", RunID: "r1", SourceZone: 901, TargetZone: 902,
		StartedAt: "2026-09-08T03:00:00Z", StartedAtUnixMs: 1, ExpiresAtUnixMs: 2,
		Operator: "h/pid=1", ManifestPath: "m.json",
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	// 契约:读者只做 EXISTS,但 value 必须是可读 JSON 且带 run_id / started_at。
	for _, want := range []string{`"tool":"tools/merge_zone"`, `"run_id":"r1"`, `"started_at"`, `"source_zone":901`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("fence value missing %s: %s", want, raw)
		}
	}
	var back mergeInProgressValue
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back != v {
		t.Errorf("round trip changed the value")
	}
}

func TestNewRunIDIsUnique(t *testing.T) {
	now := time.Now()
	a := newRunID(1, 2, now)
	b := newRunID(1, 2, now)
	if a == b {
		t.Fatal("two runs started in the same second must still get different ids")
	}
}

// ── guild 键 ─────────────────────────────────────────────────

func TestGuildKeyShapesMatchGuildRepo(t *testing.T) {
	if got := guildCacheKey(7); got != "guild:v2:7" {
		t.Fatalf("got %q", got)
	}
	if got := guildCacheGenerationKey(7); got != "guild:v2:cache_generation:7" {
		t.Fatalf("got %q", got)
	}
	if got := guildZoneRankKey(902); got != "guild_rank:zone:902" {
		t.Fatalf("got %q", got)
	}
	if guildRankLockKey != "guild_rank:maintenance_lock" {
		t.Fatalf("lock key drifted from guild_repo.go: %q", guildRankLockKey)
	}
}

// ── 帮会库名 ─────────────────────────────────────────────────

func TestGuildNamesMatchGuildService(t *testing.T) {
	// merge_zone 是独立 module,不能 import go/guild:库名镜像 go/guild/internal/data.DatabaseName
	// (guild 服的 config.Validate 强制 DSN 库名等于它),表名镜像 proto/guild/guild_db.proto 的
	// OptionTableName。漂移 = 改写落到不存在的表上,或更糟:落到另一个库的同名表上。
	if defaultGuildSchema != "mmorpg_guild" {
		t.Fatalf("defaultGuildSchema drifted from go/guild data.DatabaseName: %q", defaultGuildSchema)
	}
	if guildTable != "guild" || guildMemberTable != "guild_member" {
		t.Fatalf("guild table names drifted from guild_db.proto: %q / %q", guildTable, guildMemberTable)
	}
	// 重名探测比的是唯一键那一列(uk_guild = name_norm),不是展示名。
	if guildNameNormColumn != "name_norm" {
		t.Fatalf("guild unique-key column drifted from guild_db.proto: %q", guildNameNormColumn)
	}
	if got := guildQualified(defaultGuildSchema, guildTable); got != "mmorpg_guild.guild" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateGuildSchemaName(t *testing.T) {
	for _, ok := range []string{"mmorpg_guild", "merge_zone_it_guild", "Guild1", strings.Repeat("a", 64)} {
		if err := validateGuildSchemaName(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	// 库名直接拼进 SQL:任何带点、引号、空白、分号、连字符的都必须拒绝。
	for _, bad := range []string{
		"", "mmorpg_guild.guild", "x;DROP DATABASE y", "a b", "`mmorpg_guild`",
		"mmorpg-guild", "mmorpg_guild\n", strings.Repeat("a", 65),
	} {
		if err := validateGuildSchemaName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// ── 审计 INFRA 标记 ──────────────────────────────────────────

func TestInfraAuditIsBlockAndDetectable(t *testing.T) {
	r := infraAudit("x", "redis ping failed: %v", "timeout")
	if r.Severity != "block" {
		t.Errorf("infra findings must be block severity, got %q", r.Severity)
	}
	if !isInfraAudit(r) {
		t.Error("infra finding not detected — exit code would be 1 instead of 2")
	}
	if isInfraAudit(ResourceAudit{Name: "y", Severity: "block", Notes: "real problem"}) {
		t.Error("a genuine block finding must not be misread as infra")
	}
	if !strings.Contains(r.Notes, "timeout") {
		t.Errorf("infra note lost the cause: %q", r.Notes)
	}
}

// ── 数据锁键(item 10 的排除对象)────────────────────────────

func TestPlayerDataLockKeyMatchesRouter(t *testing.T) {
	// go/data_service/internal/routing/router.go::PlayerDataLockKey
	if got := playerDataLockKey(42); got != "player:{42}:__lock" {
		t.Fatalf("got %q", got)
	}
	// 它必须落在 scan pattern 内 —— 正因为如此才需要显式排除。
	if !strings.HasPrefix(playerDataLockKey(42), "player:{42}:") {
		t.Fatal("lock key no longer matches the scan pattern; the skip is now dead code")
	}
}

// ── 写意图 ───────────────────────────────────────────────────

func TestRequireWriteIntent(t *testing.T) {
	if err := requireWriteIntent(options{}); err == nil {
		t.Error("neither flag should be refused")
	}
	if err := requireWriteIntent(options{dryRun: true, apply: true}); err == nil {
		t.Error("both flags should be refused (ops would think it wrote)")
	}
	if err := requireWriteIntent(options{dryRun: true}); err != nil {
		t.Errorf("-dry-run alone: %v", err)
	}
	if err := requireWriteIntent(options{apply: true}); err != nil {
		t.Errorf("-apply alone: %v", err)
	}
}

// ── 默认 DB 号(item 3 的回归护栏)──────────────────────────

func TestRedisDBDefaultsMatchServiceYAML(t *testing.T) {
	// 这三个常量若漂移,合服会安静地扫错库。数值来源见 main.go 顶部注释。
	// (friend 的 DB 3 一条已随 friend:online 退役删除,2026-09-18 friend 移植 F3。)
	cases := []struct {
		name string
		got  int
		want int
	}{
		// go-zero RedisConf 没有 DB 字段 → mapping Redis 恒为 DB 0(见 main.go 注释)。
		{"mapping (data_service MappingRedis, go-zero 无 DB 字段)", defaultMappingRedisDB, 0},
		{"guild (guild.yaml RedisClient.DB;2026-10-23 由 2 改为 0)", defaultGuildRedisDB, 0},
		{"shared/login (login.yaml Node.RedisClient.DB)", defaultSharedRedisDB, 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.name, c.got, c.want)
		}
	}
}

func TestReadOnlyPreflightDoesNotNeedWriteGrantButStillRequiresFacts(t *testing.T) {
	err := runPreflight(context.Background(), preflightDeps{}, preflightParams{readOnly: true, zone: 1, scope: "source"})
	if err == nil || errors.Is(err, errNormalFreezeProof) || errors.Is(err, drainproof.ErrHold) || !strings.Contains(err.Error(), "no scene_manager Redis handle") {
		t.Fatalf("只读预演应进入真实事实检查、缺句柄仍拒绝: %v", err)
	}
}
func TestWritePreflightStillRejectsMissingNormalFreeze(t *testing.T) {
	err := runPreflight(context.Background(), preflightDeps{}, preflightParams{zone: 1, scope: "source"})
	if !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("默认真实写不能借只读路径免除冻结: %v", err)
	}
	ctx, owner := testMaintenanceContext()
	err = runPreflight(ctx, preflightDeps{}, preflightParams{zone: 1, scope: "source"})
	if !errors.Is(err, errNormalFreezeProof) {
		t.Fatalf("只有TOOL没有正常冻结仍须拒绝: %v", err)
	}
	owner.session.CloseAdmission()
	if !errors.Is(owner.session.Wait(context.Background()), drainproof.ErrHold) {
		t.Fatal("写路径拒绝未留下HOLD")
	}
}
