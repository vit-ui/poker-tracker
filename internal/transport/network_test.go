package transport

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// startServer generates a cert, listens on localhost:0, serves the given token
// against a fresh server-side Link, and returns that Link plus a ConnInfo a
// client can dial. The listener is closed on cleanup.
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

// TestNetworkRoundTrip runs a real localhost TLS connection end to end: the
// client dials with the pinned fingerprint + token, an Upload written on the
// client Link arrives on the server Link, and a Command written on the server
// Link arrives on the client Link.
func TestNetworkRoundTrip(t *testing.T) {
	serverLink, info := startServer(t, "s3cr3t")

	clientLink, _, err := DialServer(info)
	if err != nil {
		t.Fatalf("DialServer: %v", err)
	}

	// Up: client -> server.
	clientLink.Uploads <- Upload{FileID: "f1", Type: "cash", Offset: 0, Bytes: []byte("hello"), ScreenName: "Hero", ClientClock: time.Unix(1700000000, 0).UTC()}
	got := recvUpload(t, serverLink)
	if got.FileID != "f1" || got.Type != "cash" || string(got.Bytes) != "hello" || got.ScreenName != "Hero" {
		t.Fatalf("server received %+v, want the sent upload", got)
	}

	// Down: server -> client.
	serverLink.Commands <- Command{Kind: CommandLock, Lock: &config.LockFile{Reason: "manual", DurationHours: 12}}
	cmd := recvCommand(t, clientLink)
	if cmd.Kind != CommandLock || cmd.Lock == nil || cmd.Lock.Reason != "manual" || cmd.Lock.DurationHours != 12 {
		t.Fatalf("client received %+v, want the sent lock command", cmd)
	}
}

// TestNetworkRejectsBadToken confirms a client presenting the wrong token is
// dropped: its uploads never reach the server.
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
		// Correct: the server dropped the connection at the token check.
	}
}

// TestNetworkRejectsBadFingerprint confirms the client refuses to connect when
// the pinned fingerprint doesn't match the server's actual certificate.
func TestNetworkRejectsBadFingerprint(t *testing.T) {
	_, info := startServer(t, "tok")
	info.Fingerprint = "0000000000000000000000000000000000000000000000000000000000000000"

	if _, _, err := DialServer(info); err == nil {
		t.Fatal("DialServer succeeded despite a fingerprint mismatch, want an error")
	}
}

// TestConnectionStringRoundTrip pins the pairing-string format and parse.
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
		"http://host:1/tok#fp=x",   // wrong scheme
		"tracker://host/tok#fp=x",  // no port
		"tracker://host:1#fp=x",    // no token
		"tracker://host:1/tok",     // no fingerprint
		"tracker://host:1/tok#x=y", // fragment not fp=
	} {
		if _, err := ParseConnectionString(bad); err == nil {
			t.Errorf("ParseConnectionString(%q) = nil error, want a rejection", bad)
		}
	}
}
