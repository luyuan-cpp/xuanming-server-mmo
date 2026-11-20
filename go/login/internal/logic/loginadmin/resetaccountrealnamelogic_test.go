package loginadminlogic

import (
	"context"
	"testing"

	"login/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	"proto/login"
	"shared/accountdirectory"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// resetDirectory 是账号目录的替身:一个账号 wx_a 带着实名、名下一个角色 7。
type resetDirectory struct {
	accountdirectory.Directory
	pi     map[string]string
	resets []string
}

func (d *resetDirectory) Lookup(_ context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	if player != 7 {
		return nil, accountdirectory.ErrMissing
	}
	return &dspb.AccountRoleOwnership{Account: "wx_a", Role: &base.AccountSimplePlayer{PlayerId: 7}, OwnerRevision: 1}, nil
}

func (d *resetDirectory) ReadWithLifecycle(context.Context, string, bool) (*dspb.AccountDirectoryResponse, error) {
	return &dspb.AccountDirectoryResponse{}, nil
}

func (d *resetDirectory) SetRealName(context.Context, string, string) (string, error) { return "", nil }

func (d *resetDirectory) ResetRealName(_ context.Context, account string) (bool, error) {
	if _, ok := d.pi[account]; !ok {
		return false, accountdirectory.ErrMissing
	}
	d.resets = append(d.resets, account)
	had := d.pi[account] != ""
	d.pi[account] = ""
	return had, nil
}

func newResetLogic(t *testing.T) (*ResetAccountRealNameLogic, *resetDirectory, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := &resetDirectory{pi: map[string]string{"wx_a": "1hpfml09b57f3f8185f8cb5094ea3f26278efb"}}
	return NewResetAccountRealNameLogic(context.Background(), &svc.ServiceContext{AccountDirectory: dir, LoginRedis: rdb}), dir, mr
}

func TestResetAccountRealNameByPlayerClearsPiAndPending(t *testing.T) {
	logic, dir, mr := newResetLogic(t)
	_ = mr.Set(redisslot.RealNamePendingKey("wx_a"), "ai-1")
	out, err := logic.ResetAccountRealName(&login.ResetAccountRealNameRequest{PlayerId: 7, Operator: "cs-1", Reason: "wrong id"})
	if err != nil || !out.GetHadRealName() || len(dir.resets) != 1 || dir.pi["wx_a"] != "" {
		t.Fatalf("reset by player: out=%v err=%v resets=%v", out, err, dir.resets)
	}
	if mr.Exists(redisslot.RealNamePendingKey("wx_a")) {
		t.Fatal("the pending submission must be dropped with the reset")
	}
	// 重放:本来就没有实名。
	out, err = logic.ResetAccountRealName(&login.ResetAccountRealNameRequest{Account: "wx_a", Operator: "cs-1", Reason: "wrong id"})
	if err != nil || out.GetHadRealName() {
		t.Fatalf("replay by account: out=%v err=%v", out, err)
	}
}

func TestResetAccountRealNameReportsUnknownTargets(t *testing.T) {
	logic, _, _ := newResetLogic(t)
	for _, in := range []*login.ResetAccountRealNameRequest{
		{PlayerId: 8, Operator: "cs-1", Reason: "r"},
		{Account: "wx_missing", Operator: "cs-1", Reason: "r"},
	} {
		out, err := logic.ResetAccountRealName(in)
		if err != nil || out.GetErrorMessage().GetId() != uint32(table.LoginError_kLoginAccountNotFound) {
			t.Fatalf("%v: out=%v err=%v; want kLoginAccountNotFound", in, out, err)
		}
	}
}

func TestResetAccountRealNameValidatesTheRequest(t *testing.T) {
	logic, dir, _ := newResetLogic(t)
	for _, in := range []*login.ResetAccountRealNameRequest{
		{Operator: "cs-1", Reason: "r"},                               // 两个都没给
		{PlayerId: 7, Account: "wx_a", Operator: "cs-1", Reason: "r"}, // 两个都给了
		{PlayerId: 7, Reason: "r"},                                    // 没有操作人
		{PlayerId: 7, Operator: "cs-1"},                               // 没有理由
	} {
		if _, err := logic.ResetAccountRealName(in); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%v: err=%v; want InvalidArgument", in, err)
		}
	}
	if len(dir.resets) != 0 {
		t.Fatalf("invalid requests must not touch the directory: %v", dir.resets)
	}
}
