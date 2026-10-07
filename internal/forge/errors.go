package forge

import (
	"errors"
	"fmt"
	"time"
)

// Typed errors returned across the Adapter boundary.
var (
	ErrHeadChanged     = errors.New("head changed during collection")
	ErrIncomplete      = errors.New("collection incomplete")
	ErrAuth            = errors.New("authentication failed")
	ErrNotFound        = errors.New("not found")
	ErrUnsupportedHost = errors.New("unsupported host")
	ErrAccountMismatch = errors.New("authenticated account does not match the requested account")
)

// ErrRateLimited reports that a provider resource is exhausted or backing
// off until Until.
type ErrRateLimited struct {
	Resource string
	Until    time.Time
}

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited on %s until %s", e.Resource, e.Until.UTC().Format(time.RFC3339))
}
