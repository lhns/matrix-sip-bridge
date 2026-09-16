package connector

import (
	"maunium.net/go/mautrix/appservice"

	"github.com/lhns/matrix-sip-bridge/pkg/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricsPath is served on the appservice HTTP port, beside ReadinessPath.
const MetricsPath = "/metrics"

// registerMetrics publishes MetricsPath on the appservice router.
//
// It rides the appservice port so the deployment gains no second container
// port and no second Service entry, which also means /metrics is reachable
// unauthenticated by anything already inside the vnet. That is acceptable
// only because of the cardinality rule at the top of pkg/metrics: nothing
// exposed here names a caller, a room or a call. Adding a label that does
// would turn this endpoint into a leak.
//
// Registered from Init for the same reason as readiness: it is the last point
// before the appservice HTTP server starts serving. Like readiness, it does
// not exist when the bridge is configured for websocket transport or
// no_server; the collectors are still recorded, just not exposed.
func (sc *SIPConnector) registerMetrics(as *appservice.AppService, gatherer prometheus.Gatherer) {
	as.Router.Handle("GET "+MetricsPath, metrics.Handler(gatherer))
}
