package token

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"

	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 令牌字符串 <AccountTag>.<secret> 与「一个账号的键全部同槽」的契约(docs/design/redis-player-store-cluster.md §10.2)。

// newGuardedManager 用生产的构造器建句柄:守卫在发送前拒绝不带 tag、跨槽的多键命令,任何一段脚本声明的键不同槽,操作直接报错。
func newGuardedManager(t *testing.T) (*Manager, redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb, err := playerredis.New(playerredis.Config{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return NewManager(rdb, testConfig), rdb, mr
}

// commandCounter 数经过客户端的命令,用来钉住「格式不对不访问 Redis」。
type commandCounter struct{ n atomic.Int64 }

func (c *commandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *commandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.n.Add(1)
		return next(ctx, cmd)
	}
}

func (c *commandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.n.Add(int64(len(cmds)))
		return next(ctx, cmds)
	}
}

func TestIssuedTokensCarryTheAccountTag(t *testing.T) {
	m, _, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	wantTag := redisslot.AccountTag(testAccount)
	for name, token := range map[string]string{"access": pair.AccessToken, "refresh": pair.RefreshToken} {
		tag, secret, ok := parseToken(token)
		if !ok {
			t.Fatalf("%s 令牌 %q 解析失败", name, token)
		}
		if tag != wantTag {
			t.Fatalf("%s 令牌 tag = %q, want %q", name, tag, wantTag)
		}
		if len(secret) != 43 {
			t.Fatalf("%s 令牌 secret 长度 = %d, want 43", name, len(secret))
		}
		if token != wantTag+"."+secret {
			t.Fatalf("%s 令牌形状 = %q", name, token)
		}
	}
	if !mr.Exists(redisslot.AccessTokenKey(mustParseToken(t, pair.AccessToken))) || !mr.Exists(redisslot.RefreshTokenKey(mustParseToken(t, pair.RefreshToken))) {
		t.Fatalf("令牌键不在预期位置: %v", mr.Keys())
	}
	if data, err := m.ValidateAccess(context.Background(), pair.AccessToken); err != nil || data.Account != testAccount {
		t.Fatalf("ValidateAccess = %+v, %v", data, err)
	}
}

// 令牌是客户端输入。格式不对按无效令牌,不拼键、不访问 Redis。
func TestMalformedTokenIsInvalidWithoutTouchingRedis(t *testing.T) {
	m, rdb, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	tag, secret := mustParseToken(t, pair.AccessToken)
	before := familySnapshot(t, mr)
	counter := &commandCounter{}
	rdb.AddHook(counter)

	cases := map[string]string{
		"空串":              "",
		"只有分隔符":           ".",
		"没有分隔符":           tag + secret,
		"只有 secret":       secret,
		"只有 tag":          tag + ".",
		"缺 tag":           "." + secret,
		"tag 大写":          strings.ToUpper(tag) + "." + secret,
		"tag 不规范":         "zzzz." + secret,
		"tag 带花括号":        "{" + tag + "}." + secret,
		"tag 想闭合键里的花括号":   tag + "}{x." + secret,
		"tag 太长":          "abcde." + secret,
		"secret 太短":       tag + "." + secret[:42],
		"secret 太长":       tag + "." + secret + "A",
		"secret 非法字符":     tag + "." + strings.Repeat("!", 43),
		"secret 带换行":      tag + "." + secret[:20] + "\n" + secret[20:],
		"多一段":             tag + "." + secret + ".x",
		"tag 与 secret 反了": secret + "." + tag,
	}
	ctx := context.Background()
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := m.ValidateAccess(ctx, token); !errors.Is(err, errMalformedToken) {
				t.Fatalf("ValidateAccess err = %v, want errMalformedToken", err)
			}
			if _, err := m.Refresh(ctx, token); !errors.Is(err, errMalformedToken) {
				t.Fatalf("Refresh err = %v, want errMalformedToken", err)
			}
		})
	}
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("格式不对的令牌发出了 %d 条 Redis 命令", n)
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("格式不对的令牌改了 Redis")
	}
}

// tag 合法但不是这枚令牌的:键不存在,按无效令牌,原令牌不受影响。
func TestTokenWithAnotherAccountTagIsInvalid(t *testing.T) {
	m, _, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	otherTag := redisslot.AccountTag("foreign-account")
	if otherTag == redisslot.AccountTag(testAccount) {
		t.Fatal("夹具的两个账号必须不同 tag")
	}
	before := familySnapshot(t, mr)
	ctx := context.Background()
	if _, err := m.ValidateAccess(ctx, otherTag+"."+secretOf(t, pair.AccessToken)); err == nil {
		t.Fatal("换了 tag 的 access 令牌不得通过")
	}
	if _, err := m.Refresh(ctx, otherTag+"."+secretOf(t, pair.RefreshToken)); err == nil {
		t.Fatal("换了 tag 的 refresh 令牌不得轮换")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("换了 tag 的令牌改了 Redis")
	}
	if _, err := m.ValidateAccess(ctx, pair.AccessToken); err != nil {
		t.Fatalf("原令牌不受影响: %v", err)
	}
}

// 载荷账号与令牌 tag 不一致:账号索引键按账号算槽,家族键按 tag 算槽,这样的载荷进不了同一段脚本,在发脚本之前拒绝。
func TestTokenPayloadAccountMustMatchTheTag(t *testing.T) {
	m, rdb, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	ctx := context.Background()
	for _, key := range []string{accessKeyOf(t, pair.AccessToken), refreshKeyOf(t, pair.RefreshToken)} {
		raw, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			t.Fatal(err)
		}
		var data TokenData
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		data.Account = "foreign-account"
		forged, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.Set(ctx, key, forged, testConfig.RefreshTokenTTL).Err(); err != nil {
			t.Fatal(err)
		}
	}
	before := familySnapshot(t, mr)
	if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
		t.Fatal("载荷账号与 tag 不一致的 access 令牌不得通过")
	}
	if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("载荷账号与 tag 不一致的 refresh 令牌不得轮换")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("拒绝不得修改原 token / family / 索引")
	}
}

// 发放、校验、轮换、封顶淘汰、撤销全部走一遍。守卫保证每段脚本声明的键同槽(不同槽的操作会报错);
// 这里再核对脚本现拼的键:Redis 里留下的每个键都在账号的槽里,封顶淘汰删得掉现拼的 refresh 与 family 键。
func TestTokenLifecycleKeysStayInTheAccountSlot(t *testing.T) {
	for _, account := range []string{testAccount, "a{b}c", "}{", "{42}", "玩家甲"} {
		t.Run(account, func(t *testing.T) {
			m, _, mr := newGuardedManager(t)
			ctx := context.Background()
			wantSlot := redisslot.HashSlot("{" + redisslot.AccountTag(account) + "}")
			requireAccountSlot := func(step string) {
				t.Helper()
				for _, key := range mr.Keys() {
					if !redisslot.HasHashTag(key) {
						t.Fatalf("%s: 键 %q 没带 hash tag", step, key)
					}
					if got := redisslot.HashSlot(key); got != wantSlot {
						t.Fatalf("%s: 键 %q 在 slot %d, want %d", step, key, got, wantSlot)
					}
				}
			}

			pair, err := m.Issue(ctx, account, testAuthType, testDevice)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			requireAccountSlot("issue")
			if _, err := m.ValidateAccess(ctx, pair.AccessToken); err != nil {
				t.Fatalf("ValidateAccess: %v", err)
			}
			rotated, err := m.Refresh(ctx, pair.RefreshToken)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			requireAccountSlot("refresh")
			if _, err := m.ValidateAccess(ctx, rotated.AccessToken); err != nil {
				t.Fatalf("ValidateAccess after rotation: %v", err)
			}
			// 一次性轮换:旧 refresh 不能再用。
			if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
				t.Fatal("已消费的 refresh 令牌不得再次轮换")
			}

			// 账号封顶:多签几枚,超出的被淘汰,现拼的 refresh 与 family 键都要真的删掉。
			for i := 0; i < maxRefreshTokensPerAccount+2; i++ {
				if _, err := m.Issue(ctx, account, testAuthType, testDevice); err != nil {
					t.Fatalf("Issue #%d: %v", i, err)
				}
			}
			requireAccountSlot("trim")
			if got := len(keysWithPrefix(mr, "refresh_token:")); got != maxRefreshTokensPerAccount {
				t.Fatalf("封顶之后 refresh 键 %d 个, want %d", got, maxRefreshTokensPerAccount)
			}
			if got := len(keysWithPrefix(mr, "refresh_family:")); got != maxRefreshTokensPerAccount {
				t.Fatalf("封顶之后 family 键 %d 个, want %d", got, maxRefreshTokensPerAccount)
			}

			if err := m.RevokeAll(ctx, account); err != nil {
				t.Fatalf("RevokeAll: %v", err)
			}
			requireAccountSlot("revoke")
			if got := len(keysWithPrefix(mr, "refresh_token:")) + len(keysWithPrefix(mr, "refresh_family:")); got != 0 {
				t.Fatalf("撤销之后还剩 refresh / family 键: %v", mr.Keys())
			}
			if _, err := m.ValidateAccess(ctx, rotated.AccessToken); err == nil {
				t.Fatal("家族撤销之后 access 令牌不得通过")
			}
		})
	}
}
