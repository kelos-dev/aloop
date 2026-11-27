package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kelos-dev/aloop"
)

type Notifications struct {
	dir    string
	output io.Writer
}

type Notification struct {
	Sent    time.Time
	Message string
}

var notificationName = regexp.MustCompile(`^[0-9a-f]{64}\.txt$`)

// Recent reads the newest published notifications from the durable outbox.
func (n *Notifications) Recent(limit int) ([]Notification, error) {
	entries, err := os.ReadDir(n.dir)
	if err != nil {
		return nil, err
	}
	var files []os.FileInfo
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !notificationName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, info)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].ModTime().After(files[j].ModTime()) })
	var messages []Notification
	for _, file := range files[:min(len(files), max(limit, 0))] {
		data, err := os.ReadFile(filepath.Join(n.dir, file.Name()))
		if err != nil {
			return nil, err
		}
		messages = append(messages, Notification{Sent: file.ModTime(), Message: strings.TrimSuffix(string(data), "\n")})
	}
	return messages, nil
}

func OpenNotifications(dir string, output io.Writer) (*Notifications, error) {
	if dir == "" || output == nil {
		return nil, errors.New("notification directory and output are required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Notifications{dir, output}, nil
}

func (n *Notifications) Send(ctx context.Context, c aloop.Call) (string, error) {
	// Publish a complete file once; retries reuse the existing notification.
	name := c.Key + ".txt"
	f, err := os.CreateTemp(n.dir, ".pending-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(c.Args["message"] + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = os.Link(f.Name(), filepath.Join(n.dir, name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return name, nil
		}
		return "", err
	}
	if _, err := fmt.Fprintln(n.output, c.Args["message"]); err != nil {
		return "", err
	}
	return name, nil
}

func Tools(m *Maildir, t *Tasks, n *Notifications) []aloop.Tool {
	tools := []aloop.Tool{
		{Name: "read_email", Description: "Read the referenced email", Run: m.Read},
		{Name: "notify", Description: "Send the user a text notification", Parameters: []string{"message"}, Effect: true, Run: n.Send},
	}
	if t == nil {
		return tools
	}
	return append(tools, []aloop.Tool{
		{Name: "list_tasks", Description: "List current open tasks with IDs and due dates", Run: t.List},
		{Name: "create_task", Description: "Create a new task in Todoist", Parameters: []string{"title"}, Effect: true, Run: t.Create},
		{Name: "update_due", Description: "Change an existing Todoist task's due date (YYYY-MM-DD)", Parameters: []string{"id", "date"}, Effect: true, Run: t.Update},
	}...)
}

func Daily(now time.Time, hour, minute int) []aloop.Entry {
	if now.Hour()*60+now.Minute() < hour*60+minute {
		return nil
	}
	day := now.Format(time.DateOnly)
	return []aloop.Entry{{ID: "daily:" + day, Source: "schedule", Ref: day, Summary: "Daily digest due for " + day}}
}
