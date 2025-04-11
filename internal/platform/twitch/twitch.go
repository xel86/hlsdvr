package twitch

import (
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/xel86/hlsdvr/internal/hls"
	"github.com/xel86/hlsdvr/internal/platform"
)

type Streamer struct {
	UserID      string
	UserLogin   string
	DisplayName string
	OutputPath  string
	ArchivePath *string
}

// Returned from setupValidatedStreamers()
// All Streamer(s) will have a filled in and validated user id, login, and display name.
// user id and login maps will take in a key of the id or login respectivly and return a
// pointer to the streamer with the same value in the Streamers slice.
type ValidatedStreamersReturn struct {
	Streamers    []Streamer
	userIdMap    map[string]*Streamer
	userLoginMap map[string]*Streamer
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
	// List of streamers derived from the config file
	// All streamers in this list have been validated to exist, and UserID, UserLogin, and DisplayName are set.
	// TODO: this is probably going to need a mutex and to audit every use so far.
	streamers    []Streamer
	userIdMap    map[string]*Streamer
	userLoginMap map[string]*Streamer

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

	t.api = NewAPIClient(cfg.ClientID, cfg.ClientSecret, cfg.UserToken, t.httpClient)
	t.checkInterval = cfg.CheckLiveInterval

	err := t.api.ensureValidAppToken()
	if err != nil {
		return nil, fmt.Errorf("Unable to obtain valid twitch helix api token: %v", err)
	}

	if cfg.UserToken != "" {
		err = t.api.ensureValidUserToken()
		if err != nil {
			return nil, fmt.Errorf("Failed to validate user token in config: %v", err)
		}
	}

	valid, err := t.setupValidatedStreamers(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to extract and validate streamer(s) from config:", err)
	}

	t.streamers = valid.Streamers
	t.userIdMap = valid.userIdMap
	t.userLoginMap = valid.userLoginMap

	t.currentActiveCfg = cfg
	return &t, nil
}

func (t *Platform) Name() string {
	return platform.TwitchPlatformName
}

func (t *Platform) GetCheckInterval() int {
	return t.checkInterval
}

// Take a list of streamer configs, presumably from a config file and:
// filter out empty ones, validate through api that they are real streamers,
// then return a validated streamer list with the config options set.
// If any streamers from the config can't be validated, it will be logged.
// An error is only set if there was a direct issue retrieving any of the streamer info.
func (t *Platform) setupValidatedStreamers(cfg Config) (*ValidatedStreamersReturn, error) {
	var userIdsParam, userLoginsParam []string
	userIdsParamMap := make(map[string]*StreamerConfig)
	userLoginsParamMap := make(map[string]*StreamerConfig)

	for i, sc := range cfg.Streamers {
		if !sc.Enabled {
			continue
		}

		if (sc.UserID == "") && (sc.UserLogin == "") {
			slog.Info("Tried to validate a streamer in config with neither a user id nor user login specified, skipping.")
			continue
		}

		if sc.UserID != "" {
			userIdsParam = append(userIdsParam, sc.UserID)
			userIdsParamMap[sc.UserID] = &cfg.Streamers[i]
			continue
		}

		if sc.UserLogin != "" {
			userLogin := strings.ToLower(sc.UserLogin)
			userLoginsParam = append(userLoginsParam, userLogin)
			userLoginsParamMap[userLogin] = &cfg.Streamers[i]
			continue
		}
	}

	usersResp, err := t.api.GetUsers(GetUsersParams{UserIDs: userIdsParam, UserLogins: userLoginsParam})
	if err != nil {
		return nil, fmt.Errorf("failed to call GetUsers from api: %v", err)
	}

	var validated ValidatedStreamersReturn
	validated.userIdMap = make(map[string]*Streamer)
	validated.userLoginMap = make(map[string]*Streamer)
	validStreamerIdx := 0

	for _, u := range usersResp.Data {
		// This is probably overly cautious, doubtful that the twitch api will accidentally return users
		// we didn't request, but just making sure...
		streamerCfg, found := userIdsParamMap[u.UserID]
		if !found {
			streamerCfg, found = userLoginsParamMap[u.UserLogin]
			if !found {
				slog.Warn(fmt.Sprintf("received data for user %s (%s) that wasn't requested?", u.UserLogin, u.UserID))
				continue
			}
		}

		// Override hierarchy for output & archive directory paths from config.
		// 1. <top-level-path>/twitch/username (default)
		// 2. <platform-level-path>/username
		// 3. <streamer-level-path>/
		// Highest number path that is set is used.
		// Unfortunately this work can't just be done in config.go because we want a validated
		// username for the default folder path, not the user login a config MAY provide.
		var outputDirPath string
		if streamerCfg.OutputDirPath != nil {
			outputDirPath = *streamerCfg.OutputDirPath // streamer level config
		} else {
			// cfg.OutputDirPath guaranteed to not be nil because we set it in config.go
			outputDirPath = filepath.Join(*cfg.OutputDirPath, u.UserLogin) // default derived from platform level
		}

		var archiveDirPath *string
		if streamerCfg.ArchiveDirPath != nil {
			archiveDirPath = streamerCfg.ArchiveDirPath // streamer level config
		} else if cfg.ArchiveDirPath != nil {
			withUsername := filepath.Join(*cfg.ArchiveDirPath, u.UserLogin) // default derived from platform level
			archiveDirPath = &withUsername
		}

		streamer := Streamer{
			UserID:      u.UserID,
			UserLogin:   u.UserLogin,
			DisplayName: u.DisplayName,
			OutputPath:  outputDirPath,
			ArchivePath: archiveDirPath,
		}
		validated.Streamers = append(validated.Streamers, streamer)
		validated.userIdMap[streamer.UserID] = &validated.Streamers[validStreamerIdx]
		validated.userLoginMap[streamer.UserLogin] = &validated.Streamers[validStreamerIdx]
		validStreamerIdx++
	}

	// Check for an user ids or logins that were requested from config but were not returned from api.
	// Almost certainely because the user doesn't exist.
	for _, s := range cfg.Streamers {
		_, foundId := usersResp.GetUserByID(s.UserID)
		_, foundLogin := usersResp.GetUserByLogin(s.UserLogin)
		if !(foundId || foundLogin) {
			slog.Warn(fmt.Sprintf(
				"Streamer %s (%s) from config not found by twitch, most likely user info incorrect or user doesn't exist.",
				s.UserLogin, s.UserID))
		}
	}

	return &validated, nil
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
	appliedChanges := false

	// If both either the client id or client secret was changed, create a new helix api and app token.
	// if the user token was also changed, validate that as well on the new helix api.
	// if only the user token was changed, replace that on the existing helix api
	// and do not generate a new app access token.
	if cfg.ClientID != t.currentActiveCfg.ClientID ||
		cfg.ClientSecret != t.currentActiveCfg.ClientSecret {
		newApi := NewAPIClient(cfg.ClientID, cfg.ClientSecret, cfg.UserToken, t.httpClient)
		err := newApi.ensureValidAppToken()
		if err != nil {
			return appliedChanges, fmt.Errorf(
				"Unable to obtain valid twitch helix api token with new config credentials: %v", err)
		}

		slog.Info("(twitch) set new ClientID and/or ClientSecret from config successfully.")

		if cfg.UserToken != t.currentActiveCfg.UserToken {
			if cfg.UserToken != "" {
				err = newApi.ensureValidUserToken()
				if err != nil {
					return appliedChanges, fmt.Errorf("User token in new config is invalid: %v", err)
				}
				slog.Info("(twitch) set new user token from config successfully.")
			} else {
				slog.Info("(twitch) removed user token from config successfully.")
			}
		}

		t.api = newApi // Replace api only if all credentials are validated working.
		appliedChanges = true
	} else if cfg.UserToken != t.currentActiveCfg.UserToken {
		t.api.ReplaceUserToken(cfg.UserToken)
		if cfg.UserToken != "" {
			err := t.api.ensureValidUserToken()
			if err != nil {
				t.api.ReplaceUserToken(t.currentActiveCfg.UserToken)
				return appliedChanges, fmt.Errorf(
					"User token in new config is invalid: %v. Continuing to use previous one.", err)
			}
			slog.Info("(twitch) set new user token from config successfully.")
		} else {
			slog.Info("(twitch) removed user token from config successfully.")
		}
		appliedChanges = true
	}

	if t.checkInterval != cfg.CheckLiveInterval {
		t.checkInterval = cfg.CheckLiveInterval
		slog.Info(fmt.Sprintf(
			"(twitch) set new check live interval (%d) from config successfully.", t.checkInterval))
		appliedChanges = true
	}

	// Update the internal streamers list only if there were changes to the streamers list in the config.
	// Changing either the output or archive path's for the platform level config will result in the derived
	// path's for each streamer to change (assuming not all of them were specifically set on the streamer level config)
	// So recreate the internal streamers list if any of these change from the current config.
	if !(reflect.DeepEqual(cfg.OutputDirPath, t.currentActiveCfg.OutputDirPath) &&
		reflect.DeepEqual(cfg.ArchiveDirPath, t.currentActiveCfg.ArchiveDirPath) &&
		reflect.DeepEqual(cfg.Streamers, t.currentActiveCfg.Streamers)) {
		valid, err := t.setupValidatedStreamers(*cfg)
		if err != nil {
			return appliedChanges, fmt.Errorf("failed to setup new streamers from config changes: %v", err)
		}

		t.streamers = valid.Streamers
		t.userIdMap = valid.userIdMap
		t.userLoginMap = valid.userLoginMap

		sListStr := "[ "
		for _, s := range t.streamers {
			sListStr += (s.UserLogin + " ")
		}
		sListStr += "]"

		slog.Info(fmt.Sprintf("(twitch) streamers list updated, now monitoring: %s", sListStr))
		appliedChanges = true
	}

	t.currentActiveCfg = *cfg

	return appliedChanges, nil
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
		return nil, fmt.Errorf("error from twitch api: %v", err)
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

func (t *Platform) GetHLSStream(s platform.Streamer) (hls.M3U8StreamVariant, error) {
	token, err := t.api.getPlaybackAccessToken(s.Username())
	if err != nil {
		return hls.M3U8StreamVariant{},
			fmt.Errorf("Twitch: Error getting playback access token: %v", err)
	}

	variantPlaylistUrl := makeUsherM3U8PlaylistUrl(s.Username(), token)

	streamVariants, err := hls.GetM3U8StreamVariants(t.httpClient, variantPlaylistUrl)
	if err != nil {
		return hls.M3U8StreamVariant{},
			fmt.Errorf("Twitch: Error getting stream variants from m3u8 playlist: %v", err)
	}

	// TODO: configuration for quality, sorting by quality, resolution, codec, etc.
	return streamVariants[0], nil
}
