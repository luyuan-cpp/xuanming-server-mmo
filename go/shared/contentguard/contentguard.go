// Package contentguard 是内容安全服务(go/content_guard)的调用端:收玩家文字的服务用它送检
// (docs/design/rename-and-content-safety.md §6.6)。
//
// 口径收在这一处,调用方不用各写一遍:
//   - 缺省严格:必须配目标(没配拒启);内容安全服务不可达、超时、回了不认识的结局,一律不放行(Unavailable 或 Reject),
//     宁可让玩家稍后再试,也不放过没检过的文字;
//   - 只有显式写了 Optional: true(本机开发)才放宽:没配目标或服务不可达时不送检、原样放行,并打 ERROR;
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
	// Optional 为真时内容安全是可有可无的:没配目标、或服务不可达时不送检,原样放行并打 ERROR。只许本机开发这样写。
	// 缺省为假(staging / prod 的 ConfigMap 不写这一键):没配目标拒启,服务不可达时文字不放行。
	Optional bool `json:",default=false"`
	// TimeoutMs 是单次送检的时限(毫秒)。判定是纯内存计算,正常在几毫秒内;时限只是兜底。
	TimeoutMs int64 `json:",default=300"`
}

// Configured 报告是否写了目标。
func (c Conf) Configured() bool {
	return len(c.Rpc.Endpoints) > 0 || c.Rpc.Target != "" || (len(c.Rpc.Etcd.Hosts) > 0 && c.Rpc.Etcd.Key != "")
}

// Validate 核对配置自身是否合理。
func (c Conf) Validate() error {
	if !c.Configured() && !c.Optional {
		return errors.New("ContentGuard.Rpc has no target (Etcd / Endpoints / Target): configure it, or set ContentGuard.Optional: true for local development only")
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
	Verdict               Verdict
	Masked                string
	Category              cgpb.TextCategory
	PendingPlatformReview bool
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

// Client 是内容安全服务的客户端。零值(没有 rpc)表示没配目标:Check 一律放行,只有 Optional 的配置才建得出这样的客户端。
type Client struct {
	rpc     cgpb.ContentGuardClient
	timeout time.Duration
	// optional 为真时调用失败按放行处理(Conf.Optional)。
	optional    bool
	lastLogUnix atomic.Int64
}

// New 按配置建客户端;service 是调用方的名字,只进日志。
// 没配目标且 Optional 时返回一个一律放行的客户端并打一行 ERROR;没配目标又不是 Optional 时返回错误(调用方拒启)。
func New(service string, conf Conf, options ...zrpc.ClientOption) (*Client, error) {
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	if !conf.Configured() {
		logx.Errorf("[contentguard] %s has no ContentGuard.Rpc target and ContentGuard.Optional is true: player text is NOT checked (local development only)", service)
		return &Client{optional: true}, nil
	}
	if conf.Optional {
		logx.Errorf("[contentguard] %s runs with ContentGuard.Optional: true: player text passes unchecked whenever the content guard is unreachable (local development only)", service)
	}
	rpc, err := zrpc.NewClient(conf.Rpc, options...)
	if err != nil {
		return nil, err
	}
	return &Client{rpc: cgpb.NewContentGuardClient(rpc.Conn()), timeout: time.Duration(conf.TimeoutMs) * time.Millisecond, optional: conf.Optional}, nil
}

// NewWithClient 用现成的 gRPC 客户端建一个严格的客户端(测试、或调用方自己管连接时用)。
func NewWithClient(rpc cgpb.ContentGuardClient, timeout time.Duration) *Client {
	return &Client{rpc: rpc, timeout: timeout}
}

// Enabled 报告是否真的会送检。
func (c *Client) Enabled() bool { return c != nil && c.rpc != nil }

type accountCtxKey struct{}

// WithAccount 在 ctx 上附带玩家的渠道账号(如 wx_<openid> / dy_<openid>),供 content_guard 路由外部平台审核接口。
func WithAccount(ctx context.Context, account string) context.Context {
	if account == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, account)
}

func accountFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	acc, _ := ctx.Value(accountCtxKey{}).(string)
	return acc
}

// Check 送检一段文字。空文字不送检,直接放行。
func (c *Client) Check(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result {
	return c.doCheck(ctx, scene, playerID, text, false)
}

// CheckWorld 送检世界频道文字,允许 content_guard 在短超时后先放行并由调用方异步调用 CheckPlatform 复核。
func (c *Client) CheckWorld(ctx context.Context, playerID uint64, text string) Result {
	return c.doCheck(ctx, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, playerID, text, true)
}

// CheckPlatform 同步调起外部平台复检(用于世界频道短超时先放行后的异步补审撤回)。
func (c *Client) CheckPlatform(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) Result {
	if text == "" || !c.Enabled() {
		return Result{Verdict: Pass}
	}
	budget := c.timeout
	if budget < 3*time.Second {
		budget = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, err := c.rpc.CheckPlatformText(callCtx, &cgpb.CheckTextRequest{
		Scene:    scene,
		Text:     text,
		PlayerId: playerID,
		Account:  accountFromContext(ctx),
	})
	if err != nil {
		c.logFailure(scene, err)
		return Result{Verdict: Unavailable}
	}
	return FromResponse(out)
}

func (c *Client) doCheck(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string, asyncPlatform bool) Result {
	if text == "" || !c.Enabled() {
		return Result{Verdict: Pass}
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out, err := c.rpc.CheckText(callCtx, &cgpb.CheckTextRequest{
		Scene:         scene,
		Text:          text,
		PlayerId:      playerID,
		Account:       accountFromContext(ctx),
		AsyncPlatform: asyncPlatform,
	})
	if err != nil {
		c.logFailure(scene, err)
		if c.optional {
			return Result{Verdict: Pass}
		}
		return Result{Verdict: Unavailable}
	}
	return FromResponse(out)
}

// FromResponse 把服务的回包换成结局。只认 PASS 与 MASKED;其余取值(含将来新增的)一律当拒绝。
// MASKED 却没带打码后的正文是服务的缺陷,同样当拒绝,不把原文放出去。
func FromResponse(out *cgpb.CheckTextResponse) Result {
	switch out.GetVerdict() {
	case cgpb.TextVerdict_TEXT_VERDICT_PASS:
		return Result{Verdict: Pass, PendingPlatformReview: out.GetPendingPlatformReview()}
	case cgpb.TextVerdict_TEXT_VERDICT_MASKED:
		if out.GetMaskedText() != "" {
			return Result{
				Verdict:               Masked,
				Masked:                out.GetMaskedText(),
				Category:              out.GetCategory(),
				PendingPlatformReview: out.GetPendingPlatformReview(),
			}
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
	if c.optional {
		logx.Errorf("[contentguard] check failed (scene=%s), passing the text UNCHECKED because ContentGuard.Optional is true: %v", scene, err)
		return
	}
	logx.Errorf("[contentguard] check failed (scene=%s), treating the text as unusable: %v", scene, err)
}
