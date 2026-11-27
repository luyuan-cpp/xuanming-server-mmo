package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cgpb "proto/content_guard"

	"github.com/prometheus/client_golang/prometheus"

	"content_guard/internal/metrics"
)

func TestWeChatAdapterTokenCacheRetryAndVerdicts(t *testing.T) {
	var tokenCalls atomic.Int32
	var checkCalls atomic.Int32
	suggest := "pass"
	label := 100
	expireFirstCheck := false
	forceErrcode := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/stable_token":
			// 稳定版接口:POST,AppSecret 在请求体里,URL 上不带任何凭据。
			var body struct {
				GrantType string `json:"grant_type"`
				AppID     string `json:"appid"`
				Secret    string `json:"secret"`
			}
			if r.Method != http.MethodPost || r.URL.RawQuery != "" || json.NewDecoder(r.Body).Decode(&body) != nil ||
				body.GrantType != "client_credential" || body.AppID != "app1" || body.Secret != "sec1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := tokenCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + string(rune('0'+n)), "expires_in": 7200})
		case "/wxa/msg_sec_check":
			c := checkCalls.Add(1)
			if expireFirstCheck && c == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 40001, "errmsg": "invalid token"})
				return
			}
			if forceErrcode != 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": forceErrcode, "errmsg": "forced"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode": 0,
				"result":  map[string]any{"suggest": suggest, "label": label},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewWeChatAdapter("app1", "sec1", srv.URL, srv.Client())
	res := adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "oid-1", "青云客")
	if res.Outcome != OutcomePass || res.Provider != ProviderWeChat {
		t.Fatalf("pass check got %+v", res)
	}

	suggest, label = "risky", 10001
	res = adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "oid-1", "加微信领礼包")
	if res.Outcome != OutcomeReject || res.Category != cgpb.TextCategory_TEXT_CATEGORY_AD {
		t.Fatalf("risky check got %+v", res)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token should be cached, got %d fetches", got)
	}

	// token 被平台提前吊销时应自动刷新并重试一次。
	checkCalls.Store(0)
	expireFirstCheck = true
	suggest = "pass"
	res = adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "oid-1", "正常发言")
	if res.Outcome != OutcomePass || tokenCalls.Load() != 2 || checkCalls.Load() != 2 {
		t.Fatalf("token refresh retry failed: res=%+v tokenCalls=%d checkCalls=%d", res, tokenCalls.Load(), checkCalls.Load())
	}

	// 平台用错误码答复的三种情况:内容违规按违规算;用户近两小时没访问过小游戏不是故障,只走本地层;其余错误码按不可用算。
	expireFirstCheck = false
	for code, want := range map[int]Outcome{87014: OutcomeReject, 61010: OutcomeSkip, 45009: OutcomeUnavailable} {
		forceErrcode = code
		if res = adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "oid-1", "正常发言"); res.Outcome != want {
			t.Fatalf("errcode %d: got %+v, want outcome %d", code, res, want)
		}
	}
}

// 请求失败时进日志的错误不能带 URL:微信审核接口的查询串里有调用凭据。
func TestWithoutURLDropsTheRequestURL(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	// http.Client 的请求错误就是这个形状:文本里带完整的 URL。
	raw := &url.Error{Op: "Post", URL: "https://api.weixin.qq.com/wxa/msg_sec_check?access_token=SECRET-TOKEN", Err: cause}
	if !strings.Contains(raw.Error(), "SECRET-TOKEN") {
		t.Fatalf("precondition: the raw error should carry the URL, got %q", raw.Error())
	}
	redacted := withoutURL(raw)
	if text := redacted.Error(); strings.Contains(text, "SECRET-TOKEN") || strings.Contains(text, "access_token") || !strings.Contains(text, "i/o timeout") {
		t.Fatalf("redacted error must drop the URL and keep the cause: %q", text)
	}
	if !errors.Is(redacted, cause) {
		t.Fatalf("the cause must stay reachable through errors.Is")
	}
	// 不是请求错误的照原样返回。
	if withoutURL(cause) != cause {
		t.Fatalf("a non-request error must be returned unchanged")
	}
}

func TestDouyinAdapterHitProbAnd401Retry(t *testing.T) {
	hit := false
	retry401 := true
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/apps/v2/token":
			n := tokenCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"err_no": 0,
				"data":   map[string]any{"access_token": "dy-tok-" + string(rune('0'+n)), "expires_in": 7200},
			})
		case "/api/v2/tags/text/antidirt":
			if retry401 && r.Header.Get("X-Token") == "dy-tok-1" {
				retry401 = false
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"code":     0,
					"predicts": []map[string]any{{"hit": hit, "prob": 0, "model_name": "short_content_antidirt"}},
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := NewDouyinAdapter("dy-app", "dy-sec", srv.URL, srv.Client())
	if res := adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_GUILD_NAME, "dy-open", "逍遥帮"); res.Outcome != OutcomePass {
		t.Fatalf("pass after 401 retry got %+v", res)
	}
	if got := tokenCalls.Load(); got != 2 {
		t.Fatalf("expected token refresh after 401, got %d token calls", got)
	}
	hit = true
	if res := adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_GUILD_NAME, "dy-open", "违规词"); res.Outcome != OutcomeReject {
		t.Fatalf("hit=true reject got %+v", res)
	}
}

type fakeAdapter struct {
	provider string
	delay    time.Duration
	res      ReviewResult

	mu      sync.Mutex
	openIDs []string // 每次调用收到的用户标识
}

func (f *fakeAdapter) seenOpenIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.openIDs...)
}

func (f *fakeAdapter) Provider() string { return f.provider }
func (f *fakeAdapter) Check(ctx context.Context, _ cgpb.TextScene, openID, _ string) ReviewResult {
	f.mu.Lock()
	f.openIDs = append(f.openIDs, openID)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ReviewResult{Outcome: OutcomeUnavailable, Provider: f.provider}
		}
	}
	out := f.res
	out.Provider = f.provider
	return out
}

// 带给平台的用户标识只从目录取:账号名的后缀是 unionid 不是 openid;App 端微信登录(channel=wechat)记下的 openid 属于另一个 AppID,
// 这两种都不能送小游戏的审核接口(平台会按 openid 无效拒掉,名字类场景就成了永远起不了名)。
func TestRouterTakesTheOpenIDFromTheDirectoryOnly(t *testing.T) {
	wechat := &fakeAdapter{provider: ProviderWeChat, res: ReviewResult{Outcome: OutcomePass}}
	identities := map[string]Identity{
		"wx_union_mini": {Channel: "wechat_minigame", OpenID: "mini-openid", Account: "wx_union_mini"},
		"wx_union_app":  {Channel: "wechat", OpenID: "app-openid", Account: "wx_union_app"},
		"wx_union_none": {Account: "wx_union_none"},
	}
	var askedPlayers []uint64
	router := NewRouter(RouterOptions{
		Lookup: IdentityLookupFunc(func(_ context.Context, playerID uint64, account string) (Identity, error) {
			if playerID != 0 {
				askedPlayers = append(askedPlayers, playerID)
				return identities["wx_union_mini"], nil
			}
			return identities[account], nil
		}),
		Adapters: []Adapter{wechat},
	})
	review := func(playerID uint64, account string) ReviewResult {
		return router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, playerID, account, "青云客", false)
	}
	// 建角:没有 player_id,按账号到目录查;送给平台的是目录里的 openid,不是账号名的后缀。
	if res := review(0, "wx_union_mini"); res.Outcome != OutcomePass {
		t.Fatalf("mini game account got %+v", res)
	}
	// App 端微信登录的账号、没有渠道身份的账号:不送小游戏的审核接口,只走本地层。
	for _, account := range []string{"wx_union_app", "wx_union_none"} {
		if res := review(0, account); res.Outcome != OutcomeSkip {
			t.Fatalf("account %s must skip the mini game reviewer, got %+v", account, res)
		}
	}
	// 有 player_id 时以它为准,请求里带着别的账号名也不看。
	if res := review(7, "wx_union_app"); res.Outcome != OutcomePass {
		t.Fatalf("player lookup got %+v", res)
	}
	if got := wechat.seenOpenIDs(); len(got) != 2 || got[0] != "mini-openid" || got[1] != "mini-openid" {
		t.Fatalf("the reviewer must only ever see the open id recorded in the directory, got %v", got)
	}
	if len(askedPlayers) != 1 || askedPlayers[0] != 7 {
		t.Fatalf("expected exactly one lookup by player 7, got %v", askedPlayers)
	}
}

func TestRouterScenePoliciesLookupCacheAndPendingAsyncSync(t *testing.T) {
	var lookupCalls atomic.Int32
	slowReject := &fakeAdapter{
		provider: ProviderWeChat,
		delay:    80 * time.Millisecond,
		res:      ReviewResult{Outcome: OutcomeReject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD},
	}
	router := NewRouter(RouterOptions{
		Lookup: IdentityLookupFunc(func(_ context.Context, id uint64, _ string) (Identity, error) {
			lookupCalls.Add(1)
			if id == 99 {
				return Identity{}, errors.New("db down")
			}
			return Identity{Channel: "wechat_minigame", OpenID: "oid123", Account: "custom_acc"}, nil
		}),
		SyncTimeout:   300 * time.Millisecond,
		WorldTimeout:  20 * time.Millisecond,
		AsyncTimeout:  300 * time.Millisecond,
		LookupTimeout: 100 * time.Millisecond,
		Adapters:      []Adapter{slowReject},
	})

	// 私有标签跳过外部审核。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_PRIVATE_LABEL, 1, "", "方案1", false); res.Outcome != OutcomeSkip {
		t.Fatalf("private label got %+v", res)
	}
	// 建角场景没有 player_id:按账号到目录查渠道身份,照样路由到微信。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, "wx_some_union", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("account-only role name got %+v", res)
	}
	// 名字类同步等待并返回拒绝,且第二次命中本地身份缓存不重复查目录。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("role name got %+v", res)
	}
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("cached role name got %+v", res)
	}
	if got := lookupCalls.Load(); got != 2 {
		t.Fatalf("expected 2 lookups (one by account, one by player, the repeat served from cache), got %d", got)
	}
	// 名字类查不到渠道账号按不可用 fail-closed。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 99, "", "正常名", false); res.Outcome != OutcomeUnavailable {
		t.Fatalf("lookup error on name got %+v", res)
	}
	// 世界频道短超时 + asyncWorld=true:先回 OutcomePending 让 chat 落库后后台调 ReviewSync。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 7, "", "隐藏广告", true); res.Outcome != OutcomePending {
		t.Fatalf("world chat short timeout got %+v, want Pending", res)
	}
	// 世界频道 ReviewSync 用完整 AsyncTimeout 跑完并判定违规。
	if res := router.ReviewSync(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 7, "", "隐藏广告"); res.Outcome != OutcomeReject {
		t.Fatalf("world chat ReviewSync got %+v, want Reject", res)
	}
}

// platformReviewCount 读 content_guard_platform_reviews_total{provider, outcome} 的当前值;没出现过的组合是 0。
func platformReviewCount(t *testing.T, provider, outcome string) float64 {
	t.Helper()
	metrics.Register()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "content_guard_platform_reviews_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["provider"] == provider && labels["outcome"] == outcome {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// 平台调用按最终结局计数:世界频道先行的短预算没等到记 deferred(交给后台补审,不算故障),补审与名字类等不到记 unavailable。
// 告警按 unavailable 的占比报平台故障(deploy/k8s/content-guard-alerts.yaml ContentGuardPlatformFailing);
// 世界频道补审再不可用只打日志,没有这个计数就看不见平台挂了。
func TestRouterCountsPlatformOutcomes(t *testing.T) {
	down := &fakeAdapter{provider: ProviderDouyin, res: ReviewResult{Outcome: OutcomeUnavailable}}
	router := NewRouter(RouterOptions{
		Lookup: IdentityLookupFunc(func(context.Context, uint64, string) (Identity, error) {
			return Identity{Channel: "douyin_minigame", OpenID: "dy-open", Account: "dy_acc"}, nil
		}),
		Adapters: []Adapter{down},
	})
	deferredBefore := platformReviewCount(t, ProviderDouyin, metrics.PlatformOutcomeDeferred)
	unavailableBefore := platformReviewCount(t, ProviderDouyin, metrics.PlatformOutcomeUnavailable)

	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 5, "", "大家好", true); res.Outcome != OutcomePending {
		t.Fatalf("world chat with the platform down got %+v, want Pending", res)
	}
	if res := router.ReviewSync(context.Background(), cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, 5, "", "大家好"); res.Outcome != OutcomeUnavailable {
		t.Fatalf("async re-check with the platform down got %+v, want Unavailable", res)
	}
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 5, "", "青云客", false); res.Outcome != OutcomeUnavailable {
		t.Fatalf("role name with the platform down got %+v, want Unavailable", res)
	}

	if got := platformReviewCount(t, ProviderDouyin, metrics.PlatformOutcomeDeferred) - deferredBefore; got != 1 {
		t.Fatalf("deferred grew by %v, want 1 (only the world chat fast path)", got)
	}
	if got := platformReviewCount(t, ProviderDouyin, metrics.PlatformOutcomeUnavailable) - unavailableBefore; got != 2 {
		t.Fatalf("unavailable grew by %v, want 2 (the async re-check and the role name)", got)
	}
}
