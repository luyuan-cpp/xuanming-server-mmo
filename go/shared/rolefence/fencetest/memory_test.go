package fencetest

import (
	"context"
	"errors"
	"testing"

	rfpb "proto/role_fence"
	"shared/rolefence"
)

// MemoryReplica 要认得 rolefence 真正发出的语句:rolefence 改了读写副本的 SQL 而这里没跟上时,这些用例转红,
// 而不是让用它的服务单测悄悄拿到「unexpected statement」。

func TestMemoryReplicaSpeaksTheReplicaStatementsOfRolefence(t *testing.T) {
	ctx := context.Background()
	replica := NewMemoryReplica(t)

	if _, err := rolefence.Read(ctx, replica.DB(), 11); !errors.Is(err, rolefence.ErrMissing) {
		t.Fatalf("没有这一行应当回 ErrMissing: %v", err)
	}
	state := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 5, FreezeToken: rolefence.RemovalToken(11), FreezeFinal: true, Version: 9, HomeZone: 3}
	if applied, err := rolefence.Apply(ctx, replica.DB(), state); err != nil || !applied {
		t.Fatalf("写副本: applied %v err %v", applied, err)
	}
	fence, err := rolefence.Read(ctx, replica.DB(), 11)
	if err != nil {
		t.Fatal(err)
	}
	want := rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, FreezeToken: rolefence.RemovalToken(11), FreezeFinal: true, Version: 9, HomeZone: 3}
	if fence != want {
		t.Fatalf("读回的副本 %+v,期望 %+v", fence, want)
	}

	// 版本大的赢:旧版本、同版本都不覆盖。
	older := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 5, Version: 8, HomeZone: 3}
	if applied, err := rolefence.Apply(ctx, replica.DB(), older); err != nil || applied {
		t.Fatalf("旧版本不该覆盖: applied %v err %v", applied, err)
	}
	newer := &rfpb.RoleFenceState{PlayerId: 11, Account: "buyer", OwnerRevision: 6, Version: 10, HomeZone: 3}
	if applied, err := rolefence.Apply(ctx, replica.DB(), newer); err != nil || !applied {
		t.Fatalf("新版本应当覆盖: applied %v err %v", applied, err)
	}
	if row, ok := replica.Row(11); !ok || row.Account != "buyer" || row.OwnerRevision != 6 || row.FreezeToken != 0 {
		t.Fatalf("覆盖之后的行 %+v", row)
	}
	if got := replica.Reads(); len(got) != 2 || got[0] != 11 || got[1] != 11 {
		t.Fatalf("两次读都记了下来: %v", got)
	}
}

func TestMemoryReplicaPassesInjectedFailuresThrough(t *testing.T) {
	ctx := context.Background()
	replica := NewMemoryReplica(t)
	replica.Put(rolefence.Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5})
	boom := errors.New("sql failed")
	replica.FailWith(boom)

	if _, err := rolefence.Read(ctx, replica.DB(), 11); !errors.Is(err, boom) {
		t.Fatalf("读应当回注入的错误: %v", err)
	}
	state := &rfpb.RoleFenceState{PlayerId: 11, Account: "seller", OwnerRevision: 5, Version: 2, HomeZone: 1}
	if _, err := rolefence.Apply(ctx, replica.DB(), state); !errors.Is(err, boom) {
		t.Fatalf("写应当回注入的错误: %v", err)
	}
	replica.FailWith(nil)
	if fence, err := rolefence.Read(ctx, replica.DB(), 11); err != nil || fence.Account != "seller" {
		t.Fatalf("恢复之后照常读: %+v %v", fence, err)
	}
}

// 替身只服务不加锁的预检路径:事务与 FOR UPDATE 一律报错,免得有人拿它测提交点的加锁复核而得到假的通过。
func TestMemoryReplicaRefusesTransactions(t *testing.T) {
	replica := NewMemoryReplica(t)
	if _, err := replica.DB().BeginTx(context.Background(), nil); err == nil {
		t.Fatal("内存副本不支持事务")
	}
}

// 与目录替身配在一起就是一个完整的 Guard:副本缺行时读一次目录、写回副本,之后只读本库。
func TestMemoryReplicaDrivesGuardVerifyOwner(t *testing.T) {
	ctx := context.Background()
	replica := NewMemoryReplica(t)
	directory := NewDirectory()
	directory.Set(11, "seller", 5, 0)
	guard := rolefence.NewGuard(replica.DB(), directory)
	expect := rolefence.Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5}

	for round := 0; round < 2; round++ {
		if err := guard.VerifyOwner(ctx, expect); err != nil {
			t.Fatalf("第 %d 次预检: %v", round+1, err)
		}
	}
	if len(directory.Reads) != 1 {
		t.Fatalf("副本缺行只读一次目录,之后只读本库: %v", directory.Reads)
	}
	if row, ok := replica.Row(11); !ok || row.Account != "seller" {
		t.Fatalf("自愈之后副本里应当有这一行: %+v", row)
	}
}
