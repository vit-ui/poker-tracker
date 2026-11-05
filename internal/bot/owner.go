package bot

// OwnerControl pauses command mutations and acknowledges after accepted worker
// operations finish. Manual protection remains available while collection holds.
type OwnerControl struct {
	Pause       bool
	StopPolling bool
	Reply       chan error
}
