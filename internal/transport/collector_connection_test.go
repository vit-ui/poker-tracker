package transport

import (
	"net"
	"testing"
	"time"
)

func TestCollectorConnectionCloseUnblocksFullCommandChannel(t *testing.T) {
	link, info := startServer(t, "owned-token")
	connection, err := ConnectCollector(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection.Link.Uploads <- Upload{Type: ImportStart}
	recvUpload(t, link)
	// Fill the receiving buffer and leave a reader blocked on its next command.
	for i := 0; i < cap(connection.Link.Commands)+3; i++ {
		link.Commands <- Command{Kind: CommandRescan}
	}
	deadline := time.Now().Add(time.Second)
	for len(connection.Link.Commands) < cap(connection.Link.Commands) {
		if time.Now().After(deadline) {
			t.Fatal("command buffer never filled")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { connection.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close left a command reader blocked")
	}
	select {
	case <-connection.Done:
	default:
		t.Fatal("close did not signal connection loss")
	}
	connection.Close()
}

func TestCollectorConnectionCancellationInterruptsTLSHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := ConnectCollector(ConnInfo{Host: host, Port: port, Token: "test", Fingerprint: "unused"}, stop)
		finished <- err
	}()
	var peer net.Conn
	select {
	case peer = <-accepted:
		defer peer.Close()
	case <-time.After(time.Second):
		t.Fatal("dial never reached stalled TLS peer")
	}
	close(stop)
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt TLS handshake")
	}
}

func TestCollectorConnectionDrainFinishesAcceptedUploads(t *testing.T) {
	link, info := startServer(t, "owned-token")
	connection, err := ConnectCollector(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	finished := make(chan struct{})
	go func() {
		for i := range 32 {
			connection.Link.Uploads <- Upload{Offset: int64(i), Bytes: []byte{byte(i)}}
		}
		connection.Drain()
		close(finished)
	}()
	for i := range 32 {
		up := recvUpload(t, link)
		if up.Offset != int64(i) || len(up.Bytes) != 1 || up.Bytes[0] != byte(i) {
			t.Fatalf("upload %d: %+v", i, up)
		}
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not join transport")
	}
}
