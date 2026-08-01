package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// Defaults for an Engine created with New.
const (
	// DefaultWorkers is the job concurrency of whitepaper §10.2. Chunks within
	// one file stay serial: upstream's TempLocation+offset protocol makes no
	// promise about out-of-order writes, and §10.2 requires that be measured
	// before it is relied on. Throughput comes from running several files at
	// once, which needs no such promise.
	DefaultWorkers = 4
	// DefaultMaxAttempts bounds how many times the engine re-runs a job whose
	// failure the classifier called temporary.
	DefaultMaxAttempts = 5
	// verifyTimeout bounds the wait for an upload to become visible (D44).
	verifyTimeout = 30 * time.Second
)

// ErrNotFound is returned for an unknown job id.
var ErrNotFound = errors.New("no such job")

// Engine runs transfers from a queue with a bounded number of workers.
type Engine struct {
	client   *opendrive.Client
	store    Store
	log      *slog.Logger
	workers  int
	attempts int
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	// verify makes an uploaded file's visibility check overridable in tests.
	verify func(context.Context, string) error

	mu      sync.Mutex
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc
	queue   chan string

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
	done      chan struct{}
}

// Option configures an Engine.
type Option func(*Engine)

// WithWorkers sets the job concurrency.
func WithWorkers(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.workers = n
		}
	}
}

// WithStore supplies the persistence backend (MemoryStore when unset).
func WithStore(s Store) Option {
	return func(e *Engine) {
		if s != nil {
			e.store = s
		}
	}
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// WithMaxAttempts bounds re-runs of a job the classifier called temporary.
func WithMaxAttempts(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.attempts = n
		}
	}
}

// withClock and withSleep make the retry ladder testable without waiting.
func withClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

func withSleep(f func(context.Context, time.Duration) error) Option {
	return func(e *Engine) {
		if f != nil {
			e.sleep = f
		}
	}
}

// New creates an Engine. It does not start any workers; call Start.
func New(c *opendrive.Client, opts ...Option) (*Engine, error) {
	if c == nil {
		return nil, fmt.Errorf("the job engine needs a client")
	}
	e := &Engine{
		client:   c,
		store:    NewMemoryStore(),
		log:      slog.New(discardHandler{}),
		workers:  DefaultWorkers,
		attempts: DefaultMaxAttempts,
		now:      time.Now,
		sleep:    sleepCtx,
		jobs:     map[string]*Job{},
		cancels:  map[string]context.CancelFunc{},
		done:     make(chan struct{}),
	}
	for _, o := range opts {
		o(e)
	}
	e.queue = make(chan string, 1024)
	if e.verify == nil {
		e.verify = e.waitUntilVisible
	}
	return e, nil
}

// Start launches the worker pool. It returns immediately.
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		for i := 0; i < e.workers; i++ {
			e.wg.Add(1)
			go e.worker(ctx)
		}
	})
}

// Stop stops accepting work and waits for the running jobs to notice. Jobs
// still in flight are cancelled, which reclaims their upstream records.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		close(e.done)
		e.mu.Lock()
		cancels := make([]context.CancelFunc, 0, len(e.cancels))
		for _, c := range e.cancels {
			cancels = append(cancels, c)
		}
		e.mu.Unlock()
		for _, c := range cancels {
			c()
		}
		e.wg.Wait()
	})
}

// Submit queues a transfer and returns the job as accepted.
func (e *Engine) Submit(spec Spec) (*Job, error) {
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	now := e.now()
	j := &Job{
		ID:         newJobID(),
		Kind:       spec.Kind,
		State:      StateQueued,
		LocalPath:  spec.LocalPath,
		RemotePath: spec.RemotePath,
		BytesTotal: spec.Size,
		CreatedAt:  now,
		UpdatedAt:  now,
		Spec:       spec,
	}

	// Persist before publishing. Once the job is in e.jobs and on the queue a
	// worker owns it, and anything that still reads it here would be racing
	// that worker.
	if err := e.store.Save(j); err != nil {
		// Persisting is what makes recovery possible, so failing to do it is
		// worth refusing the job over rather than discovering after a crash.
		return nil, fmt.Errorf("cannot persist the job: %w", err)
	}

	e.mu.Lock()
	e.jobs[j.ID] = j
	accepted := j.Clone()
	e.mu.Unlock()

	select {
	case e.queue <- j.ID:
	default:
		e.mu.Lock()
		delete(e.jobs, j.ID)
		e.mu.Unlock()
		_ = e.store.Delete(j.ID)
		return nil, fmt.Errorf("the job queue is full")
	}
	return accepted, nil
}

func validateSpec(spec *Spec) error {
	switch spec.Kind {
	case KindUpload:
		if spec.LocalPath == "" {
			return fmt.Errorf("an upload needs a local path")
		}
		if spec.Name == "" {
			spec.Name = filepath.Base(spec.LocalPath)
		}
	case KindDownload:
		if spec.FileID == "" {
			return fmt.Errorf("a download needs a file id")
		}
		if spec.LocalPath == "" {
			return fmt.Errorf("a download needs a local path")
		}
	default:
		return fmt.Errorf("unknown job kind %q", spec.Kind)
	}
	return nil
}

// Get returns a copy of one job.
func (e *Engine) Get(id string) (*Job, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return j.Clone(), nil
}

// List returns every job the engine knows about, newest last.
func (e *Engine) List() []*Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Job, 0, len(e.jobs))
	for _, j := range e.jobs {
		out = append(out, j.Clone())
	}
	sortJobs(out)
	return out
}

// Cancel stops a job. A queued job never starts; a running one is interrupted.
//
// Either way the upstream record the transfer may have created is reclaimed
// before the job reaches StateCancelled, so cancelling a bulk upload cannot
// leave the destination folder full of invisible records (D39).
func (e *Engine) Cancel(id string) error {
	e.mu.Lock()
	j, ok := e.jobs[id]
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	if j.State.Terminal() {
		e.mu.Unlock()
		return nil
	}
	cancel := e.cancels[id]
	wasRunning := j.State == StateRunning
	var snapshot *Job
	if !wasRunning {
		// A queued job has no worker to notice, so it is finished here.
		j.State = StateCancelled
		j.UpdatedAt = e.now()
		j.Speed = 0
		snapshot = j.Clone()
	}
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if !wasRunning {
		// Nothing has run, so there is usually nothing upstream — but a job
		// requeued after a crash may carry a record from its previous life.
		e.reclaim(context.Background(), snapshot)
		return e.persist(id)
	}
	return nil
}

// Recover reloads persisted jobs after a restart and requeues the ones that
// were interrupted.
//
// A job caught mid-transfer is the case that matters: it may have left an
// upstream record behind. The record id was persisted before the first byte
// was sent, so recovery can reclaim it and start the transfer cleanly, which is
// the only honest option — upstream's chunk protocol offers no way to ask "how
// much of this file do you already hold?" without sending a chunk first, so
// byte-level resume across a restart is not available for uploads. Downloads
// resume properly, because the answer is on the local disk.
func (e *Engine) Recover(ctx context.Context) error {
	stored, err := e.store.Load()
	if err != nil {
		// Report it, but recover what could be read: losing the readable jobs
		// as well would leak their records too.
		e.log.Error("some persisted jobs could not be read",
			slog.String("error", err.Error()))
	}

	var requeued int
	for _, j := range stored {
		if j.State.Terminal() {
			e.mu.Lock()
			e.jobs[j.ID] = j
			e.mu.Unlock()
			continue
		}

		// Anything not terminal was interrupted.
		if j.Kind == KindUpload && j.UpstreamFileID != "" {
			e.reclaim(ctx, j)
			j.UpstreamFileID, j.UpstreamTempLocation = "", ""
			j.BytesDone = 0
		}
		j.State = StateQueued
		j.Speed = 0
		j.UpdatedAt = e.now()

		e.mu.Lock()
		e.jobs[j.ID] = j
		e.mu.Unlock()
		if saveErr := e.store.Save(j); saveErr != nil {
			e.log.Error("cannot persist a recovered job",
				slog.String("job", j.ID), slog.String("error", saveErr.Error()))
		}
		select {
		case e.queue <- j.ID:
			requeued++
		default:
			return fmt.Errorf("the job queue is full while recovering")
		}
	}
	if requeued > 0 {
		e.log.Info("requeued interrupted jobs", slog.Int("count", requeued))
	}
	return nil
}

// ---------------------------------------------------------------- the worker

func (e *Engine) worker(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-e.done:
			return
		case <-ctx.Done():
			return
		case id := <-e.queue:
			e.run(ctx, id)
		}
	}
}

// run executes one job, retrying while the classification layer says the
// failure is temporary and the budget allows.
func (e *Engine) run(parent context.Context, id string) {
	for {
		ctx, cancel := context.WithCancel(parent)

		e.mu.Lock()
		j, ok := e.jobs[id]
		if !ok || j.State.Terminal() {
			e.mu.Unlock()
			cancel()
			return
		}
		j.State = StateRunning
		j.Attempts++
		j.Error = nil
		j.UpdatedAt = e.now()
		attempt := j.Attempts
		e.cancels[id] = cancel
		spec := j.Spec
		e.mu.Unlock()
		_ = e.persist(id)

		err := e.transfer(ctx, id, spec)

		e.mu.Lock()
		delete(e.cancels, id)
		e.mu.Unlock()
		cancel()

		if err == nil {
			e.finish(id, StateSucceeded, nil)
			return
		}

		// Cancellation is a decision, not a failure, and it is the caller's.
		if e.cancelled(id) || errors.Is(err, context.Canceled) {
			e.reclaimByID(context.WithoutCancel(parent), id)
			e.finish(id, StateCancelled, nil)
			return
		}

		// Everything below is the classifier's verdict, copied rather than
		// second-guessed (docs/error-taxonomy.md).
		if !opendrive.IsTemporary(err) || attempt >= e.maxAttempts(err) {
			e.reclaimByID(context.WithoutCancel(parent), id)
			e.finish(id, StateFailed, err)
			return
		}

		delay := opendrive.RetryAfter(err)
		if delay <= 0 {
			delay = backoff(attempt)
		}
		e.log.Info("retrying a transfer",
			slog.String("job", id), slog.Int("attempt", attempt),
			slog.Duration("delay", delay),
			slog.String("kind", string(opendrive.ErrorKind(err))))

		e.mu.Lock()
		if j, ok := e.jobs[id]; ok {
			j.State = StateQueued
			j.Error = newError(err)
			j.Speed = 0
			j.UpdatedAt = e.now()
		}
		e.mu.Unlock()
		_ = e.persist(id)

		if sleepErr := e.sleep(parent, delay); sleepErr != nil {
			e.reclaimByID(context.WithoutCancel(parent), id)
			e.finish(id, StateCancelled, nil)
			return
		}
	}
}

// maxAttempts honours a per-error budget when the classifier set one: an
// ambiguous refusal that has only weak evidence behind it is worth one more
// attempt, not five (docs/error-taxonomy.md T2).
func (e *Engine) maxAttempts(err error) int {
	var ae *opendrive.APIError
	if errors.As(err, &ae) {
		if b := ae.RetryBudget(); b > 0 && b < e.attempts {
			return b
		}
	}
	return e.attempts
}

func (e *Engine) transfer(ctx context.Context, id string, spec Spec) error {
	switch spec.Kind {
	case KindUpload:
		return e.runUpload(ctx, id, spec)
	case KindDownload:
		return e.runDownload(ctx, id, spec)
	default:
		return fmt.Errorf("unknown job kind %q", spec.Kind)
	}
}

func (e *Engine) runUpload(ctx context.Context, id string, spec Spec) error {
	tracker := newSpeed(e.now)

	_, err := e.client.Uploads().UploadFile(ctx, spec.LocalPath, opendrive.UploadParams{
		FolderID:     spec.FolderID,
		Name:         spec.Name,
		OpenIfExists: spec.Overwrite,
		// The engine owns the record's lifetime: it persists the id so a crash
		// can reclaim it, and reclaims it itself on every terminal path. The
		// SDK deleting it underneath would take the id away before it could be
		// recorded.
		KeepOnFailure: true,
		OnRecordCreated: func(fileID, tempLocation string) {
			e.mu.Lock()
			if j, ok := e.jobs[id]; ok {
				j.UpstreamFileID = fileID
				j.UpstreamTempLocation = tempLocation
				j.UpdatedAt = e.now()
			}
			e.mu.Unlock()
			// Persisted before a single byte moves, so a crash one moment
			// later still knows what to reclaim (D39).
			_ = e.persist(id)
		},
		Progress: func(sent, total int64) {
			e.progress(id, sent, total, tracker)
		},
	})
	if err != nil {
		return err
	}

	// D44: the file is not always visible to the download endpoint the instant
	// close_file_upload returns. Waiting for it here is deliberate — a 404 is
	// not a retryable error and must not be taught to the classifier as one, so
	// the wait lives where the context is known instead.
	e.mu.Lock()
	fileID := ""
	if j, ok := e.jobs[id]; ok {
		fileID = j.UpstreamFileID
	}
	e.mu.Unlock()
	if fileID != "" {
		if err := e.verify(ctx, fileID); err != nil {
			return err
		}
	}

	// The transfer is complete and verified, so the record is no longer an
	// orphan candidate.
	e.mu.Lock()
	if j, ok := e.jobs[id]; ok {
		j.UpstreamFileID, j.UpstreamTempLocation = "", ""
	}
	e.mu.Unlock()
	return nil
}

func (e *Engine) runDownload(ctx context.Context, id string, spec Spec) error {
	tracker := newSpeed(e.now)

	if dir := filepath.Dir(spec.LocalPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
	}

	// A partial file from an earlier attempt is the resume point, and
	// DownloadFile finds it from the file itself.
	var already int64
	if info, statErr := os.Stat(spec.LocalPath); statErr == nil && !info.IsDir() {
		already = info.Size()
	}

	_, err := e.client.Downloads().DownloadFile(ctx, spec.LocalPath, opendrive.DownloadParams{
		FileID: spec.FileID,
		Size:   spec.Size,
		Hash:   spec.Hash,
		Progress: func(written, total int64) {
			e.progress(id, already+written, total, tracker)
		},
	})
	return err
}

// waitUntilVisible polls the cheap test=1 probe until upstream admits an
// uploaded file exists (D44).
func (e *Engine) waitUntilVisible(ctx context.Context, fileID string) error {
	deadline := e.now().Add(verifyTimeout)
	for {
		_, err := e.client.Downloads().Probe(ctx, fileID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, opendrive.ErrNotFound) {
			return err
		}
		if e.now().After(deadline) {
			// The Kind stays the classifier's, so the reported code is the
			// stable enum as always. The sentence is ours because the condition
			// is ours: this is a timeout the engine owns, and upstream's own
			// "File does not exist" would tell a user their just-uploaded file
			// was never there. Describing what actually happened is the point
			// of the rule, not an exception to it.
			return &opendrive.APIError{
				Kind:        opendrive.ErrorKind(err),
				Op:          "POST " + string(KindUpload),
				UpstreamMsg: fmt.Sprintf("the upload completed but upstream did not make the file visible within %s", verifyTimeout),
				Err:         err,
			}
		}
		if sleepErr := e.sleep(ctx, time.Second); sleepErr != nil {
			return sleepErr
		}
	}
}

// ---------------------------------------------------------------- bookkeeping

func (e *Engine) progress(id string, done, total int64, tracker *speed) {
	e.mu.Lock()
	if j, ok := e.jobs[id]; ok {
		j.BytesDone = done
		if total > 0 {
			j.BytesTotal = total
		}
		j.Speed = tracker.observe(done)
		j.UpdatedAt = e.now()
	}
	e.mu.Unlock()
}

func (e *Engine) finish(id string, state State, err error) {
	e.mu.Lock()
	if j, ok := e.jobs[id]; ok {
		j.State = state
		j.Speed = 0
		j.Error = newError(err)
		j.UpdatedAt = e.now()
		if state == StateSucceeded && j.BytesTotal > 0 {
			j.BytesDone = j.BytesTotal
		}
	}
	e.mu.Unlock()
	_ = e.persist(id)
}

func (e *Engine) cancelled(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	return ok && j.State == StateCancelled
}

func (e *Engine) persist(id string) error {
	e.mu.Lock()
	j, ok := e.jobs[id]
	var snapshot *Job
	if ok {
		snapshot = j.Clone()
	}
	e.mu.Unlock()
	if snapshot == nil {
		return nil
	}
	if err := e.store.Save(snapshot); err != nil {
		e.log.Error("cannot persist job state",
			slog.String("job", id), slog.String("error", err.Error()))
		return err
	}
	return nil
}

func (e *Engine) reclaimByID(ctx context.Context, id string) {
	e.mu.Lock()
	j, ok := e.jobs[id]
	var snapshot *Job
	if ok {
		snapshot = j.Clone()
	}
	e.mu.Unlock()
	e.reclaim(ctx, snapshot)

	e.mu.Lock()
	if j, ok := e.jobs[id]; ok {
		j.UpstreamFileID, j.UpstreamTempLocation = "", ""
	}
	e.mu.Unlock()
}

// reclaim removes the upstream record a half-finished upload created.
//
// It runs on every terminal path — cancellation, permanent failure, exhausted
// retries and recovery — because that is the only way the guarantee holds. An
// Overwrite job is exempt: create_file may have handed back a file that already
// existed, which was never ours to delete.
func (e *Engine) reclaim(ctx context.Context, j *Job) {
	if j == nil || j.UpstreamFileID == "" || j.Spec.Overwrite {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := e.client.Uploads().Reclaim(ctx, j.UpstreamFileID, "", ""); err != nil {
		e.log.Warn("could not reclaim the record left by a stopped upload",
			slog.String("job", j.ID), slog.String("file_id", j.UpstreamFileID),
			slog.String("error", err.Error()))
		return
	}
	e.log.Debug("reclaimed the record left by a stopped upload",
		slog.String("job", j.ID), slog.String("file_id", j.UpstreamFileID))
}

// ---------------------------------------------------------------- small parts

// speed reports a smoothed transfer rate in bytes per second.
type speed struct {
	now       func() time.Time
	start     time.Time
	lastAt    time.Time
	lastBytes int64
	rate      float64
}

func newSpeed(now func() time.Time) *speed {
	t := now()
	return &speed{now: now, start: t, lastAt: t}
}

// observe folds a new byte count into an exponentially weighted rate, which
// keeps a progress display from flickering on every chunk.
func (s *speed) observe(done int64) float64 {
	t := s.now()
	elapsed := t.Sub(s.lastAt).Seconds()
	if elapsed <= 0 {
		return s.rate
	}
	instant := float64(done-s.lastBytes) / elapsed
	if instant < 0 {
		instant = 0
	}
	const alpha = 0.3
	if s.rate == 0 {
		s.rate = instant
	} else {
		s.rate = alpha*instant + (1-alpha)*s.rate
	}
	s.lastAt, s.lastBytes = t, done
	return s.rate
}

// backoff is the ladder of §10.1: 1s, 2s, 4s … capped at a minute.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << (attempt - 1) //nolint:gosec // attempt is bounded by maxAttempts
	if d > time.Minute || d <= 0 {
		return time.Minute
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newJobID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func sortJobs(js []*Job) {
	for i := 1; i < len(js); i++ {
		for k := i; k > 0 && js[k].CreatedAt.Before(js[k-1].CreatedAt); k-- {
			js[k], js[k-1] = js[k-1], js[k]
		}
	}
}

// discardHandler is a slog.Handler that drops everything.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
