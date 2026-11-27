package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Project-Helianthus/helianthus-gateway/internal/runtimecontrol"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/host"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
)

// gatewayRuntimeStateFile is the runtime-state file used when RuntimeStatePath
// is empty. The host receives the directory of that file.
const gatewayRuntimeStateFile = "/data/runtime_state.json"

// gatewayHostInstance is one configured driver option as host enablement.
// Kind is the driver id, Instance is the configured key, and Enabled is the
// option's eligibility before a portal record narrows it.
type gatewayHostInstance struct {
	Kind      string
	Instance  string
	Enabled   bool
	Transport json.RawMessage
	Options   json.RawMessage
}

// gatewayHost is the process's SemReg host. Production selects no driver
// factory: constructor packages stay in the composition file that owns them.
// An enabled option with no factory is reported unmatched and is not started.
type gatewayHost struct {
	site     *host.Site
	stateDir string
}

// gatewayHostPrepareClose runs before the process closes the host. The gateway
// binary leaves it nil. A test sets it so the checkpoint write cannot add a
// file under the state directory while that directory is being removed. The
// cleanup drops whatever the hook moved; nil means the directories stayed.
var gatewayHostPrepareClose func(stateDir string) (cleanup func())

func gatewayRuntimeStateDir(path string) string {
	if path == "" {
		path = gatewayRuntimeStateFile
	}
	return filepath.Dir(path)
}

func hostInstancesFromDescriptors(descriptors []runtimecontrol.Descriptor) []gatewayHostInstance {
	instances := make([]gatewayHostInstance, 0, len(descriptors))
	for _, descriptor := range descriptors {
		instances = append(instances, gatewayHostInstance{
			Kind:     string(descriptor.DriverID),
			Instance: "primary",
			Enabled:  descriptor.Disposition == runtimecontrol.DispositionEnabled,
		})
	}
	return instances
}

func gatewayHostConfig(statePath string, instances []gatewayHostInstance) host.Config {
	cfg := host.Config{StateDir: gatewayRuntimeStateDir(statePath)}
	for _, instance := range instances {
		key := instance.Instance
		if key == "" {
			key = "primary"
		}
		cfg.Instances = append(cfg.Instances, host.InstanceConfig{
			Kind:      driver.DriverKind(instance.Kind),
			Instance:  driver.InstanceKey(key),
			Enabled:   instance.Enabled,
			Transport: append(json.RawMessage(nil), instance.Transport...),
			Options:   append(json.RawMessage(nil), instance.Options...),
		})
	}
	return cfg
}

func openGatewayHost(ctx context.Context, statePath string, instances []gatewayHostInstance, factories ...driver.Factory) (*gatewayHost, error) {
	cfg := gatewayHostConfig(statePath, instances)
	site, err := host.Open(ctx, cfg, factories...)
	if err != nil {
		return nil, err
	}
	return &gatewayHost{site: site, stateDir: cfg.StateDir}, nil
}

func (opened *gatewayHost) Start(ctx context.Context) error {
	return opened.site.Start(ctx)
}

func (opened *gatewayHost) Close(ctx context.Context) error {
	if opened == nil || opened.site == nil {
		return nil
	}
	return opened.site.Close(ctx)
}

// closeProcess closes the host from process shutdown. A test hook may move the
// host directories aside first. The checkpoint write then cannot create a file
// in the state directory, and that write's error is not a process error.
func (opened *gatewayHost) closeProcess(ctx context.Context) error {
	if opened == nil || opened.site == nil {
		return nil
	}
	var cleanup func()
	displaced := false
	if gatewayHostPrepareClose != nil {
		cleanup = gatewayHostPrepareClose(opened.stateDir)
		displaced = cleanup != nil
	}
	err := opened.Close(ctx)
	if cleanup != nil {
		cleanup()
	}
	if displaced {
		return nil
	}
	return err
}

func (opened *gatewayHost) Unmatched() []string {
	if opened == nil || opened.site == nil {
		return nil
	}
	return opened.site.Unmatched()
}

func (opened *gatewayHost) planned() []string {
	if opened == nil || opened.site == nil {
		return nil
	}
	set := opened.site.Runtime().Plan()
	planned := make([]string, 0, len(set.Instances))
	for _, instance := range set.Instances {
		planned = append(planned, instance.Ref.String())
	}
	return planned
}

// gatewayHostStateDirUnavailable reports a state directory the process cannot
// create. Runtime-state load already continues in that case.
func gatewayHostStateDirUnavailable(err error) bool {
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}
