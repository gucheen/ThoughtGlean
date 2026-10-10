package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type oidcAttempt struct {
	state  string
	nonce  string
	pkce   string
	cookie *http.Cookie
}

func oidcTestApp(t *testing.T) (*App, *oidcAttempt) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	attempt := &oidcAttempt{}
	var issuer string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, http.StatusOK, map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			id, secret, _ := r.BasicAuth()
			if id != "test-client" || secret != "test-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "http://localhost:5173/api/auth/oidc/callback" || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != attempt.pkce {
				t.Errorf("invalid authorization code exchange")
				http.Error(w, "invalid exchange", http.StatusBadRequest)
				return
			}
			code := r.Form.Get("code")
			if code == "exchange-failure" {
				http.Error(w, "unavailable", http.StatusBadRequest)
				return
			}
			claims := map[string]any{"iss": issuer, "aud": "test-client", "sub": "owner", "nonce": attempt.nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
			switch code {
			case "wrong-subject":
				claims["sub"] = "someone-else"
			case "wrong-nonce":
				claims["nonce"] = "wrong"
			case "wrong-issuer":
				claims["iss"] = "https://other.example"
			case "wrong-audience":
				claims["aud"] = "other-client"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			}
			payload, _ := json.Marshal(claims)
			header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
			unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
			digest := sha256.Sum256([]byte(unsigned))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Error(err)
			}
			if code == "bad-signature" {
				signature[0] ^= 1
			}
			tokens := map[string]string{"access_token": "private-access-token", "token_type": "Bearer", "id_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)}
			if code == "missing-id-token" {
				delete(tokens, "id_token")
			}
			writeJSON(w, http.StatusOK, tokens)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	issuer = provider.URL
	a := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = a.ConfigureOIDC(ctx, OIDCConfig{Issuer: issuer, ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "http://localhost:5173/api/auth/oidc/callback", OwnerSubject: "owner"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	return a, attempt
}

func startOIDCTest(t *testing.T, a *App, attempt *oidcAttempt, returnTo string) {
	t.Helper()
	response := requestJSON(t, a, http.MethodGet, "/api/auth/oidc/login?"+url.Values{"returnTo": {returnTo}}.Encode(), nil)
	if response.Code != http.StatusFound {
		t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := location.Query()
	attempt.state, attempt.nonce, attempt.pkce = q.Get("state"), q.Get("nonce"), q.Get("code_challenge")
	attempt.cookie = response.Result().Cookies()[0]
	if attempt.state == "" || attempt.nonce == "" || attempt.pkce == "" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid" || !attempt.cookie.HttpOnly || attempt.cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe login attempt: %v %v", q, attempt.cookie)
	}
}

func finishOIDCTest(a *App, attempt *oidcAttempt, code string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?"+url.Values{"state": {attempt.state}, "code": {code}}.Encode(), nil)
	if attempt.cookie != nil {
		r.AddCookie(attempt.cookie)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func TestOIDCLoginSessionAndLogout(t *testing.T) {
	a, attempt := oidcTestApp(t)
	if got := requestJSON(t, a, http.MethodGet, "/api/sync/snapshot", nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected data: %d", got.Code)
	}
	startOIDCTest(t, a, attempt, "/spaces/personal?view=all#note")
	response := finishOIDCTest(a, attempt, "valid")
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/spaces/personal?view=all#note" {
		t.Fatalf("callback: %d %s", response.Code, response.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "thoughtglean_session" {
			session = cookie
		}
	}
	if session == nil || !session.HttpOnly || session.MaxAge != 30*24*60*60 || strings.Contains(session.Value, "private") {
		t.Fatalf("invalid session: %v", session)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/sync/snapshot", nil)
	r.AddCookie(session)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated snapshot: %d %s", w.Code, w.Body.String())
	}
	if replay := finishOIDCTest(a, attempt, "valid"); replay.Header().Get("Location") != "/?auth_error=oidc_failed" {
		t.Fatal("callback replay was accepted")
	}
	r = httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	r.AddCookie(session)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	valid, err := a.store.ValidateOIDCSession(context.Background(), session.Value, a.oidc.scope)
	if err != nil || valid {
		t.Fatalf("session survived logout: %v %v", valid, err)
	}
}

func TestOIDCRejectsInvalidTokens(t *testing.T) {
	a, attempt := oidcTestApp(t)
	for _, code := range []string{"wrong-subject", "wrong-nonce", "wrong-issuer", "wrong-audience", "expired", "bad-signature", "missing-id-token", "exchange-failure"} {
		t.Run(code, func(t *testing.T) {
			startOIDCTest(t, a, attempt, "/")
			response := finishOIDCTest(a, attempt, code)
			if response.Header().Get("Location") != "/?auth_error=oidc_failed" || len(response.Result().Cookies()) != 1 {
				t.Fatalf("invalid token accepted: %d %v", response.Code, response.Result().Cookies())
			}
		})
	}
}

func TestOIDCRequiresBrowserBinding(t *testing.T) {
	a, attempt := oidcTestApp(t)
	startOIDCTest(t, a, attempt, "/")
	cookie := attempt.cookie
	for _, value := range []*http.Cookie{nil, {Name: cookie.Name, Value: "unrelated-browser"}} {
		attempt.cookie = value
		if got := finishOIDCTest(a, attempt, "valid"); got.Header().Get("Location") != "/?auth_error=oidc_failed" {
			t.Fatal("callback without browser binding was accepted")
		}
	}
	attempt.cookie = cookie
	if got := finishOIDCTest(a, attempt, "valid"); got.Header().Get("Location") != "/" {
		t.Fatal("browser binding failures consumed another browser's attempt")
	}
}

func TestOIDCProviderDenialConsumesAttempt(t *testing.T) {
	a, attempt := oidcTestApp(t)
	startOIDCTest(t, a, attempt, "/")
	r := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?"+url.Values{"state": {attempt.state}, "error": {"access_denied"}}.Encode(), nil)
	r.AddCookie(attempt.cookie)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Header().Get("Location") != "/?auth_error=oidc_failed" {
		t.Fatal("provider denial did not return a login error")
	}
	if got := finishOIDCTest(a, attempt, "valid"); got.Header().Get("Location") != "/?auth_error=oidc_failed" {
		t.Fatal("denied attempt was reusable")
	}
}

func TestOIDCStatusAndSecureCookies(t *testing.T) {
	a, attempt := oidcTestApp(t)
	a.oidc.secure = true
	a.SetOwnerToken("backup-token")
	status := requestJSON(t, a, http.MethodGet, "/api/auth/status", nil)
	var result map[string]bool
	if err := json.Unmarshal(status.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result["oidcEnabled"] || !result["tokenLoginEnabled"] || result["authenticated"] || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected auth status: %v", result)
	}
	startOIDCTest(t, a, attempt, "/")
	if !attempt.cookie.Secure {
		t.Fatal("HTTPS deployment issued an insecure state cookie behind a proxy")
	}
	for _, cookie := range finishOIDCTest(a, attempt, "valid").Result().Cookies() {
		if !cookie.Secure {
			t.Fatal("HTTPS deployment issued an insecure callback cookie")
		}
	}
	login := requestJSON(t, a, http.MethodPost, "/api/auth/token", map[string]string{"token": "backup-token"})
	if login.Code != http.StatusOK || !login.Result().Cookies()[0].Secure {
		t.Fatal("backup token login must also issue a secure cookie")
	}
}

func TestOIDCReturnToRejectsExternalAndAPIURLs(t *testing.T) {
	for _, value := range []string{"", "https://evil.example", "//evil.example", "/\\evil.example", "/api/auth/oidc/login", "/%2f%2fevil.example", "/\r\nLocation: evil"} {
		if got := oidcReturnTo(value); got != "/" {
			t.Errorf("unsafe returnTo %q accepted as %q", value, got)
		}
	}
}

func TestOIDCConfigRejectsIncompleteOrInsecureSettings(t *testing.T) {
	a := testApp(t)
	for _, config := range []OIDCConfig{
		{},
		{Issuer: "http://identity.example", ClientID: "client", ClientSecret: "secret", OwnerSubject: "owner", RedirectURL: "http://localhost:5173/api/auth/oidc/callback"},
		{Issuer: "https://identity.example", ClientID: "client", ClientSecret: "secret", OwnerSubject: "owner", RedirectURL: "https://notes.example/wrong"},
	} {
		if err := a.ConfigureOIDC(context.Background(), config); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
