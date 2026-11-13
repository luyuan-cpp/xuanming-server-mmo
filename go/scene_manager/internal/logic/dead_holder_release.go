package logic

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
)

// releaseDeadPlayerStoreHolder 在 LeaveScene 判出 owner_dead 之后、删 location 之前释放判死持有者的存档写权
// (docs/design/player-save-direct-commit.md §5.5)。locationEpoch 是 location 记的这一代,调用方保证非 0。
// 返回 nil = 已释放或没有东西要释放,调用方接着删 location;返回错误 = 保留 location,清理重试。
//
// 三步与交易判死(data_service account_role_dead_holder.go fenceDeadTradeHolders)同一口径,顺序要严格:
//
//  1. data_service 普通读写权行,记下 (E, H, claim_seq)。没有写权行,或代次已比 location 这一代新(新的持有者认领过了):
//     没有东西要释放。
//  2. H 非空时向 etcd 做一次线性一致读,复核 H 仍不在册(freshRegistryInstances,与 GO-3 顶替判定共用一份,见 owner_replaced.go)。
//     判死看的是本副本的注册表镜像,镜像可能滞后;判死之后持有进程也可能失租重注册、以同一 UUID 回来(ReRegisterExistingNode
//     保留身份),玩家同节点重连复用实体接着玩 —— 这时释放会把在线玩家踢掉,丢掉他判死之后的进度。H 在册就不释放,回
//     FailedPrecondition(与「没拿到释放证据」同一类,player_locator 保留清理任务重试,不记成故障);复核做不成同样不释放,
//     回 Unavailable。H 为空(建角初行、已被判死释放过)没有要复核的进程,直接走第 3 步:释放还要挡住可能在途的认领。
//  3. data_service 在存档库事务里比较并写:写权行还是第 1 步读到的那一份、代次不比 location 这一代新,才抬到 location 这一代 + 1、
//     清持有者、置 released=1;变了回 FailedPrecondition,下一次重试重读、重新复核。
//
// 第 1 步排在第 2 步前面是第 3 步比较并写的依据:第 2 步看到 H 不在册,H 之后的任何一次认领(要先重注册才进得了场)都晚于第 1 步,
// claim_seq 必然变了,第 3 步什么都不做。玩家同节点重登时 (E, H) 写回原值,只有 claim_seq 变,所以三列一起比。
//
// 残余:
//   - H 在册但不是判死节点上的进程:判死节点还没认领(交接不 release,写权行停在仍然活着的交接源上,location-release-binding.md §17)。
//     拿不准它此刻是否还持有这名玩家(交接回滚之后它就是现任),所以同样不释放。代价是 released 保持 0、location 留着:
//     资产操作的自动放弃与这次清理都等到玩家下次进场(新认领换掉写权行与 location)或那个进程下线,不丢不盖,
//     同 player-save-direct-commit.md §9 第 8 条已接受的残余。旧实现在这里直接释放,交接回滚的竞态下会把现任踢掉。
//   - 第 2 步之后、第 3 步提交之前(一次 RPC 的工夫)H 重注册、玩家同节点重连**复用实体**(不调认领,claim_seq 不变),
//     释放照样挡住它。2026-11-12 起这个窗口由持有者存活租约收口(holder_lease.go):失租的持有进程在身份不确认期间给内存里的
//     每名玩家续租,LeaveScene 的释放判定(judgeHolderRelease)看到租约就不判 owner_dead,走不到本函数;
//     重注册成功之后最后一次续的租约还会留到过期(至多 15 秒),盖住本副本镜像滞后的那一段。
//     只有租约续不上(玩家域 Redis 对那个节点也不可达)时窗口才还在。
func releaseDeadPlayerStoreHolder(ctx context.Context, svcCtx *svc.ServiceContext, playerID, locationEpoch uint64) error {
	result, err := releaseDeadPlayerStoreHolderOnce(ctx, svcCtx, playerID, locationEpoch)
	metrics.ObserveDeadHolderRelease(result)
	return err
}

// deadHolderReleaseKeepsLocation 报告 releaseDeadPlayerStoreHolder 的错误是不是「还不能释放」(持有实例在册,或写权行在复核之后变了)。
// 这是可以长期存在的正常等待(交接源仍在册的残余,见上),调用方按 Info 记、保留 location 重试;其余错误才是故障。
func deadHolderReleaseKeepsLocation(err error) bool {
	return status.Code(err) == codes.FailedPrecondition
}

// releaseDeadPlayerStoreHolderOnce 是上面三步的本体,另外回一个结果标签(metrics.DeadHolderRelease*,低基数)。
func releaseDeadPlayerStoreHolderOnce(ctx context.Context, svcCtx *svc.ServiceContext, playerID, locationEpoch uint64) (string, error) {
	fence := svcCtx.PlayerStoreDeadHolders
	if fence == nil {
		return metrics.DeadHolderReleaseError, status.Error(codes.Unavailable, "player store dead-holder fence is not configured; keeping the location")
	}
	read, err := fence.ReadPlayerStoreHolder(ctx, playerID)
	if err != nil {
		return metrics.DeadHolderReleaseError, err
	}
	if !read.GetFound() {
		return metrics.DeadHolderReleaseNoRow, nil
	}
	seen := read.GetHolder()
	if seen == nil {
		return metrics.DeadHolderReleaseError, status.Errorf(codes.Unavailable,
			"data_service reported a player store holder row for player %d without its columns", playerID)
	}
	if seen.GetOwnerEpoch() > locationEpoch {
		return metrics.DeadHolderReleaseNewerOwner, nil
	}
	if holder := string(seen.GetHolderInstance()); holder != "" {
		instances, err := freshRegistryInstances(ctx, svcCtx)
		if err != nil {
			return metrics.DeadHolderReleaseRegistryUnavailable, status.Errorf(codes.Unavailable,
				"re-checking holder instance %s of player %d in etcd failed; not releasing: %v", holder, playerID, err)
		}
		if _, present := instances[holder]; present {
			// 它在册:本副本对它的消失记录(若有)已经过时,作废(与 playerLocationOwnerReplaced 的镜像滞后分支同一处理),
			// 之后再消失时屏障重新起算。
			forgetInstanceGoneObservation(holder)
			return metrics.DeadHolderReleaseHolderRegistered, status.Errorf(codes.FailedPrecondition,
				"%s: player=%d holder instance %s is registered in etcd; not releasing its player store claim (owner_epoch=%d location_epoch=%d)",
				holderReleasePendingMessage, playerID, holder, seen.GetOwnerEpoch(), locationEpoch)
		}
	}
	err = fence.ReleaseDeadPlayerStoreHolderIfUnchanged(ctx, playerID, locationEpoch, seen)
	switch {
	case err == nil:
		return metrics.DeadHolderReleaseReleased, nil
	case deadHolderReleaseKeepsLocation(err):
		return metrics.DeadHolderReleaseRowChanged, err
	default:
		return metrics.DeadHolderReleaseError, err
	}
}
