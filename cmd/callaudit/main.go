// Command callaudit reads a livekit-sip log and asserts that calls carried
// audio in both directions.
//
// It is the end-to-end audio check for the Matrix<->SIP call path, and it is a
// log reader because the bridge is not in the media path (ADR-0005): nothing
// the bridge can observe distinguishes a working call from a silent one, and
// livekit-sip's `call statistics` line is the only place either end of the
// media path is counted.
//
// One-shot, after placing a test call -- the exit status is the assertion:
//
//	kubectl -n matrix logs deploy/livekit-sip --since=10m | callaudit
//
// Long-running, exporting the same verdicts as Prometheus series. Not a sidecar
// in the livekit-sip pod: a container cannot read another container's stdout,
// so this reads pods/log through the API server.
//
//	kubectl -n matrix logs -f deploy/livekit-sip | callaudit -listen :9102
//
// Exit status: 0 every judged call carried audio both ways; 1 at least one did
// not; 2 the run proved nothing -- no call statistics line was seen, or one
// could not be read. 2 is not folded into 0: a probe that placed no call is the
// failure mode a green check hides.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lhns/matrix-sip-bridge/pkg/callaudit"
)

// Exit statuses. See the package doc.
const (
	exitOK        = 0
	exitNoAudio   = 1
	exitInvalid   = 2
	defaultFailOn = callaudit.AudioSIPOnly + "," + callaudit.AudioMatrixOnly + "," + callaudit.AudioNone
)

type options struct {
	minPackets   int64
	requireCalls int
	failOn       []string
	listen       string
	quiet        bool
}

func main() {
	var (
		minPackets   = flag.Int64("min-packets", callaudit.DefaultMinPackets, "audio packets a call must move in total before it is judged at all; below this it is too_short. Not applied per direction")
		requireCalls = flag.Int("require-calls", 1, "fail with status 2 if fewer than this many calls were found in the log")
		failOn       = flag.String("fail-on", defaultFailOn, "comma-separated verdicts that fail the run")
		listen       = flag.String("listen", "", "serve /metrics on this address and keep reading until the input ends")
		quiet        = flag.Bool("quiet", false, "print the summary only, not one line per call")
	)
	flag.Parse()

	opts := options{
		minPackets:   *minPackets,
		requireCalls: *requireCalls,
		failOn:       splitVerdicts(*failOn),
		listen:       *listen,
		quiet:        *quiet,
	}
	if err := validate(opts); err != nil {
		fmt.Fprintln(os.Stderr, "callaudit:", err)
		os.Exit(exitInvalid)
	}

	var reg *prometheus.Registry
	if opts.listen != "" {
		reg = prometheus.NewRegistry()
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{Addr: opts.listen, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// An exporter that never bound is worse than one that is not
				// there at all, because the series simply stop existing.
				fmt.Fprintln(os.Stderr, "callaudit: listen:", err)
				os.Exit(exitInvalid)
			}
		}()
	}

	os.Exit(run(os.Stdin, os.Stdout, opts, reg))
}

func validate(o options) error {
	if o.minPackets < 1 {
		return fmt.Errorf("-min-packets must be at least 1")
	}
	if o.requireCalls < 0 {
		return fmt.Errorf("-require-calls must not be negative")
	}
	for _, v := range o.failOn {
		if !slices.Contains(callaudit.Verdicts, v) {
			return fmt.Errorf("-fail-on: %q is not one of %s", v, strings.Join(callaudit.Verdicts, ", "))
		}
	}
	return nil
}

func splitVerdicts(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// run reads the log and returns the process exit status. reg is the concrete
// registry rather than a prometheus.Registerer on purpose: a nil *Registry in
// an interface is not a nil interface, so the check below would not be one.
func run(in io.Reader, out io.Writer, o options, reg *prometheus.Registry) int {
	var rec *callaudit.Recorder
	if reg != nil {
		rec = callaudit.NewRecorder(reg)
	}

	counts := map[string]int{}
	total, malformed := 0, 0
	err := callaudit.Scan(in, func(s callaudit.Stats) {
		total++
		counts[s.Audio(o.minPackets)]++
		rec.Observe(s, o.minPackets)
		if !o.quiet {
			_, _ = fmt.Fprintln(out, s.Summary(o.minPackets))
		}
	}, func(e error) {
		malformed++
		rec.Malformed()
		fmt.Fprintln(os.Stderr, "callaudit:", e)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "callaudit: read:", err)
		return exitInvalid
	}

	parts := make([]string, 0, len(counts))
	for v := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", v, counts[v]))
	}
	sort.Strings(parts)
	line := fmt.Sprintf("%d call(s)", total)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, " ")
	}
	if malformed > 0 {
		line += fmt.Sprintf(", %d unreadable", malformed)
	}
	_, _ = fmt.Fprintln(out, line)

	// A log with nothing in it is the failure a green check hides: the probe
	// never placed a call, or it was scraped from the wrong window.
	if total < o.requireCalls {
		fmt.Fprintf(os.Stderr, "callaudit: found %d call(s), want at least %d -- this run proved nothing\n", total, o.requireCalls)
		return exitInvalid
	}
	// A line that announced itself as call statistics and could not be read
	// means the field names moved. Every verdict after that is a guess.
	if malformed > 0 {
		return exitInvalid
	}
	for _, v := range o.failOn {
		if counts[v] > 0 {
			return exitNoAudio
		}
	}
	return exitOK
}
