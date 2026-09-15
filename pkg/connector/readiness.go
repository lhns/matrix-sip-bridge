package connector

import (
	"net/http"
	"sync/atomic"

	"maunium.net/go/mautrix/appservice"
)

// ReadinessPath is served on the appservice HTTP port and is what a Kubernetes
// readiness probe should check. Upstream's /_matrix/mau/ready is true as soon
// as the Matrix connector is up, which is before the SIP listener binds.
const ReadinessPath = "/_health/ready"

// registerReadiness publishes ReadinessPath on the appservice router.
//
// Matrix.Start finishes -- appservice listening, AS.Ready set -- up to 30s
// before Network.Start binds the SIP port, because its homeserver ping retries
// six times with a 5s sleep. A probe that only checks the appservice port
// reports Ready in that window and the SIP peer's INVITEs are refused.
func (sc *SIPConnector) registerReadiness(as *appservice.AppService) {
	as.Router.Handle("GET "+ReadinessPath, readinessHandler(func() bool {
		return as.Ready && sc.sipReady()
	}))
}

// readinessHandler answers 200 once ready has been true, and 503 until then.
//
// It latches on purpose. Readiness also gates the Service endpoint for the
// appservice port, so letting a dropped SIP registration flip the pod out of
// the Service would take the whole bridge out of rotation -- Matrix side
// included -- for a fault that only degrades it. Closing the startup window is
// the entire job here; a running bridge reports its faults as bridge state.
func readinessHandler(ready func() bool) http.HandlerFunc {
	var latched atomic.Bool
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !latched.Load() && !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready": false}`))
			return
		}
		latched.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ready": true}`))
	}
}
