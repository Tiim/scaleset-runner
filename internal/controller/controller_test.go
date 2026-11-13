package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tim/scaleset-runner/internal/store"
)

// runJob plays GitHub and the runner for the Job of runner name: it starts,
// the runner exits, and the next poll reports completion and the new count.
func runJob(h *harness, repo, name, job string, assignedAfter int) {
	s := h.gh.session(h.t, repo)
	s.push(msgStarted(name, job))
	h.docker.exitRunner(name, 0)
	s.push(msgCompleted(name, job, assignedAfter))
}

// UC-007, FR-008, FR-009, FR-010: Jobs from two Scale Sets share one Slot and
// are served in the order their start requests arose.
func TestSharedSlotServesJobsInOrder(t *testing.T) {
	h := newHarness(t, testConfig(1), nil)
	h.start()
	a, b := h.gh.session(t, "tim/a"), h.gh.session(t, "tim/b")
	eventually(t, "healthy", h.healthy)

	a.push(msgStats(2, 2))
	eventually(t, "first runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	b.push(msgStats(1, 1))
	eventually(t, "b waits", func() bool {
		h.ctl.mu.Lock()
		defer h.ctl.mu.Unlock()
		return h.ctl.sets[1].queued == 1
	})
	// Capacity: always the Slot limit, even with no Slot free.
	eventually(t, "capacities", func() bool { return a.lastCapacity() == 1 && b.lastCapacity() == 1 })

	remaining := map[string]int{"tim/a": 2, "tim/b": 1}
	for i := 0; i < 3; i++ {
		var name string
		eventually(t, fmt.Sprintf("runner %d", i+1), func() bool {
			l := h.docker.liveRunners()
			if len(l) == 1 {
				name = l[0]
				return true
			}
			return false
		})
		h.docker.mu.Lock()
		repo := h.docker.byName[name].spec.Repository
		h.docker.mu.Unlock()
		remaining[repo]--
		runJob(h, repo, name, fmt.Sprintf("job-%d", i), remaining[repo])
		eventually(t, "runner removed", func() bool { return !slices.Contains(h.docker.liveRunners(), name) })
	}
	eventually(t, "all slots free", func() bool { return h.docker.liveCount() == 0 })

	h.docker.mu.Lock()
	order, maxLive := h.docker.startedRepos, h.docker.maxLive
	h.docker.mu.Unlock()
	if want := []string{"tim/a", "tim/a", "tim/b"}; !slices.Equal(order, want) {
		t.Fatalf("start order %v, want %v", order, want)
	}
	if maxLive != 1 {
		t.Fatalf("slot limit exceeded: %d containers at once", maxLive)
	}
	if len(a.acquired) != 2 || len(b.acquired) != 1 {
		t.Fatalf("jobs not acquired: a=%v b=%v", a.acquired, b.acquired)
	}
	h.gh.mu.Lock()
	leftRunners := len(h.gh.runners)
	h.gh.mu.Unlock()
	if leftRunners != 0 {
		t.Fatalf("%d runner registrations left on GitHub", leftRunners)
	}
	if regs, _ := h.st.Registrations(); len(regs) != 0 {
		t.Fatalf("registrations left in store: %+v", regs)
	}
	if rows, _ := h.st.Containers(); len(rows) != 0 {
		t.Fatalf("containers left in store: %+v", rows)
	}
	if strings.Contains(h.logs.String(), "JITSECRET") || strings.Contains(h.logs.String(), "MQTOKEN") {
		t.Fatalf("secret in logs")
	}
	if !strings.Contains(h.logs.String(), `"job_id":"job-0"`) {
		t.Fatalf("job id missing from logs")
	}
}

// FR-009: a Scale Set keeps advertising the Slot limit while its only Slot is
// held by a Runner Container whose Job just completed, so GitHub assigns a Job
// queued in that window; it starts once the Slot frees up.
func TestCapacityStaysAtSlotLimitWhileSlotBusy(t *testing.T) {
	h := newHarness(t, testConfig(1), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	eventually(t, "healthy", h.healthy)

	a.push(msgStats(1, 1))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	first := h.docker.liveRunners()[0]
	a.push(msgStarted(first, "job-1"))
	a.push(msgCompleted(first, "job-1", 0))
	eventually(t, "job completed", func() bool {
		h.ctl.mu.Lock()
		defer h.ctl.mu.Unlock()
		r := h.ctl.sets[0].runners[first]
		return r != nil && r.completed
	})
	a.mu.Lock()
	from := len(a.capacities)
	a.mu.Unlock()
	eventually(t, "more polls", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.capacities) >= from+3
	})
	a.mu.Lock()
	caps := slices.Clone(a.capacities[from:])
	a.mu.Unlock()
	for _, c := range caps {
		if c != 1 {
			t.Fatalf("capacity %d advertised while the slot was busy, want 1 (all: %v)", c, caps)
		}
	}

	// GitHub assigns the next Job before the first container has exited.
	a.push(msgStats(1, 1))
	h.docker.exitRunner(first, 0)
	eventually(t, "second runner", func() bool {
		l := h.docker.liveRunners()
		return len(l) == 1 && l[0] != first
	})
}

// UC-008, FR-028: a cancelled Job's Idle Runner Container is removed after
// GitHub confirms the deletion of its registration.
func TestCancelledJobRemovesIdleRunner(t *testing.T) {
	h := newHarness(t, testConfig(2), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	name := h.docker.liveRunners()[0]
	id := h.gh.runnerID(name)

	a.push(msgStats(0, 0))
	eventually(t, "idle runner removed", func() bool { return h.docker.liveCount() == 0 })
	h.gh.mu.Lock()
	removed := slices.Contains(h.gh.removed, id)
	h.gh.mu.Unlock()
	if !removed {
		t.Fatal("registration not deleted")
	}
}

// UC-008 A3: GitHub refuses the deletion because the runner holds a Job; the
// Runner Container stays.
func TestIdleRunnerHoldingJobIsKept(t *testing.T) {
	h := newHarness(t, testConfig(2), nil)
	var refusals sync.WaitGroup
	refusals.Add(1)
	var once sync.Once
	h.gh.removeHook = func(int64) error {
		once.Do(refusals.Done)
		return fmt.Errorf("%w: job still running", ErrRunnerBusy)
	}
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	name := h.docker.liveRunners()[0]
	a.push(msgStats(0, 0))
	refusals.Wait()
	time.Sleep(50 * time.Millisecond)
	if len(h.docker.liveRunners()) != 1 {
		t.Fatal("busy runner was removed")
	}
	h.ctl.mu.Lock()
	ph := h.ctl.sets[0].runners[name].phase
	h.ctl.mu.Unlock()
	if ph != phaseBusy {
		t.Fatalf("phase %v, want busy", ph)
	}
}

// UC-007 A5: a Runner Container that cannot start is cleaned up and retried
// with a new JIT Config.
func TestStartFailureRetriesWithNewJIT(t *testing.T) {
	h := newHarness(t, testConfig(1), func(h *harness) { h.docker.createFailures = 2 })
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "runner after retries", func() bool { return len(h.docker.liveRunners()) == 1 })
	h.gh.mu.Lock()
	jits, removed := h.gh.jits, len(h.gh.removed)
	h.gh.mu.Unlock()
	if jits != 3 || removed != 2 {
		t.Fatalf("jits=%d removed=%d, want 3 and 2", jits, removed)
	}
}

// UC-007 A3/A4: JIT Config refused, then demand drops: the start request is dropped.
func TestJITRefusedThenDemandDrops(t *testing.T) {
	h := newHarness(t, testConfig(1), func(h *harness) { h.gh.jitFailures = 1000 })
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "retrying", func() bool {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		return h.gh.jitFailures < 997
	})
	a.push(msgStats(0, 0))
	eventually(t, "request dropped", func() bool {
		h.ctl.mu.Lock()
		defer h.ctl.mu.Unlock()
		return h.ctl.sets[0].queued == 0 && len(h.ctl.sets[0].runners) == 0
	})
	h.gh.mu.Lock()
	n := h.gh.jitFailures
	h.gh.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.jitFailures != n {
		t.Fatal("still retrying after demand dropped")
	}
}

// UC-007 A6: no Ready marker within the readiness timeout.
func TestReadinessTimeoutReplacesRunner(t *testing.T) {
	cfg := testConfig(1)
	cfg.ReadinessTimeout = 50 * time.Millisecond
	h := newHarness(t, cfg, func(h *harness) { h.docker.autoReady = false })
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "second runner", func() bool {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		return h.gh.jits >= 2 && len(h.gh.removed) >= 1
	})
	h.docker.mu.Lock()
	maxLive := h.docker.maxLive
	h.docker.mu.Unlock()
	if maxLive != 1 {
		t.Fatalf("slot limit exceeded: %d", maxLive)
	}
}

// UC-007 A9, FR-035: after an exit without JobCompleted, no replacement starts
// until a later poll reports the assigned Job count.
func TestExitWithoutCompletionWaitsForFreshDemand(t *testing.T) {
	h := newHarness(t, testConfig(2), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	h.docker.exitRunner(h.docker.liveRunners()[0], 1)
	eventually(t, "removed", func() bool { return h.docker.liveCount() == 0 })
	time.Sleep(100 * time.Millisecond)
	h.docker.mu.Lock()
	started := len(h.docker.startedRepos)
	h.docker.mu.Unlock()
	if started != 1 {
		t.Fatalf("replacement started without fresh statistics")
	}
	a.push(msgStats(1, 0)) // GitHub still reports the Job as assigned
	eventually(t, "replacement", func() bool { return len(h.docker.liveRunners()) == 1 })
}

// UC-007 A1, FR-033: a failed acquisition leaves the message unacknowledged
// and the Jobs are acquired on redelivery.
func TestAcquireFailureLeavesMessageUnacked(t *testing.T) {
	h := newHarness(t, testConfig(1), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.mu.Lock()
	a.acquireErrs = 1
	a.mu.Unlock()
	a.push(msgStats(1, 1))
	eventually(t, "acquired on retry", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.acquired) == 1 && len(a.acked) == 1
	})
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
}

// UC-004: shutdown removes Idle Runner Containers, waits for running Jobs,
// closes sessions, and leaves nothing behind.
func TestShutdownWaitsForRunningJobs(t *testing.T) {
	h := newHarness(t, testConfig(2), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(2, 2))
	eventually(t, "two runners", func() bool { return len(h.docker.liveRunners()) == 2 })
	busy := h.docker.liveRunners()[0]
	a.push(msgStarted(busy, "job-busy"))
	eventually(t, "busy", func() bool {
		h.ctl.mu.Lock()
		defer h.ctl.mu.Unlock()
		return h.ctl.sets[0].runners[busy].phase == phaseBusy
	})

	stopped := make(chan error, 1)
	go func() { stopped <- h.stop() }()
	eventually(t, "idle runner removed", func() bool { return len(h.docker.liveRunners()) == 1 })
	if h.docker.liveRunners()[0] != busy {
		t.Fatal("busy runner removed before its job finished")
	}
	h.docker.exitRunner(busy, 0)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if h.docker.liveCount() != 0 {
		t.Fatal("runner containers left")
	}
	if !a.closed {
		t.Fatal("session not closed")
	}
	if ss, _ := h.st.ScaleSets(); len(ss) != 2 {
		t.Fatal("shutdown must keep scale sets")
	}
}

// UC-004 A3: Jobs still running when the shutdown timeout expires are ended.
func TestShutdownTimeoutEndsJobs(t *testing.T) {
	cfg := testConfig(1)
	cfg.ShutdownTimeout = 100 * time.Millisecond
	h := newHarness(t, cfg, nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(1, 1))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	name := h.docker.liveRunners()[0]
	a.push(msgStarted(name, "job-long"))
	eventually(t, "busy", func() bool {
		h.ctl.mu.Lock()
		defer h.ctl.mu.Unlock()
		return h.ctl.sets[0].runners[name].phase == phaseBusy
	})
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if h.docker.liveCount() != 0 {
		t.Fatal("runner container left after timeout")
	}
	if !strings.Contains(h.logs.String(), "ending job") || !strings.Contains(h.logs.String(), "job-long") {
		t.Fatalf("ended job not logged")
	}
}

// UC-001: leftovers are removed, stale Scale Sets deleted after their
// registrations, labelled Scale Sets taken over, and foreign ones left alone.
func TestStartupRecovery(t *testing.T) {
	cfg := testConfig(1)
	var takenID, foreignID, staleID int
	h := newHarness(t, cfg, func(h *harness) {
		h.docker.leftovers = []string{"leftover-container"}
		takenID = h.gh.addScaleSet("tim/a", "a-set", "a-set", cfg.InstanceLabel())
		foreignID = h.gh.addScaleSet("tim/b", "b-set", "b-set")
		staleID = h.gh.addScaleSet("tim/old", "old-set", "old-set", cfg.InstanceLabel())
		ssID, err := h.st.InsertScaleSet(staleID, "tim/old", "old-set", store.OriginCreated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.InsertRegistration(ssID, 555, "ssr-test-old"); err != nil {
			t.Fatal(err)
		}
		h.gh.runners[555] = "ssr-test-old"
		var calls int
		h.gh.removeHook = func(id int64) error {
			if id == 555 {
				calls++
				if calls == 1 {
					return errors.New("GitHub timeout")
				}
			}
			return nil
		}
	})
	h.start()
	h.gh.session(t, "tim/a")

	eventually(t, "leftover removed", func() bool {
		h.docker.mu.Lock()
		defer h.docker.mu.Unlock()
		return slices.Contains(h.docker.removedLeft, "leftover-container")
	})
	eventually(t, "stale scale set deleted", func() bool {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		return slices.Contains(h.gh.deletedSS, staleID)
	})
	h.gh.mu.Lock()
	created, deleted := slices.Clone(h.gh.created), slices.Clone(h.gh.deletedSS)
	h.gh.mu.Unlock()
	if len(created) != 0 {
		t.Fatalf("created %v; expected take-over and no creation", created)
	}
	if slices.Contains(deleted, foreignID) || slices.Contains(deleted, takenID) {
		t.Fatal("deleted a scale set that is configured or foreign")
	}
	recs, _ := h.st.ScaleSets()
	if len(recs) != 1 || recs[0].GitHubID != takenID || recs[0].Origin != store.OriginTakenOver {
		t.Fatalf("records %+v", recs)
	}
	time.Sleep(50 * time.Millisecond)
	ok, problems := h.ctl.Health()
	if ok || len(problems) != 1 || !strings.Contains(problems[0], "tim/b") {
		t.Fatalf("health %v %v; want unhealthy for tim/b", ok, problems)
	}
}

// UC-001 A2: rejected credentials end the Controller.
func TestRejectedCredentialsExit(t *testing.T) {
	h := newHarness(t, testConfig(1), func(h *harness) { h.gh.authErr = fmt.Errorf("%w: 401", ErrUnauthorized) })
	h.start()
	select {
	case err := <-h.done:
		h.done <- err
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controller kept running")
	}
}

// UC-001 A6: a per-repository image that cannot be pulled leaves only that
// repository unserved until a pull succeeds.
func TestMissingRepositoryImageIsRetried(t *testing.T) {
	cfg := testConfig(1)
	cfg.Repos[1].Image = "ghcr.io/tim/special:1"
	h := newHarness(t, cfg, func(h *harness) { h.docker.pullErr = errors.New("registry down") })
	h.start()
	h.gh.session(t, "tim/a")
	time.Sleep(50 * time.Millisecond)
	if _, problems := h.ctl.Health(); len(problems) != 1 || !strings.Contains(problems[0], "tim/b") {
		t.Fatalf("problems %v", problems)
	}
	h.docker.mu.Lock()
	h.docker.pullErr = nil
	h.docker.mu.Unlock()
	h.gh.session(t, "tim/b")
	eventually(t, "healthy", h.healthy)
}

// UC-001 step 4: a missing default image is built.
func TestMissingDefaultImageIsBuilt(t *testing.T) {
	h := newHarness(t, testConfig(1), func(h *harness) { delete(h.docker.images, "scaleset-runner/runner:test") })
	h.start()
	eventually(t, "healthy", h.healthy)
	h.docker.mu.Lock()
	defer h.docker.mu.Unlock()
	if h.docker.builds != 1 {
		t.Fatalf("builds=%d", h.docker.builds)
	}
}

func TestMetrics(t *testing.T) {
	h := newHarness(t, testConfig(1), nil)
	h.start()
	a := h.gh.session(t, "tim/a")
	a.push(msgStats(2, 2))
	eventually(t, "runner", func() bool { return len(h.docker.liveRunners()) == 1 })
	rec := newRecorder()
	h.ctl.MetricsHandler().ServeHTTP(rec, nil)
	out := rec.String()
	for _, want := range []string{
		`scaleset_runner_start_requests_waiting{repository="tim/a",scale_set="a-set"} 1`,
		`scaleset_runner_slots_busy{repository="tim/a",scale_set="a-set"} 1`,
		`scaleset_runner_job_duration_seconds_count{repository="tim/a",scale_set="a-set"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
