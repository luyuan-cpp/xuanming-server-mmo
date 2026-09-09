// Package homezone 是 login 对 data_service `player:zone:{player_id}` 映射
// (玩家当前归属哪个 zone)的唯一客户端,供三条链复用:
//
//   - CreatePlayer 铸出 PlayerId 后、落账号 blob 之前,用它登记映射
//     (见 RegisterPlayerZone);这是**唯一**的生产侧写入点——修复前只有
//     data_service/cmd/debug_import 与合服重映射会写,新建的玩家根本没有映射,
//     整套 home_zone 模型只覆盖导入的玩家;
//   - Login 返回角色列表时,用它覆盖每个角色的 zone_id(见 RefreshRoleZones);
//     这是合服后的**主修正机制**:客户端按它选区就直接连到归属 zone 的 gate;
//   - EnterGame 组 EnterScene 请求时(仅 HomeZone.RedirectOnEnterEnabled=true 且
//     首次登录、无在场 scene),用它决定 ZoneId,让 scene_manager 既有的跨区重定向
//     把玩家送去正确的 zone(见 ResolveEnterZone)。
//
// 为什么需要它:账号 blob 里 AccountSimplePlayer.zone_id 是**建角那一刻**盖的章,
// 之后再也不更新(createplayerlogic.go);合服(tools/merge_zone /
// RemapHomeZoneForMerge)只改映射不改账号 blob。于是合服之后角色列表仍指向已下线
// 的源 zone,客户端按它自动选区就会被引向一个空壳;而经源 zone 进来的玩家会在源
// zone 的 scene 进程里玩、数据/公会/榜却按目标 zone 分组。映射才是唯一真源,所以
// 这里只读映射、只在返回给客户端的那一份上覆盖,**刻意不回写账号 blob**:
// 一旦回写就有了第二份「真相」,再次合服 / 回滚时两份必然打架。
//
// 「没有映射」的两种线上契约都要认:老版 data_service 对缺失的 key 返回一个
// gRPC 错误(Unknown,消息含 "no home zone mapping");新版改为 HomeZoneId=0 + nil。
// HomeZone 把两者统一成 (0, nil)——「未映射」是**数据状态**(老玩家 / 登记失败),
// 按 INFO 记;只有传输层错误(Unavailable / 超时 / 熔断)才是**故障**,按 ERROR 记。
//
// 读路径的失败方向恒为「保留存量值、不阻断登录」:data_service 是弱依赖,
// 它挂了玩家仍要能登进来(只是可能被送到旧 zone,和修复前一样)。写路径
// (RegisterPlayerZone)相反,失败必须让建角失败——见 createplayerlogic.go 的理由。
package homezone

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pbbase "proto/common/base"
	dspb "proto/data_service"
)

const (
	// DefaultRoleListLookupTimeout 是 Login 角色列表那一次 BatchGetPlayerHomeZone
	// 的默认预算。它串在**每一次登录**上,而且失败只是退回建角 zone,所以要短:
	// 500ms 覆盖 data_service 一次 Redis 往返加网络抖动,再长就是拿所有人的登录
	// 延迟换一个本来就允许失败的查询。
	DefaultRoleListLookupTimeout = 500 * time.Millisecond
	// DefaultEnterLookupTimeout 是 EnterGame 那一次 GetPlayerHomeZone 的默认预算。
	// EnterGame 链路本身异步(5 分钟预算),可以比角色列表宽松。
	DefaultEnterLookupTimeout = 1500 * time.Millisecond
	// DefaultRegisterTimeout 是 CreatePlayer 写映射的默认预算。写失败建角整体失败,
	// 所以与 DataServiceRpc.Timeout(3s)对齐,不抢先超时。
	DefaultRegisterTimeout = 3 * time.Second

	// unmappedErrText 是老版 data_service 对缺失映射返回的错误消息片段
	// (go/data_service/internal/routing/router.go GetPlayerHomeZone)。
	unmappedErrText = "no home zone mapping"
)

// ErrUnavailable 表示没有可用的 data_service 客户端(IdSegment 关闭且没配
// DataServiceRpc 时 login 根本不拨号)。读路径按「查不到」处理,写路径按失败处理。
var ErrUnavailable = errors.New("homezone: data_service client not configured")

// Lookup 是 EnterGame 侧依赖的最小接口,方便单测用假实现替换 Resolver。
type Lookup interface {
	// HomeZone 返回玩家当前归属 zone;(0, nil) 表示映射里没有,err 非 nil 表示查询失败。
	HomeZone(ctx context.Context, playerID uint64) (uint32, error)
}

// Resolver 包装 data_service 的映射 RPC,并给每次调用套上有界超时。
// Client 为 nil 时所有方法返回 ErrUnavailable,不 panic。
type Resolver struct {
	Client dspb.DataServiceClient
	// RoleListTimeout 给 BatchHomeZones(Login 角色列表);<=0 用 DefaultRoleListLookupTimeout。
	RoleListTimeout time.Duration
	// EnterTimeout 给 HomeZone(EnterGame 单查);<=0 用 DefaultEnterLookupTimeout。
	EnterTimeout time.Duration
	// RegisterTimeout 给 RegisterPlayerZone(CreatePlayer);<=0 用 DefaultRegisterTimeout。
	RegisterTimeout time.Duration
}

// New 构造 Resolver;各超时 <=0 用对应默认值。
func New(client dspb.DataServiceClient, roleListTimeout, enterTimeout, registerTimeout time.Duration) *Resolver {
	r := &Resolver{
		Client:          client,
		RoleListTimeout: roleListTimeout,
		EnterTimeout:    enterTimeout,
		RegisterTimeout: registerTimeout,
	}
	r.normalize()
	return r
}

func (r *Resolver) normalize() {
	if r.RoleListTimeout <= 0 {
		r.RoleListTimeout = DefaultRoleListLookupTimeout
	}
	if r.EnterTimeout <= 0 {
		r.EnterTimeout = DefaultEnterLookupTimeout
	}
	if r.RegisterTimeout <= 0 {
		r.RegisterTimeout = DefaultRegisterTimeout
	}
}

func bounded(ctx context.Context, timeout, fallback time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = fallback
	}
	return context.WithTimeout(ctx, timeout)
}

// IsUnmapped 判断 GetPlayerHomeZone 的错误是否只是「映射里没有这个玩家」:
// gRPC NotFound,或老版 data_service 的 Unknown + "no home zone mapping" 文案。
// 导出只为让调用方 / 单测能对齐同一条判据。
func IsUnmapped(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.NotFound {
			return true
		}
		return strings.Contains(st.Message(), unmappedErrText)
	}
	return strings.Contains(err.Error(), unmappedErrText)
}

// HomeZone 查单个玩家的当前归属 zone。(0, nil) 表示映射里确实没有这个玩家
// (老数据 / 建角时 RegisterPlayerZone 失败)——两种服务端契约(NotFound 错误 /
// HomeZoneId=0)都归一到这里;调用方应回退到自己的默认值。err 非 nil 只剩传输层故障。
func (r *Resolver) HomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	if r == nil || r.Client == nil {
		return 0, ErrUnavailable
	}
	cctx, cancel := bounded(ctx, r.EnterTimeout, DefaultEnterLookupTimeout)
	defer cancel()
	resp, err := r.Client.GetPlayerHomeZone(cctx, &dspb.GetPlayerHomeZoneRequest{PlayerId: playerID})
	if err != nil {
		if IsUnmapped(err) {
			return 0, nil
		}
		return 0, err
	}
	return resp.GetHomeZoneId(), nil
}

// BatchHomeZones 一次 RPC 查多个玩家。返回的 map 只含映射里存在的 id;
// 缺席的 id 由调用方保留存量值。
func (r *Resolver) BatchHomeZones(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
	if r == nil || r.Client == nil {
		return nil, ErrUnavailable
	}
	if len(playerIDs) == 0 {
		return map[uint64]uint32{}, nil
	}
	cctx, cancel := bounded(ctx, r.RoleListTimeout, DefaultRoleListLookupTimeout)
	defer cancel()
	resp, err := r.Client.BatchGetPlayerHomeZone(cctx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: playerIDs})
	if err != nil {
		return nil, err
	}
	return resp.GetPlayerZoneMap(), nil
}

// RegisterPlayerZone 把 player_id → home_zone 写进映射。这是生产侧**唯一**的写入点
// (CreatePlayer),失败必须向上返回让建角失败——映射是数据路由 / 公会 / 榜的
// 归属权威,没有映射的玩家是一颗迟早引爆的路由炸弹,不能「先建了再说」。
// zone 为 0 视为调用方 bug,直接拒绝而不是写一条无意义的映射。
func (r *Resolver) RegisterPlayerZone(ctx context.Context, playerID uint64, zone uint32) error {
	if r == nil || r.Client == nil {
		return ErrUnavailable
	}
	if playerID == 0 || zone == 0 {
		return errors.New("homezone: RegisterPlayerZone needs non-zero player_id and zone")
	}
	cctx, cancel := bounded(ctx, r.RegisterTimeout, DefaultRegisterTimeout)
	defer cancel()
	_, err := r.Client.RegisterPlayerZone(cctx, &dspb.RegisterPlayerZoneRequest{PlayerId: playerID, HomeZoneId: zone})
	return err
}

// RefreshRoleZones 返回 players 的**副本**,其中每个角色的 zone_id 被映射里的
// 当前归属 zone 覆盖(映射有且非 0 才覆盖)。映射里没有的 id、或整次 RPC 失败,
// 一律保留账号 blob 里的建角 zone,并只打一条日志 —— 绝不让登录因此失败。
//
// 返回副本而不是原地改,是把「不回写账号 blob」这条决定落实在类型上:调用方
// 拿到的切片与 userAccount 不共享指针,之后无论谁 Marshal userAccount 都不会
// 把刷新后的 zone 带进去。
func (r *Resolver) RefreshRoleZones(ctx context.Context, players []*pbbase.AccountSimplePlayer) []*pbbase.AccountSimplePlayer {
	out := make([]*pbbase.AccountSimplePlayer, 0, len(players))
	ids := make([]uint64, 0, len(players))
	for _, p := range players {
		if p == nil {
			continue
		}
		out = append(out, proto.Clone(p).(*pbbase.AccountSimplePlayer))
		ids = append(ids, p.GetPlayerId())
	}
	if len(ids) == 0 {
		return out
	}

	zones, err := r.BatchHomeZones(ctx, ids)
	if err != nil {
		// 每个请求最多一条:这里不会循环打日志。
		logx.WithContext(ctx).Errorf("[home-zone] role list keeps creation-time zone_id for %d role(s): "+
			"BatchGetPlayerHomeZone failed: %v", len(ids), err)
		return out
	}

	missing := 0
	for _, p := range out {
		zone, ok := zones[p.GetPlayerId()]
		if !ok || zone == 0 {
			missing++
			continue
		}
		p.ZoneId = zone
	}
	if missing > 0 {
		logx.WithContext(ctx).Infof("[home-zone] %d/%d role(s) absent from player:zone mapping, keeping stored zone_id",
			missing, len(out))
	}
	return out
}

// ResolveEnterZone 决定 EnterScene 请求里的 ZoneId。
//
// 返回 (targetZone, redirected):
//   - 映射给出非 0 且 != ownZone 的归属 zone → (homeZone, true):调用方把 ZoneId
//     设成它、GateZoneId 仍是 login 自己的 zone,scene_manager 的跨区重定向随之触发;
//   - 映射给出 == ownZone、或未映射(0)、或查询失败 → (ownZone, false):维持修复前行为。
//
// 未映射按 INFO(数据状态,老玩家很常见);只有传输层故障才按 ERROR。两者都不阻断:
// 进游戏不能因 data_service 不可用而挂掉。
func ResolveEnterZone(ctx context.Context, lookup Lookup, playerID uint64, ownZone uint32) (uint32, bool) {
	if lookup == nil {
		return ownZone, false
	}
	home, err := lookup.HomeZone(ctx, playerID)
	if err != nil {
		logx.WithContext(ctx).Errorf("[home-zone] EnterGame keeps zone=%d for player=%d: home zone lookup failed: %v",
			ownZone, playerID, err)
		return ownZone, false
	}
	if home == 0 {
		logx.WithContext(ctx).Infof("[home-zone] EnterGame keeps zone=%d for player=%d: no player:zone mapping",
			ownZone, playerID)
		return ownZone, false
	}
	if home == ownZone {
		return ownZone, false
	}
	logx.WithContext(ctx).Infof("[home-zone] EnterGame routing player=%d from zone=%d to home zone=%d (cross-zone redirect)",
		playerID, ownZone, home)
	return home, true
}
