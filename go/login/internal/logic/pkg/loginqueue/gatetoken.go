// gatetoken.go — gate selection + HMAC signing primitives shared by the
// AssignGate handler (fast path, no queue) and the dispatcher (slow path,
// signing on behalf of a popped queue entry).
//
// This is a lift-and-shift from the legacy `pregate.AssignGate` /
// `pregate.signHMAC` pair, made into a reusable helper so the queue
// dispatcher and the handler both use a single source of truth for gate
// token shape. The legacy file `pregate/getgatelistlogic.go` is removed in
// PR3.
//
// signQueueToken signs the OPAQUE queue token (different from gate token):
// it carries (queueId, zoneId, expire) and is verified server-side only.
// Reusing the gate HMAC secret is fine because the wire formats are
// disjoint — gate verifies a serialized GateTokenPayload proto, queue
// verifies a JSON blob with a "kind":"queue" tag.
package loginqueue

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mathrand "math/rand/v2"
	"time"

	commonpb "proto/common/base"
	"shared/gatepick"

	"google.golang.org/protobuf/proto"
)

// hmacSessionKeyLen is the size in bytes of the per-session HMAC key carried
// inside GateTokenPayload (todo.md #76 message tampering). 32 bytes = 256
// bits matches HMAC-SHA256's natural key size and is well above the 128-bit
// security floor expected for anti-tamper. The key is regenerated on every
// AssignGate call — there is no Redis state to rotate.
const hmacSessionKeyLen = 32

// GateCandidate is the projection of NodeInfo we need for selection. Kept
// minimal so capacity.go can build it from any source (etcd watcher today,
// possibly a metrics-driven view later) without lugging the full proto.
type GateCandidate struct {
	NodeID      uint32
	IP          string
	Port        uint32
	PlayerCount uint32
	ZoneID      uint32
	// MaxConnections 是这台 gate 自报的客户端连接上限(NodeInfo.max_connections,即它的 GateMaxConnections),
	// 与 PlayerCount 同一单位。0 = 没声明(dev 下不设上限的 gate),不按上限过滤。
	MaxConnections uint32
	// Stale:这台 gate 的注册记录已有三个上报周期没被重写,多半卡住了(见 node.WatchedNode.Stale)。
	Stale bool
}

// PickAndSignGateToken selects a gate from the candidate list (already
// filtered by zone) with PickGate's two-random-choices rule and signs a
// one-time HMAC token. Returns (admit, error) — admit is non-nil iff a gate
// was selected.
//
// Returned AdmitToken matches the wire shape AssignGateResponse expects, so
// callers can copy fields directly without re-marshalling.
//
// Convenience wrapper around PickGate + SignGateToken — kept for the
// AssignGate fast-path (no queue) which still picks-and-signs in one shot
// because there is no admit-cache delay between the two halves.
//
// The QUEUE path (PopAdmit + consumeAdmit) deliberately splits the two
// stages: dispatcher picks at admit time (gatePicker via newBatchPickFn,
// not PickGate), sign at consume time. See
// the comment on `consumeAdmitAndSign` for the rationale (R17 R1 root
// cause — admit could sit in Redis for 4-5 minutes and burn the token's
// 5-min TTL before the client redeemed it).
// (2026-11-05)取走现签之前还会复核放行时选定的 gate,不合格就用 PickGate 的规则重挑,见 admit_recheck.go 的 NewAdmitSigner。
func PickAndSignGateToken(candidates []GateCandidate, secret []byte, ttl time.Duration) (*AdmitToken, error) {
	gate, err := PickGate(candidates)
	if err != nil {
		return nil, err
	}
	return SignGateToken(gate, secret, ttl)
}

// PickGate is the gate-selection half of PickAndSignGateToken — chooses
// a candidate without signing. Returned value carries the minimum endpoint
// info needed for a later SignGateToken call to produce a fresh-TTL token.
//
// 快速通道的挑法(2026-10-18 起):在候选里随机抽两台不同的 gate,取人数少的那台,同人数取第一次抽中的那台;
// 只有一台就是它,恰好两台时等于取最少。不改调用方的切片,不跨调用记账。
// 同人数不能按 NodeID 定胜负:开新区第一个上报窗口里所有 gate 都报 0,按 NodeID 定会让小号 gate 拿双倍份额、
// NodeID 最大的那台一个人都分不到。抽样是均匀的有序对,同人数时两台各一半,全体就是均匀的。
//
// 为什么不再取全局最少:PlayerCount 来自 etcd,gate 每 10s 才上报一次。快速通道每个请求各挑各的,
// 互相看不到别的请求刚分出去的人,确定性地取最少会让一个上报周期里本 zone 的新连接全部落到同一台 gate,
// 开新区时一个窗口就是成千上万个(容量模型 §2.1「PickGate 集中度」、A0-7)。随机二选一在负载信息滞后时
// 仍能把人摊开;最忙的那台只有和同样忙的 gate 比时才会被选中。
//
// dispatcher 一拍放一批,在同一份快照上连续挑并记账,用的是确定性的 gatePicker(同数取 NodeID 小的),规则不同。
//
// 挑之前先剔除停写与满载的 gate(eligibleGates,两条挑法共用);全部不合格时回落全集。
func PickGate(candidates []GateCandidate) (*GateCandidate, error) {
	return pickTwoChoices(eligibleGates(candidates), mathrand.IntN)
}

// pickTwoChoices 是 PickGate 的实现,随机源显式传入:intn(n) 返回 [0, n) 上均匀分布的整数
// (生产用 math/rand/v2 的全局源,并发安全)。单测传固定序列钉住结果。
// 挑选规则本身在 shared/gatepick(scene_manager 跨 zone 重定向共用同一份),这里只做 GateCandidate 的适配。
func pickTwoChoices(candidates []GateCandidate, intn func(int) int) (*GateCandidate, error) {
	i, ok := gatepick.TwoChoices(candidates, func(c GateCandidate) uint32 { return c.PlayerCount }, intn)
	if !ok {
		return nil, fmt.Errorf("no gate candidates")
	}
	gate := candidates[i]
	return &gate, nil
}

// gatePicker 在一份候选快照上连续挑 gate,挑一次记一次账:负载 = PlayerCount + 本快照上已分给它的人数,
// 取负载最少的,同负载取 NodeID 最小的。结果确定,看 dispatcher 日志时能推出为什么挑中了某台 gate;
// 负载与 NodeID 都相同(重复候选)时取靠前的那个。
//
// 为什么要记账:PlayerCount 来自 etcd,gate 每 10s 才上报一次,一拍之内不变。只按 PlayerCount 挑,
// 一拍放的 N 个人会全部落在同一台 gate 上;把本拍已分出去的人数算进负载,一拍的放行先填最闲的 gate,
// 填平之后在各台之间轮转。账只记在这份快照里,不跨拍:下一拍重新取候选,从 gate 上报的人数重新算起。
//
// 构造时复制候选,之后调用方改原切片不影响记账。不是并发安全的:dispatcher 经 PopAdmit 逐条串行调用。
type gatePicker struct {
	candidates []GateCandidate
	// assigned 与 candidates 同下标:本快照上已经分给该 gate 的人数。
	assigned []uint64
}

func newGatePicker(candidates []GateCandidate) *gatePicker {
	return &gatePicker{
		candidates: append([]GateCandidate(nil), candidates...),
		assigned:   make([]uint64, len(candidates)),
	}
}

// pick 返回负载最少的 gate(副本),并给它记一人。没有候选时返回错误,不记账。
func (p *gatePicker) pick() (*GateCandidate, error) {
	if len(p.candidates) == 0 {
		return nil, fmt.Errorf("no gate candidates")
	}
	best := 0
	for i := 1; i < len(p.candidates); i++ {
		if p.lighter(i, best) {
			best = i
		}
	}
	p.assigned[best]++
	gate := p.candidates[best]
	return &gate, nil
}

// lighter 报告第 i 个候选是否严格排在第 j 个之前:负载少的在前,同负载 NodeID 小的在前。
// 负载用 uint64 相加,PlayerCount 接近 uint32 上限时也不回绕。
func (p *gatePicker) lighter(i, j int) bool {
	loadI := uint64(p.candidates[i].PlayerCount) + p.assigned[i]
	loadJ := uint64(p.candidates[j].PlayerCount) + p.assigned[j]
	if loadI != loadJ {
		return loadI < loadJ
	}
	return p.candidates[i].NodeID < p.candidates[j].NodeID
}

// SignGateToken builds + HMACs a GateTokenPayload for the chosen gate.
// `ttl` starts from time.Now() at the moment this call runs — that is the
// whole point of separating it from PickGate: a slow Redis round-trip
// between admit-pick and client-consume must NOT eat into the token's
// validity window.
//
// Caller is responsible for handing AdmitToken back to the client (write
// to admit:{queueId}, return inline in AssignGateResponse, etc.). The
// shape mirrors what AssignGateResponse expects.
func SignGateToken(gate *GateCandidate, secret []byte, ttl time.Duration) (*AdmitToken, error) {
	if gate == nil {
		return nil, fmt.Errorf("nil gate")
	}

	expireTS := time.Now().Add(ttl).Unix()

	// Generate a fresh per-session HMAC key (todo.md #76 slice A). 32
	// random bytes from crypto/rand. The key piggybacks on the existing
	// HMAC-SHA256 signature over payloadBytes, so an attacker tampering
	// with it on the wire would invalidate the gate token signature and
	// the gate would reject the whole handshake. No Redis state — fresh
	// key on every sign call (which is now per-consume, not per-admit).
	sessionKey := make([]byte, hmacSessionKeyLen)
	if _, err := rand.Read(sessionKey); err != nil {
		return nil, fmt.Errorf("generate hmac session key: %w", err)
	}

	payload := &commonpb.GateTokenPayload{
		GateNodeId:      gate.NodeID,
		ZoneId:          gate.ZoneID,
		ExpireTimestamp: expireTS,
		HmacSessionKey:  sessionKey,
	}
	payloadBytes, err := proto.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal gate token payload: %w", err)
	}

	signature := hmacHex(secret, payloadBytes)

	return &AdmitToken{
		IP:             gate.IP,
		Port:           gate.Port,
		TokenPayload:   payloadBytes,
		TokenSignature: signature,
		TokenDeadline:  expireTS,
	}, nil
}

// hmacHex computes HMAC-SHA256(secret, data) and returns the hex-encoded
// string as bytes. Matches the cpp gate verifier's HmacSha256Hex output
// (lowercase hex, no separators) so the on-wire bytes line up.
func hmacHex(secret, data []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

// ── Queue token (server-only, opaque to client) ────────────────────────

// queueTokenBody is the JSON we sign + base64-encode as the queue_token.
// Clients MUST treat it as opaque; we re-parse on the server only.
type queueTokenBody struct {
	Kind     string `json:"k"`
	QueueID  string `json:"q"`
	ZoneID   uint32 `json:"z"`
	ExpireTS int64  `json:"e"`
}

// signQueueToken returns base64url(JSON | "." | hex(HMAC)). We pick this
// shape over a Redis-stored opaque token because:
//   - Self-validating: server can reject expired/forged tokens before
//     touching Redis.
//   - No extra Redis key per token (the queue:token:{token} index is for
//     fast Lookup, but we could fall back to parse-only if Redis evicted
//     the index — Lookup currently relies on the index, kept for clarity).
func signQueueToken(secret []byte, queueID string, zoneID uint32, expireTS int64) (string, error) {
	body := queueTokenBody{Kind: "queue", QueueID: queueID, ZoneID: zoneID, ExpireTS: expireTS}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	sig := hmacHex(secret, bodyBytes)
	combined := append(append(bodyBytes, '.'), sig...)
	return base64.RawURLEncoding.EncodeToString(combined), nil
}

// ParseAndVerifyQueueToken returns the embedded queueId/zoneId or an error
// if the signature is bad / token expired. Pure CPU — no Redis touch.
//
// On rejection it increments expiredTotal with a reason label so dashboards
// can distinguish "client tampering / forged token" from "client polled
// slower than the TTL". Note we record under zone=unknown when we can't
// parse the body (signature mismatch before unmarshalling); for TTL
// rejection we have the real zoneID.
//
// 单密钥版本,保留给测试与不涉及轮换的调用方;生产路径请用
// ParseAndVerifyQueueTokenMulti,它在密钥轮换期能同时认新旧 token。
//
// (2026-10-18 更正)上面「client polled slower than the TTL」已不成立:排队令牌的过期改为绝对上限
// queueTokenMaxAge(24h),空闲过期由 Redis 侧的空闲 TTL 管、计为 meta_gone / queue_gone。reason=ttl 现在只表示
// 这张令牌签发已超过 24h(或旧版本签发的 +QueueEntryTTL 令牌到期)。
func ParseAndVerifyQueueToken(secret []byte, token string) (queueID string, zoneID uint32, err error) {
	return ParseAndVerifyQueueTokenMulti([][]byte{secret}, token)
}

// ParseAndVerifyQueueTokenMulti 依次用给定的候选密钥校验排队 token。
//
// 候选顺序是「主密钥在前,只验不签的旧密钥在后」。三段式不停服轮换靠的就是
// 这个列表:新密钥先进候选(所有实例都能验旧 token),再提升为主密钥开始签,
// 最后把旧密钥从候选里摘掉。任意一步都能单独滚动发布。
// (2026-10-18 补注)最后一步(摘旧密钥)须在「提升为主密钥」全量生效后至少等 queueTokenMaxAge(24h):
// 旧密钥签出的排队令牌最长 24h 内仍合法,提前摘掉会把仍在排队的玩家判成 bad_signature、打回队尾。
//
// 候选为空 = 密钥没配好,一律拒绝(fail-closed),绝不当成「无需校验」。
func ParseAndVerifyQueueTokenMulti(secrets [][]byte, token string) (queueID string, zoneID uint32, err error) {
	if len(secrets) == 0 {
		recordExpired(0, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("queue token secret not configured")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		recordExpired(0, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("decode queue token: %w", err)
	}
	dot := -1
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(raw)-1 {
		recordExpired(0, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("malformed queue token")
	}
	bodyBytes := raw[:dot]
	sig := raw[dot+1:]
	// 不短路:命中后继续把剩下的密钥算完,免得从耗时差反推出命中的是哪一把。
	matched := false
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		if hmac.Equal(sig, hmacHex(secret, bodyBytes)) {
			matched = true
		}
	}
	if !matched {
		recordExpired(0, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("queue token signature mismatch")
	}
	var body queueTokenBody
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		recordExpired(0, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("unmarshal queue token: %w", err)
	}
	if body.Kind != "queue" {
		recordExpired(body.ZoneID, ExpireReasonBadSignature)
		return "", 0, fmt.Errorf("queue token wrong kind: %s", body.Kind)
	}
	if time.Now().Unix() > body.ExpireTS {
		recordExpired(body.ZoneID, ExpireReasonTTL)
		return "", 0, fmt.Errorf("queue token expired")
	}
	return body.QueueID, body.ZoneID, nil
}
