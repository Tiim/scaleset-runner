// Package controller is the scaleset-runner Controller: it serves one Scale
// Set per configured repository, shares a fixed number of Slots across them,
// and starts one ephemeral Runner Container per Job (docs/use_cases).
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tim/scaleset-runner/internal/backoff"
	"github.com/tim/scaleset-runner/internal/config"
	"github.com/tim/scaleset-runner/internal/logging"
	"github.com/tim/scaleset-runner/internal/store"
)

// Options wires the Controller to its environment.
type Options struct {
	Config   *config.Config
	Docker   Docker
	GitHub   GitHub
	Store    *store.Store
	Logger   *slog.Logger
	Redactor *logging.Redactor
	// Owner is the message session owner name; defaults to the Instance label.
	Owner string
	// Delay returns the growing delay before retry attempt; tests shorten it.
	Delay func(attempt int) time.Duration
	// Tunables tests shorten; zero means the production default.
	StaleCheckInterval   time.Duration
	RefreshCheckInterval time.Duration
	CompletedGrace       time.Duration
	FinalCleanupTimeout  time.Duration
}

type Controller struct {
	cfg    *config.Config
	docker Docker
	gh     GitHub
	st     *store.Store
	log    *slog.Logger
	red    *logging.Redactor
	owner  string
	delay  func(int) time.Duration
	now    func() time.Time

	staleCheckInterval   time.Duration
	refreshCheckInterval time.Duration
	completedGrace       time.Duration
	finalCleanupTimeout  time.Duration

	// lifeCtx outlives the stop request so cleanup can finish; it ends when
	// the Controller exits.
	lifeCtx context.Context
	pollers sync.WaitGroup

	mu            sync.Mutex
	sets          []*scaleSet
	queue         []*startRequest
	seq           uint64
	leftovers     int // Slots held by leftover Runner Containers (UC-001 A7)
	shuttingDown  bool
	dispatchTimer *time.Timer
	changed       chan struct{} // closed and replaced whenever a Slot frees up
	images        map[string]*imageState
}

func New(o Options) *Controller {
	c := &Controller{
		cfg: o.Config, docker: o.Docker, gh: o.GitHub, st: o.Store, log: o.Logger, red: o.Redactor,
		owner: o.Owner, delay: o.Delay, now: time.Now,
		staleCheckInterval:   o.StaleCheckInterval,
		refreshCheckInterval: o.RefreshCheckInterval,
		completedGrace:       o.CompletedGrace,
		finalCleanupTimeout:  o.FinalCleanupTimeout,
		changed:              make(chan struct{}),
		images:               map[string]*imageState{},
		lifeCtx:              context.Background(),
	}
	if c.owner == "" {
		c.owner = o.Config.InstanceLabel()
	}
	if c.delay == nil {
		c.delay = backoff.Delay
	}
	if c.red == nil {
		c.red = logging.NewRedactor()
	}
	if c.staleCheckInterval == 0 {
		c.staleCheckInterval = 2 * time.Second
	}
	if c.refreshCheckInterval == 0 {
		c.refreshCheckInterval = time.Hour
	}
	if c.completedGrace == 0 {
		c.completedGrace = 60 * time.Second
	}
	if c.finalCleanupTimeout == 0 {
		c.finalCleanupTimeout = 20 * time.Second
	}
	for _, r := range o.Config.Repos {
		ss := newScaleSet(r, o.Config.ImageFor(r), c.log)
		c.sets = append(c.sets, ss)
		if _, ok := c.images[ss.image]; !ok {
			c.images[ss.image] = &imageState{ref: ss.image, kind: c.imageKind(ss.image), available: make(chan struct{})}
		}
	}
	return c
}

// Run brings the Controller up (UC-001), serves Jobs until ctx ends, then
// shuts down (UC-004). It returns an error only for failures that end the
// Controller at startup: rejected credentials, an unreachable Docker Engine,
// or a default Runner Image that cannot be built.
func (c *Controller) Run(ctx context.Context) error {
	lifeCtx, cancelLife := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelLife()
	c.lifeCtx = lifeCtx

	if err := c.signIn(ctx); err != nil {
		return err
	}
	if err := c.ensureImages(ctx); err != nil {
		return err
	}
	if err := c.removeLeftovers(ctx); err != nil {
		return err
	}
	c.deleteStaleScaleSets(ctx)
	for _, ss := range c.sets {
		c.pollers.Add(1)
		go func() {
			defer c.pollers.Done()
			c.serve(ctx, ss)
		}()
	}
	go c.refreshLoop(ctx)

	<-ctx.Done()
	c.shutdown()
	return nil
}

// signIn checks the credentials against GitHub (UC-001 step 3). Rejected
// credentials end the Controller; an unreachable GitHub is retried (A2, A3).
func (c *Controller) signIn(ctx context.Context) error {
	repo := c.cfg.Repos[0].Repository
	for attempt := 0; ; attempt++ {
		err := c.gh.CheckAuth(ctx, repo)
		if err == nil {
			c.log.Info("signed in to GitHub")
			return nil
		}
		if errors.Is(err, ErrUnauthorized) {
			c.log.Error("GitHub rejected the credentials", "error", err)
			return fmt.Errorf("GitHub rejected the credentials")
		}
		c.log.Warn("GitHub unreachable; retrying", "error", err, "attempt", attempt+1)
		if err := backoff.Sleep(ctx, c.delay(attempt)); err != nil {
			return err
		}
	}
}

// notifyLocked wakes everyone waiting for a Slot to free up. Callers hold c.mu.
func (c *Controller) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Controller) sleep(ctx context.Context, attempt int) error {
	return backoff.Sleep(ctx, c.delay(attempt))
}

// timeoutCtx bounds one remote call made during cleanup.
func (c *Controller) timeoutCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.lifeCtx, 30*time.Second)
}
