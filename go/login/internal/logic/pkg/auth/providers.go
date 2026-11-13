package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
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

// maxOAuthResponseBytes 是渠道应答体的上限。微信 / QQ 的应答是几百字节的 JSON;不设上限时,
// 一个异常或被劫持的上游可以让每个登录请求都读进任意大的应答(与 Java 版 OAuthHttp.MAX_BODY_BYTES 同值)。
const maxOAuthResponseBytes = 64 << 10

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
		return nil, fmt.Errorf("sa-token not found or expired")
	}
	if err != nil {
		return nil, fmt.Errorf("sa-token Redis lookup failed: %w", err)
	}
	if loginId == "" {
		return nil, fmt.Errorf("sa-token maps to empty loginId")
	}
	logx.Infof("SA-Token validated: account=%s", loginId)
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
// Account key uses unionid (so the same player keeps the same account across
// multiple apps under the same WeChat Open Platform subject). Without
// unionid the login is refused with ErrProviderUnavailable unless
// AllowOpenIdFallback is set: openid differs per app, so falling back
// silently splits one player into several accounts that cannot be merged.
type WeChatProvider struct {
	AppId     string
	AppSecret string
	HTTP      *http.Client // optional, injected for tests; nil -> default 5s client

	// AllowOpenIdFallback 为 true 时拿不到 unionid 就用 openid 建账号(配置 RequireUnionId=false)。
	// 零值 = 不回落,缺 unionid 即失败(fail-closed)。
	AllowOpenIdFallback bool

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
		return nil, fmt.Errorf("wechat: provider not configured (missing AppId/AppSecret): %w", ErrProviderUnavailable)
	}

	endpoint := wechatBaseURL(p.Endpoint) + "/sns/oauth2/access_token?" + url.Values{
		"appid":      {p.AppId},
		"secret":     {p.AppSecret},
		"code":       {code},
		"grant_type": {"authorization_code"},
	}.Encode()

	body, err := oauthGet(ctx, p.HTTP, "wechat", endpoint)
	if err != nil {
		return nil, err
	}
	var r wechatTokenResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("wechat: bad json: %w", ErrProviderUnavailable)
	}
	if r.ErrCode != 0 {
		if wechatErrCodeIsProviderSide(r.ErrCode) {
			return nil, fmt.Errorf("wechat: errcode=%d: %w", r.ErrCode, ErrProviderUnavailable)
		}
		return nil, fmt.Errorf("wechat: errcode=%d", r.ErrCode)
	}
	if r.OpenId == "" {
		// errcode=0 却没有 openid:应答不合约,不是玩家凭证的问题。
		return nil, fmt.Errorf("wechat: empty openid: %w", ErrProviderUnavailable)
	}

	id := r.UnionId
	if id == "" {
		if !p.AllowOpenIdFallback {
			return nil, fmt.Errorf("wechat: unionid missing (RequireUnionId; bind the app to an Open Platform subject): %w",
				ErrProviderUnavailable)
		}
		id = r.OpenId
	}
	account := "wx_" + id
	logx.Infof("WeChat auth ok: account=%s openid=%s union=%v", account, maskOAuthId(r.OpenId), r.UnionId != "")
	return &AuthResult{Account: account}, nil
}

// wechatErrCodeIsProviderSide 判断 errcode 指向的是本服务的渠道配置或微信自身状态,而不是玩家凭证:
// -1 系统繁忙;40001 / 40125 AppSecret 无效;40013 AppId 无效;41002 / 41004 缺 AppId / AppSecret;
// 40164 出口 IP 不在白名单;45009 / 45011 接口调用量超限。
//
// 其余 errcode(40029 code 无效、40163 code 已被使用、42003 code 超时等)按凭证无效处理。拿不准的一律
// 不算故障,与 docs/design/tip-code-axis.md §2.9 的判定原则一致:把玩家侧拒绝误标成故障会刷满屏假告警。
func wechatErrCodeIsProviderSide(code int) bool {
	switch code {
	case -1, 40001, 40013, 40125, 40164, 41002, 41004, 45009, 45011:
		return true
	}
	return false
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
// the token. ClientId in the response must equal our AppId. unionid policy
// is the same as WeChatProvider (AllowOpenIdFallback).
type QQProvider struct {
	AppId  string
	AppKey string       // Reserved for future server-side WebOAuth code->token exchange; unused here.
	HTTP   *http.Client // optional, injected for tests

	// AllowOpenIdFallback 语义同 WeChatProvider.AllowOpenIdFallback;零值 = 缺 unionid 即失败。
	AllowOpenIdFallback bool
	// DisableUnionIdRequest 为 true 时不带 unionid=1(配置 RequestUnionId=false,应用没有 unionid 权限时用);
	// 零值 = 照旧请求 unionid。
	DisableUnionIdRequest bool

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
		return nil, fmt.Errorf("qq: provider not configured (missing AppId): %w", ErrProviderUnavailable)
	}

	query := url.Values{
		"access_token": {accessToken},
		"fmt":          {"json"},
	}
	if !p.DisableUnionIdRequest {
		query.Set("unionid", "1")
	}
	endpoint := qqBaseURL(p.Endpoint) + "/oauth2.0/me?" + query.Encode()

	raw, err := oauthGet(ctx, p.HTTP, "qq", endpoint)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(raw))
	if m := qqJsonpRe.FindStringSubmatch(body); len(m) == 2 {
		body = m[1]
	}

	var r qqMeResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return nil, fmt.Errorf("qq: bad json: %w", ErrProviderUnavailable)
	}
	if r.Error != 0 {
		// QQ 的 error 码没有可靠的「哪些是应用侧故障」清单,拿不准一律按凭证无效(tip-code-axis.md §2.9)。
		return nil, fmt.Errorf("qq: error=%d", r.Error)
	}
	if r.OpenId == "" {
		return nil, fmt.Errorf("qq: empty openid: %w", ErrProviderUnavailable)
	}
	if r.ClientId != p.AppId {
		// token 是发给别的应用的:对本服务而言就是无效凭证,不是渠道故障。
		return nil, fmt.Errorf("qq: client_id mismatch")
	}

	id := r.UnionId
	if id == "" {
		if !p.AllowOpenIdFallback {
			return nil, fmt.Errorf("qq: unionid missing (RequireUnionId; apply for unionid permission or set RequestUnionId): %w",
				ErrProviderUnavailable)
		}
		id = r.OpenId
	}
	account := "qq_" + id
	logx.Infof("QQ auth ok: account=%s openid=%s union=%v", account, maskOAuthId(r.OpenId), r.UnionId != "")
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

// OAuth 的 URL 含凭证，net/http 的错误会携带完整 URL；第三方响应也可能回显凭证。
// 仅保留固定阶段和可识别的取消/超时，不把原错误链带到登录调用者的日志中。
func oauthRequestError(provider, stage string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %s: %w", provider, stage, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %s: %w", provider, stage, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s: %s", provider, stage)
}

// oauthGet 发一次渠道 GET 并读回应答体。这里的失败全部标 ErrProviderUnavailable:还没轮到解析玩家凭证。
// 错误文案只带固定阶段(oauthRequestError)与状态码,不带 URL —— 查询串里有 code / access_token / secret。
func oauthGet(ctx context.Context, client *http.Client, provider, rawURL string) ([]byte, error) {
	return oauthGetWithHeader(ctx, client, provider, rawURL, nil)
}

// oauthGetWithHeader 同 oauthGet,另带请求头(抖音小游戏的 code2Session 要求 Content-Type: application/json)。
// 状态码检查、应答限长与错误脱敏只有这一份实现;header 为 nil 时就是 oauthGet。
func oauthGetWithHeader(ctx context.Context, client *http.Client, provider, rawURL string, header http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, providerFault(oauthRequestError(provider, "build request", err))
	}
	if header != nil {
		req.Header = header.Clone()
	}
	resp, err := httpClientOrDefault(client).Do(req)
	if err != nil {
		return nil, providerFault(oauthRequestError(provider, "http error", err))
	}
	defer resp.Body.Close()

	// 渠道的业务拒绝(code 无效等)以 200 + JSON 错误码返回;非 2xx 只可能是网关 / 限流 / 渠道自身故障,
	// 应答体也不是认识的 JSON,不读不解析。
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s: http status %d: %w", provider, resp.StatusCode, ErrProviderUnavailable)
	}
	// 多读 1 字节,用来区分「恰好 64 KiB」和「超过上限被截断」。
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return nil, providerFault(oauthRequestError(provider, "read body", err))
	}
	if len(body) > maxOAuthResponseBytes {
		return nil, fmt.Errorf("%s: response exceeds %d bytes: %w", provider, maxOAuthResponseBytes, ErrProviderUnavailable)
	}
	return body, nil
}

// providerFault 给已经脱敏的阶段错误再挂上 ErrProviderUnavailable,保留原有的取消 / 超时语义。
func providerFault(err error) error {
	return fmt.Errorf("%w: %w", err, ErrProviderUnavailable)
}

// maskOAuthId 只留首 4 尾 2 个字符:排障时够对上号,又不在每条成功日志里整段落 openid。
func maskOAuthId(id string) string {
	if len(id) <= 8 {
		return "***"
	}
	return id[:4] + "***" + id[len(id)-2:]
}

// wechatBaseURL returns the production WeChat OAuth base URL when
// override is empty, otherwise the override. Sandbox-mock pointing
// goes through this exact lever — see go/login/cmd/sandbox_mock/main.go.
//
// The override is validated at startup by config.ResolveSecrets
// (config/auth_providers.go checkOAuthEndpoint): https only outside
// Mode=dev/test, no userinfo / query / fragment.
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
