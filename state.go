package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type StateStore struct {
	mu   sync.Mutex
	path string
	db   *sql.DB
}

func openStateStore(path string) (*StateStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	legacy, backup, err := moveLegacyJSONState(path)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite state: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &StateStore{path: path, db: db}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)

	if legacy != nil {
		if err := store.importLegacy(*legacy); err != nil {
			db.Close()
			return nil, fmt.Errorf("import legacy JSON state (backup retained at %s): %w", backup, err)
		}
		log.Printf("migrated legacy JSON state to SQLite; original retained at %s", backup)
	}
	return store, nil
}

func (s *StateStore) initialize() error {
	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable SQLite WAL mode: %w", err)
	}
	statements := []string{
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS programs (
			handle TEXT PRIMARY KEY,
			snapshot_json BLOB NOT NULL,
			missing_count INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS reports (
			report_id TEXT PRIMARY KEY,
			snapshot_json BLOB NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`INSERT INTO metadata(key, value) VALUES('schema_version', '2')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize SQLite state: %w", err)
		}
	}
	return nil
}

func (s *StateStore) Close() error {
	return s.db.Close()
}

func (s *StateStore) Snapshot() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := newState()
	state.Version = 2
	var err error
	if state.ProgramsInitialized, err = metadataBool(s.db, "programs_initialized"); err != nil {
		return State{}, err
	}
	if state.ReportsInitialized, err = metadataBool(s.db, "reports_initialized"); err != nil {
		return State{}, err
	}

	rows, err := s.db.Query(`SELECT handle, snapshot_json, missing_count FROM programs ORDER BY handle`)
	if err != nil {
		return State{}, fmt.Errorf("read program state: %w", err)
	}
	for rows.Next() {
		var handle string
		var raw []byte
		var missing int
		if err := rows.Scan(&handle, &raw, &missing); err != nil {
			rows.Close()
			return State{}, fmt.Errorf("scan program state: %w", err)
		}
		var snapshot ProgramSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			rows.Close()
			return State{}, fmt.Errorf("decode program %s state: %w", handle, err)
		}
		state.Programs[handle] = snapshot
		if missing > 0 {
			state.MissingPrograms[handle] = missing
		}
	}
	if err := rows.Close(); err != nil {
		return State{}, fmt.Errorf("close program state rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return State{}, fmt.Errorf("iterate program state: %w", err)
	}

	rows, err = s.db.Query(`SELECT report_id, snapshot_json FROM reports ORDER BY report_id`)
	if err != nil {
		return State{}, fmt.Errorf("read report state: %w", err)
	}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return State{}, fmt.Errorf("scan report state: %w", err)
		}
		var snapshot ReportSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			rows.Close()
			return State{}, fmt.Errorf("decode report %s state: %w", id, err)
		}
		state.Reports[id] = snapshot
	}
	if err := rows.Close(); err != nil {
		return State{}, fmt.Errorf("close report state rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return State{}, fmt.Errorf("iterate report state: %w", err)
	}
	return state, nil
}

func (s *StateStore) InitializePrograms(programs map[string]ProgramSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM programs`); err != nil {
			return err
		}
		for handle, snapshot := range programs {
			if err := upsertProgram(tx, handle, snapshot, 0); err != nil {
				return err
			}
		}
		return setMetadata(tx, "programs_initialized", "true")
	})
}

func (s *StateStore) SetProgram(handle string, snapshot ProgramSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(func(tx *sql.Tx) error { return upsertProgram(tx, handle, snapshot, 0) })
}

func (s *StateStore) UpdateProgramPresence(current map[string]ProgramSnapshot) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := make([]string, 0)
	err := s.withTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT handle, missing_count FROM programs`)
		if err != nil {
			return err
		}
		missing := make(map[string]int)
		for rows.Next() {
			var handle string
			var count int
			if err := rows.Scan(&handle, &count); err != nil {
				rows.Close()
				return err
			}
			missing[handle] = count
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for handle, count := range missing {
			if _, exists := current[handle]; exists {
				if count > 0 {
					if _, err := tx.Exec(`UPDATE programs SET missing_count=0 WHERE handle=?`, handle); err != nil {
						return err
					}
				}
				continue
			}
			count++
			if count >= 2 {
				if _, err := tx.Exec(`DELETE FROM programs WHERE handle=?`, handle); err != nil {
					return err
				}
				removed = append(removed, handle)
				continue
			}
			if _, err := tx.Exec(`UPDATE programs SET missing_count=? WHERE handle=?`, count, handle); err != nil {
				return err
			}
		}
		return nil
	})
	return removed, err
}

func (s *StateStore) InitializeReports(reports map[string]ReportSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM reports`); err != nil {
			return err
		}
		for id, snapshot := range reports {
			if err := upsertReport(tx, id, snapshot); err != nil {
				return err
			}
		}
		return setMetadata(tx, "reports_initialized", "true")
	})
}

func (s *StateStore) SetReport(id string, snapshot ReportSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(func(tx *sql.Tx) error { return upsertReport(tx, id, snapshot) })
}

func (s *StateStore) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin state transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state transaction: %w", err)
	}
	return nil
}

func upsertProgram(tx *sql.Tx, handle string, snapshot ProgramSnapshot, missing int) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode program %s: %w", handle, err)
	}
	_, err = tx.Exec(`INSERT INTO programs(handle, snapshot_json, missing_count, updated_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(handle) DO UPDATE SET snapshot_json=excluded.snapshot_json,
		missing_count=excluded.missing_count, updated_at=excluded.updated_at`,
		handle, raw, missing, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func upsertReport(tx *sql.Tx, id string, snapshot ReportSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode report %s: %w", id, err)
	}
	_, err = tx.Exec(`INSERT INTO reports(report_id, snapshot_json, updated_at)
		VALUES(?, ?, ?)
		ON CONFLICT(report_id) DO UPDATE SET snapshot_json=excluded.snapshot_json,
		updated_at=excluded.updated_at`, id, raw, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func metadataBool(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, key string) (bool, error) {
	var value string
	err := queryer.QueryRow(`SELECT value FROM metadata WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read metadata %s: %w", key, err)
	}
	return value == "true", nil
}

func setMetadata(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO metadata(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func moveLegacyJSONState(path string) (*State, string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("inspect state file: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, "", nil
	}
	state := newState()
	if err := json.Unmarshal(trimmed, &state); err != nil {
		return nil, "", fmt.Errorf("parse legacy JSON state: %w", err)
	}
	if state.Programs == nil {
		state.Programs = make(map[string]ProgramSnapshot)
	}
	if state.Reports == nil {
		state.Reports = make(map[string]ReportSnapshot)
	}
	if state.MissingPrograms == nil {
		state.MissingPrograms = make(map[string]int)
	}
	backup := path + ".json-backup"
	if _, err := os.Stat(backup); err == nil {
		backup = fmt.Sprintf("%s.%d", backup, time.Now().UTC().Unix())
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("inspect legacy state backup: %w", err)
	}
	if err := os.Rename(path, backup); err != nil {
		return nil, "", fmt.Errorf("preserve legacy JSON state: %w", err)
	}
	return &state, backup, nil
}

func (s *StateStore) importLegacy(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(func(tx *sql.Tx) error {
		for handle, snapshot := range state.Programs {
			if err := upsertProgram(tx, handle, snapshot, state.MissingPrograms[handle]); err != nil {
				return err
			}
		}
		for id, snapshot := range state.Reports {
			if err := upsertReport(tx, id, snapshot); err != nil {
				return err
			}
		}
		if state.ProgramsInitialized {
			if err := setMetadata(tx, "programs_initialized", "true"); err != nil {
				return err
			}
		}
		if state.ReportsInitialized {
			if err := setMetadata(tx, "reports_initialized", "true"); err != nil {
				return err
			}
		}
		return nil
	})
}
