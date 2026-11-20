package pkg

import (
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"proto/common/base"
)

// 传输层(transport.go)的单测。假节点是一台只会回显的 WebSocket 服务端:握手、掩码、帧头由 x/net 的服务端实现负责,
// 这里只看 robot 这一侧——地址怎么选、客户端帧是不是一帧一条二进制消息原样到达、经边缘时拨的是哪里。
// gate / battle 自己的握手与拆帧由 C++ 的 websocket_entry_test.cpp 覆盖;wireConn 的收发与重拨由 client / heartbeat /
// redirect 的用例覆盖(它们用 TCP 的 wireConn 连假 gate)。

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

// fakeWsNode 把收到的每条消息原样发回。
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

// recvRequestID 等 wireConn 的下一帧,要求它是带这个请求号的 ClientRequest(假节点原样回显)。
func recvRequestID(t *testing.T, conn *wireConn, want uint64) {
	t.Helper()
	type received struct {
		message any
		err     error
	}
	done := make(chan received, 1)
	go func() {
		message, err := conn.Recv()
		done <- received{message, err}
	}()
	select {
	case got := <-done:
		request, ok := got.message.(*base.ClientRequest)
		if got.err != nil || !ok || request.GetId() != want {
			t.Fatalf("echo = %#v, %v; want the ClientRequest with id %d", got.message, got.err, want)
		}
	case <-time.After(transportTestTimeout):
		t.Fatalf("no echo of request %d within the deadline", want)
	}
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

// GameClient 用的那条路:wireConn 直接在 WebSocket 上收发。每个客户端帧是一条二进制消息(与 Unity 客户端相同),
// 回来的字节照常解码;没下发地址时直连节点的客户端端口。
func TestTransport_WebSocketCarriesEachClientFrameAsOneBinaryMessage(t *testing.T) {
	node := newFakeWsNode(t, false)
	host, port := node.hostPort(t)
	useTransport(t, TransportConfig{Kind: TransportWebSocket})

	conn, err := dialNode(host, port)
	if err != nil {
		t.Fatalf("dialNode: %v", err)
	}
	defer conn.Close()

	// 发得比拨号早的帧排在队列里,连上之后按序发出。
	conn.Send(&base.ClientRequest{Id: 7, MessageId: 900001})
	conn.Send(&base.ClientRequest{Id: 8, MessageId: 900001})
	recvRequestID(t, conn, 7)
	recvRequestID(t, conn, 8)

	paths, _, payloadTypes := node.seen()
	if len(paths) != 1 || paths[0] != "/" {
		t.Fatalf("handshake paths = %v, want one direct connection to /", paths)
	}
	if len(payloadTypes) != 2 || payloadTypes[0] != websocket.BinaryFrame || payloadTypes[1] != websocket.BinaryFrame {
		t.Fatalf("frames seen by the node = %v, want two binary messages (one per client frame)", payloadTypes)
	}
}

// 配置要求必须下发、而服务端没下发:连接在本地就失败,不去猜地址。
func TestTransport_RequireAdvertisedFailsBeforeDialing(t *testing.T) {
	useTransport(t, TransportConfig{Kind: TransportWebSocket, RequireAdvertised: true})
	if conn, err := dialNode("203.0.113.9", 30000); err == nil {
		conn.Close()
		t.Fatal("dialNode must fail when no WebSocket address was advertised")
	}
	if _, err := NewGameClient("203.0.113.9", 30000); err == nil {
		t.Fatal("NewGameClient must surface the missing WebSocket address")
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

// 缺省(TCP)不受影响:裸连接与 wireConn 都是 TCP,地址簿里有没有这台节点的 WebSocket 地址都一样。
func TestTransport_TCPIsTheDefaultAndIgnoresAdvertisements(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			accepted, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = accepted.Close() }()
				_, _ = io.Copy(accepted, accepted)
			}()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	useTransport(t, TransportConfig{})
	AdvertiseWsURL("127.0.0.1", port, "wss://ws.example.test/ws/10.244.0.5/18000")

	stream, err := DialStream("127.0.0.1", port, transportTestTimeout)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, isTCP := stream.(*net.TCPConn); !isTCP {
		t.Fatalf("connection type %T, want a plain TCP connection", stream)
	}
	if got := echoOnce(t, stream, "raw"); got != "raw" {
		t.Fatalf("echo = %q, want raw", got)
	}

	// 原始 TCP 的回显服务端只认得出没有 WebSocket 帧头的客户端帧:回显能解出来,说明 wireConn 走的也是 TCP。
	conn, err := dialNode("127.0.0.1", port)
	if err != nil {
		t.Fatalf("dialNode: %v", err)
	}
	defer conn.Close()
	conn.Send(&base.ClientRequest{Id: 5, MessageId: 900001})
	recvRequestID(t, conn, 5)
}
