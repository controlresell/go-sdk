package controlresell_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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
