package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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

	updated := requestJSON(t, app, http.MethodPatch, "/api/notes/1", map[string]any{
		"content": "记下失败时先保住原始数据和 SQLite", "expectedRevision": createdBody.Note.Revision,
	})
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	conflict := requestJSON(t, app, http.MethodPatch, "/api/notes/1", map[string]any{
		"content": "旧窗口的内容", "expectedRevision": createdBody.Note.Revision,
	})
	if conflict.Code != http.StatusConflict || !bytes.Contains(conflict.Body.Bytes(), []byte("currentNote")) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	search := requestJSON(t, app, http.MethodGet, "/api/notes?q=失败%20SQLite", nil)
	if search.Code != http.StatusOK || !bytes.Contains(search.Body.Bytes(), []byte("SQLite")) {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
	deleted := requestJSON(t, app, http.MethodDelete, "/api/notes/1", nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	trash := requestJSON(t, app, http.MethodGet, "/api/notes?view=trash", nil)
	if trash.Code != http.StatusOK || !bytes.Contains(trash.Body.Bytes(), []byte("原始数据")) {
		t.Fatalf("trash status=%d body=%s", trash.Code, trash.Body.String())
	}
	restored := requestJSON(t, app, http.MethodPost, "/api/notes/1/restore", map[string]any{})
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
