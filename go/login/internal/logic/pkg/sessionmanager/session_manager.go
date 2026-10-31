// Package sessionmanager replaces Centre's in-memory session management.
// Session I/O is delegated to the player_locator gRPC service (single source of truth).
// Pure decision logic (DecideEnterGame, CanReconnect) remains local.
package sessionmanager

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	comppb "proto/common/component"
	plpb "proto/player_locator"
	"shared/redisslot"
)

// PlayerSession is the Login-side view of a player session.
// It mirrors player_locator.PlayerSession for local decision logic.
type PlayerSession struct {
	PlayerID       uint64 `json:"player_id"`
	SessionID      uint32 `json:"session_id"`
	GateID         string `json:"gate_id"`
	GateInstanceID string `json:"gate_instance_id"`
	SceneNodeID    string `json:"scene_node_id"`
	SceneID        uint64 `json:"scene_id"`
	Account        string `json:"account"`
	SessionVersion uint32 `json:"session_version"`
	State          int32  `json:"state"` // 0=unknown, 1=online, 2=disconnecting, 3=offline
	LastActiveTs   int64  `json:"last_active_ts"`
	RequestID      string `json:"request_id"`
	// LoginGeneration 是写下这条会话的登录链铸出的代次(login_generation.go);0 = 旧版 login 写入、无代次。
	// 下一次铸造以它为下限,保证 player_locator 提交点"严格递增"的校验能过(scene-enter-session-fence.md §3.4)。
	LoginGeneration uint64 `json:"login_generation"`
}

// EnterGameDecision mirrors Centre's enum.
type EnterGameDecision int

const (
	FirstLogin     EnterGameDecision = 0
	ShortReconnect EnterGameDecision = 1
	ReplaceLogin   EnterGameDecision = 2
)

const (
	StateOnline        int32 = 1
	StateDisconnecting int32 = 2
	StateOffline       int32 = 3

	idempotencyTTL = 5 * time.Minute
)

// EnterGsType values derived from proto EnterGsType enum (player_login_comp.proto).
const (
	EnterGsTypeNone      = uint32(comppb.EnterGsType_LOGIN_NONE)
	EnterGsTypeFirst     = uint32(comppb.EnterGsType_LOGIN_FIRST)
	EnterGsTypeReplace   = uint32(comppb.EnterGsType_LOGIN_REPLACE)
	EnterGsTypeReconnect = uint32(comppb.EnterGsType_LOGIN_RECONNECT)
)

// DecisionToEnterGsType maps a login decision to the corresponding proto enter_gs_type value.
func DecisionToEnterGsType(d EnterGameDecision) uint32 {
	switch d {
	case FirstLogin:
		return EnterGsTypeFirst
	case ShortReconnect:
		return EnterGsTypeReconnect
	case ReplaceLogin:
		return EnterGsTypeReplace
	default:
		return EnterGsTypeNone
	}
}

type OnlineSessionInput struct {
	PlayerID       uint64
	SessionID      uint32
	GateID         string
	GateInstanceID string
	Account        string
	RequestID      string
	Now            time.Time
	// LoginGeneration 是本条登录链铸出的代次,原样写进会话记录。调用方保证非 0:铸造失败的链走不到写会话。
	LoginGeneration uint64
}

func idempotencyKey(playerID uint64, requestID string) string {
	return redisslot.LoginIdempotentKey(playerID, requestID)
}

// ---- I/O functions (delegate to player_locator gRPC) ----

// GetSession retrieves the player session from player_locator. Returns nil if not found.
func GetSession(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64) (*PlayerSession, error) {
	resp, err := plClient.GetSession(ctx, &plpb.GetSessionRequest{PlayerId: playerID})
	if err != nil {
		return nil, fmt.Errorf("player_locator GetSession: %w", err)
	}
	if !resp.Found {
		return nil, nil
	}
	return fromProto(resp.Session), nil
}

// SetSession writes/overwrites a player session via player_locator.
func SetSession(ctx context.Context, plClient plpb.PlayerLocatorClient, session *PlayerSession) error {
	_, err := plClient.SetSession(ctx, &plpb.SetSessionRequest{Session: toProto(session)})
	if err != nil {
		return fmt.Errorf("player_locator SetSession: %w", err)
	}
	return nil
}

// SetSessionDisconnecting marks the session as disconnecting with a 30s lease via player_locator.
//
// gateInstanceID 是发起断线的 gate 实例 uuid(SessionDetails.gate_instance_id,或入场链 state 里的同一个值)。
// player_locator 在它与会话记录的实例号都非空且不等时,按旧实例迟到的上报忽略;空 = 不校验
// (docs/design/scene-enter-chain-residuals.md §6)。
func SetSessionDisconnecting(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64, sessionID uint32, gateInstanceID string) error {
	_, err := plClient.SetDisconnecting(ctx, &plpb.SetDisconnectingRequest{
		PlayerId:        playerID,
		SessionId:       sessionID,
		LeaseTtlSeconds: 30,
		GateInstanceId:  gateInstanceID,
	})
	if err != nil {
		return fmt.Errorf("player_locator SetDisconnecting: %w", err)
	}
	return nil
}

// Reconnect cancels the disconnect lease and restores the session to online via player_locator.
//
// loginGeneration 是本条重连链铸出的代次,由 player_locator 随整字节 CAS 写进会话记录。
// 返回值是 locator 回带的会话记录:旧版 locator 不认这个字段,会原样回出上一条会话的代次,
// 所以调用方下游一律用自己铸出的值,不用返回值里的(scene-enter-session-fence.md §3.4)。
func Reconnect(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64,
	newSessionID uint32, gateID string, gateInstanceID string,
	account string, requestID string, loginGeneration uint64,
) (*PlayerSession, error) {
	resp, err := plClient.Reconnect(ctx, &plpb.ReconnectRequest{
		PlayerId:        playerID,
		NewSessionId:    newSessionID,
		GateId:          gateID,
		GateInstanceId:  gateInstanceID,
		Account:         account,
		RequestId:       requestID,
		LoginGeneration: loginGeneration,
	})
	if err != nil {
		return nil, fmt.Errorf("player_locator Reconnect: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("reconnect failed: %s", resp.ErrorMessage)
	}
	return fromProto(resp.Session), nil
}

// DeleteSession removes only the exact Gate session that issued LeaveGame.
// A delayed request from a replaced connection is intentionally a successful no-op.
//
// expectedGateInstanceID 是发起登出的 gate 实例 uuid,原样随 MarkOffline 下发;空 = 旧口径,locator 不比较。
// 实例号**不做本地预检**:本地只按会话号挡掉常见的「被替换连接迟到的登出」,同号跨实例交给 player_locator
// 在 GET 到的同一份字节上原子判定,并在那里打 Info、计 player_locator_stale_gate_instance_total{op="mark_offline"}。
// 本地也比的话会先短路,locator 的日志与计数只在 GetSession 与 locator GET 之间记录被改写时才走得到,
// LeaveGame 这一面就观测不到了。代价是罕见的同号跨实例多一次 RPC(docs/design/scene-enter-chain-residuals.md §6.2)。
func DeleteSession(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64, expectedSessionID uint32, expectedGateInstanceID string) error {
	current, err := GetSession(ctx, plClient, playerID)
	if err != nil {
		return err
	}
	if current == nil || current.SessionID != expectedSessionID {
		return nil
	}

	_, err = plClient.MarkOffline(ctx, &plpb.MarkOfflineRequest{
		PlayerId:               playerID,
		ExpectedSessionId:      expectedSessionID,
		ExpectedSessionVersion: current.SessionVersion,
		ExpectedGateInstanceId: expectedGateInstanceID,
	})
	if err != nil {
		return fmt.Errorf("player_locator MarkOffline: %w", err)
	}
	return nil
}

// ---- Pure logic (no I/O) ----

// DecideEnterGame implements the login decision logic.
func DecideEnterGame(existing *PlayerSession, incomingAccount string) EnterGameDecision {
	if existing == nil {
		return FirstLogin
	}
	if CanReconnect(existing, incomingAccount) {
		return ShortReconnect
	}
	return ReplaceLogin
}

func CanReconnect(existing *PlayerSession, incomingAccount string) bool {
	if existing == nil {
		return false
	}
	return existing.State == StateDisconnecting && existing.Account == incomingAccount
}

func NewOnlineSession(existing *PlayerSession, input OnlineSessionInput) *PlayerSession {
	session := &PlayerSession{
		PlayerID: input.PlayerID,
	}
	if existing != nil {
		session.SessionVersion = existing.SessionVersion
	}
	applyOnlineSession(session, input)
	session.SessionVersion = nextSessionVersion(existing)
	return session
}

// ---- Idempotency (stays in Redis — not session-related) ----

func SetIdempotency(ctx context.Context, rdb redis.UniversalClient, playerID uint64, requestID string) error {
	if requestID == "" {
		return nil
	}
	key := idempotencyKey(playerID, requestID)
	return rdb.Set(ctx, key, "1", idempotencyTTL).Err()
}

// ---- Proto conversion helpers ----

func toProto(s *PlayerSession) *plpb.PlayerSession {
	return &plpb.PlayerSession{
		PlayerId:        s.PlayerID,
		SessionId:       s.SessionID,
		GateId:          s.GateID,
		GateInstanceId:  s.GateInstanceID,
		SceneNodeId:     s.SceneNodeID,
		SceneId:         s.SceneID,
		SessionVersion:  s.SessionVersion,
		State:           plpb.PlayerSessionState(s.State),
		LastActiveTs:    s.LastActiveTs,
		RequestId:       s.RequestID,
		Account:         s.Account,
		LoginGeneration: s.LoginGeneration,
	}
}

func fromProto(pb *plpb.PlayerSession) *PlayerSession {
	if pb == nil {
		return nil
	}
	return &PlayerSession{
		PlayerID:        pb.PlayerId,
		SessionID:       pb.SessionId,
		GateID:          pb.GateId,
		GateInstanceID:  pb.GateInstanceId,
		SceneNodeID:     pb.SceneNodeId,
		SceneID:         pb.SceneId,
		Account:         pb.Account,
		SessionVersion:  pb.SessionVersion,
		State:           int32(pb.State),
		LastActiveTs:    pb.LastActiveTs,
		RequestID:       pb.RequestId,
		LoginGeneration: pb.LoginGeneration,
	}
}

func applyOnlineSession(session *PlayerSession, input OnlineSessionInput) {
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}

	session.PlayerID = input.PlayerID
	session.SessionID = input.SessionID
	session.GateID = input.GateID
	session.GateInstanceID = input.GateInstanceID
	session.Account = input.Account
	session.State = StateOnline
	session.LastActiveTs = now.UnixMilli()
	session.RequestID = input.RequestID
	session.LoginGeneration = input.LoginGeneration
}

func nextSessionVersion(existing *PlayerSession) uint32 {
	if existing == nil {
		return 1
	}
	return existing.SessionVersion + 1
}
