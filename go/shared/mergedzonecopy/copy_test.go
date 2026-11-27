package mergedzonecopy

import (
	"context"
	"errors"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// mapping 造一份映射;pairs 依次是源区、目标区。
func mapping(generation string, version uint64, pairs ...uint32) Mapping {
	merges := make(map[uint32]uint32, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		merges[pairs[i]] = pairs[i+1]
	}
	return Mapping{Generation: generation, Version: version, Merges: merges}
}

// 编译期断言:EtcdCopies 满足 Copies,*clientv3.Client 满足 KV。
var (
	_ Copies = (*EtcdCopies)(nil)
	_ KV     = (*clientv3.Client)(nil)
)

// Decide 是两个写者共用的判定(设计稿 §4.3 上半张):缺席、落后就发布;相同不写;领先、同号不同内容、别的世代、坏映射都拒绝。
func TestDecide(t *testing.T) {
	const key = KeyPrefix + "testdb"
	truth := mapping("g1", 4, 5, 6)
	for _, tc := range []struct {
		name        string
		published   Mapping
		found       bool
		truth       Mapping
		wantPublish bool
		wantErr     string
	}{
		{name: "没有副本", found: false, truth: truth, wantPublish: true},
		{name: "副本落后", published: mapping("g1", 3), found: true, truth: truth, wantPublish: true},
		{name: "版本相同、内容相同", published: mapping("g1", 4, 5, 6), found: true, truth: truth},
		{name: "版本相同、内容不同", published: mapping("g1", 4, 5, 7), found: true, truth: truth, wantErr: "different contents"},
		{name: "第 0 片落后于副本", published: mapping("g1", 5, 5, 6), found: true, truth: truth, wantErr: "behind version 5"},
		{name: "副本是别的目录库的", published: mapping("g0", 9), found: true, truth: truth, wantErr: "another directory database"},
		{name: "成环", found: false, truth: mapping("g1", 4, 5, 6, 6, 5), wantErr: "broken"},
		{name: "区号 0", found: false, truth: mapping("g1", 4, 0, 6), wantErr: "cannot be published"},
		{name: "没有世代", found: false, truth: mapping("", 4, 5, 6), wantErr: "cannot be published"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publish, err := Decide(key, tc.published, tc.found, tc.truth)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || publish {
					t.Fatalf("Decide = (%t, %v), want an error containing %q", publish, err, tc.wantErr)
				}
				return
			}
			if err != nil || publish != tc.wantPublish {
				t.Fatalf("Decide = (%t, %v), want (%t, nil)", publish, err, tc.wantPublish)
			}
		})
	}
	// 拒绝的文案点名副本键:运维要删的就是它。
	if _, err := Decide(key, mapping("g1", 5), true, truth); err == nil || !strings.Contains(err.Error(), key) {
		t.Fatalf("the refusal must name the key: %v", err)
	}
}

func TestCodec(t *testing.T) {
	m := mapping("g1", 7, 9, 6, 5, 6)
	raw, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"generation":"g1","version":7,"merges":[{"src":5,"dst":6},{"src":9,"dst":6}]}`; string(raw) != want {
		t.Fatalf("按源区升序编码,同一份映射字节相同:\n got %s\nwant %s", raw, want)
	}
	decoded, err := Decode(raw)
	if err != nil || decoded.Generation != "g1" || decoded.Version != 7 || !maps.Equal(decoded.Merges, m.Merges) {
		t.Fatalf("往返: (%+v, %v)", decoded, err)
	}
	empty, err := Decode([]byte(`{"generation":"g1","version":0,"merges":[]}`))
	if err != nil || empty.Merges == nil || len(empty.Merges) != 0 {
		t.Fatalf("没有合服时是空表,不是 nil: (%+v, %v)", empty, err)
	}
	if _, err := Decode([]byte(`{"generation":"g1","version":1,"merges":[],"later_field":true}`)); err != nil {
		t.Fatalf("多余字段不拒绝: %v", err)
	}
	for name, raw := range map[string]string{
		"坏 JSON": `not json`,
		"没有世代":   `{"version":1,"merges":[]}`,
		"区号 0":   `{"generation":"g1","version":1,"merges":[{"src":0,"dst":6}]}`,
		"目标区 0":  `{"generation":"g1","version":1,"merges":[{"src":5,"dst":0}]}`,
		"源区出现两次": `{"generation":"g1","version":1,"merges":[{"src":5,"dst":6},{"src":5,"dst":7}]}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("%s 必须拒绝: %s", name, raw)
		}
	}
	if _, err := Encode(mapping("", 1)); err == nil {
		t.Fatal("没有世代的映射不能编码进副本")
	}
	if got := Key("mmorpg_global_00"); got != "/mmorpg/merged_zone_copy/mmorpg_global_00" {
		t.Fatalf("Key = %q", got)
	}
}

// fakeKV 是 etcd 的替身:只有一个键,ModRevision 0 = 不存在;事务按 Compare(ModRevision, "=", r) 判,和 etcd 同一语义。
type fakeKV struct {
	value  []byte
	modRev int64
	next   int64 // 下一次写入的 ModRevision:etcd 的 revision 全局递增,写一次不止加 1 也合法
}

func (f *fakeKV) Get(_ context.Context, key string, _ ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	if f.modRev == 0 {
		return &clientv3.GetResponse{}, nil
	}
	return &clientv3.GetResponse{Kvs: []*mvccpb.KeyValue{{Key: []byte(key), Value: f.value, ModRevision: f.modRev}}}, nil
}

func (f *fakeKV) Txn(context.Context) clientv3.Txn { return &fakeTxn{kv: f} }

type fakeTxn struct {
	kv   *fakeKV
	cmps []clientv3.Cmp
	ops  []clientv3.Op
}

func (t *fakeTxn) If(cmps ...clientv3.Cmp) clientv3.Txn { t.cmps = cmps; return t }
func (t *fakeTxn) Then(ops ...clientv3.Op) clientv3.Txn { t.ops = ops; return t }
func (t *fakeTxn) Else(...clientv3.Op) clientv3.Txn     { return t }
func (t *fakeTxn) Commit() (*clientv3.TxnResponse, error) {
	if len(t.cmps) != 1 || t.cmps[0].Target != etcdserverpb.Compare_MOD || t.cmps[0].Result != etcdserverpb.Compare_EQUAL || len(t.ops) != 1 {
		return nil, errors.New("fake etcd: the publish must be one ModRevision equality compare and one put")
	}
	union, ok := t.cmps[0].TargetUnion.(*etcdserverpb.Compare_ModRevision)
	if !ok {
		return nil, errors.New("fake etcd: compare without a ModRevision")
	}
	if union.ModRevision != t.kv.modRev {
		return &clientv3.TxnResponse{Succeeded: false}, nil
	}
	t.kv.next += 7
	t.kv.value, t.kv.modRev = t.ops[0].ValueBytes(), t.kv.next
	return &clientv3.TxnResponse{Succeeded: true}, nil
}

// EtcdCopies 的 CAS:键不存在时 revision 0 成立;按读到的 revision 覆盖;旧 revision 冲突;坏值按读失败报。
func TestEtcdCopiesCompareAndPut(t *testing.T) {
	ctx := context.Background()
	kv := &fakeKV{}
	copies := NewEtcdCopies(kv, "testdb")
	if copies.Key() != KeyPrefix+"testdb" {
		t.Fatalf("Key = %q", copies.Key())
	}
	if _, revision, found, err := copies.Get(ctx); err != nil || found || revision != 0 {
		t.Fatalf("键不存在: revision=%d found=%t err=%v", revision, found, err)
	}
	if written, err := copies.Put(ctx, 0, mapping("g1", 1, 5, 6)); err != nil || !written {
		t.Fatalf("键不存在时 revision 0 成立: written=%t err=%v", written, err)
	}
	if written, err := copies.Put(ctx, 0, mapping("g1", 1, 5, 6)); err != nil || written {
		t.Fatalf("键已存在时 revision 0 必须冲突: written=%t err=%v", written, err)
	}
	got, revision, found, err := copies.Get(ctx)
	if err != nil || !found || revision == 0 || got.Version != 1 || !maps.Equal(got.Merges, map[uint32]uint32{5: 6}) {
		t.Fatalf("读回: %+v revision=%d found=%t err=%v", got, revision, found, err)
	}
	if written, err := copies.Put(ctx, revision, mapping("g1", 2)); err != nil || !written {
		t.Fatalf("按读到的 revision 覆盖: written=%t err=%v", written, err)
	}
	if written, err := copies.Put(ctx, revision, mapping("g1", 3)); err != nil || written {
		t.Fatalf("旧 revision 必须冲突: written=%t err=%v", written, err)
	}
	if _, err := copies.Put(ctx, kv.modRev, mapping("", 4)); err == nil {
		t.Fatal("编码不过的映射一个字节都不写")
	}
	kv.value = []byte(`{"generation":"","version":1}`)
	if _, _, _, err := copies.Get(ctx); err == nil {
		t.Fatal("坏值按读失败报")
	}
}

// 真 etcd 用例:MERGEDZONECOPY_IT_ETCD=127.0.0.1:2379 时才跑。钉住真 etcd 上 CAS 的形状。
func TestEtcdCopiesAgainstRealEtcd(t *testing.T) {
	hosts := os.Getenv("MERGEDZONECOPY_IT_ETCD")
	if hosts == "" {
		t.Skip("MERGEDZONECOPY_IT_ETCD is not set")
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: strings.Split(hosts, ","), DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	copies := NewEtcdCopies(client, "it_"+time.Now().Format("20060102150405.000000"))
	defer client.Delete(context.Background(), copies.Key())

	if _, revision, found, err := copies.Get(ctx); err != nil || found || revision != 0 {
		t.Fatalf("键不存在: revision=%d found=%t err=%v", revision, found, err)
	}
	if written, err := copies.Put(ctx, 0, mapping("g1", 1, 5, 6)); err != nil || !written {
		t.Fatalf("键不存在时 revision 0 成立: written=%t err=%v", written, err)
	}
	if written, err := copies.Put(ctx, 0, mapping("g1", 1, 5, 6)); err != nil || written {
		t.Fatalf("键已存在时 revision 0 必须冲突: written=%t err=%v", written, err)
	}
	got, revision, found, err := copies.Get(ctx)
	if err != nil || !found || revision == 0 || got.Version != 1 || !maps.Equal(got.Merges, map[uint32]uint32{5: 6}) {
		t.Fatalf("读回: %+v revision=%d found=%t err=%v", got, revision, found, err)
	}
	if written, err := copies.Put(ctx, revision, mapping("g1", 2)); err != nil || !written {
		t.Fatalf("按读到的 revision 覆盖: written=%t err=%v", written, err)
	}
	if written, err := copies.Put(ctx, revision, mapping("g1", 3)); err != nil || written {
		t.Fatalf("旧 revision 必须冲突: written=%t err=%v", written, err)
	}
}
