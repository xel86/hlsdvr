package util

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func IsFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func MoveFile(sourcePath string, destDirPath string, destFileName string) error {
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't open source file: %v", err)
	}
	defer inputFile.Close()

	if err := os.MkdirAll(destDirPath, 0755); err != nil {
		return fmt.Errorf("error creating directory %s to move/archive stream(s) into: %v",
			destDirPath, err)
	}

	// don't open a file if it already exists.
	destPath := filepath.Join(destDirPath, destFileName)
	outputFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("Couldn't create dest file: %v", err)
	}
	defer outputFile.Close()

	_, err = io.Copy(outputFile, inputFile)
	if err != nil {
		return fmt.Errorf("Couldn't copy to dest from source: %v", err)
	}

	// for Windows, close before trying to remove: https://stackoverflow.com/a/64943554/246801
	inputFile.Close()

	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't remove source file: %v", err)
	}

	return nil
}
