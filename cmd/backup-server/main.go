package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"backup/internal/server"
	"backup/internal/tlsconfig"
)

type serverConfig struct {
	Addr                string `json:"addr"`
	DB                  string `json:"db"`
	ReplicationInterval string `json:"replication_interval"`
	CertFile            string `json:"cert_file"`
	KeyFile             string `json:"key_file"`
	CACertFile          string `json:"ca_cert_file"`
	InsecureSkipVerify  bool   `json:"insecure_skip_verify"`
}

func main() {
	addr := flag.String("addr", ":8080", "server listen address")
	dbPath := flag.String("db", "server-data/backup.db", "sqlite database path")
	replicationInterval := flag.Duration("replication-interval", 30*time.Second, "replication scheduler interval")
	certFile := flag.String("cert-file", "", "TLS certificate file for HTTPS")
	keyFile := flag.String("key-file", "", "TLS private key file for HTTPS")
	caCertFile := flag.String("ca-cert-file", "", "CA certificate file trusted when connecting to nodes")
	insecureSkipVerify := flag.Bool("insecure-skip-verify", false, "skip TLS certificate verification when connecting to nodes")
	configPath := flag.String("config", "", "optional JSON config file")
	flag.Parse()

	if *configPath != "" {
		overrides := visitedFlags()
		cfg, err := readServerConfig(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		if cfg.Addr != "" && !overrides["addr"] {
			*addr = cfg.Addr
		}
		if cfg.DB != "" && !overrides["db"] {
			*dbPath = cfg.DB
		}
		if cfg.ReplicationInterval != "" && !overrides["replication-interval"] {
			parsed, err := time.ParseDuration(cfg.ReplicationInterval)
			if err != nil {
				log.Fatalf("invalid replication_interval %q: %v", cfg.ReplicationInterval, err)
			}
			*replicationInterval = parsed
		}
		if cfg.CertFile != "" && !overrides["cert-file"] {
			*certFile = cfg.CertFile
		}
		if cfg.KeyFile != "" && !overrides["key-file"] {
			*keyFile = cfg.KeyFile
		}
		if cfg.CACertFile != "" && !overrides["ca-cert-file"] {
			*caCertFile = cfg.CACertFile
		}
		if cfg.InsecureSkipVerify && !overrides["insecure-skip-verify"] {
			*insecureSkipVerify = cfg.InsecureSkipVerify
		}
	}

	nodeClient, err := tlsconfig.HTTPClient(tlsconfig.ClientConfig{
		CACertFile:         *caCertFile,
		InsecureSkipVerify: *insecureSkipVerify,
	}, 10*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	server.SetNodeHTTPClient(nodeClient)

	state, err := server.NewState(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer state.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.StartReplicationScheduler(ctx, state, *replicationInterval)
	handler := server.NewHandler(state)

	log.Printf("backup server listening on %s, db=%s", *addr, *dbPath)
	if err := tlsconfig.ListenAndServe(*addr, handler.Routes(), tlsconfig.ServerConfig{
		CertFile: *certFile,
		KeyFile:  *keyFile,
	}); err != nil {
		log.Fatal(err)
	}
}

func readServerConfig(path string) (serverConfig, error) {
	var cfg serverConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

func visitedFlags() map[string]bool {
	out := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		out[f.Name] = true
	})
	return out
}
