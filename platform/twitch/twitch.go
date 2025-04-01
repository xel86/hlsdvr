package twitch

import (
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/xel86/hlsdvr/hls"
	"github.com/xel86/hlsdvr/platform"
)

type Streamer struct {
	UserID      string
	UserLogin   string
	DisplayName string
	OutputPath  string
	ArchivePath *string
}

func (s *Streamer) UniqueID() string {
	return s.UserID
}

func (s *Streamer) Username() string {
	return s.UserLogin
}

func (s *Streamer) GetOutputDirPath() string {
	return s.OutputPath
}

func (s *Streamer) GetArchiveDirPath() *string {
	return s.ArchivePath
}

// Manages all internal state about the twitch streamers we are recording.
type Platform struct {
	streamers     []Streamer // TODO: this is probably going to need a mutex and to audit every use so far.
	userIdMap     map[string]*Streamer
	userLoginMap  map[string]*Streamer
	httpClient    *http.Client
	api           *APIClient
	checkInterval int // In seconds

	// This isn't supposed to be used after UpdateConfig() is called.
	// It is only for comparing a new config to the current one to know if we have to change anything.
	// This only gets set once a successful config has been parsed and validated.
	currentActiveCfg Config
}

// Twitch specific section from config file.
type Config struct {
	// For helix twitch api
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`

	// User access token for GQL queries from device code grant flow or web frontend
	// https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/#device-code-grant-flow
	UserToken string `json:"user_token"`

	CheckLiveInterval int     `json:"check_live_interval,omitempty"` // in seconds
	OutputDirPath     *string `json:"output_dir_path,omitempty"`
	ArchiveDirPath    *string `json:"archive_dir_path,omitempty"`

	Streamers []StreamerConfig `json:"streamers"`
}

type StreamerConfig struct {
	Enabled bool `json:"enabled"`
	// Either user login or user id needed. If both are present, user id will take priority.
	UserLogin string `json:"user_login,omitempty"`
	UserID    string `json:"user_id,omitempty"`

	OutputDirPath  *string `json:"output_dir_path,omitempty"`  // optional (overrides default twitch output dir)
	ArchiveDirPath *string `json:"archive_dir_path,omitempty"` // optional (overrides default twitch archive dir)
}

func NewPlatform(cfg Config) (*Platform, error) {
	var t Platform

	t.httpClient = &http.Client{Timeout: 10 * time.Second}
	t.userIdMap = make(map[string]*Streamer)
	t.userLoginMap = make(map[string]*Streamer)
	t.streamers = []Streamer{}

	err := t.updateConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Failed to create platform with config: %v", err)
	}

	return &t, nil
}

func (t *Platform) Name() string {
	return platform.TwitchPlatformName
}

func (t *Platform) GetCheckInterval() int {
	return t.checkInterval
}

// Takes a parsed twitch config and updates the internal streamers slice based on whats in it.
// Will fill in missing account information such as UserLogin and UserID if its not provided directly in
// the config file using the twitch api, and will filter out non-existing or empty streamers.
func (t *Platform) updateStreamers(cfg Config) error {
	t.streamers = []Streamer{} // Reset streamers

	for _, sc := range cfg.Streamers {
		if !sc.Enabled {
			continue
		}

		if (sc.UserID == "") && (sc.UserLogin == "") {
			fmt.Println("Streamer in config with neither a user id or user login specified, skipping.")
			continue
		}

		t.streamers = append(t.streamers, Streamer{
			UserID:    sc.UserID,
			UserLogin: strings.ToLower(sc.UserLogin),
		})
	}

	err := t.updateStreamersInfo()
	if err != nil {
		return fmt.Errorf("failed to fetch user info from twitch api: %v", err)
	}

	// Update each streamer struct's output & archive directory paths after we have validated user info
	// and filtered out incorrectly configured streamers / streamers that don't exist.
	for _, sc := range cfg.Streamers {
		if !sc.Enabled {
			continue
		}

		// See if this streamer specified in the config was validated and information
		// retrieved from the updateStreamersInfo call earlier, and grab the pointer to it if so.
		var streamer *Streamer
		if sc.UserID != "" {
			streamer, _ = t.userIdMap[sc.UserID]
		}
		if (streamer == nil) && (sc.UserLogin != "") {
			streamer, _ = t.userLoginMap[sc.UserLogin]
		}
		if streamer == nil {
			continue
		}

		// Override hierarchy for output & archive directory paths from config.
		// 1. <top-level-path>/twitch/username (default)
		// 2. <platform-level-path>/username
		// 3. <streamer-level-path>/
		// Highest number path that is set is used.
		var outputDirPath string
		if sc.OutputDirPath != nil {
			outputDirPath = *sc.OutputDirPath // streamer level config
		} else {
			outputDirPath = filepath.Join(*cfg.OutputDirPath, streamer.UserLogin)
		}

		var archiveDirPath *string
		if sc.ArchiveDirPath != nil {
			archiveDirPath = sc.ArchiveDirPath // streamer level config
		} else if cfg.ArchiveDirPath != nil {
			withUsername := filepath.Join(*cfg.ArchiveDirPath, streamer.UserLogin)
			archiveDirPath = &withUsername
		}

		streamer.OutputPath = outputDirPath
		streamer.ArchivePath = archiveDirPath
	}

	return nil
}

func (t *Platform) updateConfig(cfg Config) error {
	t.api = NewAPIClient(cfg.ClientID, cfg.ClientSecret, cfg.UserToken, t.httpClient)
	t.checkInterval = cfg.CheckLiveInterval

	err := t.api.ensureValidAppToken()
	if err != nil {
		return fmt.Errorf("Unable to obtain valid twitch helix api token: %v", err)
	}

	err = t.api.ensureValidUserToken()
	if err != nil {
		return fmt.Errorf("Failed to validate user token in config: %v", err)
	}

	err = t.updateStreamers(cfg)
	if err != nil {
		return fmt.Errorf("failed to get streamer(s) info from config: %v", err)
	}

	t.currentActiveCfg = cfg
	return nil
}

// Generic platform interface method
// The caller HAS TO manage a mutex to lock the current instance of the platform before calling this.
func (t *Platform) UpdateConfig(platformCfg any) (bool, error) {
	cfg, ok := platformCfg.(*Config)
	if !ok {
		return false, fmt.Errorf("tried to update with non-twitch config: %v", platformCfg)
	}

	if reflect.DeepEqual(*cfg, t.currentActiveCfg) {
		return false, nil
	}

	slog.Info("(twitch) updating platform with new config.")

	// If both either the client id or client secret was changed, create a new helix api and app token.
	// if the user token was also changed, validate that as well on the new helix api.
	// if only the user token was changed, replace that on the existing helix api
	// and do not generate a new app access token.
	if cfg.ClientID != t.currentActiveCfg.ClientID ||
		cfg.ClientSecret != t.currentActiveCfg.ClientSecret {
		newApi := NewAPIClient(cfg.ClientID, cfg.ClientSecret, cfg.UserToken, t.httpClient)
		err := newApi.ensureValidAppToken()
		if err != nil {
			return false, fmt.Errorf(
				"Unable to obtain valid twitch helix api token with new config credentials: %v", err)
		}

		slog.Info("(twitch) set ClientID and/or ClientSecret from new config successfully.")

		if cfg.UserToken != t.currentActiveCfg.UserToken {
			err = newApi.ensureValidUserToken()
			if err != nil {
				return false, fmt.Errorf("User token in new config is invalid: %v", err)
			}
			slog.Info("(twitch) set user token from new config successfully.")
		}

		t.api = newApi // Replace api only if all credentials are validated working.
	} else if cfg.UserToken != t.currentActiveCfg.UserToken {
		t.api.ReplaceUserToken(cfg.UserToken)
		err := t.api.ensureValidUserToken()
		if err != nil {
			t.api.ReplaceUserToken(t.currentActiveCfg.UserToken)
			return false, fmt.Errorf("User token in new config is invalid: %v. Continuing to use previous one.", err)
		}

		slog.Info("(twitch) set user token from new config successfully.")
	}

	t.checkInterval = cfg.CheckLiveInterval

	// TODO: would checking if any changes were made to the streamer list cfg (both removing and/or adding streamers)
	//       before updating be even more work than just updating from scratch? is it even worth
	err := t.updateStreamers(*cfg)
	if err != nil {
		return true, fmt.Errorf("failed to get streamer(s) info from new config: %v", err)
	}

	t.currentActiveCfg = *cfg

	return true, nil
}

// Return a list of platform-generic streamers, which are copies from our internal streamer slice.
func (t *Platform) GetStreamers() []platform.Streamer {
	var streamers []platform.Streamer
	for _, s := range t.streamers {
		streamers = append(streamers, &s)
	}
	return streamers
}

// Platform interface method implementation for returning the streamers who are live.
// Returns a slice of platform-generic streamers.
// They are copies of the Streamers inside of the internal platform slice, not direct references.
func (t *Platform) GetLiveStreamers() ([]platform.Streamer, error) {
	var liveStreamers []platform.Streamer
	var userIds []string
	for _, s := range t.streamers {
		userIds = append(userIds, s.UserID)
	}

	resp, err := t.api.GetStreams(GetStreamsParams{UserIDs: userIds})
	if err != nil {
		return nil, fmt.Errorf("Error calling GetStreams twitch api: %v", err)
	}

	for _, stream := range resp.Data {
		streamer, found := t.userIdMap[stream.UserID]
		if !found {
			slog.Warn(fmt.Sprintf("twitch: a live stream (%s) which was not requested was returned?", stream.UserLogin))
			continue
		}

		// Return copies of streamers, not a reference to the actual internally managed Streamer struct.
		// &(*streamer) does not do the same thing :)
		streamerCopy := *streamer
		liveStreamers = append(liveStreamers, &streamerCopy)
	}

	return liveStreamers, nil
}

func (t *Platform) GetHLSUrl(s platform.Streamer) (string, error) {
	token, err := t.api.getPlaybackAccessToken(s.Username())
	if err != nil {
		return "", fmt.Errorf("Twitch: Error getting playback access token: %v", err)
	}

	variantPlaylistUrl := makeUsherM3U8PlaylistUrl(s.Username(), token)

	streamVariants, err := hls.GetM3U8StreamVariants(t.httpClient, variantPlaylistUrl)
	if err != nil {
		return "", fmt.Errorf("Twitch: Error getting stream variants from m3u8 playlist: %v", err)
	}

	// TODO: configuration for quality, sorting by quality, resolution, codec, etc.
	return streamVariants[0].Url, nil
}

// Assumes an api and internal streamers slice has been set up already.
func (t *Platform) updateStreamersInfo() error {
	var userIds, userLogins []string
	idMap := make(map[string]*Streamer)
	loginMap := make(map[string]*Streamer)

	for i, s := range t.streamers {
		if s.UserID != "" {
			userIds = append(userIds, s.UserID)
			idMap[s.UserID] = &t.streamers[i]
			continue
		}
		if s.UserLogin != "" {
			userLogins = append(userLogins, s.UserLogin)
			loginMap[s.UserLogin] = &t.streamers[i]
			continue
		}
		slog.Warn("twitch: tried to update a streamer's info with no user id or login set, skipping.")
	}

	usersResp, err := t.api.GetUsers(GetUsersParams{UserIDs: userIds, UserLogins: userLogins})
	if err != nil {
		return fmt.Errorf("Failed to GetUsers from api: %v", err)
	}

	t.userIdMap = make(map[string]*Streamer)    // reset user id -> streamer map
	t.userLoginMap = make(map[string]*Streamer) // reset user login -> streamer map
	for _, u := range usersResp.Data {
		streamer, found := idMap[u.UserID]
		if !found {
			streamer, found = loginMap[u.UserLogin]
			if !found {
				slog.Warn(fmt.Sprintf("received data for user %s (%s) that wasn't requested?", u.UserLogin, u.UserID))
				continue
			}
		}

		streamer.UserID = u.UserID
		streamer.UserLogin = u.UserLogin
		streamer.DisplayName = u.DisplayName
		t.userIdMap[streamer.UserID] = streamer
		t.userLoginMap[streamer.UserLogin] = streamer

		// Delete streamers from these temporary maps as we successfully update their info.
		// makes it easy to check if any streamers didn't get any data returned.
		delete(idMap, u.UserID)
		delete(loginMap, u.UserLogin)
	}

	// Check for an user ids or logins that were requested but not returned from api.
	// Almost certainely because the user doesn't exist.
	for _, streamer := range t.streamers {
		_, foundID := idMap[streamer.UserID]
		_, foundLogin := loginMap[streamer.UserLogin]
		if foundID || foundLogin {
			slog.Warn(fmt.Sprintf(
				"Streamer %s (%s) was not returned from GetUsers, most likely user info incorrect or user doesn't exist.",
				streamer.UserLogin, streamer.UserID))
		}
	}

	return nil
}
