package deployment

import (
	"time"

	"github.com/vit-ui/self-tracker/internal/storage"
)

func storagePrecision(store storage.Store) time.Duration {
	if store.GetSheetURL() != "" {
		return time.Minute
	}
	return time.Second
}
