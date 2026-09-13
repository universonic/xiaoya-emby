package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// alistTestServer serves controllable api/fs/list responses.
type alistTestServer struct {
	*httptest.Server
	handler func(w http.ResponseWriter, r *http.Request)
}

func newAlistServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *AlistClient {
	t.Helper()
	srv := &alistTestServer{handler: handler}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)
	client, err := NewAlistClient(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestAlistReadDirValidatesCodeAndData(t *testing.T) {
	// Non-success API code is a hard error, never a silent empty listing.
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":500,"message":"failed list: storage driver error","data":null}`))
	})
	if _, err := client.ReadDir(context.Background(), "/每日更新"); err == nil {
		t.Fatal("non-success code accepted")
	} else if !strings.Contains(err.Error(), "500") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Success code with a nil Data payload is malformed.
	client = newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":200,"message":"ok","data":null}`))
	})
	if _, err := client.ReadDir(context.Background(), "/每日更新"); err == nil {
		t.Fatal("nil data accepted")
	}
}

func TestAlistReadDirClassifiesNotFound(t *testing.T) {
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":500,"message":"failed list: object not found","data":null}`))
	})
	_, err := client.ReadDir(context.Background(), "/missing")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("not-found classification = %v", err)
	}
}

func TestAlistReadDirDetectsStalledPagination(t *testing.T) {
	// Total claims more entries than any page delivers: the listing stalls
	// and must fail instead of returning a partial result.
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":200,"message":"ok","data":{"content":[],"total":5}}`))
	})
	if _, err := client.ReadDir(context.Background(), "/每日更新"); err == nil {
		t.Fatal("stalled pagination accepted")
	}
}

func TestAlistReadDirCompletesPagination(t *testing.T) {
	page := 0
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 1:
			fmt.Fprint(w, `{"code":200,"data":{"content":[{"name":"a","size":1},{"name":"b","size":2}],"total":3}}`)
		case 2:
			fmt.Fprint(w, `{"code":200,"data":{"content":[{"name":"c","size":3}],"total":3}}`)
		default:
			fmt.Fprint(w, `{"code":200,"data":{"content":[],"total":3}}`)
		}
	})
	infos, err := client.ReadDir(context.Background(), "/每日更新")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("listing = %d entries, want 3", len(infos))
	}
}

func TestAlistCodeErrorClassification(t *testing.T) {
	if err := alistCodeError(200, ""); err != nil {
		t.Fatalf("success classified as error: %v", err)
	}
	// Provider throttling markers and bare HTTP 429 are transient.
	for _, tc := range []struct {
		code int
		msg  string
	}{
		{500, "无法显示目录下文件，请刷新重试: rule:TooManyRequests"},
		{429, ""},
		{500, "Too many requests, please retry later"},
	} {
		err := alistCodeError(tc.code, tc.msg)
		if !errors.Is(err, errAlistRateLimited) {
			t.Fatalf("(%d, %q) not classified as rate limit: %v", tc.code, tc.msg, err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("(%d, %q) misclassified as not-found: %v", tc.code, tc.msg, err)
		}
	}
	if err := alistCodeError(500, "storage driver error"); errors.Is(err, errAlistRateLimited) {
		t.Fatalf("generic error misclassified as rate limit: %v", err)
	}
	if err := alistCodeError(500, "object not found"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("not-found = %v", err)
	}
	if err := alistCodeError(500, "failed get obj: object not found"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrapped object not-found = %v", err)
	}
	// Infrastructure errors must never masquerade as a missing object:
	// they fail the phase instead of producing a deletion plan.
	for _, msg := range []string{"storage not found", "failed get storage: storage not found", "path not exist", "record not found"} {
		if err := alistCodeError(500, msg); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%q misclassified as not-found: %v", msg, err)
		}
	}
	if err := alistCodeError(403, "permission denied"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("permission error = %v", err)
	}
}

// shrinkRateLimitDelay makes the retry waits test-fast and restores the
// real delay afterwards.
func shrinkRateLimitDelay(t *testing.T) {
	t.Helper()
	orig := alistRateLimitBaseDelay
	alistRateLimitBaseDelay = time.Millisecond
	t.Cleanup(func() { alistRateLimitBaseDelay = orig })
}

func TestAlistReadDirRetriesRateLimit(t *testing.T) {
	shrinkRateLimitDelay(t)
	calls := 0
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls <= 2 {
			fmt.Fprint(w, `{"code":500,"message":"无法显示目录下文件，请刷新重试: rule:TooManyRequests","data":null}`)
			return
		}
		fmt.Fprint(w, `{"code":200,"data":{"content":[{"name":"a","size":1}],"total":1}}`)
	})
	infos, err := client.ReadDir(context.Background(), "/纪录片/地理风光/挺进珠峰")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("listing = %d entries, want 1", len(infos))
	}
	if calls != 3 {
		t.Fatalf("requests = %d, want 3 (2 rate-limited + 1 success)", calls)
	}
}

func TestAlistReadDirRetriesHTTPRateLimit(t *testing.T) {
	shrinkRateLimitDelay(t)
	calls := 0
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":200,"data":{"content":[{"name":"a","size":1}],"total":1}}`)
	})
	infos, err := client.ReadDir(context.Background(), "/纪录片")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || calls != 3 {
		t.Fatalf("listing = %d entries after %d requests, want 1 after 3", len(infos), calls)
	}
}

func TestJitteredBackoffBounds(t *testing.T) {
	delay := 100 * time.Millisecond
	for range 100 {
		wait := jitteredBackoff(delay)
		if wait < delay/2 || wait > delay {
			t.Fatalf("jittered wait %v outside [%v, %v]", wait, delay/2, delay)
		}
	}
}

func TestAlistReadDirRateLimitExhausted(t *testing.T) {
	shrinkRateLimitDelay(t)
	calls := 0
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":500,"message":"无法显示目录下文件，请刷新重试: rule:TooManyRequests","data":null}`)
	})
	_, err := client.ReadDir(context.Background(), "/纪录片")
	if err == nil {
		t.Fatal("persistent rate limit accepted")
	}
	if !errors.Is(err, errAlistRateLimited) {
		t.Fatalf("exhausted error lost rate-limit marker: %v", err)
	}
	if !strings.Contains(err.Error(), "TooManyRequests") {
		t.Fatalf("exhausted error lost original message: %v", err)
	}
	if want := alistRateLimitRetries + 1; calls != want {
		t.Fatalf("requests = %d, want %d", calls, want)
	}
}

func TestAlistReadDirDoesNotRetryOtherErrors(t *testing.T) {
	shrinkRateLimitDelay(t)
	calls := 0
	client := newAlistServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":500,"message":"failed list: storage driver error","data":null}`)
	})
	if _, err := client.ReadDir(context.Background(), "/每日更新"); err == nil {
		t.Fatal("storage error accepted")
	} else if errors.Is(err, errAlistRateLimited) {
		t.Fatalf("storage error misclassified as rate limit: %v", err)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1 (no retry for non-rate-limit errors)", calls)
	}
}

func TestVerifyAlistTargetsStopsAfterRateLimitExhaustion(t *testing.T) {
	shrinkRateLimitDelay(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":500,"message":"rule:TooManyRequests","data":null}`)
	}))
	t.Cleanup(srv.Close)

	const dirs = 32
	alistToScan := make(map[string]map[string]string, dirs)
	for i := range dirs {
		path := fmt.Sprintf("/dir-%d", i)
		alistToScan[path] = map[string]string{"video.strm": path + "/video.strm"}
	}
	validDirs := 0
	err := (&Config{}).verifyAlistTargets(
		context.Background(),
		SyncSettings{AlistURL: srv.URL + "/"},
		alistToScan,
		make(map[string]bool),
		make(map[string]int),
		&validDirs,
	)
	if !errors.Is(err, errAlistRateLimited) {
		t.Fatalf("verification error = %v, want exhausted rate limit", err)
	}

	maxCalls := min(defaultWorkers(), dirs) * (alistRateLimitRetries + 1)
	if got := int(calls.Load()); got > maxCalls {
		t.Fatalf("requests = %d, want at most %d after scan cancellation", got, maxCalls)
	}
}
