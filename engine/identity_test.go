package engine

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeadIdentityFailoverOn404(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	// First mirror 404s the path, second serves it: the file exists and
	// must be reported, because mirrors lag each other.
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	m1.failPaths["/电影/a.nfo"] = http.StatusNotFound
	m2.addFile("/电影/a.nfo", "AAA", `"ea"`)
	mc := newFullCrawler(dir, []string{m1.URL, m2.URL}, []string{m1.URL, m2.URL}, 1)
	meta, err := mc.headIdentity(context.Background(), []string{m1.URL + "/", m2.URL + "/"}, "/电影/a.nfo")
	if err != nil {
		t.Fatalf("headIdentity with one lagging mirror = %v", err)
	}
	if meta == nil || meta.etag != `"ea"` {
		t.Fatalf("meta = %+v", meta)
	}

	// Unanimous 404 is a confirmed not-exist.
	_, err = mc.headIdentity(context.Background(), []string{m1.URL + "/"}, "/电影/a.nfo")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unanimous 404 = %v, want ErrNotExist", err)
	}
}

func TestHeadIdentityMixed404AndErrorStaysHardError(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	// One mirror 404s, the other is unreachable: absence is NOT confirmed.
	m1 := newMirrorServer(t)
	m1.failPaths["/电影/a.nfo"] = http.StatusNotFound
	mc := newFullCrawler(dir, []string{m1.URL}, []string{m1.URL}, 1)
	_, err := mc.headIdentity(context.Background(), []string{m1.URL + "/", "http://127.0.0.1:1/"}, "/电影/a.nfo")
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatal("mixed 404+transport error must not confirm absence")
	}
	if err == nil {
		t.Fatal("mixed 404+transport error must be an error")
	}
}

func TestCheckMediaOverlap(t *testing.T) {
	roots := []string{"/电影", "/115"}
	base := t.TempDir()
	dl := filepath.Join(base, "cache")
	md := filepath.Join(base, "media")
	for _, dir := range []string{dl, md} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			t.Fatal(err)
		}
	}

	if err := checkMediaOverlap(md, dl, roots); err != nil {
		t.Fatalf("separate dirs rejected: %v", err)
	}
	if err := checkMediaOverlap(dl, dl, roots); err == nil {
		t.Fatal("media dir == download dir accepted")
	}
	nested := filepath.Join(dl, "电影", "media")
	if err := os.MkdirAll(nested, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaOverlap(nested, dl, roots); err == nil {
		t.Fatal("media dir nested under a sync root accepted")
	}
	safe := filepath.Join(dl, ".media")
	if err := os.MkdirAll(safe, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaOverlap(safe, dl, roots); err != nil {
		t.Fatalf("media dir under a non-root subdir rejected: %v", err)
	}

	// The download cache itself may not sit below the media library:
	// clearing one of its roots would still delete media-tree content.
	reverseMedia := filepath.Join(base, "reverse-media")
	reverseDL := filepath.Join(reverseMedia, "cache")
	if err := os.MkdirAll(reverseDL, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaOverlap(reverseMedia, reverseDL, roots); err == nil {
		t.Fatal("download dir nested under media dir accepted")
	}

	// Symlink aliases are resolved before comparing directory trees.
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "cache-link")
	if err := os.Symlink(reverseDL, alias); err != nil {
		t.Fatal(err)
	}
	if err := checkMediaOverlap(reverseMedia, alias, roots); err == nil {
		t.Fatal("symlinked download dir nested under media dir accepted")
	}
}

func TestStrongETagAndContentID(t *testing.T) {
	if isStrongETag("") || isStrongETag(`W/"a"`) || isStrongETag(`w/"a"`) {
		t.Fatal("weak/empty etags must not be strong")
	}
	if !isStrongETag(`"abc"`) {
		t.Fatal("quoted etag must be strong")
	}
	if contentIDFor(`W/"a"`, 5) != "" || contentIDFor("", 5) != "" {
		t.Fatal("weak/empty etags must not produce content ids")
	}
	if got := contentIDFor(` "abc" `, 7); got != `"abc":7` {
		t.Fatalf("content id = %q", got)
	}
	if strongETagEqual("", `"a"`) || strongETagEqual(`W/"a"`, `W/"a"`) {
		t.Fatal("weak or empty etags must never establish equality")
	}
	if !strongETagEqual(`"a"`, ` "a" `) {
		t.Fatal("equal strong etags with padding must match")
	}
	if id := newMaterializationID("dl"); id == "" || newMaterializationID("dl") == id {
		t.Fatal("materialization ids must be non-empty and unique")
	}
}

func TestCreateFileTableMigratesLegacySchema(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir)
	ctx := context.Background()
	// Create the legacy 5-column schema with two rows: one strong-etag row
	// and one weak-etag row.
	if _, err := db.Exec(`CREATE TABLE files (
		path TEXT PRIMARY KEY, name TEXT, size INTEGER, modified INTEGER, etag TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO files VALUES (?,?,?,?,?)", "/电影/a.nfo", "a.nfo", 10, 100, `"ea"`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO files VALUES (?,?,?,?,?)", "/电影/b.nfo", "b.nfo", 20, 200, `W/"eb"`); err != nil {
		t.Fatal(err)
	}
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	var base, cid string
	if err := db.QueryRow("SELECT time_base, content_id FROM files WHERE path = '/电影/a.nfo'").Scan(&base, &cid); err != nil {
		t.Fatal(err)
	}
	if base != timeBaseUnknown {
		t.Fatalf("legacy row time base = %s, want unknown", base)
	}
	if cid != `"ea":10` {
		t.Fatalf("strong-etag legacy row content id = %q", cid)
	}
	if err := db.QueryRow("SELECT time_base, content_id FROM files WHERE path = '/电影/b.nfo'").Scan(&base, &cid); err != nil {
		t.Fatal(err)
	}
	if base != timeBaseUnknown || cid != "" {
		t.Fatalf("weak-etag legacy row = %s/%q, want unknown/empty", base, cid)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != filesSchemaVersion {
		t.Fatalf("schema version = %d/%v, want %d", version, err, filesSchemaVersion)
	}
}

func TestCreateFileTableResumesInterruptedBackfill(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	ctx := context.Background()
	// Simulate a crash after ALTER TABLE committed but before strong-ETag
	// identity backfill and the user_version completion marker.
	if _, err := db.Exec(`CREATE TABLE files (
		path TEXT PRIMARY KEY, name TEXT, size INTEGER, modified INTEGER, etag TEXT,
		time_base TEXT NOT NULL DEFAULT 'unknown',
		content_id TEXT NOT NULL DEFAULT '',
		provenance TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO files VALUES (?,?,?,?,?,?,?,?)", "/电影/a.nfo", "a.nfo", 10, 100, `"ea"`, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	var base, cid string
	if err := db.QueryRow("SELECT time_base, content_id FROM files WHERE path = '/电影/a.nfo'").Scan(&base, &cid); err != nil {
		t.Fatal(err)
	}
	if base != timeBaseUnknown || cid != `"ea":10` {
		t.Fatalf("resumed row = %s/%q, want unknown/%q", base, cid, `"ea":10`)
	}
}

// seedMediaAndDownloadDBs builds a media tree, a media DB and a download DB
// with one row each for the same path.
func seedCompareDBs(t *testing.T, downloadRow, mediaRow *MetadataFile) (*Config, SyncSettings) {
	t.Helper()
	resetGlobalStatus()
	downloadDir := t.TempDir()
	mediaDir := t.TempDir()
	ctx := context.Background()

	downDB := openTestDB(t, downloadDir)
	if err := createFileTable(ctx, downDB); err != nil {
		t.Fatal(err)
	}
	if err := insertTestRow(ctx, downDB, downloadRow); err != nil {
		t.Fatal(err)
	}
	mediaDB := openTestDB(t, mediaDir)
	if err := createFileTable(ctx, mediaDB); err != nil {
		t.Fatal(err)
	}
	if err := insertTestRow(ctx, mediaDB, mediaRow); err != nil {
		t.Fatal(err)
	}
	// The media file itself must exist on disk.
	rel := rootRel(mediaRow.Path())
	if err := os.MkdirAll(filepath.Join(mediaDir, filepath.Dir(rel)), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, rel), []byte("data"), filePerm); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DownloadDir: downloadDir, MediaDir: mediaDir}
	return cfg, validSettings()
}

func insertTestRow(ctx context.Context, db *sql.DB, f *MetadataFile) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO files ("+filesTableColumns+") VALUES (?,?,?,?,?,?,?,?)",
		f.Path(), f.Name(), f.Size(), f.ModTime().Unix(), f.ETag(), f.TimeBase(), f.ContentID(), f.Provenance())
	return err
}

func TestPrepareMetadataUpdateContentIDSkip(t *testing.T) {
	// Identical non-empty content IDs on both sides: never re-copy, even
	// across different time bases.
	cfg, s := seedCompareDBs(t,
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: `"ea"`, timeBase: timeBaseManifest, contentID: `"ea":10`, provenance: provenanceETag},
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 100, etag: `"ea"`, timeBase: timeBaseHTTP, contentID: `"ea":10`, provenance: provenanceETag},
	)
	preserve := map[string]bool{"/电影/a.nfo": true}
	need, err := cfg.prepareMetadataUpdate(context.Background(), s, preserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 0 {
		t.Fatalf("identical content ids triggered a copy: %v", need)
	}
}

func TestPrepareMetadataUpdateUnknownBaseCopiesConservatively(t *testing.T) {
	// Different content ids with an unknown base on the media side: the
	// timestamps cannot be ordered, so the file is copied once.
	cfg, s := seedCompareDBs(t,
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: `"ea"`, timeBase: timeBaseManifest, contentID: `"ea":10`, provenance: provenanceETag},
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 900, etag: `"ea"`, timeBase: timeBaseUnknown, contentID: `"ea":10`, provenance: provenanceETag},
	)
	// Force differing content ids (e.g. a re-download minted a new one).
	downDB := openTestDB(t, cfg.DownloadDir)
	if _, err := downDB.Exec("UPDATE files SET content_id = 'dl-new' WHERE path = '/电影/a.nfo'"); err != nil {
		t.Fatal(err)
	}
	preserve := map[string]bool{"/电影/a.nfo": true}
	need, err := cfg.prepareMetadataUpdate(context.Background(), s, preserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 {
		t.Fatalf("unknown base must copy conservatively: %v", need)
	}
}

func TestPrepareMetadataUpdateSameBaseComparesTimes(t *testing.T) {
	// Same known base, different content ids: pure timestamp comparison.
	cfg, s := seedCompareDBs(t,
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: `"ea2"`, timeBase: timeBaseManifest, contentID: `"ea2":10`, provenance: provenanceETag},
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 100, etag: `"ea"`, timeBase: timeBaseManifest, contentID: `"ea":10`, provenance: provenanceETag},
	)
	preserve := map[string]bool{"/电影/a.nfo": true}
	need, err := cfg.prepareMetadataUpdate(context.Background(), s, preserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 {
		t.Fatalf("newer remote on the same base must copy: %v", need)
	}

	// Remote older: skip.
	cfg2, s2 := seedCompareDBs(t,
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 100, etag: `"ea2"`, timeBase: timeBaseManifest, contentID: `"ea2":10`, provenance: provenanceETag},
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: `"ea"`, timeBase: timeBaseManifest, contentID: `"ea":10`, provenance: provenanceETag},
	)
	need2, err := cfg2.prepareMetadataUpdate(context.Background(), s2, preserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(need2) != 0 {
		t.Fatalf("older remote on the same base must skip: %v", need2)
	}
}

func TestPrepareMetadataUpdateEmptyIDsNeverEqual(t *testing.T) {
	// Two empty content ids (no strong etag anywhere) must not short-circuit
	// when the bases differ.
	cfg, s := seedCompareDBs(t,
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: "", timeBase: timeBaseManifest},
		&MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 10, modified: 500, etag: "", timeBase: timeBaseHTTP},
	)
	preserve := map[string]bool{"/电影/a.nfo": true}
	need, err := cfg.prepareMetadataUpdate(context.Background(), s, preserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 {
		t.Fatalf("empty ids with different bases must copy: %v", need)
	}
}

func TestManifestRoundIdentityMigrationViaObservation(t *testing.T) {
	// A row on the HTTP time base is re-identified via a HEAD observation
	// (size + strong etag) before its timestamp is rewritten to the
	// manifest base; insufficient evidence means re-download.
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:04 /电影/a.nfo\n")
		m.addFile("/电影/a.nfo", "AAA", `"ea"`)
	}

	// Local row: same size + same strong etag, HTTP base, older timestamp.
	if err := os.MkdirAll(filepath.Join(dir, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "电影", "a.nfo"), []byte("AAA"), filePerm); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, dir)
	ctx := context.Background()
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := insertTestRow(ctx, db, &MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 3, modified: 1000, etag: `"ea"`, timeBase: timeBaseHTTP, contentID: `"ea":3`, provenance: provenanceETag}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mc := newFullCrawler(dir, []string{m1.URL, m2.URL}, []string{m1.URL, m2.URL}, 2)
	mc.manifestLastMod = testManifestLastModified
	// The stored generation differs so the short-circuit is not taken.
	if err := mc.Sync(ctx); err != nil {
		t.Fatalf("manifest sync failed: %v", err)
	}
	db2 := openTestDB(t, dir)
	_, modified, _, base, cid, _ := rowIdentity(t, db2, "/电影/a.nfo")
	ts, _ := time.Parse(manifestTimeLayout, "2024-01-02 03:04")
	if base != timeBaseManifest || modified != ts.Unix() {
		t.Fatalf("row not migrated to manifest base: base=%s modified=%d want=%d", base, modified, ts.Unix())
	}
	if cid != `"ea":3` {
		t.Fatalf("content id changed during migration: %q", cid)
	}
	// The unchanged file must not have been re-downloaded.
	if m1.gets("/电影/a.nfo")+m2.gets("/电影/a.nfo") != 0 {
		t.Fatal("verified row was re-downloaded")
	}
}

func TestManifestRoundIdentityMismatchDownloads(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	m1, m2 := newMirrorServer(t), newMirrorServer(t)
	for _, m := range []*mirrorTestServer{m1, m2} {
		m.setManifest(t, "2024-01-02 03:04 /电影/a.nfo\n")
		// Different remote content with a different strong etag: the local
		// row cannot be re-identified, so it must be downloaded.
		m.addFile("/电影/a.nfo", "XXXX", `"enew"`)
	}
	if err := os.MkdirAll(filepath.Join(dir, "电影"), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "电影", "a.nfo"), []byte("AAA"), filePerm); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t, dir)
	ctx := context.Background()
	if err := createFileTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := insertTestRow(ctx, db, &MetadataFile{path: "/电影/a.nfo", name: "a.nfo", size: 3, modified: 1000, etag: `"ea"`, timeBase: timeBaseHTTP, contentID: `"ea":3`, provenance: provenanceETag}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mc := newFullCrawler(dir, []string{m1.URL, m2.URL}, []string{m1.URL, m2.URL}, 2)
	mc.manifestLastMod = testManifestLastModified
	if err := mc.Sync(ctx); err != nil {
		t.Fatalf("manifest sync failed: %v", err)
	}
	if m1.gets("/电影/a.nfo")+m2.gets("/电影/a.nfo") == 0 {
		t.Fatal("mismatched identity was not re-downloaded")
	}
	db2 := openTestDB(t, dir)
	size, _, etag, base, _, _ := rowIdentity(t, db2, "/电影/a.nfo")
	if size != 4 || etag != `"enew"` || base != timeBaseManifest {
		t.Fatalf("re-downloaded row identity: size=%d etag=%s base=%s", size, etag, base)
	}
}
