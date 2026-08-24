package node

import (
	"context"
	"errors"
	"os"
	"time"
)

var (
	ErrMountDisabled    = errors.New("node mount is disabled")
	ErrBadSourceSubPath = errors.New("bad source subpath")
)

// DefaultCreatedSubPathMode is the default permission mode requested for
// source subpath directories created by the node mounter. The process umask
// still applies, and existing directory modes are never changed.
const DefaultCreatedSubPathMode os.FileMode = 0o770

type Config struct {
	DriverName            string
	StagingRoot           string
	HostProcRoot          string
	EnableMount           bool
	UnstageAfterMount     bool
	CreateMissingSubPaths bool
	CreatedSubPathMode    os.FileMode
	Timeout               time.Duration
}

type PersistentVolume struct {
	Name         string
	VolumeHandle string
	Server       string
	Share        string
	SubDir       string
	MountOptions []string
}

type MountPlan struct {
	PV            PersistentVolume
	PodUID        string
	ContainerName string
	ContainerID   string
	SourceSubPath string
	TargetPath    string
}

type Mounter interface {
	Mount(ctx context.Context, plan MountPlan) error
	Unmount(ctx context.Context, plan MountPlan) error
	IsMounted(ctx context.Context, plan MountPlan) (bool, error)
}
