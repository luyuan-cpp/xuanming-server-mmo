package scenenode

import (
	"fmt"
	"sync"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConnCache 按 endpoint("ip:port")缓存到 scene 节点的 gRPC 连接。
//
// 与 match discovery 的包级全局变量不同,这里是实例:一个进程里可能同时跑
// 生产装配与测试装配(bufconn),包级全局会让测试之间互相串连接 —— scene_manager
// 为此专门写了 ResetNodeConnCacheForTest。实例化之后 t.Cleanup(cache.Close) 就够了。
//
// 生命周期:连接由 Watcher 的 onRemove 回调在节点下线 / 换端口时 Remove;
// 进程退出前调用方负责 Close。grpc.NewClient 是惰性连接,建缓存本身不产生 I/O,
// 所以这里不需要超时,也不需要 ctx。
//
// 线程安全:所有方法可并发调用。
type ConnCache struct {
	mu    sync.RWMutex
	conns map[string]*grpc.ClientConn
}

// NewConnCache 构造空缓存。
func NewConnCache() *ConnCache {
	return &ConnCache{conns: make(map[string]*grpc.ClientConn)}
}

// Dial 返回缓存的连接或新建一条。内网 overlay,insecure transport
// (与 scene_manager defaultNodeDialer、match DialEndpoint 同口径)。
// 双检加锁:并发首拨只会有一条连接进缓存,多出来的那条立刻关掉。
func (c *ConnCache) Dial(endpoint string) (*grpc.ClientConn, error) {
	if endpoint == "" {
		// 空 endpoint 拨得出连接但永远连不上,当场报错比让 RPC 超时更省事。
		return nil, fmt.Errorf("scenenode: 空 endpoint")
	}
	c.mu.RLock()
	conn, ok := c.conns[endpoint]
	c.mu.RUnlock()
	if ok {
		return conn, nil
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", endpoint, err)
	}

	c.mu.Lock()
	if existing, ok := c.conns[endpoint]; ok {
		c.mu.Unlock()
		_ = conn.Close()
		return existing, nil
	}
	if c.conns == nil {
		c.conns = make(map[string]*grpc.ClientConn)
	}
	c.conns[endpoint] = conn
	c.mu.Unlock()

	logx.Infof("[scenenode] 已连接节点 %s", endpoint)
	return conn, nil
}

// Remove 关闭并移除某 endpoint 的缓存连接。节点下线 / 重启换端口时由 Watcher
// 的 onRemove 调用;对不存在的 endpoint 是空操作。
func (c *ConnCache) Remove(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn, ok := c.conns[endpoint]; ok {
		_ = conn.Close()
		delete(c.conns, endpoint)
		logx.Infof("[scenenode] 已移除节点连接 %s", endpoint)
	}
}

// Close 关闭并清空全部连接。进程退出 / 测试收尾调用;可重复调用。
func (c *ConnCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for endpoint, conn := range c.conns {
		_ = conn.Close()
		delete(c.conns, endpoint)
	}
}
