package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"robot/metrics"
)

// httpAssignGateRequest mirrors java/gateway_node AssignGateRequest. Snake
// case matches the gateway's Jackson configuration.
type httpAssignGateRequest struct {
	ZoneID uint32 `json:"zone_id"`
	// AccessToken 是 /api/login 签发的 access token。login 只认它解出的账号(免排队、排队身份);
	// 空 = 匿名分配,只在 login 的旧 gate 凭据登录还开着时可用(robot 先分配 gate、连上之后才认证)。
	// 2026-11-12 之前这里是自报的 account,login 已不再读它。
	AccessToken string `json:"access_token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	QueueToken  string `json:"queue_token,omitempty"`
}

// errAssignGateUnauthenticated 标记 /api/assign-gate 回 code=401:带的 access token 无效(过期、已吊销),
// 或没带令牌而 login 已关匿名分配。调用方据此丢掉令牌或停止重试,不按普通失败退避。
var errAssignGateUnauthenticated = errors.New("assign-gate unauthenticated")

// httpAssignGateResponse mirrors AssignGateResponse from Java side. The DTO
// is shared between the initial /assign-gate response and the polling
// /queue-status response, so a single shape covers both.
type httpAssignGateResponse struct {
	Code           int    `json:"code"`
	GateIP         string `json:"gate_ip"`
	GatePort       int    `json:"gate_port"`
	TokenPayload   []byte `json:"token_payload"`
	TokenSignature []byte `json:"token_signature"`
	TokenDeadline  int64  `json:"token_deadline"`
	Error          string `json:"error"`

	// queue layer (code=100)
	RetryAfterMs int64  `json:"retry_after_ms"`
	QueuePos     int64  `json:"queue_pos"`
	QueueSource  string `json:"queue_source"`
	QueueToken   string `json:"queue_token"`
	QueueRank    int64  `json:"queue_rank"`
	QueueTotal   int64  `json:"queue_total"`
}

// httpAssignGate calls POST /api/assign-gate on the Java Gateway.
func httpAssignGate(gatewayAddr string, req *httpAssignGateRequest, timeout time.Duration) (*httpAssignGateResponse, error) {
	return postJSON(gatewayAddr+"/api/assign-gate", req, timeout)
}

// httpQueueStatus polls POST /api/queue-status with an existing queue_token.
// Used by queue-aware robots when AssignGate returns code=100 + queueSource="login".
//
// zoneId is a routing hint: in 3-zone deployments the Java Gateway uses it to
// dispatch the poll to the same login.rpc instance that issued the token (each
// instance only watches its own zone's queue state). A 0 falls back to
// round-robin, which is fine for single-zone setups but produces sporadic
// EXPIRED responses across multi-zone configs.
func httpQueueStatus(gatewayAddr, queueToken string, zoneId uint32, timeout time.Duration) (*httpAssignGateResponse, error) {
	return postJSON(gatewayAddr+"/api/queue-status",
		map[string]any{"queue_token": queueToken, "zone_id": zoneId}, timeout)
}

func postJSON(url string, payload any, timeout time.Duration) (*httpAssignGateResponse, error) {
	return postJSONWithClient(sharedHTTPClient, url, payload, timeout)
}

// postJSONWithClient 让受限的本机准备入口禁用重定向，仍共用正文编码和错误脱敏规则。
func postJSONWithClient(client *http.Client, url string, payload any, timeout time.Duration) (*httpAssignGateResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: response_bytes=%d", httpResp.StatusCode, len(respBody))
	}
	var out httpAssignGateResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		// 错误会进入日志；应答正文和解码错误的值字段都可能含票据。
		return nil, fmt.Errorf("decode resp: status=%d response_bytes=%d error_type=%T", httpResp.StatusCode, len(respBody), err)
	}
	return &out, nil
}

// gatewayQueueAPI 是排队状态机用到的两个网关端点。生产实现是 httpGatewayQueueAPI;
// 用例注入按脚本应答的假实现,不起 HTTP 服务。
type gatewayQueueAPI interface {
	AssignGate(req *httpAssignGateRequest) (*httpAssignGateResponse, error)
	QueueStatus(queueToken string, zoneID uint32) (*httpAssignGateResponse, error)
}

// httpGatewayQueueAPI 把 gatewayQueueAPI 落到 /api/assign-gate 与 /api/queue-status,每次调用各带 perCallTimeout。
type httpGatewayQueueAPI struct {
	gatewayAddr    string
	perCallTimeout time.Duration
}

func (a httpGatewayQueueAPI) AssignGate(req *httpAssignGateRequest) (*httpAssignGateResponse, error) {
	return httpAssignGate(a.gatewayAddr, req, a.perCallTimeout)
}

func (a httpGatewayQueueAPI) QueueStatus(queueToken string, zoneID uint32) (*httpAssignGateResponse, error) {
	return httpQueueStatus(a.gatewayAddr, queueToken, zoneID, a.perCallTimeout)
}

// sleepCtx 等待 d;ctx 先结束,或到点时 ctx 已结束,都返回 ctx.Err()。AssignGateWithQueue 的生产用 sleep。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// 计时器与 ctx 同时就绪时 select 随机选一支;到点后再查一次 ctx,保证排队预算已用完时不会放出下一次请求。
		return ctx.Err()
	}
}

// uniformJitter 在 [0, limit] 内均匀取一个时长;limit<=0 时返回 0。AssignGateWithQueue 的生产用抖动源。
// math/rand/v2 的包级函数自带种子且并发安全,这里不持有任何可变状态。
func uniformJitter(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	return rand.N(limit + 1)
}

// 持有 queue_token 时的等待口径(open-server-rate-limit-design.md §11.5 的 2026-10-15 更正及其补注)。
const (
	// heldTokenPollFloor 是持 token 时每次轮询前等待的下限:服务端回的 retry_after_ms 比它小(甚至是限流桶给的 1ms)
	// 也至少等这么久,防止 queue-status 故障时「失败 → 重入回 ratelimit(1ms) → 再失败」的毫秒级循环把 login 故障放大。
	heldTokenPollFloor = 2 * time.Second
	// heldTokenJitterPercent:每次等待在基础间隔上再加 [0, 基础间隔的 20%] 的均匀抖动,打散同一时刻排进来的客户端。
	heldTokenJitterPercent = 20
	// heldTokenBackoffCap 是 queue-status 连续失败时指数退避下限的封顶。取 login 放行窗口 AdmitTTL(60s)的一半:
	// 退避本身最多把两次查询的间隔拉到 30s(加抖动 36s),被放行的玩家仍能在放行窗口内再查到一次,不会因退避错过放行。
	// (服务端回的 retry_after_ms 若更大则以服务端为准,那是服务端自己的口径,不属于这里的退避。)
	heldTokenBackoffCap = 30 * time.Second
)

// heldTokenWait 是持有 queue_token 时发下一次 /api/queue-status 前的等待,所有持 token 的等待只经这里计算:
// 基础间隔 = max(retry_after_ms, 下限);下限平时为 heldTokenPollFloor,queue-status 连续失败 n(>=1)次时为
// 2s*2^(n-1),封顶 heldTokenBackoffCap;再加 jitter 在 [0, 基础间隔的 20%] 内取的抖动。
// jitter 的返回值被夹到 [0, 上界],保证等待总在 [基础间隔, 基础间隔×1.2] 内。
func heldTokenWait(retryAfterMs int64, consecutiveFailures int, jitter func(limit time.Duration) time.Duration) time.Duration {
	floor := heldTokenPollFloor
	// 逐次翻倍到封顶为止,不用移位,n 再大也不会溢出。
	for i := 1; i < consecutiveFailures && floor < heldTokenBackoffCap; i++ {
		floor *= 2
	}
	floor = min(floor, heldTokenBackoffCap)
	base := max(time.Duration(retryAfterMs)*time.Millisecond, floor)
	jitterLimit := base * heldTokenJitterPercent / 100
	return base + min(max(jitter(jitterLimit), 0), jitterLimit)
}

// queuePollInterval 只用于不持有 queue_token 时(code=100 ratelimit)重调 /api/assign-gate 前的等待:
// 取 retry_after_ms,为 0 时取 2s,不加抖动,也不设下限(这一分支口径未变)。持 token 时的等待见 heldTokenWait。
func queuePollInterval(rsp *httpAssignGateResponse) time.Duration {
	wait := time.Duration(rsp.RetryAfterMs) * time.Millisecond
	if wait <= 0 {
		wait = 2 * time.Second
	}
	return wait
}

// AssignGateWithQueue 是 robot 的排队状态机:先调 /api/assign-gate,再按回包推进,直到放行(code=0)、
// 遇到不可恢复的回包或网络错误,或 ctx 预算用完。按负责人 2026-10-15 定的客户端契约
// (docs/design/open-server-rate-limit-design.md §11.5 的 2026-10-15 更正及其补注):
//   - 持有 queue_token 时一律轮询 /api/queue-status 查排位、取放行票。该端点不过限流器。
//     每次轮询前的等待见 heldTokenWait:max(retry_after_ms, 下限) 加 [0, 20%] 抖动;下限平时 2s,
//     queue-status 连续失败时按 2s 起翻倍退避,封顶 30s。
//   - /api/queue-status 本身失败(只限网络错误、HTTP 非 200、回包 code==500,见 pollQueueStatus)时,
//     带同一 token 立即重调一次 /api/assign-gate 作恢复,连续失败次数加 1;queue-status 回一次非失败回包即清零,
//     恢复重入的回包不清零。重入回 code=100(ratelimit,或 login 续上原排队条目)或回 code=429,
//     都保留 token、按 heldTokenWait 等待后回到 /api/queue-status 轮询;不按 429 的 30/60/120s 退避重调
//     assign-gate,否则已被放行的玩家会错过 login 的放行窗口(AdmitTTL 60s)。
//   - queue-status 回 503 zone_*(zone_maintenance / zone_closed / zone_not_open,zone 不可进的终态)或
//     404 zone_not_found:不发恢复重入,按错误返回,交外层。
//   - 收到 410 丢掉 token,不带 token 立即重调 /api/assign-gate;此后按不持 token 的口径处理。
//   - 不持有 token 时(口径未变),code=100(ratelimit)按 queuePollInterval 重调 /api/assign-gate,不加抖动;
//     code=429 直接返回错误,交给调用方的外层重试(robot 是 main.go 的 resolveGateViaHTTPLocal,退避节奏以该函数为准,
//     远短于真实客户端此时按设计稿 §11.5 原第 2 条用的 30/60/120s;robot 把 429 当失败,设计稿 §11.7 R2)。
//
// 每个请求发出前都确认排队预算(ctx)未用完:等待都经 sleep,sleep 返回错误即返回;queue-status 失败后紧接的恢复重入、
// 410 后不带 token 的重调不等待,发出前先查 ctx.Err(),预算用完同样立即返回,不再发请求。已发出的请求不随预算取消
// (每次调用各带 perCallTimeout),所以最后一次请求的回包可能在截止后才到,此时回 code=0 仍算放行。
// 恢复用的重入也失败时返回错误,外层重试从头开始,token 随之丢弃(压测可接受)。
//
// req 被就地更新(QueueToken 随回包变化)。stats 可为 nil,口径:首次回 code=100 记 entered,
// 放行时记等待时长与最大名次,丢弃 token 时记 expired。sleep 与 jitter 由调用方注入,
// 生产分别用 sleepCtx 与 uniformJitter;jitter 只在持 token 的等待里调用。
func AssignGateWithQueue(
	ctx context.Context,
	api gatewayQueueAPI,
	req *httpAssignGateRequest,
	stats *metrics.Stats,
	sleep func(context.Context, time.Duration) error,
	jitter func(limit time.Duration) time.Duration,
) (*httpAssignGateResponse, error) {
	var firstQueueAt time.Time
	var maxRankSeen int64 = -1
	enteredQueue := false
	// statusFailures 是 /api/queue-status 连续失败的次数,决定持 token 时的退避下限(heldTokenWait)。
	// 只由 pollHeld 更新:queue-status 失败加 1,回非失败回包清零;恢复重入的回包不动它。
	statusFailures := 0
	pollHeld := func() (*httpAssignGateResponse, error) {
		rsp, statusFailed, err := pollQueueStatus(ctx, api, req)
		if statusFailed {
			statusFailures++
		} else {
			statusFailures = 0
		}
		return rsp, err
	}

	rsp, err := api.AssignGate(req)
	if err != nil {
		return nil, err
	}
	for {
		switch rsp.Code {
		case 0:
			// 放行。排过队的话记等待时长与最大名次。
			if enteredQueue && stats != nil {
				stats.QueueAdmittedAfterWait()
				stats.QueueWait(time.Since(firstQueueAt))
				if maxRankSeen >= 0 {
					stats.QueueRankSeen(maxRankSeen)
				}
			}
			return rsp, nil

		case 100:
			if !enteredQueue {
				enteredQueue = true
				firstQueueAt = time.Now()
				if stats != nil {
					stats.QueueEntered()
				}
			}
			if rsp.QueueRank > maxRankSeen {
				maxRankSeen = rsp.QueueRank
			}
			// 真排队的回包带 token(queue-status 回显同一个);ratelimit 的回包不带,保留手里原有的 token。
			if rsp.QueueSource == "login" && rsp.QueueToken != "" {
				req.QueueToken = rsp.QueueToken
			}
			if req.QueueToken != "" {
				// 持 token:等待走下限 / 退避加抖动,然后轮询 queue-status。
				if err := sleep(ctx, heldTokenWait(rsp.RetryAfterMs, statusFailures, jitter)); err != nil {
					return nil, fmt.Errorf("queue budget exceeded (rank=%d, source=%s)", rsp.QueueRank, rsp.QueueSource)
				}
				rsp, err = pollHeld()
			} else {
				// 还没有 token(首调就被波次或 zone 桶挡住):按 retry_after_ms 等待后重调 assign-gate,让限流器重新判定。
				if err := sleep(ctx, queuePollInterval(rsp)); err != nil {
					return nil, fmt.Errorf("queue budget exceeded (rank=%d, source=%s)", rsp.QueueRank, rsp.QueueSource)
				}
				rsp, err = api.AssignGate(req)
			}
			if err != nil {
				return nil, err
			}

		case 429:
			if req.QueueToken == "" {
				return nil, fmt.Errorf("rate-limited: %s", rsp.Error)
			}
			// 恢复用的重入被 IP 桶挡住:按持 token 的同一口径等待后回到 queue-status 轮询,不退避重调 assign-gate。
			if err := sleep(ctx, heldTokenWait(rsp.RetryAfterMs, statusFailures, jitter)); err != nil {
				return nil, fmt.Errorf("queue budget exceeded (rate-limited: %s)", rsp.Error)
			}
			rsp, err = pollHeld()
			if err != nil {
				return nil, err
			}

		case 410:
			if req.QueueToken == "" {
				// 手里没有 token 却回 410,不在排队契约里;直接报错,免得不等待地反复重调 assign-gate。
				return nil, fmt.Errorf("AssignGate code=410 without queue_token err=%q", rsp.Error)
			}
			// token 失效(排队已关、token 过期,或放行窗口已过):丢掉 token,从 assign-gate 重来。
			if stats != nil {
				stats.QueueExpiredToken()
			}
			req.QueueToken = ""
			// 这次重调不等待,所以在这里确认排队预算未用完:410 可能是预算到期前发出、到期后才回的那次请求。
			if ctx.Err() != nil {
				return nil, fmt.Errorf("queue budget exceeded (code=410 err=%q)", rsp.Error)
			}
			rsp, err = api.AssignGate(req)
			if err != nil {
				return nil, err
			}

		case 401:
			// 鉴权失败不是排队状态:等多久都不会变,交调用方决定丢掉令牌重来还是停止。
			return nil, fmt.Errorf("%w: err=%q", errAssignGateUnauthenticated, rsp.Error)

		default:
			// 含 queue-status 或重入回的 503 zone_* / 404 zone_not_found(zone 不可进),以及重入回的 500:交外层。
			return nil, fmt.Errorf("AssignGate code=%d err=%q", rsp.Code, rsp.Error)
		}
	}
}

// pollQueueStatus 用 req 里的 token 查一次 /api/queue-status,statusFailed 告诉调用方这次 queue-status 本身是否失败
// (调用方据此维护连续失败次数)。「本身失败」的分类只在这里:网络错误、HTTP 非 200(postJSON 返回 error)、
// 或回包 code==500。网关的业务回包一律 HTTP 200 + JSON code,所以 503 zone_* / 404 zone_not_found / 410 等
// 都是非失败回包,原样交回状态机,不发重入。
//
// 失败时带同一 token 立即重调一次 /api/assign-gate 作恢复,把重入的回包交回状态机(statusFailed=true);
// 重入本身也失败(网络错误 / 非 200)才返回错误。发重入前先查 ctx:排队预算已在这次 queue-status 途中用完则不发重入,
// 返回 queue budget exceeded 错误(statusFailed=true)。口径见 open-server-rate-limit-design.md §11.5 的 2026-10-15 更正及其补注。
func pollQueueStatus(ctx context.Context, api gatewayQueueAPI, req *httpAssignGateRequest) (rsp *httpAssignGateResponse, statusFailed bool, err error) {
	rsp, err = api.QueueStatus(req.QueueToken, req.ZoneID)
	if err == nil && rsp.Code != 500 {
		return rsp, false, nil
	}
	if err == nil {
		err = fmt.Errorf("code=%d err=%q", rsp.Code, rsp.Error)
	}
	// 恢复重入前不等待:预算在这次 queue-status 途中用完就不再发(契约「排队预算耗尽即停,不再发请求」)。
	if ctx.Err() != nil {
		return nil, true, fmt.Errorf("queue budget exceeded (queue-status failed: %v)", err)
	}
	reentry, reentryErr := api.AssignGate(req)
	if reentryErr != nil {
		return nil, true, fmt.Errorf("queue-status failed (%v) and re-entry failed: %w", err, reentryErr)
	}
	return reentry, true, nil
}
