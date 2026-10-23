package controlresell

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"os"
	"time"
)

// Version is the version of this SDK.
const Version = "0.1.0"

// DefaultBaseURL is the production address of the API.
const DefaultBaseURL = "https://api.controlresell.com"

// APIKeyVariable is the environment variable read when no API key is given.
const APIKeyVariable = "CONTROLRESELL_API_KEY"

type options struct {
	apiKey  string
	baseURL string
	retry   RetryPolicy
	http    *http.Client
}

// Option configures a Client.
type Option func(*options)

// WithAPIKey sets the API key; absent, the CONTROLRESELL_API_KEY environment variable is read.
func WithAPIKey(key string) Option { return func(o *options) { o.apiKey = key } }

// WithBaseURL sets the API's address (staging, a test environment).
func WithBaseURL(url string) Option { return func(o *options) { o.baseURL = url } }

// WithRetryPolicy sets how calls are retried.
func WithRetryPolicy(policy RetryPolicy) Option { return func(o *options) { o.retry = policy } }

// WithHTTPClient sets the http.Client to call through (proxy, timeouts...); its transport gets the retries.
func WithHTTPClient(client *http.Client) Option { return func(o *options) { o.http = client } }

// Client is a client of the ControlResell public API, for one API key. It holds no global state: create one per API
// key. Calls are retried on a rate limit (after Retry-After), on a gateway or availability error (502, 503, 504), on
// a 409 idempotency_in_progress and on a network error; a POST always carries the same Idempotency-Key across its
// retries. Any other error is returned as a typed error (*APIError and its per-status types).
//
//	client, err := controlresell.NewClient(controlresell.WithAPIKey("cr_live_..."))
//	me, err := client.Me.Get(ctx)
//	for item, err := range client.Items.All(ctx, controlresell.ItemListParams{}) { ... }
type Client struct {
	// Me is the seller the API key acts for.
	Me *MeService
	// Accounts are the seller's marketplace accounts.
	Accounts *AccountsService
	// Items are the items of the seller's stock.
	Items *ItemsService
	// Conversations are the conversations with buyers.
	Conversations *ConversationsService
	// Orders are the seller's sales and purchases.
	Orders *OrdersService
	// Customers are the other parties of the seller's sales and purchases.
	Customers *CustomersService
	// Catalogs are the identifiers an item is written with: categories, brands, sizes, colors...
	Catalogs *CatalogsService
	// API is the underlying generated client, configured with the key and the retries.
	API *APIClient
}

// NewClient creates a client.
func NewClient(opts ...Option) (*Client, error) {
	o := options{baseURL: DefaultBaseURL, retry: DefaultRetryPolicy()}
	for _, opt := range opts {
		opt(&o)
	}
	if o.apiKey == "" {
		o.apiKey = os.Getenv(APIKeyVariable)
	}
	if o.apiKey == "" {
		return nil, errors.New("controlresell: no API key: use WithAPIKey or set " + APIKeyVariable)
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	if o.http != nil {
		copied := *o.http
		httpClient = &copied
	}
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = &retryTransport{base: base, policy: o.retry}

	configuration := NewConfiguration()
	configuration.Servers = ServerConfigurations{{URL: o.baseURL}}
	configuration.UserAgent = "controlresell-go/" + Version
	configuration.HTTPClient = httpClient
	configuration.AddDefaultHeader("Authorization", "Bearer "+o.apiKey)
	api := NewAPIClient(configuration)
	return &Client{
		Me:            &MeService{api},
		Accounts:      &AccountsService{api},
		Items:         &ItemsService{api},
		Conversations: &ConversationsService{api},
		Orders:        &OrdersService{api},
		Customers:     &CustomersService{api},
		Catalogs:      &CatalogsService{api},
		API:           api,
	}, nil
}

// MeService reads the seller the API key acts for.
type MeService struct{ api *APIClient }

// Get returns the seller.
func (s *MeService) Get(ctx context.Context) (*Me, error) {
	value, response, err := s.api.MeAPI.GetMe(ctx).Execute()
	return result(value, response, err)
}

// AccountsService reads the seller's marketplace accounts.
type AccountsService struct{ api *APIClient }

// ListParams are the paging parameters of a list: the zero value reads the first page with the default size.
type ListParams struct {
	// Limit is the page size (1 to 100); 0 for the default.
	Limit int32
	// Cursor is the opaque cursor of the page to read; empty for the first one.
	Cursor string
}

// List reads one page of accounts.
func (s *AccountsService) List(ctx context.Context, params ListParams) (*AccountPage, error) {
	request := s.api.AccountsAPI.ListAccounts(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// Get reads one account by its identifier.
func (s *AccountsService) Get(ctx context.Context, id string) (*Account, error) {
	value, response, err := s.api.AccountsAPI.GetAccount(ctx, id).Execute()
	return result(value, response, err)
}

// All yields every account, page after page.
func (s *AccountsService) All(ctx context.Context, limit int32) iter.Seq2[Account, error] {
	return paginate(func(cursor string) ([]Account, *string, error) {
		page, err := s.List(ctx, ListParams{Limit: limit, Cursor: cursor})
		if err != nil {
			return nil, nil, err
		}
		return page.Data, page.NextCursor.Get(), nil
	})
}

// ItemsService reads the items of the seller's stock.
type ItemsService struct{ api *APIClient }

// ItemListParams are the parameters of an item list.
type ItemListParams struct {
	ListParams
	// Sold keeps the sold (true) or the unsold (false) items; nil for both.
	Sold *bool
	// AccountID keeps the items listed on one marketplace account; empty for all.
	AccountID string
}

// List reads one page of items.
func (s *ItemsService) List(ctx context.Context, params ItemListParams) (*ItemPage, error) {
	request := s.api.ItemsAPI.ListItems(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.Sold != nil {
		request = request.Sold(*params.Sold)
	}
	if params.AccountID != "" {
		request = request.AccountId(params.AccountID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// Get reads one item by its identifier.
func (s *ItemsService) Get(ctx context.Context, id int64) (*Item, error) {
	value, response, err := s.api.ItemsAPI.GetItem(ctx, id).Execute()
	return result(value, response, err)
}

// Listings reads where an item is listed on the seller's marketplace accounts.
func (s *ItemsService) Listings(ctx context.Context, id int64) (*ListingPage, error) {
	value, response, err := s.api.ItemsAPI.ListItemListings(ctx, id).Execute()
	return result(value, response, err)
}

// Create adds an item to the stock, without publishing it. opts may set the Idempotency-Key: to retry a Create yourself
// (after a timeout, say), pass the same WithIdempotencyKey, or a second item is created.
func (s *ItemsService) Create(ctx context.Context, body ItemCreateRequest, opts ...WriteOption) (*Item, error) {
	value, response, err := s.api.ItemsAPI.CreateItem(ctx).ItemCreateRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// Update changes the given fields of an item (a PATCH: the fields left out are kept).
func (s *ItemsService) Update(ctx context.Context, id int64, body ItemUpdateRequest) (*Item, error) {
	value, response, err := s.api.ItemsAPI.UpdateItem(ctx, id).ItemUpdateRequest(body).Execute()
	return result(value, response, err)
}

// Delete removes an item from the stock; an item still online is refused (409 item_still_listed).
func (s *ItemsService) Delete(ctx context.Context, id int64) error {
	response, err := s.api.ItemsAPI.DeleteItem(ctx, id).Execute()
	if err != nil {
		return convert(response, err)
	}
	return nil
}

// Publish schedules the publication of an item on the given accounts. opts may set the Idempotency-Key.
func (s *ItemsService) Publish(ctx context.Context, id int64, body PublishRequest, opts ...WriteOption) (*ScheduledAction, error) {
	value, response, err := s.api.ItemsAPI.PublishItem(ctx, id).PublishRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// PriceDrop schedules a price drop of the item's listings. opts may set the Idempotency-Key.
func (s *ItemsService) PriceDrop(ctx context.Context, id int64, body PriceDropRequest, opts ...WriteOption) (*ScheduledAction, error) {
	value, response, err := s.api.ItemsAPI.DropItemPrice(ctx, id).PriceDropRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// Republish schedules the republication of an item (the zero RepublishRequest means every account it is listed on).
// opts may set the Idempotency-Key.
func (s *ItemsService) Republish(ctx context.Context, id int64, body RepublishRequest, opts ...WriteOption) (*ScheduledAction, error) {
	value, response, err := s.api.ItemsAPI.RepublishItem(ctx, id).RepublishRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// All yields every item, page after page, with the same filters (params.Cursor is ignored).
func (s *ItemsService) All(ctx context.Context, params ItemListParams) iter.Seq2[Item, error] {
	return paginate(func(cursor string) ([]Item, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.List(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// ConversationsService reads the conversations with buyers.
type ConversationsService struct{ api *APIClient }

// ConversationListParams are the parameters of a conversation list.
type ConversationListParams struct {
	ListParams
	// Unread keeps the unread (true) or the read (false) conversations; nil for both.
	Unread *bool
	// AccountID keeps the conversations of one marketplace account; empty for all.
	AccountID string
	// From keeps the conversations whose last message is at or after this instant (inclusive); zero for no bound.
	From time.Time
	// To keeps the conversations whose last message is before this instant (exclusive); zero for no bound.
	To time.Time
}

// List reads one page of conversations.
func (s *ConversationsService) List(ctx context.Context, params ConversationListParams) (*ConversationPage, error) {
	request := s.api.ConversationsAPI.ListConversations(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.Unread != nil {
		request = request.Unread(*params.Unread)
	}
	if params.AccountID != "" {
		request = request.AccountId(params.AccountID)
	}
	if !params.From.IsZero() {
		request = request.From(params.From)
	}
	if !params.To.IsZero() {
		request = request.To(params.To)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// Get reads one conversation by its identifier.
func (s *ConversationsService) Get(ctx context.Context, id string) (*Conversation, error) {
	value, response, err := s.api.ConversationsAPI.GetConversation(ctx, id).Execute()
	return result(value, response, err)
}

// MessageListParams are the parameters of a message list.
type MessageListParams struct {
	ListParams
	// From keeps the messages created at or after this instant (inclusive); zero for no bound.
	From time.Time
	// To keeps the messages created before this instant (exclusive); zero for no bound.
	To time.Time
}

// Messages reads one page of the messages of a conversation, oldest first.
func (s *ConversationsService) Messages(ctx context.Context, id string, params MessageListParams) (*MessagePage, error) {
	request := s.api.ConversationsAPI.ListConversationMessages(ctx, id)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if !params.From.IsZero() {
		request = request.From(params.From)
	}
	if !params.To.IsZero() {
		request = request.To(params.To)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// SendMessage sends a message to the buyer. opts may set the Idempotency-Key.
func (s *ConversationsService) SendMessage(ctx context.Context, id string, body SendMessageRequest, opts ...WriteOption) (*Message, error) {
	value, response, err := s.api.ConversationsAPI.SendMessage(ctx, id).SendMessageRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// SendOffer sends the buyer an offer. opts may set the Idempotency-Key.
func (s *ConversationsService) SendOffer(ctx context.Context, id string, body SendOfferRequest, opts ...WriteOption) (*Message, error) {
	value, response, err := s.api.ConversationsAPI.SendOffer(ctx, id).SendOfferRequest(body).IdempotencyKey(keyOf(opts)).Execute()
	return result(value, response, err)
}

// All yields every conversation, page after page, with the same filters (params.Cursor is ignored).
func (s *ConversationsService) All(ctx context.Context, params ConversationListParams) iter.Seq2[Conversation, error] {
	return paginate(func(cursor string) ([]Conversation, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.List(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// AllMessages yields every message of a conversation, page after page, with the same period (params.Cursor is ignored).
func (s *ConversationsService) AllMessages(ctx context.Context, id string, params MessageListParams) iter.Seq2[Message, error] {
	return paginate(func(cursor string) ([]Message, *string, error) {
		query := params
		query.Cursor = cursor
		page, err := s.Messages(ctx, id, query)
		if err != nil {
			return nil, nil, err
		}
		return page.Data, page.NextCursor.Get(), nil
	})
}

// OrdersService reads the seller's sales and purchases.
type OrdersService struct{ api *APIClient }

// OrderListParams are the parameters of an order list.
type OrderListParams struct {
	ListParams
	// Bucket is "to_send" (the sales to ship) or "to_pickup" (the purchases waiting at a pickup point); empty for all.
	Bucket string
	// Platform keeps the orders of the accounts on one marketplace ("VINTED", "SHOPIFY"...); empty for all.
	Platform string
	// AccountID keeps the orders of one marketplace account; empty for all.
	AccountID string
	// From keeps the orders dated at or after this instant (inclusive); zero for no bound.
	From time.Time
	// To keeps the orders dated before this instant (exclusive); zero for no bound.
	To time.Time
}

// List reads one page of orders, the most recent first.
func (s *OrdersService) List(ctx context.Context, params OrderListParams) (*OrderPage, error) {
	request := s.api.OrdersAPI.ListOrders(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.Bucket != "" {
		request = request.Bucket(params.Bucket)
	}
	if params.Platform != "" {
		request = request.Platform(params.Platform)
	}
	if params.AccountID != "" {
		request = request.AccountId(params.AccountID)
	}
	if !params.From.IsZero() {
		request = request.From(params.From)
	}
	if !params.To.IsZero() {
		request = request.To(params.To)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// Get reads one order by its identifier.
func (s *OrdersService) Get(ctx context.Context, id string) (*Order, error) {
	value, response, err := s.api.OrdersAPI.GetOrder(ctx, id).Execute()
	return result(value, response, err)
}

// Labels reads the shipping labels of an order.
func (s *OrdersService) Labels(ctx context.Context, id string) (*OrderLabelPage, error) {
	value, response, err := s.api.OrdersAPI.ListOrderLabels(ctx, id).Execute()
	return result(value, response, err)
}

// LabelsToShip reads one page of the shipping labels still to ship over all the orders, the oldest order first; each
// label names its order (OrderId).
func (s *OrdersService) LabelsToShip(ctx context.Context, params ListParams) (*OrderLabelPage, error) {
	request := s.api.OrdersAPI.ListLabelsToShip(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllLabelsToShip yields every label still to ship, page after page (params.Cursor is ignored).
func (s *OrdersService) AllLabelsToShip(ctx context.Context, params ListParams) iter.Seq2[OrderLabel, error] {
	return paginate(func(cursor string) ([]OrderLabel, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.LabelsToShip(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// All yields every order, page after page, with the same filters (params.Cursor is ignored).
func (s *OrdersService) All(ctx context.Context, params OrderListParams) iter.Seq2[Order, error] {
	return paginate(func(cursor string) ([]Order, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.List(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// CustomersService reads the other parties of the seller's sales and purchases (needs the customers:read scope).
type CustomersService struct{ api *APIClient }

// CustomerListParams are the parameters of a customer list.
type CustomerListParams struct {
	ListParams
	// AccountID keeps the customers of an order on one marketplace account; empty for all.
	AccountID string
	// From keeps the customers created at or after this instant (inclusive); zero for no bound.
	From time.Time
	// To keeps the customers created before this instant (exclusive); zero for no bound.
	To time.Time
}

// List reads one page of customers, the most recently created first.
func (s *CustomersService) List(ctx context.Context, params CustomerListParams) (*CustomerPage, error) {
	request := s.api.CustomersAPI.ListCustomers(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.AccountID != "" {
		request = request.AccountId(params.AccountID)
	}
	if !params.From.IsZero() {
		request = request.From(params.From)
	}
	if !params.To.IsZero() {
		request = request.To(params.To)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// Get reads one customer by its identifier.
func (s *CustomersService) Get(ctx context.Context, id string) (*Customer, error) {
	value, response, err := s.api.CustomersAPI.GetCustomer(ctx, id).Execute()
	return result(value, response, err)
}

// All yields every customer, page after page, with the same filters (params.Cursor is ignored).
func (s *CustomersService) All(ctx context.Context, params CustomerListParams) iter.Seq2[Customer, error] {
	return paginate(func(cursor string) ([]Customer, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.List(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// CatalogsService reads the catalog an item is written with (needs the items:read scope).
type CatalogsService struct{ api *APIClient }

// CatalogListParams are the parameters of a catalog list.
type CatalogListParams struct {
	ListParams
	// CategoryID keeps what applies to one category (sizes, colors, conditions, package sizes, materials); 0 for all.
	CategoryID int64
	// ParentID keeps the children of one category (categories only); 0 for all.
	ParentID int64
	// Platform keeps the categories of one marketplace (categories only); empty for all.
	Platform string
	// Q keeps the brands whose name starts with it (brands only); empty for all.
	Q string
}

// Categories reads one page of categories, flat, by identifier.
func (s *CatalogsService) Categories(ctx context.Context, params CatalogListParams) (*CatalogCategoryPage, error) {
	request := s.api.CatalogsAPI.ListCatalogCategories(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.ParentID > 0 {
		request = request.ParentId(params.ParentID)
	}
	if params.Platform != "" {
		request = request.Platform(params.Platform)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllCategories yields all the categories, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllCategories(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogCategory, error] {
	return paginate(func(cursor string) ([]CatalogCategory, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Categories(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// Category reads one category by its identifier.
func (s *CatalogsService) Category(ctx context.Context, id int64) (*CatalogCategory, error) {
	value, response, err := s.api.CatalogsAPI.GetCatalogCategory(ctx, id).Execute()
	return result(value, response, err)
}

// CategoryFields reads what an item of a category is filled with: its size groups, enumerated and text fields, measurements.
func (s *CatalogsService) CategoryFields(ctx context.Context, id int64) (*CatalogCategoryFields, error) {
	value, response, err := s.api.CatalogsAPI.GetCatalogCategoryFields(ctx, id).Execute()
	return result(value, response, err)
}

// Brands reads one page of brands, by identifier or, with params.Q, by name.
func (s *CatalogsService) Brands(ctx context.Context, params CatalogListParams) (*CatalogBrandPage, error) {
	request := s.api.CatalogsAPI.ListCatalogBrands(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.Q != "" {
		request = request.Q(params.Q)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllBrands yields all the brands, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllBrands(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogBrand, error] {
	return paginate(func(cursor string) ([]CatalogBrand, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Brands(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// Sizes reads one page of sizes, each with its size group; params.CategoryID keeps those of one category.
func (s *CatalogsService) Sizes(ctx context.Context, params CatalogListParams) (*CatalogSizePage, error) {
	request := s.api.CatalogsAPI.ListCatalogSizes(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.CategoryID > 0 {
		request = request.CategoryId(params.CategoryID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllSizes yields all the sizes, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllSizes(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogSize, error] {
	return paginate(func(cursor string) ([]CatalogSize, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Sizes(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// Colors reads one page of colors; params.CategoryID keeps those of one category.
func (s *CatalogsService) Colors(ctx context.Context, params CatalogListParams) (*CatalogValuePage, error) {
	request := s.api.CatalogsAPI.ListCatalogColors(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.CategoryID > 0 {
		request = request.CategoryId(params.CategoryID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllColors yields all the colors, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllColors(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogValue, error] {
	return paginate(func(cursor string) ([]CatalogValue, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Colors(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// Conditions reads one page of conditions; params.CategoryID keeps those of one category.
func (s *CatalogsService) Conditions(ctx context.Context, params CatalogListParams) (*CatalogValuePage, error) {
	request := s.api.CatalogsAPI.ListCatalogConditions(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.CategoryID > 0 {
		request = request.CategoryId(params.CategoryID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllConditions yields all the conditions, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllConditions(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogValue, error] {
	return paginate(func(cursor string) ([]CatalogValue, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Conditions(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// PackageSizes reads one page of package sizes; params.CategoryID keeps those of one category.
func (s *CatalogsService) PackageSizes(ctx context.Context, params CatalogListParams) (*CatalogValuePage, error) {
	request := s.api.CatalogsAPI.ListCatalogPackageSizes(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.CategoryID > 0 {
		request = request.CategoryId(params.CategoryID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllPackageSizes yields all the package sizes, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllPackageSizes(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogValue, error] {
	return paginate(func(cursor string) ([]CatalogValue, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.PackageSizes(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// Materials reads one page of materials; params.CategoryID keeps those of one category.
func (s *CatalogsService) Materials(ctx context.Context, params CatalogListParams) (*CatalogValuePage, error) {
	request := s.api.CatalogsAPI.ListCatalogMaterials(ctx)
	if params.Limit > 0 {
		request = request.Limit(params.Limit)
	}
	if params.Cursor != "" {
		request = request.Cursor(params.Cursor)
	}
	if params.CategoryID > 0 {
		request = request.CategoryId(params.CategoryID)
	}
	value, response, err := request.Execute()
	return result(value, response, err)
}

// AllMaterials yields all the materials, page after page, with the same filters (params.Cursor is ignored).
func (s *CatalogsService) AllMaterials(ctx context.Context, params CatalogListParams) iter.Seq2[CatalogValue, error] {
	return paginate(func(cursor string) ([]CatalogValue, *string, error) {
		page := params
		page.Cursor = cursor
		result, err := s.Materials(ctx, page)
		if err != nil {
			return nil, nil, err
		}
		return result.Data, result.NextCursor.Get(), nil
	})
}

// WriteOption configures a write call.
type WriteOption func(*writeOptions)

type writeOptions struct{ idempotencyKey string }

// WithIdempotencyKey sets the Idempotency-Key of a write (a random one by default). The key is the same at every retry.
func WithIdempotencyKey(key string) WriteOption {
	return func(o *writeOptions) { o.idempotencyKey = key }
}

// keyOf returns the Idempotency-Key of a write: the caller's, else a random one chosen once, before the retries.
func keyOf(opts []WriteOption) string {
	var o writeOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.idempotencyKey != "" {
		return o.idempotencyKey
	}
	return newKey()
}

func result[T any](value *T, response *http.Response, err error) (*T, error) {
	if err != nil {
		return nil, convert(response, err)
	}
	return value, nil
}
