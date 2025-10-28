package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/fs"
)

type Extractor struct {
	logger     *logging.Logger
	fileSystem fs.FileSystem
}

func NewExtractor(logger *logging.Logger) *Extractor {
	return &Extractor{
		logger:     logger,
		fileSystem: fs.NewFileSystem(logger),
	}
}

func (e *Extractor) Extract(ctx context.Context, archivePath, targetDir string) error {
	switch {
	case strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz"):
		return e.extractTarGz(ctx, archivePath, targetDir)
	case filepath.Ext(archivePath) == ".zip":
		return e.extractZip(ctx, archivePath, targetDir)
	default:
		ext := filepath.Ext(archivePath)
		if strings.Contains(archivePath, ".") {
			ext = archivePath[strings.LastIndex(archivePath, "."):]
		}
		return errors.NewOperationError("extract archive", fmt.Errorf("unsupported archive format: %s", ext))
	}
}

func (e *Extractor) extractTarGz(ctx context.Context, src, dst string) (err error) {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	file, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "open archive")
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close archive file")
		}
	}()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return errors.Wrap(err, "create gzip reader")
	}
	defer func() {
		if closeErr := gzr.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close gzip reader")
		}
	}()

	tr := tar.NewReader(gzr)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.Wrap(err, "read tar header")
		}

		if header.Typeflag == tar.TypeReg {
			target := filepath.Join(dst, filepath.Base(header.Name))
			if err := e.fileSystem.WriteFileAtomic(target, tr, header.FileInfo().Mode()); err != nil {
				return errors.Wrap(err, fmt.Sprintf("write file %s", header.Name))
			}
			e.logger.Info().Str("file", target).Msg("Extracted file from archive")
		}
	}

	return nil
}

func (e *Extractor) extractZip(ctx context.Context, src, dst string) (err error) {
	reader, err := zip.OpenReader(src)
	if err != nil {
		return errors.Wrap(err, "open zip file")
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close zip reader")
		}
	}()

	for _, file := range reader.File {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		target := filepath.Join(dst, filepath.Base(file.Name))
		if err := e.extractZipEntry(file, target); err != nil {
			return errors.Wrap(err, fmt.Sprintf("extract zip entry %s", file.Name))
		}
	}

	return nil
}

func (e *Extractor) extractZipEntry(file *zip.File, targetPath string) (err error) {
	if file.FileInfo().IsDir() {
		if err := e.fileSystem.CreateDirectory(targetPath, file.FileInfo().Mode()); err != nil {
			return errors.Wrap(err, "create directory")
		}
		return nil
	}

	reader, err := file.Open()
	if err != nil {
		return errors.Wrap(err, "open zip entry")
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close zip entry reader")
		}
	}()

	if err := e.fileSystem.WriteFileAtomic(targetPath, reader, file.FileInfo().Mode()); err != nil {
		return errors.Wrap(err, "write file")
	}

	return nil
}
