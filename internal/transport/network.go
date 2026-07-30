package transport

import (
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
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
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
// write to a given conn (the transport keeps a single writer per direction).
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

// DialServer connects to a server described by info over TLS, pinning the
// server's certificate to info.Fingerprint (no CA), presents the token, and
// returns a *Link whose channels are bridged to the connection: Uploads written
// to link.Uploads go up, Commands from the server arrive on link.Commands. The
// returned conn is closed when the read pump ends; close it to stop the client.
func DialServer(info ConnInfo) (*Link, net.Conn, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: true, // we pin the exact cert below instead of a CA chain
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
	conn, err := tls.Dial("tcp", info.Address(), cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing server: %w", err)
	}
	if err := writeFrame(conn, frame{Token: info.Token}); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("sending token: %w", err)
	}

	link := NewLink()
	go func() { // upload writer: the only writer to conn on the client side
		for up := range link.Uploads {
			if err := writeFrame(conn, frame{Upload: &up}); err != nil {
				conn.Close()
				return
			}
		}
	}()
	go func() { // command reader
		defer conn.Close()
		for {
			f, err := readFrame(conn)
			if err != nil {
				return
			}
			if f.Command != nil {
				link.Commands <- *f.Command
			}
		}
	}()
	return link, conn, nil
}

// Listen starts a TLS listener on addr with the given PEM cert/key. addr is a
// host:port (host empty = all interfaces).
func Listen(addr string, certPEM, keyPEM []byte) (net.Listener, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return ln, nil
}

// Serve accepts client connections on ln and bridges each to link after the
// client presents the matching token. One client is served at a time (v3.7
// scope); a new connection's pumps attach to the same link. Blocks until ln is
// closed. A connection presenting the wrong token, or whose first frame isn't
// the token handshake, is dropped.
func Serve(ln net.Listener, token string, link *Link) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(conn, token, link)
	}
}

func serveConn(conn net.Conn, token string, link *Link) {
	first, err := readFrame(conn)
	if err != nil || first.Token != token {
		conn.Close()
		return
	}
	done := make(chan struct{})
	go func() { // command writer: the only writer to conn on the server side
		for {
			select {
			case <-done:
				return
			case cmd := <-link.Commands:
				if err := writeFrame(conn, frame{Command: &cmd}); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	// upload reader
	for {
		f, err := readFrame(conn)
		if err != nil {
			close(done)
			conn.Close()
			return
		}
		if f.Upload != nil {
			link.Uploads <- *f.Upload
		}
	}
}
