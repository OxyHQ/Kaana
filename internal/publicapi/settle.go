package publicapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// SettlementStore is the durable outbox (credentialstore.Postgres).
type SettlementStore interface {
	RecordSettlement(ctx context.Context, events []providercost.Event, settlement credentialstore.OutboxSettlement) error
	ClaimSettlements(ctx context.Context, limit int, lease time.Duration) ([]credentialstore.ClaimedSettlement, error)
	MarkSettlementDelivered(ctx context.Context, settlementID, status string) error
	DeferSettlement(ctx context.Context, settlementID string, retryAt time.Time, failure string) error
	ReadSettlements(ctx context.Context, afterPosition int64, limit int) ([]credentialstore.PositionedSettlement, error)
}

// SettlerConfig wires the settlement side.
type SettlerConfig struct {
	Store SettlementStore
	// URL is POST …/internal/kaana/v1/settlements.
	URL string
	// Token authenticates Kaana to Oxy as a workload.
	Token TokenSource
	// Signer signs each delivery with Kaana's own settlement key.
	Signer *edgeauth.Signer
	Client *http.Client
	Logger *slog.Logger
	Now    func() time.Time
	// Interval is how often the worker looks for due settlements when nothing
	// nudges it. Zero takes 5 seconds.
	Interval time.Duration
}

// Settler writes settlements to the outbox and delivers them to Oxy.
type Settler struct {
	store    SettlementStore
	url      string
	token    TokenSource
	signer   *edgeauth.Signer
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time
	interval time.Duration
	nudge    chan struct{}

	stopOnce sync.Once
	stop     chan struct{}
	stopped  chan struct{}
}

// Delivery bounds.
const (
	settlementBatch        = 20
	settlementLease        = time.Minute
	settlementMaxBackoff   = 10 * time.Minute
	settlementPostTimeout  = 10 * time.Second
	settlementWriteTimeout = 10 * time.Second
	maxAcknowledgementSize = 64 << 10
)

// NewSettler builds the settler. The settlement URL must be https, except a
// loopback origin for local development.
func NewSettler(config SettlerConfig) (*Settler, error) {
	parsed, err := url.Parse(config.URL)
	switch {
	case config.Store == nil:
		return nil, errors.New("publicapi: no settlement outbox")
	case err != nil || !secureOrLoopback(parsed):
		return nil, fmt.Errorf("publicapi: the Oxy settlement URL %q is not an https URL", config.URL)
	case config.Token == nil:
		return nil, errors.New("publicapi: no Oxy service token source")
	case config.Signer == nil:
		return nil, errors.New("publicapi: no settlement signing key")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	interval := config.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Settler{
		store: config.Store, url: parsed.String(), token: config.Token, signer: config.Signer, client: &copied,
		logger: logger, now: now, interval: interval, nudge: make(chan struct{}, 1),
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}, nil
}

// Settle writes one grant's settlement, with its provider costs, to the
// outbox. It is called exactly once per grant, whatever the outcome — served,
// failed, cancelled or never attempted — so Oxy releases the hold now rather
// than at its expiry. It never blocks the caller's response on Oxy: delivery
// is the worker's.
func (s *Settler) Settle(ctx context.Context, grant *contract.Grant, report contract.UsageReport, servedStep *int, cost providercost.Record) error {
	settlement := contract.Settlement{
		SchemaVersion: contract.SettlementSchemaVersion,
		SettlementID:  newSettlementID(),
		GrantID:       grant.GrantID,
		RequestID:     grant.RequestID,
		ServedStep:    servedStep,
		Report:        report,
		CreatedAt:     contract.NewTimestamp(s.now()),
	}
	if grant.Reservation != nil {
		id := grant.Reservation.ReservationID
		settlement.ReservationID = &id
	}
	if grant.Capacity != nil {
		id := grant.Capacity.MeteredUsageID
		settlement.MeteredUsageID = &id
	}
	if report.Outcome != contract.OutcomeCompleted && len(report.Units) == 0 {
		// Nothing was served: no step served it.
		settlement.ServedStep = nil
	}
	if err := settlement.Validate(); err != nil {
		s.logger.Error("a settlement does not satisfy the contract and was not written", "requestId", grant.RequestID, "grantId", grant.GrantID, "error", err)
		return err
	}
	body, err := json.Marshal(settlement)
	if err != nil {
		return fmt.Errorf("publicapi: encoding a settlement: %w", err)
	}
	events, err := providercost.Events(cost)
	if err != nil {
		// The cost cannot be recorded as measured; the settlement must still
		// be, or Oxy holds the customer's money until expiry for work that ran.
		s.logger.Error("provider cost of a settled request is not recordable", "requestId", grant.RequestID, "errorType", "provider_cost_persistence")
		events = nil
	}
	row := credentialstore.OutboxSettlement{SettlementID: settlement.SettlementID, GrantID: grant.GrantID, RequestID: string(grant.RequestID), Body: body}
	writeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), settlementWriteTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		err = s.store.RecordSettlement(writeContext, events, row)
		if err == nil || errors.Is(err, credentialstore.ErrSettlementConflict) || attempt == 3 {
			break
		}
		timer := time.NewTimer((50 * time.Millisecond) << (2 * attempt))
		select {
		case <-writeContext.Done():
			timer.Stop()
		case <-timer.C:
			continue
		}
		break
	}
	if err != nil {
		s.logger.Error("a settlement could not be written to the outbox", "requestId", grant.RequestID, "grantId", grant.GrantID,
			"errorType", "settlement_persistence", "error", err)
		return err
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
	return nil
}

func newSettlementID() string {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		panic(fmt.Sprintf("publicapi: the system entropy source failed: %v", err))
	}
	return "stl_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(entropy[:]))
}

// Run delivers due settlements until ctx ends or Stop is called. Every task
// runs one; the outbox's lease keeps each row with one deliverer at a time.
func (s *Settler) Run(ctx context.Context) {
	defer close(s.stopped)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.DeliverDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-ticker.C:
		case <-s.nudge:
		}
	}
}

// Stop ends Run after its current delivery and waits for it.
func (s *Settler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.stopped
}

// DeliverDue delivers every settlement that is due now, in batches.
func (s *Settler) DeliverDue(ctx context.Context) {
	for ctx.Err() == nil {
		claimed, err := s.store.ClaimSettlements(ctx, settlementBatch, settlementLease)
		if err != nil {
			s.logger.Error("settlements could not be claimed for delivery", "errorType", "settlement_claim", "error", err)
			return
		}
		for _, row := range claimed {
			s.deliver(ctx, row)
		}
		if len(claimed) < settlementBatch {
			return
		}
	}
}

// deliver posts one settlement, signed by Kaana, and records Oxy's answer. A
// settlement is never dropped: a refusal is retried slowly and logged, because
// the request it settles has already run.
func (s *Settler) deliver(ctx context.Context, row credentialstore.ClaimedSettlement) {
	status, failure := s.post(ctx, row)
	if failure == "" {
		if err := s.store.MarkSettlementDelivered(ctx, row.SettlementID, status); err != nil {
			// The lease expires and the row is delivered again; Oxy answers
			// duplicate, never a second charge.
			s.logger.Error("a delivered settlement could not be marked", "settlementId", row.SettlementID, "error", err)
		}
		return
	}
	backoff := min(settlementMaxBackoff, time.Duration(1<<min(row.Attempts, 10))*time.Second)
	backoff += time.Duration(mathrand.Int64N(int64(backoff/4) + 1))
	if failure == "oxy_rejected" {
		backoff = settlementMaxBackoff
		s.logger.Error("Oxy refused a settlement; it is retried until it is accepted", "settlementId", row.SettlementID, "attempts", row.Attempts)
	} else {
		s.logger.Warn("a settlement delivery failed and will be retried", "settlementId", row.SettlementID, "attempts", row.Attempts, "failure", failure)
	}
	if err := s.store.DeferSettlement(ctx, row.SettlementID, s.now().Add(backoff), failure); err != nil {
		s.logger.Error("a failed settlement delivery could not be deferred", "settlementId", row.SettlementID, "error", err)
	}
}

// post returns Oxy's acknowledgement status, or a closed failure class.
func (s *Settler) post(ctx context.Context, row credentialstore.ClaimedSettlement) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, settlementPostTimeout)
	defer cancel()
	token, err := s.token(ctx)
	if err != nil {
		return "", "kaana_identity_unavailable"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(row.Body))
	if err != nil {
		return "", "kaana_request_invalid"
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	s.signer.Sign(request.Header, row.Body)
	response, err := s.client.Do(request)
	if err != nil {
		return "", "oxy_unreachable"
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAcknowledgementSize+1))
	switch {
	case err != nil || len(body) > maxAcknowledgementSize:
		return "", "oxy_unreadable"
	case response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests:
		return "", "oxy_unavailable"
	case response.StatusCode != http.StatusOK:
		return "", "oxy_rejected"
	}
	var acknowledgement contract.SettlementAcknowledgement
	if decodeStrict(body, &acknowledgement) != nil || acknowledgement.Validate() != nil || acknowledgement.SettlementID != row.SettlementID {
		return "", "oxy_unreadable"
	}
	return string(acknowledgement.Status), ""
}

/* -------------------------------------------------------------------------- */
/*  Reconciliation: GET /internal/v1/settlements?cursor=                      */
/* -------------------------------------------------------------------------- */

// ReconciliationHandler serves Oxy's read of the outbox, signed by Oxy under
// oxy-kaana-settlement-read:v1 over the raw query string.
type ReconciliationHandler struct {
	Store    SettlementStore
	Verifier *edgeauth.Verifier
	Logger   *slog.Logger
}

const (
	reconciliationDefaultLimit = 100
	reconciliationMaxLimit     = 500
)

func (h ReconciliationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	refuse := func(status int, code contract.ErrorCode, message string) {
		failure := contract.NewError(NewRequestID(), code, message)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(failure)
	}
	if err := h.Verifier.Verify(r.Header, []byte(r.URL.RawQuery)); err != nil {
		if h.Logger != nil {
			h.Logger.Warn("rejected an unsigned settlement reconciliation read")
		}
		refuse(http.StatusUnauthorized, contract.CodeAuthenticationFailed, "the read is not signed by Oxy")
		return
	}
	query := r.URL.Query()
	after, limit := int64(0), reconciliationDefaultLimit
	for name := range query {
		if name != "cursor" && name != "limit" {
			refuse(http.StatusBadRequest, contract.CodeInvalidRequest, "the settlement page takes cursor and limit")
			return
		}
	}
	if raw := query.Get("cursor"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			refuse(http.StatusBadRequest, contract.CodeInvalidRequest, "the cursor is not one this feed issued")
			return
		}
		after = parsed
	}
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > reconciliationMaxLimit {
			refuse(http.StatusBadRequest, contract.CodeInvalidRequest, "limit is 1..500")
			return
		}
		limit = parsed
	}
	rows, err := h.Store.ReadSettlements(r.Context(), after, limit)
	if err != nil {
		refuse(http.StatusServiceUnavailable, contract.CodeServiceUnavailable, "the settlement outbox is unavailable")
		return
	}
	// The bodies are the exact bytes delivered, so the page carries them as
	// they are rather than re-encoding them.
	var page bytes.Buffer
	page.WriteString(`{"data":[`)
	for index, row := range rows {
		if index > 0 {
			page.WriteByte(',')
		}
		page.Write(row.Body)
	}
	page.WriteByte(']')
	// The cursor is the last row's position, so a reader can always resume
	// from what it has read; an empty page is the last one and carries none.
	if len(rows) > 0 {
		cursor, _ := json.Marshal(strconv.FormatInt(rows[len(rows)-1].Position, 10))
		page.WriteString(`,"nextCursor":`)
		page.Write(cursor)
	}
	page.WriteByte('}')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page.Bytes())
}
