package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xel86/hlsdvr/internal/config"
	"github.com/xel86/hlsdvr/internal/hls"
	"github.com/xel86/hlsdvr/internal/platform"
	"github.com/xel86/hlsdvr/internal/util"
)

type PlatformMonitor struct {
	platform platform.Platform
	ctx      context.Context

	// Receives command messages from a platform command sender
	cmdMsgChan <-chan platform.CommandMsg

	// string is from a platform.Streamer's UniqueID().
	recordingMap      map[string]*hls.Recorder
	recordingMapMutex sync.Mutex

	stats      platform.HistoricalStats
	statsMutex sync.RWMutex
}

func NewPlatformMonitor(ctx context.Context,
	p platform.Platform, cmdMsgChan <-chan platform.CommandMsg) *PlatformMonitor {
	recordingMap := make(map[string]*hls.Recorder)
	return &PlatformMonitor{
		platform:     p,
		ctx:          ctx,
		cmdMsgChan:   cmdMsgChan,
		recordingMap: recordingMap,
	}
}

func (pm *PlatformMonitor) doPlatformCmdConfigReload(msg platform.CommandMsg) (bool, error) {
	cfg, ok := msg.Value.(config.Config)
	if !ok {
		return false,
			fmt.Errorf("command message does not contain valid config value: %v", msg.Value)
	}

	pCfg, found := cfg.PlatformCfgMap[pm.platform.Name()]
	if !found {
		// TODO: a previously configured platform has since been deleted from the config,
		//       what shall we do? for now just ignore but we should probably teardown platform.
		return false, fmt.Errorf("platform's config removed from config.")
	}

	applied, err := pm.platform.UpdateConfig(pCfg)
	if err != nil {
		return false, fmt.Errorf("error updating platform's config: %v", err)
	}

	return applied, nil
}

func (pm *PlatformMonitor) doPlatformCmdStatus(msg platform.CommandMsg) {
	pm.recordingMapMutex.Lock()
	defer pm.recordingMapMutex.Unlock()

	var digests []hls.RecordingDigest
	for _, v := range pm.recordingMap {
		if v != nil {
			digests = append(digests, v.GetCurrentDigest())
		}
	}

	returnMsg := platform.CmdStatusReturn{
		PlatformName: pm.platform.Name(),
		Digests:      digests,
	}

	// ensure non-blocking send
	select {
	case msg.ReturnChan <- returnMsg:
	default:
		slog.Warn(fmt.Sprintf(
			"(%s) tried returning status, but the channel was unavailable. Dropped message: %v",
			pm.platform.Name(), returnMsg))
	}
}

// TODO: would it be worthwhile/efficient/cleaner to just have stats be returned by CmdStatus?
func (pm *PlatformMonitor) doPlatformCmdStats(msg platform.CommandMsg) {
	pm.statsMutex.RLock()
	defer pm.statsMutex.RUnlock()

	returnMsg := platform.CmdStatsReturn{
		PlatformName: pm.platform.Name(),
		Stats:        pm.stats,
	}

	// ensure non-blocking send
	select {
	case msg.ReturnChan <- returnMsg:
	default:
		slog.Warn(fmt.Sprintf(
			"(%s) tried returning stats, but the channel was unavailable. Dropped message: %v",
			pm.platform.Name(), returnMsg))
	}
}

func (pm *PlatformMonitor) handlePlatformCommandMsg(msg platform.CommandMsg) {
	switch msg.Type {
	case platform.CmdConfigReload:
		{
			applied, err := pm.doPlatformCmdConfigReload(msg)
			if err != nil {
				slog.Error(fmt.Sprintf(
					"(%s) error performing config-reload command: %v", pm.platform.Name(), err))
				return
			}

			if applied {
				slog.Info(fmt.Sprintf("(%s) config successfully updated", pm.platform.Name()))
			} else {
				slog.Info(fmt.Sprintf(
					"(%s) config successfully updated, but no changes were applied", pm.platform.Name()))
			}
		}
	case platform.CmdStatus:
		pm.doPlatformCmdStatus(msg)
	case platform.CmdStats:
		pm.doPlatformCmdStats(msg)
	}
}

func (pm *PlatformMonitor) StartMonitor() {
	sListStr := "[ "
	for _, s := range pm.platform.GetStreamers() {
		sListStr += (s.Username() + " ")
	}
	sListStr += "]"

	slog.Info(fmt.Sprintf("(%s) started monitoring: %s", pm.platform.Name(), sListStr))

	var wg sync.WaitGroup

	pm.stats.StartTime = time.Now()

	// set timer to 1 so that the first loop iteration will start instantly.
	// reset and wait the platform check interval every time after this.
	timer := time.NewTimer(1)

loop:
	for {
		select {
		case <-pm.ctx.Done():
			slog.Info(fmt.Sprintf("(%s) got shutdown signal, stopping...", pm.platform.Name()))
			break loop
		case msg := <-pm.cmdMsgChan:
			pm.handlePlatformCommandMsg(msg) // always a blocking call.
			continue
		case <-timer.C:
		}

		timer.Reset(time.Duration(pm.platform.GetCheckInterval()) * time.Second)

		liveStreamers, err := pm.platform.GetLiveStreamers()
		if err != nil {
			slog.Error(fmt.Sprintf("(%s) failed checking for live streams: %v", pm.platform.Name(),
				util.TruncateUrlsFromString(err.Error())))
			continue // this will attempt forever; shut down daemon, change config, or send signal to stop.
		}

		pm.recordingMapMutex.Lock()
		for _, liveStreamer := range liveStreamers {
			_, found := pm.recordingMap[liveStreamer.UniqueID()] // check if already recording live stream
			if found {
				continue
			}

			hlsStream, err := pm.platform.GetHLSStream(liveStreamer)
			if err != nil {
				slog.Error(
					fmt.Sprintf("(%s) error getting hls url for %s: %v",
						pm.platform.Name(), liveStreamer.Username(), err))
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
				pm.ctx,
				liveStreamer.Username(),
				&http.Client{Timeout: 5 * time.Second},
				hlsStream,
				outputPath)

			pm.recordingMap[liveStreamer.UniqueID()] = recorder

			// Record the stream
			wg.Add(1)
			go func() {
				defer wg.Done()
				slog.Info(fmt.Sprintf("(%s) starting to record streamer %s", pm.platform.Name(), liveStreamer.Username()))

				digest, err := recorder.Record()
				if err != nil {
					slog.Error(fmt.Sprintf(
						"(%s) error recording %s hls stream: %v", pm.platform.Name(), liveStreamer.Username(), err))
				}

				pm.recordingMapMutex.Lock()
				delete(pm.recordingMap, liveStreamer.UniqueID())
				pm.recordingMapMutex.Unlock()

				if err != nil {
					if digest.BytesWritten != 0 {
						slog.Error(fmt.Sprintf("(%s) error downloading mid-stream for %s: %v",
							pm.platform.Name(), liveStreamer.Username(), err))
					} else {
						slog.Error(fmt.Sprintf("(%s) error starting stream download, no file made for %s: %v",
							pm.platform.Name(), liveStreamer.Username(), err))
						return
					}
				}

				if digest.GracefulEnd {
					slog.Info(fmt.Sprintf("(%s) %s went offline %s",
						pm.platform.Name(), liveStreamer.Username(), hls.DigestFileInfoString(digest)))
				} else {
					slog.Info(fmt.Sprintf("(%s) recording for %s ended abruptly %s",
						pm.platform.Name(), liveStreamer.Username(), hls.DigestFileInfoString(digest)))
				}

				pm.statsMutex.Lock()
				pm.stats.BytesWritten += digest.BytesWritten
				pm.stats.Recordings += 1
				pm.stats.FinishedDigests = append(pm.stats.FinishedDigests, digest)
				pm.statsMutex.Unlock()

				// If an archive dir path has been set in the config for either the top, platform, or streamer level,
				// move the finished downloaded stream file into the archive directory.
				if liveStreamer.GetArchiveDirPath() != nil {
					err := util.MoveFile(outputPath, *liveStreamer.GetArchiveDirPath(), fileName)
					if err != nil {
						slog.Error(fmt.Sprintf("(%s) error moving recording %s into archive %s",
							pm.platform.Name(), fileName, *liveStreamer.GetArchiveDirPath()))
					} else {
						slog.Info(fmt.Sprintf("(%s) moved recording %s into %s",
							pm.platform.Name(), fileName, *liveStreamer.GetArchiveDirPath()))
					}
				}
			}()
		}

		pm.recordingMapMutex.Unlock()
		slog.Debug(fmt.Sprintf("(%s) platform loop iteration...", pm.platform.Name()))
	}

	wg.Wait()
	slog.Info(fmt.Sprintf("(%s) gracefully stopped monitoring.", pm.platform.Name()))
}
