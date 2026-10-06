package action

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(unit, "agentgw-action-open@"), "agentgw-action@"), ".service")
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
	exit, output, stdout, _, err := runCommand(context.Background(),
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
	exit, output, _, _, _ := runCommand(context.Background(), []string{"sleep", "30"},
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
	if exit != -1 || len(*stops) != 1 || !strings.HasPrefix((*stops)[0], "agentgw-action-open@") {
		t.Fatalf("exit=%d stops=%v, want -1 and the instance stopped", exit, *stops)
	}
}

func TestExecJobRejectsPathInFileName(t *testing.T) {
	jobFilesDir = t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(jobFilesDir, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../x", "/abs", "a/../../b", "linked/x"} {
		t.Run(name, func(t *testing.T) {
			run := t.TempDir()
			job, _ := json.Marshal(JobSpec{Argv: []string{"true"}, Files: map[string][]byte{name: []byte("bad")}})
			path := filepath.Join(run, "job.json")
			if err := os.WriteFile(path, job, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errb strings.Builder
			if code := execJob(run, path, &out, &errb); code != 125 {
				t.Fatalf("code=%d stderr=%q", code, errb.String())
			}
		})
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside HOME: %v", entries)
	}
}

func TestStopOrphansTargetsTemplateInstances(t *testing.T) {
	stops := fakeSystemd(t, t.TempDir())
	var reset []string
	resetFailed = func(units ...string) { reset = append(reset, units...) }
	if err := StopOrphans(context.Background()); err != nil || !reflect.DeepEqual(*stops, []string{"agentgw-action@*.service", "agentgw-action-open@*.service"}) {
		t.Fatalf("stops=%v err=%v", *stops, err)
	}
	if !reflect.DeepEqual(reset, *stops) {
		t.Fatalf("reset=%v stops=%v", reset, *stops)
	}
}

func TestTemplateEgressSelectionAndEnv(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "restricted"}[restricted], func(t *testing.T) {
			dir := t.TempDir()
			fakeSystemd(t, dir)
			original := startUnit
			var unit string
			var spec JobSpec
			startUnit = func(ctx context.Context, name string) error {
				unit = name
				id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(name, "agentgw-action-open@"), "agentgw-action@"), ".service")
				b, err := os.ReadFile(filepath.Join(dir, id, "job.json"))
				if err != nil {
					return err
				}
				if err := json.Unmarshal(b, &spec); err != nil {
					return err
				}
				return original(ctx, name)
			}
			opts := SandboxOptions{Mode: "systemd", Dir: dir}
			if restricted {
				opts.Egress = &EgressEnv{ProxyURL: "http://run-id:token-secret@127.77.0.1:3128"}
			}
			exit, _, err := RunCmd(context.Background(), []string{"true"}, opts, nil)
			if err != nil || exit != 0 {
				t.Fatalf("exit=%d err=%v", exit, err)
			}
			prefix := "agentgw-action-open@"
			if restricted {
				prefix = "agentgw-action@"
			}
			if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, ".service") {
				t.Fatalf("unit=%q", unit)
			}
			if len(spec.Argv) != 1 || spec.Argv[0] != "true" {
				t.Fatalf("argv=%q", spec.Argv)
			}
			if restricted {
				for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
					if spec.Env[name] != opts.Egress.ProxyURL {
						t.Fatalf("%s=%q", name, spec.Env[name])
					}
				}
				for _, name := range []string{"NO_PROXY", "no_proxy"} {
					if value, ok := spec.Env[name]; !ok || value != "" {
						t.Fatalf("%s=%q present=%t", name, value, ok)
					}
				}
			} else if len(spec.Env) != 0 {
				t.Fatalf("open env=%v", spec.Env)
			}
		})
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
	if code := execJob(filepath.Dir(job), job, &out, &errb); code != 125 {
		t.Fatalf("code=%d", code)
	}
}

func TestExecJobNestedFiles(t *testing.T) {
	jobFilesDir = t.TempDir()
	run := t.TempDir()
	job, _ := json.Marshal(JobSpec{Argv: []string{"true"}, Files: map[string][]byte{".codex/auth.json": []byte("token")}})
	path := filepath.Join(run, "job.json")
	if err := os.WriteFile(path, job, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := execJob(run, path, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	parent := filepath.Join(jobFilesDir, ".codex")
	info, err := os.Stat(parent)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode: %v %v", info, err)
	}
	data, err := os.ReadFile(filepath.Join(parent, "auth.json"))
	if err != nil || string(data) != "token" {
		t.Fatalf("file: %q %v", data, err)
	}
	info, err = os.Stat(filepath.Join(parent, "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func TestTemplateWriteback(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		cmd := "true"
		if changed {
			name = "changed"
			cmd = `printf new > "$HOME/.codex/auth.json"; exit 3`
		}
		t.Run(name, func(t *testing.T) {
			exit, _, _, wb, err := templateRun(context.Background(), dir, JobSpec{
				Argv: []string{"sh", "-c", cmd}, Files: map[string][]byte{".codex/auth.json": []byte("old")},
				Writeback: []string{".codex/auth.json"},
			}, outputCap, false)
			if err != nil || (changed && exit != 3) || (!changed && exit != 0) {
				t.Fatalf("exit=%d err=%v", exit, err)
			}
			if changed && string(wb[0]) != "new" || !changed && len(wb) != 0 {
				t.Fatalf("writeback=%v", wb)
			}
		})
	}
}

func TestTemplateWritebackCap(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	_, _, _, wb, err := templateRun(context.Background(), dir, JobSpec{
		Argv: []string{"sh", "-c", `head -c 1100000 /dev/zero > "$HOME/big"`}, Writeback: []string{"big"},
	}, outputCap, false)
	if err != nil || len(wb[0]) != outputCap {
		t.Fatalf("writeback length=%d err=%v", len(wb[0]), err)
	}
}

func TestTemplateRunIgnoresFIFOs(t *testing.T) {
	for _, name := range []string{"stdout", "wb-0"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			orig := startUnit
			startUnit = func(_ context.Context, unit string) error {
				id := strings.TrimSuffix(strings.TrimPrefix(unit, "agentgw-action@"), ".service")
				run := filepath.Join(dir, id)
				if err := syscall.Mkfifo(filepath.Join(run, name), 0o600); err != nil {
					return err
				}
				return nil
			}
			t.Cleanup(func() { startUnit = orig })
			started := time.Now()
			_, stdout, _, wb, err := templateRun(context.Background(), dir, JobSpec{Writeback: []string{"token"}}, outputCap, false)
			if err != nil || len(stdout) != 0 || len(wb) != 0 || time.Since(started) >= 2*time.Second {
				t.Fatalf("stdout=%q wb=%v err=%v elapsed=%v", stdout, wb, err, time.Since(started))
			}
		})
	}
}

func TestWritebackErrorKeepsExitAndOutput(t *testing.T) {
	dir := t.TempDir()
	fakeSystemd(t, dir)
	exit, stdout, stderr, wb, err := templateRun(context.Background(), dir, JobSpec{
		Argv:      []string{"sh", "-c", `ln -s /tmp "$HOME/linked"; printf good > "$HOME/changed"; printf answer`},
		Writeback: []string{"linked/token", "changed"},
	}, outputCap, false)
	if err != nil || exit != 0 || string(stdout) != "answer" || !strings.Contains(string(stderr), "writeback") || string(wb[1]) != "good" {
		t.Fatalf("exit=%d stdout=%q stderr=%q wb=%v err=%v", exit, stdout, stderr, wb, err)
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
