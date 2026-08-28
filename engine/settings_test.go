package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validSettings() SyncSettings {
	return SyncSettings{
		RunMode:             7,
		RunCron:             "0 0 * * *",
		Cleanup:             false,
		Purge:               true,
		ForceCrawl:          false,
		DownloadWorkers:     4,
		MirrorURL:           []string{"https://mirror1.example.com/", "https://mirror2.example.com"},
		AlistURL:            "http://xiaoya.host:5678",
		AlistStrmRootPath:   "/d",
		AlistPathSkipVerify: []string{"/动漫/合集（115）"},
		StrmPathSkipVerify:  []string{"/115"},
	}
}

func TestValidateSyncSettingsForWeb(t *testing.T) {
	s := validSettings()
	if err := validateSyncSettingsForWeb(s); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}

	// At least one behavior bit must stay enabled.
	noBits := s
	noBits.RunMode = 0
	if err := validateSyncSettingsForWeb(noBits); err == nil {
		t.Fatal("mode without behavior bits accepted")
	}
	downloadOnly := s
	downloadOnly.RunMode = 4
	if err := validateSyncSettingsForWeb(downloadOnly); err != nil {
		t.Fatalf("download-only mode rejected: %v", err)
	}
	mediaOnly := s
	mediaOnly.RunMode = 1
	if err := validateSyncSettingsForWeb(mediaOnly); err != nil {
		t.Fatalf("media-only mode rejected: %v", err)
	}

	// Legacy CLI baseline keeps accepting mode 0.
	if err := validateSyncSettingsCommon(noBits); err != nil {
		t.Fatalf("baseline validation must accept legacy modes: %v", err)
	}

	bad := s
	bad.RunMode = 8
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("out-of-range mode accepted")
	}

	bad = s
	bad.RunCron = "not a cron"
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("invalid cron accepted")
	}
	bad = s
	bad.RunCron = ""
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("empty cron accepted")
	}

	bad = s
	bad.DownloadWorkers = 65
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("out-of-range workers accepted")
	}
	bad = s
	bad.DownloadWorkers = -1
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("negative workers accepted")
	}

	bad = s
	bad.MirrorURL = []string{"https://ok.example.com/", "ftp://nope.example.com/"}
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("non-http mirror accepted")
	}

	bad = s
	bad.AlistURL = "http://xiaoya.host:5678/sub"
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("non-root alist url accepted")
	}
	bad = s
	bad.AlistURL = "://bad"
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("malformed alist url accepted")
	}

	bad = s
	bad.AlistPathSkipVerify = []string{"/"}
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("root skip path accepted")
	}
	bad = s
	bad.StrmPathSkipVerify = []string{"//"}
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("multi-slash skip path accepted (normalizes to root)")
	}
	bad = s
	bad.StrmPathSkipVerify = []string{" /// "}
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("padded multi-slash skip path accepted (normalizes to root)")
	}
	bad = s
	bad.StrmPathSkipVerify = []string{""}
	if err := validateSyncSettingsForWeb(bad); err == nil {
		t.Fatal("empty skip path accepted")
	}
}

func TestNormalizeSyncSettings(t *testing.T) {
	s := validSettings()
	s.AlistURL = "http://xiaoya.host:5678/"
	s.MirrorURL = []string{"  https://m.example.com  ", ""}
	s.AlistPathSkipVerify = []string{"动漫", "/已刮削//"}
	s.StrmPathSkipVerify = []string{"/115/", "  /ISO  "}
	n := normalizeSyncSettings(s)
	if n.AlistURL != "http://xiaoya.host:5678/" {
		t.Fatalf("alist url = %q", n.AlistURL)
	}
	if len(n.MirrorURL) != 1 || n.MirrorURL[0] != "https://m.example.com" {
		t.Fatalf("mirrors = %v", n.MirrorURL)
	}
	if len(n.AlistPathSkipVerify) != 2 || n.AlistPathSkipVerify[0] != "/动漫/" || n.AlistPathSkipVerify[1] != "/已刮削/" {
		t.Fatalf("alist skip = %v", n.AlistPathSkipVerify)
	}
	if len(n.StrmPathSkipVerify) != 2 || n.StrmPathSkipVerify[0] != "/115/" || n.StrmPathSkipVerify[1] != "/ISO/" {
		t.Fatalf("strm skip = %v", n.StrmPathSkipVerify)
	}
	// Deep copy: mutating the normalized slices must not touch the source.
	n.MirrorURL[0] = "x"
	if s.MirrorURL[0] != "  https://m.example.com  " {
		t.Fatal("normalize did not deep copy")
	}
}

func TestSettingsStoreUpdateResetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	settings, rev := store.Snapshot()
	if rev != 0 || store.Persisted() {
		t.Fatalf("fresh store revision=%d persisted=%v", rev, store.Persisted())
	}

	// Stale revision conflicts.
	if _, _, err := store.Update(99, settings); err == nil {
		t.Fatal("stale revision accepted")
	}

	next := settings
	next.DownloadWorkers = 9
	updated, rev2, err := store.Update(rev, next)
	if err != nil {
		t.Fatal(err)
	}
	if rev2 != rev+1 || updated.DownloadWorkers != 9 {
		t.Fatalf("update result rev=%d workers=%d", rev2, updated.DownloadWorkers)
	}
	if !store.Persisted() || store.PersistError() != "" {
		t.Fatal("persist state not recorded")
	}

	// A new store over the same file loads the persisted override.
	store2 := NewSettingsStore(validSettings(), dir)
	if s2, r2 := store2.Snapshot(); r2 != rev2 || s2.DownloadWorkers != 9 {
		t.Fatalf("reload rev=%d workers=%d", r2, s2.DownloadWorkers)
	}

	// Invalid candidate is rejected without bumping the revision.
	bad := updated
	bad.RunCron = "nope"
	if _, _, err := store2.Update(rev2, bad); err == nil {
		t.Fatal("invalid update accepted")
	}
	if store2.Revision() != rev2 {
		t.Fatal("revision changed on failed update")
	}

	// Reset restores the startup baseline and deletes the file.
	base := store2.Baseline()
	if base.DownloadWorkers != 4 {
		t.Fatalf("baseline workers = %d", base.DownloadWorkers)
	}
	reset, rev3, err := store2.Reset()
	if err != nil {
		t.Fatal(err)
	}
	if reset.DownloadWorkers != 4 || store2.Persisted() {
		t.Fatalf("reset result %+v persisted=%v", reset, store2.Persisted())
	}
	if _, err := os.Stat(filepath.Join(dir, settingsFileName)); !os.IsNotExist(err) {
		t.Fatal("persisted file still exists after reset")
	}
	if rev3 <= rev2 {
		t.Fatalf("revision after reset = %d, want > %d", rev3, rev2)
	}
}

func TestSettingsStoreCorruptFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, settingsFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewSettingsStore(validSettings(), dir)
	if store.Persisted() {
		t.Fatal("corrupt file counted as persisted")
	}
	if store.PersistError() == "" {
		t.Fatal("persistence error not recorded")
	}
	if s, _ := store.Snapshot(); s.DownloadWorkers != 4 {
		t.Fatal("corrupt file leaked into effective settings")
	}
	// A successful save clears the warning.
	rev := store.Revision()
	fixed := validSettings()
	fixed.Cleanup = true
	if _, _, err := store.Update(rev, fixed); err != nil {
		t.Fatal(err)
	}
	if store.PersistError() != "" || !store.Persisted() {
		t.Fatal("successful save did not clear the persistence error")
	}

	// Unknown schema versions fall back the same way.
	if err := os.WriteFile(path, []byte(`{"schema_version":99,"revision":1,"settings":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store2 := NewSettingsStore(validSettings(), dir)
	if store2.Persisted() || store2.PersistError() == "" {
		t.Fatal("unknown schema version not treated as corrupt")
	}

	// Unknown fields are rejected as corrupt.
	deep := validSettings()
	payload := map[string]any{"schema_version": settingsSchemaVersion, "revision": 1, "settings": deep, "extra": 1}
	b, _ := json.Marshal(payload)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	store3 := NewSettingsStore(validSettings(), dir)
	if store3.Persisted() || store3.PersistError() == "" {
		t.Fatal("unknown fields not treated as corrupt")
	}
}

func TestSettingsStoreAtomicPersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	s, rev := store.Snapshot()
	s.Cleanup = true
	if _, _, err := store.Update(rev, s); err != nil {
		t.Fatal(err)
	}
	// The persisted file must parse and no temp files may remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != settingsFileName {
			t.Fatalf("leftover file %q in settings dir", e.Name())
		}
	}
	info, err := os.Stat(filepath.Join(dir, settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("settings file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSettingsStoreOnUpdateNotify(t *testing.T) {
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	var calls int
	// The callback runs with the store lock held: it must only post, never
	// call back into the store.
	store.SetOnUpdate(func() { calls++ })
	s, rev := store.Snapshot()
	if _, _, err := store.Update(rev, s); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("onUpdate calls = %d, want 2", calls)
	}
}

func TestSleepContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := sleepContext(ctx, 10*time.Second); err == nil {
		t.Fatal("sleep did not honor cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation took too long")
	}
}
