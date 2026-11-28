package dashboard

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"

	"github.com/kelos-dev/aloop"
)

func registerChat(mux *http.ServeMux, inbox *aloop.Inbox, password string) {
	post := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if status := privateStatus(w, r, password); status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid chat form.", http.StatusBadRequest)
			return
		}
		err := inbox.SendChat(r.PostForm.Get("request"), r.PostForm.Get("message"), r.PostForm.Get("reply"))
		if errors.Is(err, aloop.ErrChatMessage) {
			http.Error(w, "Enter a message of up to 4096 characters and select an existing reply target.", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "Unable to save the message.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/chat#compose", http.StatusSeeOther)
	})
	mux.Handle("POST /chat", http.NewCrossOriginProtection().Handler(post))
}

func (v *view) loadChat(inbox *aloop.Inbox, query url.Values) error {
	v.RequestID = rand.Text()
	messages, err := inbox.ChatMessages(query.Get("before"), 51)
	if err != nil {
		return err
	}
	if len(messages) > 50 {
		messages = messages[1:]
		v.Older = messages[0].ID
	}
	v.Chat = messages
	v.Quotes = map[string]string{}
	for index, m := range messages {
		if m.ReplyTo == "" || (m.Role == "assistant" && index > 0 && messages[index-1].ID == m.ReplyTo) {
			continue
		}
		parent, err := inbox.ChatMessage(m.ReplyTo)
		if err != nil {
			return err
		}
		quote := []rune(parent.Text)
		if len(quote) > 140 {
			quote = append(quote[:140], '…')
		}
		v.Quotes[m.ID] = string(quote)
	}
	if id := query.Get("reply"); id != "" {
		m, err := inbox.ChatMessage(id)
		if err != nil {
			return err
		}
		v.Reply = &m
	}
	return nil
}
