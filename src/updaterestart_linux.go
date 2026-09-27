//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// restartSystemdServers restarts every systemd service whose main process is
// this binary (exe, symlinks resolved), so a supervised server picks up a
// freshly swapped binary. With staleOnly it touches only servers still running
// a replaced inode, which is how an up-to-date `lasso update` finishes a
// previous update that never restarted anything.
//
// A unit is restarted only when the lasso process IS its MainPID. A lasso
// started by a wrapper (a supervise.sh-style workspace unit whose MainPID is a
// shell) is left alone: restarting that unit would bounce everything else it
// runs. It reports how many units it found, whether or not each restart worked.
func restartSystemdServers(exe string, staleOnly bool) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	selfUnit, selfInUnit := unitOfPid("self")
	seen := map[systemdUnit]bool{}
	found := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue // gone, or another user's process
		}
		path, deleted := strings.CutSuffix(target, " (deleted)")
		if path != exe || (staleOnly && !deleted) {
			continue
		}
		u, ok := unitOfPid(strconv.Itoa(pid))
		if !ok || seen[u] {
			continue
		}
		seen[u] = true
		if u.User && u.UID != os.Getuid() {
			// Another user's manager: `systemctl --user` here would reach ours.
			fmt.Printf("lasso (pid %d) runs under uid %d's %s; restart it as that user: %s\n", pid, u.UID, u.Name, u.restartCommand())
			found++
			continue
		}
		if mainPID(u) != pid {
			continue
		}
		found++
		restartUnit(u, selfInUnit && selfUnit == u)
	}
	return found
}

// unitOfPid reads the systemd service of a /proc entry ("self" or a pid).
func unitOfPid(pid string) (systemdUnit, bool) {
	b, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return systemdUnit{}, false
	}
	return unitFromCgroup(string(b))
}

// mainPID asks systemd for u's MainPID; 0 when it can't say.
func mainPID(u systemdUnit) int {
	out, err := exec.Command("systemctl", u.systemctlArgs("show", "-p", "MainPID", "--value")...).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// restartUnit restarts u: a user unit through the caller's own manager, a
// system unit directly as root or else via non-interactive sudo. On failure it
// prints the command to run by hand rather than prompting. inside means this
// process lives in u itself (a shell in lasso's own Terminal tab), where the
// restart kills us, so the job is queued with --no-block and announced first.
func restartUnit(u systemdUnit, inside bool) {
	verb := []string{"restart"}
	if inside {
		verb = append(verb, "--no-block")
	}
	args := u.systemctlArgs(verb...)
	name, argv := "systemctl", args
	if !u.User && os.Geteuid() != 0 {
		name, argv = "sudo", append([]string{"-n", "systemctl"}, args...)
	}
	if inside {
		fmt.Printf("restarting %s (this shell runs inside it and will be cut off) …\n", u.Name)
	} else {
		fmt.Printf("restarting %s …\n", u.Name)
	}
	if out, err := exec.Command(name, argv...).CombinedOutput(); err != nil {
		fmt.Printf("could not restart %s: %v\n%s", u.Name, err, out)
		fmt.Printf("run: %s\n", u.restartCommand())
		return
	}
	fmt.Printf("%s restarted\n", u.Name)
}
