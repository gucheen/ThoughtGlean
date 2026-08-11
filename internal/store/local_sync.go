package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// LocalSyncEvent is plaintext only inside the user's local library. It must
// be encrypted in the browser before it is sent to /api/sync/v1/.
type LocalSyncEvent struct {
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"createdAt"`
}

func (s *Store) QueueLocalSyncEvent(ctx context.Context, payload any) (LocalSyncEvent, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return LocalSyncEvent{}, fmt.Errorf("encode local sync event: %w", err)
	}
	id, err := newSyncID()
	if err != nil {
		return LocalSyncEvent{}, err
	}
	event := LocalSyncEvent{ID: id, Payload: encoded, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_sync_events (id, payload_json, created_at) VALUES (?, ?, ?)`, event.ID, event.Payload, event.CreatedAt)
	return event, err
}

func (s *Store) PendingLocalSyncEvents(ctx context.Context, limit int) ([]LocalSyncEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, payload_json, created_at FROM local_sync_events WHERE uploaded_at IS NULL ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]LocalSyncEvent, 0)
	for rows.Next() {
		var event LocalSyncEvent
		if err := rows.Scan(&event.ID, &event.Payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) MarkLocalSyncEventsUploaded(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE local_sync_events SET uploaded_at = ? WHERE id = ? AND uploaded_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ApplyRemoteNote uses the stable sync id to merge a remote version without
// adding it back to the local upload queue. Newer timestamps win only when
// there is no true concurrent edit; concurrency is surfaced to the client by
// preserving revisions in the next conflict-resolution layer.
func (s *Store) ApplyRemoteNote(ctx context.Context, incoming Note) (Note, error) {
	return s.ApplyRemoteNoteWithParent(ctx, incoming, "")
}

func (s *Store) ApplyRemoteNoteWithParent(ctx context.Context, incoming Note, parentSyncID string) (Note, error) {
	if incoming.SyncID == "" || incoming.Content == "" {
		return Note{}, invalidInput("invalid remote note")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	var localID string
	var localUpdated string
	err = tx.QueryRowContext(ctx, `SELECT id, updated_at FROM notes WHERE sync_id = ?`, incoming.SyncID).Scan(&localID, &localUpdated)
	if err == nil {
		if localUpdated >= incoming.UpdatedAt {
			local, getErr := getNote(ctx, tx, localID)
			if getErr != nil {
				return Note{}, getErr
			}
			if incoming.Revision >= local.Revision && (incoming.Title != local.Title || incoming.Content != local.Content || incoming.Starred != local.Starred || !sameStringPointer(incoming.DeletedAt, local.DeletedAt)) {
				conflictSyncID, idErr := newSyncID()
				if idErr != nil {
					return Note{}, idErr
				}
				conflictTitle := "同步冲突：" + incoming.Title
				if incoming.Title == "" {
					conflictTitle = "同步冲突记录"
				}
				conflictID, idErr := newSyncID(); if idErr != nil { return Note{}, idErr }
				_, insertErr := tx.ExecContext(ctx, `INSERT INTO notes (id,sync_id,title,content,search_text,starred,continued_from_id,revision,created_at,updated_at,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, conflictID, conflictSyncID, conflictTitle, incoming.Content, normalizeSearch(conflictTitle+"\n"+incoming.Content), incoming.Starred, localID, 1, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
				if insertErr != nil {
					return Note{}, insertErr
				}
				conflict := Note{ID: conflictID, SyncID: conflictSyncID, Title: conflictTitle, Content: incoming.Content, Starred: incoming.Starred, ContinuedFromID: &localID, Revision: 1, CreatedAt: incoming.CreatedAt, UpdatedAt: incoming.UpdatedAt, DeletedAt: incoming.DeletedAt}
				if err := insertRevision(ctx, tx, conflict); err != nil {
					return Note{}, err
				}
				if err := tx.Commit(); err != nil {
					return Note{}, err
				}
				return conflict, nil
			}
			return local, tx.Commit()
		}
		_, err = tx.ExecContext(ctx, `UPDATE notes SET title=?, content=?, search_text=?, starred=?, revision=?, updated_at=?, deleted_at=? WHERE id=?`, incoming.Title, incoming.Content, normalizeSearch(incoming.Title+"\n"+incoming.Content), incoming.Starred, incoming.Revision, incoming.UpdatedAt, incoming.DeletedAt, localID)
		if err != nil {
			return Note{}, err
		}
		incoming.ID = localID
	} else if err == sql.ErrNoRows {
		var parent any
		var parentID *string
		if parentSyncID != "" {
			var id string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM notes WHERE sync_id = ?`, parentSyncID).Scan(&id); err == nil {
				parentID = &id
				parent = id
			}
		}
		newID, idErr := newSyncID(); if idErr != nil { return Note{}, idErr }
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO notes (id,sync_id,title,content,search_text,starred,continued_from_id,revision,created_at,updated_at,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, newID, incoming.SyncID, incoming.Title, incoming.Content, normalizeSearch(incoming.Title+"\n"+incoming.Content), incoming.Starred, parent, incoming.Revision, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
		if insertErr != nil {
			return Note{}, insertErr
		}
		incoming.ID = newID
		incoming.ContinuedFromID = parentID
	} else {
		return Note{}, err
	}
	if err := insertRevision(ctx, tx, incoming); err != nil {
		return Note{}, err
	}
	if err := tx.Commit(); err != nil {
		return Note{}, err
	}
	return incoming, nil
}
