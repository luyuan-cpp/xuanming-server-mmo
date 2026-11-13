package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
)

// 小游戏登录(微信 wx.login / 抖音 tt.login)。
//
// 与 providers.go 里 App 形态的 WeChatProvider 相比,不同的只有渠道接口:小游戏的 code 只能用小游戏自己的
// AppId / AppSecret 走 code2Session 换身份,发到 App 的 sns/oauth2/access_token 换不出来,反过来也一样。
// 所以两种形态各是一个 auth_type、各配各的 AppId;账号名都用 unionid,同一主体下同一个人从 App 和小游戏
// 进来是同一个账号。
//
// code2Session 的应答里还有 session_key(小游戏端做数据签名 / 解密用)。登录用不到它,下面的应答结构体
// 故意不声明这个字段:不解析就不会进内存里的结构体,也就不可能被日志或错误文案带出去。支付要用时由支付
// 服务拿新的 code 现取现弃(docs/design/payment-recharge.md §6)。

// 小游戏渠道的 auth_type。客户端在 LoginRequest.auth_type 里送这两个值,它们也是 login_auth_path_total 的
// auth_type 标签取值,上线后不能改名。
const (
	WeChatMiniGameAuthType = "wechat_minigame"
	DouyinMiniGameAuthType = "douyin_minigame"
)

// WeChatMiniGameProvider 校验微信小游戏的登录凭证。
//
// 小游戏端调 wx.login() 拿到一次性 code(约 5 分钟有效),客户端发
// LoginRequest{auth_type:"wechat_minigame", auth_token: code},服务端拿它调 code2Session:
//
//	GET https://api.weixin.qq.com/sns/jscode2session?appid=..&secret=..&js_code=..&grant_type=authorization_code
//
// 账号名与 WeChatProvider 相同("wx_" + unionid)。小游戏要先绑定到微信开放平台账号才会下发 unionid;
// 拿不到时与 App 形态同一口径:缺省拒绝并标 ErrProviderUnavailable,AllowOpenIdFallback 才回落 openid。
type WeChatMiniGameProvider struct {
	AppId     string
	AppSecret string
	HTTP      *http.Client // 可选,测试注入;nil 用默认 5s 客户端

	// AllowOpenIdFallback 语义同 WeChatProvider.AllowOpenIdFallback;零值 = 缺 unionid 即失败。
	AllowOpenIdFallback bool

	// Endpoint 覆盖 api.weixin.qq.com,指向沙箱 mock(go/login/cmd/sandbox_mock);空 = 生产地址。
	Endpoint string
}

// wechatSessionResp 是 code2Session 的应答里登录要用的字段。session_key 故意不声明,见文件头;
// errmsg 是渠道给的自由文本,不进错误文案,也不声明。
type wechatSessionResp struct {
	OpenId  string `json:"openid"`
	UnionId string `json:"unionid"`
	ErrCode int    `json:"errcode"`
}

func (p *WeChatMiniGameProvider) Validate(ctx context.Context, code string) (*AuthResult, error) {
	if code == "" {
		return nil, fmt.Errorf("%s: empty code", WeChatMiniGameAuthType)
	}
	if p.AppId == "" || p.AppSecret == "" {
		return nil, fmt.Errorf("%s: provider not configured (missing AppId/AppSecret): %w",
			WeChatMiniGameAuthType, ErrProviderUnavailable)
	}

	endpoint := wechatBaseURL(p.Endpoint) + "/sns/jscode2session?" + url.Values{
		"appid":      {p.AppId},
		"secret":     {p.AppSecret},
		"js_code":    {code},
		"grant_type": {"authorization_code"},
	}.Encode()

	body, err := oauthGet(ctx, p.HTTP, WeChatMiniGameAuthType, endpoint)
	if err != nil {
		return nil, err
	}
	var r wechatSessionResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: bad json: %w", WeChatMiniGameAuthType, ErrProviderUnavailable)
	}
	if r.ErrCode != 0 {
		if wechatMiniGameErrCodeIsProviderSide(r.ErrCode) {
			return nil, fmt.Errorf("%s: errcode=%d: %w", WeChatMiniGameAuthType, r.ErrCode, ErrProviderUnavailable)
		}
		return nil, fmt.Errorf("%s: errcode=%d", WeChatMiniGameAuthType, r.ErrCode)
	}
	if r.OpenId == "" {
		// errcode=0 却没有 openid:应答不合约,不是玩家凭证的问题。
		return nil, fmt.Errorf("%s: empty openid: %w", WeChatMiniGameAuthType, ErrProviderUnavailable)
	}

	account, err := miniGameAccount(WeChatMiniGameAuthType, "wx_", r.OpenId, r.UnionId, p.AllowOpenIdFallback,
		"bind the mini game to a WeChat Open Platform account")
	if err != nil {
		return nil, err
	}
	logx.Infof("WeChat mini game auth ok: account=%s openid=%s union=%v", account, maskOAuthId(r.OpenId), r.UnionId != "")
	return &AuthResult{Account: account}, nil
}

// wechatMiniGameErrCodeIsProviderSide 判断 code2Session 的 errcode 是否指向本服务的渠道配置或微信自身状态。
//
// 清单沿用 wechatErrCodeIsProviderSide,只有 45011 例外:在 code2Session 上它是按用户限频(微信文档口径是
// 每个用户每分钟 100 次;2026-11-12 没能打开官方页面复核,按 tip-code-axis.md §2.9「拿不准不算故障」处理)。
// 那是单个玩家或刷接口的人的问题,标成故障会让一个客户端刷出满屏假告警。
// 40029(code 无效)、40163(code 已被使用)、40226(高风险用户被微信拦截)等一律按凭证无效处理。
func wechatMiniGameErrCodeIsProviderSide(code int) bool {
	return code != 45011 && wechatErrCodeIsProviderSide(code)
}

// DouyinMiniGameProvider 校验抖音小游戏的登录凭证。
//
// 小游戏端调 tt.login({force: true}) 拿到一次性 code,客户端发
// LoginRequest{auth_type:"douyin_minigame", auth_token: code},服务端拿它调 code2Session
// (文档要求带 Content-Type: application/json):
//
//	GET https://minigame.zijieapi.com/mgplatform/api/apps/jscode2session?appid=..&secret=..&code=..
//
// 没登录抖音的匿名用户只有 anonymousCode、没有 code:匿名身份跟着设备走,换设备就变,不能当账号,这里不接受。
// 成功时 error=0,带 openid / unionid;失败时 error 非 0,另带 errcode / errmsg。
// 账号名 "dy_" + unionid(同一开发者主体下各小游戏通用的用户标识);缺 unionid 的处理与微信相同。
type DouyinMiniGameProvider struct {
	AppId     string
	AppSecret string
	HTTP      *http.Client // 可选,测试注入;nil 用默认 5s 客户端

	// AllowOpenIdFallback 语义同 WeChatProvider.AllowOpenIdFallback;零值 = 缺 unionid 即失败。
	AllowOpenIdFallback bool

	// Endpoint 覆盖 minigame.zijieapi.com,指向沙箱 mock;空 = 生产地址。
	Endpoint string
}

// douyinSessionResp 是抖音 code2Session 的应答里登录要用的字段。session_key、anonymous_openid 与自由文本
// errmsg / message 故意不声明,理由同 wechatSessionResp。
type douyinSessionResp struct {
	Error   int    `json:"error"`
	ErrCode int    `json:"errcode"`
	OpenId  string `json:"openid"`
	UnionId string `json:"unionid"`
}

// douyinCode2SessionHeader 是抖音 code2Session 文档要求的请求头。oauthGetWithHeader 会复制一份,这里只读。
var douyinCode2SessionHeader = http.Header{"Content-Type": {"application/json"}}

func (p *DouyinMiniGameProvider) Validate(ctx context.Context, code string) (*AuthResult, error) {
	if code == "" {
		return nil, fmt.Errorf("%s: empty code", DouyinMiniGameAuthType)
	}
	if p.AppId == "" || p.AppSecret == "" {
		return nil, fmt.Errorf("%s: provider not configured (missing AppId/AppSecret): %w",
			DouyinMiniGameAuthType, ErrProviderUnavailable)
	}

	endpoint := douyinMiniGameBaseURL(p.Endpoint) + "/mgplatform/api/apps/jscode2session?" + url.Values{
		"appid":  {p.AppId},
		"secret": {p.AppSecret},
		"code":   {code},
	}.Encode()

	body, err := oauthGetWithHeader(ctx, p.HTTP, DouyinMiniGameAuthType, endpoint, douyinCode2SessionHeader)
	if err != nil {
		return nil, err
	}
	var r douyinSessionResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%s: bad json: %w", DouyinMiniGameAuthType, ErrProviderUnavailable)
	}
	if r.Error != 0 || r.ErrCode != 0 {
		// 抖音的 code2Session 文档没有给出错误码表,分不清哪些是应用侧故障:与 QQ 同一处理,拿不准一律按凭证无效
		// (tip-code-axis.md §2.9)。AppId / AppSecret 配错时所有登录都会走到这里,errcode 在 Login 的 ERROR 日志里。
		return nil, fmt.Errorf("%s: error=%d errcode=%d", DouyinMiniGameAuthType, r.Error, r.ErrCode)
	}
	if r.OpenId == "" {
		// error=0 却没有 openid:应答不合约(带 code 的请求必返回 openid),不是玩家凭证的问题。
		return nil, fmt.Errorf("%s: empty openid: %w", DouyinMiniGameAuthType, ErrProviderUnavailable)
	}

	account, err := miniGameAccount(DouyinMiniGameAuthType, "dy_", r.OpenId, r.UnionId, p.AllowOpenIdFallback,
		"check the response of the Douyin code2Session API")
	if err != nil {
		return nil, err
	}
	logx.Infof("Douyin mini game auth ok: account=%s openid=%s union=%v", account, maskOAuthId(r.OpenId), r.UnionId != "")
	return &AuthResult{Account: account}, nil
}

// miniGameAccount 按 unionid 策略拼账号名:有 unionid 就用它;没有时只有显式允许才回落 openid,否则拒绝并标
// ErrProviderUnavailable(缺 unionid 是应用配置问题,不是玩家凭证问题)。规则与 WeChatProvider / QQProvider 相同。
func miniGameAccount(provider, prefix, openid, unionid string, allowOpenIdFallback bool, hint string) (string, error) {
	if unionid != "" {
		return prefix + unionid, nil
	}
	if !allowOpenIdFallback {
		return "", fmt.Errorf("%s: unionid missing (RequireUnionId; %s): %w", provider, hint, ErrProviderUnavailable)
	}
	return prefix + openid, nil
}

// douyinMiniGameBaseURL 返回抖音小游戏服务端接口的域名;override 非空时用它(沙箱 mock)。
// override 的形状由 config.ResolveSecrets 在启动期校验(config/auth_providers.go checkOAuthEndpoint)。
func douyinMiniGameBaseURL(override string) string {
	if override == "" {
		return "https://minigame.zijieapi.com"
	}
	return strings.TrimRight(override, "/")
}
