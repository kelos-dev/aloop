package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/kelos-dev/aloop"
)

type Maildir struct {
	root  string
	known map[string]bool
}

func OpenMaildir(root string) (*Maildir, error) {
	for _, dir := range []string{"new", "cur"} {
		st, err := os.Stat(filepath.Join(root, dir))
		if err != nil || !st.IsDir() {
			return nil, fmt.Errorf("expected a Maildir at %q", root)
		}
	}
	return &Maildir{root, map[string]bool{}}, nil
}

func snippet(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

type mailMessage struct {
	id, from, subject, body, date string
}

func readMail(path string) (mailMessage, error) {
	var message mailMessage
	f, err := os.Open(path)
	if err != nil {
		return mailMessage{}, err
	}
	defer f.Close()
	m, err := mail.CreateReader(f)
	if err != nil {
		return mailMessage{}, err
	}
	defer m.Close()
	message.id, _ = m.Header.MessageID()
	message.subject, _ = m.Header.Subject()
	message.from = m.Header.Get("From")
	if date, err := m.Header.Date(); err == nil && !date.IsZero() {
		message.date = date.Format(time.RFC3339)
	}
	for {
		part, err := m.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return mailMessage{}, err
		}
		h, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		typ, _, _ := h.ContentType()
		if typ != "text/plain" && typ != "text/html" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part.Body, 16384))
		if err != nil {
			return mailMessage{}, err
		}
		message.body = strings.ToValidUTF8(string(data), string(utf8.RuneError))
		if typ == "text/plain" {
			break
		}
	}
	return message, nil
}

// Collect reads stable Maildir filenames. The same key survives a new-to-cur move.
func (m *Maildir) Collect(ctx context.Context, inbox *aloop.Inbox) ([]aloop.Entry, error) {
	var out []aloop.Entry
	var errs []error
	for _, dir := range []string{"new", "cur"} {
		files, err := os.ReadDir(filepath.Join(m.root, dir))
		if err != nil {
			return out, err
		}
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if !f.Type().IsRegular() || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			key, _, _ := strings.Cut(f.Name(), ":2,")
			if m.known[key] {
				continue
			}
			msg, err := readMail(filepath.Join(m.root, dir, f.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("read mail %q: %w", key, err))
				continue
			}
			eventID := "maildir:" + key
			if msg.id != "" {
				eventID = "email:" + msg.id
			}
			seen, err := inbox.Received(eventID)
			if err != nil {
				return out, err
			}
			if seen {
				m.known[key] = true
				continue
			}
			summary := fmt.Sprintf("From %s. Subject %s. %s", snippet(msg.from, 100), snippet(msg.subject, 160), snippet(msg.body, 200))
			if msg.date != "" {
				summary = "Date " + msg.date + ". " + summary
			}
			out = append(out, aloop.Entry{ID: eventID, Source: "email", Ref: key, Summary: summary})
		}
	}
	return out, errors.Join(errs...)
}

func (m *Maildir) Read(ctx context.Context, c aloop.Call) (string, error) {
	if c.Entry.Source != "email" {
		return "", aloop.ToolError("read_email requires an email entry")
	}
	for _, dir := range []string{"new", "cur"} {
		files, err := os.ReadDir(filepath.Join(m.root, dir))
		if err != nil {
			return "", err
		}
		for _, f := range files {
			key, _, _ := strings.Cut(f.Name(), ":2,")
			if key == c.Entry.Ref && f.Type().IsRegular() {
				msg, err := readMail(filepath.Join(m.root, dir, f.Name()))
				if err != nil {
					return "", err
				}
				header := fmt.Sprintf("From: %s\nSubject: %s", msg.from, msg.subject)
				if msg.date != "" {
					header += "\nDate: " + msg.date
				}
				return header + "\n\n" + msg.body, nil
			}
		}
	}
	return "", aloop.ToolError("referenced email is no longer available")
}
