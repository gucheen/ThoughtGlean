package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "thoughtglean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCreateIsIdempotentAndSearchUsesAllTerms(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	input := CreateNoteInput{RequestID: "capture-1", Content: "真正重要的不是自动化，而是失败时先保住原始数据。SQLite 会留下它。"}
	first, duplicate, err := store.CreateNote(ctx, input)
	if err != nil || duplicate {
		t.Fatalf("create: duplicate=%v err=%v", duplicate, err)
	}
	second, duplicate, err := store.CreateNote(ctx, input)
	if err != nil || !duplicate || second.ID != first.ID || second.Content != first.Content {
		t.Fatalf("idempotent create = %#v duplicate=%v err=%v", second, duplicate, err)
	}
	_, _, err = store.CreateNote(ctx, CreateNoteInput{RequestID: "capture-1", Content: "different"})
	var idempotencyConflict *IdempotencyConflictError
	if !errors.As(err, &idempotencyConflict) || idempotencyConflict.Current.ID != first.ID {
		t.Fatalf("expected idempotency conflict with original note, got %v", err)
	}

	for _, query := range []string{"失败 原始", "SQLITE", "sqlite 数据"} {
		notes, err := store.ListNotes(ctx, ListOptions{Query: query})
		if err != nil || len(notes) != 1 || notes[0].ID != first.ID {
			t.Fatalf("query %q returned %#v, %v", query, notes, err)
		}
	}
	notes, err := store.ListNotes(ctx, ListOptions{Query: "失败 不存在"})
	if err != nil || len(notes) != 0 {
		t.Fatalf("AND query returned %#v, %v", notes, err)
	}
}

func TestRequestHashMigrationPreservesIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thoughtglean.db")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := CreateNoteInput{RequestID: "before-request-hash", Title: "原题", Content: "迁移前的正文", Starred: true}
	created, _, err := legacy.CreateNote(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`ALTER TABLE notes DROP COLUMN request_hash`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { migrated.Close() })
	duplicate, wasDuplicate, err := migrated.CreateNote(context.Background(), input)
	if err != nil || !wasDuplicate || duplicate.ID != created.ID {
		t.Fatalf("migrated duplicate = %#v duplicate=%v err=%v", duplicate, wasDuplicate, err)
	}
	_, _, err = migrated.CreateNote(context.Background(), CreateNoteInput{RequestID: input.RequestID, Content: "另一段正文"})
	var conflict *IdempotencyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected migrated request hash conflict, got %v", err)
	}
}

func TestUpdateConflictDeleteAndRestore(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	note, _, err := store.CreateNote(ctx, CreateNoteInput{Content: "第一版"})
	if err != nil {
		t.Fatal(err)
	}
	content := "第二版，加入 SQLite"
	updated, err := store.UpdateNote(ctx, note.ID, UpdateNoteInput{Content: &content, ExpectedRevision: note.Revision})
	if err != nil || updated.Revision != 2 || updated.CreatedAt != note.CreatedAt {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	_, err = store.UpdateNote(ctx, note.ID, UpdateNoteInput{Content: &content, ExpectedRevision: note.Revision})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current.Revision != 2 {
		t.Fatalf("expected conflict with current note, got %v", err)
	}

	deleted, err := store.DeleteNote(ctx, note.ID)
	if err != nil || deleted.DeletedAt == nil {
		t.Fatalf("delete = %#v, %v", deleted, err)
	}
	visible, _ := store.ListNotes(ctx, ListOptions{})
	trash, _ := store.ListNotes(ctx, ListOptions{View: "trash"})
	if len(visible) != 0 || len(trash) != 1 {
		t.Fatalf("visible=%d trash=%d", len(visible), len(trash))
	}
	restored, err := store.RestoreNote(ctx, note.ID)
	if err != nil || restored.DeletedAt != nil {
		t.Fatalf("restore = %#v, %v", restored, err)
	}
}

func TestContinuedNoteContext(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	first, _, _ := store.CreateNote(ctx, CreateNoteInput{Content: "最初的念头"})
	second, _, err := store.CreateNote(ctx, CreateNoteInput{Content: "接着想", ContinuedFromID: &first.ID})
	if err != nil {
		t.Fatal(err)
	}
	context, err := store.NoteContext(ctx, second.ID, 2)
	if err != nil || len(context.Before) != 1 || context.Before[0].ID != first.ID {
		t.Fatalf("context = %#v, %v", context, err)
	}
}
