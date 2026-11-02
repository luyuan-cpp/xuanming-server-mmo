package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// Refresh 的提交语义(docs/design/client-request-ordering.md §6.1):作废旧 refresh token 与签发新的一对
// 是同一次原子提交;请求 ctx 已截止 / 已取消时一个字节都不写。全部用 miniredis,不依赖墙钟。
// 本包用例共用进程级的 Prometheus 计数器、按增量断言,不能加 t.Parallel()。

const (
	testAccount  = "acct-refresh"
	testAuthType = "password"
	testDevice   = "device-1"
	testGrace    = 30 * time.Second
)

var (
	// testConfig 的宽限窗口是 0:轮换不写记录,旧 refresh 再来就是重用。
	testConfig = Config{AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: 720 * time.Hour}
	// graceConfig 开着宽限窗口,其余同 testConfig。
	graceConfig = Config{AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: 720 * time.Hour, RefreshReuseGrace: testGrace}
	// testIndexKey 是 testAccount 的 refresh 索引键。
	testIndexKey = redisslot.AccountRefreshKey(testAccount)
)

// 用例里的键一律从令牌字符串经生产构造函数得到,键形本身由 redisslot 的金样钉住。

func parsedOf(t *testing.T, token string) parsedToken {
	t.Helper()
	parsed, ok := parseToken(token)
	if !ok {
		t.Fatalf("令牌 %q 格式不对", token)
	}
	return parsed
}

func mustParseToken(t *testing.T, token string) (tag, secret string) {
	t.Helper()
	parsed := parsedOf(t, token)
	return parsed.tag, parsed.secret
}

func accessKeyOf(t *testing.T, token string) string {
	t.Helper()
	return redisslot.AccessTokenKey(mustParseToken(t, token))
}

func refreshKeyOf(t *testing.T, token string) string {
	t.Helper()
	return redisslot.RefreshTokenKey(mustParseToken(t, token))
}

// rotatedKeyOf 是一枚 refresh token 被轮换后留下的轮换记录的键。
func rotatedKeyOf(t *testing.T, token string) string {
	t.Helper()
	return redisslot.RefreshRotatedKey(mustParseToken(t, token))
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

// familyKeyOfToken 是令牌所属家族的记录键。
func familyKeyOfToken(t *testing.T, token string) string {
	t.Helper()
	parsed := parsedOf(t, token)
	return redisslot.RefreshFamilyKey(parsed.tag, parsed.familyID)
}

func newManagerWith(t *testing.T, cfg Config) (*Manager, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewManager(rdb, cfg), rdb, mr
}

func newTestManager(t *testing.T) (*Manager, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	return newManagerWith(t, testConfig)
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

func mustRefresh(t *testing.T, m *Manager, refreshToken string) *TokenPair {
	t.Helper()
	pair, err := m.Refresh(context.Background(), refreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return pair
}

// requireRejected 断言 Refresh 是业务拒绝(客户端看到的码与普通拒绝相同),错误里没有令牌与家族号原值。
func requireRejected(t *testing.T, m *Manager, refreshToken string) {
	t.Helper()
	pair, err := m.Refresh(context.Background(), refreshToken)
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("Refresh = (%+v, %v), want ErrRefreshRejected", pair, err)
	}
	requireNoSecrets(t, err.Error(), refreshToken)
}

// requireDamaged 断言 Refresh 按存储故障或数据损坏失败:不是业务拒绝,只计一次 error,错误里没有令牌与家族号原值。
func requireDamaged(t *testing.T, m *Manager, refreshToken string) {
	t.Helper()
	var pair *TokenPair
	var err error
	requireOnlyResult(t, "error", func() { pair, err = m.Refresh(context.Background(), refreshToken) })
	if err == nil || errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("Refresh = (%+v, %v), want 按损坏报错", pair, err)
	}
	requireNoSecrets(t, err.Error(), refreshToken)
}

// requireNoSecrets 断言文本里没有这些令牌的原值。secret 的前 21 个字符就是家族号的前 21 个字符
// (第 22 个字符的低位被随机半段占了),按这一段核对能同时挡住家族号原值和 secret。
func requireNoSecrets(t *testing.T, text string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if token != "" && strings.Contains(text, token) {
			t.Fatalf("文本里出现了令牌原值: %s", text)
		}
		if parsed, ok := parseToken(token); ok && strings.Contains(text, parsed.familyID[:21]) {
			t.Fatalf("文本里出现了 secret 或家族号原值: %s", text)
		}
	}
}

func requireAccessValid(t *testing.T, m *Manager, accessToken string) {
	t.Helper()
	if _, err := m.ValidateAccess(context.Background(), accessToken); err != nil {
		t.Fatalf("access 令牌被拒绝: %v", err)
	}
}

func requireAccessInvalid(t *testing.T, m *Manager, accessToken string) {
	t.Helper()
	data, err := m.ValidateAccess(context.Background(), accessToken)
	if err == nil {
		t.Fatalf("access 令牌仍然有效: %+v", data)
	}
	requireNoSecrets(t, err.Error(), accessToken)
}

// tokenMetrics 只登记本包的两个指标,读数不受默认注册表里别的采集器影响。
// 不用 prometheus/testutil:它依赖的 kylelemons/godebug 不在 login 的 go.mod 里,不为测试加依赖。
var tokenMetrics = newTokenMetrics()

func newTokenMetrics() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(refreshTotal, reuseDelay)
	return registry
}

// refreshResultCounts 读 login_refresh_token_total 每个 result 当前的累计值;还没计过数的 result 不出现。
func refreshResultCounts(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := tokenMetrics.Gather()
	if err != nil {
		t.Fatalf("读指标: %v", err)
	}
	counts := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != "login_refresh_token_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" {
					counts[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	return counts
}

// refreshResultDelta 返回 f 执行期间 login_refresh_token_total 的增量,没变的 result 不列。
func refreshResultDelta(t *testing.T, f func()) map[string]float64 {
	t.Helper()
	before := refreshResultCounts(t)
	f()
	delta := make(map[string]float64)
	for result, count := range refreshResultCounts(t) {
		if d := count - before[result]; d != 0 {
			delta[result] = d
		}
	}
	return delta
}

// requireOnlyResult 断言 f 恰好计了一次 result,别的结果一个都没动。
func requireOnlyResult(t *testing.T, result string, f func()) {
	t.Helper()
	if delta := refreshResultDelta(t, f); len(delta) != 1 || delta[result] != 1 {
		t.Fatalf("login_refresh_token_total 增量 = %v, want 只有 %s 加 1", delta, result)
	}
}

// reuseDelaySamples 读 login_refresh_reuse_delay_seconds{replayed} 当前的样本数。
func reuseDelaySamples(t *testing.T, replayed string) uint64 {
	t.Helper()
	families, err := tokenMetrics.Gather()
	if err != nil {
		t.Fatalf("读指标: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "login_refresh_reuse_delay_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "replayed" && label.GetValue() == replayed {
					return metric.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
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

// requireUnchanged 断言 Redis 与 before(familySnapshot 拍的)一模一样:值、TTL、hash 字段都没动。
func requireUnchanged(t *testing.T, mr *miniredis.Miniredis, before map[string]string) {
	t.Helper()
	if after := familySnapshot(t, mr); !maps.Equal(before, after) {
		t.Fatalf("Redis 被改了:\nbefore=%v\nafter=%v", before, after)
	}
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

// requireRotated 断言旧 refresh token 已作废、新的一对已签出且挂上了 TTL 与集合成员,家族推进到新的头。
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
	family := parsedOf(t, old.RefreshToken).familyID
	for _, token := range []string{got.AccessToken, got.RefreshToken} {
		if parsedOf(t, token).familyID != family {
			t.Fatalf("轮换换了家族: %q 不属于旧 token 的家族", token)
		}
	}
	familyKey := familyKeyOfToken(t, got.RefreshToken)
	if mr.HGet(familyKey, "head") != secretOf(t, got.RefreshToken) || mr.HGet(familyKey, "prev") != secretOf(t, old.RefreshToken) {
		t.Fatal("轮换没有把家族的 head / prev 推进到新的一对")
	}
	for key, wantTTL := range map[string]time.Duration{
		accessKeyOf(t, got.AccessToken):   testConfig.AccessTokenTTL,
		refreshKeyOf(t, got.RefreshToken): testConfig.RefreshTokenTTL,
		familyKey:                         testConfig.RefreshTokenTTL,
	} {
		if ttl := mr.TTL(key); ttl != wantTTL {
			t.Fatalf("%s ttl = %s, want %s", key, ttl, wantTTL)
		}
		if key == familyKey {
			continue
		}
		raw, err := mr.Get(key)
		if err != nil {
			t.Fatalf("new token key %s missing: %v", key, err)
		}
		var data TokenData
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatalf("unmarshal %s: %v", key, err)
		}
		if data.Account != testAccount || data.AuthType != testAuthType || data.DeviceID != testDevice || data.FamilyID != family {
			t.Fatalf("%s payload = %+v, want the rotated token's identity", key, data)
		}
	}
}

// 正常轮换:旧的作废、新的一对签出,集合里换成新成员。窗口为 0 不写轮换记录。
func TestRefresh_RotatesOldTokenIntoNewPair(t *testing.T) {
	m, _, mr := newTestManager(t)
	old := issueSeed(t, m)

	var got *TokenPair
	requireOnlyResult(t, "rotated", func() { got = mustRefresh(t, m, old.RefreshToken) })
	requireRotated(t, mr, old, got)
	if got.RefreshToken == old.RefreshToken || got.AccessToken == old.AccessToken {
		t.Fatal("rotation must mint fresh tokens")
	}
	if keys := keysWithPrefix(mr, "refresh_rotated:"); len(keys) != 0 {
		t.Fatalf("窗口为 0 不该写轮换记录: %v", keys)
	}
}

// 已取消 ctx → 不提交:旧 token 原样有效,不签出任何新 token。
func TestRefresh_CancelledContextWritesNothing(t *testing.T) {
	m, _, mr := newTestManager(t)
	old := issueSeed(t, m)
	before := familySnapshot(t, mr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pair, err := m.Refresh(ctx, old.RefreshToken); err == nil {
		t.Fatalf("Refresh on a cancelled ctx returned %+v, want an error", pair)
	}
	requireUnchanged(t, mr, before)
}

// 读到旧值之后请求才截止:不发轮换脚本,计 abandoned,一个字节都不写,旧 token 留着让客户端重试。
func TestRefresh_AbandonedBeforeCommitWritesNothing(t *testing.T) {
	m, rdb, mr := newManagerWith(t, graceConfig)
	old := issueSeed(t, m)
	before := familySnapshot(t, mr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdb.AddHook(&afterFirstCommand{do: cancel})

	var err error
	requireOnlyResult(t, "abandoned", func() { _, err = m.Refresh(ctx, old.RefreshToken) })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("err = %v, want 截止错误而不是业务拒绝", err)
	}
	requireUnchanged(t, mr, before)
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
// 客户端只能回去走第三方重新认证 —— cut=2 时这条用例在修复前必红。窗口开着时轮换记录也在这次提交里。
func TestRefresh_AllOrNothingAtEveryCommandBoundary(t *testing.T) {
	for _, cfg := range []Config{testConfig, graceConfig} {
		for cut := 1; cut <= 8; cut++ {
			t.Run(fmt.Sprintf("grace=%s cancel after command %d", cfg.RefreshReuseGrace, cut), func(t *testing.T) {
				m, rdb, mr := newManagerWith(t, cfg)
				old := issueSeed(t, m)
				before := familySnapshot(t, mr)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				rdb.AddHook(&cancelAfterHook{after: cut, cancel: cancel})

				got, err := m.Refresh(ctx, old.RefreshToken)
				if err != nil {
					requireUnchanged(t, mr, before)
					return
				}
				requireRotated(t, mr, old, got)
				if mr.Exists(rotatedKeyOf(t, old.RefreshToken)) != (cfg.RefreshReuseGrace > 0) {
					t.Fatal("轮换记录与窗口配置不符")
				}
			})
		}
	}
}

// GET 之后、脚本之前这枚 token 的键没了(外部删除或恰好过期),家族头还指着它:按值比对失败,整段不写;
// 转重放判定,重放的就是头自己,普通拒绝,不吊销。
func TestRefresh_KeyVanishedBetweenReadAndCommitWritesNothing(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	old := issueSeed(t, m)
	oldKey := refreshKeyOf(t, old.RefreshToken)
	rdb.AddHook(&afterFirstCommand{do: func() { mr.Del(oldKey) }})
	before := familySnapshot(t, mr)
	delete(before, oldKey)

	requireOnlyResult(t, "rejected", func() { requireRejected(t, m, old.RefreshToken) })
	requireUnchanged(t, mr, before)
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
	before := familySnapshot(t, mr)

	for _, cfg := range []Config{
		{AccessTokenTTL: 0, RefreshTokenTTL: testConfig.RefreshTokenTTL},
		{AccessTokenTTL: testConfig.AccessTokenTTL, RefreshTokenTTL: -time.Second},
	} {
		requireDamaged(t, NewManager(rdb, cfg), old.RefreshToken)
	}
	requireUnchanged(t, mr, before)
}

// 签发的输入、配置或请求不合法时一个字节都不写。
func TestIssue_RejectsBadInputWithoutWriting(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	for _, tc := range []struct{ account, authType string }{{"", testAuthType}, {testAccount, ""}} {
		if _, err := m.Issue(ctx, tc.account, tc.authType, testDevice); err == nil {
			t.Fatalf("Issue(%q, %q) 不该成功", tc.account, tc.authType)
		}
	}
	subMillisecond := NewManager(rdb, Config{AccessTokenTTL: time.Microsecond, RefreshTokenTTL: time.Hour})
	if _, err := subMillisecond.Issue(ctx, testAccount, testAuthType, testDevice); err == nil {
		t.Fatal("亚毫秒 TTL 不该签发成功")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Issue(cancelled, testAccount, testAuthType, testDevice); err == nil {
		t.Fatal("请求已取消不该签发成功")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("被拒绝的签发写了 %v", keys)
	}
}

// 配错的令牌配置拒绝启动,不静默截断:窗口越界、亚毫秒(含漏写单位被解析成纳秒)、TTL 不可用、refresh 短于 access。
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"默认值", Config{AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: 720 * time.Hour, RefreshReuseGrace: 30 * time.Second}, true},
		{"窗口取上限", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: MaxRefreshReuseGrace}, true},
		{"窗口为 0", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour}, true},
		{"窗口 1 毫秒", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: time.Millisecond}, true},
		{"refresh 等于 access", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour}, true},
		{"窗口为负", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: -time.Second}, false},
		{"窗口超过上限", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: MaxRefreshReuseGrace + time.Millisecond}, false},
		{"窗口不到 1 毫秒", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 500 * time.Microsecond}, false},
		{"窗口漏写单位被解析成纳秒", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 30}, false},
		{"窗口不是整毫秒", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour, RefreshReuseGrace: 1500 * time.Microsecond}, false},
		{"access TTL 为 0", Config{RefreshTokenTTL: time.Hour}, false},
		{"access TTL 不到 1 毫秒", Config{AccessTokenTTL: time.Microsecond, RefreshTokenTTL: time.Hour}, false},
		{"refresh TTL 为 0", Config{AccessTokenTTL: time.Hour}, false},
		{"refresh TTL 为负", Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: -time.Hour}, false},
		{"refresh 短于 access", Config{AccessTokenTTL: 2 * time.Hour, RefreshTokenTTL: time.Hour}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
