package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"

	"github.com/xel86/hlsdvr/internal/config"
	"github.com/xel86/hlsdvr/internal/monitor"
	"github.com/xel86/hlsdvr/internal/platform"
	"github.com/xel86/hlsdvr/internal/platform/twitch"
	"github.com/xel86/hlsdvr/internal/server"
	"github.com/xel86/hlsdvr/internal/util"
)

func createPlatformsFromConfigs(cfg config.Config) ([]platform.Platform, error) {
	var platforms []platform.Platform
	if cfg.TwitchConfig != nil {
		twitchPlatform, err := twitch.NewPlatform(*cfg.TwitchConfig)
		if err != nil {
			return nil, fmt.Errorf("Error creating twitch platform: %v", err)
		}
		platforms = append(platforms, twitchPlatform)
	}

	return platforms, nil
}

func main() {
	var cfgPath string
	var socketPath string
	var noRpc bool
	var logDebug bool
	var showVersion bool
	var showHelp bool
	flag.StringVar(
		&cfgPath,
		"config",
		filepath.Join(util.GetDefaultConfigDir(config.ConfigDirName), config.ConfigFileName),
		"Path to config file to use or create.")
	flag.StringVar(
		&socketPath,
		"socket",
		util.GetDefaultSocketPath(server.SocketFileName),
		"Path to create the unix socket in for RPC server.")
	flag.BoolVar(
		&logDebug,
		"debug",
		false,
		"Enable debug log level for output")
	flag.BoolVar(
		&noRpc,
		"no-rpc",
		false,
		"Don't create or listen on a unix socket for RPC commands.")
	flag.BoolVar(&showHelp, "help", false, "Show help message")
	flag.BoolVar(&showHelp, "h", false, "Show help message (shorthand)")
	flag.BoolVar(&showVersion, "version", false, "Show build version")
	flag.BoolVar(&showVersion, "v", false, "Show build version (shorthand)")

	flag.Parse()

	if showHelp {
		fmt.Println("Usage: hlsdvr [options]")
		fmt.Println("\nOptions:")
		flag.PrintDefaults()
		return
	}

	if showVersion {
		build, ok := debug.ReadBuildInfo()
		if ok {
			fmt.Printf("hlsdvr: %v\n", build.Main.Version)
		} else {
			fmt.Printf("hlsdvr: v1\n")
		}
		return
	}

	// Default is slog.LevelInfo
	if logDebug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Info(fmt.Sprintf("Received signal: %v", sig))
		slog.Info("Shutting down... stopping all recordings and doing any post-recording processing.")
		cancel()
	}()

	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		err = config.GenerateDefaultExampleConfig(cfgPath)
		if err != nil {
			slog.Error(fmt.Sprintf("error generating default example config: %v", err))
			return
		}
		slog.Info(fmt.Sprintf("Generated default config to %s, edit it then rerun hlsdvr.", cfgPath))
		return
	}

	cfg, err := config.ReadConfig(cfgPath)
	if err != nil {
		slog.Error(fmt.Sprintf("Error reading config (%s): %v", cfgPath, err))
		return
	}

	// Override the config socket path if the socket flag was passed in.
	if !util.IsFlagPassed("socket") {
		if cfg.UnixSocketPath != nil {
			socketPath = *cfg.UnixSocketPath
		}
	}

	platforms, err := createPlatformsFromConfigs(cfg)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to create initial platforms from config: %v", err))
		return
	}

	pcs := platform.NewCommandSender(platforms)
	var wg sync.WaitGroup

	// Watch config file for changes
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitor.ConfigFileChanges(ctx, cfgPath, pcs)
	}()

	if !noRpc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := server.RpcServer(ctx, pcs, socketPath)
			if err != nil {
				slog.Warn(fmt.Sprintf("RPC Server didn't gracefully exit: %v", err))
			}
		}()
	} else {
		slog.Info("Not starting RPC server due to -no-rpc flag.")
	}

	for _, p := range platforms {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ch, found := pcs.GetPlatformChan(p.Name())
			if !found {
				slog.Error(fmt.Sprintf(
					"Failed to get platform (%s) command channel to start monitoring, skipping platform.",
					p.Name()))
				return
			}

			pm := monitor.NewPlatformMonitor(ctx, p, ch)
			pm.StartMonitor()
		}()
	}

	wg.Wait()
}
