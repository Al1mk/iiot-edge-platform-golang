package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/alimk/iiot-edge-platform/pkg/models"
)

// store wraps the SQLite database and exposes the operations needed by the
// ingestor HTTP handlers. All methods are safe for concurrent use; SQLite is
// opened with WAL mode and a single *sql.DB whose connection pool serialises
// writes while allowing concurrent reads.
type store struct {
	db *sql.DB
}

// openStore opens (or creates) the SQLite database at path and runs the
// schema migration. Returns a ready-to-use *store.
func openStore(path string) (*store, error) {
	// ?_journal_mode=WAL allows concurrent readers alongside one writer.
	// ?_busy_timeout=5000 retries for up to 5 s on lock contention.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Limit to one writer connection to avoid SQLITE_BUSY under load.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &store{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS telemetry (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id     TEXT    NOT NULL,
    ts_rfc3339    TEXT    NOT NULL,
    received_at_unix INTEGER NOT NULL,
    temperature_c REAL    NOT NULL,
    pressure_hpa  REAL    NOT NULL,
    humidity_pct  REAL    NOT NULL,
    status        TEXT    NOT NULL,
    raw_json      TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_telemetry_device_received
    ON telemetry (device_id, received_at_unix DESC);
`)
	return err
}

// insert persists one accepted telemetry reading.
func (s *store) insert(r models.TelemetryReading, receivedAt int64) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal raw_json: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO telemetry
		    (device_id, ts_rfc3339, received_at_unix,
		     temperature_c, pressure_hpa, humidity_pct, status, raw_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DeviceID,
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		receivedAt,
		r.TemperatureC,
		r.PressureHPA,
		r.HumidityPct,
		r.Status,
		string(raw),
	)
	return err
}

// telemetryRow is what the query endpoints return.
type telemetryRow struct {
	ID           int64   `json:"id"`
	DeviceID     string  `json:"device_id"`
	TsRFC3339    string  `json:"ts_rfc3339"`
	ReceivedAt   int64   `json:"received_at_unix"`
	TemperatureC float64 `json:"temperature_c"`
	PressureHPA  float64 `json:"pressure_hpa"`
	HumidityPct  float64 `json:"humidity_pct"`
	Status       string  `json:"status"`
}

// queryLast returns the most-recently received record, optionally filtered by
// deviceID (empty string = no filter). Returns (nil, nil) when no rows match.
func (s *store) queryLast(deviceID string) (*telemetryRow, error) {
	var q string
	var args []any
	if deviceID != "" {
		q = `SELECT id, device_id, ts_rfc3339, received_at_unix,
		            temperature_c, pressure_hpa, humidity_pct, status
		     FROM telemetry WHERE device_id = ?
		     ORDER BY received_at_unix DESC, id DESC LIMIT 1`
		args = []any{deviceID}
	} else {
		q = `SELECT id, device_id, ts_rfc3339, received_at_unix,
		            temperature_c, pressure_hpa, humidity_pct, status
		     FROM telemetry
		     ORDER BY received_at_unix DESC, id DESC LIMIT 1`
	}
	row := s.db.QueryRow(q, args...)
	return scanRow(row)
}

// queryRecent returns the last limit records, newest first, optionally
// filtered by deviceID. limit is clamped to [1, 500] by the caller.
func (s *store) queryRecent(deviceID string, limit int) ([]telemetryRow, error) {
	var q string
	var args []any
	if deviceID != "" {
		q = `SELECT id, device_id, ts_rfc3339, received_at_unix,
		            temperature_c, pressure_hpa, humidity_pct, status
		     FROM telemetry WHERE device_id = ?
		     ORDER BY received_at_unix DESC, id DESC LIMIT ?`
		args = []any{deviceID, limit}
	} else {
		q = `SELECT id, device_id, ts_rfc3339, received_at_unix,
		            temperature_c, pressure_hpa, humidity_pct, status
		     FROM telemetry
		     ORDER BY received_at_unix DESC, id DESC LIMIT ?`
		args = []any{limit}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []telemetryRow
	for rows.Next() {
		r := telemetryRow{}
		if err := rows.Scan(
			&r.ID, &r.DeviceID, &r.TsRFC3339, &r.ReceivedAt,
			&r.TemperatureC, &r.PressureHPA, &r.HumidityPct, &r.Status,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ping returns nil if the DB is reachable.
func (s *store) ping() error {
	return s.db.Ping()
}

// close releases all DB resources.
func (s *store) close() error {
	return s.db.Close()
}

// scanRow scans a single *sql.Row into a telemetryRow.
// Returns (nil, nil) on sql.ErrNoRows.
func scanRow(row *sql.Row) (*telemetryRow, error) {
	r := &telemetryRow{}
	err := row.Scan(
		&r.ID, &r.DeviceID, &r.TsRFC3339, &r.ReceivedAt,
		&r.TemperatureC, &r.PressureHPA, &r.HumidityPct, &r.Status,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
