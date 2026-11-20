package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// queueCall 是脚本里的一步:期望状态机调哪个端点、带哪个 queue_token,以及假网关怎么应答。
type queueCall struct {
	endpoint string // "assign-gate" 或 "queue-status"
	token    string // 期望请求携带的 queue_token,空串表示不带
	rsp      *httpAssignGateResponse
	err      error
}

// scriptedGateway 按脚本逐步应答。调用的端点或 token 与脚本不符,或脚本用完后还被调用,立即判失败;
// 状态机在测试 goroutine 里同步运行,所以可以直接 Fatalf。
type scriptedGateway struct {
	t     *testing.T
	steps []queueCall
}

func (g *scriptedGateway) AssignGate(req *httpAssignGateRequest) (*httpAssignGateResponse, error) {
	return g.next("assign-gate", req.QueueToken)
}

func (g *scriptedGateway) QueueStatus(queueToken string, zoneID uint32) (*httpAssignGateResponse, error) {
	return g.next("queue-status", queueToken)
}

func (g *scriptedGateway) next(endpoint, token string) (*httpAssignGateResponse, error) {
	g.t.Helper()
	if len(g.steps) == 0 {
		g.t.Fatalf("unexpected %s(token=%q): script exhausted", endpoint, token)
	}
	step := g.steps[0]
	g.steps = g.steps[1:]
	if step.endpoint != endpoint || step.token != token {
		g.t.Fatalf("call = %s(token=%q), want %s(token=%q)", endpoint, token, step.endpoint, step.token)
	}
	return step.rsp, step.err
}

// recordSleeps 返回一个不真等待的 sleep,只记下每次的等待时长;ctx 已结束时与 sleepCtx 一样返回 ctx.Err()。
func recordSleeps(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
}

// zeroJitter 是不加抖动的抖动源,让用例的等待时长恰等于基础间隔。
func zeroJitter(time.Duration) time.Duration { return 0 }

// recordFullJitter 返回一个抖动取满(返回上界)的抖动源,并记下每次收到的上界。
func recordFullJitter(limits *[]time.Duration) func(time.Duration) time.Duration {
	return func(limit time.Duration) time.Duration {
		*limits = append(*limits, limit)
		return limit
	}
}

// statusFailuresRateLimited 生成 n 轮「持 token 查 queue-status 回 500 → 恢复重入回 ratelimit(1ms)」的脚本步。
func statusFailuresRateLimited(token string, n int) []queueCall {
	var steps []queueCall
	for range n {
		steps = append(steps,
			queueCall{"queue-status", token, codeResponse(500, "login_unavailable"), nil},
			queueCall{"assign-gate", token, rateLimitQueued(1), nil},
		)
	}
	return steps
}

func loginQueued(token string, rank, retryAfterMs int64) *httpAssignGateResponse {
	return &httpAssignGateResponse{Code: 100, QueueSource: "login", QueueToken: token, QueueRank: rank, RetryAfterMs: retryAfterMs}
}

func rateLimitQueued(retryAfterMs int64) *httpAssignGateResponse {
	return &httpAssignGateResponse{Code: 100, QueueSource: "ratelimit", RetryAfterMs: retryAfterMs}
}

func admittedResponse() *httpAssignGateResponse {
	return &httpAssignGateResponse{Code: 0, GateIP: "10.0.0.7", GatePort: 9000}
}

func codeResponse(code int, errTag string) *httpAssignGateResponse {
	return &httpAssignGateResponse{Code: code, Error: errTag}
}

// TestAssignGateWithQueue 钉住 2026-10-15 的客户端契约(设计稿 open-server-rate-limit-design.md §11.5 更正及其补注):
// 持有 queue_token 时轮询 /api/queue-status,等待 = max(retry_after_ms, 下限) 加抖动,下限平时 2s、queue-status
// 连续失败时 2s 起翻倍封顶 30s;queue-status 本身失败(网络错误 / 非 200 / code==500)才带 token 重入 assign-gate;
// 重入被限流回到轮询;503 zone_* / 404 zone_not_found 直接报错;410 丢 token 重来。
// 本表统一注入 zeroJitter,等待恰为基础间隔;抖动上界见 TestAssignGateWithQueue_Jitter。
// 用例只看调用序列与等待时长,不依赖墙钟。
func TestAssignGateWithQueue(t *testing.T) {
	errNet := errors.New("dial tcp: connection refused")
	cases := []struct {
		name      string
		steps     []queueCall
		wantWaits []time.Duration
		wantErr   string // 空串表示期望放行
	}{
		{
			// 修复前红:queue-status 回 5xx 落进 default 直接返回错误,token 随之丢弃,外层重试从头排队。
			// 2026-10-15 下限修复前红在首次等待:取 retry_after 1s,没有 2s 下限。
			name: "queue-status 500, re-entry held by zone bucket, back to queue-status",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 3, 1000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", rateLimitQueued(3000), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 3 * time.Second},
		},
		{
			// 修复前红:queue-status 网络错误直接返回;且 code=429 一律当失败返回,不回到轮询。
			name: "queue-status network error, re-entry hits IP bucket 429, back to queue-status",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 0, 0), nil},
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", codeResponse(429, "IP_RATE_LIMIT"), nil},
				{"queue-status", "T1", loginQueued("T1", 0, 2000), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second},
		},
		{
			// 修复前红:同第一例,queue-status 的 5xx 直接返回,恢复用的重入根本不会发出。
			// 2026-10-15 起改用 500:网关对 login 故障回 500,从不回 503「login_unavailable」;503 只表示 zone 不可进(见下面两例)。
			name: "queue-status 500, re-entry resumes the login queue entry",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 9, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", loginQueued("T1", 4, 2000), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second},
		},
		{
			// 修复前红在第二次等待:取重入回的 retry_after 1ms,queue-status 持续 500 时形成毫秒级循环。
			name: "queue-status 500, re-entry ratelimit 1ms still waits the 2s floor",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 1, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", rateLimitQueued(1), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second},
		},
		{
			// 修复前红:每次等待都是重入回的 1ms,连续失败不退避。
			// 连续 6 次 queue-status 500,等待 2s、4s、8s、16s 后封顶 30s;queue-status 回一次排队即复位到 max(1s, 2s)。
			name: "consecutive queue-status 500 back off to the 30s cap, a good poll resets",
			steps: slices.Concat(
				[]queueCall{{"assign-gate", "", loginQueued("T1", 1, 2000), nil}},
				statusFailuresRateLimited("T1", 6),
				[]queueCall{
					{"queue-status", "T1", loginQueued("T1", 1, 1000), nil},
					{"queue-status", "T1", admittedResponse(), nil},
				},
			),
			wantWaits: []time.Duration{
				2 * time.Second, // 首次排队,未失败
				2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
				2 * time.Second, // queue-status 成功一次,退避复位
			},
		},
		{
			// 修复前红:rsp.Code < 500 把 503 当成 queue-status 本身失败,发出恢复重入,脚本用完即失败。
			name: "queue-status 503 zone_maintenance fails without re-entry",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 2, 2000), nil},
				{"queue-status", "T1", codeResponse(503, "zone_maintenance"), nil},
			},
			wantWaits: []time.Duration{2 * time.Second},
			wantErr:   `code=503 err="zone_maintenance"`,
		},
		{
			// 修复前即绿(404 < 500,本来就交回状态机);钉住失败分类只认 code==500,不被放宽成「>=400 都重入」。
			name: "queue-status 404 zone_not_found fails without re-entry",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 2, 2000), nil},
				{"queue-status", "T1", codeResponse(404, "zone_not_found"), nil},
			},
			wantWaits: []time.Duration{2 * time.Second},
			wantErr:   `code=404 err="zone_not_found"`,
		},
		{
			// 修复前红在第三次等待:持 token 的 429 固定等 2s,第二次连续失败没有退避到 4s。
			name: "held token, re-entry 429 twice, waits follow the same floor and backoff",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 0, 2000), nil},
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", codeResponse(429, "IP_RATE_LIMIT"), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", codeResponse(429, "IP_RATE_LIMIT"), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			// 修复前红在第三、四次等待:取重入回的 retry_after 2s,没有退避。
			// 恢复重入回 login 排队(续上原条目)不算 queue-status 成功,连续失败次数不清零。
			name: "re-entry resuming the login entry does not reset the backoff",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 5, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", loginQueued("T1", 4, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", loginQueued("T1", 3, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", loginQueued("T1", 2, 2000), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second},
		},
		{
			// 修复前即绿;钉住 410 丢 token 后不忙等:不带 token 的 assign-gate 再回 410 直接报错,不再重调。
			name: "410 then assign-gate 410 again fails instead of spinning",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 5, 2000), nil},
				{"queue-status", "T1", codeResponse(410, "queue_token_expired"), nil},
				{"assign-gate", "", codeResponse(410, "missing_queue_token"), nil},
			},
			wantWaits: []time.Duration{2 * time.Second},
			wantErr:   "code=410",
		},
		{
			// 修复前即绿;钉住 410 丢 token 后被 zone 桶挡住时走不持 token 的口径:按 retry_after 等待再重调 assign-gate。
			name: "410 then ratelimit without a token waits before re-calling assign-gate",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 5, 2000), nil},
				{"queue-status", "T1", codeResponse(410, "queue_token_expired"), nil},
				{"assign-gate", "", rateLimitQueued(500), nil},
				{"assign-gate", "", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 500 * time.Millisecond},
		},
		{
			name: "410 drops the token and restarts from assign-gate",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 5, 2000), nil},
				{"queue-status", "T1", codeResponse(410, "queue_token_expired"), nil},
				{"assign-gate", "", loginQueued("T2", 1, 2000), nil},
				{"queue-status", "T2", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second},
		},
		{
			name: "without a token, ratelimit queueing re-calls assign-gate",
			steps: []queueCall{
				{"assign-gate", "", rateLimitQueued(500), nil},
				{"assign-gate", "", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{500 * time.Millisecond},
		},
		{
			name: "without a token, 429 is left to the outer retry loop",
			steps: []queueCall{
				{"assign-gate", "", codeResponse(429, "IP_RATE_LIMIT"), nil},
			},
			wantErr: "rate-limited: IP_RATE_LIMIT",
		},
		{
			// 修复前红:没有 token 也照样「丢 token」重调 assign-gate,且中间不等待,脚本用完即失败。
			name: "410 without a token fails instead of spinning",
			steps: []queueCall{
				{"assign-gate", "", codeResponse(410, "missing_queue_token"), nil},
			},
			wantErr: "code=410",
		},
		{
			// 2026-11-20 前红:重入网络失败直接返回 re-entry failed,token 随之丢弃(§11.5 第 6 条改定前的口径)。
			// 现在保票:重入失败算进同一串连续失败,等待按 2s、4s 退避后回到 queue-status,成功一次复位。
			name: "queue-status and the re-entry both fail, the token is kept and polling backs off",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 0, 2000), nil},
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", nil, errNet},
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", nil, errNet},
				{"queue-status", "T1", loginQueued("T1", 0, 2000), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second, 2 * time.Second},
		},
		{
			// 2026-11-20 前红:重入回 500(重入到了 login 而 login 仍不可用)落进 default 直接返回,token 随之丢弃。
			name: "re-entry 500 keeps the token and backs off",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 1, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", codeResponse(500, "login_unavailable"), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", codeResponse(500, "login_unavailable"), nil},
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 4 * time.Second},
		},
		{
			// 2026-11-20 前红在第三次等待:410 换票时不清连续失败数,新 token 的首次等待沿用旧的 4s 退避。
			name: "410 after failed re-entries resets the backoff for the new token",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 1, 2000), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", codeResponse(500, "login_unavailable"), nil},
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", codeResponse(410, "queue_token_expired"), nil},
				{"assign-gate", "", loginQueued("T2", 0, 2000), nil},
				{"queue-status", "T2", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second},
		},
		{
			// 钉住 500 只在持 token 时保票:首调就回 500 仍交外层,不在这里重试。
			name: "without a token, 500 is left to the outer retry loop",
			steps: []queueCall{
				{"assign-gate", "", codeResponse(500, "login_unavailable"), nil},
			},
			wantErr: "code=500",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &scriptedGateway{t: t, steps: tc.steps}
			var waits []time.Duration
			rsp, err := AssignGateWithQueue(context.Background(), gw, &httpAssignGateRequest{ZoneID: 1}, nil, recordSleeps(&waits), zeroJitter)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want admitted", err)
				}
				if rsp.Code != 0 {
					t.Fatalf("code = %d, want 0", rsp.Code)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if len(gw.steps) != 0 {
				t.Fatalf("%d scripted calls never made, next %s(token=%q)", len(gw.steps), gw.steps[0].endpoint, gw.steps[0].token)
			}
			if !slices.Equal(waits, tc.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tc.wantWaits)
			}
		})
	}
}

// TestAssignGateWithQueue_StopsWhenBudgetExhausted:排队预算(ctx)用完时停在等待处返回,不再发请求。
func TestAssignGateWithQueue_StopsWhenBudgetExhausted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gw := &scriptedGateway{t: t, steps: []queueCall{
		{"assign-gate", "", loginQueued("T1", 7, 2000), nil},
	}}
	var waits []time.Duration
	_, err := AssignGateWithQueue(ctx, gw, &httpAssignGateRequest{ZoneID: 1}, nil, recordSleeps(&waits), zeroJitter)
	if err == nil || !strings.Contains(err.Error(), "queue budget exceeded") {
		t.Fatalf("err = %v, want queue budget exceeded", err)
	}
	if len(gw.steps) != 0 {
		t.Fatalf("%d scripted calls never made", len(gw.steps))
	}
}

// budgetExpiresDuring 包住脚本网关:第 at 次调用(以 1 起算,两个端点合计)应答前结束排队预算 ctx,
// 模拟预算恰在这次请求途中到期(生产实现每次调用各带 perCallTimeout,不随预算取消,所以请求能跨过截止时间)。
type budgetExpiresDuring struct {
	inner  *scriptedGateway
	at     int
	calls  int
	cancel context.CancelFunc
}

func (g *budgetExpiresDuring) tick() {
	g.calls++
	if g.calls == g.at {
		g.cancel()
	}
}

func (g *budgetExpiresDuring) AssignGate(req *httpAssignGateRequest) (*httpAssignGateResponse, error) {
	g.tick()
	return g.inner.AssignGate(req)
}

func (g *budgetExpiresDuring) QueueStatus(queueToken string, zoneID uint32) (*httpAssignGateResponse, error) {
	g.tick()
	return g.inner.QueueStatus(queueToken, zoneID)
}

// TestAssignGateWithQueue_NoRequestAfterBudgetExpiresInFlight 钉住「排队预算耗尽即停,不再发请求」对不经等待的两次请求
// 同样成立:预算在 queue-status 或恢复重入途中到期时,恢复重入与 410 后不带 token 的重调都不再发出,
// 直接返回 queue budget exceeded。脚本里不留后续步,多发任何请求都会让 scriptedGateway 以 script exhausted 判失败。
// 用例只看调用序列与等待时长,不依赖墙钟。
func TestAssignGateWithQueue_NoRequestAfterBudgetExpiresInFlight(t *testing.T) {
	errNet := errors.New("dial tcp: connection refused")
	queued := queueCall{"assign-gate", "", loginQueued("T1", 7, 2000), nil}
	cases := []struct {
		name     string
		steps    []queueCall
		expireAt int    // 预算在第几次调用途中到期,以 1 起算
		wantErr  string // 空串表示期望放行
	}{
		{
			// 修复前红:queue-status 失败后无条件发恢复重入 assign-gate(token=T1),红在 script exhausted。
			name:     "queue-status network error: no re-entry",
			steps:    []queueCall{queued, {"queue-status", "T1", nil, errNet}},
			expireAt: 2,
			wantErr:  "queue budget exceeded",
		},
		{
			// 修复前红:同上,回包 code=500 也无条件发恢复重入。
			name:     "queue-status 500: no re-entry",
			steps:    []queueCall{queued, {"queue-status", "T1", codeResponse(500, "login_unavailable"), nil}},
			expireAt: 2,
			wantErr:  "queue budget exceeded",
		},
		{
			// 修复前红:case 410 无条件发不带 token 的 assign-gate,红在 script exhausted。
			name:     "queue-status 410: no tokenless assign-gate",
			steps:    []queueCall{queued, {"queue-status", "T1", codeResponse(410, "queue_token_expired"), nil}},
			expireAt: 2,
			wantErr:  "queue budget exceeded",
		},
		{
			// 修复前红:预算在恢复重入途中到期,重入回 410 后仍发不带 token 的 assign-gate,红在 script exhausted。
			name: "re-entry 410: no tokenless assign-gate",
			steps: []queueCall{
				queued,
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", codeResponse(410, "queue_token_expired"), nil},
			},
			expireAt: 3,
			wantErr:  "queue budget exceeded",
		},
		{
			// 修复前即绿;钉住已发出的请求不随预算作废:截止后才回的放行照常生效,不被预算检查吞掉。
			name:     "admission arriving after expiry still counts",
			steps:    []queueCall{queued, {"queue-status", "T1", admittedResponse(), nil}},
			expireAt: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inner := &scriptedGateway{t: t, steps: tc.steps}
			gw := &budgetExpiresDuring{inner: inner, at: tc.expireAt, cancel: cancel}
			var waits []time.Duration
			rsp, err := AssignGateWithQueue(ctx, gw, &httpAssignGateRequest{ZoneID: 1}, nil, recordSleeps(&waits), zeroJitter)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want admitted", err)
				}
				if rsp.Code != 0 {
					t.Fatalf("code = %d, want 0", rsp.Code)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if len(inner.steps) != 0 {
				t.Fatalf("%d scripted calls never made, next %s(token=%q)", len(inner.steps), inner.steps[0].endpoint, inner.steps[0].token)
			}
			// 只有首次排队后的那一次等待;预算到期之后不再进入任何等待。
			if want := []time.Duration{2 * time.Second}; !slices.Equal(waits, want) {
				t.Fatalf("waits = %v, want %v", waits, want)
			}
		})
	}
}

// TestAssignGateWithQueue_ReentryFailureStopsAtBudget:重入自身失败后保票等待(2026-11-20 起),预算恰在这次重入途中
// 到期时停在等待处返回,不再查 queue-status。脚本不留后续步,多发任何请求都会以 script exhausted 判失败。
func TestAssignGateWithQueue_ReentryFailureStopsAtBudget(t *testing.T) {
	errNet := errors.New("dial tcp: connection refused")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := &scriptedGateway{t: t, steps: []queueCall{
		{"assign-gate", "", loginQueued("T1", 7, 2000), nil},
		{"queue-status", "T1", nil, errNet},
		{"assign-gate", "T1", nil, errNet},
	}}
	gw := &budgetExpiresDuring{inner: inner, at: 3, cancel: cancel}
	var waits []time.Duration
	_, err := AssignGateWithQueue(ctx, gw, &httpAssignGateRequest{ZoneID: 1}, nil, recordSleeps(&waits), zeroJitter)
	if err == nil || !strings.Contains(err.Error(), "queue budget exceeded") {
		t.Fatalf("err = %v, want queue budget exceeded", err)
	}
	if len(inner.steps) != 0 {
		t.Fatalf("%d scripted calls never made", len(inner.steps))
	}
	// 首次排队后一次;重入失败后进入的那次等待因预算已到期立即返回(recordSleeps 照样记下),之后不再发请求。
	if want := []time.Duration{2 * time.Second, 2 * time.Second}; !slices.Equal(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// TestSleepCtx_ExpiredBudgetNeverReturnsNil:排队预算已用完时 sleepCtx 必须返回错误,即使计时器同时到点。
// 修复前红(几乎必然):d=0 的计时器与已取消的 ctx 同时就绪,select 随机选一支,每次一半概率走计时器分支返回 nil;
// 64 次全部侥幸走 ctx 分支的概率是 2^-64。修复后计时器分支再查 ctx,结果确定。不真等待,不依赖墙钟。
func TestSleepCtx_ExpiredBudgetNeverReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 64 {
		if err := sleepCtx(ctx, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("attempt %d: sleepCtx = %v, want context.Canceled", i, err)
		}
	}
}

// TestAssignGateWithQueue_Jitter 钉住抖动口径:持 token 时每次等待 = 基础间隔 + [0, 基础间隔的 20%];
// 不持 token 的分支(按 retry_after_ms 重调 assign-gate)不加抖动,抖动源一次都不调。
func TestAssignGateWithQueue_Jitter(t *testing.T) {
	errNet := errors.New("dial tcp: connection refused")
	cases := []struct {
		name            string
		steps           []queueCall
		wantWaits       []time.Duration
		wantJitterLimit []time.Duration // 抖动源每次收到的上界,nil 表示一次都不该调
	}{
		{
			// 修复前红:持 token 的等待不加抖动(抖动源一次都没被调),等待恰为 retry_after。
			// 抖动取满时等待 = 基础间隔 × 1.2,上界恰为基础间隔的 20%;退避下限与 retry_after 都按同一比例加抖动。
			name: "held token waits add up to 20% of the base interval",
			steps: []queueCall{
				{"assign-gate", "", loginQueued("T1", 1, 2000), nil}, // 基础 2s
				{"queue-status", "T1", nil, errNet},
				{"assign-gate", "T1", codeResponse(429, "IP_RATE_LIMIT"), nil}, // 连续失败 1 次,基础 2s
				{"queue-status", "T1", codeResponse(500, "login_unavailable"), nil},
				{"assign-gate", "T1", rateLimitQueued(1), nil},          // 连续失败 2 次,基础 4s
				{"queue-status", "T1", loginQueued("T1", 1, 5000), nil}, // 成功复位,基础取 retry_after 5s
				{"queue-status", "T1", admittedResponse(), nil},
			},
			wantWaits: []time.Duration{
				2400 * time.Millisecond, 2400 * time.Millisecond, 4800 * time.Millisecond, 6 * time.Second,
			},
			wantJitterLimit: []time.Duration{
				400 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second,
			},
		},
		{
			// 修复前即绿(本来就没有抖动);钉住不持 token 的分支口径不变:按 retry_after_ms(缺省 2s)等待,不调抖动源。
			name: "without a token the jitter source is never called",
			steps: []queueCall{
				{"assign-gate", "", rateLimitQueued(500), nil},
				{"assign-gate", "", rateLimitQueued(0), nil},
				{"assign-gate", "", admittedResponse(), nil},
			},
			wantWaits:       []time.Duration{500 * time.Millisecond, 2 * time.Second},
			wantJitterLimit: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &scriptedGateway{t: t, steps: tc.steps}
			var waits, jitterLimits []time.Duration
			rsp, err := AssignGateWithQueue(context.Background(), gw, &httpAssignGateRequest{ZoneID: 1}, nil,
				recordSleeps(&waits), recordFullJitter(&jitterLimits))
			if err != nil {
				t.Fatalf("err = %v, want admitted", err)
			}
			if rsp.Code != 0 {
				t.Fatalf("code = %d, want 0", rsp.Code)
			}
			if len(gw.steps) != 0 {
				t.Fatalf("%d scripted calls never made, next %s(token=%q)", len(gw.steps), gw.steps[0].endpoint, gw.steps[0].token)
			}
			if !slices.Equal(waits, tc.wantWaits) {
				t.Fatalf("waits = %v, want %v", waits, tc.wantWaits)
			}
			if !slices.Equal(jitterLimits, tc.wantJitterLimit) {
				t.Fatalf("jitter limits = %v, want %v", jitterLimits, tc.wantJitterLimit)
			}
		})
	}
}

// TestAssignGateWithQueue_HeldTokenWait 直接钉住 heldTokenWait 的边界:退避逐次翻倍封顶 30s、失败次数再大也不溢出、
// 服务端给的更长 retry_after 优先、注入的抖动越界时被夹回 [0, 基础间隔的 20%]。
func TestAssignGateWithQueue_HeldTokenWait(t *testing.T) {
	fixedJitter := func(d time.Duration) func(time.Duration) time.Duration {
		return func(time.Duration) time.Duration { return d }
	}
	cases := []struct {
		name         string
		retryAfterMs int64
		failures     int
		jitter       func(time.Duration) time.Duration
		want         time.Duration
	}{
		{"no failure, retry_after below the floor", 1, 0, zeroJitter, 2 * time.Second},
		{"no failure, negative retry_after", -5, 0, zeroJitter, 2 * time.Second},
		{"no failure, retry_after above the floor", 3500, 0, zeroJitter, 3500 * time.Millisecond},
		{"1 failure", 0, 1, zeroJitter, 2 * time.Second},
		{"4 failures", 0, 4, zeroJitter, 16 * time.Second},
		{"5 failures hit the cap", 0, 5, zeroJitter, 30 * time.Second},
		{"huge failure count stays at the cap", 0, 1 << 20, zeroJitter, 30 * time.Second},
		{"longer server retry_after wins over the cap", 45000, 9, zeroJitter, 45 * time.Second},
		{"negative jitter clamped to 0", 0, 0, fixedJitter(-time.Second), 2 * time.Second},
		{"oversized jitter clamped to 20%", 0, 3, fixedJitter(time.Minute), 8*time.Second + 1600*time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := heldTokenWait(tc.retryAfterMs, tc.failures, tc.jitter); got != tc.want {
				t.Fatalf("heldTokenWait(%d, %d) = %v, want %v", tc.retryAfterMs, tc.failures, got, tc.want)
			}
		})
	}
}
