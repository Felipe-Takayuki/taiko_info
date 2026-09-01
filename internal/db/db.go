package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Message represents a captured WhatsApp message.
type Message struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
	Processed int       `json:"processed"`
}

// Event represents an event extracted by the LLM.
type Event struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Category     string    `json:"category"` // "apresentacao", "treino", "geral"
	EventDate    time.Time `json:"event_date"`
	SourceSender string    `json:"source_sender"`
	CreatedAt    time.Time `json:"created_at"`
}

// DB wraps the sql.DB instance with a mutex for thread-safe serialized write operations if needed.
type DB struct {
	*sql.DB
	writeMu sync.Mutex
}

// Open initializes and configures the SQLite connection with concurrency-safe pragmas.
func Open(dbPath string) (*DB, error) {
	// Setup SQLite DSN with WAL mode, busy timeout, and normal sync
	params := url.Values{}
	params.Add("_journal_mode", "WAL")
	params.Add("_busy_timeout", "5000")
	params.Add("_synchronous", "NORMAL")
	params.Add("_foreign_keys", "1")

	dsn := fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	if !strings.HasPrefix(dbPath, "/") && !strings.HasPrefix(dbPath, "./") && !strings.HasPrefix(dbPath, "../") {
		dsn = fmt.Sprintf("file:%s?%s", dbPath, params.Encode())
	}

	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// SQLite best practice for concurrent writes: max 1 open connection to avoid SQLITE_BUSY
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite database: %w", err)
	}

	database := &DB{DB: conn}
	if err := database.migrate(ctx); err != nil {
		return nil, fmt.Errorf("failed to execute migrations: %w", err)
	}

	return database, nil
}

// migrate creates required tables and indexes.
func (d *DB) migrate(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		sender TEXT,
		message TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		processed INTEGER DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_messages_processed ON messages(processed);

	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		description TEXT,
		category TEXT NOT NULL DEFAULT 'geral',
		event_date DATETIME NOT NULL,
		source_sender TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_events_date ON events(event_date);
	`
	if _, err := d.ExecContext(ctx, schema); err != nil {
		return err
	}

	// Safe migration for pre-existing databases that might be missing the category column
	var count int
	_ = d.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('events') WHERE name='category'`).Scan(&count)
	if count == 0 {
		if _, err := d.ExecContext(ctx, `ALTER TABLE events ADD COLUMN category TEXT NOT NULL DEFAULT 'geral'`); err != nil {
			return fmt.Errorf("failed to add category column: %w", err)
		}
	}

	// Create index on category after column is guaranteed to exist
	if _, err := d.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_events_category ON events(category);`); err != nil {
		return fmt.Errorf("failed to create category index: %w", err)
	}

	return nil
}

// SaveMessage stores an incoming WhatsApp message into the database.
func (d *DB) SaveMessage(ctx context.Context, id, sender, message string, createdAt time.Time) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	query := `
	INSERT INTO messages (id, sender, message, created_at, processed)
	VALUES (?, ?, ?, ?, 0)
	ON CONFLICT(id) DO UPDATE SET
		message = excluded.message,
		sender = excluded.sender;
	`
	_, err := d.ExecContext(ctx, query, id, sender, message, createdAt.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// GetUnprocessedMessages fetches all unprocessed messages ordered chronologically.
func (d *DB) GetUnprocessedMessages(ctx context.Context, limit int) ([]Message, error) {
	query := `
	SELECT id, sender, message, created_at, processed
	FROM messages
	WHERE processed = 0
	ORDER BY created_at ASC
	`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAtStr string
		if err := rows.Scan(&m.ID, &m.Sender, &m.Message, &createdAtStr, &m.Processed); err != nil {
			return nil, err
		}
		// Parse date formats returned by SQLite
		t, err := parseSQLiteTime(createdAtStr)
		if err == nil {
			m.CreatedAt = t
		}
		msgs = append(msgs, m)
	}

	return msgs, rows.Err()
}

// SaveEventsAndMarkProcessed atomically inserts or updates extracted events and updates messages to processed = 1.
func (d *DB) SaveEventsAndMarkProcessed(ctx context.Context, events []Event, messageIDs []string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx failed: %w", err)
	}
	defer tx.Rollback()

	if len(events) > 0 {
		stmtInsert, err := tx.PrepareContext(ctx, `
			INSERT INTO events (title, description, category, event_date, source_sender, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`)
		if err != nil {
			return fmt.Errorf("prepare event insert failed: %w", err)
		}
		defer stmtInsert.Close()

		stmtUpdate, err := tx.PrepareContext(ctx, `
			UPDATE events
			SET title = ?, description = ?, category = ?, event_date = ?, source_sender = ?
			WHERE id = ?
		`)
		if err != nil {
			return fmt.Errorf("prepare event update failed: %w", err)
		}
		defer stmtUpdate.Close()

		now := time.Now().Format("2006-01-02 15:04:05")
		for _, e := range events {
			eventDateStr := e.EventDate.Format("2006-01-02 15:04:05")
			category := strings.TrimSpace(strings.ToLower(e.Category))
			if category == "" {
				category = "geral"
			}

			if e.ID > 0 {
				// Update existing event if ID is provided
				res, err := stmtUpdate.ExecContext(ctx, e.Title, e.Description, category, eventDateStr, e.SourceSender, e.ID)
				if err != nil {
					return fmt.Errorf("update event failed: %w", err)
				}
				rowsAffected, _ := res.RowsAffected()
				if rowsAffected == 0 {
					// Fallback: if event with ID does not exist, insert as new
					if _, err := stmtInsert.ExecContext(ctx, e.Title, e.Description, category, eventDateStr, e.SourceSender, now); err != nil {
						return fmt.Errorf("fallback insert event failed: %w", err)
					}
				}
			} else {
				// Insert new event
				if _, err := stmtInsert.ExecContext(ctx, e.Title, e.Description, category, eventDateStr, e.SourceSender, now); err != nil {
					return fmt.Errorf("insert event failed: %w", err)
				}
			}
		}
	}

	if len(messageIDs) > 0 {
		stmtMsg, err := tx.PrepareContext(ctx, `UPDATE messages SET processed = 1 WHERE id = ?`)
		if err != nil {
			return fmt.Errorf("prepare update message failed: %w", err)
		}
		defer stmtMsg.Close()

		for _, msgID := range messageIDs {
			if _, err := stmtMsg.ExecContext(ctx, msgID); err != nil {
				return fmt.Errorf("update message processed failed: %w", err)
			}
		}
	}

	return tx.Commit()
}

// GetEventByID fetches a single event by its ID.
func (d *DB) GetEventByID(ctx context.Context, id int64) (*Event, error) {
	query := `
	SELECT id, title, description, category, event_date, source_sender, created_at
	FROM events
	WHERE id = ?
	`
	var e Event
	var eventDateStr, createdAtStr string
	err := d.QueryRowContext(ctx, query, id).Scan(&e.ID, &e.Title, &e.Description, &e.Category, &eventDateStr, &e.SourceSender, &createdAtStr)
	if err != nil {
		return nil, err
	}
	if t, err := parseSQLiteTime(eventDateStr); err == nil {
		e.EventDate = t
	}
	if t, err := parseSQLiteTime(createdAtStr); err == nil {
		e.CreatedAt = t
	}
	return &e, nil
}

// GetAllEvents returns all events ordered by event_date descending.
func (d *DB) GetAllEvents(ctx context.Context) ([]Event, error) {
	return d.GetAllEventsInLocation(ctx, time.Local)
}

// GetAllEventsInLocation returns all events parsing date strings in the given timezone location.
func (d *DB) GetAllEventsInLocation(ctx context.Context, loc *time.Location) ([]Event, error) {
	if loc == nil {
		loc = time.Local
	}

	query := `
	SELECT id, title, description, category, event_date, source_sender, created_at
	FROM events
	ORDER BY event_date DESC
	`
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var eventDateStr, createdAtStr string
		if err := rows.Scan(&e.ID, &e.Title, &e.Description, &e.Category, &eventDateStr, &e.SourceSender, &createdAtStr); err != nil {
			return nil, err
		}
		if e.Category == "" {
			e.Category = "geral"
		}
		if t, err := ParseSQLiteTimeInLocation(eventDateStr, loc); err == nil {
			e.EventDate = t
		}
		if t, err := ParseSQLiteTimeInLocation(createdAtStr, loc); err == nil {
			e.CreatedAt = t
		}
		events = append(events, e)
	}

	return events, rows.Err()
}

// GetStats returns summary counts for dashboard metrics.
func (d *DB) GetStats(ctx context.Context) (totalEvents int, unprocessedMsgs int, processedMsgs int, err error) {
	err = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&totalEvents)
	if err != nil {
		return
	}
	err = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE processed = 0`).Scan(&unprocessedMsgs)
	if err != nil {
		return
	}
	err = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE processed = 1`).Scan(&processedMsgs)
	return
}

func parseSQLiteTime(s string) (time.Time, error) {
	return ParseSQLiteTimeInLocation(s, time.Local)
}

// ParseSQLiteTimeInLocation parses SQLite date strings preserving wall-clock time in loc.
func ParseSQLiteTimeInLocation(s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	s = strings.TrimSpace(s)
	sClean := strings.TrimSuffix(s, "Z")
	sClean = strings.TrimSuffix(sClean, "z")

	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, sClean, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time: %s", s)
}
