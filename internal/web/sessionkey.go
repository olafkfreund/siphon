package web

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

const sessionKeyFile = "portal-session.key"

// LoadSessionKey returns the 32-byte portal session key stored in dir,
// creating it if missing. An error means nothing could be written; the caller
// falls back to a per-process key. The key is never logged or put in an error.
func LoadSessionKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, sessionKeyFile)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		f, cerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if cerr == nil {
			_, werr := f.Write(key)
			if cerr = errors.Join(werr, f.Close()); cerr != nil {
				os.Remove(path)
			}
			return key, cerr
		}
		if errors.Is(cerr, fs.ErrExist) { // lost a creation race: use the winner's key
			if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
				return b, nil
			}
		} else {
			return nil, cerr
		}
	}
	slog.Warn("portal session key unusable, generating a new one")
	tmp, err := os.CreateTemp(dir, sessionKeyFile+".*")
	if err != nil {
		return nil, err
	}
	_, werr := tmp.Write(key)
	if err := errors.Join(werr, tmp.Close(), os.Rename(tmp.Name(), path)); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	return key, nil
}
