package spec

import (
	"context"
)

const SecondsPerDay = 24 * 60 * 60

// Keep core nodes with timestamp in the last 2 days.
// We're relying on the local Core Node's database, which updates
// slowly as other nodes gossip addresses (about 1 per minute)
const MaxCoreNodeDays = 2

// Store is the top-level interface (e.g. SQLiteStore)
// It is bound to a cancellable Context.
type Store interface {
	WithCtx(ctx context.Context) Store
	// common
	CoreStats() (mapSize int, newNodes int, err error)
	NodeList() (res []CoreNode, err error)
	TrimNodes() (advanced bool, remCore int64, err error)
	// core nodes
	AddCoreNode(address Address, time int64, services uint64) error
	UpdateCoreTime(address Address) error
	ChooseCoreNode() (Address, error)
}
