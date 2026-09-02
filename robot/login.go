package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/login"
	"robot/generated/pb/game"
	"robot/metrics"
	"robot/pkg"
)

// defaultLoginTimeout is the maximum time to wait for any single login-flow
// request/response. If a response is not received within this duration, the
// login is considered "stuck" and the attempt fails with a timeout error.
const defaultLoginTimeout = 15 * time.Second

// loginAndEnter performs: login → create player (if needed) → enter game.
func loginAndEnter(gc *pkg.GameClient, password string, stats *metrics.Stats) error {
	// 1. Login
	var lr login.LoginResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{Account: gc.Account, Password: password},
		&lr,
	); err != nil {
		stats.LoginFail()
		return fmt.Errorf("login: %w", err)
	}
	if lr.ErrorMessage != nil {
		stats.LoginFail()
		return fmt.Errorf("login: server error %v", lr.ErrorMessage)
	}

	// Store access/refresh tokens for potential reconnect. Use SetTokens so
	// all token-field writes go through the same mutex as the RefreshToken
	// handler — keeps the API uniform and survives a future refactor that
	// re-runs login on a gc whose RecvLoop already started.
	if lr.AccessToken != "" {
		gc.SetTokens(lr.AccessToken, lr.RefreshToken, lr.AccessTokenExpire, lr.RefreshTokenExpire)
		zap.L().Debug("received auth tokens",
			zap.String("account", gc.Account),
			zap.Int64("access_expire", lr.AccessTokenExpire),
			zap.Int64("refresh_expire", lr.RefreshTokenExpire),
		)
	}

	// 2. Create player if needed
	if len(lr.Players) == 0 {
		var cr login.CreatePlayerResponse
		if err := sendAndRecv(gc, stats,
			game.ClientPlayerLoginCreatePlayerMessageId,
			&login.CreatePlayerRequest{},
			&cr,
		); err != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: %w", err)
		}
		if cr.ErrorMessage != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: server error %v", cr.ErrorMessage)
		}
		lr.Players = cr.Players
	}
	if len(lr.Players) == 0 {
		stats.LoginFail()
		return fmt.Errorf("no players after create")
	}

	// 3. Enter game
	playerId := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerId},
		&er,
	); err != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: %w", err)
	}
	if er.ErrorMessage != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: server error %v", er.ErrorMessage)
	}

	gc.PlayerId = er.PlayerId
	return nil
}

// sendAndRecv sends a request and blocks until the matching response arrives,
// then unmarshals it into resp.
func sendAndRecv(gc *pkg.GameClient, stats *metrics.Stats, msgId uint32, req, resp proto.Message) error {
	return sendAndRecvTimeout(gc, stats, msgId, req, resp, defaultLoginTimeout)
}

// sendAndRecvTimeout is like sendAndRecv but with an explicit timeout.
// If the response is not received within the timeout, it returns an error
// indicating the login is stuck.
func sendAndRecvTimeout(gc *pkg.GameClient, stats *metrics.Stats, msgId uint32, req, resp proto.Message, timeout time.Duration) error {
	if err := gc.SendRequest(msgId, req); err != nil {
		return err
	}
	stats.MsgSent()

	type recvResult struct {
		msg *base.MessageContent
		err error
	}
	ch := make(chan recvResult, 1)

	go func() {
		for {
			raw, err := gc.RecvOne()
			if err != nil {
				ch <- recvResult{err: err}
				return
			}
			stats.MsgRecv()
			if raw.MessageId == msgId {
				ch <- recvResult{msg: raw}
				return
			}
			// SceneManager 路由与 EnterGameResponse 是并发链路，Scene 的
			// NotifyEnterScene 可能先到。暂存非目标消息，待 Player 注册并
			// 启动 RecvLoop 后补投递，不能在这里静默吞掉。
			gc.DeferMessage(raw)
			zap.L().Debug("deferring msg during login",
				zap.Uint32("got", raw.MessageId),
				zap.Uint32("want", msgId),
			)
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		return proto.Unmarshal(r.msg.SerializedMessage, resp)
	case <-time.After(timeout):
		stats.LoginStuck()
		return fmt.Errorf("STUCK: no response for msg_id=%d within %s", msgId, timeout)
	}
}

// runTokenRefresher proactively refreshes the access_token when it's within
// refreshLead of expiry, and exits cleanly when either `stop` closes (global
// shutdown) or `done` closes (this session's RecvLoop ended).
//
// Two refresh transports are supported:
//
//   - HTTP via Java Gateway POST /api/refresh-token (preferred when
//     gatewayAddr is non-empty). This is the path the boundary doc commits
//     to: refresh has no session affinity, so it does not need to ride the
//     gate TCP channel. Once the server hands back a new pair we write it
//     into gc via SetTokens (same place ClientPlayerLoginRefreshTokenHandler
//     would patch under the legacy path).
//
//   - Legacy gate TCP RPC ClientPlayerLoginRefreshTokenMessageId, kept as
//     fallback for the duration of the deprecation window (see ARCH §12).
//
// Semantics of the counters it touches:
//   - TokenRefreshOK   : refresh round-tripped successfully (new tokens stored).
//   - TokenRefreshFail : transport error or server returned non-zero code.
//
// HTTP mode is synchronous and atomic — we know on return whether the
// rotation happened. The legacy mode is fire-and-forget at this layer; the
// asynchronous rejection path was previously surfaced via
// AccessReconnectFallback on the next reconnect, and that still works
// because we keep the existing branch unchanged when gatewayAddr is empty.
func runTokenRefresher(gc *pkg.GameClient, gatewayAddr string, stats *metrics.Stats, stop, done <-chan struct{}) {
	const (
		checkInterval = 30 * time.Second
		refreshLead   = 10 * time.Minute // refresh when <10min of TTL remains
		sendCooldown  = 60 * time.Second // min gap between successive refreshes
	)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	var lastSent time.Time
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-ticker.C:
			_, refresh, accessExp, _ := gc.SnapshotTokens()
			if refresh == "" || accessExp == 0 {
				continue
			}
			remaining := time.Until(time.Unix(accessExp, 0))
			if remaining > refreshLead {
				continue
			}
			if !lastSent.IsZero() && time.Since(lastSent) < sendCooldown {
				continue
			}
			zap.L().Info("refreshing access_token",
				zap.String("account", gc.Account),
				zap.Duration("remaining", remaining),
				zap.Bool("via_http", gatewayAddr != ""),
			)
			lastSent = time.Now()
			if gatewayAddr != "" {
				rsp, err := httpRefreshToken(gatewayAddr, refresh, 5*time.Second)
				if err != nil {
					stats.TokenRefreshFail()
					zap.L().Warn("http refresh failed", zap.Error(err))
					continue
				}
				if rsp.Code != 0 {
					stats.TokenRefreshFail()
					zap.L().Warn("http refresh rejected",
						zap.Int("code", rsp.Code),
						zap.String("msg", rsp.Message))
					continue
				}
				gc.SetTokens(rsp.AccessToken, rsp.RefreshToken,
					rsp.AccessTokenExpire, rsp.RefreshTokenExpire)
				stats.TokenRefreshOK()
				continue
			}
			// Legacy gate-TCP path (deprecated, see ARCH §12).
			if err := gc.SendRequest(
				game.ClientPlayerLoginRefreshTokenMessageId,
				&login.RefreshTokenRequest{RefreshToken: refresh},
			); err != nil {
				stats.TokenRefreshFail()
				zap.L().Warn("refresh send failed", zap.Error(err))
				continue
			}
			stats.TokenRefreshOK()
		}
	}
}

// sendDisconnectBestEffort tells login that this client is going away
// gracefully so the player_locator session moves from Online to
// Disconnecting (with a 30s reconnect lease) instead of staying stuck at
// Online. This is the lightweight "treat me as TCP-FIN" signal — we do
// not wait for a response and never block the deferred Close path.
//
// The legacy gate still owns the cleanup decision; login.Disconnect is a
// hint, not a kick. If the send itself fails (gate already gone, queue
// full) we silently swallow — the lease will eventually time out anyway.
//
// Architecture-level fix is in cpp gate's TCP-close handler (see ARCH §12
// follow-up); this function exists so robot smoke runs can iterate without
// having to FLUSHDB between rounds.
func sendDisconnectBestEffort(gc *pkg.GameClient) {
	if gc == nil || gc.PlayerId == 0 {
		return
	}
	defer func() { _ = recover() }()
	_ = gc.SendRequest(
		game.ClientPlayerLoginDisconnectMessageId,
		&login.LoginNodeDisconnectRequest{},
	)
}

// fetchSaToken calls the SA-Token dev-login endpoint and returns the token value.
func fetchSaToken(saTokenAddr, account string) (string, error) {
	url := saTokenAddr + "/auth/dev-login?account=" + account
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("SA-Token dev-login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read SA-Token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SA-Token dev-login HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		OK         bool   `json:"ok"`
		TokenValue string `json:"token_value"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode SA-Token response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("SA-Token dev-login failed: %s", result.Message)
	}
	if result.TokenValue == "" {
		return "", fmt.Errorf("SA-Token dev-login returned empty token")
	}
	return result.TokenValue, nil
}

// revokeSaToken calls the SA-Token logout endpoint to remove the manually-
// written Redis key so long-running stress tests don't accumulate stale
// satoken:login:token:* entries. Best-effort; errors are logged but not fatal.
func revokeSaToken(saTokenAddr, token string) {
	if saTokenAddr == "" || token == "" {
		return
	}
	url := saTokenAddr + "/auth/logout?token=" + token
	resp, err := http.Get(url)
	if err != nil {
		zap.L().Debug("SA-Token logout failed", zap.Error(err))
		return
	}
	_ = resp.Body.Close()
}

// loginAndEnterSaToken performs: fetch SA-Token → login (auth_type=satoken) → create player → enter game.
func loginAndEnterSaToken(gc *pkg.GameClient, saTokenAddr string, stats *metrics.Stats) error {
	token, err := fetchSaToken(saTokenAddr, gc.Account)
	if err != nil {
		stats.LoginFail()
		return fmt.Errorf("fetch satoken: %w", err)
	}
	gc.SaToken = token

	var lr login.LoginResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{
			Account:  gc.Account,
			AuthType: "satoken",
			AuthToken: token,
		},
		&lr,
	); err != nil {
		stats.LoginFail()
		return fmt.Errorf("login(satoken): %w", err)
	}
	if lr.ErrorMessage != nil {
		stats.LoginFail()
		return fmt.Errorf("login(satoken): server error %v", lr.ErrorMessage)
	}

	if lr.AccessToken != "" {
		gc.SetTokens(lr.AccessToken, lr.RefreshToken, lr.AccessTokenExpire, lr.RefreshTokenExpire)
	}

	if len(lr.Players) == 0 {
		var cr login.CreatePlayerResponse
		if err := sendAndRecv(gc, stats,
			game.ClientPlayerLoginCreatePlayerMessageId,
			&login.CreatePlayerRequest{},
			&cr,
		); err != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: %w", err)
		}
		if cr.ErrorMessage != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: server error %v", cr.ErrorMessage)
		}
		lr.Players = cr.Players
	}
	if len(lr.Players) == 0 {
		stats.LoginFail()
		return fmt.Errorf("no players after create")
	}

	playerId := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerId},
		&er,
	); err != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: %w", err)
	}
	if er.ErrorMessage != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: server error %v", er.ErrorMessage)
	}

	gc.PlayerId = er.PlayerId
	return nil
}

// loginAndEnterAccessToken performs: login (auth_type=access_token) → create
// player (if needed) → enter game. Used on reconnect to skip the SA-Token /
// password round-trip. The server does NOT rotate tokens for access_token auth,
// so gc.AccessToken stays the same across successful reconnects.
func loginAndEnterAccessToken(gc *pkg.GameClient, accessToken string, stats *metrics.Stats) error {
	var lr login.LoginResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginLoginMessageId,
		&login.LoginRequest{
			Account:   gc.Account,
			AuthType:  "access_token",
			AuthToken: accessToken,
		},
		&lr,
	); err != nil {
		stats.LoginFail()
		return fmt.Errorf("login(access_token): %w", err)
	}
	if lr.ErrorMessage != nil {
		stats.LoginFail()
		return fmt.Errorf("login(access_token): server error %v", lr.ErrorMessage)
	}

	if len(lr.Players) == 0 {
		var cr login.CreatePlayerResponse
		if err := sendAndRecv(gc, stats,
			game.ClientPlayerLoginCreatePlayerMessageId,
			&login.CreatePlayerRequest{},
			&cr,
		); err != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: %w", err)
		}
		if cr.ErrorMessage != nil {
			stats.LoginFail()
			return fmt.Errorf("create player: server error %v", cr.ErrorMessage)
		}
		lr.Players = cr.Players
	}
	if len(lr.Players) == 0 {
		stats.LoginFail()
		return fmt.Errorf("no players after create")
	}

	playerID := lr.Players[0].GetPlayer().GetPlayerId()
	var er login.EnterGameResponse
	if err := sendAndRecv(gc, stats,
		game.ClientPlayerLoginEnterGameMessageId,
		&login.EnterGameRequest{PlayerId: playerID},
		&er,
	); err != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: %w", err)
	}
	if er.ErrorMessage != nil {
		stats.EnterFail()
		return fmt.Errorf("enter game: server error %v", er.ErrorMessage)
	}

	gc.PlayerId = er.PlayerId
	return nil
}
