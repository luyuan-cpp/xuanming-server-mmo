// Package killswitch 提供 RPC 级热关停:不改代码、不重启进程,
// 往 etcd 里写一个 key 就能把某个方法 / 某个服务 / 全部方法立刻短路掉。
//
// # 用途
//
// 线上出事时最缺的是"止血阀":某个方法把 DB 打爆了、某条新链路在
// 灰度里疯狂重试、某个接口正在放大故障 —— 这时需要的是秒级关掉它,
// 而不是走一遍构建-发布-滚动更新。
//
// # 铁律:fail-open
//
// 下面每一种情况都必须**放行**,一条都不能例外:
//
//   - 没配 etcd 客户端 / 没调用 Start
//   - etcd 连不上、Get 失败、Watch 断了
//   - 前缀下没有任何 key
//   - key 的值解析失败(运维手抖写坏了)
//   - 没有任何规则匹配上当前方法
//
// 开关系统自身故障绝不能拖垮业务。为此:
//   - 规则快照放在 atomic.Pointer 里,读路径零锁、零阻塞、绝不返回 error;
//   - 与 etcd 失联超过 Config.StaleAfter 后,本地规则**主动作废**,
//     整体退回全放行(见 Config.StaleAfter 的取舍说明)。
//
// # etcd 布局
//
//	/mmorpg/killswitch/login.LoginService/Login   精确到方法
//	/mmorpg/killswitch/LoginService/*             整个服务(短名亦可)
//	/mmorpg/killswitch/*                          全局
//
// 值的两种写法见 ParseRule。匹配优先级见 MatchKeys ——
// 精确规则写 {"deny":false} 可以把自己从 Service/* 或 * 里豁免出来。
//
// # 用法
//
//	ks := killswitch.New(killswitch.Config{})
//	ks.Start(ctx, etcdClient)                 // 非阻塞;cli 为 nil 就是永远放行
//	server.AddUnaryInterceptors(ks.UnaryServerInterceptor())
package killswitch

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// DefaultPrefix 是规则在 etcd 里的默认前缀。
const DefaultPrefix = "/mmorpg/killswitch/"

const (
	// defaultStaleAfter 见 Config.StaleAfter。
	defaultStaleAfter = time.Minute
	// defaultResyncBackoff 是全量同步失败后的重试间隔,
	// 与 scene_manager LoadReporter 的 3s 对齐。
	defaultResyncBackoff = 3 * time.Second
)

// Config 配置一个 Switch。零值可用。
type Config struct {
	// Prefix etcd 前缀,留空用 DefaultPrefix。
	Prefix string

	// StaleAfter 与 etcd 失联多久之后**主动作废**本地规则快照。
	// 留 0 用 1 分钟;负数表示永不作废。
	//
	// 取舍:铁律说"etcd 连不上就放行",但一断线立刻清空会让网络抖一下
	// 就把正在生效的止血阀松开(flap)。所以这里给一个宽限期:
	// 抖动期间保持现状,真的长时间失联再退回全放行。
	StaleAfter time.Duration

	// ResyncBackoff 全量同步失败后的重试间隔,留 0 用 3s。
	ResyncBackoff time.Duration
}

func (c Config) prefix() string {
	if c.Prefix == "" {
		return DefaultPrefix
	}
	return c.Prefix
}

func (c Config) staleAfter() time.Duration {
	if c.StaleAfter == 0 {
		return defaultStaleAfter
	}
	return c.StaleAfter
}

func (c Config) resyncBackoff() time.Duration {
	if c.ResyncBackoff <= 0 {
		return defaultResyncBackoff
	}
	return c.ResyncBackoff
}

// ruleSet 是一份**不可变**的规则快照。发布后绝不原地修改,
// 只整体替换 —— 这是读路径可以零锁的前提。
type ruleSet struct {
	rules map[string]Rule
	// syncedAt 是这份快照对应的最后一次成功同步时刻。
	syncedAt time.Time
}

// Switch 是一个规则快照的持有者 + gRPC 拦截器工厂。
// 零值不可用,用 New 构造。
type Switch struct {
	cfg  Config
	snap atomic.Pointer[ruleSet]
	// now 便于测试注入时钟,生产恒为 time.Now。
	now func() time.Time
}

// New 构造一个 Switch。**立刻可用**:此时规则为空,即全部放行。
// 是否连 etcd、连不连得上,都不影响它被挂进拦截器链。
func New(cfg Config) *Switch {
	return &Switch{cfg: cfg, now: time.Now}
}

// SetRules 直接替换规则快照。
//
// 给两类调用方用:静态配置(不接 etcd 也想用本包)、以及单元测试。
// key 是匹配模式(见 MatchKeys),不是 etcd 的完整 key。
func (s *Switch) SetRules(rules map[string]Rule) {
	s.setRules(rules, s.now())
}

func (s *Switch) setRules(rules map[string]Rule, syncedAt time.Time) {
	cp := make(map[string]Rule, len(rules))
	for k, v := range rules {
		cp[k] = v
	}
	s.snap.Store(&ruleSet{rules: cp, syncedAt: syncedAt})
	setRuleCount(len(cp))
}

// Rules 返回当前快照的一份拷贝,供诊断接口 / 测试读。
func (s *Switch) Rules() map[string]Rule {
	rs := s.snap.Load()
	if rs == nil {
		return map[string]Rule{}
	}
	cp := make(map[string]Rule, len(rs.rules))
	for k, v := range rs.rules {
		cp[k] = v
	}
	return cp
}

// Lookup 判断一个 gRPC 全方法名是否被关停。
//
// 零锁、零分配(未配置规则时)、绝不返回 error —— 读路径的全部要求。
// 第二个返回值为 true 才表示"关停",其余一切情况都是放行。
func (s *Switch) Lookup(fullMethod string) (Rule, bool) {
	rs := s.snap.Load()
	// 未配置 / 从没同步成功过 → 放行。
	if rs == nil || len(rs.rules) == 0 {
		return Rule{}, false
	}
	// 快照过期(与 etcd 长时间失联)→ 放行。
	if s.isStale(rs, s.now()) {
		return Rule{}, false
	}

	for _, key := range MatchKeys(fullMethod) {
		if r, ok := rs.rules[key]; ok {
			// 命中即定谳:更精确的规则可以用 deny:false 显式豁免更宽的规则。
			return r, r.Deny
		}
	}
	return Rule{}, false
}

// isStale 判断快照是否已因长时间失联而作废。
func (s *Switch) isStale(rs *ruleSet, now time.Time) bool {
	after := s.cfg.staleAfter()
	if after < 0 {
		return false // 显式要求永不作废
	}
	if rs.syncedAt.IsZero() {
		return false // 静态配置(SetRules 之外的路径没填),不作废
	}
	return now.Sub(rs.syncedAt) > after
}

// UnaryServerInterceptor 返回关停拦截器。命中就短路,handler 根本不会被调用。
func (s *Switch) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if rule, blocked := s.Lookup(info.FullMethod); blocked {
			incBlocked(info.FullMethod)
			return nil, status.Error(rule.StatusCode(), denyMessage(info.FullMethod, rule))
		}
		return handler(ctx, req)
	}
}

// denyMessage 拼给调用方看的错误文本。
// 刻意带上方法名与原因:客户端日志里能直接看出"是被关停了,不是超时"。
func denyMessage(fullMethod string, rule Rule) string {
	msg := "killswitch: 方法 " + fullMethod + " 已被热关停"
	if rule.Reason != "" {
		msg += ",原因: " + rule.Reason
	}
	return msg
}
