package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gzipString(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseScanListFiltersAndRoots(t *testing.T) {
	manifest := strings.Join([]string{
		"2024-01-02 03:04 /电影/a.txt",
		"2024-01-02 03:05 /电视剧/b.txt",
		"",
		"2024-01-02 03:06 /电影/sub/c d.txt",
	}, "\n")
	entries, topLevel, malformed, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 0 {
		t.Fatalf("malformed = %d, want 0", malformed)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want the two /电影 paths", entries)
	}
	want, _ := time.Parse(manifestTimeLayout, "2024-01-02 03:06")
	if got := entries["/电影/sub/c d.txt"]; got != want.Unix() {
		t.Fatalf("timestamp for spaced path = %v, want %v", got, want.Unix())
	}
	if !topLevel["电视剧"] || !topLevel["电影"] {
		t.Fatalf("topLevel = %v, want both roots present", topLevel)
	}
}

func TestParseScanListNeutralizesTraversal(t *testing.T) {
	manifest := strings.Join([]string{
		"2024-01-02 03:04 /电影/../../pwned.txt",
		"2024-01-02 03:04 /电影/a/../../.metadata.db",
		"2024-01-02 03:04 /电影/a/../b.txt",
	}, "\n")
	entries, _, malformed, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 0 {
		t.Fatalf("malformed = %d, want 0", malformed)
	}
	for path := range entries {
		if !strings.HasPrefix(path, "/电影/") {
			t.Fatalf("entry %q escapes the selected root", path)
		}
	}
	if _, ok := entries["/电影/b.txt"]; !ok {
		t.Fatalf("in-root normalized path missing: %v", entries)
	}
	if _, ok := entries["/pwned.txt"]; ok {
		t.Fatal("out-of-root traversal survived")
	}
	if _, ok := entries["/.metadata.db"]; ok {
		t.Fatal("control-file traversal survived")
	}
}

func TestParseScanListMalformedLines(t *testing.T) {
	manifest := strings.Join([]string{
		"not a timestamp /电影/a.txt",
		"2024-01-02 03:04 relative/path.txt",
		"2024-01-02 03:04 /电影/bad\x00name.txt",
		"2024-01-02 03:04 /../..",
		"2024-01-02 03:04 /电影/" + strings.Repeat("x", maxManifestPathLen+1),
		"2024-01-02 03:04 /电影/ok.txt",
	}, "\n")
	entries, _, malformed, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 5 {
		t.Fatalf("malformed = %d, want 5", malformed)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want only the valid line", entries)
	}
}

func TestParseScanListAllowsPathBeyondPATHMAX(t *testing.T) {
	longPath := "/电影/" + strings.Repeat(strings.Repeat("d", 200)+"/", 24) + "movie.nfo"
	manifest := "2024-01-02 03:04 " + longPath + "\n"
	entries, _, malformed, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 0 || len(entries) != 1 {
		t.Fatalf("long path parse: malformed=%d entries=%d", malformed, len(entries))
	}
}

func TestParseScanListRejectsFileParentConflict(t *testing.T) {
	manifest := "2024-01-02 03:04 /电影/a\n2024-01-02 03:04 /电影/a/b.nfo\n"
	_, _, _, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err == nil || !strings.Contains(err.Error(), "conflicts with descendant") {
		t.Fatalf("err = %v, want file/descendant conflict", err)
	}
}

func TestParseScanListRejectsCaseInsensitivePathConflict(t *testing.T) {
	manifest := "2024-01-02 03:04 /电影/A\n2024-01-02 03:04 /电影/a/b.nfo\n"
	_, _, _, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err == nil || !strings.Contains(err.Error(), "conflicts with descendant") {
		t.Fatalf("err = %v, want case-insensitive path conflict", err)
	}
}

func TestParseScanListDecompressionLimit(t *testing.T) {
	old := maxScanListDecompressedBytes
	maxScanListDecompressedBytes = 64
	defer func() { maxScanListDecompressedBytes = old }()

	var lines []string
	for i := range 100 {
		lines = append(lines, "2024-01-02 03:04 /电影/file-"+strings.Repeat("x", 8)+strings.Repeat("0", 3)+".txt")
		_ = i
	}
	_, _, _, err := parseScanList(bytes.NewReader(gzipString(t, strings.Join(lines, "\n"))), map[string]bool{"电影": true})
	if err == nil || !strings.Contains(err.Error(), "decompressed size limit") {
		t.Fatalf("err = %v, want decompressed size limit error", err)
	}
}

func TestParseScanListEntryLimit(t *testing.T) {
	old := maxManifestEntries
	maxManifestEntries = 2
	defer func() { maxManifestEntries = old }()
	manifest := strings.Join([]string{
		"2024-01-02 03:04 /电影/a.txt",
		"2024-01-02 03:04 /电影/b.txt",
		"2024-01-02 03:04 /电影/c.txt",
	}, "\n")
	_, _, _, err := parseScanList(bytes.NewReader(gzipString(t, manifest)), map[string]bool{"电影": true})
	if err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("err = %v, want entry limit error", err)
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"https://emby.xiaoya.pro/":                        "https://emby.xiaoya.pro/",
		"https://user:pass@mirror.example.com/base?k=v#f": "https://mirror.example.com/base",
		"http://127.0.0.1:8965/":                          "http://127.0.0.1:8965/",
	}
	for in, want := range cases {
		if got := sanitizeURL(in); got != want {
			t.Errorf("sanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func newTestCrawlerRoot(t *testing.T) (string, *MetadataCrawler) {
	t.Helper()
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return base, &MetadataCrawler{
		client:      http.DefaultClient,
		downloadDir: base,
		fsRoot:      root,
		workers:     2,
	}
}

func TestDownloadRejectsSymlinkEscape(t *testing.T) {
	base, mc := newTestCrawlerRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "电影")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Last-Modified", time.Now().UTC().Format(time.RFC1123))
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	mc.client = srv.Client()
	_, err := mc.Download(context.Background(), "/电影/out.nfo", downloadOpts{mirrors: []string{srv.URL}, expectFile: true, repairParents: true})
	if err == nil {
		t.Fatal("download through escaping symlink unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "out.nfo")); !os.IsNotExist(err) {
		t.Fatalf("outside file was created: %v", err)
	}
}

func TestDownloadRejectsOversizedResponse(t *testing.T) {
	_, mc := newTestCrawlerRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(maxMetadataFileBytes+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	mc.client = srv.Client()
	_, err := mc.Download(context.Background(), "/电影/huge.nfo", downloadOpts{mirrors: []string{srv.URL}, expectFile: true})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("err = %v, want size limit error", err)
	}
}

func TestDownloadSupportsMaxLengthFilename(t *testing.T) {
	_, mc := newTestCrawlerRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	mc.client = srv.Client()

	name := strings.Repeat("a", 251) + ".nfo"
	remotePath := "/电影/" + name
	if _, err := mc.Download(context.Background(), remotePath, downloadOpts{mirrors: []string{srv.URL}, expectFile: true}); err != nil {
		t.Fatalf("download with 255-byte filename failed: %v", err)
	}
	if got, err := mc.fsRoot.ReadFile(rootRel(remotePath)); err != nil || string(got) != "payload" {
		t.Fatalf("downloaded content = %q, err = %v", got, err)
	}
}

func TestDownloadSupportsPathBeyondPATHMAX(t *testing.T) {
	_, mc := newTestCrawlerRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "7")
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	mc.client = srv.Client()

	remotePath := "/电影/" + strings.Repeat(strings.Repeat("d", 200)+"/", 24) + "movie.nfo"
	if len(remotePath) <= 4096 {
		t.Fatalf("test path is only %d bytes", len(remotePath))
	}
	if _, err := mc.Download(context.Background(), remotePath, downloadOpts{mirrors: []string{srv.URL}, expectFile: true}); err != nil {
		t.Fatalf("download with %d-byte path failed: %v", len(remotePath), err)
	}
	if got, err := mc.fsRoot.ReadFile(rootRel(remotePath)); err != nil || string(got) != "payload" {
		t.Fatalf("downloaded content = %q, err = %v", got, err)
	}
}

func TestProbeRejectsFutureManifestTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", time.Now().Add(time.Hour).UTC().Format(time.RFC1123))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if _, _, ok := probeScanList(context.Background(), srv.URL); ok {
		t.Fatal("future manifest timestamp was accepted")
	}
}

func TestReconcileTrashRestoresUncommittedDeletion(t *testing.T) {
	base, mc := newTestCrawlerRoot(t)
	db, err := openMetadataDB(base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := createFileTable(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO files ("+filesTableColumns+") VALUES (?,?,?,?,?,?,?,?)", "/电影/a.nfo", "a.nfo", 4, 1, "e", timeBaseHTTP, "e:4", provenanceETag); err != nil {
		t.Fatal(err)
	}
	if err := mc.fsRoot.MkdirAll(filepath.Join(trashDirName, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.fsRoot.WriteFile(filepath.Join(trashDirName, "电影", "a.nfo"), []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.reconcileTrash(context.Background(), db, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := mc.fsRoot.ReadFile(filepath.Join("电影", "a.nfo")); err != nil || string(got) != "data" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
}

func TestReconcileTrashDropsCommittedDeletion(t *testing.T) {
	base, mc := newTestCrawlerRoot(t)
	db, err := openMetadataDB(base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := createFileTable(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := mc.fsRoot.MkdirAll(filepath.Join(trashDirName, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.fsRoot.WriteFile(filepath.Join(trashDirName, "电影", "gone.nfo"), []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.reconcileTrash(context.Background(), db, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.fsRoot.Stat(trashDirName); !os.IsNotExist(err) {
		t.Fatalf("trash still exists: %v", err)
	}
}

func TestWriteMetadataFilesBatchesRows(t *testing.T) {
	base := t.TempDir()
	db, err := openMetadataDB(base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := createFileTable(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	files := make(chan *MetadataFile, 300)
	for i := range 300 {
		files <- &MetadataFile{path: fmt.Sprintf("/电影/%03d.nfo", i), name: "x.nfo", size: 1, modified: 1, etag: "e"}
	}
	close(files)
	if err := writeMetadataFiles(context.Background(), db, files); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM files").Scan(&count); err != nil || count != 300 {
		t.Fatalf("count = %d, err = %v", count, err)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || strings.ToLower(journal) != "delete" {
		t.Fatalf("journal_mode = %q, err = %v", journal, err)
	}
}

func TestRootDropsRoundTrip(t *testing.T) {
	d := parseRootDrops("")
	if d.gen != "" || len(d.roots) != 0 {
		t.Fatalf("empty encoding parsed to %+v", d)
	}
	enc := encodeRootDrops("abc123", map[string]bool{"电影": true, "电视剧": true})
	d = parseRootDrops(enc)
	if d.gen != "abc123" || !d.roots["电影"] || !d.roots["电视剧"] {
		t.Fatalf("round trip failed: %+v from %q", d, enc)
	}
	if encodeRootDrops("abc123", nil) != "" {
		t.Fatal("empty roots must encode to empty string")
	}
}

func TestTempPathFor(t *testing.T) {
	target := filepath.Join("电影", strings.Repeat("a", 251)+".nfo")
	tmp := tempPathFor(target)
	if filepath.Dir(tmp) != filepath.Dir(target) {
		t.Fatalf("temp file is not beside the target: %q", tmp)
	}
	if tempPathFor(target) == tmp {
		t.Fatal("temp paths must be unique")
	}
	if strings.Contains(filepath.Base(tmp), filepath.Base(target)) {
		t.Fatalf("temp name %q includes the target name", filepath.Base(tmp))
	}
	if len(filepath.Base(tmp)) >= len(filepath.Base(target)) {
		t.Fatalf("temp basename was not shortened: %q", filepath.Base(tmp))
	}
}

func TestRenameReplaceFileDoesNotRemoveDirectory(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("source.xtmp", []byte("new"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := root.MkdirAll("target", dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join("target", "media"), []byte("keep"), filePerm); err != nil {
		t.Fatal(err)
	}

	if err := renameReplaceFile(root, "source.xtmp", "target"); err == nil {
		t.Fatal("media-safe replacement unexpectedly removed a directory")
	}
	if got, err := root.ReadFile(filepath.Join("target", "media")); err != nil || string(got) != "keep" {
		t.Fatalf("media directory was changed: %q, %v", got, err)
	}
	if _, err := root.Stat("source.xtmp"); err != nil {
		t.Fatalf("source temp file was removed after rejected replacement: %v", err)
	}
}
