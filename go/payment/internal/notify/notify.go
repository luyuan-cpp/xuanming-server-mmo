// Package notify 是渠道回调的 HTTP 入口:POST /notify/<渠道名>(微信小游戏的配置握手是 GET)。
//
// 一条回调的处理:验签解析(channel)→ 记账或翻转事实(core,只写本库)→ 按渠道约定应答。商城推进不在这里做,
// 由后台任务异步完成,所以应答不受商城可用性影响,能满足微信 5 秒、微信小游戏 3 秒的应答时限。
// 只在事实落库之后才回成功;落库失败回失败让渠道重发(处理是幂等的)。
package notify

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"payment/internal/channel"
	"payment/internal/core"
	"shared/authoritysql"
	"shared/drainproof"
)

// maxBodyBytes 是回调正文的上限。App Store 的通知(三张证书的 JWS 套 JWS)约十几 KB,其余渠道都在几 KB 以内。
const maxBodyBytes = 256 << 10

// handleTimeout 是一条回调落库的上限:最紧的是微信小游戏的 3 秒应答时限。
const handleTimeout = 2500 * time.Millisecond

type Server struct {
	svc      *core.Service
	relay    *drainproof.Session
	http     *http.Server
	listener net.Listener
	done     chan struct{}
}

func New(listenAddr string, svc *core.Service, relay *drainproof.Session) *Server {
	s := &Server{svc: svc, relay: relay, done: make(chan struct{})}
	s.http = &http.Server{Addr: listenAddr, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	return s
}

// Start 先监听再返回:端口占用时启动即失败,不带着一个收不到回调的进程上线。
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	s.listener = listener
	go func() {
		defer close(s.done)
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Errorf("[payment] Notification listener stopped: %v", err)
		}
	}()
	return nil
}

// Shutdown 停止接收新回调并等在途的处理完(有界)。没启动过时什么都不做。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.listener == nil {
		return nil
	}
	err := s.http.Shutdown(ctx)
	<-s.done
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/notify/")
	if !ok || name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	ch, ok := s.svc.Channel(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		if handshaker, ok := ch.(channel.Handshaker); ok && handshaker.Handshake(w, r) {
			return
		}
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	finish, err := s.relay.Enter()
	if err != nil {
		// 正在排空:回失败,渠道稍后重发,由接手的副本处理。
		ch.WriteAck(w, channel.AckRetry)
		core.ObserveNotification(ch.Name(), "retry")
		return
	}
	defer finish()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		ch.WriteAck(w, channel.AckRejected)
		core.ObserveNotification(ch.Name(), "rejected")
		return
	}
	note, err := ch.ParseNotification(r, body)
	if err != nil {
		if !errors.Is(err, channel.ErrBadSignature) {
			logx.WithContext(r.Context()).Errorf("[payment] Parsing a %s notification failed: %v", ch.Name(), err)
		}
		ch.WriteAck(w, channel.AckRejected)
		core.ObserveNotification(ch.Name(), "rejected")
		return
	}
	// 渠道断开连接也把这一次处理做完:处理是幂等的,做完下一次重发就是重复报告。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), handleTimeout)
	defer cancel()
	if err := s.svc.HandleNotification(ctx, ch, note); err != nil {
		if authoritysql.ClassifyOutcome(err) == authoritysql.OutcomeUnknown {
			s.relay.Hold()
		}
		logx.WithContext(ctx).Errorf("[payment] Handling a %s notification failed, asking the channel to resend: %v", ch.Name(), err)
		ch.WriteAck(w, channel.AckRetry)
		core.ObserveNotification(ch.Name(), "retry")
		return
	}
	ch.WriteAck(w, channel.AckOK)
	core.ObserveNotification(ch.Name(), "handled")
}
