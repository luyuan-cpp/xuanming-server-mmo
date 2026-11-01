package data

// 同道历练在 match 上的战斗入口占用,规格见 docs/design/horizontal-scale-logical-db.md §4。
//
// guild 和 match 的护栏副本分在两个库,同一把 P 不再串行它们。match 是战斗入口占用的唯一权威:
//   - 进事务之前 Acquire(建房、同意、Prepare),对应旧 requireTrialEntryFree 在事务里读 match 的两条;
//   - 建房事务失败就 Release,大厅结束、launch 终结之后也 Release。Release 丢了不要紧,占用有租约;
//   - match 的 StartActivityBattle 是提交点,租约丢了那边拒绝,guild 按开局失败收尾。
//
// 大厅的整个存续期都在租约里:建房时租到邀请截止之后,同意时按邀请时长续,Prepare 时再续够开局用的时间,开局之后 match 延到战斗期限之后。
// 所以不需要单独的续租循环:guild 停摆时续不了租,也发不了 Start,占用过期不会造成重复占用。

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "proto/guild"
	matchpb "proto/match"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// trialOccupancyMargin 是租约在大厅截止之上多留的时间。要盖住开战窗口(5s)和两边库的时钟差。
	trialOccupancyMargin = 30 * time.Second
	// trialOccupancyBudget 是单次调 match 的上限,事务之外调,不占事务子预算。
	trialOccupancyBudget = 1000 * time.Millisecond
	// matchInternalCaller 是 guild 调 MatchInternal 时的 ServiceCallAuth.caller。
	matchInternalCaller = "guild"
)

// ErrTrialMatchUnavailable 表示这一次没问到 match 的结论。不是业务拒绝,logic 回「服务繁忙」,玩家重试即可。
var ErrTrialMatchUnavailable = errors.New("历练占用暂不可用")

// TrialOccupancy 是 guild 对 match 占用的全部要求。
//
// Acquire 全成或全不成,同一个 owner 重放幂等:角色有票、有未终结的整队开战、被别的大厅占着、冻结或已换主都回 ErrWriteConflict,
// 没问到结论回 ErrTrialMatchUnavailable。roster 里 owner_revision 为 0 的角色由 match 从它的副本捕获身份。
// Release 只放开这个 owner 的行,幂等。
type TrialOccupancy interface {
	Acquire(ctx context.Context, owner uint64, roster []*pb.GuildTrialRoleIdentity, ttl time.Duration) error
	Release(ctx context.Context, owner uint64, players []uint64) error
}

// BindTrialOccupancy 接入 match 上的占用,在 repo 交给任何调用方之前写一次。没绑时(没配 MatchRpc)建房和开局一律回 ErrTrialMatchUnavailable。
func (r *GuildRepo) BindTrialOccupancy(occupancy TrialOccupancy) {
	r.occupancy = occupancy
}

func (r *GuildRepo) acquireTrialOccupancy(ctx context.Context, owner uint64, roster []*pb.GuildTrialRoleIdentity, ttl time.Duration) error {
	if r.occupancy == nil {
		return ErrTrialMatchUnavailable
	}
	return r.occupancy.Acquire(ctx, owner, roster, ttl)
}

// releaseTrialOccupancy 尽力放开占用,失败只记日志:占用有租约,到期自己空闲。不继承请求的取消:这一步常在请求预算见底之后才走到。
func (r *GuildRepo) releaseTrialOccupancy(ctx context.Context, owner uint64, players []uint64) {
	if r.occupancy == nil {
		return
	}
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), trialOccupancyBudget)
	defer cancel()
	if err := r.occupancy.Release(call, owner, players); err != nil {
		logx.Errorf("[GuildTrial] release occupancy failed, it stays until the lease ends (lobby %d): %v", owner, err)
	}
}

// MatchOccupancyClient 用 match 内部服务实现 TrialOccupancy,带 guild 的签名。
type MatchOccupancyClient struct {
	client matchpb.MatchInternalClient
	secret string
}

// NewMatchOccupancyClient 装配。secret 和取消、开局用同一把:guild 调 MatchInternal 只有这一把密钥。
func NewMatchOccupancyClient(client matchpb.MatchInternalClient, secret string) (*MatchOccupancyClient, error) {
	if client == nil {
		return nil, errors.New("历练占用缺 match 客户端")
	}
	if _, err := serviceauth.New(map[string]string{matchInternalCaller: secret}); err != nil {
		return nil, err
	}
	return &MatchOccupancyClient{client: client, secret: secret}, nil
}

// Acquire 实现 TrialOccupancy。
func (c *MatchOccupancyClient) Acquire(ctx context.Context, owner uint64, roster []*pb.GuildTrialRoleIdentity, ttl time.Duration) error {
	in := &matchpb.AcquireTrialOccupancyRequest{OwnerId: owner, TtlMs: uint64(ttl.Milliseconds())}
	for _, role := range roster {
		in.Roster = append(in.Roster, &matchpb.TrialOccupancyRole{PlayerId: role.GetPlayerId(), Account: role.GetAccount(), OwnerRevision: role.GetOwnerRevision()})
	}
	if err := serviceauth.Sign(matchpb.MatchInternal_AcquireTrialOccupancy_FullMethodName, in, matchInternalCaller, c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return errors.Join(ErrTrialMatchUnavailable, err)
	}
	call, cancel := context.WithTimeout(ctx, trialOccupancyBudget)
	defer cancel()
	out, err := c.client.AcquireTrialOccupancy(call, in)
	if err != nil {
		return errors.Join(ErrTrialMatchUnavailable, err)
	}
	switch out.GetReject() {
	case matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_NONE:
		return nil
	case matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_OCCUPIED, matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_ROLE:
		return fmt.Errorf("%w: match rejected trial occupancy (%v, player %d)", ErrWriteConflict, out.GetReject(), out.GetOffenderPlayerId())
	default:
		return fmt.Errorf("%w: %v", ErrTrialMatchUnavailable, out.GetReject())
	}
}

// Release 实现 TrialOccupancy。
func (c *MatchOccupancyClient) Release(ctx context.Context, owner uint64, players []uint64) error {
	in := &matchpb.ReleaseTrialOccupancyRequest{OwnerId: owner, PlayerIds: players}
	if err := serviceauth.Sign(matchpb.MatchInternal_ReleaseTrialOccupancy_FullMethodName, in, matchInternalCaller, c.secret, uint64(time.Now().UnixMilli())); err != nil {
		return err
	}
	out, err := c.client.ReleaseTrialOccupancy(ctx, in)
	if err != nil {
		return err
	}
	if out.GetReject() != matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_NONE {
		return fmt.Errorf("match rejected trial occupancy release: %v", out.GetReject())
	}
	return nil
}
