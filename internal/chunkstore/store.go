package chunkstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrHashMismatch = errors.New("chunk hash mismatch")

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
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

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, nil
}

func (s *Store) Open(hash string) (*os.File, error) {
	path, err := s.Path(hash)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (s *Store) Delete(hash string) error {
	path, err := s.Path(hash)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
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
	return files, bytes, nil
}

func (s *Store) Used() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
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

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
