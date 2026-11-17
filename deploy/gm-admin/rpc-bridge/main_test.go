package main

import (
	"io"
	"net"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestForwardPreservesBinaryRequestAndResponseAfterHalfClose(t *testing.T) {
	upstream, bridge := listen(t), listen(t)
	request := []byte{0, 255, 1, 0, 13, 10}
	response := []byte{255, 0, 7, 128}
	backend := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			backend <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		got, err := io.ReadAll(conn)
		if err == nil && string(got) != string(request) {
			t.Errorf("request changed: %v", got)
		}
		if err == nil {
			_, err = conn.Write(response)
		}
		backend <- err
	}()
	go serve(bridge, upstream.Addr().String())
	client, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = client.Write(request); err != nil {
		t.Fatal(err)
	}
	client.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(response) {
		t.Fatalf("response changed: %v", got)
	}
	if err = <-backend; err != nil {
		t.Fatal(err)
	}
}

func TestUnavailableUpstreamClosesClientAndListenerCanStop(t *testing.T) {
	upstream, bridge := listen(t), listen(t)
	target := upstream.Addr().String()
	upstream.Close()
	done := make(chan error, 1)
	go func() { done <- serve(bridge, target) }()
	client, err := net.Dial("tcp", bridge.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 1)
	if _, err = client.Read(buffer); err == nil {
		t.Fatal("unavailable upstream accepted payload")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("client remained blocked")
	}
	bridge.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not stop")
	}
}

func TestHalfClosedPeerCannotRetainConnectionSlotForever(t *testing.T) {
	client, clientPeer := net.Pipe()
	server, serverPeer := net.Pipe()
	t.Cleanup(func() { client.Close(); clientPeer.Close(); server.Close(); serverPeer.Close() })
	done := make(chan struct{})
	go func() { relay(client, server, 25*time.Millisecond); close(done) }()
	// 对端结束输入，而上游无限等待；排空期限必须结束另一方向的复制并释放连接。
	clientPeer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("half-closed stream retained its connection slot")
	}
	serverPeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := serverPeer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("upstream connection was not closed: %v", err)
	}
}
