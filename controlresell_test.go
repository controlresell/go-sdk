package controlresell_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	controlresell "github.com/controlresell/go-sdk"
)

const key = "cr_test_secret"
const me = `{"id":1,"username":"seller","currency":"EUR","unknownField":true}`

type seen struct {
	header http.Header
	query  string
	method string
}

type stub struct {
	mu    sync.Mutex
	calls []seen
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// client starts a stub server answering with handler (n is the 1-based attempt) and a client on it.
func client(t *testing.T, handler func(n int, w http.ResponseWriter, r *http.Request)) (*stub, *controlresell.Client) {
	t.Helper()
	s := &stub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, seen{r.Header.Clone(), r.URL.RawQuery, r.Method})
		n := len(s.calls)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handler(n, w, r)
	}))
	t.Cleanup(server.Close)
	c, err := controlresell.NewClient(
		controlresell.WithAPIKey(key),
		controlresell.WithBaseURL(server.URL),
		controlresell.WithRetryPolicy(controlresell.RetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func envelope(code string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":"m","params":{},"requestId":"req-1"}}`, code)
}

func reply(w http.ResponseWriter, status int, body string, headers ...string) {
	for i := 0; i+1 < len(headers); i += 2 {
		w.Header().Set(headers[i], headers[i+1])
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func item(id int64, sex string) string {
	return fmt.Sprintf(`{"id":%d,"name":"n","sku":null,"description":null,"photos":[],"categoryId":null,"brandId":null,`+
		`"sizeId":null,"colorIds":[],"state":null,"sex":%q,"labels":[],"sold":false,`+
		`"materialIds":[],"enumValues":[],"attributes":[],"customFields":[],`+
		`"estimatedPrice":{"amount":"12.50","currency":"EUR"},"purchasePrice":null,"purchaseDate":null,`+
		`"sellingPrice":null,"sellingDate":null,"createdAt":"2026-10-20T10:00:00Z","updatedAt":"2026-10-20T10:00:00Z",`+
		`"unknownField":1}`, id, sex)
}

func TestACallSendsTheKeyAndTheUserAgent(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, 200, me) })
	got, err := c.Me.Get(context.Background())
	if err != nil || got.Username != "seller" {
		t.Fatalf("%v %v", got, err)
	}
	if h := s.calls[0].header; h.Get("Authorization") != "Bearer "+key || h.Get("User-Agent") != "controlresell-go/"+controlresell.Version {
		t.Fatalf("%v", h)
	}
}

func TestAnErrorIsTypedByItsStatusWithItsCodeAndRequestID(t *testing.T) {
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, 404, envelope("item_not_found")) })
	_, err := c.Items.Get(context.Background(), 1)
	var notFound *controlresell.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("not a NotFoundError: %v", err)
	}
	var base *controlresell.APIError
	if !errors.As(err, &base) || base.Code != "item_not_found" || base.RequestID != "req-1" || base.Status != 404 {
		t.Fatalf("%+v", base)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("the key appears in an error")
	}
}

func TestARateLimitIsRetriedAfterRetryAfterThenSucceeds(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 429, envelope("rate_limited"), "Retry-After", "0")
			return
		}
		reply(w, 200, `{"data":[],"nextCursor":null}`)
	})
	if _, err := c.Accounts.List(context.Background(), controlresell.ListParams{}); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("%d calls", s.count())
	}
}

func TestARateLimitAfterTheRetriesIsARateLimitError(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		reply(w, 429, envelope("rate_limited"), "Retry-After", "0")
	})
	_, err := c.Accounts.List(context.Background(), controlresell.ListParams{})
	var limited *controlresell.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter != 0 {
		t.Fatalf("%v", err)
	}
	if s.count() != 3 {
		t.Fatalf("%d calls, want the first attempt and two retries", s.count())
	}
}

func TestAPostKeepsTheSameIdempotencyKeyAcrossRetries(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n < 3 {
			reply(w, 503, envelope("idempotency_unavailable"))
			return
		}
		reply(w, 201, `{}`)
	})
	response, err := c.API.GetConfig().HTTPClient.Post(c.API.GetConfig().Servers[0].URL+"/anything", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 201 || s.count() != 3 {
		t.Fatalf("%d after %d calls", response.StatusCode, s.count())
	}
	first := s.calls[0].header.Get(controlresell.IdempotencyKeyHeader)
	for _, call := range s.calls {
		if first == "" || call.header.Get(controlresell.IdempotencyKeyHeader) != first {
			t.Fatalf("keys differ: %q", call.header.Get(controlresell.IdempotencyKeyHeader))
		}
	}
}

func TestA500OfAPostIsNotRetried(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, 500, envelope("internal_error")) })
	response, err := c.API.GetConfig().HTTPClient.Post(c.API.GetConfig().Servers[0].URL+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 500 || s.count() != 1 {
		t.Fatalf("%d after %d calls", response.StatusCode, s.count())
	}
}

func TestA5xxThroughARouteIsAServerError(t *testing.T) {
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, 500, envelope("internal_error")) })
	_, err := c.Me.Get(context.Background())
	var server *controlresell.ServerError
	if !errors.As(err, &server) {
		t.Fatalf("%v", err)
	}
}

func TestAllReadsEveryPageWithTheSameFilters(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 200, `{"data":[`+item(1, "WOMEN")+`],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[`+item(2, "WOMEN")+`],"nextCursor":null}`)
	})
	sold := false
	var ids []int64
	for it, err := range c.Items.All(context.Background(), controlresell.ItemListParams{Sold: &sold}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.Id)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("%v", ids)
	}
	if q := s.calls[0].query; !strings.Contains(q, "sold=false") || strings.Contains(q, "cursor") {
		t.Fatalf("first page: %s", q)
	}
	if q := s.calls[1].query; !strings.Contains(q, "sold=false") || !strings.Contains(q, "cursor=c2") {
		t.Fatalf("second page: %s", q)
	}
}

func TestAnAmountKeepsItsDecimalStringAndAnUnknownValueIsTolerated(t *testing.T) {
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		reply(w, 200, item(9007199254740993, "A_NEW_VALUE"))
	})
	got, err := c.Items.Get(context.Background(), 9007199254740993)
	if err != nil {
		t.Fatal(err)
	}
	if got.Id != 9007199254740993 || got.EstimatedPrice.Get().Amount != "12.50" || *got.Sex.Get() != "A_NEW_VALUE" || got.PurchasePrice.IsSet() && got.PurchasePrice.Get() != nil {
		t.Fatalf("%+v", got)
	}
}

func TestOnlyTheConflictIdempotencyInProgressIsRetried(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, 409, envelope("idempotency_key_reused")) })
	_, err := c.Me.Get(context.Background())
	var conflict *controlresell.ConflictError
	if !errors.As(err, &conflict) || s.count() != 1 {
		t.Fatalf("%v after %d calls", err, s.count())
	}

	s, c = client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 409, envelope("idempotency_in_progress"))
			return
		}
		reply(w, 200, me)
	})
	if _, err := c.Me.Get(context.Background()); err != nil || s.count() != 2 {
		t.Fatalf("%v after %d calls", err, s.count())
	}
}

func TestTheKeyIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("CONTROLRESELL_API_KEY", "")
	if _, err := controlresell.NewClient(); err == nil {
		t.Fatal("no key, no client")
	}
	t.Setenv("CONTROLRESELL_API_KEY", "cr_test_env")
	if _, err := controlresell.NewClient(); err != nil {
		t.Fatal(err)
	}
}

const action = `{"itemId":7,"action":"PUBLISH","accountIds":[],"scheduledAt":"2026-10-21T10:00:00Z"}`
const account = "3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10"

func TestPublishPostsTheAccountsWithOneIdempotencyKeyAcrossRetries(t *testing.T) {
	var paths, bodies []string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(raw))
		if n < 2 {
			reply(w, 503, envelope("idempotency_unavailable"))
			return
		}
		reply(w, 202, action)
	})
	scheduled, err := c.Items.Publish(context.Background(), 7, controlresell.PublishRequest{AccountIds: []string{account}})
	if err != nil || scheduled.ItemId != 7 {
		t.Fatalf("%v %+v", err, scheduled)
	}
	if len(paths) != 2 || paths[1] != "/api/public/v1/items/7/publish" || bodies[0] != bodies[1] || !strings.Contains(bodies[1], account) {
		t.Fatalf("%v %v", paths, bodies)
	}
	first := s.calls[0].header.Get(controlresell.IdempotencyKeyHeader)
	if first == "" || s.calls[1].header.Get(controlresell.IdempotencyKeyHeader) != first || len(s.calls[1].header.Values(controlresell.IdempotencyKeyHeader)) != 1 {
		t.Fatalf("keys: %v", s.calls)
	}
}

func TestAWriteUsesTheIdempotencyKeyItIsGiven(t *testing.T) {
	var path string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { path = r.URL.Path; reply(w, 202, action) })
	if _, err := c.Items.Republish(context.Background(), 7, controlresell.RepublishRequest{}, controlresell.WithIdempotencyKey("my-key")); err != nil {
		t.Fatal(err)
	}
	if path != "/api/public/v1/items/7/republish" || s.calls[0].header.Get(controlresell.IdempotencyKeyHeader) != "my-key" {
		t.Fatalf("%s %v", path, s.calls[0].header)
	}
}

func TestPriceDropPostsToThePriceDropRoute(t *testing.T) {
	var path, body string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(raw)
		reply(w, 202, action)
	})
	if _, err := c.Items.PriceDrop(context.Background(), 7, controlresell.PriceDropRequest{Type: "PERCENT", Value: "10"}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/public/v1/items/7/price-drop" || !strings.Contains(body, `"PERCENT"`) {
		t.Fatalf("%s %s", path, body)
	}
}

func TestPublishPriceDropAndRepublishSendScheduledAt(t *testing.T) {
	var bodies []string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		reply(w, 202, action)
	})
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	publish := controlresell.PublishRequest{AccountIds: []string{account}}
	publish.SetScheduledAt(at)
	drop := controlresell.PriceDropRequest{Type: "PERCENT", Value: "10"}
	drop.SetScheduledAt(at)
	republish := controlresell.RepublishRequest{}
	republish.SetScheduledAt(at)
	ctx := context.Background()
	if _, err := c.Items.Publish(ctx, 7, publish); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Items.PriceDrop(ctx, 7, drop); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Items.Republish(ctx, 7, republish); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%v", bodies)
	}
	for _, body := range bodies {
		if !strings.Contains(body, `"scheduledAt":"2026-11-02T10:00:00Z"`) {
			t.Fatalf("%s", body)
		}
	}
}

func TestDeleteResolvesOn204AndReturnsATypedErrorOn409(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(204)
			return
		}
		reply(w, 409, envelope("item_still_listed"))
	})
	if err := c.Items.Delete(context.Background(), 7); err != nil || s.calls[0].method != "DELETE" {
		t.Fatalf("%v", err)
	}
	var conflict *controlresell.ConflictError
	var base *controlresell.APIError
	err := c.Items.Delete(context.Background(), 7)
	if !errors.As(err, &conflict) || !errors.As(err, &base) || base.Code != "item_still_listed" {
		t.Fatalf("%v", err)
	}
}

func TestSendMessageAndSendOfferPostToTheConversation(t *testing.T) {
	message := fmt.Sprintf(`{"id":%q,"conversationId":%q,"author":"SELLER","type":"TEXT","status":"SENT","body":"hi","createdAt":"2026-10-21T10:00:00Z"}`, account, account)
	var paths, bodies []string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(raw))
		reply(w, 201, message)
	})
	ctx := context.Background()
	if _, err := c.Conversations.SendMessage(ctx, account, controlresell.SendMessageRequest{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	offer := controlresell.SendOfferRequest{Price: controlresell.Money{Amount: "12.50", Currency: "EUR"}}
	if _, err := c.Conversations.SendOffer(ctx, account, offer); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/api/public/v1/conversations/"+account+"/messages" || paths[1] != "/api/public/v1/conversations/"+account+"/offers" || !strings.Contains(bodies[1], "12.50") {
		t.Fatalf("%v %v", paths, bodies)
	}
}

func TestCreatePostsTheItemWithAKeyAndUpdatePatchesItWithoutOne(t *testing.T) {
	var patched string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		status := 200
		if n == 1 {
			status = 201
		}
		if n == 2 {
			body, _ := io.ReadAll(r.Body)
			patched = string(body)
		}
		reply(w, status, item(7, "WOMEN"))
	})
	created, err := c.Items.Create(context.Background(), controlresell.ItemCreateRequest{Name: "n"}, controlresell.WithIdempotencyKey("k1"))
	if err != nil {
		t.Fatal(err)
	}
	patch := controlresell.ItemUpdateRequest{}
	patch.SetName("m")
	updated, err := c.Items.Update(context.Background(), 7, patch)
	if err != nil {
		t.Fatal(err)
	}
	if created.Id != 7 || updated.Id != 7 {
		t.Fatalf("%v %v", created, updated)
	}
	if s.calls[0].method != "POST" || s.calls[0].header.Get(controlresell.IdempotencyKeyHeader) != "k1" {
		t.Fatalf("%v", s.calls[0])
	}
	if strings.TrimSpace(patched) != `{"name":"m"}` {
		t.Fatalf("%s", patched)
	}
	if s.calls[1].method != "PATCH" || s.calls[1].header.Get(controlresell.IdempotencyKeyHeader) != "" {
		t.Fatalf("%v", s.calls[1])
	}
}

const order = `{"id":"3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10","accountId":"3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10","type":"SALE",` +
	`"status":"PAID","platformOrderId":%q,"price":{"amount":"12.50","currency":"EUR"},"prepared":false,` +
	`"date":"2026-10-21T10:00:00Z","items":[]}`

func TestOrdersGetAndLabelsReadTheOrderAndItsLabels(t *testing.T) {
	var paths []string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/labels") {
			reply(w, 200, `{"data":[],"nextCursor":null}`)
			return
		}
		reply(w, 200, fmt.Sprintf(order, "p1"))
	})
	id := "3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10"
	got, err := c.Orders.Get(context.Background(), id)
	if err != nil || got.PlatformOrderId != "p1" {
		t.Fatalf("%v %v", got, err)
	}
	labels, err := c.Orders.Labels(context.Background(), id)
	if err != nil || len(labels.Data) != 0 {
		t.Fatalf("%v %v", labels, err)
	}
	if len(paths) != 2 || paths[0] != "/api/public/v1/orders/"+id || paths[1] != "/api/public/v1/orders/"+id+"/labels" {
		t.Fatalf("%v", paths)
	}
}

func TestOrdersAllFollowsTheCursorWithTheSameFilters(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 200, `{"data":[`+fmt.Sprintf(order, "a")+`],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[`+fmt.Sprintf(order, "b")+`],"nextCursor":null}`)
	})
	params := controlresell.OrderListParams{Bucket: "to_send", Platform: "VINTED", From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	var ids []string
	for o, err := range c.Orders.All(context.Background(), params) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.PlatformOrderId)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("%v", ids)
	}
	for _, call := range s.calls {
		if !strings.Contains(call.query, "bucket=to_send") || !strings.Contains(call.query, "platform=VINTED") || !strings.Contains(call.query, "from=2026-10-01") {
			t.Fatalf("%s", call.query)
		}
	}
	if strings.Contains(s.calls[0].query, "cursor") || !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%s | %s", s.calls[0].query, s.calls[1].query)
	}
}

func TestOrdersAllLabelsToShipFollowsTheCursorOnOrdersLabels(t *testing.T) {
	var path string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			path = r.URL.Path
			reply(w, 200, `{"data":[{"id":"l1","orderId":"o1"}],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[{"id":"l2","orderId":"o2"}],"nextCursor":null}`)
	})
	var orders []string
	for l, err := range c.Orders.AllLabelsToShip(context.Background(), controlresell.ListParams{Limit: 1}) {
		if err != nil {
			t.Fatal(err)
		}
		orders = append(orders, l.OrderId)
	}
	if len(orders) != 2 || orders[0] != "o1" || orders[1] != "o2" {
		t.Fatalf("%v", orders)
	}
	if path != "/api/public/v1/orders/labels" || !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%v", s.calls)
	}
}

const customer = `{"id":"3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10","firstName":%q,"lastName":"Doe","email":null,` +
	`"address":{"country":"FR"},"createdAt":"2026-10-21T10:00:00Z"}`

func TestCustomersAllFollowsTheCursorWithTheSameFilters(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 200, `{"data":[`+fmt.Sprintf(customer, "Ann")+`],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[`+fmt.Sprintf(customer, "Bob")+`],"nextCursor":null}`)
	})
	params := controlresell.CustomerListParams{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), AccountID: "acc"}
	var names []string
	for cu, err := range c.Customers.All(context.Background(), params) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, cu.FirstName)
	}
	if len(names) != 2 || names[0] != "Ann" || names[1] != "Bob" {
		t.Fatalf("%v", names)
	}
	for _, call := range s.calls {
		if !strings.Contains(call.query, "from=2026-10-01") || !strings.Contains(call.query, "accountId=acc") {
			t.Fatalf("%s", call.query)
		}
	}
	if strings.Contains(s.calls[0].query, "cursor") || !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%s | %s", s.calls[0].query, s.calls[1].query)
	}
}

func TestCustomersGetReadsOneCustomer(t *testing.T) {
	var path string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		reply(w, 200, fmt.Sprintf(customer, "Ann"))
	})
	id := "3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10"
	got, err := c.Customers.Get(context.Background(), id)
	if err != nil || got.FirstName != "Ann" || path != "/api/public/v1/customers/"+id {
		t.Fatalf("%v %v %s", got, err, path)
	}
}

const favorite = `{"id":"3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10","accountId":"3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10","platform":"VINTED",` +
	`"buyerId":%q,"createdAt":"2026-10-24T00:00:00Z","messageSentAt":null,"listingId":"L1",` +
	`"item":{"id":42,"name":"Jacket","price":{"amount":"12.50","currency":"EUR"},"sold":false}}`

func TestFavoritesAllFollowsTheCursorWithTheSameFilters(t *testing.T) {
	var path string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if n == 1 {
			reply(w, 200, `{"data":[`+fmt.Sprintf(favorite, "b1")+`],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[`+fmt.Sprintf(favorite, "b2")+`],"nextCursor":null}`)
	})
	params := controlresell.FavoriteListParams{From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ItemID: 42, AccountID: "acc"}
	var buyers []string
	for f, err := range c.Favorites.All(context.Background(), params) {
		if err != nil {
			t.Fatal(err)
		}
		if f.Item.Get().Name != "Jacket" {
			t.Fatalf("%v", f)
		}
		buyers = append(buyers, f.BuyerId)
	}
	if len(buyers) != 2 || buyers[0] != "b1" || buyers[1] != "b2" {
		t.Fatalf("%v", buyers)
	}
	for _, call := range s.calls {
		if path != "/api/public/v1/favorites" || !strings.Contains(call.query, "from=2026-10-01") ||
			!strings.Contains(call.query, "itemId=42") || !strings.Contains(call.query, "accountId=acc") {
			t.Fatalf("%s?%s", path, call.query)
		}
	}
	if strings.Contains(s.calls[0].query, "cursor") || !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%s | %s", s.calls[0].query, s.calls[1].query)
	}
}

func TestFavoritesAllItemReadsTheFavoritesOfOneItem(t *testing.T) {
	var path string
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		reply(w, 200, `{"data":[`+fmt.Sprintf(favorite, "b1")+`],"nextCursor":null}`)
	})
	params := controlresell.ItemFavoriteListParams{To: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	count := 0
	for _, err := range c.Favorites.AllItem(context.Background(), 42, params) {
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 1 || path != "/api/public/v1/items/42/favorites" || !strings.Contains(s.calls[0].query, "to=2026-10-02") {
		t.Fatalf("%d %s?%s", count, path, s.calls[0].query)
	}
}

func TestConversationsListSendsTheBuyerItemAndOfferStatusFilters(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"data":[],"nextCursor":null}`)
	})
	if _, err := c.Conversations.List(context.Background(), controlresell.ConversationListParams{BuyerID: "b-1", ItemID: 42, OfferStatus: "PENDING"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"buyerId=b-1", "itemId=42", "offerStatus=PENDING"} {
		if !strings.Contains(s.calls[0].query, want) {
			t.Fatalf("%s: %s", want, s.calls[0].query)
		}
	}
}

func TestCatalogsAllBrandsFollowsTheCursorWithTheSameQuery(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 200, `{"data":[{"id":1,"name":"Nike","custom":false}],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[{"id":2,"name":"Nikon","custom":true}],"nextCursor":null}`)
	})
	var names []string
	for b, err := range c.Catalogs.AllBrands(context.Background(), controlresell.CatalogListParams{Q: "nik"}) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, b.Name)
	}
	if len(names) != 2 || names[0] != "Nike" || names[1] != "Nikon" {
		t.Fatalf("%v", names)
	}
	for _, call := range s.calls {
		if !strings.Contains(call.query, "q=nik") {
			t.Fatalf("%s", call.query)
		}
	}
	if strings.Contains(s.calls[0].query, "cursor") || !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%s | %s", s.calls[0].query, s.calls[1].query)
	}
}

func TestCatalogsColorsSendTheCategoryAndReadTheValues(t *testing.T) {
	var path, query string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		reply(w, 200, `{"data":[{"id":3,"key":"red","title":"Red"}],"nextCursor":null}`)
	})
	page, err := c.Catalogs.Colors(context.Background(), controlresell.CatalogListParams{CategoryID: 12})
	if err != nil || len(page.Data) != 1 || page.Data[0].Title != "Red" ||
		path != "/api/public/v1/catalogs/colors" || !strings.Contains(query, "categoryId=12") {
		t.Fatalf("%v %v %s %s", page, err, path, query)
	}
}

func TestCatalogsCategoryFieldsReadsTheFieldsOfOneCategory(t *testing.T) {
	var path string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		reply(w, 200, `{"categoryId":12,"sizeGroupIds":[1],"enumFields":[],"textFields":[],"measurements":[]}`)
	})
	got, err := c.Catalogs.CategoryFields(context.Background(), 12)
	if err != nil || got.CategoryId != 12 || path != "/api/public/v1/catalogs/categories/12/fields" {
		t.Fatalf("%v %v %s", got, err, path)
	}
}

func TestConversationsMessagesSendTheFromAndToWindow(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"data":[],"nextCursor":null}`)
	})
	params := controlresell.MessageListParams{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}
	if _, err := c.Conversations.Messages(context.Background(), "3f0c2a52-0b0c-4d65-9f40-2d2d3b1f4a10", params); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.calls[0].query, "from=2026-10-01") || !strings.Contains(s.calls[0].query, "to=2026-10-02") {
		t.Fatalf("%s", s.calls[0].query)
	}
}

func TestWebhooksCreateEndpointPostsWithAKeyAndReturnsTheSecret(t *testing.T) {
	var method, path, key, body string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		method, path, key = r.Method, r.URL.Path, r.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		reply(w, 201, `{"endpoint":{"id":"0b6f3c1e-8d43-4a4c-9a55-7f1f2c3d4e5f","url":"https://example.com/hook","eventTypes":["item.sold"],"payloadMode":"light","status":"enabled","apiVersion":"v1","createdAt":"2026-10-24T00:00:00Z","updatedAt":"2026-10-24T00:00:00Z"},"secret":"whsec_abc"}`)
	})
	created, err := c.Webhooks.CreateEndpoint(context.Background(), controlresell.WebhookEndpointCreateRequest{Url: "https://example.com/hook", EventTypes: []string{"item.sold"}})
	if err != nil || created.Secret != "whsec_abc" || created.Endpoint.Status != "enabled" {
		t.Fatalf("%v %v", created, err)
	}
	if method != "POST" || path != "/api/public/v1/webhook-endpoints" || key == "" || !strings.Contains(body, `"eventTypes":["item.sold"]`) {
		t.Fatalf("%s %s %q %s", method, path, key, body)
	}
}

func TestWebhooksAllEventsFollowsTheCursorWithTheSameTypesAndReadsAnyData(t *testing.T) {
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 200, `{"data":[{"id":"0192f3c1-0000-7000-8000-000000000001","type":"item.sold","createdAt":"2026-10-24T00:00:00Z","apiVersion":"v1","sellerId":7,"data":{"itemId":42,"source":"order","object":null}}],"nextCursor":"c2"}`)
			return
		}
		reply(w, 200, `{"data":[{"id":"0192f3c1-0000-7000-8000-000000000002","type":"order.created","createdAt":"2026-10-24T00:01:00Z","apiVersion":"v1","sellerId":7,"data":{"orderId":"o1","items":[1,2]}}],"nextCursor":null}`)
	})
	var types []string
	var itemID any
	for e, err := range c.Webhooks.AllEvents(context.Background(), controlresell.EventListParams{Types: []string{"item.sold", "order.created"}}) {
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, e.Type)
		if e.Type == "item.sold" {
			itemID = e.Data["itemId"]
		}
	}
	if len(types) != 2 || itemID != float64(42) {
		t.Fatalf("%v %v", types, itemID)
	}
	for _, call := range s.calls {
		if !strings.Contains(call.query, "type=item.sold%2Corder.created") {
			t.Fatalf("%s", call.query)
		}
	}
	if !strings.Contains(s.calls[1].query, "cursor=c2") {
		t.Fatalf("%s", s.calls[1].query)
	}
}

func TestWebhooksDeliveriesSendTheFiltersAndRetryPostsToTheDelivery(t *testing.T) {
	var paths, queries []string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		paths, queries = append(paths, r.Method+" "+r.URL.Path), append(queries, r.URL.RawQuery)
		delivery := `{"id":"d1","eventId":"e1","eventType":"item.sold","status":"failed","attempt":3,"createdAt":"2026-10-24T00:00:00Z","updatedAt":"2026-10-24T00:00:00Z"}`
		if n == 1 {
			reply(w, 200, `{"data":[`+delivery+`],"nextCursor":null}`)
			return
		}
		reply(w, 202, delivery)
	})
	page, err := c.Webhooks.Deliveries(context.Background(), "e-1", controlresell.DeliveryListParams{Status: "failed"})
	if err != nil || len(page.Data) != 1 || page.Data[0].Attempt != 3 {
		t.Fatalf("%v %v", page, err)
	}
	if _, err := c.Webhooks.RetryDelivery(context.Background(), "e-1", "d1"); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "GET /api/public/v1/webhook-endpoints/e-1/deliveries" || !strings.Contains(queries[0], "status=failed") ||
		paths[1] != "POST /api/public/v1/webhook-endpoints/e-1/deliveries/d1/retry" {
		t.Fatalf("%v %v", paths, queries)
	}
}

func TestWebhooksDeleteEndpointResolvesOn204(t *testing.T) {
	var method string
	_, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.WriteHeader(204)
	})
	if err := c.Webhooks.DeleteEndpoint(context.Background(), "e-1"); err != nil || method != "DELETE" {
		t.Fatalf("%v %s", err, method)
	}
}

func TestWebhooksTestIsNeverReplayedAfterA5xxOrALostConnectionButIsAfterA429(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) { reply(w, status, envelope("unavailable")) })
		if _, err := c.Webhooks.Test(context.Background(), "e-1"); err == nil {
			t.Fatalf("status %d: no error", status)
		}
		if s.count() != 1 {
			t.Fatalf("status %d replayed the test: %d calls", status, s.count())
		}
	}
	s, c := client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close() // the request reached the server, the answer is lost
	})
	if _, err := c.Webhooks.Test(context.Background(), "e-1"); err == nil || s.count() != 1 {
		t.Fatalf("a lost connection replayed the test: %d calls, %v", s.count(), err)
	}
	s, c = client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 429, envelope("webhook_endpoint_busy"), "Retry-After", "0")
			return
		}
		reply(w, 200, `{"success":true,"statusCode":200,"durationMs":12}`)
	})
	if result, err := c.Webhooks.Test(context.Background(), "e-1"); err != nil || !result.Success || s.count() != 2 {
		t.Fatalf("%v %v %d", result, err, s.count())
	}
	s, c = client(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			reply(w, 503, envelope("unavailable"))
			return
		}
		reply(w, 200, `{"data":[],"nextCursor":null}`)
	})
	if _, err := c.Accounts.List(context.Background(), controlresell.ListParams{}); err != nil || s.count() != 2 {
		t.Fatalf("other calls keep the retry on 503: %v %d", err, s.count())
	}
}

type dialFailing struct{ calls int }

func (d *dialFailing) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls++
	if d.calls == 1 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"success":true,"durationMs":1}`))}, nil
}

func TestWebhooksTestIsReplayedAfterAFailedDial(t *testing.T) {
	transport := &dialFailing{}
	c, err := controlresell.NewClient(
		controlresell.WithAPIKey(key),
		controlresell.WithBaseURL("http://api.test"),
		controlresell.WithHTTPClient(&http.Client{Transport: transport}),
		controlresell.WithRetryPolicy(controlresell.RetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Webhooks.Test(context.Background(), "e-1"); err != nil || transport.calls != 2 {
		t.Fatalf("%v %d", err, transport.calls)
	}
}

type headerSpy struct{ header http.Header }

func (h *headerSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	h.header = r.Header.Clone()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"success":true,"durationMs":1}`))}, nil
}

func TestWebhooksTestKeyIsNotSeenByNetHTTPAsAReplayMarker(t *testing.T) {
	spy := &headerSpy{}
	c, err := controlresell.NewClient(
		controlresell.WithAPIKey(key),
		controlresell.WithBaseURL("http://api.test"),
		controlresell.WithHTTPClient(&http.Client{Transport: spy}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Webhooks.Test(context.Background(), "e-1"); err != nil {
		t.Fatal(err)
	}
	if len(spy.header["Idempotency-Key"]) != 0 || len(spy.header["idempotency-key"]) != 1 {
		t.Fatalf("%v", spy.header)
	}
}
