package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// From /proc/mounts on linux.
type FilesystemInfo struct {
	Device     string
	MountPoint string
	FSType     string
	Options    string
}

// Get a string identifier for the underlying physical device/drive for the path.
func GetDriveIdentifier(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	// TODO: add build constraints to use windows syscalls to get accurate windows info.
	switch runtime.GOOS {
	case "windows":
		// On Windows, the drive letter at the root of a path is the drive identifier
		return strings.ToUpper(filepath.VolumeName(absPath)), nil
	default: // linux, macOS, freebsd, etc.
		fileInfo, err := os.Stat(absPath)
		if err != nil {
			return "", err
		}
		stat, ok := fileInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return "", fmt.Errorf("failed to get system stat info for %s", absPath)
		}
		return fmt.Sprintf("dev-%d", stat.Dev), nil
	}
}

// Get drive utilization for a path.
// returned in order:  total bytes, used bytes, free bytes.
func GetDriveUtilization(path string) (uint64, uint64, uint64, error) {
	var total, free, used uint64
	var err error

	switch runtime.GOOS {
	case "windows":
		// TODO: add build constraints to use windows syscalls to use windows.GetDiskFreeSpaceEx()
		return 0, 0, 0, nil
	default: // linux, macOS, freebsd, etc.
		var stat syscall.Statfs_t
		err = syscall.Statfs(path, &stat)
		if err != nil {
			return 0, 0, 0, err
		}

		// Calculate disk space in bytes
		total = stat.Blocks * uint64(stat.Bsize)
		free = stat.Bfree * uint64(stat.Bsize)
		used = total - free
	}

	return total, used, free, nil
}

func GetMountPointForPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	// TODO: support windows & macOS getting root/mount dir path
	switch runtime.GOOS {
	case "windows":
		return "", nil
	case "darwin":
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
