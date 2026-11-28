package runtimecontrol

import (
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
)

func reservedHookDeclarations() []HookDeclaration {
	names := []HookName{HookSemantic, HookMCP, HookPortal, HookSetup, HookEvidence}
	hooks := make([]HookDeclaration, len(names))
	for i, name := range names {
		hooks[i] = HookDeclaration{Name: name, Version: RegistrationHookVersion}
	}
	return hooks
}

type recordingSurface struct{ name HookName }

func (s recordingSurface) Hook() HookName { return s.name }

type pointerSurface struct{ name HookName }

func (s *pointerSurface) Hook() HookName { return s.name }

func boundSurfaces() map[HookName]Surface {
	return map[HookName]Surface{
		HookSemantic: recordingSurface{name: HookSemantic},
		HookMCP:      recordingSurface{name: HookMCP},
		HookPortal:   recordingSurface{name: HookPortal},
		HookSetup:    recordingSurface{name: HookSetup},
		HookEvidence: recordingSurface{name: HookEvidence},
	}
}

// TestRegistrationHookAdmitsReservedNames varies the declaration identity.
//
//   - Name: each reserved name is stored. Replacing mcp with an unknown name
//     refuses the descriptor and leaves the registry unchanged.
//   - Version: RegistrationHookVersion is stored. Replacing it with v1, an
//     empty version, 2 or a padded 1 refuses unsupported version.
//   - Direction: southbound, northbound and both each admit the same reserved
//     list. Copying the output direction check would refuse southbound and both.
//   - Repetition: a second copy of one name is a duplicate hook.
//   - Output beside a reserved name at version 1 stays unimplemented. The
//     same pair with the reserved version v1 is an unsupported version.
func TestRegistrationHookAdmitsReservedNames(t *testing.T) {
	for _, direction := range []Direction{DirectionSouthbound, DirectionNorthbound, DirectionBoth} {
		var calls atomic.Int64
		registry := NewRegistry(nil)
		descriptor := fixture(&calls, "family.sample", direction, "sample-src")
		descriptor.Hooks = reservedHookDeclarations()
		if err := registry.Register(descriptor); err != nil {
			t.Fatalf("%s Register: %v", direction, err)
		}
		got, err := registry.Get("family.sample")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Driver.Hooks, reservedHookDeclarations()) {
			t.Fatalf("%s hooks = %+v", direction, got.Driver.Hooks)
		}
		if calls.Load() != 0 {
			t.Fatalf("factory calls = %d", calls.Load())
		}
	}

	var calls atomic.Int64
	registry := NewRegistry(nil)
	if err := registry.Register(fixture(&calls, "base.driver", DirectionBoth, "base-src")); err != nil {
		t.Fatal(err)
	}
	before := registry.List()
	refused := []struct {
		name  string
		hooks []HookDeclaration
		want  error
	}{
		{"empty version", []HookDeclaration{{Name: HookMCP, Version: ""}}, ErrUnsupportedHookVersion},
		{"prefixed version", []HookDeclaration{{Name: HookSetup, Version: "v1"}}, ErrUnsupportedHookVersion},
		{"other version", []HookDeclaration{{Name: HookPortal, Version: "2"}}, ErrUnsupportedHookVersion},
		{"padded version", []HookDeclaration{{Name: HookSemantic, Version: " 1"}}, ErrUnsupportedHookVersion},
		{"duplicate name", []HookDeclaration{{Name: HookEvidence, Version: RegistrationHookVersion}, {Name: HookEvidence, Version: RegistrationHookVersion}}, ErrDuplicateHook},
		{"unknown name", []HookDeclaration{{Name: "telemetry", Version: RegistrationHookVersion}}, ErrUnknownHook},
		{"output beside reserved", []HookDeclaration{{Name: HookOutput, Version: OutputHookVersion}, {Name: HookPortal, Version: RegistrationHookVersion}}, ErrHookUnimplemented},
		{"reserved version beside output", []HookDeclaration{{Name: HookOutput, Version: OutputHookVersion}, {Name: HookMCP, Version: "v1"}}, ErrUnsupportedHookVersion},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			descriptor := fixture(&calls, "family.sample", DirectionNorthbound, "sample-src")
			descriptor.Hooks = tc.hooks
			err := registry.Register(descriptor)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Register = %v, want %v", err, tc.want)
			}
			if after := registry.List(); !reflect.DeepEqual(before, after) {
				t.Fatalf("registry changed after %s", tc.name)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("factory calls = %d", calls.Load())
	}
}

// TestRegistrationHookResolvesTheBoundSurface varies the resolution identity.
//
//   - DriverID: another id is not found. Replacing the registered id with an
//     unregistered one must not return the bound surface.
//   - HookName: each declared name returns the surface bound under that name.
//     Swapping the semantic surface onto mcp fails closed and keeps the
//     previous surfaces.
//   - Declaration: a driver that did not declare the name stays unimplemented
//     while the surface is bound. An output declaration stays unknown.
//   - Disposition: a disabled driver that declared the hook still resolves.
//     Requiring enabled would refuse it.
//   - Binding: a declared name with no surface is unimplemented. Replacing
//     the whole map with only semantic makes mcp unimplemented. An empty
//     bind clears every surface. A refused bind keeps the previous map.
func TestRegistrationHookResolvesTheBoundSurface(t *testing.T) {
	var calls atomic.Int64
	registry := NewRegistry(nil)
	declared := fixture(&calls, "family.sample", DirectionSouthbound, "sample-src")
	declared.Hooks = reservedHookDeclarations()
	disabled := fixture(&calls, "family.disabled", DirectionBoth, "disabled-src")
	disabled.Disposition = DispositionDisabled
	disabled.Hooks = reservedHookDeclarations()
	output := fixture(&calls, "family.output", DirectionNorthbound, "output-src")
	output.Hooks = []HookDeclaration{{Name: HookOutput, Version: OutputHookVersion}}
	plain := fixture(&calls, "family.plain", DirectionNorthbound, "plain-src")
	if err := registry.Register(declared, disabled, output, plain); err != nil {
		t.Fatal(err)
	}

	for _, name := range []HookName{HookSemantic, HookMCP, HookPortal, HookSetup, HookEvidence} {
		surface, err := registry.ResolveHook("family.sample", name)
		if surface != nil || !errors.Is(err, ErrHookUnimplemented) {
			t.Fatalf("unbound %s = %v, %v", name, surface, err)
		}
	}
	if err := registry.BindHookSurfaces(boundSurfaces()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []HookName{HookSemantic, HookMCP, HookPortal, HookSetup, HookEvidence} {
		surface, err := registry.ResolveHook("family.sample", name)
		if err != nil {
			t.Fatalf("ResolveHook(%s) = %v", name, err)
		}
		if surface != (recordingSurface{name: name}) {
			t.Fatalf("ResolveHook(%s) = %#v", name, surface)
		}
		disabledSurface, err := registry.ResolveHook("family.disabled", name)
		if err != nil || disabledSurface != surface {
			t.Fatalf("disabled %s = %#v, %v", name, disabledSurface, err)
		}
	}
	if surface, err := registry.ResolveHook("family.missing", HookSemantic); surface != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing driver = %#v, %v", surface, err)
	}
	if surface, err := registry.ResolveHook("family.plain", HookMCP); surface != nil || !errors.Is(err, ErrHookUnimplemented) {
		t.Fatalf("undeclared mcp = %#v, %v", surface, err)
	}
	if surface, err := registry.ResolveHook("family.output", HookOutput); surface != nil || !errors.Is(err, ErrUnknownHook) {
		t.Fatalf("output = %#v, %v", surface, err)
	}
	if surface, err := registry.ResolveHook("family.output", HookEvidence); surface != nil || !errors.Is(err, ErrHookUnimplemented) {
		t.Fatalf("output driver evidence = %#v, %v", surface, err)
	}

	caller := boundSurfaces()
	if err := registry.BindHookSurfaces(caller); err != nil {
		t.Fatal(err)
	}
	caller[HookSemantic] = recordingSurface{name: HookMCP}
	surface, err := registry.ResolveHook("family.sample", HookSemantic)
	if err != nil || surface != (recordingSurface{name: HookSemantic}) {
		t.Fatalf("caller map mutation reached the registry: %#v, %v", surface, err)
	}

	before := boundSurfaces()
	if err := registry.BindHookSurfaces(before); err != nil {
		t.Fatal(err)
	}
	refused := map[HookName]Surface{
		HookSemantic: recordingSurface{name: HookMCP},
		HookOutput:   recordingSurface{name: HookOutput},
		HookMCP:      (*pointerSurface)(nil),
	}
	for name, surface := range refused {
		err := registry.BindHookSurfaces(map[HookName]Surface{name: surface})
		if name == HookOutput {
			if !errors.Is(err, ErrUnknownHook) {
				t.Fatalf("bind %s = %v, want unknown", name, err)
			}
		} else if !errors.Is(err, ErrHookUnimplemented) {
			t.Fatalf("bind %s = %v, want unimplemented", name, err)
		}
		got, err := registry.ResolveHook("family.sample", HookSemantic)
		if err != nil || got != (recordingSurface{name: HookSemantic}) {
			t.Fatalf("refused bind of %s replaced semantic: %#v, %v", name, got, err)
		}
	}

	if err := registry.BindHookSurfaces(map[HookName]Surface{HookSemantic: recordingSurface{name: HookSemantic}}); err != nil {
		t.Fatal(err)
	}
	if surface, err := registry.ResolveHook("family.sample", HookSemantic); err != nil || surface != (recordingSurface{name: HookSemantic}) {
		t.Fatalf("partial semantic = %#v, %v", surface, err)
	}
	if surface, err := registry.ResolveHook("family.sample", HookMCP); surface != nil || !errors.Is(err, ErrHookUnimplemented) {
		t.Fatalf("partial bind kept mcp: %#v, %v", surface, err)
	}
	if err := registry.BindHookSurfaces(nil); err != nil {
		t.Fatal(err)
	}
	if surface, err := registry.ResolveHook("family.sample", HookSemantic); surface != nil || !errors.Is(err, ErrHookUnimplemented) {
		t.Fatalf("cleared semantic = %#v, %v", surface, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("factory calls = %d", calls.Load())
	}
}
