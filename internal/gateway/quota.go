// This file keeps the data plane honest: a tenant may only store as many bytes
// as it has actually paid for. Orders are sold by capacity, so without a write
// gate a 2 GB customer could park 100 GB in the cache and the platform would
// pay suppliers for memory it never billed.
package gateway

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/buntdb"

	"github.com/xiancuncloud/idlemem-server/internal/model"
)

const capCacheTTL = 10 * time.Second

// quotaState 跟踪每个租户已写入的字节数与已购容量。
//
// used 用增量维护（SET 覆盖时扣掉旧值长度），并由后台循环周期性全量校准，
// 因为 TTL 过期删除的 key 不会经过 DEL，只靠增量会只多不少。
type quotaState struct {
	mu     sync.Mutex
	used   map[string]int64
	loaded map[string]bool
	cap    map[string]int64
	capAt  map[string]time.Time
}

func newQuotaState() *quotaState {
	return &quotaState{
		used:   make(map[string]int64),
		loaded: make(map[string]bool),
		cap:    make(map[string]int64),
		capAt:  make(map[string]time.Time),
	}
}

// capacity 返回租户可写字节上限。
//
// 取值为活跃订单容量之和；若租户还设置了 quota_bytes，则再取一次上限。
// 两者都为 0 视作"未配置配额"（本地开发/试用），此时不做写入限制。
func (g *Gateway) capacity(t *model.Tenant) (int64, error) {
	q := g.quota
	q.mu.Lock()
	if at, ok := q.capAt[t.ID]; ok && time.Since(at) < capCacheTTL {
		v := q.cap[t.ID]
		q.mu.Unlock()
		return v, nil
	}
	q.mu.Unlock()

	orders, err := g.store.ListOrders()
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, o := range orders {
		if o.TenantID == t.ID && o.Status == model.OrderActive {
			sum += int64(o.CapacityBytes)
		}
	}
	if quota := int64(t.QuotaBytes); quota > 0 && (sum == 0 || quota < sum) {
		sum = quota
	}

	q.mu.Lock()
	q.cap[t.ID] = sum
	q.capAt[t.ID] = time.Now()
	q.mu.Unlock()
	return sum, nil
}

// usedBytes 返回租户当前已写字节（首次调用时全量扫描本地缓存）。
func (g *Gateway) usedBytes(t *model.Tenant) int64 {
	q := g.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.loaded[t.ID] {
		q.used[t.ID] = g.scanTenantLocked(t)
		q.loaded[t.ID] = true
	}
	return q.used[t.ID]
}

// scanTenantLocked 统计某租户在本地缓存中的字节总数（调用方需持有 q.mu）。
func (g *Gateway) scanTenantLocked(t *model.Tenant) int64 {
	prefix := t.ID + ":"
	var sum int64
	_ = g.cache.View(func(tx *buntdb.Tx) error {
		return tx.AscendGreaterOrEqual("", prefix, func(k, v string) bool {
			if !strings.HasPrefix(k, prefix) {
				return false // 升序，越界即可停止
			}
			sum += int64(len(v))
			return true
		})
	})
	return sum
}

// checkSet 在写入前校验容量，返回写入成功后应累加的字节增量。
// oldLen 为被覆盖的旧值长度（键不存在时为 0）。
func (g *Gateway) checkSet(t *model.Tenant, oldLen, newLen int) (int64, error) {
	limit, err := g.capacity(t)
	if err != nil {
		return 0, err
	}
	if limit <= 0 {
		return int64(newLen) - int64(oldLen), nil
	}
	q := g.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.loaded[t.ID] {
		q.used[t.ID] = g.scanTenantLocked(t)
		q.loaded[t.ID] = true
	}
	used := q.used[t.ID]
	after := used - int64(oldLen) + int64(newLen)
	if after > limit {
		return 0, fmt.Errorf("capacity exceeded: used=%d requested=%d limit=%d",
			used-int64(oldLen), int64(newLen), limit)
	}
	// 先返回增量，由调用方在写入成功后提交，避免写失败污染计数。
	return int64(newLen) - int64(oldLen), nil
}

// commit 累加已写字节（SET 成功或 DEL 完成后调用）。
func (g *Gateway) commit(tenantID string, delta int64) {
	if delta == 0 {
		return
	}
	q := g.quota
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.loaded[tenantID] {
		return // 未初始化则交给下次全量扫描
	}
	q.used[tenantID] += delta
	if q.used[tenantID] < 0 {
		q.used[tenantID] = 0
	}
}

// RecountUsage 重新扫描所有已跟踪租户的已写字节，纠正 TTL 过期造成的漂移。
// 由后台循环定期调用。
func (g *Gateway) RecountUsage() {
	if g.quota == nil {
		return
	}
	tenants, err := g.store.ListTenants()
	if err != nil {
		g.logger.Printf("gateway: recount usage: %v", err)
		return
	}
	for _, t := range tenants {
		q := g.quota
		q.mu.Lock()
		if q.loaded[t.ID] {
			q.used[t.ID] = g.scanTenantLocked(t)
		}
		q.mu.Unlock()
	}
}
