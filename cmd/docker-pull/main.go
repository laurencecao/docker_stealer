package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/docker-stealer/go-pull/pkg/image"
	"github.com/docker-stealer/go-pull/pkg/puller"
	"github.com/spf13/cobra"
)

var (
	proxyURL  string
	resumeDir string
	outputDir string
	insecure  bool
	noExtract bool
	quiet     bool
	version   = "0.1.0"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "docker-pull [IMAGE]",
		Short: "Pull Docker images without Docker daemon",
		Long: `docker-pull fetches Docker images directly from registries and saves them
as tar archives that can be loaded with 'docker load'. Supports resume
for interrupted downloads and proxy for network-restricted environments.

Examples:
  docker-pull nginx:latest
  docker-pull library/alpine:3.18
  docker-pull ghcr.io/owner/repo:v1
  docker-pull registry.example.com/app:tag
  docker-pull nginx@sha256:abc123...

Resume interrupted downloads:
  docker-pull --resume tmp_nginx_latest
  docker-pull --resume tmp_nginx_latest --output /data

With proxy:
  docker-pull --proxy socks5://127.0.0.1:1080 nginx:latest
  docker-pull --proxy http://user:pass@proxy:8080 alpine:latest
  docker-pull --proxy https://proxy:443 myimage:tag

Image reference formats:
  IMAGE              Uses registry-1.docker.io, repo defaults to 'library'
  REPO/IMAGE         Uses registry-1.docker.io
  HOST/REPO/IMAGE    Uses HOST as registry (must contain '.' or ':')
  IMAGE@DIGEST       Pulls by digest instead of tag`,
		Args: cobra.MaximumNArgs(1),
		RunE: runPull,
	}

	rootCmd.Flags().StringVar(&proxyURL, "proxy", "", "Proxy URL (socks5://host:port, http://host:port, https://host:port)")
	rootCmd.Flags().StringVar(&resumeDir, "resume", "", "Resume download from existing directory (e.g. tmp_nginx_latest)")
	rootCmd.Flags().StringVarP(&outputDir, "output", "o", "", "Output directory for image files (default: current directory)")
	rootCmd.Flags().BoolVar(&insecure, "insecure", false, "Skip TLS certificate verification")
	rootCmd.Flags().BoolVar(&noExtract, "no-extract", false, "Keep layers compressed (skip gzip extraction)")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress output")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runPull(cmd *cobra.Command, args []string) error {
	// If resume mode, image ref is optional
	if resumeDir != "" {
		return runResume()
	}

	if len(args) == 0 {
		return fmt.Errorf("image reference is required\nUsage: docker-pull [IMAGE]\nRun 'docker-pull --help' for more information")
	}

	imageRef := args[0]

	// Parse to validate early
	ref, err := image.Parse(imageRef)
	if err != nil {
		return fmt.Errorf("invalid image reference '%s': %w", imageRef, err)
	}

	if !quiet {
		fmt.Printf("Pulling %s from %s\n", ref.String(), ref.Registry)
		if proxyURL != "" {
			fmt.Printf("Using proxy: %s\n", maskProxyPassword(proxyURL))
		}
	}

	opts := puller.Options{
		ImageRef:  imageRef,
		ProxyURL:  proxyURL,
		Insecure:  insecure,
		OutputDir: outputDir,
		NoExtract: noExtract,
	}

	if !quiet {
		opts.OnProgress = makeProgressPrinter()
	}

	tarPath, err := puller.PullAsTar(opts)
	if err != nil {
		return err
	}

	if !quiet {
		fmt.Printf("\nDone! Image saved to: %s\n", tarPath)
	}

	return nil
}

func runResume() error {
	if !quiet {
		fmt.Printf("Resuming download from: %s\n", resumeDir)
		if proxyURL != "" {
			fmt.Printf("Using proxy: %s\n", maskProxyPassword(proxyURL))
		}
	}

	opts := puller.Options{
		ImageRef:  "", // Will be read from checkpoint
		ProxyURL:  proxyURL,
		Insecure:  insecure,
		ResumeDir: resumeDir,
		OutputDir: outputDir,
		NoExtract: noExtract,
	}

	if !quiet {
		opts.OnProgress = makeProgressPrinter()
	}

	tarPath, err := puller.PullAsTar(opts)
	if err != nil {
		return err
	}

	if !quiet {
		fmt.Printf("\nDone! Image saved to: %s\n", tarPath)
	}

	return nil
}

func makeProgressPrinter() puller.ProgressFunc {
	layerStates := make(map[string]bool) // track if we've printed "complete"

	return func(event puller.ProgressEvent) {
		switch event.Type {
		case "message":
			fmt.Printf("%s\n", event.Message)

		case "config_start":
			fmt.Printf("%s\n", event.Message)

		case "config_done":
			fmt.Printf("%s\n", event.Message)

		case "layer_start":
			pct := float64(0)
			if event.Total > 0 && event.Downloaded > 0 {
				pct = float64(event.Downloaded) / float64(event.Total) * 100
			}
			fmt.Printf("\r%s: Downloading %5.1f%%", shortDigest(event.LayerDigest), pct)

		case "layer_progress":
			if event.Total > 0 {
				pct := float64(event.Downloaded) / float64(event.Total) * 100
				barWidth := 40
				filled := int(pct / 100 * float64(barWidth))
				if filled > barWidth {
					filled = barWidth
				}
				bar := strings.Repeat("=", filled)
				if filled < barWidth {
					bar += ">"
					bar += strings.Repeat(" ", barWidth-filled-1)
				}
				fmt.Printf("\r%s: [%s] %5.1f%% %s",
					shortDigest(event.LayerDigest),
					bar,
					pct,
					formatBytes(event.Downloaded))
			}

		case "layer_complete":
			if !layerStates[event.LayerDigest] {
				fmt.Printf("\r%s: Pull complete [%s]      \n",
					shortDigest(event.LayerDigest),
					formatBytes(event.Total))
				layerStates[event.LayerDigest] = true
			}

		case "layer_extract":
			fmt.Printf("\r%s: Extracting...      \n", shortDigest(event.LayerDigest))

		case "assembly_start":
			fmt.Printf("%s\n", event.Message)

		case "tar_start":
			fmt.Printf("%s\n", event.Message)

		case "done":
			fmt.Printf("\n%s\n", event.Message)
		}
	}
}

func shortDigest(digest string) string {
	if len(digest) > 7 {
		d := digest[7:]
		if len(d) > 12 {
			return d[:12]
		}
		return d
	}
	return digest
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func maskProxyPassword(proxy string) string {
	// Mask password in proxy URL for display
	// socks5://user:pass@host -> socks5://user:***@host
	atIdx := strings.LastIndex(proxy, "@")
	if atIdx == -1 {
		return proxy
	}
	colonIdx := strings.Index(proxy[:atIdx], "://")
	if colonIdx == -1 {
		return proxy
	}
	credStart := colonIdx + 3
	credPart := proxy[credStart:atIdx]
	if strings.Contains(credPart, ":") {
		user := credPart[:strings.Index(credPart, ":")]
		return proxy[:credStart] + user + ":***@" + proxy[atIdx+1:]
	}
	return proxy
}
