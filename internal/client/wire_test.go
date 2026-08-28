package client

import (
	"strings"
	"testing"
)

func TestDecodeWire(t *testing.T) {
	t.Run("whoami", func(t *testing.T) {
		body := `{"principal":"alice","realm":{"name":"payments-dev","namespaces":["payments"],"services":["*"],"profiles":["cpu","heap"],"pgo":{"read":true,"collect":false,"configure":true}},"auth":{"mode":"oidc"}}`
		w, err := Decode[WhoamiResponse]([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if w.Principal != "alice" || w.Realm.Name != "payments-dev" || w.Realm.Namespaces[0] != "payments" || w.Realm.Services[0] != "*" || len(w.Realm.Profiles) != 2 {
			t.Fatalf("decoded %+v", w)
		}
		if !w.Realm.PGO.Read || w.Realm.PGO.Collect || !w.Realm.PGO.Configure {
			t.Fatalf("pgo = %+v", w.Realm.PGO)
		}
	})

	t.Run("limits", func(t *testing.T) {
		body := `{"cpuSeconds":60,"traceSeconds":30,"profiles":["cpu","trace"],"pprof":{"default":{"port":6060},"allowedSelections":[{"port":6061},{"portName":"pprof-alt"},{"port":"*"},{"portName":"*"}]},"pgo":{"enabled":true}}`
		l, err := Decode[LimitsResponse]([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if l.CPUSeconds != 60 || l.TraceSeconds != 30 || len(l.Profiles) != 2 || !l.PGO.Enabled {
			t.Fatalf("decoded %+v", l)
		}
		if got := l.Pprof.Default.String(); got != "port 6060" {
			t.Fatalf("default = %q, want %q", got, "port 6060")
		}
		want := []string{"port 6061", "portName pprof-alt", "port *", "portName *"}
		if len(l.Pprof.AllowedSelections) != len(want) {
			t.Fatalf("allowedSelections = %v", l.Pprof.AllowedSelections)
		}
		for i, sel := range l.Pprof.AllowedSelections {
			if sel.String() != want[i] {
				t.Fatalf("allowedSelections[%d] = %q, want %q", i, sel.String(), want[i])
			}
		}
	})

	t.Run("namespaces", func(t *testing.T) {
		n, err := Decode[NamespacesResponse]([]byte(`{"namespaces":["orders","payments"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(n.Namespaces) != 2 || n.Namespaces[1] != "payments" {
			t.Fatalf("decoded %+v", n)
		}
	})

	t.Run("services", func(t *testing.T) {
		s, err := Decode[ServicesResponse]([]byte(`{"namespace":"payments","services":["checkout","ledger"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if s.Namespace != "payments" || len(s.Services) != 2 {
			t.Fatalf("decoded %+v", s)
		}
	})

	t.Run("targets", func(t *testing.T) {
		body := `{"namespace":"payments","service":"checkout","targets":[{"pod":"checkout-1","node":"worker-1","version":"1.42.0"},{"pod":"checkout-2","node":"worker-2","version":""}]}`
		r, err := Decode[TargetsResponse]([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Targets) != 2 || r.Targets[0].Pod != "checkout-1" || r.Targets[0].Node != "worker-1" || r.Targets[0].Version != "1.42.0" || r.Targets[1].Version != "" {
			t.Fatalf("decoded %+v", r)
		}
	})

	t.Run("two documents are refused", func(t *testing.T) {
		_, err := Decode[NamespacesResponse]([]byte(`{"namespaces":[]} {"namespaces":[]}`))
		if err == nil || !strings.Contains(err.Error(), "after the JSON value") {
			t.Fatalf("err = %v, want the trailing document refused", err)
		}
	})

	t.Run("not the shape is refused", func(t *testing.T) {
		_, err := Decode[NamespacesResponse]([]byte(`{"namespaces":"orders"}`))
		if err == nil {
			t.Fatal("a string where the array belongs decoded")
		}
	})
}
