package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cgpb "proto/content_guard"
)

// memoryTokenStore 是共用处的替身,版本号语义与 etcd 相同:键不存在为 0,每次写加一。
type memoryTokenStore struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	revision  int64
}

func (m *memoryTokenStore) Load(context.Context) (string, time.Time, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token, m.expiresAt, m.revision, nil
}

func (m *memoryTokenStore) CompareAndStore(_ context.Context, revision int64, token string, expiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if revision != m.revision {
		return false, nil
	}
	m.token, m.expiresAt, m.revision = token, expiresAt, m.revision+1
	return true, nil
}

// 两个副本共用一份抖音凭据:第二个副本不再去取;平台判凭据失效之后只有发现的那个副本重取一次,
// 另一个副本自己的缓存失效时从共用处拿到新的,不再重取。平台按「只认最新取到的那个」判(最严的情形)。
func TestDouyinReplicasShareOneToken(t *testing.T) {
	var fetches atomic.Int32
	var current atomic.Value
	current.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/apps/v2/token":
			token := "tok-" + strconv.Itoa(int(fetches.Add(1)))
			current.Store(token)
			_ = json.NewEncoder(w).Encode(map[string]any{"err_no": 0, "data": map[string]any{"access_token": token, "expires_in": 7200}})
		case "/api/v2/tags/text/antidirt":
			if r.Header.Get("X-Token") != current.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": []any{map[string]any{"code": 0, "predicts": []any{}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &memoryTokenStore{}
	a := NewDouyinAdapter("app", "secret", srv.URL, srv.Client()).WithSharedToken(store)
	b := NewDouyinAdapter("app", "secret", srv.URL, srv.Client()).WithSharedToken(store)
	check := func(name string, adapter *DouyinAdapter) {
		t.Helper()
		if res := adapter.Check(context.Background(), cgpb.TextScene_TEXT_SCENE_GUILD_NAME, "", "青云"); res.Outcome != OutcomePass {
			t.Fatalf("%s: want pass, got %+v", name, res)
		}
	}

	check("a first", a)
	check("b first", b)
	if got := fetches.Load(); got != 1 {
		t.Fatalf("the second replica must reuse the shared token, got %d fetches", got)
	}

	// 平台那边凭据失效了(到期或被别处重取):a 发现、重取一次并写回共用处。
	current.Store("")
	check("a after expiry", a)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("only the replica that saw the failure refetches, got %d fetches", got)
	}
	// b 手里还是旧的:被判失效之后从共用处拿到 a 取的那个,不再自己取(自己取会让 a 的那个失效)。
	check("b after a refreshed", b)
	if got := fetches.Load(); got != 2 {
		t.Fatalf("the other replica must pick up the shared token instead of fetching, got %d fetches", got)
	}
}
