// 独立 GM 的固定内部 RPC 桥；只发布到宿主回环，不读取或记录业务载荷。
package main

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

var routes = []struct{ listen, upstream string }{
	{":50901", "xuanming-debug-current-data-service-1:9000"},
	{":50902", "xuanming-debug-current-player-locator-1:53200"},
}

func forward(client net.Conn, upstream string) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", upstream, 3*time.Second)
	if err != nil {
		log.Print("RPC bridge upstream connection failed")
		return
	}
	relay(client, server, 30*time.Second)
}

func relay(client, server net.Conn, drainTimeout time.Duration) {
	defer client.Close()
	defer server.Close()
	var streams sync.WaitGroup
	var drain sync.Once
	streams.Add(2)
	copyStream := func(destination, source net.Conn) {
		defer streams.Done()
		_, err := io.Copy(destination, source)
		if err != nil {
			// 异常必须同时解除另一方向的阻塞，避免失联客户端耗尽连接槽。
			client.Close()
			server.Close()
			return
		}
		if tcp, ok := destination.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil {
				client.Close()
				server.Close()
				return
			}
		}
		// 保留 TCP 半关闭后的响应；只有一端结束输入后才限制排空时间，正常长连接不设总时限。
		drain.Do(func() {
			deadline := time.Now().Add(drainTimeout)
			if client.SetDeadline(deadline) != nil || server.SetDeadline(deadline) != nil {
				client.Close()
				server.Close()
			}
		})
	}
	go copyStream(server, client)
	go copyStream(client, server)
	streams.Wait()
}

func serve(listener net.Listener, upstream string) error {
	slots := make(chan struct{}, 64)
	for {
		client, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); forward(client, upstream) }()
		default:
			client.Close()
		}
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		for _, route := range routes {
			conn, err := net.DialTimeout("tcp", route.upstream, 2*time.Second)
			if err != nil {
				os.Exit(1)
			}
			conn.Close()
		}
		return
	}
	if len(os.Args) != 1 {
		log.Fatal("Unsupported RPC bridge arguments")
	}
	errors := make(chan error, len(routes))
	for _, route := range routes {
		listener, err := net.Listen("tcp", route.listen)
		if err != nil {
			log.Fatal("RPC bridge listen failed")
		}
		go func(upstream string) { errors <- serve(listener, upstream) }(route.upstream)
	}
	log.Print("GM RPC bridge is listening on its two fixed internal routes")
	<-errors
	log.Fatal("RPC bridge listener stopped")
}
