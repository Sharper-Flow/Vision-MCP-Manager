// Package main provides the entry point for the Vision daemon.
// Vision is a Go-native MCP server daemon that provides centralized
// management of MCP servers for AI agents.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/admin"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/daemon"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func init() {
	// Propagate build-time version metadata to the admin package so
	// GET /version reflects the running binary's actual version.
	admin.Version = version
	admin.BuildInfo = commit + " " + buildTime
}

// Global flags
var (
	configPath string
	daemonAddr string
	jsonOutput bool
	verbose    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "vision",
		Short: "Vision - Go-native MCP Server Daemon",
		Long: `Vision is a centralized MCP server daemon that manages stdio-based
MCP servers and exposes them via HTTP endpoints.

It provides:
  - Centralized server registry (~/.config/vision/servers.yaml)
  - Stdio-to-HTTP bridging with session management
  - Erlang-style process supervision with automatic restarts
  - Per-port isolation for security (no path-based routing)
  - OpenCode config generation through the Admin MCP`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if verbose {
				slog.SetLogLoggerLevel(slog.LevelDebug)
			}
		},
	}

	// Global flags
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "Path to config file (default: ~/.config/vision/servers.yaml)")
	rootCmd.PersistentFlags().StringVar(&daemonAddr, "daemon-addr", "http://localhost:6275", "Daemon management API address")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")

	// Add commands
	// Core daemon lifecycle commands (keep)
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(healthCmd())

	// Config debugging commands (keep for operators)
	rootCmd.AddCommand(configCmd())

	// NOTE: Server management commands (server, init) have been removed.
	// Use the Admin MCP tools (vision_list, vision_add, vision_remove, etc.)
	// through the OpenCode plugin or direct MCP calls to port 6275.

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// --- Version Command ---

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			if jsonOutput {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
					"version": version,
					"commit":  commit,
					"built":   buildTime,
				})
				return
			}
			fmt.Printf("Vision %s\n", version)
			fmt.Printf("  Commit:  %s\n", commit)
			fmt.Printf("  Built:   %s\n", buildTime)
		},
	}
}

// --- Daemon Commands ---

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "daemon",
		Short:   "Daemon management commands",
		Aliases: []string{"d"},
	}

	// daemon start (foreground)
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Vision daemon (foreground)",
		Long:  "Start the Vision daemon in the foreground. Use Ctrl+C to stop.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon()
		},
	}

	// daemon stop
	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running Vision daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopDaemon()
		},
	}

	// daemon status
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showDaemonStatus()
		},
	}

	// daemon reload
	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Reload daemon configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return reloadDaemon()
		},
	}

	cmd.AddCommand(startCmd, stopCmd, statusCmd, reloadCmd)
	return cmd
}

func runDaemon() error {
	logger := slog.Default()
	if verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	// Check PID file
	pidFile := daemon.NewPIDFile("")
	pidFile.AcquireOrFail()
	defer func() { _ = pidFile.Release() }()

	// Create daemon
	d, err := daemon.New(daemon.Config{
		ConfigPath: configPath,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	// Run with signal handling
	return daemon.GracefulShutdown(d, logger)
}

func stopDaemon() error {
	pidFile := daemon.NewPIDFile("")
	running, pid := pidFile.IsRunning()
	if !running {
		fmt.Println("Daemon is not running")
		return nil
	}

	// Send SIGTERM to the daemon
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send signal: %w", err)
	}

	fmt.Printf("Sent SIGTERM to daemon (PID %d)\n", pid)

	// Wait for it to stop
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		running, _ = pidFile.IsRunning()
		if !running {
			fmt.Println("Daemon stopped")
			return nil
		}
	}

	fmt.Println("Daemon did not stop in time, consider kill -9")
	return nil
}

func showDaemonStatus() error {
	pidFile := daemon.NewPIDFile("")
	running, pid := pidFile.IsRunning()
	var health map[string]interface{}
	if running {
		resp, err := http.Get(daemonAddr + "/health")
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				_ = json.NewDecoder(resp.Body).Decode(&health)
			}
		}
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(daemonStatusPayload(running, pid, health))
		return nil
	}

	if running {
		fmt.Printf("Daemon is running (PID %d)\n", pid)
		if health != nil {
			fmt.Printf("  Status: %v\n", health["status"])
			// Presence-guarded so version skew (new CLI, old daemon whose
			// /health lacks the field) renders no line at all rather than
			// the fabricated "Uptime: <nil>".
			if up, ok := health["uptime"]; ok && up != nil {
				fmt.Printf("  Uptime: %v\n", up)
			}
		}
	} else {
		fmt.Println("Daemon is not running")
	}

	return nil
}

func daemonStatusPayload(running bool, pid int, health map[string]interface{}) map[string]interface{} {
	payload := map[string]interface{}{
		"running": running,
		"pid":     pid,
	}
	if health == nil {
		return payload
	}
	if status, ok := health["status"]; ok {
		payload["status"] = status
	}
	if uptime, ok := health["uptime"].(string); ok && uptime != "" {
		payload["uptime"] = uptime
	}
	return payload
}

func reloadDaemon() error {
	pidFile := daemon.NewPIDFile("")
	running, pid := pidFile.IsRunning()
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	if err := process.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("failed to send signal: %w", err)
	}

	fmt.Printf("Sent SIGHUP to daemon (PID %d)\n", pid)
	return nil
}

// NOTE: Server commands (serverCmd, listServers, showServerInfo, serverAction)
// have been removed. Server management is now handled via Admin MCP tools.
// Use vision_list, vision_add, vision_remove, etc. through the OpenCode plugin
// or direct MCP calls to port 6275.

// --- Config Commands ---

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration management",
	}

	// config show
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show current configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return showConfig()
		},
	}

	// config validate
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return validateConfig()
		},
	}

	// config init
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Create initial configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return initConfig()
		},
	}

	cmd.AddCommand(showCmd, validateCmd, initCmd)
	return cmd
}

func showConfig() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(cfg)
		return nil
	}

	fmt.Printf("Configuration: %s\n\n", config.DefaultConfigPath())
	fmt.Printf("Servers (%d):\n", len(cfg.Servers))
	for name, srv := range cfg.Servers {
		fmt.Printf("  %s:\n", name)
		fmt.Printf("    Port: %d\n", srv.Port)
		fmt.Printf("    Transport: %s\n", srv.InferTransport())
		if srv.Command != "" {
			fmt.Printf("    Command: %s\n", srv.Command)
		}
		if srv.URL != "" {
			fmt.Printf("    URL: %s\n", srv.URL)
		}
		fmt.Printf("    Autostart: %v\n", srv.Autostart)
	}

	return nil
}

func validateConfig() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration invalid: %v\n", err)
		os.Exit(1)
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration invalid: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Configuration is valid")
	return nil
}

func initConfig() error {
	path := configPath
	if path == "" {
		path = config.DefaultConfigPath()
	}

	// Check if file exists
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("configuration file already exists: %s", path)
	}

	// Create example config
	cfg := &config.Config{
		Servers: map[string]*config.ServerConfig{
			"time": {
				Port:      6276,
				Command:   "npx",
				Args:      []string{"-y", "@anthropic/mcp-time"},
				Autostart: true,
			},
		},
		Supervision: config.SupervisionConfig{},
	}

	if err := config.Save(cfg, path); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Created configuration file: %s\n", path)
	return nil
}

// --- Health Command ---

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check daemon health",
		RunE: func(cmd *cobra.Command, args []string) error {
			return checkHealth()
		},
	}
}

func checkHealth() error {
	resp, err := http.Get(daemonAddr + "/health")
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var health map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(health)
		return nil
	}

	fmt.Printf("Status: %v\n", health["status"])
	fmt.Printf("Uptime: %v\n", health["uptime"])

	if registry, ok := health["registry"].(map[string]interface{}); ok {
		fmt.Printf("Servers:\n")
		fmt.Printf("  Total: %v\n", registry["total_servers"])
		fmt.Printf("  Running: %v\n", registry["running_servers"])
		fmt.Printf("  Stopped: %v\n", registry["stopped_servers"])
		fmt.Printf("  Failed: %v\n", registry["failed_servers"])
	}

	return nil
}

// NOTE: Init command (initCmd, initClientConfig) has been removed.
// Client configuration generation is now handled via Admin MCP tool vision_init.
// Use the OpenCode plugin or direct MCP calls to port 6275.
