package dashboard

import (
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/internal/personal"
	"golang.org/x/oauth2"
)

type googleConsent struct {
	state, verifier string
	drive           bool
	expires         time.Time
}

func registerGoogle(mux *http.ServeMux, google *personal.GoogleConnection, inbox *aloop.Inbox, password string) {
	redirect, _ := url.Parse(google.RedirectURL())
	origin := redirect.Scheme + "://" + redirect.Host
	cookie := http.Cookie{Name: "aloop-google-state", Path: "/", HttpOnly: true, Secure: redirect.Scheme == "https", SameSite: http.SameSiteLaxMode}
	if cookie.Secure {
		cookie.Name = "__Host-aloop-google-state"
	}
	var mu sync.Mutex
	var pending googleConsent
	private := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			if status := privateStatus(w, r, password); status != http.StatusOK {
				http.Error(w, http.StatusText(status), status)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /oauth/google/connect", private(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin {
			http.Error(w, "Open the dashboard at its configured address.", http.StatusForbidden)
			return
		}
		flow := googleConsent{state: oauth2.GenerateVerifier(), verifier: oauth2.GenerateVerifier(), drive: r.PostFormValue("drive") == "on", expires: time.Now().Add(10 * time.Minute)}
		mu.Lock()
		pending = flow
		mu.Unlock()
		c := cookie
		c.Value = flow.state
		c.MaxAge = 600
		http.SetCookie(w, &c)
		http.Redirect(w, r, google.ConsentURL(flow.state, flow.verifier, flow.drive), http.StatusSeeOther)
	}))
	mux.HandleFunc("GET "+personal.GoogleCallback, private(func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		c, err := r.Cookie(cookie.Name)
		if err != nil || state == "" || c.Value != state {
			http.Error(w, "Invalid Google consent session. Start again from the signal inbox.", http.StatusBadRequest)
			return
		}
		mu.Lock()
		flow := pending
		if flow.state == state {
			pending = googleConsent{}
		}
		mu.Unlock()
		if flow.state != state || time.Now().After(flow.expires) {
			http.Error(w, "Google consent session expired. Start again from the signal inbox.", http.StatusBadRequest)
			return
		}
		expired := cookie
		expired.MaxAge = -1
		http.SetCookie(w, &expired)
		result := "cancelled"
		if code := r.URL.Query().Get("code"); code != "" && r.URL.Query().Get("error") == "" {
			result = "connected"
			if err := google.Connect(r.Context(), inbox, code, flow.verifier, flow.drive); err != nil {
				slog.Warn("Google account connection failed", "error", err)
				result = "failed"
			}
		}
		http.Redirect(w, r, "/inbox?google="+result, http.StatusSeeOther)
	}))
}

func googleMessage(result string) string {
	switch result {
	case "cancelled":
		return "Google authorization was cancelled."
	case "failed":
		return "Could not connect. Check that the Gmail API is enabled and grant Gmail read access, then try again."
	default:
		return ""
	}
}
