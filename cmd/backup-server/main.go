package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"backup/internal/auth"
	"backup/internal/server"
	"backup/internal/tlsconfig"
	"backup/internal/version"
)

type serverConfig struct {
	Addr                string `json:"addr"`
	DB                  string `json:"db"`
	ReplicationInterval string `json:"replication_interval"`
	AuthToken           string `json:"auth_token"`
	MaxUploadBytes      int64  `json:"max_upload_bytes"`
	CertFile            string `json:"cert_file"`
	KeyFile             string `json:"key_file"`
	CACertFile          string `json:"ca_cert_file"`
	InsecureSkipVerify  bool   `json:"insecure_skip_verify"`
}

func main() {
	log.Printf("%s backup-server starting", version.FullName())

	addr := flag.String("addr", ":8080", "server listen address")
	dbPath := flag.String("db", "server-data/backup.db", "sqlite database path")
	replicationInterval := flag.Duration("replication-interval", 30*time.Second, "replication scheduler interval")
	authToken := flag.String("auth-token", "", "shared bearer token required on every API request (or set "+auth.EnvVar+")")
	maxUploadBytes := flag.Int64("max-upload-bytes", 0, "reject backup uploads larger than this many bytes (0 = unlimited)")
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
		if cfg.AuthToken != "" && !overrides["auth-token"] {
			*authToken = cfg.AuthToken
		}
		if cfg.MaxUploadBytes != 0 && !overrides["max-upload-bytes"] {
			*maxUploadBytes = cfg.MaxUploadBytes
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

	token := auth.Resolve(*authToken)
	if token == "" {
		log.Fatalf("auth token is required: set -auth-token, auth_token in the config, or %s", auth.EnvVar)
	}

	nodeClient, err := tlsconfig.HTTPClient(tlsconfig.ClientConfig{
		CACertFile:         *caCertFile,
		InsecureSkipVerify: *insecureSkipVerify,
	}, 10*time.Second)
	if err != nil {
		log.Fatal(err)
	}
	server.SetNodeHTTPClient(auth.Client(nodeClient, token))

	state, err := server.NewState(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer state.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.StartReplicationScheduler(ctx, state, *replicationInterval)
	handler := server.NewHandler(state, token, *maxUploadBytes)

	if *certFile == "" {
		log.Printf("WARNING: serving plaintext HTTP on %s; set cert_file/key_file to enable TLS", *addr)
	}
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
