// Package main provides the entry point for the Vision daemon.
// Vision is a Go-native MCP server daemon that provides centralized
// management of MCP servers for AI agents.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
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
	}

	// Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Vision %s\n", version)
			fmt.Printf("  Commit:  %s\n", commit)
			fmt.Printf("  Built:   %s\n", buildTime)
		},
	}
	rootCmd.AddCommand(versionCmd)

	// Daemon commands (placeholder)
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon management commands",
	}
	daemonCmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the Vision daemon",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("TODO: Implement daemon start")
		},
	})
	daemonCmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the Vision daemon",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("TODO: Implement daemon stop")
		},
	})
	daemonCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("TODO: Implement daemon status")
		},
	})
	rootCmd.AddCommand(daemonCmd)

	// Server commands (placeholder)
	serverCmd := &cobra.Command{
		Use:   "server",
		Short: "Server management commands",
	}
	serverCmd.AddCommand(&cobra.Command{
		Use:   "add [name]",
		Short: "Add a server to the registry",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("TODO: Implement server add for %s\n", args[0])
		},
	})
	serverCmd.AddCommand(&cobra.Command{
		Use:   "remove [name]",
		Short: "Remove a server from the registry",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("TODO: Implement server remove for %s\n", args[0])
		},
	})
	serverCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all servers",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("TODO: Implement server list")
		},
	})
	rootCmd.AddCommand(serverCmd)

	// Init command (placeholder)
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize client configuration",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("TODO: Implement init")
		},
	}
	rootCmd.AddCommand(initCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
