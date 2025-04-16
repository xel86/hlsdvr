package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/xel86/hlsdvr/internal/platform"
	"github.com/xel86/hlsdvr/internal/platform/twitch"
)

const (
	configDirName  = "hlsdvr"
	configFileName = "config.json"
)

type Config struct {
	OutputDirPath  string         `json:"output_dir_path"`
	ArchiveDirPath *string        `json:"archive_dir_path,omitempty"`
	UnixSocketPath *string        `json:"unix_socket_path"`
	TwitchConfig   *twitch.Config `json:"twitch"`

	// Path to this config file.
	// Only set once a config has been read without error.
	Path string `json:"-"`

	// This is just a string -> platform config.
	// So key "twitch" will return the value for a config's TwitchConfig member variable.
	// cfg.PlatformCfgMap["twitch"] -> cfg.TwitchConfig
	// Enables more generic platform logic code later on for config reloading.
	PlatformCfgMap map[string]any `json:"-"`
}

func ReadConfig(path string) (Config, error) {
	var cfg Config
	cfgFile, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("error opening path: %v", err)
	}
	defer cfgFile.Close()

	bytes, err := io.ReadAll(cfgFile)
	if err != nil {
		return cfg, fmt.Errorf("error reading from file: %v", err)
	}

	err = json.Unmarshal(bytes, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("error unmarshaling json: %v", err)
	}

	cfg.PlatformCfgMap = make(map[string]any)
	// TODO: is there a better way of doing this that doesn't involve passing in the entire config
	//       or two additional args to each NewPlatform() call later for the output/archive paths?
	//       cause this is gonna require giant list for every platform
	//       but i rather it here than in each platform implementation...
	if cfg.TwitchConfig != nil {
		if cfg.TwitchConfig.OutputDirPath == nil {
			defaultOutDirPath := filepath.Join(cfg.OutputDirPath, platform.TwitchPlatformName)
			cfg.TwitchConfig.OutputDirPath = &defaultOutDirPath
		}
		if cfg.ArchiveDirPath != nil && cfg.TwitchConfig.ArchiveDirPath == nil {
			defaultArchiveDirPath := filepath.Join(*cfg.ArchiveDirPath, platform.TwitchPlatformName)
			cfg.TwitchConfig.ArchiveDirPath = &defaultArchiveDirPath
		}
		cfg.PlatformCfgMap[platform.TwitchPlatformName] = cfg.TwitchConfig
	}

	cfg.Path = path
	return cfg, nil
}

func GenerateDefaultExampleConfig(cfgPath string) error {
	// All streamers example configs are disabled by default (Enabled = false)
	exampleStreamerOutputDir := "relative/streams-tmp/username"
	exampleStreamerArchiveDir := "/absolute/other-drive/streams/username"
	exampleUnixSocketPath := "/tmp/hlsdvr.sock" // TODO: windows friendly default location?
	cfg := Config{
		OutputDirPath:  "/home/user/hlsdvr",
		UnixSocketPath: &exampleUnixSocketPath,
		TwitchConfig: &twitch.Config{
			ClientID:          "",
			ClientSecret:      "",
			UserToken:         "",
			CheckLiveInterval: 10,
			Streamers: []twitch.StreamerConfig{
				twitch.StreamerConfig{UserLogin: "streamers-username"},
				twitch.StreamerConfig{UserID: "0"},
				twitch.StreamerConfig{
					UserLogin: "streamers-username", UserID: "0",
					OutputDirPath:  &exampleStreamerOutputDir,
					ArchiveDirPath: &exampleStreamerArchiveDir},
			},
		},
	}

	err := ensureDirectory(filepath.Dir(cfgPath))
	if err != nil {
		return fmt.Errorf("error checking/creating default config directory: %v", err)
	}

	json, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("Failed to marshal struct into json: %v", err)
	}

	err = os.WriteFile(cfgPath, json, 0644)
	if err != nil {
		return fmt.Errorf("error writing default config file: %v", err)
	}

	return nil
}

func ensureDirectory(path string) error {
	fileInfo, err := os.Stat(path)
	if err == nil {
		if !fileInfo.IsDir() {
			return fmt.Errorf("path exists but is not a directory: %s", path)
		}
		return nil
	}

	if os.IsNotExist(err) {
		// Create directory and all parent directories
		err = os.MkdirAll(path, 0755)
		if err != nil {
			return fmt.Errorf("failed to create directory(s): %v", err)
		}
		return nil
	}

	return fmt.Errorf("error checking path: %v", err)
}
