package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Full rebuild modes.
const (
	fullModeStrict  = "strict"
	fullModeRelaxed = "relaxed"
)

// Full rebuild phases persisted in full_sync_state.
const (
	fullPhaseClearing    = "clearing"
	fullPhaseDownloading = "downloading"
	fullPhaseRebuilding  = "rebuilding"
)

const (
	metaFullSyncState  = "full_sync_state"
	fullStateVersion   = 1
	fullInvInsertBatch = 1000
	fullInvPageSize    = 500
	fullGCBatch        = 5000
)

// errInventoryDeferred marks authoritative inventory conditions that were
// not met (too few distinct mirrors, disagreement, malformed manifest). The
// round ends with outcome=deferred and waits for the next trigger instead
// of busy-retrying.
var errInventoryDeferred = fmt.Errorf("%w: full inventory unavailable", errDeferred)

// errFullSyncPartial keeps crawl-sourced failures recoverable while allowing
// the controller to copy this round's successful entries to media.
var errFullSyncPartial = fmt.Errorf("%w: full rebuild has pending crawl entries", errDeferred)

// fullSyncState is the versioned state machine of a (possibly interrupted)
// full rebuild. The sync ID stays stable from start to final commit; the
// inventory run ID is replaced whenever a recovery round re-validates a new
// mirror generation.
type fullSyncState struct {
	Version         int      `json:"version"`
	SyncID          string   `json:"sync_id"`
	InventoryRunID  string   `json:"inventory_run_id"`
	Mode            string   `json:"mode"`
	Phase           string   `json:"phase"`
	StartRevision   int64    `json:"start_revision"`
	Roots           []string `json:"roots"`
	InventoryCount  int      `json:"inventory_count"`
	ManifestHash    string   `json:"manifest_hash"`
	ManifestLastMod string   `json:"manifest_last_modified,omitempty"`
	StartedAt       int64    `json:"started_at"`
}

func (st *fullSyncState) syncType() string {
	if st.Mode == fullModeStrict {
		return SyncTypeFullStrict
	}
	return SyncTypeFullRelaxed
}

func readFullSyncStateDB(ctx context.Context, db sqlExecuter) (*fullSyncState, error) {
	s, err := getMeta(ctx, db, metaFullSyncState)
	if err != nil || s == "" {
		return nil, err
	}
	var st fullSyncState
	if err := json.Unmarshal([]byte(s), &st); err != nil {
		return nil, fmt.Errorf("corrupt full_sync_state: %w", err)
	}
	if st.Version != fullStateVersion {
		return nil, fmt.Errorf("unsupported full_sync_state version %d", st.Version)
	}
	return &st, nil
}

func writeFullSyncStateDB(ctx context.Context, db sqlExecuter, st *fullSyncState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return setMeta(ctx, db, metaFullSyncState, string(b))
}

// probePendingFullSync reports an unfinished full rebuild, creating the
// control tables when the database is fresh. It never mutates live state.
func probePendingFullSync(downloadDir string) (*fullSyncState, error) {
	ctx := context.Background()
	if err := os.MkdirAll(downloadDir, dirPerm); err != nil {
		return nil, err
	}
	db, err := openMetadataDB(downloadDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := createMetaTable(ctx, db); err != nil {
		return nil, err
	}
	if err := createFileTable(ctx, db); err != nil {
		return nil, err
	}
	if err := createFullTables(ctx, db); err != nil {
		return nil, err
	}
	return readFullSyncStateDB(ctx, db)
}

func createFullTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS full_inventory_runs (
			run_id TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL,
			authoritative INTEGER NOT NULL DEFAULT 0,
			orphaned INTEGER NOT NULL DEFAULT 0,
			entry_count INTEGER NOT NULL DEFAULT 0,
			manifest_hash TEXT NOT NULL DEFAULT '',
			roots_completed TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS full_inventory (
			inventory_run_id TEXT NOT NULL,
			path TEXT NOT NULL,
			time_base TEXT NOT NULL,
			modified INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL,
			PRIMARY KEY (inventory_run_id, path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_full_inventory_run ON full_inventory (inventory_run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_full_inventory_run_fold ON full_inventory (inventory_run_id, lower(path))`,
		`CREATE TABLE IF NOT EXISTS full_previous_files (
			sync_id TEXT NOT NULL,
			path TEXT NOT NULL,
			PRIMARY KEY (sync_id, path)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// inventoryResult summarizes one authoritative inventory run.
type inventoryResult struct {
	RunID           string
	Count           int
	ManifestHash    string
	ManifestLastMod string
	UsedManifest    bool
	CoveredRoots    []string // manifest-covered roots (no leading slash)
	CrawlRoots      []string // roots discovered via dual-source crawl
}

// normalizeMirrorKey canonicalizes a mirror address (scheme://host + cleaned
// path) so the "two distinct mirrors" requirement cannot be satisfied by
// one address written twice.
func normalizeMirrorKey(m string) string {
	u, err := url.Parse(strings.TrimSpace(m))
	if err != nil {
		return m
	}
	host := strings.ToLower(u.Host)
	return strings.ToLower(u.Scheme) + "://" + host + path.Clean("/"+u.Path)
}

// distinctMirrorPairs returns the first two mirrors from pool whose
// normalized addresses differ.
func distinctMirrorPair(pool []string) (string, string, error) {
	if len(pool) == 0 {
		return "", "", fmt.Errorf("no mirrors available")
	}
	first := pool[0]
	firstKey := normalizeMirrorKey(first)
	for _, m := range pool[1:] {
		if normalizeMirrorKey(m) != firstKey {
			return first, m, nil
		}
	}
	return "", "", fmt.Errorf("fewer than two distinct mirrors available")
}

// buildInventory constructs a fresh staging inventory in its own run:
//
//   - Manifest part (default): at least two distinct newest-generation
//     mirrors must return byte-identical manifests; zero malformed lines
//     are tolerated and every entry path passes the normal normalization
//     and limits.
//   - Crawl part: every valid sync root not covered by the manifest (or
//     every root under force-crawl) is traversed completely and
//     independently on two distinct crawl mirrors; the union is taken and
//     any failure on either source fails the root.
//
// Nothing touches live files, the cache directories or the media library;
// the run is marked authoritative only after all sources and roots
// completed.
func (mc *MetadataCrawler) buildInventory(ctx context.Context, db *sql.DB, forceCrawl bool) (*inventoryResult, error) {
	globalStatus.setPhase(PhaseInventory)

	roots := make([]string, 0, len(mc.selectedPaths))
	rootSet := make(map[string]bool, len(mc.selectedPaths))
	for _, p := range mc.selectedPaths {
		r := strings.TrimPrefix(p, "/")
		if r != "" && !rootSet[r] {
			rootSet[r] = true
			roots = append(roots, r)
		}
	}
	sort.Strings(roots)

	runID := "inv-" + newMaterializationID("run")
	if _, err := db.ExecContext(ctx,
		"INSERT INTO full_inventory_runs (run_id, created_at) VALUES (?, ?)", runID, time.Now().Unix()); err != nil {
		return nil, err
	}

	result := &inventoryResult{RunID: runID}
	selectedRoots := make(map[string]bool, len(roots))
	for _, r := range roots {
		selectedRoots[r] = true
	}

	// Stage entries in bounded transactions. INSERT OR REPLACE deduplicates
	// paths from duplicate manifest lines and the two crawl sources in
	// SQLite, so no whole-library map is needed in memory.
	type stagedEntry struct {
		path     string
		modified int64
		timeBase string
		source   string
	}
	batch := make([]stagedEntry, 0, fullInvInsertBatch)
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx,
			"INSERT OR REPLACE INTO full_inventory (inventory_run_id, path, time_base, modified, source) VALUES (?,?,?,?,?)")
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, e := range batch {
			if _, err := stmt.ExecContext(ctx, runID, e.path, e.timeBase, e.modified, e.source); err != nil {
				stmt.Close()
				tx.Rollback()
				return err
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	stage := func(path string, modified int64, timeBase, source string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		batch = append(batch, stagedEntry{path: path, modified: modified, timeBase: timeBase, source: source})
		if len(batch) == fullInvInsertBatch {
			return flushBatch()
		}
		return nil
	}

	var crawlRoots []string
	if !forceCrawl {
		manifestMirrors := mc.activeManifestMirrors()
		if len(manifestMirrors) < 2 {
			return nil, fmt.Errorf("%w: fewer than two newest-generation manifest mirrors", errInventoryDeferred)
		}
		first, second, err := distinctMirrorPair(manifestMirrors)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInventoryDeferred, err)
		}
		body, lastMod, _, err := fetchScanList(ctx, []string{first})
		if err != nil {
			return nil, fmt.Errorf("%w: manifest fetch failed: %v", errInventoryDeferred, err)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(body))
		confirmBody, confirmMod, _, err := fetchScanList(ctx, []string{second})
		if err != nil {
			return nil, fmt.Errorf("%w: consensus manifest fetch failed: %v", errInventoryDeferred, err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(confirmBody)) != hash {
			return nil, fmt.Errorf("%w: newest-generation mirrors disagree on manifest content", errInventoryDeferred)
		}
		_ = confirmMod

		topLevel, malformed, manifestEntries, err := walkScanList(bytes.NewReader(body), selectedRoots, func(path string, modified int64) error {
			return stage(path, modified, timeBaseManifest, "manifest")
		})
		if err != nil {
			return nil, fmt.Errorf("%w: manifest parse failed: %v", errInventoryDeferred, err)
		}
		if malformed > 0 {
			return nil, fmt.Errorf("%w: manifest contained %d malformed lines", errInventoryDeferred, malformed)
		}
		if manifestEntries == 0 {
			return nil, fmt.Errorf("%w: manifest contains no entries for the selected roots", errInventoryDeferred)
		}
		if err := flushBatch(); err != nil {
			return nil, err
		}
		result.ManifestHash = hash
		result.ManifestLastMod = lastMod
		result.UsedManifest = true
		for _, r := range roots {
			if topLevel[r] {
				result.CoveredRoots = append(result.CoveredRoots, r)
			} else {
				crawlRoots = append(crawlRoots, r)
			}
		}
		slog.Info("Full inventory manifest part staged", "entries", manifestEntries, "covered_roots", len(result.CoveredRoots), "hash", hash)
	} else {
		crawlRoots = append(crawlRoots, roots...)
	}

	if len(crawlRoots) > 0 {
		crawlPool := mc.activeCrawlMirrors()
		first, second, err := distinctMirrorPair(crawlPool)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInventoryDeferred, err)
		}
		for _, root := range crawlRoots {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			observed, err := mc.dualCrawlRoot(ctx, first, second, "/"+root, func(path string) error {
				return stage(path, 0, timeBaseHTTP, "crawl")
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				// An unavailable authoritative crawl source defers the
				// whole rebuild; no live file is touched.
				return nil, fmt.Errorf("%w: crawl root %s failed: %v", errInventoryDeferred, "/"+root, err)
			}
			if err := flushBatch(); err != nil {
				return nil, err
			}
			result.CrawlRoots = append(result.CrawlRoots, root)
			slog.Info("Full inventory crawl root completed", "root", "/"+root, "observations", observed)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM full_inventory WHERE inventory_run_id = ?", runID).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: inventory is empty", errInventoryDeferred)
	}
	// Use case-folded keys because the same cache and media trees must also be
	// representable on the case-insensitive filesystems supported by the app.
	var parentPath, childPath string
	err := db.QueryRowContext(ctx, `
		SELECT first.path, second.path
		FROM full_inventory AS first
		JOIN full_inventory AS second
		  ON second.inventory_run_id = first.inventory_run_id
		 AND lower(second.path) = lower(first.path)
		 AND second.path != first.path
		WHERE first.inventory_run_id = ?
		LIMIT 1`, runID).Scan(&parentPath, &childPath)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		return nil, fmt.Errorf("%w: paths %q and %q are aliases on a case-insensitive filesystem", errInventoryDeferred, parentPath, childPath)
	}
	err = db.QueryRowContext(ctx, `
		SELECT parent.path, child.path
		FROM full_inventory AS parent
		JOIN full_inventory AS child
		  ON child.inventory_run_id = parent.inventory_run_id
		 AND lower(child.path) >= lower(parent.path) || '/'
		 AND lower(child.path) < lower(parent.path) || '0'
		WHERE parent.inventory_run_id = ?
		LIMIT 1`, runID).Scan(&parentPath, &childPath)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		return nil, fmt.Errorf("%w: file path %q conflicts with descendant %q", errInventoryDeferred, parentPath, childPath)
	}
	result.Count = count

	completed := append(append([]string(nil), result.CoveredRoots...), result.CrawlRoots...)
	sort.Strings(completed)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE full_inventory_runs SET authoritative = 1, entry_count = ?, manifest_hash = ?, roots_completed = ? WHERE run_id = ?",
		count, result.ManifestHash, strings.Join(completed, ","), runID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	globalStatus.setDownloadPlan(count, count)
	return result, nil
}

// dualCrawlRoot walks one root completely and independently on two pinned
// mirrors and returns the union. A missing or empty root contributes an
// empty set: optional roots are not published by every metadata mirror and
// must not prevent the manifest-backed portion of a full rebuild. Once the
// root is confirmed to exist, errors below it still fail closed so a partial
// traversal can never become authoritative.
func (mc *MetadataCrawler) dualCrawlRoot(ctx context.Context, first, second, root string, emit func(string) error) (int, error) {
	walkOne := func(mirror string) (int, error) {
		info, err := mc.StatPinned(ctx, mirror, root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, nil
			}
			return 0, fmt.Errorf("pinned crawl of %s via %s failed: %w", root, sanitizeURL(mirror), err)
		}
		found := 0
		err = mc.walkPinnedRec(ctx, mirror, root, info, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			if found >= maxManifestEntries {
				return fmt.Errorf("crawl root %s exceeds the entry limit (%d)", root, maxManifestEntries)
			}
			found++
			return emit(p)
		})
		if err != nil {
			return 0, fmt.Errorf("pinned crawl of %s via %s failed: %w", root, sanitizeURL(mirror), err)
		}
		return found, nil
	}
	a, err := walkOne(first)
	if err != nil {
		return 0, err
	}
	b, err := walkOne(second)
	if err != nil {
		return 0, err
	}
	return a + b, nil
}

// SyncFull executes (or resumes) a full download-cache rebuild in the given
// mode. startRevision records the settings revision that launched the round
// (audit only; it never influences behavior). It returns nil when the
// rebuild fully committed; an error leaves the persistent state machine in
// place so any later round resumes it.
func (mc *MetadataCrawler) SyncFull(ctx context.Context, mode string, startRevision int64) error {
	if mode != fullModeStrict && mode != fullModeRelaxed {
		return fmt.Errorf("invalid full sync mode %q", mode)
	}
	globalStatus.setCleanupEnabled(mc.cleanup)
	if _, err := mc.openRoot(); err != nil {
		return err
	}
	defer mc.closeRoot()

	db, err := openMetadataDB(mc.downloadDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := createMetaTable(ctx, db); err != nil {
		return err
	}
	if err := createFileTable(ctx, db); err != nil {
		return err
	}
	if err := createFullTables(ctx, db); err != nil {
		return err
	}

	st, err := readFullSyncStateDB(ctx, db)
	if err != nil {
		return err
	}
	// Crash recovery: quarantine is reconciled before anything else so an
	// interrupted finalization cannot delete still-tracked files.
	pendingSyncID := ""
	if st != nil {
		pendingSyncID = st.SyncID
	}
	if err := mc.reconcileTrash(ctx, db, pendingSyncID); err != nil {
		return err
	}

	settingsRev := startRevision

	// Both branches stage a fresh authoritative inventory; it is kept (not
	// reconstructed from the state) so finalizeFull sees the manifest root
	// coverage needed for the covered_roots marker.
	var inv *inventoryResult
	if st == nil {
		// First run of this full rebuild: stage the authoritative
		// inventory, then atomically flip into the destructive phase.
		var err error
		inv, err = mc.buildInventory(ctx, db, mc.forceCrawl)
		if err != nil {
			return err
		}
		st = &fullSyncState{
			Version:         fullStateVersion,
			SyncID:          newMaterializationID("fs"),
			InventoryRunID:  inv.RunID,
			Mode:            mode,
			Phase:           fullPhaseClearing,
			StartRevision:   settingsRev,
			Roots:           mc.rootNames(),
			InventoryCount:  inv.Count,
			ManifestHash:    inv.ManifestHash,
			ManifestLastMod: inv.ManifestLastMod,
			StartedAt:       time.Now().Unix(),
		}
		if mode == fullModeRelaxed {
			st.Phase = fullPhaseRebuilding
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if mode == fullModeRelaxed {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO full_previous_files (sync_id, path) SELECT ?, path FROM files", st.SyncID); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM files"); err != nil {
			tx.Rollback()
			return err
		}
		// Invalidate the incremental success markers: a crashed rebuild
		// must not be short-circuited by the next incremental round.
		if err := deleteMeta(ctx, tx, metaScanListLastModified); err != nil {
			tx.Rollback()
			return err
		}
		if err := deleteMeta(ctx, tx, metaScanListHash); err != nil {
			tx.Rollback()
			return err
		}
		if err := writeFullSyncStateDB(ctx, tx, st); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slog.Info("Full rebuild state committed", "sync_id", st.SyncID, "mode", mode, "phase", st.Phase, "inventory", inv.Count)
	} else {
		// Recovery: re-validate the world with a brand new inventory run;
		// the stable sync ID and the previous-files snapshot are kept.
		var err error
		inv, err = mc.buildInventory(ctx, db, mc.forceCrawl)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if st.InventoryRunID != inv.RunID {
			if _, err := tx.ExecContext(ctx, "UPDATE full_inventory_runs SET orphaned = 1 WHERE run_id = ?", st.InventoryRunID); err != nil {
				tx.Rollback()
				return err
			}
		}
		st.InventoryRunID = inv.RunID
		st.InventoryCount = inv.Count
		st.ManifestHash = inv.ManifestHash
		st.ManifestLastMod = inv.ManifestLastMod
		if err := writeFullSyncStateDB(ctx, tx, st); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slog.Info("Full rebuild recovery: inventory generation replaced", "sync_id", st.SyncID, "phase", st.Phase, "inventory", inv.Count)
	}

	if st.Phase == fullPhaseClearing {
		if err := mc.clearSyncRoots(ctx, st); err != nil {
			return err
		}
		st.Phase = fullPhaseDownloading
		if err := writeFullSyncStateDB(ctx, db, st); err != nil {
			return err
		}
	}

	switch st.Phase {
	case fullPhaseDownloading:
		if err := mc.processStrictFull(ctx, db, st, inv); err != nil {
			return err
		}
	case fullPhaseRebuilding:
		if err := mc.processRelaxedFull(ctx, db, st, inv); err != nil {
			return err
		}
	default:
		return fmt.Errorf("full sync state has unexpected phase %q", st.Phase)
	}

	return mc.finalizeFull(ctx, db, st, inv)
}

// rootNames returns the single-level sync root names (no leading slash).
func (mc *MetadataCrawler) rootNames() []string {
	out := make([]string, 0, len(mc.selectedPaths))
	seen := make(map[string]bool, len(mc.selectedPaths))
	for _, p := range mc.selectedPaths {
		r := strings.TrimPrefix(p, "/")
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// reservedTopLevel names below downloadDir that a rebuild must never
// delete: the metadata database, the persisted settings and the quarantine
// tree.
var reservedTopLevel = map[string]bool{
	".metadata.db":      true,
	".xiaoya-emby.json": true,
	trashDirName:        true,
}

// clearSyncRoots removes the configured single-level sync roots below
// downloadDir. Deletion targets are normalized, deduplicated, non-empty,
// non-dot, non-reserved names; os.Root confines the removal and symlinks
// are removed as links, never followed.
func (mc *MetadataCrawler) clearSyncRoots(ctx context.Context, st *fullSyncState) error {
	globalStatus.setPhase(PhaseClearingCache)
	if mc.fsRoot == nil {
		return errors.New("metadata filesystem root is not open")
	}
	seen := make(map[string]bool)
	for _, root := range st.Roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := path.Clean("/" + root)
		name = strings.TrimPrefix(name, "/")
		if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || reservedTopLevel[name] || seen[name] {
			continue
		}
		seen[name] = true
		slog.Warn("Clearing download sync root for strict full rebuild", "root", "/"+name)
		if err := mc.fsRoot.RemoveAll(name); err != nil {
			return fmt.Errorf("cannot clear sync root %s: %w", "/"+name, err)
		}
	}
	return nil
}

type invEntry struct {
	path     string
	timeBase string
	modified int64
	source   string
}

// pageInventory reads one bounded page of the staging inventory by path
// keyset, fully draining and closing the cursor before returning.
func pageInventory(ctx context.Context, db *sql.DB, runID, after string, limit int) ([]invEntry, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT path, time_base, modified, source FROM full_inventory WHERE inventory_run_id = ? AND path > ? ORDER BY path LIMIT ?",
		runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []invEntry
	for rows.Next() {
		var e invEntry
		if err := rows.Scan(&e.path, &e.timeBase, &e.modified, &e.source); err != nil {
			return nil, err
		}
		page = append(page, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// invMirrors selects the download mirror pool for an entry source.
func (mc *MetadataCrawler) invMirrors(source string) []string {
	if source == "manifest" {
		if m := mc.activeManifestMirrors(); len(m) > 0 {
			return m
		}
	}
	return mc.activeCrawlMirrors()
}

// strictResumeMatch reports whether an existing row plus its on-disk file
// are confirmed to still match the current remote: identity encoding
// honored, sizes equal everywhere and equal strong ETags. Anything less
// re-downloads.
func strictResumeMatch(row *MetadataFile, info os.FileInfo, rm *remoteMeta) bool {
	if rm == nil || rm.encoded || !rm.hasSize || rm.size < 0 {
		return false
	}
	if info == nil || !info.Mode().IsRegular() || info.Size() != row.Size() {
		return false
	}
	if rm.size != row.Size() {
		return false
	}
	return strongETagEqual(rm.etag, row.ETag())
}

// runFullPages drives page-by-page processing: each page is read into a
// bounded slice, its cursor closed, workers dispatched, and the page's
// single writer fully committed before the next page starts.
func (mc *MetadataCrawler) runFullPages(
	ctx context.Context,
	db *sql.DB,
	st *fullSyncState,
	inv *inventoryResult,
	strict bool,
) error {
	processEntry := func(ctx context.Context, e invEntry, files chan<- *MetadataFile) error {
		if strict {
			row, err := pickFirstFile(ctx, db, e.path)
			if err != nil {
				return err
			}
			if row != nil {
				info, statErr := mc.rootStat(e.path)
				if statErr != nil && !os.IsNotExist(statErr) {
					return statErr
				}
				if statErr == nil {
					rm, herr := mc.headIdentity(ctx, mc.invMirrors(e.source), e.path)
					if herr != nil && ctx.Err() != nil {
						return ctx.Err()
					}
					if herr == nil && strictResumeMatch(row, info, rm) {
						globalStatus.incReused()
						return nil // verified progress: keep row and file
					}
				}
			}
			file, err := mc.Download(ctx, e.path, downloadOpts{
				mirrors:       mc.invMirrors(e.source),
				modified:      manifestModified(e),
				expectFile:    true,
				timeBase:      e.timeBase,
				identity:      true,
				repairParents: true,
			})
			if err != nil {
				return err
			}
			if file != nil {
				files <- file
				globalStatus.incDownloaded()
			}
			return nil
		}
		return mc.relaxedEntry(ctx, e, files)
	}

	var failed []invEntry
	after := ""
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := pageInventory(ctx, db, st.InventoryRunID, after, fullInvPageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].path

		var (
			mux     sync.Mutex
			pageBad []invEntry
			wg      sync.WaitGroup
			slots   = make(chan struct{}, mc.workerCount())
			files   = make(chan *MetadataFile, mc.workerCount()*2)
		)
		writer := startMetadataFilesWriter(ctx, db, files)
		for _, e := range page {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(e invEntry) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				if err := processEntry(ctx, e, files); err != nil {
					if ctx.Err() == nil {
						slog.Error("Full rebuild entry failed", "path", e.path, "error", err)
					}
					mux.Lock()
					pageBad = append(pageBad, e)
					mux.Unlock()
				}
			}(e)
		}
		wg.Wait()
		close(files)
		if err := <-writer; err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed = append(failed, pageBad...)
	}
	globalStatus.setFailed(len(failed))

	// Bounded retries for transient per-entry failures. Entries still failing
	// at the limit are explicitly ignored so successful entries continue to
	// media without publishing a complete-manifest marker.
	for retry := 0; len(failed) > 0; retry++ {
		if retry >= maxMetadataEntryRetryRounds {
			hasCrawlFailure := false
			for _, e := range failed {
				mc.markIgnored(e.path)
				hasCrawlFailure = hasCrawlFailure || e.source == "crawl"
			}
			globalStatus.addIgnored(len(failed))
			globalStatus.setFailed(0)
			if hasCrawlFailure {
				return fmt.Errorf("%w: %d entries failed after maximum retries", errFullSyncPartial, len(failed))
			}

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			stmt, err := tx.PrepareContext(ctx, "DELETE FROM full_inventory WHERE inventory_run_id = ? AND path = ?")
			if err != nil {
				tx.Rollback()
				return err
			}
			for _, e := range failed {
				if _, err := stmt.ExecContext(ctx, st.InventoryRunID, e.path); err != nil {
					stmt.Close()
					tx.Rollback()
					return err
				}
			}
			stmt.Close()
			if _, err := tx.ExecContext(ctx, "UPDATE full_inventory_runs SET entry_count = entry_count - ? WHERE run_id = ?", len(failed), st.InventoryRunID); err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			inv.Count -= len(failed)
			slog.Warn("Ignoring full rebuild entries after maximum retries; successful entries will continue to media", "count", len(failed))
			return nil
		}
		if retry > 0 {
			slog.Info("Failed full rebuild entries will be retried...", "count", len(failed))
			globalStatus.setRetryRound(retry)
			if err := sleepContext(ctx, time.Duration(1<<min(retry, maxMetadataEntryRetryRounds))*metadataRetryBaseDelay); err != nil {
				return err
			}
		}
		var (
			mux   sync.Mutex
			next  []invEntry
			wg    sync.WaitGroup
			files = make(chan *MetadataFile, mc.workerCount()*2)
		)
		writer := startMetadataFilesWriter(ctx, db, files)
		// Fixed worker pool: the failed set can hold a whole library's
		// worth of entries, so goroutines must not scale with it.
		jobs := make(chan invEntry)
		go func() {
			defer close(jobs)
			for _, e := range failed {
				select {
				case jobs <- e:
				case <-ctx.Done():
					return
				}
			}
		}()
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for e := range jobs {
					if ctx.Err() != nil {
						return
					}
					if err := processEntry(ctx, e, files); err != nil {
						if ctx.Err() == nil {
							slog.Error("Full rebuild entry failed", "path", e.path, "error", err)
						}
						mux.Lock()
						next = append(next, e)
						mux.Unlock()
					}
				}
			}()
		}
		wg.Wait()
		close(files)
		if err := <-writer; err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed = next
		globalStatus.setFailed(len(failed))
	}
	return nil
}

func manifestModified(e invEntry) int64 {
	if e.timeBase == timeBaseManifest {
		return e.modified
	}
	return 0
}

// relaxedEntry processes one entry with the size/mtime reuse heuristic:
// reuse only when the server honored identity encoding, reports a valid
// size and time, sizes match exactly and the local second-precision mtime
// is not older than the mirror time; otherwise download.
func (mc *MetadataCrawler) relaxedEntry(ctx context.Context, e invEntry, files chan<- *MetadataFile) error {
	rm, err := mc.headIdentity(ctx, mc.invMirrors(e.source), e.path)
	if err != nil {
		return err
	}
	if rm != nil {
		remoteTime := int64(0)
		switch {
		case e.timeBase == timeBaseManifest:
			remoteTime = e.modified
		case rm.hasMod:
			remoteTime = rm.modif.Unix()
		}
		if info, statErr := mc.rootStat(e.path); statErr == nil &&
			info.Mode().IsRegular() &&
			!rm.encoded && rm.hasSize && rm.size >= 0 &&
			remoteTime > 0 && remoteTime <= time.Now().Add(maxManifestFutureSkew).Unix() &&
			info.Size() == rm.size && info.ModTime().Unix() >= remoteTime {
			f := &MetadataFile{
				path:     e.path,
				name:     path.Base(e.path),
				size:     rm.size,
				modified: remoteTime,
				etag:     rm.etag,
				timeBase: e.timeBase,
			}
			if cid := contentIDFor(rm.etag, rm.size); cid != "" {
				f.contentID, f.provenance = cid, provenanceETag
			} else {
				f.contentID, f.provenance = newMaterializationID("reuse"), provenanceReused
			}
			if mt := f.ModTime(); mc.fsRoot != nil {
				if err := mc.fsRoot.Chtimes(rootRel(e.path), mt, mt); err != nil {
					if os.IsNotExist(err) {
						slog.Warn("Local file disappeared during full rebuild reuse check; downloading it", "path", e.path)
					} else {
						slog.Warn("Failed to set file modification time", "path", e.path, "error", err)
						files <- f
						globalStatus.incReused()
						return nil
					}
				} else {
					files <- f
					globalStatus.incReused()
					return nil
				}
			}
		}
	}
	file, err := mc.Download(ctx, e.path, downloadOpts{
		mirrors:       mc.invMirrors(e.source),
		modified:      manifestModified(e),
		expectFile:    true,
		timeBase:      e.timeBase,
		identity:      true,
		repairParents: true,
	})
	if err != nil {
		return err
	}
	if file != nil {
		files <- file
		globalStatus.incDownloaded()
	}
	return nil
}

func (mc *MetadataCrawler) processStrictFull(ctx context.Context, db *sql.DB, st *fullSyncState, inv *inventoryResult) error {
	globalStatus.setPhase(PhaseDownloading)
	slog.Info("Strict full rebuild downloading", "inventory", inv.Count)
	return mc.runFullPages(ctx, db, st, inv, true)
}

func (mc *MetadataCrawler) processRelaxedFull(ctx context.Context, db *sql.DB, st *fullSyncState, inv *inventoryResult) error {
	globalStatus.setPhase(PhaseRebuilding)
	slog.Info("Relaxed full rebuild reusing/downloading", "inventory", inv.Count)
	return mc.runFullPages(ctx, db, st, inv, false)
}

// pageFullDiff reads one keyset page of the distinct deletion diff. SQL's
// UNION deduplicates paths that occur in both the live rows and the
// pre-rebuild snapshot, keeping memory bounded by one page.
func pageFullDiff(ctx context.Context, db *sql.DB, runID, syncID, after string, limit int) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.path FROM (
			SELECT f.path AS path
			FROM files f
			LEFT JOIN full_inventory i ON i.inventory_run_id = ? AND i.path = f.path
			WHERE i.path IS NULL
			UNION
			SELECT p.path AS path
			FROM full_previous_files p
			LEFT JOIN full_inventory i ON i.inventory_run_id = ? AND i.path = p.path
			WHERE p.sync_id = ? AND i.path IS NULL
		) AS d
		WHERE d.path > ?
		ORDER BY d.path
		LIMIT ?`, runID, runID, syncID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]string, 0, limit)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		page = append(page, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// finalizeFull quarantines the deletion diff, verifies completeness and
// commits the rebuild. The final transaction only clears state/references
// and marks staging orphaned; bulk staging rows are GC'ed in batches
// afterwards.
func (mc *MetadataCrawler) finalizeFull(ctx context.Context, db *sql.DB, st *fullSyncState, inv *inventoryResult) error {
	globalStatus.setPhase(PhaseCleanup)

	// Strict always quarantines the diff so previous-generation partial
	// files cannot survive; relaxed only quarantines when cleanup is
	// enabled, otherwise rows are removed and files become untracked.
	quarantine := st.Mode == fullModeStrict || mc.cleanup
	deleted := 0
	if quarantine && mc.fsRoot != nil {
		after := ""
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			page, err := pageFullDiff(ctx, db, st.InventoryRunID, st.SyncID, after, fullInvPageSize)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			after = page[len(page)-1]
			for _, p := range page {
				if mc.ignoredPaths[p] {
					continue
				}
				fp := rootRel(p)
				trashPath := filepath.Join(trashDirName, fp)
				if info, err := mc.fsRoot.Lstat(fp); err == nil && info.IsDir() {
					// A stale file path can become a parent directory in the new
					// inventory. Preserve that directory and its downloaded children.
					deleted++
					continue
				}
				if err := mc.fsRoot.MkdirAll(filepath.Dir(trashPath), dirPerm); err != nil {
					return err
				}
				if err := renameReplaceCachePath(mc.fsRoot, fp, trashPath); err != nil {
					if !os.IsNotExist(err) {
						return err
					}
				} else {
					deleteEmptyRootDirs(mc.fsRoot, filepath.Dir(fp))
				}
				deleted++
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	deleteResult, err := tx.ExecContext(ctx,
		"DELETE FROM files WHERE NOT EXISTS (SELECT 1 FROM full_inventory i WHERE i.inventory_run_id = ? AND i.path = files.path)",
		st.InventoryRunID)
	if err != nil {
		tx.Rollback()
		return err
	}
	if !quarantine {
		// Without quarantine only live rows are affected; report actual DB
		// deletions rather than snapshot-only paths.
		if n, err := deleteResult.RowsAffected(); err == nil {
			deleted = int(n)
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&count); err != nil {
		tx.Rollback()
		return err
	}
	if count != inv.Count {
		tx.Rollback()
		return fmt.Errorf("full rebuild completeness check failed: %d rows present, inventory has %d", count, inv.Count)
	}
	// Success markers so the next incremental round short-circuits on the
	// same generation (or, for force-crawl runs, starts from the HTTP base).
	if inv.UsedManifest {
		if err := setMeta(ctx, tx, metaTimeBase, timeBaseManifest); err != nil {
			tx.Rollback()
			return err
		}
		// An ignored manifest entry must be retried by the next incremental
		// round, so only complete rebuilds may publish short-circuit markers.
		if len(mc.ignoredPaths) == 0 {
			if err := setMeta(ctx, tx, metaScanListHash, inv.ManifestHash); err != nil {
				tx.Rollback()
				return err
			}
			if err := setMeta(ctx, tx, metaScanListLastModified, inv.ManifestLastMod); err != nil {
				tx.Rollback()
				return err
			}
		}
		covered := inv.CoveredRoots
		if covered == nil {
			covered = []string{}
		}
		if err := setMeta(ctx, tx, metaCoveredRoots, strings.Join(covered, ",")); err != nil {
			tx.Rollback()
			return err
		}
		if err := setMeta(ctx, tx, metaPendingRootDrops, ""); err != nil {
			tx.Rollback()
			return err
		}
	} else if err := setMeta(ctx, tx, metaTimeBase, timeBaseHTTP); err != nil {
		tx.Rollback()
		return err
	}
	if err := setMeta(ctx, tx, metaIntegrityCursor, "0"); err != nil {
		tx.Rollback()
		return err
	}
	if err := deleteMeta(ctx, tx, metaFullSyncState); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE full_inventory_runs SET orphaned = 1 WHERE run_id = ?", st.InventoryRunID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	globalStatus.addDeleted(deleted)
	slog.Info("Full rebuild committed", "sync_id", st.SyncID, "mode", st.Mode, "files", count, "deleted", deleted)

	// Post-commit cleanup: empty the quarantine and batched GC of orphaned
	// staging rows; leftovers of either are self-healing on later rounds.
	if mc.fsRoot != nil {
		if err := mc.fsRoot.RemoveAll(trashDirName); err != nil {
			slog.Warn("Failed to remove quarantine directory", "path", trashDirName, "error", err)
		}
	}
	return gcFullInventory(ctx, db, "", "")
}

// gcFullInventory removes orphaned staging rows in bounded batches: every
// inventory run other than keepRunID and every previous-files snapshot
// other than keepSyncID. It is safe to call repeatedly; the controller
// serializes idle sweeps with sync jobs through maintenanceMu.
func gcFullInventory(ctx context.Context, db *sql.DB, keepRunID, keepSyncID string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := db.ExecContext(ctx,
			"DELETE FROM full_inventory WHERE rowid IN (SELECT rowid FROM full_inventory WHERE inventory_run_id != ? LIMIT ?)",
			keepRunID, fullGCBatch)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM full_inventory_runs WHERE run_id != ?", keepRunID); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := db.ExecContext(ctx,
			"DELETE FROM full_previous_files WHERE rowid IN (SELECT rowid FROM full_previous_files WHERE sync_id != ? LIMIT ?)",
			keepSyncID, fullGCBatch)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			break
		}
	}
	return nil
}

// sweepOrphanFullInventory is the idle-window entry point used after a job
// finishes: it keeps only what the pending state still references.
func sweepOrphanFullInventory(ctx context.Context, downloadDir string) error {
	db, err := openMetadataDB(downloadDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := createMetaTable(ctx, db); err != nil {
		return err
	}
	if err := createFullTables(ctx, db); err != nil {
		return err
	}
	st, err := readFullSyncStateDB(ctx, db)
	if err != nil {
		return err
	}
	keepRun, keepSync := "", ""
	if st != nil {
		keepRun, keepSync = st.InventoryRunID, st.SyncID
	}
	return gcFullInventory(ctx, db, keepRun, keepSync)
}
