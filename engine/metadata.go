package engine

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
	_ "github.com/mattn/go-sqlite3"
)

const (
	filePerm = 0644
	dirPerm  = 0755
)

const (
	// scanListPath is the pre-built manifest published by every metadata
	// mirror. One download replaces the recursive crawl of autoindex pages.
	scanListPath = "/.scan.list.gz"
	// manifestTimeLayout is the per-line timestamp format used inside the
	// manifest: "YYYY-MM-DD HH:MM /absolute/path". The timestamps are minute
	// truncated and written in the manifest generator's local timezone (not
	// GMT), so they must never be compared against HTTP Last-Modified values
	// directly; see the per-row time base handling in Sync.
	manifestTimeLayout = "2006-01-02 15:04"
	// maxScanListBytes guards against absurd manifest downloads.
	maxScanListBytes = 128 << 20
	// maxManifestPathLen rejects absurdly long entry paths so a hostile
	// manifest cannot amplify memory and log usage.
	maxManifestPathLen = 4096
	// maxMetadataFileBytes bounds one metadata response; maxRoundDownloadBytes
	// bounds cumulative bytes written by one Sync call.
	maxMetadataFileBytes  int64 = 512 << 20
	maxRoundDownloadBytes int64 = 64 << 30
	maxManifestFutureSkew       = 10 * time.Minute
	// cleanupGuardMinFiles is the minimum library size for the mass
	// deletion guard: below it every deletion is assumed intentional.
	cleanupGuardMinFiles = 100
	// perRootGuardMinFiles is the minimum per-root file count before the
	// per-root deletion ratio guard applies.
	perRootGuardMinFiles = 20
	// integrityBatchSize is how many database rows the incremental local
	// integrity sweep verifies per short-circuited round.
	integrityBatchSize          = 2000
	maxMetadataEntryRetryRounds = 5
)

// maxScanListDecompressedBytes caps how many decompressed bytes are
// accepted from the gzipped manifest, so a corrupt or malicious gzip
// stream cannot inflate without bound while entries accumulate in memory.
// It is a var only so tests can shrink it.
var (
	maxScanListDecompressedBytes int64 = 256 << 20
	// maxManifestEntries is a var only so tests can shrink it. Production
	// currently publishes about 700k entries.
	maxManifestEntries = 2_000_000
	// metadataRetryBaseDelay is a var only so tests can remove retry waits.
	metadataRetryBaseDelay = time.Second
)

// Metadata state keys stored in the meta table of .metadata.db.
const (
	metaTimeBase             = "time_base"
	metaScanListLastModified = "scan_list_last_modified"
	metaScanListHash         = "scan_list_hash"
	metaCoveredRoots         = "covered_roots"
	metaPendingRootDrops     = "pending_root_drops"
	metaIntegrityCursor      = "integrity_cursor"
	timeBaseManifest         = "manifest"
	timeBaseHTTP             = "http"
	timeBaseUnknown          = "unknown"
)

// Per-row provenance values explaining how a content identity was
// established. Rows carrying a strong ETag identity use "etag"; rows whose
// identity is only this materialization use "downloaded"/"reused" with a
// unique ID that can never accidentally equal another row's identity.
const (
	provenanceETag       = "etag"
	provenanceDownloaded = "downloaded"
	provenanceReused     = "reused"
)

// trashDirName is the quarantine directory (directly below downloadDir)
// used by the crash-safe two-phase cleanup.
var trashDirName = ".trash"

var (
	// 全局定义常量及初始值，与 Python 代码中对应
	sPaths = []string{
		"/115",
		"/ISO",
		"/PikPak",
		"/动漫",
		"/每日更新",
		"/电影",
		"/电视剧",
		"/纪录片",
		"/纪录片（已刮削）",
		"/综艺",
		"/音乐",
	}
	sMirrors = []string{
		"https://icyou.eu.org/",
		"https://emby.8.net.co/",
		"https://emby.raydoom.tk/",
		"https://embyxiaoya.laogl.top/",
		"https://emby-data.bdbd.fun/",
		"https://emby-data.ymschh.top/",
		"https://emby-data.r2s.site/",
		"https://emby-data.neversay.eu.org/",
		"https://emby-data.800686.xyz/",
		"https://emby-data.xn--yetq23gxma.org/",
		"https://emby-data.younv.at/",
		"https://emby.kaiserver.uk/",
		"https://emby-data.wwwh.eu.org/",
		"https://emby-data.f1rst.top/",
		"https://emby-data.xnn.ee/",
		"https://emby.xiaoya.pro/",
	}
	sFolder = []string{".sync"}
	sExt    = []string{".ass", ".srt", ".ssa"}
)

// errNoManifest signals that manifest mode is unavailable and the caller
// should fall back to HTML crawling.
var errNoManifest = errors.New("metadata manifest is not available")

// errManifestPending reports an integrity repair that cannot complete yet.
// It is a deferred condition: rounds end without busy retrying and wait for
// the next trigger.
var errManifestPending = fmt.Errorf("%w: manifest entries are not yet available", errDeferred)

// errDeferred marks authoritative conditions that were not met (an
// incomplete integrity repair, too few mirrors, mirror disagreement). Both
// automatic and manual rounds end with outcome=deferred instead of retrying.
var errDeferred = errors.New("sync deferred")

func isDeferredErr(err error) bool {
	return err != nil && errors.Is(err, errDeferred)
}

type MetadataCrawler struct {
	mux               sync.Mutex
	client            *http.Client
	fsRoot            *os.Root
	downloadDir       string
	mirrors           []string
	manifestMirrors   []string // mirrors serving the newest manifest, by latency
	manifestLastMod   string   // Last-Modified of the newest manifest
	crawlMirrors      []string // HTML-valid mirrors, by latency
	selectedPaths     []string
	ignoredDirs       []string // TODO:
	ignoredExtentions []string // TODO:
	ignoredPaths      map[string]bool
	cleanup           bool
	forceCrawl        bool
	workers           int
	roundBytes        atomic.Int64
}

// downloadOpts controls how a single remote file is downloaded.
type downloadOpts struct {
	// mirrors is the failover-ordered list of mirror roots to try.
	mirrors []string
	// modified, when non-zero, overrides the file timestamp stored in the
	// database and set on disk (manifest mode passes the manifest timestamp).
	modified int64
	// expectFile treats text/html responses as errors instead of silently
	// skipping them (manifest mode lists real files only).
	expectFile bool
	// timeBase is the per-row time base recorded with the download result.
	timeBase string
	// identity sends Accept-Encoding: identity so Content-Length matches the
	// bytes written (full rebuild rounds); reused rows are only allowed when
	// the server honors it.
	identity bool
	// filterFn is applied to the response metadata before the body is read;
	// returning false skips the download.
	filterFn func(f *MetadataFile) bool
}

// mirrorProbe is the result of validating one mirror.
type mirrorProbe struct {
	mirror  string
	lastMod string // Last-Modified of /.scan.list.gz, empty when unavailable
	latency time.Duration
}

// manifestEntry couples a manifest path with its manifest-time-base
// timestamp.
type manifestEntry struct {
	path string
	ts   int64
}

type failedEntry struct {
	path string
	info *MetadataFile
}

// NewMetadataCrawler prepares a crawler from the per-round settings
// snapshot. Mirror validation may probe the network and honors ctx.
func NewMetadataCrawler(ctx context.Context, downloadDir string, s SyncSettings) (*MetadataCrawler, error) {
	mc := &MetadataCrawler{
		client:      newBrowserClient(60 * time.Second),
		downloadDir: downloadDir,
		cleanup:     s.Cleanup,
		forceCrawl:  s.ForceCrawl,
		workers:     s.DownloadWorkers,
	}

	if len(s.MirrorURL) == 0 {
		mc.mirrors = append([]string(nil), sMirrors...)
	} else {
		mc.mirrors = append([]string(nil), s.MirrorURL...)
	}
	var err error
	for range 3 {
		if err = mc.validateMirrors(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}

	// Sync roots always come from the built-in root set. Skip-verify
	// prefixes only narrow media-phase verification (config.go); they must
	// never narrow the sync scope, because cleanup and strict rebuilds
	// derive their deletion roots from selectedPaths.
	mc.selectedPaths = defaultSyncRoots()
	if len(mc.ignoredDirs) == 0 {
		mc.ignoredDirs = sFolder
	}
	if len(mc.ignoredExtentions) == 0 {
		mc.ignoredExtentions = sExt
	}
	return mc, nil
}

// defaultSyncRoots returns the built-in sync roots (first path segment
// only). Skip-verify prefixes do not change the root set; they are handled
// during the media phase.
func defaultSyncRoots() []string {
	selected := sPaths
	out := make([]string, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, p := range selected {
		ss := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for _, s := range ss {
			if s != "" {
				if !seen["/"+s] {
					seen["/"+s] = true
					out = append(out, "/"+s)
				}
				break
			}
		}
	}
	return out
}

func (mc *MetadataCrawler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute * 10)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case <-ticker.C:
			if err := mc.validateMirrors(ctx); err != nil && ctx.Err() == nil {
				slog.Error("Failed to validate mirrors", "error", err)
			}
		case <-ctx.Done():
			break LOOP
		}
	}
}

// validateMirrors probes all mirrors concurrently. Mirrors exposing the
// newest manifest form the manifest pool; all HTML-valid mirrors (including
// manifest-capable ones) form the crawl pool used for fallback.
func (mc *MetadataCrawler) validateMirrors(ctx context.Context) error {
	globalStatus.setPhase(PhaseProbing)
	slog.Info("Validating metadata mirrors...")
	probes := make([]mirrorProbe, len(mc.mirrors))
	var wg sync.WaitGroup
	for i, mirror := range mc.mirrors {
		wg.Add(1)
		go func(i int, mirror string) {
			defer wg.Done()
			probes[i] = mc.probeMirror(ctx, mirror)
		}(i, mirror)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var (
		newest        time.Time
		newestStr     string
		manifestTimes = make(map[string]time.Time, len(probes))
		manifestPr    []mirrorProbe
		crawlPr       []mirrorProbe
	)
	for _, p := range probes {
		if p.lastMod == "" {
			continue
		}
		if t, err := time.Parse(time.RFC1123, p.lastMod); err == nil {
			manifestTimes[p.mirror] = t
			if newest.IsZero() || t.After(newest) {
				newest, newestStr = t, p.lastMod
			}
		}
	}
	for _, p := range probes {
		if p.mirror == "" {
			continue // invalid mirror, already logged by probeMirror
		}
		if t, ok := manifestTimes[p.mirror]; ok {
			// Only mirrors serving exactly the newest manifest generation
			// join the manifest pool. A time window here would mix
			// different generations: the fastest mirror could serve an
			// older body while the newest timestamp gets persisted.
			if newest.Equal(t) {
				manifestPr = append(manifestPr, p)
				slog.Info("Validated metadata mirror", "mirror", sanitizeURL(p.mirror), "latency_ms", p.latency.Milliseconds(), "manifest_last_modified", p.lastMod)
				continue
			}
			slog.Info("Metadata mirror manifest is stale, crawl only", "mirror", sanitizeURL(p.mirror), "manifest_last_modified", p.lastMod)
			crawlPr = append(crawlPr, p)
			continue
		}
		crawlPr = append(crawlPr, p)
		slog.Info("Validated metadata mirror (crawl only)", "mirror", sanitizeURL(p.mirror), "latency_ms", p.latency.Milliseconds())
	}
	// Manifest-capable mirrors are eligible for crawling as well.
	crawlPr = append(crawlPr, manifestPr...)

	sort.Slice(manifestPr, func(i, j int) bool { return manifestPr[i].latency < manifestPr[j].latency })
	sort.Slice(crawlPr, func(i, j int) bool { return crawlPr[i].latency < crawlPr[j].latency })

	if len(manifestPr) == 0 && len(crawlPr) == 0 {
		return fmt.Errorf("at least one metadata mirror is required")
	}

	// Report the probe outcome to the status page.
	statusMirrors := make([]mirrorStatus, 0, len(probes))
	manifestSet := make(map[string]bool, len(manifestPr))
	for _, p := range manifestPr {
		manifestSet[p.mirror] = true
	}
	crawlSet := make(map[string]bool, len(crawlPr))
	for _, p := range crawlPr {
		crawlSet[p.mirror] = true
	}
	for i, p := range probes {
		switch {
		case p.mirror == "":
			statusMirrors = append(statusMirrors, mirrorStatus{URL: sanitizeURL(mc.mirrors[i]), State: mirrorStateInvalid})
		case manifestSet[p.mirror]:
			statusMirrors = append(statusMirrors, mirrorStatus{URL: sanitizeURL(p.mirror), State: mirrorStateFresh, LatencyMs: p.latency.Milliseconds(), LastModified: p.lastMod})
		case p.lastMod != "":
			statusMirrors = append(statusMirrors, mirrorStatus{URL: sanitizeURL(p.mirror), State: mirrorStateStale, LatencyMs: p.latency.Milliseconds(), LastModified: p.lastMod})
		case crawlSet[p.mirror]:
			statusMirrors = append(statusMirrors, mirrorStatus{URL: sanitizeURL(p.mirror), State: mirrorStateCrawlOnly, LatencyMs: p.latency.Milliseconds()})
		}
	}
	globalStatus.setMirrors(statusMirrors)

	mc.mux.Lock()
	defer mc.mux.Unlock()

	mc.manifestMirrors = mc.manifestMirrors[:0]
	for _, p := range manifestPr {
		mc.manifestMirrors = append(mc.manifestMirrors, p.mirror)
	}
	mc.crawlMirrors = mc.crawlMirrors[:0]
	for _, p := range crawlPr {
		mc.crawlMirrors = append(mc.crawlMirrors, p.mirror)
	}
	mc.manifestLastMod = newestStr
	return nil
}

// probeMirror checks manifest availability first (unless force crawl) and
// falls back to the legacy HTML content check.
func (mc *MetadataCrawler) probeMirror(ctx context.Context, mirror string) mirrorProbe {
	if !mc.forceCrawl {
		for range 2 {
			if ctx.Err() != nil {
				return mirrorProbe{}
			}
			if lm, d, ok := probeScanList(ctx, mirror); ok {
				return mirrorProbe{mirror: mirror, lastMod: lm, latency: d}
			}
		}
		slog.Warn("Metadata mirror has no usable manifest", "mirror", sanitizeURL(mirror))
	}
	if d := validateMirror(ctx, mirror); d > 0 {
		return mirrorProbe{mirror: mirror, latency: d}
	}
	slog.Warn("Invalid metadata mirror", "mirror", sanitizeURL(mirror))
	return mirrorProbe{}
}

func (mc *MetadataCrawler) activeCrawlMirrors() []string {
	mc.mux.Lock()
	defer mc.mux.Unlock()

	return append([]string(nil), mc.crawlMirrors...)
}

func (mc *MetadataCrawler) activeManifestMirrors() []string {
	mc.mux.Lock()
	defer mc.mux.Unlock()

	return append([]string(nil), mc.manifestMirrors...)
}

func (mc *MetadataCrawler) activeManifestLastModified() string {
	mc.mux.Lock()
	defer mc.mux.Unlock()

	return mc.manifestLastMod
}

func (mc *MetadataCrawler) workerCount() int {
	if mc.workers > 0 {
		return mc.workers
	}
	return defaultWorkers()
}

// Sync downloads metadata, preferring the manifest-based mode and falling
// back to HTML crawling when the manifest is unavailable or force crawl is
// requested. Stale ".xtmp" files are swept by the mode implementations, only
// when downloads will actually happen, so a no-op manifest run does not pay
// for a full tree walk.
func (mc *MetadataCrawler) Sync(ctx context.Context) error {
	mc.ignoredPaths = nil
	globalStatus.setCleanupEnabled(mc.cleanup)
	if _, err := mc.openRoot(); err != nil {
		return err
	}
	defer mc.closeRoot()

	if !mc.forceCrawl {
		err := mc.syncManifest(ctx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNoManifest):
			slog.Warn("Metadata manifest unavailable, falling back to HTML crawling mode")
		default:
			return err
		}
	}
	return mc.syncCrawl(ctx)
}

// openRoot opens the rooted filesystem view of downloadDir used by all
// download/cleanup operations.
func (mc *MetadataCrawler) openRoot() (*os.Root, error) {
	if err := os.MkdirAll(mc.downloadDir, dirPerm); err != nil && !os.IsExist(err) {
		return nil, err
	}
	root, err := os.OpenRoot(mc.downloadDir)
	if err != nil {
		return nil, err
	}
	mc.fsRoot = root
	mc.roundBytes.Store(0)
	return root, nil
}

func (mc *MetadataCrawler) closeRoot() {
	if mc.fsRoot != nil {
		mc.fsRoot.Close()
		mc.fsRoot = nil
	}
}

// rootRel converts a remote-style absolute path into an os.Root-relative
// platform path. os.Root then enforces that symlink traversal cannot escape
// the configured directory tree.
func rootRel(p string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(filepath.ToSlash(p), "/"))
	return filepath.FromSlash(strings.TrimPrefix(cleaned, "/"))
}

func (mc *MetadataCrawler) rootStat(p string) (os.FileInfo, error) {
	if mc.fsRoot == nil {
		return nil, errors.New("metadata filesystem root is not open")
	}
	return mc.fsRoot.Stat(rootRel(p))
}

func readRootDir(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func deleteEmptyRootDirs(root *os.Root, dir string) {
	dir = filepath.Clean(dir)
	for dir != "." && dir != "" {
		entries, err := readRootDir(root, dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := root.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// sweepTempFiles removes stale temporary files left over from interrupted
// downloads (owned exclusively by this program: names of the form
// "<target>.xtmp"). Legitimate remote files ending in ".xtmp"
// are never touched.
func (mc *MetadataCrawler) sweepTempFiles(ctx context.Context) {
	if mc.fsRoot == nil {
		return
	}
	for _, root := range mc.selectedPaths {
		if ctx.Err() != nil {
			return
		}
		dir := rootRel(root)
		err := fs.WalkDir(mc.fsRoot.FS(), dir, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.IsDir() && isOwnTempName(entry.Name()) {
				if err := mc.fsRoot.Remove(name); err == nil {
					slog.Info("Cleaned up stale temporary file", "path", name)
				}
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			slog.Warn("Failed to sweep temporary files", "path", root, "error", err)
		}
	}
}

// reconcileTrash resolves cleanup state left by a crash. A quarantined file
// whose live files row still exists — or whose path is part of the pending
// full-sync snapshot (full_previous_files of the current pending sync) — was
// not committed as deleted and is restored; a file with neither row belongs
// to a committed deletion and is removed.
func (mc *MetadataCrawler) reconcileTrash(ctx context.Context, db *sql.DB, pendingSyncID string) error {
	if mc.fsRoot == nil {
		return errors.New("metadata filesystem root is not open")
	}
	if _, err := mc.fsRoot.Stat(trashDirName); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var restoreErr error
	err := fs.WalkDir(mc.fsRoot.FS(), trashDirName, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(trashDirName, name)
		if err != nil {
			return err
		}
		remotePath := "/" + filepath.ToSlash(rel)
		var exists int
		err = db.QueryRowContext(ctx, "SELECT 1 FROM files WHERE path = ?", remotePath).Scan(&exists)
		if err == sql.ErrNoRows && pendingSyncID != "" {
			// The pending full-sync snapshot counts as an uncommitted
			// deletion: restore it so the recovery round can decide again.
			err = db.QueryRowContext(ctx, "SELECT 1 FROM full_previous_files WHERE sync_id = ? AND path = ?", pendingSyncID, remotePath).Scan(&exists)
		}
		switch {
		case err == nil:
			if _, statErr := mc.fsRoot.Stat(rel); statErr == nil {
				// A newer copy already exists at the original path.
				if err := mc.fsRoot.Remove(name); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			if err := mc.fsRoot.MkdirAll(filepath.Dir(rel), dirPerm); err != nil && !os.IsExist(err) {
				return err
			}
			if err := renameReplaceCachePath(mc.fsRoot, name, rel); err != nil {
				return err
			}
			slog.Warn("Restored file from interrupted cleanup", "path", remotePath)
		case err == sql.ErrNoRows:
			if err := mc.fsRoot.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
		default:
			return err
		}
		return nil
	})
	if err != nil && !isDeferredErr(err) && ctx.Err() == nil {
		restoreErr = err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if restoreErr != nil {
		return restoreErr
	}
	return mc.fsRoot.RemoveAll(trashDirName)
}

var ownTempSeq atomic.Uint64

func isOwnTempName(name string) bool {
	return strings.HasSuffix(name, ".xtmp")
}

// tempPathFor returns a unique sibling temp path for filePath, so two
// workers downloading the same target never collide and the sweep can tell
// our temp files apart from genuine ".xtmp" content.
func tempPathFor(filePath string) string {
	return fmt.Sprintf("%s-%d.xtmp", filePath, ownTempSeq.Add(1))
}

func isRenameConflict(err error) bool {
	return os.IsExist(err) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENOTEMPTY)
}

// renameReplaceRoot installs oldname at newname. recursive is only for the
// disposable download cache; media paths must never recursively remove a
// directory merely because it conflicts with a metadata file.
func renameReplaceRoot(root *os.Root, oldname, newname string, recursive bool) error {
	err := root.Rename(oldname, newname)
	if err == nil || !isRenameConflict(err) {
		return err
	}
	info, statErr := root.Lstat(newname)
	if statErr != nil {
		return err
	}
	if info.IsDir() && !recursive {
		return err
	}
	if recursive {
		statErr = root.RemoveAll(newname)
	} else {
		statErr = root.Remove(newname)
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	return root.Rename(oldname, newname)
}

func renameReplaceCachePath(root *os.Root, oldname, newname string) error {
	return renameReplaceRoot(root, oldname, newname, true)
}

func renameReplaceFile(root *os.Root, oldname, newname string) error {
	return renameReplaceRoot(root, oldname, newname, false)
}

// writeFileAtomicRoot replaces rel below root via sibling temp file, close,
// rename, so an interrupted write never leaves a partial file behind.
func writeFileAtomicRoot(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	tmp := tempPathFor(rel)
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, writeErr := out.Write(data)
	closeErr := out.Close()
	if writeErr != nil {
		root.Remove(tmp)
		return writeErr
	}
	if closeErr != nil {
		root.Remove(tmp)
		return closeErr
	}
	// The temp file must not survive a failed rename: this root is the
	// media library, which sweepTempFiles does not cover.
	if err := renameReplaceFile(root, tmp, rel); err != nil {
		root.Remove(tmp)
		return err
	}
	return nil
}

// syncManifest implements the manifest-based incremental sync: download
// /.scan.list.gz, diff it against the local database (re-identifying rows
// on a foreign time base via HTTP observations) and download only what
// changed.
//
// Success markers (scan_list_last_modified / scan_list_hash) are written
// last, only after downloads and cleanup completed, so an interrupted round
// is retried instead of being short-circuited forever.
func (mc *MetadataCrawler) syncManifest(ctx context.Context) error {
	globalStatus.setMode(ModeManifest)
	globalStatus.setPhase(PhaseManifest)
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
	if err := mc.reconcileTrash(ctx, db, ""); err != nil {
		return err
	}

	mirrors := mc.activeManifestMirrors()
	if len(mirrors) == 0 {
		return errNoManifest
	}

	coveredRootsMeta, err := getMeta(ctx, db, metaCoveredRoots)
	if err != nil {
		return err
	}
	pendingDropsMeta, err := getMeta(ctx, db, metaPendingRootDrops)
	if err != nil {
		return err
	}

	body, bodyLastModified, bodySource, err := fetchScanList(ctx, mirrors)
	if err != nil {
		slog.Error("Failed to download metadata manifest", "error", err)
		return errNoManifest
	}
	bodyHash := fmt.Sprintf("%x", sha256.Sum256(body))
	cleanupAuthorized := mc.cleanup
	if cleanupAuthorized {
		// The consensus fetch must come from a canonically DISTINCT
		// mirror than the one that served the body; duplicate spellings
		// of one physical mirror never authorize deletion.
		sourceKey := normalizeMirrorKey(bodySource)
		var consensusPool []string
		for _, m := range mirrors {
			if normalizeMirrorKey(m) != sourceKey {
				consensusPool = append(consensusPool, m)
			}
		}
		if len(consensusPool) == 0 {
			cleanupAuthorized = false
			slog.Error("Skipping metadata cleanup: fewer than two distinct newest-generation mirrors are available")
			globalStatus.setCleanupGuard(true)
		} else {
			consensusBody, _, _, consensusErr := fetchScanList(ctx, consensusPool)
			if consensusErr != nil || sha256.Sum256(consensusBody) != sha256.Sum256(body) {
				cleanupAuthorized = false
				slog.Error("Skipping metadata cleanup: newest-generation mirrors do not agree on manifest content", "error", consensusErr)
				globalStatus.setCleanupGuard(true)
			}
		}
	}

	selectedRoots := make(map[string]bool, len(mc.selectedPaths))
	for _, p := range mc.selectedPaths {
		selectedRoots[strings.TrimPrefix(p, "/")] = true
	}
	entries, topLevel, malformed, err := parseScanList(bytes.NewReader(body), selectedRoots)
	if err != nil {
		slog.Error("Failed to parse metadata manifest", "error", err)
		return errNoManifest
	}
	if len(entries) == 0 {
		slog.Error("Metadata manifest contains no entries for the selected paths")
		return errNoManifest
	}
	if malformed > 0 {
		slog.Warn("Skipped malformed manifest lines", "count", malformed)
	}
	slog.Info("Parsed metadata manifest", "entries", len(entries))
	globalStatus.setPhase(PhaseParsing)
	globalStatus.setManifest(bodyLastModified, len(entries), false)

	// Warn about selected paths the manifest does not cover. Paths never
	// covered by the manifest keep their existing local state (only
	// --force-crawl syncs them). A previously covered root that vanished
	// from the manifest is only deleted after it stays missing across two
	// consecutive manifest generations (pending_root_drops confirmation),
	// so a truncated or partial manifest cannot trigger the deletion.
	prevCovered := make(map[string]bool)
	for _, r := range strings.Split(coveredRootsMeta, ",") {
		if r != "" {
			prevCovered[r] = true
		}
	}
	pendingDrops := parseRootDrops(pendingDropsMeta)
	var coveredRoots []string
	var retainedRoots []string
	newlyCoveredRoots := make(map[string]bool)
	nextPendingDrops := make(map[string]bool)
	for _, p := range mc.selectedPaths {
		r := strings.TrimPrefix(p, "/")
		if topLevel[r] {
			coveredRoots = append(coveredRoots, r)
			if !prevCovered[r] && !pendingDrops.roots[r] {
				newlyCoveredRoots[r] = true
			}
			continue
		}
		// A root recorded in pending_root_drops counts as previously
		// covered: it was covered before and its disappearance has not
		// been confirmed yet.
		wasCovered := prevCovered[r] || pendingDrops.roots[r]
		if wasCovered {
			if pendingDrops.gen != "" && pendingDrops.gen != bodyHash && pendingDrops.roots[r] {
				slog.Warn("Selected path confirmed absent from the metadata manifest across generations; its files will be cleaned up", "path", p)
				continue
			}
			nextPendingDrops[r] = true
			retainedRoots = append(retainedRoots, p)
			slog.Warn("Selected path is missing from the metadata manifest; deletion deferred until confirmed by a later manifest generation", "path", p)
			continue
		}
		retainedRoots = append(retainedRoots, p)
		slog.Warn("Selected path is not covered by the metadata manifest, skipping it; use --force-crawl to sync it via crawling", "path", p)
	}

	local, err := mc.localFiles(ctx, db)
	if err != nil {
		return err
	}
	localMap := make(map[string]*MetadataFile, len(local))
	for _, file := range local {
		localMap[file.Path()] = file
	}

	// remoteMap is only needed to protect files from cleanup; skip the
	// bookkeeping entirely when cleanup is disabled.
	var remoteMap map[string]bool
	if cleanupAuthorized {
		remoteMap = make(map[string]bool, len(entries))
	}
	var toDownload, toRewrite []manifestEntry
	// Rows on a foreign time base (HTTP observations from crawl rounds, or
	// unknown-base rows migrated from older versions) must be re-identified
	// against the current remote before their timestamps may be rewritten to
	// the manifest base; insufficient evidence means re-download.
	type verifyItem struct {
		e   manifestEntry
		old *MetadataFile
	}
	var toVerify []verifyItem
	for path, ts := range entries {
		if remoteMap != nil {
			remoteMap[path] = true
		}
		old := localMap[path]
		if old == nil {
			toDownload = append(toDownload, manifestEntry{path: path, ts: ts})
			continue
		}
		if _, err := mc.rootStat(path); err != nil {
			slog.Warn("Missing file", "path", path)
			toDownload = append(toDownload, manifestEntry{path: path, ts: ts})
			continue
		}
		if old.TimeBase() != timeBaseManifest {
			toVerify = append(toVerify, verifyItem{e: manifestEntry{path: path, ts: ts}, old: old})
			continue
		}
		if ts <= old.ModTime().Unix() {
			continue
		}
		toDownload = append(toDownload, manifestEntry{path: path, ts: ts})
	}

	if len(toVerify) > 0 {
		slog.Info("Verifying content identity of rows on a foreign time base", "count", len(toVerify))
		// A fixed worker pool (jobs channel) keeps goroutine count bounded
		// regardless of how many legacy rows must be re-identified.
		type verifyJob struct {
			e   manifestEntry
			old *MetadataFile
		}
		var (
			vmux       sync.Mutex
			vDownloads []manifestEntry
			vRewrites  []manifestEntry
			verifyWG   sync.WaitGroup
		)
		jobs := make(chan verifyJob)
		go func() {
			defer close(jobs)
			for _, item := range toVerify {
				select {
				case jobs <- verifyJob(item):
				case <-ctx.Done():
					return
				}
			}
		}()
		for range mc.workerCount() {
			verifyWG.Add(1)
			go func() {
				defer verifyWG.Done()
				for item := range jobs {
					if ctx.Err() != nil {
						return
					}
					obs, err := mc.observeHead(ctx, mirrors, item.e.path)
					if err == nil && identityMatches(item.old, obs) {
						vmux.Lock()
						if item.e.ts != item.old.ModTime().Unix() {
							vRewrites = append(vRewrites, item.e)
						}
						vmux.Unlock()
						continue
					}
					vmux.Lock()
					vDownloads = append(vDownloads, item.e)
					vmux.Unlock()
				}
			}()
		}
		verifyWG.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sortEntries(vDownloads)
		sortEntries(vRewrites)
		toDownload = append(toDownload, vDownloads...)
		toRewrite = append(toRewrite, vRewrites...)
	}

	if len(toRewrite) > 0 {
		if err := rewriteModified(ctx, db, toRewrite); err != nil {
			return err
		}
		slog.Info("Migrated verified metadata records to the manifest time base", "count", len(toRewrite))
	}
	slog.Info("Metadata files to download", "count", len(toDownload), "total", len(entries))
	globalStatus.setDownloadPlan(len(entries), len(toDownload))
	globalStatus.setPhase(PhaseDownloading)

	if len(toDownload) > 0 {
		mc.sweepTempFiles(ctx)
	}

	// runRound downloads one round of entries with a fixed-size worker
	// pool and separates transient failures from entries confirmed absent
	// on every mirror. Confirmed absences are terminal only for this trigger.
	runRound := func(list []manifestEntry) ([]manifestEntry, []manifestEntry, error) {
		var (
			mux         sync.Mutex
			next        []manifestEntry
			unavailable []manifestEntry
		)
		files := make(chan *MetadataFile, mc.workerCount()*2)
		writerDone := startMetadataFilesWriter(ctx, db, files)
		jobs := make(chan manifestEntry)
		go func() {
			defer close(jobs)
			for _, item := range list {
				select {
				case jobs <- item:
				case <-ctx.Done():
					return
				}
			}
		}()

		var wg sync.WaitGroup
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					if ctx.Err() != nil {
						return
					}
					file, derr := mc.Download(ctx, item.path, downloadOpts{
						mirrors:    mirrors,
						modified:   item.ts,
						expectFile: true,
						timeBase:   timeBaseManifest,
					})
					if derr != nil {
						if errors.Is(derr, fs.ErrNotExist) {
							slog.Warn("Manifest entry is absent from all mirrors; skipping it for this round", "path", item.path)
							globalStatus.incUnavailable()
							mux.Lock()
							unavailable = append(unavailable, item)
							mux.Unlock()
						} else {
							slog.Error("Failed to download", "path", item.path, "error", derr)
							mux.Lock()
							next = append(next, item)
							mux.Unlock()
						}
						continue
					}
					if file != nil {
						files <- file
					}
					globalStatus.incDownloaded()
				}
			}()
		}
		wg.Wait()
		close(files)
		if err := <-writerDone; err != nil {
			return next, unavailable, err
		}
		globalStatus.setFailed(len(next))
		return next, unavailable, nil
	}

	pending := toDownload
	var unavailable []manifestEntry
	for retry := 0; len(pending) > 0; retry++ {
		if retry > maxMetadataEntryRetryRounds {
			slog.Warn("Ignoring metadata entries after maximum retry attempts; continuing with the remaining files", "count", len(pending))
			globalStatus.addIgnored(len(pending))
			globalStatus.setFailed(0)
			break
		}
		if retry > 0 {
			slog.Info("Failed metadata entries will be retried...", "count", len(pending))
			globalStatus.setRetryRound(retry)
			if err := sleepContext(ctx, time.Duration(1<<min(retry, maxMetadataEntryRetryRounds))*metadataRetryBaseDelay); err != nil {
				return err
			}
		}
		var roundErr error
		var missing []manifestEntry
		pending, missing, roundErr = runRound(pending)
		if roundErr != nil {
			return roundErr
		}
		unavailable = append(unavailable, missing...)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	for _, item := range pending {
		mc.markIgnored(item.path)
	}
	for _, item := range unavailable {
		mc.markIgnored(item.path)
	}
	if len(unavailable) > 0 {
		slog.Warn("Manifest entries are temporarily unavailable on all mirrors; keeping cached state and continuing with the remaining files", "count", len(unavailable))
	}

	if err := setMeta(ctx, db, metaTimeBase, timeBaseManifest); err != nil {
		return err
	}
	if err := setMeta(ctx, db, metaCoveredRoots, strings.Join(coveredRoots, ",")); err != nil {
		return err
	}
	if err := setMeta(ctx, db, metaPendingRootDrops, encodeRootDrops(bodyHash, nextPendingDrops)); err != nil {
		return err
	}

	if cleanupAuthorized {
		if malformed > 0 {
			// A partially parseable manifest must never drive deletions.
			slog.Error("Skipping metadata cleanup: the manifest contained malformed lines", "malformed", malformed)
		} else {
			globalStatus.setPhase(PhaseCleanup)
			if err := mc.cleanupStale(ctx, db, local, remoteMap, retainedRoots); err != nil {
				return err
			}
		}
	}

	// Success markers go last: an interrupted cleanup or download round
	// must be retried rather than short-circuited.
	if err := setMeta(ctx, db, metaScanListHash, bodyHash); err != nil {
		return err
	}
	return setMeta(ctx, db, metaScanListLastModified, bodyLastModified)
}

func sortEntries(list []manifestEntry) {
	sort.Slice(list, func(i, j int) bool { return list[i].path < list[j].path })
}

// parseRootDrops decodes the pending_root_drops meta value
// ("gen|root1,root2") recorded by a previous manifest round.
func parseRootDrops(s string) (d struct {
	gen   string
	roots map[string]bool
}) {
	d.roots = make(map[string]bool)
	gen, rest, ok := strings.Cut(s, "|")
	if !ok {
		return d
	}
	d.gen = gen
	for _, r := range strings.Split(rest, ",") {
		if r != "" {
			d.roots[r] = true
		}
	}
	return d
}

// encodeRootDrops encodes the pending_root_drops meta value.
func encodeRootDrops(gen string, roots map[string]bool) string {
	if gen == "" || len(roots) == 0 {
		return ""
	}
	ss := make([]string, 0, len(roots))
	for r := range roots {
		ss = append(ss, r)
	}
	sort.Strings(ss)
	return gen + "|" + strings.Join(ss, ",")
}

// integritySweep incrementally verifies that database rows still have their
// files on disk and repairs missing ones. A bounded batch of rows is checked per
// round via the integrity_cursor meta key, so short-circuited rounds still
// make local-consistency progress without a full-tree walk.
func (mc *MetadataCrawler) integritySweep(ctx context.Context, db *sql.DB, mirrors []string) error {
	cursorStr, err := getMeta(ctx, db, metaIntegrityCursor)
	if err != nil {
		return err
	}
	cursor, _ := strconv.ParseInt(cursorStr, 10, 64)

	rows, err := db.QueryContext(ctx, "SELECT rowid, path, modified, time_base FROM files WHERE rowid > ? ORDER BY rowid LIMIT ?", cursor, integrityBatchSize)
	if err != nil {
		return err
	}
	type rowRef struct {
		id       int64
		path     string
		ts       int64
		timeBase string
	}
	var (
		batch    []rowRef
		lastRow  int64
		wrapped  bool
		affected bool
	)
	for rows.Next() {
		var r rowRef
		if err := rows.Scan(&r.id, &r.path, &r.ts, &r.timeBase); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, r)
		lastRow = r.id
		affected = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) < integrityBatchSize {
		wrapped = true // reached the end of the table; restart next round
	}

	var missing []rowRef
	for _, r := range batch {
		if _, err := mc.rootStat(r.path); err != nil {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		slog.Info("Local integrity sweep found missing files", "count", len(missing))
		globalStatus.setDownloadPlan(len(batch), len(missing))
		globalStatus.setPhase(PhaseDownloading)
		var (
			mux    sync.Mutex
			failed int
		)
		files := make(chan *MetadataFile, mc.workerCount()*2)
		writerDone := startMetadataFilesWriter(ctx, db, files)
		jobs := make(chan rowRef)
		go func() {
			defer close(jobs)
			for _, item := range missing {
				select {
				case jobs <- item:
				case <-ctx.Done():
					return
				}
			}
		}()
		var wg sync.WaitGroup
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					if ctx.Err() != nil {
						return
					}
					file, derr := mc.Download(ctx, item.path, downloadOpts{
						mirrors:    mirrors,
						modified:   item.ts,
						expectFile: true,
						timeBase:   item.timeBase,
					})
					if derr == nil {
						if file != nil {
							files <- file
						}
						globalStatus.incDownloaded()
						continue
					}
					if errors.Is(derr, fs.ErrNotExist) {
						slog.Warn("Keeping database row: file missing locally and temporarily unavailable on mirrors", "path", item.path)
					} else {
						slog.Error("Failed to repair missing file", "path", item.path, "error", derr)
					}
					globalStatus.incFailed()
					mux.Lock()
					failed++
					mux.Unlock()
				}
			}()
		}
		wg.Wait()
		close(files)
		if err := <-writerDone; err != nil {
			return err
		}
		if failed > 0 {
			return fmt.Errorf("%w: local integrity sweep failed to repair %d files", errManifestPending, failed)
		}
	}

	next := lastRow
	if wrapped || !affected {
		next = 0
	}
	return setMeta(ctx, db, metaIntegrityCursor, strconv.FormatInt(next, 10))
}

// syncCrawl implements the legacy HTML crawling sync, kept as the fallback
// path and for --force-crawl.
func (mc *MetadataCrawler) syncCrawl(ctx context.Context) error {
	globalStatus.setMode(ModeCrawl)
	globalStatus.setPhase(PhaseDownloading)
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
	if err := mc.reconcileTrash(ctx, db, ""); err != nil {
		return err
	}

	mc.sweepTempFiles(ctx)

	local, err := mc.localFiles(ctx, db)
	if err != nil {
		return err
	}

	selectedRoot := make(map[string]bool)
	for _, path := range mc.selectedPaths {
		selectedRoot[strings.TrimPrefix(path, "/")] = true
	}

	var remoteMap map[string]bool
	if mc.cleanup {
		remoteMap = make(map[string]bool)
	}
	crawlMirrors := mc.activeCrawlMirrors()

	// runPool processes download jobs with a fixed-size worker pool,
	// appending failures to *out.
	runPool := func(jobs <-chan failedEntry, out *[]failedEntry) error {
		var (
			mux sync.Mutex
			wg  sync.WaitGroup
		)
		// crawlRewrite records rows whose content was re-identified from
		// the HTTP observation and can now carry the HTTP time base.
		var crawlRewrites []*MetadataFile
		files := make(chan *MetadataFile, mc.workerCount()*2)
		writerDone := startMetadataFilesWriter(ctx, db, files)
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobs {
					if ctx.Err() != nil {
						return
					}
					file, derr := mc.Download(ctx, job.path, downloadOpts{
						mirrors:  crawlMirrors,
						timeBase: timeBaseHTTP,
						filterFn: func(newFile *MetadataFile) bool {
							mux.Lock()
							defer mux.Unlock()

							if remoteMap != nil {
								remoteMap[newFile.Path()] = true
							}
							if job.info == nil {
								return true
							}
							old := job.info
							if old.TimeBase() != timeBaseHTTP {
								// Symmetric to the manifest path: only a
								// strong-ETag+size match re-identifies the
								// row; otherwise download.
								if strongETagEqual(old.ETag(), newFile.ETag()) && old.Size() == newFile.Size() {
									crawlRewrites = append(crawlRewrites, newFile)
									return false
								}
								return true
							}
							return newFile.ModTime().Sub(old.ModTime()) > 0 && (newFile.Size() != old.Size() || newFile.ETag() != old.ETag())
						},
					})
					if derr == nil {
						if file != nil {
							files <- file
							globalStatus.incDownloaded()
						}
						continue
					}
					if errors.Is(derr, fs.ErrNotExist) {
						slog.Warn("Crawl entry is unavailable on all mirrors; keeping existing state", "path", job.path)
						mc.markIgnored(job.path)
						globalStatus.incUnavailable()
						if remoteMap != nil {
							mux.Lock()
							delete(remoteMap, job.path)
							mux.Unlock()
						}
						continue
					}
					slog.Error("Failed to download", "path", job.path, "error", derr)
					mux.Lock()
					if remoteMap != nil {
						// The crawl already listed this path, so a body failure
						// must not make cleanup treat the cached copy as stale.
						remoteMap[job.path] = true
					}
					*out = append(*out, job)
					mux.Unlock()
				}
			}()
		}
		wg.Wait()
		close(files)
		if err := <-writerDone; err != nil {
			return err
		}
		mux.Lock()
		rewrites := append([]*MetadataFile(nil), crawlRewrites...)
		mux.Unlock()
		if len(rewrites) > 0 {
			if err := rewriteHTTPBase(ctx, db, rewrites); err != nil {
				return err
			}
			slog.Info("Migrated verified metadata records to the HTTP time base", "count", len(rewrites))
		}
		return nil
	}

	// The walk feeds the worker pool directly so downloads overlap with
	// the (serial) directory crawl, with backpressure via the channel.
	var failed []failedEntry
	feed := make(chan failedEntry)
	poolDone := make(chan error, 1)
	go func() {
		poolDone <- runPool(feed, &failed)
	}()

	walkErr := mc.Walk(ctx, "/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			slog.Error("Error validating metadata file", "path", path, "error", err)
			return err
		}
		if info.IsDir() {
			ss := strings.Split(strings.TrimPrefix(path, "/"), "/")
			rootpath := ss[0]
			if path != "/" && !selectedRoot[rootpath] {
				slog.Info("Skipped directory", "path", path)
				return filepath.SkipDir
			}
			return nil
		}

		oldFile, err := pickFirstFile(ctx, db, path)
		if err != nil {
			return err
		}
		if oldFile != nil {
			_, err = mc.rootStat(oldFile.Path())
			if err != nil {
				slog.Warn("Missing file", "path", oldFile.Path())
				oldFile = nil
			}
		}

		select {
		case feed <- failedEntry{path: path, info: oldFile}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	close(feed)
	poolErr := <-poolDone
	if walkErr != nil {
		slog.Error("Critical error", "error", walkErr)
		return walkErr
	}
	if poolErr != nil {
		return poolErr
	}
	globalStatus.setFailed(len(failed))
	if ctx.Err() != nil {
		return ctx.Err()
	}

	for retry := 1; len(failed) > 0 && retry <= maxMetadataEntryRetryRounds; retry++ {
		slog.Info("Failed metadata entries will be retried...", "count", len(failed))
		globalStatus.setRetryRound(retry)
		var next []failedEntry
		jobs := make(chan failedEntry)
		go func() {
			defer close(jobs)
			for _, job := range failed {
				select {
				case jobs <- job:
				case <-ctx.Done():
					return
				}
			}
		}()
		if err := runPool(jobs, &next); err != nil {
			return err
		}
		failed = next
		globalStatus.setFailed(len(failed))
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if len(failed) > 0 {
		slog.Warn("Ignoring metadata entries after maximum retry attempts; continuing with the remaining files", "count", len(failed))
		for _, item := range failed {
			mc.markIgnored(item.path)
		}
		globalStatus.addIgnored(len(failed))
		globalStatus.setFailed(0)
	}

	if err := setMeta(ctx, db, metaTimeBase, timeBaseHTTP); err != nil {
		return err
	}

	if mc.cleanup {
		// Cleanup decisions must be based on the union of every distinct
		// crawl mirror: the main crawl lists only the first reachable
		// source, while a full rebuild inventories the union — a file
		// listed by just one source must never look vanished. The union
		// walks are pinned (no per-directory failover) and any source
		// failure skips cleanup entirely.
		if _, _, err := distinctMirrorPair(crawlMirrors); err != nil {
			slog.Error("Skipping metadata cleanup: fewer than two distinct crawl mirrors are available", "error", err)
			globalStatus.setCleanupGuard(true)
			return nil
		}
		seen := make(map[string]bool, len(crawlMirrors))
		for _, mirror := range crawlMirrors {
			key := normalizeMirrorKey(mirror)
			if seen[key] {
				continue
			}
			seen[key] = true
			walkErr := mc.walkPinned(ctx, mirror, "/", func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					ss := strings.Split(strings.TrimPrefix(p, "/"), "/")
					if p != "/" && !selectedRoot[ss[0]] {
						return filepath.SkipDir
					}
					return nil
				}
				remoteMap[p] = true
				return nil
			})
			if walkErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Error("Skipping metadata cleanup: an authoritative crawl source failed", "mirror", sanitizeURL(mirror), "error", walkErr)
				globalStatus.setCleanupGuard(true)
				return nil
			}
		}
		globalStatus.setPhase(PhaseCleanup)
		return mc.cleanupStale(ctx, db, local, remoteMap, nil)
	}
	return nil
}

func (mc *MetadataCrawler) LocalFiles() ([]*MetadataFile, error) {
	db, err := openMetadataDB(mc.downloadDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return mc.localFiles(context.Background(), db)
}

func (mc *MetadataCrawler) markIgnored(path string) {
	mc.mux.Lock()
	defer mc.mux.Unlock()
	if mc.ignoredPaths == nil {
		mc.ignoredPaths = make(map[string]bool)
	}
	mc.ignoredPaths[path] = true
}

func (mc *MetadataCrawler) localFiles(ctx context.Context, db *sql.DB) ([]*MetadataFile, error) {
	if err := createFileTable(ctx, db); err != nil {
		return nil, err
	}
	return listFiles(ctx, db)
}

// head performs a HEAD request against one mirror with bounded retries.
func (mc *MetadataCrawler) head(ctx context.Context, mirror, path string) (*MetadataFile, error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
	}
	u.Path = path

	var file *MetadataFile
	var lastErr error
	for range 3 {
		if ctx.Err() != nil {
			return nil, &fs.PathError{Op: "Head", Path: path, Err: ctx.Err()}
		}
		req, err := http.NewRequestWithContext(ctx, "HEAD", u.String(), nil)
		if err != nil {
			return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
		}
		setFetchHeaders(req.Header)

		resp, err := mc.client.Do(req)
		if err != nil {
			lastErr = err
			if err := sleepContext(ctx, backoffFor(err)); err != nil {
				return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				return nil, &fs.PathError{Op: "Head", Path: path, Err: fs.ErrNotExist}
			}
			lastErr = errors.New(resp.Status)
			resp.Body.Close()
			if err := sleepContext(ctx, 3*time.Second); err != nil {
				return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
			}
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		ss := strings.Split(contentType, ";")
		if len(ss) > 1 {
			contentType = strings.TrimSpace(ss[0])
		}
		if contentType == "text/html" {
			resp.Body.Close()
			file = &MetadataFile{
				path:  path,
				name:  filepath.Base(path),
				size:  128,
				isdir: true,
			}
		} else {
			size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
			timestamp, _ := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
			file = &MetadataFile{
				path:     path,
				name:     filepath.Base(path),
				size:     size,
				modified: timestamp.Unix(),
				etag:     resp.Header.Get("ETag"),
			}
		}
		resp.Body.Close()
		return file, nil
	}
	return nil, &fs.PathError{Op: "Head", Path: path, Err: lastErr}
}

// observeHead fetches the current remote identity of path, failing over
// across mirrors.
// observeHead returns the first successful HEAD observation across the
// mirrors. A 404 fails over like any other error: mirrors lag each other,
// so absence is only reported when every reachable mirror answers 404.
func (mc *MetadataCrawler) observeHead(ctx context.Context, mirrors []string, path string) (*MetadataFile, error) {
	var lastErr error
	sawNotFound := false
	for _, mirror := range mirrors {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		file, err := mc.head(ctx, mirror, path)
		if err == nil {
			return file, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			sawNotFound = true
			continue
		}
		lastErr = err
	}
	// A mix of 404 and transport errors never confirms absence.
	if sawNotFound && lastErr == nil {
		return nil, fs.ErrNotExist
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fs.ErrNotExist
}

// remoteMeta is an identity-encoding HEAD observation used by the full
// rebuild rounds. encoded records that the server ignored
// Accept-Encoding: identity, in which case Content-Length must not be
// trusted for reuse decisions.
type remoteMeta struct {
	size    int64
	hasSize bool
	etag    string
	modif   time.Time
	hasMod  bool
	encoded bool
}

// headIdentity performs a single HEAD with Accept-Encoding: identity
// against the given mirrors, failing over on transport errors and on 404:
// mirrors lag each other, so a path is reported as missing only when every
// reachable mirror answers 404 (a mix of 404 and transport errors stays a
// hard error — absence is never confirmed by a partial answer).
func (mc *MetadataCrawler) headIdentity(ctx context.Context, mirrors []string, path string) (*remoteMeta, error) {
	var lastErr error
	sawNotFound := false
	for _, mirror := range mirrors {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		u, err := url.Parse(mirror)
		if err != nil {
			lastErr = err
			continue
		}
		u.Path = path
		req, err := http.NewRequestWithContext(ctx, "HEAD", u.String(), nil)
		if err != nil {
			return nil, err
		}
		setFetchHeaders(req.Header)
		req.Header.Set("Accept-Encoding", "identity")

		resp, err := mc.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				sawNotFound = true
				continue
			}
			lastErr = errors.New(resp.Status)
			continue
		}
		meta := &remoteMeta{etag: resp.Header.Get("ETag")}
		if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
			meta.encoded = true
		}
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
				meta.size, meta.hasSize = n, true
			}
		}
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			if t, err := time.Parse(time.RFC1123, lm); err == nil && !t.IsZero() {
				meta.modif, meta.hasMod = t, true
			}
		}
		resp.Body.Close()
		return meta, nil
	}
	if sawNotFound && lastErr == nil {
		return nil, &fs.PathError{Op: "Head", Path: path, Err: fs.ErrNotExist}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &fs.PathError{Op: "Head", Path: path, Err: fs.ErrNotExist}
}

func (mc *MetadataCrawler) Stat(ctx context.Context, path string) (fi os.FileInfo, err error) {
	var file os.FileInfo
	for _, mirror := range mc.activeCrawlMirrors() {
		file, err = mc.head(ctx, mirror, path)
		if err != nil {
			continue
		}
		return file, nil
	}
	if err == nil {
		err = fs.ErrNotExist
	}
	return
}

// get fetches the HTML directory listing of path from exactly one mirror.
// listing is true only when the HTML carries a recognizable autoindex
// marker; generic 200 HTML pages must not become authoritative empty roots.
func (mc *MetadataCrawler) get(ctx context.Context, mirror, path string) (files []*MetadataFile, listing bool, err error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, false, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	u.Path = filepath.Join(u.Path, path)

	var resp *http.Response
	var lastErr error
	for attempt := range 3 {
		if ctx.Err() != nil {
			return nil, false, &fs.PathError{Op: "Get", Path: path, Err: ctx.Err()}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(u.String(), "/")+"/", nil)
		if err != nil {
			return nil, false, &fs.PathError{Op: "Get", Path: path, Err: err}
		}
		setNavigationHeaders(req.Header)

		resp, err = mc.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 2 {
				if serr := sleepContext(ctx, backoffFor(err)); serr != nil {
					return nil, false, &fs.PathError{Op: "Get", Path: path, Err: serr}
				}
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			status := errors.New(resp.Status)
			notFound := resp.StatusCode == http.StatusNotFound
			resp.Body.Close()
			if notFound {
				return nil, false, &fs.PathError{Op: "Get", Path: path, Err: fs.ErrNotExist}
			}
			lastErr = status
			if attempt < 2 {
				if serr := sleepContext(ctx, 3*time.Second); serr != nil {
					return nil, false, &fs.PathError{Op: "Get", Path: path, Err: serr}
				}
			}
			continue
		}
		break
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, false, &fs.PathError{Op: "Get", Path: path, Err: lastErr}
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	ss := strings.Split(contentType, ";")
	if len(ss) > 1 {
		contentType = strings.TrimSpace(ss[0])
	}
	if contentType != "text/html" {
		return nil, false, nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, false, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	for _, marker := range []string{doc.Find("title").First().Text(), doc.Find("h1").First().Text()} {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(marker)), "index of") {
			listing = true
			break
		}
	}
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if !ok {
			return
		}
		if href = strings.TrimSpace(href); href == ".." || href == "../" {
			listing = true
			return
		}
		link, err := url.Parse(href)
		if err != nil {
			return
		}
		href = u.ResolveReference(link).String()
		link, err = url.Parse(href)
		if err != nil {
			return
		}
		relPath, err := filepath.Rel(u.Path, link.Path)
		if err != nil {
			return
		}
		if len(link.Path) > 0 && link.Path[len(link.Path)-1] == '/' {
			relPath = relPath + "/"
		}
		name := filepath.Base(relPath)

		if isFilePath(relPath) {
			files = append(files, &MetadataFile{
				path: filepath.Join(path, name),
				name: name,
			})
			return
		} else if isDirPath(relPath) {
			name = strings.TrimSuffix(name, "/")
			files = append(files, &MetadataFile{
				path:  filepath.Join(path, name),
				name:  name,
				isdir: true,
			})
		}
	})
	return files, listing, nil
}

// getPinned fetches a listing from exactly one mirror and fails when the
// response is not a valid HTML listing. Full-inventory crawls must not
// silently accept non-HTML responses.
func (mc *MetadataCrawler) getPinned(ctx context.Context, mirror, path string) ([]*MetadataFile, error) {
	files, listing, err := mc.get(ctx, mirror, path)
	if err != nil {
		return nil, err
	}
	if !listing {
		return nil, fmt.Errorf("mirror %s returned an invalid directory listing for %s", sanitizeURL(mirror), path)
	}
	return files, nil
}

func (mc *MetadataCrawler) ReadDir(ctx context.Context, path string) (fileInfos []os.FileInfo, err error) {
	var files []*MetadataFile
	for _, mirror := range mc.activeCrawlMirrors() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		files, _, err = mc.get(ctx, mirror, path)
		if err != nil {
			continue
		}

		for _, file := range files {
			fileInfos = append(fileInfos, file)
		}
		return
	}
	if err == nil {
		err = fs.ErrNotExist
	}
	return
}

func (mc *MetadataCrawler) Walk(ctx context.Context, root string, fn WalkFunc) error {
	info, err := mc.Stat(ctx, root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = mc.walk(ctx, root, info, fn)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func (mc *MetadataCrawler) walk(ctx context.Context, path string, info os.FileInfo, walkFn WalkFunc) error {
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}

	fileInfos, err := mc.ReadDir(ctx, path)
	err1 := walkFn(path, info, err)
	// If err != nil, walk can't walk into this directory.
	// err1 != nil means walkFn want walk to skip this directory or stop walking.
	// Therefore, if one of err and err1 isn't nil, walk will return.
	if err != nil || err1 != nil {
		return err1
	}
	sort.Slice(fileInfos, func(i, j int) bool { return fileInfos[i].Name() < fileInfos[j].Name() })

	for _, fileInfo := range fileInfos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = mc.walk(ctx, filepath.Join(path, fileInfo.Name()), fileInfo, walkFn)
		if err != nil {
			if !fileInfo.IsDir() || err != filepath.SkipDir {
				return err
			}
		}
	}
	return nil
}

// walkPinned traverses the tree rooted at root using only the given mirror.
// There is no per-directory failover: any error on any directory fails the
// whole traversal, so a "dual source" inventory can never be a blend of two
// mirrors.
func (mc *MetadataCrawler) walkPinned(ctx context.Context, mirror, root string, fn WalkFunc) error {
	info, err := mc.StatPinned(ctx, mirror, root)
	if err != nil {
		return fn(root, nil, err)
	}
	err = mc.walkPinnedRec(ctx, mirror, root, info, fn)
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func (mc *MetadataCrawler) walkPinnedRec(ctx context.Context, mirror, path string, info os.FileInfo, walkFn WalkFunc) error {
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}
	fileInfos, err := mc.getPinned(ctx, mirror, path)
	if err != nil {
		return walkFn(path, info, err)
	}
	sort.Slice(fileInfos, func(i, j int) bool { return fileInfos[i].Name() < fileInfos[j].Name() })
	for _, fileInfo := range fileInfos {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = mc.walkPinnedRec(ctx, mirror, filepath.Join(path, fileInfo.Name()), fileInfo, walkFn)
		if err != nil {
			if !fileInfo.IsDir() || err != filepath.SkipDir {
				return err
			}
		}
	}
	return nil
}

// StatPinned HEADs path against one fixed mirror.
func (mc *MetadataCrawler) StatPinned(ctx context.Context, mirror, path string) (os.FileInfo, error) {
	return mc.head(ctx, mirror, path)
}

func (mc *MetadataCrawler) download(ctx context.Context, mirror, path string, o downloadOpts) (*MetadataFile, error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	u.Path = filepath.Join(u.Path, path)

	var resp *http.Response
	var lastErr error
	for attempt := range 3 {
		if ctx.Err() != nil {
			return nil, &fs.PathError{Op: "Get", Path: path, Err: ctx.Err()}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
		}
		setNavigationHeaders(req.Header)
		if o.identity {
			req.Header.Set("Accept-Encoding", "identity")
		}

		resp, err = mc.client.Do(req)
		if err != nil {
			slog.Warn("Error downloading", "mirror", sanitizeURL(mirror), "path", path, "error", err)
			lastErr = err
			if attempt < 2 {
				if serr := sleepContext(ctx, backoffFor(err)); serr != nil {
					return nil, &fs.PathError{Op: "Get", Path: path, Err: serr}
				}
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			status := errors.New(resp.Status)
			notFound := resp.StatusCode == http.StatusNotFound
			resp.Body.Close()
			if notFound {
				// 404 is definitive; do not retry this or other mirrors.
				return nil, &fs.PathError{Op: "Get", Path: path, Err: fs.ErrNotExist}
			}
			lastErr = status
			if attempt < 2 {
				if serr := sleepContext(ctx, 3*time.Second); serr != nil {
					return nil, &fs.PathError{Op: "Get", Path: path, Err: serr}
				}
			}
			continue
		}
		break
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: lastErr}
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	ss := strings.Split(contentType, ";")
	if len(ss) > 1 {
		contentType = strings.TrimSpace(ss[0])
	}
	if contentType == "text/html" {
		if o.expectFile {
			return nil, &fs.PathError{Op: "Get", Path: path, Err: errors.New("unexpected html document")}
		}
		// ignore html document
		return nil, nil
	}

	size := resp.ContentLength
	if size > maxMetadataFileBytes {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: fmt.Errorf("metadata file exceeds size limit: %d", size)}
	}
	f := &MetadataFile{
		path: path,
		name: filepath.Base(path),
		size: size,
		etag: resp.Header.Get("ETag"),
	}
	if o.modified != 0 {
		f.modified = o.modified
	} else {
		timestamp, _ := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
		f.modified = timestamp.Unix()
	}
	f.timeBase = o.timeBase

	if o.filterFn == nil || o.filterFn(f) {
		slog.Info("Downloading", "mirror", sanitizeURL(mirror), "path", path)
		if mc.fsRoot == nil {
			return nil, &fsError{errors.New("metadata filesystem root is not open")}
		}
		filePath := rootRel(f.Path())
		if err := mc.fsRoot.MkdirAll(filepath.Dir(filePath), dirPerm); err != nil && !os.IsExist(err) {
			return nil, &fsError{err}
		}

		// Atomic replacement: write to a sibling temp file first so an
		// interrupted download never leaves a partial file behind.
		tmpPath := tempPathFor(filePath)
		out, err := mc.fsRoot.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
		if err != nil {
			return nil, &fsError{err}
		}
		written, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxMetadataFileBytes+1))
		closeErr := out.Close()
		if copyErr != nil {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: copyErr}
		}
		if closeErr != nil {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fsError{closeErr}
		}
		if written > maxMetadataFileBytes {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: fmt.Errorf("metadata file exceeds size limit")}
		}
		// When the server honored identity encoding, Content-Length must
		// match the bytes written; when it compressed anyway, the decoded
		// byte count is authoritative.
		if size >= 0 && written != size {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: fmt.Errorf("content length mismatch: expected %d, got %d", size, written)}
		}
		if total := mc.roundBytes.Add(written); total > maxRoundDownloadBytes {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: fmt.Errorf("sync round exceeds download byte budget")}
		}
		f.size = written
		if err := renameReplaceCachePath(mc.fsRoot, tmpPath, filePath); err != nil {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fsError{err}
		}
		if f.modified > 0 {
			modTime := f.ModTime()
			if err := mc.fsRoot.Chtimes(filePath, modTime, modTime); err != nil {
				slog.Warn("Failed to set file modification time", "path", filePath, "error", err)
			}
		}
		f.finalizeIdentity()
		slog.Info("Downloaded", "path", path)
		return f, nil
	}

	slog.Info("Skipped", "path", f.Path())
	return nil, nil
}

// backoffFor maps a transport error to the retry backoff used by the
// download paths (network errors wait longer than protocol failures).
func backoffFor(err error) time.Duration {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) || errors.Is(urlErr.Err, io.EOF) {
			return 10 * time.Second
		}
	}
	return 3 * time.Second
}

// fsError marks a local filesystem failure (as opposed to a mirror/network
// failure), so that Download skips retrying the remaining mirrors.
type fsError struct{ err error }

func (e *fsError) Error() string { return e.err.Error() }
func (e *fsError) Unwrap() error { return e.err }

func (mc *MetadataCrawler) Download(ctx context.Context, path string, o downloadOpts) (*MetadataFile, error) {
	if len(o.mirrors) == 0 {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: errors.New("no metadata mirror available")}
	}
	var (
		allNotFound = true
		lastRealErr error
	)
	for i := range o.mirrors {
		mirror := o.mirrors[i]
		file, err := mc.download(ctx, mirror, path, o)
		if err == nil {
			return file, nil
		}
		var ferr *fsError
		if errors.As(err, &ferr) {
			slog.Error("Local filesystem error while downloading, skipping mirror retry", "path", path, "error", err)
			return nil, err
		}
		if errors.Is(err, fs.ErrNotExist) {
			// A single mirror returning 404 is not definitive (it may lag
			// behind the manifest or the other mirrors); fail over and only
			// report ErrNotExist when every mirror missed the file.
			if i < len(o.mirrors)-1 {
				slog.Warn("File not found on mirror, will try next mirror", "path", path, "mirror", sanitizeURL(mirror))
			}
			continue
		}
		allNotFound = false
		lastRealErr = err
		if i < len(o.mirrors)-1 {
			slog.Warn("Failed to download, will try next mirror", "path", path, "mirror", sanitizeURL(mirror), "error", err)
			continue
		}
	}
	if allNotFound {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: fs.ErrNotExist}
	}
	if lastRealErr != nil {
		return nil, lastRealErr
	}
	return nil, &fs.PathError{Op: "Get", Path: path, Err: errors.New("download failed")}
}

// MetadataFile is a file in metadata file server
type MetadataFile struct {
	path       string
	name       string
	size       int64
	modified   int64
	etag       string
	isdir      bool
	timeBase   string
	contentID  string
	provenance string
}

// finalizeIdentity derives the content identity of a freshly materialized
// file: a strong ETag pins the identity to etag+size; otherwise this
// materialization gets a unique ID that can never equal another row's.
func (f *MetadataFile) finalizeIdentity() {
	if cid := contentIDFor(f.etag, f.size); cid != "" {
		f.contentID, f.provenance = cid, provenanceETag
		return
	}
	f.contentID, f.provenance = newMaterializationID("dl"), provenanceDownloaded
}

// Path returns the full path of a file
func (f MetadataFile) Path() string {
	return f.path
}

// Name returns the name of a file
func (f MetadataFile) Name() string {
	return f.name
}

// Size returns the size of a file
func (f MetadataFile) Size() int64 {
	return f.size
}

func (f MetadataFile) IsDir() bool {
	return f.isdir
}

// Mode will return the mode of a given file
func (f MetadataFile) Mode() os.FileMode {
	if f.isdir {
		return dirPerm | os.ModeDir
	}
	return filePerm
}

// ModTime returns the modified time of a file
func (f MetadataFile) ModTime() time.Time {
	return time.Unix(f.modified, 0)
}

func (f MetadataFile) ETag() string {
	return f.etag
}

// TimeBase returns the per-row time base (manifest, http or unknown).
func (f MetadataFile) TimeBase() string {
	return f.timeBase
}

// ContentID returns the content identity (empty when absent).
func (f MetadataFile) ContentID() string {
	return f.contentID
}

// Provenance reports how the content identity was established.
func (f MetadataFile) Provenance() string {
	return f.provenance
}

// Sys ????
func (f MetadataFile) Sys() any {
	return nil
}

// String lets us see file information
func (f MetadataFile) String() string {
	if f.isdir {
		return fmt.Sprintf("drwxr-xr-x\t%d\t%v\t%s", f.size, f.ModTime(), f.path)
	}
	return fmt.Sprintf("-rw-r--r--\t%d\t%v\t%s", f.size, f.ModTime(), f.path)
}

// isStrongETag reports whether etag pins content identity: non-empty and
// not a weak validator.
func isStrongETag(etag string) bool {
	etag = strings.TrimSpace(etag)
	return etag != "" && !strings.HasPrefix(etag, "W/") && !strings.HasPrefix(etag, "w/")
}

// strongETagEqual reports whether two strong, non-empty ETags are equal.
// Weak or empty validators never establish equality.
func strongETagEqual(a, b string) bool {
	return isStrongETag(a) && isStrongETag(b) && strings.TrimSpace(a) == strings.TrimSpace(b)
}

// contentIDFor derives the stable content identity from a strong ETag and
// the byte size; weak/absent ETags yield no identity.
func contentIDFor(etag string, size int64) string {
	if !isStrongETag(etag) {
		return ""
	}
	return strings.TrimSpace(etag) + ":" + strconv.FormatInt(size, 10)
}

// newMaterializationID mints a unique identity for content whose bytes were
// accepted without a strong ETag (download or relaxed reuse).
func newMaterializationID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), ownTempSeq.Add(1))
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

// identityMatches reports whether an HTTP observation confirms that old row
// and the observed remote still describe identical content: equal size plus
// equal strong ETags. Anything less is insufficient evidence.
func identityMatches(old *MetadataFile, obs *MetadataFile) bool {
	if old == nil || obs == nil || obs.IsDir() {
		return false
	}
	return obs.Size() == old.Size() && strongETagEqual(obs.ETag(), old.ETag())
}

// fetchScanList downloads the manifest from the given mirrors, failing over
// to the next mirror on error. It returns the body together with the
// Last-Modified of the response that actually served it, so callers
// persist the generation of the parsed content rather than a probed
// timestamp of a different mirror, plus the serving mirror so consensus
// checks can exclude it canonically.
func fetchScanList(ctx context.Context, mirrors []string) (body0 []byte, lastMod0, source0 string, err0 error) {
	client := newBrowserClient(60 * time.Second)
	var lastErr error
	for _, mirror := range mirrors {
		if ctx.Err() != nil {
			return nil, "", "", ctx.Err()
		}
		u, err := url.Parse(mirror)
		if err != nil {
			lastErr = err
			continue
		}
		u.Path = scanListPath

		var (
			resp        *http.Response
			gotResponse bool
		)
		for range 3 {
			if ctx.Err() != nil {
				return nil, "", "", ctx.Err()
			}
			req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			if err != nil {
				lastErr = err
				gotResponse = false
				break
			}
			setNavigationHeaders(req.Header)

			resp, err = client.Do(req)
			if err != nil {
				lastErr = err
				if serr := sleepContext(ctx, backoffFor(err)); serr != nil {
					return nil, "", "", serr
				}
				continue
			}
			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				lastErr = fs.ErrNotExist
				break
			}
			if resp.StatusCode != http.StatusOK {
				lastErr = errors.New(resp.Status)
				resp.Body.Close()
				if serr := sleepContext(ctx, 3*time.Second); serr != nil {
					return nil, "", "", serr
				}
				continue
			}
			gotResponse = true
			break
		}
		if !gotResponse {
			lastErr = &fs.PathError{Op: "Get", Path: scanListPath, Err: lastErr}
			continue
		}

		lastModified := resp.Header.Get("Last-Modified")
		if lastModified == "" {
			resp.Body.Close()
			lastErr = fmt.Errorf("metadata manifest from %s has no Last-Modified header", sanitizeURL(mirror))
			continue
		}
		if _, err := time.Parse(time.RFC1123, lastModified); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("metadata manifest from %s has an invalid Last-Modified header: %q", sanitizeURL(mirror), lastModified)
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxScanListBytes))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(body)) >= maxScanListBytes {
			lastErr = fmt.Errorf("metadata manifest from %s exceeds the size limit", sanitizeURL(mirror))
			continue
		}
		if len(body) == 0 {
			lastErr = fmt.Errorf("empty metadata manifest from %s", sanitizeURL(mirror))
			continue
		}
		return body, lastModified, mirror, nil
	}
	return nil, "", "", lastErr
}

// probeScanList checks manifest availability with a HEAD request and returns
// the manifest Last-Modified and the request latency.
func probeScanList(ctx context.Context, mirror string) (string, time.Duration, bool) {
	u, err := url.Parse(mirror)
	if err != nil {
		return "", 0, false
	}
	u.Path = scanListPath

	req, err := http.NewRequestWithContext(ctx, "HEAD", u.String(), nil)
	if err != nil {
		return "", 0, false
	}
	setFetchHeaders(req.Header)

	client := newBrowserClient(3 * time.Second)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, false
	}
	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		return "", 0, false
	}
	parsed, err := time.Parse(time.RFC1123, lm)
	if err != nil {
		return "", 0, false
	}
	if parsed.After(time.Now().Add(maxManifestFutureSkew)) {
		return "", 0, false
	}
	return lm, time.Since(start), true
}

// walkScanList parses the gzipped manifest and emits selected entries one
// at a time. Full rebuilds use this callback form to stage rows directly in
// bounded SQLite transactions instead of retaining the whole library in
// memory. Every entry path is lexically normalized before use so traversal
// sequences such as "/电影/../.metadata.db" cannot escape their root, and
// the decompressed size is capped to reject gzip bombs.
func walkScanList(r io.Reader, selectedRoots map[string]bool, emit func(string, int64) error) (map[string]bool, int, int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, 0, err
	}
	defer gz.Close()

	counter := &countingReader{r: gz}
	topLevel := make(map[string]bool)
	malformed := 0
	selected := 0

	scanner := bufio.NewScanner(counter)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if counter.n > maxScanListDecompressedBytes {
			return nil, 0, 0, fmt.Errorf("metadata manifest exceeds the decompressed size limit (%d bytes)", maxScanListDecompressedBytes)
		}
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 || !strings.HasPrefix(parts[2], "/") {
			malformed++
			continue
		}
		t, err := time.Parse(manifestTimeLayout, parts[0]+" "+parts[1])
		if err != nil {
			malformed++
			continue
		}
		p := path.Clean(parts[2])
		if p == "/" || strings.ContainsRune(p, 0) || len(p) > maxManifestPathLen {
			malformed++
			continue
		}
		root := pathRoot(p)
		topLevel[root] = true
		if selectedRoots[root] {
			if selected >= maxManifestEntries {
				return nil, 0, 0, fmt.Errorf("metadata manifest exceeds the entry limit (%d)", maxManifestEntries)
			}
			if err := emit(p, t.Unix()); err != nil {
				return nil, 0, 0, err
			}
			selected++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, 0, err
	}
	return topLevel, malformed, selected, nil
}

// parseScanList keeps the map-returning API used by incremental sync and
// focused parser tests. Full rebuilds call walkScanList directly.
func parseScanList(r io.Reader, selectedRoots map[string]bool) (map[string]int64, map[string]bool, int, error) {
	entries := make(map[string]int64)
	topLevel, malformed, _, err := walkScanList(r, selectedRoots, func(p string, modified int64) error {
		entries[p] = modified
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	return entries, topLevel, malformed, nil
}

// countingReader counts the bytes read through r.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// pathRoot returns the first path segment of a remote-style absolute path.
func pathRoot(path string) string {
	ss := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// rewriteModified rewrites the stored timestamps of verified files to the
// manifest time base in a single transaction.
func rewriteModified(ctx context.Context, db *sql.DB, entries []manifestEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, "UPDATE files SET modified = ?, time_base = ? WHERE path = ?")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, e.ts, timeBaseManifest, e.path); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// rewriteHTTPBase rewrites rows re-identified from HTTP observations to the
// HTTP time base with the observed metadata.
func rewriteHTTPBase(ctx context.Context, db *sql.DB, files []*MetadataFile) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, "UPDATE files SET modified = ?, size = ?, etag = ?, time_base = ? WHERE path = ?")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, f := range files {
		if _, err := stmt.ExecContext(ctx, f.ModTime().Unix(), f.Size(), f.ETag(), timeBaseHTTP, f.Path()); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// underAnyRoot reports whether path lies under one of the given
// remote-style root paths.
func underAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(root, "/")
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// sanitizeURL strips userinfo, query and fragment from a mirror URL so the
// status page never leaks credentials that may be embedded in custom
// --mirror-url values.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// cleanupStale removes the local files (and their database records) whose
// paths are absent from remoteMap, skipping the retained roots. Safety
// guards abort deletion when either the whole library or a single root
// would lose a majority of its files in one round, which indicates a
// truncated or partial remote listing rather than genuine deletions.
//
// Deletion is crash-safe in two phases: files are first renamed into a
// .trash quarantine below downloadDir, the database transaction deleting
// their rows is committed, and only then is the quarantine emptied. A
// crash between rename and commit leaves self-healing state: rows whose
// files vanished are re-downloaded by the manifest diff (or the integrity
// sweep), and unlisted rows are simply re-attempted next round. Leftover
// quarantine content is swept by sweepTempFiles on the next round that
// processes downloads.
func (mc *MetadataCrawler) cleanupStale(ctx context.Context, db *sql.DB, local []*MetadataFile, remoteMap map[string]bool, retainedRoots []string) error {
	var toDelete []*MetadataFile
	for _, oldFile := range local {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if remoteMap[oldFile.Path()] {
			continue
		}
		if underAnyRoot(oldFile.Path(), retainedRoots) {
			continue
		}
		toDelete = append(toDelete, oldFile)
	}
	if len(toDelete) == 0 {
		return nil
	}
	if len(local) >= cleanupGuardMinFiles && len(toDelete)*2 > len(local) {
		slog.Error("Skipping metadata cleanup: more than half of the local files would be deleted, which suggests an incomplete metadata listing; if the removals are intentional, clean up manually", "to_delete", len(toDelete), "local", len(local))
		globalStatus.setCleanupGuard(true)
		return nil
	}

	// Per-root ratio guard: a partial manifest that silently drops one
	// root's entries must not delete that root's files.
	localPerRoot := make(map[string]int)
	for _, f := range local {
		localPerRoot[pathRoot(f.Path())]++
	}
	deletePerRoot := make(map[string]int)
	for _, f := range toDelete {
		deletePerRoot[pathRoot(f.Path())]++
	}
	guardedRoots := make(map[string]bool)
	for root, del := range deletePerRoot {
		if localPerRoot[root] >= perRootGuardMinFiles && del*2 > localPerRoot[root] {
			guardedRoots[root] = true
			slog.Error("Skipping metadata cleanup for root: more than half of the root's files would be deleted", "root", root, "to_delete", del, "local", localPerRoot[root])
		}
	}
	if len(guardedRoots) > 0 {
		globalStatus.setCleanupGuard(true)
		var kept []*MetadataFile
		for _, f := range toDelete {
			if !guardedRoots[pathRoot(f.Path())] {
				kept = append(kept, f)
			}
		}
		toDelete = kept
		if len(toDelete) == 0 {
			return nil
		}
	}

	if mc.fsRoot == nil {
		return errors.New("metadata filesystem root is not open")
	}
	return quarantineAndDelete(ctx, mc.fsRoot, db, toDelete)
}

// quarantineAndDelete moves the given files into the .trash quarantine,
// commits the row deletions and then empties the quarantine.
func quarantineAndDelete(ctx context.Context, root *os.Root, db *sql.DB, toDelete []*MetadataFile) error {
	type quarantined struct {
		fp    string
		trash string
	}
	var (
		renamed    []quarantined
		markDelete []string
	)
	for _, oldFile := range toDelete {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fp := rootRel(oldFile.Path())
		trashPath := filepath.Join(trashDirName, fp)
		if err := root.MkdirAll(filepath.Dir(trashPath), dirPerm); err != nil && !os.IsExist(err) {
			slog.Warn("Failed to prepare quarantine for stale metadata file", "path", oldFile.Path(), "error", err)
			continue
		}
		if err := renameReplaceCachePath(root, fp, trashPath); err != nil {
			if os.IsNotExist(err) {
				// File already gone (e.g. after an interrupted earlier
				// cleanup): the row can be deleted safely.
				markDelete = append(markDelete, oldFile.Path())
				continue
			}
			slog.Warn("Failed to quarantine stale metadata file", "path", oldFile.Path(), "error", err)
			continue
		}
		renamed = append(renamed, quarantined{fp: fp, trash: trashPath})
		markDelete = append(markDelete, oldFile.Path())
	}
	if len(markDelete) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, path := range markDelete {
		if _, err := tx.ExecContext(ctx, "DELETE FROM files WHERE path = ?", path); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	globalStatus.addDeleted(len(markDelete))

	// Database rows are gone; finish by emptying the quarantine and
	// pruning empty parent directories. Failures here are harmless: the
	// leftovers are swept on the next processing round.
	for _, q := range renamed {
		if err := root.RemoveAll(q.trash); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove quarantined file", "path", q.trash, "error", err)
			continue
		}
		deleteEmptyRootDirs(root, filepath.Dir(q.fp))
	}
	// Best effort: drop the now-empty quarantine tree. Anything left
	// (e.g. from an earlier interrupted cleanup) is swept next round.
	if err := root.RemoveAll(trashDirName); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to remove quarantine directory", "path", trashDirName, "error", err)
	}
	return nil
}

// openMetadataDB opens the sqlite database below dir using the rollback
// journal, which is safe on common NFS/SMB-backed media volumes. The
// single batched writer avoids the write-contention problem that originally
// motivated WAL.
func openMetadataDB(dir string) (*sql.DB, error) {
	dsn := filepath.Join(dir, ".metadata.db")
	// A path containing '?' cannot carry DSN parameters (the driver splits
	// on the first '?'), so fall back to the plain DSN for such paths.
	paramSets := []string{
		"?_busy_timeout=10000&_journal_mode=DELETE&_synchronous=FULL",
		"",
	}
	if strings.Contains(dsn, "?") {
		paramSets = []string{""}
	}
	var lastErr error
	for _, params := range paramSets {
		db, err := sql.Open("sqlite3", dsn+params)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(8)
		if err := db.Ping(); err != nil {
			db.Close()
			lastErr = err
			continue
		}
		return db, nil
	}
	return nil, lastErr
}

func isFilePath(path string) bool {
	name := filepath.Base(path)
	return name != "." && name != ".." && !strings.HasSuffix(path, "/")
}

func isDirPath(path string) bool {
	name := filepath.Base(path)
	return name != "." && name != ".." && strings.HasSuffix(path, "/")
}

func validateMirror(ctx context.Context, raw string) time.Duration {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return 0
	}
	setNavigationHeaders(req.Header)

	client := newBrowserClient(3 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0
	}
	if !strings.Contains(string(body), "每日更新") {
		return 0
	}
	return time.Since(start)
}

// filesTableColumns is the canonical column list of the files table; every
// SELECT/INSERT/writer names columns explicitly instead of relying on
// VALUES position.
const filesTableColumns = "path, name, size, modified, etag, time_base, content_id, provenance"

// filesSchemaVersion is committed only after all identity backfills finish.
// A crash after ALTER TABLE but before completion therefore resumes the
// idempotent batches on the next open.
const filesSchemaVersion = 1

// createFileTable creates (or migrates) the files table with the per-row
// identity columns. Migration is idempotent: missing columns are added via
// ALTER TABLE and old rows are backfilled — every legacy row becomes
// time_base='unknown' because the old global time base cannot be proven per
// row; rows with a strong ETag additionally get their etag+size identity.
func createFileTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS files (
		path TEXT PRIMARY KEY,
		name TEXT,
		size INTEGER,
		modified INTEGER,
		etag TEXT,
		time_base TEXT NOT NULL DEFAULT 'unknown',
		content_id TEXT NOT NULL DEFAULT '',
		provenance TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(files)")
	if err != nil {
		return err
	}
	type colDef struct {
		name string
		decl string
	}
	var cols []colDef
	for rows.Next() {
		var cid int
		var c colDef
		var notnull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &c.name, &c.decl, &notnull, &dfltValue, &pk); err != nil {
			rows.Close()
			return err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	present := make(map[string]string, len(cols))
	for _, c := range cols {
		present[c.name] = c.decl
	}
	added := false
	for _, add := range []struct {
		name string
		decl string
	}{
		{"time_base", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"content_id", "TEXT NOT NULL DEFAULT ''"},
		{"provenance", "TEXT NOT NULL DEFAULT ''"},
	} {
		if _, ok := present[add.name]; !ok {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE files ADD COLUMN %s %s", add.name, add.decl)); err != nil {
				return err
			}
			added = true
		}
	}
	var schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return err
	}
	if !added && schemaVersion >= filesSchemaVersion {
		return nil
	}
	// Migration is incomplete: backfill in bounded batches so the writer
	// lock is never held for a whole-table transaction. Legacy rows become
	// time_base='unknown' because the old global time base cannot be
	// proven per row; rows with a strong ETag additionally get their
	// etag+size identity.
	for {
		res, err := db.ExecContext(ctx,
			"UPDATE files SET time_base = 'unknown' WHERE rowid IN (SELECT rowid FROM files WHERE time_base IS NULL OR time_base = '' LIMIT ?)",
			dbMigrationBatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			break
		}
	}
	// Weak or empty validators leave the identity empty (never equal to
	// anything).
	for {
		res, err := db.ExecContext(ctx,
			"UPDATE files SET content_id = TRIM(etag) || ':' || size WHERE rowid IN (SELECT rowid FROM files WHERE (content_id IS NULL OR content_id = '') AND etag IS NOT NULL AND TRIM(etag) != '' AND TRIM(etag) NOT LIKE 'W/%' AND TRIM(etag) NOT LIKE 'w/%' LIMIT ?)",
			dbMigrationBatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			break
		}
	}
	if schemaVersion < filesSchemaVersion {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", filesSchemaVersion)); err != nil {
			return err
		}
	}
	return nil
}

// hasUnknownBaseRows reports whether any row still carries the unknown
// (migrated) time base. LIMIT 1 keeps the probe cheap while such rows
// exist; after convergence it degrades to one full scan per round.
func hasUnknownBaseRows(ctx context.Context, db *sql.DB) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, "SELECT 1 FROM files WHERE time_base = 'unknown' LIMIT 1").Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func createMetaTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT
	)`); err != nil {
		return err
	}
	return nil
}

// sqlExecuter is satisfied by both *sql.DB and *sql.Tx so meta helpers can
// run standalone or inside a transaction.
type sqlExecuter interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func getMeta(ctx context.Context, db sqlExecuter, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

func setMeta(ctx context.Context, db sqlExecuter, key, value string) error {
	_, err := db.ExecContext(ctx, "INSERT OR REPLACE INTO meta VALUES (?,?)", key, value)
	return err
}

func deleteMeta(ctx context.Context, db sqlExecuter, key string) error {
	_, err := db.ExecContext(ctx, "DELETE FROM meta WHERE key = ?", key)
	return err
}

const dbWriteBatchSize = 256

// dbMigrationBatch bounds the one-time schema backfill transactions.
const dbMigrationBatch = 5000

// startMetadataFilesWriter launches the single SQLite writer for a download
// round. The writer runs under the round context; once the round is
// canceled it switches to a detached, short-timeout finalize context so
// files that already completed their atomic rename still get their rows
// committed (bounded by the SQLite busy timeout), and anything that fails
// remains in the self-healing "file exists without a row" state repaired by
// later rounds.
func startMetadataFilesWriter(ctx context.Context, db *sql.DB, files chan *MetadataFile) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- writeMetadataFiles(ctx, db, files)
	}()
	return done
}

// writeMetadataFiles is the single SQLite writer for a download round. It
// drains the channel even after an error so download workers never deadlock;
// callers receive the first error after all workers stop.
func writeMetadataFiles(ctx context.Context, db *sql.DB, files <-chan *MetadataFile) error {
	batch := make([]*MetadataFile, 0, dbWriteBatchSize)
	var firstErr error
	// finalizeCtx replaces the round context after cancellation so the
	// remaining renamed-but-uncommitted batch can still land.
	var finalizeCtx context.Context
	var finalizeCancel context.CancelFunc
	defer func() {
		if finalizeCancel != nil {
			finalizeCancel()
		}
	}()
	writeCtx := func() context.Context {
		if ctx.Err() == nil {
			return ctx
		}
		if finalizeCtx == nil {
			finalizeCtx, finalizeCancel = context.WithTimeout(context.Background(), 30*time.Second)
		}
		return finalizeCtx
	}
	flush := func() {
		if len(batch) == 0 || firstErr != nil {
			batch = batch[:0]
			return
		}
		wctx := writeCtx()
		tx, err := db.BeginTx(wctx, nil)
		if err != nil {
			firstErr = err
			batch = batch[:0]
			return
		}
		stmt, err := tx.PrepareContext(wctx, "INSERT OR REPLACE INTO files ("+filesTableColumns+") VALUES (?,?,?,?,?,?,?,?)")
		if err != nil {
			tx.Rollback()
			firstErr = err
			batch = batch[:0]
			return
		}
		for _, file := range batch {
			if _, err := stmt.ExecContext(wctx, file.Path(), file.Name(), file.Size(), file.modified, file.ETag(), file.TimeBase(), file.ContentID(), file.Provenance()); err != nil {
				stmt.Close()
				tx.Rollback()
				firstErr = err
				batch = batch[:0]
				return
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			firstErr = err
		}
		batch = batch[:0]
	}
	for file := range files {
		if firstErr != nil {
			continue
		}
		batch = append(batch, file)
		if len(batch) == dbWriteBatchSize {
			flush()
		}
	}
	flush()
	return firstErr
}

// deleteFile removes the file below root from disk (and its empty parent
// directories) and deletes its database record.
func deleteFile(ctx context.Context, tx *sql.Tx, file *MetadataFile, root *os.Root) error {
	stmt, err := tx.PrepareContext(ctx, "DELETE FROM files WHERE path = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	if root != nil {
		fp := rootRel(file.Path())
		if err := root.Remove(fp); err != nil && !os.IsNotExist(err) {
			return err
		}
		deleteEmptyRootDirs(root, filepath.Dir(fp))
	}

	_, err = stmt.ExecContext(ctx, file.Path())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func listFiles(ctx context.Context, db *sql.DB) ([]*MetadataFile, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+filesTableColumns+" FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []*MetadataFile
	for rows.Next() {
		f := &MetadataFile{}
		if err := rows.Scan(&f.path, &f.name, &f.size, &f.modified, &f.etag, &f.timeBase, &f.contentID, &f.provenance); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

func pickFirstFile(ctx context.Context, db *sql.DB, path string) (*MetadataFile, error) {
	f := &MetadataFile{}
	err := db.QueryRowContext(ctx,
		"SELECT "+filesTableColumns+" FROM files WHERE path = ?",
		path,
	).Scan(&f.path, &f.name, &f.size, &f.modified, &f.etag, &f.timeBase, &f.contentID, &f.provenance)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

func defaultWorkers() int {
	cpus := runtime.NumCPU()
	if cpus > 8 {
		return 8
	}
	return cpus
}
