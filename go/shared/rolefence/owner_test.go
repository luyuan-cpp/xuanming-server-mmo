package rolefence

import (
	"context"
	"errors"
	"fmt"
	"testing"

	rfpb "proto/role_fence"
)

// 归属预检(CheckOwner / Guard.VerifyOwner)只给只读入口用,见 docs/design/gate-online-identity.md §9.3。

func TestCheckOwner(t *testing.T) {
	owner := Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, Version: 9, HomeZone: 1}
	frozen := owner
	frozen.FreezeToken, frozen.FreezeFinal = 101, true
	preparing := owner
	preparing.FreezeToken = 101
	session := Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, Reject: errDenied}
	boom := errors.New("sql failed")
	for _, tc := range []struct {
		name    string
		fence   Fence
		readErr error
		expect  Expect
		want    error // nil 放行;errStale 该自愈;其余原样
	}{
		{name: "原身份相符", fence: owner, expect: session},
		{name: "最终冻结仍归原身份,不拦读", fence: frozen, expect: session},
		{name: "投票期间仍归原身份,不拦读", fence: preparing, expect: session},
		{name: "缺行要自愈", fence: Fence{PlayerID: 11}, readErr: ErrMissing, expect: session, want: errStale},
		{name: "SQL 失败原样返回", readErr: boom, expect: session, want: boom},
		{name: "旧主的会话直接拒绝,不自愈", fence: Fence{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Version: 10, HomeZone: 1}, expect: session, want: errDenied},
		{name: "新主的会话副本落后,自愈", fence: owner, expect: Expect{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Reject: errDenied}, want: errStale},
		{name: "修订号相等账号不等,拒绝", fence: owner, expect: Expect{PlayerID: 11, Account: "other", OwnerRevision: 5, Reject: errDenied}, want: errDenied},
		{name: "删角墓碑修订号更大,拒绝", fence: Fence{PlayerID: 11, OwnerRevision: 6, Version: 10, HomeZone: 1}, expect: session, want: errDenied},
		{name: "删角墓碑修订号相同,账号已清空,拒绝", fence: Fence{PlayerID: 11, OwnerRevision: 5, Version: 10, HomeZone: 1}, expect: session, want: errDenied},
		{name: "没有原修订号不是原身份", fence: owner, expect: Expect{PlayerID: 11, Account: "seller", Reject: errDenied}, want: errDenied},
		{name: "没有原账号不是原身份", fence: Fence{PlayerID: 11, OwnerRevision: 5, Version: 10, HomeZone: 1}, expect: Expect{PlayerID: 11, OwnerRevision: 5, Reject: errDenied}, want: errDenied},
		{name: "没有角色不是原身份", fence: owner, expect: Expect{Account: "seller", OwnerRevision: 5, Reject: errDenied}, want: errDenied},
		{name: "没填 Reject 也不能放行", fence: owner, expect: Expect{PlayerID: 11}, want: ErrRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOwner(tc.fence, tc.readErr, tc.expect)
			var stale *StaleError
			if errors.As(err, &stale) {
				err = errStale
				if len(stale.Players) != 1 || stale.Players[0] != 11 {
					t.Fatalf("players %v", stale.Players)
				}
			}
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func ownerState(account string, revision, token uint64, final bool, version uint64) *rfpb.RoleFenceState {
	return &rfpb.RoleFenceState{PlayerId: 11, Account: account, OwnerRevision: revision, FreezeToken: token, FreezeFinal: final, Version: version, HomeZone: 1}
}

func sellerSession() Expect {
	return Expect{PlayerID: 11, Account: "seller", OwnerRevision: 5, Reject: errDenied}
}

// 稳态:副本相符,一次不加锁的读就放行,不开事务、不读目录。
func TestVerifyOwnerPassesOnMatchingReplicaWithOneUnlockedRead(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, Version: 9, HomeZone: 1}
	reader := &fakeStateReader{}
	if err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.reads) != "[11]" || len(f.locked) != 0 || f.commits != 0 || f.rollbacks != 0 || f.applies != 0 {
		t.Fatalf("只应有一次不加锁的读: reads %v locked %v commits %d rollbacks %d applies %d", f.reads, f.locked, f.commits, f.rollbacks, f.applies)
	}
	if len(reader.calls) != 0 {
		t.Fatalf("副本相符不读目录: %v", reader.calls)
	}
}

// 过户之后旧主的会话:副本修订号已经更大,直接拒绝,不读目录。
func TestVerifyOwnerRejectsPreviousOwnerWithoutRPC(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Version: 10, HomeZone: 1}
	reader := &fakeStateReader{}
	err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession())
	if !errors.Is(err, errDenied) {
		t.Fatalf("err %v", err)
	}
	if len(reader.calls) != 0 || fmt.Sprint(f.reads) != "[11]" {
		t.Fatalf("确定拒绝不读目录: calls %v reads %v", reader.calls, f.reads)
	}
}

func TestVerifyOwnerHealsMissingReplica(t *testing.T) {
	f, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("seller", 5, 0, false, 9)}}
	if err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(reader.calls) != "[11]" || fmt.Sprint(f.reads) != "[11 11]" || f.rows[11].Version != 9 {
		t.Fatalf("缺行读一次目录、写副本、再读一次: calls %v reads %v row %+v", reader.calls, f.reads, f.rows[11])
	}
}

// 过户之后新主的会话:副本还停在旧主,自愈之后通过。
func TestVerifyOwnerHealsLaggingReplicaForNewOwner(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, Version: 9, HomeZone: 1}
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("buyer", 6, 0, false, 11)}}
	err := NewGuard(db, reader).VerifyOwner(context.Background(), Expect{PlayerID: 11, Account: "buyer", OwnerRevision: 6, Reject: errDenied})
	if err != nil {
		t.Fatal(err)
	}
	if f.rows[11].Account != "buyer" || f.rows[11].OwnerRevision != 6 {
		t.Fatalf("副本没按权威状态更新: %+v", f.rows[11])
	}
}

// 这是不看冻结之后唯一要守住的洞:过户已完成、过户完成的推送丢了,本库副本还停在「旧主 + 冻结」。
// 只比修订号和账号会把旧主的会话放过去,所以冻结的副本必须先读权威状态再判。
func TestVerifyOwnerFrozenReplicaBehindTransferRejectsPreviousOwner(t *testing.T) {
	for _, final := range []bool{true, false} {
		f, db := newFakeFenceDB(t)
		f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, FreezeToken: 101, FreezeFinal: final, Version: 9, HomeZone: 1}
		reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("buyer", 6, 0, false, 11)}}
		err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession())
		if !errors.Is(err, errDenied) {
			t.Fatalf("final=%v: 旧主的会话必须被拒: %v", final, err)
		}
		if fmt.Sprint(reader.calls) != "[11]" || f.rows[11].Account != "buyer" {
			t.Fatalf("final=%v: 冻结的副本要先自愈: calls %v row %+v", final, reader.calls, f.rows[11])
		}
	}
}

// 真冻结(公示、投票、删角待确认)而角色仍归原身份:读一次目录确认之后放行,冻结本身不拦读。
func TestVerifyOwnerFrozenButStillOwnedPasses(t *testing.T) {
	for _, final := range []bool{true, false} {
		f, db := newFakeFenceDB(t)
		f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, FreezeToken: 101, FreezeFinal: final, Version: 9, HomeZone: 1}
		reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("seller", 5, 101, final, 9)}}
		if err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession()); err != nil {
			t.Fatalf("final=%v: %v", final, err)
		}
		if fmt.Sprint(reader.calls) != "[11]" || fmt.Sprint(f.reads) != "[11 11]" {
			t.Fatalf("final=%v: 冻结的副本读一次目录再判一次: calls %v reads %v", final, reader.calls, f.reads)
		}
	}
}

// 缺行自愈之后副本是冻结的:它刚按权威状态写过,不能再当成「可能落后」要求第二次自愈,否则会被 RunTx 当成真实拒绝。
func TestVerifyOwnerMissingReplicaHealedIntoFrozenStillPasses(t *testing.T) {
	f, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("seller", 5, 101, true, 9)}}
	if err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(reader.calls) != "[11]" || f.rows[11].FreezeToken != 101 {
		t.Fatalf("calls %v row %+v", reader.calls, f.rows[11])
	}
}

func TestVerifyOwnerDeletedRoleIsRejected(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, FreezeToken: RemovalToken(11), FreezeFinal: true, Version: 9, HomeZone: 1}
	// 删角完成:目录里是账号清空的墓碑。
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("", 6, 0, false, 11)}}
	if err := NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession()); !errors.Is(err, errDenied) {
		t.Fatalf("err %v", err)
	}
}

func TestVerifyOwnerRoleMissingFromDirectoryIsRoleNotFound(t *testing.T) {
	_, db := newFakeFenceDB(t)
	err := NewGuard(db, &fakeStateReader{}).VerifyOwner(context.Background(), sellerSession())
	if !errors.Is(err, ErrRoleNotFound) || errors.Is(err, errDenied) {
		t.Fatalf("目录里没有这名角色交给调用方判: %v", err)
	}
}

// 会话声称的修订号比目录还大:自愈之后副本仍然落后,第二次按拒绝处理。gate 签名之下不会出现,这里只守住不放行。
func TestVerifyOwnerSessionAheadOfDirectoryIsRejected(t *testing.T) {
	_, db := newFakeFenceDB(t)
	reader := &fakeStateReader{states: map[uint64]*rfpb.RoleFenceState{11: ownerState("seller", 5, 0, false, 9)}}
	err := NewGuard(db, reader).VerifyOwner(context.Background(), Expect{PlayerID: 11, Account: "seller", OwnerRevision: 9, Reject: errDenied})
	if !errors.Is(err, errDenied) {
		t.Fatalf("err %v", err)
	}
	if _, stale := err.(*StaleError); stale {
		t.Fatal("StaleError 不能漏给调用方")
	}
}

// 这一次判不了(目录读不到、库故障)不是拒绝:调用方要让客户端重试,不能告诉它角色变了。
func TestVerifyOwnerInconclusiveIsNotARejection(t *testing.T) {
	_, db := newFakeFenceDB(t)
	err := NewGuard(db, &fakeStateReader{err: errors.Join(ErrUnavailable, errors.New("data_service down"))}).VerifyOwner(context.Background(), sellerSession())
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, errDenied) || errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("目录读不到: %v", err)
	}

	f, db := newFakeFenceDB(t)
	boom := errors.New("sql failed")
	f.lockErr = boom
	reader := &fakeStateReader{}
	err = NewGuard(db, reader).VerifyOwner(context.Background(), sellerSession())
	if !errors.Is(err, boom) || errors.Is(err, errDenied) || len(reader.calls) != 0 {
		t.Fatalf("库故障原样返回、不读目录: err %v calls %v", err, reader.calls)
	}
}

func TestVerifyOwnerRejectsIncompleteOriginalWithoutReading(t *testing.T) {
	f, db := newFakeFenceDB(t)
	f.rows[11] = Fence{PlayerID: 11, Account: "seller", OwnerRevision: 5, Version: 9, HomeZone: 1}
	reader := &fakeStateReader{}
	guard := NewGuard(db, reader)
	for _, expect := range []Expect{
		{PlayerID: 11, Reject: errDenied},
		{PlayerID: 11, Account: "seller", Reject: errDenied},
		{PlayerID: 11, OwnerRevision: 5, Reject: errDenied},
		{Account: "seller", OwnerRevision: 5, Reject: errDenied},
	} {
		if err := guard.VerifyOwner(context.Background(), expect); !errors.Is(err, errDenied) {
			t.Fatalf("%+v: %v", expect, err)
		}
	}
	if len(f.reads) != 0 || len(reader.calls) != 0 {
		t.Fatalf("不完整的原身份不读库: reads %v calls %v", f.reads, reader.calls)
	}
}

func TestVerifyOwnerUnconfiguredGuardIsUnavailable(t *testing.T) {
	_, db := newFakeFenceDB(t)
	for name, guard := range map[string]*Guard{"nil": nil, "no reader": NewGuard(db, nil), "no db": NewGuard(nil, &fakeStateReader{})} {
		if err := guard.VerifyOwner(context.Background(), sellerSession()); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
