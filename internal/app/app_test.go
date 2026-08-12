package app

import (
	"bytes"
	"context"
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
	return New(noteStore)
}

func TestHealthIncludesServerVersion(t *testing.T) {
	app := testApp(t)
	app.SetVersion("test-build")
	response := requestJSON(t, app, http.MethodGet, "/api/health", nil)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"version":"test-build"`)) {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
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

func TestOwnerTokenProtectsPlaintextSync(t *testing.T) {
	app := testApp(t)
	app.SetOwnerToken("personal-test-token")

	blocked := requestJSON(t, app, http.MethodGet, "/api/sync/snapshot", nil)
	if blocked.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed snapshot status=%d body=%s", blocked.Code, blocked.Body.String())
	}
	wrong := requestJSON(t, app, http.MethodPost, "/api/auth/token", map[string]string{"token": "wrong"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status=%d", wrong.Code)
	}
	login := requestJSON(t, app, http.MethodPost, "/api/auth/token", map[string]string{"token": "personal-test-token"})
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%v body=%s", login.Code, login.Result().Cookies(), login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	note := store.Note{ID: "browser-local-id-00001", SyncID: "browser-sync-id-000001", Title: "服务端同步", Content: "明文同步内容", Revision: 1, CreatedAt: "2026-08-11T00:00:00Z", UpdatedAt: "2026-08-11T00:00:00Z"}
	applyRequest := httptest.NewRequest(http.MethodPost, "/api/sync/apply", bytes.NewBufferString(mustJSON(t, map[string]any{"kind": "note.upsert", "note": note})))
	applyRequest.Header.Set("Content-Type", "application/json")
	applyRequest.AddCookie(cookie)
	applied := httptest.NewRecorder()
	app.ServeHTTP(applied, applyRequest)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body.String())
	}
	snapshotRequest := httptest.NewRequest(http.MethodGet, "/api/sync/snapshot", nil)
	snapshotRequest.AddCookie(cookie)
	snapshot := httptest.NewRecorder()
	app.ServeHTTP(snapshot, snapshotRequest)
	if snapshot.Code != http.StatusOK || !bytes.Contains(snapshot.Body.Bytes(), []byte("明文同步内容")) {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
}

func TestPasskeySessionAlsoAuthenticatesOwnerTokenMode(t *testing.T) {
	app := testApp(t)
	app.SetOwnerToken("personal-test-token")
	owner, err := app.store.CreateOwner(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddPasskeyCredential(context.Background(), owner, webauthn.Credential{ID: []byte("passkey-credential")}); err != nil {
		t.Fatal(err)
	}
	token, err := app.store.CreateAuthSession(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/sync/snapshot", nil)
	request.AddCookie(&http.Cookie{Name: "thoughtglean_session", Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("passkey session status=%d body=%s", response.Code, response.Body.String())
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
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

func TestAPIServerDoesNotServeWebUI(t *testing.T) {
	app := testApp(t)
	response := requestJSON(t, app, http.MethodGet, "/", nil)
	if response.Code != http.StatusNotFound || response.Header().Get("Content-Security-Policy") == "" {
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
		"non HTTP origin":     func(request *http.Request) { request.Header.Set("Origin", "ftp://example.com") },
		"different HTTP host": func(request *http.Request) { request.Header.Set("Origin", "https://evil.example") },
		"cross-site fetch":    func(request *http.Request) { request.Header.Set("Sec-Fetch-Site", "cross-site") },
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

func TestConfiguredOriginCanWriteThroughDevelopmentProxy(t *testing.T) {
	app := testApp(t)
	if err := app.ConfigurePasskey("localhost", "http://localhost:5173"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/notes", bytes.NewBufferString(`{"content":"正文"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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
