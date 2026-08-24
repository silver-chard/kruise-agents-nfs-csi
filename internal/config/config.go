package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	DefaultDriverName    = "csi.nfs.zhida"
	DefaultTokenAudience = "kruise-agents-nfs-csi.zhida/sandbox-mounter"
	DefaultSocketPath    = "/var/lib/kruise-agents-nfs-csi/wrapper.sock"
	DefaultTokenFile     = "/var/run/secrets/kruise-agents-nfs-csi/token"
	DefaultNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	DefaultPodInfoDir    = "/var/run/secrets/kruise-agents-nfs-csi"
	DefaultPodNameFile   = DefaultPodInfoDir + "/pod_name"
	DefaultPodUIDFile    = DefaultPodInfoDir + "/pod_uid"
	DefaultPodNSFile     = DefaultPodInfoDir + "/namespace"
	DefaultKubeTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	DefaultKubeCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	DefaultStagingRoot   = "/var/lib/kruise-agents-nfs-csi/staging"
	DefaultMountStateDir = "/var/lib/kruise-agents-nfs-csi/mounts"
	DefaultHostProcRoot  = "/proc"

	DefaultCreatedSubPathMode os.FileMode = 0o770
)

type WrapperConfig struct {
	DriverName            string
	SocketPath            string
	SocketMode            os.FileMode
	TokenAudience         string
	KubeTokenFile         string
	KubeCAFile            string
	StagingRoot           string
	MountStateDir         string
	NodeName              string
	HostProcRoot          string
	RequestTimeout        time.Duration
	EnableMount           bool
	UnstageAfterMount     bool
	CreateMissingSubPaths bool
	CreatedSubPathMode    os.FileMode
}

type MounterConfig struct {
	DriverName    string
	SocketPath    string
	TokenFile     string
	TokenAudience string
	NamespaceFile string
	PodNameFile   string
	PodUIDFile    string
	HTTPTimeout   time.Duration
}

func LoadWrapperConfig() (WrapperConfig, error) {
	createdSubPathMode, err := StrictFileModeEnv("WRAPPER_CREATED_SUBPATH_MODE", DefaultCreatedSubPathMode)
	if err != nil {
		return WrapperConfig{}, err
	}

	return WrapperConfig{
		DriverName:     Env("DRIVER_NAME", DefaultDriverName),
		SocketPath:     Env("WRAPPER_SOCKET_PATH", DefaultSocketPath),
		SocketMode:     FileModeEnv("WRAPPER_SOCKET_MODE", 0o660),
		TokenAudience:  Env("TOKEN_AUDIENCE", DefaultTokenAudience),
		KubeTokenFile:  Env("KUBE_TOKEN_FILE", DefaultKubeTokenFile),
		KubeCAFile:     Env("KUBE_CA_FILE", DefaultKubeCAFile),
		StagingRoot:    Env("WRAPPER_STAGING_ROOT", DefaultStagingRoot),
		MountStateDir:  Env("WRAPPER_MOUNT_STATE_DIR", DefaultMountStateDir),
		NodeName:       Env("WRAPPER_NODE_NAME", Env("NODE_ID", Env("KUBE_NODE_NAME", ""))),
		HostProcRoot:   Env("WRAPPER_HOST_PROC_ROOT", DefaultHostProcRoot),
		RequestTimeout: DurationEnv("WRAPPER_REQUEST_TIMEOUT", 30*time.Second),
		EnableMount:    BoolEnv("WRAPPER_ENABLE_MOUNT", true),
		UnstageAfterMount: BoolEnv(
			"WRAPPER_UNSTAGE_AFTER_MOUNT",
			true,
		),
		CreateMissingSubPaths: BoolEnv("WRAPPER_CREATE_MISSING_SUBPATHS", false),
		CreatedSubPathMode:    createdSubPathMode,
	}, nil
}

func LoadMounterConfig() MounterConfig {
	return MounterConfig{
		DriverName:    Env("DRIVER_NAME", DefaultDriverName),
		SocketPath:    Env("WRAPPER_SOCKET_PATH", DefaultSocketPath),
		TokenFile:     Env("PROJECTED_TOKEN_FILE", DefaultTokenFile),
		TokenAudience: Env("TOKEN_AUDIENCE", DefaultTokenAudience),
		NamespaceFile: Env("NAMESPACE_FILE", DefaultNamespaceFile),
		PodNameFile:   Env("POD_NAME_FILE", DefaultPodNameFile),
		PodUIDFile:    Env("POD_UID_FILE", DefaultPodUIDFile),
		HTTPTimeout:   DurationEnv("MOUNTER_HTTP_TIMEOUT", 15*time.Second),
	}
}

func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func BoolEnv(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func DurationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func FileModeEnv(key string, fallback os.FileMode) os.FileMode {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return fallback
	}
	return os.FileMode(parsed)
}

// StrictFileModeEnv parses a Unix file mode from an environment variable.
// Unlike FileModeEnv, an invalid non-empty value is returned as an error.
func StrictFileModeEnv(key string, fallback os.FileMode) (os.FileMode, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	mode, err := ParseFileMode(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return mode, nil
}

// ParseFileMode parses an octal Unix file mode from 0001 through 07777,
// including setuid, setgid, and sticky bits, into Go's os.FileMode representation.
func ParseFileMode(value string) (os.FileMode, error) {
	parsed, err := strconv.ParseUint(value, 8, 16)
	if err != nil {
		return 0, fmt.Errorf("parse %q as an octal file mode: %w", value, err)
	}
	if parsed > 0o7777 {
		return 0, fmt.Errorf("file mode %q exceeds 07777", value)
	}
	if parsed == 0 {
		return 0, fmt.Errorf("file mode %q must be at least 0001", value)
	}

	mode := os.FileMode(parsed & 0o777)
	if parsed&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if parsed&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if parsed&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode, nil
}

// FormatFileMode formats Go's file mode bits as a four-digit Unix octal mode.
func FormatFileMode(mode os.FileMode) string {
	unixMode := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		unixMode |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		unixMode |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		unixMode |= 0o1000
	}
	return fmt.Sprintf("%04o", unixMode)
}
