package store

import (
	"database/sql"

	"github.com/sdougbrown/git-feedback/internal/forge"
	"github.com/sdougbrown/git-feedback/internal/github"
)

// apiRepresentation is the Accept/API-version string the transport sends on
// every request; it is part of the cache key so representations never mix.
const apiRepresentation = "application/vnd.github+json"

// HTTPCache is the store-backed github.HTTPCache for one (host, account)
// scope. Entries are keyed by (host, account, full URL, representation) and
// store the ETag, the complete body, and the pagination metadata.
type HTTPCache struct {
	db             *sql.DB
	host           string
	account        string
	representation string
}

// NewHTTPCache returns the HTTP cache bound to one (host, account) scope.
func (s *Store) NewHTTPCache(host, account string) *HTTPCache {
	return &HTTPCache{
		db:             s.db,
		host:           host,
		account:        forge.CanonicalAccount(account),
		representation: apiRepresentation,
	}
}

// Get implements github.HTTPCache.
func (c *HTTPCache) Get(key string) (github.CacheEntry, bool) {
	var e github.CacheEntry
	var link sql.NullString
	var hasLink int
	err := c.db.QueryRow(
		`SELECT etag, body, link, has_link FROM http_cache
		 WHERE host = ? AND account = ? AND url = ? AND representation = ?`,
		c.host, c.account, key, c.representation).Scan(&e.ETag, &e.Body, &link, &hasLink)
	if err != nil {
		return github.CacheEntry{}, false
	}
	e.Link = link.String
	e.HasLink = hasLink != 0
	return e, true
}

// Put implements github.HTTPCache.
func (c *HTTPCache) Put(key string, entry github.CacheEntry) {
	var link sql.NullString
	if entry.HasLink {
		link = sql.NullString{String: entry.Link, Valid: true}
	}
	_, _ = c.db.Exec(
		`INSERT INTO http_cache (host, account, url, representation, etag, body, link, has_link)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, account, url, representation) DO UPDATE SET
		   etag = excluded.etag, body = excluded.body,
		   link = excluded.link, has_link = excluded.has_link`,
		c.host, c.account, key, c.representation,
		entry.ETag, entry.Body, link, boolToInt(entry.HasLink))
}
