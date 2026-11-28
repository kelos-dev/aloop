package personal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
)

type googleTransport func(http.ResponseWriter, *http.Request)

func (serve googleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	serve(w, r)
	return w.Result(), nil
}

func testGoogle(t *testing.T, drive bool, api http.HandlerFunc) *GoogleConnection {
	t.Helper()
	client := &http.Client{Transport: googleTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "oauth2.googleapis.com" {
			if r.URL.Path != "/token" || r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "fixture" {
				t.Fatal("unexpected token exchange")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Fatal("expected an authenticated read")
		}
		api(w, r)
	})}
	return openTestGoogle(t, drive, client)
}

func openTestGoogle(t *testing.T, drive bool, client *http.Client) *GoogleConnection {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grant.json")
	data, err := json.Marshal(googleGrant{ClientID: "id", Email: "me@example.org", RefreshToken: "fixture", Drive: drive})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	g, err := OpenGoogleConnection(t.Context(), `{"web":{"client_id":"id","client_secret":"secret","redirect_uris":["http://localhost/oauth/google/callback"]}}`, "http://localhost/oauth/google/callback", path, client)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestE2EGmailSignalReadsLinkedDoc(t *testing.T) {
	history := "10"
	g := testGoogle(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			fmt.Fprintf(w, `{"emailAddress":"me@example.org","historyId":%q}`, history)
		case "/gmail/v1/users/me/history":
			if r.URL.Query().Get("startHistoryId") != "10" || r.URL.Query().Get("historyTypes") != "messageAdded" || r.URL.Query().Get("labelId") != "INBOX" {
				t.Fatal("incorrect Gmail history query")
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"nextPageToken":"next"}`)
			} else {
				fmt.Fprint(w, `{"history":[{"messagesAdded":[{"message":{"id":"abc"}},{"message":{"id":"abc"}}]}]}`)
			}
		case "/gmail/v1/users/me/messages/abc":
			switch r.URL.Query().Get("format") {
			case "metadata":
				fmt.Fprint(w, `{"labelIds":["INBOX"],"snippet":"The agenda is ready","payload":{"headers":[{"name":"Subject","value":"Friday meeting"}]}}`)
			case "raw":
				message := "Subject: Friday meeting\r\nContent-Type: text/plain\r\n\r\nAgenda: https://docs.google.com/document/d/agenda_1/edit"
				fmt.Fprintf(w, `{"raw":%q}`, base64.URLEncoding.EncodeToString([]byte(message)))
			default:
				t.Fatal("unexpected message format")
			}
		case "/drive/v3/files/agenda_1/export":
			if r.URL.Host != "www.googleapis.com" || r.URL.Query().Get("mimeType") != "text/plain" {
				t.Fatal("incorrect Google Docs export")
			}
			fmt.Fprint(w, "Meet at noon.")
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	})
	path := filepath.Join(t.TempDir(), "inbox")
	inbox, err := aloop.OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Current().Collect(t.Context(), inbox); err != nil {
		t.Fatal(err)
	}
	if status, err := inbox.Status(); err != nil || status.Received != 0 {
		t.Fatalf("initial mailbox import: %+v %v", status, err)
	}
	if err = inbox.Close(); err != nil {
		t.Fatal(err)
	}
	history = "14"
	inbox, err = aloop.OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	if err = g.Current().Collect(t.Context(), inbox); err != nil {
		t.Fatal(err)
	}
	const entry = "gmail:me@example.org:abc"
	triage := func(_ context.Context, _ string, _ time.Time, entries []aloop.Entry) (aloop.Plan, error) {
		if len(entries) != 1 || entries[0].ID != entry || !strings.Contains(entries[0].Summary, "Friday meeting") || strings.Contains(entries[0].Summary, "agenda_1") {
			t.Fatalf("unexpected triage inbox: %+v", entries)
		}
		return aloop.Plan{Run: []string{entry}}, nil
	}
	step := 0
	worker := modelFunc(func(_ context.Context, _ string, input any) (json.RawMessage, error) {
		step++
		history, _ := json.Marshal(input.(map[string]any)["history"])
		switch step {
		case 1:
			return json.Marshal(map[string]any{"tool": "read_email", "entry": entry, "args": map[string]string{}})
		case 2:
			if !strings.Contains(string(history), "/d/agenda_1/edit") {
				t.Fatal("missing link from Gmail")
			}
			return json.Marshal(map[string]any{"tool": "read_google_doc", "entry": entry, "args": map[string]string{"id": "agenda_1"}})
		case 3:
			if !strings.Contains(string(history), "Meet at noon.") {
				t.Fatal("missing Google Doc text")
			}
			return json.Marshal(map[string]any{"tool": "notify", "entry": entry, "args": map[string]string{"message": "Friday meeting at noon."}})
		default:
			return json.Marshal(map[string]any{"done": []string{entry}})
		}
	})
	var out bytes.Buffer
	notifications, err := OpenNotifications(t.TempDir(), &out)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := aloop.New(inbox, "Notify me about meetings", time.UTC, triage, worker, Tools(MailSources{Google: g}, nil, notifications)...)
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = g.Current().Collect(t.Context(), inbox); err != nil {
		t.Fatal(err)
	}
	status, err := inbox.Status()
	if err != nil || len(status.Pending) != 0 || status.Received != 1 || status.Effects["notify"] != 1 || out.String() != "Friday meeting at noon.\n" {
		t.Fatalf("status=%+v output=%q err=%v", status, out.String(), err)
	}
	t.Logf("Received %d Gmail signal, read the linked Google Doc, and delivered %q", status.Received, strings.TrimSpace(out.String()))
}

func TestGmailExpiredHistoryRetry(t *testing.T) {
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	const source = "gmail:user@example.org"
	if err = inbox.AppendWithCursor(source, "9/1700000000"); err != nil {
		t.Fatal(err)
	}
	fail := true
	g := testGoogle(t, false, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			fmt.Fprint(w, `{"emailAddress":"user@example.org","historyId":"72"}`)
		case "/gmail/v1/users/me/history":
			w.WriteHeader(http.StatusNotFound)
		case "/gmail/v1/users/me/messages":
			if r.URL.Query().Get("q") != "after:1700000000" || r.URL.Query().Get("labelIds") != "INBOX" {
				t.Fatal("incorrect catch-up query")
			}
			fmt.Fprint(w, `{"messages":[{"id":"one"},{"id":"two"},{"id":"deleted"},{"id":"archived"}]}`)
		case "/gmail/v1/users/me/messages/deleted":
			w.WriteHeader(http.StatusNotFound)
		case "/gmail/v1/users/me/messages/archived":
			fmt.Fprint(w, `{"labelIds":["IMPORTANT"]}`)
		default:
			if fail && strings.HasSuffix(r.URL.Path, "/two") {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"labelIds":["INBOX"],"snippet":"Agenda"}`)
		}
	})
	if err = g.Current().Collect(t.Context(), inbox); !errors.Is(err, googleStatus(503)) {
		t.Fatalf("expected temporary failure, got %v", err)
	}
	if cursor, err := inbox.Cursor(source); cursor != "9/1700000000" || err != nil {
		t.Fatalf("cursor=%q err=%v", cursor, err)
	}
	fail = false
	if err = g.Current().Collect(t.Context(), inbox); err != nil {
		t.Fatal(err)
	}
	if status, err := inbox.Status(); status.Received != 2 || err != nil {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	for _, tool := range Tools(MailSources{Google: g}, nil, nil) {
		if tool.Name == "read_google_doc" {
			if _, err := tool.Run(t.Context(), aloop.Call{}); err == nil {
				t.Fatal("Drive should require consent")
			}
		}
		if tool.Name == "read_email" {
			_, err := tool.Run(t.Context(), aloop.Call{Entry: aloop.Entry{ID: "gmail:other@example.org:one", Source: "email", Ref: "one"}})
			var denied aloop.ToolError
			if !errors.As(err, &denied) {
				t.Fatalf("wrong mailbox was accepted: %v", err)
			}
		}
	}
}

func TestReadGoogleDoc(t *testing.T) {
	g := testGoogle(t, true, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/v3/files/missing/export":
			w.WriteHeader(http.StatusNotFound)
		case "/drive/v3/files/long/export":
			fmt.Fprint(w, strings.Repeat("x", 20_000))
		default:
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
	})
	for _, id := range []string{"../private", "missing", "long"} {
		text, err := g.Current().readDoc(t.Context(), aloop.Call{Args: map[string]string{"id": id}})
		if id == "long" {
			if err != nil || !strings.HasSuffix(text, "[Document truncated]") || len(text) >= 20_000 {
				t.Fatal("expected a bounded document excerpt")
			}
		} else {
			var toolError aloop.ToolError
			if !errors.As(err, &toolError) {
				t.Fatalf("expected a worker-visible error for %q: %v", id, err)
			}
		}
	}
}

func TestGoogleAuthorizationFailures(t *testing.T) {
	errorCode := "invalid_grant"
	g := openTestGoogle(t, true, &http.Client{Transport: googleTransport(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "oauth2.googleapis.com" {
			t.Fatal("unexpected API call")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":%q,"error_description":"private details"}`, errorCode)
	})})
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	if err := g.Current().Collect(t.Context(), inbox); err == nil {
		t.Fatal("collector did not report the expired grant")
	}
	mail := MailSources{Google: g}
	call := aloop.Call{Entry: aloop.Entry{Source: "email", ID: "gmail:me@example.org:a", Ref: "a"}, Args: map[string]string{"id": "doc"}}
	for _, read := range []func(context.Context, aloop.Call) (string, error){mail.read, mail.readDoc} {
		_, err := read(t.Context(), call)
		var problem aloop.ToolError
		if !errors.As(err, &problem) || !strings.Contains(problem.Error(), "reconnect") {
			t.Fatalf("expected actionable tool error, got %v", err)
		}
	}
	errorCode = "invalid_client"
	err = g.Connect(t.Context(), inbox, "code", "verifier", false)
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "private details") {
		t.Fatalf("unexpected exchange error %v", err)
	}
}
