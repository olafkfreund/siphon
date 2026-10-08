package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

// exitError carries a specific exit code out of a local command.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

const (
	manifestName   = "siphon-backup.json"
	restoreMaxSize = 1 << 30
	// restoreMaxEntries caps files and dirs in an archive.
	restoreMaxEntries = 100_000
)

type manifest struct {
	Format  int    `json:"format"`
	Version string `json:"version"`
	Schema  string `json:"schema"`
	Created string `json:"created"`
}

func backup(args []string) error {
	if len(args) > 0 && args[0] == "create" {
		return backupCreate(args[1:], os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdout.Fd())))
	}
	if len(args) > 0 && args[0] == "restore" {
		return backupRestore(args[1:], os.Stdin, os.Stderr, term.IsTerminal(int(os.Stdin.Fd())))
	}
	return exitError{2, errors.New("usage: siphon backup create [-config f | -db path] [--force] <file|-> | backup restore [-config f | -db path] [--yes] <file|->")}
}

// backupDB is -db if given, else Server.DB of -config.
func backupDB(fl *flag.FlagSet, cfgPath, db string) (string, error) {
	if db != "" {
		return db, nil
	}
	cfg, err := config.Load(resolveConfig(fl, cfgPath))
	if err != nil {
		return "", err
	}
	return cfg.Server.DB, nil
}

// refuseTTY stops an archive being written to a terminal.
func refuseTTY(isTTY bool) error {
	if isTTY {
		return exitError{2, errors.New("refusing to write an archive to a terminal: redirect stdout or give a file")}
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func backupCreate(args []string, stdout, stderr io.Writer, stdoutTTY bool) (err error) {
	fl := flag.NewFlagSet("backup create", flag.ContinueOnError)
	cfgPath := fl.String("config", "siphon.yaml", "config file")
	dbFlag := fl.String("db", "", "database file (wins over -config)")
	force := fl.Bool("force", false, "replace an existing file")
	if err := fl.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fl.NArg() != 1 {
		return exitError{2, errors.New("usage: siphon backup create [-config f | -db path] [--force] <file|->")}
	}
	target := fl.Arg(0)
	db, err := backupDB(fl, *cfgPath, *dbFlag)
	if err != nil {
		return err
	}
	dir := filepath.Dir(db)
	if target == "-" {
		if err := refuseTTY(stdoutTTY); err != nil {
			return err
		}
	} else if _, serr := os.Lstat(target); serr == nil && !*force {
		return fmt.Errorf("%s exists: pass --force to replace it", target)
	}

	tmp, err := os.MkdirTemp(dir, ".backup-*") // 0700
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	snap := filepath.Join(tmp, "state.db")
	schema, err := store.Snapshot(db, snap) // first; the directories are read after
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", db, err)
	}

	var out io.Writer = stdout
	var tmpFile *os.File
	var tmpName string
	if target != "-" {
		var r [6]byte
		rand.Read(r[:])
		tmpName = target + ".tmp-" + hex.EncodeToString(r[:])
		if tmpFile, err = os.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				tmpFile.Close()
				os.Remove(tmpName)
			}
		}()
		out = tmpFile
	}
	cw := &countWriter{w: out}
	secrets, logins, err := writeArchive(cw, dir, snap, schema, stderr)
	if err != nil {
		return err
	}
	if tmpFile != nil {
		if err = tmpFile.Sync(); err != nil {
			return err
		}
		if err = tmpFile.Close(); err != nil {
			return err
		}
		if *force {
			err = os.Rename(tmpName, target)
		} else if err = os.Link(tmpName, target); err == nil { // fails if a file appeared meanwhile
			os.Remove(tmpName)
		}
		if err != nil {
			os.Remove(tmpName)
			return err
		}
	}
	where := target
	if target == "-" {
		where = "stdout"
	}
	fmt.Fprintf(stderr, "backed up to %s (%d bytes, %d secrets, %d logins)\n", where, cw.n, secrets, logins)
	return nil
}

// writeArchive writes the tar.gz: manifest, state.db, secrets/, credentials/.
func writeArchive(w io.Writer, dir, snap, schema string, warn io.Writer) (secrets, logins int, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(manifest{1, version, schema, time.Now().UTC().Format(time.RFC3339)})
	if err = addBytes(tw, manifestName, mb); err != nil {
		return
	}
	if err = addFile(tw, "state.db", snap); err != nil {
		return
	}
	for _, d := range []string{"secrets", "credentials"} {
		var n int
		if n, err = addTree(tw, dir, d, warn); err != nil {
			return
		}
		if d == "secrets" {
			secrets = n
		} else {
			logins = n
		}
	}
	if err = tw.Close(); err != nil {
		return
	}
	err = gz.Close()
	return
}

func addBytes(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// addTree adds dir/sub: directories and regular files only; anything else is skipped with a warning.
func addTree(tw *tar.Writer, dir, sub string, warn io.Writer) (files int, err error) {
	root := filepath.Join(dir, sub)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o700, ModTime: time.Now()})
		case d.Type().IsRegular():
			files++
			return addFile(tw, rel, p)
		}
		fmt.Fprintf(warn, "warning: skipping %s (not a regular file)\n", p)
		return nil
	})
	return files, err
}

func backupRestore(args []string, in io.Reader, stderr io.Writer, stdinTTY bool) error {
	fl := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	cfgPath := fl.String("config", "siphon.yaml", "config file")
	dbFlag := fl.String("db", "", "database file (wins over -config)")
	yes := fl.Bool("yes", false, "replace existing state without asking")
	if err := fl.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fl.NArg() != 1 {
		return exitError{2, errors.New("usage: siphon backup restore [-config f | -db path] [--yes] <file|->")}
	}
	src := fl.Arg(0)
	db, err := backupDB(fl, *cfgPath, *dbFlag)
	if err != nil {
		return err
	}
	dir := filepath.Dir(db)
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// A new host: create it as the caller, but never as root, or the daemon's user could not read it.
		if os.Getuid() == 0 {
			return fmt.Errorf("%s does not exist: create it owned by the daemon's user, or run as that user", dir)
		}
		if err = os.MkdirAll(dir, 0o700); err == nil {
			fi, err = os.Stat(dir)
		}
	}
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("run as the user that owns %s (sudo -u #%d …)", dir, st.Uid)
	}
	unlock, err := store.Lock(db)
	if err != nil {
		return exitError{5, fmt.Errorf("siphon is running (it holds %s.lock): stop it first", db)}
	}
	defer unlock()

	r := in
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	tmp, err := os.MkdirTemp(dir, ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractBackup(r, tmp); err != nil {
		return fmt.Errorf("rejected %s: %w", src, err)
	}

	secretsDir, credsDir := filepath.Join(dir, "secrets"), filepath.Join(dir, "credentials")
	old := []string{db, db + "-wal", db + "-shm", secretsDir, credsDir}
	exists := false
	for _, p := range old {
		if _, err := os.Lstat(p); err == nil {
			exists = true
		}
	}
	if exists {
		desc := "restore replaces existing state:"
		if st, err := os.Stat(db); err == nil {
			jobs := "?"
			if n, err := store.JobCount(db); err == nil {
				jobs = fmt.Sprint(n)
			}
			desc += fmt.Sprintf(" db %d bytes, %s jobs,", st.Size(), jobs)
		}
		fmt.Fprintf(stderr, "%s %d secret files, %d credential files\n", desc, countFiles(secretsDir), countFiles(credsDir))
		switch {
		case *yes:
		case stdinTTY && src != "-":
			fmt.Fprint(stderr, "continue? [y/N] ")
			line, _ := bufio.NewReader(in).ReadString('\n')
			if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
				return exitError{1, errors.New("aborted")}
			}
		default:
			return exitError{2, errors.New("restore replaces existing state: pass --yes")}
		}
	}

	pre := filepath.Join(dir, "pre-restore-"+time.Now().UTC().Format("20060102T150405Z"))
	if exists {
		if err := os.Mkdir(pre, 0o700); err != nil {
			return err
		}
	}
	for _, p := range old {
		if !exists {
			break
		}
		if err := os.Rename(p, filepath.Join(pre, filepath.Base(p))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("moving %s aside (previous state is in %s): %w", p, pre, err)
		}
	}
	for _, m := range [][2]string{{filepath.Join(tmp, "state.db"), db}, {filepath.Join(tmp, "secrets"), secretsDir}, {filepath.Join(tmp, "credentials"), credsDir}} {
		if err := os.Rename(m[0], m[1]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("installing %s: %w; the state is now mixed: move the files in %s back", m[1], err, pre)
		}
	}
	if !exists {
		pre = "nowhere (there was none)"
	}
	fmt.Fprintf(stderr, "restored; the previous state is in %s; start siphon, then run siphon validate\n", pre)
	return nil
}

func countFiles(root string) (n int) {
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return
}

// extractBackup unpacks the archive into dest (0700, empty) and validates it; any problem rejects the whole archive.
func extractBackup(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	left := int64(restoreMaxSize)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%q: only files and directories are allowed", h.Name)
		}
		if strings.HasPrefix(name, "/") || name == "" || seen[name] {
			return fmt.Errorf("%q: bad or duplicate name", h.Name)
		}
		for _, el := range strings.Split(name, "/") {
			if el == "" || el == "." || el == ".." {
				return fmt.Errorf("%q: bad name", h.Name)
			}
		}
		seen[name] = true
		// each header counts too, so empty files and dirs can't flood the inode table
		if left -= 512; left < 0 || len(seen) > restoreMaxEntries {
			return errors.New("archive is larger than 1 GiB or has too many entries")
		}
		isDir := h.Typeflag == tar.TypeDir
		switch {
		case name == manifestName || name == "state.db":
			if isDir {
				return fmt.Errorf("%q: must be a file", h.Name)
			}
		case name == "secrets" || name == "credentials":
			if !isDir {
				return fmt.Errorf("%q: must be a directory", h.Name)
			}
		case strings.HasPrefix(name, "secrets/") || strings.HasPrefix(name, "credentials/"):
		default:
			return fmt.Errorf("%q: unexpected entry", h.Name)
		}
		p := filepath.Join(dest, filepath.FromSlash(name))
		if isDir {
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(tr, left+1))
		f.Close()
		if err != nil {
			return err
		}
		if left -= n; left < 0 {
			return errors.New("archive is larger than 1 GiB")
		}
	}
	mb, err := os.ReadFile(filepath.Join(dest, manifestName))
	if err != nil {
		return errors.New("no " + manifestName)
	}
	var m manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.Format != 1 {
		return errors.New("unsupported manifest")
	}
	if m.Schema > store.NewestMigration() {
		return fmt.Errorf("backup is from a newer siphon (%s)", m.Version)
	}
	if _, err := os.Stat(filepath.Join(dest, "state.db")); err != nil {
		return errors.New("no state.db")
	}
	_, err = store.Verify(filepath.Join(dest, "state.db")) // the db's own schema, not just the manifest's
	return err
}
