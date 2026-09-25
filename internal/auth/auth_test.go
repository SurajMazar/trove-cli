package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialRedactsItself(t *testing.T) {
	c := Credential{Kind: KindToken, Token: "ghp_supersecretvalue1234567890"}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
		if strings.Contains(s, "supersecret") {
			t.Fatalf("credential leaked via fmt: %s", s)
		}
	}
	b, _ := json.Marshal(map[string]any{"c": c})
	if strings.Contains(string(b), "supersecret") {
		t.Fatalf("credential leaked via JSON: %s", b)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	plain := Credential{Kind: KindToken, Token: "abc"}
	if plain.Encode() != "abc" {
		t.Fatalf("plain token should be stored verbatim, got %q", plain.Encode())
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rich := Credential{Kind: KindOAuth, Token: "a", RefreshToken: "r", Username: "u", ClientSecret: "s", Expiry: exp, Scopes: []string{"api"}}
	got, err := DecodeCredential(rich.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "a" || got.RefreshToken != "r" || got.Username != "u" || got.ClientSecret != "s" || !got.Expiry.Equal(exp) || got.Kind != KindOAuth {
		t.Fatal("round trip lost fields")
	}
	if !got.Refreshable() || got.Expired() {
		t.Fatal("unexpected refreshable/expired state")
	}
	// Hand-entered JSON that is not ours is an opaque token.
	other, _ := DecodeCredential(`{"foo":1}`)
	if other.Kind != KindToken || other.Token != `{"foo":1}` {
		t.Fatal("foreign JSON should be treated as a token")
	}
	if _, err := DecodeCredential("  "); err == nil {
		t.Fatal("empty value must fail")
	}
}

func TestDeviceFlow(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device":
			if r.Form.Get("client_id") != "cid" || r.Form.Get("scope") != "repo read:org" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"device_code":"dc","user_code":"ABCD-1234","verification_uri":"https://example/device","expires_in":60,"interval":1}`)
		case "/token":
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dc" {
				w.WriteHeader(400)
				return
			}
			if atomic.AddInt32(&polls, 1) == 1 {
				fmt.Fprint(w, `{"error":"authorization_pending"}`) // GitHub style: HTTP 200 + error
				return
			}
			fmt.Fprint(w, `{"access_token":"tok","token_type":"bearer","refresh_token":"ref","expires_in":3600,"scope":"repo,read:org"}`)
		}
	}))
	defer srv.Close()
	f := &DeviceFlow{DeviceCodeURL: srv.URL + "/device", TokenURL: srv.URL + "/token", ClientID: "cid", Scopes: []string{"repo", "read:org"},
		sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	var shown string
	c, err := f.Run(context.Background(), promptFunc(func(uri, code string) { shown = uri + " " + code }))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok" || c.RefreshToken != "ref" || c.Expiry.IsZero() || len(c.Scopes) != 2 || shown != "https://example/device ABCD-1234" {
		t.Fatalf("unexpected result: kind=%s scopes=%v shown=%q", c.Kind, c.Scopes, shown)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			fmt.Fprint(w, `{"device_code":"dc","user_code":"X","verification_uri":"u","expires_in":60,"interval":1}`)
			return
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"access_denied","error_description":"user said no"}`)
	}))
	defer srv.Close()
	f := &DeviceFlow{DeviceCodeURL: srv.URL + "/device", TokenURL: srv.URL + "/token", ClientID: "c",
		sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	_, err := f.Run(context.Background(), nil)
	if !IsOAuthError(err, "access_denied") {
		t.Fatalf("want access_denied, got %v", err)
	}
}

func TestDeviceCodeErrorWith200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":"unauthorized_client","error_description":"device flow disabled"}`)
	}))
	defer srv.Close()
	f := &DeviceFlow{DeviceCodeURL: srv.URL, TokenURL: srv.URL, ClientID: "c"}
	if _, err := f.RequestCode(context.Background()); !IsOAuthError(err, "unauthorized_client") {
		t.Fatalf("want unauthorized_client, got %v", err)
	}
}

func TestRefreshTokenBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		_ = r.ParseForm()
		if !ok || u != "key" || p != "secret" || r.Form.Get("refresh_token") != "old" || r.Form.Get("client_id") != "" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new","expires_in":7200}`)
	}))
	defer srv.Close()
	c, err := RefreshToken(context.Background(), srv.Client(), srv.URL, "key", "secret", "old", true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "new" || c.RefreshToken != "old" {
		t.Fatal("refresh should keep the old refresh token when none is returned")
	}
}

type promptFunc func(uri, code string)

func (p promptFunc) DeviceCode(uri, code string, _ time.Duration) { p(uri, code) }
func (p promptFunc) Info(string)                                  {}
