package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
)

func TestDecisionsRefreshesRejectedCodexLoginOnce(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "codex")
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"access_token":"expired"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
test "$1" = app-server
read -r init
case "$init" in *'"method":"initialize"'*) ;; *) exit 1;; esac
echo '{"id":1,"result":{}}'
read -r initialized
read -r account
case "$account" in *'"method":"getAuthStatus"'*'"refreshToken":true,"includeToken":false'*) ;; *) exit 1;; esac
echo '{"tokens":{"access_token":"renewed"}}' > "$CODEX_HOME/auth.json"
echo '{"method":"account/updated"}'
echo '{"id":2,"result":{"account":{}}}'
`
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := NewCodexDecisions(exe, home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer renewed" || calls > 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"answers":[{"type":"choice","name":"entry_0","choice":"run"}]}`)
	}))
	defer server.Close()
	d.endpoint = server.URL
	batch := []aloop.Entry{{ID: "email:1"}}
	plan, err := d.Decide(t.Context(), "digest", time.Now(), batch)
	if err != nil || calls != 2 || !slices.Equal(plan.Run, []string{"email:1"}) {
		t.Fatalf("plan=%v calls=%d err=%v", plan, calls, err)
	}
	if _, err := d.Decide(t.Context(), "digest", time.Now(), batch); err == nil || calls != 4 {
		t.Fatalf("persistent authorization failure: calls=%d err=%v", calls, err)
	}
}

func TestDecisionsSharesSnapshotAcrossQuestionBatches(t *testing.T) {
	const count = 401
	entries := make([]aloop.Entry, count)
	for i := range entries {
		entries[i] = aloop.Entry{ID: fmt.Sprintf("mail:%d", i), Source: "mail", Summary: fmt.Sprintf("pending %d", i)}
	}
	now := time.Now().UTC()
	home := t.TempDir()
	authFile := filepath.Join(home, "auth.json")
	writeToken := func(s string) {
		t.Helper()
		if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"tokens":{"access_token":%q}}`, s)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeToken("initial")
	d, err := NewDecisions("", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeToken("rotated")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer rotated" {
			t.Error("unexpected decision request")
		}
		var req struct {
			Model, Input string
			Questions    []decisionQuestion
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var snapshot struct {
			Standing string `json:"standing_prompt"`
			Now      time.Time
			Inbox    []struct {
				aloop.Entry
				Question string
			}
		}
		if err := json.Unmarshal([]byte(req.Input), &snapshot); err != nil {
			t.Error(err)
			return
		}
		if snapshot.Standing != "Manage my inbox" || !snapshot.Now.Equal(now) || len(snapshot.Inbox) != count || req.Model != DecisionsModel {
			t.Error("incomplete shared snapshot")
			return
		}
		for i, row := range snapshot.Inbox {
			if row.Entry != entries[i] || row.Question != fmt.Sprintf("entry_%d", i) {
				t.Error("incorrect shared entry")
				return
			}
		}
		start := calls * 400
		calls++
		if len(req.Questions) != min(400, count-start) {
			t.Error("incorrect question count")
			return
		}
		var answers []map[string]string
		for i, q := range req.Questions {
			if q.Name != snapshot.Inbox[start+i].Question || q.Type != "choice" || len(q.Choices) != 3 || !strings.Contains(q.Instructions, q.Name) {
				t.Error("incorrect choice question")
				return
			}
			disposition := "dismiss"
			if start+i == 0 {
				disposition = "defer"
			} else if start+i == count-1 {
				disposition = "run"
			}
			answers = append(answers, map[string]string{"name": q.Name, "type": "choice", "choice": disposition})
		}
		slices.Reverse(answers)
		json.NewEncoder(w).Encode(map[string]any{"answers": answers})
	}))
	defer server.Close()
	d.endpoint = server.URL + "/v1/decisions"
	got, err := d.Decide(t.Context(), "Manage my inbox", now, entries)
	if err != nil || calls != 2 || !slices.Equal(got.Run, []string{"mail:400"}) || !slices.Equal(got.Defer, []aloop.Deferral{{ID: "mail:0"}}) || len(got.Dismiss) != 399 || got.Dismiss[0] != "mail:1" {
		t.Fatalf("unexpected decision batches: calls=%d run=%v defer=%v dismiss=%d err=%v", calls, got.Run, got.Defer, len(got.Dismiss), err)
	}
}

func TestDecisionsRejectsIncompletePlan(t *testing.T) {
	for _, answer := range []string{`{"type":"refusal","name":"entry_1"}`, `{"type":"choice","name":"entry_0","choice":"run"}`, `{"type":"choice","name":"entry_1","choice":"unknown"}`} {
		d, err := NewDecisions("key", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"answers":[{"type":"choice","name":"entry_0","choice":"dismiss"},%s]}`, answer)
		}))
		d.endpoint = server.URL
		plan, err := d.Decide(t.Context(), "sort", time.Now(), []aloop.Entry{{ID: "a"}, {ID: "b"}})
		server.Close()
		if err == nil || len(plan.Run)+len(plan.Defer)+len(plan.Dismiss) != 0 {
			t.Fatalf("partial decision applied: %+v, %v", plan, err)
		}
	}
}

func TestDecisionRunThreshold(t *testing.T) {
	var answer decisionAnswer
	if err := json.Unmarshal([]byte(`{"type":"choice","choice":"dismiss","probabilities":[{"value":"run","probability":0.2},{"value":"defer","probability":0.35},{"value":"dismiss","probability":0.45}]}`), &answer); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		threshold float64
		want      string
	}{{0.2, "run"}, {0.21, "dismiss"}} {
		d, err := NewDecisions("key", "", &test.threshold)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.disposition(answer)
		if err != nil || got != test.want {
			t.Fatalf("threshold %f: %q %v", test.threshold, got, err)
		}
	}
	threshold := 0.5
	d, err := NewDecisions("key", "", &threshold)
	if err != nil {
		t.Fatal(err)
	}
	answer.Probabilities = answer.Probabilities[:1]
	if _, err = d.disposition(answer); err == nil {
		t.Fatal("missing probabilities accepted")
	}
}
