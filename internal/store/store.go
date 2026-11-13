// Package store is the Controller's State Store (C-007): a SQLite database
// recording the Scale Sets, Runner Registrations, Runner Containers and Runner
// Images it created, as docs/entity_model.md describes.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS controller (
	id          INTEGER PRIMARY KEY,
	instance_id TEXT NOT NULL UNIQUE,
	created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS scale_set (
	id                  INTEGER PRIMARY KEY,
	controller_id       INTEGER NOT NULL REFERENCES controller(id),
	github_scale_set_id INTEGER NOT NULL UNIQUE,
	repository          TEXT NOT NULL,
	name                TEXT NOT NULL,
	origin              TEXT NOT NULL CHECK (origin IN ('Created','TakenOver')),
	created_at          TEXT NOT NULL,
	UNIQUE (controller_id, repository, name)
);
CREATE TABLE IF NOT EXISTS runner_registration (
	id               INTEGER PRIMARY KEY,
	scale_set_id     INTEGER NOT NULL REFERENCES scale_set(id),
	github_runner_id INTEGER NOT NULL UNIQUE,
	runner_name      TEXT NOT NULL UNIQUE,
	created_at       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runner_image (
	id             INTEGER PRIMARY KEY,
	controller_id  INTEGER NOT NULL REFERENCES controller(id),
	reference      TEXT NOT NULL,
	kind           TEXT NOT NULL CHECK (kind IN ('Default','Repository')),
	digest         TEXT NOT NULL,
	status         TEXT NOT NULL CHECK (status IN ('Current','Superseded')),
	refreshed_at   TEXT NOT NULL,
	last_failed_at TEXT,
	UNIQUE (reference, digest)
);
CREATE UNIQUE INDEX IF NOT EXISTS runner_image_current ON runner_image(controller_id, reference) WHERE status = 'Current';
CREATE TABLE IF NOT EXISTS runner_container (
	id                     INTEGER PRIMARY KEY,
	runner_registration_id INTEGER NOT NULL UNIQUE REFERENCES runner_registration(id),
	runner_image_id        INTEGER NOT NULL REFERENCES runner_image(id),
	docker_container_id    TEXT NOT NULL UNIQUE,
	status                 TEXT NOT NULL CHECK (status IN ('Idle','Busy','Removing')),
	job_id                 TEXT,
	started_at             TEXT NOT NULL,
	ready_at               TEXT,
	job_started_at         TEXT
);
`

// ErrOtherInstance means the State Store belongs to another Controller.
var ErrOtherInstance = errors.New("state store belongs to another instance")

type Store struct {
	db           *sql.DB
	controllerID int64
	now          func() time.Time
}

// Open opens or creates the State Store at path for instanceID.
func Open(path, instanceID string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	var id int64
	var owner string
	err = db.QueryRow(`SELECT id, instance_id FROM controller LIMIT 1`).Scan(&id, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := db.Exec(`INSERT INTO controller(instance_id, created_at) VALUES (?, ?)`, instanceID, ts(s.now()))
		if err != nil {
			db.Close()
			return nil, err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		db.Close()
		return nil, err
	case owner != instanceID:
		db.Close()
		return nil, fmt.Errorf("%w %q", ErrOtherInstance, owner)
	}
	s.controllerID = id
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v.String)
	return t
}

// ---- Scale Sets ----

type ScaleSet struct {
	ID         int64
	GitHubID   int
	Repository string
	Name       string
	Origin     string
}

const (
	OriginCreated   = "Created"
	OriginTakenOver = "TakenOver"
)

func (s *Store) ScaleSets() ([]ScaleSet, error) {
	rows, err := s.db.Query(`SELECT id, github_scale_set_id, repository, name, origin FROM scale_set WHERE controller_id = ? ORDER BY id`, s.controllerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScaleSet
	for rows.Next() {
		var r ScaleSet
		if err := rows.Scan(&r.ID, &r.GitHubID, &r.Repository, &r.Name, &r.Origin); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InsertScaleSet(githubID int, repository, name, origin string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO scale_set(controller_id, github_scale_set_id, repository, name, origin, created_at) VALUES (?,?,?,?,?,?)`,
		s.controllerID, githubID, repository, name, origin, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteScaleSet(id int64) error {
	_, err := s.db.Exec(`DELETE FROM scale_set WHERE id = ?`, id)
	return err
}

// ---- Runner Registrations ----

type Registration struct {
	ID         int64
	ScaleSetID int64
	RunnerID   int64
	RunnerName string
	Repository string
}

func (s *Store) InsertRegistration(scaleSetID, runnerID int64, runnerName string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runner_registration(scale_set_id, github_runner_id, runner_name, created_at) VALUES (?,?,?,?)`,
		scaleSetID, runnerID, runnerName, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Registrations() ([]Registration, error) {
	rows, err := s.db.Query(`SELECT r.id, r.scale_set_id, r.github_runner_id, r.runner_name, ss.repository
		FROM runner_registration r JOIN scale_set ss ON ss.id = r.scale_set_id
		WHERE ss.controller_id = ? ORDER BY r.id`, s.controllerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		var r Registration
		if err := rows.Scan(&r.ID, &r.ScaleSetID, &r.RunnerID, &r.RunnerName, &r.Repository); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CountRegistrations(scaleSetID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM runner_registration WHERE scale_set_id = ?`, scaleSetID).Scan(&n)
	return n, err
}

func (s *Store) DeleteRegistration(id int64) error {
	_, err := s.db.Exec(`DELETE FROM runner_registration WHERE id = ?`, id)
	return err
}

// ---- Runner Containers ----

const (
	ContainerIdle     = "Idle"
	ContainerBusy     = "Busy"
	ContainerRemoving = "Removing"
)

type Container struct {
	ID             int64
	RegistrationID int64
	ImageID        int64
	DockerID       string
	Status         string
}

func (s *Store) InsertContainer(registrationID, imageID int64, dockerID string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runner_container(runner_registration_id, runner_image_id, docker_container_id, status, started_at) VALUES (?,?,?,?,?)`,
		registrationID, imageID, dockerID, ContainerIdle, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Containers() ([]Container, error) {
	rows, err := s.db.Query(`SELECT id, runner_registration_id, runner_image_id, docker_container_id, status FROM runner_container ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		var c Container
		if err := rows.Scan(&c.ID, &c.RegistrationID, &c.ImageID, &c.DockerID, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetContainerStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE runner_container SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) SetContainerReady(id int64) error {
	_, err := s.db.Exec(`UPDATE runner_container SET ready_at = ? WHERE id = ?`, ts(s.now()), id)
	return err
}

func (s *Store) SetContainerJob(id int64, jobID string) error {
	_, err := s.db.Exec(`UPDATE runner_container SET status = ?, job_id = ?, job_started_at = ? WHERE id = ?`,
		ContainerBusy, jobID, ts(s.now()), id)
	return err
}

func (s *Store) DeleteContainer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM runner_container WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteContainerByDockerID(dockerID string) error {
	_, err := s.db.Exec(`DELETE FROM runner_container WHERE docker_container_id = ?`, dockerID)
	return err
}

// ---- Runner Images ----

const (
	KindDefault     = "Default"
	KindRepository  = "Repository"
	ImageCurrent    = "Current"
	ImageSuperseded = "Superseded"
)

type Image struct {
	ID           int64
	Reference    string
	Kind         string
	Digest       string
	Status       string
	RefreshedAt  time.Time
	LastFailedAt time.Time
}

const imageCols = `id, reference, kind, digest, status, refreshed_at, last_failed_at`

func scanImage(sc interface{ Scan(...any) error }) (Image, error) {
	var im Image
	var refreshed, failed sql.NullString
	err := sc.Scan(&im.ID, &im.Reference, &im.Kind, &im.Digest, &im.Status, &refreshed, &failed)
	im.RefreshedAt = parseTS(refreshed)
	im.LastFailedAt = parseTS(failed)
	return im, err
}

// CurrentImage returns the Current version of reference, or nil.
func (s *Store) CurrentImage(reference string) (*Image, error) {
	row := s.db.QueryRow(`SELECT `+imageCols+` FROM runner_image WHERE controller_id = ? AND reference = ? AND status = ?`,
		s.controllerID, reference, ImageCurrent)
	im, err := scanImage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &im, nil
}

func (s *Store) Images(status string) ([]Image, error) {
	rows, err := s.db.Query(`SELECT `+imageCols+` FROM runner_image WHERE controller_id = ? AND status = ? ORDER BY id`, s.controllerID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		im, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, im)
	}
	return out, rows.Err()
}

// RecordRefresh records a successful build or pull of reference that produced
// digest at time at. The same digest only moves refreshed_at; a new digest
// supersedes the Current row and becomes Current itself.
func (s *Store) RecordRefresh(reference, kind, digest string, at time.Time) (*Image, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var curID int64
	var curDigest string
	err = tx.QueryRow(`SELECT id, digest FROM runner_image WHERE controller_id = ? AND reference = ? AND status = ?`,
		s.controllerID, reference, ImageCurrent).Scan(&curID, &curDigest)
	switch {
	case err == nil && curDigest == digest:
		if _, err := tx.Exec(`UPDATE runner_image SET refreshed_at = ?, last_failed_at = NULL WHERE id = ?`, ts(at), curID); err != nil {
			return nil, err
		}
	case err == nil || errors.Is(err, sql.ErrNoRows):
		if err == nil {
			if _, err := tx.Exec(`UPDATE runner_image SET status = ? WHERE id = ?`, ImageSuperseded, curID); err != nil {
				return nil, err
			}
		}
		// A digest seen before (e.g. a rollback upstream) becomes Current again.
		if _, err := tx.Exec(`DELETE FROM runner_image WHERE reference = ? AND digest = ?`, reference, digest); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO runner_image(controller_id, reference, kind, digest, status, refreshed_at) VALUES (?,?,?,?,?,?)`,
			s.controllerID, reference, kind, digest, ImageCurrent, ts(at)); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.CurrentImage(reference)
}

func (s *Store) RecordRefreshFailure(reference string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE runner_image SET last_failed_at = ? WHERE controller_id = ? AND reference = ? AND status = ?`,
		ts(at), s.controllerID, reference, ImageCurrent)
	return err
}

// ImageInUse reports whether any recorded Runner Container references the image row.
func (s *Store) ImageInUse(imageID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM runner_container WHERE runner_image_id = ?`, imageID).Scan(&n)
	return n > 0, err
}

func (s *Store) DeleteImage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM runner_image WHERE id = ?`, id)
	return err
}
