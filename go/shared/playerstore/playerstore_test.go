package playerstore

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"shared/redisslot"
)

func newStandalone(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New(Config{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

func TestConfigValidate(t *testing.T) {
	bad := []Config{
		{},
		{Addrs: []string{""}},
		{Addrs: []string{"a:1", "b:2"}}, // 多地址必须 Cluster=true
		{Addrs: []string{"a:1"}, PoolSizePerNode: -1},
		{Addrs: []string{"a:1"}, PoolSizePerNode: MaxPoolSizePerNode + 1},
		{Addrs: []string{"a:1"}, ReadTimeoutMs: MaxTimeoutMs + 1},
		{Addrs: []string{"a:1"}, ConnMaxIdleSeconds: -1},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: Validate 应拒绝 %+v", i, c)
		}
	}
	good := []Config{
		{Addrs: []string{"a:1"}},
		{Addrs: []string{"a:1", "b:2", "c:3"}, Cluster: true, PoolSizePerNode: 2},
	}
	for i, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("good case %d: %v", i, err)
		}
	}
}

func TestCommandKeys(t *testing.T) {
	keys, ok, err := CommandKeys([]interface{}{"evalsha", "abc", 2, "a:{1}", "b:{1}", "arg"})
	if err != nil || !ok || len(keys) != 2 || keys[0] != "a:{1}" || keys[1] != "b:{1}" {
		t.Fatalf("evalsha keys = %v ok=%v err=%v", keys, ok, err)
	}
	keys, ok, _ = CommandKeys([]interface{}{"EVAL", "return 1", "0"})
	if !ok || len(keys) != 0 {
		t.Fatalf("eval 0 keys = %v ok=%v", keys, ok)
	}
	if _, _, err := CommandKeys([]interface{}{"eval", "x", 5, "a"}); err == nil {
		t.Fatal("numkeys 超过参数个数应报错")
	}
	keys, ok, _ = CommandKeys([]interface{}{"mset", "a:{1}", "v", "b:{1}", "v"})
	if !ok || len(keys) != 2 || keys[1] != "b:{1}" {
		t.Fatalf("mset keys = %v", keys)
	}
	if _, ok, _ := CommandKeys([]interface{}{"scan", "0", "match", "x*"}); ok {
		t.Fatal("scan 不应被识别出键")
	}
}

func TestCheckCommand(t *testing.T) {
	cases := []struct {
		args []interface{}
		want error
	}{
		{[]interface{}{"get", "PlayerAllData:{42}"}, nil},
		{[]interface{}{"get", "PlayerAllData:42"}, ErrUntaggedKey},
		{[]interface{}{"mget", "a:{42}", "b:{1}"}, ErrCrossSlot},
		{[]interface{}{"evalsha", "sha", 2, "player:{42}:owner_epoch", "player:save_pending:" + redisslot.PlayerIndexTag(42)}, nil},
		{[]interface{}{"eval", "s", 2, "player:{42}:owner_epoch", "player:{1}:owner_epoch"}, ErrCrossSlot},
		{[]interface{}{"ping"}, nil},
		{[]interface{}{"publish", "untagged-channel", "x"}, nil}, // 不认识的命令放行
	}
	for i, tc := range cases {
		err := CheckCommand(tc.args)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("case %d %v: err = %v, want %v", i, tc.args, err, tc.want)
		}
	}
}

func TestGuardOnStandaloneClient(t *testing.T) {
	ctx := context.Background()
	mr, c := newStandalone(t)
	if err := c.Set(ctx, "untagged", "v", 0).Err(); !errors.Is(err, ErrUntaggedKey) {
		t.Fatalf("Set untagged err = %v", err)
	}
	if mr.Exists("untagged") {
		t.Fatal("守卫拒绝的命令不应被执行")
	}
	if err := c.Eval(ctx, "return 1", []string{"a:{42}", "b:{1}"}).Err(); !errors.Is(err, ErrCrossSlot) {
		t.Fatalf("cross-slot eval err = %v", err)
	}
	if err := c.Set(ctx, "PlayerAllData:{42}", "v", 0).Err(); err != nil {
		t.Fatalf("tagged set: %v", err)
	}
	// 同 slot 的玩家键 + 按 slot 索引键可以进同一段脚本。
	idx := "player:save_pending:" + redisslot.PlayerIndexTag(42)
	if err := c.Eval(ctx, "redis.call('ZADD', KEYS[2], 0, ARGV[1]) return redis.call('GET', KEYS[1])",
		[]string{"PlayerAllData:{42}", idx}, "42").Err(); err != nil {
		t.Fatalf("same-slot eval: %v", err)
	}
}

func TestGuardRejectsWholePipeline(t *testing.T) {
	ctx := context.Background()
	mr, c := newStandalone(t)
	_, err := c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, "ok:{42}", "v", 0)
		p.Set(ctx, "bad", "v", 0)
		return nil
	})
	if !errors.Is(err, ErrUntaggedKey) {
		t.Fatalf("pipeline err = %v", err)
	}
	if mr.Exists("ok:{42}") {
		t.Fatal("管道里有违规命令时整批都不应发出")
	}
}

func TestMGetBySlotKeepsInputOrder(t *testing.T) {
	ctx := context.Background()
	_, c := newStandalone(t)
	for _, k := range []string{"s:{42}", "s:{1}", "s:{1000003}"} {
		if err := c.Set(ctx, k, "v-"+k, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{"s:{1}", "missing:{7}", "s:{42}", "s:{1000003}", "s:{1}"}
	vals, err := MGetBySlot(ctx, c, keys)
	if err != nil {
		t.Fatalf("MGetBySlot: %v", err)
	}
	want := []interface{}{"v-s:{1}", nil, "v-s:{42}", "v-s:{1000003}", "v-s:{1}"}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("vals[%d] = %v, want %v", i, vals[i], want[i])
		}
	}
	if v, err := MGetBySlot(ctx, c, nil); err != nil || v != nil {
		t.Fatalf("empty input: %v %v", v, err)
	}
}

// stubRunIDs 让 runIDReader 依次返回给定值(miniredis 不支持 INFO server),测试结束恢复。
func stubRunIDs(t *testing.T, ids ...string) {
	t.Helper()
	orig := runIDReader
	i := 0
	runIDReader = func(context.Context, *redis.Client) (string, error) {
		id := ids[len(ids)-1]
		if i < len(ids) {
			id = ids[i]
		}
		i++
		return id, nil
	}
	t.Cleanup(func() { runIDReader = orig })
}

func TestScanEachMasterStandalone(t *testing.T) {
	ctx := context.Background()
	_, c := newStandalone(t)
	stubRunIDs(t, "run-a")
	want := []string{"player:session:{1}", "player:session:{2}", "player:session:{3}"}
	for _, k := range want {
		if err := c.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Set(ctx, "other:{1}", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ScanEachMaster(ctx, c, "player:session:*", 2, func(keys []string) error {
		got = append(got, keys...)
		return nil
	}); err != nil {
		t.Fatalf("ScanEachMaster: %v", err)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	stop := errors.New("stop")
	if err := ScanEachMaster(ctx, c, "player:session:*", 1, func([]string) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("fn 的错误应原样返回,得到 %v", err)
	}
}

// 扫描期间节点重启(run_id 变化)必须报不完整:旧游标在新进程上续扫会漏键而不报错。
func TestScanEachMasterDetectsRestart(t *testing.T) {
	ctx := context.Background()
	_, c := newStandalone(t)
	if err := c.Set(ctx, "player:session:{1}", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	stubRunIDs(t, "run-before", "run-after")
	err := ScanEachMaster(ctx, c, "player:session:*", 10, func([]string) error { return nil })
	if !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("run_id 变化应返回 ErrTopologyChanged,得到 %v", err)
	}
}

func TestParseMyselfLine(t *testing.T) {
	nodes := "07c37dfeb235213a872192d90877d0cd55635b91 127.0.0.1:30004@31004 slave e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca 0 1426238317239 4 connected\n" +
		"67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1 127.0.0.1:30002@31002 myself,master - 0 1426238316232 2 connected 5461-10922 12000\n"
	self, err := parseMyselfLine(nodes)
	if err != nil {
		t.Fatalf("parseMyselfLine: %v", err)
	}
	if self.id != "67ed2db8d677e59ec4a4cefb06858cf2a1a89fa1" || self.slots != 10922-5461+1+1 || len(self.ranges) != 2 {
		t.Fatalf("解析结果不对: %+v", self)
	}
	migrating := "67ed 127.0.0.1:30002@31002 myself,master - 0 1 2 connected 0-100 [101->-e7d1eecce10fd6bb5eb35b9f99a514335d9ba9ca]\n"
	if _, err := parseMyselfLine(migrating); !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("迁移标记应报 ErrTopologyChanged,得到 %v", err)
	}
	if _, err := parseMyselfLine("abc 127.0.0.1:1@2 master - 0 1 2 connected 0-10\n"); err == nil {
		t.Fatal("没有 myself 行应报错")
	}
	if _, err := parseMyselfLine("abc 127.0.0.1:1@2 myself,master - 0 1 2 connected 0-16384\n"); err == nil {
		t.Fatal("越界 slot 段应报错")
	}
}

func TestWrapResultUnknown(t *testing.T) {
	if err := wrapResultUnknown(io.EOF); !errors.As(err, new(*ResultUnknownError)) || !errors.Is(err, io.EOF) {
		t.Fatalf("io.EOF 应包成 ResultUnknownError 且保留原因,得到 %v", err)
	}
	if err := wrapResultUnknown(io.EOF); err == io.EOF {
		t.Fatal("包装后不得再等于 io.EOF(否则 go-redis 集群层会重发)")
	}
	opErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
	if err := wrapResultUnknown(opErr); !errors.As(err, new(*ResultUnknownError)) {
		t.Fatalf("网络错误应包装,得到 %v", err)
	}
	if err := wrapResultUnknown(redis.Nil); err != redis.Nil {
		t.Fatalf("服务端应答的错误不应包装,得到 %v", err)
	}
	once := wrapResultUnknown(io.EOF)
	if wrapResultUnknown(once) != once {
		t.Fatal("已包装的错误不应重复包装")
	}
	if wrapResultUnknown(nil) != nil {
		t.Fatal("nil 应原样返回")
	}
}

func TestGuardRejectsNonIdempotentInPipeline(t *testing.T) {
	ctx := context.Background()
	mr, c := newStandalone(t)
	_, err := c.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Incr(ctx, "cnt:{42}")
		return nil
	})
	if !errors.Is(err, ErrNonIdempotentInPipeline) {
		t.Fatalf("管道里的 INCR 应被拒绝,得到 %v", err)
	}
	if mr.Exists("cnt:{42}") {
		t.Fatal("被拒绝的管道不应执行")
	}
	if err := c.Incr(ctx, "cnt:{42}").Err(); err != nil {
		t.Fatalf("单条 INCR 应允许: %v", err)
	}
}
