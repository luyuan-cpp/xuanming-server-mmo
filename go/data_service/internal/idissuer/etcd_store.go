package idissuer

import (
	"bytes"
	"context"
	"crypto/tls"
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
// 值是 JSON:{"counter":<计数器>,"authority":"<发号世系>","deployment":"<写下它的部署环境身份>"}。
// deployment 在 dev 写的记录里缺省。没上线,不认旧的纯数字格式:读到就当记录损坏,
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
	Counter    uint64 `json:"counter"`
	Authority  string `json:"authority"`
	Deployment string `json:"deployment,omitempty"`
}

// validRecord 是本地记录读写共用的格式校验:计数器非 0,世系合法,部署身份为空(dev)或合法。
func validRecord(body recordJSON) bool {
	return body.Counter != 0 && ValidAuthority(body.Authority) && (body.Deployment == "" || ValidDeployment(body.Deployment))
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
	if !validRecord(body) {
		return Record{}, fmt.Errorf("id issuer: record %s is corrupt: counter=%d authority=%q deployment=%q",
			s.key, body.Counter, body.Authority, body.Deployment)
	}
	return Record{Value: body.Counter, Authority: body.Authority, Deployment: body.Deployment, Revision: kv.ModRevision,
		Exists: true, ClusterID: clusterID}, nil
}

// CompareAndSwap 用 Txn 比较版本。超时等错误一律是结果未知,见 CounterStore 的契约。
func (s *EtcdStore) CompareAndSwap(ctx context.Context, prev Record, value uint64, authority, deployment string) (bool, error) {
	body := recordJSON{Counter: value, Authority: authority, Deployment: deployment}
	if !validRecord(body) {
		return false, fmt.Errorf("id issuer: refusing to write record %s: counter=%d authority=%q deployment=%q",
			s.key, value, authority, deployment)
	}
	raw, err := json.Marshal(body)
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
// (/id_registry/v1/next_region,重建期间带 rebuilding 标记,见 nextJSON)。
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
// 值是 JSON,字段见 claimJSON。墓碑可以没有 authority;在用的码必须有。deployment(部署环境身份)可以缺省:
// dev 自动认领的码和本字段出现之前写的记录没有它,生产预留会拒绝这样的码,要 rebind 补上。
type EtcdRegistry struct {
	cli  *clientv3.Client
	root string
}

// NewEtcdRegistry 建登记处的 etcd 客户端。不设 DialTimeout,理由同 NewEtcdStore。
// tlsConfig 是双向 TLS(LoadRegistryTLS),生产必须给;nil 只给 dev、test 的本地登记处。
// 地址照旧写 host:port、不带 scheme:tlsConfig 非空时客户端对每个地址都走 TLS,并按地址里的主机名核对服务端证书。
func NewEtcdRegistry(endpoints []string, tlsConfig *tls.Config) (*EtcdRegistry, error) {
	return newEtcdRegistryAt(endpoints, registryRoot, tlsConfig)
}

// newEtcdRegistryAt 让真 etcd 上的回归用自己的键根,不碰正式的登记处键。
func newEtcdRegistryAt(endpoints []string, root string, tlsConfig *tls.Config) (*EtcdRegistry, error) {
	if len(endpoints) == 0 {
		return nil, errors.New("id issuer: no etcd endpoints for the id registry")
	}
	cli, err := clientv3.New(clientv3.Config{Endpoints: endpoints, TLS: tlsConfig})
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
	Deployment       string `json:"deployment,omitempty"`
	PrimaryCluster   uint64 `json:"primary_cluster"`
	SecondaryCluster uint64 `json:"secondary_cluster"`
	HighWater        uint64 `json:"high_water"`
	Blocks           uint64 `json:"blocks"`
}

func toClaimJSON(c Claim) claimJSON {
	return claimJSON{State: c.State, Authority: c.Authority, Deployment: c.Deployment, PrimaryCluster: c.PrimaryCluster,
		SecondaryCluster: c.SecondaryCluster, HighWater: c.HighWater, Blocks: c.Blocks}
}

func validClaim(c Claim) bool {
	if c.Deployment != "" && !ValidDeployment(c.Deployment) {
		return false
	}
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
	claim := Claim{State: body.State, Authority: body.Authority, Deployment: body.Deployment, PrimaryCluster: body.PrimaryCluster,
		SecondaryCluster: body.SecondaryCluster, HighWater: body.HighWater, Blocks: body.Blocks}
	if !validClaim(claim) {
		return ClaimRecord{}, fmt.Errorf("id issuer: registry entry %s is corrupt: %+v", key, body)
	}
	return ClaimRecord{Claim: claim, Revision: kv.ModRevision, Exists: true, ClusterID: clusterID}, nil
}

// CompareAndSwap 用 Txn 比较版本。超时等错误一律是结果未知。prev 不存在时是补一条记录,见 createEntry。
func (r *EtcdRegistry) CompareAndSwap(ctx context.Context, region uint8, prev ClaimRecord, next Claim) (bool, error) {
	key := r.key(region)
	if !validClaim(next) {
		return false, fmt.Errorf("id issuer: refusing to write registry entry %s: %+v", key, next)
	}
	raw, err := json.Marshal(toClaimJSON(next))
	if err != nil {
		return false, err
	}
	if prev.Exists {
		return casKey(ctx, r.cli, key, true, prev.Revision, raw)
	}
	return r.createEntry(ctx, region, next.State, key, raw)
}

// createEntry 给一个没有记录的码补一条记录(Recover 重新登记、Retire 补墓碑、dev 第一次预留时认领),并在同一个事务里
// 守住「有记录的码都小于下一个可认领的码」,规则见 Registry.CompareAndSwap 的契约。
func (r *EtcdRegistry) createEntry(ctx context.Context, region uint8, state, key string, raw []byte) (bool, error) {
	p, err := r.readPointer(ctx)
	if err != nil {
		return false, err
	}
	absent := clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	put := clientv3.OpPut(key, string(raw))
	txn := r.cli.Txn(ctx)
	switch {
	case !p.declared:
		if refusal := allowCreateWithoutPointer(region, state); refusal != nil {
			return false, refusal
		}
		// 提交时必须仍然没有指针:和 Open、BeginRebuild 抢在一起时,由它们的范围复核或这里的比较让其中一方重来。
		txn = txn.If(absent, clientv3.Compare(clientv3.CreateRevision(r.nextKey()), "=", 0)).Then(put)
	case uint32(region) < p.next:
		// 下一个可认领的码只增不减,这个码之后也一直小于它,不用再比。
		txn = txn.If(absent).Then(put)
	default:
		if refusal := allowCreateAtOrAboveNext(region, state, p.next); refusal != nil {
			return false, refusal
		}
		// 重建中推指针,仍然留在重建状态:开放认领只由 Open 决定。
		rawNext, marshalErr := json.Marshal(nextJSON{Next: uint32(region) + 1, Rebuilding: p.rebuilding})
		if marshalErr != nil {
			return false, marshalErr
		}
		txn = txn.If(absent, clientv3.Compare(clientv3.ModRevision(r.nextKey()), "=", p.revision)).
			Then(put, clientv3.OpPut(r.nextKey(), string(rawNext)))
	}
	resp, err := txn.Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// allowCreateWithoutPointer 判断登记处还没声明「下一个可认领的码」时能不能补记录。在用的码可以:dev 第一次预留时认领,
// 或者 Recover 凭幸存的本地记录重新登记;之后声明指针时会核对它高于全部已有记录。墓碑不行:没有指针可以对照,
// 填错的码也会写进去,之后声明指针就被它卡住。重建登记处要先用 -id-registry-rebuild 声明指针,再补墓碑。
func allowCreateWithoutPointer(region uint8, state string) error {
	if state == ClaimActive {
		return nil
	}
	return fmt.Errorf("%w: a tombstone for code %d needs the next claimable code to be checked against; "+
		"declare it first with data_service -id-registry-rebuild", ErrRegistryNotOpened, region)
}

// allowCreateAtOrAboveNext 判断能不能给一个不小于「下一个可认领的码」的码补记录(补完会把那个指针推到 region+1)。
// 在用的码可以:它是 Recover 凭幸存的本地记录重新登记的,说明登记处重建时开启得太低。墓碑只有恰好等于
// 下一个可认领的码时可以:这个码不用了,跳过它。墓碑更大时拒绝:没有任何证据说明中间的码用过,填错一次就烧掉一段。
func allowCreateAtOrAboveNext(region uint8, state string, next uint32) error {
	if state == ClaimActive || uint32(region) == next {
		return nil
	}
	return fmt.Errorf("%w: code %d was never claimed and the next claimable code is %d, there is nothing to retire",
		ErrRegionOutOfOrder, region, next)
}

// nextJSON 是登记处里「下一个可认领的码」:{"next":N},重建期间是 {"next":N,"rebuilding":true}。
type nextJSON struct {
	Next       uint32 `json:"next"`
	Rebuilding bool   `json:"rebuilding,omitempty"`
}

// claimPointer 是读到的「下一个可认领的码」,有三种状态:
//   - 没声明(declared=false):键不存在。新世界还没开启,或者登记处整个丢了数据。
//   - 重建中(rebuilding=true):声明了,只用来核对补进来的记录,还不开放认领。
//   - 已开放:声明了,ClaimNew 可以认领 next。
type claimPointer struct {
	next       uint32
	revision   int64
	declared   bool
	rebuilding bool
}

func (r *EtcdRegistry) readPointer(ctx context.Context) (claimPointer, error) {
	resp, err := r.cli.Get(ctx, r.nextKey())
	if err != nil {
		return claimPointer{}, err
	}
	if len(resp.Kvs) == 0 {
		return claimPointer{}, nil
	}
	var body nextJSON
	if err := decodeStrict(resp.Kvs[0].Value, &body); err != nil || body.Next > MaxNextRegion {
		return claimPointer{}, fmt.Errorf("id issuer: registry key %s is corrupt: %q", r.nextKey(), resp.Kvs[0].Value)
	}
	return claimPointer{next: body.Next, revision: resp.Kvs[0].ModRevision, declared: true, rebuilding: body.Rebuilding}, nil
}

// ClaimNew 在一个 Txn 里核「下一个可认领的码」没变、这个码没有记录,再写这个码的记录并推进「下一个可认领的码」。
func (r *EtcdRegistry) ClaimNew(ctx context.Context, region uint8, claim Claim) (bool, error) {
	key := r.key(region)
	if claim.State != ClaimActive || !validClaim(claim) {
		return false, fmt.Errorf("id issuer: refusing to claim %s with %+v", key, claim)
	}
	p, err := r.readPointer(ctx)
	if err != nil {
		return false, err
	}
	if !p.declared {
		return false, ErrRegistryNotOpened
	}
	if p.rebuilding {
		return false, ErrRegistryRebuilding
	}
	if uint32(region) < p.next {
		return false, fmt.Errorf("%w: code %d is below the next claimable code %d", ErrRegionClaimed, region, p.next)
	}
	if uint32(region) > p.next {
		return false, fmt.Errorf("%w: the next claimable code is %d, this deployment is configured with %d", ErrRegionOutOfOrder, p.next, region)
	}
	raw, err := json.Marshal(toClaimJSON(claim))
	if err != nil {
		return false, err
	}
	rawNext, err := json.Marshal(nextJSON{Next: uint32(region) + 1})
	if err != nil {
		return false, err
	}
	resp, err := r.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.ModRevision(r.nextKey()), "=", p.revision),
		clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
	).Then(clientv3.OpPut(key, string(raw)), clientv3.OpPut(r.nextKey(), string(rawNext))).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// BeginRebuild 声明「下一个可认领的码」但不开放认领,见 Registry 的契约。
func (r *EtcdRegistry) BeginRebuild(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		p, err := r.readPointer(ctx)
		if err != nil || p.declared {
			return false, err
		}
		declared, err := r.declarePointer(ctx, nextJSON{Next: next, Rebuilding: true})
		if err != nil || declared {
			return declared, err
		}
	}
	return false, ErrContended
}

// Open 开放认领,见 Registry 的契约:没声明时写入「下一个可认领的码」,重建中时结束重建。
func (r *EtcdRegistry) Open(ctx context.Context, next uint32) (bool, error) {
	if next > MaxNextRegion {
		return false, fmt.Errorf("id issuer: next region %d exceeds %d", next, MaxNextRegion)
	}
	for attempt := 0; attempt < reserveAttempts; attempt++ {
		p, err := r.readPointer(ctx)
		if err != nil {
			return false, err
		}
		if p.declared && !p.rebuilding {
			return false, nil
		}
		var opened bool
		if p.rebuilding {
			opened, err = r.finishRebuild(ctx, p, next)
		} else {
			opened, err = r.declarePointer(ctx, nextJSON{Next: next})
		}
		if err != nil || opened {
			return opened, err
		}
	}
	return false, ErrContended
}

// declarePointer 在还没有「下一个可认领的码」时写入它。
//   - 声明重建(body.Rebuilding):已有记录的码(之前凭本地记录重新登记的)都必须小于它。
//   - 直接开放(世界第一次部署):不能已有任何码的记录。有就是丢过数据的登记处,要先声明重建(ErrRegistryNeedsRebuild)。
//
// false = 提交时输给了并发的写入,什么都没写,调用方重读再来。
func (r *EtcdRegistry) declarePointer(ctx context.Context, body nextJSON) (bool, error) {
	prefix := r.root + "region/"
	raw, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	resp, err := r.cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return false, err
	}
	for _, kv := range resp.Kvs {
		code, err := strconv.ParseUint(strings.TrimPrefix(string(kv.Key), prefix), 10, 8)
		if err != nil {
			return false, fmt.Errorf("id issuer: unexpected registry key %q", kv.Key)
		}
		if !body.Rebuilding {
			return false, fmt.Errorf("%w: code %d already has an entry", ErrRegistryNeedsRebuild, code)
		}
		if uint32(code) >= body.Next {
			return false, fmt.Errorf("%w: code %d already has an entry, the next claimable code must be above it", ErrRegionClaimed, code)
		}
	}
	// 扫描与提交之间可能有 Recover 新增记录,必须连同范围一起在提交点复核。
	// 范围内任何键的版本超过扫描版本就重读;仅比较已扫描键会漏掉当时还不存在的码。
	txn, err := r.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.CreateRevision(r.nextKey()), "=", 0),
		clientv3.Compare(clientv3.ModRevision(prefix), "<", resp.Header.Revision+1).WithPrefix(),
	).Then(clientv3.OpPut(r.nextKey(), string(raw))).Commit()
	if err != nil {
		return false, err
	}
	return txn.Succeeded, nil
}

// finishRebuild 结束重建、开放认领。next 必须等于登记处里现在的值:重建期间重新登记的码可能把它推高过,
// 让运维对着地域码台账再确认一次。重建期间补记录都按指针核对过,这里不用再扫。
// false = 提交时指针被并发的补记录推过,什么都没写,调用方重读再来。
func (r *EtcdRegistry) finishRebuild(ctx context.Context, p claimPointer, next uint32) (bool, error) {
	if next != p.next {
		return false, fmt.Errorf("%w: the registry holds %d, the command gave %d", ErrNextRegionMismatch, p.next, next)
	}
	raw, err := json.Marshal(nextJSON{Next: next})
	if err != nil {
		return false, err
	}
	return casKey(ctx, r.cli, r.nextKey(), true, p.revision, raw)
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
