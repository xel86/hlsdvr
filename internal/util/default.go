package util

import (
	"os"
	"path/filepath"
	"runtime"
)

// returns the platform-specific default configuration directory
func GetDefaultConfigDir(dirName string) string {
	var configDir string

	switch runtime.GOOS {
	case "windows":
		configDir = os.Getenv("APPDATA")
		if configDir == "" {
			userProfile := os.Getenv("USERPROFILE")
			if userProfile != "" {
				configDir = filepath.Join(userProfile, "AppData", "Roaming")
			} else {
				// Just put it in the current working directory as last resort
				configDir = ""
			}
		}
		configDir = filepath.Join(configDir, dirName)

	case "darwin": // macOS
		configDir = os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			// Standard macOS location: ~/Library/Application Support/
			home := os.Getenv("HOME")
			if home == "" {
				// Just put it in the current working directory as last resort
				configDir = ""
			} else {
				configDir = filepath.Join(home, "Library", "Application Support")
			}
		}
		configDir = filepath.Join(configDir, dirName)

	default: // Linux and others following XDG spec
		configDir = os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			// Standard XDG fallback: ~/.config/
			home := os.Getenv("HOME")
			if home == "" {
				// Just put it in the current working directory as last resort
				configDir = ""
			} else {
				configDir = filepath.Join(home, ".config")
			}
		}
		configDir = filepath.Join(configDir, dirName)
	}

	return configDir
}

// returns a default location for the unix socket file for the rpc server
func GetDefaultSocketPath(fileName string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.TempDir(), fileName)
	case "darwin":
		return filepath.Join("/var/run", fileName)
	default: // Linux and others following XDG spec
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir != "" {
			return filepath.Join(dir, fileName)
		}
		return filepath.Join("/tmp", fileName)
	}
}
