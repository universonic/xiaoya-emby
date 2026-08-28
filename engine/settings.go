package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
)

// settingsSchemaVersion is the schema version of the persisted settings
// override. Unknown versions (written by a newer binary) are treated as
// corrupt and fall back to the startup baseline.
const settingsSchemaVersion = 1

// settingsFileName is the persisted settings override stored directly below
// downloadDir. It survives full rebuilds, which only ever clear the sync
// roots.
const settingsFileName = ".xiaoya-emby.json"

// Run mode behavior bits. Bit 2 is unused today; web edits keep the raw
// RunMode integer, so any future meaning of bit 2 survives a save.
const (
	modeBitMedia    = 1
	modeBitDownload = 4
)

// SyncSettings is the hot-updatable subset of the run configuration. Every
// sync round receives an immutable deep copy; editing settings while a round
// is running only affects the next round.
type SyncSettings struct {
	RunMode             int      `json:"run_mode"`
	RunCron             string   `json:"run_cron"`
	Cleanup             bool     `json:"cleanup"`
	Purge               bool     `json:"purge"`
	ForceCrawl          bool     `json:"force_crawl"`
	DownloadWorkers     int      `json:"download_workers"`
	MirrorURL           []string `json:"mirror_url"`
	AlistURL            string   `json:"alist_url"`
	AlistStrmRootPath   string   `json:"alist_strm_root_path"`
	AlistPathSkipVerify []string `json:"alist_path_skip_verify"`
	StrmPathSkipVerify  []string `json:"strm_path_skip_verify"`
}

// Clone returns a deep copy with all slices detached from the original.
func (s SyncSettings) Clone() SyncSettings {
	out := s
	out.MirrorURL = append([]string(nil), s.MirrorURL...)
	out.AlistPathSkipVerify = append([]string(nil), s.AlistPathSkipVerify...)
	out.StrmPathSkipVerify = append([]string(nil), s.StrmPathSkipVerify...)
	return out
}

func (s SyncSettings) DownloadEnabled() bool { return s.RunMode&modeBitDownload != 0 }
func (s SyncSettings) MediaEnabled() bool    { return s.RunMode&modeBitMedia != 0 }

// validateSyncSettingsForWeb checks a candidate coming from the control API
// without mutating it. Web saves must keep at least one behavior bit enabled.
func validateSyncSettingsForWeb(s SyncSettings) error {
	if s.RunMode&modeBitMedia == 0 && s.RunMode&modeBitDownload == 0 {
		return fmt.Errorf("run mode %d must enable at least one of the download (%d) or media (%d) bits", s.RunMode, modeBitDownload, modeBitMedia)
	}
	return validateSyncSettingsCommon(s)
}

// validateSyncSettingsCommon applies the rules shared by web saves and the
// startup baseline. Legacy CLI mode values (including 0) are accepted
// verbatim at startup, so the mode itself is only checked by the web rules.
func validateSyncSettingsCommon(s SyncSettings) error {
	if s.RunMode < 0 || s.RunMode > 7 {
		return fmt.Errorf("run mode %d is out of range (0-7)", s.RunMode)
	}
	if s.RunCron == "" {
		return fmt.Errorf("cron expression must not be empty")
	}
	if _, err := cron.ParseStandard(s.RunCron); err != nil {
		return fmt.Errorf("invalid cron expression %q", s.RunCron)
	}
	if s.DownloadWorkers < 0 || s.DownloadWorkers > 64 {
		return fmt.Errorf("invalid download workers %d (valid: 0-64, 0 means auto)", s.DownloadWorkers)
	}
	for _, m := range s.MirrorURL {
		u, err := url.Parse(strings.TrimSpace(m))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid mirror url %q (must be an http(s) URL)", m)
		}
	}
	au := strings.TrimSuffix(strings.TrimSpace(s.AlistURL), "/") + "/"
	u, err := url.Parse(au)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid alist url %q", s.AlistURL)
	}
	if u.Path != "/" {
		return fmt.Errorf("alist url must be a root path: %q", s.AlistURL)
	}
	if err := validatePathList(s.AlistPathSkipVerify); err != nil {
		return fmt.Errorf("invalid alist skip-verify entry: %w", err)
	}
	if err := validatePathList(s.StrmPathSkipVerify); err != nil {
		return fmt.Errorf("invalid strm skip-verify entry: %w", err)
	}
	return nil
}

func validatePathList(list []string) error {
	for _, p := range list {
		// Reject anything whose normalized form is the filesystem root:
		// "/", "//", "///" all collapse to a match-everything prefix
		// (every path has the "/" prefix), which would silently disable
		// all verification.
		if strings.Trim(strings.TrimSpace(p), "/") == "" {
			return fmt.Errorf("path %q must not be empty or the filesystem root", p)
		}
	}
	return nil
}

// normalizeSyncSettings returns a canonicalized deep copy: whitespace and
// trailing slashes are trimmed, skip lists are shaped ("/prefix/"), and
// empty entries are dropped, so equivalent settings always compare equal
// after a save.
func normalizeSyncSettings(s SyncSettings) SyncSettings {
	out := s.Clone()
	out.RunCron = strings.TrimSpace(out.RunCron)
	out.AlistURL = strings.TrimSuffix(strings.TrimSpace(out.AlistURL), "/") + "/"
	out.MirrorURL = normalizeTrimmedList(out.MirrorURL)
	out.AlistPathSkipVerify = normalizeSkipList(out.AlistPathSkipVerify)
	out.StrmPathSkipVerify = normalizeSkipList(out.StrmPathSkipVerify)
	return out
}

func normalizeTrimmedList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeSkipList(list []string) []string {
	trimmed := normalizeTrimmedList(list)
	if trimmed == nil {
		return nil
	}
	out := make([]string, 0, len(trimmed))
	for _, each := range trimmed {
		if each == "/" {
			continue
		}
		each = "/" + strings.TrimPrefix(each, "/")
		each = strings.TrimRight(each, "/") + "/"
		out = append(out, each)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// persistedSettings is the on-disk layout of the settings override.
type persistedSettings struct {
	SchemaVersion int          `json:"schema_version"`
	Revision      int64        `json:"revision"`
	Settings      SyncSettings `json:"settings"`
}

// errRevisionConflict reports a stale revision in a PUT /api/config call.
var errRevisionConflict = errors.New("settings revision conflict")

// SettingsStore owns the startup baseline and the effective sync settings.
// The effective settings are the baseline overridden by the persisted file
// below downloadDir; every mutation is validated, atomically persisted and
// published with a new monotonic revision.
type SettingsStore struct {
	mu         sync.Mutex
	path       string
	baseline   SyncSettings
	current    SyncSettings
	revision   int64
	persisted  bool
	persistErr string
	onUpdate   func()
}

// NewSettingsStore loads (or falls back from) the persisted override. A
// corrupt, unreadable or schema-incompatible file never publishes a
// half-valid configuration: the startup baseline stays in force and a
// sanitized warning is recorded until the next successful save or reset.
func NewSettingsStore(baseline SyncSettings, downloadDir string) *SettingsStore {
	st := &SettingsStore{
		path:     filepath.Join(downloadDir, settingsFileName),
		baseline: normalizeSyncSettings(baseline),
	}
	st.current = st.baseline.Clone()
	if err := st.load(); err != nil {
		st.persistErr = err.Error()
		slog.Error("Persisted sync settings are unusable; using the startup baseline", "file", settingsFileName, "error", err)
	}
	return st
}

// load reads the persisted override. It must be called with the lock held.
func (st *SettingsStore) load() error {
	data, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read persisted settings: %w", err)
	}
	var p persistedSettings
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("persisted settings are malformed")
	}
	if p.SchemaVersion != settingsSchemaVersion {
		return fmt.Errorf("persisted settings schema version %d is not supported (want %d)", p.SchemaVersion, settingsSchemaVersion)
	}
	if err := validateSyncSettingsCommon(p.Settings); err != nil {
		return fmt.Errorf("persisted settings are invalid: %w", err)
	}
	st.current = normalizeSyncSettings(p.Settings)
	st.revision = p.Revision
	st.persisted = true
	return nil
}

// Snapshot returns the effective settings and their revision.
func (st *SettingsStore) Snapshot() (SyncSettings, int64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.current.Clone(), st.revision
}

// Baseline returns the startup baseline derived from CLI/env values.
func (st *SettingsStore) Baseline() SyncSettings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.baseline.Clone()
}

// Revision returns the current settings revision.
func (st *SettingsStore) Revision() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.revision
}

// Persisted reports whether a settings override file is in force.
func (st *SettingsStore) Persisted() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.persisted
}

// PersistError returns a sanitized description of the persistence error
// state (empty when the last persistence attempt succeeded).
func (st *SettingsStore) PersistError() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.persistErr
}

// SetOnUpdate registers a callback invoked after every successful mutation.
// It is called with the store lock held, so it must not call back into the
// store; posting to a channel is the intended use.
func (st *SettingsStore) SetOnUpdate(fn func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.onUpdate = fn
}

// Update validates next against expectedRev, atomically persists it and
// publishes the new snapshot. A stale revision, invalid settings or a
// persistence failure leaves the previous state untouched.
func (st *SettingsStore) Update(expectedRev int64, next SyncSettings) (SyncSettings, int64, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if expectedRev != st.revision {
		return SyncSettings{}, 0, errRevisionConflict
	}
	if err := validateSyncSettingsForWeb(next); err != nil {
		return SyncSettings{}, 0, err
	}
	normalized := normalizeSyncSettings(next)
	rev := st.revision + 1
	if err := st.persist(&persistedSettings{SchemaVersion: settingsSchemaVersion, Revision: rev, Settings: normalized}); err != nil {
		return SyncSettings{}, 0, err
	}
	st.current = normalized
	st.revision = rev
	st.persisted = true
	st.persistErr = ""
	if st.onUpdate != nil {
		st.onUpdate()
	}
	return normalized.Clone(), rev, nil
}

// Reset removes the persisted override and restores the startup baseline.
func (st *SettingsStore) Reset() (SyncSettings, int64, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rev := st.revision + 1
	if err := os.Remove(st.path); err != nil && !os.IsNotExist(err) {
		return SyncSettings{}, 0, fmt.Errorf("cannot delete persisted settings: %w", err)
	}
	if err := syncDir(filepath.Dir(st.path)); err != nil {
		slog.Warn("Failed to fsync settings directory after delete", "error", err)
	}
	st.current = st.baseline.Clone()
	st.revision = rev
	st.persisted = false
	st.persistErr = ""
	if st.onUpdate != nil {
		st.onUpdate()
	}
	return st.current.Clone(), st.revision, nil
}

// persist atomically writes the snapshot: a 0600 temp file in the same
// directory, fsync, rename over the target, then an fsync of the directory.
func (st *SettingsStore) persist(p *persistedSettings) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("cannot create settings directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".xiaoya-emby-settings-*")
	if err != nil {
		return fmt.Errorf("cannot create settings temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		return fmt.Errorf("cannot protect settings temp file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("cannot write settings temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("cannot fsync settings temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close settings temp file: %w", err)
	}
	if err := os.Rename(tmpName, st.path); err != nil {
		return fmt.Errorf("cannot install settings file: %w", err)
	}
	tmpName = "" // renamed successfully; nothing to clean up
	if err := syncDir(dir); err != nil {
		slog.Warn("Failed to fsync settings directory", "error", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename becomes durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// sleepContext waits for d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
