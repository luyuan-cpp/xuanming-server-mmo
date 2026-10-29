// Package accountdirectory 是全局 SQL 账号目录的调用接缝，不保存认证凭据。
package accountdirectory

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/savebarrier"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const SecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN"
const MethodPrefix = "/data_service.DataService/"

var (
	ErrUnavailable = errors.New("全局账号目录不可用")
	ErrMissing     = errors.New("账号或角色目录不存在")
	ErrConflict    = errors.New("角色归属或身份冲突")
	ErrFull        = errors.New("账号角色数量已达全局上限")
	ErrInvalid     = errors.New("账号目录参数非法")
)

// Directory 的所有成功写应答均表示 SQL 已提交；未知结果必须强读原 player_id。
// Repair/Remove 的归属修订号必须来自 Lookup，不能用 scene 的 owner_epoch。
type Directory interface {
	Read(context.Context, string, bool) (*base.AccountSimplePlayerList, error)
	Create(context.Context, string, *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error)
	Lookup(context.Context, uint64) (*dspb.AccountRoleOwnership, error)
	Repair(context.Context, string, *base.AccountSimplePlayer, uint64) (*base.AccountSimplePlayerList, error)
	Remove(context.Context, string, []*dspb.AccountRoleVersion) error
}

// InitialCommitCreator 只在本次 SQL 严格新建成功时返回可缓存的持久初始原件。
// 旧 Directory 的调用面保留；不支持此能力的实现不获得种子优化，必须走 SQL 核验。
type InitialCommitCreator interface {
	CreateWithInitialCommit(context.Context, string, *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error)
}

func CreateWithInitialCommit(ctx context.Context, directory Directory, account string, role *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	if creator, ok := directory.(InitialCommitCreator); ok {
		return creator.CreateWithInitialCommit(ctx, account, role)
	}
	if directory == nil {
		return nil, ErrUnavailable
	}
	players, err := directory.Create(ctx, account, role)
	if err != nil {
		return nil, err
	}
	return &dspb.AccountDirectoryResponse{Players: players}, nil
}

func ValidAccount(account string) bool {
	return len(account) > 0 && len(account) <= 191 && utf8.ValidString(account)
}

type Client struct {
	rpc    dspb.DataServiceClient
	secret string
}

func NewClient(rpc dspb.DataServiceClient, secret string) *Client {
	return &Client{rpc: rpc, secret: secret}
}
func (c *Client) prepare(ctx context.Context, method string, in proto.Message) (context.Context, context.CancelFunc, error) {
	call, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	if c == nil || c.rpc == nil {
		cancel()
		return nil, func() {}, ErrUnavailable
	}
	if err := serviceauth.Sign(MethodPrefix+method, in, "login", c.secret, uint64(time.Now().UnixMilli())); err != nil {
		cancel()
		return nil, func() {}, ErrUnavailable
	}
	return call, cancel, nil
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		return ErrMissing
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		return ErrConflict
	case codes.ResourceExhausted:
		return ErrFull
	case codes.InvalidArgument:
		return ErrInvalid
	default:
		return errors.Join(ErrUnavailable, err)
	}
}
func (c *Client) Read(ctx context.Context, account string, create bool) (*base.AccountSimplePlayerList, error) {
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
	return out.Players, nil
}
func (c *Client) Create(ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	out, err := c.CreateWithInitialCommit(ctx, account, role)
	if err != nil {
		return nil, err
	}
	return out.Players, nil
}

func (c *Client) CreateWithInitialCommit(ctx context.Context, account string, role *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	in := &dspb.CreateAccountRoleRequest{Account: account, Role: role}
	call, cancel, err := c.prepare(ctx, "CreateAccountRole", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.CreateAccountRole(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Players == nil {
		return nil, ErrUnavailable
	}
	if out.InitialPlayerData != nil && (!savebarrier.IsInitialPlayerData(out.InitialPlayerData) ||
		out.InitialPlayerData.GetPlayerDatabaseData().GetPlayerId() != role.GetPlayerId()) {
		return nil, ErrUnavailable
	}
	return out, nil
}
func (c *Client) Lookup(ctx context.Context, player uint64) (*dspb.AccountRoleOwnership, error) {
	in := &dspb.LookupAccountRoleRequest{PlayerId: player}
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
func (c *Client) Repair(ctx context.Context, account string, role *base.AccountSimplePlayer, rev uint64) (*base.AccountSimplePlayerList, error) {
	in := &dspb.RepairAccountRoleRequest{Account: account, Role: role, ExpectedOwnerRevision: rev}
	call, cancel, err := c.prepare(ctx, "RepairAccountRole", in)
	if err != nil {
		return nil, err
	}
	defer cancel()
	out, err := c.rpc.RepairAccountRole(call, in)
	if err != nil {
		return nil, classify(err)
	}
	if out == nil || out.Players == nil {
		return nil, ErrUnavailable
	}
	return out.Players, nil
}
func (c *Client) Remove(ctx context.Context, account string, roles []*dspb.AccountRoleVersion) error {
	in := &dspb.RemoveAccountRolesRequest{Account: account, Roles: roles}
	call, cancel, err := c.prepare(ctx, "RemoveAccountRoles", in)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.rpc.RemoveAccountRoles(call, in)
	return classify(err)
}
