package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Run starts the gridlane HTTP surface (and optional metrics listener) and
// blocks until the provided context is cancelled or a listener fails.
// SIGHUP triggers Reload when opts.ReloadOnSIGHUP is set.
func Run(ctx context.Context, opts Options, logger *slog.Logger) error {
	handler, err := NewReloadingHandler(opts)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              opts.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       opts.ProxyTimeout,
		WriteTimeout:      opts.ProxyTimeout,
		IdleTimeout:       2 * opts.GracefulPeriod,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 2)
	startServer := func(name string, srv *http.Server) {
		go func() {
			logger.Info("starting "+name, "listen", srv.Addr)
			errCh <- srv.ListenAndServe()
		}()
	}
	var metricsSrv *http.Server
	if opts.MetricsListen != "" {
		metricsSrv = &http.Server{
			Addr:              opts.MetricsListen,
			Handler:           handler.MetricsHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       opts.GracefulPeriod,
			MaxHeaderBytes:    1 << 20,
		}
	}

	reloadCtx, stopReload := context.WithCancel(ctx)
	defer stopReload()
	if opts.ReloadOnSIGHUP {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		defer signal.Stop(hupCh)
		go func() {
			for {
				select {
				case <-reloadCtx.Done():
					return
				case <-hupCh:
					if err := handler.Reload(); err != nil {
						logger.Error("config reload failed", "path", opts.ConfigPath, "error", err)
						continue
					}
					logger.Info("config reloaded", "path", opts.ConfigPath)
				}
			}
		}()
	}

	startServer("gridlane", srv)
	if metricsSrv != nil {
		startServer("gridlane metrics", metricsSrv)
	}

	select {
	case <-ctx.Done():
		return shutdownServers(opts.GracefulPeriod, srv, metricsSrv)
	case err := <-errCh:
		stopReload()
		_ = shutdownServers(opts.GracefulPeriod, srv, metricsSrv)
		return err
	}
}

// shutdownServers drains each listener within a shared graceful timeout
// anchored to context.Background so the shutdown window is not cut short if
// the parent context was the trigger.
func shutdownServers(timeout time.Duration, servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var shutdownErr error
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}
