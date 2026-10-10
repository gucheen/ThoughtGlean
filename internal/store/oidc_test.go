package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOIDCSessionPersistenceAndScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, err := s.CreateOIDCSession(ctx, "issuer/client/owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, check := range []struct {
		token, scope string
		want         bool
	}{{token, "issuer/client/owner", true}, {token, "other-issuer/client/owner", false}, {"wrong-token", "issuer/client/owner", false}} {
		valid, err := s.ValidateOIDCSession(ctx, check.token, check.scope)
		if err != nil || valid != check.want {
			t.Fatalf("session valid=%v want=%v err=%v", valid, check.want, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE oidc_sessions SET expires_at = ?`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if valid, err := s.ValidateOIDCSession(ctx, token, "issuer/client/owner"); err != nil || valid {
		t.Fatalf("expired session accepted: %v %v", valid, err)
	}
}

func TestOIDCLoginExpiryAndSingleUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := OIDCLogin{Nonce: "nonce", Verifier: "verifier", ReturnTo: "/notes/123"}
	if err := s.CreateOIDCLogin(ctx, "state", "scope", want); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeOIDCLogin(ctx, "state", "different-scope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong scope accepted: %v", err)
	}
	got, err := s.TakeOIDCLogin(ctx, "state", "scope")
	if err != nil || got != want {
		t.Fatalf("login: %#v %v", got, err)
	}
	if _, err := s.TakeOIDCLogin(ctx, "state", "scope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replay accepted: %v", err)
	}
	if err := s.CreateOIDCLogin(ctx, "expired", "scope", want); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE oidc_logins SET expires_at = ?`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakeOIDCLogin(ctx, "expired", "scope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired login accepted: %v", err)
	}
}
