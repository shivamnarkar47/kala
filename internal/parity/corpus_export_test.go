package parity

import (
	"encoding/json"
	"os"
	"testing"
)

// TestExportCorpus writes the corpus JSON for manual driver debugging.
func TestExportCorpus(t *testing.T) {
	if os.Getenv("KAAL_EXPORT_CORPUS") == "" {
		t.Skip("set KAAL_EXPORT_CORPUS to export")
	}
	b, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/corpus.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
}
