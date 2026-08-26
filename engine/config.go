package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	cron "github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
)

const (
	defaultAlistEndpoint     = "http://xiaoya.host:5678"
	defaultAlistStrmRootPath = "/d"
)

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

	alistClient *AlistClient
}

func (cfg *Config) Run(ecodeCh chan<- int, errCh chan<- error) {
	if cfg.alistClient == nil {
		cfg.alistClient, _ = NewAlistClient(cfg.AlistURL)
	}

	if cfg.ListenAddr != "" {
		installRingLogHandler()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startStatusServer(ctx, cfg.ListenAddr)
	}

	var (
		remote []*MetadataFile
		err    error
	)

	if cfg.RunAsDaemon {
		slog.Info("Run as daemon in foreground...")
	}

METADATA:
	if cfg.RunMode&4 == 4 {
		globalStatus.roundStart()
		remote, err = cfg.downloadMetadata()
		if err != nil {
			globalStatus.roundEnd(err)
			if cfg.RunAsDaemon {
				if errors.Is(err, errManifestPending) {
					slog.Warn("Manifest generation deferred until the next scheduled run", "error", err)
					cfg.waitUntilNextRun()
					goto METADATA
				}
				slog.Error("Critical error", "error", err)
				time.Sleep(time.Second * 5)
				goto METADATA
			}
			ecodeCh <- 2
			errCh <- err
			return
		}
		if cfg.RunMode&1 != 1 {
			globalStatus.roundEnd(nil)
		}
		slog.Info("Finished metadata download.")
	} else {
		crawler := &MetadataCrawler{downloadDir: cfg.DownloadDir}
		remote, err = crawler.LocalFiles()
		if err != nil {
			if cfg.RunAsDaemon {
				slog.Error("Critical error", "error", err)
				time.Sleep(time.Second * 5)
				goto METADATA
			}
			ecodeCh <- 2
			errCh <- err
			return
		}
		slog.Info("Skipped metadata download.")
	}

	if cfg.RunMode&1 != 1 {
		globalStatus.setPhase(PhaseIdle)
		ecodeCh <- 0
		errCh <- nil
		return
	}

COMPARE:
	globalStatus.setPhase(PhaseComparing)
	filesToPreserve, err := cfg.compareMetadata(remote)
	if err != nil {
		globalStatus.roundEnd(err)
		if cfg.RunAsDaemon {
			slog.Error("Critical error", "error", err)
			time.Sleep(time.Second * 5)
			goto COMPARE
		}
		ecodeCh <- 126
		errCh <- err
		return
	}
	slog.Info("Metadata files to sync", "count", len(filesToPreserve))

PREPARE:
	globalStatus.setPhase(PhasePreparing)
	filesNeedUpdate, err := cfg.prepareMetadataUpdate(filesToPreserve)
	if err != nil {
		globalStatus.roundEnd(err)
		if cfg.RunAsDaemon {
			slog.Error("Critical error", "error", err)
			time.Sleep(time.Second * 5)
			goto PREPARE
		}
		ecodeCh <- 127
		errCh <- err
		return
	}
	slog.Info("Files need to be updated", "count", len(filesNeedUpdate))

SYNC:
	globalStatus.setPhase(PhaseCopying)
	err = cfg.syncMetadata(filesNeedUpdate)
	if err != nil {
		globalStatus.roundEnd(err)
		if cfg.RunAsDaemon {
			slog.Error("Critical error", "error", err)
			time.Sleep(time.Second * 5)
			goto SYNC
		}
		ecodeCh <- 128
		errCh <- err
		return
	}
	// The media copy succeeded: it is now safe to commit the time base
	// migration planned by prepareMetadataUpdate.
	if ferr := finalizeMediaTimeBase(cfg.MediaDir); ferr != nil {
		slog.Error("Failed to finalize media time base migration", "error", ferr)
	}
	globalStatus.roundEnd(nil)

	if cfg.RunAsDaemon {
		cfg.waitUntilNextRun()
		goto METADATA
	}
	globalStatus.setPhase(PhaseIdle)
	ecodeCh <- 0
	errCh <- nil
}

func (cfg *Config) waitUntilNextRun() {
	sche, _ := cron.ParseStandard(cfg.RunCron)
	next := sche.Next(time.Now())
	d := time.Until(next)
	slog.Info("Next task will be started", "at", next.Format(time.RFC3339), "wait", d)
	globalStatus.setNextRun(&next)
	globalStatus.setPhase(PhaseSleeping)
	time.Sleep(d)
}

func (cfg *Config) downloadMetadata() ([]*MetadataFile, error) {
	slog.Info("Start metadata download...")
	crawler, err := NewMetadataCrawler(cfg.DownloadDir, cfg.MirrorURL, nil, nil, nil, cfg.Cleanup, cfg.ForceCrawl, cfg.DownloadWorkers)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go crawler.Run(ctx)

	if err = crawler.Sync(); err != nil {
		return nil, err
	}

	return crawler.LocalFiles()
}

func (cfg *Config) compareMetadata(files []*MetadataFile) (map[string]bool, error) {
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
		wg        sync.WaitGroup
		mux       sync.Mutex
		validDirs int
	)
	rootDirMap := make(map[string]int)
	strmToSkip := make(map[string]bool)
	alistToScan := make(map[string]map[string]string)
	workerChan := make(chan struct{}, defaultWorkers())

	for path, strmsMap := range strmMap {
		if !cfg.Purge {
			rootDirMap[getRootDir(path, cfg.MediaDir)]++
			validDirs++
		}

	LOOP:
		for strm := range strmsMap {
			fpath := filepath.Join(path, strm)

			for _, toSkip := range cfg.StrmPathSkipVerify {
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

			s := strings.ReplaceAll(string(bytes.TrimSpace(p)), "%20", " ")
			if strings.HasPrefix(s, defaultAlistEndpoint) {
				relpath := "/" + strings.TrimPrefix(strings.TrimPrefix(s, defaultAlistEndpoint), "/")
				relUrl := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(relpath, "/"), defaultAlistStrmRootPath), "/")
				u, err := url.ParseRequestURI(relUrl)
				if err == nil {
					relUrl = u.Path
				}

				for _, toSkip := range cfg.AlistPathSkipVerify {
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

	if cfg.Purge {
		fdirMap := make(map[string]int)
		for alistdir, alistfiles := range alistToScan {
			wg.Add(1)
			go func(alistpath string, alistfiles map[string]string) {
				defer wg.Done()

				workerChan <- struct{}{}
				defer func() { <-workerChan }()

				files, err := cfg.alistClient.ReadDir(alistpath)
				if err != nil {
					mux.Lock()
					defer mux.Unlock()

					for _, fpath := range alistfiles {
						strmToSkip[fpath] = true
					}

					if os.IsNotExist(err) {
						slog.Warn("Absent stream folder on Alist", "path", alistpath)
						return
					}

					slog.Error("Cannot verify stream folder on Alist", "path", alistpath, "error", err)
					return
				}

				mux.Lock()
				defer mux.Unlock()

				m := make(map[string]bool)
				for _, file := range files {
					alistfile := file.Name()
					m[alistfile] = true
				}

				for alistfile, fpath := range alistfiles {
					if m[alistfile] {
						fdirMap[filepath.Dir(fpath)]++
						continue
					}
					strmToSkip[fpath] = true
					slog.Warn("Absent stream on Alist", "path", filepath.Join(alistpath, alistfile))
				}
			}(alistdir, alistfiles)
		}

		wg.Wait()

		for fpath := range fdirMap {
			rootDirMap[getRootDir(fpath, cfg.MediaDir)]++
			validDirs++
		}
	}

	filesToPreserve := make(map[string]bool)
	for dir, files := range fullMap {
		for file := range files {
			fpath := filepath.Join(dir, file)
			if ok := strmToSkip[fpath]; ok {
				continue
			}

			filesToPreserve[fpath] = true
		}
	}

	p, err := json.MarshalIndent(rootDirMap, "", "  ")
	if err == nil {
		slog.Info("Valid metadata directories", "dirs", string(p))
	}
	slog.Info("Valid metadata directories in total", "valid", validDirs, "total", len(strmMap))
	return filesToPreserve, nil
}

func (cfg *Config) prepareMetadataUpdate(filesToPreserve map[string]bool) (map[string]bool, error) {
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

	if err := createFileTable(localDB); err != nil {
		return nil, err
	}
	if err := createMetaTable(localDB); err != nil {
		return nil, err
	}

	rows, err := localDB.Query("SELECT path, name, size, modified, etag FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	localMap := make(map[string]*MetadataFile)
	for rows.Next() {
		f := &MetadataFile{}
		if err := rows.Scan(&f.path, &f.name, &f.size, &f.modified, &f.etag); err != nil {
			return nil, err
		}
		localMap[f.Path()] = f

		if ok := filesToPreserve[f.Path()]; !ok {
			tx, err := localDB.Begin()
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()

			if err := deleteFile(tx, f, mediaRoot); err != nil {
				// Keep the row so the deletion is retried next round; the
				// combination of a missing file with a surviving row is
				// repaired by the existence check below.
				slog.Warn("Failed to delete stale media file record", "path", f.Path(), "error", err)
				tx.Rollback()
			}
		}
	}

	remoteDB, err := openMetadataDB(cfg.DownloadDir)
	if err != nil {
		return nil, err
	}
	defer remoteDB.Close()
	if err := createMetaTable(remoteDB); err != nil {
		return nil, err
	}

	// The two databases may record timestamps on different time bases
	// (manifest vs HTTP Last-Modified) when the download side switched
	// modes since the media side last synced. Detect that once so the
	// comparison below never orders timestamps across bases. A pending
	// migration (planned but not yet copied) also counts as a mismatch so
	// an interrupted copy is re-planned with the conservative rules.
	localBase, err := getMeta(localDB, metaTimeBase)
	if err != nil {
		return nil, err
	}
	remoteBase, err := getMeta(remoteDB, metaTimeBase)
	if err != nil {
		return nil, err
	}
	pendingBase, err := getMeta(localDB, metaPendingTimeBase)
	if err != nil {
		return nil, err
	}
	if localBase == "" {
		localBase = timeBaseHTTP
	}
	if remoteBase == "" {
		remoteBase = timeBaseHTTP
	}
	baseMismatch := localBase != remoteBase || pendingBase != ""

	filesNeedUpdate := make(map[string]bool)
	var realign []manifestEntry
	for path := range filesToPreserve {
		remoteFile, err := pickFirstFile(remoteDB, path)
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
		if remoteFile.Size() == localFile.Size() && remoteFile.ETag() == localFile.ETag() {
			// Identical content: never re-copy. Realign the stored
			// timestamp when the delta is a pure timezone offset so
			// future comparisons are on the same time base.
			delta := remoteFile.ModTime().Unix() - localFile.ModTime().Unix()
			if delta != 0 && nearHourOffset(delta) {
				realign = append(realign, manifestEntry{path: remoteFile.Path(), ts: remoteFile.ModTime().Unix()})
			}
			continue
		}
		// Content differs: update whenever the bases differ (the ordering
		// is meaningless across bases) or the remote copy is newer.
		if baseMismatch || remoteFile.ModTime().Sub(localFile.ModTime()) > 0 {
			filesNeedUpdate[remoteFile.Path()] = true
		}
	}
	if len(realign) > 0 {
		if err := rewriteModified(localDB, realign); err != nil {
			return nil, err
		}
		slog.Info("Aligned media library timestamps to the metadata time base", "count", len(realign))
	}
	if baseMismatch && remoteBase != localBase {
		// Record the planned migration; it is only committed after the
		// copy phase succeeds (see finalizeMediaTimeBase), so a crash
		// mid-copy leaves the conservative cross-base rules in force.
		if err := setMeta(localDB, metaPendingTimeBase, remoteBase); err != nil {
			return nil, err
		}
	}
	return filesNeedUpdate, nil
}

// finalizeMediaTimeBase commits the time base migration planned by
// prepareMetadataUpdate once the media copy has completed successfully.
func finalizeMediaTimeBase(mediaDir string) error {
	db, err := openMetadataDB(mediaDir)
	if err != nil {
		return err
	}
	defer db.Close()
	pending, err := getMeta(db, metaPendingTimeBase)
	if err != nil {
		return err
	}
	if pending == "" {
		return nil
	}
	if err := setMeta(db, metaTimeBase, pending); err != nil {
		return err
	}
	if err := deleteMeta(db, metaPendingTimeBase); err != nil {
		return err
	}
	slog.Info("Finalized media library time base migration", "time_base", pending)
	return nil
}

func (cfg *Config) syncMetadata(filesToUpdate map[string]bool) error {
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

	o, err := url.Parse(cfg.AlistURL)
	if err != nil {
		return err
	}

	for strm := range strmList {
		rel := rootRel(strm)
		p, err := downloadRoot.ReadFile(rel)
		if err != nil {
			return err
		}

		s := strings.ReplaceAll(string(bytes.TrimSpace(p)), "%20", " ")

		if strings.HasPrefix(s, defaultAlistEndpoint) {
			relpath := "/" + strings.TrimPrefix(strings.TrimPrefix(s, defaultAlistEndpoint), "/")
			relUrl := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(relpath, "/"), defaultAlistStrmRootPath), "/")
			relUrl = "/" + strings.TrimPrefix(cfg.AlistStrmRootPath, "/") + "/" + strings.TrimPrefix(relUrl, "/")
			u, err := url.ParseRequestURI(relUrl)
			if err == nil {
				relUrl = u.Path
			}

			uu := &url.URL{Scheme: o.Scheme, Opaque: o.Opaque, User: o.User, Host: o.Host, Path: relUrl}
			s = uu.String()
		}

		if err := mediaRoot.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
			return err
		}
		if err := mediaRoot.WriteFile(rel, []byte(s+"\n"), filePerm); err != nil {
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
		remoteFile, err := pickFirstFile(remoteDB, file)
		if err != nil {
			return err
		}
		if remoteFile == nil {
			continue
		}

		tx, err := localDB.Begin()
		if err != nil {
			return err
		}

		if err := copyFile(tx, remoteFile, mediaRoot, downloadRoot, rootRel(file)); err != nil {
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

			go cfg.Run(ecodeCh, errCh)

			ecode, err = <-ecodeCh, <-errCh
			if err != nil {
				fmt.Fprintln(os.Stdout, err)
				os.Exit(ecode)
			}
		},
	}
	var version bool
	cmd.Flags().IntVar(&cfg.RunMode, "mode", 7, "Run mode (4: scan metadata, 2: preserved bit, 1: sync metadata)")
	cmd.Flags().BoolVar(&cfg.RunAsDaemon, "daemon", true, "Run as daemon in foreground.")
	cmd.Flags().StringVar(&cfg.RunCron, "cron-expr", "0 0 * * *", "Cron expression as scheduled task. Must run as daemon.")
	cmd.Flags().StringVarP(&cfg.LogLevel, "log-level", "l", defaultLogLevel(), "Minimum log level (debug, info, warn, error). Env: LOG_LEVEL.")
	cmd.Flags().StringVar(&cfg.ListenAddr, "listen-addr", "127.0.0.1:9527", "Address for the read-only status page (progress and logs). Set to \"\" to disable; expose beyond localhost only on trusted networks.")
	cmd.Flags().StringVarP(&cfg.MediaDir, "media-dir", "d", "/media", "Media directory of Emby to maintain metadata.")
	cmd.Flags().StringVarP(&cfg.DownloadDir, "download-dir", "D", "/download", "Media directory of Emby to download metadata to.")
	cmd.Flags().BoolVar(&cfg.Cleanup, "cleanup", false, "Cleanup downloaded metadata when file no longer exists on remote server.")
	cmd.Flags().BoolVarP(&cfg.Purge, "purge", "p", true, "Whether to purge useless file or directory when media is no longer available.")
	cmd.Flags().BoolVar(&cfg.ForceCrawl, "force-crawl", false, "Force HTML crawling mode instead of manifest-based metadata sync.")
	cmd.Flags().IntVar(&cfg.DownloadWorkers, "download-workers", 0, "Number of concurrent download workers. 0 means auto (min(CPU, 8)).")
	cmd.Flags().BoolVarP(&cfg.Help, "help", "h", false, "Print this message.")
	cmd.Flags().BoolVarP(&version, "version", "v", false, "Print software version.")
	cmd.Flags().StringSliceVarP(&cfg.MirrorURL, "mirror-url", "m", nil, "Specify the mirror URL to sync metadata from.")
	cmd.Flags().StringVarP(&cfg.AlistURL, "alist-url", "u", defaultAlistEndpoint, "Endpoint of xiaoya Alist. Change this value will result to url overide in strm file.")
	cmd.Flags().StringVarP(&cfg.AlistStrmRootPath, "alist-strm-root-path", "r", defaultAlistStrmRootPath, "Root path of strm files in xiaoya Alist.")
	cmd.Flags().StringSliceVar(&cfg.AlistPathSkipVerify, "alist-path-skip-verify", nil, "Specify the Alist path to skip verify files. For example: \"/🏷️我的115分享\".")
	cmd.Flags().StringVar(&cfg.AlistPathSkipVerifyFromFile, "alist-path-skip-verify-from-file", "", "A file contains a list of Alist path to skip verify.")
	cmd.Flags().StringSliceVar(&cfg.StrmPathSkipVerify, "strm-path-skip-verify", nil, "Specify the metadata path to skip verify strm files. For example: \"/115\".")
	cmd.Flags().StringVar(&cfg.StrmPathSkipVerifyFromFile, "strm-path-skip-verify-from-file", "", "A file contains a list of strm path to skip verify.")
	return cmd
}

func (cfg *Config) Validate() (int, error) {
	cfg.AlistURL = strings.TrimSuffix(cfg.AlistURL, "/") + "/"

	u, err := url.Parse(cfg.AlistURL)
	if err != nil {
		return 2, fmt.Errorf("invalid Alist url: %s", cfg.AlistURL)
	}
	if u.Path != "/" {
		return 2, fmt.Errorf("alist url must be root path: %s", cfg.AlistURL)
	}

	_, err = cron.ParseStandard(cfg.RunCron)
	if err != nil {
		return 2, fmt.Errorf("invalid cron expression: %s", cfg.RunCron)
	}

	if cfg.DownloadWorkers < 0 || cfg.DownloadWorkers > 64 {
		return 2, fmt.Errorf("invalid download workers %d (valid: 0-64, 0 means auto)", cfg.DownloadWorkers)
	}

	if cfg.AlistPathSkipVerifyFromFile != "" {
		p, err := os.ReadFile(cfg.AlistPathSkipVerifyFromFile)
		if err != nil {
			return 2, fmt.Errorf("AlistPathSkipVerifyFromFile is invalid: %v", err)
		}
		ss := strings.SplitN(string(bytes.TrimSpace(p)), "\n", -1)
		for _, each := range ss {
			if each == "" {
				continue
			}
			cfg.AlistPathSkipVerify = append(cfg.AlistPathSkipVerify, strings.TrimSpace(each))
		}
	}
	if len(cfg.AlistPathSkipVerify) > 0 {
		var ss []string
		for _, each := range cfg.AlistPathSkipVerify {
			each = strings.TrimSpace(each)
			if each == "/" || each == "" {
				continue
			}
			each = "/" + strings.TrimPrefix(each, "/")
			each = strings.TrimSuffix(each, "/") + "/"
			ss = append(ss, each)
		}
		cfg.AlistPathSkipVerify = ss
	}
	if cfg.StrmPathSkipVerifyFromFile != "" {
		p, err := os.ReadFile(cfg.StrmPathSkipVerifyFromFile)
		if err != nil {
			return 2, fmt.Errorf("StrmPathSkipVerifyFromFile is invalid: %v", err)
		}
		ss := strings.SplitN(string(bytes.TrimSpace(p)), "\n", -1)
		for _, each := range ss {
			if each == "" {
				continue
			}
			cfg.StrmPathSkipVerify = append(cfg.StrmPathSkipVerify, strings.TrimSpace(each))
		}
	}
	if len(cfg.StrmPathSkipVerify) > 0 {
		var ss []string
		for _, each := range cfg.StrmPathSkipVerify {
			each = strings.TrimSpace(each)
			if each == "/" || each == "" {
				continue
			}
			each = "/" + strings.TrimPrefix(each, "/")
			each = strings.TrimSuffix(each, "/") + "/"
			ss = append(ss, each)
		}
		cfg.StrmPathSkipVerify = ss
	}
	return 0, nil
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

func copyFile(tx *sql.Tx, file *MetadataFile, toRoot, fromRoot *os.Root, rel string) error {
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?,?,?,?,?)")
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

	_, copyErr := io.Copy(toFile, fromFile)
	closeErr := toFile.Close()
	if copyErr != nil {
		toRoot.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		toRoot.Remove(tmp)
		return closeErr
	}
	if err := toRoot.Rename(tmp, rel); err != nil {
		toRoot.Remove(tmp)
		return err
	}

	_, err = stmt.Exec(file.Path(), file.Name(), file.Size(), file.modified, file.ETag())
	if err != nil {
		return err
	}
	return tx.Commit()
}
