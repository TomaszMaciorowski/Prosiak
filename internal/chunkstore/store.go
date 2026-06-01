package chunkstore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrHashMismatch = errors.New("chunk hash mismatch")

type Store struct {
	root string
	aead cipher.AEAD // optional at-rest encryption; nil means chunks are stored as plaintext

	// used is the on-disk byte total, kept in memory so the hot path (capacity
	// checks on every PUT, heartbeats) never has to walk the whole storage tree.
	mu   sync.Mutex
	used int64
}

// New opens a chunk store rooted at root. If key is non-empty it must be 32
// bytes (AES-256) and every chunk is encrypted with AES-256-GCM before it
// touches the disk, so a stolen storage directory leaks no backup data.
func New(root string, key []byte) (*Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	store := &Store{root: root}
	if len(key) > 0 {
		if len(key) != 32 {
			return nil, fmt.Errorf("storage key must be 32 bytes, got %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		store.aead = gcm
	}
	// Seed the in-memory usage counter once; from here on it is maintained
	// incrementally by Put/Delete/Clear.
	used, err := store.scanUsed()
	if err != nil {
		return nil, err
	}
	store.used = used
	return store, nil
}

// Encrypted reports whether chunks are stored encrypted at rest.
func (s *Store) Encrypted() bool {
	return s.aead != nil
}

// scanUsed walks the storage tree and sums the size of every chunk file. It is
// only called once, at startup, to seed the cached counter.
func (s *Store) scanUsed() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".chunk") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// addUsed adjusts the cached usage counter, clamping at zero.
func (s *Store) addUsed(delta int64) {
	s.mu.Lock()
	s.used += delta
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

// fileSize returns the size of path, or 0 if it does not exist.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *Store) Path(hash string) (string, error) {
	clean := strings.TrimPrefix(hash, "sha256:")
	if len(clean) < 2 {
		return "", fmt.Errorf("invalid hash %q", hash)
	}
	if _, err := hex.DecodeString(clean); err != nil {
		return "", fmt.Errorf("invalid hash %q: %w", hash, err)
	}
	return filepath.Join(s.root, clean[:2], clean+".chunk"), nil
}

func (s *Store) Has(hash string) bool {
	path, err := s.Path(hash)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *Store) Put(hash string, r io.Reader) (int64, error) {
	if s.aead != nil {
		return s.putEncrypted(hash, r)
	}
	return s.putPlain(hash, r)
}

// putPlain streams the chunk straight to disk, hashing on the way through so it
// never has to hold the whole chunk in memory.
func (s *Store) putPlain(hash string, r io.Reader) (int64, error) {
	path, err := s.Path(hash)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, err
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return n, closeErr
	}

	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != hash {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("%w: expected %s got %s", ErrHashMismatch, hash, got)
	}

	oldSize := fileSize(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	s.addUsed(fileSize(path) - oldSize)
	return n, nil
}

// putEncrypted buffers the chunk (chunks are size-bounded), verifies the hash of
// the plaintext, then writes nonce||ciphertext sealed with AES-256-GCM.
func (s *Store) putEncrypted(hash string, r io.Reader) (int64, error) {
	path, err := s.Path(hash)
	if err != nil {
		return 0, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return int64(len(data)), err
	}
	n := int64(len(data))

	got := HashBytes(data)
	if got != hash {
		return n, fmt.Errorf("%w: expected %s got %s", ErrHashMismatch, hash, got)
	}

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return n, err
	}
	sealed := s.aead.Seal(nonce, nonce, data, nil)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return n, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0644); err != nil {
		return n, err
	}
	oldSize := fileSize(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	s.addUsed(fileSize(path) - oldSize)
	return n, nil
}

func (s *Store) Open(hash string) (io.ReadCloser, error) {
	path, err := s.Path(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if s.aead == nil {
		return f, nil
	}

	defer f.Close()
	sealed, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	nonceSize := s.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("chunk %s is too short to be encrypted", hash)
	}
	plain, err := s.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt chunk %s: %w", hash, err)
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
}

func (s *Store) Delete(hash string) error {
	path, err := s.Path(hash)
	if err != nil {
		return err
	}
	size := fileSize(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.addUsed(-size)
	return nil
}

func (s *Store) Clear() (int64, int64, error) {
	cleanRoot := filepath.Clean(s.root)
	if cleanRoot == "." || cleanRoot == string(filepath.Separator) || cleanRoot == filepath.VolumeName(cleanRoot)+string(filepath.Separator) {
		return 0, 0, fmt.Errorf("refusing to clear unsafe storage root %q", s.root)
	}

	var files int64
	var bytes int64
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return files, bytes, err
	}
	if err := os.RemoveAll(s.root); err != nil {
		return files, bytes, err
	}
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return files, bytes, err
	}
	s.mu.Lock()
	s.used = 0
	s.mu.Unlock()
	return files, bytes, nil
}

// Used returns the cached on-disk byte total. It no longer walks the storage
// tree, so it stays O(1) no matter how many chunks the node holds.
func (s *Store) Used() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used, nil
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
