package peablebilling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Existing Peable contracts/vocabulary/{version,identity}. No alternate origin
// or central-billing identity is selected by this client.
const peableOrigin = "https://api.peable.to"
const peableAudience = "oxy-service:6a2f851751b784a86fd0e958"
const peableVersion = "2026-11-03"

// TokenProvider must be Kaana's reviewed own workload identity, not a user token
// or another application's assumed role. Tokens are never stored or logged.
type TokenProvider func(context.Context, string) (string, error)
type Config struct {
	Enabled                                                bool
	ApplicationID, OwnerAccountID, MerchantID, Environment string
}
type Client struct {
	cfg   Config
	http  *http.Client
	token TokenProvider
}

func NewClient(cfg Config, transport http.RoundTripper, token TokenProvider) (*Client, error) {
	if !cfg.Enabled {
		return nil, ErrDisabled
	}
	if cfg.ApplicationID == "" || cfg.OwnerAccountID == "" || cfg.MerchantID == "" || token == nil || (cfg.Environment != "production" && cfg.Environment != "development") {
		return nil, ErrMismatch
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{cfg: cfg, token: token, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// request makes one HTTP attempt. In particular, POST errors never authorize a
// second create; the persisted delivery stage must reconcile them first.
func (c *Client) request(ctx context.Context, method, path, key string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Peable request: %w", err)
		}
	}
	token, err := c.token(ctx, peableAudience)
	if err != nil || token == "" {
		return ErrMismatch
	}
	req, err := http.NewRequestWithContext(ctx, method, peableOrigin+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build Peable request: %w", err)
	}
	// Do not let net/http replay an idempotency-key POST on a reused connection.
	// Unknown create outcomes belong to the durable reconciliation stage.
	if body != nil {
		req.GetBody = nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Peable-Version", peableVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return ErrReconcile
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		return ErrReconcile
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return ErrReconcile
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return ErrReconcile
	}
	return nil
}
func (c *Client) Verify(ctx context.Context, d Draft) error {
	var m struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		ApplicationID string `json:"oxy_app_id"`
		Owner         string `json:"owner_account"`
		Environment   string `json:"environment"`
		Live          *bool  `json:"livemode"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/merchants/me", "", nil, &m); err != nil {
		return err
	}
	live := c.cfg.Environment == "production"
	if m.Object != "merchant" || m.ID != c.cfg.MerchantID || m.ID != d.MerchantID || m.ApplicationID != c.cfg.ApplicationID || m.Owner != c.cfg.OwnerAccountID || m.Environment != c.cfg.Environment || m.Live == nil || *m.Live != live {
		return ErrMismatch
	}
	var customer struct {
		ID       string            `json:"id"`
		Object   string            `json:"object"`
		Currency *string           `json:"currency"`
		Live     *bool             `json:"livemode"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(d.CustomerID), "", nil, &customer); err != nil {
		return err
	}
	if customer.Object != "customer" || customer.ID != d.CustomerID || customer.Live == nil || *customer.Live != live || customer.Metadata["kaana_account_id"] != d.AccountID || (customer.Currency != nil && *customer.Currency != d.Currency) {
		return ErrMismatch
	}
	return nil
}

type Invoice struct {
	AutomaticTax struct {
		Enabled *bool `json:"enabled"`
	} `json:"automatic_tax"`
	Lines struct {
		Data []struct {
			Item     string `json:"invoice_item"`
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"data"`
		HasMore *bool `json:"has_more"`
	} `json:"lines"`
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Customer         string            `json:"customer"`
	Currency         string            `json:"currency"`
	Status           string            `json:"status"`
	Live             *bool             `json:"livemode"`
	AutoAdvance      *bool             `json:"auto_advance"`
	CollectionMethod string            `json:"collection_method"`
	DaysUntilDue     *int              `json:"days_until_due"`
	Metadata         map[string]string `json:"metadata"`
}
type Item struct {
	Discountable *bool             `json:"discountable"`
	ID           string            `json:"id"`
	Object       string            `json:"object"`
	Customer     string            `json:"customer"`
	Invoice      string            `json:"invoice"`
	Currency     string            `json:"currency"`
	Amount       string            `json:"amount"`
	Live         *bool             `json:"livemode"`
	Metadata     map[string]string `json:"metadata"`
}

func metadata(d Draft) map[string]string {
	return map[string]string{"kaana_draft_id": d.ID, "kaana_account_id": d.AccountID}
}
func (c *Client) validInvoice(v Invoice, d Draft) bool {
	return v.ID != "" && v.Object == "invoice" && v.Customer == d.CustomerID && v.Currency == d.Currency && v.Status == "draft" && v.AutomaticTax.Enabled != nil && *v.AutomaticTax.Enabled && v.Live != nil && *v.Live == (c.cfg.Environment == "production") && v.AutoAdvance != nil && !*v.AutoAdvance && v.CollectionMethod == "send_invoice" && v.DaysUntilDue != nil && *v.DaysUntilDue == d.DaysUntilDue && v.Metadata["kaana_draft_id"] == d.ID && v.Metadata["kaana_account_id"] == d.AccountID
}
func (c *Client) validItem(v Item, d Draft) bool {
	return v.ID != "" && v.Object == "invoiceitem" && v.Discountable != nil && !*v.Discountable && v.Customer == d.CustomerID && v.Invoice == d.InvoiceID && v.Currency == d.Currency && v.Amount == d.AmountMinor && v.Live != nil && *v.Live == (c.cfg.Environment == "production") && v.Metadata["kaana_draft_id"] == d.ID && v.Metadata["kaana_account_id"] == d.AccountID
}
func (c *Client) CreateInvoice(ctx context.Context, d Draft) (string, error) {
	var v Invoice
	body := map[string]any{"customer": d.CustomerID, "currency": d.Currency, "auto_advance": false, "collection_method": "send_invoice", "days_until_due": d.DaysUntilDue, "pending_invoice_items_behavior": "exclude", "automatic_tax": map[string]bool{"enabled": true}, "metadata": metadata(d)}
	if err := c.request(ctx, http.MethodPost, "/v1/invoices", "kaana-draft:"+d.ID, body, &v); err != nil {
		return "", err
	}
	if !c.validInvoice(v, d) {
		return "", ErrMismatch
	}
	return v.ID, nil
}
func (c *Client) CreateItem(ctx context.Context, d Draft) (string, error) {
	var v Item
	body := map[string]any{"customer": d.CustomerID, "invoice": d.InvoiceID, "currency": d.Currency, "amount": d.AmountMinor, "discountable": false, "metadata": metadata(d)}
	if err := c.request(ctx, http.MethodPost, "/v1/invoiceitems", "kaana-item:"+d.ID, body, &v); err != nil {
		return "", err
	}
	if !c.validItem(v, d) {
		return "", ErrMismatch
	}
	return v.ID, nil
}
func (c *Client) ReadInvoice(ctx context.Context, d Draft) error {
	var v Invoice
	if err := c.request(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(d.InvoiceID), "", nil, &v); err != nil {
		return err
	}
	if !c.validInvoice(v, d) || v.ID != d.InvoiceID {
		return ErrMismatch
	}
	return nil
}
func (c *Client) Complete(ctx context.Context, d Draft) error {
	var v Invoice
	if err := c.request(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(d.InvoiceID), "", nil, &v); err != nil {
		return err
	}
	if !c.validInvoice(v, d) || v.ID != d.InvoiceID || v.Lines.HasMore == nil || *v.Lines.HasMore || len(v.Lines.Data) != 1 {
		return ErrMismatch
	}
	line := v.Lines.Data[0]
	if line.Item != d.ItemID || line.Amount != d.AmountMinor || line.Currency != d.Currency {
		return ErrMismatch
	}
	return nil
}
func (c *Client) ReadItem(ctx context.Context, d Draft) error {
	var v Item
	if err := c.request(ctx, http.MethodGet, "/v1/invoiceitems/"+url.PathEscape(d.ItemID), "", nil, &v); err != nil {
		return err
	}
	if !c.validItem(v, d) || v.ID != d.ItemID {
		return ErrMismatch
	}
	return nil
}

// Reconciliation reads the merchant-scoped collection to its end, bounded to
// 1,000 objects. Absence, truncation or duplicate metadata never permits create.
func (c *Client) Find(ctx context.Context, d Draft, step string) (string, error) {
	path := "/v1/invoices"
	if step == "item" {
		path = "/v1/invoiceitems"
	} else if step != "invoice" {
		return "", ErrMismatch
	}
	query := url.Values{"customer": {d.CustomerID}, "limit": {"100"}}
	if step == "item" {
		query.Set("invoice", d.InvoiceID)
	}
	found := ""
	cursor := ""
	for page := 0; page < 10; page++ {
		if cursor != "" {
			query.Set("starting_after", cursor)
		}
		var list struct {
			Object  string            `json:"object"`
			Data    []json.RawMessage `json:"data"`
			HasMore *bool             `json:"has_more"`
		}
		if err := c.request(ctx, http.MethodGet, path+"?"+query.Encode(), "", nil, &list); err != nil {
			return "", err
		}
		if list.Object != "list" || list.HasMore == nil {
			return "", ErrMismatch
		}
		for _, raw := range list.Data {
			var ref struct {
				ID       string            `json:"id"`
				Metadata map[string]string `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &ref); err != nil || ref.ID == "" {
				return "", ErrMismatch
			}
			cursor = ref.ID
			if ref.Metadata["kaana_draft_id"] != d.ID {
				continue
			}
			if found != "" {
				return "", ErrMismatch
			}
			if step == "invoice" {
				var v Invoice
				if json.Unmarshal(raw, &v) != nil || !c.validInvoice(v, d) {
					return "", ErrMismatch
				}
			} else {
				var v Item
				if json.Unmarshal(raw, &v) != nil || !c.validItem(v, d) {
					return "", ErrMismatch
				}
			}
			found = ref.ID
		}
		if !*list.HasMore {
			if found == "" {
				return "", ErrReconcile
			}
			return found, nil
		}
		if len(list.Data) == 0 {
			return "", ErrMismatch
		}
	}
	return "", ErrReconcile
}

type EventReference struct{ DraftID, AccountID string }

func (c *Client) DraftForEvent(ctx context.Context, eventID string) (EventReference, error) {
	var ref EventReference
	if eventID == "" || len(eventID) > 128 {
		return ref, ErrMismatch
	}
	var event struct {
		Object  string  `json:"object"`
		ID      string  `json:"id"`
		Type    string  `json:"type"`
		Live    *bool   `json:"livemode"`
		Account *string `json:"account"`
		Data    struct {
			Object struct {
				Object   string            `json:"object"`
				Metadata map[string]string `json:"metadata"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/events/"+url.PathEscape(eventID), "", nil, &event); err != nil {
		return ref, err
	}
	if event.ID != eventID || event.Object != "event" || event.Live == nil || *event.Live != (c.cfg.Environment == "production") || event.Account != nil {
		return ref, ErrMismatch
	}
	expected := "invoice"
	switch event.Type {
	case "invoice.created", "invoice.updated":
	case "invoiceitem.created":
		expected = "invoiceitem"
	default:
		return ref, ErrMismatch
	}
	if event.Data.Object.Object != expected {
		return ref, ErrMismatch
	}
	ref = EventReference{DraftID: event.Data.Object.Metadata["kaana_draft_id"], AccountID: event.Data.Object.Metadata["kaana_account_id"]}
	if ref.DraftID == "" || len(ref.DraftID) > 128 || ref.AccountID == "" || len(ref.AccountID) > 128 {
		return EventReference{}, ErrMismatch
	}
	return ref, nil
}
