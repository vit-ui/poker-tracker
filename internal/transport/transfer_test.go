package transport

import (
	"strings"
	"testing"
	"time"
)

func TestTransferFrameValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request TransferRequest
		valid   bool
	}{
		{"manifest", TransferRequest{Kind: TransferBegin, ID: "owned", TotalBytes: 1, SHA256: strings.Repeat("a", 64)}, true},
		{"missing id", TransferRequest{Kind: TransferStatus}, false},
		{"empty manifest", TransferRequest{Kind: TransferBegin, ID: "owned", SHA256: strings.Repeat("a", 64)}, false},
		{"bad digest", TransferRequest{Kind: TransferBegin, ID: "owned", TotalBytes: 1, SHA256: "bad"}, false},
		{"empty chunk", TransferRequest{Kind: TransferChunk, ID: "owned"}, false},
		{"one byte", TransferRequest{Kind: TransferChunk, ID: "owned", Data: []byte{1}}, true},
		{"negative offset", TransferRequest{Kind: TransferChunk, ID: "owned", Offset: -1, Data: []byte{1}}, false},
		{"oversize", TransferRequest{Kind: TransferChunk, ID: "owned", Data: make([]byte, TransferChunkBytes+1)}, false},
		{"status has data", TransferRequest{Kind: TransferStatus, ID: "owned", Data: []byte{1}}, false},
		{"unknown", TransferRequest{Kind: "unknown", ID: "owned"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTransferRequest(tc.request); (err == nil) != tc.valid {
				t.Fatalf("error=%v valid=%v", err, tc.valid)
			}
		})
	}
}

func TestTransferCancellationInterruptsPendingWorker(t *testing.T) {
	link, info := startServer(t, "owned-token")
	stop := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, _, err := ConnectTransfer(info, TransferRequest{Kind: TransferStatus, ID: "owned-cancel"}, stop)
		finished <- err
	}()
	select {
	case <-link.Transfers:
	case <-time.After(time.Second):
		t.Fatal("transfer did not reach worker")
	}
	close(stop)
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled transfer succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left a transfer reader blocked")
	}
}

func TestTransferPreservesPinningAndAuthentication(t *testing.T) {
	_, info := startServer(t, "owned-token")
	for _, bad := range []string{"pin", "token"} {
		t.Run(bad, func(t *testing.T) {
			wrong := info
			if bad == "pin" {
				wrong.Fingerprint = strings.Repeat("0", 64)
			} else {
				wrong.Token = "wrong"
			}
			conn, _, err := ConnectTransfer(wrong, TransferRequest{Kind: TransferStatus, ID: "owned-auth"}, nil)
			if conn != nil {
				conn.Close()
			}
			if err == nil {
				t.Fatal("invalid transfer authentication succeeded")
			}
		})
	}
}
