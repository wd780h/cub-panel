// Command cub-panel is the master web application: storefront, tenant
// dashboard and administration backend.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cubpanel/internal/panel"
	"cubpanel/internal/store"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func main() {
	var cfg panel.Config
	flag.StringVar(&cfg.Listen, "listen", env("CUB_PANEL_LISTEN", "0.0.0.0:8080"), "listen address")
	flag.StringVar(&cfg.DBPath, "db", env("CUB_PANEL_DB", "./data/panel.db"), "sqlite database path")
	flag.StringVar(&cfg.SiteName, "site", env("CUB_PANEL_SITE", "Cub Panel"), "site name")
	flag.BoolVar(&cfg.SecureCookies, "secure-cookies", envBool("CUB_PANEL_SECURE_COOKIES", false), "set the Secure flag on cookies (enable behind HTTPS)")
	flag.BoolVar(&cfg.TrustProxy, "trust-proxy", envBool("CUB_PANEL_TRUST_PROXY", false), "trust X-Forwarded-For / X-Real-IP")
	flag.BoolVar(&cfg.AllowSignup, "allow-signup", envBool("CUB_PANEL_ALLOW_SIGNUP", true), "allow public registration")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	srv, err := panel.New(cfg, db)
	if err != nil {
		log.Fatalf("init panel: %v", err)
	}

	ctx, stopJobs := context.WithCancel(context.Background())
	defer stopJobs()
	srv.StartJobs(ctx)

	if n, _ := db.CountUsers(ctx); n == 0 {
		fmt.Println(strings.Repeat("=", 62))
		fmt.Println("  尚无账号：第一个注册的用户将自动成为管理员。")
		fmt.Printf("  请立即访问 http://<服务器地址>%s/register 完成注册。\n", portOf(cfg.Listen))
		fmt.Println(strings.Repeat("=", 62))
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the console websocket is long-lived.
	}

	go func() {
		log.Printf("cub-panel listening on %s (db: %s)", cfg.Listen, cfg.DBPath)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("shutting down...")
	stopJobs()
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(sctx)
}

// portOf renders the listen port for the first-run hint.
func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ""
}
