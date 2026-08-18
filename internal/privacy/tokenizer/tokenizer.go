// Package tokenizer is a byte-level BPE tokenizer that reads the model's own
// tokenizer.json and reports byte offsets alongside token ids.
//
// It exists because the NER detector needs to map the model's per-token
// predictions back to spans in the original string, and a tokenizer that
// disagrees with the reference by one character produces spans that redact the
// wrong bytes — silently, in the component whose whole job is trustworthiness.
// TestEncodeMatchesReferenceExactly is the gate that keeps it honest.
//
// The vocabulary is read from the file shipped with the weights rather than from
// a baked-in o200k table: the file is the source of truth, and a table that
// drifted from it would produce ids the model was never trained on.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// Token is one token and the byte range of the input it covers.
type Token struct {
	ID         int
	Start, End int
}

// Tokenizer holds a byte-level BPE vocabulary and its merge ranks.
type Tokenizer struct {
	// vocab maps a token's byte-level-encoded string to its id.
	vocab map[string]int
	// ranks maps a merge pair to its priority; lower merges first.
	ranks map[[2]string]int
	// split is the pretokenizer: it chops input into pieces that BPE is applied
	// to independently. Uses regexp2 because the o200k pattern needs lookahead,
	// which Go's RE2 does not provide.
	split *regexp2.Regexp
	// byteEncoder maps a raw byte to its byte-level representation rune, the
	// GPT-2 convention every byte-level BPE vocabulary is written in.
	byteEncoder [256]string
	// added holds the added tokens, which are matched literally in the input
	// before the pretokenizer ever sees it.
	added []addedToken
}

// addedToken is one entry from the file's added_tokens.
type addedToken struct {
	id      int
	content string
}

// tokenizerFile is the subset of tokenizer.json this needs.
type tokenizerFile struct {
	Normalizer    json.RawMessage `json:"normalizer"`
	PreTokenizer  json.RawMessage `json:"pre_tokenizer"`
	PostProcessor json.RawMessage `json:"post_processor"`
	AddedTokens   []struct {
		ID         int    `json:"id"`
		Content    string `json:"content"`
		SingleWord bool   `json:"single_word"`
		LStrip     bool   `json:"lstrip"`
		RStrip     bool   `json:"rstrip"`
		Normalized bool   `json:"normalized"`
	} `json:"added_tokens"`
	Model struct {
		Type   string         `json:"type"`
		Vocab  map[string]int `json:"vocab"`
		Merges []any          `json:"merges"`
	} `json:"model"`
}

// Load reads a tokenizer.json.
func Load(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}
	var f tokenizerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tokenizer: parse %s: %w", path, err)
	}
	if f.Model.Type != "BPE" {
		return nil, fmt.Errorf("tokenizer: model type %q is not supported; only byte-level BPE is", f.Model.Type)
	}
	// A normalizer rewrites the input before tokenization, so offsets would no
	// longer be positions in the caller's own string. Refusing is the only safe
	// answer: the whole point of this package is that a span means exactly what
	// it says.
	if !isNullJSON(f.Normalizer) {
		return nil, fmt.Errorf("tokenizer: file has a normalizer, which rewrites the input and would invalidate byte offsets: %s", trimForError(string(f.Normalizer)))
	}

	t := &Tokenizer{vocab: f.Model.Vocab, ranks: make(map[[2]string]int, len(f.Model.Merges))}
	buildByteEncoder(&t.byteEncoder)

	// merges is either ["a b", ...] or [["a","b"], ...] depending on the version
	// of the tokenizers library that wrote the file. Both are accepted rather
	// than assuming one, because guessing wrong yields a tokenizer that loads and
	// then produces subtly wrong ids.
	for i, m := range f.Model.Merges {
		var a, b string
		switch v := m.(type) {
		case string:
			parts := strings.Split(v, " ")
			if len(parts) != 2 {
				return nil, fmt.Errorf("tokenizer: merge %d is not a pair: %q", i, v)
			}
			a, b = parts[0], parts[1]
		case []any:
			if len(v) != 2 {
				return nil, fmt.Errorf("tokenizer: merge %d is not a pair", i)
			}
			a, _ = v[0].(string)
			b, _ = v[1].(string)
		default:
			return nil, fmt.Errorf("tokenizer: merge %d has unexpected type %T", i, m)
		}
		// Earlier merges win: a duplicate pair later in the list is a lower
		// priority, never a higher one.
		if _, dup := t.ranks[[2]string{a, b}]; !dup {
			t.ranks[[2]string{a, b}] = i
		}
	}

	// Added tokens are matched literally. The stripping and single-word variants
	// change what "literally" means, so they are refused rather than ignored:
	// getting them subtly wrong would move every offset after the match.
	for _, a := range f.AddedTokens {
		if a.Content == "" {
			continue
		}
		if a.SingleWord || a.LStrip || a.RStrip || a.Normalized {
			return nil, fmt.Errorf("tokenizer: added token %q uses single_word/lstrip/rstrip/normalized, which is not supported", a.Content)
		}
		t.added = append(t.added, addedToken{id: a.ID, content: a.Content})
	}

	if err := checkPostProcessor(f.PostProcessor); err != nil {
		return nil, err
	}
	pattern, err := parsePreTokenizer(f.PreTokenizer)
	if err != nil {
		return nil, err
	}
	// regexp2's None option matches .NET default semantics, which is what the
	// o200k pattern was written against.
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: compile pretokenizer: %w", err)
	}
	t.split = re
	return t, nil
}

// preTokNode is one pre_tokenizer stage, with every field this implementation's
// correctness depends on. Bool fields are pointers so an absent field is
// distinguishable from an explicit false.
type preTokNode struct {
	Type    string `json:"type"`
	Pattern struct {
		Regex string
	} `json:"pattern"`
	Behavior       string            `json:"behavior"`
	Invert         *bool             `json:"invert"`
	AddPrefixSpace *bool             `json:"add_prefix_space"`
	UseRegex       *bool             `json:"use_regex"`
	PreTokenizers  []json.RawMessage `json:"pretokenizers"`
}

// parsePreTokenizer extracts the Split stage's regex and asserts every other
// setting that the offset arithmetic depends on.
//
// The assertions are the point. This package was verified byte-for-byte against
// one specific tokenizer.json, and a later model file could change any of these
// fields and still load: the ids and offsets would simply be quietly wrong,
// which is the exact failure the gate exists to prevent and the exact failure no
// fixture can catch, because it is a property of a file that does not exist yet.
// So anything not verified is refused by name, loudly, at load time.
func parsePreTokenizer(raw json.RawMessage) (string, error) {
	if isNullJSON(raw) {
		return "", fmt.Errorf("tokenizer: no pre_tokenizer in the file")
	}
	var node preTokNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return "", fmt.Errorf("tokenizer: parse pre_tokenizer: %w", err)
	}
	switch node.Type {
	case "Split":
		return splitPattern(node)
	case "Sequence":
		var pattern string
		for i, rawSub := range node.PreTokenizers {
			var sub preTokNode
			if err := json.Unmarshal(rawSub, &sub); err != nil {
				return "", fmt.Errorf("tokenizer: parse pre_tokenizer stage %d: %w", i, err)
			}
			switch sub.Type {
			case "Split":
				if pattern != "" {
					return "", fmt.Errorf("tokenizer: pre_tokenizer has more than one Split stage; only one was verified")
				}
				p, err := splitPattern(sub)
				if err != nil {
					return "", err
				}
				pattern = p
			case "ByteLevel":
				// A ByteLevel stage before the Split would change what the Split
				// sees, so the order is asserted rather than assumed.
				if pattern == "" {
					return "", fmt.Errorf("tokenizer: pre_tokenizer stage %d is ByteLevel before any Split stage, which this implementation was not verified against", i)
				}
				if err := checkByteLevelPreTokenizer(sub); err != nil {
					return "", err
				}
			default:
				return "", fmt.Errorf("tokenizer: pre_tokenizer stage %d has type %q, which is not supported", i, sub.Type)
			}
		}
		if pattern == "" {
			return "", fmt.Errorf("tokenizer: no Split stage in the pretokenizer sequence")
		}
		return pattern, nil
	default:
		return "", fmt.Errorf("tokenizer: pretokenizer type %q is not supported", node.Type)
	}
}

// splitPattern returns a Split stage's regex, having checked that the stage
// splits the way Encode assumes.
func splitPattern(node preTokNode) (string, error) {
	if node.Pattern.Regex == "" {
		return "", fmt.Errorf("tokenizer: Split pretokenizer has no regex")
	}
	// Encode treats the pattern's matches as the pieces, in order, with any gap
	// between them as a piece of its own. That is "Isolated" behaviour; "Removed"
	// would drop the delimiters and "MergedWith*" would attach them to a
	// neighbour, either of which changes both the ids and the spans.
	if node.Behavior != "Isolated" {
		return "", fmt.Errorf("tokenizer: Split pretokenizer has behavior %q; only %q was verified, and the others change which bytes each token covers", node.Behavior, "Isolated")
	}
	if node.Invert != nil && *node.Invert {
		return "", fmt.Errorf("tokenizer: Split pretokenizer has invert=true, which inverts the matches and was not verified")
	}
	return node.Pattern.Regex, nil
}

// checkByteLevelPreTokenizer asserts the two ByteLevel settings that change what
// Encode should produce.
//
// trim_offsets is deliberately not checked here: in the pre_tokenizer position it
// is inert, and the file this was verified against sets it to true while the
// reference still reports untrimmed offsets. It is the post_processor's copy of
// that field which governs the offsets, and checkPostProcessor asserts it.
func checkByteLevelPreTokenizer(node preTokNode) error {
	if node.AddPrefixSpace != nil && *node.AddPrefixSpace {
		return fmt.Errorf("tokenizer: ByteLevel pre_tokenizer has add_prefix_space=true, which prepends a byte that is not in the caller's input and would shift every offset by one")
	}
	// use_regex=true makes ByteLevel apply its own GPT-2 pattern on top of the
	// Split stage, so the pieces — and therefore the ids — would differ from what
	// Encode produces.
	if node.UseRegex != nil && *node.UseRegex {
		return fmt.Errorf("tokenizer: ByteLevel pre_tokenizer has use_regex=true, which splits again on its own pattern and would change the token ids")
	}
	return nil
}

// checkPostProcessor asserts that nothing in the post-processing stage rewrites
// the offsets Encode reports.
//
// The reference runs the post-processor even for encode(add_special_tokens=False),
// and ByteLevel's trim_offsets there strips leading whitespace out of every
// reported span. This implementation was verified against trim_offsets=false, so
// the tokens for " main" cover the space too. With trim_offsets=true the
// reference's spans would no longer cover the input and every span after a run of
// whitespace would disagree with this package — silently, since a redactor cannot
// tell a trimmed span from an untrimmed one.
func checkPostProcessor(raw json.RawMessage) error {
	if isNullJSON(raw) {
		return nil
	}
	var node struct {
		Type        string `json:"type"`
		TrimOffsets *bool  `json:"trim_offsets"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return fmt.Errorf("tokenizer: parse post_processor: %w", err)
	}
	if node.Type != "ByteLevel" {
		return fmt.Errorf("tokenizer: post_processor has type %q; only ByteLevel was verified, and another processor may rewrite the offsets", node.Type)
	}
	if node.TrimOffsets == nil {
		return fmt.Errorf("tokenizer: post_processor has no trim_offsets field, so the offsets it reports are unknown; re-run the tokenizer gate against this file")
	}
	if *node.TrimOffsets {
		return fmt.Errorf("tokenizer: post_processor has trim_offsets=true, which trims whitespace out of the reported spans; this implementation was verified against trim_offsets=false and would report spans that disagree with the reference")
	}
	return nil
}

// isNullJSON reports whether raw is absent or the JSON null.
func isNullJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// buildByteEncoder is the GPT-2 byte-to-unicode map every byte-level BPE
// vocabulary is written in: printable ASCII maps to itself, and the remaining
// bytes map into a private range so the vocabulary is valid UTF-8 text.
func buildByteEncoder(out *[256]string) {
	var bs []int
	for i := '!'; i <= '~'; i++ {
		bs = append(bs, int(i))
	}
	for i := '¡'; i <= '¬'; i++ {
		bs = append(bs, int(i))
	}
	for i := '®'; i <= 'ÿ'; i++ {
		bs = append(bs, int(i))
	}
	inSet := map[int]bool{}
	for _, b := range bs {
		inSet[b] = true
	}
	next := 0
	for b := 0; b < 256; b++ {
		if inSet[b] {
			out[b] = string(rune(b))
			continue
		}
		out[b] = string(rune(256 + next))
		next++
	}
}

// Encode tokenizes s and reports each token's byte span.
//
// Offsets come out of the pretokenizer's own match positions plus the byte
// lengths of the pieces BPE produces — never from re-decoding and searching,
// which is where offset bugs come from.
//
// Spans are character-aligned, matching the reference: when one character's
// bytes are split across several tokens (which happens for any character whose
// bytes never merge, such as U+FDFD), every one of those tokens reports the
// whole character's range rather than a slice of its bytes. So spans cover the
// input in order but may repeat, and a span never cuts a character in half —
// which is the property a redactor needs.
func (t *Tokenizer) Encode(s string) ([]Token, error) {
	if s == "" {
		return nil, nil
	}
	var out []Token
	// Added tokens (<|endoftext|> and friends) are matched literally and taken
	// out of the text first, exactly as the reference does, and the remaining
	// segments are pretokenized independently. Skipping this would silently
	// tokenize a literal "<|endoftext|>" in a user's prompt as six ordinary
	// tokens, which is not what the model was fed at training time.
	pos := 0
	for pos < len(s) {
		at, id, n := t.nextAddedToken(s, pos)
		if at < 0 {
			break
		}
		if at > pos {
			if err := t.encodeSegment(s[pos:at], pos, &out); err != nil {
				return nil, err
			}
		}
		out = append(out, Token{ID: id, Start: at, End: at + n})
		pos = at + n
	}
	if pos < len(s) {
		if err := t.encodeSegment(s[pos:], pos, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// encodeSegment pretokenizes and BPEs one added-token-free segment, whose first
// byte is at offset base in the original input.
func (t *Tokenizer) encodeSegment(seg string, base int, out *[]Token) error {
	// regexp2 reports rune indices. runeStart[i] is the byte offset of rune i,
	// so a match position converts to a byte offset by lookup. Building it once
	// per segment rather than once per match keeps this linear in the input.
	runeStart := make([]int, 0, len(seg)+1)
	for i := range seg {
		runeStart = append(runeStart, i)
	}
	runeStart = append(runeStart, len(seg))

	m, err := t.split.FindStringMatch(seg)
	if err != nil {
		return fmt.Errorf("tokenizer: pretokenize: %w", err)
	}
	prev := 0
	for m != nil {
		start := runeStart[m.Index]
		end := runeStart[m.Index+m.Length]
		// The pattern is expected to cover the whole input, but a gap is treated
		// as its own piece rather than dropped: dropping it would silently lose
		// bytes, and the spans would no longer cover the input.
		if start > prev {
			if err := t.encodePiece(seg[prev:start], base+prev, out); err != nil {
				return err
			}
		}
		if err := t.encodePiece(seg[start:end], base+start, out); err != nil {
			return err
		}
		prev = end
		if m, err = t.split.FindNextMatch(m); err != nil {
			return fmt.Errorf("tokenizer: pretokenize: %w", err)
		}
	}
	if prev < len(seg) {
		if err := t.encodePiece(seg[prev:], base+prev, out); err != nil {
			return err
		}
	}
	return nil
}

// encodePiece BPEs one pretokenized piece, whose first byte is at offset base in
// the original input, and appends its tokens with character-aligned spans.
func (t *Tokenizer) encodePiece(piece string, base int, out *[]Token) error {
	// bounds holds the byte offset of every character start in the piece, plus
	// its length, so a part's byte range can be widened to the characters it
	// touches. Invalid UTF-8 bytes count as one character each, matching how Go
	// ranges over a string.
	bounds := make([]int, 0, len(piece)+1)
	for i := range piece {
		bounds = append(bounds, i)
	}
	bounds = append(bounds, len(piece))

	at, lo := 0, 0
	for _, part := range t.bpe(piece) {
		enc := t.encodeBytes(part)
		id, ok := t.vocab[enc]
		if !ok {
			return fmt.Errorf("tokenizer: piece %q (byte-level %q) is not in the vocabulary; nearest entries: %v",
				part, enc, t.sortedVocab(firstRune(enc), 8))
		}
		// Widen [at, at+len(part)) to whole characters. Parts arrive in order, so
		// lo only moves forwards.
		for lo+1 < len(bounds) && bounds[lo+1] <= at {
			lo++
		}
		hi := lo
		for bounds[hi] < at+len(part) {
			hi++
		}
		*out = append(*out, Token{ID: id, Start: base + bounds[lo], End: base + bounds[hi]})
		at += len(part)
	}
	return nil
}

// nextAddedToken finds the first added token at or after from, preferring the
// earliest match and the longest one where two start at the same place, which is
// the reference's leftmost-longest rule.
func (t *Tokenizer) nextAddedToken(s string, from int) (at, id, n int) {
	at, id, n = -1, 0, 0
	for _, a := range t.added {
		i := strings.Index(s[from:], a.content)
		if i < 0 {
			continue
		}
		i += from
		if at < 0 || i < at || (i == at && len(a.content) > n) {
			at, id, n = i, a.id, len(a.content)
		}
	}
	return at, id, n
}

// encodeBytes converts raw bytes to the byte-level representation the vocabulary
// is keyed on.
func (t *Tokenizer) encodeBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); i++ {
		b.WriteString(t.byteEncoder[s[i]])
	}
	return b.String()
}

// bpe applies merges to one pretokenized piece and returns the resulting parts,
// as raw byte substrings of the input.
//
// The merge order — lowest rank, leftmost occurrence, one pair at a time — is
// what the reference implementation does, and it is not the same as merging
// every occurrence of the best pair in one pass.
func (t *Tokenizer) bpe(piece string) []string {
	if len(piece) <= 1 {
		return []string{piece}
	}
	// Start from single bytes, so every part is a byte substring and offsets are
	// exact by construction. enc holds each part's byte-level form so the merge
	// scan does not re-encode on every pass.
	parts := make([]string, len(piece))
	enc := make([]string, len(piece))
	for i := 0; i < len(piece); i++ {
		parts[i] = piece[i : i+1]
		enc[i] = t.byteEncoder[piece[i]]
	}
	for len(parts) > 1 {
		bestRank, bestAt := 0, -1
		for i := 0; i+1 < len(parts); i++ {
			rank, ok := t.ranks[[2]string{enc[i], enc[i+1]}]
			if !ok {
				continue
			}
			if bestAt < 0 || rank < bestRank {
				bestRank, bestAt = rank, i
			}
		}
		if bestAt < 0 {
			break
		}
		parts[bestAt] += parts[bestAt+1]
		enc[bestAt] += enc[bestAt+1]
		parts = append(parts[:bestAt+1], parts[bestAt+2:]...)
		enc = append(enc[:bestAt+1], enc[bestAt+2:]...)
	}
	return parts
}

// firstRune returns the first rune of s as a string, for use as a vocabulary
// prefix in diagnostics.
func firstRune(s string) string {
	if s == "" {
		return ""
	}
	_, n := utf8.DecodeRuneInString(s)
	return s[:n]
}

// trimForError shortens a JSON blob so an error message stays readable.
func trimForError(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// sortedVocab is a diagnostic used when the gate fails: it prints the vocabulary
// entries closest to an unmatched piece, which is almost always a byte-encoding
// mistake rather than a missing entry.
func (t *Tokenizer) sortedVocab(prefix string, n int) []string {
	var out []string
	for k := range t.vocab {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
