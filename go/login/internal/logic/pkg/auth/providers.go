package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"login/internal/logic/pkg/token"
)

// defaultHTTPTimeout is used by third-party OAuth Providers when no client is injected.
const defaultHTTPTimeout = 5 * time.Second

func httpClientOrDefault(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

// DevelopmentPasswordProvider 是受控的开发/机器人登录入口，不是生产账号
// 口令实现：所有允许的测试账号共用一个仅由环境变量注入的密钥，并且账号必须
// 命中显式前缀白名单。生产默认不构造它；生产 PasswordAuth 走独立的 MySQL
// Argon2id 实现，也不会把“没有口令记录”的存量账号自动认领给首次调用者。
type DevelopmentPasswordProvider struct {
	sharedSecret           string
	allowedAccountPrefixes []string
}

func NewDevelopmentPasswordProvider(sharedSecret string, allowedAccountPrefixes []string) (*DevelopmentPasswordProvider, error) {
	if sharedSecret == "" {
		return nil, fmt.Errorf("development password auth requires a non-empty shared secret")
	}
	prefixes := make([]string, 0, len(allowedAccountPrefixes))
	for _, prefix := range allowedAccountPrefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			return nil, fmt.Errorf("development password auth contains an empty account prefix")
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("development password auth requires at least one account prefix")
	}
	return &DevelopmentPasswordProvider{
		sharedSecret:           sharedSecret,
		allowedAccountPrefixes: prefixes,
	}, nil
}

func (p *DevelopmentPasswordProvider) ValidatePassword(_ context.Context, account, password string) (*AuthResult, error) {
	if account == "" || account != strings.TrimSpace(account) {
		return nil, ErrInvalidCredentials
	}
	allowed := false
	for _, prefix := range p.allowedAccountPrefixes {
		if strings.HasPrefix(account, prefix) {
			allowed = true
			break
		}
	}
	if !allowed || subtle.ConstantTimeCompare([]byte(password), []byte(p.sharedSecret)) != 1 {
		return nil, ErrInvalidCredentials
	}
	return &AuthResult{Account: account}, nil
}

// AccessTokenProvider validates our own access token via Redis.
// Used for reconnect: client sends access_token instead of third-party credential.
type AccessTokenProvider struct {
	TokenManager *token.Manager
}

func (p *AccessTokenProvider) Validate(ctx context.Context, accessToken string) (*AuthResult, error) {
	data, err := p.TokenManager.ValidateAccess(ctx, accessToken)
	if err != nil {
		return nil, fmt.Errorf("access token invalid: %w", err)
	}
	logx.Infof("Access token validated: account=%s auth_type=%s", data.Account, data.AuthType)
	return &AuthResult{Account: data.Account}, nil
}

// SaTokenProvider validates SA-Token via Redis lookup.
type SaTokenProvider struct {
	RedisClient *redis.Client
	TokenName   string // SA-Token key prefix (e.g. "satoken")
	LoginType   string // SA-Token login type (e.g. "login")
}

func (p *SaTokenProvider) Validate(ctx context.Context, tokenValue string) (*AuthResult, error) {
	key := fmt.Sprintf("%s:%s:token:%s", p.TokenName, p.LoginType, tokenValue)
	loginId, err := p.RedisClient.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, fmt.Errorf("sa-token not found or expired: %s", tokenValue)
	}
	if err != nil {
		return nil, fmt.Errorf("sa-token Redis lookup failed: %w", err)
	}
	if loginId == "" {
		return nil, fmt.Errorf("sa-token maps to empty loginId")
	}
	logx.Infof("SA-Token validated: token=%s -> account=%s", tokenValue, loginId)
	return &AuthResult{Account: loginId}, nil
}

// WeChatProvider validates a WeChat OAuth2 authorization code.
//
// Flow (already done by the client SDK before calling us):
//  1. Client SDK pulls up WeChat auth UI (SendAuth.Req, scope=snsapi_userinfo).
//  2. WeChat returns a short-lived (~5min) `code` to the client.
//  3. Client sends LoginRequest{auth_type:"wechat", auth_token: code}.
//
// We exchange `code` for openid/unionid via the official endpoint:
//
//	GET https://api.weixin.qq.com/sns/oauth2/access_token
//
// Account key uses unionid when available (so the same player keeps the
// same account across multiple apps under the same WeChat Open Platform
// subject), otherwise falls back to openid.
type WeChatProvider struct {
	AppId     string
	AppSecret string
	HTTP      *http.Client // optional, injected for tests; nil -> default 5s client

	// Endpoint overrides the api.weixin.qq.com base URL. Empty means
	// production. Set this to a local sandbox-mock server (see
	// go/login/cmd/sandbox_mock) when the network egress whitelist or
	// real AppId/AppSecret aren't available — the wire format is
	// preserved end-to-end so the only thing that changes is which host
	// the GET lands on.
	Endpoint string
}

type wechatTokenResp struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	OpenId       string `json:"openid"`
	Scope        string `json:"scope"`
	UnionId      string `json:"unionid,omitempty"`
	ErrCode      int    `json:"errcode,omitempty"`
	ErrMsg       string `json:"errmsg,omitempty"`
}

func (p *WeChatProvider) Validate(ctx context.Context, code string) (*AuthResult, error) {
	if code == "" {
		return nil, fmt.Errorf("wechat: empty code")
	}
	if p.AppId == "" || p.AppSecret == "" {
		return nil, fmt.Errorf("wechat: provider not configured (missing AppId/AppSecret)")
	}

	endpoint := wechatBaseURL(p.Endpoint) + "/sns/oauth2/access_token?" + url.Values{
		"appid":      {p.AppId},
		"secret":     {p.AppSecret},
		"code":       {code},
		"grant_type": {"authorization_code"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("wechat: build request: %w", err)
	}
	resp, err := httpClientOrDefault(p.HTTP).Do(req)
	if err != nil {
		return nil, fmt.Errorf("wechat: http error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wechat: read body: %w", err)
	}
	var r wechatTokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("wechat: bad json: %w (body=%s)", err, body)
	}
	if r.ErrCode != 0 {
		return nil, fmt.Errorf("wechat: errcode=%d msg=%s", r.ErrCode, r.ErrMsg)
	}
	if r.OpenId == "" {
		return nil, fmt.Errorf("wechat: empty openid")
	}

	id := r.UnionId
	if id == "" {
		id = r.OpenId
	}
	account := "wx_" + id
	logx.Infof("WeChat auth ok: account=%s openid=%s union=%v", account, r.OpenId, r.UnionId != "")
	return &AuthResult{Account: account}, nil
}

// QQProvider validates a QQ Connect access_token (NOT a code).
//
// Flow (already done by the client SDK before calling us):
//  1. Client SDK pulls up QQ auth UI (Tencent.login).
//  2. QQ returns access_token + openid to the client.
//  3. Client sends LoginRequest{auth_type:"qq", auth_token: access_token}.
//
// We verify the token + extract openid/unionid via:
//
//	GET https://graph.qq.com/oauth2.0/me?access_token=...&unionid=1&fmt=json
//
// We never trust openid sent by the client — we always re-derive it from
// the token. ClientId in the response must equal our AppId.
type QQProvider struct {
	AppId  string
	AppKey string       // Reserved for future server-side WebOAuth code->token exchange; unused here.
	HTTP   *http.Client // optional, injected for tests

	// Endpoint overrides the graph.qq.com base URL. Same purpose as
	// WeChatProvider.Endpoint — point at a sandbox-mock server for
	// network-egress-restricted environments.
	Endpoint string
}

type qqMeResp struct {
	ClientId  string `json:"client_id"`
	OpenId    string `json:"openid"`
	UnionId   string `json:"unionid,omitempty"`
	Error     int    `json:"error,omitempty"`
	ErrorDesc string `json:"error_description,omitempty"`
}

// Old QQ endpoints return JSONP: `callback({...});`. With fmt=json this
// should be plain JSON, but we still strip the wrapper defensively.
var qqJsonpRe = regexp.MustCompile(`(?s)callback\s*\(\s*(\{.*\})\s*\)\s*;?`)

func (p *QQProvider) Validate(ctx context.Context, accessToken string) (*AuthResult, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("qq: empty access_token")
	}
	if p.AppId == "" {
		return nil, fmt.Errorf("qq: provider not configured (missing AppId)")
	}

	endpoint := qqBaseURL(p.Endpoint) + "/oauth2.0/me?" + url.Values{
		"access_token": {accessToken},
		"unionid":      {"1"},
		"fmt":          {"json"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("qq: build request: %w", err)
	}
	resp, err := httpClientOrDefault(p.HTTP).Do(req)
	if err != nil {
		return nil, fmt.Errorf("qq: http error: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("qq: read body: %w", err)
	}
	body := strings.TrimSpace(string(raw))
	if m := qqJsonpRe.FindStringSubmatch(body); len(m) == 2 {
		body = m[1]
	}

	var r qqMeResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Errorf("qq: bad json: %w (body=%s)", err, body)
	}
	if r.Error != 0 {
		return nil, fmt.Errorf("qq: error=%d desc=%s", r.Error, r.ErrorDesc)
	}
	if r.OpenId == "" {
		return nil, fmt.Errorf("qq: empty openid")
	}
	if r.ClientId != p.AppId {
		return nil, fmt.Errorf("qq: client_id mismatch: got=%s want=%s", r.ClientId, p.AppId)
	}

	id := r.UnionId
	if id == "" {
		id = r.OpenId
	}
	account := "qq_" + id
	logx.Infof("QQ auth ok: account=%s openid=%s union=%v", account, r.OpenId, r.UnionId != "")
	return &AuthResult{Account: account}, nil
}

// NeteaseProvider validates NetEase login token.
// TODO: implement when integrating NetEase login.
type NeteaseProvider struct {
	AppKey    string
	AppSecret string
}

func (p *NeteaseProvider) Validate(_ context.Context, token string) (*AuthResult, error) {
	// TODO: call NetEase auth API to verify token
	return nil, fmt.Errorf("netease auth not implemented")
}

// wechatBaseURL returns the production WeChat OAuth base URL when
// override is empty, otherwise the override (which the caller is
// responsible for trimming and validating). Sandbox-mock pointing
// goes through this exact lever — see go/login/cmd/sandbox_mock/main.go.
//
// We deliberately do NOT validate the URL: the override is sourced from
// trusted ops yaml, not user input, and even a malformed value will
// fail loudly at the first http.NewRequestWithContext rather than
// silently corrupting wire shape.
func wechatBaseURL(override string) string {
	if override == "" {
		return "https://api.weixin.qq.com"
	}
	return strings.TrimRight(override, "/")
}

// qqBaseURL is the QQ counterpart to wechatBaseURL.
func qqBaseURL(override string) string {
	if override == "" {
		return "https://graph.qq.com"
	}
	return strings.TrimRight(override, "/")
}
