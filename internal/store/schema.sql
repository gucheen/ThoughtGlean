PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS notes (
    id TEXT PRIMARY KEY,
    sync_id TEXT NOT NULL UNIQUE,
    request_id TEXT UNIQUE,
    request_hash TEXT,
    title TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    search_text TEXT NOT NULL,
    starred INTEGER NOT NULL DEFAULT 0 CHECK (starred IN (0, 1)),
    continued_from_id TEXT REFERENCES notes(id) ON DELETE SET NULL,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT,
    CHECK (continued_from_id IS NULL OR continued_from_id != id)
);

CREATE INDEX IF NOT EXISTS idx_notes_recent
    ON notes(deleted_at, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_notes_starred
    ON notes(deleted_at, starred, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_notes_continued_from
    ON notes(continued_from_id);

CREATE TABLE IF NOT EXISTS note_revisions (
    note_id TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    starred INTEGER NOT NULL CHECK (starred IN (0, 1)),
    continued_from_id TEXT,
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (note_id, revision)
);

CREATE TABLE IF NOT EXISTS passkey_users (
    id BLOB PRIMARY KEY,
    name TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS passkey_credentials (
    id BLOB PRIMARY KEY,
    user_id BLOB NOT NULL REFERENCES passkey_users(id) ON DELETE CASCADE,
    credential_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_ceremonies (
    id BLOB PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('registration', 'login')),
    session_json BLOB NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_sessions (
    token_hash BLOB PRIMARY KEY,
    user_id BLOB NOT NULL REFERENCES passkey_users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS note_sources (
    note_id TEXT PRIMARY KEY REFERENCES notes(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    search_text TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS note_attachments (
    id TEXT PRIMARY KEY,
    sync_id TEXT NOT NULL UNIQUE,
    note_id TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    original_name TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    byte_size INTEGER NOT NULL CHECK (byte_size > 0),
    created_at TEXT NOT NULL,
    UNIQUE(note_id, content_hash)
);

CREATE INDEX IF NOT EXISTS idx_note_attachments_note ON note_attachments(note_id, id);

-- The sync relay deliberately has no foreign key to notes. It is used by a
-- separately deployed relay instance and must never receive note plaintext.
CREATE TABLE IF NOT EXISTS sync_vaults (
    id TEXT PRIMARY KEY,
    token_hash BLOB NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_operations (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    vault_id TEXT NOT NULL REFERENCES sync_vaults(id) ON DELETE CASCADE,
    operation_id TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(vault_id, operation_id)
);

CREATE INDEX IF NOT EXISTS idx_sync_operations_vault_sequence
    ON sync_operations(vault_id, sequence);

CREATE TABLE IF NOT EXISTS sync_blobs (
    vault_id TEXT NOT NULL REFERENCES sync_vaults(id) ON DELETE CASCADE,
    blob_id TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (vault_id, blob_id)
);

-- Plaintext changes are retained only in the local personal library until
-- the browser encrypts and hands them to a remote relay. A relay deployment
-- never calls these tables or APIs.
CREATE TABLE IF NOT EXISTS local_sync_events (
    id TEXT PRIMARY KEY,
    payload_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    uploaded_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_local_sync_events_pending
    ON local_sync_events(uploaded_at, created_at, id);
