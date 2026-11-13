// Command bench measures PostTransaction throughput against Postgres in two
// shapes of traffic:
//
//	spread: transfers between random pairs of many accounts
//	hot:    every transfer is funded by one account, so all of them queue on
//	        that account's row lock
//
// The gap between the two is the classic ledger scaling problem; see the
// README for how real ledgers deal with it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	ledgerv1 "github.com/learnercys/ledger/gen/ledger/v1"
	"github.com/learnercys/ledger/internal/store"
)

func main() {
	var (
		dbURL    = flag.String("db", "postgres://ledger_app:ledger_app@localhost:55433/ledger?sslmode=disable", "Postgres URL")
		workers  = flag.Int("concurrency", 32, "concurrent writers")
		duration = flag.Duration("duration", 10*time.Second, "how long to run each scenario")
		accounts = flag.Int("accounts", 1000, "accounts in the spread scenario")
	)
	flag.Parse()
	ctx := context.Background()
	s, err := store.New(ctx, *dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	tenant := fmt.Sprintf("bench_%d", time.Now().Unix())
	funding := open(ctx, s, tenant, true)
	ids := make([]string, *accounts)
	for i := range ids {
		ids[i] = open(ctx, s, tenant, true)
	}

	fmt.Printf("%-8s %12s %10s %10s %10s\n", "traffic", "txn/s", "p50", "p95", "p99")
	run("spread", *workers, *duration, func(r *rand.Rand) []*ledgerv1.Posting {
		a, b := r.IntN(len(ids)), r.IntN(len(ids)-1)
		if b >= a {
			b++
		}
		amt := 1 + r.Int64N(10_000)
		return []*ledgerv1.Posting{{AccountId: ids[a], Amount: -amt}, {AccountId: ids[b], Amount: amt}}
	}, s, tenant)
	run("hot", *workers, *duration, func(r *rand.Rand) []*ledgerv1.Posting {
		amt := 1 + r.Int64N(10_000)
		return []*ledgerv1.Posting{{AccountId: funding, Amount: -amt}, {AccountId: ids[r.IntN(len(ids))], Amount: amt}}
	}, s, tenant)
}

func open(ctx context.Context, s *store.Store, tenant string, allowNegative bool) string {
	a, err := s.CreateAccount(ctx, tenant, &ledgerv1.CreateAccountRequest{
		IdempotencyKey: fmt.Sprintf("open-%d", rand.Int64()), Name: "bench", Currency: "USD", AllowNegative: allowNegative})
	if err != nil {
		log.Fatal(err)
	}
	return a.GetId()
}

func run(name string, workers int, d time.Duration, gen func(*rand.Rand) []*ledgerv1.Posting,
	s *store.Store, tenant string) {
	var (
		mu    sync.Mutex
		lat   []time.Duration
		count atomic.Int64
		seq   atomic.Int64
		wg    sync.WaitGroup
	)
	stop := time.Now().Add(d)
	for w := range workers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), uint64(time.Now().UnixNano())))
			var local []time.Duration
			for time.Now().Before(stop) {
				req := &ledgerv1.PostTransactionRequest{
					IdempotencyKey: fmt.Sprintf("%s-%d", name, seq.Add(1)),
					Postings:       gen(r),
				}
				t0 := time.Now()
				if _, err := s.PostTransaction(context.Background(), tenant, req); err != nil {
					log.Fatalf("%s: %v", name, err)
				}
				local = append(local, time.Since(t0))
				count.Add(1)
			}
			mu.Lock()
			lat = append(lat, local...)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(lat)
	p := func(q float64) time.Duration { return lat[int(float64(len(lat)-1)*q)].Round(10 * time.Microsecond) }
	fmt.Printf("%-8s %12.0f %10s %10s %10s\n", name, float64(count.Load())/d.Seconds(), p(.5), p(.95), p(.99))
}
