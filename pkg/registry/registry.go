package registry

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/docker-stealer/go-pull/pkg/image"
	"github.com/docker-stealer/go-pull/pkg/proxy"
)

const (
	manifestV2 = "application/vnd.docker.distribution.manifest.v2+json"
	listV2     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

// Client is a Docker registry client.
type Client struct {
	httpClient  *http.Client
	ref         *image.Reference
	authURL     string
	authService string
	token       string
	tokenExpiry time.Time
}

// Manifest represents a Docker manifest v2.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// ManifestList represents a manifest list v2 (multi-arch).
type ManifestList struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Manifests     []ManifestDescriptor `json:"manifests"`
}

// ManifestDescriptor is a manifest entry in a manifest list.
type ManifestDescriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  map[string]string `json:"platform"`
}

// Descriptor is a blob descriptor.
type Descriptor struct {
	MediaType string   `json:"mediaType"`
	Size      int64    `json:"size"`
	Digest    string   `json:"digest"`
	URLs      []string `json:"urls,omitempty"`
}

// NewClient creates a new registry client.
func NewClient(ref *image.Reference, proxyCfg *proxy.Config, insecure bool) (*Client, error) {
	transport, err := proxy.NewTransport(proxyCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create transport: %w", err)
	}

	if insecure {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	client := &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute,
		},
		ref: ref,
	}

	// Discover auth endpoint
	if err := client.discoverAuth(); err != nil {
		return nil, fmt.Errorf("failed to discover auth: %w", err)
	}

	return client, nil
}

func (c *Client) discoverAuth() error {
	resp, err := c.httpClient.Get(fmt.Sprintf("https://%s/v2/", c.ref.Registry))
	if err != nil {
		return fmt.Errorf("failed to ping registry: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		c.authURL = ""
		return nil
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Parse WWW-Authenticate header
	authHeader := resp.Header.Get("WWW-Authenticate")
	if authHeader == "" {
		return fmt.Errorf("no WWW-Authenticate header in 401 response")
	}

	// Format: Bearer realm="...",service="...",scope="..."
	c.authURL = extractQuoted(authHeader, "realm")
	c.authService = extractQuoted(authHeader, "service")
	if c.authService == "" {
		c.authService = "registry.docker.io"
	}

	return nil
}

func (c *Client) ensureToken(scope string) error {
	if c.authURL == "" {
		return nil // No auth required
	}

	// Check if token is still valid (refresh 30s before expiry)
	if c.token != "" && time.Now().Before(c.tokenExpiry.Add(-30*time.Second)) {
		return nil
	}

	tokenURL := fmt.Sprintf("%s?service=%s&scope=%s", c.authURL, c.authService, scope)
	resp, err := c.httpClient.Get(tokenURL)
	if err != nil {
		return fmt.Errorf("failed to get token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token request failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to decode token response: %w", err)
	}

	c.token = tokenResp.Token
	if c.token == "" {
		c.token = tokenResp.AccessToken
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300 // default 5 minutes
	}
	c.tokenExpiry = time.Now().Add(time.Duration(expiresIn) * time.Second)

	return nil
}

func (c *Client) makeRequest(method, urlStr string, acceptType string) (*http.Response, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, err
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if acceptType != "" {
		req.Header.Set("Accept", acceptType)
	}

	return c.httpClient.Do(req)
}

// FetchManifest fetches the image manifest. Returns manifest and manifest list (if applicable).
func (c *Client) FetchManifest() (*Manifest, *ManifestList, error) {
	scope := fmt.Sprintf("repository:%s:pull", c.ref.FullRepo)
	if err := c.ensureToken(scope); err != nil {
		return nil, nil, err
	}

	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s",
		c.ref.Registry, c.ref.FullRepo, c.ref.Tag)

	// Try manifest v2 first
	resp, err := c.makeRequest("GET", manifestURL, manifestV2)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var manifest Manifest
		if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
			return nil, nil, fmt.Errorf("failed to decode manifest: %w", err)
		}
		return &manifest, nil, nil
	}

	// Try manifest list v2
	if err := c.ensureToken(scope); err != nil {
		return nil, nil, err
	}

	resp2, err := c.makeRequest("GET", manifestURL, listV2)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch manifest list: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == http.StatusOK {
		var manifestList ManifestList
		if err := json.NewDecoder(resp2.Body).Decode(&manifestList); err != nil {
			return nil, nil, fmt.Errorf("failed to decode manifest list: %w", err)
		}
		return nil, &manifestList, nil
	}

	body, _ := io.ReadAll(resp.Body)
	return nil, nil, fmt.Errorf("cannot fetch manifest [HTTP %d]: %s", resp.StatusCode, string(body))
}

// FetchBlob downloads a blob (layer or config) and returns the response for streaming.
// Caller is responsible for closing the response body.
func (c *Client) FetchBlob(digest string) (*http.Response, error) {
	scope := fmt.Sprintf("repository:%s:pull", c.ref.FullRepo)
	if err := c.ensureToken(scope); err != nil {
		return nil, err
	}

	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s",
		c.ref.Registry, c.ref.FullRepo, digest)

	resp, err := c.makeRequest("GET", blobURL, manifestV2)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blob %s: %w", shortDigest(digest), err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("blob %s download failed [HTTP %d]", shortDigest(digest), resp.StatusCode)
	}

	return resp, nil
}

// FetchBlobURL downloads from a specific URL (for external layers).
func (c *Client) FetchBlobURL(blobURL string) (*http.Response, error) {
	scope := fmt.Sprintf("repository:%s:pull", c.ref.FullRepo)
	if err := c.ensureToken(scope); err != nil {
		return nil, err
	}

	resp, err := c.makeRequest("GET", blobURL, manifestV2)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blob from URL: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("blob download from URL failed [HTTP %d]", resp.StatusCode)
	}

	return resp, nil
}

// FetchConfig downloads the image config blob as raw JSON.
func (c *Client) FetchConfig(digest string) ([]byte, error) {
	resp, err := c.FetchBlob(digest)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// extractQuoted extracts a value from a quoted string in a header.
// e.g. Bearer realm="https://auth.docker.io/token",service="registry.docker.io"
func extractQuoted(header, key string) string {
	searchKey := key + `="`
	idx := strings.Index(header, searchKey)
	if idx == -1 {
		return ""
	}
	start := idx + len(searchKey)
	end := strings.Index(header[start:], `"`)
	if end == -1 {
		return ""
	}
	return header[start : start+end]
}

// shortDigest returns the first 12 chars of a digest (for display).
func shortDigest(digest string) string {
	// sha256:abcdef... -> abcdef...
	if len(digest) > 7 {
		d := digest[7:]
		if len(d) > 12 {
			return d[:12]
		}
		return d
	}
	return digest
}

// ShortDigest is the exported version.
func ShortDigest(digest string) string {
	return shortDigest(digest)
}
