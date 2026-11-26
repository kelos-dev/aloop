package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kelos-dev/aloop"
)

type Notifications struct {
	dir    string
	output io.Writer
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
	return []aloop.Tool{
		{Name: "read_email", Description: "Read the referenced email", Run: m.Read},
		{Name: "list_tasks", Description: "List current open tasks with IDs and due dates", Run: t.List},
		{Name: "create_task", Description: "Create a new task in Todoist", Parameters: []string{"title"}, Effect: true, Run: t.Create},
		{Name: "update_due", Description: "Change an existing Todoist task's due date (YYYY-MM-DD)", Parameters: []string{"id", "date"}, Effect: true, Run: t.Update},
		{Name: "notify", Description: "Send the user a text notification", Parameters: []string{"message"}, Effect: true, Run: n.Send},
	}
}

func Daily(now time.Time, hour, minute int) []aloop.Entry {
	if now.Hour()*60+now.Minute() < hour*60+minute {
		return nil
	}
	day := now.Format(time.DateOnly)
	return []aloop.Entry{{ID: "daily:" + day, Source: "schedule", Ref: day, Summary: "Daily to-do digest due for " + day}}
}
