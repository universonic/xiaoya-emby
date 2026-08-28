package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// alistClientTimeout bounds every Alist API call; requests are also bound
// by the round context so hangs cannot outlive their sync round.
const alistClientTimeout = 30 * time.Second

// alistMaxPages and alistMaxEntries bound one ReadDir so a misbehaving
// server cannot loop the pagination forever.
const (
	alistMaxPages   = 100_000
	alistMaxEntries = 1_000_000
)

type AlistClient struct {
	Endpoint *url.URL

	client *http.Client
}

func NewAlistClient(endpoint string) (*AlistClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	return &AlistClient{Endpoint: u, client: newBrowserClient(alistClientTimeout)}, nil
}

// doPOST performs the JSON POST with bounded retries. Every attempt builds
// a fresh request (the body reader cannot be reused) and closes the
// response body, including on failures.
func (c *AlistClient) doPOST(ctx context.Context, opName, apiPath, path string, payload any) ([]byte, error) {
	u := *c.Endpoint
	u.Path = apiPath

	var lastErr error
	for range 3 {
		if ctx.Err() != nil {
			return nil, &fs.PathError{Op: opName, Path: path, Err: ctx.Err()}
		}
		p, err := json.Marshal(payload)
		if err != nil {
			return nil, &fs.PathError{Op: opName, Path: path, Err: err}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(p))
		if err != nil {
			return nil, &fs.PathError{Op: opName, Path: path, Err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", GlobalUserAgent)

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			if serr := sleepContext(ctx, backoffFor(err)); serr != nil {
				return nil, &fs.PathError{Op: opName, Path: path, Err: serr}
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = errors.New(resp.Status)
			resp.Body.Close()
			if serr := sleepContext(ctx, 3*time.Second); serr != nil {
				return nil, &fs.PathError{Op: opName, Path: path, Err: serr}
			}
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return body, nil
	}
	return nil, &fs.PathError{Op: opName, Path: path, Err: lastErr}
}

func (c *AlistClient) get(ctx context.Context, path string) (*AlistGetResult, error) {
	body, err := c.doPOST(ctx, "Get", "api/fs/get", path, AlistGetPayload{Path: path})
	if err != nil {
		return nil, err
	}
	r := &AlistGetResult{}
	if err := json.Unmarshal(body, r); err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	return r, nil
}

func (c *AlistClient) Stat(ctx context.Context, path string) (os.FileInfo, error) {
	r, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := alistCodeError(r.Code, r.Message); err != nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: err}
	}
	if r.Data == nil {
		return nil, &fs.PathError{Op: "Get", Path: path, Err: fmt.Errorf("alist get returned no data for %s", path)}
	}
	return AlistFile{
		path:     path,
		name:     r.Data.Name,
		size:     r.Data.Size,
		modified: r.Data.Modified.Time,
		isdir:    r.Data.IsDir,
	}, nil
}

func (c *AlistClient) list(ctx context.Context, path string, page, perPage int) (*AlistListResult, error) {
	body, err := c.doPOST(ctx, "List", "api/fs/list", path, AlistListPayload{
		Path:    path,
		Page:    page,
		PerPage: perPage,
	})
	if err != nil {
		return nil, err
	}
	r := &AlistListResult{}
	if err := json.Unmarshal(body, r); err != nil {
		return nil, &fs.PathError{Op: "List", Path: path, Err: err}
	}
	return r, nil
}

// alistCodeError classifies Alist API result codes: success is 200; an
// object-not-found response is fs.ErrNotExist; anything else is a hard
// error that must fail the verification phase (never silently produce a
// deletion plan). The match is limited to object-level not-found phrases
// so that infrastructure errors like "storage not found" (a disabled or
// missing storage driver) are never treated as a missing object.
func alistCodeError(code int, message string) error {
	if code == 200 {
		return nil
	}
	msg := strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(msg, "object not found") || strings.Contains(msg, "path not found") {
		return fs.ErrNotExist
	}
	return fmt.Errorf("alist api error %d: %s", code, message)
}

// ReadDir lists a directory with full pagination validation: the API code
// must be success, Data must be non-nil, every page must make forward
// progress toward Total, and the result must be complete. Anything else is
// an error; a partial listing is never returned.
func (c *AlistClient) ReadDir(ctx context.Context, path string) ([]os.FileInfo, error) {
	var files []os.FileInfo
	count, total := 0, 1
	for i := 1; count < total; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if i > alistMaxPages || count > alistMaxEntries {
			return nil, &fs.PathError{Op: "List", Path: path, Err: fmt.Errorf("alist listing for %s exceeds pagination limits", path)}
		}
		r, err := c.list(ctx, path, i, 1024)
		if err != nil {
			return nil, err
		}
		if err := alistCodeError(r.Code, r.Message); err != nil {
			return nil, &fs.PathError{Op: "List", Path: path, Err: err}
		}
		if r.Data == nil {
			return nil, &fs.PathError{Op: "List", Path: path, Err: fmt.Errorf("alist list returned no data for %s", path)}
		}
		n := len(r.Data.Content)
		if n == 0 && r.Data.Total > count {
			return nil, &fs.PathError{Op: "List", Path: path, Err: fmt.Errorf("alist listing for %s stalled at page %d (%d/%d)", path, i, count, r.Data.Total)}
		}
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

func (c *AlistClient) Walk(ctx context.Context, root string, fn WalkFunc) error {
	info, err := c.Stat(ctx, root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = c.walk(ctx, root, info, fn)
	}
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func (c *AlistClient) walk(ctx context.Context, path string, info os.FileInfo, walkFn WalkFunc) error {
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}

	fileInfos, err := c.ReadDir(ctx, path)
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
		err = c.walk(ctx, filepath.Join(path, fileInfo.Name()), fileInfo, walkFn)
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
	Code    int                 `json:"code"`
	Data    *AlistGetResultData `json:"data"`
	Message string              `json:"message"`
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
	Code    int                  `json:"code"`
	Data    *AlistListResultData `json:"data"`
	Message string               `json:"message"`
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
