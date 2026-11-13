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

// 声明指针(-id-registry-rebuild 声明重建、-id-registry-open 直接开启)和并发的重新登记抢在一起:扫描之后、提交之前插进来
// 一条在用码的记录,提交点必须连同范围一起复核。重建要求指针高于全部已有码;直接开启要求一条记录都没有(有就是丢过数据)。
// 没有指针时补墓碑一律拒绝,所以并发写者只有重新登记一种。
func TestEtcdIntegration_DeclaringThePointerRechecksConcurrentRegionEntry(t *testing.T) {
	endpoints := itEndpoints(t, "MMORPG_ID_ISSUER_IT_REGISTRY")
	rebuild := func(ctx context.Context, r *EtcdRegistry) (bool, error) { return r.BeginRebuild(ctx, 9) }
	open := func(ctx context.Context, r *EtcdRegistry) (bool, error) { return r.Open(ctx, 9) }
	for _, tc := range []struct {
		name       string
		region     uint8
		declare    func(context.Context, *EtcdRegistry) (bool, error)
		rebuilding bool
		refusal    error // nil = 声明成功
	}{
		{"重建:同码恢复", 9, rebuild, true, ErrRegionClaimed},
		{"重建:更大码恢复", 10, rebuild, true, ErrRegionClaimed},
		{"重建:更小码恢复", 8, rebuild, true, nil},
		{"开启:更小码恢复", 8, open, false, ErrRegistryNeedsRebuild},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := newEtcdRegistryAt(endpoints, itRoot(t)+"registry/", nil)
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
				claim := Claim{State: ClaimActive, Authority: testAuthority, PrimaryCluster: 1, SecondaryCluster: 2,
					HighWater: uint64(tc.region)<<regionShift | 1, Blocks: 1}
				ok, err := r.CompareAndSwap(ctx, tc.region, ClaimRecord{}, claim)
				if err != nil || !ok {
					t.Fatalf("并发写者未提交: ok=%v err=%v", ok, err)
				}
			}}
			declared, err := tc.declare(ctx, r)
			if tc.refusal != nil {
				if declared || !errors.Is(err, tc.refusal) {
					t.Fatalf("并发新增记录后必须拒绝声明: declared=%v err=%v, want %v", declared, err, tc.refusal)
				}
			} else if !declared || err != nil {
				t.Fatalf("所有已用码仍小于指针时应能声明: declared=%v err=%v", declared, err)
			}
			p, err := r.readPointer(ctx)
			if err != nil || p.declared == (tc.refusal != nil) || (p.declared && (p.next != 9 || p.rebuilding != tc.rebuilding)) {
				t.Fatalf("声明结果与持久状态不符: pointer=%+v err=%v", p, err)
			}
		})
	}
}
