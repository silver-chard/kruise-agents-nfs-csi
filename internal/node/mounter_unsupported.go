//go:build !linux

package node

import (
	"context"
	"fmt"
)

type unsupportedMounter struct{}

func NewMounter(Config) Mounter {
	return unsupportedMounter{}
}

// CleanupStagingRoot is a no-op on unsupported platforms.
func CleanupStagingRoot(string) error {
	return nil
}

func (unsupportedMounter) Mount(context.Context, MountPlan) error {
	return fmt.Errorf("node mount is only supported on linux")
}
