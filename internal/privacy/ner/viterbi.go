package ner

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// negInf marks a forbidden transition. A large negative float32 rather than an
// actual infinity so that arithmetic on it stays finite and comparable.
const negInf = float32(-1e30)

// Span is a decoded entity over TOKEN indices; the caller maps them to byte
// offsets using the tokenizer's spans.
type Span struct {
	Start, End int // token indices, half-open
	Label      string
	Score      float64
}

// Decode runs a constrained Viterbi over per-token logits and returns the entity
// spans of the best legal path.
//
// Constrained is the operative word: taking each token's argmax independently
// produces taggings BIOES does not permit — a B with nothing after it, an I of one
// category following a B of another — which then decode into spans that do not
// exist. The transition matrix makes those paths unreachable rather than
// something to clean up afterwards.
//
// logits is [token][label]; trans is [from][to]; labels[i] names column i.
func Decode(logits [][]float32, labels []string, trans [][]float32) []Span {
	n := len(logits)
	if n == 0 || len(labels) == 0 {
		return nil
	}
	L := len(labels)

	score := make([][]float32, n)
	back := make([][]int, n)
	for i := range score {
		score[i] = make([]float32, L)
		back[i] = make([]int, L)
	}

	// A sequence may only open with O, B, or S.
	for j := 0; j < L; j++ {
		prefix, _ := splitTag(labels[j])
		if prefix == "I" || prefix == "E" {
			score[0][j] = negInf
			continue
		}
		score[0][j] = logits[0][j]
	}

	for i := 1; i < n; i++ {
		for j := 0; j < L; j++ {
			best, bestFrom := negInf, 0
			for k := 0; k < L; k++ {
				if score[i-1][k] <= negInf || trans[k][j] <= negInf {
					continue
				}
				if v := score[i-1][k] + trans[k][j]; v > best {
					best, bestFrom = v, k
				}
			}
			if best <= negInf {
				score[i][j] = negInf
				continue
			}
			score[i][j] = best + logits[i][j]
			back[i][j] = bestFrom
		}
	}

	// A sequence may only close on O, E, or S.
	bestLast, bestScore := -1, negInf
	for j := 0; j < L; j++ {
		prefix, _ := splitTag(labels[j])
		if prefix == "B" || prefix == "I" {
			continue
		}
		if score[n-1][j] > bestScore {
			bestScore, bestLast = score[n-1][j], j
		}
	}
	if bestLast < 0 {
		return nil
	}

	path := make([]int, n)
	path[n-1] = bestLast
	for i := n - 1; i > 0; i-- {
		path[i-1] = back[i][path[i]]
	}
	return spansFromPath(path, labels, logits)
}

// spansFromPath turns a tag path into spans. B..E and S both open and close a
// span; anything else is outside one.
func spansFromPath(path []int, labels []string, logits [][]float32) []Span {
	var out []Span
	start, category := -1, ""
	var sum float64
	for i, id := range path {
		prefix, cat := splitTag(labels[id])
		switch prefix {
		case "S":
			out = append(out, Span{Start: i, End: i + 1, Label: cat,
				Score: softmaxAt(logits[i], id)})
			start, category, sum = -1, "", 0
		case "B":
			start, category = i, cat
			sum = softmaxAt(logits[i], id)
		case "I":
			if start >= 0 {
				sum += softmaxAt(logits[i], id)
			}
		case "E":
			if start >= 0 {
				sum += softmaxAt(logits[i], id)
				out = append(out, Span{Start: start, End: i + 1, Label: category,
					Score: sum / float64(i+1-start)})
			}
			start, category, sum = -1, "", 0
		default: // O
			start, category, sum = -1, "", 0
		}
	}
	return out
}

// softmaxAt is the probability of column j, so Span.Score is a confidence rather
// than an unbounded logit. Nothing filters on it today; it exists so a threshold
// is a config change rather than an interface change.
func softmaxAt(row []float32, j int) float64 {
	max := row[0]
	for _, v := range row {
		if v > max {
			max = v
		}
	}
	var sum float64
	for _, v := range row {
		sum += math.Exp(float64(v - max))
	}
	if sum == 0 {
		return 0
	}
	return math.Exp(float64(row[j]-max)) / sum
}

// calibration is viterbi_calibration.json as shipped with the weights.
//
// The file does NOT carry a 33x33 matrix and does NOT carry a constraint list.
// It carries six named scalar BIASES, one per BIOES transition CLASS, under a
// named operating point:
//
//	{"operating_points":{"default":{"biases":{
//	   "transition_bias_background_stay":       0.0,
//	   "transition_bias_background_to_start":   0.0,
//	   "transition_bias_end_to_background":     0.0,
//	   "transition_bias_end_to_start":          0.0,
//	   "transition_bias_inside_to_continue":    0.0,
//	   "transition_bias_inside_to_end":         0.0}}}}
//
// So the matrix is built from BIOES legality — the fallback the brief describes
// — and each legal cell then gets the bias for its class added. At the shipped
// "default" operating point every bias is 0.0, which makes the result exactly
// the legality matrix; the biases are read anyway so a future operating point
// that actually tunes precision against recall is a data change rather than a
// code change.
type calibration struct {
	OperatingPoints map[string]struct {
		Biases map[string]float64 `json:"biases"`
	} `json:"operating_points"`
}

// defaultOperatingPoint is the operating point loadTransitions reads. The
// shipped file defines exactly one.
const defaultOperatingPoint = "default"

// transitionClass names the bias that applies to a legal from->to pair, or ""
// when no bias in the file covers it. The six classes in the file partition
// every legal BIOES transition, so "" only ever means the file named something
// this build does not know about.
func transitionClass(from, to string) string {
	fp, _ := splitTag(from)
	tp, _ := splitTag(to)
	switch fp {
	case "O":
		if tp == "O" {
			return "transition_bias_background_stay"
		}
		return "transition_bias_background_to_start" // B or S
	case "E", "S":
		if tp == "O" {
			return "transition_bias_end_to_background"
		}
		return "transition_bias_end_to_start" // B or S
	case "B", "I":
		if tp == "I" {
			return "transition_bias_inside_to_continue"
		}
		return "transition_bias_inside_to_end" // E
	}
	return ""
}

// loadTransitions builds the [from][to] transition matrix for labels, reading
// viterbi_calibration.json for the per-class biases. A missing or unparseable
// file is an error rather than a silent fall back to zeros: it ships with the
// weights, so its absence means the install is not the one that was verified.
func loadTransitions(path string, labels []string) ([][]float32, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ner: %w", err)
	}
	var cal calibration
	if err := json.Unmarshal(raw, &cal); err != nil {
		return nil, fmt.Errorf("ner: parse %s: %w", path, err)
	}
	op, ok := cal.OperatingPoints[defaultOperatingPoint]
	if !ok {
		return nil, fmt.Errorf("ner: %s has no %q operating point", path, defaultOperatingPoint)
	}
	return transitionMatrix(labels, op.Biases), nil
}

// transitionMatrix is legality plus biases, split out so the decode tests can
// build a matrix without a file.
func transitionMatrix(labels []string, biases map[string]float64) [][]float32 {
	n := len(labels)
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, n)
		for j := range out[i] {
			if !legalTransition(labels[i], labels[j]) {
				out[i][j] = negInf
				continue
			}
			out[i][j] = float32(biases[transitionClass(labels[i], labels[j])])
		}
	}
	return out
}
