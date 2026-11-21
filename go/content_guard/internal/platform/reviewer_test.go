package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	cgpb "proto/content_guard"
)

func TestWeChatAdapterTokenCacheRetryAndVerdicts(t *testing.T) {
	var tokenCalls atomic.Int32
	var checkCalls atomic.Int32
	suggest := "pass"
	label := 100
	expireFirstCheck := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/token":
			n := tokenCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + string(rune('0'+n)), "expires_in": 7200})
		case "/wxa/msg_sec_check":
			c := checkCalls.Add(1)
			if expireFirstCheck && c == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 40001, "errmsg": "invalid token"})
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
}

func (f *fakeAdapter) Provider() string { return f.provider }
func (f *fakeAdapter) Check(ctx context.Context, _ cgpb.TextScene, _, _ string) ReviewResult {
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

func TestRouterScenePoliciesLookupCacheAndPendingAsyncSync(t *testing.T) {
	var lookupCalls atomic.Int32
	slowReject := &fakeAdapter{
		provider: ProviderWeChat,
		delay:    80 * time.Millisecond,
		res:      ReviewResult{Outcome: OutcomeReject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD},
	}
	router := NewRouter(RouterOptions{
		Lookup: IdentityLookupFunc(func(_ context.Context, id uint64) (Identity, error) {
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
	// 名字类直传 account 时无需 player_id(建角场景)也能路由到微信。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, "wx_direct_oid", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("direct account role name got %+v", res)
	}
	// 名字类同步等待并返回拒绝,且第二次命中本地身份缓存不重复查目录。
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("role name got %+v", res)
	}
	if res := router.Review(context.Background(), cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 1, "", "广告名", false); res.Outcome != OutcomeReject {
		t.Fatalf("cached role name got %+v", res)
	}
	if got := lookupCalls.Load(); got != 1 {
		t.Fatalf("expected 1 lookup due to cache, got %d", got)
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
