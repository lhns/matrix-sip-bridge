package connector

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix/appservice"

	"github.com/lhns/matrix-sip-bridge/pkg/metrics"
)

// The endpoint rides the appservice port, so it is registered from Init like
// readiness is, and it has to answer with every family already present: a
// dashboard built on a series that only appears after the first event cannot
// tell "nothing happened" from "the bridge is not exporting".
func TestMetricsEndpointServesEveryFamily(t *testing.T) {
	reg := metrics.NewRegistry()
	metrics.New(reg)
	as := &appservice.AppService{Router: http.NewServeMux()}
	(&SIPConnector{}).registerMetrics(as, reg)

	w := httptest.NewRecorder()
	as.Router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, MetricsPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, name := range []string{
		"sip_bridge_calls_total",
		"sip_bridge_call_setup_seconds",
		"sip_bridge_sip_registered",
		"sip_bridge_trunk_reconcile_total",
		"sip_bridge_messages_total",
		"sip_bridge_start_timestamp_seconds",
		"sip_bridge_sip_listen_timestamp_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics does not carry %s", name)
		}
	}
}
