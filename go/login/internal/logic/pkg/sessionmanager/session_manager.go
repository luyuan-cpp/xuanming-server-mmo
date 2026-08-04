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

	idempotencyKeyPrefix = "login_idempotent:"
	idempotencyTTL       = 5 * time.Minute
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
}

func idempotencyKey(playerID uint64, requestID string) string {
	return fmt.Sprintf("%s%d:%s", idempotencyKeyPrefix, playerID, requestID)
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
func SetSessionDisconnecting(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64, sessionID uint32) error {
	_, err := plClient.SetDisconnecting(ctx, &plpb.SetDisconnectingRequest{
		PlayerId:        playerID,
		SessionId:       sessionID,
		LeaseTtlSeconds: 30,
	})
	if err != nil {
		return fmt.Errorf("player_locator SetDisconnecting: %w", err)
	}
	return nil
}

// Reconnect cancels the disconnect lease and restores the session to online via player_locator.
func Reconnect(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64,
	newSessionID uint32, gateID string, gateInstanceID string,
	account string, requestID string,
) (*PlayerSession, error) {
	resp, err := plClient.Reconnect(ctx, &plpb.ReconnectRequest{
		PlayerId:       playerID,
		NewSessionId:   newSessionID,
		GateId:         gateID,
		GateInstanceId: gateInstanceID,
		Account:        account,
		RequestId:      requestID,
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
func DeleteSession(ctx context.Context, plClient plpb.PlayerLocatorClient, playerID uint64, expectedSessionID uint32) error {
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

func SetIdempotency(ctx context.Context, rdb *redis.Client, playerID uint64, requestID string) error {
	if requestID == "" {
		return nil
	}
	key := idempotencyKey(playerID, requestID)
	return rdb.Set(ctx, key, "1", idempotencyTTL).Err()
}

// ---- Proto conversion helpers ----

func toProto(s *PlayerSession) *plpb.PlayerSession {
	return &plpb.PlayerSession{
		PlayerId:       s.PlayerID,
		SessionId:      s.SessionID,
		GateId:         s.GateID,
		GateInstanceId: s.GateInstanceID,
		SceneNodeId:    s.SceneNodeID,
		SceneId:        s.SceneID,
		SessionVersion: s.SessionVersion,
		State:          plpb.PlayerSessionState(s.State),
		LastActiveTs:   s.LastActiveTs,
		RequestId:      s.RequestID,
		Account:        s.Account,
	}
}

func fromProto(pb *plpb.PlayerSession) *PlayerSession {
	if pb == nil {
		return nil
	}
	return &PlayerSession{
		PlayerID:       pb.PlayerId,
		SessionID:      pb.SessionId,
		GateID:         pb.GateId,
		GateInstanceID: pb.GateInstanceId,
		SceneNodeID:    pb.SceneNodeId,
		SceneID:        pb.SceneId,
		Account:        pb.Account,
		SessionVersion: pb.SessionVersion,
		State:          int32(pb.State),
		LastActiveTs:   pb.LastActiveTs,
		RequestID:      pb.RequestId,
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
}

func nextSessionVersion(existing *PlayerSession) uint32 {
	if existing == nil {
		return 1
	}
	return existing.SessionVersion + 1
}
