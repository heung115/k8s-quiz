package container

import (
	"testing"
)

func TestNewDockerManager(t *testing.T) {
	mgr, err := NewDockerManager("unix:///var/run/docker.sock")
	if err != nil {
		t.Fatalf("NewDockerManager failed: %v", err)
	}
	if mgr.cli == nil {
		t.Fatal("expected docker client to be initialized")
	}
}

func TestNewDockerManagerDefaultHost(t *testing.T) {
	mgr, err := NewDockerManager("")
	if err != nil {
		t.Fatalf("NewDockerManager with default host failed: %v", err)
	}
	if mgr.cli == nil {
		t.Fatal("expected docker client to be initialized")
	}
}
