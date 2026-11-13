package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tim/scaleset-runner/internal/store"
)

// overdueAfter is when a Runner Image counts as overdue (UC-006 BR-003).
const overdueAfter = 49 * 24 * time.Hour

// failedRetry is the delay before retrying a failed refresh of a present image.
const failedRetry = 24 * time.Hour

type imageState struct {
	ref       string
	kind      string
	available chan struct{} // closed once the image is on the Host
	ready     bool
	overdue   bool
}

func (c *Controller) imageKind(ref string) string {
	if ref == c.cfg.DefaultImage() {
		return store.KindDefault
	}
	return store.KindRepository
}

// ensureImages makes sure every Runner Image the Config uses is on the Host
// (UC-001 step 4, UC-006 A6). A missing default image that cannot be built
// ends the Controller (A5); a missing per-repository image is pulled again
// with growing delays while its repository waits (A6).
func (c *Controller) ensureImages(ctx context.Context) error {
	for ref, im := range c.images {
		id, created, err := c.docker.ImageID(ctx, ref)
		switch {
		case err == nil:
			row, serr := c.st.CurrentImage(ref)
			c.storeErr(serr)
			if row == nil || row.Digest != id {
				at := c.now()
				if row == nil && !created.IsZero() && created.Before(at) {
					at = created // unknown history: count from when it was built
				}
				_, serr = c.st.RecordRefresh(ref, im.kind, id, at)
				c.storeErr(serr)
			}
			c.markAvailable(im)
		case errors.Is(err, ErrNotFound):
			c.log.Info("runner image missing; refreshing it", "image", ref)
			if err := c.refreshImage(ctx, im); err != nil {
				if im.kind == store.KindDefault {
					c.log.Error("building the default runner image failed", "image", ref, "error", err)
					return fmt.Errorf("default runner image cannot be built: %w", err)
				}
				c.logImageFailure(im, err)
				go c.pullUntilPresent(ctx, im)
				continue
			}
			c.markAvailable(im)
		default:
			c.log.Error("Docker Engine unreachable", "error", err)
			return fmt.Errorf("inspect image %s: %w", ref, err)
		}
	}
	c.updateOverdue()
	return nil
}

func (c *Controller) logImageFailure(im *imageState, err error) {
	var repos []string
	for _, ss := range c.sets {
		if ss.image == im.ref {
			repos = append(repos, ss.repo.Repository)
		}
	}
	c.log.Error("pulling runner image failed", "image", im.ref, "repositories", repos, "error", err)
}

func (c *Controller) pullUntilPresent(ctx context.Context, im *imageState) {
	for attempt := 0; ; attempt++ {
		if c.sleep(ctx, attempt) != nil {
			return
		}
		if err := c.refreshImage(ctx, im); err != nil {
			c.logImageFailure(im, err)
			continue
		}
		c.markAvailable(im)
		return
	}
}

func (c *Controller) markAvailable(im *imageState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !im.ready {
		im.ready = true
		close(im.available)
	}
}

// waitImage blocks until the Runner Image of ss is on the Host.
func (c *Controller) waitImage(ctx context.Context, ss *scaleSet) bool {
	im := c.images[ss.image]
	select {
	case <-im.available:
		return true
	case <-ctx.Done():
		return false
	}
}

// refreshImage builds the default image or pulls a per-repository one and
// records the result (UC-006 steps 2-5).
func (c *Controller) refreshImage(ctx context.Context, im *imageState) error {
	var id string
	var err error
	if im.kind == store.KindDefault {
		c.log.Info("building default runner image", "image", im.ref)
		id, err = c.docker.BuildDefaultImage(ctx, im.ref)
	} else {
		c.log.Info("pulling runner image", "image", im.ref)
		id, err = c.docker.PullImage(ctx, im.ref)
	}
	if err != nil {
		c.storeErr(c.st.RecordRefreshFailure(im.ref, c.now()))
		return err
	}
	_, err = c.st.RecordRefresh(im.ref, im.kind, id, c.now())
	c.storeErr(err)
	c.log.Info("runner image refreshed", "image", im.ref, "image_id", id)
	return nil
}

func (c *Controller) currentImage(ref string) (*store.Image, error) {
	row, err := c.st.CurrentImage(ref)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("runner image %s not recorded", ref)
	}
	return row, nil
}

// refreshLoop refreshes Runner Images when due, removes superseded ones, and
// tracks overdue ones (UC-006).
func (c *Controller) refreshLoop(ctx context.Context) {
	t := time.NewTicker(c.refreshCheckInterval)
	defer t.Stop()
	for {
		c.refreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Controller) refreshDue(ctx context.Context) {
	now := c.now()
	for _, im := range c.images {
		c.mu.Lock()
		ready := im.ready
		c.mu.Unlock()
		if !ready {
			continue // still being pulled with growing delays
		}
		row, err := c.st.CurrentImage(im.ref)
		if err != nil || row == nil {
			c.storeErr(err)
			continue
		}
		due := row.RefreshedAt.Add(c.cfg.RefreshInterval)
		if row.LastFailedAt.After(row.RefreshedAt) {
			due = row.LastFailedAt.Add(failedRetry)
		}
		if now.Before(due) {
			continue
		}
		if err := c.refreshImage(ctx, im); err != nil {
			c.log.Error("refreshing runner image failed; keeping the current one and retrying in a day",
				"image", im.ref, "error", err)
		}
	}
	c.updateOverdue()
	c.removeSuperseded(ctx)
}

func (c *Controller) updateOverdue() {
	now := c.now()
	for _, im := range c.images {
		row, err := c.st.CurrentImage(im.ref)
		c.storeErr(err)
		overdue := row != nil && now.Sub(row.RefreshedAt) > overdueAfter
		if overdue {
			c.log.Warn("runner image overdue: its last successful refresh is more than 49 days old",
				"image", im.ref, "refreshed_at", row.RefreshedAt)
		}
		c.mu.Lock()
		im.overdue = overdue
		c.mu.Unlock()
	}
}

// removeSuperseded removes Runner Images a refresh superseded once no Runner
// Container uses them (FR-037, UC-006 A5).
func (c *Controller) removeSuperseded(ctx context.Context) {
	rows, err := c.st.Images(store.ImageSuperseded)
	c.storeErr(err)
	for _, row := range rows {
		used, err := c.st.ImageInUse(row.ID)
		if err != nil || used {
			c.storeErr(err)
			continue
		}
		if err := c.docker.RemoveImage(ctx, row.Digest); err != nil {
			c.log.Warn("removing superseded runner image failed; retrying later", "image", row.Reference, "image_id", row.Digest, "error", err)
			continue
		}
		c.storeErr(c.st.DeleteImage(row.ID))
		c.log.Info("superseded runner image removed", "image", row.Reference, "image_id", row.Digest)
	}
}
