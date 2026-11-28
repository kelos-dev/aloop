package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
)

func TestPrivateChat(t *testing.T) {
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	if err = inbox.ImportNotification("seed", "Private <mail>", time.Now()); err != nil {
		t.Fatal(err)
	}
	handler := Handler(inbox, nil, time.UTC, "09:00", "password", nil)
	request := func(method, path string, auth bool, origin string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://aloop.example"+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if auth {
			r.SetBasicAuth("aloop", "password")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	form := url.Values{"request": {"one"}, "message": {"What did it say?"}, "reply": {"note:seed"}}
	for _, method := range []string{"GET", "POST"} {
		w := request(method, "/chat", false, "", form)
		if w.Code != 401 || strings.Contains(w.Body.String(), "Private &lt;mail&gt;") {
			t.Fatalf("%s was not protected", method)
		}
	}
	if w := request("POST", "/chat", true, "https://other.example", form); w.Code != http.StatusForbidden {
		t.Fatal("allowed cross-site submission")
	}
	if w := request("GET", "/chat?reply=note:seed", true, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Private &lt;mail&gt;") || !strings.Contains(w.Body.String(), "Cancel reply") {
		t.Fatal("missing private reply context")
	}
	for range 2 {
		if w := request("POST", "/chat", true, "https://aloop.example", form); w.Code != 303 {
			t.Fatalf("send returned %d", w.Code)
		}
	}
	messages, err := inbox.ChatMessages("", 10)
	if err != nil || len(messages) != 2 || !messages[1].Pending || messages[1].ReplyTo != "note:seed" {
		t.Fatalf("unexpected conversation %#v %v", messages, err)
	}
	form.Set("reply", "missing")
	form.Set("request", "two")
	if w := request("POST", "/chat", true, "https://aloop.example", form); w.Code != 400 {
		t.Fatal("accepted a nonexistent reply target")
	}
}

type chatAnswer struct{}

func (chatAnswer) Complete(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage(`{"answer":"Confirmed."}`), nil
}

func TestQueuedAnswersQuoteTheirQuestions(t *testing.T) {
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	for _, q := range []string{"First question", "Second question"} {
		if err = inbox.SendChat(q, q, ""); err != nil {
			t.Fatal(err)
		}
	}
	worker := chatAnswer{}
	a, err := aloop.New(inbox, "Answer questions", time.UTC, aloop.PromptTriage(worker), worker)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = a.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var v view
	if err = v.loadChat(inbox, nil); err != nil {
		t.Fatal(err)
	}
	if len(v.Chat) != 4 || len(v.Quotes) != 2 {
		t.Fatalf("expected two queued questions and two quoted answers, got %d messages and %d quotes", len(v.Chat), len(v.Quotes))
	}
	for _, m := range v.Chat {
		if m.Role == "assistant" {
			parent, err := inbox.ChatMessage(m.ReplyTo)
			if err != nil || v.Quotes[m.ID] != parent.Text {
				t.Fatalf("answer %q missing parent quote", m.ID)
			}
		}
	}
}
