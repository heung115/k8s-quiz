package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const (
	createSpecLabel = "k8s-quiz.create-spec"
	recoveryTimeout = 5 * time.Second
)

var ErrOwnedAllocationAmbiguous = errors.New("owned allocation metadata is incomplete or ambiguous")

type DockerManager struct {
	dockerHost string
	cli        *client.Client
}

func NewDockerManager(dockerHost string) (*DockerManager, error) {
	opts := []client.Opt{}
	if dockerHost != "" {
		opts = append(opts, client.WithHost(dockerHost))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerManager{dockerHost: dockerHost, cli: cli}, nil
}

// ensureNetwork creates a dedicated Docker bridge network for the user if it
// does not exist yet. Each user gets their own network so containers from
// different users cannot reach each other. The network is NOT --internal:
// k3s refuses to boot without a default route ("no default routes found in
// /proc/net/route"), and outbound access is needed to pull images anyway.
func (m *DockerManager) ensureNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	result, err := m.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		if err := validateOwnedNetwork(result.Network, name, labels); err != nil {
			return "", err
		}
		return result.Network.ID, nil
	}
	if !cerrdefs.IsNotFound(err) {
		return "", fmt.Errorf("network inspect %s: %w", name, err)
	}
	created, err := m.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: labels,
	})
	if err != nil {
		createErr := fmt.Errorf("network create %s: %w", name, err)
		recoveryCtx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		result, inspectErr := m.cli.NetworkInspect(recoveryCtx, name, client.NetworkInspectOptions{})
		if inspectErr != nil {
			return "", errors.Join(createErr, fmt.Errorf("inspect ambiguous network %s: %w", name, inspectErr))
		}
		if validateErr := validateOwnedNetwork(result.Network, name, labels); validateErr != nil {
			return "", errors.Join(createErr, fmt.Errorf("refuse ambiguous network adoption: %w", validateErr))
		}
		return result.Network.ID, nil
	}
	if created.ID == "" {
		return "", errors.New("Docker returned an empty network id")
	}
	return created.ID, nil
}

func (m *DockerManager) removeNetwork(ctx context.Context, name string) error {
	if _, err := m.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("network remove %s: %w", name, err)
	}
	return nil
}

func (m *DockerManager) Create(ctx context.Context, opts CreateOpts) (string, error) {
	handle, err := m.createAllocation(ctx, opts, false)
	if err != nil {
		return "", err
	}
	return handle.ContainerID, nil
}

func (m *DockerManager) CreateOwnedAllocation(ctx context.Context, opts CreateOpts) (OwnedAllocationHandle, error) {
	if opts.Name == "" || !strings.HasPrefix(opts.Name, "k8s-quiz-") || opts.NetworkMode != opts.Name {
		return OwnedAllocationHandle{}, errors.New("owned allocation requires one deterministic container and network name")
	}
	return m.createAllocation(ctx, opts, true)
}

func (m *DockerManager) createAllocation(ctx context.Context, opts CreateOpts, exactOwned bool) (OwnedAllocationHandle, error) {
	labels, err := labelsForCreate(opts)
	if err != nil {
		return OwnedAllocationHandle{}, fmt.Errorf("fingerprint container create request: %w", err)
	}
	var networkID string
	if opts.NetworkMode != "" {
		networkID, err = m.ensureNetwork(ctx, opts.NetworkMode, labels)
		if err != nil {
			return OwnedAllocationHandle{}, fmt.Errorf("create network %s: %w", opts.NetworkMode, err)
		}
	}

	cfg := &container.Config{
		Image:  opts.Image,
		Labels: labels,
		Env:    opts.Env,
		Cmd:    []string{"server"},
	}

	// NOTE on isolation: sessions run as privileged k3s containers because
	// kubelet requires it. A privileged container on a shared host is a
	// container-escape risk, so production SHOULD run each session in an
	// isolated VM / gVisor / dedicated node instead of on the backend host.
	// The limits below (CPU, memory, pids) at least contain resource abuse.
	pids := int64(1024)
	hostCfg := &container.HostConfig{
		Privileged: opts.Privileged,
		// k3s kubelet needs to enter the host cgroup namespace on cgroup v2
		// hosts (e.g. Docker Desktop); without this the node never becomes Ready.
		CgroupnsMode: container.CgroupnsModeHost,
		Resources: container.Resources{
			NanoCPUs:  opts.CPULimit,
			Memory:    opts.MemoryLimit,
			PidsLimit: &pids,
		},
	}

	var netCfg *network.NetworkingConfig
	if opts.NetworkMode != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				opts.NetworkMode: {},
			},
		}
	}

	resp, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
		Name:             opts.Name,
	})
	if err != nil {
		createErr := fmt.Errorf("container create: %w", err)
		if !exactOwned {
			if networkID != "" {
				createErr = errors.Join(createErr, m.removeNetwork(context.WithoutCancel(ctx), networkID))
			}
			return OwnedAllocationHandle{}, createErr
		}
		recoveryCtx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		handle, recoverErr := m.recoverAmbiguousCreate(recoveryCtx, opts, labels, networkID)
		if recoverErr == nil {
			return handle, nil
		}
		return OwnedAllocationHandle{}, errors.Join(createErr, recoverErr)
	}
	if resp.ID == "" {
		emptyIDErr := errors.New("Docker returned an empty container id")
		if exactOwned {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
			defer cancel()
			handle, recoverErr := m.recoverAmbiguousCreate(recoveryCtx, opts, labels, networkID)
			if recoverErr == nil {
				return handle, nil
			}
			return OwnedAllocationHandle{}, errors.Join(emptyIDErr, recoverErr)
		}
		if networkID != "" {
			emptyIDErr = errors.Join(emptyIDErr, m.removeNetwork(context.WithoutCancel(ctx), networkID))
		}
		return OwnedAllocationHandle{}, emptyIDErr
	}
	handle := OwnedAllocationHandle{ContainerID: resp.ID, NetworkID: networkID}

	if _, err := m.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		startErr := fmt.Errorf("container start: %w", err)
		recoveryCtx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		var cleanupErr error
		if exactOwned {
			_, cleanupErr = m.RemoveOwnedAllocation(recoveryCtx, OwnedAllocationTarget{Handle: handle, Create: opts})
		} else {
			if _, err := m.cli.ContainerRemove(recoveryCtx, handle.ContainerID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
				cleanupErr = fmt.Errorf("remove failed-start container %s: %w", handle.ContainerID, err)
			}
			if handle.NetworkID != "" {
				cleanupErr = errors.Join(cleanupErr, m.removeNetwork(recoveryCtx, handle.NetworkID))
			}
		}
		return OwnedAllocationHandle{}, errors.Join(startErr, cleanupErr)
	}

	return handle, nil
}

// recoverAmbiguousCreate resolves the only dangerous Docker create outcome:
// the daemon may have committed a container while the client lost or failed to
// decode the response. A deterministic name is not authority by itself, so the
// resource is adopted only after every immutable ownership and isolation field
// matches the request. Ambiguous or mismatched resources are preserved for
// scoped reconciliation instead of being deleted speculatively.
func (m *DockerManager) recoverAmbiguousCreate(ctx context.Context, opts CreateOpts, labels map[string]string, networkID string) (OwnedAllocationHandle, error) {
	result, err := m.cli.ContainerInspect(ctx, opts.Name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			var cleanupErr error
			if networkID != "" {
				_, cleanupErr = m.RemoveOwnedAllocation(ctx, OwnedAllocationTarget{
					Handle: OwnedAllocationHandle{NetworkID: networkID}, Create: opts,
				})
			}
			return OwnedAllocationHandle{}, errors.Join(errors.New("container create was not committed"), cleanupErr)
		}
		return OwnedAllocationHandle{}, fmt.Errorf("inspect ambiguous container %s: %w", opts.Name, err)
	}
	info := result.Container
	if err := validateRecoveredContainer(info, opts, labels); err != nil {
		return OwnedAllocationHandle{}, fmt.Errorf("refuse ambiguous container adoption: %w", err)
	}
	handle := OwnedAllocationHandle{ContainerID: info.ID}
	if opts.NetworkMode != "" {
		networkResult, err := m.cli.NetworkInspect(ctx, opts.NetworkMode, client.NetworkInspectOptions{})
		if err != nil {
			return OwnedAllocationHandle{}, fmt.Errorf("inspect ambiguous network %s: %w", opts.NetworkMode, err)
		}
		networkInfo := networkResult.Network
		if err := validateOwnedNetwork(networkInfo, opts.NetworkMode, labels); err != nil {
			return OwnedAllocationHandle{}, fmt.Errorf("refuse ambiguous network adoption: %w", err)
		}
		if networkID != "" && networkInfo.ID != networkID {
			return OwnedAllocationHandle{}, errors.New("refuse ambiguous network replacement adoption")
		}
		handle.NetworkID = networkInfo.ID
		endpoint := info.NetworkSettings.Networks[opts.NetworkMode]
		if endpoint == nil || endpoint.NetworkID == "" || endpoint.NetworkID != networkInfo.ID {
			return OwnedAllocationHandle{}, fmt.Errorf("container %s is not attached to the owned network", info.ID)
		}
	}
	if info.State == nil {
		return OwnedAllocationHandle{}, fmt.Errorf("container %s has no observable state", info.ID)
	}
	if info.State.Running {
		return handle, nil
	}
	if info.State.Status != "created" {
		return OwnedAllocationHandle{}, fmt.Errorf("container %s is in unexpected state %q", info.ID, info.State.Status)
	}
	if _, err := m.cli.ContainerStart(ctx, info.ID, client.ContainerStartOptions{}); err != nil {
		_, cleanupErr := m.RemoveOwnedAllocation(ctx, OwnedAllocationTarget{Handle: handle, Create: opts})
		return OwnedAllocationHandle{}, errors.Join(fmt.Errorf("start recovered container %s: %w", info.ID, err), cleanupErr)
	}
	return handle, nil
}

func labelsForCreate(opts CreateOpts) (map[string]string, error) {
	type fingerprintInput struct {
		Name        string
		Image       string
		Labels      map[string]string
		Env         []string
		CPULimit    int64
		MemoryLimit int64
		NetworkMode string
		Privileged  bool
	}
	baseLabels := maps.Clone(opts.Labels)
	if baseLabels == nil {
		baseLabels = make(map[string]string)
	}
	delete(baseLabels, createSpecLabel)
	payload, err := json.Marshal(fingerprintInput{
		Name: opts.Name, Image: opts.Image, Labels: baseLabels, Env: opts.Env,
		CPULimit: opts.CPULimit, MemoryLimit: opts.MemoryLimit,
		NetworkMode: opts.NetworkMode, Privileged: opts.Privileged,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	baseLabels[createSpecLabel] = hex.EncodeToString(sum[:])
	return baseLabels, nil
}

func validateRecoveredContainer(info container.InspectResponse, opts CreateOpts, labels map[string]string) error {
	if info.ID == "" || info.Name != "/"+opts.Name {
		return errors.New("container identity does not match deterministic name")
	}
	if info.Config == nil || info.Config.Image != opts.Image || !slices.Equal([]string(info.Config.Cmd), []string{"server"}) ||
		!slices.Equal(info.Config.Env, opts.Env) || !maps.Equal(info.Config.Labels, labels) {
		return errors.New("container image, command, environment, or ownership labels differ")
	}
	if info.HostConfig == nil || info.HostConfig.Privileged != opts.Privileged || !info.HostConfig.CgroupnsMode.IsHost() ||
		info.HostConfig.NanoCPUs != opts.CPULimit || info.HostConfig.Memory != opts.MemoryLimit ||
		info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != 1024 {
		return errors.New("container privilege or resource profile differs")
	}
	if hasUnexpectedHostAuthority(info.HostConfig) {
		return errors.New("container has unapproved host authority or lifecycle configuration")
	}
	if opts.NetworkMode != "" {
		if info.NetworkSettings == nil || len(info.NetworkSettings.Networks) != 1 || info.NetworkSettings.Networks[opts.NetworkMode] == nil {
			return errors.New("container network attachment differs")
		}
	}
	return nil
}

// hasUnexpectedHostAuthority rejects every security-sensitive HostConfig
// capability that the canonical create request leaves unset. Docker may
// normalize unrelated daemon defaults during inspection, so this deliberately
// compares authority-bearing fields instead of the complete HostConfig value.
func hasUnexpectedHostAuthority(host *container.HostConfig) bool {
	privatePID := host.PidMode == "" || host.PidMode == "private"
	privateIPC := host.IpcMode.IsEmpty() || host.IpcMode.IsPrivate()
	privateUTS := host.UTSMode == "" || host.UTSMode == "private"
	privateUserNS := host.UsernsMode == "" || host.UsernsMode == "private"
	defaultRuntime := host.Runtime == "" || host.Runtime == "runc"
	return len(host.Binds) != 0 || len(host.Mounts) != 0 || len(host.PortBindings) != 0 || host.PublishAllPorts ||
		len(host.CapAdd) != 0 || len(host.CapDrop) != 0 || len(host.Devices) != 0 ||
		len(host.DeviceCgroupRules) != 0 || len(host.DeviceRequests) != 0 || len(host.SecurityOpt) != 0 ||
		!host.RestartPolicy.IsNone() || host.RestartPolicy.MaximumRetryCount != 0 || host.ReadonlyRootfs ||
		host.AutoRemove || !privatePID || !privateIPC || !privateUTS || !privateUserNS ||
		len(host.GroupAdd) != 0 || len(host.VolumesFrom) != 0 || len(host.Links) != 0 ||
		len(host.ExtraHosts) != 0 || len(host.Tmpfs) != 0 || len(host.Sysctls) != 0 ||
		len(host.StorageOpt) != 0 || !defaultRuntime
}

func validateOwnedNetwork(info network.Inspect, name string, labels map[string]string) error {
	if info.Name != name || info.ID == "" || info.Driver != "bridge" || info.Internal || info.Ingress || info.ConfigOnly || !maps.Equal(info.Labels, labels) {
		return fmt.Errorf("network %q does not match the approved bridge and ownership labels", name)
	}
	return nil
}

func (m *DockerManager) Exec(ctx context.Context, containerID string, cmd []string) (ExecResult, error) {
	execResp, err := m.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := m.cli.ExecAttach(ctx, execResp.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()

	var stdout, stderr bytes.Buffer
	// Exec runs without a TTY, so the stream is multiplexed (8-byte header
	// per frame) and must be demultiplexed with stdcopy.
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader); err != nil && err != io.EOF {
		return ExecResult{}, fmt.Errorf("exec read: %w", err)
	}

	inspectResp, err := m.cli.ExecInspect(ctx, execResp.ID, client.ExecInspectOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec inspect: %w", err)
	}

	return ExecResult{
		ExitCode: inspectResp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (m *DockerManager) ExecInteractive(ctx context.Context, containerID string, cmd []string) (TerminalSession, error) {
	execConfig := client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          true,
	}
	execResp, err := m.cli.ExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := m.cli.ExecAttach(ctx, execResp.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	if _, err := m.cli.ExecResize(ctx, execResp.ID, client.ExecResizeOptions{Width: 80, Height: 24}); err != nil {
		attachResp.Close()
		return nil, fmt.Errorf("exec resize: %w", err)
	}

	go func() {
		_, _ = m.cli.ExecStart(ctx, execResp.ID, client.ExecStartOptions{Detach: false, TTY: true})
	}()

	return &dockerTerminal{
		conn:   attachResp.Conn,
		cli:    m.cli,
		execID: execResp.ID,
	}, nil
}

type dockerTerminal struct {
	conn   net.Conn
	cli    *client.Client
	execID string
}

func (t *dockerTerminal) Read(p []byte) (int, error)  { return t.conn.Read(p) }
func (t *dockerTerminal) Write(p []byte) (int, error) { return t.conn.Write(p) }
func (t *dockerTerminal) Close() error                { return t.conn.Close() }

func (t *dockerTerminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := t.cli.ExecResize(ctx, t.execID, client.ExecResizeOptions{
		Width:  uint(cols),
		Height: uint(rows),
	})
	return err
}

func (m *DockerManager) Remove(ctx context.Context, containerID string) error {
	networkName := m.getContainerNetwork(ctx, containerID)
	return m.RemoveAllocation(ctx, containerID, networkName)
}

// RemoveAllocation removes both parts of a LocalDockerRunner allocation using
// an explicit, deterministic network name. Supplying the name lets retries
// remove an orphan network even after the container is already absent.
func (m *DockerManager) RemoveAllocation(ctx context.Context, containerID, networkName string) error {
	var errs []error
	if _, err := m.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("container remove %s: %w", containerID, err))
	}
	if networkName != "" && strings.HasPrefix(networkName, "k8s-quiz-") {
		if err := m.removeNetwork(ctx, networkName); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *DockerManager) getContainerNetwork(ctx context.Context, containerID string) string {
	result, err := m.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	info := result.Container
	if err != nil || info.NetworkSettings == nil {
		return ""
	}
	for name := range info.NetworkSettings.Networks {
		return name
	}
	return ""
}

func (m *DockerManager) Logs(ctx context.Context, containerID string) (string, error) {
	rc, err := m.cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "100",
	})
	if err != nil {
		return "", err
	}
	defer rc.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, rc); err != nil && err != io.EOF {
		return "", err
	}
	return stdout.String() + stderr.String(), nil
}

func (m *DockerManager) WaitReady(ctx context.Context, containerID string, check func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for container ready")
			}
			if check() {
				return nil
			}
		}
	}
}

func (m *DockerManager) IsRunning(ctx context.Context, containerID string) (bool, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	info := result.Container
	return info.State != nil && info.State.Running, nil
}

func (m *DockerManager) RemoveByLabel(ctx context.Context, key, value string) error {
	return m.RemoveByLabels(ctx, map[string]string{key: value})
}

func (m *DockerManager) RemoveByLabels(ctx context.Context, labels map[string]string) error {
	filterArgs := make(client.Filters)
	for key, value := range labels {
		filterArgs.Add("label", fmt.Sprintf("%s=%s", key, value))
	}
	list, err := m.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range list.Items {
		if err := m.Remove(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// InspectOwnedAllocation binds physical identity to the trusted create
// specification. A supplied handle is inspected by ID; deterministic names
// are checked only to detect replacements, never to authorize their deletion.
func (m *DockerManager) InspectOwnedAllocation(ctx context.Context, target OwnedAllocationTarget) (OwnedAllocationObservation, error) {
	observation, _, err := m.inspectOwnedAllocation(ctx, target)
	return observation, err
}

// RemoveOwnedAllocation deletes only the IDs proven by the preflight and then
// re-inspects those IDs plus their deterministic names. A same-name
// replacement therefore makes the result ambiguous without being mutated.
func (m *DockerManager) RemoveOwnedAllocation(ctx context.Context, target OwnedAllocationTarget) (OwnedAllocationRemoval, error) {
	before, resolved, err := m.inspectOwnedAllocation(ctx, target)
	removal := OwnedAllocationRemoval{Before: before}
	if err != nil {
		return removal, err
	}
	if !before.OwnershipComplete {
		return removal, ErrOwnedAllocationAmbiguous
	}
	var errs []error
	if resolved.ContainerID != "" {
		if _, err := m.cli.ContainerRemove(ctx, resolved.ContainerID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove exactly owned container %s: %w", resolved.ContainerID, err))
		}
	}
	if resolved.NetworkID != "" {
		if err := m.removeNetwork(ctx, resolved.NetworkID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return removal, err
	}
	after, _, err := m.inspectOwnedAllocation(ctx, OwnedAllocationTarget{Handle: resolved, Create: target.Create})
	removal.After = after
	if err != nil {
		return removal, err
	}
	if !after.OwnershipComplete || after.ContainerPresent || after.NetworkPresent {
		return removal, ErrOwnedAllocationAmbiguous
	}
	return removal, nil
}

func (m *DockerManager) inspectOwnedAllocation(ctx context.Context, target OwnedAllocationTarget) (OwnedAllocationObservation, OwnedAllocationHandle, error) {
	observation := OwnedAllocationObservation{}
	opts := target.Create
	if opts.Name == "" || !strings.HasPrefix(opts.Name, "k8s-quiz-") || opts.NetworkMode != opts.Name {
		return observation, OwnedAllocationHandle{}, ErrOwnedAllocationAmbiguous
	}
	expectedLabels, err := labelsForCreate(opts)
	if err != nil {
		return observation, OwnedAllocationHandle{}, err
	}
	resolved := target.Handle

	containerLookup := opts.Name
	if target.Handle.ContainerID != "" {
		containerLookup = target.Handle.ContainerID
	}
	var containerInfo *container.InspectResponse
	containerResult, err := m.cli.ContainerInspect(ctx, containerLookup, client.ContainerInspectOptions{})
	if err == nil {
		info := containerResult.Container
		observation.ContainerPresent = true
		if target.Handle.ContainerID != "" && info.ID != target.Handle.ContainerID {
			return observation, resolved, nil
		}
		if err := validateRecoveredContainer(info, opts, expectedLabels); err != nil {
			return observation, resolved, nil
		}
		resolved.ContainerID = info.ID
		containerInfo = &info
	} else if !cerrdefs.IsNotFound(err) {
		return observation, resolved, fmt.Errorf("inspect owned container %s: %w", containerLookup, err)
	} else if target.Handle.ContainerID != "" {
		// The exact object is absent. A resource now occupying its deterministic
		// name is a replacement and must make absence proof ambiguous.
		_, replacementErr := m.cli.ContainerInspect(ctx, opts.Name, client.ContainerInspectOptions{})
		if replacementErr == nil {
			observation.ContainerPresent = true
			return observation, resolved, nil
		}
		if !cerrdefs.IsNotFound(replacementErr) {
			return observation, resolved, fmt.Errorf("inspect replacement container %s: %w", opts.Name, replacementErr)
		}
		resolved.ContainerID = ""
	}

	networkLookup := opts.NetworkMode
	if target.Handle.NetworkID != "" {
		networkLookup = target.Handle.NetworkID
	}
	var networkInfo *network.Inspect
	networkResult, err := m.cli.NetworkInspect(ctx, networkLookup, client.NetworkInspectOptions{})
	if err == nil {
		info := networkResult.Network
		observation.NetworkPresent = true
		if target.Handle.NetworkID != "" && info.ID != target.Handle.NetworkID {
			return observation, resolved, nil
		}
		if err := validateOwnedNetwork(info, opts.NetworkMode, expectedLabels); err != nil {
			return observation, resolved, nil
		}
		resolved.NetworkID = info.ID
		networkInfo = &info
	} else if !cerrdefs.IsNotFound(err) {
		return observation, resolved, fmt.Errorf("inspect owned network %s: %w", networkLookup, err)
	} else if target.Handle.NetworkID != "" {
		_, replacementErr := m.cli.NetworkInspect(ctx, opts.NetworkMode, client.NetworkInspectOptions{})
		if replacementErr == nil {
			observation.NetworkPresent = true
			return observation, resolved, nil
		}
		if !cerrdefs.IsNotFound(replacementErr) {
			return observation, resolved, fmt.Errorf("inspect replacement network %s: %w", opts.NetworkMode, replacementErr)
		}
		resolved.NetworkID = ""
	}

	if containerInfo != nil && networkInfo != nil {
		endpoint := containerInfo.NetworkSettings.Networks[opts.NetworkMode]
		if endpoint == nil || endpoint.NetworkID != networkInfo.ID {
			return observation, resolved, nil
		}
	}
	observation.OwnershipComplete = true
	return observation, resolved, nil
}

// RemoveNetworksByLabel removes owned networks left behind after an ambiguous
// create/destroy failure. It is intentionally scoped by an exact label filter.
func (m *DockerManager) RemoveNetworksByLabel(ctx context.Context, key, value string) error {
	return m.RemoveNetworksByLabels(ctx, map[string]string{key: value})
}

func (m *DockerManager) RemoveNetworksByLabels(ctx context.Context, labels map[string]string) error {
	filterArgs := make(client.Filters)
	for key, value := range labels {
		filterArgs.Add("label", fmt.Sprintf("%s=%s", key, value))
	}
	list, err := m.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: filterArgs,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range list.Items {
		if err := m.removeNetwork(ctx, n.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ResolveImage returns the immutable local content ID Docker will execute. The
// strict runtime problem loader includes this ID in the approved revision and
// passes the same ID to LocalDockerRunner, making the binding restart-stable.
func (m *DockerManager) ResolveImage(ctx context.Context, image string) (string, error) {
	info, err := m.cli.ImageInspect(ctx, image)
	if err != nil {
		return "", fmt.Errorf("inspect image %q: %w", image, err)
	}
	if !strings.HasPrefix(info.ID, "sha256:") {
		return "", fmt.Errorf("image %q resolved to non-content-addressed ID %q", image, info.ID)
	}
	return info.ID, nil
}
