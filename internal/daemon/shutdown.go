package daemon

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// SignalHandler manages OS signal handling for the daemon.
type SignalHandler struct {
	daemon   *Daemon
	logger   *slog.Logger
	shutdown chan struct{}
	done     chan struct{}
}

// NewSignalHandler creates a new signal handler for the daemon.
func NewSignalHandler(d *Daemon, logger *slog.Logger) *SignalHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &SignalHandler{
		daemon:   d,
		logger:   logger,
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins listening for OS signals.
// - SIGTERM, SIGINT: Graceful shutdown
// - SIGHUP: Configuration reload
func (h *SignalHandler) Start(ctx context.Context) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	go func() {
		defer close(h.done)

		for {
			select {
			case <-ctx.Done():
				signal.Stop(sigChan)
				return

			case <-h.shutdown:
				signal.Stop(sigChan)
				return

			case sig := <-sigChan:
				switch sig {
				case syscall.SIGTERM, syscall.SIGINT:
					h.logger.Info("received shutdown signal", slog.String("signal", sig.String()))
					h.handleShutdown()
					return

				case syscall.SIGHUP:
					h.logger.Info("received reload signal")
					h.handleReload()
				}
			}
		}
	}()
}

// Stop stops the signal handler.
func (h *SignalHandler) Stop() {
	close(h.shutdown)
	<-h.done
}

// handleShutdown performs graceful shutdown.
func (h *SignalHandler) handleShutdown() {
	timeout := 30 * time.Second
	if h.daemon.cfg != nil && h.daemon.cfg.Supervision.ShutdownTimeout.Duration() > 0 {
		timeout = h.daemon.cfg.Supervision.ShutdownTimeout.Duration()
	}

	h.logger.Info("initiating graceful shutdown", slog.Duration("timeout", timeout))

	if err := h.daemon.Stop(timeout); err != nil {
		h.logger.Error("shutdown error", slog.String("error", err.Error()))
	}
}

// handleReload performs configuration reload.
func (h *SignalHandler) handleReload() {
	if err := h.daemon.Reload(); err != nil {
		h.logger.Error("reload error", slog.String("error", err.Error()))
	}
}

// GracefulShutdown is a helper that sets up signal handling and waits for shutdown.
// This is the main entry point for running the daemon with signal handling.
func GracefulShutdown(d *Daemon, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := NewSignalHandler(d, logger)
	handler.Start(ctx)

	// Start the daemon
	if err := d.Start(); err != nil {
		return err
	}

	// Wait for daemon to stop (either via signal or programmatic stop)
	d.Wait()

	return nil
}
