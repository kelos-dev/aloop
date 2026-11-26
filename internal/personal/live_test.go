package personal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kelos-dev/aloop"
	"github.com/kelos-dev/aloop/backend"
)

func TestE2ELiveDecisionsAndCodexTools(t *testing.T) {
	if os.Getenv("ALOOP_LIVE_CODEX") != "1" {
		t.Skip("set ALOOP_LIVE_CODEX=1 to use a signed-in Codex CLI")
	}
	exe, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	model, err := backend.NewCodex(exe, "", home, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")})
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	for _, dir := range []string{"new", "cur", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeMail := func(id, subject, body string) {
		t.Helper()
		raw := fmt.Sprintf("From: Team <team@example.invalid>\nMessage-ID: <%s@example.invalid>\nSubject: %s\n\n%s\n", id, subject, body)
		if err := os.WriteFile(filepath.Join(root, "new", id), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	today := time.Now().UTC()
	due := today.AddDate(0, 0, 7).Format(time.DateOnly)
	// The deadline and delivery reference are beyond the Maildir preview.
	padding := strings.Repeat("More details follow in the confirmation below. ", 5)
	writeMail("m01", "Revised deadline for the Expense report", "The deadline for our existing task changed. "+padding+"The confirmed due date is "+due+".")
	writeMail("m02", "Member savings", "Our stationery sale starts today. Save 20 percent on notebooks.")
	writeMail("m03", "Next week's office supplies", "Routine office update. "+padding+"The paper shipment arrives Tuesday. Delivery reference: PINE-238.")
	mailbox, err := OpenMaildir(root)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := aloop.OpenInbox(filepath.Join(t.TempDir(), "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbox.Close() })
	ctx := t.Context()
	mail, err := mailbox.Collect(ctx, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Append(mail...); err != nil {
		t.Fatal(err)
	}
	tasks, updates := expenseReportFixture(t, today.AddDate(0, 0, 2).Format(time.DateOnly), due)
	var output bytes.Buffer
	dir := t.TempDir()
	notifications, err := OpenNotifications(dir, &output)
	if err != nil {
		t.Fatal(err)
	}

	decisions, err := backend.NewDecisions("", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	var plans []aloop.Plan
	var batches [][]string
	calls := 0
	triage := func(ctx context.Context, p string, now time.Time, batch []aloop.Entry) (aloop.Plan, error) {
		calls++
		plan, err := decisions.Decide(ctx, p, now, batch)
		if err == nil {
			plans = append(plans, plan)
			var ids []string
			for _, entry := range batch {
				ids = append(ids, entry.ID)
			}
			batches = append(batches, ids)
			t.Logf("Decisions triage: %+v", plan)
		}
		return plan, err
	}
	worker := modelFunc(func(ctx context.Context, p string, input any) (json.RawMessage, error) {
		calls++
		result, err := model.Complete(ctx, p, input)
		if err == nil {
			t.Logf("Codex worker: %s", result)
		}
		return result, err
	})
	const prompt = `Keep existing task deadlines up to date as soon as changes arrive.
Confirm actual changes immediately with the task title and due date in YYYY-MM-DD format.
Ignore advertisements. Save routine office updates for the daily digest.
When the digest is due, send one digest with the current tasks and dates plus any office delivery reference.
Use the source records for exact details.`
	agent, err := aloop.New(inbox, prompt, time.UTC, triage, worker, Tools(mailbox, tasks, notifications)...)
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if err := agent.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	checkIdle := func() {
		t.Helper()
		before := calls
		run()
		if calls != before {
			t.Fatalf("idle poll made %d model calls", calls-before)
		}
	}
	run()
	if len(plans) != 1 {
		t.Fatalf("expected one triage call, got %d", len(plans))
	}
	first := plans[0]
	if !slices.Equal(first.Run, []string{"email:m01@example.invalid"}) {
		t.Fatalf("unexpected mixed-inbox decision: %+v", first)
	}
	if updates() != 1 || !strings.Contains(output.String(), due) || strings.Contains(output.String(), "PINE-238") {
		t.Fatalf("updates=%d, immediate notification=%q", updates(), output.String())
	}
	checkIdle()
	output.Reset()
	daily := Daily(today, 0, 0)
	if err := inbox.Append(daily...); err != nil {
		t.Fatal(err)
	}
	run()
	if len(plans) != 2 {
		t.Fatalf("expected a second triage call for the digest, got %d", len(plans))
	}
	pending := batches[1]
	if len(pending) != 2 || !slices.Contains(pending, "email:m03@example.invalid") || !slices.Contains(pending, daily[0].ID) {
		t.Fatalf("pending digest entries: %v", pending)
	}
	selected := plans[1].Run
	if !slices.Contains(selected, daily[0].ID) {
		t.Fatalf("digest work: %v", selected)
	}
	digest := output.String()
	t.Logf("Delivered digest: %s", digest)
	if !strings.Contains(digest, "PINE-238") || !strings.Contains(digest, due) || updates() != 1 {
		t.Fatalf("updates=%d, digest=%q", updates(), digest)
	}
	if err := inbox.Append(append(mail, daily...)...); err != nil {
		t.Fatal(err)
	}
	checkIdle()
	writeMail("m04", "Member coupon", "Enjoy another 15 percent off stationery this week.")
	newMail, err := mailbox.Collect(ctx, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Append(newMail...); err != nil {
		t.Fatal(err)
	}
	before := calls
	run()
	if calls != before+1 || len(plans) != 3 || !slices.Equal(plans[2].Dismiss, []string{"email:m04@example.invalid"}) {
		t.Fatalf("promotional mail should only invoke triage; got %d calls and decisions %+v", calls-before, plans)
	}
	checkIdle()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("notification files=%v, err=%v", files, err)
	}
}
