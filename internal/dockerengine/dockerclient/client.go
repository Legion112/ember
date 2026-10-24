package dockerclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/Legion112/ember/internal/dockerengine"
)

// Package dockerclient is the real Docker Engine API, adapted to
// dockerengine.API.
//
// It lives in its own package so the coverage gate can exclude it by name. It
// is deliberately branchless: everything that decides anything lives above it,
// against the interface, where dockerengine.RecordingEngine makes it testable
// without a daemon. The exclusion is only honest while this file stays thin.
//
// Client is the real Docker Engine API, adapted to the narrow API interface.
type Client struct{ c *client.Client }

// NewClient connects to the daemon the environment points at.
func NewClient() (*Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to the docker daemon: %w", err)
	}
	return &Client{c: c}, nil
}

// Close releases the daemon connection.
func (d *Client) Close() error { return d.c.Close() }

func (d *Client) Ping(ctx context.Context) error {
	if _, err := d.c.Ping(ctx); err != nil {
		return fmt.Errorf("ping the docker daemon: %w", err)
	}
	return nil
}

func (d *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.c.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("inspect image %s: %w", ref, err)
}

func (d *Client) NetworkCreate(ctx context.Context, name string, labels map[string]string) (string, error) {
	resp, err := d.c.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		// Not internal, and that is a decision rather than an oversight.
		//
		// An internal bridge has no NAT to the outside, so Docker silently
		// publishes nothing from it -- which made sshd unreachable and left
		// WaitReady polling for its full ten minutes with no clue as to why.
		//
		// It is also the wrong shape for the guarantee ember actually makes.
		// That guarantee is about *inbound*: nothing ember creates is
		// reachable from the internet, which is enforced by publishing only
		// 22 and only on 127.0.0.1. Outbound is a different question, and a
		// node that cannot reach the internet cannot pull a model, which is
		// the entire point of renting it.
		Internal: false,
		Labels:   labels,
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return resp.ID, nil
}

func (d *Client) NetworkRemove(ctx context.Context, id string) error {
	err := d.c.NetworkRemove(ctx, id)
	if err != nil && cerrdefs.IsNotFound(err) {
		return dockerengine.ErrNotFound
	}
	return err
}

func (d *Client) NetworkList(ctx context.Context, label string) ([]dockerengine.Network, error) {
	nets, err := d.c.NetworkList(ctx, network.ListOptions{Filters: labelFilter(label)})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	out := make([]dockerengine.Network, 0, len(nets))
	for _, n := range nets {
		out = append(out, dockerengine.Network{ID: n.ID, Name: n.Name, Labels: n.Labels})
	}
	return out, nil
}

func (d *Client) ContainerCreate(ctx context.Context, spec dockerengine.ContainerSpec) (string, error) {
	if err := dockerengine.RefuseHostNetwork(spec.NetworkMode); err != nil {
		return "", err
	}

	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range spec.PublishTCP {
		port, err := nat.NewPort("tcp", strconv.Itoa(int(p)))
		if err != nil {
			return "", fmt.Errorf("port %d: %w", p, err)
		}
		exposed[port] = struct{}{}
		// HostIP is loopback and HostPort is empty so the daemon chooses.
		// Binding 0.0.0.0 here would publish the node's sshd on the LAN,
		// which is the one thing this whole program is built not to do.
		bindings[port] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}}
	}

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}

	resp, err := d.c.ContainerCreate(ctx,
		&container.Config{
			Image:        spec.Image,
			Cmd:          spec.Cmd,
			Env:          env,
			Labels:       spec.Labels,
			ExposedPorts: exposed,
		},
		&container.HostConfig{
			PortBindings: bindings,
			Binds:        spec.Binds,
		},
		networkConfig(spec.Network),
		nil, spec.Name,
	)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", spec.Name, err)
	}
	return resp.ID, nil
}

func networkConfig(name string) *network.NetworkingConfig {
	if name == "" {
		return nil
	}
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{name: {}},
	}
}

func (d *Client) ContainerStart(ctx context.Context, id string) error {
	if err := d.c.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		if cerrdefs.IsNotFound(err) {
			return dockerengine.ErrNotFound
		}
		return fmt.Errorf("start container %s: %w", id, err)
	}
	return nil
}

func (d *Client) ContainerInspect(ctx context.Context, id string) (dockerengine.Container, error) {
	j, err := d.c.ContainerInspect(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return dockerengine.Container{}, dockerengine.ErrNotFound
		}
		return dockerengine.Container{}, fmt.Errorf("inspect container %s: %w", id, err)
	}

	ports := map[uint16]dockerengine.Binding{}
	for p, bs := range j.NetworkSettings.Ports {
		for _, b := range bs {
			host, err := strconv.ParseUint(b.HostPort, 10, 16)
			if err != nil {
				continue
			}
			// p.Int() comes from a port string Docker parsed, and host was
			// just parsed with a 16-bit bound. The check is here because a
			// narrowing that is only safe by construction is one nobody
			// re-checks when the construction changes.
			cp := p.Int()
			if cp < 1 || cp > 65535 {
				continue
			}
			ports[uint16(cp)] = dockerengine.Binding{IP: b.HostIP, Port: uint16(host)}
		}
	}
	ips := map[string]string{}
	for name, ep := range j.NetworkSettings.Networks {
		ips[name] = ep.IPAddress
	}

	return dockerengine.Container{
		ID:       j.ID,
		Name:     strings.TrimPrefix(j.Name, "/"),
		Image:    j.Config.Image,
		State:    j.State.Status,
		Labels:   j.Config.Labels,
		Ports:    ports,
		IPs:      ips,
		ExitCode: j.State.ExitCode,
	}, nil
}

func (d *Client) ContainerRemove(ctx context.Context, id string) error {
	err := d.c.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return dockerengine.ErrNotFound
		}
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

func (d *Client) ContainerList(ctx context.Context, label string) ([]dockerengine.Container, error) {
	cs, err := d.c.ContainerList(ctx, container.ListOptions{All: true, Filters: labelFilter(label)})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]dockerengine.Container, 0, len(cs))
	for _, c := range cs {
		// ContainerList's summary does not carry the port bindings reliably,
		// so each one is inspected. Trusting the summary is how a node that
		// has a published port reads as one that does not.
		full, err := d.ContainerInspect(ctx, c.ID)
		if err != nil {
			if errors.Is(err, dockerengine.ErrNotFound) {
				continue // it went away between listing and inspecting
			}
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

func (d *Client) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	resp, err := d.c.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("exec in %s: %w", id, err)
	}
	att, err := d.c.ContainerExecAttach(ctx, resp.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", fmt.Errorf("attach to exec in %s: %w", id, err)
	}
	defer att.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read exec output from %s: %w", id, err)
	}

	insp, err := d.c.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return "", fmt.Errorf("inspect exec in %s: %w", id, err)
	}
	if insp.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("exec %v in %s exited %d: %s",
			cmd, id, insp.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func labelFilter(label string) filters.Args {
	f := filters.NewArgs()
	if label != "" {
		f.Add("label", label)
	}
	return f
}

// Client must satisfy API.
var _ dockerengine.API = (*Client)(nil)
