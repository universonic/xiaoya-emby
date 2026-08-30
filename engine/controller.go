package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	cron "github.com/robfig/cron/v3"
)

// errRecoveryPaused reports a pending full rebuild whose recovery cannot
// run because the download stage is disabled in the current run mode.
var errRecoveryPaused = errors.New("pending full rebuild recovery is paused: the download stage is disabled")

// roundError carries the phase and legacy exit code of a failed phase so
// the non-daemon path can exit with the same codes as before.
type roundError struct {
	code  int
	phase string
	err   error
}

func (e *roundError) Error() string {
	if e.phase == "" {
		return e.err.Error()
	}
	return e.phase + ": " + e.err.Error()
}

func (e *roundError) Unwrap() error { return e.err }

func asRoundError(err error) *roundError {
	var re *roundError
	if errors.As(err, &re) {
		return re
	}
	return nil
}

// Job is one sync round: it owns a context, a fixed settings snapshot and
// its identity. Only one job exists at a time.
type Job struct {
	ID              string
	Trigger         string
	Settings        SyncSettings
	Revision        int64
	CancelRequested bool

	ctx    context.Context
	cancel context.CancelFunc
}

// controllerEvent is one message processed by the controller loop.
type controllerEvent struct {
	kind     string // evTrigger | evAbort | evReschedule | evJobDone
	syncType string
	confirm  bool
	job      *Job
	outcome  string
	err      error
	resp     chan controllerReply
}

// controllerReply answers synchronous API calls.
type controllerReply struct {
	err             error
	accepted        bool
	jobID           string
	effectiveMode   string
	cancelRequested bool
}

type controllerEventKind string

const (
	evTrigger    = "trigger"
	evAbort      = "abort"
	evReschedule = "reschedule"
	evJobDone    = "jobdone"
)

// Controller serializes all sync work: startup runs, cron schedules,
// manual triggers, aborts and settings reschedules flow through a single
// event loop, and at most one job runs at any time.
type Controller struct {
	cfg   *Config
	store *SettingsStore

	events chan controllerEvent
	quit   chan struct{}
	done   chan struct{}

	current *Job
	jobSeq  atomic.Int64
	timer   *time.Timer
	// maintenanceMu serializes sync jobs with idle staging GC. A job holds
	// it for its entire lifetime; GC only takes a short, bounded idle lease.
	maintenanceMu sync.Mutex
	busy          atomic.Bool  // a job is currently running (test/observability)
	rounds        atomic.Int64 // completed rounds (test/observability)

	// runRound executes one sync round; it is injectable for tests.
	runRound func(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error)
	// nextCronTime is injectable for tests.
	nextCronTime func(expr string, t time.Time) (time.Time, error)
}

// NewController wires the controller to the settings store; the store's
// update callback is registered so cron changes reschedule immediately.
func NewController(cfg *Config, store *SettingsStore) *Controller {
	c := &Controller{
		cfg:      cfg,
		store:    store,
		events:   make(chan controllerEvent, 16),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		timer:    time.NewTimer(time.Duration(1<<62 - 1)),
		runRound: cfg.runSyncRound,
		nextCronTime: func(expr string, t time.Time) (time.Time, error) {
			sche, err := cron.ParseStandard(expr)
			if err != nil {
				return time.Time{}, err
			}
			return sche.Next(t), nil
		},
	}
	store.SetOnUpdate(func() {
		select {
		case c.events <- controllerEvent{kind: evReschedule}:
		case <-c.quit:
		default:
			// The loop is busy; the reschedule is re-evaluated when the
			// current event (or job) finishes.
		}
	})
	return c
}

func (c *Controller) postEvent(ev controllerEvent) bool {
	select {
	case c.events <- ev:
		return true
	case <-c.quit:
		return false
	}
}

// Start launches the event loop. In daemon mode the first round starts
// immediately; the next one is scheduled by cron once the job completes.
func (c *Controller) Start() {
	go c.loop()
}

// Stop terminates the loop and cancels any running job.
func (c *Controller) Stop() {
	select {
	case <-c.quit:
		return
	default:
		close(c.quit)
	}
	<-c.done
}

func (c *Controller) loop() {
	defer close(c.done)
	c.startJob(TriggerStartup, SyncTypeIncremental)
	for {
		select {
		case ev := <-c.events:
			c.handleEvent(ev)
		case <-c.timer.C:
			// The timer only fires while idle (it is reset whenever a job
			// finishes or the schedule changes).
			c.startJob(TriggerCron, SyncTypeIncremental)
		case <-c.quit:
			if c.current != nil {
				c.current.cancel()
			}
			return
		}
	}
}

func (c *Controller) handleEvent(ev controllerEvent) {
	switch ev.kind {
	case evTrigger:
		reply := c.handleTrigger(ev.syncType, ev.confirm)
		if ev.resp != nil {
			ev.resp <- reply
		}
	case evAbort:
		reply := c.handleAbort()
		if ev.resp != nil {
			ev.resp <- reply
		}
	case evReschedule:
		if c.current == nil {
			c.scheduleNextCron()
		}
	case evJobDone:
		c.handleJobDone(ev)
	}
}

// Busy reports whether a job is currently running; Completed returns the
// number of finished rounds. Intended for tests and monitoring.
func (c *Controller) Busy() bool       { return c.busy.Load() }
func (c *Controller) Completed() int64 { return c.rounds.Load() }

// Trigger requests a new sync round. Busy conflicts, mode conflicts and a
// paused recovery return typed errors for the API layer.
func (c *Controller) Trigger(syncType string, confirm bool) (jobID, effectiveMode string, err error) {
	resp := make(chan controllerReply, 1)
	if !c.postEvent(controllerEvent{kind: evTrigger, syncType: syncType, confirm: confirm, resp: resp}) {
		return "", "", errControlUnavailable
	}
	reply := <-resp
	return reply.jobID, reply.effectiveMode, reply.err
}

// Abort requests cooperative cancellation of the running job. Aborting
// without an active job is a conflict; repeat aborts report the current
// state idempotently.
func (c *Controller) Abort() (accepted bool, cancelRequested bool, err error) {
	resp := make(chan controllerReply, 1)
	if !c.postEvent(controllerEvent{kind: evAbort, resp: resp}) {
		return false, false, errControlUnavailable
	}
	reply := <-resp
	return reply.accepted, reply.cancelRequested, reply.err
}

var errControlUnavailable = errors.New("control plane unavailable")

// Sentinel errors surfaced by the API with typed status codes.
var (
	errBusy         = errors.New("a sync job is already running")
	errModeConflict = errors.New("the full rebuild modes require the download stage to be enabled")
	errConfirm      = errors.New("strict full rebuild requires confirmation")
	errNoActiveJob  = errors.New("no active sync job")
)

func (c *Controller) handleTrigger(syncType string, confirm bool) controllerReply {
	switch syncType {
	case SyncTypeIncremental, SyncTypeFullRelaxed, SyncTypeFullStrict:
	default:
		return controllerReply{err: fmt.Errorf("unknown sync type %q", syncType)}
	}
	if c.current != nil {
		return controllerReply{err: fmt.Errorf("%w (%s)", errBusy, c.current.ID)}
	}
	settings, _ := c.store.Snapshot()
	pending, err := probePendingFullSync(c.cfg.DownloadDir)
	if err != nil {
		return controllerReply{err: fmt.Errorf("cannot inspect pending full sync state: %w", err)}
	}
	paused := pending != nil && !settings.DownloadEnabled()
	globalStatus.setPending(pending != nil, paused)
	if paused {
		return controllerReply{err: fmt.Errorf("%w", errRecoveryPaused)}
	}
	if (syncType == SyncTypeFullRelaxed || syncType == SyncTypeFullStrict) && !settings.DownloadEnabled() {
		return controllerReply{err: errModeConflict}
	}
	if syncType == SyncTypeFullStrict && !confirm {
		return controllerReply{err: errConfirm}
	}
	effective := syncType
	if pending != nil {
		effective = pending.syncType()
	}
	job := c.launchJob(syncType, effective, TriggerManual, settings)
	return controllerReply{accepted: true, jobID: job.ID, effectiveMode: effective}
}

func (c *Controller) handleAbort() controllerReply {
	if c.current == nil {
		return controllerReply{err: errNoActiveJob}
	}
	j := c.current
	if !j.CancelRequested {
		j.CancelRequested = true
		globalStatus.setCancelRequested(true)
		j.cancel()
		slog.Info("Cancel requested for sync job", "job", j.ID)
	}
	return controllerReply{accepted: true, cancelRequested: true, jobID: j.ID}
}

// startJob launches a job with the current settings snapshot unless one is
// already running (used by the internal startup/cron triggers).
func (c *Controller) startJob(trigger, syncType string) {
	if c.current != nil {
		slog.Warn("Skipping scheduled sync trigger: a job is already running", "trigger", trigger)
		c.scheduleNextCron()
		return
	}
	settings, _ := c.store.Snapshot()
	pending, err := probePendingFullSync(c.cfg.DownloadDir)
	effective := syncType
	if err != nil {
		slog.Error("Cannot inspect pending full sync state", "error", err)
	}
	if pending != nil {
		if !settings.DownloadEnabled() {
			// Recovery paused: log, record the round as deferred and wait
			// for the next schedule.
			globalStatus.setPending(true, true)
			globalStatus.roundStart()
			globalStatus.roundEndWith(fmt.Errorf("%w", errRecoveryPaused), OutcomeDeferred)
			slog.Warn("Scheduled sync skipped: pending full rebuild recovery is paused (download stage disabled)")
			c.scheduleNextCron()
			return
		}
		effective = pending.syncType()
	}
	c.launchJob(syncType, effective, trigger, settings)
}

func (c *Controller) launchJob(requested, effective, trigger string, settings SyncSettings) *Job {
	// Serialize job staging with orphan GC. Idle GC is time-bounded, so a
	// trigger can wait at most its short maintenance lease here.
	c.maintenanceMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		ID:       fmt.Sprintf("job-%d", c.jobSeq.Add(1)),
		Trigger:  trigger,
		Settings: settings.Clone(),
		Revision: c.store.Revision(),
		ctx:      ctx,
		cancel:   cancel,
	}
	c.current = j
	c.busy.Store(true)
	globalStatus.startJob(trigger, effective, j.ID)
	slog.Info("Sync job started", "job", j.ID, "trigger", trigger, "requested", requested, "effective", effective)
	go func() {
		defer c.maintenanceMu.Unlock()
		outcome, err := c.runRound(j.ctx, j.Settings, requested, trigger, j.Revision)
		c.postEvent(controllerEvent{kind: evJobDone, job: j, outcome: outcome, err: err})
	}()
	return j
}

func (c *Controller) handleJobDone(ev controllerEvent) {
	j := ev.job
	if c.current != j {
		return
	}
	c.current = nil
	c.busy.Store(false)
	c.rounds.Add(1)
	globalStatus.roundEndWith(ev.err, ev.outcome)
	globalStatus.clearJob()
	j.cancel()
	slog.Info("Sync job finished", "job", j.ID, "trigger", j.Trigger, "outcome", ev.outcome, "error", ev.err)
	// Idle window: batched GC of orphaned staging tables. GC and job staging
	// share maintenanceMu, eliminating the check/delete race. The lease is
	// capped at one second; leftovers are collected on a later idle window.
	if ev.outcome != OutcomeCanceled {
		go func() {
			if !c.maintenanceMu.TryLock() {
				return
			}
			defer c.maintenanceMu.Unlock()
			if c.busy.Load() {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := sweepOrphanFullInventory(ctx, c.cfg.DownloadDir); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				slog.Warn("Failed to sweep orphaned full-sync staging", "error", err)
			}
		}()
	}
	c.scheduleNextCron()
}

// scheduleNextCron arms the cron timer from the newest settings snapshot.
func (c *Controller) scheduleNextCron() {
	settings, _ := c.store.Snapshot()
	next, err := c.nextCronTime(settings.RunCron, time.Now())
	if err != nil {
		slog.Error("Invalid cron expression in settings; retrying scheduling in one minute", "error", err)
		next = time.Now().Add(time.Minute)
	}
	d := time.Until(next)
	if !c.timer.Stop() {
		select {
		case <-c.timer.C:
		default:
		}
	}
	c.timer.Reset(d)
	globalStatus.setNextRun(&next)
	globalStatus.setPhase(PhaseSleeping)
	slog.Info("Next task will be started", "at", next.Format(time.RFC3339), "wait", d.Round(time.Second))
}

// runSyncRound executes one complete round (pending resolution, optional
// download phase, optional media compare/copy) with the retry policy of
// its trigger: automatic daemon rounds retry critical phase errors after
// 5s (cancellation-aware); manual and non-daemon rounds end on the first
// phase failure; deferred conditions end the round for every trigger type.
//
// Pending recovery is re-probed before every attempt: when a previous
// attempt completed the full rebuild but failed a later phase, the retry
// falls back to the requested (typically incremental) mode instead of
// starting a brand-new destructive rebuild.
func (cfg *Config) runSyncRound(ctx context.Context, s SyncSettings, requested, trigger string, revision int64) (string, error) {
	var lastErr error
	for {
		pending, err := probePendingFullSync(cfg.DownloadDir)
		if err != nil {
			slog.Error("Cannot inspect pending full sync state", "error", err)
			lastErr = &roundError{code: 2, phase: "download", err: err}
		} else {
			paused := pending != nil && !s.DownloadEnabled()
			globalStatus.setPending(pending != nil, paused)
			if paused {
				return OutcomeDeferred, errRecoveryPaused
			} else {
				effective := requested
				if pending != nil {
					effective = pending.syncType()
					if requested != effective {
						slog.Info("Requested sync mode is overridden by pending full rebuild recovery", "requested", requested, "effective", effective)
					}
				}
				globalStatus.setSyncType(effective)
				lastErr = cfg.runSyncRoundOnce(ctx, s, effective, revision)
			}
		}
		if lastErr == nil || ctx.Err() != nil || isDeferredErr(lastErr) {
			break
		}
		if trigger == TriggerManual || !cfg.RunAsDaemon {
			break
		}
		slog.Error("Critical error; the automatic round will retry in 5s", "error", lastErr)
		if serr := sleepContext(ctx, 5*time.Second); serr != nil {
			lastErr = serr
			break
		}
	}
	switch {
	case lastErr == nil:
		return OutcomeSuccess, nil
	case ctx.Err() != nil || errors.Is(lastErr, context.Canceled):
		return OutcomeCanceled, context.Canceled
	case isDeferredErr(lastErr):
		return OutcomeDeferred, lastErr
	default:
		return OutcomeFailed, lastErr
	}
}

// runSyncRoundOnce runs every phase exactly once.
func (cfg *Config) runSyncRoundOnce(ctx context.Context, s SyncSettings, syncType string, revision int64) error {
	var remote []*MetadataFile
	if s.DownloadEnabled() {
		crawler, err := NewMetadataCrawler(ctx, cfg.DownloadDir, s)
		if err != nil {
			return &roundError{code: 2, phase: "download", err: err}
		}
		globalStatus.setSyncRoots(crawler.selectedPaths)
		probeCtx, probeCancel := context.WithCancel(ctx)
		go crawler.Run(probeCtx)
		var syncErr error
		switch syncType {
		case SyncTypeFullRelaxed:
			syncErr = crawler.SyncFull(ctx, fullModeRelaxed, revision)
		case SyncTypeFullStrict:
			// Strict clearing RemoveAlls the sync roots below the
			// download dir; refuse when the media library sits inside
			// one of them (misconfiguration must never delete media).
			if err := checkMediaOverlap(cfg.MediaDir, cfg.DownloadDir, crawler.selectedPaths); err != nil {
				probeCancel()
				return &roundError{code: 2, phase: "download", err: err}
			}
			syncErr = crawler.SyncFull(ctx, fullModeStrict, revision)
		default:
			syncErr = crawler.Sync(ctx)
		}
		probeCancel()
		if syncErr != nil {
			return &roundError{code: 2, phase: "download", err: syncErr}
		}
		if !s.MediaEnabled() {
			slog.Info("Finished download phase; the media stage is disabled by run mode")
			return nil
		}
		remote, err = crawler.LocalFiles()
		if err != nil {
			return &roundError{code: 2, phase: "download", err: err}
		}
	} else {
		crawler := &MetadataCrawler{downloadDir: cfg.DownloadDir}
		var err error
		remote, err = crawler.LocalFiles()
		if err != nil {
			return &roundError{code: 2, phase: "download", err: err}
		}
		slog.Info("Skipped metadata download.")
	}
	if !s.MediaEnabled() {
		return nil
	}

	filesToPreserve, err := cfg.compareMetadata(ctx, s, remote)
	if err != nil {
		return &roundError{code: 126, phase: "compare", err: err}
	}
	slog.Info("Metadata files to sync", "count", len(filesToPreserve))

	filesNeedUpdate, err := cfg.prepareMetadataUpdate(ctx, s, filesToPreserve)
	if err != nil {
		return &roundError{code: 127, phase: "prepare", err: err}
	}
	slog.Info("Files need to be updated", "count", len(filesNeedUpdate))

	if err := cfg.syncMetadata(ctx, s, filesNeedUpdate); err != nil {
		return &roundError{code: 128, phase: "copy", err: err}
	}
	return nil
}
