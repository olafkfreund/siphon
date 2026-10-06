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
	origStart, origStatus, origReset, origStop := startUnit, unitExitStatus, resetFailed, stopUnits
	startUnit = func(ctx context.Context, unit string) error {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, "agentgw-action@"), ".service")
		run := filepath.Join(dir, id)
		out, _ := os.OpenFile(filepath.Join(run, "stdout"), os.O_WRONLY, 0)
		errf, _ := os.OpenFile(filepath.Join(run, "stderr"), os.O_WRONLY, 0)
		defer out.Close()
		defer errf.Close()
		last = ExecJob(filepath.Join(run, "job.json"), out, errf)
		if last != 0 {
			return os.ErrInvalid // systemctl start --wait fails when the unit fails
		}
		return nil
	}
	unitExitStatus = func(string) (int, error) { return last, nil }
	resetFailed = func(string) {}
	stopUnits = func(_ context.Context, units ...string) error { stops = append(stops, units...); return nil }
	t.Cleanup(func() { startUnit, unitExitStatus, resetFailed, stopUnits = origStart, origStatus, origReset, origStop })
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
	if code := ExecJob(job, &out, &errb); code != 125 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestStopOrphansTargetsTemplateInstances(t *testing.T) {
	stops := fakeSystemd(t, t.TempDir())
	if err := StopOrphans(context.Background()); err != nil || len(*stops) != 1 || (*stops)[0] != "agentgw-action@*.service" {
		t.Fatalf("stops=%v err=%v", *stops, err)
	}
}
