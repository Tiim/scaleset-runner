// Command scaleset-runner is the Controller: one process that serves a Scale
// Set per configured repository and runs every Job in a fresh Runner Container.
//
// Usage:
//
//	scaleset-runner              run the Controller (configured by environment)
//	scaleset-runner healthcheck  exit 0 when the running Controller is healthy
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tim/scaleset-runner/internal/config"
	"github.com/tim/scaleset-runner/internal/controller"
	"github.com/tim/scaleset-runner/internal/dockerx"
	"github.com/tim/scaleset-runner/internal/githubx"
	"github.com/tim/scaleset-runner/internal/logging"
	"github.com/tim/scaleset-runner/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	os.Exit(run())
}

func run() int {
	red := logging.NewRedactor()
	cfg, errs := config.Load(os.Getenv)
	if cfg != nil {
		for _, s := range cfg.Secrets() {
			red.Add(s)
		}
	}
	level := slog.LevelInfo
	if cfg != nil && len(errs) == 0 {
		level = cfg.LogLevel
	}
	log := logging.NewLogger(os.Stderr, level, red).With("instance_id", cfgInstance(cfg))
	slog.SetDefault(log)

	// UC-001 A1: name every invalid or missing setting, then exit.
	if len(errs) > 0 {
		for _, err := range errs {
			log.Error("invalid config", "error", err)
		}
		return 2
	}
	log.Info("starting scaleset-runner", "version", version, "repositories", len(cfg.Repos), "slots", cfg.Slots)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// UC-001 A4: an unreachable Docker Engine ends the Controller.
	dock, err := dockerx.New(ctx, cfg.Runtime)
	if err != nil {
		log.Error("Docker Engine unusable", "error", err)
		return 1
	}
	// UC-001 A12: the Instance ID must be unique on the Host.
	if other, err := dock.InstanceInUse(ctx, config.EnvInstanceID, cfg.InstanceID); err != nil {
		log.Error("checking for another controller with this instance ID failed", "error", err)
		return 1
	} else if other != "" {
		log.Error("another running controller uses this instance ID; give one of them another instance ID",
			"other_container", other)
		return 1
	}

	st, err := store.Open(cfg.StatePath, cfg.InstanceID)
	if err != nil {
		log.Error("opening state store failed", "path", cfg.StatePath, "error", err)
		return 1
	}
	defer st.Close()

	githubx.Version = version
	ctl := controller.New(controller.Options{
		Config:   cfg,
		Docker:   dock,
		GitHub:   githubx.New(cfg, log.With("component", "scaleset-client")),
		Store:    st,
		Logger:   log,
		Redactor: red,
	})

	mux := http.NewServeMux()
	mux.Handle("/healthz", ctl.HealthHandler())
	mux.Handle("/metrics", ctl.MetricsHandler())
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("health endpoint cannot listen", "addr", cfg.ListenAddr, "error", err)
		return 1
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health endpoint failed", "error", err)
		}
	}()
	defer srv.Close()

	if err := ctl.Run(ctx); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		log.Error("controller stopped", "error", err)
		return 1
	}
	return 0
}

func cfgInstance(c *config.Config) string {
	if c == nil {
		return ""
	}
	return c.InstanceID
}

// healthcheck queries the health endpoint of the Controller in this container,
// for the Docker Compose healthcheck.
func healthcheck() int {
	addr := os.Getenv(config.EnvListenAddr)
	if addr == "" {
		addr = ":9090"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}
