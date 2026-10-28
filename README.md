# ControlResell Go SDK

Go client for the [ControlResell](https://controlresell.com) public API (Go 1.23+).

## Install

From the 1.0.0 release (Git tag `v1.0.0`):

```sh
go get github.com/controlresell/go-sdk@v1.0.0
```

Versions are Git tags; see the [releases page](https://github.com/controlresell/go-sdk/releases) for the available ones.


```go
import controlresell "github.com/controlresell/go-sdk"

client, err := controlresell.NewClient(controlresell.WithAPIKey("cr_live_...")) // or set CONTROLRESELL_API_KEY
me, err := client.Me.Get(ctx)
for item, err := range client.Items.All(ctx, controlresell.ItemListParams{}) {
	if err != nil {
		return err
	}
	fmt.Println(item.Name)
}
for order, err := range client.Orders.All(ctx, controlresell.OrderListParams{Bucket: "to_send"}) { // needs the orders:read scope; also Orders.Get(ctx, id), Orders.Labels(ctx, id)
	if err != nil {
		return err
	}
	fmt.Println(order.Id)
}

for brand, err := range client.Catalogs.AllBrands(ctx, controlresell.CatalogListParams{Q: "nik"}) { // the identifiers an item is written with (items:read): Categories, Category, CategoryFields, Brands, Sizes, Colors, Conditions, PackageSizes, Materials
	if err != nil {
		return err
	}
	fmt.Println(brand.Id, brand.Name)
}

for favorite, err := range client.Favorites.All(ctx, controlresell.FavoriteListParams{ItemID: 42}) { // the buyers who favorited a listing (items:read); also Favorites.AllItem(ctx, 42, ...), List, ListItem
	if err != nil {
		return err
	}
	fmt.Println(favorite.BuyerId)
}
conversations, err := client.Conversations.List(ctx, controlresell.ConversationListParams{BuyerID: "BUYER_ID", OfferStatus: "PENDING"}) // BuyerID, ItemID and OfferStatus filter the conversations

// Writes (API key with items:write / conversations:write); controlresell.WithIdempotencyKey("...") chooses the key.
created, err := client.Items.Create(ctx, controlresell.ItemCreateRequest{Name: "Jacket"}) // Items.Update(ctx, id, controlresell.ItemUpdateRequest{...}) patches
scheduled, err := client.Items.Publish(ctx, 42, controlresell.PublishRequest{AccountIds: []string{accountID}})
request := controlresell.PublishRequest{AccountIds: []string{accountID}}
request.SetScheduledAt(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) // Publish, PriceDrop and Republish take an optional ScheduledAt, at most 30 days ahead; without it they run now
scheduled, err = client.Items.Publish(ctx, 42, request)
message, err := client.Conversations.SendMessage(ctx, conversationID, controlresell.SendMessageRequest{Text: "Hello!"})

// Webhooks (webhooks:read / webhooks:write): Endpoints, Endpoint, CreateEndpoint, UpdateEndpoint, DeleteEndpoint, Secret,
// RotateSecret, Test, Deliveries, RetryDelivery, Recover, EventTypes; Events / AllEvents catch up the last 7 days.
endpoint, err := client.Webhooks.CreateEndpoint(ctx, controlresell.WebhookEndpointCreateRequest{Url: "https://example.com/hooks", EventTypes: []string{"item.sold"}})
fmt.Println(endpoint.Endpoint.Id, endpoint.Secret) // the signing secret, shown once (Webhooks.Secret reads it again)
for event, err := range client.Webhooks.AllEvents(ctx, controlresell.EventListParams{Types: []string{"item.sold"}}) {
	if err != nil {
		return err
	}
	fmt.Println(event.Type, event.Data["itemId"])
}
```

Retries (rate limits, 502/503/504, network errors), `Idempotency-Key` on every `POST`, cursor pagination with range-over-func iterators and typed errors (`*NotFoundError`, `*RateLimitError`... all unwrapping to `*APIError`) are built in. The client is generated from the API's OpenAPI document (`scripts/generate.sh`; the generated files are listed in `.generated-files`).

```sh
go test ./...
```

Licensed under the Apache License 2.0.
