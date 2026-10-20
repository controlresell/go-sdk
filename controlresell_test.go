package controlresell_test

import (
	"context"
	"errors"
	"fmt"
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
