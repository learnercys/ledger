// Package ledger holds the double-entry rules that don't need a database.
package ledger

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// MaxAmount bounds a single posting (10 trillion minor units) so that sums of
// many postings can't overflow int64.
const MaxAmount = 10_000_000_000_000

// Errors returned by the ledger. The API layer maps them to status codes.
var (
	ErrInvalid             = errors.New("invalid request")
	ErrNotFound            = errors.New("not found")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrIdempotencyMismatch = errors.New("idempotency key already used for a different request")
	ErrAlreadyReversed     = errors.New("transaction already reversed")
	ErrNotReversible       = errors.New("transaction cannot be reversed")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// PostingInput is one leg of a transaction as requested by the caller.
type PostingInput struct {
	AccountID string
	Amount    int64
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidateCurrency checks for a three-letter ISO 4217 code.
func ValidateCurrency(c string) error {
	if !currencyRe.MatchString(c) {
		return invalid("currency must be a 3-letter ISO code, got %q", c)
	}
	return nil
}

// ValidateName checks an account name.
func ValidateName(name string) error {
	if n := len(strings.TrimSpace(name)); n == 0 || len(name) > 200 {
		return invalid("name must be 1-200 characters")
	}
	return nil
}

// ValidateIdempotencyKey checks that a key is present and reasonably sized.
func ValidateIdempotencyKey(key string) error {
	if key == "" || len(key) > 255 {
		return invalid("idempotency_key is required (1-255 characters)")
	}
	return nil
}

// ValidatePostings checks the shape of a transaction before touching the
// database: at least two legs, no zero or oversized amounts, each account at
// most once. Whether the legs balance per currency needs the accounts'
// currencies, so that check is CheckBalanced.
func ValidatePostings(ps []PostingInput) error {
	if len(ps) < 2 {
		return invalid("a transaction needs at least 2 postings")
	}
	if len(ps) > 100 {
		return invalid("a transaction can have at most 100 postings")
	}
	seen := make(map[string]bool, len(ps))
	for i, p := range ps {
		switch {
		case p.AccountID == "":
			return invalid("posting %d: account_id is required", i)
		case p.Amount == 0:
			return invalid("posting %d: amount must not be zero", i)
		case p.Amount > MaxAmount || p.Amount < -MaxAmount:
			return invalid("posting %d: amount exceeds %d", i, int64(MaxAmount))
		case seen[p.AccountID]:
			return invalid("posting %d: account %s appears more than once", i, p.AccountID)
		}
		seen[p.AccountID] = true
	}
	return nil
}

// CheckBalanced verifies that the postings sum to zero in every currency.
// currencyOf maps account ID to currency.
func CheckBalanced(ps []PostingInput, currencyOf map[string]string) error {
	sums := map[string]int64{}
	for _, p := range ps {
		sums[currencyOf[p.AccountID]] += p.Amount
	}
	for cur, sum := range sums {
		if sum != 0 {
			return invalid("postings in %s sum to %d, must sum to 0", cur, sum)
		}
	}
	return nil
}

// Reverse returns postings that undo the given ones.
func Reverse(ps []PostingInput) []PostingInput {
	out := make([]PostingInput, len(ps))
	for i, p := range ps {
		out[i] = PostingInput{AccountID: p.AccountID, Amount: -p.Amount}
	}
	return out
}
