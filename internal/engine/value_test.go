package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestTypedValueRoundTrip(t *testing.T) {
	beyondDoublePrecision := int64(1)<<53 + 7
	cases := []struct {
		name     string
		input    TypedValue
		expected any
	}{
		{name: "null", input: NewNullValue(), expected: nil},
		{name: "min_int64", input: NewIntValue(math.MinInt64), expected: int64(math.MinInt64)},
		{name: "max_int64", input: NewIntValue(math.MaxInt64), expected: int64(math.MaxInt64)},
		{name: "above_2_pow_53", input: NewIntValue(beyondDoublePrecision), expected: beyondDoublePrecision},
		{name: "blob_empty", input: NewBlobValue([]byte{}), expected: []byte{}},
		{name: "blob_binary", input: NewBlobValue([]byte{0x00, 0xff, 0x7f, 0x80}), expected: []byte{0x00, 0xff, 0x7f, 0x80}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			var decoded TypedValue
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			driverVal, err := decoded.ToDriverValue()
			if err != nil {
				t.Fatalf("ToDriverValue failed: %v", err)
			}
			assertDriverValueEqual(t, tc.expected, driverVal)
		})
	}
}

func assertDriverValueEqual(t *testing.T, expected any, actual any) {
	t.Helper()
	if expectedBytes, ok := expected.([]byte); ok {
		actualBytes, actualOK := actual.([]byte)
		if !actualOK || !bytes.Equal(expectedBytes, actualBytes) {
			t.Fatalf("expected bytes %v, got %v", expectedBytes, actual)
		}
		return
	}
	if expected != actual {
		t.Fatalf("expected %v (%T), got %v (%T)", expected, expected, actual, actual)
	}
}

func TestTypedValueRejectsInvalidInputs(t *testing.T) {
	if _, err := NewFloatValue(math.NaN()); err == nil {
		t.Fatal("expected NaN float to be rejected")
	}
	if _, err := NewFloatValue(math.Inf(1)); err == nil {
		t.Fatal("expected +Inf float to be rejected")
	}
	if _, err := NewTextValue(string([]byte{0xff, 0xfe})); err == nil {
		t.Fatal("expected invalid UTF-8 text to be rejected")
	}

	invalidPayloads := []string{
		`{"type":"integer","value":"not-a-number"}`,
		`{"type":"float","value":"NaN"}`,
		`{"type":"float","value":"Inf"}`,
		`{"type":"blob","value":"!!!not-base64!!!"}`,
		`{"type":"unknown","value":"1"}`,
		`{"type":"integer","value":null}`,
		`{"type":"null","value":"unexpected"}`,
	}
	for _, payload := range invalidPayloads {
		var tv TypedValue
		if err := json.Unmarshal([]byte(payload), &tv); err == nil {
			t.Fatalf("expected payload %s to fail unmarshaling", payload)
		}
	}
}

func TestValidateNameAndSafeJoinPath(t *testing.T) {
	validNames := []string{"main", "app_db", "branch-1", "a", "0db_test-9"}
	for _, name := range validNames {
		if err := ValidateName(name); err != nil {
			t.Fatalf("expected %q to be valid, got: %v", name, err)
		}
	}

	invalidNames := []string{
		"",
		"-starts-with-dash",
		"_starts-with-underscore",
		"UpperCase",
		"../escape",
		"a/b",
		"a\\b",
		"null\x00byte",
		"a234567890123456789012345678901234567890123456789012345678901234",
	}
	for _, name := range invalidNames {
		if err := ValidateName(name); !errors.Is(err, ErrInvalidIdentifier) {
			t.Fatalf("expected %q to fail with ErrInvalidIdentifier, got: %v", name, err)
		}
	}

	baseDir := t.TempDir()
	joined, err := SafeJoinPath(baseDir, "project1", "main.sqlite")
	if err != nil {
		t.Fatalf("expected SafeJoinPath to succeed: %v", err)
	}
	expected := filepath.Join(baseDir, "project1", "main.sqlite")
	if joined != expected {
		t.Fatalf("expected %q, got %q", expected, joined)
	}

	traversalInputs := [][]string{
		{"..", "etc", "passwd"},
		{"project1", "../secret"},
		{"project1", "sub\\file"},
		{"project1", "null\x00file"},
		{"."},
		{""},
	}
	for _, segments := range traversalInputs {
		if _, err := SafeJoinPath(baseDir, segments...); err == nil {
			t.Fatalf("expected segments %v to be rejected by SafeJoinPath", segments)
		}
	}
}

func FuzzTypedValueRoundTrip(f *testing.F) {
	f.Add(int64(0), float64(0.0), "hello", []byte("world"))
	f.Add(int64(math.MaxInt64), float64(3.141592653589793), "unicode-✓", []byte{0, 1, 2, 255})
	f.Add(int64(math.MinInt64), float64(-1e-12), "", []byte{})

	f.Fuzz(func(t *testing.T, intVal int64, floatVal float64, textVal string, blobVal []byte) {
		iv := NewIntValue(intVal)
		rawInt, err := json.Marshal(iv)
		if err != nil {
			t.Fatalf("marshal int: %v", err)
		}
		var decodedInt TypedValue
		if err := json.Unmarshal(rawInt, &decodedInt); err != nil {
			t.Fatalf("unmarshal int: %v", err)
		}
		gotInt, err := decodedInt.ToDriverValue()
		if err != nil || gotInt != intVal {
			t.Fatalf("int round-trip mismatch: got %v (err %v), want %d", gotInt, err, intVal)
		}

		if !math.IsNaN(floatVal) && !math.IsInf(floatVal, 0) {
			fv, err := NewFloatValue(floatVal)
			if err != nil {
				t.Fatalf("NewFloatValue: %v", err)
			}
			rawFloat, err := json.Marshal(fv)
			if err != nil {
				t.Fatalf("marshal float: %v", err)
			}
			var decodedFloat TypedValue
			if err := json.Unmarshal(rawFloat, &decodedFloat); err != nil {
				t.Fatalf("unmarshal float: %v", err)
			}
			gotFloat, err := decodedFloat.ToDriverValue()
			if err != nil || gotFloat != floatVal {
				t.Fatalf("float round-trip mismatch: got %v (err %v), want %v", gotFloat, err, floatVal)
			}
		}

		bv := NewBlobValue(blobVal)
		rawBlob, err := json.Marshal(bv)
		if err != nil {
			t.Fatalf("marshal blob: %v", err)
		}
		var decodedBlob TypedValue
		if err := json.Unmarshal(rawBlob, &decodedBlob); err != nil {
			t.Fatalf("unmarshal blob: %v", err)
		}
		gotBlob, err := decodedBlob.ToDriverValue()
		if err != nil || !bytes.Equal(gotBlob.([]byte), blobVal) {
			t.Fatalf("blob round-trip mismatch")
		}
	})
}

func FuzzValidateName(f *testing.F) {
	f.Add("main")
	f.Add("../etc/passwd")
	f.Add("db_01-prod")
	f.Add("bad\x00name")

	f.Fuzz(func(t *testing.T, candidate string) {
		err := ValidateName(candidate)
		if err == nil {
			if len(candidate) == 0 || len(candidate) > 63 {
				t.Fatalf("invalid length accepted: %q", candidate)
			}
		}
	})
}

func FuzzSQLSplitterDifferential(f *testing.F) {
	f.Add("SELECT [a;b] FROM t")
	f.Add("SELECT 1; SELECT 2")
	f.Add("CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET x = 1; END;")
	f.Add("CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET x = CASE WHEN 1 THEN 2 END; END; DROP TABLE t")
	f.Add("SELECT 'a;b', \"c;d\", `e;f`, [g;h] -- comment ; here\n/* block ; */")

	f.Fuzz(func(t *testing.T, sqlInput string) {
		parts := SplitSQLStatements(sqlInput)
		if len(parts) == 1 {
			withSmuggled := sqlInput + "; SELECT 999"
			if !hasMultipleStatements(withSmuggled) && !strings.Contains(strings.ToUpper(sqlInput), "TRIGGER") {
				if validateErr := ValidateSingleStatement(withSmuggled); validateErr == nil {
					t.Fatalf("splitter failed to detect second statement in %q", withSmuggled)
				}
			}
		}
	})
}
