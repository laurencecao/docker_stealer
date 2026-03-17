package puller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Checkpoint tracks the download state for resume support.
type Checkpoint struct {
	Image          string          `json:"image"` // Full image reference
	Registry       string          `json:"registry"`
	Repository     string          `json:"repository"`
	Tag            string          `json:"tag"`
	ConfigDigest   string          `json:"config_digest"`
	ManifestDigest string          `json:"manifest_digest,omitempty"`
	Layers         []LayerProgress `json:"layers"`
	Completed      bool            `json:"completed"` // All layers done, ready for assembly
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	mu             sync.Mutex      `json:"-"`
	filePath       string          `json:"-"`
}

// LayerProgress tracks individual layer download state.
type LayerProgress struct {
	Digest         string `json:"digest"`
	TotalSize      int64  `json:"total_size"`
	DownloadedSize int64  `json:"downloaded_size"`
	LayerID        string `json:"layer_id"`  // Computed fake layer ID
	FilePath       string `json:"file_path"` // Where the downloaded blob is
	Extracted      bool   `json:"extracted"` // gzip extracted
	ExternalURL    string `json:"external_url,omitempty"`
	Completed      bool   `json:"completed"`
}

// checkpointFileName is the name of the checkpoint file.
const checkpointFileName = ".checkpoint.json"

// NewCheckpoint creates a new checkpoint for an image.
func NewCheckpoint(dirPath string, imageRef string) *Checkpoint {
	return &Checkpoint{
		Image:     imageRef,
		Layers:    make([]LayerProgress, 0),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		filePath:  filepath.Join(dirPath, checkpointFileName),
	}
}

// LoadCheckpoint loads an existing checkpoint from a directory.
// Returns nil if no checkpoint exists.
func LoadCheckpoint(dirPath string) (*Checkpoint, error) {
	path := filepath.Join(dirPath, checkpointFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	cp.filePath = path
	return &cp, nil
}

// Save writes the checkpoint to disk.
func (cp *Checkpoint) Save() error {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	cp.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint: %w", err)
	}

	// Write atomically using temp file + rename
	tmpPath := cp.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write checkpoint: %w", err)
	}

	return os.Rename(tmpPath, cp.filePath)
}

// UpdateLayer updates progress for a specific layer and saves checkpoint.
func (cp *Checkpoint) UpdateLayer(digest string, downloaded int64) error {
	cp.mu.Lock()
	for i := range cp.Layers {
		if cp.Layers[i].Digest == digest {
			cp.Layers[i].DownloadedSize = downloaded
			break
		}
	}
	cp.mu.Unlock()
	return cp.Save()
}

// SetLayerComplete marks a layer as fully downloaded and extracted.
func (cp *Checkpoint) SetLayerComplete(digest string, layerID string, filePath string) error {
	cp.mu.Lock()
	for i := range cp.Layers {
		if cp.Layers[i].Digest == digest {
			cp.Layers[i].Completed = true
			cp.Layers[i].Extracted = true
			cp.Layers[i].LayerID = layerID
			cp.Layers[i].FilePath = filePath
			cp.Layers[i].DownloadedSize = cp.Layers[i].TotalSize
			break
		}
	}
	cp.mu.Unlock()
	return cp.Save()
}

// GetLayer returns the progress for a specific layer.
func (cp *Checkpoint) GetLayer(digest string) *LayerProgress {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for i := range cp.Layers {
		if cp.Layers[i].Digest == digest {
			return &cp.Layers[i]
		}
	}
	return nil
}

// IsComplete returns true if all layers are fully downloaded.
func (cp *Checkpoint) IsComplete() bool {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if len(cp.Layers) == 0 {
		return false
	}

	for _, layer := range cp.Layers {
		if !layer.Completed {
			return false
		}
	}
	return true
}

// IncompleteLayers returns layers that haven't been fully downloaded.
func (cp *Checkpoint) IncompleteLayers() []LayerProgress {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	var incomplete []LayerProgress
	for _, layer := range cp.Layers {
		if !layer.Completed {
			incomplete = append(incomplete, layer)
		}
	}
	return incomplete
}

// SetAllLayers configures all layers to be downloaded.
func (cp *Checkpoint) SetAllLayers(layers []LayerProgress) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.Layers = layers
}

// MarkComplete marks the entire checkpoint as complete.
func (cp *Checkpoint) MarkComplete() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.Completed = true
}

// RemoveCheckpoint deletes the checkpoint file.
func RemoveCheckpoint(dirPath string) error {
	path := filepath.Join(dirPath, checkpointFileName)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// GetExistingDownloadSize returns the total size of already-downloaded bytes for a layer.
func GetExistingDownloadSize(filePath string) int64 {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0
	}
	return info.Size()
}
