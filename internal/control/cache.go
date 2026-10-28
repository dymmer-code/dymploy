package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/dymmer-code/dymploy/internal/runner"
)

func cacheHandler(m runner.CacheManager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/cache", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		items, err := m.ListCaches(runner.CacheRequest{Repository: q.Get("repository"), Name: q.Get("name"), Scope: q.Get("scope")})
		if err != nil {
			reply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, items)
	})
	mux.HandleFunc("POST /v1/cache/invalidate", func(w http.ResponseWriter, r *http.Request) {
		var q runner.CacheRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil {
			reply(w, 400, map[string]string{"error": "invalid cache request"})
			return
		}
		out, err := m.InvalidateCaches(q)
		if err != nil {
			reply(w, 422, map[string]string{"error": err.Error()})
			return
		}
		reply(w, 200, out)
	})
	return mux
}
func (c *Client) Caches(ctx context.Context, q runner.CacheRequest) ([]runner.CacheEntry, error) {
	var out []runner.CacheEntry
	v := url.Values{"repository": {q.Repository}, "name": {q.Name}, "scope": {q.Scope}}
	err := c.call(ctx, http.MethodGet, "/v1/cache?"+v.Encode(), &out)
	return out, err
}
func (c *Client) InvalidateCache(ctx context.Context, q runner.CacheRequest) (runner.CacheInvalidation, error) {
	var out runner.CacheInvalidation
	raw, _ := json.Marshal(q)
	var response struct {
		runner.CacheInvalidation
		Error string `json:"error"`
	}
	code, err := c.doBody(ctx, http.MethodPost, "/v1/cache/invalidate", raw, &response)
	out = response.CacheInvalidation
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("daemon answered %d: %s", code, response.Error)
	}
	return out, err
}
