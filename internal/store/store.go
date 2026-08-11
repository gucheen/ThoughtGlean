package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/go-webauthn/webauthn/webauthn"
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
	ID              string  `json:"id"`
	SyncID          string  `json:"syncId"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	Starred         bool    `json:"starred"`
	ContinuedFromID *string `json:"continuedFromId,omitempty"`
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
	ContinuedFromID *string
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

type NoteSource struct {
	NoteID    string `json:"noteId"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

type Attachment struct {
	ID           string `json:"id"`
	SyncID       string `json:"syncId"`
	NoteID       string `json:"noteId"`
	ContentHash  string `json:"contentHash"`
	OriginalName string `json:"originalName"`
	MIMEType     string `json:"mimeType"`
	ByteSize     int64  `json:"byteSize"`
	CreatedAt    string `json:"createdAt"`
}

// Backup is a complete, portable snapshot of the authoritative note data.
// Search text and request hashes are deliberately omitted: both are derived
// implementation details and can be recreated from the records below.
type Backup struct {
	Format      string          `json:"format"`
	Version     int             `json:"version"`
	GeneratedAt string          `json:"generatedAt"`
	Notes       []Note          `json:"notes"`
	Revisions   []NoteRevision  `json:"revisions"`
	Sources     []NoteSource    `json:"sources,omitempty"`
	Attachments []Attachment    `json:"attachments,omitempty"`
	Requests    []BackupRequest `json:"requests"`
	Integrity   BackupIntegrity `json:"integrity"`
}

// BackupRequest preserves create idempotency across a restore.
type BackupRequest struct {
	NoteID      string `json:"noteId"`
	RequestID   string `json:"requestId"`
	RequestHash string `json:"requestHash"`
}

type NoteRevision struct {
	NoteID          string  `json:"noteId"`
	Revision        int     `json:"revision"`
	Title           string  `json:"title"`
	Content         string  `json:"content"`
	ContentHash     string  `json:"contentHash"`
	Starred         bool    `json:"starred"`
	ContinuedFromID *string `json:"continuedFromId,omitempty"`
	DeletedAt       *string `json:"deletedAt,omitempty"`
	CreatedAt       string  `json:"createdAt"`
}

type BackupIntegrity struct {
	Algorithm string `json:"algorithm"`
	SHA256    string `json:"sha256"`
}

type Store struct {
	db *sql.DB
}

// PasskeyUser is the single local owner used by ThoughtGlean's personal mode.
// It implements webauthn.User without exposing any of its fields to the API.
type PasskeyUser struct {
	ID          []byte
	Name        string
	DisplayName string
	Credentials []webauthn.Credential
}

type PasskeyCredentialInfo struct {
	ID        []byte
	CreatedAt string
	UpdatedAt string
}

func (u PasskeyUser) WebAuthnID() []byte                         { return u.ID }
func (u PasskeyUser) WebAuthnName() string                       { return u.Name }
func (u PasskeyUser) WebAuthnDisplayName() string                { return u.DisplayName }
func (u PasskeyUser) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

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

func (s *Store) HasPasskey(ctx context.Context) (bool, error) {
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM passkey_credentials)`).Scan(&present)
	return present != 0, err
}

// Owner returns the local owner and every one of their registered passkeys.
func (s *Store) Owner(ctx context.Context) (PasskeyUser, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, display_name FROM passkey_users ORDER BY created_at ASC LIMIT 1`)
	var user PasskeyUser
	if err := row.Scan(&user.ID, &user.Name, &user.DisplayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PasskeyUser{}, ErrNotFound
		}
		return PasskeyUser{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT credential_json FROM passkey_credentials WHERE user_id = ? ORDER BY created_at ASC`, user.ID)
	if err != nil {
		return PasskeyUser{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return PasskeyUser{}, err
		}
		var credential webauthn.Credential
		if err := json.Unmarshal(encoded, &credential); err != nil {
			return PasskeyUser{}, fmt.Errorf("decode stored passkey: %w", err)
		}
		user.Credentials = append(user.Credentials, credential)
	}
	return user, rows.Err()
}

func (s *Store) CreateOwner(ctx context.Context, name string) (PasskeyUser, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return PasskeyUser{}, invalidInput("passkey owner name is required and must be at most 100 characters")
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return PasskeyUser{}, fmt.Errorf("generate passkey user id: %w", err)
	}
	user := PasskeyUser{ID: id, Name: name, DisplayName: name}
	_, err := s.db.ExecContext(ctx, `INSERT INTO passkey_users (id, name, display_name, created_at) VALUES (?, ?, ?, ?)`, user.ID, user.Name, user.DisplayName, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return PasskeyUser{}, err
	}
	return user, nil
}

func (s *Store) AddPasskeyCredential(ctx context.Context, user PasskeyUser, credential webauthn.Credential) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO passkey_credentials (id, user_id, credential_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, credential.ID, user.ID, encoded, now, now)
	return err
}

func (s *Store) UpdatePasskeyCredential(ctx context.Context, credential webauthn.Credential) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE passkey_credentials SET credential_json = ?, updated_at = ? WHERE id = ?`, encoded, time.Now().UTC().Format(time.RFC3339Nano), credential.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListPasskeyCredentials(ctx context.Context, user PasskeyUser) ([]PasskeyCredentialInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, updated_at FROM passkey_credentials WHERE user_id = ? ORDER BY created_at ASC`, user.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := make([]PasskeyCredentialInfo, 0)
	for rows.Next() {
		var credential PasskeyCredentialInfo
		if err := rows.Scan(&credential.ID, &credential.CreatedAt, &credential.UpdatedAt); err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	return credentials, rows.Err()
}

// DeletePasskeyCredential refuses to remove the final credential, avoiding an
// accidental permanent lockout of this personal application.
func (s *Store) DeletePasskeyCredential(ctx context.Context, user PasskeyUser, credentialID []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkey_credentials WHERE user_id = ?`, user.ID).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return invalidInput("cannot remove the final passkey")
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM passkey_credentials WHERE id = ? AND user_id = ?`, credentialID, user.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) CreateAuthSession(ctx context.Context, user PasskeyUser) (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate authentication session: %w", err)
	}
	hash := sha256.Sum256(token)
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_sessions (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`, hash[:], user.ID, now.Add(30*24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func (s *Store) ValidateAuthSession(ctx context.Context, token string) (bool, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return false, nil
	}
	hash := sha256.Sum256(raw)
	var expires string
	err = s.db.QueryRowContext(ctx, `SELECT expires_at FROM auth_sessions WHERE token_hash = ?`, hash[:]).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !expiresAt.After(time.Now()) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash = ?`, hash[:])
		return false, nil
	}
	return true, nil
}

func (s *Store) DeleteAuthSession(ctx context.Context, token string) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return nil
	}
	hash := sha256.Sum256(raw)
	_, err = s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash = ?`, hash[:])
	return err
}

func (s *Store) CreateAuthCeremony(ctx context.Context, kind string, session webauthn.SessionData) ([]byte, error) {
	if kind != "registration" && kind != "login" {
		return nil, invalidInput("unknown authentication ceremony")
	}
	if session.Expires.IsZero() || !session.Expires.After(time.Now()) {
		return nil, invalidInput("authentication ceremony has already expired")
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("generate ceremony id: %w", err)
	}
	encoded, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO auth_ceremonies (id, kind, session_json, expires_at) VALUES (?, ?, ?, ?)`, id, kind, encoded, session.Expires.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	return id, nil
}

// TakeAuthCeremony consumes a challenge even if verification later fails.
func (s *Store) TakeAuthCeremony(ctx context.Context, id []byte, kind string) (webauthn.SessionData, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return webauthn.SessionData{}, err
	}
	defer tx.Rollback()
	var encoded []byte
	var expires string
	err = tx.QueryRowContext(ctx, `SELECT session_json, expires_at FROM auth_ceremonies WHERE id = ? AND kind = ?`, id, kind).Scan(&encoded, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return webauthn.SessionData{}, ErrNotFound
	}
	if err != nil {
		return webauthn.SessionData{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_ceremonies WHERE id = ?`, id); err != nil {
		return webauthn.SessionData{}, err
	}
	if err := tx.Commit(); err != nil {
		return webauthn.SessionData{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !parsed.After(time.Now()) {
		return webauthn.SessionData{}, ErrNotFound
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(encoded, &session); err != nil {
		return webauthn.SessionData{}, fmt.Errorf("decode authentication ceremony: %w", err)
	}
	return session, nil
}

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
	id, err := newSyncID()
	if err != nil {
		return Note{}, false, err
	}
	syncID, err := newSyncID()
	if err != nil {
		return Note{}, false, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO notes (id, sync_id, request_id, request_hash, title, content, search_text, starred, continued_from_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, syncID, request, requestHash, in.Title, in.Content, normalizeSearch(in.Title+"\n"+in.Content), in.Starred, in.ContinuedFromID, now, now)
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
	note := Note{ID: id, SyncID: syncID, Title: in.Title, Content: in.Content, Starred: in.Starred, ContinuedFromID: in.ContinuedFromID, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := insertRevision(ctx, tx, note); err != nil {
		return Note{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Note{}, false, err
	}
	return note, false, nil
}

func (s *Store) GetNote(ctx context.Context, id string) (Note, error) {
	return getNote(ctx, s.db, id)
}

func (s *Store) GetNoteBySyncID(ctx context.Context, syncID string) (Note, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at FROM notes WHERE sync_id = ?`, syncID)
	note, err := scanNote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	return note, err
}

func (s *Store) GetNoteSource(ctx context.Context, noteID string) (NoteSource, error) {
	var source NoteSource
	err := s.db.QueryRowContext(ctx, `SELECT note_id, url, title, updated_at FROM note_sources WHERE note_id = ?`, noteID).Scan(&source.NoteID, &source.URL, &source.Title, &source.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NoteSource{}, ErrNotFound
	}
	return source, err
}

func (s *Store) ListNoteSources(ctx context.Context) ([]NoteSource, error) {
	return allSources(ctx, s.db)
}

func (s *Store) SetNoteSource(ctx context.Context, noteID string, source NoteSource) (NoteSource, error) {
	if _, err := s.GetNote(ctx, noteID); err != nil {
		return NoteSource{}, err
	}
	if strings.TrimSpace(source.URL) == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM note_sources WHERE note_id = ?`, noteID); err != nil {
			return NoteSource{}, err
		}
		return NoteSource{}, nil
	}
	parsed, err := url.ParseRequestURI(source.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return NoteSource{}, invalidInput("source URL must be an absolute HTTP(S) URL")
	}
	if len(source.URL) > 4096 || len(source.Title) > 500 {
		return NoteSource{}, invalidInput("source URL or title is too long")
	}
	source.NoteID = noteID
	source.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.ExecContext(ctx, `INSERT INTO note_sources (note_id, url, title, search_text, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(note_id) DO UPDATE SET url = excluded.url, title = excluded.title, search_text = excluded.search_text, updated_at = excluded.updated_at`, source.NoteID, source.URL, source.Title, normalizeSearch(source.Title+"\n"+source.URL), source.UpdatedAt)
	if err != nil {
		return NoteSource{}, err
	}
	return source, nil
}

func (s *Store) AddAttachment(ctx context.Context, attachment Attachment) (Attachment, error) {
	if _, err := s.GetNote(ctx, attachment.NoteID); err != nil {
		return Attachment{}, err
	}
	attachment.OriginalName = strings.TrimSpace(attachment.OriginalName)
	if attachment.OriginalName == "" || len(attachment.OriginalName) > 255 || attachment.ByteSize <= 0 {
		return Attachment{}, invalidInput("invalid attachment metadata")
	}
	attachment.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if attachment.SyncID == "" {
		syncID, err := newSyncID()
		if err != nil {
			return Attachment{}, err
		}
		attachment.SyncID = syncID
	} else if !validSyncID(attachment.SyncID) {
		return Attachment{}, invalidInput("invalid attachment sync id")
	}
	var err error
	attachment.ID, err = newSyncID()
	if err != nil {
		return Attachment{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO note_attachments (id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, attachment.ID, attachment.SyncID, attachment.NoteID, attachment.ContentHash, attachment.OriginalName, attachment.MIMEType, attachment.ByteSize, attachment.CreatedAt)
	if err != nil {
		return Attachment{}, err
	}
	return attachment, err
}

func (s *Store) ListAttachments(ctx context.Context, noteID string) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at FROM note_attachments WHERE note_id = ? ORDER BY id ASC`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]Attachment, 0)
	for rows.Next() {
		var attachment Attachment
		if err := rows.Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func (s *Store) ListAllAttachments(ctx context.Context) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at FROM note_attachments ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]Attachment, 0)
	for rows.Next() {
		var attachment Attachment
		if err := rows.Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func (s *Store) GetAttachment(ctx context.Context, id string) (Attachment, error) {
	var attachment Attachment
	err := s.db.QueryRowContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at FROM note_attachments WHERE id = ?`, id).Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return attachment, err
}

func (s *Store) GetAttachmentBySyncID(ctx context.Context, syncID string) (Attachment, error) {
	var attachment Attachment
	err := s.db.QueryRowContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at FROM note_attachments WHERE sync_id = ?`, syncID).Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return attachment, err
}

func (s *Store) DeleteAttachment(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM note_attachments WHERE id = ?`, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAttachmentBySyncID(ctx context.Context, syncID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM note_attachments WHERE sync_id = ?`, syncID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AttachmentReferenceCount(ctx context.Context, contentHash string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_attachments WHERE content_hash = ?`, contentHash).Scan(&count)
	return count, err
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
		where = append(where, `(search_text LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM note_sources ns WHERE ns.note_id = notes.id AND ns.search_text LIKE ? ESCAPE '\'))`)
		pattern := "%" + escapeLike(token) + "%"
		args = append(args, pattern, pattern)
	}
	args = append(args, limit)
	query := `SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at
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

// ActiveNotes returns every non-deleted note for durable projections such as
// the Markdown mirror. Unlike ListNotes, it has no presentation limit.
func (s *Store) ActiveNotes(ctx context.Context) ([]Note, error) {
	notes, err := allNotes(ctx, s.db)
	if err != nil {
		return nil, err
	}
	active := make([]Note, 0, len(notes))
	for _, note := range notes {
		if note.DeletedAt == nil {
			active = append(active, note)
		}
	}
	return active, nil
}

// BackupData returns every note, including trashed records, and every saved
// revision. Its integrity digest covers all fields except the digest itself.
func (s *Store) BackupData(ctx context.Context) (Backup, error) {
	// Keep notes and revisions in one SQLite read transaction: a concurrent
	// edit must appear either wholly before or wholly after this snapshot.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Backup{}, err
	}
	defer tx.Rollback()
	notes, err := allNotes(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	revisions, err := allRevisions(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	backup := Backup{
		Format:      "thoughtglean-backup",
		Version:     2,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Notes:       notes,
		Revisions:   revisions,
	}
	backup.Requests, err = backupRequests(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	backup.Sources, err = allSources(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	if err := sealBackup(&backup); err != nil {
		return Backup{}, err
	}
	if err := tx.Commit(); err != nil {
		return Backup{}, err
	}
	return backup, nil
}

// VerifyBackup verifies both the format contract and the snapshot digest.
func VerifyBackup(backup Backup) error {
	if backup.Format != "thoughtglean-backup" || backup.Version != 2 {
		return errors.New("unsupported backup format")
	}
	if backup.Integrity.Algorithm != "sha256" || len(backup.Integrity.SHA256) != sha256.Size*2 {
		return errors.New("backup is missing a SHA-256 integrity value")
	}
	expected := backup.Integrity.SHA256
	if err := sealBackup(&backup); err != nil {
		return err
	}
	if backup.Integrity.SHA256 != expected {
		return errors.New("backup integrity check failed")
	}
	for _, revision := range backup.Revisions {
		hash := sha256.Sum256([]byte(revision.Content))
		if revision.ContentHash != hex.EncodeToString(hash[:]) {
			return fmt.Errorf("content hash mismatch for note %s revision %d", revision.NoteID, revision.Revision)
		}
	}
	return validateBackupStructure(backup)
}

func sealBackup(backup *Backup) error {
	backup.Integrity = BackupIntegrity{}
	payload, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	backup.Integrity = BackupIntegrity{Algorithm: "sha256", SHA256: hex.EncodeToString(hash[:])}
	return nil
}

func SealBackup(backup *Backup) error { return sealBackup(backup) }

func validateBackupStructure(backup Backup) error {
	if _, err := time.Parse(time.RFC3339Nano, backup.GeneratedAt); err != nil {
		return errors.New("backup has an invalid generation time")
	}
	notes := make(map[string]Note, len(backup.Notes))
	for _, note := range backup.Notes {
		if note.ID == "" || note.Revision <= 0 || strings.TrimSpace(note.Content) == "" {
			return fmt.Errorf("backup has an invalid note %s", note.ID)
		}
		if _, exists := notes[note.ID]; exists {
			return fmt.Errorf("backup has duplicate note id %s", note.ID)
		}
		notes[note.ID] = note
	}
	revisions := make(map[string]map[int]NoteRevision, len(notes))
	for _, revision := range backup.Revisions {
		if _, exists := notes[revision.NoteID]; !exists || revision.Revision <= 0 {
			return fmt.Errorf("backup has an invalid revision for note %s", revision.NoteID)
		}
		byNumber := revisions[revision.NoteID]
		if byNumber == nil {
			byNumber = make(map[int]NoteRevision)
			revisions[revision.NoteID] = byNumber
		}
		if _, exists := byNumber[revision.Revision]; exists {
			return fmt.Errorf("backup has duplicate revision %d for note %s", revision.Revision, revision.NoteID)
		}
		byNumber[revision.Revision] = revision
	}
	for id, note := range notes {
		if note.ContinuedFromID != nil {
			if *note.ContinuedFromID == id {
				return fmt.Errorf("note %s cannot continue itself", id)
			}
			if _, exists := notes[*note.ContinuedFromID]; !exists {
				return fmt.Errorf("note %s references a missing continued note", id)
			}
		}
		byNumber := revisions[id]
		if len(byNumber) != note.Revision {
			return fmt.Errorf("note %s has incomplete revision history", id)
		}
		for number := 1; number <= note.Revision; number++ {
			if _, exists := byNumber[number]; !exists {
				return fmt.Errorf("note %s is missing revision %d", id, number)
			}
		}
		latest := byNumber[note.Revision]
		if latest.Title != note.Title || latest.Content != note.Content || latest.Starred != note.Starred ||
			!sameStringPointer(latest.ContinuedFromID, note.ContinuedFromID) || !sameStringPointer(latest.DeletedAt, note.DeletedAt) || latest.CreatedAt != note.UpdatedAt {
			return fmt.Errorf("note %s does not match its latest revision", id)
		}
	}
	requestIDs := make(map[string]string, len(backup.Requests))
	for _, request := range backup.Requests {
		if _, exists := notes[request.NoteID]; !exists || request.RequestID == "" || request.RequestHash == "" {
			return fmt.Errorf("backup has an invalid create request for note %s", request.NoteID)
		}
		if existing, exists := requestIDs[request.RequestID]; exists && existing != request.NoteID {
			return fmt.Errorf("backup reuses request id for notes %s and %s", existing, request.NoteID)
		}
		requestIDs[request.RequestID] = request.NoteID
	}
	return nil
}

func sameStringPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// RestoreBackup replaces all current data with a verified backup atomically.
// Callers should require an explicit user confirmation before invoking it.
func (s *Store) RestoreBackup(ctx context.Context, backup Backup) error {
	if err := VerifyBackup(backup); err != nil {
		return err
	}
	requests := make(map[string]BackupRequest, len(backup.Requests))
	for _, request := range backup.Requests {
		requests[request.NoteID] = request
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM notes`); err != nil {
		return err
	}
	for _, note := range backup.Notes {
		request := requests[note.ID]
		var requestID, requestHash any
		if request.RequestID != "" {
			requestID, requestHash = request.RequestID, request.RequestHash
		}
		if note.SyncID == "" {
			note.SyncID, err = newSyncID()
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes (id, sync_id, request_id, request_hash, title, content, search_text, starred, continued_from_id, revision, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)`, note.ID, note.SyncID, requestID, requestHash, note.Title, note.Content,
			normalizeSearch(note.Title+"\n"+note.Content), note.Starred, note.Revision, note.CreatedAt, note.UpdatedAt, note.DeletedAt); err != nil {
			return err
		}
	}
	for _, note := range backup.Notes {
		if note.ContinuedFromID == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET continued_from_id = ? WHERE id = ?`, *note.ContinuedFromID, note.ID); err != nil {
			return err
		}
	}
	for _, revision := range backup.Revisions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_revisions (note_id, revision, title, content, content_hash, starred, continued_from_id, deleted_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, revision.NoteID, revision.Revision, revision.Title, revision.Content, revision.ContentHash,
			revision.Starred, revision.ContinuedFromID, revision.DeletedAt, revision.CreatedAt); err != nil {
			return err
		}
	}
	for _, attachment := range backup.Attachments {
		if attachment.ID == "" {
			attachment.ID, err = newSyncID()
			if err != nil {
				return err
			}
		}
		if attachment.SyncID == "" {
			attachment.SyncID, err = newSyncID()
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_attachments (id, sync_id, note_id, content_hash, original_name, mime_type, byte_size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, attachment.ID, attachment.SyncID, attachment.NoteID, attachment.ContentHash, attachment.OriginalName, attachment.MIMEType, attachment.ByteSize, attachment.CreatedAt); err != nil {
			return err
		}
	}
	for _, source := range backup.Sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_sources (note_id, url, title, search_text, updated_at) VALUES (?, ?, ?, ?, ?)`, source.NoteID, source.URL, source.Title, normalizeSearch(source.Title+"\n"+source.URL), source.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func allNotes(ctx context.Context, q queryer) ([]Note, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at
		FROM notes ORDER BY id ASC`)
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

func allRevisions(ctx context.Context, q queryer) ([]NoteRevision, error) {
	rows, err := q.QueryContext(ctx, `SELECT note_id, revision, title, content, content_hash, starred, continued_from_id, deleted_at, created_at
		FROM note_revisions ORDER BY note_id ASC, revision ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := make([]NoteRevision, 0)
	for rows.Next() {
		var revision NoteRevision
		var starred int
		var continued sql.NullString
		var deleted sql.NullString
		if err := rows.Scan(&revision.NoteID, &revision.Revision, &revision.Title, &revision.Content, &revision.ContentHash,
			&starred, &continued, &deleted, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revision.Starred = starred != 0
		if continued.Valid {
			revision.ContinuedFromID = &continued.String
		}
		if deleted.Valid {
			revision.DeletedAt = &deleted.String
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func allSources(ctx context.Context, q queryer) ([]NoteSource, error) {
	rows, err := q.QueryContext(ctx, `SELECT note_id, url, title, updated_at FROM note_sources ORDER BY note_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := make([]NoteSource, 0)
	for rows.Next() {
		var source NoteSource
		if err := rows.Scan(&source.NoteID, &source.URL, &source.Title, &source.UpdatedAt); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func backupRequests(ctx context.Context, q queryer) ([]BackupRequest, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, request_id, request_hash FROM notes WHERE request_id IS NOT NULL ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := make([]BackupRequest, 0)
	for rows.Next() {
		var request BackupRequest
		var hash sql.NullString
		if err := rows.Scan(&request.NoteID, &request.RequestID, &hash); err != nil {
			return nil, err
		}
		request.RequestHash = hash.String
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// MarkdownExport produces a readable, dependency-free copy of non-deleted
// records. The JSON backup remains the lossless archival format.
func (s *Store) MarkdownExport(ctx context.Context) ([]byte, error) {
	notes, err := allNotes(ctx, s.db)
	if err != nil {
		return nil, err
	}
	sources, err := s.ListNoteSources(ctx)
	if err != nil {
		return nil, err
	}
	sourceByNote := make(map[string]NoteSource, len(sources))
	for _, source := range sources {
		sourceByNote[source.NoteID] = source
	}
	var out strings.Builder
	out.WriteString("# ThoughtGlean export\n\n")
	out.WriteString("This file contains active records exported from ThoughtGlean. For complete history and deleted records, keep the accompanying JSON backup.\n")
	for _, note := range notes {
		if note.DeletedAt != nil {
			continue
		}
		out.WriteString("\n---\n\n")
		if note.Title != "" {
			out.WriteString("## ")
			out.WriteString(note.Title)
			out.WriteString("\n\n")
		} else {
			out.WriteString("## Untitled note\n\n")
		}
		fmt.Fprintf(&out, "<!-- thoughtglean:id=%s revision=%d createdAt=%s updatedAt=%s -->\n\n", note.ID, note.Revision, note.CreatedAt, note.UpdatedAt)
		if source, exists := sourceByNote[note.ID]; exists {
			title := source.Title
			if title == "" {
				title = source.URL
			}
			fmt.Fprintf(&out, "来源：[%s](%s)\n\n", title, source.URL)
		}
		out.WriteString(note.Content)
		out.WriteString("\n")
	}
	return []byte(out.String()), nil
}

func (s *Store) UpdateNote(ctx context.Context, id string, in UpdateNoteInput) (Note, error) {
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

func (s *Store) DeleteNote(ctx context.Context, id string) (Note, error) {
	return s.setDeleted(ctx, id, true)
}

func (s *Store) RestoreNote(ctx context.Context, id string) (Note, error) {
	return s.setDeleted(ctx, id, false)
}

func (s *Store) setDeleted(ctx context.Context, id string, deleted bool) (Note, error) {
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

func (s *Store) NoteContext(ctx context.Context, id string, count int) (Context, error) {
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
	query := fmt.Sprintf(`SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at
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
}, id string) (Note, error) {
	row := q.QueryRowContext(ctx, `SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at FROM notes WHERE id = ?`, id)
	note, err := scanNote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	return note, err
}

func getNoteByRequestID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requestID string) (Note, string, error) {
	row := q.QueryRowContext(ctx, `SELECT id, sync_id, title, content, starred, continued_from_id, revision, created_at, updated_at, deleted_at, request_hash FROM notes WHERE request_id = ?`, requestID)
	var note Note
	var starred int
	var continued sql.NullString
	var deleted sql.NullString
	var requestHash sql.NullString
	err := row.Scan(&note.ID, &note.SyncID, &note.Title, &note.Content, &starred, &continued, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted, &requestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, "", ErrNotFound
	}
	if err != nil {
		return Note{}, "", err
	}
	note.Starred = starred != 0
	if continued.Valid {
		note.ContinuedFromID = &continued.String
	}
	if deleted.Valid {
		note.DeletedAt = &deleted.String
	}
	return note, requestHash.String, nil
}

func scanNote(row rowScanner) (Note, error) {
	var note Note
	var starred int
	var continued sql.NullString
	var deleted sql.NullString
	err := row.Scan(&note.ID, &note.SyncID, &note.Title, &note.Content, &starred, &continued, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted)
	if err != nil {
		return Note{}, err
	}
	note.Starred = starred != 0
	if continued.Valid {
		note.ContinuedFromID = &continued.String
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
		Title           string  `json:"title"`
		Content         string  `json:"content"`
		Starred         bool    `json:"starred"`
		ContinuedFromID *string `json:"continuedFromId"`
	}{
		Title: in.Title, Content: in.Content, Starred: in.Starred, ContinuedFromID: in.ContinuedFromID,
	})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

// newSyncID is stable across backup/restore and can be safely exposed to
// other devices. It is deliberately independent from SQLite's local integer
// primary key.
func newSyncID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate sync id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validSyncID(id string) bool {
	if len(id) < 22 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
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
	hasSyncID := false
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
		if name == "sync_id" {
			hasSyncID = true
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
	if !hasSyncID {
		if _, err := tx.Exec(`ALTER TABLE notes ADD COLUMN sync_id TEXT`); err != nil {
			return err
		}
	}
	rows, err = tx.Query(`SELECT id FROM notes WHERE sync_id IS NULL OR sync_id = ''`)
	if err != nil {
		return err
	}
	missingSyncIDs := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		missingSyncIDs = append(missingSyncIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range missingSyncIDs {
		syncID, err := newSyncID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE notes SET sync_id = ? WHERE id = ?`, syncID, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_notes_sync_id ON notes(sync_id)`); err != nil {
		return err
	}

	rows, err = tx.Query(`PRAGMA table_info(note_sources)`)
	if err != nil {
		return err
	}
	hasSourceSearch := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "search_text" {
			hasSourceSearch = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasSourceSearch {
		if _, err := tx.Exec(`ALTER TABLE note_sources ADD COLUMN search_text TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE note_sources SET search_text = lower(title || char(10) || url) WHERE search_text = ''`); err != nil {
		return err
	}

	rows, err = tx.Query(`PRAGMA table_info(note_attachments)`)
	if err != nil {
		return err
	}
	hasAttachmentSyncID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "sync_id" {
			hasAttachmentSyncID = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasAttachmentSyncID {
		if _, err := tx.Exec(`ALTER TABLE note_attachments ADD COLUMN sync_id TEXT`); err != nil {
			return err
		}
	}
	rows, err = tx.Query(`SELECT id FROM note_attachments WHERE sync_id IS NULL OR sync_id = ''`)
	if err != nil {
		return err
	}
	attachmentIDs := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		attachmentIDs = append(attachmentIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range attachmentIDs {
		syncID, err := newSyncID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE note_attachments SET sync_id=? WHERE id=?`, syncID, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_note_attachments_sync_id ON note_attachments(sync_id)`); err != nil {
		return err
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
		var continued sql.NullString
		if err := rows.Scan(&item.requestID, &item.input.Title, &item.input.Content, &starred, &continued); err != nil {
			rows.Close()
			return err
		}
		item.input.Starred = starred != 0
		if continued.Valid {
			item.input.ContinuedFromID = &continued.String
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
