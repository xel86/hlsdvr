package disk

// From /proc/mounts on linux.
type FilesystemInfo struct {
	Device     string
	MountPoint string
	FSType     string
	Options    string
}

type DiskUtilization struct {
	TotalBytes uint64
	FreeBytes  uint64
	UsedBytes  uint64
}

// Get a string identifier for the underlying physical device/drive for the path.
func GetDriveIdentifier(path string) (string, error) {
	return getDriveIdentifierImpl(path)
}

// Get drive utilization for a path.
// returned in order:  total bytes, used bytes, free bytes.
func GetDriveUtilization(path string) (DiskUtilization, error) {
	return getDriveUtilizationImpl(path)
}

func GetMountPointForPath(path string) (string, error) {
	return getMountPointForPathImpl(path)
}
