package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// The network transport carries the same Upload/Command messages the in-process
// Link does, over one TLS connection the client dials out to the server. It is
// the second way of feeding a *Link's channels (the first is the in-process
// share): a per-connection pair of goroutines bridges the socket to the
// channels, so the Collector and Server are identical in both builds — only the
// construction differs. Auth is a pinned self-signed certificate (SSH-style
// trust-on-first-use: the client verifies the server's cert fingerprint rather
// than a CA chain) plus a shared token presented as the first frame.

// maxFrameBytes caps a single frame's payload so a malformed length prefix
// can't make the reader allocate unbounded memory. A hand-history delta or a
// tournament summary is far smaller than this.
const maxFrameBytes = 16 << 20 // 16 MiB

// handshakeTimeout bounds how long the server waits for a newly accepted
// connection to complete the TLS handshake and present its token. The token is
// only checked after the connection is accepted, so without this a peer that
// opens a socket and then stays silent — which needs no token and no LAN/VPS
// trust — would park a serveConn goroutine forever (readFrame blocks with no
// deadline), an unauthenticated resource-exhaustion vector. This is a
// deliberate, approved exception to the "no custom timeouts" rule (it is a
// security control, not an operation timeout); it applies only to the pre-auth
// window and is cleared once the token checks out, so an authenticated
// collector may stay idle between uploads. A var, not a const, only so the
// transport's own test can shorten it — never reassigned in any real build.
var handshakeTimeout = 10 * time.Second

// frame is one length-prefixed JSON message on the wire. Exactly one field is
// set: Token on the client's first (handshake) frame, then Upload frames going
// up or Command frames coming down.
type frame struct {
	Token   string   `json:"token,omitempty"`
	Upload  *Upload  `json:"upload,omitempty"`
	Command *Command `json:"command,omitempty"`
}

// GenerateServerCert creates a self-signed ECDSA P-256 certificate for the
// server to present, returning the PEM cert, PEM private key, and the SHA-256
// fingerprint (lowercase hex) of the certificate DER — the value the client
// pins via the connection string. The cert is long-lived (10 years) and self-
// signed; there is no CA, by design.
func GenerateServerCert() (certPEM, keyPEM []byte, fingerprint string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, "", fmt.Errorf("generating serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "self-tracker"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshaling key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	sum := sha256.Sum256(der)
	return certPEM, keyPEM, fmt.Sprintf("%x", sum), nil
}

// ConnInfo is the parsed content of a connection string.
type ConnInfo struct {
	Host        string
	Port        int
	Token       string
	Fingerprint string
}

// Address is the host:port the client dials.
func (c ConnInfo) Address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// ConnectionString formats the one-line pairing string a client pastes:
// tracker://host:port/token#fp=<sha256hex>.
func ConnectionString(host string, port int, token, fingerprint string) string {
	return fmt.Sprintf("tracker://%s/%s#fp=%s", net.JoinHostPort(host, strconv.Itoa(port)), token, fingerprint)
}

// ParseConnectionString parses a tracker:// pairing string. Every field is
// required; a missing scheme, port, token, or fingerprint is an error rather
// than a silent default, since a wrong value can't reach the server.
func ParseConnectionString(s string) (ConnInfo, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ConnInfo{}, fmt.Errorf("parsing connection string: %w", err)
	}
	if u.Scheme != "tracker" {
		return ConnInfo{}, fmt.Errorf("connection string must start with tracker://")
	}
	host := u.Hostname()
	if host == "" {
		return ConnInfo{}, fmt.Errorf("connection string missing host")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return ConnInfo{}, fmt.Errorf("connection string missing a valid port")
	}
	token := strings.TrimPrefix(u.Path, "/")
	if token == "" {
		return ConnInfo{}, fmt.Errorf("connection string missing token")
	}
	if !strings.HasPrefix(u.Fragment, "fp=") {
		return ConnInfo{}, fmt.Errorf("connection string missing #fp=<fingerprint>")
	}
	fp := strings.TrimPrefix(u.Fragment, "fp=")
	if fp == "" {
		return ConnInfo{}, fmt.Errorf("connection string has an empty fingerprint")
	}
	return ConnInfo{Host: host, Port: port, Token: token, Fingerprint: fp}, nil
}

// writeFrame writes one length-prefixed JSON frame. Only one goroutine may
// write to a given conn; server connections serialize through frameConn.write.
func writeFrame(w io.Writer, f frame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encoding frame: %w", err)
	}
	if len(payload) > maxFrameBytes {
		return fmt.Errorf("frame too large: %d bytes", len(payload))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// readFrame reads one length-prefixed JSON frame.
func readFrame(r io.Reader) (frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameBytes {
		return frame{}, fmt.Errorf("frame too large: %d bytes", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(payload, &f); err != nil {
		return frame{}, fmt.Errorf("decoding frame: %w", err)
	}
	return f, nil
}

// FingerprintPEM returns the SHA-256 fingerprint (lowercase hex) of the first
// certificate in a PEM block — the value pinned in a connection string.
func FingerprintPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM certificate found")
	}
	sum := sha256.Sum256(block.Bytes)
	return fmt.Sprintf("%x", sum), nil
}

// CollectorConnection owns its socket and all transport goroutines. Close also
// cancels a command delivery blocked behind a full collector channel.
type CollectorConnection struct {
	Link     *Link
	Done     <-chan struct{}
	socket   *frameConn
	routines sync.WaitGroup
}

func (c *CollectorConnection) Close() { c.socket.close(); c.routines.Wait() }

// DialServer connects to a server described by info over TLS, pinning the
// server's certificate to info.Fingerprint (no CA), presents the token, and
// returns a *Link whose channels are bridged to the connection: Uploads written
// to link.Uploads go up, Commands from the server arrive on link.Commands. The
// returned dead channel is closed when the connection drops, so the caller can
// close this connection and reconnect without stopping lock enforcement.
// Its listener owns shutdown; use ConnectCollector for explicit session ownership.
func DialServer(info ConnInfo) (*Link, <-chan struct{}, error) {
	connection, err := ConnectCollector(info, nil)
	if err != nil {
		return nil, nil, err
	}
	return connection.Link, connection.Done, nil
}

// ConnectCollector uses caller cancellation for dialing/handshake and active
// socket ownership. It adds no network timeout or reconnect policy.
func ConnectCollector(info ConnInfo, stop <-chan struct{}) (*CollectorConnection, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13, // both ends are our own code — require the newest TLS, no downgrade
		InsecureSkipVerify: true,             // we pin the exact cert below instead of a CA chain
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if fmt.Sprintf("%x", sum) != strings.ToLower(info.Fingerprint) {
				return fmt.Errorf("server certificate fingerprint mismatch")
			}
			return nil
		},
	}

	dialContext, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-stop:
			cancel()
		case <-dialContext.Done():
		}
	}()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(dialContext, "tcp", info.Address())
	cancel()
	<-cancelDone
	if err != nil {
		return nil, fmt.Errorf("dialing server: %w", err)
	}
	socket := &frameConn{Conn: conn, done: make(chan struct{})}
	connection := &CollectorConnection{Link: NewLink(), Done: socket.done, socket: socket}
	connection.routines.Go(func() {
		select {
		case <-stop:
			socket.close()
		case <-socket.done:
		}
	})
	if err := socket.write(frame{Token: info.Token}); err != nil {
		connection.Close()
		return nil, fmt.Errorf("sending token: %w", err)
	}
	connection.routines.Go(func() {
		defer socket.close()
		for {
			select {
			case <-socket.done:
				return
			case up, ok := <-connection.Link.Uploads:
				if !ok {
					return
				}
				if err := socket.write(frame{Upload: &up}); err != nil {
					return
				}
			}
		}
	})
	connection.routines.Go(func() {
		defer socket.close()
		for {
			f, err := readFrame(socket)
			if err != nil {
				return
			}
			if f.Command != nil {
				select {
				case connection.Link.Commands <- *f.Command:
				case <-socket.done:
					return
				}
			}
		}
	})
	return connection, nil
}

// Listen starts a TLS listener on addr with the given PEM cert/key. addr is a
// host:port (host empty = all interfaces).
func Listen(addr string, certPEM, keyPEM []byte) (net.Listener, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13, // both ends are our own code — require the newest TLS, no downgrade
	})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, nil
}

// frameConn serializes complete frames, including reconnect lock replay. Closing
// the socket does not take writeMu, so replacement can interrupt a blocked write.
type frameConn struct {
	net.Conn
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func (c *frameConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.Conn.Close()
	})
}

func (c *frameConn) write(f frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.Conn, f)
}

// serveState is Serve's shared per-listener state: the current collector
// connection and the most recent hard-stop lock, both guarded by mu. Tracking
// the active lock is what lets a lock survive a disconnected collector: it is
// re-pushed when a collector (re)connects, so a lock decided while the poker PC
// was offline — or lost to a socket that closed mid-write during a reconnect —
// is still delivered.
type serveState struct {
	mu          sync.Mutex
	current     *frameConn
	activeLock  *config.LockFile
	connections map[net.Conn]struct{}
	done        chan struct{}
	closed      bool
}

// Serve accepts client connections on ln and bridges each to link after the
// client presents the matching token. Only one collector is served at a time
// (v3.7 scope): a new connection replaces and closes the previous one, and a
// single goroutine is the sole consumer of link.Commands, writing each command
// to whichever collector is current. This is what keeps a hard-stop lock from
// being split between a live and a stale connection — the bug otherwise is that
// after an ungraceful drop and collector reconnect, two per-connection
// writers would both drain link.Commands and one could write a lock into the
// dead socket's buffer (no error), silently losing enforcement.
//
// A lock command is also remembered as the active lock and re-pushed to a
// collector when it connects (see serveConn), so the two remaining gaps are
// closed: (1) a lock decided while no collector is connected is no longer
// dropped, and (2) a lock racing a reconnect can't be lost to the just-closed
// old socket — storing the lock and reading current happen under the same mutex
// as the connection swap and its re-push, so whichever orders first, the new
// collector still receives it (a duplicate re-push is harmless: Enforce is
// idempotent). Blocks until ln is closed.
func Serve(ln net.Listener, token string, link *Link) {
	link.useNetwork()
	var routines sync.WaitGroup
	st := &serveState{connections: make(map[net.Conn]struct{}), done: make(chan struct{})}
	defer func() {
		st.mu.Lock()
		st.closed = true
		close(st.done)
		if st.current != nil {
			st.current.close()
		}
		for conn := range st.connections {
			conn.Close()
		}
		st.mu.Unlock()
		routines.Wait()
	}()

	// The sole command writer: one consumer of link.Commands, targeting the
	// current collector. Storing the active lock and reading the current
	// connection are one critical section, serialized against serveConn's
	// connection swap + re-push (see the type doc), so a lock is never lost to a
	// reconnect and is re-delivered to whoever connects next.
	routines.Add(1)
	go func() {
		defer routines.Done()
		for {
			var cmd Command
			select {
			case <-st.done:
				return
			case value, ok := <-link.Commands:
				if !ok {
					return
				}
				cmd = value
			}
			st.mu.Lock()
			if cmd.Kind == CommandLock && cmd.Lock != nil {
				st.activeLock = cmd.Lock
			}
			conn := st.current
			st.mu.Unlock()
			if conn == nil {
				if cmd.Kind == CommandLock && cmd.Lock != nil {
					slog.Warn("collector disconnected; lock retained for reconnect")
				} else {
					slog.Warn("collector disconnected; command not delivered", "kind", cmd.Kind)
				}
				continue
			}
			if err := conn.write(frame{Command: &cmd}); err != nil {
				slog.Error("sending command to collector", "err", err)
				conn.close()
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		st.mu.Lock()
		st.connections[conn] = struct{}{}
		st.mu.Unlock()
		enableKeepAlive(conn)
		routines.Add(1)
		go func() { defer routines.Done(); serveConn(conn, token, link, st) }()
	}
}

func serveConn(conn net.Conn, token string, link *Link, st *serveState) {
	defer func() {
		conn.Close()
		st.mu.Lock()
		delete(st.connections, conn)
		st.mu.Unlock()
	}()
	// Bound the pre-auth window: the TLS handshake happens on this first read,
	// and the token frame must arrive within it, or the connection is dropped
	// instead of blocking this goroutine indefinitely (see handshakeTimeout).
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	first, err := readFrame(conn)
	if err != nil || first.Token != token {
		conn.Close()
		return
	}
	// Authenticated: clear the deadline so an idle-but-live collector (long gaps
	// between uploads) isn't dropped; a real drop is caught by TCP keepalive.
	_ = conn.SetReadDeadline(time.Time{})
	// Become the active collector, closing any previous one so its reader exits
	// and it stops being a target for commands. Under the same lock, snapshot
	// any still-active lock so it can be re-pushed to this fresh connection —
	// covering a lock decided while nothing was connected and a lock lost to a
	// socket that closed mid-write during this very reconnect.
	framed := &frameConn{Conn: conn, done: make(chan struct{})}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		framed.close()
		return
	}
	// Reserve the first write before publishing the connection. A newer lock
	// must follow the replay, rather than be overwritten by an older lock.
	framed.writeMu.Lock()
	old := st.current
	st.current = framed
	link.setCollectorDone(framed.done)
	lock := st.activeLock
	st.mu.Unlock()
	if old != nil {
		old.close()
	}
	if lock != nil && !config.IsLockExpired(lock) {
		if err := writeFrame(conn, frame{Command: &Command{Kind: CommandLock, Lock: lock}}); err != nil {
			slog.Error("re-pushing active lock to reconnected collector", "err", err)
			framed.close()
		}
	}
	framed.writeMu.Unlock()
	defer func() {
		st.mu.Lock()
		if st.current == framed {
			st.current = nil
		}
		st.mu.Unlock()
		framed.close()
	}()
	for {
		f, err := readFrame(conn)
		if err != nil {
			return
		}
		if f.Upload != nil {
			f.Upload.collector = framed.done
			if reply := f.Upload.LockReply; reply != nil && reply.Error == "" {
				st.mu.Lock()
				if st.current == framed {
					st.activeLock = reply.Lock
				}
				st.mu.Unlock()
			}
			select {
			case link.Uploads <- *f.Upload:
			case <-framed.done:
				return
			case <-st.done:
				return
			}
		}
	}
}

// enableKeepAlive turns on TCP keepalive so a collector that drops ungracefully
// (power loss, Wi-Fi cut, reboot — no FIN) is detected in seconds rather than
// the OS default of minutes, so its reader errors out and it stops being the
// current connection promptly.
func enableKeepAlive(conn net.Conn) {
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if tcp, ok := tlsConn.NetConn().(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(15 * time.Second)
	}
}
