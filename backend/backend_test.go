package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatCompletions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sample" {
			t.Error("incorrect API request")
		}
		var body struct {
			Model    string
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "small" || len(body.Messages) != 2 || body.Messages[0].Content != "prompt" || body.Messages[1].Content != `{"id":"email"}` {
			t.Errorf("body: %+v %v", body, err)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"run\":[]}"}}]}`))
	}))
	defer server.Close()
	m, err := NewAPI(server.URL+"/v1", "small", "sample")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Complete(t.Context(), "prompt", map[string]string{"id": "email"})
	if err != nil || string(got) != `{"run":[]}` {
		t.Fatalf("%s %v", got, err)
	}
}

func TestCLIAdapters(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, provider)
			script := `#!/bin/sh
set -eu
if test "$1" = debug; then
 test "$2" = models
 printf '%s' '{"models":[{"slug":"test-model","tool_mode":"code_mode","multi_agent_version":"v2","experimental_supported_tools":["clock"],"apply_patch_tool_type":"freeform","context_window":100000}]}'
 exit 0
fi
input=$(cat)
case "$input" in *'"id":"email"'*) ;; *) exit 10;; esac
case "$0" in
*claude)
 test "$CLAUDE_CODE_OAUTH_TOKEN" = sample
 test "$1" = -p
 printf '{"dismiss":["email"]}'
 ;;
*codex)
 test -f "$CODEX_HOME/auth.json"
 test "$1" = exec
 test -f models.json
 grep -q '"context_window":100000' models.json
 grep -q '"tool_mode":"direct"' models.json
 grep -q '"multi_agent_version":"disabled"' models.json
 grep -Fq '"experimental_supported_tools":[]' models.json
 if grep -q '"apply_patch_tool_type"' models.json; then exit 1; fi
 case "$*" in *'model_catalog_json='*) ;; *) exit 1;; esac
 while test "$1" != --output-last-message; do shift; done
 printf '{"dismiss":["email"]}' > "$2"
 ;;
esac
`
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=/bin:/usr/bin"}
			var model *CLI
			var err error
			if provider == "claude" {
				model, err = NewClaude(exe, "", "sample", env)
			} else {
				if err = os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				model, err = NewCodex(exe, "", dir, env)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := model.Complete(t.Context(), "standing prompt", map[string]string{"id": "email"})
			if err != nil || !strings.Contains(string(got), "dismiss") {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}
