package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

type ServerConfig struct {
	CertFile string
	KeyFile  string
}

type ClientConfig struct {
	CACertFile         string
	InsecureSkipVerify bool
}

func ListenAndServe(addr string, handler http.Handler, cfg ServerConfig) error {
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		return http.ListenAndServe(addr, handler)
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return fmt.Errorf("both cert_file and key_file are required for HTTPS")
	}
	return http.ListenAndServeTLS(addr, cfg.CertFile, cfg.KeyFile, handler)
}

func HTTPClient(cfg ClientConfig, timeout time.Duration) (*http.Client, error) {
	if cfg.CACertFile == "" && !cfg.InsecureSkipVerify {
		return &http.Client{Timeout: timeout}, nil
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	if cfg.CACertFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		data, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("no certificates found in %s", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}, nil
}
