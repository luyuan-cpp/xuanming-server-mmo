package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shared/playerredis"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 家族模型、宽限窗口(docs/design/dual-token-authentication.md「轮换重试的宽限窗口」)与重用检测、家族吊销
// (同文「令牌家族、重用检测与家族吊销」)。全部用 miniredis;轮换记录的过期用 FastForward 推进,不依赖墙钟。

func familyData(t *testing.T, rdb *redis.Client, token string) TokenData {
	t.Helper()
	raw, err := rdb.Get(context.Background(), refreshKeyOf(t, token)).Bytes()
	if err != nil {
		t.Fatal("读取本用例 refresh 失败", err)
	}
	var data TokenData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// 独立连接不携带并发注入 hook；hash 字段也进入原子性比较，不能只比较键类型。
func familySnapshot(t *testing.T, mr *miniredis.Miniredis) map[string]string {
	t.Helper()
	out := snapshot(t, mr)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	for _, key := range mr.Keys() {
		if mr.Type(key) != "hash" {
			continue
		}
		fields, err := rdb.HGetAll(context.Background(), key).Result()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		out[key] = string(raw) + " ttl=" + mr.TTL(key).String()
	}
	return out
}

// requireFamilyRevoked 断言这个家族已吊销:家族记录和头的键都没了,头已从账号索引摘掉,头不能再轮换,
// 头这一对和 earlierAccess 里家族签过的 access 全部失效。
func requireFamilyRevoked(t *testing.T, m *Manager, mr *miniredis.Miniredis, head *TokenPair, earlierAccess ...string) {
	t.Helper()
	if mr.Exists(familyKeyOfToken(t, head.RefreshToken)) {
		t.Fatal("吊销之后家族记录还在")
	}
	if mr.Exists(refreshKeyOf(t, head.RefreshToken)) {
		t.Fatal("吊销之后头的 refresh 键还在")
	}
	if members, err := mr.ZMembers(testIndexKey); err == nil && slices.Contains(members, secretOf(t, head.RefreshToken)) {
		t.Fatal("吊销之后头还在账号索引里")
	}
	requireRejected(t, m, head.RefreshToken)
	requireAccessInvalid(t, m, head.AccessToken)
	for _, access := range earlierAccess {
		requireAccessInvalid(t, m, access)
	}
}

// refreshConcurrently 让 callers 路同时拿同一枚令牌刷新,按调用方下标返回结果。
func refreshConcurrently(m *Manager, refreshToken string, callers int) ([]*TokenPair, []error) {
	pairs := make([]*TokenPair, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pairs[i], errs[i] = m.Refresh(context.Background(), refreshToken)
		}()
	}
	close(start)
	wg.Wait()
	return pairs, errs
}

// scriptKeysOf 取脚本命令声明的 KEYS;不是 EVAL / EVALSHA 回 nil。
func scriptKeysOf(cmd redis.Cmder) []string {
	if name := cmd.Name(); name != "eval" && name != "evalsha" {
		return nil
	}
	keys, ok, err := playerredis.CommandKeys(cmd.Args())
	if !ok || err != nil {
		return nil
	}
	return keys
}

// 按 KEYS 的形状认出是哪段脚本,顺序见 token.go 各脚本的调用点。

func issueScriptKeys(keys []string) bool {
	return len(keys) == 4 && strings.HasPrefix(keys[0], "access_token:") && strings.HasPrefix(keys[3], "refresh_family:")
}

func rotateScriptKeys(keys []string) bool {
	return len(keys) == 6 && strings.HasPrefix(keys[0], "refresh_token:") && strings.HasPrefix(keys[5], "refresh_rotated:")
}

func replayScriptKeys(keys []string) bool {
	return len(keys) == 2 && strings.HasPrefix(keys[0], "refresh_family:") && strings.HasPrefix(keys[1], "refresh_rotated:")
}

// scriptHook 在第一条 KEYS 满足 match 的脚本命令发出前执行 before,resend 为真时把它原样再发一次
// (模拟驱动对读超时的重发)。EVALSHA 回 NOSCRIPT 时不算触发:Script.Run 随后改发 EVAL,钩子在 EVAL 上触发,
// before 因此可能执行两次,用例里的 before 都是幂等的。只触发一次。
type scriptHook struct {
	match  func(keys []string) bool
	before func(keys []string)
	resend bool
	done   bool
}

func (h *scriptHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *scriptHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		keys := scriptKeysOf(cmd)
		if h.done || keys == nil || !h.match(keys) {
			return next(ctx, cmd)
		}
		if h.before != nil {
			h.before(keys)
		}
		err := next(ctx, cmd)
		if err != nil && strings.HasPrefix(err.Error(), "NOSCRIPT") {
			return err
		}
		h.done = true
		if h.resend {
			err = next(ctx, cmd)
		}
		return err
	}
}

func (h *scriptHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// failScriptHook 让 KEYS 满足 match 的脚本命令前 failures 次直接失败(不发到 Redis),之后放行;calls 数匹配到的次数。
type failScriptHook struct {
	match    func(keys []string) bool
	err      error
	failures int
	calls    int
}

func (h *failScriptHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failScriptHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		keys := scriptKeysOf(cmd)
		if keys == nil || !h.match(keys) {
			return next(ctx, cmd)
		}
		h.calls++
		if h.calls <= h.failures {
			return h.err
		}
		return next(ctx, cmd)
	}
}

func (h *failScriptHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// injectedServerError 冒充服务端明确回的错误(实现 redis.Error)。
type injectedServerError string

func (e injectedServerError) Error() string { return string(e) }

func (injectedServerError) RedisError() {}

// logBuffer / captureLogs:把本用例期间的 logx 输出收进内存,用例结束时恢复原来的 writer。
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	old := logx.Reset()
	logx.SetWriter(logx.NewWriter(buf))
	t.Cleanup(func() {
		logx.Reset()
		if old != nil {
			logx.SetWriter(old)
		}
	})
	return buf
}

func TestTokenFamily_IssueRotationAndAccountRevocation(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	old := issueSeed(t, m)
	data := familyData(t, rdb, old.RefreshToken)
	rawID, err := base64.RawURLEncoding.DecodeString(data.FamilyID)
	if err != nil || len(data.FamilyID) != 22 || len(rawID) != 16 {
		t.Fatal("family 必须为 16 随机字节的规范 base64url")
	}
	access, refresh := parsedOf(t, old.AccessToken), parsedOf(t, old.RefreshToken)
	if access.familyID != data.FamilyID || refresh.familyID != data.FamilyID || access.kind != accessKind || refresh.kind != refreshKind {
		t.Fatal("一对令牌必须嵌同一个家族号,类型位分别是 access / refresh")
	}
	key := familyKeyOf(testAccount, data.FamilyID)
	fields, err := rdb.HGetAll(ctx, key).Result()
	if err != nil || fields["account"] != testAccount || fields["head"] != secretOf(t, old.RefreshToken) || fields["prev"] != "" || mr.TTL(key) != testConfig.RefreshTokenTTL {
		t.Fatal("新认证必须同时提交 family 归属、head 与 TTL")
	}
	other, err := m.Issue(ctx, "other-account", testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Issue(ctx, testAccount, testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	if familyData(t, rdb, second.RefreshToken).FamilyID == data.FamilyID {
		t.Fatal("同账号再次认证必须建立独立随机 family")
	}
	got, err := m.Refresh(ctx, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if familyData(t, rdb, got.RefreshToken).FamilyID != data.FamilyID {
		t.Fatal("轮换不得建立另一条 family")
	}
	if _, err := m.ValidateAccess(ctx, old.AccessToken); err != nil {
		t.Fatal("正常轮换不得撤销同 family 未过期 access", err)
	}
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{old.AccessToken, got.AccessToken, second.AccessToken} {
		if _, err := m.ValidateAccess(ctx, access); err == nil {
			t.Fatal("撤销 family 后旧新 access 均须拒绝")
		}
	}
	if _, err := m.Refresh(ctx, got.RefreshToken); err == nil {
		t.Fatal("撤销后不能刷新复活")
	}
	if _, err := m.ValidateAccess(ctx, other.AccessToken); err != nil {
		t.Fatal("不能误撤其它账号", err)
	}
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal("重复撤销应当幂等", err)
	}
}

// 每次轮换:家族号不变,head / prev 逐次推进(requireRotated 核对),created_at 不变,记录 TTL 续满;
// 家族活着时历次签出的 access 都仍有效。
func TestRefresh_FamilySurvivesRotations(t *testing.T) {
	m, _, mr := newTestManager(t)
	seed := issueSeed(t, m)
	key := familyKeyOfToken(t, seed.RefreshToken)
	createdAt := mr.HGet(key, "created_at")

	mr.FastForward(time.Hour)
	p1 := mustRefresh(t, m, seed.RefreshToken)
	requireRotated(t, mr, seed, p1)
	p2 := mustRefresh(t, m, p1.RefreshToken)
	requireRotated(t, mr, p1, p2)

	if got := mr.HGet(key, "created_at"); got != createdAt {
		t.Fatalf("created_at 跨轮换变了: %s → %s", createdAt, got)
	}
	for _, access := range []string{seed.AccessToken, p1.AccessToken, p2.AccessToken} {
		requireAccessValid(t, m, access)
	}
}

// 损坏与类型不符一律 fail-closed,拒绝时一个字节都不写。result 是这种情形下 Refresh 计的结果:
// 键不在、脱离索引、家族已不在是业务拒绝(rejected*),其余按损坏报错(error)。
func TestTokenFamily_InvalidFamilyFailsClosedWithoutRotation(t *testing.T) {
	for kind, result := range map[string]string{
		"missing":            "rejected_family_gone",
		"other-account":      "error",
		"stale-head":         "error",
		"wrong-type":         "error",
		"missing-head-token": "rejected",
		"missing-index":      "rejected",
		"invalid-id":         "error",
		"noncanonical-id":    "error",
		"empty-id":           "error",
		"null-id":            "error",
		"persistent-family":  "error",
		"persistent-refresh": "rejected",
	} {
		t.Run(kind, func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			ctx := context.Background()
			pair := issueSeed(t, m)
			data := familyData(t, rdb, pair.RefreshToken)
			key := familyKeyOf(testAccount, data.FamilyID)
			switch kind {
			case "missing":
				mr.Del(key)
			case "other-account":
				rdb.HSet(ctx, key, "account", "different-account")
			case "stale-head":
				rdb.HSet(ctx, key, "head", "another-head")
			case "wrong-type":
				mr.Del(key)
				mr.Set(key, "broken")
			case "missing-head-token":
				mr.Del(refreshKeyOf(t, pair.RefreshToken))
			case "missing-index":
				mr.Del(testIndexKey)
			case "persistent-family", "persistent-refresh":
				persistentKey := key
				if kind == "persistent-refresh" {
					persistentKey = refreshKeyOf(t, pair.RefreshToken)
				}
				if removed, err := rdb.Persist(ctx, persistentKey).Result(); err != nil || !removed {
					t.Fatal("夹具必须移除原正 TTL", err)
				}
			case "invalid-id", "noncanonical-id":
				data.FamilyID = strings.Repeat("A", 21) + "!"
				if kind == "noncanonical-id" {
					data.FamilyID = strings.Repeat("A", 21) + "B"
				}
				raw, _ := json.Marshal(data)
				rdb.Set(ctx, accessKeyOf(t, pair.AccessToken), raw, testConfig.AccessTokenTTL)
				rdb.Set(ctx, refreshKeyOf(t, pair.RefreshToken), raw, testConfig.RefreshTokenTTL)
			case "empty-id", "null-id":
				fields := map[string]any{"account": data.Account, "auth_type": data.AuthType, "created_at": data.CreatedAt, "family_id": ""}
				if kind == "null-id" {
					fields["family_id"] = nil
				}
				raw, _ := json.Marshal(fields)
				rdb.Set(ctx, accessKeyOf(t, pair.AccessToken), raw, testConfig.AccessTokenTTL)
				rdb.Set(ctx, refreshKeyOf(t, pair.RefreshToken), raw, testConfig.RefreshTokenTTL)
			}
			before := familySnapshot(t, mr)
			if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
				t.Fatal("损坏 family 不得认证")
			}
			var err error
			requireOnlyResult(t, result, func() { _, err = m.Refresh(ctx, pair.RefreshToken) })
			if err == nil {
				t.Fatal("损坏 family 不得签出后继")
			}
			if errors.Is(err, ErrRefreshRejected) == (result == "error") {
				t.Fatalf("err = %v 与计数结果 %s 不符", err, result)
			}
			if !maps.Equal(before, familySnapshot(t, mr)) {
				t.Fatal("拒绝必须不修改原 token/family/索引")
			}
		})
	}
}

func TestTokenFamily_ExpiredFamilyRejectsUnexpiredAccess(t *testing.T) {
	m, _, mr := newTestManager(t)
	m.cfg = Config{AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Minute}
	pair := issueSeed(t, m)
	mr.FastForward(time.Minute + time.Millisecond)
	if !mr.Exists(accessKeyOf(t, pair.AccessToken)) {
		t.Fatal("夹具须保留尚未过期 access")
	}
	if _, err := m.ValidateAccess(context.Background(), pair.AccessToken); err == nil {
		t.Fatal("family 到期不能只凭 access 继续认证")
	}
	t.Run("persistent-access", func(t *testing.T) {
		m, rdb, mr := newTestManager(t)
		ctx := context.Background()
		pair := issueSeed(t, m)
		if removed, err := rdb.Persist(ctx, accessKeyOf(t, pair.AccessToken)).Result(); err != nil || !removed {
			t.Fatal("夹具必须移除 access 的原正 TTL", err)
		}
		before := familySnapshot(t, mr)
		if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
			t.Fatal("新 family 的永久 access 必须拒绝认证")
		}
		if !maps.Equal(before, familySnapshot(t, mr)) {
			t.Fatal("拒绝永久 access 不能修改 token/family/索引")
		}
	})
}

// 窗口内重放:原样返回同一对,不签发、不吊销、不续期,Redis 一个字节都不动。
func TestRefresh_GraceReplayReturnsTheSamePair(t *testing.T) {
	m, _, mr := newManagerWith(t, graceConfig)
	seed := issueSeed(t, m)
	first := mustRefresh(t, m, seed.RefreshToken)
	if ttl := mr.TTL(rotatedKeyOf(t, seed.RefreshToken)); ttl != testGrace {
		t.Fatalf("轮换记录 TTL = %s, want %s", ttl, testGrace)
	}
	// 窗口过去一半再重放:令牌和过期时间都是原来那一份,TTL 也没有被续。
	mr.FastForward(testGrace / 2)
	before := familySnapshot(t, mr)

	var got *TokenPair
	requireOnlyResult(t, "replayed", func() { got = mustRefresh(t, m, seed.RefreshToken) })

	if *got != *first {
		t.Fatalf("窗口内重放 = %+v, want 同一对 %+v", got, first)
	}
	requireUnchanged(t, mr, before)
	requireAccessValid(t, m, got.AccessToken)
	// 拿到的确实是活的头:客户端接着用它轮换,走的是正常轮换,不是重用。
	requireOnlyResult(t, "rotated", func() { mustRefresh(t, m, got.RefreshToken) })
}

// 窗口内同一旧 token 并发:只轮换一次,全部拿到同一对,家族完好。
func TestTokenFamily_ConcurrentRefreshWithinGraceConvergesOnOnePair(t *testing.T) {
	for _, callers := range []int{2, 8} {
		t.Run(strconv.Itoa(callers), func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			old := issueSeed(t, m)

			var pairs []*TokenPair
			var errs []error
			delta := refreshResultDelta(t, func() { pairs, errs = refreshConcurrently(m, old.RefreshToken, callers) })

			for i := range callers {
				if errs[i] != nil {
					t.Fatalf("第 %d 路: %v", i, errs[i])
				}
				if *pairs[i] != *pairs[0] {
					t.Fatalf("第 %d 路拿到了另一对", i)
				}
			}
			if len(delta) != 2 || delta["rotated"] != 1 || delta["replayed"] != float64(callers-1) {
				t.Fatalf("结果增量 = %v, want rotated 1、replayed %d", delta, callers-1)
			}
			requireRotated(t, mr, old, pairs[0])
			if n := rdb.ZCard(context.Background(), testIndexKey).Val(); n != 1 {
				t.Fatalf("账号索引有 %d 个成员,不能重复推进 head", n)
			}
			if got := keysWithPrefix(mr, "refresh_token:"); len(got) != 1 {
				t.Fatalf("refresh 键 = %v, want 只轮换一次", got)
			}
			requireAccessValid(t, m, pairs[0].AccessToken)
		})
	}
}

// 窗口为 0 时同一请求并发两份:一份轮换成功,另一份就是重用,家族随之吊销,赢家刚拿到的那一对也作废。
// 这是严格模式本来的含义,配置注释与启动日志都提示不要把窗口 0 与吊销同时开。
func TestTokenFamily_ZeroGraceConcurrentDuplicateLeavesNoUsablePair(t *testing.T) {
	m, _, mr := newTestManager(t)
	old := issueSeed(t, m)

	var pairs []*TokenPair
	var errs []error
	delta := refreshResultDelta(t, func() { pairs, errs = refreshConcurrently(m, old.RefreshToken, 2) })

	if len(delta) != 2 || delta["rotated"] != 1 || delta["reuse_revoked"] != 1 {
		t.Fatalf("结果增量 = %v, want rotated 1、reuse_revoked 1", delta)
	}
	var winner *TokenPair
	for i, err := range errs {
		if err == nil {
			winner = pairs[i]
		} else if !errors.Is(err, ErrRefreshRejected) {
			t.Fatalf("输的一路 err = %v, want ErrRefreshRejected", err)
		}
	}
	if winner == nil {
		t.Fatal("夹具:两路里应当有一路轮换成功")
	}
	requireFamilyRevoked(t, m, mr, winner, old.AccessToken)
}

// 读到旧值之后、脚本之前,另一路(另一个 login 实例)抢先轮换了同一枚 token:本路的脚本回 0,转重放判定,
// 拿回那一路换出的同一对。确定性地覆盖并发用例只能碰运气走到的「脚本回 0 → 重放 → 同一对」。
func TestRefresh_LostRaceWithinGraceReturnsTheWinnersPair(t *testing.T) {
	m, rdb, mr := newManagerWith(t, graceConfig)
	seed := issueSeed(t, m)
	otherClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = otherClient.Close() })
	other := NewManager(otherClient, graceConfig)
	var won *TokenPair
	rdb.AddHook(&afterFirstCommand{do: func() { won = mustRefresh(t, other, seed.RefreshToken) }})

	var got *TokenPair
	delta := refreshResultDelta(t, func() { got = mustRefresh(t, m, seed.RefreshToken) })

	if len(delta) != 2 || delta["rotated"] != 1 || delta["replayed"] != 1 {
		t.Fatalf("结果增量 = %v, want rotated 1、replayed 1", delta)
	}
	if won == nil || *got != *won {
		t.Fatal("输的一路没有拿到赢的一路换出的那一对")
	}
	if keys := keysWithPrefix(mr, "refresh_token:"); len(keys) != 1 {
		t.Fatalf("refresh 键 = %v, want 只轮换一次", keys)
	}
}

// 已轮换的旧成员再次出现,家族还活着:窗口外(记录过期)、窗口内而后继已被轮换走、窗口为 0,都判重用并吊销整个家族。
// 重放的是头的前一枚记 predecessor,更早的记 older。同账号另一次登录(另一台设备)不受影响。
func TestRefresh_ReuseRevokesTheFamily(t *testing.T) {
	cases := []struct {
		name         string
		cfg          Config
		rotations    int
		expireRecord bool
		replayed     string
	}{
		{"窗口外重放头的前一枚", graceConfig, 1, true, "predecessor"},
		{"窗口外重放更早的成员", graceConfig, 2, true, "older"},
		{"窗口内而后继已被轮换走", graceConfig, 2, false, "older"},
		{"窗口为 0 重放头的前一枚", testConfig, 1, false, "predecessor"},
		{"窗口为 0 重放更早的成员", testConfig, 2, false, "older"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, mr := newManagerWith(t, tc.cfg)
			seed := issueSeed(t, m)
			head := seed
			access := []string{seed.AccessToken}
			for range tc.rotations {
				head = mustRefresh(t, m, head.RefreshToken)
				access = append(access, head.AccessToken)
			}
			if tc.expireRecord {
				mr.FastForward(testGrace + time.Second)
			}
			// 记录在不在决定走的是窗口内还是窗口外的分支;窗口为 0 时根本不写。
			if want := tc.cfg.RefreshReuseGrace > 0 && !tc.expireRecord; mr.Exists(rotatedKeyOf(t, seed.RefreshToken)) != want {
				t.Fatalf("夹具:旧 refresh 的轮换记录应当 exists=%v", want)
			}
			other := issueSeed(t, m)
			samples := reuseDelaySamples(t, tc.replayed)

			requireOnlyResult(t, "reuse_revoked", func() { requireRejected(t, m, seed.RefreshToken) })

			if got := reuseDelaySamples(t, tc.replayed) - samples; got != 1 {
				t.Fatalf("login_refresh_reuse_delay_seconds{replayed=%s} 增量 = %d, want 1", tc.replayed, got)
			}
			requireFamilyRevoked(t, m, mr, head, access...)
			requireAccessValid(t, m, other.AccessToken)
			mustRefresh(t, m, other.RefreshToken)
		})
	}
}

// 只观测:照常检测、计数,客户端照常被拒绝,但一个字节都不写,新 refresh 仍可用。
func TestRefresh_ReuseObserveOnlyRejectsWithoutRevoking(t *testing.T) {
	observe := func(grace time.Duration) Config {
		cfg := testConfig
		cfg.RefreshReuseGrace = grace
		cfg.ReuseObserveOnly = true
		return cfg
	}
	cases := []struct {
		name         string
		cfg          Config
		rotations    int
		expireRecord bool
		replayed     string
	}{
		{"窗口为 0", observe(0), 1, false, "predecessor"},
		{"窗口外", observe(testGrace), 1, true, "predecessor"},
		{"窗口内而后继已被轮换走", observe(testGrace), 2, false, "older"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, mr := newManagerWith(t, tc.cfg)
			seed := issueSeed(t, m)
			head := seed
			for range tc.rotations {
				head = mustRefresh(t, m, head.RefreshToken)
			}
			if tc.expireRecord {
				mr.FastForward(testGrace + time.Second)
			}
			before := familySnapshot(t, mr)
			samples := reuseDelaySamples(t, tc.replayed)

			requireOnlyResult(t, "reuse_observed", func() { requireRejected(t, m, seed.RefreshToken) })

			if got := reuseDelaySamples(t, tc.replayed) - samples; got != 1 {
				t.Fatalf("login_refresh_reuse_delay_seconds{replayed=%s} 增量 = %d, want 1", tc.replayed, got)
			}
			requireUnchanged(t, mr, before)
			requireAccessValid(t, m, seed.AccessToken)
			requireAccessValid(t, m, head.AccessToken)
			mustRefresh(t, m, head.RefreshToken)
		})
	}
}

// 读到旧 refresh 已不在之后请求就截止:重放判定走独立预算,照常吊销。这次漏掉的话,合法客户端去重登、
// 不再拿这枚旧 token 回来,就再也等不到下一次检测。
func TestRefresh_ReuseRevocationIgnoresTheRequestDeadline(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	seed := issueSeed(t, m)
	p1 := mustRefresh(t, m, seed.RefreshToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdb.AddHook(&afterFirstCommand{do: cancel})

	var err error
	requireOnlyResult(t, "reuse_revoked", func() { _, err = m.Refresh(ctx, seed.RefreshToken) })
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("err = %v, want ErrRefreshRejected", err)
	}
	requireFamilyRevoked(t, m, mr, p1)
}

// 重放判定脚本发不出去时在预算内有界重试;次数用完按存储故障计 error、不吊销;服务端明确回的错误不重试。
func TestRefresh_ReplayScriptRetriesOnlyTransientFailures(t *testing.T) {
	network := errors.New("injected network failure")
	cases := []struct {
		name      string
		err       error
		failures  int
		wantCalls int
		result    string
	}{
		{"瞬时故障在次数内恢复", network, replayAttempts - 1, replayAttempts, "reuse_revoked"},
		{"次数用完", network, replayAttempts, replayAttempts, "error"},
		{"服务端明确报错", injectedServerError("injected server error"), replayAttempts, 1, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, rdb, mr := newTestManager(t)
			seed := issueSeed(t, m)
			mustRefresh(t, m, seed.RefreshToken)
			// 预先载入脚本,放行的那一次不会多走一趟 NOSCRIPT → EVAL,匹配次数才确定。
			if err := replayRefreshScript.Load(ctx, rdb).Err(); err != nil {
				t.Fatal(err)
			}
			hook := &failScriptHook{match: replayScriptKeys, err: tc.err, failures: tc.failures}
			rdb.AddHook(hook)

			var err error
			requireOnlyResult(t, tc.result, func() { _, err = m.Refresh(ctx, seed.RefreshToken) })

			if hook.calls != tc.wantCalls {
				t.Fatalf("脚本发了 %d 次, want %d", hook.calls, tc.wantCalls)
			}
			familyAlive := mr.Exists(familyKeyOfToken(t, seed.RefreshToken))
			if tc.result == "error" {
				if err == nil || errors.Is(err, ErrRefreshRejected) || !familyAlive {
					t.Fatalf("err = %v familyAlive = %v, want 按存储故障报错且不吊销", err, familyAlive)
				}
			} else if !errors.Is(err, ErrRefreshRejected) || familyAlive {
				t.Fatalf("err = %v familyAlive = %v, want 判重用并吊销", err, familyAlive)
			}
		})
	}
}

// 家族已吊销或已被淘汰:窗口内重放不再返回这个家族的令牌,即使轮换记录还在。
func TestRefresh_GraceReplayRejectedOnceFamilyRevoked(t *testing.T) {
	revocations := map[string]func(t *testing.T, m *Manager, mr *miniredis.Miniredis, seed, head *TokenPair){
		"重用吊销": func(t *testing.T, m *Manager, _ *miniredis.Miniredis, seed, _ *TokenPair) {
			requireOnlyResult(t, "reuse_revoked", func() { requireRejected(t, m, seed.RefreshToken) })
		},
		"RevokeAll": func(t *testing.T, m *Manager, _ *miniredis.Miniredis, _, _ *TokenPair) {
			if err := m.RevokeAll(context.Background(), testAccount); err != nil {
				t.Fatal(err)
			}
		},
		"封顶淘汰": func(t *testing.T, m *Manager, mr *miniredis.Miniredis, _, head *TokenPair) {
			// 同一秒签发的成员 score 相同,谁先被淘汰由成员字典序决定;把头的 score 调低,让它确定是最旧的那个。
			if _, err := mr.ZAdd(testIndexKey, float64(head.RefreshTokenExpire-100), secretOf(t, head.RefreshToken)); err != nil {
				t.Fatal(err)
			}
			for range maxRefreshTokensPerAccount {
				issueSeed(t, m)
			}
		},
	}
	for name, revoke := range revocations {
		t.Run(name, func(t *testing.T) {
			m, _, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			p1 := mustRefresh(t, m, seed.RefreshToken)
			head := mustRefresh(t, m, p1.RefreshToken)
			revoke(t, m, mr, seed, head)
			if mr.Exists(familyKeyOfToken(t, head.RefreshToken)) {
				t.Fatal("夹具:家族记录应当已被删除")
			}
			if !mr.Exists(rotatedKeyOf(t, p1.RefreshToken)) {
				t.Fatal("夹具:p1 的轮换记录应当还在窗口内")
			}
			before := familySnapshot(t, mr)

			requireOnlyResult(t, "rejected_family_gone", func() { requireRejected(t, m, p1.RefreshToken) })

			requireUnchanged(t, mr, before)
			requireAccessInvalid(t, m, head.AccessToken)
		})
	}
}

// 拼出「家族号 + 任意随机数」(类型位置成 refresh)等同于重放旧成员:知道家族号的人本来就持有过该家族的令牌,能力不新增。
func TestRefresh_ForgedTokenCarryingFamilyIDCountsAsReuse(t *testing.T) {
	m, _, mr := newTestManager(t)
	seed := issueSeed(t, m)
	secret, err := newSecret(parsedOf(t, seed.RefreshToken).familyID, refreshKind)
	if err != nil {
		t.Fatal(err)
	}
	forged := formatToken(redisslot.AccountTag(testAccount), secret)
	samples := reuseDelaySamples(t, "older")

	requireOnlyResult(t, "reuse_revoked", func() { requireRejected(t, m, forged) })

	if got := reuseDelaySamples(t, "older") - samples; got != 1 {
		t.Fatalf("older 样本增量 = %d, want 1", got)
	}
	requireFamilyRevoked(t, m, mr, seed)
}

// 规范但从未签发过的令牌(家族不存在):普通拒绝,不吊销任何东西。
func TestRefresh_UnknownFamilyIsPlainRejection(t *testing.T) {
	m, _, mr := newTestManager(t)
	issueSeed(t, m)
	family, err := generateOpaque(familyByteLen)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := newSecret(family, refreshKind)
	if err != nil {
		t.Fatal(err)
	}
	stranger := formatToken(redisslot.AccountTag(testAccount), secret)
	before := familySnapshot(t, mr)

	requireOnlyResult(t, "rejected", func() { requireRejected(t, m, stranger) })

	requireUnchanged(t, mr, before)
}

// 家族头自己的键过期或被外部删除,家族记录还指着它:重放这枚头不是重用,普通拒绝,不吊销,不写。
func TestRefresh_VanishedHeadIsPlainRejection(t *testing.T) {
	vanishes := map[string]func(mr *miniredis.Miniredis, key string){
		"被外部删除": func(mr *miniredis.Miniredis, key string) { mr.Del(key) },
		"TTL 到期": func(mr *miniredis.Miniredis, key string) {
			mr.SetTTL(key, time.Second)
			mr.FastForward(2 * time.Second)
		},
	}
	for name, vanish := range vanishes {
		t.Run(name, func(t *testing.T) {
			m, _, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			head := mustRefresh(t, m, seed.RefreshToken)
			vanish(mr, refreshKeyOf(t, head.RefreshToken))
			before := familySnapshot(t, mr)

			requireOnlyResult(t, "rejected", func() { requireRejected(t, m, head.RefreshToken) })

			requireUnchanged(t, mr, before)
			if !mr.Exists(familyKeyOfToken(t, head.RefreshToken)) {
				t.Fatal("头自己的键没了不是重用,家族记录不该被吊销")
			}
		})
	}
}

// 窗口内重放,后继的键被外部删除而家族头仍指着它:不是两方在用,普通拒绝,不吊销,不写。
func TestRefresh_GraceReplayWithSuccessorVanishedIsPlainRejection(t *testing.T) {
	m, _, mr := newManagerWith(t, graceConfig)
	seed := issueSeed(t, m)
	p1 := mustRefresh(t, m, seed.RefreshToken)
	mr.Del(refreshKeyOf(t, p1.RefreshToken))
	before := familySnapshot(t, mr)

	requireOnlyResult(t, "rejected_successor_gone", func() { requireRejected(t, m, seed.RefreshToken) })

	requireUnchanged(t, mr, before)
}

// 轮换记录的剩余 TTL 不能用来延长窗口:被 PERSIST 或超过 60 秒硬上限按损坏报错;
// 落在(当前窗口, 硬上限]之间(窗口刚调小后的过渡期)普通拒绝。都不写、不吊销。
func TestRefresh_RotationRecordTTLOutsideTheWindow(t *testing.T) {
	setTTL := func(ttl time.Duration) func(*testing.T, *redis.Client, *miniredis.Miniredis, string) {
		return func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record string) {
			mr.SetTTL(record, ttl)
		}
	}
	cases := []struct {
		name    string
		mutate  func(t *testing.T, rdb *redis.Client, mr *miniredis.Miniredis, record string)
		damaged bool
	}{
		{"被 PERSIST", func(t *testing.T, rdb *redis.Client, _ *miniredis.Miniredis, record string) {
			if removed, err := rdb.Persist(context.Background(), record).Result(); err != nil || !removed {
				t.Fatal("夹具必须移除轮换记录的 TTL", err)
			}
		}, true},
		{"超过硬上限", setTTL(MaxRefreshReuseGrace + time.Millisecond), true},
		{"恰好等于硬上限", setTTL(MaxRefreshReuseGrace), false},
		{"比当前窗口长", setTTL(testGrace + time.Millisecond), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			p1 := mustRefresh(t, m, seed.RefreshToken)
			tc.mutate(t, rdb, mr, rotatedKeyOf(t, seed.RefreshToken))
			before := familySnapshot(t, mr)

			if tc.damaged {
				requireDamaged(t, m, seed.RefreshToken)
			} else {
				requireOnlyResult(t, "rejected", func() { requireRejected(t, m, seed.RefreshToken) })
			}

			requireUnchanged(t, mr, before)
			requireAccessValid(t, m, p1.AccessToken)
		})
	}
}

// 窗口调小(含调成 0)的过渡期:按旧窗口写下的记录比当前窗口长,普通拒绝,不返回同一对,也不当成重用吊销。
func TestRefresh_ShrunkWindowTransitionIsPlainRejection(t *testing.T) {
	for _, grace := range []time.Duration{0, 10 * time.Second} {
		t.Run(grace.String(), func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			p1 := mustRefresh(t, m, seed.RefreshToken)
			cfg := graceConfig
			cfg.RefreshReuseGrace = grace
			shrunk := NewManager(rdb, cfg)
			before := familySnapshot(t, mr)

			requireOnlyResult(t, "rejected", func() { requireRejected(t, shrunk, seed.RefreshToken) })

			requireUnchanged(t, mr, before)
			requireAccessValid(t, shrunk, p1.AccessToken)
		})
	}
}

// 窗口内重放时轮换记录或后继的内容不对:按损坏报错,不写,不把半截令牌发出去。
func TestRefresh_DamagedRotationRecordFailsClosed(t *testing.T) {
	cases := map[string]func(t *testing.T, rdb *redis.Client, mr *miniredis.Miniredis, record, successorKey string){
		"类型不对": func(t *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record, _ string) {
			mr.Del(record)
			if err := mr.Set(record, "broken"); err != nil {
				t.Fatal(err)
			}
			mr.SetTTL(record, testGrace)
		},
		"缺后继字段": func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record, _ string) {
			mr.HDel(record, "refresh")
		},
		"缺 access 字段": func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record, _ string) {
			mr.HDel(record, "access")
		},
		"缺 access 过期时间": func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record, _ string) {
			mr.HDel(record, "access_expire")
		},
		"refresh 过期时间不是数字": func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, record, _ string) {
			mr.HSet(record, "refresh_expire", "not-a-number")
		},
		"后继被 PERSIST": func(t *testing.T, rdb *redis.Client, _ *miniredis.Miniredis, _, successorKey string) {
			if removed, err := rdb.Persist(context.Background(), successorKey).Result(); err != nil || !removed {
				t.Fatal("夹具必须移除后继的 TTL", err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			p1 := mustRefresh(t, m, seed.RefreshToken)
			damage(t, rdb, mr, rotatedKeyOf(t, seed.RefreshToken), refreshKeyOf(t, p1.RefreshToken))
			before := familySnapshot(t, mr)

			requireDamaged(t, m, seed.RefreshToken)

			requireUnchanged(t, mr, before)
		})
	}
}

// 家族记录损坏:重放判定和轮换两条路都报错,都不写,不把损坏当成已吊销或重用。
func TestRefresh_DamagedFamilyRecordFailsClosed(t *testing.T) {
	cases := map[string]func(t *testing.T, rdb *redis.Client, mr *miniredis.Miniredis, key string){
		"类型不对": func(t *testing.T, _ *redis.Client, mr *miniredis.Miniredis, key string) {
			mr.Del(key)
			if err := mr.Set(key, "broken"); err != nil {
				t.Fatal(err)
			}
			mr.SetTTL(key, testConfig.RefreshTokenTTL)
		},
		"被 PERSIST": func(t *testing.T, rdb *redis.Client, _ *miniredis.Miniredis, key string) {
			if removed, err := rdb.Persist(context.Background(), key).Result(); err != nil || !removed {
				t.Fatal("夹具必须移除家族记录的 TTL", err)
			}
		},
		"缺账号": func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, key string) { mr.HDel(key, "account") },
		"缺头":  func(_ *testing.T, _ *redis.Client, mr *miniredis.Miniredis, key string) { mr.HDel(key, "head") },
	}
	for name, damage := range cases {
		// 旧 refresh 已不在,走重放判定脚本。
		t.Run(name+"/重放", func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			mustRefresh(t, m, seed.RefreshToken)
			damage(t, rdb, mr, familyKeyOfToken(t, seed.RefreshToken))
			before := familySnapshot(t, mr)

			requireDamaged(t, m, seed.RefreshToken)

			requireUnchanged(t, mr, before)
		})
		// 头还活着,走轮换脚本。
		t.Run(name+"/轮换", func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			damage(t, rdb, mr, familyKeyOfToken(t, seed.RefreshToken))
			before := familySnapshot(t, mr)

			requireDamaged(t, m, seed.RefreshToken)

			requireUnchanged(t, mr, before)
		})
	}
}

func TestTokenFamily_TrimStaleMemberCannotDeleteRotatedFamily(t *testing.T) {
	m, rdb, _ := newTestManager(t)
	ctx := context.Background()
	old := issueSeed(t, m)
	oldData := familyData(t, rdb, old.RefreshToken)
	got, err := m.Refresh(ctx, old.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧候选残留；当前 head 已经推进，淘汰此旧成员不能删除新 head 的 family。
	raw, _ := json.Marshal(oldData)
	rdb.Set(ctx, refreshKeyOf(t, old.RefreshToken), raw, testConfig.RefreshTokenTTL)
	rdb.ZAdd(ctx, testIndexKey, redis.Z{Score: -1, Member: secretOf(t, old.RefreshToken)})
	tag := redisslot.AccountTag(testAccount)
	for i := 0; i < maxRefreshTokensPerAccount-1; i++ {
		secret, err := newSecret(oldData.FamilyID, refreshKind)
		if err != nil {
			t.Fatal(err)
		}
		rdb.Set(ctx, redisslot.RefreshTokenKey(tag, secret), raw, testConfig.RefreshTokenTTL)
		rdb.ZAdd(ctx, testIndexKey, redis.Z{Score: float64(i + 1), Member: secret})
	}
	m.trimRefreshSet(ctx, testAccount, "")
	if n := rdb.ZCard(ctx, testIndexKey).Val(); n != maxRefreshTokensPerAccount {
		t.Fatal("容量整理应在一个提交中完成")
	}
	if _, err := m.ValidateAccess(ctx, got.AccessToken); err != nil {
		t.Fatal("旧候选不能误删已轮换的新 family", err)
	}
}

// 封顶:活成员超过 maxRefreshTokensPerAccount 时淘汰 score 最小(最久没轮换)的家族,连同它的头和 access,不牵连别的家族。
func TestTokenFamily_CapEvictsTheOldestFamily(t *testing.T) {
	m, _, mr := newTestManager(t)
	oldest := issueSeed(t, m)
	// 同一秒签发的成员 score 相同,谁先被淘汰由成员字典序决定;把它的 score 调低,让它确定是最旧的那个。
	if _, err := mr.ZAdd(testIndexKey, float64(oldest.RefreshTokenExpire-100), secretOf(t, oldest.RefreshToken)); err != nil {
		t.Fatal(err)
	}
	var newest *TokenPair
	for range maxRefreshTokensPerAccount {
		newest = issueSeed(t, m)
	}
	if got := keysWithPrefix(mr, "refresh_family:"); len(got) != maxRefreshTokensPerAccount {
		t.Fatalf("封顶之后家族记录 %d 个, want %d", len(got), maxRefreshTokensPerAccount)
	}
	requireFamilyRevoked(t, m, mr, oldest)
	requireAccessValid(t, m, newest.AccessToken)
}

func TestTokenFamily_RevokeDoesNotDeleteAuthenticationIssuedAfterCommit(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	issueSeed(t, m)
	if err := retireRefreshScript.Load(ctx, rdb).Err(); err != nil {
		t.Fatal(err)
	}
	otherClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer otherClient.Close()
	otherManager := NewManager(otherClient, testConfig)
	var next *TokenPair
	rdb.AddHook(&afterFirstCommand{do: func() {
		var err error
		next, err = otherManager.Issue(ctx, testAccount, testAuthType, testDevice)
		if err != nil {
			t.Fatal(err)
		}
	}})
	if err := m.RevokeAll(ctx, testAccount); err != nil {
		t.Fatal(err)
	}
	if next == nil {
		t.Fatal("夹具必须在撤销提交后创建新认证")
	}
	if _, err := otherManager.ValidateAccess(ctx, next.AccessToken); err != nil {
		t.Fatal("旧撤销不得误删提交后的新 family", err)
	}
}

func TestTokenFamily_CrossAccountIndexCorruptionCannotDeleteForeignFamily(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	issueSeed(t, m)
	other, err := m.Issue(ctx, "foreign-account", testAuthType, testDevice)
	if err != nil {
		t.Fatal(err)
	}
	// 脚本按本账号的 tag 现拼候选键,够不到别的账号的键。要构造这种损坏,得把别的账号的载荷写进本账号的键空间。
	foreignPayload, err := rdb.Get(ctx, refreshKeyOf(t, other.RefreshToken)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	corrupted := secretOf(t, other.RefreshToken)
	rdb.Set(ctx, redisslot.RefreshTokenKey(redisslot.AccountTag(testAccount), corrupted), foreignPayload, testConfig.RefreshTokenTTL)
	rdb.ZAdd(ctx, testIndexKey, redis.Z{Score: 0, Member: corrupted})
	before := familySnapshot(t, mr)
	if err := m.RevokeAll(ctx, testAccount); err == nil {
		t.Fatal("跨账号索引损坏必须拒绝")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("所有候选核验完前不得删除部分 family")
	}
	if _, err := m.ValidateAccess(ctx, other.AccessToken); err != nil {
		t.Fatal("不得误删其它账号 family", err)
	}
}

func TestTokenFamily_WrongIndexTypeWritesNothing(t *testing.T) {
	m, _, mr := newTestManager(t)
	ctx := context.Background()
	pair := issueSeed(t, m)
	mr.Del(testIndexKey)
	if err := mr.Set(testIndexKey, "broken-index"); err != nil {
		t.Fatal(err)
	}
	before := familySnapshot(t, mr)
	if _, err := m.Issue(ctx, testAccount, testAuthType, testDevice); err == nil {
		t.Fatal("损坏 ZSET 不得签出部分新 pair")
	}
	if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("损坏 ZSET 不得轮换")
	}
	if err := m.RevokeAll(ctx, testAccount); err == nil {
		t.Fatal("损坏 ZSET 不得冒撤销成功")
	}
	if !maps.Equal(before, familySnapshot(t, mr)) {
		t.Fatal("WRONGTYPE 路径必须在任何写入前拒绝")
	}
}

func TestTokenFamily_DamagedRevocationCandidateWritesNothing(t *testing.T) {
	for _, kind := range []string{"foreign-head", "wrong-type", "missing-head", "malformed-payload", "empty-family", "persistent-refresh", "persistent-family"} {
		t.Run(kind, func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			ctx := context.Background()
			issueSeed(t, m)
			bad := issueSeed(t, m)
			data := familyData(t, rdb, bad.RefreshToken)
			key := familyKeyOf(testAccount, data.FamilyID)
			// 保证损坏候选排在正常候选之后，仍不能先删除正常候选。
			rdb.ZAdd(ctx, testIndexKey, redis.Z{Score: float64(time.Now().Add(testConfig.RefreshTokenTTL).Unix() + 100), Member: secretOf(t, bad.RefreshToken)})
			switch kind {
			case "foreign-head":
				foreign, err := m.Issue(ctx, "foreign-head-account", testAuthType, testDevice)
				if err != nil {
					t.Fatal(err)
				}
				rdb.HSet(ctx, key, "head", secretOf(t, foreign.RefreshToken))
			case "wrong-type":
				mr.Del(key)
				if err := mr.Set(key, "broken-family"); err != nil {
					t.Fatal(err)
				}
			case "missing-head":
				rdb.HDel(ctx, key, "head")
			case "malformed-payload":
				rdb.Set(ctx, refreshKeyOf(t, bad.RefreshToken), "{", testConfig.RefreshTokenTTL)
			case "persistent-refresh", "persistent-family":
				persistentKey := key
				if kind == "persistent-refresh" {
					persistentKey = refreshKeyOf(t, bad.RefreshToken)
				}
				if removed, err := rdb.Persist(ctx, persistentKey).Result(); err != nil || !removed {
					t.Fatal("夹具必须移除撤销候选的原正 TTL", err)
				}
			case "empty-family":
				fields := map[string]any{"account": data.Account, "auth_type": data.AuthType, "device_id": data.DeviceID, "created_at": data.CreatedAt, "family_id": ""}
				payload, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				rdb.Set(ctx, refreshKeyOf(t, bad.RefreshToken), payload, testConfig.RefreshTokenTTL)
			}
			before := familySnapshot(t, mr)
			if err := m.RevokeAll(ctx, testAccount); err == nil {
				t.Fatal("损坏候选必须拒绝撤销")
			}
			if !maps.Equal(before, familySnapshot(t, mr)) {
				t.Fatal("Lua 错误没有回滚，必须先核全部候选再写")
			}
		})
	}
}

func TestTokenFamily_ReplacedRawValueCannotRotate(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	ctx := context.Background()
	pair := issueSeed(t, m)
	data := familyData(t, rdb, pair.RefreshToken)
	original, err := rdb.Get(ctx, refreshKeyOf(t, pair.RefreshToken)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	data.DeviceID = "concurrent-replacement"
	replacement, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	other := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer other.Close()
	if err := other.Set(ctx, refreshKeyOf(t, pair.RefreshToken), replacement, testConfig.RefreshTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	expected := familySnapshot(t, mr)
	if err := other.Set(ctx, refreshKeyOf(t, pair.RefreshToken), original, testConfig.RefreshTokenTTL).Err(); err != nil {
		t.Fatal(err)
	}
	rdb.AddHook(&afterFirstCommand{do: func() {
		if err := other.Set(ctx, refreshKeyOf(t, pair.RefreshToken), replacement, testConfig.RefreshTokenTTL).Err(); err != nil {
			t.Fatal(err)
		}
	}})
	if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
		t.Fatal("head 未变也不能消费已被替换的原始值")
	}
	if !maps.Equal(expected, familySnapshot(t, mr)) {
		t.Fatal("旧 raw CAS 失败不得更新 family 或签出 token")
	}
}

// 首发已写入、回包丢失后驱动原样重发同一段签发脚本:按已写入返回,签发成功,不报撞键、不留孤儿家族。
func TestIssue_ResendAfterCommitIsIdempotent(t *testing.T) {
	m, rdb, mr := newTestManager(t)
	issueSeed(t, m)
	hook := &scriptHook{match: issueScriptKeys, resend: true}
	rdb.AddHook(hook)

	pair := issueSeed(t, m)

	if !hook.done {
		t.Fatal("夹具:签发脚本应当被重发过")
	}
	requireAccessValid(t, m, pair.AccessToken)
	if got := keysWithPrefix(mr, "refresh_family:"); len(got) != 2 {
		t.Fatalf("家族记录 %d 个, want 2", len(got))
	}
	members, err := mr.ZMembers(testIndexKey)
	if err != nil || len(members) != 2 {
		t.Fatalf("账号索引 = %v / %v, want 2 个成员", members, err)
	}
	mustRefresh(t, m, pair.RefreshToken)
}

// 签发脚本撞上已有键且内容对不上:报撞键,不覆盖已有键,也不写别的键、不登记索引。
func TestIssue_CollisionIsRejectedWithoutOverwriting(t *testing.T) {
	// 下标是签发脚本的 KEYS 顺序:0 access、1 refresh、3 family。
	for name, slot := range map[string]int{"access": 0, "refresh": 1, "family": 3} {
		t.Run(name, func(t *testing.T) {
			m, rdb, mr := newTestManager(t)
			var squatted string
			rdb.AddHook(&scriptHook{match: issueScriptKeys, before: func(keys []string) {
				squatted = keys[slot]
				if err := mr.Set(squatted, "squatter"); err != nil {
					t.Error(err)
				}
			}})

			_, err := m.Issue(context.Background(), testAccount, testAuthType, testDevice)

			if err == nil || !strings.Contains(err.Error(), "token identity collision") {
				t.Fatalf("err = %v, want 撞键", err)
			}
			if value, err := mr.Get(squatted); err != nil || value != "squatter" {
				t.Fatalf("已有键被改成 %q / %v", value, err)
			}
			if keys := mr.Keys(); len(keys) != 1 {
				t.Fatalf("撞键之后还写了别的键: %v", keys)
			}
		})
	}
}

// 轮换脚本撞上已有键:报撞键,旧 refresh 仍有效,家族不推进,不写新令牌。
func TestRefresh_CollisionIsRejectedWithoutWriting(t *testing.T) {
	// 下标是轮换脚本的 KEYS 顺序:2 新 access、3 新 refresh、5 轮换记录。
	for name, slot := range map[string]int{"新 access": 2, "新 refresh": 3, "轮换记录": 5} {
		t.Run(name, func(t *testing.T) {
			m, rdb, mr := newManagerWith(t, graceConfig)
			seed := issueSeed(t, m)
			before := familySnapshot(t, mr)
			var squatted string
			rdb.AddHook(&scriptHook{match: rotateScriptKeys, before: func(keys []string) {
				squatted = keys[slot]
				if err := mr.Set(squatted, "squatter"); err != nil {
					t.Error(err)
				}
			}})

			var err error
			requireOnlyResult(t, "error", func() { _, err = m.Refresh(context.Background(), seed.RefreshToken) })

			if err == nil || !strings.Contains(err.Error(), "token identity collision") {
				t.Fatalf("err = %v, want 撞键", err)
			}
			after := familySnapshot(t, mr)
			delete(after, squatted)
			if !maps.Equal(before, after) {
				t.Fatalf("撞键之后还写了别的键:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// 日志里不出现令牌与家族号原值:签发、轮换、宽限重放、重用吊销、只观测、access 因家族吊销被拒,全部走一遍。
func TestTokenLogsCarryNoSecrets(t *testing.T) {
	logs := captureLogs(t)
	m, rdb, mr := newManagerWith(t, graceConfig)
	seed := issueSeed(t, m)
	p1 := mustRefresh(t, m, seed.RefreshToken)
	mustRefresh(t, m, seed.RefreshToken) // 宽限重放
	mr.FastForward(testGrace + time.Second)
	requireRejected(t, m, seed.RefreshToken) // 窗口外重用:吊销,replayed=predecessor
	requireAccessInvalid(t, m, p1.AccessToken)

	observeCfg := graceConfig
	observeCfg.ReuseObserveOnly = true
	observer := NewManager(rdb, observeCfg)
	other := issueSeed(t, observer)
	o1 := mustRefresh(t, observer, other.RefreshToken)
	o2 := mustRefresh(t, observer, o1.RefreshToken)
	requireRejected(t, observer, other.RefreshToken) // 窗口内后继已被轮换走:只观测,replayed=older

	text := logs.String()
	// 先确认确实捕获到了那几行,否则下面的「不含」什么都没证明。
	for _, want := range []string{
		"SECURITY", "replayed=predecessor", "replayed=older", "revoked=true", "revoked=false",
		"family=" + familyLogTag(parsedOf(t, seed.RefreshToken).familyID),
		"family=" + familyLogTag(parsedOf(t, other.RefreshToken).familyID),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志里没有 %q:\n%s", want, text)
		}
	}
	requireNoSecrets(t, text,
		seed.RefreshToken, seed.AccessToken, p1.RefreshToken, p1.AccessToken,
		other.RefreshToken, other.AccessToken, o1.RefreshToken, o1.AccessToken, o2.RefreshToken, o2.AccessToken)
}

// 仅具名本机 Redis 窗口执行；所有凭证与键均随机生成，清理仅删除本用例登记的原键，不 FLUSH/SCAN。
// miniredis 的 Lua 与类型语义不能完全替代真实后端,脚本的主要分支这里各走一遍。
func TestTokenFamily_RealRedis(t *testing.T) {
	if os.Getenv("LOGIN_TOKEN_REQUIRE_REAL_REDIS") != "1" {
		t.Skip("待具名真实 Redis 验证窗口")
	}
	addr := os.Getenv("LOGIN_TOKEN_TEST_REDIS_ADDR")
	host, _, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("只允许明确的数值 loopback Redis")
	}
	db, err := strconv.Atoi(os.Getenv("LOGIN_TOKEN_TEST_REDIS_DB"))
	if err != nil || db < 1 || db > 15 {
		t.Fatal("须显式指定本任务 Redis DB 1..15")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: db, Password: os.Getenv("LOGIN_TOKEN_TEST_REDIS_PASSWORD"), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal("本次 Redis 协议不可用", err)
	}
	suffix, err := generateOpaque(familyByteLen)
	if err != nil {
		t.Fatal(err)
	}
	account := "codex-token-family-it-" + suffix
	cfg := Config{AccessTokenTTL: 2 * time.Minute, RefreshTokenTTL: 5 * time.Minute}
	m := NewManager(rdb, cfg)
	keys := []string{redisslot.AccountRefreshKey(account)}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		if err := rdb.Del(cleanup, keys...).Err(); err != nil {
			t.Error("本用例精确键清理失败", err)
		}
	})
	// issue / rotate 登记本例产生的每个键,清理时精确删除。
	own := func(t *testing.T, pair *TokenPair) {
		t.Helper()
		keys = append(keys, accessKeyOf(t, pair.AccessToken), refreshKeyOf(t, pair.RefreshToken), familyKeyOfToken(t, pair.RefreshToken))
	}
	issue := func(t *testing.T, mgr *Manager, acct string) *TokenPair {
		t.Helper()
		keys = append(keys, redisslot.AccountRefreshKey(acct))
		pair, err := mgr.Issue(ctx, acct, testAuthType, testDevice)
		if err != nil {
			t.Fatal(err)
		}
		own(t, pair)
		return pair
	}
	rotate := func(t *testing.T, mgr *Manager, pair *TokenPair) *TokenPair {
		t.Helper()
		keys = append(keys, rotatedKeyOf(t, pair.RefreshToken))
		next, err := mgr.Refresh(ctx, pair.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		own(t, next)
		return next
	}
	requireRevokedOnRealRedis := func(t *testing.T, mgr *Manager, head *TokenPair, access ...string) {
		t.Helper()
		if rdb.Exists(ctx, familyKeyOfToken(t, head.RefreshToken), refreshKeyOf(t, head.RefreshToken)).Val() != 0 {
			t.Fatal("真实 Redis 吊销后家族记录或头的键还在")
		}
		if _, err := mgr.Refresh(ctx, head.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("真实 Redis 吊销后头不能再轮换", err)
		}
		for _, token := range append(access, head.AccessToken) {
			if _, err := mgr.ValidateAccess(ctx, token); err == nil {
				t.Fatal("真实 Redis 吊销后 access 仍可用")
			}
		}
	}

	t.Run("zero-grace-reuse-revokes", func(t *testing.T) {
		old := issue(t, m, account)
		got := rotate(t, m, old)
		if _, err := m.ValidateAccess(ctx, old.AccessToken); err != nil {
			t.Fatal("正常轮换不撤销同 family 旧 access", err)
		}
		if _, err := m.Refresh(ctx, old.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("真实 Redis 不得二次消费旧 refresh", err)
		}
		requireRevokedOnRealRedis(t, m, got, old.AccessToken)
	})
	t.Run("revoke-all", func(t *testing.T) {
		pair := issue(t, m, account)
		if err := m.RevokeAll(ctx, account); err != nil {
			t.Fatal(err)
		}
		requireRevokedOnRealRedis(t, m, pair)
	})
	graceCfg := cfg
	graceCfg.RefreshReuseGrace = 2 * time.Second
	grace := NewManager(rdb, graceCfg)
	t.Run("grace-replay-then-expiry-revokes", func(t *testing.T) {
		seed := issue(t, grace, account+"-grace")
		first := rotate(t, grace, seed)
		record := rotatedKeyOf(t, seed.RefreshToken)
		if ttl := rdb.PTTL(ctx, record).Val(); ttl <= 0 || ttl > graceCfg.RefreshReuseGrace {
			t.Fatal("轮换记录的 TTL 必须等于宽限窗口", ttl)
		}
		replayed, err := grace.Refresh(ctx, seed.RefreshToken)
		if err != nil || *replayed != *first {
			t.Fatal("窗口内重放必须返回同一对", err)
		}
		// 等真实 PX 过期(有上限,不靠本地时钟判定)后重放 → 吊销整个家族。
		deadline := time.Now().Add(5 * time.Second)
		for rdb.Exists(ctx, record).Val() == 1 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := grace.Refresh(ctx, seed.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("窗口外重放必须拒绝", err)
		}
		requireRevokedOnRealRedis(t, grace, first, seed.AccessToken)
	})
	t.Run("grace-successor-rotated-away-revokes", func(t *testing.T) {
		seed := issue(t, grace, account+"-grace-reuse")
		p1 := rotate(t, grace, seed)
		p2 := rotate(t, grace, p1)
		if _, err := grace.Refresh(ctx, seed.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("后继已被轮换走的重放必须拒绝", err)
		}
		requireRevokedOnRealRedis(t, grace, p2, seed.AccessToken, p1.AccessToken)
		// 家族已吊销,p1 的轮换记录(若还在窗口内)也不能再换出令牌。
		if _, err := grace.Refresh(ctx, p1.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("家族吊销之后窗口内重放必须拒绝", err)
		}
	})
	t.Run("observe-only-keeps-the-family", func(t *testing.T) {
		observeCfg := cfg
		observeCfg.ReuseObserveOnly = true
		observer := NewManager(rdb, observeCfg)
		seed := issue(t, observer, account+"-observe")
		p1 := rotate(t, observer, seed)
		if _, err := observer.Refresh(ctx, seed.RefreshToken); !errors.Is(err, ErrRefreshRejected) {
			t.Fatal("只观测也要拒绝重用", err)
		}
		if _, err := observer.ValidateAccess(ctx, p1.AccessToken); err != nil {
			t.Fatal("只观测不得吊销", err)
		}
		rotate(t, observer, p1)
	})
	t.Run("persistent-record-is-damage", func(t *testing.T) {
		seed := issue(t, grace, account+"-record")
		p1 := rotate(t, grace, seed)
		record := rotatedKeyOf(t, seed.RefreshToken)
		if removed, err := rdb.Persist(ctx, record).Result(); err != nil || !removed {
			t.Fatal("真实 Redis 须实际移除轮换记录的正 TTL", err)
		}
		owned := []string{record, familyKeyOfToken(t, p1.RefreshToken), refreshKeyOf(t, p1.RefreshToken)}
		before := map[string]string{}
		for _, key := range owned {
			value, err := rdb.Dump(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			before[key] = value
		}
		if _, err := grace.Refresh(ctx, seed.RefreshToken); err == nil || errors.Is(err, ErrRefreshRejected) {
			t.Fatal("永久轮换记录必须按损坏报错", err)
		}
		for _, key := range owned {
			value, err := rdb.Dump(ctx, key).Result()
			if err != nil || value != before[key] {
				t.Fatal("真实拒绝不得改写本例任意原件", err)
			}
		}
	})
	// miniredis 的 JSON 解码把 null 变成 nil，无法替代 Redis cjson 的 null 原件。
	// 这项必须在真实后端运行，且不打印随机键、令牌或 DUMP 内容。
	t.Run("null-retire-preserves-all-candidates", func(t *testing.T) {
		nullAccount := account + "-null"
		setKey := redisslot.AccountRefreshKey(nullAccount)
		keys = append(keys, setKey)
		localKeys := []string{setKey}
		var bad *TokenPair
		for range 2 {
			pair, err := m.Issue(ctx, nullAccount, testAuthType, testDevice)
			if err != nil {
				t.Fatal(err)
			}
			data := familyData(t, rdb, pair.RefreshToken)
			owned := []string{accessKeyOf(t, pair.AccessToken), refreshKeyOf(t, pair.RefreshToken), familyKeyOf(nullAccount, data.FamilyID)}
			keys = append(keys, owned...)
			localKeys = append(localKeys, owned...)
			bad = pair
		}
		data := familyData(t, rdb, bad.RefreshToken)
		payload, err := json.Marshal(map[string]any{"account": data.Account, "auth_type": data.AuthType, "device_id": data.DeviceID, "created_at": data.CreatedAt, "family_id": nil})
		if err != nil {
			t.Fatal(err)
		}
		if err := rdb.Set(ctx, refreshKeyOf(t, bad.RefreshToken), payload, m.cfg.RefreshTokenTTL).Err(); err != nil {
			t.Fatal(err)
		}
		if err := rdb.ZAdd(ctx, setKey, redis.Z{Score: float64(time.Now().Add(m.cfg.RefreshTokenTTL).Unix() + 100), Member: secretOf(t, bad.RefreshToken)}).Err(); err != nil {
			t.Fatal(err)
		}
		snapshot := func() map[string]string {
			values := map[string]string{}
			for _, key := range localKeys {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil {
					t.Fatal("读取本例精确状态失败", err)
				}
				values[key] = value
			}
			return values
		}
		before := snapshot()
		if err := m.RevokeAll(ctx, nullAccount); err == nil {
			t.Fatal("显式 null 不得作为旧 JSON 撤销")
		}
		if !maps.Equal(before, snapshot()) {
			t.Fatal("拒绝 null 候选必须保留全部原候选、索引和 family")
		}
	})
	for _, kind := range []string{"persistent-access", "persistent-head-refresh", "persistent-family"} {
		t.Run(kind, func(t *testing.T) {
			caseAccount := account + "-" + kind
			keys = append(keys, redisslot.AccountRefreshKey(caseAccount))
			pair, err := m.Issue(ctx, caseAccount, testAuthType, testDevice)
			if err != nil {
				t.Fatal(err)
			}
			data := familyData(t, rdb, pair.RefreshToken)
			owned := []string{accessKeyOf(t, pair.AccessToken), refreshKeyOf(t, pair.RefreshToken), familyKeyOf(caseAccount, data.FamilyID), redisslot.AccountRefreshKey(caseAccount)}
			keys = append(keys, owned[:3]...)
			corrupted := owned[0]
			if kind == "persistent-head-refresh" {
				corrupted = owned[1]
			} else if kind == "persistent-family" {
				corrupted = owned[2]
			}
			if removed, err := rdb.Persist(ctx, corrupted).Result(); err != nil || !removed {
				t.Fatal("真实 Redis 须实际移除本例正 TTL", err)
			}
			before := map[string]string{}
			for _, key := range owned {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil {
					t.Fatal(err)
				}
				before[key] = value
			}
			if _, err := m.ValidateAccess(ctx, pair.AccessToken); err == nil {
				t.Fatal("永久凭证不能通过真实认证")
			}
			if kind != "persistent-access" {
				if _, err := m.Refresh(ctx, pair.RefreshToken); err == nil {
					t.Fatal("永久 refresh/family 不能签出后继")
				}
				if err := m.RevokeAll(ctx, caseAccount); err == nil {
					t.Fatal("永久撤销候选必须先拒绝且不写")
				}
			}
			for _, key := range owned {
				value, err := rdb.Dump(ctx, key).Result()
				if err != nil || value != before[key] {
					t.Fatal("真实拒绝不得改写本例任意原件", err)
				}
			}
		})
	}
}
