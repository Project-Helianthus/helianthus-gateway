package runtimecontrol

import (
	"fmt"
	"reflect"
)

// Surface is the host surface bound for one reserved hook. Hook is pure and
// must not call the registry. The package does not interpret the surface
// beyond that name.
type Surface interface {
	Hook() HookName
}

func reservedHook(name HookName) bool {
	switch name {
	case HookMCP, HookSetup, HookPortal, HookSemantic, HookEvidence:
		return true
	default:
		return false
	}
}

func surfaceUsable(surface Surface) bool {
	if surface == nil {
		return false
	}
	value := reflect.ValueOf(surface)
	switch value.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan:
		return !value.IsNil()
	default:
		return true
	}
}

// BindHookSurfaces replaces the surfaces reserved hooks resolve to. A nil or
// empty map clears them. The caller's map is not retained. An unknown name,
// output included, is rejected. A nil surface, or one whose Hook method
// returns a different name, is rejected. A refusal leaves the previous
// surfaces in place.
func (r *Registry) BindHookSurfaces(surfaces map[HookName]Surface) error {
	next := make(map[HookName]Surface, len(surfaces))
	for name, surface := range surfaces {
		if !reservedHook(name) {
			return fmt.Errorf("%w: %s", ErrUnknownHook, name)
		}
		if !surfaceUsable(surface) || surface.Hook() != name {
			return fmt.Errorf("%w: %s is reserved", ErrHookUnimplemented, name)
		}
		next[name] = surface
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.surfaces = next
	return nil
}
