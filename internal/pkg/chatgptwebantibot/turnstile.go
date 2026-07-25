package antibot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// SolveTurnstileToken executes ChatGPT's compact Turnstile bytecode and
// returns its base64 token. dx is the base64-encoded, XOR-obfuscated program;
// p is the requirements token used as its XOR key. It has no network or
// browser dependency.
func SolveTurnstileToken(dx, p string) (string, error) {
	encoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		return "", fmt.Errorf("decode turnstile program: %w", err)
	}
	var tokens [][]any
	if err := json.Unmarshal([]byte(xorString(string(encoded), p)), &tokens); err != nil {
		return "", fmt.Errorf("parse turnstile program: %w", err)
	}
	vm := newTurnstileVM(tokens, p)
	for _, token := range tokens {
		// Upstream programs can contain optional browser branches. Match the
		// Python implementation: an unsupported branch is skipped instead of
		// invalidating the complete challenge.
		_ = vm.execute(token)
	}
	if vm.result == "" {
		return "", fmt.Errorf("turnstile program produced no token")
	}
	return vm.result, nil
}

type turnstileFunc func([]any) error

type turnstileVM struct {
	values map[int]any
	result string
	start  time.Time
}

func newTurnstileVM(tokens [][]any, key string) *turnstileVM {
	vm := &turnstileVM{
		values: map[int]any{
			9:  tokens,
			10: "window",
			16: key,
		},
		start: time.Now(),
	}
	vm.values[1] = turnstileFunc(vm.opXOR)
	vm.values[2] = turnstileFunc(vm.opSet)
	vm.values[3] = turnstileFunc(vm.opResult)
	vm.values[5] = turnstileFunc(vm.opAppend)
	vm.values[6] = turnstileFunc(vm.opJoinURL)
	vm.values[7] = turnstileFunc(vm.opCallValues)
	vm.values[8] = turnstileFunc(vm.opCopy)
	vm.values[14] = turnstileFunc(vm.opJSONParse)
	vm.values[15] = turnstileFunc(vm.opJSONStringify)
	vm.values[17] = turnstileFunc(vm.opBrowserCall)
	vm.values[18] = turnstileFunc(vm.opBase64Decode)
	vm.values[19] = turnstileFunc(vm.opBase64Encode)
	vm.values[20] = turnstileFunc(vm.opCallWhenEqual)
	vm.values[21] = turnstileFunc(func([]any) error { return nil })
	vm.values[23] = turnstileFunc(vm.opCallRaw)
	vm.values[24] = turnstileFunc(vm.opJoin)
	return vm
}

func (s *turnstileVM) execute(token []any) error {
	if len(token) == 0 {
		return nil
	}
	op, ok := toIndex(token[0])
	if !ok {
		return fmt.Errorf("turnstile opcode is not a number")
	}
	fn, ok := s.values[op].(turnstileFunc)
	if !ok {
		return nil
	}
	return fn(token[1:])
}

func (s *turnstileVM) opXOR(args []any) error {
	dest, left, right, err := threeIndexes(args)
	if err != nil {
		return err
	}
	s.values[dest] = xorString(turnstileString(s.values[left]), turnstileString(s.values[right]))
	return nil
}

func (s *turnstileVM) opSet(args []any) error {
	if len(args) != 2 {
		return fmt.Errorf("set requires two arguments")
	}
	dest, ok := toIndex(args[0])
	if !ok {
		return fmt.Errorf("set destination is invalid")
	}
	s.values[dest] = args[1]
	return nil
}

func (s *turnstileVM) opResult(args []any) error {
	if len(args) != 1 {
		return fmt.Errorf("result requires one argument")
	}
	value, ok := s.value(args[0])
	if !ok {
		// func_3 can be called directly with a literal in addition to an index.
		value = args[0]
	}
	s.result = base64.StdEncoding.EncodeToString([]byte(turnstileString(value)))
	return nil
}

func (s *turnstileVM) opAppend(args []any) error {
	dest, currentID, incomingID, err := threeIndexes(args)
	if err != nil {
		return err
	}
	current, incoming := s.values[currentID], s.values[incomingID]
	if values, ok := current.([]any); ok {
		s.values[dest] = append(append([]any{}, values...), incoming)
		return nil
	}
	if _, ok := current.(string); ok {
		s.values[dest] = turnstileString(current) + turnstileString(incoming)
		return nil
	}
	if _, ok := current.(float64); ok {
		s.values[dest] = turnstileString(current) + turnstileString(incoming)
		return nil
	}
	if _, ok := incoming.(string); ok {
		s.values[dest] = turnstileString(current) + turnstileString(incoming)
		return nil
	}
	if _, ok := incoming.(float64); ok {
		s.values[dest] = turnstileString(current) + turnstileString(incoming)
		return nil
	}
	s.values[dest] = "NaN"
	return nil
}

func (s *turnstileVM) opJoinURL(args []any) error {
	dest, left, right, err := threeIndexes(args)
	if err != nil {
		return err
	}
	leftValue, leftOK := s.values[left].(string)
	rightValue, rightOK := s.values[right].(string)
	if !leftOK || !rightOK {
		return nil
	}
	joined := leftValue + "." + rightValue
	if joined == "window.document.location" {
		joined = "https://chatgpt.com/"
	}
	s.values[dest] = joined
	return nil
}

func (s *turnstileVM) opCallValues(args []any) error {
	if len(args) < 1 {
		return fmt.Errorf("call requires a target")
	}
	targetID, ok := toIndex(args[0])
	if !ok {
		return fmt.Errorf("call target is invalid")
	}
	target := s.values[targetID]
	values := make([]any, 0, len(args)-1)
	for _, arg := range args[1:] {
		value, ok := s.value(arg)
		if !ok {
			return fmt.Errorf("call argument is invalid")
		}
		values = append(values, value)
	}
	if targetName, ok := target.(string); ok && targetName == "window.Reflect.set" {
		if len(values) != 3 {
			return fmt.Errorf("Reflect.set requires three values")
		}
		object, ok := values[0].(*orderedMap)
		if !ok {
			return fmt.Errorf("Reflect.set receiver is invalid")
		}
		object.add(turnstileString(values[1]), values[2])
		return nil
	}
	if fn, ok := target.(turnstileFunc); ok {
		return fn(values)
	}
	return nil
}

func (s *turnstileVM) opCopy(args []any) error {
	dest, source, err := twoIndexes(args)
	if err != nil {
		return err
	}
	s.values[dest] = s.values[source]
	return nil
}

func (s *turnstileVM) opJSONParse(args []any) error {
	dest, source, err := twoIndexes(args)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal([]byte(turnstileString(s.values[source])), &value); err != nil {
		return err
	}
	s.values[dest] = value
	return nil
}

func (s *turnstileVM) opJSONStringify(args []any) error {
	dest, source, err := twoIndexes(args)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(s.values[source])
	if err != nil {
		return err
	}
	s.values[dest] = string(encoded)
	return nil
}

func (s *turnstileVM) opBrowserCall(args []any) error {
	if len(args) < 2 {
		return fmt.Errorf("browser call requires destination and target")
	}
	dest, ok := toIndex(args[0])
	if !ok {
		return fmt.Errorf("browser destination is invalid")
	}
	target, ok := s.value(args[1])
	if !ok {
		return fmt.Errorf("browser target is invalid")
	}
	values := make([]any, 0, len(args)-2)
	for _, arg := range args[2:] {
		value, ok := s.value(arg)
		if !ok {
			return fmt.Errorf("browser argument is invalid")
		}
		values = append(values, value)
	}
	switch target {
	case "window.performance.now":
		s.values[dest] = float64(time.Since(s.start).Nanoseconds())/1e6 + rand.Float64()/1e6
	case "window.Object.create":
		s.values[dest] = &orderedMap{}
	case "window.Object.keys":
		if len(values) > 0 && values[0] == "window.localStorage" {
			s.values[dest] = []any{
				"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
				"STATSIG_LOCAL_STORAGE_STABLE_ID",
				"client-correlated-secret",
				"oai/apps/capExpiresAt",
				"oai-did",
				"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
				"UiState.isNavigationCollapsed.1",
			}
		}
	case "window.Math.random":
		s.values[dest] = rand.Float64()
	default:
		if fn, ok := target.(turnstileFunc); ok {
			return fn(values)
		}
	}
	return nil
}

func (s *turnstileVM) opBase64Decode(args []any) error {
	dest, source, err := twoIndexes(args)
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(turnstileString(s.values[source]))
	if err != nil {
		return err
	}
	s.values[dest] = string(decoded)
	return nil
}

func (s *turnstileVM) opBase64Encode(args []any) error {
	dest, source, err := twoIndexes(args)
	if err != nil {
		return err
	}
	s.values[dest] = base64.StdEncoding.EncodeToString([]byte(turnstileString(s.values[source])))
	return nil
}

func (s *turnstileVM) opCallWhenEqual(args []any) error {
	if len(args) < 3 {
		return fmt.Errorf("conditional call requires three arguments")
	}
	left, ok := s.value(args[0])
	if !ok {
		return fmt.Errorf("conditional left is invalid")
	}
	right, ok := s.value(args[1])
	if !ok || !equalValues(left, right) {
		return nil
	}
	target, ok := s.value(args[2])
	if !ok {
		return fmt.Errorf("conditional target is invalid")
	}
	fn, ok := target.(turnstileFunc)
	if !ok {
		return nil
	}
	values := make([]any, 0, len(args)-3)
	for _, arg := range args[3:] {
		value, ok := s.value(arg)
		if !ok {
			return fmt.Errorf("conditional argument is invalid")
		}
		values = append(values, value)
	}
	return fn(values)
}

func (s *turnstileVM) opCallRaw(args []any) error {
	if len(args) < 2 {
		return fmt.Errorf("raw call requires two arguments")
	}
	guard, ok := s.value(args[0])
	if !ok || guard == nil {
		return nil
	}
	target, ok := s.value(args[1])
	if !ok {
		return fmt.Errorf("raw call target is invalid")
	}
	fn, ok := target.(turnstileFunc)
	if !ok {
		return nil
	}
	return fn(args[2:])
}

func (s *turnstileVM) opJoin(args []any) error {
	dest, left, right, err := threeIndexes(args)
	if err != nil {
		return err
	}
	leftValue, leftOK := s.values[left].(string)
	rightValue, rightOK := s.values[right].(string)
	if leftOK && rightOK {
		s.values[dest] = leftValue + "." + rightValue
	}
	return nil
}

func (s *turnstileVM) value(input any) (any, bool) {
	id, ok := toIndex(input)
	if !ok {
		return nil, false
	}
	value, found := s.values[id]
	return value, found
}

type orderedMap struct {
	keys   []string
	values map[string]any
}

func (s *orderedMap) add(key string, value any) {
	if s.values == nil {
		s.values = map[string]any{}
	}
	if _, exists := s.values[key]; !exists {
		s.keys = append(s.keys, key)
	}
	s.values[key] = value
}

func xorString(text, key string) string {
	if key == "" {
		return text
	}
	textRunes := []rune(text)
	keyRunes := []rune(key)
	for i := range textRunes {
		textRunes[i] ^= keyRunes[i%len(keyRunes)]
	}
	return string(textRunes)
}

func turnstileString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return turnstileSpecialString(typed)
	case float64:
		out := strconv.FormatFloat(typed, 'g', -1, 64)
		if !strings.ContainsAny(out, ".eE") {
			return out + ".0"
		}
		return out
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case []any:
		stringsOnly := make([]string, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return fmt.Sprint(value)
			}
			stringsOnly[i] = text
		}
		return strings.Join(stringsOnly, ",")
	default:
		return fmt.Sprint(value)
	}
}

func turnstileSpecialString(value string) string {
	special := map[string]string{
		"window.Math":            "[object Math]",
		"window.Reflect":         "[object Reflect]",
		"window.performance":     "[object Performance]",
		"window.localStorage":    "[object Storage]",
		"window.Object":          "function Object() { [native code] }",
		"window.Reflect.set":     "function set() { [native code] }",
		"window.performance.now": "function () { [native code] }",
		"window.Object.create":   "function create() { [native code] }",
		"window.Object.keys":     "function keys() { [native code] }",
		"window.Math.random":     "function random() { [native code] }",
	}
	if out, ok := special[value]; ok {
		return out
	}
	return value
}

func twoIndexes(args []any) (int, int, error) {
	if len(args) != 2 {
		return 0, 0, fmt.Errorf("expected two indexes")
	}
	first, ok := toIndex(args[0])
	if !ok {
		return 0, 0, fmt.Errorf("first index is invalid")
	}
	second, ok := toIndex(args[1])
	if !ok {
		return 0, 0, fmt.Errorf("second index is invalid")
	}
	return first, second, nil
}

func threeIndexes(args []any) (int, int, int, error) {
	if len(args) != 3 {
		return 0, 0, 0, fmt.Errorf("expected three indexes")
	}
	first, second, err := twoIndexes(args[:2])
	if err != nil {
		return 0, 0, 0, err
	}
	third, ok := toIndex(args[2])
	if !ok {
		return 0, 0, 0, fmt.Errorf("third index is invalid")
	}
	return first, second, third, nil
}

func toIndex(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func equalValues(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
