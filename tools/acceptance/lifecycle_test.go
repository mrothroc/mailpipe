package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var shellScript = "candidate-formula.sh"
var buildOnce sync.Once
var builtTool string
var buildErr error

func tool(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		d, e := os.MkdirTemp("", "acceptance-test-tool-")
		if e != nil {
			buildErr = e
			return
		}
		builtTool = filepath.Join(d, "acceptance")
		c := exec.Command("go", "build", "-o", builtTool, ".")
		b, e := c.CombinedOutput()
		if e != nil {
			buildErr = &buildFailure{string(b)}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtTool
}

type buildFailure struct{ text string }

func (e *buildFailure) Error() string { return e.text }
func TestMain(m *testing.M) {
	code := m.Run()
	if builtTool != "" {
		os.RemoveAll(filepath.Dir(builtTool))
	}
	os.Exit(code)
}
func runTool(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	return exec.Command(tool(t), args...).CombinedOutput()
}
func TestCandidateFormulaLocalRefusal(t *testing.T) {
	c := exec.Command("/bin/bash", shellScript, "--ci", "--candidate", strings.Repeat("a", 40))
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temp(t)}
	b, e := c.CombinedOutput()
	if e == nil || !bytes.Contains(b, []byte("CI-only")) {
		t.Fatalf("ordinary local install did not refuse safely: %v %s", e, b)
	}
}

type observation struct {
	Root      string `json:"root"`
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	PhasePID  int    `json:"phase_pid"`
	Event     string `json:"event"`
	Candidate string `json:"candidate"`
}
type shellRun struct {
	cmd      *exec.Cmd
	output   bytes.Buffer
	done     chan error
	observer string
	finished bool
}

func startShell(t *testing.T, mode string) *shellRun {
	t.Helper()
	parent := temp(t)
	r := &shellRun{done: make(chan error, 1), observer: filepath.Join(parent, "observer")}
	os.WriteFile(filepath.Join(parent, "sibling"), []byte("untouched"), 0600)
	r.cmd = exec.Command("/bin/bash", shellScript, "--lifecycle-test", mode, "--helper", tool(t), "--test-parent", parent, "--observer", r.observer)
	r.cmd.WaitDelay = 2 * time.Second
	r.cmd.Stdout = &r.output
	r.cmd.Stderr = &r.output
	if e := r.cmd.Start(); e != nil {
		t.Fatal(e)
	}
	go func() { r.done <- r.cmd.Wait() }()
	t.Cleanup(func() {
		if !r.finished {
			_ = r.cmd.Process.Signal(syscall.SIGTERM)
		}
		// Observations are produced only by this just-launched wrapper in its owned fixture directory.
		raw, _ := os.ReadFile(r.observer)
		roots := map[string]bool{}
		pids := map[int]bool{}
		groups := map[int]bool{}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			var o observation
			if json.Unmarshal(line, &o) != nil {
				continue
			}
			if filepath.Dir(o.Root) != parent || !strings.HasPrefix(filepath.Base(o.Root), "mailpipe-candidate.") {
				t.Error("invalid fixture observation")
				continue
			}
			roots[o.Root] = true
			if o.Event == "bootstrap" && o.PhasePID > 0 {
				groups[o.PhasePID] = true
			}
			if o.PID > 0 {
				pids[o.PID] = true
			}
			if o.PhasePID > 0 {
				pids[o.PhasePID] = true
			}
		}
		for root := range roots {
			for _, name := range []string{"phase-child", "bootstrap-child", "fixture-child"} {
				if raw, e := os.ReadFile(filepath.Join(root, name)); e == nil {
					pid, e := strconv.Atoi(string(raw))
					if e == nil && pid > 0 {
						pids[pid] = true
						if group, e := syscall.Getpgid(pid); e == nil && group == pid {
							groups[group] = true
						}
					}
				}
			}
		}
		for pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		alive := func() bool {
			for pid := range pids {
				if syscall.Kill(pid, 0) == nil {
					return true
				}
			}
			return false
		}
		deadline := time.Now().Add(5 * time.Second)
		for alive() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		for group := range groups {
			if syscall.Kill(-group, 0) == nil {
				_ = syscall.Kill(-group, syscall.SIGKILL)
			}
		}
		for pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		deadline = time.Now().Add(time.Second)
		for alive() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if alive() {
			t.Error("recorded fixture process cleanup uncertain")
		}
		if !r.finished {
			select {
			case <-r.done:
				r.finished = true
			case <-time.After(8 * time.Second):
				r.cmd.Process.Kill()
				t.Error("wrapper cleanup deadline")
			}
		}
		for root := range roots {
			_ = os.RemoveAll(root)
		}
	})
	return r
}
func (r *shellRun) observe(t *testing.T, event string) observation {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(r.observer)
		for _, line := range bytes.Split(b, []byte("\n")) {
			var o observation
			if json.Unmarshal(line, &o) == nil && o.Event == event {
				return o
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing %s observation", event)
	return observation{}
}
func (r *shellRun) finish(t *testing.T, wantOK bool) {
	t.Helper()
	select {
	case e := <-r.done:
		r.finished = true
		if b, e := os.ReadFile(filepath.Join(filepath.Dir(r.observer), "sibling")); e != nil || string(b) != "untouched" {
			t.Error("sibling path damaged")
		}
		if (e == nil) != wantOK {
			t.Fatalf("wrapper exit: %v %s", e, r.output.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatal("wrapper failed to reap within deadline")
	}
}
func assertGone(t *testing.T, o observation) {
	defer func() {
		if strings.HasPrefix(filepath.Base(o.Root), "mailpipe-candidate.") {
			_ = os.RemoveAll(o.Root)
		}
	}()
	t.Helper()
	if _, e := os.Stat(o.Root); !os.IsNotExist(e) {
		t.Error("owned root remains")
	}
	if o.Port > 0 {
		c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(o.Port)), 100*time.Millisecond)
		if e == nil {
			c.Close()
			t.Error("owned listener remains")
		}
	}
	if o.PhasePID > 0 && syscall.Kill(o.PhasePID, 0) == nil {
		t.Error("owned phase supervisor remains")
		syscall.Kill(o.PhasePID, syscall.SIGTERM)
	}
	if o.PID > 0 && syscall.Kill(o.PID, 0) == nil {
		t.Error("owned server remains")
		_ = syscall.Kill(o.PID, syscall.SIGTERM)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && syscall.Kill(o.PID, 0) == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
}
func itoa(n int) string { return strconv.Itoa(n) }

// Exercise the checked-in phase function under the same system shell as CI.
// Only fake phase children run; these modes never pass real Brew admission.
func TestCandidateFormulaOptionalObserver(t *testing.T) {
	for _, mode := range []string{"no-observer-success", "no-observer-failure", "no-observer-hold", "hold"} {
		t.Run(mode, func(t *testing.T) {
			r := startShell(t, mode)
			o := r.observe(t, "ready")
			if strings.HasSuffix(mode, "hold") {
				o = r.lastObservation(o)
				r.observe(t, "phase")
				marker := "fixture-child"
				if mode == "hold" {
					marker = "phase-child"
				}
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if b, e := os.ReadFile(filepath.Join(o.Root, marker)); e == nil && len(b) > 0 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				raw, e := os.ReadFile(filepath.Join(o.Root, marker))
				if e != nil || len(raw) == 0 {
					t.Fatal("phase child never started")
				}
				pid, e := strconv.Atoi(string(raw))
				if e != nil || pid <= 0 {
					t.Fatal("invalid fixture child identity")
				}
				t.Cleanup(func() {
					if syscall.Kill(pid, 0) == nil {
						t.Error("phase child remains")
						syscall.Kill(pid, syscall.SIGKILL)
					}
				})
				_, e = os.Stat(filepath.Join(o.Root, "phase-child"))
				if (mode == "hold") != (e == nil) {
					t.Fatal("optional observer selection failed")
				}
				r.cmd.Process.Signal(syscall.SIGTERM)
			}
			r.finish(t, mode == "no-observer-success")
			wantExit := 0
			if mode == "no-observer-failure" {
				wantExit = 1
			}
			if strings.HasSuffix(mode, "hold") {
				wantExit = 143
			}
			if r.cmd.ProcessState.ExitCode() != wantExit {
				t.Fatalf("lost original phase status: %v", r.cmd.ProcessState)
			}
			assertGone(t, r.lastObservation(o))
			output := r.output.String()
			if strings.Count(output, "cleanup_elapsed_seconds=") != 1 || strings.Contains(output, "unbound variable") {
				t.Fatalf("cleanup was not owned once: %s", output)
			}
			if !strings.Contains(output, "phase=install") {
				t.Fatalf("shared phase did not execute: %s", output)
			}
		})
	}
}

func TestCandidateFormulaServerLifecycle(t *testing.T) {
	t.Run("parallel-held-ports", func(t *testing.T) {
		a := startShell(t, "hold")
		b := startShell(t, "hold")
		oa := a.observe(t, "ready")
		ob := b.observe(t, "ready")
		if oa.Port == ob.Port {
			t.Fatal("shared live port")
		}
		a.cmd.Process.Signal(syscall.SIGTERM)
		a.finish(t, false)
		assertGone(t, oa)
		resp, e := http.Get("http://127.0.0.1:" + itoa(ob.Port) + "/mailpipe-" + ob.Candidate + ".tar.gz")
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal("other instance damaged")
		}
		b.cmd.Process.Signal(syscall.SIGTERM)
		b.finish(t, false)
		assertGone(t, ob)
	})
	for _, mode := range []string{"success", "install-failure", "phase-timeout", "readiness-timeout", "server-failure", "cleanup-failure", "repeated-cleanup", "archive-corruption", "cleanup-budget", "bootstrap-failure"} {
		t.Run(mode, func(t *testing.T) {
			r := startShell(t, mode)
			o := r.observe(t, "created")
			r.finish(t, mode == "success" || mode == "repeated-cleanup")
			last := r.lastObservation(o)
			assertGone(t, last)
		})
	}
	for _, mode := range []string{"startup-hold", "hold"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM} {
			t.Run(mode+sig.String(), func(t *testing.T) {
				r := startShell(t, mode)
				event := "ready"
				if mode == "startup-hold" {
					event = "started"
				}
				o := r.observe(t, event)
				if mode == "hold" {
					r.observe(t, "phase")
					deadline := time.Now().Add(3 * time.Second)
					for time.Now().Before(deadline) {
						if b, e := os.ReadFile(filepath.Join(o.Root, "phase-child")); e == nil && len(b) > 0 {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					if b, e := os.ReadFile(filepath.Join(o.Root, "phase-child")); e != nil || len(b) == 0 {
						t.Fatal("phase child never started")
					}
				}
				r.cmd.Process.Signal(sig)
				r.finish(t, false)
				last := r.lastObservation(o)
				assertGone(t, last)
			})
		}
	}
}

func temp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}

func (r *shellRun) lastObservation(fallback observation) observation {
	b, _ := os.ReadFile(r.observer)
	for _, line := range bytes.Split(b, []byte("\n")) {
		var o observation
		if json.Unmarshal(line, &o) == nil {
			if o.PID > 0 {
				fallback.PID = o.PID
			}
			if o.Port > 0 {
				fallback.Port = o.Port
			}
			if o.PhasePID > 0 {
				fallback.PhasePID = o.PhasePID
			}
		}
	}
	return fallback
}

func TestPhaseSupervisorOwnsOnlyItsGroup(t *testing.T) {
	sibling := exec.Command("/bin/sleep", "30")
	if e := sibling.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { sibling.Process.Kill(); sibling.Wait() })
	d := temp(t)
	pidfile := filepath.Join(d, "child")
	observer := filepath.Join(d, "phase")
	c := exec.Command(tool(t), "run", "--phase", "install", "--timeout", "100ms", "--observer", observer, "--", "/bin/bash", "-c", `trap '' TERM; /bin/sleep 30 & printf '%s' "$!" > "$1"; wait`, "fixture", pidfile)
	start := time.Now()
	b, e := c.CombinedOutput()
	if e == nil || time.Since(start) > 11*time.Second {
		t.Fatalf("timeout/teardown failed: %v %s", e, b)
	}
	if syscall.Kill(sibling.Process.Pid, 0) != nil {
		t.Fatal("unrelated sibling process damaged")
	}
	raw, e := os.ReadFile(pidfile)
	if e != nil {
		t.Fatal("descendant positive control missing")
	}
	pid, e := strconv.Atoi(string(raw))
	if e != nil {
		t.Fatal(e)
	}
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("owned descendant survived")
	}
	if !bytes.Contains(b, []byte("status=fail")) {
		t.Fatal("failure timing omitted")
	}
}
func TestProductionRejectsLifecycleOverridesBeforeBrew(t *testing.T) {
	d := temp(t)
	brew := filepath.Join(d, "brew")
	marker := filepath.Join(d, "brew-called")
	os.WriteFile(brew, []byte("#!/bin/sh\nprintf called > '"+marker+"'\nexit 91\n"), 0700)
	for _, option := range []string{"--lifecycle-test", "--helper", "--test-parent", "--observer"} {
		c := exec.Command("/bin/bash", shellScript, "--ci", "--candidate", strings.Repeat("a", 40), option, "fixture")
		c.Env = []string{"PATH=" + d + ":/usr/bin:/bin", "GITHUB_ACTIONS=true", "RUNNER_ENVIRONMENT=github-hosted", "RUNNER_OS=macOS", "GITHUB_JOB=candidate_formula", "MAILPIPE_CANDIDATE_RUNNER=macos-15", "EXPECTED_CANDIDATE=" + strings.Repeat("a", 40)}
		b, e := c.CombinedOutput()
		if e == nil || !bytes.Contains(b, []byte("CI-only")) {
			t.Fatalf("override not refused: %s", b)
		}
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("Brew invoked before admission")
	}
}

func TestTimingSummaryIsFixedAndFailureBlocking(t *testing.T) {
	d := temp(t)
	summary := filepath.Join(d, "summary")
	os.WriteFile(summary, nil, 0600)
	for _, command := range []string{"/usr/bin/true", "/usr/bin/false"} {
		c := exec.Command(tool(t), "run", "--phase", "install", "--timeout", "1s", "--", command)
		c.Env = append(os.Environ(), "GITHUB_STEP_SUMMARY="+summary)
		b, e := c.CombinedOutput()
		if (e == nil) != (command == "/usr/bin/true") {
			t.Fatalf("failure propagation %v %s", e, b)
		}
	}
	b, _ := os.ReadFile(summary)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatal("unbounded timing shape")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "| install | ") || len(line) > 128 || strings.Contains(line, "/usr/") {
			t.Fatal("timing includes arbitrary command data")
		}
	}
	c := exec.Command(tool(t), "run", "--phase", "token@example.org", "--timeout", "1s", "--", "/usr/bin/true")
	b, e := c.CombinedOutput()
	if e == nil || bytes.Contains(b, []byte("example.org")) {
		t.Fatal("invalid phase reflected")
	}
}

func TestBootstrapCancellationBeforeHelperReadiness(t *testing.T) {
	start := time.Now()
	r := startShell(t, "bootstrap-hold")
	o := r.observe(t, "bootstrap")
	deadline := time.Now().Add(3 * time.Second)
	var child int
	for time.Now().Before(deadline) {
		if b, e := os.ReadFile(filepath.Join(o.Root, "bootstrap-child")); e == nil {
			child, _ = strconv.Atoi(string(b))
			if child > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("bootstrap descendant positive control missing")
	}
	r.cmd.Process.Signal(syscall.SIGTERM)
	r.finish(t, false)
	assertGone(t, r.lastObservation(o))
	if syscall.Kill(child, 0) == nil {
		syscall.Kill(child, syscall.SIGKILL)
		t.Fatal("bootstrap descendant survived")
	}
	t.Logf("pre-helper bootstrap group and partial root removed after cancellation in %s", time.Since(start))
}
func TestSharedCleanupBudgetIncludesCommandJoin(t *testing.T) {
	start := time.Now()
	r := startShell(t, "cleanup-budget")
	o := r.observe(t, "created")
	r.finish(t, false)
	assertGone(t, r.lastObservation(o))
	if time.Since(start) > 11*time.Second {
		t.Fatal("shared cleanup deadline exceeded")
	}
	t.Logf("four cleanup command/join budgets plus owned-root removal completed in %s under the shared 8s test budget", time.Since(start))
	if !bytes.Contains(r.output.Bytes(), []byte("shared_budget_seconds=8")) {
		t.Fatal("missing shared budget evidence")
	}
}
