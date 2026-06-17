package config

import (
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
	DefaultHostProcRoot  = "/proc"
)

type WrapperConfig struct {
	DriverName     string
	SocketPath     string
	SocketMode     os.FileMode
	TokenAudience  string
	KubeTokenFile  string
	KubeCAFile     string
	StagingRoot    string
	HostProcRoot   string
	RequestTimeout time.Duration
	EnableMount    bool
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

func LoadWrapperConfig() WrapperConfig {
	return WrapperConfig{
		DriverName:     Env("DRIVER_NAME", DefaultDriverName),
		SocketPath:     Env("WRAPPER_SOCKET_PATH", DefaultSocketPath),
		SocketMode:     FileModeEnv("WRAPPER_SOCKET_MODE", 0o660),
		TokenAudience:  Env("TOKEN_AUDIENCE", DefaultTokenAudience),
		KubeTokenFile:  Env("KUBE_TOKEN_FILE", DefaultKubeTokenFile),
		KubeCAFile:     Env("KUBE_CA_FILE", DefaultKubeCAFile),
		StagingRoot:    Env("WRAPPER_STAGING_ROOT", DefaultStagingRoot),
		HostProcRoot:   Env("WRAPPER_HOST_PROC_ROOT", DefaultHostProcRoot),
		RequestTimeout: DurationEnv("WRAPPER_REQUEST_TIMEOUT", 30*time.Second),
		EnableMount:    BoolEnv("WRAPPER_ENABLE_MOUNT", true),
	}
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
