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
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Sandboxed actions run in a Nix-defined siphon-action template unit
// (see nix/module.nix). Its hardening is fixed in Nix, so siphon can only ask
// systemd to start that unit: no transient units, no way to request User=root.
//
// PID 1 never opens a path inside a directory siphon can write (that would let
// a compromised siphon redirect a root-opened file with a symlink). The unit
// runs `siphon exec-job <dir>/<id>` as its own DynamicUser, which reads
// job.json and creates stdout/stderr itself (O_EXCL|O_NOFOLLOW). The shared
// directory is setgid siphon-io so both sides can read what the other wrote.

// JobSpec is what siphon hands the unit; exec-job runs it.
type JobSpec struct {
	Argv       []string          `json:"argv"`
	Stdin      []byte            `json:"stdin,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Files      map[string][]byte `json:"files,omitempty"` // written 0600 under jobFilesDir
	Writeback  []string          `json:"writeback,omitempty"`
	TimeoutSec int               `json:"timeout_sec"`
	// EgressSocket: exec-job forwards forwardAddr to this unix socket.
	EgressSocket string `json:"egress_socket,omitempty"`
}

// forwardAddr is where exec-job listens inside the unit's private network
// namespace; the run's proxy URL points here. Tests override it.
var forwardAddr = "127.0.0.1:3128"

// forward copies each connection accepted on ln to the unix socket sock until ln is closed.
func forward(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			u, err := net.Dial("unix", sock)
			if err != nil {
				c.Close()
				return
			}
			go func() { io.Copy(u, c); u.Close(); c.Close() }()
			io.Copy(c, u)
			u.Close()
			c.Close()
		}()
	}
}

// jobFilesDir is where exec-job writes JobSpec.Files inside the unit (its
// PrivateTmp); argv refers to files by this path. Tests override it.
var jobFilesDir = "/tmp/siphon"

// outputCap bounds what siphon reads back; the unit's LimitFSIZE bounds the files.
const outputCap = 1 << 20

// FilePath is the in-unit path of a JobSpec file.
func FilePath(name string) string { return filepath.Join(jobFilesDir, name) }

func templateUnit(id string, restricted bool) string {
	if restricted {
		return "siphon-action@" + id + ".service"
	}
	return "siphon-action-open@" + id + ".service"
}

func systemctl(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "systemctl", append([]string{"--no-ask-password"}, args...)...)
}

// systemd calls go through these so tests never touch the host's systemd.
var (
	startUnit = func(ctx context.Context, unit string) error {
		return systemctl(ctx, "start", "--wait", "--", unit).Run()
	}
	// unitExit reads how a failed unit's main process ended.
	unitExit = func(unit string) (code string, status int, err error) {
		out, err := systemctl(context.Background(), "show", "-p", "ExecMainCode", "-p", "ExecMainStatus", "--", unit).Output()
		if err != nil {
			return "", 0, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			switch k {
			case "ExecMainCode":
				code = v
			case "ExecMainStatus":
				status, _ = strconv.Atoi(v)
			}
		}
		return code, status, nil
	}
	resetFailed = func(units ...string) {
		_ = systemctl(context.Background(), append([]string{"reset-failed", "--"}, units...)...).Run()
	}
	// stopUnits blocks until the units are gone (the template's TimeoutStopSec
	// bounds it), so nothing is requeued while an old copy is still running.
	stopUnits = func(ctx context.Context, units ...string) error {
		return systemctl(ctx, append([]string{"stop", "--"}, units...)...).Run()
	}
)

// templateRun runs spec in a fresh siphon-action instance and returns its
// exit code and (stdout, stderr). exit -1 means it never ran or was cancelled.
func templateRun(ctx context.Context, dir string, spec JobSpec, captureLimit int, restricted bool) (int, []byte, []byte, map[int][]byte, error) {
	if dir == "" {
		return -1, nil, nil, nil, errors.New("sandbox: no action directory configured")
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return -1, nil, nil, nil, err
	}
	id := hex.EncodeToString(b)
	runDir := filepath.Join(dir, id)
	if err := os.Mkdir(runDir, 0o700); err != nil { // Mkdir, not MkdirAll: an id is never shared
		return -1, nil, nil, nil, err
	}
	defer os.RemoveAll(runDir)
	// The run dir inherits group siphon-io from the setgid parent (created by
	// root via tmpfiles). Group may enter and create files but not list. No
	// setgid of our own: RestrictSUIDSGID forbids it, so files get the group
	// by chown instead (both sides are members of siphon-io).
	if err := os.Chmod(runDir, 0o730); err != nil {
		return -1, nil, nil, nil, err
	}
	gid, err := dirGid(runDir)
	if err != nil {
		return -1, nil, nil, nil, err
	}
	job, err := json.Marshal(spec)
	if err != nil {
		return -1, nil, nil, nil, err
	}
	jobPath := filepath.Join(runDir, "job.json")
	if err := os.WriteFile(jobPath, job, 0o600); err != nil {
		return -1, nil, nil, nil, err
	}
	if err := os.Chown(jobPath, -1, gid); err != nil {
		return -1, nil, nil, nil, fmt.Errorf("sandbox: is siphon in group siphon-io? %w", err)
	}
	if err := os.Chmod(jobPath, 0o640); err != nil {
		return -1, nil, nil, nil, err
	}
	unit := templateUnit(id, restricted)
	runErr := startUnit(ctx, unit)
	if ctx.Err() != nil {
		// Cancelled or timed out: make sure the unit doesn't outlive the job.
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = stopUnits(sctx, unit)
		cancel()
		resetFailed(unit)
	}
	stdout := readCapped(filepath.Join(runDir, "stdout"), captureLimit)
	stderr := readCapped(filepath.Join(runDir, "stderr"), captureLimit)
	writeback := readWritebacks(runDir, spec.Writeback)
	switch {
	case ctx.Err() != nil:
		return -1, stdout, stderr, writeback, nil
	case runErr == nil:
		return 0, stdout, stderr, writeback, nil
	}
	// The unit failed: it stays loaded as "failed" until reset, so this is race-free.
	code, status, err := unitExit(unit)
	resetFailed(unit)
	switch {
	case err != nil:
		return 1, stdout, stderr, writeback, nil
	// systemctl show prints ExecMainCode as the CLD_* number (1 exited, 2
	// killed, 3 dumped); accept the names too.
	case (code == "1" || code == "exited") && status != 0:
		return status, stdout, stderr, writeback, nil
	case code == "2" || code == "3" || code == "killed" || code == "dumped":
		return 128 + status, stdout, stderr, writeback, nil // status is the signal (RuntimeMaxSec, LimitFSIZE)
	default:
		return 1, stdout, stderr, writeback, nil // failed before the process ran
	}
}

func dirGid(dir string) (int, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("sandbox: cannot read directory group")
	}
	return int(st.Gid), nil
}

func readCapped(path string, limit int) []byte {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(f, int64(limit)))
	return b
}

func readWritebacks(runDir string, names []string) map[int][]byte {
	var writeback map[int][]byte
	for i := range names {
		if b := readCapped(filepath.Join(runDir, fmt.Sprintf("wb-%d", i)), outputCap); b != nil {
			if writeback == nil {
				writeback = make(map[int][]byte)
			}
			writeback[i] = b
		}
	}
	return writeback
}

func relativeFile(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("bad file name %q", name)
	}
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("bad file name %q", name)
		}
	}
	name = filepath.Clean(name)
	if name == "." {
		return "", fmt.Errorf("bad file name %q", name)
	}
	return name, nil
}

func filePath(home, name string, create bool) (string, error) {
	name, err := relativeFile(name)
	if err != nil {
		return "", err
	}
	parent := home
	for _, part := range strings.Split(filepath.Dir(name), string(filepath.Separator)) {
		if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlinked parent %q", parent)
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if part == "." {
			break
		}
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) && create {
			if err = os.Mkdir(parent, 0o700); err != nil {
				return "", err
			}
			info, err = os.Lstat(parent)
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("unsafe parent %q", parent)
		}
	}
	return filepath.Join(home, name), nil
}

func writeJobFiles(home string, files map[string][]byte) error {
	for name := range files {
		if _, err := relativeFile(name); err != nil {
			return err
		}
	}
	for name, data := range files {
		path, err := filePath(home, name, true)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		if err = f.Chmod(0o600); err == nil {
			_, err = f.Write(data)
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func saveWritebacks(home, runDir string, spec JobSpec, stderr io.Writer) {
	if len(spec.Writeback) == 0 {
		return
	}
	gid, err := dirGid(runDir)
	if err != nil {
		fmt.Fprintln(stderr, "exec-job: writeback:", err)
		return
	}
	for i, name := range spec.Writeback {
		if err := saveWriteback(home, runDir, gid, i, name, spec.Files); err != nil {
			fmt.Fprintln(stderr, "exec-job: writeback:", err)
		}
	}
}

func saveWriteback(home, runDir string, gid, i int, name string, files map[string][]byte) error {
	path, err := filePath(home, name, false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) || errors.Is(err, syscall.ELOOP) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return err
		}
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, outputCap+1))
	f.Close()
	if err != nil {
		return err
	}
	var old []byte
	var wrote bool
	for writtenName, writtenData := range files {
		clean, _ := relativeFile(writtenName)
		if clean == filepath.Clean(name) {
			old, wrote = writtenData, true
			break
		}
	}
	if wrote && bytes.Equal(data, old) {
		return nil
	}
	out, err := os.OpenFile(filepath.Join(runDir, fmt.Sprintf("wb-%d", i)), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return err
	}
	if err = out.Chown(-1, gid); err == nil {
		err = out.Chmod(0o640)
	}
	if err == nil {
		_, err = out.Write(data[:min(len(data), outputCap)])
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

// StopOrphans stops siphon-action instances left running by a crashed
// siphon (they live outside its cgroup) and waits for them, so their jobs
// can be requeued without two copies running at once.
func StopOrphans(ctx context.Context) error {
	err := stopUnits(ctx, "siphon-action@*.service", "siphon-action-open@*.service")
	resetFailed("siphon-action@*.service", "siphon-action-open@*.service")
	return err
}

// ExecJob is the `siphon exec-job <run dir>` entry point inside the template
// unit. It runs as the unit's DynamicUser: it reads job.json, creates its own
// output files, writes the job's private files, runs argv and returns the exit
// code (124 timeout/stopped, 125 bad job, 127 could not start).
func ExecJob(runDir string) int {
	gid, err := dirGid(runDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec-job:", err)
		return 125
	}
	// Create our own outputs (never follow a planted symlink) and give them the
	// run dir's group so siphon can read them back.
	open := func(name string) (*os.File, error) {
		f, err := os.OpenFile(filepath.Join(runDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o640)
		if err != nil {
			return nil, err
		}
		if err := f.Chown(-1, gid); err != nil {
			f.Close()
			return nil, err
		}
		return f, f.Chmod(0o640)
	}
	stdout, err := open("stdout")
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec-job:", err)
		return 125
	}
	defer stdout.Close()
	stderr, err := open("stderr")
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec-job:", err)
		return 125
	}
	defer stderr.Close()
	return execJob(runDir, filepath.Join(runDir, "job.json"), stdout, stderr)
}

func execJob(runDir, jobFile string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(jobFile)
	if err != nil {
		fmt.Fprintln(stderr, "exec-job:", err)
		return 125
	}
	// It holds the run's proxy token and logins: don't leave it readable by
	// other runs (all share group siphon-io) for the run's lifetime.
	os.Remove(jobFile)
	var spec JobSpec
	if err := json.Unmarshal(raw, &spec); err != nil || len(spec.Argv) == 0 {
		fmt.Fprintln(stderr, "exec-job: bad job spec")
		return 125
	}
	if err := os.MkdirAll(jobFilesDir, 0o700); err != nil {
		fmt.Fprintln(stderr, "exec-job:", err)
		return 125
	}
	if err := writeJobFiles(jobFilesDir, spec.Files); err != nil {
		fmt.Fprintln(stderr, "exec-job:", err)
		return 125
	}
	for _, name := range spec.Writeback {
		if _, err := relativeFile(name); err != nil {
			fmt.Fprintln(stderr, "exec-job:", err)
			return 125
		}
	}
	cmdEnv := []string{"HOME=" + jobFilesDir, "LANG=C.UTF-8"}
	if p, ok := os.LookupEnv("PATH"); ok {
		cmdEnv = append(cmdEnv, "PATH="+p)
	}
	for k, v := range spec.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			fmt.Fprintln(stderr, "exec-job: bad env")
			return 125
		}
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if spec.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutSec)*time.Second)
		defer cancel()
	}
	if spec.EgressSocket != "" {
		ln, err := net.Listen("tcp", forwardAddr)
		if err != nil {
			fmt.Fprintln(stderr, "egress forwarder:", err)
			return 1
		}
		defer ln.Close()
		go forward(ln, spec.EgressSocket)
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = cmdEnv
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	saveWritebacks(jobFilesDir, runDir, spec, stderr)
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
