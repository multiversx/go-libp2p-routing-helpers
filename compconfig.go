package routinghelpers

import (
	"context"
	"time"

	"github.com/multiformats/go-multihash"
	"github.com/multiversx/go-libp2p/core/routing"
)

type ParallelRouter struct {
	Timeout      time.Duration
	IgnoreError  bool
	Router       routing.Routing
	ExecuteAfter time.Duration
}

type SequentialRouter struct {
	Timeout     time.Duration
	IgnoreError bool
	Router      routing.Routing
}

type ProvideManyRouter interface {
	ProvideMany(ctx context.Context, keys []multihash.Multihash) error
	Ready() bool
}
