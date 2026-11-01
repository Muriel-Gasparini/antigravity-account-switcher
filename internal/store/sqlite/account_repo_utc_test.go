package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/Muriel-Gasparini/antigravity-account-switcher/internal/domain"
)

// An offset timestamp must sort by instant, not by its local wall-clock text.
// 12:00 IST is 06:30Z, which is earlier than 08:00Z even though "12:00" sorts
// after "08:00" as plain text.
func TestAccountRepository_OrderingIgnoresTimezoneOffset(t *testing.T) {
	ist := time.FixedZone("IST", 19800)
	earlier := time.Date(2026, 1, 1, 12, 0, 0, 0, ist) // 06:30Z
	later := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)

	t.Run("List orders by created_at", func(t *testing.T) {
		_, repo, _, _, _ := setupTestStore(t)
		ctx := context.Background()

		for _, acc := range []*domain.Account{
			{ID: "late", Email: "late@example.com", CreatedAt: later, UpdatedAt: later},
			{ID: "early", Email: "early@example.com", CreatedAt: earlier, UpdatedAt: earlier},
		} {
			if err := repo.Create(ctx, acc); err != nil {
				t.Fatalf("Create %s: %v", acc.ID, err)
			}
		}

		got, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 || got[0].ID != "early" || got[1].ID != "late" {
			t.Fatalf("List order = %v, want [early late]", ids(got))
		}
	})

	t.Run("GetNextAvailable picks least recently updated", func(t *testing.T) {
		_, repo, _, _, _ := setupTestStore(t)
		ctx := context.Background()

		for _, acc := range []*domain.Account{
			{ID: "late", Email: "late@example.com", CreatedAt: later, UpdatedAt: later},
			{ID: "early", Email: "early@example.com", CreatedAt: earlier, UpdatedAt: earlier},
		} {
			if err := repo.Create(ctx, acc); err != nil {
				t.Fatalf("Create %s: %v", acc.ID, err)
			}
		}

		got, err := repo.GetNextAvailable(ctx, "")
		if err != nil {
			t.Fatalf("GetNextAvailable: %v", err)
		}
		if got.ID != "early" {
			t.Fatalf("GetNextAvailable = %q, want %q", got.ID, "early")
		}
	})
}

// Every timestamp column is written as UTC text, whatever zone the caller used.
func TestAccountRepository_StoresTimestampsAsUTC(t *testing.T) {
	db, repo, _, _, _ := setupTestStore(t)
	ctx := context.Background()
	ist := time.FixedZone("IST", 19800)
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, ist)

	acc := &domain.Account{
		ID: "acc", Email: "acc@example.com",
		TokenExpiry: ts, CreatedAt: ts, UpdatedAt: ts,
	}
	if err := repo.Create(ctx, acc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const want = "2026-01-01T06:30:00Z"
	raw := func(col string) string {
		t.Helper()
		var s string
		if err := db.QueryRowContext(ctx, "SELECT "+col+" FROM accounts WHERE id = 'acc'").Scan(&s); err != nil {
			t.Fatalf("select %s: %v", col, err)
		}
		return s
	}
	for _, col := range []string{"token_expiry", "created_at", "updated_at"} {
		if got := raw(col); got != want {
			t.Errorf("after Create, %s = %q, want %q", col, got, want)
		}
	}

	if err := repo.UpdateToken(ctx, "acc", "at", ts.Add(time.Hour)); err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if got, want := raw("token_expiry"), "2026-01-01T07:30:00Z"; got != want {
		t.Errorf("after UpdateToken, token_expiry = %q, want %q", got, want)
	}
}

func ids(accs []*domain.Account) []string {
	out := make([]string, len(accs))
	for i, a := range accs {
		out[i] = a.ID
	}
	return out
}
