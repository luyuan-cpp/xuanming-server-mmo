package authority

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	battlepb "proto/battle"
	pb "proto/match"
)

// 2026-11-06 批 B5:截止作废先取证、「哪些局会有终局」只有一个判据、过截止的非 v1 成局不再空转(terminal_abort.go、
// ready_recovery.go、completion.go、direct_cancel.go、terminal_read.go、store.go)。纯函数不碰 SQL;真 TiDB 用例沿用
// MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB 与 formation_recovery_exit_test.go 的夹具,不起 battle(取证用替身),不依赖墙钟:
// 截止与排期都用 TiDB 当前时刻推出的固定值注入。

// fixtureNoRoomProbe 是 testStore 的缺省取证:原执行实例已证明没有冻结待提交的终局。它让 2026-11-06 之前的用例保持
// 「过了截止加宽限即作废」的原意;专测取证的用例自己换(BindDeadlineAbortProbe)。
func fixtureNoRoomProbe(context.Context, *pb.MatchFormationRecord) DeadlineAbortEvidence {
	return DeadlineEvidenceNoRoom
}

// scriptedProbe 按设定的证据作答并记次数;可被并行取证的多个 goroutine 同时调用。
type scriptedProbe struct {
	mu       sync.Mutex
	evidence DeadlineAbortEvidence
	calls    int
}

func (p *scriptedProbe) set(evidence DeadlineAbortEvidence) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evidence = evidence
}

func (p *scriptedProbe) probe(context.Context, *pb.MatchFormationRecord) DeadlineAbortEvidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.evidence
}

func (p *scriptedProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ---- 纯函数 ----

// 截止 ABORT 的时间判据逐条:没过截止加宽限不作废;有证据就作废(不论硬上限);没到硬上限等,排期截在硬上限上;
// 过了硬上限第一次看到房间给一次持有宽限,宽限内不论证据都等,宽限过了作废;过了硬上限判不了作废。
func TestJudgeDeadlineAbortTable(t *testing.T) {
	const deadline uint64 = 1_800_000_000_000
	due := deadline + terminalOutcomeGraceMs
	capAt := deadlineAbortCapMs(deadline)
	require.Equal(t, deadline+terminalOutcomeGraceMs+deadlineAbortHardCapMs, capAt)
	wait := deadlineAbortVerdict{}
	for _, c := range []struct {
		why      string
		stamp    uint64
		retry    uint64
		evidence DeadlineAbortEvidence
		want     deadlineAbortVerdict
	}{
		{"宽限差 1 毫秒", due - 1, 1, DeadlineEvidenceNoRoom, wait},
		{"刚过宽限、没有房间", due, 1, DeadlineEvidenceNoRoom, deadlineAbortVerdict{abort: true}},
		{"刚过宽限、原进程已不在", due, 1, DeadlineEvidenceInstanceGone, deadlineAbortVerdict{abort: true}},
		{"刚过宽限、原实例仍持有房间", due, 1, DeadlineEvidenceBattleStillHolds, deadlineAbortVerdict{nextProbeMs: due + deadlineProbeBackoffMs}},
		{"刚过宽限、判不了", due, 1, DeadlineEvidenceUndetermined, deadlineAbortVerdict{nextProbeMs: due + deadlineProbeBackoffMs}},
		{"临近硬上限:排期截在硬上限上", capAt - 1000, 1, DeadlineEvidenceUndetermined, deadlineAbortVerdict{nextProbeMs: capAt}},
		{"硬上限、判不了", capAt, capAt, DeadlineEvidenceUndetermined, deadlineAbortVerdict{abort: true, capped: true}},
		{"硬上限、第一次看到房间", capAt, capAt, DeadlineEvidenceBattleStillHolds, deadlineAbortVerdict{nextProbeMs: capAt + deadlineHeldPatienceMs}},
		{"持有宽限内、判不了也等", capAt + deadlineHeldPatienceMs - 1, capAt + deadlineHeldPatienceMs, DeadlineEvidenceUndetermined, wait},
		{"持有宽限内、仍持有", capAt + deadlineHeldPatienceMs - 1, capAt + deadlineHeldPatienceMs, DeadlineEvidenceBattleStillHolds, wait},
		{"持有宽限已过、仍持有", capAt + deadlineHeldPatienceMs, capAt + deadlineHeldPatienceMs, DeadlineEvidenceBattleStillHolds, deadlineAbortVerdict{abort: true, capped: true}},
		{"持有宽限内有了证据", capAt + 1, capAt + deadlineHeldPatienceMs, DeadlineEvidenceNoRoom, deadlineAbortVerdict{abort: true}},
		{"取值认不出按判不了", due, 1, DeadlineAbortEvidence(99), deadlineAbortVerdict{nextProbeMs: due + deadlineProbeBackoffMs}},
	} {
		require.Equal(t, c.want, judgeDeadlineAbort(deadline, c.stamp, c.retry, c.evidence), c.why)
	}
	require.Equal(t, wait, judgeDeadlineAbort(0, math.MaxUint64, 1, DeadlineEvidenceNoRoom), "没有截止的原件不作废")
	require.Equal(t, uint64(math.MaxUint64), deadlineAbortCapMs(math.MaxUint64-1), "硬上限上溢饱和")
	near := uint64(math.MaxUint64) - terminalOutcomeGraceMs - 1000
	require.Equal(t, deadlineAbortVerdict{nextProbeMs: math.MaxUint64}, judgeDeadlineAbort(near, near+terminalOutcomeGraceMs, 1, DeadlineEvidenceUndetermined),
		"临近上界:判不了就一直等,排期饱和,不因加法溢出提前作废或排回过去")
}

// scene 与 battle 侧和截止作废上限一起定的常量:写的是 C++ 源码里的值,TestDeadlineAbortSceneConstantsMatchCpp 逐个比对源码,
// C++ 改了不同步这里就转红。
const (
	sceneTerminalGraceMs  uint64 = 60 * 1000      // player_battle.h PlayerBattleSystem::kTerminalOutcomeGraceMs
	sceneDeliveryMarginMs uint64 = 60 * 1000      // player_battle.h PlayerBattleSystem::kInboxSettlementDeliveryMarginMs
	sceneFreezeHoldCapMs  uint64 = 15 * 60 * 1000 // player_battle.h PlayerBattleSystem::kFreezeHoldCapMs
	battleSlowReplayMs    uint64 = 60 * 1000      // result_commit.h turnbattle::kSqlOutcomeSlowRetryInterval(C++ 里是秒)
)

// cppConstant 从仓库里的 C++ 源码读一个整数常量(只认「名字 = 字面量相乘;」与「名字{字面量}」两种写法,字面量可带 ULL 后缀)。
// 只拷了 go/ 的构建环境里找不到 C++ 源码时跳过调用它的用例。
func cppConstant(t *testing.T, relative, name string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", filepath.FromSlash(relative)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("C++ source %s not found; constant cross-check skipped", relative)
	}
	require.NoError(t, err)
	match := regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\s*(?:=\s*([0-9ULul\s*]+);|\{\s*([0-9]+)\s*\})`).FindSubmatch(raw)
	require.NotNil(t, match, "%s 里找不到 %s 的定义", relative, name)
	expr := string(match[1]) + string(match[2])
	value := uint64(1)
	for _, factor := range strings.Split(expr, "*") {
		n, err := strconv.ParseUint(strings.TrimRight(strings.TrimSpace(factor), "ULul"), 10, 64)
		require.NoError(t, err, "%s 的 %s 不是字面量相乘: %q", relative, name, expr)
		value *= n
	}
	return value
}

// 评审补(2026-11-06):上限不等式里的 scene / battle 常量与 C++ 源码一致。
func TestDeadlineAbortSceneConstantsMatchCpp(t *testing.T) {
	const playerBattle = "cpp/libs/services/scene/battle/system/player_battle.h"
	const resultCommit = "cpp/libs/services/battle/system/result_commit.h"
	require.Equal(t, sceneTerminalGraceMs, cppConstant(t, playerBattle, "kTerminalOutcomeGraceMs"))
	require.Equal(t, sceneDeliveryMarginMs, cppConstant(t, playerBattle, "kInboxSettlementDeliveryMarginMs"))
	require.Equal(t, sceneFreezeHoldCapMs, cppConstant(t, playerBattle, "kFreezeHoldCapMs"))
	require.Equal(t, battleSlowReplayMs, 1000*cppConstant(t, resultCommit, "kSqlOutcomeSlowRetryInterval"))
}

// 硬上限与 scene 冻结到期之后的续冻一起定(cpp/libs/services/scene/battle/system/player_battle.h;三个常量任一改动,本用例与
// terminal_abort.go 的说明同批改)。match 可用时截止 ABORT 的最晚时刻要经过两次访问(评审补 2026-11-10,原先只算了一次):
// 截止前最后一次排期截在硬上限上,越过硬上限的第一次访问最晚在 硬上限 + d1 才到,看到房间还在,持有宽限从这次的锁内时刻起算
// (nextProbeMs = 锁内时刻 + 持有宽限),不是从硬上限起算;宽限过后还要再等一次访问 d2 才作废。所以最晚在
// 截止 + 宽限 + 硬上限 + d1 + 持有宽限 + d2(现值约截止 + 14.2 分钟)。每次访问的延迟 = 同一车道两次访问最多隔两圈(每圈
// ExpiredOutcomeLanes 拍,车道顺序每圈重新洗牌)+ 这一拍里锁前的取证(车道页与全量页各至多一个取证期限)。它必须早于 scene 按
// 续冻上限解冻,scene 才能按 SQL 终局解冻,而不是在「结果未知」时被迫解冻。
// 每拍的耗时分两档核对:原实例都当场作答时每拍 1 秒(globalOutcomeRecoverInterval);原 battle 被黑洞时一页要等满取证期限,
// 一拍拖到约 1 秒加一个取证期限(ticker 丢掉跑不完的拍、不补发,下一拍紧接着跑),一圈约 48 秒,两档都要成立。每拍两页都等满
// 取证期限、SQL 也慢到用满一拍预算(globalOutcomeRecoverBudget)那种更坏的情形不保证,与积压同归残余。
// 硬上限还要盖住工作租约,「retry_at_ms 越过硬上限」才是持有宽限的记号;持有宽限要长于 battle 越界之后的慢速重放间隔
// (result_commit.h kSqlOutcomeSlowRetryInterval = 60 秒)。
// 前提是恢复跟得上:2026-11-11 起一拍在预算之内把一条车道连续扫空(logic/global_outcome_recover.go),一条车道同时到期超过一页
// 不再要隔几圈;一拍预算也清不完的积压(或 match 长期不可用)由每分钟巡检按 deadlineAbortOverdueMs 报「超期未作废」,
// 它就是下面黑洞那一档的最晚落定时刻。那时 scene 按上限解冻,晚到的结算由战斗道具预留兜住(docs/design/battle-item-reservation.md)。
func TestDeadlineAbortBoundsFitSceneFreezeHold(t *testing.T) {
	const sceneHoldDeadlineAfterDue = sceneTerminalGraceMs + sceneDeliveryMarginMs + sceneFreezeHoldCapMs
	recoveryTickMs := uint64(ExpiredOutcomeRecoverTick / time.Millisecond)
	probeTimeoutMs := uint64(DeadlineProbeTimeout / time.Millisecond)
	require.Equal(t, sceneTerminalGraceMs, terminalOutcomeGraceMs, "终局宽限 match 与 scene 同值")
	visitDelayMs := func(tickMs uint64) uint64 { return 2*ExpiredOutcomeLanes*tickMs + 2*probeTimeoutMs }
	matchLatest := func(tickMs uint64) uint64 {
		return terminalOutcomeGraceMs + deadlineAbortHardCapMs + deadlineHeldPatienceMs + 2*visitDelayMs(tickMs)
	}
	require.Less(t, matchLatest(recoveryTickMs), sceneHoldDeadlineAfterDue, "match 可用时截止 ABORT 必须早于 scene 按续冻上限解冻")
	require.Less(t, matchLatest(recoveryTickMs+probeTimeoutMs), sceneHoldDeadlineAfterDue, "原 battle 被黑洞、每拍多等一个取证期限时也要早于")
	require.Equal(t, matchLatest(recoveryTickMs+probeTimeoutMs), deadlineAbortOverdueMs, "「超期未作废」的巡检阈值就是黑洞那一档的最晚落定时刻")
	require.LessOrEqual(t, WorkLeaseMs, terminalOutcomeGraceMs+deadlineAbortHardCapMs, "工作租约不得越过硬上限")
	require.Greater(t, deadlineHeldPatienceMs, battleSlowReplayMs, "持有宽限至少盖住 battle 的一次慢速重放")
	require.Greater(t, deadlineAbortHardCapMs, deadlineProbeBackoffMs, "硬上限之内至少取证两次以上")
}

// 要不要取证只看 deadlineAbortPending:原件可裁决、在途、已过截止加宽限;不看排期(retry_at_ms),排期只在锁内按锁内时刻复核
// (评审修正 2026-11-06:取证用的时刻早于锁内时刻,拿它比排期会漏取证)。
func TestDeadlineAbortPendingIgnoresSchedule(t *testing.T) {
	f := terminalReadFormation(t)
	due := f.DeadlineMs + terminalOutcomeGraceMs
	f.RetryAtMs = due
	require.True(t, deadlineAbortPending(f, due))
	require.False(t, deadlineAbortPending(f, due-1), "宽限没过")
	f.RetryAtMs = deadlineAbortCapMs(f.DeadlineMs)
	require.True(t, deadlineAbortPending(f, due), "排期晚于取证时刻也问:锁内时刻可能已过排期")
	for why, edit := range map[string]func(*pb.MatchFormationRecord){
		"缺终局的 state 5": func(f *pb.MatchFormationRecord) { f.State = 5 },
		"state 1":       func(f *pb.MatchFormationRecord) { f.State = 1 },
		"终局协议 0":        func(f *pb.MatchFormationRecord) { f.OutcomeProtocolVersion = 0 },
		"operation 为空":  func(f *pb.MatchFormationRecord) { f.OperationId = "" },
	} {
		g := terminalReadFormation(t)
		g.RetryAtMs = 1
		edit(g)
		require.False(t, deadlineAbortPending(g, due), why)
	}
	require.False(t, deadlineAbortPending(nil, due))
}

// 一页并行取证只问截止 ABORT 只差证据的局,其余位置(含 nil:已有终局行的候选)回 Undetermined;取证 panic 不吞掉。没装配取证时
// 单局取证与帮会取消取证都不读 SQL(零值 Store 也不会碰库)。
// 评审修正(2026-11-06)的回归:候选 SQL 按 SQL 时刻筛过排期,页时刻读得更早;retry_at_ms 正好截在硬上限上、晚于页时刻的局
// 照样要问。修复前它不取证就进锁,锁内时刻已到硬上限,拿着 Undetermined 跳过持有宽限、无证据作废。
func TestProbeDeadlinePageAsksPendingFormations(t *testing.T) {
	due := terminalReadFormation(t)
	due.RetryAtMs = 1
	atCap := terminalReadFormation(t)
	capAt := deadlineAbortCapMs(atCap.DeadlineMs)
	atCap.RetryAtMs = capAt
	pageStamp := capAt - 1 // 页时刻早于排期;SQL 时刻与锁内时刻都已到 capAt
	legacy := terminalReadFormation(t)
	legacy.RetryAtMs, legacy.OutcomeProtocolVersion = 1, 0
	probe := &scriptedProbe{evidence: DeadlineEvidenceBattleStillHolds}
	s := &Store{deadlineProbe: probe.probe}
	got := s.probeDeadlinePage(context.Background(), pageStamp, []*pb.MatchFormationRecord{due, atCap, legacy, nil})
	require.Equal(t, []DeadlineAbortEvidence{DeadlineEvidenceBattleStillHolds, DeadlineEvidenceBattleStillHolds, DeadlineEvidenceUndetermined, DeadlineEvidenceUndetermined}, got)
	require.Equal(t, 2, probe.count())
	require.Equal(t, deadlineAbortVerdict{nextProbeMs: capAt + deadlineHeldPatienceMs}, judgeDeadlineAbort(atCap.DeadlineMs, capAt, atCap.RetryAtMs, got[1]),
		"锁内时刻到了硬上限:拿到了房间还在的证据,给持有宽限")
	require.Equal(t, deadlineAbortVerdict{abort: true, capped: true}, judgeDeadlineAbort(atCap.DeadlineMs, capAt, atCap.RetryAtMs, DeadlineEvidenceUndetermined),
		"对照:没问就进锁,会无证据作废")

	panicking := &Store{deadlineProbe: func(context.Context, *pb.MatchFormationRecord) DeadlineAbortEvidence { panic("fixture probe boom") }}
	require.PanicsWithValue(t, "fixture probe boom", func() {
		panicking.probeDeadlinePage(context.Background(), pageStamp, []*pb.MatchFormationRecord{due})
	})

	unbound := &Store{}
	evidence, err := unbound.deadlineEvidence(context.Background(), due)
	require.NoError(t, err)
	require.Equal(t, DeadlineEvidenceUndetermined, evidence, "没装配取证:判不了,截止 ABORT 只在硬上限之后写")
	evidence, err = unbound.deadlineEvidenceOf(context.Background(), due.BattleId)
	require.NoError(t, err)
	require.Equal(t, DeadlineEvidenceUndetermined, evidence)
}

// 「哪些局会有终局」只有一个判据(autoTerminalOriginal):scene 查询(TerminalOutcomeExpected)、截止 ABORT 与到期恢复的取证
// (deadlineAbortPending)、人工处置工具的 AUTO 类(classifyStranded)对每一类成局给出同一结论。
func TestTerminalOutcomeExpectedIsTheAutomaticTerminalPredicate(t *testing.T) {
	for _, c := range []struct {
		why  string
		edit func(*pb.MatchFormationRecord)
		want bool
	}{
		{"可裁决 v1 state 2", func(*pb.MatchFormationRecord) {}, true},
		{"可裁决 v1 state 3", func(f *pb.MatchFormationRecord) { f.State = 3 }, true},
		{"可裁决 v1 state 4、采用回执 1", func(f *pb.MatchFormationRecord) {
			f.State, f.AdoptedOutcomeObserved, f.AdoptedOutcomeProtocolVersion = 4, true, 1
		}, true},
		{"终局协议 0", func(f *pb.MatchFormationRecord) { f.OutcomeProtocolVersion = 0 }, false},
		{"battle 明确采用旧协议", func(f *pb.MatchFormationRecord) { f.AdoptedOutcomeObserved = true }, false},
		{"预检实例与目标不符", func(f *pb.MatchFormationRecord) { f.PreflightInstanceId = "other-instance" }, false},
		{"原件截止与成局不符", func(f *pb.MatchFormationRecord) { f.DeadlineMs++ }, false},
		{"operation 为空(原件也为空,原件判据放行,到期恢复不选)", func(f *pb.MatchFormationRecord) {
			r := &battlepb.CreateBattleRequest{}
			if proto.Unmarshal(f.CreateRequest, r) != nil {
				panic("fixture create")
			}
			r.PlacementOperationId = ""
			f.OperationId = ""
			f.CreateRequest, _ = proto.MarshalOptions{Deterministic: true}.Marshal(r)
		}, false},
		{"缺终局的 state 5", func(f *pb.MatchFormationRecord) { f.State = 5 }, false},
		{"state 1", func(f *pb.MatchFormationRecord) { f.State = 1 }, false},
	} {
		f := terminalReadFormation(t)
		c.edit(f)
		require.Equal(t, c.want, TerminalOutcomeExpected(f), c.why)
		require.Equal(t, c.want, deadlineAbortPending(f, f.DeadlineMs+terminalOutcomeGraceMs), "%s:截止 ABORT 与 scene 查询同一判据", c.why)
		if formationInFlight(f) {
			auto := classifyStranded(f).class == pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_AUTO
			require.Equal(t, c.want, auto, "%s:人工处置的 AUTO 类与自动路径同一口径", c.why)
		}
	}
	require.False(t, TerminalOutcomeExpected(nil))
}

// ---- 真 TiDB ----

// rewindDeadline 把成局行与持久 v1 原件的创建、截止时刻一起改成「截止在 TiDB 时刻之前 agoMs」,retry_at_ms 置 1
// (原工作租约早已到期),返回改后的成局行与改之前读到的 TiDB 时刻。两处时刻必须同改(supportedOutcomeOriginal 逐项核对)。
func rewindDeadline(t *testing.T, s *Store, battleID, agoMs uint64) (*pb.MatchFormationRecord, uint64) {
	t.Helper()
	ctx := context.Background()
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	request := &battlepb.CreateBattleRequest{}
	require.NoError(t, proto.Unmarshal(f.CreateRequest, request))
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	request.DeadlineMs = stamp - agoMs
	request.CreatedAtMs = request.DeadlineMs - 300_000
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET created_ms=?,deadline_ms=?,create_request=?,retry_at_ms=1 WHERE battle_id=?", request.CreatedAtMs, request.DeadlineMs, raw, battleID)
	require.NoError(t, err)
	f, err = s.Formation(ctx, battleID)
	require.NoError(t, err)
	return f, stamp
}

func requireNoTerminal(t *testing.T, s *Store, battleID uint64, why string) {
	t.Helper()
	row, err := readTerminal(context.Background(), s.DB, battleID)
	require.NoError(t, err)
	require.Nil(t, row, why)
}

func requireDeadlineAbort(t *testing.T, s *Store, battleID uint64) {
	t.Helper()
	ctx := context.Background()
	row, err := readTerminal(ctx, s.DB, battleID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, row.Decision)
	var sourceKind uint32
	require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT source_kind FROM match_battle_completion WHERE battle_id=?", battleID).Scan(&sourceKind))
	require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT, sourceKind)
	f, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.EqualValues(t, 5, f.State)
}

// 审计会话报的场景(回归,修复前转红):截止前已经正常打完、battle 正在重放 CommitBattleOutcome,match 或它的库不可用跨过了
// 截止加宽限。恢复后到期恢复先问原实例,它仍持有房间(PRESENT):不作废,只把下次取证排到 30 秒之后(全集群共用),排期到之前
// 别的轮次不再问;battle 的重放到了,COMPLETE 落库,全员个人结算照常发出。修复前这里直接写截止 ABORT,整局作废、奖励丢失。
func TestDeadlineAbortTiDBWaitsWhileBattleHoldsAndLateCompleteWins(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	probe := &scriptedProbe{evidence: DeadlineEvidenceBattleStillHolds}
	s.BindDeadlineAbortProbe(probe.probe)
	f, complete := terminalResultFixture(t, s, false)
	f, stamp := rewindDeadline(t, s, f.BattleId, terminalOutcomeGraceMs+1000)
	waits := metrics.GlobalFlowValue("terminal", "abort_wait_battle_still_holds")
	deadlineAborts := metrics.GlobalFlowValue("terminal", terminalAbortDeadline)

	_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireNoTerminal(t, s, f.BattleId, "原实例仍持有房间:不作废")
	require.Equal(t, 1, probe.count(), "锁外问了一次原实例")
	deferred, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.EqualValues(t, 2, deferred.State)
	require.Greater(t, deferred.RetryAtMs, stamp, "下次取证排进 retry_at_ms")
	require.LessOrEqual(t, deferred.RetryAtMs, deadlineAbortCapMs(f.DeadlineMs), "没到硬上限的排期截在硬上限上")
	require.Equal(t, waits+1, metrics.GlobalFlowValue("terminal", "abort_wait_battle_still_holds"))
	_, err = s.AcquireWork(ctx, f.BattleId)
	require.ErrorIs(t, err, ErrWorkLease, "排期不是工作租约,过截止的局也不会再领到租约")

	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 1, probe.count(), "排期没到不进候选、不再问 battle")
	// 另一副本拿着改排期之前的观察进锁(多副本同一拍撞上同一局):读到排期已被改过,这一轮跳过,不算故障、不写任何行。
	stale, err := s.RecoverExpiredOutcome(ctx, f)
	require.NoError(t, err, "别的副本刚改了排期:跳过,不报未确认")
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, stale)
	requireNoTerminal(t, s, f.BattleId, "过期的观察不写终局")
	again, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(deferred, again), "也不改排期")

	decision, err := s.CommitBattleOutcome(ctx, complete)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision, "battle 的重放先到,COMPLETE 落库")
	require.Equal(t, 2, settlementSources(t, s, f.BattleId), "全员个人结算来源都在")
	require.Equal(t, deadlineAborts, metrics.GlobalFlowValue("terminal", terminalAbortDeadline), "没有写过截止 ABORT")
	expected, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.True(t, FormationOutcomeDecided(expected))
}

// 没装配取证(或原实例连不上、判不了):截止加宽限之后照样不作废,排期按 30 秒推进;过了硬上限才作废,计 abort_deadline_cap
// (不计 abort_deadline)。fail-closed 的代价有上限。
func TestDeadlineAbortTiDBUndeterminedWaitsUntilHardCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.BindDeadlineAbortProbe(nil)
	f, _ := terminalResultFixture(t, s, false)
	_, stamp := rewindDeadline(t, s, f.BattleId, terminalOutcomeGraceMs+1000)
	_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireNoTerminal(t, s, f.BattleId, "判不了、没到硬上限:不作废")
	deferred, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.Greater(t, deferred.RetryAtMs, stamp)

	capped := metrics.GlobalFlowValue("terminal", terminalAbortDeadlineCap)
	evidenced := metrics.GlobalFlowValue("terminal", terminalAbortDeadline)
	rewindDeadline(t, s, f.BattleId, terminalOutcomeGraceMs+deadlineAbortHardCapMs+1000)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireDeadlineAbort(t, s, f.BattleId)
	require.Equal(t, capped+1, metrics.GlobalFlowValue("terminal", terminalAbortDeadlineCap), "越过硬上限、没有证据的作废单独计数")
	require.Equal(t, evidenced, metrics.GlobalFlowValue("terminal", terminalAbortDeadline))
}

// 有证据(没有房间 / 原进程已不在)就作废,不必等硬上限,计 abort_deadline。
func TestDeadlineAbortTiDBEvidenceAbortsBeforeHardCap(t *testing.T) {
	for _, evidence := range []DeadlineAbortEvidence{DeadlineEvidenceNoRoom, DeadlineEvidenceInstanceGone} {
		t.Run(evidence.String(), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			probe := &scriptedProbe{evidence: evidence}
			s.BindDeadlineAbortProbe(probe.probe)
			f, _ := terminalResultFixture(t, s, false)
			rewindDeadline(t, s, f.BattleId, terminalOutcomeGraceMs+1000)
			evidenced := metrics.GlobalFlowValue("terminal", terminalAbortDeadline)
			_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
			require.NoError(t, err)
			requireDeadlineAbort(t, s, f.BattleId)
			require.Equal(t, evidenced+1, metrics.GlobalFlowValue("terminal", terminalAbortDeadline))
			require.Equal(t, 1, probe.count())
		})
	}
}

// match 不可用超过硬上限:恢复后第一眼就过了硬上限,原实例仍持有房间。给它一次持有宽限(起点记在 retry_at_ms,越过硬上限即记号),
// 宽限内到期恢复不再访问它、帮会取消也不作废(不论这次证据如何);宽限过了仍没提交才作废,计 abort_deadline_cap。
func TestDeadlineAbortTiDBPastCapGivesHeldBattleOnePatienceWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	probe := &scriptedProbe{evidence: DeadlineEvidenceBattleStillHolds}
	s.BindDeadlineAbortProbe(probe.probe)
	f, _ := terminalResultFixture(t, s, false)
	f, stamp := rewindDeadline(t, s, f.BattleId, terminalOutcomeGraceMs+deadlineAbortHardCapMs+1000)
	capAt := deadlineAbortCapMs(f.DeadlineMs)
	require.LessOrEqual(t, capAt, stamp)

	_, _, err := s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireNoTerminal(t, s, f.BattleId, "第一次看到房间:给持有宽限")
	held, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.Greater(t, held.RetryAtMs, capAt, "持有宽限的起点记在 retry_at_ms,越过硬上限即记号")
	require.GreaterOrEqual(t, held.RetryAtMs, stamp+deadlineHeldPatienceMs)

	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	require.Equal(t, 1, probe.count(), "宽限内不进候选")
	tx, locked, _, err := s.terminalFormation(ctx, f.BattleId)
	require.NoError(t, err)
	decision, verdict, err := s.abortTerminalTx(ctx, tx, locked, DeadlineEvidenceUndetermined)
	require.NoError(t, tx.Rollback())
	require.NoError(t, err)
	require.False(t, verdict.abort, "持有宽限内帮会取消同样不作废,即使这次判不了")
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)

	// 宽限已过(retry_at_ms 仍在硬上限之后、但已到期),仍持有:作废。
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=? WHERE battle_id=?", capAt+1, f.BattleId)
	require.NoError(t, err)
	capped := metrics.GlobalFlowValue("terminal", terminalAbortDeadlineCap)
	_, _, err = s.RecoverExpiredOutcomes(ctx, 0, 0)
	require.NoError(t, err)
	requireDeadlineAbort(t, s, f.BattleId)
	require.Equal(t, capped+1, metrics.GlobalFlowValue("terminal", terminalAbortDeadlineCap))
}

// 帮会取消走同一判据:截止加宽限之后原实例仍持有房间,只报 formation=4 / UNKNOWN、不签终局、不改排期;拿到「没有房间」的
// 证据才签 formation=5 / ABORT(计 abort_cancel)。取消在事务之前问原实例,不看排期。
func TestDeadlineAbortTiDBCancelNeedsEvidenceBeforeHardCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	probe := &scriptedProbe{evidence: DeadlineEvidenceBattleStillHolds}
	s.BindDeadlineAbortProbe(probe.probe)
	const battleID = 2801
	a, b := testJoin(t, s, 28011), testJoin(t, s, 28012)
	require.NoError(t, s.Cancel(ctx, a.PlayerId, a.Ticket))
	require.NoError(t, s.Cancel(ctx, b.PlayerId, b.Ticket))
	formation := &pb.MatchFormationRecord{BattleId: battleID, OperationId: "deadline-cancel-op", PhysicalRegion: 1, TargetNodeId: 3,
		TargetInstanceId: "original-instance", TargetEndpoint: "127.0.0.1:1", Mode: a.Mode, ConfigId: a.ConfigId}
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: 47, RequestLow: 481}
	body := []byte("same-entire-original-start")
	_, err := s.ReserveDirect(ctx, key, body, formation, []*pb.MatchAuthorityTicketRecord{a, b})
	require.NoError(t, err)
	startFormation(t, s, battleID)
	expireStartedFormation(t, s, battleID)
	before, err := s.Formation(ctx, battleID)
	require.NoError(t, err)

	receipt, err := s.CancelActivity(ctx, key, body)
	require.NoError(t, err)
	require.EqualValues(t, 4, receipt.FormationState)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, receipt.TerminalDecision, "原实例仍持有房间:帮会取消不签终局")
	require.Equal(t, 1, probe.count(), "取消在事务之前问了原实例(不看排期)")
	unchanged, err := s.Formation(ctx, battleID)
	require.NoError(t, err)
	require.True(t, proto.Equal(before, unchanged), "帮会取消不改排期")

	// 一次取消调用里各次尝试共用一份证据(评审修正 2026-11-06):同一 battle 再要不再问原实例;别的 battle 不沿用;还没成局不问。
	once := &cancelEvidence{}
	first, err := once.of(ctx, s, battleID)
	require.NoError(t, err)
	second, err := once.of(ctx, s, battleID)
	require.NoError(t, err)
	require.Equal(t, DeadlineEvidenceBattleStillHolds, first)
	require.Equal(t, first, second)
	require.Equal(t, 2, probe.count(), "第二次尝试复用,不再问")
	other, err := once.of(ctx, s, battleID+1)
	require.NoError(t, err)
	require.Equal(t, DeadlineEvidenceUndetermined, other, "不是取证所指的那一局")
	unformed, err := (&cancelEvidence{}).of(ctx, s, 0)
	require.NoError(t, err)
	require.Equal(t, DeadlineEvidenceUndetermined, unformed)
	require.Equal(t, 2, probe.count())

	probe.set(DeadlineEvidenceNoRoom)
	cancels := metrics.GlobalFlowValue("terminal", terminalAbortCancel)
	receipt, err = s.CancelActivity(ctx, key, body)
	require.NoError(t, err)
	require.EqualValues(t, 5, receipt.FormationState)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, receipt.TerminalDecision)
	require.Equal(t, cancels+1, metrics.GlobalFlowValue("terminal", terminalAbortCancel))
	requireDeadlineAbort(t, s, battleID)
}

// 评审修正(2026-11-06)的回归:已有终局行、却不是同一原件合法终局的成局(只有归档的行、身份与成局不符的行)照旧是到期恢复的
// 候选(锁外回放,冲突照报),但锁内只回放既有行、从不排期,修复前每圈都向 battle 白问一次,battle 连不上时每页还要多等一个
// 取证期限。现在车道页与单局入口都不问,成局行也不变。
func TestDeadlineAbortTiDBTerminalRowFormationsAreNotProbed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	probe := &scriptedProbe{evidence: DeadlineEvidenceNoRoom}
	s.BindDeadlineAbortProbe(probe.probe)
	const archived, conflicting uint64 = 2921, 2922
	strandedFixture(t, s, archived, nil)
	insertArchivedOnly(t, s, archived)
	strandedFixture(t, s, conflicting, nil)
	_, err := s.DB.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,0,1,'',0,0,?,?,?,?,1,1,0)",
		conflicting, []byte{}, []byte{}, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT), uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT), "someone-else-op", "someone-else-instance")
	require.NoError(t, err)
	before := map[uint64]*pb.MatchFormationRecord{}
	for _, battle := range []uint64{archived, conflicting} {
		f, err := s.Formation(ctx, battle)
		require.NoError(t, err)
		before[battle] = f
	}
	for range 2 {
		_, err := s.RecoverExpiredOutcomeLane(ctx, 1, 0, ExpiredOutcomeCursor{})
		if err != nil {
			require.ErrorIs(t, err, ErrBattleOutcomeOriginal, "只有身份冲突的终局行照报未确认")
		}
	}
	decision, err := s.RecoverExpiredOutcome(ctx, before[archived])
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.Zero(t, probe.count(), "已有终局行:车道页与单局入口都不问 battle")
	for battle, f := range before {
		after, err := s.Formation(ctx, battle)
		require.NoError(t, err)
		require.True(t, proto.Equal(f, after), "battle=%d:成局行不变,不排期", battle)
	}
}

// 评审补(2026-11-10)的回归:等证据的局被锁内改了排期之后仍是候选,排期往后推。修复前车道页照常把游标停在它的旧 retry_at_ms 上,
// 它的新排期到期时又排在游标之后,同一车道两次访问隔得比取证间隔久时每页都不空,游标一直追着它走;同车道里之后才到期的局
// retry_at_ms 还是开局时的旧值(早于游标),进不了任何一页,凭证据本可立即作废也要等追着的局全部在硬上限上作废。
// 现在这一页有局改了排期就回零值游标,下一页从最早到期的局重扫。不依赖墙钟:「过了取证间隔」用直接改 retry_at_ms 模拟。
func TestRecoverExpiredOutcomeLaneTiDBRestartsAfterRescheduling(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const waiting = uint64(2931)
	late := waiting + ExpiredOutcomeLanes // 同一车道
	lane := waiting % ExpiredOutcomeLanes
	require.Equal(t, lane, late%ExpiredOutcomeLanes)
	// 原 battle 连不上的那一局一直判不了;另一局的原实例没有房间。
	s.BindDeadlineAbortProbe(func(_ context.Context, f *pb.MatchFormationRecord) DeadlineAbortEvidence {
		if f.GetBattleId() == waiting {
			return DeadlineEvidenceUndetermined
		}
		return DeadlineEvidenceNoRoom
	})
	strandedFixture(t, s, waiting, nil)
	const oldRetryMs uint64 = 1000 // 晚于下面那一局的旧排期(1),修复前的游标就停在这里
	_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=? WHERE battle_id=?", oldRetryMs, waiting)
	require.NoError(t, err)

	next, err := s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, lane, ExpiredOutcomeCursor{})
	require.NoError(t, err)
	requireNoTerminal(t, s, waiting, "判不了、没到硬上限:不作废")
	rescheduled, err := s.Formation(ctx, waiting)
	require.NoError(t, err)
	require.Greater(t, rescheduled.RetryAtMs, oldRetryMs, "下次取证排进 retry_at_ms")
	require.Equal(t, ExpiredOutcomeCursor{}, next, "这一页改了排期:回零值(修复前停在 {1000, waiting})")

	// 过了取证间隔:等证据的局排期又到期,仍晚于它的旧排期;同车道里另一局刚过截止加宽限,retry_at_ms 还是开局时的旧值(1)。
	stamp, err := s.Now(ctx)
	require.NoError(t, err)
	_, err = s.DB.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=? WHERE battle_id=?", stamp-1, waiting)
	require.NoError(t, err)
	strandedFixture(t, s, late, nil)

	next, err = s.RecoverExpiredOutcomeLane(ctx, ExpiredOutcomeLanes, lane, next)
	require.NoError(t, err)
	requireDeadlineAbort(t, s, late) // 修复前游标在 {1000, waiting}:这一局(retry_at_ms=1)不进页,等证据的局又被排期,游标继续往后追
	requireNoTerminal(t, s, waiting, "等证据的局照旧排期")
	again, err := s.Formation(ctx, waiting)
	require.NoError(t, err)
	require.Greater(t, again.RetryAtMs, stamp-1, "这一页同样访问了等证据的局并改了排期")
	require.Equal(t, ExpiredOutcomeCursor{}, next)
}

// 「回包说会有终局」与「恢复循环实际写终局」逐类一致(修复前 operation 为空、缺终局的 state 5 回包说会、实际永远不写;
// 只有归档的终局行同样永远写不进)。每类成局先读 scene 查询的结论,再让到期恢复(证据齐全)把全部候选跑完两遍:
// 说会落定的局写出了 ABORT,说不会的局成局行、终局行都不变。
func TestTerminalTiDBExpectationMatchesRecoveryForEveryFormationClass(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	classes := []struct {
		why          string
		battle       uint64
		edit         func(stamp uint64, f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest)
		after        func(battle uint64)
		wantExpected bool
	}{
		{"可裁决 v1 state 2", 2901, nil, nil, true},
		{"可裁决 v1 state 3", 2902, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.State = 3 }, nil, true},
		{"可裁决 v1 state 4、采用回执 1", 2903, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) {
			f.State, f.AdoptedOutcomeObserved, f.AdoptedOutcomeProtocolVersion = 4, true, 1
		}, nil, true},
		{"终局协议 0", 2904, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.OutcomeProtocolVersion = 0 }, nil, false},
		{"battle 明确采用旧协议", 2905, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.AdoptedOutcomeObserved = true }, nil, false},
		{"坏形状:预检实例与目标不符", 2906, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.PreflightInstanceId = "other-instance" }, nil, false},
		{"原件与成局不符", 2907, clockSkewed, nil, false},
		{"operation 为空", 2908, func(_ uint64, _ *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) { r.PlacementOperationId = "" }, func(battle uint64) {
			_, err := s.DB.ExecContext(ctx, "UPDATE match_formation SET operation_id='' WHERE battle_id=?", battle)
			require.NoError(t, err)
		}, false},
		{"缺终局的 state 5", 2909, func(_ uint64, f *pb.MatchFormationRecord, _ *battlepb.CreateBattleRequest) { f.State = 5 }, nil, false},
		{"只有归档的终局行", 2910, nil, func(battle uint64) { insertArchivedOnly(t, s, battle) }, false},
		{"已有同一原件的 ABORT", 2911, nil, func(battle uint64) {
			f, err := s.Formation(ctx, battle)
			require.NoError(t, err)
			_, err = s.DB.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,0,1,'',0,0,?,?,?,?,1,1,0)",
				battle, []byte{}, []byte{}, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT), uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT), f.OperationId, f.TargetInstanceId)
			require.NoError(t, err)
		}, true},
	}
	type observed struct {
		decision  pb.BattleOutcomeDecision
		expected  bool
		formation *pb.MatchFormationRecord
	}
	before := map[uint64]observed{}
	for _, c := range classes {
		strandedFixture(t, s, c.battle, c.edit)
		if c.after != nil {
			c.after(c.battle)
		}
		decision, expected, err := s.BattleTerminalState(ctx, c.battle)
		require.NoError(t, err, c.why)
		require.Equal(t, c.wantExpected, expected, c.why)
		f, err := s.Formation(ctx, c.battle)
		require.NoError(t, err)
		before[c.battle] = observed{decision, expected, f}
	}
	// 11 类里满足车道页候选 SQL 的只有 5 局(2901、2902、2903、2907、2910),不足一页:沿游标走到末尾(第二次调用回零值),跑两遍。
	// 不覆盖跨页翻页,那由 TestRecoverExpiredOutcomeLaneTiDBPagesThroughBacklogAcrossRounds 与
	// TestRecoverExpiredOutcomeLaneTiDBRestartsAfterRescheduling 管。
	for range 2 {
		var after ExpiredOutcomeCursor
		for page := 0; page < 8; page++ {
			next, err := s.RecoverExpiredOutcomeLane(ctx, 1, 0, after)
			require.NoError(t, err)
			if next == (ExpiredOutcomeCursor{}) {
				break
			}
			after = next
		}
	}
	for _, c := range classes {
		b := before[c.battle]
		decision, expected, err := s.BattleTerminalState(ctx, c.battle)
		require.NoError(t, err)
		if b.decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN && b.expected {
			require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision, "%s:回包说会落定,恢复循环就写出了终局", c.why)
			requireDeadlineAbort(t, s, c.battle)
			continue
		}
		require.Equal(t, b.decision, decision, "%s:回包说不会落定(或已落定),恢复循环不写", c.why)
		require.Equal(t, b.expected, expected, c.why)
		f, err := s.Formation(ctx, c.battle)
		require.NoError(t, err)
		require.True(t, proto.Equal(b.formation, f), "%s:成局行不变,也不改排期", c.why)
	}
}
