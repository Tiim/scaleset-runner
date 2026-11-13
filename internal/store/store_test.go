package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "prod")
	if err != nil {
		t.Fatal(err)
	}
	ssID, err := s.InsertScaleSet(7, "tim/a", "orpheus", OriginCreated)
	if err != nil {
		t.Fatal(err)
	}
	regID, err := s.InsertRegistration(ssID, 99, "ssr-prod-abc")
	if err != nil {
		t.Fatal(err)
	}
	im, err := s.RecordRefresh("img:prod", KindDefault, "sha256:aaa", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cID, err := s.InsertContainer(regID, im.ID, "dockerid")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetContainerJob(cID, "job-1"); err != nil {
		t.Fatal(err)
	}
	regs, _ := s.Registrations()
	if len(regs) != 1 || regs[0].Repository != "tim/a" || regs[0].RunnerID != 99 {
		t.Fatalf("bad registrations %+v", regs)
	}

	// A new build supersedes the old one, which stays in use by the container.
	im2, err := s.RecordRefresh("img:prod", KindDefault, "sha256:bbb", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if im2.ID == im.ID {
		t.Fatal("expected a new current row")
	}
	sup, _ := s.Images(ImageSuperseded)
	if len(sup) != 1 || sup[0].Digest != "sha256:aaa" {
		t.Fatalf("bad superseded %+v", sup)
	}
	if used, _ := s.ImageInUse(sup[0].ID); !used {
		t.Fatal("superseded image should be in use")
	}
	// Same digest again only moves refreshed_at.
	if im3, _ := s.RecordRefresh("img:prod", KindDefault, "sha256:bbb", time.Now()); im3.ID != im2.ID {
		t.Fatal("same digest should keep the row")
	}
	s.Close()

	if _, err := Open(path, "staging"); !errors.Is(err, ErrOtherInstance) {
		t.Fatalf("expected ErrOtherInstance, got %v", err)
	}
	s, err = Open(path, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, _ := s.CountRegistrations(ssID); n != 1 {
		t.Fatalf("registration should persist, got %d", n)
	}
}

// GitHub numbers Scale Sets and runners per repository, so two repositories
// can carry the same IDs.
func TestSameGitHubIDsAcrossRepositories(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.InsertScaleSet(1, "tim/a", "orpheus", OriginCreated)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.InsertScaleSet(1, "tim/b", "orpheus", OriginCreated)
	if err != nil {
		t.Fatalf("second repository with the same scale set id: %v", err)
	}
	if _, err := s.InsertScaleSet(1, "tim/a", "other", OriginCreated); err == nil {
		t.Fatal("same scale set id twice in one repository should fail")
	}
	if _, err := s.InsertRegistration(a, 5, "r-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRegistration(b, 5, "r-b"); err != nil {
		t.Fatalf("same runner id in another scale set: %v", err)
	}
}

// A State Store created with the version 0 schema is migrated in place.
func TestMigrateFromVersion0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE controller (id INTEGER PRIMARY KEY, instance_id TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
CREATE TABLE scale_set (
	id INTEGER PRIMARY KEY, controller_id INTEGER NOT NULL REFERENCES controller(id),
	github_scale_set_id INTEGER NOT NULL UNIQUE, repository TEXT NOT NULL, name TEXT NOT NULL,
	origin TEXT NOT NULL CHECK (origin IN ('Created','TakenOver')), created_at TEXT NOT NULL,
	UNIQUE (controller_id, repository, name));
CREATE TABLE runner_registration (
	id INTEGER PRIMARY KEY, scale_set_id INTEGER NOT NULL REFERENCES scale_set(id),
	github_runner_id INTEGER NOT NULL UNIQUE, runner_name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
INSERT INTO controller VALUES (1, 'prod', '2026-01-01T00:00:00Z');
INSERT INTO scale_set VALUES (1, 1, 1, 'tim/a', 'orpheus', 'Created', '2026-01-01T00:00:00Z');
INSERT INTO runner_registration VALUES (1, 1, 7, 'r-a', '2026-01-01T00:00:00Z');
`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path, "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	regs, err := s.Registrations()
	if err != nil || len(regs) != 1 || regs[0].RunnerID != 7 || regs[0].Repository != "tim/a" {
		t.Fatalf("registrations after migration: %+v, %v", regs, err)
	}
	b, err := s.InsertScaleSet(1, "tim/b", "orpheus", OriginCreated)
	if err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	if _, err := s.InsertRegistration(b, 7, "r-b"); err != nil {
		t.Fatalf("insert registration after migration: %v", err)
	}
}
