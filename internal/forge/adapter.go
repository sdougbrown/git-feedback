package forge

import "context"

// Session is an authenticated view of one account on one forge.
type Session interface {
	Login() string
}

// CollectOptions carries the parameters of one collection attempt.
type CollectOptions struct {
	ExpectedHead string
}

// CollectResult is the outcome of one collection attempt. Snapshot is nil
// unless the attempt produced a complete inventory.
type CollectResult struct {
	Snapshot   *Snapshot
	HeadBefore string
	HeadAfter  string
	Complete   bool
	Rate       []RateInfo
}

// Adapter is the forge-neutral boundary implemented by each provider.
type Adapter interface {
	Host() string
	ParseTarget(raw string) (Target, error)
	Authenticate(ctx context.Context, account string) (Session, error)
	Collect(ctx context.Context, s Session, t Target, o CollectOptions) (CollectResult, error)
}
