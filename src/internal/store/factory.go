package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bmjdotnet/teamster/internal/config"
)

// Options holds backend-neutral construction flags. Every backend's OpenFunc
// receives the same Options; a backend that cannot honor one documents it.
// The two options here predate the registry (they moved from mysql.Option)
// and are universal — no per-backend divergence yet.
type Options struct {
	// RequireTagsOnDone gates hard close-out enforcement: a workunit's 'done'
	// transition is rejected if a required tag key has no value bound.
	RequireTagsOnDone bool
	// SkipMigrate skips the backend's auto-migration step. Use for read-only
	// callers (e.g. teamster status) that must never modify schema.
	SkipMigrate bool
}

// Option configures Options at construction.
type Option func(*Options)

// WithRequireTagsOnDone enables hard close-out enforcement. Wired from
// config.RequireTagsOnDone (TEAMSTER_REQUIRE_TAGS_ON_DONE). Omitting the
// option leaves enforcement off, byte-identical to pre-feature behavior.
func WithRequireTagsOnDone(v bool) Option {
	return func(o *Options) { o.RequireTagsOnDone = v }
}

// WithSkipMigrate skips the auto-migration step. Use for read-only callers
// (e.g. teamster status) that should never modify schema.
func WithSkipMigrate() Option {
	return func(o *Options) { o.SkipMigrate = true }
}

// OpenFunc constructs a Store from a raw DSN string plus Options. Backend
// packages register one under their scheme(s) via Register, from init() —
// the same side-effect-import idiom database/sql drivers use.
type OpenFunc func(ctx context.Context, dsn string, opts ...Option) (Store, error)

// registry maps a DSN scheme to the backend that handles it. Populated only
// from backend package init() functions, which Go runs single-threaded
// before main() — Open is never called until after every init() has run, so
// no synchronization is needed between writes (registration) and reads.
var registry = map[string]OpenFunc{}

// Register adds a backend opener under scheme. Panics on a duplicate scheme —
// called only from a backend package's init(), so a duplicate is a build-time
// wiring bug, not a runtime condition to handle gracefully.
func Register(scheme string, open OpenFunc) {
	if _, exists := registry[scheme]; exists {
		panic(fmt.Sprintf("store: backend already registered for scheme %q", scheme))
	}
	registry[scheme] = open
}

// Open parses dsn, dispatches to the backend registered for its scheme, and
// returns the constructed Store. This is the only construction path — no
// caller anywhere names a concrete backend package directly.
func Open(ctx context.Context, dsn string, opts ...Option) (Store, error) {
	d, err := config.ParseStoreDSN(dsn)
	if err != nil {
		return nil, err
	}
	open, ok := registry[d.Scheme]
	if !ok {
		return nil, fmt.Errorf("store: no backend registered for scheme %q (have: %s)", d.Scheme, registeredSchemes())
	}
	return open(ctx, dsn, opts...)
}

// registeredSchemes returns the sorted, comma-joined list of registered
// schemes, for the "no backend registered" error message.
func registeredSchemes() string {
	if len(registry) == 0 {
		return "(none)"
	}
	schemes := make([]string, 0, len(registry))
	for s := range registry {
		schemes = append(schemes, s)
	}
	sort.Strings(schemes)
	return strings.Join(schemes, ", ")
}
