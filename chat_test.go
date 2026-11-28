package aloop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDigestReplyUsesCitedEmails(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	err = inbox.Append(Entry{"a", "email", "ref-a", "Design review"}, Entry{"b", "email", "ref-b", "Newsletter"}, Entry{"daily", "schedule", "today", "Digest due"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	notify := Tool{Name: "notify", Effect: true, Parameters: []string{"message"}, Run: func(context.Context, Call) (string, error) { calls++; return "file", nil }}
	read := Tool{Name: "read_email", Run: func(_ context.Context, c Call) (string, error) {
		if c.Entry.Ref != "ref-a" {
			t.Fatalf("accessed unrelated message: %v", c.Entry)
		}
		return "Tuesday at 10:30", nil
	}}
	worker := replies(t,
		`{"tool":"notify","entry":"daily","email_sources":["a"],"args":{"message":"Maya moved the design review."}}`,
		`{"done":["a","b","daily"]}`,
		`{"read_email":"b"}`, `{"read_email":"a"}`, `{"answer":"Tuesday at 10:30"}`)
	a, err := New(inbox, "Manage mail", time.UTC, PromptTriage(replies(t, `{"run":["daily"],"dismiss":["b"],"defer":[{"id":"a"}]}`)), worker, notify, read)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	thread, err := inbox.ChatMessages("", 10)
	if err != nil || calls != 1 || len(thread) != 1 || len(thread[0].Sources) != 1 || thread[0].Sources[0].ID != "a" {
		t.Fatalf("digest sources: %#v %v", thread, err)
	}
	if err = inbox.SendChat("one", "When?", thread[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	answer, err := inbox.ChatMessage("answer:user:one")
	if err != nil || answer.Text != "Tuesday at 10:30" || len(answer.Sources) != 1 || answer.Sources[0].ID != "a" {
		t.Fatalf("answer: %#v %v", answer, err)
	}
}

func TestImportedNotificationSources(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	for _, tc := range []struct{ entry, ref string }{{"gmail:me@example.org:abc", "abc"}, {"email:mail@example.org", ""}, {"maildir:letter", "letter"}, {"daily:today", ""}} {
		key := effectKey(tc.entry, "notify")
		if err = inbox.record(key, Receipt{Entry: tc.entry, Tool: "notify"}, nil); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err = inbox.ImportNotification(key, "Update", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		n, err := inbox.ChatMessage("note:" + key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(tc.entry, "daily:") {
			if len(n.Sources) != 0 {
				t.Fatal("unexpected email attribution")
			}
		} else if len(n.Sources) != 1 || n.Sources[0].ID != tc.entry || n.Sources[0].Ref != tc.ref {
			t.Fatalf("source: %v", n)
		}
	}
	messages, err := inbox.ChatMessages("", 10)
	if err != nil || len(messages) != 4 {
		t.Fatalf("imported %d messages: %v", len(messages), err)
	}
}

func TestQueuedChatTurnsSeePreviousAnswer(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	for _, q := range []string{"first", "second"} {
		if err = inbox.SendChat(q, q, ""); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	worker := modelFunc(func(_ context.Context, _ string, input any) (json.RawMessage, error) {
		calls++
		turn := input.(map[string]any)
		history := turn["conversation"].([]ChatMessage)
		switch calls {
		case 1:
			if turn["question"] != "first" || len(history) != 0 {
				t.Fatal("first turn saw a future message")
			}
		case 2:
			if turn["question"] != "second" || len(history) != 2 || history[0].Text != "first" || history[1].Text != "Received." {
				t.Fatalf("missing earlier answer: %v", history)
			}
		default:
			t.Fatal("answered a turn more than once")
		}
		return json.RawMessage(`{"answer":"Received."}`), nil
	})
	a, err := New(inbox, "Handle mail", time.UTC, PromptTriage(replies(t)), worker)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = a.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("handled %d questions", calls)
	}
}
