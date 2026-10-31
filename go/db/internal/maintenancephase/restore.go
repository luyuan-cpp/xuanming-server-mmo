// Package maintenancephase 是维护排空(shared/drainproof)的阶段动作:核正式原件,再交给 authoritysql.Catalog 推进阶段。
package maintenancephase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/handofffence"
	"shared/kafkautil"
	"shared/placement"

	"google.golang.org/protobuf/proto"
)

func validSchema(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func maintenanceAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	n, e := strconv.ParseUint(port, 10, 16)
	return err == nil && host != "" && !strings.ContainsAny(host, " /\\@|\r\n\t") && e == nil && n != 0 && strconv.FormatUint(n, 10) == port
}

type ReleaseFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func releaseHex(v string, n int) bool {
	return len(v) == n && strings.Trim(v, "0123456789abcdef") == "" && v != strings.Repeat("0", n)
}

func releaseExactStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, v := range b {
		if seen[v] {
			return false
		}
		seen[v] = true
	}
	for _, v := range a {
		if !seen[v] {
			return false
		}
		delete(seen, v)
	}
	return len(seen) == 0
}

type maintenanceReleaseRedisKeyObservation struct {
	Key     string   `json:"key"`
	KeySHA  string   `json:"key_sha256"`
	Kind    string   `json:"kind"`
	Topic   string   `json:"topic,omitempty"`
	Type    string   `json:"type"`
	DumpSHA string   `json:"dump_sha256"`
	PTTL    int64    `json:"pttl_ms"`
	Count   int64    `json:"count"`
	Members []string `json:"members"`
	Values  []string `json:"values"`
}

// 原key及原字段值是机核事实；摘要仅防传输替换，不能代替内容和等集检查。
func releaseRedisObservation(o maintenanceReleaseRedisKeyObservation) error {
	h := sha256.Sum256([]byte(o.Key))
	if o.Key == "" || hex.EncodeToString(h[:]) != o.KeySHA || o.Count < 0 || len(o.Members) > 16384 || len(o.Values) > 16384 {
		return errors.New("Redis原键/条目不规范")
	}
	id := strings.TrimSuffix(strings.TrimPrefix(o.Key, "player:{"), "}:handoff_fence")
	n, e := strconv.ParseUint(id, 10, 64)
	if e != nil || n == 0 || handofffence.Key(n) != o.Key || o.Kind != "handoff_fence" || o.Topic != "" {
		return errors.New("Redis未定义键或键角色")
	}
	if (o.Type != "none" && o.Type != "hash") || !releaseHex(o.DumpSHA, 64) {
		return errors.New("Redis原类型或原件摘要错误")
	}
	if o.Type == "none" {
		empty := sha256.Sum256(nil)
		if o.PTTL != -2 || o.Count != 0 || len(o.Members) != 0 || len(o.Values) != 0 || o.DumpSHA != hex.EncodeToString(empty[:]) {
			return errors.New("缺键原事实冲突")
		}
		return nil
	}
	if o.PTTL != -1 || int64(len(o.Members)) != o.Count || len(o.Values) != len(o.Members) {
		return errors.New("交接资格 hash 原条目数/永久性不符")
	}
	seen := map[string]bool{}
	for _, field := range o.Members {
		if seen[field] {
			return errors.New("Redis原成员重复")
		}
		seen[field] = true
		if field == "pending" || field == "player_database" || field == "player_database_1" || strings.HasPrefix(field, "outbox_") {
			return errors.New("交接资格 hash 残留旧存盘字段")
		}
	}
	return nil
}

func releaseEqualZones(a, b []uint32) bool {
	aa := []string{}
	bb := []string{}
	for _, z := range a {
		aa = append(aa, strconv.FormatUint(uint64(z), 10))
	}
	for _, z := range b {
		bb = append(bb, strconv.FormatUint(uint64(z), 10))
	}
	return releaseExactStrings(aa, bb)
}

// ReadAuthorityProto 读取正式 typed 原件。
func ReadAuthorityProto(pin ReleaseFile, out proto.Message) error {
	digest, err := hex.DecodeString(pin.SHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("authority原件摘要非法")
	}
	raw, err := drainproof.ReadPin(&databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: pin.Path, Sha256: digest}, 16<<20)
	if err != nil {
		return err
	}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, out); err != nil {
		return err
	}
	if len(out.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("authority原件包含未知字段")
	}
	return nil
}

// ValidateAuthorityPlan核完整typed ManifestProto，不能按当前配置topic或COUNT缩成子集。
func ValidateAuthorityPlan(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) error {
	if plan == nil || len(plan.RunId) != 16 || len(plan.PhysicalClusterIdentity) != 32 || !validSchema(plan.AuthoritySchema) || len(plan.ProtectedSchemas) == 0 || plan.BeforeEpoch == 0 || plan.CatalogRevision == 0 || len(plan.ActiveWriterKeys) == 0 || len(plan.RootIssuers) == 0 || len(plan.RootIssuers) > 1024 || len(plan.QueueMutators) == 0 || len(plan.QueueMutators) > 1024 || len(plan.KafkaPartitions) == 0 || len(plan.KafkaPartitions) > 16384 || len(plan.RedisTargets) == 0 || len(plan.RedisTargets) > 128 || len(plan.SourcePins) == 0 || len(plan.SourcePins) > 128 || len(plan.Launchers) == 0 || len(plan.Launchers) > 1024 {
		return errors.New("authority完整目录缺失或超预算")
	}
	if len(plan.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("authority manifest未知字段")
	}
	for i, key := range plan.ActiveWriterKeys {
		if len(key) != 16 || i > 0 && bytes.Compare(plan.ActiveWriterKeys[i-1], key) >= 0 {
			return errors.New("ACTIVE物理writer全集不规范")
		}
	}
	for i, schema := range plan.ProtectedSchemas {
		if !validSchema(schema) || i > 0 && plan.ProtectedSchemas[i-1] >= schema {
			return errors.New("受保护schema全集不规范")
		}
	}
	issuers := map[string]bool{}
	for _, roles := range [][]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators, plan.MaintenanceIssuers} {
		for _, role := range roles {
			if role == nil || len(role.IssuerId) != 16 || len(role.InstanceId) != 16 || len(role.PermanentRevivalFenceId) != 16 || role.LogicalRole == "" || len(role.SourceEvidence) == 0 {
				return errors.New("逻辑角色seed身份缺失")
			}
			key := string(role.IssuerId)
			if issuers[key] {
				return errors.New("逻辑角色issuer重复")
			}
			issuers[key] = true
		}
	}
	if len(plan.MaintenanceIssuers) > 1024 {
		return errors.New("冻结维护工具目录超预算")
	}
	for _, tool := range plan.MaintenanceIssuers {
		if tool.LogicalRole != "MAINTENANCE" || len(tool.PhysicalWriterKey) != 0 || tool.AcceptingClosedMs != 0 || tool.AcceptedSettledMs != 0 || len(tool.AcceptedFrontierDigest) != 0 || tool.UnresolvedPublications != 0 {
			return errors.New("维护工具必须是P0固定单次boot，不得预填闭合或复用SQL写者")
		}
	}
	partitions := map[string]bool{}
	for _, row := range plan.KafkaPartitions {
		if row == nil || len(row.ClusterId) == 0 || len(row.TopicId) != 16 || row.TopicName == "" || row.Group == "" || row.InitialEarliest < 0 || row.ProtectedCommittedFloor < row.InitialEarliest {
			return errors.New("历史Kafka物理身份/保护floor缺失")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		if partitions[key] {
			return errors.New("历史Kafka订阅重复")
		}
		partitions[key] = true
	}
	targets := map[string]bool{}
	for _, target := range plan.RedisTargets {
		if target == nil || !maintenanceAddress(target.Address) || target.Db < 0 || len(target.ServerRunId) != 40 || strings.Trim(string(target.ServerRunId), "0123456789abcdef") != "" || bytes.Equal(target.ServerRunId, bytes.Repeat([]byte{'0'}, 40)) || len(target.MaintenanceNonce) != 16 || bytes.Equal(target.MaintenanceNonce, make([]byte, 16)) || len(target.Zones) == 0 {
			return errors.New("Redis目标原runid/nonce缺失")
		}
		key := fmt.Sprintf("%s/%d", target.Address, target.Db)
		if targets[key] {
			return errors.New("Redis目标重复")
		}
		targets[key] = true
		zones := map[uint32]bool{}
		for _, zone := range target.Zones {
			if zone == 0 || zone > placement.MaxZoneStorageID || zones[zone] {
				return errors.New("Redis目标zone非法或重复")
			}
			zones[zone] = true
		}
	}
	for _, pin := range plan.SourcePins {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	launchers := map[string]bool{}
	for _, launcher := range plan.Launchers {
		if launcher == nil || len(launcher.LauncherId) != 16 || len(launcher.PermanentFenceId) != 16 || len(launcher.SourcePins) == 0 || launchers[string(launcher.LauncherId)] {
			return errors.New("启动者永久禁止复活目录缺失或重复")
		}
		launchers[string(launcher.LauncherId)] = true
		for _, pin := range launcher.SourcePins {
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
	}
	if err := verifyAuthorityRedisBaseline(plan); err != nil {
		return err
	}
	_, err := authorityKafkaBaseline(plan)
	return err
}
func authoritySameRole(a, b *databasepb.SqlWriterMaintenanceReceipt_IssuerClosure) bool {
	return a != nil && b != nil && bytes.Equal(a.IssuerId, b.IssuerId) && bytes.Equal(a.InstanceId, b.InstanceId) && bytes.Equal(a.PhysicalWriterKey, b.PhysicalWriterKey) && a.LogicalRole == b.LogicalRole && bytes.Equal(a.PermanentRevivalFenceId, b.PermanentRevivalFenceId)
}
func verifyAuthorityRoles(expected, actual []*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure) error {
	if len(actual) != len(expected) || len(actual) == 0 || len(actual) > 1024 {
		return errors.New("逻辑角色闭合不是审定全集")
	}
	set := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, seed := range expected {
		set[string(seed.IssuerId)] = seed
	}
	for _, role := range actual {
		if role == nil || !authoritySameRole(set[string(role.IssuerId)], role) {
			return errors.New("逻辑角色身份/禁止复活范围变化")
		}
		if err := drainproof.VerifyClosure(role); err != nil {
			return err
		}
		seed := set[string(role.IssuerId)]
		if !authorityPinSetEqual(seed.SourceEvidence, role.SourceEvidence) {
			return errors.New("逻辑角色换P0构建/监督源pins")
		}
		delete(set, string(role.IssuerId))
	}
	if len(set) != 0 {
		return errors.New("逻辑角色闭合遗漏")
	}
	return nil
}

// VerifyAuthorityEvidence是v2唯一portable事实owner。调用方必须把按原ID强读的SQL阶段收据传入；
// 文件/Redis/Kafka采集在控制事务外完成，本函数不把未知提交重放成新动作。
func VerifyAuthorityEvidence(ctx context.Context, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, manifest []byte, fact *databasepb.SqlWriterMaintenanceReceipt, rootReceipt, relayReceipt *databasepb.SqlWriterMaintenanceReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateAuthorityPlan(plan); err != nil {
		return err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil || !bytes.Equal(encoded, manifest) {
		return errors.New("manifest不是原完整确定性proto")
	}
	sum := sha256.Sum256(manifest)
	if fact == nil || fact.Evidence == nil || !bytes.Equal(fact.RunId, plan.RunId) || !bytes.Equal(fact.Evidence.OperationId, plan.RunId) || !bytes.Equal(fact.Evidence.ManifestDigest, sum[:]) || fact.BeforeEpoch != plan.BeforeEpoch || fact.CatalogRevision < plan.CatalogRevision || fact.CatalogRevision-plan.CatalogRevision > uint64(len(plan.ActiveWriterKeys)) {
		return errors.New("阶段原件偏离run/manifest/epoch")
	}
	if publication := fact.Evidence.GetPublication(); publication != nil {
		expected := plan.RootIssuers
		if fact.Kind == databasepb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED {
			expected = plan.QueueMutators
		} else if fact.Kind != databasepb.SqlWriterMaintenanceReceipt_ROLE_CLOSED {
			return errors.New("角色事实kind不符")
		}
		if err := verifyAuthorityPublicationOriginals(expected, publication); err != nil {
			return err
		}
		return verifyAuthorityRoles(expected, publication.LogicalRoles)
	}
	closure := fact.Evidence.GetNonSqlClosure()
	if closure == nil {
		return errors.New("当前阶段缺正式nonSQL闭包")
	}
	if rootReceipt == nil || relayReceipt == nil || rootReceipt.Kind != databasepb.SqlWriterMaintenanceReceipt_ROLE_CLOSED || rootReceipt.RunPhase != databasepb.SqlWriterDrainRun_ROOTS_CLOSED || relayReceipt.Kind != databasepb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED || relayReceipt.RunPhase != databasepb.SqlWriterDrainRun_DRAINING || !bytes.Equal(closure.RootClosedReceiptId, rootReceipt.RequestId) || !bytes.Equal(closure.RootClosedReceiptDigest, rootReceipt.ReceiptDigest) || !bytes.Equal(closure.RelayQuiescedReceiptId, relayReceipt.RequestId) || !bytes.Equal(closure.RelayQuiescedReceiptDigest, relayReceipt.ReceiptDigest) || rootReceipt.AttemptRevision == 0 || rootReceipt.AttemptRevision > fact.AttemptRevision || relayReceipt.AttemptRevision == 0 || relayReceipt.AttemptRevision > fact.AttemptRevision {
		return errors.New("nonSQL未绑定同run已永久封闭的原root/relay SQL收据")
	}
	for _, receipt := range []*databasepb.SqlWriterMaintenanceReceipt{rootReceipt, relayReceipt} {
		if receipt.Evidence == nil || !bytes.Equal(receipt.RunId, plan.RunId) || !bytes.Equal(receipt.Evidence.ManifestDigest, sum[:]) || receipt.Generation != fact.Generation || !bytes.Equal(receipt.PlanProto, manifest) || receipt.BeforeEpoch != plan.BeforeEpoch || receipt.AfterEpoch != plan.BeforeEpoch {
			return errors.New("root/relay原收据换manifest")
		}
	}
	if err := verifyAuthorityPublicationOriginals(plan.RootIssuers, rootReceipt.Evidence.GetPublication()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.RootIssuers, rootReceipt.Evidence.GetPublication().GetLogicalRoles()); err != nil {
		return err
	}
	if err := verifyAuthorityPublicationOriginals(plan.QueueMutators, relayReceipt.Evidence.GetPublication()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.QueueMutators, relayReceipt.Evidence.GetPublication().GetLogicalRoles()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.QueueMutators, closure.QueueMutators); err != nil {
		return err
	}
	if closure.ObservedStartMs == 0 || closure.ObservedEndMs < closure.ObservedStartMs || closure.ObservedEndMs-closure.ObservedStartMs > 60000 || closure.ObservedEndMs > uint64(time.Now().UnixMilli()) {
		return errors.New("nonSQL观察不在60秒新窗口")
	}
	for _, role := range append(append([]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}, rootReceipt.Evidence.GetPublication().LogicalRoles...), closure.QueueMutators...) {
		if role.AcceptedSettledMs > closure.ObservedStartMs {
			return errors.New("队列观察早于发行者实际settled")
		}
	}
	if len(closure.SourceEvidence) == 0 || len(closure.SourceEvidence) > 128 {
		return errors.New("nonSQL缺原始采集源")
	}
	for _, pin := range closure.SourceEvidence {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	if err := verifyAuthorityKafka(plan, closure); err != nil {
		return err
	}
	return verifyAuthorityRedis(plan, closure)
}

type authorityManualLeader struct {
	Schema             string            `json:"schema"`
	ObservedStartMS    uint64            `json:"observed_start_ms"`
	ObservedEndMS      uint64            `json:"observed_end_ms"`
	CatalogFirstSHA256 []byte            `json:"catalog_first_sha256"`
	CatalogLastSHA256  []byte            `json:"catalog_last_sha256"`
	Consumers          []json.RawMessage `json:"consumers"`
}

func verifyManualClosure(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	if len(plan.ManualAssignments) == 0 {
		if len(actual.ManualConsumers) != 0 {
			return errors.New("P4出现P0未列manual owner")
		}
		return nil
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	sources := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, row := range plan.ManualAssignments {
		expected[manualAssignmentKey(row)] = row
	}
	for _, source := range plan.RootIssuers {
		sources[string(source.IssuerId)] = source
	}
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		return err
	}
	manifestDigest := sha256.Sum256(manifest)
	var leader *authorityManualLeader
	var leaderPin *databasepb.SqlWriterMaintenanceReceipt_SourcePin
	catalogs := map[string]bool{}
	for _, pin := range actual.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil {
			continue
		}
		if marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			sum := sha256.Sum256(raw)
			catalogs[string(sum[:])] = true
		}
		if marker.Schema != "SQL_DRAIN_MANUAL_KAFKA_LEADER_V1" {
			continue
		}
		if leader != nil {
			return errors.New("manual P4 leader原件重复")
		}
		leader = &authorityManualLeader{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(leader) != nil {
			return errors.New("manual P4 leader原件格式无效")
		}
		leaderPin = pin
	}
	if leader == nil || leader.ObservedStartMS < actual.ObservedStartMs || leader.ObservedEndMS < leader.ObservedStartMS || leader.ObservedEndMS > actual.ObservedEndMs || leader.ObservedEndMS-leader.ObservedStartMS > 60000 || len(leader.Consumers) == 0 || len(leader.Consumers) != len(actual.ManualConsumers) || !catalogs[string(leader.CatalogFirstSHA256)] || !catalogs[string(leader.CatalogLastSHA256)] {
		return errors.New("manual P4缺真实采样窗口或对应首尾物理目录")
	}
	observed := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{}
	for _, entry := range actual.ManualConsumers {
		if entry == nil || len(entry.Assignments) == 0 || len(entry.SourcePins) != 2 {
			return errors.New("manual P4条目缺P3/leader原件")
		}
		key := string(entry.Assignments[0].IssuerId) + string(entry.Assignments[0].InstanceId)
		if observed[key] != nil {
			return errors.New("manual P4 boot重复")
		}
		observed[key] = entry
	}
	seen := map[string]bool{}
	seenP3 := map[string]bool{}
	for _, raw := range leader.Consumers {
		sampled, err := parseManualEvidence(raw)
		if err != nil {
			return err
		}
		if len(sampled.SourcePins) != 1 || !bytes.Equal(sampled.OperationId, plan.RunId) || !bytes.Equal(sampled.ManifestDigest, manifestDigest[:]) || !sampled.AcceptingClosed || !sampled.CallbacksJoined || sampled.Accepted != sampled.Settled || sampled.UnresolvedPublications != 0 {
			return errors.New("manual P4不是同run已关闭的零UNKNOWN原件")
		}
		key := string(sampled.Assignments[0].IssuerId) + string(sampled.Assignments[0].InstanceId)
		published := observed[key]
		if published == nil {
			return errors.New("manual P4发布遗漏采样boot")
		}
		comparison := proto.Clone(published).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
		withoutLeader := []*databasepb.SqlWriterMaintenanceReceipt_SourcePin{}
		leaderCount := 0
		for _, pin := range comparison.SourcePins {
			if proto.Equal(pin, leaderPin) {
				leaderCount++
			} else {
				withoutLeader = append(withoutLeader, pin)
			}
		}
		comparison.SourcePins = withoutLeader
		if leaderCount != 1 || !proto.Equal(comparison, sampled) {
			return errors.New("manual P4正文不是实际leader原件")
		}
		delete(observed, key)
		p3Pin := sampled.SourcePins[0]
		if p3Pin == nil || filepath.Base(p3Pin.AbsolutePath) != "manual-closure.report.json" || seenP3[p3Pin.AbsolutePath] {
			return errors.New("manual P3文件不是固定唯一原件")
		}
		seenP3[p3Pin.AbsolutePath] = true
		p3Raw, err := drainproof.ReadPin(p3Pin, 16<<20)
		if err != nil {
			return err
		}
		closed, err := parseManualEvidence(p3Raw)
		if err != nil {
			return err
		}
		if !bytes.Equal(closed.OperationId, sampled.OperationId) || !bytes.Equal(closed.ManifestDigest, sampled.ManifestDigest) || !closed.AcceptingClosed || !closed.CallbacksJoined || closed.Accepted != sampled.Accepted || closed.Settled != sampled.Settled || closed.UnresolvedPublications != 0 || len(closed.Assignments) != len(sampled.Assignments) {
			return errors.New("manual P3原件没有同boot真实闭合")
		}
		source := sources[string(closed.Assignments[0].IssuerId)]
		if source == nil || !bytes.Equal(source.InstanceId, closed.Assignments[0].InstanceId) || !authorityPinSetEqual(source.SourceEvidence, closed.SourcePins) {
			return errors.New("manual P3身份/配置证据不属于P0 SOURCE")
		}
		fixedDirectory := false
		for _, pin := range source.SourceEvidence {
			if filepath.Clean(filepath.Dir(pin.AbsolutePath)) == filepath.Clean(filepath.Dir(p3Pin.AbsolutePath)) {
				fixedDirectory = true
			}
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		if !fixedDirectory {
			return errors.New("manual P3离开P0固定私有目录")
		}
		priorRows := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
		for _, row := range closed.Assignments {
			rowKey := manualAssignmentKey(row)
			if priorRows[rowKey] != nil {
				return errors.New("manual P3分区重复")
			}
			priorRows[rowKey] = row
		}
		for _, row := range sampled.Assignments {
			rowKey := manualAssignmentKey(row)
			base, p3 := expected[rowKey], priorRows[rowKey]
			if base == nil || p3 == nil || seen[rowKey] || !bytes.Equal(p3.IssuerId, source.IssuerId) || !bytes.Equal(p3.InstanceId, source.InstanceId) || p3.ObservedMs < base.ObservedMs || !sameManualConfiguration(base, p3) || !bytes.Equal(base.InstanceId, p3.InstanceId) || p3.AssignmentRevision != base.AssignmentRevision || p3.StartOffset != base.StartOffset || p3.Position < base.Position || p3.Position != p3.ExclusiveEnd || p3.Earliest < base.Earliest || p3.Earliest > base.Position || p3.ObservedMs > leader.ObservedStartMS || row.ObservedMs < leader.ObservedStartMS || row.ObservedMs > leader.ObservedEndMS || row.Earliest < p3.Earliest || row.Earliest > base.Position || row.Position != row.ExclusiveEnd {
				return errors.New("manual P3/P4物理身份、位置、保留范围或观察窗口不符")
			}
			stable := proto.Clone(row).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
			stable.Earliest = p3.Earliest
			stable.ExclusiveEnd = p3.ExclusiveEnd
			stable.ObservedMs = p3.ObservedMs
			if !proto.Equal(stable, p3) {
				return errors.New("manual P4修改了P3原生赋值/位置")
			}
			seen[rowKey] = true
		}
	}
	if len(observed) != 0 || len(seen) != len(expected) {
		return errors.New("manual P4没有覆盖P0全部owner")
	}
	return nil
}

func verifyAuthorityKafka(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	topics := map[string]bool{}
	groups := map[string]bool{}
	for _, row := range plan.KafkaPartitions {
		expected[fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)] = row
		topics[row.TopicName] = true
		groups[row.Group] = true
	}
	for _, row := range plan.ManualAssignments {
		if row == nil {
			return errors.New("manual P0空分区")
		}
		topics[row.TopicName] = true
	}
	if len(actual.KafkaPartitions) != len(expected) {
		return errors.New("Kafka历史分区数量不等")
	}
	for _, row := range actual.KafkaPartitions {
		if row == nil {
			return errors.New("Kafka空分区原件")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		base := expected[key]
		if base == nil || !bytes.Equal(base.ClusterId, row.ClusterId) || !bytes.Equal(base.TopicId, row.TopicId) || base.InitialEarliest != row.InitialEarliest || base.ProtectedCommittedFloor != row.ProtectedCommittedFloor || row.FinalEarliest < row.InitialEarliest || row.FinalEarliest > row.ProtectedCommittedFloor || row.FinalCommitted != row.FinalExclusiveEnd || row.FinalCommitted < row.ProtectedCommittedFloor {
			return errors.New("Kafka原UUID/保留floor/固定末水位不符")
		}
		delete(expected, key)
	}
	var baseline []byte
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			if baseline != nil {
				return errors.New("P0 Kafka目录原件重复")
			}
			baseline = raw
		}
	}
	if baseline == nil {
		return errors.New("P0缺broker/有效retention完整目录")
	}
	baselineDigest, err := kafkautil.StableDrainCatalogDigest(baseline)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual.HistoricalInventoryDigest, baselineDigest) {
		return errors.New("nonSQL历史目录digest不是原完整稳定目录")
	}
	var catalog kafkautil.DrainKafkaCatalog
	if json.Unmarshal(baseline, &catalog) != nil || len(catalog.BrokerConfigs) == 0 {
		return errors.New("P0 Kafka目录损坏")
	}
	if _, err := authorityKafkaBaseline(plan); err != nil {
		return err
	}
	names := []string{"__consumer_offsets"}
	for topic := range topics {
		names = append(names, topic)
	}
	if !releaseExactStrings(catalog.TopicNames, names) {
		return errors.New("P0目录没有全部历史业务topic")
	}
	groupNames := []string{}
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	if !releaseExactStrings(catalog.GroupNames, groupNames) {
		return errors.New("P0目录没有全部历史消费组")
	}
	for _, row := range plan.KafkaPartitions {
		if !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) {
			return errors.New("P0实际目录与typed历史UUID不符")
		}
	}
	observed := 0
	seenPins := map[string]bool{}
	for _, pin := range actual.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			digest, digestErr := kafkautil.StableDrainCatalogDigest(raw)
			var captured kafkautil.DrainKafkaCatalog
			decodeErr := json.Unmarshal(raw, &captured)
			if seenPins[pin.AbsolutePath] || digestErr != nil || decodeErr != nil || !bytes.Equal(digest, baselineDigest) || captured.ObservedStartMS < actual.ObservedStartMs || captured.ObservedEndMS > actual.ObservedEndMs {
				return errors.New("Kafka首尾目录/broker/retention变化、重复原件或旧窗口")
			}
			seenPins[pin.AbsolutePath] = true
			observed++
		}
	}
	wantedCatalogs := 2
	if len(plan.ManualAssignments) > 0 {
		wantedCatalogs = 4
	}
	if observed != wantedCatalogs {
		return errors.New("Kafka缺本窗group/manual各自首尾真实目录")
	}
	return verifyManualClosure(plan, actual)
}

// authorityRedisPatterns 是 P4 要扫完的键族。存盘直接提交 TiDB 之后 Redis 上没有待投队列,只剩交接资格 hash 要核。
var authorityRedisPatterns = []string{"player:{*}:handoff_fence"}

type authorityRedisScanPage struct {
	Pattern string   `json:"pattern"`
	Input   uint64   `json:"input_cursor"`
	Output  uint64   `json:"output_cursor"`
	Keys    []string `json:"keys"`
}
type authorityRedisScan struct {
	ObservedStartMS   uint64                   `json:"observed_start_ms"`
	ObservedEndMS     uint64                   `json:"observed_end_ms"`
	Schema            string                   `json:"schema"`
	Role              string                   `json:"role"`
	Address           string                   `json:"address"`
	DB                int32                    `json:"db"`
	RunID             []byte                   `json:"server_run_id"`
	NonceKey          string                   `json:"nonce_key"`
	NonceBefore       []byte                   `json:"nonce_before"`
	NonceAfter        []byte                   `json:"nonce_after"`
	NoncePTTLBeforeMS int64                    `json:"nonce_pttl_before_ms"`
	NoncePTTLAfterMS  int64                    `json:"nonce_pttl_after_ms"`
	Pages             []authorityRedisScanPage `json:"pages,omitempty"`
	// Cluster 只有 Redis Cluster 目标才有,核对规则见 redis_cluster.go。
	Cluster *authorityRedisClusterEvidence `json:"cluster,omitempty"`
}

func verifyRedisNonceObservation(source *authorityRedisScan, target *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure, runID []byte) error {
	key, err := drainproof.RedisMaintenanceNonceKey(runID)
	if err != nil {
		return err
	}
	if source == nil || target == nil || len(target.MaintenanceNonce) != 16 || !redisNonceKeyMatches(source, key) || source.NoncePTTLBeforeMS != -1 || source.NoncePTTLAfterMS != -1 || !bytes.Equal(source.NonceBefore, target.MaintenanceNonce) || !bytes.Equal(source.NonceAfter, target.MaintenanceNonce) {
		return errors.New("Redis维护nonce缺失、变化或有TTL，不能用输入副本代替实际读回")
	}
	return nil
}
func verifyAuthorityRedisBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure{}
	for _, target := range plan.RedisTargets {
		expected[fmt.Sprintf("%s/%d", target.Address, target.Db)] = target
	}
	clusterViews := []*authorityRedisClusterEvidence{}
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.Schema != "SQL_DRAIN_REDIS_BASELINE_V1" {
			continue
		}
		var source authorityRedisScan
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&source) != nil || source.ObservedStartMS == 0 || source.ObservedEndMS < source.ObservedStartMS || source.ObservedEndMS-source.ObservedStartMS > 60000 || len(source.Pages) != 0 {
			return errors.New("Redis P0基线格式/窗口无效")
		}
		key := fmt.Sprintf("%s/%d", source.Address, source.DB)
		target := expected[key]
		if target == nil || source.Role != target.Role || !bytes.Equal(source.RunID, target.ServerRunId) {
			return errors.New("Redis P0原件重复或不属于固定物理目标")
		}
		if err := verifyRedisNonceObservation(&source, target, plan.RunId); err != nil {
			return err
		}
		delete(expected, key)
		if source.Cluster != nil {
			clusterViews = append(clusterViews, source.Cluster)
		}
	}
	if len(expected) != 0 {
		return errors.New("Redis P0缺实际单次nonce读回基线")
	}
	return verifyAuthorityRedisClusterViews(clusterViews)
}

func authorityScanKeys(target *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure, startMS, endMS uint64, runID []byte) (map[string]bool, error) {
	keys, _, err := readAuthorityRedisScan(target, startMS, endMS, runID)
	return keys, err
}

// readAuthorityRedisScan 读一个目标的 SCAN 原件,返回它列出的键,以及 Cluster 目标的拓扑读数(单实例为 nil)。
func readAuthorityRedisScan(target *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure, startMS, endMS uint64, runID []byte) (map[string]bool, *authorityRedisClusterEvidence, error) {
	if len(target.ScanSourceEvidence) != 1 {
		return nil, nil, errors.New("Redis目标必须恰有完整SCAN原件")
	}
	raw, err := drainproof.ReadPin(target.ScanSourceEvidence[0], 16<<20)
	if err != nil {
		return nil, nil, err
	}
	var source authorityRedisScan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&source) != nil || source.Schema != "SQL_DRAIN_REDIS_SCAN_V2" || source.ObservedStartMS < startMS || source.ObservedEndMS < source.ObservedStartMS || source.ObservedEndMS > endMS || source.ObservedEndMS-source.ObservedStartMS > 60000 || source.Role != target.Role || source.Address != target.Address || source.DB != target.Db || !bytes.Equal(source.RunID, target.ServerRunId) || len(source.Pages) == 0 || len(source.Pages) > 16384 {
		return nil, nil, errors.New("Redis SCAN实际目标/epoch/格式不符")
	}
	if err := verifyRedisNonceObservation(&source, target, runID); err != nil {
		return nil, nil, err
	}
	pages := map[string][]authorityRedisScanPage{}
	for _, page := range source.Pages {
		known := false
		for _, pattern := range authorityRedisPatterns {
			if page.Pattern == pattern {
				known = true
			}
		}
		if !known {
			return nil, nil, errors.New("Redis SCAN未知pattern")
		}
		pages[page.Pattern] = append(pages[page.Pattern], page)
	}
	keys := map[string]bool{}
	for _, pattern := range authorityRedisPatterns {
		cursor := uint64(0)
		list := pages[pattern]
		if len(list) == 0 {
			return nil, nil, errors.New("Redis SCAN缺pattern")
		}
		for i, page := range list {
			if page.Input != cursor || i < len(list)-1 && page.Output == 0 {
				return nil, nil, errors.New("Redis SCAN游标链中断或提前结束")
			}
			for _, key := range page.Keys {
				matched, err := filepath.Match(pattern, key)
				if err != nil || !matched {
					return nil, nil, errors.New("Redis SCAN页含不匹配原key")
				}
				keys[key] = true
				if len(keys) > 16384 {
					return nil, nil, errors.New("Redis SCAN key超过16K")
				}
			}
			cursor = page.Output
		}
		if cursor != 0 {
			return nil, nil, errors.New("Redis SCAN未读到cursor0")
		}
	}
	return keys, source.Cluster, nil
}
func verifyAuthorityRedis(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure{}
	for _, target := range plan.RedisTargets {
		expected[fmt.Sprintf("%s/%d", target.Address, target.Db)] = target
	}
	if len(actual.RedisTargets) != len(expected) {
		return errors.New("Redis目标全集不等")
	}
	clusterViews := []*authorityRedisClusterEvidence{}
	for _, target := range actual.RedisTargets {
		if target == nil {
			return errors.New("Redis空目标")
		}
		key := fmt.Sprintf("%s/%d", target.Address, target.Db)
		base := expected[key]
		if base == nil || base.Role != target.Role || !bytes.Equal(base.ServerRunId, target.ServerRunId) || !bytes.Equal(base.MaintenanceNonce, target.MaintenanceNonce) || !releaseEqualZones(base.Zones, target.Zones) || len(target.Observations) > 16384 {
			return errors.New("Redis目标/原epoch/zone/nonce变化")
		}
		delete(expected, key)
		required, clusterView, err := readAuthorityRedisScan(target, actual.ObservedStartMs, actual.ObservedEndMs, plan.RunId)
		if err != nil {
			return err
		}
		if clusterView != nil {
			clusterViews = append(clusterViews, clusterView)
		}
		if len(required) > 16384 || len(target.Observations) != len(required) {
			return errors.New("Redis SCAN目录与观察非等集")
		}
		totalMembers := 0
		for _, o := range target.Observations {
			if o != nil {
				totalMembers += len(o.Members)
			}
			if totalMembers > 16384 {
				return errors.New("Redis目标原成员总量超过16K预算")
			}
			if o == nil || !required[o.Key] {
				return errors.New("Redis观察重复/未知key")
			}
			delete(required, o.Key)
			members, values := []string{}, []string{}
			for _, member := range o.Members {
				members = append(members, string(member))
			}
			for _, value := range o.Values {
				values = append(values, string(value))
			}
			sha := sha256.Sum256([]byte(o.Key))
			observation := maintenanceReleaseRedisKeyObservation{Key: o.Key, KeySHA: hex.EncodeToString(sha[:]), Kind: o.Kind, Topic: o.Topic, Type: o.RedisType, PTTL: o.PttlMs, Count: int64(o.Count), Members: members, Values: values, DumpSHA: hex.EncodeToString(o.DumpSha256)}
			if err := releaseRedisObservation(observation); err != nil {
				return err
			}
		}
		if len(required) != 0 {
			return errors.New("Redis完整目录遗漏")
		}
	}
	return verifyAuthorityRedisClusterViews(clusterViews)
}

// VerifyAuthorityReleaseReadiness 核释放原件绑定的原正常冻结、nonSQL 闭合和维护工具实际闭合,并确认 SQL 冻结资格还在。
// 路由表只有 SQL 一份,没有要比对的副本;不接受任意文件或「恢复完成」布尔作依据。
func VerifyAuthorityReleaseReadiness(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt) error {
	evidence := fact.GetEvidence().GetReleaseReadiness()
	if evidence == nil {
		return errors.New("release缺正式释放原件")
	}
	frozen, err := catalog.MaintenanceReceipt(ctx, evidence.OriginalFreezeReceiptId)
	if err != nil {
		return err
	}
	closed, err := catalog.MaintenanceReceipt(ctx, evidence.NonSqlClosedReceiptId)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(fact.PlanProto)
	if frozen.Kind != databasepb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != databasepb.SqlWriterMaintenanceReceipt_FROZEN || frozen.RunPhase != databasepb.SqlWriterDrainRun_SQL_FROZEN || !bytes.Equal(frozen.ReceiptDigest, evidence.OriginalFreezeReceiptDigest) || !bytes.Equal(closed.ReceiptDigest, evidence.NonSqlClosedReceiptDigest) || closed.RunPhase != databasepb.SqlWriterDrainRun_NON_SQL_CLOSED || frozen.AttemptRevision != fact.AttemptRevision || closed.AttemptRevision != fact.AttemptRevision || !bytes.Equal(frozen.RunId, plan.RunId) || !bytes.Equal(closed.RunId, plan.RunId) || frozen.Evidence == nil || closed.Evidence == nil || !bytes.Equal(frozen.Evidence.ManifestDigest, sum[:]) || !bytes.Equal(closed.Evidence.ManifestDigest, sum[:]) {
		return errors.New("release未绑定原正常freeze/nonSQL SQL收据")
	}
	if len(plan.MaintenanceIssuers) == 0 {
		if evidence.MaintenancePublication != nil {
			return errors.New("P0无维护工具，不能在P7增补")
		}
	} else {
		if err := verifyAuthorityPublicationOriginals(plan.MaintenanceIssuers, evidence.MaintenancePublication); err != nil {
			return err
		}
		if err := verifyAuthorityRoles(plan.MaintenanceIssuers, evidence.MaintenancePublication.LogicalRoles); err != nil {
			return err
		}
		for _, tool := range evidence.MaintenancePublication.LogicalRoles {
			if tool.AcceptingClosedMs < frozen.CommittedMs {
				return errors.New("维护工具闭合早于原正常冻结")
			}
		}
	}
	return verifyRecoveryFrozenState(ctx, catalog, plan, fact, frozen)
}

func verifyRecoveryFrozenState(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact, frozen *databasepb.SqlWriterMaintenanceReceipt) error {
	run, err := catalog.DrainRun(ctx, plan.RunId)
	if err != nil {
		return err
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		return err
	}
	if (run.Phase != databasepb.SqlWriterDrainRun_SQL_FROZEN && run.Phase != databasepb.SqlWriterDrainRun_RELEASE_READY) || !bytes.Equal(run.ManifestProto, fact.PlanProto) || !bytes.Equal(run.FrozenRequestId, frozen.RequestId) || run.Generation != fact.Generation || run.AttemptRevision != fact.AttemptRevision || meta.Writable != 0 || meta.WriterEpoch != frozen.AfterEpoch || meta.CatalogRevision != frozen.CatalogRevision || !bytes.Equal(meta.FreezeRunId, plan.RunId) {
		return errors.New("原SQL冻结资格变化")
	}
	return nil
}

// verifyConsumerReadiness 合并同组各个实际boot的分区；历史组是否重启由固定启动owner决定，
// 一旦声明要启动某组，其配置目录不能被分片报告缩小，所有实际分配的并集必须完整。
func verifyManualReadiness(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, boot *databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness, reports []*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, manifestDigest []byte) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	// 当前命令流每个SOURCE在同topic只有一个手动owner；新boot nodeID由etcd重新分配。
	for _, row := range plan.ManualAssignments {
		if bytes.Equal(row.IssuerId, boot.IssuerId) {
			if expected[row.TopicName] != nil {
				return errors.New("manual SOURCE同topic配置重复")
			}
			expected[row.TopicName] = row
		}
	}
	seen := map[string]bool{}
	for _, report := range reports {
		if report == nil || !bytes.Equal(report.OperationId, plan.RunId) || !bytes.Equal(report.ManifestDigest, manifestDigest) || !report.AcceptingClosed || !report.CallbacksJoined || report.Accepted != 0 || report.Settled != 0 || report.UnresolvedPublications != 0 || len(report.Assignments) == 0 {
			return errors.New("manual新boot不是实际暂停且零接收准备")
		}
		for _, pin := range report.SourcePins {
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		for _, row := range report.Assignments {
			if !validManualAssignment(row) {
				return errors.New("manual P8身份/位置无效")
			}
			prior := expected[row.TopicName]
			if prior == nil || seen[row.TopicName] || !bytes.Equal(row.IssuerId, boot.IssuerId) || !bytes.Equal(row.InstanceId, boot.InstanceId) || bytes.Equal(row.InstanceId, prior.InstanceId) || !bytes.Equal(row.ClusterId, prior.ClusterId) || !bytes.Equal(row.TopicId, prior.TopicId) || row.PartitionCount != prior.PartitionCount || row.Partition != row.TargetNodeId%row.PartitionCount || row.Position != row.StartOffset {
				return errors.New("manual新boot配置/原绝对起点/目标身份不匹配")
			}
			seen[row.TopicName] = true
		}
	}
	if len(seen) != len(expected) {
		return errors.New("manual新boot遗漏实际消费者")
	}
	return nil
}

func verifyConsumerReadiness(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, boots []*databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness, manifest []byte) error {
	type groupState struct {
		generation int32
		expected   map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure
		assigned   map[string]bool
		members    map[string]bool
	}
	groups := map[string]*groupState{}
	planned := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	key := func(row *databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) string {
		return fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
	}
	for _, row := range plan.KafkaPartitions {
		planned[key(row)] = row
	}
	sum := sha256.Sum256(manifest)
	for _, boot := range boots {
		if boot == nil || len(boot.ConsumerReadyFrontierDigest) != 32 {
			return errors.New("boot缺消费者就绪摘要")
		}
		var selected *databasepb.SqlWriterMaintenanceReceipt_SourcePin
		for _, pin := range boot.SourcePins {
			if pin != nil && bytes.Equal(pin.Sha256, boot.ConsumerReadyFrontierDigest) {
				if selected != nil {
					return errors.New("consumer就绪原件摘要绑定不唯一")
				}
				selected = pin
			}
		}
		if selected == nil {
			return errors.New("boot遗漏正式consumer就绪原件，空组也必须提供")
		}
		raw, err := drainproof.ReadPin(selected, 16<<20)
		if err != nil {
			return err
		}
		report, err := drainproof.ParseConsumerReadiness(raw)
		if err != nil {
			return errors.New("consumer就绪原件不是正式ConsumerReadiness")
		}
		if err := verifyManualReadiness(plan, boot, report.ManualAssignments, sum[:]); err != nil {
			return err
		}
		{
			seenGroups := map[string]bool{}
			for _, current := range report.Groups {
				if current == nil || len(current.ProtoReflect().GetUnknown()) != 0 || !bytes.Equal(current.OperationId, plan.RunId) || !bytes.Equal(current.ManifestDigest, sum[:]) || !bytes.Equal(current.IssuerId, boot.IssuerId) || !bytes.Equal(current.InstanceId, boot.InstanceId) || current.Group == "" || current.GenerationId < 0 || current.MemberId == "" || len(current.MemberId) > 1024 || seenGroups[current.Group] || len(current.ExpectedPartitions) == 0 || len(current.ExpectedPartitions) > 16384 {
					return errors.New("consumer代次原件身份/配置目录无效")
				}
				seenGroups[current.Group] = true
				wanted := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
				for _, part := range current.ExpectedPartitions {
					if part == nil || part.Group != current.Group || wanted[key(part)] != nil || !proto.Equal(part, planned[key(part)]) {
						return errors.New("consumer配置目录不属于原P0全集")
					}
					wanted[key(part)] = part
				}
				state := groups[current.Group]
				if state == nil {
					state = &groupState{generation: current.GenerationId, expected: wanted, assigned: map[string]bool{}, members: map[string]bool{}}
					groups[current.Group] = state
				} else {
					if state.generation != current.GenerationId || len(state.expected) != len(wanted) {
						return errors.New("consumer组代次或配置范围不一致")
					}
					for id, part := range wanted {
						if !proto.Equal(part, state.expected[id]) {
							return errors.New("consumer副本配置分区不一致")
						}
					}
				}
				if state.members[current.MemberId] {
					return errors.New("consumer成员重复报告")
				}
				state.members[current.MemberId] = true
				for _, part := range current.AssignedPartitions {
					if part == nil {
						return errors.New("consumer assignment为空")
					}
					original := wanted[key(part)]
					if original == nil || state.assigned[key(part)] || !bytes.Equal(original.ClusterId, part.ClusterId) || !bytes.Equal(original.TopicId, part.TopicId) || original.ProtectedCommittedFloor != part.ProtectedCommittedFloor || original.InitialEarliest != part.InitialEarliest || part.FinalEarliest < 0 || part.FinalEarliest > part.ProtectedCommittedFloor || part.FinalCommitted < part.ProtectedCommittedFloor || part.FinalExclusiveEnd < part.FinalCommitted {
						return errors.New("consumer分配重复、物理身份变化、保留缺口或提交前沿未知")
					}
					state.assigned[key(part)] = true
				}
			}
		}
	}
	for _, state := range groups {
		if len(state.assigned) != len(state.expected) {
			return errors.New("consumer组的实际分配并集遗漏配置分区")
		}
		for id := range state.expected {
			if !state.assigned[id] {
				return errors.New("consumer分区未实际准备")
			}
		}
	}
	return nil
}

// VerifyAuthorityPublicationReadiness核全体全新boot、永久REGISTER原收据及每个实际open ACK。
func VerifyAuthorityPublicationReadiness(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt) error {
	ready := fact.GetEvidence().GetPublicationReadiness()
	if ready == nil || len(ready.PreparedBoots) != len(plan.RootIssuers) || len(ready.SourceOpenAcks) != len(plan.RootIssuers) {
		return errors.New("publication readiness不是原逻辑root全集")
	}
	released, err := catalog.MaintenanceReceipt(ctx, ready.ReleaseReceiptId)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(fact.PlanProto)
	if released.Kind != databasepb.SqlWriterMaintenanceReceipt_RELEASE || released.Phase != databasepb.SqlWriterMaintenanceReceipt_RELEASED || released.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED || released.Evidence == nil || !bytes.Equal(released.RunId, plan.RunId) || !bytes.Equal(released.ReceiptDigest, ready.ReleaseReceiptDigest) || !bytes.Equal(released.Evidence.ManifestDigest, sum[:]) || released.AttemptRevision != fact.AttemptRevision {
		return errors.New("prepared/open未绑定原SQL_RELEASED收据")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	oldKeys := map[string]bool{}
	for _, role := range plan.RootIssuers {
		expected[string(role.IssuerId)] = role
	}
	for _, key := range plan.ActiveWriterKeys {
		oldKeys[string(key)] = true
	}
	boots := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness{}
	freshKeys := map[string]bool{}
	for _, boot := range ready.PreparedBoots {
		if boot == nil {
			return errors.New("prepared空原件")
		}
		prior := expected[string(boot.IssuerId)]
		if prior == nil || prior.LogicalRole != boot.LogicalRole || bytes.Equal(prior.InstanceId, boot.InstanceId) || len(boot.InstanceId) != 16 || boot.PreparedClosedMs < released.CommittedMs || boot.PreparedClosedMs > uint64(time.Now().UnixMilli()) || len(boot.ConsumerReadyFrontierDigest) != 32 || len(boot.SourcePins) == 0 || boots[string(boot.IssuerId)] != nil {
			return errors.New("prepared复用了旧boot/缺真实closed consumer前沿")
		}
		if len(prior.PhysicalWriterKey) != 0 {
			if len(boot.PhysicalWriterKey) != 16 || oldKeys[string(boot.PhysicalWriterKey)] || len(boot.RegistrationReceiptId) != 16 || len(boot.RegistrationReceiptDigest) != 32 {
				return errors.New("prepared复用旧物理writer或缺正式注册")
			}
			registration, err := catalog.LifecycleReceipt(ctx, boot.RegistrationReceiptId)
			if err != nil {
				return err
			}
			request := &databasepb.SqlWriterLifecycleReceipt_Request{}
			if registration.Kind != databasepb.SqlWriterLifecycleReceipt_REGISTER || !bytes.Equal(registration.RequestDigest, boot.RegistrationReceiptDigest) || proto.Unmarshal(registration.RequestProto, request) != nil || request.Writer == nil || !bytes.Equal(request.MaintenanceRunId, plan.RunId) || !bytes.Equal(request.Writer.WriterKey, boot.PhysicalWriterKey) || !bytes.Equal(request.Writer.InstanceId, boot.InstanceId) || request.Writer.State != databasepb.SqlWriterRegistry_ACTIVE || registration.ResultingEpoch != released.AfterEpoch {
				return errors.New("prepared注册不是本run发布后的全新同boot")
			}
			freshKeys[string(boot.PhysicalWriterKey)] = true
		} else if len(boot.PhysicalWriterKey) != 0 || len(boot.RegistrationReceiptId) != 0 || len(boot.RegistrationReceiptDigest) != 0 {
			return errors.New("无SQL逻辑root伪填物理注册")
		}
		for _, pin := range boot.SourcePins {
			if _, err = drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		boots[string(boot.IssuerId)] = boot
		delete(expected, string(boot.IssuerId))
	}
	if len(expected) != 0 {
		return errors.New("prepared root遗漏")
	}
	if err := verifyConsumerReadiness(plan, ready.PreparedBoots, fact.PlanProto); err != nil {
		return err
	}
	active, err := catalog.ActiveWriterKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range active {
		if oldKeys[string(key)] || !freshKeys[string(key)] {
			return errors.New("发布前旧writer未退役或出现未审定物理writer")
		}
		delete(freshKeys, string(key))
	}
	if len(freshKeys) != 0 {
		return errors.New("prepared物理writer已不在ACTIVE目录")
	}
	opened := map[string]bool{}
	for _, pin := range ready.SourceOpenAcks {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		ack := &databasepb.SqlWriterMaintenanceReceipt_SourceOpenAck{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, ack) != nil {
			return errors.New("实际open ACK非正式proto")
		}
		boot := boots[string(ack.IssuerId)]
		if boot == nil || opened[string(ack.IssuerId)] || !bytes.Equal(ack.InstanceId, boot.InstanceId) || !bytes.Equal(ack.PhysicalWriterKey, boot.PhysicalWriterKey) || ack.LogicalRole != boot.LogicalRole || !bytes.Equal(ack.RegistrationReceiptId, boot.RegistrationReceiptId) || !bytes.Equal(ack.RegistrationReceiptDigest, boot.RegistrationReceiptDigest) || !bytes.Equal(ack.OperationId, plan.RunId) || !bytes.Equal(ack.ManifestDigest, sum[:]) || ack.OpenedMs < boot.PreparedClosedMs || ack.OpenedMs > uint64(time.Now().UnixMilli()) || len(ack.OpenRequestId) != 16 || len(ack.AcceptedFrontierDigest) != 32 || len(ack.SourcePins) == 0 {
			return errors.New("实际open ACK换boot/run/注册或无真正提交点")
		}
		for _, source := range ack.SourcePins {
			if _, err = drainproof.ReadPin(source, 16<<20); err != nil {
				return err
			}
		}
		opened[string(ack.IssuerId)] = true
	}
	return nil
}

// VerifyAuthorityEmergency只为显式危险HOLD核原件；不生成NON_SQL_CLOSED或正常FROZEN结论。
func VerifyAuthorityEmergency(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt, requirePrior bool) error {
	danger := fact.GetEvidence().GetEmergency()
	if danger == nil || strings.TrimSpace(danger.Reason) == "" || len(danger.Reason) > 2048 || len(danger.SourceEvidence) == 0 || len(danger.SourceEvidence) > 128 {
		return errors.New("紧急HOLD缺危险原因和原始证据")
	}
	for _, pin := range danger.SourceEvidence {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	if !requirePrior {
		return nil
	}
	prior, err := catalog.MaintenanceReceipt(ctx, danger.DangerReceiptId)
	if err != nil {
		return err
	}
	if !bytes.Equal(prior.RunId, plan.RunId) || !bytes.Equal(prior.ReceiptDigest, danger.DangerReceiptDigest) || prior.AttemptRevision != fact.AttemptRevision || prior.Evidence == nil || prior.Evidence.GetEmergency() == nil || !bytes.Equal(prior.Evidence.ManifestDigest, fact.Evidence.ManifestDigest) {
		return errors.New("紧急HOLD危险原收据被替换")
	}
	return nil
}

// manualAssignmentKey按逻辑SOURCE、consumer及分区标识配置；boot变化另按阶段显式校验。
func manualAssignmentKey(row *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) string {
	return fmt.Sprintf("%x/%q/%q/%d", row.IssuerId, row.ConsumerId, row.TopicName, row.Partition)
}
func validManualAssignment(row *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	return row != nil && len(row.ProtoReflect().GetUnknown()) == 0 && len(row.IssuerId) == 16 && len(row.InstanceId) == 16 && len(row.ConsumerId) > 0 && len(row.ConsumerId) <= 256 && !strings.ContainsAny(row.ConsumerId, "\x00\r\n") && len(row.ClusterId) > 0 && len(row.TopicId) == 16 && row.TopicName != "" && row.PartitionCount > 0 && row.PartitionCount <= 4096 && row.Partition < row.PartitionCount && row.TargetNodeId > 0 && row.Partition == row.TargetNodeId%row.PartitionCount && bytes.Equal(row.TargetInstanceId, row.InstanceId) && row.AssignmentRevision > 0 && row.StartOffset >= 0 && row.Position >= row.StartOffset && row.Earliest >= 0 && row.Earliest <= row.Position && row.ExclusiveEnd >= row.Position && row.ObservedMs > 0
}
func sameManualConfiguration(a, b *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	return a != nil && b != nil && bytes.Equal(a.IssuerId, b.IssuerId) && a.ConsumerId == b.ConsumerId && bytes.Equal(a.ClusterId, b.ClusterId) && bytes.Equal(a.TopicId, b.TopicId) && a.TopicName == b.TopicName && a.PartitionCount == b.PartitionCount && a.Partition == b.Partition && a.TargetNodeId == b.TargetNodeId
}
func parseManualEvidence(raw []byte) (*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, error) {
	if len(raw) == 0 || len(raw) > 16<<20 {
		return nil, errors.New("manual原件大小无效")
	}
	proof := &databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, proof) != nil {
			return nil, errors.New("manual原件不是正式JSON")
		}
	} else if proto.Unmarshal(raw, proof) != nil {
		return nil, errors.New("manual原件不是正式二进制")
	}
	if len(proof.ProtoReflect().GetUnknown()) != 0 || len(proof.Assignments) == 0 || len(proof.Assignments) > 16384 || len(proof.SourcePins) > 128 {
		return nil, errors.New("manual原件为空或超预算")
	}
	for _, row := range proof.Assignments {
		if !validManualAssignment(row) {
			return nil, errors.New("manual原件身份/位置无效")
		}
	}
	for _, pin := range proof.SourcePins {
		if pin == nil || len(pin.ProtoReflect().GetUnknown()) != 0 {
			return nil, errors.New("manual证据pin无效")
		}
	}
	return proof, nil
}
func verifyManualBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, catalog *kafkautil.DrainKafkaCatalog) error {
	if len(plan.ManualAssignments) > 16384 {
		return errors.New("manual P0目录超预算")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	sources := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, source := range plan.RootIssuers {
		sources[string(source.IssuerId)] = source
	}
	for _, row := range plan.ManualAssignments {
		if !validManualAssignment(row) {
			return errors.New("manual P0身份/位置无效")
		}
		source := sources[string(row.IssuerId)]
		if source == nil || !bytes.Equal(source.InstanceId, row.InstanceId) || !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) || int(row.PartitionCount) != catalog.TopicPartitions[row.TopicName] {
			return errors.New("manual P0不属于实际SOURCE或物理topic目录")
		}
		key := manualAssignmentKey(row)
		if expected[key] != nil {
			return errors.New("manual P0重复配置")
		}
		expected[key] = row
	}
	seen := map[string]bool{}
	leaderBaselines := 0
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_MANUAL_KAFKA_BASELINE_V1" {
			leaderBaselines++
			if leaderBaselines != 1 || len(expected) == 0 {
				return errors.New("manual P0 leader基线重复或没有实际owner")
			}
			captured := &kafkautil.DrainManualKafkaBaseline{}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(captured) != nil || captured.CatalogSource == nil || captured.ObservedStartMS == 0 || captured.ObservedEndMS < captured.ObservedStartMS || captured.ObservedEndMS-captured.ObservedStartMS > 60000 || len(captured.Assignments) != len(expected) || !bytes.Equal(captured.CatalogSHA256, captured.CatalogSource.Sha256) {
				return errors.New("manual P0 leader基线格式/窗口无效")
			}
			actualCatalog, err := drainproof.ReadPin(captured.CatalogSource, 16<<20)
			if err != nil {
				return err
			}
			actualDigest, err := kafkautil.StableDrainCatalogDigest(actualCatalog)
			if err != nil {
				return err
			}
			mainRaw, err := json.Marshal(catalog)
			if err != nil {
				return err
			}
			mainDigest, err := kafkautil.StableDrainCatalogDigest(mainRaw)
			if err != nil || !bytes.Equal(actualDigest, mainDigest) {
				return errors.New("manual P0物理目录与group目录不同")
			}
			sampled := map[string]bool{}
			for _, encoded := range captured.Assignments {
				row := &databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
				if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(encoded, row) != nil || !validManualAssignment(row) {
					return errors.New("manual P0 leader分区无效")
				}
				key := manualAssignmentKey(row)
				base := expected[key]
				if base == nil || sampled[key] || row.Earliest < base.Earliest || row.Earliest > base.Position || row.ExclusiveEnd < base.ExclusiveEnd || row.ObservedMs < captured.ObservedStartMS || row.ObservedMs > captured.ObservedEndMS || row.ObservedMs < base.ObservedMs {
					return errors.New("manual P0 leader保留范围变化或分区缺失")
				}
				stable := proto.Clone(row).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
				stable.Earliest = base.Earliest
				stable.ExclusiveEnd = base.ExclusiveEnd
				stable.ObservedMs = base.ObservedMs
				if !proto.Equal(stable, base) {
					return errors.New("manual P0 leader采样替换了原生position/配置")
				}
				sampled[key] = true
			}
			continue
		}
		proof, err := parseManualEvidence(raw)
		if err != nil {
			continue
		}
		if !bytes.Equal(proof.OperationId, plan.RunId) || len(proof.ManifestDigest) != 0 {
			return errors.New("manual P0原件换run或形成manifest自引用")
		}
		source := sources[string(proof.Assignments[0].IssuerId)]
		if filepath.Base(pin.AbsolutePath) != "manual-baseline.report.json" || source == nil || !authorityPinSetEqual(source.SourceEvidence, proof.SourcePins) {
			return errors.New("manual P0原件未绑定固定SOURCE证据")
		}
		fixedDirectory := false
		for _, sourcePin := range source.SourceEvidence {
			if filepath.Clean(filepath.Dir(sourcePin.AbsolutePath)) == filepath.Clean(filepath.Dir(pin.AbsolutePath)) {
				fixedDirectory = true
			}
		}
		if !fixedDirectory {
			return errors.New("manual P0原件换私有目录")
		}
		for _, row := range proof.Assignments {
			if !bytes.Equal(row.IssuerId, source.IssuerId) || !bytes.Equal(row.InstanceId, source.InstanceId) {
				return errors.New("manual P0一个原件混入多个boot")
			}
		}
		for _, row := range proof.Assignments {
			key := manualAssignmentKey(row)
			if seen[key] || !proto.Equal(row, expected[key]) {
				return errors.New("manual P0配置不是实际owner原件或重复")
			}
			seen[key] = true
		}
		for _, source := range proof.SourcePins {
			if _, err := drainproof.ReadPin(source, 16<<20); err != nil {
				return err
			}
		}
	}
	if len(seen) != len(expected) || len(expected) > 0 && leaderBaselines != 1 {
		return errors.New("manual P0缺实际owner或leader基线原件")
	}
	return nil
}

func authorityKafkaBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) (*kafkautil.DrainKafkaCatalog, error) {
	var catalog *kafkautil.DrainKafkaCatalog
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return nil, err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.Schema != "SQL_DRAIN_KAFKA_CATALOG_V2" {
			continue
		}
		if catalog != nil {
			return nil, errors.New("P0 Kafka原目录重复")
		}
		catalog = &kafkautil.DrainKafkaCatalog{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(catalog) != nil {
			return nil, errors.New("P0 Kafka目录schema未知")
		}
	}
	if catalog == nil || catalog.ObservedStartMS == 0 || catalog.ObservedEndMS < catalog.ObservedStartMS || catalog.ObservedEndMS-catalog.ObservedStartMS > 60000 || len(catalog.ClusterID) == 0 || len(catalog.TopicNames) == 0 || len(catalog.TopicNames) > 129 || len(catalog.TopicIDs) != len(catalog.TopicNames) || len(catalog.TopicPartitions) != len(catalog.TopicNames) || len(catalog.TopicConfigs) != len(catalog.TopicNames) || len(catalog.BrokerConfigs) == 0 || len(catalog.BrokerConfigs) > 128 {
		return nil, errors.New("P0 Kafka目录/有效保留配置全集缺失")
	}
	topics := map[string]bool{}
	groups := map[string]bool{}
	subscriptions := map[string]map[uint32]bool{}
	for _, row := range plan.KafkaPartitions {
		if !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) {
			return nil, errors.New("P0 typed分区与实际UUID不符")
		}
		topics[row.TopicName] = true
		groups[row.Group] = true
		key := row.Group + "\x00" + row.TopicName
		if subscriptions[key] == nil {
			subscriptions[key] = map[uint32]bool{}
		}
		subscriptions[key][row.Partition] = true
	}
	for _, row := range plan.ManualAssignments {
		if row == nil {
			return nil, errors.New("manual P0空分区")
		}
		topics[row.TopicName] = true
	}
	names := []string{"__consumer_offsets"}
	for topic := range topics {
		names = append(names, topic)
	}
	if !releaseExactStrings(names, catalog.TopicNames) {
		return nil, errors.New("P0漏历史topic或未知业务topic")
	}
	groupNames := []string{}
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	if !releaseExactStrings(groupNames, catalog.GroupNames) {
		return nil, errors.New("P0漏历史group")
	}
	for key, partitions := range subscriptions {
		parts := strings.SplitN(key, "\x00", 2)
		n := catalog.TopicPartitions[parts[1]]
		if n < 1 || n > 4096 || len(partitions) != n {
			return nil, errors.New("P0订阅partition不是实际全量")
		}
		for p := 0; p < n; p++ {
			if !partitions[uint32(p)] {
				return nil, errors.New("P0分区不连续/缺失")
			}
		}
	}
	expectedTopicConfig := []string{"retention.ms", "retention.bytes", "cleanup.policy", "segment.ms", "segment.bytes", "delete.retention.ms", "min.insync.replicas", "unclean.leader.election.enable"}
	expectedBrokerConfig := []string{"log.retention.ms", "log.retention.bytes", "log.segment.bytes", "log.retention.check.interval.ms", "unclean.leader.election.enable", "address"}
	keys := func(m map[string]string) []string {
		out := []string{}
		for key, value := range m {
			if value == "" {
				return nil
			}
			out = append(out, key)
		}
		return out
	}
	for _, topic := range catalog.TopicNames {
		if len(catalog.TopicIDs[topic]) != 16 || catalog.TopicPartitions[topic] < 1 || !releaseExactStrings(keys(catalog.TopicConfigs[topic]), expectedTopicConfig) {
			return nil, errors.New("P0 topic UUID/有效retention配置缺项")
		}
		if topic != "__consumer_offsets" && catalog.TopicConfigs[topic]["cleanup.policy"] != "delete" {
			return nil, errors.New("P0业务topic可压缩删除，不能证明完整排空")
		}
	}
	for id, config := range catalog.BrokerConfigs {
		n, err := strconv.ParseInt(id, 10, 32)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != id || !releaseExactStrings(keys(config), expectedBrokerConfig) || !maintenanceAddress(config["address"]) {
			return nil, errors.New("P0 broker/有效retention配置缺项")
		}
	}
	var baseline *kafkautil.DrainKafkaBaseline
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return nil, err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_BASELINE_V2" {
			if baseline != nil {
				return nil, errors.New("P0 C0/L0原件重复")
			}
			baseline = &kafkautil.DrainKafkaBaseline{}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(baseline) != nil {
				return nil, errors.New("P0 C0/L0原件损坏")
			}
		}
	}
	if baseline == nil || baseline.ObservedMS == 0 || len(baseline.Partitions) != len(plan.KafkaPartitions) {
		return nil, errors.New("P0缺实际固定C0/L0分区全原件")
	}
	catalogRaw, _ := json.Marshal(catalog)
	catalogSum := sha256.Sum256(catalogRaw)
	if !bytes.Equal(catalogSum[:], baseline.CatalogSHA256) {
		return nil, errors.New("P0 C0/L0未绑定同目录")
	}
	observed := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	for _, row := range baseline.Partitions {
		if row == nil {
			return nil, errors.New("P0空C0/L0原件")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		if observed[key] != nil {
			return nil, errors.New("P0 C0/L0分区重复")
		}
		observed[key] = row
	}
	for _, row := range plan.KafkaPartitions {
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		base := observed[key]
		if base == nil || !bytes.Equal(base.ClusterId, row.ClusterId) || !bytes.Equal(base.TopicId, row.TopicId) || base.InitialEarliest != row.InitialEarliest || base.ProtectedCommittedFloor != row.ProtectedCommittedFloor {
			return nil, errors.New("P0 typed C0/L0不是实际原件")
		}
		if base.FinalCommitted != base.ProtectedCommittedFloor && !(base.FinalCommitted == -1 && base.InitialEarliest == 0 && base.ProtectedCommittedFloor == 0 && base.FinalExclusiveEnd == 0) {
			return nil, errors.New("P0实际committed不构成保护floor")
		}
		delete(observed, key)
	}
	if err := verifyManualBaseline(plan, catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

func authorityPinSetEqual(expected, actual []*databasepb.SqlWriterMaintenanceReceipt_SourcePin) bool {
	if len(expected) != len(actual) {
		return false
	}
	set := map[string][]byte{}
	for _, pin := range expected {
		if pin == nil || set[pin.AbsolutePath] != nil {
			return false
		}
		set[pin.AbsolutePath] = pin.Sha256
	}
	for _, pin := range actual {
		if pin == nil || !bytes.Equal(set[pin.AbsolutePath], pin.Sha256) {
			return false
		}
		delete(set, pin.AbsolutePath)
	}
	return len(set) == 0
}
func verifyAuthorityPublicationOriginals(seeds []*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure, publication *databasepb.SqlWriterMaintenanceReceipt_PublicationEvidence) error {
	if publication == nil || len(publication.SourceEvidence) != len(seeds) {
		return errors.New("角色阶段缺实际app报告原件全集")
	}
	inventory := &databasepb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: seeds}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(inventory)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], publication.IssuerInventoryDigest) {
		return errors.New("角色阶段issuer inventory摘要偏离P0完整原序")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, role := range publication.LogicalRoles {
		if role == nil || expected[string(role.IssuerId)] != nil {
			return errors.New("角色阶段实际issuer重复")
		}
		expected[string(role.IssuerId)] = role
	}
	seenPins := map[string]bool{}
	for _, pin := range publication.SourceEvidence {
		if pin == nil || seenPins[pin.AbsolutePath] {
			return errors.New("app报告原件重复")
		}
		seenPins[pin.AbsolutePath] = true
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		original := &databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, original) != nil || !proto.Equal(original, expected[string(original.IssuerId)]) {
			return errors.New("阶段closure不是该app原报告完整正文")
		}
		delete(expected, string(original.IssuerId))
	}
	if len(expected) != 0 {
		return errors.New("app原报告遗漏")
	}
	return nil
}
