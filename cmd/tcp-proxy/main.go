// Copyright 2025 Butler Labs LLC.
// SPDX-License-Identifier: Apache-2.0

// steward-tcp-proxy solves the "wrong endpoint" problem in Steward/Kamaji
// tenant clusters. When a tenant cluster's kube-apiserver runs in the
// management cluster, the default `kubernetes` EndpointSlice points to the
// external address. This breaks in-cluster clients expecting the ClusterIP.
//
// tcp-proxy solves this by:
// 1. Rewriting the `kubernetes` EndpointSlice to point to itself
// 2. Proxying all traffic to the real API server endpoint
//
// For Ingress/Gateway modes, tcp-proxy operates in TLS termination mode:
// - Terminates TLS from pods using the API server certificate
// - Establishes a new TLS connection to the Ingress with proper SNI
// - Relays decrypted data between the two connections
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/butlerdotdev/steward-tcp-proxy/internal/proxy"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

var (
	listenAddr        = flag.String("listen-addr", ":6443", "Address to listen on for proxied connections")
	upstreamAddr      = flag.String("upstream-addr", "", "Upstream API server address (host:port, required)")
	serviceName       = flag.String("service-name", "steward-tcp-proxy", "Name of the tcp-proxy Service in kube-system")
	servicePort       = flag.Int("service-port", 6443, "Port of the tcp-proxy Service")
	reconcileInterval = flag.Duration("reconcile-interval", 30*time.Second, "How often to reconcile the EndpointSlice")

	// TLS termination mode flags
	tlsCertFile      = flag.String("tls-cert-file", "", "Path to TLS certificate file (enables TLS termination mode)")
	tlsKeyFile       = flag.String("tls-key-file", "", "Path to TLS private key file")
	upstreamSNI      = flag.String("upstream-sni", "", "SNI hostname for upstream TLS connections (defaults to upstream-addr host)")
	upstreamInsecure = flag.Bool("upstream-insecure", false, "Skip TLS verification for upstream (testing only)")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	// Allow upstream address from env if not set via flag
	upstream := *upstreamAddr
	if upstream == "" {
		upstream = os.Getenv("UPSTREAM_ADDR")
	}
	if upstream == "" {
		klog.Fatal("--upstream-addr or UPSTREAM_ADDR is required")
	}

	// Allow TLS cert/key from env
	certFile := *tlsCertFile
	if certFile == "" {
		certFile = os.Getenv("TLS_CERT_FILE")
	}
	keyFile := *tlsKeyFile
	if keyFile == "" {
		keyFile = os.Getenv("TLS_KEY_FILE")
	}

	// Validate TLS config
	if (certFile != "" && keyFile == "") || (certFile == "" && keyFile != "") {
		klog.Fatal("Both --tls-cert-file and --tls-key-file must be specified together")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		klog.Infof("Received signal %v, initiating graceful shutdown", sig)
		cancel()
	}()

	// Create Kubernetes client (in-cluster config)
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Create and run the proxy server
	srv := proxy.NewServer(proxy.Config{
		ListenAddr:        *listenAddr,
		UpstreamAddr:      upstream,
		ServiceName:       *serviceName,
		ServicePort:       int32(*servicePort),
		ReconcileInterval: *reconcileInterval,
		Clientset:         clientset,
		TLSCertFile:       certFile,
		TLSKeyFile:        keyFile,
		UpstreamSNI:       *upstreamSNI,
		UpstreamInsecure:  *upstreamInsecure,
	})

	mode := "passthrough"
	if certFile != "" {
		mode = "TLS termination"
	}
	klog.Infof("Starting steward-tcp-proxy (%s mode): listen=%s upstream=%s", mode, *listenAddr, upstream)

	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		klog.Fatalf("Server error: %v", err)
	}

	klog.Info("steward-tcp-proxy shutdown complete")
}
