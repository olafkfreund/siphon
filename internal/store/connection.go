package store

import (
	"database/sql"
	"time"
)

// ConnectionCheck is the last Test of a connection.
type ConnectionCheck struct {
	Connection string    `json:"connection"`
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Detail     string    `json:"detail"`
}

func SetConnectionCheck(db *sql.DB, c ConnectionCheck) error {
	_, err := db.Exec(`INSERT INTO connection_check(connection, at, ok, detail) VALUES (?,?,?,?)
		ON CONFLICT(connection) DO UPDATE SET at=excluded.at, ok=excluded.ok, detail=excluded.detail`,
		c.Connection, c.At.Unix(), c.OK, c.Detail)
	return err
}

// GetConnectionCheck returns nil if the connection was never tested.
func GetConnectionCheck(db *sql.DB, connection string) (*ConnectionCheck, error) {
	c := ConnectionCheck{Connection: connection}
	var at int64
	err := db.QueryRow(`SELECT at, ok, detail FROM connection_check WHERE connection=?`, connection).Scan(&at, &c.OK, &c.Detail)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	c.At = time.Unix(at, 0)
	return &c, err
}
