package server

import (
	"context"
	"log"
	"time"
)

func StartReplicationScheduler(ctx context.Context, state *State, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runReplicationOnce(state)
			}
		}
	}()
}

func runReplicationOnce(state *State) {
	cleanupOrphanChunks(state)
	pruneExtraCopies(state)
	replicateMissingCopies(state)
}

func cleanupOrphanChunks(state *State) {
	tasks := state.OrphanChunkTasks(100)
	for _, task := range tasks {
		if err := deleteChunk(task.Node.Address, task.Hash); err != nil {
			log.Printf("orphan cleanup failed: hash=%s node=%s: %v", task.Hash, task.Node.ID, err)
			continue
		}
		state.RemoveChunkLocation(task.Hash, task.Node.ID)
		log.Printf("orphan cleanup deleted: hash=%s node=%s size=%d", task.Hash, task.Node.ID, task.Size)
	}
}

func replicateMissingCopies(state *State) {
	tasks := state.ReplicationTasks(100)
	for _, task := range tasks {
		if task.Missing <= 0 {
			continue
		}
		if len(task.Sources) == 0 {
			log.Printf("replication waiting: hash=%s desired=%d active=%d no online source", task.Hash, task.Desired, task.Active)
			continue
		}
		if len(task.Candidates) == 0 {
			log.Printf("replication waiting: hash=%s desired=%d active=%d no target node", task.Hash, task.Desired, task.Active)
			continue
		}

		data, err := fetchFromAny(task.Sources, task.Hash)
		if err != nil {
			log.Printf("replication fetch failed: hash=%s: %v", task.Hash, err)
			continue
		}
		if got := hashData(data); got != task.Hash {
			log.Printf("replication hash mismatch: expected=%s got=%s", task.Hash, got)
			continue
		}

		copied := 0
		for _, target := range task.Candidates {
			if copied >= task.Missing {
				break
			}
			if err := putChunk(target.Address, task.Hash, data); err != nil {
				log.Printf("replication upload failed: hash=%s target=%s: %v", task.Hash, target.ID, err)
				continue
			}
			if err := state.ConfirmChunk(task.Hash, task.Size, target.ID); err != nil {
				log.Printf("replication confirm failed: hash=%s target=%s: %v", task.Hash, target.ID, err)
				continue
			}
			copied++
			log.Printf("replication copied: hash=%s target=%s active=%d desired=%d", task.Hash, target.ID, task.Active+copied, task.Desired)
		}
	}
}

func pruneExtraCopies(state *State) {
	tasks := state.PruneTasks(100)
	for _, task := range tasks {
		if task.Extra <= 0 {
			continue
		}
		removed := 0
		for _, node := range task.Locations {
			if removed >= task.Extra {
				break
			}
			if task.Active-removed <= task.Desired {
				break
			}
			if err := deleteChunk(node.Address, task.Hash); err != nil {
				log.Printf("replication prune failed: hash=%s node=%s: %v", task.Hash, node.ID, err)
				continue
			}
			state.RemoveChunkLocation(task.Hash, node.ID)
			removed++
			log.Printf("replication pruned: hash=%s node=%s active=%d desired=%d", task.Hash, node.ID, task.Active-removed, task.Desired)
		}
	}
}
