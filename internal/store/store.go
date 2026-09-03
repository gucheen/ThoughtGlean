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
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
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
	Kind            string  `json:"kind"`
	Starred         bool    `json:"starred"`
	ContinuedFromID *string `json:"continuedFromId,omitempty"`
	DerivedFromID   *string `json:"derivedFromId,omitempty"`
	Revision        int     `json:"revision"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
	DeletedAt       *string `json:"deletedAt,omitempty"`
}

type CreateNoteInput struct {
	RequestID       string
	Title           string
	Content         string
	Kind            string
	Starred         bool
	ContinuedFromID *string
	DerivedFromID   *string
}

type UpdateNoteInput struct {
	Title            *string
	Content          *string
	Kind             *string
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

type NoteMaterialLink struct {
	ID         string `json:"id"`
	SyncID     string `json:"syncId"`
	NoteID     string `json:"noteId"`
	MaterialID string `json:"materialId"`
	CreatedAt  string `json:"createdAt"`
}

type NoteVerification struct {
	ID           string `json:"id"`
	SyncID       string `json:"syncId"`
	NoteID       string `json:"noteId"`
	NoteRevision int    `json:"noteRevision"`
	VerifiedAt   string `json:"verifiedAt"`
	Environment  string `json:"environment"`
	Result       string `json:"result"`
	Comment      string `json:"comment"`
}

type Topic struct {
	ID        string  `json:"id"`
	SyncID    string  `json:"syncId"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
	DeletedAt *string `json:"deletedAt,omitempty"`
}

type TopicMembership struct {
	ID        string  `json:"id"`
	SyncID    string  `json:"syncId"`
	TopicID   string  `json:"topicId"`
	NoteID    string  `json:"noteId"`
	Pinned    bool    `json:"pinned"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
	DeletedAt *string `json:"deletedAt,omitempty"`
}

type Attachment struct {
	ID           string `json:"id"`
	SyncID       string `json:"syncId"`
	NoteID       string `json:"noteId"`
	ContentHash  string `json:"contentHash"`
	OriginalName string `json:"originalName"`
	AltText      string `json:"altText,omitempty"`
	MIMEType     string `json:"mimeType"`
	ByteSize     int64  `json:"byteSize"`
	CreatedAt    string `json:"createdAt"`
}

// Backup is a complete, portable snapshot of the authoritative note data.
// Search text and request hashes are deliberately omitted: both are derived
// implementation details and can be recreated from the records below.
type Backup struct {
	Format           string             `json:"format"`
	Version          int                `json:"version"`
	GeneratedAt      string             `json:"generatedAt"`
	Notes            []Note             `json:"notes"`
	Revisions        []NoteRevision     `json:"revisions"`
	Sources          []NoteSource       `json:"sources,omitempty"`
	MaterialLinks    []NoteMaterialLink `json:"materialLinks,omitempty"`
	Verifications    []NoteVerification `json:"verifications,omitempty"`
	Topics           []Topic            `json:"topics,omitempty"`
	TopicMemberships []TopicMembership  `json:"topicMemberships,omitempty"`
	Attachments      []Attachment       `json:"attachments,omitempty"`
	Requests         []BackupRequest    `json:"requests"`
	Integrity        BackupIntegrity    `json:"integrity"`
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
	Kind            string  `json:"kind"`
	Starred         bool    `json:"starred"`
	ContinuedFromID *string `json:"continuedFromId,omitempty"`
	DerivedFromID   *string `json:"derivedFromId,omitempty"`
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
	parameters := url.Values{}
	parameters.Add("_pragma", "busy_timeout(5000)")
	parameters.Add("_pragma", "foreign_keys(ON)")
	parameters.Add("_pragma", "journal_mode(WAL)")
	parameters.Add("_pragma", "synchronous(FULL)")
	dsn := path + "?" + parameters.Encode()
	db, err := sql.Open("sqlite", dsn)
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
	in.Kind = normalizedNoteKind(in.Kind)
	if !validNoteKind(in.Kind) {
		return Note{}, false, invalidInput("unknown note kind")
	}
	if in.DerivedFromID != nil && in.Kind != "procedure" {
		return Note{}, false, invalidInput("only procedure notes can reference source material")
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
	if in.DerivedFromID != nil {
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM notes WHERE id = ? AND deleted_at IS NULL`, *in.DerivedFromID).Scan(&kind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Note{}, false, invalidInput("source material does not exist")
			}
			return Note{}, false, err
		}
		if kind != "material" {
			return Note{}, false, invalidInput("derived note must reference source material")
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
		INSERT INTO notes (id, sync_id, request_id, request_hash, title, content, search_text, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, syncID, request, requestHash, in.Title, in.Content, normalizeSearch(in.Title+"\n"+in.Content), in.Kind, in.Starred, in.ContinuedFromID, in.DerivedFromID, now, now)
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
	note := Note{ID: id, SyncID: syncID, Title: in.Title, Content: in.Content, Kind: in.Kind, Starred: in.Starred, ContinuedFromID: in.ContinuedFromID, DerivedFromID: in.DerivedFromID, Revision: 1, CreatedAt: now, UpdatedAt: now}
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
	row := s.db.QueryRowContext(ctx, `SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at FROM notes WHERE sync_id = ?`, syncID)
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

func (s *Store) ListMaterialLinks(ctx context.Context) ([]NoteMaterialLink, error) {
	return allMaterialLinks(ctx, s.db)
}

func (s *Store) ListVerifications(ctx context.Context) ([]NoteVerification, error) {
	return allVerifications(ctx, s.db)
}

func (s *Store) ListTopics(ctx context.Context) ([]Topic, error) {
	return allTopics(ctx, s.db)
}

func (s *Store) ListTopicMemberships(ctx context.Context) ([]TopicMembership, error) {
	return allTopicMemberships(ctx, s.db)
}

func (s *Store) ApplyRemoteTopic(ctx context.Context, incoming Topic) (Topic, error) {
	incoming.Name = strings.TrimSpace(incoming.Name)
	if !validSyncID(incoming.SyncID) || incoming.Name == "" || len(incoming.Name) > 80 {
		return Topic{}, invalidInput("invalid topic")
	}
	if _, err := time.Parse(time.RFC3339Nano, incoming.CreatedAt); err != nil {
		return Topic{}, invalidInput("invalid topic creation time")
	}
	incomingUpdated, err := time.Parse(time.RFC3339Nano, incoming.UpdatedAt)
	if err != nil {
		return Topic{}, invalidInput("invalid topic update time")
	}
	if incoming.DeletedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *incoming.DeletedAt); err != nil {
			return Topic{}, invalidInput("invalid topic deletion time")
		}
	}
	var existing Topic
	err = s.db.QueryRowContext(ctx, `SELECT id, sync_id, name, created_at, updated_at, deleted_at FROM topics WHERE sync_id = ?`, incoming.SyncID).
		Scan(&existing.ID, &existing.SyncID, &existing.Name, &existing.CreatedAt, &existing.UpdatedAt, &existing.DeletedAt)
	if err == nil {
		existingUpdated, parseErr := time.Parse(time.RFC3339Nano, existing.UpdatedAt)
		if parseErr == nil && incomingUpdated.Before(existingUpdated) {
			return existing, nil
		}
		_, err = s.db.ExecContext(ctx, `UPDATE topics SET name = ?, updated_at = ?, deleted_at = ? WHERE id = ?`, incoming.Name, incoming.UpdatedAt, incoming.DeletedAt, existing.ID)
		incoming.ID = existing.ID
		return incoming, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Topic{}, err
	}
	incoming.ID, err = newSyncID()
	if err != nil {
		return Topic{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO topics (id, sync_id, name, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?)`, incoming.ID, incoming.SyncID, incoming.Name, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
	return incoming, err
}

func (s *Store) ApplyRemoteTopicMembership(ctx context.Context, incoming TopicMembership, topicSyncID, noteSyncID string) (TopicMembership, error) {
	if !validSyncID(incoming.SyncID) || !validSyncID(topicSyncID) || !validSyncID(noteSyncID) {
		return TopicMembership{}, invalidInput("invalid topic membership")
	}
	if _, err := time.Parse(time.RFC3339Nano, incoming.CreatedAt); err != nil {
		return TopicMembership{}, invalidInput("invalid topic membership creation time")
	}
	incomingUpdated, err := time.Parse(time.RFC3339Nano, incoming.UpdatedAt)
	if err != nil {
		return TopicMembership{}, invalidInput("invalid topic membership update time")
	}
	if incoming.DeletedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *incoming.DeletedAt); err != nil {
			return TopicMembership{}, invalidInput("invalid topic membership deletion time")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TopicMembership{}, err
	}
	defer tx.Rollback()
	var topicID, noteID, noteKind string
	var topicDeletedAt, noteDeletedAt *string
	if err := tx.QueryRowContext(ctx, `SELECT id, deleted_at FROM topics WHERE sync_id = ?`, topicSyncID).Scan(&topicID, &topicDeletedAt); err != nil {
		return TopicMembership{}, invalidInput("topic does not exist")
	}
	if err := tx.QueryRowContext(ctx, `SELECT id, kind, deleted_at FROM notes WHERE sync_id = ?`, noteSyncID).Scan(&noteID, &noteKind, &noteDeletedAt); err != nil {
		return TopicMembership{}, invalidInput("topic record does not exist")
	}
	if incoming.DeletedAt == nil && (topicDeletedAt != nil || noteDeletedAt != nil) {
		return TopicMembership{}, invalidInput("topic or record is deleted")
	}
	if incoming.DeletedAt == nil && incoming.Pinned && noteKind != "procedure" {
		return TopicMembership{}, invalidInput("only procedure notes can be pinned in a topic")
	}
	var existing TopicMembership
	err = tx.QueryRowContext(ctx, `SELECT id, sync_id, topic_id, note_id, pinned, created_at, updated_at, deleted_at FROM topic_memberships WHERE sync_id = ? OR (topic_id = ? AND note_id = ?) ORDER BY CASE WHEN sync_id = ? THEN 0 ELSE 1 END LIMIT 1`, incoming.SyncID, topicID, noteID, incoming.SyncID).
		Scan(&existing.ID, &existing.SyncID, &existing.TopicID, &existing.NoteID, &existing.Pinned, &existing.CreatedAt, &existing.UpdatedAt, &existing.DeletedAt)
	if err == nil {
		if existing.TopicID != topicID || existing.NoteID != noteID {
			return TopicMembership{}, invalidInput("topic membership sync id is already in use")
		}
		existingUpdated, parseErr := time.Parse(time.RFC3339Nano, existing.UpdatedAt)
		if parseErr == nil && incomingUpdated.Before(existingUpdated) {
			return existing, tx.Commit()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE topic_memberships SET pinned = ?, updated_at = ?, deleted_at = ? WHERE id = ?`, incoming.Pinned, incoming.UpdatedAt, incoming.DeletedAt, existing.ID); err != nil {
			return TopicMembership{}, err
		}
		incoming.ID, incoming.SyncID, incoming.TopicID, incoming.NoteID = existing.ID, existing.SyncID, topicID, noteID
		return incoming, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TopicMembership{}, err
	}
	incoming.ID, err = newSyncID()
	if err != nil {
		return TopicMembership{}, err
	}
	incoming.TopicID, incoming.NoteID = topicID, noteID
	if _, err := tx.ExecContext(ctx, `INSERT INTO topic_memberships (id, sync_id, topic_id, note_id, pinned, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, incoming.ID, incoming.SyncID, topicID, noteID, incoming.Pinned, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt); err != nil {
		return TopicMembership{}, err
	}
	return incoming, tx.Commit()
}

func (s *Store) ApplyRemoteMaterialLink(ctx context.Context, link NoteMaterialLink, noteSyncID, materialSyncID string) (NoteMaterialLink, error) {
	if !validSyncID(link.SyncID) || !validSyncID(noteSyncID) || !validSyncID(materialSyncID) || noteSyncID == materialSyncID {
		return NoteMaterialLink{}, invalidInput("invalid material link")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NoteMaterialLink{}, err
	}
	defer tx.Rollback()
	var noteID, noteKind, materialID, materialKind string
	if err := tx.QueryRowContext(ctx, `SELECT id, kind FROM notes WHERE sync_id = ?`, noteSyncID).Scan(&noteID, &noteKind); err != nil {
		return NoteMaterialLink{}, invalidInput("linked procedure does not exist")
	}
	if err := tx.QueryRowContext(ctx, `SELECT id, kind FROM notes WHERE sync_id = ?`, materialSyncID).Scan(&materialID, &materialKind); err != nil {
		return NoteMaterialLink{}, invalidInput("linked source material does not exist")
	}
	if noteKind != "procedure" || materialKind != "material" {
		return NoteMaterialLink{}, invalidInput("material links require a procedure and source material")
	}
	if link.CreatedAt == "" {
		link.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, err := time.Parse(time.RFC3339Nano, link.CreatedAt); err != nil {
		return NoteMaterialLink{}, invalidInput("invalid material link time")
	}
	var existing NoteMaterialLink
	err = tx.QueryRowContext(ctx, `SELECT id, sync_id, note_id, material_id, created_at FROM note_material_links WHERE sync_id = ?`, link.SyncID).
		Scan(&existing.ID, &existing.SyncID, &existing.NoteID, &existing.MaterialID, &existing.CreatedAt)
	if err == nil {
		if existing.NoteID != noteID || existing.MaterialID != materialID {
			return NoteMaterialLink{}, invalidInput("material link sync id is already in use")
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NoteMaterialLink{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id, sync_id, note_id, material_id, created_at FROM note_material_links WHERE note_id = ? AND material_id = ?`, noteID, materialID).
		Scan(&existing.ID, &existing.SyncID, &existing.NoteID, &existing.MaterialID, &existing.CreatedAt)
	if err == nil {
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NoteMaterialLink{}, err
	}
	link.ID, err = newSyncID()
	if err != nil {
		return NoteMaterialLink{}, err
	}
	link.NoteID, link.MaterialID = noteID, materialID
	if _, err := tx.ExecContext(ctx, `INSERT INTO note_material_links (id, sync_id, note_id, material_id, created_at) VALUES (?, ?, ?, ?, ?)`, link.ID, link.SyncID, noteID, materialID, link.CreatedAt); err != nil {
		return NoteMaterialLink{}, err
	}
	if err := tx.Commit(); err != nil {
		return NoteMaterialLink{}, err
	}
	return link, nil
}

func (s *Store) ApplyRemoteVerification(ctx context.Context, verification NoteVerification, noteSyncID string) (NoteVerification, error) {
	if !validSyncID(verification.SyncID) || !validSyncID(noteSyncID) || !validVerificationResult(verification.Result) {
		return NoteVerification{}, invalidInput("invalid verification")
	}
	verification.Environment = strings.TrimSpace(verification.Environment)
	verification.Comment = strings.TrimSpace(verification.Comment)
	if verification.Environment == "" || len(verification.Environment) > 500 || len(verification.Comment) > 4000 {
		return NoteVerification{}, invalidInput("invalid verification details")
	}
	if _, err := time.Parse(time.RFC3339Nano, verification.VerifiedAt); err != nil {
		return NoteVerification{}, invalidInput("invalid verification time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NoteVerification{}, err
	}
	defer tx.Rollback()
	var noteID, kind string
	var revision int
	if err := tx.QueryRowContext(ctx, `SELECT id, kind, revision FROM notes WHERE sync_id = ?`, noteSyncID).Scan(&noteID, &kind, &revision); err != nil {
		return NoteVerification{}, invalidInput("verified note does not exist")
	}
	if kind != "procedure" || verification.NoteRevision <= 0 || verification.NoteRevision > revision {
		return NoteVerification{}, invalidInput("verification must reference an existing procedure revision")
	}
	var existing NoteVerification
	err = tx.QueryRowContext(ctx, `SELECT id, sync_id, note_id, note_revision, verified_at, environment, result, comment FROM note_verifications WHERE sync_id = ?`, verification.SyncID).
		Scan(&existing.ID, &existing.SyncID, &existing.NoteID, &existing.NoteRevision, &existing.VerifiedAt, &existing.Environment, &existing.Result, &existing.Comment)
	if err == nil {
		if existing.NoteID != noteID {
			return NoteVerification{}, invalidInput("verification sync id is already in use")
		}
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return NoteVerification{}, err
	}
	verification.ID, err = newSyncID()
	if err != nil {
		return NoteVerification{}, err
	}
	verification.NoteID = noteID
	if _, err := tx.ExecContext(ctx, `INSERT INTO note_verifications (id, sync_id, note_id, note_revision, verified_at, environment, result, comment) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, verification.ID, verification.SyncID, noteID, verification.NoteRevision, verification.VerifiedAt, verification.Environment, verification.Result, verification.Comment); err != nil {
		return NoteVerification{}, err
	}
	if err := tx.Commit(); err != nil {
		return NoteVerification{}, err
	}
	return verification, nil
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
	attachment.AltText = strings.TrimSpace(attachment.AltText)
	if len(attachment.AltText) > 500 {
		return Attachment{}, invalidInput("image description is too long")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO note_attachments (id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, attachment.ID, attachment.SyncID, attachment.NoteID, attachment.ContentHash, attachment.OriginalName, attachment.AltText, attachment.MIMEType, attachment.ByteSize, attachment.CreatedAt)
	if err != nil {
		return Attachment{}, err
	}
	return attachment, err
}

func (s *Store) ListAttachments(ctx context.Context, noteID string) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at FROM note_attachments WHERE note_id = ? ORDER BY id ASC`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]Attachment, 0)
	for rows.Next() {
		var attachment Attachment
		if err := rows.Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.AltText, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func (s *Store) ListAllAttachments(ctx context.Context) ([]Attachment, error) {
	return allAttachments(ctx, s.db)
}

func allAttachments(ctx context.Context, q queryer) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at FROM note_attachments ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]Attachment, 0)
	for rows.Next() {
		var attachment Attachment
		if err := rows.Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.AltText, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt); err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, rows.Err()
}

func (s *Store) GetAttachment(ctx context.Context, id string) (Attachment, error) {
	var attachment Attachment
	err := s.db.QueryRowContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at FROM note_attachments WHERE id = ?`, id).Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.AltText, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return attachment, err
}

func (s *Store) GetAttachmentBySyncID(ctx context.Context, syncID string) (Attachment, error) {
	var attachment Attachment
	err := s.db.QueryRowContext(ctx, `SELECT id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at FROM note_attachments WHERE sync_id = ?`, syncID).Scan(&attachment.ID, &attachment.SyncID, &attachment.NoteID, &attachment.ContentHash, &attachment.OriginalName, &attachment.AltText, &attachment.MIMEType, &attachment.ByteSize, &attachment.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	return attachment, err
}

func (s *Store) UpdateAttachmentAltBySyncID(ctx context.Context, syncID, altText string) error {
	altText = strings.TrimSpace(altText)
	if !validSyncID(syncID) || len(altText) > 500 {
		return invalidInput("invalid image description")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE note_attachments SET alt_text = ? WHERE sync_id = ?`, altText, syncID)
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
		if utf8.RuneCountInString(token) >= 3 {
			where = append(where, `id IN (SELECT note_id FROM note_search WHERE note_search MATCH ?)`)
			args = append(args, fts5Phrase(token))
		} else {
			where = append(where, `(search_text LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM note_sources ns WHERE ns.note_id = notes.id AND ns.search_text LIKE ? ESCAPE '\'))`)
			pattern := "%" + escapeLike(token) + "%"
			args = append(args, pattern, pattern)
		}
	}
	args = append(args, limit)
	query := `SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at
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

// BackupData returns every note, including trashed records, and every saved
// revision. Its integrity digest covers all fields except the digest itself.
func (s *Store) BackupData(ctx context.Context) (Backup, error) {
	return s.backupData(ctx, false)
}

// FullBackupData returns a transactionally consistent snapshot including
// attachment metadata. Callers must archive the referenced content files.
func (s *Store) FullBackupData(ctx context.Context) (Backup, error) {
	return s.backupData(ctx, true)
}

func (s *Store) backupData(ctx context.Context, includeAttachments bool) (Backup, error) {
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
	backup.MaterialLinks, err = allMaterialLinks(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	backup.Verifications, err = allVerifications(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	backup.Topics, err = allTopics(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	backup.TopicMemberships, err = allTopicMemberships(ctx, tx)
	if err != nil {
		return Backup{}, err
	}
	if includeAttachments {
		backup.Attachments, err = allAttachments(ctx, tx)
		if err != nil {
			return Backup{}, err
		}
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
		if note.ID == "" || note.Revision <= 0 || strings.TrimSpace(note.Content) == "" || !validNoteKind(normalizedNoteKind(note.Kind)) {
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
		if note.DerivedFromID != nil {
			if normalizedNoteKind(note.Kind) != "procedure" {
				return fmt.Errorf("note %s is not a procedure but references source material", id)
			}
			if *note.DerivedFromID == id {
				return fmt.Errorf("note %s cannot derive from itself", id)
			}
			source, exists := notes[*note.DerivedFromID]
			if !exists || normalizedNoteKind(source.Kind) != "material" {
				return fmt.Errorf("note %s references invalid source material", id)
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
		if latest.Title != note.Title || latest.Content != note.Content || normalizedNoteKind(latest.Kind) != normalizedNoteKind(note.Kind) || latest.Starred != note.Starred ||
			!sameStringPointer(latest.ContinuedFromID, note.ContinuedFromID) || !sameStringPointer(latest.DerivedFromID, note.DerivedFromID) || !sameStringPointer(latest.DeletedAt, note.DeletedAt) || latest.CreatedAt != note.UpdatedAt {
			return fmt.Errorf("note %s does not match its latest revision", id)
		}
	}
	linkIDs := make(map[string]struct{}, len(backup.MaterialLinks))
	linkPairs := make(map[string]struct{}, len(backup.MaterialLinks))
	for _, link := range backup.MaterialLinks {
		note, noteExists := notes[link.NoteID]
		material, materialExists := notes[link.MaterialID]
		pair := link.NoteID + "\x00" + link.MaterialID
		if link.ID == "" || link.SyncID == "" || !noteExists || !materialExists || normalizedNoteKind(note.Kind) != "procedure" || normalizedNoteKind(material.Kind) != "material" || link.NoteID == link.MaterialID {
			return fmt.Errorf("backup has an invalid material link %s", link.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, link.CreatedAt); err != nil {
			return fmt.Errorf("backup has an invalid material link time %s", link.ID)
		}
		if _, exists := linkIDs[link.SyncID]; exists {
			return fmt.Errorf("backup has duplicate material link sync id %s", link.SyncID)
		}
		if _, exists := linkPairs[pair]; exists {
			return fmt.Errorf("backup has duplicate material link for note %s", link.NoteID)
		}
		linkIDs[link.SyncID], linkPairs[pair] = struct{}{}, struct{}{}
	}
	verificationIDs := make(map[string]struct{}, len(backup.Verifications))
	for _, verification := range backup.Verifications {
		note, exists := notes[verification.NoteID]
		if verification.ID == "" || verification.SyncID == "" || !exists || normalizedNoteKind(note.Kind) != "procedure" || verification.NoteRevision <= 0 || verification.NoteRevision > note.Revision || !validVerificationResult(verification.Result) || strings.TrimSpace(verification.Environment) == "" || len(verification.Environment) > 500 || len(verification.Comment) > 4000 {
			return fmt.Errorf("backup has an invalid verification %s", verification.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, verification.VerifiedAt); err != nil {
			return fmt.Errorf("backup has an invalid verification time %s", verification.ID)
		}
		if _, exists := verificationIDs[verification.SyncID]; exists {
			return fmt.Errorf("backup has duplicate verification sync id %s", verification.SyncID)
		}
		verificationIDs[verification.SyncID] = struct{}{}
	}
	topics := make(map[string]Topic, len(backup.Topics))
	topicSyncIDs := make(map[string]struct{}, len(backup.Topics))
	for _, topic := range backup.Topics {
		if topic.ID == "" || topic.SyncID == "" || strings.TrimSpace(topic.Name) == "" || len(topic.Name) > 80 {
			return fmt.Errorf("backup has an invalid topic %s", topic.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, topic.CreatedAt); err != nil {
			return fmt.Errorf("backup has an invalid topic creation time %s", topic.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, topic.UpdatedAt); err != nil {
			return fmt.Errorf("backup has an invalid topic update time %s", topic.ID)
		}
		if topic.DeletedAt != nil {
			if _, err := time.Parse(time.RFC3339Nano, *topic.DeletedAt); err != nil {
				return fmt.Errorf("backup has an invalid topic deletion time %s", topic.ID)
			}
		}
		if _, exists := topics[topic.ID]; exists {
			return fmt.Errorf("backup has duplicate topic id %s", topic.ID)
		}
		if _, exists := topicSyncIDs[topic.SyncID]; exists {
			return fmt.Errorf("backup has duplicate topic sync id %s", topic.SyncID)
		}
		topics[topic.ID], topicSyncIDs[topic.SyncID] = topic, struct{}{}
	}
	membershipSyncIDs := make(map[string]struct{}, len(backup.TopicMemberships))
	membershipPairs := make(map[string]struct{}, len(backup.TopicMemberships))
	for _, membership := range backup.TopicMemberships {
		note, noteExists := notes[membership.NoteID]
		_, topicExists := topics[membership.TopicID]
		pair := membership.TopicID + "\x00" + membership.NoteID
		if membership.ID == "" || membership.SyncID == "" || !topicExists || !noteExists || (membership.DeletedAt == nil && membership.Pinned && normalizedNoteKind(note.Kind) != "procedure") {
			return fmt.Errorf("backup has an invalid topic membership %s", membership.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, membership.CreatedAt); err != nil {
			return fmt.Errorf("backup has an invalid topic membership creation time %s", membership.ID)
		}
		if _, err := time.Parse(time.RFC3339Nano, membership.UpdatedAt); err != nil {
			return fmt.Errorf("backup has an invalid topic membership update time %s", membership.ID)
		}
		if membership.DeletedAt != nil {
			if _, err := time.Parse(time.RFC3339Nano, *membership.DeletedAt); err != nil {
				return fmt.Errorf("backup has an invalid topic membership deletion time %s", membership.ID)
			}
		}
		if _, exists := membershipSyncIDs[membership.SyncID]; exists {
			return fmt.Errorf("backup has duplicate topic membership sync id %s", membership.SyncID)
		}
		if _, exists := membershipPairs[pair]; exists {
			return fmt.Errorf("backup has duplicate topic membership for topic %s", membership.TopicID)
		}
		membershipSyncIDs[membership.SyncID], membershipPairs[pair] = struct{}{}, struct{}{}
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM topics`); err != nil {
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
		note.Kind = normalizedNoteKind(note.Kind)
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes (id, sync_id, request_id, request_hash, title, content, search_text, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?)`, note.ID, note.SyncID, requestID, requestHash, note.Title, note.Content,
			normalizeSearch(note.Title+"\n"+note.Content), note.Kind, note.Starred, note.Revision, note.CreatedAt, note.UpdatedAt, note.DeletedAt); err != nil {
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
	for _, note := range backup.Notes {
		if note.DerivedFromID == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notes SET derived_from_id = ? WHERE id = ?`, *note.DerivedFromID, note.ID); err != nil {
			return err
		}
	}
	for _, revision := range backup.Revisions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_revisions (note_id, revision, title, content, content_hash, kind, starred, continued_from_id, derived_from_id, deleted_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, revision.NoteID, revision.Revision, revision.Title, revision.Content, revision.ContentHash,
			normalizedNoteKind(revision.Kind), revision.Starred, revision.ContinuedFromID, revision.DerivedFromID, revision.DeletedAt, revision.CreatedAt); err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_attachments (id, sync_id, note_id, content_hash, original_name, alt_text, mime_type, byte_size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, attachment.ID, attachment.SyncID, attachment.NoteID, attachment.ContentHash, attachment.OriginalName, attachment.AltText, attachment.MIMEType, attachment.ByteSize, attachment.CreatedAt); err != nil {
			return err
		}
	}
	for _, source := range backup.Sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_sources (note_id, url, title, search_text, updated_at) VALUES (?, ?, ?, ?, ?)`, source.NoteID, source.URL, source.Title, normalizeSearch(source.Title+"\n"+source.URL), source.UpdatedAt); err != nil {
			return err
		}
	}
	for _, link := range backup.MaterialLinks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_material_links (id, sync_id, note_id, material_id, created_at) VALUES (?, ?, ?, ?, ?)`, link.ID, link.SyncID, link.NoteID, link.MaterialID, link.CreatedAt); err != nil {
			return err
		}
	}
	for _, verification := range backup.Verifications {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_verifications (id, sync_id, note_id, note_revision, verified_at, environment, result, comment) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, verification.ID, verification.SyncID, verification.NoteID, verification.NoteRevision, verification.VerifiedAt, verification.Environment, verification.Result, verification.Comment); err != nil {
			return err
		}
	}
	for _, topic := range backup.Topics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO topics (id, sync_id, name, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?)`, topic.ID, topic.SyncID, topic.Name, topic.CreatedAt, topic.UpdatedAt, topic.DeletedAt); err != nil {
			return err
		}
	}
	for _, membership := range backup.TopicMemberships {
		if _, err := tx.ExecContext(ctx, `INSERT INTO topic_memberships (id, sync_id, topic_id, note_id, pinned, created_at, updated_at, deleted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, membership.ID, membership.SyncID, membership.TopicID, membership.NoteID, membership.Pinned, membership.CreatedAt, membership.UpdatedAt, membership.DeletedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func allNotes(ctx context.Context, q queryer) ([]Note, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at
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
	rows, err := q.QueryContext(ctx, `SELECT note_id, revision, title, content, content_hash, kind, starred, continued_from_id, derived_from_id, deleted_at, created_at
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
		var derived sql.NullString
		var deleted sql.NullString
		if err := rows.Scan(&revision.NoteID, &revision.Revision, &revision.Title, &revision.Content, &revision.ContentHash,
			&revision.Kind, &starred, &continued, &derived, &deleted, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revision.Starred = starred != 0
		if continued.Valid {
			revision.ContinuedFromID = &continued.String
		}
		if derived.Valid {
			revision.DerivedFromID = &derived.String
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

func allMaterialLinks(ctx context.Context, q queryer) ([]NoteMaterialLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, note_id, material_id, created_at FROM note_material_links ORDER BY note_id ASC, created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]NoteMaterialLink, 0)
	for rows.Next() {
		var link NoteMaterialLink
		if err := rows.Scan(&link.ID, &link.SyncID, &link.NoteID, &link.MaterialID, &link.CreatedAt); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func allVerifications(ctx context.Context, q queryer) ([]NoteVerification, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, note_id, note_revision, verified_at, environment, result, comment FROM note_verifications ORDER BY note_id ASC, verified_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	verifications := make([]NoteVerification, 0)
	for rows.Next() {
		var verification NoteVerification
		if err := rows.Scan(&verification.ID, &verification.SyncID, &verification.NoteID, &verification.NoteRevision, &verification.VerifiedAt, &verification.Environment, &verification.Result, &verification.Comment); err != nil {
			return nil, err
		}
		verifications = append(verifications, verification)
	}
	return verifications, rows.Err()
}

func allTopics(ctx context.Context, q queryer) ([]Topic, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, name, created_at, updated_at, deleted_at FROM topics ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	topics := make([]Topic, 0)
	for rows.Next() {
		var topic Topic
		if err := rows.Scan(&topic.ID, &topic.SyncID, &topic.Name, &topic.CreatedAt, &topic.UpdatedAt, &topic.DeletedAt); err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, rows.Err()
}

func allTopicMemberships(ctx context.Context, q queryer) ([]TopicMembership, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, sync_id, topic_id, note_id, pinned, created_at, updated_at, deleted_at FROM topic_memberships ORDER BY topic_id ASC, created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memberships := make([]TopicMembership, 0)
	for rows.Next() {
		var membership TopicMembership
		if err := rows.Scan(&membership.ID, &membership.SyncID, &membership.TopicID, &membership.NoteID, &membership.Pinned, &membership.CreatedAt, &membership.UpdatedAt, &membership.DeletedAt); err != nil {
			return nil, err
		}
		memberships = append(memberships, membership)
	}
	return memberships, rows.Err()
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
	materialLinks, err := s.ListMaterialLinks(ctx)
	if err != nil {
		return nil, err
	}
	verifications, err := s.ListVerifications(ctx)
	if err != nil {
		return nil, err
	}
	topics, err := s.ListTopics(ctx)
	if err != nil {
		return nil, err
	}
	memberships, err := s.ListTopicMemberships(ctx)
	if err != nil {
		return nil, err
	}
	sourceByNote := make(map[string]NoteSource, len(sources))
	noteByID := make(map[string]Note, len(notes))
	topicByID := make(map[string]Topic, len(topics))
	topicNamesByNote := make(map[string][]string)
	materialIDsByNote := make(map[string]map[string]struct{})
	verificationsByNote := make(map[string][]NoteVerification)
	for _, note := range notes {
		noteByID[note.ID] = note
	}
	for _, topic := range topics {
		if topic.DeletedAt == nil {
			topicByID[topic.ID] = topic
		}
	}
	for _, membership := range memberships {
		if membership.DeletedAt == nil {
			if topic, exists := topicByID[membership.TopicID]; exists {
				topicNamesByNote[membership.NoteID] = append(topicNamesByNote[membership.NoteID], topic.Name)
			}
		}
	}
	for _, source := range sources {
		sourceByNote[source.NoteID] = source
	}
	for _, link := range materialLinks {
		if materialIDsByNote[link.NoteID] == nil {
			materialIDsByNote[link.NoteID] = make(map[string]struct{})
		}
		materialIDsByNote[link.NoteID][link.MaterialID] = struct{}{}
	}
	for _, verification := range verifications {
		verificationsByNote[verification.NoteID] = append(verificationsByNote[verification.NoteID], verification)
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
		if topicNames := topicNamesByNote[note.ID]; len(topicNames) > 0 {
			slices.Sort(topicNames)
			fmt.Fprintf(&out, "主题：%s\n\n", strings.Join(topicNames, "、"))
		}
		if note.Kind == "procedure" {
			status := "未实际验证"
			currentVerification := false
			for _, verification := range verificationsByNote[note.ID] {
				if verification.NoteRevision != note.Revision {
					continue
				}
				currentVerification = true
				switch verification.Result {
				case "success":
					status = "已验证 · " + verification.Environment
				case "partial":
					status = "最近使用部分成功"
				case "failed":
					status = "最近使用失败"
				}
			}
			if !currentVerification && len(verificationsByNote[note.ID]) > 0 {
				status = "当前版本待重新验证"
			}
			fmt.Fprintf(&out, "类型：操作记录\n\n状态：%s\n\n", status)
		} else if note.Kind == "material" {
			out.WriteString("类型：原始素材\n\n")
		}
		if materialIDsByNote[note.ID] == nil {
			materialIDsByNote[note.ID] = make(map[string]struct{})
		}
		if note.DerivedFromID != nil {
			materialIDsByNote[note.ID][*note.DerivedFromID] = struct{}{}
		}
		materialTitles := make([]string, 0, len(materialIDsByNote[note.ID]))
		for materialID := range materialIDsByNote[note.ID] {
			if material, exists := noteByID[materialID]; exists {
				materialTitles = append(materialTitles, material.Title)
			}
		}
		if len(materialTitles) > 0 {
			slices.Sort(materialTitles)
			fmt.Fprintf(&out, "提炼自：%s\n\n", strings.Join(materialTitles, "、"))
		}
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
	if in.Kind != nil {
		kind := normalizedNoteKind(*in.Kind)
		if !validNoteKind(kind) {
			return Note{}, invalidInput("unknown note kind")
		}
		if current.DerivedFromID != nil && kind != "procedure" {
			return Note{}, invalidInput("only procedure notes can reference source material")
		}
		if kind != "procedure" {
			var sourceCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_material_links WHERE note_id = ?`, current.ID).Scan(&sourceCount); err != nil {
				return Note{}, err
			}
			if sourceCount > 0 {
				return Note{}, invalidInput("only procedure notes can reference source material")
			}
			var pinnedCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM topic_memberships WHERE note_id = ? AND pinned = 1 AND deleted_at IS NULL`, current.ID).Scan(&pinnedCount); err != nil {
				return Note{}, err
			}
			if pinnedCount > 0 {
				return Note{}, invalidInput("a topic-pinned procedure cannot change kind")
			}
		}
		if current.Kind == "material" && kind != "material" {
			var derivedCount int
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM notes WHERE derived_from_id = ?) + (SELECT COUNT(*) FROM note_material_links WHERE material_id = ?)`, current.ID, current.ID).Scan(&derivedCount); err != nil {
				return Note{}, err
			}
			if derivedCount > 0 {
				return Note{}, invalidInput("source material is referenced by procedure notes")
			}
		}
		current.Kind = kind
	}
	if in.Starred != nil {
		current.Starred = *in.Starred
	}
	current.Revision++
	current.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE notes SET title = ?, content = ?, search_text = ?, kind = ?, starred = ?, revision = ?, updated_at = ?
		WHERE id = ? AND revision = ? AND deleted_at IS NULL`,
		current.Title, current.Content, normalizeSearch(current.Title+"\n"+current.Content), current.Kind, current.Starred, current.Revision, current.UpdatedAt, id, in.ExpectedRevision)
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
	query := fmt.Sprintf(`SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at
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
	row := q.QueryRowContext(ctx, `SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at FROM notes WHERE id = ?`, id)
	note, err := scanNote(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	return note, err
}

func getNoteByRequestID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requestID string) (Note, string, error) {
	row := q.QueryRowContext(ctx, `SELECT id, sync_id, title, content, kind, starred, continued_from_id, derived_from_id, revision, created_at, updated_at, deleted_at, request_hash FROM notes WHERE request_id = ?`, requestID)
	var note Note
	var starred int
	var continued sql.NullString
	var derived sql.NullString
	var deleted sql.NullString
	var requestHash sql.NullString
	err := row.Scan(&note.ID, &note.SyncID, &note.Title, &note.Content, &note.Kind, &starred, &continued, &derived, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted, &requestHash)
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
	if derived.Valid {
		note.DerivedFromID = &derived.String
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
	var derived sql.NullString
	var deleted sql.NullString
	err := row.Scan(&note.ID, &note.SyncID, &note.Title, &note.Content, &note.Kind, &starred, &continued, &derived, &note.Revision, &note.CreatedAt, &note.UpdatedAt, &deleted)
	if err != nil {
		return Note{}, err
	}
	note.Starred = starred != 0
	if continued.Valid {
		note.ContinuedFromID = &continued.String
	}
	if derived.Valid {
		note.DerivedFromID = &derived.String
	}
	if deleted.Valid {
		note.DeletedAt = &deleted.String
	}
	return note, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, note Note) error {
	hash := sha256.Sum256([]byte(note.Content))
	_, err := tx.ExecContext(ctx, `INSERT INTO note_revisions
		(note_id, revision, title, content, content_hash, kind, starred, continued_from_id, derived_from_id, deleted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		note.ID, note.Revision, note.Title, note.Content, hex.EncodeToString(hash[:]), normalizedNoteKind(note.Kind), note.Starred, note.ContinuedFromID, note.DerivedFromID, note.DeletedAt, note.UpdatedAt)
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

func normalizedNoteKind(kind string) string {
	if kind == "" {
		return "note"
	}
	return kind
}

func validNoteKind(kind string) bool {
	return kind == "note" || kind == "procedure" || kind == "material"
}

func validVerificationResult(result string) bool {
	return result == "success" || result == "partial" || result == "failed"
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var schemaVersion int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		return err
	}

	rows, err := tx.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return err
	}
	hasRequestHash := false
	hasSyncID := false
	hasKind := false
	hasDerivedFromID := false
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
		if name == "kind" {
			hasKind = true
		}
		if name == "derived_from_id" {
			hasDerivedFromID = true
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
	if !hasKind {
		if _, err := tx.Exec(`ALTER TABLE notes ADD COLUMN kind TEXT NOT NULL DEFAULT 'note' CHECK (kind IN ('note', 'procedure', 'material'))`); err != nil {
			return err
		}
	}
	if !hasDerivedFromID {
		if _, err := tx.Exec(`ALTER TABLE notes ADD COLUMN derived_from_id TEXT REFERENCES notes(id) ON DELETE SET NULL`); err != nil {
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
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_notes_derived_from ON notes(derived_from_id)`); err != nil {
		return err
	}
	rows, err = tx.Query(`SELECT id, derived_from_id FROM notes WHERE derived_from_id IS NOT NULL AND NOT EXISTS (
		SELECT 1 FROM note_material_links WHERE note_id = notes.id AND material_id = notes.derived_from_id)`)
	if err != nil {
		return err
	}
	type missingMaterialLink struct{ noteID, materialID string }
	missingLinks := make([]missingMaterialLink, 0)
	for rows.Next() {
		var link missingMaterialLink
		if err := rows.Scan(&link.noteID, &link.materialID); err != nil {
			rows.Close()
			return err
		}
		missingLinks = append(missingLinks, link)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, link := range missingLinks {
		id, err := newSyncID()
		if err != nil {
			return err
		}
		syncID, err := newSyncID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO note_material_links (id, sync_id, note_id, material_id, created_at) SELECT ?, ?, ?, ?, updated_at FROM notes WHERE id = ?`, id, syncID, link.noteID, link.materialID, link.noteID); err != nil {
			return err
		}
	}

	rows, err = tx.Query(`PRAGMA table_info(note_revisions)`)
	if err != nil {
		return err
	}
	hasRevisionKind := false
	hasRevisionDerivedFromID := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == "kind" {
			hasRevisionKind = true
		}
		if name == "derived_from_id" {
			hasRevisionDerivedFromID = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hasRevisionKind {
		if _, err := tx.Exec(`ALTER TABLE note_revisions ADD COLUMN kind TEXT NOT NULL DEFAULT 'note' CHECK (kind IN ('note', 'procedure', 'material'))`); err != nil {
			return err
		}
	}
	if !hasRevisionDerivedFromID {
		if _, err := tx.Exec(`ALTER TABLE note_revisions ADD COLUMN derived_from_id TEXT`); err != nil {
			return err
		}
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
	hasAttachmentAltText := false
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
		if name == "alt_text" {
			hasAttachmentAltText = true
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
	if !hasAttachmentAltText {
		if _, err := tx.Exec(`ALTER TABLE note_attachments ADD COLUMN alt_text TEXT NOT NULL DEFAULT ''`); err != nil {
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
	if schemaVersion < 3 {
		if _, err := tx.Exec(`DELETE FROM note_search`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO note_search(note_id, search_text)
			SELECT n.id, n.search_text || char(10) || COALESCE(s.search_text, '')
			FROM notes n LEFT JOIN note_sources s ON s.note_id = n.id`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO note_search(note_search) VALUES ('optimize')`); err != nil {
			return err
		}
		if _, err := tx.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
	}
	if schemaVersion < 4 {
		if _, err := tx.Exec(`PRAGMA user_version = 4`); err != nil {
			return err
		}
	}
	if schemaVersion < 5 {
		if _, err := tx.Exec(`PRAGMA user_version = 5`); err != nil {
			return err
		}
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

func fts5Phrase(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
