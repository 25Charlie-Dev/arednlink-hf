package jsoncheck

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidObjects(t *testing.T) {
	cases := []string{
		`{}`,
		` {"a":1,"b":"text","c":true,"d":null} `,
		`{"a":{"b":1},"c":[{"b":2}]}`,
		`{"a":[[],{},["string",false,null,1.2e9]],"b":0}`,
		`{"a":1e999999}`,
		`{"a":1,"á":2}`,
		`{"a":{"same":1},"b":{"same":2}}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if err := Validate([]byte(input), 8); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMalformedAndTrailingInput(t *testing.T) {
	cases := []string{
		``, `null`, `1`, `"string"`, `true`, `[]`, `[{}]`,
		`{`, `}`, `{"a":}`, `{"a":1,}`, `{"a" 1}`, `{"a":01}`,
		`{"a":[1,]}`, `{"a":[}`, `{"a":{]}`, `{"a":"unterminated}`,
		`{} {}`, `{} []`, `{} 1`, `{} true`, `{} junk`, "{}\x00",
		"{\"a\":\"\xff\"}",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if err := Validate([]byte(input), 8); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want invalid json", err)
			}
		})
	}
}

func TestDuplicateKeysAndCaseAliases(t *testing.T) {
	cases := []string{
		`{"a":1,"a":2}`,
		`{"a":1,"A":2}`,
		`{"name":1,"NAME":2}`,
		`{"NaMe":1,"nAmE":2}`,
		`{"a":{"key":1,"KEY":2}}`,
		`{"a":[{"key":1,"Key":2}]}`,
		`{"a":1,"\u0061":2}`,
		`{"name":1,"\u004eAME":2}`,
		`{"k":1,"\u212a":2}`,
		`{"s":1,"\u017f":2}`,
		`{"é":1,"É":2}`,
		`{"σ":1,"ς":2}`,
		`{"":1,"":2}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if err := Validate([]byte(input), 16); !errors.Is(err, ErrInvalid) {
				t.Fatalf("duplicate error = %v", err)
			}
		})
	}
}

func TestUnicodeAliasesMatchTypedDecoding(t *testing.T) {
	var target struct {
		K int `json:"k"`
		S int `json:"s"`
	}
	input := []byte(`{"k":1,"\u212a":2,"s":3,"\u017f":4}`)
	if err := json.Unmarshal(input, &target); err != nil {
		t.Fatal(err)
	}
	if target.K != 2 || target.S != 4 {
		t.Fatalf("fixture does not exercise typed aliases: %+v", target)
	}
	if err := Validate(input, 8); !errors.Is(err, ErrInvalid) {
		t.Fatalf("typed aliases accepted: %v", err)
	}
}

func TestNestingBoundaries(t *testing.T) {
	if err := Validate([]byte(`{"a":1}`), 1); err != nil {
		t.Fatalf("primitive consumes container depth: %v", err)
	}
	for _, depth := range []int{1, 8, 16, 64} {
		input := `{"a":` + strings.Repeat("[", depth-1) + `0` + strings.Repeat("]", depth-1) + `}`
		if err := Validate([]byte(input), depth); err != nil {
			t.Fatalf("depth %d at limit: %v", depth, err)
		}
		over := `{"a":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`
		if err := Validate([]byte(over), depth); !errors.Is(err, ErrInvalid) {
			t.Fatalf("depth %d over limit error = %v", depth, err)
		}
	}
	for _, limit := range []int{-1, 0, 65} {
		if err := Validate([]byte(`{}`), limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid depth %d error = %v", limit, err)
		}
	}
}
