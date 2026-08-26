package engine

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
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
	// directly; see the meta time base handling in Sync.
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
	integrityBatchSize = 2000
	// manifestMigrationTolerance is the maximum distance from a whole hour
	// for a timestamp delta to be attributed to the timezone offset between
	// the manifest time base and HTTP Last-Modified times (minutes are
	// truncated, hence up to 60s of slack, plus a small margin).
	manifestMigrationTolerance = 90 * time.Second
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
)

// Metadata state keys stored in the meta table of .metadata.db.
const (
	metaTimeBase             = "time_base"
	metaScanListLastModified = "scan_list_last_modified"
	metaScanListHash         = "scan_list_hash"
	metaCoveredRoots         = "covered_roots"
	metaPendingRootDrops     = "pending_root_drops"
	metaPendingTimeBase      = "pending_time_base"
	metaIntegrityCursor      = "integrity_cursor"
	timeBaseManifest         = "manifest"
	timeBaseHTTP             = "http"
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

// errManifestPending reports a generation that cannot complete because one
// or more listed files are not yet available on any mirror. Daemon mode
// defers this error to the next scheduled run instead of retrying every five
// seconds indefinitely.
var errManifestPending = errors.New("manifest entries are not yet available")

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

func NewMetadataCrawler(downloadDir string, mirrors, selectedPaths, ignoredDirs, ignoredExtentions []string, cleanup, forceCrawl bool, workers int) (*MetadataCrawler, error) {
	mc := &MetadataCrawler{
		client:            newBrowserClient(60 * time.Second),
		downloadDir:       downloadDir,
		selectedPaths:     selectedPaths,
		ignoredDirs:       ignoredDirs,
		ignoredExtentions: ignoredExtentions,
		cleanup:           cleanup,
		forceCrawl:        forceCrawl,
		workers:           workers,
	}

	if len(mirrors) == 0 {
		mc.mirrors = sMirrors
	} else {
		mc.mirrors = mirrors
	}
	var err error
	for range 3 {
		if err = mc.validateMirrors(); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	if len(selectedPaths) == 0 {
		selectedPaths = sPaths
	} else {
		ss := make([]string, len(selectedPaths))
		copy(ss, selectedPaths)
		selectedPaths = selectedPaths[:0]
		for _, path := range ss {
			sss := strings.Split(strings.TrimPrefix(path, "/"), "/")
			for _, s := range sss {
				if s != "" {
					selectedPaths = append(selectedPaths, "/"+s)
					break
				}
			}
		}
	}
	mc.selectedPaths = selectedPaths
	if len(mc.ignoredDirs) == 0 {
		mc.ignoredDirs = sFolder
	}
	if len(mc.ignoredExtentions) == 0 {
		mc.ignoredExtentions = sExt
	}
	return mc, nil
}

func (mc *MetadataCrawler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute * 10)
	defer ticker.Stop()
LOOP:
	for {
		select {
		case <-ticker.C:
			if err := mc.validateMirrors(); err != nil {
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
func (mc *MetadataCrawler) validateMirrors() error {
	globalStatus.setPhase(PhaseProbing)
	slog.Info("Validating metadata mirrors...")
	probes := make([]mirrorProbe, len(mc.mirrors))
	var wg sync.WaitGroup
	for i, mirror := range mc.mirrors {
		wg.Add(1)
		go func(i int, mirror string) {
			defer wg.Done()
			probes[i] = mc.probeMirror(mirror)
		}(i, mirror)
	}
	wg.Wait()

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
				slog.Info("Validated metadata mirror", "mirror", p.mirror, "latency_ms", p.latency.Milliseconds(), "manifest_last_modified", p.lastMod)
				continue
			}
			slog.Info("Metadata mirror manifest is stale, crawl only", "mirror", p.mirror, "manifest_last_modified", p.lastMod)
			crawlPr = append(crawlPr, p)
			continue
		}
		crawlPr = append(crawlPr, p)
		slog.Info("Validated metadata mirror (crawl only)", "mirror", p.mirror, "latency_ms", p.latency.Milliseconds())
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
func (mc *MetadataCrawler) probeMirror(mirror string) mirrorProbe {
	if !mc.forceCrawl {
		for range 2 {
			if lm, d, ok := probeScanList(mirror); ok {
				return mirrorProbe{mirror: mirror, lastMod: lm, latency: d}
			}
		}
		slog.Warn("Metadata mirror has no usable manifest", "mirror", mirror)
	}
	if d := validateMirror(mirror); d > 0 {
		return mirrorProbe{mirror: mirror, latency: d}
	}
	slog.Warn("Invalid metadata mirror", "mirror", mirror)
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
// requested. Stale ".tmp" files are swept by the mode implementations, only
// when downloads will actually happen, so a no-op manifest run does not pay
// for a full tree walk.
func (mc *MetadataCrawler) Sync() error {
	globalStatus.setCleanupEnabled(mc.cleanup)
	if err := os.MkdirAll(mc.downloadDir, dirPerm); err != nil {
		return err
	}
	root, err := os.OpenRoot(mc.downloadDir)
	if err != nil {
		return err
	}
	mc.fsRoot = root
	mc.roundBytes.Store(0)
	defer func() {
		mc.fsRoot = nil
		root.Close()
	}()

	if !mc.forceCrawl {
		err := mc.syncManifest()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errNoManifest):
			slog.Warn("Metadata manifest unavailable, falling back to HTML crawling mode")
		default:
			return err
		}
	}
	return mc.syncCrawl()
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

// sweepTempFiles removes stale temporary files left over by interrupted
// downloads (owned exclusively by this program: names of the form
// "<target>.xiaoya-<rand>.tmp"), plus leftover quarantine content from an
// interrupted cleanup. Legitimate remote files ending in ".tmp" are never
// touched.
func (mc *MetadataCrawler) sweepTempFiles() {
	if mc.fsRoot == nil {
		return
	}
	for _, root := range mc.selectedPaths {
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
		if err != nil {
			slog.Warn("Failed to sweep temporary files", "path", root, "error", err)
		}
	}
}

// reconcileTrash resolves cleanup state left by a crash. A quarantined file
// whose DB row still exists was not committed as deleted and is restored;
// a file whose row is gone belongs to a committed deletion and is removed.
func (mc *MetadataCrawler) reconcileTrash(db *sql.DB) error {
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
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(trashDirName, name)
		if err != nil {
			return err
		}
		remotePath := "/" + filepath.ToSlash(rel)
		var exists int
		err = db.QueryRow("SELECT 1 FROM files WHERE path = ?", remotePath).Scan(&exists)
		switch {
		case err == nil:
			if _, statErr := mc.fsRoot.Stat(rel); statErr == nil {
				// A newer copy already exists at the original path.
				return mc.fsRoot.Remove(name)
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			if err := mc.fsRoot.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
				return err
			}
			if err := mc.fsRoot.Rename(name, rel); err != nil {
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
	if err != nil {
		restoreErr = err
	}
	if restoreErr != nil {
		return restoreErr
	}
	return mc.fsRoot.RemoveAll(trashDirName)
}

// ownTempSuffix and ownTempInfix identify this program's download temp
// files ("<infix>.xiaoya-<rand>.tmp").
const ownTempInfix = ".xiaoya-"

var ownTempSeq atomic.Uint64

func isOwnTempName(name string) bool {
	return strings.Contains(name, ownTempInfix) && strings.HasSuffix(name, ".tmp")
}

// tempPathFor returns a unique sibling temp path for filePath, so two
// workers downloading the same target never collide and the sweep can tell
// our temp files apart from genuine ".tmp" content.
func tempPathFor(filePath string) string {
	return fmt.Sprintf("%s%s%d-%d.tmp", filePath, ownTempInfix, os.Getpid(), ownTempSeq.Add(1))
}

// syncManifest implements the manifest-based sync: download /.scan.list.gz,
// diff it against the local database (with one-time time base migration for
// pre-existing records) and download only what changed.
//
// Success markers (scan_list_last_modified / scan_list_hash) are written
// last, only after downloads and cleanup completed, so an interrupted round
// is retried instead of being short-circuited forever.
func (mc *MetadataCrawler) syncManifest() error {
	globalStatus.setMode(ModeManifest)
	globalStatus.setPhase(PhaseManifest)
	db, err := openMetadataDB(mc.downloadDir)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := createMetaTable(db); err != nil {
		return err
	}
	if err := createFileTable(db); err != nil {
		return err
	}
	if err := mc.reconcileTrash(db); err != nil {
		return err
	}

	mirrors := mc.activeManifestMirrors()
	lastModified := mc.activeManifestLastModified()
	if len(mirrors) == 0 {
		return errNoManifest
	}

	timeBase, err := getMeta(db, metaTimeBase)
	if err != nil {
		return err
	}
	storedLastModified, err := getMeta(db, metaScanListLastModified)
	if err != nil {
		return err
	}
	storedHash, err := getMeta(db, metaScanListHash)
	if err != nil {
		return err
	}
	coveredRootsMeta, err := getMeta(db, metaCoveredRoots)
	if err != nil {
		return err
	}
	pendingDropsMeta, err := getMeta(db, metaPendingRootDrops)
	if err != nil {
		return err
	}

	// Short-circuit the whole download phase when the newest manifest
	// generation is exactly the one already processed. Exact equality is
	// required: a tolerance window could skip a genuinely regenerated
	// manifest.
	if timeBase == timeBaseManifest && storedLastModified != "" && lastModified != "" {
		storedT, err1 := time.Parse(time.RFC1123, storedLastModified)
		currentT, err2 := time.Parse(time.RFC1123, lastModified)
		if err1 == nil && err2 == nil && currentT.Equal(storedT) {
			slog.Info("Metadata manifest unchanged since last sync, skipping download phase", "last_modified", lastModified)
			if err := mc.integritySweep(db, mirrors); err != nil {
				return err
			}
			globalStatus.setManifest(lastModified, 0, true)
			return nil
		}
	}

	body, bodyLastModified, err := fetchScanList(mirrors)
	if err != nil {
		slog.Error("Failed to download metadata manifest", "error", err)
		return errNoManifest
	}
	bodyHash := fmt.Sprintf("%x", sha256.Sum256(body))
	cleanupAuthorized := mc.cleanup
	if cleanupAuthorized {
		if len(mirrors) < 2 {
			cleanupAuthorized = false
			slog.Error("Skipping metadata cleanup: fewer than two newest-generation mirrors are available")
			globalStatus.setCleanupGuard(true)
		} else {
			consensusBody, _, consensusErr := fetchScanList(mirrors[1:])
			if consensusErr != nil || sha256.Sum256(consensusBody) != sha256.Sum256(body) {
				cleanupAuthorized = false
				slog.Error("Skipping metadata cleanup: newest-generation mirrors do not agree on manifest content", "error", consensusErr)
				globalStatus.setCleanupGuard(true)
			}
		}
	}

	// The generation differs by timestamp but the content is identical
	// (mirrors regenerate at slightly different times): refresh the stored
	// generation marker after the integrity sweep and skip the diff.
	if timeBase == timeBaseManifest && storedHash != "" && storedHash == bodyHash {
		if err := mc.integritySweep(db, mirrors); err != nil {
			return err
		}
		slog.Info("Metadata manifest content unchanged, skipping diff", "last_modified", bodyLastModified)
		if err := setMeta(db, metaScanListLastModified, bodyLastModified); err != nil {
			return err
		}
		globalStatus.setManifest(bodyLastModified, 0, true)
		return nil
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

	local, err := mc.localFiles(db)
	if err != nil {
		return err
	}
	localMap := make(map[string]*MetadataFile, len(local))
	for _, file := range local {
		localMap[file.Path()] = file
	}

	// Migrate records written by crawl mode (HTTP Last-Modified time base)
	// to the manifest time base without re-downloading them. This runs on
	// the first manifest sync after a crawl-mode run and when the manifest
	// starts covering roots whose existing records are still on the HTTP
	// time base.
	globalMigration := timeBase != timeBaseManifest && len(local) > 0

	// remoteMap is only needed to protect files from cleanup; skip the
	// bookkeeping entirely when cleanup is disabled.
	var remoteMap map[string]bool
	if cleanupAuthorized {
		remoteMap = make(map[string]bool, len(entries))
	}
	var toDownload, toRewrite []manifestEntry
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
		migratePath := globalMigration || newlyCoveredRoots[pathRoot(path)]
		if migratePath {
			if old.ModTime().Unix() > 0 && nearHourOffset(ts-old.ModTime().Unix()) {
				if ts != old.ModTime().Unix() {
					toRewrite = append(toRewrite, manifestEntry{path: path, ts: ts})
				}
				continue
			}
		} else if ts <= old.ModTime().Unix() {
			continue
		}
		toDownload = append(toDownload, manifestEntry{path: path, ts: ts})
	}

	if len(toRewrite) > 0 {
		if err := rewriteModified(db, toRewrite); err != nil {
			return err
		}
		slog.Info("Migrated existing metadata records to the manifest time base", "count", len(toRewrite))
	}
	slog.Info("Metadata files to download", "count", len(toDownload), "total", len(entries))
	globalStatus.setDownloadPlan(len(entries), len(toDownload))
	globalStatus.setPhase(PhaseDownloading)

	if len(toDownload) > 0 {
		mc.sweepTempFiles()
	}

	// runRound downloads one round of entries with a fixed-size worker
	// pool and returns the entries that should be retried.
	runRound := func(list []manifestEntry) ([]manifestEntry, error) {
		var (
			mux  sync.Mutex
			next []manifestEntry
		)
		files := make(chan *MetadataFile, mc.workerCount()*2)
		writerDone := make(chan error, 1)
		go func() { writerDone <- writeMetadataFiles(db, files) }()
		jobs := make(chan manifestEntry)
		go func() {
			for _, item := range list {
				jobs <- item
			}
			close(jobs)
		}()

		var wg sync.WaitGroup
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					file, derr := mc.Download(item.path, downloadOpts{
						mirrors:    mirrors,
						modified:   item.ts,
						expectFile: true,
					})
					if derr != nil {
						if errors.Is(derr, fs.ErrNotExist) {
							slog.Error("Manifest entry missing on all mirrors", "path", item.path)
						} else {
							slog.Error("Failed to download", "path", item.path, "error", derr)
						}
						globalStatus.incFailed()
						mux.Lock()
						next = append(next, item)
						mux.Unlock()
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
			return next, err
		}
		return next, nil
	}

	pending := toDownload
	for retry := 0; len(pending) > 0; retry++ {
		if retry > 5 {
			slog.Error("Metadata download has exceeded the maximum retry attempts.")
			return fmt.Errorf("%w: maximum retry attempts exceeded", errManifestPending)
		}
		if retry > 0 {
			slog.Info("Failed metadata entries will be retried...", "count", len(pending))
			globalStatus.setRetryRound(retry)
			time.Sleep(time.Duration(1<<min(retry, 5)) * time.Second)
		}
		var roundErr error
		pending, roundErr = runRound(pending)
		if roundErr != nil {
			return roundErr
		}
	}

	if err := setMeta(db, metaTimeBase, timeBaseManifest); err != nil {
		return err
	}
	if err := setMeta(db, metaCoveredRoots, strings.Join(coveredRoots, ",")); err != nil {
		return err
	}
	if err := setMeta(db, metaPendingRootDrops, encodeRootDrops(bodyHash, nextPendingDrops)); err != nil {
		return err
	}

	if cleanupAuthorized {
		if malformed > 0 {
			// A partially parseable manifest must never drive deletions.
			slog.Error("Skipping metadata cleanup: the manifest contained malformed lines", "malformed", malformed)
		} else {
			globalStatus.setPhase(PhaseCleanup)
			if err := mc.cleanupStale(db, local, remoteMap, retainedRoots); err != nil {
				return err
			}
		}
	}

	// Success markers go last: an interrupted cleanup or download round
	// must be retried rather than short-circuited.
	if err := setMeta(db, metaScanListHash, bodyHash); err != nil {
		return err
	}
	return setMeta(db, metaScanListLastModified, bodyLastModified)
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
func (mc *MetadataCrawler) integritySweep(db *sql.DB, mirrors []string) error {
	cursorStr, err := getMeta(db, metaIntegrityCursor)
	if err != nil {
		return err
	}
	cursor, _ := strconv.ParseInt(cursorStr, 10, 64)

	rows, err := db.Query("SELECT rowid, path, modified FROM files WHERE rowid > ? ORDER BY rowid LIMIT ?", cursor, integrityBatchSize)
	if err != nil {
		return err
	}
	type rowRef struct {
		id   int64
		path string
		ts   int64
	}
	var (
		batch    []rowRef
		lastRow  int64
		wrapped  bool
		affected bool
	)
	for rows.Next() {
		var r rowRef
		if err := rows.Scan(&r.id, &r.path, &r.ts); err != nil {
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

	var missing []manifestEntry
	for _, r := range batch {
		if _, err := mc.rootStat(r.path); err != nil {
			missing = append(missing, manifestEntry{path: r.path, ts: r.ts})
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
		writerDone := make(chan error, 1)
		go func() { writerDone <- writeMetadataFiles(db, files) }()
		jobs := make(chan manifestEntry)
		go func() {
			for _, item := range missing {
				jobs <- item
			}
			close(jobs)
		}()
		var wg sync.WaitGroup
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for item := range jobs {
					file, derr := mc.Download(item.path, downloadOpts{
						mirrors:    mirrors,
						modified:   item.ts,
						expectFile: true,
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
	return setMeta(db, metaIntegrityCursor, strconv.FormatInt(next, 10))
}

// syncCrawl implements the legacy HTML crawling sync, kept as the fallback
// path and for --force-crawl.
func (mc *MetadataCrawler) syncCrawl() error {
	globalStatus.setMode(ModeCrawl)
	globalStatus.setPhase(PhaseDownloading)
	db, err := openMetadataDB(mc.downloadDir)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := createMetaTable(db); err != nil {
		return err
	}
	if err := createFileTable(db); err != nil {
		return err
	}
	if err := mc.reconcileTrash(db); err != nil {
		return err
	}

	mc.sweepTempFiles()

	local, err := mc.localFiles(db)
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
		files := make(chan *MetadataFile, mc.workerCount()*2)
		writerDone := make(chan error, 1)
		go func() { writerDone <- writeMetadataFiles(db, files) }()
		for range mc.workerCount() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobs {
					file, derr := mc.Download(job.path, downloadOpts{
						mirrors: crawlMirrors,
						filterFn: func(newFile *MetadataFile) bool {
							mux.Lock()
							defer mux.Unlock()

							if remoteMap != nil {
								remoteMap[newFile.Path()] = true
							}

							return job.info == nil || newFile.ModTime().Sub(job.info.ModTime()) > 0 && (newFile.Size() != job.info.Size() || newFile.ETag() != job.info.ETag())
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
						slog.Warn("Skipped to download as it appears to no longer exist on the mirror server", "path", job.path)
						if remoteMap != nil {
							mux.Lock()
							delete(remoteMap, job.path)
							mux.Unlock()
						}
						continue
					}
					slog.Error("Failed to download", "path", job.path, "error", derr)
					globalStatus.incFailed()
					mux.Lock()
					*out = append(*out, job)
					mux.Unlock()
				}
			}()
		}
		wg.Wait()
		close(files)
		return <-writerDone
	}

	// The walk feeds the worker pool directly so downloads overlap with
	// the (serial) directory crawl, with backpressure via the channel.
	var failed []failedEntry
	feed := make(chan failedEntry)
	poolDone := make(chan error, 1)
	go func() {
		poolDone <- runPool(feed, &failed)
	}()

	walkErr := mc.Walk("/", func(path string, info os.FileInfo, err error) error {
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

		oldFile, err := pickFirstFile(db, path)
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

		feed <- failedEntry{path: path, info: oldFile}
		return nil
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

	for retry := 0; len(failed) > 0; retry++ {
		if retry > 5 {
			slog.Error("Metadata download has exceeded the maximum retry attempts.")
			return fmt.Errorf("maximum retry attempts exceeded")
		}
		if retry > 0 {
			slog.Info("Failed metadata entries will be retried...", "count", len(failed))
		}
		var next []failedEntry
		jobs := make(chan failedEntry)
		go func() {
			for _, job := range failed {
				jobs <- job
			}
			close(jobs)
		}()
		if err := runPool(jobs, &next); err != nil {
			return err
		}
		failed = next
	}

	if err := setMeta(db, metaTimeBase, timeBaseHTTP); err != nil {
		return err
	}

	if mc.cleanup {
		globalStatus.setPhase(PhaseCleanup)
		return mc.cleanupStale(db, local, remoteMap, nil)
	}
	return nil
}

func (mc *MetadataCrawler) LocalFiles() ([]*MetadataFile, error) {
	db, err := openMetadataDB(mc.downloadDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	return mc.localFiles(db)
}

func (mc *MetadataCrawler) localFiles(db *sql.DB) ([]*MetadataFile, error) {
	if err := createFileTable(db); err != nil {
		return nil, err
	}
	return listFiles(db)
}

func (mc *MetadataCrawler) head(path, mirror string) (*MetadataFile, error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
	}
	u.Path = path

	req, err := http.NewRequest("HEAD", u.String(), nil)
	if err != nil {
		return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
	}
	setFetchHeaders(req.Header)

	var resp *http.Response
	for range 3 {
		resp, err = mc.client.Do(req)
		if err != nil {
			if err, ok := err.(*url.Error); ok {
				err := err.Err
				_, ok := err.(*net.OpError)
				if ok || err == io.EOF {
					time.Sleep(time.Second * 10)
					continue
				}
			}
			time.Sleep(time.Second * 3)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusNotFound {
				err = fs.ErrNotExist
			} else {
				err = errors.New(resp.Status)
			}
			time.Sleep(time.Second * 3)
			continue
		}
		break
	}
	if err != nil {
		return nil, &fs.PathError{Op: "Head", Path: path, Err: err}
	}

	contentType := resp.Header.Get("Content-Type")
	ss := strings.Split(contentType, ";")
	if len(ss) > 1 {
		contentType = strings.TrimSpace(ss[0])
	}
	if contentType == "text/html" {
		return &MetadataFile{
			path:  path,
			name:  filepath.Base(path),
			size:  128,
			isdir: true,
		}, nil
	}

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	timestamp, _ := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
	return &MetadataFile{
		path:     path,
		name:     filepath.Base(path),
		size:     size,
		modified: timestamp.Unix(),
		etag:     resp.Header.Get("ETag"),
	}, nil
}

func (mc *MetadataCrawler) Stat(path string) (fi os.FileInfo, err error) {
	var file *MetadataFile
	for _, mirror := range mc.activeCrawlMirrors() {
		file, err = mc.head(path, mirror)
		if err != nil {
			continue
		}
		return file, nil
	}
	return
}

func (mc *MetadataCrawler) get(path, mirror string) ([]*MetadataFile, error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	u.Path = filepath.Join(u.Path, path)

	req, err := http.NewRequest("GET", strings.TrimSuffix(u.String(), "/")+"/", nil)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	setNavigationHeaders(req.Header)

	var resp *http.Response
	for range 3 {
		resp, err = mc.client.Do(req)
		if err != nil {
			if err, ok := err.(*url.Error); ok {
				err := err.Err
				_, ok := err.(*net.OpError)
				if ok || err == io.EOF {
					time.Sleep(time.Second * 10)
					continue
				}
			}
			time.Sleep(time.Second * 3)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusNotFound {
				err = fs.ErrNotExist
			} else {
				err = errors.New(resp.Status)
			}
			time.Sleep(time.Second * 3)
			continue
		}
		break
	}
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: fmt.Errorf("invalid http status code %d", resp.StatusCode)}
	}

	contentType := resp.Header.Get("Content-Type")
	ss := strings.Split(contentType, ";")
	if len(ss) > 1 {
		contentType = strings.TrimSpace(ss[0])
	}
	if contentType != "text/html" {
		return nil, nil
	}

	var files []*MetadataFile
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if !ok {
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
	return files, nil
}

func (mc *MetadataCrawler) ReadDir(path string) (fileInfos []os.FileInfo, err error) {
	var files []*MetadataFile
	for _, mirror := range mc.activeCrawlMirrors() {
		files, err = mc.get(path, mirror)
		if err != nil {
			continue
		}

		for _, file := range files {
			fileInfos = append(fileInfos, file)
		}
		return
	}
	return
}

func (mc *MetadataCrawler) Walk(root string, fn WalkFunc) error {
	info, err := mc.Stat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = mc.walk(root, info, fn)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func (mc *MetadataCrawler) walk(path string, info os.FileInfo, walkFn WalkFunc) error {
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}

	fileInfos, err := mc.ReadDir(path)
	err1 := walkFn(path, info, err)
	// If err != nil, walk can't walk into this directory.
	// err1 != nil means walkFn want walk to skip this directory or stop walking.
	// Therefore, if one of err and err1 isn't nil, walk will return.
	if err != nil || err1 != nil {
		// The caller's behavior is controlled by the return value, which is decided
		// by walkFn. walkFn may ignore err and return nil.
		// If walkFn returns SkipDir or SkipAll, it will be handled by the caller.
		// So walk should return whatever walkFn returns.
		return err1
	}
	sort.Slice(fileInfos, func(i, j int) bool { return fileInfos[i].Name() < fileInfos[j].Name() })

	for _, fileInfo := range fileInfos {
		err = mc.walk(filepath.Join(path, fileInfo.Name()), fileInfo, walkFn)
		if err != nil {
			if !fileInfo.IsDir() || err != filepath.SkipDir {
				return err
			}
		}
	}
	return nil
}

func (mc *MetadataCrawler) download(mirror, path string, o downloadOpts) (*MetadataFile, error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	u.Path = filepath.Join(u.Path, path)

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	setNavigationHeaders(req.Header)

	var resp *http.Response
	for range 3 {
		resp, err = mc.client.Do(req)
		if err != nil {
			slog.Warn("Error downloading", "mirror", mirror, "path", path, "error", err)
			if err, ok := err.(*url.Error); ok {
				err := err.Err
				_, ok := err.(*net.OpError)
				if ok || err == io.EOF {
					time.Sleep(time.Second * 10)
					continue
				}
			}
			time.Sleep(time.Second * 3)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusNotFound {
				// 404 is definitive; do not retry this or other mirrors.
				return nil, &fs.PathError{Op: "Get", Path: path, Err: fs.ErrNotExist}
			}
			err = errors.New(resp.Status)
			time.Sleep(time.Second * 3)
			continue
		}
		break
	}
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}

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

	if o.filterFn == nil || o.filterFn(f) {
		slog.Info("Downloading", "mirror", mirror, "path", path)
		if mc.fsRoot == nil {
			return nil, &fsError{errors.New("metadata filesystem root is not open")}
		}
		filePath := rootRel(f.Path())
		if err := mc.fsRoot.MkdirAll(filepath.Dir(filePath), dirPerm); err != nil {
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
		if size >= 0 && written != size {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: fmt.Errorf("content length mismatch: expected %d, got %d", size, written)}
		}
		if total := mc.roundBytes.Add(written); total > maxRoundDownloadBytes {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fs.PathError{Op: "Get", Path: f.Path(), Err: fmt.Errorf("sync round exceeds download byte budget")}
		}
		f.size = written
		if err := mc.fsRoot.Rename(tmpPath, filePath); err != nil {
			mc.fsRoot.Remove(tmpPath)
			return nil, &fsError{err}
		}
		if f.modified > 0 {
			modTime := f.ModTime()
			if err := mc.fsRoot.Chtimes(filePath, modTime, modTime); err != nil {
				slog.Warn("Failed to set file modification time", "path", filePath, "error", err)
			}
		}
		slog.Info("Downloaded", "path", path)
		return f, nil
	}

	slog.Info("Skipped", "path", f.Path())
	return nil, nil
}

// fsError marks a local filesystem failure (as opposed to a mirror/network
// failure), so that Download skips retrying the remaining mirrors.
type fsError struct{ err error }

func (e *fsError) Error() string { return e.err.Error() }
func (e *fsError) Unwrap() error { return e.err }

func (mc *MetadataCrawler) Download(path string, o downloadOpts) (*MetadataFile, error) {
	if len(o.mirrors) == 0 {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: errors.New("no metadata mirror available")}
	}
	var (
		allNotFound = true
		lastRealErr error
	)
	for i := range o.mirrors {
		mirror := o.mirrors[i]
		file, err := mc.download(mirror, path, o)
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
				slog.Warn("File not found on mirror, will try next mirror", "path", path, "mirror", mirror)
			}
			continue
		}
		allNotFound = false
		lastRealErr = err
		if i < len(o.mirrors)-1 {
			slog.Warn("Failed to download, will try next mirror", "path", path, "mirror", mirror, "error", err)
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
	path     string
	name     string
	size     int64
	modified int64
	etag     string
	isdir    bool
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

// fetchScanList downloads the manifest from the given mirrors, failing over
// to the next mirror on error. It returns the body together with the
// Last-Modified of the response that actually served it, so callers
// persist the generation of the parsed content rather than a probed
// timestamp of a different mirror.
func fetchScanList(mirrors []string) ([]byte, string, error) {
	client := newBrowserClient(60 * time.Second)
	var lastErr error
	for _, mirror := range mirrors {
		u, err := url.Parse(mirror)
		if err != nil {
			lastErr = err
			continue
		}
		u.Path = scanListPath

		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			lastErr = err
			continue
		}
		setNavigationHeaders(req.Header)

		var resp *http.Response
		for range 3 {
			resp, err = client.Do(req)
			if err != nil {
				if err, ok := err.(*url.Error); ok {
					err := err.Err
					_, ok := err.(*net.OpError)
					if ok || err == io.EOF {
						time.Sleep(time.Second * 10)
						continue
					}
				}
				time.Sleep(time.Second * 3)
				continue
			}
			if resp.StatusCode == http.StatusNotFound {
				resp.Body.Close()
				err = fs.ErrNotExist
				break
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				err = errors.New(resp.Status)
				time.Sleep(time.Second * 3)
				continue
			}
			break
		}
		if err != nil {
			lastErr = &fs.PathError{Op: "Get", Path: scanListPath, Err: err}
			continue
		}

		lastModified := resp.Header.Get("Last-Modified")
		if lastModified == "" {
			resp.Body.Close()
			lastErr = fmt.Errorf("metadata manifest from %s has no Last-Modified header", mirror)
			continue
		}
		if _, err := time.Parse(time.RFC1123, lastModified); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("metadata manifest from %s has an invalid Last-Modified header: %q", mirror, lastModified)
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxScanListBytes))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if int64(len(body)) >= maxScanListBytes {
			lastErr = fmt.Errorf("metadata manifest from %s exceeds the size limit", mirror)
			continue
		}
		if len(body) == 0 {
			lastErr = fmt.Errorf("empty metadata manifest from %s", mirror)
			continue
		}
		return body, lastModified, nil
	}
	return nil, "", lastErr
}

// probeScanList checks manifest availability with a HEAD request and returns
// the manifest Last-Modified and the request latency.
func probeScanList(mirror string) (string, time.Duration, bool) {
	u, err := url.Parse(mirror)
	if err != nil {
		return "", 0, false
	}
	u.Path = scanListPath

	req, err := http.NewRequest("HEAD", u.String(), nil)
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
	if _, err := time.Parse(time.RFC1123, lm); err != nil {
		return "", 0, false
	}
	parsed, _ := time.Parse(time.RFC1123, lm)
	if parsed.After(time.Now().Add(maxManifestFutureSkew)) {
		return "", 0, false
	}
	return lm, time.Since(start), true
}

// parseScanList parses the gzipped manifest. It returns the entries under
// the selected roots, the set of all top-level directories present in the
// manifest and the number of malformed lines. Every entry path is
// lexically normalized before use so traversal sequences such as
// "/电影/../.metadata.db" cannot escape their root, and the decompressed
// size is capped to reject gzip bombs.
func parseScanList(r io.Reader, selectedRoots map[string]bool) (map[string]int64, map[string]bool, int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, nil, 0, err
	}
	defer gz.Close()

	counter := &countingReader{r: gz}
	entries := make(map[string]int64)
	topLevel := make(map[string]bool)
	malformed := 0

	scanner := bufio.NewScanner(counter)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if counter.n > maxScanListDecompressedBytes {
			return nil, nil, 0, fmt.Errorf("metadata manifest exceeds the decompressed size limit (%d bytes)", maxScanListDecompressedBytes)
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
			if len(entries) >= maxManifestEntries {
				return nil, nil, 0, fmt.Errorf("metadata manifest exceeds the entry limit (%d)", maxManifestEntries)
			}
			entries[p] = t.Unix()
		}
	}
	if err := scanner.Err(); err != nil {
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

// nearHourOffset reports whether delta (in seconds) is within
// manifestMigrationTolerance of a whole number of hours and no larger than a
// plausible timezone offset. Unchanged files differ between the manifest and
// HTTP time bases by exactly the generator's timezone offset (a whole number
// of hours, subject to minute truncation), so a matching delta implies the
// file is unchanged.
func nearHourOffset(delta int64) bool {
	if delta < 0 {
		delta = -delta
	}
	if delta > 26*3600 {
		return false
	}
	rem := delta % 3600
	if rem > 1800 {
		rem = 3600 - rem
	}
	return time.Duration(rem)*time.Second <= manifestMigrationTolerance
}

// rewriteModified rewrites the stored timestamps of unchanged files to the
// manifest time base in a single transaction.
func rewriteModified(db *sql.DB, entries []manifestEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("UPDATE files SET modified = ? WHERE path = ?")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if _, err := stmt.Exec(e.ts, e.path); err != nil {
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
func (mc *MetadataCrawler) cleanupStale(db *sql.DB, local []*MetadataFile, remoteMap map[string]bool, retainedRoots []string) error {
	var toDelete []*MetadataFile
	for _, oldFile := range local {
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
	type quarantined struct {
		fp    string
		trash string
	}
	var (
		renamed    []quarantined
		markDelete []string
	)
	for _, oldFile := range toDelete {
		fp := rootRel(oldFile.Path())
		trashPath := filepath.Join(trashDirName, fp)
		if err := mc.fsRoot.MkdirAll(filepath.Dir(trashPath), dirPerm); err != nil {
			slog.Warn("Failed to prepare quarantine for stale metadata file", "path", oldFile.Path(), "error", err)
			continue
		}
		if err := mc.fsRoot.Rename(fp, trashPath); err != nil {
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

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, path := range markDelete {
		if _, err := tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
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
		if err := mc.fsRoot.Remove(q.trash); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove quarantined file", "path", q.trash, "error", err)
			continue
		}
		deleteEmptyRootDirs(mc.fsRoot, filepath.Dir(q.fp))
	}
	// Best effort: drop the now-empty quarantine tree. Anything left
	// (e.g. from an earlier interrupted cleanup) is swept next round.
	if err := mc.fsRoot.RemoveAll(trashDirName); err != nil {
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

func validateMirror(url string) time.Duration {
	start := time.Now()

	req, err := http.NewRequest("GET", url, nil)
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

func createFileTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS files (
		path TEXT PRIMARY KEY,
		name TEXT,
		size INTEGER,
		modified INTEGER,
		etag TEXT
	)`); err != nil {
		return err
	}
	return nil
}

func createMetaTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT
	)`); err != nil {
		return err
	}
	return nil
}

func getMeta(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

func setMeta(db *sql.DB, key, value string) error {
	_, err := db.Exec("INSERT OR REPLACE INTO meta VALUES (?,?)", key, value)
	return err
}

func deleteMeta(db *sql.DB, key string) error {
	_, err := db.Exec("DELETE FROM meta WHERE key = ?", key)
	return err
}

const dbWriteBatchSize = 256

// writeMetadataFiles is the single SQLite writer for a download round. It
// drains the channel even after an error so download workers never deadlock;
// callers receive the first error after all workers stop.
func writeMetadataFiles(db *sql.DB, files <-chan *MetadataFile) error {
	batch := make([]*MetadataFile, 0, dbWriteBatchSize)
	var firstErr error
	flush := func() {
		if len(batch) == 0 || firstErr != nil {
			batch = batch[:0]
			return
		}
		tx, err := db.Begin()
		if err != nil {
			firstErr = err
			batch = batch[:0]
			return
		}
		stmt, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?,?,?,?,?)")
		if err != nil {
			tx.Rollback()
			firstErr = err
			batch = batch[:0]
			return
		}
		for _, file := range batch {
			if _, err := stmt.Exec(file.Path(), file.Name(), file.Size(), file.modified, file.ETag()); err != nil {
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
func deleteFile(tx *sql.Tx, file *MetadataFile, root *os.Root) error {
	stmt, err := tx.Prepare("DELETE FROM files WHERE path = ?")
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

	_, err = stmt.Exec(file.Path())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func listFiles(db *sql.DB) ([]*MetadataFile, error) {
	rows, err := db.Query("SELECT path, name, size, modified, etag FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []*MetadataFile
	for rows.Next() {
		f := &MetadataFile{}
		if err := rows.Scan(&f.path, &f.name, &f.size, &f.modified, &f.etag); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func pickFirstFile(db *sql.DB, path string) (*MetadataFile, error) {
	f := &MetadataFile{}
	err := db.QueryRow(
		"SELECT path, name, size, modified, etag FROM files WHERE path = ?",
		path,
	).Scan(&f.path, &f.name, &f.size, &f.modified, &f.etag)
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
