package transport

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"
)

// Transfer request kinds share the authenticated framing with collector traffic.
const (
	TransferReserve  = "reserve"
	TransferBegin    = "begin"
	TransferChunk    = "chunk"
	TransferVerify   = "verify"
	TransferStatus   = "status"
	TransferActivate = "activate"
	TransferRollback = "rollback"
)

// Transfer phases describe durable worker state, never socket state.
const (
	TransferPhaseReserved         = "reserved"
	TransferPhaseReceiving        = "receiving"
	TransferPhaseImporting        = "importing"
	TransferPhaseVerified         = "verified"
	TransferPhaseApplyingSettings = "applying-settings"
	TransferPhaseRollingBack      = "rolling-back"
	TransferPhaseActivated        = "activated"
	TransferPhaseRolledBack       = "rolled-back"
)

// TransferChunkBytes leaves ample room for JSON/base64 below maxFrameBytes.
const TransferChunkBytes = 1 << 20

// TransferRequest carries one manifest, chunk or durable-state operation.
// The destination worker interprets Data; transport never parses its contents.
type TransferRequest struct {
	Kind           string  `json:"kind"`
	TelegramChatID *string `json:"telegram_chat_id,omitempty"`
	ID             string  `json:"id"`
	Offset         int64   `json:"offset,omitempty"`
	TotalBytes     int64   `json:"total_bytes,omitempty"`
	SHA256         string  `json:"sha256,omitempty"`
	Data           []byte  `json:"data,omitempty"`
}

// TransferReply acknowledges durable progress or a worker refusal.
type TransferReply struct {
	ID         string `json:"id"`
	Phase      string `json:"phase"`
	NextOffset int64  `json:"next_offset,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Error      string `json:"error,omitempty"`
}

// TransferCall is serialized with destination worker operations. Reply must have
// capacity one, so a disconnected caller cannot block worker completion.
type TransferCall struct {
	Request TransferRequest
	Reply   chan TransferReply
}

// TransferConnection owns a pinned TLS transfer session and cancellation loop.
// Exchanges are serialized; Close interrupts and joins an in-flight exchange.
type TransferConnection struct {
	socket    *frameConn
	exchanges sync.Mutex
	routines  sync.WaitGroup
}

// ConnectTransfer authenticates as a transfer peer without replacing a collector.
// first may begin a transfer or query a previous durable receipt. Worker refusals
// are returned in reply.Error; network/authentication errors are returned as err.
func ConnectTransfer(info ConnInfo, first TransferRequest, stop <-chan struct{}) (*TransferConnection, TransferReply, error) {
	if err := validateTransferRequest(first); err != nil {
		return nil, TransferReply{}, err
	}
	conn, err := dialPinned(info, stop)
	if err != nil {
		return nil, TransferReply{}, err
	}
	socket := &frameConn{Conn: conn, done: make(chan struct{})}
	session := &TransferConnection{socket: socket}
	session.routines.Go(func() {
		select {
		case <-stop:
			socket.close()
		case <-socket.done:
		}
	})
	reply, err := session.exchange(frame{Token: info.Token, Transfer: &first}, first.ID)
	if err != nil {
		session.Close()
		return nil, TransferReply{}, err
	}
	return session, reply, nil
}

// Exchange sends one request and waits for its corresponding durable reply.
func (c *TransferConnection) Exchange(request TransferRequest) (TransferReply, error) {
	if err := validateTransferRequest(request); err != nil {
		return TransferReply{}, err
	}
	return c.exchange(frame{Transfer: &request}, request.ID)
}

func (c *TransferConnection) exchange(out frame, id string) (TransferReply, error) {
	c.exchanges.Lock()
	defer c.exchanges.Unlock()
	if err := c.socket.write(out); err != nil {
		return TransferReply{}, fmt.Errorf("sending transfer request: %w", err)
	}
	incoming, err := readFrame(c.socket)
	if err != nil {
		return TransferReply{}, fmt.Errorf("receiving transfer reply: %w", err)
	}
	if incoming.TransferReply == nil || incoming.Token != "" || incoming.Transfer != nil || incoming.Upload != nil || incoming.Command != nil || incoming.TransferReply.ID != id {
		c.socket.close()
		return TransferReply{}, fmt.Errorf("invalid transfer reply")
	}
	return *incoming.TransferReply, nil
}

// Close cancels the socket and joins owned work without acquiring its write lock.
func (c *TransferConnection) Close() {
	c.socket.close()
	c.exchanges.Lock()
	c.exchanges.Unlock()
	c.routines.Wait()
}

func validateTransferRequest(request TransferRequest) error {
	if request.ID == "" {
		return fmt.Errorf("transfer request has no identity")
	}
	switch request.Kind {
	case TransferReserve:
		if request.TotalBytes != 0 || request.SHA256 != "" {
			return fmt.Errorf("reservation must precede the transfer manifest")
		}
	case TransferBegin:
		digest, err := hex.DecodeString(request.SHA256)
		if err != nil || len(digest) != 32 || request.TotalBytes <= 0 {
			return fmt.Errorf("transfer begin requires a byte length and SHA256 digest")
		}
	case TransferChunk:
		if len(request.Data) == 0 {
			return fmt.Errorf("transfer chunk is empty")
		}
	case TransferVerify, TransferStatus, TransferActivate, TransferRollback:
	default:
		return fmt.Errorf("unknown transfer request kind %q", request.Kind)
	}
	if request.Offset < 0 || request.TotalBytes < 0 {
		return fmt.Errorf("transfer request has a negative byte position")
	}
	if len(request.Data) > TransferChunkBytes {
		return fmt.Errorf("transfer chunk exceeds %d bytes", TransferChunkBytes)
	}
	if request.Kind != TransferChunk && len(request.Data) != 0 {
		return fmt.Errorf("only transfer chunks may contain data")
	}
	return nil
}

func (s *serveState) reserveTransfer(request TransferRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("server transport is stopping")
	}
	if request.Kind == TransferStatus {
		if s.transferID != "" && s.transferID != request.ID {
			return fmt.Errorf("another transfer reserves this destination")
		}
		return nil
	}
	if s.current != nil {
		return fmt.Errorf("destination already has a collector connected")
	}
	if (request.Kind == TransferReserve || request.Kind == TransferBegin || request.Kind == TransferRollback) && s.transferID == "" {
		s.transferID = request.ID
	}
	if s.transferID != request.ID {
		return fmt.Errorf("destination is not reserved for this transfer")
	}
	return nil
}

func (s *serveState) observeTransfer(request TransferRequest, reply TransferReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transferID != request.ID || reply.ID != request.ID {
		return
	}
	switch reply.Phase {
	case "", TransferPhaseActivated, TransferPhaseRolledBack:
		s.transferID = ""
	}
}

func (s *serveState) dispatchTransfer(request TransferRequest, link *Link, disconnected <-chan struct{}) TransferReply {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	reply := TransferReply{ID: request.ID}
	if err := validateTransferRequest(request); err != nil {
		reply.Error = err.Error()
		return reply
	}
	if err := s.reserveTransfer(request); err != nil {
		reply.Error = err.Error()
		return reply
	}
	call := TransferCall{Request: request, Reply: make(chan TransferReply, 1)}
	select {
	case link.Transfers <- call:
	case <-disconnected:
		return reply
	case <-s.done:
		return reply
	}
	// Once accepted, consume the durable result even if the peer disconnects.
	// Admission must observe this result before any later status/reservation.
	select {
	case reply = <-call.Reply:
	case <-s.done:
		return TransferReply{ID: request.ID, Error: "server transport is stopping"}
	}
	if reply.ID != request.ID {
		return TransferReply{ID: request.ID, Error: "worker returned a different transfer identity"}
	}
	s.observeTransfer(request, reply)
	return reply
}

func serveTransfer(conn net.Conn, first TransferRequest, link *Link, state *serveState) {
	socket := &frameConn{Conn: conn, done: make(chan struct{})}
	incoming, readerDone := make(chan TransferRequest), make(chan struct{})
	go func() {
		defer close(readerDone)
		defer socket.close()
		for {
			next, err := readFrame(socket)
			if err != nil {
				return
			}
			if next.Transfer == nil || next.Token != "" || next.Upload != nil || next.Command != nil || next.TransferReply != nil {
				return
			}
			select {
			case incoming <- *next.Transfer:
			case <-socket.done:
				return
			}
		}
	}()
	defer func() { socket.close(); <-readerDone }()
	request := first
	for {
		reply := state.dispatchTransfer(request, link, socket.done)
		if err := socket.write(frame{TransferReply: &reply}); err != nil {
			return
		}
		select {
		case request = <-incoming:
		case <-socket.done:
			return
		case <-state.done:
			return
		}
	}
}
