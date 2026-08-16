package store

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the highest migration this binary understands.
//
// A binary refuses to open a database stamped higher than this: an older build
// silently misreading a newer schema is a corruption path, and this hub holds
// the only copy of any op whose origin device has since pruned it.
const schemaVersion = 1

const schemaV1 = `
CREATE TABLE ops (
  user_id          TEXT    NOT NULL,
  seq              INTEGER NOT NULL,
  entity_id        TEXT    NOT NULL,
  entity_rev       TEXT    NOT NULL,
  kind             TEXT    NOT NULL,
  origin_device_id TEXT    NOT NULL,
  operation_sha256 TEXT    NOT NULL,
  body             TEXT    NOT NULL,
  server_ts        INTEGER NOT NULL,
  PRIMARY KEY (user_id, seq)
);

CREATE UNIQUE INDEX ux_ops_entity ON ops (user_id, entity_id, entity_rev);

CREATE TABLE meta (
  user_id  TEXT    PRIMARY KEY,
  epoch    TEXT    NOT NULL,
  head_seq INTEGER NOT NULL
);

CREATE TABLE devices (
  user_id     TEXT    NOT NULL,
  device_id   TEXT    NOT NULL,
  device_name TEXT,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  revoked_at  INTEGER,
  PRIMARY KEY (user_id, device_id)
);
`

func migrate(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("store: reading user_version: %w", err)
	}

	if current > schemaVersion {
		return fmt.Errorf(
			"store: database schema is version %d but this build understands %d; upgrade cmemlan",
			current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if current < 1 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return fmt.Errorf("store: applying schema v1: %w", err)
		}
	}

	// PRAGMA does not accept a bound parameter.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("store: stamping user_version: %w", err)
	}
	return tx.Commit()
}
