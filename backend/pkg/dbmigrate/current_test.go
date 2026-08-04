package dbmigrate

import (
	"testing"
	"testing/fstest"
)

func TestLatestVersion(t *testing.T) {
	files := fstest.MapFS{
		"001_init.up.sql":   {Data: []byte("SELECT 1")},
		"001_init.down.sql": {Data: []byte("SELECT 1")},
		"014_proof.up.sql":  {Data: []byte("SELECT 1")},
		"README":            {Data: nil},
	}
	got, err := latestVersion(files, ".")
	if err != nil || got != 14 {
		t.Fatalf("latestVersion=%d,%v", got, err)
	}
}
