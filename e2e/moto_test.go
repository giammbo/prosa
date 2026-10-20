package e2e

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckMoto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/moto-api/" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := checkMoto(srv.URL); err != nil {
		t.Errorf("reachable Moto: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	l.Close() // nothing listens there now
	if err := checkMoto(dead); err == nil || !strings.Contains(err.Error(), dead) {
		t.Errorf("unreachable Moto: err = %v, want an error naming %s", err, dead)
	}
	if err := checkMoto(srv.URL + "/nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("non-200 Moto: err = %v", err)
	}
}
