package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/runtimecontrol"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

// TestGatewayHostRegistrationCopiesIdentityFields varies every field of the
// host instance identity.
//
//   - Kind: hostInstancesFromDescriptors copies DriverID. Replacing
//     family.alpha with family.beta changes Kind and leaves Enabled.
//   - Instance: gatewayHostConfig copies Instance, and an empty key becomes
//     primary. Replacing sample.one with sample.two changes only that key.
//   - Enabled: hostInstancesFromDescriptors copies Disposition. Replacing
//     enabled with disabled changes Enabled and leaves Kind.
//   - Transport: gatewayHostConfig copies the transport document. Replacing
//     the east document with the west document changes Transport and leaves
//     Options.
//   - Options: gatewayHostConfig copies the options document. Replacing the
//     first document with the second changes Options and leaves Transport.
//   - StateDir: gatewayHostConfig uses the directory of RuntimeStatePath.
//     An empty path selects /data. A file path selects its parent, not the
//     file and not a sibling directory.
func TestGatewayHostRegistrationCopiesIdentityFields(t *testing.T) {
	enabled := hostInstancesFromDescriptors([]runtimecontrol.Descriptor{{
		DriverID: "family.alpha", Disposition: runtimecontrol.DispositionEnabled,
	}})
	renamed := hostInstancesFromDescriptors([]runtimecontrol.Descriptor{{
		DriverID: "family.beta", Disposition: runtimecontrol.DispositionEnabled,
	}})
	disabled := hostInstancesFromDescriptors([]runtimecontrol.Descriptor{{
		DriverID: "family.alpha", Disposition: runtimecontrol.DispositionDisabled,
	}})
	if enabled[0].Kind != "family.alpha" || enabled[0].Enabled != true || enabled[0].Instance != "primary" {
		t.Fatalf("enabled identity = %+v", enabled[0])
	}
	if renamed[0].Kind != "family.beta" || renamed[0].Enabled != true {
		t.Fatalf("renamed identity = %+v; Kind must follow DriverID and Enabled must stay", renamed[0])
	}
	if disabled[0].Kind != "family.alpha" || disabled[0].Enabled != false {
		t.Fatalf("disabled identity = %+v; Enabled must follow Disposition and Kind must stay", disabled[0])
	}

	east := json.RawMessage(`{"lane":"east"}`)
	west := json.RawMessage(`{"lane":"west"}`)
	firstOptions := json.RawMessage(`{"limit":1}`)
	secondOptions := json.RawMessage(`{"limit":2}`)
	withEast := gatewayHostConfig("", []gatewayHostInstance{{
		Kind: "family.sample", Instance: "sample.one", Enabled: true, Transport: east, Options: firstOptions,
	}})
	withWest := gatewayHostConfig("", []gatewayHostInstance{{
		Kind: "family.sample", Instance: "sample.two", Enabled: true, Transport: west, Options: firstOptions,
	}})
	withSecondOptions := gatewayHostConfig("", []gatewayHostInstance{{
		Kind: "family.sample", Instance: "sample.one", Enabled: true, Transport: east, Options: secondOptions,
	}})
	if withEast.Instances[0].Instance != "sample.one" || withWest.Instances[0].Instance != "sample.two" || withEast.Instances[0].Kind != withWest.Instances[0].Kind {
		t.Fatalf("instance key did not change alone: %+v %+v", withEast.Instances[0], withWest.Instances[0])
	}
	if !bytesEqual(withEast.Instances[0].Transport, east) || bytesEqual(withEast.Instances[0].Transport, withWest.Instances[0].Transport) || !bytesEqual(withEast.Instances[0].Options, withWest.Instances[0].Options) {
		t.Fatalf("transport document did not change alone: %+v %+v", withEast.Instances[0], withWest.Instances[0])
	}
	if !bytesEqual(withSecondOptions.Instances[0].Options, secondOptions) || bytesEqual(withSecondOptions.Instances[0].Options, withEast.Instances[0].Options) || !bytesEqual(withSecondOptions.Instances[0].Transport, withEast.Instances[0].Transport) {
		t.Fatalf("options document did not change alone: %+v %+v", withEast.Instances[0], withSecondOptions.Instances[0])
	}
	filled := gatewayHostConfig("", []gatewayHostInstance{{Kind: "family.sample", Enabled: true}})
	if filled.Instances[0].Instance != "primary" {
		t.Fatalf("empty instance key = %q; want primary", filled.Instances[0].Instance)
	}

	if got := gatewayHostConfig("", nil).StateDir; got != "/data" {
		t.Fatalf("empty runtime state path directory = %q; want /data", got)
	}
	dir := t.TempDir()
	sibling := filepath.Join(filepath.Dir(dir), "other-state")
	path := filepath.Join(dir, "runtime_state.json")
	if got := gatewayHostConfig(path, nil).StateDir; got != dir || got == path || got == sibling {
		t.Fatalf("state directory = %q; want the parent %q of the runtime state file", got, dir)
	}
}

func bytesEqual(left, right []byte) bool { return string(left) == string(right) }

// TestGatewayHostRegistrationStartsTwoInstancesOfOneKind varies the instance
// key of one driver kind. Planning only the first key would leave sample.two
// unmatched and out of the plan.
func TestGatewayHostRegistrationStartsTwoInstancesOfOneKind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	kind := "family.sample"
	instances := []gatewayHostInstance{
		{Kind: kind, Instance: "sample.one", Enabled: true},
		{Kind: kind, Instance: "sample.two", Enabled: true},
	}
	opened, err := openGatewayHost(ctx, filepath.Join(dir, "runtime_state.json"), instances, gatewayHostTestFactory{kind: driver.DriverKind(kind)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			t.Errorf("close host: %v", err)
		}
	})
	if unmatched := opened.Unmatched(); len(unmatched) != 0 {
		t.Fatalf("unmatched = %v; both instances have a factory", unmatched)
	}
	if err := opened.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"family.sample/sample.one", "family.sample/sample.two"}
	if got := opened.planned(); !slices.Equal(got, want) {
		t.Fatalf("planned = %v; want %v", got, want)
	}
}

// TestGatewayHostRegistrationLeavesEnabledOptionsUnmatched varies Enabled.
// An enabled option with no factory is unmatched and is not planned. A
// disabled option is planned and is not unmatched. Swapping the two flags
// swaps those results.
func TestGatewayHostRegistrationLeavesEnabledOptionsUnmatched(t *testing.T) {
	instances := []gatewayHostInstance{
		{Kind: "family.alpha", Instance: "primary", Enabled: true},
		{Kind: "family.beta", Instance: "primary", Enabled: false},
	}
	opened := openHostForTest(t, instances)
	if got, want := opened.Unmatched(), []string{"family.alpha/primary"}; !slices.Equal(got, want) {
		t.Fatalf("unmatched = %v; want %v", got, want)
	}
	if got, want := opened.planned(), []string{"family.beta/primary"}; !slices.Equal(got, want) {
		t.Fatalf("planned = %v; want %v", got, want)
	}
	swapped := []gatewayHostInstance{
		{Kind: "family.alpha", Instance: "primary", Enabled: false},
		{Kind: "family.beta", Instance: "primary", Enabled: true},
	}
	again := openHostForTest(t, swapped)
	if got, want := again.Unmatched(), []string{"family.beta/primary"}; !slices.Equal(got, want) {
		t.Fatalf("swapped unmatched = %v; want %v", got, want)
	}
	if got, want := again.planned(), []string{"family.alpha/primary"}; !slices.Equal(got, want) {
		t.Fatalf("swapped planned = %v; want %v", got, want)
	}
}

// TestGatewayHostRegistrationRepeatsTheSameAdmission opens the same configured
// instances twice. The unmatched set and the plan stay equal across the two
// lifecycles.
func TestGatewayHostRegistrationRepeatsTheSameAdmission(t *testing.T) {
	instances := []gatewayHostInstance{
		{Kind: "family.alpha", Instance: "primary", Enabled: true},
		{Kind: "family.beta", Instance: "primary", Enabled: false},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime_state.json")
	openOnce := func() (unmatched, planned []string) {
		t.Helper()
		opened, err := openGatewayHost(context.Background(), path, instances)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := opened.Close(context.Background()); err != nil {
				t.Errorf("close host: %v", err)
			}
		}()
		if err := opened.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return append([]string(nil), opened.Unmatched()...), append([]string(nil), opened.planned()...)
	}
	firstUnmatched, firstPlanned := openOnce()
	secondUnmatched, secondPlanned := openOnce()
	if !reflect.DeepEqual(firstUnmatched, secondUnmatched) || !reflect.DeepEqual(firstPlanned, secondPlanned) {
		t.Fatalf("lifecycles differ: unmatched %v vs %v; planned %v vs %v", firstUnmatched, secondUnmatched, firstPlanned, secondPlanned)
	}
}

// TestGatewayHostRegistrationAdmitsConfiguredInstances uses the driver options
// the process composition registers. An enabled option is unmatched when no
// factory is selected. A disabled option is planned. Dropping either class
// fails the partition.
func TestGatewayHostRegistrationAdmitsConfiguredInstances(t *testing.T) {
	cfg := ebusgateway.Config{PortalConfigRoot: t.TempDir()}
	sidecars := prepareManagedSidecars(context.Background(), cfg, nil, "fixture", nil, openGatewayPortalConfigRuntime(cfg))
	if len(sidecars.hostInstances) == 0 {
		t.Fatal("configured driver options produced no host instances")
	}
	opened := openHostForTest(t, sidecars.hostInstances)
	var wantUnmatched, wantPlanned []string
	seen := map[string]bool{}
	for _, instance := range sidecars.hostInstances {
		if instance.Kind == "" || instance.Instance == "" {
			t.Fatalf("incomplete identity %+v", instance)
		}
		id := instance.Kind + "/" + instance.Instance
		if seen[id] {
			t.Fatalf("duplicate host instance %s", id)
		}
		seen[id] = true
		if instance.Enabled {
			wantUnmatched = append(wantUnmatched, id)
			continue
		}
		wantPlanned = append(wantPlanned, id)
	}
	if !slices.Equal(opened.Unmatched(), wantUnmatched) {
		t.Fatalf("unmatched = %v; want enabled options %v", opened.Unmatched(), wantUnmatched)
	}
	gotPlanned := append([]string(nil), opened.planned()...)
	slices.Sort(wantPlanned)
	slices.Sort(gotPlanned)
	if !slices.Equal(gotPlanned, wantPlanned) {
		t.Fatalf("planned = %v; want disabled options %v", gotPlanned, wantPlanned)
	}
}

// TestGatewayHostShutdownWritesCheckpointInPlace is the process shutdown
// path. The close writes the host checkpoint into the state directory and no
// test hook moves anything aside: identity and lifecycle are still there after
// the process is joined, and the test removes the directory afterwards.
func TestGatewayHostShutdownWritesCheckpointInPlace(t *testing.T) {
	dir := t.TempDir()
	cfg := ebusgateway.Config{RuntimeStatePath: filepath.Join(dir, "runtime_state.json")}
	shell, err := newGatewayProcessShell(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	shell.sidecars = &managedSidecars{hostInstances: []gatewayHostInstance{{Kind: "family.alpha", Instance: "primary", Enabled: false}}}
	if err := shell.acquireSemRegHost(); err != nil {
		t.Fatal(err)
	}
	if shell.semregHost == nil {
		t.Fatal("host was not opened")
	}
	if err := shell.runShutdown(nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	for _, name := range []string{"identity", "lifecycle"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s after shutdown: %v; the checkpoint is written in place", name, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove state directory after the process is joined: %v", err)
	}
}

// TestGatewayHostCloseReturnsTheCheckpointError pins that the close path
// returns the error the host close returns and never reports success because
// something else happened: a checkpoint that cannot be written is an error.
func TestGatewayHostCloseReturnsTheCheckpointError(t *testing.T) {
	dir := t.TempDir()
	opened, err := openGatewayHost(context.Background(), filepath.Join(dir, "runtime_state.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A directory where the checkpoint's temporary file belongs makes the write fail.
	blocker := filepath.Join(dir, "identity", "checkpoint.json.tmp")
	if err := os.MkdirAll(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(context.Background()); err == nil {
		t.Fatal("Close = nil; a checkpoint that cannot be written is a close error")
	}
}

func TestGatewayHostStateDirUnavailableIsNarrow(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	other := filepath.Join(root, "other", "x")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"mkdir of the state directory", &os.PathError{Op: "mkdir", Path: state, Err: syscall.EACCES}, true},
		{"mkdir below the state directory", &os.PathError{Op: "mkdir", Path: filepath.Join(state, "lifecycle"), Err: syscall.EROFS}, true},
		{"path through a file", &os.PathError{Op: "open", Path: filepath.Join(state, "local-identity.json"), Err: syscall.ENOTDIR}, true},
		{"read error of a record", &os.PathError{Op: "open", Path: filepath.Join(state, "local-identity.json"), Err: syscall.EIO}, false},
		{"mkdir elsewhere", &os.PathError{Op: "mkdir", Path: other, Err: syscall.EACCES}, false},
		{"not a path error", errors.New("local identity record is damaged"), false},
	}
	for _, tc := range cases {
		if got := gatewayHostStateDirUnavailable(tc.err, state); got != tc.want {
			t.Errorf("%s: unavailable = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestGatewayHostRegistrationSkipsAnUnusableStateDirectory(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := ebusgateway.Config{RuntimeStatePath: filepath.Join(blocker, "runtime_state.json")}
	shell, err := newGatewayProcessShell(context.Background(), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	shell.sidecars = &managedSidecars{hostInstances: []gatewayHostInstance{{Kind: "family.alpha", Instance: "primary", Enabled: false}}}
	if err := shell.acquireSemRegHost(); err != nil {
		t.Fatalf("unusable state directory returned %v; the process continues", err)
	}
	if shell.semregHost != nil {
		t.Fatal("unusable state directory opened a host")
	}
	if err := shell.runShutdown(nil); err != nil {
		t.Fatalf("shutdown = %v", err)
	}
}

func openHostForTest(t *testing.T, instances []gatewayHostInstance) *gatewayHost {
	t.Helper()
	dir := t.TempDir()
	opened, err := openGatewayHost(context.Background(), filepath.Join(dir, "runtime_state.json"), instances)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Close(context.Background()); err != nil {
			t.Errorf("close host: %v", err)
		}
	})
	if err := opened.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lifecycle")); err != nil {
		t.Fatalf("host state directory: %v", err)
	}
	return opened
}

type gatewayHostTestFactory struct{ kind driver.DriverKind }

func (f gatewayHostTestFactory) Descriptor() driver.Descriptor { return driver.Descriptor{} }

func (f gatewayHostTestFactory) Plan(config driver.Config) ([]driver.InstancePlan, error) {
	if !config.Enabled {
		return nil, nil
	}
	return []driver.InstancePlan{{Driver: f.kind, Instance: driver.InstanceKey(config.Instance)}}, nil
}

func (f gatewayHostTestFactory) Construct(context.Context, driver.InstancePlan) (driver.Instance, error) {
	return gatewayHostTestInstance{}, nil
}

type gatewayHostTestInstance struct{}

func (gatewayHostTestInstance) Start(context.Context, driver.Session) error { return nil }
func (gatewayHostTestInstance) FenceWithdrawal()                            {}
func (gatewayHostTestInstance) Stop(context.Context) driver.CloseReport {
	return driver.CloseReport{Confirmed: true}
}
