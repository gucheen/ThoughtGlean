package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

type OIDCLogin struct {
	Nonce    string
	Verifier string
	ReturnTo string
}

func authTokenHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

func (s *Store) CreateOIDCLogin(ctx context.Context, state, scope string, login OIDCLogin) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oidc_logins WHERE expires_at <= ?`, time.Now().Unix()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_logins (state_hash, scope, nonce, verifier, return_to, expires_at) VALUES (?, ?, ?, ?, ?, ?)`, authTokenHash(state), scope, login.Nonce, login.Verifier, login.ReturnTo, time.Now().Add(10*time.Minute).Unix())
	return err
}

// TakeOIDCLogin consumes the attempt before token exchange, so callbacks cannot be replayed.
func (s *Store) TakeOIDCLogin(ctx context.Context, state, scope string) (OIDCLogin, error) {
	var login OIDCLogin
	var expires int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM oidc_logins WHERE state_hash = ? AND scope = ? RETURNING nonce, verifier, return_to, expires_at`, authTokenHash(state), scope).Scan(&login.Nonce, &login.Verifier, &login.ReturnTo, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires <= time.Now().Unix()) {
		return OIDCLogin{}, ErrNotFound
	}
	return login, err
}

func (s *Store) CreateOIDCSession(ctx context.Context, scope string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oidc_sessions WHERE expires_at <= ?`, time.Now().Unix()); err != nil {
		return "", err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_sessions (token_hash, scope, expires_at) VALUES (?, ?, ?)`, authTokenHash(token), scope, time.Now().Add(30*24*time.Hour).Unix())
	return token, err
}

func (s *Store) ValidateOIDCSession(ctx context.Context, token, scope string) (bool, error) {
	var valid bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM oidc_sessions WHERE token_hash = ? AND scope = ? AND expires_at > ?)`, authTokenHash(token), scope, time.Now().Unix()).Scan(&valid)
	return valid, err
}

func (s *Store) DeleteOIDCSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oidc_sessions WHERE token_hash = ?`, authTokenHash(token))
	return err
}
