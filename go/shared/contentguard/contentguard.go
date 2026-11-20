// Package contentguard 是内容安全服务(go/content_guard)的调用端:收玩家文字的服务用它送检
// (docs/design/rename-and-content-safety.md §6.6)。
//
// 口径收在这一处,调用方不用各写一遍:
//   - 内容安全服务不可达、超时、回了不认识的结局,一律不放行(Unavailable 或 Reject):宁可让玩家稍后再试,也不放过没检过的文字;
//   - 没配目标时(只允许本机开发,见 Conf.Required)跳过送检,一律放行;
//   - 被检文字不进日志。
package contentguard

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	cgpb "proto/content_guard"
)

// Conf 是调用端的配置,各服务挂在自己配置的 ContentGuard 段下。
type Conf struct {
	// Rpc 是内容安全服务的 zrpc 客户端配置,按 go-zero 的发现键找(Etcd.Key: contentguard.rpc);
	// 建议 NonBlock: true,内容安全服务晚起不挡本服务启动。三种目标(Etcd / Endpoints / Target)都没写 = 没配。
	Rpc zrpc.RpcClientConf `json:",optional"`
	// Required 为真时没配目标就拒绝启动。staging / prod 必须为真;本机缺省为假,没配时跳过送检并在启动时打一行警告。
	Required bool `json:",default=false"`
	// TimeoutMs 是单次送检的时限(毫秒)。判定是纯内存计算,正常在几毫秒内;时限只是兜底。
	TimeoutMs int64 `json:",default=300"`
}

// Configured 报告是否写了目标。
func (c Conf) Configured() bool {
	return len(c.Rpc.Endpoints) > 0 || c.Rpc.Target != "" || (len(c.Rpc.Etcd.Hosts) > 0 && c.Rpc.Etcd.Key != "")
}

// Validate 核对配置自身是否合理。
func (c Conf) Validate() error {
	if c.Required && !c.Configured() {
		return errors.New("ContentGuard.Required is true but ContentGuard.Rpc has no target (Etcd / Endpoints / Target)")
	}
	if c.Configured() && (c.TimeoutMs < 20 || c.TimeoutMs > 5000) {
		return errors.New("ContentGuard.TimeoutMs must be in [20, 5000]")
	}
	return nil
}

// Verdict 是送检的结局。
type Verdict uint8

const (
	// Pass 原文可用。
	Pass Verdict = iota + 1
	// Masked 用 Result.Masked 代替原文(只有聊天场景会出现)。
	Masked
	// Reject 命中屏蔽词,不可用。
	Reject
	// Unavailable 内容安全服务不可达或超时。调用方按不可用处理,回可重试的提示。
	Unavailable
)

// Result 是一次送检的结果。
type Result struct {
	Verdict  Verdict
	Masked   string
	Category cgpb.TextCategory
}

// Usable 报告这段文字能不能用(原文或打码后的)。
func (r Result) Usable() bool { return r.Verdict == Pass || r.Verdict == Masked }

// Checker 是调用方依赖的接口:生产用 *Client,测试用 Func。
type Checker interface {
	Check(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result
}

// Func 把一个函数当 Checker 用(测试替身)。
type Func func(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result

// Check 实现 Checker。
func (f Func) Check(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result {
	return f(ctx, scene, playerID, text)
}

// Client 是内容安全服务的客户端。零值(没有 rpc)表示没配目标:Check 一律放行。
type Client struct {
	rpc         cgpb.ContentGuardClient
	timeout     time.Duration
	lastLogUnix atomic.Int64
}

// New 按配置建客户端。没配目标且不要求时返回一个一律放行的客户端并打一行警告;service 是调用方的名字,只进日志。
func New(service string, conf Conf, options ...zrpc.ClientOption) (*Client, error) {
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	if !conf.Configured() {
		logx.Errorf("[contentguard] %s has no ContentGuard.Rpc target: player text is NOT checked (allowed for local development only)", service)
		return &Client{}, nil
	}
	rpc, err := zrpc.NewClient(conf.Rpc, options...)
	if err != nil {
		return nil, err
	}
	return &Client{rpc: cgpb.NewContentGuardClient(rpc.Conn()), timeout: time.Duration(conf.TimeoutMs) * time.Millisecond}, nil
}

// NewWithClient 用现成的 gRPC 客户端建(测试、或调用方自己管连接时用)。
func NewWithClient(rpc cgpb.ContentGuardClient, timeout time.Duration) *Client {
	return &Client{rpc: rpc, timeout: timeout}
}

// Enabled 报告是否真的会送检。
func (c *Client) Enabled() bool { return c != nil && c.rpc != nil }

// Check 送检一段文字。空文字不送检,直接放行。
func (c *Client) Check(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result {
	if text == "" || !c.Enabled() {
		return Result{Verdict: Pass}
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.rpc.CheckText(callCtx, &cgpb.CheckTextRequest{Scene: scene, Text: text, PlayerId: playerID})
	if err != nil {
		c.logFailure(scene, err)
		return Result{Verdict: Unavailable}
	}
	return FromResponse(out)
}

// FromResponse 把服务的回包换成结局。只认 PASS 与 MASKED;其余取值(含将来新增的)一律当拒绝。
// MASKED 却没带打码后的正文是服务的缺陷,同样当拒绝,不把原文放出去。
func FromResponse(out *cgpb.CheckTextResponse) Result {
	switch out.GetVerdict() {
	case cgpb.TextVerdict_TEXT_VERDICT_PASS:
		return Result{Verdict: Pass}
	case cgpb.TextVerdict_TEXT_VERDICT_MASKED:
		if out.GetMaskedText() != "" {
			return Result{Verdict: Masked, Masked: out.GetMaskedText(), Category: out.GetCategory()}
		}
	}
	return Result{Verdict: Reject, Category: out.GetCategory()}
}

// logFailure 记一次送检失败,每秒最多一行:内容安全服务整体不可用时,聊天的每条消息都会走到这里。
func (c *Client) logFailure(scene cgpb.TextScene, err error) {
	now := time.Now().Unix()
	last := c.lastLogUnix.Load()
	if now == last || !c.lastLogUnix.CompareAndSwap(last, now) {
		return
	}
	logx.Errorf("[contentguard] check failed (scene=%s), treating the text as unusable: %v", scene, err)
}
