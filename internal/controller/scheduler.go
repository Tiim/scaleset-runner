package controller

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"

	"github.com/tim/scaleset-runner/internal/config"
	"github.com/tim/scaleset-runner/internal/store"
)

// scaleSet is the in-memory state of one configured repository's Scale Set.
// All fields below log are guarded by Controller.mu.
type scaleSet struct {
	repo  config.Repo
	image string
	log   *slog.Logger

	dbID     int64
	ghID     int
	served   bool // has an open message session
	unserved string
	lastPoll time.Time

	assigned int // assigned Job count from the latest statistics
	pollSeq  int // number of statistics received
	holdSeq  int // no new start requests while pollSeq <= holdSeq (FR-035)

	runners     map[string]*runner
	queued      int
	scalingDown bool

	jobs jobStats
}

func newScaleSet(r config.Repo, image string, log *slog.Logger) *scaleSet {
	return &scaleSet{
		repo:    r,
		image:   image,
		log:     log.With("repository", r.Repository, "scale_set", r.Name),
		runners: map[string]*runner{},
		holdSeq: -1,
	}
}

// active counts the Runner Containers that serve or will serve a Job:
// everything but those being removed, dropped while starting, or whose Job
// completed (an ephemeral runner never takes a second Job).
func (ss *scaleSet) active() int {
	n := 0
	for _, r := range ss.runners {
		if r.completed {
			continue
		}
		switch r.phase {
		case phasePreparing:
			if !r.dropped {
				n++
			}
		case phaseIdle, phaseBusy:
			n++
		}
	}
	return n
}

// startRequest is a Scale Set's need for one more Runner Container.
type startRequest struct {
	ss        *scaleSet
	seq       uint64 // order of arising, kept across retries (BR-004)
	attempt   int
	notBefore time.Time
}

type phase int

const (
	phasePreparing phase = iota // JIT Config, create, start in progress
	phaseIdle                   // started; no Job known to run on it
	phaseBusy                   // JobStarted arrived, or GitHub said it holds a Job
	phaseRemoving               // being removed; still holds its Slot
)

// l returns the runner's logger, which gains the Job ID from JobStarted on.
func (r *runner) l() *slog.Logger { return r.logp.Load() }

func (p phase) String() string {
	return [...]string{"preparing", "idle", "busy", "removing"}[p]
}

// runner is one Runner Container and its Runner Registration. Fields are
// guarded by Controller.mu.
type runner struct {
	name   string
	ss     *scaleSet
	req    *startRequest
	logp   atomic.Pointer[slog.Logger]
	ctx    context.Context
	cancel context.CancelFunc

	phase      phase
	dropped    bool // start request dropped while preparing
	releasing  bool // a deletion of its registration is in flight
	regDeleted bool
	ready      bool
	exited     bool
	completed  bool
	observed   bool // job duration recorded

	runnerID    int64 // GitHub runner ID
	regDBID     int64
	containerID string
	contDBID    int64
	jit         string
	jobID       string
	jobStart    time.Time
}

// ---- Slots and Capacity ----

func (c *Controller) usedLocked() int {
	n := c.leftovers
	for _, ss := range c.sets {
		n += len(ss.runners)
	}
	return n
}

func (c *Controller) freeLocked() int {
	return max(0, c.cfg.Slots-c.usedLocked())
}

// capacity is what a Scale Set advertises: its own Runner Containers plus
// the free Slots (FR-009).
func (c *Controller) capacity(ss *scaleSet) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ss.active() + c.freeLocked()
}

// ---- Demand ----

// handleMessage applies one poll's Job events and statistics. Callers must
// have acquired the offered Jobs already.
func (c *Controller) handleMessage(ss *scaleSet, msg *scaleset.RunnerScaleSetMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, js := range msg.JobStartedMessages {
		r := ss.runners[js.RunnerName]
		if r == nil {
			ss.log.Debug("JobStarted for an unknown runner", "runner", js.RunnerName, "job_id", js.JobID)
			continue
		}
		if r.jobID == js.JobID {
			continue // redelivered message
		}
		r.jobID = js.JobID
		r.jobStart = c.now()
		r.logp.Store(r.l().With("job_id", js.JobID))
		if r.phase == phasePreparing || r.phase == phaseIdle {
			r.phase = phaseBusy
		}
		if r.contDBID != 0 {
			c.storeErr(c.st.SetContainerJob(r.contDBID, js.JobID))
		}
		r.l().Info("job started", "workflow_run_id", js.WorkflowRunID, "job", js.JobDisplayName)
	}
	for _, jc := range msg.JobCompletedMessages {
		r := ss.runners[jc.RunnerName]
		if r == nil || r.completed {
			continue
		}
		r.completed = true
		if r.jobID == "" {
			r.jobID = jc.JobID
			r.logp.Store(r.l().With("job_id", jc.JobID))
		}
		r.l().Info("job completed", "result", jc.Result)
		c.observeJobLocked(r)
		// The ephemeral runner exits by itself; remove it anyway if it lingers.
		time.AfterFunc(c.completedGrace, func() {
			c.mu.Lock()
			if r.phase == phaseRemoving {
				c.mu.Unlock()
				return
			}
			r.phase = phaseRemoving
			c.mu.Unlock()
			r.l().Warn("runner container still running after its job completed; removing it")
			c.finish(r, nil)
		})
	}
	if msg.Statistics != nil {
		ss.assigned = msg.Statistics.TotalAssignedJobs
		ss.pollSeq++
	}
	c.reconcileLocked(ss)
}

// reconcileLocked compares a Scale Set's assigned Job count with its Runner
// Containers and waiting start requests, adds or drops start requests, and
// starts removing surplus Idle Runner Containers.
func (c *Controller) reconcileLocked(ss *scaleSet) {
	if c.shuttingDown {
		return
	}
	have := ss.active() + ss.queued
	switch want := ss.assigned; {
	case want > have:
		if ss.pollSeq <= ss.holdSeq {
			break // FR-035: wait for fresh statistics after an unexpected exit
		}
		for range want - have {
			c.seq++
			c.enqueueLocked(&startRequest{ss: ss, seq: c.seq})
		}
		ss.log.Info("start requests made", "assigned_jobs", want, "runner_containers", ss.active(), "waiting", ss.queued)
	case want < have:
		if drop := min(ss.queued, have-want); drop > 0 {
			c.dropQueuedLocked(ss, drop)
			ss.log.Info("start requests dropped", "count", drop, "assigned_jobs", want)
		}
		if ss.active() > want && !ss.scalingDown {
			ss.scalingDown = true
			go c.scaleDown(ss)
		}
	}
	c.dispatchLocked()
}

func (c *Controller) enqueueLocked(req *startRequest) {
	i, _ := slices.BinarySearchFunc(c.queue, req.seq, func(r *startRequest, seq uint64) int {
		switch {
		case r.seq < seq:
			return -1
		case r.seq > seq:
			return 1
		}
		return 0
	})
	c.queue = slices.Insert(c.queue, i, req)
	req.ss.queued++
}

// dropQueuedLocked drops the n newest waiting start requests of ss; the
// order of the others is kept (UC-008 BR-003).
func (c *Controller) dropQueuedLocked(ss *scaleSet, n int) {
	for i := len(c.queue) - 1; i >= 0 && n > 0; i-- {
		if c.queue[i].ss == ss {
			c.queue = slices.Delete(c.queue, i, i+1)
			ss.queued--
			n--
		}
	}
}

// dispatchLocked gives free Slots to waiting start requests, earliest first
// across all Scale Sets (FR-010).
func (c *Controller) dispatchLocked() {
	if c.shuttingDown {
		return
	}
	now := c.now()
	for c.freeLocked() > 0 {
		idx := -1
		var next time.Time
		for i, req := range c.queue {
			if !req.notBefore.After(now) {
				idx = i
				break
			}
			if next.IsZero() || req.notBefore.Before(next) {
				next = req.notBefore
			}
		}
		if idx < 0 {
			if !next.IsZero() {
				c.scheduleDispatchLocked(next.Sub(now))
			}
			return
		}
		req := c.queue[idx]
		c.queue = slices.Delete(c.queue, idx, idx+1)
		req.ss.queued--
		r := c.newRunnerLocked(req)
		go c.startRunner(r)
	}
}

func (c *Controller) scheduleDispatchLocked(d time.Duration) {
	if c.dispatchTimer != nil {
		c.dispatchTimer.Stop()
	}
	c.dispatchTimer = time.AfterFunc(d, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.dispatchLocked()
	})
}

// scaleDown removes surplus Runner Containers of ss while it has more than
// its assigned Job count (FR-028, UC-008): first those still preparing, then
// Idle ones, each only after GitHub confirmed the deletion of its registration.
func (c *Controller) scaleDown(ss *scaleSet) {
	attempt := 0
	for {
		c.mu.Lock()
		if c.shuttingDown || ss.active() <= ss.assigned {
			ss.scalingDown = false
			c.mu.Unlock()
			return
		}
		var cand *runner
		for _, r := range ss.runners {
			if r.phase == phasePreparing && !r.dropped {
				cand = r
				break
			}
		}
		if cand != nil {
			cand.dropped = true
			cand.cancel()
			cand.l().Info("start request dropped while starting", "assigned_jobs", ss.assigned)
			c.mu.Unlock()
			continue
		}
		for _, r := range ss.runners {
			if r.phase == phaseIdle && !r.releasing && (cand == nil || (!r.ready && cand.ready)) {
				cand = r
			}
		}
		if cand == nil {
			// Every remaining one holds a Job; the next poll resolves the surplus.
			ss.scalingDown = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		switch err := c.releaseIdle(cand); {
		case err == nil:
			cand.l().Info("idle runner removed", "reason", "assigned job count dropped")
			go c.finish(cand, nil)
			attempt = 0
		case errors.Is(err, ErrRunnerBusy), errors.Is(err, errSkipped):
			attempt = 0
		default:
			if c.sleep(c.lifeCtx, attempt) != nil {
				return
			}
			attempt++
		}
	}
}

var errSkipped = errors.New("runner no longer idle")

// releaseIdle asks GitHub to delete the registration of an Idle runner once.
// On success the runner is marked removing and the caller must finish it; on
// ErrRunnerBusy it is kept as running a Job (UC-008 A3).
func (c *Controller) releaseIdle(r *runner) error {
	c.mu.Lock()
	if r.phase != phaseIdle || r.releasing {
		c.mu.Unlock()
		return errSkipped
	}
	r.releasing = true
	repo, runnerID := r.ss.repo.Repository, r.runnerID
	c.mu.Unlock()

	ctx, cancel := c.timeoutCtx()
	err := c.gh.RemoveRunner(ctx, repo, runnerID)
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	r.releasing = false
	switch {
	case err == nil:
		r.regDeleted = true
		c.storeErr(c.st.DeleteRegistration(r.regDBID))
		if r.phase == phaseRemoving {
			return errSkipped // someone else finishes it
		}
		r.phase = phaseRemoving
		return nil
	case errors.Is(err, ErrRunnerBusy):
		if r.phase == phaseIdle {
			r.phase = phaseBusy
			if r.contDBID != 0 {
				c.storeErr(c.st.SetContainerStatus(r.contDBID, store.ContainerBusy))
			}
			r.l().Info("idle runner holds a job; keeping it", "reason", "GitHub refused deleting its registration")
		}
		return err
	default:
		r.l().Warn("deleting runner registration failed", "error", err)
		return err
	}
}

func (c *Controller) storeErr(err error) {
	if err != nil {
		c.log.Error("state store write failed", "error", err)
	}
}
