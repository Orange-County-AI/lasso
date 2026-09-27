package main

import "testing"

func TestUnitFromCgroup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want systemdUnit
		ok   bool
	}{
		{"user unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/lasso.service\n",
			systemdUnit{Name: "lasso.service", User: true, UID: 1000}, true},
		{"system unit", "0::/system.slice/ws-lasso.service\n",
			systemdUnit{Name: "ws-lasso.service"}, true},
		{"templated system unit, delegated subtree", "0::/system.slice/system-workspace.slice/workspace@acme.service/payload\n",
			systemdUnit{Name: "workspace@acme.service"}, true},
		{"user manager itself", "0::/user.slice/user-1000.slice/user@1000.service/init.scope\n",
			systemdUnit{Name: "user@1000.service"}, true},
		{"session scope", "0::/user.slice/user-1000.slice/session-4.scope\n", systemdUnit{}, false},
		{"hybrid v1", "12:pids:/system.slice/lasso.service\n1:name=systemd:/system.slice/lasso.service\n0::/\n",
			systemdUnit{Name: "lasso.service"}, true},
		{"v1 only", "12:pids:/x\n1:name=systemd:/user.slice/user-1001.slice/user@1001.service/lasso.service\n",
			systemdUnit{Name: "lasso.service", User: true, UID: 1001}, true},
		{"empty", "", systemdUnit{}, false},
	}
	for _, c := range cases {
		got, ok := unitFromCgroup(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestSystemdUnitCommands(t *testing.T) {
	u := systemdUnit{Name: "lasso.service", User: true, UID: 1000}
	if got := u.restartCommand(); got != "systemctl --user restart lasso.service" {
		t.Errorf("user restart: %q", got)
	}
	s := systemdUnit{Name: "ws-lasso.service"}
	if got := s.restartCommand(); got != "sudo systemctl restart ws-lasso.service" {
		t.Errorf("system restart: %q", got)
	}
	if got := s.systemctlArgs("show", "-p", "MainPID", "--value"); len(got) != 5 || got[0] != "show" || got[4] != "ws-lasso.service" {
		t.Errorf("system args: %q", got)
	}
}
