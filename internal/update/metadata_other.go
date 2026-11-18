//go:build !windows

package update

func (*serviceState) syncMetadata(string) error { return nil }
