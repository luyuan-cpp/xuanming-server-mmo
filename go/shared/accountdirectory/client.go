// Package accountdirectory 是全局 SQL 账号目录的调用接缝，不保存认证凭据。
package accountdirectory

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	base "proto/common/base"
	dspb "proto/data_service"
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
	// ErrNotCommitted 表示建角走到提交点之前就失败了,这次调用确定没有写角色行。login 对一个 player_id 只发一次建角,
	// 所以收到它就能确定角色没建成,可以释放名字。ErrUnavailable 不同:那是结局未知,提交可能已经成了。
	// 规格 docs/design/horizontal-scale-logical-db.md §5.3。
	ErrNotCommitted = errors.New("账号角色未提交")
	// ErrRetryLater 表示这次调用的结局已知,但还没做完,用同样的参数重放。投票问不齐、过户已提交但收尾没做完、前面的角色已删后面的被拒都走它。
	// 这次调用可能什么都没写,也可能已经提交过东西,所以它不等于「没提交」:退款、释放名字这类不可逆的判定不能凭它下结论,要走各自的串行点。
	// 和结局未知的 ErrUnavailable 不同,这次调用没有结局未知的写,RPC 回 Aborted,排空证明按确定算。错误链上有 ErrUnavailable:只认识后者的调用方照旧按「重试」处理。
	// 规格 docs/design/horizontal-scale-logical-db.md §3.6。
	ErrRetryLater = fmt.Errorf("%w: 结局已知,同参数重放", ErrUnavailable)
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

// classifyCreate 只给建角用:只有建角的 Aborted 是「确定没提交」,login 收到它释放名字。别的方法回 Aborted 不能这样解读。
func classifyCreate(err error) error {
	if status.Code(err) == codes.Aborted {
		return ErrNotCommitted
	}
	return classify(err)
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		return ErrMissing
	case codes.AlreadyExists, codes.FailedPrecondition:
		return ErrConflict
	case codes.Aborted:
		return ErrRetryLater
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
		return nil, classifyCreate(err)
	}
	if out == nil || out.Players == nil {
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
