# ControlResell Go SDK

Go client for the [ControlResell](https://controlresell.com) public API (Go 1.23+).

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

// Writes (API key with items:write / conversations:write); controlresell.WithIdempotencyKey("...") chooses the key.
created, err := client.Items.Create(ctx, controlresell.ItemCreateRequest{Name: "Jacket"}) // Items.Update(ctx, id, controlresell.ItemUpdateRequest{...}) patches
scheduled, err := client.Items.Publish(ctx, 42, controlresell.PublishRequest{AccountIds: []string{accountID}})
message, err := client.Conversations.SendMessage(ctx, conversationID, controlresell.SendMessageRequest{Text: "Hello!"})
```

Retries (rate limits, 502/503/504, network errors), `Idempotency-Key` on every `POST`, cursor pagination with range-over-func iterators and typed errors (`*NotFoundError`, `*RateLimitError`... all unwrapping to `*APIError`) are built in. The client is generated from the API's OpenAPI document (`scripts/generate.sh`; the generated files are listed in `.generated-files`).

```sh
go test ./...
```

Not published yet (no version tag). Licensed under the Apache License 2.0.
