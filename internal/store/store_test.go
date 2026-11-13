package store

import (
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
