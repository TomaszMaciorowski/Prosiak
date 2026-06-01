package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"backup/internal/auth"
	"backup/internal/chunkstore"
	"backup/internal/httpjson"
	"backup/internal/protocol"
	"backup/internal/tlsconfig"
	"backup/internal/version"
)

type nodeConfig struct {
	ID                 string `json:"id"`
	Addr               string `json:"addr"`
	PublicAddr         string `json:"public_addr"`
	Server             string `json:"server"`
	Storage            string `json:"storage"`
	Capacity           string `json:"capacity"`
	WipeStorage        bool   `json:"wipe_storage"`
	AuthToken          string `json:"auth_token"`
	CertFile           string `json:"cert_file"`
	KeyFile            string `json:"key_file"`
	CACertFile         string `json:"ca_cert_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

var serverHTTPClient = &http.Client{Timeout: 10 * time.Second}

func main() {
	log.Printf("%s backup-node starting", version.FullName())

	id := flag.String("id", "node-1", "node id")
	addr := flag.String("addr", ":9001", "node listen address")
	publicAddr := flag.String("public-addr", "http://localhost:9001", "address other nodes can use")
	serverURL := flag.String("server", "http://localhost:8080", "central server url")
	storage := flag.String("storage", "storage-node-1", "chunk storage directory")
	capacityText := flag.String("capacity", "10GB", "storage capacity reserved for backup, e.g. 10GB, 500MB, 1TB")
	wipeStorage := flag.Bool("wipe-storage", false, "clear storage directory and exit")
	authToken := flag.String("auth-token", "", "shared bearer token required to talk to the server and to this node (or set "+auth.EnvVar+")")
	certFile := flag.String("cert-file", "", "TLS certificate file for HTTPS")
	keyFile := flag.String("key-file", "", "TLS private key file for HTTPS")
	caCertFile := flag.String("ca-cert-file", "", "CA certificate file trusted when connecting to the server")
	insecureSkipVerify := flag.Bool("insecure-skip-verify", false, "skip TLS certificate verification when connecting to the server")
	configPath := flag.String("config", "", "optional JSON config file")
	flag.Parse()

	if *configPath != "" {
		overrides := visitedFlags()
		cfg, err := readNodeConfig(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		if cfg.ID != "" && !overrides["id"] {
			*id = cfg.ID
		}
		if cfg.Addr != "" && !overrides["addr"] {
			*addr = cfg.Addr
		}
		if cfg.PublicAddr != "" && !overrides["public-addr"] {
			*publicAddr = cfg.PublicAddr
		}
		if cfg.Server != "" && !overrides["server"] {
			*serverURL = cfg.Server
		}
		if cfg.Storage != "" && !overrides["storage"] {
			*storage = cfg.Storage
		}
		if cfg.Capacity != "" && !overrides["capacity"] {
			*capacityText = cfg.Capacity
		}
		if cfg.WipeStorage && !overrides["wipe-storage"] {
			*wipeStorage = cfg.WipeStorage
		}
		if cfg.AuthToken != "" && !overrides["auth-token"] {
			*authToken = cfg.AuthToken
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

	client, err := tlsconfig.HTTPClient(tlsconfig.ClientConfig{
		CACertFile:         *caCertFile,
		InsecureSkipVerify: *insecureSkipVerify,
	}, 10*time.Second)
	if err != nil {
		log.Fatal(err)
	}

	capacity, err := parseCapacity(*capacityText)
	if err != nil {
		log.Fatal(err)
	}

	store, err := chunkstore.New(*storage)
	if err != nil {
		log.Fatal(err)
	}
	if *wipeStorage {
		files, bytes, err := store.Clear()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("storage cleared: path=%s files=%d bytes=%d", *storage, files, bytes)
		return
	}

	token := auth.Resolve(*authToken)
	if token == "" {
		log.Fatalf("auth token is required: set -auth-token, auth_token in the config, or %s", auth.EnvVar)
	}
	serverHTTPClient = auth.Client(client, token)

	registerReq := nodeRegisterRequest(store, *id, *publicAddr, capacity)
	if err := register(*serverURL, registerReq); err != nil {
		log.Printf("register failed: %v", err)
	}
	go heartbeatLoop(store, *serverURL, *id, *publicAddr, capacity)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("PUT /chunks/", putChunk(store, capacity))
	mux.HandleFunc("GET /chunks/", getChunk(store))
	mux.HandleFunc("DELETE /chunks/", deleteChunk(store))
	mux.HandleFunc("DELETE /storage", clearStorage(store))

	handler := auth.Middleware(token, func(r *http.Request) bool {
		return r.Method == http.MethodGet && r.URL.Path == "/health"
	}, mux)

	log.Printf("backup node %s listening on %s, storage=%s, capacity=%d", *id, *addr, *storage, capacity)
	if err := tlsconfig.ListenAndServe(*addr, handler, tlsconfig.ServerConfig{
		CertFile: *certFile,
		KeyFile:  *keyFile,
	}); err != nil {
		log.Fatal(err)
	}
}

func readNodeConfig(path string) (nodeConfig, error) {
	var cfg nodeConfig
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

func nodeRegisterRequest(store *chunkstore.Store, id string, publicAddr string, capacity int64) protocol.NodeRegisterRequest {
	used, err := store.Used()
	if err != nil {
		log.Printf("storage usage failed: %v", err)
	}
	free := capacity - used
	if free < 0 {
		free = 0
	}
	return protocol.NodeRegisterRequest{
		ID:       id,
		Address:  publicAddr,
		Capacity: capacity,
		Free:     free,
	}
}

func heartbeatLoop(store *chunkstore.Store, serverURL string, id string, publicAddr string, capacity int64) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		req := nodeRegisterRequest(store, id, publicAddr, capacity)
		if err := register(serverURL, req); err != nil {
			log.Printf("heartbeat failed: %v", err)
		}
	}
}

func register(serverURL string, req protocol.NodeRegisterRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := serverHTTPClient.Post(serverURL+"/nodes/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %s: %s", resp.Status, string(data))
	}
	return nil
}

func putChunk(store *chunkstore.Store, capacity int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("*")
		if hash == "" {
			hash = r.URL.Path[len("/chunks/"):]
		}
		if r.ContentLength > 0 {
			used, err := store.Used()
			if err != nil {
				httpjson.Error(w, http.StatusInternalServerError, err.Error())
				return
			}
			if used+r.ContentLength > capacity {
				httpjson.Error(w, http.StatusInsufficientStorage, "not enough reserved storage capacity")
				return
			}
		}
		size, err := store.Put(hash, r.Body)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		httpjson.Write(w, http.StatusCreated, map[string]any{
			"hash": hash,
			"size": size,
		})
	}
}

func parseCapacity(value string) (int64, error) {
	raw := strings.TrimSpace(strings.ToUpper(value))
	if raw == "" {
		return 0, fmt.Errorf("capacity is required")
	}

	units := []struct {
		suffix string
		mul    int64
	}{
		{"TIB", 1024 * 1024 * 1024 * 1024},
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GIB", 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MIB", 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KIB", 1024},
		{"KB", 1024},
		{"B", 1},
	}

	multiplier := int64(1)
	number := raw
	for _, unit := range units {
		if strings.HasSuffix(raw, unit.suffix) {
			multiplier = unit.mul
			number = strings.TrimSpace(strings.TrimSuffix(raw, unit.suffix))
			break
		}
	}

	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid capacity %q", value)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("capacity must be positive")
	}
	return int64(parsed * float64(multiplier)), nil
}

func getChunk(store *chunkstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("*")
		if hash == "" {
			hash = r.URL.Path[len("/chunks/"):]
		}
		f, err := store.Open(hash)
		if err != nil {
			if os.IsNotExist(err) {
				httpjson.Error(w, http.StatusNotFound, "chunk not found")
				return
			}
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, f)
	}
}

func deleteChunk(store *chunkstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("*")
		if hash == "" {
			hash = r.URL.Path[len("/chunks/"):]
		}
		if err := store.Delete(hash); err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func clearStorage(store *chunkstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("confirm") != "delete-all-chunks" {
			httpjson.Error(w, http.StatusBadRequest, "confirm=delete-all-chunks is required")
			return
		}
		files, bytes, err := store.Clear()
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpjson.Write(w, http.StatusOK, map[string]any{
			"status":        "cleared",
			"deleted_files": files,
			"deleted_bytes": bytes,
		})
	}
}
