package personal

import (
	"bytes"
	"context"
	"encoding/json"
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

type modelFunc func(context.Context, string, any) (json.RawMessage, error)

func (f modelFunc) Complete(ctx context.Context, p string, v any) (json.RawMessage, error) {
	return f(ctx, p, v)
}

func TestE2EDeadlineChangeAndDigest(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"new", "cur", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	putMail := func(name, id, text string) {
		t.Helper()
		msg := fmt.Sprintf("Message-ID: <%s>\r\nFrom: Maya <maya@example.org>\r\nDate: Thu, 26 Nov 2026 08:45:00 -0500\r\nSubject: Re: Expense report\r\n\r\n%s\r\n", id, text)
		if err := os.WriteFile(filepath.Join(root, "new", name), []byte(msg), 0600); err != nil {
			t.Fatal(err)
		}
	}
	putMail("1", "due@example.org", "Monday is fine instead.")
	mailbox, err := OpenMaildir(root)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer inbox.Close()
	ctx := t.Context()
	collect := func(extra ...aloop.Entry) {
		t.Helper()
		entries, err := mailbox.Collect(ctx, inbox)
		if err != nil {
			t.Fatal(err)
		}
		if err = inbox.Append(append(entries, extra...)...); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	daily := Daily(clock, 9, 0)
	if len(daily) != 1 || len(Daily(clock.Add(-time.Minute), 9, 0)) != 0 {
		t.Fatal("daily signal fired at wrong time")
	}
	collect(daily...)
	tasks, updates := expenseReportFixture(t, "2026-11-27", "2026-11-30")
	dir := t.TempDir()
	var output bytes.Buffer
	notifications, err := OpenNotifications(dir, &output)
	if err != nil {
		t.Fatal(err)
	}
	const mailID = "email:due@example.org"
	triageCalls := 0
	triage := modelFunc(func(_ context.Context, p string, v any) (json.RawMessage, error) {
		if !strings.Contains(p, "Manage deadlines") {
			t.Error("missing standing prompt")
		}
		batch := v.(map[string]any)["inbox"].([]aloop.Entry)
		triageCalls++
		if triageCalls == 1 {
			if len(batch) != 2 {
				t.Fatalf("initial batch %+v", batch)
			}
			for _, e := range batch {
				if e.ID == mailID && !strings.Contains(e.Summary, "2026-11-26T08:45:00-05:00") {
					t.Fatal("missing message date in summary")
				}
			}
			return json.Marshal(aloop.Plan{Run: []string{daily[0].ID}, Defer: []aloop.Deferral{{ID: mailID}}})
		}
		if len(batch) != 1 || batch[0].ID != "email:later@example.org" {
			t.Fatalf("follow-up batch %+v", batch)
		}
		return json.Marshal(map[string]any{"dismiss": []string{batch[0].ID}})
	})
	steps := 0
	worker := modelFunc(func(_ context.Context, p string, v any) (json.RawMessage, error) {
		if !strings.Contains(p, "Manage deadlines") {
			t.Error("missing standing prompt")
		}
		in := v.(map[string]any)
		if len(in["inbox"].([]aloop.Entry)) != 2 || in["triage"].(aloop.Plan).Defer[0].ID != mailID {
			t.Fatal("worker needs the full snapshot and the original triage recommendation")
		}
		history, _ := json.Marshal(in["history"])
		steps++
		tool := func(name, entry string, args map[string]string) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"tool": name, "entry": entry, "args": args})
		}
		switch steps {
		case 1:
			putMail("2", "later@example.org", "Thanks!")
			collect()
			return tool("read_email", mailID, nil)
		case 2:
			if !strings.Contains(string(history), "Date: 2026-11-26T08:45:00-05:00") {
				t.Fatal("missing message date in read_email")
			}
			if !strings.Contains(string(history), "Monday is fine") {
				t.Fatal("email body missing")
			}
			return tool("list_tasks", mailID, nil)
		case 3:
			if !strings.Contains(string(history), "T17") {
				t.Fatal("existing task missing")
			}
			return tool("update_due", mailID, map[string]string{"id": "T17", "date": "2026-11-30"})
		case 4:
			return tool("list_tasks", daily[0].ID, nil)
		case 5:
			var observations []struct{ Result string }
			if err := json.Unmarshal(history, &observations); err != nil {
				t.Fatal(err)
			}
			var current []struct {
				Content string
				Due     struct{ Date string }
			}
			if err := json.Unmarshal([]byte(observations[3].Result), &current); err != nil || len(current) != 1 {
				t.Fatal("missing updated task")
			}
			return tool("notify", daily[0].ID, map[string]string{"message": current[0].Content + " due " + current[0].Due.Date})
		case 6:
			return json.Marshal(map[string]any{"done": []string{mailID, daily[0].ID}})
		default:
			t.Fatal("unexpected worker call")
			return nil, nil
		}
	})
	agent, err := aloop.New(inbox, "Manage deadlines", time.UTC, aloop.PromptTriage(triage), worker, Tools(mailbox, tasks, notifications)...)
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	putMail("3", "due@example.org", "Monday is fine instead.")
	collect(daily...)
	if err = agent.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = agent.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("notification files %v %v", files, err)
	}
	digest, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != string(digest) || string(digest) != "Expense report due 2026-11-30\n" || updates() != 1 || triageCalls != 2 || steps != 6 {
		t.Fatalf("digest=%s updates=%d triage=%d steps=%d", digest, updates(), triageCalls, steps)
	}
}

// The fixture serves one existing Todoist task and records changes to its due date.
func expenseReportFixture(t *testing.T, initialDate, expectedDate string) (*Tasks, func() int) {
	t.Helper()
	date := initialDate
	updates := 0
	todoist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sample" {
			t.Error("missing Todoist auth")
		}
		switch r.URL.Path {
		case "/tasks":
			fmt.Fprintf(w, `{"results":[{"id":"T17","content":"Expense report","due":{"date":%q}}]}`, date)
		case "/sync":
			var commands []struct {
				Type, UUID string
				Args       struct {
					ID  string
					Due struct{ Date string }
				}
			}
			if err := json.Unmarshal([]byte(r.FormValue("commands")), &commands); err != nil || len(commands) != 1 {
				t.Error("bad commands")
				http.Error(w, "bad command", 400)
				return
			}
			cmd := commands[0]
			if cmd.Type != "item_update" || cmd.Args.ID != "T17" || cmd.Args.Due.Date != expectedDate {
				t.Errorf("unexpected update %+v", cmd)
			}
			date = cmd.Args.Due.Date
			updates++
			fmt.Fprintf(w, `{"sync_status":{%q:"ok"}}`, cmd.UUID)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(todoist.Close)
	tasks, err := NewTasks(todoist.URL, "sample")
	if err != nil {
		t.Fatal(err)
	}
	return tasks, func() int { return updates }
}

func TestCreateTaskReusesCommandID(t *testing.T) {
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var commands []struct {
			UUID   string
			TempID string `json:"temp_id"`
			Type   string
			Args   struct{ Content string }
		}
		if err := json.Unmarshal([]byte(r.FormValue("commands")), &commands); err != nil || len(commands) != 1 {
			t.Error("invalid commands")
			return
		}
		c := commands[0]
		if c.Type != "item_add" || c.Args.Content != "Expense report" || c.TempID == "" {
			t.Error("invalid task creation")
		}
		ids = append(ids, c.UUID)
		fmt.Fprintf(w, `{"sync_status":{%q:"ok"},"temp_id_mapping":{%q:"T17"}}`, c.UUID, c.TempID)
	}))
	defer server.Close()
	tasks, err := NewTasks(server.URL, "sample")
	if err != nil {
		t.Fatal(err)
	}
	c := aloop.Call{Key: strings.Repeat("f", 64), Args: map[string]string{"title": "Expense report"}}
	for range 2 {
		result, err := tasks.Create(t.Context(), c)
		if err != nil || result != "T17" {
			t.Fatalf("result=%q err=%v", result, err)
		}
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("operation IDs %v", ids)
	}
}
