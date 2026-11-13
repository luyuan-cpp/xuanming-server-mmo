package logic

import (
	"context"
	"errors"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/ownerepoch"
)

// 持有者存活租约的读方(docs/design/data-consistency-guarantee-map-20261020.md §6.3 第 6 条)。
//
// 判死接管(playerLocationOwnerDead / playerLocationOwnerGone / playerLocationOwnerReplaced)和 LeaveScene 的判死释放,
// 证据都是「持有节点从 etcd 注册表消失、再入屏障已过」。这证明不了持有进程已经停写:节点只和 etcd 失联时,进程照常连着
// gate 与 go/db,玩家还在它上面玩。这时接管,新持有者认领把写权行的代次抬上去,老进程最后一次提交之后的进度被拒掉 ——
// 写权行保证只丢不盖,但确实丢了。
//
// 所以持有进程在自己的节点身份不确认期间,给内存里的每名玩家在玩家域 Redis 续一个短租约 player:{id}:holder_lease
// (值 = 进程实例号,PX 15 秒,每 3 秒续一次;C++ player_lifecycle.cpp 的 RenewHolderLeasesAtRisk)。这里在接管与判死释放之前读它:
//
//   - 在:持有进程还活着,实体还在它内存里。不按判死处理,照旧走换手门(18,等它存盘释放)或回可重试的 17。
//     玩家留在原节点接着玩;等他离开(退出存盘提交、实体销毁,租约不再续)或那个进程真的退出,租约过期之后才接管。
//   - 不在:照原判定。进程真死了,租约 15 秒内过期,比再入屏障(缺省 20 秒)短,不给死节点的接管加延迟。
//   - 读失败:拿不准,按「在」处理(fail-closed),请求本来就是可重试的。
//
// 持有者自己为当前代写出的落盘标记优先于租约:标记是它的声明「这一代的状态已落盘、不再写」(疏散改派、退出释放都会写),
// 这时接管不丢东西,也不能把这些协作式的流程挡在门外。
//
// 身份确认时持有进程不写这个键:那时节点在 etcd 在册,判死的前提不成立,用不到它。身份不确认比 etcd 租约到期早半个租约,
// 再加上再入屏障,第一份租约一定先于最早可能的接管。
//
// 安全性不靠这一层:存档的围栏仍然只有 TiDB 写权行。租约续不上(玩家域 Redis 对那个节点也不可达)或在这里被读成不在时,
// 最坏回到原来的行为 —— 只丢不盖,最多丢持有进程的补存间隔(5 秒)。它只负责不去抢活着的持有者。

// holder_lease_blocked_total 的 site 取值。**必须是常量**:它们直接变成 Prometheus label。
const (
	holderLeaseSiteDeadOwnerTakeover     = "dead_owner_takeover"
	holderLeaseSiteStaleLocation         = "stale_location"
	holderLeaseSiteReplacedOwnerTakeover = "replaced_owner_takeover"
	holderLeaseSiteLeaveRelease          = "leave_release"
)

// holderLeaseState 是读一次租约的结论,名字同时是 holder_lease_blocked_total 的 state 标签。
// 底层类型 uint8 数的是结论种类数(现 3 种)。
type holderLeaseState uint8

const (
	// holderLeaseAbsent:键不存在。没有活着的进程声称内存里还有这名玩家。
	holderLeaseAbsent holderLeaseState = iota
	// holderLeaseHeld:键存在。持有进程还活着,实体还在它内存里。
	holderLeaseHeld
	// holderLeaseUnknown:读失败,拿不准,按 held 处理。
	holderLeaseUnknown

	holderLeaseStateCount
)

var holderLeaseStateNames = [...]string{
	holderLeaseAbsent:  "absent",
	holderLeaseHeld:    "held",
	holderLeaseUnknown: "unknown",
}

// 名表长度与 holderLeaseStateCount 不一致时编译失败(漏加 / 多加一行)。
var _ [holderLeaseStateCount]struct{} = [len(holderLeaseStateNames)]struct{}{}

func (s holderLeaseState) String() string {
	if s < holderLeaseStateCount {
		return holderLeaseStateNames[s]
	}
	return "invalid"
}

// keepsOwner:这个结论下不能把持有者当成已死。只有确实读到「键不存在」才放行。
func (s holderLeaseState) keepsOwner() bool {
	return s != holderLeaseAbsent
}

// readHolderLease 读 player:{id}:holder_lease。instance 是租约里的进程实例号,只在 held 时有意义(进日志);
// err 只在 unknown 时非 nil。值是空串也算 held:判的是键在不在,不是值。
func readHolderLease(svcCtx *svc.ServiceContext, playerID uint64) (state holderLeaseState, instance string, err error) {
	instance, err = svcCtx.PlayerRedis.Get(context.Background(), ownerepoch.HolderLeaseKey(playerID)).Result()
	switch {
	case errors.Is(err, goredis.Nil):
		return holderLeaseAbsent, "", nil
	case err != nil:
		return holderLeaseUnknown, "", err
	default:
		return holderLeaseHeld, instance, nil
	}
}

// holderLeaseKeepsOwner 在调用方已经确认「持有者没有为当前代写出落盘标记」时用(LeaveScene 的释放判定自己读过标记):
// 租约在或读不出来就返回 true,调用方不按判死处理。返回 true 时计数并打日志。
func holderLeaseKeepsOwner(svcCtx *svc.ServiceContext, log logx.Logger, playerID uint64, zoneID uint32, site string) bool {
	state, instance, err := readHolderLease(svcCtx, playerID)
	if !state.keepsOwner() {
		return false
	}
	reportHolderLeaseBlocked(log, playerID, zoneID, site, state, instance, err)
	return true
}

// liveHolderBlocksTakeover 是 EnterScene 三个判死接管点的入口,在判死 / 判顶替 / 判整 zone 下线成立之后调用。
// epoch 是当前代(owner_epoch 键;键为 0 时取 location 记的值,见 leaveReleaseEpoch)。
// 返回 true = 持有进程还活着、又没有为当前代写出落盘标记,不能按无持有者接管。
//
// 先读租约,只有租约在才去读标记:平时(租约不在)每次接管只多一次 GET。标记读不出来按没有标记处理,由租约决定。
func liveHolderBlocksTakeover(svcCtx *svc.ServiceContext, log logx.Logger, playerID uint64, zoneID uint32, epoch uint64, site string) bool {
	state, instance, err := readHolderLease(svcCtx, playerID)
	if !state.keepsOwner() {
		return false
	}
	if state == holderLeaseHeld {
		if markerRaw, markerErr := playerRedisGet(svcCtx, ownerepoch.HandoffKey(playerID)); markerErr == nil && markerReleasesEpoch(markerRaw, epoch) {
			return false
		}
	}
	reportHolderLeaseBlocked(log, playerID, zoneID, site, state, instance, err)
	return true
}

// reportHolderLeaseBlocked 记一次被租约挡下的判死。held 是「节点和 etcd 失联但进程活着」的正常表现,记 Info;
// unknown 是玩家域 Redis 读失败,记 Error。
func reportHolderLeaseBlocked(log logx.Logger, playerID uint64, zoneID uint32, site string, state holderLeaseState, instance string, err error) {
	metrics.ObserveHolderLeaseBlocked(zoneID, site, state.String())
	if state == holderLeaseUnknown {
		log.Errorf("[HolderLease] cannot read the holder lease; not treating the location owner as dead: player=%d zone=%d site=%s err=%v",
			playerID, zoneID, site, err)
		return
	}
	log.Infof("[HolderLease] location owner is gone from etcd but its process still renews the holder lease; not treating it as dead: player=%d zone=%d site=%s holder_instance=%s",
		playerID, zoneID, site, instance)
}
