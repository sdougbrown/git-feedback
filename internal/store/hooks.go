package store

// callAfterPublishCommit invokes the after-commit hook when set. A panic
// propagates to the caller: the publication is already durable, which is
// exactly the crash point tests must be able to inject.
func (h Hooks) callAfterPublishCommit() {
	if h.AfterPublishCommit != nil {
		h.AfterPublishCommit()
	}
}
