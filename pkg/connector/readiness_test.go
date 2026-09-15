package connector

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func probe(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, ReadinessPath, nil))
	return w.Code
}

func TestReadinessIsNotReadyBeforeSIPIsUp(t *testing.T) {
	h := readinessHandler(func() bool { return false })
	if code := probe(t, h); code != http.StatusServiceUnavailable {
		t.Errorf("status %d while SIP was down, want 503", code)
	}
}

func TestReadinessIsReadyOnceSIPIsUp(t *testing.T) {
	up := false
	h := readinessHandler(func() bool { return up })
	if code := probe(t, h); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d before SIP was up, want 503", code)
	}
	up = true
	if code := probe(t, h); code != http.StatusOK {
		t.Errorf("status %d once SIP was up, want 200", code)
	}
}

// Readiness gates the appservice port's Service endpoint too, so it must not
// flap the pod out of rotation when SIP drops after a successful start.
func TestReadinessLatches(t *testing.T) {
	up := true
	h := readinessHandler(func() bool { return up })
	if code := probe(t, h); code != http.StatusOK {
		t.Fatalf("status %d while SIP was up, want 200", code)
	}
	up = false
	if code := probe(t, h); code != http.StatusOK {
		t.Errorf("status %d after SIP dropped, want 200: readiness did not latch", code)
	}
}
