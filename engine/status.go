package engine

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sync phases reported by the status page.
const (
	PhaseProbing     = "probing"
	PhaseManifest    = "downloading-manifest"
	PhaseParsing     = "parsing-manifest"
	PhaseDownloading = "downloading"
	PhaseComparing   = "comparing"
	PhasePreparing   = "preparing"
	PhaseCopying     = "copying"
	PhaseCleanup     = "cleanup"
	PhaseSleeping    = "sleeping"
	PhaseIdle        = "idle"
)

// Sync modes.
const (
	ModeManifest = "manifest"
	ModeCrawl    = "crawl"
)

// Mirror states derived from probe results.
const (
	mirrorStateFresh     = "fresh"
	mirrorStateStale     = "stale"
	mirrorStateCrawlOnly = "crawl-only"
	mirrorStateInvalid   = "invalid"
)

const (
	statusLogRingSize  = 1000
	statusHistoryLimit = 20
	// Ring memory bounds: no single record may exceed these field limits,
	// and the whole ring is evicted from the front beyond this budget so a
	// hostile manifest with huge paths cannot amplify memory usage.
	statusLogMaxFieldLen = 2048
	statusLogMaxBytes    = 2 << 20
	// statusMaxConcurrent bounds concurrent status page requests so slow
	// clients cannot exhaust the process.
	statusMaxConcurrent = 64
)

// mirrorStatus is one row of the mirror table on the status page.
type mirrorStatus struct {
	URL          string `json:"url"`
	State        string `json:"state"`
	LatencyMs    int64  `json:"latency_ms"`
	LastModified string `json:"last_modified,omitempty"`
}

// roundStatus summarizes one completed (or failed) sync round.
type roundStatus struct {
	StartedAt    time.Time `json:"started_at"`
	DurationMs   int64     `json:"duration_ms"`
	Mode         string    `json:"mode"`
	Downloaded   int       `json:"downloaded"`
	Deleted      int       `json:"deleted"`
	ShortCircuit bool      `json:"short_circuit"`
	Error        string    `json:"error,omitempty"`
}

// statusSnapshot is the JSON payload of /api/status. It is a deep copy
// taken under the status mutex, so handlers never hold the lock while
// serializing.
type statusSnapshot struct {
	Version        string     `json:"version"`
	Mode           string     `json:"mode"`
	Phase          string     `json:"phase"`
	PhaseStartedAt time.Time  `json:"phase_started_at"`
	RoundStartedAt time.Time  `json:"round_started_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	NextRunAt      *time.Time `json:"next_run_at,omitempty"`

	Manifest struct {
		LastModified string `json:"last_modified,omitempty"`
		Entries      int    `json:"entries"`
		ShortCircuit bool   `json:"short_circuit"`
	} `json:"manifest"`

	Download struct {
		Total      int `json:"total"`
		Planned    int `json:"planned"`
		Downloaded int `json:"downloaded"`
		Failed     int `json:"failed"`
		RetryRound int `json:"retry_round"`
	} `json:"download"`

	Cleanup struct {
		Enabled        bool `json:"enabled"`
		Deleted        int  `json:"deleted"`
		GuardTriggered bool `json:"guard_triggered"`
	} `json:"cleanup"`

	Mirrors []mirrorStatus `json:"mirrors"`
	History []roundStatus  `json:"history"`
}

// syncStatus is the process-wide status registry written by the sync code
// and read by the status HTTP handlers.
type syncStatus struct {
	mu sync.Mutex

	mode          string
	phase         string
	phaseStarted  time.Time
	roundStarted  time.Time
	roundDownload int
	roundDeleted  int
	nextRun       *time.Time

	manifestLastModified string
	manifestEntries      int
	manifestShortCircuit bool

	downloadTotal      int
	downloadPlanned    int
	downloadDownloaded int
	downloadFailed     int
	downloadRetryRound int

	cleanupEnabled        bool
	cleanupDeleted        int
	cleanupGuardTriggered bool

	mirrors []mirrorStatus
	history []roundStatus
}

// globalStatus is the single status registry of the process.
var globalStatus = &syncStatus{phase: PhaseIdle, mode: ModeManifest}

func (s *syncStatus) setPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
	s.phaseStarted = time.Now()
}

func (s *syncStatus) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}

func (s *syncStatus) roundStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roundStarted = time.Now()
	s.roundDownload = 0
	s.roundDeleted = 0
	s.manifestShortCircuit = false
	s.downloadTotal = 0
	s.downloadPlanned = 0
	s.downloadDownloaded = 0
	s.downloadFailed = 0
	s.downloadRetryRound = 0
	s.cleanupDeleted = 0
	s.cleanupGuardTriggered = false
}

func (s *syncStatus) roundEnd(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.roundStarted.IsZero() {
		return
	}
	r := roundStatus{
		StartedAt:    s.roundStarted,
		DurationMs:   time.Since(s.roundStarted).Milliseconds(),
		Mode:         s.mode,
		Downloaded:   s.roundDownload,
		Deleted:      s.roundDeleted,
		ShortCircuit: s.manifestShortCircuit,
	}
	if err != nil {
		r.Error = err.Error()
	}
	s.history = append([]roundStatus{r}, s.history...)
	if len(s.history) > statusHistoryLimit {
		s.history = s.history[:statusHistoryLimit]
	}
	s.roundStarted = time.Time{}
}

func (s *syncStatus) setNextRun(t *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun = t
}

func (s *syncStatus) setManifest(lastModified string, entries int, shortCircuit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifestLastModified = lastModified
	s.manifestEntries = entries
	s.manifestShortCircuit = shortCircuit
}

func (s *syncStatus) setDownloadPlan(total, planned int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadTotal = total
	s.downloadPlanned = planned
	s.downloadDownloaded = 0
	s.downloadFailed = 0
	s.downloadRetryRound = 0
}

func (s *syncStatus) incDownloaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadDownloaded++
	s.roundDownload++
}

func (s *syncStatus) incFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadFailed++
}

func (s *syncStatus) setRetryRound(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.downloadRetryRound = n
}

func (s *syncStatus) setCleanupEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupEnabled = enabled
}

func (s *syncStatus) addDeleted(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupDeleted += n
	s.roundDeleted += n
}

func (s *syncStatus) setCleanupGuard(triggered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupGuardTriggered = triggered
}

func (s *syncStatus) setMirrors(mirrors []mirrorStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mirrors = mirrors
}

func (s *syncStatus) snapshot() statusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := statusSnapshot{
		Version:        Version,
		Mode:           s.mode,
		Phase:          s.phase,
		PhaseStartedAt: s.phaseStarted,
		RoundStartedAt: s.roundStarted,
		UpdatedAt:      time.Now(),
		NextRunAt:      s.nextRun,
	}
	snap.Manifest.LastModified = s.manifestLastModified
	snap.Manifest.Entries = s.manifestEntries
	snap.Manifest.ShortCircuit = s.manifestShortCircuit
	snap.Download.Total = s.downloadTotal
	snap.Download.Planned = s.downloadPlanned
	snap.Download.Downloaded = s.downloadDownloaded
	snap.Download.Failed = s.downloadFailed
	snap.Download.RetryRound = s.downloadRetryRound
	snap.Cleanup.Enabled = s.cleanupEnabled
	snap.Cleanup.Deleted = s.cleanupDeleted
	snap.Cleanup.GuardTriggered = s.cleanupGuardTriggered
	if s.roundStarted.IsZero() && len(s.history) > 0 {
		snap.RoundStartedAt = s.history[0].StartedAt
	}
	snap.Mirrors = append([]mirrorStatus(nil), s.mirrors...)
	snap.History = append([]roundStatus(nil), s.history...)
	return snap
}

// logRecord is one captured log line of the status page.
type logRecord struct {
	Seq   int64  `json:"seq"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"`
}

// ringLogHandler wraps another slog.Handler and keeps the last
// statusLogRingSize formatted records in memory for /api/logs, while the
// wrapped handler keeps writing to the real destination.
type ringLogHandler struct {
	inner slog.Handler

	mu      sync.Mutex
	records []logRecord
	nextSeq int64
	bytes   int
}

func newRingLogHandler(inner slog.Handler) *ringLogHandler {
	return &ringLogHandler{inner: inner}
}

func (h *ringLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

var logURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

func redactLogString(s string) string {
	return logURLPattern.ReplaceAllStringFunc(s, sanitizeURL)
}

func redactLogAttr(a slog.Attr) string {
	value := a.Value.String()
	key := strings.ToLower(a.Key)
	if key == "mirror" || strings.Contains(key, "url") || strings.Contains(key, "endpoint") {
		if sanitized := sanitizeURL(value); sanitized != "invalid-url" {
			return sanitized
		}
	}
	return redactLogString(value)
}

// truncateRunes cuts s to at most limit bytes without splitting a rune.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if len(s)-cut <= 3 {
		// Avoid producing a suffix shorter than the marker.
		cut -= 3
		if cut < 0 {
			cut = 0
		}
	}
	return s[:cut] + "..."
}

func (h *ringLogHandler) Handle(ctx context.Context, r slog.Record) error {
	var attrs strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.MessageKey {
			return true
		}
		if attrs.Len() > 0 {
			attrs.WriteByte(' ')
		}
		attrs.WriteString(a.Key)
		attrs.WriteByte('=')
		attrs.WriteString(redactLogAttr(a))
		return true
	})
	rec := logRecord{
		Time:  r.Time.Format(time.RFC3339),
		Level: r.Level.String(),
		Msg:   truncateRunes(redactLogString(r.Message), statusLogMaxFieldLen),
		Attrs: truncateRunes(attrs.String(), statusLogMaxFieldLen),
	}

	h.mu.Lock()
	h.nextSeq++
	rec.Seq = h.nextSeq
	h.records = append(h.records, rec)
	h.bytes += len(rec.Msg) + len(rec.Attrs) + 64
	drop := func() {
		oldest := h.records[0]
		h.bytes -= len(oldest.Msg) + len(oldest.Attrs) + 64
		h.records = h.records[1:]
	}
	for len(h.records) > statusLogRingSize && len(h.records) > 1 {
		drop()
	}
	for h.bytes > statusLogMaxBytes && len(h.records) > 1 {
		drop()
	}
	h.mu.Unlock()

	return h.inner.Handle(ctx, r)
}

func (h *ringLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringLogHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *ringLogHandler) WithGroup(name string) slog.Handler {
	return &ringLogHandler{inner: h.inner.WithGroup(name)}
}

// logsAfter returns the records with sequence numbers greater than after.
func (h *ringLogHandler) logsAfter(after int64) []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return nil
	}
	idx := 0
	for idx < len(h.records) && h.records[idx].Seq <= after {
		idx++
	}
	if idx >= len(h.records) {
		return nil
	}
	return append([]logRecord(nil), h.records[idx:]...)
}

// statusRingLog is the process-wide ring buffer installed by installRingLogHandler.
var statusRingLog *ringLogHandler

// installRingLogHandler wraps the current default logger so every log line
// is also captured for the status page. It is idempotent.
func installRingLogHandler() {
	if statusRingLog != nil {
		return
	}
	base := slog.Default().Handler()
	statusRingLog = newRingLogHandler(base)
	slog.SetDefault(slog.New(statusRingLog))
}

//go:embed web/index.html
var statusWebFS embed.FS

// statusHTTPHandler builds the HTTP routes of the status page.
func statusHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(globalStatus.snapshot())
	})
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		var records []logRecord
		if statusRingLog != nil {
			records = statusRingLog.logsAfter(after)
		}
		if records == nil {
			records = []logRecord{}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body, err := statusWebFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "status page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
	return mux
}

// limitConcurrency bounds the number of in-flight status page requests;
// excess requests receive 503 instead of tying up process resources.
func limitConcurrency(h http.Handler) http.Handler {
	sem := make(chan struct{}, statusMaxConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			h.ServeHTTP(w, r)
		default:
			http.Error(w, "server busy", http.StatusServiceUnavailable)
		}
	})
}

// startStatusServer serves the status page on addr until ctx is done. The
// returned channel is closed once the server has fully shut down.
func startStatusServer(ctx context.Context, addr string) <-chan struct{} {
	done := make(chan struct{})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("Failed to start status page server", "addr", addr, "error", err)
		close(done)
		return done
	}

	srv := &http.Server{
		Handler:           limitConcurrency(statusHTTPHandler()),
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	slog.Info("Status page is available", "addr", "http://"+ln.Addr().String())

	go func() {
		defer close(done)
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("Status page server failed", "error", err)
		}
	}()
	return done
}

// formatDuration renders d like "1h02m03s" or "4.5s" for the status page.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return d.Truncate(time.Millisecond * 100).String()
	}
}
