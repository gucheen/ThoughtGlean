package store

import (
	"context"
	"database/sql"
)

// ApplyRemoteNote merges a client note into the server's authoritative copy.
func (s *Store) ApplyRemoteNote(ctx context.Context, incoming Note) (Note, error) {
	return s.ApplyRemoteNoteWithParent(ctx, incoming, "")
}

func (s *Store) ApplyRemoteNoteWithParent(ctx context.Context, incoming Note, parentSyncID string) (Note, error) {
	if !validSyncID(incoming.SyncID) || incoming.Content == "" {
		return Note{}, invalidInput("invalid synced note")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()

	var localID, localUpdated string
	err = tx.QueryRowContext(ctx, `SELECT id, updated_at FROM notes WHERE sync_id = ?`, incoming.SyncID).Scan(&localID, &localUpdated)
	if err == nil {
		if localUpdated >= incoming.UpdatedAt {
			local, getErr := getNote(ctx, tx, localID)
			if getErr != nil {
				return Note{}, getErr
			}
			if incoming.Revision >= local.Revision && syncedNoteDiffers(incoming, local) {
				conflict, conflictErr := insertSyncConflict(ctx, tx, incoming, localID)
				if conflictErr != nil {
					return Note{}, conflictErr
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
		newID, idErr := newSyncID()
		if idErr != nil {
			return Note{}, idErr
		}
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

func syncedNoteDiffers(incoming, local Note) bool {
	return incoming.Title != local.Title || incoming.Content != local.Content || incoming.Starred != local.Starred || !sameStringPointer(incoming.DeletedAt, local.DeletedAt)
}

func insertSyncConflict(ctx context.Context, tx *sql.Tx, incoming Note, localID string) (Note, error) {
	conflictSyncID, err := newSyncID()
	if err != nil {
		return Note{}, err
	}
	conflictID, err := newSyncID()
	if err != nil {
		return Note{}, err
	}
	conflictTitle := "同步冲突：" + incoming.Title
	if incoming.Title == "" {
		conflictTitle = "同步冲突记录"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notes (id,sync_id,title,content,search_text,starred,continued_from_id,revision,created_at,updated_at,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, conflictID, conflictSyncID, conflictTitle, incoming.Content, normalizeSearch(conflictTitle+"\n"+incoming.Content), incoming.Starred, localID, 1, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
	if err != nil {
		return Note{}, err
	}
	conflict := Note{ID: conflictID, SyncID: conflictSyncID, Title: conflictTitle, Content: incoming.Content, Starred: incoming.Starred, ContinuedFromID: &localID, Revision: 1, CreatedAt: incoming.CreatedAt, UpdatedAt: incoming.UpdatedAt, DeletedAt: incoming.DeletedAt}
	if err := insertRevision(ctx, tx, conflict); err != nil {
		return Note{}, err
	}
	return conflict, nil
}
