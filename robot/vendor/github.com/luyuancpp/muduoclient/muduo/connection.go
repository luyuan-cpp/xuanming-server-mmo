package muduo

import (
	"bytes"
	"context"
	"errors"
	"github.com/golang/protobuf/proto"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Connection struct {
	addr  string
	codec Codec

	netConn net.Conn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	incoming chan proto.Message
	outgoing chan proto.Message

	recvBuffer  bytes.Buffer
	bufferMutex sync.Mutex

	connMutex  sync.Mutex // 保护 netConn 读写操作
	writeMutex sync.Mutex // 保护写数据顺序性

	closed atomic.Bool
}

// NewConnection 创建连接并启动管理协程，自动重连
func NewConnection(addr string, codec Codec) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &Connection{
		addr:     addr,
		codec:    codec,
		ctx:      ctx,
		cancel:   cancel,
		incoming: make(chan proto.Message, 100),
		outgoing: make(chan proto.Message, 100),
	}
	conn.wg.Add(1)
	go conn.connectionManager()
	return conn
}

func (conn *Connection) connectionManager() {
	defer conn.wg.Done()

	for {
		if conn.IsClosed() {
			return
		}

		netConn, err := net.Dial("tcp", conn.addr)
		if err != nil {
			log.Println("Connect failed:", err)
			time.Sleep(time.Second)
			continue
		}

		conn.setConn(netConn)
		log.Println("Connected to", conn.addr)

		// 清空旧 buffer
		conn.bufferMutex.Lock()
		conn.recvBuffer.Reset()
		conn.bufferMutex.Unlock()

		readCtx, readCancel := context.WithCancel(conn.ctx)
		writeCtx, writeCancel := context.WithCancel(conn.ctx)

		var rwGroup sync.WaitGroup
		rwGroup.Add(2)
		go func() {
			defer rwGroup.Done()
			conn.readLoop(readCtx, readCancel)
		}()
		go func() {
			defer rwGroup.Done()
			conn.writeLoop(writeCtx, writeCancel)
		}()

		rwGroup.Wait()

		// 清理连接
		conn.closeConn()

		if conn.IsClosed() {
			return
		}

		log.Println("Disconnected, retrying connection...")
		time.Sleep(time.Second)
	}
}

func (conn *Connection) setConn(netConn net.Conn) {
	conn.connMutex.Lock()
	defer conn.connMutex.Unlock()

	if conn.netConn != nil {
		_ = conn.netConn.Close()
	}
	conn.netConn = netConn
}

func (conn *Connection) closeConn() {
	conn.connMutex.Lock()
	defer conn.connMutex.Unlock()

	if conn.netConn != nil {
		_ = conn.netConn.Close()
		conn.netConn = nil
	}
}

func (conn *Connection) readLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	readBuf := make([]byte, 64*1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn.connMutex.Lock()
		netConn := conn.netConn
		conn.connMutex.Unlock()

		if netConn == nil {
			return
		}

		_ = netConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := netConn.Read(readBuf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			// 重点判断和忽略“use of closed network connection”错误
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
			conn.bufferMutex.Lock()
			conn.recvBuffer.Write(readBuf[:n])

			for {
				message, decodedLen, err := conn.codec.Decode(conn.recvBuffer.Bytes())
				if err != nil {
					log.Println("Decode error:", err)
					break
				}
				if decodedLen <= 0 {
					break
				}
				conn.recvBuffer.Next(int(decodedLen))

				select {
				case conn.incoming <- message:
				default:
					log.Println("Incoming channel full, dropping message")
				}
			}
			conn.bufferMutex.Unlock()
		}
	}
}

func (conn *Connection) writeLoop(ctx context.Context, cancel context.CancelFunc) {
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

			conn.writeMutex.Lock()
			totalWritten := 0
			for totalWritten < len(data) {
				n, err := netConn.Write(data[totalWritten:])
				if err != nil {
					log.Println("Write error:", err)
					conn.writeMutex.Unlock()
					return
				}
				totalWritten += n
			}
			conn.writeMutex.Unlock()
		}
	}
}

func (conn *Connection) Send(message proto.Message) (err error) {
	if conn.IsClosed() {
		return errors.New("connection closed")
	}
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("send failed: connection closed")
		}
	}()
	select {
	case conn.outgoing <- message:
		return nil
	default:
		return errors.New("send buffer full")
	}
}

func (conn *Connection) Recv() (proto.Message, error) {
	message, ok := <-conn.incoming
	if !ok {
		return nil, errors.New("connection closed")
	}
	return message, nil
}

func (conn *Connection) Close() {
	if conn.closed.CompareAndSwap(false, true) {
		conn.cancel()
		conn.closeConn()
		close(conn.outgoing) // outgoing 关闭后，写协程才能退出
		conn.wg.Wait()
		log.Println("Connection closed cleanly")
	}
}

func (conn *Connection) IsClosed() bool {
	return conn.closed.Load()
}
