package authority

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
)

// 永久 UNKNOWN 全球成局的人工处置(2026-11-05,数据覆写审计 R2 r2-guild#1;手册 docs/ops/match-formation-manual-abort-runbook.md,
// 设计 cross-zone-matchmaking.md §13.3)。
//
// 为什么要有它:终局两条路径(COMPLETE 与截止 ABORT)共读 supportedOutcomeOriginal,原件过不了这道核对的成局按设计 fail-closed
// 永远停在 UNKNOWN——形状不合格(到期恢复计 expired_unrecoverable)、形状合格但原件正文与成局字段不符(original_mismatch)。
// 2026-11-05 起 SaveSupportedIntent 落库前跑同一判据,这类原件不再能新产生;剩下的只来自数据损坏、手改或修复前落库的行。
// 它们没有任何自动出口:全员票停在 2/3、帮会 launch 与参与者占用永不释放。本文件是这类成局唯一合规的出口,只给运维工具
// go/match/cmd/formationfix 经 MatchInternal.ResolveStrandedFormation 调用(logic/stranded_formation_ops.go),不替代自动路径。
//
// 释放只走截止裁决同一个事务入口 commitAbortTx(终局行 source_kind=MANUAL_ABORT、formation→5、原票 2/3→4),同一事务写审计行
// match_manual_resolution,不另写释放 SQL。帮会历练局之后由 guild 恢复器重放取消读到这份 ABORT,按 formation=5 / ABORT 释放,
// 与截止 ABORT 完全同一路径;match 不读写 guild 的库。
//
// 为什么放行条件(assessManualAbort)足以证明「这一局不会再有合法终局」:
//   - 原件过不了 supportedOutcomeOriginal,COMPLETE 的 validateTerminalResult 读同一个判据,battle 的任何结果都会被永久拒收;
//     到期恢复、帮会取消、提前裁决也都先过这道判据,对这类成局只回 UNKNOWN、不写库,所以人工 ABORT 不会与合法终局抢。
//     唯一的例外是 operation_missing(operation_id 为空):它过得了这道判据(判据不核 operation 是否为空),COMPLETE 在入口就因
//     operation 为空被拒,到期恢复的候选 SQL 也不选它,但帮会历练局的取消会照截止裁决写 ABORT。人工与它走同一把锁、同一个终局
//     主键,只落一份 ABORT,后到的一方回已处理,结果无害;
//   - 要求 TiDB 时刻已过 max(成局截止, 原件截止, 原件准入期限) 加 60 秒终局宽限:battle 房间按原件截止强制结束,
//     准入期限之后不会再接这份 Create;宽限与截止裁决同一个常量;
//   - 要求工作租约已到期:持租者还可能在推进这一局(与到期恢复同一条);
//   - 锁内复核与 COMPLETE / 截止 ABORT / 帮会取消同一入口 terminalFormation(全员原 P 升序 → formation FOR UPDATE → 原票 FOR UPDATE),
//     锁内先复读终局;操作员核对过的成局摘要在锁内逐字节比对,迟到的采用回执、租约变化都会让它不符而拒绝。
//     match_battle_completion 与审计表的主键 battle_id 保证多副本、多位操作员同时执行也只落一份 ABORT、一行审计。

var (
	// ErrManualNotFound:没有这一局成局。
	ErrManualNotFound = errors.New("人工处置：没有这一局成局")
	// ErrManualNotAllowed:前置条件不满足,具体原因码见 *ManualRefusal。未改动任何行。
	ErrManualNotAllowed = errors.New("人工处置：前置条件不满足")
	// ErrManualChanged:锁内成局摘要与操作员核对过的不符(成局在 SHOW 之后变化)。未改动,重新 SHOW。
	ErrManualChanged = errors.New("人工处置：成局在核对之后已变化")
	// ErrManualAlreadyResolved:这一局已有终局决定(另一位操作员或自动路径已处理)。未改动。
	ErrManualAlreadyResolved = errors.New("人工处置：这一局已有终局决定")
	// ErrManualCommitUnknown:ABORT 事务提交结果未知。绝不报成功,先 SHOW 重查:有 source=MANUAL_ABORT 的终局与审计行即已完成。
	ErrManualCommitUnknown = errors.New("人工处置：ABORT 提交结果未知")
)

// 人工处置不可放行的原因码(ASCII,低基数,运维工具原样打印;含义见 docs/ops/match-formation-manual-abort-runbook.md §3)。
const (
	manualRefuseInvalidRequest  = "invalid_request"      // 缺 battle_id / expect_digest,或操作人、依据不合规
	manualRefuseHasTerminalRow  = "has_terminal_row"     // 已有终局行却不是同一原件的决定(只有归档、或身份冲突):工具不处置
	manualRefuseNotStarted      = "not_started"          // state 1:没有 Create 原件,由 Finish(false) 收尾
	manualRefuseReleased        = "state_released"       // state 5 却没有终局行:旧代码的安全中止,不归本工具
	manualRefuseStateInvalid    = "state_invalid"        // state 不在 1..5
	manualRefuseCreateMissing   = "create_missing"       // 没有持久 Create 原件
	manualRefuseAutoRecoverable = "auto_recoverable"     // 原件合格:归到期恢复 / 帮会取消自动裁决
	manualRefuseWorkLeaseActive = "work_lease_active"    // 工作租约尚未到期
	manualRefuseDeadlineMissing = "deadline_missing"     // 成局与原件都没有截止
	manualRefuseDeadlineNotDue  = "deadline_not_due"     // 还没过有效截止加终局宽限
	manualRefuseTicketsChanged  = "tickets_not_original" // 成员原票已不在 2/3、换了轮次或换了主:原 P→票锁拿不到
	manualRefuseTicketMissing   = "ticket_missing"       // 成局还在,某名成员的票行却不存在(match 从不删票行:数据损坏或手改)
	// 成局行本身解不开(成员 / 备战端点等 protobuf 列损坏)。只认 protobuf 产生的错误,见 formationUnreadable。
	manualRefuseFormationUnreadable = "formation_unreadable"
	manualVerdictResolved           = "resolved" // 只用在展示:已有终局决定
)

// formationUnreadable 判读一局成局时的错误是不是「这一行本身坏了」:protobuf 解码或序列化失败(成员、备战端点列损坏,
// 字符串列不是合法 UTF-8),换一次读也不会好。连接断开、超时、ctx 取消等读库失败不是它,整次调用按可重试失败返回;
// 两者混在一起,值班会把一次瞬时读错当成数据损坏升级,或把损坏当成「稍后重试」一直等(2026-11-05 评审补)。
func formationUnreadable(err error) bool { return errors.Is(err, proto.Error) }

// ManualRefusal 是 ErrManualNotAllowed 的具体原因。errors.Is(err, ErrManualNotAllowed) 为真。
type ManualRefusal struct{ Code string }

func (e *ManualRefusal) Error() string        { return ErrManualNotAllowed.Error() + ": " + e.Code }
func (e *ManualRefusal) Is(target error) bool { return target == ErrManualNotAllowed }

func refuseManual(code string) error { return &ManualRefusal{Code: code} }

// ManualRefusalCode 取错误里的原因码;不是前置条件拒绝时回空串。
func ManualRefusalCode(err error) string {
	var refusal *ManualRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// 审计字段的字节上限:写进 match_manual_resolution,也原样进 ERROR 日志。
const (
	ManualOperatorMaxBytes = 64
	ManualReasonMaxBytes   = 191
)

// ValidateManualAudit 校验人工处置的操作人与依据:非空白、合法 UTF-8、不含控制字符(换行会伪造日志行)、不超过字节上限。
// 运维工具发请求之前、服务端 RPC 入口与提交点都调它,同一输入各处给出同一结论。
func ValidateManualAudit(operator, reason string) error {
	if err := validateManualText("operator", operator, ManualOperatorMaxBytes); err != nil {
		return err
	}
	return validateManualText("reason", reason, ManualReasonMaxBytes)
}

func validateManualText(name, value string, maxBytes int) error {
	switch {
	case strings.TrimSpace(value) == "":
		return fmt.Errorf("%s 不能为空", name)
	case len(value) > maxBytes:
		return fmt.Errorf("%s 超过 %d 字节", name, maxBytes)
	case !utf8.ValidString(value):
		return fmt.Errorf("%s 不是合法 UTF-8", name)
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return fmt.Errorf("%s 不能含控制字符", name)
	}
	return nil
}

// manualAssessment 是一局成局的分类与有效截止(不含放行结论)。
type manualAssessment struct {
	class               pb.StrandedFormationClass
	failedCheck         string                        // 第一项不通过的原件核对;AUTO 时为空
	original            *battlepb.CreateBattleRequest // 持久 Create 能解码时非 nil(核对不通过也给出)
	effectiveDeadlineMs uint64
}

// classifyStranded 用终局两条路径的同一份原件判据(originalCapabilityCheck + decodeOutcomeOriginal,即 supportedOutcomeOriginal)
// 给成局分类:原件合格且在到期恢复候选里的是 AUTO;其余按到期恢复候选形状 recoverableOutcomeShape(到期恢复也用它区分
// original_mismatch 与形状不合格)分两类。有效截止取 max(成局截止, 原件截止, 原件准入期限):原件不可解码时只有成局截止,
// 不可解码的原件 gather 根本发不出去(gather 先按规范形式解码再发 Create)。
func classifyStranded(f *pb.MatchFormationRecord) manualAssessment {
	original, check := decodeOutcomeOriginal(f)
	if capability := originalCapabilityCheck(f); capability != "" {
		check = capability
	}
	a := manualAssessment{failedCheck: check, original: original, effectiveDeadlineMs: f.GetDeadlineMs()}
	if original != nil {
		a.effectiveDeadlineMs = max(f.GetDeadlineMs(), original.DeadlineMs, original.AdmitBeforeMs)
	}
	shape := recoverableOutcomeShape(f)
	switch {
	case check == "" && shape:
		a.class = pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO
	case check == "":
		// 原件判据全过、却不在到期恢复候选里:只剩 operation_id 为空这一项(supportedOutcomeOriginal 不核它)。
		a.class, a.failedCheck = pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID, "operation_missing"
	case shape:
		a.class = pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH
	default:
		a.class = pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_SHAPE_INVALID
	}
	return a
}

// assessManualAbort 是人工 ABORT 的全部放行判据,纯函数。锁外 SHOW 与锁内 DRY_RUN / ABORT 读同一个函数,nowMs 都取 TiDB 时刻。
// 返回 nil 才可处置;ErrManualAlreadyResolved:已有同一原件的终局决定;*ManualRefusal:前置条件不满足。
// 放行要同时满足(为什么够,见文件头):
//  1. match_battle_completion 没有这一局的任何行(只有归档、或身份冲突的行同样拒绝:工具只写新终局,不覆盖已有行);
//  2. state 2/3/4 且有持久 Create 原件;
//  3. 原件过不了终局核对(不是 AUTO);
//  4. 工作租约已到期;
//  5. TiDB 时刻已过有效截止加终局宽限(terminalAbortDue,宽限与截止裁决同一个常量)。
func assessManualAbort(f *pb.MatchFormationRecord, terminal *pb.MatchBattleCompletionRecord, nowMs uint64) (manualAssessment, error) {
	a := classifyStranded(f)
	if terminal != nil {
		decision, err := replayFormationTerminal(terminal, f)
		if err == nil && decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN {
			return a, fmt.Errorf("%w: decision=%s", ErrManualAlreadyResolved, decision)
		}
		return a, refuseManual(manualRefuseHasTerminalRow)
	}
	switch {
	case f.State == 1:
		return a, refuseManual(manualRefuseNotStarted)
	case f.State == 5:
		return a, refuseManual(manualRefuseReleased)
	case f.State < 2 || f.State > 4:
		return a, refuseManual(manualRefuseStateInvalid)
	case len(f.CreateRequest) == 0:
		return a, refuseManual(manualRefuseCreateMissing)
	case a.class == pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO:
		return a, refuseManual(manualRefuseAutoRecoverable)
	case f.RetryAtMs > nowMs:
		return a, refuseManual(manualRefuseWorkLeaseActive)
	case a.effectiveDeadlineMs == 0:
		return a, refuseManual(manualRefuseDeadlineMissing)
	case !terminalAbortDue(a.effectiveDeadlineMs, nowMs):
		return a, refuseManual(manualRefuseDeadlineNotDue)
	}
	return a, nil
}

// formationDigest 是成局行确定性序列化的 SHA-256(hex)。只在 match 服务端算:SHOW 回给运维,DRY_RUN / ABORT 在锁内比对。
// 任何字段变化(state、retry_at_ms、采用回执、原件……)摘要都会变。滚动升级期间两个副本的序列化万一不同,只会回 CHANGED
// (重新 SHOW 即可),不会误放行。
func formationDigest(f *pb.MatchFormationRecord) (string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// manualVerdictCode 是展示用的放行结论:原因码、已有终局时 resolved、可处置时空串。
func manualVerdictCode(verdict error) string {
	if errors.Is(verdict, ErrManualAlreadyResolved) {
		return manualVerdictResolved
	}
	return ManualRefusalCode(verdict)
}

// strandedView 组展示投影;terminalSource 只在 terminal 非 nil 时有意义。
func strandedView(f *pb.MatchFormationRecord, a manualAssessment, terminal *pb.MatchBattleCompletionRecord, terminalSource uint32, digest string, verdict error) *pb.StrandedFormation {
	v := &pb.StrandedFormation{
		BattleId: f.BattleId, State: f.State, OperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId,
		PreflightInstanceId: f.PreflightInstanceId, MatchMode: f.Mode, ConfigId: f.ConfigId, MemberCount: uint32(len(f.Members)),
		Activity: len(f.ActivityContext) > 0, CreatedMs: f.CreatedMs, DeadlineMs: f.DeadlineMs, RetryAtMs: f.RetryAtMs,
		OutcomeProtocolVersion: f.OutcomeProtocolVersion, PreflightSupportedVersion: f.PreflightSupportedVersion,
		AdoptedOutcomeProtocolVersion: f.AdoptedOutcomeProtocolVersion, AdoptedOutcomeObserved: f.AdoptedOutcomeObserved,
		CreateRequestBytes: uint32(len(f.CreateRequest)), FormationClass: a.class, FailedCheck: a.failedCheck,
		EffectiveDeadlineMs: a.effectiveDeadlineMs, Digest: digest, Refusal: manualVerdictCode(verdict),
	}
	if r := a.original; r != nil {
		v.CreateDecodable = true
		v.CreateCreatedAtMs, v.CreateDeadlineMs, v.CreateAdmitBeforeMs = r.CreatedAtMs, r.DeadlineMs, r.AdmitBeforeMs
		v.CreatePlayerCount = uint32(len(r.Players))
	}
	if terminal != nil {
		v.HasTerminalRow, v.TerminalDecision, v.TerminalSourceKind = true, terminal.Decision, terminalSource
	}
	return v
}

func ticketViews(tickets []*pb.MatchAuthorityTicketRecord) []*pb.StrandedFormationTicket {
	out := make([]*pb.StrandedFormationTicket, 0, len(tickets))
	for _, t := range tickets {
		out = append(out, &pb.StrandedFormationTicket{PlayerId: t.PlayerId, State: t.State, FormationId: t.FormationId, OwnerRevision: t.OwnerRevision})
	}
	return out
}

// terminalSourceKind 读已有终局行的来源种类(readTerminal 不读这一列,只给展示用)。
func terminalSourceKind(ctx context.Context, q rowQuerier, battle uint64) (uint32, error) {
	var source uint32
	err := q.QueryRowContext(ctx, "SELECT source_kind FROM match_battle_completion WHERE battle_id=?", battle).Scan(&source)
	return source, err
}

// assessUnlocked 锁外读终局,按调用方读好的 TiDB 时刻 stamp 跑同一份放行判据,组展示投影。withTickets 时再读全员当前票
// (只用于展示,是否仍是原票以锁内 DRY_RUN 为准)。投影为 nil 时错误是读库失败;投影非 nil 时错误就是 assessManualAbort 的放行结论。
func (s *Store) assessUnlocked(ctx context.Context, f *pb.MatchFormationRecord, stamp uint64, withTickets bool) (*pb.StrandedFormation, error) {
	terminal, err := readTerminal(ctx, s.DB, f.BattleId)
	if err != nil {
		return nil, err
	}
	var source uint32
	if terminal != nil {
		if source, err = terminalSourceKind(ctx, s.DB, f.BattleId); err != nil {
			return nil, err
		}
	}
	digest, err := formationDigest(f)
	if err != nil {
		return nil, err
	}
	a, verdict := assessManualAbort(f, terminal, stamp)
	view := strandedView(f, a, terminal, source, digest, verdict)
	if withTickets {
		for _, m := range f.Members {
			t, err := scanTicket(s.DB.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", m.PlayerId))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			view.Tickets = append(view.Tickets, ticketViews([]*pb.MatchAuthorityTicketRecord{t})...)
		}
	}
	return view, verdict
}

// StrandedFormationPageMax 是 ListStrandedFormations 一页的上限。
const StrandedFormationPageMax = 100

// ListStrandedFormations 只读列出已过截止加宽限、仍停在 state 2/3/4、带持久 Create 原件且尚未退休的成局(含原件合格、
// 等自动恢复的 AUTO 局),逐局跑同一份放行判据。沿 (state,retry_at_ms) 索引按 (retry_at_ms, battle_id) 升序翻页,
// 与到期恢复同一顺序;COMPLETE 已退休(retry_at_ms=terminalOutcomeNoRetryMs)的局不在内。先只取标量游标、再逐局读整行:
// 行本身解不开(formationUnreadable)的一局只标 formation_unreadable,不卡住整页(同 RecoverExpiredOutcomeLane);
// 读库失败(连接、超时、ctx)让整页返回错误,入口回 retry,不把瞬时错误报成数据损坏。返回本页、下一页游标(到末尾为零值)
// 与 TiDB 时刻。补上 CountStrandedExpiredFormations 看不到的 state 4 与 original_mismatch。
func (s *Store) ListStrandedFormations(ctx context.Context, after ExpiredOutcomeCursor, limit int) ([]*pb.StrandedFormation, ExpiredOutcomeCursor, uint64, error) {
	if s == nil || s.DB == nil {
		return nil, after, 0, errors.New("人工处置权威未装配")
	}
	if limit < 1 || limit > StrandedFormationPageMax {
		return nil, after, 0, fmt.Errorf("每页条数必须在 1..%d", StrandedFormationPageMax)
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return nil, after, 0, err
	}
	if stamp < terminalOutcomeGraceMs {
		return nil, ExpiredOutcomeCursor{}, stamp, nil
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT retry_at_ms,battle_id FROM match_formation WHERE state IN(2,3,4) AND retry_at_ms<? AND (retry_at_ms>? OR (retry_at_ms=? AND battle_id>?)) AND deadline_ms>0 AND deadline_ms<=? AND LENGTH(create_request)>0 ORDER BY retry_at_ms,battle_id LIMIT "+strconv.Itoa(limit),
		terminalOutcomeNoRetryMs, after.AfterRetryMs, after.AfterRetryMs, after.AfterBattle, stamp-terminalOutcomeGraceMs)
	if err != nil {
		return nil, after, 0, err
	}
	var page []ExpiredOutcomeCursor
	for rows.Next() {
		var c ExpiredOutcomeCursor
		if err := rows.Scan(&c.AfterRetryMs, &c.AfterBattle); err != nil {
			rows.Close()
			return nil, after, 0, err
		}
		page = append(page, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, after, 0, err
	}
	var out []*pb.StrandedFormation
	for _, c := range page {
		view, err := s.listedFormation(ctx, c.AfterBattle, stamp)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case formationUnreadable(err):
			// 这一行本身坏了:只报出来,不让整页失败。
			out = append(out, &pb.StrandedFormation{BattleId: c.AfterBattle, RetryAtMs: c.AfterRetryMs, Refusal: manualRefuseFormationUnreadable})
			continue
		case err != nil:
			return nil, after, 0, err
		}
		out = append(out, view)
	}
	next := ExpiredOutcomeCursor{}
	if len(page) == limit {
		next = page[len(page)-1]
	}
	return out, next, stamp, nil
}

// listedFormation 读一局整行并按 stamp 跑放行判据,组列表投影;放行结论已写进投影的 refusal。
// 错误只表示没读成:sql.ErrNoRows、formationUnreadable,或读库失败,由调用方分别处理。
func (s *Store) listedFormation(ctx context.Context, battle, stamp uint64) (*pb.StrandedFormation, error) {
	f, err := s.Formation(ctx, battle)
	if err != nil {
		return nil, err
	}
	view, verdict := s.assessUnlocked(ctx, f, stamp, false)
	if view == nil {
		return nil, verdict
	}
	return view, nil
}

// ShowStrandedFormation 锁外只读核对一局:不加锁、不写。返回投影、TiDB 时刻与放行结论(nil = 锁外看可处置,以 DRY_RUN 为准)。
// 成局行本身解不开(formationUnreadable)时投影为 nil、回 formation_unreadable 拒绝:换一次读也不会好,不当成可重试的读库失败。
func (s *Store) ShowStrandedFormation(ctx context.Context, battle uint64) (*pb.StrandedFormation, uint64, error) {
	if s == nil || s.DB == nil {
		return nil, 0, errors.New("人工处置权威未装配")
	}
	f, err := s.Formation(ctx, battle)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, 0, ErrManualNotFound
	case formationUnreadable(err):
		return nil, 0, refuseManual(manualRefuseFormationUnreadable)
	case err != nil:
		return nil, 0, err
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return nil, 0, err
	}
	view, verdict := s.assessUnlocked(ctx, f, stamp, true)
	if view == nil && formationUnreadable(verdict) {
		return nil, stamp, refuseManual(manualRefuseFormationUnreadable)
	}
	return view, stamp, verdict
}

// ManualAbortRequest 是一次人工 ABORT(或它的锁内演练)的完整输入。
type ManualAbortRequest struct {
	BattleID     uint64
	ExpectDigest string // 操作员 SHOW 时看到的成局摘要
	Operator     string
	Reason       string
	Replica      string // 执行副本(主机名),只进审计行
	Execute      bool   // false = 锁内复核后整段回滚(DRY_RUN)
}

// ManualAbortFormation 在锁内复核后写人工 ABORT。锁序与 COMPLETE、截止 ABORT、帮会取消同一入口 terminalFormation;
// 锁内先复读终局、再取 TiDB 时刻跑 assessManualAbort,再比对操作员核对过的摘要。Execute=false 时到此回滚,返回同一份投影。
// Execute=true 时经 commitAbortTx(source=MANUAL_ABORT)写终局行、formation→5、原票 2/3→4,同一事务写审计行,提交成功后
// 计 abort_manual。返回投影是处置前的状态。错误:ErrManualNotFound / ErrManualNotAllowed(*ManualRefusal) / ErrManualChanged /
// ErrManualAlreadyResolved 都没有改动任何行;ErrManualCommitUnknown 是提交结果未知;其余是读写失败(事务已回滚,未改动)。
func (s *Store) ManualAbortFormation(ctx context.Context, req ManualAbortRequest) (*pb.StrandedFormation, uint64, error) {
	if s == nil || s.DB == nil {
		return nil, 0, errors.New("人工处置权威未装配")
	}
	if req.BattleID == 0 || req.ExpectDigest == "" || ValidateManualAudit(req.Operator, req.Reason) != nil {
		return nil, 0, refuseManual(manualRefuseInvalidRequest)
	}
	tx, f, originals, err := s.terminalFormation(ctx, req.BattleID)
	if err != nil {
		return s.manualAbortUnlockable(ctx, req.BattleID, err)
	}
	defer tx.Rollback()
	terminal, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return nil, 0, err
	}
	var source uint32
	if terminal != nil {
		if source, err = terminalSourceKind(ctx, tx, f.BattleId); err != nil {
			return nil, 0, err
		}
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	digest, err := formationDigest(f)
	if err != nil {
		return nil, 0, err
	}
	a, verdict := assessManualAbort(f, terminal, stamp)
	view := strandedView(f, a, terminal, source, digest, verdict)
	view.Tickets = ticketViews(originals)
	if errors.Is(verdict, ErrManualAlreadyResolved) {
		return view, stamp, verdict
	}
	if digest != req.ExpectDigest {
		return view, stamp, fmt.Errorf("%w: 锁内摘要 %s,核对过的是 %s", ErrManualChanged, digest, req.ExpectDigest)
	}
	if verdict != nil {
		return view, stamp, verdict
	}
	if !req.Execute {
		return view, stamp, nil
	}
	if err = commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MANUAL_ABORT); err != nil {
		return view, stamp, err
	}
	if err = insertManualResolution(ctx, tx, f, originals, a, digest, req, stamp); err != nil {
		return view, stamp, err
	}
	if err = tx.Commit(); err != nil {
		return view, stamp, fmt.Errorf("%w: %v", ErrManualCommitUnknown, err)
	}
	observeTerminalAbort(f.BattleId, terminalAbortManual)
	return view, stamp, nil
}

// manualAbortUnlockable 处理 terminalFormation 拿不到锁内原行的情形:没有这一局、某名成员的票行不存在、原票已关闭或换主、
// 成局并发变化、成局行解不开、读库失败。另一位操作员或别的入口可能刚提交了终局并在同一事务里关掉原票,所以一律先锁外复读,
// 有终局就照它回答,不当成失败。是不是「没有这一局」也只由这次复读回答(2026-11-05 评审补):terminalFormationOnce 在成局行
// 不存在和某名成员的票行不存在时都回 sql.ErrNoRows,只看锁路径的错误会把缺票行的局报成 NOT_FOUND,与 show 的结论对不上。
func (s *Store) manualAbortUnlockable(ctx context.Context, battle uint64, lockErr error) (*pb.StrandedFormation, uint64, error) {
	view, stamp, err := s.ShowStrandedFormation(ctx, battle)
	switch {
	case errors.Is(err, ErrManualAlreadyResolved), errors.Is(err, ErrManualNotFound), errors.Is(err, ErrManualNotAllowed):
		return view, stamp, err
	case err != nil:
		return view, stamp, errors.Join(lockErr, err)
	case errors.Is(lockErr, ErrOriginalPlayer):
		view.Refusal = manualRefuseTicketsChanged
		return view, stamp, refuseManual(manualRefuseTicketsChanged)
	case errors.Is(lockErr, sql.ErrNoRows):
		// 复读读到了成局(match 从不删成局行),锁路径却读不到行:只剩某名成员的票行不存在。
		view.Refusal = manualRefuseTicketMissing
		return view, stamp, refuseManual(manualRefuseTicketMissing)
	default:
		return view, stamp, lockErr
	}
}

// insertManualResolution 在 ABORT 同一事务里写审计行。主键 battle_id:同一局只人工处置一次。
func insertManualResolution(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, originals []*pb.MatchAuthorityTicketRecord, a manualAssessment, digest string, req ManualAbortRequest, stamp uint64) error {
	deterministic := proto.MarshalOptions{Deterministic: true}
	formationBefore, err := deterministic.Marshal(f)
	if err != nil {
		return err
	}
	ticketsBefore, err := deterministic.Marshal(&pb.MatchManualResolutionRecord{TicketsBefore: originals})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_manual_resolution(battle_id,action,formation_class,failed_check,operator_name,reason,resolved_ms,formation_digest_before,formation_before,tickets_before,effective_deadline_ms,replica_instance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		f.BattleId, uint32(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT), uint32(a.class), a.failedCheck, req.Operator, req.Reason,
		stamp, digest, formationBefore, ticketsBefore, a.effectiveDeadlineMs, req.Replica)
	return err
}
