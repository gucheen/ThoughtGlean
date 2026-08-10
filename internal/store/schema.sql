PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT UNIQUE,
    request_hash TEXT,
    title TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL,
    search_text TEXT NOT NULL,
    starred INTEGER NOT NULL DEFAULT 0 CHECK (starred IN (0, 1)),
    continued_from_id INTEGER REFERENCES notes(id) ON DELETE SET NULL,
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
    note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    starred INTEGER NOT NULL CHECK (starred IN (0, 1)),
    continued_from_id INTEGER,
    deleted_at TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (note_id, revision)
);
