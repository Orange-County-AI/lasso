package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// tailnetDNSName returns this node's MagicDNS name (e.g.
// "citadel.tail9dd8e.ts.net") without the trailing dot, read from
// `tailscale status --json`. It's the hostname `tailscale serve` publishes on,
// so we surface it as the user-facing URL.
func tailnetDNSName() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parse tailscale status: %w", err)
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if name == "" {
		return "", fmt.Errorf("tailscale reported no MagicDNS name (is MagicDNS enabled on the tailnet?)")
	}
	return name, nil
}

// exposeOverTailnet publishes the loopback server on localPort to the tailnet via
// `tailscale serve`, terminating HTTPS on httpsPort (443 gives the clean
// https://<node>.ts.net URL; use another port when 443 is already taken on the
// box — e.g. by a Docker/Traefik :443 publish). Returns the public URL and a stop
// func that removes the route. tailscaled (running as root) terminates TLS with
// real certs, so lasso needs no privileged bind — but writing serve config
// requires the one-time `sudo tailscale set --operator=$USER`.
//
// We use `--bg` (persistent, queryable via `tailscale serve status`) and tear it
// down explicitly on shutdown, rather than foreground mode — foreground serve is
// silently shadowed when another process owns the HTTPS port and is harder to
// supervise.
func exposeOverTailnet(localPort, httpsPort int) (stop func(), url string, err error) {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return nil, "", fmt.Errorf("tailscale not found on PATH (install tailscale, or drop --tailscale)")
	}
	dns, err := tailnetDNSName()
	if err != nil {
		return nil, "", err
	}

	serveArgs := []string{"serve", "--bg"}
	if httpsPort != 443 {
		serveArgs = append(serveArgs, "--https="+strconv.Itoa(httpsPort))
	}
	serveArgs = append(serveArgs, strconv.Itoa(localPort))
	if out, err := exec.Command("tailscale", serveArgs...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "denied") {
			return nil, "", fmt.Errorf("`tailscale serve` was denied — grant this user permission once with:\n    sudo tailscale set --operator=$USER")
		}
		return nil, "", fmt.Errorf("tailscale serve: %v\n%s", err, msg)
	}

	url = "https://" + dns
	if httpsPort != 443 {
		url = fmt.Sprintf("https://%s:%d", dns, httpsPort)
	}

	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = exec.Command("tailscale", "serve", "--https="+strconv.Itoa(httpsPort), "off").Run()
		})
	}
	return stop, url, nil
}
