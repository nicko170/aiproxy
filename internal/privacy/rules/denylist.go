package rules

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// Denylist matches operator-supplied literals and regexes, labelled ID.
//
// This is what covers internal identifiers — hostnames, bucket names, project
// codenames — which no general model can know about and no shipped rule can
// guess. It is deliberately exempt from privacy.MinScanBytes: if an operator
// asks for a four-character codename to be redacted, redacting it is the whole
// job.
type Denylist struct {
	res []*regexp.Regexp
}

// NewDenylist compiles entries. A bare string is a literal; /.../ is a regex,
// matching the allowlist's syntax so there is one convention to learn. An
// invalid pattern is an error here rather than a surprise mid-request.
func NewDenylist(entries []string) (*Denylist, error) {
	d := &Denylist{}
	for _, e := range entries {
		if e == "" {
			continue
		}
		if len(e) >= 2 && strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") {
			re, err := regexp.Compile(e[1 : len(e)-1])
			if err != nil {
				return nil, fmt.Errorf("privacy: denylist pattern %s: %w", e, err)
			}
			d.res = append(d.res, re)
			continue
		}
		// Literals are matched case-insensitively: an operator writing a
		// hostname in lower case means it wherever it appears, and mixed casing
		// in logs is routine.
		d.res = append(d.res, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(e)))
	}
	return d, nil
}

func (d *Denylist) Name() string { return "denylist" }

// Scan reports every occurrence of every entry. Overlaps between entries, and
// with rule findings, are settled by privacy.Resolve.
func (d *Denylist) Scan(_ context.Context, text string) ([]privacy.Finding, error) {
	if len(d.res) == 0 {
		return nil, nil
	}
	var out []privacy.Finding
	for _, re := range d.res {
		for _, m := range re.FindAllStringIndex(text, -1) {
			out = append(out, privacy.Finding{
				Start: m[0], End: m[1], Label: privacy.LabelID,
				Rule: "denylist", Confidence: 1.0,
			})
		}
	}
	return out, nil
}
