package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestE2EOneShotUsesConfiguredRolesAndReopensInbox(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{"new", "cur"} {
		if err := os.Mkdir(filepath.Join(dir, s), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
			t.Error("bad model request")
			return
		}
		var snapshot struct {
			Inbox []struct{ ID string }
			Now   time.Time
		}
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &snapshot); err != nil || len(snapshot.Inbox) != 1 {
			t.Error("expected one daily entry")
			return
		}
		if _, offset := snapshot.Now.Zone(); snapshot.Now.IsZero() || offset != 9*60*60 {
			t.Errorf("model %s received time %s", request.Model, snapshot.Now)
		}
		if !strings.Contains(request.Messages[0].Content, "Use my tools") {
			t.Error("missing shared prompt")
		}
		call := calls.Add(1)
		group := "run"
		if call == 2 {
			group = "done"
		}
		if (call == 1 && request.Model != "cheap") || (call == 2 && request.Model != "worker") || call > 2 {
			t.Errorf("unexpected model %s at call %d", request.Model, call)
		}
		result, _ := json.Marshal(map[string][]string{group: {snapshot.Inbox[0].ID}})
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(result)}}}})
	}))
	defer api.Close()
	cfg := config{
		Prompt:  "Use my tools",
		Triage:  modelConfig{Provider: "openai-compatible", Model: "cheap", BaseURL: api.URL},
		Worker:  modelConfig{Provider: "openai-compatible", Model: "worker", BaseURL: api.URL},
		Maildir: dir, Timezone: "Asia/Tokyo", DailyAt: "00:00",
		Inbox: filepath.Join(dir, "state"), Notifications: filepath.Join(dir, "out"),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.json")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TODOIST_API_TOKEN", "unused")
	t.Setenv("OPENAI_API_KEY", "")
	binary := filepath.Join(dir, "aloop")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for range 2 {
		cmd := exec.CommandContext(t.Context(), binary, "-config", file, "-once")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("aloop: %v\n%s", err, out)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls %d", got)
	}
}

func TestDecisionsTriageConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	if triage, err := makeTriage(modelConfig{}); err != nil || triage == nil {
		t.Fatalf("default triage: %v", err)
	}
	if _, err := makeModel(modelConfig{Provider: "openai-decisions"}); err == nil {
		t.Fatal("decisions used as worker")
	}
	high := 1.1
	for _, cfg := range []modelConfig{{Provider: "openai-decisions", RunThreshold: &high}, {KeyEnv: "MISSING_DECISIONS_TEST_KEY"}, {BaseURL: "https://example.invalid"}, {CodexHome: t.TempDir(), KeyEnv: "OPENAI_API_KEY"}} {
		if _, err := makeTriage(cfg); err == nil {
			t.Fatalf("invalid triage configuration accepted: %+v", cfg)
		}
	}
}
