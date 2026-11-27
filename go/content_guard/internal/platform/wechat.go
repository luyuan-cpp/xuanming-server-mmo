package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	cgpb "proto/content_guard"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	defaultWeChatBaseURL     = "https://api.weixin.qq.com"
	wechatTokenRefreshMargin = 5 * time.Minute
	maxPlatformRespBytes     = 64 * 1024

	// wechatErrRiskyContent:平台用错误码回「内容违规」(旧版接口的做法;新版走 result.suggest)。
	wechatErrRiskyContent = 87014
	// wechatErrUserNotRecentlyActive:这名用户近两小时没有访问过小游戏,平台不审他发的内容。
	wechatErrUserNotRecentlyActive = 61010
)

// WeChatAdapter 封装微信小游戏内容安全接口 msgSecCheck v2。
// AppSecret 仅由进程启动期从环境变量读入内存,不进配置和日志。
//
// 调用凭据用「稳定版」接口(/cgi-bin/stable_token)取:普通模式下有效期内重复调用拿到的是同一个凭据,不会让别的副本手里的
// 凭据失效。老接口(/cgi-bin/token)每取一次就让上一个凭据在几分钟后失效,本服务多副本各取各的会互相挤掉,
// 副本越多越频繁,还会撞上每天的取用次数上限。稳定版把 AppSecret 放在请求体里,不进 URL。
type WeChatAdapter struct {
	appID     string
	appSecret string
	baseURL   string
	client    *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func NewWeChatAdapter(appID, appSecret, baseURL string, client *http.Client) *WeChatAdapter {
	if baseURL == "" {
		baseURL = defaultWeChatBaseURL
	}
	if client == nil {
		client = newDefaultHTTPClient()
	}
	return &WeChatAdapter{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   baseURL,
		client:    client,
	}
}

func (w *WeChatAdapter) Provider() string { return ProviderWeChat }

func (w *WeChatAdapter) accessToken(ctx context.Context) (string, error) {
	now := time.Now()
	w.mu.Lock()
	if w.token != "" && now.Add(wechatTokenRefreshMargin).Before(w.expiresAt) {
		tok := w.token
		w.mu.Unlock()
		return tok, nil
	}
	w.mu.Unlock()

	payload, err := json.Marshal(struct {
		GrantType string `json:"grant_type"`
		AppID     string `json:"appid"`
		Secret    string `json:"secret"`
	}{GrantType: "client_credential", AppID: w.appID, Secret: w.appSecret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/cgi-bin/stable_token", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return "", withoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wechat token http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlatformRespBytes))
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", fmt.Errorf("wechat token errcode=%d errmsg=%s", out.ErrCode, out.ErrMsg)
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= wechatTokenRefreshMargin {
		ttl = wechatTokenRefreshMargin + time.Minute
	}
	w.mu.Lock()
	w.token = out.AccessToken
	w.expiresAt = time.Now().Add(ttl)
	w.mu.Unlock()
	return out.AccessToken, nil
}

func wechatSceneCode(scene cgpb.TextScene) int {
	switch scene {
	case cgpb.TextScene_TEXT_SCENE_ROLE_NAME,
		cgpb.TextScene_TEXT_SCENE_GUILD_NAME,
		cgpb.TextScene_TEXT_SCENE_PET_NAME,
		cgpb.TextScene_TEXT_SCENE_TRADE_TITLE:
		return 1 // 资料
	case cgpb.TextScene_TEXT_SCENE_GUILD_ANNOUNCEMENT:
		return 2 // 评论/公告
	case cgpb.TextScene_TEXT_SCENE_CHAT_WORLD:
		return 3 // 论坛/公共频道
	default:
		return 4 // 社交日志
	}
}

func wechatLabelCategory(label int) cgpb.TextCategory {
	switch label {
	case 20001:
		return cgpb.TextCategory_TEXT_CATEGORY_POLITICS
	case 20002, 20003:
		return cgpb.TextCategory_TEXT_CATEGORY_PORN
	case 20006, 20008, 21000:
		return cgpb.TextCategory_TEXT_CATEGORY_OTHER
	case 20012, 20013:
		return cgpb.TextCategory_TEXT_CATEGORY_ABUSE
	case 10001:
		return cgpb.TextCategory_TEXT_CATEGORY_AD
	default:
		return cgpb.TextCategory_TEXT_CATEGORY_OTHER
	}
}

func (w *WeChatAdapter) invalidateToken(stale string) {
	w.mu.Lock()
	if w.token == stale {
		w.token = ""
		w.expiresAt = time.Time{}
	}
	w.mu.Unlock()
}

func (w *WeChatAdapter) Check(ctx context.Context, scene cgpb.TextScene, openID, text string) ReviewResult {
	if openID == "" {
		// 微信 msgSecCheck v2 必填 openid;缺 openid 时跳过外部层(由本地词库兜底)。
		return ReviewResult{Outcome: OutcomeSkip, Provider: ProviderWeChat}
	}
	payload, err := json.Marshal(struct {
		Content string `json:"content"`
		Version int    `json:"version"`
		Scene   int    `json:"scene"`
		OpenID  string `json:"openid"`
	}{
		Content: text,
		Version: 2,
		Scene:   wechatSceneCode(scene),
		OpenID:  openID,
	})
	if err != nil {
		return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := w.accessToken(ctx)
		if err != nil {
			logx.Errorf("[content_guard/wechat] token fetch failed: %v", err)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		endpoint := fmt.Sprintf("%s/wxa/msg_sec_check?access_token=%s", w.baseURL, url.QueryEscape(token))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := w.client.Do(req)
		if err != nil {
			// 这个 URL 的查询串里带着调用凭据:只记去掉 URL 之后的原因。
			logx.Errorf("[content_guard/wechat] msg_sec_check request failed: %v", withoutURL(err))
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			logx.Errorf("[content_guard/wechat] msg_sec_check http status=%d", resp.StatusCode)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlatformRespBytes))
		resp.Body.Close()
		if err != nil {
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		var out struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
			Result  struct {
				Suggest string `json:"suggest"`
				Label   int    `json:"label"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			logx.Errorf("[content_guard/wechat] msg_sec_check invalid json: %v", err)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		if out.ErrCode != 0 {
			// access_token 失效/过期(40001/40014/42001)时清缓存并立刻重取重试一次。
			if out.ErrCode == 40001 || out.ErrCode == 40014 || out.ErrCode == 42001 {
				w.invalidateToken(token)
				continue
			}
			switch out.ErrCode {
			case wechatErrRiskyContent:
				// 「平台说违规」不能当成「平台没答复」:世界频道对没答复的口径是先放行。
				return ReviewResult{Outcome: OutcomeReject, Category: cgpb.TextCategory_TEXT_CATEGORY_OTHER, Provider: ProviderWeChat}
			case wechatErrUserNotRecentlyActive:
				// 不是故障,重试也不会变:和没有 openid 一样只走本地词库。当成不可用的话,连续在线超过两小时的玩家
				// 会一直改不了名、建不了帮、发不了公告(这几类对不可用的口径是拒绝)。
				return ReviewResult{Outcome: OutcomeSkip, Provider: ProviderWeChat}
			}
			logx.Errorf("[content_guard/wechat] msg_sec_check errcode=%d errmsg=%s", out.ErrCode, out.ErrMsg)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
		}
		if out.Result.Suggest == "pass" {
			return ReviewResult{Outcome: OutcomePass, Provider: ProviderWeChat}
		}
		return ReviewResult{
			Outcome:  OutcomeReject,
			Category: wechatLabelCategory(out.Result.Label),
			Provider: ProviderWeChat,
		}
	}
	return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderWeChat}
}
