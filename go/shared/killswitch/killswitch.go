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
// # etcd 布局(与 C++ 节点同一套,docs/design/rpc-kill-switch.md §4)
//
//	/mmorpg/killswitch/login.LoginService/Login                精确到方法
//	/mmorpg/killswitch/LoginService/*                          整个服务(短名亦可)
//	/mmorpg/killswitch/*                                       全局
//	/mmorpg/killswitch/type/LoginNodeService/<同上目标>         只对这一类节点(eNodeType 名)
//	/mmorpg/killswitch/instance/<节点 uuid>/<同上目标>          只对这一个进程
//
// 值的两种写法见 ParseRule。匹配顺序:范围「实例 → 类型 → 全局」,同一范围里按 MatchKeys「方法 → 服务 → *」,
// 第一条命中的规则说了算 —— 更具体的规则写 {"deny":false} 可以把自己从更宽的规则里豁免出来。
// 受保护的方法(Config.Protected:会话收尾、补偿与回执)只认精确到方法的规则,`*` 与 `服务/*` 关不掉。
//
// # 用法
//
//	ks := killswitch.New(killswitch.Config{NodeTypes: []string{"ChatNodeService"}})
//	ks.Start(ctx, etcdClient)                 // 非阻塞;cli 为 nil 就是永远放行
//	server.AddUnaryInterceptors(ks.UnaryServerInterceptor())
//	nr, _ := noderegistry.Register(...)       // 注册成功后登记本进程 uuid
//	ks.AddInstance(nr.NodeUUID)
package killswitch

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"shared/etcdwatch"
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

	// NodeTypes 是本进程注册的节点类型(eNodeType 枚举名,如 "ChatNodeService";用 base.ENodeType_name 取,不手写),
	// 认 type/<名>/ 下的规则。一个进程注册多种类型时(match = MatchNodeService + TeamNodeService)都写上。
	// 留空 = 不认类型范围,只认全局与实例规则。
	NodeTypes []string

	// Protected 是受保护的方法全名("/pkg.Svc/Method",用生成的 *_FullMethodName 常量):只认精确到方法的规则,
	// `*` 与 `服务/*` 关不掉(docs/design/rpc-kill-switch.md §8)。放会话收尾、补偿与回执这类「在途工作的收尾」——
	// 热关停只挡新工作,挡住它们会让在途工作收不了尾。
	Protected []string
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

// minRecheckInterval 是定期复核间隔的下限,只防 StaleAfter 配得极小时把 etcd 打成忙循环。
const minRecheckInterval = 100 * time.Millisecond

// recheckInterval 是 watch 最多连续跑多久就回去全量读一次(watch.go)。快照的 syncedAt 只在真正读到 etcd 时刷新,
// 所以取 StaleAfter 的三分之一:与 etcd 失联满 StaleAfter 才作废,在那之前至少还有两次全量读的机会。
// StaleAfter 为负(永不作废)时只需要兜底对账,用 etcdwatch.ResyncInterval。
func (c Config) recheckInterval() time.Duration {
	after := c.staleAfter()
	if after < 0 {
		return etcdwatch.ResyncInterval
	}
	return max(after/3, minRecheckInterval)
}

// ruleSet 是一份**不可变**的规则快照。发布后绝不原地修改,
// 只整体替换 —— 这是读路径可以零锁的前提。
type ruleSet struct {
	rules map[string]Rule
	// syncedAt 是这份快照对应的最后一次成功同步时刻。
	syncedAt time.Time
}

// identity 是本进程在范围规则里的身份,发布后不原地修改(读路径零锁)。
// layers 按优先级排好:实例范围的键前缀 → 类型范围的键前缀 → 全局("")。
type identity struct {
	nodeTypes []string
	instances []string
	layers    [][]string
}

func newIdentity(nodeTypes, instances []string) *identity {
	id := &identity{nodeTypes: nodeTypes, instances: instances}
	instanceLayer := make([]string, 0, len(instances))
	for _, uuid := range instances {
		instanceLayer = append(instanceLayer, InstanceScopePrefix(uuid))
	}
	typeLayer := make([]string, 0, len(nodeTypes))
	for _, nodeType := range nodeTypes {
		typeLayer = append(typeLayer, TypeScopePrefix(nodeType))
	}
	id.layers = [][]string{instanceLayer, typeLayer, {""}}
	return id
}

// applies 判断一条规则(相对键)的范围是不是本进程。范围段残缺的键不是规则,恒为 false。
func (id *identity) applies(pattern string) bool {
	kind, value, _, ok := splitScope(pattern)
	if !ok {
		return false
	}
	switch kind {
	case scopeNodeType:
		return slices.Contains(id.nodeTypes, value)
	case scopeInstance:
		return slices.Contains(id.instances, value)
	default:
		return true
	}
}

// Switch 是一个规则快照的持有者 + gRPC 拦截器工厂。
// 零值不可用,用 New 构造。
type Switch struct {
	cfg  Config
	snap atomic.Pointer[ruleSet]
	// id 是本进程身份;AddInstance 整体替换。idMu 只串行化写者。
	id   atomic.Pointer[identity]
	idMu sync.Mutex
	// protected 是受保护的方法全名(带开头斜杠),构造后不变。
	protected map[string]struct{}
	// now 便于测试注入时钟,生产恒为 time.Now。
	now func() time.Time
}

// New 构造一个 Switch。**立刻可用**:此时规则为空,即全部放行。
// 是否连 etcd、连不连得上,都不影响它被挂进拦截器链。
func New(cfg Config) *Switch {
	s := &Switch{cfg: cfg, now: time.Now, protected: make(map[string]struct{}, len(cfg.Protected))}
	for _, method := range cfg.Protected {
		s.protected["/"+strings.TrimPrefix(method, "/")] = struct{}{}
	}
	s.id.Store(newIdentity(slices.Clone(cfg.NodeTypes), nil))
	return s
}

// AddInstance 登记本进程的一个节点 uuid(noderegistry 注册成功后调用),此后 instance/<uuid>/ 下的规则对本进程生效。
// 一个进程注册多种节点类型时每个 uuid 调一次;重复与空串忽略。可以在任意 goroutine 调。
func (s *Switch) AddInstance(nodeUUID string) {
	if nodeUUID == "" {
		return
	}
	s.idMu.Lock()
	defer s.idMu.Unlock()
	cur := s.id.Load()
	if slices.Contains(cur.instances, nodeUUID) {
		return
	}
	s.id.Store(newIdentity(cur.nodeTypes, append(slices.Clone(cur.instances), nodeUUID)))
	if rs := s.snap.Load(); rs != nil {
		setRuleCount(s.applicableCount(rs.rules))
	}
}

// applicableCount 是适用于本进程的规则条数(含 deny=false 的豁免规则),供 killswitch_rules 指标用:
// 别的节点类型、别的进程的规则不计,否则一条孤儿实例规则会让全网的指标都显示「有止血阀开着」。
func (s *Switch) applicableCount(rules map[string]Rule) int {
	id := s.id.Load()
	n := 0
	for pattern := range rules {
		if id.applies(pattern) {
			n++
		}
	}
	return n
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
	setRuleCount(s.applicableCount(cp))
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

	keys := MatchKeys(fullMethod)
	if s.isProtected(fullMethod) {
		keys = ExactKeys(fullMethod) // 受保护的方法:通配规则关不掉
	}
	// 范围「实例 → 类型 → 全局」,同一范围里「方法 → 服务 → *」;命中即定谳,
	// 更精确的规则可以用 deny:false 显式豁免更宽的规则。
	for _, layer := range s.id.Load().layers {
		for _, key := range keys {
			if r, found := lookupLayer(rs.rules, layer, key); found {
				return r, r.Deny
			}
		}
	}
	return Rule{}, false
}

// lookupLayer 在同一范围层(本进程的若干个 uuid,或若干种节点类型)里找 key。
// 同一层里本进程的两个身份写了相反的规则时取关停。
func lookupLayer(rules map[string]Rule, layer []string, key string) (Rule, bool) {
	var allow Rule
	found := false
	for _, scopePrefix := range layer {
		r, ok := rules[scopePrefix+key]
		if !ok {
			continue
		}
		if r.Deny {
			return r, true
		}
		allow, found = r, true
	}
	return allow, found
}

func (s *Switch) isProtected(fullMethod string) bool {
	if len(s.protected) == 0 {
		return false
	}
	_, ok := s.protected["/"+strings.TrimPrefix(fullMethod, "/")]
	return ok
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

// Deny 与 Lookup 判定相同,被关停时另记一次 killswitch_blocked_total。
// 给拦截器之外的判定点用:路由服按转发的目标方法判(docs/design/rpc-kill-switch.md §7)。
func (s *Switch) Deny(fullMethod string) (Rule, bool) {
	rule, blocked := s.Lookup(fullMethod)
	if blocked {
		incBlocked(fullMethod)
	}
	return rule, blocked
}

// UnaryServerInterceptor 返回关停拦截器。命中就短路,handler 根本不会被调用。
func (s *Switch) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if rule, blocked := s.Deny(info.FullMethod); blocked {
			return nil, status.Error(rule.StatusCode(), denyMessage(info.FullMethod, rule))
		}
		return handler(ctx, req)
	}
}

// denyMessage 拼给调用方看的错误文本(英文,会进调用方日志;与 C++ kill_switch::DenyMessage 同一格式)。
// 刻意带上方法名与原因:客户端日志里能直接看出"是被关停了,不是超时"。
func denyMessage(fullMethod string, rule Rule) string {
	msg := "killswitch: method " + fullMethod + " is disabled"
	if rule.Reason != "" {
		msg += ", reason: " + rule.Reason
	}
	return msg
}
