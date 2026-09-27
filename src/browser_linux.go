//go:build linux

package main

import "syscall"

// browserSysProcAttr puts the shared Chromium in its own process group (so a
// stop can signal the renderer/GPU/zygote children with it, the way ttyd's
// shell goes down with ttyd) and asks the kernel to SIGTERM it the moment
// lasso dies. The second half is what keeps a `kill -9` of lasso, or a crash
// the shutdown path never sees, from leaving a headless browser burning cores
// against a profile the next lasso then has to fight for. It survives the exec
// systemd-run --scope does in place, so it holds for the capped launch too.
//
// The kernel ties Pdeathsig to the THREAD that forked, not the process, so it
// would also fire if the Go runtime retired that OS thread — which it only does
// when a goroutine exits while holding runtime.LockOSThread. Nothing in lasso
// does; this is the backstop, and the shutdown path's explicit stop is the rule.
func browserSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
