package pkg

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"proto/common/base"
)

func TestReplayDeferredMessagesPreservesFIFO(t *testing.T) {
	gc := &GameClient{}
	gc.DeferMessage(&base.MessageContent{MessageId: 79})
	gc.DeferMessage(&base.MessageContent{MessageId: 80})

	var got []uint32
	gc.replayDeferredMessages(func(_ *GameClient, msg *base.MessageContent) {
		got = append(got, msg.GetMessageId())
	})

	want := []uint32{79, 80}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed message ids = %v, want %v", got, want)
	}
	if msg := gc.popDeferredMessage(); msg != nil {
		t.Fatalf("deferred queue not empty after replay: message_id=%d", msg.GetMessageId())
	}
}

func TestDeferMessageIgnoresNil(t *testing.T) {
	gc := &GameClient{}
	gc.DeferMessage(nil)
	if msg := gc.popDeferredMessage(); msg != nil {
		t.Fatalf("nil defer unexpectedly queued message_id=%d", msg.GetMessageId())
	}
}

// ---------------------------------------------------------------------------
// 请求自带回调(docs/design/client-rpc-callback.md §6)
// ---------------------------------------------------------------------------

// callHarness 把 GameClient 接到测试替身上:transmit 记下发出的请求,afterFunc 换成手动定时器。
// muduo 连接接到本机监听端口,只为让 Close / SwapConn 走真实路径;请求不经过它发送。
// 收包一律走 gc.deliver —— RecvLoop 对每条 MessageContent 调用的就是它。
type callHarness struct {
	gc   *GameClient
	port int
	sent chan *base.ClientRequest

	mu     sync.Mutex
	timers []*manualTimer
}

// manualTimer 只在测试调用 fire 时到期。fire 不看 stopped:模拟「回调已被调度、Stop 来晚了」的竞争。
type manualTimer struct {
	d       time.Duration
	f       func()
	stopped atomic.Bool
}

func (tm *manualTimer) fire() { tm.f() }

func newCallHarness(t *testing.T) *callHarness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	c := newTCPWireConn("127.0.0.1", port)
	h := &callHarness{
		gc:   &GameClient{client: c, gateIP: "127.0.0.1", gatePort: port, Account: "call-test"},
		port: port,
		sent: make(chan *base.ClientRequest, 1024),
	}
	h.gc.transmit = func(_ *wireConn, req *base.ClientRequest) { h.sent <- req }
	h.gc.afterFunc = h.newTimer
	// Cleanup 后进先出:先关客户端,再关监听。Close 幂等,用例里自己 Close 过也无妨。
	t.Cleanup(h.gc.Close)
	return h
}

func (h *callHarness) newTimer(d time.Duration, f func()) func() bool {
	tm := &manualTimer{d: d, f: f}
	h.mu.Lock()
	h.timers = append(h.timers, tm)
	h.mu.Unlock()
	return func() bool { return !tm.stopped.Swap(true) }
}

func (h *callHarness) timer(t *testing.T, i int) *manualTimer {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.timers) {
		t.Fatalf("timer #%d not created (have %d)", i, len(h.timers))
	}
	return h.timers[i]
}

// nextSent 取下一个发出的请求。CallSync 在别的 goroutine 上发送,所以要等,但有界。
func (h *callHarness) nextSent(t *testing.T) *base.ClientRequest {
	t.Helper()
	select {
	case req := <-h.sent:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("no request was sent within 5s")
		return nil
	}
}

func (h *callHarness) assertNothingSent(t *testing.T) {
	t.Helper()
	select {
	case req := <-h.sent:
		t.Fatalf("unexpected request sent: id=%d message_id=%d", req.GetId(), req.GetMessageId())
	default:
	}
}

func replyTo(req *base.ClientRequest, body string) *base.MessageContent {
	return &base.MessageContent{Id: req.GetId(), MessageId: req.GetMessageId(), SerializedMessage: []byte(body)}
}

type callOutcome struct {
	reply *base.MessageContent
	err   error
}

// outcomes 记录一个 done 收到的全部结局,用来断言「恰好一次」。
type outcomes struct {
	mu   sync.Mutex
	list []callOutcome
}

func (o *outcomes) done(reply *base.MessageContent, err error) {
	o.mu.Lock()
	o.list = append(o.list, callOutcome{reply: reply, err: err})
	o.mu.Unlock()
}

func (o *outcomes) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.list)
}

func (o *outcomes) only(t *testing.T) callOutcome {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.list) != 1 {
		t.Fatalf("done called %d times, want exactly 1: %+v", len(o.list), o.list)
	}
	return o.list[0]
}

// pushRecorder 是 onMessage 的替身:只有 id = 0 的消息应该到这里。
type pushRecorder struct {
	mu   sync.Mutex
	msgs []*base.MessageContent
}

func (p *pushRecorder) onMessage(_ *GameClient, m *base.MessageContent) {
	p.mu.Lock()
	p.msgs = append(p.msgs, m)
	p.mu.Unlock()
}

func (p *pushRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.msgs)
}

func TestSendRequestSendsUncorrelatedZeroId(t *testing.T) {
	h := newCallHarness(t)
	if err := h.gc.SendRequest(42, &base.Empty{}); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	req := h.nextSent(t)
	if req.GetId() != 0 || req.GetMessageId() != 42 {
		t.Fatalf("sent id=%d message_id=%d, want id=0 message_id=42", req.GetId(), req.GetMessageId())
	}
}

func TestCallClaimsReplyByRequestIdNotMessageId(t *testing.T) {
	h := newCallHarness(t)
	var first, second outcomes
	var pushes pushRecorder
	h.gc.Call(7, &base.Empty{}, 5*time.Second, first.done)
	h.gc.Call(7, &base.Empty{}, 5*time.Second, second.done)
	reqFirst, reqSecond := h.nextSent(t), h.nextSent(t)
	if reqFirst.GetId() == 0 || reqSecond.GetId() == 0 || reqFirst.GetId() == reqSecond.GetId() {
		t.Fatalf("request ids must be non-zero and distinct, got %d and %d", reqFirst.GetId(), reqSecond.GetId())
	}

	// 同号两个在途调用,后发的先回:按 message_id 先进先出会把它错配给第一个。
	h.gc.deliver(pushes.onMessage, replyTo(reqSecond, "second"))
	if got := second.only(t); got.err != nil || string(got.reply.GetSerializedMessage()) != "second" {
		t.Fatalf("second call outcome = %+v, want reply \"second\"", got)
	}
	if first.count() != 0 {
		t.Fatalf("first call completed by another call's reply: %+v", first.list)
	}

	h.gc.deliver(pushes.onMessage, replyTo(reqFirst, "first"))
	if got := first.only(t); got.err != nil || string(got.reply.GetSerializedMessage()) != "first" {
		t.Fatalf("first call outcome = %+v, want reply \"first\"", got)
	}
	if pushes.count() != 0 {
		t.Fatalf("replies leaked to onMessage: %d", pushes.count())
	}
}

func TestCallOutOfOrderRepliesReachTheirOwnCallbacks(t *testing.T) {
	h := newCallHarness(t)
	var pushes pushRecorder
	results := make([]outcomes, 3)
	reqs := make([]*base.ClientRequest, 3)
	for i := range results {
		h.gc.Call(uint32(100+i), &base.Empty{}, 5*time.Second, results[i].done)
		reqs[i] = h.nextSent(t)
	}

	for _, i := range []int{2, 0, 1} {
		h.gc.deliver(pushes.onMessage, replyTo(reqs[i], fmt.Sprintf("reply-%d", i)))
	}

	for i := range results {
		got := results[i].only(t)
		if got.err != nil || string(got.reply.GetSerializedMessage()) != fmt.Sprintf("reply-%d", i) {
			t.Fatalf("call #%d outcome = %+v, want reply-%d", i, got, i)
		}
	}
	if pushes.count() != 0 || h.gc.LateReplies() != 0 {
		t.Fatalf("pushes=%d late=%d, want 0/0", pushes.count(), h.gc.LateReplies())
	}
}

func TestCallEnvelopeErrorCompletesWithServerTipError(t *testing.T) {
	h := newCallHarness(t)
	var result outcomes
	h.gc.Call(9, &base.Empty{}, 5*time.Second, result.done)
	req := h.nextSent(t)

	h.gc.deliver((&pushRecorder{}).onMessage, &base.MessageContent{
		Id: req.GetId(), MessageId: req.GetMessageId(), ErrorMessage: &base.TipInfoMessage{Id: 1003},
	})

	got := result.only(t)
	var tipErr *ServerTipError
	if !errors.As(got.err, &tipErr) || tipErr.Tip != 1003 {
		t.Fatalf("err = %v, want *ServerTipError{Tip: 1003}", got.err)
	}
	if got.reply == nil || got.reply.GetErrorMessage().GetId() != 1003 {
		t.Fatalf("reply = %+v, want the envelope carrying tip 1003", got.reply)
	}
}

func TestReplyWithZeroIdGoesToOnMessageAndLeavesCallPending(t *testing.T) {
	h := newCallHarness(t)
	var result outcomes
	var pushes pushRecorder
	h.gc.Call(11, &base.Empty{}, 5*time.Second, result.done)
	h.nextSent(t)

	// 同号的推送 / 不关联回包(id = 0)不认领,交给 onMessage。
	h.gc.deliver(pushes.onMessage, &base.MessageContent{MessageId: 11, SerializedMessage: []byte("push")})

	if pushes.count() != 1 {
		t.Fatalf("onMessage got %d messages, want 1", pushes.count())
	}
	if result.count() != 0 {
		t.Fatalf("call completed by an id=0 message: %+v", result.list)
	}
}

func TestCallTimeoutThenLateReplyIsDropped(t *testing.T) {
	h := newCallHarness(t)
	var result outcomes
	var pushes pushRecorder
	h.gc.Call(12, &base.Empty{}, 3*time.Second, result.done)
	req := h.nextSent(t)
	tm := h.timer(t, 0)
	if tm.d != 3*time.Second {
		t.Fatalf("timer duration = %s, want 3s", tm.d)
	}

	tm.fire()
	if got := result.only(t); !errors.Is(got.err, ErrRPCTimeout) || got.reply != nil {
		t.Fatalf("outcome = %+v, want ErrRPCTimeout", got)
	}

	// 超时之后才到的回包:不得交给 onMessage(同号处理器会误读),计迟到后丢弃。
	h.gc.deliver(pushes.onMessage, replyTo(req, "late"))
	if pushes.count() != 0 {
		t.Fatalf("late reply leaked to onMessage")
	}
	if h.gc.LateReplies() != 1 {
		t.Fatalf("late replies = %d, want 1", h.gc.LateReplies())
	}
	if result.count() != 1 {
		t.Fatalf("done called %d times after late reply, want 1", result.count())
	}
}

func TestReplyStopsTimerAndRacingExpiryIsIgnored(t *testing.T) {
	h := newCallHarness(t)
	var result outcomes
	h.gc.Call(13, &base.Empty{}, time.Second, result.done)
	req := h.nextSent(t)
	tm := h.timer(t, 0)

	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(req, "ok"))
	if !tm.stopped.Load() {
		t.Fatal("timer not stopped after reply")
	}
	// 定时器回调与回包竞争、Stop 来晚了:回调照样跑,但摘不到表,done 不会再被调用。
	tm.fire()
	if got := result.only(t); got.err != nil {
		t.Fatalf("outcome = %+v, want the reply", got)
	}
}

func TestCloseFailsAllOutstandingCallsAndRejectsNewOnes(t *testing.T) {
	h := newCallHarness(t)
	var order []uint64
	var orderMu sync.Mutex
	reqs := make([]*base.ClientRequest, 3)
	results := make([]outcomes, 3)
	for i := range results {
		h.gc.Call(uint32(20+i), &base.Empty{}, 5*time.Second, func(reply *base.MessageContent, err error) {
			orderMu.Lock()
			order = append(order, reqs[i].GetId())
			orderMu.Unlock()
			results[i].done(reply, err)
		})
		reqs[i] = h.nextSent(t)
	}

	h.gc.Close()

	for i := range results {
		if got := results[i].only(t); !errors.Is(got.err, ErrRPCDisconnected) {
			t.Fatalf("call #%d outcome = %+v, want ErrRPCDisconnected", i, got)
		}
	}
	want := []uint64{reqs[0].GetId(), reqs[1].GetId(), reqs[2].GetId()}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("failure order = %v, want request order %v", order, want)
	}

	// Close 是终态:之后的 Call 在返回前同步以 NotSent 完成,不发包。
	var after outcomes
	h.gc.Call(30, &base.Empty{}, 5*time.Second, after.done)
	if got := after.only(t); !errors.Is(got.err, ErrRPCNotSent) {
		t.Fatalf("call after Close outcome = %+v, want ErrRPCNotSent", got)
	}
	h.assertNothingSent(t)

	// 关闭前发出的请求,回包此后才到:迟到,丢弃。
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(reqs[0], "late"))
	if h.gc.LateReplies() != 1 || results[0].count() != 1 {
		t.Fatalf("late=%d done=%d after Close, want 1/1", h.gc.LateReplies(), results[0].count())
	}
}

func TestSwapConnFailsOnlyCallsOnOldConnection(t *testing.T) {
	h := newCallHarness(t)
	var before, after outcomes
	h.gc.Call(40, &base.Empty{}, 5*time.Second, before.done)
	h.nextSent(t)

	if err := h.gc.SwapConn("127.0.0.1", h.port); err != nil {
		t.Fatalf("SwapConn: %v", err)
	}
	if got := before.only(t); !errors.Is(got.err, ErrRPCDisconnected) {
		t.Fatalf("call on old connection outcome = %+v, want ErrRPCDisconnected", got)
	}

	h.gc.Call(41, &base.Empty{}, 5*time.Second, after.done)
	reqAfter := h.nextSent(t)
	if after.count() != 0 {
		t.Fatalf("call on new connection completed early: %+v", after.list)
	}
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(reqAfter, "new"))
	if got := after.only(t); got.err != nil || string(got.reply.GetSerializedMessage()) != "new" {
		t.Fatalf("call on new connection outcome = %+v, want reply \"new\"", got)
	}
}

func TestCallWithoutConnectionOrBudgetIsNotSent(t *testing.T) {
	var noConn outcomes
	(&GameClient{}).Call(50, &base.Empty{}, time.Second, noConn.done)
	if got := noConn.only(t); !errors.Is(got.err, ErrRPCNotSent) {
		t.Fatalf("no connection outcome = %+v, want ErrRPCNotSent", got)
	}

	h := newCallHarness(t)
	var noBudget outcomes
	h.gc.Call(51, &base.Empty{}, 0, noBudget.done)
	if got := noBudget.only(t); !errors.Is(got.err, ErrRPCNotSent) {
		t.Fatalf("zero timeout outcome = %+v, want ErrRPCNotSent", got)
	}
	h.assertNothingSent(t)
}

func TestCallSyncReturnsItsOwnReply(t *testing.T) {
	h := newCallHarness(t)
	type syncResult struct {
		reply *base.MessageContent
		err   error
	}
	resultCh := make(chan syncResult, 1)
	go func() {
		reply, err := h.gc.CallSync(context.Background(), 60, &base.Empty{}, 5*time.Second)
		resultCh <- syncResult{reply, err}
	}()
	req := h.nextSent(t)
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(req, "sync"))

	select {
	case r := <-resultCh:
		if r.err != nil || string(r.reply.GetSerializedMessage()) != "sync" {
			t.Fatalf("CallSync = (%+v, %v), want reply \"sync\"", r.reply, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallSync did not return within 5s after its reply was delivered")
	}
}

func TestCallSyncContextCancelCompletesWithCauseAndDropsLateReply(t *testing.T) {
	h := newCallHarness(t)
	errStop := errors.New("gate said service unavailable")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	errCh := make(chan error, 1)
	go func() {
		_, err := h.gc.CallSync(ctx, 61, &base.Empty{}, 5*time.Second)
		errCh <- err
	}()
	req := h.nextSent(t)
	cancel(errStop)

	select {
	case err := <-errCh:
		if !errors.Is(err, errStop) {
			t.Fatalf("CallSync err = %v, want the cancel cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallSync did not return within 5s after ctx was cancelled")
	}
	if !h.timer(t, 0).stopped.Load() {
		t.Fatal("timer not stopped after ctx cancellation")
	}
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(req, "late"))
	if h.gc.LateReplies() != 1 {
		t.Fatalf("late replies = %d, want 1", h.gc.LateReplies())
	}
}

func TestCallSyncDoesNotSendWhenContextAlreadyDone(t *testing.T) {
	h := newCallHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.gc.CallSync(ctx, 62, &base.Empty{}, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("CallSync err = %v, want context.Canceled", err)
	}
	h.assertNothingSent(t)
}

// 回包、重复回包、真实定时器到期、ctx 取消与 Close 全部并发:每个 done 恰好一次。配合 -race 跑。
func TestCallDoneExactlyOnceUnderConcurrentOutcomes(t *testing.T) {
	h := newCallHarness(t)
	h.gc.afterFunc = nil // 用真实定时器制造与回包的竞争;断言不依赖谁先到

	const callers, perCaller = 8, 50
	var (
		countsMu sync.Mutex
		counts   = make(map[int]int)
		pending  sync.WaitGroup
	)
	record := func(key int) {
		countsMu.Lock()
		counts[key]++
		countsMu.Unlock()
		pending.Done() // 重复调用会让计数变负并 panic
	}

	// 单一的「收包 goroutine」,与生产里 RecvLoop 的位置相同:每个回包投两次,第二次必是迟到。
	stopDeliver := make(chan struct{})
	deliverDone := make(chan struct{})
	go func() {
		defer close(deliverDone)
		onMessage := (&pushRecorder{}).onMessage
		for {
			select {
			case req := <-h.sent:
				h.gc.deliver(onMessage, replyTo(req, "r"))
				h.gc.deliver(onMessage, replyTo(req, "dup"))
			case <-stopDeliver:
				return
			}
		}
	}()

	var issuers sync.WaitGroup
	for c := 0; c < callers; c++ {
		issuers.Add(1)
		go func(c int) {
			defer issuers.Done()
			rng := rand.New(rand.NewSource(int64(c)))
			for i := 0; i < perCaller; i++ {
				key := c*perCaller + i
				pending.Add(1)
				timeout := time.Duration(1+rng.Intn(3)) * time.Millisecond
				if i%5 == 0 {
					ctx, cancel := context.WithCancel(context.Background())
					go cancel()
					_, _ = h.gc.CallSync(ctx, uint32(key), &base.Empty{}, timeout)
					record(key)
					continue
				}
				h.gc.Call(uint32(key), &base.Empty{}, timeout, func(*base.MessageContent, error) { record(key) })
			}
		}(c)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(2 * time.Millisecond)
		h.gc.Close()
	}()

	issuers.Wait()
	<-closed
	allDone := make(chan struct{})
	go func() { pending.Wait(); close(allDone) }()
	select {
	case <-allDone:
	case <-time.After(10 * time.Second):
		t.Fatal("not every call completed within 10s")
	}
	close(stopDeliver)
	<-deliverDone

	countsMu.Lock()
	defer countsMu.Unlock()
	if len(counts) != callers*perCaller {
		t.Fatalf("%d calls completed, want %d", len(counts), callers*perCaller)
	}
	for key, n := range counts {
		if n != 1 {
			t.Fatalf("call %d completed %d times, want 1", key, n)
		}
	}
}

// CallSyncThen:回包先在派发它的 goroutine 上交给 onReply,再唤醒调用方。deliver 是同步的,
// 它返回时 onReply 必须已经跑完 —— 这就证明 onReply 跑在收包 goroutine 上、早于 CallSyncThen 返回。
func TestCallSyncThenRunsOnReplyOnDeliveringGoroutineBeforeReturning(t *testing.T) {
	h := newCallHarness(t)
	var got outcomes
	type syncResult struct {
		reply *base.MessageContent
		err   error
	}
	resultCh := make(chan syncResult, 1)
	go func() {
		reply, err := h.gc.CallSyncThen(context.Background(), 63, &base.Empty{}, 5*time.Second, got.done)
		resultCh <- syncResult{reply, err}
	}()
	req := h.nextSent(t)
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(req, "then"))

	if o := got.only(t); o.err != nil || string(o.reply.GetSerializedMessage()) != "then" {
		t.Fatalf("onReply got (%+v, %v), want reply \"then\"", o.reply, o.err)
	}
	select {
	case r := <-resultCh:
		if r.err != nil || string(r.reply.GetSerializedMessage()) != "then" {
			t.Fatalf("CallSyncThen = (%+v, %v), want reply \"then\"", r.reply, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallSyncThen did not return within 5s after its reply was delivered")
	}
	if n := got.count(); n != 1 {
		t.Fatalf("onReply called %d times, want 1", n)
	}
}

// 信封错误也是回包:onReply 收到那条信封与 *ServerTipError,调用方自己决定要不要执行副作用。
func TestCallSyncThenPassesEnvelopeErrorToOnReply(t *testing.T) {
	h := newCallHarness(t)
	var got outcomes
	errCh := make(chan error, 1)
	go func() {
		_, err := h.gc.CallSyncThen(context.Background(), 64, &base.Empty{}, 5*time.Second, got.done)
		errCh <- err
	}()
	req := h.nextSent(t)
	h.gc.deliver((&pushRecorder{}).onMessage, &base.MessageContent{
		Id: req.GetId(), MessageId: req.GetMessageId(), ErrorMessage: &base.TipInfoMessage{Id: 1003}})

	o := got.only(t)
	var tipErr *ServerTipError
	if !errors.As(o.err, &tipErr) || tipErr.Tip != 1003 || o.reply.GetErrorMessage().GetId() != 1003 {
		t.Fatalf("onReply got (%+v, %v), want the envelope with ServerTipError{1003}", o.reply, o.err)
	}
	select {
	case err := <-errCh:
		if !errors.As(err, &tipErr) || tipErr.Tip != 1003 {
			t.Fatalf("CallSyncThen err = %v, want ServerTipError{1003}", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallSyncThen did not return within 5s after its envelope was delivered")
	}
}

// 没有回包的结局(超时)不调 onReply;之后到达的回包按迟到丢弃,同样不调。
func TestCallSyncThenSkipsOnReplyWhenNoReplyArrives(t *testing.T) {
	h := newCallHarness(t)
	var got outcomes
	errCh := make(chan error, 1)
	go func() {
		_, err := h.gc.CallSyncThen(context.Background(), 65, &base.Empty{}, 5*time.Second, got.done)
		errCh <- err
	}()
	req := h.nextSent(t)
	h.timer(t, 0).fire()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRPCTimeout) {
			t.Fatalf("CallSyncThen err = %v, want ErrRPCTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallSyncThen did not return within 5s after its timer fired")
	}
	h.gc.deliver((&pushRecorder{}).onMessage, replyTo(req, "late"))
	if n := got.count(); n != 0 {
		t.Fatalf("onReply called %d times, want 0", n)
	}
	if h.gc.LateReplies() != 1 {
		t.Fatalf("late replies = %d, want 1", h.gc.LateReplies())
	}
}
