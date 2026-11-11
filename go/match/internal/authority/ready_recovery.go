package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	battlepb "proto/battle"
	pb "proto/match"
)

// SQL只缩小待核候选；完整Create正文及当前角色P/工作租约仍由锁内入口复核。
// recoverableOutcomeShape 是同一规则的 Go 写法,两处必须一起改。
const recoverableOutcomeShapeSQL = "f.outcome_protocol_version=1 AND f.preflight_supported_version=1 AND f.operation_id<>'' AND f.target_instance_id<>'' AND f.preflight_instance_id=f.target_instance_id AND LENGTH(f.create_request)>0 AND ((f.adopted_outcome_observed=0 AND f.adopted_outcome_protocol_version=0) OR (f.adopted_outcome_observed=1 AND f.adopted_outcome_protocol_version=1))"

// recoverableOutcomeShape 是 recoverableOutcomeShapeSQL 的 Go 写法。只用来把「形状合格、却过不了持久 Create 原件核对」
// 与旧协议 / 能力证据不全(按设计保持 UNKNOWN,过截止后由巡检的 CountStrandedExpiredFormations 计数)分开。
func recoverableOutcomeShape(f *pb.MatchFormationRecord) bool {
	return f != nil && f.OutcomeProtocolVersion == 1 && f.PreflightSupportedVersion == 1 && f.OperationId != "" &&
		f.TargetInstanceId != "" && f.PreflightInstanceId == f.TargetInstanceId && len(f.CreateRequest) > 0 &&
		((!f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion == 0) || (f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion == 1))
}

// errOutsideRecoveryShape:原件过得了 supportedOutcomeOriginal,成局行却不在到期恢复的候选形状里(只剩 operation_id 为空)。
var errOutsideRecoveryShape = errors.New("成局不在到期恢复候选形状里(operation_id为空)，终局保持UNKNOWN")

// outcomeAdjudicable 是「自动路径能裁决这份成局的终局」的全部原件判据(2026-11-06):在到期恢复的候选形状里
// (recoverableOutcomeShape,即候选 SQL),且持久 Create 原件过得了截止 ABORT 与 COMPLETE 共读的 supportedOutcomeOriginal。
// COMPLETE 的 completableOriginal 比它严,且 COMPLETE 入口要求 operation 非空,过得了 COMPLETE 的一定过得了它。
// 人工处置工具的 AUTO 类(classifyStranded)与它同一口径,其余各类只能人工处置。只看原件,不看成局状态;
// 「自动路径会不会写终局」还要求成局在途,见 autoTerminalOriginal。
// 形状合格却过不了原件核对回 ErrUnrecoverableFormation;其余不过回普通错误(旧协议、能力证据不全、operation 为空)。纯函数。
func outcomeAdjudicable(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, error) {
	shape := recoverableOutcomeShape(f)
	r, err := supportedOutcomeOriginal(f)
	switch {
	case err != nil && shape:
		return nil, fmt.Errorf("%w: %v", ErrUnrecoverableFormation, err)
	case err != nil:
		return nil, err
	case !shape:
		return nil, errOutsideRecoveryShape
	}
	return r, nil
}

// formationInFlight:成局在途、尚未终结(state 2/3/4),与到期恢复候选 SQL 的 state IN(2,3,4) 同一口径。
// 有没有持久 Create 原件另由 outcomeAdjudicable 判(state 3 也可能是还没落原件就失联的局)。
func formationInFlight(f *pb.MatchFormationRecord) bool {
	return f != nil && f.State >= 2 && f.State <= 4
}

// autoTerminalOriginal 是「自动路径会给这一局写出终局」的唯一判据(2026-11-06):成局在途(formationInFlight)且原件可裁决
// (outcomeAdjudicable),为真时返回可裁决的 Create 原件。截止 ABORT(abortTerminalTx,到期恢复与帮会取消共用)、到期恢复的
// 取证排期(deadlineProbeDue)与 scene 冻结到期查询(TerminalOutcomeExpected)都只调它,所以「回包说会有终局」与「恢复循环
// 实际写终局」是同一组判据:为真且还没有终局行的局,match 可用时到期恢复最迟在 截止 + 宽限 + 硬上限 + 持有宽限 之后写出
// ABORT(COMPLETE 先落库就以 COMPLETE 为准,见 judgeDeadlineAbort);为假的局任何自动路径都不写终局,COMPLETE 也落不了库
// (completableOriginal 比它严,commitBattleOutcomeLocked 拒 state 5),只能人工处置(formationfix)。纯函数。
func autoTerminalOriginal(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, bool) {
	if !formationInFlight(f) {
		return nil, false
	}
	r, err := outcomeAdjudicable(f)
	return r, err == nil
}

// ErrUnrecoverableFormation:成局满足到期恢复的候选形状(recoverableOutcomeShape),持久 Create 原件却与成局字段对不上
// (supportedOutcomeOriginal 失败)。到期 ABORT 与 COMPLETE 都裁决不了它,票据永远停在 state 2/3/4,只能人工用
// formationfix 处置(manual_abort.go,docs/ops/match-formation-manual-abort-runbook.md)。用 errors.Is 判断(2026-10-30,评审 B-4)。
var ErrUnrecoverableFormation = errors.New("成局形状合格但持久Create原件与成局字段不符，到期终局无法裁决")

// unrecoverableFormationOutcome 是这一类在 global_flow_total{stage="recover"} 下的 outcome 取值(低基数;battle_id 只进日志)。
const unrecoverableFormationOutcome = "original_mismatch"

// reportUnrecoverableFormation 记一次「到期恢复访问到不可恢复成局」:计数 + ERROR。每次访问都报,值班按速率判断还在不在。
func reportUnrecoverableFormation(battleID uint64, cause error) {
	metrics.ObserveGlobalFlow("recover", unrecoverableFormationOutcome)
	logx.Errorf("global formation can never reach a terminal outcome battle_id=%d: %v; its tickets stay occupied, "+
		"handle it with go/match/cmd/formationfix per docs/ops/match-formation-manual-abort-runbook.md "+
		"(never hand-edit match_formation / match_authority_ticket / match_battle_completion)",
		battleID, cause)
}

// 同一持久原操作与实例的合法终局才退出恢复；冲突/旧来源记录不能被当作新终局证明。
const matchingOutcomeTerminalSQL = "c.battle_id=f.battle_id AND c.outcome_protocol_version=1 AND c.placement_operation_id=f.operation_id AND c.target_instance_id=f.target_instance_id AND c.decision IN(1,2)"

// 新COMPLETE在提交事务中直接退休。旧版本少量成功局每轮最多八条只退休调度字段，
// 不动state4、deadline、原Create、票或双outbox；并发更新用原retry值与同终局原件CAS。
func (s *Store) retireTerminalOutcomeSchedules(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT f.battle_id,f.retry_at_ms FROM match_formation f JOIN match_battle_completion c ON "+matchingOutcomeTerminalSQL+" WHERE f.state=4 AND f.retry_at_ms<? AND "+recoverableOutcomeShapeSQL+" ORDER BY f.retry_at_ms,f.battle_id LIMIT 8", terminalOutcomeNoRetryMs)
	if err != nil {
		return err
	}
	type schedule struct{ battle, retryMs uint64 }
	var schedules []schedule
	for rows.Next() {
		var item schedule
		if err := rows.Scan(&item.battle, &item.retryMs); err != nil {
			rows.Close()
			return err
		}
		schedules = append(schedules, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range schedules {
		if _, err := s.DB.ExecContext(ctx, "UPDATE match_formation f JOIN match_battle_completion c ON "+matchingOutcomeTerminalSQL+" SET f.retry_at_ms=? WHERE f.battle_id=? AND f.state=4 AND f.retry_at_ms=? AND "+recoverableOutcomeShapeSQL, terminalOutcomeNoRetryMs, item.battle, item.retryMs); err != nil {
			return err
		}
	}
	return nil
}

// ExpiredOutcomeLanes 是到期终局恢复按 battle_id 取模分出的车道数(2026-11-05,吞吐随副本数扩展)。
// 修复前每个副本每拍都从头扫同一页的 8 局、在同一把行锁上排队,后到的副本只读到既有 ABORT,整体吞吐与副本数无关
// (battle 整批崩溃、上万局待作废时要几十分钟才放完票)。现在每个副本每拍扫一条车道,车道顺序每圈随机重排
// (logic/global_outcome_recover.go),多个副本自然落在不同车道上,吞吐约为 副本数 × 每页上限 / 每拍。
// 分车道只决定谁先访问哪一局,不授权任何写入:裁决仍在原 P→formation 锁内,终局主键保证每局只落一份决定。
// battle_id 由发号器打散(AGENTS.md §7.1),取模分布均匀。车道数改了只影响调度,不影响正确性。
const ExpiredOutcomeLanes uint64 = 16

// ExpiredOutcomePageSize 是到期终局恢复一页的上限,与 2026-11-05 之前写死的 LIMIT 8 相同。
const ExpiredOutcomePageSize = 8

// ExpiredOutcomeCursor 是到期终局恢复一条车道里的翻页游标(按 retry_at_ms、battle_id 升序),只用于调度。
// 零值表示从头扫;扫到末尾回零值。
type ExpiredOutcomeCursor struct{ AfterRetryMs, AfterBattle uint64 }

// RecoverExpiredOutcomes 是普通PVE/PVP与活动共用的全量恢复页:先退休旧版本 COMPLETE 留下的调度字段
// (retireTerminalOutcomeSchedules),再不分车道(全部候选)扫一页。
// 两条 SQL 都沿 (state,retry_at_ms) 走完在途的 state 4 行(Finish(true) 不改 retry_at_ms,开打中的局租约早已到期),
// 成本与在途局数成正比、与积压多少无关,所以生产循环每圈(ExpiredOutcomeLanes 拍)只调它一次,每拍只跑便宜的车道页
// (2026-11-05 评审修正:本批最初把退休放在车道页里、车道页不满还补一页全量页,积压为空时每副本每拍要全量扫四遍)。
func (s *Store) RecoverExpiredOutcomes(ctx context.Context, afterRetryMs, afterBattle uint64) (uint64, uint64, error) {
	if err := s.retireTerminalOutcomeSchedules(ctx); err != nil {
		return afterRetryMs, afterBattle, err
	}
	next, err := s.RecoverExpiredOutcomeLane(ctx, 1, 0, ExpiredOutcomeCursor{AfterRetryMs: afterRetryMs, AfterBattle: afterBattle})
	return next.AfterRetryMs, next.AfterBattle, err
}

// RecoverExpiredOutcomeLane 扫 battle_id % lanes == lane 这条车道的一页候选(lanes=1 即全部候选):先逐局读整行,再在锁外
// 并行向各局原执行实例取证(probeDeadlinePage,2026-11-06),最后逐局在原 P→formation 锁内裁决(recoverExpiredOutcome)。
// 沿原state/retry_at_ms索引每页至多 ExpiredOutcomePageSize 局，只选尚无合法终局的v1资格形状。
// 游标只用于调度；故障项仍推进，下轮重访，旧0/明确采用0保持UNKNOWN且不占候选页。返回本车道的新游标,扫到末尾回零值。
// 等证据的局由裁决把下次取证排进 retry_at_ms(全集群共用),排期到之前不再进候选页。
// 不退休旧调度字段(那是全量扫描,只在 RecoverExpiredOutcomes 里做)。
func (s *Store) RecoverExpiredOutcomeLane(ctx context.Context, lanes, lane uint64, cursor ExpiredOutcomeCursor) (ExpiredOutcomeCursor, error) {
	if lanes == 0 || lane >= lanes {
		return cursor, fmt.Errorf("到期终局恢复车道无效 lanes=%d lane=%d", lanes, lane)
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return cursor, err
	}
	if stamp < terminalOutcomeGraceMs {
		return ExpiredOutcomeCursor{}, nil
	}
	// 先减宽限再绑定SQL参数，避免deadline+grace的uint64溢出；锁内仍重新读SQL时钟。
	// 车道条件只读 battle_id(二级索引里本就带主键),在索引侧就能过滤,车道页的回表约为全量页的 1/lanes(由索引结构推算,未压测)。
	rows, err := s.DB.QueryContext(ctx, "SELECT f.retry_at_ms,f.battle_id FROM match_formation f WHERE f.state IN(2,3,4) AND "+recoverableOutcomeShapeSQL+" AND f.retry_at_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND f.deadline_ms>0 AND f.deadline_ms<=? AND f.battle_id%?=? AND (f.retry_at_ms>? OR (f.retry_at_ms=? AND f.battle_id>?)) AND NOT EXISTS(SELECT 1 FROM match_battle_completion c WHERE "+matchingOutcomeTerminalSQL+") ORDER BY f.retry_at_ms,f.battle_id LIMIT "+strconv.Itoa(ExpiredOutcomePageSize), stamp-terminalOutcomeGraceMs, lanes, lane, cursor.AfterRetryMs, cursor.AfterRetryMs, cursor.AfterBattle)
	if err != nil {
		return cursor, err
	}
	type candidate struct{ retryMs, battle uint64 }
	var formations []candidate
	for rows.Next() {
		var f candidate
		if err := rows.Scan(&f.retryMs, &f.battle); err != nil {
			rows.Close()
			return cursor, err
		}
		formations = append(formations, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cursor, err
	}
	if len(formations) == 0 {
		return ExpiredOutcomeCursor{}, nil
	}
	// 先用独立标量游标读整行、再解析完整原件:损坏的一条记失败、不卡住整页,页尾游标照样推进到最后一个候选。
	type visit struct {
		at ExpiredOutcomeCursor
		f  *pb.MatchFormationRecord
	}
	var failures []error
	var visits []visit
	for _, c := range formations {
		if err := ctx.Err(); err != nil {
			return cursor, errors.Join(append(failures, err)...)
		}
		original, err := s.Formation(ctx, c.battle)
		if err != nil {
			failures = append(failures, fmt.Errorf("到期成局恢复未确认 battle=%d: %w", c.battle, err))
			continue
		}
		if original.RetryAtMs != c.retryMs {
			continue // 并发worker换了租约、或别的副本刚排了下次取证，后续完整轮次重访，绝不沿旧观察写入。
		}
		visits = append(visits, visit{at: ExpiredOutcomeCursor{AfterRetryMs: c.retryMs, AfterBattle: c.battle}, f: original})
	}
	records := make([]*pb.MatchFormationRecord, len(visits))
	for i, v := range visits {
		records[i] = v.f
	}
	// 取证只发 RPC、不持锁、不在事务里;一页并行,原实例连不上时整页最多多等一个 DeadlineProbeTimeout。
	evidence := s.probeDeadlinePage(ctx, stamp, records)
	for i, v := range visits {
		if err := ctx.Err(); err != nil {
			return cursor, errors.Join(append(failures, err)...)
		}
		cursor = v.at
		if _, err := s.recoverExpiredOutcomeReported(ctx, v.f, evidence[i]); err != nil {
			failures = append(failures, fmt.Errorf("到期成局恢复未确认 battle=%d: %w", v.f.BattleId, err))
		}
	}
	last := formations[len(formations)-1]
	return ExpiredOutcomeCursor{AfterRetryMs: last.retryMs, AfterBattle: last.battle}, errors.Join(failures...)
}

// RecoverExpiredOutcome 不根据节点死亡猜终局。原发房前能力与完整 v1 Create证明所有个人效果必须先过本SQL门；
// 到期后先在锁外向原执行实例取一次证(deadlineEvidence,2026-11-06),再在原P→formation锁内按 judgeDeadlineAbort 争唯一ABORT:
// 有「原实例没有冻结待提交的终局」的证据才作废,原实例仍持有房间或判不了就等,最多等到硬上限。
// 原工作租约仍有效时不抢占(也不取证)；多副本重放只读取同原操作/UUID的已提交终局。
//
// 不可恢复成局(ErrUnrecoverableFormation)在这里计数并打 ERROR,对外仍回 UNKNOWN、nil:现有调用方
// (RecoverExpiredOutcomes 的整轮失败、logic globalAbortPrepared 的 HoldRelay)会把非 nil error 当成本轮未确认的故障,
// 而这一类是永久状态,不是本轮故障。要区分它的新调用方用 recoverExpiredOutcome 拿哨兵。
func (s *Store) RecoverExpiredOutcome(ctx context.Context, observed *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	evidence, err := s.deadlineEvidence(ctx, observed)
	if err != nil {
		return 0, err
	}
	return s.recoverExpiredOutcomeReported(ctx, observed, evidence)
}

// recoverExpiredOutcomeReported 是到期恢复的单局裁决(RecoverExpiredOutcome 与车道页共用):不可恢复成局计数、打 ERROR 后
// 按 UNKNOWN、nil 回(理由见 RecoverExpiredOutcome)。evidence 是调用方在锁外取到的截止作废证据。
func (s *Store) recoverExpiredOutcomeReported(ctx context.Context, observed *pb.MatchFormationRecord, evidence DeadlineAbortEvidence) (pb.BattleOutcomeDecision, error) {
	decision, err := s.recoverExpiredOutcome(ctx, observed, evidence)
	if errors.Is(err, ErrUnrecoverableFormation) {
		reportUnrecoverableFormation(observed.GetBattleId(), err)
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	return decision, err
}

// recoverExpiredOutcome 是到期恢复的锁内裁决;形状合格却过不了原件核对时回 ErrUnrecoverableFormation(决定为 UNKNOWN)。
// evidence 只在锁外取得(事务里不调 RPC)。判要等时把下次取证的时刻写进 retry_at_ms,与判定在同一把锁、同一个事务里。
func (s *Store) recoverExpiredOutcome(ctx context.Context, observed *pb.MatchFormationRecord, evidence DeadlineAbortEvidence) (pb.BattleOutcomeDecision, error) {
	if observed == nil {
		return 0, errors.New("到期恢复缺持久创建原件")
	}
	if _, err := outcomeAdjudicable(observed); err != nil {
		if errors.Is(err, ErrUnrecoverableFormation) {
			return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, err
		}
		// 旧协议 / 能力证据不全 / operation 为空:不归自动路径,保持 UNKNOWN(过截止的由巡检 CountStrandedExpiredFormations
		// 计数,用 formationfix 处置)。
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	readExisting := func() (pb.BattleOutcomeDecision, bool, error) { return s.committedTerminal(ctx, observed) }
	if decision, exists, err := readExisting(); err != nil || exists {
		return decision, err
	}
	// 缺终局的 state 5 只来自旧代码(新代码写 state 5 时同一事务写终局,或根本没有 Create 原件):不归自动路径,保持 UNKNOWN。
	// 原票可能已换轮，不能补锁当前新票或以旧状态号倒造一份安全ABORT。
	if !formationInFlight(observed) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	tx, f, _, err := s.terminalFormation(ctx, observed.BattleId)
	if err != nil {
		// 另一副本可能刚持P提交ABORT并关闭原票；不能把同原件的成功重放当角色丢失。
		if decision, exists, readErr := readExisting(); readErr != nil || exists {
			return decision, readErr
		}
		return 0, err
	}
	defer tx.Rollback()
	if !proto.Equal(f, observed) {
		_ = tx.Rollback()
		if decision, exists, err := readExisting(); err != nil || exists {
			return decision, err
		}
		return 0, ErrWorkLease
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	// 原工作租约、或上次取证排下的间隔(含持有宽限)还没过:不抢占、不改排期。
	if f.RetryAtMs > stamp || !formationInFlight(f) {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	decision, verdict, err := s.abortTerminalTx(ctx, tx, f, evidence)
	if err != nil {
		return decision, err
	}
	if !verdict.abort {
		if verdict.nextProbeMs == 0 {
			return decision, nil
		}
		// 只是排期,不授权任何写入:与判定同在这把锁里,COMPLETE、别的 ABORT 入口与别的副本都改不了这一行。
		// 提交结果未知也无害,下一轮按读到的 retry_at_ms 重来。
		if _, err = tx.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=? WHERE battle_id=?", verdict.nextProbeMs, f.BattleId); err != nil {
			return 0, err
		}
		if err = tx.Commit(); err != nil {
			return 0, fmt.Errorf("截止作废取证排期提交未知: %w", err)
		}
		observeDeadlineWait(evidence)
		return decision, nil
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("到期ABORT提交未知，必须复用原件重查: %w", err)
	}
	observeDeadlineAbort(f.BattleId, verdict, evidence, terminalAbortDeadline)
	return decision, nil
}

// ReadyAfter 的游标仅调度巡检，不能授权释放。即使前面的房间一直 UNKNOWN，
// 下一页也照常访问；进程重启从头扫描不会改变任何票据的业务状态。
//
// 按片分工(2026-11-06,global-match-hierarchical-pools.md §5.2):只读本副本负责的片(片号 = player_id % WorkShards,
// work_owner.go 的 workReadyTicket),条件在 LIMIT 之前,加副本就分摊,扫一圈的时间随副本数缩短;没绑归属(单测)时条件是 1=1。
// 归属只管分工:释放的互斥仍在 ReleaseCompletedReady 的原 P→票锁与条件更新。副本下线或成员表变化时,这一片在本副本下一轮
// 计算归属时就归新的负责者,游标扫到末尾回零值后从头访问,所以接手的片在一圈之内被扫到。
func (s *Store) ReadyAfter(ctx context.Context, afterMs, afterPlayer uint64) ([]*pb.MatchAuthorityTicketRecord, error) {
	owned, shardArgs := s.WorkShardFilter(workReadyTicket, "player_id")
	args := append([]any{afterMs, afterMs, afterPlayer}, shardArgs...)
	rows, err := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=3 AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) AND "+owned+" ORDER BY enqueued_ms,player_id LIMIT 8", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ticket)
	}
	return out, rows.Err()
}

// ReleaseCompletedReady 只认已归档且校验完整的本玩家结算原件，不使用客户端
// 状态、缓存或超时猜测房间结束。原角色P锁先于票锁；旧巡检不能释放新一轮票。
func (s *Store) ReleaseCompletedReady(ctx context.Context, observed *pb.MatchAuthorityTicketRecord) (released bool, err error) {
	err = s.Fenced(ctx, func() (e error) { released, e = s.releaseCompletedReadyOnce(ctx, observed); return e })
	return released, err
}

func (s *Store) releaseCompletedReadyOnce(ctx context.Context, observed *pb.MatchAuthorityTicketRecord) (bool, error) {
	if observed == nil || observed.State != 3 || observed.FormationId == 0 || observed.Ticket == "" {
		return false, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, []*pb.MatchAuthorityTicketRecord{observed}); err != nil {
		return false, err
	}
	var battle uint64
	// 同一原owner的签名永久APPLIED收据才是释放依据；归档/入箱/水位猜测都不能代替。
	err = tx.QueryRowContext(ctx, "SELECT s.battle_id FROM match_battle_settlement_source s JOIN match_battle_completion c ON c.battle_id=s.battle_id WHERE s.battle_id=? AND s.player_id=? AND s.owner_account=? AND s.owner_revision=? AND s.dispatch_state=2 AND s.inbox_state=2 AND s.inbox_seq>0 AND c.decision=1 AND c.outcome_protocol_version=1", observed.FormationId, observed.PlayerId, observed.OwnerAccount, observed.OwnerRevision).Scan(&battle)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND formation_id=? AND state=3 AND owner_account=? AND owner_revision=?", observed.PlayerId, observed.Ticket, observed.FormationId, observed.OwnerAccount, observed.OwnerRevision)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return changed != 0, nil
}
