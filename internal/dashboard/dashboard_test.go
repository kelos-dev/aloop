package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/internal/personal"
)

func TestDashboardViews(t *testing.T) {
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	err = inbox.Append(aloop.Entry{ID: "mail:private", Source: "email", Ref: "private-reference", Summary: "Private <message>"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	notes, err := personal.OpenNotifications(dir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, err = notes.Send(t.Context(), aloop.Call{Key: strings.Repeat("a", 64), Args: map[string]string{"message": "Private <digest>"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pending-test"), []byte("unpublished"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path, password, supplied string
		code                     int
		want, absent             string
	}{
		{"/", "test", "", 200, "Agent overview", "Private"},
		{"/inbox", "test", "", 401, "Private view", "message"},
		{"/notifications", "test", "wrong", 401, "Private view", "digest&gt;"},
		{"/inbox", "", "any", 403, "Private view", "message"},
		{"/inbox", "test", "test", 200, "Private &lt;message&gt;", "private-reference"},
		{"/inbox?state=deferred", "test", "test", 200, "No signals in this view", "message"},
		{"/notifications", "test", "test", 200, "Private &lt;digest&gt;", "unpublished"},
		{"/assets/dashboard.css", "", "", 200, "@font-face", "private-reference"},
		{"/unknown", "", "", 404, "404", "Agent overview"},
	} {
		t.Run(tt.path+"/"+tt.supplied, func(t *testing.T) {
			handler := Handler(inbox, notes, time.UTC, "09:00", tt.password)
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.supplied != "" {
				request.SetBasicAuth("aloop", tt.supplied)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)
			body := w.Body.String()
			if w.Code != tt.code || !strings.Contains(body, tt.want) || strings.Contains(body, tt.absent) {
				t.Fatalf("response %d: want %d containing %q, excluding %q", w.Code, tt.code, tt.want, tt.absent)
			}
			if (w.Header().Get("WWW-Authenticate") != "") != (tt.code == 401) {
				t.Error("unexpected authentication challenge")
			}
		})
	}
}
