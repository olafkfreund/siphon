package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSystemd stands in for systemctl: "starting" agentgw-action@<id> runs
// ExecJob in-process with the run dir's job.json and output files, the way
// the NixOS template unit would. It records the last exit for `show`.
func fakeSystemd(t *testing.T, dir string) (stopped *[]string) {
	t.Helper()
	jobFilesDir = t.TempDir()
	var last int
	var stops []string
	origStart, origExit, origReset, origStop := startUnit, unitExit, resetFailed, stopUnits
	startUnit = func(ctx context.Context, unit string) error {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "agentgw-action@"), ".service")
		last = ExecJob(filepath.Join(dir, id)) // what `agentgw exec-job <dir>/%i` does in the unit
		if last != 0 {
			return os.ErrInvalid // systemctl start --wait fails when the unit fails
		}
		return nil
	}
	unitExit = func(string) (string, int, error) { return "1", last, nil } // CLD_EXITED, as systemctl prints it
	resetFailed = func(...string) {}
	stopUnits = func(_ context.Context, units ...string) error { stops = append(stops, units...); return nil }
	t.Cleanup(func() { startUnit, unitExit, resetFailed, stopUnits = origStart, origExit, origReset, origStop })
	return &stops
}

func TestTemplateRunSuccessWithFilesStdinEnv(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	exit, output, stdout, err := runCommand(context.Background(),
		[]string{"sh", "-c", `cat "$1"; printf ' %s ' "$TOKEN"; cat; echo oops >&2`, "x", FilePath("cfg")},
		SandboxOptions{Mode: "systemd", Dir: dir, Timeout: 5 * time.Second,
			Env: map[string]string{"TOKEN": "s3cret"}, Files: map[string][]byte{"cfg": []byte("from-file")}},
		[]string{"s3cret"}, []byte("from-stdin"), true)
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v output=%q", exit, err, output)
	}
	if got := string(stdout); got != "from-file *** from-stdin" {
		t.Fatalf("stdout %q (secret must be masked, file and stdin delivered)", got)
	}
	if !strings.Contains(string(output), "oops") {
		t.Fatalf("stderr missing from output %q", output)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("run dir not cleaned up: %v", entries)
	}
}

func TestTemplateRunFailureExitCode(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	exit, _, err := RunCmd(context.Background(), []string{"sh", "-c", "exit 3"},
		SandboxOptions{Mode: "systemd", Dir: dir, Timeout: 5 * time.Second}, nil)
	if err != nil || exit != 3 {
		t.Fatalf("exit=%d err=%v, want 3 read back from the failed unit", exit, err)
	}
}

func TestTemplateRunTimeoutInsideUnit(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	exit, output, _, _ := runCommand(context.Background(), []string{"sleep", "30"},
		SandboxOptions{Mode: "systemd", Dir: dir, Timeout: time.Second}, nil, nil, false)
	if exit != 124 || !strings.Contains(string(output), "timed out") {
		t.Fatalf("exit=%d output=%q, want 124 from exec-job's timeout", exit, output)
	}
}

func TestTemplateRunCancelStopsUnit(t *testing.T) {
	dir := t.TempDir()
	stops := fakeSystemd(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	startUnit = func(ctx context.Context, unit string) error { cancel(); <-ctx.Done(); return ctx.Err() }
	exit, _, _ := RunCmd(ctx, []string{"true"}, SandboxOptions{Mode: "systemd", Dir: dir, Timeout: time.Minute}, nil)
	if exit != -1 || len(*stops) != 1 || !strings.HasPrefix((*stops)[0], "agentgw-action@") {
		t.Fatalf("exit=%d stops=%v, want -1 and the instance stopped", exit, *stops)
	}
}

func TestExecJobRejectsPathInFileName(t *testing.T) {
	jobFilesDir = t.TempDir()
	job := filepath.Join(t.TempDir(), "job.json")
	os.WriteFile(job, []byte(`{"argv":["true"],"files":{"../evil":"eA=="}}`), 0o600)
	var out, errb strings.Builder
	if code := execJob(job, &out, &errb); code != 125 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestStopOrphansTargetsTemplateInstances(t *testing.T) {
	stops := fakeSystemd(t, t.TempDir())
	if err := StopOrphans(context.Background()); err != nil || len(*stops) != 1 || (*stops)[0] != "agentgw-action@*.service" {
		t.Fatalf("stops=%v err=%v", *stops, err)
	}
}

// Review C1: exec-job creates its output files itself and never follows a
// planted symlink (PID 1 no longer opens anything in the run dir).
func TestExecJobRefusesPlantedSymlink(t *testing.T) {
	jobFilesDir = t.TempDir()
	run := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(target, []byte("original"), 0o600)
	os.WriteFile(filepath.Join(run, "job.json"), []byte(`{"argv":["echo","pwned"]}`), 0o600)
	if err := os.Symlink(target, filepath.Join(run, "stdout")); err != nil {
		t.Fatal(err)
	}
	if code := ExecJob(run); code != 125 {
		t.Fatalf("code=%d, want 125 (refused)", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "original" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

func TestExecJobRejectsBadEnv(t *testing.T) {
	jobFilesDir = t.TempDir()
	job := filepath.Join(t.TempDir(), "job.json")
	os.WriteFile(job, []byte(`{"argv":["true"],"env":{"A=B":"x"}}`), 0o600)
	var out, errb strings.Builder
	if code := execJob(job, &out, &errb); code != 125 {
		t.Fatalf("code=%d", code)
	}
}

func TestTemplateRunSignalExit(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	startUnit = func(context.Context, string) error { return os.ErrInvalid }
	unitExit = func(string) (string, int, error) { return "2", 9, nil } // CLD_KILLED
	exit, _, _ := RunCmd(context.Background(), []string{"true"}, SandboxOptions{Mode: "systemd", Dir: dir, Timeout: time.Second}, nil)
	if exit != 137 {
		t.Fatalf("exit=%d, want 128+9", exit)
	}
}
