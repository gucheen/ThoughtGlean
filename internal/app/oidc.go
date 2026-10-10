package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"thoughtglean/internal/store"
)

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	OwnerSubject string
}

type oidcLogin struct {
	oauth        oauth2.Config
	verifier     *oidc.IDTokenVerifier
	client       *http.Client
	ownerSubject string
	scope        string
	secure       bool
}

func validateOIDCURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid OIDC URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, fmt.Errorf("OIDC URLs require HTTPS except on loopback hosts")
	}
	return u, nil
}

func (a *App) ConfigureOIDC(ctx context.Context, config OIDCConfig) error {
	if strings.TrimSpace(config.ClientID) == "" || strings.TrimSpace(config.ClientSecret) == "" || strings.TrimSpace(config.OwnerSubject) == "" {
		return fmt.Errorf("OIDC client ID, client secret and owner subject are required")
	}
	if _, err := validateOIDCURL(config.Issuer); err != nil {
		return fmt.Errorf("issuer: %w", err)
	}
	redirect, err := validateOIDCURL(config.RedirectURL)
	if err != nil || redirect.Path != "/api/auth/oidc/callback" {
		return fmt.Errorf("OIDC redirect URL must be an HTTP(S) URL ending in /api/auth/oidc/callback")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, config.Issuer)
	if err != nil {
		return fmt.Errorf("discover OIDC provider: %w", err)
	}
	endpoint := provider.Endpoint()
	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return fmt.Errorf("read OIDC provider metadata: %w", err)
	}
	for _, value := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKSURL} {
		if _, err := validateOIDCURL(value); err != nil {
			return fmt.Errorf("OIDC provider endpoint: %w", err)
		}
	}
	identity, _ := json.Marshal([]string{config.Issuer, config.ClientID, config.OwnerSubject})
	scopeHash := sha256.Sum256(identity)
	a.oidc = &oidcLogin{
		oauth:    oauth2.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, Endpoint: endpoint, RedirectURL: config.RedirectURL, Scopes: []string{oidc.ScopeOpenID}},
		verifier: provider.Verifier(&oidc.Config{ClientID: config.ClientID}),
		client:   client, ownerSubject: config.OwnerSubject,
		scope: base64.RawURLEncoding.EncodeToString(scopeHash[:]), secure: redirect.Scheme == "https",
	}
	origin, _ := normalizeOrigin(redirect.Scheme + "://" + redirect.Host)
	a.trustedOrigins[origin] = struct{}{}
	return nil
}

func (a *App) sessionIsSecure(r *http.Request) bool {
	return requestIsHTTPS(r) || (a.oidc != nil && a.oidc.secure)
}

func oidcReturnTo(value string) string {
	u, err := url.Parse(value)
	if err != nil || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n") || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "/api/") {
		return "/"
	}
	return value
}

func (a *App) beginOIDCLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.oidc == nil {
		writeAPIError(w, &clientError{Status: http.StatusServiceUnavailable, Message: "OIDC is not configured"})
		return
	}
	state, nonce, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	login := store.OIDCLogin{Nonce: nonce, Verifier: verifier, ReturnTo: oidcReturnTo(r.URL.Query().Get("returnTo"))}
	if err := a.store.CreateOIDCLogin(r.Context(), state, a.oidc.scope, login); err != nil {
		writeAPIError(w, err)
		return
	}
	// Lax allows the browser binding cookie on the identity provider's GET callback.
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_oidc_state", Value: state, Path: "/api/auth/oidc", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.sessionIsSecure(r)})
	http.Redirect(w, r, a.oidc.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

func (a *App) finishOIDCLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_oidc_state", Value: "", Path: "/api/auth/oidc", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.sessionIsSecure(r)})
	fail := func() { http.Redirect(w, r, "/?auth_error=oidc_failed", http.StatusSeeOther) }
	if a.oidc == nil {
		fail()
		return
	}
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("thoughtglean_oidc_state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		fail()
		return
	}
	login, err := a.store.TakeOIDCLogin(r.Context(), state, a.oidc.scope)
	if err != nil || r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, a.oidc.client)
	token, err := a.oidc.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(login.Verifier))
	if err != nil {
		log.Printf("OIDC token exchange failed")
		fail()
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		fail()
		return
	}
	idToken, err := a.oidc.verifier.Verify(ctx, rawIDToken)
	if err != nil || idToken.Nonce != login.Nonce || idToken.Subject != a.oidc.ownerSubject {
		fail()
		return
	}
	session, err := a.store.CreateOIDCSession(ctx, a.oidc.scope)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "thoughtglean_session", Value: session, Path: "/", MaxAge: 30 * 24 * 60 * 60, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.sessionIsSecure(r)})
	http.Redirect(w, r, oidcReturnTo(login.ReturnTo), http.StatusSeeOther)
}
