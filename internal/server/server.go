// Package server exposes the ledger over gRPC, gRPC-Web and HTTP/JSON
// (Connect protocol), all from one handler.
package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/gen/ledger/v1/ledgerv1connect"
	"github.com/learnercys/ledger/internal/auth"
	"github.com/learnercys/ledger/internal/ledger"
)

// Store is the ledger persistence used by the server.
type Store interface {
	CreateAccount(ctx context.Context, tenant string, req *ledgerv1.CreateAccountRequest) (*ledgerv1.Account, error)
	GetAccount(ctx context.Context, tenant, id string) (*ledgerv1.Account, error)
	ListAccounts(ctx context.Context, tenant string, pageSize int32, after string) ([]*ledgerv1.Account, string, error)
	PostTransaction(ctx context.Context, tenant string, req *ledgerv1.PostTransactionRequest) (*ledgerv1.Transaction, error)
	GetTransaction(ctx context.Context, tenant, id string) (*ledgerv1.Transaction, error)
	ReverseTransaction(ctx context.Context, tenant string, req *ledgerv1.ReverseTransactionRequest) (*ledgerv1.Transaction, error)
	ListEntries(ctx context.Context, tenant, accountID string, pageSize int32, pageToken string) ([]*ledgerv1.Entry, string, error)
}

// RequiredScopes lists the scope each procedure needs.
var RequiredScopes = map[string]string{
	ledgerv1connect.LedgerServiceCreateAccountProcedure:      auth.ScopeWrite,
	ledgerv1connect.LedgerServicePostTransactionProcedure:    auth.ScopeWrite,
	ledgerv1connect.LedgerServiceReverseTransactionProcedure: auth.ScopeWrite,
	ledgerv1connect.LedgerServiceGetAccountProcedure:         auth.ScopeRead,
	ledgerv1connect.LedgerServiceListAccountsProcedure:       auth.ScopeRead,
	ledgerv1connect.LedgerServiceGetTransactionProcedure:     auth.ScopeRead,
	ledgerv1connect.LedgerServiceListEntriesProcedure:        auth.ScopeRead,
}

// Server implements ledgerv1connect.LedgerServiceHandler.
type Server struct {
	Store Store
	Log   *slog.Logger
}

var _ ledgerv1connect.LedgerServiceHandler = (*Server)(nil)

func tenant(ctx context.Context) string {
	p, _ := auth.FromContext(ctx)
	return p.Tenant
}

func (s *Server) CreateAccount(ctx context.Context, req *ledgerv1.CreateAccountRequest) (*ledgerv1.CreateAccountResponse, error) {
	a, err := s.Store.CreateAccount(ctx, tenant(ctx), req)
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.CreateAccountResponse{Account: a}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	a, err := s.Store.GetAccount(ctx, tenant(ctx), req.GetId())
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.GetAccountResponse{Account: a}, nil
}

func (s *Server) ListAccounts(ctx context.Context, req *ledgerv1.ListAccountsRequest) (*ledgerv1.ListAccountsResponse, error) {
	accounts, next, err := s.Store.ListAccounts(ctx, tenant(ctx), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.ListAccountsResponse{Accounts: accounts, NextPageToken: next}, nil
}

func (s *Server) PostTransaction(ctx context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	t, err := s.Store.PostTransaction(ctx, tenant(ctx), req)
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.PostTransactionResponse{Transaction: t}, nil
}

func (s *Server) GetTransaction(ctx context.Context, req *ledgerv1.GetTransactionRequest) (*ledgerv1.GetTransactionResponse, error) {
	t, err := s.Store.GetTransaction(ctx, tenant(ctx), req.GetId())
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.GetTransactionResponse{Transaction: t}, nil
}

func (s *Server) ReverseTransaction(ctx context.Context, req *ledgerv1.ReverseTransactionRequest) (*ledgerv1.ReverseTransactionResponse, error) {
	t, err := s.Store.ReverseTransaction(ctx, tenant(ctx), req)
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.ReverseTransactionResponse{Transaction: t}, nil
}

func (s *Server) ListEntries(ctx context.Context, req *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	entries, next, err := s.Store.ListEntries(ctx, tenant(ctx), req.GetAccountId(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, s.toConnect(ctx, err)
	}
	return &ledgerv1.ListEntriesResponse{Entries: entries, NextPageToken: next}, nil
}

// toConnect maps ledger errors to status codes. Anything unexpected becomes
// Internal with a generic message; details go to the log, not the caller.
func (s *Server) toConnect(ctx context.Context, err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, ledger.ErrInvalid):
		code = connect.CodeInvalidArgument
	case errors.Is(err, ledger.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, ledger.ErrInsufficientFunds),
		errors.Is(err, ledger.ErrAlreadyReversed),
		errors.Is(err, ledger.ErrNotReversible):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, ledger.ErrIdempotencyMismatch):
		code = connect.CodeAlreadyExists
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	}
	if code == connect.CodeInternal {
		s.Log.ErrorContext(ctx, "internal error", "tenant", tenant(ctx), "err", err)
		return connect.NewError(code, errors.New("internal error"))
	}
	return connect.NewError(code, err)
}

// LoggingInterceptor logs one line per call with its outcome and latency.
func LoggingInterceptor(log *slog.Logger) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			start := time.Now()
			resp, err := next(ctx, req)
			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			log.InfoContext(ctx, "rpc", "procedure", req.Spec().Procedure, "tenant", tenant(ctx),
				"code", code, "duration_ms", float64(time.Since(start).Microseconds())/1000)
			return resp, err
		}
	}
}
