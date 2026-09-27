//go:build !linux

package main

// restartSystemdServers is Linux-only; elsewhere there is no systemd to ask.
func restartSystemdServers(exe string, staleOnly bool) int { return 0 }
