package pkg

import (
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"proto/common/base"
)

// 传输层(transport.go)的单测。假节点是一台只会回显的 WebSocket 服务端:握手、掩码、帧头由 x/net 的服务端实现负责,
// 这里只看 robot 这一侧——地址怎么选、字节是不是按二进制消息原样到达、桥在断线与关闭时的行为。
// gate / battle 自己的握手与拆帧由 C++ 的 websocket_entry_test.cpp 覆盖。

const transportTestTimeout = 5 * time.Second

type wsTestFrame struct {
	payloadType byte
	data        []byte
}

// 收一帧并留下它的类型(x/net 自带的 Message 编解码不区分文本与二进制)。
var wsTestCodec = websocket.Codec{
	Unmarshal: func(data []byte, payloadType byte, v interface{}) error {
		frame := v.(*wsTestFrame)
		frame.payloadType = payloadType
		frame.data = append([]byte(nil), data...)
		return nil
	},
}

// fakeWsNode 把收到的每条消息原样发回;收到 "bye" 就结束这条会话(服务端关连接)。
type fakeWsNode struct {
	server *httptest.Server

	mu           sync.Mutex
	paths        []string // 每条会话握手请求的路径
	hosts        []string // 每条会话握手请求的 Host
	payloadTypes []byte   // 收到的每一帧的类型
}

func newFakeWsNode(t *testing.T, useTLS bool) *fakeWsNode {
	t.Helper()
	node := &fakeWsNode{}
	// 不设 Handshake:不查 Origin,与 gate / battle 一致。
	handler := websocket.Server{Handler: node.serve}
	if useTLS {
		node.server = httptest.NewTLSServer(handler)
	} else {
		node.server = httptest.NewServer(handler)
	}
	t.Cleanup(node.server.Close)
	return node
}

func (n *fakeWsNode) serve(ws *websocket.Conn) {
	n.mu.Lock()
	n.paths = append(n.paths, ws.Request().URL.Path)
	n.hosts = append(n.hosts, ws.Request().Host)
	n.mu.Unlock()
	ws.PayloadType = websocket.BinaryFrame
	for {
		var frame wsTestFrame
		if err := wsTestCodec.Receive(ws, &frame); err != nil {
			return
		}
		n.mu.Lock()
		n.payloadTypes = append(n.payloadTypes, frame.payloadType)
		n.mu.Unlock()
		if string(frame.data) == "bye" {
			return
		}
		if _, err := ws.Write(frame.data); err != nil {
			return
		}
	}
}

// hostPort 是假节点监听的地址,相当于服务端下发的 TCP 地址。
func (n *fakeWsNode) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(n.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("listener port: %v", err)
	}
	return host, port
}

func (n *fakeWsNode) seen() (paths, hosts []string, payloadTypes []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.paths...), append([]string(nil), n.hosts...), append([]byte(nil), n.payloadTypes...)
}

// useTransport 切换全进程的传输方式,用例结束时恢复成缺省(TCP、空地址簿)。
func useTransport(t *testing.T, cfg TransportConfig) {
	t.Helper()
	ConfigureTransport(cfg)
	t.Cleanup(func() {
		transport.mu.Lock()
		transport.cfg = TransportConfig{}
		transport.wsURLs = nil
		transport.mu.Unlock()
	})
}

// echoOnce 写一段字节并读回同样长度,带期限。
func echoOnce(t *testing.T, conn net.Conn, payload string) string {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(transportTestTimeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write %q: %v", payload, err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read echo of %q: %v", payload, err)
	}
	return string(reply)
}

func TestTransportConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     TransportConfig
		wantErr bool
	}{
		{"default is tcp", TransportConfig{}, false},
		{"tcp", TransportConfig{Kind: TransportTCP}, false},
		{"websocket", TransportConfig{Kind: TransportWebSocket}, false},
		{"websocket with edge options", TransportConfig{Kind: TransportWebSocket, RequireAdvertised: true, DialAddr: "127.0.0.1:30443", InsecureSkipVerify: true}, false},
		{"unknown kind", TransportConfig{Kind: "quic"}, true},
		{"dial_addr without a port", TransportConfig{Kind: TransportWebSocket, DialAddr: "ws.example.test"}, true},
		// 写了 WebSocket 的选项却没选 websocket:照跑就是悄悄测了 TCP,所以拒绝。
		{"dial_addr on tcp", TransportConfig{DialAddr: "127.0.0.1:30443"}, true},
		{"require_advertised on tcp", TransportConfig{Kind: TransportTCP, RequireAdvertised: true}, true},
		{"insecure_skip_verify on tcp", TransportConfig{InsecureSkipVerify: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// 服务端下发过地址就用下发的;没下发时直连节点的客户端端口,除非配置要求必须下发。空的下发清掉旧记录。
func TestTransport_AdvertisedAddressWinsAndAMissingOneFallsBackOrFails(t *testing.T) {
	useTransport(t, TransportConfig{Kind: TransportWebSocket})
	direct := TransportConfig{Kind: TransportWebSocket}
	strict := TransportConfig{Kind: TransportWebSocket, RequireAdvertised: true}
	const advertised = "wss://ws.example.test/ws/10.244.0.5/18000"

	if got, err := wsURLFor(direct, "203.0.113.9", 30000); err != nil || got != "ws://203.0.113.9:30000/" {
		t.Fatalf("no advertisement: got %q, %v; want the direct address", got, err)
	}
	if got, err := wsURLFor(direct, "::1", 18000); err != nil || got != "ws://[::1]:18000/" {
		t.Fatalf("IPv6 host: got %q, %v", got, err)
	}
	if _, err := wsURLFor(strict, "203.0.113.9", 30000); err == nil {
		t.Fatal("require_advertised with no advertisement must fail instead of falling back")
	}

	AdvertiseWsURL("203.0.113.9", 30000, advertised)
	for name, cfg := range map[string]TransportConfig{"direct": direct, "strict": strict} {
		if got, err := wsURLFor(cfg, "203.0.113.9", 30000); err != nil || got != advertised {
			t.Fatalf("%s: got %q, %v; want the advertised address", name, got, err)
		}
	}
	if got, _ := wsURLFor(direct, "203.0.113.9", 30001); got != "ws://203.0.113.9:30001/" {
		t.Fatalf("another node must not inherit the advertisement, got %q", got)
	}

	AdvertiseWsURL("203.0.113.9", 30000, "")
	if got, _ := wsURLFor(direct, "203.0.113.9", 30000); got != "ws://203.0.113.9:30000/" {
		t.Fatalf("an empty advertisement must clear the old one, got %q", got)
	}
}

// GameClient 用的那条路:muduo 客户端经本机桥连 WebSocket。发出的客户端帧以二进制消息到达,回来的字节照常解码。
func TestTransport_WebSocketCarriesMuduoFramesAsBinaryMessages(t *testing.T) {
	node := newFakeWsNode(t, false)
	host, port := node.hostPort(t)
	useTransport(t, TransportConfig{Kind: TransportWebSocket})

	client, closeTransport, err := newMuduoClient(host, port)
	if err != nil {
		t.Fatalf("newMuduoClient: %v", err)
	}
	if closeTransport == nil {
		t.Fatal("websocket transport must return a closer for its bridge")
	}
	// 与 GameClient.Close 同序:先关 muduo 客户端,再收桥。
	defer closeTransport()
	defer client.Close()

	client.Send(&base.ClientRequest{Id: 7, MessageId: 900001})
	type received struct {
		message any
		err     error
	}
	done := make(chan received, 1)
	go func() {
		message, err := client.Recv()
		done <- received{message, err}
	}()
	select {
	case got := <-done:
		request, ok := got.message.(*base.ClientRequest)
		if got.err != nil || !ok || request.GetId() != 7 || request.GetMessageId() != 900001 {
			t.Fatalf("echo = %#v, %v; want the ClientRequest that was sent", got.message, got.err)
		}
	case <-time.After(transportTestTimeout):
		t.Fatal("no echo within the deadline")
	}

	paths, _, payloadTypes := node.seen()
	if len(paths) != 1 || paths[0] != "/" {
		t.Fatalf("handshake paths = %v, want one direct connection to /", paths)
	}
	if len(payloadTypes) == 0 {
		t.Fatal("the node received no frames")
	}
	for _, payloadType := range payloadTypes {
		if payloadType != websocket.BinaryFrame {
			t.Fatalf("frame types = %v, want binary frames only", payloadTypes)
		}
	}
}

// 经边缘的那条路:地址用服务端下发的 wss,实际拨 dial_addr,握手的 Host 与路径仍是下发地址里的;证书校验缺省开着。
func TestTransport_EdgeAddressIsDialedThroughDialAddrOverTLS(t *testing.T) {
	node := newFakeWsNode(t, true)
	const advertised = "wss://ws.example.test/ws/10.244.0.5/18000"
	edge := TransportConfig{
		Kind:               TransportWebSocket,
		RequireAdvertised:  true,
		DialAddr:           node.server.Listener.Addr().String(),
		InsecureSkipVerify: true,
	}
	useTransport(t, edge)
	AdvertiseWsURL("203.0.113.9", 30000, advertised)

	conn, err := DialStream("203.0.113.9", 30000, transportTestTimeout)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := echoOnce(t, conn, "ping"); got != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
	paths, hosts, _ := node.seen()
	if len(paths) != 1 || paths[0] != "/ws/10.244.0.5/18000" || hosts[0] != "ws.example.test" {
		t.Fatalf("handshake path %v host %v, want the advertised path and host", paths, hosts)
	}

	// 假节点的证书是自签的:不开 insecure_skip_verify 就握不上。
	edge.InsecureSkipVerify = false
	ConfigureTransport(edge)
	if verified, err := DialStream("203.0.113.9", 30000, transportTestTimeout); err == nil {
		_ = verified.Close()
		t.Fatal("a self-signed edge certificate must be rejected unless insecure_skip_verify is set")
	}
}

// muduo 断线后会重拨同一个地址:桥对每条连进来的连接各拨一条 WebSocket。服务端关掉会话,回环这头读到断开;
// 桥关掉时回环连接随之断开,之后不再接新连接。
func TestWsBridge_RedialsPerConnectionAndStopsOnClose(t *testing.T) {
	node := newFakeWsNode(t, false)
	host, port := node.hostPort(t)
	wsURL := "ws://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	bridge, err := startWsBridge(func() (net.Conn, error) {
		return dialWebSocket(wsURL, TransportConfig{Kind: TransportWebSocket}, transportTestTimeout)
	})
	if err != nil {
		t.Fatalf("startWsBridge: %v", err)
	}
	defer bridge.Close()
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(bridge.port()))
	dial := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", loopback, transportTestTimeout)
		if err != nil {
			t.Fatalf("dial the bridge: %v", err)
		}
		return conn
	}

	first := dial()
	defer func() { _ = first.Close() }()
	if got := echoOnce(t, first, "one"); got != "one" {
		t.Fatalf("first echo = %q", got)
	}
	if _, err := first.Write([]byte("bye")); err != nil {
		t.Fatalf("write bye: %v", err)
	}
	if rest, err := io.ReadAll(first); err != nil || len(rest) != 0 {
		t.Fatalf("after the server closed the session: read %q, %v; want a clean end of stream", rest, err)
	}

	second := dial()
	defer func() { _ = second.Close() }()
	if got := echoOnce(t, second, "two"); got != "two" {
		t.Fatalf("second echo = %q", got)
	}
	if paths, _, _ := node.seen(); len(paths) != 2 {
		t.Fatalf("websocket sessions = %d, want one per loopback connection (2)", len(paths))
	}

	bridge.Close()
	// 读到连接重置或干净的结束都算断开;一直读到期限才返回说明连接还挂着。
	if _, err := io.ReadAll(second); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the loopback connection must be closed when the bridge closes")
	}
	if conn, err := net.DialTimeout("tcp", loopback, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the bridge must stop accepting connections after Close")
	}
}

// 缺省(TCP)不受影响:连接就是 TCP,地址簿里有没有这台节点的 WebSocket 地址都一样,也不起桥。
func TestTransport_TCPIsTheDefaultAndIgnoresAdvertisements(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	useTransport(t, TransportConfig{})
	AdvertiseWsURL("127.0.0.1", port, "wss://ws.example.test/ws/10.244.0.5/18000")

	conn, err := DialStream("127.0.0.1", port, transportTestTimeout)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, isTCP := conn.(*net.TCPConn); !isTCP {
		t.Fatalf("connection type %T, want a plain TCP connection", conn)
	}
	if got := echoOnce(t, conn, "raw"); got != "raw" {
		t.Fatalf("echo = %q, want raw", got)
	}

	client, closeTransport, err := newMuduoClient("127.0.0.1", port)
	if err != nil {
		t.Fatalf("newMuduoClient: %v", err)
	}
	client.Close()
	if closeTransport != nil {
		t.Fatal("the tcp transport has nothing to tear down; a closer means a bridge was started")
	}
}
