// Package lifecycle 协调 trade 进程的正常停机:注销 → 排空 RPC → 关业务资源 → 框架收尾,全程有界。
//
// 为什么需要:Linux 上 go-zero core/proc 在 init 就订阅 SIGTERM/SIGINT,默认收到信号 1s 后对 zrpc 的
// grpc.Server 调 GracefulStop、5.5s 后强杀(core/proc/shutdown.go、zrpc/internal/rpcserver.go),完全不等
// 业务侧 noderegistry 注销。etcd 一慢就变成"先停服、后注销",违反契约 §2"Close 必须先于 gRPC Stop"
// (路由服 PickRandom 不看连接状态,key 还在就会继续选中本节点)。
//
// 来源:照搬 go/chat/internal/lifecycle(2026-09-15;契约 microservice-zone-contract-20260914 §14.1 第 5 条、
// §16 记录的已活体验证做法),只去掉了错误文案里的服务名前缀(由调用方加)。这是一份临时副本:
// chat 的实现定稿提交后应抽到 go/shared,chat / trade / guild 统一替换;在那之前两份必须同改。
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
)

const (
	// HardTimeout:K8s 的 30s 宽限包含 5s preStop;进程留 1s 余量,收到信号后至多使用 24s。
	// 这是最后兜底,不能保证 SIGKILL 或硬截止时在途业务已完成。
	// 正常预算:noderegistry.Close 最坏约 15s(等 keepalive 5s + Txn 5s + Revoke 5s)
	// + DrainTimeout 5s + FinishTimeout 2s = 22s < 24s(lifecycle_test.go 钉住)。
	HardTimeout   = 24 * time.Second
	DrainTimeout  = 5 * time.Second
	FinishTimeout = 2 * time.Second
)

// Shutdown 只协调本进程拥有的资源。正常退出顺序必须是注销尝试返回、排空 RPC、
// 关闭业务资源、执行框架收尾。注销器 Close 的传输失败由其日志和租约 TTL 兜底,不能把
// Close 返回解释成 etcd 已确认删除。Stop 幂等,重复触发等待同一次收尾。
//
// 字段都可以为 nil(启动失败时部分资源尚未创建):nil 的步骤直接跳过。
// 字段必须在第一次 Stop 之前、由同一个 goroutine 填好;Stop 可并发调用。
type Shutdown struct {
	Server          *grpc.Server
	ServeDone       <-chan struct{}
	Unregister      func()
	CloseResources  func()
	FinishFramework func()
	DrainTimeout    time.Duration
	FinishTimeout   time.Duration
	once            sync.Once
	err             error
}

func (s *Shutdown) Stop() error {
	s.once.Do(func() {
		if s.Unregister != nil {
			s.Unregister()
		}
		if s.Server != nil {
			drained := make(chan struct{})
			go func() { s.Server.GracefulStop(); close(drained) }()
			timer := time.NewTimer(s.DrainTimeout)
			select {
			case <-drained:
			case <-timer.C:
				s.err = fmt.Errorf("RPC 未在 %v 内排空,已请求强制关闭连接", s.DrainTimeout)
				// grpc 的 GracefulStop 可能持锁等待不响应取消的 handler;Stop 也可能
				// 因此卡住。必须异步发起,最终由进程硬截止兜底,不能把退出线程锁死。
				go s.Server.Stop()
			}
			timer.Stop()
		}
		if s.CloseResources != nil {
			s.CloseResources()
		}

		// Linux 的 zrpc.Start 在 Serve 返回后仍等待 proc shutdown listeners;
		// Windows 的 defer 则直接再次调用 GracefulStop。两者都可能受卡住的 handler
		// 影响,所以框架收尾与 Start 返回共用一个有限预算,不在这里无限等待。
		finished := make(chan struct{})
		go func() {
			if s.FinishFramework != nil {
				s.FinishFramework()
			}
			if s.ServeDone != nil {
				<-s.ServeDone
			}
			close(finished)
		}()
		timer := time.NewTimer(s.FinishTimeout)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			s.err = errors.Join(s.err, fmt.Errorf("框架收尾或 Start 未在 %v 内退出", s.FinishTimeout))
		}
	})
	return s.err
}

// ServerSlot 在启动回调与退出线程之间转交 Server 所有权。Publish 只在 zrpc 注册回调、
// Serve 开始前调用;若退出已开始,直接 Stop 新 Server,避免 ctx 取消和 ready 同时发生时漏关。
type ServerSlot struct {
	mu      sync.Mutex
	server  *grpc.Server
	closing bool
}

func (s *ServerSlot) Publish(server *grpc.Server) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		server.Stop() // 还没 Serve,不存在等待在途 handler 的风险。
		return
	}
	s.server = server
	s.mu.Unlock()
}

func (s *ServerSlot) TakeForShutdown() *grpc.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	return s.server
}
