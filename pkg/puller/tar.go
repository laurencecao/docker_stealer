package puller

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// createTarArchive creates a Docker-compatible tar archive from a directory.
// This uses Go's archive/tar for cross-platform compatibility.
// The archive contents are placed at the root (like Python's arcname=os.path.sep).
func createTarArchive(sourceDir, tarPath string) error {
	outFile, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	tw := tar.NewWriter(outFile)
	defer tw.Close()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		// Skip the root directory itself
		if relPath == "." {
			return nil
		}

		// Convert to forward slashes (tar standard)
		headerName := strings.ReplaceAll(relPath, "\\", "/")

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = headerName

		// Write header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// If it's a regular file, write the content
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = io.Copy(tw, file)
			return err
		}

		return nil
	})
}
