package artifact

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/archive"
	"github.com/darkfella/cni-plugins-install/pkg/checksum"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/fs"
	httpClient "github.com/darkfella/cni-plugins-install/pkg/http"
	"github.com/darkfella/cni-plugins-install/pkg/validator"
	"golang.org/x/sync/errgroup"
)

type Downloader struct {
	logger     *logging.Logger
	httpClient *httpClient.Client
	fileSystem fs.FileSystem
	cleanup    *fs.Cleanup
	config     *Config
	stagingDir string
	extractor  *archive.Extractor
	validator  *validator.Validator
}

type Config struct {
	BaseURL         string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
}

func NewDownloader(logger *logging.Logger, client *httpClient.Client, config *Config) *Downloader {
	if client == nil {
		client = httpClient.NewClient(logger, &http.Client{}, &httpClient.Config{
			DownloadTimeout: config.DownloadTimeout,
			MaxRetries:      config.MaxRetries,
			BufferSize:      config.BufferSize,
		})
	}
	return &Downloader{
		logger:     logger,
		httpClient: client,
		fileSystem: fs.NewFileSystem(logger),
		cleanup:    fs.NewCleanup(logger),
		config:     config,
		extractor:  archive.NewExtractor(logger),
		validator:  validator.NewValidator(logger),
	}
}

func (d *Downloader) Cleanup() error {
	return d.cleanup.Execute(d.fileSystem)
}

func (d *Downloader) DownloadAndExtract(ctx context.Context, version, targetDir string) error {
	if err := d.validator.ValidateVersion(version); err != nil {
		return err
	}
	if err := d.validator.ValidateDirectory(targetDir); err != nil {
		return err
	}

	stagingDir := filepath.Join(targetDir, fmt.Sprintf(".cni-staging-%d", rand.Intn(1000000)))
	if err := d.fileSystem.CreateDirectory(stagingDir, constants.DirPerm); err != nil {
		return errors.Wrap(err, "create staging directory")
	}
	d.stagingDir = stagingDir

	downloadsDir := filepath.Join(stagingDir, "downloads")
	if err := d.fileSystem.CreateDirectory(downloadsDir, constants.DirPerm); err != nil {
		return errors.Wrap(err, "create downloads directory")
	}

	d.cleanup.AddDirectory(downloadsDir)
	d.cleanup.AddDirectory(stagingDir)

	d.logger.Debug().Str("staging_dir", stagingDir).Msg("Staging directory created")

	archiveName := fmt.Sprintf("cni-plugins-linux-amd64-%s.tgz", version)
	shaName := fmt.Sprintf("%s.sha256", archiveName)
	archiveURL := fmt.Sprintf("%s/%s/%s", d.config.BaseURL, version, archiveName)
	shaURL := fmt.Sprintf("%s/%s/%s", d.config.BaseURL, version, shaName)

	if err := d.validator.ValidateURL(archiveURL); err != nil {
		return err
	}
	if err := d.validator.ValidateURL(shaURL); err != nil {
		return err
	}

	archivePath := filepath.Join(downloadsDir, archiveName)
	shaPath := filepath.Join(downloadsDir, shaName)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		tmpFile, err := os.Create(archivePath)
		if err != nil {
			return errors.Wrap(err, "create archive temp file")
		}
		defer func() {
			if closeErr := tmpFile.Close(); closeErr != nil && err == nil {
				err = errors.Wrap(closeErr, "close archive temp file")
			}
		}()

		d.logger.Debug().Str("url", archiveURL).Msg("Downloading archive")
		if err := d.httpClient.DownloadFile(gCtx, archiveURL, tmpFile); err != nil {
			return err
		}

		size, err := d.fileSystem.GetFileSize(archivePath)
		if err != nil {
			return errors.Wrap(err, "get archive size")
		}

		d.logger.Info().Str("file", archivePath).Int64("size", size).Msg("Archive downloaded")
		return nil
	})

	g.Go(func() (err error) {
		tmpFile, err := os.Create(shaPath)
		if err != nil {
			return errors.Wrap(err, "create sha temp file")
		}
		defer func() {
			if closeErr := tmpFile.Close(); closeErr != nil && err == nil {
				err = errors.Wrap(closeErr, "close sha temp file")
			}
		}()

		d.logger.Debug().Str("url", shaURL).Msg("Downloading SHA256 checksum")
		if err := d.httpClient.DownloadFile(gCtx, shaURL, tmpFile); err != nil {
			return err
		}

		d.logger.Info().Str("file", shaPath).Msg("SHA256 checksum downloaded")
		return nil
	})

	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "download failed")
	}

	if err := d.verifyChecksum(ctx, archivePath, shaPath); err != nil {
		return errors.Wrap(err, "checksum verification failed")
	}

	if err := d.extractor.Extract(ctx, archivePath, stagingDir); err != nil {
		return errors.Wrap(err, "archive extraction failed")
	}

	return nil
}

func (d *Downloader) verifyChecksum(ctx context.Context, filePath, shaPath string) error {
	if err := d.validator.ValidateFile(filePath); err != nil {
		return err
	}
	if err := d.validator.ValidateFile(shaPath); err != nil {
		return err
	}

	expectedHash, err := checksum.ReadSHAFile(shaPath)
	if err != nil {
		return errors.Wrap(err, "read sha file")
	}

	if err := checksum.VerifyFileSHA256(ctx, filePath, expectedHash); err != nil {
		return errors.Wrap(err, "verify checksum")
	}

	d.logger.Info().Msg("Checksum verification successful")
	return nil
}

func (d *Downloader) StagingDir() string {
	return d.stagingDir
}
