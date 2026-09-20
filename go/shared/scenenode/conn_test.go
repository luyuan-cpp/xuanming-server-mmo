package scenenode

import (
	"sync"
	"testing"

	"google.golang.org/grpc"
)

// ConnCache 是本包唯一重写(而非从 match discovery 逐字复制)的组件:包级全局
// 变量改成实例、加了双检加锁与空 endpoint 守卫。AGENTS §11.4 要求按改动风险覆盖
// 失败 / 幂等 / 并发,而这四条契约全部能离线断言 —— grpc.NewClient 是惰性的,
// 建连接不产生 I/O,所以这些用例都**不需要起 listener**。
//
// 用的是一个不会有人监听的回环地址:即使 gRPC 日后改成后台重连,也只是连不上,
// 不会让用例变慢或不稳定。
const testEndpointA = "127.0.0.1:59201"
const testEndpointB = "127.0.0.1:59202"

// 空 endpoint 拨得出连接但永远连不上。当场报错,且不能污染缓存 ——
// 否则后续对空串的调用会拿到一条永远超时的连接。
func TestDialRejectsEmptyEndpoint(t *testing.T) {
	c := NewConnCache()
	t.Cleanup(c.Close)

	conn, err := c.Dial("")
	if err == nil {
		t.Fatal("空 endpoint 必须报错")
	}
	if conn != nil {
		t.Fatalf("报错时不得返回连接,实得 %v", conn)
	}
	if n := len(c.conns); n != 0 {
		t.Fatalf("失败的拨号不得进缓存,实得 %d 条", n)
	}
}

// 幂等:同一 endpoint 反复 Dial 只建一条连接。每次新建会把 scene 节点的连接数
// 按调用次数放大,最终耗尽文件描述符。
func TestDialReusesCachedConn(t *testing.T) {
	c := NewConnCache()
	t.Cleanup(c.Close)

	first, err := c.Dial(testEndpointA)
	if err != nil {
		t.Fatalf("首次拨号失败: %v", err)
	}
	second, err := c.Dial(testEndpointA)
	if err != nil {
		t.Fatalf("二次拨号失败: %v", err)
	}
	if first != second {
		t.Fatal("同一 endpoint 必须复用同一条连接")
	}

	other, err := c.Dial(testEndpointB)
	if err != nil {
		t.Fatalf("另一 endpoint 拨号失败: %v", err)
	}
	if other == first {
		t.Fatal("不同 endpoint 不得共用连接")
	}
}

// Remove 是 Watcher onRemove 的落点:节点下线 / 重启换端口后,旧连接必须被丢弃,
// 下一次 Dial 要拿到新的一条 —— 否则会一直往已经不存在的进程发资产 RPC。
func TestRemoveDropsConnAndIsIdempotent(t *testing.T) {
	c := NewConnCache()
	t.Cleanup(c.Close)

	first, err := c.Dial(testEndpointA)
	if err != nil {
		t.Fatalf("首次拨号失败: %v", err)
	}
	c.Remove(testEndpointA)

	second, err := c.Dial(testEndpointA)
	if err != nil {
		t.Fatalf("移除后重新拨号失败: %v", err)
	}
	if second == first {
		t.Fatal("Remove 之后必须建新连接")
	}

	// 对不存在的 endpoint 是空操作:onRemove 可能对同一节点回调两次
	// (watch 事件重放 / fullSync 与 delete 事件重叠),不能 panic。
	c.Remove("127.0.0.1:59999")
	c.Remove(testEndpointA)
	c.Remove(testEndpointA)
}

// Close 可重复调用:进程退出路径与 t.Cleanup 都会调,重复调用不得 panic,
// 且 Close 之后缓存必须是空的(不能把已关闭的连接再发给调用方)。
func TestCloseIsRepeatable(t *testing.T) {
	c := NewConnCache()
	if _, err := c.Dial(testEndpointA); err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	c.Close()
	if n := len(c.conns); n != 0 {
		t.Fatalf("Close 后缓存必须为空,实得 %d 条", n)
	}
	c.Close()
}

// 并发首拨:双检加锁必须保证只有一条连接进缓存,多出来的那条立刻关掉。
// 配合 -race 跑;若退回成"先查后写"的非原子实现,这里会拿到不同指针。
func TestDialConcurrentSameEndpointKeepsOne(t *testing.T) {
	c := NewConnCache()
	t.Cleanup(c.Close)

	const n = 16
	conns := make([]*grpc.ClientConn, n)
	errs := make([]error, n)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			start.Wait() // 尽量让 n 个 goroutine 同时冲进 Dial
			conns[i], errs[i] = c.Dial(testEndpointA)
		}(i)
	}
	start.Done()
	done.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d 拨号失败: %v", i, errs[i])
		}
		if conns[i] != conns[0] {
			t.Fatalf("goroutine %d 拿到了不同的连接,双检加锁失效", i)
		}
	}
	if got := len(c.conns); got != 1 {
		t.Fatalf("缓存里应当只剩 1 条连接,实得 %d", got)
	}
}
