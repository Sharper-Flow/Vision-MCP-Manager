// Package daemon provides the main Vision daemon orchestration.
// It coordinates the config, supervisor, registry, and HTTP servers.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/catalog"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/mcp"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/ownership"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/reachability"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/server"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/slots"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/supervisor"
)

// Daemon is the main Vision daemon that coordinates all components.
type Daemon struct {
	cfg        *config.Config
	configPath string

	supervisor  *supervisor.Supervisor
	registry    *server.Registry
	portManager *mcp.PortManager
	adminServer *admin.Server // Admin MCP server on port 6275
	catalog     *catalog.Catalog

	reachabilityStore *reachability.Store
	probeManager      *reachability.Manager

	suggestionProvider mcp.FallbackSuggestionProvider

	logger *slog.Logger

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// State
	mu      sync.RWMutex
	running bool

	// Proxy setup deduplication (prevents race between handleServerEvent and setupHTTPProxies)
	proxySetupInProgress sync.Map

	// Per-server session metrics, keyed by server name.
	serverMetrics   map[string]*metrics.ServerMetrics
	serverMetricsMu sync.RWMutex

	managedGateways                  map[string]*mcp.ManagedHTTPGateway
	managedBackends                  map[string]*supervisor.BackendCoordinator
	managedCancels                   map[string]context.CancelFunc
	managedHTTPDrainWarningThreshold time.Duration
	managedGatewaysMu                sync.RWMutex
	ownershipStore                   *ownership.Store
	reconciler                       ownershipReconciler
	daemonID                         string
}

type ownershipReconciler interface {
	Reconcile(context.Context, map[string]ownership.ServerIdentity) []ownership.Result
	ReconcileOne(context.Context, string, ownership.ServerIdentity) (ownership.Result, bool)
}

// Config configures the daemon.
type Config struct {
	ConfigPath     string // Path to servers.yaml
	ManagementPort int    // Port for management API (default: 6275)
	Logger         *slog.Logger
	OwnershipRoot  string
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

	env := make(map[string]string)
	for _, value := range os.Environ() {
		if key, val, ok := strings.Cut(value, "="); ok {
			env[key] = val
		}
	}
	root, err := ownership.RuntimeRoot(cfg.OwnershipRoot, env, os.Getuid(), os.TempDir())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("resolve ownership root: %w", err)
	}
	store, err := ownership.NewStore(root)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create ownership store: %w", err)
	}
	daemonID, err := ownership.GenerateDaemonID()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("generate daemon identity: %w", err)
	}
	reader := ownership.NewLinuxProcReader("")
	reconciler := &ownership.Reconciler{Store: store, ProcReader: reader, Signaler: ownership.NewSignaler(), TermTimeout: visionCfg.Supervision.ShutdownTimeout.Duration(), KillTimeout: visionCfg.Supervision.ShutdownTimeout.Duration()}
	// Create supervisor
	sup := supervisor.NewWithOptions(visionCfg.Supervision, cfg.Logger, supervisor.WithLeaseStore(store, daemonID))

	// Create registry
	reg := server.NewRegistry(sup, cfg.Logger)

	// Create port manager for MCP HTTP endpoints
	pm := mcp.NewPortManager(cfg.Logger)
	cat := catalog.Default()
	suggestionProvider := newCatalogSuggestionProvider(cat, reg)

	// Create Admin MCP server (primary management interface)
	adminSrv := admin.NewServer(admin.Config{
		Registry:     reg,
		Catalog:      cat,
		Instructions: instructions,
		DaemonConfig: visionCfg,
		ConfigPath:   cfg.ConfigPath,
		Port:         cfg.ManagementPort,
		Logger:       cfg.Logger,
	})

	d := &Daemon{
		cfg:                              visionCfg,
		configPath:                       cfg.ConfigPath,
		supervisor:                       sup,
		registry:                         reg,
		portManager:                      pm,
		adminServer:                      adminSrv,
		catalog:                          cat,
		suggestionProvider:               suggestionProvider,
		logger:                           cfg.Logger,
		ctx:                              ctx,
		cancel:                           cancel,
		serverMetrics:                    make(map[string]*metrics.ServerMetrics),
		managedGateways:                  make(map[string]*mcp.ManagedHTTPGateway),
		managedBackends:                  make(map[string]*supervisor.BackendCoordinator),
		managedCancels:                   make(map[string]context.CancelFunc),
		managedHTTPDrainWarningThreshold: defaultManagedHTTPDrainWarningThreshold,
		ownershipStore:                   store,
		reconciler:                       reconciler,
		daemonID:                         daemonID,
		reachabilityStore:                reachability.NewStore(),
	}
	d.probeManager = reachability.NewManager(d.reachabilityStore, reachability.NewVersionSelector(mcp.NewListenerProbe(), mcp.NewEndToEndProbe()), cfg.Logger)

	// Give the port manager the store so a listener that fails to bind records
	// listener-depth evidence instead of silently reporting running.
	pm.SetReachabilityStore(d.reachabilityStore)
	reg.SetStartupGuard(d.reconcileManagedBackend)

	// Register event handler for dynamic server lifecycle management
	reg.SetEventHandler(d.handleServerEvent)

	// Wire per-server metrics accessor into admin server.
	adminSrv.SetServerMetricsAccessor(d)
	adminSrv.SetSessionLifecycleAccessor(d)
	adminSrv.SetReachabilityStore(d.reachabilityStore)

	return d, nil
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
	expected := make(map[string]ownership.ServerIdentity)
	for name, serverCfg := range d.cfg.Servers {
		if serverCfg.InferTransport() == config.TransportManagedHTTP {
			expected[name] = ownership.ServerIdentity{Name: name, Command: serverCfg.Command, Args: serverCfg.Args, URL: serverCfg.URL, Transport: string(serverCfg.InferTransport()), Env: serverCfg.Env}
		}
	}
	for _, result := range d.reconciler.Reconcile(d.ctx, expected) {
		if _, configured := expected[result.ServerName]; !configured {
			d.logger.Warn("unmatched ownership lease", slog.String("name", result.ServerName))
			continue
		}
		switch result.Status {
		case "reclaimed", "removed_dead":
		default:
			d.registry.BlockStartup(result.ServerName, errors.New("managed backend ownership conflict"))
		}
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

	// Set up HTTP proxies for stdio servers
	if err := d.setupHTTPProxies(); err != nil {
		d.logger.Warn("some HTTP proxies failed to start", slog.String("error", err.Error()))
	}
	if err := d.setupSlotGroupProxies(); err != nil {
		d.logger.Warn("some slot group proxies failed to start", slog.String("error", err.Error()))
	}

	// NOTE: Legacy REST API removed - use Admin MCP on port 6275 instead

	d.logger.Info("Vision daemon started",
		slog.Int("server_count", d.registry.Count()),
	)

	return nil
}

func (d *Daemon) reconcileManagedBackend(name string) error {
	cfg := d.cfg.Servers[name]
	if cfg == nil || cfg.InferTransport() != config.TransportManagedHTTP {
		return nil
	}
	identity := ownership.ServerIdentity{Name: name, Command: cfg.Command, Args: cfg.Args, URL: cfg.URL, Transport: string(cfg.InferTransport()), Env: cfg.Env}
	result, found := d.reconciler.ReconcileOne(d.ctx, name, identity)
	if !found || result.Status == "reclaimed" || result.Status == "removed_dead" {
		return nil
	}
	return errors.New("managed backend ownership conflict")
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

	// Stop all MCP port listeners
	if err := d.portManager.Close(); err != nil {
		d.logger.Warn("port manager close error", slog.String("error", err.Error()))
	}

	// Stop all servers
	if err := d.registry.StopAll(ctx); err != nil {
		d.logger.Warn("some servers failed to stop", slog.String("error", err.Error()))
	}

	// Drain remaining lifecycle events and stop the registry's delivery
	// goroutine. Ordered after StopAll so the Stop events it just produced are
	// still delivered to the handler rather than dropped at teardown.
	d.registry.Close()

	// Cancel supervisor context (stops it)
	// Note: suture.Supervisor stops when context is cancelled

	// Cancel daemon context
	d.cancel()
	if d.probeManager != nil {
		d.probeManager.Close()
	}

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
			current := d.registry.Get(name)
			if current != nil && !reflect.DeepEqual(current.Config, newCfg.Servers[name]) {
				toUpdate = append(toUpdate, name)
			}
		}
	}

	for name := range currentNames {
		if !newNames[name] {
			toRemove = append(toRemove, name)
		}
	}

	// Remove deleted servers
	var reloadErrs []error

	for _, name := range toRemove {
		d.logger.Info("removing server", slog.String("name", name))
		if err := d.registry.Stop(name); err != nil {
			d.logger.Warn("failed to stop server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("stop %s: %w", name, err))
		}
		if err := d.registry.Remove(name); err != nil {
			d.logger.Warn("failed to remove server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("remove %s: %w", name, err))
		} else {
			d.deleteServerMetrics(name)
		}
	}

	// Update config reference BEFORE adding servers so that event handlers
	// (e.g., setupProxyForServer reading d.cfg.Security) see the new config.
	d.mu.RLock()
	oldCfg := d.cfg
	d.mu.RUnlock()
	d.mu.Lock()
	d.cfg = newCfg
	d.mu.Unlock()

	// Add new servers
	for _, name := range toAdd {
		d.logger.Info("adding server", slog.String("name", name))
		cfg := newCfg.Servers[name]
		if err := d.registry.Add(name, cfg); err != nil {
			d.logger.Warn("failed to add server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("add %s: %w", name, err))
			continue
		}
		if cfg.Autostart {
			if err := d.registry.Start(name); err != nil {
				d.logger.Warn("failed to start server",
					slog.String("name", name),
					slog.String("error", err.Error()),
				)
				reloadErrs = append(reloadErrs, fmt.Errorf("start %s: %w", name, err))
				// Rollback: remove the added-but-not-started server to keep
				// the registry consistent. Ignore remove errors since the
				// server may be in a transitional state.
				if rmErr := d.registry.Remove(name); rmErr != nil {
					d.logger.Debug("rollback remove failed",
						slog.String("name", name),
						slog.String("error", rmErr.Error()),
					)
				}
			}
		}
	}

	// Recreate updated servers so new config applies to the registry and any
	// future proxy/session-manager wiring.
	for _, name := range toUpdate {
		d.logger.Info("updating server", slog.String("name", name))

		existing := d.registry.Get(name)
		wasRunning := existing != nil && (existing.State == server.StateRunning || existing.State == server.StateStarting)

		if err := d.registry.Stop(name); err != nil {
			d.logger.Warn("failed to stop server for update",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("update-stop %s: %w", name, err))
			continue
		}
		if err := d.registry.Remove(name); err != nil {
			d.logger.Warn("failed to remove server for update",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("update-remove %s: %w", name, err))
			continue
		}

		cfg := newCfg.Servers[name]
		if err := d.registry.Add(name, cfg); err != nil {
			d.logger.Warn("failed to re-add updated server",
				slog.String("name", name),
				slog.String("error", err.Error()),
			)
			reloadErrs = append(reloadErrs, fmt.Errorf("update-add %s: %w", name, err))
			continue
		}
		if wasRunning || cfg.Autostart {
			if err := d.registry.Start(name); err != nil {
				d.logger.Warn("failed to restart updated server",
					slog.String("name", name),
					slog.String("error", err.Error()),
				)
				reloadErrs = append(reloadErrs, fmt.Errorf("update-start %s: %w", name, err))
			}
		}
	}

	if err := d.syncSlotGroupProxies(oldCfg, newCfg); err != nil {
		reloadErrs = append(reloadErrs, fmt.Errorf("slot-group-sync: %w", err))
	}
	if err := d.setupSlotGroupProxies(); err != nil {
		reloadErrs = append(reloadErrs, fmt.Errorf("slot-group-setup: %w", err))
	}

	d.logger.Info("configuration reloaded",
		slog.Int("added", len(toAdd)),
		slog.Int("removed", len(toRemove)),
		slog.Int("updated", len(toUpdate)),
	)

	return errors.Join(reloadErrs...)
}

func (d *Daemon) syncSlotGroupProxies(oldCfg, newCfg *config.Config) error {
	if d.portManager == nil {
		return nil
	}
	var errs []error
	oldGroups := map[string]*config.SlotGroupConfig{}
	newGroups := map[string]*config.SlotGroupConfig{}
	if oldCfg != nil {
		oldGroups = oldCfg.SlotGroups
	}
	if newCfg != nil {
		newGroups = newCfg.SlotGroups
	}

	for name, oldGroup := range oldGroups {
		newGroup, exists := newGroups[name]
		if exists && reflect.DeepEqual(oldGroup, newGroup) {
			continue
		}
		listenerName := fmt.Sprintf("slot_group:%s", name)
		if d.portManager.Get(listenerName) == nil {
			continue
		}
		if err := d.portManager.Remove(listenerName); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", listenerName, err))
		}
	}

	return errors.Join(errs...)
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

// setupHTTPProxies creates streamable HTTP proxies for all running stdio servers.
// Each stdio server gets a per-session proxy on its configured port.
func (d *Daemon) setupHTTPProxies() error {
	var errs []error

	for _, srv := range d.registry.List() {
		if err := d.setupProxyForServer(srv); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func slotServerHealthy(srv *server.ManagedServer, value reachability.Reachability, grace time.Duration) bool {
	if srv == nil || srv.State != server.StateRunning {
		return false
	}

	switch value.State {
	case reachability.StateReachable, reachability.StateProbing:
		return true
	case reachability.StateUnreachable:
		return false
	case reachability.StateUnprobed:
		return srv.Uptime() <= grace
	default:
		return false
	}
}

func (d *Daemon) setupSlotGroupProxies() error {
	d.mu.RLock()
	cfg := d.cfg
	d.mu.RUnlock()
	if cfg == nil || len(cfg.SlotGroups) == 0 {
		return nil
	}

	var errs []error
	for groupName, group := range cfg.SlotGroups {
		listenerName := fmt.Sprintf("slot_group:%s", groupName)

		entries := make([]slots.Entry, 0, group.Count)
		var representative *config.ServerConfig
		for serverName, serverCfg := range cfg.Servers {
			if serverCfg == nil || serverCfg.SlotGroup != groupName {
				continue
			}
			if representative == nil {
				representative = serverCfg
			}
			listener := d.portManager.Get(serverName)
			if listener == nil {
				continue
			}
			mgr, ok := listener.SessionManager.(*session.Manager)
			if !ok || mgr == nil {
				continue
			}
			nameCopy := serverName
			entries = append(entries, slots.Entry{
				SlotName: serverName,
				Index:    serverCfg.SlotIndex,
				Manager:  mgr,
				Healthy: func() bool {
					if d.registry == nil {
						return true
					}
					srv := d.registry.Get(nameCopy)
					if d.reachabilityStore == nil {
						return srv != nil && srv.State == server.StateRunning
					}
					value, _ := d.reachabilityStore.Get(nameCopy)
					return slotServerHealthy(srv, value, admin.DefaultReachabilityGrace)
				},
			})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
		if len(entries) == 0 || representative == nil {
			continue
		}
		healthCheckInterval := representative.HealthCheckInterval.Duration()
		if healthCheckInterval <= 0 {
			healthCheckInterval = 30 * time.Second
		}
		requestTimeout := representative.RequestTimeout.Duration()
		if requestTimeout <= 0 {
			requestTimeout = 30 * time.Second
		}
		retryCfg := mcp.RetryConfig{}
		if representative.Retry != nil {
			retryCfg = mcp.RetryConfig{
				MaxAttempts:     representative.Retry.MaxAttempts,
				InitialDelay:    representative.Retry.InitialDelay.Duration(),
				MaxDelay:        representative.Retry.MaxDelay.Duration(),
				RetryableErrors: append([]string(nil), representative.Retry.RetryableErrors...),
			}
		}
		cbCfg := mcp.CircuitBreakerConfig{}
		if representative.CircuitBreaker != nil {
			cbCfg = mcp.CircuitBreakerConfig{
				FailureThreshold: representative.CircuitBreaker.FailureThreshold,
				RecoveryTimeout:  representative.CircuitBreaker.RecoveryTimeout.Duration(),
			}
		}
		if listener := d.portManager.Get(listenerName); listener != nil {
			if selector, ok := listener.SessionManager.(*slots.Multiplexer); ok && selector != nil {
				selector.ReplaceEntries(entries)
				continue
			}
			continue
		}

		selector := slots.NewMultiplexer(groupName, d.logger, entries)
		handler := mcp.NewProxyHandler(mcp.ProxyConfig{
			ServerName:            groupName,
			Selector:              selector,
			Logger:                d.logger,
			ReachabilityStore:     d.reachabilityStore,
			SuggestionProvider:    d.suggestionProvider,
			HealthCheckInterval:   healthCheckInterval,
			RequestTimeout:        requestTimeout,
			SharedReadOnlyTools:   append([]string(nil), representative.SharedReadOnlyTools...),
			SharedResultCacheTTL:  representative.SharedResultCacheTTL.Duration(),
			SharedResultCacheSize: representative.SharedResultCacheSize,
			MaxInFlightRequests:   representative.MaxInFlightRequests,
			RetryConfig:           retryCfg,
			CircuitBreakerConfig:  cbCfg,
		})

		secCfg := mcp.SecurityConfig{ListenerExposure: mcp.ListenerLoopback}
		if cfg != nil {
			secCfg.BearerToken = cfg.Security.BearerToken
			secCfg.AllowedOrigins = cfg.Security.AllowedOrigins
		}
		if err := d.portManager.AddStreamable(listenerName, group.GroupPort, handler, selector, secCfg); err != nil {
			errs = append(errs, fmt.Errorf("slot group %s: %w", groupName, err))
			continue
		}
	}

	return errors.Join(errs...)
}

// handleServerEvent processes server lifecycle events from the registry.
// This is called asynchronously when servers start or stop.
func (d *Daemon) handleServerEvent(event server.ServerEvent) {
	switch event.Type {
	case server.EventServerStarted:
		if err := d.setupProxyForServer(event.Server); err != nil {
			d.logger.Warn("failed to setup proxy for server",
				slog.String("server", event.Name),
				slog.String("error", err.Error()),
			)
		}
		if err := d.setupSlotGroupProxies(); err != nil {
			d.logger.Warn("failed to setup slot group proxies",
				slog.String("server", event.Name),
				slog.String("error", err.Error()),
			)
		}
		d.startProbeWorker(event.Server, reachability.DefaultProbeInterval)
	case server.EventServerStopped:
		d.removeProbeWorker(event.Name)
		d.teardownProxyForServer(event.Name)
	}
}

func (d *Daemon) startProbeWorker(srv *server.ManagedServer, interval time.Duration) {
	if d.probeManager == nil || srv == nil || srv.Config == nil || srv.Config.Port <= 0 {
		return
	}
	err := d.probeManager.Start(d.ctx, reachability.Target{
		Name:            srv.Name,
		Port:            srv.Config.Port,
		ProtocolVersion: reachability.ProtocolVersion2025_11_25,
		BearerToken:     d.cfg.Security.BearerToken,
	}, interval)
	if err != nil {
		d.logger.Warn("failed to start reachability probe worker",
			slog.String("server", srv.Name),
			slog.String("error", err.Error()),
		)
	}
}

func (d *Daemon) removeProbeWorker(name string) {
	if d.probeManager == nil {
		return
	}
	if err := d.probeManager.Remove(name); err != nil {
		d.logger.Warn("failed to stop reachability probe worker",
			slog.String("server", name),
			slog.String("error", err.Error()),
		)
	}
}

// setupProxyForServer creates a StreamableHTTPHandler proxy for a single server.
// For stateful servers (Stateful: true), each upstream session gets its own
// isolated downstream subprocess via session.Manager.
// For stateless servers (Stateful: false, default), all upstream sessions
// share a single downstream subprocess via SharedSessionManager.
func (d *Daemon) setupProxyForServer(srv *server.ManagedServer) error {
	if srv == nil {
		return errors.New("server is nil")
	}

	// Skip servers that aren't running
	if srv.State != server.StateRunning {
		return nil
	}

	transport := srv.Config.InferTransport()
	if transport == config.TransportManagedHTTP {
		return d.setupManagedHTTPProxy(srv)
	}

	// Externally owned HTTP transports do not need a Vision proxy.
	if transport != config.TransportStdio {
		d.logger.Debug("skipping non-stdio server",
			slog.String("server", srv.Name),
			slog.String("transport", string(srv.Config.InferTransport())),
		)
		return nil
	}

	// A listener is expected here only when an earlier setup completed. The
	// setup marker is written after AddStreamable succeeds, so a listener with
	// no marker is stale or was installed by an unexpected owner.
	if listener := d.portManager.Get(srv.Name); listener != nil {
		d.serverMetricsMu.RLock()
		configured := d.serverMetrics[srv.Name] != nil
		d.serverMetricsMu.RUnlock()
		if configured {
			d.logger.Debug("proxy already exists for server",
				slog.String("server", srv.Name),
			)
			return nil
		}
		err := fmt.Errorf("unexpected pre-existing proxy listener: server=%s listener_port=%d configured_port=%d", srv.Name, listener.Port, srv.Config.Port)
		d.recordListenerSetupFailure(srv.Name, err)
		return err
	}

	// Check if we're already setting up this server (deduplication)
	if _, loaded := d.proxySetupInProgress.LoadOrStore(srv.Name, true); loaded {
		d.logger.Debug("proxy setup already in progress for server",
			slog.String("server", srv.Name),
		)
		return nil
	}
	defer d.proxySetupInProgress.Delete(srv.Name)

	// Build proxy config common to both modes
	proxyCfg := mcp.ProxyConfig{
		ServerName:            srv.Name,
		Logger:                d.logger,
		ReachabilityStore:     d.reachabilityStore,
		SuggestionProvider:    d.suggestionProvider,
		HealthCheckInterval:   srv.Config.HealthCheckInterval.Duration(),
		RequestTimeout:        srv.Config.RequestTimeout.Duration(),
		SharedReadOnlyTools:   append([]string(nil), srv.Config.SharedReadOnlyTools...),
		SharedResultCacheTTL:  srv.Config.SharedResultCacheTTL.Duration(),
		SharedResultCacheSize: srv.Config.SharedResultCacheSize,
		MaxInFlightRequests:   srv.Config.MaxInFlightRequests,
		RetryConfig: mcp.RetryConfig{
			MaxAttempts:     srv.Config.Retry.MaxAttempts,
			InitialDelay:    srv.Config.Retry.InitialDelay.Duration(),
			MaxDelay:        srv.Config.Retry.MaxDelay.Duration(),
			RetryableErrors: append([]string(nil), srv.Config.Retry.RetryableErrors...),
		},
		CircuitBreakerConfig: mcp.CircuitBreakerConfig{
			FailureThreshold: srv.Config.CircuitBreaker.FailureThreshold,
			RecoveryTimeout:  srv.Config.CircuitBreaker.RecoveryTimeout.Duration(),
		},
	}

	// Per-server metrics for session observability.
	srvMetrics := metrics.NewServerMetrics()
	proxyCfg.Metrics = srvMetrics

	var closer mcp.SessionCloser

	if srv.Config.Stateful {
		// Stateful mode: per-session subprocess isolation (existing behavior)
		mgr := session.NewManager(srv.Name, srv.Config, d.logger)

		// Start the session reaper for idle/TTL cleanup.
		idleTimeout := srv.Config.SessionTimeout.Duration()
		sessionTTL := srv.Config.SessionTTL.Duration()
		if idleTimeout > 0 || sessionTTL > 0 {
			shortest := idleTimeout
			if sessionTTL > 0 && (shortest == 0 || sessionTTL < shortest) {
				shortest = sessionTTL
			}
			checkInterval := shortest / 2
			if checkInterval < time.Second {
				checkInterval = time.Second
			}
			if checkInterval > 30*time.Second {
				checkInterval = 30 * time.Second
			}
			mgr.StartReaper(d.ctx, checkInterval)
		}

		proxyCfg.SessionManager = mgr
		closer = mgr
	} else if srv.Config.SlotGroup != "" {
		// Slot group members always use per-session isolation.
		// The Multiplexer requires *session.Manager instances for load balancing.
		mgr := session.NewManager(srv.Name, srv.Config, d.logger)

		idleTimeout := srv.Config.SessionTimeout.Duration()
		sessionTTL := srv.Config.SessionTTL.Duration()
		if idleTimeout > 0 || sessionTTL > 0 {
			shortest := idleTimeout
			if sessionTTL > 0 && (shortest == 0 || sessionTTL < shortest) {
				shortest = sessionTTL
			}
			checkInterval := shortest / 2
			if checkInterval < time.Second {
				checkInterval = time.Second
			}
			if checkInterval > 30*time.Second {
				checkInterval = 30 * time.Second
			}
			mgr.StartReaper(d.ctx, checkInterval)
		}

		proxyCfg.SessionManager = mgr
		closer = mgr

		d.logger.Debug("slot group member uses per-session mode",
			slog.String("server", srv.Name),
			slog.String("slot_group", srv.Config.SlotGroup),
		)
	} else {
		// Shared mode: single subprocess shared across all upstream sessions
		idleTimeout := srv.Config.ResolvedIdleReapTimeout()
		sharedMgr := session.NewSharedSessionManager(srv.Name, srv.Config, d.logger, idleTimeout, srvMetrics)
		sharedMgr.StartHealthProbe(d.ctx)

		proxyCfg.SharedManager = sharedMgr
		proxyCfg.DisconnectGracePeriod = srv.Config.ResolvedDisconnectGracePeriod()
		closer = sharedMgr

		d.logger.Info("using shared subprocess mode for server",
			slog.String("server", srv.Name),
			slog.Duration("disconnect_grace_period", proxyCfg.DisconnectGracePeriod),
			slog.Duration("idle_reap_timeout", idleTimeout),
		)
	}

	// Create the streamable proxy handler
	handler := mcp.NewProxyHandler(proxyCfg)

	// Build security config from daemon-wide settings (read under lock).
	d.mu.RLock()
	secCfg := mcp.SecurityConfig{
		BearerToken:      d.cfg.Security.BearerToken,
		AllowedOrigins:   d.cfg.Security.AllowedOrigins,
		ListenerExposure: mcp.ListenerLoopback,
	}
	d.mu.RUnlock()

	// Add to port manager with session manager for cleanup and security middleware.
	if err := d.portManager.AddStreamable(srv.Name, srv.Config.Port, handler, closer, secCfg); err != nil {
		setupErr := fmt.Errorf("failed to add streamable proxy: %w", err)
		d.recordListenerSetupFailure(srv.Name, setupErr)
		return setupErr
	}
	d.serverMetricsMu.Lock()
	d.serverMetrics[srv.Name] = srvMetrics
	d.serverMetricsMu.Unlock()

	d.logger.Info("streamable proxy started",
		slog.String("server", srv.Name),
		slog.Int("port", srv.Config.Port),
		slog.Bool("stateful", srv.Config.Stateful),
	)
	if srv.Config != nil && srv.Config.SlotGroup != "" {
		if err := d.setupSlotGroupProxies(); err != nil {
			d.logger.Warn("failed to refresh slot group proxies after slot proxy setup",
				slog.String("server", srv.Name),
				slog.String("error", err.Error()),
			)
		}
	}

	return nil
}

func (d *Daemon) setupManagedHTTPProxy(srv *server.ManagedServer) error {
	if listener := d.portManager.Get(srv.Name); listener != nil {
		d.managedGatewaysMu.RLock()
		configured := d.managedGateways[srv.Name] != nil
		d.managedGatewaysMu.RUnlock()
		if configured {
			d.logger.Debug("managed HTTP proxy already exists for server",
				slog.String("server", srv.Name),
			)
			return nil
		}
		err := fmt.Errorf("unexpected pre-existing managed HTTP listener: server=%s listener_port=%d configured_port=%d", srv.Name, listener.Port, srv.Config.Port)
		d.recordListenerSetupFailure(srv.Name, err)
		return err
	}
	if _, loaded := d.proxySetupInProgress.LoadOrStore(srv.Name, true); loaded {
		return nil
	}
	defer d.proxySetupInProgress.Delete(srv.Name)

	target, err := url.Parse(srv.Config.URL)
	if err != nil {
		setupErr := fmt.Errorf("parse managed HTTP target: %w", err)
		d.recordListenerSetupFailure(srv.Name, setupErr)
		return setupErr
	}
	coordinator := supervisor.NewBackendCoordinator()
	coordinator.MarkProbing()
	srvMetrics := metrics.NewServerMetrics()
	process := srv.Process
	if process == nil {
		setupErr := errors.New("managed HTTP server has no supervised process")
		d.recordListenerSetupFailure(srv.Name, setupErr)
		return setupErr
	}
	monitorCtx, monitorCancel := context.WithCancel(d.ctx)
	success := false
	defer func() {
		if !success {
			monitorCancel()
		}
	}()
	var gateway *mcp.ManagedHTTPGateway
	gateway, err = mcp.NewManagedHTTPGateway(mcp.ManagedHTTPGatewayConfig{
		Target:                target,
		MaxSessions:           srv.Config.MaxSessions,
		IdleTimeout:           srv.Config.SessionTimeout.Duration(),
		DisconnectGracePeriod: srv.Config.ResolvedDisconnectGracePeriod(),
		HungRequestBound:      10 * srv.Config.RequestTimeout.Duration(),
		Backend:               coordinator,
		OnAmbiguousFailure: func(cause error) {
			go d.recycleManagedHTTPBackend(monitorCtx, srv.Name, process, coordinator, cause)
		},
		Metrics: srvMetrics,
		Logger:  d.logger.With(slog.String("server", srv.Name)),
	})
	if err != nil {
		setupErr := fmt.Errorf("create managed HTTP gateway: %w", err)
		d.recordListenerSetupFailure(srv.Name, setupErr)
		return setupErr
	}

	d.mu.RLock()
	secCfg := mcp.SecurityConfig{
		BearerToken:      d.cfg.Security.BearerToken,
		AllowedOrigins:   d.cfg.Security.AllowedOrigins,
		ListenerExposure: mcp.ListenerLoopback,
	}
	d.mu.RUnlock()
	if err := d.portManager.AddStreamable(srv.Name, srv.Config.Port, gateway, gateway, secCfg); err != nil {
		setupErr := fmt.Errorf("add managed HTTP listener: %w", err)
		d.recordListenerSetupFailure(srv.Name, setupErr)
		return setupErr
	}
	reapInterval := managedHTTPReapInterval(
		srv.Config.SessionTimeout.Duration(),
		srv.Config.ResolvedDisconnectGracePeriod(),
	)
	gateway.StartReaper(monitorCtx, reapInterval)
	d.managedGatewaysMu.Lock()
	d.managedGateways[srv.Name] = gateway
	d.managedBackends[srv.Name] = coordinator
	d.managedCancels[srv.Name] = monitorCancel
	success = true
	d.managedGatewaysMu.Unlock()
	d.serverMetricsMu.Lock()
	d.serverMetrics[srv.Name] = srvMetrics
	d.serverMetricsMu.Unlock()

	d.wg.Add(1)
	go d.monitorManagedHTTPBackend(monitorCtx, srv.Name, process, target, gateway, coordinator)
	return nil
}

// recordListenerSetupFailure turns a hard proxy setup failure into immediate
// unreachable evidence. RecordProbe intentionally tolerates transient probe
// failures, but setup cannot have a transient listener state: without a
// successfully registered listener the running server is not reachable.
func (d *Daemon) recordListenerSetupFailure(name string, err error) {
	if d.reachabilityStore == nil {
		return
	}
	d.reachabilityStore.RecordDefinitiveFailure(
		name,
		reachability.DepthListener,
		time.Now(),
		err.Error(),
	)
}

func managedHTTPReapInterval(sessionTimeout, disconnectGracePeriod time.Duration) time.Duration {
	reapInterval := sessionTimeout / 2
	if reapInterval <= 0 || reapInterval > 30*time.Second {
		reapInterval = 30 * time.Second
	}
	if disconnectGracePeriod > 0 && disconnectGracePeriod/2 < reapInterval {
		reapInterval = disconnectGracePeriod / 2
	}
	if reapInterval < time.Second {
		return time.Second
	}
	return reapInterval
}

const defaultManagedHTTPDrainWarningThreshold = 30 * time.Second

func (d *Daemon) managedHTTPDrainWarningThresholdDuration() time.Duration {
	if d.managedHTTPDrainWarningThreshold > 0 {
		return d.managedHTTPDrainWarningThreshold
	}
	return defaultManagedHTTPDrainWarningThreshold
}

func (d *Daemon) recycleManagedHTTPBackend(ctx context.Context, name string, process *supervisor.ManagedProcess, coordinator *supervisor.BackendCoordinator, cause error) {
	slowDrainWarning := time.AfterFunc(d.managedHTTPDrainWarningThresholdDuration(), func() {
		if coordinator.State() != supervisor.BackendDraining {
			return
		}
		d.logger.Warn("managed HTTP recycle drain is slow",
			slog.String("server", name), slog.Int("in_flight", coordinator.InFlight()))
	})
	defer slowDrainWarning.Stop()

	err := coordinator.DrainAndRecycle(ctx, func() error {
		if process == nil {
			return supervisor.ErrBackendUnavailable
		}
		return process.RequestRestart()
	})
	if err != nil && !errors.Is(err, supervisor.ErrBackendUnavailable) && !errors.Is(err, context.Canceled) {
		d.logger.Error("managed HTTP recycle failed",
			slog.String("server", name), slog.String("cause", cause.Error()), slog.String("error", err.Error()))
	}
}

func (d *Daemon) monitorManagedHTTPBackend(ctx context.Context, name string, process *supervisor.ManagedProcess, target *url.URL, gateway *mcp.ManagedHTTPGateway, coordinator *supervisor.BackendCoordinator) {
	defer d.wg.Done()
	if process == nil {
		coordinator.MarkProcessLost()
		return
	}
	var attemptedGeneration uint64
	var activeGeneration uint64
	for {
		state, generation := process.LifecycleSnapshot()
		switch state {
		case supervisor.StateRunning:
			if generation != 0 && generation != attemptedGeneration {
				if activeGeneration != 0 && generation != activeGeneration {
					coordinator.MarkProcessLost()
					gateway.InvalidateAll(metrics.ReapReasonProcessLost)
					activeGeneration = 0
				}
				attemptedGeneration = generation
				coordinator.MarkProbing()
				probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := waitForManagedHTTPBackend(probeCtx, target)
				if err == nil {
					err = mcp.ProbeManagedHTTPBackend(probeCtx, target, nil)
				}
				cancel()
				if err == nil {
					coordinator.MarkReady()
					activeGeneration = generation
					d.logger.Info("managed HTTP gateway ready",
						slog.String("server", name), slog.Uint64("generation", generation))
				} else {
					coordinator.MarkProcessLost()
					gateway.InvalidateAll(metrics.ReapReasonProcessLost)
					d.logger.Error("managed HTTP readiness probe failed; requesting restart",
						slog.String("server", name), slog.String("error", err.Error()))
					_ = process.RequestRestart()
				}
			}
		case supervisor.StateCrashed, supervisor.StateStopped, supervisor.StateFailed:
			coordinator.MarkProcessLost()
			gateway.InvalidateAll(metrics.ReapReasonProcessLost)
			activeGeneration = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-process.LifecycleEvents():
		}
	}
}

func waitForManagedHTTPBackend(ctx context.Context, target *url.URL) error {
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := dialer.DialContext(ctx, "tcp", target.Host)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w", target.Host, ctx.Err())
		case <-ticker.C:
		}
	}
}

// teardownProxyForServer removes the HTTP proxy for a server.
func (d *Daemon) teardownProxyForServer(name string) {
	if err := d.portManager.Remove(name); err != nil {
		d.logger.Debug("failed to remove proxy for server",
			slog.String("server", name),
			slog.String("error", err.Error()),
		)
	} else {
		d.logger.Info("HTTP proxy removed",
			slog.String("server", name),
		)
	}
	d.managedGatewaysMu.Lock()
	if cancel := d.managedCancels[name]; cancel != nil {
		cancel()
	}
	delete(d.managedGateways, name)
	delete(d.managedBackends, name)
	delete(d.managedCancels, name)
	d.managedGatewaysMu.Unlock()
	d.deleteServerMetrics(name)
}

func (d *Daemon) deleteServerMetrics(name string) {
	d.serverMetricsMu.Lock()
	delete(d.serverMetrics, name)
	d.serverMetricsMu.Unlock()
}

// Registry returns the server registry.
func (d *Daemon) Registry() *server.Registry {
	return d.registry
}

// Reachability returns the probe-backed reachability store used by status
// surfaces. The store is independent from managed-server lifecycle state.
func (d *Daemon) Reachability() *reachability.Store {
	return d.reachabilityStore
}

// Config returns the current configuration.
func (d *Daemon) Config() *config.Config {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg
}

// ServerMetricsSnapshot returns per-server session metrics for the named server.
// Implements admin.ServerMetricsAccessor.
func (d *Daemon) ServerMetricsSnapshot(serverName string) *metrics.ServerMetricsSnapshot {
	d.serverMetricsMu.RLock()
	m, ok := d.serverMetrics[serverName]
	d.serverMetricsMu.RUnlock()
	if !ok || m == nil {
		return nil
	}
	snap := m.Snapshot()
	return &snap
}

// SessionLifecycleSnapshot returns the bounded secret-safe managed HTTP
// lifecycle projection used by both admin MCP and /v1 status surfaces.
func (d *Daemon) SessionLifecycleSnapshot(serverName string) *admin.SessionLifecycleSnapshot {
	d.managedGatewaysMu.RLock()
	gateway := d.managedGateways[serverName]
	backend := d.managedBackends[serverName]
	d.managedGatewaysMu.RUnlock()
	if gateway == nil || backend == nil {
		return nil
	}
	snapshot := gateway.Snapshot(100)
	result := &admin.SessionLifecycleSnapshot{
		BackendState:  string(backend.State()),
		CapacityUsed:  snapshot.CapacityUsed,
		CapacityMax:   snapshot.CapacityMax,
		Sessions:      make([]admin.SessionLifecycleRow, 0, len(snapshot.Rows)),
		Closed:        make([]admin.SessionLifecycleRow, 0, len(snapshot.Closed)),
		Omitted:       snapshot.Omitted,
		ClosedOmitted: snapshot.ClosedOmitted,
	}
	for _, row := range snapshot.Rows {
		result.Sessions = append(result.Sessions, lifecycleRow(row))
	}
	for _, row := range snapshot.Closed {
		result.Closed = append(result.Closed, lifecycleRow(row))
	}
	return result
}

func lifecycleRow(row mcp.LeaseSnapshotRow) admin.SessionLifecycleRow {
	return admin.SessionLifecycleRow{
		SafeID:                 row.SafeID,
		State:                  string(row.State),
		AgeSeconds:             int64(row.Age.Seconds()),
		ApplicationIdleSeconds: int64(row.Idle.Seconds()),
		InFlight:               row.InFlight,
		SSEConnections:         row.SSEConnections,
		LifecycleReason:        row.LifecycleReason,
	}
}
