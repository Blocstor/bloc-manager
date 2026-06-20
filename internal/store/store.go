package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Volume represents a DRBD volume record.
type Volume struct {
	ID             string
	Name           string
	Nodes          []string
	Minor          int
	SizeMB         int
	AttachedTo     string // Kubernetes node name (e.g., "cluster-a-worker-0")
	AttachedDevice string // VM virtio device path (e.g., "/dev/vdb")
}

// Store wraps a SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS volumes (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL UNIQUE,
	minor           INTEGER NOT NULL UNIQUE,
	size_mb         INTEGER NOT NULL,
	attached_to     TEXT DEFAULT '',
	attached_device TEXT DEFAULT '',
	nodes           TEXT NOT NULL,
	created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT
);
`

// NewStore opens the SQLite database at dsn and runs schema migrations.
func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=10000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run schema: %w", err)
	}

	// Migrate: add attached_device column if it doesn't exist yet.
	_, _ = db.Exec(`ALTER TABLE volumes ADD COLUMN attached_device TEXT DEFAULT ''`)

	// Seed next_minor if not present.
	_, err = db.Exec(
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('next_minor', '1000')`,
	)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("seed next_minor: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// AllocateMinor atomically increments next_minor and returns the new value.
func (s *Store) AllocateMinor() (int, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var current int
	if err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='next_minor'`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read next_minor: %w", err)
	}

	next := current + 1
	if _, err := tx.Exec(`UPDATE meta SET value=? WHERE key='next_minor'`, fmt.Sprintf("%d", next)); err != nil {
		return 0, fmt.Errorf("update next_minor: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	return current, nil
}

// CreateVolume inserts a new volume record.
func (s *Store) CreateVolume(v Volume) error {
	nodesJSON, err := json.Marshal(v.Nodes)
	if err != nil {
		return fmt.Errorf("marshal nodes: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO volumes (id, name, minor, size_mb, attached_to, attached_device, nodes)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.Name, v.Minor, v.SizeMB, v.AttachedTo, v.AttachedDevice, string(nodesJSON),
	)
	if err != nil {
		return fmt.Errorf("insert volume: %w", err)
	}
	return nil
}

// GetVolume retrieves a volume by ID.
func (s *Store) GetVolume(id string) (*Volume, error) {
	row := s.db.QueryRow(
		`SELECT id, name, minor, size_mb, attached_to, attached_device, nodes FROM volumes WHERE id=?`, id,
	)
	return scanVolume(row)
}

// ListVolumes returns all volumes.
func (s *Store) ListVolumes() ([]Volume, error) {
	rows, err := s.db.Query(
		`SELECT id, name, minor, size_mb, attached_to, attached_device, nodes FROM volumes ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("query volumes: %w", err)
	}
	defer rows.Close()

	var volumes []Volume
	for rows.Next() {
		v, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, *v)
	}
	return volumes, rows.Err()
}

// UpdateVolume updates an existing volume record.
func (s *Store) UpdateVolume(v Volume) error {
	nodesJSON, err := json.Marshal(v.Nodes)
	if err != nil {
		return fmt.Errorf("marshal nodes: %w", err)
	}

	res, err := s.db.Exec(
		`UPDATE volumes SET name=?, minor=?, size_mb=?, attached_to=?, attached_device=?, nodes=? WHERE id=?`,
		v.Name, v.Minor, v.SizeMB, v.AttachedTo, v.AttachedDevice, string(nodesJSON), v.ID,
	)
	if err != nil {
		return fmt.Errorf("update volume: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("volume %s not found", v.ID)
	}
	return nil
}

// DeleteVolume removes a volume record by ID.
func (s *Store) DeleteVolume(id string) error {
	res, err := s.db.Exec(`DELETE FROM volumes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("volume %s not found", id)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanVolume(s scanner) (*Volume, error) {
	var v Volume
	var nodesJSON string

	if err := s.Scan(&v.ID, &v.Name, &v.Minor, &v.SizeMB, &v.AttachedTo, &v.AttachedDevice, &nodesJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan volume: %w", err)
	}

	if err := json.Unmarshal([]byte(nodesJSON), &v.Nodes); err != nil {
		return nil, fmt.Errorf("unmarshal nodes: %w", err)
	}

	return &v, nil
}
