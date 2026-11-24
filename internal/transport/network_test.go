package transport

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

func startServer(t *testing.T, token string) (*Link, ConnInfo) {
	t.Helper()
	certPEM, keyPEM, fp, err := GenerateServerCert()
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}
	ln, err := Listen("127.0.0.1:0", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	serverLink := NewLink()
	go Serve(ln, token, serverLink)

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return serverLink, ConnInfo{Host: "127.0.0.1", Port: port, Token: token, Fingerprint: fp}
}

func recvUpload(t *testing.T, link *Link) Upload {
	t.Helper()
	select {
	case up := <-link.Uploads:
		return up
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an upload on the server")
		return Upload{}
	}
}

func recvCommand(t *testing.T, link *Link) Command {
	t.Helper()
	select {
	case cmd := <-link.Commands:
		return cmd
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a command on the client")
		return Command{}
	}
}

func TestNetworkRoundTrip(t *testing.T) {
	serverLink, info := startServer(t, "s3cr3t")

	clientLink, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}

	clientLink.Uploads <- Upload{FileID: "f1", Type: "cash", Offset: 0, Bytes: []byte("hello"), ScreenName: "Hero", ClientClock: time.Unix(1700000000, 0).UTC()}
	got := recvUpload(t, serverLink)
	if got.FileID != "f1" || got.Type != "cash" || string(got.Bytes) != "hello" || got.ScreenName != "Hero" {
		t.Fatalf("server received %+v, want the sent upload", got)
	}

	serverLink.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{Reason: "manual", DurationHours: 12}}
	cmd := recvCommand(t, clientLink)
	if cmd.Kind != CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "manual" || cmd.Lock.DurationHours != 12 {
		t.Fatalf("client received %+v, want the sent lock command", cmd)
	}
}

func TestNetworkReconnectReplacesConnection(t *testing.T) {
	serverLink, info := startServer(t, "tok")

	link1, dead1, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer (first): %v", err)
	}
	link1.Uploads <- Upload{FileID: "a", Type: "cash", Bytes: []byte("1")}
	recvUpload(t, serverLink)

	link2, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer (reconnect): %v", err)
	}
	link2.Uploads <- Upload{FileID: "a", Type: "cash", Bytes: []byte("2")}
	recvUpload(t, serverLink)

	select {
	case <-dead1:
	case <-time.After(2 * time.Second):
		t.Fatal("the first connection was not closed when the second connected")
	}

	serverLink.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{Reason: "manual", DurationHours: 1}}
	cmd := recvCommand(t, link2)
	if cmd.Kind != CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "manual" {
		t.Fatalf("reconnected collector received %+v, want the lock command", cmd)
	}
}

func TestNetworkLockHeldForDisconnectedCollector(t *testing.T) {
	serverLink, info := startServer(t, "tok")

	serverLink.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{
		Reason: "manual", DurationHours: 1, ExpiresAt: time.Now().Add(time.Hour),
	}}
	time.Sleep(100 * time.Millisecond)

	link, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}
	cmd := recvCommand(t, link)
	if cmd.Kind != CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "manual" {
		t.Fatalf("reconnected collector received %+v, want the held lock", cmd)
	}
}

func TestNetworkExpiredLockNotRepushed(t *testing.T) {
	serverLink, info := startServer(t, "tok")

	serverLink.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{
		Reason: "manual", ExpiresAt: time.Now().Add(-time.Hour),
	}}
	time.Sleep(100 * time.Millisecond)

	link, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}
	select {
	case cmd := <-link.Commands:
		t.Fatalf("an expired lock was re-pushed: %+v", cmd)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestNetworkRejectsBadToken(t *testing.T) {
	serverLink, info := startServer(t, "correct-token")
	info.Token = "wrong-token"

	clientLink, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}

	clientLink.Uploads <- Upload{FileID: "f1", Type: "cash", Bytes: []byte("x")}
	select {
	case up := <-serverLink.Uploads:
		t.Fatalf("server accepted an upload from a bad-token client: %+v", up)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestNetworkDropsUnauthenticatedIdleConnection(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	_, info := startServer(t, "tok")

	conn, err := tls.Dial("tcp", info.Address(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the server to close the unauthenticated connection")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("server did not drop the idle unauthenticated connection within handshakeTimeout")
	}
}

func TestNetworkRejectsBadFingerprint(t *testing.T) {
	_, info := startServer(t, "tok")
	info.Fingerprint = "0000000000000000000000000000000000000000000000000000000000000000"

	if _, _, err := DialServer(info); err == nil {
		t.Fatal("DialServer succeeded despite a fingerprint mismatch, want an error")
	}
}

func TestNetworkManyUploadsPreserveOrderAndContent(t *testing.T) {
	serverLink, info := startServer(t, "tok")
	clientLink, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}

	const n = 50
	go func() {
		for i := 0; i < n; i++ {
			clientLink.Uploads <- Upload{FileID: "f", Type: "cash", Offset: int64(i), Bytes: []byte(strconv.Itoa(i))}
		}
	}()
	for i := 0; i < n; i++ {
		up := recvUpload(t, serverLink)
		if up.Offset != int64(i) || string(up.Bytes) != strconv.Itoa(i) {
			t.Fatalf("upload %d arrived as offset=%d bytes=%q — order/content not preserved", i, up.Offset, string(up.Bytes))
		}
	}
}

func TestConnectionStringRoundTrip(t *testing.T) {
	s := ConnectionString("192.168.1.40", 8443, "tok123", "abcdef")
	want := "tracker://192.168.1.40:8443/tok123#fp=abcdef"
	if s != want {
		t.Fatalf("ConnectionString = %q, want %q", s, want)
	}
	info, err := ParseConnectionString(s)
	if err != nil {
		t.Fatalf("ParseConnectionString: %v", err)
	}
	if info.Host != "192.168.1.40" || info.Port != 8443 || info.Token != "tok123" || info.Fingerprint != "abcdef" {
		t.Fatalf("parsed %+v, want host/port/token/fp to match", info)
	}

	for _, bad := range []string{
		"http://host:1/tok#fp=x",
		"tracker://host/tok#fp=x",
		"tracker://host:1#fp=x",
		"tracker://host:1/tok",
		"tracker://host:1/tok#x=y",
	} {
		if _, err := ParseConnectionString(bad); err == nil {
			t.Errorf("ParseConnectionString(%q) = nil error, want a rejection", bad)
		}
	}
}
