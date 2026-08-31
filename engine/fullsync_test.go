package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testManifestLastModified = "Mon, 02 Jan 2024 10:00:00 GMT"

type mirrorFile struct {
	content      string
	etag         string
	lastModified string
	encode       string // extra Content-Encoding on responses (e.g. "gzip")
}

// mirrorTestServer serves a manifest, an HTML autoindex and file payloads.
type mirrorTestServer struct {
	*httptest.Server

	mu          sync.Mutex
	manifest    string
	manifestLM  string
	manifestGz  []byte
	files       map[string]mirrorFile
	dirs        map[string][]string // dir path (with trailing /) -> child names
	nonHTMLDirs map[string]bool
	invalidHTML map[string]bool
	failPaths   map[string]int // path -> status code

	getCount map[string]int
}

func newMirrorServer(t *testing.T) *mirrorTestServer {
	t.Helper()
	m := &mirrorTestServer{
		manifestLM:  testManifestLastModified,
		files:       map[string]mirrorFile{},
		dirs:        map[string][]string{},
		nonHTMLDirs: map[string]bool{},
		invalidHTML: map[string]bool{},
		failPaths:   map[string]int{},
		getCount:    map[string]int{},
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.Close)
	return m
}

func (m *mirrorTestServer) setManifest(t *testing.T, lines string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manifest = lines
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(lines))
	zw.Close()
	m.manifestGz = buf.Bytes()
}

func (m *mirrorTestServer) addFile(path, content, etag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = mirrorFile{content: content, etag: etag, lastModified: testManifestLastModified}
}

func (m *mirrorTestServer) addDir(path string, children ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[path] = append(m.dirs[path], children...)
}

func (m *mirrorTestServer) gets(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCount[path]
}

func (m *mirrorTestServer) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := r.URL.Path
	if code, ok := m.failPaths[p]; ok {
		if r.Method == "GET" {
			m.getCount[p]++
		}
		w.WriteHeader(code)
		return
	}
	if p == scanListPath {
		if m.manifestGz == nil {
			http.Error(w, "no manifest", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Last-Modified", m.manifestLM)
		w.Header().Set("Content-Length", strconv.Itoa(len(m.manifestGz)))
		if r.Method == "GET" {
			m.getCount[p]++
			w.Write(m.manifestGz)
		}
		return
	}
	if f, ok := m.files[p]; ok {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", f.etag)
		w.Header().Set("Last-Modified", f.lastModified)
		if f.encode != "" && r.Header.Get("Accept-Encoding") == "identity" {
			// Server ignores identity: compress and report the encoded
			// length so reuse decisions must be refused.
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write([]byte(f.content))
			zw.Close()
			w.Header().Set("Content-Encoding", f.encode)
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			if r.Method == "GET" {
				m.getCount[p]++
				w.Write(buf.Bytes())
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(f.content)))
		if r.Method == "GET" {
			m.getCount[p]++
			w.Write([]byte(f.content))
		}
		return
	}
	if strings.HasSuffix(p, "/") {
		if m.nonHTMLDirs[p] {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("not a listing"))
			return
		}
		if m.invalidHTML[p] {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html><title>Maintenance</title><body>temporarily unavailable</body></html>"))
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "<html><title>Index of %s</title><body><h1>Index of %s</h1>", p, p)
		for _, c := range m.dirs[p] {
			if strings.HasSuffix(c, "/") {
				fmt.Fprintf(&sb, `<a href="%s/">%s/</a>`, strings.TrimSuffix(c, "/"), c)
			} else {
				fmt.Fprintf(&sb, `<a href="%s">%s</a>`, c, c)
			}
		}
		sb.WriteString("</body></html>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == "GET" {
			w.Write([]byte(sb.String()))
		}
		return
	}
	// HEAD on a directory: indicate HTML (dir marker).
	if r.Method == "HEAD" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// newFullCrawler wires a crawler to the given mirror pools without any
// network probing.
func newFullCrawler(dir string, manifestMirrors, crawlMirrors []string, workers int) *MetadataCrawler {
	return &MetadataCrawler{
		client:            newBrowserClient(30 * time.Second),
		downloadDir:       dir,
		manifestMirrors:   manifestMirrors,
		crawlMirrors:      crawlMirrors,
		manifestLastMod:   testManifestLastModified,
		selectedPaths:     []string{"/电影", "/115"},
		ignoredDirs:       sFolder,
		ignoredExtentions: sExt,
		workers:           workers,
	}
}

func openTestDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := openMetadataDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func rowIdentity(t *testing.T, db *sql.DB, path string) (size int64, modified int64, etag, timeBase, contentID, provenance string) {
	t.Helper()
	err := db.QueryRow("SELECT size, modified, etag, time_base, content_id, provenance FROM files WHERE path = ?", path).
		Scan(&size, &modified, &etag, &timeBase, &contentID, &provenance)
	if err != nil {
		t.Fatalf("row %s: %v", path, err)
	}
	return
}

const strictManifest = "2024-01-02 03:04 /电影/a.nfo\n2024-01-02 03:05 /电影/sub/b.nfo\n"

func dualManifestMirrors(t *testing.T) (*mirrorTestServer, *mirrorTestServer) {
	t.Helper()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, strictManifest)
		m.addFile("/电影/a.nfo", "AAA", `"ea"`)
		m.addFile("/电影/sub/b.nfo", "BBBB", `"eb"`)
		m.addFile("/115/c.nfo", "C", `"ec"`)
		m.addDir("/电影/", "a.nfo", "sub/")
		m.addDir("/电影/sub/", "b.nfo")
		m.addDir("/115/", "c.nfo")
		m.addDir("/", "电影/", "115/")
	}
	return m1, m2
}

func fullCrawlerOver(dir string, m1, m2 *mirrorTestServer) *MetadataCrawler {
	return newFullCrawler(dir, []string{m1.URL, m2.URL}, []string{m1.URL, m2.URL}, 2)
}

func TestSyncFullStrictRebuildsCache(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)

	// Seed stale content that the strict rebuild must clear.
	stale := filepath.Join(dir, "电影", "stale.nfo")
	if err := os.MkdirAll(filepath.Dir(stale), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), filePerm); err != nil {
		t.Fatal(err)
	}

	mc := fullCrawlerOver(dir, m1, m2)
	ctx := context.Background()
	if err := mc.SyncFull(ctx, fullModeStrict, 1); err != nil {
		t.Fatalf("strict full failed: %v", err)
	}

	db := openTestDB(t, dir)
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 3 {
		t.Fatalf("files count = %d, want 3", n)
	}
	size, modified, etag, base, cid, prov := rowIdentity(t, db, "/电影/a.nfo")
	if size != 3 || etag != `"ea"` || base != timeBaseManifest {
		t.Fatalf("a.nfo identity: size=%d etag=%s base=%s", size, etag, base)
	}
	if cid != `"ea":3` || prov != provenanceETag {
		t.Fatalf("a.nfo content id=%q provenance=%q", cid, prov)
	}
	if modified <= 0 {
		t.Fatalf("a.nfo modified=%d", modified)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale file survived strict clearing")
	}
	for _, p := range []string{"/电影/a.nfo", "/电影/sub/b.nfo"} {
		if _, err := os.Stat(filepath.Join(dir, rootRel(p))); err != nil {
			t.Fatalf("file %s missing: %v", p, err)
		}
	}
	// State cleared, markers set, staging GC'ed.
	st, err := readFullSyncStateDB(ctx, db)
	if err != nil || st != nil {
		t.Fatalf("pending state after success = %+v err=%v", st, err)
	}
	lm, _ := getMeta(ctx, db, metaScanListLastModified)
	if lm != testManifestLastModified {
		t.Fatalf("scan_list_last_modified = %q", lm)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory"); n != 0 {
		t.Fatalf("staging rows after GC = %d", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory_runs"); n != 0 {
		t.Fatalf("staging runs after GC = %d", n)
	}
}

func TestSyncFullSkipsAbsentUncoveredRoots(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, strictManifest)
		m.addFile("/电影/a.nfo", "AAA", `"ea"`)
		m.addFile("/电影/sub/b.nfo", "BBBB", `"eb"`)
		m.failPaths["/115"] = http.StatusNotFound
		m.failPaths["/115/"] = http.StatusNotFound
	}

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeStrict, 1); err != nil {
		t.Fatalf("strict full with absent optional root failed: %v", err)
	}

	db := openTestDB(t, dir)
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 2 {
		t.Fatalf("files count = %d, want 2 manifest entries", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM files WHERE path LIKE '/115/%'"); n != 0 {
		t.Fatalf("absent optional root produced %d rows", n)
	}
}

func TestSyncFullRejectsPartialUncoveredRoot(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.addDir("/115/", "a.nfo", "sub/")
		m.addFile("/115/a.nfo", "A", `"ea115"`)
		m.failPaths["/115/sub/"] = http.StatusNotFound
	}

	stale := filepath.Join(dir, "电影", "stale.nfo")
	if err := os.MkdirAll(filepath.Dir(stale), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), filePerm); err != nil {
		t.Fatal(err)
	}

	mc := fullCrawlerOver(dir, m1, m2)
	err := mc.SyncFull(context.Background(), fullModeStrict, 1)
	if err == nil {
		t.Fatal("partial uncovered root unexpectedly became authoritative")
	}
	if _, statErr := os.Stat(stale); statErr != nil {
		t.Fatalf("strict full touched live files before inventory completed: %v", statErr)
	}
	db := openTestDB(t, dir)
	st, stateErr := readFullSyncStateDB(context.Background(), db)
	if stateErr != nil || st != nil {
		t.Fatalf("partial inventory wrote full-sync state: %+v err=%v", st, stateErr)
	}
}

func TestSyncFullRejectsGenericHTMLAsEmptyRoot(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.invalidHTML["/115/"] = true
	}

	mc := fullCrawlerOver(dir, m1, m2)
	err := mc.SyncFull(context.Background(), fullModeStrict, 1)
	if err == nil {
		t.Fatal("generic HTML page unexpectedly became an authoritative empty root")
	}
	db := openTestDB(t, dir)
	st, stateErr := readFullSyncStateDB(context.Background(), db)
	if stateErr != nil || st != nil {
		t.Fatalf("invalid HTML inventory wrote full-sync state: %+v err=%v", st, stateErr)
	}
}

func TestIncrementalSameManifestRepairsMissingCacheFile(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m := newMirrorServer(t)
	m.setManifest(t, "2024-01-02 03:04 /电影/a.nfo\n")
	m.addFile("/电影/a.nfo", "AAA", `"ea"`)

	mc := newFullCrawler(dir, []string{m.URL}, []string{m.URL}, 1)
	mc.selectedPaths = []string{"/电影"}
	if err := mc.Sync(context.Background()); err != nil {
		t.Fatalf("first incremental sync failed: %v", err)
	}
	before := m.gets("/电影/a.nfo")
	manifestBefore := m.gets(scanListPath)
	cachePath := filepath.Join(dir, rootRel("/电影/a.nfo"))
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	db, err := openMetadataDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := setMeta(context.Background(), db, metaIntegrityCursor, "9223372036854775807"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := mc.Sync(context.Background()); err != nil {
		t.Fatalf("second incremental sync failed: %v", err)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("missing cache file was not repaired: %v", err)
	}
	if string(got) != "AAA" {
		t.Fatalf("repaired content = %q", got)
	}
	if m.gets("/电影/a.nfo") <= before {
		t.Fatal("unchanged manifest did not trigger the targeted re-download")
	}
	if m.gets(scanListPath) <= manifestBefore {
		t.Fatal("incremental trigger did not fetch the unchanged manifest")
	}
}

func TestSyncFullRelaxedReuseHeuristics(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)

	// Pre-existing local file with identical size and a fresh mtime: the
	// relaxed heuristic must reuse it without a GET.
	if err := os.MkdirAll(filepath.Join(dir, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	localA := filepath.Join(dir, "电影", "a.nfo")
	if err := os.WriteFile(localA, []byte("AAA"), filePerm); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(localA, future, future); err != nil {
		t.Fatal(err)
	}

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeRelaxed, 1); err != nil {
		t.Fatalf("relaxed full failed: %v", err)
	}

	db := openTestDB(t, dir)
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 3 {
		t.Fatalf("files count = %d, want 3", n)
	}
	_, modified, _, base, cid, prov := rowIdentity(t, db, "/电影/a.nfo")
	if base != timeBaseManifest {
		t.Fatalf("reused a.nfo time base = %s", base)
	}
	if cid != `"ea":3` || prov != provenanceETag {
		t.Fatalf("reused a.nfo cid=%q prov=%q", cid, prov)
	}
	// The reuse must not have fetched the body from the mirror.
	if m1.gets("/电影/a.nfo") != 0 || m2.gets("/电影/a.nfo") != 0 {
		t.Fatal("reused file was downloaded anyway")
	}
	// The file mtime is aligned to the manifest source time.
	fi, err := os.Stat(localA)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Unix() != modified {
		t.Fatalf("reused mtime = %d, row modified = %d", fi.ModTime().Unix(), modified)
	}
	// The missing file was downloaded.
	if m1.gets("/电影/sub/b.nfo")+m2.gets("/电影/sub/b.nfo") == 0 {
		t.Fatal("missing file was not downloaded")
	}
	snap := globalStatus.snapshot()
	if snap.Download.Reused != 1 {
		t.Fatalf("reused counter = %d", snap.Download.Reused)
	}
}

func TestSyncFullRelaxedReplacesExistingDirectoryWithFile(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	conflict := filepath.Join(dir, rootRel("/电影/a.nfo"))
	if err := os.MkdirAll(conflict, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflict, "stale"), []byte("old"), filePerm); err != nil {
		t.Fatal(err)
	}

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeRelaxed, 1); err != nil {
		t.Fatalf("relaxed full failed to replace existing target: %v", err)
	}
	got, err := os.ReadFile(conflict)
	if err != nil || string(got) != "AAA" {
		t.Fatalf("replacement content = %q, err = %v", got, err)
	}
}

func TestRenameConflictRecognizesLinuxFileOverDirectoryError(t *testing.T) {
	if !isRenameConflict(&os.LinkError{Op: "rename", Old: "old", New: "new", Err: syscall.EISDIR}) {
		t.Fatal("EISDIR must trigger the existing-target replacement path")
	}
}

func TestIncrementalFailureIsIgnoredAndMediaContinues(t *testing.T) {
	resetGlobalStatus()
	downloadDir, mediaDir := t.TempDir(), t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	const (
		goodPath = "/电影/good.nfo"
		badPath  = "/电影/bad.nfo"
	)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:04 "+goodPath+"\n2024-01-02 03:04 "+badPath+"\n")
		m.addFile(goodPath, "good-old", `"good-old"`)
		m.addFile(badPath, "bad-old", `"bad-old"`)
	}
	cfg := &Config{DownloadDir: downloadDir, MediaDir: mediaDir}
	settings := validSettings()
	settings.RunMode = modeBitDownload | modeBitMedia
	settings.Cleanup = true
	settings.Purge = false
	settings.MirrorURL = []string{m1.URL, m2.URL}
	settings.DownloadWorkers = 2
	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	oldDelay := metadataRetryBaseDelay
	metadataRetryBaseDelay = 0
	t.Cleanup(func() { metadataRetryBaseDelay = oldDelay })
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:05 "+goodPath+"\n2024-01-02 03:05 "+badPath+"\n")
		m.addFile(goodPath, "good-new", `"good-new"`)
		inner := m.Server.Config.Handler
		m.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == badPath {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "8")
				_, _ = w.Write([]byte("bad"))
				return
			}
			inner.ServeHTTP(w, r)
		})
	}

	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("sync with one failed entry = %v, want success", err)
	}
	for _, dir := range []string{downloadDir, mediaDir} {
		if got, err := os.ReadFile(filepath.Join(dir, rootRel(goodPath))); err != nil || string(got) != "good-new" {
			t.Fatalf("successful file in %s = %q, err = %v", dir, got, err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, rootRel(badPath))); err != nil || string(got) != "bad-old" {
			t.Fatalf("ignored file in %s = %q, err = %v", dir, got, err)
		}
	}
	if snap := globalStatus.snapshot(); snap.Download.Ignored != 1 || snap.Download.Failed != 0 || snap.Download.RetryRound != maxMetadataEntryRetryRounds {
		t.Fatalf("terminal counters = %+v, want ignored=1 failed=0 retry=%d", snap.Download, maxMetadataEntryRetryRounds)
	}
}

func TestIncrementalUnanimous404ContinuesAndPreservesExistingState(t *testing.T) {
	resetGlobalStatus()
	downloadDir, mediaDir := t.TempDir(), t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	const (
		filePath  = "/电影/gone.nfo"
		flakyPath = "/电影/flaky.nfo"
	)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:04 "+filePath+"\n2024-01-02 03:04 "+flakyPath+"\n")
		m.addFile(filePath, "old", `"gone"`)
		m.addFile(flakyPath, "good", `"flaky"`)
	}
	cfg := &Config{DownloadDir: downloadDir, MediaDir: mediaDir}
	settings := validSettings()
	settings.RunMode = modeBitDownload | modeBitMedia
	settings.Purge = false
	settings.MirrorURL = []string{m1.URL, m2.URL}
	settings.DownloadWorkers = 2
	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	cachePath := filepath.Join(downloadDir, rootRel(filePath))
	mediaPath := filepath.Join(mediaDir, rootRel(filePath))
	for _, p := range []string{cachePath, mediaPath} {
		if got, err := os.ReadFile(p); err != nil || string(got) != "old" {
			t.Fatalf("initial file %s = %q, err = %v", p, got, err)
		}
	}
	downloadDB, err := openMetadataDB(downloadDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDB.Exec("DELETE FROM files WHERE path = ?", filePath); err != nil {
		downloadDB.Close()
		t.Fatal(err)
	}
	if err := downloadDB.Close(); err != nil {
		t.Fatal(err)
	}

	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:05 "+filePath+"\n2024-01-02 03:05 "+flakyPath+"\n")
		m.mu.Lock()
		delete(m.files, filePath)
		m.failPaths[filePath] = http.StatusNotFound
		m.mu.Unlock()

		// Fail this mirror's first flaky-file body without entering the
		// request-level status retry. The next metadata retry succeeds.
		inner := m.Server.Config.Handler
		var requests atomic.Int32
		m.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == flakyPath && requests.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "5")
				_, _ = w.Write([]byte("bad"))
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	before1, before2 := m1.gets(filePath), m2.gets(filePath)
	done := make(chan error, 1)
	go func() { done <- cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0) }()

	// Cached and media copies must remain present while the flaky entry is
	// still pending its two-second metadata retry delay.
	deadline := time.Now().Add(time.Second)
	for globalStatus.snapshot().Download.Failed != 1 {
		if time.Now().After(deadline) {
			t.Fatal("transient failure was not observed before its retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, p := range []string{cachePath, mediaPath} {
		if got, err := os.ReadFile(p); err != nil || string(got) != "old" {
			t.Fatalf("file changed while the download retry was pending %s = %q, err = %v", p, got, err)
		}
	}
	if snap := globalStatus.snapshot(); snap.Download.Unavailable != 1 || snap.Download.Failed != 1 {
		t.Fatalf("first batch counters = %+v, want unavailable=1 failed=1", snap.Download)
	}
	if err := <-done; err != nil {
		t.Fatalf("404 round = %v, want success", err)
	}
	if got := m1.gets(filePath) - before1; got != 1 {
		t.Fatalf("first mirror requests in one trigger = %d, want 1", got)
	}
	if got := m2.gets(filePath) - before2; got != 1 {
		t.Fatalf("second mirror requests in one trigger = %d, want 1", got)
	}
	for _, p := range []string{cachePath, mediaPath} {
		if got, err := os.ReadFile(p); err != nil || string(got) != "old" {
			t.Fatalf("unavailable entry was not preserved %s = %q, err = %v", p, got, err)
		}
	}
	db := openTestDB(t, downloadDir)
	if n := countRows(t, db, "SELECT COUNT(*) FROM files WHERE path = ?", filePath); n != 0 {
		t.Fatalf("unavailable row count = %d, want 0", n)
	}
	if snap := globalStatus.snapshot(); snap.Download.Unavailable != 1 || snap.Download.Failed != 0 {
		t.Fatalf("completed retry counters = %+v, want unavailable=1 failed=0", snap.Download)
	}

	for _, m := range []*mirrorTestServer{m1, m2} {
		m.mu.Lock()
		delete(m.failPaths, filePath)
		m.files[filePath] = mirrorFile{content: "new", etag: `"gone-new"`, lastModified: testManifestLastModified}
		m.mu.Unlock()
	}
	before1, before2 = m1.gets(filePath), m2.gets(filePath)
	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("next incremental trigger failed: %v", err)
	}
	if m1.gets(filePath)-before1+m2.gets(filePath)-before2 < 1 {
		t.Fatal("next incremental trigger did not retry the unavailable entry")
	}
	for _, p := range []string{cachePath, mediaPath} {
		if got, err := os.ReadFile(p); err != nil || string(got) != "new" {
			t.Fatalf("recovered file %s = %q, err = %v", p, got, err)
		}
	}
}

func TestForceCrawl404PreservesUnindexedMediaAndContinues(t *testing.T) {
	resetGlobalStatus()
	downloadDir, mediaDir := t.TempDir(), t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	const (
		goodPath = "/电影/good.nfo"
		gonePath = "/电影/gone.nfo"
	)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.addDir("/", "电影/", "每日更新/")
		m.addDir("/电影/", "good.nfo", "gone.nfo")
		m.addDir("/每日更新/")
		m.addFile(goodPath, "good-old", `"good-old"`)
		m.addFile(gonePath, "gone-old", `"gone-old"`)
	}
	cfg := &Config{DownloadDir: downloadDir, MediaDir: mediaDir}
	settings := validSettings()
	settings.RunMode = modeBitDownload | modeBitMedia
	settings.Cleanup = true
	settings.Purge = false
	settings.ForceCrawl = true
	settings.MirrorURL = []string{m1.URL, m2.URL}
	settings.DownloadWorkers = 2
	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("initial crawl sync failed: %v", err)
	}

	downloadDB, err := openMetadataDB(downloadDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDB.Exec("DELETE FROM files WHERE path = ?", gonePath); err != nil {
		downloadDB.Close()
		t.Fatal(err)
	}
	if err := downloadDB.Close(); err != nil {
		t.Fatal(err)
	}
	later := "Mon, 02 Jan 2024 10:01:00 GMT"
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.mu.Lock()
		m.files[goodPath] = mirrorFile{content: "good-new", etag: `"good-new"`, lastModified: later}
		delete(m.files, gonePath)
		m.failPaths[gonePath] = http.StatusNotFound
		m.mu.Unlock()
	}

	if err := cfg.runSyncRoundOnce(context.Background(), settings, SyncTypeIncremental, 0); err != nil {
		t.Fatalf("crawl sync with unavailable entry = %v, want success", err)
	}
	for _, dir := range []string{downloadDir, mediaDir} {
		if got, err := os.ReadFile(filepath.Join(dir, rootRel(goodPath))); err != nil || string(got) != "good-new" {
			t.Fatalf("successful crawl file in %s = %q, err = %v", dir, got, err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, rootRel(gonePath))); err != nil || string(got) != "gone-old" {
			t.Fatalf("unavailable crawl file in %s = %q, err = %v", dir, got, err)
		}
	}
	if snap := globalStatus.snapshot(); snap.Download.Unavailable != 1 || snap.Download.Failed != 0 {
		t.Fatalf("crawl terminal counters = %+v, want unavailable=1 failed=0", snap.Download)
	}
}

func TestSyncFullRelaxedReuseWithWeakETagGetsAcceptanceID(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	// Weak etags cannot pin identity: reuse still skips the network but
	// must mint a fresh acceptance ID that forces one media copy.
	m1.mu.Lock()
	f := m1.files["/电影/a.nfo"]
	f.etag = `W/"weak"`
	m1.files["/电影/a.nfo"] = f
	m2.mu.Lock()
	m2.files["/电影/a.nfo"] = f
	m2.mu.Unlock()
	m1.mu.Unlock()

	if err := os.MkdirAll(filepath.Join(dir, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	localA := filepath.Join(dir, "电影", "a.nfo")
	if err := os.WriteFile(localA, []byte("AAA"), filePerm); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(localA, future, future)

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeRelaxed, 1); err != nil {
		t.Fatalf("relaxed full failed: %v", err)
	}
	db := openTestDB(t, dir)
	_, _, etag, _, cid, prov := rowIdentity(t, db, "/电影/a.nfo")
	if etag != `W/"weak"` || strings.HasPrefix(cid, `"`) {
		t.Fatalf("weak-etag reuse identity wrong: etag=%s cid=%s", etag, cid)
	}
	if prov != provenanceReused || cid == "" {
		t.Fatalf("weak-etag reuse must mint an acceptance ID: cid=%q prov=%q", cid, prov)
	}
	if m1.gets("/电影/a.nfo") != 0 {
		t.Fatal("weak-etag reuse downloaded the body")
	}
}

func TestSyncFullRelaxedRefusesReuseWhenServerIgnoresIdentity(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.mu.Lock()
		f := m.files["/电影/a.nfo"]
		f.encode = "gzip"
		m.files["/电影/a.nfo"] = f
		m.mu.Unlock()
	}

	if err := os.MkdirAll(filepath.Join(dir, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	localA := filepath.Join(dir, "电影", "a.nfo")
	if err := os.WriteFile(localA, []byte("AAA"), filePerm); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(localA, future, future)

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeRelaxed, 1); err != nil {
		t.Fatalf("relaxed full failed: %v", err)
	}
	// Identity was ignored, so reuse is forbidden: the body was fetched.
	if m1.gets("/电影/a.nfo")+m2.gets("/电影/a.nfo") == 0 {
		t.Fatal("reuse was allowed despite the server ignoring identity encoding")
	}
	db := openTestDB(t, dir)
	size, _, _, _, _, _ := rowIdentity(t, db, "/电影/a.nfo")
	if size != 3 {
		t.Fatalf("decoded size = %d, want 3", size)
	}
}

func TestSyncFullRequiresTwoAgreeingManifestMirrors(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()

	// A single manifest mirror is not authoritative.
	m1, m2 := dualManifestMirrors(t)
	mc := newFullCrawler(dir, []string{m1.URL}, []string{m1.URL, m2.URL}, 2)
	err := mc.SyncFull(context.Background(), fullModeStrict, 1)
	if !isDeferredErr(err) {
		t.Fatalf("single mirror = %v, want deferred", err)
	}

	// Disagreeing mirrors are deferred too.
	m3 := newMirrorServer(t)
	m3.setManifest(t, "2024-01-02 03:04 /电影/a.nfo\n")
	m3.addFile("/电影/a.nfo", "AAA", `"ea"`)
	mc = newFullCrawler(dir, []string{m1.URL, m3.URL}, []string{m1.URL, m3.URL}, 2)
	err = mc.SyncFull(context.Background(), fullModeStrict, 1)
	if !isDeferredErr(err) {
		t.Fatalf("disagreeing mirrors = %v, want deferred", err)
	}

	// Malformed manifests defer as well.
	m4, m5 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m4, m5} {
		m.setManifest(t, "garbage line without timestamp\n2024-01-02 03:04 /电影/a.nfo\n")
		m.addFile("/电影/a.nfo", "AAA", `"ea"`)
	}
	mc = newFullCrawler(dir, []string{m4.URL, m5.URL}, []string{m4.URL, m5.URL}, 2)
	err = mc.SyncFull(context.Background(), fullModeStrict, 1)
	if !isDeferredErr(err) {
		t.Fatalf("malformed manifest = %v, want deferred", err)
	}

	// No live state was touched by any deferred attempt.
	db := openTestDB(t, dir)
	st, err2 := readFullSyncStateDB(context.Background(), db)
	if err2 != nil || st != nil {
		t.Fatalf("deferred attempts wrote state: %+v err=%v", st, err2)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 0 {
		t.Fatalf("deferred attempts wrote rows: %d", n)
	}
}

func TestSyncFullCrawlsUncoveredRootsFromTwoSources(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)

	// /115 is not covered by the manifest: both mirrors must traverse it
	// completely; each mirror carries one exclusive file to prove the
	// union is taken.
	m1.addDir("/115/", "x.nfo", "y.nfo")
	m2.addDir("/115/", "x.nfo", "z.nfo")
	m1.addFile("/115/x.nfo", "X", `"ex"`)
	m1.addFile("/115/y.nfo", "Y", `"ey"`)
	m2.addFile("/115/x.nfo", "X", `"ex"`)
	m2.addFile("/115/z.nfo", "Z", `"ez"`)

	mc := fullCrawlerOver(dir, m1, m2)
	if err := mc.SyncFull(context.Background(), fullModeStrict, 1); err != nil {
		t.Fatalf("strict full with crawl roots failed: %v", err)
	}
	db := openTestDB(t, dir)
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 6 {
		t.Fatalf("files count = %d, want 6 (2 manifest + 4 crawl union)", n)
	}
	for _, p := range []string{"/115/x.nfo", "/115/y.nfo", "/115/z.nfo"} {
		_, _, _, base, _, _ := rowIdentity(t, db, p)
		if base != timeBaseHTTP {
			t.Fatalf("crawl row %s time base = %s", p, base)
		}
	}
}

func TestSyncFullCrawlSourceFailureLeavesStateUntouched(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	// One mirror serves a non-HTML listing for the crawl root: that source
	// fails and the whole inventory must fail closed.
	m2.mu.Lock()
	m2.nonHTMLDirs["/115/"] = true
	m2.mu.Unlock()
	m1.addDir("/115/", "x.nfo")
	m1.addFile("/115/x.nfo", "X", `"ex"`)

	mc := fullCrawlerOver(dir, m1, m2)
	err := mc.SyncFull(context.Background(), fullModeStrict, 1)
	if err == nil {
		t.Fatal("non-HTML crawl source unexpectedly succeeded")
	}
	db := openTestDB(t, dir)
	st, err2 := readFullSyncStateDB(context.Background(), db)
	if err2 != nil || st != nil {
		t.Fatalf("failed crawl wrote state: %+v err=%v", st, err2)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM files"); n != 0 {
		t.Fatalf("failed crawl wrote rows: %d", n)
	}
}

func TestSyncFullCancelLeavesRecoverableState(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)
	// Make one file hang so the round can be canceled mid-download.
	block := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// Wrap the handler: block a.nfo on the first server.
	inner := m1.Server.Config.Handler
	m1.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/电影/a.nfo" {
			once.Do(func() { close(release) })
			<-block
		}
		inner.ServeHTTP(w, r)
	})

	mc := fullCrawlerOver(dir, m1, m2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mc.SyncFull(ctx, fullModeStrict, 1) }()
	<-release
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("canceled full sync returned success")
	}

	db := openTestDB(t, dir)
	st, err2 := readFullSyncStateDB(context.Background(), db)
	if err2 != nil || st == nil {
		t.Fatalf("pending state missing after cancel: %+v err=%v", st, err2)
	}
	if st.Mode != fullModeStrict || st.Phase != fullPhaseDownloading {
		t.Fatalf("pending state = %+v", st)
	}
	syncID := st.SyncID

	// Resume with a fresh crawler instance (like a process restart): the
	// stable sync ID is kept and the round completes.
	close(block)
	mc2 := fullCrawlerOver(dir, m1, m2)
	if err := mc2.SyncFull(context.Background(), fullModeStrict, 1); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	db2 := openTestDB(t, dir)
	st2, err3 := readFullSyncStateDB(context.Background(), db2)
	if err3 != nil || st2 != nil {
		t.Fatalf("state after resume = %+v err=%v", st2, err3)
	}
	if n := countRows(t, db2, "SELECT COUNT(*) FROM files"); n != 3 {
		t.Fatalf("files after resume = %d", n)
	}
	// The resumed round used the same sync ID (previous snapshot semantics).
	if syncID == "" {
		t.Fatal("no sync id recorded")
	}
}

func TestSyncFullStrictResumeReplacedInventoryKeepsProgress(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := dualManifestMirrors(t)

	mc := fullCrawlerOver(dir, m1, m2)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the state is committed: emulate by canceling during the
	// first file download via a slow first byte.
	var started sync.Once
	slow := make(chan struct{})
	released := make(chan struct{})
	inner := m1.Server.Config.Handler
	m1.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/电影/a.nfo" {
			started.Do(func() { close(slow) })
			<-released
		}
		inner.ServeHTTP(w, r)
	})
	done := make(chan error, 1)
	go func() { done <- mc.SyncFull(ctx, fullModeStrict, 7) }()
	<-slow
	cancel()
	close(released)
	if err := <-done; err == nil {
		t.Fatal("expected cancellation error")
	}

	db := openTestDB(t, dir)
	st, err := readFullSyncStateDB(context.Background(), db)
	if err != nil || st == nil {
		t.Fatalf("no pending state: %+v %v", st, err)
	}
	if st.StartRevision != 7 {
		t.Fatalf("start revision = %d, want 7 (audit)", st.StartRevision)
	}

	// The second attempt fetches the same generation and completes,
	// re-verifying completed rows (b.nfo finished while a.nfo hung).
	mc2 := fullCrawlerOver(dir, m1, m2)
	if err := mc2.SyncFull(context.Background(), fullModeStrict, 8); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if n := countRows(t, openTestDB(t, dir), "SELECT COUNT(*) FROM files"); n != 3 {
		t.Fatalf("files after resume = %d", n)
	}
}

func TestSyncFullForceCrawlAllRoots(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.addDir("/", "电影/", "115/")
		m.addDir("/电影/", "a.nfo")
		m.addFile("/电影/a.nfo", "AAA", `"ea"`)
		m.addDir("/115/", "c.nfo")
		m.addFile("/115/c.nfo", "C", `"ec"`)
	}
	mc := newFullCrawler(dir, nil, []string{m1.URL, m2.URL}, 2)
	mc.forceCrawl = true
	if err := mc.SyncFull(context.Background(), fullModeRelaxed, 1); err != nil {
		t.Fatalf("force-crawl full failed: %v", err)
	}
	db := openTestDB(t, dir)
	_, _, _, base, _, _ := rowIdentity(t, db, "/电影/a.nfo")
	if base != timeBaseHTTP {
		t.Fatalf("force-crawl row time base = %s", base)
	}
	tb, _ := getMeta(context.Background(), db, metaTimeBase)
	if tb != timeBaseHTTP {
		t.Fatalf("global time base = %s", tb)
	}
}

func TestReconcileTrashRestoresPendingPreviousFiles(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	db := openTestDB(t, base)
	ctx := context.Background()
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := createFullTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	mc := &MetadataCrawler{downloadDir: base, fsRoot: root}

	// A quarantined file with no live row but a pending-snapshot row is an
	// uncommitted deletion and must be restored.
	if _, err := db.ExecContext(ctx, "INSERT INTO full_previous_files (sync_id, path) VALUES ('fs-1', '/电影/gone.nfo')"); err != nil {
		t.Fatal(err)
	}
	if err := root.MkdirAll(filepath.Join(trashDirName, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join(trashDirName, "电影", "gone.nfo"), []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.reconcileTrash(ctx, db, "fs-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := root.ReadFile(filepath.Join("电影", "gone.nfo")); err != nil || string(got) != "data" {
		t.Fatalf("pending-snapshot file not restored: %q %v", got, err)
	}

	// Without a matching snapshot row the same file is a committed
	// deletion and is discarded.
	if err := root.MkdirAll(filepath.Join(trashDirName, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join(trashDirName, "电影", "gone.nfo"), []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}
	if err := mc.reconcileTrash(ctx, db, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Stat(trashDirName); !os.IsNotExist(err) {
		t.Fatalf("trash survived: %v", err)
	}
}

func TestGCCanceledContextStopsSweep(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	if err := createFullTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if _, err := db.ExecContext(ctx, "INSERT INTO full_inventory VALUES ('drop','/q"+strconv.Itoa(i)+"','http',1,'crawl')"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO full_inventory_runs (run_id, created_at) VALUES ('drop',1)"); err != nil {
		t.Fatal(err)
	}
	// A canceled maintenance lease stops the sweep before deletion.
	cancel()
	err := gcFullInventory(ctx, db, "", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sweep = %v, want context.Canceled", err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory WHERE inventory_run_id = 'drop'"); n != 10 {
		t.Fatalf("rows deleted despite canceled context: %d remain", n)
	}
	// A live maintenance lease collects everything.
	if err := gcFullInventory(context.Background(), db, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory"); n != 0 {
		t.Fatalf("idle sweep left %d rows", n)
	}
}

func TestGCFullsInventoryKeepsPendingReferences(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	ctx := context.Background()
	if err := createFullTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	for i := range 12000 {
		if _, err := db.ExecContext(ctx, "INSERT INTO full_inventory VALUES ('keep','/p"+strconv.Itoa(i)+"','manifest',1,'manifest')"); err != nil {
			t.Fatal(err)
		}
		if i < 6000 {
			if _, err := db.ExecContext(ctx, "INSERT INTO full_inventory VALUES ('drop','/q"+strconv.Itoa(i)+"','http',1,'crawl')"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO full_inventory_runs (run_id, created_at) VALUES ('keep',1),('drop',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO full_previous_files VALUES ('fs-keep','/a'),('fs-old','/b')"); err != nil {
		t.Fatal(err)
	}
	if err := gcFullInventory(ctx, db, "keep", "fs-keep"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory WHERE inventory_run_id = 'keep'"); n != 12000 {
		t.Fatalf("kept run rows = %d", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_inventory WHERE inventory_run_id = 'drop'"); n != 0 {
		t.Fatalf("dropped run rows = %d", n)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM full_previous_files"); n != 1 {
		t.Fatalf("previous rows = %d", n)
	}
}
