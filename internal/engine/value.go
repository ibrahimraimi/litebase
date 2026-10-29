package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

type ValueType string

const (
	TypeNull    ValueType = "null"
	TypeInteger ValueType = "integer"
	TypeFloat   ValueType = "float"
	TypeText    ValueType = "text"
	TypeBlob    ValueType = "blob"

	jsonNullLiteral = "null"
	decimalBase     = 10
	int64BitSize    = 64
	float64BitSize  = 64
)

// SQLite 64-bit integers exceed IEEE-754 double precision above 2^53, so
// integer values are serialized as base-10 strings and blobs as base64.
type TypedValue struct {
	Type  ValueType `json:"type"`
	Value *string   `json:"value"`
}

func NewNullValue() TypedValue {
	return TypedValue{Type: TypeNull, Value: nil}
}

func NewIntValue(number int64) TypedValue {
	formatted := strconv.FormatInt(number, decimalBase)
	return TypedValue{Type: TypeInteger, Value: &formatted}
}

func NewFloatValue(number float64) (TypedValue, error) {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return TypedValue{}, fmt.Errorf("encode float %v: NaN and Inf are not valid JSON SQL values; use NULL or a finite float", number)
	}
	formatted := strconv.FormatFloat(number, 'g', -1, float64BitSize)
	return TypedValue{Type: TypeFloat, Value: &formatted}, nil
}

func NewTextValue(text string) (TypedValue, error) {
	if !utf8.ValidString(text) {
		return TypedValue{}, errors.New("encode text value: input contains invalid UTF-8 bytes; use blob type for binary payloads")
	}
	copied := text
	return TypedValue{Type: TypeText, Value: &copied}, nil
}

func NewBlobValue(payload []byte) TypedValue {
	encoded := base64.StdEncoding.EncodeToString(payload)
	return TypedValue{Type: TypeBlob, Value: &encoded}
}

func (tv *TypedValue) UnmarshalJSON(wireBytes []byte) error {
	var envelope struct {
		Type  ValueType       `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(wireBytes, &envelope); err != nil {
		return fmt.Errorf("unmarshal typed value %s: %w; provide a JSON object with type and value fields", string(wireBytes), err)
	}
	tv.Type = envelope.Type
	if tv.Type == TypeNull {
		if len(envelope.Value) > 0 && string(envelope.Value) != jsonNullLiteral {
			return fmt.Errorf("unmarshal null value with payload %s: null type requires value to be null or omitted", string(envelope.Value))
		}
		tv.Value = nil
		return nil
	}
	if len(envelope.Value) == 0 || string(envelope.Value) == jsonNullLiteral {
		return fmt.Errorf("unmarshal typed value of type %q: value field is missing or null; provide a string value", tv.Type)
	}
	return tv.decodeWireValue(envelope.Value)
}

func (tv *TypedValue) decodeWireValue(rawField json.RawMessage) error {
	var stringLiteral string
	if err := json.Unmarshal(rawField, &stringLiteral); err == nil {
		tv.Value = &stringLiteral
		_, err := tv.ToDriverValue()
		return err
	}
	if tv.Type == TypeInteger || tv.Type == TypeFloat {
		numericLiteral := string(rawField)
		tv.Value = &numericLiteral
		_, err := tv.ToDriverValue()
		return err
	}
	return fmt.Errorf("unmarshal value %s for type %q: expected a JSON string", string(rawField), tv.Type)
}

func (tv TypedValue) ToDriverValue() (any, error) {
	switch tv.Type {
	case TypeNull:
		return tv.parseNull()
	case TypeInteger:
		return tv.parseInteger()
	case TypeFloat:
		return tv.parseFloat()
	case TypeText:
		return tv.parseText()
	case TypeBlob:
		return tv.parseBlob()
	default:
		return nil, fmt.Errorf("unsupported value type %q: use null, integer, float, text, or blob", tv.Type)
	}
}

func (tv TypedValue) parseNull() (any, error) {
	if tv.Value != nil {
		return nil, fmt.Errorf("invalid null value %q: null type must have a nil value", *tv.Value)
	}
	return nil, nil
}

func (tv TypedValue) parseInteger() (int64, error) {
	if tv.Value == nil {
		return 0, errors.New("parse integer value: value is nil; provide a base-10 int64 string")
	}
	parsed, err := strconv.ParseInt(*tv.Value, decimalBase, int64BitSize)
	if err != nil {
		return 0, fmt.Errorf("parse integer %q: %w; provide a valid 64-bit integer string", *tv.Value, err)
	}
	return parsed, nil
}

func (tv TypedValue) parseFloat() (float64, error) {
	if tv.Value == nil {
		return 0, errors.New("parse float value: value is nil; provide a finite float64 string")
	}
	parsed, err := strconv.ParseFloat(*tv.Value, float64BitSize)
	if err != nil {
		return 0, fmt.Errorf("parse float %q: %w; provide a valid float64 string", *tv.Value, err)
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("parse float %q: NaN and Inf are not supported; use null or a finite number", *tv.Value)
	}
	return parsed, nil
}

func (tv TypedValue) parseText() (string, error) {
	if tv.Value == nil {
		return "", errors.New("parse text value: value is nil; provide a UTF-8 string")
	}
	if !utf8.ValidString(*tv.Value) {
		return "", errors.New("parse text value: string contains invalid UTF-8; encode binary data as blob")
	}
	return *tv.Value, nil
}

func (tv TypedValue) parseBlob() ([]byte, error) {
	if tv.Value == nil {
		return nil, errors.New("parse blob value: value is nil; provide a base64-encoded string")
	}
	decoded, err := base64.StdEncoding.DecodeString(*tv.Value)
	if err != nil {
		return nil, fmt.Errorf("decode base64 blob %q: %w; provide standard base64 encoding", *tv.Value, err)
	}
	return decoded, nil
}

func FromDriverValue(columnValue any) (TypedValue, error) {
	switch typed := columnValue.(type) {
	case nil:
		return NewNullValue(), nil
	case int64:
		return NewIntValue(typed), nil
	case int:
		return NewIntValue(int64(typed)), nil
	case float64:
		return NewFloatValue(typed)
	case bool:
		return boolToIntValue(typed), nil
	case string:
		if !utf8.ValidString(typed) {
			return NewBlobValue([]byte(typed)), nil
		}
		return NewTextValue(typed)
	case []byte:
		if typed == nil {
			return NewNullValue(), nil
		}
		return NewBlobValue(typed), nil
	default:
		return NewTextValue(fmt.Sprintf("%v", typed))
	}
}

func boolToIntValue(flag bool) TypedValue {
	if flag {
		return NewIntValue(1)
	}
	return NewIntValue(0)
}

func (tv TypedValue) ToPlainValue() (any, error) {
	switch tv.Type {
	case TypeNull:
		return tv.parseNull()
	case TypeInteger:
		return tv.parseInteger()
	case TypeFloat:
		return tv.parseFloat()
	case TypeText:
		return tv.parseText()
	case TypeBlob:
		if tv.Value == nil {
			return nil, errors.New("plain blob value is nil; provide a base64 string")
		}
		return *tv.Value, nil
	default:
		return nil, fmt.Errorf("unsupported value type %q: use null, integer, float, text, or blob", tv.Type)
	}
}

func ConvertParamsToDriverArgs(params []TypedValue) ([]any, error) {
	if len(params) == 0 {
		return nil, nil
	}
	driverArgs := make([]any, len(params))
	for idx, param := range params {
		converted, err := param.ToDriverValue()
		if err != nil {
			return nil, fmt.Errorf("invalid query parameter at index %d: %w", idx, err)
		}
		driverArgs[idx] = converted
	}
	return driverArgs, nil
}
