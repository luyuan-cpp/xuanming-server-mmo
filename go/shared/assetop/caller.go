package assetop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"

	"shared/scenenode"
)

// 一次投递 + durable 重查(规格 §4.19 caller.go、§4.37 caller.go)。
//
// 为什么要重查:scene 不在 gRPC 线程上等存盘 —— 生成器模板在 future.get() 之后没有守护段,
// 硬塞等待会被重生成吞掉。于是 scene 应用完立刻发起写 Redis,响应里如实报 durable;
// Go 用**同一个请求**按 100/200/400ms 再查几次。重查对 Debit/Credit 都安全:
// scene 见过的 seq 只读答复,不会重办;真丢了状态(崩溃)那这次重调就是唯一的一次应用。

// DefaultCallTimeout 是单次资产 RPC 的超时。
// 它 + DefaultRequery 的总和(800 + 700ms)要留在帮会同步写 RPC 的 2500ms 预算内。
const DefaultCallTimeout = 800 * time.Millisecond

// DefaultRequery 是 durable 重查的等待序列。实测 Redis SET 是毫秒级,一般第二次就拿到。
var DefaultRequery = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
}

// Resolver 把 player_id 解析成一个可直接调用的 scene 目标。
// 生产实现是 shared/scenenode.Locator;单测注入假的。
type Resolver interface {
	Resolve(ctx context.Context, playerID uint64) (scenenode.Target, error)
}

// Caller 负责「把一条资产指令投出去,并拿回一个可信的结局」。
// 构造后只读,可被多 goroutine 共享。
type Caller struct {
	// Resolver 必填。
	Resolver Resolver
	// Signer 必填:没有签名的请求会被 scene 判 UNKNOWN 且不记账,行会一直卡着,
	// 所以这里直接在发包前失败(见 ErrNoSigner)。
	Signer *Signer
	// CallTimeout 单次 RPC 超时;<=0 用 DefaultCallTimeout。
	// ctx 本身的截止时间更早时以 ctx 为准(context.WithTimeout 取二者较早者)。
	CallTimeout time.Duration
	// Requery 为 nil 时用 DefaultRequery;显式给空切片表示不重查(只有测试会这么做)。
	Requery []time.Duration
	// Metrics 可为 nil。
	Metrics *Metrics
	// Now 只为测试注入;为 nil 时用 time.Now。签名时间戳与耗时统计都取它。
	Now func() time.Time
}

func (c *Caller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Caller) requeryDelays() []time.Duration {
	if c.Requery == nil {
		return DefaultRequery
	}
	return c.Requery
}

// Do 投递一次资产指令并返回结局。
//
// 契约:
//   - 返回 err == nil 时,Result 一定是可以交给 Decide 的结论(可能是未 durable 的中间态);
//   - 玩家无位置 / 节点未注册时返回 Local 的 NOT_HERE,**不是错误**:这是常态(玩家离线);
//   - 传输层错误原样返回,调用方按重试处理;
//   - 同一 seq 的结局在两次查询之间变了,返回 ErrOutcomeFlip(违反不变量 I2,必须告警)。
//
// req 不会被就地修改:每次实际发包都克隆一份再签名。
func (c *Caller) Do(ctx context.Context, rpc RPC, req *assetpb.AssetOpRequest) (Result, error) {
	if req == nil {
		return Result{}, errors.New("assetop: 空请求")
	}
	if c.Resolver == nil {
		return Result{}, errors.New("assetop: Caller 缺少 Resolver")
	}
	if c.Signer == nil {
		return Result{}, ErrNoSigner
	}
	if rpc >= rpcCount || rpc == 0 {
		return Result{}, fmt.Errorf("assetop: 未知的资产 RPC %d", uint8(rpc))
	}

	target, err := c.Resolver.Resolve(ctx, req.GetPlayerId())
	if err != nil {
		if errors.Is(err, scenenode.ErrNotOnline) || errors.Is(err, scenenode.ErrNodeUnknown) {
			// 不在线 / 节点还没注册:本地合成 NOT_HERE。不记账、不终结,交给重投循环退避。
			c.Metrics.incRPCNoLocation(req.GetStream(), rpc)
			return Result{
				Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE,
				Local:   true,
			}, nil
		}
		return Result{}, err
	}

	res, err := c.invoke(ctx, target, rpc, req)
	if err != nil {
		return Result{}, err
	}

	// scene 说人不在这儿:位置可能刚过期。重新定位,**只有换了节点**才重调一次,
	// 同节点重调只会得到同样的答复(还多占一条 poller 线程)。
	if res.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE {
		next, rerr := c.Resolver.Resolve(ctx, req.GetPlayerId())
		if rerr == nil && next.Endpoint != target.Endpoint {
			res2, err2 := c.invoke(ctx, next, rpc, req)
			if err2 != nil {
				return Result{}, err2
			}
			target, res = next, res2
		}
	}

	if !isSceneTerminal(res.Outcome) || res.Durable {
		return res, nil
	}
	return c.requery(ctx, target, rpc, req, res)
}

// requery 用同一请求重查,直到拿到 durable、预算用完或出错。
// 任何一种「没等到」都返回**最后一次**结果且 err==nil:上层会按 AwaitDurable 重排,
// 而不是把一个未落盘的结局当成终结。
func (c *Caller) requery(ctx context.Context, target scenenode.Target, rpc RPC, req *assetpb.AssetOpRequest, last Result) (Result, error) {
	for _, wait := range c.requeryDelays() {
		if err := sleepCtx(ctx, wait); err != nil {
			c.Metrics.incRequery(rpc, "timeout")
			return last, nil
		}
		res, err := c.invoke(ctx, target, rpc, req)
		if err != nil {
			c.Metrics.incRequery(rpc, "error")
			return last, nil
		}
		if !isSceneTerminal(res.Outcome) {
			// 重查期间玩家换了节点 / 账本被判损坏之类:旧答复仍然有效(结局固定),
			// 但这次拿不到落盘证据。按「没等到」处理,绝不当成结局翻转。
			c.Metrics.incRequery(rpc, "timeout")
			return last, nil
		}
		if res.Outcome != last.Outcome {
			c.Metrics.incOutcomeFlip(req.GetStream())
			return res, fmt.Errorf("%w (stream=%d seq=%d %s -> %s)",
				ErrOutcomeFlip, req.GetStream(), req.GetSeq(),
				outcomeLabel(last.Outcome), outcomeLabel(res.Outcome))
		}
		last = res
		if res.Durable {
			c.Metrics.incRequery(rpc, "durable")
			return res, nil
		}
	}
	c.Metrics.incRequery(rpc, "timeout")
	return last, nil
}

// invoke 发一次包:克隆 → 现签 → 调对应方法 → 转成 Result。
//
// 每次都重签的原因:签名带时间戳,scene 只接受 ±300s 的窗口;重查若复用旧签名,
// 慢路径上就会被判验签失败。
func (c *Caller) invoke(ctx context.Context, target scenenode.Target, rpc RPC, req *assetpb.AssetOpRequest) (Result, error) {
	if target.Client == nil {
		return Result{}, fmt.Errorf("assetop: 定位结果没有可用客户端(endpoint=%s)", target.Endpoint)
	}

	signed, ok := proto.Clone(req).(*assetpb.AssetOpRequest)
	if !ok {
		return Result{}, errors.New("assetop: 克隆请求失败")
	}
	start := c.now()
	c.Signer.Sign(rpc, signed, uint64(start.UnixMilli()))

	timeout := c.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		resp *assetpb.AssetOpResponse
		err  error
	)
	switch rpc {
	case RPCDebit:
		resp, err = target.Client.AssetDebit(callCtx, signed)
	case RPCCredit:
		resp, err = target.Client.AssetCredit(callCtx, signed)
	case RPCAbort:
		resp, err = target.Client.AssetAbortDebit(callCtx, signed)
	default:
		return Result{}, fmt.Errorf("assetop: 未知的资产 RPC %d", uint8(rpc))
	}

	elapsed := c.now().Sub(start).Seconds()
	if err != nil {
		c.Metrics.observeRPC(req.GetStream(), rpc, outcomeLabelError, elapsed)
		return Result{}, fmt.Errorf("assetop: 调用 %s 失败(endpoint=%s seq=%d): %w",
			rpc.String(), target.Endpoint, req.GetSeq(), err)
	}

	res := Result{
		Outcome: resp.GetOutcome(),
		Reason:  resp.GetReason().GetId(),
		Durable: resp.GetDurable(),
		Partial: resp.GetPartial(),
	}
	c.Metrics.observeRPC(req.GetStream(), rpc, outcomeLabel(res.Outcome), elapsed)
	if res.Partial {
		c.Metrics.incPartial(req.GetStream())
	}
	return res, nil
}

// isSceneTerminal 报告这个结局是否「已记账」。RETRY / NOT_HERE / UNKNOWN 永不记账。
func isSceneTerminal(o assetpb.AssetOpOutcome) bool {
	return o == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED ||
		o == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED
}

// sleepCtx 等 d,或在 ctx 结束时提前返回它的错误。定时器一定会被释放。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
