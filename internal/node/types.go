package node

import (
	"context"
	"errors"
	"time"
)

var (
	ErrMountDisabled    = errors.New("node mount is disabled")
	ErrBadSourceSubPath = errors.New("bad source subpath")
)

type Config struct {
	DriverName        string
	StagingRoot       string
	HostProcRoot      string
	EnableMount       bool
	UnstageAfterMount bool
	Timeout           time.Duration
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
}
