package main

import (
	"strconv"
	"strings"
)

// systemdUnit names the service a process runs under, read from its cgroup.
// User is set for a unit of a per-user manager (user@UID.service), which is
// driven with `systemctl --user` as that UID rather than the system manager.
type systemdUnit struct {
	Name string
	User bool
	UID  int
}

// unitFromCgroup extracts the innermost .service unit from the contents of
// /proc/<pid>/cgroup. It reads the unified (v2) "0::" line, falling back to the
// v1 "name=systemd" hierarchy on hybrid hosts. A process inside a delegated
// subtree (foo.service/payload) still resolves to foo.service, and anything
// that is not under a service (a scope, a bare slice) reports ok=false.
func unitFromCgroup(data string) (u systemdUnit, ok bool) {
	v2, v1 := "", ""
	for _, line := range strings.Split(data, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[0] == "0" && parts[1] == "":
			v2 = parts[2]
		case parts[1] == "name=systemd":
			v1 = parts[2]
		}
	}
	// A hybrid host can leave the unified line at "/" while the v1 systemd
	// hierarchy carries the real placement.
	path := v2
	if !strings.Contains(path, ".service") && v1 != "" {
		path = v1
	}
	if path == "" {
		return systemdUnit{}, false
	}
	uid := -1
	for _, c := range strings.Split(path, "/") {
		if !strings.HasSuffix(c, ".service") {
			continue
		}
		if n, found := strings.CutPrefix(strings.TrimSuffix(c, ".service"), "user@"); found {
			if id, err := strconv.Atoi(n); err == nil {
				// The user manager itself is a system unit; only what sits
				// below it belongs to the user's own manager.
				uid = id
				u = systemdUnit{Name: c}
				continue
			}
		}
		u = systemdUnit{Name: c, User: uid >= 0, UID: uid}
	}
	if !u.User {
		u.UID = 0
	}
	return u, u.Name != ""
}

// systemctlArgs is the systemctl argv (without the binary) for verb on u.
func (u systemdUnit) systemctlArgs(verb ...string) []string {
	args := []string{}
	if u.User {
		args = append(args, "--user")
	}
	args = append(args, verb...)
	return append(args, u.Name)
}

// restartCommand is the command a human would type to restart u by hand.
func (u systemdUnit) restartCommand() string {
	if u.User {
		return "systemctl --user restart " + u.Name
	}
	return "sudo systemctl restart " + u.Name
}
