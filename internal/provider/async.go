package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The VVM API is asynchronous: a create returns 202 with a job handle and the
// resource reaches its target state later (vvm-iac-spec.md §2.1). Terraform
// expects "create → it exists", so this file is the bridge: one place that
// waits, one place that deletes, and both of them say what the control plane
// actually reported when they give up.
//
// ⛔ NOTHING HERE MAY REPORT SUCCESS IT DID NOT OBSERVE. A wait that ran out of
// time is an error carrying the last state the control plane reported and the
// state of the job behind it — never a silent return that would leave
// `terraform apply` claiming a VM is Running when it is still Scheduling.

// asyncPollInterval is the delay between reads while waiting. A package
// variable so a test can take it down to a millisecond; a real apply benefits
// from a longer one, because a VM create takes minutes and every poll is an API
// call.
var asyncPollInterval = 5 * time.Second

// asyncRow is the part of a VVM row that every wait here reads. Every write
// route answers these and GET returns them for networks, datastores and VMs.
//
// UpdatedAt is kept as the raw string the API sent, deliberately: it is the
// If-Match value for a later delete, and the control plane compares it against
// the row's own timestamp. Re-rendering a parsed time would change the string.
type asyncRow struct {
	ID           string `json:"id"`
	DesiredState string `json:"desired_state"`
	ActualState  string `json:"actual_state"`
	UpdatedAt    string `json:"updated_at"`
}

// writeAck is the 202 body a VVM write answers with: the row's id and the job
// that will carry the work out.
type writeAck struct {
	ID    string `json:"id"`
	JobID string `json:"job_id"`
	// GuestIPv4 is on the VM create answer only, and it is the ONLY place the
	// guest's address appears: there is no DHCP lease and no DNS record.
	GuestIPv4 string `json:"guest_ipv4"`
}

// jobRow is what GET /v1/jobs/{id} answers. Read only when a wait is about to
// fail, so the error can name the real job state instead of "timed out".
type jobRow struct {
	Kind         string          `json:"kind"`
	State        string          `json:"state"`
	Result       json.RawMessage `json:"result"`
	ErrorMessage *string         `json:"error_message"`
	Attempts     int32           `json:"attempts"`
}

// awaitJob polls a job until it succeeds or fails and returns the result it
// recorded. For a write whose 202 names no row yet: the row is made by the job,
// and the job's result is the only place its id appears.
func awaitJob(ctx context.Context, c *vvmClient, jobID string, timeout time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(timeout)
	for {
		raw, err := c.get(ctx, "/v1/jobs/"+jobID)
		if err != nil {
			return nil, err
		}
		var job jobRow
		if jsonErr := json.Unmarshal(raw, &job); jsonErr != nil {
			return nil, fmt.Errorf("parse job %s: %w", jobID, jsonErr)
		}
		switch job.State {
		case "succeeded":
			return job.Result, nil
		case "failed":
			msg := "no reason recorded"
			if job.ErrorMessage != nil && *job.ErrorMessage != "" {
				msg = *job.ErrorMessage
			}
			return nil, fmt.Errorf("job %s %s failed: %s", jobID, job.Kind, msg)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("job %s %s was still %s after %s", jobID, job.Kind, stateOrUnknown(job.State), timeout)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("job %s %s was still %s when Terraform gave up: %w", jobID, job.Kind, stateOrUnknown(job.State), ctx.Err())
		case <-time.After(asyncPollInterval):
		}
	}
}

// idempotencyKeyFor derives the Idempotency-Key for a write.
//
// Two things need it. The control plane REFUSES a write with no key at all
// (internal/idempotency: 400 idempotency_key_required), and a key derived from
// the inputs rather than minted fresh is what makes a retried `terraform apply`
// find the create it already made instead of making a second one.
func idempotencyKeyFor(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// idempotencyKeyForBody derives a create's key from the WHOLE request body.
//
// 🚨 EVERY FIELD, NOT THE ONES THAT NAME THE RESOURCE. A key derived from (say)
// the cluster, the name and the network is the same key for two different
// requests, and the control plane answers the second with 409
// idempotency_key_reused — "previously used with a different request body".
// Since a change to ANY attribute here destroys and recreates, that is the
// ordinary case: change a VM's user-data and the create that follows the
// destroy is refused for as long as the idempotency record lives, and no retry
// clears it.
//
// encoding/json sorts a map's keys, so the same body always renders the same
// bytes and a retried apply with an unchanged config still replays rather than
// building a second VM.
func idempotencyKeyForBody(kind string, body map[string]any) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("derive the idempotency key for %s: %w", kind, err)
	}
	return idempotencyKeyFor(kind, string(b)), nil
}

// isNotFound reports whether err is the API saying the resource is not there.
// A resource that has gone out of band is removed from Terraform state rather
// than reported as an error, which is what makes `plan` show the truth.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// readRow reads one row and decodes the fields every wait needs.
//
// 🔑 IT ALSO RETURNS THE RAW ANSWER. A VM and a datastore carry fields no wait
// cares about (the placement, the measured capacity), and handing back the body
// the wait already read is what lets them decode the SAME answer instead of
// asking the control plane a second time for the row it just described.
func readRow(ctx context.Context, c *vvmClient, path string) (asyncRow, []byte, error) {
	raw, err := c.get(ctx, path)
	if err != nil {
		return asyncRow{}, nil, err
	}
	var row asyncRow
	if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
		return asyncRow{}, raw, fmt.Errorf("parse %s: %w", path, jsonErr)
	}
	return row, raw, nil
}

// stateWait is one resource's wait for its target state.
type stateWait struct {
	client *vvmClient
	// path is the row's own address, e.g. /v1/vms/<id>.
	path string
	// jobID is the job the write emitted, so a wait that runs out of time can
	// say what that job is doing. Empty when the write named none.
	jobID string
	// kind names the resource in messages, e.g. "VM".
	kind string
	// target is the actual_state that means the resource is really there.
	target string
	// failed are the actual_states it will never leave.
	failed []string
}

// wait polls until the row reaches the target state, reaches a state it will
// never leave, or the budget runs out. The raw body of the last answer comes
// back too, so the caller can read its own fields out of it.
func (w stateWait) wait(ctx context.Context, timeout time.Duration) (asyncRow, []byte, error) {
	deadline := time.Now().Add(timeout)
	var last asyncRow
	var lastRaw []byte
	for {
		row, raw, err := readRow(ctx, w.client, w.path)
		if err != nil {
			return last, lastRaw, err
		}
		last, lastRaw = row, raw
		if row.ActualState == w.target {
			return row, raw, nil
		}
		for _, bad := range w.failed {
			if row.ActualState == bad {
				return row, raw, fmt.Errorf("%s %s reached %s, which it will not leave%s",
					w.kind, row.ID, row.ActualState, w.jobSuffix(ctx))
			}
		}
		if !time.Now().Before(deadline) {
			return last, lastRaw, fmt.Errorf("%s %s was still %s after %s, not %s%s",
				w.kind, w.pathID(), stateOrUnknown(last.ActualState), timeout, w.target, w.jobSuffix(ctx))
		}
		select {
		case <-ctx.Done():
			return last, lastRaw, fmt.Errorf("%s %s was still %s when Terraform gave up: %w%s",
				w.kind, w.pathID(), stateOrUnknown(last.ActualState), ctx.Err(), w.jobSuffix(ctx))
		case <-time.After(asyncPollInterval):
		}
	}
}

// waitGone polls until the row is no longer there. A delete is not done when
// the API accepts it: the row goes when the job behind it finishes, and for a
// VM that means a host has confirmed the domain is gone (#639).
func (w stateWait) waitGone(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last asyncRow
	for {
		row, _, err := readRow(ctx, w.client, w.path)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		last = row
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s %s was still there after %s, reading %s%s",
				w.kind, w.pathID(), timeout, stateOrUnknown(last.ActualState), w.jobSuffix(ctx))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %s was still there when Terraform gave up, reading %s: %w%s",
				w.kind, w.pathID(), stateOrUnknown(last.ActualState), ctx.Err(), w.jobSuffix(ctx))
		case <-time.After(asyncPollInterval):
		}
	}
}

// deleteAndWait removes the row and waits until it is gone.
//
// 🚨 THE If-Match IS READ FRESH, NOT TAKEN FROM TERRAFORM STATE. Every VVM
// delete is guarded on the row's current updated_at, and the row's timestamp
// moves on its own: the scheduler places a VM, a host reports it Running, a
// usage report arrives. The value stored at create is stale within seconds of
// it, so a delete sent with it is refused 412 — and a destroy that fails on
// every run leaves an estate standing.
func (w stateWait) deleteAndWait(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		row, _, err := readRow(ctx, w.client, w.path)
		if isNotFound(err) {
			// Already gone — a destroy after a partly-failed one, or a resource
			// removed out of band. Nothing to do and nothing wrong.
			return nil
		}
		if err != nil {
			return err
		}
		_, status, err := w.client.send(ctx, apiRequest{
			method:         http.MethodDelete,
			path:           w.path,
			ifMatch:        row.UpdatedAt,
			idempotencyKey: idempotencyKeyFor("delete", w.path, row.UpdatedAt),
		})
		switch {
		case err == nil, status == http.StatusNotFound:
			return w.waitGone(ctx, time.Until(deadline))
		case status == http.StatusPreconditionFailed:
			// The row moved between the read and the delete. Read it again and
			// send the timestamp it has now.
		case status == http.StatusConflict:
			// The product is holding the delete — a VM whose migration has not
			// settled is held for five minutes (#728). Asking again is the only
			// thing that clears it.
		default:
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s %s could not be deleted within %s: the last answer was %d (%s)%s",
				w.kind, w.pathID(), timeout, status, errText(err), w.jobSuffix(ctx))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s %s could not be deleted before Terraform gave up: %w", w.kind, w.pathID(), ctx.Err())
		case <-time.After(asyncPollInterval):
		}
	}
}

// pathID is the id out of the row's path, for messages.
func (w stateWait) pathID() string {
	at := strings.LastIndex(w.path, "/")
	if at < 0 || at == len(w.path)-1 {
		return w.path
	}
	return w.path[at+1:]
}

// jobSuffix is what the job behind this resource is doing, read at the moment a
// wait gives up. The spec requires a timeout to surface the actual VVM job
// state rather than Terraform's own idea of elapsed time (§2.1).
//
// ⚠️ It reports that it could not ask rather than saying nothing: an empty
// suffix would read as "the job is fine", which is the one thing a failed wait
// cannot claim.
func (w stateWait) jobSuffix(ctx context.Context) string {
	if w.jobID == "" {
		return ""
	}
	raw, err := w.client.get(ctx, "/v1/jobs/"+w.jobID)
	if err != nil {
		return fmt.Sprintf(" (job %s could not be read: %s)", w.jobID, err)
	}
	var job jobRow
	if jsonErr := json.Unmarshal(raw, &job); jsonErr != nil {
		return fmt.Sprintf(" (job %s could not be parsed: %s)", w.jobID, jsonErr)
	}
	msg := fmt.Sprintf(" (job %s %s is %s, attempt %d", w.jobID, job.Kind, stateOrUnknown(job.State), job.Attempts)
	if job.ErrorMessage != nil && *job.ErrorMessage != "" {
		msg += ": " + *job.ErrorMessage
	}
	return msg + ")"
}

// stateOrUnknown names an empty state, so a message never reads "was still ".
func stateOrUnknown(s string) string {
	if s == "" {
		return "(no state reported)"
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// parseTimeout reads a duration an operator wrote in HCL, falling back to the
// resource's default when they wrote nothing.
func parseTimeout(given, fallback string) (time.Duration, error) {
	if given == "" {
		given = fallback
	}
	d, err := time.ParseDuration(given)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (try \"30m\"): %w", given, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not a time to wait", given)
	}
	return d, nil
}
