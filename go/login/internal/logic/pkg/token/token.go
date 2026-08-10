// Package token manages access/refresh token lifecycle in Redis.
//
// Token architecture (WeChat/QQ game login style):
//
//	Access Token:  opaque, 2h TTL, used for reconnect without re-auth from third-party.
//	Refresh Token: opaque, 30d TTL, used to obtain new access token after expiry.
//
// Redis keys:
//
//	access_token:{token}   → JSON{account, auth_type, device_id, created_at}   TTL=AccessTokenTTL
//	refresh_token:{token}  → JSON{account, auth_type, device_id, created_at}   TTL=RefreshTokenTTL
//	account_refresh:{account} → ZSET of active refresh tokens, score = refresh 过期 unix 秒
//	   (member 与其 refresh_token:{token} 键同生共死:键按 TTL 过期的同一刻,score 也越过
//	    now,可被 ZREMRANGEBYSCORE 一并清除。旧版是无 score 的 SET —— 成员不随对应键
//	    过期而消失,死成员无上界累积成 Redis 内存泄漏,客户端反复 Login 即可触发。)
package token

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	accessTokenPrefix  = "access_token:"
	refreshTokenPrefix = "refresh_token:"
	accountRefreshKey  = "account_refresh:"

	tokenByteLen = 32 // 256-bit random, base64url encoded → 43 chars

	// maxRefreshTokensPerAccount 封顶单账号活跃 refresh token 数。死成员由
	// ZREMRANGEBYSCORE 按过期 score 清,这个封顶再挡住"客户端高频 Login 在 30d
	// 窗口内把**活**成员刷到任意大"的病态情况:超出即淘汰最旧的,并删其 token 键。
	maxRefreshTokensPerAccount = 32
)

// Config holds token TTL settings.
type Config struct {
	AccessTokenTTL  time.Duration // e.g. 2h
	RefreshTokenTTL time.Duration // e.g. 30d (720h)
}

// TokenData is the payload stored in Redis for each token.
type TokenData struct {
	Account   string `json:"account"`
	AuthType  string `json:"auth_type"`
	DeviceID  string `json:"device_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// TokenPair is returned after issuing or refreshing tokens.
type TokenPair struct {
	AccessToken        string
	RefreshToken       string
	AccessTokenExpire  int64 // unix seconds
	RefreshTokenExpire int64 // unix seconds
}

// Manager handles token CRUD operations.
type Manager struct {
	rdb *redis.Client
	cfg Config
}

// NewManager creates a token manager.
func NewManager(rdb *redis.Client, cfg Config) *Manager {
	return &Manager{rdb: rdb, cfg: cfg}
}

// Issue generates a new access+refresh token pair and stores them in Redis.
func (m *Manager) Issue(ctx context.Context, account, authType, deviceID string) (*TokenPair, error) {
	now := time.Now()
	data := &TokenData{
		Account:   account,
		AuthType:  authType,
		DeviceID:  deviceID,
		CreatedAt: now.Unix(),
	}

	accessToken, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	refreshToken, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal token data: %w", err)
	}

	accessExpire := now.Add(m.cfg.AccessTokenTTL)
	refreshExpire := now.Add(m.cfg.RefreshTokenTTL)

	setKey := accountRefreshKey + account
	pipe := m.rdb.TxPipeline()
	pipe.Set(ctx, accessTokenPrefix+accessToken, dataBytes, m.cfg.AccessTokenTTL)
	pipe.Set(ctx, refreshTokenPrefix+refreshToken, dataBytes, m.cfg.RefreshTokenTTL)
	// 先按 score 清掉已过期的死成员,再加入新成员。score = 该 refresh token 的
	// 过期 unix 秒;score < now 的成员其 refresh_token 键早已 TTL 过期,清成员即可,
	// 无需再删键。这一步把"死成员无上界累积"根治掉。
	pipe.ZRemRangeByScore(ctx, setKey, "-inf", fmt.Sprintf("%d", now.Unix()-1))
	pipe.ZAdd(ctx, setKey, redis.Z{Score: float64(refreshExpire.Unix()), Member: refreshToken})
	pipe.Expire(ctx, setKey, m.cfg.RefreshTokenTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("store tokens in Redis: %w", err)
	}

	// 封顶活跃成员数(best-effort,失败不阻断签发):超出即淘汰最旧的若干枚,
	// 并连带删除它们的 refresh_token 键,让集合与 token 键都保持有界。
	m.trimRefreshSet(ctx, setKey)

	logx.Infof("[Token] Issued tokens for account=%s auth_type=%s access_ttl=%v refresh_ttl=%v",
		account, authType, m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)

	return &TokenPair{
		AccessToken:        accessToken,
		RefreshToken:       refreshToken,
		AccessTokenExpire:  accessExpire.Unix(),
		RefreshTokenExpire: refreshExpire.Unix(),
	}, nil
}

// ValidateAccess validates an access token, returns the account if valid.
func (m *Manager) ValidateAccess(ctx context.Context, accessToken string) (*TokenData, error) {
	return m.validate(ctx, accessTokenPrefix+accessToken)
}

// Refresh validates a refresh token, invalidates it, and issues a new token pair (rotation).
func (m *Manager) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	key := refreshTokenPrefix + refreshToken
	data, err := m.validate(ctx, key)
	if err != nil {
		return nil, err
	}

	// Atomically delete old refresh token (one-time use)
	deleted, err := m.rdb.Del(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("delete old refresh token: %w", err)
	}
	if deleted == 0 {
		// Race: another request already consumed this token
		return nil, fmt.Errorf("refresh token already consumed")
	}

	// Remove from account set (ZSET now — 见文件头 key 说明)
	m.rdb.ZRem(ctx, accountRefreshKey+data.Account, refreshToken)

	// Issue new pair
	return m.Issue(ctx, data.Account, data.AuthType, data.DeviceID)
}

// trimRefreshSet 把 account_refresh ZSET 封顶到 maxRefreshTokensPerAccount 枚活跃成员:
// 超出则淘汰 score 最小(最旧)的若干枚,并删除它们的 refresh_token 键。best-effort。
func (m *Manager) trimRefreshSet(ctx context.Context, setKey string) {
	n, err := m.rdb.ZCard(ctx, setKey).Result()
	if err != nil || n <= maxRefreshTokensPerAccount {
		return
	}
	// 最旧的 (n - max) 枚:ZSET 按 score 升序,rank [0, n-max-1] 即最旧那批。
	overflow, err := m.rdb.ZRange(ctx, setKey, 0, n-maxRefreshTokensPerAccount-1).Result()
	if err != nil || len(overflow) == 0 {
		return
	}
	delKeys := make([]string, 0, len(overflow))
	members := make([]interface{}, 0, len(overflow))
	for _, t := range overflow {
		delKeys = append(delKeys, refreshTokenPrefix+t)
		members = append(members, t)
	}
	pipe := m.rdb.TxPipeline()
	pipe.Del(ctx, delKeys...)
	pipe.ZRem(ctx, setKey, members...)
	if _, err := pipe.Exec(ctx); err != nil {
		logx.Errorf("[Token] trim refresh set %s failed: %v", setKey, err)
	}
}

// RevokeAll revokes all refresh tokens for an account (e.g. password change).
func (m *Manager) RevokeAll(ctx context.Context, account string) error {
	setKey := accountRefreshKey + account
	tokens, err := m.rdb.ZRange(ctx, setKey, 0, -1).Result()
	if err != nil {
		return fmt.Errorf("list refresh tokens: %w", err)
	}

	if len(tokens) > 0 {
		keys := make([]string, 0, len(tokens)+1)
		for _, t := range tokens {
			keys = append(keys, refreshTokenPrefix+t)
		}
		keys = append(keys, setKey)
		m.rdb.Del(ctx, keys...)
	}

	logx.Infof("[Token] Revoked all refresh tokens for account=%s count=%d", account, len(tokens))
	return nil
}

func (m *Manager) validate(ctx context.Context, key string) (*TokenData, error) {
	val, err := m.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("token not found or expired")
	}
	if err != nil {
		return nil, fmt.Errorf("Redis lookup: %w", err)
	}

	var data TokenData
	if err := json.Unmarshal(val, &data); err != nil {
		return nil, fmt.Errorf("unmarshal token data: %w", err)
	}
	return &data, nil
}

func generateToken() (string, error) {
	b := make([]byte, tokenByteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
