package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Health reports whether the Controller serves every configured repository,
// every Scale Set polled within the health threshold, and no Runner Image is
// overdue (FR-022, UC-001 BR-005). problems names what is wrong, without secrets.
func (c *Controller) Health() (ok bool, problems []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, ss := range c.sets {
		switch {
		case ss.unserved != "":
			problems = append(problems, fmt.Sprintf("%s: not served (%s)", ss.repo.Repository, ss.unserved))
		case !ss.served:
			problems = append(problems, fmt.Sprintf("%s: scale set %q has no message session", ss.repo.Repository, ss.repo.Name))
		case now.Sub(ss.lastPoll) > c.cfg.HealthThreshold:
			problems = append(problems, fmt.Sprintf("%s: no successful poll for %s", ss.repo.Repository, now.Sub(ss.lastPoll).Round(1e9)))
		}
	}
	for _, im := range c.images {
		if im.overdue {
			problems = append(problems, fmt.Sprintf("runner image %s overdue", im.ref))
		}
	}
	sort.Strings(problems)
	return len(problems) == 0, problems
}

// HealthHandler serves the health status: 200 when healthy, 503 otherwise.
func (c *Controller) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ok, problems := c.Health()
		w.Header().Set("Content-Type", "application/json")
		status := "healthy"
		if !ok {
			status = "unhealthy"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "problems": problems})
	})
}

// jobBuckets are the upper bounds, in seconds, of the Job duration histogram.
var jobBuckets = []float64{60, 300, 600, 1200, 1800, 3600, 7200, 21600}

type jobStats struct {
	counts []uint64
	count  uint64
	sum    float64
}

func (j *jobStats) observe(seconds float64) {
	if j.counts == nil {
		j.counts = make([]uint64, len(jobBuckets))
	}
	for i, b := range jobBuckets {
		if seconds <= b {
			j.counts[i]++
		}
	}
	j.count++
	j.sum += seconds
}

// MetricsHandler serves Prometheus metrics for waiting start requests, busy
// Slots and Job durations per Scale Set (FR-026).
func (c *Controller) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		c.mu.Lock()
		fmt.Fprintf(&b, "# HELP scaleset_runner_slots Slot limit of the controller.\n# TYPE scaleset_runner_slots gauge\nscaleset_runner_slots %d\n", c.cfg.Slots)
		fmt.Fprintf(&b, "# HELP scaleset_runner_slots_busy_total Slots held by runner containers, leftovers included.\n# TYPE scaleset_runner_slots_busy_total gauge\nscaleset_runner_slots_busy_total %d\n", c.usedLocked())
		b.WriteString("# HELP scaleset_runner_start_requests_waiting Start requests waiting for a free slot.\n# TYPE scaleset_runner_start_requests_waiting gauge\n")
		for _, ss := range c.sets {
			fmt.Fprintf(&b, "scaleset_runner_start_requests_waiting{%s} %d\n", labels(ss), ss.queued)
		}
		b.WriteString("# HELP scaleset_runner_slots_busy Slots held by the runner containers of a scale set.\n# TYPE scaleset_runner_slots_busy gauge\n")
		for _, ss := range c.sets {
			fmt.Fprintf(&b, "scaleset_runner_slots_busy{%s} %d\n", labels(ss), len(ss.runners))
		}
		b.WriteString("# HELP scaleset_runner_assigned_jobs Assigned job count GitHub last reported.\n# TYPE scaleset_runner_assigned_jobs gauge\n")
		for _, ss := range c.sets {
			fmt.Fprintf(&b, "scaleset_runner_assigned_jobs{%s} %d\n", labels(ss), ss.assigned)
		}
		b.WriteString("# HELP scaleset_runner_job_duration_seconds Job durations from JobStarted to completion.\n# TYPE scaleset_runner_job_duration_seconds histogram\n")
		for _, ss := range c.sets {
			l := labels(ss)
			for i, bound := range jobBuckets {
				var n uint64
				if ss.jobs.counts != nil {
					n = ss.jobs.counts[i]
				}
				fmt.Fprintf(&b, "scaleset_runner_job_duration_seconds_bucket{%s,le=\"%g\"} %d\n", l, bound, n)
			}
			fmt.Fprintf(&b, "scaleset_runner_job_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", l, ss.jobs.count)
			fmt.Fprintf(&b, "scaleset_runner_job_duration_seconds_sum{%s} %g\n", l, ss.jobs.sum)
			fmt.Fprintf(&b, "scaleset_runner_job_duration_seconds_count{%s} %d\n", l, ss.jobs.count)
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(b.String()))
	})
}

func labels(ss *scaleSet) string {
	return fmt.Sprintf("repository=%q,scale_set=%q", ss.repo.Repository, ss.repo.Name)
}
