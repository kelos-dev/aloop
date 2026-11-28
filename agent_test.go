package aloop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type modelFunc func(context.Context, string, any) (json.RawMessage, error)

func (f modelFunc) Complete(c context.Context, p string, in any) (json.RawMessage, error) {
	return f(c, p, in)
}
func replies(t *testing.T, values ...string) Model {
	t.Helper()
	return modelFunc(func(context.Context, string, any) (json.RawMessage, error) {
		if len(values) == 0 {
			t.Fatal("unexpected model call")
		}
		value := values[0]
		values = values[1:]
		return json.RawMessage(value), nil
	})
}

func TestDeferredRetryAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox")
	inbox, err := OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	writes := 0
	task := Tool{Name: "create", Effect: true, Parameters: []string{"title"}, Run: func(context.Context, Call) (string, error) { writes++; return "T17", nil }}
	triage := replies(t, `{"defer":[{"id":"mail"}]}`, `{"run":["mail"],"dismiss":["timer"]}`)
	call := `{"entry":"mail","tool":"create","args":{"title":"Report"}}`
	a, err := New(inbox, "Manage tasks", time.UTC, PromptTriage(triage), replies(t, call, "invalid"), task)
	if err != nil {
		t.Fatal(err)
	}
	mail := Entry{ID: "mail", Source: "email", Summary: "Submit report"}
	if err = inbox.Append(mail); err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = inbox.Append(Entry{ID: "timer", Source: "clock", Summary: "Digest due"}); err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(ctx); err == nil {
		t.Fatal("malformed worker response accepted")
	}
	if writes != 1 {
		t.Fatalf("writes %d", writes)
	}
	if err = inbox.Close(); err != nil {
		t.Fatal(err)
	}
	inbox, err = OpenInbox(path)
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	a, err = New(inbox, "Manage tasks", time.UTC, PromptTriage(replies(t, `{"run":["mail"],"dismiss":["timer"]}`)), replies(t, call, `{"done":["mail","timer"]}`), task)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = inbox.Append(mail); err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("retried effect executed %d times", writes)
	}
}

func TestTimedDeferralAndInvalidPartition(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	event := Entry{ID: "1", Source: "clock", Summary: "Weekly digest"}
	if err = inbox.Append(event); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	deferred, _ := json.Marshal(Plan{Defer: []Deferral{{"1", &until}}})
	a, err := New(inbox, "Triage", time.UTC, PromptTriage(replies(t, `{"run":["1"],"dismiss":["1"]}`, string(deferred))), replies(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RunOnce(t.Context()); err == nil {
		t.Fatal("accepted overlapping groups")
	}
	if err = a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now  time.Time
		want int
	}{{until.Add(-time.Second), 0}, {until, 1}} {
		got, err := inbox.snapshot(tc.now)
		if err != nil || len(got) != tc.want {
			t.Fatalf("snapshot %v %v", got, err)
		}
	}
}

func TestArrivalsDuringRun(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var batches [][]Entry
	triage := modelFunc(func(_ context.Context, _ string, in any) (json.RawMessage, error) {
		batch := in.(map[string]any)["inbox"].([]Entry)
		batches = append(batches, batch)
		if len(batches) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ids := []string{}
		for _, e := range batch {
			ids = append(ids, e.ID)
		}
		if len(batches) == 2 {
			cancel()
		}
		return json.Marshal(Plan{Dismiss: ids})
	})
	a, err := New(inbox, "Triage", time.UTC, PromptTriage(triage), replies(t))
	if err != nil {
		t.Fatal(err)
	}
	appendID := func(id string) {
		t.Helper()
		if err := inbox.Append(Entry{ID: id, Source: "mail", Summary: id}); err != nil {
			t.Fatal(err)
		}
	}
	appendID("a")
	go func() { done <- a.Run(ctx, time.Hour) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	appendID("b")
	appendID("c")
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != 1 || len(batches[1]) != 2 {
		t.Fatalf("batches: %v", batches)
	}
}

func TestWorkerRejectsInvalidCalls(t *testing.T) {
	for name, reply := range map[string]string{
		"unknown tool":                `{"tool":"shell","entry":"E","args":{}}`,
		"outside snapshot":            `{"tool":"notify","entry":"other","args":{"message":"hello"}}`,
		"missing argument":            `{"tool":"notify","entry":"E","args":{}}`,
		"unexpected argument":         `{"tool":"notify","entry":"E","args":{"message":"hello","extra":"value"}}`,
		"mixed action and completion": `{"tool":"notify","entry":"E","args":{"message":"hello"},"done":["E"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			in, err := OpenInbox(filepath.Join(t.TempDir(), "inbox"))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			if err = in.Append(Entry{ID: "E", Source: "test", Summary: "event"}); err != nil {
				t.Fatal(err)
			}
			tool := Tool{Name: "notify", Parameters: []string{"message"}, Run: func(context.Context, Call) (string, error) { t.Fatal("invalid call reached the tool"); return "", nil }}
			a, err := New(in, "standing prompt", time.UTC, PromptTriage(replies(t, `{"run":["E"]}`)), replies(t, reply), tool)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.RunOnce(t.Context()); err == nil {
				t.Fatal("accepted invalid call")
			}
			batch, err := in.snapshot(time.Now())
			if err != nil || len(batch) != 1 {
				t.Fatalf("lost work: %v %v", batch, err)
			}
		})
	}
}

func TestWorkerCanSettleOtherEntriesAfterToolError(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	if err := inbox.Append(Entry{ID: "mail", Source: "email", Summary: "Archived mail"}, Entry{ID: "timer", Source: "clock", Summary: "Daily digest"}); err != nil {
		t.Fatal(err)
	}
	read := Tool{Name: "read", Run: func(context.Context, Call) (string, error) { return "", ToolError("email was archived") }}
	seen := false
	worker := modelFunc(func(_ context.Context, _ string, input any) (json.RawMessage, error) {
		history := input.(map[string]any)["history"].([]observation)
		if len(history) == 0 {
			return json.RawMessage(`{"tool":"read","entry":"mail","args":{}}`), nil
		}
		if len(history) != 1 || history[0].Call.Entry != "mail" || history[0].Error != "email was archived" {
			t.Fatalf("tool history: %+v", history)
		}
		seen = true
		return json.RawMessage(`{"done":["timer"],"defer":[{"id":"mail"}]}`), nil
	})
	a, err := New(inbox, "Send my digest", time.UTC, PromptTriage(replies(t, `{"run":["mail","timer"]}`)), worker, read)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("worker did not observe the failed read")
	}
}

func TestScheduledDeferralSurvivesOtherSignals(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	now := time.Now()
	until := now.Add(time.Hour)
	event := Entry{ID: "reminder", Source: "timer", Summary: "Send the reminder"}
	if err := inbox.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := inbox.settle(now, nil, []Deferral{{ID: event.ID, Until: &until}}); err != nil {
		t.Fatal(err)
	}
	other := Entry{ID: "ad", Source: "email", Summary: "Sale"}
	if err := inbox.Append(other); err != nil {
		t.Fatal(err)
	}
	if err := inbox.settle(now, []string{other.ID}, []Deferral{{ID: event.ID}}); err != nil {
		t.Fatal(err)
	}
	before, err := inbox.snapshot(until.Add(-time.Second))
	if err != nil || len(before) != 0 {
		t.Fatalf("premature wake: %v %v", before, err)
	}
	due, err := inbox.snapshot(until)
	if err != nil || len(due) != 1 || due[0].ID != event.ID {
		t.Fatalf("scheduled wake: %v %v", due, err)
	}
	if err := inbox.settle(until, nil, []Deferral{{ID: event.ID}}); err != nil {
		t.Fatal(err)
	}
	again, err := inbox.snapshot(until.Add(time.Minute))
	if err != nil || len(again) != 0 {
		t.Fatalf("consumed deadline woke again: %v %v", again, err)
	}
}

func TestInboxStatusIncludesDeferredSignalsAndEffectCounts(t *testing.T) {
	inbox, err := OpenInbox(filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	err = inbox.Append(Entry{"mail:1", "email", "1", "Deadline moved"}, Entry{"day:1", "schedule", "1", "Digest due"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.settle(time.Now(), []string{"day:1"}, []Deferral{{ID: "mail:1"}}); err != nil {
		t.Fatal(err)
	}
	if err := inbox.record("digest-notification", Receipt{Entry: "day:1", Tool: "notify"}, nil); err != nil {
		t.Fatal(err)
	}
	status, err := inbox.Status()
	if err != nil || status.Received != 2 || len(status.Pending) != 1 || !status.Pending[0].Deferred || status.Pending[0].ID != "mail:1" || status.Effects["notify"] != 1 {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
}
