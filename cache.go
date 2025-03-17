package icalmiddleware

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

const (
	// NoExpiration indicates that the item never expires
	NoExpiration time.Duration = -1

	// DefaultExpiration indicates to use the default expiration time
	DefaultExpiration time.Duration = 0
)

// Item represents a cache item
type Item struct {
	Object     bool      // The cached value
	Expiration int64     // When the item expires (Unix nano time)
	LastAccess time.Time // When the item was last accessed
}

// Expired returns true if the item has expired
func (item Item) Expired() bool {
	if item.Expiration == 0 {
		return false
	}
	return time.Now().UnixNano() > item.Expiration
}

// Cache is the main cache structure
type Cache struct {
	*cache
}

// NewCache creates a new cache with the given expiration and cleanup interval
func NewCache(defaultExpiration, cleanupInterval time.Duration) *Cache {
	items := make(map[string]Item, 100) // Pre-allocate space for better performance
	return newCacheWithJanitor(defaultExpiration, cleanupInterval, items)
}

// NewFrom creates a new cache with the given expiration, cleanup interval, and items
func NewFrom(defaultExpiration, cleanupInterval time.Duration, items map[string]Item) *Cache {
	return newCacheWithJanitor(defaultExpiration, cleanupInterval, items)
}

// cache is the internal cache implementation
type cache struct {
	defaultExpiration time.Duration
	items             map[string]Item
	mu                sync.RWMutex
	onEvicted         func(string, bool)
	janitor           *janitor
}

// newCache creates a new cache with the given expiration and items
func newCache(de time.Duration, m map[string]Item) *cache {
	if de == 0 {
		de = -1
	}
	c := &cache{
		defaultExpiration: de,
		items:             m,
	}
	return c
}

// newCacheWithJanitor creates a new cache with a janitor goroutine
func newCacheWithJanitor(de time.Duration, ci time.Duration, m map[string]Item) *Cache {
	c := newCache(de, m)
	// This trick ensures that the janitor goroutine (which--granted it
	// was enabled--is running DeleteExpired on c forever) does not keep
	// the returned C object from being garbage collected. When it is
	// garbage collected, the finalizer stops the janitor goroutine, after
	// which c can be collected.
	C := &Cache{
		cache: c,
	}
	if ci > 0 {
		runJanitor(c, ci)
		runtime.SetFinalizer(C, stopJanitor)
	}
	return C
}

// Set adds an item to the cache
func (c *cache) Set(k string, x bool, d time.Duration) {
	// "Inlining" of set
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.mu.Lock()
	c.items[k] = Item{
		Object:     x,
		Expiration: e,
		LastAccess: time.Now(),
	}
	c.mu.Unlock()
}

// set is an internal method to set a cache item
func (c *cache) set(k string, x bool, d time.Duration) {
	var e int64
	if d == DefaultExpiration {
		d = c.defaultExpiration
	}
	if d > 0 {
		e = time.Now().Add(d).UnixNano()
	}
	c.items[k] = Item{
		Object:     x,
		Expiration: e,
		LastAccess: time.Now(),
	}
}

// SetDefault adds an item to the cache with the default expiration
func (c *cache) SetDefault(k string, x bool) {
	c.Set(k, x, DefaultExpiration)
}

// Add adds an item to the cache only if it doesn't already exist
func (c *cache) Add(k string, x bool, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if found {
		c.mu.Unlock()
		return fmt.Errorf("item %v already exists", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Replace replaces an existing item in the cache
func (c *cache) Replace(k string, x bool, d time.Duration) error {
	c.mu.Lock()
	_, found := c.get(k)
	if !found {
		c.mu.Unlock()
		return fmt.Errorf("item %v doesn't exist", k)
	}
	c.set(k, x, d)
	c.mu.Unlock()
	return nil
}

// Has checks if an item exists in the cache
func (c *cache) Has(k string) bool {
	_, has := c.Get(k)
	return has
}

// Get retrieves an item from the cache
func (c *cache) Get(k string) (bool, bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return item.Object, false
	}
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return item.Object, false
		}
	}
	c.mu.RUnlock()
	
	// Update last access time
	c.mu.Lock()
	if item, found := c.items[k]; found {
		item.LastAccess = time.Now()
		c.items[k] = item
	}
	c.mu.Unlock()
	
	return item.Object, true
}

// GetWithExpiration retrieves an item and its expiration time
func (c *cache) GetWithExpiration(k string) (interface{}, time.Time, bool) {
	c.mu.RLock()
	// "Inlining" of get and Expired
	item, found := c.items[k]
	if !found {
		c.mu.RUnlock()
		return nil, time.Time{}, false
	}

	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			c.mu.RUnlock()
			return nil, time.Time{}, false
		}

		// Return the item and the expiration time
		c.mu.RUnlock()
		
		// Update last access time
		c.mu.Lock()
		if item, found := c.items[k]; found {
			item.LastAccess = time.Now()
			c.items[k] = item
		}
		c.mu.Unlock()
		
		return item.Object, time.Unix(0, item.Expiration), true
	}

	// If expiration <= 0 (i.e. no expiration time set) then return the item
	// and a zeroed time.Time
	c.mu.RUnlock()
	
	// Update last access time
	c.mu.Lock()
	if item, found := c.items[k]; found {
		item.LastAccess = time.Now()
		c.items[k] = item
	}
	c.mu.Unlock()
	
	return item.Object, time.Time{}, true
}

// get is an internal method to get an item from the cache
func (c *cache) get(k string) (bool, bool) {
	item, found := c.items[k]
	if !found {
		return item.Object, false
	}
	// "Inlining" of Expired
	if item.Expiration > 0 {
		if time.Now().UnixNano() > item.Expiration {
			return item.Object, false
		}
	}
	return item.Object, true
}

// Delete removes an item from the cache
func (c *cache) Delete(k string) {
	c.mu.Lock()
	v, evicted := c.delete(k)
	c.mu.Unlock()
	if evicted {
		c.onEvicted(k, v)
	}
}

// delete is an internal method to delete an item from the cache
func (c *cache) delete(k string) (bool, bool) {
	if c.onEvicted != nil {
		if v, found := c.items[k]; found {
			delete(c.items, k)
			return v.Object, true
		}
	}

	delete(c.items, k)
	return false, false
}

// keyAndValue represents a key-value pair
type keyAndValue struct {
	key   string
	value bool
}

// DeleteExpired deletes all expired items from the cache
func (c *cache) DeleteExpired() {
	var evictedItems []keyAndValue
	now := time.Now().UnixNano()
	c.mu.Lock()
	for k, v := range c.items {
		// "Inlining" of expired
		if v.Expiration > 0 && now > v.Expiration {
			ov, evicted := c.delete(k)
			if evicted {
				evictedItems = append(evictedItems, keyAndValue{k, ov})
			}
		}
	}
	c.mu.Unlock()
	for _, v := range evictedItems {
		c.onEvicted(v.key, v.value)
	}
}

// DeleteLeastRecent removes the least recently accessed items when the cache exceeds the given size
func (c *cache) DeleteLeastRecent(maxSize int) {
	if len(c.items) <= maxSize {
		return
	}
	
	// Find items to delete
	var itemsToDelete []string
	var oldestTime time.Time
	var oldestKey string
	
	c.mu.RLock()
	// Initialize with the first item
	for k, v := range c.items {
		oldestTime = v.LastAccess
		oldestKey = k
		break
	}
	
	// Find the oldest items
	for len(c.items) - len(itemsToDelete) > maxSize {
		// Find the oldest item
		for k, v := range c.items {
			if v.LastAccess.Before(oldestTime) && !contains(itemsToDelete, k) {
				oldestTime = v.LastAccess
				oldestKey = k
			}
		}
		itemsToDelete = append(itemsToDelete, oldestKey)
		
		// Reset for next iteration
		oldestTime = time.Now()
		for k, v := range c.items {
			if v.LastAccess.Before(oldestTime) && !contains(itemsToDelete, k) {
				oldestTime = v.LastAccess
				oldestKey = k
			}
		}
	}
	c.mu.RUnlock()
	
	// Delete the items
	c.mu.Lock()
	for _, k := range itemsToDelete {
		delete(c.items, k)
	}
	c.mu.Unlock()
}

// contains checks if a string is in a slice
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// OnEvicted sets a callback for when items are evicted
func (c *cache) OnEvicted(f func(string, bool)) {
	c.mu.Lock()
	c.onEvicted = f
	c.mu.Unlock()
}

// Save serializes the cache to an io.Writer
func (c *cache) Save(w io.Writer) (err error) {
	enc := gob.NewEncoder(w)
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("error registering item types with Gob library")
		}
	}()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, v := range c.items {
		gob.Register(v.Object)
	}
	err = enc.Encode(&c.items)
	return
}

// SaveFile saves the cache to a file
func (c *cache) SaveFile(fname string) error {
	fp, err := os.Create(fname)
	if err != nil {
		return err
	}
	err = c.Save(fp)
	if err != nil {
		fp.Close()
		return err
	}
	return fp.Close()
}

// Load deserializes the cache from an io.Reader
func (c *cache) Load(r io.Reader) error {
	dec := gob.NewDecoder(r)
	items := map[string]Item{}
	err := dec.Decode(&items)
	if err == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		for k, v := range items {
			ov, found := c.items[k]
			if !found || ov.Expired() {
				c.items[k] = v
			}
		}
	}
	return err
}

// LoadFile loads the cache from a file
func (c *cache) LoadFile(fname string) error {
	fp, err := os.Open(fname)
	if err != nil {
		return err
	}
	err = c.Load(fp)
	if err != nil {
		fp.Close()
		return err
	}
	return fp.Close()
}

// Items returns all unexpired items in the cache
func (c *cache) Items() map[string]Item {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[string]Item, len(c.items))
	now := time.Now().UnixNano()
	for k, v := range c.items {
		// "Inlining" of Expired
		if v.Expiration > 0 {
			if now > v.Expiration {
				continue
			}
		}
		m[k] = v
	}
	return m
}

// ItemCount returns the number of items in the cache
func (c *cache) ItemCount() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// Flush removes all items from the cache
func (c *cache) Flush() {
	c.mu.Lock()
	c.items = map[string]Item{}
	c.mu.Unlock()
}

// janitor cleans up expired items at regular intervals
type janitor struct {
	Interval time.Duration
	stop     chan bool
}

// Run runs the janitor
func (j *janitor) Run(c *cache) {
	ticker := time.NewTicker(j.Interval)
	for {
		select {
		case <-ticker.C:
			c.DeleteExpired()
			// Also clean up if the cache gets too big (more than 10000 items)
			c.DeleteLeastRecent(10000)
		case <-j.stop:
			ticker.Stop()
			return
		}
	}
}

// stopJanitor stops the janitor
func stopJanitor(c *Cache) {
	c.janitor.stop <- true
}

// runJanitor starts the janitor
func runJanitor(c *cache, ci time.Duration) {
	j := &janitor{
		Interval: ci,
		stop:     make(chan bool),
	}
	c.janitor = j
	go j.Run(c)
}
