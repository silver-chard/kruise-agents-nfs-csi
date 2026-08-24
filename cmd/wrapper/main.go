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
	cfg, err := config.LoadWrapperConfig()
	if err != nil {
		log.Fatalf("load wrapper config: %v", err)
	}

	bindWrapperFlags(flag.CommandLine, &cfg)
	flag.Parse()

	logger := log.New(os.Stdout, "kruise-nfs-wrapper ", log.LstdFlags|log.LUTC)

	kubeClient, err := kube.NewInClusterClient(cfg.KubeTokenFile, cfg.KubeCAFile, cfg.RequestTimeout)
	if err != nil {
		logger.Fatalf("create kubernetes client: %v", err)
	}

	nodeMounter := node.NewMounter(node.Config{
		DriverName:            cfg.DriverName,
		StagingRoot:           cfg.StagingRoot,
		HostProcRoot:          cfg.HostProcRoot,
		EnableMount:           cfg.EnableMount,
		UnstageAfterMount:     cfg.UnstageAfterMount,
		CreateMissingSubPaths: cfg.CreateMissingSubPaths,
		CreatedSubPathMode:    cfg.CreatedSubPathMode,
		Timeout:               cfg.RequestTimeout,
	})
	if cfg.UnstageAfterMount {
		if err := node.CleanupStagingRoot(cfg.StagingRoot); err != nil {
			logger.Printf("warning: cleanup staging root %s: %v", cfg.StagingRoot, err)
		}
	}

	wrapperServer, err := wrapper.NewServer(cfg, kubeClient, nodeMounter, logger)
	if err != nil {
		logger.Fatalf("create wrapper server: %v", err)
	}
	httpServer := &http.Server{
		Handler: wrapperServer,
	}

	listener, err := listenUnix(cfg.SocketPath, cfg.SocketMode)
	if err != nil {
		logger.Fatalf("listen on unix socket: %v", err)
	}
	logger.Printf("listening on %s for driver %s", cfg.SocketPath, cfg.DriverName)

	errCh := make(chan error, 1)
	reconcileCtx, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()
	go wrapperServer.RunReconciler(reconcileCtx)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-signalCh:
		logger.Printf("received signal %s, shutting down", sig)
		stopReconciler()
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Printf("shutdown failed: %v", err)
		}
	case err := <-errCh:
		stopReconciler()
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server failed: %v", err)
		}
	}
}

func bindWrapperFlags(flagSet *flag.FlagSet, cfg *config.WrapperConfig) {
	flagSet.StringVar(&cfg.DriverName, "driver-name", cfg.DriverName, "CSI driver name")
	flagSet.StringVar(&cfg.SocketPath, "socket-path", cfg.SocketPath, "Unix socket path")
	flagSet.StringVar(&cfg.TokenAudience, "token-audience", cfg.TokenAudience, "projected service account token audience")
	flagSet.StringVar(&cfg.KubeTokenFile, "kube-token-file", cfg.KubeTokenFile, "wrapper service account token file")
	flagSet.StringVar(&cfg.KubeCAFile, "kube-ca-file", cfg.KubeCAFile, "Kubernetes CA file")
	flagSet.StringVar(&cfg.StagingRoot, "staging-root", cfg.StagingRoot, "node staging root")
	flagSet.StringVar(&cfg.MountStateDir, "mount-state-dir", cfg.MountStateDir, "persistent desired mount state directory")
	flagSet.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "Kubernetes node name used to watch local pods")
	flagSet.StringVar(&cfg.HostProcRoot, "host-proc-root", cfg.HostProcRoot, "host proc root visible to wrapper")
	flagSet.BoolVar(&cfg.EnableMount, "enable-mount", cfg.EnableMount, "enable real node mount operations")
	flagSet.BoolVar(&cfg.UnstageAfterMount, "unstage-after-mount", cfg.UnstageAfterMount, "unmount wrapper staging source after each dynamic bind mount")
	flagSet.BoolVar(&cfg.CreateMissingSubPaths, "create-missing-subpaths", cfg.CreateMissingSubPaths, "create missing source subPath directories")
	flagSet.Var(&fileModeValue{target: &cfg.CreatedSubPathMode}, "created-subpath-mode", "Unix mode for newly created source subPath directories")
}

type fileModeValue struct {
	target *os.FileMode
}

func (value *fileModeValue) String() string {
	if value == nil || value.target == nil {
		return ""
	}
	return config.FormatFileMode(*value.target)
}

func (value *fileModeValue) Set(raw string) error {
	mode, err := config.ParseFileMode(raw)
	if err != nil {
		return err
	}
	*value.target = mode
	return nil
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
