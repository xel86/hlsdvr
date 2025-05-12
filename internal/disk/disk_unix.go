//go:build unix
// +build unix

package disk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func getDriveIdentifierImpl(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	var stat unix.Stat_t
	if err := unix.Stat(absPath, &stat); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("failed to stat path: %w", err)
	}

	major := unix.Major(stat.Dev)
	minor := unix.Minor(stat.Dev)

	return fmt.Sprintf("dev-%X-%X", major, minor), nil
}

func getDriveUtilizationImpl(path string) (DiskUtilization, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return DiskUtilization{}, err
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(absPath, &stat); err != nil {
		return DiskUtilization{}, fmt.Errorf("failed to get filesystem stats: %w", err)
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free

	return DiskUtilization{
		TotalBytes: total,
		FreeBytes:  free,
		UsedBytes:  used,
	}, nil
}

func getMountPointForPathImpl(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	switch runtime.GOOS {
	case "darwin": // TODO: macOS doesn't have /proc/mounts, do something else that isn't ugly?
		return "", nil
	default: // linux, freebsd, etc.
		mounts, err := getMountInfos()
		if err != nil {
			return "", fmt.Errorf("Failed to get mount points: %v", err)
		}

		// Find the longest matching mount point
		var bestMatch *FilesystemInfo
		bestMatchLen := 0
		for _, info := range mounts {
			if strings.HasPrefix(absPath, info.MountPoint) && len(info.MountPoint) > bestMatchLen {
				bestMatch = info
				bestMatchLen = len(info.MountPoint)
			}
		}

		if bestMatch == nil {
			return "", fmt.Errorf("no filesystem mount point found for path %s", path)
		}

		return bestMatch.MountPoint, nil
	}
}

// returns information about all mount points on the system
func getMountInfos() ([]*FilesystemInfo, error) {
	content, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(content), "\n")
	var mountInfos []*FilesystemInfo

	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		info := &FilesystemInfo{
			Device:     fields[0],
			MountPoint: fields[1],
			FSType:     fields[2],
			Options:    fields[3],
		}

		mountInfos = append(mountInfos, info)
	}

	return mountInfos, nil
}
