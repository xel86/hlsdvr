package platform

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
	GetHLSUrl(s Streamer) (string, error)
}
