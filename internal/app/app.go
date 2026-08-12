package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"thoughtglean/internal/attachments"
	"thoughtglean/internal/mirror"
	"thoughtglean/internal/store"
)

type App struct {
	store          *store.Store
	mirror         *mirror.Mirror
	attachments    *attachments.Store
	passkey        *webauthn.WebAuthn
	ownerToken     []byte
	trustedOrigins map[string]struct{}
	handler        http.Handler
}

func (a *App) SetAttachmentStore(attachmentStore *attachments.Store) {
	a.attachments = attachmentStore
}

// SetOwnerToken enables the single-owner authentication mode used by the
// server-backed application. The token is exchanged for an HttpOnly cookie
// and is never stored in browser JavaScript storage.
func (a *App) SetOwnerToken(token string) { a.ownerToken = []byte(token) }

func (a *App) ConfigurePasskey(rpID, origin string) error {
	canonicalOrigin, err := normalizeOrigin(origin)
	if err != nil {
		return err
	}
	configured, err := webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: "ThoughtGlean", RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
		Timeouts: webauthn.TimeoutsConfig{
			Registration: webauthn.TimeoutConfig{Enforce: true},
			Login:        webauthn.TimeoutConfig{Enforce: true},
		}})
	if err != nil {
		return err
	}
	a.passkey = configured
	a.trustedOrigins[canonicalOrigin] = struct{}{}
	return nil
}

func (a *App) SetMarkdownMirror(markdownMirror *mirror.Mirror) {
	a.mirror = markdownMirror
}

func (a *App) SyncMarkdownMirror(ctx context.Context) error {
	if a.mirror == nil {
		return nil
	}
	return a.mirror.Sync(ctx, a.store)
}

func New(noteStore *store.Store) *App {
	app := &App{store: noteStore, trustedOrigins: make(map[string]struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", app.health)
	mux.HandleFunc("GET /api/sync/snapshot", app.syncSnapshot)
	mux.HandleFunc("POST /api/sync/apply", app.applyLocalSyncEvent)
	mux.HandleFunc("POST /api/sync/attachments", app.applyLocalSyncAttachment)
	mux.HandleFunc("GET /api/backup.json", app.downloadBackup)
	mux.HandleFunc("GET /api/backup.zip", app.downloadFullBackup)
	mux.HandleFunc("GET /api/export.md", app.downloadMarkdown)
	mux.HandleFunc("POST /api/backups/validate", app.validateBackup)
	mux.HandleFunc("POST /api/backups/restore", app.restoreBackup)
	mux.HandleFunc("POST /api/backups/restore.zip", app.restoreFullBackup)
	mux.HandleFunc("GET /api/auth/status", app.authStatus)
	mux.HandleFunc("POST /api/auth/token", app.loginWithOwnerToken)
	mux.HandleFunc("POST /api/auth/register/options", app.beginRegistration)
	mux.HandleFunc("POST /api/auth/register/verify", app.finishRegistration)
	mux.HandleFunc("POST /api/auth/login/options", app.beginLogin)
	mux.HandleFunc("POST /api/auth/login/verify", app.finishLogin)
	mux.HandleFunc("POST /api/auth/logout", app.logout)
	mux.HandleFunc("GET /api/auth/passkeys", app.listPasskeys)
	mux.HandleFunc("POST /api/auth/passkeys/options", app.beginAdditionalPasskey)
	mux.HandleFunc("POST /api/auth/passkeys/verify", app.finishAdditionalPasskey)
	mux.HandleFunc("DELETE /api/auth/passkeys/{id}", app.deletePasskey)
	mux.HandleFunc("GET /api/notes", app.listNotes)
	mux.HandleFunc("POST /api/notes", app.createNote)
	mux.HandleFunc("GET /api/notes/{id}", app.getNote)
	mux.HandleFunc("PATCH /api/notes/{id}", app.updateNote)
	mux.HandleFunc("DELETE /api/notes/{id}", app.deleteNote)
	mux.HandleFunc("POST /api/notes/{id}/restore", app.restoreNote)
	mux.HandleFunc("GET /api/notes/{id}/context", app.noteContext)
	mux.HandleFunc("GET /api/notes/{id}/source", app.getNoteSource)
	mux.HandleFunc("PUT /api/notes/{id}/source", app.setNoteSource)
	mux.HandleFunc("GET /api/notes/{id}/attachments", app.listAttachments)
	mux.HandleFunc("POST /api/notes/{id}/attachments", app.uploadAttachment)
	mux.HandleFunc("GET /api/attachments/{id}", app.serveAttachment)
	mux.HandleFunc("DELETE /api/attachments/{id}", app.deleteAttachment)
	app.handler = securityHeaders(app.sameOrigin(app.requireAuthentication(mux)))
	return app
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.handler.ServeHTTP(w, r)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// syncSnapshot returns the authoritative plaintext state for the owner's
// devices. A full snapshot keeps the contract intentionally simple: personal
// libraries are small, and the complete server state is easy to recover,
// inspect and evolve.
func (a *App) syncSnapshot(w http.ResponseWriter, r *http.Request) {
	backup, err := a.store.BackupData(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items, err := a.store.ListAllAttachments(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"generatedAt": backup.GeneratedAt,
		"notes":       backup.Notes,
		"sources":     backup.Sources,
		"attachments": items,
	})
}

func (a *App) applyLocalSyncEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind                string           `json:"kind"`
		Note                store.Note       `json:"note"`
		NoteSyncID          string           `json:"noteSyncId"`
		AttachmentSyncID    string           `json:"attachmentSyncId"`
		ContinuedFromSyncID string           `json:"continuedFromSyncId"`
		Source              store.NoteSource `json:"source"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if body.Kind == "source.upsert" {
		note, err := a.store.GetNoteBySyncID(r.Context(), body.NoteSyncID)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if _, err := a.store.SetNoteSource(r.Context(), note.ID, body.Source); err != nil {
			writeAPIError(w, err)
			return
		}
		if err := a.SyncMarkdownMirror(r.Context()); err != nil {
			writeMirrorError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if body.Kind == "attachment.delete" {
		attachment, err := a.store.GetAttachmentBySyncID(r.Context(), body.AttachmentSyncID)
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if err := a.store.DeleteAttachmentBySyncID(r.Context(), body.AttachmentSyncID); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, err)
			return
		}
		if a.attachments != nil {
			count, err := a.store.AttachmentReferenceCount(r.Context(), attachment.ContentHash)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			if count == 0 {
				if err := a.attachments.Delete(attachment.ContentHash); err != nil {
					log.Printf("remove unreferenced attachment %s: %v", attachment.ContentHash, err)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if body.Kind != "note.upsert" {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "unsupported sync event"})
		return
	}
	note, err := a.store.ApplyRemoteNoteWithParent(r.Context(), body.Note, body.ContinuedFromSyncID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": note})
}

func (a *App) applyLocalSyncAttachment(w http.ResponseWriter, r *http.Request) {
	if a.attachments == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "attachments are not configured"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachments.MaxImageSize+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid synced image"})
		return
	}
	note, err := a.store.GetNoteBySyncID(r.Context(), r.FormValue("noteSyncId"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "image file is required"})
		return
	}
	defer file.Close()
	saved, err := a.attachments.Save(file)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	item, err := a.store.AddAttachment(r.Context(), store.Attachment{SyncID: r.FormValue("syncId"), NoteID: note.ID, ContentHash: saved.Hash, OriginalName: header.Filename, MIMEType: header.Header.Get("Content-Type"), ByteSize: saved.Size})
	if err != nil {
		// The identical content may already be present after a repeated pull.
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": item})
}

func (a *App) authStatus(w http.ResponseWriter, r *http.Request) {
	configured, err := a.store.HasPasskey(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	authenticated, err := a.hasAuthenticatedSession(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": a.passkey != nil, "configured": configured, "authenticated": authenticated, "tokenLoginEnabled": len(a.ownerToken) != 0})
}

func (a *App) loginWithOwnerToken(w http.ResponseWriter, r *http.Request) {
	if len(a.ownerToken) == 0 {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "owner login is not configured"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	provided := []byte(body.Token)
	if len(provided) != len(a.ownerToken) || subtle.ConstantTimeCompare(provided, a.ownerToken) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "访问密钥不正确"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_session", Value: a.ownerSessionToken(), Path: "/", MaxAge: 30 * 24 * 60 * 60, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (a *App) ownerSessionToken() string {
	digest := sha256.Sum256(append(append([]byte(nil), a.ownerToken...), []byte("\x00ThoughtGlean owner session v1")...))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (a *App) hasOwnerSession(r *http.Request) bool {
	if len(a.ownerToken) == 0 {
		return false
	}
	cookie, err := r.Cookie("thoughtglean_session")
	if err != nil {
		return false
	}
	expected := a.ownerSessionToken()
	return len(cookie.Value) == len(expected) && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(expected)) == 1
}

func (a *App) hasAuthenticatedSession(r *http.Request) (bool, error) {
	if a.hasOwnerSession(r) {
		return true, nil
	}
	cookie, err := r.Cookie("thoughtglean_session")
	if err != nil {
		return false, nil
	}
	return a.store.ValidateAuthSession(r.Context(), cookie.Value)
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (a *App) beginRegistration(w http.ResponseWriter, r *http.Request) {
	if a.passkey == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "passkey is not configured"})
		return
	}
	configured, err := a.store.HasPasskey(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if configured {
		writeAPIError(w, &clientError{Status: http.StatusForbidden, Message: "a passkey is already registered"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	owner, err := a.store.Owner(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		if body.Name == "" {
			body.Name = "ThoughtGlean owner"
		}
		owner, err = a.store.CreateOwner(r.Context(), body.Name)
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	options, session, err := a.passkey.BeginRegistration(owner,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	ceremonyID, err := a.store.CreateAuthCeremony(r.Context(), "registration", *session)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremonyId": base64.RawURLEncoding.EncodeToString(ceremonyID), "publicKey": options.Response})
}

func (a *App) finishRegistration(w http.ResponseWriter, r *http.Request) {
	a.finishPasskeyRegistration(w, r, true)
}

func (a *App) finishPasskeyRegistration(w http.ResponseWriter, r *http.Request, createSession bool) {
	if a.passkey == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "passkey is not configured"})
		return
	}
	var body struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	ceremonyID, err := base64.RawURLEncoding.DecodeString(body.CeremonyID)
	if err != nil || len(ceremonyID) != 32 || len(body.Credential) == 0 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid registration response"})
		return
	}
	session, err := a.store.TakeAuthCeremony(r.Context(), ceremonyID, "registration")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	verification := r.Clone(r.Context())
	verification.Body = io.NopCloser(bytes.NewReader(body.Credential))
	verification.Header.Set("Content-Type", "application/json")
	credential, err := a.passkey.FinishRegistration(owner, session, verification)
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "passkey registration could not be verified"})
		return
	}
	if err := a.store.AddPasskeyCredential(r.Context(), owner, *credential); err != nil {
		writeAPIError(w, err)
		return
	}
	if createSession {
		token, err := a.store.CreateAuthSession(r.Context(), owner)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "thoughtglean_session", Value: token, Path: "/", MaxAge: 30 * 24 * 60 * 60, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r)})
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"registered": true})
}

func (a *App) beginAdditionalPasskey(w http.ResponseWriter, r *http.Request) {
	if a.passkey == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "passkey is not configured"})
		return
	}
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	options, session, err := a.passkey.BeginRegistration(owner, webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	ceremonyID, err := a.store.CreateAuthCeremony(r.Context(), "registration", *session)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremonyId": base64.RawURLEncoding.EncodeToString(ceremonyID), "publicKey": options.Response})
}

func (a *App) finishAdditionalPasskey(w http.ResponseWriter, r *http.Request) {
	a.finishPasskeyRegistration(w, r, false)
}

func (a *App) listPasskeys(w http.ResponseWriter, r *http.Request) {
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	credentials, err := a.store.ListPasskeyCredentials(r.Context(), owner)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	items := make([]map[string]string, 0, len(credentials))
	for _, credential := range credentials {
		items = append(items, map[string]string{"id": base64.RawURLEncoding.EncodeToString(credential.ID), "createdAt": credential.CreatedAt, "updatedAt": credential.UpdatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": items})
}

func (a *App) deletePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil || len(id) == 0 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid passkey id"})
		return
	}
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.store.DeletePasskeyCredential(r.Context(), owner, id); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) beginLogin(w http.ResponseWriter, r *http.Request) {
	if a.passkey == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "passkey is not configured"})
		return
	}
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	options, session, err := a.passkey.BeginLogin(owner, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	ceremonyID, err := a.store.CreateAuthCeremony(r.Context(), "login", *session)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ceremonyId": base64.RawURLEncoding.EncodeToString(ceremonyID), "publicKey": options.Response})
}

func (a *App) finishLogin(w http.ResponseWriter, r *http.Request) {
	if a.passkey == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "passkey is not configured"})
		return
	}
	var body struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	ceremonyID, err := base64.RawURLEncoding.DecodeString(body.CeremonyID)
	if err != nil || len(ceremonyID) != 32 || len(body.Credential) == 0 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid login response"})
		return
	}
	session, err := a.store.TakeAuthCeremony(r.Context(), ceremonyID, "login")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	owner, err := a.store.Owner(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	verification := r.Clone(r.Context())
	verification.Body = io.NopCloser(bytes.NewReader(body.Credential))
	verification.Header.Set("Content-Type", "application/json")
	credential, err := a.passkey.FinishLogin(owner, session, verification)
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusUnauthorized, Message: "passkey login could not be verified"})
		return
	}
	if err := a.store.UpdatePasskeyCredential(r.Context(), *credential); err != nil {
		writeAPIError(w, err)
		return
	}
	token, err := a.store.CreateAuthSession(r.Context(), owner)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_session", Value: token, Path: "/", MaxAge: 30 * 24 * 60 * 60, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: requestIsHTTPS(r)})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("thoughtglean_session"); err == nil {
		if !a.hasOwnerSession(r) {
			if err := a.store.DeleteAuthSession(r.Context(), cookie.Value); err != nil {
				writeAPIError(w, err)
				return
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) downloadBackup(w http.ResponseWriter, r *http.Request) {
	backup, err := a.store.BackupData(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="thoughtglean-backup.json"`)
	w.Header().Set("X-ThoughtGlean-Backup-SHA256", backup.Integrity.SHA256)
	if err := json.NewEncoder(w).Encode(backup); err != nil {
		log.Printf("write backup: %v", err)
	}
}

func (a *App) downloadFullBackup(w http.ResponseWriter, r *http.Request) {
	if a.attachments == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "attachments are not configured"})
		return
	}
	backup, err := a.store.BackupData(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachments, err := a.store.ListAllAttachments(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="thoughtglean-backup.zip"`)
	archive := zip.NewWriter(w)
	defer archive.Close()
	backup.Attachments = attachments
	if err := store.SealBackup(&backup); err != nil {
		return
	}
	manifest, err := archive.Create("backup.json")
	if err != nil {
		return
	}
	if err := json.NewEncoder(manifest).Encode(backup); err != nil {
		return
	}
	seen := make(map[string]bool)
	for _, attachment := range attachments {
		if seen[attachment.ContentHash] {
			continue
		}
		seen[attachment.ContentHash] = true
		file, err := a.attachments.Open(attachment.ContentHash)
		if err != nil {
			log.Printf("open backup attachment %s: %v", attachment.ContentHash, err)
			return
		}
		entry, err := archive.Create("attachments/" + attachment.ContentHash)
		if err == nil {
			_, err = io.Copy(entry, file)
		}
		file.Close()
		if err != nil {
			log.Printf("write backup attachment %s: %v", attachment.ContentHash, err)
			return
		}
	}
}

func (a *App) downloadMarkdown(w http.ResponseWriter, r *http.Request) {
	contents, err := a.store.MarkdownExport(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="thoughtglean-export.md"`)
	_, _ = w.Write(contents)
}

func (a *App) validateBackup(w http.ResponseWriter, r *http.Request) {
	backup, err := decodeBackup(w, r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupSummary(backup))
}

func (a *App) restoreBackup(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-ThoughtGlean-Confirm") != "replace" {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "explicit restore confirmation is required"})
		return
	}
	backup, err := decodeBackup(w, r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	attachments, err := a.store.ListAllAttachments(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if len(attachments) > 0 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "this library contains images; restore a complete ZIP backup instead"})
		return
	}
	if err := a.store.RestoreBackup(r.Context(), backup); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupSummary(backup))
}

func (a *App) restoreFullBackup(w http.ResponseWriter, r *http.Request) {
	if a.attachments == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "attachments are not configured"})
		return
	}
	if r.Header.Get("X-ThoughtGlean-Confirm") != "replace" {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "explicit restore confirmation is required"})
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/zip" {
		writeAPIError(w, &clientError{Status: http.StatusUnsupportedMediaType, Message: "Content-Type must be application/zip"})
		return
	}
	temporary, err := os.CreateTemp("", "thoughtglean-restore-*.zip")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if _, err := io.Copy(temporary, r.Body); err != nil {
		temporary.Close()
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid or oversized ZIP backup"})
		return
	}
	if err := temporary.Close(); err != nil {
		writeAPIError(w, err)
		return
	}
	archive, err := zip.OpenReader(temporaryName)
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid ZIP backup"})
		return
	}
	defer archive.Close()
	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		entries[entry.Name] = entry
	}
	manifest := entries["backup.json"]
	if manifest == nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "ZIP backup is missing backup.json"})
		return
	}
	manifestReader, err := manifest.Open()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	var backup store.Backup
	err = json.NewDecoder(manifestReader).Decode(&backup)
	manifestReader.Close()
	if err != nil || store.VerifyBackup(backup) != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "ZIP backup integrity check failed"})
		return
	}
	seen := make(map[string]bool)
	for _, attachment := range backup.Attachments {
		if seen[attachment.ContentHash] {
			continue
		}
		seen[attachment.ContentHash] = true
		entry := entries["attachments/"+attachment.ContentHash]
		if entry == nil {
			writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "ZIP backup is missing an image"})
			return
		}
		image, err := entry.Open()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		stored, saveErr := a.attachments.Save(image)
		image.Close()
		if saveErr != nil || stored.Hash != attachment.ContentHash {
			writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "ZIP backup image integrity check failed"})
			return
		}
	}
	if err := a.store.RestoreBackup(r.Context(), backup); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupSummary(backup))
}

func backupSummary(backup store.Backup) map[string]any {
	trashed := 0
	for _, note := range backup.Notes {
		if note.DeletedAt != nil {
			trashed++
		}
	}
	return map[string]any{
		"format": backup.Format, "version": backup.Version, "generatedAt": backup.GeneratedAt,
		"notes": len(backup.Notes), "revisions": len(backup.Revisions), "trashedNotes": trashed,
		"sha256": backup.Integrity.SHA256,
	}
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
		RequestID       string  `json:"requestId"`
		Title           string  `json:"title"`
		Content         string  `json:"content"`
		Starred         bool    `json:"starred"`
		ContinuedFromID *string `json:"continuedFromId"`
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
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
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
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
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
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
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
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
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

func (a *App) getNoteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	source, err := a.store.GetNoteSource(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"source": nil})
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source})
}

func (a *App) setNoteSource(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	var source store.NoteSource
	if err := decodeJSON(w, r, &source); err != nil {
		writeAPIError(w, err)
		return
	}
	updated, err := a.store.SetNoteSource(r.Context(), id, source)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.SyncMarkdownMirror(r.Context()); err != nil {
		writeMirrorError(w, err)
		return
	}
	if updated.NoteID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"source": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": updated})
}

func (a *App) listAttachments(w http.ResponseWriter, r *http.Request) {
	id, ok := noteID(w, r)
	if !ok {
		return
	}
	items, err := a.store.ListAttachments(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": items})
}

func (a *App) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	if a.attachments == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "attachments are not configured"})
		return
	}
	noteID, ok := noteID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachments.MaxImageSize+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid or oversized image upload"})
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "image file is required"})
		return
	}
	defer file.Close()
	stored, err := a.attachments.Save(file)
	if err != nil {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: err.Error()})
		return
	}
	item, err := a.store.AddAttachment(r.Context(), store.Attachment{NoteID: noteID, ContentHash: stored.Hash, OriginalName: header.Filename, MIMEType: stored.MIME, ByteSize: stored.Size})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"attachment": item})
}

func (a *App) serveAttachment(w http.ResponseWriter, r *http.Request) {
	if a.attachments == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	if len(id) < 16 || len(id) > 128 {
		http.NotFound(w, r)
		return
	}
	item, err := a.store.GetAttachment(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	file, err := a.attachments.Open(item.ContentHash)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", item.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": item.OriginalName}))
	http.ServeContent(w, r, item.OriginalName, time.Time{}, file)
}

func (a *App) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) < 16 || len(id) > 128 {
		writeAPIError(w, &clientError{Status: http.StatusBadRequest, Message: "invalid attachment id"})
		return
	}
	attachment, err := a.store.GetAttachment(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := a.store.DeleteAttachment(r.Context(), id); err != nil {
		writeAPIError(w, err)
		return
	}
	if a.attachments != nil {
		count, err := a.store.AttachmentReferenceCount(r.Context(), attachment.ContentHash)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if count == 0 {
			if err := a.attachments.Delete(attachment.ContentHash); err != nil {
				log.Printf("remove unreferenced attachment %s: %v", attachment.ContentHash, err)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func noteID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if len(id) < 16 || len(id) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid note id"})
		return "", false
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

func decodeBackup(w http.ResponseWriter, r *http.Request) (store.Backup, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return store.Backup{}, &clientError{Status: http.StatusUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	var backup store.Backup
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&backup); err != nil {
		return store.Backup{}, &clientError{Status: http.StatusBadRequest, Message: fmt.Sprintf("invalid backup JSON: %v", err)}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return store.Backup{}, &clientError{Status: http.StatusBadRequest, Message: "invalid backup JSON: request body must contain exactly one JSON value"}
	}
	if err := store.VerifyBackup(backup); err != nil {
		return store.Backup{}, &clientError{Status: http.StatusBadRequest, Message: fmt.Sprintf("backup validation failed: %v", err)}
	}
	return backup, nil
}

type clientError struct {
	Status  int
	Message string
}

func (e *clientError) Error() string { return e.Message }

func writeMirrorError(w http.ResponseWriter, err error) {
	log.Printf("Markdown mirror failed: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "记录已保存，但 Markdown 镜像未更新；请修复镜像目录后重试。",
	})
}

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

func normalizeOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid HTTP origin %q", value)
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func (a *App) originAllowed(origin string, r *http.Request) bool {
	canonical, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	requestScheme := "http"
	if requestIsHTTPS(r) {
		requestScheme = "https"
	}
	requestOrigin, err := normalizeOrigin(requestScheme + "://" + r.Host)
	if err == nil && canonical == requestOrigin {
		return true
	}
	_, trusted := a.trustedOrigins[canonical]
	return trusted
}

func (a *App) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete || r.Method == http.MethodPut {
			if origin := r.Header.Get("Origin"); origin != "" {
				if !a.originAllowed(origin, r) {
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

func (a *App) requireAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if isPublicAuthRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		authenticated, err := a.hasAuthenticatedSession(r)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if authenticated {
			next.ServeHTTP(w, r)
			return
		}
		configured, err := a.store.HasPasskey(r.Context())
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if !configured && len(a.ownerToken) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
	})
}

func isPublicAuthRoute(path string) bool {
	switch path {
	case "/api/auth/status", "/api/auth/token", "/api/auth/login/options", "/api/auth/login/verify", "/api/auth/logout":
		return true
	default:
		return false
	}
}
