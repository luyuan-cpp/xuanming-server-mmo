package assetop

import (
	"errors"
	"math"
	"time"

	assetpb "proto/common/asset"
)

// 把一次投递的结果翻译成「调用方该做什么」(规格 §4.19 decide.go、§4.37 decide.go)。
//
// 这一层是纯函数:没有 I/O、没有时钟、没有随机数(要用时也是参数传进来),
// 所以四个结局 × 各种错误的全表可以在单测里一次跑完。

// Action 是调用方对一行待办该做的事。
type Action uint8

const (
	// ActionFinalize 结局固定且已落盘:可以终结这一行,并做对侧账。
	ActionFinalize Action = iota + 1
	// ActionAwaitDurable 结局有了但还没落盘:很短的延迟后用同一请求再查一次。
	ActionAwaitDurable
	// ActionRetry 暂时条件(冻结 / 战斗 / 背包满 / 不在线 / 传输失败):按退避重投。
	ActionRetry
	// ActionAlert 不该发生的情况(UNKNOWN、结局翻转):打日志 + 计数 + 长退避,
	// **绝不终结**。修好配置后会自动恢复,否则走人工处置。
	ActionAlert

	actionCount
)

var actionNames = [...]string{"invalid", "finalize", "await_durable", "retry", "alert"}

var _ = [1]struct{}{}[len(actionNames)-int(actionCount)]

// String 返回重排指标用的低基数名字。
func (a Action) String() string {
	if a >= actionCount {
		return actionNames[0]
	}
	return actionNames[a]
}

// Decide 给出唯一的动作。判断顺序是有讲究的:
//  1. 传输层错误优先 —— 它意味着我们根本不知道 scene 做了什么,只能重试(重试是安全的,
//     同 seq 只读答复);
//  2. 结局翻转与 UNKNOWN 单独拎出来告警,不能混进"重试"里被淹没;
//  3. 剩下的才按结局分。
//
// 未来新增结局枚举时落到 default 分支的 Alert:宁可卡住 + 告警,也不能静默当成功或重试
// (AGENTS §11.3 玩家资产路径 fail-closed)。
func Decide(res Result, err error) Action {
	if err != nil {
		if errors.Is(err, ErrOutcomeFlip) {
			return ActionAlert
		}
		return ActionRetry
	}
	switch res.Outcome {
	case assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN:
		return ActionAlert
	case assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED:
		if res.Durable {
			return ActionFinalize
		}
		return ActionAwaitDurable
	case assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY,
		assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE:
		return ActionRetry
	default:
		return ActionAlert
	}
}

// FinalStatus 把一个终结结局映射成 outbox 行的最终状态。
//
// 两处细节值得记住:
//   - Abort 打在未见过的 seq 上时 scene 回 REJECTED 且 reason==0,这叫"中止占位",
//     业务侧要**退还**先前预扣的东西;真正的业务拒绝(余额不足等)带非 0 reason。
//   - 部分发放有两个来源:本次答复的 partial,以及行上记着的上次答复 reason
//     (重投拿到的只读答复不再带 partial 标记)。漏看后者会把只发了一半的操作
//     当成全额成功入账。
//
// 只应在 Decide 返回 ActionFinalize 后调用;其它情况返回 StatusPending,
// 调用方必须当成 bug 处理而不是照着写库(reconcile.go 就是这么做的)。
func FinalStatus(rpc RPC, res Result, op Op) Status {
	switch res.Outcome {
	case assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED:
		if res.Partial || op.LastReason == ReasonPartialApplied || res.Reason == ReasonPartialApplied {
			return StatusAppliedPartial
		}
		return StatusApplied
	case assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED:
		if rpc == RPCAbort && res.Reason == 0 {
			return StatusAborted
		}
		return StatusRejected
	default:
		return StatusPending
	}
}

// NextAttemptMs 算下一次投递时刻:指数退避 + ±20% 抖动。
//
//	delay = min(base << min(attempts,16), max) × (0.8 + 0.4×rnd())
//
// 抖动是为了不让同一批被同一个 scene 拒掉的行永远挤在同一毫秒重投(惊群)。
// rnd 由调用方注入(单测要确定性);为 nil 时取 0.5,即不抖动的中值。
// 移位上限 16 只是防溢出,真正封顶的是 max。
func NextAttemptMs(nowMs uint64, attempts uint32, base, max time.Duration, rnd func() float64) uint64 {
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}
	shift := attempts
	if shift > 16 {
		shift = 16
	}
	delay := base
	if shifted := int64(base) << shift; shifted > 0 && shifted <= int64(max) {
		delay = time.Duration(shifted)
	} else {
		delay = max
	}

	jitter := 0.5
	if rnd != nil {
		jitter = rnd()
	}
	// 越界的 rnd 实现(比如返回 1.3)会把退避拉成任意值,这里夹回 [0,1)。
	if math.IsNaN(jitter) || jitter < 0 {
		jitter = 0
	} else if jitter >= 1 {
		jitter = math.Nextafter(1, 0)
	}
	scaled := float64(delay) * (0.8 + 0.4*jitter)
	return nowMs + uint64(scaled/float64(time.Millisecond))
}
