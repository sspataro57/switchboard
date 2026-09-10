package classify

import (
	"bytes"
	"encoding/json"
	"errors"
)

// DuplicateLabelKey reports the first key repeated in one JSON object — one
// line of a labelled-set file — or "" when every key is unique.
//
// encoding/json keeps only the LAST value of a repeated key, so a line such as
// {"note":"<a client's message>","note":"<the allowed marker>",...} would pass
// every closed-vocabulary check while the first value sat in git (Codex
// adversarial review, round 10). The keys are compared DECODED, so a key with
// one of its letters written as a JSON unicode escape (a backslash-u sequence)
// is a repeat of its plain spelling too. One spelling, called by
// cmd/classify's loader and by the committed-file structure test.
func DuplicateLabelKey(line []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", errors.New("a label line must be one JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return "", err
		}
		k, _ := kt.(string)
		if seen[k] {
			return k, nil
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return "", err
		}
	}
	return "", nil
}
