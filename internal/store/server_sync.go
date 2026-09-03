package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
)

// ApplyRemoteNote merges a client note into the server's authoritative copy.
func (s *Store) ApplyRemoteNote(ctx context.Context, incoming Note) (Note, error) {
	return s.ApplyRemoteNoteWithRelations(ctx, incoming, "", "")
}

func (s *Store) ApplyRemoteNoteWithParent(ctx context.Context, incoming Note, parentSyncID string) (Note, error) {
	return s.ApplyRemoteNoteWithRelations(ctx, incoming, parentSyncID, "")
}

func (s *Store) ApplyRemoteNoteWithRelations(ctx context.Context, incoming Note, parentSyncID, derivedFromSyncID string) (Note, error) {
	if !validSyncID(incoming.SyncID) || incoming.Content == "" {
		return Note{}, invalidInput("invalid synced note")
	}
	incoming.Kind = normalizedNoteKind(incoming.Kind)
	if !validNoteKind(incoming.Kind) {
		return Note{}, invalidInput("unknown note kind")
	}
	if derivedFromSyncID != "" && incoming.Kind != "procedure" {
		return Note{}, invalidInput("only procedure notes can reference source material")
	}
	if derivedFromSyncID == incoming.SyncID {
		return Note{}, invalidInput("note cannot derive from itself")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Note{}, err
	}
	defer tx.Rollback()
	var derived any
	if derivedFromSyncID != "" {
		var id, kind string
		if err := tx.QueryRowContext(ctx, `SELECT id, kind FROM notes WHERE sync_id = ?`, derivedFromSyncID).Scan(&id, &kind); err != nil {
			if err == sql.ErrNoRows {
				return Note{}, invalidInput("source material does not exist")
			}
			return Note{}, err
		}
		if kind != "material" {
			return Note{}, invalidInput("derived note must reference source material")
		}
		incoming.DerivedFromID = &id
		derived = id
	} else {
		incoming.DerivedFromID = nil
	}

	var localID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM notes WHERE sync_id = ?`, incoming.SyncID).Scan(&localID)
	if err == nil {
		local, getErr := getNote(ctx, tx, localID)
		if getErr != nil {
			return Note{}, getErr
		}
		if local.Kind == "material" && incoming.Kind != "material" {
			var derivedCount int
			if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM notes WHERE derived_from_id = ?) + (SELECT COUNT(*) FROM note_material_links WHERE material_id = ?)`, localID, localID).Scan(&derivedCount); err != nil {
				return Note{}, err
			}
			if derivedCount > 0 {
				return Note{}, invalidInput("source material is referenced by procedure notes")
			}
		}
		if incoming.Kind != "procedure" {
			var sourceCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM note_material_links WHERE note_id = ?`, localID).Scan(&sourceCount); err != nil {
				return Note{}, err
			}
			if sourceCount > 0 {
				return Note{}, invalidInput("only procedure notes can reference source material")
			}
			var pinnedCount int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM topic_memberships WHERE note_id = ? AND pinned = 1 AND deleted_at IS NULL`, localID).Scan(&pinnedCount); err != nil {
				return Note{}, err
			}
			if pinnedCount > 0 {
				return Note{}, invalidInput("a topic-pinned procedure cannot change kind")
			}
		}
		if !syncedNoteDiffers(incoming, local) {
			return local, tx.Commit()
		}
		// Revision is the concurrency boundary. Browser clocks are not trusted:
		// an edit based on the current or an older revision is preserved as a
		// conflict even if that device's timestamp happens to be later.
		if incoming.Revision <= local.Revision {
			conflict, conflictErr := insertSyncConflict(ctx, tx, incoming, localID)
			if conflictErr != nil {
				return Note{}, conflictErr
			}
			if err := tx.Commit(); err != nil {
				return Note{}, err
			}
			return conflict, nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE notes SET title=?, content=?, search_text=?, kind=?, starred=?, derived_from_id=?, revision=?, updated_at=?, deleted_at=? WHERE id=?`, incoming.Title, incoming.Content, normalizeSearch(incoming.Title+"\n"+incoming.Content), incoming.Kind, incoming.Starred, derived, incoming.Revision, incoming.UpdatedAt, incoming.DeletedAt, localID)
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
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO notes (id,sync_id,title,content,search_text,kind,starred,continued_from_id,derived_from_id,revision,created_at,updated_at,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID, incoming.SyncID, incoming.Title, incoming.Content, normalizeSearch(incoming.Title+"\n"+incoming.Content), incoming.Kind, incoming.Starred, parent, derived, incoming.Revision, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
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
	return incoming.Title != local.Title || incoming.Content != local.Content || normalizedNoteKind(incoming.Kind) != normalizedNoteKind(local.Kind) || incoming.Starred != local.Starred || !sameStringPointer(incoming.DerivedFromID, local.DerivedFromID) || !sameStringPointer(incoming.DeletedAt, local.DeletedAt)
}

func insertSyncConflict(ctx context.Context, tx *sql.Tx, incoming Note, localID string) (Note, error) {
	conflictSyncID := conflictIdentifier("sync", incoming)
	var existingID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM notes WHERE sync_id = ?`, conflictSyncID).Scan(&existingID); err == nil {
		return getNote(ctx, tx, existingID)
	} else if err != sql.ErrNoRows {
		return Note{}, err
	}
	conflictID := conflictIdentifier("note", incoming)
	conflictTitle := "同步冲突：" + incoming.Title
	if incoming.Title == "" {
		conflictTitle = "同步冲突记录"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notes (id,sync_id,title,content,search_text,kind,starred,continued_from_id,derived_from_id,revision,created_at,updated_at,deleted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, conflictID, conflictSyncID, conflictTitle, incoming.Content, normalizeSearch(conflictTitle+"\n"+incoming.Content), incoming.Kind, incoming.Starred, localID, incoming.DerivedFromID, 1, incoming.CreatedAt, incoming.UpdatedAt, incoming.DeletedAt)
	if err != nil {
		return Note{}, err
	}
	conflict := Note{ID: conflictID, SyncID: conflictSyncID, Title: conflictTitle, Content: incoming.Content, Kind: incoming.Kind, Starred: incoming.Starred, ContinuedFromID: &localID, DerivedFromID: incoming.DerivedFromID, Revision: 1, CreatedAt: incoming.CreatedAt, UpdatedAt: incoming.UpdatedAt, DeletedAt: incoming.DeletedAt}
	if err := insertRevision(ctx, tx, conflict); err != nil {
		return Note{}, err
	}
	return conflict, nil
}

func conflictIdentifier(kind string, incoming Note) string {
	deletedAt := ""
	derivedFromID := ""
	if incoming.DeletedAt != nil {
		deletedAt = *incoming.DeletedAt
	}
	if incoming.DerivedFromID != nil {
		derivedFromID = *incoming.DerivedFromID
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%t\x00%s\x00%s", kind, incoming.SyncID, incoming.Revision, incoming.Title, incoming.Content, incoming.Kind, incoming.Starred, derivedFromID, deletedAt)))
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}
