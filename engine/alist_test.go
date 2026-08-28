package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
