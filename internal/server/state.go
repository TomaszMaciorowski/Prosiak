package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"backup/internal/protocol"

	_ "modernc.org/sqlite"
)

type State struct {
	db *sql.DB
}

const nodeOfflineAfter = 45 * time.Second

type ReplicationTask struct {
	Hash       string
	Size       int64
	Desired    int
	Active     int
	Missing    int
	Sources    []protocol.NodeInfo
	Candidates []protocol.NodeInfo
}

type PruneTask struct {
	Hash      string
	Size      int64
	Desired   int
	Active    int
	Extra     int
	Locations []protocol.NodeInfo
}

type OrphanChunkTask struct {
	Hash string
	Size int64
	Node protocol.NodeInfo
}

func NewState(dbPath string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil && filepath.Dir(dbPath) != "." {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	state := &State{db: db}
	if err := state.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return state, nil
}

func (s *State) Close() error {
	return s.db.Close()
}

func (s *State) migrate() error {
	_, err := s.db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS nodes (
	id TEXT PRIMARY KEY,
	address TEXT NOT NULL,
	capacity INTEGER NOT NULL,
	free INTEGER NOT NULL,
	last_seen TEXT NOT NULL,
	status TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	backup_name TEXT NOT NULL DEFAULT '',
	version INTEGER NOT NULL DEFAULT 1,
	retention INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL,
	chunk_size INTEGER NOT NULL,
	replication INTEGER NOT NULL DEFAULT 3,
	dedup_new_bytes INTEGER NOT NULL DEFAULT 0,
	dedup_reused_bytes INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS file_chunks (
	file_id TEXT NOT NULL,
	chunk_index INTEGER NOT NULL,
	hash TEXT NOT NULL,
	size INTEGER NOT NULL,
	PRIMARY KEY (file_id, chunk_index),
	FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_chunks_hash ON file_chunks(hash);

CREATE TABLE IF NOT EXISTS chunk_locations (
	hash TEXT NOT NULL,
	node_id TEXT NOT NULL,
	size INTEGER NOT NULL,
	verified_at TEXT NOT NULL,
	PRIMARY KEY (hash, node_id),
	FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
);
`)
	if err != nil {
		return err
	}
	if err := s.ensureColumn("files", "replication", `ALTER TABLE files ADD COLUMN replication INTEGER NOT NULL DEFAULT 3`); err != nil {
		return err
	}
	if err := s.ensureColumn("files", "backup_name", `ALTER TABLE files ADD COLUMN backup_name TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn("files", "version", `ALTER TABLE files ADD COLUMN version INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if err := s.ensureColumn("files", "retention", `ALTER TABLE files ADD COLUMN retention INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("files", "dedup_new_bytes", `ALTER TABLE files ADD COLUMN dedup_new_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("files", "dedup_reused_bytes", `ALTER TABLE files ADD COLUMN dedup_reused_bytes INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE files SET backup_name = name WHERE backup_name = ''`)
	return err
}

func (s *State) RegisterNode(req protocol.NodeRegisterRequest) protocol.NodeInfo {
	now := time.Now().UTC().Format(time.RFC3339)
	node := protocol.NodeInfo{
		ID:       req.ID,
		Address:  req.Address,
		Capacity: req.Capacity,
		Free:     req.Free,
		LastSeen: now,
		Status:   "online",
	}
	_, _ = s.db.Exec(`
INSERT INTO nodes (id, address, capacity, free, last_seen, status)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	address = excluded.address,
	capacity = excluded.capacity,
	free = excluded.free,
	last_seen = excluded.last_seen,
	status = excluded.status
`, node.ID, node.Address, node.Capacity, node.Free, node.LastSeen, node.Status)
	return node
}

func (s *State) Nodes() []protocol.NodeInfo {
	rows, err := s.db.Query(`SELECT id, address, capacity, free, last_seen, status FROM nodes ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	nodes := make([]protocol.NodeInfo, 0)
	for rows.Next() {
		var node protocol.NodeInfo
		if err := rows.Scan(&node.ID, &node.Address, &node.Capacity, &node.Free, &node.LastSeen, &node.Status); err == nil {
			node = withDerivedStatus(node)
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func (s *State) Node(id string) (protocol.NodeInfo, bool) {
	var node protocol.NodeInfo
	err := s.db.QueryRow(`SELECT id, address, capacity, free, last_seen, status FROM nodes WHERE id = ?`, id).
		Scan(&node.ID, &node.Address, &node.Capacity, &node.Free, &node.LastSeen, &node.Status)
	if err != nil {
		return protocol.NodeInfo{}, false
	}
	return withDerivedStatus(node), true
}

func (s *State) DeleteNode(id string) (protocol.NodeInfo, int64, bool) {
	node, ok := s.Node(id)
	if !ok {
		return protocol.NodeInfo{}, 0, false
	}

	var removedLocations int64
	tx, err := s.db.Begin()
	if err != nil {
		return protocol.NodeInfo{}, 0, false
	}
	defer tx.Rollback()

	result, err := tx.Exec(`DELETE FROM chunk_locations WHERE node_id = ?`, id)
	if err != nil {
		return protocol.NodeInfo{}, 0, false
	}
	removedLocations, _ = result.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		return protocol.NodeInfo{}, 0, false
	}
	if err := tx.Commit(); err != nil {
		return protocol.NodeInfo{}, 0, false
	}
	return node, removedLocations, true
}

func (s *State) PlanChunk(hash string, size int64, replication int) []protocol.NodeInfo {
	return s.PlanChunkSpread(hash, size, replication, nil)
}

func (s *State) PlanChunkSpread(hash string, size int64, replication int, selectedCounts map[string]int) []protocol.NodeInfo {
	if replication <= 0 {
		replication = 3
	}

	cutoff := time.Now().UTC().Add(-nodeOfflineAfter).Format(time.RFC3339)
	rows, err := s.db.Query(`
SELECT id, address, capacity, free, last_seen, status
FROM nodes
WHERE last_seen >= ?
  AND free >= ?
  AND id NOT IN (SELECT node_id FROM chunk_locations WHERE hash = ?)
ORDER BY free DESC, id
`, cutoff, size, hash)
	if err != nil {
		return nil
	}
	defer rows.Close()

	nodes := make([]protocol.NodeInfo, 0)
	for rows.Next() {
		var node protocol.NodeInfo
		if err := rows.Scan(&node.ID, &node.Address, &node.Capacity, &node.Free, &node.LastSeen, &node.Status); err == nil {
			node = withDerivedStatus(node)
			nodes = append(nodes, node)
		}
	}
	// Przy uploadzie patrzymy nie tylko na wolne miejsce, ale tez na to, zeby jeden node nie dostal calej serii chunkow.
	sort.Slice(nodes, func(i, j int) bool {
		leftCount := 0
		rightCount := 0
		if selectedCounts != nil {
			leftCount = selectedCounts[nodes[i].ID]
			rightCount = selectedCounts[nodes[j].ID]
		}
		if leftCount != rightCount {
			return leftCount < rightCount
		}
		if nodes[i].Free != nodes[j].Free {
			return nodes[i].Free > nodes[j].Free
		}
		return nodes[i].ID < nodes[j].ID
	})
	if len(nodes) > replication {
		nodes = nodes[:replication]
	}
	return nodes
}

func (s *State) ConfirmChunk(hash string, size int64, nodeID string) error {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM nodes WHERE id = ?`, nodeID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return errors.New("unknown node")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.Exec(`
INSERT OR IGNORE INTO chunk_locations (hash, node_id, size, verified_at)
VALUES (?, ?, ?, ?)
`, hash, nodeID, size, now)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		_, _ = s.db.Exec(`
UPDATE nodes
SET free = CASE WHEN free - ? < 0 THEN 0 ELSE free - ? END,
	last_seen = ?,
	status = 'online'
WHERE id = ?
`, size, size, now, nodeID)
	}
	return nil
}

func (s *State) ChunkLocations(hash string) []protocol.NodeInfo {
	rows, err := s.db.Query(`
SELECT n.id, n.address, n.capacity, n.free, n.last_seen, n.status
FROM chunk_locations cl
JOIN nodes n ON n.id = cl.node_id
WHERE cl.hash = ?
ORDER BY n.id
`, hash)
	if err != nil {
		return nil
	}
	defer rows.Close()

	nodes := make([]protocol.NodeInfo, 0)
	for rows.Next() {
		var node protocol.NodeInfo
		if err := rows.Scan(&node.ID, &node.Address, &node.Capacity, &node.Free, &node.LastSeen, &node.Status); err == nil {
			node = withDerivedStatus(node)
			nodes = append(nodes, node)
		}
	}
	return nodes
}

func (s *State) ChunkInUse(hash string) bool {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM file_chunks WHERE hash = ?`, hash).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func (s *State) CreateFile(req protocol.CreateFileRequest) protocol.FileManifest {
	id := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102"), time.Now().UTC().UnixNano())
	backupName := strings.TrimSpace(req.BackupName)
	if backupName == "" {
		backupName = req.Name
	}
	retention := normalizedRetention(req.Retention)
	dedupNewBytes := req.DedupNewBytes
	dedupReusedBytes := req.DedupReusedBytes
	if dedupNewBytes == 0 && dedupReusedBytes == 0 && req.Size > 0 {
		dedupNewBytes = req.Size
	}
	file := protocol.FileManifest{
		ID:               id,
		Name:             req.Name,
		BackupName:       backupName,
		Retention:        retention,
		Size:             req.Size,
		ChunkSize:        req.ChunkSize,
		Replication:      normalizedReplication(req.Replication),
		DedupNewBytes:    dedupNewBytes,
		DedupReusedBytes: dedupReusedBytes,
		Chunks:           req.Chunks,
	}
	file.DedupRatio = dedupRatio(file.DedupReusedBytes, file.Size)

	tx, err := s.db.Begin()
	if err != nil {
		return file
	}
	defer tx.Rollback()

	createdAt := time.Now().UTC().Format(time.RFC3339)
	file.CreatedAt = createdAt
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) + 1 FROM files WHERE backup_name = ?`, backupName).Scan(&file.Version); err != nil {
		return file
	}
	if _, err := tx.Exec(`INSERT INTO files (id, name, backup_name, version, retention, size, chunk_size, replication, dedup_new_bytes, dedup_reused_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		file.ID, file.Name, file.BackupName, file.Version, file.Retention, file.Size, file.ChunkSize, file.Replication, file.DedupNewBytes, file.DedupReusedBytes, file.CreatedAt); err != nil {
		return file
	}
	for _, chunk := range file.Chunks {
		if _, err := tx.Exec(`INSERT INTO file_chunks (file_id, chunk_index, hash, size) VALUES (?, ?, ?, ?)`,
			file.ID, chunk.Index, chunk.Hash, chunk.Size); err != nil {
			return file
		}
	}
	_ = tx.Commit()
	return file
}

func (s *State) File(id string) (protocol.FileManifest, bool) {
	var file protocol.FileManifest
	err := s.db.QueryRow(`SELECT id, name, backup_name, version, retention, size, chunk_size, replication, dedup_new_bytes, dedup_reused_bytes, created_at FROM files WHERE id = ?`, id).
		Scan(&file.ID, &file.Name, &file.BackupName, &file.Version, &file.Retention, &file.Size, &file.ChunkSize, &file.Replication, &file.DedupNewBytes, &file.DedupReusedBytes, &file.CreatedAt)
	if err != nil {
		return protocol.FileManifest{}, false
	}
	file.DedupRatio = dedupRatio(file.DedupReusedBytes, file.Size)
	file.Chunks = s.fileChunks(id)
	return file, true
}

func (s *State) UpdateFileReplication(id string, replication int) (protocol.FileManifest, bool) {
	replication = normalizedReplication(replication)
	result, err := s.db.Exec(`UPDATE files SET replication = ? WHERE id = ?`, replication, id)
	if err != nil {
		return protocol.FileManifest{}, false
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return protocol.FileManifest{}, false
	}
	return s.File(id)
}

func (s *State) FileAvailability(id string) (protocol.FileAvailability, bool) {
	file, ok := s.File(id)
	if !ok {
		return protocol.FileAvailability{}, false
	}

	availability := protocol.FileAvailability{
		FileID:           file.ID,
		Name:             file.Name,
		BackupName:       file.BackupName,
		Version:          file.Version,
		Retention:        file.Retention,
		CreatedAt:        file.CreatedAt,
		Size:             file.Size,
		ChunkSize:        file.ChunkSize,
		Replication:      file.Replication,
		DedupNewBytes:    file.DedupNewBytes,
		DedupReusedBytes: file.DedupReusedBytes,
		DedupRatio:       file.DedupRatio,
		Chunks:           make([]protocol.ChunkAvailability, 0, len(file.Chunks)),
	}

	for _, chunk := range file.Chunks {
		locations := s.ChunkLocations(chunk.Hash)
		active := len(onlineNodes(locations))
		status := "healthy"
		if active == 0 {
			status = "missing"
		} else if active < file.Replication {
			status = "degraded"
		} else if active > file.Replication {
			status = "extra"
		}

		availability.Chunks = append(availability.Chunks, protocol.ChunkAvailability{
			Index:     chunk.Index,
			Hash:      chunk.Hash,
			Size:      chunk.Size,
			Desired:   file.Replication,
			Active:    active,
			Total:     len(locations),
			Status:    status,
			Locations: locations,
		})
	}
	return availability, true
}

func (s *State) Files() []protocol.FileManifest {
	rows, err := s.db.Query(`SELECT id, name, backup_name, version, retention, size, chunk_size, replication, dedup_new_bytes, dedup_reused_bytes, created_at FROM files ORDER BY backup_name, version DESC, created_at DESC, id DESC`)
	if err != nil {
		return nil
	}

	files := make([]protocol.FileManifest, 0)
	for rows.Next() {
		var file protocol.FileManifest
		if err := rows.Scan(&file.ID, &file.Name, &file.BackupName, &file.Version, &file.Retention, &file.Size, &file.ChunkSize, &file.Replication, &file.DedupNewBytes, &file.DedupReusedBytes, &file.CreatedAt); err != nil {
			continue
		}
		file.DedupRatio = dedupRatio(file.DedupReusedBytes, file.Size)
		files = append(files, file)
	}
	_ = rows.Close()

	for i := range files {
		files[i].Chunks = s.fileChunks(files[i].ID)
	}
	return files
}

func (s *State) DeleteFile(id string) (protocol.FileManifest, []string, bool) {
	file, ok := s.File(id)
	if !ok {
		return protocol.FileManifest{}, nil, false
	}

	tx, err := s.db.Begin()
	if err != nil {
		return protocol.FileManifest{}, nil, false
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, id); err != nil {
		return protocol.FileManifest{}, nil, false
	}

	unused := make([]string, 0)
	seen := make(map[string]bool)
	for _, chunk := range file.Chunks {
		if seen[chunk.Hash] {
			continue
		}
		seen[chunk.Hash] = true
		var stillUsed int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM file_chunks WHERE hash = ?`, chunk.Hash).Scan(&stillUsed); err != nil {
			return protocol.FileManifest{}, nil, false
		}
		if stillUsed == 0 {
			unused = append(unused, chunk.Hash)
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.FileManifest{}, nil, false
	}
	return file, unused, true
}

func (s *State) ApplyRetention(backupName string, keep int) ([]protocol.FileManifest, []string) {
	backupName = strings.TrimSpace(backupName)
	keep = normalizedRetention(keep)
	if backupName == "" || keep <= 0 {
		return nil, nil
	}

	rows, err := s.db.Query(`
SELECT id
FROM files
WHERE backup_name = ?
ORDER BY version DESC, created_at DESC, id DESC
LIMIT -1 OFFSET ?
`, backupName, keep)
	if err != nil {
		return nil, nil
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}

	// Najpierw zapamietujemy manifesty do odpowiedzi/API, dopiero potem kasujemy je z bazy.
	deleted := make([]protocol.FileManifest, 0, len(ids))
	for _, id := range ids {
		file, ok := s.File(id)
		if ok {
			deleted = append(deleted, file)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil
	}
	defer tx.Rollback()

	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, id); err != nil {
			return nil, nil
		}
	}

	unused := make([]string, 0)
	seen := make(map[string]bool)
	for _, file := range deleted {
		for _, chunk := range file.Chunks {
			if seen[chunk.Hash] {
				continue
			}
			seen[chunk.Hash] = true
			var stillUsed int
			if err := tx.QueryRow(`SELECT COUNT(1) FROM file_chunks WHERE hash = ?`, chunk.Hash).Scan(&stillUsed); err != nil {
				return nil, nil
			}
			if stillUsed == 0 {
				unused = append(unused, chunk.Hash)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil
	}
	return deleted, unused
}

func (s *State) LocationsSnapshot(hashes []string) map[string][]protocol.NodeInfo {
	out := make(map[string][]protocol.NodeInfo, len(hashes))
	for _, hash := range hashes {
		out[hash] = s.ChunkLocations(hash)
	}
	return out
}

func (s *State) RemoveChunkLocation(hash string, nodeID string) {
	var size int64
	_ = s.db.QueryRow(`SELECT size FROM chunk_locations WHERE hash = ? AND node_id = ?`, hash, nodeID).Scan(&size)
	result, _ := s.db.Exec(`DELETE FROM chunk_locations WHERE hash = ? AND node_id = ?`, hash, nodeID)
	affected, _ := result.RowsAffected()
	if affected > 0 && size > 0 {
		_, _ = s.db.Exec(`
UPDATE nodes
SET free = CASE WHEN free + ? > capacity THEN capacity ELSE free + ? END
WHERE id = ?
`, size, size, nodeID)
	}
}

func (s *State) ReplicationTasks(limit int) []ReplicationTask {
	if limit <= 0 {
		limit = 100
	}
	cutoff := time.Now().UTC().Add(-nodeOfflineAfter).Format(time.RFC3339)
	rows, err := s.db.Query(`
SELECT
	fc.hash,
	MAX(fc.size) AS size,
	MAX(f.replication) AS desired,
	COUNT(DISTINCT CASE WHEN n.last_seen >= ? THEN cl.node_id END) AS active
FROM file_chunks fc
JOIN files f ON f.id = fc.file_id
LEFT JOIN chunk_locations cl ON cl.hash = fc.hash
LEFT JOIN nodes n ON n.id = cl.node_id
GROUP BY fc.hash
HAVING active < desired
ORDER BY desired - active DESC, fc.hash
LIMIT ?
`, cutoff, limit)
	if err != nil {
		return nil
	}

	tasks := make([]ReplicationTask, 0)
	for rows.Next() {
		var task ReplicationTask
		if err := rows.Scan(&task.Hash, &task.Size, &task.Desired, &task.Active); err != nil {
			continue
		}
		task.Missing = task.Desired - task.Active
		tasks = append(tasks, task)
	}
	_ = rows.Close()

	for i := range tasks {
		tasks[i].Sources = onlineNodes(s.ChunkLocations(tasks[i].Hash))
		tasks[i].Candidates = s.PlanChunkSpread(tasks[i].Hash, tasks[i].Size, tasks[i].Missing, nil)
	}
	return tasks
}

func (s *State) PruneTasks(limit int) []PruneTask {
	if limit <= 0 {
		limit = 100
	}
	cutoff := time.Now().UTC().Add(-nodeOfflineAfter).Format(time.RFC3339)
	rows, err := s.db.Query(`
SELECT
	fc.hash,
	MAX(fc.size) AS size,
	MAX(f.replication) AS desired,
	COUNT(DISTINCT CASE WHEN n.last_seen >= ? THEN cl.node_id END) AS active
FROM file_chunks fc
JOIN files f ON f.id = fc.file_id
LEFT JOIN chunk_locations cl ON cl.hash = fc.hash
LEFT JOIN nodes n ON n.id = cl.node_id
GROUP BY fc.hash
HAVING active > desired
ORDER BY active - desired DESC, fc.hash
LIMIT ?
`, cutoff, limit)
	if err != nil {
		return nil
	}

	tasks := make([]PruneTask, 0)
	for rows.Next() {
		var task PruneTask
		if err := rows.Scan(&task.Hash, &task.Size, &task.Desired, &task.Active); err != nil {
			continue
		}
		task.Extra = task.Active - task.Desired
		tasks = append(tasks, task)
	}
	_ = rows.Close()

	for i := range tasks {
		locations := onlineNodes(s.ChunkLocations(tasks[i].Hash))
		sort.Slice(locations, func(left int, right int) bool {
			if locations[left].Free != locations[right].Free {
				return locations[left].Free < locations[right].Free
			}
			if locations[left].LastSeen != locations[right].LastSeen {
				return locations[left].LastSeen < locations[right].LastSeen
			}
			return locations[left].ID < locations[right].ID
		})
		tasks[i].Locations = locations
	}
	return tasks
}

func (s *State) OrphanChunkTasks(limit int) []OrphanChunkTask {
	if limit <= 0 {
		limit = 100
	}
	cutoff := time.Now().UTC().Add(-nodeOfflineAfter).Format(time.RFC3339)
	// Osierocony chunk to taki, ktory nadal ma lokalizacje na nodzie, ale nie nalezy juz do zadnego backupu.
	rows, err := s.db.Query(`
SELECT
	cl.hash,
	cl.size,
	n.id,
	n.address,
	n.capacity,
	n.free,
	n.last_seen,
	n.status
FROM chunk_locations cl
JOIN nodes n ON n.id = cl.node_id
LEFT JOIN file_chunks fc ON fc.hash = cl.hash
WHERE fc.hash IS NULL
  AND n.last_seen >= ?
ORDER BY cl.hash, n.id
LIMIT ?
`, cutoff, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	tasks := make([]OrphanChunkTask, 0)
	for rows.Next() {
		var task OrphanChunkTask
		if err := rows.Scan(
			&task.Hash,
			&task.Size,
			&task.Node.ID,
			&task.Node.Address,
			&task.Node.Capacity,
			&task.Node.Free,
			&task.Node.LastSeen,
			&task.Node.Status,
		); err != nil {
			continue
		}
		task.Node = withDerivedStatus(task.Node)
		tasks = append(tasks, task)
	}
	return tasks
}

func (s *State) fileChunks(fileID string) []protocol.ChunkRef {
	rows, err := s.db.Query(`SELECT chunk_index, hash, size FROM file_chunks WHERE file_id = ? ORDER BY chunk_index`, fileID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	chunks := make([]protocol.ChunkRef, 0)
	for rows.Next() {
		var chunk protocol.ChunkRef
		if err := rows.Scan(&chunk.Index, &chunk.Hash, &chunk.Size); err == nil {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func (s *State) markOffline() {
}

func (s *State) ensureColumn(table string, column string, alterSQL string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}

	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			_ = rows.Close()
			return nil
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	_, err = s.db.Exec(alterSQL)
	return err
}

func withDerivedStatus(node protocol.NodeInfo) protocol.NodeInfo {
	lastSeen, err := time.Parse(time.RFC3339, node.LastSeen)
	if err == nil && time.Since(lastSeen) > nodeOfflineAfter {
		node.Status = "offline"
	}
	return node
}

func onlineNodes(nodes []protocol.NodeInfo) []protocol.NodeInfo {
	out := make([]protocol.NodeInfo, 0, len(nodes))
	for _, node := range nodes {
		if node.Status == "online" {
			out = append(out, node)
		}
	}
	return out
}

func normalizedReplication(replication int) int {
	if replication <= 0 {
		return 3
	}
	return replication
}

func normalizedRetention(retention int) int {
	if retention < 0 {
		return 0
	}
	return retention
}

func dedupRatio(reusedBytes int64, totalBytes int64) float64 {
	if reusedBytes <= 0 || totalBytes <= 0 {
		return 0
	}
	return float64(reusedBytes) / float64(totalBytes)
}
