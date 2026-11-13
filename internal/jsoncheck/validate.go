// Package jsoncheck validates JSON structure before typed decoding.
package jsoncheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalid identifies rejected JSON input or validation bounds.
var ErrInvalid = errors.New("invalid json")

// Validate accepts exactly one JSON object with no case-insensitive duplicate keys.
// Callers bound data bytes; maxDepth counts object and array containers, including
// the root, and must be in 1..64. Key storage is bounded by the supplied data bytes.
func Validate(data []byte, maxDepth int) error {
	if maxDepth < 1 || maxDepth > 64 {
		return fmt.Errorf("%w: depth limit outside 1..64", ErrInvalid)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: invalid utf-8", ErrInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root, err := decoder.Token()
	if err != nil || root != json.Delim('{') {
		return fmt.Errorf("%w: root must be an object", ErrInvalid)
	}
	if err := readObject(decoder, 1, maxDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	return nil
}

func readObject(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nesting limit exceeded", ErrInvalid)
	}
	keys := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return fmt.Errorf("%w: malformed object key", ErrInvalid)
		}
		folded := foldKey(key)
		if _, exists := keys[folded]; exists {
			return fmt.Errorf("%w: duplicate object key", ErrInvalid)
		}
		keys[folded] = struct{}{}
		if err := readValue(decoder, depth+1, maxDepth); err != nil {
			return err
		}
	}
	return readEnd(decoder, '}')
}

func readValue(decoder *json.Decoder, depth, maxDepth int) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: malformed value", ErrInvalid)
	}
	marker, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch marker {
	case '{':
		return readObject(decoder, depth, maxDepth)
	case '[':
		return readArray(decoder, depth, maxDepth)
	default:
		return fmt.Errorf("%w: unexpected delimiter", ErrInvalid)
	}
}

func readArray(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nesting limit exceeded", ErrInvalid)
	}
	for decoder.More() {
		if err := readValue(decoder, depth+1, maxDepth); err != nil {
			return err
		}
	}
	return readEnd(decoder, ']')
}

func readEnd(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil || token != expected {
		return fmt.Errorf("%w: malformed container", ErrInvalid)
	}
	return nil
}

func foldKey(key string) string {
	var folded strings.Builder
	folded.Grow(len(key))
	for _, r := range key {
		folded.WriteRune(foldRune(r))
	}
	return folded.String()
}

func foldRune(r rune) rune {
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(r) {
		if next < r {
			return next
		}
		r = next
	}
	return r
}
