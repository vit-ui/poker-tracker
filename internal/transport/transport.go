package transport

import (
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

const (
	ImportStart = "import_start"
	ImportDone  = "import_done"
)

type CollectionReply struct {
	RequestID string
	Error     string
}

type LockReply struct {
	TriggerID string
	Lock      *config.LockFile
	Error     string
}

type CollectorStatus struct {
	Update          *config.UpdateReport
	Lock            *config.LockFile
	Error           string
	Settings        *config.CollectorSettings
	Watching        bool
	CollectionError string
}

type CollectorConfigReply struct {
	RequestID       string
	Settings        config.CollectorSettings
	Watching        bool
	Error           string
	CollectionError string
}

type UpdateReply struct {
	RequestID string
	Report    config.UpdateReport
	Error     string
}

type ScanChunk struct {
	RequestID  string
	ScreenName string
	Bytes      []byte
	Done       bool
	Error      string
}

type Upload struct {
	Scan            *ScanChunk
	Update          *config.UpdateReport
	UpdateReply     *UpdateReply
	collector       <-chan struct{} `json:"-"`
	Status          *CollectorStatus
	Recovery        bool
	FileActivity    time.Time
	Closure         *config.ClosureEvent
	LockReply       *LockReply
	CollectionReply *CollectionReply
	ConfigReply     *CollectorConfigReply
	FileID          string
	Type            string
	Offset          int64
	Bytes           []byte
	ScreenName      string
	ClientClock     time.Time

	Import bool

	PokerStarsRunning *bool `json:",omitempty"`
}

func (l *Link) UploadConnection(upload Upload) (<-chan struct{}, bool) {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	connection := upload.collector
	if connection == nil {
		connection = l.collectorDone
	}
	return connection, connection == l.collectorDone
}

const (
	CommandLock             = "lock"
	CommandClosureAck       = "closure_ack"
	CommandReplayClosures   = "replay_closures"
	CommandRescan           = "rescan"
	CommandScanFile         = "scan_file"
	CommandResyncFiles      = "resync_files"
	CommandPauseCollection  = "pause_collection"
	CommandDrainCollection  = "drain_collection"
	CommandResumeCollection = "resume_collection"
	CommandCollectorConfig  = "collector_config"
	CommandUpdate           = "update"
)

type Command struct {
	ScanPath  string
	Update    *config.UpdateRequest
	ClosureID string
	Files     []string
	RequestID string
	Kind      string
	Lock      *config.LockFile
	Settings  *config.CollectorSettings
}

type Link struct {
	InitialTransferID string
	Transfers         chan TransferCall
	ConnectionChanged chan struct{}
	connectionMu      sync.Mutex
	collectorDone     <-chan struct{}
	localDone         chan struct{}
	Uploads           chan Upload
	Commands          chan Command
}

func NewLink() *Link {
	localDone := make(chan struct{})
	return &Link{
		ConnectionChanged: make(chan struct{}, 1),
		Transfers:         make(chan TransferCall),
		collectorDone:     localDone,
		localDone:         localDone,
		Uploads:           make(chan Upload, 10),
		Commands:          make(chan Command, 10),
	}
}

func (l *Link) CollectorDone() <-chan struct{} {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	return l.collectorDone
}

func (l *Link) useNetwork() {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	close(l.localDone)
	l.collectorDone = l.localDone
	select {
	case l.ConnectionChanged <- struct{}{}:
	default:
	}
}

func (l *Link) setCollectorDone(done <-chan struct{}) {
	l.connectionMu.Lock()
	defer l.connectionMu.Unlock()
	l.collectorDone = done
	select {
	case l.ConnectionChanged <- struct{}{}:
	default:
	}
}
