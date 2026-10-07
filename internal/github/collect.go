package github

import (
	"context"
	"fmt"
	"time"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// Object is one normalized feedback object with its forge fingerprint. The
// synthetic target object carries the stable collected head and is
// metadata, not a finding.
type Object struct {
	Kind        forge.Kind
	ProviderID  string
	Fingerprint string
	Head        string
	Thread      *forge.Thread
	Review      *forge.Review
	Comment     *forge.Comment
}

// Inventory is the normalized output of one complete collection attempt.
type Inventory struct {
	Target         forge.Target
	Head           string
	HeadAfter      string
	CollectedStart time.Time
	CollectedEnd   time.Time
	Objects        []Object
	Rates          []forge.RateInfo
	Snapshot       forge.Snapshot
}

// Collect implements forge.Adapter.
func (a *Adapter) Collect(ctx context.Context, s forge.Session, t forge.Target, o forge.CollectOptions) (forge.CollectResult, error) {
	sess, ok := s.(*Session)
	if !ok || sess.tp == nil {
		return forge.CollectResult{}, fmt.Errorf("%w: collect requires an authenticated GitHub session", forge.ErrAuth)
	}
	inv, err := a.collect(ctx, sess.tp, t, o)
	headAfter := inv.HeadAfter
	if headAfter == "" {
		headAfter = inv.Head
	}
	result := forge.CollectResult{
		Snapshot:   nil,
		HeadBefore: inv.Head,
		HeadAfter:  headAfter,
		Complete:   false,
		Rate:       inv.Rates,
	}
	if err == nil {
		snap := inv.Snapshot
		result.Snapshot = &snap
		result.Complete = true
	}
	return result, err
}

type collectState struct {
	rates []forge.RateInfo
}

// collect reads the head before and after, gathers threads (GraphQL) and
// reviews/comments (REST), and normalizes everything into forge objects.
func (a *Adapter) collect(ctx context.Context, tp *Transport, t forge.Target, o forge.CollectOptions) (Inventory, error) {
	st := &collectState{}
	start := a.clock.Now()
	headBefore, err := a.restHead(ctx, tp, t)
	if err != nil {
		return Inventory{}, err
	}
	if o.ExpectedHead != "" && headBefore != o.ExpectedHead {
		return Inventory{Head: headBefore, Rates: st.rates}, fmt.Errorf("%w: expected head %s but observed %s", forge.ErrHeadChanged, o.ExpectedHead, headBefore)
	}

	threads, err := a.collectThreads(ctx, tp, t, st)
	if err != nil {
		return Inventory{Head: headBefore, Rates: st.rates}, err
	}
	reviews, err := a.collectReviews(ctx, tp, t, st)
	if err != nil {
		return Inventory{Head: headBefore, Rates: st.rates}, err
	}
	comments, err := a.collectComments(ctx, tp, t, st)
	if err != nil {
		return Inventory{Head: headBefore, Rates: st.rates}, err
	}

	headAfter, err := a.restHead(ctx, tp, t)
	if err != nil {
		return Inventory{Head: headBefore, Rates: st.rates}, err
	}
	if headBefore != headAfter {
		return Inventory{Head: headBefore, HeadAfter: headAfter, Rates: st.rates}, fmt.Errorf("%w: head moved from %s to %s during collection", forge.ErrHeadChanged, headBefore, headAfter)
	}

	inv := Inventory{
		Target:         t,
		Head:           headBefore,
		CollectedStart: start,
		CollectedEnd:   a.clock.Now(),
	}
	inv.Snapshot.Head = headBefore
	inv.Snapshot.CollectedStart = start
	inv.Snapshot.CollectedEnd = inv.CollectedEnd
	// Synthetic target object: metadata tracking the stable collected head.
	inv.Objects = append(inv.Objects, Object{Kind: forge.KindTarget, ProviderID: t.ID, Head: inv.Head})

	for _, th := range threads {
		ft, err := normalizeThread(th)
		if err != nil {
			return Inventory{Head: headBefore, Rates: st.rates}, err
		}
		inv.Snapshot.Threads = append(inv.Snapshot.Threads, *ft)
		inv.Objects = append(inv.Objects, Object{
			Kind:        forge.KindThread,
			ProviderID:  th.ID,
			Fingerprint: forge.FingerprintThread(*ft),
			Thread:      ft,
		})
	}
	for _, rv := range reviews {
		fr, err := normalizeReview(rv)
		if err != nil {
			return Inventory{Head: headBefore, Rates: st.rates}, err
		}
		inv.Snapshot.Reviews = append(inv.Snapshot.Reviews, *fr)
		inv.Objects = append(inv.Objects, Object{
			Kind:        forge.KindReview,
			ProviderID:  fmt.Sprintf("%d", rv.ID),
			Fingerprint: forge.FingerprintReview(*fr),
			Review:      fr,
		})
	}
	for _, cm := range comments {
		fc, err := normalizeComment(cm)
		if err != nil {
			return Inventory{Head: headBefore, Rates: st.rates}, err
		}
		inv.Snapshot.Comments = append(inv.Snapshot.Comments, *fc)
		inv.Objects = append(inv.Objects, Object{
			Kind:        forge.KindComment,
			ProviderID:  fmt.Sprintf("%d", cm.ID),
			Fingerprint: forge.FingerprintComment(*fc),
			Comment:     fc,
		})
	}
	return inv, nil
}
