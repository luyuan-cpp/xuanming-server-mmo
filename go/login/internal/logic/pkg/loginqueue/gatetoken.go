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
	"sort"
	"time"

	commonpb "proto/common/base"

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
}

// PickAndSignGateToken selects the least-loaded gate from the candidate
// list (already filtered by zone) and signs a one-time HMAC token. Returns
// (admit, error) — admit is non-nil iff a gate was selected.
//
// Returned AdmitToken matches the wire shape AssignGateResponse expects, so
// callers can copy fields directly without re-marshalling.
//
// Convenience wrapper around PickGate + SignGateToken — kept for the
// AssignGate fast-path (no queue) which still picks-and-signs in one shot
// because there is no admit-cache delay between the two halves.
//
// The QUEUE path (PopAdmit + consumeAdmit) deliberately splits the two
// stages: dispatcher PickGate at admit time, sign at consume time. See
// the comment on `consumeAdmitAndSign` for the rationale (R17 R1 root
// cause — admit could sit in Redis for 4-5 minutes and burn the token's
// 5-min TTL before the client redeemed it).
func PickAndSignGateToken(candidates []GateCandidate, secret []byte, ttl time.Duration) (*AdmitToken, error) {
	gate, err := PickGate(candidates)
	if err != nil {
		return nil, err
	}
	return SignGateToken(gate, secret, ttl)
}

// PickGate is the gate-selection half of PickAndSignGateToken — chooses
// the least-loaded candidate without signing. Returned value carries the
// minimum endpoint info needed for a later SignGateToken call to produce
// a fresh-TTL token.
//
// Splitting selection from signing lets the queue dispatcher decide
// "this entry is admitted to gate X" at admit time but defer the token
// HMAC + expire until the client actually polls for it.
func PickGate(candidates []GateCandidate) (*GateCandidate, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no gate candidates")
	}

	// Stable sort by player count so ties resolve deterministically — useful
	// when reading dispatcher logs to figure out why gate X kept getting
	// picked. Cost is O(n log n) where n = #gates per zone (small).
	sorted := make([]GateCandidate, len(candidates))
	copy(sorted, candidates)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].PlayerCount != sorted[j].PlayerCount {
			return sorted[i].PlayerCount < sorted[j].PlayerCount
		}
		return sorted[i].NodeID < sorted[j].NodeID
	})
	best := sorted[0]
	return &best, nil
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
func ParseAndVerifyQueueToken(secret []byte, token string) (queueID string, zoneID uint32, err error) {
	return ParseAndVerifyQueueTokenMulti([][]byte{secret}, token)
}

// ParseAndVerifyQueueTokenMulti 依次用给定的候选密钥校验排队 token。
//
// 候选顺序是「主密钥在前,只验不签的旧密钥在后」。三段式不停服轮换靠的就是
// 这个列表:新密钥先进候选(所有实例都能验旧 token),再提升为主密钥开始签,
// 最后把旧密钥从候选里摘掉。任意一步都能单独滚动发布。
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
