package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kelos-dev/aloop"
)

// Tasks uses Todoist Sync command UUIDs so a retried write has the same identity.
type Tasks struct {
	base, token string
	client      *http.Client
}

func NewTasks(base, token string) (*Tasks, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || token == "" {
		return nil, errors.New("Todoist requires an API URL and token")
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Tasks{base: strings.TrimRight(base, "/"), token: token, client: client}, nil
}

func (t *Tasks) request(ctx context.Context, path string, form url.Values, out any) error {
	method := http.MethodGet
	if form != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Todoist returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (t *Tasks) List(ctx context.Context, c aloop.Call) (string, error) {
	type task struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Due     *struct {
			Date string `json:"date"`
		} `json:"due"`
	}
	tasks := []task{}
	cursor := ""
	for {
		var page struct {
			Results []task `json:"results"`
			Next    string `json:"next_cursor"`
		}
		path := "/tasks"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor)
		}
		if err := t.request(ctx, path, nil, &page); err != nil {
			return "", err
		}
		tasks = append(tasks, page.Results...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	data, err := json.Marshal(tasks)
	return string(data), err
}

func (t *Tasks) write(ctx context.Context, c aloop.Call, kind string, args map[string]any) (string, error) {
	// Format the stable effect key as a UUID accepted by Todoist's Sync endpoint.
	k := c.Key
	id := fmt.Sprintf("%s-%s-5%s-8%s-%s", k[:8], k[8:12], k[13:16], k[17:20], k[20:32])
	command := map[string]any{"type": kind, "uuid": id, "args": args}
	if kind == "item_add" {
		command["temp_id"] = id
	}
	data, err := json.Marshal([]any{command})
	if err != nil {
		return "", err
	}
	var response struct {
		Status  map[string]json.RawMessage `json:"sync_status"`
		Mapping map[string]string          `json:"temp_id_mapping"`
	}
	if err = t.request(ctx, "/sync", url.Values{"commands": {string(data)}}, &response); err != nil {
		return "", err
	}
	status, ok := response.Status[id]
	if !ok {
		return "", errors.New("Todoist response is missing the command status")
	}
	if string(status) != `"ok"` {
		return "", aloop.ToolError("Todoist rejected the task command")
	}
	if ref := response.Mapping[id]; ref != "" {
		return ref, nil
	}
	if taskID, ok := args["id"].(string); ok {
		return taskID, nil
	}
	return "created", nil
}

func (t *Tasks) Create(ctx context.Context, c aloop.Call) (string, error) {
	return t.write(ctx, c, "item_add", map[string]any{"content": c.Args["title"]})
}
func (t *Tasks) Update(ctx context.Context, c aloop.Call) (string, error) {
	if _, err := time.Parse(time.DateOnly, c.Args["date"]); err != nil {
		return "", aloop.ToolError("expected due date YYYY-MM-DD")
	}
	return t.write(ctx, c, "item_update", map[string]any{"id": c.Args["id"], "due": map[string]string{"date": c.Args["date"]}})
}
