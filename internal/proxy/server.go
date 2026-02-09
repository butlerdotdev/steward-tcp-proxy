// Copyright 2025 Butler Labs LLC.
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	// kubernetesEndpointSliceName is the name of the EndpointSlice we manage.
	kubernetesEndpointSliceName = "kubernetes"

	// kubernetesNamespace is where the kubernetes EndpointSlice lives.
	kubernetesNamespace = "default"

	// tcpProxyNamespace is where the tcp-proxy Service lives.
	tcpProxyNamespace = "kube-system"

	// managedByLabel identifies EndpointSlices managed by tcp-proxy.
	managedByLabel = "steward-tcp-proxy"
)

// Config holds the configuration for the proxy server.
type Config struct {
	// ListenAddr is the address to listen on for incoming connections.
	ListenAddr string

	// UpstreamAddr is the address of the real API server to proxy to.
	// For TLS mode, this should be the Ingress hostname:port (e.g., "tenant.k8s.example.com:443")
	UpstreamAddr string

	// ServiceName is the name of the tcp-proxy Service in kube-system.
	ServiceName string

	// ServicePort is the port of the tcp-proxy Service.
	ServicePort int32

	// ReconcileInterval is how often to reconcile the EndpointSlice.
	ReconcileInterval time.Duration

	// Clientset is the Kubernetes client for managing EndpointSlices.
	Clientset kubernetes.Interface

	// TLSCertFile is the path to the TLS certificate file (API server cert).
	// If empty, tcp-proxy operates in passthrough mode.
	TLSCertFile string

	// TLSKeyFile is the path to the TLS private key file.
	TLSKeyFile string

	// UpstreamSNI is the SNI hostname to use when connecting to upstream.
	// Required for TLS mode when connecting through an Ingress.
	// If empty, defaults to the host portion of UpstreamAddr.
	UpstreamSNI string

	// UpstreamInsecure skips TLS verification for upstream connections.
	// Use only for testing.
	UpstreamInsecure bool
}

// Server is the tcp-proxy server that handles both proxying and
// EndpointSlice reconciliation.
type Server struct {
	config    Config
	listener  net.Listener
	tlsConfig *tls.Config
	wg        sync.WaitGroup

	// Metrics
	activeConns   atomic.Int64
	totalConns    atomic.Int64
	totalBytes    atomic.Int64
	reconcileOK   atomic.Int64
	reconcileFail atomic.Int64
}

// NewServer creates a new proxy server with the given configuration.
func NewServer(config Config) *Server {
	return &Server{
		config: config,
	}
}

// Run starts the proxy server and EndpointSlice reconciler.
// It blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Start the EndpointSlice reconciler
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.reconcileLoop(ctx)
	}()

	// Determine if we're in TLS termination mode
	tlsMode := s.config.TLSCertFile != "" && s.config.TLSKeyFile != ""

	var err error
	if tlsMode {
		// Load TLS certificate
		cert, err := tls.LoadX509KeyPair(s.config.TLSCertFile, s.config.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("failed to load TLS certificate: %w", err)
		}

		s.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		s.listener, err = tls.Listen("tcp", s.config.ListenAddr, s.tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to listen on %s with TLS: %w", s.config.ListenAddr, err)
		}
		klog.Infof("TLS proxy listening on %s, forwarding to %s (SNI: %s)",
			s.config.ListenAddr, s.config.UpstreamAddr, s.getUpstreamSNI())
	} else {
		// Plain TCP passthrough mode
		s.listener, err = net.Listen("tcp", s.config.ListenAddr)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %w", s.config.ListenAddr, err)
		}
		klog.Infof("TCP proxy (passthrough) listening on %s, forwarding to %s",
			s.config.ListenAddr, s.config.UpstreamAddr)
	}
	defer s.listener.Close()

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	// Accept loop
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break // Clean shutdown
			}
			klog.Errorf("Accept error: %v", err)
			continue
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if tlsMode {
				s.handleTLSConnection(ctx, conn)
			} else {
				s.handleConnection(ctx, conn)
			}
		}()
	}

	// Wait for all goroutines
	klog.Info("Waiting for active connections to close...")
	s.wg.Wait()
	return nil
}

// getUpstreamSNI returns the SNI hostname to use for upstream connections.
func (s *Server) getUpstreamSNI() string {
	if s.config.UpstreamSNI != "" {
		return s.config.UpstreamSNI
	}
	// Extract host from UpstreamAddr
	host, _, err := net.SplitHostPort(s.config.UpstreamAddr)
	if err != nil {
		return s.config.UpstreamAddr
	}
	return host
}

// handleTLSConnection handles a TLS-terminated connection.
// It terminates TLS from the client and establishes a new TLS connection
// to the upstream with proper SNI.
func (s *Server) handleTLSConnection(ctx context.Context, clientConn net.Conn) {
	s.activeConns.Add(1)
	s.totalConns.Add(1)
	defer func() {
		s.activeConns.Add(-1)
		clientConn.Close()
	}()

	// The clientConn is already a TLS connection (from tls.Listen)
	// Now connect to upstream with TLS and proper SNI
	upstreamTLSConfig := &tls.Config{
		ServerName:         s.getUpstreamSNI(),
		InsecureSkipVerify: s.config.UpstreamInsecure,
		MinVersion:         tls.VersionTLS12,
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    upstreamTLSConfig,
	}

	upstreamConn, err := dialer.DialContext(ctx, "tcp", s.config.UpstreamAddr)
	if err != nil {
		klog.Errorf("Failed to connect to upstream %s (SNI: %s): %v",
			s.config.UpstreamAddr, s.getUpstreamSNI(), err)
		return
	}
	defer upstreamConn.Close()

	klog.V(4).Infof("TLS proxying %s -> %s (SNI: %s)",
		clientConn.RemoteAddr(), s.config.UpstreamAddr, s.getUpstreamSNI())

	// Bidirectional copy of decrypted data
	s.relayConnections(clientConn, upstreamConn, ctx)
}

// handleConnection proxies a single connection to the upstream server.
// This is a simple TCP passthrough - all bytes are forwarded unchanged.
func (s *Server) handleConnection(ctx context.Context, clientConn net.Conn) {
	s.activeConns.Add(1)
	s.totalConns.Add(1)
	defer func() {
		s.activeConns.Add(-1)
		clientConn.Close()
	}()

	// Connect to upstream with timeout
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	upstreamConn, err := dialer.DialContext(ctx, "tcp", s.config.UpstreamAddr)
	if err != nil {
		klog.Errorf("Failed to connect to upstream %s: %v", s.config.UpstreamAddr, err)
		return
	}
	defer upstreamConn.Close()

	klog.V(4).Infof("Proxying %s -> %s", clientConn.RemoteAddr(), s.config.UpstreamAddr)

	// Bidirectional copy
	s.relayConnections(clientConn, upstreamConn, ctx)
}

// relayConnections performs bidirectional data relay between two connections.
func (s *Server) relayConnections(clientConn, upstreamConn net.Conn, ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Client -> Upstream
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstreamConn, clientConn)
		s.totalBytes.Add(n)
		// Signal upstream we're done sending
		if tc, ok := upstreamConn.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
	}()

	// Upstream -> Client
	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientConn, upstreamConn)
		s.totalBytes.Add(n)
		// Signal client we're done sending
		if tc, ok := clientConn.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
	}()

	// Wait for both directions or context cancel
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// reconcileLoop periodically reconciles the kubernetes EndpointSlice.
func (s *Server) reconcileLoop(ctx context.Context) {
	// Reconcile immediately on startup
	if err := s.reconcileEndpointSlice(ctx); err != nil {
		klog.Errorf("Initial EndpointSlice reconciliation failed: %v", err)
		s.reconcileFail.Add(1)
	} else {
		s.reconcileOK.Add(1)
	}

	ticker := time.NewTicker(s.config.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.reconcileEndpointSlice(ctx); err != nil {
				klog.Errorf("EndpointSlice reconciliation failed: %v", err)
				s.reconcileFail.Add(1)
			} else {
				s.reconcileOK.Add(1)
			}
		}
	}
}

// reconcileEndpointSlice ensures the kubernetes EndpointSlice points to the
// tcp-proxy service instead of the external API server.
func (s *Server) reconcileEndpointSlice(ctx context.Context) error {
	// Get the tcp-proxy service to find its ClusterIP
	svc, err := s.config.Clientset.CoreV1().Services(tcpProxyNamespace).Get(
		ctx, s.config.ServiceName, metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get tcp-proxy service %s/%s: %w",
			tcpProxyNamespace, s.config.ServiceName, err)
	}

	proxyIP := svc.Spec.ClusterIP
	if proxyIP == "" || proxyIP == "None" {
		return fmt.Errorf("tcp-proxy service has no ClusterIP")
	}

	klog.V(4).Infof("Ensuring EndpointSlice points to tcp-proxy at %s:%d",
		proxyIP, s.config.ServicePort)

	// Get the existing kubernetes EndpointSlice
	es, err := s.config.Clientset.DiscoveryV1().EndpointSlices(kubernetesNamespace).Get(
		ctx, kubernetesEndpointSliceName, metav1.GetOptions{},
	)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return s.createEndpointSlice(ctx, proxyIP)
		}
		return fmt.Errorf("failed to get EndpointSlice: %w", err)
	}

	// Check if already correct
	if s.endpointSliceMatches(es, proxyIP) {
		klog.V(4).Info("EndpointSlice already points to tcp-proxy, no update needed")
		return nil
	}

	// Update the EndpointSlice
	es.Endpoints = []discoveryv1.Endpoint{
		{
			Addresses: []string{proxyIP},
			Conditions: discoveryv1.EndpointConditions{
				Ready:       ptr.To(true),
				Serving:     ptr.To(true),
				Terminating: ptr.To(false),
			},
		},
	}
	es.Ports = []discoveryv1.EndpointPort{
		{
			Name:     ptr.To("https"),
			Port:     ptr.To(s.config.ServicePort),
			Protocol: ptr.To(corev1.ProtocolTCP),
		},
	}

	// Add managed-by label
	if es.Labels == nil {
		es.Labels = make(map[string]string)
	}
	es.Labels["endpointslice.kubernetes.io/managed-by"] = managedByLabel

	_, err = s.config.Clientset.DiscoveryV1().EndpointSlices(kubernetesNamespace).Update(
		ctx, es, metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update EndpointSlice: %w", err)
	}

	klog.Infof("Updated EndpointSlice %s/%s to point to tcp-proxy at %s:%d",
		kubernetesNamespace, kubernetesEndpointSliceName, proxyIP, s.config.ServicePort)
	return nil
}

// createEndpointSlice creates the kubernetes EndpointSlice pointing to tcp-proxy.
func (s *Server) createEndpointSlice(ctx context.Context, proxyIP string) error {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubernetesEndpointSliceName,
			Namespace: kubernetesNamespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName:             "kubernetes",
				"endpointslice.kubernetes.io/managed-by": managedByLabel,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses: []string{proxyIP},
				Conditions: discoveryv1.EndpointConditions{
					Ready:       ptr.To(true),
					Serving:     ptr.To(true),
					Terminating: ptr.To(false),
				},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     ptr.To("https"),
				Port:     ptr.To(s.config.ServicePort),
				Protocol: ptr.To(corev1.ProtocolTCP),
			},
		},
	}

	_, err := s.config.Clientset.DiscoveryV1().EndpointSlices(kubernetesNamespace).Create(
		ctx, es, metav1.CreateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to create EndpointSlice: %w", err)
	}

	klog.Infof("Created EndpointSlice %s/%s pointing to tcp-proxy at %s:%d",
		kubernetesNamespace, kubernetesEndpointSliceName, proxyIP, s.config.ServicePort)
	return nil
}

// endpointSliceMatches checks if the EndpointSlice already points to our proxy.
func (s *Server) endpointSliceMatches(es *discoveryv1.EndpointSlice, proxyIP string) bool {
	if len(es.Endpoints) != 1 {
		return false
	}
	if len(es.Endpoints[0].Addresses) != 1 {
		return false
	}
	if es.Endpoints[0].Addresses[0] != proxyIP {
		return false
	}
	if len(es.Ports) != 1 {
		return false
	}
	if es.Ports[0].Port == nil || *es.Ports[0].Port != s.config.ServicePort {
		return false
	}
	return true
}

// Stats returns current server statistics.
func (s *Server) Stats() (active, total, bytes, reconcileOK, reconcileFail int64) {
	return s.activeConns.Load(), s.totalConns.Load(), s.totalBytes.Load(),
		s.reconcileOK.Load(), s.reconcileFail.Load()
}
