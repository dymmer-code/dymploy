package runner

import "time"

type CacheRequest struct {
	Repository string `json:"repository"`
	Name       string `json:"name,omitempty"`
	Scope      string `json:"scope,omitempty"`
	All        bool   `json:"all,omitempty"`
}
type CacheEntry struct {
	Repository string    `json:"repository"`
	Name       string    `json:"name"`
	Scope      string    `json:"scope"`
	Generation string    `json:"generation"`
	Key        string    `json:"key,omitempty"`
	Bytes      int64     `json:"bytes"`
	LastUsed   time.Time `json:"last_used"`
	InUse      bool      `json:"in_use"`
	Retired    bool      `json:"retired"`
}
type CacheInvalidation struct {
	Invalidated int   `json:"invalidated"`
	Freed       int64 `json:"freed_bytes"`
}
type CacheManager interface {
	ListCaches(CacheRequest) ([]CacheEntry, error)
	InvalidateCaches(CacheRequest) (CacheInvalidation, error)
}
