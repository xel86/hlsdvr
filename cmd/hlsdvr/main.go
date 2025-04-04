package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path"
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

func HumanReadableBytes(b int) string {
	bf := float64(b)
	for _, unit := range []string{"", "Ki", "Mi", "Gi", "Ti"} {
		if math.Abs(bf) < 1024.0 {
			return fmt.Sprintf("%3.1f %sB", bf, unit)
		}
		bf /= 1024.0
	}
	return fmt.Sprintf("%.1f YiB", bf)
}

func HumanReadableSeconds(seconds int) string {
	duration := time.Duration(seconds) * time.Second

	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	secs := int(duration.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
	} else {
		return fmt.Sprintf("%02d:%02d", minutes, secs)
	}
}

func DigestInfoString(d hls.RecordingDigest) string {
	return fmt.Sprintf("[%s]: %s / %s (%dx%d @ %.2f)",
		path.Base(d.OutputPath),
		HumanReadableBytes(d.BytesWritten),
		HumanReadableSeconds(int(d.RecordingDuration)),
		d.Width, d.Height, d.FrameRate)
}

func MoveFile(sourcePath string, destDirPath string, destFileName string) error {
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't open source file: %v", err)
	}
	defer inputFile.Close()

	if err := os.MkdirAll(destDirPath, 0755); err != nil {
		return fmt.Errorf("error creating directory %s to move/archive stream(s) into: %v",
			destDirPath, err)
	}

	// don't open a file if it already exists.
	destPath := filepath.Join(destDirPath, destFileName)
	outputFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("Couldn't create dest file: %v", err)
	}
	defer outputFile.Close()

	_, err = io.Copy(outputFile, inputFile)
	if err != nil {
		return fmt.Errorf("Couldn't copy to dest from source: %v", err)
	}

	// for Windows, close before trying to remove: https://stackoverflow.com/a/64943554/246801
	inputFile.Close()

	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't remove source file: %v", err)
	}

	return nil
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
func downloadHLS(ctx context.Context,
	hlsStream hls.M3U8StreamVariant,
	s platform.Streamer, outputPath string) (*hls.RecordingDigest, error) {
	var returnErr error
	if err := os.MkdirAll(s.GetOutputDirPath(), 0755); err != nil {
		return nil, fmt.Errorf("error creating directory %s to download stream into: %v",
			s.GetOutputDirPath(), err)
	}

	recorder := hls.NewRecorder(
		ctx,
		s.Username(),
		&http.Client{Timeout: 10 * time.Second},
		hlsStream,
		outputPath)

	digest, err := recorder.Record()
	if err != nil {
		returnErr = fmt.Errorf("error recording %s hls stream: %v", s.Username(), err)
		if digest.BytesWritten == 0 {
			return nil, returnErr
		}
	}

	return &digest, nil
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
	recordingMap := make(map[string]struct{})
	recordingMutex := sync.Mutex{}

	// to lock the entire platform struct from use, mainly to be used when updating config.
	platformMutex := sync.Mutex{}

	// set timer to 1 so that the first loop iteration will start instantly.
	// reset and wait the platform check interval every time after this.
	timer := time.NewTimer(1)

loop:
	for {
		select {
		case <-ctx.Done():
			slog.Info(fmt.Sprintf("(%s) got shutdown signal, stopped monitoring.", p.Name()))
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
				}
			}
		case <-timer.C:
		}

		platformMutex.Lock()

		timer.Reset(time.Duration(p.GetCheckInterval()) * time.Second)

		liveStreamers, err := p.GetLiveStreamers()
		if err != nil {
			slog.Error(fmt.Sprintf("(%s) failed to get streamers who are live: %v", p.Name(), err))
			continue // TODO: do we want to just try forever or should we have a retry limit?
		}

		recordingMutex.Lock()
		for _, liveStreamer := range liveStreamers {
			_, found := recordingMap[liveStreamer.UniqueID()]
			if !found {
				hls, err := p.GetHLSStream(liveStreamer)
				if err != nil {
					slog.Error(
						fmt.Sprintf("(%s) error getting hls url for %s: %v",
							p.Name(), liveStreamer.Username(), err))
					continue
				}

				recordingMap[liveStreamer.UniqueID()] = struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					slog.Info(fmt.Sprintf("(%s) starting to record streamer %s", p.Name(), liveStreamer.Username()))

					// TODO: make the filename template configurable?
					fileName := fmt.Sprintf("%s-%s.ts", liveStreamer.Username(), time.Now().Format("20060102_150405"))
					outputPath := filepath.Join(liveStreamer.GetOutputDirPath(), fileName)

					digest, err := downloadHLS(ctx, hls, liveStreamer, outputPath)

					recordingMutex.Lock()
					delete(recordingMap, liveStreamer.UniqueID())
					recordingMutex.Unlock()

					if err != nil {
						if digest != nil {
							slog.Error(fmt.Sprintf("(%s) error downloading mid-stream for %s: %v",
								p.Name(), liveStreamer.Username(), err))
						} else {
							slog.Error(fmt.Sprintf("(%s) error starting stream download, no file made for %s: %v",
								p.Name(), liveStreamer.Username(), err))
							return
						}
					}

					if digest == nil {
						slog.Warn(fmt.Sprintf("(%s) %s stream download has no digest, and also no error...",
							p.Name(), liveStreamer.Username()))
						return
					}

					if digest.GracefulEnd {
						slog.Info(fmt.Sprintf("(%s) %s went offline %s",
							p.Name(), liveStreamer.Username(), DigestInfoString(*digest)))
					} else {
						slog.Info(fmt.Sprintf("(%s) recording for %s ended abruptly %s",
							p.Name(), liveStreamer.Username(), DigestInfoString(*digest)))
					}

					// If an archive dir path has been set in the config for either the top, platform, or streamer level,
					// move the finished downloaded stream file into the archive directory.
					if liveStreamer.GetArchiveDirPath() != nil {
						err := MoveFile(outputPath, *liveStreamer.GetArchiveDirPath(), fileName)
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
		}
		recordingMutex.Unlock()
		platformMutex.Unlock()
		slog.Debug(fmt.Sprintf("(%s) platform loop iteration...", p.Name()))
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
