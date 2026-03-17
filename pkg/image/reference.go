package image

import (
	"fmt"
	"strings"
)

// Reference represents a parsed Docker image reference.
type Reference struct {
	Registry   string // e.g. "registry-1.docker.io"
	Repository string // e.g. "library/nginx"
	Image      string // e.g. "nginx"
	Tag        string // e.g. "latest"
	Digest     string // e.g. "sha256:abc..."
	FullRepo   string // e.g. "library/nginx" (repo/image)
}

// Parse parses a Docker image reference string.
// Supports formats:
//   - nginx
//   - nginx:tag
//   - nginx@sha256:digest
//   - library/nginx:tag
//   - registry.io/repo/image:tag
//   - registry.io:5000/repo/image:tag
func Parse(ref string) (*Reference, error) {
	if ref == "" {
		return nil, fmt.Errorf("empty image reference")
	}

	result := &Reference{
		Registry: "registry-1.docker.io",
		Tag:      "latest",
	}

	// Split off digest (@sha256:...) if present
	parts := strings.SplitN(ref, "@", 2)
	if len(parts) == 2 {
		result.Digest = parts[1]
		ref = parts[0]
		// When digest is used, tag is the digest itself
		result.Tag = result.Digest
	}

	// Split off tag (:tag) if present and no digest
	if result.Digest == "" {
		lastColon := strings.LastIndex(ref, ":")
		if lastColon != -1 {
			// Check if this colon is part of the registry (port number)
			// by seeing if there's a / after it
			afterColon := ref[lastColon+1:]
			if !strings.Contains(afterColon, "/") {
				result.Tag = ref[lastColon+1:]
				ref = ref[:lastColon]
			}
		}
	}

	// Now ref is like: registry/repo/image or just image
	imgparts := strings.Split(ref, "/")

	// Determine if first part is a registry (contains '.' or ':')
	if len(imgparts) > 1 && (strings.Contains(imgparts[0], ".") || strings.Contains(imgparts[0], ":")) {
		result.Registry = imgparts[0]
		result.Image = imgparts[len(imgparts)-1]
		if len(imgparts) > 2 {
			result.Repository = strings.Join(imgparts[1:len(imgparts)-1], "/")
		}
	} else {
		// Docker Hub: default to library/ prefix for official images
		result.Image = imgparts[len(imgparts)-1]
		if len(imgparts) > 1 {
			result.Repository = strings.Join(imgparts[:len(imgparts)-1], "/")
		} else {
			result.Repository = "library"
		}
	}

	result.FullRepo = result.Repository + "/" + result.Image

	return result, nil
}

// ImageName returns the Docker-compatible image name (repo/image:tag).
func (r *Reference) ImageName() string {
	if r.Repository != "" && r.Repository != "library" {
		return fmt.Sprintf("%s/%s:%s", r.Repository, r.Image, r.Tag)
	}
	return fmt.Sprintf("%s:%s", r.Image, r.Tag)
}

// DirName returns a safe directory name for storing image data.
func (r *Reference) DirName() string {
	tag := strings.ReplaceAll(r.Tag, ":", "@")
	if r.Repository != "" && r.Repository != "library" {
		safeRepo := strings.ReplaceAll(r.Repository, "/", "_")
		return fmt.Sprintf("tmp_%s_%s_%s", safeRepo, r.Image, tag)
	}
	return fmt.Sprintf("tmp_%s_%s", r.Image, tag)
}

// String returns the full reference string.
func (r *Reference) String() string {
	if r.Registry == "registry-1.docker.io" {
		if r.Repository == "library" {
			return fmt.Sprintf("%s:%s", r.Image, r.Tag)
		}
		return fmt.Sprintf("%s/%s:%s", r.Repository, r.Image, r.Tag)
	}
	return fmt.Sprintf("%s/%s/%s:%s", r.Registry, r.Repository, r.Image, r.Tag)
}
