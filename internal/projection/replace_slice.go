package projection

import (
	"fmt"
)

// ReplaceSlice replaces the Nth (1-based) match of the given slice kind
// with `replacement` verbatim. The rest of the body is preserved byte-
// exact.
//
// Byte-exact PUTGET: for a well-formed (body, kind, index, replacement),
// the returned string is exactly body[:s.StartOff] + replacement +
// body[s.EndOff:] where s = Slices(body, kind)[index-1].
//
// v1 limitations:
//   - Comments inside the replaced range are discarded silently. Callers
//     that need comment preservation should splice via Slices() directly
//     and reconstruct the surrounding text themselves.
//   - Callers must supply properly-indented replacement bytes;
//     ReplaceSlice performs no formatting adjustment.
func ReplaceSlice(body, kind string, index int, replacement string) (string, error) {
	if body == "" {
		return "", fmt.Errorf("replace-slice: body is empty")
	}
	if index < 1 {
		return "", fmt.Errorf("replace-slice: index must be >= 1 (1-based), got %d", index)
	}
	slices, err := Slices(body, kind)
	if err != nil {
		return "", err
	}
	if len(slices) == 0 {
		return "", fmt.Errorf("replace-slice: no %s slices found in body", kind)
	}
	if index > len(slices) {
		return "", fmt.Errorf("replace-slice: index %d exceeds %d match(es)", index, len(slices))
	}
	t := slices[index-1]
	return body[:t.StartOff] + replacement + body[t.EndOff:], nil
}
