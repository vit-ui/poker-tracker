package transport

import (
	"encoding/json"
	"testing"
)

func TestControlUploadsRetainConnectionIdentityLocally(t *testing.T) {
	link := NewLink()
	old := make(chan struct{})
	link.setCollectorDone(old)
	upload := Upload{collector: old, Status: &CollectorStatus{}}
	if connection, current := link.UploadConnection(upload); !current || connection != old {
		t.Fatal("current control upload rejected")
	}
	if _, err := json.Marshal(upload); err != nil {
		t.Fatalf("local connection identity leaked into wire frame: %v", err)
	}
	newConnection := make(chan struct{})
	link.setCollectorDone(newConnection)
	if connection, current := link.UploadConnection(upload); current || connection != old {
		t.Fatal("obsolete control state accepted as current")
	}
	if connection, current := link.UploadConnection(Upload{}); !current || connection != newConnection {
		t.Fatal("in-process upload did not use current link")
	}
}
