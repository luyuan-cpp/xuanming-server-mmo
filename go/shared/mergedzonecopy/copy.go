// Package mergedzonecopy 是合服映射在 etcd 里的副本(docs/design/merged-zone-etcd-copy.md)。
//
// 真源是目录第 0 片的 zone_merged_into,只有 tools/merge_zone 写它,和 zone_merge_history 同一个事务。副本由两方写:
//   - data_service 每读到一个新版本,先发布进副本再回答;第 0 片读不到时按副本回答;
//   - tools/merge_zone 提交之后马上发布,读不到第 0 片的 data_service 副本因此也在一个刷新周期内看到这次合服。
//
// 两方用同一条读第 0 片的语句(ReadSQL)、同一个编码、同一套发布判定(Decide),按 ModRevision 做 CAS。
// 副本只会被改大,所以它是所有写过它的一方读到过的最高版本。
package mergedzonecopy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/placement"
)

// KeyPrefix 是副本键的前缀,后面接目录第 0 片的库名。
const KeyPrefix = "/mmorpg/merged_zone_copy/"

// PublishAttempts 是一次发布撞上别的写者(CAS 不成立)时最多读几轮。
const PublishAttempts = 3

// Key 返回目录第 0 片的副本键。shardZeroDB 是第 0 片的库名:Directory.LogicalShards=1 时就是目录基名,
// 否则是 <基名>_00(shared/dirshard.DirectoryDBName)。按库名区分,两个部署误用同一套 etcd 时不撞键。
func Key(shardZeroDB string) string { return KeyPrefix + shardZeroDB }

// Mapping 是第 0 片上合服映射的一次一致读,也是副本的值。
type Mapping struct {
	// Generation 是第 0 片的 route_meta.generation:迁移种行时随机生成,之后不改;PITR、搬库都带着它走,只有库重建才会变。
	Generation string
	// Version 是 zone_merge_history 的行数。merge_zone 每改一次映射都在同一个事务里追加恰好一行历史,历史只增不改,
	// 所以映射每变一次它加 1,同一个值只对应一份映射。
	Version uint64
	// Merges 是源区 → 目标区(没有沿链解析)。没有合服时是空表,不是 nil。
	Merges map[uint32]uint32
}

// ReadSQL 是第 0 片上合服映射的一次一致读(不取锁):库世代、版本和全部映射行,一条语句读完。
// 一条语句在 MySQL 上共用一个读视图(RC 下按语句取),在 TiDB 上共用语句的时间戳,所以三样是同一个快照;
// merge_zone 的映射与历史同一个事务提交,要么都看到,要么都看不到。没有映射时 LEFT JOIN 回一行,src_zone、dst_zone 为 NULL。
const ReadSQL = "SELECT m.generation, h.history_rows, z.src_zone, z.dst_zone" +
	" FROM (SELECT generation FROM route_meta WHERE id = 1) AS m" +
	" CROSS JOIN (SELECT COUNT(*) AS history_rows FROM zone_merge_history) AS h" +
	" LEFT JOIN zone_merged_into AS z ON 1 = 1" +
	" ORDER BY z.src_zone"

// ErrNoRouteMeta 表示第 0 片没有 route_meta(id=1):迁移没跑过,或连错了库。
var ErrNoRouteMeta = errors.New("route_meta has no row id=1")

// Queryer 是 *sql.DB 与 *sql.Tx 共有的多行读。
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Read 在 q 上执行 ReadSQL。q 必须连着目录第 0 片(zone_merged_into、zone_merge_history 只在那里)。
func Read(ctx context.Context, q Queryer) (Mapping, error) {
	rows, err := q.QueryContext(ctx, ReadSQL)
	if err != nil {
		return Mapping{}, fmt.Errorf("read zone_merged_into: %w", err)
	}
	defer rows.Close()

	out := Mapping{Merges: map[uint32]uint32{}}
	found := false
	for rows.Next() {
		var (
			generation string
			version    uint64
			src, dst   sql.Null[uint32]
		)
		if err := rows.Scan(&generation, &version, &src, &dst); err != nil {
			return Mapping{}, fmt.Errorf("scan zone_merged_into row: %w", err)
		}
		out.Generation, out.Version, found = generation, version, true
		if src.Valid != dst.Valid {
			// 两列都是 NOT NULL;只有 LEFT JOIN 没匹配上时一起为 NULL。
			return Mapping{}, fmt.Errorf("read zone_merged_into: src_zone null=%t but dst_zone null=%t", !src.Valid, !dst.Valid)
		}
		if src.Valid {
			out.Merges[src.V] = dst.V
		}
	}
	if err := rows.Err(); err != nil {
		return Mapping{}, fmt.Errorf("iterate zone_merged_into: %w", err)
	}
	if !found {
		return Mapping{}, ErrNoRouteMeta
	}
	return out, nil
}

// Decide 判定第 0 片刚读到的 truth 要不要发布进副本(published、found 是在读 truth 之前读到的副本)。
// publish=true:副本缺席或比 truth 旧,按读副本时的 ModRevision 写 truth;回错误:两边对不上,谁对拿不准,什么都不写。
// key 只进错误文案。data_service 与 merge_zone 都按这一份判定(设计稿 §4.3 判定表的上半张)。
//
// 调用方必须先读副本、后读第 0 片:副本里只有第 0 片已经提交过的版本,这个顺序下「第 0 片落后于副本」只可能是第 0 片回档了,
// 不会是两次读之间夹了一次合服。
func Decide(key string, published Mapping, found bool, truth Mapping) (bool, error) {
	// 和缓存、编码同一套规则:坏映射不发布。
	if _, err := placement.NewMergedZones(truth.Merges); err != nil {
		return false, fmt.Errorf("zone_merged_into on directory shard 0 is broken at version %d: %w", truth.Version, err)
	}
	if err := valueOf(truth).validate(); err != nil {
		return false, fmt.Errorf("zone_merged_into on directory shard 0 cannot be published at version %d: %w", truth.Version, err)
	}
	switch {
	case !found:
		return true, nil
	case published.Generation != truth.Generation:
		return false, fmt.Errorf("the etcd copy %s belongs to directory generation %s, but directory shard 0 is generation %s: "+
			"this process points at another directory database, or the directory was recreated; "+
			"if it was recreated, stop every data_service replica, delete that etcd key and start them again",
			key, published.Generation, truth.Generation)
	case published.Version < truth.Version:
		return true, nil
	case published.Version > truth.Version:
		return false, fmt.Errorf("directory shard 0 is at version %d of the merged zone mapping, behind version %d in the etcd copy %s: "+
			"shard 0 was restored, its route tables were cleared, or it lost committed merges; "+
			"if the restore or the reset was intended, stop every data_service replica, delete that etcd key and start them again",
			truth.Version, published.Version, key)
	case !maps.Equal(published.Merges, truth.Merges):
		return false, fmt.Errorf("directory shard 0 and the etcd copy %s both hold version %d of the merged zone mapping with different contents: "+
			"zone_merged_into was changed without a zone_merge_history row (by hand rather than by merge_zone), or the copy was edited",
			key, truth.Version)
	default:
		return false, nil
	}
}

// value 是副本在 etcd 里的值。merges 按源区升序,同一份映射编出来的字节相同。
type value struct {
	Generation string `json:"generation"`
	Version    uint64 `json:"version"`
	Merges     []pair `json:"merges"`
}

type pair struct {
	Src uint32 `json:"src"`
	Dst uint32 `json:"dst"`
}

func valueOf(mapping Mapping) value {
	v := value{Generation: mapping.Generation, Version: mapping.Version, Merges: make([]pair, 0, len(mapping.Merges))}
	for _, src := range slices.Sorted(maps.Keys(mapping.Merges)) {
		v.Merges = append(v.Merges, pair{Src: src, Dst: mapping.Merges[src]})
	}
	return v
}

// validate 是副本的值自己的约束。zone_merged_into 没有「区号 ≥ 1」的 CHECK,区号 0 只可能是手写的,在这里拒绝。
func (v value) validate() error {
	if v.Generation == "" {
		return errors.New("the merged zone mapping has no directory generation")
	}
	seen := make(map[uint32]bool, len(v.Merges))
	for _, p := range v.Merges {
		if p.Src == 0 || p.Dst == 0 {
			return fmt.Errorf("the merged zone mapping maps zone %d to zone %d: zone 0 is not a zone", p.Src, p.Dst)
		}
		if seen[p.Src] {
			return fmt.Errorf("the merged zone mapping lists source zone %d twice", p.Src)
		}
		seen[p.Src] = true
	}
	return nil
}

// Encode 校验并编码副本的值。
func Encode(mapping Mapping) ([]byte, error) {
	v := valueOf(mapping)
	if err := v.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Decode 解码并校验副本的值。多余字段不拒绝(以后加字段时旧进程照常读),形状不对才报错。
func Decode(raw []byte) (Mapping, error) {
	var v value
	if err := json.Unmarshal(raw, &v); err != nil {
		return Mapping{}, fmt.Errorf("the merged zone copy is not valid JSON: %w", err)
	}
	if err := v.validate(); err != nil {
		return Mapping{}, err
	}
	mapping := Mapping{Generation: v.Generation, Version: v.Version, Merges: make(map[uint32]uint32, len(v.Merges))}
	for _, p := range v.Merges {
		mapping.Merges[p.Src] = p.Dst
	}
	return mapping, nil
}

// Copies 是副本的读写。单测换成内存替身。
type Copies interface {
	// Key 是副本键,只进日志与错误文案。
	Key() string
	// Get 读副本。found=false 表示还没有副本;revision 是键的 ModRevision,写时按它比较。
	Get(ctx context.Context) (published Mapping, revision int64, found bool, err error)
	// Put 只在键的 ModRevision 仍等于 revision 时写入(0 = 键必须不存在)。written=false:条件不成立,这次确定没写;
	// 其余错误的结局未知。
	Put(ctx context.Context, revision int64, mapping Mapping) (written bool, err error)
}

// KV 是 etcd 客户端里这里用到的两样,*clientv3.Client 满足它。
type KV interface {
	Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Txn(ctx context.Context) clientv3.Txn
}

// EtcdCopies 是副本在 etcd 里的读写。客户端的生命期归调用方。
type EtcdCopies struct {
	kv  KV
	key string
}

// NewEtcdCopies 用 etcd 客户端建副本的读写。shardZeroDB 见 Key。
func NewEtcdCopies(kv KV, shardZeroDB string) *EtcdCopies {
	return &EtcdCopies{kv: kv, key: Key(shardZeroDB)}
}

// Key 见 Copies。
func (e *EtcdCopies) Key() string { return e.key }

// Get 见 Copies。值解析不了按读失败报:副本只由 data_service 与 merge_zone 写,坏值说明被别人改过,谁都不能用它。
func (e *EtcdCopies) Get(ctx context.Context) (Mapping, int64, bool, error) {
	resp, err := e.kv.Get(ctx, e.key)
	if err != nil {
		return Mapping{}, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return Mapping{}, 0, false, nil
	}
	mapping, err := Decode(resp.Kvs[0].Value)
	if err != nil {
		return Mapping{}, 0, false, err
	}
	return mapping, resp.Kvs[0].ModRevision, true, nil
}

// Put 见 Copies。不存在的键 ModRevision 按 0 比较,所以 revision 为 0 就是「键必须不存在」
// (和 dbplacement.Store.CompareAndPut 同一写法)。
func (e *EtcdCopies) Put(ctx context.Context, revision int64, mapping Mapping) (bool, error) {
	raw, err := Encode(mapping)
	if err != nil {
		return false, err
	}
	resp, err := e.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(e.key), "=", revision)).
		Then(clientv3.OpPut(e.key, string(raw))).
		Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}
