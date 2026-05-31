package protocol

type NodeRegisterRequest struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Capacity int64  `json:"capacity"`
	Free     int64  `json:"free"`
}

type NodeInfo struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Capacity int64  `json:"capacity"`
	Free     int64  `json:"free"`
	LastSeen string `json:"last_seen"`
	Status   string `json:"status"`
}

type ChunkRef struct {
	Index int    `json:"index"`
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
}

type FileManifest struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	BackupName       string     `json:"backup_name"`
	Version          int        `json:"version"`
	Retention        int        `json:"retention"`
	CreatedAt        string     `json:"created_at"`
	Size             int64      `json:"size"`
	ChunkSize        int64      `json:"chunk_size"`
	Replication      int        `json:"replication"`
	DedupNewBytes    int64      `json:"dedup_new_bytes"`
	DedupReusedBytes int64      `json:"dedup_reused_bytes"`
	DedupRatio       float64    `json:"dedup_ratio"`
	Chunks           []ChunkRef `json:"chunks"`
	RetentionDeleted []string   `json:"retention_deleted,omitempty"`
}

type CreateFileRequest struct {
	Name             string     `json:"name"`
	BackupName       string     `json:"backup_name"`
	Retention        int        `json:"retention"`
	Size             int64      `json:"size"`
	ChunkSize        int64      `json:"chunk_size"`
	Replication      int        `json:"replication"`
	DedupNewBytes    int64      `json:"dedup_new_bytes"`
	DedupReusedBytes int64      `json:"dedup_reused_bytes"`
	Chunks           []ChunkRef `json:"chunks"`
}

type UpdateReplicationRequest struct {
	Replication int `json:"replication"`
}

type ChunkPlanRequest struct {
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	Replication int    `json:"replication"`
}

type ChunkPlanResponse struct {
	Hash  string     `json:"hash"`
	Nodes []NodeInfo `json:"nodes"`
}

type ChunkConfirmRequest struct {
	Hash   string `json:"hash"`
	Size   int64  `json:"size"`
	NodeID string `json:"node_id"`
}

type ChunkLocationsResponse struct {
	Hash  string     `json:"hash"`
	Nodes []NodeInfo `json:"nodes"`
}

type ChunkAvailability struct {
	Index     int        `json:"index"`
	Hash      string     `json:"hash"`
	Size      int64      `json:"size"`
	Desired   int        `json:"desired"`
	Active    int        `json:"active"`
	Total     int        `json:"total"`
	Status    string     `json:"status"`
	Locations []NodeInfo `json:"locations"`
}

type FileAvailability struct {
	FileID           string              `json:"file_id"`
	Name             string              `json:"name"`
	BackupName       string              `json:"backup_name"`
	Version          int                 `json:"version"`
	Retention        int                 `json:"retention"`
	CreatedAt        string              `json:"created_at"`
	Size             int64               `json:"size"`
	ChunkSize        int64               `json:"chunk_size"`
	Replication      int                 `json:"replication"`
	DedupNewBytes    int64               `json:"dedup_new_bytes"`
	DedupReusedBytes int64               `json:"dedup_reused_bytes"`
	DedupRatio       float64             `json:"dedup_ratio"`
	Chunks           []ChunkAvailability `json:"chunks"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
