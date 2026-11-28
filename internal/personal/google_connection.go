package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kelos-dev/aloop"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

const gmailScope = "https://www.googleapis.com/auth/gmail.readonly"
const driveScope = "https://www.googleapis.com/auth/drive.readonly"
const GoogleCallback = "/oauth/google/callback"

type googleGrant struct {
	ClientID, Email, RefreshToken string
	Drive                         bool
}

// GoogleConnection supplies a stable client for each mail collection or tool call.
type GoogleConnection struct {
	config  oauth2.Config
	ctx     context.Context
	path    string
	mu      sync.Mutex
	current atomic.Pointer[Google]
}

func OpenGoogleConnection(ctx context.Context, appJSON, redirect, path string, client *http.Client) (*GoogleConnection, error) {
	var app struct {
		Web struct {
			ID        string   `json:"client_id"`
			Secret    string   `json:"client_secret"`
			Redirects []string `json:"redirect_uris"`
		}
	}
	if json.Unmarshal([]byte(appJSON), &app) != nil || app.Web.ID == "" || app.Web.Secret == "" {
		return nil, errors.New("Google requires a Web application OAuth client JSON")
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Host == "" || u.User != nil || u.Path != GoogleCallback || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))) ||
		!slices.Contains(app.Web.Redirects, redirect) {
		return nil, errors.New("google_redirect_url must match a registered Google callback URL using HTTPS or loopback HTTP")
	}
	if path == "" || client == nil {
		return nil, errors.New("Google grant path and HTTP client required")
	}
	config := oauth2.Config{ClientID: app.Web.ID, ClientSecret: app.Web.Secret, Endpoint: endpoints.Google, RedirectURL: redirect}
	config.Endpoint.AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	config.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	g := &GoogleConnection{config: config, ctx: context.WithValue(ctx, oauth2.HTTPClient, client), path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	var grant googleGrant
	if json.Unmarshal(data, &grant) != nil || grant.RefreshToken == "" || grant.Email == "" {
		return nil, errors.New("invalid saved Google grant")
	}
	if grant.ClientID == config.ClientID {
		g.current.Store(g.account(&oauth2.Token{RefreshToken: grant.RefreshToken}, grant))
	}
	return g, nil
}

func (g *GoogleConnection) account(token *oauth2.Token, grant googleGrant) *Google {
	return &Google{client: g.config.Client(g.ctx, token), drive: grant.Drive, email: grant.Email}
}

func (g *GoogleConnection) Current() *Google {
	if g == nil {
		return nil
	}
	return g.current.Load()
}

func (g *GoogleConnection) Status() (string, bool) {
	if account := g.Current(); account != nil {
		return account.email, account.drive
	}
	return "", false
}

func (g *GoogleConnection) RedirectURL() string { return g.config.RedirectURL }

func (g *GoogleConnection) ConsentURL(state, verifier string, drive bool) string {
	config := g.config
	config.Scopes = []string{gmailScope}
	if drive {
		config.Scopes = append(config.Scopes, driveScope)
	}
	return config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
}

func (g *GoogleConnection) Connect(ctx context.Context, inbox *aloop.Inbox, code, verifier string, drive bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.ctx.Value(oauth2.HTTPClient))
	token, err := g.config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("exchange Google authorization code: %w", googleRequestError(err))
	}
	if token.RefreshToken == "" {
		return errors.New("Google did not grant offline access. Please connect again.")
	}
	// OAuth permits omission of scope when the grant matches the request.
	if scope, ok := token.Extra("scope").(string); ok {
		scopes := strings.Fields(scope)
		if !slices.Contains(scopes, gmailScope) {
			return errors.New("Gmail read access is required. Please connect again.")
		}
		drive = drive && slices.Contains(scopes, driveScope)
	}
	grant := googleGrant{ClientID: g.config.ClientID, RefreshToken: token.RefreshToken, Drive: drive}
	account := g.account(token, grant)
	started := time.Now().Unix()
	profile, err := account.profile(ctx)
	if err != nil {
		return fmt.Errorf("read Gmail profile: %w", err)
	}
	grant.Email = profile.EmailAddress
	account.email = grant.Email
	source := "gmail:" + grant.Email
	cursor, err := inbox.Cursor(source)
	if err != nil {
		return err
	}
	if cursor == "" {
		if err = inbox.AppendWithCursor(source, gmailCursor(profile.HistoryID, started)); err != nil {
			return err
		}
	}
	if err = g.save(grant); err != nil {
		return err
	}
	g.current.Store(account)
	return nil
}

func (g *GoogleConnection) save(grant googleGrant) error {
	temp, err := os.CreateTemp(filepath.Dir(g.path), ".google-grant-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = json.NewEncoder(temp).Encode(grant); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	return os.Rename(temp.Name(), g.path)
}
