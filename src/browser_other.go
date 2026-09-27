//go:build !linux

package main

import "syscall"

// browserSysProcAttr: no Pdeathsig outside linux, so a lasso that dies without
// running its shutdown path leaves Chromium behind — which is what the pid
// file's stale-owner check in browser.go exists to clean up on the next start.
func browserSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
