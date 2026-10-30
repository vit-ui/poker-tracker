package client

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestNetworkCollectorRecoversFromBootOutageAndDisconnect(t *testing.T) {
	verifyNetworkCollectorRecovery(t, false)
}

// Active enforcement is invoked only by non-Windows tests or the coordinated,
// ignored native check. Ordinary Windows tests must never terminate PokerStars.
func verifyNetworkCollectorRecovery(t *testing.T, enforce bool) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	name := "Hero"
	hh, ts := collectionFolders(t, root, name)
	cert, key, fp, err := transport.GenerateServerCert()
	if err != nil {
		t.Fatal(err)
	}
	// Reserve an address, then close it to reproduce unavailable-at-boot.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	host, portText, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portText)
	connectionString := transport.ConnectionString(host, port, "owned-token", fp)
	if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{ConnectionString: &connectionString, PokerStarsScreenName: &name}, Paths: config.Paths{HandHistory: &hh, TournamentSummary: &ts}}); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "lockfile.json")
	enforcer := NewEnforcer(lockPath)
	lock := config.LockFile{TriggerID: "owned-outage-lock", ExpiresAt: time.Now().Add(time.Hour)}
	if enforce {
		if err := config.SaveLock(lockPath, &lock); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- runNetworkCollector(path, enforcer, done, 20*time.Millisecond) }()
	defer func() {
		close(done)
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("network collector did not stop")
		}
	}()
	time.Sleep(70 * time.Millisecond)
	select {
	case err := <-finished:
		t.Fatalf("boot outage stopped collector: %v", err)
	default:
	}
	for cycle := range 3 {
		listener, err := transport.Listen(address, cert, key)
		if err != nil {
			t.Fatal(err)
		}
		link := transport.NewLink()
		served := make(chan struct{})
		go func() { transport.Serve(listener, "owned-token", link); close(served) }()
		up := collectionUpload(t, link)
		if up.Status == nil || !up.Status.Watching {
			listener.Close()
			<-served
			t.Fatalf("reconnect %d missing ready status: %+v", cycle, up)
		}
		if enforce && (up.Status.Lock == nil || !up.Status.Lock.ExpiresAt.Equal(lock.ExpiresAt)) {
			t.Fatal("reconnect changed active lock expiry")
		}
		// Exercise live collection after each reconnect, including a fresh offset map.
		file := filepath.Join(hh, name, "cycle"+strconv.Itoa(cycle)+".txt")
		if err := os.WriteFile(file, []byte("cycle data"), 0600); err != nil {
			t.Fatal(err)
		}
		up = collectionUpload(t, link)
		if up.FileID != file || up.Offset != 0 || string(up.Bytes) != "cycle data" {
			t.Fatalf("bad recovered collection: %+v", up)
		}
		listener.Close()
		<-served
		if enforce {
			lock.TriggerID = "owned-outage-" + strconv.Itoa(cycle)
			lock.ExpiresAt = lock.ExpiresAt.Add(time.Hour)
			if err := config.SaveLock(lockPath, &lock); err != nil {
				t.Fatal(err)
			}
			awaitEnforcedExpiry(t, enforcer, lock.ExpiresAt)
			stored, err := config.LoadLock(lockPath)
			if err != nil || !stored.ExpiresAt.Equal(lock.ExpiresAt) {
				t.Fatalf("outage altered expiry: %+v %v", stored, err)
			}
		}
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-finished:
			t.Fatalf("disconnect stopped collector: %v", err)
		default:
		}
	}
}

func verifyNetworkCollectorEnforcesInvalidPairing(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	connection := "tracker://127.0.0.1:1/owned#fp=invalid"
	// Invalid pairing still cannot disable an existing lock or terminate checks.
	if err := config.SaveConfig(path, &config.Config{Setup: config.Setup{ConnectionString: &connection}}); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "lockfile.json")
	enforcer := NewEnforcer(lockPath)
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- runNetworkCollector(path, enforcer, done, 30*time.Millisecond) }()
	defer func() {
		close(done)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	for i := range 2 {
		lock := config.LockFile{TriggerID: "outage-" + strconv.Itoa(i), ExpiresAt: time.Now().Add(time.Duration(i+1) * time.Hour)}
		if err := config.SaveLock(lockPath, &lock); err != nil {
			t.Fatal(err)
		}
		awaitEnforcedExpiry(t, enforcer, lock.ExpiresAt)
		stored, err := config.LoadLock(lockPath)
		if err != nil || !stored.ExpiresAt.Equal(lock.ExpiresAt) {
			t.Fatalf("outage changed expiry: %+v %v", stored, err)
		}
	}
}

func awaitEnforcedExpiry(t *testing.T, enforcer *Enforcer, expiry time.Time) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		enforcer.mu.Lock()
		observed := enforcer.active != nil && enforcer.active.ExpiresAt.Equal(expiry)
		enforcer.mu.Unlock()
		if observed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("enforcement stopped during outage")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
