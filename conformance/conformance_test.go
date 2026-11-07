package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AndreaAcanfora/openheartbeat/server"
)

func TestReferenceServerConforms(t *testing.T) {
	s := server.New(server.Config{Key: "0123456789abcdef0123456789abcdef"})
	s.AddCheck("job", time.Hour, time.Minute)
	ts := httptest.NewServer(s)
	defer ts.Close()

	results, err := Run(context.Background(), ts.Client(), s.CheckURL(ts.URL, "job"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s %s: %s", r.Level, r.Name, r.Detail)
		}
	}
	if st, _ := s.Status("job"); st.State != server.StateUp {
		t.Errorf("suite left the check %s, want up", st.State)
	}
}

// A server that answers 200 to everything must fail the rejection cases.
func TestDetectsNonConformingServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))
	defer ts.Close()

	results, err := Run(context.Background(), ts.Client(), ts.URL+"/ping/x/job")
	if err != nil {
		t.Fatal(err)
	}
	if Passed(results) {
		t.Fatal("permissive server reported as conforming")
	}
	failed := map[string]bool{}
	for _, r := range results {
		if !r.Passed {
			failed[r.Name] = true
		}
	}
	for _, name := range []string{"exit code 256 is rejected", "invalid rid is rejected", "unknown suffix is rejected"} {
		if !failed[name] {
			t.Errorf("%q should fail", name)
		}
	}
}
