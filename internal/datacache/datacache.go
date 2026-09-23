// Package datacache is the caching gateway of whitepaper §3.5: a local store of
// file *contents* that clients read from and write to, with OpenDrive behind it.
//
// # This is not internal/cache
//
// `internal/cache` is the metadata cache — path to folder id, invalidated by
// DirUpdateTime (§10.3). It holds a few thousand strings and losing all of it
// costs one round trip per lookup.
//
// This package holds bytes, and in write-back mode it holds bytes that exist
// nowhere else. The whitepaper is emphatic that the two must not be confused,
// down to the configuration keys, the log fields and the metric names, so
// nothing here is called plain "cache": the config block is `datacache`, the log
// fields are `datacache_*`, and the type is a DataCache. §3.5.4 makes the point
// that a reader who thinks "the cache" is one thing will reason about the wrong
// risks.
//
// # The asymmetry that shapes everything
//
// §3.5.2 is the section to read before changing anything in this package. Reads
// and writes are not two directions of one feature; their failure modes differ
// by an order of magnitude:
//
//	                  read-through          write-back
//	authority         OpenDrive             this directory, and nowhere else
//	cost of eviction  one re-download       the user's data, permanently
//	cost of a crash   nothing               everything not yet flushed
//
// Write-back makes the bridge the sole holder of a user's bytes for a while,
// which is a responsibility nothing in v1.0 or v1.1 carried. Four rules follow,
// and none of them is negotiable:
//
//  1. A dirty entry is never evicted. Capacity pressure takes clean entries
//     only; when dirty data fills the allowance, a new write is *refused*
//     (507 cache_full) rather than making room by discarding something nobody
//     else has. This outranks any capacity target.
//  2. The journal is fsynced before the caller is told the write succeeded.
//     "2xx" means the bytes are on this disk and recorded — never that they
//     reached OpenDrive.
//  3. The user can see what has not gone up yet: dirty_bytes and dirty_objects
//     on /v1/cache/status, `odctl cache flush --wait`, and a plain "safe to shut
//     down" indicator in the GUI. Not knowing whether data is in flight at
//     shutdown is how this kind of gateway hurts people.
//  4. Shutdown drains. SIGTERM stops accepting writes, flushes what it can, and
//     if it runs out of time it names every object it could not finish rather
//     than exiting quietly.
//
// # What it is not
//
// A single-user local accelerator. Not a distributed cache, no multi-instance
// coherence (§3.5.4). The directory holds the user's data **in the clear** —
// unlike `.env`, which is encrypted — and its protection is the 0700 mode and
// whatever the disk underneath provides. Every user-facing document has to say
// so, because "the bridge encrypts things" is an easy and wrong inference.
package datacache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Defaults mirror the `datacache` block of §3.4.
const (
	// DefaultMaxBytes is the total the store may occupy.
	DefaultMaxBytes int64 = 20 << 30 // 20 GiB
	// DefaultMaxDirtyBytes bounds data that exists only here. Past it, writes
	// are refused; it is deliberately a fraction of the total so that eviction
	// always has clean entries to work with.
	DefaultMaxDirtyBytes int64 = 5 << 30 // 5 GiB
	// DefaultHighWatermark is the fraction of MaxBytes at which eviction starts.
	DefaultHighWatermark = 0.90
	// DefaultLowWatermark is where eviction stops.
	DefaultLowWatermark = 0.70
	// DefaultFlushWorkers bounds concurrent uploads to OpenDrive. Separate from
	// the job engine's pool on purpose: flushing must not compete with the
	// transfers a user is watching (§10.2d).
	DefaultFlushWorkers = 2
	// DefaultDrainTimeout bounds the SIGTERM flush before unfinished objects are
	// listed and the daemon exits.
	DefaultDrainTimeout = 2 * time.Minute
)

// Errors the REST layer maps to §4.5 codes.
var (
	// ErrCacheFull means dirty data has reached MaxDirtyBytes. It becomes
	// HTTP 507 `cache_full`. It is not an upstream error and does not go
	// through the classification layer — it is this gateway's own state — but it
	// owes the user the same plain sentence: nothing was lost, it is only not
	// sent yet.
	ErrCacheFull = errors.New("the cache is holding as much unsent data as it is allowed to; " +
		"nothing has been lost, but nothing more can be accepted until some of it reaches " +
		"OpenDrive — wait, or run `odctl cache flush --wait`")
	// ErrDirty is returned by operations that would discard unsent data:
	// refreshing or clearing a dirty entry. It becomes HTTP 409.
	ErrDirty = errors.New("that would throw away data this cache is still the only copy of; " +
		"flush it first")
	// ErrNotCached means the object is not in the store.
	ErrNotCached = errors.New("not in the cache")
	// ErrClosed means the gateway is shutting down and no longer accepting
	// writes. It is the first half of rule 4.
	ErrClosed = errors.New("the cache is draining for shutdown and is not accepting new writes")
)

// Uploader is the slice of the SDK the flusher needs. It is an interface so that
// the durability rules can be tested against an upstream that fails on demand,
// which is the only way to test them at all.
type Uploader interface {
	// Upload sends one object and returns the hash upstream recorded for it.
	//
	// Returning the hash rather than an error alone is deliberate: a successful
	// upload is not evidence that the right bytes arrived, and this project has
	// ten discrepancies saying so. The flusher compares it with the hash it
	// computed while the object was being written, and a mismatch keeps the entry
	// dirty rather than marking it clean.
	Upload(ctx context.Context, obj *Object, content *os.File) (hash string, err error)
}

// Config describes the gateway. The field names are the `datacache` keys of
// §3.4, not the metadata cache's.
type Config struct {
	// Dir is the cache directory. It sits beside .env in the unpacked folder by
	// default (§3.4), and is created 0700.
	Dir string
	// MaxBytes is the total allowance. Zero means DefaultMaxBytes.
	MaxBytes int64
	// MaxDirtyBytes bounds unsent data. Zero means DefaultMaxDirtyBytes.
	MaxDirtyBytes int64
	// HighWatermark and LowWatermark are fractions of MaxBytes: eviction starts
	// at the first and stops at the second.
	HighWatermark float64
	LowWatermark  float64
	// WriteBack enables rule 1 to 4. With it false the gateway is a read cache
	// only: writes go straight upstream, nothing is ever dirty, and none of the
	// durability questions arise. §8.3.1 notes this is the honest setting for a
	// container whose cache directory is not on a persistent volume.
	WriteBack bool
	// FlushWorkers bounds concurrent uploads. Zero means DefaultFlushWorkers.
	FlushWorkers int
	// DrainTimeout bounds the shutdown flush. Zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
	// Upstream sends dirty objects. Required when WriteBack is true.
	Upstream Uploader
	// Logger receives structured logs. Its fields are prefixed datacache_ so
	// that a log line cannot be mistaken for the metadata cache's.
	Logger *slog.Logger
}

func (c *Config) withDefaults() error {
	if c.Dir == "" {
		return errors.New("datacache: a directory is required")
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxDirtyBytes <= 0 {
		c.MaxDirtyBytes = DefaultMaxDirtyBytes
	}
	if c.HighWatermark <= 0 || c.HighWatermark > 1 {
		c.HighWatermark = DefaultHighWatermark
	}
	if c.LowWatermark <= 0 || c.LowWatermark >= c.HighWatermark {
		c.LowWatermark = DefaultLowWatermark
	}
	if c.FlushWorkers <= 0 {
		c.FlushWorkers = DefaultFlushWorkers
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.New(discardHandler{})
	}
	// MaxDirtyBytes above MaxBytes would make rule 1 unsatisfiable: dirty data
	// could fill the store with nothing clean left to evict, and then every
	// write would be refused with the allowance apparently unused. Catching it
	// here is better than explaining it in a support thread.
	if c.MaxDirtyBytes > c.MaxBytes {
		return fmt.Errorf("datacache: max_dirty_bytes (%d) cannot exceed max_bytes (%d): "+
			"unsent data would fill the cache with nothing left to evict", c.MaxDirtyBytes, c.MaxBytes)
	}
	if c.WriteBack && c.Upstream == nil {
		return errors.New("datacache: write-back needs somewhere to flush to")
	}
	abs, err := filepath.Abs(c.Dir)
	if err != nil {
		return fmt.Errorf("datacache: %s: %w", c.Dir, err)
	}
	c.Dir = abs
	return nil
}

// discardHandler is a slog.Handler that drops everything, so a nil logger needs
// no checks at the call sites.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
