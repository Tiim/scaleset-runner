// Package githubx adapts github.com/actions/scaleset to controller.GitHub,
// with one scaleset client per repository sharing the configured credentials.
package githubx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/actions/scaleset"

	"github.com/tim/scaleset-runner/internal/config"
	"github.com/tim/scaleset-runner/internal/controller"
)

// Version is reported to GitHub in the user agent.
var Version = "dev"

// defaultRunnerGroupID is the "Default" runner group of a repository.
const defaultRunnerGroupID = 1

type GitHub struct {
	cfg    *config.Config
	logger *slog.Logger

	mu      sync.Mutex
	clients map[string]*scaleset.Client
}

var _ controller.GitHub = (*GitHub)(nil)

func New(cfg *config.Config, logger *slog.Logger) *GitHub {
	return &GitHub{cfg: cfg, logger: logger, clients: map[string]*scaleset.Client{}}
}

func (g *GitHub) client(repo string) (*scaleset.Client, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.clients[repo]; ok {
		return c, nil
	}
	url := config.Repo{Repository: repo}.URL()
	info := scaleset.SystemInfo{System: "scaleset-runner", Version: Version, Subsystem: g.cfg.InstanceID}
	opts := []scaleset.HTTPOption{scaleset.WithLogger(g.logger)}
	var c *scaleset.Client
	var err error
	if g.cfg.Token != "" {
		c, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: url, PersonalAccessToken: g.cfg.Token, SystemInfo: info,
		}, opts...)
	} else {
		c, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: url,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID: g.cfg.AppClientID, InstallationID: g.cfg.AppInstallationID, PrivateKey: g.cfg.AppPrivateKey,
			},
			SystemInfo: info,
		}, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("create GitHub client for %s: %w", repo, err)
	}
	g.clients[repo] = c
	return c, nil
}

// mapErr translates scaleset errors into the controller's sentinel errors,
// keeping the original text for the log.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		return fmt.Errorf("%w: %w", controller.ErrRunnerBusy, err)
	case errors.Is(err, scaleset.RunnerNotFoundError), errors.Is(err, scaleset.NotFoundError):
		return fmt.Errorf("%w: %w", controller.ErrNotFound, err)
	case errors.Is(err, scaleset.UnauthorizedError):
		return fmt.Errorf("%w: %w", controller.ErrUnauthorized, err)
	default:
		return err
	}
}

func (g *GitHub) CheckAuth(ctx context.Context, repo string) error {
	c, err := g.client(repo)
	if err != nil {
		return err
	}
	_, err = c.ListRunnerScaleSets(ctx, defaultRunnerGroupID)
	return mapErr(err)
}

func (g *GitHub) ScaleSetByName(ctx context.Context, repo, name string) (*scaleset.RunnerScaleSet, error) {
	c, err := g.client(repo)
	if err != nil {
		return nil, err
	}
	ss, err := c.GetRunnerScaleSet(ctx, defaultRunnerGroupID, name)
	return ss, mapErr(err)
}

func (g *GitHub) ScaleSetByID(ctx context.Context, repo string, id int) (*scaleset.RunnerScaleSet, error) {
	c, err := g.client(repo)
	if err != nil {
		return nil, err
	}
	ss, err := c.GetRunnerScaleSetByID(ctx, id)
	if err == nil && ss == nil {
		return nil, controller.ErrNotFound
	}
	return ss, mapErr(err)
}

func (g *GitHub) CreateScaleSet(ctx context.Context, repo string, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	c, err := g.client(repo)
	if err != nil {
		return nil, err
	}
	ss.RunnerGroupID = defaultRunnerGroupID
	out, err := c.CreateRunnerScaleSet(ctx, ss)
	return out, mapErr(err)
}

func (g *GitHub) UpdateScaleSet(ctx context.Context, repo string, id int, ss *scaleset.RunnerScaleSet) error {
	c, err := g.client(repo)
	if err != nil {
		return err
	}
	ss.RunnerGroupID = defaultRunnerGroupID
	_, err = c.UpdateRunnerScaleSet(ctx, id, ss)
	return mapErr(err)
}

func (g *GitHub) DeleteScaleSet(ctx context.Context, repo string, id int) error {
	c, err := g.client(repo)
	if err != nil {
		return err
	}
	err = mapErr(c.DeleteRunnerScaleSet(ctx, id))
	if errors.Is(err, controller.ErrNotFound) {
		return nil
	}
	return err
}

func (g *GitHub) GenerateJIT(ctx context.Context, repo string, scaleSetID int, runnerName string) (int64, string, error) {
	c, err := g.client(repo)
	if err != nil {
		return 0, "", err
	}
	jit, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName}, scaleSetID)
	if err != nil {
		return 0, "", mapErr(err)
	}
	if jit == nil || jit.Runner == nil || jit.EncodedJITConfig == "" {
		return 0, "", errors.New("GitHub returned an empty JIT Config")
	}
	return int64(jit.Runner.ID), jit.EncodedJITConfig, nil
}

func (g *GitHub) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	c, err := g.client(repo)
	if err != nil {
		return err
	}
	err = mapErr(c.RemoveRunner(ctx, runnerID))
	if errors.Is(err, controller.ErrNotFound) {
		return nil
	}
	return err
}

func (g *GitHub) OpenSession(ctx context.Context, repo string, scaleSetID int, owner string) (controller.Session, error) {
	c, err := g.client(repo)
	if err != nil {
		return nil, err
	}
	s, err := c.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, mapErr(err)
	}
	return s, nil
}
