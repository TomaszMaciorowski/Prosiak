package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"backup/internal/protocol"
)

const defaultChunkSize = 512 * 1024

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "backup":
		if err := backup(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "restore":
		if err := restore(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage:")
	fmt.Println("  backupctl backup  -server http://localhost:8080 -file path [-name backup-name] [-retention 5] [-chunk-size 524288] [-replication 3]")
	fmt.Println("  backupctl restore -server http://localhost:8080 -id file-id -out path")
}

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "central server url")
	filePath := fs.String("file", "", "file to backup")
	backupName := fs.String("name", "", "logical backup name used for versioning")
	retention := fs.Int("retention", 0, "keep last N versions for this backup name, 0 disables retention")
	chunkSize := fs.Int64("chunk-size", defaultChunkSize, "chunk size in bytes")
	replication := fs.Int("replication", 3, "copies per chunk")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *filePath == "" {
		return errors.New("-file is required")
	}
	if *chunkSize <= 0 {
		return errors.New("-chunk-size must be positive")
	}

	if *retention < 0 {
		return errors.New("-retention must be zero or positive")
	}

	manifest, err := uploadToServer(*serverURL, *filePath, *backupName, *retention, *chunkSize, *replication)
	if err != nil {
		return err
	}

	fmt.Printf("backup saved: name=%s version=%d file_id=%s chunks=%d size=%d dedup=%.1f%% reused=%d new=%d\n",
		manifest.BackupName,
		manifest.Version,
		manifest.ID,
		len(manifest.Chunks),
		manifest.Size,
		manifest.DedupRatio*100,
		manifest.DedupReusedBytes,
		manifest.DedupNewBytes)
	if len(manifest.RetentionDeleted) > 0 {
		fmt.Printf("retention deleted old versions: %v\n", manifest.RetentionDeleted)
	}
	return nil
}

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "central server url")
	fileID := fs.String("id", "", "file id")
	outPath := fs.String("out", "", "restore output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fileID == "" || *outPath == "" {
		return errors.New("-id and -out are required")
	}

	out, err := os.Create(*outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(*serverURL + "/files/" + *fileID + "/download")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("restored: id=%s out=%s size=%d\n", *fileID, *outPath, n)
	return nil
}

func uploadToServer(serverURL string, filePath string, backupName string, retention int, chunkSize int64, replication int) (protocol.FileManifest, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return protocol.FileManifest{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		part, err := mw.CreateFormFile("file", filePath)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, f); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	endpoint, err := url.Parse(serverURL + "/backups")
	if err != nil {
		return protocol.FileManifest{}, err
	}
	q := endpoint.Query()
	q.Set("chunk_size", strconv.FormatInt(chunkSize, 10))
	q.Set("replication", strconv.Itoa(replication))
	if backupName != "" {
		q.Set("backup_name", backupName)
	}
	if retention > 0 {
		q.Set("retention", strconv.Itoa(retention))
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPost, endpoint.String(), pr)
	if err != nil {
		return protocol.FileManifest{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return protocol.FileManifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return protocol.FileManifest{}, fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	var manifest protocol.FileManifest
	return manifest, json.NewDecoder(resp.Body).Decode(&manifest)
}
