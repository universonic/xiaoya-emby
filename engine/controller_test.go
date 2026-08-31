package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestController builds a controller with a stubbed round runner over a
// throwaway download dir (so pending-state probes hit a fresh database).
type controllerHarness struct {
	ctrl    *Controller
	store   *SettingsStore
	started atomic.Int64 // rounds started
	skip    atomic.Bool  // when set, new rounds do not block
	gatesMu sync.Mutex
	gates   []*roundGate
}

type roundGate struct {
	ch   chan struct{}
	once sync.Once
}

func (g *roundGate) open() { g.once.Do(func() { close(g.ch) }) }

func newControllerHarness(t *testing.T, settings SyncSettings) *controllerHarness {
	t.Helper()
	dir := t.TempDir()
	store := NewSettingsStore(settings, dir)
	cfg := &Config{DownloadDir: dir}
	h := &controllerHarness{}
	ctrl := NewController(cfg, store)
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		h.started.Add(1)
		if !h.skip.Load() {
			g := &roundGate{ch: make(chan struct{})}
			h.gatesMu.Lock()
			h.gates = append(h.gates, g)
			h.gatesMu.Unlock()
			<-g.ch
		}
		return OutcomeSuccess, nil
	}
	h.ctrl = ctrl
	h.store = store
	return h
}

// release unblocks every started round and lets later rounds run freely.
func (h *controllerHarness) release() {
	h.skip.Store(true)
	h.gatesMu.Lock()
	defer h.gatesMu.Unlock()
	for _, g := range h.gates {
		g.open()
	}
}

// waitForRoundDone waits until the controller finished at least one round
// and is idle (robust against very short rounds and shared status state).
func waitForRoundDone(t *testing.T) {
	t.Helper()
	waitForControllerIdle(t, nil)
}

func waitForControllerIdle(t *testing.T, ctrl *Controller) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		if ctrl != nil {
			return !ctrl.Busy() && ctrl.Completed() >= 1
		}
		snap := globalStatus.snapshot()
		return !snap.Running && len(snap.History) >= 1
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestRunSyncRoundRetryPolicy pins the trigger-based retry contract without
// sleeping through production backoff intervals.
func TestRunSyncRoundRetryPolicy(t *testing.T) {
	permanent := errors.New("permanent")
	attempts := 0
	err := retryRound(context.Background(), true, 0, func() error {
		attempts++
		return permanent
	})
	if !errors.Is(err, permanent) || attempts != 1+maxAutomaticRoundRetries {
		t.Fatalf("automatic permanent failure = %v after %d attempts", err, attempts)
	}

	attempts = 0
	err = retryRound(context.Background(), true, 0, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary")
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("automatic recovery = %v after %d attempts", err, attempts)
	}

	for name, automaticErr := range map[string]error{
		"manual":   permanent,
		"deferred": errManifestPending,
	} {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			err := retryRound(context.Background(), name != "manual", 0, func() error {
				attempts++
				return automaticErr
			})
			if !errors.Is(err, automaticErr) || attempts != 1 {
				t.Fatalf("%s failure = %v after %d attempts", name, err, attempts)
			}
		})
	}
}

func TestControllerFailedAutomaticRoundSchedulesNextRun(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	ctrl := NewController(&Config{DownloadDir: dir}, NewSettingsStore(validSettings(), dir))
	wantNext := time.Now().Add(time.Hour)
	ctrl.nextCronTime = func(expr string, now time.Time) (time.Time, error) { return wantNext, nil }
	var attempts atomic.Int64
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		err := retryRound(ctx, true, 0, func() error {
			attempts.Add(1)
			return errors.New("permanent")
		})
		return OutcomeFailed, err
	}
	ctrl.Start()
	defer ctrl.Stop()
	waitForControllerIdle(t, ctrl)

	snap := globalStatus.snapshot()
	if attempts.Load() != 1+maxAutomaticRoundRetries {
		t.Fatalf("attempts = %d, want %d", attempts.Load(), 1+maxAutomaticRoundRetries)
	}
	if len(snap.History) != 1 || snap.History[0].Outcome != OutcomeFailed {
		t.Fatalf("history = %+v, want one failed round", snap.History)
	}
	if snap.NextRunAt == nil || !snap.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at = %v, want %v", snap.NextRunAt, wantNext)
	}
}

func TestRunSyncRoundReportsMediaPhases(t *testing.T) {
	t.Run("comparing", func(t *testing.T) {
		resetGlobalStatus()
		downloadDir := t.TempDir()
		db := openTestDB(t, downloadDir)
		if err := createFileTable(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		path := "/电影/bad.strm"
		if err := insertTestRow(context.Background(), db, &MetadataFile{path: path, name: "bad.strm"}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(downloadDir, rootRel(path)), dirPerm); err != nil {
			t.Fatal(err)
		}
		s := validSettings()
		s.RunMode, s.Purge = modeBitMedia, false
		err := (&Config{DownloadDir: downloadDir, MediaDir: t.TempDir()}).runSyncRoundOnce(context.Background(), s, SyncTypeIncremental, 0)
		if err == nil || globalStatus.snapshot().Phase != PhaseComparing {
			t.Fatalf("compare failure = %v, phase = %q", err, globalStatus.snapshot().Phase)
		}
	})

	t.Run("preparing", func(t *testing.T) {
		resetGlobalStatus()
		mediaPath := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(mediaPath, []byte("x"), filePerm); err != nil {
			t.Fatal(err)
		}
		s := validSettings()
		s.RunMode, s.Purge = modeBitMedia, false
		err := (&Config{DownloadDir: t.TempDir(), MediaDir: mediaPath}).runSyncRoundOnce(context.Background(), s, SyncTypeIncremental, 0)
		if err == nil || globalStatus.snapshot().Phase != PhasePreparing {
			t.Fatalf("prepare failure = %v, phase = %q", err, globalStatus.snapshot().Phase)
		}
	})

	t.Run("copying", func(t *testing.T) {
		resetGlobalStatus()
		s := validSettings()
		s.RunMode, s.Purge = modeBitMedia, false
		err := (&Config{DownloadDir: t.TempDir(), MediaDir: t.TempDir()}).runSyncRoundOnce(context.Background(), s, SyncTypeIncremental, 0)
		if err != nil || globalStatus.snapshot().Phase != PhaseCopying {
			t.Fatalf("copy phase = %v/%q", err, globalStatus.snapshot().Phase)
		}
	})
}

func TestControllerStartupRunsImmediatelyAndCronSchedules(t *testing.T) {
	resetGlobalStatus()
	h := newControllerHarness(t, validSettings())
	h.ctrl.nextCronTime = func(expr string, t time.Time) (time.Time, error) {
		return t.Add(time.Hour), nil
	}
	h.ctrl.Start()
	defer h.ctrl.Stop()

	waitFor(t, 5*time.Second, func() bool { return h.started.Load() == 1 })
	// While the startup job blocks, no cron timer fires a second job.
	time.Sleep(100 * time.Millisecond)
	if h.started.Load() != 1 {
		t.Fatalf("concurrent rounds started: %d", h.started.Load())
	}

	h.release()
	waitFor(t, 5*time.Second, func() bool {
		snap := globalStatus.snapshot()
		return snap.NextRunAt != nil
	})
	// The finished job released the controller; a manual trigger works now.
	if _, _, err := h.ctrl.Trigger(SyncTypeIncremental, false); err != nil {
		t.Fatalf("trigger after idle rejected: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return h.started.Load() == 2 })
	h.release()
}

func TestControllerBusyReturnsConflict(t *testing.T) {
	resetGlobalStatus()
	h := newControllerHarness(t, validSettings())
	h.ctrl.Start()
	defer h.ctrl.Stop()

	waitFor(t, 5*time.Second, func() bool { return h.started.Load() == 1 })
	_, _, err := h.ctrl.Trigger(SyncTypeIncremental, false)
	if !errors.Is(err, errBusy) {
		t.Fatalf("trigger while busy = %v, want errBusy", err)
	}
	// Abort is idempotent and reports the cancel request.
	accepted, cancelRequested, err := h.ctrl.Abort()
	if err != nil || !accepted || !cancelRequested {
		t.Fatalf("abort = %v/%v/%v", accepted, cancelRequested, err)
	}
	accepted, cancelRequested, err = h.ctrl.Abort()
	if err != nil || !accepted || !cancelRequested {
		t.Fatalf("repeat abort = %v/%v/%v", accepted, cancelRequested, err)
	}
	h.release()

	// Without an active job abort is a conflict.
	waitFor(t, 5*time.Second, func() bool { return h.started.Load() == 1 && globalStatus.snapshot().Running == false })
	_, _, err = h.ctrl.Abort()
	if err == nil {
		t.Fatal("abort without an active job succeeded")
	}
}

func TestControllerStopCancelsAndWaitsForActiveRound(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	ctrl := NewController(&Config{DownloadDir: dir}, NewSettingsStore(validSettings(), dir))
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return OutcomeCanceled, ctx.Err()
	}
	ctrl.Start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("startup round did not start")
	}

	stopped := make(chan struct{})
	go func() {
		ctrl.Stop()
		close(stopped)
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the active round")
	}
	if _, _, err := ctrl.Trigger(SyncTypeIncremental, false); !errors.Is(err, errControlUnavailable) {
		t.Fatalf("trigger during shutdown = %v, want errControlUnavailable", err)
	}
	if _, _, err := ctrl.Abort(); !errors.Is(err, errControlUnavailable) {
		t.Fatalf("abort during shutdown = %v, want errControlUnavailable", err)
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before the active round exited")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the active round exited")
	}
}

func TestControllerStopCannotCrossBlockedJobLaunch(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	ctrl := NewController(&Config{DownloadDir: dir}, NewSettingsStore(validSettings(), dir))
	ctrl.maintenanceMu.Lock()
	var started atomic.Int64
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		started.Add(1)
		return OutcomeSuccess, nil
	}
	ctrl.Start()

	stopped := make(chan struct{})
	go func() {
		ctrl.Stop()
		close(stopped)
	}()
	waitFor(t, 5*time.Second, ctrl.stopping.Load)
	ctrl.maintenanceMu.Unlock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete after releasing blocked launch")
	}
	if started.Load() != 0 {
		t.Fatal("sync job started after shutdown began")
	}
}

func TestControllerTriggerPublishesStartingStatusAtomically(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	ctrl := NewController(&Config{DownloadDir: dir}, store)
	manualStarted := make(chan struct{})
	releaseManual := make(chan struct{})
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		if trigger == TriggerManual {
			close(manualStarted)
			<-releaseManual
		}
		return OutcomeSuccess, nil
	}
	ctrl.Start()
	defer ctrl.Stop()
	waitForControllerIdle(t, ctrl)

	// Seed values from the previous idle round. Trigger must replace all of
	// them before making the new job visible as running.
	globalStatus.setPhase(PhaseSleeping)
	globalStatus.setDownloadPlan(10, 4)
	if _, effective, err := ctrl.Trigger(SyncTypeFullRelaxed, false); err != nil {
		t.Fatal(err)
	} else if effective != SyncTypeFullRelaxed {
		t.Fatalf("effective mode = %q, want %q", effective, SyncTypeFullRelaxed)
	}
	select {
	case <-manualStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("manual round did not start")
	}

	snap := globalStatus.snapshot()
	if !snap.Running || snap.Phase != PhaseStarting {
		t.Fatalf("initial job status = running:%v phase:%q", snap.Running, snap.Phase)
	}
	if snap.SyncType != SyncTypeFullRelaxed || snap.Download.Total != 0 || snap.Download.Planned != 0 {
		t.Fatalf("initial job snapshot retained previous round state: %+v", snap)
	}

	close(releaseManual)
	waitFor(t, 5*time.Second, func() bool { return !globalStatus.snapshot().Running })
}

func TestControllerTriggerValidations(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	cfg := &Config{DownloadDir: dir}
	ctrl := NewController(cfg, store)
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		return OutcomeSuccess, nil
	}
	ctrl.Start()
	defer ctrl.Stop()
	waitForRoundDone(t)

	// Unknown type.
	if _, _, err := ctrl.Trigger("weird", false); err == nil {
		t.Fatal("unknown sync type accepted")
	}
	// Strict without confirmation.
	if _, _, err := ctrl.Trigger(SyncTypeFullStrict, false); !errors.Is(err, errConfirm) {
		t.Fatalf("strict without confirm = %v", err)
	}
	// Full types require the download bit.
	noDownload := validSettings()
	noDownload.RunMode = 1
	s, rev := store.Snapshot()
	noDownload.RunMode = modeBitMedia
	if _, _, err := store.Update(rev, noDownload); err != nil {
		t.Fatal(err)
	}
	_ = s
	if _, _, err := ctrl.Trigger(SyncTypeFullStrict, true); !errors.Is(err, errModeConflict) {
		t.Fatalf("strict without download bit = %v, want errModeConflict", err)
	}
	if _, _, err := ctrl.Trigger(SyncTypeFullRelaxed, false); !errors.Is(err, errModeConflict) {
		t.Fatalf("relaxed without download bit = %v, want errModeConflict", err)
	}
	// Incremental is accepted (the round itself is stubbed).
	jobID, effective, err := ctrl.Trigger(SyncTypeIncremental, false)
	if err != nil || jobID == "" || effective != SyncTypeIncremental {
		t.Fatalf("incremental trigger = %q/%q/%v", jobID, effective, err)
	}
	waitFor(t, 5*time.Second, func() bool { return globalStatus.snapshot().Running == false })
}

func TestControllerPendingRecoveryOverridesAndPauses(t *testing.T) {
	resetGlobalStatus()
	dir := t.TempDir()
	store := NewSettingsStore(validSettings(), dir)
	cfg := &Config{DownloadDir: dir}

	// Seed a pending full rebuild state.
	db, err := openMetadataDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := createMetaTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := createFullTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &fullSyncState{
		Version: fullStateVersion, SyncID: "fs-test", InventoryRunID: "inv-test",
		Mode: fullModeStrict, Phase: fullPhaseDownloading,
		Roots: []string{"电影"},
	}
	if err := writeFullSyncStateDB(ctx, db, st); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var effectiveSeen atomic.Value
	ctrl := NewController(cfg, store)
	ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		snap := globalStatus.snapshot()
		effectiveSeen.Store(snap.SyncType)
		return OutcomeSuccess, nil
	}
	ctrl.Start()
	defer ctrl.Stop()
	waitForRoundDone(t)

	// Any trigger is overridden by the pending recovery mode.
	_, effective, err := ctrl.Trigger(SyncTypeIncremental, false)
	if err != nil {
		t.Fatal(err)
	}
	if effective != SyncTypeFullStrict {
		t.Fatalf("effective mode = %q, want pending full-strict", effective)
	}
	waitFor(t, 5*time.Second, func() bool { return globalStatus.snapshot().Running == false })
	if got := effectiveSeen.Load(); got != SyncTypeFullStrict {
		t.Fatalf("round ran with sync type %v", got)
	}

	// Disabling the download stage pauses the recovery and blocks manual
	// triggers with a typed conflict.
	settings, rev := store.Snapshot()
	settings.RunMode = modeBitMedia
	if _, _, err := store.Update(rev, settings); err != nil {
		t.Fatal(err)
	}
	_, _, err = ctrl.Trigger(SyncTypeIncremental, false)
	if !errors.Is(err, errRecoveryPaused) {
		t.Fatalf("paused trigger = %v, want errRecoveryPaused", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		snap := globalStatus.snapshot()
		return snap.PendingRecovery && snap.RecoveryPaused
	})
}

func TestControllerRoundUsesSettingsSnapshot(t *testing.T) {
	resetGlobalStatus()
	h := newControllerHarness(t, validSettings())
	var (
		revSeen atomic.Int64
		ready   = make(chan struct{})
		first   = int64(0)
	)
	var gate = &roundGate{ch: make(chan struct{})}
	h.ctrl.runRound = func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
		revSeen.Store(revision)
		if s.DownloadWorkers != 4 {
			return OutcomeFailed, errors.New("unexpected snapshot")
		}
		close(ready)
		<-gate.ch
		return OutcomeSuccess, nil
	}
	h.ctrl.Start()
	defer h.ctrl.Stop()
	<-ready

	// Editing settings while the round runs must not affect the running
	// snapshot (verified above) and bumps the revision for the next round.
	next, rev := h.store.Snapshot()
	next.DownloadWorkers = 9
	if _, _, err := h.store.Update(rev, next); err != nil {
		t.Fatal(err)
	}
	gate.open()
	waitFor(t, 5*time.Second, func() bool { return globalStatus.snapshot().Running == false })
	if revSeen.Load() != first {
		t.Fatalf("round revision = %d, want %d", revSeen.Load(), first)
	}
}

func TestControllerCronReschedulesOnSettingsUpdate(t *testing.T) {
	resetGlobalStatus()
	h := newControllerHarness(t, validSettings())
	var (
		mu    sync.Mutex
		exprs []string
	)
	h.ctrl.nextCronTime = func(expr string, t time.Time) (time.Time, error) {
		mu.Lock()
		exprs = append(exprs, expr)
		mu.Unlock()
		return t.Add(time.Hour), nil
	}
	h.ctrl.Start()
	defer h.ctrl.Stop()

	h.release()
	waitFor(t, 5*time.Second, func() bool { return globalStatus.snapshot().NextRunAt != nil })

	// A cron change while idle reschedules immediately with the new expr.
	next, rev := h.store.Snapshot()
	next.RunCron = "*/5 * * * *"
	if _, _, err := h.store.Update(rev, next); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(exprs) >= 2 && exprs[len(exprs)-1] == "*/5 * * * *"
	})
}

func TestPendingFullSyncProbeOnFreshDir(t *testing.T) {
	dir := t.TempDir()
	st, err := probePendingFullSync(filepath.Join(dir, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatalf("fresh dir reported pending state %+v", st)
	}
}
