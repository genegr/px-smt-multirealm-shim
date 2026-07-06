// Command px-smt-multirealm-shim is the Portworx Enterprise <-> Pure FlashArray REST gateway.
//
// Phase 1 (current): a transparent TLS reverse proxy that forwards every request to the
// real FlashArray and logs the full request/response flow (bodies included) so we can
// identify the exact host-connection calls Portworx makes for FADA realm volumes. It does
// NOT yet rewrite anything — pass-through must be byte-faithful.
//
// See the README for the architecture, the bug being worked around, and the roadmap.
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/genegr/px-smt-multirealm-shim/internal/config"
	"github.com/genegr/px-smt-multirealm-shim/internal/proxy"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("px-smt-multirealm-shim starting: listen=%s upstream=%s logBodies=%v maxBody=%d",
		cfg.ListenAddr, cfg.UpstreamURL, cfg.LogBodies, cfg.MaxLogBodyBytes)

	tlsCert, err := cfg.LoadOrGenerateCert()
	if err != nil {
		log.Fatalf("tls cert: %v", err)
	}

	handler, err := proxy.New(cfg)
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	// Graceful shutdown so in-flight FlashArray calls are not cut mid-response.
	idleClosed := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		log.Printf("shutdown signal received, draining...")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
		close(idleClosed)
	}()

	// ListenAndServeTLS with empty cert/key files uses srv.TLSConfig.Certificates.
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	<-idleClosed
	log.Printf("px-smt-multirealm-shim stopped")
}
