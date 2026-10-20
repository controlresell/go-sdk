package controlresell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// APIError is an error answered by the API, read from its envelope {"error": {code, message, params, requestId}}.
//
// Code is an open string (item_not_found, rate_limited...): match the codes you handle and treat any other one by
// its Status. Each status has its own type (NotFoundError, RateLimitError...) that unwraps to *APIError, so both
// errors.As(err, &notFound) and errors.As(err, &apiErr) work.
type APIError struct {
	// Status is the HTTP status.
	Status int
	// Code is the error's code.
	Code string
	// Message describes the error.
	Message string
	// Params are the error's parameters.
	Params map[string]string
	// RequestID is the identifier to give the support, when the API sent one.
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("controlresell: %s (HTTP %d, %s)", e.Message, e.Status, e.Code)
}

// BadRequestError is a 400: the request is malformed.
type BadRequestError struct{ *APIError }

// AuthenticationError is a 401: no credential, or an unknown, revoked or expired one.
type AuthenticationError struct{ *APIError }

// PaymentRequiredError is a 402: the seller's plan does not include this.
type PaymentRequiredError struct{ *APIError }

// PermissionDeniedError is a 403: the API key does not grant the scope the route requires.
type PermissionDeniedError struct{ *APIError }

// NotFoundError is a 404: no such resource for this seller.
type NotFoundError struct{ *APIError }

// ConflictError is a 409: an Idempotency-Key used with another request, or still in flight.
type ConflictError struct{ *APIError }

// UnprocessableEntityError is a 422: the request is well formed but refused.
type UnprocessableEntityError struct{ *APIError }

// RateLimitError is a 429: the rate limit of the API key is reached.
type RateLimitError struct {
	*APIError
	// RetryAfter is the Retry-After the API answered, or zero.
	RetryAfter time.Duration
}

// ServerError is a 5xx: an error of the API; retry later.
type ServerError struct{ *APIError }

func (e *BadRequestError) Unwrap() error          { return e.APIError }
func (e *AuthenticationError) Unwrap() error      { return e.APIError }
func (e *PaymentRequiredError) Unwrap() error     { return e.APIError }
func (e *PermissionDeniedError) Unwrap() error    { return e.APIError }
func (e *NotFoundError) Unwrap() error            { return e.APIError }
func (e *ConflictError) Unwrap() error            { return e.APIError }
func (e *UnprocessableEntityError) Unwrap() error { return e.APIError }
func (e *RateLimitError) Unwrap() error           { return e.APIError }
func (e *ServerError) Unwrap() error              { return e.APIError }

// ConnectionError means the API could not be reached (network, DNS, TLS), after the retries.
type ConnectionError struct{ Err error }

func (e *ConnectionError) Error() string {
	return "controlresell: could not reach the API: " + e.Err.Error()
}

func (e *ConnectionError) Unwrap() error { return e.Err }

// errorOf is the typed error of a response of the given status whose body is body (the API's envelope, or anything).
func errorOf(status int, body []byte, header http.Header) error {
	var envelope struct {
		Error struct {
			Code      *string           `json:"code"`
			Message   *string           `json:"message"`
			Params    map[string]string `json:"params"`
			RequestID *string           `json:"requestId"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	e := &APIError{Status: status, Code: "http_" + strconv.Itoa(status), Message: "HTTP " + strconv.Itoa(status), Params: envelope.Error.Params}
	if envelope.Error.Code != nil {
		e.Code = *envelope.Error.Code
	}
	if envelope.Error.Message != nil {
		e.Message = *envelope.Error.Message
	}
	if envelope.Error.RequestID != nil {
		e.RequestID = *envelope.Error.RequestID
	}
	switch {
	case status == 400:
		return &BadRequestError{e}
	case status == 401:
		return &AuthenticationError{e}
	case status == 402:
		return &PaymentRequiredError{e}
	case status == 403:
		return &PermissionDeniedError{e}
	case status == 404:
		return &NotFoundError{e}
	case status == 409:
		return &ConflictError{e}
	case status == 422:
		return &UnprocessableEntityError{e}
	case status == 429:
		return &RateLimitError{APIError: e, RetryAfter: retryAfter(header)}
	case status >= 500:
		return &ServerError{e}
	}
	return e
}

func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.ParseFloat(header.Get("Retry-After"), 64)
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// convert turns what the generated client returned for a failed call into a typed error.
func convert(response *http.Response, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var generated *GenericOpenAPIError
	if response != nil && response.StatusCode >= 400 && errors.As(err, &generated) {
		return errorOf(response.StatusCode, generated.Body(), response.Header)
	}
	if response == nil {
		return &ConnectionError{Err: err}
	}
	return err
}
