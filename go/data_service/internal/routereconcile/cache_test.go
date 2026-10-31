package routereconcile

// RedisCache 的单测:miniredis 起一个进程内 Redis,用 go-redis 客户端直连(newRedisCacheFromClient),不经 go-zero。
//
// miniredis v2.37.0 不支持 INFO 的 server / replication 段、不支持 WAITAOF 与 CONFIG(核实意见已亲阅),所以:
//   - Probe / SelfCheck 与挂标记脚本「令牌对、标记不在」那一支(要调 INFO)只在真 Redis 上测(cache_integration_test.go);
//     身份解析只测纯函数 probeFromReplies(捕获的 Redis 7.2 INFO 文本);
//   - 本文件的 RedisCache 一律配 WAITAOF 0/0(不发 WAITAOF)。
// 不碰墙钟:涉及过期的用例用 miniredis.SetTime 钉住服务端的「此刻」。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// cmdCounter 用 miniredis 的 pre-hook 数顶层命令(大写命令名)。只计数,不拦截。
type cmdCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *cmdCounter) get(cmd string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[cmd]
}

// newMiniCache 起 miniredis 并装一个 WAITAOF 0/0 的 RedisCache;返回的计数器记下每个顶层命令的次数。
func newMiniCache(t *testing.T) (*RedisCache, *miniredis.Miniredis, *cmdCounter) {
	t.Helper()
	mr := miniredis.RunT(t)
	counter := &cmdCounter{n: map[string]int{}}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		counter.mu.Lock()
		counter.n[cmd]++
		counter.mu.Unlock()
		return false
	})
	zero := 0
	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst, WaitAOFNumLocal: &zero, WaitAOFNumReplicas: &zero}.Normalize()
	c := newRedisCacheFromClient(goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1}), rc)
	t.Cleanup(func() { _ = c.Close() })
	return c, mr, counter
}

func mustSet(t *testing.T, mr *miniredis.Miniredis, key, value string) {
	t.Helper()
	if err := mr.Set(key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}

func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return v
}

// TestNewRedisCacheRejectsNonNodeTypes:多键脚本与 INFO 身份按单实例定义,MappingRedis.Type 非空且不是 node 一律拒绝;
// 构造不连接(给一个连不上的地址也能构造成功)。
func TestNewRedisCacheRejectsNonNodeTypes(t *testing.T) {
	rc := config.RouteStoreConfig{Mode: config.RouteStoreModeDBFirst}.Normalize()
	if _, err := NewRedisCache(redis.RedisConf{Host: "127.0.0.1:1", Type: redis.ClusterType}, rc); err == nil {
		t.Fatal("a cluster mapping redis must be rejected")
	}
	if _, err := NewRedisCache(redis.RedisConf{Host: " ", Type: redis.NodeType}, rc); err == nil {
		t.Fatal("an empty MappingRedis.Host must be rejected")
	}
	for _, typ := range []string{"", redis.NodeType} {
		c, err := NewRedisCache(redis.RedisConf{Host: "127.0.0.1:1", Type: typ, User: "acl-user", Pass: "x"}, rc)
		if err != nil {
			t.Fatalf("Type=%q must be accepted without connecting: %v", typ, err)
		}
		if got := c.rdb.Options().Username; got != "acl-user" {
			t.Fatalf("the ACL user must be passed through (nit b), got %q", got)
		}
		if got := c.rdb.Options().MaxRetries; got != -1 && got != 0 {
			t.Fatalf("go-redis retries must be off, got MaxRetries=%d", got)
		}
		_ = c.Close()
	}
}

// TestPushSkipsCurrentRowsWithoutEval:与缓存全等(三键 + maprev)的行只走一次 MGET,零 EVALSHA;其余行才发布。
func TestPushSkipsCurrentRowsWithoutEval(t *testing.T) {
	c, mr, counter := newMiniCache(t)
	mustSet(t, mr, placement.HomeZoneKey(1), "3")
	mustSet(t, mr, placement.MapRevKey(1), "2")
	rows := []store.RouteRow{homeRow(1, 3, 2, 10), homeRow(2, 4, 1, 10)}

	got, err := c.Push(context.Background(), rows)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if got[0].Outcome != PushSkippedCurrent || got[1].Outcome != PushApplied {
		t.Fatalf("outcomes %+v, want [skipped_current applied]", got)
	}
	evals := counter.get("EVALSHA") + counter.get("EVAL")
	if evals != 2 { // 第 2 行:第一次 EVALSHA 回 NOSCRIPT(脚本还没载入),SCRIPT LOAD 后再发一次
		t.Fatalf("EVAL/EVALSHA sent %d times, want exactly 2 for the one non-current row", evals)
	}

	got, err = c.Push(context.Background(), rows)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if got[0].Outcome != PushSkippedCurrent || got[1].Outcome != PushSkippedCurrent {
		t.Fatalf("second push outcomes %+v, want both skipped_current", got)
	}
	if n := counter.get("EVALSHA") + counter.get("EVAL"); n != evals {
		t.Fatalf("a push of current rows must send no script, EVAL/EVALSHA went %d → %d", evals, n)
	}
}

// TestPushAppliesAndClassifies:逐行按发布脚本的回复分类;墓碑行与库行无效在发 Redis 之前就判掉。
func TestPushAppliesAndClassifies(t *testing.T) {
	c, mr, _ := newMiniCache(t)
	withPlacement := homeRow(10, 3, 2, 10)
	withPlacement.StorageID, withPlacement.PlacementVersion = 4, 2
	plc, err := withPlacement.PlacementValue()
	if err != nil || plc == "" {
		t.Fatalf("setup: placement value %q: %v", plc, err)
	}
	// 11:缓存 rev 更大 → superseded;12:同 rev 不同值 → conflict;13:无 maprev、遗留值不同 → legacy_conflict;
	// 14:畸形 maprev → 脚本错误回复(逐行 error)。
	mustSet(t, mr, placement.MapRevKey(11), "5")
	mustSet(t, mr, placement.HomeZoneKey(11), "3")
	mustSet(t, mr, placement.MapRevKey(12), "1")
	mustSet(t, mr, placement.HomeZoneKey(12), "8")
	mustSet(t, mr, placement.HomeZoneKey(13), "9")
	mustSet(t, mr, placement.MapRevKey(14), "abc")
	badPlacement := homeRow(16, 3, 1, 10)
	badPlacement.PlacementVersion = 3 // 没有 storage_id 却带着 version:三列不自洽
	zeroRev := homeRow(17, 3, 0, 10)
	rows := []store.RouteRow{
		withPlacement,
		homeRow(11, 3, 2, 10),
		homeRow(12, 3, 1, 10),
		homeRow(13, 3, 1, 10),
		homeRow(14, 3, 1, 10),
		{PlayerID: 15, Rev: 4}, // 墓碑
		badPlacement,
		zeroRev,
	}
	got, err := c.Push(context.Background(), rows)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	want := []PushOutcome{PushApplied, PushSuperseded, PushConflict, PushLegacyConflict, PushError, PushTombstoneSkipped, PushDBInvalid, PushDBInvalid}
	for i, w := range want {
		if got[i].PlayerID != rows[i].PlayerID || got[i].Outcome != w {
			t.Fatalf("row %d (player %d): outcome %s, want %s (detail %q)", i, rows[i].PlayerID, got[i].Outcome.label(), w.label(), got[i].Detail)
		}
	}
	if got[1].CacheRev != "5" {
		t.Fatalf("superseded must carry the cached rev, got %q", got[1].CacheRev)
	}
	if mustGet(t, mr, placement.HomeZoneKey(10)) != "3" || mustGet(t, mr, placement.Key(10)) != plc || mustGet(t, mr, placement.MapRevKey(10)) != "2" {
		t.Fatal("the applied row must write all three keys")
	}
	if mustGet(t, mr, placement.HomeZoneKey(12)) != "8" || mustGet(t, mr, placement.HomeZoneKey(13)) != "9" {
		t.Fatal("conflicting rows must not be overwritten")
	}
	for _, id := range []uint64{15, 16, 17} {
		if mr.Exists(placement.MapRevKey(id)) || mr.Exists(placement.HomeZoneKey(id)) {
			t.Fatalf("player %d must not be written (tombstone / invalid row)", id)
		}
	}
}

// TestPushReloadsOnNoScript:脚本缓存被清(SCRIPT FLUSH、Redis 重启)之后,回 NOSCRIPT 的行 SCRIPT LOAD 后只重发这些行。
func TestPushReloadsOnNoScript(t *testing.T) {
	c, mr, counter := newMiniCache(t)
	ctx := context.Background()
	got, err := c.Push(ctx, []store.RouteRow{homeRow(21, 3, 1, 10)})
	if err != nil || got[0].Outcome != PushApplied {
		t.Fatalf("first push %+v: %v", got, err)
	}
	loads := counter.get("SCRIPT")
	if err := c.rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH: %v", err)
	}
	got, err = c.Push(ctx, []store.RouteRow{homeRow(22, 4, 1, 10), homeRow(21, 3, 1, 10)})
	if err != nil {
		t.Fatalf("push after SCRIPT FLUSH: %v", err)
	}
	if got[0].Outcome != PushApplied || got[1].Outcome != PushSkippedCurrent {
		t.Fatalf("outcomes after SCRIPT FLUSH %+v, want [applied skipped_current]", got)
	}
	if mustGet(t, mr, placement.HomeZoneKey(22)) != "4" {
		t.Fatal("the row must be published after the script reload")
	}
	// SCRIPT FLUSH 本身也是一次 SCRIPT;之后至少还要一次 SCRIPT LOAD。
	if n := counter.get("SCRIPT"); n < loads+2 {
		t.Fatalf("SCRIPT commands %d → %d, want a SCRIPT LOAD after NOSCRIPT", loads, n)
	}
}

// TestPushTombstoneSkipped:墓碑行(home 与落点都为空)不推:推了会造出只有 maprev 的键,导入 / 审计工具判它畸形。
func TestPushTombstoneSkipped(t *testing.T) {
	c, mr, counter := newMiniCache(t)
	got, err := c.Push(context.Background(), []store.RouteRow{{PlayerID: 31, Rev: 2, UpdatedBy: "test"}})
	if err != nil || len(got) != 1 || got[0].Outcome != PushTombstoneSkipped {
		t.Fatalf("push %+v: %v", got, err)
	}
	if mr.Exists(placement.MapRevKey(31)) || counter.get("MGET") != 0 || counter.get("EVALSHA") != 0 {
		t.Fatal("a tombstone must not touch the mapping redis at all")
	}
}

// TestReadyCASAndCAD:就绪标记的 CAS / CAD 只在原值相等时生效;水位键缺席时 SET NX、在时按原值 CAS。
func TestReadyCASAndCAD(t *testing.T) {
	c, mr, _ := newMiniCache(t)
	ctx := context.Background()
	mustSet(t, mr, placement.RouteReadyKey, "a")
	if ok, err := c.CompareAndSetReady(ctx, "b", "c"); err != nil || ok {
		t.Fatalf("CAS with a stale old value: ok=%t err=%v", ok, err)
	}
	if ok, err := c.CompareAndSetReady(ctx, "a", "b"); err != nil || !ok || mustGet(t, mr, placement.RouteReadyKey) != "b" {
		t.Fatalf("CAS with the current value: ok=%t err=%v", ok, err)
	}
	if ok, err := c.CompareAndDeleteReady(ctx, "a"); err != nil || ok || !mr.Exists(placement.RouteReadyKey) {
		t.Fatalf("CAD with a stale value must not delete: ok=%t err=%v", ok, err)
	}
	if ok, err := c.CompareAndDeleteReady(ctx, "b"); err != nil || !ok || mr.Exists(placement.RouteReadyKey) {
		t.Fatalf("CAD with the current value: ok=%t err=%v", ok, err)
	}

	if ok, err := c.AdvanceWatermark(ctx, nil, "w1"); err != nil || !ok {
		t.Fatalf("SET NX on an absent watermark key: ok=%t err=%v", ok, err)
	}
	if ok, err := c.AdvanceWatermark(ctx, nil, "w2"); err != nil || ok || mustGet(t, mr, placement.RouteWatermarkKey) != "w1" {
		t.Fatalf("SET NX must not overwrite: ok=%t err=%v", ok, err)
	}
	prev := "w1"
	if ok, err := c.AdvanceWatermark(ctx, &prev, "w3"); err != nil || !ok || mustGet(t, mr, placement.RouteWatermarkKey) != "w3" {
		t.Fatalf("CAS from the current value: ok=%t err=%v", ok, err)
	}
}

// TestArmReadyNXDoesNotOverwrite(M2):令牌不符回 0、标记已存在回 2,都零写入(两支都在脚本调 INFO 之前返回,miniredis 测得到);
// 令牌不合法时不发脚本。「令牌对、标记不在」那一支要调 INFO,在 TestRealRedisArmWithWaitAOF 里测。
func TestArmReadyNXDoesNotOverwrite(t *testing.T) {
	c, mr, counter := newMiniCache(t)
	ctx := context.Background()
	m := placement.RouteReadyMarker{Generation: testGen, RedisRunID: idA.RunID, MasterReplID: idA.MasterReplID, SetAtMs: 1, WatermarkMs: 1, Leader: testLeader}
	token := strings.Repeat("1", placement.RouteWarmTokenLen)
	if err := c.SetWarmToken(ctx, token); err != nil {
		t.Fatalf("set warm token: %v", err)
	}
	if mustGet(t, mr, placement.RouteWarmKey) != token {
		t.Fatal("SetWarmToken must write mapping:route:warm")
	}

	out, err := c.ArmReady(ctx, otherToken, m)
	if err != nil || out != placement.RouteReadyArmTokenMismatch || mr.Exists(placement.RouteReadyKey) {
		t.Fatalf("token mismatch: out=%s err=%v present=%t", out, err, mr.Exists(placement.RouteReadyKey))
	}

	mustSet(t, mr, placement.RouteReadyKey, "old")
	out, err = c.ArmReady(ctx, token, m)
	if err != nil || out != placement.RouteReadyArmAlreadyPresent || mustGet(t, mr, placement.RouteReadyKey) != "old" {
		t.Fatalf("already present: out=%s err=%v value=%q", out, err, mustGet(t, mr, placement.RouteReadyKey))
	}

	evals := counter.get("EVAL")
	if _, err := c.ArmReady(ctx, "not-a-token", m); err == nil || counter.get("EVAL") != evals {
		t.Fatalf("a malformed token must be rejected before sending the script (err=%v)", err)
	}
	if err := c.SetWarmToken(ctx, "NOT HEX"); err == nil {
		t.Fatal("SetWarmToken must reject a malformed token")
	}
}

// TestReadControlDistinguishesWrongType:三把控制键一次读回;就绪标记键在但不是 string 时 Ready 为 nil 而 ReadyWrongType 为 true,
// DeleteReadyWrongType 删掉它。
func TestReadControlDistinguishesWrongType(t *testing.T) {
	c, mr, _ := newMiniCache(t)
	ctx := context.Background()
	ctl, err := c.ReadControl(ctx)
	if err != nil || ctl.Ready != nil || ctl.ReadyWrongType || ctl.Warm != nil || ctl.Watermark != nil {
		t.Fatalf("empty instance: %+v err=%v", ctl, err)
	}
	mustSet(t, mr, placement.RouteWarmKey, "t")
	mustSet(t, mr, placement.RouteWatermarkKey, "w")
	if _, err := mr.Lpush(placement.RouteReadyKey, "x"); err != nil {
		t.Fatalf("lpush: %v", err)
	}
	ctl, err = c.ReadControl(ctx)
	if err != nil || ctl.Ready != nil || !ctl.ReadyWrongType || ctl.Warm == nil || *ctl.Warm != "t" || ctl.Watermark == nil || *ctl.Watermark != "w" {
		t.Fatalf("wrong-type marker: %+v err=%v", ctl, err)
	}
	if err := c.DeleteReadyWrongType(ctx); err != nil || mr.Exists(placement.RouteReadyKey) {
		t.Fatalf("DeleteReadyWrongType: err=%v present=%t", err, mr.Exists(placement.RouteReadyKey))
	}
	mustSet(t, mr, placement.RouteReadyKey, "m")
	ctl, err = c.ReadControl(ctx)
	if err != nil || ctl.Ready == nil || *ctl.Ready != "m" || ctl.ReadyWrongType || ctl.WatermarkWrongType {
		t.Fatalf("string marker: %+v err=%v", ctl, err)
	}

	// 水位键同理(核实意见 A1-8):不是 string 时 Watermark 为 nil 而 WatermarkWrongType 为 true,DeleteWatermarkWrongType 删掉它。
	mr.Del(placement.RouteWatermarkKey)
	if _, err := mr.SAdd(placement.RouteWatermarkKey, "x"); err != nil {
		t.Fatalf("sadd: %v", err)
	}
	ctl, err = c.ReadControl(ctx)
	if err != nil || ctl.Watermark != nil || !ctl.WatermarkWrongType || ctl.ReadyWrongType {
		t.Fatalf("wrong-type watermark: %+v err=%v", ctl, err)
	}
	if err := c.DeleteWatermarkWrongType(ctx); err != nil || mr.Exists(placement.RouteWatermarkKey) {
		t.Fatalf("DeleteWatermarkWrongType: err=%v present=%t", err, mr.Exists(placement.RouteWatermarkKey))
	}
	ctl, err = c.ReadControl(ctx)
	if err != nil || ctl.Watermark != nil || ctl.WatermarkWrongType {
		t.Fatalf("absent watermark: %+v err=%v", ctl, err)
	}
}

// TestMirrorMergedIntoAddOnly:合走标记只增:缺席的 SET NX,同值计 present,不同值 / 非 string 计冲突,一概不覆盖、不删除。
func TestMirrorMergedIntoAddOnly(t *testing.T) {
	c, mr, _ := newMiniCache(t)
	ctx := context.Background()
	mustSet(t, mr, placement.MergedIntoKey(6), `{"dst":99,"run_id":"other"}`)
	if _, err := mr.Lpush(placement.MergedIntoKey(9), "x"); err != nil {
		t.Fatalf("lpush: %v", err)
	}
	rows := []store.MergedIntoRow{
		{SrcZone: 5, DstZone: 7, RunID: "r1"},
		{SrcZone: 6, DstZone: 8, RunID: "r2"},
		{SrcZone: 9, DstZone: 8, RunID: "r3"},
		{SrcZone: 10, DstZone: 0, RunID: "r4"}, // 编码不了(dst 为 0)
	}
	res, err := c.MirrorMergedInto(ctx, rows)
	if err != nil || res.Added != 1 || res.Present != 0 || res.Conflicts != 2 || res.Errors != 1 {
		t.Fatalf("first mirror %+v err=%v, want added=1 conflicts=2 errors=1", res, err)
	}
	want, _ := placement.EncodeMergedIntoValue(7, "r1")
	if mustGet(t, mr, placement.MergedIntoKey(5)) != want || mustGet(t, mr, placement.MergedIntoKey(6)) != `{"dst":99,"run_id":"other"}` {
		t.Fatal("add-only: the missing key is added and the conflicting one left as is")
	}
	res, err = c.MirrorMergedInto(ctx, rows[:2])
	if err != nil || res.Added != 0 || res.Present != 1 || res.Conflicts != 1 {
		t.Fatalf("second mirror %+v err=%v, want present=1 conflicts=1", res, err)
	}
}

// redisReplyError 模拟 Redis 的错误回复(实现 goredis.Error;go-redis 没有导出可直接构造的错误回复类型)。
type redisReplyError string

func (e redisReplyError) Error() string { return string(e) }

func (redisReplyError) RedisError() {}

// captured Redis 7.2 INFO 文本(删减到用得到的行,CRLF 原样)。
const (
	info72Server = "# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\nos:Linux 6.6.12-linuxkit x86_64\r\n" +
		"process_id:1\r\nrun_id:5c3b1f1e8a1f2e3d4c5b6a7980716253443526a1\r\ntcp_port:6379\r\n"

	info72Replication = "# Replication\r\nrole:master\r\nconnected_slaves:0\r\n" +
		"master_failover_state:no-failover\r\nmaster_replid:8f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c\r\n" +
		"master_replid2:0000000000000000000000000000000000000000\r\nmaster_repl_offset:0\r\n"

	info72Stats = "# Stats\r\ntotal_connections_received:3\r\nexpired_keys:0\r\nevicted_keys:7\r\nevicted_clients:0\r\n"
)

// TestParseRedisInfo:Probe 的纯解析部分,用捕获的 Redis 7.2 INFO 文本表驱动(miniredis 不支持 INFO server / replication)。
func TestParseRedisInfo(t *testing.T) {
	ok := func(text string) infoReply { return infoReply{text: text} }
	cfg := func(name, v string) configReply { return configReply{vals: map[string]string{name: v}} }
	policy, debug := cfg("maxmemory-policy", "noeviction"), cfg("enable-debug-command", "no")
	var replyErr error = redisReplyError("ERR unknown command 'CONFIG'")

	p, err := probeFromReplies(ok(info72Server), ok(info72Replication), ok(info72Stats), policy, debug)
	if err != nil {
		t.Fatalf("captured 7.2 text: %v", err)
	}
	if p.Identity.RunID != "5c3b1f1e8a1f2e3d4c5b6a7980716253443526a1" || p.Identity.Role != "master" ||
		p.Identity.MasterReplID != "8f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c" ||
		p.EvictedKeys != 7 || p.MaxmemoryPolicy != "noeviction" || p.DebugCommand != "no" {
		t.Fatalf("parsed %+v", p)
	}

	// LF 也认;role:slave 照样解析(由 leader 判「不是主」);master_replid2 不会误中 master_replid。
	lfRepl := strings.ReplaceAll(strings.Replace(info72Replication, "role:master", "role:slave", 1), "\r\n", "\n")
	if p, err := probeFromReplies(ok(strings.ReplaceAll(info72Server, "\r\n", "\n")), ok(lfRepl), ok(info72Stats), policy, debug); err != nil ||
		p.Identity.Role != "slave" || p.Identity.MasterReplID != "8f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c" {
		t.Fatalf("LF / slave: %+v err=%v", p, err)
	}

	// 缺 run_id → 身份读不到。
	noRunID := strings.Replace(info72Server, "run_id:5c3b1f1e8a1f2e3d4c5b6a7980716253443526a1\r\n", "", 1)
	if _, err := probeFromReplies(ok(noRunID), ok(info72Replication), ok(info72Stats), policy, debug); err == nil {
		t.Fatal("a missing run_id must fail the probe")
	}
	// INFO 任一段报错 → 整体报错。
	if _, err := probeFromReplies(ok(info72Server), infoReply{err: errors.New("i/o timeout")}, ok(info72Stats), policy, debug); err == nil {
		t.Fatal("an INFO replication error must fail the probe")
	}
	// 缺 evicted_keys → -1(取不到;leader 按淘汰异常 fail-closed)。
	if p, err := probeFromReplies(ok(info72Server), ok(info72Replication), ok("# Stats\r\n"), policy, debug); err != nil || p.EvictedKeys != -1 {
		t.Fatalf("missing evicted_keys: %+v err=%v", p, err)
	}
	// CONFIG 被禁用 / 改名(错误回复)→ ""(未知);网络错误 → 整体报错。
	if p, err := probeFromReplies(ok(info72Server), ok(info72Replication), ok(info72Stats),
		configReply{err: replyErr}, configReply{err: replyErr}); err != nil || p.MaxmemoryPolicy != "" || p.DebugCommand != "" {
		t.Fatalf("CONFIG disabled: %+v err=%v", p, err)
	}
	if _, err := probeFromReplies(ok(info72Server), ok(info72Replication), ok(info72Stats),
		configReply{err: errors.New("connection reset")}, debug); err == nil {
		t.Fatal("a CONFIG network error must fail the probe")
	}
}

// TestCheckWaitAOFReply:与 routing 的 checkWaitAOFReply 同一判据(两处同改)。
func TestCheckWaitAOFReply(t *testing.T) {
	cases := []struct {
		vals []any
		ok   bool
	}{
		{[]any{int64(1), int64(0)}, true},
		{[]any{int64(1), int64(1)}, true},
		{[]any{int64(0), int64(0)}, false},
		{[]any{int64(1)}, false},
		{[]any{"1", int64(0)}, false},
		{[]any{int64(-1), int64(0)}, false},
	}
	for _, tc := range cases {
		if err := checkWaitAOFReply(tc.vals, 1, 0); (err == nil) != tc.ok {
			t.Fatalf("WAITAOF reply %v: err=%v, want ok=%t", tc.vals, err, tc.ok)
		}
	}
	if err := checkWaitAOFReply([]any{int64(1), int64(0)}, 1, 1); err == nil {
		t.Fatal("too few replica acks must fail")
	}
}
