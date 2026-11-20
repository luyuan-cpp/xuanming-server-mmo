package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件钉住身份令牌的一次性消费(docs/design/account-deletion.md §3.4):
//   - 同一枚令牌第二次来按凭证无效拒绝(不标渠道故障);
//   - 登记读写失败按渠道故障,fail-closed;
//   - 验签不过的令牌不占用登记;登记的键不含令牌原文和 sub;
//   - 登记留到令牌过期加时钟偏差,最少 1 秒。

// memoryReplayGuard 是测试用的一次性消费登记:记下键与 TTL,可以注入故障。
type memoryReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Duration
	err  error
}

func (g *memoryReplayGuard) FirstUse(_ context.Context, key string, ttl time.Duration) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return false, g.err
	}
	if _, ok := g.seen[key]; ok {
		return false, nil
	}
	if g.seen == nil {
		g.seen = map[string]time.Duration{}
	}
	g.seen[key] = ttl
	return true, nil
}

func (g *memoryReplayGuard) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}

func TestIdTokenProviderConsumesEachTokenOnce(t *testing.T) {
	stub := &jwksStub{paths: make(chan string, 16)}
	stub.publish(publicJWK("kid1", testRSAKey1()))
	cli, srv := newTestClient(stub)
	t.Cleanup(srv.Close)
	guard := &memoryReplayGuard{}
	p := NewAppleProvider(testIdTokenClientID, "", cli).WithReplayGuard(guard)

	claims := validClaims(newTestClock())
	claims["iss"] = appleIssuer
	claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(10*time.Minute).Unix()
	token := signTestIdToken(t, testRSAKey1(), "kid1", claims)

	if _, err := p.Validate(context.Background(), token); err != nil {
		t.Fatalf("第一次: %v", err)
	}
	_, err := p.Validate(context.Background(), token)
	requireInvalidCredential(t, err, "同一枚令牌第二次")

	if guard.count() != 1 {
		t.Fatalf("登记条数 = %d, want 1", guard.count())
	}
	for key, ttl := range guard.seen {
		if strings.Contains(key, token) || strings.Contains(key, claims["sub"].(string)) {
			t.Fatalf("登记的键带了令牌原文或 sub: %s", key)
		}
		if !strings.HasPrefix(key, "login:idtoken:used:v1:apple:") {
			t.Fatalf("键形不对: %s", key)
		}
		if ttl <= 10*time.Minute || ttl > 10*time.Minute+idTokenLeeway {
			t.Fatalf("ttl = %v,应到 exp 加时钟偏差为止", ttl)
		}
	}

	// 验签不过的不占用登记:否则随手编的串能把合法令牌提前用掉。
	_, err = p.Validate(context.Background(), signTestIdToken(t, testRSAKey2(), "kid1", claims))
	requireInvalidCredential(t, err, "签名不对")
	if guard.count() != 1 {
		t.Fatal("验签不过的令牌占用了登记")
	}

	// 登记失败:判断不了是不是重放,按渠道故障。
	claims["sub"] = "another-subject"
	guard.err = errors.New("redis down")
	_, err = p.Validate(context.Background(), signTestIdToken(t, testRSAKey1(), "kid1", claims))
	requireProviderUnavailable(t, err, "登记失败")
}

func TestIdTokenUsedForCoversExpiryPlusLeeway(t *testing.T) {
	v, _, clock := newTestVerifier(t)
	claimsExpiringIn := func(d time.Duration) *idTokenClaims {
		return &idTokenClaims{Exp: json.Number(strconv.FormatInt(clock.Now().Add(d).Unix(), 10))}
	}
	if got := v.usedFor(claimsExpiringIn(10 * time.Minute)); got != 10*time.Minute+idTokenLeeway {
		t.Fatalf("usedFor = %v", got)
	}
	// 刚好在偏差边上的令牌也要登记一会儿,PX 不收 0。
	if got := v.usedFor(claimsExpiringIn(-idTokenLeeway)); got != time.Second {
		t.Fatalf("边界 usedFor = %v, want 1s", got)
	}
}
