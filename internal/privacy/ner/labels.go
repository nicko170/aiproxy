package ner

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// categoryLabels maps the model's category names to placeholder labels.
//
// Only categories the operator enabled are ever consulted, and the default set is
// empty. private_url and private_date are the two worth being deliberate about:
// in source code, import URLs, API endpoints, doc links, changelog dates and
// licence years are everywhere, so enabling them corrupts the agent's context for
// very little privacy gain.
var categoryLabels = map[string]privacy.Label{
	"secret":          privacy.LabelSecret,
	"private_email":   privacy.LabelEmail,
	"private_phone":   privacy.LabelPhone,
	"private_address": privacy.LabelAddress,
	"private_person":  privacy.LabelPerson,
	"private_url":     privacy.LabelURL,
	"private_date":    privacy.LabelDate,
	"account_number":  privacy.LabelAccount,
}

// Categories lists every model category the detector understands, sorted, so a
// config error can name the valid set rather than leaving the operator to guess.
func Categories() []string {
	out := make([]string, 0, len(categoryLabels))
	for k := range categoryLabels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadLabels reads id2label from the model's config.json, ordered by id, so index
// i of the returned slice is the label for logit column i.
func LoadLabels(configPath string) ([]string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("ner: %w", err)
	}
	var cfg struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("ner: parse %s: %w", configPath, err)
	}
	if len(cfg.ID2Label) == 0 {
		return nil, fmt.Errorf("ner: %s has no id2label", configPath)
	}
	ids := make([]int, 0, len(cfg.ID2Label))
	for k := range cfg.ID2Label {
		id, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("ner: id2label key %q is not an integer", k)
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]string, len(ids))
	for i, id := range ids {
		if id != i {
			return nil, fmt.Errorf("ner: id2label is not contiguous from 0 (saw %d at position %d)", id, i)
		}
		out[i] = cfg.ID2Label[strconv.Itoa(id)]
	}
	return out, nil
}

// splitTag divides "B-secret" into its BIOES prefix and category. "O" has no
// category.
func splitTag(tag string) (prefix, category string) {
	if tag == "O" {
		return "O", ""
	}
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i], tag[i+1:]
	}
	return tag, ""
}

// legalTransition encodes BIOES: a span opened with B must continue with I or end
// with E, and only E, S, or O may precede a new span. Without this, the best
// per-token guesses routinely form taggings that do not describe any set of
// spans at all.
func legalTransition(from, to string) bool {
	fp, fc := splitTag(from)
	tp, tc := splitTag(to)
	switch fp {
	case "O", "E", "S":
		return tp == "O" || tp == "B" || tp == "S"
	case "B", "I":
		return (tp == "I" || tp == "E") && tc == fc
	}
	return false
}
