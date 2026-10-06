package store

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"time"
)

// Config overlay: portal edits stored on top of the config file.

type ConfigItem struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	YAML      string    `json:"yaml"`
	Deleted   bool      `json:"deleted"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ConfigRevision struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	Summary   string    `json:"summary"`
	ItemsJSON string    `json:"items_json"`
	Diff      string    `json:"diff"`
}

func ConfigItems(db *sql.DB) ([]ConfigItem, error) {
	rows, err := db.Query(`SELECT kind, name, yaml, deleted, updated_at FROM config_item ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigItem
	for rows.Next() {
		var it ConfigItem
		var at int64
		if err := rows.Scan(&it.Kind, &it.Name, &it.YAML, &it.Deleted, &at); err != nil {
			return nil, err
		}
		it.UpdatedAt = time.UnixMilli(at)
		out = append(out, it)
	}
	return out, rows.Err()
}

// PutConfigItem upserts an overlay item; deleted=true is a tombstone for a file item.
func PutConfigItem(tx *sql.Tx, kind, name, yaml string, deleted bool, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO config_item(kind,name,yaml,deleted,updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(kind,name) DO UPDATE SET yaml=excluded.yaml, deleted=excluded.deleted, updated_at=excluded.updated_at`,
		kind, name, yaml, deleted, ms(now))
	return err
}

// DeleteConfigItem removes the overlay row ("reset to file").
func DeleteConfigItem(tx *sql.Tx, kind, name string) error {
	_, err := tx.Exec(`DELETE FROM config_item WHERE kind=? AND name=?`, kind, name)
	return err
}

func AddRevision(tx *sql.Tx, now time.Time, actor, summary, itemsJSON, diff string) (int64, error) {
	r, err := tx.Exec(`INSERT INTO config_revision(at,actor,summary,items_json,diff) VALUES (?,?,?,?,?)`,
		ms(now), actor, summary, itemsJSON, diff)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

type scanner interface{ Scan(...any) error }

func scanRevision(s scanner) (ConfigRevision, error) {
	var r ConfigRevision
	var at int64
	err := s.Scan(&r.ID, &at, &r.Actor, &r.Summary, &r.ItemsJSON, &r.Diff)
	r.At = time.UnixMilli(at)
	return r, err
}

const revCols = `SELECT id, at, actor, summary, items_json, diff FROM config_revision`

// Revisions lists the newest revisions first.
func Revisions(db *sql.DB, limit int) ([]ConfigRevision, error) {
	rows, err := db.Query(revCols+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRevision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Revision returns sql.ErrNoRows if id does not exist.
func Revision(db *sql.DB, id int64) (ConfigRevision, error) {
	return scanRevision(db.QueryRow(revCols+` WHERE id=?`, id))
}

// LatestRevision reports ok=false when there are no revisions yet.
func LatestRevision(db *sql.DB) (r ConfigRevision, ok bool, err error) {
	r, err = scanRevision(db.QueryRow(revCols + ` ORDER BY id DESC LIMIT 1`))
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

const maxEventBytes = 64 << 10

var secretKey = regexp.MustCompile(`(?i)token|secret|password|authorization|api_key|cookie`)

// RedactKeys returns v with the value of every map key that looks secret
// replaced by "[redacted]", recursively. v must be JSON-decoded data.
func RedactKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if secretKey.MatchString(k) {
				out[k] = "[redacted]"
			} else {
				out[k] = RedactKeys(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = RedactKeys(e)
		}
		return out
	}
	return v
}

// SetSourceEvent stores a source's last event (redacted, capped at 64 KiB) for
// the rule tester. data is any JSON-marshalable value.
func SetSourceEvent(db *sql.DB, source string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	b, err = json.Marshal(RedactKeys(v))
	if err != nil {
		return err
	}
	if len(b) > maxEventBytes {
		b = []byte(`{"_truncated":true}`)
	}
	_, err = db.Exec(`INSERT INTO source_state(source,json) VALUES (?,?)
		ON CONFLICT(source) DO UPDATE SET json=excluded.json`, source, string(b))
	return err
}
