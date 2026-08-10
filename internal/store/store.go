package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/text/unicode/norm"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("note not found")

type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalidInput(message string) error { return &ValidationError{Message: message} }

type ConflictError struct {
	Current Note
}

func (e *ConflictError) Error() string { return "note was changed by another client" }

type IdempotencyConflictError struct {
	Current Note
}

func (e *IdempotencyConflictError) Error() string {
	return "request id was already used for different note content"
}

type Note struct {
	ID              int64   `json:"id"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	Starred         bool    `json:"starred"`
	ContinuedFromID *int64  `json:"continuedFromId,omitempty"`
	Revision        int     `json:"revision"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
	DeletedAt       *string `json:"deletedAt,omitempty"`
}

type CreateNoteInput struct {
	RequestID       string
	Title           string
	Content         string
	Starred         bool
	ContinuedFromID *int64
}

type UpdateNoteInput struct {
	Title            *string
	Content          *string
	Starred          *bool
	ExpectedRevision int
}

type ListOptions struct {
	Query string
	View  string
	Limit int
}

type Context struct {
	Before  []Note `json:"before"`
	Current Note   `json:"current"`
	After   []Note `json:"after"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dsn := path + "?_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateNote(ctx context.Context, in CreateNoteInput) (Note, bool, error) {
	if strings.TrimSpace(in.Content) == "" {
		return Note{}, false, invalidInput("content is required")
	}
	if len(in.Content) > 1<<20 {
		return Note{}, false, invalidInput("content exceeds 1 MiB")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, false, err
	}
	defer tx.Rollback()

	requestID := strings.TrimSpace(in.RequestID)
	requestHash := createRequestHash(in)
	if requestID != "" {
		note, existingHash, err := getNoteByRequestID(ctx, tx, requestID)
		if err == nil {
			if existingHash != requestHash {
				return Note{}, false, &IdempotencyConflictError{Current: note}
			}
			return note, true, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return Note{}, false, err
		}
	}
	if in.ContinuedFromID != nil {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM notes WHERE id = ? AND deleted_at IS NULL`, *in.ContinuedFromID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Note{}, false, invalidInput("continued note does not exist")
			}
			return Note{}, false, err
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var request any
	if requestID != "" {
		request = requestID
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO notes (request_id, request_hash, title, content, search_text, starred, continued_from_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		request, requestHash, in.Title, in.Content, normalizeSearch(in.Title+"\n"+in.Content), in.Starred, in.ContinuedFromID, now, now)
	if err != nil {
		if requestID != "" && strings.Contains(strings.ToLower(err.Error()), "unique") {
			note, existingHash, getErr := getNoteByRequestID(ctx, tx, requestID)
			if getErr != nil {
				return Note{}, false, getErr
			}
			if existingHash != requestHash {
				return Note{}, false, &IdempotencyConflictError{Current: note}
			}
			return note, true, nil
		}
		return Note{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Note{}, false, err
	}
	note := Note{ID: id, Title: in.Title, Content: in.Content, Starred: in.Starred, ContinuedFromID: in.ContinuedFromID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := insertRevision(ctx, tx, note); err != nil {
		return Note{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Note{}, false, err
	}
	return note, false, nil
}

func (s *Store) GetNote(ctx context.Context, id int64) (Note, error) {
	return getNote(ctx, s.db, id)
}

func (s *Store) ListNotes(ctx context.Context, opts ListOptions) ([]Note, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	where := []string{"deleted_at IS NULL"}
	args := make([]any, 0, 8)
	switch opts.View {
	case "", "recent", "all":
	case "starred":
		where = append(where, "starred = 1")
	case "trash":
		where[0] = "deleted_at IS NOT NULL"
	default:
		return nil, invalidInput("unknown view")
	}
	for _, token := range queryTokens(opts.Query) {
		where = append(where, `search_text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(token)+"%")
	}
	args = append(args, limit)
	query := `SELECT id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at
		FROM notes WHERE ` + strings.Join(where, " AND ") + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := make([]Note, 0)
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func (s *Store) UpdateNote(ctx context.Context, id int64, in UpdateNoteInput) (Note, error) {
	if in.ExpectedRevision <= 0 {
		return Note{}, invalidInput("expected revision is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	current, err := getNote(ctx, tx, id)
	if err != nil {
		return Note{}, err
	}
	if current.DeletedAt != nil {
		return Note{}, ErrNotFound
	}
	if current.Revision != in.ExpectedRevision {
		return Note{}, &ConflictError{Current: current}
	}
	if in.Title != nil {
		current.Title = *in.Title
	}
	if in.Content != nil {
		if strings.TrimSpace(*in.Content) == "" {
			return Note{}, invalidInput("content cannot be empty")
		}
		if len(*in.Content) > 1<<20 {
			return Note{}, invalidInput("content exceeds 1 MiB")
		}
		current.Content = *in.Content
	}
	if in.Starred != nil {
		current.Starred = *in.Starred
	}
	current.Revision++
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE notes SET title = ?, content = ?, search_text = ?, starred = ?, revision = ?, updated_at = ?
		WHERE id = ? AND revision = ? AND deleted_at IS NULL`,
		current.Title, current.Content, normalizeSearch(current.Title+"\n"+current.Content), current.Starred, current.Revision, current.UpdatedAt, id, in.ExpectedRevision)
	if err != nil {
		return Note{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Note{}, err
	}
	if changed != 1 {
		latest, getErr := getNote(ctx, tx, id)
		if getErr != nil {
			return Note{}, getErr
		}
		return Note{}, &ConflictError{Current: latest}
	}
	if err := insertRevision(ctx, tx, current); err != nil {
		return Note{}, err
	}
	if err := tx.Commit(); err != nil {
		return Note{}, err
	}
	return current, nil
}

func (s *Store) DeleteNote(ctx context.Context, id int64) (Note, error) {
	return s.setDeleted(ctx, id, true)
}

func (s *Store) RestoreNote(ctx context.Context, id int64) (Note, error) {
	return s.setDeleted(ctx, id, false)
}

func (s *Store) setDeleted(ctx context.Context, id int64, deleted bool) (Note, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	note, err := getNote(ctx, tx, id)
	if err != nil {
		return Note{}, err
	}
	if (deleted && note.DeletedAt != nil) || (!deleted && note.DeletedAt == nil) {
		return note, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if deleted {
		note.DeletedAt = &now
	} else {
		note.DeletedAt = nil
	}
	note.Revision++
	note.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `UPDATE notes SET deleted_at = ?, revision = ?, updated_at = ? WHERE id = ?`, note.DeletedAt, note.Revision, note.UpdatedAt, note.ID)
	if err != nil {
		return Note{}, err
	}
	if err := insertRevision(ctx, tx, note); err != nil {
		return Note{}, err
	}
	if err := tx.Commit(); err != nil {
		return Note{}, err
	}
	return note, nil
}

func (s *Store) NoteContext(ctx context.Context, id int64, count int) (Context, error) {
	if count <= 0 || count > 10 {
		count = 2
	}
	current, err := s.GetNote(ctx, id)
	if err != nil {
		return Context{}, err
	}
	if current.DeletedAt != nil {
		return Context{}, ErrNotFound
	}
	before, err := s.contextSide(ctx, current, count, true)
	if err != nil {
		return Context{}, err
	}
	after, err := s.contextSide(ctx, current, count, false)
	if err != nil {
		return Context{}, err
	}
	return Context{Before: before, Current: current, After: after}, nil
}

func (s *Store) contextSide(ctx context.Context, current Note, count int, before bool) ([]Note, error) {
	op, order := "<", "DESC"
	if !before {
		op, order = ">", "ASC"
	}
	query := fmt.Sprintf(`SELECT id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at
		FROM notes WHERE deleted_at IS NULL AND (created_at %s ? OR (created_at = ? AND id %s ?))
		ORDER BY created_at %s, id %s LIMIT ?`, op, op, order, order)
	rows, err := s.db.QueryContext(ctx, query, current.CreatedAt, current.CreatedAt, current.ID, count)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := make([]Note, 0, count)
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func getNote(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (Note, error) {
	row := q.QueryRowContext(ctx, `SELECT id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at FROM notes WHERE id = ?`, id)
	note, err := scanNote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	return note, err
}

func getNoteByRequestID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requestID string) (Note, string, error) {
	row := q.QueryRowContext(ctx, `SELECT id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at, request_hash FROM notes WHERE request_id = ?`, requestID)
	var note Note
	var starred int
	var continued sql.NullInt64
	var deleted sql.NullString
	var requestHash sql.NullString
	err := row.Scan(&note.ID, &note.Title, &note.Content, &starred, &continued, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted, &requestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, "", ErrNotFound
	}
	if err != nil {
		return Note{}, "", err
	}
	note.Starred = starred != 0
	if continued.Valid {
		note.ContinuedFromID = &continued.Int64
	}
	if deleted.Valid {
		note.DeletedAt = &deleted.String
	}
	return note, requestHash.String, nil
}

func scanNote(row rowScanner) (Note, error) {
	var note Note
	var starred int
	var continued sql.NullInt64
	var deleted sql.NullString
	err := row.Scan(&note.ID, &note.Title, &note.Content, &starred, &continued, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted)
	if err != nil {
		return Note{}, err
	}
	note.Starred = starred != 0
	if continued.Valid {
		note.ContinuedFromID = &continued.Int64
	}
	if deleted.Valid {
		note.DeletedAt = &deleted.String
	}
	return note, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, note Note) error {
	hash := sha256.Sum256([]byte(note.Content))
	_, err := tx.ExecContext(ctx, `INSERT INTO note_revisions
		(note_id, revision, title, content, content_hash, starred, continued_from_id, deleted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		note.ID, note.Revision, note.Title, note.Content, hex.EncodeToString(hash[:]), note.Starred, note.ContinuedFromID, note.DeletedAt, note.UpdatedAt)
	return err
}

func createRequestHash(in CreateNoteInput) string {
	payload, _ := json.Marshal(struct {
		Title           string `json:"title"`
		Content         string `json:"content"`
		Starred         bool   `json:"starred"`
		ContinuedFromID *int64 `json:"continuedFromId"`
	}{
		Title: in.Title, Content: in.Content, Starred: in.Starred, ContinuedFromID: in.ContinuedFromID,
	})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return err
	}
	hasRequestHash := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "request_hash" {
			hasRequestHash = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasRequestHash {
		if _, err := tx.Exec(`ALTER TABLE notes ADD COLUMN request_hash TEXT`); err != nil {
			return err
		}
	}

	type requestBackfill struct {
		requestID string
		input     CreateNoteInput
	}
	rows, err = tx.Query(`
		SELECT n.request_id, COALESCE(r.title, n.title), COALESCE(r.content, n.content),
			COALESCE(r.starred, n.starred), COALESCE(r.continued_from_id, n.continued_from_id)
		FROM notes n
		LEFT JOIN note_revisions r ON r.note_id = n.id AND r.revision = 1
		WHERE n.request_id IS NOT NULL AND (n.request_hash IS NULL OR n.request_hash = '')`)
	if err != nil {
		return err
	}
	backfills := make([]requestBackfill, 0)
	for rows.Next() {
		var item requestBackfill
		var starred int
		var continued sql.NullInt64
		if err := rows.Scan(&item.requestID, &item.input.Title, &item.input.Content, &starred, &continued); err != nil {
			rows.Close()
			return err
		}
		item.input.Starred = starred != 0
		if continued.Valid {
			item.input.ContinuedFromID = &continued.Int64
		}
		backfills = append(backfills, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range backfills {
		if _, err := tx.Exec(`UPDATE notes SET request_hash = ? WHERE request_id = ?`, createRequestHash(item.input), item.requestID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizeSearch(value string) string {
	return strings.ToLower(norm.NFKC.String(value))
}

func queryTokens(query string) []string {
	normalized := normalizeSearch(query)
	parts := strings.FieldsFunc(normalized, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}
