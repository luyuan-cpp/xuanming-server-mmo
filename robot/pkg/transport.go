package pkg

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"go.uber.org/zap"
	"golang.org/x/net/websocket"
)

// 本文件是 robot 连 gate / battle 的传输方式(docs/design/minigame-websocket-gate.md):
// 原生客户端走 TCP;微信 / 抖音小游戏只有 WebSocket。gate / battle 的客户端端口两种连接都接,
// 升级之后一条二进制 WebSocket 消息承载的就是原来的客户端帧字节,所以这里只换「字节怎么送到」,
// 编解码、验票、心跳、重定向都不变。
//
// 传输方式对全进程生效:启动时调一次 ConfigureTransport,之后 NewGameClient / SwapConn / DialStream 都按它连。

const (
	TransportTCP       = "tcp"
	TransportWebSocket = "websocket"

	// wsDialTimeout 是一条 WebSocket 从拨号到握手完成(含 TLS)的期限,与 Unity 客户端的打开期限一致。
	wsDialTimeout = 10 * time.Second
	// wsOrigin 只是握手里必须带的 Origin 头;gate / battle 不查 Origin。
	wsOrigin = "http://robot.invalid/"
)

// TransportConfig 是配置里的 transport 段。
type TransportConfig struct {
	// Kind:"" 或 "tcp"(缺省,原生客户端)、"websocket"(小游戏)。下面三项只在 websocket 下有意义。
	Kind string `yaml:"kind"`
	// RequireAdvertised:服务端没有为这台节点下发 WebSocket 地址时连接失败,不回落到直连 ws://host:port/。
	// 经边缘(wss)验证时打开,否则服务端漏下发会被直连掩盖。
	RequireAdvertised bool `yaml:"require_advertised"`
	// DialAddr 非空时实际拨这个 host:port,握手的 Host 与 TLS 的 SNI 仍按下发地址里的主机。
	// 给边缘域名在本机解析不到的测试环境用(例如 kind 里的 NodePort)。
	DialAddr string `yaml:"dial_addr"`
	// InsecureSkipVerify 不校验边缘证书。只给自签证书的测试环境。
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

func (c TransportConfig) webSocket() bool { return c.Kind == TransportWebSocket }

// Validate 只做本地可判的检查,不碰网络。
func (c TransportConfig) Validate() error {
	switch c.Kind {
	case "", TransportTCP:
		if c.RequireAdvertised || c.DialAddr != "" || c.InsecureSkipVerify {
			// 写了 WebSocket 的选项却没选 websocket:照跑就是悄悄测了 TCP。
			return errors.New("transport.require_advertised / dial_addr / insecure_skip_verify need transport.kind: websocket")
		}
	case TransportWebSocket:
		if c.DialAddr != "" {
			if _, _, err := net.SplitHostPort(c.DialAddr); err != nil {
				return fmt.Errorf("transport.dial_addr %q is not host:port: %w", c.DialAddr, err)
			}
		}
	default:
		return fmt.Errorf("transport.kind %q is not one of %q, %q", c.Kind, TransportTCP, TransportWebSocket)
	}
	return nil
}

var transport struct {
	mu  sync.RWMutex
	cfg TransportConfig
	// wsURLs:"host:port" → 服务端为这台节点下发的 WebSocket 地址。
	wsURLs map[string]string
}

// ConfigureTransport 定下全进程的传输方式。启动时、建立任何连接之前调一次。
func ConfigureTransport(cfg TransportConfig) {
	transport.mu.Lock()
	transport.cfg = cfg
	transport.mu.Unlock()
	if cfg.webSocket() {
		zap.L().Info("robot transport: websocket",
			zap.Bool("require_advertised", cfg.RequireAdvertised),
			zap.String("dial_addr", cfg.DialAddr),
			zap.Bool("insecure_skip_verify", cfg.InsecureSkipVerify))
	}
}

func currentTransport() TransportConfig {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	return transport.cfg
}

// AdvertiseWsURL 记下服务端为 host:port 这台节点下发的 WebSocket 地址:assign-gate / queue-status 的 gate_ws_url、
// msg 124 的 target_ws_url、战斗分配与测速目标的 ws_url。wsURL 为空表示这一次没下发,清掉旧记录。
//
// robot 里几十处按 (host, port) 传地址,所以不逐层加参数:收到地址的地方登记,连接时按 host:port 查。
// 同一台节点的两种地址总是一起下发,连接又总在下发之后,查到的就是这一次的。TCP 模式下照记,只是没人读。
func AdvertiseWsURL(host string, port int, wsURL string) {
	if host == "" || port <= 0 {
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if wsURL == "" {
		delete(transport.wsURLs, addr)
		return
	}
	if transport.wsURLs == nil {
		transport.wsURLs = make(map[string]string)
	}
	transport.wsURLs[addr] = wsURL
}

// AdvertisedWsURL 返回 host:port 这台节点当前登记的 WebSocket 地址,没有登记为空。
func AdvertisedWsURL(host string, port int) string {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	return transport.wsURLs[net.JoinHostPort(host, strconv.Itoa(port))]
}

// wsURLFor 返回连 host:port 这台节点要用的 WebSocket 地址:服务端下发过就用下发的;没下发时直连它的客户端端口
// (本机 / 开发栈没有边缘,gate 与 battle 的客户端端口自己就接 WebSocket),除非配置要求必须下发。
func wsURLFor(cfg TransportConfig, host string, port int) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if advertised := AdvertisedWsURL(host, port); advertised != "" {
		return advertised, nil
	}
	if cfg.RequireAdvertised {
		return "", fmt.Errorf("the server advertised no WebSocket address for %s (transport.require_advertised is set)", addr)
	}
	return "ws://" + addr + "/", nil
}

// DialStream 拨一条到 host:port 这台节点的字节流连接,给不经 GameClient 的裸连接用(测速、空闲超时探测、重定向前的探测)。
// TCP 模式就是 TCP 连接;WebSocket 模式是一条 WebSocket,读写的仍是同一套客户端帧字节。timeout 覆盖建连与握手。
func DialStream(host string, port int, timeout time.Duration) (net.Conn, error) {
	cfg := currentTransport()
	if !cfg.webSocket() {
		return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	}
	wsURL, err := wsURLFor(cfg, host, port)
	if err != nil {
		return nil, err
	}
	return dialWebSocket(wsURL, cfg, timeout)
}

// newMuduoClient 按当前传输方式连到 host:port。第二个返回值是这条连接的收尾函数,在 client.Close() 之后调;
// TCP 模式没有要收尾的东西,为 nil。
func newMuduoClient(host string, port int) (*muduo.Client, func(), error) {
	cfg := currentTransport()
	codec := &muduo.TcpCodec{}
	if !cfg.webSocket() {
		c, err := muduo.NewClient(host, port, codec)
		return c, nil, err
	}
	wsURL, err := wsURLFor(cfg, host, port)
	if err != nil {
		return nil, nil, err
	}
	bridge, err := startWsBridge(func() (net.Conn, error) { return dialWebSocket(wsURL, cfg, wsDialTimeout) })
	if err != nil {
		return nil, nil, err
	}
	zap.L().Debug("connecting over websocket",
		zap.String("node", net.JoinHostPort(host, strconv.Itoa(port))), zap.String("url", wsURL))
	c, err := muduo.NewClient("127.0.0.1", bridge.port(), codec)
	if err != nil {
		bridge.Close()
		return nil, nil, err
	}
	return c, bridge.Close, nil
}

// wsConn 是一条 WebSocket 上的字节流:每次 Write 的字节是一条二进制消息,Read 读到的是各条消息载荷接起来的流。
//
// Close 直接关底层连接,不发 Close 帧:x/net 的 Close 要先拿写锁发 Close 帧,对端不读的时候会和阻塞中的 Write 互相卡住。
// 对服务端来说等同于客户端直接断开,原生客户端断 TCP 也是这样。
type wsConn struct {
	*websocket.Conn
	raw net.Conn
}

func (c *wsConn) Close() error { return c.raw.Close() }

func dialWebSocket(rawURL string, cfg TransportConfig, timeout time.Duration) (net.Conn, error) {
	wsConfig, err := websocket.NewConfig(rawURL, wsOrigin)
	if err != nil {
		return nil, fmt.Errorf("websocket address %q: %w", rawURL, err)
	}
	location := wsConfig.Location
	secure := location.Scheme == "wss"
	if (!secure && location.Scheme != "ws") || location.Hostname() == "" {
		return nil, fmt.Errorf("websocket address %q must be ws://host[:port]/... or wss://host[:port]/...", rawURL)
	}
	addr := cfg.DialAddr
	if addr == "" {
		port := location.Port()
		if port == "" {
			port = "80"
			if secure {
				port = "443"
			}
		}
		addr = net.JoinHostPort(location.Hostname(), port)
	}

	deadline := time.Now().Add(timeout)
	raw, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s for %s: %w", addr, rawURL, err)
	}
	stream := raw
	if secure {
		stream = tls.Client(raw, &tls.Config{
			ServerName: location.Hostname(),
			MinVersion: tls.VersionTLS12,
			// 只在配置显式打开时跳过校验,给自签证书的测试环境。
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec
		})
	}
	// TLS 与 WebSocket 握手受同一个期限约束;握完清掉,之后的读写期限由调用方自己设。
	_ = raw.SetDeadline(deadline)
	ws, err := websocket.NewClient(wsConfig, stream)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("websocket handshake with %s via %s: %w", rawURL, addr, err)
	}
	_ = raw.SetDeadline(time.Time{})
	ws.PayloadType = websocket.BinaryFrame
	return &wsConn{Conn: ws, raw: raw}, nil
}

// wsBridge 在回环上开一个监听,把连进来的每条 TCP 接到一条新拨的 WebSocket 上,两边原样转发字节。
//
// vendored muduo.Client 只会自己拨 TCP,没有注入拨号函数的口子,而 GameClient 到处拿着 *muduo.Client;
// 所以 WebSocket 模式下让它拨这个监听。muduo 断线后会重拨同一个地址,桥对每条连进来的连接各拨一条 WebSocket,
// 重连的节奏与 TCP 模式相同。
//
// shortcut: 每条连接多一个回环监听和一跳回环 TCP,只适合冒烟与小规模;要用 WebSocket 打高并发压测,
// 先给 muduoclient 加注入拨号函数的口子,再把这一层删掉。
type wsBridge struct {
	listener net.Listener
	dial     func() (net.Conn, error)

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func startWsBridge(dial func() (net.Conn, error)) (*wsBridge, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the websocket bridge: %w", err)
	}
	b := &wsBridge{listener: listener, dial: dial, conns: make(map[net.Conn]struct{})}
	go b.acceptLoop()
	return b, nil
}

func (b *wsBridge) port() int { return b.listener.Addr().(*net.TCPAddr).Port }

// Close 关掉监听和桥上的全部连接。可以重复调。要在 muduo 客户端 Close 之后调:
// 先关桥的话 muduo 那头读到断开,会先睡一秒准备重拨,它的 Close 就得多等这一秒。
func (b *wsBridge) Close() {
	b.mu.Lock()
	b.closed = true
	conns := b.conns
	b.conns = nil
	b.mu.Unlock()
	_ = b.listener.Close()
	for conn := range conns {
		_ = conn.Close()
	}
}

func (b *wsBridge) acceptLoop() {
	for {
		local, err := b.listener.Accept()
		if err != nil {
			return // 监听已关
		}
		go b.serve(local)
	}
}

func (b *wsBridge) serve(local net.Conn) {
	remote := b.dialUntilUp()
	if remote == nil || !b.track(local, remote) {
		_ = local.Close()
		if remote != nil {
			_ = remote.Close()
		}
		return
	}
	defer b.untrack(local, remote)
	// 任一方向结束就两头都关,另一个方向随之结束。重复 Close 无害。
	closeBoth := func() {
		_ = local.Close()
		_ = remote.Close()
	}
	go func() {
		pump(remote, local)
		closeBoth()
	}()
	pump(local, remote)
	closeBoth()
}

// dialUntilUp 拨到通为止,与 muduo 在 TCP 上「拨不通每秒重试」同一个节奏。这期间 muduo 写进回环的字节留在内核缓冲里,
// 拨通后原样送出(TCP 模式下它们排在 muduo 的发送队列里,同样不丢)。桥被关掉时返回 nil。
func (b *wsBridge) dialUntilUp() net.Conn {
	for {
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if closed {
			return nil
		}
		remote, err := b.dial()
		if err == nil {
			return remote
		}
		zap.L().Warn("websocket bridge: dial failed, retrying", zap.Error(err))
		time.Sleep(time.Second)
	}
}

func (b *wsBridge) track(conns ...net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	for _, conn := range conns {
		b.conns[conn] = struct{}{}
	}
	return true
}

func (b *wsBridge) untrack(conns ...net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, conn := range conns {
		delete(b.conns, conn)
	}
}

// pump 把 src 读到的字节原样写给 dst,到任一头出错为止。往 WebSocket 写时每次 Write 是一条二进制消息,
// 所以缓冲大小就是单条消息的上限:16KB,远小于 gate / battle 的单帧上限(64KB)。
// 切在哪里与客户端帧的边界无关,服务端按字节流接。
func pump(dst io.Writer, src io.Reader) {
	buf := make([]byte, 16*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
