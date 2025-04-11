package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/xel86/hlsdvr/internal/hls"
	"github.com/xel86/hlsdvr/internal/platform"
	"github.com/xel86/hlsdvr/internal/platform/twitch"
	"github.com/xel86/hlsdvr/internal/server"
	"github.com/xel86/hlsdvr/internal/util"
)

func createPlatformsFromConfigs(cfg Config) ([]platform.Platform, error) {
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

func monitorPlatform(ctx context.Context, p platform.Platform, cmdMsgChan <-chan platform.CommandMsg) {
	sListStr := "[ "
	for _, s := range p.GetStreamers() {
		sListStr += (s.Username() + " ")
	}
	sListStr += "]"

	slog.Info(fmt.Sprintf("(%s) started monitoring: %s", p.Name(), sListStr))

	var wg sync.WaitGroup

	// string is from a platform.Streamer's UniqueID().
	recordingMap := make(map[string]*hls.Recorder)
	recordingMapMutex := sync.Mutex{}

	// to lock the entire platform struct from use, mainly to be used when updating config.
	platformMutex := sync.Mutex{}

	// set timer to 1 so that the first loop iteration will start instantly.
	// reset and wait the platform check interval every time after this.
	timer := time.NewTimer(1)

loop:
	for {
		select {
		case <-ctx.Done():
			slog.Info(fmt.Sprintf("(%s) got shutdown signal, stopping...", p.Name()))
			break loop
		case cmdMsg := <-cmdMsgChan:
			{
				switch cmdMsg.Type {
				case platform.CmdConfigReload:
					{
						// TODO: maybe put this in a proper function?
						func() {
							platformMutex.Lock()
							defer platformMutex.Unlock()

							cfg, ok := cmdMsg.Value.(Config)
							if !ok {
								slog.Error(fmt.Sprintf(
									"Config-Reload command message sent with a non-config value: %v",
									cmdMsg.Value))
								return
							}

							pCfg, found := cfg.PlatformCfgMap[p.Name()]
							if !found {
								// TODO: a previously configured platform has since been deleted from the config,
								//       what shall we do? for now just ignore but we should probably teardown platform.
								slog.Info(fmt.Sprintf(
									"Previously configured platform (%s) has been removed from config.",
									p.Name()))
								return
							}

							applied, err := p.UpdateConfig(pCfg)
							if err != nil {
								slog.Error(fmt.Sprintf(
									"Error updating platform (%s) config: %v",
									p.Name(), err))
								return
							}

							if applied {
								slog.Info(fmt.Sprintf("(%s) config successfully updated", p.Name()))
							} else {
								slog.Info(fmt.Sprintf(
									"(%s) config successfully updated, but no changes were applied", p.Name()))
							}
						}()
					}
				case platform.CmdStatus:
					{
						func() {
							recordingMapMutex.Lock()
							defer recordingMapMutex.Unlock()

							var digests []hls.RecordingDigest
							for _, v := range recordingMap {
								if v != nil {
									digests = append(digests, v.GetCurrentDigest())
								}
							}

							returnMsg := platform.CmdStatusReturn{
								PlatformName: p.Name(),
								Digests:      digests,
							}

							// ensure non-blocking send
							select {
							case cmdMsg.ReturnChan <- returnMsg:
							default:
								slog.Warn(fmt.Sprintf(
									"(%s) tried returning status, but the channel was unavailable. Dropped message: %v",
									p.Name(), returnMsg))
							}
						}()
					}
				}
				continue // go back to top waiting for select {} after handling command.
			}
		case <-timer.C:
		}

		// TODO: should we defer an Unlock() to avoid bugs with early exiting/continuing from this loop?
		platformMutex.Lock()

		timer.Reset(time.Duration(p.GetCheckInterval()) * time.Second)

		liveStreamers, err := p.GetLiveStreamers()
		if err != nil {
			slog.Error(fmt.Sprintf("(%s) failed to get streamers who are live: %v", p.Name(), err))
			platformMutex.Unlock()
			continue // TODO: do we want to just try forever or should we have a retry limit?
		}

		recordingMapMutex.Lock()
		for _, liveStreamer := range liveStreamers {
			_, found := recordingMap[liveStreamer.UniqueID()] // check if already recording live stream
			if found {
				continue
			}

			hlsStream, err := p.GetHLSStream(liveStreamer)
			if err != nil {
				slog.Error(
					fmt.Sprintf("(%s) error getting hls url for %s: %v",
						p.Name(), liveStreamer.Username(), err))
				continue
			}

			if err := os.MkdirAll(liveStreamer.GetOutputDirPath(), 0755); err != nil {
				slog.Error(fmt.Sprintf("error creating directory %s to download stream into: %v",
					liveStreamer.GetOutputDirPath(), err))
				continue
			}

			// TODO: make the filename template configurable?
			fileName := fmt.Sprintf("%s-%s.ts", liveStreamer.Username(), time.Now().Format("20060102_150405"))
			outputPath := filepath.Join(liveStreamer.GetOutputDirPath(), fileName)

			// TODO: probably want a secondary context to stop recording this specific stream,
			// 	     on top of the global context.
			recorder := hls.NewRecorder(
				ctx,
				liveStreamer.Username(),
				&http.Client{Timeout: 5 * time.Second},
				hlsStream,
				outputPath)

			recordingMap[liveStreamer.UniqueID()] = recorder

			wg.Add(1)
			go func() {
				defer wg.Done()
				slog.Info(fmt.Sprintf("(%s) starting to record streamer %s", p.Name(), liveStreamer.Username()))

				digest, err := recorder.Record()
				if err != nil {
					slog.Error(fmt.Sprintf(
						"(%s) error recording %s hls stream: %v", p.Name(), liveStreamer.Username(), err))
				}

				recordingMapMutex.Lock()
				delete(recordingMap, liveStreamer.UniqueID())
				recordingMapMutex.Unlock()

				if err != nil {
					if digest.BytesWritten != 0 {
						slog.Error(fmt.Sprintf("(%s) error downloading mid-stream for %s: %v",
							p.Name(), liveStreamer.Username(), err))
					} else {
						slog.Error(fmt.Sprintf("(%s) error starting stream download, no file made for %s: %v",
							p.Name(), liveStreamer.Username(), err))
						return
					}
				}

				if digest.GracefulEnd {
					slog.Info(fmt.Sprintf("(%s) %s went offline %s",
						p.Name(), liveStreamer.Username(), hls.DigestFileInfoString(digest)))
				} else {
					slog.Info(fmt.Sprintf("(%s) recording for %s ended abruptly %s",
						p.Name(), liveStreamer.Username(), hls.DigestFileInfoString(digest)))
				}

				// If an archive dir path has been set in the config for either the top, platform, or streamer level,
				// move the finished downloaded stream file into the archive directory.
				if liveStreamer.GetArchiveDirPath() != nil {
					err := util.MoveFile(outputPath, *liveStreamer.GetArchiveDirPath(), fileName)
					if err != nil {
						slog.Error(fmt.Sprintf("(%s) error moving recording %s into archive %s",
							p.Name(), fileName, *liveStreamer.GetArchiveDirPath()))
					} else {
						slog.Info(fmt.Sprintf("(%s) moved recording %s into %s",
							p.Name(), fileName, *liveStreamer.GetArchiveDirPath()))
					}
				}
			}()
		}

		recordingMapMutex.Unlock()
		platformMutex.Unlock()
		slog.Debug(fmt.Sprintf("(%s) platform loop iteration...", p.Name()))
	}

	wg.Wait()
	slog.Info(fmt.Sprintf("(%s) gracefully stopped monitoring.", p.Name()))
}

func monitorConfigFileChanges(ctx context.Context, cfgPath string, pcs *platform.CommandSender) {
	initial, err := os.Stat(cfgPath)
	if err != nil {
		slog.Error(fmt.Sprintf(
			"Error initially accessing config file (%s) to monitor for changes: %v", cfgPath, err))
		return
	}

	lastModTime := initial.ModTime()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			{
				currentInfo, err := os.Stat(cfgPath)
				if err != nil {
					slog.Warn(fmt.Sprintf("Error accessing config file (%s) to check for changes: %v", cfgPath, err))
					continue
				}

				currentModTime := currentInfo.ModTime()

				if currentModTime != lastModTime {
					lastModTime = currentModTime
					cfg, err := ReadConfig(cfgPath)
					if err != nil {
						slog.Error(fmt.Sprintf(
							"Error reading config (%s): %v. Ignoring and using current config...",
							cfgPath, err))
						continue
					}
					pcs.Broadcast(platform.CommandMsg{Type: platform.CmdConfigReload, Value: cfg})
					slog.Debug("Broadcasted CmdConfigReload Message for all platforms.")
				}
			}
		}
	}
}

func main() {
	var cfgPath string
	var socketPath string
	var noRpc bool
	var debug bool
	flag.StringVar(
		&cfgPath,
		"config",
		filepath.Join(GetDefaultConfigDir(), configFileName),
		"Path to config file to use or create.")
	flag.StringVar(
		&socketPath,
		"socket",
		"/tmp/hlsdvr.sock", // TODO: a working windows default?
		"Path to create the unix socket in for RPC server.")
	flag.BoolVar(
		&debug,
		"debug",
		false,
		"Enable debug log level for output")
	flag.BoolVar(
		&noRpc,
		"no-rpc",
		false,
		"Don't create or listen on a unix socket for RPC commands.")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Default is slog.LevelInfo
	if debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Info(fmt.Sprintf("Received signal: %v", sig))
		slog.Info("Shutting down... stopping all recordings and doing any post-recording processing.")
		cancel()
	}()

	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		err = GenerateDefaultExampleConfig(cfgPath)
		if err != nil {
			slog.Error(fmt.Sprintf("error generating default example config: %v", err))
			return
		}
		slog.Info(fmt.Sprintf("Generated default config to %s, edit it then rerun hlsdvr.", cfgPath))
		return
	}

	cfg, err := ReadConfig(cfgPath)
	if err != nil {
		slog.Error(fmt.Sprintf("Error reading config (%s): %v", cfgPath, err))
		return
	}

	// Override the config socket path if the socket flag was passed in.
	// If no config value is present, treat it as if RPC has been disabled.
	if !util.IsFlagPassed("socket") {
		if cfg.UnixSocketPath != nil {
			socketPath = *cfg.UnixSocketPath
		} else {
			noRpc = true
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
		monitorConfigFileChanges(ctx, cfgPath, pcs)
	}()

	if !noRpc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.RpcServer(ctx, pcs, socketPath)
		}()
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

			monitorPlatform(ctx, p, ch)
		}()
	}

	wg.Wait()
}
