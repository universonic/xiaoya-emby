package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type AlistClient struct {
	Endpoint *url.URL

	client *http.Client
}

func (c *AlistClient) get(path string) (*AlistGetResult, error) {
	u := *c.Endpoint
	u.Path = "api/fs/get"

	p, _ := json.Marshal(AlistGetPayload{
		Path: path,
	})

	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(p))
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", GlobalUserAgent)

	var resp *http.Response
	for range 3 {
		resp, err = c.client.Do(req)
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

	p, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}

	r := &AlistGetResult{}
	if err = json.Unmarshal(p, r); err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	return r, nil
}

func (c *AlistClient) Stat(path string) (os.FileInfo, error) {
	r, err := c.get(path)
	if err != nil {
		return nil, err
	}
	return AlistFile{
		path:     path,
		name:     r.Data.Name,
		size:     r.Data.Size,
		modified: r.Data.Modified.Time,
		isdir:    r.Data.IsDir,
	}, nil
}

func (c *AlistClient) list(path string, page, perPage int) (*AlistListResult, error) {
	u := *c.Endpoint
	u.Path = "api/fs/list"

	p, _ := json.Marshal(AlistListPayload{
		Path:    path,
		Page:    page,
		PerPage: perPage,
	})

	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(p))
	if err != nil {
		return nil, &fs.PathError{Op: "List", Path: path, Err: err}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", GlobalUserAgent)

	var resp *http.Response
	for range 3 {
		resp, err = c.client.Do(req)
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
		return nil, &fs.PathError{Op: "List", Path: path, Err: err}
	}

	p, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, &fs.PathError{Op: "List", Path: path, Err: err}
	}

	r := &AlistListResult{}
	if err = json.Unmarshal(p, r); err != nil {
		return nil, &fs.PathError{Op: "List", Path: path, Err: err}
	}
	return r, nil
}

func (c *AlistClient) ReadDir(path string) ([]os.FileInfo, error) {
	var files []os.FileInfo
	count, total := 0, 1
	for i := 1; count < total; i++ {
		r, err := c.list(path, i, 64)
		if err != nil {
			return nil, err
		}
		n := len(r.Data.Content)
		count += n
		total = r.Data.Total

		for j := 0; j < n; j++ {
			singleContent := r.Data.Content[j]
			files = append(files, AlistFile{
				path:     path,
				name:     singleContent.Name,
				size:     singleContent.Size,
				modified: singleContent.Modified.Time,
				isdir:    singleContent.IsDir,
			})
		}
	}
	return files, nil
}

func (c *AlistClient) Walk(root string, fn WalkFunc) error {
	info, err := c.Stat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = c.walk(root, info, fn)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func (c *AlistClient) walk(path string, info os.FileInfo, walkFn WalkFunc) error {
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}

	fileInfos, err := c.ReadDir(path)
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
		err = c.walk(filepath.Join(path, fileInfo.Name()), fileInfo, walkFn)
		if err != nil {
			if !fileInfo.IsDir() || err != filepath.SkipDir {
				return err
			}
		}
	}
	return nil
}

type WalkFunc func(path string, info os.FileInfo, err error) error

type AlistGetPayload struct {
	Path     string `json:"path"`
	Password string `json:"password"`
}

type AlistGetResult struct {
	Code    int                `json:"code"`
	Data    AlistGetResultData `json:"data"`
	Message string             `json:"message"`
}

type AlistGetResultData struct {
	IsDir    bool      `json:"is_dir"`
	Modified Timestamp `json:"modified"`
	Name     string    `json:"name"`
	Provider string    `json:"provider"`
	RawURL   string    `json:"raw_url"`
	ReadMe   string    `json:"readme"`
	Size     int64     `json:"size"`
	Type     int       `json:"type"`
}

type AlistListPayload struct {
	Page     int    `json:"page"`
	Path     string `json:"path"`
	Password string `json:"password"`
	PerPage  int    `json:"per_page"`
	Refresh  bool   `json:"refresh"`
}

type AlistListResult struct {
	Code    int                 `json:"code"`
	Data    AlistListResultData `json:"data"`
	Message string              `json:"message"`
}

type AlistListResultData struct {
	Content  []*AlistListResultDataEntry `json:"content"`
	Provider string                      `json:"provider"`
	ReadMe   string                      `json:"readme"`
	Total    int                         `json:"total"`
	Write    bool                        `json:"write"`
}

type AlistListResultDataEntry struct {
	IsDir    bool      `json:"is_dir"`
	Modified Timestamp `json:"modified"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Type     int       `json:"type"`
}

type Timestamp struct {
	time.Time
}

func (t *Timestamp) UnmarshalJSON(p []byte) error {
	var s string
	err := json.Unmarshal(p, &s)
	if err != nil {
		return err
	}

	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

func (t *Timestamp) MarshalJSON() ([]byte, error) {
	s := t.Time.Format(time.RFC3339Nano)
	p, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func NewAlistClient(endpoint string) (*AlistClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	return &AlistClient{Endpoint: u, client: http.DefaultClient}, nil
}

// AlistFile is file in Alist
type AlistFile struct {
	path     string
	name     string
	size     int64
	modified time.Time
	isdir    bool
}

// Path returns the full path of a file
func (f AlistFile) Path() string {
	return f.path
}

// Name returns the name of a file
func (f AlistFile) Name() string {
	return f.name
}

// Size returns the size of a file
func (f AlistFile) Size() int64 {
	return f.size
}

// Mode will return the mode of a given file
func (f AlistFile) Mode() os.FileMode {
	if f.isdir {
		return dirPerm | os.ModeDir
	}
	return filePerm
}

// ModTime returns the modified time of a file
func (f AlistFile) ModTime() time.Time {
	return f.modified
}

// IsDir let us see if a given file is a directory or not
func (f AlistFile) IsDir() bool {
	return f.isdir
}

// Sys ????
func (f AlistFile) Sys() any {
	return nil
}

// String lets us see file information
func (f AlistFile) String() string {
	if f.isdir {
		return fmt.Sprintf("drwxr-xr-x\t%d\t%v\t%s", f.size, f.ModTime(), f.path)
	}
	return fmt.Sprintf("-rw-r--r--\t%d\t%v\t%s", f.size, f.ModTime(), f.path)
}
