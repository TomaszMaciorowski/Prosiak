package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backup/internal/httpjson"
	"backup/internal/protocol"
)

var nodeHTTPClient = &http.Client{Timeout: 10 * time.Second}

const defaultChunkSize = 512 * 1024

func SetNodeHTTPClient(client *http.Client) {
	if client != nil {
		nodeHTTPClient = client
	}
}

type Handler struct {
	state *State
}

func NewHandler(state *State) *Handler {
	return &Handler{state: state}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.Dir("web")))
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /nodes/register", h.registerNode)
	mux.HandleFunc("POST /nodes/heartbeat", h.registerNode)
	mux.HandleFunc("GET /nodes", h.nodes)
	mux.HandleFunc("DELETE /nodes/", h.deleteNode)
	mux.HandleFunc("POST /chunks/plan-upload", h.planChunk)
	mux.HandleFunc("POST /chunks/confirm", h.confirmChunk)
	mux.HandleFunc("GET /chunks/", h.chunkLocations)
	mux.HandleFunc("POST /backups", h.backupFile)
	mux.HandleFunc("GET /files", h.files)
	mux.HandleFunc("POST /files", h.createFile)
	mux.HandleFunc("GET /files/", h.fileManifest)
	mux.HandleFunc("PATCH /files/", h.patchFile)
	mux.HandleFunc("DELETE /files/", h.deleteFile)
	return mux
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) registerNode(w http.ResponseWriter, r *http.Request) {
	var req protocol.NodeRegisterRequest
	if err := httpjson.Read(r, &req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID == "" || req.Address == "" {
		httpjson.Error(w, http.StatusBadRequest, "id and address are required")
		return
	}
	httpjson.Write(w, http.StatusOK, h.state.RegisterNode(req))
}

func (h *Handler) nodes(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, h.state.Nodes())
}

func (h *Handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/nodes/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		httpjson.Error(w, http.StatusBadRequest, "node id is required")
		return
	}
	purgeStorage := r.URL.Query().Get("purge") == "true"
	if purgeStorage {
		node, ok := h.state.Node(id)
		if !ok {
			httpjson.Error(w, http.StatusNotFound, "node not found")
			return
		}
		if err := clearNodeStorage(node.Address); err != nil {
			httpjson.Error(w, http.StatusBadGateway, fmt.Sprintf("storage cleanup failed: %v", err))
			return
		}
	}
	node, removedLocations, ok := h.state.DeleteNode(id)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "node not found")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"deleted_node":      node.ID,
		"removed_locations": removedLocations,
		"purged_storage":    purgeStorage,
	})
}

func (h *Handler) files(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, h.state.Files())
}

func (h *Handler) planChunk(w http.ResponseWriter, r *http.Request) {
	var req protocol.ChunkPlanRequest
	if err := httpjson.Read(r, &req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Hash == "" || req.Size <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "hash and positive size are required")
		return
	}
	httpjson.Write(w, http.StatusOK, protocol.ChunkPlanResponse{
		Hash:  req.Hash,
		Nodes: h.state.PlanChunk(req.Hash, req.Size, req.Replication),
	})
}

func (h *Handler) confirmChunk(w http.ResponseWriter, r *http.Request) {
	var req protocol.ChunkConfirmRequest
	if err := httpjson.Read(r, &req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Hash == "" || req.NodeID == "" || req.Size <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "hash, node_id and positive size are required")
		return
	}
	if err := h.state.ConfirmChunk(req.Hash, req.Size, req.NodeID); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

func (h *Handler) chunkLocations(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/chunks/")
	hash = strings.TrimSuffix(hash, "/locations")
	if hash == "" {
		httpjson.Error(w, http.StatusBadRequest, "hash is required")
		return
	}
	httpjson.Write(w, http.StatusOK, protocol.ChunkLocationsResponse{
		Hash:  hash,
		Nodes: h.state.ChunkLocations(hash),
	})
}

func (h *Handler) createFile(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateFileRequest
	if err := httpjson.Read(r, &req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" || req.Size < 0 || req.ChunkSize <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "name, size and positive chunk_size are required")
		return
	}
	manifest := h.state.CreateFile(req)
	deletedVersions, unusedHashes := h.state.ApplyRetention(manifest.BackupName, manifest.Retention)
	manifest.RetentionDeleted = fileIDs(deletedVersions)
	h.deleteUnusedChunks(unusedHashes)
	httpjson.Write(w, http.StatusCreated, manifest)
}

func (h *Handler) fileManifest(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	if strings.HasSuffix(id, "/download") {
		h.downloadFile(w, r)
		return
	}
	if strings.HasSuffix(id, "/availability") {
		h.fileAvailability(w, r)
		return
	}
	id = strings.TrimSuffix(id, "/manifest")
	file, ok := h.state.File(id)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "file not found")
		return
	}
	httpjson.Write(w, http.StatusOK, file)
}

func (h *Handler) fileAvailability(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	id = strings.TrimSuffix(id, "/availability")
	availability, ok := h.state.FileAvailability(id)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "file not found")
		return
	}
	httpjson.Write(w, http.StatusOK, availability)
}

func (h *Handler) patchFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/files/")
	if strings.HasSuffix(path, "/replication") {
		h.updateFileReplication(w, r)
		return
	}
	httpjson.Error(w, http.StatusNotFound, "unknown file operation")
}

func (h *Handler) updateFileReplication(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	id = strings.TrimSuffix(id, "/replication")

	var req protocol.UpdateReplicationRequest
	if err := httpjson.Read(r, &req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Replication <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "replication must be positive")
		return
	}
	file, ok := h.state.UpdateFileReplication(id, req.Replication)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "file not found")
		return
	}
	httpjson.Write(w, http.StatusOK, file)
}

func (h *Handler) backupFile(w http.ResponseWriter, r *http.Request) {
	log.Printf("backup upload started")
	chunkSize := queryInt64(r, "chunk_size", defaultChunkSize)
	replication := queryInt(r, "replication", 3)
	retention := queryInt(r, "retention", 0)
	backupName := strings.TrimSpace(r.URL.Query().Get("backup_name"))
	if chunkSize <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "chunk_size must be positive")
		return
	}
	if retention < 0 {
		httpjson.Error(w, http.StatusBadRequest, "retention must be zero or positive")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "multipart field file is required")
		return
	}
	defer file.Close()
	if backupName == "" {
		backupName = header.Filename
	}
	log.Printf("backup upload file=%s backup_name=%s chunk_size=%d replication=%d retention=%d", header.Filename, backupName, chunkSize, replication, retention)

	// Granice chunkow wynikaja z tresci, wiec mala wstawka w archiwum nie przesuwa calej reszty backupu.
	reader := bufio.NewReader(file)
	chunks := make([]protocol.ChunkRef, 0)
	selectedCounts := make(map[string]int)
	seenHashes := make(map[string]bool)
	var totalSize int64
	var dedupNewBytes int64
	var dedupReusedBytes int64
	for index := 0; ; index++ {
		data, readErr := readContentDefinedChunk(reader, chunkSize)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			httpjson.Error(w, http.StatusInternalServerError, readErr.Error())
			return
		}
		n := len(data)

		hash := hashData(data)
		if seenHashes[hash] || h.state.ChunkInUse(hash) {
			dedupReusedBytes += int64(n)
		} else {
			dedupNewBytes += int64(n)
		}
		seenHashes[hash] = true
		existingCopies := onlineCount(h.state.ChunkLocations(hash))
		missingCopies := replication - existingCopies
		if missingCopies > 0 {
			// selectedCounts rozklada nowe chunki rowniej po node'ach w ramach jednego uploadu.
			nodes := h.state.PlanChunkSpread(hash, int64(n), missingCopies, selectedCounts)
			if len(nodes) < missingCopies {
				httpjson.Error(w, http.StatusServiceUnavailable, fmt.Sprintf("not enough nodes for chunk %s: need %d got %d", hash, missingCopies, len(nodes)))
				return
			}
			for _, node := range nodes {
				log.Printf("upload chunk index=%d hash=%s to node=%s", index, hash, node.ID)
				if err := putChunk(node.Address, hash, data); err != nil {
					httpjson.Error(w, http.StatusBadGateway, fmt.Sprintf("upload chunk %s to %s failed: %v", hash, node.ID, err))
					return
				}
				if err := h.state.ConfirmChunk(hash, int64(n), node.ID); err != nil {
					httpjson.Error(w, http.StatusBadGateway, fmt.Sprintf("confirm chunk %s on %s failed: %v", hash, node.ID, err))
					return
				}
				selectedCounts[node.ID]++
				log.Printf("uploaded chunk index=%d hash=%s to node=%s", index, hash, node.ID)
			}
		}

		chunks = append(chunks, protocol.ChunkRef{
			Index: index,
			Hash:  hash,
			Size:  int64(n),
		})
		totalSize += int64(n)
	}

	manifest := h.state.CreateFile(protocol.CreateFileRequest{
		Name:             header.Filename,
		BackupName:       backupName,
		Retention:        retention,
		Size:             totalSize,
		ChunkSize:        chunkSize,
		Replication:      replication,
		DedupNewBytes:    dedupNewBytes,
		DedupReusedBytes: dedupReusedBytes,
		Chunks:           chunks,
	})
	deletedVersions, unusedHashes := h.state.ApplyRetention(manifest.BackupName, manifest.Retention)
	manifest.RetentionDeleted = fileIDs(deletedVersions)
	h.deleteUnusedChunks(unusedHashes)
	log.Printf("backup upload complete file_id=%s chunks=%d size=%d", manifest.ID, len(manifest.Chunks), manifest.Size)
	httpjson.Write(w, http.StatusCreated, manifest)
}

func (h *Handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	id = strings.TrimSuffix(id, "/download")
	file, ok := h.state.File(id)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "file not found")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))

	for _, chunk := range file.Chunks {
		data, err := fetchFromAny(h.state.ChunkLocations(chunk.Hash), chunk.Hash)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if got := hashData(data); got != chunk.Hash {
			http.Error(w, "chunk hash mismatch", http.StatusBadGateway)
			return
		}
		if _, err := w.Write(data); err != nil {
			return
		}
	}
}

func (h *Handler) deleteFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	id = strings.TrimSuffix(id, "/")
	file, unusedHashes, ok := h.state.DeleteFile(id)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "file not found")
		return
	}

	deleted, failed := h.deleteUnusedChunks(unusedHashes)

	httpjson.Write(w, http.StatusOK, map[string]any{
		"deleted_file":   file.ID,
		"deleted_chunks": deleted,
		"failed":         failed,
	})
}

func (h *Handler) deleteUnusedChunks(unusedHashes []string) (int, []string) {
	locations := h.state.LocationsSnapshot(unusedHashes)
	deleted := 0
	failed := make([]string, 0)
	for _, hash := range unusedHashes {
		for _, node := range locations[hash] {
			if err := deleteChunk(node.Address, hash); err != nil {
				failed = append(failed, fmt.Sprintf("%s on %s: %v", hash, node.ID, err))
				continue
			}
			h.state.RemoveChunkLocation(hash, node.ID)
			deleted++
		}
	}
	return deleted, failed
}

func fileIDs(files []protocol.FileManifest) []string {
	ids := make([]string, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.ID)
	}
	return ids
}

func putChunk(nodeURL string, hash string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, nodeURL+"/chunks/"+hash, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := nodeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return nil
}

func deleteChunk(nodeURL string, hash string) error {
	req, err := http.NewRequest(http.MethodDelete, nodeURL+"/chunks/"+hash, nil)
	if err != nil {
		return err
	}
	resp, err := nodeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Skoro node juz tego nie ma, to z punktu widzenia sprzatania jestesmy w domu.
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return nil
}

func clearNodeStorage(nodeURL string) error {
	req, err := http.NewRequest(http.MethodDelete, nodeURL+"/storage?confirm=delete-all-chunks", nil)
	if err != nil {
		return err
	}
	resp, err := nodeHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return nil
}

func fetchFromAny(nodes []protocol.NodeInfo, hash string) ([]byte, error) {
	var lastErr error
	for _, node := range nodes {
		resp, err := nodeHTTPClient.Get(node.Address + "/chunks/" + hash)
		if err != nil {
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("%s: %s", resp.Status, string(data))
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		return data, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no locations for %s", hash)
}

func queryInt64(r *http.Request, key string, fallback int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func onlineCount(nodes []protocol.NodeInfo) int {
	count := 0
	for _, node := range nodes {
		if node.Status == "online" {
			count++
		}
	}
	return count
}

func hashData(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readContentDefinedChunk(r *bufio.Reader, targetSize int64) ([]byte, error) {
	if targetSize <= 0 {
		targetSize = defaultChunkSize
	}
	minSize := int(targetSize / 4)
	if minSize < 8*1024 {
		minSize = 8 * 1024
	}
	maxSize := int(targetSize * 4)
	if maxSize < minSize {
		maxSize = minSize
	}

	mask := uint64(nextPowerOfTwo(targetSize) - 1)
	if mask == 0 {
		mask = uint64(defaultChunkSize - 1)
	}

	// Prosty rolling hash: czekamy na "naturalne" miejsce ciecia, ale pilnujemy min/max rozmiaru.
	data := make([]byte, 0, int(targetSize))
	var hash uint64
	for len(data) < maxSize {
		b, err := r.ReadByte()
		if err == io.EOF {
			if len(data) == 0 {
				return nil, io.EOF
			}
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		data = append(data, b)
		hash = (hash << 1) + gearTable[b]
		if len(data) >= minSize && (hash&mask) == 0 {
			return data, nil
		}
	}
	return data, nil
}

func nextPowerOfTwo(value int64) int64 {
	if value <= 1 {
		return 1
	}
	value--
	for shift := 1; shift < 64; shift *= 2 {
		value |= value >> shift
	}
	return value + 1
}

var gearTable = makeGearTable()

func makeGearTable() [256]uint64 {
	var table [256]uint64
	var seed uint64 = 0x9e3779b97f4a7c15
	for i := range table {
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		table[i] = z ^ (z >> 31)
	}
	return table
}
