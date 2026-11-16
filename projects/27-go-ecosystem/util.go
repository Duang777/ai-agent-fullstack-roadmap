package goeco

import "encoding/json"

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
