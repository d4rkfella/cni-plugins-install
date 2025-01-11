// copyfiles.go
package main

import (
	"fmt"   // Import fmt to use for printing messages
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

	// Check if the destination file already exists
	if _, err := os.Stat(dstPath); err == nil {
		// If the file exists, overwrite it
		fmt.Printf("Overwriting file: %s\n", dstPath)  // Use fmt to print message
	} else {
		// If the file doesn't exist, create it
		fmt.Printf("Creating new file: %s\n", dstPath)  // Use fmt to print message
	}

	// Create or overwrite the destination file
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
	srcDir := "/opt/cni/bin/"
	dstDir := "/host/opt/cni/bin/"

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
