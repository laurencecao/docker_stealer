package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Config holds proxy configuration.
type Config struct {
	Type     Type   // socks5, http, https
	Address  string // e.g. "127.0.0.1:1080"
	Username string // optional auth
	Password string // optional auth
}

// Type represents the proxy protocol type.
type Type string

const (
	TypeNone   Type = "none"
	TypeSOCKS5 Type = "socks5"
	TypeHTTP   Type = "http"
	TypeHTTPS  Type = "https"
)

// ParseProxyURL parses a proxy URL string like:
//   - socks5://user:pass@host:port
//   - http://host:port
//   - https://host:port
//   - host:port (defaults to socks5)
func ParseProxyURL(proxyURL string) (*Config, error) {
	if proxyURL == "" {
		return nil, nil
	}

	// Try parsing as URL first
	u, err := url.Parse(proxyURL)
	if err != nil {
		// Maybe it's just host:port, default to socks5
		if strings.Contains(proxyURL, ":") {
			return &Config{
				Type:    TypeSOCKS5,
				Address: proxyURL,
			}, nil
		}
		return nil, fmt.Errorf("invalid proxy URL: %s", proxyURL)
	}

	cfg := &Config{}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks":
		cfg.Type = TypeSOCKS5
	case "http":
		cfg.Type = TypeHTTP
	case "https":
		cfg.Type = TypeHTTPS
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}

	cfg.Address = u.Host
	if u.User != nil {
		cfg.Username = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}

	return cfg, nil
}

// NewTransport creates an http.Transport configured with the proxy.
func NewTransport(cfg *Config) (*http.Transport, error) {
	if cfg == nil || cfg.Type == TypeNone {
		return &http.Transport{
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 10 * time.Second,
		}, nil
	}

	switch cfg.Type {
	case TypeSOCKS5:
		return newSOCKS5Transport(cfg)
	case TypeHTTP, TypeHTTPS:
		return newHTTPTransport(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported proxy type: %s", cfg.Type)
	}
}

func newSOCKS5Transport(cfg *Config) (*http.Transport, error) {
	var auth *proxy.Auth
	if cfg.Username != "" {
		auth = &proxy.Auth{
			User:     cfg.Username,
			Password: cfg.Password,
		}
	}

	dialer, err := proxy.SOCKS5("tcp", cfg.Address, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
	}, nil
}

func newHTTPTransport(cfg *Config) *http.Transport {
	proxyURL := &url.URL{
		Scheme: string(cfg.Type),
		Host:   cfg.Address,
	}
	if cfg.Username != "" {
		proxyURL.User = url.UserPassword(cfg.Username, cfg.Password)
	}

	return &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL),
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
	}
}
