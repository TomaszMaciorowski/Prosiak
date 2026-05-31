package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"backup/internal/server"
)

type serverConfig struct {
	Addr                string `json:"addr"`
	DB                  string `json:"db"`
	ReplicationInterval string `json:"replication_interval"`
}

func main() {
	addr := flag.String("addr", ":8080", "server listen address")
	dbPath := flag.String("db", "server-data/backup.db", "sqlite database path")
	replicationInterval := flag.Duration("replication-interval", 30*time.Second, "replication scheduler interval")
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
	}

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
	if err := http.ListenAndServe(*addr, handler.Routes()); err != nil {
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
