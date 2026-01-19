// Package daemon provides the main Vision daemon orchestration.
// It coordinates the config, supervisor, registry, and HTTP servers.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jrede/vision/internal/admin"
	"github.com/jrede/vision/internal/api"
	"github.com/jrede/vision/internal/bridge"
	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/mcp"
	"github.com/jrede/vision/internal/server"
	"github.com/jrede/vision/internal/supervisor"
)

// Daemon is the main Vision daemon that coordinates all components.
type Daemon struct {
	cfg            *config.Config
	configPath     string
	managementPort int

	supervisor  *supervisor.Supervisor
	registry    *server.Registry
	portManager *mcp.PortManager
	apiServer   *api.Server
	adminServer *admin.Server // Admin MCP server on port 6275
	httpServer  *http.Server

	logger *slog.Logger

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// State
	mu      sync.RWMutex
	running bool
}

// Config configures the daemon.
type Config struct {
	ConfigPath     string // Path to servers.yaml
	ManagementPort int    // Port for management API (default: 6275)
	Logger         *slog.Logger
}

// New creates a new daemon instance.
func New(cfg Config) (*Daemon, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ManagementPort == 0 {
		cfg.ManagementPort = 6275
	}

	// Load configuration
	visionCfg, err := config.Load(cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Load instructions (optional - won't fail if file doesn't exist)
	instructions, err := config.LoadInstructions("")
	if err != nil {
		cfg.Logger.Warn("failed to load instructions", slog.String("error", err.Error()))
		instructions = &config.Instructions{
			Servers: make(map[string]*config.ServerInstructions),
			Tools:   make(map[string]*config.ToolInstructions),
		}
	} else if instructions.HasInstructions() {
		cfg.Logger.Info("loaded tool guidance instructions",
			slog.Int("servers", len(instructions.Servers)),
			slog.Int("tools", len(instructions.Tools)),
		)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Create supervisor
	sup := supervisor.New(visionCfg.Supervision, cfg.Logger)

	// Create registry
	reg := server.NewRegistry(sup, cfg.Logger)

	// Create port manager for MCP HTTP endpoints
	pm := mcp.NewPortManager(cfg.Logger)

	// Create API server (REST, for backward compatibility - will be deprecated)
	apiSrv := api.NewServer(api.ServerConfig{
		Registry:       reg,
		Config:         visionCfg,
		Logger:         cfg.Logger,
		AllowedOrigins: []string{"*"},
	})

	// Create Admin MCP server (primary management interface)
	adminSrv := admin.NewServer(admin.Config{
		Registry:     reg,
		Instructions: instructions,
		Port:         admin.DefaultPort, // 6275
		Logger:       cfg.Logger,
	})

	return &Daemon{
		cfg:            visionCfg,
		configPath:     cfg.ConfigPath,
		managementPort: cfg.ManagementPort,
		supervisor:     sup,
		registry:       reg,
		portManager:    pm,
		apiServer:      apiSrv,
		adminServer:    adminSrv,
		logger:         cfg.Logger,
		ctx:            ctx,
		cancel:         cancel,
	}, nil
}

// Start starts the daemon.
func (d *Daemon) Start() error {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return errors.New("daemon already running")
	}
	d.running = true
	d.mu.Unlock()

	d.logger.Info("starting Vision daemon")

	// Load servers from config
	if err := d.registry.LoadFromConfig(d.cfg); err != nil {
		return fmt.Errorf("failed to load servers: %w", err)
	}

	// Start supervisor (runs in background)
	d.supervisor.ServeBackground(d.ctx)

	// Start servers with autostart enabled
	if err := d.registry.StartAll(d.ctx); err != nil {
		d.logger.Warn("some servers failed to start", slog.String("error", err.Error()))
	}

	// Start Admin MCP server (port 6275)
	if d.adminServer != nil {
		if err := d.adminServer.Start(d.ctx); err != nil {
			d.logger.Error("failed to start admin MCP server", slog.String("error", err.Error()))
		}
	}

	// Give servers a moment to initialize their stdio pipes
	time.Sleep(500 * time.Millisecond)

	// Set up HTTP proxies for stdio servers
	if err := d.setupHTTPProxies(); err != nil {
		d.logger.Warn("some HTTP proxies failed to start", slog.String("error", err.Error()))
	}

	// Start management HTTP server (REST API - for backward compatibility)
	// Note: In future, this will be removed in favor of Admin MCP
	d.wg.Add(1)
	go d.runHTTPServer()

	d.logger.Info("Vision daemon started",
		slog.Int("server_count", d.registry.Count()),
	)

	return nil
}

// Stop gracefully stops the daemon.
func (d *Daemon) Stop(timeout time.Duration) error {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return nil
	}
	d.running = false
	d.mu.Unlock()

	d.logger.Info("stopping Vision daemon")

	// Create timeout context for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Stop Admin MCP server
	if d.adminServer != nil {
		if err := d.adminServer.Stop(ctx); err != nil {
			d.logger.Warn("admin MCP server shutdown error", slog.String("error", err.Error()))
		}
	}

	// Stop HTTP server (REST API)
	if d.httpServer != nil {
		if err := d.httpServer.Shutdown(ctx); err != nil {
			d.logger.Warn("HTTP server shutdown error", slog.String("error", err.Error()))
		}
	}

	// Stop all MCP port listeners
	if err := d.portManager.Close(); err != nil {
		d.logger.Warn("port manager close error", slog.String("error", err.Error()))
	}

	// Stop all servers
	if err := d.registry.StopAll(ctx); err != nil {
		d.logger.Warn("some servers failed to stop", slog.String("error", err.Error()))
	}

	// Cancel supervisor context (stops it)
	// Note: suture.Supervisor stops when context is cancelled

	// Cancel daemon context
	d.cancel()

	// Wait for all goroutines
	d.wg.Wait()

	d.logger.Info("Vision daemon stopped")
	return nil
}

// Wait blocks until the daemon stops.
func (d *Daemon) Wait() {
	<-d.ctx.Done()
	d.wg.Wait()
}

// IsRunning returns whether the daemon is running.
func (d *Daemon) IsRunning() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.running
}

// Reload reloads the configuration from disk.
// It starts new servers, stops removed servers, and updates changed servers.
func (d *Daemon) Reload() error {
	d.mu.RLock()
	if !d.running {
		d.mu.RUnlock()
		return errors.New("daemon not running")
	}
	d.mu.RUnlock()

	d.logger.Info("reloading configuration")

	// Load new config
	newCfg, err := config.Load(d.configPath)
	if err != nil {
		d.logger.Error("failed to reload config", slog.String("error", err.Error()))
		return fmt.Errorf("failed to reload config: %w", err)
	}

	// Get current server names
	currentNames := make(map[string]bool)
	for _, srv := range d.registry.List() {
		currentNames[srv.Name] = true
	}

	// Get new server names
	newNames := make(map[string]bool)
	for name := range newCfg.Servers {
		newNames[name] = true
	}

	// Find servers to add, remove, and update
	var toAdd, toRemove, toUpdate []string

	for name := range newCfg.Servers {
		if !currentNames[name] {
			toAdd = append(toAdd, name)
		} else {
			// Check if config changed
			toUpdate = append(toUpdate, name)
		}
	}

	for name := range currentNames {
		if !newNames[name] {
			toRemove = append(toRemove, name)
		}
	}

	// Remove deleted servers
	for _, name := range toRemove {
		d.logger.Info("removing server", slog.String("name", name))
		if err := d.registry.Stop(name); err != nil {
			d.logger.Warn("failed to stop server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
		}
		if err := d.registry.Remove(name); err != nil {
			d.logger.Warn("failed to remove server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
		}
	}

	// Add new servers
	for _, name := range toAdd {
		d.logger.Info("adding server", slog.String("name", name))
		cfg := newCfg.Servers[name]
		if err := d.registry.Add(name, cfg); err != nil {
			d.logger.Warn("failed to add server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			continue
		}
		if cfg.Autostart {
			if err := d.registry.Start(name); err != nil {
				d.logger.Warn("failed to start server",
					slog.String("name", name),
					slog.String("error", err.Error()),
				)
			}
		}
	}

	// Update config reference
	d.mu.Lock()
	d.cfg = newCfg
	d.mu.Unlock()

	d.logger.Info("configuration reloaded",
		slog.Int("added", len(toAdd)),
		slog.Int("removed", len(toRemove)),
		slog.Int("updated", len(toUpdate)),
	)

	return nil
}

// Status returns the current daemon status.
func (d *Daemon) Status() DaemonStatus {
	d.mu.RLock()
	running := d.running
	d.mu.RUnlock()

	var registryStatus server.RegistryStatus
	if d.registry != nil {
		registryStatus = d.registry.Status()
	}

	return DaemonStatus{
		Running:    running,
		ConfigPath: d.configPath,
		Registry:   registryStatus,
	}
}

// DaemonStatus contains daemon state information.
type DaemonStatus struct {
	Running    bool                  `json:"running"`
	ConfigPath string                `json:"config_path"`
	Registry   server.RegistryStatus `json:"registry"`
}

// setupHTTPProxies creates HTTP bridges for all running stdio servers.
// Each stdio server gets an HTTP endpoint on its configured port.
func (d *Daemon) setupHTTPProxies() error {
	var errs []error

	for _, srv := range d.registry.List() {
		// Skip servers that aren't running
		if srv.State != server.StateRunning {
			continue
		}

		// Skip HTTP transport servers (they don't need a bridge)
		if srv.Config.InferTransport() != config.TransportStdio {
			d.logger.Debug("skipping non-stdio server",
				slog.String("server", srv.Name),
				slog.String("transport", string(srv.Config.InferTransport())),
			)
			continue
		}

		// Get the managed process
		proc := srv.Process
		if proc == nil {
			d.logger.Warn("no process for running server",
				slog.String("server", srv.Name),
			)
			continue
		}

		// Get stdin/stdout pipes
		stdin := proc.Stdin()
		stdout := proc.Stdout()
		if stdin == nil || stdout == nil {
			d.logger.Warn("missing stdio pipes for server",
				slog.String("server", srv.Name),
			)
			continue
		}

		// Create the bridge
		b := bridge.NewStdioHTTPBridge(stdin, stdout, &bridge.BridgeOptions{
			Logger: d.logger.With(slog.String("server", srv.Name)),
		})
		b.Start()

		// Add to port manager (starts HTTP listener on the server's port)
		if err := d.portManager.Add(srv.Name, srv.Config.Port, b); err != nil {
			d.logger.Warn("failed to add HTTP proxy",
				slog.String("server", srv.Name),
				slog.Int("port", srv.Config.Port),
				slog.String("error", err.Error()),
			)
			errs = append(errs, err)
			continue
		}

		d.logger.Info("HTTP proxy started",
			slog.String("server", srv.Name),
			slog.Int("port", srv.Config.Port),
		)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// runHTTPServer runs the management HTTP server.
func (d *Daemon) runHTTPServer() {
	defer d.wg.Done()

	addr := fmt.Sprintf(":%d", d.managementPort)

	d.httpServer = &http.Server{
		Addr:    addr,
		Handler: d.apiServer.Handler(),
	}

	d.logger.Info("starting management API", slog.String("addr", addr))

	if err := d.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		d.logger.Error("HTTP server error", slog.String("error", err.Error()))
	}
}

// Registry returns the server registry.
func (d *Daemon) Registry() *server.Registry {
	return d.registry
}

// Config returns the current configuration.
func (d *Daemon) Config() *config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg
}
