package runner

import (
	"testing"
)

func TestControllerLeaseKeyStableAndProviderScoped(t *testing.T) {
	first := controllerLeaseKey("local-docker:scope-a")
	if first != controllerLeaseKey("local-docker:scope-a") {
		t.Fatal("controller lease key is not stable")
	}
	if first == controllerLeaseKey("local-docker:scope-b") {
		t.Fatal("different scopes share a controller lease key")
	}
	if first == controllerLeaseKey("home-proxmox:scope-a") {
		t.Fatal("different providers share a controller lease key")
	}
}
