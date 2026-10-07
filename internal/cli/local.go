package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sdougbrown/git-feedback/internal/clock"
	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
	"github.com/sdougbrown/git-feedback/internal/store"
	"github.com/sdougbrown/git-feedback/internal/tracker"
)

// newClock is the clock seam for command handlers; tests substitute a fake.
var newClock = func() clock.Clock { return clock.Real{} }

// stateDir resolves the effective state directory for one invocation:
// the explicit --state-dir, or the pinned per-user default.
func stateDir(inv Invocation) (string, error) {
	if dir := inv.Flags["state-dir"]; dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", &statusError{code: "state_dir", message: fmt.Sprintf("resolve state directory: %v", err)}
	}
	return filepath.Join(base, "git-feedback", "state"), nil
}

// openStore opens the invocation's store with the given clock.
func openStore(inv Invocation, clk clock.Clock) (*store.Store, error) {
	dir, err := stateDir(inv)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dir, store.Options{Clock: clk})
	if err != nil {
		return nil, &statusError{code: "store_error", message: err.Error()}
	}
	return st, nil
}

// parseTarget resolves the invocation URL. Unsupported hosts are rejected
// as an operational error before any credential lookup; malformed URLs and
// non-pull-request GitHub URLs are caller errors.
func parseTarget(inv Invocation) (forge.Target, error) {
	t, err := github.ParseTarget(inv.URL)
	if err == nil {
		return t, nil
	}
	if errors.Is(err, forge.ErrUnsupportedHost) {
		return forge.Target{}, &statusError{code: "unsupported_host", message: err.Error()}
	}
	return forge.Target{}, &usageError{err.Error()}
}

// resolveLocal resolves the target and account for a local command without
// authenticating: an explicit --account wins; otherwise the account is
// inferred only when exactly one stored account matches the target.
// Ambiguity and a missing stream are explicit errors, never guesses.
func resolveLocal(ctx context.Context, st *store.Store, inv Invocation) (forge.Target, string, error) {
	t, err := parseTarget(inv)
	if err != nil {
		return t, "", err
	}
	if account := inv.Flags["account"]; account != "" {
		return t, forge.CanonicalAccount(account), nil
	}
	accounts, err := st.AccountsForTarget(ctx, t.ID)
	if err != nil {
		return t, "", mapStoreError(err)
	}
	switch len(accounts) {
	case 1:
		return t, accounts[0], nil
	case 0:
		return t, "", &statusError{
			code:    "unknown_stream",
			message: fmt.Sprintf("no initialized stream for %s; run reconcile first", t.ID),
		}
	default:
		return t, "", &statusError{
			code:    "ambiguous_account",
			message: fmt.Sprintf("multiple stored accounts (%s) for %s; pass --account", strings.Join(accounts, ", "), t.ID),
		}
	}
}

// parseLimit resolves the shared --limit flag (1–200, default 50).
func parseLimit(inv Invocation) (int, error) {
	limit := store.DefaultInboxLimit
	if raw := inv.Flags["limit"]; raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < store.MinInboxLimit || n > store.MaxInboxLimit {
			return 0, &usageError{
				fmt.Sprintf("invalid --limit %q (expected %d–%d)", raw, store.MinInboxLimit, store.MaxInboxLimit),
			}
		}
		limit = n
	}
	return limit, nil
}

// localEnvelope fills the shared envelope fields for a local command from
// the store: target, account, current snapshot, head, freshness, and the
// latest recorded attempt. No network access occurs.
func localEnvelope(ctx context.Context, command string, st *store.Store, clk clock.Clock, t forge.Target, account string) (Result, error) {
	r := Result{
		Command: command,
		Status:  StatusOK,
		Target:  envelopeTarget(t),
	}
	acct := account
	r.Account = &acct

	if sum, ok, err := st.CurrentSnapshot(ctx, t.ID, account); err != nil {
		return r, mapStoreError(err)
	} else if ok {
		r.Snapshot = snapshotEnvelope(sum)
		head := sum.Head
		r.ObservedHead = &head
		r.Freshness = Freshness{
			SnapshotObservedAt: stamp(sum.ObservedAt),
			Stale:              tracker.IsStale(sum.ObservedAt, clk.Now(), tracker.DefaultMinInterval),
		}
	}
	if att, ok, err := st.LatestAttempt(ctx, t.ID, account); err != nil {
		return r, mapStoreError(err)
	} else if ok {
		r.Attempt = attemptEnvelope(att)
	}
	return r, nil
}

// envelopeTarget maps a forge target onto the envelope target object.
func envelopeTarget(t forge.Target) *Target {
	return &Target{
		ID:     t.ID,
		URL:    t.URL,
		Forge:  t.Forge,
		Host:   t.Host,
		Repo:   t.Repo,
		Number: t.Number,
	}
}

// snapshotEnvelope maps a snapshot summary onto the envelope snapshot object.
// The synthetic target object is excluded from the counts by the store; the
// three feedback kinds are always present.
func snapshotEnvelope(sum store.SnapshotSummary) *Snapshot {
	return &Snapshot{
		ID:             sum.ID,
		CollectedStart: stamp(sum.CollectedStart),
		CollectedEnd:   stamp(sum.CollectedEnd),
		Complete:       true,
		ObjectCounts: map[string]int{
			"thread":  sum.Threads,
			"review":  sum.Reviews,
			"comment": sum.Comments,
		},
	}
}

// attemptEnvelope maps a recorded attempt onto the envelope attempt object.
func attemptEnvelope(a store.Attempt) *Attempt {
	att := &Attempt{
		At:       stamp(a.At),
		OK:       a.Outcome == store.OutcomeOK,
		Complete: a.Complete,
		NextDue:  "",
	}
	if a.HasErrorCode {
		att.ErrorCode = &a.ErrorCode
	}
	if a.HasNextDue {
		att.NextDue = stamp(a.NextDue)
	}
	return att
}

// eventMap renders one stored event as an envelope event record. Head
// fields appear only on head events.
func eventMap(ev store.Event) map[string]any {
	m := map[string]any{
		"id":          ev.ID,
		"kind":        ev.Kind,
		"object_kind": ev.ObjectKind,
		"object_id":   ev.ObjectID,
		"revision":    ev.Revision,
		"url":         ev.URL,
		"snapshot_id": ev.SnapshotID,
		"observed_at": stamp(ev.ObservedAt),
	}
	if ev.HeadBefore != "" {
		m["head_before"] = ev.HeadBefore
	}
	if ev.HeadAfter != "" {
		m["head_after"] = ev.HeadAfter
	}
	return m
}

// stamp renders a time as a UTC RFC 3339 string; the zero time renders as
// the empty string so envelope nulls stay unambiguous.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// mapStoreError converts store errors: caller mistakes (malformed cursor,
// invalid consumer name) are usage errors; everything else is operational.
func mapStoreError(err error) error {
	var se *store.Error
	if !errors.As(err, &se) {
		return err
	}
	switch se.Code {
	case store.CodeInvalidCursor, store.CodeInvalidConsumer:
		return &usageError{se.Message}
	}
	return &statusError{code: se.Code, message: se.Message}
}
