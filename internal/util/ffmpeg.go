package util

import (
	"fmt"
	"os"
	"os/exec"
)

// Remux the video file at the source path into a newly created video file at the dest path.
// The source and dest path should only different by the file extension at the end.
// This function will return the size of the newly output remuxed file in bytes if successful.
// An error will be returned ffmpeg had an error, or the output file failed to validate.
func RemuxFile(sourcePath string, destPath string) (uint64, error) {
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("original source file %s does not exist", sourcePath)
		} else {
			return 0, fmt.Errorf("unable to stat() original source file with error: %v", err)
		}
	}

	cmd := exec.Command("ffmpeg",
		"-i", sourcePath,
		"-c", "copy",
		destPath,
	)
	// Suppress all output (stdout and stderr)
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Execute the command
	err = cmd.Run()
	if err != nil {
		return 0, fmt.Errorf("ffmpeg command failed: %v", err)
	}
	if !cmd.ProcessState.Success() {
		return 0, fmt.Errorf("ffmpeg process exited with non-zero status")
	}

	// Validate that remuxed output file was created and has content
	outputInfo, err := os.Stat(destPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("output file %s was not created.", destPath)
		} else {
			return 0, fmt.Errorf(
				"unable to stat() dest file with error: %v",
				err)
		}
	}

	if outputInfo.Size() == 0 {
		return 0, fmt.Errorf("output file %s is empty", destPath)
	}

	// This is paranoid but just to double ensure we do not accidentally delete the original footage
	// without a proper remuxed output file, we will compare the source and newly made output file.
	// Check if output file size is reasonable (within 20% of source file size for remux)
	// This is an arbitrary percentage but since remuxing is lossless and ts -> mkv is generally
	// going to have a size reduction of about 5%, 20% is probably a bit wide but reasonable enough.
	sizeDiff := float64(outputInfo.Size()) / float64(sourceInfo.Size())
	if sizeDiff < 0.8 || sizeDiff > 1.2 {
		return uint64(outputInfo.Size()),
			fmt.Errorf("output file size seems abnormal (source: %s, output: %s)",
				HumanReadableBytes(uint64(sourceInfo.Size())), HumanReadableBytes(uint64(outputInfo.Size())))
	}

	return uint64(outputInfo.Size()), nil
}
