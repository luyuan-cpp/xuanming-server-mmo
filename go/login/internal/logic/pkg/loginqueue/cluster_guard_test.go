package loginqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"login/internal/logic/pkg/locker"
	"shared/clusterredis"
	"shared/gatepick"
	"shared/redisslot"
)

// 本文件钉住排队与 gate 标记在控制集群上的两件事(docs/design/redis-cluster-all-domains-20261110.md §4.3、§4.4、§5.1):
//   - 键形:一个 zone 的排队键、放行锁同槽,一个 zone 的 gate 标记同槽,tag 是登记过的族;
//   - 每一条命令都过得了控制集群句柄的发送前守卫(多键命令同 tag、pipeline 里没有非幂等命令),放行流水线走专用句柄。
//
// 其余用例的夹具直接连 miniredis、不挂守卫(它们要在同一个句柄上挂故障注入钩子);这里按生产的接线再跑一遍主流程。

// 排队族的键名是 login 各副本之间、以及与运维排查命令之间的契约。
func TestQueueKeys_Golden(t *testing.T) {
	for got, want := range map[string]string{
		zoneZSetKey(3):          "queue:{q:3}",
		admittedZSetKey(3):      "admitted:zset:{q:3}",
		admitKey(3, "qid"):      "admit:{q:3}:qid",
		metaKey(3, "qid"):       "queue:meta:{q:3}:qid",
		tokenIndexKey(3, "tok"): "queue:token:{q:3}:tok",
		ZoneDispatcherLockKey("dispatcher:lock:login_queue", 3): "dispatcher:lock:login_queue:{q:3}",
	} {
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// 一个 zone 的排队键与放行锁必须同槽:入队的 MULTI、轮询与放行两段脚本各碰三四把键。
// queueId 与令牌来自外部输入的那一半(令牌)即使带花括号,也不能把键带到别的槽上:tag 写在键里第一个花括号的位置。
func TestQueueKeys_OneZoneSharesOneSlotWhateverTheMemberLooksLike(t *testing.T) {
	const zone = 7
	wantSlot := redisslot.HashSlot(zoneZSetKey(zone))
	keys := []string{zoneZSetKey(zone), admittedZSetKey(zone), ZoneDispatcherLockKey("dispatcher:lock:login_queue", zone)}
	for _, member := range []string{"", "plain", "{other}", "a{b}c", "}{", "{q:8}", "{123}"} {
		keys = append(keys, admitKey(zone, member), metaKey(zone, member), tokenIndexKey(zone, member))
	}
	for _, key := range keys {
		tag, tagged := redisslot.HashTag(key)
		if !tagged || tag != "q:7" || !redisslot.IsControlTag(tag) {
			t.Fatalf("%q: tag=%q tagged=%v, want the registered control tag q:7", key, tag, tagged)
		}
		if redisslot.HashSlot(key) != wantSlot {
			t.Fatalf("%q is not in the slot of %q", key, zoneZSetKey(zone))
		}
	}
	if err := clusterredis.ControlClusterKeys.CheckKeys(keys); err != nil {
		t.Fatalf("the control-cluster guard must accept one zone's queue keys in one command: %v", err)
	}
	// 不同 zone 不共用 tag:它们之间没有原子操作,可以落在不同主节点上。
	if tag, _ := redisslot.HashTag(zoneZSetKey(zone + 1)); tag != "q:8" {
		t.Fatalf("zone 8 queue tag = %q, want q:8", tag)
	}
}

// newGuardedQueue 按生产的接线建队列:带发送前守卫的控制集群句柄,加放行流水线专用的不带守卫的句柄,都连同一个 miniredis。
func newGuardedQueue(t *testing.T) (*Queue, redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	conf := clusterredis.Config{Addrs: []string{mr.Addr()}}
	guarded, err := clusterredis.New(conf, clusterredis.ControlClusterKeys)
	if err != nil {
		t.Fatalf("clusterredis.New: %v", err)
	}
	pinned, err := clusterredis.NewPinned(conf)
	if err != nil {
		t.Fatalf("clusterredis.NewPinned: %v", err)
	}
	t.Cleanup(func() { _ = guarded.Close(); _ = pinned.Close() })
	q := New(guarded, time.Hour, 60*time.Second, []byte("test-secret"))
	q.UseAdmitPipelineClient(pinned)
	return q, guarded, mr
}

func enqueueForZone(t *testing.T, q *Queue, zone uint32, n int) []*EnqueueResult {
	t.Helper()
	entries := make([]*EnqueueResult, 0, n)
	for i := 0; i < n; i++ {
		res, err := q.Enqueue(context.Background(), zone, fmt.Sprintf("acct-%d", i), "dev")
		if err != nil {
			t.Fatalf("enqueue #%d: %v", i, err)
		}
		entries = append(entries, res)
	}
	return entries
}

// 排队的每一条命令都要过控制集群的发送前守卫。守卫拒绝时命令根本不发,所以整条流程走通、状态对得上,就说明没有一条被拒。
// 5 个人:放行的第一块只有探路的一条(单发),其余 4 条走放行流水线。
func TestQueueFlow_PassesTheControlClusterGuard(t *testing.T) {
	q, _, mr := newGuardedQueue(t)
	ctx := context.Background()
	const zone, players = 1, 5

	entries := enqueueForZone(t, q, zone, players)
	if n, err := q.QueueLen(ctx, zone); err != nil || n != players {
		t.Fatalf("QueueLen = (%d, %v), want (%d, nil)", n, err, players)
	}
	for _, e := range entries {
		st, err := q.Poll(ctx, e.QueueToken, zone, signMustNotRun(t))
		if err != nil || st.Status != StatusQueueing || st.Total != players {
			t.Fatalf("poll while queueing = (%+v, %v), want QUEUEING total=%d", st, err, players)
		}
	}
	if ok, err := q.TryReserveFastPathSlot(ctx, zone, 100); err != nil || !ok {
		t.Fatalf("TryReserveFastPathSlot = (%v, %v), want (true, nil)", ok, err)
	}
	if n, err := q.PopAdmit(ctx, zone, players, noBudgetLimit, pickZone1Gate); err != nil || n != players {
		t.Fatalf("PopAdmit = (%d, %v), want (%d, nil)", n, err, players)
	}
	if n, err := q.AdmittedCount(ctx, zone); err != nil || n != players+1 {
		t.Fatalf("AdmittedCount = (%d, %v), want (%d, nil): %d admits + 1 fast-path placeholder", n, err, players+1, players)
	}
	for _, e := range entries {
		st, err := q.Poll(ctx, e.QueueToken, zone, stubSignFn)
		if err != nil || st.Status != StatusAdmitted || st.Admit == nil {
			t.Fatalf("poll after admit = (%+v, %v), want ADMITTED", st, err)
		}
		// 重取窗口里同一令牌再取一次:按 meta 里记下的槽重签。
		st, err = q.Poll(ctx, e.QueueToken, zone, stubSignFn)
		if err != nil || st.Status != StatusAdmitted || st.Admit == nil {
			t.Fatalf("retake = (%+v, %v), want ADMITTED again", st, err)
		}
	}
	if n, err := q.QueueLen(ctx, zone); err != nil || n != 0 {
		t.Fatalf("QueueLen after admit = (%d, %v), want (0, nil)", n, err)
	}
	// 写下的键全部在本 zone 的 tag 下。
	for _, key := range mr.Keys() {
		if tag, tagged := redisslot.HashTag(key); !tagged || tag != "q:1" {
			t.Fatalf("key %q was written outside the zone's queue tag", key)
		}
	}
}

// 放行流水线必须换成不带守卫的句柄(UseAdmitPipelineClient):带守卫的句柄拒绝 pipeline 里的 EVAL,每一块流水线整批被拒。
// 被拒的条目原样留在队列,不丢、不半截写入;漏接线的后果是每拍只放探路的那一条,不是放错。
func TestPopAdmit_GuardedClientAloneRejectsThePipelineAndKeepsEntriesQueued(t *testing.T) {
	mr := miniredis.RunT(t)
	guarded, err := clusterredis.New(clusterredis.Config{Addrs: []string{mr.Addr()}}, clusterredis.ControlClusterKeys)
	if err != nil {
		t.Fatalf("clusterredis.New: %v", err)
	}
	t.Cleanup(func() { _ = guarded.Close() })
	q := New(guarded, time.Hour, 60*time.Second, []byte("test-secret")) // 没调 UseAdmitPipelineClient
	ctx := context.Background()
	enqueueForZone(t, q, 1, 4)

	n, err := q.PopAdmit(ctx, 1, 4, noBudgetLimit, pickZone1Gate)
	if n != 1 || !errors.Is(err, clusterredis.ErrNonIdempotentInPipeline) {
		t.Fatalf("PopAdmit = (%d, %v), want 1 admitted (the single probe) and ErrNonIdempotentInPipeline", n, err)
	}
	if left, err := q.QueueLen(ctx, 1); err != nil || left != 3 {
		t.Fatalf("QueueLen = (%d, %v), want (3, nil): rejected entries stay queued", left, err)
	}
	if cnt, err := q.AdmittedCount(ctx, 1); err != nil || cnt != 1 {
		t.Fatalf("AdmittedCount = (%d, %v), want (1, nil): nothing was half-written", cnt, err)
	}
}

// serverReply 实现 redis.Error:模拟服务端的错误回复。
type serverReply string

func (e serverReply) Error() string { return string(e) }
func (serverReply) RedisError()     {}

// redirectPipelineHook 在开着的时候让流水线里的每条命令都收到同一条错误回复、不发往 Redis:模拟迁槽后节点客户端
// 拿着旧路由收到的重定向。单发的命令不受影响。
type redirectPipelineHook struct {
	on    *atomic.Bool
	reply serverReply
	hits  *atomic.Int32
}

func (redirectPipelineHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (redirectPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h redirectPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !h.on.Load() {
			return next(ctx, cmds)
		}
		h.hits.Add(1)
		for _, cmd := range cmds {
			cmd.SetErr(h.reply)
		}
		return h.reply
	}
}

// 迁槽期间放行流水线整块收到重定向(都是「没执行」):这一块之后停批,条目原样留在队列、没有半截写入;
// 探路那一条走带守卫的集群句柄,照常放行。路由恢复后的下一拍把剩下的放完,不重复放行。
func TestPopAdmit_RedirectedPipelineStopsTheBatchAndLeavesEntriesQueued(t *testing.T) {
	for _, reply := range []serverReply{
		"MOVED 3999 127.0.0.1:6381",
		"ASK 3999 127.0.0.1:6381",
		"TRYAGAIN Multiple keys request during rehashing of slot",
	} {
		t.Run(strings.Fields(string(reply))[0], func(t *testing.T) {
			q, _, _ := newGuardedQueue(t)
			ctx := context.Background()
			var on atomic.Bool
			var hits atomic.Int32
			on.Store(true)
			q.admitNodes.AddHook(redirectPipelineHook{on: &on, reply: reply, hits: &hits})
			entries := enqueueForZone(t, q, 1, 4)

			n, err := q.PopAdmit(ctx, 1, 4, noBudgetLimit, pickZone1Gate)
			if n != 1 || err == nil {
				t.Fatalf("PopAdmit = (%d, %v), want 1 admitted (the single probe) and the redirect error", n, err)
			}
			if kind, _ := clusterredis.ClassifyNotExecuted(err); kind == clusterredis.NotExecutedNo {
				t.Fatalf("PopAdmit error %v must still be recognisable as a not-executed reply", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("pipelines sent = %d, want 1: the batch stops after the redirected chunk", hits.Load())
			}
			if left, err := q.QueueLen(ctx, 1); err != nil || left != 3 {
				t.Fatalf("QueueLen = (%d, %v), want (3, nil)", left, err)
			}
			if cnt, err := q.AdmittedCount(ctx, 1); err != nil || cnt != 1 {
				t.Fatalf("AdmittedCount = (%d, %v), want (1, nil)", cnt, err)
			}

			on.Store(false)
			if n, err := q.PopAdmit(ctx, 1, 4, noBudgetLimit, pickZone1Gate); err != nil || n != 3 {
				t.Fatalf("PopAdmit after the route recovers = (%d, %v), want (3, nil)", n, err)
			}
			for _, e := range entries {
				if st, err := q.Poll(ctx, e.QueueToken, 1, stubSignFn); err != nil || st.Status != StatusAdmitted {
					t.Fatalf("poll = (%+v, %v), want ADMITTED exactly once per entry", st, err)
				}
			}
			if cnt, err := q.AdmittedCount(ctx, 1); err != nil || cnt != 4 {
				t.Fatalf("AdmittedCount = (%d, %v), want (4, nil): no entry admitted twice", cnt, err)
			}
		})
	}
}

// 放行锁的三条命令(SET NX、续期与释放两段单键脚本)在带守卫的控制集群句柄上能执行。
func TestDispatcherLock_PassesTheControlClusterGuard(t *testing.T) {
	_, guarded, _ := newGuardedQueue(t)
	ctx := context.Background()
	lock, err := locker.NewRedisLocker(guarded).TryLock(ctx, ZoneDispatcherLockKey("dispatcher:lock:login_queue", 1), 30*time.Second)
	if err != nil || !lock.IsLocked() {
		t.Fatalf("TryLock = (%+v, %v), want a held lock", lock, err)
	}
	if ok, err := lock.Renew(ctx, 30*time.Second); err != nil || !ok {
		t.Fatalf("Renew = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := lock.Release(ctx); err != nil || !ok {
		t.Fatalf("Release = (%v, %v), want (true, nil)", ok, err)
	}
}

// gate 标记的读写在带守卫的控制集群句柄上能执行:一个 zone 的 MGET 同 tag,停写标记的 pipeline 里只有 SET。
// 标记按 zone 隔开:别的 zone 用同一批 node_id 读不到。
func TestGateMarks_PassTheControlClusterGuardAndStayInTheirZone(t *testing.T) {
	_, guarded, mr := newGuardedQueue(t)
	ctx := context.Background()
	const zone = 3
	now := time.Now().Unix()

	if err := MarkGateDraining(ctx, guarded, zone, 2, now, 600); err != nil {
		t.Fatalf("MarkGateDraining: %v", err)
	}
	cs := []GateCandidate{{NodeID: 1, ZoneID: zone}, {NodeID: 2, ZoneID: zone}, {NodeID: 3, ZoneID: zone}}
	if got := DrainingGates(ctx, guarded, zone, cs); len(got) != 1 || !got[2] {
		t.Fatalf("DrainingGates = %v, want only gate 2", got)
	}
	if got := NewDrainMarkCache(guarded, zone, 0, nil).Draining(ctx, 0, cs); len(got) != 1 || !got[2] {
		t.Fatalf("DrainMarkCache.Draining with the unfiltered partition key = %v, want only gate 2 (marks are read by the cache's own zone)", got)
	}
	if got, err := DrainingGateSet(ctx, guarded, zone, []uint32{1, 2, 3}); err != nil || len(got) != 1 || !got[2] {
		t.Fatalf("DrainingGateSet = (%v, %v), want only gate 2", got, err)
	}

	gates := []GateOnline{drainingGate(1, 5), drainingGate(2, 0), drainingGate(3, 9)}
	if drained := EvaluateDrainingGates(ctx, guarded, zone, gates, GateDrainPolicy{}, now); len(drained) != 1 || drained[0].NodeID != 2 {
		t.Fatalf("EvaluateDrainingGates = %+v, want only gate 2 drained", drained)
	}
	if !mr.Exists(gatepick.DrainedKey(zone, 2)) {
		t.Fatalf("missing drained mark %q", gatepick.DrainedKey(zone, 2))
	}
	if err := MarkGateDraining(ctx, guarded, zone, 3, now, 600); err != nil {
		t.Fatalf("MarkGateDraining: %v", err)
	}
	if due := DrainMigrationsDue(ctx, guarded, zone, gates, migratePolicy(), now); len(due) != 1 || due[0].NodeID != 3 {
		t.Fatalf("DrainMigrationsDue = %+v, want only gate 3 (gate 2 is already empty, gate 1 is not draining)", due)
	}
	if err := PublishStaleMarks(ctx, guarded, zone, []uint32{1, 3}); err != nil {
		t.Fatalf("PublishStaleMarks: %v", err)
	}
	if !mr.Exists(gatepick.StaleKey(zone, 1)) || !mr.Exists(gatepick.StaleKey(zone, 3)) || mr.Exists(gatepick.StaleKey(zone, 2)) {
		t.Fatalf("stale marks: want gates 1 and 3 only, keys = %v", mr.Keys())
	}

	// 别的 zone 读不到这些标记。
	if got := DrainingGates(ctx, guarded, zone+1, cs); len(got) != 0 {
		t.Fatalf("zone %d must not see zone %d's draining marks, got %v", zone+1, zone, got)
	}
	if got, err := DrainingGateSet(ctx, guarded, zone+1, []uint32{1, 2, 3}); err != nil || len(got) != 0 {
		t.Fatalf("DrainingGateSet for another zone = (%v, %v), want empty", got, err)
	}
	for _, key := range mr.Keys() {
		if tag, tagged := redisslot.HashTag(key); !tagged || tag != "gm:3" {
			t.Fatalf("key %q was written outside the zone's gate-mark tag", key)
		}
	}

	if err := ClearGateDraining(ctx, guarded, zone, 2); err != nil {
		t.Fatalf("ClearGateDraining: %v", err)
	}
	if got := DrainingGates(ctx, guarded, zone, cs); len(got) != 1 || !got[3] {
		t.Fatalf("DrainingGates after clearing gate 2 = %v, want only gate 3", got)
	}
}
