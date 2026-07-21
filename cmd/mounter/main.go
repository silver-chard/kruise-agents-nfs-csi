package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

func main() {
	cfg := config.LoadMounterConfig()

	cfg, request, err := parseRequest(os.Args[1:], cfg)
	if err != nil {
		exitError(err)
	}

	request.Mount = completePodIdentity(request.Mount, cfg)

	if err := validateCLIRequest(request.Mount); err != nil {
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

type operation string

const (
	operationMount   operation = "mount"
	operationUnmount operation = "unmount"
)

type cliRequest struct {
	Operation operation
	Mount     api.MountRequest
}

type commandState struct {
	cfg           config.MounterConfig
	request       api.MountRequest
	operation     operation
	encodedConfig string
}

func parseRequest(args []string, cfg config.MounterConfig) (config.MounterConfig, cliRequest, error) {
	state := commandState{
		cfg:       cfg,
		request:   defaultMountRequest(cfg),
		operation: operationMount,
	}
	root := newRootCommand(&state)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		return state.cfg, cliRequest{}, err
	}
	return state.cfg, cliRequest{Operation: state.operation, Mount: state.request}, nil
}

func newRootCommand(state *commandState) *cobra.Command {
	root := &cobra.Command{
		Use:           "kruise-nfs-mounter",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
			}
			state.operation = operationMount
			return nil
		},
	}

	bindCommonFlags(root, state)
	root.Flags().StringVar(&state.request.PVName, "pv", "", "persistent volume name to mount")
	root.Flags().StringVar(&state.request.SourceSubPath, "sub-path", "", "directory subPath inside the persistent volume")
	root.Flags().StringVar(&state.request.TargetPath, "target", "", "target path inside the business container")
	root.Flags().StringVar(&state.request.ContainerName, "container", state.request.ContainerName, "business container name")

	mountCommand := &cobra.Command{
		Use:           "mount",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected mount arguments: %s", strings.Join(args, " "))
			}
			state.operation = operationMount
			if state.encodedConfig == "" {
				return nil
			}
			return applyNodePublishConfig(&state.request, state.encodedConfig, true)
		},
	}
	bindCommonFlags(mountCommand, state)
	mountCommand.Flags().StringVar(&state.encodedConfig, "config", "", "base64 encoded CSI NodePublishVolumeRequest protobuf")
	mountCommand.Flags().StringVar(&state.request.PVName, "pv", "", "persistent volume name to mount")
	mountCommand.Flags().StringVar(&state.request.SourceSubPath, "sub-path", "", "directory subPath inside the persistent volume")
	mountCommand.Flags().StringVar(&state.request.TargetPath, "target", "", "target path inside the business container")
	mountCommand.Flags().StringVar(&state.request.ContainerName, "container", state.request.ContainerName, "business container name")

	unmountCommand := &cobra.Command{
		Use:           "unmount",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("unexpected unmount arguments: %s", strings.Join(args, " "))
			}
			state.operation = operationUnmount
			if state.encodedConfig != "" {
				if err := applyNodePublishConfig(&state.request, state.encodedConfig, false); err != nil {
					return err
				}
			}
			state.request.SourceSubPath = ""
			return nil
		},
	}
	bindCommonFlags(unmountCommand, state)
	unmountCommand.Flags().StringVar(&state.encodedConfig, "config", "", "base64 encoded CSI NodePublishVolumeRequest protobuf")
	unmountCommand.Flags().StringVar(&state.request.PVName, "pv", "", "persistent volume name to unmount")
	unmountCommand.Flags().StringVar(&state.request.TargetPath, "target", "", "target path inside the business container")
	unmountCommand.Flags().StringVar(&state.request.ContainerName, "container", state.request.ContainerName, "business container name")

	root.AddCommand(mountCommand, unmountCommand)
	return root
}

func bindCommonFlags(command *cobra.Command, state *commandState) {
	command.Flags().StringVar(&state.cfg.SocketPath, "socket-path", state.cfg.SocketPath, "wrapper Unix socket path")
	command.Flags().StringVar(&state.cfg.TokenFile, "token-file", state.cfg.TokenFile, "projected service account token file")
	command.Flags().StringVar(&state.request.DriverName, "driver", state.request.DriverName, "CSI driver name")
	command.Flags().StringVar(&state.request.DriverName, "driver-name", state.request.DriverName, "CSI driver name")
	command.Flags().StringVar(&state.request.Namespace, "namespace", state.request.Namespace, "requesting pod namespace")
	command.Flags().StringVar(&state.request.PodName, "pod-name", state.request.PodName, "requesting pod name")
	command.Flags().StringVar(&state.request.PodUID, "pod-uid", state.request.PodUID, "requesting pod UID")
}

func applyNodePublishConfig(request *api.MountRequest, encodedConfig string, includeSubPath bool) error {
	csiRequest, err := decodeNodePublishVolumeRequest(encodedConfig)
	if err != nil {
		return err
	}
	request.PVName, err = pvNameFromNodePublishRequest(csiRequest)
	if err != nil {
		return err
	}
	if includeSubPath && request.SourceSubPath == "" {
		request.SourceSubPath = sourceSubPathFromNodePublishRequest(csiRequest)
	}
	request.TargetPath = csiRequest.TargetPath
	return nil
}

func sourceSubPathFromNodePublishRequest(request *csi.NodePublishVolumeRequest) string {
	if request == nil {
		return ""
	}
	return firstNonEmpty(
		request.VolumeContext["sourceSubPath"],
		request.VolumeContext["source_sub_path"],
		request.VolumeContext["subPath"],
		request.VolumeContext["sub_path"],
		request.PublishContext["sourceSubPath"],
		request.PublishContext["source_sub_path"],
		request.PublishContext["subPath"],
		request.PublishContext["sub_path"],
	)
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

func callWrapper(ctx context.Context, cfg config.MounterConfig, token string, request cliRequest) (any, error) {
	var endpoint string
	var payloadRequest any
	switch request.Operation {
	case operationMount:
		endpoint = "/v1/mount"
		payloadRequest = request.Mount
	case operationUnmount:
		endpoint = "/v1/unmount"
		payloadRequest = api.UnmountRequest{
			APIVersion:    request.Mount.APIVersion,
			DriverName:    request.Mount.DriverName,
			Namespace:     request.Mount.Namespace,
			PodName:       request.Mount.PodName,
			PodUID:        request.Mount.PodUID,
			PVName:        request.Mount.PVName,
			TargetPath:    request.Mount.TargetPath,
			ContainerName: request.Mount.ContainerName,
		}
	default:
		return nil, fmt.Errorf("unsupported operation %q", request.Operation)
	}

	payload, err := json.Marshal(payloadRequest)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", request.Operation, err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{}
		return dialer.DialContext(ctx, "unix", cfg.SocketPath)
	}
	client := http.Client{Transport: transport, Timeout: cfg.HTTPTimeout}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+endpoint, bytes.NewReader(payload))
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
		Data  json.RawMessage `json:"data,omitempty"`
		Error string          `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, fmt.Errorf("decode wrapper response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if apiResponse.Error != "" {
			return nil, fmt.Errorf("wrapper rejected %s request: %s", request.Operation, apiResponse.Error)
		}
		return nil, fmt.Errorf("wrapper rejected %s request with status %s", request.Operation, response.Status)
	}
	if len(apiResponse.Data) == 0 {
		return nil, fmt.Errorf("wrapper response did not include data")
	}
	if request.Operation == operationUnmount {
		var result api.UnmountResult
		if err := json.Unmarshal(apiResponse.Data, &result); err != nil {
			return nil, fmt.Errorf("decode wrapper unmount result: %w", err)
		}
		return &result, nil
	}
	var result api.MountResult
	if err := json.Unmarshal(apiResponse.Data, &result); err != nil {
		return nil, fmt.Errorf("decode wrapper mount result: %w", err)
	}
	return &result, nil
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
