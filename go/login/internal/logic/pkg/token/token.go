// Package token manages access/refresh token lifecycle in Redis.
//
// Token architecture (WeChat/QQ game login style):
//
//	Access Token:  opaque, 2h TTL, used for reconnect without re-auth from third-party.
//	Refresh Token: opaque, 30d TTL, used to obtain new access token after expiry.
//
// 令牌字符串是 <AccountTag>.<secret>:tag 决定令牌的键落在哪个槽,secret 是 256 位随机数。对客户端是不透明串,客户端不解析。
// 校验时先取 tag 再拼键,格式不对按无效令牌,不访问 Redis。
//
// Redis keys(登录域,键形在 shared/redisslot/login_keys.go,一个账号的键全部同槽):
//
//	access_token:{<tag>}:<secret>     → JSON{account, auth_type, device_id, created_at}   TTL=AccessTokenTTL
//	refresh_token:{<tag>}:<secret>    → JSON{account, auth_type, device_id, created_at}   TTL=RefreshTokenTTL
//	refresh_family:{<tag>}:<id>       → HASH{account, head, created_at, rotated_at}       TTL=RefreshTokenTTL
//	account_refresh:{<tag>}:<account> → ZSET of active refresh secrets, score = refresh 过期 unix 秒
//	   (member 与其 refresh_token 键同生共死:键按 TTL 过期的同一刻,score 也越过
//	    now,可被 ZREMRANGEBYSCORE 一并清除。旧版是无 score 的 SET —— 成员不随对应键
//	    过期而消失,死成员无上界累积成 Redis 内存泄漏,客户端反复 Login 即可触发。)
//
// 索引成员与家族 head 存的是 secret,不是完整令牌:脚本按「前缀 + secret」现拼 refresh 键,前缀带账号 tag,
// 现拼出来的键与脚本声明的键同槽。
package token

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	tokenSeparator = "."

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
	FamilyID  string `json:"family_id,omitempty"`
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
	rdb redis.UniversalClient
	cfg Config
}

// NewManager creates a token manager. rdb 是登录域句柄。
func NewManager(rdb redis.UniversalClient, cfg Config) *Manager {
	return &Manager{rdb: rdb, cfg: cfg}
}

// errMalformedToken:令牌字符串不是 <AccountTag>.<secret>。没有访问 Redis,与「令牌不存在」对调用方是同一种无效令牌。
var errMalformedToken = errors.New("token malformed")

// formatToken 拼出发给客户端的令牌字符串。
func formatToken(tag, secret string) string {
	return tag + tokenSeparator + secret
}

// parseToken 拆开令牌字符串。令牌是客户端输入,tag 与 secret 都要校验过才能进键名。
func parseToken(raw string) (tag, secret string, ok bool) {
	tag, secret, found := strings.Cut(raw, tokenSeparator)
	if !found || !redisslot.ValidAccountTag(tag) || !validOpaque(secret, tokenByteLen) {
		return "", "", false
	}
	return tag, secret, true
}

// Issue generates a new access+refresh token pair and stores them in Redis.
func (m *Manager) Issue(ctx context.Context, account, authType, deviceID string) (*TokenPair, error) {
	if m.cfg.AccessTokenTTL.Milliseconds() <= 0 || m.cfg.RefreshTokenTTL.Milliseconds() <= 0 {
		return nil, fmt.Errorf("token TTL 必须至少为 1ms")
	}
	if account == "" || authType == "" {
		return nil, fmt.Errorf("token 账号与认证类型不能为空")
	}
	now := time.Now()
	familyID, err := generateOpaque(familyByteLen)
	if err != nil {
		return nil, fmt.Errorf("生成 token family: %w", err)
	}
	accessSecret, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	refreshSecret, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	dataBytes, err := json.Marshal(&TokenData{Account: account, AuthType: authType, DeviceID: deviceID, CreatedAt: now.Unix(), FamilyID: familyID})
	if err != nil {
		return nil, fmt.Errorf("marshal token data: %w", err)
	}
	accessExpire := now.Add(m.cfg.AccessTokenTTL)
	refreshExpire := now.Add(m.cfg.RefreshTokenTTL)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tag := redisslot.AccountTag(account)
	if _, err := issueFamilyScript.Run(ctx, m.rdb,
		[]string{redisslot.AccessTokenKey(tag, accessSecret), redisslot.RefreshTokenKey(tag, refreshSecret), redisslot.AccountRefreshKey(account), redisslot.RefreshFamilyKey(tag, familyID)},
		dataBytes, account, now.Unix(), m.cfg.AccessTokenTTL.Milliseconds(), m.cfg.RefreshTokenTTL.Milliseconds(), now.Unix()-1, refreshExpire.Unix(), refreshSecret,
	).Int64(); err != nil {
		return nil, fmt.Errorf("store token family: %w", err)
	}
	m.trimRefreshSet(ctx, account)
	logx.Infof("[Token] Issued tokens for account=%s auth_type=%s access_ttl=%v refresh_ttl=%v", account, authType, m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)
	return &TokenPair{AccessToken: formatToken(tag, accessSecret), RefreshToken: formatToken(tag, refreshSecret), AccessTokenExpire: accessExpire.Unix(), RefreshTokenExpire: refreshExpire.Unix()}, nil
}

// ValidateAccess validates an access token, returns the account if valid.
func (m *Manager) ValidateAccess(ctx context.Context, accessToken string) (*TokenData, error) {
	tag, secret, ok := parseToken(accessToken)
	if !ok {
		return nil, errMalformedToken
	}
	return m.validate(ctx, tag, redisslot.AccessTokenKey(tag, secret))
}

// family.go 中的轮换脚本同时提交旧值 CAS、family head、两枚 token 与账号索引。
// rotateRefreshScript 的返回值。
const (
	refreshRotated  = 1
	refreshConsumed = 0
)

// Refresh validates a refresh token, invalidates it, and issues a new token pair (rotation).
//
// 作废与签发是同一次原子提交(rotateRefreshScript):要么旧的仍有效、新的没签,要么旧的作废、新的已签。
// 提交前先查 ctx:请求已截止 / 已取消就不发脚本(截止之后 gate 已放行同一会话的下一条请求,
// 见 client-request-ordering.md §6.1)。已知残留(不在本函数解决):脚本已在 Redis 执行、回包却丢了时,
// 新的一对签出了但客户端拿不到,旧的已作废,重试必然失败(同文 §7.2 第 5 条,是重试问题,不是顺序问题)。
func (m *Manager) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	// 非正 TTL 会让 SET PX / PEXPIRE 报错或签出永不过期的 token,提交前显式拒绝(fail-closed)。
	if m.cfg.AccessTokenTTL.Milliseconds() <= 0 || m.cfg.RefreshTokenTTL.Milliseconds() <= 0 {
		return nil, fmt.Errorf("token TTL must be positive (access=%v refresh=%v)", m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)
	}
	tag, secret, ok := parseToken(refreshToken)
	if !ok {
		return nil, errMalformedToken
	}
	key := redisslot.RefreshTokenKey(tag, secret)
	raw, data, err := m.load(ctx, tag, key)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newAccess, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}
	newRefresh, err := generateSecret()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}
	familyID := data.FamilyID
	legacy := 0
	if familyID == "" {
		familyID, err = generateOpaque(familyByteLen)
		if err != nil {
			return nil, fmt.Errorf("生成旧 refresh 的 family: %w", err)
		}
		legacy = 1
	}
	payload, err := json.Marshal(&TokenData{
		Account:   data.Account,
		AuthType:  data.AuthType,
		DeviceID:  data.DeviceID,
		CreatedAt: now.Unix(),
		FamilyID:  familyID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal token data: %w", err)
	}
	accessExpire := now.Add(m.cfg.AccessTokenTTL)
	refreshExpire := now.Add(m.cfg.RefreshTokenTTL)
	setKey := redisslot.AccountRefreshKey(data.Account)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("request done before rotating refresh token, nothing written: %w", err)
	}
	res, err := rotateRefreshScript.Run(ctx, m.rdb,
		[]string{key, setKey, redisslot.AccessTokenKey(tag, newAccess), redisslot.RefreshTokenKey(tag, newRefresh), redisslot.RefreshFamilyKey(tag, familyID)},
		raw, secret, payload,
		m.cfg.AccessTokenTTL.Milliseconds(), m.cfg.RefreshTokenTTL.Milliseconds(),
		now.Unix()-1, refreshExpire.Unix(), newRefresh, data.Account, legacy, now.Unix(),
	).Int64()
	if err != nil {
		return nil, fmt.Errorf("rotate refresh token: %w", err)
	}
	switch res {
	case refreshRotated:
	case refreshConsumed:
		// 并发的另一次刷新先消费了它,或在 GET 与脚本之间过期。
		return nil, fmt.Errorf("refresh token already consumed")
	default:
		return nil, fmt.Errorf("unexpected rotateRefreshScript result %d", res)
	}

	// 提交之后的封顶清理,与 Issue 相同是 best-effort:请求恰好此刻截止时这一次不清,集合最多暂时多出
	// 一枚活成员,下一次签发会清掉。
	m.trimRefreshSet(ctx, data.Account)

	logx.Infof("[Token] Rotated refresh token for account=%s auth_type=%s access_ttl=%v refresh_ttl=%v",
		data.Account, data.AuthType, m.cfg.AccessTokenTTL, m.cfg.RefreshTokenTTL)

	return &TokenPair{
		AccessToken:        formatToken(tag, newAccess),
		RefreshToken:       formatToken(tag, newRefresh),
		AccessTokenExpire:  accessExpire.Unix(),
		RefreshTokenExpire: refreshExpire.Unix(),
	}, nil
}

// trimRefreshSet 在一个 Lua 提交内淘汰最旧成员及其尚未轮换的 family。
// 与原合同相同，提交后的容量整理为 best-effort；错误不伪装成整理成功。
func (m *Manager) trimRefreshSet(ctx context.Context, account string) {
	if _, err := m.retire(ctx, account, maxRefreshTokensPerAccount); err != nil {
		logx.Errorf("[Token] trim refresh families failed: %v", err)
	}
}

// RevokeAll 原子撤销本账号当前 refresh 及对应 family，使已有 family access 同步失效。
// 并发的新认证只能线性化在撤销之前或之后，不会因旧索引的分步删除误伤新 family。
func (m *Manager) RevokeAll(ctx context.Context, account string) error {
	count, err := m.retire(ctx, account, 0)
	if err != nil {
		return fmt.Errorf("revoke refresh families: %w", err)
	}
	logx.Infof("[Token] Revoked refresh families for account=%s count=%d", account, count)
	return nil
}

// retire 淘汰账号索引里超出 keep 的最旧成员。脚本现拼的 refresh / family 键都带账号 tag,与索引键同槽。
func (m *Manager) retire(ctx context.Context, account string, keep int) (int64, error) {
	tag := redisslot.AccountTag(account)
	return retireRefreshScript.Run(ctx, m.rdb, []string{redisslot.AccountRefreshKey(account)},
		account, keep, redisslot.RefreshTokenKeyPrefix(tag), redisslot.RefreshFamilyKeyPrefix(tag)).Int64()
}

func (m *Manager) validate(ctx context.Context, tag, key string) (*TokenData, error) {
	raw, data, err := m.load(ctx, tag, key)
	if err != nil {
		return nil, err
	}
	if data.FamilyID == "" {
		return data, nil
	} // 旧 access 按原 TTL 兼容，不伪造 family。
	valid, err := validateFamilyAccessScript.Run(ctx, m.rdb, []string{key, redisslot.RefreshFamilyKey(tag, data.FamilyID), redisslot.AccountRefreshKey(data.Account)}, raw, data.Account, data.FamilyID, redisslot.RefreshTokenKeyPrefix(tag)).Int64()
	if err != nil {
		return nil, fmt.Errorf("validate token family: %w", err)
	}
	if valid != 1 {
		return nil, fmt.Errorf("token family 已失效或不匹配")
	}
	return data, nil
}

// load 读出 token 键的原始字节并解析。Refresh 要原始字节交给轮换脚本做按值比对(见 rotateRefreshScript)。
// tag 是令牌字符串里的 tag,必须等于 payload 账号的 AccountTag:账号索引键按账号算槽,家族键按 tag 算槽,
// 两者不一致的载荷进不了同一段脚本。
func (m *Manager) load(ctx context.Context, tag, key string) ([]byte, *TokenData, error) {
	val, err := m.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil, fmt.Errorf("token not found or expired")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("Redis lookup: %w", err)
	}

	var data TokenData
	if err := json.Unmarshal(val, &data); err != nil {
		return nil, nil, fmt.Errorf("unmarshal token data: %w", err)
	}
	if data.Account == "" || data.AuthType == "" || data.CreatedAt <= 0 || (data.FamilyID != "" && !validFamilyID(data.FamilyID)) {
		return nil, nil, fmt.Errorf("token payload 身份字段损坏")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(val, &fields); err != nil {
		return nil, nil, fmt.Errorf("token payload 字段损坏: %w", err)
	}
	if _, exists := fields["family_id"]; exists && !validFamilyID(data.FamilyID) {
		return nil, nil, fmt.Errorf("显式 family 字段必须为真实规范 ID")
	}
	if redisslot.AccountTag(data.Account) != tag {
		return nil, nil, fmt.Errorf("token tag 与账号不匹配")
	}
	return val, &data, nil
}

func generateSecret() (string, error) { return generateOpaque(tokenByteLen) }

func generateOpaque(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
