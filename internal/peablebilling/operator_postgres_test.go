package peablebilling

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func operatorLogin(t *testing.T, f *fixture, group string) (*pgxpool.Config, string) {
	t.Helper()
	login := "kpo_" + unique()
	password := unique()
	if _, err := f.ownerDB.Exec(f.ctx, "CREATE ROLE "+pgx.Identifier{login}.Sanitize()+" LOGIN NOINHERIT PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ownerDB.Exec(f.ctx, "GRANT "+pgx.Identifier{group}.Sanitize()+" TO "+pgx.Identifier{login}.Sanitize()+" WITH INHERIT FALSE, SET TRUE, ADMIN FALSE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.ownerDB.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{login}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	cfg := f.config.Copy()
	cfg.ConnConfig.User = login
	cfg.ConnConfig.Password = password
	cfg.AfterConnect = nil
	return cfg, login
}
func TestPostgresOperatorRuntimePermissions(t *testing.T) {
	f := database(t)
	a := f.account(t)
	r := f.receipt(t, a, "1.25")
	d, err := f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 30)
	if err != nil {
		t.Fatal(err)
	}
	cfg, login := operatorLogin(t, f, "kaana_peable_billing_runtime")
	s, closePool, err := openOperatorConfig(f.ctx, cfg, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(f.ctx, d.ID)
	if err != nil || got.ID != d.ID {
		t.Fatalf("read: %v", err)
	}
	if err = s.BindCustomer(f.ctx, a.ID, "foreign", "customer"); err == nil {
		t.Fatal("runtime bound customer")
	}
	closePool()
	for _, tc := range []struct{ name, grant, revoke string }{
		{"delegation", "GRANT kaana_peable_billing_runtime TO " + pgx.Identifier{login}.Sanitize() + " WITH ADMIN TRUE", "REVOKE ADMIN OPTION FOR kaana_peable_billing_runtime FROM " + pgx.Identifier{login}.Sanitize()},
		{"money admin", "GRANT kaana_billing_admin TO " + pgx.Identifier{login}.Sanitize(), "REVOKE kaana_billing_admin FROM " + pgx.Identifier{login}.Sanitize()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err = f.ownerDB.Exec(f.ctx, tc.grant); err != nil {
				t.Fatal(err)
			}
			s, closePool, err = openOperatorConfig(f.ctx, cfg, "inspect")
			if err == nil {
				closePool()
				t.Fatal("excessive authority accepted")
			}
			if _, err = f.ownerDB.Exec(f.ctx, tc.revoke); err != nil {
				t.Fatal(err)
			}
			_, closePool, err = openOperatorConfig(f.ctx, cfg, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			closePool()
		})
	}
	if _, err = f.ownerDB.Exec(f.ctx, "GRANT UPDATE ON kaana_peable_billing.drafts TO "+pgx.Identifier{login}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_, closePool, err = openOperatorConfig(f.ctx, cfg, "inspect")
	if err == nil {
		closePool()
		t.Fatal("direct login DML accepted")
	}
	if _, err = f.ownerDB.Exec(f.ctx, "REVOKE UPDATE ON kaana_peable_billing.drafts FROM "+pgx.Identifier{login}.Sanitize()); err != nil {
		t.Fatal(err)
	}
}
func TestPostgresOperatorPrepareAtomicMerchant(t *testing.T) {
	f := database(t)
	a := f.account(t)
	r := f.receipt(t, a, "1.25")
	cfg, _ := operatorLogin(t, f, "kaana_peable_billing_admin")
	s, closePool, err := openOperatorConfig(f.ctx, cfg, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()
	if _, err = s.PrepareForMerchant(f.ctx, "foreign", a.ID, "wrong", []string{r.ID}, 30); err == nil {
		t.Fatal("foreign seller prepared")
	}
	var n int
	if err = f.ownerDB.QueryRow(f.ctx, `SELECT count(*) FROM kaana_peable_billing.draft_receipts WHERE receipt_id=$1`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed preparation retained allocation %d %v", n, err)
	}
	d, err := s.PrepareForMerchant(f.ctx, "merch_kaana", a.ID, "correct", []string{r.ID}, 30)
	if err != nil || d.ID == "" {
		t.Fatal(err)
	}
	if _, err = s.Read(f.ctx, d.ID); err == nil {
		t.Fatal("admin acquired delivery permission")
	}
}
func TestPostgresOperatorMigrationRole(t *testing.T) {
	f := database(t)
	cfg, _ := operatorLogin(t, f, "kaana_billing_migrator")
	s, closePool, err := openOperatorConfig(f.ctx, cfg, "migrate")
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()
	if err = s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = openOperatorConfig(f.ctx, cfg, "deliver"); err == nil {
		t.Fatal("migrator accepted for delivery")
	}
}
