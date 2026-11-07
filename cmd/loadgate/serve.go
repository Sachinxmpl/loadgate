package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/Sachinxmpl/loadgate/internal/balancer"
	"github.com/Sachinxmpl/loadgate/internal/config"
	"github.com/Sachinxmpl/loadgate/internal/debug"
	"github.com/Sachinxmpl/loadgate/internal/health"
	"github.com/Sachinxmpl/loadgate/internal/listener"
	"github.com/Sachinxmpl/loadgate/internal/metrics"
	"github.com/Sachinxmpl/loadgate/internal/proxy/l7"
	"github.com/Sachinxmpl/loadgate/internal/ratelimit"
	"github.com/Sachinxmpl/loadgate/internal/reload"
)

type server interface {
	Start() error
	ShutDown(context.Context) error
}

func Serve(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("c", "config.yaml", "path to the config file")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "json", "json or text")

	metricServerAddr := fs.String(
		"metrics-addr",
		"127.0.0.1:9095",
		"metrics server address",
	)

	debugAddr := fs.String(
		"debug-addr",
		"",
		"pprof debug server address, e.g. 127.0.0.1:6060 (empty = disabled)",
	)

	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*path)
	if err != nil {
		log.Error("failed to load config", "path", *path, "err", err)
		return err
	}

	store := config.NewStore(cfg)

	registry := health.NewRegistry()
	added, _ := registry.Reconcile(cfg.BackendAddrs())

	mtrs := metrics.New(registry)

	metricsSrv := metrics.NewServer(*metricServerAddr, mtrs, log)
	if err := metricsSrv.Start(); err != nil {
		log.Error("failed to start metrics server", "addr", *metricServerAddr, "err", err)
		return err
	}

	var debugSrv *debug.Server
	if *debugAddr != "" {
		debugSrv = debug.NewServer(*debugAddr, log)
		if err := debugSrv.Start(); err != nil {
			log.Error("failed to start debug server", "addr", *debugAddr, "err", err)
			return err
		}
	}

	mgr := health.NewManager(registry, cfg.Health, log, mtrs.HealthTransition)
	mgr.Sync(added, nil)
	defer mgr.Stop()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"config", *path,
		"mode", cfg.Mode,
		"listen", cfg.Listen,
		"balancer", cfg.Balancer,
		"pools", len(cfg.Pools),
		"backends", len(cfg.AllBackends()),
	)

	var limiter *ratelimit.Limiter
	if cfg.RateLimit.GlobalRPS > 0 || cfg.RateLimit.PerClientRPS > 0 {
		limiter = ratelimit.New(cfg.RateLimit.GlobalRPS, cfg.RateLimit.PerClientRPS)
	}

	var srv server
	switch cfg.Mode {
	case config.ModeL4:
		bal, err := balancer.New(cfg.Balancer, registry)
		if err != nil {
			log.Error("failed to create balancer", "algorithm", cfg.Balancer, "err", err)
			return err
		}
		srv = listener.New(listener.Options{Addr: cfg.Listen, Store: store, Balancer: bal, Registry: registry, Limiter: limiter, Log: log, Metrics: mtrs})
	case config.ModeL7:
		// one balancer per pool, so pools never share round-robin or hash state
		bals, err := balancer.NewPools(cfg.Balancer, registry)
		if err != nil {
			log.Error("failed to create balancer", "algorithm", cfg.Balancer, "err", err)
			return err
		}
		srv = l7.New(l7.Options{Config: cfg, Store: store, Balancers: bals, Registry: registry, Limiter: limiter, Log: log, Metrics: mtrs})
	}

	if err := srv.Start(); err != nil {
		log.Error("failed to start server", "mode", cfg.Mode, "listen", cfg.Listen, "err", err)
		return err
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	// exists then shutdown context is cancelled (SIGINT/SIGTERM)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				err := reload.Apply(*path, store, registry, mgr, log)
				if err != nil {
					log.Error("reloaded failed, keeping old config", "err", err)
				}
				mtrs.ReloadResult(err == nil)
			}
		}
	}()

	<-ctx.Done()
	stop()

	drain := store.Load().Timeouts.Drain.Std()
	log.Info("shutdown signal received, draining", "timeout", drain)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()

	if err := srv.ShutDown(shutdownCtx); err != nil {
		log.Warn("drain did not complete cleanly", "err", err)
	}

	if err := metricsSrv.ShutDown(shutdownCtx); err != nil {
		log.Warn("Failed to shutdown metrics server", "err", err)
	}

	if debugSrv != nil {
		if err := debugSrv.ShutDown(shutdownCtx); err != nil {
			log.Warn("failed to shutdown debug server", "err", err)
		}
	}

	log.Info("stopped")
	return nil
}
