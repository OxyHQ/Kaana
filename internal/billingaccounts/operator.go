package billingaccounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config binds the reviewed account scope, not caller-selected payer attribution.
// Digest is computed from the exact file, never accepted inside the document.
type Config struct {
	Enabled        bool   `json:"enabled"`
	OwnerAccountID string `json:"ownerAccountId"`
	Currency       string `json:"currency"`
	Digest         string `json:"-"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func (c Config) Validate() error {
	if !c.Enabled || !bounded(c.OwnerAccountID) || !currencyPattern.MatchString(c.Currency) || !hashPattern.MatchString(c.Digest) {
		return ErrReview
	}
	return nil
}
func ReadConfig(path, digest string) (Config, error) {
	var c Config
	if !hashPattern.MatchString(digest) {
		return c, ErrReview
	}
	f, err := os.Open(path)
	if err != nil {
		return c, ErrReview
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
		return c, ErrReview
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	sum := sha256.Sum256(raw)
	if err != nil || len(raw) > 65536 || hex.EncodeToString(sum[:]) != digest {
		return c, ErrReview
	}
	if err = decode(raw, &c); err != nil {
		return c, err
	}
	c.Digest = digest
	return c, c.Validate()
}

type Operation struct {
	Action      string `json:"action"`
	AccountID   string `json:"accountId,omitempty"`
	Key         string `json:"key,omitempty"`
	EvidenceID  string `json:"evidenceId,omitempty"`
	Revision    string `json:"revision,omitempty"`
	BudgetKey   string `json:"budgetKey,omitempty"`
	StartsAt    string `json:"startsAt,omitempty"`
	EndsAt      string `json:"endsAt,omitempty"`
	Amount      string `json:"amount,omitempty"`
	CreditLimit string `json:"creditLimit,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

func ReadOperation(input io.Reader) (Operation, error) {
	var o Operation
	raw, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil || len(raw) > 65536 {
		return o, ErrReview
	}
	if err = decode(raw, &o); err != nil {
		return o, err
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return o, ErrReview
	}
	required := map[string][]string{
		"migrate":   {"action"},
		"inspect":   {"action", "accountId"},
		"provision": {"action", "key", "evidenceId"},
		"configure": {"action", "accountId", "key", "evidenceId", "revision", "budgetKey", "startsAt", "endsAt", "amount", "creditLimit", "enabled"},
	}[o.Action]
	if len(required) == 0 || len(fields) != len(required) {
		return o, ErrReview
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return o, ErrReview
		}
	}
	return o, o.Validate()
}
func (o Operation) Validate() error {
	var expected Operation
	switch o.Action {
	case "migrate":
		expected = Operation{Action: o.Action}
	case "inspect":
		if !bounded(o.AccountID) {
			return ErrReview
		}
		expected = Operation{Action: o.Action, AccountID: o.AccountID}
	case "provision":
		if !bounded(o.Key) || !bounded(o.EvidenceID) {
			return ErrReview
		}
		expected = Operation{Action: o.Action, Key: o.Key, EvidenceID: o.EvidenceID}
	case "configure":
		if !bounded(o.AccountID) || !bounded(o.Key) || !bounded(o.EvidenceID) || !bounded(o.BudgetKey) || !hashPattern.MatchString(o.Revision) || o.Enabled == nil || !bounded(o.Amount) || !bounded(o.CreditLimit) {
			return ErrReview
		}
		// Monetary grammar/scale/overflow is enforced by the existing exact-decimal
		// billing.amount function in the atomic transaction, not a second price engine.
		const wireTime = "2006-01-02T15:04:05.000Z"
		start, e := time.Parse(wireTime, o.StartsAt)
		end, e2 := time.Parse(wireTime, o.EndsAt)
		if e != nil || e2 != nil || start.Format(wireTime) != o.StartsAt || end.Format(wireTime) != o.EndsAt || !end.After(start) {
			return ErrReview
		}
		expected = o
	default:
		return ErrReview
	}
	if !reflect.DeepEqual(o, expected) {
		return ErrReview
	}
	return nil
}
func bounded(v string) bool {
	return len(v) > 0 && len(v) <= 256 && strings.TrimSpace(v) == v && !strings.ContainsFunc(v, unicode.IsControl)
}

// Flat private operator documents: reject case variants, duplicate keys, null,
// unknown fields and trailing values before any database connection.
func decode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return ErrReview
	}
	typ := reflect.TypeOf(out).Elem()
	known := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "-" {
			known[name] = true
		}
	}
	seen := map[string]bool{}
	for d.More() {
		k, e := d.Token()
		key, ok := k.(string)
		if e != nil || !ok || !known[key] || seen[key] {
			return ErrReview
		}
		seen[key] = true
		var v json.RawMessage
		if e = d.Decode(&v); e != nil || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ErrReview
		}
	}
	if _, err = d.Token(); err != nil {
		return ErrReview
	}
	var trailing any
	if !errors.Is(d.Decode(&trailing), io.EOF) {
		return ErrReview
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrReview
	}
	return nil
}

func OpenOperator(ctx context.Context, url string, op Operation) (*Store, func(), error) {
	if err := op.Validate(); err != nil {
		return nil, nil, err
	}
	cfg, err := credentialstore.VerifiedPostgresConfig(url)
	if err != nil {
		return nil, nil, ErrReview
	}
	return openConfig(ctx, cfg, op.Action)
}
func openConfig(ctx context.Context, cfg *pgxpool.Config, action string) (*Store, func(), error) {
	role := "kaana_billing_account_admin"
	if action == "migrate" {
		role = "kaana_billing_migrator"
	}
	cfg = cfg.Copy()
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, ErrReview
	}
	if err = verifyRole(ctx, p, role); err != nil {
		p.Close()
		return nil, nil, err
	}
	tx, err := p.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		p.Close()
		return nil, nil, ErrReview
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = billing.VerifyMigration(ctx, tx)
	if err == nil && action != "migrate" {
		var count int
		var actual string
		err = tx.QueryRow(ctx, `SELECT count(*),coalesce(max(checksum) FILTER(WHERE version='0001'),'') FROM public.kaana_billing_account_migrations`).Scan(&count, &actual)
		sum := sha256.Sum256([]byte(migration))
		if err == nil && (count != 1 || actual != hex.EncodeToString(sum[:])) {
			err = ErrReview
		}
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		p.Close()
		return nil, nil, ErrReview
	}
	if err = tx.Commit(ctx); err != nil {
		p.Close()
		return nil, nil, ErrReview
	}
	return NewStore(p), p.Close, nil
}
func verifyRole(ctx context.Context, p *pgxpool.Pool, role string) error {
	var valid bool
	err := p.QueryRow(ctx, `SELECT current_user=$1
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN(current_user,session_user) AND(rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls))
 AND NOT (SELECT rolcanlogin FROM pg_roles WHERE rolname=current_user)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member IN(current_user::regrole,session_user::regrole) AND admin_option)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=current_user::regrole)
 AND NOT EXISTS(SELECT 1 FROM pg_auth_members WHERE member=session_user::regrole AND(roleid<>$1::regrole OR inherit_option OR NOT set_option))`, role).Scan(&valid)
	if err != nil || !valid {
		return ErrReview
	}
	if role == "kaana_billing_migrator" {
		err = p.QueryRow(ctx, `SELECT nspowner=current_user::regrole AND has_database_privilege(current_user,current_database(),'CREATE') AND has_schema_privilege(current_user,'public','CREATE') FROM pg_namespace WHERE nspname='kaana_billing'`).Scan(&valid)
	} else {
		allowed := []string{"kaana_billing_accounts.inspect(text,text,text)", "kaana_billing_accounts.provision(text,text,text,text,text)", "kaana_billing_accounts.configure(text,text,text,text,text,text,text,text,timestamp with time zone,timestamp with time zone,text,text,boolean)"}
		err = p.QueryRow(ctx, `SELECT has_schema_privilege(current_user,'kaana_billing_accounts','USAGE')
 AND NOT EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname IN('kaana_billing','kaana_billing_accounts','kaana_peable_billing','kaana_commercial','kaana_admission') AND(has_schema_privilege(current_user,n.oid,'CREATE') OR has_schema_privilege(session_user,n.oid,'CREATE')))
 AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE (n.nspname IN('kaana_billing','kaana_billing_accounts','kaana_peable_billing','kaana_commercial','kaana_admission') OR c.oid IN(to_regclass('public.kaana_billing_migrations'),to_regclass('public.kaana_billing_account_migrations'))) AND c.relkind IN('r','p','v','m','f') AND(has_table_privilege(current_user,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_table_privilege(session_user,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')))
 AND NOT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname IN('kaana_billing','kaana_billing_accounts','kaana_peable_billing','kaana_commercial','kaana_admission') AND(has_function_privilege(current_user,p.oid,'EXECUTE') OR has_function_privilege(session_user,p.oid,'EXECUTE')) AND p.oid<>ALL(SELECT to_regprocedure(f)::oid FROM unnest($1::text[]) f))
 AND NOT EXISTS(SELECT 1 FROM unnest($1::text[]) f WHERE to_regprocedure(f) IS NULL OR NOT has_function_privilege(current_user,to_regprocedure(f),'EXECUTE'))`, allowed).Scan(&valid)
	}
	if err != nil || !valid {
		return ErrReview
	}
	return nil
}
