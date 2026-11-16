package saver

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os/exec"
)

// StartHeadroom runs `headroom proxy --port N [--code-aware]` (as 9Router's
// dashboard does) when autostart is on, the URL is local and the headroom CLI
// is already installed. The process stops when ctx ends. Nothing is installed.
func (s *Saver) StartHeadroom(ctx context.Context) (*exec.Cmd, error) {
	if s == nil || s.Headroom == nil || !s.HeadroomAutostartWant {
		return nil, nil
	}
	u, err := url.Parse(s.HeadroomURL)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("headroom autostart only runs for a localhost URL")
	}
	if port == "" {
		port = "8787"
	}
	bin, err := exec.LookPath("headroom")
	if err != nil {
		return nil, errors.New("headroom CLI not found on PATH (install it yourself, e.g. pip install headroom-ai[proxy])")
	}
	cmd := exec.CommandContext(ctx, bin, s.HeadroomArgs(port)...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd, nil
}

// HeadroomArgs are the `headroom proxy` arguments 9Router uses for these settings.
func (s *Saver) HeadroomArgs(port string) []string {
	args := []string{"proxy", "--port", port}
	if s.HeadroomCodeAware {
		args = append(args, "--code-aware")
	}
	if !s.HeadroomKompress {
		args = append(args, "--disable-kompress")
	}
	return args
}
