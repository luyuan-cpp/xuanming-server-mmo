package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	cgpb "proto/content_guard"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	defaultDouyinBaseURL     = "https://developer.toutiao.com"
	douyinTokenRefreshMargin = 5 * time.Minute
)

// DouyinAdapter 封装抖音小游戏内容安全检测接口(antidirt)。
type DouyinAdapter struct {
	appID     string
	appSecret string
	baseURL   string
	client    *http.Client

	// shared 是各副本共用凭据的地方(token_store.go);nil = 各取各的(单测、没连 etcd)。
	shared TokenStore

	mu        sync.Mutex
	token     string
	expiresAt time.Time
	// stale 是最近一次被平台判为失效的凭据:共用处还存着它时不能再拿来用。
	stale string
}

// WithSharedToken 让这个适配器与别的副本共用一份调用凭据。
func (d *DouyinAdapter) WithSharedToken(store TokenStore) *DouyinAdapter {
	d.shared = store
	return d
}

func NewDouyinAdapter(appID, appSecret, baseURL string, client *http.Client) *DouyinAdapter {
	if baseURL == "" {
		baseURL = defaultDouyinBaseURL
	}
	if client == nil {
		client = newDefaultHTTPClient()
	}
	return &DouyinAdapter{
		appID:     appID,
		appSecret: appSecret,
		baseURL:   baseURL,
		client:    client,
	}
}

func (d *DouyinAdapter) Provider() string { return ProviderDouyin }

// accessToken 取调用凭据:先看本副本的缓存,再看共用处(别的副本可能已经换过了),都不能用才向平台取,
// 取到之后写回共用处;写不进去(别的副本同时也取了)就改用写进去的那个,大家用同一个。
func (d *DouyinAdapter) accessToken(ctx context.Context) (string, error) {
	now := time.Now()
	d.mu.Lock()
	if d.token != "" && now.Add(douyinTokenRefreshMargin).Before(d.expiresAt) {
		tok := d.token
		d.mu.Unlock()
		return tok, nil
	}
	stale := d.stale
	d.mu.Unlock()

	usable := func(token string, expiresAt time.Time) bool {
		return token != "" && token != stale && time.Now().Add(douyinTokenRefreshMargin).Before(expiresAt)
	}
	revision, shared := int64(0), d.shared != nil
	if shared {
		token, expiresAt, rev, err := d.shared.Load(ctx)
		switch {
		case err != nil:
			logx.Errorf("[content_guard/douyin] shared token read failed, fetching one for this replica: %v", err)
			shared = false
		case usable(token, expiresAt):
			d.remember(token, expiresAt)
			return token, nil
		default:
			revision = rev
		}
	}
	token, expiresAt, err := d.fetchToken(ctx)
	if err != nil {
		return "", err
	}
	if shared {
		stored, storeErr := d.shared.CompareAndStore(ctx, revision, token, expiresAt)
		switch {
		case storeErr != nil:
			logx.Errorf("[content_guard/douyin] shared token write failed, using this replica's token: %v", storeErr)
		case !stored:
			if other, otherExpiresAt, _, loadErr := d.shared.Load(ctx); loadErr == nil && usable(other, otherExpiresAt) {
				token, expiresAt = other, otherExpiresAt
			}
		}
	}
	d.remember(token, expiresAt)
	return token, nil
}

func (d *DouyinAdapter) remember(token string, expiresAt time.Time) {
	d.mu.Lock()
	d.token = token
	d.expiresAt = expiresAt
	d.mu.Unlock()
}

// fetchToken 向平台取一个新的调用凭据。
func (d *DouyinAdapter) fetchToken(ctx context.Context) (string, time.Time, error) {
	payload, err := json.Marshal(struct {
		AppID     string `json:"appid"`
		Secret    string `json:"secret"`
		GrantType string `json:"grant_type"`
	}{
		AppID:     d.appID,
		Secret:    d.appSecret,
		GrantType: "client_credential",
	})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/api/apps/v2/token", bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", time.Time{}, withoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("douyin token http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlatformRespBytes))
	if err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		ErrNo   int    `json:"err_no"`
		ErrTips string `json:"err_tips"`
		Data    struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.ErrNo != 0 || out.Data.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("douyin token err_no=%d err_tips=%s", out.ErrNo, out.ErrTips)
	}
	ttl := time.Duration(out.Data.ExpiresIn) * time.Second
	if ttl <= douyinTokenRefreshMargin {
		ttl = douyinTokenRefreshMargin + time.Minute
	}
	return out.Data.AccessToken, time.Now().Add(ttl), nil
}

// invalidateToken 在平台判凭据失效时调用:清掉本副本的缓存,并记住它,免得从共用处又把它读回来。
func (d *DouyinAdapter) invalidateToken(stale string) {
	d.mu.Lock()
	d.stale = stale
	if d.token == stale {
		d.token = ""
		d.expiresAt = time.Time{}
	}
	d.mu.Unlock()
}

func (d *DouyinAdapter) Check(ctx context.Context, _ cgpb.TextScene, _ string, text string) ReviewResult {
	payload, err := json.Marshal(struct {
		Tasks []struct {
			Content string `json:"content"`
		} `json:"tasks"`
	}{
		Tasks: []struct {
			Content string `json:"content"`
		}{{Content: text}},
	})
	if err != nil {
		return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
	}

	for attempt := 0; attempt < 2; attempt++ {
		token, err := d.accessToken(ctx)
		if err != nil {
			logx.Errorf("[content_guard/douyin] token fetch failed: %v", err)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/api/v2/tags/text/antidirt", bytes.NewReader(payload))
		if err != nil {
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Token", token)

		resp, err := d.client.Do(req)
		if err != nil {
			logx.Errorf("[content_guard/douyin] antidirt request failed: %v", err)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			d.invalidateToken(token)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			logx.Errorf("[content_guard/douyin] antidirt http status=%d", resp.StatusCode)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlatformRespBytes))
		resp.Body.Close()
		if err != nil {
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		var out struct {
			ErrorID string `json:"error_id"`
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    []struct {
				Code     int `json:"code"`
				Predicts []struct {
					Hit   bool    `json:"hit"`
					Prob  float64 `json:"prob"`
					Model string  `json:"model_name"`
				} `json:"predicts"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			logx.Errorf("[content_guard/douyin] antidirt invalid json: %v", err)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		if out.Code == 401 || out.Code == 40002 || out.Code == 40020 {
			d.invalidateToken(token)
			continue
		}
		if out.Code != 0 || len(out.Data) == 0 {
			logx.Errorf("[content_guard/douyin] antidirt code=%d message=%s", out.Code, out.Message)
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
		}
		for _, item := range out.Data {
			if item.Code != 0 {
				return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
			}
			for _, p := range item.Predicts {
				if p.Hit || p.Prob >= 1 {
					return ReviewResult{
						Outcome:  OutcomeReject,
						Category: cgpb.TextCategory_TEXT_CATEGORY_OTHER,
						Provider: ProviderDouyin,
					}
				}
			}
		}
		return ReviewResult{Outcome: OutcomePass, Provider: ProviderDouyin}
	}
	return ReviewResult{Outcome: OutcomeUnavailable, Provider: ProviderDouyin}
}
