package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	podREST rest.Interface
}

type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string
	Detail     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes api %s %s returned %s: %s", e.Method, e.Path, e.Status, e.Detail)
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func NewInClusterClient(tokenFile, caFile string, timeout time.Duration) (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT must be set")
	}

	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read kubernetes token file: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		caBytes, err := os.ReadFile(caFile)
		if err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caBytes) {
				transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
		}
	}

	restConfig := &rest.Config{
		Host:            "https://" + netJoinHostPort(host, port),
		BearerTokenFile: tokenFile,
		TLSClientConfig: rest.TLSClientConfig{CAFile: caFile},
		UserAgent:       "kruise-agents-nfs-csi-wrapper",
	}
	coreClient, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes core client: %w", err)
	}

	return &Client{
		baseURL: "https://" + netJoinHostPort(host, port),
		token:   strings.TrimSpace(string(tokenBytes)),
		http:    &http.Client{Transport: transport, Timeout: timeout},
		podREST: coreClient.RESTClient(),
	}, nil
}

func (c *Client) ReviewToken(ctx context.Context, token string, audiences []string) (*TokenReviewStatus, error) {
	request := TokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec: TokenReviewSpec{
			Token:     token,
			Audiences: audiences,
		},
	}
	var response TokenReviewResponse
	if err := c.do(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", request, &response); err != nil {
		return nil, err
	}
	return &response.Status, nil
}

func (c *Client) GetPod(ctx context.Context, namespace, name string) (*Pod, error) {
	var pod Pod
	resourcePath := path.Join("/api/v1/namespaces", namespace, "pods", name)
	if err := c.do(ctx, http.MethodGet, resourcePath, nil, &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

func (c *Client) GetPersistentVolume(ctx context.Context, name string) (*PersistentVolume, error) {
	var pv PersistentVolume
	resourcePath := path.Join("/api/v1/persistentvolumes", name)
	if err := c.do(ctx, http.MethodGet, resourcePath, nil, &pv); err != nil {
		return nil, err
	}
	return &pv, nil
}

func (c *Client) GetPersistentVolumeClaim(ctx context.Context, namespace, name string) (*PersistentVolumeClaimResource, error) {
	var pvc PersistentVolumeClaimResource
	resourcePath := path.Join("/api/v1/namespaces", namespace, "persistentvolumeclaims", name)
	if err := c.do(ctx, http.MethodGet, resourcePath, nil, &pvc); err != nil {
		return nil, err
	}
	return &pvc, nil
}

func (c *Client) do(ctx context.Context, method, resourcePath string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal kubernetes request: %w", err)
		}
		body = bytes.NewReader(payload)
	}

	endpoint := c.baseURL + resourcePath
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create kubernetes request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call kubernetes api %s %s: %w", method, resourcePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{
			Method:     method,
			Path:       resourcePath,
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Detail:     strings.TrimSpace(string(limited)),
		}
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode kubernetes response %s %s: %w", method, resourcePath, err)
	}
	return nil
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return url.URL{Host: host + ":" + port}.Host
}
