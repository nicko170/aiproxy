package view

import (
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// The aliases below exist so a front-end can drive every Source method while
// importing view and nothing below it. Source.Login returns a
// provider.LoginSession and ImportCredentials takes a config.ImportSource;
// without these aliases a caller would need to import provider and config
// just to name those types, and the import-graph rule the seam exists for
// (internal/tui imports internal/view only — see internal/tui's
// TestTUIImportsOnlyTheViewSeam) would be unsatisfiable rather than merely
// enforced.
//
// They are aliases, not copies: the types stay identical, so view.Local can
// return the provider's session unchanged and nothing is converted or lost.
type (
	// LoginSession is provider.LoginSession, re-exported for front-ends.
	LoginSession = provider.LoginSession
	// LoginResult is provider.LoginResult, re-exported for front-ends.
	LoginResult = provider.LoginResult
	// ImportSource is config.ImportSource, re-exported for front-ends.
	ImportSource = config.ImportSource
)

// Import sources a front-end may pass to ImportCredentials.
const (
	ImportSourceClaudeCode = config.ImportSourceClaudeCode
	ImportSourceCodex      = config.ImportSourceCodex
)
