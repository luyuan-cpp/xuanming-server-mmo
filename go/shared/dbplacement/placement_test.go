package dbplacement

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func gaugeValue(t *testing.T, logicalDB string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := versionGauge.WithLabelValues(logicalDB).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetGauge().GetValue()
}

// hasSeries 走真实的采集,不用 WithLabelValues:那会顺手建出序列。
func hasSeries(t *testing.T, logicalDB string) bool {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	versionGauge.Collect(ch)
	close(ch)
	for m := range ch {
		metric := &dto.Metric{}
		if err := m.Write(metric); err != nil {
			t.Fatal(err)
		}
		for _, label := range metric.GetLabel() {
			if label.GetName() == "logical_db" && label.GetValue() == logicalDB {
				return true
			}
		}
	}
	return false
}

func TestPlacementCodec(t *testing.T) {
	in := Placement{Cluster: "b", Version: 7, State: StateMoving, Move: &Move{RunID: "abc", TargetCluster: "c", VerifiedMs: 5}}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cluster != "b" || out.Version != 7 || out.State != StateMoving || out.Move == nil || out.Move.VerifiedMs != 5 {
		t.Fatalf("round trip: %+v", out)
	}
	// 键名是和工具、手册共用的契约。
	if !strings.Contains(string(raw), `"cluster":"b"`) || !strings.Contains(string(raw), `"version":7`) || !strings.Contains(string(raw), `"state":"MOVING"`) {
		t.Fatalf("值的形状变了: %s", raw)
	}
	for _, bad := range []string{`{`, `{"cluster":"","version":1,"state":"ACTIVE"}`, `{"cluster":"a b","version":1,"state":"ACTIVE"}`, `{"cluster":"a","version":1,"state":"ELSE"}`, `{"cluster":"a","version":1}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("坏值被接受: %s", bad)
		}
	}
	if _, err := Decode([]byte(`{"cluster":"a","version":1,"state":"ACTIVE","future":1}`)); err != nil {
		t.Fatalf("多余字段不该拒绝: %v", err)
	}
	if _, err := Encode(Placement{Cluster: "a", State: "ELSE"}); err == nil {
		t.Fatal("坏状态不能写进 etcd")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	if Key(guildDB) != "/mmorpg/db_placement/mmorpg_guild" {
		t.Fatalf("键的形状变了: %s", Key(guildDB))
	}
	if name, ok := LogicalDBOfKey(Key(guildDB)); !ok || name != guildDB {
		t.Fatalf("LogicalDBOfKey: %s %v", name, ok)
	}
	for _, bad := range []string{"/other/x", KeyPrefix} {
		if _, ok := LogicalDBOfKey(bad); ok {
			t.Fatalf("不是映射键: %s", bad)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("没启用时恒为 nil: %v", err)
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){
		"默认集群不在清单": func(c *Config) { c.DefaultCluster = "z" },
		"没有 etcd":  func(c *Config) { c.EtcdHosts = nil },
		"地址没有端口":   func(c *Config) { c.SqlClusters["b"] = SqlCluster{Hosts: "10.0.0.2"} },
		"集群名非法":    func(c *Config) { c.SqlClusters["a b"] = SqlCluster{Hosts: "10.0.0.2:1"} },
	} {
		c := testConfig()
		change(&c)
		if c.Validate() == nil {
			t.Fatalf("%s 被接受", name)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	got, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil || got.Enabled() {
		t.Fatalf("没设环境变量 = 不按映射拨号: %+v %v", got, err)
	}
	env := func(name string) string {
		if name != EnvConfig {
			t.Fatalf("读了别的环境变量: %s", name)
		}
		return `{"DefaultCluster":"main","SqlClusters":{"main":{"Hosts":"h:1"}},"EtcdHosts":["e:2379"]}`
	}
	got, err = ConfigFromEnv(env)
	if err != nil || got.SqlClusters["main"].Hosts != "h:1" || got.EtcdHosts[0] != "e:2379" {
		t.Fatalf("解析: %+v %v", got, err)
	}
	if _, err := ConfigFromEnv(func(string) string { return "{" }); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
	if _, err := ConfigFromEnv(func(string) string { return `{"SqlClusters":{"main":{"Hosts":"h:1"}}}` }); err == nil {
		t.Fatal("形状不对必须报错")
	}
}

func TestResolveWith(t *testing.T) {
	etcd := newFakeEtcd()
	got, err := resolveWith(context.Background(), etcd, testConfig(), guildDB)
	if err != nil || got.Addr != "10.0.0.1:4000" || got.Placement.Version != 0 {
		t.Fatalf("没有键走默认集群: %+v %v", got, err)
	}
	if got.RefuseMoving(guildDB) != nil {
		t.Fatal("ACTIVE 不拒绝")
	}
	etcd.put(guildDB, placementValue(t, Placement{Cluster: "b", Version: 2, State: StateMoving}))
	got, err = resolveWith(context.Background(), etcd, testConfig(), guildDB)
	if err != nil || got.Addr != "10.0.0.2:4000" {
		t.Fatalf("按映射: %+v %v", got, err)
	}
	if err := got.RefuseMoving(guildDB); !errors.Is(err, ErrMoving) {
		t.Fatalf("-migrate 看到 MOVING 必须拒绝: %v", err)
	}
	if len(got.OpenOptions()) != 1 {
		t.Fatal("启用映射时要给连接选项")
	}
	addr, version, err := fixedResolver{addr: "x:1", version: 2}.ResolvePlacement(guildDB)
	if err != nil || addr != "x:1" || version != 2 {
		t.Fatalf("一次性解析器版本恒定: %s %d %v", addr, version, err)
	}
	etcd.failGets(errors.New("etcd is down"))
	if _, err := resolveWith(context.Background(), etcd, testConfig(), guildDB); err == nil {
		t.Fatal("读不到映射不能回退到默认集群")
	}
}

// defaultKV 是默认集群键的内存实现:CreateRevision=0 的事务第一次写入,之后回读。
type defaultKV struct {
	clientv3.KV
	stored string
	found  bool
	err    error
}

func (k *defaultKV) Txn(context.Context) clientv3.Txn { return &defaultTxn{kv: k} }

type defaultTxn struct {
	kv   *defaultKV
	then string
}

func (t *defaultTxn) If(...clientv3.Cmp) clientv3.Txn { return t }
func (t *defaultTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.then = string(ops[0].ValueBytes())
	return t
}
func (t *defaultTxn) Else(...clientv3.Op) clientv3.Txn { return t }
func (t *defaultTxn) Commit() (*clientv3.TxnResponse, error) {
	if t.kv.err != nil {
		return nil, t.kv.err
	}
	if !t.kv.found {
		t.kv.stored, t.kv.found = t.then, true
		return &clientv3.TxnResponse{Succeeded: true}, nil
	}
	return &clientv3.TxnResponse{Responses: []*etcdserverpb.ResponseOp{{Response: &etcdserverpb.ResponseOp_ResponseRange{
		ResponseRange: &etcdserverpb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Key: []byte(DefaultClusterKey), Value: []byte(t.kv.stored)}}},
	}}}}, nil
}

func TestEnsureDefaultCluster(t *testing.T) {
	ctx := context.Background()
	kv := &defaultKV{}
	if err := EnsureDefaultCluster(ctx, kv, testConfig()); err != nil || kv.stored != "main" {
		t.Fatalf("第一次部署写入: %q %v", kv.stored, err)
	}
	if err := EnsureDefaultCluster(ctx, kv, testConfig()); err != nil {
		t.Fatalf("一致时放行: %v", err)
	}
	changed := testConfig()
	changed.DefaultCluster = "b"
	if err := EnsureDefaultCluster(ctx, kv, changed); !errors.Is(err, ErrDefaultClusterChanged) {
		t.Fatalf("配置不一致要拒启: %v", err)
	}
	if kv.stored != "main" {
		t.Fatalf("不一致时不能覆盖: %q", kv.stored)
	}
	if err := EnsureDefaultCluster(ctx, kv, Config{}); err != nil {
		t.Fatalf("没启用映射时什么都不做: %v", err)
	}
	kv.err = errors.New("etcd is down")
	if err := EnsureDefaultCluster(ctx, kv, testConfig()); err == nil || errors.Is(err, ErrDefaultClusterChanged) {
		t.Fatalf("读不到不能算一致,也不能说成不一致: %v", err)
	}
	if strings.HasPrefix(DefaultClusterKey, KeyPrefix) {
		t.Fatal("默认集群键不能落在映射前缀下面")
	}
}

func TestRequireForMigration(t *testing.T) {
	required := func(name string) string {
		if name == EnvRequired {
			return "1"
		}
		return ""
	}
	if err := RequireForMigration(Config{}, required); !errors.Is(err, ErrPlacementRequired) {
		t.Fatalf("要求了又没配要拒绝: %v", err)
	}
	if err := RequireForMigration(testConfig(), required); err != nil {
		t.Fatalf("配了放行: %v", err)
	}
	if err := RequireForMigration(Config{}, func(string) string { return "" }); err != nil {
		t.Fatalf("dev 不要求: %v", err)
	}
	t.Setenv(EnvRequired, "1")
	if _, err := ForMigration(context.Background(), Config{}, guildDB); !errors.Is(err, ErrPlacementRequired) {
		t.Fatalf("-migrate 入口要走这条: %v", err)
	}
}

func TestDisabledConfigResolvesToNothing(t *testing.T) {
	options, err := ForMigration(context.Background(), Config{}, guildDB)
	if err != nil || options != nil {
		t.Fatalf("没启用映射时 -migrate 和原来一样: %v %v", options, err)
	}
	options, err = ForOneShot(context.Background(), Config{}, guildDB)
	if err != nil || options != nil {
		t.Fatalf("没启用映射时工具和原来一样: %v %v", options, err)
	}
}

// 真 etcd 用例:DBPLACEMENT_IT_ETCD=127.0.0.1:2379 时才跑。CAS 的语义靠它钉住。
func TestStoreAgainstRealEtcd(t *testing.T) {
	hosts := os.Getenv("DBPLACEMENT_IT_ETCD")
	if hosts == "" {
		t.Skip("DBPLACEMENT_IT_ETCD is not set")
	}
	client, err := Dial(Config{EtcdHosts: strings.Split(hosts, ",")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := "it_" + time.Now().Format("20060102150405.000000")
	defer client.Delete(ctx, Key(name))
	store := NewStore(client)
	rec, err := store.Get(ctx, name)
	if err != nil || rec.ModRevision != 0 {
		t.Fatalf("键不存在: %+v %v", rec, err)
	}
	moving := Placement{Cluster: "main", State: StateMoving}
	if err := store.CompareAndPut(ctx, name, 0, moving); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndPut(ctx, name, 0, moving); !errors.Is(err, ErrConflict) {
		t.Fatalf("键已存在时 CAS 必须冲突: %v", err)
	}
	rec, err = store.Get(ctx, name)
	if err != nil || rec.ModRevision == 0 || rec.Placement.State != StateMoving {
		t.Fatalf("读回: %+v %v", rec, err)
	}
	flipped := Placement{Cluster: "b", Version: 1, State: StateActive}
	if err := store.CompareAndPut(ctx, name, rec.ModRevision, flipped); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareAndPut(ctx, name, rec.ModRevision, moving); !errors.Is(err, ErrConflict) {
		t.Fatalf("旧版本 CAS 必须冲突: %v", err)
	}
	all, err := store.List(ctx)
	if err != nil || all[name].Placement.Cluster != "b" {
		t.Fatalf("List: %+v %v", all[name], err)
	}
	// 默认集群键:真 etcd 上的事务形状。这个键是全局的,用例跑完不能留,否则本机下一次部署会被它拒启。
	if existing, found, err := store.DefaultCluster(ctx); err != nil {
		t.Fatal(err)
	} else if found {
		t.Skipf("etcd 里已有默认集群 %q,不碰它", existing)
	}
	defer client.Delete(ctx, DefaultClusterKey)
	cfg := Config{DefaultCluster: "it_main", SqlClusters: map[string]SqlCluster{"it_main": {Hosts: "h:1"}, "it_b": {Hosts: "h:2"}}, EtcdHosts: []string{hosts}}
	if err := EnsureDefaultCluster(ctx, client, cfg); err != nil {
		t.Fatal(err)
	}
	if cluster, found, err := store.DefaultCluster(ctx); err != nil || !found || cluster != "it_main" {
		t.Fatalf("读回默认集群: %q %v %v", cluster, found, err)
	}
	if err := EnsureDefaultCluster(ctx, client, cfg); err != nil {
		t.Fatalf("一致: %v", err)
	}
	cfg.DefaultCluster = "it_b"
	if err := EnsureDefaultCluster(ctx, client, cfg); !errors.Is(err, ErrDefaultClusterChanged) {
		t.Fatalf("不一致要拒绝: %v", err)
	}
}
