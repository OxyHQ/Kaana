package peablebilling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperatorConfig is non-secret reviewed deployment configuration. It neither
// selects a payer nor grants permission to create a merchant or a billing account.
type OperatorConfig struct {
	Enabled        bool   `json:"enabled"`
	ApplicationID  string `json:"applicationId"`
	OwnerAccountID string `json:"ownerAccountId"`
	MerchantID     string `json:"merchantId"`
	Environment    string `json:"environment"`
}

func (c OperatorConfig) ClientConfig() Config {
	return Config(c)
}

// ReadOperatorConfig verifies exact reviewed bytes before any database or
// identity connection. There is no default seller, environment or opt-in.
func ReadOperatorConfig(path, expectedSHA string) (OperatorConfig, error) {
	var c OperatorConfig
	digest, err := hex.DecodeString(expectedSHA)
	if err != nil || len(digest) != sha256.Size || expectedSHA != strings.ToLower(expectedSHA) {
		return c, ErrMismatch
	}
	f, err := os.Open(path)
	if err != nil {
		return c, ErrMismatch
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
		return c, ErrMismatch
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	sum := sha256.Sum256(raw)
	if err != nil || len(raw) > 65536 || !bytes.Equal(sum[:], digest) {
		return c, ErrMismatch
	}
	if err = decodeOperatorJSON(raw, &c); err != nil {
		return c, err
	}
	if !c.Enabled {
		return c, ErrDisabled
	}
	if !operatorID(c.ApplicationID) || !operatorID(c.OwnerAccountID) || !operatorID(c.MerchantID) || (c.Environment != "production" && c.Environment != "development") {
		return c, ErrMismatch
	}
	return c, nil
}

// Operation names exact existing receipts/terms. Delivery and event wakeup have
// no account-creation, finalization, send, pay, refund or funding operation.
type Operation struct {
	Action       string   `json:"action"`
	AccountID    string   `json:"accountId,omitempty"`
	CustomerID   string   `json:"customerId,omitempty"`
	Key          string   `json:"key,omitempty"`
	ReceiptIDs   []string `json:"receiptIds,omitempty"`
	DaysUntilDue int      `json:"daysUntilDue,omitempty"`
	DraftID      string   `json:"draftId,omitempty"`
	EventID      string   `json:"eventId,omitempty"`
}

func ReadOperation(input io.Reader) (Operation, error) {
	var op Operation
	raw, err := io.ReadAll(io.LimitReader(input, 262145))
	if err != nil || len(raw) > 262144 {
		return op, ErrMismatch
	}
	if err = decodeOperatorJSON(raw, &op); err != nil {
		return op, err
	}
	if err = op.Validate(); err != nil {
		return op, err
	}
	return op, nil
}
func (o Operation) Validate() error {
	var expected Operation
	switch o.Action {
	case "migrate":
		expected = Operation{Action: o.Action}
	case "bind":
		if !operatorID(o.AccountID) || !operatorID(o.CustomerID) {
			return ErrMismatch
		}
		expected = Operation{Action: o.Action, AccountID: o.AccountID, CustomerID: o.CustomerID}
	case "prepare":
		if !operatorID(o.AccountID) || !operatorID(o.Key) || len(o.ReceiptIDs) < 1 || len(o.ReceiptIDs) > 1000 || o.DaysUntilDue < 1 || o.DaysUntilDue > 365 {
			return ErrMismatch
		}
		seen := map[string]bool{}
		for _, id := range o.ReceiptIDs {
			if !operatorID(id) || seen[id] {
				return ErrMismatch
			}
			seen[id] = true
		}
		expected = Operation{Action: o.Action, AccountID: o.AccountID, Key: o.Key, ReceiptIDs: o.ReceiptIDs, DaysUntilDue: o.DaysUntilDue}
	case "inspect", "deliver":
		if !operatorID(o.DraftID) {
			return ErrMismatch
		}
		expected = Operation{Action: o.Action, DraftID: o.DraftID}
	case "reconcile-event":
		if !operatorID(o.EventID) {
			return ErrMismatch
		}
		expected = Operation{Action: o.Action, EventID: o.EventID}
	default:
		return ErrMismatch
	}
	actual, _ := json.Marshal(o)
	want, _ := json.Marshal(expected)
	if !bytes.Equal(actual, want) {
		return ErrMismatch
	}
	return nil
}
func operatorID(s string) bool {
	return len(s) > 0 && len(s) <= 128 && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// All operator documents are flat objects (the only array contains receipt
// strings). Reject duplicate fields instead of letting JSON's last value win.
func decodeOperatorJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return ErrMismatch
	}
	seen := map[string]bool{}
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return ErrMismatch
		}
		key, ok := k.(string)
		if !ok || seen[key] {
			return ErrMismatch
		}
		seen[key] = true
		var value json.RawMessage
		if e = d.Decode(&value); e != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrMismatch
		}
	}
	if _, err = d.Token(); err != nil {
		return ErrMismatch
	}
	var trailing any
	if !errors.Is(d.Decode(&trailing), io.EOF) {
		return ErrMismatch
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return ErrMismatch
	}
	return nil
}

// OpenOperator uses a dedicated role per operation; the inference connection
// never receives these grants. It does not migrate or run any operation itself.
func OpenOperator(ctx context.Context, databaseURL string, op Operation) (*Store, func(), error) {
	if err := op.Validate(); err != nil {
		return nil, nil, err
	}
	cfg, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, nil, ErrMismatch
	}
	return openOperatorConfig(ctx, cfg, op.Action)
}
func operatorRole(action string) string {
	switch action {
	case "migrate":
		return "kaana_billing_migrator"
	case "bind", "prepare":
		return "kaana_peable_billing_admin"
	default:
		return "kaana_peable_billing_runtime"
	}
}
func openOperatorConfig(ctx context.Context, cfg *pgxpool.Config, action string) (*Store, func(), error) {
	role := operatorRole(action)
	cfg = cfg.Copy()
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, e := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return e
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, ErrMismatch
	}
	if err = verifyOperator(ctx, pool, role); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return NewStore(pool), pool.Close, nil
}
func verifyOperator(ctx context.Context, pool *pgxpool.Pool, role string) error {
	var valid bool
	err := pool.QueryRow(ctx, `SELECT current_user=$1
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN(current_user,session_user) AND(rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls))
 AND NOT (SELECT rolcanlogin FROM pg_roles WHERE rolname=current_user)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member IN(current_user::regrole,session_user::regrole) AND admin_option)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=current_user::regrole)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=session_user::regrole AND roleid<>$1::regrole)
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN('kaana_migrator','kaana_admission_admin','kaana_billing_admin','kaana_billing_migrator','kaana_peable_billing_admin') AND rolname<>$1 AND pg_has_role(session_user,oid,'MEMBER'))`, role).Scan(&valid)
	if err != nil || !valid {
		return ErrMismatch
	}
	if role == "kaana_billing_migrator" {
		err = pool.QueryRow(ctx, `SELECT n.nspowner=current_user::regrole AND has_schema_privilege(current_user,'public','CREATE') AND has_database_privilege(current_user,current_database(),'CREATE') FROM pg_namespace n WHERE n.nspname='kaana_billing'`).Scan(&valid)
	} else {
		err = pool.QueryRow(ctx, `SELECT has_schema_privilege(current_user,'kaana_peable_billing','USAGE')
 AND NOT has_schema_privilege(current_user,'kaana_peable_billing','CREATE')
 AND NOT has_table_privilege(current_user,'kaana_peable_billing.drafts','INSERT,UPDATE,DELETE')
 AND NOT has_table_privilege(session_user,'kaana_peable_billing.drafts','INSERT,UPDATE,DELETE')
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='kaana_billing' AND ((c.relname='accounts' AND (has_table_privilege(current_user,c.oid,'UPDATE') OR has_table_privilege(session_user,c.oid,'UPDATE')))
 OR (c.relname='journal' AND (has_table_privilege(current_user,c.oid,'INSERT') OR has_table_privilege(session_user,c.oid,'INSERT')))))
 AND NOT has_schema_privilege(current_user,'kaana_billing','CREATE')
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='kaana_billing' AND p.proname IN('fund','refund','configure_account')
 AND (has_function_privilege(current_user,p.oid,'EXECUTE') OR has_function_privilege(session_user,p.oid,'EXECUTE')))
 AND has_function_privilege(current_user,'kaana_peable_billing.bind_customer(text,text,text)','EXECUTE')=$1
 AND has_function_privilege(current_user,'kaana_peable_billing.prepare_draft(text,text,text[],integer)','EXECUTE')=$1
 AND has_function_privilege(current_user,'kaana_peable_billing.read_draft(text)','EXECUTE')= (NOT $1)
 AND has_function_privilege(current_user,'kaana_peable_billing.claim_step(text,text)','EXECUTE')= (NOT $1)
 AND has_function_privilege(current_user,'kaana_peable_billing.record_step(text,text,text)','EXECUTE')= (NOT $1)`, role == "kaana_peable_billing_admin").Scan(&valid)
	}
	if err != nil || !valid {
		return fmt.Errorf("operator schema or least-privilege role not ready: %w", ErrMismatch)
	}
	return nil
}

// PrepareForMerchant checks the reviewed seller in the SAME transaction as
// receipt allocation. A stale/foreign customer binding leaves no draft behind.
func (s *Store) PrepareForMerchant(ctx context.Context, merchant, account, key string, receipts []string, days int) (Draft, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Draft{}, ErrMismatch
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	var d Draft
	if err = tx.QueryRow(ctx, `SELECT kaana_peable_billing.prepare_draft($1,$2,$3,$4)`, account, key, receipts, days).Scan(&raw); err != nil {
		return d, err
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return d, ErrMismatch
	}
	if d.MerchantID != merchant {
		return d, ErrMismatch
	}
	return d, tx.Commit(ctx)
}
