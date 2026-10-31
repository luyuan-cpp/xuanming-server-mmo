package token

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Refresh 的提交语义(docs/design/client-request-ordering.md §6.1):作废旧 refresh token 与签发新的一对
// 是同一次原子提交;请求 ctx 已截止 / 已取消时一个字节都不写。全部用 miniredis,不依赖墙钟。

const (
	testAccount  = "acct-refresh"
	testAuthType = "password"
	testDevice   = "device-1"
)

var (
	testConfig = Config{AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: 720 * time.Hour}
	// testIndexKey 是 testAccount 的 refresh 索引键。
	testIndexKey = redisslot.AccountRefreshKey(testAccount)
)

// 用例里的键一律从令牌字符串经生产构造函数得到,键形本身由 redisslot 的金样钉住。

func mustParseToken(t *testing.T, token string) (tag, secret string) {
	t.Helper()
	tag, secret, ok := parseToken(token)
	if !ok {
		t.Fatalf("令牌 %q 格式不对", token)
	}
	return tag, secret
}

func accessKeyOf(t *testing.T, token string) string {
	t.Helper()
	return redisslot.AccessTokenKey(mustParseToken(t, token))
}

func refreshKeyOf(t *testing.T, token string) string {
	t.Helper()
	return redisslot.RefreshTokenKey(mustParseToken(t, token))
}

// secretOf 是令牌在账号索引里的成员值,也是家族 head 的取值。
func secretOf(t *testing.T, token string) string {
	t.Helper()
	_, secret := mustParseToken(t, token)
	return secret
}

func familyKeyOf(account, familyID string) string {
	return redisslot.RefreshFamilyKey(redisslot.AccountTag(account), familyID)
}

func newTestManager(t *testing.T) (*Manager, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewManager(rdb, testConfig), rdb, mr
}

// issueSeed 签出用例的起始 token 对(走 Login 用的 Issue,与生产同一条路径)。
func issueSeed(t *testing.T, m *Manager) *TokenPair {
	t.Helper()
	pair, err := m.Issue(context.Background(), testAccount, testAuthType, testDevice)
	if err != nil {
		t.Fatalf("seed Issue: %v", err)
	}
	return pair
}

// snapshot 把 miniredis 里全部键(值 + TTL;ZSET 带成员与 score)拍平成可比较的 map。
func snapshot(t *testing.T, mr *miniredis.Miniredis) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, k := range mr.Keys() {
		switch typ := mr.Type(k); typ {
		case "string":
			v, err := mr.Get(k)
			if err != nil {
				t.Fatalf("read %s: %v", k, err)
			}
			out[k] = fmt.Sprintf("string:%s ttl=%s", v, mr.TTL(k))
		case "zset":
			members, err := mr.ZMembers(k)
			if err != nil {
				t.Fatalf("read %s: %v", k, err)
			}
			parts := make([]string, 0, len(members))
			for _, member := range members {
				score, err := mr.ZScore(k, member)
				if err != nil {
					t.Fatalf("score %s/%s: %v", k, member, err)
				}
				parts = append(parts, fmt.Sprintf("%s=%v", member, score))
			}
			out[k] = fmt.Sprintf("zset:%s ttl=%s", strings.Join(parts, ","), mr.TTL(k))
		default:
			out[k] = typ
		}
	}
	return out
}

func keysWithPrefix(mr *miniredis.Miniredis, prefix string) []string {
	var out []string
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// requireRotated 断言旧 refresh token 已作废、新的一对已签出且挂上了 TTL 与集合成员。
func requireRotated(t *testing.T, mr *miniredis.Miniredis, old, got *TokenPair) {
	t.Helper()
	if got == nil {
		t.Fatal("rotation committed but Refresh returned no pair")
	}
	if mr.Exists(refreshKeyOf(t, old.RefreshToken)) {
		t.Fatal("old refresh token still valid after a committed rotation")
	}
	setKey := testIndexKey
	members, err := mr.ZMembers(setKey)
	if err != nil {
		t.Fatalf("read account set: %v", err)
	}
	if slices.Contains(members, secretOf(t, old.RefreshToken)) {
		t.Fatalf("old refresh token still in the account set: %v", members)
	}
	if !slices.Contains(members, secretOf(t, got.RefreshToken)) {
		t.Fatalf("new refresh token missing from the account set: %v", members)
	}
	if score, err := mr.ZScore(setKey, secretOf(t, got.RefreshToken)); err != nil || int64(score) != got.RefreshTokenExpire {
		t.Fatalf("new member score = %v/%v, want refresh expiry %d", score, err, got.RefreshTokenExpire)
	}
	if ttl := mr.TTL(setKey); ttl != testConfig.RefreshTokenTTL {
		t.Fatalf("account set ttl = %s, want %s", ttl, testConfig.RefreshTokenTTL)
	}
	for key, wantTTL := range map[string]time.Duration{
		accessKeyOf(t, got.AccessToken):   testConfig.AccessTokenTTL,
		refreshKeyOf(t, got.RefreshToken): testConfig.RefreshTokenTTL,
	} {
		raw, err := mr.Get(key)
		if err != nil {
			t.Fatalf("new token key %s missing: %v", key, err)
		}
		var data TokenData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatalf("unmarshal %s: %v", key, err)
		}
		if data.Account != testAccount || data.AuthType != testAuthType || data.DeviceID != testDevice {
			t.Fatalf("%s payload = %+v, want the rotated token's identity", key, data)
		}
		if ttl := mr.TTL(key); ttl != wantTTL {
			t.Fatalf("%s ttl = %s, want %s", key, ttl, wantTTL)
		}
	}
}

// 正常轮换:旧的作废、新的一对签出,集合里换成新成员。
func TestRefresh_RotatesOldTokenIntoNewPair(t *testing.T) {
	m, _, mr := newTestManager(t)
	old := issueSeed(t, m)

	got, err := m.Refresh(context.Background(), old.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	requireRotated(t, mr, old, got)
	if got.RefreshToken == old.RefreshToken || got.AccessToken == old.AccessToken {
		t.Fatal("rotation must mint fresh tokens")
	}

	// 一次性:同一枚旧 token 再刷一次必须失败,且不再签出任何东西。
	before := snapshot(t, mr)
	if _, err := m.Refresh(context.Background(), old.RefreshToken); err == nil {
		t.Fatal("a consumed refresh token was accepted twice")
	}
	if after := snapshot(t, mr); !maps.Equal(before, after) {
		t.Fatalf("refreshing a consumed token changed Redis:\nbefore=%v\nafter=%v", before, after)
	}
}

// 已取消 ctx → 不提交:旧 token 原样有效,不签出任何新 token。
func TestRefresh_CancelledContextWritesNothing(t *testing.T) {
	m, _, mr := newTestManager(t)
	old := issueSeed(t, m)
	before := snapshot(t, mr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pair, err := m.Refresh(ctx, old.RefreshToken); err == nil {
		t.Fatalf("Refresh on a cancelled ctx returned %+v, want an error", pair)
	}
	if after := snapshot(t, mr); !maps.Equal(before, after) {
		t.Fatalf("Refresh on a cancelled ctx changed Redis:\nbefore=%v\nafter=%v", before, after)
	}
}

// cancelAfterHook 在第 after 条命令(单条或一次 pipeline 各算一条)返回之后取消请求 ctx,
// 模拟 zrpc 服务端 Timeout 恰好落在两条命令之间。
type cancelAfterHook struct {
	after  int
	seen   int
	cancel context.CancelFunc
}

func (h *cancelAfterHook) tick() {
	h.seen++
	if h.seen == h.after {
		h.cancel()
	}
}

func (h *cancelAfterHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *cancelAfterHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		h.tick()
		return err
	}
}

func (h *cancelAfterHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		h.tick()
		return err
	}
}

// 回归:请求截止落在任意两条命令之间,结局都只能是「什么都没变」或「完整轮换」二者之一。
// 修复前作废(DEL)与签发(MULTI)是分开的往返:截止落在 DEL 之后,旧 token 已作废、新的没签出,
// 客户端只能回去走第三方重新认证 —— cut=2 时这条用例在修复前必红。
func TestRefresh_AllOrNothingAtEveryCommandBoundary(t *testing.T) {
	for cut := 1; cut <= 8; cut++ {
		t.Run(fmt.Sprintf("cancel after command %d", cut), func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			old := issueSeed(t, m)
			before := snapshot(t, mr)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rdb.AddHook(&cancelAfterHook{after: cut, cancel: cancel})

			got, err := m.Refresh(ctx, old.RefreshToken)
			if err != nil {
				if after := snapshot(t, mr); !maps.Equal(before, after) {
					t.Fatalf("Refresh failed (%v) but left a partial write:\nbefore=%v\nafter=%v", err, before, after)
				}
				return
			}
			requireRotated(t, mr, old, got)
		})
	}
}

// 并发消费:GET 之后、脚本之前另一次刷新已经消费了这枚 token → 按值比对失败,整段不写。
func TestRefresh_ConsumedBetweenReadAndCommitWritesNothing(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	old := issueSeed(t, m)
	oldKey := refreshKeyOf(t, old.RefreshToken)
	rdb.AddHook(&afterFirstCommand{do: func() { mr.Del(oldKey) }})
	before := snapshot(t, mr)
	delete(before, oldKey)

	if pair, err := m.Refresh(context.Background(), old.RefreshToken); err == nil {
		t.Fatalf("Refresh issued %+v for a token consumed concurrently", pair)
	}
	if after := snapshot(t, mr); !maps.Equal(before, after) {
		t.Fatalf("a lost race must write nothing:\nbefore=%v\nafter=%v", before, after)
	}
	if got := keysWithPrefix(mr, "refresh_token:"); len(got) != 0 {
		t.Fatalf("refresh keys after a lost race = %v, want none", got)
	}
}

// afterFirstCommand 在第一条命令(Refresh 的 GET)返回之后执行一次 do。
type afterFirstCommand struct {
	done bool
	do   func()
}

func (h *afterFirstCommand) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterFirstCommand) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if !h.done {
			h.done = true
			h.do()
		}
		return err
	}
}

func (h *afterFirstCommand) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// 非正 TTL 在提交前就拒绝:不签出永不过期的 token,旧 token 原样有效。
func TestRefresh_NonPositiveTTLWritesNothing(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	old := issueSeed(t, m)
	before := snapshot(t, mr)

	for _, cfg := range []Config{
		{AccessTokenTTL: 0, RefreshTokenTTL: testConfig.RefreshTokenTTL},
		{AccessTokenTTL: testConfig.AccessTokenTTL, RefreshTokenTTL: -time.Second},
	} {
		if pair, err := NewManager(rdb, cfg).Refresh(context.Background(), old.RefreshToken); err == nil {
			t.Fatalf("cfg %+v: Refresh returned %+v, want an error", cfg, pair)
		}
	}
	if after := snapshot(t, mr); !maps.Equal(before, after) {
		t.Fatalf("a rejected Refresh changed Redis:\nbefore=%v\nafter=%v", before, after)
	}
}
