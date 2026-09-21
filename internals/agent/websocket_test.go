package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/jobengine"
)

// Regression test for ekilie/ekilied#3: the HTTP 409 from the claim endpoint
// must be recognizable with errors.Is so the engine can skip execution.
func TestClaimJobMaps409ToErrJobAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	cfg := &config.Config{APIURL: srv.URL, SessionToken: "test-session"}
	c := NewWSClient(cfg, context.Background(), nil)

	_, err := c.ClaimJob(context.Background(), 7)
	if !errors.Is(err, jobengine.ErrJobAlreadyClaimed) {
		t.Fatalf("ClaimJob error = %v, want ErrJobAlreadyClaimed", err)
	}
}
