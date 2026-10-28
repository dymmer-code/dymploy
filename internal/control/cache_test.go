package control

import (
	"context"
	"errors"
	"github.com/dymmer-code/dymploy/internal/runner"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeCacheManager struct{}

func (fakeCacheManager) ListCaches(q runner.CacheRequest) ([]runner.CacheEntry, error) {
	if q.Repository == "bad" {
		return nil, errors.New("bad repo")
	}
	return []runner.CacheEntry{{Repository: "github/acme/app", Name: "plt", InUse: true}}, nil
}
func (fakeCacheManager) InvalidateCaches(q runner.CacheRequest) (runner.CacheInvalidation, error) {
	if !q.All && q.Name == "" {
		return runner.CacheInvalidation{}, errors.New("choose name or all")
	}
	return runner.CacheInvalidation{Invalidated: 1, Freed: 12}, nil
}
func TestCacheControlRoundTripAndValidation(t *testing.T) {
	path := sockPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- Serve(ctx, path, Handlers{Status: fixed, Caches: fakeCacheManager{}}, func() { close(ready) })
	}()
	<-ready
	defer func() { cancel(); <-done }()
	cl := &Client{Socket: path}
	items, err := cl.Caches(ctx, runner.CacheRequest{})
	if err != nil || len(items) != 1 || !items[0].InUse {
		t.Fatal(items, err)
	}
	if _, err := cl.Caches(ctx, runner.CacheRequest{Repository: "bad"}); err == nil {
		t.Fatal("list error hidden")
	}
	if _, err := cl.InvalidateCache(ctx, runner.CacheRequest{}); err == nil {
		t.Fatal("invalidation error hidden")
	}
	if r, err := cl.InvalidateCache(ctx, runner.CacheRequest{All: true}); err != nil || r.Invalidated != 1 {
		t.Fatal(r, err)
	}
	h := cacheHandler(fakeCacheManager{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/cache/invalidate", strings.NewReader(`{"unknown":1}`)))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
