package main

import (
	"archive/tar"
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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"backup/internal/protocol"
	"backup/internal/tlsconfig"
)

const defaultChunkSize = 512 * 1024

type clientConfig struct {
	Server             string      `json:"server"`
	ChunkSize          int64       `json:"chunk_size"`
	Replication        int         `json:"replication"`
	Retention          int         `json:"retention"`
	CACertFile         string      `json:"ca_cert_file"`
	InsecureSkipVerify bool        `json:"insecure_skip_verify"`
	Jobs               []backupJob `json:"jobs"`
}

type backupJob struct {
	Name        string   `json:"name"`
	Paths       []string `json:"paths"`
	Exclude     []string `json:"exclude"`
	ChunkSize   int64    `json:"chunk_size"`
	Replication int      `json:"replication"`
	Retention   int      `json:"retention"`
}

var httpClient = &http.Client{}

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
	case "backup-job":
		if err := backupConfiguredJob(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "list":
		if err := listBackups(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "restore":
		if err := restore(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "restore-job":
		if err := restoreConfiguredJob(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage:")
	fmt.Println("  backupctl backup  -server http://localhost:8080 -file path [-name backup-name] [-retention 5] [-chunk-size 524288] [-replication 3] [-ca-cert-file ca.pem]")
	fmt.Println("  backupctl backup-job -config configs/client.json [-job documents] [-ca-cert-file ca.pem]")
	fmt.Println("  backupctl list -server http://localhost:8080 [-ca-cert-file ca.pem]")
	fmt.Println("  backupctl restore -server http://localhost:8080 -id file-id -out path [-ca-cert-file ca.pem]")
	fmt.Println("  backupctl restore-job -server http://localhost:8080 -id file-id -out directory [-ca-cert-file ca.pem]")
}

func addTLSFlags(fs *flag.FlagSet) (*string, *bool) {
	caCertFile := fs.String("ca-cert-file", "", "CA certificate file trusted when connecting to the server")
	insecureSkipVerify := fs.Bool("insecure-skip-verify", false, "skip TLS certificate verification when connecting to the server")
	return caCertFile, insecureSkipVerify
}

func configureHTTPClient(caCertFile string, insecureSkipVerify bool) error {
	client, err := tlsconfig.HTTPClient(tlsconfig.ClientConfig{
		CACertFile:         caCertFile,
		InsecureSkipVerify: insecureSkipVerify,
	}, 0)
	if err != nil {
		return err
	}
	httpClient = client
	return nil
}

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "central server url")
	filePath := fs.String("file", "", "file to backup")
	backupName := fs.String("name", "", "logical backup name used for versioning")
	retention := fs.Int("retention", 0, "keep last N versions for this backup name, 0 disables retention")
	chunkSize := fs.Int64("chunk-size", defaultChunkSize, "chunk size in bytes")
	replication := fs.Int("replication", 3, "copies per chunk")
	caCertFile, insecureSkipVerify := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := configureHTTPClient(*caCertFile, *insecureSkipVerify); err != nil {
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

	f, err := os.Open(*filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	manifest, err := uploadToServer(*serverURL, *filePath, f, *backupName, *retention, *chunkSize, *replication)
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

func backupConfiguredJob(args []string) error {
	fs := flag.NewFlagSet("backup-job", flag.ExitOnError)
	configPath := fs.String("config", "configs/client.json", "JSON client config file")
	jobName := fs.String("job", "", "job name from config")
	caCertFile, insecureSkipVerify := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := readClientConfig(*configPath)
	if err != nil {
		return err
	}
	job, ok, err := selectJob(cfg, *jobName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("job %q not found in %s", *jobName, *configPath)
	}
	job.applyDefaults(cfg)
	if err := job.validate(); err != nil {
		return err
	}
	serverURL := cfg.Server
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}
	if *caCertFile != "" {
		cfg.CACertFile = *caCertFile
	}
	if *insecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if err := configureHTTPClient(cfg.CACertFile, cfg.InsecureSkipVerify); err != nil {
		return err
	}

	// Tar leci strumieniem, zeby nie robic lokalnego pliku tymczasowego dla duzych katalogow.
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTarJob(pw, job))
	}()

	fileName := job.Name + ".tar"
	manifest, err := uploadToServer(serverURL, fileName, pr, job.Name, job.Retention, job.ChunkSize, job.Replication)
	if err != nil {
		return err
	}
	fmt.Printf("backup job saved: job=%s version=%d file_id=%s chunks=%d size=%d dedup=%.1f%% reused=%d new=%d\n",
		job.Name,
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
	caCertFile, insecureSkipVerify := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := configureHTTPClient(*caCertFile, *insecureSkipVerify); err != nil {
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

	resp, err := httpClient.Get(*serverURL + "/files/" + *fileID + "/download")
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

func listBackups(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "central server url")
	caCertFile, insecureSkipVerify := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := configureHTTPClient(*caCertFile, *insecureSkipVerify); err != nil {
		return err
	}

	resp, err := httpClient.Get(*serverURL + "/files")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}

	var files []protocol.FileManifest
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Println("no backups")
		return nil
	}

	fmt.Printf("%-24s %-8s %-28s %-12s %-10s %-20s %s\n", "BACKUP", "VERSION", "FILE_ID", "SIZE", "DEDUP", "CREATED_AT", "NAME")
	for _, file := range files {
		fmt.Printf("%-24s %-8d %-28s %-12s %-10.1f %-20s %s\n",
			file.BackupName,
			file.Version,
			file.ID,
			formatBytes(file.Size),
			file.DedupRatio*100,
			file.CreatedAt,
			file.Name)
	}
	return nil
}

func restoreConfiguredJob(args []string) error {
	fs := flag.NewFlagSet("restore-job", flag.ExitOnError)
	serverURL := fs.String("server", "http://localhost:8080", "central server url")
	fileID := fs.String("id", "", "file id")
	outPath := fs.String("out", "", "restore output directory")
	caCertFile, insecureSkipVerify := addTLSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := configureHTTPClient(*caCertFile, *insecureSkipVerify); err != nil {
		return err
	}
	if *fileID == "" || *outPath == "" {
		return errors.New("-id and -out are required")
	}

	resp, err := httpClient.Get(*serverURL + "/files/" + *fileID + "/download")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	if err := extractTar(resp.Body, *outPath); err != nil {
		return err
	}
	fmt.Printf("restored job: id=%s out=%s\n", *fileID, *outPath)
	return nil
}

func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	unit := ""
	for _, candidate := range units {
		value /= 1024
		unit = candidate
		if value < 1024 {
			break
		}
	}
	if value >= 10 {
		return fmt.Sprintf("%.0f %s", value, unit)
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

func uploadToServer(serverURL string, fileName string, r io.Reader, backupName string, retention int, chunkSize int64, replication int) (protocol.FileManifest, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		defer pw.Close()
		part, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, r); err != nil {
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
	resp, err := httpClient.Do(req)
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

func readClientConfig(path string) (clientConfig, error) {
	var cfg clientConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(data, &cfg)
}

func findJob(cfg clientConfig, name string) (backupJob, bool) {
	for _, job := range cfg.Jobs {
		if strings.EqualFold(job.Name, name) {
			return job, true
		}
	}
	return backupJob{}, false
}

func selectJob(cfg clientConfig, name string) (backupJob, bool, error) {
	if len(cfg.Jobs) == 0 {
		return backupJob{}, false, errors.New("config must contain at least one job")
	}
	if name != "" {
		job, ok := findJob(cfg, name)
		return job, ok, nil
	}
	if len(cfg.Jobs) == 1 {
		return cfg.Jobs[0], true, nil
	}

	// Przy wielu jobach nie zgadujemy. Lepiej zmusic usera do wyboru niz zbackupowac zly katalog.
	names := make([]string, 0, len(cfg.Jobs))
	for _, job := range cfg.Jobs {
		names = append(names, job.Name)
	}
	return backupJob{}, false, fmt.Errorf("-job is required when config contains multiple jobs: %s", strings.Join(names, ", "))
}

func (j *backupJob) applyDefaults(cfg clientConfig) {
	if j.ChunkSize == 0 {
		j.ChunkSize = cfg.ChunkSize
	}
	if j.ChunkSize == 0 {
		j.ChunkSize = defaultChunkSize
	}
	if j.Replication == 0 {
		j.Replication = cfg.Replication
	}
	if j.Replication == 0 {
		j.Replication = 3
	}
	if j.Retention == 0 {
		j.Retention = cfg.Retention
	}
}

func (j backupJob) validate() error {
	if j.Name == "" {
		return errors.New("job name is required")
	}
	if len(j.Paths) == 0 {
		return fmt.Errorf("job %q must have at least one path", j.Name)
	}
	if j.ChunkSize <= 0 {
		return fmt.Errorf("job %q chunk_size must be positive", j.Name)
	}
	if j.Replication <= 0 {
		return fmt.Errorf("job %q replication must be positive", j.Name)
	}
	if j.Retention < 0 {
		return fmt.Errorf("job %q retention must be zero or positive", j.Name)
	}
	return nil
}

func writeTarJob(w io.Writer, job backupJob) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	matcher, err := newExcludeMatcher(job.Exclude)
	if err != nil {
		return err
	}

	roots, err := tarRoots(job.Paths)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if err := addRootToTar(tw, root, matcher); err != nil {
			return err
		}
	}
	return nil
}

type tarRoot struct {
	source string
	parent string
	prefix string
}

func tarRoots(paths []string) ([]tarRoot, error) {
	roots := make([]tarRoot, 0, len(paths))
	used := make(map[string]int)
	for _, raw := range paths {
		cleaned := filepath.Clean(raw)
		info, err := os.Lstat(cleaned)
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(abs)
		if base == "." || base == string(filepath.Separator) {
			base = "root"
		}
		used[base]++
		prefix := base
		if used[base] > 1 {
			prefix = fmt.Sprintf("%s-%d", base, used[base])
		}
		// W archiwum trzymamy nazwe katalogu startowego, a nie cala lokalna sciezke z dysku.
		parent := filepath.Dir(abs)
		if !info.IsDir() {
			parent = filepath.Dir(abs)
		}
		roots = append(roots, tarRoot{source: abs, parent: parent, prefix: prefix})
	}
	sort.Slice(roots, func(i, j int) bool {
		return roots[i].source < roots[j].source
	})
	return roots, nil
}

func addRootToTar(tw *tar.Writer, root tarRoot, matcher excludeMatcher) error {
	return filepath.WalkDir(root.source, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root.parent, path)
		if err != nil {
			return err
		}
		tarPath := filepath.ToSlash(rel)
		if tarPath == "." {
			tarPath = root.prefix
		}
		if matcher.match(tarPath, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return err
		}
		header.Name = tarPath
		if info.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

type excludeMatcher struct {
	patterns []excludePattern
}

type excludePattern struct {
	raw string
	re  *regexp.Regexp
}

func newExcludeMatcher(patterns []string) (excludeMatcher, error) {
	m := excludeMatcher{patterns: make([]excludePattern, 0, len(patterns))}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		pattern = strings.Trim(pattern, "/")
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile("^" + globToRegexp(pattern) + "$")
		if err != nil {
			return m, fmt.Errorf("invalid exclude pattern %q: %v", pattern, err)
		}
		m.patterns = append(m.patterns, excludePattern{raw: pattern, re: re})
	}
	return m, nil
}

func (m excludeMatcher) match(path string, isDir bool) bool {
	path = strings.Trim(filepath.ToSlash(path), "/")
	for _, pattern := range m.patterns {
		if pattern.re.MatchString(path) {
			return true
		}
		if isDir && pattern.re.MatchString(path+"/") {
			return true
		}
	}
	return false
}

func globToRegexp(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '{', '}', '[', ']', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteByte(pattern[i])
		default:
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}

func extractTar(r io.Reader, outDir string) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	outAbs, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeExtractPath(outAbs, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			continue
		}
	}
}

func safeExtractPath(outAbs string, name string) (string, error) {
	// Tar potrafi zawierac sciezki typu ../plik. Tego nie wolno wypuscic poza katalog restore.
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	target := filepath.Join(outAbs, cleaned)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if targetAbs != outAbs && !strings.HasPrefix(targetAbs, outAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return targetAbs, nil
}
