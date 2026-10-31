package playerredis

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
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
		{Addrs: []string{"a:1"}, DB: -1},
		{Addrs: []string{"a:1"}, Cluster: true, DB: 3}, // Redis Cluster 只有 DB 0
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: Validate 应拒绝 %+v", i, c)
		}
	}
	good := []Config{
		{Addrs: []string{"a:1"}},
		{Addrs: []string{"a:1"}, DB: 3},
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
	var unknown *ResultUnknownError
	if err := wrapResultUnknown(io.EOF); !errors.As(err, &unknown) || unknown.Cause() != io.EOF {
		t.Fatalf("io.EOF 应包成 ResultUnknownError 且保留原因,得到 %v", err)
	}
	// go-redis 9.17 的 shouldRetry 用 errors.Is 判 EOF,解得开就会重发已执行的命令。
	if err := wrapResultUnknown(io.EOF); errors.Is(err, io.EOF) {
		t.Fatal("包装后 errors.Is 不得再认出 io.EOF(否则 go-redis 集群层会重发)")
	}
	opErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
	if err := wrapResultUnknown(opErr); !errors.As(err, new(*ResultUnknownError)) {
		t.Fatalf("网络错误应包装,得到 %v", err)
	}
	if err := wrapResultUnknown(redis.Nil); err != redis.Nil {
		t.Fatalf("服务端应答的错误不应包装,得到 %v", err)
	}
	// DeadlineExceeded 实现了 net.Error,不能被当成网络错误包住。
	for _, ctxErr := range []error{context.DeadlineExceeded, context.Canceled} {
		if err := wrapResultUnknown(ctxErr); err != ctxErr {
			t.Fatalf("上下文错误不应包装,得到 %v", err)
		}
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

func TestConfigValidateSection(t *testing.T) {
	if err := (Config{}).ValidateSection(); err != nil {
		t.Fatalf("整段不写是合法的: %v", err)
	}
	if err := (Config{PoolSizePerNode: 8}).ValidateSection(); err != nil {
		t.Fatalf("只写连接池是合法的: %v", err)
	}
	if err := (Config{Addrs: []string{"a:1"}}).ValidateSection(); err != nil {
		t.Fatalf("单实例: %v", err)
	}
	bad := []Config{
		{Cluster: true}, // 集群没写种子,不能静默落到现有实例
		{PoolSizePerNode: MaxPoolSizePerNode + 1},
		{ReadTimeoutMs: -1},
		{Addrs: []string{"a:1", "b:2"}},
	}
	for i, c := range bad {
		if err := c.ValidateSection(); err == nil {
			t.Errorf("case %d: 应拒绝 %+v", i, c)
		}
	}
}

func TestConfigOrDefault(t *testing.T) {
	fallback := Config{Addrs: []string{"control:6379"}, PoolSizePerNode: 20}
	if got := (Config{}).OrDefault(fallback); got.Addrs[0] != "control:6379" || got.PoolSizePerNode != 20 {
		t.Fatalf("没写 Addrs 应取 fallback,得到 %+v", got)
	}
	// 回落到现有实例要落在它原来的库号上。
	if got := (Config{}).OrDefault(Config{Addrs: []string{"control:6379"}, DB: 3}); got.DB != 3 {
		t.Fatalf("没写 Addrs 应取 fallback 的 DB,得到 %+v", got)
	}
	own := Config{Addrs: []string{"player:6379"}, Cluster: true}
	if got := own.OrDefault(fallback); got.Addrs[0] != "player:6379" || !got.Cluster || got.PoolSizePerNode != 0 {
		t.Fatalf("写了 Addrs 应原样返回,得到 %+v", got)
	}
	// 只写了池与超时、没写地址:地址取 fallback,写了的那几项优先。
	tuned := Config{PoolSizePerNode: 7, ReadTimeoutMs: 500}
	if got := tuned.OrDefault(Config{Addrs: []string{"control:6379"}, PoolSizePerNode: 20, ReadTimeoutMs: 3000, WriteTimeoutMs: 3000}); got.Addrs[0] != "control:6379" ||
		got.PoolSizePerNode != 7 || got.ReadTimeoutMs != 500 || got.WriteTimeoutMs != 3000 {
		t.Fatalf("单独写的池与超时应优先,得到 %+v", got)
	}
}

// 读登录态的服务:地址、口令、库号都取这一段,调用方的超时和缺省连接池保留。
func TestLoginReaderConfig(t *testing.T) {
	base := Config{PoolSizePerNode: 8, ReadTimeoutMs: 300, Addrs: []string{"不该留下:1"}, Password: "不该留下"}

	cluster := LoginReaderConfig{Addrs: []string{"a:1", "b:2"}, Cluster: true, PasswordEnv: "ENV", PoolSizePerNode: 5}
	got := cluster.ClientConfig(base)
	if len(got.Addrs) != 2 || got.Addrs[0] != "a:1" || !got.Cluster || got.PasswordEnv != "ENV" || got.Password != "" || got.PoolSizePerNode != 5 || got.ReadTimeoutMs != 300 {
		t.Fatalf("这一段应覆盖调用方的地址和口令,调用方的超时保留,得到 %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("集群形状应合法: %v", err)
	}

	single := LoginReaderConfig{Addrs: []string{"host:3"}, Password: "pw", DB: 3}
	got = single.ClientConfig(base)
	if len(got.Addrs) != 1 || got.Addrs[0] != "host:3" || got.Cluster || got.Password != "pw" || got.DB != 3 || got.PoolSizePerNode != 8 {
		t.Fatalf("单实例形状应生效,没写连接池取调用方的缺省,得到 %+v", got)
	}

	if (LoginReaderConfig{}).HasAddr() {
		t.Fatal("没写地址时 HasAddr 应为 false")
	}
	if !(LoginReaderConfig{Addrs: []string{"a:1"}}).HasAddr() {
		t.Fatal("写了地址时 HasAddr 应为 true")
	}
	if err := (LoginReaderConfig{}).ClientConfig(Config{}).Validate(); err == nil {
		t.Fatal("没写地址不能静默落到别的实例,Validate 应拒绝")
	}
	if err := (LoginReaderConfig{Addrs: []string{"  "}}).ClientConfig(Config{}).Validate(); err == nil {
		t.Fatal("空白地址不能过校验")
	}
	if err := (LoginReaderConfig{Addrs: []string{"a:1"}, Cluster: true, DB: 3}).ClientConfig(Config{}).Validate(); err == nil {
		t.Fatal("Redis Cluster 只有 DB 0")
	}
}

// 口令可以来自环境变量,优先于 Password;Describe 不带口令。
func TestConfigPasswordEnvAndDescribe(t *testing.T) {
	const env = "PLAYERREDIS_TEST_PASSWORD"
	t.Setenv(env, "from-env")
	c := Config{Addrs: []string{"a:1"}, Password: "plain", PasswordEnv: env}
	if c.password() != "from-env" {
		t.Fatalf("PasswordEnv 应优先,得到 %q", c.password())
	}
	if (Config{Password: "plain"}).password() != "plain" {
		t.Fatal("没配 PasswordEnv 时取 Password")
	}
	if strings.Contains(c.Describe(), "plain") || strings.Contains(c.Describe(), "from-env") {
		t.Fatalf("Describe 不得带口令: %s", c.Describe())
	}
}

func TestScanCursorRoundTrip(t *testing.T) {
	if (ScanCursor{}).String() != "" {
		t.Fatal("零值编码成空串")
	}
	start, err := ParseScanCursor("")
	if err != nil || !start.IsStart() {
		t.Fatalf("空串是零值: %+v %v", start, err)
	}
	c := ScanCursor{digest: "0a1b2c3d", master: 12, node: 18446744073709551615}
	raw := c.String()
	if len(raw) > 35 {
		t.Fatalf("游标超过文档写的上限: %d", len(raw))
	}
	back, err := ParseScanCursor(raw)
	if err != nil || back != c {
		t.Fatalf("往返不一致: %+v %v", back, err)
	}
	for _, bad := range []string{
		"x", "a.b.c", "0a1b2c3d.0", "0a1b2c3d.0.0.0", "0A1B2C3D.0.0", "0a1b2c3.0.0", "0a1b2c3d.-1.0",
		"0a1b2c3d.65536.0", "0a1b2c3d.0.18446744073709551616", "0a1b2c3d.0.-1", "0a1b2c3d..0",
	} {
		if _, err := ParseScanCursor(bad); !errors.Is(err, ErrInvalidScanCursor) {
			t.Errorf("%q 应判非法,得到 %v", bad, err)
		}
	}
}

// 单实例分页:一批读完就结束,游标来自别的拓扑时从头开始。
func TestScanPageStandalone(t *testing.T) {
	ctx := context.Background()
	_, c := newStandalone(t)
	for _, k := range []string{"player:session:{1}", "player:session:{2}"} {
		if err := c.Set(ctx, k, "x", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Set(ctx, "other:{1}", "x", 0).Err(); err != nil {
		t.Fatal(err)
	}
	page, err := ScanPage(ctx, c, ScanCursor{}, "player:session:*", 10)
	if err != nil {
		t.Fatalf("ScanPage: %v", err)
	}
	if len(page.Keys) != 2 || !page.Next.IsStart() || page.Restarted {
		t.Fatalf("首页: %+v", page)
	}
	if page.Current.digest != standaloneScanDigest || page.Current.master != 0 || page.Current.node != 0 {
		t.Fatalf("Current: %+v", page.Current)
	}
	stale := ScanCursor{digest: "deadbeef", master: 3, node: 99}
	page, err = ScanPage(ctx, c, stale, "player:session:*", 10)
	if err != nil {
		t.Fatalf("ScanPage(stale): %v", err)
	}
	if !page.Restarted || page.Current.node != 0 || page.Current.master != 0 || len(page.Keys) != 2 {
		t.Fatalf("别的拓扑的游标应从头开始: %+v", page)
	}
	if _, err := ScanPage(ctx, stubUniversal{c}, ScanCursor{}, "*", 1); !errors.Is(err, ErrUnsupportedClient) {
		t.Fatalf("不认识的客户端应报错,得到 %v", err)
	}
}

// stubUniversal 包一层,让类型判断认不出来。
type stubUniversal struct{ redis.UniversalClient }

// Cluster 分页经 miniredis 的 CLUSTER NODES(一个主节点持有全部 slot)走一遍:指纹、取节点、SCAN。
func TestScanPageClusterClient(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	c, err := New(Config{Addrs: []string{mr.Addr()}, Cluster: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := mr.Set("player:session:{7}", "x"); err != nil {
		t.Fatal(err)
	}
	page, err := ScanPage(ctx, c, ScanCursor{}, "player:session:*", 10)
	if err != nil {
		t.Fatalf("ScanPage: %v", err)
	}
	if len(page.Keys) != 1 || page.Keys[0] != "player:session:{7}" || !page.Next.IsStart() {
		t.Fatalf("结果不对: %+v", page)
	}
	if page.Current.digest == standaloneScanDigest || !isScanDigest(page.Current.digest) {
		t.Fatalf("Cluster 的指纹摘要应来自 CLUSTER NODES: %q", page.Current.digest)
	}
}

func TestParseScanTopology(t *testing.T) {
	const nodes = "bbb 10.0.0.2:6379@16379 master - 0 1 2 connected 5461-10922\n" +
		"aaa 10.0.0.1:6379@16379 myself,master - 0 1 1 connected 0-5460 [5460->-bbb]\n" +
		"ccc 10.0.0.3:6379@16379 master - 0 1 3 connected 10923-16383\n" +
		"ddd 10.0.0.4:6379@16379 slave aaa 0 1 1 connected\n" +
		"eee 10.0.0.5:6379@16379 master - 0 1 4 connected\n"
	topology, err := parseScanTopology(nodes)
	if err != nil {
		t.Fatalf("parseScanTopology: %v", err)
	}
	if len(topology.masters) != 3 {
		t.Fatalf("副本与没有 slot 的主节点不算: %+v", topology.masters)
	}
	if topology.masters[0].id != "aaa" || topology.masters[0].firstSlot != 0 || topology.masters[1].firstSlot != 5461 {
		t.Fatalf("按节点 ID 排序并取首个 slot: %+v", topology.masters)
	}
	// 行序、节点视角(myself)、迁移标记都不影响指纹。
	same := "ccc 10.0.0.3:6379@16379 myself,master - 9 9 3 connected 10923-16383\n" +
		"aaa 10.0.0.1:6379@16379 master - 9 9 1 connected 0-5460\n" +
		"bbb 10.0.0.2:6379@16379 master - 9 9 2 connected 5461-10922\n"
	sameTopology, err := parseScanTopology(same)
	if err != nil || sameTopology.digest != topology.digest {
		t.Fatalf("同一拓扑指纹应相同: %q vs %q err=%v", sameTopology.digest, topology.digest, err)
	}
	// slot 归属变化、换主都要改变指纹。
	moved := "aaa 10.0.0.1:6379@16379 master - 0 1 1 connected 0-5459\n" +
		"bbb 10.0.0.2:6379@16379 master - 0 1 2 connected 5460-10922\n" +
		"ccc 10.0.0.3:6379@16379 master - 0 1 3 connected 10923-16383\n"
	movedTopology, err := parseScanTopology(moved)
	if err != nil || movedTopology.digest == topology.digest {
		t.Fatalf("slot 归属变化应改变指纹: %v", err)
	}
	failedOver := "fff 10.0.0.6:6379@16379 master - 0 1 5 connected 0-5460\n" +
		"bbb 10.0.0.2:6379@16379 master - 0 1 2 connected 5461-10922\n" +
		"ccc 10.0.0.3:6379@16379 master - 0 1 3 connected 10923-16383\n"
	failedOverTopology, err := parseScanTopology(failedOver)
	if err != nil || failedOverTopology.digest == topology.digest {
		t.Fatalf("换主应改变指纹: %v", err)
	}
	if _, err := parseScanTopology("ddd 10.0.0.4:6379@16379 slave aaa 0 1 1 connected\n"); !errors.Is(err, ErrTopologyChanged) {
		t.Fatalf("没有持有 slot 的主节点应报错,得到 %v", err)
	}
	if _, err := parseScanTopology("aaa 10.0.0.1:6379@16379 master - 0 1 1 connected 0-16384\n"); err == nil {
		t.Fatal("越界 slot 段应报错")
	}
}

// replyError 实现 redis.Error:模拟服务端的错误回复。
type replyError string

func (e replyError) Error() string { return string(e) }
func (replyError) RedisError()     {}

func TestClassifyNotExecuted(t *testing.T) {
	cases := []struct {
		err  error
		want NotExecuted
		addr string
	}{
		{replyError("MOVED 3999 127.0.0.1:6381"), NotExecutedMoved, "127.0.0.1:6381"},
		{replyError("ASK 3999 127.0.0.1:6381"), NotExecutedAsk, "127.0.0.1:6381"},
		{replyError("TRYAGAIN Multiple keys request during rehashing of slot"), NotExecutedRetryLater, ""},
		{replyError("CLUSTERDOWN The cluster is down"), NotExecutedRetryLater, ""},
		{replyError("LOADING Redis is loading the dataset in memory"), NotExecutedRetryLater, ""},
		{replyError("READONLY You can't write against a read only replica."), NotExecutedRetryLater, ""},
		{replyError("ERR script error"), NotExecutedNo, ""},
		{replyError(""), NotExecutedNo, ""},
		{io.EOF, NotExecutedNo, ""},
		{redis.Nil, NotExecutedNo, ""},
		{nil, NotExecutedNo, ""},
	}
	for _, tc := range cases {
		got, addr := ClassifyNotExecuted(tc.err)
		if got != tc.want || addr != tc.addr {
			t.Errorf("%v: got (%d, %q), want (%d, %q)", tc.err, got, addr, tc.want, tc.addr)
		}
	}
}

// 铸造通道的客户端不挂守卫的管道规则:EVAL 与 WAITAOF 要进同一条 pipeline。
func TestPinnedClientAllowsEvalInPipeline(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	c, err := NewPinned(Config{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	node, err := MasterForKey(ctx, c, "cnt:{42}")
	if err != nil {
		t.Fatalf("MasterForKey: %v", err)
	}
	var eval *redis.Cmd
	if _, err := node.Pipelined(ctx, func(p redis.Pipeliner) error {
		eval = p.Eval(ctx, "return redis.call('INCR', KEYS[1])", []string{"cnt:{42}"})
		return nil
	}); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if eval.Val() != int64(1) {
		t.Fatalf("eval = %v", eval.Val())
	}
	calls := 0
	if err := ForEachMaster(ctx, c, func(_ context.Context, got *redis.Client) error {
		calls++
		if got != node {
			t.Error("单实例的主节点就是它自己")
		}
		return nil
	}); err != nil || calls != 1 {
		t.Fatalf("ForEachMaster: calls=%d err=%v", calls, err)
	}
	// 单实例上什么都不做,不 panic。
	ReloadTopology(ctx, c)
}
