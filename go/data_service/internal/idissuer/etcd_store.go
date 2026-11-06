package idissuer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdStore 把一份本地预留记录存在 etcd 的一个键里。
//
// 值是 JSON:{"counter":<计数器>,"authority":"<发号世系>"}。没上线,不认旧的纯数字格式:读到就当记录损坏,
// dev 清掉 /id_issuer/ 和 /id_registry/ 下的键重来。
type EtcdStore struct {
	cli *clientv3.Client
	key string
}

// NewEtcdStore 建 etcd 客户端。key 是这份记录的键。
//
// 刻意不设 DialTimeout:设了就是阻塞拨号,etcd 不可达时 New 会失败,而发号器不该因为 etcd
// 暂时不可达就装配不起来。连不上的症状留给每次调用,受调用方的 ctx 约束,恢复后自动好。
func NewEtcdStore(endpoints []string, key string) (*EtcdStore, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("id issuer: no etcd endpoints for %s", key)
	}
	if key == "" {
		return nil, fmt.Errorf("id issuer: empty etcd key")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints})
	if err != nil {
		return nil, fmt.Errorf("id issuer: connect etcd for %s: %w", key, err)
	}
	return &EtcdStore{cli: cli, key: key}, nil
}

type recordJSON struct {
	Counter   uint64 `json:"counter"`
	Authority string `json:"authority"`
}

// Read 是 etcd 默认的线性读。值损坏时返回错误,不当作不存在。
func (s *EtcdStore) Read(ctx context.Context) (Record, error) {
	resp, err := s.cli.Get(ctx, s.key)
	if err != nil {
		return Record{}, err
	}
	clusterID := resp.Header.ClusterId
	if len(resp.Kvs) == 0 {
		return Record{ClusterID: clusterID}, nil
	}
	kv := resp.Kvs[0]
	var body recordJSON
	if err := decodeStrict(kv.Value, &body); err != nil {
		return Record{}, fmt.Errorf("id issuer: record %s is corrupt: %w", s.key, err)
	}
	if body.Counter == 0 || !ValidAuthority(body.Authority) {
		return Record{}, fmt.Errorf("id issuer: record %s is corrupt: counter=%d authority=%q", s.key, body.Counter, body.Authority)
	}
	return Record{Value: body.Counter, Authority: body.Authority, Revision: kv.ModRevision, Exists: true, ClusterID: clusterID}, nil
}

// CompareAndSwap 用 Txn 比较版本。超时等错误一律是结果未知,见 CounterStore 的契约。
func (s *EtcdStore) CompareAndSwap(ctx context.Context, prev Record, value uint64, authority string) (bool, error) {
	if value == 0 || !ValidAuthority(authority) {
		return false, fmt.Errorf("id issuer: refusing to write record %s: counter=%d authority=%q", s.key, value, authority)
	}
	raw, err := json.Marshal(recordJSON{Counter: value, Authority: authority})
	if err != nil {
		return false, err
	}
	return casKey(ctx, s.cli, s.key, prev.Exists, prev.Revision, raw)
}

// Close 关闭 etcd 连接。
func (s *EtcdStore) Close() error {
	return s.cli.Close()
}

// registryRoot 是登记处的键根。一个地域码一个键(/id_registry/v1/region/07),外加「下一个可认领的码」
// (/id_registry/v1/next_region)。
const registryRoot = "/id_registry/v1/"

// RegistryKey 是地域码在登记处的键,运维手册里 etcdctl 查看用。
func RegistryKey(region uint8) string {
	return registryRegionKey(registryRoot, region)
}

func registryRegionKey(root string, region uint8) string {
	return fmt.Sprintf("%sregion/%02d", root, region)
}

// EtcdRegistry 是放在 etcd 上的全局号源登记处。
//
// 值是 JSON,字段见 claimJSON。墓碑可以没有 authority;在用的码必须有。
type EtcdRegistry struct {
	cli  *clientv3.Client
	root string
}

// NewEtcdRegistry 建登记处的 etcd 客户端。不设 DialTimeout,理由同 NewEtcdStore。
func NewEtcdRegistry(endpoints []string) (*EtcdRegistry, error) {
	return newEtcdRegistryAt(endpoints, registryRoot)
}

// newEtcdRegistryAt 让真 etcd 上的回归用自己的键根,不碰正式的登记处键。
func newEtcdRegistryAt(endpoints []string, root string) (*EtcdRegistry, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("id issuer: no etcd endpoints for the id registry")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints})
	if err != nil {
		return nil, fmt.Errorf("id issuer: connect etcd for the id registry: %w", err)
	}
	return &EtcdRegistry{cli: cli, root: root}, nil
}

func (r *EtcdRegistry) key(region uint8) string {
	return registryRegionKey(r.root, region)
}

func (r *EtcdRegistry) nextKey() string {
	return r.root + "next_region"
}

type claimJSON struct {
	State            string `json:"state"`
	Authority        string `json:"authority,omitempty"`
	PrimaryCluster   uint64 `json:"primary_cluster"`
	SecondaryCluster uint64 `json:"secondary_cluster"`
	HighWater        uint64 `json:"high_water"`
	Blocks           uint64 `json:"blocks"`
}

func validClaim(c Claim) bool {
	switch c.State {
	case ClaimActive:
		return ValidAuthority(c.Authority) && c.HighWater != 0
	case ClaimRetired:
		return c.Authority == "" || ValidAuthority(c.Authority)
	}
	return false
}

// Read 是线性读。值损坏时返回错误,不当作不存在:登记处的一条记录读不懂,就不能证明这个码没用过。
func (r *EtcdRegistry) Read(ctx context.Context, region uint8) (ClaimRecord, error) {
	key := r.key(region)
	resp, err := r.cli.Get(ctx, key)
	if err != nil {
		return ClaimRecord{}, err
	}
	clusterID := resp.Header.ClusterId
	if len(resp.Kvs) == 0 {
		return ClaimRecord{ClusterID: clusterID}, nil
	}
	kv := resp.Kvs[0]
	var body claimJSON
	if err := decodeStrict(kv.Value, &body); err != nil {
		return ClaimRecord{}, fmt.Errorf("id issuer: registry entry %s is corrupt: %w", key, err)
	}
	claim := Claim{State: body.State, Authority: body.Authority, PrimaryCluster: body.PrimaryCluster,
		SecondaryCluster: body.SecondaryCluster, HighWater: body.HighWater, Blocks: body.Blocks}
	if !validClaim(claim) {
		return ClaimRecord{}, fmt.Errorf("id issuer: registry entry %s is corrupt: %+v", key, body)
	}
	return ClaimRecord{Claim: claim, Revision: kv.ModRevision, Exists: true, ClusterID: clusterID}, nil
}

// CompareAndSwap 用 Txn 比较版本。超时等错误一律是结果未知。
func (r *EtcdRegistry) CompareAndSwap(ctx context.Context, region uint8, prev ClaimRecord, next Claim) (bool, error) {
	key := r.key(region)
	if !validClaim(next) {
		return false, fmt.Errorf("id issuer: refusing to write registry entry %s: %+v", key, next)
	}
	raw, err := json.Marshal(claimJSON{State: next.State, Authority: next.Authority, PrimaryCluster: next.PrimaryCluster,
		SecondaryCluster: next.SecondaryCluster, HighWater: next.HighWater, Blocks: next.Blocks})
	if err != nil {
		return false, err
	}
	return casKey(ctx, r.cli, key, prev.Exists, prev.Revision, raw)
}

// nextJSON 是登记处里「下一个可认领的码」:{"next":N}。没有这个键 = 登记处没开启。
type nextJSON struct {
	Next uint32 `json:"next"`
}

func (r *EtcdRegistry) readNext(ctx context.Context) (next uint32, revision int64, opened bool, err error) {
	resp, err := r.cli.Get(ctx, r.nextKey())
	if err != nil {
		return 0, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return 0, 0, false, nil
	}
	var body nextJSON
	if err := decodeStrict(resp.Kvs[0].Value, &body); err != nil || body.Next > MaxNextRegion {
		return 0, 0, false, fmt.Errorf("id issuer: registry key %s is corrupt: %q", r.nextKey(), resp.Kvs[0].Value)
	}
	return body.Next, resp.Kvs[0].ModRevision, true, nil
}

// ClaimNew 在一个 Txn 里核「下一个可认领的码」没变、这个码没有记录,再写这个码的记录并推进「下一个可认领的码」。
func (r *EtcdRegistry) ClaimNew(ctx context.Context, region uint8, claim Claim) (bool, error) {
	key := r.key(region)
	if claim.State != ClaimActive || !validClaim(claim) {
		return false, fmt.Errorf("id issuer: refusing to claim %s with %+v", key, claim)
	}
	next, revision, opened, err := r.readNext(ctx)
	if err != nil {
		return false, err
	}
	if !opened {
		return false, ErrRegistryNotOpened
	}
	if uint32(region) < next {
		return false, fmt.Errorf("%w: code %d is below the next claimable code %d", ErrRegionClaimed, region, next)
	}
	raw, err := json.Marshal(claimJSON{State: claim.State, Authority: claim.Authority, PrimaryCluster: claim.PrimaryCluster,
		SecondaryCluster: claim.SecondaryCluster, HighWater: claim.HighWater, Blocks: claim.Blocks})
	if err != nil {
		return false, err
	}
	rawNext, err := json.Marshal(nextJSON{Next: uint32(region) + 1})
	if err != nil {
		return false, err
	}
	resp, err := r.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.ModRevision(r.nextKey()), "=", revision),
		clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
	).Then(clientv3.OpPut(key, string(raw)), clientv3.OpPut(r.nextKey(), string(rawNext))).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// Open 写入「下一个可认领的码」。已有记录的码(重建时重新登记的、补的墓碑)都必须小于 next。
func (r *EtcdRegistry) Open(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	if _, _, opened, err := r.readNext(ctx); err != nil || opened {
		return false, err
	}
	prefix := r.root + "region/"
	resp, err := r.cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return false, err
	}
	for _, kv := range resp.Kvs {
		code, err := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), prefix), 10, 8)
		if err != nil {
			return false, fmt.Errorf("id issuer: unexpected registry key %q", kv.Key)
		}
		if uint32(code) >= next {
			return false, fmt.Errorf("%w: code %d already has an entry, the next claimable code must be above it", ErrRegionClaimed, code)
		}
	}
	raw, err := json.Marshal(nextJSON{Next: next})
	if err != nil {
		return false, err
	}
	txn, err := r.cli.Txn(ctx).If(clientv3.Compare(clientv3.CreateRevision(r.nextKey()), "=", 0)).
		Then(clientv3.OpPut(r.nextKey(), string(raw))).Commit()
	if err != nil {
		return false, err
	}
	return txn.Succeeded, nil
}

// Close 关闭 etcd 连接。
func (r *EtcdRegistry) Close() error {
	return r.cli.Close()
}

// casKey:键仍是 prev(存在时比 ModRevision,不存在时比 CreateRevision=0)才写入。Txn 客户端最多发一次,
// 错误就是结果未知。
func casKey(ctx context.Context, cli *clientv3.Client, key string, exists bool, revision int64, value []byte) (bool, error) {
	var cmp clientv3.Cmp
	if exists {
		cmp = clientv3.Compare(clientv3.ModRevision(key), "=", revision)
	} else {
		cmp = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	}
	resp, err := cli.Txn(ctx).If(cmp).Then(clientv3.OpPut(key, string(value))).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// decodeStrict 解一个 JSON 对象:不认识的字段、多余的内容都算损坏。
func decodeStrict(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}
