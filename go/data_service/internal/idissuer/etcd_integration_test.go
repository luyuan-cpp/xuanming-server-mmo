//go:build etcd_integration

package idissuer

// 真 etcd 上的发号器回归,由 Codex 执行,步骤见 docs/handoff/global-id-issuer-registry-20261105.md。
//
// 需要三套互相独立的 etcd(三个单节点进程即可,集群令牌不同,集群号就不同),地址逗号分隔:
//
//	MMORPG_ID_ISSUER_IT_PRIMARY / MMORPG_ID_ISSUER_IT_SECONDARY / MMORPG_ID_ISSUER_IT_REGISTRY
//
// 重复部署的用例还要第二对本地 etcd:MMORPG_ID_ISSUER_IT_PRIMARY2 / MMORPG_ID_ISSUER_IT_SECONDARY2。没给就跳过那一条。
// 每个用例的键都在 /id_issuer_it/<随机串>/ 下,不碰正式的 /id_issuer/、/id_registry/ 键,可以重复跑。
//
// TestEtcdDrill 是真快照回档演练,分三段跑(MMORPG_ID_ISSUER_IT_PHASE=drill-a / drill-b / drill-c),
// 段与段之间由执行者用 etcdctl / etcdutl 拍快照、恢复,状态存在 MMORPG_ID_ISSUER_IT_STATE 指向的文件里。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const itRegion = uint8(9)

func itEndpoints(t *testing.T, name string) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		t.Skipf("%s is not set", name)
	}
	var out []string
	for _, endpoint := range strings.Split(raw, ",") {
		if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
			out = append(out, endpoint)
		}
	}
	return out
}

// itEnv 是一个部署环境:两份本地记录 + 登记处,键都在 root 下。
type itEnv struct {
	t         *testing.T
	root      string
	primary   []string
	secondary []string
	registry  []string
	now       time.Time
}

func newItEnv(t *testing.T, root string, primary, secondary, registry []string) *itEnv {
	return &itEnv{t: t, root: root, primary: primary, secondary: secondary, registry: registry, now: testNow}
}

func itRoot(t *testing.T) string {
	t.Helper()
	suffix, err := randomAuthority()
	if err != nil {
		t.Fatal(err)
	}
	return "/id_issuer_it/" + suffix[:12] + "/"
}

// issuer 建一个生产形态的发号器(相当于一个新进程)。
func (e *itEnv) issuer(observe func(string)) *Issuer {
	e.t.Helper()
	p, err := NewEtcdStore(e.primary, e.root+"counter/primary")
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := NewEtcdStore(e.secondary, e.root+"counter/secondary")
	if err != nil {
		e.t.Fatal(err)
	}
	g, err := newEtcdRegistryAt(e.registry, e.root+"registry/")
	if err != nil {
		e.t.Fatal(err)
	}
	iss, err := New(Options{Primary: p, Secondary: s, Registry: g, Region: itRegion, BlockSize: testBlock,
		Tags: []string{"player"}, Now: func() time.Time { return e.now }, OnReserve: observe})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = iss.Close() })
	return iss
}

func (e *itEnv) client(endpoints []string) *clientv3.Client {
	e.t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func (e *itEnv) raw(endpoints []string, key string) []byte {
	e.t.Helper()
	resp, err := e.client(endpoints).Get(context.Background(), key)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(resp.Kvs) == 0 {
		return nil
	}
	return resp.Kvs[0].Value
}

func (e *itEnv) put(endpoints []string, key string, value []byte) {
	e.t.Helper()
	if _, err := e.client(endpoints).Put(context.Background(), key, string(value)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *itEnv) registryHighWater() uint64 {
	e.t.Helper()
	g, err := newEtcdRegistryAt(e.registry, e.root+"registry/")
	if err != nil {
		e.t.Fatal(err)
	}
	defer g.Close()
	rec, err := g.Read(context.Background(), itRegion)
	if err != nil || !rec.Exists {
		e.t.Fatalf("registry entry: %+v %v", rec, err)
	}
	return rec.HighWater
}

func (e *itEnv) prepare() {
	e.t.Helper()
	iss := e.issuer(nil)
	if _, err := iss.OpenRegistry(context.Background(), uint32(itRegion)); err != nil {
		e.t.Fatal(err)
	}
	if created, err := iss.Init(context.Background()); err != nil || !created {
		e.t.Fatalf("init: created=%v err=%v", created, err)
	}
}

// 三套 etcd 报的集群号都不为 0 且两两不同:环境本身要满足「互相独立」,否则下面的用例证明不了什么。
func TestEtcdIntegration_ClustersAreIndependent(t *testing.T) {
	root := itRoot(t)
	env := newItEnv(t, root, itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY"))
	ids := map[uint64]string{}
	for name, endpoints := range map[string][]string{"primary": env.primary, "secondary": env.secondary, "registry": env.registry} {
		resp, err := env.client(endpoints).Get(context.Background(), root+"probe")
		if err != nil {
			t.Fatal(err)
		}
		id := resp.Header.ClusterId
		if id == 0 {
			t.Fatalf("%s reports cluster id 0", name)
		}
		if other, dup := ids[id]; dup {
			t.Fatalf("%s and %s report the same cluster id %x: start them with different --initial-cluster-token", name, other, id)
		}
		ids[id] = name
	}
}

// 存储契约:记录和登记处的格式、只写一次的认领、坏值不能被当成不存在。
func TestEtcdIntegration_StoreContract(t *testing.T) {
	root := itRoot(t)
	env := newItEnv(t, root, itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY"))
	ctx := context.Background()

	store, err := NewEtcdStore(env.primary, root+"contract/record")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	empty, err := store.Read(ctx)
	if err != nil || empty.Exists || empty.ClusterID == 0 {
		t.Fatalf("empty record: %+v %v", empty, err)
	}
	if ok, err := store.CompareAndSwap(ctx, empty, 1<<62, testAuthority); err != nil || !ok {
		t.Fatalf("create: %v %v", ok, err)
	}
	if ok, err := store.CompareAndSwap(ctx, empty, 7, testAuthority); err != nil || ok {
		t.Fatalf("a second create from the same stale view must lose: %v %v", ok, err)
	}
	rec, err := store.Read(ctx)
	if err != nil || rec.Value != 1<<62 || rec.Authority != testAuthority {
		t.Fatalf("read back: %+v %v", rec, err)
	}
	env.put(env.primary, root+"contract/record", []byte("12345"))
	if _, err := store.Read(ctx); err == nil {
		t.Fatal("a plain number is not a valid record and must not be read as absent")
	}

	registry, err := newEtcdRegistryAt(env.registry, root+"contract/registry/")
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	claim := Claim{State: ClaimActive, Authority: testAuthority, PrimaryCluster: 1, SecondaryCluster: 2, HighWater: 1}
	if _, err := registry.ClaimNew(ctx, 3, claim); !errors.Is(err, ErrRegistryNotOpened) {
		t.Fatalf("claim before open: %v", err)
	}
	if opened, err := registry.Open(ctx, 3); err != nil || !opened {
		t.Fatalf("open: %v %v", opened, err)
	}
	if opened, err := registry.Open(ctx, 1); err != nil || opened {
		t.Fatalf("second open must do nothing: %v %v", opened, err)
	}
	if _, err := registry.ClaimNew(ctx, 2, claim); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("claim below next: %v", err)
	}
	if ok, err := registry.ClaimNew(ctx, 3, claim); err != nil || !ok {
		t.Fatalf("claim 3: %v %v", ok, err)
	}
	if _, err := registry.ClaimNew(ctx, 3, claim); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("claim 3 twice: %v", err)
	}
	got, err := registry.Read(ctx, 3)
	if err != nil || !got.Exists || got.Claim != claim || got.ClusterID == 0 {
		t.Fatalf("read claim: %+v %v", got, err)
	}
	env.put(env.registry, root+"contract/registry/region/03", []byte(`{"state":"active"}`))
	if _, err := registry.Read(ctx, 3); err == nil {
		t.Fatal("a claim without authority must be reported as corrupt")
	}
}

// 问题 1 的值级回档:两份本地记录被改回旧快照里的原始字节(两条记录都还在),时钟正常,新进程不得重叠历史预留。
func TestEtcdIntegration_BothLocalRolledBack(t *testing.T) {
	root := itRoot(t)
	env := newItEnv(t, root, itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY"))
	env.prepare()
	a := &allocated{t: t}
	a.take(env.issuer(nil), 100)
	snapP, snapS := env.raw(env.primary, root+"counter/primary"), env.raw(env.secondary, root+"counter/secondary")
	for restart := 0; restart < 5; restart++ {
		a.take(env.issuer(nil), 100)
	}
	historical := env.registryHighWater()
	env.put(env.primary, root+"counter/primary", snapP)
	env.put(env.secondary, root+"counter/secondary", snapS)

	obs := newObserver()
	lo, _ := a.take(env.issuer(obs.record), 100)
	if lo < historical {
		t.Fatalf("lo=%d overlaps reservations that end at %d", lo, historical)
	}
	if obs.count(ReserveLocalBehind) != 1 {
		t.Fatalf("observed %v", obs.seen)
	}
}

// 问题 2:第二对独立 etcd 配同一个地域码,拿不到发号资格,也不改登记处。
func TestEtcdIntegration_SecondPairWithTheSameCodeIsRefused(t *testing.T) {
	root := itRoot(t)
	registry := itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY")
	first := newItEnv(t, root, itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY"), registry)
	second := newItEnv(t, root, itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY2"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY2"), registry)
	first.prepare()
	a := &allocated{t: t}
	a.take(first.issuer(nil), 100)
	before := first.raw(registry, root+"registry/region/09")

	dup := second.issuer(nil)
	if _, err := dup.Init(context.Background()); !errors.Is(err, ErrRegionClaimed) {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := dup.Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := dup.Recover(context.Background(), false); !errors.Is(err, ErrRebindRequired) {
		t.Fatalf("recover: %v", err)
	}
	// 克隆:把第一对的记录原样拷进第二对。
	second.put(second.primary, root+"counter/primary", first.raw(first.primary, root+"counter/primary"))
	second.put(second.secondary, root+"counter/secondary", first.raw(first.secondary, root+"counter/secondary"))
	if _, _, err := second.issuer(nil).Allocate(context.Background(), "player", 100); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("clone allocate: %v", err)
	}
	if after := first.raw(registry, root+"registry/region/09"); string(after) != string(before) {
		t.Fatalf("the refused deployments changed the registry: %s -> %s", before, after)
	}
	a.take(first.issuer(nil), 100)
}

// drillState 是演练三段之间传递的状态。
type drillState struct {
	Root   string      `json:"root"`
	Ranges [][2]uint64 `json:"ranges"`
}

func loadDrill(t *testing.T, path string) drillState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state %s: %v", path, err)
	}
	var st drillState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func saveDrill(t *testing.T, path string, st drillState) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEtcdDrill 是真快照回档演练(问题 1 的验收):
//
//	drill-a:开启登记处、初始化、领一段。之后执行者对本地两套 etcd 拍快照。
//	drill-b:再重启 5 次,每次领一段,计数器跑到时钟前面。之后执行者停掉本地两套 etcd,
//	         用 drill-a 之后的快照恢复(同样的令牌和成员地址,集群号不变),再启动。
//	drill-c:新进程领段,必须在全部历史预留之上;本地记录回档必须被上报。
func TestEtcdDrill(t *testing.T) {
	phase := os.Getenv("MMORPG_ID_ISSUER_IT_PHASE")
	statePath := os.Getenv("MMORPG_ID_ISSUER_IT_STATE")
	if phase == "" || statePath == "" {
		t.Skip("MMORPG_ID_ISSUER_IT_PHASE and MMORPG_ID_ISSUER_IT_STATE are not set")
	}
	primary, secondary, registry := itEndpoints(t, "MMORPG_ID_ISSUER_IT_PRIMARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_SECONDARY"), itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY")
	switch phase {
	case "drill-a":
		st := drillState{Root: itRoot(t)}
		env := newItEnv(t, st.Root, primary, secondary, registry)
		env.prepare()
		lo, hi, err := env.issuer(nil).Allocate(context.Background(), "player", 100)
		if err != nil {
			t.Fatal(err)
		}
		st.Ranges = append(st.Ranges, [2]uint64{lo, hi})
		saveDrill(t, statePath, st)
	case "drill-b":
		st := loadDrill(t, statePath)
		env := newItEnv(t, st.Root, primary, secondary, registry)
		for restart := 0; restart < 5; restart++ {
			lo, hi, err := env.issuer(nil).Allocate(context.Background(), "player", 100)
			if err != nil {
				t.Fatal(err)
			}
			st.Ranges = append(st.Ranges, [2]uint64{lo, hi})
		}
		saveDrill(t, statePath, st)
		t.Logf("registry high water %d, records now point there; take the restore from the snapshot made after drill-a", env.registryHighWater())
	case "drill-c":
		st := loadDrill(t, statePath)
		env := newItEnv(t, st.Root, primary, secondary, registry)
		historical := env.registryHighWater()
		obs := newObserver()
		lo, hi, err := env.issuer(obs.record).Allocate(context.Background(), "player", 100)
		if err != nil {
			t.Fatal(err)
		}
		if lo < historical {
			t.Fatalf("lo=%d overlaps reservations that end at %d", lo, historical)
		}
		for _, r := range st.Ranges {
			if lo < r[1] && r[0] < hi {
				t.Fatalf("[%d, %d) overlaps issued [%d, %d)", lo, hi, r[0], r[1])
			}
		}
		if obs.count(ReserveLocalBehind) != 1 {
			t.Fatalf("the restore must be visible as records_behind_registry, observed %v (was the snapshot restored?)", obs.seen)
		}
	default:
		t.Fatalf("unknown phase %q", phase)
	}
}
