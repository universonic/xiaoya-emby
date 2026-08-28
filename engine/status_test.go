package engine

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func resetGlobalStatus() {
	globalStatus = &syncStatus{phase: PhaseIdle, mode: ModeManifest}
}

func TestRingLogHandlerCapturesAndPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := newRingLogHandler(inner)
	log := slog.New(h)
	log.Info("downloading file", "path", "/电影/a.nfo", "mirror", "http://user:secret@m1/?token=abc")
	log.Warn("stale manifest")
	log.Error("boom", "code", 500)

	if !strings.Contains(buf.String(), "downloading file") || !strings.Contains(buf.String(), "code=500") {
		t.Fatalf("inner handler did not receive records: %q", buf.String())
	}

	recs := h.logsAfter(0)
	if len(recs) != 3 {
		t.Fatalf("captured %d records, want 3", len(recs))
	}
	if recs[0].Level != "INFO" || recs[0].Msg != "downloading file" {
		t.Fatalf("unexpected first record: %+v", recs[0])
	}
	if !strings.Contains(recs[0].Attrs, "path=/电影/a.nfo") || !strings.Contains(recs[0].Attrs, "mirror=http://m1/") {
		t.Fatalf("attrs not captured: %q", recs[0].Attrs)
	}
	if strings.Contains(recs[0].Attrs, "secret") || strings.Contains(recs[0].Attrs, "token") {
		t.Fatalf("credentials leaked into ring: %q", recs[0].Attrs)
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			t.Fatalf("sequence not monotonic: %v", recs)
		}
	}

	if again := h.logsAfter(recs[2].Seq); again != nil {
		t.Fatalf("logsAfter(latest) = %v, want nil", again)
	}
	if mid := h.logsAfter(recs[0].Seq); len(mid) != 2 {
		t.Fatalf("logsAfter(first) returned %d records, want 2", len(mid))
	}
}

func TestRingLogHandlerRingSize(t *testing.T) {
	h := newRingLogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	log := slog.New(h)
	for i := 0; i < statusLogRingSize+50; i++ {
		log.Info("line", "i", i)
	}
	recs := h.logsAfter(0)
	if len(recs) != statusLogRingSize {
		t.Fatalf("ring holds %d records, want %d", len(recs), statusLogRingSize)
	}
	if want := int64(statusLogRingSize + 50); recs[len(recs)-1].Seq != want {
		t.Fatalf("last seq = %d, want %d", recs[len(recs)-1].Seq, want)
	}
}

func TestStatusSnapshotLifecycle(t *testing.T) {
	resetGlobalStatus()
	s := globalStatus

	s.setMode(ModeManifest)
	s.roundStart()
	s.setPhase(PhaseDownloading)
	s.setManifest("Tue, 25 Aug 2026 13:00:00 GMT", 700390, false)
	s.setDownloadPlan(700390, 4)
	s.incDownloaded()
	s.incDownloaded()
	s.incFailed()
	s.setRetryRound(1)
	s.setCleanupEnabled(true)
	s.addDeleted(3)
	s.setCleanupGuard(true)
	next := time.Now().Add(time.Hour)
	s.setNextRun(&next)
	s.setMirrors([]mirrorStatus{{URL: "http://m1/", State: mirrorStateFresh, LatencyMs: 12, LastModified: "x"}})
	s.roundEnd(nil)

	snap := s.snapshot()
	if snap.Mode != ModeManifest || snap.Phase != PhaseDownloading {
		t.Fatalf("mode/phase = %s/%s", snap.Mode, snap.Phase)
	}
	if snap.Download.Planned != 4 || snap.Download.Downloaded != 2 || snap.Download.Failed != 1 || snap.Download.RetryRound != 1 {
		t.Fatalf("download counters wrong: %+v", snap.Download)
	}
	if snap.Cleanup.Deleted != 3 || !snap.Cleanup.GuardTriggered {
		t.Fatalf("cleanup counters wrong: %+v", snap.Cleanup)
	}
	if len(snap.History) != 1 || snap.History[0].Downloaded != 2 || snap.History[0].Deleted != 3 {
		t.Fatalf("history wrong: %+v", snap.History)
	}
	if len(snap.Mirrors) != 1 || snap.Mirrors[0].State != mirrorStateFresh {
		t.Fatalf("mirrors wrong: %+v", snap.Mirrors)
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"phase"`, `"downloaded"`, `"guard_triggered"`, `"next_run_at"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("json payload missing %s: %s", key, b)
		}
	}

	s.roundEnd(nil) // second end without start must be a no-op
	if s.snapshot().History == nil || len(s.snapshot().History) != 1 {
		t.Fatalf("unexpected history after double roundEnd: %+v", s.snapshot().History)
	}
}

func TestStatusHTTPHandlers(t *testing.T) {
	resetGlobalStatus()
	globalStatus.setPhase(PhaseDownloading)
	globalStatus.setDownloadPlan(10, 5)

	h := newRingLogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	log := slog.New(h)
	log.Info("hello", "path", "/x")
	statusRingLog = h
	defer func() { statusRingLog = nil }()

	srv := httptest.NewServer(statusHTTPHandler(nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var snap statusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if snap.Phase != PhaseDownloading || snap.Download.Planned != 5 {
		t.Fatalf("status payload wrong: %+v", snap)
	}

	resp, err = http.Get(srv.URL + "/api/logs?after=0")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Records []logRecord `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(payload.Records) != 1 || payload.Records[0].Msg != "hello" {
		t.Fatalf("logs payload wrong: %+v", payload.Records)
	}

	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(body.String(), "同步状态") {
		t.Fatal("index page content missing")
	}

	resp, err = http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", resp.StatusCode)
	}
}

func TestRingLogHandlerTruncatesAndBoundedBytes(t *testing.T) {
	h := newRingLogHandler(slog.NewTextHandler(&bytes.Buffer{}, nil))
	log := slog.New(h)
	long := strings.Repeat("é", statusLogMaxFieldLen) // 2 bytes per rune
	log.Info(long, "attrs", strings.Repeat("a", statusLogMaxFieldLen*2))

	recs := h.logsAfter(0)
	if len(recs) != 1 {
		t.Fatalf("captured %d records, want 1", len(recs))
	}
	if len(recs[0].Msg) > statusLogMaxFieldLen+3 || len(recs[0].Attrs) > statusLogMaxFieldLen+3 {
		t.Fatalf("fields not truncated: msg=%d attrs=%d", len(recs[0].Msg), len(recs[0].Attrs))
	}
	for _, r := range recs {
		if !utf8.ValidString(r.Msg) || !utf8.ValidString(r.Attrs) {
			t.Fatal("truncation split a rune")
		}
	}

	// Byte budget: many large records must not grow the ring unboundedly.
	for i := 0; i < statusLogRingSize; i++ {
		log.Info(strings.Repeat("x", statusLogMaxFieldLen))
	}
	h.mu.Lock()
	size := h.bytes
	count := len(h.records)
	h.mu.Unlock()
	if count > statusLogRingSize {
		t.Fatalf("ring count %d exceeds limit", count)
	}
	if size > statusLogMaxBytes+statusLogMaxFieldLen+128 {
		t.Fatalf("ring bytes %d exceeds budget", size)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		1500 * time.Millisecond: "1.5s",
		90 * time.Second:        "1m30s",
		3725 * time.Second:      "1h02m05s",
		-1 * time.Second:        "0s",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
