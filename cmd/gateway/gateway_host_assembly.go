package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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

// gatewayHostStateDirUnavailable reports an error from creating or opening the
// state directory itself: a mkdir below it that failed, a path through it that
// is not a directory, or a write refused by a directory that is already there.
// An absent directory and one that is present but not writable have the same
// outcome. Runtime-state load already continues in that case. Any other error
// is not this one.
func gatewayHostStateDirUnavailable(err error, stateDir string) bool {
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	rel, relErr := filepath.Rel(stateDir, pathErr.Path)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	if pathErr.Op == "mkdir" || errors.Is(pathErr.Err, syscall.ENOTDIR) {
		return true
	}
	return errors.Is(pathErr.Err, os.ErrPermission) || errors.Is(pathErr.Err, syscall.EROFS)
}
