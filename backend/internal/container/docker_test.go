package container

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	containerapi "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
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

func TestDockerResolveImageReturnsContentIDAndPropagatesErrors(t *testing.T) {
	want := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		if req.Method != http.MethodGet || !strings.HasPrefix(path, "/images/") || !strings.HasSuffix(path, "/json") {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if strings.Contains(path, "missing") {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		writeJSON(t, w, map[string]any{"Id": want})
	}))
	defer server.Close()

	mgr := newDockerAPIManager(t, server)
	got, err := mgr.ResolveImage(context.Background(), "k3s-base:latest")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveImage = %q, want %q", got, want)
	}
	if _, err := mgr.ResolveImage(context.Background(), "missing:latest"); err == nil || !strings.Contains(err.Error(), "inspect image") {
		t.Fatalf("missing image error = %v", err)
	}
}

func TestDockerCreateAdoptsExactResourceAfterCommittedResponseLoss(t *testing.T) {
	opts := ambiguousCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const (
		containerID = "container-committed"
		networkID   = "network-committed"
	)
	var networkCreated, containerCommitted bool
	var startCalls, deleteCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			if !networkCreated {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, network.Inspect{
				Network: network.Network{Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels},
			})
		case req.Method == http.MethodPost && path == "/networks/create":
			networkCreated = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, network.CreateResponse{ID: networkID})
		case req.Method == http.MethodPost && path == "/containers/create":
			containerCommitted = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":`)) // daemon committed; response was truncated in transit
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			if !containerCommitted {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, networkID))
		case req.Method == http.MethodPost && path == "/containers/"+containerID+"/start":
			startCalls++
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	mgr := newDockerAPIManager(t, server)
	handle, err := mgr.CreateOwnedAllocation(context.Background(), opts)
	if err != nil {
		t.Fatalf("create did not recover committed response loss: %v", err)
	}
	if handle.ContainerID != containerID || handle.NetworkID != networkID {
		t.Fatalf("adopted allocation handle = %+v", handle)
	}
	if startCalls != 1 || deleteCalls != 0 {
		t.Fatalf("recovery start/delete calls = %d/%d, want 1/0", startCalls, deleteCalls)
	}
}

func TestDockerCreateAdoptsExactResourceAfterEmptyContainerID(t *testing.T) {
	opts := ambiguousCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const (
		containerID = "container-empty-id-committed"
		networkID   = "network-empty-id-committed"
	)
	var networkCreated, containerCommitted bool
	var startCalls, deleteCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			if !networkCreated {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodPost && path == "/networks/create":
			networkCreated = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, network.CreateResponse{ID: networkID})
		case req.Method == http.MethodPost && path == "/containers/create":
			containerCommitted = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{"Id": "", "Warnings": []string{}})
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			if !containerCommitted {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, networkID))
		case req.Method == http.MethodPost && path == "/containers/"+containerID+"/start":
			startCalls++
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	handle, err := newDockerAPIManager(t, server).CreateOwnedAllocation(context.Background(), opts)
	if err != nil {
		t.Fatalf("create did not recover empty container id: %v", err)
	}
	if handle != (OwnedAllocationHandle{ContainerID: containerID, NetworkID: networkID}) {
		t.Fatalf("recovered allocation handle = %+v", handle)
	}
	if startCalls != 1 || deleteCalls != 0 {
		t.Fatalf("empty-id recovery start/delete calls = %d/%d, want 1/0", startCalls, deleteCalls)
	}
}

func TestDockerCreateRefusesReplacementNetworkDuringAmbiguousRecovery(t *testing.T) {
	opts := ambiguousCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const (
		containerID          = "container-network-replaced"
		capturedNetworkID    = "network-captured"
		replacementNetworkID = "network-replacement"
	)
	var networkCreated, containerCommitted bool
	var startCalls, deleteCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			if !networkCreated {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: replacementNetworkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodPost && path == "/networks/create":
			networkCreated = true
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, network.CreateResponse{ID: capturedNetworkID})
		case req.Method == http.MethodPost && path == "/containers/create":
			containerCommitted = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":`))
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			if !containerCommitted {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, replacementNetworkID))
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			startCalls++
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	if _, err := newDockerAPIManager(t, server).CreateOwnedAllocation(context.Background(), opts); err == nil ||
		!strings.Contains(err.Error(), "network replacement") {
		t.Fatalf("network replacement recovery error = %v", err)
	}
	if startCalls != 0 || deleteCalls != 0 {
		t.Fatalf("replacement network was mutated: start/delete calls = %d/%d", startCalls, deleteCalls)
	}
}

func TestDockerCreateRefusesMismatchedResourceAfterResponseLoss(t *testing.T) {
	opts := ambiguousCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedLabels := mapsClone(labels)
	mismatchedLabels["k8s-quiz.allocation"] = "someone-else"
	const (
		containerID = "container-conflict"
		networkID   = "network-conflict"
	)
	var startCalls, deleteCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			writeJSON(t, w, network.Inspect{
				Network: network.Network{Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels},
			})
		case req.Method == http.MethodPost && path == "/containers/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":`))
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			writeJSON(t, w, recoveredContainerJSON(opts, mismatchedLabels, containerID, networkID))
		case req.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			startCalls++
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	mgr := newDockerAPIManager(t, server)
	if _, err := mgr.CreateOwnedAllocation(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "refuse ambiguous container adoption") {
		t.Fatalf("create error = %v, want ownership mismatch refusal", err)
	}
	if startCalls != 0 || deleteCalls != 0 {
		t.Fatalf("mismatched resource was mutated: start/delete calls = %d/%d", startCalls, deleteCalls)
	}
}

func TestDockerGenericCreateStartFailureCleansExactContainerAndNetworkIDs(t *testing.T) {
	opts := ambiguousCreateOpts()
	const (
		containerID = "container-failed-start"
		networkID   = "network-failed-start"
	)
	var deleted []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			http.Error(w, "not found", http.StatusNotFound)
		case req.Method == http.MethodPost && path == "/networks/create":
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, network.CreateResponse{ID: networkID})
		case req.Method == http.MethodPost && path == "/containers/create":
			w.WriteHeader(http.StatusCreated)
			writeJSON(t, w, map[string]any{"Id": containerID, "Warnings": []string{}})
		case req.Method == http.MethodPost && path == "/containers/"+containerID+"/start":
			http.Error(w, "start failed", http.StatusInternalServerError)
		case req.Method == http.MethodDelete && path == "/containers/"+containerID:
			deleted = append(deleted, path)
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete && path == "/networks/"+networkID:
			deleted = append(deleted, path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	if _, err := newDockerAPIManager(t, server).Create(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "container start") {
		t.Fatalf("generic create error = %v, want start failure", err)
	}
	if len(deleted) != 2 || deleted[0] != "/containers/"+containerID || deleted[1] != "/networks/"+networkID {
		t.Fatalf("generic start cleanup deleted %v, want exact container then network IDs", deleted)
	}
}

func TestValidateRecoveredContainerRejectsUnexpectedHostAuthority(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const containerID = "container-authority"
	const networkID = "network-authority"

	tests := []struct {
		name   string
		mutate func(*containerapi.HostConfig)
	}{
		{name: "capability", mutate: func(host *containerapi.HostConfig) { host.CapAdd = []string{"SYS_ADMIN"} }},
		{name: "device", mutate: func(host *containerapi.HostConfig) {
			host.Devices = []containerapi.DeviceMapping{{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm"}}
		}},
		{name: "device request", mutate: func(host *containerapi.HostConfig) {
			host.DeviceRequests = []containerapi.DeviceRequest{{Driver: "nvidia", Count: -1}}
		}},
		{name: "security option", mutate: func(host *containerapi.HostConfig) { host.SecurityOpt = []string{"seccomp=unconfined"} }},
		{name: "restart policy", mutate: func(host *containerapi.HostConfig) {
			host.RestartPolicy = containerapi.RestartPolicy{Name: containerapi.RestartPolicyAlways}
		}},
		{name: "readonly rootfs mismatch", mutate: func(host *containerapi.HostConfig) { host.ReadonlyRootfs = true }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := recoveredContainerJSON(opts, labels, containerID, networkID)
			tt.mutate(info.HostConfig)
			if err := validateRecoveredContainer(info, opts, labels); err == nil {
				t.Fatal("container with unexpected host authority was accepted")
			}
		})
	}
}

func TestValidateRecoveredContainerAcceptsDockerPrivateNamespaceDefaults(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	info := recoveredContainerJSON(opts, labels, "container-defaults", "network-defaults")
	info.HostConfig.PidMode = "private"
	info.HostConfig.IpcMode = containerapi.IPCModePrivate
	info.HostConfig.UTSMode = "private"
	info.HostConfig.UsernsMode = "private"
	info.HostConfig.Runtime = "runc"

	if err := validateRecoveredContainer(info, opts, labels); err != nil {
		t.Fatalf("Docker private namespace defaults were rejected: %v", err)
	}
}

func TestDockerRemoveOwnedAllocationRefusesAmbiguousEvidenceWithoutMutation(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const containerID = "container-ambiguous"
	const networkID = "network-ambiguous"
	mismatched := recoveredContainerJSON(opts, labels, containerID, networkID)
	mismatched.Config.Image = "sha256:" + strings.Repeat("f", 64)
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			writeJSON(t, w, mismatched)
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), OwnedAllocationTarget{Create: opts})
	if !errors.Is(err, ErrOwnedAllocationAmbiguous) {
		t.Fatalf("ambiguous ownership error = %v", err)
	}
	if !removal.Before.ContainerPresent || removal.Before.OwnershipComplete {
		t.Fatalf("ambiguous observation = %+v", removal.Before)
	}
	if deleteCalls != 0 {
		t.Fatalf("ambiguous ownership caused %d delete call(s)", deleteCalls)
	}
}

func TestDockerRemoveOwnedAllocationDeletesOnlyExactlyInspectedIDs(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const containerID = "container-exact"
	const networkID = "network-exact"
	var deleted []string
	containerPresent, networkPresent := true, true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && (path == "/containers/"+opts.Name+"/json" || path == "/containers/"+containerID+"/json"):
			if !containerPresent {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, networkID))
		case req.Method == http.MethodGet && (path == "/networks/"+opts.NetworkMode || path == "/networks/"+networkID):
			if !networkPresent {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodDelete && path == "/containers/"+containerID:
			deleted = append(deleted, path)
			containerPresent = false
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodDelete && path == "/networks/"+networkID:
			deleted = append(deleted, path)
			networkPresent = false
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), OwnedAllocationTarget{Create: opts})
	if err != nil {
		t.Fatalf("remove exact ownership: %v", err)
	}
	if !removal.Before.ContainerPresent || !removal.Before.NetworkPresent || !removal.Before.OwnershipComplete ||
		removal.After.ContainerPresent || removal.After.NetworkPresent || !removal.After.OwnershipComplete {
		t.Fatalf("exact removal proof = %+v", removal)
	}
	if len(deleted) != 2 || deleted[0] != "/containers/"+containerID || deleted[1] != "/networks/"+networkID {
		t.Fatalf("deleted paths = %v", deleted)
	}
}

func TestDockerRemoveOwnedAllocationRejectsArbitraryCreateSpecDigest(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	actualLabels := mapsClone(labels)
	actualLabels[createSpecLabel] = strings.Repeat("f", 64)
	const containerID = "container-untrusted-spec"
	const networkID = "network-untrusted-spec"
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			writeJSON(t, w, recoveredContainerJSON(opts, actualLabels, containerID, networkID))
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: actualLabels,
			}})
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), OwnedAllocationTarget{Create: opts})
	if !errors.Is(err, ErrOwnedAllocationAmbiguous) {
		t.Fatalf("arbitrary create-spec error = %v", err)
	}
	if removal.Before.OwnershipComplete || !removal.Before.ContainerPresent {
		t.Fatalf("arbitrary create-spec observation = %+v", removal.Before)
	}
	if deleteCalls != 0 {
		t.Fatalf("arbitrary create-spec caused %d delete call(s)", deleteCalls)
	}
}

func TestDockerOwnedDestroyDoesNotDeleteSameNameReplacement(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const containerID = "container-original"
	const originalNetworkID = "network-original"
	const replacementNetworkID = "network-replacement"
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/containers/"+containerID+"/json":
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, originalNetworkID))
		case req.Method == http.MethodGet && path == "/networks/"+originalNetworkID:
			http.Error(w, "not found", http.StatusNotFound)
		case req.Method == http.MethodGet && path == "/networks/"+opts.NetworkMode:
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: replacementNetworkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	target := OwnedAllocationTarget{
		Handle: OwnedAllocationHandle{ContainerID: containerID, NetworkID: originalNetworkID},
		Create: opts,
	}
	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), target)
	if !errors.Is(err, ErrOwnedAllocationAmbiguous) {
		t.Fatalf("same-name replacement error = %v", err)
	}
	if removal.Before.OwnershipComplete || !removal.Before.NetworkPresent {
		t.Fatalf("same-name replacement observation = %+v", removal.Before)
	}
	if deleteCalls != 0 {
		t.Fatalf("same-name replacement caused %d delete call(s)", deleteCalls)
	}
}

func TestDockerOwnedDestroyDoesNotDeleteSameNameContainerReplacement(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const (
		originalContainerID    = "container-original"
		replacementContainerID = "container-replacement"
		networkID              = "network-original"
	)
	deleteCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && path == "/containers/"+originalContainerID+"/json":
			http.Error(w, "not found", http.StatusNotFound)
		case req.Method == http.MethodGet && path == "/containers/"+opts.Name+"/json":
			writeJSON(t, w, recoveredContainerJSON(opts, labels, replacementContainerID, networkID))
		case req.Method == http.MethodDelete:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	target := OwnedAllocationTarget{
		Handle: OwnedAllocationHandle{ContainerID: originalContainerID, NetworkID: networkID},
		Create: opts,
	}
	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), target)
	if !errors.Is(err, ErrOwnedAllocationAmbiguous) {
		t.Fatalf("same-name container replacement error = %v", err)
	}
	if removal.Before.OwnershipComplete || !removal.Before.ContainerPresent {
		t.Fatalf("same-name container replacement observation = %+v", removal.Before)
	}
	if deleteCalls != 0 {
		t.Fatalf("same-name container replacement caused %d delete call(s)", deleteCalls)
	}
}

func TestDockerOwnedDestroyRejectsDeleteSuccessWithoutAbsence(t *testing.T) {
	opts := exactOwnedCreateOpts()
	labels, err := labelsForCreate(opts)
	if err != nil {
		t.Fatal(err)
	}
	const containerID = "container-still-present"
	const networkID = "network-still-present"
	var deleted []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.47")
		switch {
		case req.Method == http.MethodGet && (path == "/containers/"+opts.Name+"/json" || path == "/containers/"+containerID+"/json"):
			writeJSON(t, w, recoveredContainerJSON(opts, labels, containerID, networkID))
		case req.Method == http.MethodGet && (path == "/networks/"+opts.NetworkMode || path == "/networks/"+networkID):
			writeJSON(t, w, network.Inspect{Network: network.Network{
				Name: opts.NetworkMode, ID: networkID, Driver: "bridge", Labels: labels,
			}})
		case req.Method == http.MethodDelete && (path == "/containers/"+containerID || path == "/networks/"+networkID):
			deleted = append(deleted, path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+req.Method+" "+path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	removal, err := newDockerAPIManager(t, server).RemoveOwnedAllocation(context.Background(), OwnedAllocationTarget{Create: opts})
	if !errors.Is(err, ErrOwnedAllocationAmbiguous) {
		t.Fatalf("persistent resource error = %v", err)
	}
	if !removal.After.OwnershipComplete || !removal.After.ContainerPresent || !removal.After.NetworkPresent {
		t.Fatalf("persistent resource proof = %+v", removal.After)
	}
	if len(deleted) != 2 {
		t.Fatalf("delete calls = %v, want both exact IDs attempted", deleted)
	}
}

func exactOwnedCreateOpts() CreateOpts {
	return CreateOpts{
		Name:  "k8s-quiz-test-allocation-1",
		Image: "sha256:" + strings.Repeat("a", 64),
		Labels: map[string]string{
			"k8s-quiz":            "true",
			"k8s-quiz.scope":      "test",
			"k8s-quiz.provider":   "local-docker",
			"k8s-quiz.allocation": "allocation-1",
			"k8s-quiz.session":    "session-1",
			"k8s-quiz.generation": "1",
		},
		CPULimit: 1_000_000_000, MemoryLimit: 1_073_741_824,
		NetworkMode: "k8s-quiz-test-allocation-1", Privileged: true,
	}
}

func ambiguousCreateOpts() CreateOpts {
	return CreateOpts{
		Name:        "k8s-quiz-test-alloc-1",
		Image:       "sha256:approved",
		Labels:      map[string]string{"k8s-quiz": "true", "k8s-quiz.scope": "test", "k8s-quiz.allocation": "alloc-1"},
		CPULimit:    1_000_000_000,
		MemoryLimit: 1_073_741_824,
		NetworkMode: "k8s-quiz-test-alloc-1",
		Privileged:  true,
	}
}

func recoveredContainerJSON(opts CreateOpts, labels map[string]string, containerID, networkID string) containerapi.InspectResponse {
	pids := int64(1024)
	return containerapi.InspectResponse{
		ID: containerID, Name: "/" + opts.Name,
		State: &containerapi.State{Status: containerapi.StateCreated},
		HostConfig: &containerapi.HostConfig{
			Privileged: opts.Privileged, CgroupnsMode: containerapi.CgroupnsModeHost,
			Resources: containerapi.Resources{NanoCPUs: opts.CPULimit, Memory: opts.MemoryLimit, PidsLimit: &pids},
		},
		Config: &containerapi.Config{Image: opts.Image, Labels: labels, Env: opts.Env, Cmd: []string{"server"}},
		NetworkSettings: &containerapi.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			opts.NetworkMode: {NetworkID: networkID},
		}},
	}
}

func newDockerAPIManager(t *testing.T, server *httptest.Server) *DockerManager {
	t.Helper()
	cli, err := client.New(
		client.WithHost(server.URL), client.WithScheme("http"), client.WithAPIVersion("1.47"), client.WithHTTPClient(server.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &DockerManager{dockerHost: server.URL, cli: cli}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode Docker API response: %v", err)
	}
}

func mapsClone(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
