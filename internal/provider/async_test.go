package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ⛔ A PASS HERE IS NOT EVIDENCE THE PROVIDER CAN BUILD A VM. These servers
// answer the shapes internal/api answers, so the waiting, the precondition and
// the retries can be exercised without an estate. Whether the field names on
// the wire are the ones the control plane reads is proven by walk gate 5's
// Terraform act against a live control plane, and only there.

// fastPolls takes the poll interval down for the length of one test. Without
// it every wait below would take five seconds a read.
func fastPolls(t *testing.T) {
	t.Helper()
	was := asyncPollInterval
	asyncPollInterval = time.Millisecond
	t.Cleanup(func() { asyncPollInterval = was })
}

func jsonRow(w http.ResponseWriter, row asyncRow) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(row)
}

// recorder keeps what the handler saw, under a lock, so -race is quiet.
type recorder struct {
	mu       sync.Mutex
	ifMatch  []string
	idemKeys []string
}

func (r *recorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ifMatch = append(r.ifMatch, req.Header.Get("If-Match"))
	r.idemKeys = append(r.idemKeys, req.Header.Get("Idempotency-Key"))
}

func (r *recorder) seen() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ifMatch...), append([]string(nil), r.idemKeys...)
}

// TestWaitForState_PollsUntilTheTargetState — a create must not report success
// on the first read. The row says Provisioning twice before it says Active, and
// a wait that returned early would hand Terraform a network that is not up.
func TestWaitForState_PollsUntilTheTargetState(t *testing.T) {
	fastPolls(t)
	var reads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		state := "Provisioning"
		if atomic.AddInt32(&reads, 1) >= 3 {
			state = "Active"
		}
		jsonRow(w, asyncRow{ID: "net-1", ActualState: state, UpdatedAt: "2026-10-10T09:00:00Z"})
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/networks/net-1",
		kind: "network", target: "Active", failed: []string{"Failed"}}
	row, _, err := w.wait(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row.ActualState != "Active" {
		t.Errorf("returned state %q, want Active", row.ActualState)
	}
	if n := atomic.LoadInt32(&reads); n < 3 {
		t.Errorf("returned after %d read(s); the row only said Active on the third", n)
	}
}

// TestWaitForState_FailedStateIsAnError — the positive control first: the same
// wait against the same shape returns no error when the state is the target.
// Without that control, an error for Failed would also be produced by a wait
// that errors on everything.
//
// ⚠️ The elapsed time is part of the assertion, and it is the half that means
// something. A wait with NO failed-state check also returns an error for a
// datastore stuck in Failed — it just spends the whole budget first and calls it
// a timeout. On a real apply that is thirty minutes of `terraform apply` sitting
// on a datastore the control plane gave up on in the first second.
func TestWaitForState_FailedStateIsAnError(t *testing.T) {
	fastPolls(t)
	const budget = 3 * time.Second
	waitOn := func(state string) (time.Duration, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			jsonRow(w, asyncRow{ID: "ds-1", ActualState: state})
		}))
		t.Cleanup(srv.Close)
		w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/datastores/ds-1",
			kind: "datastore", target: "Active", failed: []string{"Failed"}}
		at := time.Now()
		_, _, err := w.wait(context.Background(), budget)
		return time.Since(at), err
	}
	if _, err := waitOn("Active"); err != nil {
		t.Fatalf("control: a datastore that reached Active reported %v", err)
	}
	took, err := waitOn("Failed")
	if err == nil {
		t.Fatal("a datastore in Failed was reported as created")
	}
	if !strings.Contains(err.Error(), "Failed") {
		t.Errorf("the error does not name the state it saw: %v", err)
	}
	if !strings.Contains(err.Error(), "will not leave") {
		t.Errorf("the error does not say the state is terminal, so it reads as a timeout: %v", err)
	}
	if took > budget/2 {
		t.Errorf("the wait spent %s of its %s budget on a state the control plane will not leave; "+
			"it gave up on time rather than on the state", took, budget)
	}
}

// TestWaitForState_TimeoutNamesTheJobState — vvm-iac-spec.md §2.1: "on timeout,
// surface the actual VVM job state, don't silently succeed". A bare "timed out"
// tells an operator nothing they can act on.
func TestWaitForState_TimeoutNamesTheJobState(t *testing.T) {
	fastPolls(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs/job-1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"job-1","kind":"vm.schedule","state":"failed","attempts":3,"error_message":"no host had room"}`))
			return
		}
		jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Scheduling"})
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", jobID: "job-1",
		kind: "VM", target: "Running", failed: []string{"Failed", "ScheduleFailed"}}
	_, _, err := w.wait(context.Background(), 20*time.Millisecond)
	if err == nil {
		t.Fatal("a VM that never reached Running was reported as created")
	}
	for _, want := range []string{"Scheduling", "Running", "job-1", "vm.schedule", "failed", "no host had room"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the timeout error does not mention %q: %v", want, err)
		}
	}
}

// TestWaitGone_PollsUntilTheRowIsGone — a delete is not done when the API
// accepts it. The row goes when the job behind it finishes.
func TestWaitGone_PollsUntilTheRowIsGone(t *testing.T) {
	fastPolls(t)
	var reads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&reads, 1) >= 3 {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Deleting"})
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	if err := w.waitGone(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := atomic.LoadInt32(&reads); n < 3 {
		t.Errorf("stopped waiting after %d read(s); the row was only gone on the third", n)
	}
}

// TestWaitGone_TimeoutSaysWhatItLastRead — a destroy that gave up must say the
// resource is still there, and in what state, or the next operator reads the
// failure as a network fault.
func TestWaitGone_TimeoutSaysWhatItLastRead(t *testing.T) {
	fastPolls(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Deleting"})
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	err := w.waitGone(context.Background(), 20*time.Millisecond)
	if err == nil {
		t.Fatal("a VM that is still there was reported as deleted")
	}
	for _, want := range []string{"still there", "Deleting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestDeleteAndWait_SendsIfMatchFromAFreshRead — the one defect that makes
// every destroy fail: sending the updated_at Terraform stored at create. The
// row's timestamp moves on its own (the scheduler, a host report, a usage
// report), so the delete must carry what a read taken NOW returns.
func TestDeleteAndWait_SendsIfMatchFromAFreshRead(t *testing.T) {
	fastPolls(t)
	const now = "2026-10-10T09:30:00.123456789Z"
	rec := &recorder{}
	var deleted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			rec.note(r)
			atomic.StoreInt32(&deleted, 1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"vm-1","job_id":"job-9"}`))
		default:
			if atomic.LoadInt32(&deleted) == 1 {
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
				return
			}
			jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Running", UpdatedAt: now})
		}
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	if err := w.deleteAndWait(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ifMatch, idem := rec.seen()
	if len(ifMatch) != 1 {
		t.Fatalf("the delete was sent %d time(s), want 1", len(ifMatch))
	}
	if ifMatch[0] != now {
		t.Errorf("If-Match was %q, want the updated_at the read answered (%q)", ifMatch[0], now)
	}
	if idem[0] == "" {
		t.Error("the delete carried no Idempotency-Key; the control plane refuses a write without one")
	}
}

// TestDeleteAndWait_RereadsAfterAStalePrecondition — 412 means the row moved
// between the read and the delete. Giving up there leaves the resource standing.
func TestDeleteAndWait_RereadsAfterAStalePrecondition(t *testing.T) {
	fastPolls(t)
	const first = "2026-10-10T09:30:00Z"
	const second = "2026-10-10T09:30:05Z"
	rec := &recorder{}
	var deletes, done int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			rec.note(r)
			if atomic.AddInt32(&deletes, 1) == 1 {
				http.Error(w, `{"error":"precondition_failed"}`, http.StatusPreconditionFailed)
				return
			}
			atomic.StoreInt32(&done, 1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"net-1"}`))
		default:
			if atomic.LoadInt32(&done) == 1 {
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
				return
			}
			stamp := first
			if atomic.LoadInt32(&deletes) > 0 {
				stamp = second
			}
			jsonRow(w, asyncRow{ID: "net-1", ActualState: "Active", UpdatedAt: stamp})
		}
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/networks/net-1", kind: "network"}
	if err := w.deleteAndWait(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ifMatch, _ := rec.seen()
	if len(ifMatch) != 2 {
		t.Fatalf("the delete was sent %d time(s), want 2 (one refused, one accepted)", len(ifMatch))
	}
	if ifMatch[0] != first || ifMatch[1] != second {
		t.Errorf("If-Match values were %q then %q, want %q then the re-read %q", ifMatch[0], ifMatch[1], first, second)
	}
}

// TestDeleteAndWait_KeepsAskingWhileTheProductHoldsTheDelete — a VM is refused
// 409 delete_blocked for five minutes after a migration settles (#728). The
// only thing that clears it is asking again.
func TestDeleteAndWait_KeepsAskingWhileTheProductHoldsTheDelete(t *testing.T) {
	fastPolls(t)
	var deletes, done int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			if atomic.AddInt32(&deletes, 1) < 3 {
				http.Error(w, `{"error":"delete_blocked","code":"delete_blocked"}`, http.StatusConflict)
				return
			}
			atomic.StoreInt32(&done, 1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"vm-1"}`))
		default:
			if atomic.LoadInt32(&done) == 1 {
				http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
				return
			}
			jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Running", UpdatedAt: "2026-10-10T09:30:00Z"})
		}
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	if err := w.deleteAndWait(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := atomic.LoadInt32(&deletes); n != 3 {
		t.Errorf("the delete was sent %d time(s); it should have been asked again through both holds", n)
	}
}

// TestDeleteAndWait_AResourceAlreadyGoneIsNotAnError — a destroy run again
// after a partly-failed one must finish, not fail on a row that is gone.
func TestDeleteAndWait_AResourceAlreadyGoneIsNotAnError(t *testing.T) {
	fastPolls(t)
	var deletes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deletes, 1)
		}
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	if err := w.deleteAndWait(context.Background(), time.Second); err != nil {
		t.Fatalf("a resource that is already gone was reported as a failed delete: %v", err)
	}
	if n := atomic.LoadInt32(&deletes); n != 0 {
		t.Errorf("sent %d delete(s) for a row that was already gone", n)
	}
}

// TestDeleteAndWait_ARealFailureIsReported — the control for the two retries
// above. A 500 is not a retry, and a delete that swallowed it would report a
// resource gone while it stands.
func TestDeleteAndWait_ARealFailureIsReported(t *testing.T) {
	fastPolls(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		jsonRow(w, asyncRow{ID: "vm-1", ActualState: "Running", UpdatedAt: "2026-10-10T09:30:00Z"})
	}))
	t.Cleanup(srv.Close)

	w := stateWait{client: newVVMClient(srv.URL, "tok"), path: "/v1/vms/vm-1", kind: "VM"}
	err := w.deleteAndWait(context.Background(), time.Second)
	if err == nil {
		t.Fatal("a delete the control plane refused with a 500 was reported as done")
	}
	// ⚠️ "An error came back" is not the assertion. A delete that treated every
	// status as accepted would also error here — on the wait that followed,
	// reported as a timeout. The refusal itself has to be what is reported, or
	// the operator looks at the machine instead of the 500.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not report the status the control plane answered: %v", err)
	}
}

// TestIdempotencyKeyFor_SameInputsSameKey — what makes a retried `terraform
// apply` find the create it already made instead of making a second VM.
func TestIdempotencyKeyFor_SameInputsSameKey(t *testing.T) {
	a := idempotencyKeyFor("vvm_vm", "web-0", "cluster-1")
	b := idempotencyKeyFor("vvm_vm", "web-0", "cluster-1")
	c := idempotencyKeyFor("vvm_vm", "web-1", "cluster-1")
	if a != b {
		t.Errorf("the same inputs gave two keys: %q and %q", a, b)
	}
	if a == c {
		t.Error("two different VMs derived the same key, so the second create would replay the first")
	}
	if a == "" {
		t.Error("the key is empty, and the control plane refuses a write without one")
	}
	// A separator, not concatenation: without one, ("ab","c") and ("a","bc")
	// are the same key and two different resources share an identity.
	if idempotencyKeyFor("ab", "c") == idempotencyKeyFor("a", "bc") {
		t.Error("the parts are joined without a separator")
	}
}

// TestIdempotencyKeyForBody_EveryFieldCountsTowardsTheKey — the key must cover
// the WHOLE body, and this iterates the body so a field added to a resource
// later is covered without anyone remembering to add a case.
//
// 🚨 What a key derived from only a few fields does: every attribute of these
// resources is immutable, so changing one destroys and recreates — and the
// create that follows the destroy carries the OLD key with a NEW body, which
// the control plane answers 409 idempotency_key_reused. The apply then fails on
// every attempt for as long as the idempotency record lives.
func TestIdempotencyKeyForBody_EveryFieldCountsTowardsTheKey(t *testing.T) {
	full := map[string]any{
		"cluster_id":         "cluster-1",
		"datastore_id":       "ds-1",
		"network_id":         "net-1",
		"name":               "web-0",
		"vcpu_request":       int64(4),
		"memory_request_mib": int64(8192),
		"golden_image":       "ubuntu-2404",
		"boot_disk_gib":      int64(20),
		"boot_subnet_id":     "sub-1",
		"dr_tier":            "none",
		"encrypted":          true,
		"cloud_init": map[string]any{
			"hostname":            "web-0",
			"user_data":           "#cloud-config\nruncmd: [echo one]\n",
			"ssh_authorized_keys": []string{"ssh-ed25519 AAAA"},
		},
	}
	base, err := idempotencyKeyForBody("vvm_vm", full)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The replay property first: the same body must give the same key, or a
	// retried apply builds a second VM.
	again, err := idempotencyKeyForBody("vvm_vm", full)
	if err != nil || again != base {
		t.Fatalf("the same body gave two keys (%q and %q, err %v)", base, again, err)
	}

	changed := map[string]any{
		"cluster_id":         "cluster-2",
		"datastore_id":       "ds-2",
		"network_id":         "net-2",
		"name":               "web-1",
		"vcpu_request":       int64(8),
		"memory_request_mib": int64(16384),
		"golden_image":       "rhel-9",
		"boot_disk_gib":      int64(40),
		"boot_subnet_id":     "sub-2",
		"dr_tier":            "a",
		"encrypted":          false,
		"cloud_init": map[string]any{
			"hostname":            "web-1",
			"user_data":           "#cloud-config\nruncmd: [echo two]\n",
			"ssh_authorized_keys": []string{"ssh-ed25519 BBBB"},
		},
	}
	for field, other := range changed {
		altered := map[string]any{}
		for k, v := range full {
			altered[k] = v
		}
		altered[field] = other
		got, err := idempotencyKeyForBody("vvm_vm", altered)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", field, err)
		}
		if got == base {
			t.Errorf("changing %s did not change the key, so the create that follows a "+
				"replace would reuse it and be refused 409 idempotency_key_reused", field)
		}
	}

	// Removing a field must change it too: a body that no longer says
	// `encrypted` is a different request from one that does.
	without := map[string]any{}
	for k, v := range full {
		if k != "encrypted" {
			without[k] = v
		}
	}
	got, _ := idempotencyKeyForBody("vvm_vm", without)
	if got == base {
		t.Error("dropping a field did not change the key")
	}

	// And the resource type is part of it: two resources whose bodies happened
	// to match must not share an identity.
	other, _ := idempotencyKeyForBody("vvm_network", full)
	if other == base {
		t.Error("two different resource types derived the same key from the same body")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(&apiError{Status: http.StatusNotFound}) {
		t.Error("a 404 is not reported as not-found")
	}
	if isNotFound(&apiError{Status: http.StatusInternalServerError}) {
		t.Error("a 500 is reported as not-found, so a database fault would delete the resource from state")
	}
	if isNotFound(nil) {
		t.Error("no error is reported as not-found")
	}
}

func TestParseTimeout(t *testing.T) {
	d, err := parseTimeout("", "30m")
	if err != nil || d != 30*time.Minute {
		t.Errorf("an unset timeout gave %v, %v; want 30m", d, err)
	}
	d, err = parseTimeout("90s", "30m")
	if err != nil || d != 90*time.Second {
		t.Errorf("90s gave %v, %v", d, err)
	}
	if _, err := parseTimeout("soon", "30m"); err == nil {
		t.Error("\"soon\" was accepted as a duration")
	}
	if _, err := parseTimeout("0s", "30m"); err == nil {
		t.Error("a zero timeout was accepted; nothing asynchronous can finish in it")
	}
}
