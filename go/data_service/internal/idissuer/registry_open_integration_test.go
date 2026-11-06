//go:build etcd_integration

package idissuer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// scanInterleaveKV 在真实范围读取返回后插入另一个写者，固定开启登记处的并发窗口。
// 事务仍由真实 etcd 执行，避免内存替身把范围校验与提交放进同一把锁而掩盖竞态。
type scanInterleaveKV struct {
	clientv3.KV
	prefix string
	once   sync.Once
	after  func()
}

func (kv *scanInterleaveKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	resp, err := kv.KV.Get(ctx, key, opts...)
	if err == nil && key == kv.prefix {
		kv.once.Do(kv.after)
	}
	return resp, err
}

func TestEtcdIntegration_OpenRechecksConcurrentRegionEntry(t *testing.T) {
	endpoints := itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY")
	for _, tc := range []struct {
		name   string
		region uint8
		state  string
		refuse bool
	}{
		{"同码恢复", 9, ClaimActive, true},
		{"更大码恢复", 10, ClaimActive, true},
		{"同码退役", 9, ClaimRetired, true},
		{"更小码恢复", 8, ClaimActive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := newEtcdRegistryAt(endpoints, itRoot(t)+"registry/")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				if _, err := r.cli.Delete(cleanupCtx, r.root, clientv3.WithPrefix()); err != nil {
					t.Errorf("清理隔离登记处: %v", err)
				}
			}()
			r.cli.KV = &scanInterleaveKV{KV: r.cli.KV, prefix: r.root + "region/", after: func() {
				claim := Claim{State: tc.state, Authority: testAuthority, PrimaryCluster: 1, SecondaryCluster: 2,
					HighWater: uint64(tc.region)<<regionShift | 1, Blocks: 1}
				ok, err := r.CompareAndSwap(ctx, tc.region, ClaimRecord{}, claim)
				if err != nil || !ok {
					t.Fatalf("并发写者未提交: ok=%v err=%v", ok, err)
				}
			}}
			opened, err := r.Open(ctx, 9)
			if tc.refuse {
				if opened || !errors.Is(err, ErrRegionClaimed) {
					t.Fatalf("并发新增已用码后必须拒绝开启: opened=%v err=%v", opened, err)
				}
			} else if !opened || err != nil {
				t.Fatalf("所有已用码仍小于起点时应能开启: opened=%v err=%v", opened, err)
			}
			next, _, exists, err := r.readNext(ctx)
			if err != nil || exists == tc.refuse || (exists && next != 9) {
				t.Fatalf("开启结果与持久状态不符: next=%d exists=%v err=%v", next, exists, err)
			}
		})
	}
}
