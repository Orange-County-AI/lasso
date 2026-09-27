package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Where a plugin's MCP server runs.
//
// By default in a microsandbox microVM: its own kernel, its own filesystem, the
// plugin directory mounted read-only, egress denied except to the hosts the
// operator approved, and secrets that never enter the guest at all (msb
// substitutes a placeholder on the wire, and only toward the hosts each secret
// is approved for). A plugin marked TRUSTED by the operator runs on the host
// instead, as lasso's user, with a minimal environment.
//
// The microVM half has a transport problem, measured on msb 0.7.3: `msb run`
// does not forward piped stdin, and `msb exec` holds all output until the
// command exits, so a stdio MCP server cannot be spoken to across the VM
// boundary directly. A published port streams fine. So lasso mounts ITS OWN
// binary (static, pure Go) into the guest and runs it as a tiny adapter —
// `lasso plugin-stdio-serve -listen :7700 -- <command>` — that accepts one TCP
// connection at a time and pipes it to a fresh child's stdin/stdout. lasso
// then dials the published loopback port and speaks MCP over the socket.
//
// Network shape, also measured: --no-net blocks INGRESS too (the published
// port resets), so the policy is --net-default-egress deny with ingress
// allowed, plus one allow rule per approved host (and DNS, only when there is
// any host to resolve). Network policy is create-time only in msb, which suits
// this: a changed allowlist is a changed fingerprint, which is a restart.

const (
	pluginGuestPort   = 7700
	pluginGuestLasso  = "/opt/lasso"
	pluginGuestDir    = "/plugin"
	pluginSandboxPref = "lasso-plugin-"
	// msbStopTimeout bounds each cleanup call (msb stop / msb rm). They take a
	// second or two; a wedged runtime must not wedge lasso's shutdown.
	msbStopTimeout = 20 * time.Second
)

// defaultPluginRunner is the manager's runner choice: the host for a plugin the
// operator trusts, a microVM for everything else.
func defaultPluginRunner(trusted bool) pluginRunner {
	if trusted {
		return hostPluginRunner{}
	}
	return msbPluginRunner{}
}

// ---------------------------------------------------------------------------
// finding msb
// ---------------------------------------------------------------------------

// resolveMSB finds the microsandbox CLI: LASSO_MSB (a path or PATH name, "off"
// disables sandboxed plugins), else msb on PATH, else the installer's default
// ~/.microsandbox/bin/msb — lasso often runs as a systemd unit whose PATH does
// not include the operator's shell additions.
func resolveMSB() (bin, reason string, ok bool) {
	e := strings.TrimSpace(os.Getenv("LASSO_MSB"))
	if strings.EqualFold(e, "off") {
		return "", "sandboxed plugins are disabled (LASSO_MSB=off)", false
	}
	if e != "" {
		if strings.ContainsRune(e, '/') {
			if st, err := os.Stat(e); err != nil || st.IsDir() {
				return "", fmt.Sprintf("microsandbox (msb) not found at LASSO_MSB=%q", e), false
			}
			return e, "", true
		}
		p, err := exec.LookPath(e)
		if err != nil {
			return "", fmt.Sprintf("microsandbox (msb) not found: LASSO_MSB=%q is not on PATH", e), false
		}
		return p, "", true
	}
	if p, err := exec.LookPath("msb"); err == nil {
		return p, "", true
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".microsandbox", "bin", "msb")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, "", true
		}
	}
	return "", "microsandbox (msb) not found: install it (https://microsandbox.dev) or set LASSO_MSB", false
}

func currentMSBStatus() msbStatus {
	p, reason, ok := resolveMSB()
	return msbStatus{Available: ok, Path: p, Reason: reason}
}

// ---------------------------------------------------------------------------
// the microVM runner
// ---------------------------------------------------------------------------

type msbPluginRunner struct{}

func (msbPluginRunner) sandboxed() bool { return true }

func pluginSandboxName(plugin string) string { return pluginSandboxPref + plugin }

// msbRunArgs is `msb run`'s argv for one plugin. Pure, so the security-relevant
// shape (egress policy, read-only mounts, which secrets go where) is testable
// without a hypervisor.
//
// Secrets appear here by NAME only (`--secret NAME@host,host`): msb reads the
// value from its own process environment, which is the one place lasso puts it.
// The plain env values do ride argv — they are non-secret by definition.
func msbRunArgs(name string, hostPort int, exe, dir string, spec *pluginMCPSpec) []string {
	args := []string{
		"run",
		"--name", name,
		"--no-tty",
		"--quiet",
		"--net-default-egress", "deny",
		"--net-default-ingress", "allow",
	}
	if len(spec.Network) > 0 {
		args = append(args, "--net-rule", "allow@dns")
		for _, n := range spec.Network {
			e, _ := parsePluginNet(n) // validated with the manifest
			args = append(args, "--net-rule", fmt.Sprintf("allow@%s:tcp:%d", e.Host, e.Port))
		}
	}
	args = append(args,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, pluginGuestPort),
		"--mount-file", exe+":"+pluginGuestLasso+":ro",
		"--mount-dir", dir+":"+pluginGuestDir+":ro",
		"-w", pluginGuestDir,
	)
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	for _, s := range spec.Secrets {
		args = append(args, "--secret", s.Name+"@"+strings.Join(s.Hosts, ","))
	}
	args = append(args, spec.Image, "--",
		pluginGuestLasso, "plugin-stdio-serve", "-listen", ":"+strconv.Itoa(pluginGuestPort), "--")
	return append(args, spec.Command...)
}

// freeLoopbackPort asks the kernel for a free port and gives it back. There is
// a window before msb binds it; losing that race is a failed launch, which the
// restart loop retries on a new port.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// msbQuiet runs one msb housekeeping command, ignoring its outcome. Stdin is
// left nil, i.e. /dev/null: msb's exec-style commands block forever on an
// inherited stdin that never reaches EOF.
func msbQuiet(msb string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), msbStopTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, msb, args...)
	cmd.Stdin = nil
	_ = cmd.Run()
}

// removeSandbox stops and removes a plugin's sandbox. Run before every launch
// (a previous lasso that died without cleaning up leaves one behind, and the
// name is fixed) and after every stop.
func removeSandbox(msb, name string) {
	msbQuiet(msb, "stop", name)
	msbQuiet(msb, "rm", name)
}

func (msbPluginRunner) start(ctx context.Context, l pluginLaunch, logw io.Writer) (pluginProc, error) {
	msb, reason, ok := resolveMSB()
	if !ok {
		return nil, unavailable("%s", reason)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, unavailable("cannot find lasso's own binary to mount into the sandbox: %v", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, err
	}
	name := pluginSandboxName(l.Name)
	removeSandbox(msb, name)

	cmd := exec.Command(msb, msbRunArgs(name, port, exe, l.Dir, l.MCP)...)
	// msb's own environment carries the secret VALUES (it reads --secret from
	// there); lasso's credentials are stripped as for Chromium. The guest sees
	// neither: only -e values and msb's placeholders cross into it.
	env := browserEnv(os.Environ())
	for k, v := range l.Secrets {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logw // the guest adapter's and the server's stderr come out here
	cmd.Stderr = logw
	// Own process group + Pdeathsig, like Chromium: the `msb run` process is
	// kept as a child (not -d) precisely so its death is observable, and a
	// kill -9 of lasso must not leave it running.
	cmd.SysProcAttr = browserSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start msb: %v", err)
	}
	p := &msbProc{cmd: cmd, msb: msb, name: name, port: port, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	return p, nil
}

type msbProc struct {
	cmd    *exec.Cmd
	msb    string
	name   string
	port   int
	exited chan struct{}
	once   sync.Once
}

func (p *msbProc) done() <-chan struct{} { return p.exited }

// logTail is the guest's recent output. `msb run` without a tty holds ALL of
// its output until the sandbox exits on its own (measured on 0.7.3), and a
// sandbox lasso stops never flushes it at all — so a plugin's stderr never
// streams into lasso's log. `msb logs` reads the captured output live; this is
// fetched when a launch fails or a running server dies, which is when a human
// needs the traceback.
func (p *msbProc) logTail() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.msb, "logs", "--tail", "20", p.name)
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// connect dials the published port until it answers, the sandbox exits, or ctx
// ends. Cold boot is a couple of seconds; a first boot also pulls the image.
func (p *msbProc) connect(ctx context.Context) (pluginStream, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
	for {
		var d net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		c, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			return pluginStream{r: c, w: c}, nil
		}
		select {
		case <-p.exited:
			return pluginStream{}, fmt.Errorf("the sandbox exited before its MCP server answered (%s)", p.cmd.ProcessState)
		case <-ctx.Done():
			return pluginStream{}, fmt.Errorf("the sandbox's MCP port never answered: %v", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (p *msbProc) stop() {
	p.once.Do(func() {
		stopProcessGroup(p.cmd, p.exited, 5*time.Second)
		removeSandbox(p.msb, p.name)
	})
}

// stopProcessGroup SIGTERMs a child's process group, escalating to SIGKILL
// after grace, and waits for the leader.
func stopProcessGroup(cmd *exec.Cmd, exited <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 1 { // never kill(-0)/kill(-1)
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		log.Printf("plugins:  pid %d did not exit after SIGKILL", pid)
	}
}

// ---------------------------------------------------------------------------
// the host runner (trusted plugins)
// ---------------------------------------------------------------------------

type hostPluginRunner struct{}

func (hostPluginRunner) sandboxed() bool { return false }

// start runs the command in the plugin directory as lasso's user. The
// environment is the same allowlist chrome-devtools-mcp gets — never lasso's
// UI_AUTH/MCP_OAUTH/LASSO_MCP_TOKEN, nor whatever else the operator's shell
// exported — plus the manifest's env and the resolved secrets as plain vars.
// Trusting a plugin means trusting its code, not handing it lasso's keys.
func (hostPluginRunner) start(ctx context.Context, l pluginLaunch, logw io.Writer) (pluginProc, error) {
	cmd := exec.Command(l.MCP.Command[0], l.MCP.Command[1:]...)
	cmd.Dir = l.Dir
	env := minimalChildEnv(os.Environ())
	for k, v := range l.MCP.Env {
		env = append(env, k+"="+v)
	}
	for k, v := range l.Secrets {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stderr = logw
	cmd.SysProcAttr = browserSysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %v", l.MCP.Command[0], err)
	}
	p := &hostProc{cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	return p, nil
}

type hostProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	exited chan struct{}

	mu   sync.Mutex
	used bool
	once sync.Once
}

func (p *hostProc) done() <-chan struct{} { return p.exited }

// connect hands out the child's stdio exactly once: a stdio server has one
// conversation, and a failed initialize on it means a failed launch.
func (p *hostProc) connect(context.Context) (pluginStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return pluginStream{}, errors.New("the MCP server did not complete its handshake over stdio")
	}
	p.used = true
	return pluginStream{r: p.stdout, w: p.stdin}, nil
}

func (p *hostProc) stop() {
	p.once.Do(func() {
		_ = p.stdin.Close() // a well-behaved stdio server exits on EOF
		select {
		case <-p.exited:
			return
		case <-time.After(2 * time.Second):
		}
		stopProcessGroup(p.cmd, p.exited, 2*time.Second)
	})
}

// ---------------------------------------------------------------------------
// lasso plugin-stdio-serve (runs INSIDE the microVM)
// ---------------------------------------------------------------------------

// cliPluginStdioServe is the hidden guest-side adapter: TCP in, stdio out. It
// serves one connection at a time — lasso is the only client — and each
// connection gets a fresh child, killed when the connection closes, so a
// half-open first dial (the port can accept before this listener exists and
// then reset) costs nothing but a respawn.
func cliPluginStdioServe(args []string) {
	fs := flag.NewFlagSet("plugin-stdio-serve", flag.ExitOnError)
	listen := fs.String("listen", ":"+strconv.Itoa(pluginGuestPort), "address to accept the MCP connection on")
	_ = fs.Parse(args)
	cmd := fs.Args()
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		fatal("plugin-stdio-serve: no command (usage: lasso plugin-stdio-serve -listen :7700 -- <cmd...>)")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal("plugin-stdio-serve: %v", err)
	}
	fmt.Fprintf(os.Stderr, "plugin-stdio-serve: listening on %s for %q\n", ln.Addr(), strings.Join(cmd, " "))
	if err := stdioServe(ln, cmd); err != nil {
		fatal("plugin-stdio-serve: %v", err)
	}
}

// stdioServe accepts connections on ln one at a time until it is closed.
func stdioServe(ln net.Listener, argv []string) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		serveStdioConn(conn, argv)
	}
}

// serveStdioConn pipes one connection to one child and returns when both are
// finished: the connection closing kills the child, the child exiting closes
// the connection.
func serveStdioConn(conn net.Conn, argv []string) {
	defer conn.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin-stdio-serve: %v\n", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin-stdio-serve: %v\n", err)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "plugin-stdio-serve: start %q: %v\n", argv[0], err)
		return
	}
	exited := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		// The child's stdout to the connection; its EOF (the child exited or
		// closed stdout) ends the conversation.
		_, _ = io.Copy(conn, stdout)
		close(outDone)
	}()
	go func() {
		// The connection to the child's stdin; the peer hanging up ends the
		// child, which would otherwise wait on a stdin nobody writes to.
		_, _ = io.Copy(stdin, conn)
		_ = stdin.Close()
		select {
		case <-exited:
		case <-time.After(time.Second):
			if pid := cmd.Process.Pid; pid > 1 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}()
	// Every read from the stdout pipe must finish before Wait, which closes it.
	<-outDone
	_ = cmd.Wait()
	close(exited)
}
