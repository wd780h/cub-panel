// Command cub-agent is the node-side daemon. It exposes a small
// HMAC-authenticated API over which the panel drives the local Incus/LXD
// daemon (the two speak the same REST API).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cubpanel/internal/agent"
	"cubpanel/internal/shared"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := agent.Config{}
	flag.StringVar(&cfg.Listen, "listen", env("CUB_AGENT_LISTEN", "0.0.0.0:8788"), "listen address")
	flag.StringVar(&cfg.LXDSocket, "lxd-socket", env("CUB_AGENT_SOCKET", "/var/lib/incus/unix.socket"), "Incus/LXD unix socket")
	flag.StringVar(&cfg.StoragePool, "pool", env("CUB_AGENT_POOL", "cub"), "storage pool")
	flag.StringVar(&cfg.ImageServer, "image-server", env("CUB_AGENT_IMAGE_SERVER", "https://images.linuxcontainers.org"), "simplestreams image server")
	flag.StringVar(&cfg.ISODir, "iso-dir", env("CUB_AGENT_ISO_DIR", "/var/lib/cub-panel/isos"), "directory holding uploaded ISO images")
	flag.BoolVar(&cfg.Verbose, "v", env("CUB_AGENT_VERBOSE", "") != "", "verbose logging")
	var (
		tlsOn   bool
		tlsCert string
		tlsKey  string
	)
	flag.BoolVar(&tlsOn, "tls", env("CUB_AGENT_TLS", "1") != "0", "serve HTTPS with a self-signed certificate")
	flag.StringVar(&tlsCert, "tls-cert", env("CUB_AGENT_TLS_CERT", "agent-cert.pem"), "TLS certificate path (created if missing)")
	flag.StringVar(&tlsKey, "tls-key", env("CUB_AGENT_TLS_KEY", "agent-key.pem"), "TLS key path (created if missing)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	// The self-update path runs the freshly downloaded binary with -version to
	// prove it executes on this machine before it replaces the running one.
	if *showVersion {
		fmt.Println(shared.Version)
		return
	}

	cfg.Secret = os.Getenv("CUB_AGENT_SECRET")
	if len(cfg.Secret) < 32 {
		log.Fatal("CUB_AGENT_SECRET must be set to at least 32 characters")
	}

	// Fall back to LXD socket paths when the Incus one is absent.
	if _, err := os.Stat(cfg.LXDSocket); err != nil {
		for _, alt := range []string{"/var/lib/lxd/unix.socket", "/var/snap/lxd/common/lxd/unix.socket"} {
			if _, err := os.Stat(alt); err == nil {
				log.Printf("socket %s missing, using %s", cfg.LXDSocket, alt)
				cfg.LXDSocket = alt
				break
			}
		}
	}

	ag := agent.New(cfg)
	// Agent-managed DNAT rules (unmanaged-bridge nodes) live in the kernel
	// and vanish on reboot; replay them from the instance records.
	go ag.RestoreDNAT()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           ag.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the console websocket is long-lived.
	}

	scheme := "http"
	if tlsOn {
		scheme = "https"
		fp, err := agent.EnsureTLSCert(tlsCert, tlsKey)
		if err != nil {
			log.Fatalf("tls certificate: %v", err)
		}
		// A sidecar file lets install scripts print the fingerprint without
		// parsing the certificate themselves.
		_ = os.WriteFile(tlsCert+".fp", []byte(fp+"\n"), 0o644)
		log.Printf("tls certificate fingerprint (pin this in the panel): %s", fp)
	}

	go func() {
		log.Printf("cub-agent listening on %s (%s, socket: %s, pool: %s)", cfg.Listen, scheme, cfg.LXDSocket, cfg.StoragePool)
		var err error
		if tlsOn {
			err = srv.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Print("cub-agent stopped")
}
