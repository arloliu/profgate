package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arloliu/profgate/internal/k8s"
)

func TestWriteTargets(t *testing.T) {
	t.Run("sorted, address-free, version always present", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeTargets(rec, "payment", "payment-api", []k8s.Target{
			{Pod: "b", Node: "n2", Version: "2", PodIP: "10.0.0.5", Port: 6060, UID: "u2"},
			{Pod: "a", Node: "n1", PodIP: "10.0.0.6", Port: 6060, UID: "u1"},
		})
		want := `{"namespace":"payment","service":"payment-api","targets":[` +
			`{"pod":"a","node":"n1","version":""},{"pod":"b","node":"n2","version":"2"}]}` + "\n"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("status %d body %q, want 200 %q", rec.Code, rec.Body.String(), want)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
	})

	t.Run("nil is an empty array", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeTargets(rec, "payment", "payment-api", nil)
		want := `{"namespace":"payment","service":"payment-api","targets":[]}` + "\n"
		if rec.Body.String() != want {
			t.Errorf("body %q, want %q", rec.Body.String(), want)
		}
	})

	t.Run("input order is untouched", func(t *testing.T) {
		targets := []k8s.Target{{Pod: "b"}, {Pod: "a"}}
		writeTargets(httptest.NewRecorder(), "payment", "payment-api", targets)
		if targets[0].Pod != "b" {
			t.Error("writeTargets sorted the caller's slice in place")
		}
	})
}
