// Command acceptance verifies synthetic candidate delivery and protects private acceptance evidence.
// It is developer/owner tooling, never a mailbox or credential consumer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var errInvalid = errors.New("acceptance: invalid or unsafe operation")

func flags(name string, args []string) (*flag.FlagSet, func() error) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f, func() error {
		if f.Parse(args) != nil || f.NArg() != 0 {
			return errInvalid
		}
		return nil
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	if e := dispatch(ctx, os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errInvalid
	}
	verb, args := args[0], args[1:]
	switch verb {
	case "serve":
		f, parse := flags(verb, args)
		a := f.String("archive", "", "")
		c := f.String("candidate", "", "")
		s := f.String("sha256", "", "")
		r := f.String("ready", "", "")
		if parse() != nil {
			return errInvalid
		}
		return serveArchive(ctx, *a, *c, *s, *r)
	case "wait-ready":
		f, parse := flags(verb, args)
		r := f.String("ready", "", "")
		c := f.String("candidate", "", "")
		s := f.String("sha256", "", "")
		pid := f.Int("pid", 0, "")
		out := f.String("download", "", "")
		dur := f.Duration("timeout", 10*time.Second, "")
		if parse() != nil || *dur <= 0 || *dur > 10*time.Second {
			return errInvalid
		}
		v, e := waitReady(ctx, *r, *c, *s, *pid, *out, *dur)
		if e != nil {
			return e
		}
		fmt.Println(v.Port)
		return nil
	case "formula":
		f, parse := flags(verb, args)
		c := f.String("candidate", "", "")
		s := f.String("sha256", "", "")
		port := f.Int("port", 0, "")
		out := f.String("out", "", "")
		if parse() != nil {
			return errInvalid
		}
		return writeFormula(*out, *c, *s, *port)
	case "installed-smoke":
		f, parse := flags(verb, args)
		b := f.String("binary", "", "")
		r := f.String("root", "", "")
		e := f.String("examples", "", "")
		if parse() != nil {
			return errInvalid
		}
		return installedSmoke(ctx, *b, *r, *e)
	case "receipt-init", "receipt-validate", "summary-proposal":
		f, parse := flags(verb, args)
		r := f.String("private-root", "", "")
		c := f.String("candidate", "", "")
		approved := f.Bool("owner-reviewed", false, "")
		if parse() != nil {
			return errInvalid
		}
		return receiptCommand(verb, *r, *c, *approved)
	case "run":
		return runPhase(ctx, args)
	case "trust-check":
		f, parse := flags(verb, args)
		b := f.String("brew", "", "")
		n := f.String("formula", "", "")
		if parse() != nil {
			return errInvalid
		}
		return checkTrust(ctx, *b, *n)
	case "owned-root", "remove-owned":
		f, parse := flags(verb, args)
		r := f.String("root", "", "")
		id := f.String("identity", "", "")
		if parse() != nil {
			return errInvalid
		}
		return ownedRoot(verb, *r, *id)
	case "brew-version":
		f, parse := flags(verb, args)
		b := f.String("brew", "", "")
		if parse() != nil {
			return errInvalid
		}
		return brewVersion(ctx, *b)
	case "brew-layout":
		f, parse := flags(verb, args)
		p := f.String("prefix", "", "")
		c := f.String("cellar", "", "")
		r := f.String("repository", "", "")
		if parse() != nil {
			return errInvalid
		}
		return brewLayout(*p, *c, *r)
	case "tap-location":
		f, parse := flags(verb, args)
		p := f.String("path", "", "")
		r := f.String("repository", "", "")
		if parse() != nil {
			return errInvalid
		}
		return tapLocation(*p, *r)
	case "installed-location":
		f, parse := flags(verb, args)
		p := f.String("prefix", "", "")
		c := f.String("cellar", "", "")
		if parse() != nil {
			return errInvalid
		}
		return installedLocation(*p, *c)

	}
	return errInvalid
}
func cleanEnv(extra map[string]string) []string {
	// Deliberately construct, never filter an unbounded inherited credential environment.
	env := []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "GOTOOLCHAIN=local"}
	for _, k := range []string{"TMPDIR", "SDKROOT", "DEVELOPER_DIR", "USER", "LOGNAME", "GOCACHE", "GOMODCACHE"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
func runPhase(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("run", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	duration := f.Duration("timeout", time.Minute, "")
	budget := f.Duration("budget", 0, "")
	phase := f.String("phase", "", "")
	observer := f.String("observer", "", "")
	if f.Parse(args) != nil || f.NArg() == 0 || *duration <= 0 || *duration > 18*time.Minute {
		return errInvalid
	}
	allowed := map[string]bool{"prerequisites": true, "exact-candidate-make-check": true, "archive-readiness": true, "install": true, "formula-test": true, "installed-smoke": true, "cleanup": true}
	if !allowed[*phase] {
		return errInvalid
	}
	started := time.Now()
	if *budget == 0 {
		*budget = *duration + 5*time.Second
	}
	if *budget <= 0 || *budget > 19*time.Minute {
		return errInvalid
	}
	deadline := started.Add(*budget)
	reserve := 5 * time.Second
	if reserve > *budget/2 {
		reserve = *budget / 2
	}
	commandDuration := *duration
	if commandDuration > *budget-reserve {
		commandDuration = *budget - reserve
	}
	fmt.Printf("[candidate-formula] phase=%s start=%s\n", *phase, started.UTC().Format(time.RFC3339))
	cmd := exec.Command(f.Args()[0], f.Args()[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if cmd.Start() != nil {
		return errors.New("acceptance: phase start failed")
	}
	if *observer != "" {
		raw := []byte(strconv.Itoa(cmd.Process.Pid))
		if exclusiveWrite(*observer, raw) != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return errInvalid
		}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(max(time.Nanosecond, time.Until(started.Add(commandDuration))))
	defer timer.Stop()
	var e error
	select {
	case e = <-done: // Descendants are still owned by this invocation's process group.
		if !stopGroup(cmd.Process.Pid, nil, deadline) {
			e = errors.New("owned phase descendants remain")
		}
	case <-ctx.Done():
		e = ctx.Err()
		if !stopGroup(cmd.Process.Pid, done, deadline) {
			e = errors.New("owned phase cleanup failed")
		}
	case <-timer.C:
		e = errors.New("phase deadline")
		if !stopGroup(cmd.Process.Pid, done, deadline) {
			e = errors.New("owned phase cleanup failed")
		}
	}
	status := "pass"
	if e != nil {
		status = "fail"
	}
	elapsed := time.Since(started).Seconds()
	fmt.Printf("[candidate-formula] phase=%s elapsed_seconds=%.3f status=%s\n", *phase, elapsed, status)
	if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
		file, x := os.OpenFile(summary, os.O_APPEND|os.O_WRONLY, 0600)
		if x != nil {
			return errors.New("acceptance: timing report failed")
		}
		_, x = fmt.Fprintf(file, "| %s | %.3f | %s |\n", *phase, elapsed, status)
		x = errors.Join(x, file.Close())
		if x != nil {
			return errors.New("acceptance: timing report failed")
		}
	}
	if e != nil {
		return errors.New("acceptance: phase failed")
	}
	return nil
}
func stopGroup(pid int, done <-chan error, outerDeadline time.Time) bool {
	start := time.Now()
	deadline := start.Add(5 * time.Second)
	if outerDeadline.Before(deadline) {
		deadline = outerDeadline
	}
	grace := 3 * time.Second
	if remaining := time.Until(deadline); grace > remaining*3/5 {
		grace = remaining * 3 / 5
	}
	reaped := done == nil
	killed := false
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	for time.Now().Before(deadline) {
		if !reaped {
			select {
			case <-done:
				reaped = true
			default:
			}
		}
		if reaped && syscall.Kill(-pid, 0) != nil {
			return true
		}
		if !killed && time.Since(start) >= grace {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			killed = true
		}
		time.Sleep(time.Millisecond)
	}
	if !killed {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return false
}

func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func number(s string) (int, error) {
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 {
		return 0, errInvalid
	}
	return n, nil
}
func safeFormulaName(s string) bool {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] != "candidate" || parts[2] != "mailpipe" || !strings.HasPrefix(parts[1], "run") {
		return false
	}
	for _, c := range parts[1][3:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(parts[1]) > 3
}
