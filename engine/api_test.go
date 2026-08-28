package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// apiHarness builds a control plane around a controller with stubbed
// rounds. Control mutations always require the bearer token.
type apiHarness struct {
	srv   *httptest.Server
	store *SettingsStore
	ctrl  *Controller
	token string
}

type roundFunc func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error)

func instantRound(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
	return OutcomeSuccess, nil
}

func newAPIHarness(t *testing.T, token string) *apiHarness {
	return newAPIHarnessWithRound(t, token, instantRound)
}

func newAPIHarnessWithRound(t *testing.T, token string, round roundFunc) *apiHarness {
	t.Helper()
	resetGlobalStatus()
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	cfg := &Config{DownloadDir: dir}
	ctrl := NewController(cfg, store)
	ctrl.runRound = round
	ctrl.Start()
	waitForControllerIdle(t, ctrl)
	t.Cleanup(ctrl.Stop)
	cp := newControlPlane(ctrl, store, token)
	srv := httptest.NewServer(statusHTTPHandler(cp))
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, store: store, ctrl: ctrl, token: token}
}

func (h *apiHarness) do(method, path string, body any, header map[string]string) (*http.Response, map[string]any) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil
	}
	var payload map[string]any
	if resp.StatusCode != http.StatusNoContent {
		_ = json.NewDecoder(resp.Body).Decode(&payload)
	}
	resp.Body.Close()
	return resp, payload
}

func writeHdr() map[string]string { return map[string]string{csrfHeader: csrfValue} }
func writeHdrWithToken(token string) map[string]string {
	return map[string]string{csrfHeader: csrfValue, "Authorization": "Bearer " + token}
}

func errCodeOf(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if e, ok := payload["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

func TestAPIMethodEnforcement(t *testing.T) {
	h := newAPIHarness(t, "")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/status"},
		{"PUT", "/api/logs"},
		{"GET", "/api/sync"},
		{"PUT", "/api/sync/abort"},
		{"POST", "/nope"},
	} {
		resp, payload := h.do(tc.method, tc.path, nil, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s = %d (%s)", tc.method, tc.path, resp.StatusCode, errCodeOf(payload))
		}
	}
}

func TestAPISyncRequiresConfirmAndValidMode(t *testing.T) {
	h := newAPIHarness(t, "t0ken")
	resp, payload := h.do("POST", "/api/sync", map[string]any{"mode": "full-strict"}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusBadRequest || errCodeOf(payload) != errCodeConfirmRequired {
		t.Fatalf("strict without confirm = %d/%s", resp.StatusCode, errCodeOf(payload))
	}
	resp, payload = h.do("POST", "/api/sync", map[string]any{"mode": "bogus"}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusBadRequest || errCodeOf(payload) != errCodeInvalidMode {
		t.Fatalf("bogus mode = %d/%s", resp.StatusCode, errCodeOf(payload))
	}
	// Missing CSRF header (valid bearer).
	resp, _ = h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, map[string]string{"Authorization": "Bearer t0ken"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF header = %d", resp.StatusCode)
	}
}

func TestAPISyncAcceptedAndBusyConflict(t *testing.T) {
	// The startup round returns instantly; later rounds block until
	// released so the busy state is observable.
	blocks := make(chan struct{})
	release := func() { close(blocks) }
	var roundNum atomic.Int32
	h := newAPIHarnessWithRound(t, "t0ken", func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		if roundNum.Add(1) == 1 {
			return OutcomeSuccess, nil
		}
		<-blocks
		return OutcomeSuccess, nil
	})
	waitForRoundDone(t)
	resp, payload := h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger = %d (%v)", resp.StatusCode, payload)
	}
	if payload["job_id"] == "" || payload["effective_mode"] != SyncTypeIncremental {
		t.Fatalf("trigger payload = %v", payload)
	}
	resp, payload = h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusConflict || errCodeOf(payload) != errCodeBusy {
		t.Fatalf("busy trigger = %d/%s", resp.StatusCode, errCodeOf(payload))
	}
	resp, payload = h.do("POST", "/api/sync/abort", nil, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusAccepted || payload["cancel_requested"] != true {
		t.Fatalf("abort = %d/%v", resp.StatusCode, payload)
	}
	// Repeat abort reports the same state.
	resp, payload = h.do("POST", "/api/sync/abort", nil, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusAccepted || payload["cancel_requested"] != true {
		t.Fatalf("repeat abort = %d/%v", resp.StatusCode, payload)
	}
	release()
	waitForRoundDone(t)
	resp, payload = h.do("POST", "/api/sync/abort", nil, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusConflict || errCodeOf(payload) != errCodeNoActiveJob {
		t.Fatalf("idle abort = %d/%s", resp.StatusCode, errCodeOf(payload))
	}
}

func TestAPIConfigCRUD(t *testing.T) {
	h := newAPIHarness(t, "t0ken")

	resp, payload := h.do("GET", "/api/config", nil, map[string]string{"Authorization": "Bearer t0ken"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config get = %d (%v)", resp.StatusCode, payload)
	}
	rev, _ := payload["revision"].(float64)
	if rev != 0 {
		t.Fatalf("initial revision = %v", rev)
	}

	// PUT with stale revision conflicts.
	next := validSettings()
	next.Cleanup = true
	resp, payload = h.do("PUT", "/api/config", map[string]any{"revision": 99, "settings": next}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusConflict || errCodeOf(payload) != errCodeRevisionConflict {
		t.Fatalf("stale put = %d/%s", resp.StatusCode, errCodeOf(payload))
	}

	// PUT with invalid settings is a 400 and does not bump the revision.
	next = validSettings()
	next.RunMode = 0
	resp, payload = h.do("PUT", "/api/config", map[string]any{"revision": 0, "settings": next}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusBadRequest || errCodeOf(payload) != errCodeInvalidSettings {
		t.Fatalf("invalid put = %d/%s", resp.StatusCode, errCodeOf(payload))
	}

	// Valid PUT persists and returns the new snapshot.
	next = validSettings()
	next.Cleanup = true
	resp, payload = h.do("PUT", "/api/config", map[string]any{"revision": 0, "settings": next}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid put = %d (%v)", resp.StatusCode, payload)
	}
	if payload["revision"].(float64) != 1 {
		t.Fatalf("revision after put = %v", payload["revision"])
	}
	settings := payload["settings"].(map[string]any)
	if settings["cleanup"] != true {
		t.Fatalf("settings round trip wrong: %v", settings)
	}

	// Unknown JSON fields are rejected.
	resp, payload = h.do("PUT", "/api/config", map[string]any{"revision": 1, "settings": validSettings(), "hack": 1}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field put = %d (%s)", resp.StatusCode, errCodeOf(payload))
	}

	// Content-Type is mandatory.
	b, _ := json.Marshal(map[string]any{"revision": 1, "settings": validSettings()})
	req, _ := http.NewRequest("PUT", h.srv.URL+"/api/config", bytes.NewReader(b))
	req.Header.Set(csrfHeader, csrfValue)
	req.Header.Set("Authorization", "Bearer t0ken")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("put without content type = %d", resp2.StatusCode)
	}

	// Body limit is enforced.
	huge := validSettings()
	huge.MirrorURL = []string{strings.Repeat("https://m.example.com/", 20000)}
	resp, _ = h.do("PUT", "/api/config", map[string]any{"revision": 1, "settings": huge}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized put = %d", resp.StatusCode)
	}

	// DELETE restores the baseline.
	resp, payload = h.do("DELETE", "/api/config", nil, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d (%v)", resp.StatusCode, payload)
	}
	if payload["persisted"] != false {
		t.Fatalf("persisted after delete = %v", payload["persisted"])
	}
	s, _ := h.store.Snapshot()
	if s.Cleanup {
		t.Fatal("baseline not restored")
	}
}

func TestAPITokenPolicies(t *testing.T) {
	// Without a configured token every control operation is denied
	// (read-only deployment), regardless of the client address; a
	// same-host reverse proxy must not become an unauthenticated control
	// channel.
	h := newAPIHarness(t, "")
	resp, _ := h.do("GET", "/api/config", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless config get = %d, want 403", resp.StatusCode)
	}
	resp, payload := h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, writeHdr())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless trigger = %d (%s), want 403", resp.StatusCode, errCodeOf(payload))
	}
	// The status page still works.
	resp, _ = h.do("GET", "/api/status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// With a token: bearer required for every control call.
	h = newAPIHarness(t, "s3cret")
	resp, _ = h.do("GET", "/api/config", nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("config get without bearer = %d", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/api/config", nil, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("config get with wrong bearer = %d", resp.StatusCode)
	}
	resp, payload = h.do("GET", "/api/config", nil, map[string]string{"Authorization": "Bearer s3cret"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config get = %d (%v)", resp.StatusCode, payload)
	}
	// Writes need both bearer and CSRF.
	resp, _ = h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, map[string]string{"Authorization": "Bearer s3cret"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write without csrf = %d", resp.StatusCode)
	}
	waitForRoundDone(t)
	resp, payload = h.do("POST", "/api/sync", map[string]any{"mode": "incremental"}, writeHdrWithToken("s3cret"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("write with bearer+csrf = %d (%v)", resp.StatusCode, payload)
	}
}

func TestAPIControlUnavailableWithoutController(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	cp := newControlPlane(nil, store, "")
	srv := httptest.NewServer(statusHTTPHandler(cp))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("config get without controller = %d", resp.StatusCode)
	}

	// The status page reports the control plane as unavailable.
	resp, err = http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if snap.Control.Available {
		t.Fatal("control reported available without a controller")
	}

	body := fmt.Sprintf(`{"mode":"incremental"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/api/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, csrfValue)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("sync without controller = %d", resp.StatusCode)
	}
}

func TestAPIErrorEnvelopeShape(t *testing.T) {
	h := newAPIHarness(t, "t0ken")
	resp, payload := h.do("POST", "/api/sync", map[string]any{"mode": "full-strict"}, writeHdrWithToken("t0ken"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	e, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing error envelope: %v", payload)
	}
	if e["code"] != errCodeConfirmRequired || e["message"] == "" {
		t.Fatalf("envelope wrong: %v", e)
	}
}
