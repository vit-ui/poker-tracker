package bot

type OwnerControl struct {
	Pause       bool
	StopPolling bool
	Reply       chan error
}
