package mounter

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
)

func TestNewClientConfigValidation(t *testing.T) {
	valid := Config{
		DriverName:  "csi.test.example",
		SocketPath:  "/tmp/wrapper.sock",
		TokenFile:   "/tmp/token",
		HTTPTimeout: time.Second,
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "driver name",
			mutate:  func(cfg *Config) { cfg.DriverName = "" },
			wantErr: "driver name is required",
		},
		{
			name:    "socket path",
			mutate:  func(cfg *Config) { cfg.SocketPath = "" },
			wantErr: "socket path is required",
		},
		{
			name:    "token file",
			mutate:  func(cfg *Config) { cfg.TokenFile = "" },
			wantErr: "token file is required",
		},
		{
			name:    "negative timeout",
			mutate:  func(cfg *Config) { cfg.HTTPTimeout = -time.Second },
			wantErr: "http timeout must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			_, err := NewClient(cfg)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("NewClient error = %v, want %q", err, test.wantErr)
			}
		})
	}

	valid.HTTPTimeout = 0
	client, err := NewClient(valid)
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	if client.httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("HTTP timeout = %s, want %s", client.httpClient.Timeout, defaultHTTPTimeout)
	}

	disabled := valid
	disabled.HTTPTimeout = 0
	disabled.DisableHTTPTimeout = true
	client, err = NewClient(disabled)
	if err != nil {
		t.Fatalf("NewClient with disabled timeout returned error: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	if client.httpClient.Timeout != 0 {
		t.Fatalf("disabled HTTP timeout = %s, want 0", client.httpClient.Timeout)
	}
}

func TestClientDoesNotUseDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = failingRoundTripper{}
	t.Cleanup(func() { http.DefaultTransport = original })

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusOK, api.Response{Data: HealthResult{
			Status:     "ok",
			DriverName: "csi.test.example",
		}})
	}), time.Second)

	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health used http.DefaultTransport: %v", err)
	}
}

func TestClientMountUnmountHealthAndTokenRotation(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/mount", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("mount method = %s, want POST", request.Method)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("mount Accept = %q, want application/json", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("mount Content-Type = %q, want application/json", got)
		}
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		mu.Unlock()

		var got api.MountRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Errorf("decode mount request: %v", err)
		}
		want := api.MountRequest{
			APIVersion:    api.Version,
			DriverName:    "csi.test.example",
			Namespace:     "sandbox-ns",
			PodName:       "sandbox-pod",
			PodUID:        "pod-uid",
			PVName:        "pv-a",
			SourceSubPath: "users/alice",
			TargetPath:    "/workspace/data",
			ContainerName: "main",
		}
		if got != want {
			t.Errorf("mount request = %#v, want %#v", got, want)
		}
		writeResponse(t, w, http.StatusOK, api.Response{Data: api.MountResult{
			Mounted:       true,
			DriverName:    got.DriverName,
			PVName:        got.PVName,
			SourceSubPath: got.SourceSubPath,
			TargetPath:    got.TargetPath,
			ContainerName: got.ContainerName,
		}})
	})
	mux.HandleFunc("/v1/unmount", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("unmount method = %s, want POST", request.Method)
		}
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		mu.Unlock()

		var got api.UnmountRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Errorf("decode unmount request: %v", err)
		}
		want := api.UnmountRequest{
			APIVersion:    api.Version,
			DriverName:    "csi.test.example",
			Namespace:     "sandbox-ns",
			PodName:       "sandbox-pod",
			PodUID:        "pod-uid",
			PVName:        "pv-a",
			TargetPath:    "/workspace/data",
			ContainerName: "main",
		}
		if got != want {
			t.Errorf("unmount request = %#v, want %#v", got, want)
		}
		writeResponse(t, w, http.StatusOK, api.Response{Data: api.UnmountResult{
			Unmounted:     true,
			DriverName:    got.DriverName,
			PVName:        got.PVName,
			TargetPath:    got.TargetPath,
			ContainerName: got.ContainerName,
		}})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("health method = %s, want GET", request.Method)
		}
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("health Authorization = %q, want empty", got)
		}
		writeResponse(t, w, http.StatusOK, api.Response{Data: HealthResult{
			Status:     "ok",
			DriverName: "csi.test.example",
		}})
	})

	client, tokenFile := newTestClient(t, mux, time.Second)
	mountResult, err := client.Mount(context.Background(), MountRequest{
		Namespace:     "sandbox-ns",
		PodName:       "sandbox-pod",
		PodUID:        "pod-uid",
		PVName:        "pv-a",
		SourceSubPath: "users/alice",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	})
	if err != nil {
		t.Fatalf("Mount returned error: %v", err)
	}
	wantMountResult := &MountResult{
		Mounted:       true,
		DriverName:    "csi.test.example",
		PVName:        "pv-a",
		SourceSubPath: "users/alice",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	}
	if !reflect.DeepEqual(mountResult, wantMountResult) {
		t.Fatalf("Mount result = %#v, want %#v", mountResult, wantMountResult)
	}

	if err := os.WriteFile(tokenFile, []byte("token-two\n"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	unmountResult, err := client.Unmount(context.Background(), UnmountRequest{
		Namespace:     "sandbox-ns",
		PodName:       "sandbox-pod",
		PodUID:        "pod-uid",
		PVName:        "pv-a",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	})
	if err != nil {
		t.Fatalf("Unmount returned error: %v", err)
	}
	wantUnmountResult := &UnmountResult{
		Unmounted:     true,
		DriverName:    "csi.test.example",
		PVName:        "pv-a",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	}
	if !reflect.DeepEqual(unmountResult, wantUnmountResult) {
		t.Fatalf("Unmount result = %#v, want %#v", unmountResult, wantUnmountResult)
	}

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if want := (&HealthResult{Status: "ok", DriverName: "csi.test.example"}); !reflect.DeepEqual(health, want) {
		t.Fatalf("Health result = %#v, want %#v", health, want)
	}

	mu.Lock()
	gotAuthorizations := append([]string(nil), authorizations...)
	mu.Unlock()
	wantAuthorizations := []string{"Bearer token-one", "Bearer token-two"}
	if !reflect.DeepEqual(gotAuthorizations, wantAuthorizations) {
		t.Fatalf("Authorization headers = %#v, want %#v", gotAuthorizations, wantAuthorizations)
	}
}

func TestClientResponseError(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusForbidden, api.Response{Error: "mount denied"})
	}), time.Second)

	_, err := client.Mount(context.Background(), validMountRequest())
	if err == nil {
		t.Fatal("Mount returned nil error")
	}
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Mount error type = %T, want *ResponseError", err)
	}
	if responseErr.Operation != "mount" || responseErr.StatusCode != http.StatusForbidden || responseErr.Message != "mount denied" {
		t.Fatalf("ResponseError = %#v", responseErr)
	}
	if got, want := err.Error(), "wrapper rejected mount request: mount denied"; got != want {
		t.Fatalf("Mount error = %q, want %q", got, want)
	}
}

func TestClientResponseErrorWithoutMessage(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusServiceUnavailable, api.Response{})
	}), time.Second)

	_, err := client.Health(context.Background())
	if err == nil {
		t.Fatal("Health returned nil error")
	}
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("Health error type = %T, want *ResponseError", err)
	}
	if got, want := err.Error(), "wrapper rejected health request with status 503 Service Unavailable"; got != want {
		t.Fatalf("Health error = %q, want %q", got, want)
	}
}

func TestClientResponseErrorWithNonJSONBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty"},
		{name: "plain text", body: "service unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(test.body))
			}), time.Second)

			_, err := client.Health(context.Background())
			if err == nil {
				t.Fatal("Health returned nil error")
			}
			var responseErr *ResponseError
			if !errors.As(err, &responseErr) {
				t.Fatalf("Health error type = %T, want *ResponseError", err)
			}
			if responseErr.StatusCode != http.StatusBadGateway || responseErr.Message != "" {
				t.Fatalf("ResponseError = %#v", responseErr)
			}
		})
	}
}

func TestClientInvalidResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{name: "invalid envelope", response: "not-json", wantErr: "decode wrapper response:"},
		{name: "missing data", response: `{}`, wantErr: "wrapper response did not include data"},
		{name: "invalid result", response: `{"data":"wrong"}`, wantErr: "decode wrapper health result:"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}), time.Second)

			_, err := client.Health(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Health error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestClientContextCancellationAndTimeout(t *testing.T) {
	handler := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	})

	t.Run("caller cancellation", func(t *testing.T) {
		client, _ := newTestClient(t, handler, time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Health(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Health error = %v, want context.Canceled", err)
		}
	})

	t.Run("client timeout", func(t *testing.T) {
		client, _ := newTestClient(t, handler, 20*time.Millisecond)
		_, err := client.Health(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Health error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestClientReadsTokenForEveryAuthenticatedRequest(t *testing.T) {
	client, tokenFile := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		writeResponse(t, w, http.StatusOK, api.Response{Data: MountResult{Mounted: true}})
	}), time.Second)

	if err := os.WriteFile(tokenFile, nil, 0o600); err != nil {
		t.Fatalf("empty token file: %v", err)
	}
	_, err := client.Mount(context.Background(), validMountRequest())
	if err == nil || err.Error() != "projected token file is empty" {
		t.Fatalf("Mount error = %v, want empty token error", err)
	}

	if err := os.Remove(tokenFile); err != nil {
		t.Fatalf("remove token file: %v", err)
	}
	_, err = client.Unmount(context.Background(), validUnmountRequest())
	if err == nil || !strings.Contains(err.Error(), "read projected token file:") {
		t.Fatalf("Unmount error = %v, want token read error", err)
	}
}

func TestClientReadsExportRootKeyForEveryMountOnly(t *testing.T) {
	var mu sync.Mutex
	var mountKeys []string
	var unmountKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/mount", func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		mountKeys = append(mountKeys, request.Header.Get(api.ExportRootKeyHeader))
		mu.Unlock()
		writeResponse(t, w, http.StatusOK, api.Response{Data: MountResult{Mounted: true}})
	})
	mux.HandleFunc("/v1/unmount", func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		unmountKey = request.Header.Get(api.ExportRootKeyHeader)
		mu.Unlock()
		writeResponse(t, w, http.StatusOK, api.Response{Data: UnmountResult{Unmounted: true}})
	})

	client, _ := newTestClient(t, mux, time.Second)
	keyFile := filepath.Join(t.TempDir(), "export-root-key")
	if err := os.WriteFile(keyFile, []byte("root-key-one\n"), 0o600); err != nil {
		t.Fatalf("write export root key: %v", err)
	}
	client.exportRootKeyFile = keyFile

	if _, err := client.Mount(context.Background(), validMountRequest()); err != nil {
		t.Fatalf("first Mount returned error: %v", err)
	}
	if err := os.WriteFile(keyFile, []byte("root-key-two\n"), 0o600); err != nil {
		t.Fatalf("rotate export root key: %v", err)
	}
	if _, err := client.Mount(context.Background(), validMountRequest()); err != nil {
		t.Fatalf("second Mount returned error: %v", err)
	}
	if _, err := client.Unmount(context.Background(), validUnmountRequest()); err != nil {
		t.Fatalf("Unmount returned error: %v", err)
	}

	mu.Lock()
	gotMountKeys := append([]string(nil), mountKeys...)
	mu.Unlock()
	wantMountKeys := []string{"root-key-one", "root-key-two"}
	if !reflect.DeepEqual(gotMountKeys, wantMountKeys) {
		t.Fatalf("mount export root keys = %#v, want %#v", gotMountKeys, wantMountKeys)
	}
	mu.Lock()
	gotUnmountKey := unmountKey
	mu.Unlock()
	if gotUnmountKey != "" {
		t.Fatalf("unmount export root key = %q, want empty", gotUnmountKey)
	}
}

func TestClientSupportsConcurrentCalls(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusOK, api.Response{Data: HealthResult{
			Status:     "ok",
			DriverName: "csi.test.example",
		}})
	}), time.Second)

	const callCount = 16
	var wait sync.WaitGroup
	errorsCh := make(chan error, callCount)
	for range callCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := client.Health(context.Background())
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent Health returned error: %v", err)
		}
	}
}

func TestClientRequestValidation(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, http.StatusOK, api.Response{Data: MountResult{Mounted: true}})
	}), time.Second)

	tests := []struct {
		name    string
		mutate  func(*MountRequest)
		wantErr string
	}{
		{name: "namespace", mutate: func(request *MountRequest) { request.Namespace = "" }, wantErr: "namespace is required"},
		{name: "pod name", mutate: func(request *MountRequest) { request.PodName = "" }, wantErr: "pod name is required"},
		{name: "pod uid", mutate: func(request *MountRequest) { request.PodUID = "" }, wantErr: "pod uid is required"},
		{name: "pv", mutate: func(request *MountRequest) { request.PVName = "" }, wantErr: "pv is required"},
		{name: "target", mutate: func(request *MountRequest) { request.TargetPath = "" }, wantErr: "target is required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validMountRequest()
			test.mutate(&request)
			_, err := client.Mount(context.Background(), request)
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("Mount error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestClientConcurrentUse(t *testing.T) {
	const calls = 32
	var mu sync.Mutex
	completed := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got api.MountRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Errorf("decode mount request: %v", err)
			return
		}
		mu.Lock()
		completed++
		mu.Unlock()
		writeResponse(t, w, http.StatusOK, api.Response{Data: MountResult{
			Mounted:    true,
			DriverName: got.DriverName,
			PVName:     got.PVName,
			TargetPath: got.TargetPath,
		}})
	})
	client, _ := newTestClient(t, handler, time.Second)

	errs := make(chan error, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := client.Mount(context.Background(), validMountRequest())
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Mount returned error: %v", err)
		}
	}

	mu.Lock()
	gotCompleted := completed
	mu.Unlock()
	if gotCompleted != calls {
		t.Fatalf("completed calls = %d, want %d", gotCompleted, calls)
	}
}

func validMountRequest() MountRequest {
	return MountRequest{
		Namespace:     "sandbox-ns",
		PodName:       "sandbox-pod",
		PodUID:        "pod-uid",
		PVName:        "pv-a",
		SourceSubPath: "users/alice",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected use of http.DefaultTransport")
}

func validUnmountRequest() UnmountRequest {
	request := validMountRequest()
	return UnmountRequest{
		Namespace:     request.Namespace,
		PodName:       request.PodName,
		PodUID:        request.PodUID,
		PVName:        request.PVName,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	}
}

func newTestClient(t *testing.T, handler http.Handler, timeout time.Duration) (*Client, string) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token-one\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	socketPath := startUnixHTTPServer(t, handler)
	client, err := NewClient(Config{
		DriverName:  "csi.test.example",
		SocketPath:  socketPath,
		TokenFile:   tokenFile,
		HTTPTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, tokenFile
}

func startUnixHTTPServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mounter-client-test-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "wrapper.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on Unix socket: %v", err)
	}
	server := &http.Server{Handler: handler}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close test server: %v", err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve test server: %v", err)
		}
	})
	return socketPath
}

func writeResponse(t *testing.T, w http.ResponseWriter, status int, response api.Response) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
