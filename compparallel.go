package routinghelpers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
)

var _ routing.Routing = &Parallel{}

type ComposableParallel struct {
	routers []*ParallelRouter
}

// NewComposableParallel creates a Router that will execute methods from provided Routers in parallel.
// On all methods, If IgnoreError flag is set, that Router will not stop the entire execution.
// On all methods, If ExecuteAfter is set, that Router will be executed after the timer.
// Router specific timeout will start counting AFTER the ExecuteAfter timer.
func NewComposableParallel(routers []*ParallelRouter) *ComposableParallel {
	return &ComposableParallel{
		routers: routers,
	}
}

// Provide will call all Routers in parallel.
func (r *ComposableParallel) Provide(ctx context.Context, cid cid.Cid, provide bool) error {
	return execute(ctx, r.routers,
		func(ctx context.Context, r routing.Routing) error {
			return r.Provide(ctx, cid, provide)
		},
	)
}

// FindProvidersAsync will execute all Routers in parallel, iterating results from them in unspecified oredr.
// If count is set, only that amount of elements will be returned without any specification about from what router is obtained.
// To gather providers from a set of Routers first, you can use the ExecuteAfter timer to delay some Router execution.
func (r *ComposableParallel) FindProvidersAsync(ctx context.Context, cid cid.Cid, count int) <-chan peer.AddrInfo {
	addrChanOut := make(chan peer.AddrInfo)
	var totalCount int64
	var wg sync.WaitGroup
	for _, r := range r.routers {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			tim := time.NewTimer(r.ExecuteAfter)
			defer tim.Stop()
			select {
			case <-ctx.Done():
				return
			case <-tim.C:
				ctx, cancel := context.WithTimeout(ctx, r.Timeout)
				defer cancel()
				addrChan := r.Router.FindProvidersAsync(ctx, cid, count)
				for {
					select {
					case <-ctx.Done():
						return
					case addr, ok := <-addrChan:
						if !ok {
							return
						}

						if atomic.AddInt64(&totalCount, 1) > int64(count) && count != 0 {
							return
						}

						select {
						case <-ctx.Done():
							return
						case addrChanOut <- addr:
						}

					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(addrChanOut)
	}()

	return addrChanOut
}

// FindPeer will execute all Routers in parallel, getting the first AddrInfo found and cancelling all other Router calls.
func (r *ComposableParallel) FindPeer(ctx context.Context, id peer.ID) (peer.AddrInfo, error) {
	return getValueOrError(ctx, r.routers,
		func(ctx context.Context, r routing.Routing) (peer.AddrInfo, error) {
			return r.FindPeer(ctx, id)
		},
		func(ai peer.AddrInfo) bool {
			return ai.ID == ""
		})
}

// PutValue will execute all Routers in parallel. If a Router fails and IgnoreError flag is not set, the whole execution will fail.
// Some Puts before the failure might be successful, even if we return an error.
func (r *ComposableParallel) PutValue(ctx context.Context, key string, val []byte, opts ...routing.Option) error {
	return execute(ctx, r.routers,
		func(ctx context.Context, r routing.Routing) error {
			return r.PutValue(ctx, key, val, opts...)
		},
	)
}

// GetValue will execute all Routers in parallel. The first value found will be returned, cancelling all other executions.
func (r *ComposableParallel) GetValue(ctx context.Context, key string, opts ...routing.Option) ([]byte, error) {
	return getValueOrError(ctx, r.routers,
		func(ctx context.Context, r routing.Routing) ([]byte, error) {
			return r.GetValue(ctx, key, opts...)
		},
		func(ai []byte) bool {
			return len(ai) == 0
		})
}

func (r *ComposableParallel) SearchValue(ctx context.Context, key string, opts ...routing.Option) (<-chan []byte, error) {
	outCh := make(chan []byte)
	errCh := make(chan error)
	var wg sync.WaitGroup
	for _, r := range r.routers {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			tim := time.NewTimer(r.ExecuteAfter)
			defer tim.Stop()
			select {
			case <-ctx.Done():
				return
			case <-tim.C:
				ctx, cancel := context.WithTimeout(ctx, r.Timeout)
				defer cancel()
				valueChan, err := r.Router.SearchValue(ctx, key, opts...)
				if err != nil && !r.IgnoreError {
					select {
					case <-ctx.Done():
					case errCh <- err:
					}
					return
				}
				for {
					select {
					case <-ctx.Done():
						return
					case val, ok := <-valueChan:
						if !ok {
							return
						}
						select {
						case <-ctx.Done():
							return
						case outCh <- val:
						}
					}
				}
			}
		}()
	}

	// goroutine closing everything when finishing execution
	go func() {
		wg.Wait()
		close(outCh)
		close(errCh)
	}()

	select {
	case err, ok := <-errCh:
		if !ok {
			return nil, routing.ErrNotFound
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return outCh, nil
	}
}

func (r *ComposableParallel) Bootstrap(ctx context.Context) error {
	return execute(ctx, r.routers,
		func(ctx context.Context, r routing.Routing) error {
			return r.Bootstrap(ctx)
		})
}

func getValueOrError[T any](
	ctx context.Context,
	routers []*ParallelRouter,
	f func(context.Context, routing.Routing) (T, error),
	isEmpty func(T) bool,
) (value T, err error) {
	outCh := make(chan T)
	errCh := make(chan error)

	// global cancel context to stop early other router's execution.
	ctx, gcancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, r := range routers {
		wg.Add(1)
		go func(r *ParallelRouter) {
			defer wg.Done()
			tim := time.NewTimer(r.ExecuteAfter)
			defer tim.Stop()
			select {
			case <-ctx.Done():
				if !r.IgnoreError {
					errCh <- ctx.Err()
				}
			case <-tim.C:
				ctx, cancel := context.WithTimeout(ctx, r.Timeout)
				defer cancel()
				value, err := f(ctx, r.Router)
				if err != nil &&
					!errors.Is(err, routing.ErrNotFound) &&
					!r.IgnoreError {
					select {
					case <-ctx.Done():
					case errCh <- err:
					}
					return
				}
				if isEmpty(value) {
					return
				}
				select {
				case <-ctx.Done():
					return
				case outCh <- value:
				}
			}
		}(r)
	}

	// goroutine closing everything when finishing execution
	go func() {
		wg.Wait()
		close(outCh)
		close(errCh)
	}()

	select {
	case out, ok := <-outCh:
		gcancel()
		if !ok {
			return value, routing.ErrNotFound
		}
		return out, nil
	case err, ok := <-errCh:
		gcancel()
		if !ok {
			return value, routing.ErrNotFound
		}
		return value, err
	case <-ctx.Done():
		gcancel()
		return value, ctx.Err()
	}
}

func execute(
	ctx context.Context,
	routers []*ParallelRouter,
	f func(context.Context, routing.Routing,
	) error) error {
	var wg sync.WaitGroup
	errCh := make(chan error)
	for _, r := range routers {
		wg.Add(1)
		go func(r *ParallelRouter) {
			defer wg.Done()
			tim := time.NewTimer(r.ExecuteAfter)
			defer tim.Stop()
			select {
			case <-ctx.Done():
				if !r.IgnoreError {
					errCh <- ctx.Err()
				}
			case <-tim.C:
				ctx, cancel := context.WithTimeout(ctx, r.Timeout)
				defer cancel()
				err := f(ctx, r.Router)
				if err != nil &&
					!r.IgnoreError {
					errCh <- err
				}
			}
		}(r)
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	var errOut error
	for err := range errCh {
		errOut = multierror.Append(errOut, err)
	}

	return errOut
}
