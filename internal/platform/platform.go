package platform

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/xel86/hlsdvr/internal/hls"
)

// Uniform names that we should use across code base for a platform.
// This is important to be consistent because we will not *always* grab the name from
// a platform with the platform interface Name() method.
// We will do that in generic functions that use the interface, but in certain sections of code
// where we actually have to do an exhaustive list for each platform explicitly, like in the config,
// we want to be sure that the name for the platform here will be the same as the one returned in Platform.Name()
const (
	TwitchPlatformName  = "twitch"
	YoutubePlatformName = "youtube"
)

// Generic streamer type
// A TwitchStreamer, YoutubeStreamer, etc.
type Streamer interface {
	UniqueID() string // A unique identifier for the streamer, platform dependent.
	Username() string
	GetOutputDirPath() string
	GetArchiveDirPath() *string
}

// A live streaming platform that streamer go live and stream on
// Twitch, YouTube, Kick, etc.
// Structs that implement Platform should always be used as pointers.
type Platform interface {
	Name() string
	GetCheckInterval() int // In seconds

	// must pass in the correct config (value) type for the platform.
	// the caller must ensure that the platform this method is called on is mutex locked
	// if the platform could be accessed via multiple threads.
	// should return:
	// (true, nil)  if updated successfully
	// (false, nil) if no changes were applied and no errors.
	// (true, err)  if an error occured but some changes were applied to existing config.
	// (false, err) if an error occured and no changes were applied
	UpdateConfig(platformCfg any) (bool, error)

	GetStreamers() []Streamer
	GetLiveStreamers() ([]Streamer, error) // Should return a copy of the Streamers, not direct reference.
	GetHLSStream(s Streamer) (hls.M3U8StreamVariant, error)
}

// Historical stats for a platform being monitored.
// Variables such as BytesWritten are totals, summed incrementally from each additional recording.
type PlatformStats struct {
	BytesWritten      uint64                    // Total bytes written to disk across all recordings.
	AvgBytesPerStream uint64                    // Total average bytes per stream over all recordings.
	AvgBytesPerSecond uint64                    // Total average bytes per second over runtime of platform.
	TotalDuration     float64                   // Total amount of time recorded
	Recordings        int                       // Total number of live streams recorded.
	StreamerStats     map[string]*StreamerStats // Individual stats per streamer (string key is username/identifier)
	StartTime         time.Time                 // Time when the platform began monitoring.
}

// Historical stats for a single streamer on a platform.
// Precomputed running averages and other stats for all streams from a single streamer
type StreamerStats struct {
	Identifier            string  // String that identifies the streamer, usually a username.
	BytesWritten          uint64  // Total bytes written to disk across all recordings for a streamer.
	AvgBytesPerStream     uint64  // Total average bytes per stream over all streams from a streamer.
	AvgBytesPerSecondLive uint64  // Total average bytes per second over total duration recorded.
	AvgBytesPerSecond     uint64  // Total average bytes per second over total duration both offline & live.
	TotalDuration         float64 // Total amount of time recorded from a streamer in seconds.
	FinishedDigests       []hls.RecordingDigest
	Recordings            int       // Total number of live streams recorded / digests.
	LatestRecordingStart  time.Time // The latest/newest recording's (live or finished) start time.
}

// Commands that will be sent from a CommandSender to platforms.
// These are distinct from our RPC server commands, although the vast majority
// of commands will overlap/have their own RPC version.
type CommandType int

const (
	CmdConfigReload CommandType = iota
	CmdStats
)

var CommandNameMap = map[CommandType]string{
	CmdConfigReload: "config-reload",
	CmdStats:        "stats",
}

// A streamer and the associated directory paths that their streams will
// be downloaded into. Probably only going to be used for stats to calculate disk utilization.
type StreamerTargets struct {
	Username       string
	OutputDirPath  string
	ArchiveDirPath *string
}

type CmdStatsReturn struct {
	PlatformName    string
	StreamerTargets []StreamerTargets
	LiveDigests     []hls.RecordingDigest
	Stats           PlatformStats
}

// Optional params to include as the CommandMsg value.
type CmdStatsParams struct {
	IncludePastDigests bool // Include all digests from previous recordings that aren't live.
}

// If a command can take parameters to specify operations, it will be put in Value.
// Each command which assumes the platform will respond back with a message
// has a "Return" type, which will be put into the ReturnChan of the received msg.
// A CommandMsg with Type CmdStatus, will receive CmdStatusReturn structs in its ReturnChan.
type CommandMsg struct {
	Type       CommandType
	Value      any
	ReturnChan chan any
}

// Sends messages to running platforms in their own goroutines.
// Can broadcast to all, or specific platforms.
// Can have platform(s) respond back to each specific message.
// Concurrent and Thread-Safe to use.
type CommandSender struct {
	platformChans map[string]chan CommandMsg // platform.Name() key -> channel value
	mutex         sync.RWMutex
}

func NewCommandSender(platforms []Platform) *CommandSender {
	chanMap := make(map[string]chan CommandMsg)
	for _, p := range platforms {
		pChan := make(chan CommandMsg, 5) // arbitrary channel buffer size of 5
		chanMap[p.Name()] = pChan
	}
	return &CommandSender{
		platformChans: chanMap,
		mutex:         sync.RWMutex{},
	}
}

func (pcs *CommandSender) GetNumPlatforms() int {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	len := len(pcs.platformChans)
	return len
}

func (pcs *CommandSender) GetPlatformNames() []string {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	names := []string{}
	for k, _ := range pcs.platformChans {
		names = append(names, k)
	}
	return names
}

func (pcs *CommandSender) GetPlatformChan(platformName string) (chan CommandMsg, bool) {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	ch, found := pcs.platformChans[platformName]
	return ch, found
}

func (pcs *CommandSender) Broadcast(msg CommandMsg) {
	pcs.mutex.RLock()
	defer pcs.mutex.RUnlock()

	for name, ch := range pcs.platformChans {
		// ensure non-blocking send
		select {
		case ch <- msg:
		default:
			slog.Warn(fmt.Sprintf(
				"Tried sending command to platform (%s), but the channel was full. Dropped message: %s",
				name, CommandNameMap[msg.Type]))
		}
	}
}

func (pcs *CommandSender) BroadcastTo(platformNames []string, msg CommandMsg) {
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
				name, CommandNameMap[msg.Type]))
		}
	}
}

func (pcs *CommandSender) RemovePlatform(platformName string) {
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
