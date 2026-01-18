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
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jrede/vision/internal/config"
	"github.com/jrede/vision/internal/daemon"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

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
  - Client config generation for Claude Code, OpenCode, etc.`,
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
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(serverCmd())
	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(healthCmd())
	rootCmd.AddCommand(initCmd())

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
				json.NewEncoder(os.Stdout).Encode(map[string]string{
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
	defer pidFile.Release()

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

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"running": running,
			"pid":     pid,
		})
		return nil
	}

	if running {
		fmt.Printf("Daemon is running (PID %d)\n", pid)

		// Try to get detailed status from API
		resp, err := http.Get(daemonAddr + "/health")
		if err == nil {
			defer resp.Body.Close()
			var health map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&health) == nil {
				fmt.Printf("  Status: %v\n", health["status"])
				fmt.Printf("  Uptime: %v\n", health["uptime"])
			}
		}
	} else {
		fmt.Println("Daemon is not running")
	}

	return nil
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

// --- Server Commands ---

func serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Short:   "Server management commands",
		Aliases: []string{"s"},
	}

	// server list
	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "List all servers",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return listServers()
		},
	}

	// server info
	infoCmd := &cobra.Command{
		Use:   "info [name]",
		Short: "Show server details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return showServerInfo(args[0])
		},
	}

	// server start
	startCmd := &cobra.Command{
		Use:   "start [name]",
		Short: "Start a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return serverAction(args[0], "start")
		},
	}

	// server stop
	stopCmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Stop a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return serverAction(args[0], "stop")
		},
	}

	// server restart
	restartCmd := &cobra.Command{
		Use:   "restart [name]",
		Short: "Restart a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return serverAction(args[0], "restart")
		},
	}

	cmd.AddCommand(listCmd, infoCmd, startCmd, stopCmd, restartCmd)
	return cmd
}

func listServers() error {
	resp, err := http.Get(daemonAddr + "/api/v1/servers")
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Success bool                     `json:"success"`
		Data    []map[string]interface{} `json:"data"`
		Error   string                   `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(result.Data)
		return nil
	}

	if len(result.Data) == 0 {
		fmt.Println("No servers registered")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tPORT\tTRANSPORT\tPID")
	for _, srv := range result.Data {
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%v\n",
			srv["name"],
			srv["state"],
			srv["port"],
			srv["transport"],
			srv["pid"],
		)
	}
	w.Flush()

	return nil
}

func showServerInfo(name string) error {
	resp, err := http.Get(daemonAddr + "/api/v1/servers/" + name)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
		Error   string                 `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(result.Data)
		return nil
	}

	fmt.Printf("Server: %s\n", name)
	for k, v := range result.Data {
		fmt.Printf("  %s: %v\n", k, v)
	}

	return nil
}

func serverAction(name, action string) error {
	resp, err := http.Post(daemonAddr+"/api/v1/servers/"+name+"/"+action, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("API error: %s", result.Error)
	}

	fmt.Printf("Server %s: %s successful\n", name, action)
	return nil
}

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
		json.NewEncoder(os.Stdout).Encode(cfg)
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
	defer resp.Body.Close()

	var health map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if jsonOutput {
		json.NewEncoder(os.Stdout).Encode(health)
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

// --- Init Command ---

func initCmd() *cobra.Command {
	var (
		global  bool
		servers []string
		client  string
		dryRun  bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize client configuration for a project",
		Long: `Initialize client configuration (e.g., .claude/settings.json) for a project.

By default, creates project-local configuration. Use --global for user-wide config.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return initClientConfig(global, servers, client, dryRun)
		},
	}

	cmd.Flags().BoolVar(&global, "global", false, "Create global (user-wide) configuration")
	cmd.Flags().StringSliceVar(&servers, "servers", nil, "Servers to include (default: all)")
	cmd.Flags().StringVar(&client, "client", "claude-code", "Client format: claude-code, opencode")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview configuration without writing")

	return cmd
}

func initClientConfig(global bool, servers []string, client string, dryRun bool) error {
	// Load Vision config to get server list
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load Vision config: %w", err)
	}

	// Filter servers if specified
	selectedServers := make(map[string]*config.ServerConfig)
	if len(servers) == 0 {
		selectedServers = cfg.Servers
	} else {
		for _, name := range servers {
			if srv, ok := cfg.Servers[name]; ok {
				selectedServers[name] = srv
			} else {
				return fmt.Errorf("server not found: %s", name)
			}
		}
	}

	// Generate client config
	var output map[string]interface{}
	switch client {
	case "claude-code":
		mcpServers := make(map[string]interface{})
		for name, srv := range selectedServers {
			mcpServers[name] = map[string]interface{}{
				"type": "streamable-http",
				"url":  fmt.Sprintf("http://localhost:%d/mcp", srv.Port),
			}
		}
		output = map[string]interface{}{
			"mcpServers": mcpServers,
		}

	case "opencode":
		mcpServers := make(map[string]interface{})
		for name, srv := range selectedServers {
			mcpServers[name] = map[string]interface{}{
				"type": "sse",
				"url":  fmt.Sprintf("http://localhost:%d/mcp", srv.Port),
			}
		}
		output = map[string]interface{}{
			"mcp": mcpServers,
		}

	default:
		return fmt.Errorf("unknown client: %s", client)
	}

	// Format output
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if dryRun {
		fmt.Println(string(data))
		return nil
	}

	// Determine output path
	var outputPath string
	switch client {
	case "claude-code":
		if global {
			outputPath = os.ExpandEnv("$HOME/.config/Claude/Claude.json")
		} else {
			outputPath = ".claude/settings.json"
		}
	case "opencode":
		if global {
			outputPath = os.ExpandEnv("$HOME/.opencode.json")
		} else {
			outputPath = ".opencode.json"
		}
	}

	// Create parent directory if needed
	if dir := outputPath[:len(outputPath)-len("/"+outputPath)]; dir != "" {
		os.MkdirAll(dir, 0755)
	}

	// Write file
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("Created %s configuration: %s\n", client, outputPath)
	return nil
}
