package accountdirectory

import (
	"context"

	dspb "proto/data_service"
)

// Deletions 是账号注销(docs/design/account-deletion.md §4)在目录上的接缝,只有 login 用,*Client 实现它。
// 与 Directory 分开:不让 Directory 的所有替身都跟着多实现这几个方法。
//
// 四个动作的结论在应答的 outcome 里(带内),error 只表示这一次调用没拿到结论(依赖故障、结局未知的写),同参数重放即可:
// 申请重复回原状态,撤销重复回 0,认领按条件改,收尾按条件删。
type Deletions interface {
	// ReadWithLifecycle 同 Directory.Read,另回账号行上的注销状态与「以前注销完成的时刻」。
	ReadWithLifecycle(ctx context.Context, account string, create bool) (*dspb.AccountDirectoryResponse, error)
	// LookupWithAccountState 同 Directory.Lookup,另回所属账号的注销状态(EnterGame 的入口闸门)。
	LookupWithAccountState(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error)
	RequestDeletion(ctx context.Context, account string, source uint32) (*dspb.AccountDeletionResponse, error)
	CancelDeletion(ctx context.Context, account string) (*dspb.AccountDeletionResponse, error)
	ClaimDeletions(ctx context.Context, limit uint32) ([]string, error)
	FinishDeletion(ctx context.Context, account string) (*dspb.FinishAccountDeletionResponse, error)
}

var _ Deletions = (*Client)(nil)

func (c *Client) ReadWithLifecycle(ctx context.Context, account string, create bool) (*dspb.AccountDirectoryResponse, error) {
	in := &dspb.ReadAccountDirectoryRequest{Account: account, CreateIfMissing: create}
	call, cancel, err := c.prepare(ctx, "ReadAccountDirectory", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.ReadAccountDirectory(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Players == nil {
		return nil, ErrUnavailable
	}
	if out.Lifecycle == nil {
		// data_service 没装配注销(冷静期配置非法)时不带状态:那边也就没有注销中的账号。
		out.Lifecycle = &dspb.AccountLifecycle{}
	}
	return out, nil
}

func (c *Client) LookupWithAccountState(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	in := &dspb.LookupAccountRoleRequest{PlayerId: player, IncludeAccountState: true}
	call, cancel, err := c.prepare(ctx, "LookupAccountRole", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.LookupAccountRole(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Role == nil || out.Role.PlayerId != player || !ValidAccount(out.Account) || out.OwnerRevision == 0 {
		return nil, ErrUnavailable
	}
	return out, nil
}

func (c *Client) RequestDeletion(ctx context.Context, account string, source uint32) (*dspb.AccountDeletionResponse, error) {
	in := &dspb.RequestAccountDeletionRequest{Account: account, Source: source}
	call, cancel, err := c.prepare(ctx, "RequestAccountDeletion", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.RequestAccountDeletion(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Outcome == dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_UNSPECIFIED {
		return nil, ErrUnavailable
	}
	return out, nil
}

func (c *Client) CancelDeletion(ctx context.Context, account string) (*dspb.AccountDeletionResponse, error) {
	in := &dspb.CancelAccountDeletionRequest{Account: account}
	call, cancel, err := c.prepare(ctx, "CancelAccountDeletion", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.CancelAccountDeletion(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Outcome == dspb.AccountDeletionOutcome_ACCOUNT_DELETION_OUTCOME_UNSPECIFIED {
		return nil, ErrUnavailable
	}
	return out, nil
}

func (c *Client) ClaimDeletions(ctx context.Context, limit uint32) ([]string, error) {
	in := &dspb.ClaimAccountDeletionsRequest{Limit: limit}
	call, cancel, err := c.prepare(ctx, "ClaimAccountDeletions", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.ClaimAccountDeletions(call, in)
	if err != nil {
		return nil, classify(err)
	}
	for _, account := range out.GetAccounts() {
		if !ValidAccount(account) {
			return nil, ErrUnavailable
		}
	}
	return out.GetAccounts(), nil
}

func (c *Client) FinishDeletion(ctx context.Context, account string) (*dspb.FinishAccountDeletionResponse, error) {
	in := &dspb.FinishAccountDeletionRequest{Account: account}
	call, cancel, err := c.prepare(ctx, "FinishAccountDeletion", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.FinishAccountDeletion(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil {
		return nil, ErrUnavailable
	}
	return out, nil
}
