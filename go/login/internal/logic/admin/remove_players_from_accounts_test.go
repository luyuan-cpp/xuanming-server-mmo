package admin

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"login/internal/svc"
	base "proto/common/base"
	dspb "proto/data_service"
	loginpb "proto/login"
	"shared/accountdirectory"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// SQL 目录替身只模拟已承诺的 Remove 接缝;不把它当成真 SQL 锁或提交证据。
type adminDirectoryFake struct {
	roles        map[uint64]*dspb.AccountRoleOwnership
	lookupErrors map[uint64]error
	removeErrors map[uint64]error
	lookups      []uint64
	removes      map[string][]*dspb.AccountRoleVersion
	beforeRemove func(string, []*dspb.AccountRoleVersion)
}

var _ accountdirectory.Directory = (*adminDirectoryFake)(nil)

func (f *adminDirectoryFake) Read(context.Context, string, bool) (*base.AccountSimplePlayerList, error) {
	panic("孤儿清理不应按账号整块读回再覆盖目录")
}
func (f *adminDirectoryFake) Create(context.Context, string, *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	panic("孤儿清理不应创建角色")
}
func (f *adminDirectoryFake) Repair(context.Context, string, *base.AccountSimplePlayer, uint64) (*base.AccountSimplePlayerList, error) {
	panic("孤儿清理不应自愈或回填角色")
}
func (f *adminDirectoryFake) Lookup(_ context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	f.lookups = append(f.lookups, player)
	if err := f.lookupErrors[player]; err != nil {
		return nil, err
	}
	role := f.roles[player]
	if role == nil {
		return nil, accountdirectory.ErrMissing
	}
	return proto.Clone(role).(*dspb.AccountRoleOwnership), nil
}
func (f *adminDirectoryFake) Remove(_ context.Context, account string, versions []*dspb.AccountRoleVersion) error {
	if f.removes == nil {
		f.removes = make(map[string][]*dspb.AccountRoleVersion)
	}
	for _, version := range versions {
		f.removes[account] = append(f.removes[account], proto.Clone(version).(*dspb.AccountRoleVersion))
	}
	if f.beforeRemove != nil {
		f.beforeRemove(account, versions)
	}
	for _, version := range versions {
		if err := f.removeErrors[version.PlayerId]; err != nil {
			return err
		}
		if role := f.roles[version.PlayerId]; role != nil && (role.Account != account || role.OwnerRevision != version.OwnerRevision) {
			return accountdirectory.ErrConflict
		}
		delete(f.roles, version.PlayerId)
	}
	return nil
}

// storeProbeFake 只实现孤儿判定用到的两个方法:有写权行或路由缺失都不算孤儿。
type storeProbeFake struct {
	dspb.DataServiceClient
	found     map[uint64]bool
	errs      map[uint64]error
	routeErrs map[uint64]error
	probes    []uint64
	deadline  time.Time
}

func (f *storeProbeFake) GetPlayerHomeZone(_ context.Context, in *dspb.GetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.GetPlayerHomeZoneResponse, error) {
	if err := f.routeErrs[in.GetPlayerId()]; err != nil {
		return nil, err
	}
	return &dspb.GetPlayerHomeZoneResponse{HomeZoneId: 1}, nil
}

func (f *storeProbeFake) GetPlayerAssetOpLedger(ctx context.Context, in *dspb.GetPlayerAssetOpLedgerRequest, _ ...grpc.CallOption) (*dspb.GetPlayerAssetOpLedgerResponse, error) {
	f.probes = append(f.probes, in.GetPlayerId())
	f.deadline, _ = ctx.Deadline()
	if err := f.errs[in.GetPlayerId()]; err != nil {
		return nil, err
	}
	return &dspb.GetPlayerAssetOpLedgerResponse{Found: f.found[in.GetPlayerId()]}, nil
}

func adminOwned(account string, player, revision uint64, appearance string) *dspb.AccountRoleOwnership {
	return &dspb.AccountRoleOwnership{
		Account: account, OwnerRevision: revision,
		Role: &base.AccountSimplePlayer{PlayerId: player, AppearanceId: appearance},
	}
}

func adminRequest(players ...uint64) *RemovePlayersFromAccountsReq {
	return &RemovePlayersFromAccountsReq{PlayerIDs: players, Operator: "tester", Reason: "工单 123"}
}

func outcomesOf(out *RemovePlayersFromAccountsResp) map[uint64]loginpb.RemovePlayerOutcome {
	outcomes := make(map[uint64]loginpb.RemovePlayerOutcome, len(out.Results))
	for _, r := range out.Results {
		outcomes[r.PlayerID] = r.Outcome
	}
	return outcomes
}

func TestRemovePlayersFromAccounts_RemovesOrphanKeepsOthersAndReplays(t *testing.T) {
	kept := adminOwned("admin-retention", 2, 11, "06_thunder_caster_boy")
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{
		1: adminOwned("admin-retention", 1, 7, "04_mountain_guardian_boy"),
		2: proto.Clone(kept).(*dspb.AccountRoleOwnership),
	}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1, 1))
	if err != nil || out.RemovedCount != 1 || out.NotFoundCount != 0 || out.FailedCount != 0 || len(out.Results) != 1 {
		t.Fatalf("首次清理计数错误: out=%+v err=%v", out, err)
	}
	if !reflect.DeepEqual(directory.lookups, []uint64{1}) || directory.roles[1] != nil || !proto.Equal(directory.roles[2], kept) {
		t.Fatalf("去重或剩余角色身份错误: lookups=%v roles=%v", directory.lookups, directory.roles)
	}
	versions := directory.removes["admin-retention"]
	if len(versions) != 1 || versions[0].PlayerId != 1 || versions[0].OwnerRevision != 7 {
		t.Fatalf("提交必须带 Lookup 返回的原归属修订: %v", versions)
	}
	out, err = RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1))
	if err != nil || out.RemovedCount != 0 || out.NotFoundCount != 1 || out.FailedCount != 0 || len(directory.removes["admin-retention"]) != 1 {
		t.Fatalf("已移除角色重放不能复活或再次提交: out=%+v err=%v removes=%v", out, err, directory.removes)
	}
}

// 墓碑不可逆:有存档的角色不是孤儿,输错 id 不能把在役角色删掉。
func TestRemovePlayersFromAccounts_RoleWithStoreRowIsNotOrphan(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲")}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{found: map[uint64]bool{1: true}}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1))
	if err != nil || out.NotOrphanCount != 1 || out.RemovedCount != 0 {
		t.Fatalf("有存档不能摘: out=%+v err=%v", out, err)
	}
	if directory.roles[1] == nil || len(directory.removes) != 0 {
		t.Fatalf("有存档的角色不能进删角流程: roles=%v removes=%v", directory.roles, directory.removes)
	}
}

// 路由缺失时账本读同样回 found=false:路由没了说明不了存档也没了,不能写不可逆的墓碑。
func TestRemovePlayersFromAccounts_MissingRouteIsNotProofOfOrphan(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{
		1: adminOwned("acct", 1, 3, "甲"),
		2: adminOwned("acct", 2, 4, "乙"),
	}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{routeErrs: map[uint64]error{
		1: status.Error(codes.NotFound, "no home zone mapping"),
		2: status.Error(codes.Unavailable, "route store down"),
	}}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1, 2))
	if err != nil {
		t.Fatal(err)
	}
	outcomes := outcomesOf(out)
	if outcomes[1] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_ORPHAN || outcomes[2] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY {
		t.Fatalf("路由缺失不算孤儿,路由读失败要重试: %v", outcomes)
	}
	if len(directory.removes) != 0 {
		t.Fatalf("证明不了就不能删: %v", directory.removes)
	}
}

// 存档读不到不能当成没有存档:这是最容易把在役角色删掉的一条路。
func TestRemovePlayersFromAccounts_StoreCheckFailureIsRetryNotOrphan(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲")}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{errs: map[uint64]error{1: errors.New("读失败")}}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1))
	if err != nil || out.FailedCount != 1 || outcomesOf(out)[1] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY {
		t.Fatalf("读失败要回 RETRY: out=%+v err=%v", out, err)
	}
	if len(directory.removes) != 0 {
		t.Fatalf("读失败不能删: %v", directory.removes)
	}
}

func TestRemovePlayersFromAccounts_DryRunTouchesNothing(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲"), 2: adminOwned("acct", 2, 4, "乙")}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{found: map[uint64]bool{2: true}}}
	req := adminRequest(1, 2, 3)
	req.DryRun = true
	out, err := RemovePlayersFromAccounts(context.Background(), sc, req)
	if err != nil || out.RemovedCount != 1 || out.NotOrphanCount != 1 || out.NotFoundCount != 1 {
		t.Fatalf("预览计数错误: out=%+v err=%v", out, err)
	}
	if len(directory.removes) != 0 || directory.roles[1] == nil {
		t.Fatalf("预览不能写: removes=%v roles=%v", directory.removes, directory.roles)
	}
}

// 一名一名走:目录的删角是逐名两阶段,一名失败不影响别的,计数和结局都对得上。
func TestRemovePlayersFromAccounts_CountsEachOutcome(t *testing.T) {
	directory := &adminDirectoryFake{
		roles: map[uint64]*dspb.AccountRoleOwnership{
			1: adminOwned("acct", 1, 3, "甲"),
			2: adminOwned("acct", 2, 4, "乙"),
			5: adminOwned("acct", 5, 8, "丙"),
			6: adminOwned("acct", 6, 9, "丁"),
		},
		lookupErrors: map[uint64]error{9: errors.New("归属查询依赖失败")},
		removeErrors: map[uint64]error{
			2: accountdirectory.ErrConflict,
			5: accountdirectory.ErrRetryLater,
			6: accountdirectory.ErrMissing,
		},
	}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1, 2, 5, 6, 9, 10, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]loginpb.RemovePlayerOutcome{
		1:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_REMOVED,
		2:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_CONFLICT,
		5:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY,
		6:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND,
		9:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY,
		10: loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND,
		0:  loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND,
	}
	if got := outcomesOf(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("各结局错误: got=%v want=%v", got, want)
	}
	if out.RemovedCount != 1 || out.FailedCount != 3 || out.NotFoundCount != 3 {
		t.Fatalf("计数错误: %+v", out)
	}
	if directory.roles[1] != nil || directory.roles[2] == nil || directory.roles[5] == nil {
		t.Fatalf("失败的不能删,成功的要删: %v", directory.roles)
	}
}

func TestRemovePlayersFromAccounts_OwnerChangeAfterLookupIsConflict(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("seller-account", 1, 4, "甲")}}
	directory.beforeRemove = func(account string, versions []*dspb.AccountRoleVersion) {
		if account != "seller-account" || len(versions) != 1 || versions[0].OwnerRevision != 4 {
			t.Fatalf("没有携带原 SQL 归属修订: account=%q versions=%v", account, versions)
		}
		// 确定地插入 Lookup 之后、SQL 提交复核之前的过户,不依赖线程调度或时间等待。
		directory.roles[1] = adminOwned("buyer-account", 1, 5, "甲")
	}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	out, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1))
	if err != nil || outcomesOf(out)[1] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_CONFLICT {
		t.Fatalf("归属变化不能算清理成功: out=%+v err=%v", out, err)
	}
	if directory.roles[1].Account != "buyer-account" || !reflect.DeepEqual(directory.lookups, []uint64{1}) {
		t.Fatalf("不得删除新归属,也不能按最新归属偷偷重试: roles=%v lookups=%v", directory.roles, directory.lookups)
	}
}

// 预算用完时没做的回 RETRY,不是失败也不是成功。重跑幂等。
func TestRemovePlayersFromAccounts_CancelledContextAnswersRetry(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲")}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := RemovePlayersFromAccounts(ctx, sc, adminRequest(1))
	if err != nil || outcomesOf(out)[1] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY || len(directory.lookups) != 0 {
		t.Fatalf("预算用完应回 RETRY: out=%+v err=%v lookups=%v", out, err, directory.lookups)
	}
}

// 剩余预算不够一轮删角就不开始:拖到 RPC 超时的话,已经摘掉的名单也回不到调用方。
func TestRemovePlayersFromAccounts_ShortDeadlineStartsNothing(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲")}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	ctx, cancel := context.WithTimeout(context.Background(), removeRoleBudget-time.Second)
	defer cancel()
	out, err := RemovePlayersFromAccounts(ctx, sc, adminRequest(1))
	if err != nil || outcomesOf(out)[1] != loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY || len(directory.lookups) != 0 {
		t.Fatalf("预算不够应回 RETRY 且不开始: out=%+v err=%v lookups=%v", out, err, directory.lookups)
	}
}

// 一名角色的下游调用都限在预算内,响应才赶得上 RPC 超时。
func TestRemovePlayersFromAccounts_RoleWorkIsCappedByBudget(t *testing.T) {
	directory := &adminDirectoryFake{roles: map[uint64]*dspb.AccountRoleOwnership{1: adminOwned("acct", 1, 3, "甲")}}
	probe := &storeProbeFake{found: map[uint64]bool{1: true}}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: probe}
	if _, err := RemovePlayersFromAccounts(context.Background(), sc, adminRequest(1)); err != nil {
		t.Fatal(err)
	}
	if probe.deadline.IsZero() || time.Until(probe.deadline) > removeRoleBudget {
		t.Fatalf("下游调用要带不超过 %v 的截止时间: %v", removeRoleBudget, probe.deadline)
	}
}

func TestRemovePlayersFromAccounts_RequiresReadyDirectoryAndValidRequest(t *testing.T) {
	out, err := RemovePlayersFromAccounts(context.Background(), &svc.ServiceContext{}, adminRequest(1))
	if out == nil || !errors.Is(err, accountdirectory.ErrUnavailable) {
		t.Fatalf("目录未就绪必须明确失败: out=%+v err=%v", out, err)
	}
	directory := &adminDirectoryFake{}
	sc := &svc.ServiceContext{AccountDirectory: directory, DataServiceClient: &storeProbeFake{}}
	invalid := map[string]*RemovePlayersFromAccountsReq{
		"空请求":   nil,
		"超上限":   {PlayerIDs: make([]uint64, MaxRemovePlayers+1), Operator: "tester", Reason: "工单"},
		"没有操作人": {PlayerIDs: []uint64{1}, Reason: "工单"},
		"没有理由":  {PlayerIDs: []uint64{1}, Operator: "tester"},
	}
	for name, req := range invalid {
		out, err = RemovePlayersFromAccounts(context.Background(), sc, req)
		if out == nil || !errors.Is(err, accountdirectory.ErrInvalid) || len(directory.lookups) != 0 || len(directory.removes) != 0 {
			t.Fatalf("%s: 非法请求不得访问目录: out=%+v err=%v lookups=%v removes=%v", name, out, err, directory.lookups, directory.removes)
		}
	}
	out, err = RemovePlayersFromAccounts(context.Background(), sc, adminRequest())
	if err != nil || out == nil || len(out.Results) != 0 || len(directory.lookups) != 0 {
		t.Fatalf("空名单应无副作用: out=%+v err=%v", out, err)
	}
}
