package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
)

// openRO opens a db file read-only and raw: no migrations, no pragmas that write,
// so an older daemon's live db is never altered.
func openRO(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Snapshot writes a consistent copy of the db at dbPath to dest (VACUUM INTO, no lock taken)
// and returns the newest migration the copy has applied.
func Snapshot(dbPath, dest string) (schema string, err error) {
	db, err := openRO(url.PathEscape(dbPath))
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return "", err
	}
	return schemaOf(dest)
}

func schemaOf(path string) (string, error) {
	db, err := openRO(url.PathEscape(path))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v sql.NullString
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return "", err
	}
	return v.String, nil
}

// Verify checks a db file about to be restored: PRAGMA integrity_check, no triggers or views
// (the migrations create none; one would run on the daemon's later writes), and a schema
// this binary knows. It returns that schema, read from the db itself, not a manifest.
func Verify(path string) (schema string, err error) {
	db, err := openRO(url.PathEscape(path))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		return "", err
	}
	if res != "ok" {
		return "", errors.New("integrity_check: " + res)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type IN ('trigger','view')`).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "", errors.New("the database has triggers or views, which siphon never creates")
	}
	if schema, err = schemaOf(path); err != nil || schema == "" {
		return "", errors.New("not a siphon database (no schema_migrations)")
	}
	if schema > NewestMigration() {
		return "", fmt.Errorf("the database is from a newer siphon (schema %s)", schema)
	}
	return schema, nil
}

// JobCount is count(*) FROM jobs of a db file, read-only.
func JobCount(path string) (int, error) {
	db, err := openRO(url.PathEscape(path))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count jobs: %w", err)
	}
	return n, nil
}
