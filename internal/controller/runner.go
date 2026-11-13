package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/tim/scaleset-runner/internal/store"
)

func (c *Controller) newRunnerLocked(req *startRequest) *runner {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := fmt.Sprintf("ssr-%s-%s", c.cfg.InstanceID, hex.EncodeToString(b))
	ctx, cancel := context.WithCancel(c.lifeCtx)
	r := &runner{
		name: name, ss: req.ss, req: req, ctx: ctx, cancel: cancel,
	}
	r.logp.Store(req.ss.log.With("runner", name))
	req.ss.runners[name] = r
	return r
}

// startRunner obtains a JIT Config and starts the Runner Container for r,
// which already holds a Slot (UC-007 steps 6-8).
func (c *Controller) startRunner(r *runner) {
	ss := r.ss
	c.mu.Lock()
	ghID := ss.ghID
	c.mu.Unlock()

	img, err := c.currentImage(ss.image)
	if err != nil {
		c.startFailed(r, err)
		return
	}

	runnerID, jit, err := c.gh.GenerateJIT(r.ctx, ss.repo.Repository, ghID, r.name)
	if err != nil {
		c.startFailed(r, fmt.Errorf("JIT Config: %w", err))
		return
	}
	c.red.Add(jit)
	regDBID, err := c.st.InsertRegistration(ss.dbID, runnerID, r.name)
	c.storeErr(err)
	c.mu.Lock()
	r.jit, r.runnerID, r.regDBID = jit, runnerID, regDBID
	c.mu.Unlock()

	id, err := c.docker.CreateRunner(r.ctx, RunnerSpec{
		Name: r.name, ImageID: img.Digest, InstanceID: c.cfg.InstanceID, Repository: ss.repo.Repository,
		JITConfig: jit, NanoCPUs: c.cfg.RunnerNanoCPUs, Memory: c.cfg.RunnerMemory, Runtime: c.cfg.Runtime,
	})
	if id != "" {
		contDBID, serr := c.st.InsertContainer(regDBID, img.ID, id)
		c.storeErr(serr)
		c.mu.Lock()
		r.containerID, r.contDBID = id, contDBID
		c.mu.Unlock()
	}
	if err != nil {
		c.startFailed(r, fmt.Errorf("create container: %w", err))
		return
	}
	if err := c.docker.StartContainer(r.ctx, id); err != nil {
		c.startFailed(r, fmt.Errorf("start container: %w", err))
		return
	}

	c.mu.Lock()
	r.phase = phaseIdle
	dropped := r.dropped
	if dropped {
		// The drop cancelled r.ctx; the container lives on, so renew it.
		r.dropped = false
		r.ctx, r.cancel = context.WithCancel(c.lifeCtx)
	}
	shutting := c.shuttingDown
	c.mu.Unlock()
	r.l().Info("runner container started", "container_id", id[:min(12, len(id))])

	go c.watchExit(r)
	go c.watchReady(r)

	switch {
	case shutting:
		go c.releaseOnShutdown(r)
	case dropped:
		// Dropped after the container started: GitHub decides whether it is
		// still idle, so hand it to the regular scale-down.
		c.mu.Lock()
		c.reconcileLocked(ss)
		c.mu.Unlock()
	}
}

// startFailed cleans up after a failed start and, unless the start request was
// dropped, retries it with growing delays (UC-007 A3, A4, A5).
func (c *Controller) startFailed(r *runner, err error) {
	c.mu.Lock()
	canceled := r.dropped || r.ctx.Err() != nil || c.shuttingDown
	r.phase = phaseRemoving
	c.mu.Unlock()

	var retry *startRequest
	if canceled {
		r.l().Info("start request dropped", "reason", "demand dropped or shutting down")
	} else {
		r.l().Error("runner container failed to start", "error", err, "attempt", r.req.attempt+1)
		retry = &startRequest{ss: r.ss, seq: r.req.seq, attempt: r.req.attempt + 1,
			notBefore: c.now().Add(c.delay(r.req.attempt))}
	}
	c.finish(r, retry)
}

// watchReady waits for the Ready marker (FR-034, UC-007 A6).
func (c *Controller) watchReady(r *runner) {
	ctx, cancel := context.WithTimeout(r.ctx, c.cfg.ReadinessTimeout)
	defer cancel()
	err := c.docker.WaitReady(ctx, r.containerID)

	c.mu.Lock()
	if err == nil {
		r.ready = true
		c.storeErr(c.st.SetContainerReady(r.contDBID))
		c.mu.Unlock()
		r.l().Info("runner ready")
		return
	}
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded) && c.lifeCtx.Err() == nil
	phase, shutting, exited := r.phase, c.shuttingDown, r.exited
	c.mu.Unlock()
	if !timedOut || shutting || exited {
		return // the container exited; watchExit handles it
	}
	if phase == phaseBusy {
		r.l().Warn("runner wrote no ready marker but holds a job", "readiness_timeout", c.cfg.ReadinessTimeout.String())
		return
	}
	if phase != phaseIdle {
		return
	}
	for attempt := 0; ; attempt++ {
		switch err := c.releaseIdle(r); {
		case err == nil:
			r.l().Error("runner wrote no ready marker in time; removed it", "readiness_timeout", c.cfg.ReadinessTimeout.String())
			c.finish(r, &startRequest{ss: r.ss, seq: r.req.seq, attempt: r.req.attempt + 1,
				notBefore: c.now().Add(c.delay(r.req.attempt))})
			return
		case errors.Is(err, ErrRunnerBusy):
			r.l().Warn("runner wrote no ready marker but holds a job", "readiness_timeout", c.cfg.ReadinessTimeout.String())
			return
		case errors.Is(err, errSkipped):
			return
		}
		if c.sleep(c.lifeCtx, attempt) != nil {
			return
		}
	}
}

// watchExit waits for the Runner Container to stop and removes it.
func (c *Controller) watchExit(r *runner) {
	var code int64
	for attempt := 0; ; attempt++ {
		var err error
		code, err = c.docker.WaitExit(c.lifeCtx, r.containerID)
		if err == nil {
			break
		}
		if c.lifeCtx.Err() != nil {
			return
		}
		r.l().Warn("waiting for runner container failed", "error", err)
		if c.sleep(c.lifeCtx, attempt) != nil {
			return
		}
	}

	c.mu.Lock()
	r.exited = true
	if r.phase == phaseRemoving {
		c.mu.Unlock()
		return
	}
	r.phase = phaseRemoving
	completed := r.completed
	if !completed {
		// FR-035: replace only from an assigned Job count a later poll reports.
		r.ss.holdSeq = r.ss.pollSeq
	}
	c.mu.Unlock()

	if completed {
		r.l().Info("runner container exited", "exit_code", code)
	} else {
		r.l().Warn("runner container exited without JobCompleted message", "exit_code", code)
	}
	c.finish(r, nil)
}

// finish deletes r's Runner Registration and removes its Runner Container,
// then frees its Slot. retry, if set, is queued in the same step so its
// position and delay survive (UC-007 step 12, A10, A11).
func (c *Controller) finish(r *runner, retry *startRequest) {
	r.cancel()
	c.mu.Lock()
	repo := r.ss.repo.Repository
	runnerID, regDeleted, regDBID := r.runnerID, r.regDeleted, r.regDBID
	containerID, contDBID, jit := r.containerID, r.contDBID, r.jit
	c.mu.Unlock()

	if runnerID != 0 && !regDeleted {
		ctx, cancel := c.timeoutCtx()
		err := c.gh.RemoveRunner(ctx, repo, runnerID)
		cancel()
		if err == nil {
			c.storeErr(c.st.DeleteRegistration(regDBID))
		} else {
			r.l().Warn("runner registration kept for deletion at next start", "error", err)
		}
	}

	if containerID != "" {
		if contDBID != 0 {
			c.storeErr(c.st.SetContainerStatus(contDBID, store.ContainerRemoving))
		}
		for attempt := 0; ; attempt++ {
			ctx, cancel := c.timeoutCtx()
			err := c.docker.RemoveContainer(ctx, containerID)
			cancel()
			if err == nil {
				if contDBID != 0 {
					c.storeErr(c.st.DeleteContainer(contDBID))
				}
				break
			}
			r.l().Error("removing runner container failed; it keeps its slot", "error", err)
			if c.sleep(c.lifeCtx, attempt) != nil {
				return // shutting down: the record stays for the next start
			}
		}
	}
	if jit != "" {
		c.red.Remove(jit)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.observeJobLocked(r)
	delete(r.ss.runners, r.name)
	if retry != nil && !c.shuttingDown {
		c.enqueueLocked(retry)
	}
	r.l().Info("runner container removed", "slots_free", c.freeLocked())
	c.notifyLocked()
	c.reconcileLocked(r.ss)
	c.dispatchLocked()
}

func (c *Controller) observeJobLocked(r *runner) {
	if r.jobStart.IsZero() || r.observed {
		return
	}
	r.ss.jobs.observe(c.now().Sub(r.jobStart).Seconds())
	r.observed = true
}
