// Package dockerx adapts the Docker Engine API to controller.Docker.
package dockerx

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/tim/scaleset-runner/internal/assets"
	"github.com/tim/scaleset-runner/internal/controller"
)

type Docker struct {
	cli *client.Client
}

// New connects to the Docker Engine from the environment (DOCKER_HOST or the
// default socket) and checks that it answers and has the runtime.
func New(ctx context.Context, runtime string) (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := cli.Info(pctx)
	if err != nil {
		return nil, fmt.Errorf("docker engine unreachable: %w", err)
	}
	if _, ok := info.Runtimes[runtime]; !ok {
		return nil, fmt.Errorf("docker engine has no runtime %q; install sysbox (C-003)", runtime)
	}
	return &Docker{cli: cli}, nil
}

var _ controller.Docker = (*Docker)(nil)

var ownIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// ownContainerID finds the ID of the container this process runs in, if any.
func ownContainerID() string {
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := ownIDRe.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// InstanceInUse reports whether another running container on the Host runs a
// Controller with the same Instance ID (FR-038). It reads the environment of
// running containers and never logs it.
func (d *Docker) InstanceInUse(ctx context.Context, envName, instanceID string) (string, error) {
	self := ownContainerID()
	host, _ := os.Hostname()
	list, err := d.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return "", err
	}
	want := envName + "=" + instanceID
	for _, c := range list {
		if c.ID == self || (self == "" && strings.HasPrefix(c.ID, host)) {
			continue
		}
		if c.Labels[controller.LabelInstance] != "" {
			continue // a Runner Container, not a Controller
		}
		insp, err := d.cli.ContainerInspect(ctx, c.ID)
		if err != nil || insp.Config == nil {
			continue
		}
		for _, e := range insp.Config.Env {
			if e == want {
				return strings.TrimPrefix(insp.Name, "/"), nil
			}
		}
	}
	return "", nil
}

func (d *Docker) ListRunnerContainers(ctx context.Context, instanceID string) ([]string, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", controller.LabelInstance+"="+instanceID)),
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	return ids, nil
}

func (d *Docker) CreateRunner(ctx context.Context, spec controller.RunnerSpec) (string, error) {
	resp, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      spec.ImageID,
			User:       "0",
			Entrypoint: []string{"/scaleset-runner/entrypoint.sh"},
			Cmd:        []string{},
			Env:        []string{"SCALESET_READY_MARKER=" + assets.ReadyMarker},
			Labels: map[string]string{
				controller.LabelInstance:   spec.InstanceID,
				controller.LabelRepository: spec.Repository,
				controller.LabelRunner:     spec.Name,
			},
		},
		&container.HostConfig{
			// NFR-006: sysbox isolation, no privileges, no Docker socket, no
			// host mounts or shared volumes.
			Runtime:    spec.Runtime,
			Privileged: false,
			Resources: container.Resources{
				NanoCPUs: spec.NanoCPUs,
				Memory:   spec.Memory,
			},
			LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "2"}},
		},
		nil, nil, spec.Name)
	if err != nil {
		return "", err
	}
	archive, err := runnerFiles(spec.JITConfig)
	if err != nil {
		return resp.ID, err
	}
	if err := d.cli.CopyToContainer(ctx, resp.ID, "/", archive, container.CopyToContainerOptions{}); err != nil {
		return resp.ID, fmt.Errorf("copy entrypoint: %w", err)
	}
	return resp.ID, nil
}

// runnerFiles returns a tar with the entrypoint and the JIT Config, which the
// entrypoint reads and deletes; the JIT Config never appears in the
// container's environment or its inspect output.
func runnerFiles(jit string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{"scaleset-runner/entrypoint.sh", 0o755, assets.Entrypoint},
		{"scaleset-runner/jitconfig", 0o600, []byte(jit)},
	}
	if err := tw.WriteHeader(&tar.Header{Name: "scaleset-runner/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: now}); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: now}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}

func (d *Docker) StartContainer(ctx context.Context, id string) error {
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (d *Docker) WaitReady(ctx context.Context, id string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return err
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		pw.CloseWithError(err)
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == assets.ReadyMarker {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("container stopped before it was ready")
}

func (d *Docker) WaitExit(ctx context.Context, id string) (int64, error) {
	okCh, errCh := d.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case r := <-okCh:
		return r.StatusCode, nil
	case err := <-errCh:
		if errdefs.IsNotFound(err) {
			return -1, nil
		}
		return -1, err
	}
}

func (d *Docker) RemoveContainer(ctx context.Context, id string) error {
	err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *Docker) ImageID(ctx context.Context, ref string) (string, time.Time, error) {
	insp, err := d.cli.ImageInspect(ctx, ref)
	if errdefs.IsNotFound(err) {
		return "", time.Time{}, controller.ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	created, _ := time.Parse(time.RFC3339Nano, insp.Created)
	return insp.ID, created, nil
}

func (d *Docker) BuildDefaultImage(ctx context.Context, tag string) (string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(assets.RunnerDockerfile)), ModTime: time.Now()}); err != nil {
		return "", err
	}
	if _, err := tw.Write(assets.RunnerDockerfile); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	resp, err := d.cli.ImageBuild(ctx, &buf, build.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  "Dockerfile",
		PullParent:  true, // newest official base image (FR-030)
		NoCache:     true, // fresh packages on every refresh
		Remove:      true,
		ForceRemove: true,
		Labels:      map[string]string{"scaleset-runner.image": "default", assets.RecipeLabel: assets.RunnerRecipe},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, io.Discard, 0, false, nil); err != nil {
		return "", fmt.Errorf("build failed: %w", err)
	}
	id, _, err := d.ImageID(ctx, tag)
	return id, err
}

func (d *Docker) DefaultImageOutdated(ctx context.Context, tag string) (bool, error) {
	insp, err := d.cli.ImageInspect(ctx, tag)
	if errdefs.IsNotFound(err) {
		return false, controller.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	var labels map[string]string
	if insp.Config != nil {
		labels = insp.Config.Labels
	}
	return labels[assets.RecipeLabel] != assets.RunnerRecipe, nil
}

func (d *Docker) PullImage(ctx context.Context, ref string) (string, error) {
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		return "", fmt.Errorf("pull failed: %w", err)
	}
	id, _, err := d.ImageID(ctx, ref)
	return id, err
}

func (d *Docker) RemoveImage(ctx context.Context, id string) error {
	_, err := d.cli.ImageRemove(ctx, id, image.RemoveOptions{Force: false, PruneChildren: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}
