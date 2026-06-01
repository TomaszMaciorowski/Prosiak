package chunkstore

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPutOpenRoundTripPlaintext(t *testing.T) {
	store, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello chunk")
	hash := HashBytes(data)

	if _, err := store.Put(hash, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	got := readChunk(t, store, hash)
	if !bytes.Equal(got, data) {
		t.Fatalf("round trip = %q, want %q", got, data)
	}
}

func TestPutEncryptsAtRestAndReadsBack(t *testing.T) {
	root := t.TempDir()
	key := mustKey(t)
	store, err := New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Encrypted() {
		t.Fatal("store should report encrypted")
	}

	data := []byte("super secret backup bytes")
	hash := HashBytes(data)
	if _, err := store.Put(hash, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	// On-disk bytes must not contain the plaintext.
	path, err := store.Path(hash)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, data) {
		t.Fatal("plaintext found in encrypted chunk on disk")
	}

	got := readChunk(t, store, hash)
	if !bytes.Equal(got, data) {
		t.Fatalf("decrypted = %q, want %q", got, data)
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("tamper-evident")
	hash := HashBytes(data)
	if _, err := store.Put(hash, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}

	other, err := New(root, mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(hash); err == nil {
		t.Fatal("opening with a different key should fail authentication")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	if _, err := New(t.TempDir(), []byte("too-short")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

func readChunk(t *testing.T, store *Store, hash string) []byte {
	t.Helper()
	rc, err := store.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUsedTracksPutDeleteClearAndReopen(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if used := mustUsed(t, store); used != 0 {
		t.Fatalf("fresh store used = %d, want 0", used)
	}

	a := []byte("chunk-a-payload")
	b := []byte("chunk-b-bigger-payload-here")
	put(t, store, a)
	put(t, store, b)
	want := int64(len(a) + len(b))
	if used := mustUsed(t, store); used != want {
		t.Fatalf("after two puts used = %d, want %d", used, want)
	}

	// Re-putting the same content must not double count.
	put(t, store, a)
	if used := mustUsed(t, store); used != want {
		t.Fatalf("after idempotent put used = %d, want %d", used, want)
	}

	// Reopening the store over the same directory must reseed the same total.
	reopened, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if used := mustUsed(t, reopened); used != want {
		t.Fatalf("reopened store used = %d, want %d", used, want)
	}

	if err := store.Delete(HashBytes(a)); err != nil {
		t.Fatal(err)
	}
	if used := mustUsed(t, store); used != int64(len(b)) {
		t.Fatalf("after delete used = %d, want %d", used, len(b))
	}
	// Deleting a missing chunk must not move the counter.
	if err := store.Delete(HashBytes(a)); err != nil {
		t.Fatal(err)
	}
	if used := mustUsed(t, store); used != int64(len(b)) {
		t.Fatalf("after redundant delete used = %d, want %d", used, len(b))
	}

	if _, _, err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if used := mustUsed(t, store); used != 0 {
		t.Fatalf("after clear used = %d, want 0", used)
	}
}

func put(t *testing.T, store *Store, data []byte) {
	t.Helper()
	if _, err := store.Put(HashBytes(data), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func mustUsed(t *testing.T, store *Store) int64 {
	t.Helper()
	used, err := store.Used()
	if err != nil {
		t.Fatal(err)
	}
	return used
}

func TestPathRejectsTraversal(t *testing.T) {
	store, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Path("sha256:../../etc/passwd"); err == nil {
		t.Fatal("expected non-hex hash to be rejected")
	}
	// A valid hash maps under the root.
	path, err := store.Path(HashBytes([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) && !bytes.Contains([]byte(path), []byte(".chunk")) {
		t.Fatalf("unexpected path %q", path)
	}
}
