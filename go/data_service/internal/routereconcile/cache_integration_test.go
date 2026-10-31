//go:build integration

package routereconcile

// RedisCache 打真 Redis 的集成测试(D1 第 1b-2 批,docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §4.4、§13.1)。
//
// 为什么 miniredis 不够:INFO 的 server / replication 段、WAITAOF、CONFIG GET 在 miniredis 里都没有;挂标记脚本、读方脚本
// 在脚本里调 INFO 的语义(规格 §2 M1 的「未核实」项)、PEXPIREAT 的绝对时刻只有真 Redis 能证。
//
// 跑法(一次性 Redis,**不要**指向服务在用的 mapping Redis:用例会写 mapping:route:ready / warm / watermark,并 SCRIPT FLUSH):
//
//	docker run -d --rm --name ds-it-redis -p 6391:6379 redis:7.2 --appendonly yes
//	$env:DATA_SERVICE_IT_REDIS_HOST='127.0.0.1:6391'; $env:DATA_SERVICE_IT_REQUIRE='1'
//	go test -tags=integration -count=1 -v ./internal/routereconcile/... -run TestRealRedis
//
// 只在显式给了 DATA_SERVICE_IT_REDIS_HOST 时跑(不读 yaml 里那个开发用的 mapping Redis);跑前要求 mapping:route:* 为空、
// 本文件用到的玩家键都不在,否则 Skip;跑后只删本文件写过的键,绝不 FLUSHDB。
// 加 DATA_SERVICE_IT_REQUIRE=1:依赖缺失 / 不可达从 SKIP 变成 FAIL(PASS / FAIL / SKIP 必须可区分,规格 §6.5 同口径)。
//
// 需要 Redis 的缺省配置:maxmemory-policy=noeviction、enable-debug-command=no(Redis 7 缺省即如此);WAITAOF 用例要 --appendonly yes。
// 不做「重启 Redis 后读方判不可信」(需要重启实例),改为 TestRealRedisMarkerWithForeignIdentityIsMismatch。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

const (
	// envCacheITRedisHost 是一次性 Redis(>= 7.2)的地址。
	envCacheITRedisHost = "DATA_SERVICE_IT_REDIS_HOST"

	// envCacheITRequire 设为 1 时,被跳过的用例判失败。
	envCacheITRequire = "DATA_SERVICE_IT_REQUIRE"

	cacheITTimeout = 5 * time.Second
)

// 本文件写的玩家 id 与 zone(远离开发数据常用的小号段)。
var (
	cacheITPlayers = []uint64{9_310_001, 9_310_002, 9_310_003}
	cacheITZones   = []uint32{9301, 9302}
)

// requireCacheIT 必须是用例的第一句:它登记的清理最后一个执行,用例被跳过且设了 DATA_SERVICE_IT_REQUIRE=1 就判失败。
func requireCacheIT(t *testing.T) {
	t.Helper()
	if os.Getenv(envCacheITRequire) != "1" {
		return
	}
	t.Cleanup(func() {
		if t.Skipped() {
			t.Errorf("%s=1: this integration test was skipped (a dependency is missing or unusable, see the SKIP reason above); treated as FAIL",
				envCacheITRequire)
		}
	})
}

// cacheITKeys 是本文件会写的全部键(跑前要求不在、跑后删掉)。
func cacheITKeys() []string {
	keys := []string{placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey}
	for _, id := range cacheITPlayers {
		keys = append(keys, placement.PublishRouteKeys(id)...)
	}
	return keys
}

// newRealCache 接一个指向一次性 Redis 的 RedisCache(WAITAOF 按入参),另给一个裸客户端做断言。
func newRealCache(t *testing.T, waitLocal, waitReplicas int) (*RedisCache, *goredis.Client) {
	t.Helper()
	host := os.Getenv(envCacheITRedisHost)
	if host == "" {
		t.Skipf("needs %s (a throwaway Redis >= 7.2 with --appendonly yes): these cases write mapping:route:* and SCRIPT FLUSH", envCacheITRedisHost)
	}
	raw := goredis.NewClient(&goredis.Options{Addr: host})
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()
	if err := raw.Ping(ctx).Err(); err != nil {
		_ = raw.Close()
		t.Skipf("Redis %s unreachable: %v", host, err)
	}
	if keys, _, err := raw.Scan(ctx, 0, "mapping:route:*", 100).Result(); err != nil || len(keys) > 0 {
		_ = raw.Close()
		t.Skipf("Redis %s must have no mapping:route:* keys (got %v, err %v): use a throwaway instance", host, keys, err)
	}
	if n, err := raw.Exists(ctx, cacheITKeys()...).Result(); err != nil || n > 0 {
		_ = raw.Close()
		t.Skipf("Redis %s already holds %d of this test's keys (err %v): use a throwaway instance", host, n, err)
	}
	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, WaitAOFNumLocal: &waitLocal, WaitAOFNumReplicas: &waitReplicas}.Normalize()
	c, err := NewRedisCache(redis.RedisConf{Host: host, Type: redis.NodeType}, rc)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), cacheITTimeout)
		defer cleanCancel()
		if err := raw.Del(cleanCtx, cacheITKeys()...).Err(); err != nil {
			t.Errorf("cleanup of this test's keys failed (delete them by hand): %v", err)
		}
		_ = c.Close()
		_ = raw.Close()
	})
	return c, raw
}

// realIdentity 用普通 INFO 命令(不经脚本)读本实例身份,与 Probe / 脚本里取到的对照。
func realIdentity(t *testing.T, raw *goredis.Client) placement.RedisInstanceIdentity {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()
	srv, err := raw.Info(ctx, "server").Result()
	if err != nil {
		t.Fatalf("INFO server: %v", err)
	}
	repl, err := raw.Info(ctx, "replication").Result()
	if err != nil {
		t.Fatalf("INFO replication: %v", err)
	}
	id, err := placement.ParseRedisInstanceIdentity(srv, repl)
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	if id.Role != "master" {
		t.Fatalf("these cases need a master, got role %q", id.Role)
	}
	return id
}

func realMarker(t *testing.T, id placement.RedisInstanceIdentity) placement.RouteReadyMarker {
	t.Helper()
	return placement.RouteReadyMarker{
		Generation: testGen, RedisRunID: id.RunID, MasterReplID: id.MasterReplID,
		SetAtMs: 1_700_000_000_000, WatermarkMs: 1_700_000_000_000, Leader: testLeader,
	}
}

// TestRealRedisIdentityParses:真 INFO 能解析,role=master,与普通 INFO 命令读到的身份一致;缺省配置下 CONFIG GET
// 回 noeviction 与 enable-debug-command=no,evicted_keys 取得到。
func TestRealRedisIdentityParses(t *testing.T) {
	requireCacheIT(t)
	c, raw := newRealCache(t, 0, 0)
	want := realIdentity(t, raw)
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()
	p, err := c.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.Identity != want {
		t.Fatalf("Probe identity %+v, INFO says %+v", p.Identity, want)
	}
	if p.EvictedKeys < 0 {
		t.Fatalf("evicted_keys must be readable from INFO stats, got %d", p.EvictedKeys)
	}
	if p.MaxmemoryPolicy != "noeviction" || p.DebugCommand != "no" {
		t.Fatalf("maxmemory-policy=%q enable-debug-command=%q: run the throwaway Redis with its defaults (noeviction / no)",
			p.MaxmemoryPolicy, p.DebugCommand)
	}
}

// TestRealRedisArmWithWaitAOF(M2):挂标记脚本在真 Redis 上的四种回码 + 同连接 WAITAOF 1/0 的确认:
// 令牌不符回 0、身份不符回 3(都零写入);令牌对、标记不在、身份相符回 1 并经 WAITAOF 确认;再挂一次回 2(不覆写)。
// 挂上之后读方同一段脚本判 trusted。WAITAOF 报错(没开 AOF)时 ArmReady 会按原值撤掉刚挂的标记并报错 —— 用例随之 FAIL,
// 按提示用 --appendonly yes 重起一次性 Redis。
func TestRealRedisArmWithWaitAOF(t *testing.T) {
	requireCacheIT(t)
	c, raw := newRealCache(t, 1, 0)
	id := realIdentity(t, raw)
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()
	token := strings.Repeat("a1", placement.RouteWarmTokenLen/2)
	if err := c.SetWarmToken(ctx, token); err != nil {
		t.Fatalf("SetWarmToken: %v", err)
	}
	m := realMarker(t, id)

	if out, err := c.ArmReady(ctx, otherToken, m); err != nil || out != placement.RouteReadyArmTokenMismatch {
		t.Fatalf("wrong token: out=%s err=%v, want token_mismatch", out, err)
	}
	foreign := m
	foreign.RedisRunID = strings.Repeat("0", 40)
	if out, err := c.ArmReady(ctx, token, foreign); err != nil || out != placement.RouteReadyArmIdentityChanged {
		t.Fatalf("foreign identity: out=%s err=%v, want identity_changed", out, err)
	}
	if n, _ := raw.Exists(ctx, placement.RouteReadyKey).Result(); n != 0 {
		t.Fatal("refused arms must not write the marker")
	}

	out, err := c.ArmReady(ctx, token, m)
	if err != nil || out != placement.RouteReadyArmArmed {
		t.Fatalf("arm: out=%s err=%v, want armed and confirmed by WAITAOF 1 0 (is the Redis running with --appendonly yes?)", out, err)
	}
	want, _ := placement.EncodeRouteReadyMarker(m)
	if got, _ := raw.Get(ctx, placement.RouteReadyKey).Result(); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
	if out, err := c.ArmReady(ctx, token, m); err != nil || out != placement.RouteReadyArmAlreadyPresent {
		t.Fatalf("second arm: out=%s err=%v, want already_present", out, err)
	}

	rr, err := c.SelfCheck(ctx)
	if err != nil {
		t.Fatalf("SelfCheck (INFO inside the read script): %v", err)
	}
	if v, _ := placement.VerifyRouteReady(rr); v != placement.RouteReadyTrusted {
		t.Fatalf("self-check verdict %s, want trusted (read %+v)", v, rr)
	}
}

// TestRealRedisEvalShaAfterScriptFlush:脚本缓存被清(SCRIPT FLUSH,等同 Redis 重启后)之后,Push 回 NOSCRIPT 的行
// SCRIPT LOAD 后重发成功。**会清掉这个实例上的全部脚本缓存**:只对一次性 Redis 跑。
func TestRealRedisEvalShaAfterScriptFlush(t *testing.T) {
	requireCacheIT(t)
	c, raw := newRealCache(t, 0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()
	first := homeRow(cacheITPlayers[0], 3, 1, 10)
	got, err := c.Push(ctx, []store.RouteRow{first})
	if err != nil || got[0].Outcome != PushApplied {
		t.Fatalf("first push %+v: %v", got, err)
	}
	if err := raw.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH: %v", err)
	}
	second := homeRow(cacheITPlayers[1], 4, 2, 10)
	second.StorageID, second.PlacementVersion = 4, 1
	got, err = c.Push(ctx, []store.RouteRow{second, first})
	if err != nil {
		t.Fatalf("push after SCRIPT FLUSH: %v", err)
	}
	if got[0].Outcome != PushApplied || got[1].Outcome != PushSkippedCurrent {
		t.Fatalf("outcomes after SCRIPT FLUSH %+v, want [applied skipped_current]", got)
	}
	plc, _ := second.PlacementValue()
	vals, err := raw.MGet(ctx, placement.PublishRouteKeys(second.PlayerID)...).Result()
	if err != nil || vals[0] != "4" || vals[1] != plc || vals[2] != "2" {
		t.Fatalf("published keys %v (err %v), want [4 %s 2]", vals, err, plc)
	}
}

// TestRealRedisMarkerWithForeignIdentityIsMismatch(M1):带别的实例身份的标记(旧备份恢复、被拷到别的实例、重启前挂的)
// 在读方同一段脚本下判 identity_mismatch;换成本实例身份才判 trusted。用来代替「重启 Redis 后读方判不可信」(那要重启实例)。
func TestRealRedisMarkerWithForeignIdentityIsMismatch(t *testing.T) {
	requireCacheIT(t)
	c, raw := newRealCache(t, 0, 0)
	id := realIdentity(t, raw)
	ctx, cancel := context.WithTimeout(context.Background(), cacheITTimeout)
	defer cancel()

	rr, err := c.SelfCheck(ctx)
	if err != nil {
		t.Fatalf("SelfCheck: %v", err)
	}
	if v, _ := placement.VerifyRouteReady(rr); v != placement.RouteReadyAbsent || rr.Identity != id {
		t.Fatalf("no marker: verdict %s identity %+v, want absent and %+v", v, rr.Identity, id)
	}

	for name, mutate := range map[string]func(*placement.RouteReadyMarker){
		"other run_id":        func(m *placement.RouteReadyMarker) { m.RedisRunID = strings.Repeat("f", 40) },
		"other master_replid": func(m *placement.RouteReadyMarker) { m.MasterReplID = strings.Repeat("e", 40) },
	} {
		m := realMarker(t, id)
		mutate(&m)
		raw1, err := placement.EncodeRouteReadyMarker(m)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if err := raw.Set(ctx, placement.RouteReadyKey, raw1, 0).Err(); err != nil {
			t.Fatalf("%s: set marker: %v", name, err)
		}
		rr, err := c.SelfCheck(ctx)
		if err != nil {
			t.Fatalf("%s: SelfCheck: %v", name, err)
		}
		if v, _ := placement.VerifyRouteReady(rr); v != placement.RouteReadyIdentityMismatch {
			t.Fatalf("%s: verdict %s, want identity_mismatch", name, v)
		}
	}

	own, _ := placement.EncodeRouteReadyMarker(realMarker(t, id))
	if err := raw.Set(ctx, placement.RouteReadyKey, own, 0).Err(); err != nil {
		t.Fatalf("set own marker: %v", err)
	}
	rr, err = c.SelfCheck(ctx)
	if err != nil {
		t.Fatalf("SelfCheck: %v", err)
	}
	if v, _ := placement.VerifyRouteReady(rr); v != placement.RouteReadyTrusted {
		t.Fatalf("own-identity marker: verdict %s, want trusted", v)
	}
	ctl, err := c.ReadControl(ctx)
	if err != nil || ctl.Ready == nil || *ctl.Ready != own || ctl.ReadyWrongType {
		t.Fatalf("ReadControl %+v err=%v", ctl, err)
	}
}
