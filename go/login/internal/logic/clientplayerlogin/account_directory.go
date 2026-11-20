package clientplayerloginlogic

import (
	"context"
	"login/internal/svc"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
)

// 两次强读限制旧异步链，但不声称闭合最后重读到原生入场提交之间的竞态；
// 冻结完成仍必须由 SQL verifier 验证原生卸载与入场排空证据。
func (l *EnterGameLogic) recheckAccountRoleEntry(ctx context.Context, state enterGameSessionState) error {
	if l.svcCtx.AccountDirectory == nil {
		return accountdirectory.ErrUnavailable
	}
	owned, err := l.svcCtx.AccountDirectory.Lookup(ctx, state.playerID)
	if err != nil {
		return err
	}
	if owned.Account != state.account || state.ownerRevision == 0 || owned.OwnerRevision != state.ownerRevision || owned.TradeListingId != 0 {
		return accountdirectory.ErrConflict
	}
	return nil
}

// lookupEnteringRole 是 EnterGame 的第一次归属强读。注销装配了就顺带读所属账号的注销状态(docs/design/account-deletion.md §4.3);
// 没装配时(单测替身、data_service 没开注销)照旧只读归属,状态为 0。
func (l *EnterGameLogic) lookupEnteringRole(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	if l.svcCtx.AccountDeletions != nil {
		return l.svcCtx.AccountDeletions.LookupWithAccountState(ctx, player)
	}
	return l.svcCtx.AccountDirectory.Lookup(ctx, player)
}

// readAccountLifecycle 同 readAccountDirectory,另回账号的注销状态(登录应答要带给客户端)。没装配注销时状态为空(0)。
func readAccountLifecycle(ctx context.Context, sc *svc.ServiceContext, account string, create bool) (*dbpb.UserAccounts, *dspb.AccountLifecycle, error) {
	if sc.AccountDeletions == nil {
		accounts, err := readAccountDirectory(ctx, sc, account, create)
		return accounts, &dspb.AccountLifecycle{}, err
	}
	out, err := sc.AccountDeletions.ReadWithLifecycle(ctx, account, create)
	if err != nil {
		return nil, nil, err
	}
	return &dbpb.UserAccounts{Account: account, SimplePlayers: out.Players}, out.Lifecycle, nil
}

func readAccountDirectory(ctx context.Context, sc *svc.ServiceContext, account string, create bool) (*dbpb.UserAccounts, error) {
	if sc.AccountDirectory == nil {
		return nil, accountdirectory.ErrUnavailable
	}
	roles, err := sc.AccountDirectory.Read(ctx, account, create)
	if err != nil {
		return nil, err
	}
	return &dbpb.UserAccounts{Account: account, SimplePlayers: roles}, nil
}
