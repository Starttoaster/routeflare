package main

import (
	"github.com/chia-network/go-modules/pkg/slogs"
	"os"
	"os/signal"
	"syscall"

	"github.com/starttoaster/routeflare/pkg/cloudflare"
	"github.com/starttoaster/routeflare/pkg/config"
	"github.com/starttoaster/routeflare/pkg/controller"
	"github.com/starttoaster/routeflare/pkg/kubernetes"
)

func main() {
	slogs.Init("info")

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		slogs.Logr.Fatal("loading config", "error", err)
	}

	// Init clients
	k8sClient, err := kubernetes.NewClient(cfg.KubeconfigPath)
	if err != nil {
		slogs.Logr.Fatal("creating Kubernetes client", "error", err)
	}
	slogs.Logr.Info("Successfully connected to Kubernetes cluster")

	cfClient, err := cloudflare.NewClient(cfg.CloudflareAPIToken)
	if err != nil {
		slogs.Logr.Fatal("creating Cloudflare client", "error", err)
	}

	ctrl := controller.NewController(cfg, k8sClient, cfClient)

	// Handler for graceful shutdowns
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		slogs.Logr.Info("Received shutdown signal, shutting down...")
		ctrl.Stop()
	}()

	// Run controller
	if err := ctrl.Run(); err != nil {
		slogs.Logr.Fatal("running controller", "error", err)
	}
}
