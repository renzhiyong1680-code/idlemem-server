// Tenant snapshots: AUTH caches a *model.Tenant on the connection, so without
// a refresh a suspended tenant or a changed quota would never take effect on
// an existing connection. Every data command re-reads the tenant at most once
// per tenantSnapTTL.
package gateway

import (
	"sync"
	"time"

	"github.com/tidwall/redcon"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

const tenantSnapTTL = 10 * time.Second

type snapEntry struct {
	t  *model.Tenant
	at time.Time
}

type tenantSnaps struct {
	mu   sync.Mutex
	byID map[string]snapEntry
}

func newTenantSnaps() *tenantSnaps {
	return &tenantSnaps{byID: make(map[string]snapEntry)}
}

// requireTenant 返回当前租户的最新快照；未鉴权或已停服时写错误并返回 nil。
func (g *Gateway) requireTenant(conn redcon.Conn) *model.Tenant {
	t := tenant(conn)
	if t == nil {
		conn.WriteError("NOAUTH Authentication required")
		return nil
	}
	cur := g.snapshot(t)
	if cur.Status == model.TenantSuspended {
		conn.WriteError("ERR tenant suspended")
		return nil
	}
	return cur
}

// snapshot 返回租户的最新状态（含 quota_bytes），按 tenantSnapTTL 缓存。
func (g *Gateway) snapshot(t *model.Tenant) *model.Tenant {
	s := g.snaps
	s.mu.Lock()
	if e, ok := s.byID[t.ID]; ok && time.Since(e.at) < tenantSnapTTL {
		s.mu.Unlock()
		return e.t
	}
	s.mu.Unlock()

	fresh, err := g.store.GetTenant(t.ID)
	if err != nil || fresh == nil {
		return t
	}
	s.mu.Lock()
	s.byID[t.ID] = snapEntry{t: fresh, at: time.Now()}
	s.mu.Unlock()
	return fresh
}
