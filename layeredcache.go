// An LRU cached aimed at high concurrency
package ccache

import (
	"container/list"
	"github.com/zeebo/xxh3"
	"sync/atomic"
	"time"
)

type LayeredCache struct {
	*Configuration
	list        *list.List
	buckets     []*layeredBucket
	bucketMask  uint64
	size        int64
	deletables  chan *Item
	promotables chan *Item
	control     chan interface{}
}

// Create a new layered cache with the specified configuration.
// A layered cache used a two keys to identify a value: a primary key
// and a secondary key. Get, Set and Delete require both a primary and
// secondary key. However, DeleteAll requires only a primary key, deleting
// all values that share the same primary key.

// Layered Cache is useful as an HTTP cache, where an HTTP purge might
// delete multiple variants of the same resource:
// primary key = "user/44"
// secondary key 1 = ".json"
// secondary key 2 = ".xml"

// See ccache.Configure() for creating a configuration
func Layered(config *Configuration) *LayeredCache {
	c := &LayeredCache{
		list:          list.New(),
		Configuration: config,
		bucketMask:    uint64(config.buckets) - 1,
		buckets:       make([]*layeredBucket, config.buckets),
		deletables:    make(chan *Item, config.deleteBuffer),
		control:       make(chan interface{}),
	}
	for i := 0; i < int(config.buckets); i++ {
		c.buckets[i] = &layeredBucket{
			buckets: make(map[string]*bucket),
		}
	}
	c.restart()
	return c
}

func (c *LayeredCache) ItemCount() int {
	count := 0
	for _, b := range c.buckets {
		count += b.itemCount()
	}
	return count
}

// Get an item from the cache. Returns nil if the item wasn't found.
// This can return an expired item. Use item.Expired() to see if the item
// is expired and item.TTL() to see how long until the item expires (which
// will be negative for an already expired item).
func (c *LayeredCache) Get(primary, secondary string) *Item {
	item := c.bucket(primary).get(primary, secondary)
	if item == nil {
		return nil
	}
	if item.expires > time.Now().UnixNano() {
		c.promote(item)
	}
	return item
}

func (c *LayeredCache) ForEachFunc(primary string, matches func(key string, item *Item) bool) {
	c.bucket(primary).forEachFunc(primary, matches)
}

// Get the secondary cache for a given primary key. This operation will
// never return nil. In the case where the primary key does not exist, a
// new, underlying, empty bucket will be created and returned.
func (c *LayeredCache) GetOrCreateSecondaryCache(primary string) *SecondaryCache {
	primaryBkt := c.bucket(primary)
	bkt := primaryBkt.getSecondaryBucket(primary)
	primaryBkt.Lock()
	if bkt == nil {
		bkt = &bucket{lookup: make(map[string]*Item)}
		primaryBkt.buckets[primary] = bkt
	}
	primaryBkt.Unlock()
	return &SecondaryCache{
		bucket: bkt,
		pCache: c,
	}
}

// Used when the cache was created with the Track() configuration option.
// Avoid otherwise
func (c *LayeredCache) TrackingGet(primary, secondary string) TrackedItem {
	item := c.Get(primary, secondary)
	if item == nil {
		return NilTracked
	}
	item.track()
	return item
}

// Set the value in the cache for the specified duration
func (c *LayeredCache) TrackingSet(primary, secondary string, value interface{}, duration time.Duration) TrackedItem {
	return c.set(primary, secondary, value, duration, true)
}

// Set the value in the cache for the specified duration
func (c *LayeredCache) Set(primary, secondary string, value interface{}, duration time.Duration) {
	c.set(primary, secondary, value, duration, false)
}

// Replace the value if it exists, does not set if it doesn't.
// Returns true if the item existed an was replaced, false otherwise.
// Replace does not reset item's TTL nor does it alter its position in the LRU
func (c *LayeredCache) Replace(primary, secondary string, value interface{}) bool {
	item := c.bucket(primary).get(primary, secondary)
	if item == nil {
		return false
	}
	c.Set(primary, secondary, value, item.TTL())
	return true
}

// Attempts to get the value from the cache and calles fetch on a miss.
// If fetch returns an error, no value is cached and the error is returned back
// to the caller.
func (c *LayeredCache) Fetch(primary, secondary string, duration time.Duration, fetch func() (interface{}, error)) (*Item, error) {
	item := c.Get(primary, secondary)
	if item != nil {
		return item, nil
	}
	value, err := fetch()
	if err != nil {
		return nil, err
	}
	return c.set(primary, secondary, value, duration, false), nil
}

// Remove the item from the cache, return true if the item was present, false otherwise.
func (c *LayeredCache) Delete(primary, secondary string) bool {
	item := c.bucket(primary).delete(primary, secondary)
	if item != nil {
		c.deletables <- item
		return true
	}
	return false
}

// Deletes all items that share the same primary key
func (c *LayeredCache) DeleteAll(primary string) bool {
	return c.bucket(primary).deleteAll(primary, c.deletables)
}

// Deletes all items that share the same primary key and prefix.
func (c *LayeredCache) DeletePrefix(primary, prefix string) int {
	return c.bucket(primary).deletePrefix(primary, prefix, c.deletables)
}

// Deletes all items that share the same primary key and where the matches func evaluates to true.
func (c *LayeredCache) DeleteFunc(primary string, matches func(key string, item *Item) bool) int {
	return c.bucket(primary).deleteFunc(primary, matches, c.deletables)
}

// Clears the cache
func (c *LayeredCache) Clear() {
	done := make(chan struct{})
	c.control <- clear{done: done}
	<-done
}

func (c *LayeredCache) Stop() {
	close(c.promotables)
	<-c.control
}

// Gets the number of items removed from the cache due to memory pressure since
// the last time GetDropped was called
func (c *LayeredCache) GetDropped() int {
	res := make(chan int)
	c.control <- getDropped{res: res}
	return <-res
}

// Sets a new max size. That can result in a GC being run if the new maxium size
// is smaller than the cached size
func (c *LayeredCache) SetMaxSize(size int64) {
	c.control <- setMaxSize{size}
}

func (c *LayeredCache) restart() {
	c.promotables = make(chan *Item, c.promoteBuffer)
	c.control = make(chan interface{})
	go c.worker()
}

func (c *LayeredCache) set(primary, secondary string, value interface{}, duration time.Duration, track bool) *Item {
	item, existing := c.bucket(primary).set(primary, secondary, value, duration, track)
	if existing != nil {
		c.deletables <- existing
	}
	c.promote(item)
	return item
}

func (c *LayeredCache) bucket(key string) *layeredBucket {
	h := xxh3.New()
	h.Write([]byte(key))
	return c.buckets[h.Sum64()&c.bucketMask]
}

func (c *LayeredCache) promote(item *Item) {
	c.promotables <- item
}

func (c *LayeredCache) worker() {
	defer close(c.control)
	dropped := 0
	for {
		select {
		case item, ok := <-c.promotables:
			if ok == false {
				return
			}
			if c.doPromote(item) && c.size > c.maxSize {
				dropped += c.gc()
			}
		case item := <-c.deletables:
			if item.element == nil {
				atomic.StoreInt32(&item.promotions, -2)
			} else {
				c.size -= item.size
				if c.onDelete != nil {
					c.onDelete(item)
				}
				c.list.Remove(item.element)
			}
		case control := <-c.control:
			switch msg := control.(type) {
			case getDropped:
				msg.res <- dropped
				dropped = 0
			case setMaxSize:
				c.maxSize = msg.size
				if c.size > c.maxSize {
					dropped += c.gc()
				}
			case clear:
				for _, bucket := range c.buckets {
					bucket.clear()
				}
				c.size = 0
				c.list = list.New()
				msg.done <- struct{}{}
			}
		}
	}
}

func (c *LayeredCache) doPromote(item *Item) bool {
	// deleted before it ever got promoted
	if atomic.LoadInt32(&item.promotions) == -2 {
		return false
	}
	if item.element != nil { //not a new item
		if item.shouldPromote(c.getsPerPromote) {
			c.list.MoveToFront(item.element)
			atomic.StoreInt32(&item.promotions, 0)
		}
		return false
	}
	c.size += item.size
	item.element = c.list.PushFront(item)
	return true
}

func (c *LayeredCache) gc() int {
	element := c.list.Back()
	dropped := 0
	for i := 0; i < c.itemsToPrune; i++ {
		if element == nil {
			return dropped
		}
		prev := element.Prev()
		item := element.Value.(*Item)
		if c.tracking == false || atomic.LoadInt32(&item.refCount) == 0 {
			c.bucket(item.group).delete(item.group, item.key)
			c.size -= item.size
			c.list.Remove(element)
			item.promotions = -2
			dropped += 1
		}
		element = prev
	}
	return dropped
}
