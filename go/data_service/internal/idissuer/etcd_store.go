package idissuer

import (
	"context"
	"fmt"
	"strconv"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdStore 把一份预留记录存在 etcd 的一个键里。
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
	value, err := strconv.ParseUint(string(kv.Value), 10, 64)
	if err != nil {
		return Record{}, fmt.Errorf("id issuer: record %s is corrupt: %w", s.key, err)
	}
	return Record{Value: value, Revision: kv.ModRevision, Exists: true, ClusterID: clusterID}, nil
}

// CompareAndSwap 用 Txn 比较版本。超时等错误一律是结果未知,见 CounterStore 的契约。
func (s *EtcdStore) CompareAndSwap(ctx context.Context, prev Record, next uint64) (bool, error) {
	var cmp clientv3.Cmp
	if prev.Exists {
		cmp = clientv3.Compare(clientv3.ModRevision(s.key), "=", prev.Revision)
	} else {
		cmp = clientv3.Compare(clientv3.CreateRevision(s.key), "=", 0)
	}
	resp, err := s.cli.Txn(ctx).If(cmp).Then(clientv3.OpPut(s.key, strconv.FormatUint(next, 10))).Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// Close 关闭 etcd 连接。
func (s *EtcdStore) Close() error {
	return s.cli.Close()
}
