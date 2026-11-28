package personal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kelos-dev/aloop"
)

type gmailProfile struct {
	EmailAddress string
	HistoryID    string `json:"historyId"`
}

func (g *Google) profile(ctx context.Context) (gmailProfile, error) {
	var p gmailProfile
	err := g.gmailJSON(ctx, "profile", url.Values{"fields": {"emailAddress,historyId"}}, &p)
	if err == nil && (p.EmailAddress == "" || p.HistoryID == "") {
		err = errors.New("Google returned an incomplete Gmail profile")
	}
	return p, err
}

type gmailID struct{ ID string }
type gmailPage struct {
	History []struct {
		MessagesAdded []struct{ Message gmailID }
	}
	Messages      []gmailID
	NextPageToken string
}

func gmailCursor(history string, since int64) string {
	return history + "/" + strconv.FormatInt(since, 10)
}

// Collect starts at the current inbox position. An expired history ID is recovered
// by listing inbox mail received since the last successful poll that advanced it.
func (g *Google) Collect(ctx context.Context, inbox *aloop.Inbox) error {
	started := time.Now().Unix()
	profile, err := g.profile(ctx)
	if err != nil {
		return err
	}
	source := "gmail:" + profile.EmailAddress
	cursor, err := inbox.Cursor(source)
	if err != nil {
		return err
	}
	history, since, _ := strings.Cut(cursor, "/")
	if history == profile.HistoryID {
		return nil
	}
	var batch []aloop.Entry
	if cursor != "" {
		batch, err = g.collectPages(ctx, inbox, source, "history", url.Values{
			"startHistoryId": {history}, "historyTypes": {"messageAdded"}, "labelId": {"INBOX"},
			"fields": {"history/messagesAdded/message/id,nextPageToken"},
		})
		if errors.Is(err, googleStatus(http.StatusNotFound)) {
			batch, err = g.collectPages(ctx, inbox, source, "messages", url.Values{
				"labelIds": {"INBOX"}, "q": {"after:" + since}, "fields": {"messages/id,nextPageToken"},
			})
		}
		if err != nil {
			return err
		}
	}
	return inbox.AppendWithCursor(source, gmailCursor(profile.HistoryID, started), batch...)
}

func (g *Google) collectPages(ctx context.Context, inbox *aloop.Inbox, source, path string, query url.Values) ([]aloop.Entry, error) {
	var batch []aloop.Entry
	seen := make(map[string]bool)
	for {
		var page gmailPage
		if err := g.gmailJSON(ctx, path, query, &page); err != nil {
			return nil, err
		}
		messages := page.Messages
		for _, change := range page.History {
			for _, added := range change.MessagesAdded {
				messages = append(messages, added.Message)
			}
		}
		for _, msg := range messages {
			if seen[msg.ID] {
				continue
			}
			seen[msg.ID] = true
			entryID := source + ":" + msg.ID
			received, err := inbox.Received(entryID)
			if err != nil {
				return nil, err
			}
			if received {
				continue
			}
			entry, err := g.summary(ctx, msg.ID, entryID)
			if err != nil {
				return nil, err
			}
			if entry != nil {
				batch = append(batch, *entry)
			}
		}
		if page.NextPageToken == "" {
			return batch, nil
		}
		query.Set("pageToken", page.NextPageToken)
	}
}

func (g *Google) summary(ctx context.Context, id, entryID string) (*aloop.Entry, error) {
	var msg struct {
		LabelIDs []string `json:"labelIds"`
		Snippet  string
		Payload  struct {
			Headers []struct{ Name, Value string }
		}
	}
	err := g.gmailJSON(ctx, "messages/"+url.PathEscape(id), url.Values{
		"format": {"metadata"}, "metadataHeaders": {"From", "Subject", "Date"}, "fields": {"labelIds,snippet,payload/headers"},
	}, &msg)
	if errors.Is(err, googleStatus(http.StatusNotFound)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !slices.Contains(msg.LabelIDs, "INBOX") {
		return nil, nil
	}
	header := make(http.Header)
	for _, h := range msg.Payload.Headers {
		header.Set(h.Name, h.Value)
	}
	summary := fmt.Sprintf("Date %s. From %s. Subject %s. %s", snippet(header.Get("Date"), 64), snippet(header.Get("From"), 100), snippet(header.Get("Subject"), 160), snippet(msg.Snippet, 200))
	return &aloop.Entry{ID: entryID, Source: "email", Ref: id, Summary: summary}, nil
}

func (g *Google) readGmail(ctx context.Context, c aloop.Call) (string, error) {
	if c.Entry.ID != "gmail:"+g.email+":"+c.Entry.Ref {
		return "", aloop.ToolError("this message belongs to a different Gmail account")
	}
	var msg struct{ Raw string }
	err := g.gmailJSON(ctx, "messages/"+url.PathEscape(c.Entry.Ref), url.Values{"format": {"raw"}, "fields": {"raw"}}, &msg)
	if errors.Is(err, googleStatus(http.StatusNotFound)) {
		return "", aloop.ToolError("referenced Gmail message is no longer available")
	}
	if err != nil {
		return "", err
	}
	reader := base64.NewDecoder(base64.RawURLEncoding, strings.NewReader(strings.TrimRight(msg.Raw, "=")))
	message, err := parseMail(reader)
	if err != nil {
		return "", err
	}
	return message.text(), nil
}
