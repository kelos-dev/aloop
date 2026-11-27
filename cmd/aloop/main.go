package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/backend"
	"github.com/kelos-dev/aloop/internal/dashboard"
	"github.com/kelos-dev/aloop/internal/personal"
)

type modelConfig struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	BaseURL      string   `json:"base_url"`
	KeyEnv       string   `json:"key_env"`
	CodexHome    string   `json:"codex_home"`
	RunThreshold *float64 `json:"run_threshold,omitempty"`
}

type config struct {
	Prompt        string      `json:"prompt"`
	Triage        modelConfig `json:"triage"`
	Worker        modelConfig `json:"worker"`
	Maildir       string      `json:"maildir"`
	Inbox         string      `json:"inbox"`
	Notifications string      `json:"notifications"`
	Timezone      string      `json:"timezone"`
	DailyAt       string      `json:"daily_at"`
	WebListen     string      `json:"web_listen"`
}

func cliEnvironment() []string {
	var env []string
	for _, name := range []string{"HOME", "PATH", "LANG", "TMPDIR", "SystemRoot"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func makeModel(c modelConfig) (aloop.Model, error) {
	if c.RunThreshold != nil {
		return nil, errors.New("run_threshold applies only to Decisions triage")
	}
	switch c.Provider {
	case "openai-compatible":
		key := c.KeyEnv
		if key == "" {
			key = "OPENAI_API_KEY"
		}
		return backend.NewAPI(c.BaseURL, c.Model, os.Getenv(key))
	case "claude-code":
		bin, err := exec.LookPath("claude")
		if err != nil {
			return nil, err
		}
		return backend.NewClaude(bin, c.Model, os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), cliEnvironment())
	case "codex":
		bin, err := exec.LookPath("codex")
		if err != nil {
			return nil, err
		}
		home, err := codexHome(c.CodexHome)
		if err != nil {
			return nil, err
		}
		return backend.NewCodex(bin, c.Model, home, cliEnvironment())
	default:
		return nil, fmt.Errorf("unknown model provider %q", c.Provider)
	}
}

func codexHome(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func makeTriage(c modelConfig) (aloop.Triage, error) {
	if c.Provider != "" && c.Provider != "openai-decisions" {
		model, err := makeModel(c)
		if err != nil {
			return nil, err
		}
		return aloop.PromptTriage(model), nil
	}
	if c.BaseURL != "" || (c.Model != "" && c.Model != backend.DecisionsModel) {
		return nil, errors.New("OpenAI Decisions uses the OpenAI endpoint and model " + backend.DecisionsModel)
	}
	if c.KeyEnv != "" && c.CodexHome != "" {
		return nil, errors.New("choose either key_env or codex_home for Decisions")
	}
	key := os.Getenv("OPENAI_API_KEY")
	home := c.CodexHome
	if c.KeyEnv != "" {
		key = os.Getenv(c.KeyEnv)
		if key == "" {
			return nil, fmt.Errorf("empty Decisions credential %s", c.KeyEnv)
		}
	} else if home != "" {
		key = ""
	}
	var bin string
	var err error
	if key == "" {
		home, err = codexHome(home)
		if err != nil {
			return nil, err
		}
		bin, _ = exec.LookPath("codex")
	}
	var d *backend.Decisions
	if bin != "" {
		d, err = backend.NewCodexDecisions(bin, home, cliEnvironment(), c.RunThreshold)
	} else {
		d, err = backend.NewDecisions(key, home, c.RunThreshold)
	}
	if err != nil {
		return nil, err
	}
	return d.Decide, nil
}

func run(ctx context.Context, path string, once bool, interval time.Duration) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	var cfg config
	if err = dec.Decode(&cfg); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing configuration data")
	}
	if interval <= 0 || cfg.Inbox == "" || cfg.Maildir == "" || cfg.Timezone == "" {
		return errors.New("positive poll interval, inbox, maildir and timezone required")
	}
	zone, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return err
	}
	at, err := time.Parse("15:04", cfg.DailyAt)
	if err != nil {
		return err
	}
	triage, err := makeTriage(cfg.Triage)
	if err != nil {
		return err
	}
	worker, err := makeModel(cfg.Worker)
	if err != nil {
		return err
	}
	m, err := personal.OpenMaildir(cfg.Maildir)
	if err != nil {
		return err
	}
	var tasks *personal.Tasks
	if token := os.Getenv("TODOIST_API_TOKEN"); token != "" {
		tasks, err = personal.NewTasks("https://api.todoist.com/api/v1", token)
		if err != nil {
			return err
		}
	}
	notifications, err := personal.OpenNotifications(cfg.Notifications, os.Stdout)
	if err != nil {
		return err
	}
	inbox, err := aloop.OpenInbox(cfg.Inbox)
	if err != nil {
		return err
	}
	defer inbox.Close()
	agent, err := aloop.New(inbox, cfg.Prompt, zone, triage, worker, personal.Tools(m, tasks, notifications)...)
	if err != nil {
		return err
	}
	collect := func() error {
		mail, err := m.Collect(ctx, inbox)
		batch := append(mail, personal.Daily(time.Now().In(zone), at.Hour(), at.Minute())...)
		return errors.Join(err, inbox.Append(batch...))
	}
	first := collect()
	if once {
		return errors.Join(first, agent.RunOnce(ctx))
	}
	if first != nil {
		slog.Error("Signal collection failed", "error", first)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if cfg.WebListen != "" {
		listener, err := net.Listen("tcp", cfg.WebListen)
		if err != nil {
			return err
		}
		server := &http.Server{
			Handler:           dashboard.Handler(inbox, notifications, zone, cfg.DailyAt, os.Getenv("ALOOP_WEB_PASSWORD")),
			ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute,
		}
		defer func() {
			stop, release := context.WithTimeout(context.Background(), 5*time.Second)
			defer release()
			if err := server.Shutdown(stop); err != nil {
				server.Close()
			}
		}()
		go func() {
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				cancel(err)
			}
		}()
		slog.Info("Dashboard listening", "address", listener.Addr())
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collect(); err != nil {
					slog.Error("Signal collection failed", "error", err)
				}
			}
		}
	}()
	err = agent.Run(ctx, interval)
	cancel(err)
	<-stopped
	return context.Cause(ctx)
}

func main() {
	path := flag.String("config", "aloop.json", "Agent configuration file")
	once := flag.Bool("once", false, "Collect signals and process one snapshot")
	interval := flag.Duration("poll", 30*time.Second, "Signal polling and retry interval")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *path, *once, *interval); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("Agent stopped", "error", err)
		os.Exit(1)
	}
}
