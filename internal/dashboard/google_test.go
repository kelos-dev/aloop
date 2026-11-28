package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/internal/personal"
	"golang.org/x/oauth2"
)

type googleHTTP func(*http.Request) (*http.Response, error)

func (f googleHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDashboardGoogleConsent(t *testing.T) {
	const origin = "https://aloop.example"
	const callback = origin + personal.GoogleCallback
	const gmail = "https://www.googleapis.com/auth/gmail.readonly"
	const drive = "https://www.googleapis.com/auth/drive.readonly"
	const app = `{"web":{"client_id":"app","client_secret":"app-secret","redirect_uris":["` + callback + `"]}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "grant.json")
	inbox, err := aloop.OpenInbox(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	var challenge string
	granted := gmail + " " + drive
	exchanges := 0
	client := &http.Client{Transport: googleHTTP(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Host {
		case "oauth2.googleapis.com":
			if r.FormValue("code") != "fixture" || r.FormValue("client_secret") != "app-secret" || r.FormValue("redirect_uri") != callback || challenge == "" || oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != challenge {
				t.Fatal("invalid authorization code exchange")
			}
			exchanges++
			fmt.Fprintf(w, `{"access_token":"access","refresh_token":"refresh-private","token_type":"Bearer","expires_in":3600,"scope":%q}`, granted)
		case "gmail.googleapis.com":
			if r.URL.Path != "/gmail/v1/users/me/profile" || r.Header.Get("Authorization") != "Bearer access" {
				t.Fatal("invalid Gmail profile request")
			}
			fmt.Fprint(w, `{"emailAddress":"user@example.com","historyId":"100"}`)
		default:
			t.Fatalf("unexpected request host %s", r.URL.Host)
		}
		return w.Result(), nil
	})}
	open := func() *personal.GoogleConnection {
		g, err := personal.OpenGoogleConnection(t.Context(), app, callback, path, client)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	connection := open()
	handler := Handler(inbox, nil, time.UTC, "09:00", "test", connection)
	request := func(method, path, from, form string, auth bool, c *http.Cookie) *http.Response {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if from != "" {
			r.Header.Set("Origin", from)
		}
		if auth {
			r.SetBasicAuth("aloop", "test")
		}
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result()
	}
	for _, tc := range []struct {
		from string
		auth bool
		want int
	}{{origin, false, 401}, {"https://other.example", true, 403}} {
		r := request("POST", "/oauth/google/connect", tc.from, "", tc.auth, nil)
		if r.StatusCode != tc.want || len(r.Cookies()) != 0 {
			t.Fatal("untrusted consent initiation was accepted")
		}
	}
	start := func(withDrive bool) (string, *http.Cookie) {
		form := ""
		want := gmail
		if withDrive {
			form = "drive=on"
			want += " " + drive
		}
		r := request("POST", "/oauth/google/connect", origin, form, true, nil)
		loc, err := r.Location()
		if err != nil || r.StatusCode != 303 || loc.Host != "accounts.google.com" {
			t.Fatal("missing Google consent redirect")
		}
		q := loc.Query()
		challenge = q.Get("code_challenge")
		if q.Get("scope") != want || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != callback {
			t.Fatal("incorrect Google consent parameters")
		}
		c := r.Cookies()[0]
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Value == "" || c.Value != q.Get("state") {
			t.Fatal("consent is not bound to browser session")
		}
		return personal.GoogleCallback + "?code=fixture&state=" + url.QueryEscape(c.Value), c
	}
	pathAndQuery, c := start(true)
	for _, tc := range []struct {
		cookie *http.Cookie
		auth   bool
		want   int
	}{{c, false, 401}, {nil, true, 400}} {
		r := request("GET", pathAndQuery, "", "", tc.auth, tc.cookie)
		if r.StatusCode != tc.want || exchanges != 0 {
			t.Fatal("unbound callback was accepted")
		}
	}
	r := request("GET", pathAndQuery, "", "", true, c)
	if r.Header.Get("Location") != "/inbox?google=connected" || exchanges != 1 {
		t.Fatal("grant was not accepted")
	}
	if r = request("GET", pathAndQuery, "", "", true, c); r.StatusCode != 400 || exchanges != 1 {
		t.Fatal("callback was replayed")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("grant requires a private file")
	}
	if email, docs := open().Status(); email != "user@example.com" || !docs {
		t.Fatal("account did not survive restart")
	}
	if cursor, err := inbox.Cursor("gmail:user@example.com"); err != nil || !strings.HasPrefix(cursor, "100/") {
		t.Fatal("mail collection did not begin at consent")
	}
	r = request("GET", "/inbox", "", "", true, nil)
	page, err := io.ReadAll(r.Body)
	if err != nil || !strings.Contains(string(page), "user@example.com") || strings.Contains(string(page), "refresh-private") {
		t.Fatal("incorrect private connection status")
	}
	granted = drive
	pathAndQuery, c = start(false)
	r = request("GET", pathAndQuery, "", "", true, c)
	if r.Header.Get("Location") != "/inbox?google=failed" {
		t.Fatal("accepted a grant without Gmail access")
	}
	if _, docs := connection.Status(); !docs {
		t.Fatal("unsuccessful consent replaced the working grant")
	}
	granted = gmail
	pathAndQuery, c = start(true)
	request("GET", pathAndQuery, "", "", true, c)
	if email, docs := open().Status(); email == "" || docs {
		t.Fatal("enabled Drive without user consent")
	}
}
