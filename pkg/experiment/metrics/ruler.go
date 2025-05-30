package metrics

import (
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"

	"github.com/grafana/pyroscope/pkg/model"
	"github.com/grafana/pyroscope/pkg/validation"
)

const (
	rulesExpiryTime = time.Minute
)

type StaticRuler struct {
	overrides *validation.Overrides
}

func NewStaticRulerFromOverrides(overrides *validation.Overrides) Ruler {
	return &StaticRuler{
		overrides: overrides,
	}
}

func (ruler StaticRuler) RecordingRules(tenant string) []*model.RecordingRule {
	rules := ruler.overrides.RecordingRules(tenant)
	rs := make([]*model.RecordingRule, 0, len(rules))
	for _, rule := range rules {
		// should never fail, overrides already validated
		r, _ := model.NewRecordingRule(rule)
		rs = append(rs, r)
	}
	return rs
}

// CachedRemoteRuler is a thread-safe ruler that retrieves rules from an external service.
// It has a per-tenant cache: rulesPerTenant
type CachedRemoteRuler struct {
	rulesPerTenant map[string]*tenantCache
	mu             sync.RWMutex

	client RecordingRulesClient

	logger log.Logger
}

type RecordingRulesClient interface {
	RecordingRules(tenant string) ([]*model.RecordingRule, error)
}

func NewCachedRemoteRuler(client RecordingRulesClient, logger log.Logger) (Ruler, error) {
	return &CachedRemoteRuler{
		rulesPerTenant: make(map[string]*tenantCache),
		client:         client,
		logger:         logger,
	}, nil
}

func (r *CachedRemoteRuler) RecordingRules(tenant string) []*model.RecordingRule {
	// get the per-tenant cache
	r.mu.RLock()
	cache, ok := r.rulesPerTenant[tenant]
	r.mu.RUnlock()

	// There's no cache for given tenant: init it
	if !ok {
		r.mu.Lock()
		defer r.mu.Unlock()

		// only race-winner will initialize the per-tenant cache
		cache, ok = r.rulesPerTenant[tenant]
		if !ok {
			cache = &tenantCache{
				initFunc: func() ([]*model.RecordingRule, error) {
					return r.client.RecordingRules(tenant)
				},
				logger: r.logger,
			}
			r.rulesPerTenant[tenant] = cache
		}
	}

	// get data from cache:
	return cache.get()
}

// tenantCache is a thread-safe cache that holds an expirable array of rules.
type tenantCache struct {
	value    []*model.RecordingRule
	ttl      time.Time
	initFunc func() ([]*model.RecordingRule, error)
	mu       sync.RWMutex
	logger   log.Logger
}

// get returns the stored value if present and not expired.
// Otherwise, a single call to initFunc will be performed to retrieve the value and hold it for future calls within
// the ttl.
func (c *tenantCache) get() []*model.RecordingRule {
	c.mu.RLock()
	if c.value != nil && time.Now().Before(c.ttl) {
		defer c.mu.RUnlock()
		// value exists and didn't expired
		return c.value
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// only race-winner will fetch the data
	if c.value == nil || time.Now().After(c.ttl) {
		value, err := c.initFunc()
		if err != nil {
			// keep old value and ttl, just log an error
			level.Error(c.logger).Log("msg", "failed to fetch recording rules", "err", err)
		} else {
			c.value = value
			c.ttl = time.Now().Add(rulesExpiryTime)
		}
	}
	return c.value
}
