package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/wrapper"
)

func main() {
	cfg := config.LoadWrapperConfig()

	flag.StringVar(&cfg.DriverName, "driver-name", cfg.DriverName, "CSI driver name")
	flag.StringVar(&cfg.SocketPath, "socket-path", cfg.SocketPath, "Unix socket path")
	flag.StringVar(&cfg.TokenAudience, "token-audience", cfg.TokenAudience, "projected service account token audience")
	flag.StringVar(&cfg.KubeTokenFile, "kube-token-file", cfg.KubeTokenFile, "wrapper service account token file")
	flag.StringVar(&cfg.KubeCAFile, "kube-ca-file", cfg.KubeCAFile, "Kubernetes CA file")
	flag.StringVar(&cfg.StagingRoot, "staging-root", cfg.StagingRoot, "node staging root")
	flag.StringVar(&cfg.HostProcRoot, "host-proc-root", cfg.HostProcRoot, "host proc root visible to wrapper")
	flag.BoolVar(&cfg.EnableMount, "enable-mount", cfg.EnableMount, "enable real node mount operations")
	flag.Parse()

	logger := log.New(os.Stdout, "kruise-nfs-wrapper ", log.LstdFlags|log.LUTC)

	kubeClient, err := kube.NewInClusterClient(cfg.KubeTokenFile, cfg.KubeCAFile, cfg.RequestTimeout)
	if err != nil {
		logger.Fatalf("create kubernetes client: %v", err)
	}

	nodeMounter := node.NewMounter(node.Config{
		DriverName:   cfg.DriverName,
		StagingRoot:  cfg.StagingRoot,
		HostProcRoot: cfg.HostProcRoot,
		EnableMount:  cfg.EnableMount,
		Timeout:      cfg.RequestTimeout,
	})
	httpServer := &http.Server{
		Handler: wrapper.NewServer(cfg, kubeClient, nodeMounter, logger),
	}

	listener, err := listenUnix(cfg.SocketPath, cfg.SocketMode)
	if err != nil {
		logger.Fatalf("listen on unix socket: %v", err)
	}
	logger.Printf("listening on %s for driver %s", cfg.SocketPath, cfg.DriverName)

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-signalCh:
		logger.Printf("received signal %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Printf("shutdown failed: %v", err)
		}
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server failed: %v", err)
		}
	}
}

func listenUnix(socketPath string, mode os.FileMode) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if stat, err := os.Lstat(socketPath); err == nil {
		if stat.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return listener, nil
}
