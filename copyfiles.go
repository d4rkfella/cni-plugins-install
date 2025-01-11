// copyfiles.go
package main

import (
	"fmt"
	"io"
	"os"
	"log"
	"path/filepath"
)

func copyFile(srcPath, dstPath string) error {
	// Open the source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Create the destination file
	dstFile, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Copy the contents of the source file to the destination file
	_, err = io.Copy(dstFile, srcFile)
	return err
}

func main() {
	srcDir := "/path/to/source/"
	dstDir := "/shared-data/"

	// Read the source directory and copy each file
	err := filepath.Walk(srcDir, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip directories
		if info.IsDir() {
			return nil
		}
		dstPath := filepath.Join(dstDir, info.Name())
		err = copyFile(srcPath, dstPath)
		if err != nil {
			log.Printf("Error copying %s to %s: %v", srcPath, dstPath, err)
		} else {
			log.Printf("Copied %s to %s", srcPath, dstPath)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Error walking the source directory: %v", err)
	}
}
