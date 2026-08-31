package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

const (
	defaultAlistEndpoint     = "http://xiaoya.host:5678"
	defaultAlistStrmRootPath = "/d"
)

// Config carries the startup-level (process lifetime) configuration from
// CLI flags and environment variables. Everything hot-updatable lives in
// SyncSettings; each round receives an immutable snapshot from the
// SettingsStore.
type Config struct {
	RunMode                     int
	RunAsDaemon                 bool
	RunCron                     string
	LogLevel                    string
	ListenAddr                  string
	MediaDir                    string
	DownloadDir                 string
	Cleanup                     bool
	Purge                       bool
	Help                        bool
	ForceCrawl                  bool
	DownloadWorkers             int
	MirrorURL                   []string
	AlistURL                    string
	AlistStrmRootPath           string
	AlistPathSkipVerify         []string
	AlistPathSkipVerifyFromFile string
	StrmPathSkipVerify          []string
	StrmPathSkipVerifyFromFile  string
	ControlToken                string
}

// expandFromFiles applies the two *-from-file flags once at startup; the
// effective (expanded) path lists then live in the settings snapshot and
// the source file paths are never re-read or modified by web edits.
func (cfg *Config) expandFromFiles() error {
	if cfg.AlistPathSkipVerifyFromFile != "" {
		p, err := os.ReadFile(cfg.AlistPathSkipVerifyFromFile)
		if err != nil {
			return fmt.Errorf("AlistPathSkipVerifyFromFile is invalid: %w", err)
		}
		ss := strings.SplitN(string(bytes.TrimSpace(p)), "\n", -1)
		for _, each := range ss {
			if each = strings.TrimSpace(each); each != "" {
				cfg.AlistPathSkipVerify = append(cfg.AlistPathSkipVerify, each)
			}
		}
	}
	if cfg.StrmPathSkipVerifyFromFile != "" {
		p, err := os.ReadFile(cfg.StrmPathSkipVerifyFromFile)
		if err != nil {
			return fmt.Errorf("StrmPathSkipVerifyFromFile is invalid: %w", err)
		}
		ss := strings.SplitN(string(bytes.TrimSpace(p)), "\n", -1)
		for _, each := range ss {
			if each = strings.TrimSpace(each); each != "" {
				cfg.StrmPathSkipVerify = append(cfg.StrmPathSkipVerify, each)
			}
		}
	}
	return nil
}

// baselineSettings derives the startup baseline from the CLI/environment
// values. Legacy mode values (including 0) are accepted verbatim; only the
// structural fields are validated. The returned settings are a deep copy —
// cfg is not mutated.
func (cfg *Config) baselineSettings() (SyncSettings, error) {
	s := SyncSettings{
		RunMode:             cfg.RunMode,
		RunCron:             cfg.RunCron,
		Cleanup:             cfg.Cleanup,
		Purge:               cfg.Purge,
		ForceCrawl:          cfg.ForceCrawl,
		DownloadWorkers:     cfg.DownloadWorkers,
		MirrorURL:           append([]string(nil), cfg.MirrorURL...),
		AlistURL:            cfg.AlistURL,
		AlistStrmRootPath:   cfg.AlistStrmRootPath,
		AlistPathSkipVerify: append([]string(nil), cfg.AlistPathSkipVerify...),
		StrmPathSkipVerify:  append([]string(nil), cfg.StrmPathSkipVerify...),
	}
	if err := validateSyncSettingsCommon(s); err != nil {
		return s, err
	}
	return s, nil
}

// Validate validates and canonicalizes the CLI configuration.
func (cfg *Config) Validate() (int, error) {
	// The token env fallback lives here (after flag parsing) so the secret
	// never becomes a pflag default — pflag prints defaults in --help.
	if cfg.ControlToken == "" {
		cfg.ControlToken = os.Getenv("CONTROL_TOKEN")
	}
	cfg.AlistURL = strings.TrimSuffix(cfg.AlistURL, "/") + "/"

	u, err := url.Parse(cfg.AlistURL)
	if err != nil {
		return 2, fmt.Errorf("invalid Alist url: %s", cfg.AlistURL)
	}
	if u.Path != "/" {
		return 2, fmt.Errorf("alist url must be root path: %s", cfg.AlistURL)
	}

	if err := cfg.expandFromFiles(); err != nil {
		return 2, err
	}
	if _, err := cfg.baselineSettings(); err != nil {
		return 2, err
	}
	return 0, nil
}

// pathContains reports whether child is parent itself or lies below it.
func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkMediaOverlap refuses destructive cache clearing whenever a sync
// deletion root and the media library overlap in either direction. Paths
// are resolved through symlinks first; resolution failures fail closed
// because a strict rebuild must never guess about destructive targets.
func checkMediaOverlap(mediaDir, downloadDir string, roots []string) error {
	absMedia, err := filepath.Abs(mediaDir)
	if err != nil {
		return fmt.Errorf("cannot resolve media dir %q for strict rebuild: %w", mediaDir, err)
	}
	absDL, err := filepath.Abs(downloadDir)
	if err != nil {
		return fmt.Errorf("cannot resolve download dir %q for strict rebuild: %w", downloadDir, err)
	}
	resolvedMedia, err := filepath.EvalSymlinks(absMedia)
	if err != nil {
		return fmt.Errorf("cannot resolve media dir %q for strict rebuild: %w", mediaDir, err)
	}
	resolvedDL, err := filepath.EvalSymlinks(absDL)
	if err != nil {
		return fmt.Errorf("cannot resolve download dir %q for strict rebuild: %w", downloadDir, err)
	}
	resolvedMedia = filepath.Clean(resolvedMedia)
	resolvedDL = filepath.Clean(resolvedDL)

	for _, root := range roots {
		name := strings.TrimPrefix(root, "/")
		target := filepath.Join(resolvedDL, name)
		if _, err := os.Lstat(target); err == nil {
			resolvedTarget, err := filepath.EvalSymlinks(target)
			if err != nil {
				return fmt.Errorf("cannot resolve strict rebuild root %q: %w", target, err)
			}
			target = resolvedTarget
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("cannot inspect strict rebuild root %q: %w", target, err)
		}
		target = filepath.Clean(target)
		if pathContains(target, resolvedMedia) || pathContains(resolvedMedia, target) {
			return fmt.Errorf("media dir %q overlaps strict rebuild root /%s below %q; configure separate directory trees", mediaDir, name, downloadDir)
		}
	}
	return nil
}

// Run wires the settings store, the optional status/control server and the
// controller (daemon) or a single round (non-daemon), then reports the
// exit code and error through the channels. Cancellation stops active work
// and waits for the status server to shut down.
func (cfg *Config) Run(ctx context.Context, ecodeCh chan<- int, errCh chan<- error) {
	baseline, err := cfg.baselineSettings()
	if err != nil {
		ecodeCh <- 2
		errCh <- err
		return
	}
	store := NewSettingsStore(baseline, cfg.DownloadDir)
	settings, revision := store.Snapshot()
	globalStatus.setConfig(revision, store.PersistError() != "")
	globalStatus.setSyncRoots(defaultSyncRoots())

	// Control mutations always require the bearer token; without one the
	// status/control interface is read-only regardless of listen address.
	readOnly := cfg.ControlToken == ""
	globalStatus.setControl(cfg.RunAsDaemon, readOnly)

	var ctrl *Controller
	if cfg.RunAsDaemon {
		slog.Info("Run as daemon in foreground...")
		ctrl = NewController(cfg, store)
	}

	var (
		statusDone   <-chan struct{}
		serverCancel context.CancelFunc
	)
	if cfg.ListenAddr != "" {
		installRingLogHandler()
		var serverCtx context.Context
		serverCtx, serverCancel = context.WithCancel(ctx)
		cp := newControlPlane(ctrl, store, cfg.ControlToken)
		statusDone, err = startStatusServer(serverCtx, cfg.ListenAddr, statusHTTPHandler(cp))
		if err != nil {
			serverCancel()
			ecodeCh <- 2
			errCh <- err
			return
		}
	}
	stopStatusServer := func() {
		if serverCancel != nil {
			serverCancel()
			<-statusDone
		}
	}

	if cfg.RunAsDaemon {
		ctrl.Start()
		<-ctx.Done()
		ctrl.Stop()
		stopStatusServer()
		ecodeCh <- 0
		errCh <- nil
		return
	}

	// Non-daemon: one round with the effective settings, then exit. Web
	// control was already reported as unavailable.
	outcome, roundErr := cfg.runSyncRound(ctx, settings, SyncTypeIncremental, TriggerStartup, revision)
	ecode := 0
	if roundErr != nil {
		ecode = 2
		if re := asRoundError(roundErr); re != nil {
			ecode = re.code
		}
		if outcome == OutcomeCanceled {
			roundErr = context.Canceled
		}
	}
	stopStatusServer()
	ecodeCh <- ecode
	errCh <- roundErr
}

// compareMetadata verifies strm targets against the Alist server and
// computes the set of download-cache paths that must be preserved in the
// media library.
//
// Verification is fail-closed: any Alist error other than an explicit
// not-found fails the whole phase — an incomplete listing never produces a
// deletion plan. Only confirmed-absent targets (explicit not-found, or a
// complete listing that lacks the file) are dropped from the preserve set.
func (cfg *Config) compareMetadata(ctx context.Context, s SyncSettings, files []*MetadataFile) (map[string]bool, error) {
	downloadRoot, err := os.OpenRoot(cfg.DownloadDir)
	if err != nil {
		return nil, err
	}
	defer downloadRoot.Close()
	strmMap := make(map[string]map[string]bool)
	fullMap := make(map[string]map[string]bool)
	for _, file := range files {
		fpath := file.Path()
		dir := filepath.Dir(fpath)
		fname := filepath.Base(fpath)
		m := fullMap[dir]
		if m == nil {
			m = make(map[string]bool)
		}
		m[fname] = true
		fullMap[dir] = m

		ext := filepath.Ext(fname)
		if ext == ".strm" {
			m := strmMap[dir]
			if m == nil {
				m = make(map[string]bool)
			}
			m[fname] = true
			strmMap[dir] = m
		}
	}

	var (
		validDirs int
	)
	rootDirMap := make(map[string]int)
	strmToSkip := make(map[string]bool)
	alistToScan := make(map[string]map[string]string)

	for path, strmsMap := range strmMap {
		if !s.Purge {
			rootDirMap[getRootDir(path, cfg.MediaDir)]++
			validDirs++
		}

	LOOP:
		for strm := range strmsMap {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fpath := filepath.Join(path, strm)

			for _, toSkip := range s.StrmPathSkipVerify {
				if strings.HasPrefix(fpath, toSkip) {
					continue LOOP
				}
			}

			p, err := downloadRoot.ReadFile(rootRel(fpath))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}

			target := strings.ReplaceAll(string(bytes.TrimSpace(p)), "%20", " ")
			if strings.HasPrefix(target, defaultAlistEndpoint) {
				relpath := "/" + strings.TrimPrefix(strings.TrimPrefix(target, defaultAlistEndpoint), "/")
				relUrl := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(relpath, "/"), defaultAlistStrmRootPath), "/")
				if u, err := url.ParseRequestURI(relUrl); err == nil {
					relUrl = u.Path
				}

				for _, toSkip := range s.AlistPathSkipVerify {
					if strings.HasPrefix(relUrl, toSkip) {
						continue LOOP
					}
				}

				alistdir := filepath.Dir(relUrl)
				alistfile := filepath.Base(relUrl)
				alistfiles := alistToScan[alistdir]
				if alistfiles == nil {
					alistfiles = make(map[string]string)
				}
				alistfiles[alistfile] = fpath
				alistToScan[alistdir] = alistfiles
			}
		}
	}

	if s.Purge {
		if err := cfg.verifyAlistTargets(ctx, s, alistToScan, strmToSkip, rootDirMap, &validDirs); err != nil {
			return nil, err
		}
	}

	filesToPreserve := make(map[string]bool)
	for dir, fs := range fullMap {
		for file := range fs {
			fpath := filepath.Join(dir, file)
			if ok := strmToSkip[fpath]; ok {
				continue
			}

			filesToPreserve[fpath] = true
		}
	}

	if p, err := json.MarshalIndent(rootDirMap, "", "  "); err == nil {
		slog.Info("Valid metadata directories", "dirs", string(p))
	}
	slog.Info("Valid metadata directories in total", "valid", validDirs, "total", len(strmMap))
	return filesToPreserve, nil
}

// verifyAlistTargets scans the required Alist directories with a fixed
// worker pool and fail-closed error handling.
func (cfg *Config) verifyAlistTargets(ctx context.Context, s SyncSettings, alistToScan map[string]map[string]string, strmToSkip map[string]bool, rootDirMap map[string]int, validDirs *int) error {
	if len(alistToScan) == 0 {
		return nil
	}
	client, err := NewAlistClient(s.AlistURL)
	if err != nil {
		return err
	}

	var (
		mux      sync.Mutex
		firstErr error
		fdirMap  = make(map[string]int)
		workers  = min(defaultWorkers(), len(alistToScan))
		jobs     = make(chan struct {
			path  string
			files map[string]string
		})
		wg sync.WaitGroup
	)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for path, files := range alistToScan {
			select {
			case jobs <- struct {
				path  string
				files map[string]string
			}{path, files}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				infos, err := client.ReadDir(ctx, job.path)
				mux.Lock()
				if err != nil {
					switch {
					case os.IsNotExist(err):
						// Explicit not-found: the stream folder is gone on
						// Alist, so its strm files are dropped from the
						// preserve set.
						for _, fpath := range job.files {
							strmToSkip[fpath] = true
						}
						slog.Warn("Absent stream folder on Alist", "path", job.path)
					default:
						// Any other failure is inconclusive: the whole
						// compare phase must fail without producing a
						// deletion plan.
						if firstErr == nil {
							firstErr = fmt.Errorf("cannot verify stream folder %s on Alist: %w", job.path, err)
						}
					}
					mux.Unlock()
					continue
				}
				m := make(map[string]bool, len(infos))
				for _, info := range infos {
					m[info.Name()] = true
				}
				for alistfile, fpath := range job.files {
					if m[alistfile] {
						fdirMap[filepath.Dir(fpath)]++
						continue
					}
					strmToSkip[fpath] = true
					slog.Warn("Absent stream on Alist", "path", filepath.Join(job.path, alistfile))
				}
				mux.Unlock()
			}
		}()
	}
	// Close jobs after the producer finishes so workers exit.
	go func() {
		<-producerDone
		close(jobs)
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstErr != nil {
		return firstErr
	}

	for fpath := range fdirMap {
		rootDirMap[getRootDir(fpath, cfg.MediaDir)]++
		*validDirs++
	}
	return nil
}

// prepareMetadataUpdate diffs the download cache against the media library
// database and computes which files must be copied. Content identity is
// decided per row: identical non-empty content IDs never re-copy; rows on
// different or unknown time bases copy conservatively; only equal, known
// bases may compare timestamps.
func (cfg *Config) prepareMetadataUpdate(ctx context.Context, s SyncSettings, filesToPreserve map[string]bool) (map[string]bool, error) {
	if err := os.MkdirAll(cfg.MediaDir, dirPerm); err != nil {
		return nil, err
	}

	localDB, err := openMetadataDB(cfg.MediaDir)
	if err != nil {
		return nil, err
	}
	defer localDB.Close()
	mediaRoot, err := os.OpenRoot(cfg.MediaDir)
	if err != nil {
		return nil, err
	}
	defer mediaRoot.Close()

	if err := createFileTable(ctx, localDB); err != nil {
		return nil, err
	}
	if err := createMetaTable(ctx, localDB); err != nil {
		return nil, err
	}

	// Read the entire local inventory first: the cursor must be fully
	// drained and closed before any write transaction runs against the
	// same rollback-journal database.
	localRows, err := listFiles(ctx, localDB)
	if err != nil {
		return nil, err
	}
	localMap := make(map[string]*MetadataFile, len(localRows))
	for _, f := range localRows {
		localMap[f.Path()] = f
	}
	for _, f := range localRows {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if ok := filesToPreserve[f.Path()]; ok {
			continue
		}
		tx, err := localDB.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		if err := deleteFile(ctx, tx, f, mediaRoot); err != nil {
			tx.Rollback()
			// Keep the row so the deletion is retried next round; the
			// combination of a missing file with a surviving row is
			// repaired by the existence check below.
			slog.Warn("Failed to delete stale media file record", "path", f.Path(), "error", err)
		}
	}

	remoteDB, err := openMetadataDB(cfg.DownloadDir)
	if err != nil {
		return nil, err
	}
	defer remoteDB.Close()
	if err := createMetaTable(ctx, remoteDB); err != nil {
		return nil, err
	}
	if err := createFileTable(ctx, remoteDB); err != nil {
		return nil, err
	}

	filesNeedUpdate := make(map[string]bool)
	for path := range filesToPreserve {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		remoteFile, err := pickFirstFile(ctx, remoteDB, path)
		if err != nil {
			return nil, err
		}
		if remoteFile == nil {
			continue
		}

		localFile, ok := localMap[path]
		if filepath.Ext(remoteFile.Name()) == ".strm" || !ok {
			filesNeedUpdate[remoteFile.Path()] = true
			continue
		}
		if _, err := mediaRoot.Stat(rootRel(path)); err != nil {
			// The media file is gone but its database row survived (e.g.
			// interrupted deletion or a lost volume): re-copy it.
			slog.Warn("Media file missing, scheduling re-copy", "path", path)
			filesNeedUpdate[remoteFile.Path()] = true
			continue
		}
		if remoteFile.ContentID() != "" && remoteFile.ContentID() == localFile.ContentID() {
			// Identical content identity: never re-copy.
			continue
		}
		if remoteFile.TimeBase() != localFile.TimeBase() ||
			remoteFile.TimeBase() == timeBaseUnknown || localFile.TimeBase() == timeBaseUnknown {
			// The ordering of timestamps across (or without) known time
			// bases is meaningless: copy conservatively.
			filesNeedUpdate[remoteFile.Path()] = true
			continue
		}
		if remoteFile.ModTime().After(localFile.ModTime()) {
			filesNeedUpdate[remoteFile.Path()] = true
		}
	}
	return filesNeedUpdate, nil
}

func (cfg *Config) syncMetadata(ctx context.Context, s SyncSettings, filesToUpdate map[string]bool) error {
	strmList, otherList := make(map[string]bool), make(map[string]bool)
	for fpath := range filesToUpdate {
		fname := filepath.Base(fpath)
		ext := filepath.Ext(fname)
		if ext == ".strm" {
			strmList[fpath] = true
		} else {
			otherList[fpath] = true
		}
	}

	slog.Info("Finalizing updates...")
	if err := os.MkdirAll(cfg.MediaDir, dirPerm); err != nil {
		return err
	}
	mediaRoot, err := os.OpenRoot(cfg.MediaDir)
	if err != nil {
		return err
	}
	defer mediaRoot.Close()
	downloadRoot, err := os.OpenRoot(cfg.DownloadDir)
	if err != nil {
		return err
	}
	defer downloadRoot.Close()

	o, err := url.Parse(s.AlistURL)
	if err != nil {
		return err
	}

	for strm := range strmList {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel := rootRel(strm)
		p, err := downloadRoot.ReadFile(rel)
		if err != nil {
			return err
		}

		target := strings.ReplaceAll(string(bytes.TrimSpace(p)), "%20", " ")

		if strings.HasPrefix(target, defaultAlistEndpoint) {
			relpath := "/" + strings.TrimPrefix(strings.TrimPrefix(target, defaultAlistEndpoint), "/")
			relUrl := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(relpath, "/"), defaultAlistStrmRootPath), "/")
			relUrl = "/" + strings.TrimPrefix(s.AlistStrmRootPath, "/") + "/" + strings.TrimPrefix(relUrl, "/")
			if u, err := url.ParseRequestURI(relUrl); err == nil {
				relUrl = u.Path
			}

			uu := &url.URL{Scheme: o.Scheme, Opaque: o.Opaque, User: o.User, Host: o.Host, Path: relUrl}
			target = uu.String()
		}

		if err := mediaRoot.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
			return err
		}
		// Atomic replacement so a crash or cancellation never leaves a
		// truncated strm file behind.
		if err := writeFileAtomicRoot(mediaRoot, rel, []byte(target+"\n"), filePerm); err != nil {
			return err
		}
	}

	localDB, err := openMetadataDB(cfg.MediaDir)
	if err != nil {
		return err
	}
	defer localDB.Close()

	remoteDB, err := openMetadataDB(cfg.DownloadDir)
	if err != nil {
		return err
	}
	defer remoteDB.Close()

	for file := range otherList {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		remoteFile, err := pickFirstFile(ctx, remoteDB, file)
		if err != nil {
			return err
		}
		if remoteFile == nil {
			continue
		}

		tx, err := localDB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		if err := copyFile(ctx, tx, remoteFile, mediaRoot, downloadRoot, rootRel(file)); err != nil {
			tx.Rollback()
			return err
		}
		tx.Rollback()
	}
	slog.Info("Done.")
	return nil
}

func (cfg *Config) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "xiaoya-emby",
		Short:   "Xiaoya utility for Emby",
		Long:    `Utility to maintain metadata files in xiaoya media library for Emby`,
		Version: Version,
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := SetLogLevel(cfg.LogLevel); err != nil {
				fmt.Fprintln(os.Stdout, err)
				os.Exit(2)
			}

			ecode, err := cfg.Validate()
			if err != nil {
				fmt.Fprintln(os.Stdout, err)
				os.Exit(ecode)
			}

			ecodeCh := make(chan int, 1)
			defer close(ecodeCh)

			errCh := make(chan error, 1)
			defer close(errCh)

			go cfg.Run(cmd.Context(), ecodeCh, errCh)

			ecode, err = <-ecodeCh, <-errCh
			if err != nil {
				fmt.Fprintln(os.Stdout, err)
				os.Exit(ecode)
			}
		},
	}
	var version bool
	cmd.Flags().IntVar(&cfg.RunMode, "mode", 7, "Run mode (4: update download cache, 2: reserved, 1: sync media library)")
	cmd.Flags().BoolVar(&cfg.RunAsDaemon, "daemon", true, "Run as daemon in foreground.")
	cmd.Flags().StringVar(&cfg.RunCron, "cron-expr", "0 0 * * *", "Cron expression as scheduled task. Must run as daemon.")
	cmd.Flags().StringVarP(&cfg.LogLevel, "log-level", "l", defaultLogLevel(), "Minimum log level (debug, info, warn, error). Env: LOG_LEVEL.")
	cmd.Flags().StringVar(&cfg.ListenAddr, "listen-addr", "127.0.0.1:9527", "Address for the status page (progress, logs and, when permitted, manual controls). Set to \"\" to disable.")
	cmd.Flags().StringVarP(&cfg.MediaDir, "media-dir", "d", "/media", "Media library directory maintained for Emby.")
	cmd.Flags().StringVarP(&cfg.DownloadDir, "download-dir", "D", "/download", "Download cache directory for metadata.")
	cmd.Flags().BoolVar(&cfg.Cleanup, "cleanup", false, "Cleanup downloaded metadata when file no longer exists on remote server.")
	cmd.Flags().BoolVarP(&cfg.Purge, "purge", "p", true, "Whether to purge useless file or directory when media is no longer available.")
	cmd.Flags().BoolVar(&cfg.ForceCrawl, "force-crawl", false, "Force HTML crawling mode instead of manifest-based metadata sync.")
	cmd.Flags().IntVar(&cfg.DownloadWorkers, "download-workers", 0, "Number of concurrent download workers. 0 means auto (min(CPU, 8)).")
	cmd.Flags().StringVar(&cfg.ControlToken, "control-token", "", "Bearer token protecting the control API. Env: CONTROL_TOKEN (used when the flag is unset). Without a token the web interface is read-only; serve behind TLS.")
	cmd.Flags().BoolVarP(&cfg.Help, "help", "h", false, "Print this message.")
	cmd.Flags().BoolVarP(&version, "version", "v", false, "Print software version.")
	cmd.Flags().StringSliceVarP(&cfg.MirrorURL, "mirror-url", "m", []string{}, "Specify the mirror URL to sync metadata from.")
	cmd.Flags().StringVarP(&cfg.AlistURL, "alist-url", "u", defaultAlistEndpoint, "Endpoint of xiaoya Alist. Change this value will result to url overide in strm file.")
	cmd.Flags().StringVarP(&cfg.AlistStrmRootPath, "alist-strm-root-path", "r", defaultAlistStrmRootPath, "Root path of strm files in xiaoya Alist.")
	cmd.Flags().StringSliceVar(&cfg.AlistPathSkipVerify, "alist-path-skip-verify", []string{}, "Specify the Alist path to skip verify files. For example: \"/🏷️我的115分享\".")
	cmd.Flags().StringVar(&cfg.AlistPathSkipVerifyFromFile, "alist-path-skip-verify-from-file", "", "A file contains a list of Alist path to skip verify.")
	cmd.Flags().StringSliceVar(&cfg.StrmPathSkipVerify, "strm-path-skip-verify", []string{}, "Specify the metadata path to skip verify strm files. For example: \"/115\".")
	cmd.Flags().StringVar(&cfg.StrmPathSkipVerifyFromFile, "strm-path-skip-verify-from-file", "", "A file contains a list of strm path to skip verify.")
	return cmd
}

// getRootDir get root dir name.
func getRootDir(path, scanDir string) string {
	path, _ = filepath.Abs(path)
	scanDir, _ = filepath.Abs(scanDir)
	path = strings.TrimPrefix(path, scanDir)
	ss := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	if ss[0] == "" && len(ss) > 1 {
		return ss[1]
	}
	return ss[0]
}

// copyFile copies rel from the download cache into the media tree via an
// atomic temp-file rename and records the row — including the remote
// content identity, timestamp and time base — in the media database.
func copyFile(ctx context.Context, tx *sql.Tx, file *MetadataFile, toRoot, fromRoot *os.Root, rel string) error {
	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR REPLACE INTO files ("+filesTableColumns+") VALUES (?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	if err := toRoot.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
		return err
	}

	tmp := tempPathFor(rel)
	toFile, err := toRoot.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, filePerm)
	if err != nil {
		return err
	}

	fromFile, err := fromRoot.Open(rel)
	if err != nil {
		toFile.Close()
		toRoot.Remove(tmp)
		return err
	}
	defer fromFile.Close()

	// The copy must produce exactly the recorded source size: a silently
	// truncated copy would later pass the content-ID check and never be
	// repaired.
	n, copyErr := copyBounded(ctx, toFile, fromFile)
	closeErr := toFile.Close()
	if copyErr != nil {
		toRoot.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		toRoot.Remove(tmp)
		return closeErr
	}
	if size := file.Size(); size >= 0 && n != size {
		toRoot.Remove(tmp)
		return fmt.Errorf("copied %s: wrote %d bytes, source row records %d", rel, n, size)
	}
	if err := renameReplaceFile(toRoot, tmp, rel); err != nil {
		toRoot.Remove(tmp)
		return err
	}
	if file.ModTime().Unix() > 0 {
		// Propagate the source timestamp so later comparisons on the same
		// time base see an identical copy.
		mt := file.ModTime()
		if err := toRoot.Chtimes(rel, mt, mt); err != nil {
			slog.Warn("Failed to set copied file modification time", "path", rel, "error", err)
		}
	}

	if _, err := stmt.ExecContext(ctx,
		file.Path(), file.Name(), file.Size(), file.ModTime().Unix(), file.ETag(),
		file.TimeBase(), file.ContentID(), file.Provenance()); err != nil {
		return err
	}
	return tx.Commit()
}

// copyBounded copies with periodic context checks so large media copies
// honor cancellation.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
