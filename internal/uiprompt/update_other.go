//go:build !windows

package uiprompt

func ShowUpdateProgress(executable, requestID string, telegram bool) error {
	return nil
}
