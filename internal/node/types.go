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
	// A failed injection remains attached. Retain its lease and desired state
	// so that later unmounts and authorization revocation can still find it.
	ErrMountCleanupPending = errors.New("mount cleanup is pending")
)

// DefaultCreatedSubPathMode is the default permission mode requested for
// source subpath directories created by the node mounter. The process umask
// still applies, and existing directory modes are never changed.
const DefaultCreatedSubPathMode os.FileMode = 0o770

const JuiceFSDriver = "csi.juicefs.com"

type Config struct {
	DriverName            string
	StagingRoot           string
	HostProcRoot          string
	EnableMount           bool
	UnstageAfterMount     bool
	CreateMissingSubPaths bool
	CreatedSubPathMode    os.FileMode
	Timeout               time.Duration
	JuiceFSMountMode      string
	JuiceFSSocket         string
	JuiceFSPublishRoot    string
	JuiceFSStateDir       string
}

type PersistentVolume struct {
	ReadOnly     bool
	Driver       string
	FSType       string
	Attributes   map[string]string
	Secrets      map[string]string
	Name         string
	VolumeHandle string
	Server       string
	Share        string
	SubDir       string
	MountOptions []string
}

type MountPlan struct {
	PodNamespace   string
	PodName        string
	ServiceAccount string
	ReadOnly       bool
	PV             PersistentVolume
	PodUID         string
	ContainerName  string
	ContainerID    string
	SourceSubPath  string
	TargetPath     string
}

type Mounter interface {
	Mount(ctx context.Context, plan MountPlan) error
	Unmount(ctx context.Context, plan MountPlan) error
	IsMounted(ctx context.Context, plan MountPlan) (bool, error)
}
