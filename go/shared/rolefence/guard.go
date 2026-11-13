package rolefence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/serviceauth"
)

// 密钥都走环境变量,口令不进配置文件。每个调用方一把,互不复用。
const (
	// CoordinatorCaller 是 data_service 调参与者时的 ServiceCallAuth.caller。
	CoordinatorCaller = "coordinator"
	// CoordinatorSecretEnv 是协调者的密钥,data_service 和全部参与者共用这一把。
	CoordinatorSecretEnv = "MMORPG_ROLE_FENCE_SECRET_COORDINATOR"

	secretEnvPrefix = "MMORPG_ROLE_FENCE_SECRET_"
	// stateReadTimeout 是读一次权威状态的上限。
	stateReadTimeout = 2 * time.Second
)

// Participants 是全部参与者。协调者要它们全部通过才算冻结成功,少配一个就是漏冻结,所以配置必须逐个对上。
var Participants = []string{"guild", "trade", "mail", "match", "player_inbox", "friend"}

// Callers 是能读权威状态的调用方。chat 不写资产,不是参与者,只读。
var Callers = []string{"guild", "trade", "mail", "match", "player_inbox", "friend", "chat"}

// SecretEnv 是调用方读权威状态用的密钥环境变量。
func SecretEnv(caller string) string { return secretEnvPrefix + strings.ToUpper(caller) }

// StateReader 读目录里的权威状态。目录里没有这名角色时回 ErrRoleNotFound,其余失败回 ErrUnavailable。
type StateReader interface {
	GetRoleFenceState(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error)
}

type directoryReader struct {
	rpc    dspb.DataServiceClient
	caller string
	secret string
}

// NewDirectoryReader 经 data_service 读权威状态。caller 是 Callers 里的一个,secret 是 SecretEnv(caller) 的值。
func NewDirectoryReader(rpc dspb.DataServiceClient, caller, secret string) (StateReader, error) {
	return newDirectoryClient(rpc, caller, secret)
}

// AdmissionVerifier 问目录这份入场登记是否仍然有效(规格 §3.7)。账号目录只有 data_service 连,服务不再跨库读登记。
// 读不到回 ErrUnavailable。调用方在事务外调用,事务里不调 RPC。
type AdmissionVerifier interface {
	VerifyRoleAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) (bool, error)
}

// NewAdmissionVerifier 经 data_service 校验入场登记。调用方和密钥与 NewDirectoryReader 相同。
func NewAdmissionVerifier(rpc dspb.DataServiceClient, caller, secret string) (AdmissionVerifier, error) {
	return newDirectoryClient(rpc, caller, secret)
}

func newDirectoryClient(rpc dspb.DataServiceClient, caller, secret string) (*directoryReader, error) {
	if rpc == nil {
		return nil, errors.New("角色护栏缺 data_service 客户端")
	}
	if _, err := serviceauth.New(map[string]string{caller: secret}); err != nil {
		return nil, err
	}
	return &directoryReader{rpc: rpc, caller: caller, secret: secret}, nil
}

func (r *directoryReader) GetRoleFenceState(ctx context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	in := &dspb.GetRoleFenceStateRequest{PlayerId: player}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"GetRoleFenceState", in, r.caller, r.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	call, cancel := context.WithTimeout(ctx, stateReadTimeout)
	defer cancel()
	out, err := r.rpc.GetRoleFenceState(call, in)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if out.GetRetry() {
		return nil, ErrUnavailable
	}
	if !out.GetFound() {
		return nil, ErrRoleNotFound
	}
	state := out.GetState()
	if state.GetPlayerId() != player || !ValidState(state) {
		return nil, ErrUnavailable
	}
	return state, nil
}

func (r *directoryReader) VerifyRoleAdmission(ctx context.Context, admission *base.RoleAdmissionIdentity) (bool, error) {
	in := &dspb.VerifyRoleAdmissionRequest{Admission: admission}
	if err := serviceauth.Sign(accountdirectory.MethodPrefix+"VerifyRoleAdmission", in, r.caller, r.secret, uint64(time.Now().UnixMilli())); err != nil {
		return false, errors.Join(ErrUnavailable, err)
	}
	call, cancel := context.WithTimeout(ctx, stateReadTimeout)
	defer cancel()
	out, err := r.rpc.VerifyRoleAdmission(call, in)
	if err != nil {
		return false, errors.Join(ErrUnavailable, err)
	}
	if out.GetRetry() {
		return false, ErrUnavailable
	}
	return out.GetValid(), nil
}

// RoleNotFoundError 是 Heal 在目录里没找到的角色。其余角色已经写好副本。
// 好友的对端不存在按原逻辑跳过,其余调用方拒绝。
type RoleNotFoundError struct {
	Players []uint64
}

func (e *RoleNotFoundError) Error() string {
	return fmt.Sprintf("目录里没有这些角色: %v", e.Players)
}

// Is 让 errors.Is(err, ErrRoleNotFound) 成立。
func (e *RoleNotFoundError) Is(target error) bool { return target == ErrRoleNotFound }

// Guard 把副本自愈和服务的事务接起来。db 是副本所在逻辑库的连接池。
type Guard struct {
	db     *sql.DB
	reader StateReader
}

// NewGuard 装配 Guard。
func NewGuard(db *sql.DB, reader StateReader) *Guard {
	return &Guard{db: db, reader: reader}
}

// Heal 读权威状态并写入副本,版本大的赢。目录没有的角色不写副本,最后回 *RoleNotFoundError。
func (g *Guard) Heal(ctx context.Context, players ...uint64) error {
	if g == nil || g.db == nil || g.reader == nil {
		return ErrUnavailable
	}
	var missing []uint64
	for _, player := range players {
		state, err := g.reader.GetRoleFenceState(ctx, player)
		if errors.Is(err, ErrRoleNotFound) {
			missing = append(missing, player)
			continue
		}
		if err != nil {
			return err
		}
		if state.GetPlayerId() != player {
			return ErrUnavailable
		}
		if _, err = Apply(ctx, g.db, state); err != nil {
			return err
		}
	}
	if len(missing) != 0 {
		return &RoleNotFoundError{Players: missing}
	}
	return nil
}

// RunTx 跑一次 action,action 回 *StaleError 就自愈再跑一次。action 必须自己开事务、自己回滚,回 *StaleError 时事务已经结束。
//
// 第二次还不过就是真实拒绝,回 Expect.Reject。自愈失败时回自愈的错误:ErrUnavailable 可重试,*RoleNotFoundError 交给调用方。
//
// 自愈读不到目录时事务已回滚,这一笔确定没写。调用方应当映射成非故障的「稍后重试」(规格 §3.6);没映射、照旧回故障的,
// 由 serverbase 的拦截器凭这里置的观察位改回可重试的状态,不打排空 Hold。match 有几十个调用点,靠的是这一条。
func (g *Guard) RunTx(ctx context.Context, action func() error) error {
	err := action()
	var stale *StaleError
	if !errors.As(err, &stale) {
		return err
	}
	if err = g.Heal(ctx, stale.Players...); err != nil {
		if errors.Is(err, ErrUnavailable) {
			authoritysql.NoteNoWriteRetry(ctx)
		}
		return err
	}
	err = action()
	if errors.As(err, &stale) {
		return stale.rejection()
	}
	return err
}

// VerifyOwner 是只读入口的归属预检:不加锁读本库副本,判角色此刻是否仍归 expect 的原身份。只读,不替代提交点的 Lock + Check。
//
// 回 nil 是仍归它;回 expect.Reject 是确定不归(已换主、已删);回 *RoleNotFoundError 是目录里没有这名角色;
// 其余错误(ErrUnavailable、SQL 错误)是这一次判不了,调用方按可重试处理,不能当成归属变了。
//
// 副本缺行、修订号落后或处于冻结时读一次权威状态再判。冻结的副本可能停在过户之前(过户完成的推送丢了),
// 不先自愈就会把旧主的会话放过去;没冻结的副本不会落后于目录,因为过户、删角之前全部参与者都先冻结了。
// 冻结本身不算拒绝,见 CheckOwner。
func (g *Guard) VerifyOwner(ctx context.Context, expect Expect) error {
	if g == nil || g.db == nil || g.reader == nil {
		return ErrUnavailable
	}
	if !expect.hasOriginal() {
		return expect.rejection()
	}
	refreshed := false // RunTx 只在自愈成功之后才跑第二次,第二次读到的就是刚按权威状态写过的副本
	return g.RunTx(ctx, func() error {
		fence, err := Read(ctx, g.db, expect.PlayerID)
		verdict := CheckOwner(fence, err, expect)
		if verdict == nil && fence.FreezeToken != 0 && !refreshed {
			verdict = expect.stale()
		}
		refreshed = true
		return verdict
	})
}
