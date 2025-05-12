//go:build windows
// +build windows

package disk

import (
	"fmt"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func getDriveIdentifierImpl(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	pathPtr, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		return "", fmt.Errorf("Failed to convert path string to UTF16 pointer: %w", err)
	}

	var volumePathName [windows.MAX_PATH + 1]uint16
	err = windows.GetVolumePathName(pathPtr, &volumePathName[0], windows.MAX_PATH)
	if err != nil {
		if syserr, ok := err.(syscall.Errno); ok {
			if (syserr == windows.ERROR_PATH_NOT_FOUND) || (syserr == windows.ERROR_FILE_NOT_FOUND) {
				return "", nil
			}
		}

		return "", fmt.Errorf("Failed to get volume path name: %w", err)
	}

	var volumeNameBuffer [windows.MAX_PATH + 1]uint16
	var volumeSerialNumber uint32
	var maximumComponentLength uint32
	var fileSystemFlags uint32
	var fileSystemNameBuffer [windows.MAX_PATH + 1]uint16

	err = windows.GetVolumeInformation(
		&volumePathName[0],
		&volumeNameBuffer[0],
		windows.MAX_PATH,
		&volumeSerialNumber,
		&maximumComponentLength,
		&fileSystemFlags,
		&fileSystemNameBuffer[0],
		windows.MAX_PATH)
	if err != nil {
		return "", fmt.Errorf("Failed to get volume information from root path: %w", err)
	}

	return fmt.Sprintf("dev-%d", volumeSerialNumber), nil
}

func getDriveUtilizationImpl(path string) (DiskUtilization, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return DiskUtilization{}, err
	}

	pathPtr, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		return DiskUtilization{}, fmt.Errorf("Failed to convert string to UTF16 pointer: %w", err)
	}

	var freeUser, total, freeTotal uint64

	err = windows.GetDiskFreeSpaceEx(pathPtr, &freeUser, &total, &freeTotal)
	if err != nil {
		return DiskUtilization{}, fmt.Errorf("Failed to get free space available on disk: %w", err)
	}

	return DiskUtilization{
		TotalBytes: total,
		FreeBytes:  freeUser,
		UsedBytes:  total - freeTotal,
	}, nil
}

func getMountPointForPathImpl(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	pathPtr, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		return "", fmt.Errorf("Failed to convert string to UTF16 pointer: %w", err)
	}

	var volumePathName [windows.MAX_PATH + 1]uint16

	err = windows.GetVolumePathName(pathPtr, &volumePathName[0], windows.MAX_PATH)
	if err != nil {
		return "", fmt.Errorf("Failed to get volume path name: %w", err)
	}

	return windows.UTF16ToString(volumePathName[:]), nil
}
