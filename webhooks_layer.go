package controlresell

import (
	"context"
	"iter"
	"strings"
	"time"
)

// WebhooksService manages the seller's webhook endpoints, their secrets and their deliveries (webhooks:read and
// webhooks:write scopes), and reads the recent events.
type WebhooksService struct{ api *APIClient }

// Endpoints reads the seller's webhook endpoints, without their secrets (at most 5: a single page).
func (s *WebhooksService) Endpoints(ctx context.Context) (*WebhookEndpointPage, error) {
	value, response, err := s.api.WebhooksAPI.ListWebhookEndpoints(ctx).Execute()
	return result(value, response, err)
}

// Endpoint reads one webhook endpoint by its identifier.
func (s *WebhooksService) Endpoint(ctx context.Context, id string) (*WebhookEndpoint, error) {
	value, response, err := s.api.WebhooksAPI.GetWebhookEndpoint(ctx, id).Execute()
	return result(value, response, err)
}

// CreateEndpoint creates a webhook endpoint and returns it with its signing secret, shown this once (read it again
// with Secret). opts may set the Idempotency-Key.
func (s *WebhooksService) CreateEndpoint(ctx context.Context, body WebhookEndpointCreateRequest, opts ...WriteOption) (*CreatedWebhookEndpoint, error) {
	value, response, err := s.api.WebhooksAPI.CreateWebhookEndpoint(ctx).WebhookEndpointCreateRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// UpdateEndpoint changes the given fields of an endpoint (a JSON merge patch: the fields left out are kept;
// body.SetDescriptionNil() sends an explicit null, which clears the description). Status "enabled" re-enables a
// disabled endpoint and resets its health.
func (s *WebhooksService) UpdateEndpoint(ctx context.Context, id string, body WebhookEndpointUpdateRequest) (*WebhookEndpoint, error) {
	value, response, err := s.api.WebhooksAPI.UpdateWebhookEndpoint(ctx, id).WebhookEndpointUpdateRequest(body).Execute()
	return result(value, response, err)
}

// DeleteEndpoint deletes an endpoint; its pending deliveries are dropped.
func (s *WebhooksService) DeleteEndpoint(ctx context.Context, id string) error {
	response, err := s.api.WebhooksAPI.DeleteWebhookEndpoint(ctx, id).Execute()
	if err != nil {
		return convert(response, err)
	}
	return nil
}

// Secret reads the current signing secret of an endpoint.
func (s *WebhooksService) Secret(ctx context.Context, id string) (*WebhookSecret, error) {
	value, response, err := s.api.WebhooksAPI.GetWebhookSecret(ctx, id).Execute()
	return result(value, response, err)
}

// RotateSecret gives an endpoint a new signing secret; the previous one stays valid for body.ExpiresInHours (24 by
// default, 0 stops it at once). opts may set the Idempotency-Key.
func (s *WebhooksService) RotateSecret(ctx context.Context, id string, body WebhookSecretRotateRequest, opts ...WriteOption) (*WebhookSecret, error) {
	value, response, err := s.api.WebhooksAPI.RotateWebhookSecret(ctx, id).WebhookSecretRotateRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// Test sends a webhook.test event to the endpoint now and returns how it answered. A test is never replayed: a retry
// after a gateway error or a network error may send the receiver another webhook.test event.
func (s *WebhooksService) Test(ctx context.Context, id string) (*WebhookTestResult, error) {
	value, response, err := s.api.WebhooksAPI.TestWebhookEndpoint(ctx, id).Execute()
	return result(value, response, err)
}

// DeliveryListParams are the parameters of a delivery list.
type DeliveryListParams struct {
	ListParams
	// Status keeps the deliveries in one status (pending, delivered, failed); empty for all.
	Status string
	// Type keeps the deliveries of one event type; empty for all.
	Type string
}

// Deliveries reads one page of the deliveries of an endpoint over the last 7 days, the most recent first.
func (s *WebhooksService) Deliveries(ctx context.Context, id string, params DeliveryListParams) (*WebhookDeliveryPage, error) {
	request := s.api.WebhooksAPI.ListWebhookDeliveries(ctx, id)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.Status != "" {
		request = request.Status(params.Status)
	}
	if params.Type != "" {
		request = request.Type_(params.Type)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllDeliveries yields every delivery of an endpoint, page after page, with the same filters (params.Cursor is ignored).
func (s *WebhooksService) AllDeliveries(ctx context.Context, id string, params DeliveryListParams) iter.Seq2[WebhookDelivery, error] {
	return paginate(func(cursor string) ([]WebhookDelivery, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Deliveries(ctx, id, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// RetryDelivery sends one event to the endpoint again, with the same webhook-id. opts may set the Idempotency-Key.
func (s *WebhooksService) RetryDelivery(ctx context.Context, id string, deliveryID string, opts ...WriteOption) (*WebhookDelivery, error) {
	value, response, err := s.api.WebhooksAPI.RetryWebhookDelivery(ctx, id, deliveryID).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// Recover sends again the failed deliveries of an endpoint since an instant (7 days at most, 1,000 per call: call
// again while HasMore). opts may set the Idempotency-Key.
func (s *WebhooksService) Recover(ctx context.Context, id string, since time.Time, opts ...WriteOption) (*WebhookRecovery, error) {
	value, response, err := s.api.WebhooksAPI.RecoverWebhookDeliveries(ctx, id).WebhookRecoverRequest(WebhookRecoverRequest{Since: since}).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// EventListParams are the parameters of an event list.
type EventListParams struct {
	ListParams
	// Since keeps the events created at or after it, at most 7 days ago (the default); the zero time for the default.
	Since time.Time
	// Types keeps these event types; empty for every type the key's scopes read.
	Types []string
}

// Events reads one page of the events of the last 7 days, the oldest first: what a client that was offline catches up.
func (s *WebhooksService) Events(ctx context.Context, params EventListParams) (*WebhookEventPage, error) {
	request := s.api.WebhooksAPI.ListWebhookEvents(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if !params.Since.IsZero() {
		request = request.Since(params.Since)
	}
	if len(params.Types) > 0 {
		request = request.Type_(strings.Join(params.Types, ","))
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllEvents yields every event, page after page, with the same filters (params.Cursor is ignored).
func (s *WebhooksService) AllEvents(ctx context.Context, params EventListParams) iter.Seq2[WebhookEvent, error] {
	return paginate(func(cursor string) ([]WebhookEvent, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Events(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// EventTypes reads the catalog of event types and the scope each one needs.
func (s *WebhooksService) EventTypes(ctx context.Context) (*WebhookEventTypeList, error) {
	value, response, err := s.api.WebhooksAPI.ListWebhookEventTypes(ctx).Execute()
	return result(value, response, err)
}
