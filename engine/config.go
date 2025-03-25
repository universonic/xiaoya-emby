package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

var (
	Version = "v0.1.0"
)

const (
	GlobalUserAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/96.0.4664.110 Safari/537.36"
	defaultAlistEndpoint     = "http://xiaoya.host:5678"
	defaultAlistStrmRootPath = "/d"
)

type Config struct {
	RunMode           int
	RunAsDaemon       bool
	RunIntervalInHour int
	MediaDir          string
	DownloadDir       string
	Purge             bool
	Help              bool
	MirrorURL         []string
	AlistURL          string
	AlistStrmRootPath string

	alistClient *AlistClient
}

func (cfg *Config) getAllAlistFilesOnDemand() ([]*MetadataFile, error) {
	if err := os.MkdirAll(cfg.MediaDir, dirPerm); err != nil {
		return nil, err
	}
	fpath := filepath.Join(cfg.MediaDir, ".alist.db")
	fi, err := os.Stat(fpath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg.generateAndCacheAlistFiles()
		}

		return nil, err
	}

	if time.Since(fi.ModTime()) > time.Hour*24 {
		os.Remove(fpath)
		return cfg.generateAndCacheAlistFiles()
	}

	files, err := cfg.listAlistFilesFromCache()
	if err != nil {
		return nil, err
	}
	if len(files) > 0 {
		return files, nil
	}

	os.Remove(fpath)
	return cfg.generateAndCacheAlistFiles()
}

func (cfg *Config) listAlistFilesFromCache() ([]*MetadataFile, error) {
	log.Println("[INFO] Read Alist manifests from cache...")

	db, err := sql.Open("sqlite3", filepath.Join(cfg.MediaDir, ".alist.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err = createFileTable(db); err != nil {
		return nil, err
	}
	return listFiles(db)
}

func (cfg *Config) generateAndCacheAlistFiles() ([]*MetadataFile, error) {
	if err := cfg.generateAlistDB(); err != nil {
		return nil, err
	}
	return cfg.listAlistFilesFromCache()
}

func (cfg *Config) Run(ecodeCh chan<- int, errCh chan<- error) {
	if cfg.alistClient == nil {
		cfg.alistClient, _ = NewAlistClient(cfg.AlistURL)
	}

	var (
		remote, alistFiles []*MetadataFile
		err                error
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

ALIST:
	if cfg.RunMode&2 == 2 {
		alistFiles, err = cfg.getAllAlistFilesOnDemand()
		if err != nil {
			if cfg.RunAsDaemon {
				log.Printf("[ERROR] Critical error: %v", err)
				time.Sleep(time.Second * 5)
				goto ALIST
			}
			ecodeCh <- 3
			errCh <- err
			return
		}
		log.Printf("[INFO] Found %d Alist file(s) in total.", len(alistFiles))
	} else {
		alistFiles, err = cfg.listAlistFilesFromCache()
		if err != nil {
			if cfg.RunAsDaemon {
				log.Printf("[ERROR] Critical error: %v", err)
				time.Sleep(time.Second * 5)
				goto ALIST
			}
			ecodeCh <- 3
			errCh <- err
			return
		}
		log.Printf("[INFO] Found %d Alist file(s) in total (cached).", len(alistFiles))
	}

	if cfg.RunMode&1 != 1 {
		ecodeCh <- 0
		errCh <- nil
		return
	}

COMPARE:
	filesToPreserve, err := cfg.compareMetadata(remote, alistFiles)
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
		d := time.Hour * time.Duration(cfg.RunIntervalInHour)
		log.Printf("[INFO] Next task will be started at: %s. Waiting for %d hours...", time.Now().Add(d).Format(time.RFC3339), cfg.RunIntervalInHour)
		time.Sleep(d)
		goto METADATA
	}
	ecodeCh <- 0
	errCh <- nil
}

func (cfg *Config) downloadMetadata() ([]*MetadataFile, error) {
	log.Println("[INFO] Start metadata synchronization...")
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

func (cfg *Config) generateAlistDB() error {
	log.Println("[INFO] Collecting available Alist files...")
	defer log.Println("[INFO] Collected Alist files.")

	db, err := sql.Open("sqlite3", filepath.Join(cfg.MediaDir, ".alist.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	if err = createFileTable(db); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err = cfg.alistClient.Walk("/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if _, ok := err.(*fs.PathError); ok {
				log.Println("[WARN] Error validating Alist file:", err)
				return nil
			}
			log.Println("[ERROR] Error validating Alist file:", err)
			return err
		}
		if info.IsDir() {
			return nil
		}

		stmt, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?,?,?,?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()

		_, err = stmt.Exec(path, filepath.Base(path), info.Size(), info.ModTime().Unix(), info.IsDir())
		if err != nil {
			return err
		}

		log.Printf("[INFO] Verified file on Alist [%v]: %s", cfg.alistClient.Endpoint, path)
		return nil
	}); err != nil {
		return err
	}
	return tx.Commit()
}

func (cfg *Config) compareMetadata(files, alistFiles []*MetadataFile) (map[string]bool, error) {
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

	validDirs := 0
	rootDirMap := make(map[string]int)
	strmToSkip := make(map[string]bool)
	alistMap := make(map[string]bool)

	for _, file := range alistFiles {
		alistMap[file.Path()] = true
	}

	for path, strmsMap := range strmMap {
		valids := 0
		for strm := range strmsMap {
			fpath := filepath.Join(path, strm)
			p, err := os.ReadFile(filepath.Join(cfg.DownloadDir, fpath))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			s := strings.ReplaceAll(string(bytes.TrimSpace(p)), "%20", " ")
			u, err := url.Parse(s)
			if err != nil {
				log.Printf("[ERROR] Stream cannot be verified: [%s] %v", s, err)
				strmToSkip[fpath] = true
				continue
			}

			linkpath := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(u.Path, "/"), defaultAlistStrmRootPath), "/")
			if ok := alistMap[linkpath]; ok {
				valids++
			} else if cfg.Purge {
				strmToSkip[fpath] = true
				log.Printf("[WARN] Absent stream on Alist: %s", linkpath)
			}
		}
		if valids > 0 {
			rootDirMap[getRootDir(path, cfg.MediaDir)]++
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
		u, err := url.Parse(s)
		if err == nil {
			s = u.String()
		}

		if strings.HasPrefix(s, defaultAlistEndpoint) {
			relUrl := "/" + strings.TrimPrefix(strings.TrimPrefix("/"+strings.TrimPrefix(u.Path, "/"), defaultAlistStrmRootPath), "/")
			relUrl = "/" + strings.TrimPrefix(cfg.AlistStrmRootPath, "/") + "/" + strings.TrimPrefix(relUrl, "/")

			u.Scheme, u.Opaque, u.User, u.Host, u.Path = o.Scheme, o.Opaque, o.User, o.Host, relUrl
			s = u.String()
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
		Use:   "xiaoya-emby",
		Short: "Xiaoya utility for Emby",
		Long:  `Utility to maintain metadata files in xiaoya media library for Emby`,
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
	cmd.Flags().IntVar(&cfg.RunMode, "mode", 7, "Run mode (4: scan metadata, 2: scan alist, 1: sync metadata)")
	cmd.Flags().BoolVar(&cfg.RunAsDaemon, "daemon", true, "Run as daemon in foreground")
	cmd.Flags().IntVar(&cfg.RunIntervalInHour, "run-interval-in-hour", 24, "Hours between two run cycles. Ignored unless run as daemon.")
	cmd.Flags().StringVarP(&cfg.MediaDir, "media-dir", "d", "/media", "Media directory of Emby to maintain metadata")
	cmd.Flags().StringVarP(&cfg.DownloadDir, "download-dir", "D", "/download", "Media directory of Emby to download metadata to")
	cmd.Flags().BoolVarP(&cfg.Purge, "purge", "p", true, "Whether to purge useless file or directory when media is no longer available")
	cmd.Flags().BoolVarP(&cfg.Help, "help", "h", false, "Print this message")
	cmd.Flags().StringSliceVarP(&cfg.MirrorURL, "mirror-url", "m", nil, "Specify the mirror URL to sync metadata from")
	cmd.Flags().StringVarP(&cfg.AlistURL, "alist-url", "u", defaultAlistEndpoint, "Endpoint of xiaoya Alist. Change this value will result to url overide in strm file")
	cmd.Flags().StringVarP(&cfg.AlistStrmRootPath, "alist-strm-root-path", "r", defaultAlistStrmRootPath, "Root path of strm files in xiaoya Alist")
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
