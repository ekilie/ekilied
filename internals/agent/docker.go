package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type DockerService struct {
	cli   *client.Client
	agent *Ekilied
}

func NewDockerService(agent *Ekilied) (*DockerService, error) {
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &DockerService{cli: cli, agent: agent}, nil
}

func (s *DockerService) ListContainers(ctx context.Context) ([]types.Container, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{})
}

func (s *DockerService) StreamLogs(ctx context.Context, containerName string, tail int, logCh chan<- string) error {
	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
		Tail:       fmt.Sprintf("%d", tail),
	}

	reader, err := s.cli.ContainerLogs(ctx, containerName, options)
	if err != nil {
		return fmt.Errorf("container logs: %w", err)
	}
	defer reader.Close()

	_, err = stdcopy.StdCopy(
		&logWriter{name: containerName, stream: "stdout", ch: logCh},
		&logWriter{name: containerName, stream: "stderr", ch: logCh},
		reader,
	)
	return err
}

func (s *DockerService) Close() error {
	return s.cli.Close()
}

type logWriter struct {
	name   string
	stream string
	ch     chan<- string
}

func (w *logWriter) Write(p []byte) (int, error) {
	line := string(p)
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if line != "" {
		select {
		case w.ch <- line:
		default:
			log.Println("docker log channel full, dropping line")
		}
	}
	return len(p), nil
}

type containerInfo struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Image  string   `json:"image"`
	State  string   `json:"state"`
	Status string   `json:"status"`
	Ports  []string `json:"ports,omitempty"`
	Uptime string   `json:"uptime,omitempty"`
}

func containerToInfo(c types.Container) containerInfo {
	ports := make([]string, 0, len(c.Ports))
	for _, p := range c.Ports {
		if p.PublicPort > 0 {
			ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
		} else {
			ports = append(ports, fmt.Sprintf("%d/%s", p.PrivatePort, p.Type))
		}
	}
	name := c.Names[0]
	if len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	uptime := ""
	if c.State == "running" && c.Created > 0 {
		uptime = time.Since(time.Unix(c.Created, 0)).Round(time.Second).String()
	}
	return containerInfo{
		ID:     c.ID[:12],
		Name:   name,
		Image:  c.Image,
		State:  c.State,
		Status: c.Status,
		Ports:  ports,
		Uptime: uptime,
	}
}
