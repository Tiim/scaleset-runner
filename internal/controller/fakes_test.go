package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/tim/scaleset-runner/internal/config"
	"github.com/tim/scaleset-runner/internal/logging"
	"github.com/tim/scaleset-runner/internal/store"
)

// ---- Docker ----

type fakeContainer struct {
	id, name   string
	spec       RunnerSpec
	started    bool
	ready      chan struct{}
	exit       chan struct{}
	exited     bool
	code       int64
	removed    bool
	readyFired bool
}

type fakeDocker struct {
	mu             sync.Mutex
	containers     map[string]*fakeContainer
	byName         map[string]*fakeContainer
	leftovers      []string
	removedLeft    []string
	createFailures int
	removeFailures int
	autoReady      bool
	live, maxLive  int
	startedRepos   []string
	images         map[string]string
	builds, pulls  int
	outdated       map[string]bool
	buildErr       error
	pullErr        error
	nextID         int
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*fakeContainer{},
		byName:     map[string]*fakeContainer{},
		images:     map[string]string{"scaleset-runner/runner:test": "sha256:default1"},
		autoReady:  true,
	}
}

func (d *fakeDocker) ListRunnerContainers(context.Context, string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.leftovers...), nil
}

func (d *fakeDocker) CreateRunner(_ context.Context, spec RunnerSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.createFailures > 0 {
		d.createFailures--
		return "", errors.New("docker refused")
	}
	d.nextID++
	id := fmt.Sprintf("%064d", d.nextID)
	c := &fakeContainer{id: id, name: spec.Name, spec: spec, ready: make(chan struct{}), exit: make(chan struct{})}
	d.containers[id] = c
	d.byName[spec.Name] = c
	d.live++
	d.maxLive = max(d.maxLive, d.live)
	return id, nil
}

func (d *fakeDocker) StartContainer(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.containers[id]
	c.started = true
	d.startedRepos = append(d.startedRepos, c.spec.Repository)
	if d.autoReady {
		c.readyFired = true
		close(c.ready)
	}
	return nil
}

func (d *fakeDocker) WaitReady(ctx context.Context, id string) error {
	d.mu.Lock()
	c := d.containers[id]
	d.mu.Unlock()
	select {
	case <-c.ready:
		return nil
	case <-c.exit:
		return errors.New("exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *fakeDocker) WaitExit(ctx context.Context, id string) (int64, error) {
	d.mu.Lock()
	c := d.containers[id]
	d.mu.Unlock()
	select {
	case <-c.exit:
		d.mu.Lock()
		defer d.mu.Unlock()
		return c.code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (d *fakeDocker) RemoveContainer(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.removeFailures > 0 {
		d.removeFailures--
		return errors.New("docker busy")
	}
	c, ok := d.containers[id]
	if !ok {
		d.removedLeft = append(d.removedLeft, id)
		return nil
	}
	if !c.removed {
		c.removed = true
		d.live--
		if !c.exited {
			c.exited, c.code = true, 137
			close(c.exit)
		}
	}
	return nil
}

func (d *fakeDocker) ImageID(_ context.Context, ref string) (string, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.images[ref]; ok {
		return id, time.Now().Add(-time.Hour), nil
	}
	return "", time.Time{}, ErrNotFound
}

func (d *fakeDocker) BuildDefaultImage(_ context.Context, tag string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.builds++
	if d.buildErr != nil {
		return "", d.buildErr
	}
	id := fmt.Sprintf("sha256:build%d", d.builds)
	d.images[tag] = id
	delete(d.outdated, tag)
	return id, nil
}

func (d *fakeDocker) DefaultImageOutdated(_ context.Context, tag string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.images[tag]; !ok {
		return false, ErrNotFound
	}
	return d.outdated[tag], nil
}

func (d *fakeDocker) PullImage(_ context.Context, ref string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pulls++
	if d.pullErr != nil {
		return "", d.pullErr
	}
	id := fmt.Sprintf("sha256:pull%d", d.pulls)
	d.images[ref] = id
	return id, nil
}

func (d *fakeDocker) RemoveImage(context.Context, string) error { return nil }

// exitRunner stops the container of runner name with code.
func (d *fakeDocker) exitRunner(name string, code int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.byName[name]
	if !c.exited {
		c.exited, c.code = true, code
		close(c.exit)
	}
}

// liveRunners returns names of started, not removed containers, in creation order.
func (d *fakeDocker) liveRunners() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for i := 1; i <= d.nextID; i++ {
		c := d.containers[fmt.Sprintf("%064d", i)]
		if c != nil && c.started && !c.removed && !c.exited {
			out = append(out, c.name)
		}
	}
	return out
}

func (d *fakeDocker) liveCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.live
}

// ---- GitHub ----

type fakeGitHub struct {
	mu          sync.Mutex
	scaleSets   map[int]*scaleset.RunnerScaleSet
	repoOf      map[int]string
	nextSS      int
	created     []string
	deletedSS   []int
	runners     map[int64]string
	nextRunner  int64
	removed     []int64
	removeHook  func(id int64) error
	jitFailures int
	jits        int
	sessions    map[string]*fakeSession
	authErr     error
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		scaleSets: map[int]*scaleset.RunnerScaleSet{}, repoOf: map[int]string{}, nextSS: 100,
		runners: map[int64]string{}, nextRunner: 1000, sessions: map[string]*fakeSession{},
	}
}

func (g *fakeGitHub) CheckAuth(context.Context, string) error { return g.authErr }

func (g *fakeGitHub) ScaleSetByName(_ context.Context, repo, name string) (*scaleset.RunnerScaleSet, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, s := range g.scaleSets {
		if g.repoOf[id] == repo && s.Name == name {
			return s, nil
		}
	}
	return nil, nil
}

func (g *fakeGitHub) ScaleSetByID(_ context.Context, _ string, id int) (*scaleset.RunnerScaleSet, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.scaleSets[id]; ok {
		return s, nil
	}
	return nil, ErrNotFound
}

func (g *fakeGitHub) addScaleSet(repo, name string, labels ...string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextSS++
	s := &scaleset.RunnerScaleSet{ID: g.nextSS, Name: name}
	for _, l := range labels {
		s.Labels = append(s.Labels, scaleset.Label{Name: l})
	}
	g.scaleSets[s.ID] = s
	g.repoOf[s.ID] = repo
	return s.ID
}

func (g *fakeGitHub) CreateScaleSet(_ context.Context, repo string, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	g.mu.Lock()
	g.created = append(g.created, repo)
	g.mu.Unlock()
	labels := make([]string, len(ss.Labels))
	for i, l := range ss.Labels {
		labels[i] = l.Name
	}
	id := g.addScaleSet(repo, ss.Name, labels...)
	return &scaleset.RunnerScaleSet{ID: id, Name: ss.Name}, nil
}

func (g *fakeGitHub) UpdateScaleSet(context.Context, string, int, *scaleset.RunnerScaleSet) error {
	return nil
}

func (g *fakeGitHub) DeleteScaleSet(_ context.Context, _ string, id int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.scaleSets, id)
	g.deletedSS = append(g.deletedSS, id)
	return nil
}

func (g *fakeGitHub) GenerateJIT(_ context.Context, _ string, _ int, name string) (int64, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.jitFailures > 0 {
		g.jitFailures--
		return 0, "", errors.New("jit refused")
	}
	g.jits++
	g.nextRunner++
	g.runners[g.nextRunner] = name
	return g.nextRunner, fmt.Sprintf("JITSECRET-%d-%s", g.nextRunner, uuid.NewString()), nil
}

func (g *fakeGitHub) RemoveRunner(_ context.Context, _ string, id int64) error {
	g.mu.Lock()
	hook := g.removeHook
	g.mu.Unlock()
	if hook != nil {
		if err := hook(id); err != nil {
			return err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.runners, id)
	g.removed = append(g.removed, id)
	return nil
}

func (g *fakeGitHub) runnerID(name string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, n := range g.runners {
		if n == name {
			return id
		}
	}
	return 0
}

func (g *fakeGitHub) OpenSession(_ context.Context, repo string, _ int, _ string) (Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[repo]
	if s == nil {
		s = newFakeSession()
		g.sessions[repo] = s
	}
	return s, nil
}

func (g *fakeGitHub) session(t *testing.T, repo string) *fakeSession {
	t.Helper()
	var s *fakeSession
	eventually(t, "session for "+repo, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		s = g.sessions[repo]
		return s != nil
	})
	return s
}

// ---- Session ----

type fakeSession struct {
	mu          sync.Mutex
	queue       []*scaleset.RunnerScaleSetMessage
	nextID      int
	notify      chan struct{}
	acquired    []int64
	acquireErrs int
	acked       []int
	capacities  []int
	closed      bool
	token       string
}

func newFakeSession() *fakeSession {
	return &fakeSession{notify: make(chan struct{}, 1), token: "MQTOKEN-" + uuid.NewString()}
}

func (s *fakeSession) push(m *scaleset.RunnerScaleSetMessage) {
	s.mu.Lock()
	s.nextID++
	m.MessageID = s.nextID
	s.queue = append(s.queue, m)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *fakeSession) GetMessage(ctx context.Context, lastID, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	s.capacities = append(s.capacities, capacity)
	for _, m := range s.queue {
		if m.MessageID > lastID {
			s.mu.Unlock()
			return m, nil
		}
	}
	s.mu.Unlock()
	select {
	case <-s.notify:
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

func (s *fakeSession) DeleteMessage(_ context.Context, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = append(s.acked, id)
	for i, m := range s.queue {
		if m.MessageID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	return nil
}

func (s *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireErrs > 0 {
		s.acquireErrs--
		return nil, errors.New("acquire refused")
	}
	s.acquired = append(s.acquired, ids...)
	return ids, nil
}

func (s *fakeSession) Session() scaleset.RunnerScaleSetSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), MessageQueueAccessToken: s.token,
		Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func (s *fakeSession) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSession) lastCapacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.capacities) == 0 {
		return -1
	}
	return s.capacities[len(s.capacities)-1]
}

// ---- messages ----

var reqID int64

func msgStats(assigned int, available int) *scaleset.RunnerScaleSetMessage {
	m := &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}}
	for range available {
		reqID++
		m.JobAvailableMessages = append(m.JobAvailableMessages, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: reqID}})
	}
	return m
}

func msgStarted(runner, job string) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{
		{RunnerName: runner, JobMessageBase: scaleset.JobMessageBase{JobID: job}}}}
}

func msgCompleted(runner, job string, assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned},
		JobCompletedMessages: []*scaleset.JobCompleted{
			{RunnerName: runner, Result: "succeeded", JobMessageBase: scaleset.JobMessageBase{JobID: job}}}}
}

// ---- harness ----

type harness struct {
	t      *testing.T
	ctl    *Controller
	docker *fakeDocker
	gh     *fakeGitHub
	st     *store.Store
	cfg    *config.Config
	logs   *syncBuffer
	red    *logging.Redactor
	cancel context.CancelFunc
	done   chan error
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func testConfig(slots int) *config.Config {
	return &config.Config{
		InstanceID: "test",
		Repos: []config.Repo{
			{Repository: "tim/a", Name: "a-set"},
			{Repository: "tim/b", Name: "b-set"},
		},
		Token:            "ghp_testtokenvalue123456",
		Slots:            slots,
		RunnerNanoCPUs:   2e9,
		RunnerMemory:     4 << 30,
		ShutdownTimeout:  2 * time.Second,
		HealthThreshold:  time.Minute,
		ReadinessTimeout: 2 * time.Second,
		RefreshInterval:  7 * 24 * time.Hour,
		Runtime:          "sysbox-runc",
	}
}

// newHarness builds a Controller on fakes; prepare may adjust them before Run.
func newHarness(t *testing.T, cfg *config.Config, prepare func(h *harness)) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"), cfg.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, docker: newFakeDocker(), gh: newFakeGitHub(), st: st, cfg: cfg, logs: &syncBuffer{}, red: logging.NewRedactor()}
	for _, s := range cfg.Secrets() {
		h.red.Add(s)
	}
	if prepare != nil {
		prepare(h)
	}
	var w io.Writer = h.logs
	h.ctl = New(Options{
		Config: cfg, Docker: h.docker, GitHub: h.gh, Store: st, Redactor: h.red,
		Logger:               logging.NewLogger(w, slog.LevelDebug, h.red),
		Delay:                func(int) time.Duration { return 10 * time.Millisecond },
		StaleCheckInterval:   10 * time.Millisecond,
		RefreshCheckInterval: time.Hour,
		CompletedGrace:       5 * time.Second,
		FinalCleanupTimeout:  time.Second,
	})
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.ctl.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Error("controller did not stop")
		}
	})
}

// stop cancels the Controller and waits for Run to return.
func (h *harness) stop() error {
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err // for Cleanup
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("controller did not stop")
		return nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) healthy() bool {
	ok, _ := h.ctl.Health()
	return ok
}
