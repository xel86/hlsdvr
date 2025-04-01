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
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/xel86/hlsdvr/hls"
	"github.com/xel86/hlsdvr/platform"
	"github.com/xel86/hlsdvr/platform/twitch"
)

type PlatformCommand int

const (
	CmdStatus PlatformCommand = iota
	CmdConfigReload
)

var platformCommandName = map[PlatformCommand]string{
	CmdStatus:       "status",
	CmdConfigReload: "config-reload",
}

type PlatformCommandMsg struct {
	Type  PlatformCommand
	Value any
}

type PlatformCommandSender struct {
	platformChans map[string]chan PlatformCommandMsg // platform.Name() key -> channel value
	mutex         sync.RWMutex
}

func NewPlatformCommandSender(platforms []platform.Platform) *PlatformCommandSender {
	chanMap := make(map[string]chan PlatformCommandMsg)
	for _, p := range platforms {
		pChan := make(chan PlatformCommandMsg, 5) // arbitrary channel buffer size of 5
		chanMap[p.Name()] = pChan
	}
	return &PlatformCommandSender{
		platformChans: chanMap,
		mutex:         sync.RWMutex{},
	}
}

func (pcs *PlatformCommandSender) Broadcast(msg PlatformCommandMsg) {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	for name, ch := range pcs.platformChans {
		// ensure non-blocking send
		select {
		case ch <- msg:
		default:
			slog.Warn(fmt.Sprintf(
				"Tried sending command to platform (%s), but the channel was full. Dropped message: %s",
				name, platformCommandName[msg.Type]))
		}
	}
}

func (pcs *PlatformCommandSender) BroadcastTo(platformNames []string, msg PlatformCommandMsg) {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	for name, ch := range pcs.platformChans {
		if !slices.Contains(platformNames, name) {
			continue
		}

		// ensure non-blocking send
		select {
		case ch <- msg:
		default:
			slog.Warn(fmt.Sprintf(
				"Tried sending command to platform (%s), but the channel was full. Dropped message: %s",
				name, platformCommandName[msg.Type]))
		}
	}
}

func (pcs *PlatformCommandSender) RemovePlatform(platformName string) {
	pcs.mutex.Lock()
	defer pcs.mutex.Unlock()
	ch, found := pcs.platformChans[platformName]
	if !found {
		slog.Warn(fmt.Sprintf("Tried to remove non-existent platform (%s) from command sender.", platformName))
		return
	}

	delete(pcs.platformChans, platformName)
	close(ch)
}

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

// TODO: probably want two contexts for the future, one for global context and one for streamer specific disabling.
func downloadHLS(ctx context.Context, url string, s platform.Streamer, platformName string) {
	slog.Info(fmt.Sprintf("Starting to record streamer %s (%s)", s.Username(), platformName))

	if err := os.MkdirAll(s.GetOutputDirPath(), 0755); err != nil {
		slog.Error(fmt.Sprintf("(%s) error creating directory %s to download stream into: %v",
			platformName, s.GetOutputDirPath(), err))
		return
	}

	// TODO: make the filename template configurable?
	fileName := fmt.Sprintf("%s-%s.ts", s.Username(), time.Now().Format("20060102_150405"))
	recorder := hls.NewRecorder(ctx, s.Username(),
		&http.Client{Timeout: 10 * time.Second}, url,
		filepath.Join(s.GetOutputDirPath(), fileName))

	report, err := recorder.Record()
	if err != nil {
		slog.Error(fmt.Sprintf("error recording %s hls stream: %v", s.Username(), err))
	}

	if report.GracefulEnd {
		slog.Info(fmt.Sprintf("(%s) %s went offline, recording finished.", platformName, s.Username()))
	} else {
		slog.Info(fmt.Sprintf("(%s) recording for %s ended abruptly.", platformName, s.Username()))
	}
}

func monitorPlatform(ctx context.Context, p platform.Platform, cmdMsgChan <-chan PlatformCommandMsg) {
	sListStr := "[ "
	for _, s := range p.GetStreamers() {
		sListStr += (s.Username() + " ")
	}
	sListStr += "]"

	slog.Info(fmt.Sprintf("(%s) started monitoring: %s", p.Name(), sListStr))

	var wg sync.WaitGroup

	// string is from a platform.Streamer's UniqueID().
	recordingMap := make(map[string]bool)
	recordingMutex := sync.Mutex{}

	// to lock the entire platform struct from use, mainly to be used when updating config.
	platformMutex := sync.Mutex{}

loop:
	for {
		slog.Debug(fmt.Sprintf("(%s) platform loop iteration...", p.Name()))
		if ctx.Err() != nil {
			slog.Info(fmt.Sprintf("(%s) got shutdown signal, stopped monitoring.", p.Name()))
			break loop
		}

		platformMutex.Lock()
		ticker := time.NewTicker(time.Duration(p.GetCheckInterval()) * time.Second)

		liveStreamers, err := p.GetLiveStreamers()
		if err != nil {
			slog.Error(fmt.Sprintf("Failed to get %s streamers who are live: %v", p.Name(), err))
			return
		}

		recordingMutex.Lock()
		for _, liveStreamer := range liveStreamers {
			_, found := recordingMap[liveStreamer.UniqueID()]
			if !found {
				hlsUrl, err := p.GetHLSUrl(liveStreamer)
				if err != nil {
					slog.Error(
						fmt.Sprintf("(%s) error getting hls url for %s: %v",
							p.Name(), liveStreamer.Username(), err))
					continue
				}

				recordingMap[liveStreamer.UniqueID()] = true
				wg.Add(1)
				go func() {
					defer wg.Done()
					downloadHLS(ctx, hlsUrl, liveStreamer, p.Name())

					recordingMutex.Lock()
					delete(recordingMap, liveStreamer.UniqueID())
					recordingMutex.Unlock()
				}()
			}
		}
		recordingMutex.Unlock()
		platformMutex.Unlock()

		select {
		case <-ctx.Done():
			break loop
		case cmdMsg := <-cmdMsgChan:
			{
				switch cmdMsg.Type {
				case CmdConfigReload:
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

							err := p.UpdateConfig(pCfg)
							if err != nil {
								slog.Error(fmt.Sprintf(
									"Error updating platform (%s) config: %v",
									p.Name(), err))
								return
							}

							sListStr := "[ "
							for _, s := range p.GetStreamers() {
								sListStr += (s.Username() + " ")
							}
							sListStr += "]"

							slog.Info(fmt.Sprintf("(%s) config updated, now monitoring: %s", p.Name(), sListStr))
						}()
					}
				}
			}
		case <-ticker.C:
			continue
		}
	}

	wg.Wait()
	slog.Info(fmt.Sprintf("(%s) stopped monitoring.", p.Name()))
}

func monitorConfigFileChanges(ctx context.Context, cfgPath string, pcs *PlatformCommandSender) {
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
					pcs.Broadcast(PlatformCommandMsg{Type: CmdConfigReload, Value: cfg})
					slog.Debug(fmt.Sprintf("Broadcasted CmdConfigReload Message for: %v", pcs.platformChans))
				}
			}
		}
	}
}

func main() {
	var cfgPath string
	var debug bool
	flag.StringVar(
		&cfgPath,
		"config",
		filepath.Join(GetDefaultConfigDir(), configFileName),
		"Path to config file to use or create.")
	flag.BoolVar(
		&debug,
		"debug",
		false,
		"Enable debug log level for output")

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

	platforms, err := createPlatformsFromConfigs(cfg)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to create initial platforms from config: %v", err))
		return
	}

	pcs := NewPlatformCommandSender(platforms)
	var wg sync.WaitGroup

	// Watch config file for changes
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitorConfigFileChanges(ctx, cfgPath, pcs)
	}()

	for _, p := range platforms {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ch, found := pcs.platformChans[p.Name()]
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
