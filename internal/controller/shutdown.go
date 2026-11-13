package controller

import (
	"errors"
	"time"
)

// shutdown stops taking Jobs, removes Idle Runner Containers, lets running
// Jobs finish up to the shutdown timeout, and removes what remains (UC-004).
func (c *Controller) shutdown() {
	c.log.Info("shutting down", "shutdown_timeout", c.cfg.ShutdownTimeout.String())
	deadline := c.now().Add(c.cfg.ShutdownTimeout)

	c.mu.Lock()
	c.shuttingDown = true
	if c.dispatchTimer != nil {
		c.dispatchTimer.Stop()
	}
	dropped := len(c.queue)
	for _, req := range c.queue {
		req.ss.queued--
	}
	c.queue = nil
	var idle []*runner
	for _, ss := range c.sets {
		for _, r := range ss.runners {
			switch r.phase {
			case phasePreparing:
				r.dropped = true
				r.cancel()
			case phaseIdle:
				idle = append(idle, r)
			}
		}
	}
	c.mu.Unlock()
	if dropped > 0 {
		c.log.Info("waiting start requests dropped", "count", dropped)
	}

	// Stop polling; every poller closes its message session.
	c.pollers.Wait()

	for _, r := range idle {
		go c.releaseOnShutdown(r)
	}

	if !c.waitForRunners(deadline) {
		c.mu.Lock()
		var remaining []*runner
		var withJob []bool
		for _, ss := range c.sets {
			for _, r := range ss.runners {
				if r.phase != phaseRemoving {
					r.phase = phaseRemoving
					remaining = append(remaining, r)
					withJob = append(withJob, r.jobID != "")
				}
			}
		}
		c.mu.Unlock()
		for i, r := range remaining {
			if withJob[i] {
				r.l().Warn("shutdown timeout expired; ending job", "container_id", short(r.containerID))
			} else {
				r.l().Warn("shutdown timeout expired; removing runner container", "container_id", short(r.containerID))
			}
			go c.finish(r, nil)
		}
		if !c.waitForRunners(c.now().Add(c.finalCleanupTimeout)) {
			c.log.Warn("runner containers left for removal at the next start")
		}
	}
	c.log.Info("shutdown complete")
}

// releaseOnShutdown removes an Idle Runner Container once GitHub confirms the
// deletion of its registration, retrying until the shutdown timeout (UC-004
// step 4, A1, A6).
func (c *Controller) releaseOnShutdown(r *runner) {
	deadline := c.now().Add(c.cfg.ShutdownTimeout)
	for attempt := 0; c.now().Before(deadline); attempt++ {
		switch err := c.releaseIdle(r); {
		case err == nil:
			r.l().Info("idle runner removed", "reason", "shutting down")
			c.finish(r, nil)
			return
		case errors.Is(err, ErrRunnerBusy), errors.Is(err, errSkipped):
			return
		}
		if c.sleep(c.lifeCtx, attempt) != nil {
			return
		}
	}
}

// waitForRunners waits until no Runner Container holds a Slot, or deadline.
func (c *Controller) waitForRunners(deadline time.Time) bool {
	for {
		c.mu.Lock()
		n := c.usedLocked()
		ch := c.changed
		c.mu.Unlock()
		if n == 0 {
			return true
		}
		wait := deadline.Sub(c.now())
		if wait <= 0 {
			return false
		}
		t := time.NewTimer(wait)
		select {
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

func short(id string) string { return id[:min(12, len(id))] }
