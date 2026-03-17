package puller

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker-stealer/go-pull/pkg/image"
	"github.com/docker-stealer/go-pull/pkg/proxy"
	"github.com/docker-stealer/go-pull/pkg/registry"
)

// Options configures the pull operation.
type Options struct {
	ImageRef   string       // Docker image reference
	ProxyURL   string       // Proxy URL (socks5://..., http://...)
	Insecure   bool         // Skip TLS verification
	ResumeDir  string       // Directory to resume from (if empty, no resume)
	OutputDir  string       // Directory to store image files (default: current dir)
	NoExtract  bool         // Skip gzip extraction (keep compressed)
	OnProgress ProgressFunc // Progress callback
}

// ProgressFunc is called with download progress updates.
type ProgressFunc func(event ProgressEvent)

// ProgressEvent contains progress information.
type ProgressEvent struct {
	Type        string // "layer_start", "layer_progress", "layer_complete", "layer_extract", "config_done", "assembly_start", "done"
	LayerDigest string
	LayerIndex  int
	Total       int64
	Downloaded  int64
	Message     string
}

// PullResult contains the result of a pull operation.
type PullResult struct {
	ImageDir   string // Path to the extracted image directory
	TarFile    string // Path to the final tar file (if created)
	ImageRef   string // The image reference that was pulled
	TotalSize  int64  // Total bytes downloaded
	LayerCount int    // Number of layers
	Resumed    bool   // Whether this was a resume
}

// Pull pulls a Docker image. Returns result or error.
func Pull(opts Options) (*PullResult, error) {
	// Parse image reference
	ref, err := image.Parse(opts.ImageRef)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}

	// Parse proxy config
	var proxyCfg *proxy.Config
	if opts.ProxyURL != "" {
		proxyCfg, err = proxy.ParseProxyURL(opts.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
	}

	// Create registry client
	client, err := registry.NewClient(ref, proxyCfg, opts.Insecure)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}

	// Determine image directory
	var imgDir string
	if opts.ResumeDir != "" {
		imgDir = opts.ResumeDir
	} else {
		imgDir = ref.DirName()
		if opts.OutputDir != "" {
			imgDir = filepath.Join(opts.OutputDir, imgDir)
		}
	}

	// Check for existing checkpoint
	cp, err := LoadCheckpoint(imgDir)
	isResume := err == nil && cp != nil

	if isResume {
		progress(opts.OnProgress, ProgressEvent{
			Type:    "message",
			Message: fmt.Sprintf("Resuming download in: %s", imgDir),
		})
	} else {
		// Create directory
		if err := os.MkdirAll(imgDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", imgDir, err)
		}
	}

	// Fetch manifest
	var manifest *registry.Manifest
	var manifestList *registry.ManifestList

	if isResume && cp.ConfigDigest != "" {
		// On resume, we already have manifest info
		manifest, _, err = client.FetchManifest()
		if err != nil {
			return nil, fmt.Errorf("failed to fetch manifest: %w", err)
		}
	} else {
		manifest, manifestList, err = client.FetchManifest()
		if err != nil {
			return nil, fmt.Errorf("failed to fetch manifest: %w", err)
		}
	}

	// Handle manifest list (multi-arch)
	if manifestList != nil {
		availablePlatforms := make([]string, 0, len(manifestList.Manifests))
		for _, m := range manifestList.Manifests {
			platform := fmt.Sprintf("%s/%s", m.Platform["os"], m.Platform["architecture"])
			availablePlatforms = append(availablePlatforms, fmt.Sprintf("  %s (digest: %s)", platform, registry.ShortDigest(m.Digest)))
		}
		return nil, fmt.Errorf("multi-platform image found. Specify a platform-specific digest:\n%s\nUse: @digest format",
			strings.Join(availablePlatforms, "\n"))
	}

	if manifest == nil {
		return nil, fmt.Errorf("no manifest found")
	}

	// Set up checkpoint for new pull
	if !isResume {
		cp = NewCheckpoint(imgDir, opts.ImageRef)
		cp.Registry = ref.Registry
		cp.Repository = ref.Repository
		cp.Tag = ref.Tag
		cp.ConfigDigest = manifest.Config.Digest

		// Set up layer tracking
		layers := make([]LayerProgress, len(manifest.Layers))
		for i, layer := range manifest.Layers {
			layers[i] = LayerProgress{
				Digest:    layer.Digest,
				TotalSize: layer.Size,
			}
			if len(layer.URLs) > 0 {
				layers[i].ExternalURL = layer.URLs[0]
			}
		}
		cp.SetAllLayers(layers)
		if err := cp.Save(); err != nil {
			return nil, fmt.Errorf("failed to save initial checkpoint: %w", err)
		}
	}

	// Download config blob
	configPath := filepath.Join(imgDir, fmt.Sprintf("%s.json", registry.ShortDigest(cp.ConfigDigest)))
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		progress(opts.OnProgress, ProgressEvent{
			Type:    "config_start",
			Message: fmt.Sprintf("Downloading config %s...", registry.ShortDigest(cp.ConfigDigest)),
		})

		configData, err := client.FetchConfig(cp.ConfigDigest)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch config: %w", err)
		}

		if err := os.WriteFile(configPath, configData, 0644); err != nil {
			return nil, fmt.Errorf("failed to write config: %w", err)
		}

		progress(opts.OnProgress, ProgressEvent{
			Type:    "config_done",
			Message: fmt.Sprintf("Config %s downloaded", registry.ShortDigest(cp.ConfigDigest)),
		})
	}

	// Parse config for later use
	var configJSON map[string]interface{}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	json.Unmarshal(configData, &configJSON)

	// Download layers
	parentID := ""
	var layerIDs []string
	var totalDownloaded int64
	totalLayers := len(cp.Layers)

	for i, layerProgress := range cp.Layers {
		// Compute fake layer ID (matching Docker's format)
		fakeID := computeLayerID(parentID, layerProgress.Digest)
		layerIDs = append(layerIDs, fakeID)
		layerDir := filepath.Join(imgDir, fakeID)

		// Check if layer already complete
		if layerProgress.Completed {
			progress(opts.OnProgress, ProgressEvent{
				Type:        "layer_complete",
				LayerDigest: layerProgress.Digest,
				LayerIndex:  i,
				Message:     fmt.Sprintf("[%d/%d] %s: Already downloaded", i+1, totalLayers, registry.ShortDigest(layerProgress.Digest)),
			})
			parentID = fakeID
			continue
		}

		// Create layer directory
		if err := os.MkdirAll(layerDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create layer dir: %w", err)
		}

		// Create VERSION file
		if err := os.WriteFile(filepath.Join(layerDir, "VERSION"), []byte("1.0"), 0644); err != nil {
			return nil, fmt.Errorf("failed to write VERSION: %w", err)
		}

		gzipPath := filepath.Join(layerDir, "layer_gzip.tar")
		tarPath := filepath.Join(layerDir, "layer.tar")

		// Check for partial download (resume)
		existingSize := GetExistingDownloadSize(gzipPath)
		var downloaded int64 = existingSize

		progress(opts.OnProgress, ProgressEvent{
			Type:        "layer_start",
			LayerDigest: layerProgress.Digest,
			LayerIndex:  i,
			Total:       layerProgress.TotalSize,
			Downloaded:  existingSize,
			Message:     fmt.Sprintf("[%d/%d] %s: Downloading...", i+1, totalLayers, registry.ShortDigest(layerProgress.Digest)),
		})

		// Fetch blob
		var resp *http.Response
		if layerProgress.ExternalURL != "" {
			resp, err = client.FetchBlobURL(layerProgress.ExternalURL)
		} else {
			resp, err = client.FetchBlob(layerProgress.Digest)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to fetch layer %s: %w", registry.ShortDigest(layerProgress.Digest), err)
		}

		layerSize := resp.ContentLength

		// Download with progress tracking
		var file *os.File
		if existingSize > 0 && existingSize < layerSize {
			// Resume: append to existing file
			file, err = os.OpenFile(gzipPath, os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("failed to open file for resume: %w", err)
			}
			// Skip already downloaded bytes from response
			// Note: HTTP Range request would be better but Docker registry may not support it
			// For now, we rely on the temp file
			progress(opts.OnProgress, ProgressEvent{
				Type:    "layer_progress",
				Message: fmt.Sprintf("  Resuming from %d bytes", existingSize),
			})
		} else {
			// Fresh download
			file, err = os.Create(gzipPath)
			if err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("failed to create file: %w", err)
			}
		}

		buf := make([]byte, 32*1024) // 32KB buffer
		lastSave := time.Now()

		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := file.Write(buf[:n]); writeErr != nil {
					file.Close()
					resp.Body.Close()
					return nil, fmt.Errorf("failed to write layer data: %w", writeErr)
				}
				downloaded += int64(n)

				// Update checkpoint periodically (every 100ms)
				if time.Since(lastSave) > 100*time.Millisecond {
					cp.UpdateLayer(layerProgress.Digest, downloaded)
					lastSave = time.Now()
				}

				progress(opts.OnProgress, ProgressEvent{
					Type:        "layer_progress",
					LayerDigest: layerProgress.Digest,
					LayerIndex:  i,
					Total:       layerProgress.TotalSize,
					Downloaded:  downloaded,
				})
			}

			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				file.Close()
				resp.Body.Close()
				// Save checkpoint so we can resume
				cp.UpdateLayer(layerProgress.Digest, downloaded)
				return nil, fmt.Errorf("failed to read layer data: %w (can resume from %s)", readErr, imgDir)
			}
		}

		file.Close()
		resp.Body.Close()
		totalDownloaded += downloaded

		progress(opts.OnProgress, ProgressEvent{
			Type:        "layer_complete",
			LayerDigest: layerProgress.Digest,
			LayerIndex:  i,
			Total:       layerProgress.TotalSize,
			Downloaded:  downloaded,
			Message:     fmt.Sprintf("[%d/%d] %s: Download complete [%d bytes]", i+1, totalLayers, registry.ShortDigest(layerProgress.Digest), downloaded),
		})

		// Extract gzip
		if !opts.NoExtract {
			progress(opts.OnProgress, ProgressEvent{
				Type:        "layer_extract",
				LayerDigest: layerProgress.Digest,
				LayerIndex:  i,
				Message:     fmt.Sprintf("[%d/%d] %s: Extracting...", i+1, totalLayers, registry.ShortDigest(layerProgress.Digest)),
			})

			if err := extractGzip(gzipPath, tarPath); err != nil {
				return nil, fmt.Errorf("failed to extract layer %s: %w", registry.ShortDigest(layerProgress.Digest), err)
			}

			// Remove gzipped file to save space
			os.Remove(gzipPath)
		} else {
			// Rename gzipped file
			os.Rename(gzipPath, tarPath)
		}

		// Create layer json file
		layerJSON := createLayerJSON(fakeID, parentID, configJSON, layerProgress.Digest == cp.Layers[len(cp.Layers)-1].Digest)
		jsonPath := filepath.Join(layerDir, "json")
		jsonData, _ := json.Marshal(layerJSON)
		if err := os.WriteFile(jsonPath, jsonData, 0644); err != nil {
			return nil, fmt.Errorf("failed to write layer json: %w", err)
		}

		// Mark layer complete in checkpoint
		cp.SetLayerComplete(layerProgress.Digest, fakeID, tarPath)

		progress(opts.OnProgress, ProgressEvent{
			Type:        "layer_complete",
			LayerDigest: layerProgress.Digest,
			LayerIndex:  i,
			Message:     fmt.Sprintf("[%d/%d] %s: Pull complete", i+1, totalLayers, registry.ShortDigest(layerProgress.Digest)),
		})

		parentID = fakeID
	}

	// Build manifest.json
	progress(opts.OnProgress, ProgressEvent{
		Type:    "assembly_start",
		Message: "Assembling image...",
	})

	content := []map[string]interface{}{
		{
			"Config":   fmt.Sprintf("%s.json", registry.ShortDigest(cp.ConfigDigest)),
			"RepoTags": []string{ref.ImageName()},
			"Layers":   layerIDsToPaths(layerIDs),
		},
	}

	manifestJSON, _ := json.Marshal(content)
	if err := os.WriteFile(filepath.Join(imgDir, "manifest.json"), manifestJSON, 0644); err != nil {
		return nil, fmt.Errorf("failed to write manifest.json: %w", err)
	}

	// Build repositories
	repositories := map[string]map[string]string{
		ref.ImageName(): {
			ref.Tag: layerIDs[len(layerIDs)-1],
		},
	}
	repoJSON, _ := json.Marshal(repositories)
	if err := os.WriteFile(filepath.Join(imgDir, "repositories"), repoJSON, 0644); err != nil {
		return nil, fmt.Errorf("failed to write repositories: %w", err)
	}

	// Mark checkpoint complete
	cp.MarkComplete()
	cp.Save()

	progress(opts.OnProgress, ProgressEvent{
		Type:    "done",
		Message: fmt.Sprintf("Image pulled successfully: %s", imgDir),
	})

	return &PullResult{
		ImageDir:   imgDir,
		ImageRef:   ref.String(),
		TotalSize:  totalDownloaded,
		LayerCount: len(cp.Layers),
		Resumed:    isResume,
	}, nil
}

// computeLayerID computes the fake layer ID matching Docker's algorithm.
func computeLayerID(parentID, digest string) string {
	input := parentID + "\n" + digest + "\n"
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h)
}

// extractGzip decompresses a gzip file.
func extractGzip(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("gzip reader error: %w", err)
	}
	defer gz.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, gz)
	return err
}

// createLayerJSON creates the json metadata for a layer.
func createLayerJSON(id, parentID string, config map[string]interface{}, isLastLayer bool) map[string]interface{} {
	if isLastLayer {
		// Last layer: use config manifest minus history and rootfs
		result := make(map[string]interface{})
		for k, v := range config {
			if k == "history" || k == "rootfs" || k == "rootfS" {
				continue
			}
			result[k] = v
		}
		result["id"] = id
		if parentID != "" {
			result["parent"] = parentID
		}
		return result
	}

	// Other layers: empty json (matching Python implementation)
	return map[string]interface{}{
		"created":          "1970-01-01T00:00:00Z",
		"id":               id,
		"parent":           parentID,
		"container_config": map[string]interface{}{},
	}
}

// layerIDsToPaths converts layer IDs to Docker tar format paths.
func layerIDsToPaths(ids []string) []string {
	paths := make([]string, len(ids))
	for i, id := range ids {
		paths[i] = id + "/layer.tar"
	}
	return paths
}

// progress sends a progress event if a callback is set.
func progress(fn ProgressFunc, event ProgressEvent) {
	if fn != nil {
		fn(event)
	}
}

// PullAsTar is a convenience function that pulls an image and creates a tar archive.
func PullAsTar(opts Options) (string, error) {
	result, err := Pull(opts)
	if err != nil {
		return "", err
	}

	// Create tar archive
	tarName := filepath.Base(result.ImageDir) + ".tar"
	tarPath := tarName
	if opts.OutputDir != "" {
		tarPath = filepath.Join(opts.OutputDir, tarName)
	}

	progress(opts.OnProgress, ProgressEvent{
		Type:    "tar_start",
		Message: fmt.Sprintf("Creating archive %s...", tarPath),
	})

	if err := createTar(result.ImageDir, tarPath); err != nil {
		return "", fmt.Errorf("failed to create tar: %w", err)
	}

	// Clean up temporary directory
	os.RemoveAll(result.ImageDir)

	// Remove checkpoint
	RemoveCheckpoint(result.ImageDir)

	return tarPath, nil
}

// createTar creates a Docker-compatible tar archive from a directory.
func createTar(sourceDir, tarPath string) error {
	return createTarArchive(sourceDir, tarPath)
}
