package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sdougbrown/git-feedback/internal/store"
)

// stdin is the reader for --events-from -; tests substitute a buffer.
var stdin io.Reader = os.Stdin

// handleAck acknowledges exactly the supplied event IDs for one consumer and
// stream, transactionally and idempotently. Unknown or out-of-scope IDs
// reject the whole request. No authentication or network access occurs.
func handleAck(ctx context.Context, inv Invocation) (Result, error) {
	consumer := inv.Flags["consumer"]
	if consumer == "" {
		return Result{}, &usageError{"ack requires --consumer <NAME>"}
	}
	ids, err := resolveAckEventIDs(inv)
	if err != nil {
		return Result{}, err
	}
	if len(ids) == 0 {
		return Result{}, &usageError{"ack requires at least one --event <ID>"}
	}
	clk := newClock()
	st, err := openStore(inv, clk)
	if err != nil {
		return Result{}, err
	}
	defer st.Close()

	t, account, err := resolveLocal(ctx, st, inv)
	if err != nil {
		return Result{}, err
	}
	r, err := localEnvelope(ctx, inv.Command, st, clk, t, account)
	if err != nil {
		return Result{}, err
	}
	if _, err := st.Ack(ctx, store.AckInput{
		TargetID: t.ID,
		Account:  account,
		Consumer: consumer,
		EventIDs: ids,
	}); err != nil {
		return Result{}, mapStoreError(err)
	}
	// Echo the acknowledged IDs so the caller can confirm the receipt.
	r.Events = make([]any, 0, len(ids))
	for _, id := range ids {
		r.Events = append(r.Events, id)
	}
	return r, nil
}

// resolveAckEventIDs collects the event IDs for one ack: comma-separated
// --event values plus, when --events-from - is set, the IDs read from
// stdin. Duplicates are removed, order preserved.
func resolveAckEventIDs(inv Invocation) ([]string, error) {
	var ids []string
	for _, v := range inv.Events {
		for _, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	if raw := inv.Flags["events-from"]; raw != "" {
		if raw != "-" {
			return nil, &usageError{"--events-from only supports '-' (stdin)"}
		}
		fromStdin, err := readEventIDsFromStdin(stdin)
		if err != nil {
			return nil, err
		}
		ids = append(ids, fromStdin...)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// readEventIDsFromStdin parses stdin as one ID per line, a JSON array of
// ID strings, or a full inbox --ids-only result envelope. A form that
// yields zero IDs is a usage error, never a silent success.
func readEventIDsFromStdin(r io.Reader) ([]string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, &usageError{fmt.Sprintf("read --events-from stdin: %v", err)}
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, &usageError{"--events-from - read no event IDs from stdin"}
	}
	var ids []string
	switch {
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal([]byte(trimmed), &ids); err != nil {
			return nil, &usageError{fmt.Sprintf("--events-from stdin is not a JSON array of ID strings: %v", err)}
		}
	case strings.HasPrefix(trimmed, "{"):
		var envelope struct {
			Events  []any `json:"events"`
			HasMore bool  `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
			return nil, &usageError{fmt.Sprintf("--events-from stdin is not a result envelope: %v", err)}
		}
		ids = make([]string, 0, len(envelope.Events))
		for _, e := range envelope.Events {
			s, ok := e.(string)
			if !ok {
				return nil, &usageError{"ack --events-from - received event records, not ID strings; rerun inbox with --ids-only"}
			}
			ids = append(ids, s)
		}
		if envelope.HasMore {
			return nil, &usageError{"ack --events-from - received a truncated inbox page (has_more: true); rerun inbox with --limit 200 or page with --after"}
		}
	default:
		ids = make([]string, 0)
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				ids = append(ids, line)
			}
		}
	}
	// The JSON forms skip no whitespace of their own, so empty and
	// whitespace-only entries are dropped here exactly as the line and
	// comma forms drop them.
	kept := ids[:0]
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			kept = append(kept, id)
		}
	}
	ids = kept
	if len(ids) == 0 {
		return nil, &usageError{"--events-from - read no event IDs from stdin"}
	}
	return ids, nil
}
