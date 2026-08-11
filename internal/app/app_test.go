package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
	"thoughtglean/internal/store"
)

func testApp(t *testing.T) *App {
	t.Helper()
	noteStore, err := store.Open(filepath.Join(t.TempDir(), "thoughtglean.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { noteStore.Close() })
	app := New(noteStore)
	app.SetRelayEnrollmentToken("test-enrollment")
	return app
}

func TestPasskeyConfiguredProtectsDataAPIs(t *testing.T) {
	app := testApp(t)
	owner, err := app.store.CreateOwner(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddPasskeyCredential(context.Background(), owner, webauthn.Credential{ID: []byte("credential")}); err != nil {
		t.Fatal(err)
	}
	blocked := requestJSON(t, app, http.MethodGet, "/api/notes", nil)
	if blocked.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	token, err := app.store.CreateAuthSession(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/notes", nil)
	req.AddCookie(&http.Cookie{Name: "thoughtglean_session", Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("authed status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRelayOnlyRejectsPlaintextAndUIRoutes(t *testing.T) {
	app := testApp(t)
	app.SetRelayOnly(true)
	for _, path := range []string{"/", "/api/notes", "/api/backup.json", "/api/auth/status", "/api/sync/local/events"} {
		response := requestJSON(t, app, http.MethodGet, path, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("relay-only %s status=%d", path, response.Code)
		}
	}
	if response := requestJSON(t, app, http.MethodGet, "/api/health", nil); response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
}

func requestJSON(t *testing.T, app *App, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, req)
	return response
}

func TestNoteLifecycleAPI(t *testing.T) {
	app := testApp(t)
	created := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{
		"requestId": "test-1", "content": "记下失败时先保住原始数据",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var createdBody struct {
		Note store.Note `json:"note"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}

	notePath := "/api/notes/" + createdBody.Note.ID
	updated := requestJSON(t, app, http.MethodPatch, notePath, map[string]any{
		"content": "记下失败时先保住原始数据和 SQLite", "expectedRevision": createdBody.Note.Revision,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	conflict := requestJSON(t, app, http.MethodPatch, notePath, map[string]any{
		"content": "旧窗口的内容", "expectedRevision": createdBody.Note.Revision,
	})
	if conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte("currentNote")) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	search := requestJSON(t, app, http.MethodGet, "/api/notes?q=失败%20SQLite", nil)
	if search.Code != http.StatusOK || !bytes.Contains(search.Body.Bytes(), []byte("SQLite")) {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
	deleted := requestJSON(t, app, http.MethodDelete, notePath, nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	trash := requestJSON(t, app, http.MethodGet, "/api/notes?view=trash", nil)
	if trash.Code != http.StatusOK || !bytes.Contains(trash.Body.Bytes(), []byte("原始数据")) {
		t.Fatalf("trash status=%d body=%s", trash.Code, trash.Body.String())
	}
	restored := requestJSON(t, app, http.MethodPost, notePath+"/restore", map[string]any{})
	if restored.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restored.Code, restored.Body.String())
	}
}

func TestStaticAndSecurityHeaders(t *testing.T) {
	app := testApp(t)
	response := requestJSON(t, app, http.MethodGet, "/", nil)
	if response.Code != http.StatusOK || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
}

func TestCreateIdempotencyConflictAPI(t *testing.T) {
	app := testApp(t)
	first := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{
		"requestId": "same-request", "content": "最初送出的正文",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	duplicate := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{
		"requestId": "same-request", "content": "最初送出的正文",
	})
	if duplicate.Code != http.StatusOK || !bytes.Contains(duplicate.Body.Bytes(), []byte(`"duplicate":true`)) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	conflict := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{
		"requestId": "same-request", "content": "保存途中继续写的新正文",
	})
	if conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte(`"code":"idempotency_conflict"`)) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestJSONRequestBoundaries(t *testing.T) {
	app := testApp(t)

	trailingRequest := httptest.NewRequest(http.MethodPost, "/api/notes", bytes.NewBufferString(
		`{"content":"第一段"}{"content":"第二段"}`,
	))
	trailingRequest.Header.Set("Content-Type", "application/json")
	trailingResponse := httptest.NewRecorder()
	app.ServeHTTP(trailingResponse, trailingRequest)
	if trailingResponse.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d body=%s", trailingResponse.Code, trailingResponse.Body.String())
	}

	plainRequest := httptest.NewRequest(http.MethodPost, "/api/notes", bytes.NewBufferString(`{"content":"正文"}`))
	plainRequest.Header.Set("Content-Type", "text/plain")
	plainResponse := httptest.NewRecorder()
	app.ServeHTTP(plainResponse, plainRequest)
	if plainResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("plain status=%d body=%s", plainResponse.Code, plainResponse.Body.String())
	}
}

func TestInternalErrorsAreHidden(t *testing.T) {
	app := testApp(t)
	if err := app.store.Close(); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, app, http.MethodGet, "/api/notes", nil)
	if response.Code != http.StatusInternalServerError || response.Body.String() != "{\"error\":\"internal server error\"}\n" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCrossOriginWriteChecksSchemeAndFetchMetadata(t *testing.T) {
	app := testApp(t)
	for name, configure := range map[string]func(*http.Request){
		"non HTTP origin":  func(request *http.Request) { request.Header.Set("Origin", "ftp://example.com") },
		"cross-site fetch": func(request *http.Request) { request.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/notes", bytes.NewBufferString(`{"content":"正文"}`))
			request.Header.Set("Content-Type", "application/json")
			configure(request)
			response := httptest.NewRecorder()
			app.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEncryptedSyncRelayStoresOnlyOpaqueOperations(t *testing.T) {
	app := testApp(t)
	vaultID := "pDha3W1XR7DpM0-VJwC1xA"
	token := bytes.Repeat([]byte{7}, 32)
	credentials := func(request *http.Request) {
		request.Header.Set("X-ThoughtGlean-Vault", vaultID)
		request.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(token))
		request.Header.Set("X-ThoughtGlean-Enrollment", "test-enrollment")
	}
	claim := httptest.NewRequest(http.MethodPost, "/api/sync/v1/vaults", nil)
	credentials(claim)
	claimed := httptest.NewRecorder()
	app.ServeHTTP(claimed, claim)
	if claimed.Code != http.StatusNoContent {
		t.Fatalf("claim status=%d body=%s", claimed.Code, claimed.Body.String())
	}

	push := httptest.NewRequest(http.MethodPost, "/api/sync/v1/operations", bytes.NewBufferString(`{"operations":[{"operationId":"device-000000001","ciphertext":"eyJub3RlIjoiY2lwaGVydGV4dCJ9","createdAt":"2026-08-11T00:00:00Z"}]}`))
	push.Header.Set("Content-Type", "application/json")
	push.Header.Set("Origin", "https://local.example") // Sync relay intentionally permits the configured bearer flow cross-origin.
	credentials(push)
	pushed := httptest.NewRecorder()
	app.ServeHTTP(pushed, push)
	if pushed.Code != http.StatusCreated || !bytes.Contains(pushed.Body.Bytes(), []byte("ciphertext")) {
		t.Fatalf("push status=%d body=%s", pushed.Code, pushed.Body.String())
	}

	pull := httptest.NewRequest(http.MethodGet, "/api/sync/v1/operations?after=0", nil)
	credentials(pull)
	pulled := httptest.NewRecorder()
	app.ServeHTTP(pulled, pull)
	if pulled.Code != http.StatusOK || !bytes.Contains(pulled.Body.Bytes(), []byte("eyJub3RlIjoiY2lwaGVydGV4dCJ9")) {
		t.Fatalf("pull status=%d body=%s", pulled.Code, pulled.Body.String())
	}
	subscribe := httptest.NewRequest(http.MethodGet, "/api/sync/v1/subscribe?after=0", nil)
	credentials(subscribe)
	subscribed := httptest.NewRecorder()
	app.ServeHTTP(subscribed, subscribe)
	if subscribed.Code != http.StatusOK || !bytes.Contains(subscribed.Body.Bytes(), []byte("nextCursor")) {
		t.Fatalf("subscribe status=%d body=%s", subscribed.Code, subscribed.Body.String())
	}
	if count, err := app.store.SyncOperationCount(context.Background(), vaultID); err != nil || count != 1 {
		t.Fatalf("stored operations count=%d err=%v", count, err)
	}

	wrongToken := httptest.NewRequest(http.MethodGet, "/api/sync/v1/operations", nil)
	wrongToken.Header.Set("X-ThoughtGlean-Vault", vaultID)
	wrongToken.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	blocked := httptest.NewRecorder()
	app.ServeHTTP(blocked, wrongToken)
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("wrong token status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	blob := httptest.NewRequest(http.MethodPut, "/api/sync/v1/blobs/attachment-blob-000001", bytes.NewBufferString("opaque-encrypted-image"))
	credentials(blob)
	blobResponse := httptest.NewRecorder()
	app.ServeHTTP(blobResponse, blob)
	if blobResponse.Code != http.StatusNoContent {
		t.Fatalf("blob put status=%d body=%s", blobResponse.Code, blobResponse.Body.String())
	}
	blobGet := httptest.NewRequest(http.MethodGet, "/api/sync/v1/blobs/attachment-blob-000001", nil)
	credentials(blobGet)
	blobRead := httptest.NewRecorder()
	app.ServeHTTP(blobRead, blobGet)
	if blobRead.Code != http.StatusOK || blobRead.Body.String() != "opaque-encrypted-image" {
		t.Fatalf("blob get status=%d body=%s", blobRead.Code, blobRead.Body.String())
	}
}

func TestRelayEnrollmentProtectsNewVaultClaims(t *testing.T) {
	app := testApp(t)
	request := httptest.NewRequest(http.MethodPost, "/api/sync/v1/vaults", nil)
	request.Header.Set("X-ThoughtGlean-Vault", "pDha3W1XR7DpM0-VJwC1xA")
	request.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	blocked := httptest.NewRecorder()
	app.ServeHTTP(blocked, request)
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("unenrolled new vault status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	request.Header.Set("X-ThoughtGlean-Enrollment", "test-enrollment")
	accepted := httptest.NewRecorder()
	app.ServeHTTP(accepted, request)
	if accepted.Code != http.StatusNoContent {
		t.Fatalf("enrolled new vault status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

func TestLocalSyncEventsAreQueuedAndRemoteNoteCanBeApplied(t *testing.T) {
	app := testApp(t)
	created := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{"content": "等待加密同步"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	events := requestJSON(t, app, http.MethodGet, "/api/sync/local/events", nil)
	if events.Code != http.StatusOK || !bytes.Contains(events.Body.Bytes(), []byte("note.upsert")) {
		t.Fatalf("events status=%d body=%s", events.Code, events.Body.String())
	}
	var source struct {
		Note store.Note `json:"note"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	remote := source.Note
	remote.SyncID = "remote-note-sync-id-01"
	remote.Content = "来自另一台设备"
	remote.CreatedAt, remote.UpdatedAt = "2026-08-11T00:00:00Z", "2026-08-11T00:00:01Z"
	applied := requestJSON(t, app, http.MethodPost, "/api/sync/local/apply", map[string]any{"kind": "note.upsert", "note": remote})
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body.String())
	}
	notes := requestJSON(t, app, http.MethodGet, "/api/notes", nil)
	if notes.Code != http.StatusOK || !bytes.Contains(notes.Body.Bytes(), []byte("来自另一台设备")) {
		t.Fatalf("notes status=%d body=%s", notes.Code, notes.Body.String())
	}
}

func TestBackupAndMarkdownDownloads(t *testing.T) {
	app := testApp(t)
	created := requestJSON(t, app, http.MethodPost, "/api/notes", map[string]any{
		"title": "可导出的标题", "content": "这是完整备份中的正文",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	backup := requestJSON(t, app, http.MethodGet, "/api/backup.json", nil)
	if backup.Code != http.StatusOK || backup.Header().Get("Content-Disposition") == "" || backup.Header().Get("X-ThoughtGlean-Backup-SHA256") == "" {
		t.Fatalf("backup status=%d headers=%v", backup.Code, backup.Header())
	}
	var snapshot store.Backup
	if err := json.Unmarshal(backup.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if backup.Header().Get("X-ThoughtGlean-Backup-SHA256") != snapshot.Integrity.SHA256 {
		t.Fatalf("checksum header does not match backup body")
	}
	if err := store.VerifyBackup(snapshot); err != nil {
		t.Fatalf("downloaded backup is invalid: %v", err)
	}

	markdown := requestJSON(t, app, http.MethodGet, "/api/export.md", nil)
	if markdown.Code != http.StatusOK || markdown.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || !bytes.Contains(markdown.Body.Bytes(), []byte("可导出的标题")) {
		t.Fatalf("markdown status=%d headers=%v body=%s", markdown.Code, markdown.Header(), markdown.Body.String())
	}
}

func TestBackupRestoreRequiresConfirmationAndReplacesData(t *testing.T) {
	source := testApp(t)
	if response := requestJSON(t, source, http.MethodPost, "/api/notes", map[string]any{"content": "恢复过来的记录"}); response.Code != http.StatusCreated {
		t.Fatalf("source create status=%d", response.Code)
	}
	backup := requestJSON(t, source, http.MethodGet, "/api/backup.json", nil)
	if backup.Code != http.StatusOK {
		t.Fatalf("backup status=%d", backup.Code)
	}

	target := testApp(t)
	if response := requestJSON(t, target, http.MethodPost, "/api/notes", map[string]any{"content": "将被替换"}); response.Code != http.StatusCreated {
		t.Fatalf("target create status=%d", response.Code)
	}
	validate := requestJSON(t, target, http.MethodPost, "/api/backups/validate", json.RawMessage(backup.Body.Bytes()))
	if validate.Code != http.StatusOK || !bytes.Contains(validate.Body.Bytes(), []byte(`"notes":1`)) {
		t.Fatalf("validate status=%d body=%s", validate.Code, validate.Body.String())
	}
	restore := requestJSON(t, target, http.MethodPost, "/api/backups/restore", json.RawMessage(backup.Body.Bytes()))
	if restore.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed restore status=%d body=%s", restore.Code, restore.Body.String())
	}

	restoreRequest := httptest.NewRequest(http.MethodPost, "/api/backups/restore", bytes.NewReader(backup.Body.Bytes()))
	restoreRequest.Header.Set("Content-Type", "application/json")
	restoreRequest.Header.Set("X-ThoughtGlean-Confirm", "replace")
	restoreResponse := httptest.NewRecorder()
	target.ServeHTTP(restoreResponse, restoreRequest)
	if restoreResponse.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restoreResponse.Code, restoreResponse.Body.String())
	}
	notes := requestJSON(t, target, http.MethodGet, "/api/notes", nil)
	if notes.Code != http.StatusOK || !bytes.Contains(notes.Body.Bytes(), []byte("恢复过来的记录")) || bytes.Contains(notes.Body.Bytes(), []byte("将被替换")) {
		t.Fatalf("notes after restore status=%d body=%s", notes.Code, notes.Body.String())
	}
}
