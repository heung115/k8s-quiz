package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type DockerManager struct {
	dockerHost string
	cli        *client.Client
}

func NewDockerManager(dockerHost string) (*DockerManager, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if dockerHost != "" {
		opts = append(opts, client.WithHost(dockerHost))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.NewClientWithOpts(opts...)
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
func (m *DockerManager) ensureNetwork(ctx context.Context, name string) error {
	_, err := m.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("network inspect %s: %w", name, err)
	}
	_, err = m.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{"k8s-quiz": "true"},
	})
	if err != nil {
		return fmt.Errorf("network create %s: %w", name, err)
	}
	return nil
}

func (m *DockerManager) removeNetwork(ctx context.Context, name string) {
	_ = m.cli.NetworkRemove(ctx, name)
}

func (m *DockerManager) Create(ctx context.Context, opts CreateOpts) (string, error) {
	if opts.NetworkMode != "" {
		if err := m.ensureNetwork(ctx, opts.NetworkMode); err != nil {
			return "", fmt.Errorf("create network %s: %w", opts.NetworkMode, err)
		}
	}

	cfg := &container.Config{
		Image:  opts.Image,
		Labels: opts.Labels,
		Env:    opts.Env,
		Cmd:    []string{"server"},
	}

	hostCfg := &container.HostConfig{
		Privileged: opts.Privileged,
		// k3s kubelet needs to enter the host cgroup namespace on cgroup v2
		// hosts (e.g. Docker Desktop); without this the node never becomes Ready.
		CgroupnsMode: container.CgroupnsModeHost,
		Resources: container.Resources{
			NanoCPUs: opts.CPULimit,
			Memory:   opts.MemoryLimit,
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

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, "")
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("container start: %w", err)
	}

	return resp.ID, nil
}

func (m *DockerManager) Exec(ctx context.Context, containerID string, cmd []string) (ExecResult, error) {
	execResp, err := m.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := m.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
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

	inspectResp, err := m.cli.ContainerExecInspect(ctx, execResp.ID)
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
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}
	execResp, err := m.cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := m.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	if err := m.cli.ContainerExecResize(ctx, execResp.ID, container.ResizeOptions{Width: 80, Height: 24}); err != nil {
		attachResp.Close()
		return nil, fmt.Errorf("exec resize: %w", err)
	}

	go func() {
		_ = m.cli.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{Detach: false, Tty: true})
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
	return t.cli.ContainerExecResize(ctx, t.execID, container.ResizeOptions{
		Width:  uint(cols),
		Height: uint(rows),
	})
}

func (m *DockerManager) Remove(ctx context.Context, containerID string) error {
	networkName := m.getContainerNetwork(ctx, containerID)
	err := m.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	if networkName != "" && strings.HasPrefix(networkName, "k8s-quiz-") {
		m.removeNetwork(ctx, networkName)
	}
	return err
}

func (m *DockerManager) getContainerNetwork(ctx context.Context, containerID string) string {
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil || info.NetworkSettings == nil {
		return ""
	}
	for name := range info.NetworkSettings.Networks {
		return name
	}
	return ""
}

func (m *DockerManager) Logs(ctx context.Context, containerID string) (string, error) {
	rc, err := m.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
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
	info, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return false, err
	}
	return info.State != nil && info.State.Running, nil
}

func (m *DockerManager) RemoveByLabel(ctx context.Context, key, value string) error {
	list, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", fmt.Sprintf("%s=%s", key, value))),
	})
	if err != nil {
		return err
	}
	for _, c := range list {
		m.Remove(ctx, c.ID)
	}
	return nil
}
