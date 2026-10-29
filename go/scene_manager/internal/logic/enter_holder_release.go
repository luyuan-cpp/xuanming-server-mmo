package logic

import (
	"context"
	"time"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
)

// 登录撞上未释放的持有者(docs/design/location-release-binding.md §12,收口 §6 残余 5)。
//
// gate 崩溃后玩家立刻从别的 gate 重登:旧会话的清理(断线租约到期 → LeaveScene → session_ended 释放)要等租约到期才发,
// 这期间 location 仍指向挂着僵尸实体的持有节点 X。登录被派到别的节点时,换手门要求 X 写出当前代标记,而僵尸没人通知、
// 永远不写 —— 每次重登都回 18,直到旧会话清理到期。这里让登录在换手门拒绝时直接请 X 按会话结束释放(与 LeaveScene 的
// pending 同一个 RPC):X 持有 → 按客户端断线退出、存盘落地后写 A1′ 标记;不持有 → 补写凭证。2026-10-23 起凭证带围栏
// (r2-lc#2,location-release-binding.md §3.3):X 先读 save_barrier.handoff_fence,确认是本进程实例在该 E 下登记的资格、
// 自己仍完全不持有,才以读到的原件做 CAS 写标记;拿不出资格时不写(计 credential_qualification_no_qualification 并 WARN)。
// 下一次重登拿到标记即放行并铸造。
//
// 只替「已被证明是当前最新登录」的请求去请(§12.2 的论证):
//   - 来源是 login:correlation_id == 0。scene 进程的所有 EnterScene 都经统一出口 SendCorrelatedEnterScene 带非 0 关联号,
//     它们的持有者就是发起方(换图第一跳、交接重发、跟随、疏散),收到 18 会自己冻结、存盘、写标记;
//   - 落点身份严格更新:decidePlacementIdentity 判 newer 且 location 记着代次 —— location 的会话已被这次登录取代;
//   - 请求上下文还活着:login 发这条 RPC 时持玩家锁(丢锁即取消整条链),铸代次也在锁内,此刻不存在比它更新的登录链;
//   - 同步等持有节点应答再回 18:X 的 gRPC 服务端在节点 loop 上派发、处理完才回包,应答回来 = 释放已在 X 上执行完,
//     之后任何一次重登的进场都排在它后面,不会出现「迟到的释放退掉刚重新绑上的新会话」。
//   - 请求带上这次登录的身份(covered_*,§13):超时(failed)之后释放若迟到执行,而客户端已重登、落回 X 并以同一 epoch
//     复用实体,X 看到实体绑着比它更新的登录,不退出。同步应答管「送达了」的先后,身份管「没送达、迟到了」的那一半。

// enterHolderReleaseVerdict 是 EnterScene 换手门以 18 拒绝之后的释放判定。除 eligible 外的名字同时是
// scene_manager_enter_holder_release_total 的 verdict 标签;eligible 派发后按结果记 delivered / failed。
// 底层类型 uint8 数的是结论种类数(现 7 种)。
type enterHolderReleaseVerdict uint8

const (
	// enterHolderReleaseDisabled保留旧枚举/独立策略夹具；有效配置不再关闭释放证明协议。
	enterHolderReleaseDisabled enterHolderReleaseVerdict = iota
	// enterHolderReleaseNotLogin:请求来自 scene 进程(correlation_id != 0)。持有者就是发起方,此时请它按会话结束退出
	// 等于把正在换图的玩家踢下线(第一跳还没冻结,release_fence 的「交接在途」挡不住)。这是 18 的常规来源。
	enterHolderReleaseNotLogin
	// enterHolderReleaseIdentityNotNewer:登录请求,但落点身份不是严格更新:同会话(LeaveGame 后同连接重进,gate 已代发 ExitGame)、
	// legacy(请求无代次)、location 无代次(旧版写入方留下的,证明不了它的会话比这次登录旧)。只等,不请。
	enterHolderReleaseIdentityNotNewer
	// enterHolderReleaseLegacyEpochZero:观察到的 owner_epoch 为 0。scene 对期望 0 按「不带版本」释放(release_fence
	// kReleaseUnversioned,不比对缓存),不能替它发;存量玩家的换手门本来也没有可比的代际。
	enterHolderReleaseLegacyEpochZero
	// enterHolderReleaseUnaddressable:location 没有持有节点号,或 zone 未知 —— 节点号只在 zone 内唯一,寻址不到唯一进程。
	enterHolderReleaseUnaddressable
	// enterHolderReleaseChainGone:请求上下文已结束(login 超时 / 丢锁取消链)。发起的登录链已不再持锁,可能已有更新的登录链在跑,
	// 这次不替它请。
	enterHolderReleaseChainGone
	// enterHolderReleaseEligible:满足全部条件,同步派发。
	enterHolderReleaseEligible

	enterHolderReleaseVerdictCount
)

var enterHolderReleaseVerdictNames = [...]string{
	enterHolderReleaseDisabled:         "disabled",
	enterHolderReleaseNotLogin:         "not_login",
	enterHolderReleaseIdentityNotNewer: "identity_not_newer",
	enterHolderReleaseLegacyEpochZero:  "legacy_epoch0",
	enterHolderReleaseUnaddressable:    "holder_unaddressable",
	enterHolderReleaseChainGone:        "chain_gone",
	enterHolderReleaseEligible:         "eligible",
}

// 名表长度与 enterHolderReleaseVerdictCount 不一致时编译失败(漏加 / 多加一行)。
var _ [enterHolderReleaseVerdictCount]struct{} = [len(enterHolderReleaseVerdictNames)]struct{}{}

func (v enterHolderReleaseVerdict) String() string {
	if v < enterHolderReleaseVerdictCount {
		return enterHolderReleaseVerdictNames[v]
	}
	return "unknown"
}

// eligible 派发之后的指标结果。delivered = 持有节点已应答(释放已在它的 loop 上执行完);failed = RPC 报错或超时,结果未知。
const (
	enterHolderReleaseOutcomeDelivered = "delivered"
	enterHolderReleaseOutcomeFailed    = "failed"
)

// enterHolderReleaseFacts 是 decideEnterHolderRelease 的输入,全部由调用方采好。零值取「不派发」的一侧:漏填只会少请一次,
// 玩家照旧等旧会话清理,不会误退持有者。
type enterHolderReleaseFacts struct {
	enabled           bool   // 持有者释放协议开着(EffectiveLeaveSceneHolderReleaseWait 的 enabled)
	loginRequest      bool   // correlation_id == 0
	identityNewer     bool   // identityStrictlyNewer
	epoch             uint64 // 本次请求观察到的 owner_epoch(换手门比对的同一个值)
	holderAddressable bool   // location 有持有节点号且 zone 已知
	chainAlive        bool   // 请求上下文未结束
}

// decideEnterHolderRelease 是 docs/design/location-release-binding.md §12.3 判定表,按顺序判,零副作用。
func decideEnterHolderRelease(f enterHolderReleaseFacts) enterHolderReleaseVerdict {
	switch {
	case !f.enabled:
		return enterHolderReleaseDisabled
	case !f.loginRequest:
		return enterHolderReleaseNotLogin
	case !f.identityNewer:
		return enterHolderReleaseIdentityNotNewer
	case f.epoch == 0:
		return enterHolderReleaseLegacyEpochZero
	case !f.holderAddressable:
		return enterHolderReleaseUnaddressable
	case !f.chainAlive:
		return enterHolderReleaseChainGone
	default:
		return enterHolderReleaseEligible
	}
}

// identityStrictlyNewer:落点身份裁决判 newer,**且** location 记着代次。decidePlacementIdentity 的 newer 还覆盖
// 「location 无代次而请求有」一行,那是旧版写入方留下的 location,证明不了它的会话比这次登录旧,在这里按 legacy 处理
// (与 LeaveScene 的 R3 同一口径:任一侧无身份只等不请)。
func identityStrictlyNewer(verdict placementVerdict, loc placementIdentity) bool {
	return verdict == placementAdmitNewer && loc.loginGeneration != 0
}

// enterHolderReleaseTimeout 是等持有节点应答的上限。login 调 EnterScene 的预算见 login.yaml SceneManagerRpc.Timeout(现为 10s),
// 这里只占一小段;超时按 failed 记,照常回 18,下一次重登再请。
const enterHolderReleaseTimeout = 1 * time.Second

// enterHolderReleaseRequester 是同步发 session_ended 释放的接缝,默认 RequestNodeReleaseEndedSession。
// covered 传这次登录自己的身份(不是 location 记的旧身份):登录链持锁期间不存在比它更新的登录(§12.4 (d)),
// 本节点上不比它新的那一份 —— location 上被取代的旧会话、被它取代的中间会话、经 gate 补发绑上它自己的那一份 —— 都该退出;
// 只填 location 的身份会漏掉后两种,让持有者永远不释放(§13.2)。
// 与 LeaveScene 的 sessionEndedReleaseDispatcher(后台协程)不同,这里必须**同步**返回:回 18 之前要确认释放已在持有节点执行完
// (文件头最后一条)。包级钩子只为单测替换,生产从不改它;替换的用例必须用 t.Cleanup 还原。
var enterHolderReleaseRequester = RequestNodeReleaseEndedSession

// requestHolderReleaseForLogin 在同 zone 跨节点换手门以 18 拒绝之后调用(enterscenelogic.go 第 3 步),判定并在合格时
// 同步请 location 的持有节点按会话结束释放。不写任何 Redis 状态、不改应答:换手门的结论由下一次重登读标记重新判定,
// 这里的结果只进指标与日志。loc 是本次换手门比对的那一份 location(非 nil),locZoneID 是它所在的 zone。
func (l *EnterSceneLogic) requestHolderReleaseForLogin(in *scene_manager.EnterSceneRequest, loc *scene_manager.PlayerLocation,
	locZoneID uint32, observedEpoch uint64, identityVerdict placementVerdict) {
	_, enabled := l.svcCtx.Config.EffectiveLeaveSceneHolderReleaseWait()
	locIdentity := identityOfLocation(loc)
	verdict := decideEnterHolderRelease(enterHolderReleaseFacts{
		enabled:           enabled,
		loginRequest:      in.GetCorrelationId() == 0,
		identityNewer:     identityStrictlyNewer(identityVerdict, locIdentity),
		epoch:             observedEpoch,
		holderAddressable: loc.GetNodeId() != "" && locZoneID != 0,
		chainAlive:        l.ctx.Err() == nil,
	})
	if verdict != enterHolderReleaseEligible {
		metrics.ObserveEnterHolderRelease(verdict.String())
		// scene 自发的第一跳是 18 的常规来源、开关关闭是运维决定,都不打行;其余是登录撞门却没请释放,留一行备查。
		if verdict != enterHolderReleaseNotLogin && verdict != enterHolderReleaseDisabled {
			l.Logger.Infof("[EnterHolderRelease] login rejected by handoff gate; not requesting holder release: verdict=%s player=%d owner_epoch=%d holder_zone=%d holder_node=%s request_session=%d request_generation=%d location_session=%d location_generation=%d identity=%s",
				verdict, in.PlayerId, observedEpoch, locZoneID, loc.GetNodeId(), in.SessionId, in.LoginGeneration,
				locIdentity.sessionID, locIdentity.loginGeneration, identityVerdict)
		}
		return
	}

	// 截止取 1s 与登录链剩余预算中较短的一个:登录链取消时这次请求一并取消,不在发起方已经放弃之后还去动持有者。
	ctx, cancel := context.WithTimeout(l.ctx, enterHolderReleaseTimeout)
	defer cancel()
	start := time.Now()
	if err := enterHolderReleaseRequester(ctx, l.svcCtx, locZoneID, loc.GetNodeId(), in.PlayerId, observedEpoch,
		identityOfEnterRequest(in)); err != nil {
		metrics.ObserveEnterHolderRelease(enterHolderReleaseOutcomeFailed)
		l.Logger.Errorf("[EnterHolderRelease] session-ended release to location holder failed (next login retry will resend): player=%d owner_epoch=%d holder_zone=%d holder_node=%s request_session=%d request_generation=%d location_session=%d location_generation=%d elapsed=%s err=%v",
			in.PlayerId, observedEpoch, locZoneID, loc.GetNodeId(), in.SessionId, in.LoginGeneration,
			locIdentity.sessionID, locIdentity.loginGeneration, time.Since(start), err)
		return
	}
	metrics.ObserveEnterHolderRelease(enterHolderReleaseOutcomeDelivered)
	l.Logger.Infof("[EnterHolderRelease] session-ended release delivered to location holder: player=%d owner_epoch=%d holder_zone=%d holder_node=%s request_session=%d request_generation=%d location_session=%d location_generation=%d elapsed=%s",
		in.PlayerId, observedEpoch, locZoneID, loc.GetNodeId(), in.SessionId, in.LoginGeneration,
		locIdentity.sessionID, locIdentity.loginGeneration, time.Since(start))
}
