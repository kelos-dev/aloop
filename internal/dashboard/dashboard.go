// Package dashboard serves the personal agent's web interface.
package dashboard

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/internal/personal"
)

//go:embed page.html assets
var files embed.FS

var page = template.Must(template.ParseFS(files, "page.html"))

type group struct {
	Label, Icon string
	Count       int
}

type view struct {
	Title, Path, Time, Date, Daily, Zone, Filter  string
	Received, Pending, Handled, Deferred, Effects int
	Sources, Actions                              []group
	Signals                                       []aloop.PendingSignal
	Messages                                      []personal.Notification
	Location                                      *time.Location
	Locked                                        bool
	PrivateEnabled                                bool
	GoogleConfigured, DriveConnected              bool
	GoogleEmail, GoogleMessage                    string
}

// Handler serves public totals and password-protected personal content.
func Handler(inbox *aloop.Inbox, notifications *personal.Notifications, zone *time.Location, daily, password string, google *personal.GoogleConnection) http.Handler {
	mux := http.NewServeMux()
	if google != nil {
		registerGoogle(mux, google, inbox, password)
	}
	assets, err := fs.Sub(files, "assets")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /assets/{name}", http.StripPrefix("/assets", http.FileServerFS(assets)))
	render := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; font-src 'self'; img-src 'self'; form-action 'self' https://accounts.google.com; frame-ancestors 'none'; base-uri 'none'")
		now := time.Now().In(zone)
		v := view{
			Path: r.URL.Path, Date: now.Format("Mon, 02 Jan 2006"), Time: now.Format("15:04:05 MST"),
			Daily: daily, Zone: zone.String(), Location: zone, PrivateEnabled: password != "",
		}
		v.Title = map[string]string{"/": "Overview", "/inbox": "Signal inbox", "/notifications": "Notifications"}[v.Path]
		code := http.StatusOK
		if v.Path != "/" {
			code = privateStatus(w, r, password)
			v.Locked = code != http.StatusOK
		}
		if v.Path == "/inbox" && !v.Locked && google != nil {
			v.GoogleConfigured = true
			v.GoogleEmail, v.DriveConnected = google.Status()
			v.GoogleMessage = googleMessage(r.URL.Query().Get("google"))
		}
		var err error
		if !v.Locked {
			if v.Path == "/notifications" {
				v.Messages, err = notifications.Recent(30)
			} else {
				var status aloop.InboxStatus
				status, err = inbox.Status()
				if err == nil {
					v.summarize(status, r.URL.Query().Get("state"))
				}
			}
		}
		var buf bytes.Buffer
		if err == nil {
			err = page.Execute(&buf, v)
		}
		if err != nil {
			slog.Error("Dashboard request failed", "path", v.Path, "error", err)
			http.Error(w, "Unable to load the dashboard.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		_, _ = buf.WriteTo(w)
	}
	for _, pattern := range []string{"GET /{$}", "GET /inbox", "GET /notifications"} {
		mux.HandleFunc(pattern, render)
	}
	return mux
}

func privateStatus(w http.ResponseWriter, r *http.Request, password string) int {
	if password == "" {
		return http.StatusForbidden
	}
	user, supplied, ok := r.BasicAuth()
	got, want := sha256.Sum256([]byte(supplied)), sha256.Sum256([]byte(password))
	if !ok || user != "aloop" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="aloop", charset="UTF-8"`)
		return http.StatusUnauthorized
	}
	return http.StatusOK
}

func (v *view) summarize(s aloop.InboxStatus, filter string) {
	v.Received, v.Pending = s.Received, len(s.Pending)
	v.Handled = v.Received - v.Pending
	if filter == "pending" || filter == "deferred" {
		v.Filter = filter
	}
	sources := map[string]int{}
	for _, signal := range s.Pending {
		sources[signal.Source]++
		if signal.Deferred {
			v.Deferred++
		}
		if v.Path == "/inbox" && (v.Filter == "" || (v.Filter == "deferred") == signal.Deferred) {
			v.Signals = append(v.Signals, signal)
		}
	}
	v.Sources = []group{{"Email", "mail", sources["email"]}, {"Daily schedule", "calendar", sources["schedule"]}}
	if other := v.Pending - sources["email"] - sources["schedule"]; other > 0 {
		v.Sources = append(v.Sources, group{"Other signals", "signal", other})
	}
	for _, count := range s.Effects {
		v.Effects += count
	}
	v.Actions = []group{{"Notifications sent", "bell", s.Effects["notify"]}, {"Tasks created", "check", s.Effects["create_task"]}, {"Due dates updated", "calendar", s.Effects["update_due"]}}
	if other := v.Effects - s.Effects["notify"] - s.Effects["create_task"] - s.Effects["update_due"]; other > 0 {
		v.Actions = append(v.Actions, group{"Other actions", "bolt", other})
	}
}
