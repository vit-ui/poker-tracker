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

const maxFrameBytes = 16 << 20

var handshakeTimeout = 10 * time.Second

type frame struct {
	Token         string           `json:"token,omitempty"`
	Upload        *Upload          `json:"upload,omitempty"`
	Command       *Command         `json:"command,omitempty"`
	Transfer      *TransferRequest `json:"transfer,omitempty"`
	TransferReply *TransferReply   `json:"transfer_reply,omitempty"`
}

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

type ConnInfo struct {
	Host        string
	Port        int
	Token       string
	Fingerprint string
}

func (c ConnInfo) Address() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func ConnectionString(host string, port int, token, fingerprint string) string {
	return fmt.Sprintf("tracker://%s/%s#fp=%s", net.JoinHostPort(host, strconv.Itoa(port)), token, fingerprint)
}

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

func FingerprintPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM certificate found")
	}
	sum := sha256.Sum256(block.Bytes)
	return fmt.Sprintf("%x", sum), nil
}

type CollectorConnection struct {
	Link        *Link
	Done        <-chan struct{}
	socket      *frameConn
	routines    sync.WaitGroup
	uploadsDone chan struct{}
}

func (c *CollectorConnection) Drain() {
	close(c.Link.Uploads)
	<-c.uploadsDone
	c.Close()
}

func (c *CollectorConnection) Close() { c.socket.close(); c.routines.Wait() }

func DialServer(info ConnInfo) (*Link, <-chan struct{}, error) {
	connection, err := ConnectCollector(info, nil)
	if err != nil {
		return nil, nil, err
	}
	return connection.Link, connection.Done, nil
}

func ConnectCollector(info ConnInfo, stop <-chan struct{}) (*CollectorConnection, error) {
	conn, err := dialPinned(info, stop)
	if err != nil {
		return nil, err
	}
	socket := &frameConn{Conn: conn, done: make(chan struct{})}
	connection := &CollectorConnection{Link: NewLink(), Done: socket.done, socket: socket, uploadsDone: make(chan struct{})}
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
		defer close(connection.uploadsDone)
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

func dialPinned(info ConnInfo, stop <-chan struct{}) (net.Conn, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
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
	return conn, nil
}

func Listen(addr string, certPEM, keyPEM []byte) (net.Listener, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, nil
}

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

type serveState struct {
	mu          sync.Mutex
	transferMu  sync.Mutex
	current     *frameConn
	activeLock  *config.LockFile
	connections map[net.Conn]struct{}
	done        chan struct{}
	closed      bool
	transferID  string
}

func Serve(ln net.Listener, token string, link *Link) {
	link.useNetwork()
	var routines sync.WaitGroup
	st := &serveState{connections: make(map[net.Conn]struct{}), done: make(chan struct{}), transferID: link.InitialTransferID}
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
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	first, err := readFrame(conn)
	if err != nil || first.Token != token || first.Upload != nil || first.Command != nil || first.TransferReply != nil {
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if first.Transfer != nil {
		serveTransfer(conn, *first.Transfer, link, st)
		return
	}
	framed := &frameConn{Conn: conn, done: make(chan struct{})}
	st.mu.Lock()
	if st.closed || st.transferID != "" {
		st.mu.Unlock()
		framed.close()
		return
	}
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
