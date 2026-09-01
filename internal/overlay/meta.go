package overlay

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// objectMeta is persisted for Stat/List without decrypting blobs.
type objectMeta struct {
	Hash         string    `json:"hash,omitempty"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ContentType  string    `json:"content_type,omitempty"`
}

func encodeMeta(m objectMeta) ([]byte, error) {
	return json.Marshal(m)
}

func parseMeta(raw []byte) (objectMeta, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return objectMeta{}, fmt.Errorf("empty meta")
	}
	if !strings.HasPrefix(s, "{") {
		return objectMeta{Hash: s}, nil
	}
	var m objectMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return objectMeta{}, err
	}
	return m, nil
}
