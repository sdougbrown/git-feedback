package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sdougbrown/git-feedback/internal/forge"
)

// restPage is one fetched REST page body together with the pagination
// metadata needed to continue.
type restPage struct {
	Body    json.RawMessage
	Link    string
	Info    forge.RateInfo
	HasInfo bool
}

// restGet performs one REST GET, applying the conditional ETag cache when a
// cached entry exists. A 304 is accepted only when the cached entry carries
// a complete body and pagination metadata; otherwise one unconditional
// refetch is made. The returned entry is the one to store in the cache.
func (a *Adapter) restGet(ctx context.Context, tp *Transport, url string) (restPage, CacheEntry, error) {
	entry, cached := a.cache.Get(url)
	status, header, body, info, hasInfo, err := a.doRESTGet(ctx, tp, url, entry, cached)
	if err != nil {
		return restPage{}, CacheEntry{}, err
	}
	newEntry := CacheEntry{ETag: header.Get("ETag"), Body: body, Link: header.Get("Link"), HasLink: header.Get("Link") != ""}
	if status == http.StatusNotModified {
		if !cached || len(entry.Body) == 0 {
			// The cached representation is incomplete: refetch unconditionally.
			status, header, body, info, hasInfo, err = a.doRESTGet(ctx, tp, url, CacheEntry{}, false)
			if err != nil {
				return restPage{}, CacheEntry{}, err
			}
			newEntry = CacheEntry{ETag: header.Get("ETag"), Body: body, Link: header.Get("Link"), HasLink: header.Get("Link") != ""}
		} else {
			body = entry.Body
			newEntry = entry
			status = http.StatusOK // accepted 304
		}
	}
	if status != http.StatusOK {
		return restPage{}, CacheEntry{}, fmt.Errorf("%w: REST %s returned HTTP %d", forge.ErrIncomplete, url, status)
	}
	return restPage{Body: body, Link: newEntry.Link, Info: info, HasInfo: hasInfo}, newEntry, nil
}

func (a *Adapter) doRESTGet(ctx context.Context, tp *Transport, url string, entry CacheEntry, conditional bool) (int, http.Header, []byte, forge.RateInfo, bool, error) {
	var cond *CacheEntry
	if conditional {
		cond = &entry
	}
	return tp.Get(ctx, url, cond)
}

// restPaginate walks a paginated REST collection via Link rel="next"
// headers, requiring clean termination.
func (a *Adapter) restPaginate(ctx context.Context, tp *Transport, firstURL string) ([]json.RawMessage, []forge.RateInfo, error) {
	var pages []json.RawMessage
	var rates []forge.RateInfo
	seen := map[string]bool{firstURL: true}
	url := firstURL
	for {
		page, entry, err := a.restGet(ctx, tp, url)
		if err != nil {
			return nil, nil, err
		}
		if page.HasInfo {
			rates = append(rates, page.Info)
		}
		a.cache.Put(url, entry)
		if !json.Valid(page.Body) {
			return nil, nil, fmt.Errorf("%w: REST page %s is not valid JSON", forge.ErrIncomplete, url)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(page.Body, &arr); err != nil {
			return nil, nil, fmt.Errorf("%w: REST page %s is not an array", forge.ErrIncomplete, url)
		}
		pages = append(pages, arr...)
		next := nextLink(page.Link)
		if next == "" {
			return pages, rates, nil
		}
		if seen[next] {
			return nil, nil, fmt.Errorf("%w: REST pagination did not terminate (repeated %s)", forge.ErrIncomplete, next)
		}
		seen[next] = true
		url = next
	}
}

// nextLink extracts the rel="next" target from a Link header.
func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		target := ""
		isNext := false
		for _, seg := range strings.Split(part, ";") {
			seg = strings.TrimSpace(seg)
			if strings.HasPrefix(seg, "<") && strings.HasSuffix(seg, ">") {
				target = strings.Trim(seg, "<>")
			} else if seg == `rel="next"` {
				isNext = true
			}
		}
		if isNext && target != "" {
			return target
		}
	}
	return ""
}
