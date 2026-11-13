package controller

import (
	"context"
	"errors"
	"time"

	"github.com/actions/scaleset"
)

// Errors the adapters map their failures to.
var (
	// ErrNotFound: the object (Scale Set, runner, container, image) does not exist.
	ErrNotFound = errors.New("not found")
	// ErrRunnerBusy: GitHub refused to delete a runner because it holds a Job.
	ErrRunnerBusy = errors.New("runner holds a job")
	// ErrUnauthorized: GitHub rejected the credentials.
	ErrUnauthorized = errors.New("credentials rejected")
)

// Labels on every Runner Container.
const (
	LabelInstance   = "scaleset-runner.instance"
	LabelRepository = "scaleset-runner.repository"
	LabelRunner     = "scaleset-runner.runner"
)

// RunnerSpec describes one Runner Container to start.
type RunnerSpec struct {
	Name       string
	ImageID    string
	InstanceID string
	Repository string
	JITConfig  string
	NanoCPUs   int64
	Memory     int64
	Runtime    string
}

// Docker is what the Controller needs from the Docker Engine.
type Docker interface {
	// ListRunnerContainers returns the IDs of all containers labelled with instanceID.
	ListRunnerContainers(ctx context.Context, instanceID string) ([]string, error)
	// CreateRunner creates the container, copies the entrypoint and JIT Config
	// into it, and returns its ID. It does not start it.
	CreateRunner(ctx context.Context, spec RunnerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	// WaitReady returns nil once the container printed the Ready marker, or an
	// error when it exited first or ctx ended.
	WaitReady(ctx context.Context, id string) error
	// WaitExit blocks until the container stopped and returns its exit code.
	WaitExit(ctx context.Context, id string) (int64, error)
	// RemoveContainer force-removes the container; a missing one is no error.
	RemoveContainer(ctx context.Context, id string) error

	// ImageID returns the ID and creation time of ref, or ErrNotFound.
	ImageID(ctx context.Context, ref string) (string, time.Time, error)
	// BuildDefaultImage builds the default Runner Image as tag from the newest base image.
	BuildDefaultImage(ctx context.Context, tag string) (string, error)
	// DefaultImageOutdated reports whether the default Runner Image tag was
	// built from another Dockerfile than this Controller version ships.
	DefaultImageOutdated(ctx context.Context, tag string) (bool, error)
	PullImage(ctx context.Context, ref string) (string, error)
	// RemoveImage removes an image by ID without force, so an image another
	// container or tag still uses stays.
	RemoveImage(ctx context.Context, id string) error
}

// GitHub is what the Controller needs from GitHub, per repository.
type GitHub interface {
	CheckAuth(ctx context.Context, repo string) error
	// ScaleSetByName returns nil, nil when no Scale Set has that name.
	ScaleSetByName(ctx context.Context, repo, name string) (*scaleset.RunnerScaleSet, error)
	// ScaleSetByID returns ErrNotFound when the Scale Set is gone.
	ScaleSetByID(ctx context.Context, repo string, id int) (*scaleset.RunnerScaleSet, error)
	CreateScaleSet(ctx context.Context, repo string, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateScaleSet(ctx context.Context, repo string, id int, ss *scaleset.RunnerScaleSet) error
	// DeleteScaleSet treats a missing Scale Set as deleted.
	DeleteScaleSet(ctx context.Context, repo string, id int) error
	GenerateJIT(ctx context.Context, repo string, scaleSetID int, runnerName string) (runnerID int64, jit string, err error)
	// RemoveRunner treats a missing runner as deleted and returns ErrRunnerBusy
	// when the runner holds a Job.
	RemoveRunner(ctx context.Context, repo string, runnerID int64) error
	OpenSession(ctx context.Context, repo string, scaleSetID int, owner string) (Session, error)
}

// Session is one Scale Set's message session; *scaleset.MessageSessionClient implements it.
type Session interface {
	GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error)
	DeleteMessage(ctx context.Context, messageID int) error
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	Session() scaleset.RunnerScaleSetSession
	Close(ctx context.Context) error
}
