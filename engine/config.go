package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	GlobalUserAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/96.0.4664.110 Safari/537.36"
	defaultAlistEndpoint     = "http://xiaoya.host:5678"
	defaultAlistStrmRootPath = "/d"
)

type Config struct {
	RunMode             int
	RunAsDaemon         bool
	RunCron             string
	MediaDir            string
	DownloadDir         string
	Purge               bool
	Help                bool
	MirrorURL           []string
	AlistURL            string
	AlistStrmRootPath   string
	AlistPathSkipVerify []string
	StrmPathSkipVerify  []string

	alistClient *AlistClient
}

func (cfg *Config) Run(ecodeCh chan<- int, errCh chan<- error) {
	if cfg.alistClient == nil {
		cfg.alistClient, _ = NewAlistClient(cfg.AlistURL)
	}

	var (
		remote []*MetadataFile
		err    error
	)

	if cfg.RunAsDaemon {
		log.Println("[INFO] Run as daemon in foreground...")
	}

METADATA:
	if cfg.RunMode&4 == 4 {
		remote, err = cfg.downloadMetadata()
		if err != nil {
			if cfg.RunAsDaemon {
				log.Printf("[ERROR] Critical error: %v", err)
				time.Sleep(time.Second * 5)
				goto METADATA
			}
			ecodeCh <- 2
			errCh <- err
			return
		}
		log.Println("[INFO] Finished metadata download.")
	} else {
		crawler := &MetadataCrawler{downloadDir: cfg.DownloadDir}
		remote, err = crawler.LocalFiles()
		if err != nil {
			if cfg.RunAsDaemon {
				log.Printf("[ERROR] Critical error: %v", err)
				time.Sleep(time.Second * 5)
				goto METADATA
			}
			ecodeCh <- 2
			errCh <- err
			return
		}
		log.Println("[INFO] Skipped metadata download.")
	}

	if cfg.RunMode&1 != 1 {
		ecodeCh <- 0
		errCh <- nil
		return
	}

COMPARE:
	filesToPreserve, err := cfg.compareMetadata(remote)
	if err != nil {
		if cfg.RunAsDaemon {
			log.Printf("[ERROR] Critical error: %v", err)
			time.Sleep(time.Second * 5)
			goto COMPARE
		}
		ecodeCh <- 126
		errCh <- err
		return
	}
	log.Printf("[INFO] %d metadata files to sync.", len(filesToPreserve))

PREPARE:
	filesNeedUpdate, err := cfg.prepareMetadataUpdate(filesToPreserve)
	if err != nil {
		if cfg.RunAsDaemon {
			log.Printf("[ERROR] Critical error: %v", err)
			time.Sleep(time.Second * 5)
			goto PREPARE
		}
		ecodeCh <- 127
		errCh <- err
		return
	}
	log.Printf("[INFO] %d files need to be updated.", len(filesNeedUpdate))

SYNC:
	err = cfg.syncMetadata(filesNeedUpdate)
	if err != nil {
		if cfg.RunAsDaemon {
			log.Printf("[ERROR] Critical error: %v", err)
			time.Sleep(time.Second * 5)
			goto SYNC
		}
		ecodeCh <- 128
		errCh <- err
	}

	if cfg.RunAsDaemon {
		sche, _ := cron.ParseStandard(cfg.RunCron)
		next := sche.Next(time.Now())
		d := time.Until(next)
		log.Printf("[INFO] Next task will be started at: %s. Waiting for %v...", next.Format(time.RFC3339), d)
		time.Sleep(d)
		goto METADATA
	}
	ecodeCh <- 0
	errCh <- nil
}

func (cfg *Config) downloadMetadata() ([]*MetadataFile, error) {
	log.Println("[INFO] Start metadata download...")
	crawler, err := NewMetadataCrawler(cfg.DownloadDir, cfg.MirrorURL, nil, nil, nil, cfg.Purge)
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

			p, err := os.ReadFile(filepath.Join(cfg.DownloadDir, fpath))
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
						log.Printf("[WARN] Absent stream folder [%s] on Alist.", alistpath)
						return
					}

					log.Printf("[ERROR] Cannot verify stream folder [%s] on Alist: %v", alistpath, err)
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
					log.Printf("[WARN] Absent stream [%s] on Alist.", filepath.Join(alistpath, alistfile))
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
		log.Printf("[INFO] Valid metadata directories =>\n%s\n", p)
	}
	log.Printf("[INFO] %d/%d valid metadata directories in total.\n", validDirs, len(strmMap))
	return filesToPreserve, nil
}

func (cfg *Config) prepareMetadataUpdate(filesToPreserve map[string]bool) (map[string]bool, error) {
	if err := os.MkdirAll(cfg.MediaDir, dirPerm); err != nil {
		return nil, err
	}

	localDB, err := sql.Open("sqlite3", filepath.Join(cfg.MediaDir, ".metadata.db"))
	if err != nil {
		return nil, err
	}
	defer localDB.Close()

	if err := createFileTable(localDB); err != nil {
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

			deleteFile(tx, f)
			deleteDirIfEmpty(filepath.Dir(f.Path()))
		}
	}

	remoteDB, err := sql.Open("sqlite3", filepath.Join(cfg.DownloadDir, ".metadata.db"))
	if err != nil {
		return nil, err
	}
	defer remoteDB.Close()

	filesNeedUpdate := make(map[string]bool)
	for path := range filesToPreserve {
		remoteFile, err := pickFirstFile(remoteDB, path)
		if err != nil {
			return nil, err
		}
		if remoteFile == nil {
			continue
		}

		localFile, ok := localMap[path]
		if filepath.Ext(remoteFile.Name()) == ".strm" || !ok || (remoteFile.ModTime().Sub(localFile.ModTime()) > 0 && (remoteFile.Size() != localFile.Size() || remoteFile.ETag() != localFile.ETag())) {
			filesNeedUpdate[remoteFile.Path()] = true
		}
	}
	return filesNeedUpdate, nil
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

	log.Println("[INFO] Finalizing updates...")

	o, err := url.Parse(cfg.AlistURL)
	if err != nil {
		return err
	}

	for strm := range strmList {
		fpath := filepath.Join(cfg.DownloadDir, strm)
		dir := filepath.Dir(fpath)
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return err
		}

		p, err := os.ReadFile(fpath)
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

		target := filepath.Join(cfg.MediaDir, strm)
		if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(s+"\n"), filePerm); err != nil {
			return err
		}
	}

	localDB, err := sql.Open("sqlite3", filepath.Join(cfg.MediaDir, ".metadata.db"))
	if err != nil {
		return err
	}
	defer localDB.Close()

	remoteDB, err := sql.Open("sqlite3", filepath.Join(cfg.DownloadDir, ".metadata.db"))
	if err != nil {
		return err
	}
	defer remoteDB.Close()

	for file := range otherList {
		fpath := filepath.Join(cfg.DownloadDir, file)
		dir := filepath.Dir(fpath)
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return err
		}

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

		if err := copyFile(tx, remoteFile, filepath.Join(cfg.MediaDir, file), fpath); err != nil {
			tx.Rollback()
			return err
		}
		tx.Rollback()
	}
	log.Println("[INFO] Done.")
	return nil
}

func (cfg *Config) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "xiaoya-emby",
		Short:   "Xiaoya utility for Emby",
		Long:    `Utility to maintain metadata files in xiaoya media library for Emby`,
		Version: Version,
		Run: func(cmd *cobra.Command, args []string) {
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
	cmd.Flags().BoolVar(&cfg.RunAsDaemon, "daemon", true, "Run as daemon in foreground")
	cmd.Flags().StringVar(&cfg.RunCron, "cron-expr", "0 0 * * *", "Cron expression as scheduled task. Must run as daemon.")
	cmd.Flags().StringVarP(&cfg.MediaDir, "media-dir", "d", "/media", "Media directory of Emby to maintain metadata")
	cmd.Flags().StringVarP(&cfg.DownloadDir, "download-dir", "D", "/download", "Media directory of Emby to download metadata to")
	cmd.Flags().BoolVarP(&cfg.Purge, "purge", "p", true, "Whether to purge useless file or directory when media is no longer available")
	cmd.Flags().BoolVarP(&cfg.Help, "help", "h", false, "Print this message")
	cmd.Flags().BoolVarP(&version, "version", "v", false, "Print software version")
	cmd.Flags().StringSliceVarP(&cfg.MirrorURL, "mirror-url", "m", nil, "Specify the mirror URL to sync metadata from")
	cmd.Flags().StringVarP(&cfg.AlistURL, "alist-url", "u", defaultAlistEndpoint, "Endpoint of xiaoya Alist. Change this value will result to url overide in strm file")
	cmd.Flags().StringVarP(&cfg.AlistStrmRootPath, "alist-strm-root-path", "r", defaultAlistStrmRootPath, "Root path of strm files in xiaoya Alist")
	cmd.Flags().StringSliceVar(&cfg.AlistPathSkipVerify, "alist-path-skip-verify", nil, "Specify the Alist path to skip verify files. For example: \"/🏷️我的115分享\"")
	cmd.Flags().StringSliceVar(&cfg.StrmPathSkipVerify, "strm-path-skip-verify", nil, "Specify the metadata path to skip verify strm files. For example: \"/115\"")
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

func copyFile(tx *sql.Tx, file *MetadataFile, to, from string) error {
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	if err := os.MkdirAll(filepath.Dir(to), dirPerm); err != nil {
		return err
	}

	toFile, err := os.Create(to)
	if err != nil {
		return err
	}
	defer toFile.Close()

	fromFile, err := os.Open(from)
	if err != nil {
		return err
	}
	defer fromFile.Close()

	_, err = io.Copy(toFile, fromFile)
	if err != nil {
		return err
	}

	_, err = stmt.Exec(file.Path(), file.Name(), file.Size(), file.modified, file.ETag())
	if err != nil {
		return err
	}
	return tx.Commit()
}
