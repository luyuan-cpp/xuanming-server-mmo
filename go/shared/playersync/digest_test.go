package playersync

// digest_test.go —— 摘要契约的回归(docs/design/landing-recovery-sync-digest.md §3)。
//
// BumpScript 在 miniredis 上真实执行(经 go-redis 适配器):起种、递增、续期、
// 重建后代次必变,这些是客户端「只拉变了的域」正确性的全部前提(L-2、L-3)。
// 「不发命令」「失败计数」这类调用面行为用记录型假 Evaler 断言,不依赖 miniredis 的命令计数口径。
// 时钟一律用 miniredis.SetTime 固定,不读 Go 墙钟(AGENTS §11.4)。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"

	scenepb "proto/scene"
)

const (
	domainFriendList    = scenepb.SyncDomain_SYNC_DOMAIN_FRIEND_LIST
	domainFriendRequest = scenepb.SyncDomain_SYNC_DOMAIN_FRIEND_REQUEST
	domainFriendBlock   = scenepb.SyncDomain_SYNC_DOMAIN_FRIEND_BLOCK
)

// field 是域在 hash 里的字段名(域号十进制)。测试自己拼,不复用生产代码的格式化:
// 字段名是 C++ 解析的客户端可见契约,生产端改了格式、测试跟着一起改,谁也发现不了。
func field(d scenepb.SyncDomain) string { return strconv.Itoa(int(d)) }

// ── 记录型假 Evaler ─────────────────────────────────────────────

type evalCall struct {
	script string
	keys   []string
	args   []string
}

type recordingEvaler struct {
	mu    sync.Mutex
	calls []evalCall
	err   error
}

func (r *recordingEvaler) Eval(_ context.Context, script string, keys []string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, evalCall{
		script: script,
		keys:   append([]string(nil), keys...),
		args:   append([]string(nil), args...),
	})
	return r.err
}

func (r *recordingEvaler) taken() []evalCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// ── miniredis 上的真适配器 ─────────────────────────────────

// forEachAdapter 为每个适配器起一个独立的 miniredis 跑同一段用例。
func forEachAdapter(t *testing.T, run func(t *testing.T, mr *miniredis.Miniredis, b *Bumper)) {
	t.Helper()
	builders := map[string]func(t *testing.T, mr *miniredis.Miniredis) Evaler{
		"go-redis": func(t *testing.T, mr *miniredis.Miniredis) Evaler {
			c := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = c.Close() })
			return NewGoRedisEvaler(c)
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			run(t, mr, NewBumper(build(t, mr), "playersync_test"))
		})
	}
}

func setRedisMs(mr *miniredis.Miniredis, ms int64) { mr.SetTime(time.UnixMilli(ms).UTC()) }

func requireField(t *testing.T, mr *miniredis.Miniredis, key, name, want string) {
	t.Helper()
	if got := mr.HGet(key, name); got != want {
		t.Fatalf("HGET %s %s = %q, want %q", key, name, got, want)
	}
}

func requireTTL(t *testing.T, mr *miniredis.Miniredis, key string) {
	t.Helper()
	if got := mr.TTL(key); got != DefaultTTL {
		t.Fatalf("TTL %s = %v, want %v(每次 bump 都必须续到满 TTL)", key, got, DefaultTTL)
	}
}

func TestKey(t *testing.T) {
	if got := Key(42); got != "player:{42}:sync_digest" {
		t.Fatalf("Key(42) = %q", got)
	}
	if got := Key(18446744073709551615); got != "player:{18446744073709551615}:sync_digest" {
		t.Fatalf("Key(max uint64) = %q:player_id 是 uint64,不能按有符号格式化", got)
	}
}

// 首次 bump:以 Redis TIME 起种 g(毫秒),该域计数 = 1,整键带满 TTL。
func TestBumpSeedsGenerationAndCounter(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		setRedisMs(mr, 1_780_000_000_123)
		if err := b.Bump(context.Background(), 42, domainFriendList); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		key := Key(42)
		requireField(t, mr, key, GenerationField, "1780000000123")
		requireField(t, mr, key, field(domainFriendList), "1")
		requireTTL(t, mr, key)
	})
}

// 再次 bump:g 不变(哪怕服务器时间已前进),计数 +1,TTL 续满。
func TestBumpKeepsGenerationAndIncrements(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		ctx := context.Background()
		setRedisMs(mr, 1_780_000_000_123)
		if err := b.Bump(ctx, 42, domainFriendList); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		mr.FastForward(time.Hour)
		setRedisMs(mr, 1_780_000_999_000)
		if err := b.Bump(ctx, 42, domainFriendList); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		key := Key(42)
		requireField(t, mr, key, GenerationField, "1780000000123")
		requireField(t, mr, key, field(domainFriendList), "2")
		requireTTL(t, mr, key)
	})
}

// 摘要丢失(DEL / 过期 / FLUSHALL)后重建:g 必须变化,计数从 1 重新开始 ——
// 客户端靠「g 不等」判出全量拉,否则重建后的计数可能恰好等于缓存值而漏拉(L-3)。
func TestBumpAfterDeleteChangesGeneration(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		ctx := context.Background()
		key := Key(42)
		setRedisMs(mr, 1_780_000_000_123)
		for i := 0; i < 3; i++ {
			if err := b.Bump(ctx, 42, domainFriendRequest); err != nil {
				t.Fatalf("Bump: %v", err)
			}
		}
		requireField(t, mr, key, field(domainFriendRequest), "3")

		mr.Del(key)
		setRedisMs(mr, 1_780_000_005_000)
		if err := b.Bump(ctx, 42, domainFriendRequest); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		requireField(t, mr, key, GenerationField, "1780000005000")
		requireField(t, mr, key, field(domainFriendRequest), "1")
		requireTTL(t, mr, key)
	})
}

// TTL 到期与 DEL 同口径:键消失后重建,g 变化。
func TestBumpAfterExpiryChangesGeneration(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		ctx := context.Background()
		key := Key(7)
		setRedisMs(mr, 1_780_000_000_123)
		if err := b.Bump(ctx, 7, domainFriendBlock); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		mr.FastForward(DefaultTTL)
		if mr.Exists(key) {
			t.Fatalf("FastForward 满 TTL 之后键应已过期")
		}
		setRedisMs(mr, 1_780_000_000_124)
		if err := b.Bump(ctx, 7, domainFriendBlock); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		requireField(t, mr, key, GenerationField, "1780000000124")
		requireField(t, mr, key, field(domainFriendBlock), "1")
	})
}

// 一次 bump 多个域:同一条 Lua 里各加一;重复的域只加一次。
func TestBumpMultipleDomainsOnce(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		setRedisMs(mr, 1_780_000_000_123)
		if err := b.Bump(context.Background(), 9,
			domainFriendBlock, domainFriendList, domainFriendRequest, domainFriendList); err != nil {
			t.Fatalf("Bump: %v", err)
		}
		key := Key(9)
		for _, d := range []scenepb.SyncDomain{domainFriendList, domainFriendRequest, domainFriendBlock} {
			requireField(t, mr, key, field(d), "1")
		}
		fields, err := mr.HKeys(key)
		if err != nil {
			t.Fatalf("HKeys: %v", err)
		}
		if len(fields) != 4 {
			t.Fatalf("hash 字段 = %v,应只有 g 与三个域", fields)
		}
	})
}

// BumpAll 在真 Lua 上给每个玩家各 bump 一次。
func TestBumpAllOnRedis(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, mr *miniredis.Miniredis, b *Bumper) {
		setRedisMs(mr, 1_780_000_000_123)
		if err := b.BumpAll(context.Background(), []uint64{11, 0, 12, 11}, domainFriendList); err != nil {
			t.Fatalf("BumpAll: %v", err)
		}
		requireField(t, mr, Key(11), field(domainFriendList), "1")
		requireField(t, mr, Key(12), field(domainFriendList), "1")
		if mr.Exists(Key(0)) {
			t.Fatalf("player_id=0 不能留下摘要键")
		}
	})
}

// ARGV 形状:TTL 秒在前,其后是去重后的域号十进制;KEYS 只有摘要键;脚本就是 BumpScript。
func TestBumpScriptArguments(t *testing.T) {
	ev := &recordingEvaler{}
	b := NewBumper(ev, "playersync_args")
	if err := b.Bump(context.Background(), 5, domainFriendRequest, domainFriendList, domainFriendRequest); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	calls := ev.taken()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.script != BumpScript {
		t.Fatalf("脚本不是 BumpScript")
	}
	if len(c.keys) != 1 || c.keys[0] != "player:{5}:sync_digest" {
		t.Fatalf("keys = %v", c.keys)
	}
	want := []string{"2592000", field(domainFriendRequest), field(domainFriendList)}
	if strings.Join(c.args, ",") != strings.Join(want, ",") {
		t.Fatalf("args = %v, want %v", c.args, want)
	}
}

// playerID=0、没有域、只有非法域:都不发任何命令,也不算失败。
func TestBumpWithoutTargetSendsNothing(t *testing.T) {
	ev := &recordingEvaler{}
	b := NewBumper(ev, "playersync_noop")
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"player_id=0", func() error { return b.Bump(ctx, 0, domainFriendList) }},
		{"无域", func() error { return b.Bump(ctx, 5) }},
		{"只有 UNSPECIFIED", func() error { return b.Bump(ctx, 5, scenepb.SyncDomain_SYNC_DOMAIN_UNSPECIFIED) }},
		{"BumpAll 空名单", func() error { return b.BumpAll(ctx, []uint64{0, 0}, domainFriendList) }},
		{"BumpAll 无域", func() error { return b.BumpAll(ctx, []uint64{5}) }},
	}
	for _, c := range cases {
		if err := c.call(); err != nil {
			t.Fatalf("%s: err = %v", c.name, err)
		}
	}
	if calls := ev.taken(); len(calls) != 0 {
		t.Fatalf("不该发任何命令,实际 %d 条", len(calls))
	}
}

// 非法域(UNSPECIFIED、负值)混在合法域里:只跳过非法的,合法的照常 bump ——
// 写进 hash 会留下 C++ 不认的字段,而整条丢掉又会让合法域漏 bump。
func TestBumpSkipsInvalidDomainsKeepsValid(t *testing.T) {
	ev := &recordingEvaler{}
	b := NewBumper(ev, "playersync_invalid")
	if err := b.Bump(context.Background(), 5,
		scenepb.SyncDomain_SYNC_DOMAIN_UNSPECIFIED, domainFriendBlock, scenepb.SyncDomain(-1)); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	calls := ev.taken()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	want := []string{"2592000", field(domainFriendBlock)}
	if strings.Join(calls[0].args, ",") != strings.Join(want, ",") {
		t.Fatalf("args = %v, want %v", calls[0].args, want)
	}
}

// nil Bumper 与 nil 句柄:未装配摘要的服务、手工构造 ServiceContext 的单测不必判空。
func TestNilBumperIsNoop(t *testing.T) {
	var b *Bumper
	ctx := context.Background()
	if err := b.Bump(ctx, 5, domainFriendList); err != nil {
		t.Fatalf("nil Bumper Bump: %v", err)
	}
	if err := b.BumpAll(ctx, []uint64{5, 6}, domainFriendList); err != nil {
		t.Fatalf("nil Bumper BumpAll: %v", err)
	}
	if NewBumper(nil, "x") != nil {
		t.Fatalf("NewBumper(nil) 应返回 nil")
	}
	if NewGoRedisEvaler(nil) != nil {
		t.Fatalf("nil 句柄的适配器应返回 nil Evaler")
	}
	if NewBumper(NewGoRedisEvaler(nil), "x") != nil {
		t.Fatalf("没有核心库句柄时 Bumper 应为 nil")
	}
}

// 失败:返回错误、计 result="error"、不计 ok。
func TestBumpFailureCounted(t *testing.T) {
	const service = "playersync_fail"
	ev := &recordingEvaler{err: errors.New("connection refused")}
	b := NewBumper(ev, service)
	okBefore := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultOK))
	errBefore := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultError))

	if err := b.Bump(context.Background(), 5, domainFriendList); err == nil {
		t.Fatalf("Eval 失败时 Bump 应返回错误")
	}
	if err := b.BumpAll(context.Background(), []uint64{6, 7}, domainFriendList); err == nil {
		t.Fatalf("Eval 失败时 BumpAll 应返回错误")
	}
	if got := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultError)); got != errBefore+3 {
		t.Fatalf("error 计数 = %v, want %v", got, errBefore+3)
	}
	if got := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultOK)); got != okBefore {
		t.Fatalf("失败不许记 ok")
	}
}

// BumpAll:玩家去重、跳过 0、保持顺序;成功各计一次 ok。
func TestBumpAllDedupesPlayers(t *testing.T) {
	const service = "playersync_all"
	ev := &recordingEvaler{}
	b := NewBumper(ev, service)
	okBefore := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultOK))

	if err := b.BumpAll(context.Background(), []uint64{3, 0, 1, 3, 2, 1}, domainFriendList); err != nil {
		t.Fatalf("BumpAll: %v", err)
	}
	var keys []string
	for _, c := range ev.taken() {
		keys = append(keys, c.keys[0])
	}
	want := []string{Key(3), Key(1), Key(2)}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if got := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultOK)); got != okBefore+3 {
		t.Fatalf("ok 计数 = %v, want %v", got, okBefore+3)
	}
}

// BumpAll 的总预算:ctx 已到期就不再发命令,剩余玩家计 error,返回错误。
func TestBumpAllStopsWhenBudgetExhausted(t *testing.T) {
	const service = "playersync_budget"
	ev := &recordingEvaler{}
	b := NewBumper(ev, service)
	errBefore := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultError))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.BumpAll(ctx, []uint64{1, 2, 2}, domainFriendList); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls := ev.taken(); len(calls) != 0 {
		t.Fatalf("预算用完后不该再发命令,实际 %d 条", len(calls))
	}
	if got := testutil.ToFloat64(bumpTotal.WithLabelValues(service, resultError)); got != errBefore+2 {
		t.Fatalf("error 计数 = %v, want %v(去重后 2 人未 bump)", got, errBefore+2)
	}
}

// ── 跨语言:C++ scene 读同一个键 ────────────────────────────────

// repoRoot 是仓库根相对本包目录(go/shared/playersync,go test 的工作目录)的路径。
var repoRoot = filepath.Join("..", "..", "..")

// cppLandingHeader 是 C++ 读侧镜像键格式的头文件(批次 D1 新建)。
const cppLandingHeader = "cpp/libs/services/scene/player/system/player_landing.h"

// cppStringConstant 从 C++ 头文件里取出 `*<name> =` 之后、第一个字面量之外的分号之前的全部字符串字面量,
// 按相邻字面量拼接的规则连起来(owner_epoch_crosslang_test.go 同名函数的最小版本)。
// 只处理 \n 与「反斜杠 + 任意字符」两种转义。文件不存在返回 ("", false);
// 文件在而常量找不到 / 没有闭合时让用例失败(改名或改写法也要被发现)。
func cppStringConstant(t *testing.T, relPath, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(relPath)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("读 %s: %v", relPath, err)
	}
	src := string(data)
	decl := "*" + name + " ="
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("%s 里找不到常量 %s(按 %q 定位;声明须保持 const char 指针写法)", relPath, name, decl)
	}

	var out strings.Builder
	inLiteral := false
	for i := start + len(decl); i < len(src); i++ {
		c := src[i]
		if !inLiteral {
			switch c {
			case '"':
				inLiteral = true
			case ';':
				return out.String(), true
			}
			continue
		}
		switch {
		case c == '"':
			inLiteral = false
		case c == '\\' && i+1 < len(src):
			i++
			if src[i] == 'n' {
				out.WriteByte('\n')
			} else {
				out.WriteByte(src[i])
			}
		default:
			out.WriteByte(c)
		}
	}
	t.Fatalf("%s 的常量 %s 没有以分号结束", relPath, name)
	return "", false
}

// Go 的键格式与 C++ 的 kSyncDigestKeyFmt 必须逐字一致(只差整数格式符):
// 两边各写各的,一旦漂移,scene 读到的是空摘要 —— 客户端每次落地都全量拉,而且一个错都不报。
func TestKeyFormatMatchesCpp(t *testing.T) {
	cppFmt, ok := cppStringConstant(t, cppLandingHeader, "kSyncDigestKeyFmt")
	if !ok {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过逐字比对")
	}
	if !strings.Contains(cppFmt, "%llu") {
		t.Fatalf("C++ kSyncDigestKeyFmt = %q:player_id 是 uint64,格式符应为 %%llu", cppFmt)
	}
	if got := strings.ReplaceAll(cppFmt, "%llu", "%d"); got != KeyFormat {
		t.Fatalf("C++ kSyncDigestKeyFmt = %q(换成 %%d 后 %q)与 Go KeyFormat %q 不一致:两边必须同步", cppFmt, got, KeyFormat)
	}

	gen, ok := cppStringConstant(t, cppLandingHeader, "kSyncDigestGenerationField")
	if !ok {
		t.Fatalf("%s 刚才还在", cppLandingHeader)
	}
	if gen != GenerationField {
		t.Fatalf("C++ kSyncDigestGenerationField = %q 与 Go GenerationField %q 不一致", gen, GenerationField)
	}
}
