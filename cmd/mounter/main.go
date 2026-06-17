package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
)

func main() {
	cfg := config.LoadMounterConfig()

	cfg, request, err := parseRequest(os.Args[1:], cfg)
	if err != nil {
		exitError(err)
	}

	request = completePodIdentity(request, cfg)

	if err := validateCLIRequest(request); err != nil {
		exitError(err)
	}

	tokenBytes, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		exitError(fmt.Errorf("read projected token file: %w", err))
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		exitError(fmt.Errorf("projected token file is empty"))
	}

	result, err := callWrapper(context.Background(), cfg, token, request)
	if err != nil {
		exitError(err)
	}

	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(api.Response{Data: result}); err != nil {
		exitError(fmt.Errorf("write response: %w", err))
	}
}

func parseRequest(args []string, cfg config.MounterConfig) (config.MounterConfig, api.MountRequest, error) {
	if len(args) > 0 && args[0] == "mount" {
		return parseRuntimeMountRequest(args[1:], cfg)
	}
	return parseDirectRequest(args, cfg)
}

func parseDirectRequest(args []string, cfg config.MounterConfig) (config.MounterConfig, api.MountRequest, error) {
	request := defaultMountRequest(cfg)
	fs := flag.NewFlagSet("kruise-nfs-mounter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.SocketPath, "socket-path", cfg.SocketPath, "wrapper Unix socket path")
	fs.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "projected service account token file")
	fs.StringVar(&request.DriverName, "driver-name", request.DriverName, "CSI driver name")
	fs.StringVar(&request.Namespace, "namespace", request.Namespace, "requesting pod namespace")
	fs.StringVar(&request.PodName, "pod-name", request.PodName, "requesting pod name")
	fs.StringVar(&request.PodUID, "pod-uid", request.PodUID, "requesting pod UID")
	fs.StringVar(&request.PVName, "pv", "", "persistent volume name to mount")
	fs.StringVar(&request.TargetPath, "target", "", "target path inside the business container")
	fs.StringVar(&request.ContainerName, "container", request.ContainerName, "business container name")
	if err := fs.Parse(args); err != nil {
		return cfg, api.MountRequest{}, err
	}
	if fs.NArg() != 0 {
		return cfg, api.MountRequest{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return cfg, request, nil
}

func parseRuntimeMountRequest(args []string, cfg config.MounterConfig) (config.MounterConfig, api.MountRequest, error) {
	request := defaultMountRequest(cfg)
	var encodedConfig string

	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.SocketPath, "socket-path", cfg.SocketPath, "wrapper Unix socket path")
	fs.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "projected service account token file")
	fs.StringVar(&request.DriverName, "driver", request.DriverName, "CSI driver name")
	fs.StringVar(&encodedConfig, "config", "", "base64 encoded CSI NodePublishVolumeRequest protobuf")
	fs.StringVar(&request.Namespace, "namespace", request.Namespace, "requesting pod namespace")
	fs.StringVar(&request.PodName, "pod-name", request.PodName, "requesting pod name")
	fs.StringVar(&request.PodUID, "pod-uid", request.PodUID, "requesting pod UID")
	fs.StringVar(&request.ContainerName, "container", request.ContainerName, "business container name")
	if err := fs.Parse(args); err != nil {
		return cfg, api.MountRequest{}, err
	}
	if fs.NArg() != 0 {
		return cfg, api.MountRequest{}, fmt.Errorf("unexpected mount arguments: %s", strings.Join(fs.Args(), " "))
	}
	if encodedConfig == "" {
		return cfg, api.MountRequest{}, fmt.Errorf("config is required")
	}

	csiRequest, err := decodeNodePublishVolumeRequest(encodedConfig)
	if err != nil {
		return cfg, api.MountRequest{}, err
	}
	request.PVName, err = pvNameFromNodePublishRequest(csiRequest)
	if err != nil {
		return cfg, api.MountRequest{}, err
	}
	request.TargetPath = csiRequest.TargetPath
	return cfg, request, nil
}

func defaultMountRequest(cfg config.MounterConfig) api.MountRequest {
	return api.MountRequest{
		APIVersion:    api.Version,
		DriverName:    cfg.DriverName,
		Namespace:     config.Env("POD_NAMESPACE", ""),
		PodName:       config.Env("POD_NAME", ""),
		PodUID:        config.Env("POD_UID", ""),
		ContainerName: config.Env("CONTAINER_NAME", ""),
	}
}

func completePodIdentity(request api.MountRequest, cfg config.MounterConfig) api.MountRequest {
	if request.Namespace == "" {
		request.Namespace = readFirstExistingFile(cfg.NamespaceFile, config.DefaultPodNSFile)
	}
	if request.PodName == "" {
		request.PodName = readFirstExistingFile(cfg.PodNameFile, "/etc/hostname")
	}
	if request.PodUID == "" {
		request.PodUID = readFirstExistingFile(cfg.PodUIDFile)
	}
	return request
}

func decodeNodePublishVolumeRequest(encoded string) (*csi.NodePublishVolumeRequest, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode csi node publish config: %w", err)
	}
	var request csi.NodePublishVolumeRequest
	if err := proto.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("unmarshal csi node publish config: %w", err)
	}
	return &request, nil
}

func pvNameFromNodePublishRequest(request *csi.NodePublishVolumeRequest) (string, error) {
	if request == nil {
		return "", fmt.Errorf("csi node publish config is empty")
	}
	if name := firstNonEmpty(
		request.VolumeContext["csi.storage.k8s.io/pv/name"],
		request.VolumeContext["pvName"],
		request.VolumeContext["pv_name"],
		request.VolumeContext["persistentVolumeName"],
		request.PublishContext["csi.storage.k8s.io/pv/name"],
		request.PublishContext["pvName"],
	); name != "" {
		return name, nil
	}
	return derivePVNameFromVolumeID(request.VolumeId)
}

func derivePVNameFromVolumeID(volumeID string) (string, error) {
	if volumeID == "" {
		return "", fmt.Errorf("volume_id is required")
	}
	idx := strings.LastIndex(volumeID, "-")
	if idx > 0 && len(volumeID)-idx-1 == 6 && isLowerAlphaNumeric(volumeID[idx+1:]) {
		return volumeID[:idx], nil
	}
	return volumeID, nil
}

func isLowerAlphaNumeric(value string) bool {
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return value != ""
}

func validateCLIRequest(request api.MountRequest) error {
	if request.DriverName == "" {
		return fmt.Errorf("driver name is required")
	}
	if request.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if request.PodName == "" {
		return fmt.Errorf("pod name is required")
	}
	if request.PodUID == "" {
		return fmt.Errorf("pod uid is required")
	}
	if request.PVName == "" {
		return fmt.Errorf("pv is required")
	}
	if request.TargetPath == "" {
		return fmt.Errorf("target is required")
	}
	return nil
}

func callWrapper(ctx context.Context, cfg config.MounterConfig, token string, request api.MountRequest) (*api.MountResult, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal mount request: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{}
		return dialer.DialContext(ctx, "unix", cfg.SocketPath)
	}
	client := http.Client{Transport: transport, Timeout: cfg.HTTPTimeout}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/mount", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create wrapper request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+token)

	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("call wrapper socket %s: %w", cfg.SocketPath, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read wrapper response: %w", err)
	}

	var apiResponse struct {
		Data  *api.MountResult `json:"data,omitempty"`
		Error string           `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, fmt.Errorf("decode wrapper response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if apiResponse.Error != "" {
			return nil, fmt.Errorf("wrapper rejected mount request: %s", apiResponse.Error)
		}
		return nil, fmt.Errorf("wrapper rejected mount request with status %s", response.Status)
	}
	if apiResponse.Data == nil {
		return nil, fmt.Errorf("wrapper response did not include data")
	}
	return apiResponse.Data, nil
}

func readFirstExistingFile(paths ...string) string {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}

		data, err := os.ReadFile(path)
		if err == nil {
			if value := strings.TrimSpace(string(data)); value != "" {
				return value
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func exitError(err error) {
	_ = json.NewEncoder(os.Stderr).Encode(api.Response{Error: err.Error()})
	os.Exit(1)
}
