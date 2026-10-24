package controlresell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IdempotencyKeyHeader is the header that makes a POST safe to retry: the API replays the first answer to the same key.
const IdempotencyKeyHeader = "Idempotency-Key"

const idempotencyInProgress = "idempotency_in_progress"

// RetryPolicy is how a call is retried.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt; 0 turns them off.
	MaxRetries int
	// BaseDelay is the first delay, doubled at each retry, with a random part; a Retry-After takes precedence.
	BaseDelay time.Duration
	// MaxDelay is the longest delay between two attempts.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is two retries, 500 ms doubled each time, 30 s at most.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 2, BaseDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	delay := float64(p.BaseDelay) * math.Pow(2, float64(attempt))
	if p.BaseDelay > 1 {
		delay += mathrand.Float64() * float64(p.BaseDelay) / 2
	}
	return time.Duration(math.Min(delay, float64(p.MaxDelay)))
}

// retryTransport gives each POST an Idempotency-Key (kept across its retries), retries a rate limit, a gateway or
// availability error (502, 503, 504), a 409 idempotency_in_progress and a network error, and waits for Retry-After
// when the API sends it. A 500 is never retried. The webhook test call sends a real event and the API ignores its
// key, so it is only retried on a 429 and on a connection error before the request left (a failed dial).
type retryTransport struct {
	base   http.RoundTripper
	policy RetryPolicy
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.Header.Get(IdempotencyKeyHeader) == "" {
		request = request.Clone(request.Context())
		if isWebhookTest(request) {
			// net/http replays by itself, on a reused connection the server closed, a POST that carries an
			// Idempotency-Key. The API ignores the key of a test, so it is sent under a non-canonical name, which
			// the server reads all the same but net/http does not see.
			request.Header[strings.ToLower(IdempotencyKeyHeader)] = []string{newKey()}
		} else {
			request.Header.Set(IdempotencyKeyHeader, newKey())
		}
	}
	replayable := !isWebhookTest(request)
	for attempt := 0; ; attempt++ {
		last := attempt >= t.policy.MaxRetries
		attemptRequest := request
		if attempt > 0 && request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			attemptRequest = request.Clone(request.Context())
			attemptRequest.Body = body
		}
		response, err := t.base.RoundTrip(attemptRequest)
		var delay time.Duration
		if err != nil {
			if last || (!replayable && !failedBeforeSending(err)) {
				return nil, err
			}
			delay = t.policy.backoff(attempt)
		} else {
			if last || (!replayable && response.StatusCode != http.StatusTooManyRequests) || !shouldRetry(response) {
				return response, nil
			}
			delay = t.policy.backoff(attempt)
			if after := retryAfter(response.Header); after > 0 || response.Header.Get("Retry-After") != "" {
				delay = min(after, t.policy.MaxDelay)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		timer := time.NewTimer(delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return nil, request.Context().Err()
		case <-timer.C:
		}
	}
}

var webhookTestPath = regexp.MustCompile(`/webhook-endpoints/[^/]+/test/?$`)

func isWebhookTest(request *http.Request) bool {
	return request.Method == http.MethodPost && webhookTestPath.MatchString(request.URL.Path)
}

// failedBeforeSending is true for an error raised while connecting: nothing reached the server.
func failedBeforeSending(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func shouldRetry(response *http.Response) bool {
	switch response.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusConflict:
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		if err != nil {
			return false
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		return json.Unmarshal(body, &envelope) == nil && envelope.Error.Code == idempotencyInProgress
	}
	return false
}

func newKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
