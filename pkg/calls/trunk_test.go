package calls

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

const (
	testTrunkName     = "test-trunk"
	testTrunkAddress  = "pbx.example.com"
	testTrunkNumber   = "+15550000000"
	testTrunkUser     = "trunk-user"
	testTrunkPassword = "trunk-password-for-tests-only"
)

func trunkSubsystem(f *fakeLiveKit, log zerolog.Logger) *Subsystem {
	s := &Subsystem{lk: f.client(), log: log}
	s.cfg.LiveKit = LiveKitConfig{
		TrunkName:         testTrunkName,
		TrunkAddress:      testTrunkAddress,
		TrunkNumber:       testTrunkNumber,
		TrunkAuthUsername: testTrunkUser,
		TrunkAuthPassword: testTrunkPassword,
	}
	return s
}

// listReply builds a ListSIPOutboundTrunk body holding one trunk.
func listReply(t *testing.T, trunk *SIPOutboundTrunk) string {
	t.Helper()
	body, err := json.Marshal(listSIPOutboundTrunkResponse{Items: []*SIPOutboundTrunk{trunk}})
	if err != nil {
		t.Fatalf("marshal list reply: %v", err)
	}
	return string(body)
}

func TestReconcileTrunkCreatesMissingTrunk(t *testing.T) {
	f := newFakeLiveKit(t)
	f.stage("ListSIPOutboundTrunk", `{"items":[]}`)
	f.stage("CreateSIPOutboundTrunk", `{"sip_trunk_id":"ST_created","name":"`+testTrunkName+`"}`)

	s := trunkSubsystem(f, zerolog.Nop())
	if err := s.reconcileTrunk(t.Context()); err != nil {
		t.Fatalf("reconcileTrunk: %v", err)
	}
	if got := f.calls(); len(got) != 2 || got[1] != "CreateSIPOutboundTrunk" {
		t.Fatalf("methods = %v, want a create", got)
	}
	if id, err := s.currentTrunkID(); err != nil || id != "ST_created" {
		t.Errorf("currentTrunkID() = %q, %v; want ST_created", id, err)
	}
}

func TestReconcileTrunkLeavesMatchingTrunkAlone(t *testing.T) {
	f := newFakeLiveKit(t)
	f.stage("ListSIPOutboundTrunk", listReply(t, &SIPOutboundTrunk{
		SipTrunkID:   "ST_existing",
		Name:         testTrunkName,
		Address:      testTrunkAddress,
		Numbers:      []string{testTrunkNumber},
		AuthUsername: testTrunkUser,
		AuthPassword: testTrunkPassword,
	}))

	s := trunkSubsystem(f, zerolog.Nop())
	for range 3 {
		if err := s.reconcileTrunk(t.Context()); err != nil {
			t.Fatalf("reconcileTrunk: %v", err)
		}
	}
	for _, m := range f.calls() {
		if m != "ListSIPOutboundTrunk" {
			t.Fatalf("methods = %v, want no write RPC", f.calls())
		}
	}
	if id, _ := s.currentTrunkID(); id != "ST_existing" {
		t.Errorf("currentTrunkID() = %q, want ST_existing", id)
	}
}

func TestReconcileTrunkUpdatesDifferingTrunk(t *testing.T) {
	f := newFakeLiveKit(t)
	// The production failure: a trunk registered before the SIP credentials
	// were configured, so every call is rejected for missing auth.
	f.stage("ListSIPOutboundTrunk", listReply(t, &SIPOutboundTrunk{
		SipTrunkID: "ST_existing",
		Name:       testTrunkName,
		Address:    "old.example.com",
		Numbers:    []string{testTrunkNumber},
	}))
	f.stage("UpdateSIPOutboundTrunk", `{"sip_trunk_id":"ST_existing","name":"`+testTrunkName+`"}`)

	var logs bytes.Buffer
	s := trunkSubsystem(f, zerolog.New(&logs))
	if err := s.reconcileTrunk(t.Context()); err != nil {
		t.Fatalf("reconcileTrunk: %v", err)
	}
	got := f.calls()
	if len(got) != 2 || got[1] != "UpdateSIPOutboundTrunk" {
		t.Fatalf("methods = %v, want an update", got)
	}
	if id, err := s.currentTrunkID(); err != nil || id != "ST_existing" {
		t.Errorf("currentTrunkID() = %q, %v; want ST_existing", id, err)
	}

	var req struct {
		SipTrunkID string            `json:"sip_trunk_id"`
		Replace    *SIPOutboundTrunk `json:"replace"`
	}
	if err := json.Unmarshal(f.bodies["UpdateSIPOutboundTrunk"], &req); err != nil {
		t.Fatalf("update body: %v", err)
	}
	if req.SipTrunkID != "ST_existing" {
		t.Errorf("sip_trunk_id = %q, want the existing trunk", req.SipTrunkID)
	}
	// The replace action swaps the whole object, so every field must be sent.
	if req.Replace == nil || req.Replace.Address != testTrunkAddress ||
		req.Replace.AuthUsername != testTrunkUser || req.Replace.AuthPassword != testTrunkPassword ||
		len(req.Replace.Numbers) != 1 {
		t.Errorf("replace = %+v, want the full wanted trunk", req.Replace)
	}

	if !strings.Contains(logs.String(), "auth_password") {
		t.Errorf("log %q does not name the fields that differed", logs.String())
	}
	if strings.Contains(logs.String(), testTrunkPassword) {
		t.Error("the trunk password was logged")
	}
}

// A LiveKit deployment that withholds auth_password from List would otherwise
// show a diff no write can close, rewriting the trunk on every tick.
func TestReconcileTrunkDoesNotRewriteWhenPasswordIsWithheld(t *testing.T) {
	f := newFakeLiveKit(t)
	f.stage("ListSIPOutboundTrunk", listReply(t, &SIPOutboundTrunk{
		SipTrunkID:   "ST_existing",
		Name:         testTrunkName,
		Address:      testTrunkAddress,
		Numbers:      []string{testTrunkNumber},
		AuthUsername: testTrunkUser,
	}))
	f.stage("UpdateSIPOutboundTrunk", `{"sip_trunk_id":"ST_existing","name":"`+testTrunkName+`"}`)

	s := trunkSubsystem(f, zerolog.Nop())
	for range 4 {
		if err := s.reconcileTrunk(t.Context()); err != nil {
			t.Fatalf("reconcileTrunk: %v", err)
		}
	}
	var writes int
	for _, m := range f.calls() {
		if m == "UpdateSIPOutboundTrunk" {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("issued %d updates, want exactly one", writes)
	}
}

// A trunk lost with Redis makes every call fail with no other symptom, and
// nothing outside the log knew: the reconciler reports both halves of its
// outcome so the bridge state can follow it.
func TestTheTrunkReconcilerReportsEveryOutcome(t *testing.T) {
	f := newFakeLiveKit(t)
	f.stageError("ListSIPOutboundTrunk", http.StatusInternalServerError, `{"code":"internal","msg":"redis is gone"}`)
	f.stage("CreateSIPOutboundTrunk", `{"sip_trunk_id":"ST_created","name":"`+testTrunkName+`"}`)

	s := trunkSubsystem(f, zerolog.Nop())
	var reported []error
	s.OnTrunkState(func(err error) { reported = append(reported, err) })

	s.reconcileAndReport(t.Context())
	if len(reported) != 1 || reported[0] == nil {
		t.Fatalf("a LiveKit that is down reported %v, want one failure", reported)
	}

	f.clearError("ListSIPOutboundTrunk")
	f.stage("ListSIPOutboundTrunk", `{"items":[]}`)
	s.reconcileAndReport(t.Context())
	if len(reported) != 2 || reported[1] != nil {
		t.Fatalf("a recreated trunk reported %v, want the recovery too", reported)
	}
}
