package action

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Sandboxed actions run in the Nix-defined template unit agentgw-action@<id>.service
// (see nix/module.nix). Its hardening is fixed in Nix, so agentgw can only ask
// systemd to start that unit: no transient units, no way to request User=root.
// Each run gets <Dir>/<id>/ with job.json (passed in as the unit's "job"
// credential) and stdout/stderr files that systemd writes for the unit.

// JobSpec is what agentgw hands the unit; exec-job runs it.
type JobSpec struct {
	Argv       []string          `json:"argv"`
	Stdin      []byte            `json:"stdin,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Files      map[string][]byte `json:"files,omitempty"` // written 0600 under jobFilesDir
	TimeoutSec int               `json:"timeout_sec"`
}

// jobFilesDir is where exec-job writes JobSpec.Files inside the unit (its
// PrivateTmp); argv refers to files by this path. Tests override it.
var jobFilesDir = "/tmp/agentgw"

// FilePath is the in-unit path of a JobSpec file.
func FilePath(name string) string { return filepath.Join(jobFilesDir, name) }

func templateUnit(id string) string { return "agentgw-action@" + id + ".service" }

// systemd calls go through these so tests never touch the host's systemd.
var (
	startUnit = func(ctx context.Context, unit string) error {
		return exec.CommandContext(ctx, "systemctl", "start", "--wait", "--", unit).Run()
	}
	unitExitStatus = func(unit string) (int, error) {
		out, err := exec.Command("systemctl", "show", "-p", "ExecMainStatus", "--value", "--", unit).Output()
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(string(out)))
	}
	resetFailed = func(unit string) { _ = exec.Command("systemctl", "reset-failed", "--", unit).Run() }
	stopUnits   = func(ctx context.Context, units ...string) error {
		return exec.CommandContext(ctx, "systemctl", append([]string{"stop", "--no-block", "--"}, units...)...).Run()
	}
)

// templateRun runs spec in a fresh agentgw-action@ instance and returns its
// exit code and (stdout, stderr). exit -1 means it never ran or was cancelled.
func templateRun(ctx context.Context, dir string, spec JobSpec) (int, []byte, []byte, error) {
	if dir == "" {
		return -1, nil, nil, errors.New("sandbox: no action directory configured")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return -1, nil, nil, err
	}
	id := hex.EncodeToString(b)
	runDir := filepath.Join(dir, id)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return -1, nil, nil, err
	}
	defer os.RemoveAll(runDir)
	job, err := json.Marshal(spec)
	if err != nil {
		return -1, nil, nil, err
	}
	// Pre-create everything 0600 as agentgw; PID 1 reads job.json and opens
	// the output files as root on the unit's behalf.
	for name, data := range map[string][]byte{"job.json": job, "stdout": nil, "stderr": nil} {
		if err := os.WriteFile(filepath.Join(runDir, name), data, 0o600); err != nil {
			return -1, nil, nil, err
		}
	}
	unit := templateUnit(id)
	runErr := startUnit(ctx, unit)
	if ctx.Err() != nil {
		// Cancelled or timed out: make sure the unit doesn't outlive the job.
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = stopUnits(sctx, unit)
		cancel()
	}
	stdout, _ := os.ReadFile(filepath.Join(runDir, "stdout"))
	stderr, _ := os.ReadFile(filepath.Join(runDir, "stderr"))
	switch {
	case ctx.Err() != nil:
		return -1, stdout, stderr, nil
	case runErr == nil:
		return 0, stdout, stderr, nil
	}
	// The unit failed: it stays loaded as "failed", so its exit status is readable.
	exit, err := unitExitStatus(unit)
	resetFailed(unit)
	if err != nil || exit == 0 {
		exit = 1 // failed to start, or killed by a signal (e.g. RuntimeMaxSec)
	}
	return exit, stdout, stderr, nil
}

// StopOrphans stops agentgw-action@ instances left running by a crashed
// agentgw (they live outside its cgroup). Call at startup before requeueing.
func StopOrphans(ctx context.Context) error {
	return stopUnits(ctx, "agentgw-action@*.service")
}

// ExecJob is the `agentgw exec-job` entry point inside the template unit: it
// reads the job credential, writes its files, runs argv, and returns the exit
// code. stdout/stderr are the unit's (systemd points them at the run files).
func ExecJob(credFile string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(credFile)
	if err != nil {
		fmt.Fprintln(stderr, "exec-job:", err)
		return 125
	}
	var spec JobSpec
	if err := json.Unmarshal(raw, &spec); err != nil || len(spec.Argv) == 0 {
		fmt.Fprintln(stderr, "exec-job: bad job spec")
		return 125
	}
	if err := os.MkdirAll(jobFilesDir, 0o700); err != nil {
		fmt.Fprintln(stderr, "exec-job:", err)
		return 125
	}
	for name, data := range spec.Files {
		if strings.ContainsAny(name, "/\\") || name == "" || name == "." || name == ".." {
			fmt.Fprintln(stderr, "exec-job: bad file name")
			return 125
		}
		if err := os.WriteFile(FilePath(name), data, 0o600); err != nil {
			fmt.Fprintln(stderr, "exec-job:", err)
			return 125
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if spec.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutSec)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = []string{"HOME=" + jobFilesDir, "LANG=C.UTF-8"}
	if p, ok := os.LookupEnv("PATH"); ok {
		cmd.Env = append(cmd.Env, "PATH="+p)
	}
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode()
	case ctx.Err() != nil:
		fmt.Fprintln(stderr, "exec-job: timed out or stopped")
		return 124
	default:
		fmt.Fprintln(stderr, "exec-job:", err)
		return 127
	}
}
