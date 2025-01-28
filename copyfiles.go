package main

import (
	"fmt"
	"io"
	"os"
	"log"
	"path/filepath"
)

func copyFile(srcPath, dstPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if _, err := os.Stat(dstPath); err == nil {
		fmt.Printf("Overwriting file: %s\n", dstPath)
	} else {
		fmt.Printf("Creating new file: %s\n", dstPath)
	}

	dstFile, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func main() {
	srcDir := "/opt/cni/bin/"
	dstDir := "/host/opt/cni/bin/"

	err := filepath.Walk(srcDir, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
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
