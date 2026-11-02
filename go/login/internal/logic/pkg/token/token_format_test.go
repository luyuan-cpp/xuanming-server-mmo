package token

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 令牌字符串 <AccountTag>.<secret> 与「一个账号的键全部同槽」的契约(docs/design/redis-player-store-cluster.md §10.2)。
// secret 的前 16 字节是家族号,随机半段首字节最高位是类型位(docs/design/dual-token-authentication.md「令牌格式与键布局」)。

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
	for name, tc := range map[string]struct {
		token string
		kind  tokenKind
	}{
		"access":  {pair.AccessToken, accessKind},
		"refresh": {pair.RefreshToken, refreshKind},
	} {
		parsed, ok := parseToken(tc.token)
		if !ok {
			t.Fatalf("%s 令牌 %q 解析失败", name, tc.token)
		}
		if parsed.tag != wantTag {
			t.Fatalf("%s 令牌 tag = %q, want %q", name, parsed.tag, wantTag)
		}
		if len(parsed.secret) != 43 {
			t.Fatalf("%s 令牌 secret 长度 = %d, want 43", name, len(parsed.secret))
		}
		if tc.token != wantTag+"."+parsed.secret {
			t.Fatalf("%s 令牌形状 = %q", name, tc.token)
		}
		if parsed.kind != tc.kind {
			t.Fatalf("%s 令牌类型位 = %v, want %v", name, parsed.kind, tc.kind)
		}
	}
	if !mr.Exists(redisslot.AccessTokenKey(mustParseToken(t, pair.AccessToken))) || !mr.Exists(redisslot.RefreshTokenKey(mustParseToken(t, pair.RefreshToken))) {
		t.Fatalf("令牌键不在预期位置: %v", mr.Keys())
	}
	// 一对令牌嵌同一个家族号,家族记录就在这个号下面。
	family := parsedOf(t, pair.RefreshToken).familyID
	if parsedOf(t, pair.AccessToken).familyID != family || !mr.Exists(familyKeyOf(testAccount, family)) {
		t.Fatalf("令牌里的家族号对不上家族记录: %v", mr.Keys())
	}
	if data, err := m.ValidateAccess(context.Background(), pair.AccessToken); err != nil || data.Account != testAccount {
		t.Fatalf("ValidateAccess = %+v, %v", data, err)
	}
}

// 前 16 字节是家族号,随机半段首字节的最高位是类型位,不管随机数取到什么。
func TestNewSecretEmbedsFamilyAndKind(t *testing.T) {
	family, err := generateOpaque(familyByteLen)
	if err != nil {
		t.Fatal(err)
	}
	tag := redisslot.AccountTag(testAccount)
	for range 64 {
		for _, kind := range []tokenKind{accessKind, refreshKind} {
			secret, err := newSecret(family, kind)
			if err != nil {
				t.Fatal(err)
			}
			parsed, ok := parseToken(formatToken(tag, secret))
			if !ok || parsed.familyID != family || parsed.kind != kind || parsed.secret != secret {
				t.Fatalf("newSecret(%v) 解析成 %+v / %v", kind, parsed, ok)
			}
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("!", 22)} {
		if _, err := newSecret(bad, refreshKind); err == nil {
			t.Fatalf("家族号 %q 不该生成令牌", bad)
		}
	}
}

// malformedVariants 从一枚合法令牌的两段派生不合规的形状。
func malformedVariants(tag, secret string) map[string]string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	// 43 字符的末位只有高 4 位有效,规范编码的低 2 位是 0;把最低位置 1,解码结果不变,字符串不再规范。
	nonCanonical := secret[:42] + string(alphabet[strings.IndexByte(alphabet, secret[42])|1])
	token := tag + "." + secret
	return map[string]string{
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
		"secret 太长":       token + "A",
		"secret 非法字符":     tag + "." + strings.Repeat("!", 43),
		"secret 中间带换行":    tag + "." + secret[:20] + "\n" + secret[20:],
		"secret 中间带回车":    tag + "." + secret[:20] + "\r" + secret[20:],
		"secret 末尾带换行":    token + "\n",
		"secret 末尾带回车换行":  token + "\r\n",
		"secret 末尾带等号":    token + "=",
		"secret 非规范编码":    tag + "." + nonCanonical,
		"开头带回车换行":         "\r\n" + token,
		"两端带空格":           " " + token + " ",
		"多一段":             token + ".x",
		"tag 与 secret 反了": secret + "." + tag,
	}
}

// 令牌是客户端输入。格式不对按无效令牌,不拼键、不访问 Redis,也不会被当成家族里的旧成员而吊销家族。
// 变体由同类型的合法令牌派生:这样拦住它们的只能是格式检查,不是类型位。
func TestMalformedTokenIsInvalidWithoutTouchingRedis(t *testing.T) {
	m, rdb, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	before := familySnapshot(t, mr)
	counter := &commandCounter{}
	rdb.AddHook(counter)

	ctx := context.Background()
	accessTag, accessSecret := mustParseToken(t, pair.AccessToken)
	for name, token := range malformedVariants(accessTag, accessSecret) {
		t.Run("access/"+name, func(t *testing.T) {
			if _, err := m.ValidateAccess(ctx, token); !errors.Is(err, errMalformedToken) {
				t.Fatalf("ValidateAccess err = %v, want errMalformedToken", err)
			}
		})
	}
	refreshTag, refreshSecret := mustParseToken(t, pair.RefreshToken)
	for name, token := range malformedVariants(refreshTag, refreshSecret) {
		t.Run("refresh/"+name, func(t *testing.T) {
			requireOnlyResult(t, "rejected", func() { requireRejected(t, m, token) })
		})
	}
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("格式不对的令牌发出了 %d 条 Redis 命令", n)
	}
	requireUnchanged(t, mr, before)
}

// 类型位:把 access 当 refresh 发来(客户端字段写反),或把 refresh 当 access 用,在碰 Redis 之前就普通拒绝,
// 不会被当成家族里的旧成员而吊销家族;access 已经过期时也一样。
func TestTokenKindMismatchIsPlainRejection(t *testing.T) {
	m, rdb, mr := newGuardedManager(t)
	pair := issueSeed(t, m)
	before := familySnapshot(t, mr)
	counter := &commandCounter{}
	rdb.AddHook(counter)

	requireOnlyResult(t, "rejected", func() { requireRejected(t, m, pair.AccessToken) })
	if _, err := m.ValidateAccess(context.Background(), pair.RefreshToken); !errors.Is(err, errMalformedToken) {
		t.Fatalf("把 refresh 当 access 用: err = %v, want errMalformedToken", err)
	}
	if n := counter.n.Load(); n != 0 {
		t.Fatalf("类型不符的令牌发出了 %d 条 Redis 命令", n)
	}
	requireUnchanged(t, mr, before)

	mr.FastForward(testConfig.AccessTokenTTL + time.Second)
	requireOnlyResult(t, "rejected", func() { requireRejected(t, m, pair.AccessToken) })
	// 家族没有被牵连:头照常轮换。
	requireOnlyResult(t, "rotated", func() { mustRefresh(t, m, pair.RefreshToken) })
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
	requireOnlyResult(t, "rejected", func() { requireRejected(t, m, otherTag+"."+secretOf(t, pair.RefreshToken)) })
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
	requireDamaged(t, m, pair.RefreshToken)
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("拒绝不得修改原 token / family / 索引")
	}
}

// 发放、校验、轮换、宽限重放、重用吊销、封顶淘汰、撤销全部走一遍。守卫保证每段脚本声明的键同槽(不同槽的操作会报错);
// 这里再核对脚本现拼的键:Redis 里留下的每个键(含轮换记录)都在账号的槽里,吊销与封顶淘汰删得掉现拼的 refresh、家族与索引成员。
func TestTokenLifecycleKeysStayInTheAccountSlot(t *testing.T) {
	for _, account := range []string{testAccount, "a{b}c", "}{", "{42}", "玩家甲"} {
		t.Run(account, func(t *testing.T) {
			m, _, mr := newGuardedManager(t)
			m.cfg = graceConfig
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
			// 轮换记录确实写了,上面那次核对才算把它算进去。
			if got := len(keysWithPrefix(mr, "refresh_rotated:")); got != 1 {
				t.Fatalf("轮换之后轮换记录 %d 条, want 1", got)
			}
			if _, err := m.ValidateAccess(ctx, rotated.AccessToken); err != nil {
				t.Fatalf("ValidateAccess after rotation: %v", err)
			}
			// 宽限重放:脚本现拼后继的 refresh 键。
			if replayed, err := m.Refresh(ctx, pair.RefreshToken); err != nil || *replayed != *rotated {
				t.Fatalf("窗口内重放 = %+v, %v, want 同一对", replayed, err)
			}

			// 再轮换一次,旧 refresh 再来就是重用:吊销脚本现拼头的键与账号索引键,要真的删掉。
			head, err := m.Refresh(ctx, rotated.RefreshToken)
			if err != nil {
				t.Fatalf("second Refresh: %v", err)
			}
			requireAccountSlot("second refresh")
			if _, err := m.Refresh(ctx, pair.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
				t.Fatalf("已轮换走的旧 refresh: err = %v, want ErrRefreshRejected", err)
			}
			requireAccountSlot("reuse")
			if got := len(keysWithPrefix(mr, "refresh_token:")) + len(keysWithPrefix(mr, "refresh_family:")); got != 0 {
				t.Fatalf("重用吊销之后还剩 refresh / family 键: %v", mr.Keys())
			}
			if _, err := m.ValidateAccess(ctx, head.AccessToken); err == nil {
				t.Fatal("家族吊销之后 access 令牌不得通过")
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
