package cache

import (
	"fmt"
	"strings"
	"time"

	"github.com/bluele/gcache"
	cid "github.com/nspcc-dev/neofs-sdk-go/container/id"
	oid "github.com/nspcc-dev/neofs-sdk-go/object/id"
	"go.uber.org/zap"
)

/*
	This is an implementation of cache which keeps unsorted lists of objects' IDs (all versions)
	for a specified bucket and a prefix.

	The cache contains gcache whose entries have a key: ObjectsListKey struct and a value: list of ids.
	After putting a record, it lives for a while (default value is 60 seconds).

	When we receive a request from a user, we try to find the suitable and non-expired cache entry, go through the list
	and get ObjectInfos from common object cache or with a request to NeoFS.

	When we put an object into a container, we invalidate entries with prefixes that are prefixes of the object's name.
*/

type (
	// ObjectsListCache contains cache for ListObjects and ListObjectVersions.
	ObjectsListCache struct {
		cache  gcache.Cache
		logger *zap.Logger
	}

	// ObjectsListKey is a key to find a ObjectsListCache's entry.
	ObjectsListKey struct {
		cid        string
		prefix     string
		latestOnly bool
	}
)

const (
	// DefaultObjectsListCacheLifetime is a default lifetime of entries in cache of ListObjects.
	DefaultObjectsListCacheLifetime = time.Second * 60
	// DefaultObjectsListCacheSize is a default size of cache of ListObjects.
	DefaultObjectsListCacheSize = 1e5
)

// DefaultObjectsListConfig returns new default cache expiration values.
func DefaultObjectsListConfig(logger *zap.Logger) *Config {
	return &Config{
		Size:     DefaultObjectsListCacheSize,
		Lifetime: DefaultObjectsListCacheLifetime,
		Logger:   logger,
	}
}

// NewObjectsListCache is a constructor which creates an object of ListObjectsCache with the given lifetime of entries.
func NewObjectsListCache(config *Config) *ObjectsListCache {
	gc := gcache.New(config.Size).LRU().Expiration(config.Lifetime).Build()
	return &ObjectsListCache{cache: gc, logger: config.Logger}
}

// Get returns a list of ObjectInfo.
func (l *ObjectsListCache) Get(key ObjectsListKey) []oid.ID {
	entry, err := l.cache.Get(key)
	if err != nil {
		return nil
	}

	result, ok := entry.([]oid.ID)
	if !ok {
		l.logger.Warn("invalid cache entry type", zap.String("actual", fmt.Sprintf("%T", entry)),
			zap.String("expected", fmt.Sprintf("%T", result)))
		return nil
	}

	return result
}

// Put puts a list of objects to cache.
func (l *ObjectsListCache) Put(key ObjectsListKey, oids []oid.ID) error {
	if len(oids) == 0 {
		return fmt.Errorf("list is empty, cid: %s, prefix: %s", key.cid, key.prefix)
	}

	return l.cache.Set(key, oids)
}

// CleanCacheEntriesContainingObject deletes entries containing specified object.
func (l *ObjectsListCache) CleanCacheEntriesContainingObject(objectName string, cnr cid.ID) {
	cidStr := cnr.EncodeToString()
	keys := l.cache.Keys(true)
	for _, key := range keys {
		k, ok := key.(ObjectsListKey)
		if !ok {
			l.logger.Warn("invalid cache key type", zap.String("actual", fmt.Sprintf("%T", key)),
				zap.String("expected", fmt.Sprintf("%T", k)))
			continue
		}
		if cidStr == k.cid && strings.HasPrefix(objectName, k.prefix) {
			l.cache.Remove(k)
		}
	}
}

// CreateObjectsListCacheKey returns ObjectsListKey with the given CID, prefix and latestOnly flag.
func CreateObjectsListCacheKey(cnr *cid.ID, prefix string, latestOnly bool) ObjectsListKey {
	p := ObjectsListKey{
		cid:        cnr.EncodeToString(),
		prefix:     prefix,
		latestOnly: latestOnly,
	}

	return p
}
