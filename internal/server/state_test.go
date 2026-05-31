package server

import (
	"path/filepath"
	"testing"

	"backup/internal/protocol"
)

func TestApplyRetentionKeepsNewestVersionsAndOnlyReturnsUnusedChunks(t *testing.T) {
	state, err := NewState(filepath.Join(t.TempDir(), "backup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	create := func(name string, chunks []protocol.ChunkRef) protocol.FileManifest {
		return state.CreateFile(protocol.CreateFileRequest{
			Name:        "data.bin",
			BackupName:  name,
			Retention:   2,
			Size:        int64(len(chunks)),
			ChunkSize:   1,
			Replication: 3,
			Chunks:      chunks,
		})
	}

	v1 := create("daily", []protocol.ChunkRef{
		{Index: 0, Hash: "sha256:shared", Size: 1},
		{Index: 1, Hash: "sha256:old-only", Size: 1},
	})
	v2 := create("daily", []protocol.ChunkRef{
		{Index: 0, Hash: "sha256:shared", Size: 1},
		{Index: 1, Hash: "sha256:v2-only", Size: 1},
	})
	v3 := create("daily", []protocol.ChunkRef{
		{Index: 0, Hash: "sha256:shared", Size: 1},
		{Index: 1, Hash: "sha256:v3-only", Size: 1},
	})

	deleted, unused := state.ApplyRetention("daily", 2)
	if len(deleted) != 1 || deleted[0].ID != v1.ID {
		t.Fatalf("deleted versions = %#v, want only %s", deleted, v1.ID)
	}
	if len(unused) != 1 || unused[0] != "sha256:old-only" {
		t.Fatalf("unused chunks = %#v, want only old-only", unused)
	}
	if _, ok := state.File(v1.ID); ok {
		t.Fatalf("oldest version %s still exists", v1.ID)
	}
	if _, ok := state.File(v2.ID); !ok {
		t.Fatalf("version %s was deleted", v2.ID)
	}
	if _, ok := state.File(v3.ID); !ok {
		t.Fatalf("version %s was deleted", v3.ID)
	}
}
