package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"thoughtglean/internal/store"
	"thoughtglean/internal/webui"
)

type App struct {
	store   *store.Store
	handler http.Handler
}

func New(noteStore *store.Store) *App {
	app := &App{store: noteStore}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", app.health)
	mux.HandleFunc("GET /api/notes", app.listNotes)
	mux.HandleFunc("POST /api/notes", app.createNote)
	mux.HandleFunc("GET /api/notes/{id}", app.getNote)
	mux.HandleFunc("PATCH /api/notes/{id}", app.updateNote)
	mux.HandleFunc("DELETE /api/notes/{id}", app.deleteNote)
	mux.HandleFunc("POST /api/notes/{id}/restore", app.restoreNote)
	mux.HandleFunc("GET /api/notes/{id}/context", app.noteContext)
	mux.Handle("/", staticHandler(webui.Assets()))
	app.handler = securityHeaders(sameOrigin(mux))
	return app
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.handler.ServeHTTP(w, r)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) listNotes(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	notes, err := a.store.ListNotes(r.Context(), store.ListOptions{
		Query: r.URL.Query().Get("q"),
		View:  r.URL.Query().Get("view"),
		Limit: limit,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func (a *App) createNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID       string `json:"requestId"`
		Title           string `json:"title"`
		Content         string `json:"content"`
		Starred         bool   `json:"starred"`
		ContinuedFromID *int64 `json:"continuedFromId"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if len(body.RequestID) > 128 || len(body.Title) > 500 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "request id or title is too long"})
		return
	}
	note, duplicate, err := a.store.CreateNote(r.Context(), store.CreateNoteInput{
		RequestID: body.RequestID, Title: body.Title, Content: body.Content,
		Starred: body.Starred, ContinuedFromID: body.ContinuedFromID,
	})
	if err != nil {
		var conflict *store.IdempotencyConflictError
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code": "idempotency_conflict", "error": err.Error(), "currentNote": conflict.Current,
			})
			return
		}
		writeAPIError(w, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"note": note, "duplicate": duplicate})
}

func (a *App) getNote(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	note, err := a.store.GetNote(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *App) updateNote(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title            *string `json:"title"`
		Content          *string `json:"content"`
		Starred          *bool   `json:"starred"`
		ExpectedRevision int     `json:"expectedRevision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if body.Title == nil && body.Content == nil && body.Starred == nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "no changes supplied"})
		return
	}
	if body.Title != nil && len(*body.Title) > 500 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "title is too long"})
		return
	}
	note, err := a.store.UpdateNote(r.Context(), id, store.UpdateNoteInput{
		Title: body.Title, Content: body.Content, Starred: body.Starred, ExpectedRevision: body.ExpectedRevision,
	})
	if err != nil {
		var conflict *store.ConflictError
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code": "revision_conflict", "error": err.Error(), "currentNote": conflict.Current,
			})
			return
		}
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *App) deleteNote(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	note, err := a.store.DeleteNote(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *App) restoreNote(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	note, err := a.store.RestoreNote(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *App) noteContext(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	context, err := a.store.NoteContext(r.Context(), id, count)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, context)
}

func noteID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid note id"})
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &clientError{Status: http.StatusUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return &clientError{Status: http.StatusBadRequest, Message: fmt.Sprintf("invalid JSON: %v", err)}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &clientError{Status: http.StatusBadRequest, Message: "invalid JSON: request body must contain exactly one JSON value"}
	}
	return nil
}

type clientError struct {
	Status  int
	Message string
}

func (e *clientError) Error() string { return e.Message }

func writeAPIError(w http.ResponseWriter, err error) {
	var requestErr *clientError
	var validationErr *store.ValidationError
	switch {
	case errors.As(err, &requestErr):
		writeJSON(w, requestErr.Status, map[string]string{"error": requestErr.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.As(err, &validationErr):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": validationErr.Error()})
	default:
		log.Printf("request failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete || r.Method == http.MethodPut {
			if origin := r.Header.Get("Origin"); origin != "" {
				parsed, err := url.Parse(origin)
				requestScheme := "http"
				if r.TLS != nil {
					requestScheme = "https"
				}
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
					!strings.EqualFold(parsed.Scheme, requestScheme) || !strings.EqualFold(parsed.Host, r.Host) {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin writes are not allowed"})
					return
				}
			} else if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin writes are not allowed"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func staticHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
