package ledger

import (
	"errors"
	"testing"

	"pgregory.net/rapid"
)

func TestValidatePostings(t *testing.T) {
	ok := []PostingInput{{"a", -100}, {"b", 100}}
	if err := ValidatePostings(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]PostingInput{
		"single leg":  {{"a", 100}},
		"zero amount": {{"a", 0}, {"b", 0}},
		"missing id":  {{"", -1}, {"b", 1}},
		"duplicate":   {{"a", -1}, {"a", 1}},
		"too large":   {{"a", -MaxAmount - 1}, {"b", MaxAmount + 1}},
	}
	for name, ps := range bad {
		if err := ValidatePostings(ps); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestCheckBalanced(t *testing.T) {
	cur := map[string]string{"usd1": "USD", "usd2": "USD", "eur1": "EUR", "eur2": "EUR"}
	if err := CheckBalanced([]PostingInput{{"usd1", -5}, {"usd2", 5}, {"eur1", 3}, {"eur2", -3}}, cur); err != nil {
		t.Fatalf("balanced multi-currency transaction rejected: %v", err)
	}
	// Balanced in total, but not per currency: 5 USD can't pay for 5 EUR.
	if err := CheckBalanced([]PostingInput{{"usd1", -5}, {"eur1", 5}}, cur); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-currency transaction accepted: %v", err)
	}
}

func TestValidateCurrency(t *testing.T) {
	for _, c := range []string{"USD", "EUR", "GTQ"} {
		if err := ValidateCurrency(c); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for _, c := range []string{"", "usd", "US", "USDT"} {
		if err := ValidateCurrency(c); err == nil {
			t.Errorf("%q should be rejected", c)
		}
	}
}

// Property: a transaction followed by its reversal leaves every account
// exactly where it started.
func TestReverseUndoes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 10).Draw(t, "legs")
		ps := make([]PostingInput, n)
		for i := range ps {
			ps[i] = PostingInput{
				AccountID: string(rune('a' + i)),
				Amount:    rapid.Int64Range(-MaxAmount, MaxAmount).Filter(func(v int64) bool { return v != 0 }).Draw(t, "amount"),
			}
		}
		net := map[string]int64{}
		for _, p := range append(ps, Reverse(ps)...) {
			net[p.AccountID] += p.Amount
		}
		for acct, v := range net {
			if v != 0 {
				t.Fatalf("account %s off by %d after reversal", acct, v)
			}
		}
	})
}
