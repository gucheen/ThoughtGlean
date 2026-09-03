package store

import (
	"context"
	"database/sql"
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

func TestOpenConfiguresSQLite(t *testing.T) {
	noteStore := testStore(t)
	checks := []struct {
		pragma string
		want   string
	}{
		{"busy_timeout", "5000"},
		{"foreign_keys", "1"},
		{"journal_mode", "wal"},
		{"synchronous", "2"},
	}
	for _, check := range checks {
		var got string
		if err := noteStore.db.QueryRow("PRAGMA " + check.pragma).Scan(&got); err != nil {
			t.Fatalf("read PRAGMA %s: %v", check.pragma, err)
		}
		if got != check.want {
			t.Errorf("PRAGMA %s = %q, want %q", check.pragma, got, check.want)
		}
	}
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

	for _, query := range []string{"失败 原始", "SQLITE", "QLI", "sqlite 数据"} {
		notes, err := store.ListNotes(ctx, ListOptions{Query: query})
		if err != nil || len(notes) != 1 || notes[0].ID != first.ID {
			t.Fatalf("query %q returned %#v, %v", query, notes, err)
		}
	}
	notes, err := store.ListNotes(ctx, ListOptions{Query: "失败 不存在"})
	if err != nil || len(notes) != 0 {
		t.Fatalf("AND query returned %#v, %v", notes, err)
	}
	changedContent := "更新后的正文改为 PostgreSQL"
	updated, err := store.UpdateNote(ctx, first.ID, UpdateNoteInput{Content: &changedContent, ExpectedRevision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{"QLI": 0, "stgre": 1} {
		notes, err = store.ListNotes(ctx, ListOptions{Query: query})
		if err != nil || len(notes) != want {
			t.Fatalf("updated FTS query %q returned %#v, %v", query, notes, err)
		}
	}

	if _, err := store.SetNoteSource(ctx, updated.ID, NoteSource{URL: "https://example.com/reference", Title: "Architecture Reference"}); err != nil {
		t.Fatal(err)
	}
	notes, err = store.ListNotes(ctx, ListOptions{Query: "tectu"})
	if err != nil || len(notes) != 1 || notes[0].ID != updated.ID {
		t.Fatalf("FTS source query returned %#v, %v", notes, err)
	}
	if _, err := store.SetNoteSource(ctx, updated.ID, NoteSource{URL: "https://example.com/distributed", Title: "Distributed Systems"}); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{"tectu": 0, "tribu": 1} {
		notes, err = store.ListNotes(ctx, ListOptions{Query: query})
		if err != nil || len(notes) != want {
			t.Fatalf("updated FTS source query %q returned %#v, %v", query, notes, err)
		}
	}
	if _, err := store.SetNoteSource(ctx, updated.ID, NoteSource{}); err != nil {
		t.Fatal(err)
	}
	notes, err = store.ListNotes(ctx, ListOptions{Query: "tectu"})
	if err != nil || len(notes) != 0 {
		t.Fatalf("deleted FTS source query returned %#v, %v", notes, err)
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
	if _, err := legacy.db.Exec(`DELETE FROM note_search`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.Exec(`PRAGMA user_version = 2`); err != nil {
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
	found, err := migrated.ListNotes(context.Background(), ListOptions{Query: "迁移前"})
	if err != nil || len(found) != 1 || found[0].ID != created.ID {
		t.Fatalf("FTS migration search returned %#v, %v", found, err)
	}
	_, _, err = migrated.CreateNote(context.Background(), CreateNoteInput{RequestID: input.RequestID, Content: "另一段正文"})
	var conflict *IdempotencyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected migrated request hash conflict, got %v", err)
	}
}

func TestMigrationAddsDerivedFromBeforeCreatingItsIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thoughtglean.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE notes (
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
			deleted_at TEXT
		);
		INSERT INTO notes (
			id, sync_id, title, content, search_text, starred, revision, created_at, updated_at
		) VALUES (
			'legacy-note-id', 'legacy-note-sync-id', '旧记录', '迁移前的正文', '旧记录\n迁移前的正文', 1, 1,
			'2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z'
		);
		PRAGMA user_version = 4;
	`)
	if err != nil {
		legacy.Close()
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
	note, err := migrated.GetNote(context.Background(), "legacy-note-id")
	if err != nil {
		t.Fatal(err)
	}
	if note.Title != "旧记录" || note.Content != "迁移前的正文" || note.Kind != "note" || !note.Starred {
		t.Fatalf("migrated note = %#v", note)
	}
	var indexCount int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_notes_derived_from'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatalf("derived-from index count = %d, want 1", indexCount)
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

func TestDerivedProcedurePreservesSourceMaterialAcrossBackup(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	material, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "Docker 对话", Content: "原始内容", Kind: "material"})
	if err != nil {
		t.Fatal(err)
	}
	procedure, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "查看 Docker 占用", Content: "状态：未实际验证", Kind: "procedure", DerivedFromID: &material.ID})
	if err != nil {
		t.Fatal(err)
	}
	if procedure.Kind != "procedure" || procedure.DerivedFromID == nil || *procedure.DerivedFromID != material.ID {
		t.Fatalf("procedure=%#v", procedure)
	}
	if _, _, err := store.CreateNote(ctx, CreateNoteInput{Content: "错误关系", Kind: "procedure", DerivedFromID: &procedure.ID}); err == nil {
		t.Fatal("procedure accepted a non-material source")
	}
	if _, _, err := store.CreateNote(ctx, CreateNoteInput{Content: "错误类型", DerivedFromID: &material.ID}); err == nil {
		t.Fatal("ordinary note accepted a source material relation")
	}
	ordinaryKind := "note"
	if _, err := store.UpdateNote(ctx, material.ID, UpdateNoteInput{Kind: &ordinaryKind, ExpectedRevision: material.Revision}); err == nil {
		t.Fatal("referenced source material changed into an ordinary note")
	}
	backup, err := store.BackupData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := testStore(t)
	if err := target.RestoreBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := target.GetNote(ctx, procedure.ID)
	if err != nil || restored.Kind != "procedure" || restored.DerivedFromID == nil || *restored.DerivedFromID != material.ID {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
}

func TestRemoteDerivedProcedureResolvesMaterialBySyncIdentity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	material, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "来源", Content: "对话内容", Kind: "material"})
	if err != nil {
		t.Fatal(err)
	}
	remote := Note{SyncID: "remote-procedure-sync-id", Title: "远端操作", Content: "状态：未实际验证", Kind: "procedure", Revision: 1, CreatedAt: "2026-09-03T00:00:00Z", UpdatedAt: "2026-09-03T00:00:00Z"}
	procedure, err := store.ApplyRemoteNoteWithRelations(ctx, remote, "", material.SyncID)
	if err != nil {
		t.Fatal(err)
	}
	if procedure.DerivedFromID == nil || *procedure.DerivedFromID != material.ID {
		t.Fatalf("procedure=%#v material=%#v", procedure, material)
	}
	remote.Kind = "note"
	remote.SyncID = "remote-invalid-note-id"
	if _, err := store.ApplyRemoteNoteWithRelations(ctx, remote, "", material.SyncID); err == nil {
		t.Fatal("remote ordinary note accepted a source material relation")
	}
}

func TestMaterialLinksAndVerificationsSurviveBackup(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	firstMaterial, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "第一次对话", Content: "原始内容", Kind: "material"})
	if err != nil {
		t.Fatal(err)
	}
	secondMaterial, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "补充对话", Content: "更新来源", Kind: "material"})
	if err != nil {
		t.Fatal(err)
	}
	procedure, _, err := store.CreateNote(ctx, CreateNoteInput{Title: "查看 Docker 占用", Content: "docker system df -v", Kind: "procedure", DerivedFromID: &firstMaterial.ID})
	if err != nil {
		t.Fatal(err)
	}
	link, err := store.ApplyRemoteMaterialLink(ctx, NoteMaterialLink{SyncID: "material-link-sync-id-01", CreatedAt: "2026-09-03T01:00:00Z"}, procedure.SyncID, secondMaterial.SyncID)
	if err != nil || link.NoteID != procedure.ID || link.MaterialID != secondMaterial.ID {
		t.Fatalf("link=%#v err=%v", link, err)
	}
	verification, err := store.ApplyRemoteVerification(ctx, NoteVerification{SyncID: "verification-sync-id-001", NoteRevision: procedure.Revision, VerifiedAt: "2026-09-03T02:00:00Z", Environment: "Ubuntu 24.04 / Docker 27", Result: "success", Comment: "执行成功"}, procedure.SyncID)
	if err != nil || verification.NoteID != procedure.ID {
		t.Fatalf("verification=%#v err=%v", verification, err)
	}
	backup, err := store.BackupData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.MaterialLinks) != 1 || len(backup.Verifications) != 1 {
		t.Fatalf("backup links=%#v verifications=%#v", backup.MaterialLinks, backup.Verifications)
	}
	target := testStore(t)
	if err := target.RestoreBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	links, _ := target.ListMaterialLinks(ctx)
	verifications, _ := target.ListVerifications(ctx)
	if len(links) != 1 || len(verifications) != 1 || verifications[0].NoteRevision != procedure.Revision {
		t.Fatalf("restored links=%#v verifications=%#v", links, verifications)
	}
}

func TestTopicsAndTopicPinningSurviveBackup(t *testing.T) {
	noteStore := testStore(t)
	ctx := context.Background()
	procedure, _, err := noteStore.CreateNote(ctx, CreateNoteInput{Title: "重启 Nginx", Content: "systemctl restart nginx", Kind: "procedure"})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, _, err := noteStore.CreateNote(ctx, CreateNoteInput{Title: "故障记录", Content: "一次磁盘告警"})
	if err != nil {
		t.Fatal(err)
	}
	topic, err := noteStore.ApplyRemoteTopic(ctx, Topic{SyncID: "topic-sync-server-admin-01", Name: "服务器管理", CreatedAt: "2026-09-03T03:00:00Z", UpdatedAt: "2026-09-03T03:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	membership, err := noteStore.ApplyRemoteTopicMembership(ctx, TopicMembership{SyncID: "membership-sync-nginx-001", Pinned: true, CreatedAt: "2026-09-03T03:01:00Z", UpdatedAt: "2026-09-03T03:01:00Z"}, topic.SyncID, procedure.SyncID)
	if err != nil || membership.TopicID != topic.ID || membership.NoteID != procedure.ID || !membership.Pinned {
		t.Fatalf("membership=%#v err=%v", membership, err)
	}
	if _, err := noteStore.ApplyRemoteTopicMembership(ctx, TopicMembership{SyncID: "membership-sync-ordinary-01", Pinned: true, CreatedAt: "2026-09-03T03:02:00Z", UpdatedAt: "2026-09-03T03:02:00Z"}, topic.SyncID, ordinary.SyncID); err == nil {
		t.Fatal("ordinary note was pinned as a common operation")
	}

	backup, err := noteStore.BackupData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.Topics) != 1 || len(backup.TopicMemberships) != 1 {
		t.Fatalf("backup topics=%#v memberships=%#v", backup.Topics, backup.TopicMemberships)
	}
	target := testStore(t)
	if err := target.RestoreBackup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	topics, _ := target.ListTopics(ctx)
	memberships, _ := target.ListTopicMemberships(ctx)
	if len(topics) != 1 || topics[0].Name != "服务器管理" || len(memberships) != 1 || !memberships[0].Pinned {
		t.Fatalf("restored topics=%#v memberships=%#v", topics, memberships)
	}
	data, err := target.MarkdownExport(ctx)
	if err != nil || !strings.Contains(string(data), "主题：服务器管理") {
		t.Fatalf("markdown=%s err=%v", data, err)
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
