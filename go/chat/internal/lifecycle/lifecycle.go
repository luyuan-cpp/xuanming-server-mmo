package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
)

const (
	// K8s 的 30s 宽限包含 5s preStop;进程留 1s 余量,收到信号后至多使用 24s。
	// 这是最后兜底,不能保证 SIGKILL 或硬截止时在途业务已完成。
	HardTimeout   = 24 * time.Second
	DrainTimeout  = 5 * time.Second
	FinishTimeout = 2 * time.Second
)

// Shutdown 只协调本进程拥有的资源。正常退出顺序必须是注销尝试返回、排空RPC、
// 关闭业务资源、执行框架收尾。注销器 Close 的传输失败由其日志和租约TTL兜底,不能把
// Close 返回解释成 etcd 已确认删除。Stop 幂等,重复触发等待同一次收尾。
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
				s.err = fmt.Errorf("chat: RPC 未在 %v 内排空,已请求强制关闭连接", s.DrainTimeout)
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
		// Windows 的 defer 则直接再次调用 GracefulStop。两者都可能受卡住的handler
		// 影响,所以框架收尾与Start返回共用一个有限预算,不在这里无限等待。
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
			s.err = errors.Join(s.err, fmt.Errorf("chat: 框架收尾或Start未在 %v 内退出", s.FinishTimeout))
		}
	})
	return s.err
}

// ServerSlot 在启动回调与退出线程之间转交Server所有权。Publish只在zrpc注册回调、
// Serve开始前调用;若退出已开始,直接Stop新Server,避免ctx取消和ready同时发生时漏关。
type ServerSlot struct {
	mu      sync.Mutex
	server  *grpc.Server
	closing bool
}

func (s *ServerSlot) Publish(server *grpc.Server) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		server.Stop() // 还没Serve,不存在等待在途handler的风险。
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
