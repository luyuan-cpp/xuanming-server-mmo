package pkg

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// 本文件实现「跟随 gate 重定向」(scene 的 msg 124 RedirectToGateNotify)的客户端半边。
// 服务端半边见 cpp/nodes/gate/handler/event/gate_event_handler.cpp
// RedirectToGateEventHandler:它把 target_ip / target_port / token_payload /
// token_signature / token_deadline 打进 RedirectToGateNotify 推给客户端,自己**不做**
// 任何搬迁——搬迁全在客户端。login 侧的开关是 HomeZone.RedirectOnEnterEnabled,
// 它默认关闭,正是因为客户端不实现本文件这套动作就会卡死(EnterGame 那时已经回了成功
// 并清掉登录会话,老 gate 上再没有任何东西会推进这个玩家)。
//
// 契约里最容易被误解的一点:token 只认证**这条 TCP**,它不是跨区的登录会话转移。
// gate 校验的是 HMAC-SHA256(gate_token_secret, token_payload) 与
// token_signature 的常数时间相等,再解出 GateTokenPayload 校验
// gate_node_id == 本 gate、expire_timestamp > now
// (cpp/nodes/gate/handler/rpc/client_message_processor.cpp)。校验通过只是把这条连接
// 标成 verified,新 zone 的 login 那边**没有**任何会话——所以跟随重定向必须完整重跑
// Login + EnterGame,而不是"换个地址继续玩"。

// RedirectTarget 是 RedirectToGateNotify 的字段搬到 pkg 层的形状,
// 免得 pkg 反过来依赖 proto/scene(handler 负责把 notify 翻译成它)。
type RedirectTarget struct {
	IP        string
	Port      int
	Payload   []byte // GateTokenPayload 序列化结果,原样转发给目标 gate
	Signature []byte // HMAC-SHA256 十六进制签名,原样转发
	Deadline  int64  // unix 秒;0 表示 notify 没带
}

// ReloginFunc 在**新 gate 的连接上**重跑一遍 Login + EnterGame。
// 由 robot 主程序注册(它才知道认证方式 / 配置 / 指标),pkg 层只负责调用——
// 否则 pkg 要反向依赖 main 包,编译不过。
type ReloginFunc func(gc *GameClient) error

var (
	reloginMu sync.RWMutex
	relogin   ReloginFunc
)

// SetRedirectRelogin 注册重定向后的重登录实现。传 nil 等于关掉跟随能力
// (FollowRedirect 会明确报错,而不是悄悄把玩家丢在一条已经没人管的连接上)。
func SetRedirectRelogin(fn ReloginFunc) {
	reloginMu.Lock()
	relogin = fn
	reloginMu.Unlock()
}

func redirectRelogin() ReloginFunc {
	reloginMu.RLock()
	defer reloginMu.RUnlock()
	return relogin
}

const (
	// MaxRedirectHops 是一条会话允许连续跟随的重定向次数上限。
	// 配错的归属映射(A 的映射指向 B、B 的又指回 A)会让两边 gate 互相踢皮球,
	// 没有这道熔断就是一个不停重连的死循环。
	MaxRedirectHops = 3
	// redirectDialTimeout 是"目标 gate 到底通不通"的探测预算。
	// 必须探一次:vendored muduo 的 NewClient 只是起个后台拨号协程,**永不返回错误**,
	// 拨不通就每秒重试到天荒地老。没有这一探,一个写错的 target_ip 会表现为
	// "机器人静默卡住",而不是一条能看见的错误。
	redirectDialTimeout = 5 * time.Second
	// redirectVerifyTimeout 是等 ClientTokenVerifyResponse 的预算。
	redirectVerifyTimeout = 10 * time.Second
)

// ErrNoReloginFunc 表示进程没注册重登录实现(见 SetRedirectRelogin)。
var ErrNoReloginFunc = errors.New("redirect: no relogin func registered")

// Validate 只做本地可判的检查,不碰网络。
func (t RedirectTarget) Validate() error {
	if t.IP == "" {
		return errors.New("redirect: empty target_ip")
	}
	if t.Port <= 0 || t.Port > 65535 {
		return fmt.Errorf("redirect: invalid target_port %d", t.Port)
	}
	// token_deadline 是 gate 侧 expire_timestamp 的副本。已经过期就不用白跑一趟:
	// 换了连接之后 gate 必然回 token_expired 并把连接关掉,那时老连接已经没了,
	// 会话就此报废;不如在还连着老 gate 的时候失败出去。
	if t.Deadline > 0 && time.Now().Unix() >= t.Deadline {
		return fmt.Errorf("redirect: gate token already expired (deadline=%d, now=%d)",
			t.Deadline, time.Now().Unix())
	}
	return nil
}

// Addr 返回 "ip:port"。
func (t RedirectTarget) Addr() string {
	return net.JoinHostPort(t.IP, strconv.Itoa(t.Port))
}

// FollowRedirect 让 gc 真正搬到 target 上去:
//
//  1. 本地校验 target(地址、token 截止时间)、确认注册了重登录实现、检查环路熔断;
//  2. 探一次 TCP 看目标 gate 通不通(muduo 拨号永不报错,不探就只能表现为静默卡住);
//  3. 换连接:先接上新 gate,再关掉老 gate 的连接(SwapConn);
//  4. 首包发 ClientTokenVerifyRequest,payload/signature 原样来自 notify;
//  5. 在新连接上完整重跑 Login + EnterGame(token 只认证 TCP,不转移登录会话);
//  6. 把握手期间攒下的推送补投递给 RecvLoop(新 zone 的 NotifyEnterScene 通常在里面)。
//
// ⚠️ 调用者必须是 gc 自己的 RecvLoop goroutine(消息处理器天然满足)。
// 理由见 GameClient.SwapConn:阻塞在旧连接 Recv() 上的 goroutine 在连接关闭后
// 不会返回错误,而是永久阻塞——只有"读的人自己去换"才不会留下这么一个僵尸。
//
// 失败即会话作废:老连接这时已经关了,调用方唯一正确的动作是结束本次会话让外层重连。
func FollowRedirect(gc *GameClient, target RedirectTarget) error {
	if gc == nil {
		return errors.New("redirect: nil game client")
	}
	if err := target.Validate(); err != nil {
		return err
	}
	fn := redirectRelogin()
	if fn == nil {
		return ErrNoReloginFunc
	}

	gc.redirectMu.Lock()
	defer gc.redirectMu.Unlock()
	if gc.redirectHops >= MaxRedirectHops {
		return fmt.Errorf("redirect: hop limit reached (%d), refusing to follow %s",
			MaxRedirectHops, target.Addr())
	}
	gc.redirectHops++

	if err := probeTCP(target.Addr(), redirectDialTimeout); err != nil {
		return fmt.Errorf("redirect: target gate %s unreachable: %w", target.Addr(), err)
	}

	if err := gc.SwapConn(target.IP, target.Port); err != nil {
		return fmt.Errorf("redirect: reconnect to %s: %w", target.Addr(), err)
	}
	zap.L().Info("following gate redirect",
		zap.String("account", gc.Account),
		zap.Uint64("player_id", gc.PlayerId),
		zap.String("target", target.Addr()),
		zap.Int("hop", gc.redirectHops),
	)

	// payload 为空 = 服务端跑在"空密钥 dev 直通"档(gate 会直接放行),与
	// runRobotOnce 首次连接时的判据保持一致:有票据就验,没有就跳过。
	if len(target.Payload) > 0 {
		if err := runBounded(redirectVerifyTimeout, func() error {
			return gc.VerifyGateToken(target.Payload, target.Signature)
		}); err != nil {
			return fmt.Errorf("redirect: token verify on %s: %w", target.Addr(), err)
		}
	}

	if err := fn(gc); err != nil {
		return fmt.Errorf("redirect: relogin on %s: %w", target.Addr(), err)
	}

	// 重登录用的是同步 send/recv,期间到达的推送被暂存了(GameClient.DeferMessage);
	// RecvLoop 只在入口 replay 一次,所以这里必须显式补投,否则新 zone 的
	// NotifyEnterScene 会被永远埋在暂存队列里,机器人卡在"等进场"。
	gc.ReplayDeferred()
	return nil
}

// RedirectHops 返回本会话已跟随的重定向次数(测试与日志用)。
func (gc *GameClient) RedirectHops() int {
	gc.redirectMu.Lock()
	defer gc.redirectMu.Unlock()
	return gc.redirectHops
}

// probeTCP 只是"拨通就挂",用来把不可达变成一条立刻可见的错误。
func probeTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runBounded 给一个会永久阻塞的调用套上超时。
//
// 超时后那个 goroutine 会**留下**:它阻塞在 muduo 的 Recv() 上,而 vendored muduo
// 的 incoming channel 永不关闭,连 Close() 都唤不醒它。这是刻意接受的代价 ——
// 走到这里说明重定向已经失败、这条会话即将结束,泄漏一个 goroutine 换"不把整个
// 机器人永久挂住"是划算的;真正的修法在 muduo 那边(Close 时 close(incoming))。
func runBounded(timeout time.Duration, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	}
}
