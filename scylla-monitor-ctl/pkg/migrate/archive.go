package migrate

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PackArchive creates a tar.gz archive from a source directory.
func PackArchive(sourceDir, outputPath string) (retErr error) {
	outFile, err := os.Create(outputPath) //nolint:gosec // user-provided output path
	if err != nil {
		return fmt.Errorf("creating archive file: %w", err)
	}
	defer func() {
		if cerr := outFile.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()

	gzWriter := gzip.NewWriter(outFile)
	defer func() {
		if cerr := gzWriter.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()

	tarWriter := tar.NewWriter(gzWriter)
	defer func() {
		if cerr := tarWriter.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("creating tar header for %s: %w", relPath, err)
		}
		header.Name = relPath

		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", relPath, err)
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path) //nolint:gosec // walking known source dir
		if err != nil {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()

		if _, err := io.Copy(tarWriter, f); err != nil {
			return fmt.Errorf("writing %s to archive: %w", relPath, err)
		}

		return nil
	})
}

// UnpackArchive extracts a tar.gz archive to a destination directory.
func UnpackArchive(archivePath, destDir string) error {
	f, err := os.Open(archivePath) //nolint:gosec // user-provided archive path
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gzReader, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("creating gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		// Prevent path traversal
		target := filepath.Join(destDir, header.Name) //nolint:gosec // validated below
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) {
			return fmt.Errorf("invalid tar entry: path traversal detected: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0750); err != nil {
				return fmt.Errorf("creating directory %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return fmt.Errorf("creating parent directory for %s: %w", target, err)
			}
			if err := extractFile(target, tarReader, header.Mode); err != nil {
				return err
			}
		}
	}

	return nil
}

// extractFile writes a single file from the tar reader to disk.
func extractFile(target string, tarReader *tar.Reader, mode int64) error {
	outFile, err := os.Create(target) //nolint:gosec // path validated by caller
	if err != nil {
		return fmt.Errorf("creating file %s: %w", target, err)
	}

	// Limit extraction to 1 GiB to prevent decompression bombs.
	const maxSize = 1 << 30
	if _, err := io.Copy(outFile, io.LimitReader(tarReader, maxSize)); err != nil {
		_ = outFile.Close()
		return fmt.Errorf("extracting %s: %w", target, err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("closing extracted file %s: %w", target, err)
	}
	if err := os.Chmod(target, os.FileMode(mode)&0750); err != nil { //nolint:gosec // mode is from tar header, masked to safe range
		return fmt.Errorf("setting permissions on %s: %w", target, err)
	}
	return nil
}
