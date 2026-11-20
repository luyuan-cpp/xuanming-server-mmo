package pkg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luyuancpp/muduoclient/muduo"
	"google.golang.org/protobuf/runtime/protoiface"
)

// wireConn 是 GameClient 用的那条到 gate / battle 的分帧连接:自己在后台拨号,收发各一个 goroutine。
//
// 它是 vendored muduoclient(github.com/luyuancpp/muduoclient v0.0.14)的 muduo.Connection 搬进仓内的版本,
// 只为换掉一处:原库自己 net.Dial("tcp", addr),拨号方式插不进去,接不了小游戏的 WebSocket(transport.go);
// 这里怎么拨号由调用方给。帧的编解码仍用原库的 muduo.TcpCodec。
//
// 对 GameClient 可见的行为与原库逐条相同,robot 里别处注释说的「vendored muduo 的连接」指的就是这些:
//   - 构造即返回,拨号在后台。拨不通每秒重试,不报错;
//   - Send 不阻塞:发送队列(100)满了就丢。连上之前排队的帧在连上之后发出;
//   - 对端断开之后,读 goroutine 退出,写 goroutine 要到下一次发送失败才退出,那时才重拨同一个地址;
//     写进死连接的帧丢失、不报错。所以在途调用只能靠超时发现断线;
//   - Recv 阻塞到下一帧。Close 不关接收队列:阻塞在 Recv 上的 goroutine 不会被 Close 唤醒(见 GameClient.SwapConn);
//   - Close 等后台 goroutine 退出;拨号阻塞期间要等到拨号返回。
//
// 与原库不同的只有不影响上述行为的两点:读不设 30 秒期限(原库只用它定期醒来看 ctx,而 Close 本来就会关连接把读唤醒;
// WebSocket 下读到一半帧头超时会丢掉已读的字节,所以去掉),接收缓冲每条连接各用一个新的(原库是重连时清空)。
type wireConn struct {
	label string // 日志里的对端:host:port 或 WebSocket 地址
	dial  func() (net.Conn, error)
	codec muduo.TcpCodec

	connMutex sync.Mutex // 保护 netConn
	netConn   net.Conn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	incoming chan protoiface.MessageV1
	outgoing chan protoiface.MessageV1

	closed atomic.Bool
}

func newWireConn(label string, dial func() (net.Conn, error)) *wireConn {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &wireConn{
		label:    label,
		dial:     dial,
		ctx:      ctx,
		cancel:   cancel,
		incoming: make(chan protoiface.MessageV1, 100),
		outgoing: make(chan protoiface.MessageV1, 100),
	}
	conn.wg.Add(1)
	go conn.manage()
	return conn
}

// newTCPWireConn 是 TCP 直连的 wireConn,拨号不带超时(与原库相同)。
func newTCPWireConn(host string, port int) *wireConn {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	return newWireConn(addr, func() (net.Conn, error) { return net.Dial("tcp", addr) })
}

// manage 拨号、起读写两个 goroutine、等它们都退出之后重拨,直到 Close。
func (conn *wireConn) manage() {
	defer conn.wg.Done()

	for {
		if conn.closed.Load() {
			return
		}

		netConn, err := conn.dial()
		if err != nil {
			log.Println("Connect failed:", err)
			time.Sleep(time.Second)
			continue
		}

		conn.setConn(netConn)
		log.Println("Connected to", conn.label)

		readCtx, readCancel := context.WithCancel(conn.ctx)
		writeCtx, writeCancel := context.WithCancel(conn.ctx)

		var rwGroup sync.WaitGroup
		rwGroup.Add(2)
		go func() {
			defer rwGroup.Done()
			conn.readLoop(readCtx, readCancel, netConn)
		}()
		go func() {
			defer rwGroup.Done()
			conn.writeLoop(writeCtx, writeCancel)
		}()

		rwGroup.Wait()

		conn.closeConn()

		if conn.closed.Load() {
			return
		}

		log.Println("Disconnected, retrying connection...")
		time.Sleep(time.Second)
	}
}

func (conn *wireConn) setConn(netConn net.Conn) {
	conn.connMutex.Lock()
	defer conn.connMutex.Unlock()

	if conn.netConn != nil {
		_ = conn.netConn.Close()
	}
	conn.netConn = netConn
}

func (conn *wireConn) closeConn() {
	conn.connMutex.Lock()
	defer conn.connMutex.Unlock()

	if conn.netConn != nil {
		_ = conn.netConn.Close()
		conn.netConn = nil
	}
}

func (conn *wireConn) readLoop(ctx context.Context, cancel context.CancelFunc, netConn net.Conn) {
	defer cancel()
	readBuf := make([]byte, 64*1024)
	var recvBuffer bytes.Buffer

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := netConn.Read(readBuf)
		if err != nil {
			// Close / 重拨关掉了这条连接:不算错误。
			if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			if err == io.EOF {
				log.Println("Connection closed by server")
			} else {
				log.Println("Read error:", err)
			}
			return
		}

		if n > 0 {
			recvBuffer.Write(readBuf[:n])

			for {
				message, decodedLen, err := conn.codec.Decode(recvBuffer.Bytes())
				if err != nil {
					log.Println("Decode error:", err)
					break
				}
				if decodedLen == 0 {
					break
				}
				recvBuffer.Next(int(decodedLen))

				select {
				case conn.incoming <- message:
				default:
					log.Println("Incoming channel full, dropping message")
				}
			}
		}
	}
}

func (conn *wireConn) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-conn.outgoing:
			if !ok {
				return
			}

			data, err := conn.codec.Encode(&message)
			if err != nil {
				log.Println("Encode error:", err)
				continue
			}

			conn.connMutex.Lock()
			netConn := conn.netConn
			conn.connMutex.Unlock()

			if netConn == nil {
				log.Println("Write skipped: no active connection")
				return
			}

			_ = netConn.SetWriteDeadline(time.Now().Add(10 * time.Second))

			totalWritten := 0
			for totalWritten < len(data) {
				n, err := netConn.Write(data[totalWritten:])
				if err != nil {
					log.Println("Write error:", err)
					return
				}
				totalWritten += n
			}
		}
	}
}

// Send 把一帧排进发送队列,不阻塞;已 Close 或队列满时丢弃。
func (conn *wireConn) Send(message protoiface.MessageV1) {
	if conn.closed.Load() {
		return
	}
	// Close 与本函数并发时可能往已关闭的队列里发:按丢弃处理。
	defer func() { _ = recover() }()
	select {
	case conn.outgoing <- message:
	default:
	}
}

// Recv 阻塞到下一帧。接收队列永不关闭(见类型注释),所以错误分支实际走不到,保留是为了调用方的写法不变。
func (conn *wireConn) Recv() (protoiface.MessageV1, error) {
	message, ok := <-conn.incoming
	if !ok {
		return nil, errors.New("connection closed")
	}
	return message, nil
}

// Close 关连接并等后台 goroutine 退出。可以重复调。
func (conn *wireConn) Close() {
	if conn.closed.CompareAndSwap(false, true) {
		conn.cancel()
		conn.closeConn()
		close(conn.outgoing) // 发送队列关了,写 goroutine 才退得出来
		conn.wg.Wait()
		log.Println("Connection closed cleanly")
	}
}
