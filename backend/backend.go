// Package backend provides interchangeable model clients for aloop.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type API struct {
	endpoint, model, key string
	client               *http.Client
}

func NewAPI(baseURL, model, key string) (*API, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || model == "" {
		return nil, errors.New("Chat Completions requires an HTTP base URL and a model")
	}
	client := &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &API{endpoint: strings.TrimRight(baseURL, "/") + "/chat/completions", model: model, key: key, client: client}, nil
}

func (a *API) Complete(ctx context.Context, prompt string, input any) (json.RawMessage, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"model": a.model,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": string(data)},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Chat Completions returned HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err = json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if len(envelope.Choices) == 0 {
		return nil, errors.New("Chat Completions returned no choices")
	}
	return json.RawMessage(envelope.Choices[0].Message.Content), nil
}

type CLI struct {
	executable, model string
	env               []string
	codex             bool
}

func NewClaude(executable, model, token string, env []string) (*CLI, error) {
	if !filepath.IsAbs(executable) || token == "" {
		return nil, errors.New("Claude Code requires an executable path and setup token")
	}
	return &CLI{executable, model, append(append([]string{}, env...), "CLAUDE_CODE_OAUTH_TOKEN="+token), false}, nil
}

func NewCodex(executable, model, home string, env []string) (*CLI, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(home) {
		return nil, errors.New("Codex requires absolute executable and home paths")
	}
	if st, err := os.Stat(filepath.Join(home, "auth.json")); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("Codex home must contain auth.json")
	}
	return &CLI{executable, model, append(append([]string{}, env...), "CODEX_HOME="+home), true}, nil
}

// RefreshCodexLogin renews a file-backed login and verifies that Codex saved it.
func RefreshCodexLogin(ctx context.Context, executable, home string, env []string) error {
	cli, err := NewCodex(executable, "", home, env)
	if err != nil {
		return err
	}
	// getAuthStatus can succeed even when the OAuth exchange fails.
	before, err := codexRefreshTime(home)
	if err != nil {
		return err
	}
	if err := cli.refreshLogin(ctx); err != nil {
		return err
	}
	after, err := codexRefreshTime(home)
	if err != nil {
		return err
	}
	if after.IsZero() || after.Equal(before) {
		return errors.New("Codex did not persist a renewed login")
	}
	return nil
}

func codexRefreshTime(home string) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return time.Time{}, err
	}
	var login struct {
		LastRefresh time.Time `json:"last_refresh"`
		Tokens      struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		}
	}
	if json.Unmarshal(data, &login) != nil || login.Tokens.Refresh == "" || login.Tokens.Access == "" {
		return time.Time{}, errors.New("expected a refreshable Codex login")
	}
	return login.LastRefresh, nil
}

func (c *CLI) refreshLogin(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.executable, "app-server", "--listen", "stdio://", "-c", `cli_auth_credentials_store="file"`)
	cmd.Env = c.env
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Cancel = func() error {
		in.Close()
		out.Close()
		return cmd.Process.Kill()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		in.Close()
		cmd.Wait()
	}()

	decoder := json.NewDecoder(out)
	exchange := func(id int, request string) error {
		if _, err := fmt.Fprintln(in, request); err != nil {
			return err
		}
		for {
			var reply struct {
				ID     int
				Error  json.RawMessage
				Result json.RawMessage
			}
			if err := decoder.Decode(&reply); err != nil {
				return err
			}
			if reply.ID == id {
				if len(reply.Error) > 0 || len(reply.Result) == 0 {
					return errors.New("Codex app-server rejected the account request")
				}
				return nil
			}
		}
	}
	if err := exchange(1, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"aloop","version":"1"}}}`); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(in, `{"method":"initialized"}`); err != nil {
		return err
	}
	return exchange(2, `{"id":2,"method":"getAuthStatus","params":{"refreshToken":true,"includeToken":false}}`)
}

// Model metadata can enable Code Mode and collaboration tools independently of CLI feature flags.
func (c *CLI) writeCodexCatalog(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, c.executable, "debug", "models",
		"-c", `model_provider="openai"`, "-c", `cli_auth_credentials_store="file"`)
	cmd.Dir, cmd.Env = dir, c.env
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read Codex model catalog: %w", err)
	}
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return "", fmt.Errorf("decode Codex model catalog: %w", err)
	}
	if len(catalog.Models) == 0 {
		return "", errors.New("empty Codex model catalog")
	}
	for _, model := range catalog.Models {
		model["tool_mode"] = json.RawMessage(`"direct"`)
		model["multi_agent_version"] = json.RawMessage(`"disabled"`)
		model["experimental_supported_tools"] = json.RawMessage(`[]`)
		model["shell_type"] = json.RawMessage(`"disabled"`)
		delete(model, "apply_patch_tool_type")
		model["node_repl_disabled"] = json.RawMessage("true")
		model["supports_search_tool"] = json.RawMessage("false")
	}
	data, err = json.Marshal(catalog)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "models.json")
	return path, os.WriteFile(path, data, 0600)
}

func (c *CLI) Complete(ctx context.Context, prompt string, input any) (json.RawMessage, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "aloop-model-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "response")
	args := []string{
		"-p", "--safe-mode", "--tools", "", "--strict-mcp-config", "--no-session-persistence",
		"--output-format", "text", "--system-prompt", prompt,
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if c.codex {
		catalog, err := c.writeCodexCatalog(ctx, dir)
		if err != nil {
			return nil, err
		}
		args = []string{
			"exec", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check",
			"--sandbox", "read-only", "--disable", "shell_tool", "--disable", "apps",
			"--disable", "plugins", "--disable", "hooks", "--disable", "multi_agent",
			"--disable", "multi_agent_v2", "--disable", "goals", "--disable", "view_image",
			"--disable", "image_generation", "--disable", "skill_search", "--disable", "sleep_tool",
			"-c", "model_catalog_json=" + strconv.Quote(catalog), "-c", "agents.enabled=false",
			"-c", "tools.experimental_request_user_input.enabled=false", "-c", "tools.update_plan.enabled=false",
			"-c", `approval_policy="never"`, "-c", `web_search="disabled"`,
			"-c", `cli_auth_credentials_store="file"`, "--output-last-message", output,
		}
		data = append([]byte("Return one JSON response without calling Codex tools. The host executes any requested aloop tool.\n"+prompt+"\nInput JSON:\n"), data...)
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.codex {
		args = append(args, "-")
	}
	cmd := exec.CommandContext(ctx, c.executable, args...)
	cmd.Dir = dir
	cmd.Env = c.env
	cmd.Stdin = bytes.NewReader(data)
	var stdout bytes.Buffer
	cmd.Stdout = io.Discard
	if !c.codex {
		cmd.Stdout = &stdout
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("model CLI failed: %w", err)
	}
	if c.codex {
		return os.ReadFile(output)
	}
	return stdout.Bytes(), nil
}
