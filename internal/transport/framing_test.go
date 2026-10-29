package transport

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func TestConcurrentCompleteFrames(t *testing.T) {
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	conn := &frameConn{Conn: writer}
	const count = 64
	errs := make(chan error, count)
	var writers sync.WaitGroup
	for i := 0; i < count; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			errs <- conn.write(frame{Command: &Command{Kind: fmt.Sprintf("%d:%s", i, strings.Repeat("payload", 1024))}})
		}(i)
	}
	// A broken frame must fail the test rather than leave a pipe read blocked.
	if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for i := 0; i < count; i++ {
		f, err := readFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		if f.Command == nil || seen[f.Command.Kind] {
			t.Fatalf("missing or duplicate command: %+v", f.Command)
		}
		seen[f.Command.Kind] = true
	}
	writers.Wait()
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		want := fmt.Sprintf("%d:%s", i, strings.Repeat("payload", 1024))
		if !seen[want] {
			t.Fatalf("command %d changed or missing", i)
		}
	}
}

func TestReconnectReplayWithCommandPressure(t *testing.T) {
	server, info := startServer(t, "framing-token")
	link, _, err := DialServer(info)
	if err != nil {
		t.Fatal(err)
	}
	link.Uploads <- Upload{FileID: "ready"}
	recvUpload(t, server)
	server.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{Reason: "test", ExpiresAt: time.Now().Add(time.Hour)}}
	recvCommand(t, link)
	for round := 0; round < 12; round++ {
		next, _, err := DialServer(info)
		if err != nil {
			t.Fatal(err)
		}
		// The upload proves the connection has become current. Its replay and the
		// normal writer still share the same framed write path.
		next.Uploads <- Upload{FileID: fmt.Sprint(round)}
		recvUpload(t, server)
		const count = 32
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			for i := 0; i < count; i++ {
				server.Commands <- Command{Kind: fmt.Sprintf("%d/%d:%s", round, i, strings.Repeat("x", 4096))}
			}
		}()
		received := 0
		for received < count {
			cmd := recvCommand(t, next)
			if cmd.Kind == CommandLock {
				continue
			}
			want := fmt.Sprintf("%d/%d:%s", round, received, strings.Repeat("x", 4096))
			if cmd.Kind != want {
				t.Fatalf("round %d command %d changed or out of order", round, received)
			}
			received++
		}
		<-sent
	}
}

func TestListenerShutdownClosesCollectorWithBlockedUpload(t *testing.T) {
	cert, key, fp, err := GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := Listen("127.0.0.1:0", cert, key)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := NewLink()
	// No worker consumes uploads; shutdown must release a reader blocked here.
	server.Uploads = make(chan Upload)
	stopped := make(chan struct{})
	go func() { Serve(listener, "shutdown-token", server); close(stopped) }()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	collector, dead, err := DialServer(ConnInfo{Host: "127.0.0.1", Port: port, Token: "shutdown-token", Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	collector.Uploads <- Upload{FileID: "blocked"}
	// A returned command proves authentication completed, without consuming the
	// blocked upload or relying on a sleep to order shutdown against setup.
	server.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{ExpiresAt: time.Now().Add(time.Hour)}}
	recvCommand(t, collector)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
	select {
	case <-dead:
	case <-time.After(2 * time.Second):
		t.Fatal("collector connection remained open after listener shutdown")
	}
}

func TestConnectionCloseInterruptsFrameWriter(t *testing.T) {
	writer, reader := net.Pipe()
	defer reader.Close()
	conn := &frameConn{Conn: writer, done: make(chan struct{})}
	defer writer.Close()
	result := make(chan error, 1)
	go func() { result <- conn.write(frame{Command: &Command{Kind: strings.Repeat("x", 65536)}}) }()
	if err := reader.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	// The frame body cannot complete while the peer stops reading. Closing must
	// bypass writeMu so replacing this collector can release the blocked writer.
	closed := make(chan struct{})
	go func() { conn.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close waited for the blocked frame writer")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blocked frame unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame writer remained blocked after close")
	}
}
