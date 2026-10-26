package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8gb-ui/internal/console"
	"k8gb-ui/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	path := flag.String("config", "console.yaml", "Console configuration file")
	demo := flag.Bool("demo", false, "Use illustrative data, without Kubernetes or external services")
	listen := flag.String("listen", "127.0.0.1:8080", "Demo listen address (must be literal loopback)")
	flag.Parse()
	var cfg console.Config
	var agents map[string]console.Agent
	var err error
	if *demo {
		cfg, agents = console.Demo(*listen)
	} else {
		cfg, err = console.LoadConfig(*path)
	}
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handler, err := console.New(ctx, cfg, web.Assets, agents, *demo)
	if err != nil {
		slog.Error("initialize console", "error", err)
		os.Exit(1)
	}
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("console listening", "address", cfg.Listen, "writesEnabled", cfg.WritesEnabled, "demo", *demo)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("console server stopped", "error", err)
		os.Exit(1)
	}
}
