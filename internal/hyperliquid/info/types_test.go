package info

import (
	"encoding/json"
	"testing"
)

func TestDecimalTextUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  DecimalText
	}{
		{name: "string", input: `"1.2300"`, want: "1.2300"},
		{name: "number", input: `1.2300`, want: "1.2300"},
		{name: "negative", input: `-0.001`, want: "-0.001"},
		{name: "exponent", input: `1e-8`, want: "1e-8"},
		{name: "zero", input: `0`, want: "0"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got DecimalText
			if err := json.Unmarshal([]byte(test.input), &got); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("DecimalText = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDecimalTextUnmarshalJSONRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "invalid string", input: `"invalid"`},
		{name: "null", input: `null`},
		{name: "boolean", input: `true`},
		{name: "object", input: `{}`},
		{name: "array", input: `[]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got DecimalText
			if err := json.Unmarshal([]byte(test.input), &got); err == nil {
				t.Fatalf("json.Unmarshal(%s) succeeded, want error", test.input)
			}
		})
	}
}

func TestDecimalTextUnmarshalJSONDoesNotModifyDestinationOnError(t *testing.T) {
	t.Parallel()

	got := DecimalText("existing")
	if err := json.Unmarshal([]byte(`"invalid"`), &got); err == nil {
		t.Fatal("json.Unmarshal() succeeded, want error")
	}
	if got != "existing" {
		t.Fatalf("DecimalText = %q after failed decode, want existing", got)
	}
}

func TestDecimalTextUnmarshalJSONRejectsNilDestination(t *testing.T) {
	t.Parallel()

	var destination *DecimalText
	if err := destination.UnmarshalJSON([]byte(`1`)); err == nil {
		t.Fatal("UnmarshalJSON() succeeded, want error")
	}
}
