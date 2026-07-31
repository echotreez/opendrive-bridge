package opendrive

import (
	"context"
	"path"
	"strings"
)

// PathCache caches path to folder-id resolutions. Resolving a deep path costs
// one upstream call per lookup, and the Bridge resolves a path on nearly every
// request, so this is the difference between a snappy daemon and a chatty one
// (whitepaper §10.3).
//
// Invalidation is driven by DirUpdateTime rather than by time alone: upstream
// bumps it on every change to a folder, so a listing that comes back with a
// different value proves the cached children are stale. A TTL backs that up for
// changes made from another client. The implementation lives in internal/cache.
//
// Implementations must be safe for concurrent use.
type PathCache interface {
	// Lookup returns the cached id for a normalised path.
	Lookup(path string) (id string, ok bool)
	// Store records a resolution.
	Store(path, id string)
	// Observe records the DirUpdateTime last seen for a folder. When it differs
	// from the recorded one, everything cached below that folder is dropped.
	Observe(id string, dirUpdateTime int64)
	// InvalidatePath drops a path and everything under it.
	InvalidatePath(path string)
	// InvalidateID drops the entry for a folder id and everything under it.
	InvalidateID(id string)
	// Reset empties the cache.
	Reset()
}

// WithPathCache attaches a path cache. Without one the SDK still works, it just
// resolves every path upstream.
func WithPathCache(pc PathCache) Option {
	return func(c *Client) { c.pathCache = pc }
}

// PathCache returns the attached cache, or nil.
func (c *Client) PathCache() PathCache { return c.pathCache }

// NormalizeFolderPath cleans a user supplied path into the canonical form the
// cache and the resolver use: a leading slash, no trailing slash, no "." or
// ".." segments, and no empty segments. The root is "/".
//
// Rejecting traversal here is a security control, not a convenience: the Bridge
// API must never let a caller escape the account namespace (§9.3).
func NormalizeFolderPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", invalidRequest("a path must not contain a null byte")
	}
	// Upstream is a POSIX-style namespace; a backslash is a legal character in
	// a name, never a separator (§2.6 #10 lists it as illegal in names).
	if strings.TrimSpace(p) == "" || p == "/" || p == "." {
		return "/", nil
	}
	trimmed := p
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}

	// Whitespace is trimmed per segment, not off the path as a whole, and it is
	// done before anything else reads the segments. Both halves of that matter,
	// and a fuzzer found the first one:
	//
	// Trimming the whole string took the trailing space off the *last name*
	// whenever the path had no trailing slash, so "0 /" normalised to "/0 " and
	// normalising that gave "/0" — two spellings of one path pointing at two
	// different folders, and an answer that changed the second time it was
	// asked. That is the exact failure this function exists to prevent.
	//
	// Per-segment is also what upstream does: measured on the live API, a
	// folder created as "odbspace1 " is stored as "odbspace1", while interior
	// spaces are kept (D46). So a path asking for "a " could only ever match the
	// folder upstream calls "a", and treating them as different names would have
	// meant sending requests for something that cannot exist.
	//
	// Doing it first closes a hole the traversal check could not see: " .. " is
	// not ".." to a string comparison, but it is to upstream, which trims the
	// name before storing it. Trimming first means the check below sees what
	// upstream would see.
	segments := strings.Split(trimmed, "/")
	for i, seg := range segments {
		segments[i] = strings.TrimSpace(seg)
	}
	trimmed = strings.Join(segments, "/")

	// A ".." is refused outright rather than resolved. Resolving it would be
	// safe — path.Clean cannot escape the root — but it would silently point a
	// caller at a different folder than the one they wrote, and a bridge takes
	// its paths from scripts and config files (§9.3).
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return "", invalidRequest("a path must not contain \"..\": %q", p)
		}
	}

	cleaned := path.Clean(trimmed)
	if cleaned == "/" {
		return "/", nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(cleaned, "/"), "/") {
		if seg == "" {
			return "", invalidRequest("a path must not contain an empty segment: %q", p)
		}
		if err := ValidateName(seg); err != nil {
			return "", err
		}
	}
	return cleaned, nil
}

// ParentPath returns the parent of a normalised path, and the last segment.
func ParentPath(p string) (parent, name string) {
	if p == "/" || p == "" {
		return "/", ""
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/", strings.TrimPrefix(p, "/")
	}
	return p[:i], p[i+1:]
}

// ResolvePath turns a path into a folder id, going through the cache when the
// client has one (§10.3).
//
// The root resolves without a request at all, and a cache hit costs nothing;
// otherwise it is one folder/idbypath.json call.
func (s *FolderService) ResolvePath(ctx context.Context, p string) (string, error) {
	clean, err := NormalizeFolderPath(p)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return RootFolderID, nil
	}
	if cache := s.c.pathCache; cache != nil {
		if id, ok := cache.Lookup(clean); ok {
			return id, nil
		}
	}
	id, err := s.IDByPath(ctx, clean)
	if err != nil {
		return "", err
	}
	if cache := s.c.pathCache; cache != nil {
		cache.Store(clean, id)
	}
	return id, nil
}

// ListPath is ResolvePath followed by List, which is the shape the Bridge's
// /v1/ls endpoint needs (§4.2).
//
// The listing's DirUpdateTime is fed back into the cache, so a folder changed
// by another client drops its cached children on the next read (§10.3).
func (s *FolderService) ListPath(ctx context.Context, p string, opts ListOptions) (*FolderListing, error) {
	id, err := s.ResolvePath(ctx, p)
	if err != nil {
		return nil, err
	}
	listing, err := s.List(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	if cache := s.c.pathCache; cache != nil {
		cache.Observe(id, listing.DirUpdateTime.Unix())
		// Children seen in the listing are cheap to remember.
		clean, _ := NormalizeFolderPath(p)
		for _, f := range listing.Folders {
			if f.Name == "" || f.FolderID == "" {
				continue
			}
			child := clean
			if child == "/" {
				child = "/" + f.Name
			} else {
				child += "/" + f.Name
			}
			cache.Store(child, f.FolderID.String())
		}
	}
	return listing, nil
}

// invalidate drops cached knowledge about a folder after a write.
func (s *FolderService) invalidate(ids ...string) {
	cache := s.c.pathCache
	if cache == nil {
		return
	}
	for _, id := range ids {
		if id != "" {
			cache.InvalidateID(id)
		}
	}
}
