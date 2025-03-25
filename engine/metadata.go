package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	_ "github.com/mattn/go-sqlite3"
)

const (
	filePerm = 0644
	dirPerm  = 0755
)

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
		"https://emby.xiaoya.pro/",
		"https://icyou.eu.org/",
		"https://emby.8.net.co/",
		"https://emby.raydoom.tk/",
		"https://emby.kaiserver.uk/",
		"https://embyxiaoya.laogl.top/",
		"https://emby-data.poxi1221.eu.org/",
		"https://emby-data.ermaokj.cn/",
		"https://emby-data.bdbd.fun/",
		"https://emby-data.wwwh.eu.org/",
		"https://emby-data.ymschh.top/",
		"https://emby-data.wx1.us.kg/",
		"https://emby-data.r2s.site/",
		"https://emby-data.neversay.eu.org/",
		"https://emby-data.800686.xyz/",
	}
	sFolder = []string{".sync"}
	sExt    = []string{".ass", ".srt", ".ssa"}
)

type MetadataCrawler struct {
	mux               sync.Mutex
	client            *http.Client
	downloadDir       string
	mirrors           []string
	validMirrors      []string
	selectedPaths     []string
	ignoredDirs       []string // TODO:
	ignoredExtentions []string // TODO:
	purge             bool
}

type sortMirror struct {
	mirror   string
	duration time.Duration
}

func NewMetadataCrawler(downloadDir string, mirrors, selectedPaths, ignoredDirs, ignoredExtentions []string, purge bool) (*MetadataCrawler, error) {
	mc := &MetadataCrawler{
		client:            &http.Client{Timeout: 60 * time.Second},
		downloadDir:       downloadDir,
		selectedPaths:     selectedPaths,
		ignoredDirs:       ignoredDirs,
		ignoredExtentions: ignoredExtentions,
		purge:             purge,
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
				log.Printf("[ERROR] Failed to validate mirrors: %v", err)
			}
		case <-ctx.Done():
			break LOOP
		}
	}
}

func (mc *MetadataCrawler) validateMirrors() error {
	var mirrorsToSort []sortMirror
	log.Println("[INFO] Validating metadata mirrors...")
	for _, mirror := range mc.mirrors {
		dur := []time.Duration{}
		for i := 0; i < 5; i++ {
			if d := validateMirror(mirror); d > 0 {
				dur = append(dur, d)
			}
		}
		if len(dur) < 4 {
			log.Printf("[WARN] Invalid metadata mirror: %s", mirror)
			continue
		}
		sum := time.Duration(0)
		for _, d := range dur {
			sum += d
		}
		m := sortMirror{
			mirror:   mirror,
			duration: sum / time.Duration(len(dur)),
		}
		mirrorsToSort = append(mirrorsToSort, m)
		log.Printf("[INFO] Validated metadata mirror: %s (%dms)", mirror, m.duration/time.Millisecond)
	}
	sort.Slice(mirrorsToSort, func(i, j int) bool { return mirrorsToSort[i].duration < mirrorsToSort[j].duration })

	var mirrors []string
	for _, m := range mirrorsToSort {
		mirrors = append(mirrors, m.mirror)
	}
	if len(mirrors) == 0 {
		return fmt.Errorf("at least one metadata mirror is required")
	}

	mc.mux.Lock()
	defer mc.mux.Unlock()

	mc.validMirrors = mirrors
	return nil
}

func (mc *MetadataCrawler) activeMirrors() []string {
	mc.mux.Lock()
	defer mc.mux.Unlock()

	return mc.validMirrors
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
	req.Header.Set("User-Agent", GlobalUserAgent)

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
			err = errors.New(resp.Status)
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
	for _, mirror := range mc.activeMirrors() {
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
	req.Header.Set("User-Agent", GlobalUserAgent)

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
			err = errors.New(resp.Status)
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
	for _, mirror := range mc.activeMirrors() {
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

type failedEntry struct {
	path string
	info *MetadataFile
}

func (mc *MetadataCrawler) Sync() error {
	if err := os.MkdirAll(mc.downloadDir, dirPerm); err != nil {
		return err
	}

	db, err := sql.Open("sqlite3", filepath.Join(mc.downloadDir, ".metadata.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	localMap := make(map[string]*MetadataFile)
	remoteMap := make(map[string]*MetadataFile)

	local, err := mc.localFiles(db)
	if err != nil {
		return err
	}
	for _, file := range local {
		localMap[file.Path()] = file
	}

	selectedRoot := make(map[string]bool)
	for _, path := range mc.selectedPaths {
		selectedRoot[strings.TrimPrefix(path, "/")] = true
	}

	var (
		wg     sync.WaitGroup
		mux    sync.Mutex
		failed []failedEntry
	)
	workerChan := make(chan struct{}, defaultWorkers())

	if err := mc.Walk("/", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("[ERROR] Error validating metadata file: %v", err)
			return err
		}
		if info.IsDir() {
			ss := strings.Split(strings.TrimPrefix(path, "/"), "/")
			rootpath := ss[0]
			if path != "/" && !selectedRoot[rootpath] {
				log.Printf("[INFO] Skipped Directory: %s", path)
				return filepath.SkipDir
			}
			return nil
		}

		oldFile, err := pickFirstFile(db, path)
		if err != nil {
			return err
		}

		if oldFile != nil {
			fp := filepath.Join(mc.downloadDir, strings.TrimLeft(oldFile.Path(), "/"))
			_, err = os.Stat(fp)
			if err != nil {
				log.Printf("[WARN] Missing file: %s", oldFile.Path())
				oldFile = nil
			}
		}

		wg.Add(1)
		go func(path string, oldFile *MetadataFile) {
			defer wg.Done()

			workerChan <- struct{}{}
			defer func() { <-workerChan }()

			tx, err := db.Begin()
			if err != nil {
				mux.Lock()
				defer mux.Unlock()

				failed = append(failed, failedEntry{
					path: path,
					info: oldFile,
				})
				log.Printf("[ERROR] Critical DB error: %v", err)
				return
			}
			defer tx.Rollback()

			if err = mc.Download(tx, path, func(newFile *MetadataFile) bool {
				mux.Lock()
				defer mux.Unlock()

				remoteMap[newFile.Path()] = newFile

				return oldFile == nil || newFile.ModTime().Sub(oldFile.ModTime()) > 0 && (newFile.Size() != oldFile.Size() || newFile.ETag() != oldFile.ETag())
			}); err != nil {
				mux.Lock()
				defer mux.Unlock()

				log.Printf("[ERROR] Failed to download: %s", path)
				failed = append(failed, failedEntry{
					path: path,
					info: oldFile,
				})
				return
			}
		}(path, oldFile)

		return nil
	}); err != nil {
		log.Printf("[ERROR] Critical error: %v", err)

		wg.Wait()
		return err
	}

	wg.Wait()

	var (
		failed2 []failedEntry
		retry   int
	)
FINAL:
	for _, each := range failed {
		wg.Add(1)
		go func(path string, oldFile *MetadataFile) {
			defer wg.Done()

			workerChan <- struct{}{}
			defer func() { <-workerChan }()

			tx, err := db.Begin()
			if err != nil {
				mux.Lock()
				defer mux.Unlock()

				failed2 = append(failed2, failedEntry{
					path: path,
					info: oldFile,
				})
				log.Printf("[ERROR] Critical DB error: %v", err)
				return
			}
			defer tx.Rollback()

			if err = mc.Download(tx, path, func(newFile *MetadataFile) bool {
				mux.Lock()
				defer mux.Unlock()

				remoteMap[newFile.Path()] = newFile

				return oldFile == nil || newFile.ModTime().Sub(oldFile.ModTime()) > 0 && (newFile.Size() != oldFile.Size() || newFile.ETag() != oldFile.ETag())
			}); err != nil {
				mux.Lock()
				defer mux.Unlock()

				log.Printf("[ERROR] Failed to download: %s", path)
				failed2 = append(failed2, failedEntry{
					path: path,
					info: oldFile,
				})
				return
			}
		}(each.path, each.info)
	}

	wg.Wait()
	if len(failed2) > 0 {
		if retry > 5 {
			log.Println("[ERROR] Metadata download has exceeded the maximum retry attempts.")
			return fmt.Errorf("maximum retry attempts exceeded")
		}
		failed = make([]failedEntry, len(failed2))
		copy(failed, failed2)
		failed2 = failed2[:0]
		retry++
		log.Println("[INFO] Failed metadata entries will be retried...")
		goto FINAL
	}

	// if mc.purge {
	// 	for _, oldFile := range local {
	// 		if file, ok := remoteMap[oldFile.Path()]; !ok {
	// 			tx, err := db.Begin()
	// 			if err != nil {
	// 				return err
	// 			}

	// 			if err := deleteFile(tx, oldFile); err != nil {
	// 				tx.Rollback()
	// 				continue
	// 			}
	// 			deleteDirIfEmpty(filepath.Dir(file.Path()))
	// 			tx.Rollback()
	// 		}
	// 	}
	// }
	return nil
}

func (mc *MetadataCrawler) LocalFiles() ([]*MetadataFile, error) {
	db, err := sql.Open("sqlite3", filepath.Join(mc.downloadDir, ".metadata.db"))
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

func (mc *MetadataCrawler) download(tx *sql.Tx, path, mirror string, filterFn func(f *MetadataFile) bool) (err error) {
	u, err := url.Parse(mirror)
	if err != nil {
		return &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	u.Path = filepath.Join(u.Path, path)

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	req.Header.Set("User-Agent", GlobalUserAgent)

	var resp *http.Response
	for range 3 {
		resp, err = mc.client.Do(req)
		if err != nil {
			log.Printf("[WARN] Error downloading [%s] %s: %v", mirror, path, err)
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
			err = errors.New(resp.Status)
			time.Sleep(time.Second * 3)
			continue
		}
		break
	}
	if err != nil {
		return &fs.PathError{Op: "Get", Path: path, Err: err}
	}

	contentType := resp.Header.Get("Content-Type")
	ss := strings.Split(contentType, ";")
	if len(ss) > 1 {
		contentType = strings.TrimSpace(ss[0])
	}
	if contentType == "text/html" {
		// ignore html document
		return nil
	}

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	timestamp, _ := time.Parse(time.RFC1123, resp.Header.Get("Last-Modified"))
	f := &MetadataFile{
		path:     path,
		name:     filepath.Base(path),
		size:     size,
		modified: timestamp.Unix(),
		etag:     resp.Header.Get("ETag"),
	}

	if filterFn == nil || filterFn(f) {
		log.Printf("[INFO] Downloading [%s]: %s", mirror, path)
		filePath := filepath.Join(mc.downloadDir, strings.TrimLeft(f.Path(), "/"))
		if err := os.MkdirAll(filepath.Dir(filePath), dirPerm); err != nil {
			return &fs.PathError{Op: "Get", Path: f.Path(), Err: err}
		}

		out, err := os.Create(filePath)
		if err != nil {
			return &fs.PathError{Op: "Get", Path: f.Path(), Err: err}
		}
		defer out.Close()

		f.etag = resp.Header.Get("ETag")

		if _, err := io.Copy(out, resp.Body); err != nil {
			return &fs.PathError{Op: "Get", Path: f.Path(), Err: err}
		}

		if err = updateToDB(tx, f); err != nil {
			return &fs.PathError{Op: "Get", Path: f.Path(), Err: err}
		}
		log.Printf("[INFO] Downloaded: %s", path)
		return nil
	}

	log.Printf("[INFO] Skipped: %s", f.Path())
	return nil
}

func (mc *MetadataCrawler) Download(tx *sql.Tx, path string, filterFn func(f *MetadataFile) bool) (err error) {
	activeMirrors := mc.activeMirrors()
	for i := range activeMirrors {
		mirror := activeMirrors[i]
		err = mc.download(tx, path, mirror, filterFn)
		if err != nil && i < len(activeMirrors)-1 {
			log.Printf("[WARN] Failed to download %s from mirror %s. It will be try again.", path, mirror)
			continue
		}
		break
	}
	return
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
	req.Header.Set("User-Agent", GlobalUserAgent)

	client := &http.Client{Timeout: 3 * time.Second}
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

func deleteDirIfEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
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

func updateToDB(tx *sql.Tx, file *MetadataFile) error {
	stmt, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	_, err = stmt.Exec(file.Path(), file.Name(), file.Size(), file.modified, file.ETag())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func deleteFile(tx *sql.Tx, file *MetadataFile) error {
	stmt, err := tx.Prepare("DELETE FROM files WHERE path = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	if err := os.Remove(file.Path()); err != nil && !os.IsNotExist(err) {
		return err
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
