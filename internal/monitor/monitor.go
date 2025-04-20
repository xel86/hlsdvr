package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/xel86/hlsdvr/internal/config"
	"github.com/xel86/hlsdvr/internal/platform"
)

// Watch the config file in use for any changes.
// Read and broadcast the new config to all platforms to update their runtime config.
func ConfigFileChanges(ctx context.Context, cfgPath string, pcs *platform.CommandSender) {
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
					cfg, err := config.ReadConfig(cfgPath)
					if err != nil {
						slog.Error(fmt.Sprintf(
							"Error reading config (%s): %v. Ignoring and using current config...",
							cfgPath, err))
						continue
					}
					pcs.Broadcast(platform.CommandMsg{Type: platform.CmdConfigReload, Value: cfg})
					slog.Debug("Broadcasted CmdConfigReload Message for all platforms.")
				}
			}
		}
	}
}
