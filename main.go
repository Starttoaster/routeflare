package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/starttoaster/routeflare/pkg/cloudflare"
	"github.com/starttoaster/routeflare/pkg/config"
	"github.com/starttoaster/routeflare/pkg/controller"
	"github.com/starttoaster/routeflare/pkg/kubernetes"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Create Kubernetes client
	k8sClient, err := kubernetes.NewClient(cfg.KubeconfigPath)
	if err != nil {
		log.Fatalf("Error creating Kubernetes client: %v", err)
	}
	log.Println("Successfully connected to Kubernetes cluster")

	// Create Cloudflare client
	cfClient, err := cloudflare.NewClient(cfg.CloudflareAPIToken)
	if err != nil {
		log.Fatalf("Error creating Cloudflare client: %v", err)
	}

	// Create controller
	ctrl := controller.NewController(cfg, k8sClient, cfClient)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		ctrl.Stop()
	}()

	// Run controller
	if err := ctrl.Run(); err != nil {
		log.Fatalf("Error running controller: %v", err)
	}
}
