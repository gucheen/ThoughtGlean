package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
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

func TestPasskeyOwnerAndCredentialStorage(t *testing.T) {
	noteStore := testStore(t)
	ctx := context.Background()
	configured, err := noteStore.HasPasskey(ctx)
	if err != nil || configured {
		t.Fatalf("initial passkey configured=%v err=%v", configured, err)
	}
	owner, err := noteStore.CreateOwner(ctx, "我的拾念")
	if err != nil || len(owner.ID) != 32 {
		t.Fatalf("create owner=%#v err=%v", owner, err)
	}
	credential := webauthn.Credential{ID: []byte("credential-id")}
	if err := noteStore.AddPasskeyCredential(ctx, owner, credential); err != nil {
		t.Fatal(err)
	}
	if err := noteStore.DeletePasskeyCredential(ctx, owner, credential.ID); err == nil {
		t.Fatal("final passkey was removed")
	}
	secondCredential := webauthn.Credential{ID: []byte("second-credential-id")}
	if err := noteStore.AddPasskeyCredential(ctx, owner, secondCredential); err != nil {
		t.Fatal(err)
	}
	if err := noteStore.DeletePasskeyCredential(ctx, owner, credential.ID); err != nil {
		t.Fatal(err)
	}
	configured, err = noteStore.HasPasskey(ctx)
	if err != nil || !configured {
		t.Fatalf("stored passkey configured=%v err=%v", configured, err)
	}
	loaded, err := noteStore.Owner(ctx)
	if err != nil || loaded.Name != owner.Name || len(loaded.Credentials) != 1 || string(loaded.Credentials[0].ID) != "second-credential-id" {
		t.Fatalf("loaded owner=%#v err=%v", loaded, err)
	}
	token, err := noteStore.CreateAuthSession(ctx, loaded)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := noteStore.ValidateAuthSession(ctx, token)
	if err != nil || !valid {
		t.Fatalf("session valid=%v err=%v", valid, err)
	}
	if err := noteStore.DeleteAuthSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	valid, err = noteStore.ValidateAuthSession(ctx, token)
	if err != nil || valid {
		t.Fatalf("deleted session valid=%v err=%v", valid, err)
	}
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
	if created.SyncID == "" || len(created.SyncID) != 22 {
		t.Fatalf("new note is missing a stable sync id: %#v", created)
	}
	if created.ID == "" || len(created.ID) != 22 {
		t.Fatalf("new note is missing a random string primary key: %#v", created)
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
	if duplicate.SyncID != created.SyncID {
		t.Fatalf("sync id changed after migration: %q != %q", duplicate.SyncID, created.SyncID)
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

func TestRemoteConcurrentEditIsPreservedAsConflictNote(t *testing.T) {
	noteStore := testStore(t)
	ctx := context.Background()
	local, _, err := noteStore.CreateNote(ctx, CreateNoteInput{Title: "同一念头", Content: "本机版本"})
	if err != nil {
		t.Fatal(err)
	}
	remote := local
	remote.Content = "另一台设备版本"
	remote.UpdatedAt = local.UpdatedAt
	remote.Revision = local.Revision
	conflict, err := noteStore.ApplyRemoteNote(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.ID == local.ID || !strings.HasPrefix(conflict.Title, "同步冲突") || conflict.ContinuedFromID == nil || *conflict.ContinuedFromID != local.ID {
		t.Fatalf("conflict=%#v local=%#v", conflict, local)
	}
	retried, err := noteStore.ApplyRemoteNote(ctx, remote)
	if err != nil || retried.ID != conflict.ID {
		t.Fatalf("retried conflict=%#v err=%v; want id %s", retried, err, conflict.ID)
	}
	visible, err := noteStore.ListNotes(ctx, ListOptions{})
	if err != nil || len(visible) != 2 {
		t.Fatalf("retry created duplicate conflict: notes=%d err=%v", len(visible), err)
	}
}

func TestRemoteConcurrentEditDoesNotTrustFutureClientClock(t *testing.T) {
	noteStore := testStore(t)
	ctx := context.Background()
	local, _, err := noteStore.CreateNote(ctx, CreateNoteInput{Title: "时间", Content: "服务端版本"})
	if err != nil {
		t.Fatal(err)
	}
	remote := local
	remote.Content = "时钟超前设备的版本"
	remote.UpdatedAt = "2099-01-01T00:00:00Z"
	conflict, err := noteStore.ApplyRemoteNote(ctx, remote)
	if err != nil || conflict.ID == local.ID {
		t.Fatalf("future clock overwrote local note: conflict=%#v err=%v", conflict, err)
	}
	unchanged, err := noteStore.GetNote(ctx, local.ID)
	if err != nil || unchanged.Content != "服务端版本" {
		t.Fatalf("local note=%#v err=%v", unchanged, err)
	}
}

func TestAttachmentDescriptionPersistsAndUpdates(t *testing.T) {
	noteStore := testStore(t)
	ctx := context.Background()
	note, _, err := noteStore.CreateNote(ctx, CreateNoteInput{Content: "带图记录"})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := noteStore.AddAttachment(ctx, Attachment{NoteID: note.ID, ContentHash: strings.Repeat("a", 64), OriginalName: "photo.jpg", AltText: "原始说明", MIMEType: "image/jpeg", ByteSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err := noteStore.UpdateAttachmentAltBySyncID(ctx, attachment.SyncID, "更新后的说明"); err != nil {
		t.Fatal(err)
	}
	items, err := noteStore.ListAttachments(ctx, note.ID)
	if err != nil || len(items) != 1 || items[0].AltText != "更新后的说明" {
		t.Fatalf("attachments=%#v err=%v", items, err)
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

func TestBackupIncludesHistoryAndDetectsTampering(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	note, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "原题", Content: "第一版"})
	if err != nil {
		t.Fatal(err)
	}
	content := "第二版"
	if _, err := store.UpdateNote(ctx, note.ID, UpdateNoteInput{Content: &content, ExpectedRevision: note.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteNote(ctx, note.ID); err != nil {
		t.Fatal(err)
	}
	backup, err := store.BackupData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.Notes) != 1 || backup.Notes[0].DeletedAt == nil || len(backup.Revisions) != 3 {
		t.Fatalf("incomplete backup: %#v", backup)
	}
	if err := VerifyBackup(backup); err != nil {
		t.Fatalf("valid backup rejected: %v", err)
	}
	backup.Notes[0].Content = "已被改写"
	if err := VerifyBackup(backup); err == nil {
		t.Fatal("tampered backup passed verification")
	}
}

func TestMarkdownExportIsReadableAndExcludesTrash(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	active, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "保留", Content: "仍然可读"})
	if err != nil {
		t.Fatal(err)
	}
	trashed, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "回收", Content: "不应导出"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteNote(ctx, trashed.ID); err != nil {
		t.Fatal(err)
	}
	data, err := store.MarkdownExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "## 保留") || !strings.Contains(text, "仍然可读") || strings.Contains(text, "不应导出") {
		t.Fatalf("unexpected markdown: %s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("thoughtglean:id=%s", active.ID)) {
		t.Fatalf("missing note metadata: %s", text)
	}
}

func TestRestoreBackupReplacesDataAndPreservesIdempotency(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	note, _, err := source.CreateNote(ctx, CreateNoteInput{RequestID: "restored-request", Title: "恢复来源", Content: "第一版"})
	if err != nil {
		t.Fatal(err)
	}
	content := "第二版"
	if _, err := source.UpdateNote(ctx, note.ID, UpdateNoteInput{Content: &content, ExpectedRevision: note.Revision}); err != nil {
		t.Fatal(err)
	}
	backup, err := source.BackupData(ctx)
	if err != nil {
		t.Fatal(err)
	}

	target := testStore(t)
	if _, _, err := target.CreateNote(ctx, CreateNoteInput{Content: "应被替换"}); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	notes, err := target.ListNotes(ctx, ListOptions{View: "all"})
	if err != nil || len(notes) != 1 || notes[0].ID != note.ID || notes[0].Content != content || notes[0].Revision != 2 {
		t.Fatalf("restored notes=%#v err=%v", notes, err)
	}
	duplicate, wasDuplicate, err := target.CreateNote(ctx, CreateNoteInput{RequestID: "restored-request", Title: "恢复来源", Content: "第一版"})
	if err != nil || !wasDuplicate || duplicate.ID != note.ID {
		t.Fatalf("restored idempotency note=%#v duplicate=%v err=%v", duplicate, wasDuplicate, err)
	}
}
