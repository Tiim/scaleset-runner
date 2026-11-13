package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/actions/scaleset"

	"github.com/tim/scaleset-runner/internal/backoff"
	"github.com/tim/scaleset-runner/internal/store"
)

// serve resolves the Scale Set of one configured repository and polls it
// until ctx ends (UC-001 steps 7-9).
func (c *Controller) serve(ctx context.Context, ss *scaleSet) {
	if !c.waitImage(ctx, ss) {
		return
	}
	ghID, ok := c.resolveScaleSet(ctx, ss)
	if !ok {
		return
	}
	for ctx.Err() == nil {
		sess := c.openSession(ctx, ss, ghID)
		if sess == nil {
			return
		}
		c.poll(ctx, ss, sess)
		c.mu.Lock()
		ss.served = false
		c.mu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := sess.Close(closeCtx); err != nil {
			ss.log.Warn("closing message session failed", "error", err)
		} else {
			ss.log.Info("message session closed")
		}
		cancel()
	}
}

func (c *Controller) desiredScaleSet(ss *scaleSet) *scaleset.RunnerScaleSet {
	return &scaleset.RunnerScaleSet{
		Name: ss.repo.Name,
		Labels: []scaleset.Label{
			{Name: ss.repo.Name, Type: "System"},
			{Name: c.cfg.InstanceLabel(), Type: "System"},
		},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}
}

func hasLabel(s *scaleset.RunnerScaleSet, label string) bool {
	return slices.ContainsFunc(s.Labels, func(l scaleset.Label) bool { return l.Name == label })
}

// resolveScaleSet finds, takes over, or creates the Scale Set of ss and
// records it (UC-001 step 7, A9, A10, FR-036).
func (c *Controller) resolveScaleSet(ctx context.Context, ss *scaleSet) (int, bool) {
	repo, name := ss.repo.Repository, ss.repo.Name
	for attempt := 0; ; attempt++ {
		if attempt > 0 && c.sleep(ctx, attempt-1) != nil {
			return 0, false
		}
		recs, err := c.st.ScaleSets()
		if err != nil {
			c.storeErr(err)
			continue
		}
		var rec *store.ScaleSet
		for i := range recs {
			if recs[i].Repository == repo && recs[i].Name == name {
				rec = &recs[i]
			}
		}
		if rec != nil {
			_, err := c.gh.ScaleSetByID(ctx, repo, rec.GitHubID)
			switch {
			case err == nil:
				c.useScaleSet(ss, rec.ID, rec.GitHubID)
				ss.log.Info("serving recorded scale set", "scale_set_id", rec.GitHubID)
				go c.updateScaleSet(ctx, ss, rec.GitHubID)
				return rec.GitHubID, true
			case errors.Is(err, ErrNotFound):
				ss.log.Warn("recorded scale set no longer exists on GitHub; replacing it", "scale_set_id", rec.GitHubID)
				c.forgetScaleSet(rec.ID)
			default:
				ss.log.Warn("looking up scale set failed; retrying", "error", err)
				continue
			}
		}

		existing, err := c.gh.ScaleSetByName(ctx, repo, name)
		if err != nil {
			ss.log.Warn("looking up scale set by name failed; retrying", "error", err)
			continue
		}
		if existing != nil {
			if !hasLabel(existing, c.cfg.InstanceLabel()) {
				msg := fmt.Sprintf("a scale set named %q that this controller does not own exists; leaving it untouched", name)
				ss.log.Error(msg, "scale_set_id", existing.ID)
				c.mu.Lock()
				ss.unserved = "name clash with a foreign scale set"
				c.mu.Unlock()
				return 0, false
			}
			id, err := c.st.InsertScaleSet(existing.ID, repo, name, store.OriginTakenOver)
			if err != nil {
				c.storeErr(err)
				continue
			}
			c.useScaleSet(ss, id, existing.ID)
			ss.log.Info("took over scale set carrying the instance label", "scale_set_id", existing.ID)
			go c.updateScaleSet(ctx, ss, existing.ID)
			return existing.ID, true
		}

		created, err := c.gh.CreateScaleSet(ctx, repo, c.desiredScaleSet(ss))
		if err != nil {
			ss.log.Error("creating scale set failed; retrying", "error", err, "attempt", attempt+1)
			continue
		}
		id, err := c.st.InsertScaleSet(created.ID, repo, name, store.OriginCreated)
		if err != nil {
			c.storeErr(err)
			continue
		}
		c.useScaleSet(ss, id, created.ID)
		ss.log.Info("created scale set", "scale_set_id", created.ID)
		return created.ID, true
	}
}

func (c *Controller) useScaleSet(ss *scaleSet, dbID int64, ghID int) {
	c.mu.Lock()
	ss.dbID, ss.ghID = dbID, ghID
	c.mu.Unlock()
}

// forgetScaleSet drops the record of a Scale Set that no longer exists on
// GitHub, with its registrations, which went with it.
func (c *Controller) forgetScaleSet(id int64) {
	regs, err := c.st.Registrations()
	c.storeErr(err)
	for _, r := range regs {
		if r.ScaleSetID == id {
			c.storeErr(c.st.DeleteRegistration(r.ID))
		}
	}
	c.storeErr(c.st.DeleteScaleSet(id))
}

// updateScaleSet brings a Scale Set to the configured settings, retrying in
// the background while it is served as is (UC-001 A13).
func (c *Controller) updateScaleSet(ctx context.Context, ss *scaleSet, ghID int) {
	for attempt := 0; ; attempt++ {
		err := c.gh.UpdateScaleSet(ctx, ss.repo.Repository, ghID, c.desiredScaleSet(ss))
		if err == nil {
			return
		}
		ss.log.Warn("updating scale set settings failed; serving it as is and retrying", "error", err)
		if c.sleep(ctx, attempt) != nil {
			return
		}
	}
}

func (c *Controller) openSession(ctx context.Context, ss *scaleSet, ghID int) Session {
	for attempt := 0; ; attempt++ {
		sess, err := c.gh.OpenSession(ctx, ss.repo.Repository, ghID, c.owner)
		if err == nil {
			c.red.Add(sess.Session().MessageQueueAccessToken)
			ss.log.Info("message session opened")
			return sess
		}
		if ctx.Err() != nil {
			return nil
		}
		ss.log.Error("opening message session failed; retrying", "error", err, "attempt", attempt+1)
		if c.sleep(ctx, attempt) != nil {
			return nil
		}
	}
}

// maxPollFailures is how many failed polls in a row make the Controller open
// a new message session.
const maxPollFailures = 5

// poll runs the message loop of one session. It acquires every offered Job,
// even without a free Slot, and leaves a message unacknowledged when
// acquiring fails so GitHub offers its Jobs again (FR-033, UC-007 A1).
func (c *Controller) poll(ctx context.Context, ss *scaleSet, sess Session) {
	token := sess.Session().MessageQueueAccessToken
	defer func() { c.red.Remove(token) }()

	c.mu.Lock()
	ss.served = true
	ss.lastPoll = c.now()
	c.mu.Unlock()
	if st := sess.Session().Statistics; st != nil {
		c.handleMessage(ss, &scaleset.RunnerScaleSetMessage{Statistics: st})
	}

	lastID, failures := 0, 0
	for ctx.Err() == nil {
		msg, err := sess.GetMessage(ctx, lastID, c.capacity(ss))
		if t := sess.Session().MessageQueueAccessToken; t != token {
			c.red.Add(t)
			c.red.Remove(token)
			token = t
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			ss.log.Warn("poll failed", "error", err, "failures", failures)
			if failures >= maxPollFailures || errors.Is(err, ErrNotFound) {
				return
			}
			if c.sleep(ctx, failures-1) != nil {
				return
			}
			continue
		}
		failures = 0
		c.mu.Lock()
		ss.lastPoll = c.now()
		c.mu.Unlock()
		if msg == nil {
			continue
		}

		if len(msg.JobAvailableMessages) > 0 {
			ids := make([]int64, len(msg.JobAvailableMessages))
			for i, j := range msg.JobAvailableMessages {
				ids[i] = j.RunnerRequestID
			}
			if _, err := sess.AcquireJobs(ctx, ids); err != nil {
				failures++
				ss.log.Warn("acquiring offered jobs failed; message left unacknowledged", "error", err, "jobs", len(ids))
				if c.sleep(ctx, failures-1) != nil {
					return
				}
				continue
			}
			ss.log.Info("jobs acquired", "jobs", len(ids))
		}

		c.handleMessage(ss, msg)
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if err := sess.DeleteMessage(ackCtx, msg.MessageID); err != nil {
			ss.log.Warn("acknowledging message failed", "error", err, "message_id", msg.MessageID)
		}
		cancel()
		lastID = msg.MessageID
	}
}

// removeLeftovers removes the Runner Containers carrying this Instance ID and
// deletes every recorded Runner Registration, all left from a previous run
// (UC-001 step 5, A7). Failures are retried in the background; a leftover
// Runner Container keeps a Slot until it is removed.
func (c *Controller) removeLeftovers(ctx context.Context) error {
	ids, err := c.docker.ListRunnerContainers(ctx, c.cfg.InstanceID)
	if err != nil {
		c.log.Error("Docker Engine unreachable", "error", err)
		return fmt.Errorf("list runner containers: %w", err)
	}
	present := map[string]bool{}
	for _, id := range ids {
		present[id] = true
	}
	rows, err := c.st.Containers()
	c.storeErr(err)
	for _, row := range rows {
		if !present[row.DockerID] {
			c.storeErr(c.st.DeleteContainer(row.ID))
		}
	}

	c.mu.Lock()
	c.leftovers = len(ids)
	c.mu.Unlock()
	for _, id := range ids {
		go func() {
			short := id[:min(12, len(id))]
			for attempt := 0; ; attempt++ {
				err := c.docker.RemoveContainer(c.lifeCtx, id)
				if err == nil {
					c.storeErr(c.st.DeleteContainerByDockerID(id))
					c.log.Info("leftover runner container removed", "container_id", short)
					break
				}
				c.log.Error("removing leftover runner container failed; retrying", "container_id", short, "error", err)
				if c.sleep(c.lifeCtx, attempt) != nil {
					return
				}
			}
			c.mu.Lock()
			c.leftovers--
			c.notifyLocked()
			c.dispatchLocked()
			c.mu.Unlock()
		}()
	}

	regs, err := c.st.Registrations()
	c.storeErr(err)
	for _, reg := range regs {
		go func() {
			for attempt := 0; ; attempt++ {
				ctx, cancel := c.timeoutCtx()
				err := c.gh.RemoveRunner(ctx, reg.Repository, reg.RunnerID)
				cancel()
				if err == nil {
					c.storeErr(c.st.DeleteRegistration(reg.ID))
					c.log.Info("leftover runner registration deleted", "repository", reg.Repository, "runner", reg.RunnerName)
					return
				}
				c.log.Error("deleting leftover runner registration failed; retrying",
					"repository", reg.Repository, "runner", reg.RunnerName, "error", err)
				if c.sleep(c.lifeCtx, attempt) != nil {
					return
				}
			}
		}()
	}
	return nil
}

// deleteStaleScaleSets deletes every recorded Scale Set whose repository or
// name is no longer configured, once its recorded Runner Registrations are
// gone (UC-001 step 6, UC-003, A8).
func (c *Controller) deleteStaleScaleSets(ctx context.Context) {
	recs, err := c.st.ScaleSets()
	c.storeErr(err)
	configured := map[[2]string]bool{}
	for _, r := range c.cfg.Repos {
		configured[[2]string{r.Repository, r.Name}] = true
	}
	for _, rec := range recs {
		if configured[[2]string{rec.Repository, rec.Name}] {
			continue
		}
		log := c.log.With("repository", rec.Repository, "scale_set", rec.Name, "scale_set_id", rec.GitHubID)
		go func() {
			for {
				n, err := c.st.CountRegistrations(rec.ID)
				c.storeErr(err)
				if err == nil && n == 0 {
					break
				}
				if backoff.Sleep(ctx, c.staleCheckInterval) != nil {
					return
				}
			}
			for attempt := 0; ; attempt++ {
				err := c.gh.DeleteScaleSet(ctx, rec.Repository, rec.GitHubID)
				if err == nil {
					c.storeErr(c.st.DeleteScaleSet(rec.ID))
					log.Info("stale scale set deleted")
					return
				}
				log.Error("deleting stale scale set failed; retrying", "error", err)
				if c.sleep(ctx, attempt) != nil {
					return
				}
			}
		}()
	}
}
