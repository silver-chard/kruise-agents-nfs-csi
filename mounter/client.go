// Package mounter provides a low-privilege Go client for the node wrapper's
// HTTP-over-Unix-socket API.
package mounter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
)

const (
	defaultHTTPTimeout = 15 * time.Second
	maxResponseSize    = 1 << 20
)

// Config configures a Client.
type Config struct {
	// DriverName is the CSI driver name expected by the node wrapper.
	DriverName string
	// SocketPath is the filesystem path of the node wrapper Unix socket.
	SocketPath string
	// TokenFile is the projected service account token file. Mount and Unmount
	// read it for every request so projected token rotation is observed.
	TokenFile string
	// HTTPTimeout limits each wrapper request. Zero uses a 15-second default.
	HTTPTimeout time.Duration
	// DisableHTTPTimeout disables the client-wide request timeout. Callers
	// should normally keep the timeout enabled and also pass a bounded context.
	DisableHTTPTimeout bool
}

// MountRequest identifies a volume and target container for a mount operation.
// The client supplies the wire API version and configured driver name.
type MountRequest struct {
	// Namespace is the requesting Pod namespace.
	Namespace string `json:"namespace"`
	// PodName is the requesting Pod name.
	PodName string `json:"pod_name"`
	// PodUID is the requesting Pod UID.
	PodUID string `json:"pod_uid"`
	// PVName is the PersistentVolume to mount.
	PVName string `json:"pv_name"`
	// SourceSubPath is an optional directory below the PV root.
	SourceSubPath string `json:"source_sub_path,omitempty"`
	// TargetPath is the absolute path inside the target container.
	TargetPath string `json:"target_path"`
	// ContainerName is the optional target container name.
	ContainerName string `json:"container_name,omitempty"`
}

// MountResult reports the effective mount accepted by the node wrapper.
type MountResult struct {
	// Mounted reports whether the wrapper completed the mount.
	Mounted bool `json:"mounted"`
	// DriverName is the driver accepted by the wrapper.
	DriverName string `json:"driver_name"`
	// PVName is the mounted PersistentVolume.
	PVName string `json:"pv_name"`
	// SourceSubPath is the mounted directory below the PV root, if any.
	SourceSubPath string `json:"source_sub_path,omitempty"`
	// TargetPath is the path inside the target container.
	TargetPath string `json:"target_path"`
	// ContainerName is the effective target container name.
	ContainerName string `json:"container_name,omitempty"`
}

// UnmountRequest identifies a mounted target to remove.
type UnmountRequest struct {
	// Namespace is the requesting Pod namespace.
	Namespace string `json:"namespace"`
	// PodName is the requesting Pod name.
	PodName string `json:"pod_name"`
	// PodUID is the requesting Pod UID.
	PodUID string `json:"pod_uid"`
	// PVName is the PersistentVolume associated with the target.
	PVName string `json:"pv_name"`
	// TargetPath is the absolute path inside the target container.
	TargetPath string `json:"target_path"`
	// ContainerName is the optional target container name.
	ContainerName string `json:"container_name,omitempty"`
}

// UnmountResult reports the effective unmount accepted by the node wrapper.
type UnmountResult struct {
	// Unmounted reports whether the wrapper completed the unmount.
	Unmounted bool `json:"unmounted"`
	// DriverName is the driver accepted by the wrapper.
	DriverName string `json:"driver_name"`
	// PVName is the PersistentVolume associated with the target.
	PVName string `json:"pv_name"`
	// TargetPath is the path inside the target container.
	TargetPath string `json:"target_path"`
	// ContainerName is the effective target container name.
	ContainerName string `json:"container_name,omitempty"`
}

// HealthResult reports the node wrapper's health and configured driver name.
type HealthResult struct {
	// Status is the health status reported by the wrapper.
	Status string `json:"status"`
	// DriverName is the wrapper's configured CSI driver name.
	DriverName string `json:"driver_name"`
}

// ResponseError describes a non-successful response returned by the node
// wrapper. Callers can inspect it with errors.As.
type ResponseError struct {
	// Operation is mount, unmount, or health.
	Operation string
	// StatusCode is the HTTP response status code.
	StatusCode int
	// Message is the wrapper-provided error message, when present.
	Message string

	status string
}

// Error implements error.
func (e *ResponseError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("wrapper rejected %s request: %s", e.Operation, e.Message)
	}
	status := e.status
	if status == "" {
		status = fmt.Sprintf("%d", e.StatusCode)
	}
	return fmt.Sprintf("wrapper rejected %s request with status %s", e.Operation, status)
}

// Client calls the node wrapper over a reusable HTTP-over-Unix-socket
// transport. A Client is safe for concurrent use.
type Client struct {
	driverName string
	socketPath string
	tokenFile  string
	httpClient *http.Client
	transport  *http.Transport
}

// NewClient validates config and creates a reusable node wrapper client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.DriverName == "" {
		return nil, fmt.Errorf("driver name is required")
	}
	if cfg.SocketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("token file is required")
	}
	if cfg.HTTPTimeout < 0 {
		return nil, fmt.Errorf("http timeout must not be negative")
	}
	if cfg.DisableHTTPTimeout {
		cfg.HTTPTimeout = 0
	} else if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = defaultHTTPTimeout
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", cfg.SocketPath)
		},
	}

	return &Client{
		driverName: cfg.DriverName,
		socketPath: cfg.SocketPath,
		tokenFile:  cfg.TokenFile,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   cfg.HTTPTimeout,
		},
		transport: transport,
	}, nil
}

// Mount requests that the node wrapper mount a PV into the target container.
func (c *Client) Mount(ctx context.Context, request MountRequest) (*MountResult, error) {
	if err := validateRequest(request.Namespace, request.PodName, request.PodUID, request.PVName, request.TargetPath); err != nil {
		return nil, err
	}
	token, err := c.readToken()
	if err != nil {
		return nil, err
	}

	payload := api.MountRequest{
		APIVersion:    api.Version,
		DriverName:    c.driverName,
		Namespace:     request.Namespace,
		PodName:       request.PodName,
		PodUID:        request.PodUID,
		PVName:        request.PVName,
		SourceSubPath: request.SourceSubPath,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	}
	var result MountResult
	if err := c.call(ctx, "mount", http.MethodPost, "/v1/mount", token, payload, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Unmount requests that the node wrapper remove a mounted target and its
// persisted desired-mount state.
func (c *Client) Unmount(ctx context.Context, request UnmountRequest) (*UnmountResult, error) {
	if err := validateRequest(request.Namespace, request.PodName, request.PodUID, request.PVName, request.TargetPath); err != nil {
		return nil, err
	}
	token, err := c.readToken()
	if err != nil {
		return nil, err
	}

	payload := api.UnmountRequest{
		APIVersion:    api.Version,
		DriverName:    c.driverName,
		Namespace:     request.Namespace,
		PodName:       request.PodName,
		PodUID:        request.PodUID,
		PVName:        request.PVName,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	}
	var result UnmountResult
	if err := c.call(ctx, "unmount", http.MethodPost, "/v1/unmount", token, payload, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Health retrieves the node wrapper's unauthenticated health response.
func (c *Client) Health(ctx context.Context) (*HealthResult, error) {
	var result HealthResult
	if err := c.call(ctx, "health", http.MethodGet, "/healthz", "", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CloseIdleConnections closes idle connections held by the client's HTTP
// transport. It does not unmount volumes and does not interrupt active calls.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.transport == nil {
		return
	}
	c.transport.CloseIdleConnections()
}

func (c *Client) readToken() (string, error) {
	tokenBytes, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read projected token file: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return "", fmt.Errorf("projected token file is empty")
	}
	return token, nil
}

func validateRequest(namespace, podName, podUID, pvName, targetPath string) error {
	switch {
	case namespace == "":
		return fmt.Errorf("namespace is required")
	case podName == "":
		return fmt.Errorf("pod name is required")
	case podUID == "":
		return fmt.Errorf("pod uid is required")
	case pvName == "":
		return fmt.Errorf("pv is required")
	case targetPath == "":
		return fmt.Errorf("target is required")
	default:
		return nil
	}
}

func (c *Client) call(ctx context.Context, operation, method, endpoint, token string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal %s request: %w", operation, err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+endpoint, body)
	if err != nil {
		return fmt.Errorf("create wrapper request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call wrapper socket %s: %w", c.socketPath, err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read wrapper response: %w", err)
	}

	var envelope struct {
		Data  json.RawMessage `json:"data,omitempty"`
		Error string          `json:"error,omitempty"`
	}
	decodeErr := json.Unmarshal(responseBody, &envelope)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &ResponseError{
			Operation:  operation,
			StatusCode: response.StatusCode,
			Message:    envelope.Error,
			status:     response.Status,
		}
	}
	if decodeErr != nil {
		return fmt.Errorf("decode wrapper response: %w", decodeErr)
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("wrapper response did not include data")
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return fmt.Errorf("decode wrapper %s result: %w", operation, err)
	}
	return nil
}
