# docker-pull

Pull Docker images directly from registries without requiring a Docker daemon. Saves images as tar archives compatible with `docker load`.

## Features

- **No Docker required** — Fetch images using only HTTP, no daemon dependency
- **Proxy support** — SOCKS5, HTTP, HTTPS proxies with optional authentication
- **Resume support** — Interrupted downloads can be resumed from checkpoint, no wasted bandwidth
- **Cross-platform** — Single binary for Linux, macOS (amd64/arm64), Windows
- **Multi-registry** — Docker Hub, GHCR, private registries, any OCI-compliant registry
- **Digest pull** — Pull specific image versions by digest for reproducibility

## Install

### Build from source

```bash
git clone <repo-url>
cd go-pull
go build -o bin/docker-pull ./cmd/docker-pull/
```

### Cross-compile

```bash
# Linux
GOOS=linux GOARCH=amd64 go build -o docker-pull-linux ./cmd/docker-pull/

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o docker-pull-darwin ./cmd/docker-pull/

# Windows
GOOS=windows GOARCH=amd64 go build -o docker-pull.exe ./cmd/docker-pull/
```

## Usage

### Basic

```bash
# Docker Hub official image
docker-pull nginx:latest

# Docker Hub with namespace
docker-pull library/alpine:3.18

# Private registry
docker-pull registry.example.com/myapp:v1.2.0

# GHCR
docker-pull ghcr.io/owner/repo:tag

# Pull by digest (reproducible)
docker-pull nginx@sha256:abc123def456...
```

### With proxy

```bash
# SOCKS5
docker-pull --proxy socks5://127.0.0.1:1080 nginx:latest

# SOCKS5 with authentication
docker-pull --proxy socks5://user:pass@127.0.0.1:1080 nginx:latest

# HTTP proxy
docker-pull --proxy http://proxy.corp.com:8080 alpine:latest

# HTTPS proxy
docker-pull --proxy https://proxy:443 myimage:tag

# Bare host:port (defaults to SOCKS5)
docker-pull --proxy 127.0.0.1:1080 nginx:latest
```

### Resume interrupted downloads

Download state is saved to `.checkpoint.json` in the image directory. If a download is interrupted (network error, Ctrl+C, crash), resume by pointing to the directory:

```bash
# Original download interrupted — directory created: tmp_nginx_latest/
docker-pull nginx:latest
# ^C or network error

# Resume from where it left off
docker-pull --resume tmp_nginx_latest

# Resume with different output directory
docker-pull --resume tmp_nginx_latest --output /data/images
```

### Output options

```bash
# Save to specific directory
docker-pull -o /data/images nginx:latest

# Keep layers compressed (skip gzip extraction, faster)
docker-pull --no-extract nginx:latest

# Suppress progress output
docker-pull -q nginx:latest

# Skip TLS verification (self-signed certs)
docker-pull --insecure registry.local:5000/app:latest
```

### Load pulled image into Docker

```bash
# The tool creates a tar archive compatible with docker load
docker load -i nginx_latest.tar

# Or load from stdin
docker load < nginx_latest.tar
```

## Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--proxy` | | Proxy URL: `socks5://`, `http://`, `https://`, or `host:port` |
| `--resume` | | Resume from existing directory (e.g., `tmp_nginx_latest`) |
| `--output` | `-o` | Output directory for image files |
| `--insecure` | | Skip TLS certificate verification |
| `--no-extract` | | Keep layers gzip-compressed |
| `--quiet` | `-q` | Suppress progress output |
| `--help` | `-h` | Show help |

## Image reference formats

The tool follows Docker's reference resolution rules:

| Format | Registry | Repository | Example |
|--------|----------|------------|---------|
| `IMAGE` | `registry-1.docker.io` | `library` | `nginx` |
| `IMAGE:TAG` | `registry-1.docker.io` | `library` | `nginx:latest` |
| `REPO/IMAGE` | `registry-1.docker.io` | `REPO` | `bitnami/nginx` |
| `HOST/REPO/IMAGE` | `HOST` | `REPO` | `ghcr.io/owner/app` |
| `HOST:PORT/REPO/IMAGE` | `HOST:PORT` | `REPO` | `localhost:5000/app` |
| `IMAGE@DIGEST` | `registry-1.docker.io` | `library` | `nginx@sha256:abc...` |

A host is recognized as a registry when it contains `.` or `:`.

## Architecture

```
cmd/docker-pull/          CLI entry point (cobra)
pkg/
├── image/
│   └── reference.go      Image reference parser
├── proxy/
│   └── proxy.go          SOCKS5/HTTP/HTTPS transport
├── registry/
│   └── registry.go       Docker Registry v2 API client
└── puller/
    ├── puller.go          Core pull logic & assembly
    ├── resume.go          Checkpoint-based resume
    └── tar.go             Cross-platform tar archive
```

## Programmatic use

```go
import (
    "fmt"
    "github.com/docker-stealer/go-pull/pkg/puller"
)

func main() {
    result, err := puller.Pull(puller.Options{
        ImageRef: "nginx:latest",
        ProxyURL: "socks5://127.0.0.1:1080",
        OutputDir: "/data/images",
        OnProgress: func(e puller.ProgressEvent) {
            if e.Type == "layer_progress" {
                pct := float64(e.Downloaded) / float64(e.Total) * 100
                fmt.Printf("\r%s: %.1f%%", e.LayerDigest[:19], pct)
            }
        },
    })
    if err != nil {
        panic(err)
    }
    fmt.Printf("Saved to: %s\n", result.ImageDir)
}
```

## Limitations

- Requires registry to support Docker Manifest v2
- For multi-platform images, use `@digest` to select a specific platform
- Resume only works within the same directory (checkpoint file must exist)

## License

MIT
