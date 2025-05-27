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

	stats      platform.PlatformStats
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

func (pm *PlatformMonitor) GetPlatformStats() platform.PlatformStats {
	pm.statsMutex.RLock()
	defer pm.statsMutex.RUnlock()

	return pm.stats
}

// Really only intended to be used when restoring stats from a previous instance/session.
func (pm *PlatformMonitor) SetPlatformStats(stats platform.PlatformStats) {
	pm.statsMutex.Lock()
	defer pm.statsMutex.Unlock()

	pm.stats = stats
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

func updateStats(stats *platform.PlatformStats, digest hls.RecordingDigest) {
	// platform stats
	stats.BytesWritten += digest.BytesWritten
	stats.TotalDuration += digest.RecordingDuration
	stats.Recordings += 1
	stats.AvgBytesPerStream = stats.BytesWritten / uint64(stats.Recordings)

	// per-streamer stats
	sStats, exists := stats.StreamerStats[digest.Identifier]
	if !exists {
		sStats = &platform.StreamerStats{Identifier: digest.Identifier}
		stats.StreamerStats[digest.Identifier] = sStats
	}

	sStats.BytesWritten += digest.BytesWritten
	sStats.TotalDuration += digest.RecordingDuration
	sStats.Recordings += 1
	sStats.AvgBytesPerStream = sStats.BytesWritten / uint64(sStats.Recordings)
	if sStats.TotalDuration != 0 {
		sStats.AvgBytesPerSecondLive = sStats.BytesWritten / uint64(sStats.TotalDuration)
	}
	sStats.LatestRecordingStart = digest.RecordingStart
	sStats.FinishedDigests = append(sStats.FinishedDigests, digest)
}

func (pm *PlatformMonitor) doPlatformCmdStats(msg platform.CommandMsg) {
	pm.recordingMapMutex.Lock()
	pm.statsMutex.RLock()
	defer pm.recordingMapMutex.Unlock()
	defer pm.statsMutex.RUnlock()

	params := platform.CmdStatsParams{}
	if msg.Value != nil {
		v, ok := msg.Value.(platform.CmdStatsParams)
		if !ok {
			slog.Error(fmt.Sprintf(
				"(%s): stats command message does not contain valid CmdStatsParams value: %+v",
				pm.platform.Name(), msg.Value))
			return
		}
		params = v
	}

	var liveDigests []hls.RecordingDigest
	for _, v := range pm.recordingMap {
		if v != nil {
			liveDigests = append(liveDigests, v.GetCurrentDigest())
		}
	}

	targets := []platform.StreamerTargets{} // for calculating disk utilization and estimated times
	for _, s := range pm.platform.GetStreamers() {
		targets = append(targets, platform.StreamerTargets{
			Username:       s.Username(),
			OutputDirPath:  s.GetOutputDirPath(),
			ArchiveDirPath: s.GetArchiveDirPath(),
		})
	}

	// TODO: Can we avoid doing this incredibly ugly copy?
	// We need to create a deep copy to send out since we use pointers for the streamer stats and slices
	// and the rpc server won't be responsible for locking.
	// But also we want to avoid having to make a big copy for the digests slices if
	// the param wasn't passed in to do so.
	// All we want to do is copy existing stats but exclude/include the FinishedDigests for each streamer
	// based on the IncludePastDigests option without doing any unnecessary large copies
	statsCopy := platform.PlatformStats{
		BytesWritten:      pm.stats.BytesWritten,
		AvgBytesPerStream: pm.stats.AvgBytesPerStream,
		AvgBytesPerSecond: pm.stats.AvgBytesPerSecond,
		TotalDuration:     pm.stats.TotalDuration,
		Recordings:        pm.stats.Recordings,
		StartTime:         pm.stats.StartTime,
		StreamerStats:     make(map[string]*platform.StreamerStats, len(pm.stats.StreamerStats)),
	}
	for username, stats := range pm.stats.StreamerStats {
		digestsCopy := []hls.RecordingDigest{}
		if params.IncludePastDigests {
			digestsCopy = make([]hls.RecordingDigest, len(stats.FinishedDigests))
			copy(digestsCopy, stats.FinishedDigests)
		}

		statsCopy.StreamerStats[username] = &platform.StreamerStats{
			Identifier:            stats.Identifier,
			BytesWritten:          stats.BytesWritten,
			AvgBytesPerStream:     stats.AvgBytesPerStream,
			AvgBytesPerSecondLive: stats.AvgBytesPerSecondLive,
			AvgBytesPerSecond:     stats.AvgBytesPerSecond,
			TotalDuration:         stats.TotalDuration,
			Recordings:            stats.Recordings,
			LatestRecordingStart:  stats.LatestRecordingStart,
			FinishedDigests:       digestsCopy,
		}
	}

	// Update the historical stats with the current stats from the live digests
	// Update the copy, not the real stats, since the stats internally should only
	// track & be updated for completed recordings, not ongoing ones.
	for _, d := range liveDigests {
		updateStats(&statsCopy, d)
	}

	// Calculate average bytes per second for both platform and per-streamer (offline+live time) here.
	// This is for accuracy as the stats would only be updated after an additional stream has ended
	// if we calculated them like every other stat.
	statsCopy.AvgBytesPerSecond = (statsCopy.BytesWritten / uint64(time.Since(statsCopy.StartTime).Seconds()))
	for _, v := range statsCopy.StreamerStats {
		v.AvgBytesPerSecond = (v.BytesWritten / uint64(time.Since(statsCopy.StartTime).Seconds()))
	}

	returnMsg := platform.CmdStatsReturn{
		PlatformName:    pm.platform.Name(),
		StreamerTargets: targets,
		LiveDigests:     liveDigests,
		Stats:           statsCopy,
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

	// Initilize stats here if they were not restored from a saved stats file.
	if pm.stats.StartTime.IsZero() {
		pm.stats.StartTime = time.Now()
	}
	if pm.stats.StreamerStats == nil {
		pm.stats.StreamerStats = make(map[string]*platform.StreamerStats)
	}

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

			// TODO: probably want a secondary context to stop recording this specific stream,
			// 	     on top of the global context.
			recorder := hls.NewRecorder(
				pm.ctx,
				liveStreamer.Username(),
				&http.Client{Timeout: 5 * time.Second},
				hlsStream,
				liveStreamer.GetOutputDirPath())

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
				updateStats(&pm.stats, digest)
				pm.statsMutex.Unlock()

				// If an archive dir path has been set in the config for either the top, platform, or streamer level,
				// move the finished downloaded stream file into the archive directory.
				if liveStreamer.GetArchiveDirPath() != nil {
					fileName := filepath.Base(digest.OutputPath)

					slog.Info(fmt.Sprintf("(%s) moving recording %s into %s",
						pm.platform.Name(), fileName, *liveStreamer.GetArchiveDirPath()))

					err := util.MoveFile(digest.OutputPath, *liveStreamer.GetArchiveDirPath(), fileName)
					if err != nil {
						slog.Error(fmt.Sprintf("(%s) error moving recording %s into archive %s",
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
