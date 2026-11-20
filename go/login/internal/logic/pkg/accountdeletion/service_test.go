package accountdeletion

import (
	"context"
	"errors"
	"testing"

	"login/internal/logic/pkg/auth"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/generated/pb/table"
)

// 本文件钉住 login 一侧的编排(docs/design/account-deletion.md §4.2、§4.4、§4.5),目录、会话、Apple 都是替身:
//   - Apple 授权码不对在改任何状态之前回「需要重新授权」;提交成功之后才撤销 Apple、吊销令牌、踢线;
//   - 预检没过、没找到账号不碰登录态;撤销晚了回 2040;毫秒换成秒;
//   - 执行者:角色全删完才收尾,有一名挂起就整账号等下一轮,已是墓碑、已过户的角色算删完。

const testAccount = "apple_001234.abcdef"

type fakeDirectory struct {
	requestOut  *dspb.AccountDeletionResponse
	requestErr  error
	cancelOut   *dspb.AccountDeletionResponse
	claimed     []string
	roles       []uint64
	state       dspb.AccountDeletionState
	readErr     error
	owners      map[uint64]*dspb.AccountRoleOwnership
	removeErr   map[uint64]error
	finishOut   *dspb.FinishAccountDeletionResponse
	requests    int
	removed     []uint64
	finishCalls int
}

func (f *fakeDirectory) Lookup(_ context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	if o, ok := f.owners[player]; ok {
		return o, nil
	}
	return nil, accountdirectory.ErrMissing
}

func (f *fakeDirectory) Remove(_ context.Context, account string, roles []*dspb.AccountRoleVersion) error {
	if err := f.removeErr[roles[0].PlayerId]; err != nil {
		return err
	}
	f.removed = append(f.removed, roles[0].PlayerId)
	return nil
}

func (f *fakeDirectory) ReadWithLifecycle(context.Context, string, bool) (*dspb.AccountDirectoryResponse, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	list := &base.AccountSimplePlayerList{}
	for _, id := range f.roles {
		list.Players = append(list.Players, &base.AccountSimplePlayer{PlayerId: id})
	}
	return &dspb.AccountDirectoryResponse{Players: list, Lifecycle: &dspb.AccountLifecycle{DeletionState: f.state}}, nil
}

func (f *fakeDirectory) RequestDeletion(context.Context, string, uint32) (*dspb.AccountDeletionResponse, error) {
	f.requests++
	return f.requestOut, f.requestErr
}

func (f *fakeDirectory) CancelDeletion(context.Context, string) (*dspb.AccountDeletionResponse, error) {
	return f.cancelOut, nil
}

func (f *fakeDirectory) ClaimDeletions(context.Context, uint32) ([]string, error) { return f.claimed, nil }

func (f *fakeDirectory) FinishDeletion(context.Context, string) (*dspb.FinishAccountDeletionResponse, error) {
	f.finishCalls++
	return f.finishOut, nil
}

type fakeSessions struct {
	revoked []string
	kicked  []uint64
}

func (f *fakeSessions) RevokeAll(_ context.Context, account string) error {
	f.revoked = append(f.revoked, account)
	return nil
}

func (f *fakeSessions) KickPlayer(_ context.Context, player uint64) error {
	f.kicked = append(f.kicked, player)
	return nil
}

type fakeApple struct {
	exchangeErr error
	revokeErr   error
	revokes     int
}

func (f *fakeApple) ExchangeCode(context.Context, string) (auth.AppleGrant, error) {
	return auth.AppleGrant{}, f.exchangeErr
}

func (f *fakeApple) Revoke(context.Context, auth.AppleGrant) error {
	f.revokes++
	return f.revokeErr
}

func pending(executeAfterMs uint64) *dspb.AccountDeletionResponse {
	return &dspb.AccountDeletionResponse{Outcome: dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_OK,
		Lifecycle: &dspb.AccountLifecycle{DeletionState: dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING, DeletionExecuteAfterMs: executeAfterMs}}
}

func tipOf(t *testing.T, got uint32, want table.LoginError) {
	t.Helper()
	if got != uint32(want) {
		t.Fatalf("tip = %d, want %d", got, uint32(want))
	}
}

func TestRequestAppleCodeInvalidChangesNothing(t *testing.T) {
	dir := &fakeDirectory{requestOut: pending(1)}
	sessions := &fakeSessions{}
	s := &Service{Dir: dir, Sessions: sessions, Apple: &fakeApple{exchangeErr: auth.ErrAppleCodeInvalid}}
	resp := s.Request(context.Background(), testAccount, SourceUser, true, "bad")
	tipOf(t, resp.GetErrorMessage().GetId(), table.LoginError_kLoginAccountDeletionReauthRequired)
	if dir.requests != 0 || len(sessions.revoked) != 0 {
		t.Fatal("授权码不对时不应碰目录和登录态")
	}

	s.Apple = &fakeApple{exchangeErr: auth.ErrProviderUnavailable}
	resp = s.Request(context.Background(), testAccount, SourceUser, true, "c")
	tipOf(t, resp.GetErrorMessage().GetId(), table.LoginError_kLoginAuthProviderUnavailable)
}

func TestRequestScheduledSignsOutAndRevokesApple(t *testing.T) {
	dir := &fakeDirectory{requestOut: pending(1_800_000_000_123), roles: []uint64{11, 12}}
	sessions := &fakeSessions{}
	apple := &fakeApple{revokeErr: errors.New("apple down")}
	s := &Service{Dir: dir, Sessions: sessions, Apple: apple}
	resp := s.Request(context.Background(), testAccount, SourceUser, true, "code")
	if resp.GetErrorMessage() != nil || resp.GetState() != 1 || resp.GetExecuteAfter() != 1_800_000_000 {
		t.Fatalf("应答不对: %+v", resp)
	}
	if apple.revokes != 1 {
		t.Fatal("提交之后应撤销 Apple 授权(失败也不影响结局)")
	}
	if len(sessions.revoked) != 1 || len(sessions.kicked) != 2 {
		t.Fatalf("应吊销令牌并踢掉两名角色: revoked=%v kicked=%v", sessions.revoked, sessions.kicked)
	}
}

func TestRequestBlockedAndNotFoundLeaveSessionsAlone(t *testing.T) {
	blocked := &dspb.AccountDeletionResponse{Outcome: dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_BLOCKED, Lifecycle: &dspb.AccountLifecycle{},
		Blockers: []*dspb.AccountDeletionBlocker{{PlayerId: 11, Reason: "guild_leader"}}}
	for name, tc := range map[string]struct {
		out  *dspb.AccountDeletionResponse
		want table.LoginError
	}{
		"预检没过":  {blocked, table.LoginError_kLoginAccountDeletionBlocked},
		"没有账号":  {&dspb.AccountDeletionResponse{Outcome: dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_NOT_FOUND}, table.LoginError_kLoginAccountNotFound},
		"这次没判成": {&dspb.AccountDeletionResponse{Outcome: dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_RETRY}, table.LoginError_kLoginDataSerializeFailed},
	} {
		sessions := &fakeSessions{}
		apple := &fakeApple{}
		s := &Service{Dir: &fakeDirectory{requestOut: tc.out}, Sessions: sessions, Apple: apple}
		resp := s.Request(context.Background(), testAccount, SourceUser, true, "code")
		tipOf(t, resp.GetErrorMessage().GetId(), tc.want)
		if len(sessions.revoked) != 0 || apple.revokes != 0 {
			t.Fatalf("%s: 没提交就不应吊销令牌或撤销 Apple", name)
		}
		if name == "预检没过" && (len(resp.GetBlockers()) != 1 || resp.GetBlockers()[0].GetReason() != "guild_leader") {
			t.Fatalf("挂起清单没带回: %+v", resp.GetBlockers())
		}
	}
}

func TestCancelTooLate(t *testing.T) {
	out := &dspb.AccountDeletionResponse{Outcome: dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_TOO_LATE,
		Lifecycle: &dspb.AccountLifecycle{DeletionState: dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_DELETING}}
	s := &Service{Dir: &fakeDirectory{cancelOut: out}, Sessions: &fakeSessions{}}
	resp := s.Cancel(context.Background(), testAccount)
	tipOf(t, resp.GetErrorMessage().GetId(), table.LoginError_kLoginAccountDeletionInProgress)
	if resp.GetState() != 2 {
		t.Fatalf("state = %d", resp.GetState())
	}
}

func owner(account string) *dspb.AccountRoleOwnership {
	return &dspb.AccountRoleOwnership{Account: account, OwnerRevision: 3}
}

func TestExecutorRemovesAllRolesThenFinishes(t *testing.T) {
	dir := &fakeDirectory{claimed: []string{testAccount}, roles: []uint64{11, 12, 13},
		state:     dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_DELETING,
		owners:    map[uint64]*dspb.AccountRoleOwnership{11: owner(testAccount), 13: owner("google_other")}, // 12 已是墓碑,13 已过户
		finishOut: &dspb.FinishAccountDeletionResponse{Done: true}}
	sessions := &fakeSessions{}
	s := &Service{Dir: dir, Sessions: sessions}
	got, err := s.RunOnce(context.Background())
	if err != nil || got.Finished != 1 {
		t.Fatalf("RunOnce = %+v, %v", got, err)
	}
	if len(dir.removed) != 1 || dir.removed[0] != 11 || dir.finishCalls != 1 {
		t.Fatalf("只该删 11 再收尾: removed=%v finish=%d", dir.removed, dir.finishCalls)
	}
	if len(sessions.revoked) != 1 || len(sessions.kicked) != 3 {
		t.Fatalf("执行时应吊销令牌、逐名踢线: %v %v", sessions.revoked, sessions.kicked)
	}
}

func TestExecutorWaitsWhileARoleIsBlocked(t *testing.T) {
	dir := &fakeDirectory{claimed: []string{testAccount}, roles: []uint64{11, 12},
		state:     dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_DELETING,
		owners:    map[uint64]*dspb.AccountRoleOwnership{11: owner(testAccount), 12: owner(testAccount)},
		removeErr: map[uint64]error{12: accountdirectory.ErrConflict},
		finishOut: &dspb.FinishAccountDeletionResponse{Done: true}}
	s := &Service{Dir: dir, Sessions: &fakeSessions{}}
	got, _ := s.RunOnce(context.Background())
	if got.Waiting != 1 || dir.finishCalls != 0 {
		t.Fatalf("有一名角色挂起时不该收尾: %+v finish=%d", got, dir.finishCalls)
	}

	// ref 还没清完(在途建角、标了保留)也是等。
	dir.removeErr = nil
	dir.finishOut = &dspb.FinishAccountDeletionResponse{Done: false, RemainingRefs: 1}
	if got, _ = s.RunOnce(context.Background()); got.Waiting != 1 {
		t.Fatalf("ref 没清完应等下一轮: %+v", got)
	}
}

func TestExecutorAccountAlreadyGoneOrNotDeleting(t *testing.T) {
	s := &Service{Dir: &fakeDirectory{claimed: []string{testAccount}, readErr: accountdirectory.ErrMissing}, Sessions: &fakeSessions{}}
	if got, _ := s.RunOnce(context.Background()); got.Finished != 1 {
		t.Fatalf("行已不在算完成: %+v", got)
	}
	dir := &fakeDirectory{claimed: []string{testAccount}, roles: []uint64{11}, state: dspb.AccountDeletionState_ACCOUNT_DELETION_STATE_PENDING,
		owners: map[uint64]*dspb.AccountRoleOwnership{11: owner(testAccount)}}
	s = &Service{Dir: dir, Sessions: &fakeSessions{}}
	if got, _ := s.RunOnce(context.Background()); got.Failed != 1 || len(dir.removed) != 0 {
		t.Fatalf("不是执行中就不删: %+v removed=%v", got, dir.removed)
	}
}

func TestMaskAccount(t *testing.T) {
	if got := MaskAccount("apple_001234.abcdef"); got != "appl***ef" {
		t.Fatalf("MaskAccount = %q", got)
	}
	if got := MaskAccount("short"); got != "***" {
		t.Fatalf("MaskAccount(short) = %q", got)
	}
}
