package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/ownerepoch"

	"github.com/zeromicro/go-zero/core/logx"
)

// luaDeletePlayerLocationIfUnchanged:location 仍是调用方读到的那份原文时,删掉它并在同一次
// 提交里把人数减一(GO-4 残余);释放模式下另外原子复核"持有者已释放"(docs/design/location-release-binding.md R2)。
//
// KEYS[1]=player:{id}:location  KEYS[2]=player:{id}:owner_epoch  KEYS[3]=player:{id}:handoff
// KEYS[4]=player:{id}:leave_release_wait(删成功时一并删掉等待计时)
// KEYS[5]=instance:{sceneId}:player_count
// KEYS[6]=node:zone:{z}:{node}:player_count(scene:{id}:node 读不到时不传,脚本按 #KEYS 处理)
// ARGV[1]=调用方 GET 到、并据此核对过 SceneId 的 location 原文
// ARGV[2]=模式:any(只比原文)| released(另比 owner_epoch 与 handoff 标记)
// ARGV[3]=当前代 E 的十进制文本(只在 released 模式下用)
// 返回 1=已删且人数已减;0=键已不在;-1=已被改写,什么都没动;-2=释放模式复核不过(未释放),什么都没动
//
//   - 为什么比对原文而不只比 SceneId:原文里带 node_id / owner_epoch / update_time,同一场景里
//     换了节点或铸了新 epoch 的新落点也能认出来。只比 SceneId 的裸 DEL 会把 GET 之后并发
//     EnterScene 写入的新落点一起删掉。
//   - 为什么人数也放进脚本:go-redis 读超时后会把同一条 EVAL 原样重发(与 owner_epoch.go 的
//     重放识别同一原因)。首发已删、应答丢失时,重发只能看到「键已不在」;如果减人数留在脚本外、
//     又按「不是本次删的就不减」处理,就没有任何一方减过,实例人数永久多 1,destroy-on-empty
//     永不触发。另一种情况是 go-redis 重试耗尽后返回错误,player_locator 再次调用时 GET 为空。
//     把删与减绑成同一次提交,「谁删谁减」对这两种情况都成立。
//   - 释放模式为什么在脚本里再比一次:Go 侧预读到标记之后、删除之前,标记可能被撤回(交接取消)、owner_epoch
//     可能被推进。判定与提交锚在同一次原子执行上,中间没有窗口。owner_epoch 缺键视作未变:标记只能在键为 E
//     时写出(exit_release_mark::kLuaWriteIfOwnerEpoch),已有的 "E:t" 仍是有效证据。代际比较不用 tonumber
//     (uint64 超过 2^53 会丢精度):去掉前导零后按字符串比,与 C++ A2′ 同法。
//   - 钳 0 与 DecrInstancePlayerCount 一致;键全部由 Go 构造、经 KEYS 声明,与 scene_atomic.go
//     同一约定(脚本从不用数据拼键)。
const luaDeletePlayerLocationIfUnchanged = `
local count_end = tonumber(ARGV[6]) or #KEYS
if ARGV[4] ~= nil then
    if (redis.call('GET',KEYS[count_end+1]) or '') ~= ARGV[4] then return -1 end
    if ARGV[5] ~= '' and redis.call('GET',KEYS[count_end+2]) ~= ARGV[5] then return -1 end
end
local cur = redis.call('GET', KEYS[1])
if cur == false then
    return 0
end
if cur ~= ARGV[1] then
    return -1
end
if ARGV[2] == 'released' then
    local owner = redis.call('GET', KEYS[2])
    if owner ~= false and owner ~= ARGV[3] then
        return -2
    end
    local mark = redis.call('GET', KEYS[3])
    if mark == false then
        return -2
    end
    local e = string.match(mark, '^(%d+):%d+$')
    if e == nil then
        return -2
    end
    e = (string.gsub(e, '^0+(%d)', '%1'))
    if e ~= ARGV[3] then
        return -2
    end
elseif ARGV[2] ~= 'any' then
    return redis.error_reply('LEAVE_SCENE unknown delete mode')
end
redis.call('DEL', KEYS[1], KEYS[4])
if ARGV[4] ~= nil then redis.call('DEL',KEYS[count_end+1]) end
for i = 5, count_end do
    local v = redis.call('DECR', KEYS[i])
    if v < 0 then
        redis.call('SET', KEYS[i], '0')
    end
end
return 1
`

// deletePlayerLocationGuarded 的结果(只用于日志与单测,不进指标 label)。leaveOutcomeNotReleased 见 leave_release.go。
const (
	leaveOutcomeDeleted     = "deleted"
	leaveOutcomeAlreadyGone = "already_gone"
	leaveOutcomeSuperseded  = "superseded"
)

// deletePlayerLocationIfUnchanged 在 location 仍等于 expectedRaw 时删掉它(只比原文,不看释放证据),并在同一段 Lua 里把
// 场景人数(以及此刻 scene:{id}:node 指向的节点人数)减一。LeaveScene 走 deletePlayerLocationGuarded;本函数是它的
// any 模式,保留给只关心删除机制本身的调用方与单测。
func deletePlayerLocationIfUnchanged(svcCtx *svc.ServiceContext, playerID uint64, expectedRaw string,
	zoneID uint32, sceneID uint64) (string, error) {
	return deletePlayerLocationGuarded(svcCtx, playerID, expectedRaw, zoneID, sceneID, leaveDeleteModeAny, 0)
}

// deletePlayerLocationGuarded 按 mode 条件删除 location(见 luaDeletePlayerLocationIfUnchanged)。
//
// 节点人数键按**此刻**的 scene:{id}:node 找,找不到就只减场景计数 —— 与 DecrInstancePlayerCount
// 同一口径,也同样不原子:scene:{id}:node 在读与 EVAL 之间被改派时,节点人数最多偏差一次。
func deletePlayerLocationGuarded(svcCtx *svc.ServiceContext, playerID uint64, expectedRaw string,
	zoneID uint32, sceneID uint64, mode string, epoch uint64) (string, error) {
	_, raw, tag, err := getPlayerLocationSnapshot(svcCtx, playerID)
	if err != nil {
		return "", err
	}
	if raw == "" {
		return leaveOutcomeAlreadyGone, nil
	}
	if raw != expectedRaw {
		return leaveOutcomeSuperseded, nil
	}
	return deletePlayerLocationGuardedWithTag(svcCtx, playerID, expectedRaw, zoneID, sceneID, mode, epoch, tag, 0)
}

func deletePlayerLocationGuardedWithTag(svcCtx *svc.ServiceContext, playerID uint64, expectedRaw string,
	zoneID uint32, sceneID uint64, mode string, epoch uint64, expectedTag string, revision uint64) (string, error) {
	keys := []string{
		getPlayerLocationKey(playerID),
		ownerepoch.OwnerEpochKey(playerID),
		ownerepoch.HandoffKey(playerID),
		leaveReleaseWaitKey(playerID),
		fmt.Sprintf(InstancePlayerCountKey, sceneID),
	}
	if node := lookupSceneNode(svcCtx, sceneID); node != "" {
		keys = append(keys, nodePlayerCountKey(zoneID, node))
	}
	countEnd := len(keys)
	keys = append(keys, locationRevisionKey(playerID), locationRevisionFloorKey(playerID))

	result, err := svcCtx.Redis.Eval(luaDeletePlayerLocationIfUnchanged, keys, expectedRaw, mode,
		strconv.FormatUint(epoch, 10), expectedTag, optionalRevisionText(revision), countEnd)
	if err != nil {
		return "", fmt.Errorf("比对删除 location 失败: player=%d scene=%d mode=%s: %w", playerID, sceneID, mode, err)
	}
	switch fmt.Sprint(result) {
	case "1":
		return leaveOutcomeDeleted, nil
	case "0":
		return leaveOutcomeAlreadyGone, nil
	case "-1":
		return leaveOutcomeSuperseded, nil
	case "-2":
		return leaveOutcomeNotReleased, nil
	}
	return "", fmt.Errorf("比对删除 location 返回值非法: player=%d scene=%d mode=%s result=%v", playerID, sceneID, mode, result)
}

type LeaveSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveSceneLogic {
	return &LeaveSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// LeaveScene handles a player leaving a scene (or pre-transfer cleanup).
//
// 删除是「比对原文」的条件删除,删 location 与减人数在同一段 Lua 里提交(见
// luaDeletePlayerLocationIfUnchanged)。LeaveScene 不再单独调用 DecrInstancePlayerCount:
// 删掉它的那一方在脚本里减过;改写它的 EnterScene 已减过旧场景人数。
//
// 错误语义:Redis 读失败或删除失败返回 error(以前回成功)。依据是唯一调用方 player_locator 的
// 契约(leasemonitor.go 的租约清理:RPC 失败必须返回错误,出错时保留 claim 重试;MarkOffline
// 同步调用失败才入租约队列,cleanup-pending 屏障随之生效)。旧行为是「Redis 抖一下 → ACK →
// location 永久残留」。
//
// 会话身份(scene-enter-chain-residuals.md §3.2):SceneId 比对之后按 decideLeaveIdentity 比较 location 记的
// 身份与请求带的被删会话身份。location 已属于更新的登录(keep_newer)或代次相等而会话冲突(keep_conflict)时
// 不删、不扣人数、回成功;legacy(任一侧无代次)与 deleted 照旧原文 CAS 删除。判定读的是 GET 到的同一份原文,
// 随后的删除 Lua 仍以它为期望值,判定与提交之间没有窗口。以前挡不住的两类因此收口:
//   - samePlacement 重连:新登录在 samePlacement 时已把自己的身份写进 location(enterscenelogic.go 第 4 步),
//     迟到的旧会话 LeaveScene 判 keep_newer;
//   - 同一场景的新落点在本次 GET 之前就已写入:新落点带着新登录的身份,同样判 keep_newer;GET 之后写入的
//     由原文 CAS 挡住。
//
// 剩下的只有混版窗口:旧版 scene_manager 副本写出的无身份 location、旧版 player_locator 发来的无身份请求,
// 都按 legacy 退回旧行为(§3.5、§3.8)。
//
// 持有者释放(docs/design/location-release-binding.md §3.2):身份上允许删之后,还要证明 location 的持有节点已经把当前代
// 状态落盘并释放(当前代 handoff 标记 / 持有节点已死 / 没有持有者)。拿不到就**保留** location、回 FailedPrecondition
// (player_locator 保留清理任务重试),身份判 deleted 时顺带请持有节点按会话结束释放。会话结束不等于持有者已停笔:
// 以前在这里直接删,下一次进场就按首次落点铸造新 epoch、绕过换手门,新节点读旧快照,持有节点之后的存盘被拒、实体被销毁(回档)。
func (l *LeaveSceneLogic) LeaveScene(in *scene_manager.LeaveSceneRequest) (*base.Empty, error) {
	currentLoc, raw, tag, err := getPlayerLocationSnapshot(l.svcCtx, in.PlayerId)
	if errors.Is(err, errLocationProjectionCorrupt) {
		return &base.Empty{}, nil
	}
	if err != nil {
		l.Logger.Errorf("Player %d leave scene %d: read location failed, returning error so the caller keeps its claim and retries: %v",
			in.PlayerId, in.SceneId, err)
		return nil, fmt.Errorf("read location of player %d: %w", in.PlayerId, err)
	}
	if raw == "" {
		l.Logger.Infof("Player %d requested leave scene %d but has no location info", in.PlayerId, in.SceneId)
		return &base.Empty{}, nil
	}

	currentRole, err := lookupCurrentRole(l.ctx, l.svcCtx, in.PlayerId)
	if err != nil {
		return nil, err
	}
	if err := observeCurrentRevision(l.svcCtx, in.PlayerId, currentRole.OwnerRevision); err != nil {
		return nil, err
	}
	tagRevision, err := taggedLocationRevision(tag, raw)
	if err != nil {
		return nil, err
	}
	if tagRevision != 0 && tagRevision != currentRole.OwnerRevision {
		_, _, _, err := locationForRevision(l.svcCtx, in.PlayerId, currentRole.OwnerRevision)
		return &base.Empty{}, err
	}
	// 无原会话的旧清理不能删除已绑定新归属的投影；现代locator总带原session/generation。
	if tagRevision != 0 && (in.SessionId == 0 || in.LoginGeneration == 0) {
		return &base.Empty{}, nil
	}

	if currentLoc.SceneId != in.SceneId {
		l.Logger.Infof("Player %d requested leave scene %d but is in scene %d, ignoring",
			in.PlayerId, in.SceneId, currentLoc.SceneId)
		return &base.Empty{}, nil
	}

	locIdentity, reqIdentity := identityOfLocation(currentLoc), identityOfLeaveRequest(in)
	identityVerdict := decideLeaveIdentity(locIdentity, reqIdentity)
	metrics.ObservePlacementIdentity(metrics.PlacementIdentitySiteLeave, identityVerdict.String())
	switch identityVerdict {
	case leaveVerdictKeepNewer:
		// b1–b4 的合法拦截:离场的会话早已被更新的登录取代,这条 location 属于那次新登录。
		l.Logger.Infof("[PlacementIdentity] LeaveScene kept location owned by a newer login: player=%d scene=%d node=%s request_session=%d request_generation=%d location_session=%d location_generation=%d",
			in.PlayerId, in.SceneId, currentLoc.NodeId, reqIdentity.sessionID, reqIdentity.loginGeneration,
			locIdentity.sessionID, locIdentity.loginGeneration)
		return &base.Empty{}, nil
	case leaveVerdictKeepConflict:
		// 代次相等而会话不同 = 违反 G2(非 0 代次与会话一一对应)。删错会让在线玩家丢 location,后果比泄漏重,
		// 所以不删;有泄漏风险,以 Error 日志与 keep_conflict 计数观察。
		l.Logger.Errorf("[PlacementIdentity] LeaveScene kept location: equal login generation with different sessions (G2 violated): player=%d scene=%d node=%s request_session=%d location_session=%d login_generation=%d",
			in.PlayerId, in.SceneId, currentLoc.NodeId, reqIdentity.sessionID, locIdentity.sessionID, locIdentity.loginGeneration)
		return &base.Empty{}, nil
	}
	// legacy / deleted:身份上允许删,再看持有者有没有释放(下面)。
	if !identityVerdict.deletes() {
		// 按构造不可达:switch 已处理全部 keep 结论。防将来新增结论漏改上面的分支时静默删除。
		l.Logger.Errorf("[PlacementIdentity] LeaveScene unexpected identity verdict %s; keeping location: player=%d scene=%d",
			identityVerdict, in.PlayerId, in.SceneId)
		return &base.Empty{}, nil
	}

	// 删 location 绑定「持有者已释放」(docs/design/location-release-binding.md §3.2):拿不到当前代的释放证据就保留
	// location、回可重试错误,并请持有节点释放;下一次进场因此走换手门,而不是按首次落点铸造新 epoch、从旧快照起步。
	release, err := l.judgeHolderRelease(in, currentLoc, identityVerdict)
	if err != nil {
		l.Logger.Errorf("Player %d leave scene %d: holder release check failed, returning error for retry: %v",
			in.PlayerId, in.SceneId, err)
		return nil, err
	}
	if !release.verdict.deletes() {
		return l.keepLocationUntilReleased(in, currentLoc, release)
	}
	mode := leaveDeleteModeAny
	if release.verdict == leaveReleaseReleased {
		mode = leaveDeleteModeReleased
	}

	outcome, err := deletePlayerLocationGuardedWithTag(l.svcCtx, in.PlayerId, raw, currentLoc.ZoneId, in.SceneId, mode, release.epoch, tag, currentRole.OwnerRevision)
	if err != nil {
		l.Logger.Errorf("Player %d leave scene %d: conditional delete failed, returning error for retry: %v",
			in.PlayerId, in.SceneId, err)
		return nil, err
	}
	if outcome == leaveOutcomeNotReleased {
		// 预读到的标记在删除之前被撤回 / 换代(R2):按没拿到证据处理,这一次不删。
		release.verdict = pendingVerdictFor(identityVerdict)
		release.waited = noteLeaveReleaseWait(l.svcCtx, in.PlayerId, leaveReleaseWaitScope(release.epoch, currentLoc),
			release.budget)
		return l.keepLocationUntilReleased(in, currentLoc, release)
	}
	if outcome != leaveOutcomeDeleted {
		// already_gone:别的调用(或 go-redis 重发的首发)已经删过并在同一段脚本里减过人数;
		// superseded:GET 之后有 EnterScene 写入了新落点(它已减过旧场景人数),或同落点重连写了新身份
		// (玩家仍在这个场景里,人数本就不该减)。两种都不动人数。
		// 只有确实删除才计释放判定；UNKNOWN和并发更新均不得被记成已释放。
		l.Logger.Infof("Player %d leave scene %d: location not deleted by this call (outcome=%s holder_release=%s), player counts untouched",
			in.PlayerId, in.SceneId, outcome, release.verdict)
		return &base.Empty{}, nil
	}

	metrics.ObserveLeaveHolderRelease(release.verdict.String())
	l.Logger.Infof("Player %d left scene %d on node %s (holder_release=%s owner_epoch=%d)",
		in.PlayerId, in.SceneId, currentLoc.NodeId, release.verdict, release.epoch)
	return &base.Empty{}, nil
}

// holderReleaseJudgement 是一次 LeaveScene 的释放判定结果。
type holderReleaseJudgement struct {
	verdict leaveReleaseVerdict
	epoch   uint64        // 当前代 E(leaveReleaseEpoch)
	zoneID  uint32        // 持有节点所在 zone(派发释放用);拿不到为 0
	waited  time.Duration // 仅等待诊断，不参与授权删除
	budget  time.Duration
}

// pendingVerdictFor:没拿到证据时按离场身份选 pending(可证明属于离场会话,发释放)或 pending_unproven(只等不发,R3)。
func pendingVerdictFor(identityVerdict leaveVerdict) leaveReleaseVerdict {
	if identityVerdict == leaveVerdictDeleted {
		return leaveReleasePending
	}
	return leaveReleasePendingUnproven
}

// judgeHolderRelease 采集 decideHolderRelease 的输入并判定。只读,唯一的写是没拿到证据时登记等待计时。
// 持有节点已死只认 playerLocationOwnerDead(etcd 注册表里正面消失 + 本副本观察 / death_at 再入屏障已过 + 不在负载集,
// 屏障拦下时的计数记在 dead_owner_takeover 站点上)。**不用** playerLocationOwnerGone:它当时把"zone 负载集为空"当整 zone 已死
// (同日 EnterScene 一侧也改为要求注册表正面证据,改后它放行的 Dead 必放行,LeaveScene 不需要整 zone 口径),
// 负载键丢失 / 被淘汰时 ZCARD 读作 0、ZSCORE 为空、也没有 death_at,会把活着的持有节点判死,于是每次登出 / 断线清理都按 any
// 模式立刻删 location,回到 §2.1 的回档路径(第二轮审计 r2-sm#3)。真正下线的 zone 在 etcd 里同样已注销,走 OwnerDead 照样能删;
// 拿不准的情形保持UNKNOWN，等待预算不授权删除。
func (l *LeaveSceneLogic) judgeHolderRelease(in *scene_manager.LeaveSceneRequest, loc *scene_manager.PlayerLocation,
	identityVerdict leaveVerdict) (holderReleaseJudgement, error) {
	budget, enabled := l.svcCtx.Config.EffectiveLeaveSceneHolderReleaseWait()
	j := holderReleaseJudgement{budget: budget}
	facts := holderReleaseFacts{enabled: enabled, identityProven: identityVerdict == leaveVerdictDeleted}
	if enabled {
		observedEpoch, err := currentOwnerEpoch(l.svcCtx, in.PlayerId)
		if err != nil {
			return j, fmt.Errorf("read owner_epoch of player %d: %w", in.PlayerId, err)
		}
		j.epoch = leaveReleaseEpoch(observedEpoch, loc)
		facts.epoch = j.epoch
		facts.noHolder = loc.GetNodeId() == ""
		j.zoneID = loc.GetZoneId()
		if j.zoneID == 0 && loc.GetSceneId() != 0 {
			j.zoneID = GetSceneZone(l.svcCtx, loc.GetSceneId())
		}
		if j.epoch != 0 && !facts.noHolder {
			owner := NewEnterSceneLogic(l.ctx, l.svcCtx)
			facts.ownerDead = owner.playerLocationOwnerDead(loc, j.zoneID)
			markerRaw, err := l.svcCtx.Redis.Get(ownerepoch.HandoffKey(in.PlayerId))
			if err != nil {
				return j, fmt.Errorf("read handoff marker of player %d: %w", in.PlayerId, err)
			}
			facts.markerReleased = markerReleasesEpoch(markerRaw, j.epoch)
			// 持有进程被同号新进程顶替(GO-3)与节点判死是同一种证据:持有这一代的进程已从注册表消失、屏障已过。
			// 不认它的话,顶替后这名玩家的清理任务永远拿不到释放证据。已经拿到判死或标记证据时不再多读一次资格。
			if !facts.ownerDead && !facts.markerReleased {
				facts.ownerDead = owner.playerLocationOwnerReplaced(in.PlayerId, loc, j.zoneID, observedEpoch,
					barrierSiteReplacedOwnerLeaveRelease) == ownerReplacedTakeover
			}
		}
	}
	j.verdict = decideHolderRelease(facts)
	if j.verdict.waits() {
		j.waited = noteLeaveReleaseWait(l.svcCtx, in.PlayerId, leaveReleaseWaitScope(j.epoch, loc), budget)
		// 等待预算仅供诊断/催促；过去多久都不是持有者已落盘释放的证据。
	}
	return j, nil
}

// keepLocationUntilReleased 处理没拿到释放证据的 LeaveScene:location 一个字节都不动,pending 时请持有节点释放,
// 回 FailedPrecondition 让 player_locator 保留清理任务重试(§3.2、§3.4)。
func (l *LeaveSceneLogic) keepLocationUntilReleased(in *scene_manager.LeaveSceneRequest, loc *scene_manager.PlayerLocation,
	j holderReleaseJudgement) (*base.Empty, error) {
	metrics.ObserveLeaveHolderRelease(j.verdict.String())
	if j.verdict == leaveReleasePending {
		// 覆盖到被清理的会话(§13):pending 只在离场身份判 deleted(location 的会话不比它新)时出现,
		// 后台请求晚于同一玩家的新登录到达时,实体已绑上更新的登录,持有节点不退它。
		sessionEndedReleaseDispatcher(l.svcCtx, j.zoneID, loc.GetNodeId(), in.PlayerId, j.epoch, identityOfLeaveRequest(in))
	}
	l.Logger.Infof("[LeaveRelease] keeping location until the holder releases: player=%d scene=%d node=%s owner_epoch=%d verdict=%s waited=%s budget=%s release_requested=%v",
		in.PlayerId, in.SceneId, loc.GetNodeId(), j.epoch, j.verdict, j.waited.Truncate(time.Second), j.budget,
		j.verdict == leaveReleasePending)
	return nil, newHolderReleasePendingError(in.PlayerId, in.SceneId, loc.GetNodeId(), j.epoch, j.verdict, j.waited)
}
