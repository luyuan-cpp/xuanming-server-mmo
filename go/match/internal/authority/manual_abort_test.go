package authority

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	battlepb "proto/battle"
	pb "proto/match"
)

// 2026-11-05 人工处置工具(manual_abort.go,手册 docs/ops/match-formation-manual-abort-runbook.md)的回归:
//   - supportedOutcomeOriginal 拆成逐项原因码后,放行与拒绝和拆分前逐字相同;
//   - 放行判据 assessManualAbort:原件合格的局归自动路径、已有任何终局行、截止加宽限未到、租约未到期都拒绝;
//   - 真 TiDB:永久 UNKNOWN 的成局经人工 ABORT 恰好释放一次(同一事务写审计行),演练不改行,重放回已处理,
//     摘要不符拒绝,多位操作员与自动恢复、帮会取消并发时只落一份终局;活动局之后的帮会取消签 formation=5 / ABORT 回执;
//   - 评审补(文件末尾):审计行写失败时 ABORT 一起回滚;缺票行、原票已关闭各回自己的拒绝码而不是「没有这一局」;
//     成局行解不开与读库失败分开报;非 v1 成局被恢复循环反复续租、工具处置不了;operation_mismatch 处置后 battle 的迟到提交仍冲突。
// 纯函数不碰 SQL;真 TiDB 沿用 MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB 与 formation_recovery_exit_test.go 的夹具,
// 不依赖真实墙钟:截止与租约都用数据库当前时刻前后的固定偏移注入。永久不可裁决的原件 SaveSupportedIntent 已拒收,
// 夹具用直接 SQL 写入(模拟数据损坏、手改或修复前落库的行)。

// ---- 纯函数 ----

// manualFormation 是纯函数用例的成局:state 3、持久 v1 原件、能力证据齐全、租约已到期。edit 在原件序列化之前改成局与原件。
func manualFormation(t *testing.T, edit func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)) *pb.MatchFormationRecord {
	t.Helper()
	f := pureIntentFormation()
	f.State, f.RetryAtMs = 3, 1
	f.OutcomeProtocolVersion, f.PreflightSupportedVersion, f.PreflightInstanceId = 1, 1, f.TargetInstanceId
	r := v1IntentRequest(f, f.CreatedMs, f.CreatedMs+30_000)
	if edit != nil {
		edit(f, r)
	}
	f.CreateRequest = marshalIntent(t, r)
	return f
}

// legacySupportedOutcomeOriginal 是 2026-11-05 拆分之前 supportedOutcomeOriginal 的整条合取式,只作对照:拆分后放行与否必须逐例相同。
func legacySupportedOutcomeOriginal(f *pb.MatchFormationRecord) bool {
	if f == nil || f.OutcomeProtocolVersion != 1 || f.PreflightSupportedVersion != 1 || f.TargetInstanceId == "" || f.PreflightInstanceId != f.TargetInstanceId || (f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 1) || (!f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 0) {
		return false
	}
	r := &battlepb.CreateBattleRequest{}
	return !(proto.Unmarshal(f.CreateRequest, r) != nil || r.OutcomeProtocolVersion != 1 || r.BattleId != f.BattleId || r.PlacementOperationId != f.OperationId || r.TargetInstanceId != f.TargetInstanceId || r.BattleConfigId != f.ConfigId || r.MatchMode != f.Mode || r.DeadlineMs == 0 || r.DeadlineMs != f.DeadlineMs || r.CreatedAtMs < f.CreatedMs || r.DeadlineMs <= r.CreatedAtMs || len(r.Players) != len(f.Members))
}

// 每一项不符各回一个原因码;能力证据类与原件正文类的错误分界不变;放行与否与拆分前的合取式逐例相同。
func TestSupportedOutcomeOriginalReasonCodes(t *testing.T) {
	type edit = func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)
	for _, c := range []struct {
		code       string
		capability bool
		edit       edit
	}{
		{"", false, nil},
		{"outcome_protocol", true, func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.OutcomeProtocolVersion = 0 }},
		{"preflight_version", true, func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.PreflightSupportedVersion = 2 }},
		{"target_instance_missing", true, func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) {
			f.TargetInstanceId, f.PreflightInstanceId, r.TargetInstanceId = "", "", ""
		}},
		{"preflight_instance_mismatch", true, func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.PreflightInstanceId = "other" }},
		{"adopted_legacy", true, func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.AdoptedOutcomeObserved = true }},
		{"adopted_invalid", true, func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.AdoptedOutcomeProtocolVersion = 1 }},
		{"create_protocol", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.OutcomeProtocolVersion = 0 }},
		{"battle_id_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.BattleId++ }},
		{"operation_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.PlacementOperationId = "other" }},
		{"target_instance_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.TargetInstanceId = "other" }},
		{"config_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.BattleConfigId++ }},
		{"mode_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.MatchMode++ }},
		{"deadline_missing", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.DeadlineMs = 0 }},
		{"deadline_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.DeadlineMs++ }},
		{"created_before_formation", false, func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.CreatedMs - 1 }},
		{"created_after_deadline", false, func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.DeadlineMs }},
		{"players_count_mismatch", false, func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.Players = r.Players[:1] }},
	} {
		t.Run(c.code, func(t *testing.T) {
			f := manualFormation(t, c.edit)
			_, err := supportedOutcomeOriginal(f)
			require.Equal(t, legacySupportedOutcomeOriginal(f), err == nil, "放行与否与拆分前相同")
			if c.code == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "("+c.code+")")
			if c.capability {
				require.ErrorContains(t, err, "旧局或原能力证据不完整")
			} else {
				require.ErrorContains(t, err, "持久创建原件与终局身份冲突")
			}
		})
	}
	undecodable := manualFormation(t, nil)
	undecodable.CreateRequest = []byte{0xff}
	_, err := supportedOutcomeOriginal(undecodable)
	require.ErrorContains(t, err, "(create_undecodable)")
	require.False(t, legacySupportedOutcomeOriginal(undecodable))
	_, err = supportedOutcomeOriginal(nil)
	require.ErrorContains(t, err, "(formation_missing)")
}

// 放行判据逐条:原件合格归自动路径;永久 UNKNOWN 的两类过了有效截止加宽限、租约到期才放行;有效截止取三者最大。
func TestManualAbortEligibilityTable(t *testing.T) {
	type edit = func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)
	skewed := func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.CreatedMs - 1 }
	base := pureIntentFormation()
	late := base.DeadlineMs + terminalOutcomeGraceMs // 恰好过成局截止加宽限
	for _, c := range []struct {
		why       string
		edit      edit
		post      func(f *pb.MatchFormationRecord)
		now       uint64
		class     pb.StrandedFormationClass
		check     string
		refusal   string // 空 = 放行
		effective uint64
	}{
		{why: "原件合格:归到期恢复 / 帮会取消", now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO, refusal: manualRefuseAutoRecoverable, effective: base.DeadlineMs},
		{why: "时钟偏差:原件早于成局", edit: skewed, now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", effective: base.DeadlineMs},
		{why: "宽限还差 1 毫秒", edit: skewed, now: late - 1, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseDeadlineNotDue, effective: base.DeadlineMs},
		{why: "原件截止更晚:房间按原件截止打,有效截止取它", edit: func(_ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.DeadlineMs += 1 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "deadline_mismatch", refusal: manualRefuseDeadlineNotDue, effective: base.DeadlineMs + 1},
		{why: "准入期限更晚:取准入期限", edit: func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { skewed(f, r); r.AdmitBeforeMs = f.DeadlineMs + 10 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseDeadlineNotDue, effective: base.DeadlineMs + 10},
		{why: "battle 明确采用旧协议", edit: func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.AdoptedOutcomeObserved = true },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, check: "adopted_legacy", effective: base.DeadlineMs},
		{why: "预检实例与目标不符", edit: func(f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.PreflightInstanceId = "other" },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, check: "preflight_instance_mismatch", effective: base.DeadlineMs},
		{why: "operation 为空:原件判据全过却不在到期恢复候选里", edit: func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { f.OperationId, r.PlacementOperationId = "", "" },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, check: "operation_missing", effective: base.DeadlineMs},
		{why: "原件不可解码:有效截止只有成局截止", post: func(f *pb.MatchFormationRecord) { f.CreateRequest = []byte{0xff} },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "create_undecodable", effective: base.DeadlineMs},
		{why: "成局与原件都没有截止", post: func(f *pb.MatchFormationRecord) { f.CreateRequest, f.DeadlineMs = []byte{0xff}, 0 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "create_undecodable", refusal: manualRefuseDeadlineMissing},
		{why: "工作租约未到期", edit: skewed, post: func(f *pb.MatchFormationRecord) { f.RetryAtMs = late + 1 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseWorkLeaseActive, effective: base.DeadlineMs},
		{why: "state 1:没有原件的局归 Finish(false)", edit: skewed, post: func(f *pb.MatchFormationRecord) { f.State = 1 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseNotStarted, effective: base.DeadlineMs},
		{why: "state 5 却没有终局行", edit: skewed, post: func(f *pb.MatchFormationRecord) { f.State = 5 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseReleased, effective: base.DeadlineMs},
		{why: "state 越界", edit: skewed, post: func(f *pb.MatchFormationRecord) { f.State = 0 },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, check: "created_before_formation", refusal: manualRefuseStateInvalid, effective: base.DeadlineMs},
		{why: "没有持久原件", post: func(f *pb.MatchFormationRecord) { f.CreateRequest = nil },
			now: late, class: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, check: "create_protocol", refusal: manualRefuseCreateMissing, effective: base.DeadlineMs},
	} {
		t.Run(c.why, func(t *testing.T) {
			f := manualFormation(t, c.edit)
			if c.post != nil {
				c.post(f)
			}
			a, verdict := assessManualAbort(f, nil, c.now)
			require.Equal(t, c.class, a.class)
			require.Equal(t, c.check, a.failedCheck)
			require.Equal(t, c.effective, a.effectiveDeadlineMs)
			if c.refusal == "" {
				require.NoError(t, verdict)
				return
			}
			require.ErrorIs(t, verdict, ErrManualNotAllowed)
			require.Equal(t, c.refusal, ManualRefusalCode(verdict))
		})
	}
}

// 已有任何终局行都不放行:同一原件的决定回「已处理」,只有归档、或身份冲突的行回 has_terminal_row(工具不覆盖已有行)。
func TestManualAbortRefusesAnyTerminalRow(t *testing.T) {
	f := manualFormation(t, func(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.CreatedAtMs = f.CreatedMs - 1 })
	late := f.DeadlineMs + terminalOutcomeGraceMs
	raw := []byte("fixture-complete-original")
	digest := sha256.Sum256(raw)
	row := func(decision pb.BattleOutcomeDecision, operation string) *pb.MatchBattleCompletionRecord {
		r := &pb.MatchBattleCompletionRecord{BattleId: f.BattleId, Decision: decision, PlacementOperationId: operation, TargetInstanceId: f.TargetInstanceId, OutcomeProtocolVersion: 1}
		if decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE {
			r.RawPayload, r.PayloadDigest = raw, digest[:]
		}
		return r
	}
	_, verdict := assessManualAbort(f, row(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, f.OperationId), late)
	require.ErrorIs(t, verdict, ErrManualAlreadyResolved, "另一位操作员或自动路径已 ABORT")
	require.Equal(t, manualVerdictResolved, manualVerdictCode(verdict))
	_, verdict = assessManualAbort(f, row(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, f.OperationId), late)
	require.ErrorIs(t, verdict, ErrManualAlreadyResolved)
	for why, terminal := range map[string]*pb.MatchBattleCompletionRecord{
		"只有 Kafka 归档、尚无决定": row(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, f.OperationId),
		"终局身份与成局冲突":        row(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, "other-operation"),
	} {
		_, verdict = assessManualAbort(f, terminal, late)
		require.Equal(t, manualRefuseHasTerminalRow, ManualRefusalCode(verdict), why)
	}
}

// 摘要只依赖成局行内容:同一记录稳定;采用回执、state、租约、原件任一变化摘要都变。
func TestFormationDigestDeterministic(t *testing.T) {
	f := manualFormation(t, nil)
	first, err := formationDigest(f)
	require.NoError(t, err)
	require.Len(t, first, 64)
	again, err := formationDigest(proto.Clone(f).(*pb.MatchFormationRecord))
	require.NoError(t, err)
	require.Equal(t, first, again)
	for why, edit := range map[string]func(*pb.MatchFormationRecord){
		"采用回执":  func(f *pb.MatchFormationRecord) { f.AdoptedOutcomeObserved, f.AdoptedOutcomeProtocolVersion = true, 1 },
		"state": func(f *pb.MatchFormationRecord) { f.State = 4 },
		"租约":    func(f *pb.MatchFormationRecord) { f.RetryAtMs++ },
		"原件":    func(f *pb.MatchFormationRecord) { f.CreateRequest = append([]byte(nil), 0xff) },
	} {
		changed := proto.Clone(f).(*pb.MatchFormationRecord)
		edit(changed)
		digest, err := formationDigest(changed)
		require.NoError(t, err)
		require.NotEqual(t, first, digest, why)
	}
}

func TestValidateManualAudit(t *testing.T) {
	require.NoError(t, ValidateManualAudit("ops-alice", "INC-7:原件早于成局,battle 日志确认房间已删"))
	for why, c := range map[string][2]string{
		"没有操作人":   {"  ", "INC-7"},
		"没有依据":    {"ops", ""},
		"操作人超长":   {strings.Repeat("o", ManualOperatorMaxBytes+1), "INC-7"},
		"依据超长":    {"ops", strings.Repeat("r", ManualReasonMaxBytes+1)},
		"换行伪造日志行": {"ops", "INC-7\n[FormationManual] executed=true"},
		"非法 UTF-8": {"ops\xff", "INC-7"},
	} {
		require.Error(t, ValidateManualAudit(c[0], c[1]), why)
	}
	require.NoError(t, ValidateManualAudit(strings.Repeat("o", ManualOperatorMaxBytes), strings.Repeat("r", ManualReasonMaxBytes)), "恰好到上限")
}

// 只有 protobuf 产生的错误算「成局行本身坏了」(list 标 formation_unreadable、show / abort 回同名拒绝码);
// 连接断开、超时、ctx 取消等读库失败一律不算,整次调用按可重试失败返回(2026-11-05 评审补)。
func TestFormationUnreadableOnlyForProtobufErrors(t *testing.T) {
	decodeErr := proto.Unmarshal([]byte{0xff}, &pb.MatchFormationRecord{})
	require.Error(t, decodeErr)
	require.True(t, formationUnreadable(decodeErr))
	require.True(t, formationUnreadable(fmt.Errorf("scan formation: %w", decodeErr)), "包了一层仍认得出")
	for _, err := range []error{nil, sql.ErrNoRows, driver.ErrBadConn, context.Canceled, context.DeadlineExceeded, errors.New("dial tcp 127.0.0.1:4000: connection refused")} {
		require.False(t, formationUnreadable(err), "读库失败不是数据损坏: %v", err)
	}
}

// ---- 真 TiDB ----

// strandedFixture 把一局两人成局直接写成「已持久 v1 原件、已过截止与宽限、租约已到期」(state 2,原票 state 2),再交 edit
// 改成局与原件(stamp 是写入前读到的 TiDB 时刻)。永久不可裁决的原件 SaveSupportedIntent 拒收,只能走直接 SQL。
func strandedFixture(t *testing.T, s *Store, battleID uint64, edit func(stamp uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)) []*pb.MatchAuthorityTicketRecord {
	t.Helper()
	tickets := exitFormationFixture(t, s, battleID, false)
	rewriteStranded(t, s, battleID, edit)
	return tickets
}

func rewriteStranded(t *testing.T, s *Store, battleID uint64, edit func(stamp uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)) {
	t.Helper()
	ctx := context.Background()
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	f.State, f.CreatedMs, f.DeadlineMs = 2, stamp-terminalOutcomeGraceMs-2000, stamp-terminalOutcomeGraceMs-1000
	f.OutcomeProtocolVersion, f.PreflightSupportedVersion, f.PreflightInstanceId = 1, 1, f.TargetInstanceId
	r := v1IntentRequest(f, f.CreatedMs, f.CreatedMs+1000)
	if edit != nil {
		edit(stamp, f, r)
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET state=?,created_ms=?,deadline_ms=?,create_request=?,outcome_protocol_version=?,preflight_supported_version=?,preflight_instance_id=?,adopted_outcome_protocol_version=?,adopted_outcome_observed=?,retry_at_ms=1 WHERE battle_id=?",
		f.State, f.CreatedMs, f.DeadlineMs, marshalIntent(t, r), f.OutcomeProtocolVersion, f.PreflightSupportedVersion, f.PreflightInstanceId, f.AdoptedOutcomeProtocolVersion, f.AdoptedOutcomeObserved, battleID)
	require.NoError(t, err)
}

// clockSkewed 是修复前时钟偏差造成的形状:原件创建时刻早于成局时刻(match 本机慢于 TiDB)。
func clockSkewed(_ uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) {
	r.CreatedAtMs = f.CreatedMs - 1
}

// insertArchivedOnly 写一行只有 Kafka 归档、尚无终局决定的终局行(decision=0)。字符串列显式写空串,不依赖列缺省值。
func insertArchivedOnly(t *testing.T, s *Store, battleID uint64) {
	t.Helper()
	_, err := s.DB.ExecContext(context.Background(), "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,1,1,'match-results',0,7,0,?,'','',0,0,0)",
		battleID, []byte{1}, []byte{2}, uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_KAFKA))
	require.NoError(t, err)
}

func manualRequest(battleID uint64, digest string, execute bool) ManualAbortRequest {
	return ManualAbortRequest{BattleID: battleID, ExpectDigest: digest, Operator: "ops-alice", Reason: "INC-7 original_mismatch confirmed", Replica: "match-0", Execute: execute}
}

func manualRows(t *testing.T, s *Store, battleID uint64) int {
	t.Helper()
	var rows int
	require.NoError(t, s.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM match_manual_resolution WHERE battle_id=?", battleID).Scan(&rows))
	return rows
}

// requireUntouched:没有终局行、没有审计行,成局与全员原票逐字段不变。
func requireUntouched(t *testing.T, s *Store, before *pb.MatchFormationRecord, tickets []*pb.MatchAuthorityTicketRecord, why string) {
	t.Helper()
	require.Zero(t, terminalRows(t, s, before.BattleId), why)
	require.Zero(t, manualRows(t, s, before.BattleId), why)
	f, err := s.Formation(context.Background(), before.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(before, f), why)
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 2, current.State, why)
		require.Equal(t, before.BattleId, current.FormationId, why)
	}
}

// requireManuallyAborted:恰好一份 ABORT(来源 MANUAL_ABORT、没有 Battle 原字节)、formation=5、全员原票 4、恰好一行审计。
func requireManuallyAborted(t *testing.T, s *Store, before *pb.MatchFormationRecord, tickets []*pb.MatchAuthorityTicketRecord) {
	t.Helper()
	ctx := context.Background()
	require.Equal(t, 1, terminalRows(t, s, before.BattleId))
	terminal, err := readTerminal(ctx, s.DB, before.BattleId)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, terminal.Decision)
	require.Equal(t, before.OperationId, terminal.PlacementOperationId)
	var formatVersion, sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT format_version,source_kind FROM match_battle_completion WHERE battle_id=?", before.BattleId).Scan(&formatVersion, &sourceKind))
	require.Zero(t, formatVersion)
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MANUAL_ABORT, sourceKind, "与截止裁决分开审计")
	f, err := s.Formation(ctx, before.BattleId)
	require.NoError(t, err)
	require.EqualValues(t, 5, f.State)
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State, "原票与 ABORT 同一事务结束")
	}
	require.Equal(t, 1, manualRows(t, s, before.BattleId))
	require.Zero(t, settlementSources(t, s, before.BattleId), "作废的局没有任何个人结算来源")
}

// 时钟偏差留下的 original_mismatch:到期恢复与 COMPLETE 永远裁决不了;人工演练不改行;执行恰好释放一次并在同一事务写审计;
// 重放回已处理;身份与成局行一致的迟到 COMPLETE 读到 ABORT;之后的到期恢复不再碰它。
func TestManualAbortTiDBOriginalMismatchReleasesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2601
	tickets := strandedFixture(t, s, battleID, clockSkewed)
	before, err := s.Formation(ctx, battleID)
	require.NoError(t, err)

	// 人工之前:到期恢复两轮都不改任何行,battle 的结果永久被拒。
	for range 2 {
		_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
		require.NoError(t, err)
		requireUntouched(t, s, before, tickets, "到期恢复裁决不了 original_mismatch")
	}
	event := terminalEventFor(t, before)
	_, err = s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: battleID, PlacementOperationId: before.OperationId, TargetInstanceId: before.TargetInstanceId, RawResultPayload: resultBytes(t, event)})
	require.ErrorIs(t, err, ErrBattleOutcomeOriginal, "COMPLETE 同样永久被拒")
	requireUntouched(t, s, before, tickets, "被拒的 COMPLETE 不改行")

	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, view.FormationClass)
	require.Equal(t, "created_before_formation", view.FailedCheck)
	require.Empty(t, view.Refusal)
	require.Len(t, view.Tickets, 2)
	digest, err := formationDigest(before)
	require.NoError(t, err)
	require.Equal(t, digest, view.Digest)

	// 演练:同一锁序、同一判据,整段回滚。
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, false))
	require.NoError(t, err)
	requireUntouched(t, s, before, tickets, "演练不写任何行")

	manualAborts := metrics.GlobalFlowValue("terminal", terminalAbortManual)
	locked, _, err := s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.NoError(t, err)
	require.EqualValues(t, 2, locked.State, "回包是处置前的状态")
	requireManuallyAborted(t, s, before, tickets)
	require.Equal(t, manualAborts+1, metrics.GlobalFlowValue("terminal", terminalAbortManual), "提交成功计一次 abort_manual")

	// 审计行:操作人、依据、处置前的成局与全员原票、有效截止、执行副本。
	var action, class uint32
	var failed, operator, reason, digestBefore, replica string
	var resolvedMs, effective uint64
	var formationBefore, ticketsBefore []byte
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT action,formation_class,failed_check,operator_name,reason,resolved_ms,formation_digest_before,formation_before,tickets_before,effective_deadline_ms,replica_instance FROM match_manual_resolution WHERE battle_id=?", battleID).
		Scan(&action, &class, &failed, &operator, &reason, &resolvedMs, &digestBefore, &formationBefore, &ticketsBefore, &effective, &replica))
	require.EqualValues(t, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, action)
	require.EqualValues(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, class)
	require.Equal(t, "created_before_formation", failed)
	require.Equal(t, "ops-alice", operator)
	require.Equal(t, "INC-7 original_mismatch confirmed", reason)
	require.NotZero(t, resolvedMs)
	require.Equal(t, view.Digest, digestBefore)
	require.Equal(t, before.DeadlineMs, effective)
	require.Equal(t, "match-0", replica)
	decodedFormation := &pb.MatchFormationRecord{}
	require.NoError(t, proto.Unmarshal(formationBefore, decodedFormation))
	require.True(t, proto.Equal(before, decodedFormation), "处置前成局行原样留档")
	decodedTickets := &pb.MatchManualResolutionRecord{}
	require.NoError(t, proto.Unmarshal(ticketsBefore, decodedTickets))
	var archivedPlayers, originalPlayers []uint64
	for _, original := range decodedTickets.TicketsBefore {
		require.EqualValues(t, 2, original.State, "留档的是处置前的原票")
		require.Equal(t, uint64(battleID), original.FormationId)
		archivedPlayers = append(archivedPlayers, original.PlayerId)
	}
	for _, original := range tickets {
		originalPlayers = append(originalPlayers, original.PlayerId)
	}
	require.ElementsMatch(t, originalPlayers, archivedPlayers, "全员原票都留档")

	// 重放(提交结果未知后重跑、或另一位操作员):回已处理,不改任何行,不重复计数。
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.ErrorIs(t, err, ErrManualAlreadyResolved)
	requireManuallyAborted(t, s, before, tickets)
	require.Equal(t, manualAborts+1, metrics.GlobalFlowValue("terminal", terminalAbortManual))

	// 迟到的 COMPLETE 读到 ABORT,battle 据此收尾删房(只在原件的 operation / 目标实例与成局行一致时成立,这里的时钟偏差类就是;
	// 身份不一致的类别见 TestManualAbortTiDBOperationMismatchLateCompleteStillConflicts);到期恢复不再碰它。
	decision, err := s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: battleID, PlacementOperationId: before.OperationId, TargetInstanceId: before.TargetInstanceId, RawResultPayload: resultBytes(t, event)})
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireManuallyAborted(t, s, before, tickets)
	view, _, err = s.ShowStrandedFormation(ctx, battleID)
	require.ErrorIs(t, err, ErrManualAlreadyResolved)
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MANUAL_ABORT, view.TerminalSourceKind, "show 看得到人工来源,提交结果未知时据此判断已完成")
}

// 原件合格、已过截止的局:人工拒绝且不改行,随后由到期恢复写出 MATCH_DEADLINE_ABORT。
func TestManualAbortTiDBRefusesRecoverableFormation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2602
	tickets := strandedFixture(t, s, battleID, nil)
	before, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.Equal(t, manualRefuseAutoRecoverable, ManualRefusalCode(err))
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO, view.FormationClass)
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.Equal(t, manualRefuseAutoRecoverable, ManualRefusalCode(err))
	requireUntouched(t, s, before, tickets, "原件合格的局人工不得抢")

	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	var sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT source_kind FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&sourceKind))
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT, sourceKind)
	require.Zero(t, manualRows(t, s, battleID))
}

// 过了截止但还在宽限内、工作租约未到期:都拒绝,不改行。
func TestManualAbortTiDBRefusesBeforeDeadlineGraceOrWithActiveLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const withinGrace, leased = 2603, 2604
	graceTickets := strandedFixture(t, s, withinGrace, func(stamp uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) {
		f.CreatedMs, f.DeadlineMs = stamp-10_000, stamp-1_000 // 截止已过,宽限(60 秒)还没过
		r.CreatedAtMs, r.DeadlineMs, r.AdmitBeforeMs = f.CreatedMs-1, f.DeadlineMs, f.CreatedMs+1_000
	})
	leaseTickets := strandedFixture(t, s, leased, clockSkewed)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)+600000 WHERE battle_id=?", leased)
	require.NoError(t, err)
	for battleID, c := range map[uint64]struct {
		tickets []*pb.MatchAuthorityTicketRecord
		refusal string
	}{
		withinGrace: {graceTickets, manualRefuseDeadlineNotDue},
		leased:      {leaseTickets, manualRefuseWorkLeaseActive},
	} {
		before, err := s.Formation(ctx, battleID)
		require.NoError(t, err)
		digest, err := formationDigest(before)
		require.NoError(t, err)
		view, _, err := s.ManualAbortFormation(ctx, manualRequest(battleID, digest, true))
		require.Equal(t, c.refusal, ManualRefusalCode(err))
		require.Equal(t, c.refusal, view.Refusal)
		requireUntouched(t, s, before, c.tickets, c.refusal)
	}
}

// 已有任何终局行(只有 Kafka 归档、或身份冲突的 ABORT)都拒绝,不改行:工具只写新终局,不覆盖。
func TestManualAbortTiDBRefusesExistingTerminalRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const archived, conflicting = 2605, 2606
	archivedTickets := strandedFixture(t, s, archived, clockSkewed)
	conflictingTickets := strandedFixture(t, s, conflicting, clockSkewed)
	insertArchivedOnly(t, s, archived)
	_, err := s.DB.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,0,1,'',0,0,?,?,'other-operation','original-instance',1,1,0)",
		conflicting, []byte{}, []byte{}, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT), uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT))
	require.NoError(t, err)
	for battleID, tickets := range map[uint64][]*pb.MatchAuthorityTicketRecord{archived: archivedTickets, conflicting: conflictingTickets} {
		before, err := s.Formation(ctx, battleID)
		require.NoError(t, err)
		digest, err := formationDigest(before)
		require.NoError(t, err)
		_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, digest, true))
		require.Equal(t, manualRefuseHasTerminalRow, ManualRefusalCode(err))
		require.Equal(t, 1, terminalRows(t, s, battleID), "已有行原样保留")
		require.Zero(t, manualRows(t, s, battleID))
		f, err := s.Formation(ctx, battleID)
		require.NoError(t, err)
		require.True(t, proto.Equal(before, f))
		for _, original := range tickets {
			require.EqualValues(t, 2, ticketRow(t, s, original.PlayerId).State)
		}
	}
}

// show 之后迟到的采用回执改了成局行:按旧摘要执行回 ErrManualChanged、不改行;重新 show 拿到新摘要才放行。
func TestManualAbortTiDBDigestChangedRefuses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2607
	tickets := strandedFixture(t, s, battleID, clockSkewed)
	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	work, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.NoError(t, s.RecordBattleAdopted(ctx, work, work.TargetInstanceId, 1))
	changed, err := s.Formation(ctx, battleID)
	require.NoError(t, err)

	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.ErrorIs(t, err, ErrManualChanged)
	requireUntouched(t, s, changed, tickets, "摘要不符不改行")

	fresh, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	require.NotEqual(t, view.Digest, fresh.Digest)
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, fresh.Digest, true))
	require.NoError(t, err)
	requireManuallyAborted(t, s, changed, tickets)
}

// 帮会历练局(state 4 已开局、原票 3):人工 ABORT 之前帮会取消只报 formation=4 / UNKNOWN;之后签 formation=5 / ABORT,
// guild 的 TrialCancelReceiptIsTerminal 据此释放(guild 侧同形回执的释放由 TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort 覆盖)。
// 两位操作员同时执行,并与到期恢复、帮会取消并发:只落一份终局、一行审计,每次人工调用要么成功、要么已处理。
// 落败的一方只可能回「已处理」:到期恢复与帮会取消对这一局只读、不改成局行,摘要在并发窗口里只会被胜者的 ABORT 改掉,
// 而胜者提交之后落败方要么锁外预读到已关的原票、要么锁内读到已变的成局,都经锁外复读读到这份 ABORT。
// 不放宽到 CHANGED:那会掩盖 manualAbortUnlockable 回归成「成局已变」的情况。
func TestManualAbortTiDBActivityConcurrentWithRecoveryAndCancel(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2608
	a, b := testJoin(t, s, 26081), testJoin(t, s, 26082)
	require.NoError(t, s.Cancel(ctx, a.PlayerId, a.Ticket))
	require.NoError(t, s.Cancel(ctx, b.PlayerId, b.Ticket))
	tickets := []*pb.MatchAuthorityTicketRecord{a, b}
	formation := &pb.MatchFormationRecord{BattleId: battleID, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3,
		TargetInstanceId: "original-instance", TargetEndpoint: "127.0.0.1:1", Mode: a.Mode, ConfigId: a.ConfigId}
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 46, RequestLow: 391}
	body := []byte("same-entire-original-start")
	_, err := s.ReserveDirect(ctx, key, body, formation, tickets)
	require.NoError(t, err)
	startFormation(t, s, battleID) // state 4、原票 3、采用回执 (1,1)
	rewriteStranded(t, s, battleID, func(_ uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) {
		f.State, f.AdoptedOutcomeObserved, f.AdoptedOutcomeProtocolVersion = 4, true, 1
		r.CreatedAtMs = f.CreatedMs - 1
	})
	// 与 exitFormationFixture 相同:补齐终局入口 terminalFormation 核对的原账号。
	for _, original := range tickets {
		_, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET owner_account='original-owner',owner_revision=1 WHERE player_id=?", original.PlayerId)
		require.NoError(t, err)
	}

	receipt, err := s.CancelActivity(ctx, key, body)
	require.NoError(t, err)
	require.EqualValues(t, 4, receipt.FormationState)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, receipt.TerminalDecision, "原件不可裁决,帮会取消签不出终局")

	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 4, view.State)
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, view.FormationClass)

	type result struct {
		manual bool
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		replica := *s
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			_, _, err := store.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
			results <- result{true, err}
		}(&replica)
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
		results <- result{false, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := s.CancelActivity(ctx, key, body)
		results <- result{false, err}
	}()
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for r := range results {
		if !r.manual {
			require.NoError(t, r.err, "自动路径对不可裁决的成局只读、不报故障")
			continue
		}
		if r.err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, r.err, ErrManualAlreadyResolved, "人工调用要么成功、要么已处理")
	}
	require.Equal(t, 1, succeeded, "两位操作员只有一位写下 ABORT")
	require.Equal(t, 1, terminalRows(t, s, battleID))
	require.Equal(t, 1, manualRows(t, s, battleID))
	for _, original := range tickets {
		require.EqualValues(t, 4, ticketRow(t, s, original.PlayerId).State)
	}

	receipt, err = s.CancelActivity(ctx, key, body)
	require.NoError(t, err)
	require.EqualValues(t, battleID, receipt.BattleId)
	require.EqualValues(t, 5, receipt.FormationState)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, receipt.TerminalDecision, "帮会取消读到人工 ABORT,签 formation=5 / ABORT")
}

// 列表:state 2/3/4 的 original_mismatch、shape_invalid、有终局行的、原件合格的都列出,分类正确;游标翻页不漏不重。
func TestStrandedFormationsTiDBListsAllClasses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	strandedFixture(t, s, 2611, clockSkewed)
	strandedFixture(t, s, 2612, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.State, f.PreflightInstanceId = 3, "other-instance" })
	strandedFixture(t, s, 2613, func(stamp uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { f.State = 4; clockSkewed(stamp, f, r) })
	strandedFixture(t, s, 2614, nil)
	insertArchivedOnly(t, s, 2613)

	seen := map[uint64]*pb.StrandedFormation{}
	cursor := ExpiredOutcomeCursor{}
	for page := 0; page < 4; page++ {
		formations, next, stamp, err := s.ListStrandedFormations(ctx, cursor, 3)
		require.NoError(t, err)
		require.NotZero(t, stamp)
		for _, f := range formations {
			require.NotContains(t, seen, f.BattleId, "翻页不重")
			seen[f.BattleId] = f
		}
		if next == (ExpiredOutcomeCursor{}) {
			break
		}
		cursor = next
	}
	require.Len(t, seen, 4, "翻页不漏")
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, seen[2611].FormationClass)
	require.Empty(t, seen[2611].Refusal)
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, seen[2612].FormationClass)
	require.Equal(t, "preflight_instance_mismatch", seen[2612].FailedCheck)
	require.Empty(t, seen[2612].Refusal)
	require.True(t, seen[2613].HasTerminalRow)
	require.Equal(t, manualRefuseHasTerminalRow, seen[2613].Refusal)
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO, seen[2614].FormationClass)
	require.Equal(t, manualRefuseAutoRecoverable, seen[2614].Refusal)
	_, _, _, err := s.ListStrandedFormations(ctx, ExpiredOutcomeCursor{}, StrandedFormationPageMax+1)
	require.Error(t, err)
}

// ---- 2026-11-05 评审补 ----

// 审计行与 ABORT 同一事务:审计行写不进去(先插一行同 battle_id 的审计,制造主键冲突),终局行、成局与全员原票都不变,
// 不计 abort_manual;错误是提交之前的读写失败(入口回 RETRY),不是提交结果未知。把审计挪到提交之后另开事务,这条会红。
func TestManualAbortTiDBAuditInsertFailureRollsBackAbort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2621
	tickets := strandedFixture(t, s, battleID, clockSkewed)
	before, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "INSERT INTO match_manual_resolution(battle_id,action,formation_class,failed_check,operator_name,reason,resolved_ms,formation_digest_before,formation_before,tickets_before,effective_deadline_ms,replica_instance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		battleID, uint32(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT), uint32(pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH),
		"fixture", "fixture-operator", "fixture: forces a duplicate audit key", 1, "fixture", []byte{}, []byte{}, 0, "fixture")
	require.NoError(t, err)

	manualAborts := metrics.GlobalFlowValue("terminal", terminalAbortManual)
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.Error(t, err)
	for _, sentinel := range []error{ErrManualNotFound, ErrManualNotAllowed, ErrManualChanged, ErrManualAlreadyResolved, ErrManualCommitUnknown} {
		require.NotErrorIs(t, err, sentinel, "审计写失败发生在提交之前:事务整段回滚,结局已知")
	}
	require.Zero(t, terminalRows(t, s, battleID), "ABORT 随审计一起回滚")
	require.Equal(t, 1, manualRows(t, s, battleID), "只有夹具那一行")
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.True(t, proto.Equal(before, f), "成局逐字段不变")
	for _, original := range tickets {
		current := ticketRow(t, s, original.PlayerId)
		require.EqualValues(t, 2, current.State, "原票随 ABORT 一起回滚")
		require.Equal(t, uint64(battleID), current.FormationId)
	}
	require.Equal(t, manualAborts, metrics.GlobalFlowValue("terminal", terminalAbortManual), "没提交不计数")
}

// 锁路径拿不到原票时按锁外复读的事实回答,不报「没有这一局」:某名成员的原票已关闭(夹具置 state 4,等同换轮)回
// tickets_not_original;某名成员的票行不存在(match 从不删票行,夹具直接删,模拟数据损坏或手改)回 ticket_missing。
// 两种都不改任何行,投影照常带回;锁外 show 不看票,这两局在 show 里仍显示可处置,以锁内演练为准。
func TestManualAbortTiDBUnlockableReportsTicketProblems(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const closed, missing = 2622, 2623
	closedTickets := strandedFixture(t, s, closed, clockSkewed)
	missingTickets := strandedFixture(t, s, missing, clockSkewed)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=?", closedTickets[0].PlayerId)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "DELETE FROM match_authority_ticket WHERE player_id=?", missingTickets[0].PlayerId)
	require.NoError(t, err)
	for battleID, c := range map[uint64]struct {
		refusal string
		intact  *pb.MatchAuthorityTicketRecord // 另一名成员的原票,必须原样不动
	}{
		closed:  {manualRefuseTicketsChanged, closedTickets[1]},
		missing: {manualRefuseTicketMissing, missingTickets[1]},
	} {
		before, err := s.Formation(ctx, battleID)
		require.NoError(t, err)
		shown, _, err := s.ShowStrandedFormation(ctx, battleID)
		require.NoError(t, err, "锁外预检只读成局与终局,不看票")
		locked, _, err := s.ManualAbortFormation(ctx, manualRequest(battleID, shown.Digest, true))
		require.NotErrorIs(t, err, ErrManualNotFound, c.refusal)
		require.Equal(t, c.refusal, ManualRefusalCode(err))
		require.NotNil(t, locked, c.refusal)
		require.Equal(t, battleID, locked.BattleId)
		require.Equal(t, c.refusal, locked.Refusal)
		require.Zero(t, terminalRows(t, s, battleID), c.refusal)
		require.Zero(t, manualRows(t, s, battleID), c.refusal)
		f, err := s.Formation(ctx, battleID)
		require.NoError(t, err)
		require.True(t, proto.Equal(before, f), c.refusal)
		require.EqualValues(t, 2, ticketRow(t, s, c.intact.PlayerId).State, c.refusal)
	}
}

// 成局行协议不是 v1(failed_check=outcome_protocol)、停在 state 2/3 的局还在旧恢复循环里:Pending 照常列出,AcquireWork 每次
// 到期都续 120 秒租约(awaitsExpiredOutcome 只拦 v1 原件)。retry_at_ms 一变摘要就变,租约有效期内又回 work_lease_active,
// 所以本工具处置不了这类局,手册写明升级研发。用例把这个行为固定下来:要支持它,得另批让恢复循环对过截止的非 v1 原件停止续租。
func TestManualAbortTiDBLegacyProtocolFormationKeepsRenewingLease(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2624
	tickets := strandedFixture(t, s, battleID, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.OutcomeProtocolVersion = 0 })
	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err, "锁外看:租约恰好到期的这一刻可处置")
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, view.FormationClass)
	require.Equal(t, "outcome_protocol", view.FailedCheck)

	pending, err := s.Pending(ctx)
	require.NoError(t, err)
	listed := false
	for _, f := range pending {
		listed = listed || f.BattleId == battleID
	}
	require.True(t, listed, "非 v1 原件不归到期终局恢复,仍在 Pending 候选里")
	work, err := s.AcquireWork(ctx, battleID)
	require.NoError(t, err, "恢复循环照常续租")
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	require.Greater(t, work.RetryAtMs, stamp)

	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.ErrorIs(t, err, ErrManualChanged, "续租改了 retry_at_ms,操作员核对过的摘要已不符")
	fresh, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.Equal(t, manualRefuseWorkLeaseActive, ManualRefusalCode(err))
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, fresh.Digest, true))
	require.Equal(t, manualRefuseWorkLeaseActive, ManualRefusalCode(err), "拿到新摘要也一样:租约有效期内一律拒绝")
	require.Zero(t, terminalRows(t, s, battleID))
	require.Zero(t, manualRows(t, s, battleID))
	for _, original := range tickets {
		require.EqualValues(t, 2, ticketRow(t, s, original.PlayerId).State)
	}
}

// 原件 operation 与成局不符(operation_mismatch)的局人工 ABORT 之后:battle 用原件里的 operation 提交的迟到 COMPLETE 与这份
// ABORT(存的是成局行的 operation)身份冲突,仍回 ErrBattleOutcomeOriginal 而不是 ABORT,battle 房间不会因它收尾,只能等
// battle 本地的重放上限删房;身份与成局行一致的提交才读到 ABORT。手册据此提示这类局处置之后另查 battle 侧房间。
func TestManualAbortTiDBOperationMismatchLateCompleteStillConflicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const battleID = 2625
	const originalOperation = "original-create-operation"
	tickets := strandedFixture(t, s, battleID, func(_ uint64, _ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) {
		r.PlacementOperationId = originalOperation
	})
	before, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	view, _, err := s.ShowStrandedFormation(ctx, battleID)
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, view.FormationClass)
	require.Equal(t, "operation_mismatch", view.FailedCheck)
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(battleID, view.Digest, true))
	require.NoError(t, err)
	requireManuallyAborted(t, s, before, tickets)

	raw := resultBytes(t, terminalEventFor(t, before))
	_, err = s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: battleID, PlacementOperationId: originalOperation, TargetInstanceId: before.TargetInstanceId, RawResultPayload: raw})
	require.ErrorIs(t, err, ErrBattleOutcomeOriginal, "battle 带的是原件身份,与 ABORT 行冲突")
	decision, err := s.CommitBattleOutcome(ctx, &pb.CommitBattleOutcomeRequest{BattleId: battleID, PlacementOperationId: before.OperationId, TargetInstanceId: before.TargetInstanceId, RawResultPayload: raw})
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision, "身份与成局行一致的提交才读到 ABORT")
	requireManuallyAborted(t, s, before, tickets)
}

// 成局行本身解不开(夹具把成员列写坏):list 只把这一局标 formation_unreadable,同页其他局照常列出;show 与 abort 回同一个拒绝码,
// 不当成可重试的读库失败,也不改任何行。
func TestManualAbortTiDBUnreadableFormationReported(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const broken, healthy = 2626, 2627
	strandedFixture(t, s, broken, clockSkewed)
	strandedFixture(t, s, healthy, clockSkewed)
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET members=? WHERE battle_id=?", []byte{0xff}, broken)
	require.NoError(t, err)

	formations, _, _, err := s.ListStrandedFormations(ctx, ExpiredOutcomeCursor{}, StrandedFormationPageMax)
	require.NoError(t, err, "坏一行不让整页失败")
	seen := map[uint64]*pb.StrandedFormation{}
	for _, f := range formations {
		seen[f.BattleId] = f
	}
	require.Equal(t, manualRefuseFormationUnreadable, seen[broken].GetRefusal())
	require.Equal(t, pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, seen[healthy].GetFormationClass())
	require.Empty(t, seen[healthy].GetRefusal())

	view, _, err := s.ShowStrandedFormation(ctx, broken)
	require.Nil(t, view)
	require.Equal(t, manualRefuseFormationUnreadable, ManualRefusalCode(err))
	_, _, err = s.ManualAbortFormation(ctx, manualRequest(broken, strings.Repeat("ab", 32), true))
	require.Equal(t, manualRefuseFormationUnreadable, ManualRefusalCode(err))
	require.Zero(t, terminalRows(t, s, broken))
	require.Zero(t, manualRows(t, s, broken))
}
