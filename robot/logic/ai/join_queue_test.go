package ai

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"robot/logic/gameobject"
)

// fakeMatchQueue 记下每次调用,按脚本返回结果。
type fakeMatchQueue struct {
	joinResults   []fakeJoinResult
	cancelResults []error
	joins         int
	cancels       []QueueTicket
}

type fakeJoinResult struct {
	ticket QueueTicket
	err    error
}

func (q *fakeMatchQueue) Join() (QueueTicket, error) {
	if q.joins >= len(q.joinResults) {
		panic(fmt.Sprintf("unexpected Join #%d", q.joins+1))
	}
	r := q.joinResults[q.joins]
	q.joins++
	return r.ticket, r.err
}

func (q *fakeMatchQueue) Cancel(t QueueTicket) error {
	if len(q.cancels) >= len(q.cancelResults) {
		panic(fmt.Sprintf("unexpected Cancel #%d of %+v", len(q.cancels)+1, t))
	}
	err := q.cancelResults[len(q.cancels)]
	q.cancels = append(q.cancels, t)
	return err
}

// newQueueTestAI 返回接了假入口、走手动时钟的 RobotAI;stats 为 nil,recordBehavior 直接返回,不碰 client。
func newQueueTestAI(q MatchQueue, dwell time.Duration) (*RobotAI, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	ai := &RobotAI{now: func() time.Time { return now }}
	ai.SetMatchQueue(q, dwell)
	return ai, &now
}

// 入队 → 排队期内不打扰 → 排满 dwell 撤单(带原凭据)→ 撤掉之后下一拍重新入队。
func TestJoinQueueJoinsWaitsThenCancelsAfterDwell(t *testing.T) {
	ticket := QueueTicket{RequestID: "req-1", Ticket: "t-1"}
	q := &fakeMatchQueue{
		joinResults:   []fakeJoinResult{{ticket: ticket}, {ticket: QueueTicket{RequestID: "req-2"}}},
		cancelResults: []error{nil},
	}
	ai, now := newQueueTestAI(q, 30*time.Second)

	ai.joinQueue()
	if q.joins != 1 || ai.queued != ticket {
		t.Fatalf("after first tick: joins=%d queued=%+v, want 1 join holding %+v", q.joins, ai.queued, ticket)
	}
	*now = now.Add(29 * time.Second)
	ai.joinQueue()
	if q.joins != 1 || len(q.cancels) != 0 {
		t.Fatalf("inside dwell: joins=%d cancels=%d, want no new calls", q.joins, len(q.cancels))
	}
	*now = now.Add(time.Second)
	ai.joinQueue()
	if len(q.cancels) != 1 || q.cancels[0] != ticket || ai.queued != (QueueTicket{}) {
		t.Fatalf("at dwell: cancels=%+v queued=%+v, want one cancel of %+v and an empty queue", q.cancels, ai.queued, ticket)
	}
	ai.joinQueue()
	if q.joins != 2 {
		t.Fatalf("after cancel: joins=%d, want a fresh join", q.joins)
	}
}

// 撤单失败(含结局未知)保留凭据,下一拍接着撤,不重复入队。
func TestJoinQueueRetriesFailedCancelWithTheSameTicket(t *testing.T) {
	ticket := QueueTicket{RequestID: "req-1", Ticket: "t-1"}
	q := &fakeMatchQueue{
		joinResults:   []fakeJoinResult{{ticket: ticket}},
		cancelResults: []error{errors.New("timeout"), nil},
	}
	ai, now := newQueueTestAI(q, time.Second)
	ai.joinQueue()
	*now = now.Add(time.Second)
	ai.joinQueue()
	if ai.queued != ticket {
		t.Fatalf("after failed cancel: queued=%+v, want the ticket kept", ai.queued)
	}
	ai.joinQueue()
	if len(q.cancels) != 2 || q.cancels[1] != ticket || q.joins != 1 {
		t.Fatalf("cancels=%+v joins=%d, want the same ticket cancelled twice and no second join", q.cancels, q.joins)
	}
}

// 结局未知的入队按「在队列里」处理,到时按请求号撤;带回票号的拒绝(已在队列中)同样要撤;
// 不带票号的业务拒绝什么都不留,下一拍重新入队。
func TestJoinQueueTracksUnknownOutcomesAndTicketedRejections(t *testing.T) {
	unknown := QueueTicket{RequestID: "req-unknown"}
	ticketed := QueueTicket{RequestID: "req-dup", Ticket: "t-existing"}
	q := &fakeMatchQueue{
		joinResults: []fakeJoinResult{
			{ticket: unknown, err: fmt.Errorf("%w: JoinQueue: timeout", ErrMatchOutcomeUnknown)},
			{ticket: ticketed, err: errors.New("JoinQueue rejected error_code=1 tip=2")},
			{err: errors.New("JoinQueue rejected error_code=3 tip=4")},
			{ticket: QueueTicket{RequestID: "req-ok"}},
		},
		cancelResults: []error{nil, nil},
	}
	ai, now := newQueueTestAI(q, time.Second)

	ai.joinQueue()
	if ai.queued != unknown {
		t.Fatalf("unknown outcome: queued=%+v, want %+v", ai.queued, unknown)
	}
	*now = now.Add(time.Second)
	ai.joinQueue()
	ai.joinQueue()
	if ai.queued != ticketed {
		t.Fatalf("ticketed rejection: queued=%+v, want %+v", ai.queued, ticketed)
	}
	*now = now.Add(time.Second)
	ai.joinQueue()
	ai.joinQueue()
	if ai.queued != (QueueTicket{}) {
		t.Fatalf("plain rejection: queued=%+v, want nothing held", ai.queued)
	}
	ai.joinQueue()
	if q.joins != 4 || len(q.cancels) != 2 || q.cancels[0] != unknown || q.cancels[1] != ticketed {
		t.Fatalf("joins=%d cancels=%+v, want 4 joins and cancels of %+v then %+v", q.joins, q.cancels, unknown, ticketed)
	}
}

// 成局之后(收到 NotifyBattleStart、还没收到这局的 NotifyBattleEnd)不入队也不撤单,本地意图放下;
// 这局结束后恢复入队。
func TestJoinQueueStaysOutWhileInBattle(t *testing.T) {
	q := &fakeMatchQueue{joinResults: []fakeJoinResult{{ticket: QueueTicket{RequestID: "req-1"}}, {ticket: QueueTicket{RequestID: "req-2"}}}}
	ai, _ := newQueueTestAI(q, time.Hour)
	player := gameobject.NewPlayer(1)
	ai.SetPlayer(player)

	ai.joinQueue()
	player.SignalBattleStart(77)
	ai.joinQueue()
	if q.joins != 1 || ai.queued != (QueueTicket{}) {
		t.Fatalf("in battle: joins=%d queued=%+v, want no new join and the intent dropped", q.joins, ai.queued)
	}
	player.SignalBattleEnd(77, 1)
	ai.joinQueue()
	if q.joins != 2 {
		t.Fatalf("after the battle ended: joins=%d, want a new join", q.joins)
	}
}

// 没注入入口时只记失败,不 panic。
func TestJoinQueueWithoutMatchQueueIsANoOp(t *testing.T) {
	ai := &RobotAI{}
	ai.joinQueue()
	if ai.queued != (QueueTicket{}) {
		t.Fatalf("queued=%+v, want nothing", ai.queued)
	}
}

// match-stress 档带 join_queue 权重;stress 档不带(历史压测按它对比,不能被这次改动带偏)。
func TestBuiltinProfilesJoinQueueWeights(t *testing.T) {
	stress, matchStress := BuiltinProfiles["stress"], BuiltinProfiles["match-stress"]
	if w := stress.Weight(ActionJoinQueue); w != 0 {
		t.Fatalf("stress profile join_queue weight = %d, want 0", w)
	}
	if w := matchStress.Weight(ActionJoinQueue); w <= 0 {
		t.Fatalf("match-stress profile join_queue weight = %d, want > 0", w)
	}
	if a, ok := ParseAction("join_queue"); !ok || a != ActionJoinQueue {
		t.Fatalf("ParseAction(join_queue) = (%v, %v), want (ActionJoinQueue, true)", a, ok)
	}
	var none *Profile
	if none.Weight(ActionJoinQueue) != 0 {
		t.Fatalf("nil profile weight must be 0")
	}
}
