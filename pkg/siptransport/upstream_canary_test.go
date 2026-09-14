package siptransport

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// The assertion lives in the test file because no non-test file in this
// package may name sip.ServerTransactionContext — TestNoCallsToServerTransactionContext
// enforces that — and this is the one place allowed to reference it. It fails
// the build if upstream removes, renames or re-signatures the helper, which is
// the moment the canary below stops meaning anything.
var _ func(sip.ServerTransaction) context.Context = sip.ServerTransactionContext

// canaryFile is the only file allowed to name the upstream helper.
const canaryFile = "upstream_canary_test.go"

// upstreamFixedMessage is what a passing fix looks like from here. It is the
// whole point of the canary: the repo carries explanatory comments that have
// to be deleted, not shortened, once upstream is correct.
const upstreamFixedMessage = `sipgo's sip.ServerTransactionContext no longer returns an already-cancelled context: upstream has fixed the inverted OnTerminate check.
Nothing in this bridge needs to change — it never used the helper. Delete this canary test and the comments that explain the bug:
  - Transport.baseCtx in transport.go
  - the handler-context comment in call.go
  - the handler-context comment in message.go
Keep baseCtx itself; it is the transport's lifetime, not a workaround.`

// sip.ServerTransactionContext registers a termination hook and then cancels
// the context whenever registration SUCCEEDED, so the context it returns is
// already done for every live transaction. Handlers here parent off
// Transport.baseCtx instead; this test only watches for the day that stops
// being necessary.
func TestUpstreamServerTransactionContextIsStillBroken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	type observation struct {
		txAlive bool
		err     error
	}
	seen := make(chan observation, 1)

	// A bare sipgo server, not the bridge's Transport: this needs the raw
	// sip.ServerTransaction that the bridge deliberately never exposes.
	srvAddr := serveOptions(t, ctx, func(req *sip.Request, tx sip.ServerTransaction) {
		obs := observation{txAlive: true}
		select {
		case <-tx.Done():
			obs.txAlive = false
		default:
		}
		// Synchronous by construction: the helper cancels before it returns,
		// so there is nothing to wait for and nothing to race with.
		obs.err = sip.ServerTransactionContext(tx).Err()
		seen <- obs
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	peer := newFakePeer(t, ctx)
	host, port, _ := splitHostPort(srvAddr)
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", User: "canary", Host: host, Port: port})
	req.SetTransport("TCP")
	req.SetDestination(srvAddr)
	if _, err := peer.cli.Do(ctx, req); err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}

	var obs observation
	select {
	case obs = <-seen:
	case <-time.After(testTimeout):
		t.Fatal("the OPTIONS handler never ran")
	}
	if !obs.txAlive {
		t.Fatal("the transaction had already terminated inside its own handler; the canary proved nothing")
	}
	if obs.err == nil {
		t.Fatal(upstreamFixedMessage)
	}
}

// serveOptions runs a minimal sipgo server on loopback and returns its address.
func serveOptions(t *testing.T, ctx context.Context, h func(*sip.Request, sip.ServerTransaction)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, _, _ := splitHostPort(ln.Addr().String())
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname(host))
	if err != nil {
		t.Fatalf("canary ua: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("canary server: %v", err)
	}
	srv.OnOptions(h)
	go func() { _ = srv.ServeTCP(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ua.Close()
		_ = ln.Close()
	})
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return ln.Addr().String()
}

// Calling the helper anywhere in the bridge would hand a handler a dead
// context. The check is on the parsed AST, so a call in a comment or a string
// does not count and a renamed import still does.
func TestNoCallsToServerTransactionContext(t *testing.T) {
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" || d.Name() == canaryFile {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		alias := sipPackageName(file)
		if alias == "" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ServerTransactionContext" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias {
				t.Errorf("%s:%d calls sip.ServerTransactionContext; it returns an already-cancelled context, use Transport.baseCtx",
					rel, fset.Position(call.Pos()).Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

// sipPackageName returns the local name of the sipgo sip import, or "" if the
// file does not import it.
func sipPackageName(file *ast.File) string {
	for _, imp := range file.Imports {
		if imp.Path.Value != `"github.com/emiago/sipgo/sip"` {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "sip"
	}
	return ""
}

// moduleRoot walks up from the test's directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root")
		}
		dir = parent
	}
}
