package customerpolicy

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

var (
	activeManager atomic.Value
	registerOnce  sync.Once
)

type usageBridge struct{}

// RegisterUsagePlugin registers a deduped bridge into the global usage manager.
func RegisterUsagePlugin(manager *Manager) {
	if manager == nil {
		return
	}
	activeManager.Store(manager)
	registerOnce.Do(func() {
		usage.RegisterPlugin(usageBridge{})
	})
}

func (usageBridge) HandleUsage(context.Context, usage.Record) {}

func (usageBridge) HandleUsageSync(ctx context.Context, record usage.Record) {
	value := activeManager.Load()
	manager, _ := value.(*Manager)
	if manager == nil {
		return
	}
	manager.FinalizeUsage(ctx, record)
}
