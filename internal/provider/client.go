// Package provider implements the VVM Terraform provider.
//
// The provider talks to the VVM REST API over HTTPS using a bearer token
// (VVM_API_TOKEN or IBMCLOUD_API_KEY → IAM exchange). It exposes two
// resource types for the WS-2.3 backup subsystem:
//
//   - vvm_backup_policy   → POST/GET/PUT/DELETE /v1/backup-policies
//   - vvm_protection_group → POST/GET/PUT/DELETE /v1/protection-groups
//
// The provider is a standalone Go module under vvm-iac/ so the main vvm
// repo doesn't need to depend on the heavy Terraform SDK dependency tree.
// Spec reference: vvm-spec-ws-2.3-image-backup.md §8.4.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// vvmClient is the minimal HTTP client the resource implementations need.
// It wraps net/http with VVM-specific auth + error handling and is
// intentionally narrow — only the methods the two backup resources call.
type vvmClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newVVMClient(baseURL, token string) *vvmClient {
	return &vvmClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// apiError is returned when the VVM API responds with a non-2xx status.
// It carries the HTTP status and the raw body (may be a WS-1.6 envelope).
type apiError struct {
	Status  int
	Message string
	Body    []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("VVM API error %d: %s", e.Status, e.Message)
}

// apiRequest is one call to the VVM API. It exists so a caller can send the two
// headers every guarded write needs — If-Match (precondition.Require) and
// Idempotency-Key (idempotency.New) — without a second copy of the request
// building below.
type apiRequest struct {
	method         string
	path           string
	body           any
	ifMatch        string
	idempotencyKey string
}

func (c *vvmClient) do(ctx context.Context, method, path string, body any, idempotencyKey string) ([]byte, int, error) {
	return c.send(ctx, apiRequest{method: method, path: path, body: body, idempotencyKey: idempotencyKey})
}

// send performs one apiRequest and returns the body, the HTTP status and, for a
// non-2xx, an *apiError.
//
// 🔑 THE STATUS IS RETURNED EVEN WITH AN ERROR, and callers depend on it: a 412
// means "read the row again", a 409 on a delete means "the product is holding
// it, ask again", and both are retries rather than failures.
func (c *vvmClient) send(ctx context.Context, in apiRequest) ([]byte, int, error) {
	method, path, body, idempotencyKey := in.method, in.path, in.body, in.idempotencyKey
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if in.ifMatch != "" {
		req.Header.Set("If-Match", in.ifMatch)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := ""
		var env struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if jsonErr := json.Unmarshal(raw, &env); jsonErr == nil {
			if env.Message != "" {
				msg = env.Message
			} else {
				msg = env.Error
			}
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return raw, resp.StatusCode, &apiError{Status: resp.StatusCode, Message: msg, Body: raw}
	}
	return raw, resp.StatusCode, nil
}

func (c *vvmClient) get(ctx context.Context, path string) ([]byte, error) {
	raw, _, err := c.do(ctx, http.MethodGet, path, nil, "")
	return raw, err
}

func (c *vvmClient) post(ctx context.Context, path string, body any, idempotencyKey string) ([]byte, error) {
	raw, _, err := c.do(ctx, http.MethodPost, path, body, idempotencyKey)
	return raw, err
}

func (c *vvmClient) put(ctx context.Context, path string, body any, ifMatch, idempotencyKey string) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("build PUT request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute PUT: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := ""
		var env struct{ Message string }
		if json.Unmarshal(raw, &env) == nil {
			msg = env.Message
		}
		return raw, &apiError{Status: resp.StatusCode, Message: msg, Body: raw}
	}
	return raw, nil
}

func (c *vvmClient) delete(ctx context.Context, path string, ifMatch, idempotencyKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build DELETE request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute DELETE: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Idempotent: treat as already deleted.
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return &apiError{Status: resp.StatusCode, Message: string(raw), Body: raw}
	}
	return nil
}
